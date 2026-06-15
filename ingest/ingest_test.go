package ingest_test

import (
	"testing"

	"memserve/api"
	"memserve/ingest"
	"memserve/prune"
	"memserve/shard"
	"memserve/store"
	"memserve/store/mem"
	"memserve/teranode"
)

func TestIngestAndQuery(t *testing.T) {
	st := mem.New()
	pr := prune.New(st, prune.Policy{})
	in := ingest.New(st, pr, ingest.Config{})
	src := teranode.NewMock(teranode.MockConfig{Blocks: 3, SubtreesPer: 2, TxsPerSubtree: 16, SpendFraction: 2})
	total, err := in.Run(src)
	if err != nil {
		t.Fatal(err)
	}
	if total.TxsIndexed != 3*2*16 {
		t.Fatalf("indexed %d", total.TxsIndexed)
	}
	srv := api.New(st, 0)
	// re-run a fresh source to learn a real txid to query.
	probe := teranode.NewMock(teranode.MockConfig{Blocks: 3, SubtreesPer: 2, TxsPerSubtree: 16, SpendFraction: 2})
	b, _, _ := probe.Next()
	id := b.Subtrees[0].TxIDs[0]
	if r, _ := srv.Seen(id); !r.Seen {
		t.Fatal("ingested tx not seen")
	}
	if r, _ := srv.Mined(id); !r.Mined || r.Height != 0 {
		t.Fatalf("mined = %+v", r)
	}
}

func TestPruningDuringIngest(t *testing.T) {
	st := mem.New()
	pol := prune.Policy{ReorgHorizon: 2, RecencyWindow: 0} // D=2: aggressive pruning
	pr := prune.New(st, pol)
	in := ingest.New(st, pr, ingest.Config{})
	src := teranode.NewMock(teranode.MockConfig{Blocks: 30, SubtreesPer: 2, TxsPerSubtree: 64, SpendFraction: 2})
	if _, err := in.Run(src); err != nil {
		t.Fatal(err)
	}
	if pr.TotalPruned() == 0 {
		t.Fatal("nothing pruned with D=2 over 30 blocks of spends")
	}
	// spent-retained should be bounded (only the last ~D blocks of spends), far below
	// the total spends over 30 blocks.
	s := st.Stats()
	if s.UTXOSpent > 2*64*4 { // generous bound: a few blocks of churn
		t.Fatalf("spent-retained not bounded: %d", s.UTXOSpent)
	}
}

func TestIngestRejectsPoisonedBlock(t *testing.T) {
	st := mem.New()
	in := ingest.New(st, prune.New(st, prune.Policy{}), ingest.Config{})
	src := teranode.NewMock(teranode.MockConfig{Blocks: 1, SubtreesPer: 2, TxsPerSubtree: 8})
	b, _, _ := src.Next()
	// poison: tamper a txid so the subtree no longer hashes to its claimed root.
	b.Subtrees[0].TxIDs[0][0] ^= 0xFF
	if _, err := in.IngestBlock(b); err != ingest.ErrInvalidBlock {
		t.Fatalf("poisoned block not rejected: %v", err)
	}
	// nothing should have been stored.
	if s := st.Stats(); s.TxIndex != 0 || s.Blocks != 0 {
		t.Fatalf("poisoned block left state: %+v", s)
	}
}

func TestReorgRollbackWithinWindow(t *testing.T) {
	st := mem.New()
	// D=6, no eviction inside the reorg horizon.
	pr := prune.New(st, prune.Policy{ReorgHorizon: 6})
	in := ingest.New(st, pr, ingest.Config{})
	src := teranode.NewMock(teranode.MockConfig{Blocks: 3, SubtreesPer: 1, TxsPerSubtree: 16, SpendFraction: 2})
	var blocks []teranode.Block
	for {
		b, ok, _ := src.Next()
		if !ok {
			break
		}
		in.IngestBlock(b)
		blocks = append(blocks, b)
	}
	last := blocks[len(blocks)-1]
	// find an outpoint spent by the last block, confirm it is spent, then roll back.
	var spent store.Outpoint
	found := false
	for _, sub := range last.Subtrees {
		for _, tx := range sub.Txs {
			if len(tx.Inputs) > 0 {
				spent = tx.Inputs[0]
				found = true
			}
		}
	}
	if !found {
		t.Skip("last block spent nothing")
	}
	if u, ok, _ := st.GetUTXO(spent); !ok || !u.Spent {
		t.Fatalf("expected spent outpoint before rollback: %+v ok=%v", u, ok)
	}
	if err := in.RollbackBlock(last); err != nil {
		t.Fatal(err)
	}
	// after rollback the spend is reverted (output unspent again) — correct because the
	// record was still retained (D covered the depth).
	if u, ok, _ := st.GetUTXO(spent); !ok || u.Spent {
		t.Fatalf("rollback did not revert the spend: %+v ok=%v", u, ok)
	}
	// the rolled-back block's own tx is gone from the index.
	if r, _ := api.New(st, 0).Mined(last.Subtrees[0].TxIDs[0]); r.Mined {
		t.Fatal("rolled-back tx still indexed as mined")
	}
}

func TestEstimateRetained(t *testing.T) {
	p := prune.Policy{ReorgHorizon: 6, RecencyWindow: 4} // D=10
	if got := prune.EstimateRetainedSpends(1000, p); got != 10000 {
		t.Fatalf("estimate = %d, want 10000", got)
	}
}

func TestShardedIngestOwnsOnlyItsPrefix(t *testing.T) {
	const k = 2
	src := teranode.NewMock(teranode.MockConfig{Blocks: 2, SubtreesPer: 2, TxsPerSubtree: 50})
	// ingest shard 1 only.
	st := mem.New()
	in := ingest.New(st, prune.New(st, prune.Policy{}), ingest.Config{K: k, ID: 1})
	src2 := teranode.NewMock(teranode.MockConfig{Blocks: 2, SubtreesPer: 2, TxsPerSubtree: 50})
	for {
		b, ok, _ := src.Next()
		if !ok {
			break
		}
		in.IngestBlock(b)
	}
	srv := api.New(st, 0)
	// every indexed tx must belong to shard 1.
	for {
		b, ok, _ := src2.Next()
		if !ok {
			break
		}
		for _, sub := range b.Subtrees {
			for _, id := range sub.TxIDs {
				seen, _ := srv.Seen(id)
				owned := shard.Of(id, k) == 1
				if seen.Seen != owned {
					t.Fatalf("tx %x: seen=%v owned=%v", id[:4], seen.Seen, owned)
				}
			}
		}
	}
	_ = store.Outpoint{}
}
