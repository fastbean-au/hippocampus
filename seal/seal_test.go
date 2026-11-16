package seal

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"testing"
)

// testKey returns a fresh random key.
func testKey(t *testing.T) []byte {
	t.Helper()

	key := make([]byte, KeyBytes)

	if _, err := rand.Read(key); err != nil {
		t.Fatalf("drawing a key: %s", err)
	}

	return key
}

// testRing returns a ring sealing under a fresh key, and that key.
func testRing(t *testing.T) (*Keyring, []byte) {
	t.Helper()

	key := testKey(t)

	ring, err := NewKeyring(key, nil)
	if err != nil {
		t.Fatalf("building a ring: %s", err)
	}

	return ring, key
}

func TestSealRoundTrip(t *testing.T) {
	t.Parallel()

	ring, _ := testRing(t)
	plaintext := []byte("the quick brown fox")

	sealed, id, err := ring.Seal(plaintext, []byte("memory-1"))
	if err != nil {
		t.Fatalf("sealing: %s", err)
	}

	if id != ring.Active() {
		t.Errorf("sealed under %q, want the active key %q", id, ring.Active())
	}

	if bytes.Contains(sealed, plaintext) {
		t.Error("the sealed value carries the plaintext")
	}

	if len(sealed) != len(plaintext)+valueOverhead {
		t.Errorf("sealed length %d, want %d", len(sealed), len(plaintext)+valueOverhead)
	}

	opened, err := ring.Open(sealed, id, []byte("memory-1"))
	if err != nil {
		t.Fatalf("opening: %s", err)
	}

	if !bytes.Equal(opened, plaintext) {
		t.Errorf("opened %q, want %q", opened, plaintext)
	}
}

func TestSealDrawsAFreshKeyEveryTime(t *testing.T) {
	t.Parallel()

	ring, _ := testRing(t)

	first, _, _ := ring.Seal([]byte("same"), nil)
	second, _, _ := ring.Seal([]byte("same"), nil)

	if bytes.Equal(first, second) {
		t.Error("two seals of the same value are identical, so the salt is not being drawn")
	}
}

func TestOpenRefusesAnotherContext(t *testing.T) {
	t.Parallel()

	ring, _ := testRing(t)

	sealed, id, _ := ring.Seal([]byte("body"), []byte("memory-1"))

	if _, err := ring.Open(sealed, id, []byte("memory-2")); !errors.Is(err, ErrTampered) {
		t.Errorf("opening a value moved to another row: got %v, want ErrTampered", err)
	}
}

func TestOpenRefusesAnAlteredValue(t *testing.T) {
	t.Parallel()

	ring, _ := testRing(t)

	sealed, id, _ := ring.Seal([]byte("body"), nil)

	for i := range sealed {
		altered := bytes.Clone(sealed)
		altered[i] ^= 0x01

		if _, err := ring.Open(altered, id, nil); !errors.Is(err, ErrTampered) {
			t.Errorf("byte %d altered: got %v, want ErrTampered", i, err)
		}
	}

	if _, err := ring.Open(sealed[:valueOverhead-1], id, nil); !errors.Is(err, ErrTampered) {
		t.Errorf("a value shorter than the overhead: got %v, want ErrTampered", err)
	}
}

func TestRetiredKeyStillOpens(t *testing.T) {
	t.Parallel()

	old, oldKey := testRing(t)

	sealed, id, _ := old.Seal([]byte("written under the old key"), nil)

	rotated, err := NewKeyring(testKey(t), [][]byte{oldKey})
	if err != nil {
		t.Fatalf("building the rotated ring: %s", err)
	}

	if rotated.Active() == id {
		t.Fatal("the rotated ring is still sealing under the old key")
	}

	opened, err := rotated.Open(sealed, id, nil)
	if err != nil {
		t.Fatalf("opening under a retired key: %s", err)
	}

	if string(opened) != "written under the old key" {
		t.Errorf("opened %q", opened)
	}
}

func TestOpenReportsAKeyItDoesNotHold(t *testing.T) {
	t.Parallel()

	ring, _ := testRing(t)
	other, _ := testRing(t)

	sealed, id, _ := ring.Seal([]byte("body"), nil)

	if _, err := other.Open(sealed, id, nil); !errors.Is(err, ErrUnknownKey) {
		t.Errorf("got %v, want ErrUnknownKey", err)
	}

	var none *Keyring

	if _, err := none.Open(sealed, id, nil); !errors.Is(err, ErrUnknownKey) {
		t.Errorf("a nil ring: got %v, want ErrUnknownKey", err)
	}
}

