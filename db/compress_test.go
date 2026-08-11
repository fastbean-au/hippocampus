package db

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/fastbean-au/hippocampus/types"
)

// compressibleBody returns a body large enough to clear the size threshold and repetitive enough
// that gzip is guaranteed to shrink it - so a test asserting "this got compressed" is asserting the
// wiring, not gambling on the compressor.
func compressibleBody() string {
	return strings.Repeat("the quick brown fox jumps over the lazy dog. ", 40)
}

// storedBody reads the raw body column and its is_compressed flag, bypassing the scanners'
// decompression - the only way to tell what actually landed in the row from what the read path
// hands back.
func storedBody(t *testing.T, d *DB, id string) ([]byte, bool) {
	t.Helper()

	var body []byte
	var isCompressed bool

	if err := d.queryRow(context.Background(), `SELECT body, is_compressed FROM memories WHERE id = ?`, id).
		Scan(&body, &isCompressed); err != nil {
		t.Fatalf("read stored body: %s", err)
	}

	return body, isCompressed
}

func TestGzipRoundTrip(t *testing.T) {
	cases := map[string]string{
		"empty":      "",
		"short":      "hello",
		"repetitive": compressibleBody(),
		"binary-ish": string([]byte{0x00, 0xff, 0x1f, 0x8b, 0x00, 0x42}),
		"utf8":       "naïve café — ünïcödé",
	}

	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			packed, err := gzipBytes([]byte(body))
			if err != nil {
				t.Fatalf("gzipBytes: %s", err)
			}

			got, err := gunzipBytes(packed)
			if err != nil {
				t.Fatalf("gunzipBytes: %s", err)
			}

			if string(got) != body {
				t.Errorf("round trip = %q, want %q", got, body)
			}
		})
	}
}

// failingWriter fails every write, standing in for the destination gzipBytes can never actually
// have: a bytes.Buffer write cannot fail, so this is the only way to exercise gzipTo's failure path.
type failingWriter struct{}

func (failingWriter) Write(p []byte) (int, error) {
	return 0, errors.New("boom")
}

func TestGzipToPropagatesWriteFailure(t *testing.T) {
	// A body large enough that the compressor flushes to the destination during Write rather than
	// only at Close, so both joined failures are exercised.
	if err := gzipTo(failingWriter{}, []byte(strings.Repeat("x", 1<<20))); err == nil {
		t.Error("expected the destination's write failure to surface")
	}
}

func TestGzipToPropagatesCloseFailure(t *testing.T) {
	// A tiny body is buffered entirely inside the compressor, so nothing reaches the destination
	// until Close flushes it - the other half of the joined error.
	if err := gzipTo(failingWriter{}, []byte("x")); err == nil {
		t.Error("expected the destination's failure at flush to surface")
	}
}

// TestGzipRoundTripIsConcurrencySafe exercises the pooled writers and readers from many goroutines
// at once, with differing bodies, so a compressor returned to the pool carrying state from its
// previous use would surface as a corrupted round trip rather than lying dormant. Run under -race
// it also covers the sharing itself.
func TestGzipRoundTripIsConcurrencySafe(t *testing.T) {
	const goroutines = 16

	var wg sync.WaitGroup

	errs := make(chan error, goroutines)

	for g := range goroutines {
		wg.Add(1)

		go func(g int) {
			defer wg.Done()

			// Distinct lengths and content per goroutine, so leaked state between pooled uses
			// cannot coincidentally produce the right answer.
			body := strings.Repeat(string(rune('a'+g))+"payload ", 20*(g+1))

			for range 50 {
				packed, err := gzipBytes([]byte(body))
				if err != nil {
					errs <- fmt.Errorf("goroutine %d: gzipBytes: %w", g, err)

					return
				}

				got, err := gunzipBytes(packed)
				if err != nil {
					errs <- fmt.Errorf("goroutine %d: gunzipBytes: %w", g, err)

					return
				}

				if string(got) != body {
					errs <- fmt.Errorf("goroutine %d: round trip returned %d bytes, want %d", g, len(got), len(body))

					return
				}
			}
		}(g)
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		t.Error(err)
	}
}

