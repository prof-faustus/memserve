# MemServe — project status

Snapshot of where the project stands. Repo: https://github.com/prof-faustus/memserve
(commit `e070f2b`, CI green). BSV/Teranode only.

## What it is

An in-memory, hash-prefix-sharded transaction-lookup fabric over Teranode: answers
**Seen / Mined / Merkle-path / UTXO** from memory at scale, with pay-per-use BSV payment
channels, spend-depth pruning, signed-answer accountability, and a trust-minimizing
multi-operator client. Runs as a miner sidecar (ingests the miner's Teranode, monetizes
serving).

## Built and tested (in CI, `go test -race`, no skips)

| Area | Package | State |
|---|---|---|
| Sharding + routing | `shard` | done |
| Store (striped) + conformance suite | `store/mem`, `store/storetest` | done |
| Aerospike backend | `store/aerospike` (`aerospike` tag) | compiles + conformance-ready |
| Teranode ingest (mock + real HTTP) | `teranode`, `teranode/httpsource` | done; tested vs simulated Teranode |
| Ingest: anti-poisoning validation + reorg rollback | `ingest` | done |
| Merkle-path assembly + verify | `proof` | done |
| Query API + honest post-prune semantics | `api` | done |
| Spend-depth pruning + named reorg floor + gap sweep | `prune` | done (Defect 1 fixed) |
| Real BSV tx layer (FORKID sighash, 2-of-2, DER+low-S) | `bsvtx` | done (Defect 2/3) |
| Payment channel (prepay-then-serve) + abuse defenses | `payment`, `payment/channel` | done; signs real commitment tx |
| Accountability (signed answers, fraud proofs, miner endorsement) | `attest` | done |
| Multi-operator trust-minimizing client | `client` | done |
| Batch verify accel (CPU + CUDA tag, validator) | `accel`, `accel/cuda` | CPU done; CUDA gated by `accel.Validate` |
| Commercial HTTP/JSON server daemon | `server`, `cmd/memserved` | done (health/metrics/rate-limit/admin/signed answers) |
| Deploy infra | `Dockerfile`, `deploy/` | VM compose + multi-GPU scripts |

Docs: `DESIGN.md`, `SECURITY.md` (every audited attack vector → mitigation), `README.md`.

## Audit defects — resolved

1. **Pruning leak on non-consecutive tip** — `OnNewBlock` sweeps all bands `(lastTip-D, tip-D]`; regression-tested.
2. **No real BSV tx layer** — `bsvtx` builds/signs real funding/commitment/refund/settlement txs; channel signs the real FORKID sighash; `SettlementTx` is broadcastable.
3. **Spillman/open-ordering** — strict low-S + minimal-DER canonical encoding + confirmed-funding default; documented.

## Remaining — live-infra only (each a scripted command on the target)

- **BSV testnet**: broadcast channel txs to confirm consensus acceptance (DESIGN §10.6).
  Tool built — `cmd/channeltestnet` (funding/settlement/refund, builds + signs + broadcasts);
  runbook in `deploy/testnet/README.md`. Run on a networked box with faucet coins.
- **Teranode**: set the two endpoint templates in `httpsource` to your node's paths; point `memserved -teranode`.
- **Aerospike**: `make aerospike-up && make aerospike-test` against your cluster.
- **GPU (3 cards)**: `bash deploy/gpu/setup.sh` — builds the multi-GPU kernel and validates it via `cmd/accelcheck`.

## Run

```sh
make                       # fmt + vet + build + test
go run ./cmd/memserved -mock -addr :8080        # demo daemon
bash deploy/vm/setup.sh    # full VM stack (Aerospike + memserved)
```
