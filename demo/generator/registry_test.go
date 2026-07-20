package main

import (
	"math/rand"
	"testing"
)

func TestRegistryAddAndRandomEventID(t *testing.T) {
	r := newRegistry(10, 10)

	if _, ok := r.randomEventID(rand.New(rand.NewSource(1))); ok {
		t.Error("randomEventID() on empty registry should return false")
	}

	r.addEventID("")
	if len(r.eventIDs) != 0 {
		t.Error("addEventID(\"\") should be a no-op")
	}

	r.addEventID("evt-1")

	id, ok := r.randomEventID(rand.New(rand.NewSource(1)))
	if !ok || id != "evt-1" {
		t.Errorf("randomEventID() = (%q, %v), want (evt-1, true)", id, ok)
	}
}

func TestRegistryEventTrim(t *testing.T) {
	r := newRegistry(3, 10)

	for i := 0; i < 10; i++ {
		r.addEventID(string(rune('a' + i)))
	}

	if len(r.eventIDs) != 3 {
		t.Fatalf("len(eventIDs) = %d, want 3", len(r.eventIDs))
	}

	// The most recently added ids should survive (oldest dropped from the front).
	want := []string{"h", "i", "j"}
	for i, id := range want {
		if r.eventIDs[i] != id {
			t.Errorf("eventIDs[%d] = %q, want %q", i, r.eventIDs[i], id)
		}
	}
}

func TestRegistryAddMemoryIDsEmpty(t *testing.T) {
	r := newRegistry(10, 10)

	r.addMemoryIDs(nil)
	if len(r.memoryIDs) != 0 {
		t.Error("addMemoryIDs(nil) should be a no-op")
	}
}

func TestRegistryMemoryTrim(t *testing.T) {
	r := newRegistry(10, 3)

	r.addMemoryIDs([]string{"a", "b", "c", "d", "e"})

	if len(r.memoryIDs) != 3 {
		t.Fatalf("len(memoryIDs) = %d, want 3", len(r.memoryIDs))
	}

	want := []string{"c", "d", "e"}
	for i, id := range want {
		if r.memoryIDs[i] != id {
			t.Errorf("memoryIDs[%d] = %q, want %q", i, r.memoryIDs[i], id)
		}
	}
}

func TestTwoRandomEventIDs(t *testing.T) {
	r := newRegistry(10, 10)

	if _, _, ok := r.twoRandomEventIDs(rand.New(rand.NewSource(1))); ok {
		t.Error("twoRandomEventIDs() with < 2 ids should return false")
	}

	r.addEventID("only")
	if _, _, ok := r.twoRandomEventIDs(rand.New(rand.NewSource(1))); ok {
		t.Error("twoRandomEventIDs() with 1 id should return false")
	}

	r.addEventID("second")

	// Try many seeds: with 2 distinct ids, some picks collide (a==b) and some don't; both branches
	// of the function should be reachable across a spread of seeds.
	sawSuccess := false
	sawFailure := false

	for seed := int64(0); seed < 200; seed++ {
		a, b, ok := r.twoRandomEventIDs(rand.New(rand.NewSource(seed)))
		if ok {
			sawSuccess = true

			if a == b {
				t.Errorf("twoRandomEventIDs() returned ok=true with a==b (%q)", a)
			}
		} else {
			sawFailure = true
		}
	}

	if !sawSuccess {
		t.Error("expected at least one successful twoRandomEventIDs() over 200 seeds")
	}

	if !sawFailure {
		t.Error("expected at least one colliding twoRandomEventIDs() over 200 seeds")
	}
}

func TestRandomMemoryIDs(t *testing.T) {
	r := newRegistry(10, 10)

	if ids := r.randomMemoryIDs(rand.New(rand.NewSource(1)), 5); ids != nil {
		t.Errorf("randomMemoryIDs() on empty registry = %v, want nil", ids)
	}

	r.addMemoryIDs([]string{"m1", "m2", "m3", "m4", "m5"})

	ids := r.randomMemoryIDs(rand.New(rand.NewSource(1)), 3)
	if len(ids) == 0 {
		t.Fatal("expected at least one memory id")
	}

	seen := make(map[string]bool)
	for _, id := range ids {
		if seen[id] {
			t.Errorf("randomMemoryIDs() returned duplicate id %q", id)
		}
		seen[id] = true
	}

	// count larger than the registry: still bounded and distinct.
	ids = r.randomMemoryIDs(rand.New(rand.NewSource(2)), 100)
	if len(ids) > 5 {
		t.Errorf("randomMemoryIDs(count=100) len = %d, want <= 5", len(ids))
	}
}

func TestRemoveEventID(t *testing.T) {
	r := newRegistry(10, 10)

	r.addEventID("a")
	r.addEventID("b")
	r.addEventID("a")

	r.removeEventID("a")

	for _, id := range r.eventIDs {
		if id == "a" {
			t.Errorf("removeEventID() left %q in the registry", id)
		}
	}

	if len(r.eventIDs) != 1 {
		t.Errorf("len(eventIDs) = %d, want 1", len(r.eventIDs))
	}
}

func TestRemoveMemoryIDs(t *testing.T) {
	r := newRegistry(10, 10)

	r.removeMemoryIDs(nil) // no-op, must not panic on a nil slice.

	r.addMemoryIDs([]string{"a", "b", "c"})
	r.removeMemoryIDs([]string{"b"})

	if len(r.memoryIDs) != 2 {
		t.Fatalf("len(memoryIDs) = %d, want 2", len(r.memoryIDs))
	}

	for _, id := range r.memoryIDs {
		if id == "b" {
			t.Error("removeMemoryIDs() left 'b' in the registry")
		}
	}
}

func TestTrim(t *testing.T) {
	if got := trim([]string{"a", "b"}, 5); len(got) != 2 {
		t.Errorf("trim() under max should be a no-op, got %v", got)
	}

	got := trim([]string{"a", "b", "c", "d"}, 2)
	if len(got) != 2 || got[0] != "c" || got[1] != "d" {
		t.Errorf("trim() = %v, want [c d]", got)
	}
}
