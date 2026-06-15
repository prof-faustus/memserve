// MemServe GPU secp256k1 batch ECDSA verification (DESIGN.md §13).
//
// HONEST STATUS: this kernel is a FAITHFUL translation of the repository's proven Go
// reference (crypto/ct.go field/point arithmetic + crypto/secp256k1.go verify) into
// CUDA C. It requires `nvcc` + an NVIDIA GPU to build and run, and it is NOT
// hardware-validated in this repository. It MUST pass accel.Validate (the differential
// gate against the Go reference) before it is trusted to serve. The Go wrapper applies
// the low-S policy before dispatch, so this code does the raw EC verification only.
//
// mod-p arithmetic uses the fast secp256k1 reduction (2^256 ≡ 2^32+977). mod-n
// arithmetic uses a simple binary (double-and-add) mulmod + Fermat inverse — chosen for
// obvious correctness over speed; optimize once it passes the validator. Verification
// is over public data, so no constant-time requirement applies.
//
// Build (example):
//   nvcc -O3 -shared -Xcompiler -fPIC -o libmemserve_gpu.so verify.cu
// then build Go with: go build -tags cuda ./...

#include "verify.h"
#include <cuda_runtime.h>

typedef unsigned long long u64;
typedef unsigned char u8;

__device__ __forceinline__ u64 addc(u64 a, u64 b, u64 cin, u64 *cout) {
    u64 s = a + b;
    u64 c1 = (s < a);
    u64 s2 = s + cin;
    u64 c2 = (s2 < s);
    *cout = c1 + c2;
    return s2;
}
__device__ __forceinline__ u64 subb(u64 a, u64 b, u64 bin, u64 *bout) {
    u64 d = a - b;
    u64 b1 = (a < b);
    u64 d2 = d - bin;
    u64 b2 = (d < bin);
    *bout = b1 + b2;
    return d2;
}

// ---------------------------------------------------------------------------
// GF(p) field arithmetic — fe is u64[4], little-endian. p = 2^256 - 2^32 - 977.
// ---------------------------------------------------------------------------
__device__ __constant__ u64 PL[4]  = {0xFFFFFFFEFFFFFC2FULL, 0xFFFFFFFFFFFFFFFFULL, 0xFFFFFFFFFFFFFFFFULL, 0xFFFFFFFFFFFFFFFFULL};
__device__ __constant__ u64 PM2[4] = {0xFFFFFFFEFFFFFC2DULL, 0xFFFFFFFFFFFFFFFFULL, 0xFFFFFFFFFFFFFFFFULL, 0xFFFFFFFFFFFFFFFFULL}; // p-2
// (p+1)/4, the secp256k1 square-root exponent.
__device__ __constant__ u64 PSQRT[4] = {0xFFFFFFFFBFFFFF0CULL, 0xFFFFFFFFFFFFFFFFULL, 0xFFFFFFFFFFFFFFFFULL, 0x3FFFFFFFFFFFFFFFULL};
#define FEC 0x1000003D1ULL

__device__ void fe_cond_sub_p(u64 *a) {
    u64 r[4], b = 0;
    r[0] = subb(a[0], PL[0], 0, &b);
    r[1] = subb(a[1], PL[1], b, &b);
    r[2] = subb(a[2], PL[2], b, &b);
    r[3] = subb(a[3], PL[3], b, &b);
    u64 mask = b - 1; // b==0 (a>=p) -> all ones (take r); b==1 (a<p) -> 0 (keep a)
    a[0] = (r[0] & mask) | (a[0] & ~mask);
    a[1] = (r[1] & mask) | (a[1] & ~mask);
    a[2] = (r[2] & mask) | (a[2] & ~mask);
    a[3] = (r[3] & mask) | (a[3] & ~mask);
}

