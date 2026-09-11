package reap

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/fastbean-au/hippocampus/notify"

	"github.com/fastbean-au/hippocampus/integrations/objectstore/objects"
)

func newReaper(t *testing.T, deletes bool) (*Reaper, *objects.Memory) {
	t.Helper()

	store := objects.NewMemory("payloads")
	store.Put("traces/one.json", []byte("one"), time.Now())
	store.Put("traces/two.json", []byte("two"), time.Now())

	reaper, err := New(Config{Store: store, Delete: deletes})
	if err != nil {
		t.Fatalf("New failed: %s", err.Error())
	}

	return reaper, store
}

// Shadow mode is the default everywhere in this package, because a component that deletes somebody
// else's data on the strength of a decay model has to be watched before it is trusted.
func TestShadowModeSelectsWithoutDeleting(t *testing.T) {
	reaper, store := newReaper(t, false)

	if reaper.Armed() {
		t.Fatal("expected the default to be shadow mode")
	}

	result, err := reaper.Reap(context.Background(), PathCallback, []string{"payloads/traces/one.json"})
	if err != nil {
		t.Fatalf("Reap failed: %s", err.Error())
	}

	if result.Shadowed != 1 || result.Deleted != 0 {
		t.Errorf("expected one shadowed and none deleted, got %+v", result)
	}

	if !store.Has("traces/one.json") {
		t.Error("expected the object to survive shadow mode")
	}
}

func TestArmedItDeletes(t *testing.T) {
	reaper, store := newReaper(t, true)

	result, err := reaper.Reap(context.Background(), PathCallback, []string{
		"payloads/traces/one.json",
		"payloads/traces/two.json",
	})
	if err != nil {
		t.Fatalf("Reap failed: %s", err.Error())
	}

	if result.Deleted != 2 {
		t.Errorf("expected two deletions, got %+v", result)
	}

	if store.Len() != 0 {
		t.Errorf("expected the bucket to be empty, %d objects remain", store.Len())
	}
}

// The two ways an id is none of this agent's business. Neither is an error, and neither may ever
// become a deletion - a store shared with another producer is full of both.
func TestIdsThisAgentDoesNotOwnAreNeverDeleted(t *testing.T) {
	reaper, store := newReaper(t, true)

	result, err := reaper.Reap(context.Background(), PathCallback, []string{
		"6f1c3d0e-4a5b-4c7d-8e9f-0a1b2c3d4e5f",
		"another-bucket/traces/one.json",
	})
	if err != nil {
		t.Fatalf("Reap failed: %s", err.Error())
	}

	if result.Unmappable != 1 || result.Foreign != 1 {
		t.Errorf("expected one unmappable and one foreign, got %+v", result)
	}

	if result.Deleted != 0 || store.Len() != 2 {
		t.Error("expected nothing to be deleted")
	}
}

// Every object is tried before the error is returned, so one unreachable key does not leave the
// rest of a batch for a sweep that may not be enabled.
func TestAFailureDoesNotStopTheRestOfTheBatch(t *testing.T) {
	reaper, store := newReaper(t, true)
	store.DeleteErr = fmt.Errorf("access denied")

	result, err := reaper.Reap(context.Background(), PathCallback, []string{
		"payloads/traces/one.json",
		"payloads/traces/two.json",
	})
	if err == nil {
		t.Fatal("expected the failures to surface")
	}

	if result.Failed != 2 {
		t.Errorf("expected both to be attempted and to fail, got %+v", result)
	}
}

func TestNewRefusesAConfigurationWithNoBucket(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Error("expected a reaper with no bucket to be refused")
	}
}

func TestCausesDefaultToTheDecayPaths(t *testing.T) {
	causes, err := NewCauses("")
	if err != nil {
		t.Fatalf("NewCauses failed: %s", err.Error())
	}

	acted := []notify.Cause{notify.CauseConsolidation, notify.CauseEviction, notify.CauseCascade}
	for _, v := range acted {
		if !causes.Acts(v) {
			t.Errorf("expected the default to act on %q", v)
		}
	}

	// The omissions are each a way to destroy data that is still wanted: a clear is a MOVE, a purge
	// is an operator resetting the store, and a summary replacement is a judgement this agent does
	// not make by default.
	ignored := []notify.Cause{notify.CauseClear, notify.CausePurge, notify.CauseSummaryReplace, notify.CauseClient}
	for _, v := range ignored {
		if causes.Acts(v) {
			t.Errorf("expected the default NOT to act on %q", v)
		}
	}
}

func TestCausesCanBeWidened(t *testing.T) {
	causes, err := NewCauses("consolidation, client")
	if err != nil {
		t.Fatalf("NewCauses failed: %s", err.Error())
	}

	if !causes.Acts(notify.CauseClient) {
		t.Error("expected the widened set to act on a client deletion")
	}

	if causes.Acts(notify.CauseEviction) {
		t.Error("expected an explicit list to replace the default rather than extend it")
	}
}

func TestAnUnknownCauseIsRefused(t *testing.T) {
	if _, err := NewCauses("consolidation,whenever"); err == nil {
		t.Error("expected an unknown cause to be refused")
	}
}

// An unattributed deletion is treated as a decay one, since the two decay passes are the only thing
// that has ever produced one.
func TestAnUnsetCauseFollowsConsolidation(t *testing.T) {
	acting, err := NewCauses("consolidation")
	if err != nil {
		t.Fatalf("NewCauses failed: %s", err.Error())
	}

	if !acting.Acts("") {
		t.Error("expected an unset cause to be acted on when consolidation is")
	}

	notActing, err := NewCauses("eviction")
	if err != nil {
		t.Fatalf("NewCauses failed: %s", err.Error())
	}

	if notActing.Acts("") {
		t.Error("expected an unset cause to be ignored when consolidation is not acted on")
	}
}

func TestResultAccumulates(t *testing.T) {
	total := Result{}

	total.Add(Result{Deleted: 1, Shadowed: 2, Foreign: 3, Unmappable: 4, Failed: 5})
	total.Add(Result{Deleted: 1})

	if total.Deleted != 2 || total.Shadowed != 2 || total.Foreign != 3 || total.Unmappable != 4 || total.Failed != 5 {
		t.Errorf("unexpected total: %+v", total)
	}

	if total.Acted() != 4 {
		t.Errorf("expected Acted to count deletions and shadows, got %d", total.Acted())
	}
}