func TestANilOrReadOnlyRingSealsNothing(t *testing.T) {
	t.Parallel()

	_, key := testRing(t)

	readOnly, err := NewKeyring(nil, [][]byte{key})
	if err != nil {
		t.Fatalf("building a read-only ring: %s", err)
	}

	for name, ring := range map[string]*Keyring{"nil": nil, "read-only": readOnly} {
		stored, id, err := ring.Seal([]byte("plain"), nil)
		if err != nil || id != "" || string(stored) != "plain" {
			t.Errorf("%s ring: Seal returned %q, %q, %v; want the plaintext and no id", name, stored, id, err)
		}

		opened, err := ring.Open([]byte("plain"), "", nil)
		if err != nil || string(opened) != "plain" {
			t.Errorf("%s ring: Open of an unsealed value returned %q, %v", name, opened, err)
		}
	}
}

func TestKeyIDIsStableAndDistinct(t *testing.T) {
	t.Parallel()

	key := testKey(t)

	if KeyID(key) != KeyID(bytes.Clone(key)) {
		t.Error("the same key named twice gets two ids")
	}

	if KeyID(key) == KeyID(testKey(t)) {
		t.Error("two random keys share an id")
	}

	if len(KeyID(key)) != 8 {
		t.Errorf("id %q is not 8 characters", KeyID(key))
	}
}

func TestParseKeyAcceptsEveryBase64Form(t *testing.T) {
	t.Parallel()

	key := testKey(t)

	encodings := []*base64.Encoding{
		base64.StdEncoding,
		base64.RawStdEncoding,
		base64.URLEncoding,
		base64.RawURLEncoding,
	}

	for _, encoding := range encodings {
		parsed, err := ParseKey(" " + encoding.EncodeToString(key) + "\n")
		if err != nil {
			t.Errorf("parsing %q: %s", encoding.EncodeToString(key), err)

			continue
		}

		if !bytes.Equal(parsed, key) {
			t.Errorf("parsing %q returned a different key", encoding.EncodeToString(key))
		}
	}

	if _, err := ParseKey(base64.StdEncoding.EncodeToString(key[:16])); err == nil {
		t.Error("a 16-byte key was accepted")
	}

	if _, err := ParseKey("not base64 at all!"); err == nil {
		t.Error("a key that is not base64 was accepted")
	}
}

func TestNewKeyringRefusesAShortKey(t *testing.T) {
	t.Parallel()

	if _, err := NewKeyring(make([]byte, 16), nil); err == nil {
		t.Error("a 16-byte active key was accepted")
	}

	if _, err := NewKeyring(nil, [][]byte{make([]byte, 31)}); err == nil {
		t.Error("a 31-byte retired key was accepted")
	}
}

func TestIDsListsTheActiveKeyFirst(t *testing.T) {
	t.Parallel()

	active := testKey(t)
	retired := testKey(t)

	ring, err := NewKeyring(active, [][]byte{retired, active})
	if err != nil {
		t.Fatalf("building a ring: %s", err)
	}

	ids := ring.IDs()

	if len(ids) != 2 || ids[0] != KeyID(active) || ids[1] != KeyID(retired) {
		t.Errorf("IDs() = %v, want [%s %s]", ids, KeyID(active), KeyID(retired))
	}

	var none *Keyring

	if none.IDs() != nil || none.Holds("x") {
		t.Error("a nil ring claims keys")
	}
}

// sealStream writes data through a sealed stream and returns what reached the underlying writer.
func sealStream(t *testing.T, ring *Keyring, data []byte) []byte {
	t.Helper()

	var out bytes.Buffer

	w, err := ring.NewWriter(&out)
	if err != nil {
		t.Fatalf("starting a sealed stream: %s", err)
	}

	// Written in uneven pieces, so segment boundaries fall inside a Write rather than between them.
	for start := 0; start < len(data); start += 7919 {
		if _, err := w.Write(data[start:min(start+7919, len(data))]); err != nil {
			t.Fatalf("writing: %s", err)
		}
	}

	if err := w.Close(); err != nil {
		t.Fatalf("closing: %s", err)
	}

	return out.Bytes()
}

// openStream reads a whole stream through ring.
func openStream(ring *Keyring, sealed []byte) ([]byte, error) {
	r, err := ring.NewReader(bytes.NewReader(sealed))
	if err != nil {
		return nil, err
	}

	return io.ReadAll(r)
}

