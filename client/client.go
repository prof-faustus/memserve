// Package client is the trust-minimizing MemServe client (DESIGN.md §16; addresses the
// "blind trust" and "false status" attack vectors). It treats every operator as
// UNTRUSTED:
//
//   - Inclusion is TRUSTLESS: MerklePath/Mined verify the returned proof against the
//     PoW header locally (commitment fold). A self-verifying proof from ANY one honest
//     operator is conclusive; a lying operator cannot forge it. One honest operator
//     among many defeats all liars.
//   - Negative / state answers (not-seen, unspent, not-in-window) are NOT cryptographically
//     self-proving, so the client fans the query to MULTIPLE independent operators and
//     reports the answer WITH its agreement count, surfacing disagreement instead of
//     trusting one server.
//   - When an operator SIGNS its answers (attest), a signed false negative refuted by a
//     proof becomes a publishable FraudProof (accountability).
//
// The prepay payment model already bounds client risk: the client only ever pays what IT
// signs, so a server cannot "drain" it; SpendGuard adds an explicit per-channel cap.
// BSV only.
package client

import (
	"errors"

	"memserve/api"
	"memserve/attest"
	"memserve/proof"
	"memserve/store"
)

// Operator is any MemServe endpoint answering the four queries. *api.Server satisfies it
// directly; a remote HTTP operator is wrapped to satisfy it too.
type Operator interface {
	Seen(store.Hash) (api.SeenResult, error)
	Mined(store.Hash) (api.MinedResult, error)
	MerklePath(store.Hash) (proof.Proof, error)
	UTXO(store.Outpoint) (api.UTXOResult, error)
}

// AttestingOperator additionally returns a signed attestation for an answer, enabling
// fraud proofs.
type AttestingOperator interface {
	Operator
	MinedAttested(store.Hash) (api.MinedResult, attest.Attestation, error)
}

// MultiClient queries a set of independent operators and combines trust-minimally.
type MultiClient struct {
	ops []Operator
}

// New builds a MultiClient over one or more operators.
func New(ops ...Operator) *MultiClient { return &MultiClient{ops: append([]Operator(nil), ops...)} }

// Agreement reports how many operators responded and how many agreed with the answer
// returned (for non-self-proving answers, this is the client's only trust signal).
type Agreement struct {
	Responders int
	Agree      int
}

// ErrNoProof is returned when no operator produced a self-verifying inclusion proof.
var ErrNoProof = errors.New("client: no operator returned a verifying inclusion proof")

// MerklePath returns the FIRST inclusion proof that verifies locally (folds to its block
// root) for txid. Trustless: a forged/!verifying proof from a liar is ignored.
func (m *MultiClient) MerklePath(txid store.Hash) (proof.Proof, error) {
	for _, op := range m.ops {
		p, err := op.MerklePath(txid)
		if err != nil {
			continue
		}
		if p.Leaf == txid && p.Verify() {
			return p, nil
		}
	}
	return proof.Proof{}, ErrNoProof
}

// Mined returns mined status. If ANY operator yields a verifying inclusion proof, the tx
// is conclusively mined (proof-backed, trustless) and proven=true. Otherwise it reports
// the quorum of operators' Mined() answers with proven=false and the agreement count.
func (m *MultiClient) Mined(txid store.Hash) (res api.MinedResult, proven bool, ag Agreement) {
	if p, err := m.MerklePath(txid); err == nil {
		return api.MinedResult{Mined: true, BlockHash: p.BlockHash, Height: p.Height}, true, Agreement{Responders: len(m.ops), Agree: len(m.ops)}
	}
	minedVotes := 0
	for _, op := range m.ops {
		r, err := op.Mined(txid)
		if err != nil {
			continue
		}
		ag.Responders++
		if r.Mined {
			minedVotes++
			res = r
		}
	}
	// majority of responders; absent a proof this is non-cryptographic.
	if minedVotes*2 > ag.Responders {
		ag.Agree = minedVotes
		res.Mined = true
		return res, false, ag
	}
	ag.Agree = ag.Responders - minedVotes
	return api.MinedResult{Mined: false}, false, ag
}

// Seen reports seen-by-any: an honest operator that has seen the tx suffices. (A liar
// cannot fabricate inclusion; falsely claiming "seen" is low-stakes and surfaced via the
// count.)
func (m *MultiClient) Seen(txid store.Hash) (api.SeenResult, Agreement) {
	var ag Agreement
	var out api.SeenResult
	for _, op := range m.ops {
		r, err := op.Seen(txid)
		if err != nil {
			continue
		}
		ag.Responders++
		if r.Seen {
			ag.Agree++
			out = r
		}
	}
	out.Seen = ag.Agree > 0
	return out, ag
}

// UTXO returns the plurality status across operators plus the agreement count, surfacing
// disagreement rather than trusting one server. (Authoritative spentness ultimately rests
// on the source / a fraud proof; MemServe is a cache.)
func (m *MultiClient) UTXO(op store.Outpoint) (api.UTXOResult, Agreement) {
	tally := map[api.UTXOStatus]int{}
	results := map[api.UTXOStatus]api.UTXOResult{}
	var ag Agreement
	for _, o := range m.ops {
		r, err := o.UTXO(op)
		if err != nil {
			continue
		}
		ag.Responders++
		tally[r.Status]++
		results[r.Status] = r
	}
	var best api.UTXOStatus
	bestN := -1
	for s, n := range tally {
		if n > bestN {
			bestN, best = n, s
		}
	}
	ag.Agree = bestN
	if bestN < 0 {
		return api.UTXOResult{Status: api.UTXOUnknown}, ag
	}
	return results[best], ag
}

// DetectFraud asks each attesting operator for a signed Mined answer; if an operator
// signs "not mined" but the multi-client can produce a verifying inclusion proof, it
// returns a FraudProof naming that operator. This turns a lying operator into evidence.
func (m *MultiClient) DetectFraud(txid store.Hash) (attest.FraudProof, bool) {
	p, err := m.MerklePath(txid)
	if err != nil {
		return attest.FraudProof{}, false // no proof => cannot prove a lie
	}
	for _, op := range m.ops {
		ao, ok := op.(AttestingOperator)
		if !ok {
			continue
		}
		_, att, err := ao.MinedAttested(txid)
		if err != nil {
			continue
		}
		if fp, err := attest.ProveFalseNegative(att, p); err == nil {
			return fp, true
		}
	}
	return attest.FraudProof{}, false
}

// SpendGuard is a client-side cap: it refuses to authorize a cumulative payment beyond
// Max on a channel, bounding how much any single (possibly malicious) operator can ever
// be paid — the client's defense against channel griefing / fund-draining.
type SpendGuard struct {
	Max uint64
}

// Allow reports whether a proposed cumulative payment is within the client's cap.
func (g SpendGuard) Allow(cumulative uint64) bool { return cumulative <= g.Max }
