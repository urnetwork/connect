#!/usr/bin/env python3
"""Capture SDK window settings through its real sizing helpers, without network clients.

Usage: python3 tools/throughput-fix-2-sdk-settings.py SDK_DIR OUTPUT_DIR
The Go overlay selects the SDK's mobile policy on the local host. It changes
only that platform selector and appends an observation test; SDK files remain
untouched. This verifies constructor settings, not a mobile runtime or kernel.
"""

import hashlib
import json
from pathlib import Path
import subprocess
import sys


PROBE = r'''

// Observe actual constructors and provide-mode sizing without creating clients.
func TestThroughputFix2SdkWindowSettings(t *testing.T) {
    previousBudget := connect.MemoryBudget()
    previousWindow := connect.DefaultWindowSizing()
    defer connect.SetMemoryBudget(previousBudget)
    defer connect.SetWindowSizing(previousWindow)
    defer func() { throughputWindowSettingsMobile = nil }()
    connect.SetWindowSizing(connect.WindowSizingFromDelivery)
    pool := func(budget *connect.TransferMemoryBudget) any {
        if budget == nil { return nil }
        return budget.TotalByteCount()
    }
    record := func(name string, settings *connect.ClientSettings, target connect.ByteCount, active, explicitH1 bool) {
        send, receive := settings.SendBufferSettings, settings.ReceiveBufferSettings
        row := map[string]any{
            "Name": name, "MobilePolicy": mobileRuntime(), "ProcessBudget": connect.MemoryBudget(),
            "DeviceTarget": target, "Providing": active, "WindowSizing": send.WindowSizing,
            "ExplicitH1": explicitH1, "ClientSendQueueCount": settings.SendBufferSize,
            "SendQueueCount": send.SequenceBufferSize, "AckQueueCount": send.AckBufferSize,
            "SendPool": pool(send.ResendQueueBudget), "ReceivePool": pool(receive.ReceiveQueueBudget),
            "PackPool": pool(receive.PackQueueBudget), "SendInitial": send.ResendQueueMaxByteCount,
            "SendMinimum": send.ResendQueueMinByteCount, "WindowScale": send.DeliverySizedWindowScale,
            "TargetGoodputByteRate": send.TargetGoodputByteRate, "LaneFloor": send.LaneFloorByteCount,
            "WindowCeiling": send.DeliverySizedWindowCeilingByteCount,
            "ReceiveMaximum": receive.ReceiveQueueMaxByteCount, "ReceiveMinimum": receive.ReceiveQueueMinByteCount,
            "RetainedReceiveAccounting": receive.ReceiveQueueRetainedByteAccounting,
            "RetainedPackAccounting": receive.PackQueueRetainedByteAccounting,
            "AdvertiseReceiveWindow": receive.AdvertiseReceiveWindow,
            "AckCompressionNs": receive.AckCompressTimeout, "LogicalDataLanes": send.LogicalDataLaneCount,
            "H1QueueCount": receive.H1SequenceBufferSize, "H1QueueBytes": receive.H1SequenceBufferByteCount,
            "H1QueueAdaptiveCount": receive.H1SequenceBufferAdaptiveMaxSize,
            "H1QueueAdaptiveStepCount": receive.H1SequenceBufferAdaptiveStepSize,
            "H1QueueAdaptiveThreshold": receive.H1SequenceBufferAdaptiveSaturationThreshold,
            "H1QueueAdaptiveWindowNs": receive.H1SequenceBufferAdaptiveSaturationWindow,
            "H1QueueAdaptiveBytes": receive.H1SequenceBufferAdaptiveMaxByteCount,
            "H1QueueAdaptiveStepBytes": receive.H1SequenceBufferAdaptiveStepByteCount,
            "H1PackHandoffTimeoutNs": receive.H1PackHandoffTimeout,
            "ReliablePackHandoffTimeoutNs": receive.ReliablePackHandoffTimeout,
            "H1AckHandoffTimeoutNs": receive.H1AckHandoffTimeout,
            "ReceiveQueueCount": receive.SequenceBufferSize, "ReceiveQueueBytes": receive.SequenceBufferByteCount,
            "MinimumMessageLimit": settings.MinimumMessageLenLimit(),
        }
        data, err := json.Marshal(row)
        if err != nil { t.Fatal(err) }
        t.Logf("window-sdk-settings %s", data)
    }
    for _, process := range []connect.ByteCount{0, 384 * 1024 * 1024} {
        connect.SetMemoryBudget(process)
        record("connect-process-default", connect.DefaultClientSettings(), 0, false, false)
    }
    for _, mobile := range []bool{false, true} {
        throughputWindowSettingsMobile = &mobile
        process := connect.ByteCount(0)
        if mobile { process = 32 * 1024 * 1024 }
        connect.SetMemoryBudget(process)
        for _, active := range []bool{false, true} {
            settings := DefaultDeviceLocalSettings()
            applyMobileLowMemoryClientSettings(&settings.ClientSettings, settings.MemoryTargetByteCount)
            device := &DeviceLocal{settings: settings}
            device.applyProvideMemorySharesWithLock(active)
            destination := newDeviceClientSettings(connect.DefaultClientSettingsWithBufferSize(settings.SequenceBufferSize), "https://api.example", nil)
            destination.SendBufferSettings.ResendQueueBudget = settings.SendBufferSettings.ResendQueueBudget
            destination.ReceiveBufferSettings.ReceiveQueueBudget = settings.ReceiveBufferSettings.ReceiveQueueBudget
            destination.ReceiveBufferSettings.PackQueueBudget = settings.ReceiveBufferSettings.PackQueueBudget
            applyMobileLowMemoryClientSettings(destination, settings.MemoryTargetByteCount)
            record("sdk-device-default", destination, settings.MemoryTargetByteCount, active, false)
            if mobile {
                applyMobileH1PerformanceClientSettings(destination, settings.MemoryTargetByteCount, true)
                record("sdk-device-h1", destination, settings.MemoryTargetByteCount, active, true)
            }
            if active {
                provider := newDeviceClientSettings(&settings.ClientSettings, "https://api.example", nil)
                _, _, _, share := deviceMemoryShares(settings)
                configureDeviceLocalProviderMemory(provider, share)
                record("sdk-provider-default", provider, settings.MemoryTargetByteCount, true, false)
                if mobile {
                    provider = newDeviceClientSettings(&settings.ClientSettings, "https://api.example", nil)
                    applyMobileH1PerformanceClientSettings(provider, settings.MemoryTargetByteCount, true)
                    configureDeviceLocalProviderMemory(provider, share)
                    record("sdk-provider-h1", provider, settings.MemoryTargetByteCount, true, true)
                }
                if provider.SendBufferSettings.ResendQueueBudget == settings.SendBufferSettings.ResendQueueBudget ||
                    provider.ReceiveBufferSettings.ReceiveQueueBudget == settings.ReceiveBufferSettings.ReceiveQueueBudget ||
                    provider.ReceiveBufferSettings.PackQueueBudget != settings.ReceiveBufferSettings.PackQueueBudget {
                    t.Fatal("provider must own its transfer pair and share the device Pack budget")
                }
            }
        }
    }
}
'''


