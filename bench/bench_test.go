package bench

import (
	"testing"
	"time"

	"memserve/teranode"
)

func TestBuildAndQuery(t *testing.T) {
	p, err := Build(teranode.MockConfig{Blocks: 2, SubtreesPer: 2, TxsPerSubtree: 32, SpendFraction: 2}, 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.TxIDs) == 0 || len(p.Outpts) == 0 {
		t.Fatal("no sample keys")
	}
	// a sampled tx must be served and its proof must verify.
	id := p.TxIDs[0]
	if r, _ := p.Server.Seen(id); !r.Seen {
		t.Fatal("sample tx not seen")
	}
	pr, err := p.Server.MerklePath(id)
	if err != nil || !pr.Verify() {
		t.Fatalf("sample proof verify=%v err=%v", pr.Verify(), err)
	}
}

func TestThroughputPositive(t *testing.T) {
	p, err := Build(teranode.MockConfig{Blocks: 2, SubtreesPer: 2, TxsPerSubtree: 64}, 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []QueryKind{KSeen, KMined, KUTXO} {
		if r := p.Throughput(k, 2, 80*time.Millisecond); r <= 0 {
			t.Fatalf("%s throughput non-positive: %v", k, r)
		}
	}
	if r := p.PaidThroughput(2, 80*time.Millisecond); r <= 0 {
		t.Fatalf("paid throughput non-positive: %v", r)
	}
}
