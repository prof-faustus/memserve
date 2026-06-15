package bsvtx

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"math/big"
)

// P2PKH / address support so the funding tx can spend faucet (P2PKH) coins and so the
// tool can print a fundable address. Base58Check + Hash160, standard Bitcoin/BSV rules.

// Address version bytes.
const (
	AddrMainP2PKH = 0x00
	AddrTestP2PKH = 0x6f
)

// Hash160 = RIPEMD160(SHA256(b)).
func Hash160(b []byte) [20]byte {
	s := sha256.Sum256(b)
	return ripemd160(s[:])
}

const b58alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

// Base58Encode encodes bytes in Bitcoin Base58 (leading zero bytes -> '1').
func Base58Encode(b []byte) string {
	n := new(big.Int).SetBytes(b)
	base := big.NewInt(58)
	zero := big.NewInt(0)
	mod := new(big.Int)
	var out []byte
	for n.Cmp(zero) > 0 {
		n.DivMod(n, base, mod)
		out = append(out, b58alphabet[mod.Int64()])
	}
	for _, c := range b {
		if c != 0 {
			break
		}
		out = append(out, b58alphabet[0])
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return string(out)
}

// Base58Decode reverses Base58Encode.
func Base58Decode(s string) ([]byte, error) {
	n := big.NewInt(0)
	base := big.NewInt(58)
	for _, r := range s {
		idx := -1
		for i := 0; i < len(b58alphabet); i++ {
			if b58alphabet[i] == byte(r) {
				idx = i
				break
			}
		}
		if idx < 0 {
			return nil, errors.New("bsvtx: invalid base58 character")
		}
		n.Mul(n, base)
		n.Add(n, big.NewInt(int64(idx)))
	}
	out := n.Bytes()
	for _, r := range s {
		if byte(r) != b58alphabet[0] {
			break
		}
		out = append([]byte{0}, out...)
	}
	return out, nil
}

// Base58Check encodes payload with a 4-byte SHA-256d checksum.
func Base58Check(payload []byte) string {
	c := sha256.Sum256(payload)
	c = sha256.Sum256(c[:])
	return Base58Encode(append(append([]byte{}, payload...), c[:4]...))
}

// Base58CheckDecode decodes and validates the checksum, returning the payload.
func Base58CheckDecode(s string) ([]byte, error) {
	raw, err := Base58Decode(s)
	if err != nil {
		return nil, err
	}
	if len(raw) < 5 {
		return nil, errors.New("bsvtx: base58check too short")
	}
	payload, sum := raw[:len(raw)-4], raw[len(raw)-4:]
	c := sha256.Sum256(payload)
	c = sha256.Sum256(c[:])
	for i := 0; i < 4; i++ {
		if c[i] != sum[i] {
			return nil, errors.New("bsvtx: bad base58check checksum")
		}
	}
	return payload, nil
}

// AddressFromPubKey returns the P2PKH address for a compressed pubkey under version.
func AddressFromPubKey(compressedPub []byte, version byte) string {
	h := Hash160(compressedPub)
	return Base58Check(append([]byte{version}, h[:]...))
}

// P2PKH builds the standard locking script for a pubkey hash:
// OP_DUP OP_HASH160 <20> OP_EQUALVERIFY OP_CHECKSIG.
func P2PKH(pubKeyHash [20]byte) []byte {
	out := make([]byte, 0, 25)
	out = append(out, 0x76, 0xa9, 0x14)
	out = append(out, pubKeyHash[:]...)
	out = append(out, 0x88, 0xac)
	return out
}

// P2PKHFromPub builds the P2PKH locking script for a compressed pubkey.
func P2PKHFromPub(compressedPub []byte) []byte {
	h := Hash160(compressedPub)
	return P2PKH(h)
}

// ScriptSigP2PKH builds the unlocking script for a P2PKH input: <sig+hashtype> <pubkey>.
func ScriptSigP2PKH(sigWithType, compressedPub []byte) []byte {
	var b bytes.Buffer
	pushData(&b, sigWithType)
	pushData(&b, compressedPub)
	return b.Bytes()
}
