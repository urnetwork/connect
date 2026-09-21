#!/usr/bin/env python3
"""Archive one completed runner result, preserving its original provenance.

Usage: python3 tools/collect-throughput-fix-2.py RUN_DIR ARCHIVE_DIR
Only test outcomes, numerical ledgers and manifests enter the archive. Raw
logs may contain environment or unrelated network diagnostics; retain their
hashes here and keep the originals in RUN_DIR. Never copy a test binary.
"""

import argparse
import hashlib
import json
from pathlib import Path
import re


def collect(source, destination):
    """Validate the complete input before writing any archive files."""
    source = source.resolve()
    destination = destination.resolve()
    if source == destination:
        raise ValueError("the archive must differ from the original run directory")
    names = ("manifest.json", "status.json", "ledger.jsonl", "run.log")
    contents = {name: (source / name).read_bytes() for name in names}
    manifest = json.loads(contents["manifest.json"])
    status = json.loads(contents["status.json"])
    rows = [json.loads(line) for line in contents["ledger.jsonl"].splitlines()]
    if len(rows) != status["rows"]:
        raise ValueError("ledger row count differs from the recorded status")
    if not manifest.get("sources_stable_during_build"):
        raise ValueError("the run has no stable build-source manifest")
    comparisons = [row for row in rows if row.get("Kind") == "comparison"]
    failures = sum(bool(row.get("FailureReasons")) for row in comparisons)
    censored = sum(bool(row.get("CensoredReasons")) for row in comparisons)
    enforced = bool(comparisons) and all("FailureReasons" in row for row in comparisons)
    if not comparisons:
        acceptance = "no host comparisons in this selection"
    elif not enforced:
        acceptance = "historical gate did not enforce throughput acceptance"
    elif failures:
        acceptance = "failed"
    elif censored:
        acceptance = "inconclusive"
    else:
        acceptance = "passed"
    lines = contents["run.log"].decode(errors="replace").splitlines()
    # These prefixes contain test names/results, never logged environment.
    outcomes = [line for line in lines if re.match(
        r"^(?:=== RUN\s|--- (?:PASS|FAIL|SKIP):|PASS$|FAIL(?:\s|$)|ok\s)", line)]
    provenance = {
        "input_sha256": {name: hashlib.sha256(data).hexdigest() for name, data in contents.items()},
        "outcome": "process passed" if status["exit_code"] == 0 else "process failed",
        "top_level_passed": sum(line.startswith("--- PASS:") for line in lines),
        "top_level_failed": sum(line.startswith("--- FAIL:") for line in lines),
        "top_level_skipped": sum(line.startswith("--- SKIP:") for line in lines),
        "host_performance_acceptance": acceptance,
        "comparison_count": len(comparisons),
        "failed_comparisons": failures if enforced else None,
        "censored_comparisons": censored,
        "note": "Raw run.log remains outside this archive; its hash is retained. Outcomes are an excerpt. Process success alone does not prove performance acceptance.",
    }
    destination.mkdir(parents=True, exist_ok=True)
    for name in names[:-1]:
        (destination / name).write_bytes(contents[name])
    (destination / "outcomes.txt").write_text("\n".join(outcomes) + "\n")
    (destination / "provenance.json").write_text(json.dumps(provenance, indent=2) + "\n")
    print(f"{destination.name}: {provenance['outcome']}; host acceptance: {acceptance}")


def main():
    """Collect exactly one explicitly selected completed run."""
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("run_dir", type=Path)
    parser.add_argument("archive_dir", type=Path)
    args = parser.parse_args()
    collect(args.run_dir, args.archive_dir)


if __name__ == "__main__":
    main()
