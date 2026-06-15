//go:build aerospike

package aerospike_test

import (
	"os"
	"strconv"
	"testing"
	"time"

	aero "github.com/aerospike/aerospike-client-go/v7"

	"memserve/store"
	aerostore "memserve/store/aerospike"
	"memserve/store/storetest"
)

// TestAerospikeConformance runs the shared store contract against a live Aerospike cluster.
// Configure with AEROSPIKE_HOST / AEROSPIKE_PORT / AEROSPIKE_NAMESPACE; skipped if unset.
//
//	AEROSPIKE_HOST=127.0.0.1 AEROSPIKE_PORT=3000 AEROSPIKE_NAMESPACE=test \
//	  go test -tags aerospike ./store/aerospike/
func TestAerospikeConformance(t *testing.T) {
	host := os.Getenv("AEROSPIKE_HOST")
	if host == "" {
		t.Skip("set AEROSPIKE_HOST/AEROSPIKE_PORT/AEROSPIKE_NAMESPACE to run the Aerospike conformance suite")
	}
	port := 3000
	if p := os.Getenv("AEROSPIKE_PORT"); p != "" {
		port, _ = strconv.Atoi(p)
	}
	ns := os.Getenv("AEROSPIKE_NAMESPACE")
	if ns == "" {
		ns = "test"
	}

	admin, err := aero.NewClient(host, port)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer admin.Close()

	truncate := func() {
		for _, set := range []string{"txindex", "utxo", "subtree", "block", "header", "spentidx"} {
			_ = admin.Truncate(nil, ns, set, nil)
		}
		time.Sleep(50 * time.Millisecond) // let truncation settle
	}

	storetest.RunSuite(t, func() store.Store {
		truncate()
		s, err := aerostore.Open(host, port, ns)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		return s
	})
}
