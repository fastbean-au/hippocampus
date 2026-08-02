package bridge

import (
	"testing"

	"github.com/spf13/pflag"
)

func TestRegisterCommonFlags(t *testing.T) {
	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	RegisterCommonFlags(fs)

	// A representative sample across the connection and transform groups, plus their defaults.
	wantDefaults := map[string]string{
		"address":              "localhost:50051",
		"call-timeout-seconds": "30",
		"significance":         "1",
		"group-from-subject":   "true",
		"log-level":            "info",
	}

	for name, want := range wantDefaults {
		f := fs.Lookup(name)
		if f == nil {
			t.Errorf("flag %q not registered", name)

			continue
		}

		if f.DefValue != want {
			t.Errorf("flag %q default = %q, want %q", name, f.DefValue, want)
		}
	}

	// The short flag for address must be -a.
	if f := fs.ShorthandLookup("a"); f == nil || f.Name != "address" {
		t.Errorf("shorthand -a should map to address")
	}

	// Parsing an empty arg list must succeed with the flags defined.
	if err := fs.Parse([]string{}); err != nil {
		t.Fatalf("Parse: %v", err)
	}
}