__device__ void fe_add(const u64 *a, const u64 *b, u64 *r) {
    u64 c = 0;
    r[0] = addc(a[0], b[0], 0, &c);
    r[1] = addc(a[1], b[1], c, &c);
    r[2] = addc(a[2], b[2], c, &c);
    r[3] = addc(a[3], b[3], c, &c);
    r[0] = addc(r[0], c * FEC, 0, &c);
    r[1] = addc(r[1], 0, c, &c);
    r[2] = addc(r[2], 0, c, &c);
    r[3] = addc(r[3], 0, c, &c);
    r[0] = addc(r[0], c * FEC, 0, &c);
    r[1] = addc(r[1], 0, c, &c);
    r[2] = addc(r[2], 0, c, &c);
    r[3] = addc(r[3], 0, c, &c);
    fe_cond_sub_p(r);
}

__device__ void fe_sub(const u64 *a, const u64 *b, u64 *r) {
    u64 br = 0;
    r[0] = subb(a[0], b[0], 0, &br);
    r[1] = subb(a[1], b[1], br, &br);
    r[2] = subb(a[2], b[2], br, &br);
    r[3] = subb(a[3], b[3], br, &br);
    u64 mask = 0ULL - br, c = 0;
    r[0] = addc(r[0], PL[0] & mask, 0, &c);
    r[1] = addc(r[1], PL[1] & mask, c, &c);
    r[2] = addc(r[2], PL[2] & mask, c, &c);
    r[3] = addc(r[3], PL[3] & mask, c, &c);
}

__device__ void fe_mul(const u64 *a, const u64 *b, u64 *r) {
    u64 t[8];
    for (int i = 0; i < 8; i++) t[i] = 0;
    for (int i = 0; i < 4; i++) {
        u64 carry = 0;
        for (int j = 0; j < 4; j++) {
            u64 hi = __umul64hi(a[i], b[j]);
            u64 lo = a[i] * b[j];
            u64 c1, c2;
            u64 s = addc(t[i + j], lo, 0, &c1);
            s = addc(s, carry, 0, &c2);
            t[i + j] = s;
            carry = hi + c1 + c2;
        }
        t[i + 4] += carry;
    }
    u64 hi[4] = {t[4], t[5], t[6], t[7]};
    u64 m[5];
    {
        u64 carry = 0;
        for (int i = 0; i < 4; i++) {
            u64 h = __umul64hi(hi[i], FEC);
            u64 l = hi[i] * FEC;
            u64 c;
            u64 s = addc(l, carry, 0, &c);
            m[i] = s;
            carry = h + c;
        }
        m[4] = carry;
    }
    u64 c = 0;
    r[0] = addc(t[0], m[0], 0, &c);
    r[1] = addc(t[1], m[1], c, &c);
    r[2] = addc(t[2], m[2], c, &c);
    r[3] = addc(t[3], m[3], c, &c);
    u64 r4 = m[4] + c;
    u64 h4 = __umul64hi(r4, FEC), l4 = r4 * FEC;
    c = 0;
    r[0] = addc(r[0], l4, 0, &c);
    r[1] = addc(r[1], h4, c, &c);
    r[2] = addc(r[2], 0, c, &c);
    r[3] = addc(r[3], 0, c, &c);
    r[0] = addc(r[0], c * FEC, 0, &c);
    r[1] = addc(r[1], 0, c, &c);
    r[2] = addc(r[2], 0, c, &c);
    r[3] = addc(r[3], 0, c, &c);
    fe_cond_sub_p(r);
}

__device__ void fe_set(u64 *r, u64 v) { r[0] = v; r[1] = 0; r[2] = 0; r[3] = 0; }
__device__ void fe_copy(const u64 *a, u64 *r) { r[0]=a[0]; r[1]=a[1]; r[2]=a[2]; r[3]=a[3]; }
__device__ int fe_is_zero(const u64 *a) { return (a[0]|a[1]|a[2]|a[3]) == 0; }
__device__ int fe_eq(const u64 *a, const u64 *b) { return a[0]==b[0]&&a[1]==b[1]&&a[2]==b[2]&&a[3]==b[3]; }

__device__ void fe_pow(const u64 *base, const u64 *exp, u64 *r) {
    u64 res[4] = {1,0,0,0}, b[4], t[4];
    fe_copy(base, b);
    for (int w = 3; w >= 0; w--)
        for (int bit = 63; bit >= 0; bit--) {
            fe_mul(res, res, t); fe_copy(t, res);
            if ((exp[w] >> bit) & 1) { fe_mul(res, b, t); fe_copy(t, res); }
        }
    fe_copy(res, r);
}
__device__ void fe_inv(const u64 *a, u64 *r) { fe_pow(a, PM2, r); }

