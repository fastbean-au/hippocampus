package hippocampus

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/fastbean-au/hippocampus/db"
	"github.com/fastbean-au/hippocampus/notify"
	"github.com/fastbean-au/hippocampus/types"
)

// atRiskServer builds a Server whose decay configuration forgets what seedPreviewMemories stores,
// with the callback queue recording and the pre-reap kind on. It is previewTestServer's
// configuration plus callbackServer's, which is exactly what this feature sits across.
func atRiskServer(t *testing.T) (*Server, *db.DB) {
	t.Helper()

	database, err := db.New("")
	if err != nil {
		t.Fatalf("db.New: %s", err)
	}

	t.Cleanup(func() { _ = database.Close() })

	database.SetCallbackPolicy(db.CallbackPolicy{Enabled: true, MemoryEvents: true, EventEvents: true})

	s := &Server{
		db:                   database,
		consolidationEnabled: true,
		consolidation: Consolidation{
			method:            1,
			aggressiveness:    1.0,
			unitsOfAgeInDays:  1.0,
			deletionThreshold: 1.0,
			capacityPressure:  1.0,
		},
		callbacksEnabled:     true,
		callbackBatchSize:    10,
		callbackBaseBack:     time.Second,
		callbackMaxBack:      time.Minute,
		callbackChunkIds:     3,
		callbackAtRiskEvents: true,
		callbackAtRiskLimit:  db.PreviewLimit(0),
		stopCallbacks:        make(chan struct{}),
	}

	return s, database
}

// claimAtRisk returns the at-risk deliveries one cycle queued.
//
// Filtered by cycle id rather than by kind alone because claiming does not consume: a second pass
// over the queue sees what an earlier one already read, which is exactly the at-least-once property
// the dispatcher depends on. A zero cycle id takes every one, for the test that lets a real cycle
// choose its own.
func claimAtRisk(t *testing.T, database *db.DB, cycleId int64) []db.CallbackDelivery {
	t.Helper()

	claimed, err := database.ClaimCallbacks(context.Background(), 100, time.Now().UnixNano())
	if err != nil {
		t.Fatalf("ClaimCallbacks: %s", err)
	}

	var out []db.CallbackDelivery

	for _, delivery := range claimed {
		if delivery.Kind != db.CallbackKindMemoriesAtRisk {
			continue
		}

		if cycleId == 0 || delivery.CycleId == cycleId {
			out = append(out, delivery)
		}
	}

	return out
}

// TestAtRiskWarnsBeforeTheCycleTakesAnything is the mechanism: a cycle whose consolidation pass
// would take memories queues a delivery naming them, and does so at the top of the cycle, before
// anything has been deleted.
func TestAtRiskWarnsBeforeTheCycleTakesAnything(t *testing.T) {
	s, database := atRiskServer(t)
	ctx := context.Background()

	seedPreviewMemories(t, database, "risk-", 400, 1, 4)

	const cycleId = 77

	database.BeginCallbackCycle(cycleId)

	s.queueAtRiskCallback(ctx, cycleId)

	// The warning is queued and nothing has gone: the point of a pre-reap callback is that the
	// memories it names are still there to be acted on.
	with, without := database.CountMemories(ctx)
	if with+without != 4 {
		t.Fatalf("the at-risk scan deleted something: %d memories remain, want 4", with+without)
	}

	deliveries := claimAtRisk(t, database, cycleId)
	if len(deliveries) == 0 {
		t.Fatal("no at-risk delivery was queued")
	}

	var ids []string

	for _, delivery := range deliveries {
		if delivery.CycleId != cycleId {
			t.Errorf("an at-risk delivery carries cycle %d, want %d", delivery.CycleId, cycleId)
		}

		if delivery.Cause != db.CauseConsolidation {
			t.Errorf("an at-risk delivery carries cause %v, want consolidation", delivery.Cause)
		}

		if delivery.Payload.AtRisk == nil {
			t.Fatalf("an at-risk delivery lost its summary: %+v", delivery.Payload)
		}

		if delivery.Payload.AtRisk.Consolidating != 4 {
			t.Errorf("the summary reports %d consolidating, want 4", delivery.Payload.AtRisk.Consolidating)
		}

		for _, item := range delivery.Payload.Items {
			ids = append(ids, item.Id)

			// The value is what makes the warning actionable, and a body is never carried: the scan
			// behind it deliberately never reads one.
			if item.Value == 0 {
				t.Errorf("item %s carries no value", item.Id)
			}

			if item.Body != "" {
				t.Errorf("item %s carries a body, which an at-risk delivery must never do", item.Id)
			}
		}
	}

	if len(ids) != 4 {
		t.Errorf("the warning names %v, want all four memories", ids)
	}
}

