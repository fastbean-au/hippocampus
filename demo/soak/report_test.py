#!/usr/bin/env python3
"""Tests for the soak report's verdicts.

    python3 -m unittest discover -s demo/soak

Not wired into hooks/pre-commit, which gates Go. Run it when touching report.py - the checks encode
judgement about what a four-hour measurement means, and the failure mode is silent: a threshold that
no longer fires produces a confident PASS that reads exactly like a healthy run.

The point of most of these is the NEGATIVE case. Both fixes here relax a check that was crying wolf,
and a relaxation that also swallows the real defect is worse than the false positive it replaced -
so every "this should now pass" test is paired with one proving the genuine fault is still caught.
"""

import os
import sys
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import report  # noqa: E402


def rows_from(series, start=0, step=300):
    """Build sample rows from {column: [values]}, one row per step seconds."""
    length = max(len(values) for values in series.values())
    built = []

    for i in range(length):
        row = {"elapsed_seconds": str(start + i * step)}

        for column, values in series.items():
            row[column] = str(values[i]) if i < len(values) else ""

        built.append(row)

    return built


def points_from(values, start=0, step=300):
    return [(float(start + i * step), float(v)) for i, v in enumerate(values)]


class PlateauTests(unittest.TestCase):
    """has_plateaued, and the growth check that consumes it."""

    def test_a_series_that_rose_then_flattened_is_a_plateau(self):
        # The shape the 2026-08-30 run produced: a climb, then dead flat.
        values = [100, 120, 140, 160, 180, 200, 224, 224, 224, 224, 224, 224, 224, 224, 224, 224]
        plateaued, slope, level = report.has_plateaued(points_from(values))

        self.assertTrue(plateaued, "a series flat for its whole second half must read as levelled off")
        self.assertAlmostEqual(slope, 0.0, places=6)
        self.assertAlmostEqual(level, 224.0, places=6)

    def test_a_series_still_climbing_is_not_a_plateau(self):
        values = [100 + 20 * i for i in range(16)]

        plateaued, _, _ = report.has_plateaued(points_from(values))

        self.assertFalse(plateaued, "a straight-line climb must never read as levelled off")

    def test_growth_check_passes_a_plateau_that_exceeds_the_ratio(self):
        # +124% end to end, which the ratio alone would call a leak, but flat throughout the tail.
        values = [100, 130, 160, 190, 210, 224, 224, 224, 224, 224, 224, 224, 224, 224, 224, 224]

        check = report.growth_check(
            "Go mapped memory", points_from(values), report.MEMORY_WARN_RATIO, report.MEMORY_FAIL_RATIO
        )

        self.assertEqual(check.verdict, report.PASS)
        self.assertIn("LEVELLED OFF", check.detail)

    def test_growth_check_still_fails_a_genuine_leak(self):
        # The regression that matters: sustained growth over a long window, never flattening.
        values = [100 + 40 * i for i in range(24)]

        check = report.growth_check(
            "Go mapped memory", points_from(values), report.MEMORY_WARN_RATIO, report.MEMORY_FAIL_RATIO
        )

        self.assertEqual(check.verdict, report.FAIL, "a series climbing to the last sample is a leak")
        self.assertIn("sustained growth", check.detail)

    def test_growth_check_still_fails_a_leak_that_merely_slows(self):
        # A leak that decelerates is still a leak. The slope over the second half stays well above
        # PLATEAU_SLOPE_RATIO, so this must not be mistaken for a working set settling.
        values = [100]
        for i in range(23):
            values.append(values[-1] + max(8, 40 - i))

        check = report.growth_check(
            "Go mapped memory", points_from(values), report.MEMORY_WARN_RATIO, report.MEMORY_FAIL_RATIO
        )

        self.assertIn(check.verdict, (report.WARN, report.FAIL))
        self.assertNotIn("LEVELLED OFF", check.detail)

    def test_goroutine_absolute_floor_still_applies(self):
        # 40 -> 60 goroutines is +50%, over the fail ratio, but 20 goroutines is noise.
        values = [40, 44, 48, 52, 55, 57, 58, 59, 60, 60, 61, 60, 60, 61, 60, 60]

        check = report.growth_check(
            "Goroutines",
            points_from(values),
            report.GOROUTINE_WARN_RATIO,
            report.GOROUTINE_FAIL_RATIO,
            report.GOROUTINE_WARN_ABSOLUTE,
            report.GOROUTINE_FAIL_ABSOLUTE,
        )

        self.assertNotEqual(check.verdict, report.FAIL, "a 20-goroutine rise is below the absolute floor")


