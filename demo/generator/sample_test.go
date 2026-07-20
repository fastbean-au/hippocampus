package main

import (
	"context"
	"testing"
	"time"

	"github.com/fastbean-au/hippocampus/contract"
)

func TestSignificanceBandKey(t *testing.T) {
	want := []string{"sig_1_25", "sig_26_50", "sig_51_75", "sig_76_100"}

	for i, w := range want {
		if got := significanceBandKey(i); got != w {
			t.Errorf("significanceBandKey(%d) = %q, want %q", i, got, w)
		}
	}

	// Anything >= 3 (including out of range) falls into the default branch.
	if got := significanceBandKey(99); got != "sig_76_100" {
		t.Errorf("significanceBandKey(99) = %q, want sig_76_100", got)
	}
}

func TestCountMemoriesAndEvents(t *testing.T) {
	client := newFakeHippoClient()
	g := New(Config{Seed: 1}, client, newLatencyTracker())

	// Empty store.
	if got := g.countMemories(context.Background(), 0, 0); got != 0 {
		t.Errorf("countMemories(empty) = %d, want 0", got)
	}

	if got := g.countEvents(context.Background()); got != 0 {
		t.Errorf("countEvents(empty) = %d, want 0", got)
	}

	client.memories["m1"] = &contract.Memory{Id: "m1", Significance: 10}
	client.memories["m2"] = &contract.Memory{Id: "m2", Significance: 60}
	client.events["e1"] = &contract.Event{Id: "e1"}

	if got := g.countMemories(context.Background(), 0, 0); got != 2 {
		t.Errorf("countMemories(all) = %d, want 2", got)
	}

	if got := g.countMemories(context.Background(), 1, 25); got != 2 {
		// The fake ignores band filtering and returns everything, mirroring only the "band set"
		// code path being exercised (sigMin/sigMax > 0).
		t.Errorf("countMemories(band) = %d, want 2 (fake returns all)", got)
	}

	if got := g.countEvents(context.Background()); got != 1 {
		t.Errorf("countEvents() = %d, want 1", got)
	}
}

func TestCountMemoriesAndEventsError(t *testing.T) {
	client := newFakeHippoClient()
	client.errOn["GetMemories"] = 1
	client.errOn["GetEvents"] = 1

	g := New(Config{Seed: 1}, client, newLatencyTracker())

	if got := g.countMemories(context.Background(), 0, 0); got != 0 {
		t.Errorf("countMemories() on error = %d, want 0", got)
	}

	if got := g.countEvents(context.Background()); got != 0 {
		t.Errorf("countEvents() on error = %d, want 0", got)
	}
}

func TestSample(t *testing.T) {
	client := newFakeHippoClient()
	client.memories["m1"] = &contract.Memory{Id: "m1", Significance: 10}
	client.events["e1"] = &contract.Event{Id: "e1"}

	g := New(Config{Seed: 1}, client, newLatencyTracker())

	// Just exercise for panics/logic; the log output isn't asserted.
	g.sample(context.Background())
}

func TestSampleEmptyStore(t *testing.T) {
	client := newFakeHippoClient()
	g := New(Config{Seed: 1}, client, newLatencyTracker())

	// memTotal and evtTotal both 0: the mem_per_evt and _pct fields are skipped (divide-by-zero
	// guards).
	g.sample(context.Background())
}

func TestSampleLoop(t *testing.T) {
	client := newFakeHippoClient()
	client.memories["m1"] = &contract.Memory{Id: "m1", Significance: 10}

	g := New(Config{Seed: 1, SampleInterval: 5 * time.Millisecond}, client, newLatencyTracker())

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		g.sampleLoop(ctx)
		close(done)
	}()

	select {

	case <-done:

	case <-time.After(2 * time.Second):
		t.Fatal("sampleLoop() did not return after context cancellation")

	}

	if client.callCount("GetMemories") == 0 {
		t.Error("expected sampleLoop() to have ticked at least once")
	}
}
