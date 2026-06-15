// Package storetest is a shared conformance suite for any store.Store backend. Both
// store/mem and store/aerospike run it, so the Aerospike adapter is validated against the
// exact same contract as the tested in-memory reference the moment a cluster is available.
// BSV only.
package storetest

import (
	"testing"

	"memserve/commitment"
	"memserve/store"
)

func h(b ...byte) store.Hash { return commitment.DoubleSHA256(b) }

// RunSuite exercises the full store.Store contract against a fresh store from newStore.
func RunSuite(t *testing.T, newStore func() store.Store) {
	t.Run("TxIndex", func(t *testing.T) { testTxIndex(t, newStore()) })
	t.Run("UTXOLifecycle", func(t *testing.T) { testUTXO(t, newStore()) })
	t.Run("Prune", func(t *testing.T) { testPrune(t, newStore()) })
	t.Run("RollbackDeletes", func(t *testing.T) { testDeletes(t, newStore()) })
	t.Run("SubtreeBlockHeader", func(t *testing.T) { testSubtreeBlockHeader(t, newStore()) })
	t.Run("PruneIndexAtHeight", func(t *testing.T) { testPruneIndex(t, newStore()) })
}

func testPruneIndex(t *testing.T, s store.Store) {
	root := h(70)
	s.PutSubtree(root, []store.Hash{h(71), h(72)})
	s.PutBlock(store.BlockRec{Hash: h(73), Height: 7, MerkleRoot: h(74), SubtreeRoots: []store.Hash{root}})
	var hdr [80]byte
	s.PutHeader(7, hdr)
	s.PutTxIndex(h(71), store.TxIndex{Mined: true, Height: 7, BlockHash: h(73)})
	s.PutTxIndex(h(72), store.TxIndex{Mined: true, Height: 7, BlockHash: h(73)})
	// an unrelated block at height 8 must survive.
	s.PutBlock(store.BlockRec{Hash: h(80), Height: 8, SubtreeRoots: []store.Hash{}})
	s.PutTxIndex(h(81), store.TxIndex{Mined: true, Height: 8, BlockHash: h(80)})

	n, err := s.PruneIndexAtHeight(7)
	if err != nil {
		t.Fatal(err)
	}
	if n != 5 { // block + subtree + 2 txindex + header
		t.Fatalf("PruneIndexAtHeight freed %d records, want 5", n)
	}
	if _, ok, _ := s.GetBlock(h(73)); ok {
		t.Fatal("block survived index prune")
	}
	if _, ok, _ := s.GetSubtree(root); ok {
		t.Fatal("subtree survived index prune")
	}
	if _, ok, _ := s.GetTxIndex(h(71)); ok {
		t.Fatal("txindex survived index prune")
	}
	if _, ok, _ := s.GetHeader(7); ok {
		t.Fatal("header survived index prune")
	}
	// height 8 data is untouched.
	if _, ok, _ := s.GetTxIndex(h(81)); !ok {
		t.Fatal("unrelated height pruned")
	}
}

func testTxIndex(t *testing.T, s store.Store) {
	id := h(1)
	if _, ok, _ := s.GetTxIndex(id); ok {
		t.Fatal("empty store returned a tx")
	}
	want := store.TxIndex{Mined: true, Height: 42, SubtreeIndex: 1, LeafIndex: 7, BlockHash: h(9)}
	if err := s.PutTxIndex(id, want); err != nil {
		t.Fatal(err)
	}
	got, ok, _ := s.GetTxIndex(id)
	if !ok || got.Height != 42 || got.SubtreeIndex != 1 || got.LeafIndex != 7 || got.BlockHash != want.BlockHash {
		t.Fatalf("txindex round trip: %+v ok=%v", got, ok)
	}
}

func testUTXO(t *testing.T, s store.Store) {
	op := store.Outpoint{TxID: h(2), Vout: 3}
	if err := s.PutUTXO(op, store.UTXO{Value: 500, ScriptHash: h(8)}); err != nil {
		t.Fatal(err)
	}
	u, ok, _ := s.GetUTXO(op)
	if !ok || u.Value != 500 || u.Spent {
		t.Fatalf("put utxo: %+v ok=%v", u, ok)
	}
	if ok, _ := s.SpendUTXO(op, h(3), 100); !ok {
		t.Fatal("spend failed")
	}
	if u, _, _ := s.GetUTXO(op); !u.Spent || u.SpentHeight != 100 || u.SpentBy != h(3) {
		t.Fatalf("spend not recorded: %+v", u)
	}
	if ok, _ := s.UnspendUTXO(op); !ok {
		t.Fatal("unspend failed")
	}
	if u, _, _ := s.GetUTXO(op); u.Spent {
		t.Fatal("still spent after unspend")
	}
	// spend at a missing outpoint returns false.
	if ok, _ := s.SpendUTXO(store.Outpoint{TxID: h(99)}, h(3), 1); ok {
		t.Fatal("spend of missing outpoint reported ok")
	}
}

