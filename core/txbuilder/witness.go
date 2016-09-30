package txbuilder

import (
	"context"
	"encoding/json"

	"golang.org/x/crypto/sha3"

	"chain/crypto/ed25519"
	chainjson "chain/encoding/json"
	"chain/errors"
	"chain/protocol/bc"
	"chain/protocol/vm"
	"chain/protocol/vmutil"
)

// SignFunc is the function passed into Sign that produces
// a signature for a given xpub, derivation path, and hash.
type SignFunc func(context.Context, string, [][]byte, [32]byte) ([]byte, error)

// WitnessComponent encodes instructions for finalizing a transaction
// by populating its InputWitness fields. Each WitnessComponent object
// produces zero or more items for the InputWitness of the txinput it
// corresponds to.
type WitnessComponent interface {
	// Sign is called to add signatures. Actual signing is delegated to
	// a callback function.
	Sign(context.Context, *Template, int, []string, SignFunc) error

	// Materialize is called to turn the component into a vector of
	// arguments for the input witness.
	Materialize(*Template, int, *[][]byte) error
}

// materializeWitnesses takes a filled in Template and "materializes"
// each witness component, turning it into a vector of arguments for
// the tx's input witness, creating a fully-signed transaction.
func materializeWitnesses(txTemplate *Template) error {
	msg := txTemplate.Transaction

	if msg == nil {
		return errors.Wrap(ErrMissingRawTx)
	}

	if len(txTemplate.SigningInstructions) > len(msg.Inputs) {
		return errors.Wrap(ErrBadInstructionCount)
	}

	for i, sigInst := range txTemplate.SigningInstructions {
		if msg.Inputs[sigInst.Position] == nil {
			return errors.WithDetailf(ErrBadTxInputIdx, "signing instruction %d references missing tx input %d", i, sigInst.Position)
		}

		var witness [][]byte
		for j, c := range sigInst.WitnessComponents {
			err := c.Materialize(txTemplate, int(sigInst.Position), &witness)
			if err != nil {
				return errors.WithDetailf(err, "error in witness component %d of input %d", j, i)
			}
		}

		msg.Inputs[sigInst.Position].SetArguments(witness)
	}

	return nil
}

type (
	SignatureWitness struct {
		// Quorum is the number of signatures required.
		Quorum int `json:"quorum"`

		// Keys are the identities of the keys to sign with.
		Keys []KeyID `json:"keys"`

		// Program is the predicate part of the signature program, whose hash is what gets
		// signed. If empty, it is computed during Sign from the outputs
		// and the current input of the transaction.
		Program chainjson.HexBytes `json:"program"`

		// Sigs are signatures of Program made from each of the Keys
		// during Sign.
		Sigs []chainjson.HexBytes `json:"signatures"`
	}

	KeyID struct {
		XPub           string               `json:"xpub"`
		DerivationPath []chainjson.HexBytes `json:"derivation_path"`
	}
)

var ErrEmptyProgram = errors.New("empty signature program")

// Sign populates sw.Sigs with as many signatures of the predicate in
// sw.Program as it can from the set of keys in sw.Keys.
//
// If sw.Program is empty, it is populated with an _inferred_ predicate:
// a program committing to aspects of the current
// transaction. Specifically, the program commits to:
//  - the mintime and maxtime of the transaction (if non-zero)
//  - the outpoint and (if non-empty) reference data of the current input
//  - the assetID, amount, control program, and (if non-empty) reference data of each output.
func (sw *SignatureWitness) Sign(ctx context.Context, tpl *Template, index int, xpubs []string, signFn SignFunc) error {
	// Compute the predicate to sign. This is either a
	// txsighash program if tpl.Final is true (i.e., the tx is complete
	// and no further changes are allowed) or a program enforcing
	// constraints derived from the existing outputs and current input.
	if len(sw.Program) == 0 {
		sw.Program = buildSigProgram(tpl, int(tpl.SigningInstructions[index].Position))
		if len(sw.Program) == 0 {
			return ErrEmptyProgram
		}
	}
	if len(sw.Sigs) < len(sw.Keys) {
		// Each key in sw.Keys may produce a signature in sw.Sigs. Make
		// sure there are enough slots in sw.Sigs and that we preserve any
		// sigs already present.
		newSigs := make([]chainjson.HexBytes, len(sw.Keys))
		copy(newSigs, sw.Sigs)
		sw.Sigs = newSigs
	}
	h := sha3.Sum256(sw.Program)
	for i, keyID := range sw.Keys {
		if len(sw.Sigs[i]) > 0 {
			// Already have a signature for this key
			continue
		}
		if !contains(xpubs, keyID.XPub) {
			continue
		}
		var path [][]byte
		for _, p := range keyID.DerivationPath {
			path = append(path, p)
		}
		sigBytes, err := signFn(ctx, keyID.XPub, path, h)
		if err != nil {
			return errors.WithDetailf(err, "computing signature %d", i)
		}
		sw.Sigs[i] = sigBytes
	}
	return nil
}

