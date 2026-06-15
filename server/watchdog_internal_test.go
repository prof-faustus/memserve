package server

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"memserve/teranode"
)

// TestIngestWatchdogGatesUnderMemoryLimit is a regression test for the runaway that
// committed ~649 GB: the old watchdog gated on runtime.MemStats.HeapAlloc, which
// collapses to the live set after each GC, so bursts of short-lived ingest garbage
// drove the process's committed footprint past the limit while HeapAlloc still read
// low. The fix gates on HeapInuse+StackInuse (memory actually held from the OS). Here
// a 1 MB ceiling — below the Go runtime's own baseline — must stop ingestion cold.
func TestIngestWatchdogGatesUnderMemoryLimit(t *testing.T) {
	newSrv := func(maxMemMB int) *Server {
		src := teranode.NewMock(teranode.MockConfig{
			Blocks: 1_000_000, SubtreesPer: 2, TxsPerSubtree: 256, SpendFraction: 3,
		})
		s, err := New(Config{MaxMemMB: maxMemMB, PollEvery: time.Millisecond},
			src, slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	runFor := func(s *Server, d time.Duration) uint64 {
		ctx, cancel := context.WithCancel(context.Background())
		go s.ingestLoop(ctx)
		time.Sleep(d)
		cancel()
		return s.met.blocks.Load()
	}

	// A 1 MB limit is below the runtime's baseline held memory, so the watchdog must
	// refuse to ingest essentially anything.
	capped := runFor(newSrv(1), 100*time.Millisecond)
	if capped > 2 {
		t.Fatalf("watchdog did not gate ingestion: ingested %d blocks under a 1MB limit", capped)
	}

	// Sanity: with no limit the very same loop ingests freely — proving it was the gate,
	// not a stalled loop, that held the capped run back.
	free := runFor(newSrv(0), 100*time.Millisecond)
	if free <= capped {
		t.Fatalf("unlimited run ingested %d vs capped %d: loop not actually ingesting", free, capped)
	}
}
