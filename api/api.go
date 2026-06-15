// Package api is MemServe's query surface (DESIGN.md §4): Seen / Mined / MerklePath
// / UTXO, served from the store in memory. It encodes the honest post-pruning
// semantics (§11.5): a long-spent outpoint that has been pruned returns
// "not in retained window" — never a false "unspent" and never a false "absent".
// The node advertises its retention depth D. MemServe is an untrusted serving
// cache; the client still verifies served proofs against the PoW chain. BSV only.
package api

import (
	"memserve/proof"
	"memserve/store"
)

// Server answers lookups over a store, advertising its retention depth.
type Server struct {
	st store.Store
	// AdvertisedD is the spend-depth retention the node commits to (prune.Policy.D()).
	AdvertisedD uint32
}

// New builds a query server.
func New(st store.Store, advertisedD uint32) *Server {
	return &Server{st: st, AdvertisedD: advertisedD}
}

// SeenResult answers "has this tx been seen?".
type SeenResult struct {
	Seen     bool
	SeenTime int64 // unix nanos (valid iff Seen)
}

// Seen reports whether the tx is known (mempool or mined).
func (s *Server) Seen(txid store.Hash) (SeenResult, error) {
	ix, ok, err := s.st.GetTxIndex(txid)
	if err != nil {
		return SeenResult{}, err
	}
	if !ok {
		return SeenResult{}, nil
	}
	return SeenResult{Seen: true, SeenTime: ix.SeenTime}, nil
}

// MinedResult answers "has this tx been mined, and when/where?".
type MinedResult struct {
	Mined     bool
	BlockHash store.Hash
	Height    uint32
	BlockTime uint32
}

// Mined reports whether (and where) the tx was mined.
func (s *Server) Mined(txid store.Hash) (MinedResult, error) {
	ix, ok, err := s.st.GetTxIndex(txid)
	if err != nil {
		return MinedResult{}, err
	}
	if !ok || !ix.Mined {
		return MinedResult{}, nil
	}
	return MinedResult{Mined: true, BlockHash: ix.BlockHash, Height: ix.Height, BlockTime: ix.BlockTime}, nil
}

// MerklePath builds the inclusion proof for the tx (proof.ErrNotMined if not mined).
func (s *Server) MerklePath(txid store.Hash) (proof.Proof, error) {
	return proof.Build(s.st, txid)
}

// UTXOStatus is the tri-state answer to a UTXO query (§11.5).
type UTXOStatus uint8

const (
	// UTXOUnknown: the outpoint's tx is not even in this node's index.
	UTXOUnknown UTXOStatus = iota
	// UTXOUnspent: the output exists and is unspent.
	UTXOUnspent
	// UTXOSpent: the output exists and is spent (within the retained window).
	UTXOSpent
	// UTXONotInWindow: the output's tx is mined but the (spent) record has been
	// pruned past depth D — the node cannot reconstruct its status. Ask a node with
	// a larger D / an archival node. NOT "unspent" and NOT "absent".
	UTXONotInWindow
)

func (u UTXOStatus) String() string {
	switch u {
	case UTXOUnspent:
		return "unspent"
	case UTXOSpent:
		return "spent"
	case UTXONotInWindow:
		return "not-in-retained-window"
	default:
		return "unknown"
	}
}

// UTXOResult answers "is this outpoint unspent, and its value?".
type UTXOResult struct {
	Status      UTXOStatus
	Value       uint64     // valid iff Status == UTXOUnspent
	SpentBy     store.Hash // valid iff Status == UTXOSpent
	SpentHeight uint32     // valid iff Status == UTXOSpent
}

// UTXO returns the status of an outpoint, distinguishing pruned-spent (not in
// window) from never-seen (unknown) using the TxIndex.
func (s *Server) UTXO(op store.Outpoint) (UTXOResult, error) {
	u, ok, err := s.st.GetUTXO(op)
	if err != nil {
		return UTXOResult{}, err
	}
	if ok {
		if u.Spent {
			return UTXOResult{Status: UTXOSpent, SpentBy: u.SpentBy, SpentHeight: u.SpentHeight}, nil
		}
		return UTXOResult{Status: UTXOUnspent, Value: u.Value}, nil
	}
	// No UTXO record. Unspent outputs are never pruned, so absence means either the
	// tx was never seen, or it was spent and the record pruned past depth D.
	ix, known, err := s.st.GetTxIndex(op.TxID)
	if err != nil {
		return UTXOResult{}, err
	}
	if known && ix.Mined {
		return UTXOResult{Status: UTXONotInWindow}, nil
	}
	return UTXOResult{Status: UTXOUnknown}, nil
}
