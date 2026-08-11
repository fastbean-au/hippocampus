package rules

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const validDoc = `{"defaultAction":"drop","rules":[{"name":"r","expr":"event.name == 'keep'","action":"promote"}]}`

// write writes the rules file with an mtime distinct from the previous one, so reloadIfChanged sees
// a change. A test that wrote twice within the filesystem's mtime resolution would otherwise pass
// for the wrong reason.
func write(t *testing.T, path string, doc string, age time.Duration) {
	t.Helper()

	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatalf("writing %s: %s", path, err)
	}

	when := time.Now().Add(-age)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatalf("chtimes %s: %s", path, err)
	}
}

// TestNewWatcherFailsOnABadInitialLoad is the half that must fail closed: a named-but-broken rules
// file cannot be allowed to start an ingestor with no admission policy at all.
func TestNewWatcherFailsOnABadInitialLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rules.json")

	write(t, path, `{"defaultAction":"drop","rules":[{"name":"r","expr":"event.nope","action":"promote"}]}`, time.Minute)

	if _, err := NewWatcher(path, Options{}, time.Hour); err == nil {
		t.Fatal("expected a broken rules file to fail startup")
	}

	if _, err := NewWatcher(filepath.Join(dir, "absent.json"), Options{}, time.Hour); err == nil {
		t.Fatal("expected a missing rules file to fail startup")
	}
}

// TestWatcherReloadsOnChange covers the "apply to a running instance" requirement, and the two ways
// a reload can go wrong - unparseable JSON and an expression that does not compile - both of which
// must leave the last good ruleset serving rather than dropping to no rules.
func TestWatcherReloadsOnChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rules.json")
	write(t, path, validDoc, 2*time.Minute)

	// A refresh far in the future keeps the poller out of the way; the reloads here are driven by
	// hand so the test never sleeps.
	w, err := NewWatcher(path, Options{}, time.Hour)
	if err != nil {
		t.Fatalf("NewWatcher: %s", err)
	}
	defer w.Stop()

	if got := w.Current(); got.Rules() != 1 || got.DefaultAction() != ActionDrop {
		t.Fatalf("unexpected initial ruleset: %d rules, default %q", got.Rules(), got.DefaultAction())
	}

	first := w.Current()

	// An unchanged file is not re-read at all.
	if err := w.reloadIfChanged(); err != nil {
		t.Fatalf("reloadIfChanged: %s", err)
	}

	if w.Current() != first {
		t.Error("an unchanged file must not swap the ruleset")
	}

	// A good change lands.
	write(t, path, `{"defaultAction":"promote","rules":[]}`, time.Minute)

	if err := w.reloadIfChanged(); err != nil {
		t.Fatalf("reloadIfChanged: %s", err)
	}

	if got := w.Current(); got.DefaultAction() != ActionPromote || got.Rules() != 0 {
		t.Errorf("expected the new ruleset, got %d rules and default %q", got.Rules(), got.DefaultAction())
	}

	good := w.Current()

	// Malformed JSON: reported, discarded, last good kept.
	write(t, path, `{"defaultAction":`, 30*time.Second)

	if err := w.reloadIfChanged(); err == nil {
		t.Error("expected a malformed rules file to report an error")
	}

	if w.Current() != good {
		t.Error("a malformed reload must leave the last good ruleset in force")
	}

	// Well-formed JSON whose expression does not compile: the same.
	write(t, path, `{"defaultAction":"drop","rules":[{"name":"r","expr":"event.nope == 1","action":"promote"}]}`, 20*time.Second)

	err = w.reloadIfChanged()
	if err == nil {
		t.Error("expected an uncompilable expression to report an error")
	} else if !strings.Contains(err.Error(), "compiling expression") {
		t.Errorf("expected a compile error, got %q", err.Error())
	}

	if w.Current() != good {
		t.Error("a failed compile must leave the last good ruleset in force")
	}

	// The failed reload did not record the mtime, so a later good write is still picked up - a
	// watcher that remembered the broken file as "seen" would ignore the fix.
	write(t, path, validDoc, 10*time.Second)

	if err := w.reloadIfChanged(); err != nil {
		t.Fatalf("reloadIfChanged after a failure: %s", err)
	}

	if got := w.Current(); got.Rules() != 1 || got.DefaultAction() != ActionDrop {
		t.Errorf("expected the repaired ruleset to load, got %d rules and default %q", got.Rules(), got.DefaultAction())
	}
}

// TestWatcherPollPicksUpAChange exercises the goroutine itself, since everything else drives reload
// by hand.
func TestWatcherPollPicksUpAChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rules.json")
	write(t, path, validDoc, time.Minute)

	w, err := NewWatcher(path, Options{}, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("NewWatcher: %s", err)
	}
	defer w.Stop()

	write(t, path, `{"defaultAction":"promote","rules":[]}`, 0)

	deadline := time.Now().Add(2 * time.Second)

	for time.Now().Before(deadline) {
		if w.Current().DefaultAction() == ActionPromote {
			return
		}

		time.Sleep(5 * time.Millisecond)
	}

	t.Fatal("the poller did not pick up the changed rules file")
}

// TestStaticWatcher covers the no-file constructor, and that Stop is safe to call more than once on
// either kind.
func TestStaticWatcher(t *testing.T) {
	set, err := Parse([]byte(validDoc), Options{})
	if err != nil {
		t.Fatalf("Parse: %s", err)
	}

	w := NewStaticWatcher(set)

	if w.Current() != set {
		t.Error("expected the static watcher to serve the ruleset it was given")
	}

	w.Stop()
	w.Stop()
}