func contains(list []string, key string) bool {
	for _, k := range list {
		if k == key {
			return true
		}
	}
	return false
}

func buildSigProgram(tpl *Template, index int) []byte {
	if !tpl.AllowAdditional {
		h := tpl.Hash(index)
		builder := vmutil.NewBuilder()
		builder.AddData(h[:])
		builder.AddOp(vm.OP_TXSIGHASH).AddOp(vm.OP_EQUAL)
		return builder.Program
	}
	constraints := make([]constraint, 0, 3+len(tpl.Transaction.Outputs))
	constraints = append(constraints, &timeConstraint{
		minTimeMS: tpl.Transaction.MinTime,
		maxTimeMS: tpl.Transaction.MaxTime,
	})
	inp := tpl.Transaction.Inputs[index]
	if !inp.IsIssuance() {
		constraints = append(constraints, outpointConstraint(inp.Outpoint()))
	}
	if len(inp.ReferenceData) > 0 {
		constraints = append(constraints, refdataConstraint(inp.ReferenceData))
	}
	for i, out := range tpl.Transaction.Outputs {
		c := &payConstraint{
			Index:       i,
			AssetAmount: out.AssetAmount,
			Program:     out.ControlProgram,
		}
		if len(out.ReferenceData) > 0 {
			h := sha3.Sum256(out.ReferenceData)
			c.RefDataHash = (*bc.Hash)(&h)
		}
		constraints = append(constraints, c)
	}
	var program []byte
	for i, c := range constraints {
		program = append(program, c.code()...)
		if i < len(constraints)-1 { // leave the final bool on top of the stack
			program = append(program, byte(vm.OP_VERIFY))
		}
	}
	return program
}

func (sw SignatureWitness) Materialize(tpl *Template, index int, args *[][]byte) error {
	input := tpl.Transaction.Inputs[index]
	var multiSig []byte
	if input.IsIssuance() {
		multiSig = input.IssuanceProgram()
	} else {
		multiSig = input.ControlProgram()
	}
	pubkeys, quorum, err := vmutil.ParseP2SPMultiSigProgram(multiSig)
	if err != nil {
		return errors.Wrap(err, "parsing input program script")
	}

	// This is the value of N for the CHECKPREDICATE call. The code
	// assumes that everything already in the arg list before this call
	// to Materialize is input to the signature program, so N is
	// len(*args).
	*args = append(*args, vm.Int64Bytes(int64(len(*args))))

	var nsigs int
	h := sha3.Sum256(sw.Program)
	for i := 0; i < len(pubkeys) && nsigs < quorum; i++ {
		k := indexSig(pubkeys[i], h[:], sw.Sigs)
		if k >= 0 {
			*args = append(*args, sw.Sigs[k])
			nsigs++
		}
	}
	*args = append(*args, sw.Program)
	return nil
}

func indexSig(key ed25519.PublicKey, msg []byte, sigs []chainjson.HexBytes) int {
	for i, sig := range sigs {
		if ed25519.Verify(key, msg, sig) {
			return i
		}
	}
	return -1
}

func (sw SignatureWitness) MarshalJSON() ([]byte, error) {
	obj := struct {
		Type   string               `json:"type"`
		Quorum int                  `json:"quorum"`
		Keys   []KeyID              `json:"keys"`
		Sigs   []chainjson.HexBytes `json:"signatures"`
	}{
		Type:   "signature",
		Quorum: sw.Quorum,
		Keys:   sw.Keys,
		Sigs:   sw.Sigs,
	}
	return json.Marshal(obj)
}

func (si *SigningInstruction) AddWitnessKeys(keys []KeyID, quorum int) {
	sw := &SignatureWitness{
		Quorum: quorum,
		Keys:   keys,
	}
	si.WitnessComponents = append(si.WitnessComponents, sw)
}
