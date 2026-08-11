package rules

import (
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"
)

// DefaultRefresh is how often the watcher stats the rules file when no interval is configured.
const DefaultRefresh = 30 * time.Second

// Watcher holds the current Ruleset and reloads it when the file's mtime changes. It is the answer
// to "how are rule changes applied to a running instance", and it is modelled directly on
// auth.RevocationList, including the two behaviours that matter more than the mechanism: a bad
// INITIAL load fails startup, and a bad RELOAD is logged and discarded with the last good ruleset
// left serving.
//
// The current ruleset is read through an atomic load and every Ruleset is immutable, so a pass that
// takes a snapshot judges every event it holds against one consistent set of rules - a reload
// landing mid-pass affects the next event, never half of the one in flight.
type Watcher struct {
	path string
	opts Options

	current atomic.Pointer[Ruleset]

	mu       sync.Mutex
	lastMod  time.Time
	stopOnce sync.Once
	stop     chan struct{}
}

// NewWatcher loads path once - failing if it cannot be read, parsed or compiled, because a
// named-but-broken rules file must never be treated as "no rules" - and starts a goroutine that
// reloads it whenever its mtime changes. Call Stop to end the poller.
func NewWatcher(path string, opts Options, refresh time.Duration) (*Watcher, error) {
	log.Trace("func() rules.NewWatcher")

	w := &Watcher{
		path: path,
		opts: opts,
		stop: make(chan struct{}),
	}

	if err := w.reload(); err != nil {
		return nil, err
	}

	if refresh <= 0 {
		refresh = DefaultRefresh
	}

	go w.poll(refresh)

	return w, nil
}

// NewStaticWatcher wraps an already-built ruleset, for a caller that has no file to poll (tests, and
// a future rules-over-RPC surface). Stop is a no-op on one of these.
func NewStaticWatcher(set *Ruleset) *Watcher {
	w := &Watcher{stop: make(chan struct{})}
	w.current.Store(set)

	return w
}

// Current returns the ruleset in force. The returned value is immutable; hold it for the duration of
// one judgement rather than calling this per rule.
func (w *Watcher) Current() *Ruleset {
	return w.current.Load()
}

// Stop ends the background reload goroutine. It is idempotent.
func (w *Watcher) Stop() {
	w.stopOnce.Do(func() {
		close(w.stop)
	})
}

// poll reloads the file whenever its mtime advances, at the given interval, until Stop is called.
func (w *Watcher) poll(refresh time.Duration) {
	ticker := time.NewTicker(refresh)
	defer ticker.Stop()

	for {
		select {

		case <-w.stop:
			return

		case <-ticker.C:
			if err := w.reloadIfChanged(); err != nil {
				log.Errorf("rules reload failed, keeping the last good ruleset: %s", err.Error())
			}

		}
	}
}

// reloadIfChanged reloads only when the file's mtime differs from the last load, so an unchanged
// file costs a single stat per tick rather than a read, parse and recompile.
func (w *Watcher) reloadIfChanged() error {
	info, err := os.Stat(w.path)
	if err != nil {
		return fmt.Errorf("stat rules file: %w", err)
	}

	w.mu.Lock()
	unchanged := info.ModTime().Equal(w.lastMod)
	w.mu.Unlock()

	if unchanged {
		return nil
	}

	return w.reload()
}

// reload reads, parses and compiles the file, then atomically swaps in the new ruleset - recording
// the mtime only on success, so a failed reload is retried on the next tick rather than being
// remembered as done.
func (w *Watcher) reload() error {
	info, err := os.Stat(w.path)
	if err != nil {
		return fmt.Errorf("stat rules file: %w", err)
	}

	set, err := Load(w.path, w.opts)
	if err != nil {
		return err
	}

	w.current.Store(set)

	w.mu.Lock()
	w.lastMod = info.ModTime()
	w.mu.Unlock()

	log.Infof(
		"loaded %d rule(s) from %s, default action '%s'",
		set.Rules(),
		w.path,
		set.DefaultAction(),
	)

	return nil
}

// ReloadNow forces a reload regardless of mtime, for a caller driving the watcher by hand (tests, a
// SIGHUP handler). It obeys the same rule as the poller: on failure the last good ruleset stays.
func (w *Watcher) ReloadNow() error {
	return w.reload()
}