// TestGunzipBytesAfterFailedResetIsReusable pins that a reader returned to the pool after a failed
// Reset (a body that is not gzip at all) does not poison the next use of that pooled reader.
func TestGunzipBytesAfterFailedResetIsReusable(t *testing.T) {
	if _, err := gunzipBytes([]byte("not gzip at all")); err == nil {
		t.Fatal("expected an error")
	}

	body := compressibleBody()

	packed, err := gzipBytes([]byte(body))
	if err != nil {
		t.Fatalf("gzipBytes: %s", err)
	}

	got, err := gunzipBytes(packed)
	if err != nil {
		t.Fatalf("gunzipBytes after a failed reset: %s", err)
	}

	if string(got) != body {
		t.Error("a pooled reader reused after a failed reset returned the wrong body")
	}
}

func TestGunzipBytesRejectsNonGzip(t *testing.T) {
	if _, err := gunzipBytes([]byte("not gzip at all")); err == nil {
		t.Error("expected an error decompressing data that is not a gzip stream")
	}
}

// TestGunzipBytesRejectsTruncatedStream covers the read-side failure that a valid header cannot
// catch: the row's bytes start as a gzip stream but end early, so the failure surfaces mid-read
// rather than at the header.
func TestGunzipBytesRejectsTruncatedStream(t *testing.T) {
	packed, err := gzipBytes([]byte(compressibleBody()))
	if err != nil {
		t.Fatalf("gzipBytes: %s", err)
	}

	if _, err := gunzipBytes(packed[:len(packed)/2]); err == nil {
		t.Error("expected an error decompressing a truncated gzip stream")
	}
}

func TestDecompressBodyPassesThroughUncompressed(t *testing.T) {
	// The stored bytes are not a gzip stream, so a flag-blind implementation would fail here.
	got, err := decompressBody([]byte("plain body"), false)
	if err != nil {
		t.Fatalf("decompressBody: %s", err)
	}

	if got != "plain body" {
		t.Errorf("decompressBody = %q, want 'plain body'", got)
	}
}

func TestDecompressBodyReportsCorruptRow(t *testing.T) {
	if _, err := decompressBody([]byte("truncated"), true); err == nil {
		t.Error("expected an error when a row flagged compressed does not hold a gzip stream")
	}
}

func TestSetCompression(t *testing.T) {
	cases := []struct {
		name         string
		enabled      bool
		minBytes     int
		wantEnabled  bool
		wantMinBytes int
	}{
		{"disabled", false, 1024, false, 0},
		{"explicit threshold", true, 2048, true, 2048},
		{"zero takes the default", true, 0, true, defaultCompressionMinBytes},
		{"negative takes the default", true, -1, true, defaultCompressionMinBytes},
		{"below the floor is raised", true, 8, true, compressionMinBytesFloor},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := &DB{}
			d.SetCompression(c.enabled, c.minBytes)

			if d.compression.enabled != c.wantEnabled {
				t.Errorf("enabled = %t, want %t", d.compression.enabled, c.wantEnabled)
			}

			if d.compression.minBytes != c.wantMinBytes {
				t.Errorf("minBytes = %d, want %d", d.compression.minBytes, c.wantMinBytes)
			}
		})
	}
}

// TestSetCompressionDisabledClearsPolicy covers the toggle-off path leaving no threshold behind, so
// a later re-enable cannot inherit a stale one.
func TestSetCompressionDisabledClearsPolicy(t *testing.T) {
	d := &DB{}

	d.SetCompression(true, 4096)
	d.SetCompression(false, 0)

	if d.compression.enabled || d.compression.minBytes != 0 {
		t.Errorf("disabling left %+v, want the zero policy", d.compression)
	}
}

