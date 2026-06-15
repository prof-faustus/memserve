// Package teranode is MemServe's read-only ingest adapter to Teranode (DESIGN.md
// §7). It defines the records pulled from Teranode (subtrees, sealed blocks, the
// txs that create/spend UTXOs) and a Source interface that streams sealed blocks in
// order. A MockSource produces deterministic, Merkle-consistent blocks so the whole
// pipeline (ingest -> store -> proof -> verify) is testable and benchmarkable
// offline; the real adapter (separate, against pinned Teranode source) implements
// the same Source. BSV/Teranode only — no BTC assumptions.
package teranode

import (
	"encoding/binary"

	"memserve/commitment"
	"memserve/store"
)

// TxOut is one output: a value and a script commitment.
type TxOut struct {
	Value      uint64
	ScriptHash store.Hash
}

// Tx is the minimal transaction MemServe needs to index: its consensus txid, the
// outpoints it spends, and the outputs it creates.
type Tx struct {
	TxID    store.Hash
	Inputs  []store.Outpoint
	Outputs []TxOut
}

// Subtree is a Teranode subtree: the ordered txids it contains (its Merkle leaves)
// and the full txs for ingest. Root is the subtree's Merkle root.
type Subtree struct {
	Root  store.Hash
	TxIDs []store.Hash
	Txs   []Tx // parallel to TxIDs
}

// Block is a sealed block: its header/root, the subtree roots (block Merkle leaves)
// and the subtrees themselves.
type Block struct {
	Hash         store.Hash
	Height       uint32
	Time         uint32
	MerkleRoot   store.Hash
	SubtreeRoots []store.Hash
	Header       [80]byte
	Subtrees     []Subtree
}

// Source streams sealed blocks in order. Next returns the next block; ok=false when
// the source is exhausted. TipHeight is the height of the latest block produced.
type Source interface {
	Next() (b Block, ok bool, err error)
	TipHeight() uint32
}

// --- Deterministic mock source ----------------------------------------------

// MockConfig parameters the synthetic chain.
type MockConfig struct {
	Blocks        int    // number of blocks to produce
	SubtreesPer   int    // subtrees per block
	TxsPerSubtree int    // txs per subtree
	SpendFraction int    // 1/N of prior live UTXOs each block spends (0 = none)
	StartHeight   uint32 // height of the first block
	Seed          uint64
}

// MockSource is a deterministic Source: byte-identical chains for a given config.
type MockSource struct {
	cfg    MockConfig
	height uint32
	made   int
	rng    uint64
	live   []store.Outpoint // outpoints created and not yet spent (for realistic spends)
}

// NewMock builds a MockSource.
func NewMock(cfg MockConfig) *MockSource {
	if cfg.Blocks <= 0 {
		cfg.Blocks = 1
	}
	if cfg.SubtreesPer <= 0 {
		cfg.SubtreesPer = 1
	}
	if cfg.TxsPerSubtree <= 0 {
		cfg.TxsPerSubtree = 1
	}
	seed := cfg.Seed
	if seed == 0 {
		seed = 0x9e3779b97f4a7c15
	}
	return &MockSource{cfg: cfg, height: cfg.StartHeight, rng: seed}
}

// splitmix64 — small deterministic PRNG (no external deps).
func (m *MockSource) next64() uint64 {
	m.rng += 0x9e3779b97f4a7c15
	z := m.rng
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

func (m *MockSource) txid(blk uint32, sub, idx int) store.Hash {
	var buf [20]byte
	binary.BigEndian.PutUint32(buf[0:4], blk)
	binary.BigEndian.PutUint32(buf[4:8], uint32(sub))
	binary.BigEndian.PutUint32(buf[8:12], uint32(idx))
	binary.BigEndian.PutUint64(buf[12:20], m.next64())
	return commitment.DoubleSHA256(buf[:])
}

func (m *MockSource) TipHeight() uint32 {
	if m.made == 0 {
		return m.cfg.StartHeight
	}
	return m.height - 1
}

// Next produces the next sealed, Merkle-consistent block.
func (m *MockSource) Next() (Block, bool, error) {
	if m.made >= m.cfg.Blocks {
		return Block{}, false, nil
	}
	h := m.height
	blk := Block{Height: h, Time: 1700000000 + h*600}

	// choose this block's spends from the live set.
	var toSpend []store.Outpoint
	if m.cfg.SpendFraction > 0 && len(m.live) > 0 {
		n := len(m.live) / m.cfg.SpendFraction
		toSpend = m.live[:n]
		m.live = m.live[n:]
	}
	spendIdx := 0

	subRoots := make([]store.Hash, 0, m.cfg.SubtreesPer)
	subtrees := make([]Subtree, 0, m.cfg.SubtreesPer)
	for s := 0; s < m.cfg.SubtreesPer; s++ {
		txids := make([]store.Hash, 0, m.cfg.TxsPerSubtree)
		txs := make([]Tx, 0, m.cfg.TxsPerSubtree)
		for i := 0; i < m.cfg.TxsPerSubtree; i++ {
			id := m.txid(h, s, i)
			var ins []store.Outpoint
			if spendIdx < len(toSpend) {
				ins = []store.Outpoint{toSpend[spendIdx]}
				spendIdx++
			}
			outs := []TxOut{{Value: 1000 + m.next64()%9000, ScriptHash: commitment.DoubleSHA256(id[:])}}
			txids = append(txids, id)
			txs = append(txs, Tx{TxID: id, Inputs: ins, Outputs: outs})
			// the new output becomes a live UTXO for future spends.
			m.live = append(m.live, store.Outpoint{TxID: id, Vout: 0})
		}
		root, _ := commitment.MerkleRoot(txids)
		subRoots = append(subRoots, root)
		subtrees = append(subtrees, Subtree{Root: root, TxIDs: txids, Txs: txs})
	}
	blockRoot, _ := commitment.MerkleRoot(subRoots)
	blk.MerkleRoot = blockRoot
	blk.SubtreeRoots = subRoots
	blk.Subtrees = subtrees
	blk.Header = buildHeader(h, blk.Time, blockRoot)
	blk.Hash = commitment.DoubleSHA256(blk.Header[:])

	m.height++
	m.made++
	return blk, true, nil
}

// buildHeader lays out an 80-byte BSV header with the Merkle root in bytes 36..68.
// (version|prevhash|merkleroot|time|bits|nonce — the fields MemServe needs to serve;
// PoW validity is the client's check against the chain, not MemServe's.)
func buildHeader(height, t uint32, root store.Hash) [80]byte {
	var hdr [80]byte
	binary.LittleEndian.PutUint32(hdr[0:4], 0x20000000)   // version
	binary.BigEndian.PutUint32(hdr[32:36], height)        // (prevhash region; height marker)
	copy(hdr[36:68], root[:])                             // merkle root
	binary.LittleEndian.PutUint32(hdr[68:72], t)          // time
	binary.LittleEndian.PutUint32(hdr[72:76], 0x1d00ffff) // bits
	return hdr
}
