package mem

import (
	"sync"
	"testing"

	"memserve/commitment"
	"memserve/store"
)

func h(b ...byte) store.Hash { return commitment.DoubleSHA256(b) }

func TestTxIndexRoundTrip(t *testing.T) {
	s := New()
	id := h(1)
	if _, ok, _ := s.GetTxIndex(id); ok {
		t.Fatal("empty store returned tx")
	}
	s.PutTxIndex(id, store.TxIndex{Mined: true, Height: 42})
	ix, ok, _ := s.GetTxIndex(id)
	if !ok || !ix.Mined || ix.Height != 42 {
		t.Fatalf("got %+v ok=%v", ix, ok)
	}
}

func TestUTXOSpendUnspendPrune(t *testing.T) {
	s := New()
	o := store.Outpoint{TxID: h(2), Vout: 1}
	s.PutUTXO(o, store.UTXO{Value: 500})
	if u, ok, _ := s.GetUTXO(o); !ok || u.Value != 500 || u.Spent {
		t.Fatalf("put utxo: %+v ok=%v", u, ok)
	}
	if ok, _ := s.SpendUTXO(o, h(3), 100); !ok {
		t.Fatal("spend failed")
	}
	if u, _, _ := s.GetUTXO(o); !u.Spent || u.SpentHeight != 100 {
		t.Fatalf("spend not recorded: %+v", u)
	}
	// pruning a different height does nothing.
	if n, _ := s.PruneSpentAtHeight(99); n != 0 {
		t.Fatalf("pruned wrong height: %d", n)
	}
	// pruning the right height evicts it.
	if n, _ := s.PruneSpentAtHeight(100); n != 1 {
		t.Fatalf("prune at 100 = %d, want 1", n)
	}
	if _, ok, _ := s.GetUTXO(o); ok {
		t.Fatal("record survived prune")
	}
}

func TestUnspendRemovesFromSpentIndex(t *testing.T) {
	s := New()
	o := store.Outpoint{TxID: h(4), Vout: 0}
	s.PutUTXO(o, store.UTXO{Value: 1})
	s.SpendUTXO(o, h(5), 200)
	if ok, _ := s.UnspendUTXO(o); !ok {
		t.Fatal("unspend failed")
	}
	// after unspend, pruning at 200 must NOT evict (it is unspent again).
	if n, _ := s.PruneSpentAtHeight(200); n != 0 {
		t.Fatalf("unspent record pruned: %d", n)
	}
	if _, ok, _ := s.GetUTXO(o); !ok {
		t.Fatal("unspent record gone")
	}
}

func TestConcurrentAccess(t *testing.T) {
	s := New()
	var wg sync.WaitGroup
	for w := 0; w < 16; w++ {
		wg.Add(1)
		go func(base int) {
			defer wg.Done()
			for i := 0; i < 1000; i++ {
				id := h(byte(base), byte(i), byte(i>>8))
				s.PutTxIndex(id, store.TxIndex{Mined: true, Height: uint32(i)})
				if _, ok, _ := s.GetTxIndex(id); !ok {
					t.Errorf("lost write")
					return
				}
			}
		}(w)
	}
	wg.Wait()
	if st := s.Stats(); st.TxIndex != 16*1000 {
		t.Fatalf("txindex count = %d", st.TxIndex)
	}
}

func TestHeaderAndBlock(t *testing.T) {
	s := New()
	var hdr [80]byte
	hdr[0] = 0xAB
	s.PutHeader(7, hdr)
	if got, ok, _ := s.GetHeader(7); !ok || got[0] != 0xAB {
		t.Fatal("header round trip")
	}
	b := store.BlockRec{Hash: h(9), Height: 7, SubtreeRoots: []store.Hash{h(10)}}
	s.PutBlock(b)
	if got, ok, _ := s.GetBlock(h(9)); !ok || got.Height != 7 {
		t.Fatalf("block round trip: %+v ok=%v", got, ok)
	}
}
