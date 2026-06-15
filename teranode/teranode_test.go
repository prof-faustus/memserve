package teranode

import (
	"testing"

	"memserve/commitment"
	"memserve/proof"
)

func TestMockMerkleConsistency(t *testing.T) {
	src := NewMock(MockConfig{Blocks: 3, SubtreesPer: 3, TxsPerSubtree: 7, SpendFraction: 2})
	for {
		b, ok, err := src.Next()
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			break
		}
		// each subtree root must equal the Merkle root over its txids.
		for _, sub := range b.Subtrees {
			want, _ := commitment.MerkleRoot(sub.TxIDs)
			if want != sub.Root {
				t.Fatalf("subtree root mismatch at h%d", b.Height)
			}
			if len(sub.TxIDs) != len(sub.Txs) {
				t.Fatal("txids/txs length mismatch")
			}
		}
		// block Merkle root must equal the root over subtree roots.
		want, _ := commitment.MerkleRoot(b.SubtreeRoots)
		if want != b.MerkleRoot {
			t.Fatalf("block root mismatch at h%d", b.Height)
		}
		// header must carry the block root.
		if proof.HeaderMerkleRoot(b.Header) != b.MerkleRoot {
			t.Fatalf("header root mismatch at h%d", b.Height)
		}
	}
}

func TestMockDeterminism(t *testing.T) {
	cfg := MockConfig{Blocks: 4, SubtreesPer: 2, TxsPerSubtree: 5, SpendFraction: 3}
	a := NewMock(cfg)
	b := NewMock(cfg)
	for {
		ba, oka, _ := a.Next()
		bb, okb, _ := b.Next()
		if oka != okb {
			t.Fatal("length mismatch")
		}
		if !oka {
			break
		}
		if ba.Hash != bb.Hash || ba.MerkleRoot != bb.MerkleRoot {
			t.Fatalf("nondeterministic block at h%d", ba.Height)
		}
	}
}

func TestTipHeight(t *testing.T) {
	src := NewMock(MockConfig{Blocks: 2, StartHeight: 1000})
	if src.TipHeight() != 1000 {
		t.Fatalf("pre-tip = %d", src.TipHeight())
	}
	src.Next()
	if src.TipHeight() != 1000 {
		t.Fatalf("after block 1, tip = %d", src.TipHeight())
	}
	src.Next()
	if src.TipHeight() != 1001 {
		t.Fatalf("after block 2, tip = %d", src.TipHeight())
	}
}
