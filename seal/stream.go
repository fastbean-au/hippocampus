package seal

import (
	"bufio"
	"bytes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// A sealed stream is how an export archive is encrypted. An archive can be the whole store, so it
// cannot be sealed as one value - that would hold it in memory twice over - and it is sealed in
// segments instead:
//
//	magic (8) | key id length (1) | key id | salt (16) | segment ... | final segment
//
// Each segment is up to segmentBytes of plaintext sealed with AES-256-GCM under a key derived from
// the configured key and the stream's salt. The nonce is the segment's position, with its last byte
// marking the final segment, so a segment cannot be reordered, dropped or repeated, and a stream cut
// short at a segment boundary fails on its last segment rather than reading as a shorter archive.
// The header is the additional data of every segment, so it cannot be altered either.
//
// Every segment but the last is exactly segmentBytes of plaintext. That is what lets the reader find
// the boundaries without a length prefix: a short segment is the last, and a full one is the last
// only if nothing follows it.

// streamMagic opens a sealed stream. An archive is a gzip stream, which opens 1f 8b, so the two are
// told apart by their first bytes, and an unsealed archive written before encryption still imports.
var streamMagic = []byte("HIPSEAL\x01")

// segmentBytes is the plaintext carried by every segment but the last.
const segmentBytes = 64 << 10

// tagBytes is GCM's authentication tag, appended to every segment.
const tagBytes = 16

// streamInfo is the HKDF info string for streams; see valueInfo.
const streamInfo = "hippocampus seal stream v1"

// IsSealedStream reports whether data opens with a sealed stream's header. It is for a caller that
// has the first bytes of a stream in hand and wants to know what it is holding.
func IsSealedStream(data []byte) bool {
	return bytes.HasPrefix(data, streamMagic)
}

// NewWriter starts a sealed stream on w under the active key. Close writes the final segment and
// must be called; it does not close w. A ring with no active key returns a writer that passes
// everything through to w unchanged, so a caller writes through whatever this returns.
func (k *Keyring) NewWriter(w io.Writer) (io.WriteCloser, error) {
	if k.Active() == "" {
		return nopWriteCloser{w}, nil
	}

	salt := make([]byte, saltBytes)

	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("drawing a salt: %w", err)
	}

	header := streamHeader(k.active, salt)

	aead, err := streamCipher(k.keys[k.active], salt)
	if err != nil {
		return nil, err
	}

	if _, err := w.Write(header); err != nil {
		return nil, fmt.Errorf("writing the sealed stream header: %w", err)
	}

	return &streamWriter{
		w:      w,
		aead:   aead,
		header: header,
		buf:    make([]byte, 0, segmentBytes),
	}, nil
}

// NewReader opens a stream that may or may not be sealed. A sealed one is opened with the key its
// header names, which the ring must hold; anything else is returned as it stands, so an archive
// written unsealed - before encryption was enabled, or by an instance without it - still reads.
func (k *Keyring) NewReader(r io.Reader) (io.Reader, error) {
	buffered := bufio.NewReader(r)

	// A Peek short of the magic is a stream too short to be sealed, which is not this function's to
	// judge: it is handed on, and whatever reads it next says what is wrong with it.
	head, err := buffered.Peek(len(streamMagic))
	if err != nil || !IsSealedStream(head) {
		return buffered, nil
	}

	return k.openStream(buffered)
}

// openStream reads a sealed stream's header and returns a reader over its plaintext.
func (k *Keyring) openStream(r *bufio.Reader) (io.Reader, error) {
	if _, err := r.Discard(len(streamMagic)); err != nil {
		return nil, fmt.Errorf("reading the sealed stream header: %w", err)
	}

	idLength, err := r.ReadByte()
	if err != nil {
		return nil, fmt.Errorf("reading the sealed stream header: %w", err)
	}

	rest := make([]byte, int(idLength)+saltBytes)

	if _, err := io.ReadFull(r, rest); err != nil {
		return nil, fmt.Errorf("reading the sealed stream header: %w", err)
	}

	id := string(rest[:idLength])
	salt := rest[idLength:]

	if !k.Holds(id) {
		return nil, fmt.Errorf("key %s: %w", id, ErrUnknownKey)
	}

	aead, err := streamCipher(k.keys[id], salt)
	if err != nil {
		return nil, err
	}

	return &streamReader{
		r:       r,
		aead:    aead,
		header:  streamHeader(id, salt),
		segment: make([]byte, segmentBytes+tagBytes),
	}, nil
}

