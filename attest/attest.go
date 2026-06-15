// Package attest makes MemServe answers ACCOUNTABLE so the service need not be trusted
// (DESIGN.md §16). Each operator has a secp256k1 identity and SIGNS its answers; a
// miner ENDORSES the operator's key, binding the operator to the miner's identity. A
// signed answer is therefore a non-repudiable attestation tied to a named miner.
//
// If an operator (or its miner) lies, anyone holding the signed answer can produce a
// FraudProof:
//
//   - FalseNegative: a signed "not seen" / "not mined" for a txid, refuted by a
//     self-verifying inclusion proof of that txid (folds to a PoW header). AIRTIGHT —
//     the operator provably attested a falsehood.
//   - Equivocation: two signed, contradictory statements for the same query at the same
//     chain tip. AIRTIGHT — the operator provably said both.
//
// A verified FraudProof names the offending operator AND (via the endorsement) the
// miner, so they are held to account (reputation, bond slashing, publication). This is
// what removes trust in "seen/mined" answers: don't trust — verify, and if they lied,
// you hold the proof. BSV only — secp256k1.
package attest

import (
	"bytes"
	"encoding/binary"
	"errors"

	"memserve/commitment"
	"memserve/crypto"
	"memserve/proof"
	"memserve/store"
)

// Kind of statement being attested.
type Kind uint8

const (
	StmtSeen  Kind = iota // Flag = seen?
	StmtMined             // Flag = mined?  (Height/BlockHash valid if Flag)
	StmtUTXO              // Flag = unspent? (Height = spentHeight if !Flag)
)

// Statement is the canonical thing an operator signs: an answer about a query, bound to
// the operator's chain tip at answer time (so equivocation is per-(query,tip)).
type Statement struct {
	Kind      Kind
	TxID      store.Hash
	Vout      uint32
	Flag      bool // seen / mined / unspent depending on Kind
	Height    uint32
	BlockHash store.Hash
	Tip       uint32
}

func b32(b []byte, v uint32) { binary.BigEndian.PutUint32(b, v) }

// encode produces the canonical signing bytes.
func (s Statement) encode() []byte {
	var buf bytes.Buffer
	buf.WriteByte(byte(s.Kind))
	buf.Write(s.TxID[:])
	var u [4]byte
	b32(u[:], s.Vout)
	buf.Write(u[:])
	if s.Flag {
		buf.WriteByte(1)
	} else {
		buf.WriteByte(0)
	}
	b32(u[:], s.Height)
	buf.Write(u[:])
	buf.Write(s.BlockHash[:])
	b32(u[:], s.Tip)
	buf.Write(u[:])
	return buf.Bytes()
}

func (s Statement) sighash() []byte {
	h := commitment.DoubleSHA256(s.encode())
	return h[:]
}

// queryKey identifies a (query, tip) pair for equivocation detection (excludes the
// answer fields so two different answers to the same query collide).
func (s Statement) queryKey() [69]byte {
	var k [69]byte
	k[0] = byte(s.Kind)
	copy(k[1:33], s.TxID[:])
	b32(k[33:37], s.Vout)
	b32(k[37:41], s.Tip)
	// remaining bytes zero; kept fixed-size for comparability
	return k
}

// contradicts reports whether two statements answer the same (query, tip) differently.
func (s Statement) contradicts(o Statement) bool {
	if s.queryKey() != o.queryKey() {
		return false
	}
	return s.Flag != o.Flag || s.Height != o.Height || s.BlockHash != o.BlockHash
}

// Attestation is a Statement signed by an operator identity.
type Attestation struct {
	Statement Statement
	Operator  *crypto.PublicKey
	Sig       *crypto.Signature
}

// Verify reports whether the attestation's signature is valid (low-S enforced).
func (a Attestation) Verify() bool {
	if a.Operator == nil || a.Sig == nil || !a.Sig.IsLowS() {
		return false
	}
	return crypto.Verify(a.Operator, a.Statement.sighash(), a.Sig)
}

// Identity is an operator's signing key.
type Identity struct{ priv *crypto.PrivateKey }

// NewIdentity builds an operator identity from 32 bytes of seed.
func NewIdentity(seed []byte) (*Identity, error) {
	p, err := crypto.NewPrivateKey(seed)
	if err != nil {
		return nil, err
	}
	return &Identity{priv: p}, nil
}

// Public returns the operator's public key.
func (id *Identity) Public() *crypto.PublicKey { return id.priv.Public() }

// Attest signs a statement.
func (id *Identity) Attest(s Statement) (Attestation, error) {
	sig, err := id.priv.Sign(s.sighash())
	if err != nil {
		return Attestation{}, err
	}
	return Attestation{Statement: s, Operator: id.priv.Public(), Sig: sig}, nil
}

