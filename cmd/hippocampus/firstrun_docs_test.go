package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/viper"
)

// TestFirstRunPagesDescribeTheDefaultSleepCycle holds the two first-run pages to what the built-in
// defaults actually do. They said sleep.periodSeconds had no default, "deliberately", so nothing was
// forgotten until Sleep was called - the design before setStartupDefaults gained an hourly cycle and
// a deletion threshold, for the reason its own comment gives. For a product whose premise is
// deletion, telling a first-time user their data persists until they ask is the most consequential
// sentence those pages could get wrong (TODO-3 item 147).
//
// It reads the default from setStartupDefaults rather than restating it, so the pages may claim
// there is no automatic cycle again only if the code stops running one.
func TestFirstRunPagesDescribeTheDefaultSleepCycle(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)

	setStartupDefaults()

	if viper.GetInt("sleep.periodSeconds") <= 0 {
		t.Skip("the built-in defaults run no timed cycle, so the pages may say so")
	}

	for _, page := range []string{"docs/getting-started.md", "docs/install.md"} {
		content, err := os.ReadFile(filepath.Join("..", "..", page))
		if err != nil {
			t.Fatalf("reading %s: %s", page, err)
		}

		flattened := strings.Join(strings.Fields(string(content)), " ")

		for _, claim := range []string{"no automatic consolidation", "nothing is forgotten until you ask"} {
			if strings.Contains(flattened, claim) {
				t.Errorf("%s says %q, but the built-in defaults run a cycle every %d seconds",
					page, claim, viper.GetInt("sleep.periodSeconds"))
			}
		}
	}
}
