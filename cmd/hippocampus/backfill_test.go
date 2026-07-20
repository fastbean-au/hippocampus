package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	log "github.com/sirupsen/logrus"

	"github.com/fastbean-au/hippocampus/db"
	"github.com/fastbean-au/hippocampus/search"
	"github.com/fastbean-au/hippocampus/types"
)

// fakeOpenSearchServer stands in for a real OpenSearch cluster: it accepts every request and
// answers with a bare `{}` (200 OK), which is enough for opensearch-go's client to consider the
// index-exists check, mapping PUT, delete-index, and document-index calls all successful. It
// records every request path+method so tests can assert on what backfillSearch actually sent.
type fakeOpenSearchServer struct {
	mu       sync.Mutex
	requests []string
}

func (f *fakeOpenSearchServer) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			_, _ = io.ReadAll(r.Body)
		}

		f.mu.Lock()
		f.requests = append(f.requests, r.Method+" "+r.URL.Path)
		f.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)

		_, _ = w.Write([]byte(`{}`))
	}
}

func (f *fakeOpenSearchServer) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make([]string, len(f.requests))
	copy(out, f.requests)

	return out
}

func (f *fakeOpenSearchServer) count(substr string) int {
	n := 0

	for _, v := range f.recorded() {
		if strings.Contains(v, substr) {
			n++
		}
	}

	return n
}

// seedSQLiteFixture creates a directory-backed sqlite database (backfillSearch's read-only open
// requires a real file, unlike the in-memory test mode) with count non-binary memories, then
// closes it so the read-only reopen in backfillSearch does not contend with a live writer.
func seedSQLiteFixture(t *testing.T, count int) string {
	t.Helper()

	dir := t.TempDir()

	database, err := db.New(dir)
	if err != nil {
		t.Fatalf("db.New: %s", err)
	}

	for i := range count {
		_, err := database.CreateMemory(context.Background(), types.Memory{
			Id:        fmt.Sprintf("m%02d", i),
			TimeStamp: int64(i + 1),
			Body:      fmt.Sprintf("memory body %d", i),
		})
		if err != nil {
			t.Fatalf("CreateMemory: %s", err)
		}
	}

	if err := database.Close(); err != nil {
		t.Fatalf("closing seed database: %s", err)
	}

	return dir
}

// TestBackfillSearch_HappyPath drives backfillSearch's full real loop against a small sqlite
// fixture and a fake OpenSearch server: it exercises the Reindex branch (delete+recreate the
// index before backfilling) and the keyset pagination loop (a batch size smaller than the fixture
// forces more than one GetIndexableMemoriesPage round trip).
func TestBackfillSearch_HappyPath(t *testing.T) {
	const memoryCount = 5

	dir := seedSQLiteFixture(t, memoryCount)

	fake := &fakeOpenSearchServer{}
	server := httptest.NewServer(fake.handler())
	t.Cleanup(server.Close)

	backfillSearch(backfillConfig{
		StorageDriver:    "sqlite",
		StorageDirectory: dir,
		Search: search.Config{
			Addresses: []string{server.URL},
			Index:     "test-index",
			QueueSize: 16,
		},
		Reindex:   true,
		BatchSize: 2, // smaller than memoryCount: forces >1 GetIndexableMemoriesPage batch
	})

	if got := fake.count("/_doc/"); got != memoryCount {
		t.Errorf("expected %d document index requests (one per memory), got %d: %v", memoryCount, got, fake.recorded())
	}

	// Reindex: true must delete the index before recreating/backfilling it.
	if got := fake.count("DELETE /test-index"); got == 0 {
		t.Errorf("expected a DELETE against the index for --reindex, got requests: %v", fake.recorded())
	}
}

// withFatalPanic overrides the standard logrus logger's ExitFunc so a log.Fatalf inside fn panics
// instead of calling os.Exit (which would kill the test process), then runs fn and asserts it
// panicked via that override, restoring the original ExitFunc afterwards. This is the standard
// logrus testing trick for covering log.Fatalf branches in-process.
func withFatalPanic(t *testing.T, fn func()) {
	t.Helper()

	type fatalExit struct{ code int }

	orig := log.StandardLogger().ExitFunc
	log.StandardLogger().ExitFunc = func(code int) { panic(fatalExit{code}) }
	t.Cleanup(func() { log.StandardLogger().ExitFunc = orig })

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected a log.Fatalf call (recovered panic), got none")
		}

		if _, ok := r.(fatalExit); !ok {
			panic(r) // an unexpected panic - not the one we're trapping - must still surface
		}
	}()

	fn()
}

// TestBackfillSearch_UnknownStorageDriver verifies an unrecognised storage.driver fails fast via
// log.Fatalf rather than silently doing nothing.
func TestBackfillSearch_UnknownStorageDriver(t *testing.T) {
	withFatalPanic(t, func() {
		backfillSearch(backfillConfig{StorageDriver: "bogus"})
	})
}

