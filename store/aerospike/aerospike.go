//go:build aerospike

// Package aerospike is the Aerospike-backed store.Store (DESIGN.md §5). It mirrors
// the in-memory reference (store/mem) record-for-record. It is behind the `aerospike`
// build tag so the default build, tests and CI stay self-contained (no cluster, no
// client dependency); build it with:
//
//	go get github.com/aerospike/aerospike-client-go/v7
//	go build -tags aerospike ./...
//
// Records are keyed for O(1) lookup; spent outpoints are indexed by spend height in a
// per-height index record so the spend-depth pruner's sweep deletes exactly one band
// (§11.4). BSV only.
package aerospike

import (
	"encoding/binary"
	"fmt"

	aero "github.com/aerospike/aerospike-client-go/v7"

	"memserve/store"
)

// Set (table) names.
const (
	setTxIndex  = "txindex"
	setUTXO     = "utxo"
	setSubtree  = "subtree"
	setBlock    = "block"
	setHeader   = "header"
	setSpentIdx = "spentidx"
)

// Store implements store.Store over an Aerospike namespace.
type Store struct {
	client *aero.Client
	ns     string
	wp     *aero.WritePolicy
	rp     *aero.BasePolicy
}

// Open connects to an Aerospike cluster.
func Open(host string, port int, namespace string) (*Store, error) {
	c, err := aero.NewClient(host, port)
	if err != nil {
		return nil, err
	}
	return &Store{
		client: c,
		ns:     namespace,
		wp:     aero.NewWritePolicy(0, 0),
		rp:     aero.NewPolicy(),
	}, nil
}

// Close releases the client.
func (s *Store) Close() { s.client.Close() }

var _ store.Store = (*Store)(nil)

func (s *Store) key(set string, k []byte) (*aero.Key, error) { return aero.NewKey(s.ns, set, k) }

func outpointKey(op store.Outpoint) []byte {
	b := make([]byte, 36)
	copy(b, op.TxID[:])
	binary.BigEndian.PutUint32(b[32:], op.Vout)
	return b
}

func packHashes(hs []store.Hash) []byte {
	b := make([]byte, 32*len(hs))
	for i, h := range hs {
		copy(b[i*32:], h[:])
	}
	return b
}

func unpackHashes(b []byte) []store.Hash {
	n := len(b) / 32
	out := make([]store.Hash, n)
	for i := 0; i < n; i++ {
		copy(out[i][:], b[i*32:])
	}
	return out
}

// --- TxIndex ---------------------------------------------------------------

func (s *Store) PutTxIndex(txid store.Hash, ix store.TxIndex) error {
	k, err := s.key(setTxIndex, txid[:])
	if err != nil {
		return err
	}
	mined := 0
	if ix.Mined {
		mined = 1
	}
	return s.client.Put(s.wp, k, aero.BinMap{
		"mined": mined, "bh": ix.BlockHash[:], "ht": int(ix.Height),
		"bt": int(ix.BlockTime), "si": int(ix.SubtreeIndex), "li": int(ix.LeafIndex),
		"sn": ix.SeenTime,
	})
}

func (s *Store) GetTxIndex(txid store.Hash) (store.TxIndex, bool, error) {
	k, err := s.key(setTxIndex, txid[:])
	if err != nil {
		return store.TxIndex{}, false, err
	}
	rec, err := s.client.Get(s.rp, k)
	if err != nil {
		return store.TxIndex{}, false, err
	}
	if rec == nil {
		return store.TxIndex{}, false, nil
	}
	ix := store.TxIndex{
		Mined:        toInt(rec.Bins["mined"]) == 1,
		Height:       uint32(toInt(rec.Bins["ht"])),
		BlockTime:    uint32(toInt(rec.Bins["bt"])),
		SubtreeIndex: uint32(toInt(rec.Bins["si"])),
		LeafIndex:    uint32(toInt(rec.Bins["li"])),
		SeenTime:     toInt64(rec.Bins["sn"]),
	}
	copy(ix.BlockHash[:], toBytes(rec.Bins["bh"]))
	return ix, true, nil
}

