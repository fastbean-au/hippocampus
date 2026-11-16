// Package seal encrypts what the service keeps at rest: a memory body or a queued callback payload
// as one sealed value, and an export archive as a sealed stream. It is the whole of the service's
// cryptography for stored data, so the db package and the archive path share one implementation and
// one key ring.
//
// A Keyring holds an ACTIVE key, which seals every new value, and any number of RETIRED keys, which
// only open. Each key is named by an id derived from the key itself (KeyID), and the id is stored
// beside every sealed value, so a row written under an old key still opens after the active key has
// moved on, and a key configured with the wrong material is reported as a key that is not
// configured rather than as a value that failed to authenticate.
//
// Every value is sealed under a key of its own. The configured key is a key-ENCRYPTION key: each
// seal draws a random salt, and HKDF derives that value's AES-256-GCM key and nonce from the
// configured key and the salt. That is what makes the nonce budget a non-question - each derived key
// is used exactly once, so there is no count of seals after which a random nonce starts to repeat -
// and it costs 33 bytes a value: a version byte, the 16-byte salt and GCM's 16-byte tag. Wrapping a
// random data key under the configured one (envelope encryption in the KMS sense) would buy nothing
// more while the configured key lives in the process. Its value is that the key-encryption key can
// stay in a KMS, and that deployment is the kms subpackage's: there the key this package is handed
// is a data key the KMS unwrapped, and this package neither knows nor needs to.
package seal

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strings"
)

// KeyBytes is the length of a key: AES-256.
const KeyBytes = 32

// valueVersion is the first byte of every sealed value. A future format takes the next number, and
// Open refuses one it does not know rather than guessing.
const valueVersion = 1

// saltBytes is the per-value salt the value's key is derived from. 128 random bits put a repeated
// salt - and so a repeated key and nonce - out of reach for any number of values a store will hold.
const saltBytes = 16

// valueOverhead is what sealing adds to a value: the version byte, the salt and the GCM tag.
const valueOverhead = 1 + saltBytes + 16

// valueInfo is the HKDF info string for sealed values. It separates their derived keys from the
// stream's, so the same configured key can never yield the same key for both.
const valueInfo = "hippocampus seal value v1"

// keyIdInfo keys the HMAC that names a key. An HMAC rather than a plain hash of the key, so the id
// stored on every row is a value derived for that purpose and nothing else.
const keyIdInfo = "hippocampus key id v1"

// ErrUnknownKey is returned by Open, and by a stream reader, for a value sealed under a key this
// ring does not hold.
var ErrUnknownKey = errors.New("sealed under a key that is not configured")

// ErrTampered is returned when a sealed value or stream fails authentication: it was altered, it was
// truncated, or it was sealed for a different context than the one it is being opened in.
var ErrTampered = errors.New("sealed data failed authentication")

// ErrTooLarge is returned by Seal for a plaintext so long that its sealed size would overflow an int.
var ErrTooLarge = errors.New("too large to seal")

// Keyring is the set of keys the service seals and opens with. A nil *Keyring is valid and is the
// zero configuration: it seals nothing, and opens only what was never sealed.
type Keyring struct {
	// active is the id of the key new values are sealed with, or empty for a ring that only opens.
	active string

	keys map[string][]byte
}

// ParseKey decodes a configured key. Standard and URL-safe base64 are both accepted, padded or not,
// since every common generator emits one of the four; whatever the encoding, the key must be exactly
// KeyBytes long.
func ParseKey(encoded string) ([]byte, error) {
	encoded = strings.TrimSpace(encoded)

	encodings := []*base64.Encoding{
		base64.StdEncoding,
		base64.RawStdEncoding,
		base64.URLEncoding,
		base64.RawURLEncoding,
	}

	for _, encoding := range encodings {
		key, err := encoding.DecodeString(encoded)
		if err != nil {
			continue
		}

		if len(key) != KeyBytes {
			return nil, fmt.Errorf("a key must be %d bytes, this one is %d (generate one with `openssl rand -base64 %d`)",
				KeyBytes,
				len(key),
				KeyBytes,
			)
		}

		return key, nil
	}

	return nil, fmt.Errorf("a key must be base64-encoded (generate one with `openssl rand -base64 %d`)", KeyBytes)
}

// KeyID names a key. It is derived from the key rather than configured beside it, so it cannot be
// wrong: a ring given different material for a key that sealed a row reports that row's key as not
// configured, which says what is the matter, rather than failing to authenticate, which does not.
func KeyID(key []byte) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(keyIdInfo))

	return hex.EncodeToString(mac.Sum(nil)[:4])
}

