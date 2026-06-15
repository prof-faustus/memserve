# MemServe — an in-memory, hash-sharded transaction-lookup fabric over Teranode

**Status: IMPLEMENTED (v0).** This file records the design; the system is now built and
tested (see the repository packages and `README.md`). The mock Teranode source makes the
whole pipeline runnable offline; the real Teranode adapter and a live Aerospike cluster
are the deployment steps. The numbered "Decisions (resolved)" sections below reflect the
choices that were confirmed and implemented.

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

### 10.6 On-chain transaction layer (`bsvtx`) and the open-ordering caveat

The channel signs **real BSV transactions**, not a bespoke message (the `bsvtx` package):

- **Funding** locks the deposit in a bare **2-of-2** (`OP_2 <client> <server> OP_2
  OP_CHECKMULTISIG`); payouts use **P2PK** (no RIPEMD-160 needed — valid BSV script).
- A **commitment** is the client's **DER-encoded, low-S ECDSA signature over the FORKID
  sighash** (BIP143-style preimage with the mandatory 0x40 flag) of the commitment tx that
  pays the server `cum` and returns the change to the client. `SignCommitment` /
  `Authorize` produce and verify exactly this.
- **Settlement** (`Channel.SettlementTx`) co-signs the best commitment with the server key
  and finalizes the 2-of-2 unlocking script (`OP_0 clientSig serverSig`) — a
  **broadcastable** BSV transaction. **Refund** (`RefundTxUnsigned`) carries
  `nLockTime = x` with a locktime-enabled input sequence.
- secp256k1, SHA-256d, RFC 6979, low-S, FORKID. No CLTV/CSV, no SegWit/witness, no BTC
  primitive anywhere.

**Open-ordering / malleability (the classic Spillman exposure).** The refund is signed
before the funding output is final; if the funding txid were third-party-malleable, the
pre-signed refund's outpoint reference would break. MemServe mitigates this with **strict
canonical encoding** (low-S enforced on both sign and verify; minimal DER) and a
**confirmed-funding default**: the server should treat a channel as usable only once the
funding tx is confirmed (its txid fixed). This relies on BSV's standardness/low-S policy;
final acceptance must be verified by **broadcasting on BSV testnet** (the VM enables this).

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

### 11.2 Correctness floor — _D_ ≥ the node's reorg horizon (MUST be named, not implicit)

Pruning at depth _D_ **embeds an assumption**: `D ≥ the maximum reorg depth the node will
tolerate`. A reorg **deeper than _D_** rolls back a spend whose record has **already been
discarded** — after that the node **answers wrong**: it reports an unspent status it can no
longer reconstruct. So _D_ is **not purely a memory/recency knob — it has a correctness
floor equal to the node's reorg horizon.** Left unstated, that is a hidden defect.

Therefore, as an explicit, advertised **policy field**:

- `D = ReorgHorizon + RecencyWindow`, where **`ReorgHorizon` is the correctness floor** —
  the deepest reorg the node commits to surviving — and `RecencyWindow ≥ 0` is the extra
  spent-query window on top.
- The node **commits to and advertises `D` (and its `ReorgHorizon`)**. It must never prune
  inside the reorg horizon. (BSV reorgs are typically shallow, but the floor is whatever
  depth the node commits to surviving, and it is named, not assumed.)
- Setting `D` below the realized reorg depth is the **only** way pruning can cause a wrong
  answer; naming the floor makes that a deliberate, refused configuration rather than a
  silent bug.

### 11.3 What this bounds (feasibility)

Steady-state memory ≈ **live (unspent) UTXO set** + **spends within the last _D_ blocks** +
**header chain** + **TxIndex within its retention**. Without pruning, spent history would
accumulate forever; with it, the spent-record footprint is capped at roughly _D_ blocks of
churn regardless of how long the node runs.

**This closes the UTXO-table feasibility item:** steady-state UTXO memory becomes
`[current unspent set] + [D-block trailing spend buffer]` — **bounded and feasible** —
instead of unbounded spend history. Note: **`TxIndex` and the proof/subtree store are
untouched by this** and still grow with chain history; they need the §9 (retention/tiering)
decision **unless we also prune them** on their own depth/retention policy (§11.6).

### 11.4 Mechanism — prune on block depth, NEVER wall-clock

