#!/usr/bin/env python3
"""One sample of a soak run, emitted as a CSV row.

Called on a timer by demo/soak.sh. Reads three sources, none of which it is allowed to fail on:
the collector's Prometheus (every service metric), the OpenSearch index (its document count, which
no service metric reports), and the local process table plus filesystem (RSS and free disk, neither
of which a Go process can honestly report about itself).

The rule throughout is that a source that does not answer yields an EMPTY FIELD rather than an
error. A soak run must not end because Prometheus was restarting or a query was momentarily
unscraped - a gap in one column of one row costs almost nothing, and losing the run costs hours.
The report treats empty as unknown and says so, which is the honest reading and is distinguishable
from a zero. That distinction matters more than it looks: zero goroutines and "we could not ask"
render identically if empty is coerced.
"""

import argparse
import csv
import json
import shutil
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

# The metrics sampled each interval, in report order: the CSV column name, and the PromQL that
# produces it. Names are the OTLP->Prometheus rendering (dots to underscores, counters gaining
# _total, second-unit histograms gaining _seconds), the same translation the shipped alert rules
# use - see cmd/hippocampus/alerts_test.go, which is what holds those names to the instruments.
#
# max() rather than sum() on every gauge is deliberate: a run may have more than one instance
# reporting (the horizontal-scaling profiles), and a gauge describing the STORE - used_bytes,
# capacity_pressure, outbox_depth - is the same number seen from each of them, so summing would
# multiply it by the replica count. Counters, describing work each instance did separately, do sum.
QUERIES = [
    # Process health. The leak signals, and the whole reason item 20 asks for goroutine counts:
    # a leak is invisible in a clean log and in every domain metric.
    ("goroutines", "max(hippocampus_runtime_goroutines{SEL})"),
    ("heap_bytes", "max(hippocampus_runtime_heap_bytes{SEL})"),
    ("go_memory_bytes", "max(hippocampus_runtime_memory_bytes{SEL})"),
    # Store size and whether eviction is converging on the target rather than chasing it.
    ("memories", "sum(hippocampus_memories_count{SEL})"),
    ("events", "max(hippocampus_events_count{SEL})"),
    ("used_bytes", "max(hippocampus_used_bytes{SEL})"),
    ("capacity_bytes", "max(hippocampus_capacity_bytes{SEL})"),
    ("capacity_pressure", "max(hippocampus_capacity_pressure{SEL})"),
    # The sleep cycle, which has grown from one scan to roughly six since the last soak ran.
    #
    # THE JUDGED FIGURE IS THE MEAN, read from the histogram's own sum and count. It replaced a p95
    # because a p95 is the wrong statistic for this sample size, not merely a noisy one: at the
    # demo's 120-second sleep period a 15-minute window holds about EIGHT cycles, so a 95th
    # percentile is asking where the 7.6th of 8 observations falls - which is a bucket boundary by
    # construction. On the 2026-08-31 MySQL run that produced a perfectly bimodal series (twelve
    # readings at 0.24s, twenty at 0.38-0.46s, nothing between: the top of one bucket and the
    # inside of the next) and a reported "+56% slower" on a cycle whose true mean moved 0.1494 ->
    # 0.1578s, or +5.6%, across three hours. No choice of bucket boundaries fixes that; only a
    # statistic the sample size supports does, and a mean converges far faster than a tail.
    #
    # A 30-minute window rather than 15, because ~15 cycles is a steadier mean than ~8 and the run
    # samples every five minutes anyway - consecutive windows overlap, which is what makes the
    # series smooth enough to fit a trend to.
    ("sleeps_ok", 'sum(hippocampus_sleeps_total{success="true"SEL}) or vector(0)'),
    ("sleeps_failed", 'sum(hippocampus_sleeps_total{success="false"SEL}) or vector(0)'),
    (
        "sleep_mean_seconds",
        "sum(rate(hippocampus_sleep_duration_seconds_sum{SEL}[30m])) "
        "/ sum(rate(hippocampus_sleep_duration_seconds_count{SEL}[30m]))",
    ),
    # Kept, and deliberately NOT judged. It costs one query, every stored run has it, and a tail
    # measure would become the more informative one on a deployment whose cycle rate is high enough
    # to support it. Read it as context, never as a trend - see above.
    (
        "sleep_p95_seconds",
        "histogram_quantile(0.95, sum by (le) (rate(hippocampus_sleep_duration_seconds_bucket{SEL}[15m])))",
    ),
    # Item 84's machinery. outbox_depth is the backpressure signal the in-memory queue could never
    # publish, and a depth that climbs and does not come back down is the failure this soak exists
    # to look for.
    ("outbox_depth", "max(hippocampus_search_outbox_depth{SEL})"),
    ("outbox_applied", "sum(hippocampus_search_outbox_applied_total{SEL}) or vector(0)"),
    ("outbox_abandoned", "sum(hippocampus_search_outbox_abandoned_total{SEL}) or vector(0)"),
    ("search_dropped", "sum(hippocampus_search_dropped_total{SEL}) or vector(0)"),
    ("stale_removed", "sum(hippocampus_search_stale_documents_removed_total{SEL}) or vector(0)"),
    # Faults. Counters, so the report reads them as deltas across the run.
    #
    # Every counter above and below carries `or vector(0)`, and no gauge does. A counter that has
    # never been incremented is never EXPORTED - the SDK has no data point to send - so Prometheus
    # holds no series and the query returns empty. For a counter, empty means zero and saying so is
    # the honest reading; the alternative had a soak run report "we could not tell whether anything
    # panicked" when what actually happened was that nothing did. A gauge is the opposite case: an
    # absent gauge is a subsystem that is not running, which is genuinely unknown, so those are
    # left to come back empty.
    (
        "rpc_server_errors",
        'sum(hippocampus_rpc_requests_total{outcome="server_error"SEL}) or vector(0)',
    ),
    ("rpc_requests", "sum(hippocampus_rpc_requests_total{SEL}) or vector(0)"),
    ("panics", "sum(hippocampus_panics_recovered_total{SEL}) or vector(0)"),
    ("ratelimit_rejected", "sum(hippocampus_ratelimit_rejected_total{SEL}) or vector(0)"),
]