// TestAtRiskIsSilentWhenNothingIsAtRisk is the "only when the set is non-empty" decision: a store at
// rest must not put a delivery in the queue on every cycle. The heartbeat is sleep_completed.
func TestAtRiskIsSilentWhenNothingIsAtRisk(t *testing.T) {
	s, database := atRiskServer(t)
	ctx := context.Background()

	seedPreviewMemories(t, database, "fresh-", 0, 1000, 4)

	s.queueAtRiskCallback(ctx, 11)

	if deliveries := claimAtRisk(t, database, 11); len(deliveries) != 0 {
		t.Fatalf("a store with nothing at risk queued %d deliveries", len(deliveries))
	}
}

// TestAtRiskIsOffByDefault covers the one deviation from the other three kinds: it costs a scan, so
// it must do nothing at all - not merely deliver nothing - unless it is asked for.
func TestAtRiskIsOffByDefault(t *testing.T) {
	s, database := atRiskServer(t)
	s.callbackAtRiskEvents = false

	seedPreviewMemories(t, database, "off-", 400, 1, 4)

	s.queueAtRiskCallback(context.Background(), 12)

	if deliveries := claimAtRisk(t, database, 12); len(deliveries) != 0 {
		t.Fatalf("the kind is off and yet queued %d deliveries", len(deliveries))
	}
}

// TestAtRiskMarginWidensTheWarning is what makes the kind actionable rather than merely earlier: a
// margin raises the bar the scan selects on, so memories still above the threshold - which this
// cycle will NOT take - are reported too, with both thresholds so the two can be told apart.
func TestAtRiskMarginWidensTheWarning(t *testing.T) {
	s, database := atRiskServer(t)
	ctx := context.Background()

	// Two populations: one already past the bar, one still comfortably above it. The second is
	// invisible at the default margin and reported at a generous one, which is the whole difference
	// between a warning that arrives as the deletion happens and one with notice on it.
	seedPreviewMemories(t, database, "gone-", 400, 1, 2)
	seedPreviewMemories(t, database, "soon-", 90, 500, 2)

	s.queueAtRiskCallback(ctx, 21)

	narrow := atRiskIds(claimAtRisk(t, database, 21))

	s.callbackAtRiskMargin = 100
	s.queueAtRiskCallback(ctx, 22)

	wide := claimAtRisk(t, database, 22)
	wideIds := atRiskIds(wide)

	if len(narrow) == 0 {
		t.Fatal("the default margin reported nothing")
	}

	if len(wideIds) <= len(narrow) {
		t.Fatalf("a margin of 100 reported %d memories, want more than the %d at the default", len(wideIds), len(narrow))
	}

	summary := wide[0].Payload.AtRisk
	if summary.AtRiskThreshold <= summary.Threshold {
		t.Errorf(
			"the summary reports an at-risk threshold of %v against a threshold of %v: a margin must raise the bar",
			summary.AtRiskThreshold,
			summary.Threshold,
		)
	}
}

// atRiskIds collects the ids a set of deliveries names.
func atRiskIds(deliveries []db.CallbackDelivery) []string {
	var ids []string

	for _, delivery := range deliveries {
		for _, item := range delivery.Payload.Items {
			ids = append(ids, item.Id)
		}
	}

	return ids
}

