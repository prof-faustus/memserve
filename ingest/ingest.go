// Package ingest is MemServe's run server (DESIGN.md §7): it pulls sealed blocks
// from a teranode.Source and writes the indexed records into the store, keeping only
// the txs whose txid prefix this shard owns. After each block it drives the
// spend-depth pruner (§11). Ingest is itself shardable — each MemServe server
// ingests only its prefix. BSV only.
package ingest

import (
	"errors"

	"memserve/commitment"
	"memserve/proof"
	"memserve/prune"
	"memserve/shard"
	"memserve/store"
	"memserve/teranode"
)

// ErrInvalidBlock is returned when a block from Teranode fails internal Merkle
// consistency — the anti-"ingest poisoning" check (DESIGN.md §15). Malformed data is
// rejected and nothing is stored.
var ErrInvalidBlock = errors.New("ingest: block failed Merkle-consistency validation")

// Ingestor writes Teranode data into one shard's store.
type Ingestor struct {
	st           store.Store
	pruner       *prune.Pruner
	k            uint   // shard prefix width (0 = single shard, ingest all)
	id           uint32 // this server's shard id
	skipValidate bool
}

// Config parameters an Ingestor.
type Config struct {
	K            uint   // shard prefix width; 0 means a single (un-sharded) server
	ID           uint32 // this server's shard id (must be < 2^K)
	SkipValidate bool   // skip Merkle-consistency validation (only if Teranode is trusted/pre-validated)
}

// New builds an Ingestor over a store and pruner.
func New(st store.Store, pruner *prune.Pruner, cfg Config) *Ingestor {
	return &Ingestor{st: st, pruner: pruner, k: cfg.K, id: cfg.ID, skipValidate: cfg.SkipValidate}
}

// owns reports whether this shard owns txid.
func (in *Ingestor) owns(txid store.Hash) bool {
	if in.k == 0 {
		return true
	}
	return shard.Of(txid, in.k) == in.id
}

// Stats from ingestion.
type Stats struct {
	Blocks     int
	TxsIndexed int
	UTXOsMade  int
	UTXOsSpent int
	Pruned     int
}

// validateBlock recomputes every subtree root from its txids, the block root from the
// subtree roots, and checks both against what Teranode claimed and the header's committed
// root. This rejects malformed/poisoned ingest data before any of it is indexed.
func validateBlock(b teranode.Block) error {
	if len(b.Subtrees) != len(b.SubtreeRoots) {
		return ErrInvalidBlock
	}
	for i, sub := range b.Subtrees {
		root, err := commitment.MerkleRoot(sub.TxIDs)
		if err != nil || root != sub.Root || root != b.SubtreeRoots[i] {
			return ErrInvalidBlock
		}
	}
	blockRoot, err := commitment.MerkleRoot(b.SubtreeRoots)
	if err != nil || blockRoot != b.MerkleRoot {
		return ErrInvalidBlock
	}
	if proof.HeaderMerkleRoot(b.Header) != b.MerkleRoot {
		return ErrInvalidBlock
	}
	return nil
}

