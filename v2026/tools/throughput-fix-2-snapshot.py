#!/usr/bin/env python3
"""Freeze the source and relative fixtures read by the throughput test binary.

The binary must be compiled inside this tree: source audits use both the working
directory and runtime.Caller. Local replacement modules remain external unless
the caller also snapshots them. Raw snapshots stay in the local run directory.
"""

import hashlib
import json
import os
from pathlib import Path
import subprocess


def snapshot_repository(source, destination, excluded=()):
    """Copy tracked and nonignored inputs, rejecting a changed copy boundary."""
    source = Path(source).resolve()
    destination = Path(destination).resolve()
    if destination.is_relative_to(source):
        raise ValueError("snapshot output must be outside the source checkout")
    if destination.exists():
        raise ValueError(f"snapshot already exists: {destination}")
    excluded = tuple(Path(path).resolve() for path in (*excluded, destination))
    names = subprocess.check_output(
        ["git", "ls-files", "-z", "--cached", "--others", "--exclude-standard"],
        cwd=source,
    ).decode().split("\0")
    names = sorted({name for name in names if name and
                    not any((source / name).is_relative_to(path) for path in excluded)})
    contents = {}
    modes = {}
    for name in names:
        path = source / name
        if path.is_file():
            contents[name] = path.read_bytes()
            modes[name] = path.stat().st_mode & 0o777
        elif path.exists():
            raise ValueError(f"unsupported snapshot input: {path}")
    # A file is copied once, then checked against the original. Subsequent
    # edits are allowed: compilation and source audits use only the copy.
    for name, data in contents.items():
        if (source / name).read_bytes() != data:
            raise ValueError(f"input changed while snapshotting: {name}")
    destination.mkdir(parents=True)
    hashes = {}
    for name, data in contents.items():
        path = destination / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(data)
        path.chmod(modes[name])
        hashes[name] = hashlib.sha256(data).hexdigest()
    return {
        "file_count": len(hashes),
        "input_sha256": hashlib.sha256(
            json.dumps(hashes, sort_keys=True).encode()).hexdigest(),
        "file_sha256": hashes,
    }


def verify_snapshot(root, metadata):
    """Reject any changed frozen input before executing its compiled binary."""
    root = Path(root)
    actual = {path.relative_to(root).as_posix() for path in root.rglob("*") if path.is_file()}
    if actual != set(metadata["file_sha256"]):
        raise ValueError("snapshot input file set changed")
    for name, expected in metadata["file_sha256"].items():
        path = root / name
        if not path.is_file() or hashlib.sha256(path.read_bytes()).hexdigest() != expected:
            raise ValueError(f"snapshot input changed: {name}")


def link_local_replacements(source, destination, snapshot_parent):
    """Preserve sibling Go replacements without redirecting a snapshotted repo."""
    module = json.loads(subprocess.check_output(
        ["go", "mod", "edit", "-json"], cwd=destination))
    external = []
    for replacement in module.get("Replace") or []:
        target = replacement["New"]
        if target.get("Version"):
            continue
        path = Path(target["Path"])
        if path.is_absolute():
            external.append(str(path))
            continue
        original = (Path(source) / path).resolve()
        copied = Path(os.path.abspath(Path(destination) / path))
        if not copied.is_relative_to(snapshot_parent):
            raise ValueError(f"local replacement escapes snapshot parent: {path}")
        if not copied.exists():
            copied.parent.mkdir(parents=True, exist_ok=True)
            copied.symlink_to(original, target_is_directory=True)
        if copied.is_symlink():
            external.append(str(original))
    return sorted(set(external))
