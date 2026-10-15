"""Build and run one immutable hold-release arm, preserving every result."""
import datetime
import hashlib
import json
import os
from pathlib import Path
import platform
import re
import subprocess
import sys

root = Path('/tmp/throughput-fix-2-burst-hold-pinned-v1')
arm = sys.argv[1]
if arm not in ('before', 'after'):
    raise SystemExit('expected before or after')
directory = root / arm
source = directory / 'source/connect'
source_manifest = json.loads((directory / 'source-manifest.json').read_text())
def verify():
    for name, expected in source_manifest['file_sha256'].items():
        if hashlib.sha256((source / name).read_bytes()).hexdigest() != expected:
            raise RuntimeError('source changed: ' + name)
    for module, metadata in source_manifest['local_replacements'].items():
        for name, expected in metadata['file_sha256'].items():
            if hashlib.sha256((source.parent / module / name).read_bytes()).hexdigest() != expected:
                raise RuntimeError('local replacement changed: ' + module + '/' + name)
verify()
selections = [
    ('roots', True, '^TestWindowPacing(OnePermittedBurstKeepsHeldRate|QueueBeyondBurstReleasesHeldRate|PermittedBurstDoesNotMaskForwardBacklog|ServiceObservesQueueOnce|HeldRateFallsOnlyWithCongestion)$', 3),
    ('sdk', False, '^TestWindowPathSdkRetainedDeviceDuplexShort$', 5),
]
for name, race, pattern, count in selections:
    run_dir = directory / name
    run_dir.mkdir()
    binary = directory / ('tests-race' if race else 'tests')
    build = ['go', 'test', '-race=' + str(race).lower(), '-c', '-o', str(binary), '.']
    command = [str(binary), '-test.v', '-test.run', pattern, '-test.count=' + str(count), '-test.timeout=15m']
    manifest = {
        'created_utc': datetime.datetime.now(datetime.timezone.utc).isoformat(),
        'arm': arm, 'selection': name, 'source_sha256': source_manifest['source_sha256'],
        'input_sha256': source_manifest['input_sha256'],
        'source_manifest_sha256': hashlib.sha256((directory / 'source-manifest.json').read_bytes()).hexdigest(),
        'working_directory': str(source), 'build_command': build, 'run_command': command,
        'host_load_average_at_start': os.getloadavg(), 'no_quiescence_wait': True,
        'go_version': subprocess.check_output(['go', 'version'], text=True).strip(),
        'platform': platform.platform(), 'logical_cpus': os.cpu_count(),
        'environment': {key: value for key, value in os.environ.items() if key.startswith('CONNECT_WINDOW_') or key in ('GOMAXPROCS', 'GOGC', 'GOCACHE')},
        'run_script_sha256': hashlib.sha256(Path(__file__).read_bytes()).hexdigest(),
    }
    with (run_dir / 'build.log').open('w') as log:
        built = subprocess.run(build, cwd=source, stdout=log, stderr=subprocess.STDOUT)
    if built.returncode:
        raise RuntimeError('build failed: ' + str(run_dir))
    verify()
    manifest['binary_sha256'] = hashlib.sha256(binary.read_bytes()).hexdigest()
    manifest['go_version_m'] = subprocess.check_output(['go', 'version', '-m', str(binary)], text=True)
    (run_dir / 'manifest.json').write_text(json.dumps(manifest, indent=2) + '\n')
    with (run_dir / 'run.log').open('w') as log:
        result = subprocess.run(command, cwd=source, stdout=log, stderr=subprocess.STDOUT)
    verify()
    raw = (run_dir / 'run.log').read_text()
    rows = []
    for line in raw.splitlines():
        match = re.search(r'service-reading (\{.*\})$', line)
        if match:
            rows.append(json.loads(match[1]))
    (run_dir / 'ledger.jsonl').write_text(''.join(json.dumps(row) + '\n' for row in rows))
    outcomes = [line for line in raw.splitlines() if re.match(r'^--- (PASS|FAIL|SKIP):', line)]
    (run_dir / 'outcomes.txt').write_text('\n'.join(outcomes) + '\n')
    status = {
        'exit_code': result.returncode, 'rows': len(rows),
        'passed': sum(line.startswith('--- PASS:') for line in outcomes),
        'failed': sum(line.startswith('--- FAIL:') for line in outcomes),
        'skipped': sum(line.startswith('--- SKIP:') for line in outcomes),
        'run_log_sha256': hashlib.sha256((run_dir / 'run.log').read_bytes()).hexdigest(),
        'sources_stable_during_build_and_run': True,
        'finished_utc': datetime.datetime.now(datetime.timezone.utc).isoformat(),
        'host_load_average_at_finish': os.getloadavg(), 'no_quiescence_wait': True,
    }
    (run_dir / 'status.json').write_text(json.dumps(status, indent=2) + '\n')
    print(arm, name, json.dumps(status), flush=True)