func TestCompressBodyPolicy(t *testing.T) {
	long := compressibleBody()

	cases := []struct {
		name           string
		enabled        bool
		minBytes       int
		body           string
		isBinary       bool
		wantCompressed bool
	}{
		{"disabled stores verbatim", false, 64, long, false, false},
		{"binary is never compressed", true, 64, long, true, false},
		{"below the threshold", true, 1 << 20, long, false, false},
		{"compressible body", true, 64, long, false, true},
		{"empty body", true, 64, "", false, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := &DB{}
			d.SetCompression(c.enabled, c.minBytes)

			stored, isCompressed := d.compressBody(c.body, c.isBinary)

			if isCompressed != c.wantCompressed {
				t.Fatalf("compressed = %t, want %t", isCompressed, c.wantCompressed)
			}

			// Whatever the decision, the stored bytes must read back as the original body.
			got, err := decompressBody(stored, isCompressed)
			if err != nil {
				t.Fatalf("decompressBody: %s", err)
			}

			if got != c.body {
				t.Errorf("round trip through the policy changed the body")
			}

			if isCompressed && len(stored) >= len(c.body) {
				t.Errorf("stored %d bytes for a %d byte body: compression must never be kept when it does not shrink the body", len(stored), len(c.body))
			}
		})
	}
}

// TestCompressBodyKeepsIncompressibleBodyVerbatim is the guard that makes compression safe on
// content gzip cannot shrink: the body is stored as-is rather than stored larger than it arrived.
func TestCompressBodyKeepsIncompressibleBodyVerbatim(t *testing.T) {
	d := &DB{}
	d.SetCompression(true, compressionMinBytesFloor)

	// A gzip stream of random-ish content: already compressed, so compressing it again grows it.
	packed, err := gzipBytes([]byte(compressibleBody()))
	if err != nil {
		t.Fatalf("gzipBytes: %s", err)
	}

	body := string(packed)

	stored, isCompressed := d.compressBody(body, false)

	if isCompressed {
		t.Error("an incompressible body must be stored verbatim")
	}

	if string(stored) != body {
		t.Error("the verbatim path altered the body")
	}
}

// --- storage round trips, against a real (in-memory) SQLite database ---

func newCompressingTestDB(t *testing.T) *DB {
	t.Helper()

	d := newTestDB(t)
	d.SetCompression(true, compressionMinBytesFloor)

	return d
}

