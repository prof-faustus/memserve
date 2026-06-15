package accel

import (
	"runtime"
	"sync"
)

// CPU is the default BatchVerifier: it fans a batch across `Workers` goroutines, each
// running the trusted per-signature verifier. This already lifts the paid-access
// ceiling from one core to N cores; the CUDA backend (tag `cuda`) replaces it for
// per-card throughput.
type CPU struct {
	Workers int // <= 0 => runtime.NumCPU()
}

// NewCPU builds a CPU backend using all cores.
func NewCPU() CPU { return CPU{Workers: runtime.NumCPU()} }

func (c CPU) Name() string { return "cpu-parallel" }

func (c CPU) VerifyBatch(reqs []Request, out []bool) {
	w := c.Workers
	if w <= 0 {
		w = runtime.NumCPU()
	}
	n := len(reqs)
	if w > n {
		w = n
	}
	if w <= 1 {
		for i := range reqs {
			out[i] = reference(reqs[i])
		}
		return
	}
	var wg sync.WaitGroup
	chunk := (n + w - 1) / w
	for start := 0; start < n; start += chunk {
		end := start + chunk
		if end > n {
			end = n
		}
		wg.Add(1)
		go func(lo, hi int) {
			defer wg.Done()
			for i := lo; i < hi; i++ {
				out[i] = reference(reqs[i])
			}
		}(start, end)
	}
	wg.Wait()
}

var _ BatchVerifier = CPU{}