// streamHeader renders a stream's header, which is also the additional data of its every segment.
func streamHeader(id string, salt []byte) []byte {
	header := make([]byte, 0, len(streamMagic)+1+len(id)+len(salt))
	header = append(header, streamMagic...)
	header = append(header, byte(len(id)))
	header = append(header, id...)
	header = append(header, salt...)

	return header
}

// streamCipher derives a stream's AES-256-GCM key from the configured key and the stream's salt.
func streamCipher(key []byte, salt []byte) (cipher.AEAD, error) {
	derived, err := hkdf.Key(sha256.New, key, salt, streamInfo, KeyBytes)
	if err != nil {
		return nil, fmt.Errorf("deriving a stream key: %w", err)
	}

	return newGCM(derived)
}

// segmentNonce is a segment's nonce: its position, and whether it is the last.
func segmentNonce(counter uint64, final bool) []byte {
	nonce := make([]byte, 12)
	binary.BigEndian.PutUint64(nonce, counter)

	if final {
		nonce[11] = 1
	}

	return nonce
}

// streamWriter seals what is written to it, a segment at a time.
type streamWriter struct {
	w       io.Writer
	aead    cipher.AEAD
	header  []byte
	buf     []byte
	counter uint64
	closed  bool
}

// Write buffers p, sealing a full segment only once more data arrives behind it - so the segment
// still in the buffer at Close is the last, whatever its length.
func (s *streamWriter) Write(p []byte) (int, error) {
	if s.closed {
		return 0, errors.New("write to a closed sealed stream")
	}

	written := 0

	for len(p) > 0 {
		if len(s.buf) == segmentBytes {
			if err := s.flush(false); err != nil {
				return written, err
			}
		}

		n := copy(s.buf[len(s.buf):segmentBytes], p)
		s.buf = s.buf[:len(s.buf)+n]
		p = p[n:]
		written += n
	}

	return written, nil
}

// Close seals the final segment, which may be empty. It does not close the underlying writer.
func (s *streamWriter) Close() error {
	if s.closed {
		return nil
	}

	s.closed = true

	return s.flush(true)
}

// flush seals the buffered plaintext as the next segment and writes it out.
func (s *streamWriter) flush(final bool) error {
	sealed := s.aead.Seal(nil, segmentNonce(s.counter, final), s.buf, s.header)

	if _, err := s.w.Write(sealed); err != nil {
		return fmt.Errorf("writing a sealed segment: %w", err)
	}

	s.counter++
	s.buf = s.buf[:0]

	return nil
}

// streamReader opens a sealed stream a segment at a time.
type streamReader struct {
	r       *bufio.Reader
	aead    cipher.AEAD
	header  []byte
	segment []byte
	counter uint64

	// plain is the opened segment not yet read out, and done is set once the final segment has
	// been opened.
	plain []byte
	done  bool
	err   error
}

func (s *streamReader) Read(p []byte) (int, error) {
	for len(s.plain) == 0 {
		if s.err != nil {
			return 0, s.err
		}

		if s.done {
			return 0, io.EOF
		}

		s.plain, s.err = s.next()
	}

	n := copy(p, s.plain)
	s.plain = s.plain[n:]

	return n, nil
}

// next reads and opens the next segment. A short read is the final segment; a full one is final only
// if the stream ends behind it. Either way the nonce says which the writer meant, so a stream cut at
// a segment boundary - whose last remaining segment was sealed as not final - fails here.
func (s *streamReader) next() ([]byte, error) {
	n, err := io.ReadFull(s.r, s.segment)

	final := false

	switch {

	case err == nil:
		if _, peekErr := s.r.Peek(1); errors.Is(peekErr, io.EOF) {
			final = true
		}

	case errors.Is(err, io.ErrUnexpectedEOF):
		final = true

	case errors.Is(err, io.EOF):
		return nil, fmt.Errorf("the sealed stream ends before its final segment: %w", ErrTampered)

	default:
		return nil, fmt.Errorf("reading a sealed segment: %w", err)

	}

	plain, openErr := s.aead.Open(nil, segmentNonce(s.counter, final), s.segment[:n], s.header)
	if openErr != nil {
		return nil, fmt.Errorf("segment %d: %w", s.counter, ErrTampered)
	}

	s.counter++
	s.done = final

	return plain, nil
}

// nopWriteCloser is the writer an unsealing ring returns: w, with a Close that does nothing, since a
// sealed writer's Close does not close w either.
type nopWriteCloser struct {
	io.Writer
}

func (nopWriteCloser) Close() error {
	return nil
}