- **Height-driven incremental sweep (the only authoritative mechanism).** On **each new
  sealed block** advancing the tip to `H`, delete the spent UTXOs whose **`spentHeight =
  H − D`** — the single band that has just reached depth `D + 1`. (Equivalent to evicting
  all `spentHeight ≤ H − D`; doing it per block touches only the newly-expired band, so it
  is O(band), not a full scan — the store is ordered/indexed by `spentHeight`.)
- **No time-based TTL for depth.** Block intervals vary, so a wall-clock Aerospike TTL is a
  **wrong proxy** for "D blocks deep" and is **not** used to decide pruning. (At most it
  could act as a coarse last-resort memory safety valve — never as the depth criterion.)
- **`D = 0` is the degenerate case:** a strict UTXO set — delete a UTXO the moment it is
  spent, with **no** reorg/recency buffer (only valid if `ReorgHorizon = 0`). **`D > 0`**
  keeps a `D`-block trailing reorg/recency window. The default and the floor are policy
  choices to fix per §11.2.
- **Reorg handling.** Within the retained `D`-block window a reorged-out spend is rolled
  back correctly (its record is still present, so the output reverts to unspent on the new
  chain) — which is exactly *why* `D` must cover the reorg horizon (§11.2).

### 11.5 Query semantics after pruning (honest, SPV-consistent)

- A query for a pruned (long-spent) outpoint returns **"not in retained window"**, *not*
  "unspent" and *not* a false "doesn't exist" — the node states it only serves spends to
  depth _D_. A client needing deeper history queries a node running a larger _D_ (or an
  archival node). This is a deliberate, advertised policy, consistent with the SPV model
  where the client ultimately verifies against the PoW chain.
- The node **advertises its _D_** so callers know its retention depth.

### 11.6 Decisions (resolved)

- **Pruning is in scope now; archival is a separate, later project.** Pruned (deep-spent)
  data is simply **freed**. Keeping/archiving pruned history is **out of scope here** — a
  separate **off-disk** project to be built **after this is finished** (noted for later, on
  close of this work).
- **Scope:** prune spent UTXO records by spend depth (the stated case); `TxIndex`
  Seen/Mined retention is **configurable** on its own policy (still needs §9 if not pruned).
- **_D_ is configurable** per server (and per shard if desired), expressed as
  `D = ReorgHorizon (correctness floor, named & advertised) + RecencyWindow`; the node never
  prunes inside `ReorgHorizon`. `D = ∞` (archival) is *not* a goal here — that's the
  separate archive project. `D = 0` is allowed only when `ReorgHorizon = 0`.
- **Mechanism:** **height-driven incremental sweep on each sealed block** (the only
  authoritative pruning trigger). **No wall-clock TTL as a depth proxy.**

> **NOTE FOR LATER (archive project):** after this system is finished and closed, build a
> separate **off-disk archive** that captures pruned (deep-spent) history before/at
> eviction, so MemServe stays lean in memory while a cold store retains full history.

### 11.7 Index/proof retention — bounding the store by design

Spend-depth pruning (above) only evicts *spent UTXOs*. The **TxIndex, subtree, block and
header** records would otherwise grow with chain history forever — an unbounded in-memory
footprint (this is what caused a memory runaway in a long mock ingest). **`IndexRetention`**
(blocks) bounds them: on each new block, all index/proof data for heights buried deeper
than `IndexRetention` is freed (`PruneIndexAtHeight`, gap-safe like the spend sweep, via a
per-height index in the store).

- **Steady state** then holds ~`IndexRetention` blocks of index data, regardless of how
  long the node runs (measured: 200 blocks ingested, txindex pinned at `IndexRetention`×
  txs/block).
- **Honest semantics:** a tx older than the window answers **"not in retained window"** /
  not-found for Seen/Mined/MerklePath — the deliberate serving-window choice (§11.5). A
  client needing deeper data queries a larger-window or archival node.
