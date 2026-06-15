#!/usr/bin/env bash
# Smoke test: build memserved, run it against the built-in mock source, probe the
# health/metrics endpoints, then shut it down cleanly. Exits non-zero on any failure.
#
# Why this script exists: the obvious one-liner
#     go run ./cmd/memserved -mock & SRV=$!; ...; kill "$SRV"
# captures the WRONG pid. `&` backgrounds the whole compound, and `go run` execs a
# *child* compiler-built binary -- so $! is the `go run` wrapper, not the server.
# `kill $SRV` then reaps the wrapper and ORPHANS the daemon, which keeps running
# (and, under -mock, can grow until it eats the host's RAM). We avoid both traps by
# building a real binary first and backgrounding that binary directly, so the pid we
# capture *is* the server, and a trap guarantees it dies even if the script aborts.
set -euo pipefail

ADDR="${ADDR:-127.0.0.1:18099}"
BASE="http://${ADDR}"
BIN="$(mktemp -t memserved.XXXXXX)"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

SRV=""
cleanup() {
  if [ -n "$SRV" ] && kill -0 "$SRV" 2>/dev/null; then
    kill "$SRV" 2>/dev/null || true          # SIGTERM: memserved shuts down gracefully
    for _ in $(seq 1 50); do                  # give it up to ~5s to exit
      kill -0 "$SRV" 2>/dev/null || break
      sleep 0.1
    done
    kill -9 "$SRV" 2>/dev/null || true        # last resort
    wait "$SRV" 2>/dev/null || true
  fi
  rm -f "$BIN"
}
trap cleanup EXIT INT TERM

echo ">> building memserved"
go build -o "$BIN" ./cmd/memserved

echo ">> starting memserved on ${ADDR} (mock source)"
"$BIN" -mock -addr "$ADDR" &
SRV=$!                                        # the binary's own pid -- the one we must kill

echo ">> waiting for readiness"
ready=0
for _ in $(seq 1 100); do                     # up to ~10s
  if ! kill -0 "$SRV" 2>/dev/null; then
    echo "!! memserved exited during startup" >&2
    exit 1
  fi
  if curl -fsS "${BASE}/readyz" >/dev/null 2>&1; then ready=1; break; fi
  sleep 0.1
done
[ "$ready" = 1 ] || { echo "!! memserved did not become ready" >&2; exit 1; }

echo "--- healthz ---";       curl -fsS "${BASE}/healthz"; echo
echo "--- readyz ---";        curl -fsS "${BASE}/readyz";  echo
echo "--- metrics (head) ---"; curl -fsS "${BASE}/metrics" | head -6

echo ">> smoke OK"            # cleanup() runs on EXIT and stops the server
