# MemServe — an in-memory, hash-sharded transaction-lookup fabric over Teranode

**Status: DESIGN / understanding write-up for confirmation. No code yet.** This file
records my understanding of the system you described so you can check it is correct
and hand it to Claude for full write-ups before any building begins.

---

## 1. One-sentence summary

MemServe is a horizontally-sharded, **in-memory lookup service** (backed by Aerospike)
that **pulls data from Teranode** and answers, in microseconds and at fabric scale,
the questions an SPV verifier actually asks about a transaction —
**"has this tx been seen?", "has it been mined, and when/where?", and "give me its
Merkle path"** — *without storing or re-deriving blocks*. It is the **proof-serving /
status layer** that complements the verification fabric (which is already built and
shown to scale linearly to ~3.2×10⁸ verifications/s/box).

## 2. Why it exists (relationship to what's already built)

- The MF-SPV **verification** fabric is solved: a 64-core box verifies inclusion at
  ~3.2×10⁸/s (hash-bound), and it scales shares-nothing (32 nodes → 10¹⁰, 512 →
  1.6×10¹¹). Verification is **not** the bottleneck.
- The remaining bottleneck is **serving the proof / answering "is this tx mined?"** —
  the *pull* side. A monolithic node building each Merkle path on demand caps ~10⁷/s.
- MemServe removes that bottleneck by turning proof serving into a **distributed
  in-memory key lookup**: the data needed to answer is precomputed/ingested from
  Teranode and stored, sharded by hash prefix, in Aerospike. Serving becomes an O(1)
  memory lookup instead of an on-demand tree walk.

So: **Teranode produces and orders the data → MemServe ingests and indexes it in
memory, sharded → SPV clients/verifiers query MemServe** for tx status + Merkle path.

## 3. What it does NOT do

- It does **not** build, validate, or store full blocks. It is a **lookup/index**, not
  a node. Consensus, ordering, and block assembly stay in Teranode.
- It does not replace the verifier. It *feeds* the verifier (serves the path/header the
  verifier then checks), and it answers status queries directly.

## 4. Core queries (the API surface)

For a given `txid` (and optionally `outpoint = txid:vout`):

1. **Seen?** — is the tx known to the network (in mempool / ingested)? → bool + first-seen time.
2. **Mined?** — is it in a block? → `{mined: bool, blockHash, height, blockTime}` (when it was mined).
3. **Merkle path** — the inclusion proof: `{leaf=txid, L1 (txid→subtree root), subtreeRoot,
   L2 (subtree root→block root), header}` — exactly the `fabric.Proof` the verifier consumes.
4. **UTXO** — is `outpoint` unspent, and its value? → `{unspent: bool, value}` (for the
   double-spend / value-conservation checks at the till).

These are the inputs the SPV push/pull flow and the point-of-sale acceptance already
need; MemServe serves them from memory at scale.

## 5. Data model (what is stored, "values in order")

Stored in Aerospike, keyed for O(1) lookup; UTXOs/records held **in order** by key so
ranges and prefixes are contiguous:

- **TxIndex**: `txid → { blockHash, height, blockTime, subtreeIndex, leafIndex, seenTime }`.
  Enough to answer Seen/Mined/When and to *locate* the tx for path reconstruction.
- **UTXO set**: `outpoint(txid,vout) → { value, scriptHash, spent: bool, spentBy, spentHeight }`,
  stored ordered by outpoint. `spentHeight` is what drives spend-depth pruning (§11).
- **Subtree store**: `subtreeRoot → { leaf list or layer data }` and **Block store**:
  `blockHash → { subtreeRoots, blockRoot, header[80] }`, enough to produce L1/L2 paths
  (either precomputed paths cached, or the layer data to build a path on lookup).
- **Header chain**: `height → header[80]` (the constant ~4.2 MB/yr dataset) so a served
  header can be checked as on the most-work chain.

(Exactly which of "precomputed path" vs "layer data + build on read" we store is a
memory/CPU trade-off to decide — see §9.)

## 6. Sharding — by hash prefix, in binary (the key idea)

Because a txid is a uniformly-distributed hash, the **leading bits** partition the key
space evenly:

- **k bits → 2ᵏ shards/servers**: 1 bit → 2 (`0…`, `1…`), 2 bits → 4 (`00`,`01`,`10`,`11`),
  3 bits → 8, … n bits → 2ⁿ. Each shard owns a contiguous binary prefix range.
- **Uniform load**: hash uniformity ⇒ each shard gets ≈ equal share of txs and queries.
  No hot spots, no rebalancing logic beyond the prefix.
