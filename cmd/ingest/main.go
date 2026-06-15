// Command ingest runs the MemServe ingest server against a (mock) Teranode source:
// it pulls sealed blocks, indexes them into the store, and drives spend-depth pruning
// — printing how pruning bounds memory as the chain advances. BSV only.
//
//	go run ./cmd/ingest -blocks 200 -reorg 6 -recency 4
package main

import (
	"flag"
	"fmt"

	"memserve/ingest"
	"memserve/prune"
	"memserve/store/mem"
	"memserve/teranode"
)

func main() {
	blocks := flag.Int("blocks", 200, "number of blocks to ingest")
	subtrees := flag.Int("subtrees", 4, "subtrees per block")
	txs := flag.Int("txs", 1024, "txs per subtree")
	spendFrac := flag.Int("spendfrac", 2, "spend 1/N of live UTXOs each block (0=none)")
	reorg := flag.Uint("reorg", 6, "reorg horizon (correctness floor, blocks)")
	recency := flag.Uint("recency", 4, "extra recency window (blocks)")
	flag.Parse()

	st := mem.New()
	pol := prune.Policy{ReorgHorizon: uint32(*reorg), RecencyWindow: uint32(*recency)}
	pr := prune.New(st, pol)
	in := ingest.New(st, pr, ingest.Config{})
	src := teranode.NewMock(teranode.MockConfig{
		Blocks: *blocks, SubtreesPer: *subtrees, TxsPerSubtree: *txs, SpendFraction: *spendFrac,
	})

	fmt.Printf("# MemServe ingest (BSV/Teranode)\n")
	fmt.Printf("prune policy: D=%d (reorg-horizon floor=%d + recency=%d)\n\n", pol.D(), pol.ReorgHorizon, pol.RecencyWindow)
	fmt.Printf("%-7s %-12s %-12s %-12s %-10s %-10s\n", "height", "txindexed", "utxo-live", "utxo-spent", "pruned/blk", "pruned-tot")

	var total ingest.Stats
	h := 0
	for {
		b, ok, err := src.Next()
		if err != nil {
			fmt.Println("source error:", err)
			return
		}
		if !ok {
			break
		}
		s, err := in.IngestBlock(b)
		if err != nil {
			fmt.Println("ingest error:", err)
			return
		}
		total.TxsIndexed += s.TxsIndexed
		total.UTXOsMade += s.UTXOsMade
		total.UTXOsSpent += s.UTXOsSpent
		total.Pruned += s.Pruned
		if h%20 == 0 || h == *blocks-1 {
			st := st.Stats()
			fmt.Printf("%-7d %-12d %-12d %-12d %-10d %-10d\n",
				b.Height, st.TxIndex, st.UTXOLive, st.UTXOSpent, s.Pruned, pr.TotalPruned())
		}
		h++
	}
	fin := st.Stats()
	fmt.Printf("\nDONE: %d blocks, %d txs indexed, %d UTXOs created, %d spent, %d pruned.\n",
		total.Blocks+h, total.TxsIndexed, total.UTXOsMade, total.UTXOsSpent, pr.TotalPruned())
	fmt.Printf("Steady-state retained: %d live UTXOs + %d spent-in-window (bounded by D=%d).\n",
		fin.UTXOLive, fin.UTXOSpent, pol.D())
	fmt.Printf("=> spent-record footprint is capped at ~D blocks of churn, not unbounded history.\n")
}