// TestBackfillSearch_DBOpenFailure verifies a database that cannot be opened (a sqlite directory
// with no existing database file - NewSQLiteReadOnly cannot create one) fails fast via log.Fatalf.
func TestBackfillSearch_DBOpenFailure(t *testing.T) {
	withFatalPanic(t, func() {
		backfillSearch(backfillConfig{
			StorageDriver:    "sqlite",
			StorageDirectory: t.TempDir(), // empty: no hippocampus.db file for the read-only open to find
		})
	})
}

// failingOpenSearchServer behaves like fakeOpenSearchServer (200 "{}" for everything) except that
// any request whose "METHOD /path" contains failSubstr gets a 500, letting a test force one
// specific opensearch-go call (e.g. the index-delete behind --reindex, or a document PUT) to fail
// without standing up a real cluster that can be made to misbehave.
type failingOpenSearchServer struct {
	failSubstr string
}

func (f *failingOpenSearchServer) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			_, _ = io.ReadAll(r.Body)
		}

		if strings.Contains(r.Method+" "+r.URL.Path, f.failSubstr) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"reason":"injected failure"}}`))

			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}
}

// TestBackfillSearch_PostgresDriverOpenFailure and TestBackfillSearch_MySQLDriverOpenFailure cover
// the postgres/mysql branches of backfillSearch's storage.driver switch: both dial a DSN nothing is
// listening on, so the read-only open fails fast (connection refused) and the tool exits via
// log.Fatalf, exactly like the sqlite open-failure case already covered above.
func TestBackfillSearch_PostgresDriverOpenFailure(t *testing.T) {
	withFatalPanic(t, func() {
		backfillSearch(backfillConfig{
			StorageDriver: "postgres",
			PostgresDSN:   "postgres://bogus:bogus@127.0.0.1:1/bogus?sslmode=disable&connect_timeout=1",
		})
	})
}

func TestBackfillSearch_MySQLDriverOpenFailure(t *testing.T) {
	withFatalPanic(t, func() {
		backfillSearch(backfillConfig{
			StorageDriver: "mysql",
			MySQLDSN:      "bogus:bogus@tcp(127.0.0.1:1)/bogus?timeout=1s",
		})
	})
}

// TestBackfillSearch_OpenSearchInitFailure verifies that a malformed opensearch configuration (a
// client-certificate TLS block with only one of certFile/keyFile set) fails the tool at
// search.NewOpenSearch construction, before any database work happens.
func TestBackfillSearch_OpenSearchInitFailure(t *testing.T) {
	dir := seedSQLiteFixture(t, 1)

	withFatalPanic(t, func() {
		backfillSearch(backfillConfig{
			StorageDriver:    "sqlite",
			StorageDirectory: dir,
			Search: search.Config{
				TLS: search.TLSConfig{CertFile: "/cert.pem"}, // KeyFile missing: malformed pair
			},
		})
	})
}

// TestBackfillSearch_ReindexFailure verifies that when --reindex's index delete+recreate fails
// against the cluster, backfillSearch fails fast via log.Fatalf rather than silently indexing into
// a stale index.
func TestBackfillSearch_ReindexFailure(t *testing.T) {
	dir := seedSQLiteFixture(t, 1)

	fake := &failingOpenSearchServer{failSubstr: "DELETE"}
	server := httptest.NewServer(fake.handler())
	t.Cleanup(server.Close)

	withFatalPanic(t, func() {
		backfillSearch(backfillConfig{
			StorageDriver:    "sqlite",
			StorageDirectory: dir,
			Search: search.Config{
				Addresses: []string{server.URL},
				Index:     "test-index",
				QueueSize: 16,
			},
			Reindex:   true,
			BatchSize: 10,
		})
	})
}

// TestBackfillSearch_IndexMemoryFailure verifies that a failure indexing an individual memory
// (IndexMemorySync) fails the whole run via log.Fatalf - the documented behaviour, since the tool
// is safe to rerun from scratch and partial, unreported progress would be worse.
func TestBackfillSearch_IndexMemoryFailure(t *testing.T) {
	dir := seedSQLiteFixture(t, 1)

	fake := &failingOpenSearchServer{failSubstr: "/_doc/"}
	server := httptest.NewServer(fake.handler())
	t.Cleanup(server.Close)

	withFatalPanic(t, func() {
		backfillSearch(backfillConfig{
			StorageDriver:    "sqlite",
			StorageDirectory: dir,
			Search: search.Config{
				Addresses: []string{server.URL},
				Index:     "test-index",
				QueueSize: 16,
			},
			BatchSize: 10,
		})
	})
}
