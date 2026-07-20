package main

import (
	"encoding/json"
	"errors"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestNextID checks the id counter produces strictly increasing, prefixed, zero-padded ids
// regardless of what other tests in this binary have already consumed from the shared counter.
func TestNextID(t *testing.T) {
	first := nextID("evt")
	second := nextID("evt")

	if first == second {
		t.Errorf("nextID() returned the same id twice: %q", first)
	}

	if !strings.HasPrefix(first, "evt-") {
		t.Errorf("nextID(%q) = %q, want prefix %q", "evt", first, "evt-")
	}

	suffix := strings.TrimPrefix(first, "evt-")
	if len(suffix) != 8 {
		t.Errorf("nextID() suffix = %q, want length 8", suffix)
	}

	if _, err := strconv.Atoi(suffix); err != nil {
		t.Errorf("nextID() suffix = %q, want numeric: %s", suffix, err)
	}
}

// TestGenerateEvent runs generateEvent many times over one seeded rng and, for each draw,
// asserts the invariants specific to whichever of the two branches (clustered vs. unique) fired.
// With p(unique) = 0.18, 500 draws makes seeing neither branch astronomically unlikely, so this
// exercises both without needing to hunt for a seed that lands on a specific branch.
func TestGenerateEvent(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	now := time.Now()

	var sawClustered, sawUnique bool

	for i := 0; i < 500; i++ {
		ev, mems := generateEvent(rng, now)

		if ev.ID == "" {
			t.Fatalf("generateEvent() produced an event with an empty ID")
		}

		if ev.TimeStart == "" {
			t.Fatalf("generateEvent() produced an event with an empty TimeStart")
		}

		if _, err := strconv.ParseInt(ev.TimeStart, 10, 64); err != nil {
			t.Fatalf("event TimeStart = %q, want a parseable UnixNano string: %s", ev.TimeStart, err)
		}

		// The clustered branch's description is "name — opener"; the unique branch's description
		// is the template's literal desc, which never contains that separator.
		clustered := strings.Contains(ev.Description, " — ")

		if clustered {
			sawClustered = true
			checkClusteredEvent(t, ev, mems)
		} else {
			sawUnique = true
			checkUniqueEvent(t, ev, mems)
		}
	}

	if !sawClustered {
		t.Error("generateEvent() over 500 draws never produced a clustered event")
	}

	if !sawUnique {
		t.Error("generateEvent() over 500 draws never produced a unique event")
	}
}

func checkClusteredEvent(t *testing.T, ev jsonEvent, mems []jsonMemory) {
	t.Helper()

	if ev.Significance < 20 || ev.Significance > 89 {
		t.Errorf("clustered event significance = %d, want [20,89]", ev.Significance)
	}

	if len(mems) < 8 || len(mems) > 55 {
		t.Errorf("clustered event memory count = %d, want [8,55]", len(mems))
	}

	checkMemories(t, ev, mems, 1, 99)
}

func checkUniqueEvent(t *testing.T, ev jsonEvent, mems []jsonMemory) {
	t.Helper()

	if ev.Significance < 60 || ev.Significance > 100 {
		t.Errorf("unique event significance = %d, want [60,100]", ev.Significance)
	}

	if len(mems) < 6 || len(mems) > 15 {
		t.Errorf("unique event memory count = %d, want [6,15]", len(mems))
	}

	checkMemories(t, ev, mems, 40, 100)
}

func checkMemories(t *testing.T, ev jsonEvent, mems []jsonMemory, minSig int32, maxSig int32) {
	t.Helper()

	seenIDs := make(map[string]bool, len(mems))

	for _, m := range mems {
		if m.ID == "" {
			t.Errorf("memory has an empty ID")
		}

		if seenIDs[m.ID] {
			t.Errorf("memory ID %q generated twice within one event", m.ID)
		}

		seenIDs[m.ID] = true

		if m.EventID != ev.ID {
			t.Errorf("memory EventID = %q, want %q", m.EventID, ev.ID)
		}

		if m.Group != ev.Group {
			t.Errorf("memory Group = %q, want %q", m.Group, ev.Group)
		}

		if m.Significance < minSig || m.Significance > maxSig {
			t.Errorf("memory significance = %d, want [%d,%d]", m.Significance, minSig, maxSig)
		}

		if m.Body == "" {
			t.Errorf("memory has an empty body")
		}

		if _, err := strconv.ParseInt(m.TimeStamp, 10, 64); err != nil {
			t.Errorf("memory TimeStamp = %q, want a parseable UnixNano string: %s", m.TimeStamp, err)
		}
	}
}

// TestMemNano checks the produced timestamp is a valid UnixNano string landing within the
// documented "a few hours after the event" window.
func TestMemNano(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	base := time.Now().Add(-72 * time.Hour)

	for i := 0; i < 50; i++ {
		s := memNano(rng, base)

		nanos, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			t.Fatalf("memNano() = %q, want a parseable int64: %s", s, err)
		}

		got := time.Unix(0, nanos)

		if got.Before(base) {
			t.Errorf("memNano() = %s, want >= base %s", got, base)
		}

		if got.After(base.Add(6 * time.Hour)) {
			t.Errorf("memNano() = %s, want <= base+6h %s", got, base.Add(6*time.Hour))
		}
	}
}

