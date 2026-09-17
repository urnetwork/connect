# Throughput fix 2 handoff

Date: 2026-09-17
Connect branch: `throughput-fix-2`
Current connect commits: `5cb64f2b` (`Propagate bounded network quality
remeasurement`) and `14aecd8f` (`Join P2P workers before final statistics`)

## Current result

The core approach is implemented: the learned byte window grows during ordinary
operation and does not shrink until `NetworkQualityChanged` opens a bounded
five-second remeasurement generation. Fresh qualified service and RTT evidence
must agree before that generation can replace the retained window. Adaptive
pacing continues to respond to sustained feedback without an OS notification.
A hard `NetworkChanged` also invalidates quality exactly once.

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
- Reserved internal subprotocol ids filtered from public discovery.
- P2P test-owned route joins before final asynchronous stats snapshots.
- The previously deferred path-switch models now signal quality at the switch.

ACK compression remains one cumulative head ACK plus bounded oldest-first SACKs
above the head. The head absorbs pending SACKs at or below it. Existing pacing,
maximum-message-size and deterministic compression roots remain unchanged.

## Final local validation

- 18 core quality/drain roots: `-race -count=3`, pass.
- Three programmed path-change models: pass.
- 12-cell throughput correctness matrix: pass, zero comparison failures.
- Full throughput model snapshot: pass, 858 rows.
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
`/tmp/throughput-fix-2-terra-final-v3-1789614907`. The final test-only v4 change
adds callback-storm/statistics coverage; every `ClientNetworkQuality`,
`NetworkQuality` and `WindowQuality` root passes three times under `-race`.
The v4 log is `/tmp/throughput-fix-2-terra-v4-focused-1789616840/run.log`,
SHA-256 `18acdd7a58804ac8357ef44320d2418bc6b744a1c40ec5c4cd78220b9472b9e4`.
Both runs started immediately under substantial concurrent host load.

## Server integration

Configured server integration is complete. The local services were exercised
through `server/test-env.sh`; direct compiler wrappers used the installed Xcode
toolchain without changing host state.

- `server/connect`, `server/connect/perfvar` and
  `server/connect/sim-latency` pass in 3,772.499, 1,402.138 and 35.928 seconds.
- The immutable sim-latency baseline verifier passes, and the remaining
  `resource-bomb` package passes three tests under `-race`.
- `server/proxy` passes in 316.690 seconds and `server/proxy/acceptance` passes
  in 5.958 seconds in the final official-script run.

The first connect package-local run exposed an unrestricted-discovery defect
after its real packages passed. Server commit `21acdcb5` now uses the canonical
top-level selector and covers exact subtree selection, caller arguments and
selector failure with deterministic tests. The rebased branch passes 21/21
harness race executions and the official no-test traversal selects exactly the
four intended packages. The proxy run exposed a separate pre-existing
signal-cleanup bug. Server commit `7e19ae5d` fixes that wrapper and adds nine
deterministic lifecycle roots; its rebased validation passes 27/27 race
executions. The final branch-validation artifact is
`/tmp/throughput-fix-2-terra-server-rebase-final-1789623466`. The final full
proxy log is
`/tmp/throughput-fix-2-terra-server-proxy-final-1789622033/proxy.log`, SHA-256
`3409fa1f46440b3e9eff31d935bb6baf8fcb5e7e3e0f85b2932dd11ade3ce31a`.

## Repository boundaries

Task code is committed separately in each repository. The many untracked
connect evidence directories remain intentionally uncommitted. SDK and all
platform worktrees retain unrelated pre-existing changes. Server branch
`throughput-fix-2` contains wrapper commit `7e19ae5d` and connect-runner commit
`21acdcb5`, rebased onto clean server main revision `3a3cc698`.

## Remaining gates

1. Run Apple, Windows and Linux native CI. Local API/syntax/unit checks cannot
   replace their native signed-extension, MSVC-service and Linux-daemon builds.
2. Broader native TUN/H1 and actual-relay campaigns remain follow-up evidence;
   the unavailable PR #213 rig is not a prerequisite for this implementation.

Platform/SDK commits: SDK `0dd2943`, Android `80b4afad`, Apple `280f6678`,
Windows `960a2d9`, Linux `b97c90c`.
