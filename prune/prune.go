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

// OnNewBlock runs the incremental sweep for a tip advance to height `tip`: it evicts
// the single band of spends that has just reached depth D+1 (spentHeight = tip - D).
// It is idempotent per height and skips heights not deep enough yet. Returns the
// number of records evicted by this call.
//
// Correctness: the evicted band sits at depth D+1 > ReorgHorizon, so a spend is
// never freed while a tolerated reorg could still roll it back (§11.2).
func (pr *Pruner) OnNewBlock(tip uint32) (int, error) {
	d := pr.policy.D()
	if uint32(tip) < d {
		pr.lastTip, pr.hasTip = tip, true
		return 0, nil // chain not yet D deep; nothing has crossed the cutoff
	}
	band := tip - d // this spentHeight has just reached depth d+1
	n, err := pr.st.PruneSpentAtHeight(band)
	if err != nil {
		return 0, err
	}
	atomic.AddUint64(&pr.total, uint64(n))
	pr.lastTip, pr.hasTip = tip, true
	return n, nil
}

// Depth returns the spend depth of a spend at spentHeight given the current tip
// (the block of the spend counts as depth 1): tip - spentHeight + 1.
func Depth(tip, spentHeight uint32) uint32 {
	if tip < spentHeight {
		return 0
	}
	return tip - spentHeight + 1
}
