package seal

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

// The tests in this file cover a sealed stream's I/O failures: an underlying writer that refuses
// what it is given, a header cut short, a reader that fails mid-stream.

var errStreamIO = errors.New("stream i/o failure")

// failingWriter accepts the first allow writes and refuses every one after.
type failingWriter struct {
	allow int
}

func (f *failingWriter) Write(p []byte) (int, error) {
	if f.allow <= 0 {
		return 0, errStreamIO
	}

	f.allow--

	return len(p), nil
}

// failingReader returns data, then fails rather than reporting the end of the stream.
type failingReader struct {
	data []byte
}

func (f *failingReader) Read(p []byte) (int, error) {
	if len(f.data) == 0 {
		return 0, errStreamIO
	}

	n := copy(p, f.data)
	f.data = f.data[n:]

	return n, nil
}

func TestNewWriterReportsAHeaderWriteFailure(t *testing.T) {
	t.Parallel()

	ring, _ := testRing(t)

	if _, err := ring.NewWriter(&failingWriter{}); !errors.Is(err, errStreamIO) {
		t.Errorf("NewWriter error = %v, want the header write failure", err)
	}
}

func TestStreamWriteReportsASegmentWriteFailure(t *testing.T) {
	t.Parallel()

	ring, _ := testRing(t)

	w, err := ring.NewWriter(&failingWriter{allow: 1})
	if err != nil {
		t.Fatalf("NewWriter: %s", err)
	}

	// A full segment and one byte more makes Write seal the first segment, which the writer refuses.
	if _, err := w.Write(make([]byte, segmentBytes+1)); !errors.Is(err, errStreamIO) {
		t.Errorf("Write error = %v, want the segment write failure", err)
	}
}

func TestStreamCloseReportsAFinalSegmentWriteFailure(t *testing.T) {
	t.Parallel()

	ring, _ := testRing(t)

	w, err := ring.NewWriter(&failingWriter{allow: 1})
	if err != nil {
		t.Fatalf("NewWriter: %s", err)
	}

	if err := w.Close(); !errors.Is(err, errStreamIO) {
		t.Errorf("Close error = %v, want the final segment write failure", err)
	}

	// A second Close is a no-op, not a second attempt.
	if err := w.Close(); err != nil {
		t.Errorf("a second Close: %v", err)
	}
}

func TestStreamRefusesATruncatedHeader(t *testing.T) {
	t.Parallel()

	ring, _ := testRing(t)
	sealed := sealStream(t, ring, []byte("archive"))

	cuts := map[string]int{
		"magic only":        len(streamMagic),
		"inside the salt":   len(streamMagic) + 1 + len(ring.Active()) + saltBytes/2,
		"inside the key id": len(streamMagic) + 2,
	}

	for k, v := range cuts {
		t.Run(k, func(t *testing.T) {
			t.Parallel()

			if _, err := ring.NewReader(bytes.NewReader(sealed[:v])); err == nil {
				t.Error("a stream with a truncated header opened")
			}
		})
	}
}

func TestStreamReportsAReadFailure(t *testing.T) {
	t.Parallel()

	ring, _ := testRing(t)
	sealed := sealStream(t, ring, bytes.Repeat([]byte("x"), 3*segmentBytes))

	// Fails mid-segment, so the segment read sees the reader's own error rather than an EOF.
	r, err := ring.NewReader(&failingReader{data: sealed[:len(sealed)/2]})
	if err != nil {
		t.Fatalf("NewReader: %s", err)
	}

	if _, err := io.ReadAll(r); !errors.Is(err, errStreamIO) {
		t.Errorf("reading error = %v, want the reader's failure", err)
	}
}

func TestNewGCMRefusesAKeyOfTheWrongLength(t *testing.T) {
	t.Parallel()

	if _, err := newGCM(make([]byte, 5)); err == nil {
		t.Error("newGCM accepted a 5-byte key")
	}
}
