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

Only numerical ledgers, manifests, provenance and outcome excerpts are part of
the committed evidence. Raw logs and test binaries remain local. The collector
keeps raw-log hashes and copies complete comparison ledgers; it does not remove
slow readings or promote an excluded comparison to a capacity result.
