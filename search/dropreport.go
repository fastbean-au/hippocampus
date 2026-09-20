package search

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// The two reasons an index operation is dropped, carried as an attribute on
// hippocampus.search.dropped and named in the summary line. They are the question an operator asks
// first and could not previously answer from the metric: a full queue means this service is
// offering work faster than one worker can apply it, an exhausted retry means the cluster would not
// take it. The remedies are unrelated - the first is a rate to reduce or a queue to widen, the
// second is a cluster to fix - which is why one counter for both was not enough.
const (
	dropQueueFull   = "queue_full"
	dropApplyFailed = "apply_failed"
)

// dropReportWindow is the shortest interval between two summary lines about dropped operations. A
// var so tests can shorten it.
//
// It exists because the per-drop warning is unusable at the rate it actually fires. Measured on a
// live deployment: 113,377 lines in thirty minutes - 63 a second - filling a 4 GB systemd journal
// almost entirely with one repeated sentence, which rotated away everything else the service had to
// say. A warning that fires 63 times a second is not a warning; it is a denial of service against
// the log an operator reads to diagnose it.
var dropReportWindow = 30 * time.Second

// dropReporter aggregates dropped index operations into one periodic log line, while the metric
// keeps counting every single one.
//
// The metric is the record and the log is the notification - that split is the whole design. A
// counter loses nothing to aggregation (a rate over any window is exactly as accurate at 63 drops a
// second as at one), whereas a log line is read by a person and repeats nothing useful after the
// first. So nothing is sampled away here: the summary states the total, so the line an operator
// sees carries MORE information than any one of the lines it replaces.
type dropReporter struct {
	mu sync.Mutex

	// counts is keyed "reason/op" so the summary can say which kind of operation is being lost as
	// well as how many. Both components are small closed sets, so this map is bounded.
	counts map[string]int64

	total int64

	// firstErr is the error behind the first apply failure in the window, kept because a cluster
	// failure's cause is not derivable from the counts and is the one thing the old per-drop line
	// carried that a summary otherwise loses.
	firstErr error

	// nextAt is when the window is next allowed to log. It starts at the zero time so the first
	// drop after a quiet period reports immediately rather than waiting out a window.
	nextAt time.Time

	// now is time.Now, replaced in tests.
	now func() time.Time
}

func newDropReporter() *dropReporter {
	return &dropReporter{
		counts: make(map[string]int64),
		now:    time.Now,
	}
}

// note records one dropped operation: it always counts the drop on the metric, and logs a summary
// when the window has elapsed.
func (d *dropReporter) note(kind opKind, reason string, cause error) {
	tel.dropped.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String("op", kind.String()),
		attribute.String("reason", reason),
	))

	d.mu.Lock()
	defer d.mu.Unlock()

	d.counts[reason+"/"+kind.String()]++
	d.total++

	if cause != nil && d.firstErr == nil {
		d.firstErr = cause
	}

	now := d.now()

	if now.Before(d.nextAt) {

		return
	}

	d.nextAt = now.Add(dropReportWindow)

	d.logLocked()
}

// flush emits whatever is still accumulated, whether or not the window has elapsed. Close calls it
// so the last burst before a shutdown is reported rather than left to the metric alone.
func (d *dropReporter) flush() {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.total == 0 {

		return
	}

	d.logLocked()
}

// logLocked writes the summary and resets the accumulation. The caller holds the mutex.
func (d *dropReporter) logLocked() {
	log.Warnf("dropped %d search index operation(s) (%s)%s", d.total, d.breakdownLocked(), d.causeLocked())

	d.counts = make(map[string]int64)
	d.total = 0
	d.firstErr = nil
}

// breakdownLocked renders the counts in a stable order, so two lines from one deployment can be
// compared by eye rather than only by parsing.
func (d *dropReporter) breakdownLocked() string {
	keys := make([]string, 0, len(d.counts))

	for k := range d.counts {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	parts := make([]string, 0, len(keys))

	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, d.counts[k]))
	}

	return strings.Join(parts, " ")
}

// causeLocked renders the first apply failure of the window, if there was one. A full queue has no
// cause to report - the queue was full - so this is empty in the common case.
func (d *dropReporter) causeLocked() string {
	if d.firstErr == nil {

		return ""
	}

	return "; first cause: " + d.firstErr.Error()
}
