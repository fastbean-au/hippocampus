package archive

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNewFileStore_RequiresADirectory verifies construction fails fast with nothing configured,
// since every Put/Get is directory-scoped.
func TestNewFileStore_RequiresADirectory(t *testing.T) {
	if _, err := NewFileStore(""); err == nil {
		t.Error("expected an error when archive.directory is unset")
	}
}

// TestNewFileStore_CreatesTheDirectory verifies an operator may name a directory that does not
// exist yet, rather than discovering on their first export that they had to create it.
func TestNewFileStore_CreatesTheDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "archives", "nested")

	store, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %s", err)
	}

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("expected the directory to have been created: %s", err)
	}

	if !info.IsDir() {
		t.Error("expected a directory")
	}

	if !filepath.IsAbs(store.Directory()) {
		t.Errorf("expected an absolute directory, got %q", store.Directory())
	}

	var _ ObjectStore = store
}

// TestNewFileStore_RefusesAFile verifies a path that exists as a file is refused at startup rather
// than at the first export.
func TestNewFileStore_RefusesAFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-a-directory")

	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatalf("failed to write the fixture: %s", err)
	}

	if _, err := NewFileStore(path); err == nil {
		t.Error("expected an error when the archive directory is a file")
	}
}

// TestFileStore_RoundTrip is the contract Export/Import actually depend on: what Put wrote, Get
// returns, byte for byte, including through a key prefix's subdirectories.
func TestFileStore_RoundTrip(t *testing.T) {
	store := newTestFileStore(t)

	body := []byte("gzip-ish bytes \x00\x01\x02 and some text")

	if err := store.Put(context.Background(), "hippocampus/20260907T101112Z-abcd.archive.gz", bytes.NewReader(body)); err != nil {
		t.Fatalf("Put: %s", err)
	}

	reader, err := store.Get(context.Background(), "hippocampus/20260907T101112Z-abcd.archive.gz")
	if err != nil {
		t.Fatalf("Get: %s", err)
	}

	defer func() {
		_ = reader.Close()
	}()

	read, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("failed to read the object back: %s", err)
	}

	if !bytes.Equal(read, body) {
		t.Errorf("read back %q, expected %q", read, body)
	}
}

// TestFileStore_PutOverwrites verifies a repeated key replaces rather than appends or refuses -
// the same thing S3 does, and what makes a re-run of an export idempotent.
func TestFileStore_PutOverwrites(t *testing.T) {
	store := newTestFileStore(t)

	for _, body := range []string{"first", "second-and-longer"} {
		if err := store.Put(context.Background(), "archive.gz", strings.NewReader(body)); err != nil {
			t.Fatalf("Put: %s", err)
		}
	}

	read := mustGet(t, store, "archive.gz")
	if read != "second-and-longer" {
		t.Errorf("read back %q, expected the second write", read)
	}
}

// TestFileStore_GetMissing verifies a key nobody wrote is an error rather than an empty archive,
// which Import would otherwise read as a valid archive of nothing.
func TestFileStore_GetMissing(t *testing.T) {
	store := newTestFileStore(t)

	if _, err := store.Get(context.Background(), "never-written.gz"); err == nil {
		t.Error("expected an error for a key that was never written")
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected a not-exist error, got %s", err)
	}
}

// TestFileStore_RefusesEscapingKeys is the security property. Import takes its object_key straight
// from the request, so over a filesystem a key is an attacker-supplied path unless something stops
// it - and both directions matter: reading a file outside the directory, and writing one.
func TestFileStore_RefusesEscapingKeys(t *testing.T) {
	root := t.TempDir()

	outside := filepath.Join(root, "outside.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatalf("failed to write the fixture: %s", err)
	}

	store, err := NewFileStore(filepath.Join(root, "archives"))
	if err != nil {
		t.Fatalf("NewFileStore: %s", err)
	}

	// Refused rather than reinterpreted: a key naming something outside the directory must fail,
	// not quietly become a key naming something inside it.
	keys := []string{
		"",
		".",
		"/",
		"..",
		"../outside.txt",
		"../../etc/passwd",
		"nested/../../outside.txt",
		"/../outside.txt",
		"/hippocampus/x.gz",
		"nested//double.gz",
		"trailing/",
		"..\\outside.txt",
		"nul\x00byte",
	}

	for _, key := range keys {
		if _, err := store.Get(context.Background(), key); err == nil {
			t.Errorf("Get accepted the key %q", key)
		}

		if err := store.Put(context.Background(), key, strings.NewReader("x")); err == nil {
			t.Errorf("Put accepted the key %q", key)
		}
	}

	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("expected the file outside the directory to be untouched: %s", err)
	}

	if body, err := os.ReadFile(outside); err == nil && string(body) != "secret" {
		t.Error("a refused key overwrote a file outside the archive directory")
	}
}

// TestFileStore_PutWritesInsideTheDirectory verifies the ordinary key - the shape the service
// generates, a key prefix and a timestamped name - lands where the operator was told it would.
func TestFileStore_PutWritesInsideTheDirectory(t *testing.T) {
	store := newTestFileStore(t)

	if err := store.Put(context.Background(), "hippocampus/x.gz", strings.NewReader("body")); err != nil {
		t.Fatalf("Put: %s", err)
	}

	if _, err := os.Stat(filepath.Join(store.Directory(), "hippocampus", "x.gz")); err != nil {
		t.Errorf("expected the object inside the archive directory: %s", err)
	}
}

