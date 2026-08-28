#!/usr/bin/env python3
"""Turn a soak run's samples into a report with verdicts.

The point of this file is the verdicts, not the table. The last soak run (item 20.4, 2026-07-15)
produced a clean log and was recorded as clean, and a clean log is exactly what a goroutine leak, a
sleep cycle degrading under accumulated per-cycle work, and an eviction loop that oscillates
instead of converging all look like. So each check below states what it measured, what it compared
it against, and which of the three answers it reached - and a check whose data is missing says
UNKNOWN rather than passing by default, because a check that cannot run is not a check that passed.

Thresholds are constants at the top with the reasoning attached. They are judgement calls on a
workload the demo generator defines, and they are meant to be argued with; what they must not be is
buried in the code that applies them.
"""

import argparse
import csv
import json
import os
import re
import sys

# --- Thresholds -------------------------------------------------------------------------------
#
# Every one of these is a judgement about the demo generator's workload rather than a property of
# the service. They are deliberately asymmetric: a WARN is "look at this", a FAIL is "do not ship
# on the strength of this run".

# Samples inside this window are dropped before any trend is computed. A Go process's goroutine
# count and heap climb steeply while pools fill, workers start and the first sleep cycle runs;
# including that in a growth comparison would report a leak on every run. It is also where the
# store climbs from empty, which is what makes an index count look like a leak against it.
#
# Overridable with --warmup-minutes, which exists for validating the harness itself on a short run.
# A real soak should leave it alone.
WARMUP_SECONDS = 15 * 60

# Goroutines. A leak shows as sustained growth in the count, and the absolute floor matters as much
# as the ratio - a service settling from 40 to 60 goroutines is noise, the same 50% on a base of
# 4,000 is not.
GOROUTINE_WARN_RATIO = 0.20
GOROUTINE_WARN_ABSOLUTE = 25
GOROUTINE_FAIL_RATIO = 0.50
GOROUTINE_FAIL_ABSOLUTE = 100

# Memory. More tolerant than goroutines, because a Go process legitimately grows to its working set
# and the allocator returns pages to the OS lazily. What is NOT legitimate is growth that has not
# levelled off by the end of the run, which is why the slope over the final quarter is what decides
# rather than the total.
MEMORY_WARN_RATIO = 0.50
MEMORY_FAIL_RATIO = 1.50

# The sleep cycle. The specific worry item 20 was re-scoped around: the cycle has gone from one scan
# to roughly six (links, the forgotten log, retention stats, the reconcile sweep, the outbox drain),
# and the question is whether that shows as degradation over hours rather than in a unit test.
SLEEP_WARN_RATIO = 0.50
SLEEP_FAIL_RATIO = 2.00

# Eviction. used_bytes above capacity_bytes at the end of a run means eviction never caught up.
# A little over is expected between cycles - the target is enforced at the cycle, not continuously.
EVICTION_WARN_RATIO = 1.05
EVICTION_FAIL_RATIO = 1.25

# Index divergence: item 84's number. 1.0 is perfect agreement between index and store. The finding
# that opened the item was 20.7x, so a run that ends anywhere near 2x has reproduced the defect the
# outbox and the stale sweep were built to close.
INDEX_WARN_RATIO = 1.20
INDEX_FAIL_RATIO = 2.00

# The outbox. Depth is a queue, so a non-zero reading is normal; what is not normal is a depth that
# is still climbing at the end, which means deletions are being recorded faster than they drain.
OUTBOX_WARN_DEPTH = 5000
OUTBOX_FAIL_DEPTH = 50000

# The RED error rate, matching the shipped HippocampusHighErrorRate alert's own threshold so that a
# soak and a deployment disagree about nothing.
ERROR_RATE_FAIL = 0.01

# How much post-warm-up run a TREND verdict needs before it is allowed to say FAIL.
#
# This exists because the harness was caught crying wolf on its own validation run: over seven
# minutes the store fills from empty, mapped memory climbs 213%, and the growth check called it a
# leak. It was a working set. Over a window this short the two are genuinely indistinguishable -
# every process that is doing anything grows - so the honest verdict is "not enough run to say",
# and a check that says FAIL anyway teaches its reader to discount it. Below the span a verdict is
# capped at WARN; below the sample count it is UNKNOWN.
MIN_TREND_SECONDS = 60 * 60
MIN_TREND_SAMPLES = 8

