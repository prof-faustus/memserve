# MemServe — Security model & threat analysis

MemServe is an **untrusted serving cache** over Teranode. Its security rests on one
principle: **don't trust — verify.** Inclusion answers are self-verifying; everything
else is made accountable (signed) and/or cross-checked across independent operators.
BSV/Teranode only.

## Trust model (what you must and must not trust)

- **You must NOT trust a MemServe operator for correctness.** Inclusion proofs are
  verified locally against the PoW header chain (`proof.Verify`, MF-SPV fold). A forged
  proof cannot pass.
- **You must NOT trust a single operator for availability or for negative answers.** Use
  the multi-operator client (`client.MultiClient`): one honest operator's verifying proof
  defeats all liars; negative/state answers are quorum-checked across operators.
- **You CAN hold a lying operator (and its miner) to account.** Operators sign answers
  (`attest`); a signed false negative refuted by a verifying proof, or an equivocation, is
  a publishable `attest.FraudProof` naming the operator and (via endorsement) the miner.
- **Consensus, ordering and block validity stay in Teranode.** MemServe indexes; it does
  not validate consensus. Ingested blocks are re-checked for Merkle self-consistency
  (anti-poisoning) before indexing.

## Security properties (informal)

1. **Inclusion soundness.** If `client.MerklePath`/`Mined` returns mined=true(proven), the
   tx is in a block committed by a PoW header — regardless of operator honesty. (Folds to
   the block root; the client checks the header is on the most-work chain.)
2. **No silent false negative.** An operator that signs "not seen/not mined" for a tx that
   is in fact included yields an airtight fraud proof (`ProveFalseNegative`).
3. **No equivocation.** Two contradictory signed answers at the same tip yield a fraud
   proof (`ProveEquivocation`).
4. **Payment safety.** Prepay-then-serve: the server never serves an unpaid access; a
   client only pays what it signs, so it cannot be over-charged or drained (and
   `client.SpendGuard` caps total spend per channel).
5. **Pruning correctness floor.** A spend is never evicted within the node's reorg horizon
   (`D = ReorgHorizon + RecencyWindow`, `prune.PolicyWithD` refuses `D < ReorgHorizon`), so
   a tolerated reorg can always be rolled back (`ingest.RollbackBlock`).

## Attack vectors → mitigations (covers the v0 audit)

### Trust & verification
| Vector | Mitigation |
|---|---|
| Blind trust in answers | Client verifies every proof locally (`client.MultiClient`, `proof.Verify`); docs make verification the default path. |
| False "mined"/"seen" | Self-verifying inclusion proof; a lie is unprovable to pass and, if signed, becomes a fraud proof. |
| Incorrect/forged Merkle path | `proof.Verify` rejects it; multi-client ignores non-verifying proofs. |
| UTXO status lying | Quorum across operators surfaces disagreement; signed answers make equivocation accountable. |
| Operator collusion / consistent lying | Endorsed fraud proofs implicate operator **and** miner (reputation/bond/publication); one honest operator defeats inclusion lies. |

### Pruning & state consistency
| Vector | Mitigation |
|---|---|
| Aggressive pruning + reorg | Named reorg-horizon floor; pruner never evicts inside it; `RollbackBlock` reverts in-window reorgs. |
| Pruning depth too low | `PolicyWithD` refuses `D < ReorgHorizon`; `RecommendedPolicy` is conservative (D=30); admin advertises D. |
| Live UTXO poisoning (prune a live output) | Pruning only ever targets *spent* records indexed by `spentHeight`; unspent outputs are never in the prune index. Unit-tested. |
| Reorg-horizon bypass | The floor is a published policy; choose `ReorgHorizon` ≥ any tolerated reorg depth. |

### Query & data integrity
| Vector | Mitigation |
|---|---|
| Ingest poisoning (malformed Teranode data) | `ingest.validateBlock` recomputes subtree/block roots and checks the header before storing; mismatches rejected, nothing stored. |
| Timing side-channels | Lookups are O(1) memory reads; verification is over public data. (No secret-dependent timing on the serving path.) |

### Payment channels
| Vector | Mitigation |
|---|---|
| Channel griefing / fund draining | Prepay model: client pays only what it signs; `SpendGuard` caps spend; expensive queries are priced (`Pricing.PerType`). |
| Channel-open flood | `Policy.MinDeposit` (funded deposit required) + `Policy.MaxChannels` cap; alerts to the operator. |
| Invalid-commitment verify-flood | Cheap structural checks before the secp256k1 verify; per-channel `MaxBadAttempts` ban; bounded wasted work per funded channel. |
| Replay / non-monotonic | Commitments must strictly increase cumulative; replays are rejected as underpaid. |
| Settlement double-spend | Standard BSV channel: counter-signed `nLockTime` refund; server settles before `x' < x`. |

### DoS & resource exhaustion
| Vector | Mitigation |
|---|---|
| Merkle-path CPU exhaustion | MerklePath is priced higher (metered); rate limiting; optional precomputed-path caching (design §9). |
| High-volume query flood | Per-client token-bucket rate limiting; payment gating; shares-nothing horizontal scale-out. |
| GPU/accel for verify cost | `accel` batch verify (CPU now, CUDA backend gated by `accel.Validate`) raises the verify ceiling. |

### Sharding & ops
| Vector | Mitigation |
|---|---|
| Shard targeting / imbalance | Hash-prefix routing is uniform by hash uniformity; load spreads across shards. |
| Aerospike misconfig / data loss | Aerospike adapter mirrors the tested in-memory reference; the cache is rebuildable from Teranode (no source of truth lost). |
| Teranode dependency failure | MemServe is a cache; clients fan out to multiple operators; on source loss the cache serves stale-but-verifiable data and readiness reflects tip lag. |
| Logging/metadata leaks | Per-shard channels limit correlation; operators should avoid logging query payloads (advisory). |

### Supply chain
| Vector | Mitigation |
|---|---|
| Vendored MF-SPV crypto compromise | `crypto`/`commitment` are the audited MF-SPV sources; verification is reproducible and the fold is independently checkable; `accel.Validate` differential-tests any alternate verifier. |

## Reporting

This is research-grade software hardening toward production. Report security issues
privately to the operator before public disclosure.
