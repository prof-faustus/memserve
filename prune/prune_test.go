package prune

import (
	"testing"

	"memserve/commitment"
	"memserve/store"
	"memserve/store/mem"
)

func op(i int) store.Outpoint {
	return store.Outpoint{TxID: commitment.DoubleSHA256([]byte{byte(i), byte(i >> 8)}), Vout: 0}
}

func TestPolicyWithDFloor(t *testing.T) {
	// D below the reorg horizon must be refused (the hidden-defect guard, §11.2).
	if _, err := PolicyWithD(3, 6); err != ErrBelowFloor {
		t.Fatalf("want ErrBelowFloor for D<reorg, got %v", err)
	}
	// D == reorg horizon is allowed (RecencyWindow 0).
	p, err := PolicyWithD(6, 6)
	if err != nil || p.D() != 6 || p.RecencyWindow != 0 {
		t.Fatalf("PolicyWithD(6,6) = %+v, %v", p, err)
	}
	// D = 0 with reorg 0 (strict UTXO set) is allowed.
	p0, err := PolicyWithD(0, 0)
	if err != nil || p0.D() != 0 {
		t.Fatalf("PolicyWithD(0,0) = %+v, %v", p0, err)
	}
}

func TestOnNewBlockEvictsCorrectBand(t *testing.T) {
	st := mem.New()
	// spend an output at height 100.
	st.PutUTXO(op(1), store.UTXO{Value: 1})
	st.SpendUTXO(op(1), commitment.DoubleSHA256([]byte("spender")), 100)

	pr := New(st, Policy{ReorgHorizon: 6, RecencyWindow: 4}) // D = 10

	// tip advances to 109: spend at 100 has depth 109-100+1 = 10 = D -> retained.
	for h := uint32(101); h <= 109; h++ {
		n, _ := pr.OnNewBlock(h)
		if n != 0 {
			t.Fatalf("pruned at tip %d (depth<=D) — should retain", h)
		}
	}
	if _, ok, _ := st.GetUTXO(op(1)); !ok {
		t.Fatal("record evicted while at depth <= D")
	}
	// tip 110: depth = 110-100+1 = 11 = D+1 -> band spentHeight=110-10=100 evicted.
	n, _ := pr.OnNewBlock(110)
	if n != 1 {
		t.Fatalf("expected 1 eviction at depth D+1, got %d", n)
	}
	if _, ok, _ := st.GetUTXO(op(1)); ok {
		t.Fatal("record still present after crossing depth D")
	}
}

func TestNeverPrunesInsideReorgHorizon(t *testing.T) {
	st := mem.New()
	// spends at several heights.
	for i, h := range []uint32{50, 51, 52} {
		st.PutUTXO(op(i), store.UTXO{Value: 1})
		st.SpendUTXO(op(i), commitment.DoubleSHA256([]byte("s")), h)
	}
	reorg := uint32(6)
	pr := New(st, Policy{ReorgHorizon: reorg, RecencyWindow: 0}) // D = reorg horizon

	// Advance the tip block by block and assert nothing within the reorg horizon is gone.
	for tip := uint32(50); tip <= 70; tip++ {
		pr.OnNewBlock(tip)
		for i, h := range []uint32{50, 51, 52} {
			depth := Depth(tip, h)
			_, present, _ := st.GetUTXO(op(i))
			if depth <= reorg && !present {
				t.Fatalf("spend at h=%d evicted at depth %d (<= reorg horizon %d)", h, depth, reorg)
			}
		}
	}
}

func TestUnspendDuringWindowReorg(t *testing.T) {
	// A reorg within the window rolls a spend back: the record is still present, so
	// UnspendUTXO restores it to unspent (the reason D must cover the reorg horizon).
	st := mem.New()
	st.PutUTXO(op(7), store.UTXO{Value: 9})
	st.SpendUTXO(op(7), commitment.DoubleSHA256([]byte("s")), 200)
	pr := New(st, Policy{ReorgHorizon: 6, RecencyWindow: 0})
	pr.OnNewBlock(203) // depth 4, within horizon, retained
	if ok, _ := st.UnspendUTXO(op(7)); !ok {
		t.Fatal("could not unspend within window")
	}
	u, ok, _ := st.GetUTXO(op(7))
	if !ok || u.Spent {
		t.Fatalf("unspend failed: %+v ok=%v", u, ok)
	}
}

