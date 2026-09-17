"""Freeze the matched live prefix-only comparison without changing the checkout."""
import hashlib
import importlib.util
import json
from pathlib import Path
import shutil
import subprocess
import sys

sys.dont_write_bytecode = True
repo = Path('/Users/brien/urnetwork/connect')
output = Path('/tmp/throughput-fix-2-prefix-live-pinned-v1')
snapshot_path = repo / 'tools/throughput-fix-2-snapshot.py'
spec = importlib.util.spec_from_file_location('snapshot', snapshot_path)
snapshot = importlib.util.module_from_spec(spec)
spec.loader.exec_module(snapshot)
after = output / 'after/source/connect'
metadata = snapshot.snapshot_repository(repo, after, excluded=(repo / 'throughput-fix-2-results',))
before = output / 'before/source/connect'
shutil.copytree(after, before)
before_credit = Path('/tmp/throughput-fix-2-astra-prefix-before/transfer_window_service_credit.go')
shutil.copyfile(before_credit, before / 'transfer_window_service_credit.go')
revision = subprocess.check_output(['git', 'rev-parse', 'HEAD'], cwd=repo, text=True).strip()
for arm, source in [('before', before), ('after', after)]:
    hashes = {name: hashlib.sha256((source / name).read_bytes()).hexdigest() for name in metadata['file_sha256']}
    source_digest = hashlib.sha256()
    for name in sorted(hashes):
        if name.endswith(('.go', '.proto')) or name in ('go.mod', 'go.sum', 'testdata/window_sdk_profiles.json'):
            source_digest.update(name.encode() + b'\0' + (source / name).read_bytes() + b'\0')
    external = snapshot.link_local_replacements(repo, source, source.parent)
    manifest = {
        'arm': arm, 'revision': revision, 'source_directory': str(source),
        'source_sha256': source_digest.hexdigest(),
        'input_sha256': hashlib.sha256(json.dumps(hashes, sort_keys=True).encode()).hexdigest(),
        'file_sha256': hashes,
        'external_local_replacements': external,
        'snapshot_tool_sha256': hashlib.sha256(snapshot_path.read_bytes()).hexdigest(),
        'experiment': 'Only receiver-wait attribution to newly credited cumulative prefix differs between arms.',
    }
    (output / arm / 'source-manifest.json').write_text(json.dumps(manifest, indent=2) + '\n')
print(output)
