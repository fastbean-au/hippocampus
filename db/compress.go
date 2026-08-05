package db

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"sync"

	log "github.com/sirupsen/logrus"
)

// Memory bodies are optionally stored compressed. The algorithm is deliberately fixed rather than
// configurable: a body written by one version of the service must stay readable by every later one,
// and a configurable algorithm would make that a matter of the operator never changing a key. gzip
// is the choice - it is in the standard library (no dependency), and its magic number makes a body
// dumped straight out of the database identifiable by ordinary tooling.
//
// What IS configurable is whether a body is compressed on the way in, so the decision is recorded
// per row (memories.is_compressed) rather than inferred from the current configuration: the
// configuration can change over the lifetime of a store, and rows written under the old setting must
// keep reading correctly. Reads therefore always honour the row's flag and never consult the
// configuration at all - which is what makes enabling and disabling compression safe at any point.
//
// Compression is applied at the storage boundary (the write helpers and the row scanners in
// memory.go), so everything above the db package - the RPC layer, the search index, export/import,
// the summariser - continues to see the plain body and needs no knowledge of any of this.

// compressionMinBytesFloor is the smallest minimum-size threshold that has any effect. Below it
// gzip's own header and trailer (~18 bytes for even an empty payload) dominate, so the
// "only keep it if it actually got smaller" rule in compressBody would reject nearly every result
// anyway - the compression attempt would be spent CPU with no storage to show for it.
const compressionMinBytesFloor = 64

// defaultCompressionMinBytes is the threshold applied when compression is enabled without one. It
// is well above the point where gzip starts paying for itself on the short, log-line-shaped bodies
// this service is most often given.
const defaultCompressionMinBytes = 512

// compression holds the write-side compression policy. The zero value - the default - stores every
// body verbatim, exactly as the service behaved before compression existed.
type compression struct {
	enabled bool

	// minBytes is the body size at or above which compression is attempted. Small bodies are stored
	// verbatim: they compress poorly (often not at all, once the gzip header is counted) and are the
	// ones most likely to be read in bulk.
	minBytes int
}

// SetCompression sets the write-side body compression policy (see the compression type). Called
// once at startup from main before the server begins serving, so it needs no lock. A non-positive
// minBytes takes the default threshold; anything under compressionMinBytesFloor is raised to it,
// since a smaller threshold only buys wasted compression attempts.
//
// It governs writes only. Reads are driven by each row's own is_compressed flag, so disabling
// compression on a store that has compressed rows in it leaves those rows perfectly readable - they
// simply stop being rewritten compressed.
func (d *DB) SetCompression(enabled bool, minBytes int) {
	if !enabled {
		d.compression = compression{}

		return
	}

	if minBytes <= 0 {
		minBytes = defaultCompressionMinBytes
	}

	if minBytes < compressionMinBytesFloor {
		log.Warnf(
			"storage.compression.minBytes %d is below the useful floor; using %d",
			minBytes,
			compressionMinBytesFloor,
		)

		minBytes = compressionMinBytesFloor
	}

	d.compression = compression{
		enabled:  true,
		minBytes: minBytes,
	}
}

// compressBody returns the bytes to store for a memory body and whether they are compressed - the
// value bound to the row's body and is_compressed columns respectively.
//
// A body is stored verbatim when compression is off, when the memory is binary (its body is
// client-encoded, so it is as likely to be an already-compressed payload as anything else - see
// the note in docs/configuration.md), when it is below the size threshold, or when compressing it
// did not actually make it smaller. That last check is what makes the feature safe on incompressible
// content: the worst case is the compression attempt's CPU, never a body that grew in storage.
//
// A compression failure is not an error the caller needs to handle - the body is simply stored
// verbatim, which is always a valid representation - so it is logged and swallowed rather than
// failing the write.
func (d *DB) compressBody(body string, isBinary bool) ([]byte, bool) {
	raw := []byte(body)

	if !d.compression.enabled || isBinary || len(raw) < d.compression.minBytes {
		return raw, false
	}

	packed, err := gzipBytes(raw)
	if err != nil {
		log.Warnf("failed to compress memory body, storing it uncompressed: %s", err.Error())

		return raw, false
	}

	if len(packed) >= len(raw) {
		return raw, false
	}

	return packed, true
}

