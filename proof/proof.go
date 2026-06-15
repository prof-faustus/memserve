// Package proof assembles and verifies inclusion proofs from the MemServe store
// (DESIGN.md §4 query 3). A served proof is the L1 (txid -> subtree root) and L2
// (subtree root -> block root) sibling paths plus the 80-byte header — exactly the
// shape the MF-SPV verifier consumes. Assembly reuses the commitment fold; the
// inclusion leaf is the consensus TXID (07 §5), so L0 is empty on the inclusion
// path. BSV only.
package proof

import (
	"errors"

	"memserve/commitment"
	"memserve/store"
)

// Proof is a served inclusion proof. Folding Leaf along L1 then L2 must equal
// BlockRoot, which must be the Merkle root committed in Header.
type Proof struct {
	Leaf        store.Hash            // the consensus txid
	L1          []commitment.PathElem // txid -> subtree root
	SubtreeRoot store.Hash
	L2          []commitment.PathElem // subtree root -> block Merkle root
	BlockRoot   store.Hash
	BlockHash   store.Hash
	Height      uint32
	Header      [80]byte
}

// ErrNotMined is returned when the tx is unknown or only seen (no inclusion proof).
var ErrNotMined = errors.New("proof: transaction not mined (no inclusion proof available)")

// ErrInconsistent is returned when stored data cannot produce a coherent proof.
var ErrInconsistent = errors.New("proof: stored block/subtree data inconsistent")

// Build assembles the inclusion proof for txid from the store. It returns ErrNotMined
// if the tx is not recorded as mined.
func Build(st store.Store, txid store.Hash) (Proof, error) {
	ix, ok, err := st.GetTxIndex(txid)
	if err != nil {
		return Proof{}, err
	}
	if !ok || !ix.Mined {
		return Proof{}, ErrNotMined
	}
	blk, ok, err := st.GetBlock(ix.BlockHash)
	if err != nil {
		return Proof{}, err
	}
	if !ok || int(ix.SubtreeIndex) >= len(blk.SubtreeRoots) {
		return Proof{}, ErrInconsistent
	}
	subtreeRoot := blk.SubtreeRoots[ix.SubtreeIndex]
	leaves, ok, err := st.GetSubtree(subtreeRoot)
	if err != nil {
		return Proof{}, err
	}
	if !ok || int(ix.LeafIndex) >= len(leaves) {
		return Proof{}, ErrInconsistent
	}

	// L1: txid -> subtree root.
	_, subLayers, err := commitment.BuildMerkleTree(leaves)
	if err != nil {
		return Proof{}, err
	}
	l1, err := commitment.MerklePath(subLayers, int(ix.LeafIndex))
	if err != nil {
		return Proof{}, err
	}

	// L2: subtree root -> block Merkle root.
	_, blkLayers, err := commitment.BuildMerkleTree(blk.SubtreeRoots)
	if err != nil {
		return Proof{}, err
	}
	l2, err := commitment.MerklePath(blkLayers, int(ix.SubtreeIndex))
	if err != nil {
		return Proof{}, err
	}

	return Proof{
		Leaf:        txid,
		L1:          l1,
		SubtreeRoot: subtreeRoot,
		L2:          l2,
		BlockRoot:   blk.MerkleRoot,
		BlockHash:   blk.Hash,
		Height:      ix.Height,
		Header:      blk.Header,
	}, nil
}

// Verify reports whether the proof folds the leaf to the committed block root.
// (A full client additionally checks Header is on the most-work PoW chain; that is
// the client's job — MemServe is an untrusted serving cache, DESIGN.md §3.)
func (p Proof) Verify() bool {
	ok, _ := commitment.VerifyToBlockRoot(p.Leaf, nil, p.L1, p.L2, p.BlockRoot)
	return ok
}

// HeaderMerkleRoot extracts the 32-byte Merkle root field from an 80-byte BSV header
// (bytes 36..68, internal byte order as stored).
func HeaderMerkleRoot(hdr [80]byte) store.Hash {
	var h store.Hash
	copy(h[:], hdr[36:68])
	return h
}
