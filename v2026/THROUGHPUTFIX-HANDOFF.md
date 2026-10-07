# Throughput fix 2 handoff

Date: 2026-09-17
Connect branch: `throughput-fix-2`
Latest connect production commit: `f8261152` (`Fix shared window pacing and
provider ACK admission`)

## Current result

The core approach is implemented: the learned byte window grows during ordinary
operation and does not shrink until `NetworkQualityChanged` opens a bounded
five-second remeasurement generation. Fresh qualified service and RTT evidence
must agree before that generation can replace the retained window. Adaptive
pacing continues to respond to sustained feedback without an OS notification.
A hard `NetworkChanged` also invalidates quality exactly once.

Cold pacing is now scoped to the same physical service as its reservation
clock. Confirmed H1 delivery is counted once across sibling logical lanes in a
six-entry exact-endpoint ring and is used only when no positive serialization
rate exists. Logical delivery and retained window sizing remain per lane.

The provider's per-flow pure-ACK worker now keeps its one 52-byte cumulative
control through bounded, cancellable Transfer admission. Shared public and
synthesized callbacks retain their zero-wait refusal behavior.

The public event and peer propagation are implemented. A local event resets all
local estimators and sends one reliable 24-byte reserved-subprotocol message to
known outbound and receive-only provider peers. A received event is scoped to
the authenticated source, deduplicated by instance/generation, bounded in
memory, and never echoed. Refused zero-wait admission is retained for retry.

The event is exposed through the SDK and called by Android, Apple, Windows and
Linux. Android covers Wi-Fi bars/link bands plus cell bars/type. Apple covers
active cell type, path flags and sampled Wi-Fi bars; its public APIs do not
expose cell bars. Windows covers native WLAN bar changes. Linux classifies
physical default interface/carrier changes separately from Wi-Fi bar/link-rate
changes.

## Root fixes on the branch

- Grow-only learned windows with bounded, fresh-evidence shrink permission.
- Shared service generation ownership and stale sibling-publication rejection.
- First-new-path cold recovery floor for a pending long RTT.
- Natural-drain pacing grace while physical flight is already falling.
- Public/local/provider `NetworkQualityChanged` propagation and lifecycle.
- Idempotent callback storms: 128 concurrent notifications coalesce to one
  generation while 32 public statistics reads overlap the reset; a later
  generation is admitted only after the five-second quiet boundary.
- Shared cumulative delivery across equal/unequal one-, two- and four-lane
  services, with exact freshness, rebucketing, permission and generation
  boundaries.
- Dedicated provider pure-ACK admission, newer cumulative progress, public
  zero-wait control and cancellation/pool ownership.
- Reserved internal subprotocol ids filtered from public discovery.
- P2P test-owned route joins before final asynchronous stats snapshots.
- The previously deferred path-switch models now signal quality at the switch.

ACK compression remains one cumulative head ACK plus bounded oldest-first SACKs
above the head. The head absorbs pending SACKs at or below it. Existing pacing,
maximum-message-size and deterministic compression roots remain unchanged.

## Final local validation

- Current canonical correctness: 566 race-enabled passes, zero failures,
  skips, panics, warnings or race diagnostics. The clean `f8261152` artifact is
  `/tmp/throughput-fix-2-clean-correctness-1789630306`; source digest
  `a22399024dd9b3547d0e82a8a6e217a2718ce5171f2bd82a8762dc9681bec850`;
  run-log SHA-256
  `88122db98ed3132fa0e55f9b55211ff8b3f4d64c3f6d38f471c0a2077ecd0e8b`.
- Shared-delivery/quality focus: 294/294 race executions pass.
- Provider pure-ACK focus: 50/50; adjacent provider recovery: 18/18.
- Frequent callback/statistics storm: 10/10 race repetitions with no recovered
  error or diagnostic.
- Shared ring cost: 360 bytes per service and zero allocations. The six-entry
  medians are 375.5 ns publish, 450.0 ns estimate and 98.21 ns rebucket; the
  exact 64-entry overlay is slower and uses 3,248 additional bytes per service.
- 18 core quality/drain roots: `-race -count=3`, pass.
- Three programmed path-change models: pass.
- 12-cell throughput correctness matrix: pass, zero comparison failures.
- Final clean throughput model: pass, 858 rows; artifact
  `/tmp/throughput-fix-2-clean-model-1789627905`, run-log SHA-256
  `b28b0ab439a3487d6fcac68dc4aba96b9f45c096f9fa69753e2d5dfc153a144e`.
- Final clean broad regression: pass, 12 rows, 25 existing
  environment/candidate-gated skips and no failed or censored comparisons;
  artifact `/tmp/throughput-fix-2-clean-regression-1789628988`, run-log SHA-256
  `66463b8174cd28f23d5d593a0826ea78b830cd88419d5277d5d1417723eb67e7`.
- Final clean pacing gate: 43 rows, every operation 0 B/op and 0 allocs/op;
  artifact `/tmp/throughput-fix-2-clean-pacing-1789630209`, run-log SHA-256
  `808f64a8e659d8862c1cca7c8c7969a2a4eb0bbca1cc5630ba22d34cd7453fb9`.