// ---------------------------------------------------------------------------
// Jacobian/complete point arithmetic (a=0, Renes-Costello-Batina 2016 Alg.7).
// ---------------------------------------------------------------------------
struct jac { u64 X[4], Y[4], Z[4]; };
__device__ __constant__ u64 FEB3[4] = {21,0,0,0}; // 3*b, b=7

__device__ void point_add(const jac *p, const jac *q, jac *out) {
    u64 t0[4],t1[4],t2[4],t3[4],t4[4],X3[4],Y3[4],Z3[4];
    fe_mul(p->X,q->X,t0);
    fe_mul(p->Y,q->Y,t1);
    fe_mul(p->Z,q->Z,t2);
    fe_add(p->X,p->Y,t3);
    fe_add(q->X,q->Y,t4);
    fe_mul(t3,t4,t3);
    fe_add(t0,t1,t4);
    fe_sub(t3,t4,t3);
    fe_add(p->Y,p->Z,t4);
    fe_add(q->Y,q->Z,X3);
    fe_mul(t4,X3,t4);
    fe_add(t1,t2,X3);
    fe_sub(t4,X3,t4);
    fe_add(p->X,p->Z,X3);
    fe_add(q->X,q->Z,Y3);
    fe_mul(X3,Y3,X3);
    fe_add(t0,t2,Y3);
    fe_sub(X3,Y3,Y3);
    fe_add(t0,t0,X3);
    fe_add(X3,t0,t0);
    fe_mul(FEB3,t2,t2);
    fe_add(t1,t2,Z3);
    fe_sub(t1,t2,t1);
    fe_mul(FEB3,Y3,Y3);
    fe_mul(t4,Y3,X3);
    fe_mul(t3,t1,t2);
    fe_sub(t2,X3,X3);
    fe_mul(Y3,t0,Y3);
    fe_mul(t1,Z3,t1);
    fe_add(t1,Y3,Y3);
    fe_mul(t0,t3,t0);
    fe_mul(Z3,t4,Z3);
    fe_add(Z3,t0,Z3);
    fe_copy(X3,out->X); fe_copy(Y3,out->Y); fe_copy(Z3,out->Z);
}

__device__ void set_identity(jac *r) { fe_set(r->X,0); fe_set(r->Y,1); fe_set(r->Z,0); }

// scalar_mult: double-and-add over 256 bits (public scalar, no constant-time need).
__device__ void scalar_mult(const u64 *k, const jac *P, jac *R) {
    jac acc; set_identity(&acc);
    for (int w = 3; w >= 0; w--)
        for (int bit = 63; bit >= 0; bit--) {
            jac d; point_add(&acc, &acc, &d); acc = d;
            if ((k[w] >> bit) & 1) { jac s; point_add(&acc, P, &s); acc = s; }
        }
    *R = acc;
}

__device__ void affine_x(const jac *P, u64 *x) {
    u64 zi[4]; fe_inv(P->Z, zi); fe_mul(P->X, zi, x);
}

// ---------------------------------------------------------------------------
// Scalar (mod n) arithmetic — sc is u64[4], little-endian.
// ---------------------------------------------------------------------------
__device__ __constant__ u64 NL[4]  = {0xBFD25E8CD0364141ULL, 0xBAAEDCE6AF48A03BULL, 0xFFFFFFFFFFFFFFFEULL, 0xFFFFFFFFFFFFFFFFULL};
__device__ __constant__ u64 NM2[4] = {0xBFD25E8CD036413FULL, 0xBAAEDCE6AF48A03BULL, 0xFFFFFFFFFFFFFFFEULL, 0xFFFFFFFFFFFFFFFFULL}; // n-2

__device__ int sc_geq(const u64 *a, const u64 *b) {
    for (int i = 3; i >= 0; i--) { if (a[i] != b[i]) return a[i] > b[i]; }
    return 1;
}
__device__ int sc_is_zero(const u64 *a) { return (a[0]|a[1]|a[2]|a[3]) == 0; }
__device__ int sc_eq(const u64 *a, const u64 *b) { return a[0]==b[0]&&a[1]==b[1]&&a[2]==b[2]&&a[3]==b[3]; }

