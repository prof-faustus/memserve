// Package shard implements MemServe's hash-prefix sharding (DESIGN.md §6). A txid
// is a uniformly-distributed SHA-256d hash, so its leading k bits partition the key
// space into 2^k equal shards: 1 bit -> {0…,1…}, 2 bits -> {00,01,10,11}, … k bits
// -> 2^k. Load is uniform by hash uniformity; routing is stateless; adding capacity
// is raising k by 1 (each shard splits cleanly into its …0 and …1 halves).
//
// Shards are numbered 0..2^k-1 by the integer value of the top k bits (big-endian).
// k <= 32 (the first four bytes carry the prefix), which is far more than enough:
// k=32 is 4.3 billion shards. BSV only.
package shard

import (
	"encoding/binary"
	"fmt"

	"memserve/commitment"
)

// MaxK is the largest supported prefix width (top 32 bits of the hash).
const MaxK = 32

// Count returns the number of shards for a k-bit prefix: 2^k.
func Count(k uint) uint32 {
	if k == 0 {
		return 1
	}
	return uint32(1) << k
}

// Of returns the shard id (0..2^k-1) that owns h: the integer value of the top k
// bits of the hash, big-endian. k=0 maps everything to shard 0.
func Of(h commitment.Hash, k uint) uint32 {
	if k == 0 {
		return 0
	}
	if k > MaxK {
		k = MaxK
	}
	v := binary.BigEndian.Uint32(h[:4])
	return v >> (32 - k)
}

// Owns reports whether shard `id` (at width k) owns hash h.
func Owns(h commitment.Hash, k uint, id uint32) bool {
	return Of(h, k) == id
}

// PrefixBits returns the binary-string prefix that names shard id at width k,
// e.g. (k=3, id=5) -> "101". This is the contiguous range the shard owns.
func PrefixBits(k uint, id uint32) string {
	if k == 0 {
		return ""
	}
	out := make([]byte, k)
	for i := uint(0); i < k; i++ {
		bit := (id >> (k - 1 - i)) & 1
		out[i] = byte('0' + bit)
	}
	return string(out)
}

// Split returns the two child shard ids at width k+1 that a shard id at width k
// divides into (its …0 and …1 halves) — the elastic-split operation.
func Split(k uint, id uint32) (lo, hi uint32) {
	return id << 1, (id << 1) | 1
}

// Router maps a hash to the address of the MemServe server that owns its shard.
// Stateless: it is just the width k and the per-shard server list.
type Router struct {
	K       uint
	Servers []string // len must be Count(K); Servers[id] serves shard id
}

// NewRouter validates and builds a router. len(servers) must equal 2^k.
func NewRouter(k uint, servers []string) (*Router, error) {
	if k > MaxK {
		return nil, fmt.Errorf("shard: k=%d exceeds MaxK=%d", k, MaxK)
	}
	if uint32(len(servers)) != Count(k) {
		return nil, fmt.Errorf("shard: need %d servers for k=%d, got %d", Count(k), k, len(servers))
	}
	return &Router{K: k, Servers: append([]string(nil), servers...)}, nil
}

// ShardFor returns the shard id owning h.
func (r *Router) ShardFor(h commitment.Hash) uint32 { return Of(h, r.K) }

// ServerFor returns the server address owning h.
func (r *Router) ServerFor(h commitment.Hash) string { return r.Servers[Of(h, r.K)] }
