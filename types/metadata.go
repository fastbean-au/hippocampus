package types

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

// Metadata validation bounds. Metadata is the multi-dimensional classification that group - one
// freeform 128-character label - could only ever carry one of, so it is client-supplied, opaque to
// the server, and stored per item. That combination is exactly what needs bounding: unbounded
// metadata is a body by another name. It would escape memory.limit.sizeBytes, distort UsedBytes and
// therefore capacity pressure, and become an eviction-accounting problem - which is why the
// serialised length is counted toward the body limit (see Memory.ValidateInsert) and toward the
// store's byte accounting rather than absorbed into db.evictionRowOverheadBytes' flat allowance.
//
// These are constants rather than configuration, like the link bounds above them: they exist to
// keep a client from turning the store into something it cannot account for, and a deployment that
// could raise them could defeat that. MaxMetadataBytes is the binding constraint - the per-key and
// per-value caps only stop one pathological entry consuming the whole budget - and is deliberately
// far above db.evictionRowOverheadBytes, which is why it must be measured rather than absorbed.
const (
	MaxMetadataKeys        = 32
	MaxMetadataKeyLength   = 64
	MaxMetadataValueLength = 512
	MaxMetadataBytes       = 4096
)

// metadataFilterSeparator splits a "key=value" filter pair. The split is on the FIRST separator, so
// a value may contain one; a key may not, which is what makes the packing unambiguous. Filters are
// packed into strings rather than sent as a map because GetMemories and GetEvents are GET routes
// and
// grpc-gateway cannot bind a map field from a URL query string.
const metadataFilterSeparator = "="

// ValidateMetadata checks one item's metadata against the bounds above. kind names the item type
// for
// the error message ("memory"/"event"), matching ValidateLinks.
//
// It does NOT check the total against memory.limit.sizeBytes - that limit covers the body too, so
// the combined check belongs with the body, in Memory.ValidateInsert.
func ValidateMetadata(metadata map[string]string, kind string) error {
	if len(metadata) == 0 {
		return nil
	}

	if len(metadata) > MaxMetadataKeys {
		return fmt.Errorf("%s not valid - too many metadata keys (max %d)", kind, MaxMetadataKeys)
	}

	for _, k := range sortedMetadataKeys(metadata) {
		v := metadata[k]

		switch {

		case !validMetadataKey(k):
			return fmt.Errorf(
				"%s not valid - metadata key '%s' must match [A-Za-z0-9][A-Za-z0-9._:/-]{0,%d}",
				kind, k, MaxMetadataKeyLength-1,
			)

		case len(v) > MaxMetadataValueLength:
			return fmt.Errorf(
				"%s not valid - metadata value for key '%s' too long (max %d bytes)",
				kind, k, MaxMetadataValueLength,
			)

		case !utf8.ValidString(v):
			return fmt.Errorf("%s not valid - metadata value for key '%s' is not valid UTF-8", kind, k)

		}
	}

	if size := MetadataSerialisedLen(metadata); size > MaxMetadataBytes {
		return fmt.Errorf(
			"%s not valid - metadata too large (%d bytes serialised, max %d)",
			kind, size, MaxMetadataBytes,
		)
	}

	return nil
}

// validMetadataKey reports whether a key matches
// [A-Za-z0-9][A-Za-z0-9._:/-]{0,MaxMetadataKeyLength-1}.
//
// The charset is narrow for two specific reasons rather than out of caution. Excluding '=' is what
// makes the "key=value" filter packing unambiguous. Excluding '"', '$' and '[' is what makes
// `$."<key>"` always a well-formed JSON path, so the db layer can bind a filter key as a query
// parameter instead of concatenating it into SQL - and so a malformed key is an InvalidArgument at
// the RPC boundary rather than a driver error (MySQL's JSON_EXTRACT raises ER_INVALID_JSON_PATH,
// which would surface as Internal).
//
// It is written as an explicit scan rather than a regexp so it stays allocation-free on the write
// path, which runs it per key per stored item.
func validMetadataKey(key string) bool {
	if len(key) == 0 || len(key) > MaxMetadataKeyLength {
		return false
	}

	for i := 0; i < len(key); i++ {
		c := key[i]

		alphanumeric := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')

		// The first character must be alphanumeric, so a key can never be mistaken for a JSON path
		// operator or a flag.
		if i == 0 {
			if !alphanumeric {
				return false
			}

			continue
		}

		if !alphanumeric && c != '.' && c != '_' && c != ':' && c != '/' && c != '-' {
			return false
		}
	}

	return true
}

