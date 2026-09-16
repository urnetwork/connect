# Window follow-up evidence

Read [the results report](../THROUGHPUT-PR2-RESULTS.md) and
[the peer review/research plan](../THROUGHPUTFIX-PR2.md) for interpretation.
Rates in model rows are virtual-time payload Mb/s. TCP rows are host-clock
payload Mb/s. Server output uses MiB/s; these units are not interchangeable.

## Validation and root-cause evidence

- [Original focused race regression outcomes](correctness/outcomes.txt), with its
  [build manifest](correctness/manifest.json) and [status](correctness/status.json).
- [Failure-before reproductions](failure-before): ACK residence/sampling,
  message limits, gap wakes, SACK overflow, admission wakeup, inner TCP loss,
  replay budget admission, TUN handoff, pacing and upload shutdown ownership.
- [Server integration trials](server-integration.txt): all six H1/H3 variants
  and all three upload/download throughput trials. Environment/configuration
  logs are excluded; only test outcomes and performance lines are retained.
- [Server unit race and relay pool-balance checks](server-checks.txt).
- [Adjacent root-cause audit](../THROUGHPUTFIX-PR2.md#12-adjacent-root-cause-review-and-deterministic-completion-gate): known-limit fallbacks, changing capacities, idle gaps, duplicate sampling paths, overflow and local pacing in RTT. Nine added deterministic cases are included in the focused race selection.
- [Model run before the adjacent review](model-before-adjacent-review/provenance.json): failed immediate-ACK mismatch cells, with every paired reading retained.

## Pilot ledger index

Every extracted reading and comparison is retained in its run's `ledger.jsonl`.
`provenance.json` records the source-log hash, result, fixture stage and row
count. Runs made before the manifest-producing script have no source/binary
manifest; their role is diagnosis, not final capacity evidence.

| Directory | What it establishes or exposed |
|---|---|
| `packet-pilot-1`, `packet-pilot-2`, `packet-pilot-3` | Early host-clock FIFO calibration; includes a failed pilot. |
| `model-pilot` | Failed model cells before pacing/warmup corrections. |
| `model-before-adaptive-pacing` | The 36-cell matrix with the target-rate pacer; the later slow-link test exposed the missing service-rate control. |
| `tcp-pilot` | Initial upload stalls before the pure-ACK TUN handoff fix. |
| `tcp-handoff` | Handoff corrected; the per-packet NAT fixture still refused admissions. |
| `tcp-batched` | Batch-preserving NAT admission; short samples still included startup. |
| `tcp-jitter` | Bounded timer-lateness correction. |
| `tcp-pacing-control` | Three-second pacing on/off comparison at 100 ms/eight flows. |
| `tcp-settling` | Ten-second comparison exposing the inner TCP startup ramp in interval readings. |
| `tcp-shutdown-failure` | All 64 measured arms completed; teardown failed with 123 pooled roots outstanding. Preserved as **failed validation**. |

A/A drift over 10%, a ceiling below 90% of configured link rate, missing
controls and stalled flows are retained as censor reasons. A process pass
does not turn a censored comparison into proof of an optimum. In particular,
the unbudgeted default gVisor buffer range limits the original 100 ms
single-flow instrument.

Reproduce with [tools/throughput-fix-2.sh](../tools/throughput-fix-2.sh).
It checks that Go/protobuf/module sources stay unchanged during compilation,
records the binary hash and safe experiment environment, and preserves every
reading and the test process's exit status.
The source digest also includes the embedded SDK profile fixture.

## Burst and statistics follow-up

The report separates each source checkpoint; a result from an earlier binary
does not validate later pacing edits. `correctness-burst`, `model-burst`,
`regression-burst` and `tcp-short-burst` retain the first explicit-burst checkpoint,
including its failed compression-ablation control. The `*-burst-ring` campaign
retains the later checkpoint, including failed comparisons and the obsolete
send-item size assertion. Its test-only correction explicitly adds the new
eight-byte burst identity to the exact size guard.

The [failure-before index](failure-before/README.md) maps new deterministic
reproductions to their failed outcomes and current test names. Final confirmation
results and source boundaries belong in the report, including any later fix
for reordered samples after a ring reset.

The completed `*-burst-ring-final-service-epoch` archives retain the full
`b5b40736` campaign: passing correctness, model and root regression, plus failed
host comparisons with control exclusions. The subsequent `controlled-epoch-final`
archive records the natural-drain correction, its deterministic failures and
nine passing focused model pairs. Its adaptation diagnostics remain separate
from settled-throughput acceptance. `correctness-burst-ring-final-controlled-epoch`
passes all 156 tests under the race detector on source `f60cf11d`.
The same source's `model-burst-ring-final-controlled-epoch` archive now retains
21 passing tests, 272 paired cases and all 508 ledger rows. These runs started
immediately under recorded concurrent host work.

`correctness-burst-ring-rtt-resize` passes 160 race-enabled tests on source
`56eec7b1`, after preserving delivery checkpoints across bucket-duration changes.
Its `model-burst-ring-rtt-resize` full run completes 21 passing tests and one
slow shared-service failure, with all 510 readings retained. The preceding
passing model does not supersede this later failed result.
`rtt-resize-evidence` retains the direct and intermediate failures, 93 focused
race passes, 23 passing model pairs and the complete recovery comparison rows.

`host-feedback-controlled-epoch` retains eight induced host readings and a
three-run deterministic failure through the real inner-TCP replay worker.
The original synthetic test source retains its `.go.txt` extension; the corrected
normal regression now lives in `transfer_window_host_feedback_test.go`.
These single-arm diagnostics are not A/A-bracketed acceptance comparisons;
their controls, scheduling limits and failed outcomes remain visible.

`correctness-source-idle` passes 169 race-enabled tests on source `5605efa7`.
`source-idle-evidence` retains 18 before failures, 276 passing focused race
executions and 50 scoped model readings. The replay correction preserves the
previous service across a proven source pause; fresh slower evidence still
replaces that hold. Full model and host acceptance of this source remain open.
`source-idle-followup` retains the complete before/after host brackets and exact
SDK duplex repeats on separately pinned overlay binaries. Both host brackets
pass but record zero sampled NAT replay events, so they do not prove repair of
the replay root. One of three SDK repeats still fails after the correction.

`sdk-settings-current` records eight constructor profiles through the sibling
SDK's real sizing helpers. Its manifests pin SDK `7fe75c69` and connect source
`5f28f158`; the capture includes a newer recovery test and is not represented as
the `f60cf11d` model source. It verifies resolved settings on the host, including
selected mobile policy, without making a mobile-runtime performance claim.

`sdk-transfer-profiles` expands the capture to 11 profiles and 40 fields,
including explicit mobile H1 queues and lanes. It pins SDK `7fe75c69` and
connect source `56eec7b1`. The embedded test fixture carries its own digest.
`sdk-transfer-model-first` retains all 300 readings from 150 paired Transfer
cases on source `3242678e`: the one-way test passes and the bidirectional test
fails in the mobile H1 provider's return direction. This run precedes the
source-idle correction and stronger per-direction/calibration checks; it does
not validate those later edits or physical H1/TUN behavior.

Only numerical ledgers, manifests, provenance and outcome excerpts are part of
the committed evidence. Raw logs and test binaries remain local. The collector
keeps raw-log hashes and copies complete comparison ledgers; it does not remove
slow readings or promote an excluded comparison to a capacity result.