# Sleep cycles are counted rather than timed: at the demo's two-minute period a short run produces
# a handful of cycles, and a p95 over three of them is one slow cycle away from any answer at all.
MIN_SLEEP_CYCLES = 10

PASS, WARN, FAIL, UNKNOWN = "PASS", "WARN", "FAIL", "UNKNOWN"

MARKERS = {PASS: "✅", WARN: "⚠️", FAIL: "❌", UNKNOWN: "❔"}

# Verdict severity, worst first, for ordering the summary and picking the exit code.
SEVERITY = [FAIL, WARN, UNKNOWN, PASS]


class Check:
    """One verdict: what was measured, what it came to, and how it reads."""

    def __init__(self, name, verdict, detail):
        self.name = name
        self.verdict = verdict
        self.detail = detail


def read_samples(path):
    with open(path, newline="", encoding="utf-8") as handle:
        return [row for row in csv.DictReader(handle)]


def numbers(rows, column, skip_warmup=True):
    """The (elapsed_seconds, value) pairs for a column, dropping blanks and the warm-up.

    Blank is unknown, never zero - see sample.py. Dropping rather than interpolating is right here:
    every consumer below is a comparison between two windows, and a fabricated point would be
    indistinguishable from a measured one in exactly the comparison that decides a verdict.
    """
    points = []

    for row in rows:
        raw = (row.get(column) or "").strip()
        if not raw:
            continue

        try:
            elapsed = float(row["elapsed_seconds"])
            value = float(raw)
        except (KeyError, ValueError):
            continue

        if skip_warmup and elapsed < WARMUP_SECONDS:
            continue

        points.append((elapsed, value))

    return points


def median(values):
    if not values:
        return None

    ordered = sorted(values)
    middle = len(ordered) // 2

    if len(ordered) % 2:
        return ordered[middle]

    return (ordered[middle - 1] + ordered[middle]) / 2.0


def window_medians(points, fraction=0.25):
    """Medians of the first and last `fraction` of a series, by sample count.

    Medians rather than endpoints because a single sample landing mid-sleep-cycle is not a trend,
    and the endpoints are the two samples most likely to do exactly that.
    """
    if len(points) < 4:
        return None, None

    span = max(1, int(len(points) * fraction))
    values = [value for _, value in points]

    return median(values[:span]), median(values[-span:])


def slope_per_hour(points):
    """Least-squares slope in units per hour, or None when there is nothing to fit."""
    if len(points) < 3:
        return None

    n = float(len(points))
    mean_x = sum(x for x, _ in points) / n
    mean_y = sum(y for _, y in points) / n

    variance = sum((x - mean_x) ** 2 for x, _ in points)
    if variance == 0:
        return None

    covariance = sum((x - mean_x) * (y - mean_y) for x, y in points)

    return (covariance / variance) * 3600.0


def last_value(rows, column):
    for row in reversed(rows):
        raw = (row.get(column) or "").strip()
        if raw:
            try:
                return float(raw)
            except ValueError:
                return None

    return None


def first_value(rows, column):
    for row in rows:
        raw = (row.get(column) or "").strip()
        if raw:
            try:
                return float(raw)
            except ValueError:
                return None

    return None


def counter_delta(rows, column):
    """A counter's increase across the run.

    Read as last minus first rather than as the last value, because the run may be sampling a
    process that was already up - and because a counter reset (a service restart mid-run) shows
    here as a negative, which is worth surfacing rather than hiding.
    """
    start, end = first_value(rows, column), last_value(rows, column)
    if start is None or end is None:
        return None

    return end - start


def human_bytes(value):
    if value is None:
        return "-"

    for unit in ["B", "KiB", "MiB", "GiB", "TiB"]:
        if abs(value) < 1024.0:
            return "%.1f %s" % (value, unit)

        value /= 1024.0

    return "%.1f PiB" % value


def human_number(value):
    if value is None:
        return "-"

    if value == int(value):
        return "{:,}".format(int(value))

    return "%.2f" % value


# --- Checks -----------------------------------------------------------------------------------


