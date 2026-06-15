package accel

import "time"

// Throughput measures a backend's verifications/second over real elapsed time: it
// repeatedly verifies the batch until at least dur has passed, then divides the total
// verifications by the ACTUAL elapsed time (which may exceed dur if a single batch runs
// long). Dividing by real elapsed — not the nominal window — keeps slow backends honest
// (a batch that overruns the window is not counted as if it fit). Use a batch small
// enough that several iterations complete within dur for a stable figure.
func Throughput(v BatchVerifier, batch []Request, dur time.Duration) float64 {
	out := make([]bool, len(batch))
	start := time.Now()
	var n uint64
	for time.Since(start) < dur {
		v.VerifyBatch(batch, out)
		n += uint64(len(batch))
	}
	secs := time.Since(start).Seconds()
	if secs <= 0 {
		secs = 1
	}
	return float64(n) / secs
}
