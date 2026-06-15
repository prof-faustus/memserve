// Package mem is the in-memory Store backend: the default implementation used by
// tests and benchmarks, and the reference the Aerospike adapter mirrors.
//
// It is internally STRIPED by the first byte of the key hash (256 independent
// stripes, each with its own lock and maps). Because txids are uniform hashes, reads
// and writes spread evenly across stripes, so a single MemServe box scales across
// many cores instead of serializing on one RWMutex cache line — the same hash-prefix
// idea used for cross-server sharding (DESIGN.md §6), applied inside the process.
//
// Each stripe keeps a secondary index of spent outpoints by spend height, so the
// spend-depth pruner's per-block sweep is O(band), not a full scan (§11.4). Safe for
// concurrent use. BSV only.
package mem

import (
	"sync"

	"memserve/store"
)

const stripeCount = 256 // one per value of key[0]

type stripe struct {
	mu       sync.RWMutex
	txindex  map[store.Hash]store.TxIndex
	utxo     map[store.Outpoint]store.UTXO
	subtree  map[store.Hash][]store.Hash
	blocks   map[store.Hash]store.BlockRec
	spentIdx map[uint32]map[store.Outpoint]struct{} // spentHeight -> set
}

func newStripe() *stripe {
	return &stripe{
		txindex:  make(map[store.Hash]store.TxIndex),
		utxo:     make(map[store.Outpoint]store.UTXO),
		subtree:  make(map[store.Hash][]store.Hash),
		blocks:   make(map[store.Hash]store.BlockRec),
		spentIdx: make(map[uint32]map[store.Outpoint]struct{}),
	}
}

// heightEntry indexes the records stored for one block height, so they can be pruned by
// depth (DESIGN.md §11.7). Populated on ingest (PutBlock/PutTxIndex).
type heightEntry struct {
	block    store.Hash
	hasBlock bool
	subtrees []store.Hash
	txids    []store.Hash
}

// Store is a striped in-memory store.Store.
type Store struct {
	st   [stripeCount]*stripe
	hmu  sync.RWMutex
	hdr  map[uint32][80]byte // headers (one per block; kept in a single small map)
	imu  sync.Mutex
	hidx map[uint32]*heightEntry // height -> the records stored for it (for depth retention)
}

// New returns an empty in-memory store.
func New() *Store {
	s := &Store{hdr: make(map[uint32][80]byte), hidx: make(map[uint32]*heightEntry)}
	for i := range s.st {
		s.st[i] = newStripe()
	}
	return s
}

var _ store.Store = (*Store)(nil)

func striperOf(h store.Hash) int { return int(h[0]) }

func (s *Store) PutTxIndex(txid store.Hash, ix store.TxIndex) error {
	st := s.st[striperOf(txid)]
	st.mu.Lock()
	_, existed := st.txindex[txid]
	st.txindex[txid] = ix
	st.mu.Unlock()
	if ix.Mined && !existed {
		s.imu.Lock()
		e := s.hidx[ix.Height]
		if e == nil {
			e = &heightEntry{}
			s.hidx[ix.Height] = e
		}
		e.txids = append(e.txids, txid)
		s.imu.Unlock()
	}
	return nil
}

func (s *Store) GetTxIndex(txid store.Hash) (store.TxIndex, bool, error) {
	st := s.st[striperOf(txid)]
	st.mu.RLock()
	ix, ok := st.txindex[txid]
	st.mu.RUnlock()
	return ix, ok, nil
}

func (s *Store) PutUTXO(op store.Outpoint, u store.UTXO) error {
	st := s.st[striperOf(op.TxID)]
	st.mu.Lock()
	defer st.mu.Unlock()
	st.utxo[op] = u
	if u.Spent {
		indexSpent(st, op, u.SpentHeight)
	}
	return nil
}

func (s *Store) GetUTXO(op store.Outpoint) (store.UTXO, bool, error) {
	st := s.st[striperOf(op.TxID)]
	st.mu.RLock()
	u, ok := st.utxo[op]
	st.mu.RUnlock()
	return u, ok, nil
}

func (s *Store) SpendUTXO(op store.Outpoint, spentBy store.Hash, spentHeight uint32) (bool, error) {
	st := s.st[striperOf(op.TxID)]
	st.mu.Lock()
	defer st.mu.Unlock()
	u, ok := st.utxo[op]
	if !ok {
		return false, nil
	}
	u.Spent = true
	u.SpentBy = spentBy
	u.SpentHeight = spentHeight
	st.utxo[op] = u
	indexSpent(st, op, spentHeight)
	return true, nil
}

func (s *Store) UnspendUTXO(op store.Outpoint) (bool, error) {
	st := s.st[striperOf(op.TxID)]
	st.mu.Lock()
	defer st.mu.Unlock()
	u, ok := st.utxo[op]
	if !ok {
		return false, nil
	}
	if u.Spent {
		deindexSpent(st, op, u.SpentHeight)
	}
	u.Spent = false
	u.SpentBy = store.Hash{}
	u.SpentHeight = 0
	st.utxo[op] = u
	return true, nil
}

func (s *Store) DeleteUTXO(op store.Outpoint) (bool, error) {
	st := s.st[striperOf(op.TxID)]
	st.mu.Lock()
	defer st.mu.Unlock()
	u, ok := st.utxo[op]
	if !ok {
		return false, nil
	}
	if u.Spent {
		deindexSpent(st, op, u.SpentHeight)
	}
	delete(st.utxo, op)
	return true, nil
}

