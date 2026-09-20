package search

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"
)

// captureWarnings redirects logrus for the duration of a test and returns the buffer.
func captureWarnings(t *testing.T) *bytes.Buffer {
	t.Helper()

	var buf bytes.Buffer

	restore := log.StandardLogger().Out
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(restore) })

	return &buf
}

// newTestDropReporter builds a reporter whose clock the test drives.
func newTestDropReporter(now *time.Time) *dropReporter {
	d := newDropReporter()
	d.now = func() time.Time { return *now }

	return d
}

// TestDropReporter_AggregatesWithinAWindow is the finding: the per-drop warning ran at 63 lines a
// second on a live deployment - 113,377 in thirty minutes - and filled a 4 GB journal with one
// repeated sentence. A thousand drops inside one window must produce one line, and that line must
// state the thousand.
func TestDropReporter_AggregatesWithinAWindow(t *testing.T) {
	buf := captureWarnings(t)

	now := time.Unix(1_700_000_000, 0)
	d := newTestDropReporter(&now)

	for range 1000 {
		d.note(opIndex, dropQueueFull, nil)
	}

	lines := warningLines(buf.String())

	if len(lines) != 1 {
		t.Fatalf("1000 drops produced %d log lines, want 1:\n%s", len(lines), buf.String())
	}

	// The first drop reports immediately with a count of one; the rest accumulate. The total arrives
	// with the next line, which is what flush is for.
	d.flush()

	all := warningLines(buf.String())

	if len(all) != 2 {
		t.Fatalf("expected the flush to emit the accumulated summary, got %d lines:\n%s", len(all), buf.String())
	}

	if !strings.Contains(all[1], "999") {
		t.Errorf("the summary did not state how many were dropped: %q", all[1])
	}
}

// TestDropReporter_LogsAgainAfterTheWindow: aggregation must not become silence. A drop after the
// window has elapsed reports, so a sustained problem keeps saying so.
func TestDropReporter_LogsAgainAfterTheWindow(t *testing.T) {
	buf := captureWarnings(t)

	now := time.Unix(1_700_000_000, 0)
	d := newTestDropReporter(&now)

	d.note(opIndex, dropQueueFull, nil)

	now = now.Add(dropReportWindow + time.Second)

	d.note(opIndex, dropQueueFull, nil)

	if got := len(warningLines(buf.String())); got != 2 {
		t.Errorf("a drop a window later produced %d lines in total, want 2:\n%s", got, buf.String())
	}
}

// TestDropReporter_NamesTheReasonAndTheOperation: the breakdown is the point of aggregating rather
// than sampling - one line that says more than any of the lines it replaced. A full queue and an
// exhausted retry have unrelated remedies, so they must be distinguishable.
func TestDropReporter_NamesTheReasonAndTheOperation(t *testing.T) {
	buf := captureWarnings(t)

	now := time.Unix(1_700_000_000, 0)
	d := newTestDropReporter(&now)

	d.note(opIndex, dropQueueFull, nil)
	d.note(opDeleteIds, dropApplyFailed, errors.New("cluster on fire"))
	d.note(opIndex, dropQueueFull, nil)

	d.flush()

	line := warningLines(buf.String())[1]

	for _, want := range []string{"queue_full/index=1", "apply_failed/delete_ids=1", "cluster on fire"} {
		if !strings.Contains(line, want) {
			t.Errorf("the summary is missing %q: %s", want, line)
		}
	}
}

// TestDropReporter_FlushIsQuietWhenThereIsNothingToSay: Close calls flush unconditionally, and a
// deployment that dropped nothing must not be told about it.
func TestDropReporter_FlushIsQuietWhenThereIsNothingToSay(t *testing.T) {
	buf := captureWarnings(t)

	now := time.Unix(1_700_000_000, 0)
	d := newTestDropReporter(&now)

	d.flush()

	if got := len(warningLines(buf.String())); got != 0 {
		t.Errorf("flushing an empty reporter logged %d lines:\n%s", got, buf.String())
	}
}

// warningLines pulls the log lines mentioning dropped operations out of captured output.
func warningLines(out string) []string {
	lines := make([]string, 0, 4)

	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "search index operation") {
			lines = append(lines, line)
		}
	}

	return lines
}

// TestRecordQueueDepth pins that the depth and capacity readings are taken from the live queue rather
// than from the configuration, and that reporting them never blocks or panics against a no-op meter -
// which is what every deployment with observability off has.
func TestRecordQueueDepth(t *testing.T) {
	o, err := NewOpenSearch(Config{
		Addresses: []string{"http://opensearch.invalid:9200"},
		Index:     "idx",
		QueueSize: 4,
		Transport: &mgetTransport{},
	})
	if err != nil {
		t.Fatalf("NewOpenSearch: %s", err)
	}

	t.Cleanup(func() { _ = o.Close() })

	if got := cap(o.queue); got != 4 {
		t.Fatalf("queue capacity is %d, want the configured 4", got)
	}

	o.recordQueueDepth()
}
