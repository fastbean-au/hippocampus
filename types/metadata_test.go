package types

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// TestValidateMetadataBounds drives each bound in turn. The bounds are the whole reason metadata is
// safe to store - an unbounded map escapes memory.limit.sizeBytes and corrupts the store's byte
// accounting - so each one is pinned rather than trusted to the switch reading correctly.
func TestValidateMetadataBounds(t *testing.T) {
	tooManyKeys := make(map[string]string, MaxMetadataKeys+1)
	for i := 0; i <= MaxMetadataKeys; i++ {
		tooManyKeys["k"+strings.Repeat("x", i)] = "v"
	}

	// Under the key count and every per-entry cap, but over the total budget - which is what makes
	// MaxMetadataBytes the binding constraint rather than the per-entry caps.
	tooLarge := make(map[string]string, MaxMetadataKeys)
	for i := 0; i < MaxMetadataKeys; i++ {
		tooLarge[fmt.Sprintf("key%02d", i)] = strings.Repeat("v", MaxMetadataValueLength)
	}

	cases := []struct {
		name     string
		metadata map[string]string
		wantErr  string
	}{
		{"nil is valid", nil, ""},
		{"empty is valid", map[string]string{}, ""},
		{"ordinary pair", map[string]string{"source": "slack"}, ""},
		{"every allowed key character", map[string]string{"a0._:/-": "v"}, ""},
		{"empty value is allowed", map[string]string{"source": ""}, ""},
		{"unicode value", map[string]string{"author": "Ann-Sofie Ø"}, ""},
		{"key at the cap", map[string]string{strings.Repeat("k", MaxMetadataKeyLength): "v"}, ""},
		{"value at the cap", map[string]string{"k": strings.Repeat("v", MaxMetadataValueLength)}, ""},

		{"too many keys", tooManyKeys, "too many metadata keys"},
		{"key over the cap", map[string]string{strings.Repeat("k", MaxMetadataKeyLength+1): "v"}, "must match"},
		{"value over the cap", map[string]string{"k": strings.Repeat("v", MaxMetadataValueLength+1)}, "too long"},
		{"total over the budget", tooLarge, "too large"},

		// The charset is narrow for two specific reasons: '=' would make the "key=value" filter
		// packing ambiguous, and '"'/'$'/'[' would let a key escape the JSON path the db layer binds.
		{"empty key", map[string]string{"": "v"}, "must match"},
		{"key with an equals", map[string]string{"a=b": "v"}, "must match"},
		{"key with a quote", map[string]string{`a"b`: "v"}, "must match"},
		{"key with a dollar", map[string]string{"$a": "v"}, "must match"},
		{"key with a bracket", map[string]string{"a[0]": "v"}, "must match"},
		{"key with a space", map[string]string{"a b": "v"}, "must match"},
		{"key starting with punctuation", map[string]string{"-a": "v"}, "must match"},
		{"key starting with a dot", map[string]string{".a": "v"}, "must match"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateMetadata(c.metadata, "memory")

			switch {

			case c.wantErr == "" && err != nil:
				t.Fatalf("expected valid metadata, got error: %s", err)

			case c.wantErr != "" && err == nil:
				t.Fatalf("expected an error containing %q, got none", c.wantErr)

			case c.wantErr != "" && !strings.Contains(err.Error(), c.wantErr):
				t.Fatalf("expected an error containing %q, got: %s", c.wantErr, err)

			}
		})
	}
}

// TestValidateMetadataNamesTheKind checks the error carries the item type, matching ValidateLinks -
// the same function validates memories and events, and an error that does not say which is being
// rejected is unhelpful on a StoreEvent carrying nested memories.
func TestValidateMetadataNamesTheKind(t *testing.T) {
	err := ValidateMetadata(map[string]string{"a=b": "v"}, "event")
	if err == nil || !strings.HasPrefix(err.Error(), "event not valid") {
		t.Fatalf("expected an 'event not valid' error, got: %v", err)
	}
}