// Endorsement binds an operator key to a miner identity: the miner signs the operator's
// compressed pubkey. A fraud proof carrying a valid endorsement implicates the miner.
type Endorsement struct {
	Operator *crypto.PublicKey
	Miner    *crypto.PublicKey
	Sig      *crypto.Signature
}

func endorseHash(operator *crypto.PublicKey) []byte {
	h := commitment.DoubleSHA256(append([]byte("memserve-endorse-v1"), operator.SerializeCompressed()...))
	return h[:]
}

// Endorse: a miner signs an operator's key.
func Endorse(minerPriv *crypto.PrivateKey, operator *crypto.PublicKey) (Endorsement, error) {
	sig, err := minerPriv.Sign(endorseHash(operator))
	if err != nil {
		return Endorsement{}, err
	}
	return Endorsement{Operator: operator, Miner: minerPriv.Public(), Sig: sig}, nil
}

// Verify reports whether the endorsement is a valid miner signature over the operator key.
func (e Endorsement) Verify() bool {
	if e.Operator == nil || e.Miner == nil || e.Sig == nil || !e.Sig.IsLowS() {
		return false
	}
	return crypto.Verify(e.Miner, endorseHash(e.Operator), e.Sig)
}

// --- Fraud proofs -----------------------------------------------------------

// FraudKind classifies an accountability proof.
type FraudKind uint8

const (
	FraudFalseNegative FraudKind = iota // signed not-seen/not-mined refuted by an inclusion proof
	FraudEquivocation                   // two contradictory signed statements
)

// FraudProof is publishable evidence that an operator lied. Verify() re-establishes the
// contradiction from scratch and returns the accountable parties.
type FraudProof struct {
	Kind        FraudKind
	Att         Attestation
	Att2        *Attestation // equivocation only
	Inclusion   *proof.Proof // false-negative only
	Endorsement *Endorsement // optional: binds the operator to a miner
}

// Errors.
var (
	ErrNoContradiction  = errors.New("attest: statements do not contradict / evidence does not refute")
	ErrBadAttestation   = errors.New("attest: attestation signature invalid")
	ErrOperatorMismatch = errors.New("attest: attestations are from different operators")
)

// ProveFalseNegative builds a fraud proof from a signed not-seen/not-mined attestation
// refuted by a verifying inclusion proof of the same txid.
func ProveFalseNegative(att Attestation, incl proof.Proof) (FraudProof, error) {
	if !att.Verify() {
		return FraudProof{}, ErrBadAttestation
	}
	st := att.Statement
	negative := (st.Kind == StmtSeen || st.Kind == StmtMined) && !st.Flag
	if !negative {
		return FraudProof{}, ErrNoContradiction
	}
	if incl.Leaf != st.TxID || !incl.Verify() {
		return FraudProof{}, ErrNoContradiction
	}
	return FraudProof{Kind: FraudFalseNegative, Att: att, Inclusion: &incl}, nil
}

// ProveEquivocation builds a fraud proof from two contradictory signed statements by the
// same operator.
func ProveEquivocation(a, b Attestation) (FraudProof, error) {
	if !a.Verify() || !b.Verify() {
		return FraudProof{}, ErrBadAttestation
	}
	if !a.Operator.Equal(b.Operator) {
		return FraudProof{}, ErrOperatorMismatch
	}
	if !a.Statement.contradicts(b.Statement) {
		return FraudProof{}, ErrNoContradiction
	}
	return FraudProof{Kind: FraudEquivocation, Att: a, Att2: &b}, nil
}

// WithEndorsement attaches a miner endorsement (binding the operator to the miner).
func (fp FraudProof) WithEndorsement(e Endorsement) FraudProof { fp.Endorsement = &e; return fp }

// Verify re-validates the fraud proof from scratch. ok=true means the operator provably
// lied. operator is the accountable operator key; miner is the bound miner (nil if no
// valid endorsement is attached).
func (fp FraudProof) Verify() (operator, miner *crypto.PublicKey, ok bool) {
	switch fp.Kind {
	case FraudFalseNegative:
		if fp.Inclusion == nil {
			return nil, nil, false
		}
		if _, err := ProveFalseNegative(fp.Att, *fp.Inclusion); err != nil {
			return nil, nil, false
		}
		operator = fp.Att.Operator
	case FraudEquivocation:
		if fp.Att2 == nil {
			return nil, nil, false
		}
		if _, err := ProveEquivocation(fp.Att, *fp.Att2); err != nil {
			return nil, nil, false
		}
		operator = fp.Att.Operator
	default:
		return nil, nil, false
	}
	if fp.Endorsement != nil && fp.Endorsement.Verify() && fp.Endorsement.Operator.Equal(operator) {
		miner = fp.Endorsement.Miner
	}
	return operator, miner, true
}