func testPrune(t *testing.T, s store.Store) {
	a := store.Outpoint{TxID: h(10)}
	b := store.Outpoint{TxID: h(11)}
	s.PutUTXO(a, store.UTXO{Value: 1})
	s.PutUTXO(b, store.UTXO{Value: 1})
	s.SpendUTXO(a, h(20), 100)
	s.SpendUTXO(b, h(21), 101)
	// pruning a non-matching height evicts nothing.
	if n, _ := s.PruneSpentAtHeight(99); n != 0 {
		t.Fatalf("pruned wrong height: %d", n)
	}
	if n, _ := s.PruneSpentAtHeight(100); n != 1 {
		t.Fatalf("prune at 100 = %d, want 1", n)
	}
	if _, ok, _ := s.GetUTXO(a); ok {
		t.Fatal("a survived prune")
	}
	if _, ok, _ := s.GetUTXO(b); !ok {
		t.Fatal("b (different height) wrongly pruned")
	}
	// an unspent UTXO at the same height number is never pruned (not in spent index).
	c := store.Outpoint{TxID: h(12)}
	s.PutUTXO(c, store.UTXO{Value: 1})
	if n, _ := s.PruneSpentAtHeight(101); n != 1 { // only b
		t.Fatalf("prune at 101 = %d, want 1", n)
	}
	if _, ok, _ := s.GetUTXO(c); !ok {
		t.Fatal("unspent c was pruned")
	}
}

func testDeletes(t *testing.T, s store.Store) {
	op := store.Outpoint{TxID: h(30), Vout: 1}
	s.PutUTXO(op, store.UTXO{Value: 9})
	s.PutTxIndex(h(30), store.TxIndex{Mined: true, Height: 5})
	if ok, _ := s.DeleteUTXO(op); !ok {
		t.Fatal("delete utxo failed")
	}
	if _, ok, _ := s.GetUTXO(op); ok {
		t.Fatal("utxo present after delete")
	}
	if ok, _ := s.DeleteTxIndex(h(30)); !ok {
		t.Fatal("delete txindex failed")
	}
	if _, ok, _ := s.GetTxIndex(h(30)); ok {
		t.Fatal("txindex present after delete")
	}
	// deleting a spent UTXO must also clear it from the spent index (no phantom prune).
	op2 := store.Outpoint{TxID: h(31)}
	s.PutUTXO(op2, store.UTXO{Value: 1})
	s.SpendUTXO(op2, h(40), 200)
	s.DeleteUTXO(op2)
	if n, _ := s.PruneSpentAtHeight(200); n != 0 {
		t.Fatalf("phantom prune of deleted spent utxo: %d", n)
	}
}

func testSubtreeBlockHeader(t *testing.T, s store.Store) {
	root := h(50)
	leaves := []store.Hash{h(51), h(52), h(53)}
	if err := s.PutSubtree(root, leaves); err != nil {
		t.Fatal(err)
	}
	got, ok, _ := s.GetSubtree(root)
	if !ok || len(got) != 3 || got[1] != h(52) {
		t.Fatalf("subtree round trip: %v ok=%v", got, ok)
	}
	blk := store.BlockRec{Hash: h(60), Height: 7, Time: 123, MerkleRoot: h(61), SubtreeRoots: []store.Hash{root}}
	blk.Header[0] = 0xAB
	if err := s.PutBlock(blk); err != nil {
		t.Fatal(err)
	}
	gb, ok, _ := s.GetBlock(h(60))
	if !ok || gb.Height != 7 || gb.MerkleRoot != h(61) || len(gb.SubtreeRoots) != 1 || gb.Header[0] != 0xAB {
		t.Fatalf("block round trip: %+v ok=%v", gb, ok)
	}
	var hdr [80]byte
	hdr[5] = 0xCD
	if err := s.PutHeader(7, hdr); err != nil {
		t.Fatal(err)
	}
	if gh, ok, _ := s.GetHeader(7); !ok || gh[5] != 0xCD {
		t.Fatalf("header round trip: ok=%v", ok)
	}
}