// TestMetadataSerialisedLenIsStable is the reason sortedMetadataKeys exists. Go randomises map
// iteration, so a length that varied between calls would make the memory.limit.sizeBytes check
// non-deterministic - the same write accepted or rejected depending on iteration order.
func TestMetadataSerialisedLenIsStable(t *testing.T) {
	metadata := map[string]string{
		"source": "slack", "project": "hippocampus", "author": "sean",
		"env": "prod", "team": "platform", "region": "ap-southeast-2",
	}

	first := MetadataSerialisedLen(metadata)
	if first == 0 {
		t.Fatal("expected a non-zero serialised length")
	}

	for i := 0; i < 20; i++ {
		if got := MetadataSerialisedLen(metadata); got != first {
			t.Fatalf("serialised length varied between calls: %d then %d", first, got)
		}
	}

	if got := MetadataSerialisedLen(nil); got != 0 {
		t.Errorf("expected nil metadata to serialise to 0 bytes, got %d", got)
	}

	if got := MetadataSerialisedLen(map[string]string{}); got != 0 {
		t.Errorf("expected empty metadata to serialise to 0 bytes, got %d", got)
	}
}

// TestMarshalMetadataEmptyIsNil pins the decision that makes a metadata filter safe against rows
// written before the column existed: an empty map is stored as SQL NULL, never "" and never "{}".
// SQLite's json_extract raises "malformed JSON" on an empty string, so a column defaulting to one
// would make the first metadata-filtered query fail against every pre-migration row.
func TestMarshalMetadataEmptyIsNil(t *testing.T) {
	for _, metadata := range []map[string]string{nil, {}} {
		encoded, err := MarshalMetadata(metadata)
		if err != nil {
			t.Fatalf("failed to marshal empty metadata: %s", err)
		}

		if encoded != nil {
			t.Errorf("expected empty metadata to marshal to nil, got %#v", encoded)
		}
	}
}

// TestMetadataRoundTrip walks a map out to storage form and back, including the characters that
// would break a hand-rolled encoder.
func TestMetadataRoundTrip(t *testing.T) {
	metadata := map[string]string{
		"source":  "slack",
		"project": `a "quoted" value, with a = and a \backslash`,
		"author":  "Ann-Sofie Ø",
		"empty":   "",
	}

	encoded, err := MarshalMetadata(metadata)
	if err != nil {
		t.Fatalf("failed to marshal metadata: %s", err)
	}

	// Stored as a string; the drivers hand it back as []byte or string depending on the dialect, so
	// both are exercised.
	text, ok := encoded.(string)
	if !ok {
		t.Fatalf("expected metadata to marshal to a string, got %T", encoded)
	}

	for _, src := range []any{text, []byte(text)} {
		decoded, err := UnmarshalMetadata(src)
		if err != nil {
			t.Fatalf("failed to unmarshal metadata from %T: %s", src, err)
		}

		if !reflect.DeepEqual(decoded, metadata) {
			t.Errorf("round trip through %T changed the metadata: %#v", src, decoded)
		}
	}
}

// TestUnmarshalMetadataEmptySources covers every way a row with no metadata comes back. All of them
// must read as nil rather than an empty map, so a store round trip leaves an item exactly as
// written and ToProto keeps omitting the field.
func TestUnmarshalMetadataEmptySources(t *testing.T) {
	for _, src := range []any{nil, []byte(nil), []byte{}, "", []byte("{}"), "{}"} {
		decoded, err := UnmarshalMetadata(src)
		if err != nil {
			t.Fatalf("failed to unmarshal %#v: %s", src, err)
		}

		if decoded != nil {
			t.Errorf("expected %#v to decode to nil metadata, got %#v", src, decoded)
		}
	}
}

// TestUnmarshalMetadataRejectsGarbage keeps a corrupt or unexpected column an error rather than a
// silently empty map - a read that quietly dropped metadata would be indistinguishable from an item
// that never had any.
func TestUnmarshalMetadataRejectsGarbage(t *testing.T) {
	cases := []struct {
		name string
		src  any
	}{
		{"not json", []byte("not json")},
		{"a json array", []byte(`["a","b"]`)},
		{"non-string values", []byte(`{"a":1}`)},
		{"an unexpected column type", 42},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := UnmarshalMetadata(c.src); err == nil {
				t.Fatalf("expected an error for %#v", c.src)
			}
		})
	}
}