- **Routing**: a client/router maps `txid` → shard by its top k bits → the owning
  MemServe server (and its Aerospike partition). Trivial, stateless routing.
- **Elastic split**: add capacity by increasing k by 1 — each shard splits cleanly into
  two (its `…0` and `…1` halves). Doubling servers halves each shard's range.
- This is the same shares-nothing property as the verifier: aggregate = shards × per-shard.

## 7. Ingest path (the run server that talks to Teranode)

A **MemServe ingest server** subscribes to / pulls from Teranode:

- new **subtrees** (~per second) → record each txid's `(subtreeIndex, leafIndex)` + seenTime;
- **sealed blocks** → record `blockHash, height, blockTime, blockRoot, subtreeRoots, header`,
  and flip the txs' index entries to "mined";
- **UTXO deltas** → update the ordered UTXO set (create on output, mark spent on input).

Each ingested record is written to the shard determined by its txid's prefix. Ingest is
itself shardable (each MemServe server ingests only the txs whose prefix it owns).

## 8. How it composes (end-to-end)

- **Pull SPV**: client asks MemServe (routed by prefix) → gets `{mined, when, Merkle path,
  header}` from memory → the **verifier fabric** checks the path/header. Serving is now a
  memory lookup, not a tree walk → removes the pull bottleneck.
- **Seen/double-spend**: MemServe answers Seen? and UTXO unspent? directly for the
  point-of-sale acceptance and the alert layer.
- **Scale**: serving = 2ᵏ shards × per-shard memory-lookup rate; verification = N nodes ×
  ~3.2×10⁸/s. Both linear, both hash-prefix / shares-nothing.

## 9. Open decisions (for you to confirm before building)

1. **Path storage**: cache full precomputed Merkle paths per tx (more memory, O(1) serve)
   vs. store subtree/block layer data and build the path on read (less memory, a few µs).
2. **Aerospike topology**: in-memory namespace vs. memory+SSD hybrid; replication factor;
   strong vs. eventual consistency for the UTXO "spent" flag.
3. **Shard count k**: fixed at deploy, or dynamic split as throughput grows.
4. **Teranode interface**: which Teranode services/streams we read (subtree-validator,
   block-persister, asset/utxo) — to be confirmed against the pinned Teranode source.
5. **Retention**: keep full history in memory, or memory for recent + SSD/cold for old.
   (Spend-depth pruning — §11 — is the primary memory-bounding mechanism.)
6. **Trust**: MemServe is a serving cache; the client still **verifies** the served path
   against the PoW header chain (MemServe is not trusted — consistent with SPV).

## 10. Pay-per-use — BSV payment channel (the billing extension)

