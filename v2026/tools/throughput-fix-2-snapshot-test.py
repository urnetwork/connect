#!/usr/bin/env python3
"""Deterministic tests for the runner's build/source-inspection boundary."""

import hashlib
import importlib.util
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest


sys.dont_write_bytecode = True
tools = Path(__file__).resolve().parent
spec = importlib.util.spec_from_file_location(
    "throughput_snapshot", tools / "throughput-fix-2-snapshot.py")
snapshot = importlib.util.module_from_spec(spec)
spec.loader.exec_module(snapshot)


class SnapshotTests(unittest.TestCase):
    """Use isolated synthetic repositories; never edit the working checkout."""

    def test_source_checkout_cannot_contain_its_snapshot(self):
        """Do not add duplicate Go packages to later source-walking audits."""
        with tempfile.TemporaryDirectory(prefix="throughput-nested-test-") as directory:
            root = Path(directory)
            source = root / "connect"
            source.mkdir()
            alias = root / "outside-alias"
            alias.symlink_to(source, target_is_directory=True)
            for destination in (source, source / "result/source/connect", alias / "result"):
                with self.assertRaisesRegex(ValueError, "outside the source checkout"):
                    snapshot.snapshot_repository(source, destination)
            self.assertEqual(list(source.iterdir()), [])

    def test_runner_rejects_outputs_in_any_worktree(self):
        """In-place, nested, aliased and sibling outputs must not add Go inputs."""
        with tempfile.TemporaryDirectory(prefix="throughput-output-test-") as directory:
            root = Path(directory)
            repo, sibling = root / "connect", root / "server"
            for path in (repo, sibling):
                path.mkdir()
                subprocess.run(["git", "init", "-q", str(path)], check=True)
            (repo / "tools").mkdir()
            runner = repo / "tools/throughput-fix-2.sh"
            shutil.copyfile(tools / runner.name, runner)
            (repo / "original.go").write_text("package snapshot\n")
            alias = root / "outside-alias"
            alias.symlink_to(repo, target_is_directory=True)
            inventory = ["git", "ls-files", "--others", "--exclude-standard", "--", "*.go"]
            before = subprocess.check_output(inventory, cwd=repo)
            for output in (repo, repo / "nested/result", alias / "result", sibling / "result"):
                result = subprocess.run(["bash", str(runner), "correctness", str(output)],
                                        text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
                self.assertEqual(result.returncode, 1, result.stdout)
                self.assertIn("output directory must be outside Git worktrees", result.stdout)
                self.assertFalse((output / "source").exists())
            self.assertEqual(subprocess.check_output(inventory, cwd=repo), before)

    def test_compiled_audits_keep_the_build_source(self):
        """Force an original-source edit before both relative and Caller reads."""
        with tempfile.TemporaryDirectory(prefix="throughput-source-test-") as directory:
            root = Path(directory)
            repo = root / "connect"
            repo.mkdir()
            (repo / "tools").mkdir()
            subprocess.run(["git", "init", "-q", str(repo)], check=True)
            subprocess.run(["git", "-C", str(repo), "-c", "user.name=Synthetic Test",
                            "-c", "user.email=test@example.invalid", "commit", "-q",
                            "--allow-empty", "-m", "synthetic source snapshot"], check=True)
            for name in ("throughput-fix-2.sh", "throughput-fix-2-snapshot.py", "throughput-fix-2-ledger.py"):
                shutil.copyfile(tools / name, repo / "tools" / name)
            (repo / "go.mod").write_text("module example.invalid/snapshot\n\ngo 1.26\n")
            original = repo / "source.txt"
            original.write_text("compiled source")
            (repo / "snapshot_test.go").write_text('''package snapshot

import (
    "os"
    "path/filepath"
    "runtime"
    "testing"
)

func TestTheWindowHasOneOwner(t *testing.T) {
    if err := os.WriteFile(os.Getenv("SNAPSHOT_TEST_ORIGINAL"), []byte("edited source"), 0600); err != nil {
        t.Fatal(err)
    }
    if err := os.WriteFile(os.Getenv("SNAPSHOT_TEST_ORIGINAL_LEDGER"), []byte("raise RuntimeError('live parser used')"), 0600); err != nil {
        t.Fatal(err)
    }
    _, file, _, ok := runtime.Caller(0)
    if !ok { t.Fatal("missing source location") }
    for _, path := range []string{"source.txt", filepath.Join(filepath.Dir(file), "source.txt")} {
        data, err := os.ReadFile(path)
        if err != nil || string(data) != "compiled source" {
            t.Errorf("source inspection drifted from compiled source: %s %q %v", path, data, err)
        }
    }
}
''')
            output = root / "result"
            result = subprocess.run(
                ["bash", str(repo / "tools/throughput-fix-2.sh"), "correctness", str(output)],
                env=dict(os.environ, SNAPSHOT_TEST_ORIGINAL=str(original),
                         SNAPSHOT_TEST_ORIGINAL_LEDGER=str(repo / "tools/throughput-fix-2-ledger.py")),
                text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
            log = (output / "run.log").read_text() if (output / "run.log").exists() else ""
            self.assertEqual(result.returncode, 0, result.stdout + log)
            self.assertEqual(original.read_text(), "edited source")
            manifest = json.loads((output / "manifest.json").read_text())
            self.assertTrue(manifest["source_inspection_uses_build_snapshot"])
            self.assertIn("live parser used", (repo / "tools/throughput-fix-2-ledger.py").read_text())
            frozen_parser = output / "source/connect/tools/throughput-fix-2-ledger.py"
            self.assertEqual(manifest["benchmark_ledger_parser_sha256"],
                             hashlib.sha256(frozen_parser.read_bytes()).hexdigest())
            self.assertEqual(json.loads((output / "status.json").read_text())["exit_code"], 0)

    def test_copied_inputs_are_independent_and_verified(self):
        """New tests and symlinked fixtures are copied; later edits cannot leak in."""
        with tempfile.TemporaryDirectory(prefix="throughput-copy-test-") as directory:
            root = Path(directory)
            source = root / "connect"
            source.mkdir()
            subprocess.run(["git", "init", "-q", str(source)], check=True)
            (source / "new_test.go").write_text("package snapshot\n")
            fixture = root / "fixture.txt"
            fixture.write_text("old fixture")
            (source / "fixture.txt").symlink_to(fixture)
            output = source / "result"
            output.mkdir()
            (output / "run.log").write_text("mutable output")
            destination = root / "snapshot"
            metadata = snapshot.snapshot_repository(source, destination, excluded=(output,))
            self.assertEqual(metadata["file_count"], 2)
            fixture.write_text("new fixture")
            (source / "new_test.go").write_text("edited source")
            snapshot.verify_snapshot(destination, metadata)
            self.assertEqual((destination / "fixture.txt").read_text(), "old fixture")
            added = destination / "unexpected.go"
            added.write_text("package snapshot\n")
            with self.assertRaisesRegex(ValueError, "snapshot input file set changed"):
                snapshot.verify_snapshot(destination, metadata)
            added.unlink()
            (destination / "new_test.go").write_text("corrupted snapshot")
            with self.assertRaisesRegex(ValueError, "snapshot input changed: new_test.go"):
                snapshot.verify_snapshot(destination, metadata)

    def test_local_replacements_come_from_the_copied_module(self):
        """An edit after copying cannot select another sibling dependency."""
        with tempfile.TemporaryDirectory(prefix="throughput-module-test-") as directory:
            root = Path(directory)
            source = root / "connect"
            source.mkdir()
            subprocess.run(["git", "init", "-q", str(source)], check=True)
            module = "module example.invalid/snapshot\n\ngo 1.26\n"
            module += "require example.invalid/dep v0.0.0\nreplace example.invalid/dep => ../dep\n"
            (source / "go.mod").write_text(module)
            (source / "snapshot.go").write_text('package snapshot\nimport "example.invalid/dep"\nvar Value = dep.Value\n')
            dep = root / "dep"
            dep.mkdir()
            (dep / "go.mod").write_text("module example.invalid/dep\n\ngo 1.26\n")
            (dep / "dep.go").write_text("package dep\nconst Value = 1\n")
            parent = root / "copies"
            destination = parent / "connect"
            metadata = snapshot.snapshot_repository(source, destination)
            (source / "go.mod").write_text(module.replace("../dep", "../changed"))
            external = snapshot.link_local_replacements(source, destination, parent)
            self.assertEqual(external, [str(dep.resolve())])
            self.assertFalse((parent / "changed").exists())
            snapshot.verify_snapshot(destination, metadata)
            result = subprocess.run(["go", "test", "./..."], cwd=destination,
                                    text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
            self.assertEqual(result.returncode, 0, result.stdout)


if __name__ == "__main__":
    unittest.main()
