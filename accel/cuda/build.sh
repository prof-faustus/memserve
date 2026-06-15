#!/usr/bin/env bash
# Build the MemServe GPU secp256k1 verify library, then validate it against the Go
# reference. Requires the CUDA toolkit (nvcc) and an NVIDIA GPU. See README.md.
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
repo="$(cd "$here/../.." && pwd)"

echo ">> compiling CUDA kernel -> libmemserve_gpu.so"
nvcc -O3 -shared -Xcompiler -fPIC -o "$here/libmemserve_gpu.so" "$here/verify.cu"

echo ">> ensuring the aerospike-free go.mod can resolve cgo build"
cd "$repo"

echo ">> validating the GPU backend against the Go reference (correctness gate)"
CGO_LDFLAGS="-L$here -lmemserve_gpu -lcudart" \
LD_LIBRARY_PATH="$here:${LD_LIBRARY_PATH:-}" \
  go run -tags cuda ./cmd/accelcheck -validate 4096

echo ">> OK: GPU backend matches the reference and is safe to serve."
