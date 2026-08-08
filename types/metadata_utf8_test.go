package types

import (
	"strings"
	"testing"
)

// TestValidateMetadata_RejectsInvalidUTF8 covers the value charset check. The key charset is
// enforced separately (and is what keeps a key from escaping the JSON path in db/metadata.go); this
// is the value's own guard, and it matters because the column is JSON on two of the three dialects
// and invalid UTF-8 is not encodable there at all.
func TestValidateMetadata_RejectsInvalidUTF8(t *testing.T) {
	// A lone continuation byte: well under the length cap, and not valid UTF-8.
	metadata := map[string]string{"source": "slack\xc3\x28"}

	err := ValidateMetadata(metadata, "memory")
	if err == nil {
		t.Fatal("expected a metadata value that is not valid UTF-8 to be rejected")
	}

	if !strings.Contains(err.Error(), "not valid UTF-8") {
		t.Errorf("expected the message to name the fault, got %q", err)
	}

	if !strings.Contains(err.Error(), "source") {
		t.Errorf("expected the message to name the offending key, got %q", err)
	}
}

// TestValidateMetadata_AcceptsNonASCIIUTF8 is the other half: the guard is about encoding validity,
// not about restricting values to ASCII.
func TestValidateMetadata_AcceptsNonASCIIUTF8(t *testing.T) {
	metadata := map[string]string{"note": "café — naïve 日本語"}

	if err := ValidateMetadata(metadata, "memory"); err != nil {
		t.Errorf("expected valid non-ASCII UTF-8 to be accepted, got %s", err)
	}
}
