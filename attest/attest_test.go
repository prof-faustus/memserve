package attest_test

import (
	"testing"

	"memserve/attest"
	"memserve/commitment"
	"memserve/crypto"
	"memserve/ingest"
	"memserve/proof"
	"memserve/prune"
	"memserve/store/mem"
	"memserve/teranode"
)

func realProof(t *testing.T) (proof.Proof, [32]byte) {
	t.Helper()
	st := mem.New()
	in := ingest.New(st, prune.New(st, prune.Policy{}), ingest.Config{})
	src := teranode.NewMock(teranode.MockConfig{Blocks: 1, SubtreesPer: 1, TxsPerSubtree: 8})
	b, _, _ := src.Next()
	in.IngestBlock(b)
	id := b.Subtrees[0].TxIDs[0]
	p, err := proof.Build(st, id)
	if err != nil || !p.Verify() {
		t.Fatalf("could not build a verifying proof: %v", err)
	}
	return p, id
}

func ident(t *testing.T, tag string) *attest.Identity {
	t.Helper()
	seed := commitment.DoubleSHA256([]byte(tag))
	id, err := attest.NewIdentity(seed[:])
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestAttestationRoundTrip(t *testing.T) {
	op := ident(t, "op")
	st := attest.Statement{Kind: attest.StmtMined, TxID: commitment.DoubleSHA256([]byte("x")), Flag: true, Height: 9, Tip: 100}
	a, err := op.Attest(st)
	if err != nil {
		t.Fatal(err)
	}
	if !a.Verify() {
		t.Fatal("valid attestation did not verify")
	}
	// tamper the statement -> signature no longer matches.
	a.Statement.Height = 10
	if a.Verify() {
		t.Fatal("tampered attestation verified")
	}
}

func TestFalseNegativeFraud(t *testing.T) {
	p, txid := realProof(t)
	op := ident(t, "lying-op")
	// operator signs "this tx was NOT mined" — a lie, since p proves inclusion.
	lie, _ := op.Attest(attest.Statement{Kind: attest.StmtMined, TxID: txid, Flag: false, Tip: 100})
	fp, err := attest.ProveFalseNegative(lie, p)
	if err != nil {
		t.Fatalf("could not build fraud proof: %v", err)
	}
	operator, miner, ok := fp.Verify()
	if !ok || operator == nil || miner != nil {
		t.Fatalf("fraud proof did not verify: ok=%v op=%v miner=%v", ok, operator, miner)
	}
	if !operator.Equal(op.Public()) {
		t.Fatal("fraud proof named the wrong operator")
	}
}

func TestFalseNegativeNeedsRealLie(t *testing.T) {
	p, txid := realProof(t)
	op := ident(t, "honest-op")
	// operator signs the TRUTH (mined) — no fraud proof should be constructible.
	truth, _ := op.Attest(attest.Statement{Kind: attest.StmtMined, TxID: txid, Flag: true, Height: 0, Tip: 100})
	if _, err := attest.ProveFalseNegative(truth, p); err != attest.ErrNoContradiction {
		t.Fatalf("constructed fraud proof against an honest answer: %v", err)
	}
}

func TestEquivocationFraud(t *testing.T) {
	op := ident(t, "equiv-op")
	txid := commitment.DoubleSHA256([]byte("y"))
	a, _ := op.Attest(attest.Statement{Kind: attest.StmtSeen, TxID: txid, Flag: true, Tip: 50})
	b, _ := op.Attest(attest.Statement{Kind: attest.StmtSeen, TxID: txid, Flag: false, Tip: 50})
	fp, err := attest.ProveEquivocation(a, b)
	if err != nil {
		t.Fatalf("equivocation not detected: %v", err)
	}
	op2, _, ok := fp.Verify()
	if !ok || !op2.Equal(op.Public()) {
		t.Fatal("equivocation fraud proof invalid")
	}
	// same answer twice is NOT equivocation.
	if _, err := attest.ProveEquivocation(a, a); err != attest.ErrNoContradiction {
		t.Fatalf("false equivocation: %v", err)
	}
}

func TestEndorsementBindsMiner(t *testing.T) {
	p, txid := realProof(t)
	op := ident(t, "op-under-miner")
	minerSeed := commitment.DoubleSHA256([]byte("miner"))
	minerPriv, _ := crypto.NewPrivateKey(minerSeed[:])
	end, err := attest.Endorse(minerPriv, op.Public())
	if err != nil {
		t.Fatal(err)
	}
	if !end.Verify() {
		t.Fatal("endorsement did not verify")
	}
	lie, _ := op.Attest(attest.Statement{Kind: attest.StmtMined, TxID: txid, Flag: false, Tip: 1})
	fp, _ := attest.ProveFalseNegative(lie, p)
	fp = fp.WithEndorsement(end)
	operator, miner, ok := fp.Verify()
	if !ok || operator == nil || miner == nil {
		t.Fatal("endorsed fraud proof must name both operator and miner")
	}
	if !miner.Equal(minerPriv.Public()) {
		t.Fatal("fraud proof named the wrong miner")
	}
}
