package archive

import (
	"bytes"
	"compress/gzip"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fastbean-au/hippocampus/contract"
)

// FuzzReader (TODO-3 item 167): Import reads an archive somebody else wrote, so no stream - however
// malformed after decompression - may panic the reader or loop it forever. The fuzzed bytes are the
// record stream inside the gzip, because random bytes almost never get past a gzip header and would
// leave the part worth fuzzing untouched.
func FuzzReader(f *testing.F) {
	var valid bytes.Buffer

	w := NewWriter(&valid)
	_ = w.WriteHeader(&contract.ArchiveHeader{Version: Version})
	_ = w.WriteEvent(&contract.Event{Id: "e1", Name: "one"})
	_ = w.WriteMemory(&contract.Memory{Id: "m1", Body: "body"})
	_ = w.Close()

	gz, err := gzip.NewReader(&valid)
	if err != nil {
		f.Fatalf("gzip: %s", err)
	}

	plain, err := io.ReadAll(gz)
	if err != nil {
		f.Fatalf("read: %s", err)
	}

	f.Add(plain)
	f.Add([]byte{})
	f.Add([]byte{0xff, 0xff, 0xff, 0xff, 0x0f})
	f.Add(plain[:len(plain)/2])

	f.Fuzz(func(t *testing.T, stream []byte) {
		var compressed bytes.Buffer

		zw := gzip.NewWriter(&compressed)
		_, _ = zw.Write(stream)
		_ = zw.Close()

		r, err := NewReader(&compressed)
		if err != nil {
			return
		}

		// Every record consumes at least one byte, so a stream cannot yield more records than it
		// has bytes; more would be a reader that has stopped advancing.
		for range len(stream) + 1 {
			if _, err := r.Read(); err != nil {
				return
			}
		}

		t.Fatalf("read more records than a %d-byte stream can hold", len(stream))
	})
}

// FuzzResolve (TODO-3 item 167): Import's object_key arrives from the request, so over a filesystem
// it is a caller-supplied path. Whatever it is, an accepted key must resolve inside the directory.
func FuzzResolve(f *testing.F) {
	for _, seed := range []string{"a.archive.gz", "scheduled/x.gz", "../etc/passwd", "a/../../b", "/abs", "a//b", "a\\b", ".", "a/./b", "..", "a/..", "ok/.."} {
		f.Add(seed)
	}

	dir := f.TempDir()

	store, err := NewFileStore(dir)
	if err != nil {
		f.Fatalf("NewFileStore: %s", err)
	}

	root := store.Directory()

	f.Fuzz(func(t *testing.T, key string) {
		target, err := store.resolve(key)
		if err != nil {
			return
		}

		rel, err := filepath.Rel(root, target)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			t.Fatalf("key %q resolved to %q, outside %q", key, target, root)
		}
	})
}