- Final clean physical H1 smoke: four rows and one calibrated comparison pass
  at 91.526 versus 91.552 Mb/s; artifact
  `/tmp/throughput-fix-2-clean-physical-h1-final-1789630390`, run-log SHA-256
  `22a0b77f7a10180dc879ba7e67fa7aec5cd1d00ab039b71843999837c0e7febb`.
- Public quality, subprotocol boundary and P2P focused selection:
  `-race -count=3`, pass in 8.476 seconds.
- Three P2P forced-order roots plus five real fast/legacy tests:
  40/40 race executions pass in 11.496 seconds.
- SDK quality/hard-network focused tests: pass.
- Android Github debug unit tests for network trackers: build and pass.
- Apple modified-source parse and isolated iOS 17 API typecheck: pass.
- Windows `EgressMonitor.cpp` MinGW syntax compile: pass.
- Linux network-quality parser/classifier suite: 5/5 pass.

The frozen v3 source passes the corrected focused race selection, all three
changed-path models, 12 correctness rows, all 858 full-model rows and the broad
regression. Its archive is
`/tmp/throughput-fix-2-terra-final-v3-1789614907`. The earlier test-only v4 change
added callback-storm/statistics coverage; every `ClientNetworkQuality`,
`NetworkQuality` and `WindowQuality` root passes three times under `-race`.
The v4 log is `/tmp/throughput-fix-2-terra-v4-focused-1789616840/run.log`,
SHA-256 `18acdd7a58804ac8357ef44320d2418bc6b744a1c40ec5c4cd78220b9472b9e4`.
Both runs started immediately under substantial concurrent host load.

## Server integration

Configured server integration is complete. The local services were exercised
through `server/test-env.sh`; direct compiler wrappers used the installed Xcode
toolchain without changing host state. The final runs started immediately
under the existing host load.

- The final-source `server/connect`, `server/connect/perfvar`,
  `server/connect/sim-latency` and resource-bomb packages pass in 4,055.767,
  1,437.994, 33.015 and 0.246 seconds. The log SHA-256 is
  `b01672b716faf2039e3cbe55c4a22f93133b8d05df98c8a006203702ca479c24`.
- The corrected `server/proxy` and `server/proxy/acceptance` packages pass in
  334.528 and 6.132 seconds. The log SHA-256 is
  `38395dac8dca9013c13480e7f5304a4ad6d393daa50acff0c62071472a29d73f`.
- The complete final proxy log has zero recovered-panic, unexpected-error,
  nil-pointer, fatal, warning and race matches.

The first connect package-local run exposed an unrestricted-discovery defect
after its real packages passed. Server commit `21acdcb5` now uses the canonical
top-level selector and covers exact subtree selection, caller arguments and
selector failure with deterministic tests. The rebased branch passes 21/21
harness race executions and the official no-test traversal selects exactly the
four intended packages. The proxy run exposed a separate pre-existing
signal-cleanup bug. Server commit `7e19ae5d` fixes that wrapper and adds nine
deterministic lifecycle roots; its rebased validation passes 27/27 race
executions.

The frozen proxy source first exposed that server `23135c01` expects six SDK
telemetry fields that remain in four pre-existing uncommitted SDK files. The
exact patch is pinned as clean detached SDK commit `17a7a332`, patch SHA-256
`d2adcf17943a1338faaa1b65b233cd0e82b43510724e941017e127ddac9db48d`.
The first full run on that pairing returned zero but contained a recovered
nil-pointer panic from an incomplete server memory-budget test fixture. The
original test falsely passes three times while emitting three panics; new roots
fail 6/6 against that shape, and a context-lifetime control fails 3/3. Server
commit `27b7dad9` corrects only the fixture, and its focused selection passes
220/220 under race. Final proxy artifact:
`/tmp/throughput-fix-2-proxy-budget-fixture-integration.pY4zuf`.

## Repository boundaries

Task code is committed separately in each repository. The many untracked
connect evidence directories remain intentionally uncommitted. SDK and all
platform worktrees retain unrelated pre-existing changes. Server branch
`throughput-fix-2` contains wrapper commit `7e19ae5d` and connect-runner commit
`21acdcb5`, plus fixture correction `27b7dad9`, rebased onto clean server main
revision `3a3cc698`. The SDK snapshot did not change the live SDK branch or
index.

## Remaining gates

1. Run Apple, Windows and Linux native CI. Local API/syntax/unit checks cannot
   replace their native signed-extension, MSVC-service and Linux-daemon builds.
2. Broader native TUN/H1 and actual-relay campaigns remain follow-up evidence;
   the unavailable PR #213 rig is not a prerequisite for this implementation.
3. Publish the six server-required SDK telemetry fields as a coherent SDK
   revision. The server integration validated their exact four-file patch but
   did not take ownership of the pre-existing live SDK edits.

Platform/SDK commits: SDK `0dd2943`, Android `80b4afad`, Apple `280f6678`,
Windows `960a2d9`, Linux `b97c90c`.
