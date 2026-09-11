package keymap

import (
	"errors"
	"strings"
	"testing"
)

func TestMemoryIdRoundTrips(t *testing.T) {
	cases := []struct {
		name   string
		bucket string
		key    string
		id     string
	}{
		{name: "a plain key", bucket: "payloads", key: "traces/2026/09/abc.json", id: "payloads/traces/2026/09/abc.json"},
		{name: "a key with no prefix", bucket: "payloads", key: "abc", id: "payloads/abc"},
		{name: "a key that begins with a slash", bucket: "payloads", key: "/abc", id: "payloads//abc"},
		{name: "a key with spaces and unicode", bucket: "payloads", key: "a file — ok.txt", id: "payloads/a file — ok.txt"},
	}

	for _, v := range cases {
		t.Run(v.name, func(t *testing.T) {
			id, err := MemoryId(v.bucket, v.key)
			if err != nil {
				t.Fatalf("MemoryId(%q, %q) failed: %s", v.bucket, v.key, err.Error())
			}

			if id != v.id {
				t.Errorf("expected id %q, got %q", v.id, id)
			}

			bucket, key, err := Object(id)
			if err != nil {
				t.Fatalf("Object(%q) failed: %s", id, err.Error())
			}

			if bucket != v.bucket || key != v.key {
				t.Errorf("expected (%q, %q), got (%q, %q)", v.bucket, v.key, bucket, key)
			}
		})
	}
}

// The reversibility is the whole reason the id is not a hash: the forgotten log carries ids and
// never bodies, so an agent catching up after an outage has nothing else to name an object with.
func TestObjectRefusesWhatItDidNotMint(t *testing.T) {
	cases := []struct {
		name string
		id   string
	}{
		{name: "a uuid from another producer", id: "6f1c3d0e-4a5b-4c7d-8e9f-0a1b2c3d4e5f"},
		{name: "an empty id", id: ""},
		{name: "a bucket with no key", id: "payloads/"},
		{name: "a key with no bucket", id: "/abc"},
	}

	for _, v := range cases {
		t.Run(v.name, func(t *testing.T) {
			if _, _, err := Object(v.id); !errors.Is(err, ErrUnmappable) {
				t.Errorf("expected ErrUnmappable for %q, got %v", v.id, err)
			}
		})
	}
}

func TestMemoryIdRefusesWhatCannotBeAnId(t *testing.T) {
	cases := []struct {
		name   string
		bucket string
		key    string
	}{
		{name: "an empty bucket", bucket: "", key: "abc"},
		{name: "an empty key", bucket: "payloads", key: ""},
		{name: "a bucket with a slash", bucket: "pay/loads", key: "abc"},
		{name: "a key with a newline", bucket: "payloads", key: "a\nb"},
		{name: "a key with a NUL", bucket: "payloads", key: "a\x00b"},
		{name: "a key that is not valid UTF-8", bucket: "payloads", key: "a\xffb"},
		{name: "a key too long to be an id", bucket: "payloads", key: strings.Repeat("k", MaxIdRunes)},
	}

	for _, v := range cases {
		t.Run(v.name, func(t *testing.T) {
			if _, err := MemoryId(v.bucket, v.key); !errors.Is(err, ErrUnmappable) {
				t.Errorf("expected ErrUnmappable, got %v", err)
			}

			if Mappable(v.bucket, v.key) {
				t.Error("expected Mappable to agree with MemoryId")
			}
		})
	}
}

// The bound is on CHARACTERS rather than bytes, because that is what MySQL's VARCHAR(255) under
// utf8mb4 counts - a key of multi-byte runes that fits must not be refused.
func TestTheLengthBoundCountsCharacters(t *testing.T) {
	bucket := "b"
	key := strings.Repeat("é", MaxIdRunes-len(bucket)-1)

	id, err := MemoryId(bucket, key)
	if err != nil {
		t.Fatalf("expected a %d-character id to be accepted: %s", MaxIdRunes, err.Error())
	}

	if _, err := MemoryId(bucket, key+"x"); !errors.Is(err, ErrUnmappable) {
		t.Errorf("expected one character more to be refused, got %v", err)
	}

	if _, _, err := Object(id); err != nil {
		t.Errorf("expected the longest legal id to reverse: %s", err.Error())
	}
}
