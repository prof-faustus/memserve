package shard

import (
	"testing"

	"memserve/commitment"
)

func TestCountAndOf(t *testing.T) {
	if Count(0) != 1 || Count(1) != 2 || Count(2) != 4 || Count(3) != 8 || Count(10) != 1024 {
		t.Fatal("Count wrong")
	}
	var h commitment.Hash
	h[0] = 0b10110000
	if Of(h, 1) != 1 {
		t.Fatalf("top bit shard = %d", Of(h, 1))
	}
	if Of(h, 2) != 0b10 {
		t.Fatalf("top 2 bits = %b", Of(h, 2))
	}
	if Of(h, 4) != 0b1011 {
		t.Fatalf("top 4 bits = %b", Of(h, 4))
	}
	if Of(h, 0) != 0 {
		t.Fatal("k=0 must be shard 0")
	}
}

func TestPrefixBitsAndSplit(t *testing.T) {
	if PrefixBits(3, 5) != "101" {
		t.Fatalf("PrefixBits(3,5)=%q", PrefixBits(3, 5))
	}
	if PrefixBits(0, 0) != "" {
		t.Fatal("k=0 prefix must be empty")
	}
	lo, hi := Split(3, 5)
	if lo != 10 || hi != 11 {
		t.Fatalf("Split(3,5) = %d,%d want 10,11", lo, hi)
	}
	// child prefixes extend the parent's by one bit.
	if PrefixBits(4, lo) != "1010" || PrefixBits(4, hi) != "1011" {
		t.Fatalf("split prefixes = %q,%q", PrefixBits(4, lo), PrefixBits(4, hi))
	}
}

func TestUniformLoad(t *testing.T) {
	// hash uniformity => roughly even shard occupancy.
	const k = 4
	const n = 200000
	counts := make([]int, Count(k))
	for i := 0; i < n; i++ {
		h := commitment.DoubleSHA256([]byte{byte(i), byte(i >> 8), byte(i >> 16)})
		counts[Of(h, k)]++
	}
	expect := n / len(counts)
	for s, c := range counts {
		if c < expect*8/10 || c > expect*12/10 {
			t.Fatalf("shard %d load %d outside ±20%% of %d", s, c, expect)
		}
	}
}

func TestRouter(t *testing.T) {
	servers := []string{"a", "b", "c", "d"}
	r, err := NewRouter(2, servers)
	if err != nil {
		t.Fatal(err)
	}
	var h commitment.Hash
	h[0] = 0b11000000
	if r.ServerFor(h) != "d" {
		t.Fatalf("server = %s", r.ServerFor(h))
	}
	if _, err := NewRouter(2, servers[:3]); err == nil {
		t.Fatal("want error for wrong server count")
	}
}
