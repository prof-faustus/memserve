// Package server is MemServe's commercial-grade HTTP/JSON service (DESIGN.md §17):
// the four lookups + payment + admin over net/http (zero external deps), with health
// and readiness probes, Prometheus-style metrics, per-client rate limiting, request
// timeouts, structured logging, panic recovery, graceful shutdown, and OPTIONAL signed
// attestations on every answer (accountability). It runs as a miner sidecar: it ingests
// from the miner's Teranode and monetizes serving via payment channels (revenue). BSV only.
package server

import (
	"encoding/hex"

	"memserve/attest"
	"memserve/commitment"
	"memserve/proof"
	"memserve/store"
)

// hexHash encodes a hash as hex.
func hexHash(h store.Hash) string { return hex.EncodeToString(h[:]) }

// parseHash decodes a 32-byte hex hash.
func parseHash(s string) (store.Hash, bool) {
	var h store.Hash
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 32 {
		return h, false
	}
	copy(h[:], b)
	return h, true
}

// PathElemJSON is one Merkle sibling on the wire.
type PathElemJSON struct {
	Sibling string `json:"sibling"`
	Right   bool   `json:"right"`
}

// ProofJSON is the wire form of proof.Proof — self-verifying by the client.
type ProofJSON struct {
	Leaf        string         `json:"leaf"`
	L1          []PathElemJSON `json:"l1"`
	SubtreeRoot string         `json:"subtreeRoot"`
	L2          []PathElemJSON `json:"l2"`
	BlockRoot   string         `json:"blockRoot"`
	BlockHash   string         `json:"blockHash"`
	Height      uint32         `json:"height"`
	Header      string         `json:"header"`
}

// EncodeProof converts a proof.Proof to its wire form.
func EncodeProof(p proof.Proof) ProofJSON {
	enc := func(es []commitment.PathElem) []PathElemJSON {
		out := make([]PathElemJSON, len(es))
		for i, e := range es {
			out[i] = PathElemJSON{Sibling: hexHash(e.Sibling), Right: e.Right}
		}
		return out
	}
	return ProofJSON{
		Leaf: hexHash(p.Leaf), L1: enc(p.L1), SubtreeRoot: hexHash(p.SubtreeRoot),
		L2: enc(p.L2), BlockRoot: hexHash(p.BlockRoot), BlockHash: hexHash(p.BlockHash),
		Height: p.Height, Header: hex.EncodeToString(p.Header[:]),
	}
}

// DecodeProof converts a wire proof back to proof.Proof (so a client can .Verify() it).
func DecodeProof(j ProofJSON) (proof.Proof, bool) {
	dec := func(es []PathElemJSON) ([]commitment.PathElem, bool) {
		out := make([]commitment.PathElem, len(es))
		for i, e := range es {
			sib, ok := parseHash(e.Sibling)
			if !ok {
				return nil, false
			}
			out[i] = commitment.PathElem{Sibling: sib, Right: e.Right}
		}
		return out, true
	}
	var p proof.Proof
	var ok bool
	if p.Leaf, ok = parseHash(j.Leaf); !ok {
		return p, false
	}
	if p.L1, ok = dec(j.L1); !ok {
		return p, false
	}
	if p.SubtreeRoot, ok = parseHash(j.SubtreeRoot); !ok {
		return p, false
	}
	if p.L2, ok = dec(j.L2); !ok {
		return p, false
	}
	if p.BlockRoot, ok = parseHash(j.BlockRoot); !ok {
		return p, false
	}
	if p.BlockHash, ok = parseHash(j.BlockHash); !ok {
		return p, false
	}
	hb, err := hex.DecodeString(j.Header)
	if err != nil || len(hb) != 80 {
		return p, false
	}
	copy(p.Header[:], hb)
	p.Height = j.Height
	return p, true
}

// AttestationJSON is the wire form of a signed answer (accountability).
type AttestationJSON struct {
	Kind      uint8  `json:"kind"`
	TxID      string `json:"txid"`
	Vout      uint32 `json:"vout"`
	Flag      bool   `json:"flag"`
	Height    uint32 `json:"height"`
	BlockHash string `json:"blockHash"`
	Tip       uint32 `json:"tip"`
	Operator  string `json:"operator"` // compressed pubkey hex
	Sig       string `json:"sig"`      // 64-byte hex
}

// EncodeAttestation converts an attestation to wire form.
func EncodeAttestation(a attest.Attestation) AttestationJSON {
	return AttestationJSON{
		Kind: uint8(a.Statement.Kind), TxID: hexHash(a.Statement.TxID), Vout: a.Statement.Vout,
		Flag: a.Statement.Flag, Height: a.Statement.Height, BlockHash: hexHash(a.Statement.BlockHash),
		Tip: a.Statement.Tip, Operator: hex.EncodeToString(a.Operator.SerializeCompressed()),
		Sig: hex.EncodeToString(a.Sig.Serialize()),
	}
}

// SeenResponse / MinedResponse / UTXOResponse / MerklePathResponse are the JSON answers.
type SeenResponse struct {
	Seen        bool             `json:"seen"`
	SeenTime    int64            `json:"seenTime"`
	Tip         uint32           `json:"tip"`
	Attestation *AttestationJSON `json:"attestation,omitempty"`
}
type MinedResponse struct {
	Mined       bool             `json:"mined"`
	BlockHash   string           `json:"blockHash,omitempty"`
	Height      uint32           `json:"height"`
	BlockTime   uint32           `json:"blockTime"`
	Tip         uint32           `json:"tip"`
	Attestation *AttestationJSON `json:"attestation,omitempty"`
}
type UTXOResponse struct {
	Status      string           `json:"status"`
	Value       uint64           `json:"value"`
	SpentHeight uint32           `json:"spentHeight,omitempty"`
	Tip         uint32           `json:"tip"`
	Attestation *AttestationJSON `json:"attestation,omitempty"`
}
type MerklePathResponse struct {
	Proof ProofJSON `json:"proof"`
}
