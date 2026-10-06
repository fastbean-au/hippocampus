package reap

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/fastbean-au/hippocampus/contract"
	"github.com/fastbean-au/hippocampus/notify"

	"github.com/fastbean-au/hippocampus/integrations/objectstore/objects"
)

// fakeLog is a forgotten log of one page.
type fakeLog struct {
	records []*contract.ForgottenMemory
	enabled bool
	err     error
	since   time.Time
}

func (f *fakeLog) Forgotten(
	ctx context.Context,
	since time.Time,
	fn func([]*contract.ForgottenMemory) error,
) (bool, error) {
	f.since = since

	if f.err != nil {
		return f.enabled, f.err
	}

	if len(f.records) > 0 {
		if err := fn(f.records); err != nil {
			return f.enabled, err
		}
	}

	return f.enabled, nil
}

func forgotten(id string, rule contract.ForgetRule) *contract.ForgottenMemory {
	return &contract.ForgottenMemory{Id: id, Rule: rule}
}

func newCatchUp(t *testing.T, log *fakeLog, window time.Duration) (*CatchUp, *objects.Memory) {
	t.Helper()

	return newCatchUpHolding(t, log, window, &fakeHeld{})
}

// newCatchUpHolding is newCatchUp over a store that reports the given memories as still held.
func newCatchUpHolding(t *testing.T, log *fakeLog, window time.Duration, held *fakeHeld) (*CatchUp, *objects.Memory) {
	t.Helper()

	store := objects.NewMemory("payloads")
	store.Put("one.json", []byte("one"), time.Now())
	store.Put("two.json", []byte("two"), time.Now())

	reaper, err := New(Config{Store: store, Memories: held, Delete: true})
	if err != nil {
		t.Fatalf("New failed: %s", err.Error())
	}

	catchUp, err := NewCatchUp(reaper, log, window)
	if err != nil {
		t.Fatalf("NewCatchUp failed: %s", err.Error())
	}

	return catchUp, store
}

func TestTheCatchUpReapsWhatTheLogRecorded(t *testing.T) {
	log := &fakeLog{
		enabled: true,
		records: []*contract.ForgottenMemory{
			forgotten("payloads/one.json", contract.ForgetRule_FORGET_RULE_CONSOLIDATION),
			forgotten("payloads/two.json", contract.ForgetRule_FORGET_RULE_EVICTION),
		},
	}

	catchUp, store := newCatchUp(t, log, time.Hour)

	result, err := catchUp.Run(context.Background())
	if err != nil {
		t.Fatalf("Run failed: %s", err.Error())
	}

	if result.Deleted != 2 {
		t.Errorf("expected both to be reaped, got %+v", result)
	}

	if store.Len() != 0 {
		t.Error("expected both objects to be gone")
	}

	if window := time.Since(log.since); window < 50*time.Minute || window > 70*time.Minute {
		t.Errorf("expected the log to be read back over the window, got %s", window)
	}
}

// The pull path applies the same cause filter the push path does, mapping the log's rule onto the
// cause it corresponds to.
func TestTheCatchUpHonoursTheCauseFilter(t *testing.T) {
	log := &fakeLog{
		enabled: true,
		records: []*contract.ForgottenMemory{
			forgotten("payloads/one.json", contract.ForgetRule_FORGET_RULE_CONSOLIDATION),
			forgotten("payloads/two.json", contract.ForgetRule_FORGET_RULE_EVICTION),
		},
	}

	store := objects.NewMemory("payloads")
	store.Put("one.json", []byte("one"), time.Now())
	store.Put("two.json", []byte("two"), time.Now())

	causes, err := NewCauses("consolidation")
	if err != nil {
		t.Fatalf("NewCauses failed: %s", err.Error())
	}

	reaper, err := New(Config{Store: store, Memories: &fakeHeld{}, Delete: true, Causes: causes})
	if err != nil {
		t.Fatalf("New failed: %s", err.Error())
	}

	catchUp, err := NewCatchUp(reaper, log, time.Hour)
	if err != nil {
		t.Fatalf("NewCatchUp failed: %s", err.Error())
	}

	if _, err := catchUp.Run(context.Background()); err != nil {
		t.Fatalf("Run failed: %s", err.Error())
	}

	if store.Has("one.json") {
		t.Error("expected the consolidated memory's object to be deleted")
	}

	if !store.Has("two.json") {
		t.Error("expected the evicted memory's object to survive a consolidation-only filter")
	}
}

func TestAZeroWindowDisablesTheCatchUp(t *testing.T) {
	log := &fakeLog{
		enabled: true,
		records: []*contract.ForgottenMemory{forgotten("payloads/one.json", contract.ForgetRule_FORGET_RULE_CONSOLIDATION)},
	}

	catchUp, store := newCatchUp(t, log, 0)

	result, err := catchUp.Run(context.Background())
	if err != nil {
		t.Fatalf("Run failed: %s", err.Error())
	}

	if result.Acted() != 0 || store.Len() != 2 {
		t.Error("expected a zero window to do nothing at all")
	}
}

