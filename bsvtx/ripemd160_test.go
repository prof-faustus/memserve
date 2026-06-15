package bsvtx

import "testing"

// Published RIPEMD-160 test vectors (Bosselaers).
func TestRIPEMD160Vectors(t *testing.T) {
	cases := []struct{ in, hex string }{
		{"", "9c1185a5c5e9fc54612808977ee8f548b2258d31"},
		{"a", "0bdc9d2d256b3ee9daae347be6f4dc835a467ffe"},
		{"abc", "8eb208f7e05d987a9b044a8e98c6b087f15a0bfc"},
		{"message digest", "5d0689ef49d2fae572b881b123a85ffa21595f36"},
		{"abcdefghijklmnopqrstuvwxyz", "f71c27109c692c1b56bbdceb5b9d2865b3708dbc"},
	}
	for _, c := range cases {
		got := ripemd160([]byte(c.in))
		if toHex(got[:]) != c.hex {
			t.Fatalf("RIPEMD160(%q) = %s, want %s", c.in, toHex(got[:]), c.hex)
		}
	}
}

func toHex(b []byte) string {
	const h = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, x := range b {
		out[i*2] = h[x>>4]
		out[i*2+1] = h[x&0xf]
	}
	return string(out)
}
