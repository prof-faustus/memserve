package bsvtx

import "bytes"

// Script opcodes used here.
const (
	op0             = 0x00
	op2             = 0x52
	opCheckSig      = 0xac
	opCheckMultiSig = 0xae
)

// pushData appends a canonical minimal push of data (only the <=75-byte and OP_PUSHDATA1
// forms are needed for pubkeys/signatures).
func pushData(buf *bytes.Buffer, data []byte) {
	n := len(data)
	switch {
	case n < 0x4c:
		buf.WriteByte(byte(n))
	case n <= 0xff:
		buf.WriteByte(0x4c) // OP_PUSHDATA1
		buf.WriteByte(byte(n))
	default:
		buf.WriteByte(0x4d) // OP_PUSHDATA2
		buf.WriteByte(byte(n))
		buf.WriteByte(byte(n >> 8))
	}
	buf.Write(data)
}

// P2PK builds a pay-to-pubkey script: <pub> OP_CHECKSIG. (No RIPEMD-160 needed.)
func P2PK(compressedPub []byte) []byte {
	var b bytes.Buffer
	pushData(&b, compressedPub)
	b.WriteByte(opCheckSig)
	return b.Bytes()
}

// Multisig2of2 builds a bare 2-of-2 multisig script: OP_2 <a> <b> OP_2 OP_CHECKMULTISIG.
// Used as the funding output's locking script; both parties must sign to move funds.
func Multisig2of2(pubA, pubB []byte) []byte {
	var b bytes.Buffer
	b.WriteByte(op2)
	pushData(&b, pubA)
	pushData(&b, pubB)
	b.WriteByte(op2)
	b.WriteByte(opCheckMultiSig)
	return b.Bytes()
}

// ScriptSigP2PK builds the unlocking script for a P2PK input: <sig+hashtype>.
func ScriptSigP2PK(sigWithType []byte) []byte {
	var b bytes.Buffer
	pushData(&b, sigWithType)
	return b.Bytes()
}

// ScriptSig2of2 builds the unlocking script for a bare 2-of-2 input:
// OP_0 <sigA+hashtype> <sigB+hashtype>. The leading OP_0 is the well-known
// OP_CHECKMULTISIG dummy. Signature order must match the pubkey order in the redeem.
func ScriptSig2of2(sigA, sigB []byte) []byte {
	var b bytes.Buffer
	b.WriteByte(op0)
	pushData(&b, sigA)
	pushData(&b, sigB)
	return b.Bytes()
}
