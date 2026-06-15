#!/usr/bin/env bash
# Provision a fresh Ubuntu 22.04/24.04 VM to host the MemServe stack (Aerospike +
# memserved). Run as a user with sudo. Idempotent-ish. Hosts deployment fronts 1 (a
# Teranode endpoint to point at) and 2 (Aerospike) per the plan; the GPU box (front 3)
# is provisioned separately by deploy/gpu/setup.sh.
set -euo pipefail

REPO_URL="${REPO_URL:-https://github.com/prof-faustus/memserve.git}"
REPO_DIR="${REPO_DIR:-$HOME/memserve}"

echo ">> installing docker + compose plugin"
if ! command -v docker >/dev/null 2>&1; then
  curl -fsSL https://get.docker.com | sudo sh
  sudo usermod -aG docker "$USER" || true
fi

echo ">> cloning/updating repo"
if [ -d "$REPO_DIR/.git" ]; then
  git -C "$REPO_DIR" pull --ff-only
else
  git clone "$REPO_URL" "$REPO_DIR"
fi

echo ">> bringing up the stack (Aerospike + memserved, mock source by default)"
cd "$REPO_DIR/deploy/vm"
sudo docker compose up -d --build

echo ">> waiting for readiness"
for i in $(seq 1 60); do
  if curl -fsS localhost:8080/readyz >/dev/null 2>&1; then
    echo ">> READY. metrics:"; curl -fsS localhost:8080/metrics | head; exit 0
  fi
  sleep 2
done
echo "!! not ready after timeout; check: docker compose logs"
exit 1
