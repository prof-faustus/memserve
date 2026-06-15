# CUDA secp256k1 batch-verify backend

GPU backend for `accel.BatchVerifier` (see `DESIGN.md §13`). It accelerates the one
genuine paid-access bottleneck: secp256k1 ECDSA **verification**. The server only ever
verifies (clients sign on their own devices), so this is pure offload.

## Status (honest)

`verify.cu` is a **faithful translation of the repository's proven Go reference**
(`crypto/ct.go` field/point arithmetic + `crypto/secp256k1.go` verify) into CUDA C. It
**requires `nvcc` + an NVIDIA GPU** and is **not hardware-validated in this repo**. It is
**gated by `accel.Validate`**, which checks it bit-for-bit against the Go reference over a
mix of valid / wrong-key / corrupted / high-S cases. **Do not trust it until Validate
passes on your hardware.** mod-p uses the fast secp256k1 reduction; mod-n uses a simple
binary mulmod + Fermat inverse chosen for obvious correctness over speed (optimize after
it validates). Verification is over public data, so no constant-time requirement applies.

## Build

```sh
cd accel/cuda
nvcc -O3 -shared -Xcompiler -fPIC -o libmemserve_gpu.so verify.cu
cd ../..
CGO_LDFLAGS="-L$(pwd)/accel/cuda -lmemserve_gpu -lcudart" \
  go build -tags cuda ./...
```

## Validate before use

```go
if err := accel.Validate(accel.CUDA{}, 1024); err != nil {
    log.Fatalf("CUDA backend failed the correctness gate: %v", err)
}
```

Only after this passes should `accel.CUDA{}` be placed behind the batching `accel.Gate`
in front of the paid path.