func (s *Store) DeleteTxIndex(txid store.Hash) (bool, error) {
	st := s.st[striperOf(txid)]
	st.mu.Lock()
	defer st.mu.Unlock()
	if _, ok := st.txindex[txid]; !ok {
		return false, nil
	}
	delete(st.txindex, txid)
	return true, nil
}

func indexSpent(st *stripe, op store.Outpoint, h uint32) {
	set := st.spentIdx[h]
	if set == nil {
		set = make(map[store.Outpoint]struct{})
		st.spentIdx[h] = set
	}
	set[op] = struct{}{}
}

func deindexSpent(st *stripe, op store.Outpoint, h uint32) {
	if set := st.spentIdx[h]; set != nil {
		delete(set, op)
		if len(set) == 0 {
			delete(st.spentIdx, h)
		}
	}
}

func (s *Store) PutSubtree(root store.Hash, leaves []store.Hash) error {
	cp := make([]store.Hash, len(leaves))
	copy(cp, leaves)
	st := s.st[striperOf(root)]
	st.mu.Lock()
	st.subtree[root] = cp
	st.mu.Unlock()
	return nil
}

func (s *Store) GetSubtree(root store.Hash) ([]store.Hash, bool, error) {
	st := s.st[striperOf(root)]
	st.mu.RLock()
	v, ok := st.subtree[root]
	st.mu.RUnlock()
	return v, ok, nil
}

func (s *Store) PutBlock(b store.BlockRec) error {
	st := s.st[striperOf(b.Hash)]
	st.mu.Lock()
	st.blocks[b.Hash] = b
	st.mu.Unlock()
	s.imu.Lock()
	e := s.hidx[b.Height]
	if e == nil {
		e = &heightEntry{}
		s.hidx[b.Height] = e
	}
	e.block, e.hasBlock = b.Hash, true
	e.subtrees = append([]store.Hash(nil), b.SubtreeRoots...)
	s.imu.Unlock()
	return nil
}

func (s *Store) GetBlock(hash store.Hash) (store.BlockRec, bool, error) {
	st := s.st[striperOf(hash)]
	st.mu.RLock()
	b, ok := st.blocks[hash]
	st.mu.RUnlock()
	return b, ok, nil
}

func (s *Store) PutHeader(height uint32, hdr [80]byte) error {
	s.hmu.Lock()
	s.hdr[height] = hdr
	s.hmu.Unlock()
	return nil
}

func (s *Store) GetHeader(height uint32) ([80]byte, bool, error) {
	s.hmu.RLock()
	h, ok := s.hdr[height]
	s.hmu.RUnlock()
	return h, ok, nil
}

// PruneSpentAtHeight deletes every spent UTXO whose SpentHeight == h across all
// stripes. Per stripe it touches only that height's band (O(band), not a full scan).
func (s *Store) PruneSpentAtHeight(h uint32) (int, error) {
	n := 0
	for _, st := range s.st {
		st.mu.Lock()
		if set := st.spentIdx[h]; set != nil {
			for op := range set {
				if u, ok := st.utxo[op]; ok && u.Spent && u.SpentHeight == h {
					delete(st.utxo, op)
					n++
				}
			}
			delete(st.spentIdx, h)
		}
		st.mu.Unlock()
	}
	return n, nil
}

// PruneIndexAtHeight removes the block, subtrees, tx-index entries and header recorded at
// height h (depth-retention of index/proof data, §11.7). Returns records freed.
func (s *Store) PruneIndexAtHeight(h uint32) (int, error) {
	s.imu.Lock()
	e := s.hidx[h]
	delete(s.hidx, h)
	s.imu.Unlock()

	n := 0
	if e != nil {
		if e.hasBlock {
			st := s.st[striperOf(e.block)]
			st.mu.Lock()
			if _, ok := st.blocks[e.block]; ok {
				delete(st.blocks, e.block)
				n++
			}
			st.mu.Unlock()
		}
		for _, r := range e.subtrees {
			st := s.st[striperOf(r)]
			st.mu.Lock()
			if _, ok := st.subtree[r]; ok {
				delete(st.subtree, r)
				n++
			}
			st.mu.Unlock()
		}
		for _, id := range e.txids {
			st := s.st[striperOf(id)]
			st.mu.Lock()
			if _, ok := st.txindex[id]; ok {
				delete(st.txindex, id)
				n++
			}
			st.mu.Unlock()
		}
	}
	s.hmu.Lock()
	if _, ok := s.hdr[h]; ok {
		delete(s.hdr, h)
		n++
	}
	s.hmu.Unlock()
	return n, nil
}

func (s *Store) Stats() store.Stats {
	var out store.Stats
	for _, st := range s.st {
		st.mu.RLock()
		out.TxIndex += len(st.txindex)
		out.Subtrees += len(st.subtree)
		out.Blocks += len(st.blocks)
		for _, u := range st.utxo {
			if u.Spent {
				out.UTXOSpent++
			} else {
				out.UTXOLive++
			}
		}
		st.mu.RUnlock()
	}
	s.hmu.RLock()
	out.Headers = len(s.hdr)
	s.hmu.RUnlock()
	return out
}
