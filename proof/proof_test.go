package proof_test

import (
	"testing"

	"memserve/ingest"
	"memserve/proof"
	"memserve/prune"
	"memserve/store/mem"
	"memserve/teranode"
)

func build(t *testing.T) (*mem.Store, []teranode.Block) {
	t.Helper()
	st := mem.New()
	pr := prune.New(st, prune.Policy{})
	in := ingest.New(st, pr, ingest.Config{})
	src := teranode.NewMock(teranode.MockConfig{Blocks: 2, SubtreesPer: 3, TxsPerSubtree: 9})
	var blocks []teranode.Block
	for {
		b, ok, err := src.Next()
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			break
		}
		if _, err := in.IngestBlock(b); err != nil {
			t.Fatal(err)
		}
		blocks = append(blocks, b)
	}
	return st, blocks
}

func TestBuildAndVerify(t *testing.T) {
	st, blocks := build(t)
	// verify every tx in every block produces a valid, root-correct proof.
	for _, b := range blocks {
		for _, sub := range b.Subtrees {
			for _, id := range sub.TxIDs {
				p, err := proof.Build(st, id)
				if err != nil {
					t.Fatalf("build %x: %v", id[:4], err)
				}
				if !p.Verify() {
					t.Fatalf("proof for %x does not fold to root", id[:4])
				}
				if p.BlockRoot != b.MerkleRoot {
					t.Fatalf("proof block root mismatch")
				}
			}
		}
	}
}

func TestTamperFails(t *testing.T) {
	st, blocks := build(t)
	id := blocks[0].Subtrees[0].TxIDs[0]
	p, err := proof.Build(st, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.L1) == 0 {
		t.Skip("degenerate subtree")
	}
	p.L1[0].Sibling[0] ^= 0xFF // flip a sibling bit
	if p.Verify() {
		t.Fatal("tampered proof still verified")
	}
}

func TestNotMined(t *testing.T) {
	st, _ := build(t)
	var unknown [32]byte
	unknown[0] = 0xDE
	if _, err := proof.Build(st, unknown); err != proof.ErrNotMined {
		t.Fatalf("want ErrNotMined, got %v", err)
	}
}
