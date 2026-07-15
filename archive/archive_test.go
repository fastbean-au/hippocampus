package archive

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/fastbean-au/hippocampus/contract"
)

// TestArchiveRoundTrip verifies the codec end to end: header first, every record preserved in
// order and byte-for-byte. Bodies are proto3 strings, so like everywhere else in the API they
// are UTF-8; a client's encoded binary payload (is_binary) is just another string here.
func TestArchiveRoundTrip(t *testing.T) {
	binaryBody := "AAD/G4B/base64-encoded-by-the-client=="

	var buf bytes.Buffer
	w := NewWriter(&buf)

	if err := w.WriteHeader(&contract.ArchiveHeader{Version: Version, ExportedAt: 12345, EventCount: 1, MemoryCount: 2}); err != nil {
		t.Fatalf("WriteHeader: %s", err)
	}

	if err := w.WriteEvent(&contract.Event{Id: "e1", Name: "event", Significance: 3, Group: "g"}); err != nil {
		t.Fatalf("WriteEvent: %s", err)
	}

	if err := w.WriteMemory(&contract.Memory{Id: "m1", Body: "text", RecallCount: 2, IsSummary: true}); err != nil {
		t.Fatalf("WriteMemory: %s", err)
	}

	if err := w.WriteMemory(&contract.Memory{Id: "m2", Body: binaryBody, IsBinary: contract.Bool_TRUE}); err != nil {
		t.Fatalf("WriteMemory: %s", err)
	}

	if err := w.Close(); err != nil {
		t.Fatalf("Close: %s", err)
	}

	r, err := NewReader(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("NewReader: %s", err)
	}
	defer func() { _ = r.Close() }()

	if r.Header().GetExportedAt() != 12345 || r.Header().GetMemoryCount() != 2 {
		t.Errorf("header not preserved: %+v", r.Header())
	}

	event, err := r.Read()
	if err != nil || event.GetEvent().GetId() != "e1" || event.GetEvent().GetGroup() != "g" {
		t.Errorf("expected event e1 first, got %v (%v)", event, err)
	}

	m1, err := r.Read()
	if err != nil || m1.GetMemory().GetId() != "m1" || m1.GetMemory().GetRecallCount() != 2 || !m1.GetMemory().GetIsSummary() {
		t.Errorf("memory m1 not preserved, got %v (%v)", m1, err)
	}

	m2, err := r.Read()
	if err != nil || m2.GetMemory().GetBody() != binaryBody {
		t.Errorf("binary body not preserved byte-for-byte, got %v (%v)", m2, err)
	}

	if _, err := r.Read(); !errors.Is(err, io.EOF) {
		t.Errorf("expected io.EOF at end of archive, got %v", err)
	}
}

// failWriter fails every write after allowing a fixed number of bytes through, so the gzip stream
// can be primed and then made to fail on flush/close.
type failWriter struct {
	remaining int
}

func (f *failWriter) Write(p []byte) (int, error) {
	if f.remaining <= 0 {
		return 0, io.ErrClosedPipe
	}

	if len(p) > f.remaining {
		n := f.remaining
		f.remaining = 0

		return n, io.ErrClosedPipe
	}

	f.remaining -= len(p)

	return len(p), nil
}

// TestWriterCloseSurfacesUnderlyingError verifies Close reports a failure flushing the compressed
// stream rather than swallowing it.
func TestWriterCloseSurfacesUnderlyingError(t *testing.T) {
	w := NewWriter(&failWriter{})

	// Buffer a record so Close has bytes to flush into the failing writer.
	if err := w.WriteHeader(&contract.ArchiveHeader{Version: Version}); err != nil {
		t.Fatalf("WriteHeader: %s", err)
	}

	if err := w.Close(); err == nil {
		t.Error("expected Close to surface the underlying writer's error")
	}
}

// TestReaderReadCorrupted verifies a corrupted compressed body surfaces a read error rather than a
// clean EOF or a silently wrong record.
func TestReaderReadCorrupted(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)

	if err := w.WriteHeader(&contract.ArchiveHeader{Version: Version}); err != nil {
		t.Fatalf("WriteHeader: %s", err)
	}

	if err := w.WriteMemory(&contract.Memory{Id: "m1", Body: "a reasonably long body so the record spans several bytes"}); err != nil {
		t.Fatalf("WriteMemory: %s", err)
	}

	if err := w.Close(); err != nil {
		t.Fatalf("Close: %s", err)
	}

	// Flip bytes inside the deflate stream (past the 10-byte gzip header) so decompression fails.
	corrupt := append([]byte(nil), buf.Bytes()...)
	for i := 12; i < 20 && i < len(corrupt); i++ {
		corrupt[i] ^= 0xff
	}

	r, err := NewReader(bytes.NewReader(corrupt))
	if err != nil {
		// The corruption may already break the header read; that is also a valid rejection.
		return
	}
	defer func() { _ = r.Close() }()

	if _, err := r.Read(); err == nil {
		t.Error("expected an error reading a corrupted record")
	}
}

// TestReaderRejectsBadStreams verifies open-time failures: not gzip, no header record, and an
// unknown version.
func TestReaderRejectsBadStreams(t *testing.T) {
	if _, err := NewReader(bytes.NewReader([]byte("not an archive"))); err == nil {
		t.Error("expected a non-gzip stream to be rejected")
	}

	var noHeader bytes.Buffer
	w := NewWriter(&noHeader)

	if err := w.WriteMemory(&contract.Memory{Id: "m1"}); err != nil {
		t.Fatalf("WriteMemory: %s", err)
	}

	if err := w.Close(); err != nil {
		t.Fatalf("Close: %s", err)
	}

	if _, err := NewReader(bytes.NewReader(noHeader.Bytes())); err == nil {
		t.Error("expected an archive without a header record to be rejected")
	}

	var badVersion bytes.Buffer
	w = NewWriter(&badVersion)

	if err := w.WriteHeader(&contract.ArchiveHeader{Version: Version + 1}); err != nil {
		t.Fatalf("WriteHeader: %s", err)
	}

	if err := w.Close(); err != nil {
		t.Fatalf("Close: %s", err)
	}

	if _, err := NewReader(bytes.NewReader(badVersion.Bytes())); err == nil {
		t.Error("expected an unknown archive version to be rejected")
	}
}
