package db

import (
	"testing"
	"unicode"
)

// FuzzContentTokens (TODO-3 item 167): the tokens are what reach three query languages, quoted per
// dialect, so not one operator character may survive the split, whatever the input.
func FuzzContentTokens(f *testing.F) {
	for _, seed := range []string{"deployment problem", `"a" OR b*`, "x & !y <-> z", "+a -b ~c (d)", "日本語 テキスト", "a\x00b", ""} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, text string) {
		for _, token := range contentTokens(text) {
			if token == "" {
				t.Fatalf("an empty token from %q", text)
			}

			for _, r := range token {
				if !unicode.IsLetter(r) && !unicode.IsNumber(r) {
					t.Fatalf("token %q from %q carries %q, which is neither a letter nor a number", token, text, r)
				}
			}
		}
	})
}
