//go:build cuda

// CUDA secp256k1 batch-verify backend (DESIGN.md §13.4). Behind the `cuda` build tag so
// the default build/tests/CI stay self-contained (no CUDA toolchain). Build it with the
// GPU kernel compiled and linked, e.g.:
//
//	cd accel/cuda && nvcc -O3 -shared -Xcompiler -fPIC -o libmemserve_gpu.so verify.cu
//	CGO_LDFLAGS="-L$(pwd)/accel/cuda -lmemserve_gpu -lcudart" go build -tags cuda ./...
//
// The low-S (malleability) policy is applied here on the host; only canonical, in-range
// candidates are dispatched to the GPU for the (expensive) EC verification. Trust this
// backend only after accel.Validate passes against the Go reference.
package accel

/*
#cgo LDFLAGS: -lmemserve_gpu -lcudart
#include "cuda/verify.h"
*/
import "C"

import "unsafe"

// CUDA is the GPU BatchVerifier.
type CUDA struct{}

func (CUDA) Name() string { return "cuda-secp256k1" }

func (CUDA) VerifyBatch(reqs []Request, out []bool) {
	n := len(reqs)
	if n == 0 {
		return
	}
	pub := make([]byte, n*33)
	hash := make([]byte, n*32)
	sig := make([]byte, n*64)
	send := make([]bool, n)
	for i := range reqs {
		r := reqs[i]
		// Host-side policy: reject nil / non-low-S / malformed before dispatch.
		if r.Sig == nil || r.Pub == nil || !r.Sig.IsLowS() || len(r.Hash) != 32 {
			out[i] = false
			continue
		}
		copy(pub[i*33:i*33+33], r.Pub.SerializeCompressed())
		copy(hash[i*32:i*32+32], r.Hash)
		copy(sig[i*64:i*64+64], r.Sig.Serialize())
		send[i] = true
	}
	res := make([]byte, n)
	rc := C.memserve_secp256k1_verify_batch(
		(*C.uchar)(unsafe.Pointer(&pub[0])),
		(*C.uchar)(unsafe.Pointer(&hash[0])),
		(*C.uchar)(unsafe.Pointer(&sig[0])),
		(*C.uchar)(unsafe.Pointer(&res[0])),
		C.int(n),
	)
	if rc != 0 {
		// On a CUDA error, fail closed: nothing is reported valid.
		for i := range out[:n] {
			out[i] = false
		}
		return
	}
	for i := 0; i < n; i++ {
		if send[i] {
			out[i] = res[i] == 1
		}
	}
}

var _ BatchVerifier = CUDA{}
