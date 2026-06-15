// Command accelcheck validates the active signature-verify backend against the Go
// reference (the correctness gate) and measures its throughput. The default build checks
// the CPU backend; built with -tags cuda (and the GPU library linked) it checks the CUDA
// backend — this is how a GPU kernel is validated before it is trusted to serve.
//
//	go run ./cmd/accelcheck                         # CPU backend
//	CGO_LDFLAGS=... go run -tags cuda ./cmd/accelcheck   # CUDA backend (needs GPU + lib)
//
// Exit code is non-zero if the backend disagrees with the reference. BSV only.
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"memserve/accel"
)

func main() {
	n := flag.Int("validate", 2048, "validation vectors")
	batch := flag.Int("batch", 2048, "throughput batch size")
	dur := flag.Duration("dur", time.Second, "throughput measurement window")
	flag.Parse()

	bv := backend()
	fmt.Printf("# accelcheck — backend=%s\n", bv.Name())

	if err := accel.Validate(bv, *n); err != nil {
		fmt.Printf("FAIL: backend disagrees with the Go reference: %v\n", err)
		fmt.Printf("=> this backend is NOT safe to serve. Fix the kernel and re-run.\n")
		os.Exit(1)
	}
	fmt.Printf("PASS: %d/%d vectors match the reference (valid/wrong-key/corrupt/high-S).\n", *n, *n)

	b := accel.MakeBatch(*batch)
	ref := accel.Throughput(accel.Reference{}, accel.MakeBatch(64), *dur)
	got := accel.Throughput(bv, b, *dur)
	fmt.Printf("reference (serial, 1 core): %.3e verify/s\n", ref)
	fmt.Printf("%-26s %.3e verify/s  (%.1fx)\n", bv.Name()+":", got, got/ref)
}
