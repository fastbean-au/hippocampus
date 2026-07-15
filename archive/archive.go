// Package archive implements the export/import wire format shared by the S3 Export/Import RPCs:
// a gzip-compressed stream of length-delimited ArchiveRecord protos, header first.
// Length-delimited protobuf rather than JSON lines keeps the object compact, streams both ways
// without holding the store in memory, and reuses the exact proto messages the API speaks — the
// same records travel the direct Transfer path as ImportBatch requests, so the two paths share
// one serialization.
package archive

import (
	"bufio"
	"compress/gzip"
	"fmt"
	"io"

	"google.golang.org/protobuf/encoding/protodelim"

	"github.com/fastbean-au/hippocampus/contract"
)

// Version identifies the archive layout; a reader refuses an archive whose header carries a
// version it does not know.
const Version = 1

// Writer writes an archive onto an underlying stream. Close flushes the compression; it does not
// close the underlying writer.
type Writer struct {
	gz *gzip.Writer
	bw *bufio.Writer
}

// NewWriter starts an archive on w. The caller must call Close when done writing.
func NewWriter(w io.Writer) *Writer {
	gz := gzip.NewWriter(w)

	return &Writer{gz: gz, bw: bufio.NewWriter(gz)}
}

// WriteHeader writes the header record; it must be the first record written.
func (w *Writer) WriteHeader(header *contract.ArchiveHeader) error {
	return w.write(&contract.ArchiveRecord{Record: &contract.ArchiveRecord_Header{Header: header}})
}

// WriteEvent appends an event record.
func (w *Writer) WriteEvent(event *contract.Event) error {
	return w.write(&contract.ArchiveRecord{Record: &contract.ArchiveRecord_Event{Event: event}})
}

// WriteMemory appends a memory record.
func (w *Writer) WriteMemory(memory *contract.Memory) error {
	return w.write(&contract.ArchiveRecord{Record: &contract.ArchiveRecord_Memory{Memory: memory}})
}

func (w *Writer) write(record *contract.ArchiveRecord) error {
	if _, err := protodelim.MarshalTo(w.bw, record); err != nil {
		return fmt.Errorf("failed to write archive record: %w", err)
	}

	return nil
}

// Close flushes buffered records and the gzip stream.
func (w *Writer) Close() error {
	if err := w.bw.Flush(); err != nil {
		return fmt.Errorf("failed to flush archive: %w", err)
	}

	if err := w.gz.Close(); err != nil {
		return fmt.Errorf("failed to close archive compression: %w", err)
	}

	return nil
}

// Reader reads an archive from an underlying stream. NewReader consumes and validates the header
// eagerly, so a stream that is not an archive fails at open rather than midway.
type Reader struct {
	gz     *gzip.Reader
	br     *bufio.Reader
	header *contract.ArchiveHeader
}

// NewReader opens an archive and reads its header.
func NewReader(r io.Reader) (*Reader, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("failed to open archive compression: %w", err)
	}

	a := &Reader{gz: gz, br: bufio.NewReader(gz)}

	record, err := a.Read()
	if err != nil {
		return nil, fmt.Errorf("failed to read archive header: %w", err)
	}

	header := record.GetHeader()
	if header == nil {
		return nil, fmt.Errorf("archive does not start with a header record")
	}

	if header.GetVersion() != Version {
		return nil, fmt.Errorf("unsupported archive version %d (supported: %d)", header.GetVersion(), Version)
	}

	a.header = header

	return a, nil
}

// Header returns the archive's header record.
func (r *Reader) Header() *contract.ArchiveHeader {
	return r.header
}

// Read returns the next record, or io.EOF at the end of the archive.
func (r *Reader) Read() (*contract.ArchiveRecord, error) {
	var record contract.ArchiveRecord

	if err := protodelim.UnmarshalFrom(r.br, &record); err != nil {
		if err == io.EOF {
			return nil, io.EOF
		}

		return nil, fmt.Errorf("failed to read archive record: %w", err)
	}

	return &record, nil
}

// Close releases the decompressor; it does not close the underlying reader.
func (r *Reader) Close() error {
	return r.gz.Close()
}
