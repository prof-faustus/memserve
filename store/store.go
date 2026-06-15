// Package store defines the records MemServe holds and the Store interface every
// backend implements. The records are exactly what is needed to answer the four
// lookup queries (Seen / Mined / Merkle-path / UTXO) from memory, plus the
// spend-height index that drives spend-depth pruning (DESIGN.md §5, §11).
//
// Two backends implement Store: store/mem (in-memory, default, fully tested and
// used by the benchmarks) and store/aerospike (the Aerospike adapter, behind the
// `aerospike` build tag). BSV/Teranode only.
package store

import "memserve/commitment"

// Hash is the canonical 32-byte SHA-256d digest, shared with the commitment layer.
type Hash = commitment.Hash

// Outpoint identifies a transaction output: txid:vout.
type Outpoint struct {
	TxID Hash
	Vout uint32
}

// TxIndex answers Seen / Mined / when, and locates a tx for path reconstruction.
type TxIndex struct {
	Mined        bool // false => seen in mempool only; true => in a block
	BlockHash    Hash
	Height       uint32
	BlockTime    uint32 // unix seconds (block header time)
	SubtreeIndex uint32 // which subtree of the block holds this tx
	LeafIndex    uint32 // position of the txid within that subtree
	SeenTime     int64  // unix nanos first observed
}

// UTXO is one unspent (or recently-spent) output. SpentHeight is what drives
// spend-depth pruning (§11): once a spend is buried past depth D it is evicted.
type UTXO struct {
	Value       uint64
	ScriptHash  Hash
	Spent       bool
	SpentBy     Hash   // txid that spent it (valid iff Spent)
	SpentHeight uint32 // height of the block the spend landed in (valid iff Spent)
}

// BlockRec holds enough of a sealed block to produce L2 paths and serve headers.
type BlockRec struct {
	Hash         Hash
	Height       uint32
	Time         uint32
	MerkleRoot   Hash
	SubtreeRoots []Hash   // the block's Merkle leaves (one per subtree)
	Header       [80]byte // the 80-byte PoW header
}

// Stats is a snapshot of a store's contents (for bench / ops visibility).
type Stats struct {
	TxIndex   int
	UTXOLive  int // unspent
	UTXOSpent int // spent-but-retained (within the depth window)
	Subtrees  int
	Blocks    int
	Headers   int
}

// Store is the backend contract. All methods are safe for concurrent use.
type Store interface {
	// TxIndex.
	PutTxIndex(txid Hash, ix TxIndex) error
	GetTxIndex(txid Hash) (TxIndex, bool, error)

	// UTXO set.
	PutUTXO(op Outpoint, u UTXO) error
	GetUTXO(op Outpoint) (UTXO, bool, error)
	// SpendUTXO marks an output spent at spentHeight; returns false if absent.
	SpendUTXO(op Outpoint, spentBy Hash, spentHeight uint32) (bool, error)
	// UnspendUTXO reverts a spend (reorg rollback); returns false if absent.
	UnspendUTXO(op Outpoint) (bool, error)

	// Subtree leaves (the txids of a subtree), keyed by subtree root.
	PutSubtree(root Hash, leaves []Hash) error
	GetSubtree(root Hash) ([]Hash, bool, error)

	// Blocks and headers.
	PutBlock(b BlockRec) error
	GetBlock(hash Hash) (BlockRec, bool, error)
	PutHeader(height uint32, hdr [80]byte) error
	GetHeader(height uint32) ([80]byte, bool, error)

	// Pruning: delete every spent UTXO whose SpentHeight == h. Returns the count
	// evicted. The pruner calls this for the single band that just crossed depth D.
	PruneSpentAtHeight(h uint32) (int, error)

	Stats() Stats
}