// decompressBody turns a stored body back into the memory body the rest of the service sees. It is
// driven entirely by the row's own flag, never by the current configuration, so rows written under
// a different setting - or by a different version of the service - read correctly.
func decompressBody(stored []byte, isCompressed bool) (string, error) {
	if !isCompressed {
		return string(stored), nil
	}

	raw, err := gunzipBytes(stored)
	if err != nil {
		return "", err
	}

	return string(raw), nil
}

// The compressors are pooled because constructing one dominates the cost of using it: a
// gzip.Writer carries flate's 32 KiB window and its hash tables, so gzip.NewWriter allocates
// hundreds of kilobytes whatever the body's size. On the short bodies this service mostly handles,
// pooling measured 3-4x faster on the write path and ~2x on the read path - the difference between
// compression being a rounding error against the surrounding database round trip and being
// comparable to it. Both types support Reset, which is what makes them reusable.
//
// The level is BestSpeed, and measurably so rather than by taste: on the body shapes this service
// is actually given it lands within a few percent of the default level's ratio (4.35x vs 4.37x on
// batched log lines, 8.20x vs 8.47x on prose) for around a third of the CPU. Since compression is
// on by default, the cheap end of that curve is the right place to sit. Unlike the algorithm, the
// level is not a compatibility concern at all - it only affects the encoder, and any gzip reader
// decodes any level - so it can be revisited freely.
var (
	gzipWriters = sync.Pool{New: func() any {
		// NewWriterLevel only errors on an invalid level, and BestSpeed is a constant.
		writer, _ := gzip.NewWriterLevel(io.Discard, gzip.BestSpeed)

		return writer
	}}
	gzipReaders = sync.Pool{New: func() any { return new(gzip.Reader) }}
)

// gzipTo compresses b into w. Close flushes the trailer, so it has to succeed for the output to be
// a valid gzip stream - unlike the usual deferred, ignored Close - and both failures are joined so
// neither is lost. It is split from gzipBytes so those failures are reachable in a test: writing to
// gzipBytes' own bytes.Buffer destination cannot fail.
func gzipTo(w io.Writer, b []byte) error {
	writer, _ := gzipWriters.Get().(*gzip.Writer)

	// Reset both binds the writer to this destination and clears any state left by the previous
	// use, including a failed one - so a writer returned to the pool after an error is still safe
	// to reuse.
	writer.Reset(w)

	_, writeErr := writer.Write(b)
	err := errors.Join(writeErr, writer.Close())

	// Rebind to a throwaway destination before pooling it, so a pooled writer never keeps the
	// caller's buffer (up to memory.limit.sizeBytes) alive until its next use.
	writer.Reset(io.Discard)
	gzipWriters.Put(writer)

	return err
}

// gzipBytes compresses b with gzip.
func gzipBytes(b []byte) ([]byte, error) {
	var buf bytes.Buffer

	if err := gzipTo(&buf, b); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// gunzipBytes decompresses a gzip stream produced by gzipBytes. A pooled zero-value gzip.Reader is
// valid: Reset initialises it exactly as gzip.NewReader would, reading the header and reporting a
// stream that is not gzip at all.
func gunzipBytes(b []byte) ([]byte, error) {
	reader, _ := gzipReaders.Get().(*gzip.Reader)
	defer gzipReaders.Put(reader)

	if err := reader.Reset(bytes.NewReader(b)); err != nil {
		return nil, err
	}

	var raw bytes.Buffer

	// ReadFrom grows one buffer geometrically rather than io.ReadAll's repeated reslicing, and lets
	// the decompressed size be pre-sized below.
	raw.Grow(len(b) * 2)

	if _, err := raw.ReadFrom(reader); err != nil {
		return nil, err
	}

	return raw.Bytes(), nil
}