// IngestBlock indexes one sealed block into this shard's store and runs the pruner.
func (in *Ingestor) IngestBlock(b teranode.Block) (Stats, error) {
	var st Stats
	if !in.skipValidate {
		if err := validateBlock(b); err != nil {
			return st, err
		}
	}
	// Store the block's subtree leaves and the block record + header.
	for _, sub := range b.Subtrees {
		// Only store subtrees that contain at least one owned tx (cheap: store all
		// when un-sharded; when sharded, a subtree may still be needed for L1 paths
		// of owned txs, so store it whenever it holds an owned tx).
		ownAny := in.k == 0
		if !ownAny {
			for _, id := range sub.TxIDs {
				if in.owns(id) {
					ownAny = true
					break
				}
			}
		}
		if ownAny {
			if err := in.st.PutSubtree(sub.Root, sub.TxIDs); err != nil {
				return st, err
			}
		}
	}
	if err := in.st.PutBlock(store.BlockRec{
		Hash:         b.Hash,
		Height:       b.Height,
		Time:         b.Time,
		MerkleRoot:   b.MerkleRoot,
		SubtreeRoots: b.SubtreeRoots,
		Header:       b.Header,
	}); err != nil {
		return st, err
	}
	if err := in.st.PutHeader(b.Height, b.Header); err != nil {
		return st, err
	}

	// Index txs and apply UTXO deltas for owned txs.
	for si, sub := range b.Subtrees {
		for li, tx := range sub.Txs {
			if !in.owns(tx.TxID) {
				continue
			}
			if err := in.st.PutTxIndex(tx.TxID, store.TxIndex{
				Mined:        true,
				BlockHash:    b.Hash,
				Height:       b.Height,
				BlockTime:    b.Time,
				SubtreeIndex: uint32(si),
				LeafIndex:    uint32(li),
				SeenTime:     int64(b.Time) * 1e9,
			}); err != nil {
				return st, err
			}
			st.TxsIndexed++
			// Spend the inputs this tx consumes (mark spent at this height).
			for _, op := range tx.Inputs {
				if ok, err := in.st.SpendUTXO(op, tx.TxID, b.Height); err != nil {
					return st, err
				} else if ok {
					st.UTXOsSpent++
				}
			}
			// Create the outputs.
			for vout, o := range tx.Outputs {
				if err := in.st.PutUTXO(store.Outpoint{TxID: tx.TxID, Vout: uint32(vout)}, store.UTXO{
					Value:      o.Value,
					ScriptHash: o.ScriptHash,
				}); err != nil {
					return st, err
				}
				st.UTXOsMade++
			}
		}
	}
	st.Blocks = 1

	// Drive spend-depth pruning for the new tip.
	if in.pruner != nil {
		n, err := in.pruner.OnNewBlock(b.Height)
		if err != nil {
			return st, err
		}
		st.Pruned = n
	}
	return st, nil
}

// RollbackBlock reverses IngestBlock for a reorged-out block (DESIGN.md §11.2): it
// un-spends the inputs that block's txs consumed (restoring those outputs to unspent),
// and deletes the outputs and tx-index entries that block created. This is correct ONLY
// because spend-depth pruning never evicts within the reorg horizon (the record is still
// present to roll back) — which is exactly why D >= ReorgHorizon is a correctness floor.
func (in *Ingestor) RollbackBlock(b teranode.Block) error {
	for _, sub := range b.Subtrees {
		for _, tx := range sub.Txs {
			if !in.owns(tx.TxID) {
				continue
			}
			// restore the inputs this tx spent.
			for _, op := range tx.Inputs {
				if _, err := in.st.UnspendUTXO(op); err != nil {
					return err
				}
			}
			// remove the outputs this tx created and its index entry.
			for vout := range tx.Outputs {
				if _, err := in.st.DeleteUTXO(store.Outpoint{TxID: tx.TxID, Vout: uint32(vout)}); err != nil {
					return err
				}
			}
			if _, err := in.st.DeleteTxIndex(tx.TxID); err != nil {
				return err
			}
		}
	}
	return nil
}

// Run drains the source, ingesting every block, accumulating stats.
func (in *Ingestor) Run(src teranode.Source) (Stats, error) {
	var total Stats
	for {
		b, ok, err := src.Next()
		if err != nil {
			return total, err
		}
		if !ok {
			return total, nil
		}
		s, err := in.IngestBlock(b)
		if err != nil {
			return total, err
		}
		total.Blocks += s.Blocks
		total.TxsIndexed += s.TxsIndexed
		total.UTXOsMade += s.UTXOsMade
		total.UTXOsSpent += s.UTXOsSpent
		total.Pruned += s.Pruned
	}
}