// TestFileStore_PutLeavesNothingBehindOnFailure verifies the atomic write: a body that fails part
// way through must leave no file at the key, since a truncated archive is one Import would accept
// and then fail half way through having already upserted what it read.
func TestFileStore_PutLeavesNothingBehindOnFailure(t *testing.T) {
	store := newTestFileStore(t)

	body := io.MultiReader(strings.NewReader("the start of an archive"), &failingReader{})

	if err := store.Put(context.Background(), "partial.gz", body); err == nil {
		t.Fatal("expected Put to fail")
	}

	if _, err := store.Get(context.Background(), "partial.gz"); err == nil {
		t.Error("a failed Put left an object behind")
	}

	entries, err := os.ReadDir(store.Directory())
	if err != nil {
		t.Fatalf("failed to read the archive directory: %s", err)
	}

	if len(entries) != 0 {
		t.Errorf("a failed Put left %d file(s) in the archive directory", len(entries))
	}
}

// TestFileStore_PutUnderAFile covers the one ordinary way a well-formed key still cannot be
// written: a key prefix whose directory is already a file, which is what a key of "x" followed by
// one of "x/y" produces.
func TestFileStore_PutUnderAFile(t *testing.T) {
	store := newTestFileStore(t)

	if err := store.Put(context.Background(), "x", strings.NewReader("body")); err != nil {
		t.Fatalf("Put: %s", err)
	}

	if err := store.Put(context.Background(), "x/y.gz", strings.NewReader("body")); err == nil {
		t.Error("expected Put to fail where the key prefix is already a file")
	}
}

// TestFileStore_PingRejectsAFile covers the other half of the probe: the directory is still there,
// but is no longer a directory.
func TestFileStore_PingRejectsAFile(t *testing.T) {
	store := newTestFileStore(t)

	if err := os.RemoveAll(store.Directory()); err != nil {
		t.Fatalf("failed to remove the archive directory: %s", err)
	}

	if err := os.WriteFile(store.Directory(), []byte("x"), 0o600); err != nil {
		t.Fatalf("failed to write the fixture: %s", err)
	}

	if err := store.Ping(context.Background()); err == nil {
		t.Error("expected Ping to fail once the archive directory is a file")
	}
}

// TestFileStore_HonourCancellationBeforeWork verifies Get and Ping give up on a cancelled context
// rather than touching the filesystem for a caller that has gone.
func TestFileStore_HonourCancellationBeforeWork(t *testing.T) {
	store := newTestFileStore(t)

	if err := store.Put(context.Background(), "present.gz", strings.NewReader("body")); err != nil {
		t.Fatalf("Put: %s", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := store.Get(ctx, "present.gz"); !errors.Is(err, context.Canceled) {
		t.Errorf("Get: expected a cancellation error, got %v", err)
	}

	if err := store.Ping(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Ping: expected a cancellation error, got %v", err)
	}
}

// TestFileStore_PutHonoursContext verifies a cancelled export stops rather than streaming a whole
// store into a file nobody is waiting for.
func TestFileStore_PutHonoursContext(t *testing.T) {
	store := newTestFileStore(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := store.Put(ctx, "cancelled.gz", strings.NewReader("body")); !errors.Is(err, context.Canceled) {
		t.Errorf("expected a cancellation error, got %v", err)
	}

	if _, err := store.Get(context.Background(), "cancelled.gz"); err == nil {
		t.Error("a cancelled Put left an object behind")
	}
}

// TestFileStore_Ping verifies the topology probe answers for a live directory and fails once the
// directory is gone - the fault it exists to report, an archive directory unmounted underneath a
// running process.
func TestFileStore_Ping(t *testing.T) {
	store := newTestFileStore(t)

	if err := store.Ping(context.Background()); err != nil {
		t.Errorf("Ping: %s", err)
	}

	if err := os.RemoveAll(store.Directory()); err != nil {
		t.Fatalf("failed to remove the archive directory: %s", err)
	}

	if err := store.Ping(context.Background()); err == nil {
		t.Error("expected Ping to fail once the archive directory is gone")
	}
}

func newTestFileStore(t *testing.T) *FileStore {
	t.Helper()

	store, err := NewFileStore(filepath.Join(t.TempDir(), "archives"))
	if err != nil {
		t.Fatalf("NewFileStore: %s", err)
	}

	return store
}

func mustGet(t *testing.T, store *FileStore, key string) string {
	t.Helper()

	reader, err := store.Get(context.Background(), key)
	if err != nil {
		t.Fatalf("Get: %s", err)
	}

	defer func() {
		_ = reader.Close()
	}()

	body, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("failed to read the object back: %s", err)
	}

	return string(body)
}

// failingReader fails after whatever came before it in a MultiReader has been read, so a Put gets
// part way through a body and then cannot finish.
type failingReader struct{}

func (f *failingReader) Read(_ []byte) (int, error) {
	return 0, errors.New("the source went away")
}
