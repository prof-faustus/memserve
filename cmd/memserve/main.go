// Command memserve demonstrates and benchmarks the MemServe shard server:
//
//   - end-to-end: ingest a mock chain, then answer Seen / Mined / MerklePath / UTXO;
//   - payment: prepay-then-serve over a real BSV payment channel (secp256k1);
//   - pruning: show the honest "not-in-retained-window" answer after eviction;
//   - throughput: real lookups/s per query, single + all cores, + shard extrapolation.
//
// BSV/Teranode only.
//
//	go run ./cmd/memserve
//	go run ./cmd/memserve -dur 2s -blocks 50
package main

import (
	"flag"
	"fmt"
	"runtime"
	"time"

	"memserve/accel"
	"memserve/api"
	"memserve/bench"
	"memserve/commitment"
	"memserve/crypto"
	"memserve/payment"
	"memserve/payment/channel"
	"memserve/shard"
	"memserve/teranode"
)

func main() {
	dur := flag.Duration("dur", 1500*time.Millisecond, "measurement window per query kind")
	blocks := flag.Int("blocks", 40, "blocks to ingest for the benchmark")
	subtrees := flag.Int("subtrees", 4, "subtrees per block")
	txs := flag.Int("txs", 2048, "txs per subtree")
	cores := flag.Int("cores", runtime.NumCPU(), "parallel query workers")
	flag.Parse()

	cfg := teranode.MockConfig{Blocks: *blocks, SubtreesPer: *subtrees, TxsPerSubtree: *txs, SpendFraction: 3}
	fmt.Printf("# MemServe (BSV/Teranode) — cores=%d\n", *cores)
	fmt.Printf("ingesting mock chain: %d blocks x %d subtrees x %d txs ...\n", *blocks, *subtrees, *txs)
	p, err := bench.Build(cfg, 6, 4) // reorg horizon 6, recency 4 => D=10
	if err != nil {
		fmt.Println("build error:", err)
		return
	}
	s := p.Store.Stats()
	fmt.Printf("ingested: %d txs indexed, %d live UTXOs, %d spent-retained, %d blocks. tip=%d\n\n",
		s.TxIndex, s.UTXOLive, s.UTXOSpent, s.Blocks, p.Tip)

	// --- end-to-end correctness sample ---
	demoQueries(p)

	// --- payment: prepay-then-serve ---
	demoPayment(p)

	// --- throughput per query kind ---
	fmt.Printf("\n## Lookup throughput (real queries from memory)\n")
	fmt.Printf("%-12s %-14s %-14s\n", "query", "1-core/s", fmt.Sprintf("%d-core/s", *cores))
	var bestParallel float64
	for _, k := range []bench.QueryKind{bench.KSeen, bench.KMined, bench.KMerklePath, bench.KUTXO} {
		one := p.Throughput(k, 1, *dur)
		all := p.Throughput(k, *cores, *dur)
		if all > bestParallel {
			bestParallel = all
		}
		fmt.Printf("%-12s %-14.3e %-14.3e\n", k.String(), one, all)
	}

	// --- paid path (sig-bound, honest) ---
	paid := p.PaidThroughput(*cores, *dur)
	fmt.Printf("\n## Paid path — prepay-then-serve with real secp256k1 verify (%d cores)\n", *cores)
	fmt.Printf("  %.3e paid-answers/s  (sig-bound: this is the true metered-access cost, far below\n", paid)
	fmt.Printf("   the free in-memory lookup rate above).\n")

	// --- accel: batch signature verification (the GPU/accel target) ---
	demoAccel(*cores, *dur)

	// --- shares-nothing shard extrapolation ---
	fmt.Printf("\n## Shares-nothing shard extrapolation (per-shard rate x 2^k shards)\n")
	fmt.Printf("  per-shard (this box, best query) ~= %.3e answers/s\n", bestParallel)
	for _, k := range []uint{1, 2, 3, 5, 7, 10} {
		n := shard.Count(k)
		fmt.Printf("    k=%-2d -> %-10d shards -> %.3e answers/s\n", k, n, bestParallel*float64(n))
	}
	fmt.Printf("  (hash-prefix sharding: uniform load, stateless routing, elastic split — DESIGN §6.)\n")
}

