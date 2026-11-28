# Live receiver-prefix timing and short duplex comparison

The only production difference between the frozen arms is
`transfer_window_service_credit.go`; `source/prefix-only.patch` records it.
A cumulative head's positive receiver wait describes only that head's ingress.
If the head arrived before an earlier hole, applying its wait to the newly
credited prefix reports an impossible service rate. This remains possible
behind one known local H1 route because downstream forwarding can reorder.

The correction allows positive receiver-wait adjustment only when the newly
credited byte count equals the newly credited head's own byte count. A zero
wait keeps the raw arrival clock. Existing byte deduplication and timing
eligibility remain in force. No wire-format, window permission, burst ceiling,
SDK warmup or throughput threshold changes in this comparison.

## Deterministic contracts

Six prefix ingress definitions cover two-route and same-local-hop downstream
reorder, SACK-before-head and late-SACK orderings, ordered delivery, exact
zero-delay raw timing and a new head following an already credited prefix.
The reorder roots report 16,000,000 B/s from independent 200,000 B/s physical
inputs before the correction. The zero-delay control requires exactly
200,000 B/s, and the SACKed-prefix control preserves usable new-head timing.

The roots selection also includes eight SDK receiver-interval definitions
and five cold-refill definitions plus the existing cold queued-timing control.
These exact 20 definitions are identical
in both frozen arms. Two cold tests previously imposed a pacing upper bound
equal to measured service despite the accepted unloaded-path discovery floor.
They now require exact 267,000 B/s service and sufficient pacing; the
unsupported-gap and genuine-slowdown assertions remain unchanged.

## Exploratory SDK result

`exploratory/before.log` and `exploratory/after.log` preserve all readings from
the initial live-checkout comparison. Both provider roles of the retained
`sdk-device-h1` constructor use one bidirectional flow, 300 microseconds RTT,
a 125 MB/s serializer in each direction and the original 301.5 ms warmup.
Each candidate direction must reach at least 90% of its own ceiling reference.

| Device role | Old prefix timing: upload Mb/s | Exact-head timing: upload Mb/s |
| --- | --- | --- |
| Not providing | 814.15168, 810.63936, 811.11040 | 886.34368, 902.81984, 918.24128 |
| Providing | 839.89504, 837.73440, 851.07712 | 931.84000, 894.50496, 864.90112 |

References are about 957.2 Mb/s. All six role comparisons fail before and pass
afterward, with zero relay drops. Their temporary `go test` binaries were not
retained; they are explicitly separate from the persistent pinned comparison.

## Pinned result

| Selection, three repetitions | Before | After |
| --- | --- | --- |
| Twenty deterministic root definitions under race instrumentation | 45 passes, 15 failures | 60 passes |
| Two-role short duplex SDK definition | 0 passes, 3 failures | 2 passes, 1 failure |
| Individual SDK role comparisons | 2 passes, 4 failures | 5 passes, 1 failure |

The remaining after failure is the non-providing role in repetition three:
855.296 Mb/s upload versus 957.2352 Mb/s reference, below the unchanged 90%
gate. Its other two after uploads are 911.1552 and 910.66368 Mb/s. All three
providing-role after comparisons pass. All 24 pinned physical readings and
all failed outcomes are preserved. The correction fixes the deterministic
prefix defect and improves this SDK cell, but this archive does not establish
repeatable short-duplex acceptance. The borderline failure remains open for
root-cause analysis; no threshold, warmup or failed sample was removed.

## Pinned provenance

Source snapshots and persistent binaries are retained locally under
`/tmp/throughput-fix-2-prefix-live-pinned-v1`. Both snapshots contain the same
source and fixtures except the one production file above. The full per-file
input hashes are in each arm's `source-manifest.json`; reviewed production and
test sources are also copied under `source/` with `.txt` suffixes so this
evidence does not create additional Go packages.

| Arm | Go/fixture source SHA-256 |
| --- | --- |
| Before | `49938042f7187e7090352abb4fa64ac2dcb04f42d4c070232fb60fd37a4ff5e0` |
| After | `90606e4f81f37612a305d02a8ff44cc859005b6f7b632ade27bfce997cdf32eb` |

Terra medium built and ran the pinned pair after the core model run:

```sh
python3 /tmp/throughput-fix-2-prefix-live-run.py before
python3 /tmp/throughput-fix-2-prefix-live-run.py after
```

The archived reproduction scripts and run manifests record exact build/run commands, working
directories, race instrumentation, Go version, binary hashes, complete raw
logs, ledgers, exit statuses and host load. Each arm runs three repetitions
of the roots under `-race` and three repetitions of the affected SDK test
without race instrumentation. Both run immediately under concurrent host load
with no quiescence wait. The local `glog` replacement remains external and is
listed in the source manifests. The full core model and root regression are
separate acceptance gates; the three notified propagation-change tests remain
deferred until their `NetworkQualityChanged` calls are implemented.
