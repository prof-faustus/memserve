#!/usr/bin/env bash
# Provision a multi-GPU box (e.g. 3 NVIDIA cards) to build and VALIDATE the MemServe CUDA
# secp256k1 verify backend. Assumes the NVIDIA driver + CUDA toolkit (nvcc) are installed
# (or installs the toolkit on Ubuntu). The kernel splits each batch across all visible
# cards (see accel/cuda/verify.cu); accelcheck validates it against the Go reference and
# reports throughput — only a backend that PASSES the gate is safe to serve.
set -euo pipefail

REPO_DIR="${REPO_DIR:-$HOME/memserve}"

echo ">> GPUs visible:"; nvidia-smi --query-gpu=index,name,memory.total --format=csv || {
  echo "!! nvidia-smi not found — install the NVIDIA driver first"; exit 1; }

if ! command -v nvcc >/dev/null 2>&1; then
  echo ">> installing CUDA toolkit (Ubuntu)"
  sudo apt-get update && sudo apt-get install -y nvidia-cuda-toolkit
fi
if ! command -v go >/dev/null 2>&1; then
  echo "!! Go 1.26+ required (install from https://go.dev/dl/)"; exit 1
fi

cd "$REPO_DIR"
echo ">> building the CUDA library (uses all $(nvidia-smi -L | wc -l) cards at run time)"
nvcc -O3 -shared -Xcompiler -fPIC -o accel/cuda/libmemserve_gpu.so accel/cuda/verify.cu

echo ">> validating the GPU backend against the Go reference (correctness gate)"
CGO_LDFLAGS="-L$(pwd)/accel/cuda -lmemserve_gpu -lcudart" \
LD_LIBRARY_PATH="$(pwd)/accel/cuda:${LD_LIBRARY_PATH:-}" \
  go run -tags cuda ./cmd/accelcheck -validate 8192 -batch 16384

echo ">> if PASS above, the multi-GPU verify backend is correct and safe to serve."