// TestCopyMetadataDoesNotAlias verifies the conversions copy. The proto map belongs to the caller,
// so a stored item aliasing it would change under a caller that reused the message.
func TestCopyMetadataDoesNotAlias(t *testing.T) {
	original := map[string]string{"source": "slack"}

	copied := CopyMetadata(original)
	copied["source"] = "email"

	if original["source"] != "slack" {
		t.Errorf("CopyMetadata aliased the original map: %#v", original)
	}

	if CopyMetadata(nil) != nil || CopyMetadata(map[string]string{}) != nil {
		t.Error("expected empty metadata to copy to nil")
	}
}

// TestParseMetadataFilters covers the "key=value" packing, which exists because grpc-gateway cannot
// bind a map field from a URL query string. The first-separator split is the part worth pinning: a
// value may contain '=' and a key may not, which is what makes the packing unambiguous.
func TestParseMetadataFilters(t *testing.T) {
	cases := []struct {
		name    string
		pairs   []string
		want    map[string]string
		wantErr string
	}{
		{"none", nil, nil, ""},
		{"empty slice", []string{}, nil, ""},
		{"one pair", []string{"source=slack"}, map[string]string{"source": "slack"}, ""},
		{
			"two pairs",
			[]string{"source=slack", "project=x"},
			map[string]string{"source": "slack", "project": "x"},
			"",
		},
		{"value containing the separator", []string{"q=a=b"}, map[string]string{"q": "a=b"}, ""},
		{"empty value", []string{"source="}, map[string]string{"source": ""}, ""},
		{"repeated key, same value", []string{"a=1", "a=1"}, map[string]string{"a": "1"}, ""},

		{"no separator", []string{"novalue"}, nil, "not a 'key=value' pair"},
		{"no key", []string{"=v"}, nil, "must match"},
		{"invalid key", []string{"a b=v"}, nil, "must match"},
		{"repeated key, different values", []string{"a=1", "a=2"}, nil, "given twice"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParseMetadataFilters(c.pairs)

			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("expected an error containing %q, got: %v", c.wantErr, err)
				}

				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %s", err)
			}

			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("expected %#v, got %#v", c.want, got)
			}
		})
	}
}

// TestParseMetadataFiltersBoundsCount stops a caller turning one list request into an arbitrarily
// large conjunction of unindexed predicates.
func TestParseMetadataFiltersBoundsCount(t *testing.T) {
	pairs := make([]string, 0, MaxMetadataKeys+1)
	for i := 0; i <= MaxMetadataKeys; i++ {
		pairs = append(pairs, "k"+strings.Repeat("x", i)+"=v")
	}

	if _, err := ParseMetadataFilters(pairs); err == nil || !strings.Contains(err.Error(), "too many") {
		t.Fatalf("expected a 'too many' error, got: %v", err)
	}
}

// TestMetadataToTerms pins the search index's storage shape: sorted "key=value" strings, byte
// identical to the filter's wire form so a filter needs no conversion to become a term. The index
// holds a keyword array rather than an object because the keys are client-supplied and an object
// mapping would mint an index field per distinct key.
func TestMetadataToTerms(t *testing.T) {
	got := MetadataToTerms(map[string]string{"source": "slack", "author": "sean", "project": "x"})

	want := []string{"author=sean", "project=x", "source=slack"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("expected %#v, got %#v", want, got)
	}

	if MetadataToTerms(nil) != nil || MetadataToTerms(map[string]string{}) != nil {
		t.Error("expected empty metadata to produce no terms")
	}
}

// TestMetadataTermsMatchFilterWireForm is the pairing the OpenSearch backend depends on: a term
// built from stored metadata must equal the filter string a caller sends for that same pair, or a
// term filter would never match.
func TestMetadataTermsMatchFilterWireForm(t *testing.T) {
	metadata := map[string]string{"source": "slack", "q": "a=b"}

	terms := MetadataToTerms(metadata)

	parsed, err := ParseMetadataFilters(terms)
	if err != nil {
		t.Fatalf("terms did not parse back as filters: %s", err)
	}

	if !reflect.DeepEqual(parsed, metadata) {
		t.Errorf("terms did not round trip through the filter form: %#v", parsed)
	}
}