func TestCreateMemoryCompressesBody(t *testing.T) {
	d := newCompressingTestDB(t)
	body := compressibleBody()

	if _, err := d.CreateMemory(context.Background(), types.Memory{Id: "m1", TimeStamp: 100, Significance: 5, Body: body}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	stored, isCompressed := storedBody(t, d, "m1")

	if !isCompressed {
		t.Fatal("the row was not flagged compressed")
	}

	if len(stored) >= len(body) {
		t.Errorf("stored %d bytes for a %d byte body", len(stored), len(body))
	}

	memories, err := d.GetMemoriesByIds(context.Background(), []string{"m1"})
	if err != nil {
		t.Fatalf("GetMemoriesByIds: %s", err)
	}

	if len(*memories) != 1 || (*memories)[0].Body != body {
		t.Error("the body did not survive the round trip through storage")
	}
}

func TestCreateMemoryLeavesBinaryBodyUncompressed(t *testing.T) {
	d := newCompressingTestDB(t)
	body := compressibleBody()

	if _, err := d.CreateMemory(context.Background(), types.Memory{Id: "m1", TimeStamp: 100, Significance: 5, Body: body, IsBinary: true}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	if stored, isCompressed := storedBody(t, d, "m1"); isCompressed || string(stored) != body {
		t.Error("a binary memory's body must be stored verbatim")
	}
}

// TestRecallMemoriesDecompresses covers the RETURNING-based read path (scanMemoryStored), which is
// a separate scanner from the joined-view one every other read uses.
func TestRecallMemoriesDecompresses(t *testing.T) {
	d := newCompressingTestDB(t)
	body := compressibleBody()

	if _, err := d.CreateMemory(context.Background(), types.Memory{Id: "m1", TimeStamp: 100, Significance: 5, Body: body}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	memories, err := d.RecallMemories(context.Background(), []string{"m1"})
	if err != nil {
		t.Fatalf("RecallMemories: %s", err)
	}

	if len(*memories) != 1 || (*memories)[0].Body != body {
		t.Error("RecallMemories did not return the original body")
	}
}

// TestCompressedRowsReadableAfterCompressionDisabled is the guarantee that makes the setting safe to
// change on a live store: reads follow the row's own flag, never the current configuration.
func TestCompressedRowsReadableAfterCompressionDisabled(t *testing.T) {
	d := newCompressingTestDB(t)
	body := compressibleBody()

	if _, err := d.CreateMemory(context.Background(), types.Memory{Id: "m1", TimeStamp: 100, Significance: 5, Body: body}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	d.SetCompression(false, 0)

	memories, err := d.GetMemoriesByIds(context.Background(), []string{"m1"})
	if err != nil {
		t.Fatalf("GetMemoriesByIds: %s", err)
	}

	if len(*memories) != 1 || (*memories)[0].Body != body {
		t.Error("a compressed row must stay readable after compression is disabled")
	}

	// A second memory written while disabled is stored verbatim, so the store now holds a mix -
	// and both must read back correctly.
	if _, err := d.CreateMemory(context.Background(), types.Memory{Id: "m2", TimeStamp: 200, Significance: 5, Body: body}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	if _, isCompressed := storedBody(t, d, "m2"); isCompressed {
		t.Error("a memory written with compression disabled must not be flagged compressed")
	}

	mixed, err := d.GetMemoriesByIds(context.Background(), []string{"m1", "m2"})
	if err != nil {
		t.Fatalf("GetMemoriesByIds: %s", err)
	}

	if len(*mixed) != 2 {
		t.Fatalf("got %d memories, want 2", len(*mixed))
	}

	for _, memory := range *mixed {
		if memory.Body != body {
			t.Errorf("memory %q did not round trip", memory.Id)
		}
	}
}

// TestUpdateMemoryRewritesCompressionFlag covers both directions: the flag must always describe the
// body currently in the row, never the one it replaced.
func TestUpdateMemoryRewritesCompressionFlag(t *testing.T) {
	d := newCompressingTestDB(t)
	body := compressibleBody()

	if _, err := d.CreateMemory(context.Background(), types.Memory{Id: "m1", TimeStamp: 100, Significance: 5, Body: body}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	// Compression off: the replacement body must be stored verbatim AND the flag cleared, or the
	// read path would try to gunzip a plain body.
	d.SetCompression(false, 0)

	if ok, err := d.UpdateMemory(context.Background(), types.Memory{Id: "m1", Body: "a short plain body"}); err != nil {
		t.Fatalf("UpdateMemory: %s", err)
	} else if !ok {
		t.Fatal("UpdateMemory reported the existing memory as missing")
	}

	if _, isCompressed := storedBody(t, d, "m1"); isCompressed {
		t.Error("the compression flag was left set over an uncompressed body")
	}

	memories, err := d.GetMemoriesByIds(context.Background(), []string{"m1"})
	if err != nil {
		t.Fatalf("GetMemoriesByIds: %s", err)
	}

	if (*memories)[0].Body != "a short plain body" {
		t.Errorf("body = %q, want the updated body", (*memories)[0].Body)
	}

	// And back the other way.
	d.SetCompression(true, compressionMinBytesFloor)

	if ok, err := d.UpdateMemory(context.Background(), types.Memory{Id: "m1", Body: body}); err != nil {
		t.Fatalf("UpdateMemory: %s", err)
	} else if !ok {
		t.Fatal("UpdateMemory reported the existing memory as missing")
	}

	if _, isCompressed := storedBody(t, d, "m1"); !isCompressed {
		t.Error("the updated body was not compressed")
	}
}

// TestUpdateMemoryTakesIsBinaryFromTheStoredRow pins the reason UpdateMemory probes for is_binary:
// it is outside the partial-update surface, so the caller's copy of it cannot be trusted to decide
// whether the new body may be compressed.
func TestUpdateMemoryTakesIsBinaryFromTheStoredRow(t *testing.T) {
	d := newCompressingTestDB(t)
	body := compressibleBody()

	if _, err := d.CreateMemory(context.Background(), types.Memory{Id: "m1", TimeStamp: 100, Significance: 5, Body: "x", IsBinary: true}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	// IsBinary is deliberately left false on the update, as an ordinary partial update would.
	if ok, err := d.UpdateMemory(context.Background(), types.Memory{Id: "m1", Body: body}); err != nil {
		t.Fatalf("UpdateMemory: %s", err)
	} else if !ok {
		t.Fatal("UpdateMemory reported the existing memory as missing")
	}

	if _, isCompressed := storedBody(t, d, "m1"); isCompressed {
		t.Error("a binary memory's body was compressed on update")
	}
}

func TestUpdateMemoryMissingRowWithBody(t *testing.T) {
	d := newCompressingTestDB(t)

	ok, err := d.UpdateMemory(context.Background(), types.Memory{Id: "nope", Body: compressibleBody()})
	if err != nil {
		t.Fatalf("UpdateMemory: %s", err)
	}

	if ok {
		t.Error("UpdateMemory reported a missing memory as existing")
	}
}

// TestScanMemoryReportsUndecompressableRow covers the read-side corruption path through the joined-
// view scanner: a row flagged compressed whose body is not a gzip stream must fail the read rather
// than hand back the raw bytes as if they were the body.
func TestScanMemoryReportsUndecompressableRow(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectQuery(`FROM .* WHERE id IN`).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "timestamp", "significance", "event_id", "body",
			"is_binary", "time_recalled", "recall_count", "is_summary", "group_name", "is_compressed",
		}).AddRow("m1", int64(10), int32(5), "", []byte("not a gzip stream"), false, int64(0), int32(0), false, "", true))

	if _, err := d.GetMemoriesByIds(context.Background(), []string{"m1"}); err == nil {
		t.Fatal("expected an error reading a row whose compressed body cannot be decompressed")
	}

	expectationsMet(t, mock)
}

// TestScanMemoryStoredReportsUndecompressableRow is the same case through the RETURNING scanner,
// which is a separate code path from the one above.
func TestScanMemoryStoredReportsUndecompressableRow(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectQuery(`UPDATE memories SET time_recalled`).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "timestamp", "significance_level_id", "event_id", "body",
			"is_binary", "time_recalled", "recall_count", "is_summary", "group_name", "is_compressed",
		}).AddRow("m1", int64(10), nil, "", []byte("not a gzip stream"), false, int64(0), int32(0), false, "", true))

	if _, err := d.RecallMemories(context.Background(), []string{"m1"}); err == nil {
		t.Fatal("expected an error reading a row whose compressed body cannot be decompressed")
	}

	expectationsMet(t, mock)
}

// TestUpdateMemoryPropagatesIsBinaryProbeFailure covers the probe's error path: a database failure
// while deciding how to store the new body must fail the update rather than fall back to a guess
// about whether the memory is binary.
func TestUpdateMemoryPropagatesIsBinaryProbeFailure(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)
	d.SetCompression(true, compressionMinBytesFloor)

	mock.ExpectQuery(`SELECT is_binary FROM memories`).WillReturnError(errors.New("boom"))

	if _, err := d.UpdateMemory(context.Background(), types.Memory{Id: "m1", Body: compressibleBody()}); err == nil {
		t.Fatal("expected an error")
	}

	expectationsMet(t, mock)
}

