// Package bench measures MemServe's real lookup throughput: answers served per
// second from the in-memory store, per query type, single-core and parallel, plus
// the shares-nothing shard extrapolation (DESIGN.md §6, §8). It also measures the
// PAID path (with real secp256k1 commitment verification) so the cost of payment is
// reported honestly, not hidden. Every measurement runs real queries against a real
// ingested store. BSV only.
package bench

import (
	"sync"
	"sync/atomic"
	"time"

	"memserve/api"
	"memserve/commitment"
	"memserve/crypto"
	"memserve/ingest"
	"memserve/payment"
	"memserve/payment/channel"
	"memserve/prune"
	"memserve/store"
	"memserve/store/mem"
	"memserve/teranode"
)

// Populated holds a store filled by real ingestion plus sample keys to query.
type Populated struct {
	Store  *mem.Store
	Server *api.Server
	TxIDs  []store.Hash     // mined txids (for Seen/Mined/MerklePath)
	Outpts []store.Outpoint // live outpoints (for UTXO)
	Stats  ingest.Stats
	Tip    uint32
}

// Build ingests a deterministic mock chain into a fresh in-memory store and collects
// sample keys. reorgHorizon/recencyWindow set the prune policy (0/0 = no pruning yet
// until depth reached). Returns everything needed to benchmark queries.
func Build(cfg teranode.MockConfig, reorgHorizon, recencyWindow uint32) (*Populated, error) {
	st := mem.New()
	pol := prune.Policy{ReorgHorizon: reorgHorizon, RecencyWindow: recencyWindow}
	pr := prune.New(st, pol)
	in := ingest.New(st, pr, ingest.Config{})
	src := teranode.NewMock(cfg)

	var txids []store.Hash
	var outpts []store.Outpoint
	var tip uint32
	var totals ingest.Stats
	for {
		b, ok, err := src.Next()
		if err != nil {
			return nil, err
		}
		if !ok {
			break
		}
		s, err := in.IngestBlock(b)
		if err != nil {
			return nil, err
		}
		tip = b.Height
		totals.Blocks += s.Blocks
		totals.TxsIndexed += s.TxsIndexed
		totals.UTXOsMade += s.UTXOsMade
		totals.UTXOsSpent += s.UTXOsSpent
		totals.Pruned += s.Pruned
		// sample a few keys per block (cap the sample for memory).
		if len(txids) < 1<<20 {
			for _, sub := range b.Subtrees {
				for _, id := range sub.TxIDs {
					txids = append(txids, id)
				}
				for _, tx := range sub.Txs {
					outpts = append(outpts, store.Outpoint{TxID: tx.TxID, Vout: 0})
				}
			}
		}
	}
	return &Populated{
		Store:  st,
		Server: api.New(st, pol.D()),
		TxIDs:  txids,
		Outpts: outpts,
		Stats:  totals,
		Tip:    tip,
	}, nil
}

// QueryKind selects which lookup to benchmark.
type QueryKind uint8

const (
	KSeen QueryKind = iota
	KMined
	KMerklePath
	KUTXO
)

func (k QueryKind) String() string {
	switch k {
	case KSeen:
		return "Seen"
	case KMined:
		return "Mined"
	case KMerklePath:
		return "MerklePath"
	case KUTXO:
		return "UTXO"
	}
	return "?"
}

// Throughput measures answers/s for one query kind over the sample keys, run on
// `cores` workers for `dur`. Each worker cycles the sample so the store is hot.
func (p *Populated) Throughput(kind QueryKind, cores int, dur time.Duration) float64 {
	if cores < 1 {
		cores = 1
	}
	deadline := time.Now().Add(dur)
	var total uint64
	var wg sync.WaitGroup
	wg.Add(cores)
	for w := 0; w < cores; w++ {
		go func(off int) {
			defer wg.Done()
			var n uint64
			i := off
			for time.Now().Before(deadline) {
				// do a batch between clock checks to amortize time.Now.
				for b := 0; b < 256; b++ {
					switch kind {
					case KSeen:
						_, _ = p.Server.Seen(p.TxIDs[i%len(p.TxIDs)])
					case KMined:
						_, _ = p.Server.Mined(p.TxIDs[i%len(p.TxIDs)])
					case KMerklePath:
						_, _ = p.Server.MerklePath(p.TxIDs[i%len(p.TxIDs)])
					case KUTXO:
						_, _ = p.Server.UTXO(p.Outpts[i%len(p.Outpts)])
					}
					i++
				}
				n += 256
			}
			atomic.AddUint64(&total, n)
		}(w * 7919)
	}
	wg.Wait()
	secs := dur.Seconds()
	if secs <= 0 {
		secs = 1
	}
	return float64(total) / secs
}

// PaidThroughput measures the PAID path: per access it signs a commitment (client) and
// the server verifies it (real secp256k1) before serving Seen. This is sig-bound and
// reported honestly — it is the true cost of metered access, far below the free-lookup
// rate. Returns paid answers/s.
func (p *Populated) PaidThroughput(cores int, dur time.Duration) float64 {
	if cores < 1 {
		cores = 1
	}
	deadline := time.Now().Add(dur)
	var total uint64
	var wg sync.WaitGroup
	wg.Add(cores)
	for w := 0; w < cores; w++ {
		go func(off int) {
			defer wg.Done()
			ps, priv, params := p.newPaidChannel(uint64(off) + 1)
			var n uint64
			i := off
			cum := uint64(0)
			for time.Now().Before(deadline) {
				cum++ // flat service price 1, no settle fee for this micro-bench channel
				c, err := channel.SignCommitment(priv, params, cum)
				if err != nil {
					return
				}
				if _, err := ps.Seen(params.ChannelID, c, p.TxIDs[i%len(p.TxIDs)]); err != nil {
					// channel exhausted: reopen fresh (rare; deposit is large).
					ps, priv, params = p.newPaidChannel(uint64(off) + 1)
					cum = 0
					continue
				}
				i++
				n++
			}
			atomic.AddUint64(&total, n)
		}(w * 7919)
	}
	wg.Wait()
	secs := dur.Seconds()
	if secs <= 0 {
		secs = 1
	}
	return float64(total) / secs
}

// newPaidChannel builds a per-worker PaidServer with one open channel using a real
// secp256k1 client key. The channel carries a large deposit and N so it does not
// exhaust mid-measurement; pricing is flat-1 with no settle fee for the micro-bench.
func (p *Populated) newPaidChannel(seed uint64) (*payment.PaidServer, *crypto.PrivateKey, channel.Params) {
	ps := payment.New(p.Server)
	var sd [32]byte
	for i := 0; i < 8; i++ {
		sd[i] = byte(seed >> (8 * i))
	}
	sd[31] = 1
	priv, _ := crypto.NewPrivateKey(sd[:])
	fund := commitment.DoubleSHA256(append([]byte("memserve-bench-funding"), sd[:]...))
	params := channel.Params{
		ChannelID:        channel.DeriveChannelID(fund, 0),
		FundingTxID:      fund,
		FundingVout:      0,
		ServerScriptHash: commitment.DoubleSHA256([]byte("server-payee")),
	}
	_, _ = ps.OpenChannel(channel.Config{
		Params:    params,
		Deposit:   1 << 50,
		ClientPub: priv.Public(),
		Pricing:   channel.Pricing{Flat: true, FlatPrice: 1, SettleFee: 0, FeeMode: channel.FeeUpfront},
		N:         1 << 50,
	})
	return ps, priv, params
}