// --- UTXO ------------------------------------------------------------------

func (s *Store) PutUTXO(op store.Outpoint, u store.UTXO) error {
	k, err := s.key(setUTXO, outpointKey(op))
	if err != nil {
		return err
	}
	spent := 0
	if u.Spent {
		spent = 1
	}
	if err := s.client.Put(s.wp, k, aero.BinMap{
		"val": int(u.Value), "sh": u.ScriptHash[:], "sp": spent,
		"spby": u.SpentBy[:], "spht": int(u.SpentHeight),
	}); err != nil {
		return err
	}
	if u.Spent {
		return s.indexSpent(op, u.SpentHeight)
	}
	return nil
}

func (s *Store) GetUTXO(op store.Outpoint) (store.UTXO, bool, error) {
	k, err := s.key(setUTXO, outpointKey(op))
	if err != nil {
		return store.UTXO{}, false, err
	}
	rec, err := s.client.Get(s.rp, k)
	if err != nil {
		return store.UTXO{}, false, err
	}
	if rec == nil {
		return store.UTXO{}, false, nil
	}
	u := store.UTXO{
		Value:       uint64(toInt(rec.Bins["val"])),
		Spent:       toInt(rec.Bins["sp"]) == 1,
		SpentHeight: uint32(toInt(rec.Bins["spht"])),
	}
	copy(u.ScriptHash[:], toBytes(rec.Bins["sh"]))
	copy(u.SpentBy[:], toBytes(rec.Bins["spby"]))
	return u, true, nil
}

func (s *Store) SpendUTXO(op store.Outpoint, spentBy store.Hash, spentHeight uint32) (bool, error) {
	u, ok, err := s.GetUTXO(op)
	if err != nil || !ok {
		return false, err
	}
	u.Spent = true
	u.SpentBy = spentBy
	u.SpentHeight = spentHeight
	if err := s.PutUTXO(op, u); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) UnspendUTXO(op store.Outpoint) (bool, error) {
	u, ok, err := s.GetUTXO(op)
	if err != nil || !ok {
		return false, err
	}
	if u.Spent {
		_ = s.deindexSpent(op, u.SpentHeight)
	}
	u.Spent = false
	u.SpentBy = store.Hash{}
	u.SpentHeight = 0
	if err := s.PutUTXO(op, u); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) DeleteUTXO(op store.Outpoint) (bool, error) {
	u, ok, err := s.GetUTXO(op)
	if err != nil {
		return false, err
	}
	if ok && u.Spent {
		_ = s.deindexSpent(op, u.SpentHeight)
	}
	k, err := s.key(setUTXO, outpointKey(op))
	if err != nil {
		return false, err
	}
	return s.client.Delete(s.wp, k)
}

func (s *Store) DeleteTxIndex(txid store.Hash) (bool, error) {
	k, err := s.key(setTxIndex, txid[:])
	if err != nil {
		return false, err
	}
	return s.client.Delete(s.wp, k)
}

// spent-height index: one record per height, bin "ops" a list of outpoint keys.
func (s *Store) indexSpent(op store.Outpoint, h uint32) error {
	k, err := s.key(setSpentIdx, u32(h))
	if err != nil {
		return err
	}
	_, err = s.client.Operate(s.wp, k, aero.ListAppendOp("ops", outpointKey(op)))
	return err
}

func (s *Store) deindexSpent(op store.Outpoint, h uint32) error {
	k, err := s.key(setSpentIdx, u32(h))
	if err != nil {
		return err
	}
	_, err = s.client.Operate(s.wp, k, aero.ListRemoveByValueOp("ops", outpointKey(op), aero.ListReturnTypeNone))
	return err
}

