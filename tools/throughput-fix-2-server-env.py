#!/usr/bin/env python3
"""Configure only the audited non-database server test selections.

The regular server entry point also attests and probes its local services.
These selected tests own synthetic clients, queues and sockets and do not use
that service tier. Reuse the exact configuration function without presenting
an unrun service preflight as passed. Unknown source or selection changes need
another review; integration modes cannot use this adapter.
"""

import hashlib
import json
from pathlib import Path
import shlex
import sys


def digest(path):
    """Hash source bytes without executing the source being checked."""
    return hashlib.sha256(Path(path).read_bytes()).hexdigest()


def configuration_source(raw, original_path):
    """Preserve path-dependent exports while selecting only configuration."""
    entrypoint = "\ntest_env_main\ntest_env_status=$?\n"
    source_path = '    local source_path="${BASH_SOURCE[0]}"\n'
    main = "\ntest_env_main() {\n    test_env_configure || return $?\n    test_env_preflight || return $?\n}\n"
    if raw.count(entrypoint) != 1 or raw.count(source_path) != 1 or main not in raw:
        raise ValueError("server environment function boundary changed; review required")
    definitions = raw.split(entrypoint, 1)[0]
    definitions = definitions.replace(
        source_path, "    local source_path=" + shlex.quote(str(original_path)) + "\n")
    return definitions + "\ntest_env_configure\n"


def prepare(server, output, mode, pattern, audit):
    """Validate the reviewed source and emit the exact permitted test names."""
    server, output = Path(server).resolve(), Path(output).resolve()
    if mode not in audit["modes"]:
        raise ValueError("configuration-only environment is limited to audited non-database modes")
    selection = audit["modes"][mode]
    if pattern != selection["original_pattern"]:
        raise ValueError("server test selection changed; review required")
    environment = server / "test-env.sh"
    if digest(environment) != audit["environment_sha256"]:
        raise ValueError("server test-env.sh changed; review required")
    if digest(server / "local/run-local-state.sh") != audit["launcher_helper_sha256"]:
        raise ValueError("server configuration helper changed; review required")
    for name, expected in selection["test_file_sha256"].items():
        if digest(server / name) != expected:
            raise ValueError("audited server test source changed: " + name)
    generated = configuration_source(environment.read_text(), environment)
    source_path = output / "server-test-configure.sh"
    source_path.write_text(generated)
    source_path.chmod(0o600)
    tests = sorted(selection["tests"])
    (output / "server-test-pattern.txt").write_text("^(" + "|".join(tests) + ")$\n")
    provenance = {
        "scope": "configuration only for audited non-database tests",
        "mode": mode,
        "race_instrumented": mode == "server-connect-deterministic",
        "configuration_function": "test_env_configure",
        "environment_source": str(environment),
        "environment_sha256": audit["environment_sha256"],
        "launcher_helper_sha256": audit["launcher_helper_sha256"],
        "generated_configuration_sha256": digest(source_path),
        "service_preflight_ran": False,
        "reason": "Selected tests use owned synthetic fixtures; they do not require launcher, PostgreSQL or Redis readiness.",
        "tests": selection["tests"],
        "test_file_sha256": selection["test_file_sha256"],
        "excluded_database_tests": selection["excluded_database_tests"],
        "reviewed_selection_sha256": hashlib.sha256(json.dumps(audit, sort_keys=True).encode()).hexdigest(),
    }
    (output / "server-test-environment.json").write_text(json.dumps(provenance, indent=2) + "\n")


def main():
    """The runner passes a fixed mode and selection, never a preflight override."""
    if len(sys.argv) != 5:
        raise SystemExit("usage: throughput-fix-2-server-env.py SERVER OUTPUT MODE PATTERN")
    audit = json.loads(Path(__file__).with_name("throughput-fix-2-server-tests.json").read_text())
    prepare(*sys.argv[1:], audit)


if __name__ == "__main__":
    main()
