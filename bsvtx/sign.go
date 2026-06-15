package bsvtx

import (
	"errors"
	"math/big"

	"memserve/crypto"
)

// derEncodeInt encodes a positive integer in DER (minimal, with a leading 0x00 when the
// high bit is set so it is interpreted as positive).
func derEncodeInt(v *big.Int) []byte {
	b := v.Bytes()
	if len(b) == 0 {
		b = []byte{0x00}
	}
	if b[0]&0x80 != 0 {
		b = append([]byte{0x00}, b...)
	}
	out := make([]byte, 0, len(b)+2)
	out = append(out, 0x02, byte(len(b)))
	return append(out, b...)
}

// DEREncode encodes an (r,s) ECDSA signature as a DER SEQUENCE (without the trailing
// sighash-type byte) — for callers building scriptSigs from a crypto.Signature.
func DEREncode(r, s *big.Int) []byte { return derEncode(r, s) }

// derEncode encodes (r,s) as a DER SEQUENCE.
func derEncode(r, s *big.Int) []byte {
	rb := derEncodeInt(r)
	sb := derEncodeInt(s)
	body := append(rb, sb...)
	out := make([]byte, 0, len(body)+2)
	out = append(out, 0x30, byte(len(body)))
	return append(out, body...)
}

var errDER = errors.New("bsvtx: malformed DER signature")

// derDecode parses a DER SEQUENCE of two INTEGERs.
func derDecode(b []byte) (r, s *big.Int, err error) {
	if len(b) < 8 || b[0] != 0x30 {
		return nil, nil, errDER
	}
	if int(b[1]) != len(b)-2 {
		return nil, nil, errDER
	}
	i := 2
	readInt := func() (*big.Int, error) {
		if i+2 > len(b) || b[i] != 0x02 {
			return nil, errDER
		}
		l := int(b[i+1])
		i += 2
		if i+l > len(b) {
			return nil, errDER
		}
		v := new(big.Int).SetBytes(b[i : i+l])
		i += l
		return v, nil
	}
	if r, err = readInt(); err != nil {
		return nil, nil, err
	}
	if s, err = readInt(); err != nil {
		return nil, nil, err
	}
	return r, s, nil
}

// SignInput produces the unlocking signature for input idx: a DER-encoded low-S ECDSA
// signature over the FORKID sighash, with the 1-byte hashType appended (the form that
// goes into a scriptSig). scriptCode is the locking script of the output being spent
// (the funding 2-of-2 redeem, here), amount its value.
func SignInput(priv *crypto.PrivateKey, t *Tx, idx int, scriptCode []byte, amount uint64, hashType uint32) ([]byte, error) {
	h := t.SighashForkID(idx, scriptCode, amount, hashType)
	sig, err := priv.Sign(h) // RFC 6979, low-S enforced by crypto.Sign
	if err != nil {
		return nil, err
	}
	out := derEncode(sig.R, sig.S)
	return append(out, byte(hashType)), nil
}

// VerifyInput checks a sigWithType (DER || hashType) for input idx against pub. It
// re-derives the FORKID sighash and rejects non-low-S (malleable) signatures.
func VerifyInput(pub *crypto.PublicKey, t *Tx, idx int, scriptCode []byte, amount uint64, sigWithType []byte) bool {
	if len(sigWithType) < 9 {
		return false
	}
	hashType := uint32(sigWithType[len(sigWithType)-1])
	r, s, err := derDecode(sigWithType[:len(sigWithType)-1])
	if err != nil {
		return false
	}
	sig := &crypto.Signature{R: r, S: s}
	if !sig.IsLowS() {
		return false
	}
	h := t.SighashForkID(idx, scriptCode, amount, hashType)
	return crypto.Verify(pub, h, sig)
}
