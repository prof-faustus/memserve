# MemServe

**An in-memory, hash-sharded transaction-lookup fabric over Teranode (BSV only).**

MemServe is the **proof-serving / status layer** that complements the
[MF-SPV verification fabric](https://github.com/prof-faustus/mfspv). Verification is
already solved and shown to scale linearly (~3.2×10⁸ inclusion-verifications/s per 64-core
box; shares-nothing to 10¹⁰–10¹¹ across nodes). The remaining bottleneck is the **pull
side** — *serving* the Merkle proof and answering *"is this tx mined?"* MemServe turns that
into a distributed **O(1) in-memory key lookup** backed by Aerospike, pulling its data from
Teranode.

> **Status: implemented (v1), hardening toward production.** Full pipeline + a
> commercial-grade HTTP/JSON server daemon, a real Teranode HTTP ingest adapter, pay-per-use
> payment channels (signing **real BSV transactions** — funding 2-of-2, commitment/settlement/
> refund, FORKID sighash, DER+low-S) with abuse defenses, spend-depth pruning with a named
> reorg-horizon correctness floor, a trust-minimizing multi-operator client, and signed-answer
> accountability (fraud proofs bound to the miner). Tested in CI under `-race`. See
> [`DESIGN.md`](./DESIGN.md) and [`SECURITY.md`](./SECURITY.md).
>
> **Remaining for production (live-infra only):** broadcast-validate the channel txs on **BSV
> testnet** (§10.6); point ingest at a **live Teranode** (thin endpoint mapping); run against an
> **Aerospike cluster** (`aerospike` tag); and validate the **GPU verify kernel** (`cuda` tag,
> gated by `accel.Validate`). The off-chain accounting, sighash, and tx construction are built
> and tested; only consensus acceptance on a real network/cluster/GPU remains.

## What it answers

For a `txid` (and optionally `outpoint = txid:vout`):

- **Seen?** — known to the network? → bool + first-seen time
- **Mined?** — in a block? → `{blockHash, height, blockTime}` (when)
- **Merkle path** — the inclusion proof (the same `fabric.Proof` the verifier consumes)
- **UTXO** — `{unspent, value}` for double-spend / value-conservation checks

## Key ideas

- **Hash-prefix sharding.** A txid is a uniform hash, so its **leading bits** partition the
  key space evenly: **k bits → 2ᵏ shards**. Uniform load, stateless prefix routing, elastic
  split (raise k by 1 to double capacity). Shares-nothing, like the verifier.
- **Ingest from Teranode.** A run server pulls subtrees, sealed blocks, and UTXO deltas and
  indexes them into Aerospike, sharded by prefix. It does **not** build or store full blocks
  — consensus stays in Teranode; MemServe is a lookup/index, not a node.
- **Pay-per-use (BSV payment channel).** Native unidirectional micropayment channel,
  **prepay-then-serve** (the server can never lose an access), **per-shard channels** (more
  private, no central hub), **one signature per access**, configurable pricing (flat or
  metered), settle on **n accesses and/or time x**, with the **settlement fee built in**
  (covers the on-chain mining fee). See `DESIGN.md §10`.
- **Spend-depth pruning.** Not an archive: once a spend is buried **D blocks deep** (a
  per-server policy, e.g. "run 10 deep") the record is **pruned and freed**. Live (unspent)
  outputs are never pruned. Bounds memory to the live set + recent spends. See `DESIGN.md §11`.

## Layout

```
cmd/memserved  the production daemon (HTTP/JSON server; -mock or -teranode <url>)
cmd/memserve   demo + benchmark (ingest -> serve -> verify -> pay -> accel -> shard scale)
cmd/ingest     standalone ingest demo: pulls a Teranode source, drives spend-depth pruning
server/        commercial HTTP/JSON service: health, metrics, rate limit, admin, signed answers
shard/         hash-prefix sharding + stateless routing (k bits -> 2^k shards)
store/         Store interface + records; store/mem (striped, default), store/aerospike (tag)
teranode/      ingest source + Merkle-consistent mock; teranode/httpsource (real HTTP adapter)
ingest/        indexes blocks (with anti-poisoning validation), reorg rollback, prune driver
proof/         Merkle-path assembly + verification (reuses the commitment fold)
api/           Seen / Mined / MerklePath / UTXO, with honest post-prune semantics
prune/         spend-depth retention with the named reorg-horizon correctness floor
payment/       pay-per-use BSV channel (prepay-then-serve) + abuse defenses + alert path
attest/        signed answers, miner endorsement, fraud proofs (accountability)
client/        trust-minimizing multi-operator client (verify proofs; quorum; fraud detection)
accel/         batch secp256k1 verify (CPU + cuda tag), batching gate, differential validator
commitment/    SHA-256d Merkle core (vendored from MF-SPV)
crypto/        secp256k1 ECDSA, RFC 6979, low-S, constant-time scalar mult (vendored from MF-SPV)
```

## Run the server (miner sidecar)

```sh
go run ./cmd/memserved -mock -addr :8080                      # demo against the built-in mock
go run ./cmd/memserved -teranode http://teranode:8090 \
   -operator-seed <hex32> -admin-token <tok> -min-deposit 100000 -rate 500
# probes: /healthz /readyz /metrics ; queries: /v1/seen|mined|merklepath|utxo ;
# payment: /v1/channel/open /v1/quote /v1/paid/query ; admin: /admin/stats (Bearer token)
```

### Deployment fronts (built to the boundary)

```sh
make aerospike-up && make aerospike-test   # local Aerospike + run the store conformance suite
go run -tags aerospike ./cmd/memserved -mock -store aerospike -aerospike-host 127.0.0.1
make accelcheck                            # validate the CPU verify backend vs the reference
make cuda && make cuda-check               # build + validate the GPU kernel (needs nvcc + GPU)
```

Remaining live-infra steps: point `-teranode` at a real node (two endpoint templates),
run the conformance suite against your Aerospike cluster, and validate the CUDA kernel on
a GPU. Everything else is built and tested.

## Build / test / run

```sh
make            # fmtcheck + vet + build + test
make race       # tests under the race detector
make demo       # cmd/memserve: end-to-end + throughput + shard extrapolation
make ingest     # cmd/ingest: spend-depth pruning bounding memory as the chain grows
make aerospike  # compile the Aerospike-backed store (pulls the client library)
```

Measured on a 64-core box (`go run ./cmd/memserve`): in-memory lookups scale across cores
(Seen/Mined ~1.8×10⁸ answers/s, UTXO ~1.4×10⁸; the store is internally striped by hash
prefix so it does not serialize on one lock). MerklePath is CPU-bound (the path is rebuilt
on read — the honest pull cost). The paid path is bound by secp256k1 verification (the true
cost of metered access, reported, not hidden). Aggregate scales shares-nothing as
per-shard × 2ᵏ shards.

## License

[Open BSV License version 4](./LICENSE). BSV / Teranode only — no BTC code, parameters, or
assumptions.
