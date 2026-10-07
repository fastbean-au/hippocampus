package hippocampus

import (
	"bytes"
	"context"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/fastbean-au/hippocampus/types"
)

// List and Delete make the test's in-memory object store a Pruner, as both real stores are.
func (f *fakeObjectStore) List(_ context.Context, prefix string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	var keys []string

	for key := range f.objects {
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}

	sort.Strings(keys)

	return keys, nil
}

func (f *fakeObjectStore) Delete(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	delete(f.objects, key)

	return nil
}

func (f *fakeObjectStore) keys() []string {
	keys, _ := f.List(context.Background(), "")

	return keys
}

func scheduledExportServer(t *testing.T) (*Server, *fakeObjectStore) {
	t.Helper()

	objects := newFakeObjectStore()

	s := newTestServer(t)
	s.objects = objects
	s.consolidationEnabled = true
	s.scheduledExport = ScheduledExportConfig{Interval: 24 * time.Hour, Keep: 2}

	if _, err := s.db.CreateMemory(context.Background(), types.Memory{Id: "m1", Body: "kept", TimeStamp: time.Now().UnixNano(), Significance: 5}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	return s, objects
}

// TestRunScheduledExportWritesAndPrunes: one run writes a scheduled archive and keeps only the newest
// `keep` of them - never touching a manual export, or a key under the prefix this schedule did not
// write (TODO-3 item 158).
func TestRunScheduledExportWritesAndPrunes(t *testing.T) {
	s, objects := scheduledExportServer(t)
	ctx := context.Background()

	for _, key := range []string{
		"scheduled/20260101T000000Z.archive.gz",
		"scheduled/20260102T000000Z.archive.gz",
		"scheduled/20260103T000000Z.archive.gz",
		"scheduled/notes.txt",
		"20260101T000000Z-abcd1234.archive.gz",
	} {
		if err := objects.Put(ctx, key, bytes.NewReader([]byte("x"))); err != nil {
			t.Fatalf("Put: %s", err)
		}
	}

	if err := s.runScheduledExport(ctx, time.Date(2026, 10, 6, 1, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("runScheduledExport: %s", err)
	}

	want := []string{
		"20260101T000000Z-abcd1234.archive.gz",
		"scheduled/20260103T000000Z.archive.gz",
		"scheduled/20261006T010000Z.archive.gz",
		"scheduled/notes.txt",
	}

	if got := objects.keys(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("objects after the run:\n  %v\nwant\n  %v", got, want)
	}

	// The new archive is a real one: it imports back.
	fresh := newTestServer(t)
	fresh.objects = objects

	body, err := objects.Get(ctx, "scheduled/20261006T010000Z.archive.gz")
	if err != nil {
		t.Fatalf("Get: %s", err)
	}

	_, memories, err := fresh.importArchive(ctx, body)
	if err != nil || memories != 1 {
		t.Errorf("importing the scheduled archive: %d memories, %v; want 1 and no error", memories, err)
	}
}

// TestFirstScheduledExportWait: the schedule is read back from what the store holds, so a restart
// neither exports at once nor waits a full interval from scratch.
func TestFirstScheduledExportWait(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name   string
		newest string
		want   time.Duration
	}{
		{"nothing exported yet", "", 24 * time.Hour},
		{"the last export an hour ago", "scheduled/20261006T110000Z.archive.gz", 23 * time.Hour},
		{"the last export overdue", "scheduled/20261005T060000Z.archive.gz", 0},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, objects := scheduledExportServer(t)

			if c.newest != "" {
				_ = objects.Put(context.Background(), c.newest, bytes.NewReader([]byte("x")))
			}

			if got := s.firstScheduledExportWait(context.Background(), now); got != c.want {
				t.Errorf("first wait = %v, want %v", got, c.want)
			}
		})
	}
}

