// Package bsvtx builds and signs REAL BSV transactions for the payment channel
// (DESIGN.md §10): standard Bitcoin serialization (no SegWit/witness — BSV has none),
// SHA-256d txids, the mandatory FORKID sighash (BIP143-style preimage with the 0x40
// FORKID flag), DER-encoded low-S ECDSA signatures, and the funding (2-of-2),
// commitment, refund and settlement transactions. Uses bare multisig + P2PK so it needs
// no RIPEMD-160 (zero external deps), which is valid BSV script.
//
// HONEST SCOPE: serialization, txid and the FORKID sighash are implemented to spec and
// self-consistency-tested (sign→verify over the real preimage, deterministic txids).
// Final consensus acceptance requires broadcasting on BSV testnet (the VM enables this);
// that is the remaining validation step, called out in the docs. BSV only — no BTC
// (no CLTV/CSV; nLockTime; secp256k1; SHA-256d).
package bsvtx

import (
	"bytes"
	"encoding/binary"

	"memserve/commitment"
)

// OutPoint references a previous output by txid (internal byte order) and index.
type OutPoint struct {
	Hash  [32]byte // double-SHA256 of the referenced tx, internal order
	Index uint32
}

// TxIn is a transaction input.
type TxIn struct {
	PrevOut   OutPoint
	ScriptSig []byte
	Sequence  uint32
}

// TxOut is a transaction output.
type TxOut struct {
	Value        uint64
	ScriptPubKey []byte
}

// Tx is a BSV transaction.
type Tx struct {
	Version  int32
	Inputs   []TxIn
	Outputs  []TxOut
	LockTime uint32
}

// FinalSequence is the default (locktime-inactive) input sequence.
const FinalSequence = 0xffffffff

// EnableLockTimeSequence makes nLockTime active on an input (any value < 0xffffffff).
const EnableLockTimeSequence = 0xfffffffe

func putVarInt(buf *bytes.Buffer, n uint64) {
	switch {
	case n < 0xfd:
		buf.WriteByte(byte(n))
	case n <= 0xffff:
		buf.WriteByte(0xfd)
		var b [2]byte
		binary.LittleEndian.PutUint16(b[:], uint16(n))
		buf.Write(b[:])
	case n <= 0xffffffff:
		buf.WriteByte(0xfe)
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], uint32(n))
		buf.Write(b[:])
	default:
		buf.WriteByte(0xff)
		var b [8]byte
		binary.LittleEndian.PutUint64(b[:], n)
		buf.Write(b[:])
	}
}

func putU32(buf *bytes.Buffer, v uint32) {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], v)
	buf.Write(b[:])
}

func putU64(buf *bytes.Buffer, v uint64) {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], v)
	buf.Write(b[:])
}

// Serialize returns the canonical (non-witness) transaction bytes.
func (t *Tx) Serialize() []byte {
	var buf bytes.Buffer
	putU32(&buf, uint32(t.Version))
	putVarInt(&buf, uint64(len(t.Inputs)))
	for _, in := range t.Inputs {
		buf.Write(in.PrevOut.Hash[:])
		putU32(&buf, in.PrevOut.Index)
		putVarInt(&buf, uint64(len(in.ScriptSig)))
		buf.Write(in.ScriptSig)
		putU32(&buf, in.Sequence)
	}
	putVarInt(&buf, uint64(len(t.Outputs)))
	for _, out := range t.Outputs {
		putU64(&buf, out.Value)
		putVarInt(&buf, uint64(len(out.ScriptPubKey)))
		buf.Write(out.ScriptPubKey)
	}
	putU32(&buf, t.LockTime)
	return buf.Bytes()
}

// TxID returns the double-SHA256 of the serialized tx in INTERNAL byte order (the form
// used in a child's OutPoint.Hash). The display id is the reverse of this.
func (t *Tx) TxID() [32]byte {
	return commitment.DoubleSHA256(t.Serialize())
}

// DisplayID returns the conventional big-endian (reversed) txid for display/APIs.
func (t *Tx) DisplayID() [32]byte {
	id := t.TxID()
	var r [32]byte
	for i := 0; i < 32; i++ {
		r[i] = id[31-i]
	}
	return r
}
