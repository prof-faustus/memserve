package client_test

import (
	"errors"
	"testing"

	"memserve/api"
	"memserve/attest"
	"memserve/client"
	"memserve/commitment"
	"memserve/ingest"
	"memserve/proof"
	"memserve/prune"
	"memserve/store"
	"memserve/store/mem"
	"memserve/teranode"
)

func honestServer(t *testing.T) (*api.Server, store.Hash) {
	t.Helper()
	st := mem.New()
	in := ingest.New(st, prune.New(st, prune.Policy{}), ingest.Config{})
	src := teranode.NewMock(teranode.MockConfig{Blocks: 1, SubtreesPer: 2, TxsPerSubtree: 16})
	b, _, _ := src.Next()
	in.IngestBlock(b)
	return api.New(st, 0), b.Subtrees[0].TxIDs[0]
}

// liar lies about everything and returns a non-verifying proof.
type liar struct{ minedTxid store.Hash }

func (l liar) Seen(store.Hash) (api.SeenResult, error)   { return api.SeenResult{}, nil }
func (l liar) Mined(store.Hash) (api.MinedResult, error) { return api.MinedResult{Mined: false}, nil }
func (l liar) MerklePath(txid store.Hash) (proof.Proof, error) {
	// forged proof that will NOT verify.
	return proof.Proof{Leaf: txid, BlockRoot: commitment.DoubleSHA256([]byte("fake"))}, nil
}
func (l liar) UTXO(store.Outpoint) (api.UTXOResult, error) {
	return api.UTXOResult{Status: api.UTXOUnspent, Value: 999}, nil
}

func TestMerklePathIgnoresLiar(t *testing.T) {
	honest, txid := honestServer(t)
	mc := client.New(liar{}, honest) // liar first
	p, err := mc.MerklePath(txid)
	if err != nil {
		t.Fatalf("multi-client could not get a verifying proof: %v", err)
	}
	if !p.Verify() || p.Leaf != txid {
		t.Fatal("returned proof is not the honest verifying one")
	}
}

func TestMinedProofBackedDespiteLiar(t *testing.T) {
	honest, txid := honestServer(t)
	mc := client.New(liar{}, honest)
	res, proven, _ := mc.Mined(txid)
	if !proven || !res.Mined {
		t.Fatalf("mined not proven despite an honest operator: proven=%v res=%+v", proven, res)
	}
}

func TestNoProofForUnknown(t *testing.T) {
	honest, _ := honestServer(t)
	mc := client.New(honest, liar{})
	var unknown store.Hash
	unknown[0] = 0xAB
	if _, err := mc.MerklePath(unknown); !errors.Is(err, client.ErrNoProof) {
		t.Fatalf("want ErrNoProof for unknown tx, got %v", err)
	}
}

// attestingLiar signs "not mined" for a tx that is actually mined.
type attestingLiar struct {
	liar
	id *attest.Identity
}

func (a attestingLiar) MinedAttested(txid store.Hash) (api.MinedResult, attest.Attestation, error) {
	st := attest.Statement{Kind: attest.StmtMined, TxID: txid, Flag: false, Tip: 1}
	att, err := a.id.Attest(st)
	return api.MinedResult{Mined: false}, att, err
}

func TestDetectFraudFromSignedLie(t *testing.T) {
	honest, txid := honestServer(t)
	seed := commitment.DoubleSHA256([]byte("liar-op"))
	id, _ := attest.NewIdentity(seed[:])
	mc := client.New(honest, attestingLiar{id: id})
	fp, ok := mc.DetectFraud(txid)
	if !ok {
		t.Fatal("did not detect the signed false-negative")
	}
	op, _, valid := fp.Verify()
	if !valid || !op.Equal(id.Public()) {
		t.Fatal("fraud proof does not name the lying operator")
	}
}

func TestSpendGuard(t *testing.T) {
	g := client.SpendGuard{Max: 1000}
	if !g.Allow(1000) || g.Allow(1001) {
		t.Fatal("spend guard cap wrong")
	}
}

func TestUTXODisagreementSurfaced(t *testing.T) {
	honest, _ := honestServer(t)
	// honest says unknown for a random outpoint; liar says unspent. Surface the split.
	mc := client.New(honest, liar{})
	op := store.Outpoint{TxID: commitment.DoubleSHA256([]byte("rand")), Vout: 0}
	_, ag := mc.UTXO(op)
	if ag.Responders != 2 {
		t.Fatalf("expected 2 responders, got %d", ag.Responders)
	}
	if ag.Agree == ag.Responders {
		t.Fatal("expected disagreement between honest and liar to be visible")
	}
}