// sortedMetadataKeys returns the keys in a stable order. Every place metadata is serialised,
// validated, rendered, or turned into SQL sorts first: Go map iteration is randomised, and without
// this the serialised length would vary between calls on the same map, the SQL a filter builds
// would
// vary between identical requests, and the CLI's text output would not be reproducible.
func sortedMetadataKeys(metadata map[string]string) []string {
	keys := make([]string, 0, len(metadata))

	for k := range metadata {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	return keys
}

// MetadataSerialisedLen is the byte length metadata occupies once stored, and is what both the
// memory.limit.sizeBytes check and the transfer batch sizer measure. It is the length of exactly
// what MarshalMetadata produces, so the figure a write is validated against is the figure the store
// will hold.
func MetadataSerialisedLen(metadata map[string]string) int {
	if len(metadata) == 0 {
		return 0
	}

	encoded, err := marshalMetadata(metadata)
	if err != nil {
		return 0
	}

	return len(encoded)
}

// MarshalMetadata encodes metadata for storage, returning nil for an empty map.
//
// nil - stored as SQL NULL - rather than "" or "{}" is load-bearing, not tidiness. SQLite's
// json_extract raises "malformed JSON" on an empty string, so a column defaulting to the empty
// string the way group_name does would make the FIRST metadata-filtered query fail against every
// row written before the column existed. NULL is what json_extract, ->> and JSON_EXTRACT all return
// nothing for, so a row with no metadata is uniformly excluded by a key predicate on all three
// drivers.
func MarshalMetadata(metadata map[string]string) (any, error) {
	if len(metadata) == 0 {
		return nil, nil
	}

	encoded, err := marshalMetadata(metadata)
	if err != nil {
		return nil, err
	}

	return string(encoded), nil
}

// marshalMetadata encodes with sorted keys. encoding/json already sorts map keys, but relying on
// that would make the determinism MetadataSerialisedLen and the tests depend on an implementation
// detail of the standard library rather than something this package states.
func marshalMetadata(metadata map[string]string) ([]byte, error) {
	var b strings.Builder

	b.WriteByte('{')

	for i, k := range sortedMetadataKeys(metadata) {
		if i > 0 {
			b.WriteByte(',')
		}

		key, err := json.Marshal(k)
		if err != nil {
			return nil, fmt.Errorf("failed to encode metadata key '%s': %w", k, err)
		}

		value, err := json.Marshal(metadata[k])
		if err != nil {
			return nil, fmt.Errorf("failed to encode metadata value for key '%s': %w", k, err)
		}

		b.Write(key)
		b.WriteByte(':')
		b.Write(value)
	}

	b.WriteByte('}')

	return []byte(b.String()), nil
}

// UnmarshalMetadata decodes a stored metadata column. A NULL or empty column reads as nil rather
// than an empty map, so a round trip through the store leaves an item exactly as it was written.
//
// src is whatever the driver scanned: []byte on SQLite and MySQL, and either []byte or string on
// Postgres depending on how the JSONB column comes back.
func UnmarshalMetadata(src any) (map[string]string, error) {
	var raw []byte

	switch v := src.(type) {

	case nil:
		return nil, nil

	case []byte:
		raw = v

	case string:
		raw = []byte(v)

	default:
		return nil, fmt.Errorf("failed to decode metadata: unexpected column type %T", src)

	}

	if len(raw) == 0 {
		return nil, nil
	}

	var metadata map[string]string

	if err := json.Unmarshal(raw, &metadata); err != nil {
		return nil, fmt.Errorf("failed to decode metadata: %w", err)
	}

	if len(metadata) == 0 {
		return nil, nil
	}

	return metadata, nil
}

// CopyMetadata returns a copy of a metadata map, or nil for an empty one. Conversions to and from
// the proto messages copy rather than alias, because the proto map belongs to the caller and a
// stored item aliasing it would change under a caller that reused the message.
func CopyMetadata(metadata map[string]string) map[string]string {
	if len(metadata) == 0 {
		return nil
	}

	out := make(map[string]string, len(metadata))

	for k, v := range metadata {
		out[k] = v
	}

	return out
}

// ParseMetadataFilters turns the repeated "key=value" filter strings into the map the store filters
// on, splitting each on the first separator. Every pair must match for an item to be returned.
//
// The key is validated with the same rule a written key is - see validMetadataKey for why that
// matters here and not only on the write path. The value is not: it is bound as a query parameter
// and compared for equality, so it needs no charset, and a value that could never have been stored
// simply matches nothing.
func ParseMetadataFilters(pairs []string) (map[string]string, error) {
	if len(pairs) == 0 {
		return nil, nil
	}

	if len(pairs) > MaxMetadataKeys {
		return nil, fmt.Errorf("too many metadata filters (max %d)", MaxMetadataKeys)
	}

	filters := make(map[string]string, len(pairs))

	for _, pair := range pairs {
		key, value, found := strings.Cut(pair, metadataFilterSeparator)

		switch {

		case !found:
			return nil, fmt.Errorf("metadata filter '%s' is not a 'key%svalue' pair", pair, metadataFilterSeparator)

		case !validMetadataKey(key):
			return nil, fmt.Errorf(
				"metadata filter key '%s' must match [A-Za-z0-9][A-Za-z0-9._:/-]{0,%d}",
				key, MaxMetadataKeyLength-1,
			)

		}

		// A repeated key is rejected rather than letting the last one win: two values for one key
		// can never both match, so the request as written can only have been a mistake.
		if existing, ok := filters[key]; ok && existing != value {
			return nil, fmt.Errorf("metadata filter key '%s' given twice with different values", key)
		}

		filters[key] = value
	}

	return filters, nil
}

// MetadataToTerms renders metadata as the sorted "key=value" strings the search index holds.
//
// The index stores metadata as a keyword array in exactly this shape rather than as an object,
// because the keys are client-supplied: an object mapping would mint a new index field per distinct
// key and a client generating keys per request would exhaust the cluster's field limit. A keyword
// array cannot, term-filters exactly, ANDs naturally as several term filters, and is byte-identical
// to the filter's wire form - so a filter needs no conversion to become a term.
func MetadataToTerms(metadata map[string]string) []string {
	if len(metadata) == 0 {
		return nil
	}

	terms := make([]string, 0, len(metadata))

	for _, k := range sortedMetadataKeys(metadata) {
		terms = append(terms, k+metadataFilterSeparator+metadata[k])
	}

	return terms
}