func TestAFailingLogIsReported(t *testing.T) {
	log := &fakeLog{enabled: true, err: fmt.Errorf("the service is down")}

	catchUp, _ := newCatchUp(t, log, time.Hour)

	if _, err := catchUp.Run(context.Background()); err == nil {
		t.Error("expected the failure to surface")
	}
}

// A store with no forgotten log cannot be caught up on at all, and an agent that simply reported
// nothing to do would look exactly like one that was up to date.
func TestADisabledLogIsNotSilent(t *testing.T) {
	log := &fakeLog{enabled: false}

	catchUp, _ := newCatchUp(t, log, time.Hour)

	if _, err := catchUp.Run(context.Background()); err != nil {
		t.Fatalf("Run failed: %s", err.Error())
	}
}

func TestNewCatchUpValidatesItsArguments(t *testing.T) {
	store := objects.NewMemory("payloads")

	reaper, err := New(Config{Store: store, Memories: &fakeHeld{}})
	if err != nil {
		t.Fatalf("New failed: %s", err.Error())
	}

	if _, err := NewCatchUp(nil, &fakeLog{}, time.Hour); err == nil {
		t.Error("expected a catch-up with no reaper to be refused")
	}

	if _, err := NewCatchUp(reaper, nil, time.Hour); err == nil {
		t.Error("expected a catch-up with no log reader to be refused")
	}
}

func TestTheRuleMapsOntoACause(t *testing.T) {
	if got := cause(contract.ForgetRule_FORGET_RULE_CONSOLIDATION); got != "consolidation" {
		t.Errorf("expected consolidation, got %q", got)
	}

	if got := cause(contract.ForgetRule_FORGET_RULE_EVICTION); got != "eviction" {
		t.Errorf("expected eviction, got %q", got)
	}

	if got := cause(contract.ForgetRule_FORGET_RULE_UNSPECIFIED); got != "" {
		t.Errorf("expected an unspecified rule to carry no cause, got %q", got)
	}
}

// TestTheCatchUpLeavesAReRegisteredObjectAlone: the forgotten log is a record of what WENT, and an
// object can be uploaded again under the same key and its memory registered again afterwards. The
// log still names the old memory, so the catch-up asks the store first, as the sweep does, rather
// than delete the live object behind the new one (TODO-3 item 144).
func TestTheCatchUpLeavesAReRegisteredObjectAlone(t *testing.T) {
	log := &fakeLog{
		enabled: true,
		records: []*contract.ForgottenMemory{
			forgotten("payloads/one.json", contract.ForgetRule_FORGET_RULE_CONSOLIDATION),
			forgotten("payloads/two.json", contract.ForgetRule_FORGET_RULE_CONSOLIDATION),
		},
	}

	held := &fakeHeld{holds: map[string]bool{"payloads/one.json": true}}
	catchUp, store := newCatchUpHolding(t, log, time.Hour, held)

	if _, err := catchUp.Run(context.Background()); err != nil {
		t.Fatalf("Run failed: %s", err.Error())
	}

	if !store.Has("one.json") {
		t.Error("the catch-up deleted the object behind a memory the store holds again")
	}

	if store.Has("two.json") {
		t.Error("expected the object behind a memory that is gone to be deleted")
	}
}

// TestTheCatchUpStopsWhenTheStoreCannotBeAsked: nothing is deleted on a guess.
func TestTheCatchUpStopsWhenTheStoreCannotBeAsked(t *testing.T) {
	log := &fakeLog{
		enabled: true,
		records: []*contract.ForgottenMemory{forgotten("payloads/one.json", contract.ForgetRule_FORGET_RULE_CONSOLIDATION)},
	}

	catchUp, store := newCatchUpHolding(t, log, time.Hour, &fakeHeld{err: fmt.Errorf("unavailable")})

	if _, err := catchUp.Run(context.Background()); err == nil {
		t.Error("expected the catch-up to fail when the store cannot be asked")
	}

	if !store.Has("one.json") {
		t.Error("an object was deleted although the store could not be asked whether it was held")
	}
}

// TestExpiryIsActedOnByDefault: an expired memory's object should go like a decayed one's - expiry
// is the store forgetting on its own, which is what the default causes are (TODO-3 item 157). It is
// acted on from a delivery, accepted by --causes, and mapped from the forgotten log's rule on the
// catch-up path.
func TestExpiryIsActedOnByDefault(t *testing.T) {
	defaults, err := NewCauses("")
	if err != nil {
		t.Fatalf("NewCauses: %s", err)
	}

	if !defaults.Acts(notify.CauseExpiry) {
		t.Error("the default causes do not include expiry")
	}

	if _, err := NewCauses("expiry"); err != nil {
		t.Errorf("--causes expiry was refused: %s", err)
	}

	log := &fakeLog{
		enabled: true,
		records: []*contract.ForgottenMemory{forgotten("payloads/one.json", contract.ForgetRule_FORGET_RULE_EXPIRY)},
	}

	catchUp, store := newCatchUp(t, log, time.Hour)

	if _, err := catchUp.Run(context.Background()); err != nil {
		t.Fatalf("Run failed: %s", err.Error())
	}

	if store.Has("one.json") {
		t.Error("the catch-up left the object behind an expired memory")
	}
}