func demoAccel(cores int, dur time.Duration) {
	fmt.Printf("\n## Signature-verify acceleration (accel) — the GPU target\n")
	if err := accel.Validate(accel.NewCPU(), 256); err != nil {
		fmt.Println("  CPU backend FAILED the correctness gate:", err)
		return
	}
	fmt.Printf("  correctness gate: CPU backend passes accel.Validate (vs the Go reference)\n")
	refBatch := accel.MakeBatch(64)
	cpuBatch := accel.MakeBatch(2048)
	ref := accel.Throughput(accel.Reference{}, refBatch, dur)
	cpu := accel.Throughput(accel.NewCPU(), cpuBatch, dur)
	fmt.Printf("  reference (serial, 1 core): %.3e verify/s\n", ref)
	fmt.Printf("  CPU batch (%d cores):        %.3e verify/s  (%.1fx)\n", cores, cpu, cpu/ref)
	fmt.Printf("  NB: the pure-Go math/big verifier is slow (~1e2/core) and scales sub-linearly\n")
	fmt.Printf("   (allocator/GC pressure) — which is exactly why this is the GPU/libsecp256k1 target.\n")
	fmt.Printf("  GPU (accel.CUDA, -tags cuda) drops in behind accel.Gate once it passes the same\n")
	fmt.Printf("   gate; literature puts batched GPU secp256k1 at ~1e5-1e6 verify/s/card.\n")
}

func demoQueries(p *bench.Populated) {
	fmt.Printf("## End-to-end sample (ingest -> serve -> verify)\n")
	txid := p.TxIDs[len(p.TxIDs)/2]
	seen, _ := p.Server.Seen(txid)
	mined, _ := p.Server.Mined(txid)
	pr, err := p.Server.MerklePath(txid)
	okFold := err == nil && pr.Verify()
	op := p.Outpts[len(p.Outpts)/2]
	u, _ := p.Server.UTXO(op)
	fmt.Printf("  Seen=%v  Mined=%v@h%d  MerklePath verifies=%v (depth=%d)  UTXO=%s\n",
		seen.Seen, mined.Mined, mined.Height, okFold, len(pr.L1)+len(pr.L2), u.Status)

	// honest pruned semantics: a spent + pruned outpoint => not-in-retained-window.
	var found bool
	for _, sop := range p.Outpts {
		if r, _ := p.Server.UTXO(sop); r.Status == api.UTXONotInWindow {
			fmt.Printf("  pruned outpoint => UTXO status = %s (honest: not 'unspent', not 'absent')\n", r.Status)
			found = true
			break
		}
	}
	if !found {
		fmt.Printf("  (no pruned outpoint in sample window; rule: pruned-spent => not-in-retained-window)\n")
	}
}

func demoPayment(p *bench.Populated) {
	fmt.Printf("\n## Payment — prepay-then-serve (BSV channel, secp256k1)\n")
	ps := payment.New(p.Server)
	priv, _ := crypto.NewPrivateKey([]byte("memserve-demo-client-key-0000001"))
	fund := commitment.DoubleSHA256([]byte("demo-funding"))
	params := channel.Params{
		ChannelID:        channel.DeriveChannelID(fund, 0),
		FundingTxID:      fund,
		FundingVout:      0,
		ServerScriptHash: commitment.DoubleSHA256([]byte("server-payee")),
	}
	pricing := channel.Pricing{Flat: false, SettleFee: 200, FeeMode: channel.FeeUpfront}
	pricing.PerType[channel.QSeen] = 1
	pricing.PerType[channel.QMined] = 1
	pricing.PerType[channel.QMerklePath] = 5
	pricing.PerType[channel.QUTXO] = 2
	ch, err := ps.OpenChannel(channel.Config{
		Params: params, Deposit: 100000, ClientPub: priv.Public(),
		Pricing: pricing, N: 8, RefundLockTime: 800000,
	})
	if err != nil {
		fmt.Println("  open error:", err)
		return
	}
	txid := p.TxIDs[0]
	for i := 0; i < 4; i++ {
		cum, err := ps.Quote(params.ChannelID, channel.QMerklePath)
		if err != nil {
			fmt.Println("  quote error:", err)
			return
		}
		c, _ := channel.SignCommitment(priv, params, cum)
		if _, err := ps.MerklePath(params.ChannelID, c, txid); err != nil {
			fmt.Println("  paid query error:", err)
			return
		}
	}
	snap := ch.Snapshot()
	settle, _ := ch.Settle()
	fmt.Printf("  4 prepaid MerklePath accesses: cumPaid=%d (incl. built-in settle fee), deposit=%d\n", snap.CumPaid, snap.Deposit)
	fmt.Printf("  settle: toServer=%d (miningFee=%d, net=%d) toClient=%d  refund nLockTime=%d (client safety net)\n",
		settle.ToServer, settle.MiningFee, settle.NetServer, settle.ToClient, ch.Refund().LockTime)
	fmt.Printf("  prepay-then-serve => server never serves an unpaid access; a stopping client loses only its own prepayment.\n")
}