def growth_check(name, points, warn_ratio, fail_ratio, warn_absolute=0, fail_absolute=0, fmt=human_number):
    """The shared shape of the three trend checks: goroutines, RSS, and mapped memory.

    Two conditions have to agree before anything above PASS is reported - a ratio and, where one is
    given, an absolute floor. That pairing is what stops a service settling from 40 goroutines to 60
    being reported as a 50% leak.
    """
    if len(points) < MIN_TREND_SAMPLES:
        return Check(name, UNKNOWN, "only %d samples outside the warm-up window; %d are needed to call a trend"
                     % (len(points), MIN_TREND_SAMPLES))

    early, late = window_medians(points)
    slope = slope_per_hour(points)

    if not early:
        return Check(name, UNKNOWN, "no non-zero baseline to compare against")

    growth = late - early
    ratio = growth / early
    span = points[-1][0] - points[0][0]

    detail = "%s -> %s (%+.1f%%, %s/hour by least squares)" % (
        fmt(early),
        fmt(late),
        ratio * 100.0,
        fmt(slope) if slope is not None else "?",
    )

    over_fail = ratio >= fail_ratio and growth >= fail_absolute
    over_warn = ratio >= warn_ratio and growth >= warn_absolute

    # A short window cannot tell a leak from a process reaching its working set, so it is not
    # allowed to claim one. Saying so is the point: the reader learns the run was too short rather
    # than that the service was fine.
    if over_fail and span < MIN_TREND_SECONDS:
        return Check(name, WARN, detail + " - growing, but over only %d minutes; too short to tell a leak "
                     "from a working set filling. Re-run for at least an hour past the warm-up."
                     % int(span / 60))

    if over_fail:
        return Check(name, FAIL, detail + " - sustained growth; this is what a leak looks like")

    if over_warn:
        return Check(name, WARN, detail + " - growing; check it levels off over a longer run")

    return Check(name, PASS, detail)


def sleep_check(rows):
    points = numbers(rows, "sleep_p95_seconds")

    failed = counter_delta(rows, "sleeps_failed")
    if failed:
        return Check(
            "Sleep cycle",
            FAIL,
            "%s sleep cycles failed during the run" % human_number(failed),
        )

    if len(points) < MIN_TREND_SAMPLES:
        return Check("Sleep cycle", UNKNOWN, "only %d p95 samples outside the warm-up window; %d are needed"
                     % (len(points), MIN_TREND_SAMPLES))

    early, late = window_medians(points)
    if not early:
        return Check("Sleep cycle", UNKNOWN, "no non-zero p95 baseline to compare against")

    ratio = (late - early) / early
    cycles = counter_delta(rows, "sleeps_ok")
    detail = "p95 %.3fs -> %.3fs (%+.1f%%) over %s successful cycles" % (
        early,
        late,
        ratio * 100.0,
        human_number(cycles),
    )

    # A p95 over a handful of cycles is one slow cycle away from any answer, and the demo's
    # two-minute period means a short run produces exactly a handful.
    if cycles is not None and cycles < MIN_SLEEP_CYCLES:
        return Check("Sleep cycle", UNKNOWN, detail + " - too few cycles to judge a trend (%d needed)"
                     % MIN_SLEEP_CYCLES)

    if ratio >= SLEEP_FAIL_RATIO:
        return Check("Sleep cycle", FAIL, detail + " - the cycle is degrading as the run proceeds")

    if ratio >= SLEEP_WARN_RATIO:
        return Check("Sleep cycle", WARN, detail + " - slower than it started; worth a longer run")

    return Check("Sleep cycle", PASS, detail)


