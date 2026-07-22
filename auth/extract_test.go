package auth

import "testing"

// TestExtractBearerToken covers the header shapes ExtractBearerToken must accept or reject: a
// valid header, a missing header, the wrong scheme, case-insensitivity of the scheme, and a
// scheme with no token after it.
func TestExtractBearerToken(t *testing.T) {
	cases := []struct {
		name    string
		header  string
		want    string
		wantErr bool
	}{
		{name: "valid", header: "Bearer abc123", want: "abc123"},
		{name: "case insensitive scheme", header: "bearer abc123", want: "abc123"},
		{name: "extra whitespace after scheme", header: "Bearer   abc123", want: "abc123"},
		{name: "missing header", header: "", wantErr: true},
		{name: "wrong scheme", header: "Basic abc123", wantErr: true},
		{name: "scheme with no token", header: "Bearer ", wantErr: true},
		{name: "scheme with no separator", header: "Bearerabc123", wantErr: true},
		{name: "scheme with only whitespace after separator", header: "Bearer    ", wantErr: true},
	}

	for _, c := range cases {
		got, err := ExtractBearerToken(c.header)

		if c.wantErr {
			if err == nil {
				t.Errorf("%s: expected an error, got token %q", c.name, got)
			}

			continue
		}

		if err != nil {
			t.Errorf("%s: unexpected error: %s", c.name, err)

			continue
		}

		if got != c.want {
			t.Errorf("%s: expected token %q, got %q", c.name, c.want, got)
		}
	}
}