// NewKeyring builds a ring from an active key and the retired keys it must still open. active may be
// nil, for a ring that opens what an earlier configuration sealed but seals nothing new.
func NewKeyring(active []byte, retired [][]byte) (*Keyring, error) {
	ring := &Keyring{keys: map[string][]byte{}}

	add := func(key []byte) (string, error) {
		if len(key) != KeyBytes {
			return "", fmt.Errorf("a key must be %d bytes, this one is %d", KeyBytes, len(key))
		}

		id := KeyID(key)

		// The same key listed twice is harmless. Two different keys sharing an id is a 32-bit
		// collision, which would make one of them unreachable, so it is refused rather than
		// resolved in favour of either.
		if existing, ok := ring.keys[id]; ok && !bytes.Equal(existing, key) {
			return "", fmt.Errorf("two different keys share the id %s; replace one of them", id)
		}

		ring.keys[id] = bytes.Clone(key)

		return id, nil
	}

	if active != nil {
		id, err := add(active)
		if err != nil {
			return nil, err
		}

		ring.active = id
	}

	for _, key := range retired {
		if _, err := add(key); err != nil {
			return nil, err
		}
	}

	return ring, nil
}

// Active returns the id of the key new values are sealed with, or "" when the ring seals nothing.
func (k *Keyring) Active() string {
	if k == nil {
		return ""
	}

	return k.active
}

// Holds reports whether the ring can open values sealed under the key with this id.
func (k *Keyring) Holds(id string) bool {
	if k == nil {
		return false
	}

	_, ok := k.keys[id]

	return ok
}

// IDs returns the id of every key the ring holds, active first.
func (k *Keyring) IDs() []string {
	if k == nil {
		return nil
	}

	ids := make([]string, 0, len(k.keys))

	if k.active != "" {
		ids = append(ids, k.active)
	}

	for id := range k.keys {
		if id == k.active {
			continue
		}

		ids = append(ids, id)
	}

	return ids
}

// Seal encrypts plaintext under the active key, bound to context: the same value opened with any
// other context fails authentication, which is what stops a sealed body being moved onto another
// row. It returns the bytes to store and the id to store beside them.
//
// A ring with no active key returns plaintext unchanged and an empty id, which is the stored form of
// a value that was never sealed - so a caller writes whatever Seal returns and needs no branch.
func (k *Keyring) Seal(plaintext []byte, context []byte) ([]byte, string, error) {
	if k.Active() == "" {
		return plaintext, "", nil
	}

	salt := make([]byte, saltBytes)

	// crypto/rand.Read does not fail on any supported platform, and panics rather than returning an
	// error if it ever cannot read; the check is for the signature.
	if _, err := rand.Read(salt); err != nil {
		return nil, "", fmt.Errorf("drawing a salt: %w", err)
	}

	aead, nonce, err := valueCipher(k.keys[k.active], salt)
	if err != nil {
		return nil, "", err
	}

	size, err := sealedCapacity(len(plaintext))
	if err != nil {
		return nil, "", err
	}

	sealed := make([]byte, 0, size)
	sealed = append(sealed, valueVersion)
	sealed = append(sealed, salt...)
	sealed = aead.Seal(sealed, nonce, plaintext, context)

	return sealed, k.active, nil
}

// sealedCapacity is the size of a sealed value whose plaintext is n bytes, refusing one whose size
// would overflow an int rather than allocating from a wrapped length.
func sealedCapacity(n int) (int, error) {
	if n > math.MaxInt-valueOverhead {
		return 0, fmt.Errorf("a %d-byte value: %w", n, ErrTooLarge)
	}

	return n + valueOverhead, nil
}

// Open reverses Seal. An empty id is a value that was never sealed, returned as stored; any other id
// must be one the ring holds, and the value must authenticate under it for this context.
func (k *Keyring) Open(stored []byte, id string, context []byte) ([]byte, error) {
	if id == "" {
		return stored, nil
	}

	if !k.Holds(id) {
		return nil, fmt.Errorf("key %s: %w", id, ErrUnknownKey)
	}

	if len(stored) < valueOverhead || stored[0] != valueVersion {
		return nil, fmt.Errorf("key %s: not a sealed value this build reads: %w", id, ErrTampered)
	}

	aead, nonce, err := valueCipher(k.keys[id], stored[1:1+saltBytes])
	if err != nil {
		return nil, err
	}

	plaintext, err := aead.Open(nil, nonce, stored[1+saltBytes:], context)
	if err != nil {
		return nil, fmt.Errorf("key %s: %w", id, ErrTampered)
	}

	return plaintext, nil
}

// valueCipher derives one value's AES-256-GCM key and nonce from the configured key and the value's
// salt. Both come out of one HKDF expansion: the key is unique to the salt, so the nonce need only be
// fixed for it, and deriving it costs nothing a stored nonce would not.
func valueCipher(key []byte, salt []byte) (cipher.AEAD, []byte, error) {
	material, err := hkdf.Key(sha256.New, key, salt, valueInfo, KeyBytes+12)
	if err != nil {
		return nil, nil, fmt.Errorf("deriving a value key: %w", err)
	}

	aead, err := newGCM(material[:KeyBytes])
	if err != nil {
		return nil, nil, err
	}

	return aead, material[KeyBytes:], nil
}

// newGCM builds AES-256-GCM over a key. Neither constructor can fail on a 32-byte key; the errors
// are returned rather than ignored because the length is a property of the caller, not of this.
func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("building the block cipher: %w", err)
	}

	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("building GCM: %w", err)
	}

	return aead, nil
}
