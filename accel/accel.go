// Package accel is MemServe's batch signature-verification accelerator (DESIGN.md
// §13). Per-access payment metering produces a stream of independent secp256k1 ECDSA
// verifications — the paid-access ceiling — and these are embarrassingly parallel. A
// BatchVerifier verifies a whole batch at once; the default CPU backend fans it across
// cores, and a CUDA backend (build tag `cuda`) drops in unchanged.
//
// Verification is over PUBLIC data, so a backend need not be constant-time. Every
// backend is checked against the single-signature reference by Validate (the
// correctness gate), so a wrong implementation — GPU included — can never silently
// serve. BSV only — secp256k1.
package accel

import (
	"errors"

	"memserve/crypto"
)

// Request is one signature to verify: pubkey, 32-byte message hash, signature.
type Request struct {
	Pub  *crypto.PublicKey
	Hash []byte
	Sig  *crypto.Signature
}

// BatchVerifier verifies a batch of requests, writing out[i] = valid(reqs[i]).
// len(out) must be >= len(reqs). Implementations must be safe for concurrent use.
type BatchVerifier interface {
	VerifyBatch(reqs []Request, out []bool)
	Name() string
}

// ErrMismatch reports a backend disagreeing with the reference (from Validate).
var ErrMismatch = errors.New("accel: backend disagrees with reference verifier")

// reference is the trusted single-signature verifier (also enforces low-S, matching
// the channel's malleability rule).
func reference(r Request) bool {
	if r.Sig == nil || !r.Sig.IsLowS() {
		return false
	}
	return crypto.Verify(r.Pub, r.Hash, r.Sig)
}

// Reference is a BatchVerifier that runs the trusted per-signature verifier serially.
// It is the correctness oracle and a baseline for the benchmark.
type Reference struct{}

func (Reference) Name() string { return "reference-serial" }

func (Reference) VerifyBatch(reqs []Request, out []bool) {
	for i := range reqs {
		out[i] = reference(reqs[i])
	}
}

var _ BatchVerifier = Reference{}
