# MemServe

**An in-memory, hash-sharded transaction-lookup fabric over Teranode (BSV only).**

MemServe is the **proof-serving / status layer** that complements the
[MF-SPV verification fabric](https://github.com/prof-faustus/mfspv). Verification is
already solved and shown to scale linearly (~3.2×10⁸ inclusion-verifications/s per 64-core
box; shares-nothing to 10¹⁰–10¹¹ across nodes). The remaining bottleneck is the **pull
side** — *serving* the Merkle proof and answering *"is this tx mined?"* MemServe turns that
into a distributed **O(1) in-memory key lookup** backed by Aerospike, pulling its data from
Teranode.

> **Status: design / understanding phase.** See [`DESIGN.md`](./DESIGN.md) for the full
> write-up. No application code yet — building begins on confirmation.

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

## License

[Open BSV License version 4](./LICENSE). BSV / Teranode only — no BTC code, parameters, or
assumptions.