__device__ void sc_sub_n(u64 *a) { // a -= n (mod 2^256, discard borrow)
    u64 b = 0;
    a[0] = subb(a[0], NL[0], 0, &b);
    a[1] = subb(a[1], NL[1], b, &b);
    a[2] = subb(a[2], NL[2], b, &b);
    a[3] = subb(a[3], NL[3], b, &b);
}

__device__ void add_mod_n(const u64 *a, const u64 *b, u64 *r) {
    u64 c = 0;
    r[0] = addc(a[0], b[0], 0, &c);
    r[1] = addc(a[1], b[1], c, &c);
    r[2] = addc(a[2], b[2], c, &c);
    r[3] = addc(a[3], b[3], c, &c);
    if (c || sc_geq(r, NL)) sc_sub_n(r);
}

// mulmod_n: binary double-and-add (obvious correctness). a,b assumed < n.
__device__ void mulmod_n(const u64 *a, const u64 *b, u64 *r) {
    u64 acc[4] = {0,0,0,0}, bb[4];
    bb[0]=b[0]; bb[1]=b[1]; bb[2]=b[2]; bb[3]=b[3];
    for (int w = 3; w >= 0; w--)
        for (int bit = 63; bit >= 0; bit--) {
            u64 t[4]; add_mod_n(acc, acc, t); // acc *= 2
            acc[0]=t[0]; acc[1]=t[1]; acc[2]=t[2]; acc[3]=t[3];
            if ((a[w] >> bit) & 1) { add_mod_n(acc, bb, t); acc[0]=t[0];acc[1]=t[1];acc[2]=t[2];acc[3]=t[3]; }
        }
    r[0]=acc[0]; r[1]=acc[1]; r[2]=acc[2]; r[3]=acc[3];
}

__device__ void powmod_n(const u64 *base, const u64 *exp, u64 *r) {
    u64 res[4] = {1,0,0,0}, b[4], t[4];
    b[0]=base[0]; b[1]=base[1]; b[2]=base[2]; b[3]=base[3];
    for (int w = 3; w >= 0; w--)
        for (int bit = 63; bit >= 0; bit--) {
            mulmod_n(res, res, t); res[0]=t[0];res[1]=t[1];res[2]=t[2];res[3]=t[3];
            if ((exp[w] >> bit) & 1) { mulmod_n(res, b, t); res[0]=t[0];res[1]=t[1];res[2]=t[2];res[3]=t[3]; }
        }
    r[0]=res[0]; r[1]=res[1]; r[2]=res[2]; r[3]=res[3];
}
__device__ void inv_n(const u64 *a, u64 *r) { powmod_n(a, NM2, r); }

// ---------------------------------------------------------------------------
// Byte parsing (big-endian 32-byte -> u64[4] little-endian limbs).
// ---------------------------------------------------------------------------
__device__ void be32_to_limbs(const u8 *b, u64 *out) {
    for (int limb = 0; limb < 4; limb++) {
        u64 v = 0;
        const u8 *p = b + (3 - limb) * 8;
        for (int i = 0; i < 8; i++) v = (v << 8) | p[i];
        out[limb] = v;
    }
}

// generator G (compressed-free), little-endian fe limbs.
__device__ __constant__ u64 GX[4] = {0x59F2815B16F81798ULL, 0x029BFCDB2DCE28D9ULL, 0x55A06295CE870B07ULL, 0x79BE667EF9DCBBACULL};
__device__ __constant__ u64 GY[4] = {0x9C47D08FFB10D4B8ULL, 0xFD17B448A6855419ULL, 0x5DA4FBFC0E1108A8ULL, 0x483ADA7726A3C465ULL};