func TestReplaceMemoriesWithSummaryCompresses(t *testing.T) {
	d := newCompressingTestDB(t)
	body := compressibleBody()

	if _, err := d.CreateEvent(context.Background(), types.Event{Id: "e1", Name: "event", TimeStart: 1, Significance: 5}); err != nil {
		t.Fatalf("CreateEvent: %s", err)
	}

	if _, err := d.CreateMemory(context.Background(), types.Memory{Id: "m1", TimeStamp: 100, Significance: 5, EventId: "e1", Body: "one"}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	summary := types.Memory{Id: "s1", TimeStamp: 200, Significance: 6, EventId: "e1", Body: body, IsSummary: true}

	if _, err := d.ReplaceMemoriesWithSummary(context.Background(), "e1", summary); err != nil {
		t.Fatalf("ReplaceMemoriesWithSummary: %s", err)
	}

	if _, isCompressed := storedBody(t, d, "s1"); !isCompressed {
		t.Error("the summary body was not compressed")
	}

	memories, err := d.GetMemoriesByEventId(context.Background(), "e1")
	if err != nil {
		t.Fatalf("GetMemoriesByEventId: %s", err)
	}

	if len(*memories) != 1 || (*memories)[0].Body != body {
		t.Error("the summary body did not survive the round trip")
	}
}

// TestImportMemoriesAppliesLocalCompressionPolicy pins that the archive carries plain bodies: an
// import takes the receiving instance's policy, so a store with compression on compresses what it
// receives regardless of how the sending instance stored it.
func TestImportMemoriesAppliesLocalCompressionPolicy(t *testing.T) {
	d := newCompressingTestDB(t)
	body := compressibleBody()

	if _, err := d.ImportMemories(context.Background(), []types.Memory{
		{Id: "m1", TimeStamp: 100, Significance: 5, Body: body},
		{Id: "m2", TimeStamp: 200, Significance: 5, Body: body, IsBinary: true},
	}); err != nil {
		t.Fatalf("ImportMemories: %s", err)
	}

	if _, isCompressed := storedBody(t, d, "m1"); !isCompressed {
		t.Error("an imported body was not compressed")
	}

	if _, isCompressed := storedBody(t, d, "m2"); isCompressed {
		t.Error("an imported binary body must be stored verbatim")
	}

	// Re-importing the same rows must stay idempotent with compression in play.
	if _, err := d.ImportMemories(context.Background(), []types.Memory{{Id: "m1", TimeStamp: 100, Significance: 5, Body: body}}); err != nil {
		t.Fatalf("ImportMemories (repeat): %s", err)
	}

	memories, err := d.GetMemoriesPage(context.Background(), "", 10, nil)
	if err != nil {
		t.Fatalf("GetMemoriesPage: %s", err)
	}

	if len(memories) != 2 {
		t.Fatalf("got %d memories, want 2", len(memories))
	}

	for _, memory := range memories {
		if memory.Body != body {
			t.Errorf("memory %q did not round trip", memory.Id)
		}
	}
}

// TestIsCompressedColumnIsAddedToAnExistingDatabase covers the in-place migration: a database file
// written by a version of the service that predates compression must gain the column on the next
// startup, with its existing rows reading exactly as before.
func TestIsCompressedColumnIsAddedToAnExistingDatabase(t *testing.T) {
	directory := t.TempDir()

	d, err := New(directory)
	if err != nil {
		t.Fatalf("New: %s", err)
	}

	if _, err := d.CreateMemory(context.Background(), types.Memory{Id: "m1", TimeStamp: 100, Significance: 5, Body: "written before compression existed"}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	// Drop the column back off, reproducing the older schema around the existing row.
	if _, err := d.sql.Exec(`ALTER TABLE memories DROP COLUMN is_compressed`); err != nil {
		t.Fatalf("drop is_compressed: %s", err)
	}

	if err := d.Close(); err != nil {
		t.Fatalf("Close: %s", err)
	}

	reopened, err := New(directory)
	if err != nil {
		t.Fatalf("reopen: %s", err)
	}

	t.Cleanup(func() { _ = reopened.Close() })

	memories, err := reopened.GetMemoriesByIds(context.Background(), []string{"m1"})
	if err != nil {
		t.Fatalf("GetMemoriesByIds after migration: %s", err)
	}

	if len(*memories) != 1 || (*memories)[0].Body != "written before compression existed" {
		t.Error("a row written before the column existed did not survive the migration")
	}

	if _, isCompressed := storedBody(t, reopened, "m1"); isCompressed {
		t.Error("a pre-existing row must migrate as uncompressed")
	}
}

// TestUsedBytesReflectsCompressedSize pins the interaction with the capacity target: the store
// accounts for what it actually holds, so compression translates directly into headroom.
func TestUsedBytesReflectsCompressedSize(t *testing.T) {
	body := strings.Repeat("compress me. ", 4000)

	measure := func(compressed bool) int64 {
		d := newTestDB(t)
		d.SetCompression(compressed, compressionMinBytesFloor)

		for _, id := range []string{"m1", "m2", "m3"} {
			if _, err := d.CreateMemory(context.Background(), types.Memory{Id: id, TimeStamp: 100, Significance: 5, Body: body}); err != nil {
				t.Fatalf("CreateMemory: %s", err)
			}
		}

		used, err := d.UsedBytes(context.Background())
		if err != nil {
			t.Fatalf("UsedBytes: %s", err)
		}

		return used
	}

	plain := measure(false)
	compressed := measure(true)

	if compressed >= plain {
		t.Errorf("UsedBytes with compression = %d, without = %d: compression must reduce the accounted size", compressed, plain)
	}
}