func TestStreamRoundTrip(t *testing.T) {
	t.Parallel()

	ring, _ := testRing(t)

	// Every length that puts the last segment somewhere different: empty, short, exactly one
	// segment (the case a reader cannot tell from length alone), one byte over, and several.
	for _, size := range []int{0, 1, segmentBytes - 1, segmentBytes, segmentBytes + 1, 3*segmentBytes + 17} {
		data := make([]byte, size)
		_, _ = rand.Read(data)

		sealed := sealStream(t, ring, data)

		if size > 64 && bytes.Contains(sealed, data[:64]) {
			t.Errorf("size %d: the sealed stream carries the plaintext", size)
		}

		opened, err := openStream(ring, sealed)
		if err != nil {
			t.Errorf("size %d: opening: %s", size, err)

			continue
		}

		if !bytes.Equal(opened, data) {
			t.Errorf("size %d: opened %d bytes that differ from the %d written", size, len(opened), len(data))
		}
	}
}

func TestStreamRefusesTruncation(t *testing.T) {
	t.Parallel()

	ring, _ := testRing(t)

	data := make([]byte, 2*segmentBytes+100)
	sealed := sealStream(t, ring, data)

	headerBytes := len(streamMagic) + 1 + 8 + saltBytes
	segment := segmentBytes + tagBytes

	cuts := map[string]int{
		// The final segment gone entirely: what is left ends on a boundary, and its last segment
		// was sealed as not final.
		"at a segment boundary": headerBytes + 2*segment,
		"inside a segment":      headerBytes + segment + 100,
		"after the header":      headerBytes,
	}

	for name, cut := range cuts {
		if _, err := openStream(ring, sealed[:cut]); !errors.Is(err, ErrTampered) {
			t.Errorf("cut %s: got %v, want ErrTampered", name, err)
		}
	}
}

func TestStreamRefusesAlterationAndReordering(t *testing.T) {
	t.Parallel()

	ring, _ := testRing(t)

	data := make([]byte, 2*segmentBytes+100)
	sealed := sealStream(t, ring, data)

	headerBytes := len(streamMagic) + 1 + 8 + saltBytes
	segment := segmentBytes + tagBytes

	altered := bytes.Clone(sealed)
	altered[headerBytes+segment+5] ^= 0x01

	if _, err := openStream(ring, altered); !errors.Is(err, ErrTampered) {
		t.Errorf("an altered segment: got %v, want ErrTampered", err)
	}

	swapped := bytes.Clone(sealed)
	copy(swapped[headerBytes:headerBytes+segment], sealed[headerBytes+segment:headerBytes+2*segment])
	copy(swapped[headerBytes+segment:headerBytes+2*segment], sealed[headerBytes:headerBytes+segment])

	if _, err := openStream(ring, swapped); !errors.Is(err, ErrTampered) {
		t.Errorf("two segments swapped: got %v, want ErrTampered", err)
	}

	salted := bytes.Clone(sealed)
	salted[headerBytes-1] ^= 0x01

	if _, err := openStream(ring, salted); !errors.Is(err, ErrTampered) {
		t.Errorf("an altered salt: got %v, want ErrTampered", err)
	}
}

func TestStreamUnderARetiredKey(t *testing.T) {
	t.Parallel()

	old, oldKey := testRing(t)
	sealed := sealStream(t, old, []byte("an old archive"))

	rotated, err := NewKeyring(testKey(t), [][]byte{oldKey})
	if err != nil {
		t.Fatalf("building the rotated ring: %s", err)
	}

	opened, err := openStream(rotated, sealed)
	if err != nil || string(opened) != "an old archive" {
		t.Errorf("opened %q, %v", opened, err)
	}

	stranger, _ := testRing(t)

	if _, err := openStream(stranger, sealed); !errors.Is(err, ErrUnknownKey) {
		t.Errorf("a ring without the key: got %v, want ErrUnknownKey", err)
	}
}

func TestUnsealedStreamsPassThrough(t *testing.T) {
	t.Parallel()

	ring, _ := testRing(t)

	for _, data := range [][]byte{nil, []byte("x"), []byte("\x1f\x8b a gzip archive, as every archive was")} {
		opened, err := openStream(ring, data)
		if err != nil || !bytes.Equal(opened, data) {
			t.Errorf("%q: read back %q, %v", data, opened, err)
		}
	}

	var none *Keyring

	var out bytes.Buffer

	w, err := none.NewWriter(&out)
	if err != nil {
		t.Fatalf("a nil ring's writer: %s", err)
	}

	_, _ = w.Write([]byte("plain"))
	_ = w.Close()

	if out.String() != "plain" || IsSealedStream(out.Bytes()) {
		t.Errorf("a nil ring's writer produced %q", out.String())
	}
}

func TestWriteAfterCloseFails(t *testing.T) {
	t.Parallel()

	ring, _ := testRing(t)

	w, _ := ring.NewWriter(io.Discard)
	_ = w.Close()

	if _, err := w.Write([]byte("late")); err == nil {
		t.Error("a write after Close succeeded")
	}

	if err := w.Close(); err != nil {
		t.Errorf("a second Close: %s", err)
	}
}
