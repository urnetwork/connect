#!/usr/bin/env python3
"""Force the configuration/service boundary with synthetic local fixtures."""

import importlib.util
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest


sys.dont_write_bytecode = True
tools = Path(__file__).resolve().parent
spec = importlib.util.spec_from_file_location("scoped_server_env", tools / "throughput-fix-2-server-env.py")
adapter = importlib.util.module_from_spec(spec)
spec.loader.exec_module(adapter)


class ServerEnvironmentTests(unittest.TestCase):
    """No test touches the sibling server checkout or a running service."""

    def fixture(self, directory):
        """The service preflight always fails, independently of configuration."""
        root = Path(directory).resolve()
        server = root / "server with 'quotes' and $literal"
        (server / "local").mkdir(parents=True)
        (server / "connect").mkdir()
        output = root / "result"
        output.mkdir()
        environment = '''test_env_configure() {
    local source_path="${BASH_SOURCE[0]}"
    export WARP_ENV="local"
    export WARP_SERVICE="test"
    export SYNTHETIC_SOURCE_PATH="$source_path"
}
test_env_preflight() {
    printf 'unavailable synthetic service\\n' >&2
    return 37
}
test_env_main() {
    test_env_configure || return $?
    test_env_preflight || return $?
}

test_env_main
test_env_status=$?
return "$test_env_status"
'''
        (server / "test-env.sh").write_text(environment)
        (server / "local/run-local-state.sh").write_text("# synthetic helper\n")
        (server / "connect/synthetic_test.go").write_text("package synthetic\nfunc TestOwnedFixture() {}\n")
        audit = {
            "environment_sha256": adapter.digest(server / "test-env.sh"),
            "launcher_helper_sha256": adapter.digest(server / "local/run-local-state.sh"),
            "modes": {"server-connect-deterministic": {
                "original_pattern": "^TestOwned.*$",
                "tests": {"TestOwnedFixture": "connect/synthetic_test.go"},
                "test_file_sha256": {"connect/synthetic_test.go": adapter.digest(server / "connect/synthetic_test.go")},
                "excluded_database_tests": ["TestDatabaseFixture"],
            }},
        }
        return server, output, audit

    def test_configured_exports_keep_the_original_source_without_service_preflight(self):
        """The same failing environment configures a pinned non-DB selection."""
        with tempfile.TemporaryDirectory(prefix="throughput-env-test-") as directory:
            server, output, audit = self.fixture(directory)
            original = subprocess.run(["bash", "-c", 'source "$1"', "fixture", str(server / "test-env.sh")],
                                      text=True, capture_output=True)
            self.assertEqual(original.returncode, 37)
            adapter.prepare(server, output, "server-connect-deterministic", "^TestOwned.*$", audit)
            configured = subprocess.run(
                ["bash", "-euc", 'source "$1"; printf "%s\\n%s\\n%s\\n" "$WARP_ENV" "$WARP_SERVICE" "$SYNTHETIC_SOURCE_PATH"',
                 "fixture", str(output / "server-test-configure.sh")],
                text=True, capture_output=True)
            self.assertEqual(configured.returncode, 0, configured.stderr)
            self.assertEqual(configured.stdout.splitlines(), ["local", "test", str(server / "test-env.sh")])
            self.assertEqual(configured.stderr, "")
            self.assertEqual((output / "server-test-pattern.txt").read_text(), "^(TestOwnedFixture)$\n")
            provenance = json.loads((output / "server-test-environment.json").read_text())
            self.assertFalse(provenance["service_preflight_ran"])
            self.assertTrue(provenance["race_instrumented"])
            self.assertEqual(provenance["environment_sha256"], adapter.digest(server / "test-env.sh"))
            self.assertEqual(provenance["excluded_database_tests"], ["TestDatabaseFixture"])

    def test_changed_environment_and_test_sources_require_another_review(self):
        """A source drift cannot quietly widen the non-database exception."""
        for name in ("test-env.sh", "local/run-local-state.sh", "connect/synthetic_test.go"):
            with tempfile.TemporaryDirectory(prefix="throughput-env-test-") as directory:
                server, output, audit = self.fixture(directory)
                with (server / name).open("a") as stream:
                    stream.write("\n# changed source\n")
                with self.assertRaisesRegex(ValueError, "changed"):
                    adapter.prepare(server, output, "server-connect-deterministic", "^TestOwned.*$", audit)
                self.assertEqual(list(output.iterdir()), [])

    def test_integration_modes_and_wider_patterns_cannot_use_the_adapter(self):
        """Only the exact reviewed caller can select configuration alone."""
        with tempfile.TemporaryDirectory(prefix="throughput-env-test-") as directory:
            server, output, audit = self.fixture(directory)
            for mode, pattern in (("server-integration", "^TestOwned.*$"),
                                  ("server", "^TestOwned.*$"),
                                  ("server-connect-deterministic", ".")):
                with self.assertRaises(ValueError):
                    adapter.prepare(server, output, mode, pattern, audit)
            self.assertEqual(list(output.iterdir()), [])

    def test_unreviewed_nonrace_mode_cannot_change_instrumentation(self):
        """A different mode cannot silently remove the reviewed race tier."""
        with tempfile.TemporaryDirectory(prefix="throughput-env-test-") as directory:
            server, output, audit = self.fixture(directory)
            with self.assertRaisesRegex(ValueError, "limited to audited"):
                adapter.prepare(server, output, "server-connect-deterministic-norace", "^TestOwned.*$", audit)
            self.assertEqual(list(output.iterdir()), [])

    def test_function_boundary_drift_is_rejected_even_after_a_hash_update(self):
        """Do not guess how a different entry point separates configuration."""
        with tempfile.TemporaryDirectory(prefix="throughput-env-test-") as directory:
            server, _, _ = self.fixture(directory)
            raw = (server / "test-env.sh").read_text()
            for changed in (raw.replace("test_env_main\ntest_env_status", "test_env_main extra\ntest_env_status"),
                            raw.replace("    test_env_preflight || return $?", "    changed_preflight || return $?")):
                with self.assertRaisesRegex(ValueError, "function boundary changed"):
                    adapter.configuration_source(changed, server / "test-env.sh")


if __name__ == "__main__":
    unittest.main()