func TestPick(t *testing.T) {
	rng := rand.New(rand.NewSource(1))

	single := []string{"only"}
	for i := 0; i < 10; i++ {
		if got := pick(rng, single); got != "only" {
			t.Errorf("pick(single-element) = %q, want %q", got, "only")
		}
	}

	options := []string{"a", "b", "c"}
	seen := make(map[string]bool)

	for i := 0; i < 200; i++ {
		got := pick(rng, options)

		found := false

		for _, o := range options {
			if got == o {
				found = true

				break
			}
		}

		if !found {
			t.Fatalf("pick() = %q, want one of %v", got, options)
		}

		seen[got] = true
	}

	if len(seen) != len(options) {
		t.Errorf("pick() over 200 draws saw %d distinct values, want %d", len(seen), len(options))
	}
}

// TestFillNumbers exercises the %d, %02d/%04d, and %s substitution paths using the same phrase
// shapes the templates actually use, and checks the placeholders are fully resolved (no stray
// verbs left in the output).
func TestFillNumbers(t *testing.T) {
	rng := rand.New(rand.NewSource(5))

	cases := []string{
		"Error rate climbed to %d%% and p99 latency crossed %dms.",
		"PagerDuty alert fired at %02d:%02d UTC for the %s.",
		"Partition for %04d-%02d was reprocessed from source.",
		"no placeholders here",
	}

	for _, c := range cases {
		got := fillNumbers(rng, c)

		if got == "" {
			t.Errorf("fillNumbers(%q) returned an empty string", c)
		}

		if strings.Contains(got, "%d") || strings.Contains(got, "%s") {
			t.Errorf("fillNumbers(%q) = %q, want no unresolved verbs", c, got)
		}
	}
}

// TestFillNumbersMismatch checks a verb the switch doesn't recognise (so no arg is appended)
// does not panic; the recover() guard should let fmt.Sprintf's normal MISSING-arg formatting
// through untouched.
func TestFillNumbersMismatch(t *testing.T) {
	rng := rand.New(rand.NewSource(1))

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("fillNumbers() panicked: %v", r)
		}
	}()

	got := fillNumbers(rng, "unsupported verb %x here")
	if got == "" {
		t.Error("fillNumbers() with a mismatched verb returned an empty string")
	}
}

func TestBuildTemplateBody(t *testing.T) {
	rng := rand.New(rand.NewSource(2))
	tmpl := clusteredTemplates[0]

	body := buildTemplateBody(rng, "payments-api", tmpl)

	if !strings.HasPrefix(body, "[payments-api] ") {
		t.Errorf("buildTemplateBody() = %q, want prefix %q", body, "[payments-api] ")
	}

	if len(body) < 20 {
		t.Errorf("buildTemplateBody() length = %d, want a multi-sentence body", len(body))
	}
}

