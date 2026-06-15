package mem_test

import (
	"testing"

	"memserve/store"
	"memserve/store/mem"
	"memserve/store/storetest"
)

// The in-memory store must satisfy the full store.Store contract.
func TestMemConformance(t *testing.T) {
	storetest.RunSuite(t, func() store.Store { return mem.New() })
}
