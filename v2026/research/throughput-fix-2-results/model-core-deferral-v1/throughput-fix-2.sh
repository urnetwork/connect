#!/usr/bin/env bash
# Reproduce the local window research without the PR author's native rig.
# Usage: tools/throughput-fix-2.sh MODE [output-dir]
# Use a fresh output directory outside the connect/server source checkouts.
# Modes: correctness, model, model-core, sdk-model, regression, ack, pacing, packet, tcp, physical-h1, server,
#        server-integration, server-functional, server-tcp, server-proxy,
#        server-connect-deterministic.
set -euo pipefail

# Parse the complete runner before executing it; edits during a long test
# cannot move bash's file offset into a different command on return.
run_throughput_fix_2() {
mode=${1:-correctness}
repo=$(cd -- "$(dirname -- "$0")/.." && pwd)
output=${2:-$(mktemp -d "${TMPDIR:-/tmp}/throughput-fix-2.XXXXXX")}
mkdir -p -- "$output"
output=$(cd -- "$output" && pwd)
cd -- "$repo"
run_flags=(-test.short=false)

case "$mode" in
  correctness)
    pattern='^(TestAckCompression.*|TestAckResponses.*|TestAckOverflow.*|TestAckWorkerBounds.*|TestEvictionAcknowledgementsFitEveryCarrier|TestGapWake.*|TestSequenceAckWindow.*|TestWindow(TargetIncludes|DeliveryIncludes|DeliveryLargeResidence|DeliveryContractLead).*|TestDeliveryRate.*|TestResendCapacityRelease.*|TestTcpReturn.*|TestTcpSequenceCancelBeforeWritePublication.*|TestTunAckHandoff.*|TestWindow(BurstPacing|Pacing|Mismatch).*|TestWindowPathGapDeadline|TestTheWindowHasOneOwner|TestLandingStructs.*|TestDecodedTransferFramePoolRetainedSizeStaysSmall|TestFamilyStandbyTracks.*)$'
    pattern="$pattern|^TestWindowPerformance.*$|^TestWindowBucketStats.*$|^TestALegacyAcknowledgementCannotOverwriteAnAdvertisement$|^TestRelayInflationUsesConstantSendWindow$|^TestWebRtcNetworkPeerAdmissionWaitsOnDedicatedBudget$|^TestWindowTcp(SocketBatch|CanceledBatch|WorkloadCancel).*$"
    pattern="$pattern|^TestTun(DuplexDataHandoffDoesNotCycleThroughAdmission|FiniteTcpTailProgressesAfterEndpointOwnerReleases)$"
    pattern="$pattern|^TestReceiveSequenceBurstTail.*$"
    pattern="$pattern|^Test(AckReceiverDelay|ReceiverAckTiming|SenderReceiverTiming|RttReceiverTiming).*$"
    pattern="$pattern|^Test(SilentLaneLongerThanTheProbeCadenceStillDrains|SingleReliableLaneQueueInflatedRttDoesNotStorm)$"
    pattern="$pattern|^TestWindowRetained.*$|^Test(AdvertisementRaisesPermissionWithoutLearningCapacity|DeliveryCandidatesRequireFreshPermissionHistory|AClampedWindowReportsTheMemoryShare|ALearnedWindowSurvivesTheMemoryClamp|DeliveryGrowthRemainsBelowTheSmallMemoryShare|AConfiguredCeilingDoesNotReportTheMemoryShare|TheThreePeerBranchesHaveDifferentPermissionCeilings)$"
    pattern="$pattern|^TestWindowDestinationStats.*$|^TestWindowStats.*$"
    pattern="$pattern|^Test(Send|Forward)Buffer(CallerCancel.*|ClosedSequenceStillRecreatesForLiveCaller)$"
    pattern="$pattern|^TestWindowPhysicalH1PolicyGate.*$"
    # This wall-clock occupancy assertion originally ran only in nonrace
    # regression. Its retained-policy rename also matched the prefix above;
    # race instrumentation limits offered throughput below its opening window.
    # Keep the unchanged occupancy gate in regression, alongside other host tests.
    run_flags+=(-test.skip '^TestWindowRetainedSteadyPathPreservesOccupancyBand$')
    build_flags=(-race)
    ;;
  model|model-core)
    # The mismatch matrix already contains the four retained-growth cells;
    # their separate root test is available for focused before/after runs.
    pattern='^(TestWindowPathFifo.*|TestWindowPathGapDeadline|TestWindowCompressionResidence.*|TestWindowPathDeterministicPerformanceMatrix|TestWindowPathBoundsBurstsAtFiniteRelay|TestWindowPathSlowLinkKeepsCapacity|TestWindowPathService.*|TestWindowPathWindowMismatch.*|TestWindowPathSdk.*|TestWindowPathAckTailRoundTripGrowthControl)$'
    if [[ "$mode" == model-core ]]; then
      # Explicit propagation switches will call NetworkQualityChanged once
      # that signal is implemented. Keep the full historical model available.
      run_flags+=(-test.skip '^(TestWindowPathAckTailRoundTripGrowthControl|TestWindowPathServiceRoundTripChanges|TestWindowPathServiceRoundTripGrowthBeyondOldRing)$')
      cat > "$output/deferred-tests.json" <<'JSON'
{
  "reason": "User deferred explicit connection-quality-change performance tests until NetworkQualityChanged is implemented and invoked at the modeled path switch.",
  "tests": [
    "TestWindowPathAckTailRoundTripGrowthControl",
    "TestWindowPathServiceRoundTripChanges",
    "TestWindowPathServiceRoundTripGrowthBeyondOldRing"
  ],
  "retained_core_coverage": "Static paths, cold and warm pacing recovery, sustained congestion, window changes, ACK scheduling, ownership and FIFO fixture correctness.",
  "complete_historical_mode": "model"
}
JSON
    fi
    build_flags=(-race=false)
    ;;
  sdk-model)
    pattern='^TestWindowPathSdk.*$'
    build_flags=(-race=false)
    ;;
  physical-h1)
    export CONNECT_WINDOW_H1_MEASURE=1
    pattern='^TestWindowPhysicalH1SdkSmoke$'
    build_flags=(-race=false)
    ;;
  regression)
    pattern='.'
    # The model mode runs these separately, retaining its full paired ledger.
    run_flags=(-test.skip '^TestWindow(Path|CompressionResidence)')
    build_flags=(-race=false)
    ;;
  ack)
    pattern='^TestAckCompressionHeadDrainDoesNotAllocate$'
    run_flags=(-test.bench '^BenchmarkAckCompression' -test.benchmem -test.benchtime=250ms)
    build_flags=(-race=false)
    ;;
  pacing)
    pattern='^$'
    run_flags=(-test.bench '^(BenchmarkWindowPacing(Service|Receiver|DrainEligibility|TailLifecycle)|BenchmarkAckLifetime|BenchmarkSendSequence(SelectiveAckRecoveryNoEvidence|RouteStallUnchanged))' -test.benchmem -test.benchtime=500ms)
    build_flags=(-race=false)
    ;;
  packet|tcp)
    pattern='^TestWindowPathPerformanceMatrix$'
    if [[ "$mode" == tcp ]]; then
      export CONNECT_WINDOW_TCP_MEASURE=1
      pattern='^TestWindowTcp(Download|Upload)PerformanceMatrix$'
      case "${CONNECT_WINDOW_PATH_DIRECTION:-both}" in
        both) ;;
        download) pattern='^TestWindowTcpDownloadPerformanceMatrix$' ;;
        upload) pattern='^TestWindowTcpUploadPerformanceMatrix$' ;;
        *) printf 'Unknown TCP direction: %s\n' "$CONNECT_WINDOW_PATH_DIRECTION" >&2; exit 2 ;;
      esac
    else
      export CONNECT_WINDOW_PATH_MEASURE=1
    fi
    export CONNECT_WINDOW_PATH_RTT_US=${CONNECT_WINDOW_PATH_RTT_US:-300,100000}
    export CONNECT_WINDOW_PATH_FLOWS=${CONNECT_WINDOW_PATH_FLOWS:-1,8}
    export CONNECT_WINDOW_PATH_ACK_MS=${CONNECT_WINDOW_PATH_ACK_MS:-10}
    export CONNECT_WINDOW_PATH_REPETITIONS=${CONNECT_WINDOW_PATH_REPETITIONS:-3}
    export CONNECT_WINDOW_PATH_SECONDS=${CONNECT_WINDOW_PATH_SECONDS:-3}
    build_flags=(-race=false)
    ;;
  server)
    cd -- "$repo/../server"
    pattern='^(TestSendPooledReceive.*|TestReliableExchangeQueueSaturation.*|TestExchangeGenerationRetires.*|TestResident.*|TestProductionSocketReaders.*|TestConnectH1(ReadyDrain|UserReadyBatch|WriteBatchForConn|WorkersJoin|BatchResponseWriter).*|TestConnectH3(InitialDatagram|TransferCarrier).*|TestExchangeHeaderUnreliableTransferGobCompatibility|TestExchangeOutboundBatchFormation)$'
    build_flags=(-race)
    ;;
  server-connect-deterministic)
    cd -- "$repo/../server"
    pattern='^(TestSendPooledReceive.*|TestReliableExchangeQueueSaturation.*|TestExchangeGenerationRetires.*|TestResident.*|TestProductionSocketReaders.*|TestConnectH1(ReadyDrain|UserReadyBatch|WriteBatchForConn|WorkersJoin|BatchResponseWriter).*|TestConnectH3(InitialDatagram|TransferCarrier).*|TestExchangeHeaderUnreliableTransferGobCompatibility|TestExchangeOutboundBatchFormation)$'
    run_flags=(-test.skip '^(TestResidentControllerReturnsDroppedResponseFrameOwnership|TestResidentRunJoinsStreamHopListenerCallback)$')
    build_flags=(-race)
    ;;
  server-integration|server-functional|server-tcp)
    cd -- "$repo/../server"
    pattern='^(TestConnectH[13](Encrypted(AllowFallback)?)?|TestExchangeRelayPoolBalance|TestConnectMultiClientTcpDirectionalPerformance)$'
    if [[ "$mode" == server-functional ]]; then
      pattern='^(TestConnectH[13](Encrypted(AllowFallback)?)?|TestExchangeRelayPoolBalance)$'
    elif [[ "$mode" == server-tcp ]]; then
      pattern='^TestConnectMultiClientTcpDirectionalPerformance$'
    fi
    build_flags=(-race=false)
    ;;
  server-proxy)
    cd -- "$repo/../server"
    package=./proxy
    # server/test.sh runs proxy's wall-clock packet tier without -race.  Keep
    # the database-backed WireGuard handoff cases in the separate integration
    # attempt; this mode is the deterministic ownership/lifecycle tier.
    pattern='^(TestProxyDeviceMemoryBudget.*|TestProxyDeviceSendBorrowed.*|TestProxyDeviceWireGuardReturn.*|TestProxyDeviceTunDial.*|TestProxyDeviceManager.*|TestProxyLifecycle.*|TestWgClient.*|TestWindowIdentity.*|TestDrainCoordinator.*|TestProxyIngressCollector.*|TestProxyConnectionMetrics.*|TestProxySessionMaximum.*|TestProxySessionDuration.*|TestWireGuardPacketMetrics.*)$'
    build_flags=(-race=false)
    ;;
  *) printf 'Unknown mode: %s\n' "$mode" >&2; exit 2 ;;