func TestBuildBody(t *testing.T) {
	rng := rand.New(rand.NewSource(2))

	opener := "Legal and finance completed the acquisition."
	details := []string{"Detail one happened.", "Detail two happened."}

	body := buildBody(rng, opener, details)

	if !strings.HasPrefix(body, opener) {
		t.Errorf("buildBody() = %q, want prefix %q", body, opener)
	}

	if len(body) <= len(opener) {
		t.Errorf("buildBody() length = %d, want more than the opener alone (%d)", len(body), len(opener))
	}
}

func TestPostBatchSuccess(t *testing.T) {
	var gotPath string

	var gotBatch batchReq

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path

		if err := json.NewDecoder(r.Body).Decode(&gotBatch); err != nil {
			t.Fatalf("server failed to decode request body: %s", err)
		}

		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	batch := &batchReq{
		Events:   []jsonEvent{{ID: "evt-1", Name: "test event"}},
		Memories: []jsonMemory{{ID: "mem-1", Body: "hello"}},
	}

	if err := postBatch(server.Client(), server.URL, batch); err != nil {
		t.Fatalf("postBatch() error = %v, want nil", err)
	}

	if gotPath != "/v1/import/batch" {
		t.Errorf("postBatch() posted to %q, want %q", gotPath, "/v1/import/batch")
	}

	if len(gotBatch.Events) != 1 || gotBatch.Events[0].ID != "evt-1" {
		t.Errorf("server received events = %+v, want one event with ID evt-1", gotBatch.Events)
	}

	if len(gotBatch.Memories) != 1 || gotBatch.Memories[0].ID != "mem-1" {
		t.Errorf("server received memories = %+v, want one memory with ID mem-1", gotBatch.Memories)
	}
}

func TestPostBatchServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	}))
	defer server.Close()

	err := postBatch(server.Client(), server.URL, &batchReq{})
	if err == nil {
		t.Fatal("postBatch() error = nil, want a non-nil error on a 5xx response")
	}

	if !strings.Contains(err.Error(), "500") {
		t.Errorf("postBatch() error = %q, want it to mention the 500 status", err.Error())
	}

	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("postBatch() error = %q, want it to include the response body", err.Error())
	}
}

// TestRunInjectionStopsAtTarget checks the loop stops once totalBodyBytes reaches targetBytes,
// and that every posted batch (except possibly the last) is capped at batchMemories.
func TestRunInjectionStopsAtTarget(t *testing.T) {
	rng := rand.New(rand.NewSource(11))
	now := time.Now()

	var posted []batchReq

	post := func(b *batchReq) error {
		// Copy out the batch contents; runInjection reuses/resets the struct after each flush.
		cp := batchReq{
			Events:   append([]jsonEvent(nil), b.Events...),
			Memories: append([]jsonMemory(nil), b.Memories...),
		}
		posted = append(posted, cp)

		return nil
	}

	const batchMemories = 20

	result, err := runInjection(rng, now, 5000, batchMemories, post, nil)
	if err != nil {
		t.Fatalf("runInjection() error = %v, want nil", err)
	}

	if result.totalBodyBytes < 5000 {
		t.Errorf("runInjection() totalBodyBytes = %d, want >= target 5000", result.totalBodyBytes)
	}

	if len(posted) == 0 {
		t.Fatal("runInjection() never called post")
	}

	var sumMemories int

	for i, b := range posted {
		sumMemories += len(b.Memories)

		if i < len(posted)-1 && len(b.Memories) > batchMemories {
			// Every batch but possibly the last must respect the cap; the last can be smaller
			// (the tail) but never larger, since flush() fires as soon as the cap is reached.
			t.Errorf("batch[%d] has %d memories, want <= %d", i, len(b.Memories), batchMemories)
		}
	}

	if sumMemories != result.memoryCount {
		t.Errorf("posted memories summed = %d, want result.memoryCount %d", sumMemories, result.memoryCount)
	}
}

// TestRunInjectionZeroTarget checks a non-positive target skips the loop entirely and never
// calls post, since flush() is a no-op on an empty batch.
func TestRunInjectionZeroTarget(t *testing.T) {
	rng := rand.New(rand.NewSource(1))

	called := false

	post := func(b *batchReq) error {
		called = true

		return nil
	}

	result, err := runInjection(rng, time.Now(), 0, 400, post, nil)
	if err != nil {
		t.Fatalf("runInjection() error = %v, want nil", err)
	}

	if called {
		t.Error("runInjection() called post for a zero-byte target, want no calls")
	}

	if result.eventCount != 0 || result.memoryCount != 0 || result.totalBodyBytes != 0 {
		t.Errorf("runInjection() result = %+v, want all zero", result)
	}
}