def eviction_check(rows):
    used = last_value(rows, "used_bytes")
    capacity = last_value(rows, "capacity_bytes")

    if used is None or not capacity:
        return Check("Eviction convergence", UNKNOWN, "used_bytes or capacity_bytes never reported")

    ratio = used / capacity
    points = numbers(rows, "used_bytes")
    half = points[len(points) // 2 :]
    band = ""

    if half:
        low = min(value for _, value in half)
        high = max(value for _, value in half)
        band = ", second half ranged %s-%s" % (human_bytes(low), human_bytes(high))

    detail = "%s of %s (%.0f%% of target)%s" % (
        human_bytes(used),
        human_bytes(capacity),
        ratio * 100.0,
        band,
    )

    # A rising trend in the second half while over the target is the failure this check exists for:
    # eviction is running and losing ground, which a single end-of-run reading cannot distinguish
    # from a store that is simply full.
    slope = slope_per_hour(half)
    if ratio > EVICTION_FAIL_RATIO or (ratio > 1.0 and slope is not None and slope > 0):
        return Check(
            "Eviction convergence",
            FAIL,
            detail + " - above the target and not coming down; eviction is not converging",
        )

    if ratio > EVICTION_WARN_RATIO:
        return Check("Eviction convergence", WARN, detail + " - above the target at the end of the run")

    return Check("Eviction convergence", PASS, detail)


def index_divergence_check(rows):
    """Item 84's measurement: index documents against store rows.

    This is the single most important check in a sqlite-opensearch run. The forward direction has
    always self-healed; what the outbox and the stale sweep were built for is the reverse, and the
    only way to see whether they work is to run the write rate that broke it and then count both
    sides.
    """
    docs = last_value(rows, "index_docs")
    memories = last_value(rows, "memories")

    if docs is None:
        return Check("Index divergence (item 84)", UNKNOWN, "no OpenSearch index was sampled")

    if not memories:
        return Check("Index divergence (item 84)", UNKNOWN, "the store's memory count never reported")

    ratio = docs / memories
    detail = "%s documents against %s rows (%.2fx)" % (
        human_number(docs),
        human_number(memories),
        ratio,
    )

    # The two sides are not read at the same instant and cannot be: the index count is live, while
    # the store count is a gauge served from the stats cache (item 25.9 - counting is a full scan,
    # so it is deliberately not done per export). soak.sh sets stats.intervalSeconds to keep that
    # cache fresh, and the warm-up window drops the initial fill, which is where the skew is large
    # enough to matter - a store climbing from nothing makes a fresh index look like a leak.

    # The trend matters as much as the endpoint. A ratio of 1.3 that is falling is the sweep doing
    # its job; the same figure rising is the leak, just earlier in its life.
    ratios = []

    for row in rows:
        try:
            document_count = float(row.get("index_docs") or "")
            row_count = float(row.get("memories") or "")
        except ValueError:
            continue

        if row_count and float(row["elapsed_seconds"]) >= WARMUP_SECONDS:
            ratios.append((float(row["elapsed_seconds"]), document_count / row_count))

    slope = slope_per_hour(ratios)
    if slope is not None:
        detail += ", trending %+.3fx/hour" % slope

    if ratio >= INDEX_FAIL_RATIO:
        return Check("Index divergence (item 84)", FAIL, detail + " - the index is leaking stale documents")

    if ratio >= INDEX_WARN_RATIO or (slope is not None and slope > 0.05):
        return Check("Index divergence (item 84)", WARN, detail + " - diverging; watch over a longer run")

    return Check("Index divergence (item 84)", PASS, detail)


def outbox_check(rows):
    points = numbers(rows, "outbox_depth")
    if not points:
        return Check("Outbox drain", UNKNOWN, "outbox_depth never reported (no OpenSearch backend?)")

    final = points[-1][1]
    peak = max(value for _, value in points)
    applied = counter_delta(rows, "outbox_applied")
    abandoned = counter_delta(rows, "outbox_abandoned")

    detail = "depth ended at %s (peak %s), %s deletions applied" % (
        human_number(final),
        human_number(peak),
        human_number(applied),
    )

    if abandoned:
        detail += ", %s abandoned by the caps" % human_number(abandoned)

    slope = slope_per_hour(points[len(points) // 2 :])

    if final >= OUTBOX_FAIL_DEPTH or (final > OUTBOX_WARN_DEPTH and slope is not None and slope > 0):
        return Check("Outbox drain", FAIL, detail + " - the queue is growing faster than it drains")

    if final >= OUTBOX_WARN_DEPTH or abandoned:
        return Check("Outbox drain", WARN, detail + " - backing up; the sweep is carrying the remainder")

    return Check("Outbox drain", PASS, detail)


def fault_check(rows):
    panics = counter_delta(rows, "panics")
    errors = counter_delta(rows, "rpc_server_errors")
    requests = counter_delta(rows, "rpc_requests")

    if panics:
        return Check("Faults", FAIL, "%s panics were recovered during the run" % human_number(panics))

    if requests is None or errors is None:
        return Check("Faults", UNKNOWN, "the RED counters never reported")

    rate = (errors / requests) if requests else 0.0
    detail = "%s server errors in %s requests (%.3f%%)" % (
        human_number(errors),
        human_number(requests),
        rate * 100.0,
    )

    if rate >= ERROR_RATE_FAIL:
        return Check("Faults", FAIL, detail + " - above the shipped alert's own 1% threshold")

    if errors:
        return Check("Faults", WARN, detail)

    return Check("Faults", PASS, detail)


def dropped_check(rows):
    dropped = counter_delta(rows, "search_dropped")
    removed = counter_delta(rows, "stale_removed")

    if dropped is None:
        return Check("Index queue drops", UNKNOWN, "the drop counter never reported")

    detail = "%s index operations dropped, %s stale documents removed by the sweep" % (
        human_number(dropped),
        human_number(removed),
    )

    # Drops are not a failure by themselves - the queue is explicitly best-effort and overflow is
    # the condition item 84 describes. What matters is whether the recovery paths kept up, which
    # the divergence check answers.
    if dropped:
        return Check("Index queue drops", WARN, detail + " - expected under sustained load; see the divergence check")

    return Check("Index queue drops", PASS, detail)


def log_check(path):
    """Errors and warnings in the captured output of the service and the generator.

    Kept because the last soak's whole result was "a clean log", and a report that stops sampling
    metrics without also reading the log would be a narrower claim than the one it replaces.
    """
    if not path or not os.path.exists(path):
        return Check("Log", UNKNOWN, "no log file was captured"), []

    error_pattern = re.compile(r'level=(error|fatal|panic)|"level":"(error|fatal|panic)"')
    warn_pattern = re.compile(r'level=warning|"level":"warning"')
    message_pattern = re.compile(r'msg="([^"]*)"|"msg":"((?:[^"\\]|\\.)*)"')

    errors, warnings = 0, 0
    counts = {}

    with open(path, encoding="utf-8", errors="replace") as handle:
        for line in handle:
            is_error = bool(error_pattern.search(line))
            if not is_error and not warn_pattern.search(line):
                continue

            if is_error:
                errors += 1
            else:
                warnings += 1

            found = message_pattern.search(line)
            message = (found.group(1) or found.group(2)) if found else line.strip()
            key = ("error" if is_error else "warning", message[:160])
            counts[key] = counts.get(key, 0) + 1

    top = sorted(counts.items(), key=lambda item: item[1], reverse=True)[:12]
    detail = "%d error lines, %d warning lines" % (errors, warnings)

    if errors:
        return Check("Log", FAIL, detail), top

    if warnings:
        return Check("Log", WARN, detail), top

    return Check("Log", PASS, detail), top


# --- Rendering --------------------------------------------------------------------------------

# The hourly table's columns: the CSV name, the heading, and how to render a value.
TABLE_COLUMNS = [
    ("goroutines", "goroutines", human_number),
    ("rss_bytes", "RSS", human_bytes),
    ("go_memory_bytes", "go mem", human_bytes),
    ("memories", "memories", human_number),
    ("used_bytes", "used", human_bytes),
    ("capacity_pressure", "pressure", lambda v: "-" if v is None else "%.2f" % v),
    ("sleep_p95_seconds", "sleep p95", lambda v: "-" if v is None else "%.2fs" % v),
    ("index_docs", "index docs", human_number),
    ("outbox_depth", "outbox", human_number),
    ("disk_free_bytes", "disk free", human_bytes),
]


def hourly_rows(rows):
    """The first sample, one per elapsed hour, and the last - which is what item 20 asked for.

    The nearest sample at or after each hour boundary, rather than an average over the hour: the
    question these rows answer is "where had it got to by then", and an average would smooth away
    the step changes that make a trend legible.
    """
    if not rows:
        return []

    selected = [rows[0]]
    next_boundary = 3600.0

    for row in rows:
        try:
            elapsed = float(row["elapsed_seconds"])
        except (KeyError, ValueError):
            continue

        while elapsed >= next_boundary:
            if selected[-1] is not row:
                selected.append(row)

            next_boundary += 3600.0

    if rows[-1] is not selected[-1]:
        selected.append(rows[-1])

    return selected


def render_table(rows):
    lines = []
    headings = ["elapsed"] + [heading for _, heading, _ in TABLE_COLUMNS]
    lines.append("| " + " | ".join(headings) + " |")
    lines.append("| " + " | ".join(["---"] * len(headings)) + " |")

    for row in hourly_rows(rows):
        try:
            elapsed = float(row["elapsed_seconds"])
        except (KeyError, ValueError):
            elapsed = 0.0

        cells = ["%dh%02dm" % (int(elapsed // 3600), int(elapsed % 3600) // 60)]

        for column, _, formatter in TABLE_COLUMNS:
            raw = (row.get(column) or "").strip()

            try:
                cells.append(formatter(float(raw)) if raw else "-")
            except ValueError:
                cells.append("-")

        lines.append("| " + " | ".join(cells) + " |")

    return "\n".join(lines)


def main():
    # Declared before the parser, whose default reads it: Python forbids a global declaration that
    # follows a use of the same name in the function.
    global WARMUP_SECONDS

    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--csv", required=True)
    parser.add_argument("--log", default="")
    parser.add_argument("--meta", default="")
    parser.add_argument("--out", default="")
    parser.add_argument(
        "--warmup-minutes",
        type=float,
        default=WARMUP_SECONDS / 60.0,
        help="samples before this are excluded from every trend (default 15)",
    )
    args = parser.parse_args()

    WARMUP_SECONDS = args.warmup_minutes * 60.0

    rows = read_samples(args.csv)
    if not rows:
        print("no samples were recorded", file=sys.stderr)

        return 2

    meta = {}
    if args.meta and os.path.exists(args.meta):
        with open(args.meta, encoding="utf-8") as handle:
            meta = json.load(handle)

    log_verdict, log_messages = log_check(args.log)

    checks = [
        growth_check(
            "Goroutines",
            numbers(rows, "goroutines"),
            GOROUTINE_WARN_RATIO,
            GOROUTINE_FAIL_RATIO,
            GOROUTINE_WARN_ABSOLUTE,
            GOROUTINE_FAIL_ABSOLUTE,
        ),
        growth_check(
            "Resident memory",
            numbers(rows, "rss_bytes"),
            MEMORY_WARN_RATIO,
            MEMORY_FAIL_RATIO,
            fmt=human_bytes,
        ),
        growth_check(
            "Go mapped memory",
            numbers(rows, "go_memory_bytes"),
            MEMORY_WARN_RATIO,
            MEMORY_FAIL_RATIO,
            fmt=human_bytes,
        ),
        sleep_check(rows),
        eviction_check(rows),
        index_divergence_check(rows),
        outbox_check(rows),
        dropped_check(rows),
        fault_check(rows),
        log_verdict,
    ]

    worst = min(checks, key=lambda check: SEVERITY.index(check.verdict)).verdict

    out = []
    out.append("# Soak report - %s" % meta.get("profile", "unknown profile"))
    out.append("")
    out.append("**Overall: %s %s**" % (MARKERS[worst], worst))
    out.append("")

    for key, label in [
        ("profile", "Profile"),
        ("driver", "Driver"),
        ("search", "Search backend"),
        ("version", "Build"),
        ("started", "Started"),
        ("finished", "Finished"),
        ("duration", "Duration"),
        ("config", "Config"),
        ("generator_args", "Generator args"),
        ("exit_reason", "Ended by"),
    ]:
        if meta.get(key):
            out.append("- **%s:** %s" % (label, meta[key]))

    out.append("- **Samples:** %d, every %s" % (len(rows), meta.get("interval", "?")))
    out.append("")
    out.append("## Verdicts")
    out.append("")
    out.append("| | check | finding |")
    out.append("| --- | --- | --- |")

    for check in checks:
        out.append("| %s | %s | %s |" % (MARKERS[check.verdict], check.name, check.detail))

    out.append("")
    out.append("## Hourly")
    out.append("")
    out.append(render_table(rows))
    out.append("")

    if log_messages:
        out.append("## Log lines")
        out.append("")
        out.append("| count | level | message |")
        out.append("| --- | --- | --- |")

        for (level, message), count in log_messages:
            out.append("| %d | %s | %s |" % (count, level, message.replace("|", "\\|")))

        out.append("")

    rendered = "\n".join(out)

    if args.out:
        with open(args.out, "w", encoding="utf-8") as handle:
            handle.write(rendered + "\n")

    print(rendered)

    # A FAIL is the only exit code that should stop a caller in its tracks. A WARN is information
    # for a person, and an UNKNOWN usually means a profile that does not have that subsystem at all
    # (no OpenSearch, so no outbox) - neither is a reason to fail a script.
    return 1 if worst == FAIL else 0


if __name__ == "__main__":
    sys.exit(main())