// TestAtRiskDeliveriesGroupByCauseAndChunk covers the projection on its own: the two causes become
// two independently numbered streams, every chunk repeats the summary, and a cause with nothing in
// it produces no delivery at all.
func TestAtRiskDeliveriesGroupByCauseAndChunk(t *testing.T) {
	summary := &db.CallbackAtRisk{Consolidating: 4, Evicting: 1, Threshold: 1.0, AtRiskThreshold: 1.25}

	candidates := []db.ForgetCandidate{
		{Id: "c1", Rule: db.ForgetRuleConsolidation, Value: 0.1},
		{Id: "c2", Rule: db.ForgetRuleConsolidation, Value: 0.2},
		{Id: "c3", Rule: db.ForgetRuleConsolidation, Value: 0.3},
		{Id: "e1", Rule: db.ForgetRuleEviction, Value: 0.9},
	}

	deliveries := atRiskDeliveries(9, summary, candidates, 2)

	if len(deliveries) != 3 {
		t.Fatalf("got %d deliveries, want 2 consolidation chunks and 1 eviction", len(deliveries))
	}

	byCause := map[db.DeleteCause][]db.CallbackDelivery{}

	for _, delivery := range deliveries {
		byCause[delivery.Cause] = append(byCause[delivery.Cause], delivery)

		if delivery.Kind != db.CallbackKindMemoriesAtRisk {
			t.Errorf("a delivery carries kind %v", delivery.Kind)
		}

		if delivery.CycleId != 9 {
			t.Errorf("a delivery carries cycle %d, want 9", delivery.CycleId)
		}

		// Every chunk repeats the summary, so a receiver that drops one still knows what the cycle
		// is about to do.
		if delivery.Payload.AtRisk != summary {
			t.Errorf("a delivery lost the summary")
		}
	}

	consolidation := byCause[db.CauseConsolidation]
	if len(consolidation) != 2 {
		t.Fatalf("got %d consolidation deliveries, want 2", len(consolidation))
	}

	// Each cause is numbered from 1 within itself, which is what lets a receiver reassemble on
	// (cycle_id, cause).
	for i, delivery := range consolidation {
		if delivery.Chunk != i+1 || delivery.Chunks != 2 {
			t.Errorf("consolidation chunk %d is numbered %d of %d", i, delivery.Chunk, delivery.Chunks)
		}
	}

	// The sample's value-ascending order survives, so chunk 1 is the closest to going.
	if got := consolidation[0].Payload.Items[0].Id; got != "c1" {
		t.Errorf("the first item of the first chunk is %s, want c1", got)
	}

	eviction := byCause[db.CauseEviction]
	if len(eviction) != 1 || eviction[0].Chunks != 1 {
		t.Fatalf("got %d eviction deliveries, want a single unchunked one", len(eviction))
	}
}

// TestAtRiskDeliveriesSkipAnEmptyCause pins that a cause with no candidates produces nothing, rather
// than an empty delivery a receiver would have to read to discover it was empty.
func TestAtRiskDeliveriesSkipAnEmptyCause(t *testing.T) {
	deliveries := atRiskDeliveries(1, &db.CallbackAtRisk{}, []db.ForgetCandidate{
		{Id: "c1", Rule: db.ForgetRuleConsolidation},
	}, 10)

	if len(deliveries) != 1 || deliveries[0].Cause != db.CauseConsolidation {
		t.Fatalf("got %d deliveries, want one consolidation delivery", len(deliveries))
	}
}