// TestRunInjectionPostError checks a post failure aborts the loop immediately and the error
// propagates to the caller, matching main()'s "batch failed" exit path.
func TestRunInjectionPostError(t *testing.T) {
	rng := rand.New(rand.NewSource(1))

	wantErr := errors.New("boom")
	callCount := 0

	post := func(b *batchReq) error {
		callCount++

		return wantErr
	}

	_, err := runInjection(rng, time.Now(), 100_000_000, 10, post, nil)
	if err == nil {
		t.Fatal("runInjection() error = nil, want the post error propagated")
	}

	if !errors.Is(err, wantErr) {
		t.Errorf("runInjection() error = %v, want %v", err, wantErr)
	}

	if callCount != 1 {
		t.Errorf("post was called %d times, want exactly 1 (loop should abort on first failure)", callCount)
	}
}

// TestRunInjectionProgress checks the progress callback fires once memoryCount crosses a
// multiple-of-20000 boundary (the same "roughly every 20000 memories" gate the inline printf
// used before extraction) and reports monotonically increasing counts. batchMemories=500 evenly
// divides 20000, so a flush lands on memoryCount==20000 exactly (remainder 0), guaranteeing the
// gate fires without needing an enormous corpus.
func TestRunInjectionProgress(t *testing.T) {
	rng := rand.New(rand.NewSource(1))

	post := func(b *batchReq) error { return nil }

	var progressCalls int

	var lastMemoryCount int

	progress := func(eventCount int, memoryCount int, totalBodyBytes int64) {
		progressCalls++

		if memoryCount <= lastMemoryCount {
			t.Errorf("progress() memoryCount = %d, want > previous %d", memoryCount, lastMemoryCount)
		}

		lastMemoryCount = memoryCount
	}

	if _, err := runInjection(rng, time.Now(), 20_000_000, 500, post, progress); err != nil {
		t.Fatalf("runInjection() error = %v, want nil", err)
	}

	if progressCalls == 0 {
		t.Error("runInjection() never invoked the progress callback")
	}
}

// TestRunInjectionFinalFlushError checks a post failure on the trailing flush (the one after
// the loop exits, covering whatever didn't reach batchMemories) also propagates, not just
// mid-loop flush failures. A large batchMemories relative to targetBytes ensures the loop body
// runs to completion without ever tripping the mid-loop flush, so only the final flush executes.
func TestRunInjectionFinalFlushError(t *testing.T) {
	rng := rand.New(rand.NewSource(1))

	wantErr := errors.New("final flush boom")
	callCount := 0

	post := func(b *batchReq) error {
		callCount++

		return wantErr
	}

	_, err := runInjection(rng, time.Now(), 100, 1_000_000, post, nil)
	if err == nil {
		t.Fatal("runInjection() error = nil, want the final flush error propagated")
	}

	if !errors.Is(err, wantErr) {
		t.Errorf("runInjection() error = %v, want %v", err, wantErr)
	}

	if callCount != 1 {
		t.Errorf("post was called %d times, want exactly 1 (only the trailing flush)", callCount)
	}
}

// TestRunInjectionNilProgress checks a nil progress callback is tolerated (main always supplies
// one, but the parameter is optional by design).
func TestRunInjectionNilProgress(t *testing.T) {
	rng := rand.New(rand.NewSource(1))

	post := func(b *batchReq) error { return nil }

	if _, err := runInjection(rng, time.Now(), 10_000, 5, post, nil); err != nil {
		t.Fatalf("runInjection() error = %v, want nil", err)
	}
}

func TestPostBatchNetworkError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	url := server.URL
	server.Close() // Nothing is listening any more.

	err := postBatch(http.DefaultClient, url, &batchReq{})
	if err == nil {
		t.Fatal("postBatch() error = nil, want a non-nil error when the server is unreachable")
	}
}
