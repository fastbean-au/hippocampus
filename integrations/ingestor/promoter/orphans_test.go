package promoter

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/fastbean-au/hippocampus/contract"
)

// orphan builds an event-less memory of the given age.
func orphan(id string, age time.Duration) *contract.Memory {
	return &contract.Memory{
		Id:           id,
		Body:         "no event",
		Significance: 3,
		TimeStamp:    nanos(testNow.Add(-age)),
	}
}

// TestOrphanPolicies covers all three, and the property they share: the count is always reported, so
// an operator can see that their writers are not associating memories with events even under the
// policy that acts on nothing.
func TestOrphanPolicies(t *testing.T) {
	cases := []struct {
		name            string
		policy          OrphanPolicy
		wantOnSource    []string
		wantOnTarget    []string
		wantPromoted    int
		wantDropped     int
		wantSeenAtLeast int
	}{
		{
			name:            "ignore leaves them but reports them",
			policy:          OrphanIgnore,
			wantOnSource:    []string{"o1", "o2"},
			wantOnTarget:    []string{},
			wantSeenAtLeast: 2,
		},
		{
			name:            "promote sends them then removes them",
			policy:          OrphanPromote,
			wantOnSource:    []string{},
			wantOnTarget:    []string{"o1", "o2"},
			wantPromoted:    2,
			wantSeenAtLeast: 2,
		},
		{
			name:            "drop removes them without promoting",
			policy:          OrphanDrop,
			wantOnSource:    []string{},
			wantOnTarget:    []string{},
			wantDropped:     2,
			wantSeenAtLeast: 2,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			source := newFakeStore()
			target := newFakeStore()

			source.putMemory(orphan("o1", time.Hour))
			source.putMemory(orphan("o2", 2*time.Hour))

			p := newPromoter(t, source, target, `{"defaultAction":"drop","rules":[]}`, Config{
				Orphans:   c.policy,
				OrphanAge: time.Minute,
			})

			stats, err := p.Pass(context.Background())
			if err != nil {
				t.Fatalf("Pass: %s", err)
			}

			if stats.OrphansSeen < c.wantSeenAtLeast {
				t.Errorf("expected at least %d orphans reported, got %d", c.wantSeenAtLeast, stats.OrphansSeen)
			}

			if stats.OrphansPromoted != c.wantPromoted {
				t.Errorf("expected %d promoted, got %d", c.wantPromoted, stats.OrphansPromoted)
			}

			if stats.OrphansDropped != c.wantDropped {
				t.Errorf("expected %d dropped, got %d", c.wantDropped, stats.OrphansDropped)
			}

			if got := source.memoryIds(); !reflect.DeepEqual(got, c.wantOnSource) {
				t.Errorf("expected %v left on the source, got %v", c.wantOnSource, got)
			}

			if got := target.memoryIds(); !reflect.DeepEqual(got, c.wantOnTarget) {
				t.Errorf("expected %v on the target, got %v", c.wantOnTarget, got)
			}
		})
	}
}

// TestOrphanAgeWindow pins that a memory written moments ago is left alone: it may simply be waiting
// for its writer to create the event.
func TestOrphanAgeWindow(t *testing.T) {
	source := newFakeStore()
	target := newFakeStore()

	source.putMemory(orphan("fresh", time.Second))
	source.putMemory(orphan("old", time.Hour))

	p := newPromoter(t, source, target, `{"defaultAction":"drop","rules":[]}`, Config{
		Orphans:   OrphanDrop,
		OrphanAge: time.Minute,
	})

	if _, err := p.Pass(context.Background()); err != nil {
		t.Fatalf("Pass: %s", err)
	}

	if got := source.memoryIds(); !reflect.DeepEqual(got, []string{"fresh"}) {
		t.Errorf("expected only the recent orphan to survive, got %v", got)
	}
}

