package accel

import (
	"sync"
	"time"
)

// Gate is the batching layer (DESIGN.md §13.3): callers Submit individual verification
// requests; the gate accumulates them up to MaxBatch or MaxDelay, flushes the batch
// through the backend in one call, and returns each caller its own result. This trades
// a little latency for throughput — the regime a GPU (or many-core CPU) backend wants.
// Safe for concurrent Submit from many goroutines.
type Gate struct {
	v        BatchVerifier
	maxBatch int
	maxDelay time.Duration

	mu      sync.Mutex
	pending []item
	timer   *time.Timer
}

type item struct {
	req Request
	res chan bool
}

// NewGate builds a gate over a backend. maxBatch caps batch size; maxDelay caps how long
// the first queued request waits before a partial batch is flushed.
func NewGate(v BatchVerifier, maxBatch int, maxDelay time.Duration) *Gate {
	if maxBatch < 1 {
		maxBatch = 1
	}
	if maxDelay <= 0 {
		maxDelay = time.Millisecond
	}
	return &Gate{v: v, maxBatch: maxBatch, maxDelay: maxDelay}
}

// Submit queues a request and returns a channel that yields its verification result.
func (g *Gate) Submit(req Request) <-chan bool {
	res := make(chan bool, 1)
	g.mu.Lock()
	g.pending = append(g.pending, item{req: req, res: res})
	if len(g.pending) >= g.maxBatch {
		batch := g.takeLocked()
		g.mu.Unlock()
		g.flush(batch)
		return res
	}
	if g.timer == nil {
		g.timer = time.AfterFunc(g.maxDelay, g.onTimer)
	}
	g.mu.Unlock()
	return res
}

func (g *Gate) onTimer() {
	g.mu.Lock()
	batch := g.takeLocked()
	g.mu.Unlock()
	if len(batch) > 0 {
		g.flush(batch)
	}
}

// takeLocked removes all pending items and resets the timer (caller holds g.mu).
func (g *Gate) takeLocked() []item {
	batch := g.pending
	g.pending = nil
	if g.timer != nil {
		g.timer.Stop()
		g.timer = nil
	}
	return batch
}

func (g *Gate) flush(batch []item) {
	reqs := make([]Request, len(batch))
	for i := range batch {
		reqs[i] = batch[i].req
	}
	out := make([]bool, len(batch))
	g.v.VerifyBatch(reqs, out)
	for i := range batch {
		batch[i].res <- out[i]
	}
}
