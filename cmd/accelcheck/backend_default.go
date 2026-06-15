//go:build !cuda

package main

import "memserve/accel"

// backend (default): the CPU parallel verifier.
func backend() accel.BatchVerifier { return accel.NewCPU() }
