"""Freeze the matched hold-release comparison and local Go replacements."""
import hashlib
import importlib.util
import json
from pathlib import Path
import shutil
import subprocess
import sys

sys.dont_write_bytecode = True
repo = Path('/Users/brien/urnetwork/connect')
output = Path('/tmp/throughput-fix-2-burst-hold-pinned-v1')
snapshot_path = repo / 'tools/throughput-fix-2-snapshot.py'
spec = importlib.util.spec_from_file_location('snapshot', snapshot_path)
snapshot = importlib.util.module_from_spec(spec)
spec.loader.exec_module(snapshot)
after = output / 'after/source/connect'
metadata = snapshot.snapshot_repository(repo, after, excluded=(repo / 'throughput-fix-2-results',))
glog_metadata = snapshot.snapshot_repository(repo.parent / 'glog', after.parent / 'glog')
before = output / 'before/source/connect'
shutil.copytree(after.parent, before.parent)
old_pacing = Path('/tmp/throughput-fix-2-prefix-live-pinned-v1/after/source/connect/transfer_window_pacing.go')
assert hashlib.sha256(old_pacing.read_bytes()).hexdigest() == 'b6acc02099e55a24d7f73663bd5e2d97d88fc85f0b39ac2404cada74cb6ef755'
shutil.copyfile(old_pacing, before / 'transfer_window_pacing.go')
assert hashlib.sha256((after / 'transfer_window_pacing.go').read_bytes()).hexdigest() == '6d1fc9afcf378ff01ad419371408717745bd95be529c8579eb4fda7c5e8b3fda'
revision = subprocess.check_output(['git', 'rev-parse', 'HEAD'], cwd=repo, text=True).strip()
for arm, source in [('before', before), ('after', after)]:
    hashes = {name: hashlib.sha256((source / name).read_bytes()).hexdigest() for name in metadata['file_sha256']}
    source_digest = hashlib.sha256()
    for name in sorted(hashes):
        if name.endswith(('.go', '.proto')) or name in ('go.mod', 'go.sum', 'testdata/window_sdk_profiles.json'):
            source_digest.update(name.encode() + b'\0' + (source / name).read_bytes() + b'\0')
    manifest = {
        'arm': arm, 'revision': revision, 'source_directory': str(source),
        'source_sha256': source_digest.hexdigest(),
        'input_sha256': hashlib.sha256(json.dumps(hashes, sort_keys=True).encode()).hexdigest(),
        'file_sha256': hashes,
        'local_replacements': {'glog': glog_metadata},
        'external_local_replacements': [],
        'snapshot_tool_sha256': hashlib.sha256(snapshot_path.read_bytes()).hexdigest(),
        'experiment': 'Only RTT-based held-pace release differs. Discovery, forward backlog, recovery, physical bursts and SDK gates stay unchanged.',
    }
    (output / arm / 'source-manifest.json').write_text(json.dumps(manifest, indent=2) + '\n')
print(output)