MemServe is metered: **a client pays per access, and the server collects ("releases
funds") once it has sold _n_ accesses — or at time _x_ — whichever comes first.** On BSV
this is a native unidirectional micropayment channel; no L2, no token, just signed BSV
transactions.

### 10.1 The shape of the channel (Spillman-style, BSV-native)

1. **Open / fund.** The client locks a deposit into a channel output — a 2-of-2
   (client + server) funding tx. Before broadcasting it, the server **counter-signs a
   refund tx with `nLockTime = x`** that returns the full deposit to the client at time
   _x_. This is the client's protection: if the server vanishes, the deposit is
   refundable at _x_ and the deposit is never stuck.
2. **Prepay, then serve (the server can never lose even one access).** Payment **leads**
   service. To get access _k_, the client first sends a signed **commitment** paying the
   server the **cumulative** `fee(k)` (prepaid through access _k_), and **only after**
   verifying that signature and the increment does the server serve access _k_. The server
   is therefore always paid one step ahead and can never be out even a single access. A
   client who stops has already paid for the access it didn't take — **it can only cheat
   itself, never the server.** **One signature per access** (accesses can be very many);
   only signatures move off-chain until settlement.
3. **Release / settle.** The server broadcasts the **best (largest) commitment it holds**
   to collect. The trigger is **configurable, and both modes can apply**:
   - **count:** after the agreed **_n_ accesses** (n = 8, 1000, … per channel), and/or
   - **time:** at a settle-before `x' < x` (it must settle before the client's refund
     `nLockTime = x` matures).
   One on-chain tx settles all accesses in the channel.

So "release funds when it has sold n accesses, or at time x" is the **count-threshold
and/or timeout** settlement trigger (both configurable), with the refund timelock as the
client's safety net and prepay ensuring the server is never owed.

### 10.2 BSV mechanics (no BTC assumptions)

- **Incremental payments** use SIGHASH appropriately so each commitment is a valid,
  ever-larger spend of the same funding output (later commitments supersede earlier).
- **`nLockTime`** on the refund (and on the server's settle-before deadline) implements
  "at time _x_" — height- or timestamp-based, BSV consensus rules.
- **Large default tx / no artificial limits** on BSV means a channel can fund and settle
  big batches cheaply; micro-amounts per access are fine.
- **Settlement fee is built in.** Each settlement carries the on-chain **mining (tx) fee**
  plus the server's settlement cost, **amortized** into the channel — across e.g. 10,000
  checks it's a small fraction of each access. Because it's funded from the channel and
  paid at settlement, it also **discourages cheating** (abandoning a channel can't dodge
  it). So the per-access price the client prepays = `service_price + amortized_settle_fee`.
- The deposit sizing = `n × (price + amortized settle fee)`; top-up = open a new channel.

### 10.3 How it binds to the sharded service

- **Per-access authorization.** A query to a shard carries the latest commitment (or a
  channel handle + the incremental signature). The shard's billing layer checks it
  against that channel's running total before `api` serves Seen/Mined/MerklePath/UTXO.
- **Sharding interaction — per-shard channels (chosen).** A client holds **one channel per
  shard server it talks to**. This is shares-nothing (matches the prefix model), needs **no
  central billing hub to manage**, and is **more private**: many independent small channels
  instead of one identifying account. Aggregation happens **within** each channel — many
  off-chain per-access payments collapse into **one** on-chain settlement per channel.
- **Accounting** (count of accesses, current cumulative amount, best commitment) is small
  per-channel state, held in memory / Aerospike alongside the shard it bills.

### 10.4 Trust / safety properties

- Client can't be overcharged: it signs each increment; the server can only ever settle a
  commitment the client signed.
- Server can't be stiffed mid-stream: it serves an access only after the matching payment
  increment; worst case it loses one unpaid access if a client disconnects.
- Funds never stick: the `nLockTime = x` refund returns the deposit to the client if the
  server never settles.
- Consistent with SPV trust model: payment is enforced cryptographically, and the served
  proof is still independently verified by the client against the PoW header chain.

### 10.5 Decisions (resolved)

1. **Channel scope:** **per-shard channels** — many small channels, shares-nothing, no hub
   to manage, more private; aggregation is within-channel (many accesses → one settlement).
2. **Granularity:** **one signature per access** (accesses can be very many).
3. **Pricing:** **configurable price structures, built in** — a client can choose **flat**
   access or **metered per query type** (e.g. MerklePath > Seen). Both supported.
4. **`n` and `x`:** **both, configurable** — settle on count, on time, or either; set per
   channel/policy.
5. **Settlement fee:** **built in** — covers the on-chain mining fee + settlement cost,
   amortized across accesses; funded from the channel so it also deters cheating.
6. **Flow:** **prepay-then-serve** — payment always leads service; the server can never lose
   an access; a stopping client only forfeits its own prepayment.

## 11. Pruning — spend-depth retention (bounding memory, not "open for all time")

MemServe is **not** an archive. Once an output is **spent and that spend is buried _D_
blocks deep**, the record is **pruned and freed** — it no longer answers as a UTXO and is
no longer available at all. _D_ is a **per-server policy** ("run 10 deep"): the node
chooses how far back it still serves spent records, and everything older than that is
evicted to reclaim memory.

### 11.1 The rule

- Each spent UTXO carries `spentHeight` (the height of the block its spend landed in).
- Let `H` = current chain tip height. **Spend depth = `H − spentHeight + 1`** (the block
  it was spent in counts as depth 1).
- **While `spendDepth ≤ D`:** the record is retained and queryable. A UTXO query returns
  `unspent = false` with `{spentBy, spentHeight}`; a Seen/Mined query still answers.
- **When `spendDepth > D`:** the record is **pruned** — removed from memory. So with
  `D = 10`, a spend visible at blocks 1…10 deep disappears at the **11th** block deep.
- **Unspent outputs are never pruned by this rule** — the live UTXO set is the working set
  and must stay. Pruning only evicts *spent* history once it's deep enough to be settled.

This makes a spent outpoint visible for exactly the node's chosen depth window, then gone —
"spent → pruned at a set (by server) depth", not open for all time.

### 11.2 What this bounds

Steady-state memory ≈ **live (unspent) UTXO set** + **spends within the last _D_ blocks** +
**header chain** + **TxIndex within its retention**. Without pruning, spent history would
accumulate forever; with it, the spent-record footprint is capped at roughly _D_ blocks of
churn regardless of how long the node runs.

### 11.3 How it runs (driven by new blocks, exact by height)

- Pruning is **height-driven**, triggered each time ingest advances the tip `H`: a sweep
  evicts every spent record with `H − spentHeight + 1 > D`. Because the UTXO set is stored
  **ordered**, and we can index spends by `spentHeight`, the sweep is a contiguous range
  scan of "spends that just crossed the depth cutoff" — cheap and incremental (only the
  newly-expired band each block, not a full scan).
- **Aerospike TTL** can serve as a convenient backstop: on marking an output spent, set a
  record expiration ≈ `D × ~10 min`. TTL is *time*-based (approximate), so the
  height-driven sweep is the **authoritative**, exact-by-depth mechanism; TTL is optional
  belt-and-braces to guarantee eventual reclaim.
- **Reorgs:** if a block is reorged out, `spentHeight` is re-evaluated on the new chain
  before eviction — pruning acts only on spends confirmed deeper than _D_, which (with _D_
  beyond plausible reorg depth) are effectively final. Choosing _D_ ≥ the node's reorg
  assumption keeps pruning safe.

### 11.4 Query semantics after pruning (honest, SPV-consistent)

- A query for a pruned (long-spent) outpoint returns **"not in retained window"**, *not*
  "unspent" and *not* a false "doesn't exist" — the node states it only serves spends to
  depth _D_. A client needing deeper history queries a node running a larger _D_ (or an
  archival node). This is a deliberate, advertised policy, consistent with the SPV model
  where the client ultimately verifies against the PoW chain.
- The node **advertises its _D_** so callers know its retention depth.

### 11.5 Decisions (resolved)

- **Pruning is in scope now; archival is a separate, later project.** Pruned (deep-spent)
  data is simply **freed**. Keeping/archiving pruned history is **out of scope here** — a
  separate **off-disk** project to be built **after this is finished** (noted for later, on
  close of this work).
- **Scope:** prune spent UTXO records by spend depth (the stated case); `TxIndex`
  Seen/Mined retention is **configurable** on its own policy.
- **_D_ is configurable** per server (and per shard if desired), with a reorg-safe minimum;
  `D = ∞` (archival) is *not* a goal here — that's the separate archive project.
- **Mechanism:** height-driven sweep (authoritative) + optional Aerospike TTL backstop.

> **NOTE FOR LATER (archive project):** after this system is finished and closed, build a
> separate **off-disk archive** that captures pruned (deep-spent) history before/at
> eviction, so MemServe stays lean in memory while a cold store retains full history.

## 12. Proposed project structure (skeleton only — to build after you confirm)

```
192 MemServe/
├── DESIGN.md                ← this file
├── cmd/
│   ├── memserve/            ← the shard server (serves lookups for one prefix range)
│   └── ingest/              ← the run server that pulls from Teranode and writes Aerospike
├── shard/                   ← hash-prefix sharding + routing (k bits → 2^k shards)
├── store/                   ← Aerospike-backed stores (TxIndex, UTXO, Subtree, Block, Headers)
│   └── aerospike/           ← Aerospike client adapter (sets, bins, ordered keys)
├── teranode/                ← read-only ingest adapter to Teranode (subtrees, blocks, UTXO deltas)
├── prune/                   ← spend-depth retention: height-driven sweep (+ TTL backstop), policy D
├── proof/                   ← Merkle-path assembly/serving (reuses the MF-SPV commitment fold)
├── payment/                 ← BSV payment channel: open/fund, per-access commitments,
│   │                          settle-on-n-or-time, refund timelock (reuses MF-SPV crypto)
│   └── channel/             ← channel state, commitment verify, settlement triggers
├── api/                     ← query API: Seen / Mined / MerklePath / UTXO (payment-gated)
└── bench/                   ← sharded lookup-throughput + paid-access benchmark
```

## 13. What I will do once you confirm understanding

Build the skeleton above: the sharding+routing, the Aerospike store adapter, the
Teranode ingest server, the MemServe query API, the **BSV pay-per-use payment channel**
(open/fund + counter-signed `nLockTime` refund, per-access cumulative commitments,
release on **n accesses OR time x**), the **spend-depth pruning** (height-driven sweep to
a server-set depth _D_, freeing memory once a spend is buried past _D_), and a sharded +
paid-access benchmark — reusing the MF-SPV `commitment` (Merkle) and `crypto` (secp256k1,
low-S) and `fabric.Proof` so a served, paid proof drops straight into the verifier. Until
you confirm, **nothing is built** beyond this document.
