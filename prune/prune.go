// Package prune implements MemServe's spend-depth retention (DESIGN.md §11).
//
// Once a spend is buried D blocks deep, the spent UTXO record is freed. D is a
// per-server policy with a NAMED CORRECTNESS FLOOR (§11.2):
//
//	D = ReorgHorizon + RecencyWindow
//
// where ReorgHorizon is the deepest reorg the node commits to surviving. Pruning a
// spend shallower than the reorg horizon would let a deeper reorg roll back a spend
// whose record was already discarded — after which the node answers WRONG (reports
// an unspent it can no longer reconstruct). Expressing D this way makes the floor
// explicit and correct by construction: the pruner never evicts a spend at depth
// <= ReorgHorizon.
//
// Pruning is driven by BLOCK DEPTH, never wall-clock (§11.4): on each new sealed
// block advancing the tip to H, the band at spentHeight = H - D has just reached
// depth D+1 and is evicted. Live (unspent) outputs are never pruned. BSV only.
package prune

import (
	"errors"
	"sync/atomic"

	"memserve/store"
)

// Policy is the spend-depth retention policy. D() = ReorgHorizon + RecencyWindow.
type Policy struct {
	// ReorgHorizon is the correctness floor: the deepest reorg the node commits to
	// surviving. The pruner never evicts a spend buried <= this many blocks. The
	// node should advertise this value.
	ReorgHorizon uint32
	// RecencyWindow is extra spent-query retention on top of the floor (>= 0).
	RecencyWindow uint32
	// IndexRetention bounds the STORE BY DESIGN (§11.7): block/subtree/tx-index/header
	// data is freed once buried deeper than this many blocks. 0 = keep all (unbounded;
	// rely on a disk backend or the memory watchdog). Setting it makes Seen/Mined/
	// MerklePath answer "not in retained window" for txs older than the window — a
	// deliberate serving-window choice. Recommended >= D() so it never prunes index data
	// the spend window still needs.
	IndexRetention uint32
}

// D returns the effective prune depth: spends are evicted once buried deeper than D.
func (p Policy) D() uint32 { return p.ReorgHorizon + p.RecencyWindow }

// ErrBelowFloor is returned when a raw depth is set below the reorg horizon.
var ErrBelowFloor = errors.New("prune: depth D below the reorg-horizon correctness floor")

// PolicyWithD builds a Policy from an explicit depth D and a reorg horizon, refusing
// any configuration that would prune inside the horizon (the hidden-defect guard).
// D == 0 is permitted only when reorgHorizon == 0 (a strict UTXO set).
func PolicyWithD(d, reorgHorizon uint32) (Policy, error) {
	if d < reorgHorizon {
		return Policy{}, ErrBelowFloor
	}
	return Policy{ReorgHorizon: reorgHorizon, RecencyWindow: d - reorgHorizon}, nil
}

// Pruner evicts spent UTXO records once they are buried deeper than the policy depth.
type Pruner struct {
	policy  Policy
	st      store.Store
	lastTip uint32
	hasTip  bool
	total   uint64 // cumulative records pruned (atomic)
}

// New builds a Pruner. The policy is correct by construction (D >= ReorgHorizon).
func New(st store.Store, p Policy) *Pruner {
	return &Pruner{policy: p, st: st}
}

// Policy returns the configured policy (for advertising D / ReorgHorizon).
func (pr *Pruner) Policy() Policy { return pr.policy }

// TotalPruned returns the cumulative number of records evicted.
func (pr *Pruner) TotalPruned() uint64 { return atomic.LoadUint64(&pr.total) }

// OnNewBlock runs the sweep for a tip advance to height `tip`: it evicts EVERY spend
// band that has reached depth > D since the last call — all bands in
// (lastTip-D, tip-D]. It does NOT assume it is called once per consecutive height, so a
// tip jump (initial sync, catch-up after downtime, batch ingest) cannot leave skipped
// bands unpruned (which would leak the spendsPerBlock×D memory bound). Returns the
// number of records evicted by this call.
//
// Correctness: every evicted band sits at depth >= D+1 > ReorgHorizon, so a spend is
// never freed while a tolerated reorg could still roll it back (§11.2). It only ever
// under-/correctly-prunes, never inside the horizon.
func (pr *Pruner) OnNewBlock(tip uint32) (int, error) {
	prevTip, hadTip := pr.lastTip, pr.hasTip
	pr.lastTip, pr.hasTip = tip, true

	total := 0
	// UTXO spend-depth eviction at depth D.
	n, err := pr.sweep(prevTip, hadTip, tip, pr.policy.D(), pr.st.PruneSpentAtHeight)
	total += n
	if err != nil {
		atomic.AddUint64(&pr.total, uint64(total))
		return total, err
	}
	// Index/proof data retention at depth IndexRetention (0 = keep all).
	if pr.policy.IndexRetention > 0 {
		ni, err := pr.sweep(prevTip, hadTip, tip, pr.policy.IndexRetention, pr.st.PruneIndexAtHeight)
		total += ni
		if err != nil {
			atomic.AddUint64(&pr.total, uint64(total))
			return total, err
		}
	}
	atomic.AddUint64(&pr.total, uint64(total))
	return total, nil
}

// sweep evicts every band in (prevTip-d, tip-d] via fn — the gap-safe range that has
// reached depth > d since the last call (so tip jumps never leak). Returns records freed.
func (pr *Pruner) sweep(prevTip uint32, hadTip bool, tip, d uint32, fn func(uint32) (int, error)) (int, error) {
	if tip < d {
		return 0, nil // nothing has reached depth d+1 yet
	}
	target := tip - d
	var start uint32
	switch {
	case !hadTip:
		start = target // first call: baseline, no ancient backfill
	case prevTip >= d:
		start = (prevTip - d) + 1
	default:
		start = 0
	}
	if start > target {
		return 0, nil
	}
	total := 0
	for h := start; ; h++ {
		n, err := fn(h)
		if err != nil {
			return total, err
		}
		total += n
		if h == target {
			break
		}
	}
	return total, nil
}

// Depth returns the spend depth of a spend at spentHeight given the current tip
// (the block of the spend counts as depth 1): tip - spentHeight + 1.
func Depth(tip, spentHeight uint32) uint32 {
	if tip < spentHeight {
		return 0
	}
	return tip - spentHeight + 1
}

// RecommendedPolicy returns a conservative, safety-first policy: a reorg horizon well
// beyond any plausible BSV reorg plus a small recency window. Operators should only
// REDUCE the reorg horizon with deliberate justification (the floor is a correctness
// requirement, not a tuning knob — DESIGN.md §11.2). Defaults: ReorgHorizon=18,
// RecencyWindow=12 (D=30 blocks, ~5 hours of trailing spend history).
func RecommendedPolicy() Policy {
	return Policy{ReorgHorizon: 18, RecencyWindow: 12}
}

// EstimateRetainedSpends estimates the steady-state number of spent records retained
// (the bounded part of memory): spendsPerBlock * D. The live UTXO set is separate and
// not pruned. Use this to size a box for a given chain spend rate and depth.
func EstimateRetainedSpends(spendsPerBlock uint64, p Policy) uint64 {
	return spendsPerBlock * uint64(p.D())
}