- **Default off** (`0` = keep all) because it changes answers for old txs; set it (via
  `memserved -index-retention`, recommended `>= D`) to bound the in-memory store, or use
  the disk-backed **Aerospike** backend for full retention. The `-max-mem-mb` watchdog +
  `debug.SetMemoryLimit` backstop remain as host-safety nets regardless.

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
├── accel/                   ← batch-verify backend (CPU + CUDA tag), batching gate, validator
│   └── cuda/                ← CUDA host launcher + ABI header for the GPU secp256k1 backend
└── bench/                   ← sharded lookup-throughput + paid-access benchmark
```

## 13. GPU / hardware acceleration

GPU helps **only** the two compute-bound parts — which are exactly the bottlenecks the
benchmarks expose — and is the **wrong tool** for the lookups themselves.

### 13.1 Where GPU pays off

1. **secp256k1 signature verification (the paid-access ceiling).** Per-access metering
   produces a stream of independent ECDSA verifications — embarrassingly parallel. The
   pure-Go `math/big` verifier does ~10³/core; batched GPU secp256k1 reaches ~10⁵–10⁶/s
   per card in the literature. This is the single biggest win, and the server only ever
   **verifies** (the client signs on its own device), so it is pure offload.
2. **SHA-256d Merkle path/tree construction (the pull cost).** `MerklePath` rebuilds
   subtree/block trees on read; SHA-256d batches superbly on GPUs. *Caveat:* caching
   precomputed paths turns this into an O(1) lookup and avoids the hashing entirely, so
   GPU here is the answer only if build-on-read is chosen over caching.

### 13.2 Where GPU does NOT help

The **Seen / Mined / UTXO** lookups (~10⁸/s) are random key→value probes — **memory-latency
bound**, not compute. PCIe transfer and limited VRAM make GPU a poor fit; the right lever
is the **shares-nothing sharding** already built (per-shard × 2ᵏ). GPU is not used here.

### 13.3 How it attaches (without polluting the pure-Go core)

- A **`BatchVerifier` interface** (`accel`): `VerifyBatch([]Request, []bool)`. The default
  is a **CPU parallel backend** (real, tested) that fans the batch across cores; a
  **CUDA backend** (behind the `cuda` build tag, CGo → `accel/cuda`) drops in unchanged.
- A **batching gate** collects per-access commitments up to a max batch size or a max
  delay, flushes them through the backend, and scatters results — trading a little latency
  for throughput (right for a throughput server).
- A **differential validator** (`accel.Validate`) checks *any* backend against the
  single-signature reference over random vectors. **This is the correctness gate**: a GPU
  kernel is only trusted once it passes the validator, so a wrong kernel can never silently
  serve. Verification is over public data, so the GPU path need not be constant-time.

### 13.4 Honest scope of the GPU build

The CPU batch backend, the gate, the validator and the benchmark are **built and tested**.
The **CUDA backend is the FFI boundary** — Go CGo binding + ABI header + host launcher —
into which a vetted GPU secp256k1 verify kernel is linked; it requires `nvcc` + a GPU and
is **gated by `accel.Validate`** before use (not claimed hardware-tested in this repo). The
secp256k1 device math is the one piece that must be written/validated on a CUDA box — the
boundary, the contract, and the correctness gate are all in place so dropping it in is safe.

## 14. Build status

**Built (v1):** sharding+routing; striped in-memory store + Aerospike adapter (tag);
**real BSV on-chain channel tx layer** (`bsvtx`: funding 2-of-2, commitment/settlement/refund,
FORKID sighash, DER+low-S — testnet broadcast is the remaining acceptance step, §10.6);
Teranode ingest run-server **with Merkle-consistency anti-poisoning validation** and
**reorg rollback**; real **Teranode HTTP adapter** (`teranode/httpsource`, tested vs a
simulated Teranode); query API with honest post-prune semantics; BSV pay-per-use payment
channel (prepay-then-serve, per-shard, configurable pricing, settle on n/time, built-in
settle fee, nLockTime refund) **+ abuse defenses** (deposit floor, channel cap, bad-attempt
ban, operator alert path); spend-depth pruning with the named reorg-horizon floor +
conservative defaults + sizing helper; **accountability** (`attest`: signed answers, miner
endorsement, fraud proofs); **multi-operator trust-minimizing client** (`client`); CPU
batch-verify accelerator + gate + validator (+ CUDA backend behind a tag); **commercial
HTTP/JSON server daemon** (`server`/`cmd/memserved`: health, metrics, rate limiting,
timeouts, graceful shutdown, signed responses, admin, miner revenue). Reuses MF-SPV
`commitment` + `crypto`. Tests run in CI under `-race` with no skips. See `SECURITY.md`.

**Deployment fronts (built to the boundary; only the live-infra step remains):**

- **Aerospike:** store is pluggable (`server.Config.Store`); the daemon selects it via
  `-store aerospike` under `-tags aerospike`. A shared **conformance suite**
  (`store/storetest`) runs against `mem` in CI and against a live cluster
  (`make aerospike-up && make aerospike-test`, `deploy/docker-compose.yml`). Remaining:
  run it against your production cluster.
- **Teranode:** `teranode/httpsource` is production-hardened — configurable endpoint
  templates, bearer auth, retries with exponential backoff, body caps — and tested vs a
  simulated Teranode incl. transient-retry and permanent-4xx paths. Remaining: set the two
  endpoint templates to your Teranode build's paths.
- **GPU:** `cmd/accelcheck` runs the correctness gate + throughput on the active backend;
  `make cuda` builds the kernel and `make cuda-check` validates it (`accel.Validate`) on a
  GPU box. Remaining: compile/validate the kernel on NVIDIA hardware.

The off-disk **archive** of pruned history is a separate, later project (§11.6).

## 15. Abuse / DoS defenses (economics: the attacker loses money)

Two griefing vectors and their defeat (`payment/abuse.go`, `server/limiter.go`):

- **Channel-open flood → state exhaustion.** Channel state is allocated only against a
  funded deposit ≥ `MinDeposit`, capped by `MaxChannels`. Opening N channels costs N
  on-chain funded deposits (capital locked + miner fees); the operator is alerted.
- **Invalid-commitment verify-flood (asymmetric secp256k1 cost).** Cheap O(1) checks
  (amount/deposit/low-S) run BEFORE the expensive verify; a per-channel `MaxBadAttempts`
  budget bans a flooding channel, bounding wasted work per funded deposit.
- **Query flood.** Per-client token-bucket rate limiting + payment gating + shares-nothing
  scale-out. MerklePath (CPU-bound) is priced higher.

Net: every channel costs the attacker real on-chain capital; valid queries are prepaid (the
operator profits); invalid floods are filtered, throttled, and cut. **The attack loses the
attacker money and cannot drain the server.** The prepay model also protects clients: a
client only pays what it signs (`client.SpendGuard` caps total spend).

## 16. Trust model — verify, cross-check, and hold liars to account

MemServe is an **untrusted cache**; correctness never depends on operator honesty:

- **Inclusion is trustless.** `client.MultiClient.MerklePath/Mined` verify the returned
  proof locally against the PoW header; one honest operator among many defeats all liars.
- **Negative/state answers are cross-checked.** The client fans Seen/UTXO to multiple
  independent operators and reports the answer with its agreement count (no single point of
  trust).
- **Lies are accountable.** Operators SIGN answers (`attest`); a signed false negative
  refuted by a verifying proof, or an equivocation, is a publishable `FraudProof`. A miner
  ENDORSES its operator's key, so the fraud proof names the miner too — if a miner lies via
  its MemServe, it is provably held to account.

## 17. Teranode integration & commercial server (miner value-add)

- **Real ingest.** `teranode/httpsource` is an HTTP client to a Teranode asset/data server
  implementing the same `teranode.Source` as the mock; the production and offline paths are
  identical. Tested against a simulated Teranode; ingest re-validates every block.
- **Commercial server.** `server` + `cmd/memserved`: zero-dep `net/http` JSON API for the
  four queries + payment + admin, with health/readiness, Prometheus-style `/metrics`,
  per-client rate limiting, request timeouts, structured logging, panic recovery, graceful
  shutdown, and optional signed attestations on every answer.
- **Miner value-add.** Run `memserved` as a sidecar to a miner's Teranode: it ingests the
  miner's chain and **monetizes serving via payment channels** (`/admin/stats`
  `revenueSatoshis`). The miner turns its block/UTXO data into a paid, accountable SPV
  proof-and-status service — added value with bounded cost and built-in revenue.

Build the skeleton above: the sharding+routing, the Aerospike store adapter, the
Teranode ingest server, the MemServe query API, the **BSV pay-per-use payment channel**
(open/fund + counter-signed `nLockTime` refund, per-access cumulative commitments,
release on **n accesses OR time x**), the **spend-depth pruning** (height-driven sweep to
a server-set depth _D_, freeing memory once a spend is buried past _D_), and a sharded +
paid-access benchmark — reusing the MF-SPV `commitment` (Merkle) and `crypto` (secp256k1,
low-S) and `fabric.Proof` so a served, paid proof drops straight into the verifier. Until
you confirm, **nothing is built** beyond this document.
