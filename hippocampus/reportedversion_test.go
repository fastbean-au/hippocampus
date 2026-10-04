package hippocampus

import (
	"strings"
	"testing"
)

// TestSanitiseReportedVersion pins what the topology view keeps of a version the far end reports: a
// value it would have had to edit is dropped whole rather than shown as something never sent.
func TestSanitiseReportedVersion(t *testing.T) {
	cases := map[string]string{
		"hippo/0.52.0":              "hippo/0.52.0",
		"  hippo/0.52.0  ":          "hippo/0.52.0",
		"":                          "",
		"   ":                       "",
		"hippo/0.52.0\nX-Evil: yes": "",
		"hippo/0.52.0\x00":          "",
		"hippo/0.52.0 é":            "",
		strings.Repeat("v", 64):     strings.Repeat("v", 64),
		strings.Repeat("v", 65):     "",
	}

	for in, want := range cases {
		if got := sanitiseReportedVersion(in); got != want {
			t.Errorf("sanitiseReportedVersion(%q) = %q, want %q", in, got, want)
		}
	}
}
