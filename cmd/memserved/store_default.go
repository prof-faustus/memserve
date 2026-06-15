//go:build !aerospike

package main

import (
	"fmt"

	"memserve/store"
)

// openStore (default build): only the in-memory backend is available. Returning a nil
// store makes the server use its built-in in-memory store. Selecting "aerospike" here is
// a clear error telling the operator to rebuild with the tag.
func openStore(kind, host string, port int, ns string) (store.Store, error) {
	switch kind {
	case "", "mem":
		return nil, nil
	case "aerospike":
		return nil, fmt.Errorf("aerospike backend requires building with -tags aerospike")
	default:
		return nil, fmt.Errorf("unknown store backend %q", kind)
	}
}