class SteadyStateTests(unittest.TestCase):
    """steady_state_start, and the sleep check that consumes it."""

    def test_locates_the_point_the_store_stopped_growing(self):
        memories = [2000, 6000, 11000, 15000, 17000, 17500, 17200, 17900, 17400, 17600, 17800, 17300]
        rows = rows_from({"memories": memories})

        settled = report.steady_state_start(rows)

        self.assertIsNotNone(settled)
        # The fill samples must be excluded and the settled ones kept.
        self.assertGreaterEqual(settled, 900)
        self.assertLessEqual(settled, 1500)

    def test_a_store_still_growing_never_settles(self):
        memories = [1000 * (i + 1) for i in range(14)]
        rows = rows_from({"memories": memories})

        self.assertIsNone(report.steady_state_start(rows), "a store climbing to the last sample has not settled")

    def test_sleep_check_ignores_the_fill_phase(self):
        # The cycle climbs while the store fills, then holds flat - the 2026-08-30 shape, which the
        # old comparison reported as +150.9%.
        memories = [2000, 6000, 11000, 15000] + [17500] * 14
        mean = [0.1, 0.25, 0.9, 1.75] + [2.2, 2.25, 2.15, 2.2, 2.3, 2.2, 2.16, 2.2, 2.24, 2.2, 2.18, 2.2, 2.22, 2.2]
        rows = rows_from({"memories": memories, "sleep_mean_seconds": mean, "sleeps_ok": list(range(1, 19))})

        check = report.sleep_check(rows)

        self.assertEqual(check.verdict, report.PASS)
        self.assertIn("once the store stopped growing", check.detail)

    def test_sleep_check_still_catches_degradation_at_constant_load(self):
        # The regression that matters: the store is flat throughout, and the cycle still doubles.
        memories = [17500] * 20
        mean = [1.0 + 0.12 * i for i in range(20)]
        rows = rows_from({"memories": memories, "sleep_mean_seconds": mean, "sleeps_ok": list(range(1, 21))})

        check = report.sleep_check(rows)

        self.assertIn(check.verdict, (report.WARN, report.FAIL))
        self.assertNotIn("LEVELLED OFF", check.detail)

    def test_sleep_check_prefers_the_mean_over_the_p95(self):
        # Both columns present and disagreeing, which is the real 2026-08-31 MySQL situation: the
        # p95 jumps between bucket edges while the mean barely moves. The mean must decide.
        memories = [10000] * 20
        mean = [0.150] * 10 + [0.158] * 10
        p95 = [0.24] * 10 + [0.44] * 10

        rows = rows_from({
            "memories": memories,
            "sleep_mean_seconds": mean,
            "sleep_p95_seconds": p95,
            "sleeps_ok": list(range(1, 21)),
        })

        check = report.sleep_check(rows)

        self.assertEqual(check.verdict, report.PASS, "the p95's bucket-edge jump must not decide the verdict")
        self.assertIn("mean ", check.detail)

    def test_sleep_check_falls_back_to_p95_but_says_so(self):
        # A run stored before the mean existed. It must still be readable, and must carry the
        # warning that its trend cannot be trusted rather than being judged silently.
        memories = [17500] * 20
        p95 = [0.24] * 10 + [0.44] * 10
        rows = rows_from({"memories": memories, "sleep_p95_seconds": p95, "sleeps_ok": list(range(1, 21))})

        check = report.sleep_check(rows)

        self.assertIn("FALLBACK", check.detail)
        self.assertIn("unusable", check.detail)

    def test_a_failed_cycle_outranks_any_trend(self):
        rows = rows_from({"sleeps_ok": [1, 2, 3], "sleeps_failed": [0, 1, 2]})

        check = report.sleep_check(rows)

        self.assertEqual(check.verdict, report.FAIL)
        self.assertIn("failed", check.detail)


class AttributionTests(unittest.TestCase):
    def test_a_counter_running_backwards_fails(self):
        rows = rows_from({"rpc_requests": [100, 200, 50], "sleeps_ok": [1, 2, 3]})

        check = report.attribution_check(rows)

        self.assertEqual(check.verdict, report.FAIL)
        self.assertIn("rpc_requests", check.detail)

    def test_monotonic_counters_pass(self):
        rows = rows_from({"rpc_requests": [100, 200, 300], "sleeps_ok": [1, 2, 3]})

        self.assertEqual(report.attribution_check(rows).verdict, report.PASS)


class EvictionTests(unittest.TestCase):
    def test_over_target_never_returning_and_still_rising_fails(self):
        # Long enough to clear MIN_TREND_SECONDS, never once back under the target, still climbing.
        used = [70e6 + 4e6 * i for i in range(30)]
        rows = rows_from({"used_bytes": used, "capacity_bytes": [70e6] * 30})

        self.assertEqual(report.eviction_check(rows).verdict, report.FAIL)

    def test_converged_below_target_passes(self):
        rows = rows_from(
            {
                "used_bytes": [69e6, 63e6, 68e6, 64e6, 69e6, 63e6, 67e6, 65e6],
                "capacity_bytes": [70e6] * 8,
            }
        )

        self.assertEqual(report.eviction_check(rows).verdict, report.PASS)

    def test_a_sawtooth_ending_above_the_target_still_passes(self):
        # The regression this was written for: the target is enforced once per sleep cycle, so the
        # store climbs above it between cycles and is cut back on each one. Ending mid-tooth is not
        # a failure, and a validation run with a tight cap was reported as one.
        used = [60e6, 75e6, 58e6, 76e6, 59e6, 74e6, 61e6, 77e6]
        rows = rows_from({"used_bytes": used, "capacity_bytes": [70e6] * 8})

        check = report.eviction_check(rows)

        self.assertEqual(check.verdict, report.PASS)
        self.assertIn("brings it back under", check.detail)

    def test_over_target_on_a_short_run_is_capped_at_warn(self):
        # Never back under, but only 35 minutes of it - indistinguishable from a store still filling.
        used = [70e6 + 2e6 * i for i in range(8)]
        rows = rows_from({"used_bytes": used, "capacity_bytes": [70e6] * 8})

        check = report.eviction_check(rows)

        self.assertEqual(check.verdict, report.WARN)
        self.assertIn("too short", check.detail)


if __name__ == "__main__":
    unittest.main()
