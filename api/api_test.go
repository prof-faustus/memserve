package api_test

import (
	"testing"

	"memserve/api"
	"memserve/commitment"
	"memserve/store"
	"memserve/store/mem"
)

func TestUTXOStatuses(t *testing.T) {
	st := mem.New()
	srv := api.New(st, 10)

	unspent := store.Outpoint{TxID: commitment.DoubleSHA256([]byte("a")), Vout: 0}
	spent := store.Outpoint{TxID: commitment.DoubleSHA256([]byte("b")), Vout: 0}
	prunedTx := commitment.DoubleSHA256([]byte("c"))
	pruned := store.Outpoint{TxID: prunedTx, Vout: 0}
	unknown := store.Outpoint{TxID: commitment.DoubleSHA256([]byte("z")), Vout: 0}

	st.PutUTXO(unspent, store.UTXO{Value: 42})
	st.PutUTXO(spent, store.UTXO{Value: 7})
	st.SpendUTXO(spent, commitment.DoubleSHA256([]byte("sp")), 5)
	// pruned: tx is mined (in index) but the UTXO record is absent (evicted).
	st.PutTxIndex(prunedTx, store.TxIndex{Mined: true, Height: 1})

	if r, _ := srv.UTXO(unspent); r.Status != api.UTXOUnspent || r.Value != 42 {
		t.Fatalf("unspent => %+v", r)
	}
	if r, _ := srv.UTXO(spent); r.Status != api.UTXOSpent || r.SpentHeight != 5 {
		t.Fatalf("spent => %+v", r)
	}
	if r, _ := srv.UTXO(pruned); r.Status != api.UTXONotInWindow {
		t.Fatalf("pruned => %s (want not-in-retained-window)", r.Status)
	}
	if r, _ := srv.UTXO(unknown); r.Status != api.UTXOUnknown {
		t.Fatalf("unknown => %s", r.Status)
	}
}

func TestSeenMined(t *testing.T) {
	st := mem.New()
	srv := api.New(st, 0)
	id := commitment.DoubleSHA256([]byte("tx"))
	if r, _ := srv.Seen(id); r.Seen {
		t.Fatal("unknown tx reported seen")
	}
	// seen-but-not-mined (mempool).
	st.PutTxIndex(id, store.TxIndex{Mined: false, SeenTime: 123})
	if r, _ := srv.Seen(id); !r.Seen || r.SeenTime != 123 {
		t.Fatalf("seen = %+v", r)
	}
	if r, _ := srv.Mined(id); r.Mined {
		t.Fatal("mempool tx reported mined")
	}
	// now mined.
	st.PutTxIndex(id, store.TxIndex{Mined: true, Height: 9, BlockTime: 111})
	if r, _ := srv.Mined(id); !r.Mined || r.Height != 9 || r.BlockTime != 111 {
		t.Fatalf("mined = %+v", r)
	}
}
