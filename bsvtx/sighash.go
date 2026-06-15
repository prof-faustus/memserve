package bsvtx

import (
	"bytes"

	"memserve/commitment"
)

// Sighash flags. BSV mandates FORKID (0x40) — the post-fork replay-protected sighash.
const (
	SighashAll     = 0x01
	SighashNone    = 0x02
	SighashSingle  = 0x03
	SighashForkID  = 0x40
	SighashAnyone  = 0x80
	SighashAllFork = SighashAll | SighashForkID // 0x41 — the channel default
)

// SighashForkID computes the FORKID (BIP143-style) signature hash for input `idx`,
// spending an output with locking script `scriptCode` and value `amount`. This is the
// preimage BSV requires for signatures since the fork. Implemented for the SIGHASH_ALL |
// FORKID case used by the channel (commitment/refund/settlement spends of the funding
// output).
func (t *Tx) SighashForkID(idx int, scriptCode []byte, amount uint64, hashType uint32) []byte {
	var hashPrevouts, hashSequence, hashOutputs [32]byte

	if hashType&SighashAnyone == 0 {
		var b bytes.Buffer
		for _, in := range t.Inputs {
			b.Write(in.PrevOut.Hash[:])
			putU32(&b, in.PrevOut.Index)
		}
		hashPrevouts = commitment.DoubleSHA256(b.Bytes())
	}

	if hashType&SighashAnyone == 0 && (hashType&0x1f) != SighashSingle && (hashType&0x1f) != SighashNone {
		var b bytes.Buffer
		for _, in := range t.Inputs {
			putU32(&b, in.Sequence)
		}
		hashSequence = commitment.DoubleSHA256(b.Bytes())
	}

	if (hashType&0x1f) != SighashSingle && (hashType&0x1f) != SighashNone {
		var b bytes.Buffer
		for _, out := range t.Outputs {
			putU64(&b, out.Value)
			putVarInt(&b, uint64(len(out.ScriptPubKey)))
			b.Write(out.ScriptPubKey)
		}
		hashOutputs = commitment.DoubleSHA256(b.Bytes())
	} else if (hashType&0x1f) == SighashSingle && idx < len(t.Outputs) {
		var b bytes.Buffer
		out := t.Outputs[idx]
		putU64(&b, out.Value)
		putVarInt(&b, uint64(len(out.ScriptPubKey)))
		b.Write(out.ScriptPubKey)
		hashOutputs = commitment.DoubleSHA256(b.Bytes())
	}

	var pre bytes.Buffer
	putU32(&pre, uint32(t.Version))
	pre.Write(hashPrevouts[:])
	pre.Write(hashSequence[:])
	in := t.Inputs[idx]
	pre.Write(in.PrevOut.Hash[:])
	putU32(&pre, in.PrevOut.Index)
	putVarInt(&pre, uint64(len(scriptCode)))
	pre.Write(scriptCode)
	putU64(&pre, amount)
	putU32(&pre, in.Sequence)
	pre.Write(hashOutputs[:])
	putU32(&pre, t.LockTime)
	putU32(&pre, hashType)

	h := commitment.DoubleSHA256(pre.Bytes())
	return h[:]
}