func (s *Store) PruneSpentAtHeight(h uint32) (int, error) {
	k, err := s.key(setSpentIdx, u32(h))
	if err != nil {
		return 0, err
	}
	rec, err := s.client.Get(s.rp, k)
	if err != nil {
		return 0, err
	}
	if rec == nil {
		return 0, nil
	}
	list, _ := rec.Bins["ops"].([]interface{})
	n := 0
	for _, v := range list {
		opb := toBytes(v)
		uk, err := s.key(setUTXO, opb)
		if err != nil {
			continue
		}
		if ok, _ := s.client.Delete(s.wp, uk); ok {
			n++
		}
	}
	_, _ = s.client.Delete(s.wp, k)
	return n, nil
}

// --- Subtree / Block / Header ----------------------------------------------

func (s *Store) PutSubtree(root store.Hash, leaves []store.Hash) error {
	k, err := s.key(setSubtree, root[:])
	if err != nil {
		return err
	}
	return s.client.Put(s.wp, k, aero.BinMap{"leaves": packHashes(leaves)})
}

func (s *Store) GetSubtree(root store.Hash) ([]store.Hash, bool, error) {
	k, err := s.key(setSubtree, root[:])
	if err != nil {
		return nil, false, err
	}
	rec, err := s.client.Get(s.rp, k)
	if err != nil {
		return nil, false, err
	}
	if rec == nil {
		return nil, false, nil
	}
	return unpackHashes(toBytes(rec.Bins["leaves"])), true, nil
}

func (s *Store) PutBlock(b store.BlockRec) error {
	k, err := s.key(setBlock, b.Hash[:])
	if err != nil {
		return err
	}
	return s.client.Put(s.wp, k, aero.BinMap{
		"ht": int(b.Height), "bt": int(b.Time), "mr": b.MerkleRoot[:],
		"sr": packHashes(b.SubtreeRoots), "hdr": b.Header[:],
	})
}

func (s *Store) GetBlock(hash store.Hash) (store.BlockRec, bool, error) {
	k, err := s.key(setBlock, hash[:])
	if err != nil {
		return store.BlockRec{}, false, err
	}
	rec, err := s.client.Get(s.rp, k)
	if err != nil {
		return store.BlockRec{}, false, err
	}
	if rec == nil {
		return store.BlockRec{}, false, nil
	}
	b := store.BlockRec{
		Hash:         hash,
		Height:       uint32(toInt(rec.Bins["ht"])),
		Time:         uint32(toInt(rec.Bins["bt"])),
		SubtreeRoots: unpackHashes(toBytes(rec.Bins["sr"])),
	}
	copy(b.MerkleRoot[:], toBytes(rec.Bins["mr"]))
	copy(b.Header[:], toBytes(rec.Bins["hdr"]))
	return b, true, nil
}

func (s *Store) PutHeader(height uint32, hdr [80]byte) error {
	k, err := s.key(setHeader, u32(height))
	if err != nil {
		return err
	}
	return s.client.Put(s.wp, k, aero.BinMap{"hdr": hdr[:]})
}

func (s *Store) GetHeader(height uint32) ([80]byte, bool, error) {
	var out [80]byte
	k, err := s.key(setHeader, u32(height))
	if err != nil {
		return out, false, err
	}
	rec, err := s.client.Get(s.rp, k)
	if err != nil {
		return out, false, err
	}
	if rec == nil {
		return out, false, nil
	}
	copy(out[:], toBytes(rec.Bins["hdr"]))
	return out, true, nil
}

// Stats is approximate for Aerospike (a full scan is costly); returns zeros with a
// note that ops should read namespace statistics from the cluster instead.
func (s *Store) Stats() store.Stats { return store.Stats{} }

// --- helpers ---------------------------------------------------------------

func u32(v uint32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, v)
	return b
}

func toInt(v interface{}) int {
	switch x := v.(type) {
	case int:
		return x
	case int64:
		return int(x)
	default:
		return 0
	}
}

func toInt64(v interface{}) int64 {
	switch x := v.(type) {
	case int:
		return int64(x)
	case int64:
		return x
	default:
		return 0
	}
}

func toBytes(v interface{}) []byte {
	if b, ok := v.([]byte); ok {
		return b
	}
	return nil
}

var _ = fmt.Sprintf