func TestOnNewBlockBackfillsTipJump(t *testing.T) {
	// Defect-1 regression: spends across several heights must all be pruned even when
	// the tip jumps (non-consecutive OnNewBlock calls), or memory leaks.
	st := mem.New()
	heights := []uint32{100, 101, 102, 103, 104}
	for i, h := range heights {
		st.PutUTXO(op(i), store.UTXO{Value: 1})
		st.SpendUTXO(op(i), commitment.DoubleSHA256([]byte("s")), h)
	}
	pr := New(st, Policy{ReorgHorizon: 6, RecencyWindow: 0}) // D=6

	// establish a baseline below any spend's eviction point.
	pr.OnNewBlock(105) // target band 99 (empty)
	// now JUMP the tip far ahead in one call: all of 100..104 reach depth > 6.
	n, err := pr.OnNewBlock(200)
	if err != nil {
		t.Fatal(err)
	}
	if n != len(heights) {
		t.Fatalf("tip jump pruned %d bands, want %d (gap leaked!)", n, len(heights))
	}
	for i := range heights {
		if _, ok, _ := st.GetUTXO(op(i)); ok {
			t.Fatalf("spend %d survived the tip jump — leak", i)
		}
	}
}

func TestIndexRetentionSweep(t *testing.T) {
	st := mem.New()
	// a block at height 100 with a subtree, two txs and a header.
	root := commitment.DoubleSHA256([]byte("subroot"))
	st.PutSubtree(root, []store.Hash{commitment.DoubleSHA256([]byte("t1"))})
	st.PutBlock(store.BlockRec{Hash: commitment.DoubleSHA256([]byte("blk100")), Height: 100, SubtreeRoots: []store.Hash{root}})
	st.PutHeader(100, [80]byte{})
	st.PutTxIndex(commitment.DoubleSHA256([]byte("t1")), store.TxIndex{Mined: true, Height: 100})

	pr := New(st, Policy{ReorgHorizon: 6, RecencyWindow: 0, IndexRetention: 10})
	// below index depth: nothing pruned.
	for tip := uint32(100); tip <= 109; tip++ {
		pr.OnNewBlock(tip)
	}
	if _, ok, _ := st.GetBlock(commitment.DoubleSHA256([]byte("blk100"))); !ok {
		t.Fatal("block index pruned within retention window")
	}
	// tip 110: depth of height-100 data = 11 > 10 -> pruned.
	pr.OnNewBlock(110)
	if _, ok, _ := st.GetBlock(commitment.DoubleSHA256([]byte("blk100"))); ok {
		t.Fatal("block index NOT pruned past retention depth")
	}
	if _, ok, _ := st.GetTxIndex(commitment.DoubleSHA256([]byte("t1"))); ok {
		t.Fatal("txindex NOT pruned past retention depth")
	}
	if _, ok, _ := st.GetSubtree(root); ok {
		t.Fatal("subtree NOT pruned past retention depth")
	}
}

func TestIndexRetentionBoundsStoreDuringIngest(t *testing.T) {
	// With IndexRetention set, txindex/blocks stay bounded over a long ingest instead of
	// growing forever (the memory-runaway fix, by design).
	st := mem.New()
	pol := Policy{ReorgHorizon: 4, RecencyWindow: 0, IndexRetention: 8}
	pr := New(st, pol)
	for h := uint32(1); h <= 200; h++ {
		st.PutBlock(store.BlockRec{Hash: commitment.DoubleSHA256([]byte{byte(h), byte(h >> 8)}), Height: h, SubtreeRoots: []store.Hash{}})
		st.PutHeader(h, [80]byte{})
		for i := 0; i < 16; i++ {
			id := commitment.DoubleSHA256([]byte{byte(h), byte(h >> 8), byte(i), 0x7a})
			st.PutTxIndex(id, store.TxIndex{Mined: true, Height: h})
		}
		pr.OnNewBlock(h)
	}
	s := st.Stats()
	// retained ~ IndexRetention blocks worth, NOT all 200 (which would be 3200 txindex).
	if s.TxIndex > 16*int(pol.IndexRetention+4) {
		t.Fatalf("txindex not bounded by retention: %d", s.TxIndex)
	}
	if s.Blocks > int(pol.IndexRetention+4) {
		t.Fatalf("blocks not bounded by retention: %d", s.Blocks)
	}
}

func TestDepth(t *testing.T) {
	if d := Depth(110, 100); d != 11 {
		t.Fatalf("Depth(110,100)=%d want 11", d)
	}
	if d := Depth(100, 100); d != 1 {
		t.Fatalf("Depth(100,100)=%d want 1", d)
	}
	if d := Depth(99, 100); d != 0 {
		t.Fatalf("Depth below = %d want 0", d)
	}
}
