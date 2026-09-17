#!/usr/bin/env python3
"""Benchmark measurements must survive the runner's normalized log export."""

import importlib.util
import json
from pathlib import Path
import sys
import unittest

sys.dont_write_bytecode = True
spec = importlib.util.spec_from_file_location(
    "throughput_ledger", Path(__file__).with_name("throughput-fix-2-ledger.py")
)
ledger = importlib.util.module_from_spec(spec)
spec.loader.exec_module(ledger)


class BenchmarkLedgerTests(unittest.TestCase):
    """Exercise real output shapes without executing a performance workload."""

    def test_receiver_credit_counters_are_retained(self):
        """A timed head keeps its allocation, byte-credit and RTT sample metrics."""
        row = ledger.benchmark_reading(
            "BenchmarkWindowPacingReceiverHeadBatchCoalescing-10\t147158\t"
            "1781 ns/op\t32.00 messages/op\t128.0 rtt-samples/op\t"
            "0 B/op\t0 allocs/op"
        )
        self.assertEqual(row, {
            "Kind": "benchmark",
            "Benchmark": "BenchmarkWindowPacingReceiverHeadBatchCoalescing",
            "Iterations": 147158,
            "GoMaxProcs": 10,
            "Metrics": {
                "ns/op": 1781, "messages/op": 32, "rtt-samples/op": 128,
                "B/op": 0, "allocs/op": 0,
            },
        })

    def test_repetitions_and_custom_metrics_are_not_collapsed(self):
        """Every reading survives, including slow rows and extra units."""
        lines = [
            "BenchmarkSample/nested-4 100 1.25e3 ns/op 8 B/op 1 allocs/op",
            "BenchmarkSample/nested-4 80 2.5e3 ns/op 8 B/op 1 allocs/op",
            "BenchmarkSingle 20 3.0 ns/op 12.5 MB/s",
        ]
        rows = [ledger.benchmark_reading(line) for line in lines]
        self.assertEqual(len(rows), 3)
        self.assertEqual([row["Metrics"]["ns/op"] for row in rows], [1250, 2500, 3])
        self.assertEqual(rows[0]["Benchmark"], "BenchmarkSample/nested")
        self.assertEqual(rows[2]["Metrics"]["MB/s"], 12.5)
        self.assertNotIn("GoMaxProcs", rows[2])

    def test_headers_and_failure_diagnostics_are_not_measurements(self):
        """Only completed numerical rows qualify; failure outcomes stay separate."""
        for line in [
            "BenchmarkWindowPacingReceiverHeadCoalescing-10",
            "--- FAIL: BenchmarkWindowPacingReceiverHeadCoalescing",
            "    fixture_test.go:10: lost timing or delivery",
            "PASS",
        ]:
            self.assertIsNone(ledger.benchmark_reading(line))

    def test_truncated_or_duplicate_metrics_fail_visibly(self):
        """Malformed measurements cannot silently become incomplete ledgers."""
        with self.assertRaises(ValueError):
            ledger.benchmark_reading("BenchmarkSample-2 10 32 ns/op 128")
        with self.assertRaises(ValueError):
            ledger.benchmark_reading("BenchmarkSample-2 10 32 ns/op 64 ns/op")


class PhysicalLedgerTests(unittest.TestCase):
    """Failed physical brackets are evidence and cannot disappear on export."""

    def test_duplex_brackets_and_censors_survive(self):
        """Retain both reference arms, the candidate and calibration failures."""
        readings = [
            {"Arm": "ceiling-before", "DirectionMbps": [120, 80]},
            {"Arm": "delivery", "DirectionMbps": [200, 30]},
            {"Arm": "ceiling-after", "DirectionMbps": [90, 70]},
        ]
        comparison = {
            "Calibrated": False, "Ratio": [1.9, .4],
            "FailureReasons": ["direction1 candidate below90% matched reference"],
            "CensoredReasons": ["direction0 reference below90% nominal1Gb/s"],
        }
        lines = [
            "    fixture_test.go:1: physical-h1-duplex-reading " + json.dumps(row)
            for row in readings
        ]
        lines.append("    fixture_test.go:2: physical-h1-duplex-comparison " + json.dumps(comparison))
        rows = [ledger.physical_reading(line) for line in lines]
        self.assertEqual(rows[:3], [
            {**row, "Kind": "physical-h1-duplex", "Instrument": "physical-h1-duplex"}
            for row in readings
        ])
        self.assertEqual(rows[3], {
            **comparison, "Kind": "comparison", "Instrument": "physical-h1-duplex",
        })

    def test_smoke_schema_remains_compatible(self):
        """Existing physical rows keep their names and every numerical field."""
        row = ledger.physical_reading('    fixture:1: physical-h1-reading {"Mbps":91.5}')
        self.assertEqual(row, {"Mbps": 91.5, "Kind": "physical-h1", "Instrument": "physical-h1"})
        row = ledger.physical_reading('    fixture:2: physical-h1-comparison {"Ratio":1.0}')
        self.assertEqual(row, {"Ratio": 1.0, "Kind": "comparison", "Instrument": "physical-h1"})
        self.assertIsNone(ledger.physical_reading("--- FAIL: TestWindowPhysicalH1SdkDuplex"))


if __name__ == "__main__":
    unittest.main()
