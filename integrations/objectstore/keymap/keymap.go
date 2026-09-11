// Package keymap derives a pointer-memory's id from the object it points at, and reverses that
// derivation.
//
// It is the contract between every part of this integration and, more importantly, between this
// integration and whatever writes the pointer-memories in the first place. The tap reinforces
// keymap.MemoryId(bucket, key); the producer must have stored the memory under the same id, or
// every recall lands on nothing. That failure is SILENT by construction - a recall for an id the
// store does not hold is a no-op, which is the property that lets the tap hold no state - so the
// derivation lives in one exported, documented place rather than being open-coded per component,
// and the tap publishes a hit rate so a mismatch is visible as a number rather than as an absence.
//
// # Why the derivation is reversible
//
// The obvious choice is a hash of bucket and key: fixed width, no length limit, no character
// classes to worry about. It is the wrong one, and the reason is the catch-up path rather than
// anything about the tap.
//
// When a receiver has been unreachable long enough for the queue to give up, the only record of
// what was forgotten is the forgotten log, and a ForgottenMemory carries an id. It carries no body,
// deliberately - the log never records one - and no metadata. So an agent holding only a hash can
// read the log, learn that forty thousand memories went, and be unable to name a single object.
// The same is true of the memory_forgotten callback whenever bodies are not included in it. A
// reversible id is what makes both paths able to act, and it costs only a length bound.
//
// # What is not mappable
//
// The id column is VARCHAR(255) under utf8mb4 on MySQL, so an id is bounded at 255 characters. An
// object whose bucket and key do not fit, or whose key is not valid UTF-8 (a proto3 string must
// be), has no id here and cannot be managed by this integration at all.
//
// Such an object is not an error to be retried, and it is specifically NOT a candidate for
// deletion: the reverse sweep cannot tell an object it failed to map from an object nobody ever
// asked it to manage, and both look exactly like "the store does not hold a memory for this". The
// sweep therefore skips what it cannot map, which is the conservative direction - an unmanaged
// object outlives its budget, rather than a managed one being deleted on a guess.
package keymap

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// MaxIdRunes bounds a derived id. It is MySQL's id column width (VARCHAR(255) under utf8mb4, so
// 255 characters rather than bytes); SQLite and Postgres have no such bound, but an id that only
// works on two of the three dialects is not one this integration is prepared to mint.
const MaxIdRunes = 255

// ErrUnmappable reports an object this integration cannot address: one whose derived id would be
// too long, or whose bucket or key holds something an id may not. Callers match it with errors.Is
// and skip the object; see the package doc for why skipping is the only safe response.
var ErrUnmappable = errors.New("object cannot be mapped to a memory id")

// MemoryId returns the id of the pointer-memory for one object: the bucket and key joined by a
// slash, which is the reference an operator would write by hand anyway.
//
// The separator needs no escaping. A bucket name may not contain a slash in S3 or in any
// S3-compatible store, so the FIRST slash is unambiguously the separator however many the key
// holds - including a key that begins with one, which reverses to a key that begins with one.
func MemoryId(bucket string, key string) (string, error) {
	if err := validBucket(bucket); err != nil {
		return "", err
	}

	if err := validKey(key); err != nil {
		return "", err
	}

	id := bucket + "/" + key

	if n := utf8.RuneCountInString(id); n > MaxIdRunes {
		return "", fmt.Errorf("%w: the id would be %d characters, over the %d the store allows",
			ErrUnmappable, n, MaxIdRunes)
	}

	return id, nil
}

// Object reverses MemoryId, naming the object a memory id points at.
//
// It is deliberately strict rather than best-effort: an id this integration did not mint - a UUID
// from some other producer sharing the store, say - must be reported as unmappable rather than
// split into a plausible-looking bucket and key, because the caller on the other side of this
// function is about to delete whatever it names.
func Object(id string) (string, string, error) {
	bucket, key, found := strings.Cut(id, "/")
	if !found {
		return "", "", fmt.Errorf("%w: %q names no bucket and key", ErrUnmappable, id)
	}

	if err := validBucket(bucket); err != nil {
		return "", "", err
	}

	if err := validKey(key); err != nil {
		return "", "", err
	}

	if n := utf8.RuneCountInString(id); n > MaxIdRunes {
		return "", "", fmt.Errorf("%w: %d characters is over the %d the store allows", ErrUnmappable, n, MaxIdRunes)
	}

	return bucket, key, nil
}

// Mappable reports whether an object can be addressed at all, for a caller that wants the question
// rather than the id - the sweep, which asks it of every object it enumerates.
func Mappable(bucket string, key string) bool {
	_, err := MemoryId(bucket, key)

	return err == nil
}

func validBucket(bucket string) error {
	switch {

	case bucket == "":
		return fmt.Errorf("%w: the bucket is empty", ErrUnmappable)

	case strings.Contains(bucket, "/"):
		return fmt.Errorf("%w: the bucket %q contains a slash", ErrUnmappable, bucket)

	}

	return validText(bucket, "bucket")
}

func validKey(key string) error {
	if key == "" {
		return fmt.Errorf("%w: the key is empty", ErrUnmappable)
	}

	return validText(key, "key")
}

// validText refuses what an id may not carry. Invalid UTF-8 is refused because a memory id is a
// proto3 string and one carrying it would fail to marshal - a failure no redelivery could ever
// clear. Control characters are refused because an id reaches a log line, a URL path and an HTTP
// header, and one that can contain a newline or a NUL is a way to forge entries in all three.
func validText(s string, what string) error {
	if !utf8.ValidString(s) {
		return fmt.Errorf("%w: the %s is not valid UTF-8", ErrUnmappable, what)
	}

	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%w: the %s contains a control character", ErrUnmappable, what)
		}
	}

	return nil
}
