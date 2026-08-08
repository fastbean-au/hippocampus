package main

import (
	"testing"
)

// TestMetadataLabels covers the --metadata flag's parsing, including the deliberate leniency: a
// malformed entry is dropped with a warning rather than failing startup, because the bridge's job
// is to keep consuming and one bad flag should not stop it.
func TestMetadataLabels(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want map[string]string
	}{
		{
			name: "none configured",
			args: nil,
			want: nil,
		},
		{
			name: "key=value pairs",
			args: []string{"--metadata", "source=broker", "--metadata", "env=prod"},
			want: map[string]string{"source": "broker", "env": "prod"},
		},
		{
			// Split on the FIRST '=', so a value may itself contain one.
			name: "value containing a separator",
			args: []string{"--metadata", "url=https://x/?a=b"},
			want: map[string]string{"url": "https://x/?a=b"},
		},
		{
			name: "malformed entries are dropped, not fatal",
			args: []string{"--metadata", "no-separator", "--metadata", "=novalue", "--metadata", "good=one"},
			want: map[string]string{"good": "one"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			setupFlags(t, test.args)

			got := metadataLabels()

			if len(got) != len(test.want) {
				t.Fatalf("metadataLabels() = %v, want %v", got, test.want)
			}

			for k, v := range test.want {
				if got[k] != v {
					t.Errorf("metadataLabels()[%q] = %q, want %q", k, got[k], v)
				}
			}
		})
	}
}