// decompress a 33-byte pubkey into Q; returns 0 if invalid.
__device__ int decompress(const u8 *pub, jac *Q) {
    if (pub[0] != 0x02 && pub[0] != 0x03) return 0;
    u64 x[4]; be32_to_limbs(pub + 1, x);
    if (sc_geq(x, PL)) return 0; // x must be < p
    u64 x2[4], x3[4], seven[4], rhs[4], y[4], chk[4];
    fe_mul(x, x, x2);
    fe_mul(x2, x, x3);
    fe_set(seven, 7);
    fe_add(x3, seven, rhs);
    fe_pow(rhs, PSQRT, y);
    fe_mul(y, y, chk);
    if (!fe_eq(chk, rhs)) return 0; // not a square => not on curve
    int wantOdd = (pub[0] == 0x03);
    if ((int)(y[0] & 1) != wantOdd) { u64 z[4]; fe_set(z, 0); fe_sub(z, y, y); }
    fe_copy(x, Q->X); fe_copy(y, Q->Y); fe_set(Q->Z, 1);
    return 1;
}

__device__ int verify_one(const u8 *pub, const u8 *hash, const u8 *sig) {
    u64 r[4], s[4];
    be32_to_limbs(sig, r);
    be32_to_limbs(sig + 32, s);
    if (sc_is_zero(r) || sc_geq(r, NL)) return 0;
    if (sc_is_zero(s) || sc_geq(s, NL)) return 0;

    jac Q;
    if (!decompress(pub, &Q)) return 0;

    u64 z[4];
    be32_to_limbs(hash, z);
    if (sc_geq(z, NL)) sc_sub_n(z); // reduce hash mod n (one subtraction suffices)

    u64 w[4], u1[4], u2[4];
    inv_n(s, w);
    mulmod_n(z, w, u1);
    mulmod_n(r, w, u2);

    jac G; fe_copy(GX, G.X); fe_copy(GY, G.Y); fe_set(G.Z, 1);
    jac R1, R2, R;
    scalar_mult(u1, &G, &R1);
    scalar_mult(u2, &Q, &R2);
    point_add(&R1, &R2, &R);
    if (fe_is_zero(R.Z)) return 0; // point at infinity

    u64 x[4];
    affine_x(&R, x);
    if (sc_geq(x, NL)) sc_sub_n(x); // x mod n
    return sc_eq(x, r);
}

__global__ void verify_kernel(const u8 *pub, const u8 *hash, const u8 *sig, u8 *res, int n) {
    int i = blockIdx.x * blockDim.x + threadIdx.x;
    if (i >= n) return;
    res[i] = (u8)verify_one(pub + i * 33, hash + i * 32, sig + i * 64);
}

extern "C" int memserve_secp256k1_verify_batch(
    const unsigned char *pub33, const unsigned char *hash32,
    const unsigned char *sig64, unsigned char *results, int n) {
    if (n <= 0) return 0;
    u8 *dPub = 0, *dHash = 0, *dSig = 0, *dRes = 0;
    cudaError_t e;
    if ((e = cudaMalloc(&dPub, (size_t)n * 33)) != cudaSuccess) return (int)e;
    if ((e = cudaMalloc(&dHash, (size_t)n * 32)) != cudaSuccess) { cudaFree(dPub); return (int)e; }
    if ((e = cudaMalloc(&dSig, (size_t)n * 64)) != cudaSuccess) { cudaFree(dPub); cudaFree(dHash); return (int)e; }
    if ((e = cudaMalloc(&dRes, (size_t)n)) != cudaSuccess) { cudaFree(dPub); cudaFree(dHash); cudaFree(dSig); return (int)e; }

    cudaMemcpy(dPub, pub33, (size_t)n * 33, cudaMemcpyHostToDevice);
    cudaMemcpy(dHash, hash32, (size_t)n * 32, cudaMemcpyHostToDevice);
    cudaMemcpy(dSig, sig64, (size_t)n * 64, cudaMemcpyHostToDevice);

    int threads = 128;
    int blocks = (n + threads - 1) / threads;
    verify_kernel<<<blocks, threads>>>(dPub, dHash, dSig, dRes, n);
    e = cudaDeviceSynchronize();
    if (e == cudaSuccess)
        cudaMemcpy(results, dRes, (size_t)n, cudaMemcpyDeviceToHost);

    cudaFree(dPub); cudaFree(dHash); cudaFree(dSig); cudaFree(dRes);
    return (int)e;
}
