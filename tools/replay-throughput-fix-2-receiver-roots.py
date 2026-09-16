#!/usr/bin/env python3
"""Replay experimental receiver failures without changing the working source.

The checked-in fixtures and patches apply to one pinned commit. Each invocation
copies that source and the local glog dependency, builds under race, and retains
all outcomes. Known failing roots return failure; they are never expected-pass
wrappers. This is a correctness replay, not a production wire/performance claim.
"""

import argparse
import datetime
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import re
import subprocess
import tarfile
import time


def sha(path):
    return hashlib.sha256(Path(path).read_bytes()).hexdigest()


def main():
    repo = Path(__file__).resolve().parent.parent
    bundle = repo / "testdata/throughput_root_cases"
    manifest = json.loads((bundle / "manifest.json").read_text())
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("output", type=Path)
    parser.add_argument("--variant", choices=manifest["variants"], default="hybrid")
    parser.add_argument("--count", type=int, default=3)
    parser.add_argument("--run", default=manifest["default_pattern"])
    args = parser.parse_args()
    output = args.output.resolve()
    if output.exists() or output.is_relative_to(repo) or args.count < 1:
        parser.error("use a new output directory outside the checkout and a positive count")
    for name, expected in manifest["file_sha256"].items():
        if sha(bundle / name) != expected:
            parser.error(f"replay input changed: {name}")

    output.mkdir(parents=True)
    source = output / "source/connect"
    source.mkdir(parents=True)
    archive = subprocess.check_output(
        ["git", "archive", "--format=tar", manifest["base_commit"]], cwd=repo
    )
    with tarfile.open(fileobj=io.BytesIO(archive)) as files:
        files.extractall(source, filter="data")
    subprocess.run(["git", "apply", str(bundle / manifest["common_patch"])], cwd=source, check=True)
    for destination, origin in manifest["fixtures"].items():
        (source / destination).write_bytes((bundle / origin).read_bytes())
    for patch in manifest["variants"][args.variant]:
        subprocess.run(["git", "apply", str(bundle / patch)], cwd=source, check=True)

    spec = importlib.util.spec_from_file_location(
        "throughput_snapshot", repo / "tools/throughput-fix-2-snapshot.py"
    )
    snapshot = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(snapshot)
    snapshot.snapshot_repository(repo.parent / "glog", output / "source/glog")

    def inputs():
        return {
            path.relative_to(output / "source").as_posix(): sha(path)
            for path in sorted((output / "source").rglob("*"))
            if path.is_file() and (path.suffix == ".go" or path.name in
                                   ("go.mod", "go.sum", "window_sdk_profiles.json"))
        }

    before = inputs()
    metadata = {
        "base_commit": manifest["base_commit"], "variant": args.variant,
        "bundle_sha256": sha(bundle / "manifest.json"), "pattern": args.run,
        "count": args.count, "race": True, "no_quiescence_wait": True,
        "started_utc": datetime.datetime.now(datetime.timezone.utc).isoformat(),
        "loadavg_start": os.getloadavg(),
        "go": subprocess.check_output(["go", "version"], text=True).strip(),
        "source_sha256": hashlib.sha256(json.dumps(before, sort_keys=True).encode()).hexdigest(),
    }
    (output / "source-pins.json").write_text(json.dumps(before, indent=2) + "\n")
    started = time.monotonic()
    binary = output / "tests"
    with (output / "build.log").open("w") as log:
        build = subprocess.run(["go", "test", "-race", "-c", "-o", str(binary)],
                               cwd=source, stdout=log, stderr=subprocess.STDOUT)
    metadata["compile_exit_code"] = build.returncode
    metadata["sources_stable_during_build"] = before == inputs()
    if build.returncode or not metadata["sources_stable_during_build"]:
        (output / "status.json").write_text(json.dumps(metadata, indent=2) + "\n")
        print(json.dumps(metadata, indent=2))
        return build.returncode or 1

    metadata["binary_sha256"] = sha(binary)
    with (output / "run.log").open("w") as log:
        run = subprocess.run([str(binary), "-test.v", "-test.run", args.run,
                              f"-test.count={args.count}", "-test.timeout=30m"],
                             cwd=source, stdout=log, stderr=subprocess.STDOUT)
    raw = (output / "run.log").read_text()
    metadata.update({
        "exit_code": run.returncode, "raw_log_sha256": sha(output / "run.log"),
        "sources_stable_after": before == inputs(),
        "elapsed_seconds": time.monotonic() - started,
        "finished_utc": datetime.datetime.now(datetime.timezone.utc).isoformat(),
        "loadavg_end": os.getloadavg(), "race_warnings": raw.count("WARNING: DATA RACE"),
        "outcomes": {state: len(re.findall(r"^--- " + state + ":", raw, re.M))
                     for state in ("PASS", "FAIL", "SKIP")},
    })
    (output / "status.json").write_text(json.dumps(metadata, indent=2) + "\n")
    print(json.dumps(metadata, indent=2))
    return run.returncode or int(not metadata["sources_stable_after"])


if __name__ == "__main__":
    raise SystemExit(main())
