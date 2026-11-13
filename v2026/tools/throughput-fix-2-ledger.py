#!/usr/bin/env python3
"""Retain benchmark and physical measurements in the runner's safe ledger."""

import json
import re


def physical_reading(line):
    """Keep smoke/duplex arms and comparisons, including failed calibration."""
    match = re.search(r"(physical-h1(?:-duplex)?)-(reading|comparison) (\{.*\})$", line)
    if match is None:
        return None
    row = json.loads(match[3])
    row["Kind"] = "comparison" if match[2] == "comparison" else match[1]
    row["Instrument"] = match[1]
    return row


def benchmark_reading(line):
    """Ignore headers; preserve every metric from a completed benchmark row."""
    match = re.fullmatch(r"(Benchmark\S+)\s+(\d+)\s+(.+)", line.strip())
    if match is None:
        return None
    fields = match[3].split()
    if len(fields) % 2:
        raise ValueError("incomplete benchmark metric pair")
    metrics = {}
    for index in range(0, len(fields), 2):
        unit = fields[index + 1]
        if unit in metrics:
            raise ValueError(f"duplicate benchmark metric: {unit}")
        metrics[unit] = float(fields[index])
    name = match[1]
    concurrency = re.search(r"-(\d+)$", name)
    row = {
        "Kind": "benchmark",
        "Benchmark": name[:concurrency.start()] if concurrency else name,
        "Iterations": int(match[2]),
        "Metrics": metrics,
    }
    if concurrency:
        row["GoMaxProcs"] = int(concurrency[1])
    return row
