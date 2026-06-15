# MemServe

**An in-memory, hash-sharded transaction-lookup fabric over Teranode (BSV only).**

MemServe is the **proof-serving / status layer** that complements the
[MF-SPV verification fabric](https://github.com/prof-faustus/mfspv). Verification is
already solved and shown to scale linearly (~3.2×10⁸ inclusion-verifications/s per 64-core
box; shares-nothing to 10¹⁰–10¹¹ across nodes). The remaining bottleneck is the **pull
side** — *serving* the Merkle proof and answering *"is this tx mined?"* MemServe turns that
into a distributed **O(1) in-memory key lookup** backed by Aerospike, pulling its data from
Teranode.

> **Status: implemented (v0).** The full pipeline is built, tested, and benchmarkable
> offline against a deterministic mock Teranode source. See [`DESIGN.md`](./DESIGN.md)
> for the design rationale. The real Teranode ingest adapter and a live Aerospike
> cluster are the deployment steps; the Aerospike-backed store is implemented behind the
> `aerospike` build tag.

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
cmd/memserve   demo + benchmark (ingest -> serve -> verify -> pay -> shard extrapolation)
cmd/ingest     the run server: pulls a (mock) Teranode source, drives spend-depth pruning
shard/         hash-prefix sharding + stateless routing (k bits -> 2^k shards)
store/         Store interface + records; store/mem (striped, default), store/aerospike (tag)
teranode/      read-only ingest source + deterministic Merkle-consistent mock
ingest/        indexes blocks into a shard's store, owns only its prefix
proof/         Merkle-path assembly + verification (reuses the commitment fold)
api/           Seen / Mined / MerklePath / UTXO, with honest post-prune semantics
prune/         spend-depth retention with the named reorg-horizon correctness floor
payment/       pay-per-use BSV channel (prepay-then-serve) + payment-gated server
commitment/    SHA-256d Merkle core (vendored from MF-SPV)
crypto/        secp256k1 ECDSA, RFC 6979, low-S, constant-time scalar mult (vendored from MF-SPV)
```

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