// TestOrphansAreNotTheEventedOnes is the filter that matters: has_event FALSE, not an empty
// event_id, which would mean "no restriction" and sweep in every memory in the store.
func TestOrphansAreNotTheEventedOnes(t *testing.T) {
	source := newFakeStore()
	target := newFakeStore()

	source.putEvent(endedEvent("e1", "open business", nil))
	source.putMemory(memory("m1", "e1", "belongs to e1", 5))
	source.putMemory(orphan("o1", time.Hour))

	p := newPromoter(t, source, target, `{"defaultAction":"drop","rules":[]}`, Config{
		Orphans:   OrphanDrop,
		OrphanAge: time.Minute,
	})

	stats, err := p.Pass(context.Background())
	if err != nil {
		t.Fatalf("Pass: %s", err)
	}

	if stats.OrphansSeen != 1 {
		t.Errorf("expected exactly one orphan, got %d", stats.OrphansSeen)
	}

	// e1 was judged and drained by the pass; the orphan was dropped by the policy. Neither reached
	// the target, and nothing of e1's was counted as an orphan.
	if stats.OrphansDropped != 1 {
		t.Errorf("expected the one orphan dropped, got %d", stats.OrphansDropped)
	}
}

// TestOrphanDryRunTouchesNothing mirrors the event dry run.
func TestOrphanDryRunTouchesNothing(t *testing.T) {
	source := newFakeStore()
	target := newFakeStore()

	source.putMemory(orphan("o1", time.Hour))

	p := newPromoter(t, source, target, `{"defaultAction":"drop","rules":[]}`, Config{
		Orphans:   OrphanPromote,
		OrphanAge: time.Minute,
		DryRun:    true,
	})

	stats, err := p.Pass(context.Background())
	if err != nil {
		t.Fatalf("Pass: %s", err)
	}

	if stats.OrphansPromoted != 1 {
		t.Errorf("expected the dry run to report the promotion, got %+v", stats)
	}

	if got := source.memoryIds(); !reflect.DeepEqual(got, []string{"o1"}) {
		t.Errorf("a dry run must leave the source alone, got %v", got)
	}

	if got := target.memoryIds(); len(got) != 0 {
		t.Errorf("a dry run must not write to the target, got %v", got)
	}
}

// TestOrphanPromotionFailureLeavesThemOnTheSource is the same ordering guarantee promotion has: the
// delete only runs once the target has accepted everything.
func TestOrphanPromotionFailureLeavesThemOnTheSource(t *testing.T) {
	source := newFakeStore()
	target := newFakeStore()

	source.putMemory(orphan("o1", time.Hour))

	target.failNext("ImportBatch", errors.New("target unavailable"))

	p := newPromoter(t, source, target, `{"defaultAction":"drop","rules":[]}`, Config{
		Orphans:   OrphanPromote,
		OrphanAge: time.Minute,
	})

	stats, err := p.Pass(context.Background())
	if err != nil {
		t.Fatalf("Pass: %s", err)
	}

	if stats.Errors != 1 || stats.OrphansPromoted != 0 {
		t.Errorf("expected the promotion to fail, got %+v", stats)
	}

	if got := source.memoryIds(); !reflect.DeepEqual(got, []string{"o1"}) {
		t.Errorf("expected the orphan to survive on the source, got %v", got)
	}
}

// TestOrphanListingErrorIsCountedNotFatal keeps one bad read from failing the whole pass, which has
// already done its real work by the time orphans are looked at.
func TestOrphanListingErrorIsCountedNotFatal(t *testing.T) {
	source := newFakeStore()
	target := newFakeStore()

	source.putMemory(orphan("o1", time.Hour))
	source.failNext("GetMemories", errors.New("source busy"))

	p := newPromoter(t, source, target, `{"defaultAction":"drop","rules":[]}`, Config{Orphans: OrphanDrop})

	stats, err := p.Pass(context.Background())
	if err != nil {
		t.Fatalf("Pass must not fail on an orphan read: %s", err)
	}

	if stats.Errors != 1 {
		t.Errorf("expected the read failure to be counted, got %+v", stats)
	}

	if got := source.memoryIds(); !reflect.DeepEqual(got, []string{"o1"}) {
		t.Errorf("expected the orphan untouched, got %v", got)
	}
}

// TestValidOrphanPolicy covers the flag validation main.go performs.
func TestValidOrphanPolicy(t *testing.T) {
	for _, policy := range []OrphanPolicy{OrphanIgnore, OrphanPromote, OrphanDrop} {
		if !ValidOrphanPolicy(policy) {
			t.Errorf("expected %q to be valid", policy)
		}
	}

	for _, policy := range []OrphanPolicy{"", "keep", "PROMOTE"} {
		if ValidOrphanPolicy(policy) {
			t.Errorf("expected %q to be rejected", policy)
		}
	}
}
