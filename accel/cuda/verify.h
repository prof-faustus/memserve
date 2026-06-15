/* MemServe GPU secp256k1 batch verify — C ABI (DESIGN.md §13).
 *
 * Verifies a batch of secp256k1 ECDSA signatures on the GPU. All inputs are flat,
 * tightly-packed big-endian byte arrays of length n:
 *   pub33  : n * 33 bytes  (compressed public keys, 0x02/0x03 || X)
 *   hash32 : n * 32 bytes  (message hashes)
 *   sig64  : n * 64 bytes  (R || S, 32 || 32)
 *   results: n bytes out   (1 = valid, 0 = invalid)
 *
 * Returns 0 on success, non-zero on a CUDA error. The low-S (malleability) policy is
 * applied by the Go caller BEFORE dispatch, so this kernel performs the raw EC
 * verification only. Correctness is enforced by accel.Validate against the Go
 * reference; the kernel is not trusted until it passes that gate.
 */
#ifndef MEMSERVE_VERIFY_H
#define MEMSERVE_VERIFY_H

#ifdef __cplusplus
extern "C" {
#endif

int memserve_secp256k1_verify_batch(
    const unsigned char *pub33,
    const unsigned char *hash32,
    const unsigned char *sig64,
    unsigned char *results,
    int n);

#ifdef __cplusplus
}
#endif

#endif /* MEMSERVE_VERIFY_H */