// TestTheSleepCycleWarnsBeforeItForgets is the wiring: one real cycle produces both the warning and
// the deletions it warned about, under one cycle id, and the warning names what actually went.
func TestTheSleepCycleWarnsBeforeItForgets(t *testing.T) {
	s, database := atRiskServer(t)
	ctx := context.Background()

	seedPreviewMemories(t, database, "cycle-", 400, 1, 3)

	if err := s.sleep(triggerManual); err != nil {
		t.Fatalf("sleep: %s", err)
	}

	warned := map[string]bool{}

	var cycleIds []int64

	for _, delivery := range claimAtRisk(t, database, 0) {
		cycleIds = append(cycleIds, delivery.CycleId)

		for _, item := range delivery.Payload.Items {
			warned[item.Id] = true
		}
	}

	if len(warned) != 3 {
		t.Fatalf("the cycle warned about %d memories, want 3", len(warned))
	}

	for i := range 3 {
		id := fmt.Sprintf("cycle-%d", i)

		if !warned[id] {
			t.Errorf("the cycle forgot %s without warning about it", id)
		}
	}

	with, without := database.CountMemories(ctx)
	if with+without != 0 {
		t.Errorf("the cycle warned but forgot nothing: %d memories remain", with+without)
	}

	// The warning shares the cycle's id with the deliveries that follow it, which is what lets a
	// receiver match what it was told was going against what went.
	for _, id := range cycleIds {
		if id == 0 {
			t.Error("an at-risk delivery carries no cycle id")
		}
	}
}

// TestAtRiskSurvivesAFailedScan covers the best-effort rule: this runs before the cycle's real work
// and must never stop it, or a receiver's problem would become a store that never consolidates.
func TestAtRiskSurvivesAFailedScan(t *testing.T) {
	s, database := atRiskServer(t)

	// Closing the store makes every read fail, which is the shape of any scan failure here.
	if err := database.Close(); err != nil {
		t.Fatalf("Close: %s", err)
	}

	s.queueAtRiskCallback(context.Background(), 31)
}

// TestAtRiskCarriesNoBodies is the property stated on notify.Item: includeBodies widens the deletion
// kinds and must not reach this one, whose scan never reads a body.
func TestAtRiskCarriesNoBodies(t *testing.T) {
	s, database := atRiskServer(t)

	database.SetCallbackPolicy(db.CallbackPolicy{
		Enabled: true, MemoryEvents: true, EventEvents: true, IncludeBodies: true, MaxBodyBytes: 1 << 20,
	})

	if _, err := database.CreateMemory(context.Background(), types.Memory{
		Id:           "bodied",
		TimeStamp:    time.Now().Add(-400 * 24 * time.Hour).UnixNano(),
		Significance: 1,
		Body:         strings.Repeat("secret", 16),
	}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	s.queueAtRiskCallback(context.Background(), 41)

	for _, delivery := range claimAtRisk(t, database, 41) {
		for _, item := range delivery.Payload.Items {
			if item.Body != "" || item.BodyOmitted {
				t.Errorf("an at-risk item carries body state (%q, omitted=%t)", item.Body, item.BodyOmitted)
			}
		}
	}
}

// TestAtRiskReachesTheReceiver is the projection end to end: the summary and each item's value
// survive the queue's encoding and the storage-to-wire projection, which is where a field added to
// one of the two structs and forgotten in the other goes quiet.
func TestAtRiskReachesTheReceiver(t *testing.T) {
	s, database := atRiskServer(t)
	ctx := context.Background()

	seedPreviewMemories(t, database, "wire-", 400, 1, 2)

	s.queueAtRiskCallback(ctx, 51)

	sink := &recordingNotifier{}

	if sent := s.dispatchCallbacksOnce(sink); sent == 0 {
		t.Fatal("the dispatcher sent nothing")
	}

	var warning *notify.Delivery

	delivered := sink.taken()

	for i := range delivered {
		if delivered[i].Kind != notify.KindMemoriesAtRisk {
			continue
		}

		warning = &delivered[i]

		break
	}

	if warning == nil {
		t.Fatal("no memories_at_risk delivery reached the receiver")
	}

	if warning.Cause != notify.CauseConsolidation {
		t.Errorf("the delivery carries cause %q, want consolidation", warning.Cause)
	}

	if warning.AtRisk == nil {
		t.Fatal("the delivery reached the receiver without its summary")
	}

	if warning.AtRisk.Consolidating != 2 || warning.AtRisk.Threshold <= 0 {
		t.Errorf("the summary arrived as %+v", warning.AtRisk)
	}

	if len(warning.Items) == 0 || warning.Items[0].Value == 0 {
		t.Errorf("the items arrived without their values: %+v", warning.Items)
	}
}
