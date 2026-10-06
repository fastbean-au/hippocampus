package types

import (
	"strings"
	"testing"
)

// FuzzParseMetadataFilters (TODO-3 item 167): a key this accepts is spliced into a JSON path as
// $."<key>" on every dialect, so it must never carry anything that could close the quote or start
// another path step, whatever the input.
func FuzzParseMetadataFilters(f *testing.F) {
	for _, seed := range []string{"tenant=acme", "a.b:c/d-e_f=", "=v", "k", "\"x\"=1", "$.a=1", "a[0]=1", "é=1", "a=b=c"} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, pair string) {
		filters, err := ParseMetadataFilters([]string{pair})
		if err != nil {
			return
		}

		for key := range filters {
			if key == "" || len(key) > MaxMetadataKeyLength {
				t.Fatalf("accepted a key of length %d from %q", len(key), pair)
			}

			if strings.ContainsAny(key, "\"$[]\\'` \t\n\r") {
				t.Fatalf("accepted key %q from %q, which can escape a JSON path", key, pair)
			}

			first := key[0]

			alphanumeric := (first >= 'a' && first <= 'z') || (first >= 'A' && first <= 'Z') || (first >= '0' && first <= '9')

			if !alphanumeric {
				t.Fatalf("accepted key %q, which does not start alphanumeric", key)
			}

			for i := 0; i < len(key); i++ {
				if key[i] >= 0x80 {
					t.Fatalf("accepted key %q, which is not ASCII", key)
				}
			}
		}
	})
}
