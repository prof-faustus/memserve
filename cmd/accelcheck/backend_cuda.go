//go:build cuda

package main

import "memserve/accel"

// backend (cuda): the GPU verifier. Requires the GPU library to be built and linked
// (see accel/cuda/README.md).
func backend() accel.BatchVerifier { return accel.CUDA{} }
