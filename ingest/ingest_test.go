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
