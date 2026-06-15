package accel

import (
	"testing"
	"time"
)

func TestReferencePassesValidate(t *testing.T) {
	if err := Validate(Reference{}, 64); err != nil {
		t.Fatalf("reference failed its own validator: %v", err)
	}
}

func TestCPUPassesValidate(t *testing.T) {
	if err := Validate(NewCPU(), 256); err != nil {
		t.Fatalf("CPU backend failed validator: %v", err)
	}
}

func TestCPUAgreesWithReferenceOnBatch(t *testing.T) {
	reqs, want := testVectors(200)
	got := make([]bool, len(reqs))
	NewCPU().VerifyBatch(reqs, got)
	for i := range reqs {
		if got[i] != want[i] {
			t.Fatalf("case %d: cpu=%v want=%v", i, got[i], want[i])
		}
	}
}

// a deliberately broken backend must be CAUGHT by Validate (the gate works).
type brokenBackend struct{}

func (brokenBackend) Name() string { return "broken" }
func (brokenBackend) VerifyBatch(reqs []Request, out []bool) {
	for i := range reqs {
		out[i] = true // claims everything is valid — wrong
	}
}

func TestValidatorCatchesBrokenBackend(t *testing.T) {
	if err := Validate(brokenBackend{}, 64); err == nil {
		t.Fatal("validator did not catch a backend that accepts everything")
	}
}

func TestGate(t *testing.T) {
	reqs, want := testVectors(50)
	g := NewGate(NewCPU(), 8, 5*time.Millisecond)
	results := make([]<-chan bool, len(reqs))
	for i := range reqs {
		results[i] = g.Submit(reqs[i])
	}
	for i := range reqs {
		select {
		case got := <-results[i]:
			if got != want[i] {
				t.Fatalf("gate case %d: got=%v want=%v", i, got, want[i])
			}
		case <-time.After(time.Second):
			t.Fatalf("gate case %d timed out", i)
		}
	}
}

func TestThroughputPositive(t *testing.T) {
	batch := MakeBatch(128)
	if r := Throughput(NewCPU(), batch, 80*time.Millisecond); r <= 0 {
		t.Fatalf("cpu throughput non-positive: %v", r)
	}
}