// TestScheduledExportLoopExportsAndStops: the loop takes an export on its own and Stop ends it.
func TestScheduledExportLoopExportsAndStops(t *testing.T) {
	s, objects := scheduledExportServer(t)
	s.scheduledExport.Interval = 20 * time.Millisecond

	s.stopScheduledExport = make(chan struct{})
	s.scheduledExportStopped = make(chan struct{})

	go s.scheduledExportLoop()

	deadline := time.Now().Add(5 * time.Second)

	for time.Now().Before(deadline) {
		if keys, _ := objects.List(context.Background(), "scheduled/"); len(keys) > 0 {
			break
		}

		time.Sleep(10 * time.Millisecond)
	}

	s.Stop()

	if keys, _ := objects.List(context.Background(), "scheduled/"); len(keys) == 0 {
		t.Error("the loop took no export")
	}
}

// brokenPruner is a store that writes but cannot list or delete, as a bucket with a write-only
// policy would be.
type brokenPruner struct {
	*fakeObjectStore
}

func (brokenPruner) List(context.Context, string) ([]string, error) {
	return nil, errors.New("access denied")
}

func (brokenPruner) Delete(context.Context, string) error {
	return errors.New("access denied")
}

// TestStartScheduledExportRunsOnlyWhereItShould: the export starts only when configured, with
// somewhere to write, on the consolidating instance.
func TestStartScheduledExportRunsOnlyWhereItShould(t *testing.T) {
	cases := []struct {
		name          string
		interval      int
		objects       bool
		consolidating bool
		wantRunning   bool
	}{
		{"not configured", 0, true, true, false},
		{"nowhere to write", 24, false, true, false},
		{"a replica", 24, true, false, false},
		{"the consolidating instance", 24, true, true, true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newTestServer(t)
			s.consolidationEnabled = c.consolidating
			s.scheduledExport.Interval = time.Duration(c.interval) * time.Hour

			if c.objects {
				s.objects = newFakeObjectStore()
			}

			s.startScheduledExport()
			t.Cleanup(s.Stop)

			if running := s.stopScheduledExport != nil; running != c.wantRunning {
				t.Errorf("running = %t, want %t", running, c.wantRunning)
			}
		})
	}
}

// TestScheduledExportSurvivesAStoreThatCannotList: listing failing is not fatal - the first export is
// scheduled one interval out, and a prune that cannot delete leaves the new archive written.
func TestScheduledExportSurvivesAStoreThatCannotList(t *testing.T) {
	s, objects := scheduledExportServer(t)
	s.objects = brokenPruner{objects}

	if got := s.firstScheduledExportWait(context.Background(), time.Now()); got != s.scheduledExport.Interval {
		t.Errorf("first wait with a store that cannot list = %v, want the interval", got)
	}

	if err := s.runScheduledExport(context.Background(), time.Now()); err != nil {
		t.Fatalf("runScheduledExport: %s", err)
	}

	if len(objects.keys()) != 1 {
		t.Error("the export was not written when only pruning failed")
	}

	if err := s.pruneScheduledExports(context.Background()); err == nil {
		t.Error("a prune against a store that cannot list reported success")
	}
}

// TestScheduledExportRetriesAFailure: a failed export is reported and the loop carries on to retry,
// rather than exiting or waiting a full interval.
func TestScheduledExportRetriesAFailure(t *testing.T) {
	s, _ := scheduledExportServer(t)
	s.objects = failPutObjectStore{}

	if err := s.runScheduledExport(context.Background(), time.Now()); err == nil {
		t.Fatal("an export whose upload failed reported success")
	}

	s.scheduledExport.Interval = 10 * time.Millisecond
	s.stopScheduledExport = make(chan struct{})
	s.scheduledExportStopped = make(chan struct{})

	go s.scheduledExportLoop()

	time.Sleep(50 * time.Millisecond)

	s.Stop()
}

// TestPruneKeepsEverythingWhenKeepIsZero: keep 0 is "keep them all", for an operator whose bucket has
// lifecycle rules of its own.
func TestPruneKeepsEverythingWhenKeepIsZero(t *testing.T) {
	s, objects := scheduledExportServer(t)
	s.scheduledExport.Keep = 0

	for _, key := range []string{"scheduled/20260101T000000Z.archive.gz", "scheduled/20260102T000000Z.archive.gz"} {
		_ = objects.Put(context.Background(), key, bytes.NewReader([]byte("x")))
	}

	if err := s.pruneScheduledExports(context.Background()); err != nil || len(objects.keys()) != 2 {
		t.Errorf("keep 0 pruned to %d (%v), want both kept", len(objects.keys()), err)
	}
}