def source_manifest(repo):
    """Use the throughput runner's canonical source digest for each checkout."""
    def git(*args):
        return subprocess.check_output(['git', *args], cwd=repo, text=True).strip()
    names = git('ls-files', '--cached', '--others', '--exclude-standard', '--',
                '*.go', '*.proto', 'go.mod', 'go.sum',
                'testdata/window_sdk_profiles.json').splitlines()
    digest = hashlib.sha256()
    for name in sorted(set(names)):
        path = repo / name
        if path.is_file():
            digest.update(name.encode() + b'\0' + path.read_bytes() + b'\0')
    return {'revision': git('rev-parse', 'HEAD'), 'source_sha256': digest.hexdigest()}


def main():
    """Build a pinned diagnostic overlay and retain settings plus provenance."""
    sdk, output = map(lambda value: Path(value).resolve(), sys.argv[1:])
    connect = Path(__file__).resolve().parent.parent
    output.mkdir(parents=True, exist_ok=False)
    before = {'sdk': source_manifest(sdk), 'connect': source_manifest(connect)}
    policy_path = sdk / 'mobile_memory_policy.go'
    test_path = sdk / 'device_local_memory_test.go'
    policy = policy_path.read_text()
    original = 'func mobileRuntime() bool {\n'
    assert policy.count(original) == 1
    policy = policy.replace(original, original +
        '\tif throughputWindowSettingsMobile != nil { return *throughputWindowSettingsMobile }\n', 1)
    policy += '\nvar throughputWindowSettingsMobile *bool\n'
    tests = test_path.read_text()
    assert '"encoding/json"' not in tests
    tests = tests.replace('import (', 'import (\n\t"encoding/json"', 1) + PROBE
    policy_overlay, test_overlay = output / 'policy.go', output / 'settings_test.go'
    policy_overlay.write_text(policy)
    test_overlay.write_text(tests)
    subprocess.run(['gofmt', '-w', str(policy_overlay), str(test_overlay)], check=True)
    overlay = output / 'overlay.json'
    overlay.write_text(json.dumps({'Replace': {
        str(policy_path): str(policy_overlay), str(test_path): str(test_overlay),
    }}, indent=2) + '\n')
    binary = output / 'tests'
    subprocess.run(['go', 'test', '-c', '-overlay', str(overlay), '-o', str(binary), '.'], cwd=sdk, check=True)
    after = {'sdk': source_manifest(sdk), 'connect': source_manifest(connect)}
    manifest = {'before': before, 'after': after, 'sources_stable_during_build': before == after,
        'binary_sha256': hashlib.sha256(binary.read_bytes()).hexdigest(),
        'overlay_sha256': hashlib.sha256(overlay.read_bytes()).hexdigest(),
        'scope': 'SDK constructor and pure sizing helpers; host-selected mobile policy; no network client or mobile runtime claim'}
    (output / 'manifest.json').write_text(json.dumps(manifest, indent=2) + '\n')
    if before != after:
        raise RuntimeError('Source changed during the SDK settings build; retain this attempt as invalid')
    result = subprocess.run([str(binary), '-test.v', '-test.run', '^TestThroughputFix2SdkWindowSettings$',
                             '-test.count=1', '-test.timeout=2m'], cwd=sdk, text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
    (output / 'run.log').write_text(result.stdout)
    rows = [json.loads(line.split('window-sdk-settings ', 1)[1]) for line in result.stdout.splitlines()
            if 'window-sdk-settings ' in line]
    (output / 'settings.json').write_text(json.dumps(rows, indent=2) + '\n')
    (output / 'status.json').write_text(json.dumps({'exit_code': result.returncode, 'rows': len(rows)}) + '\n')
    print(json.dumps({'output': str(output), 'exit_code': result.returncode, 'rows': len(rows)}))
    if result.returncode:
        raise SystemExit(result.returncode)


if __name__ == '__main__':
    main()
