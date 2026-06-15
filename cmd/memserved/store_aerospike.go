//go:build aerospike

package main

import (
	"fmt"

	"memserve/store"
	aerostore "memserve/store/aerospike"
)

// openStore (aerospike build): "aerospike" connects to a cluster; "mem" still uses the
// built-in in-memory store (nil). Build with: go build -tags aerospike ./cmd/memserved
func openStore(kind, host string, port int, ns string) (store.Store, error) {
	switch kind {
	case "", "mem":
		return nil, nil
	case "aerospike":
		return aerostore.Open(host, port, ns)
	default:
		return nil, fmt.Errorf("unknown store backend %q", kind)
	}
}