esac

package=./
if [[ "$mode" == server* ]]; then package=./connect; fi
if [[ "$mode" == server-proxy ]]; then package=./proxy; fi
if [[ "$mode" == server* ]]; then
  # Use the same environment bootstrap as server/connect/test.sh and
  # server/test.sh. Their database-backed test selections remain separate.
  source "$repo/../server/test-env.sh"
fi
python3 - "$repo" "$output" "$mode" "$pattern" "$package" "${build_flags[*]}" "${run_flags[@]}" <<'PY'
import datetime, hashlib, importlib.util, json, os, pathlib, platform, subprocess, sys
repo, output, mode, pattern, package, build_flags, *run_flags = sys.argv[1:]
sys.dont_write_bytecode = True
worktree = subprocess.run(['git', '-C', output, 'rev-parse', '--is-inside-work-tree'],
                         text=True, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
if worktree.returncode == 0 and worktree.stdout.strip() == 'true':
    raise SystemExit('output directory must be outside Git worktrees; use a fresh /tmp directory')
snapshot_tool = pathlib.Path(repo, 'tools/throughput-fix-2-snapshot.py')
spec = importlib.util.spec_from_file_location('throughput_snapshot', snapshot_tool)
snapshot = importlib.util.module_from_spec(spec)
spec.loader.exec_module(snapshot)
def command(*args, cwd=None):
    return subprocess.check_output(args, cwd=cwd, text=True).strip()
def source_manifest(root):
    names = command('git', 'ls-files', '--cached', '--others', '--exclude-standard',
                    '--', '*.go', '*.proto', 'go.mod', 'go.sum',
                    'testdata/window_sdk_profiles.json', cwd=root).splitlines()
    digest = hashlib.sha256()
    for name in sorted(set(names)):
        path = pathlib.Path(root, name)
        if path.is_file():
            digest.update(name.encode() + b'\0' + path.read_bytes() + b'\0')
    return {'revision': command('git', 'rev-parse', 'HEAD', cwd=root),
            'branch': command('git', 'branch', '--show-current', cwd=root),
            'source_sha256': digest.hexdigest(),
            'dirty': bool(command('git', 'status', '--porcelain', cwd=root))}
manifest = {
    'created_utc': datetime.datetime.now(datetime.timezone.utc).isoformat(),
    'mode': mode, 'test_pattern': pattern, 'run_flags': run_flags, 'connect': source_manifest(repo),
    'package': package, 'build_flags': build_flags.split(),
    'runner_sha256': hashlib.sha256(pathlib.Path(repo, 'tools/throughput-fix-2.sh').read_bytes()).hexdigest(),
    'benchmark_ledger_parser_sha256': hashlib.sha256(pathlib.Path(repo, 'tools/throughput-fix-2-ledger.py').read_bytes()).hexdigest(),
    'host_load_average_at_start': os.getloadavg(),
    'go': command('go', 'version'), 'os': platform.system(),
    'os_release': platform.release(), 'architecture': platform.machine(),
    'logical_cpus': os.cpu_count(),
    'environment': {k: v for k, v in os.environ.items()
                    if k.startswith('CONNECT_WINDOW_') or k in ('GOMAXPROCS', 'GOGC')},
    'instrument': ('owned TLS/WebSocket H1 relay, gVisor TUN and loopback socket origin; '
                   'Transfer encryption disabled; no native kernel TUN or server authentication'
                   if mode == 'physical-h1' else
                   'local FIFO plus optional gVisor TUN and loopback socket origin; no native kernel TUN'),
}
deferred_tests = pathlib.Path(output, 'deferred-tests.json')
if deferred_tests.is_file():
    manifest['deferred_tests'] = json.loads(deferred_tests.read_text())
    manifest['deferred_tests_sha256'] = hashlib.sha256(deferred_tests.read_bytes()).hexdigest()
if mode.startswith('server'):
    manifest['server'] = source_manifest(str(pathlib.Path(repo).parent / 'server'))
    manifest['server_test_environment_source'] = str(pathlib.Path(repo).parent / 'server' / 'test-env.sh')
    manifest['server_test_environment'] = {
        key: os.environ[key] for key in (
            'WARP_ENV', 'WARP_SERVICE', 'WARP_BLOCK', 'WARP_VERSION',
            'WARP_TEST_ENV_FAIL_FAST', 'WARP_TEST_ENV_USE_PORTABLE_RESOURCES',
            'WARP_TEST_ENV_ALLOW_UNMANAGED_PORTABLE_SERVICES') if key in os.environ}
snapshot_parent = pathlib.Path(output, 'source')
snapshot_inputs = {}
roots = {'connect': pathlib.Path(repo)}
if mode.startswith('server'):
    roots['server'] = pathlib.Path(repo).parent / 'server'
if any(snapshot_parent.is_relative_to(root) for root in roots.values()):
    raise SystemExit('output directory must be outside the source checkouts; use a fresh /tmp directory')
for name, root in roots.items():
    destination = snapshot_parent / name
    snapshot_inputs[name] = snapshot.snapshot_repository(
        root, destination, excluded=(pathlib.Path(output), root / 'throughput-fix-2-results'))
    names = command('git', 'ls-files', '--cached', '--others', '--exclude-standard',
                    '--', '*.go', '*.proto', 'go.mod', 'go.sum',
                    'testdata/window_sdk_profiles.json', cwd=root).splitlines()
    digest = hashlib.sha256()
    for filename in sorted(set(names)):
        path = destination / filename
        if path.is_file():
            digest.update(filename.encode() + b'\0' + path.read_bytes() + b'\0')
    if digest.hexdigest() != manifest[name]['source_sha256']:
        raise SystemExit(f'{name} sources changed while snapshotting; rerun for a consistent manifest')
external = []
for name, root in roots.items():
    external.extend(snapshot.link_local_replacements(root, snapshot_parent / name, snapshot_parent))
manifest['source_snapshot'] = {
    'repositories': {
        name: {key: value for key, value in metadata.items() if key != 'file_sha256'}
        for name, metadata in snapshot_inputs.items()},
    'external_local_replacements': sorted(set(external)),
    'scope': 'Compile and run inside copied repository inputs; external local replacements and host services are not frozen.',
    'snapshot_tool_sha256': hashlib.sha256(snapshot_tool.read_bytes()).hexdigest(),
}
pathlib.Path(output, 'snapshot-inputs.json').write_text(json.dumps(snapshot_inputs, indent=2) + '\n')
pathlib.Path(output, 'manifest.json').write_text(json.dumps(manifest, indent=2) + '\n')
PY

build_repo="$output/source/connect"
if [[ "$mode" == server* ]]; then build_repo="$output/source/server"; fi
cd -- "$build_repo"
go test "${build_flags[@]}" -c -o "$output/tests" "$package"
python3 - "$output" <<'PY'
import hashlib, importlib.util, json, pathlib, sys
sys.dont_write_bytecode = True
output = pathlib.Path(sys.argv[1])
snapshot_tool = output / 'source/connect/tools/throughput-fix-2-snapshot.py'
spec = importlib.util.spec_from_file_location('throughput_snapshot', snapshot_tool)
snapshot = importlib.util.module_from_spec(spec)
spec.loader.exec_module(snapshot)
manifest_path = output / 'manifest.json'
manifest = json.loads(manifest_path.read_text())
for name, metadata in json.loads((output / 'snapshot-inputs.json').read_text()).items():
    snapshot.verify_snapshot(output / 'source' / name, metadata)
manifest['binary_sha256'] = hashlib.sha256((output / 'tests').read_bytes()).hexdigest()
manifest['sources_stable_during_build'] = True
manifest['source_inspection_uses_build_snapshot'] = True
manifest_path.write_text(json.dumps(manifest, indent=2) + '\n')
PY

status=0
# Both relative source reads and runtime.Caller use the same frozen build tree.
if [[ "$mode" == server-proxy ]]; then
  cd -- "$build_repo/proxy"
elif [[ "$mode" == server* ]]; then
  cd -- "$build_repo/connect"
fi
"$output/tests" -test.v -test.run "$pattern" "${run_flags[@]}" -test.count=1 -test.timeout=30m > "$output/run.log" 2>&1 || status=$?
python3 - "$output" "$status" <<'PY'
import datetime, hashlib, importlib.util, json, os, pathlib, re, sys
output = pathlib.Path(sys.argv[1])
sys.dont_write_bytecode = True
parser_path = output / 'source/connect/tools/throughput-fix-2-ledger.py'
manifest = json.loads((output / 'manifest.json').read_text())
if hashlib.sha256(parser_path.read_bytes()).hexdigest() != manifest['benchmark_ledger_parser_sha256']:
    raise SystemExit('benchmark ledger parser differs from the recorded source')
spec = importlib.util.spec_from_file_location('throughput_ledger', parser_path)
ledger = importlib.util.module_from_spec(spec)
spec.loader.exec_module(ledger)
rows = []
for line in (output / 'run.log').read_text().splitlines():
    benchmark = ledger.benchmark_reading(line)
    if benchmark is not None:
        rows.append(benchmark)
    physical = ledger.physical_reading(line)
    if physical is not None:
        rows.append(physical)
    service = re.search(r'service-reading (\{.*\})$', line)
    if service:
        row = json.loads(service[1])
        row['Kind'] = 'model-service'
        rows.append(row)
    match = re.search(r'(comparison )?repetition=(\d+) (\{.*\})$', line)
    if match:
        row = json.loads(match[3])
        row['Repetition'] = int(match[2])
        row['Kind'] = 'comparison' if match[1] else 'reading'
        rows.append(row)
    match = re.search(r'rtt=(\S+) flows=(\d+) compression=(\S+) ceiling=([\d.]+) fixed=([\d.]+) min-flow=([\d.]+) model Mb/s$', line)
    if match:
        rows.append(dict(Kind='model', RoundTrip=match[1], Flows=int(match[2]),
                         Compression=match[3], CeilingMbps=float(match[4]),
                         DeliveryMbps=float(match[5]), MinFlowMbps=float(match[6])))
(output / 'ledger.jsonl').write_text(''.join(json.dumps(row) + '\n' for row in rows))
comparisons = [row for row in rows if row.get('Kind') == 'comparison']
(output / 'status.json').write_text(json.dumps({
    'exit_code': int(sys.argv[2]), 'rows': len(rows),
    'comparison_count': len(comparisons),
    'failed_comparisons': sum(bool(row.get('FailureReasons')) for row in comparisons),
    'censored_comparisons': sum(bool(row.get('CensoredReasons')) for row in comparisons),
    'finished_utc': datetime.datetime.now(datetime.timezone.utc).isoformat(),
    'host_load_average_at_finish': os.getloadavg(),
}) + '\n')
PY
printf 'Results: %s (exit %s)\n' "$output" "$status"
exit "$status"
}

run_throughput_fix_2 "$@"
