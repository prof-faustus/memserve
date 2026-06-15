package accel

import (
	"fmt"

	"memserve/commitment"
	"memserve/crypto"
)

// Validate is the correctness gate for any BatchVerifier (DESIGN.md §13.3). It builds n
// deterministic test cases — a mix of valid signatures, wrong-key signatures, corrupted
// hashes, and high-S (malleated) signatures — and checks the backend agrees with the
// trusted reference on every one. A backend (GPU included) must pass this before it is
// trusted to serve. Returns ErrMismatch (wrapped with the index) on any disagreement.
func Validate(v BatchVerifier, n int) error {
	if n < 8 {
		n = 8
	}
	reqs, want := testVectors(n)
	got := make([]bool, len(reqs))
	v.VerifyBatch(reqs, got)
	for i := range reqs {
		if got[i] != want[i] {
			return fmt.Errorf("%w: case %d (kind shown by index%%4): backend=%v reference=%v",
				ErrMismatch, i, got[i], want[i])
		}
	}
	return nil
}

// testVectors builds n cases cycling through four kinds; want[i] is the true validity.
func testVectors(n int) (reqs []Request, want []bool) {
	reqs = make([]Request, n)
	want = make([]bool, n)
	for i := 0; i < n; i++ {
		seed := commitment.DoubleSHA256([]byte{byte(i), byte(i >> 8), byte(i >> 16), 0x5a})
		priv, _ := crypto.NewPrivateKey(seed[:])
		msg := commitment.DoubleSHA256([]byte{byte(i), 0xa5})
		sig, _ := priv.Sign(msg[:])
		switch i % 4 {
		case 0: // valid
			reqs[i] = Request{Pub: priv.Public(), Hash: msg[:], Sig: sig}
			want[i] = true
		case 1: // wrong key
			otherSeed := commitment.DoubleSHA256([]byte{byte(i), 0xff})
			other, _ := crypto.NewPrivateKey(otherSeed[:])
			reqs[i] = Request{Pub: other.Public(), Hash: msg[:], Sig: sig}
			want[i] = false
		case 2: // corrupted message
			bad := commitment.DoubleSHA256([]byte{byte(i), 0x01})
			reqs[i] = Request{Pub: priv.Public(), Hash: bad[:], Sig: sig}
			want[i] = false
		case 3: // high-S (malleated): raw ECDSA verifies, but must be rejected as non-canonical
			mall, err := crypto.Malleate(sig.Serialize())
			if err == nil {
				hs, _ := crypto.ParseSignature(mall)
				reqs[i] = Request{Pub: priv.Public(), Hash: msg[:], Sig: hs}
				want[i] = hs.IsLowS() && crypto.Verify(priv.Public(), msg[:], hs)
			} else {
				reqs[i] = Request{Pub: priv.Public(), Hash: msg[:], Sig: sig}
				want[i] = true
			}
		}
	}
	return reqs, want
}

// MakeBatch builds m valid Requests for benchmarking a backend's throughput.
func MakeBatch(m int) []Request {
	reqs := make([]Request, m)
	for i := 0; i < m; i++ {
		seed := commitment.DoubleSHA256([]byte{byte(i), byte(i >> 8), byte(i >> 16), 0x11})
		priv, _ := crypto.NewPrivateKey(seed[:])
		msg := commitment.DoubleSHA256([]byte{byte(i), byte(i >> 8), 0x22})
		sig, _ := priv.Sign(msg[:])
		reqs[i] = Request{Pub: priv.Public(), Hash: msg[:], Sig: sig}
	}
	return reqs
}
