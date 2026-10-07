package main

import (
	"path/filepath"
	"testing"

	"github.com/spf13/pflag"
	"github.com/spf13/viper"

	"github.com/fastbean-au/hippocampus/internal/flagdocs"
)

// TestEveryFlagIsDocumented holds this command's flags to the flag tables in docs/eventsource.md
// (TODO-3 item 175): the page, not --help, is where an operator finds out a flag exists.
func TestEveryFlagIsDocumented(t *testing.T) {
	viper.Reset()

	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)

	if err := registerFlags(fs, nil); err != nil {
		t.Fatalf("registerFlags failed: %s", err.Error())
	}

	flagdocs.Documented(t, fs, filepath.Join("..", "..", "..", "..", "docs", "eventsource.md"), nil)
}