def apply_selector(query, selector):
    """Substitute the {SEL} slot every query above carries.

    A deployment running several instances against one collector needs the sample scoped to one of
    them, or a store gauge is read across all of them while the index count beside it describes
    only one - a ratio between two different populations, which is worse than no ratio. The slot is
    written INSIDE the brace so that a query already carrying a matcher composes correctly:
    `{success="true"SEL}` becomes `{success="true",job="agent"}` or `{success="true"}`.

    Note the service's own metrics carry no instance label of their own by design (see item 60.1),
    so whether a selector is available at all depends on what the deployment's collector adds.
    """
    if not selector:
        return query.replace("{SEL}", "").replace("SEL}", "}")

    return query.replace("{SEL}", "{%s}" % selector).replace("SEL}", ",%s}" % selector)


# Columns the sampler fills in itself rather than reading from Prometheus.
LOCAL_COLUMNS = ["elapsed_seconds", "timestamp", "rss_bytes", "index_docs", "disk_free_bytes"]

COLUMNS = LOCAL_COLUMNS + [name for name, _ in QUERIES]


def http_json(url, timeout):
    """GET a URL and decode it as JSON, returning None on any failure whatsoever.

    Every caller treats None as "unknown", so there is deliberately no error path out of here.
    """
    try:
        with urllib.request.urlopen(url, timeout=timeout) as response:
            return json.load(response)
    except (urllib.error.URLError, OSError, ValueError, TimeoutError):
        return None


def prometheus_value(base_url, query, timeout):
    """Run one instant query and return its scalar value as a string, or "" if unavailable.

    An empty result vector - the metric exists but has no current sample, or the instrument has not
    reported yet - is "" rather than 0 for the reason in the module docstring.
    """
    url = "%s/api/v1/query?%s" % (
        base_url.rstrip("/"),
        urllib.parse.urlencode({"query": query}),
    )

    payload = http_json(url, timeout)
    if payload is None or payload.get("status") != "success":
        return ""

    result = payload.get("data", {}).get("result", [])
    if not result:
        return ""

    value = result[0].get("value", [None, None])[1]
    if value is None or value in ("NaN", "+Inf", "-Inf"):
        return ""

    return str(value)


def index_document_count(opensearch_url, index, timeout):
    """The index's document count - item 84's actual measurement.

    No service metric reports this, and it is the one number that says whether the store and its
    index have diverged: the finding that opened item 84 was 4.38M documents against 211,657 rows.
    """
    if not opensearch_url or not index:
        return ""

    payload = http_json(
        "%s/%s/_count" % (opensearch_url.rstrip("/"), urllib.parse.quote(index)), timeout
    )
    if payload is None or "count" not in payload:
        return ""

    return str(payload["count"])


def resident_bytes(pid):
    """Resident set size for a pid, in bytes, via ps.

    The Go runtime cannot report this about itself portably - hippocampus.runtime.memory_bytes is
    what it maps from the OS, which is a ceiling rather than the resident figure - so the real RSS
    is read from outside the process. ps reports it in kibibytes on both macOS and Linux.
    """
    if not pid:
        return ""

    try:
        output = subprocess.run(
            ["ps", "-o", "rss=", "-p", str(pid)],
            capture_output=True,
            text=True,
            timeout=10,
            check=False,
        )
    except (OSError, subprocess.SubprocessError):
        return ""

    value = output.stdout.strip()
    if not value.isdigit():
        return ""

    return str(int(value) * 1024)


def free_bytes(path):
    """Free space on the filesystem holding path.

    Sampled because this soak's own failure mode is filling the disk: an OpenSearch index that
    leaks documents is exactly what item 84 is about, and the host running this has ~10 GiB spare.
    soak.sh aborts the run on a floor rather than letting the host fill.
    """
    try:
        return str(shutil.disk_usage(path).free)
    except OSError:
        return ""


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--prometheus", default="http://127.0.0.1:9090")
    parser.add_argument("--opensearch", default="")
    parser.add_argument("--index", default="")
    parser.add_argument("--pid", default="")
    parser.add_argument("--disk-path", default=".")
    parser.add_argument(
        "--selector",
        default="",
        help='PromQL label matchers scoping every query to one instance, e.g. job="agent"',
    )
    parser.add_argument("--started", type=float, default=0.0, help="run start, unix seconds")
    parser.add_argument("--timeout", type=float, default=10.0)
    parser.add_argument(
        "--header", action="store_true", help="print the CSV header and exit"
    )
    args = parser.parse_args()

    writer = csv.writer(sys.stdout)

    if args.header:
        writer.writerow(COLUMNS)

        return 0

    now = time.time()
    started = args.started or now

    row = {
        "elapsed_seconds": "%d" % int(now - started),
        "timestamp": time.strftime("%Y-%m-%dT%H:%M:%S", time.localtime(now)),
        "rss_bytes": resident_bytes(args.pid),
        "index_docs": index_document_count(args.opensearch, args.index, args.timeout),
        "disk_free_bytes": free_bytes(args.disk_path),
    }

    for name, query in QUERIES:
        row[name] = prometheus_value(args.prometheus, apply_selector(query, args.selector), args.timeout)

    writer.writerow([row[column] for column in COLUMNS])

    return 0


if __name__ == "__main__":
    sys.exit(main())
