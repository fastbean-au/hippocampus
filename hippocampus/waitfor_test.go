package hippocampus

import (
	"strings"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

// waitFor polls cond until it holds or the timeout passes, failing the test with what it was waiting
// for. It replaces a fixed sleep wherever a test waits for something to HAPPEN: the wait ends as
// soon as it does, so a slow machine costs time rather than a false failure, and the timeout is
// generous because reaching it means the behaviour is genuinely missing (TODO-3 item 177).
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(timeout)

	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", timeout, what)
		}

		time.Sleep(5 * time.Millisecond)
	}
}

// captureLogs records every log entry at debug and above for the rest of the test, restoring the
// level afterwards. For a test whose evidence is what the code logged - an error swallowed on
// purpose, or the absence of any work at all.
func captureLogs(t *testing.T) *logtest.Hook {
	t.Helper()

	level := log.GetLevel()
	log.SetLevel(log.DebugLevel)

	hook := logtest.NewGlobal()

	t.Cleanup(func() {
		hook.Reset()
		log.SetLevel(level)
	})

	return hook
}

// loggedContaining reports how many captured entries mention text.
func loggedContaining(hook *logtest.Hook, text string) int {
	count := 0

	for _, entry := range hook.AllEntries() {
		if strings.Contains(entry.Message, text) {
			count++
		}
	}

	return count
}

// testConfig is the smallest Config New accepts: a valid decay method and nothing else, so each test
// sets exactly the settings it is about. It replaces the viper.Set preambles the package's tests
// used before New took a Config (TODO-3 item 177).
func testConfig() Config {
	return Config{
		Consolidation: ConsolidationConfig{
			Method:            1,
			Aggressiveness:    1.0,
			UnitsOfAgeInDays:  1.0,
			DeletionThreshold: 1.0,
		},
	}
}
