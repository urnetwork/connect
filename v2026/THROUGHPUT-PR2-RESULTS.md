# Window follow-up: implementation and local results

Branch: `throughput-fix-2`; latest production commit `f8261152`.
Date: 2026-09-16 through 2026-09-17. The delivery-sized window remains enabled.

Final evidence is summarized in [THROUGHPUT-REPORT-PR2.md](THROUGHPUT-REPORT-PR2.md).
The deterministic and host-local runs completed. The completion audit found
that host process success checked only nonzero progress and did not reject
slow candidates. Host performance acceptance remains open under the corrected
comparison gate; see the final report for all final-source rate ranges.
The configured local server integration is complete. Every legitimate
`server/connect` package selected by the repository harness passed, as did the
baseline verifier. The full `server/proxy` package and its acceptance package
pass after fixing a pre-existing signal-cleanup regression in the acceptance
wrapper and an incomplete asynchronous test fixture. The final complete log is
diagnostically clean. Both runs used the checked-in environment owner and ran
immediately under recorded host load.

The independent tests confirm several mechanisms that the published report
could not separate: missing ACK residence in the window calculation, loss
after Transfer delivery, ACK overflow/order/cadence defects, and loss when a
deep H1 window reaches a finite relay queue. An additional upload experiment
exposed a TUN endpoint-lock cycle. These are local reproductions; the missing
native rig and remote packet traces prevent attributing every published
failure to a particular one of these mechanisms.

The [peer review and research plan](THROUGHPUTFIX-PR2.md) preserves the source
review, hypotheses, evidence limits and server follow-up work.

### Final shared-service and provider-ACK closeout

The final adjacent review found two scope/ownership defects. Lane-local
cumulative delivery could underprice the one service pacer shared by two or
four active H1 lanes after a quality reset. Separately, the provider's
per-flow compressor generated a 52-byte pure TCP ACK but sent it through a
shared zero-wait callback; both H1 admission slots being occupied discarded
the control before a released slot could accept it.

Commit `f8261152` adds a six-entry service-owned raw-delivery history as a cold
pacing fallback. It counts confirmed H1 credit once across lanes, preserves
exact offer/arrival endpoints and never replaces a positive serialization
rate or a lane's logical window history. The provider ACK worker now receives
bounded, cancellable per-flow admission ownership while public and shared
synthesized controls keep zero-wait refusal. A newer cumulative ACK covers
progress that arrives during the wait.

The shared-delivery/quality selection passes 294/294 race executions. The five
pure-ACK roots pass 50/50, with 18/18 adjacent provider recovery executions.
The canonical clean-commit correctness gate passes 566 race-enabled tests
with no failure, skip or diagnostic. The six-entry structure is 360 bytes per
service, versus 3,608 bytes for the 64-entry counterfactual; all benchmark
operations allocate zero bytes and the smaller ring is faster for publication,
estimation and rebucketing. Exact artifacts and hashes are in the final report.

The final immutable `f8261152` campaign also passes all 858 model rows, all 12
broad-regression rows, 43 pacing benchmark rows and the calibrated physical H1
comparison. Every manifest records the clean commit and source digest
`a22399024dd9b3547d0e82a8a6e217a2718ce5171f2bd82a8762dc9681bec850`.

### Final network-quality phase

Ordinary qualified feedback can grow the learned byte window but cannot shrink
it. `NetworkQualityChanged` opens one bounded five-second generation in which a
fresh service and RTT pair may replace it. Adaptive pacing continues between
signals. Repeated local callbacks coalesce until five seconds of listener
quiet, and exact or older provider generations are discarded. A deterministic
storm of 128 callbacks overlaps 32 public statistics reads and applies exactly
one generation. Ten final race repetitions also prove valid snapshots, one
post-quiet generation and no panic, recovered error or race diagnostic.

The frozen combined source passes all three changed-path models, the 12-row
correctness ledger, the 858-row full model and broad regression. The v3 archive
is `/tmp/throughput-fix-2-terra-final-v3-1789614907`; the test-only callback
addition passes in `/tmp/throughput-fix-2-terra-v4-focused-1789616840/run.log`
with SHA-256
`18acdd7a58804ac8357ef44320d2418bc6b744a1c40ec5c4cd78220b9472b9e4`.
Both were run immediately under recorded concurrent host load.

The public signal propagates through the SDK and a reliable, source-scoped,
non-echoed 24-byte peer message. Android, Apple, Windows and Linux call it for
the native quality observations available on each host. ACK compression keeps
its original contract: one cumulative head, bounded oldest-first SACKs above
the head, absorption at or below the head, and a fixed maximum carrier size.

### Earlier ACK and statistics follow-up

Commit `3ce605cb`, source `538e6248fb744795d9e829e97e503744ed70284d107ebad10da646b56bf6e460`,
passes 177 race-enabled correctness tests. Eight new tests fail 24 times before
the correction: incomplete old ACK trains cannot reprice sibling pacing, fresh
and completed evidence still replace the hold, and public statistics cannot
change rates captured by later probes. The combined focused selection passes
321 executions; 37 targeted model pairs pass. The complete model then passes
24 tests and 423 pairs, retaining all 810 rows. Separately forced SDK recovery
failures remain open.

The preceding `5605efa7` source records a full 150-pair SDK test pass, but later
review finds an unlimited serializer in its nine reversed duplex pairs. Those
pairs cannot establish capacity; separately reproduced failures remain open. Its root regression
retains 3,011 passes, two failures and 24 skips. One failure read edited runtime
source; the other exposed a miscalibrated real-clock serializer. Source snapshots
and a calibrated virtual-time short-path test address those harness defects.
The corrected `7afd9e4b` test-source checkpoint passes 3,021 root tests with
zero failures and 24 skips, and all 177 race-enabled correctness tests.
Production is unchanged from `538e6248`.

An induced host experiment now forces a real NAT replay in all eight arms.
Without the source-idle delimiter, the candidate's service estimate falls from
116,086,811 to 19,038 B/s; the corrected arm preserves its preceding rate.
Both throughput brackets pass, so this confirms the estimator effect without
establishing a throughput gain. The legacy packet batching also exceeds actual
H1's message cap in a separate physical-carrier test; these FIFO results do not
validate physical H1. Evidence remains in `host-replay-confirmation`.

The first ACK-tail candidate improves forced SDK duplex recovery but fails
the real 100 Mb/s RTT-growth control at 6.8608 versus 95.8054 Mb/s. Its complete
failed evidence is retained. The reviewed combined ACK-tail/cycle correction
on `914a2a72` now passes 393 focused and 393 race executions, nine recovery
executions, six service controls and 18 SDK phase/control executions. It holds
incomplete feedback, allows new bytes across the full gap to raise a low startup
estimate, and requires completion before lowering it. Full combined correctness
records 213 race passes and one eight-byte record-growth failure. Directly
passing delivery bytes into the compressor now restores the original record
layout and exact-size assertion; all 40 selected ACK/size tests pass under race.
The full model finishes with 21 passes, five failing tests and all 814 readings
retained. Adjacent review reproduces a same-timestamp partition defect beyond
the ring. Its bounded v11 summary correction is now applied: five new roots,
408 focused and 408 race executions pass. Three additional permanent drain and
retry tests each fail three times before correction, including the real worker
retrying a drained probe before its long-RTT ACK. The isolated candidate passes
those roots but still fails the long-path performance control. Small-window
sampling, long-path recovery and combined performance acceptance remain open.

The next implementation uses receiver ACK timing to replace inference where
possible. Optional actual receiver delay is now present in the codec; receiver
stamping and adjusted RTT consumption remain under development. Sender-only
rate/drain candidates are frozen for comparison. The existing one-head,
bounded oldest-first SACK contract and original performance gates still apply;
see research-plan section 28 for timing, compatibility and completion checks.

The compression-residence control now holds both arms to their advertised
compression interval, preserving its original residence and throughput gates.
Three race executions pass; all six readings remain in
`compression-residence-isolated-control`. Reversed duplex tests had also removed
one direction's serializer. The failure-before test measures about 805 Mb/s on
a declared 100 Mb/s link in all three attempts. The fixture now preserves both
serializers and checks each SDK direction against the physical rate. Bounded
orientation/rate-change validation passes six race executions and retains all
24 readings in `duplex-fixture-bounds-after`. Historical affected rows
remain available with this calibration limitation.

### Physical H1 fixture checkpoint

Test-source `b6abfa4e` adds actual TLS/WebSocket H1 coverage around the common
TCP workload. The helper now admits logical packet groups, keeping a socket
batch below H1's fixed 8 KiB message cap, and passes the workload cancellation
context to a waiting send. Both defects fail three times before correction;
the three new ownership/cap/cancellation tests and one existing NAT lifecycle
test pass 12 focused race executions on a verified source copy.

The ordinary `physical-h1` runner passes its 100 Mb/s, 0.3 ms added-RTT,
one-flow download A/B/A: 91.550/91.469/91.549 Mb/s, with no recorded refusals and
balanced replay, Transfer and carrier budgets. Full numerical readings remain
in `physical-h1-smoke`. This uses an owned TCP origin, userspace gVisor TUN and
an unauthenticated local relay with Transfer encryption disabled. Production
is unchanged from `538e6248`; broader physical and native-TUN acceptance remains
open. The rejected ACK-tail candidate's complete diagnostic evidence remains
in `sdk-ack-tail-v3-evidence`.
See the report for failure-before evidence, source boundaries and remaining work.

The corrected generic TCP matrix on `b6abfa4e` retains 32 readings and eight
comparisons: six accepted, two excluded by reference calibration, no failed
candidate comparisons. The exclusions are one-flow, 100 ms cells in each
direction. A separate 48 MiB TCP-buffer control, measured for 12 seconds per
arm instead of three, accepts both configured cells at 916.489 Mb/s download
and 908.583 Mb/s upload. Its eight readings and two comparisons are retained
separately; it does not replace the default-buffer exclusions or isolate the
effect of the buffer change from measurement duration. Both campaigns ran
under recorded concurrent work using the generic FIFO/userspace-TUN fixture.

The first physical duplex extension does not calibrate: both three-arm runs
fail and are excluded by their controls, including stalled reference arms.
All six readings and an earlier cleanup interruption are retained in
`physical-h1-duplex-diagnostics`. A fresh-process reference also exposes a TUN
endpoint-lock dependency involving data carrying ACKs, extending the earlier
pure-ACK case. Source `c07140f7` now removes that redundant endpoint lock after
synchronous packet injection. Both root tests fail three times before the fix;
19 corrected tests pass three times under the race detector. A real finite
2,000-byte TCP tail is acknowledged before the checking `Read`, without a later
payload or replay. All 182 correctness tests pass under race. Physical duplex
completes but remains failed/excluded by reference calibration, reference drift
and provider control-return refusals; all three readings are retained in
`physical-h1-duplex-tun-fix`. The copied-source root regression finishes with
3,026 passes, zero failures and 25 skips. These results do
not establish throughput acceptance.

### Burst follow-up checkpoint

The controlled-drain correction passes 156 race-enabled correctness tests
and the full model (21 tests, 272 pairs, 508 ledger rows) on source SHA-256
`f60cf11db4f0c7a928144ad78d1b9cebe96bbf8916fa620d1b58d01598fc613a`.
It preserves serialization evidence across natural drains and fixes the settled
64 KiB capacity increase from 1.094 to 10.000 Mb/s. A subsequent recovery test
exposed lost delivery checkpoints during RTT-driven bucket resizing. The new
correction preserves those timestamps and first-arrival bytes; the fixed
10–14 s recovery interval rises from 7.733248 to 10.092544 Mb/s against the
9.961472 Mb/s reference. Its source and validation are separate from the
completed `f60cf11d` model. Recovery-time differences and host acceptance remain
open; see the report for retained failures and deterministic tests.
The rebucketing source `56eec7b1968bac2214224b2d7975f849d0fe6efef53cd9322eb11a01b31bc6ef`
passes 160 correctness tests under the race detector and 23 targeted performance
pairs across seven tests. Its subsequent full model completes 21 passes and
one failure: slow shared-service throughput is 0.82944 versus 0.94720 Mb/s.
All 510 readings remain in `model-burst-ring-rtt-resize`.

The source-idle correction now passes 169 race-enabled correctness tests on
`5605efa750834abb352be42548f7c20cd55f56d154a2837dc5996e8b580180f7`.
Its normal regression runs the real `TcpSequence.runReturnRecovery` worker:
the resumed bulk write's virtual wait falls from 18.430482186 s to zero while
service stays at 125 MB/s. Nine tests cover the root and adjacent confirmation,
cancellation, queued demand and shared-lane boundaries. Six failure cases fail
three times before the fix; 92 focused tests pass three times afterwards.
Seven scoped model controls retain 50 readings. These passes do not resolve
the separate shared-service failure or establish host acceptance.

The SDK probe now captures 11 actual constructor profiles with 40 numerical
or policy fields. The new Transfer matrix completes 150 paired cases: all
132 one-way pairs pass, while the bidirectional test exposes the mobile H1
provider's return-direction regression (428.41088 versus 587.20256 Mb/s, zero
recorded drops). Its 300 readings are retained in `sdk-transfer-model-first`.
That run predates the source-idle fix and stronger per-direction/calibration
checks. The subsequent `sdk-transfer-source-idle` run records all 150 pairs
passing once; its nine reversed duplex pairs later prove miscalibrated because
one direction was unlimited. Exact-cell failures and physical SDK/H1 coverage
remain open.

The committed follow-up's final correctness run passes 152 tests under `-race`
on source SHA-256
`b5b407364a99cbb0a022b5de897fc5eb8bade0593541ddd2e0b658636c692207`.
Its full model passes 20 top-level tests and 268 paired cases, retaining all
500 ledger rows. Both host configurations completed with failed comparisons
that also have control exclusions; root regression passes 2,996 tests with
24 skips and no failures. The report
distinguishes these results from earlier checkpoints; host acceptance remains
open.

The results below describe the original committed implementation. A later
working-tree checkpoint, source SHA-256
`38dca2f68b2050ac1a5807599980157e1661842c6497d936896d456bfb3b01d5`, completed:

| Selection | Outcome |
|---|---|
| Focused correctness, race detector | 126 passes. |
| Root regression, non-race | 2,971 passes, 24 skips, no failures. |
| Deterministic performance model | 16 top-level passes, one failed compression-ablation control. |
| Short-path host TCP uploads | Six comparisons passed; no exclusions. |

These runs started under concurrent host work and are archived in
`throughput-fix-2-results/{correctness-burst,regression-burst,model-burst,tcp-short-burst}`.
They precede newer byte/flight refinements and do not validate those changes.
The ablation now pins both arms to the known FIFO propagation time so that
compression observed in service RTT cannot restore the omitted window term.
The corrected focused comparison passes at 202.8 versus 958.3 model Mb/s.
The subsequent burst-ring candidate fixes the continuous-flight RTT and slow
shared-service reproductions. After an adjacent correction for reordered
samples following a ring reset, source SHA-256
`fab6a0ab0f065bf4dd6a04d71976cdb5e4d5a4475508ebfa97601f06533a3d82`
passes all 147 tests in the full race-enabled correctness selection, including
81 pacing/statistics tests. Focused models pass all four RTT-change pairs,
all three shared-service pairs and four large-message capacity-change pairs. Broader
validation and retained failure-before evidence are tracked in
[the report](THROUGHPUT-REPORT-PR2.md#deterministic-tests-for-the-new-failure-cases).

## Changes and failure-before evidence

| Change | Regression evidence |
|---|---|
| Include receiver ACK compression residence in both window terms | The target/delivery tests failed with the original RTT-only calculation. A 0.3 ms FIFO with 10 ms compression delivered **202.5 model Mb/s before, 958.2 after**. Raw RTT remains the basis for resend timers. |
| Advertise optional compression delay | Presence tests distinguish an old peer (10 ms), explicit zero, replacement values, and an absent field after an explicit advertisement. Native/legacy protobuf and encrypted-carrier tests include the added field. |
| Repair delivery sampling | Tests reject checkpoint byte/time mismatches, insufficient history spans and a 64-entry history too short for 400 ms. A large wire-valid compression value also checks overflow-safe multiplication. |
| Respect mismatched endpoint limits | New deterministic tests reproduced a 256 KiB working floor overriding 64/32 KiB advertisements. The effective floor now respects explicit peer and deployment limits before and after sampling. Live capacity changes and both mismatch directions have paired performance coverage. |
| Factor and bound ACK compression | Direct struct tests cover one newest head, ascending SACKs strictly above it, absorption when the head advances, ordered overflow, and an attempted count-limit bypass. Worker tests cover actual encoded response sizes, compression deadlines, early gap wakes, metadata on the cumulative head after SACK overflow, and final cancellation drain. |
| Bound eviction metadata | Before the fix, 4,096 maximal sequence numbers produced a **41,212-byte** carrier. Overflow now remains pending and each ACK carries at most 512 notices. Maximum-field tests cover both codecs, encryption and the 8 KiB minimum carrier contract. |
| Preserve gap proof across compression turns | Exact virtual-time tests reproduce the missing initial-head repair, repeated proof of an unchanged head, and evidence split across snapshots. The unchanged head gets one early wake; normal compression resumes afterward. |
| Notify send admission when capacity returns | The old 2 ms poll failed a virtual-time immediate-release test. The generation notification wakes admission without an artificial poll delay. |
| Recover inner TCP after Transfer success | Before the fix, exact middle/tail/wrap/EOF loss cases failed. The provider now retains bounded origin chunks through cumulative inner TCP ACK and replays the oldest missing segment. Tests verify exact bytes, healthy traffic without replay, IPv6, budget contention, cancellation and already-ACKed read-ahead. A further failure-before test prevents an unreserved replay copy while waiting for shared capacity. |
| Pace H1 window bursts using measured service | Target-only pacing collapsed a 100 Mb/s, 100 ms link to **9.5 model Mb/s**. Whole-window average pacing then underfilled 200/400 ms paths. The current implementation shares one bounded initial probe and pacing clock across logical sequences using the same service. It samples first-delivered wire bytes at ACK arrival and probes spare capacity only when the service is not backlogged. The detailed controls and final validation are described below. |
| Avoid the upload ACK handoff lock cycle | Both `Tun.Write` and `Tun.WriteBatch` failed while a test held the real gVisor endpoint lock. Pure ACKs now avoid that data handoff lock. The classification tests retain data/SYN/FIN/RST handling and cover both IP families, options and ECN. |
| Drain upload ownership after the producer stops | A completed TCP matrix failed teardown with 123 pooled roots outstanding. A virtual-time test then reproduced publication after the socket writer had already drained an empty queue. The producer now closes that queue on every exit and the writer drains through close before `Run` joins it. |
| Release standby after pinned-address failure | Adopted the PR's standby change and added wrapped-error, changed-evidence, H3, successful-reconnect and connected-pin controls. |

### ACK compression contract

`sequenceAckWindow`, in [transfer_ack_compression.go](transfer_ack_compression.go),
owns the independently testable coalescing state. The worker owns timers and
wire emission. Its response begins with at most one cumulative head, followed
by bounded, oldest-first SACKs above that head. A newer head absorbs pending
SACKs at or below it. Contract-recovery requests remain separate evidence.

Every response reserves at most **8 KiB and 32 entries**. The fixed per-entry
reservation is 320 bytes; eviction metadata reduces the usable entry count
(25 entries when no eviction metadata is present). Overflow drains in bounded
responses without adding another compression interval per SACK chunk. New
bursts wait for the normal deadline, apart from the tested gap-proof wake.
If only eviction metadata remains, another carrier waits for the next normal
deadline and repeats the last cumulative head, even when the prior bounded
turn ended with a SACK. The regression
reproduced eight immediate head carriers before this correction. Terminal
cancellation drains already-owned evidence in bounded responses before joining.
The tests check serialized bytes as well as counts, using maximal varints and
encrypted wrappers rather than assuming fresh, small sequence numbers.

## Instruments and performance interpretation

The raw instrument connects two actual Transfer clients through a finite FIFO
that serializes messages at 1 Gb/s and models propagation separately from
queued bytes. It has one serialization worker per direction, avoiding one
goroutine/timer per packet. Virtual time makes its results reproducible.

The TCP instrument adds a **real loopback kernel socket origin**, provider
`LocalUserNat`, and gVisor TUN/application sockets. It preserves packet batches
at the NAT boundary and counts rejected admissions. Production
`RemoteUserNatProvider.ClientReceive` also groups allowed packets into flows
before NAT admission; the fixture bypasses its policy/control overhead. This
is an isolated TCP data-path instrument. It does not include native kernel
TUN, a live WebSocket server relay, WAN conditions or SDK orchestration.

All arms share their packet shape and path settings. `matched` keeps the
2 MiB constant window with the same send/receive budgets and advertisement;
`constant` additionally restores historical unbudgeted/no-advertisement
settings. `ceiling` uses a 48 MiB constant window. `path-rtt-only` isolates the
old ACK-residence assumption, and `unpaced` isolates H1 pacing. `delivery`
includes the fixes. Small private test seams configure these before workers
start.

Each host comparison brackets the candidate with matched A/A runs. The JSON
ledger retains every arm and comparison; it marks A/A drift above 10%, a
ceiling below 90% of configured link capacity, and stalls. These flags limit
interpretation and do not discard failed runs. Three-second, three-repetition
final confirmation cells are regression evidence, not precise estimates of
rare stall rates.

### Deterministic matrix and service-pacing controls

The final deterministic model selection passed **213 paired cells**, with
**390 ledger rows** and no relay drops in any delivery arm. Earlier target-rate
pacing passed the 36-cell RTT matrix but failed a slower-service control.
Subsequent experiments exposed long-RTT underfilling, queue loss and mismatch
failures. Those failed experiments remain part of the evidence.

| Model matrix | Cells | Lowest candidate/reference throughput |
|---|---:|---:|
| Original RTT/flow/compression matrix | 36 | 99.9% |
| 1/10/100 Mb/s service rates | 54 | 97.9% (above the 90% acceptance threshold) |
| Service capacity changes | 6 | 100.0% |
| Four sequences sharing one service | 3 | 99.9% |
| Ordered endpoint window sizes | 108 | 99.3% |
| Live receiver-window changes | 6 | 98.8% |

These are measured virtual-FIFO ratios, including ordinary interval-edge
effects, rather than estimates of remote throughput. The complete ledger and
source/binary manifest are in `throughput-fix-2-results/model-final`.

The current model selection includes the original **36 RTT/flow/compression
cells**, **54 cells at 1/10/100 Mb/s**, six 10↔100 Mb/s capacity changes, and
three shared-service rates with four logical sequences and eight flows.
It also includes **108 ordered window-mismatch cells** and six live receive
window changes. Sender/receiver capacities of 256 KiB, 2 MiB and 48 MiB are
crossed with 0.3/100/400 ms RTT, 0/10 ms compression and one/eight flows.
The live receiver changes cover 64 KiB ↔ 2 MiB with 0/10/50 ms compression.
Each mismatch reference is capped to the smaller endpoint; the test checks its
calibration as well as candidate throughput, both bounds, per-flow progress,
steady-state relay loss and receive-queue eviction. Window-limited intervals
span twenty residences to bound measurement-edge effects. These fixtures offer
flows round-robin within each logical sequence, with separate lane producers.
The FIFO itself has deterministic tests for serialization, propagation, queue
bounds, and rate changes while frames are queued. Shared-service calibration
uses two BDPs per sequence, with a 256 KiB floor, instead of injecting minutes
of low-rate traffic into the large-window control. Every calibration setting
and the slow-link opening-train warmup are recorded in the ledger.

The service unit tests check every send prefix against the byte/time envelope,
rate increases and decreases, bounded idle credit, one shared probe, split
probe boundaries, cancellation, sibling isolation, and shutdown ownership.
They also cover these additional root causes:

- A canceled short write was admitted when it did not need a timer.
- A paced resend batch delayed application of newly arrived ACKs. The send
  loop now applies them between recovery writes.
- Requiring a whole initial window before trusting service allowed a slow
  relay to fill before the sender used valid delivery evidence.
- A slow H1 FIFO exhausted the two-deferral limit while still delivering.
  Extra deferrals now require fresh cumulative progress on an H1-only paced
  path; silence, mixed paths and unreliable delivery keep ordinary recovery.
- ACK processing time could double an otherwise steady service measurement.
  A local arrival timestamp survives handoff and coalescing.
- A head repairing a gap could count an already-SACKed suffix as new service.
  First-delivery accounting is independent of the retained-window total and
  of recovery's resettable SACK flag.
- Growing queue RTT extended old fast-rate evidence. Fresh samples now replace
  it within a feedback interval based on the service's stable minimum RTT.
  A four-bucket replacement limit was rejected: it forgot a correct 125 MB/s
  observation before faster sends could return ACKs over the 400 ms path.
- Independently compressed sibling ACKs inflated 12.5 MB/s of service to
  **781.25 MB/s**. Arrival buckets accept out-of-order worker application, and
  each measured rate covers at least one advertised compression interval.
- A receiver at its exact gap deadline repeatedly armed a zero timer. Expiry
  now includes equality and releases the held items at that deadline.
- Short opening trains could lose their first checkpoint at bucket boundaries,
  or fit entirely inside one bucket. A deterministic fast-train control read
  **4.26 MB/s instead of 125 MB/s** before preserving its endpoints. Small-window
  matrix cells exposed the same issue after a long idle gap.
- A proposed compressed-tail extrapolation was rejected: reusing the prior
  reply's bytes with the next reply's spacing doubled a controlled slow rate
  from **125,000 to 250,000 B/s**. Partial tails remain conservative observations;
  variable-sized replies, shared phases and replacement of old evidence are
  tested explicitly.
- Large flight alone was mistaken for a standing queue, stopping discovery on
  a long fast path. Before a complete service residence has been delivered,
  the stop condition also requires excess observed ACK residence.
- Compressed peaks could preserve a full shared queue. Queue evidence switches
  the estimate to sustained delivery over a feedback interval and multiple
  compression turns; pacing then leaves five percent of that capacity to drain
  it. Four deterministic byte/time controls verify actual drain capacity.

The opening probe covers up to two compressed reply intervals, bounded by
twice the ordinary initial window (at most 4 MiB with the default 2 MiB
initial). This remains one shared allowance and never replenishes on idle.
Ordinary discovery permits ten percent above measured service; queued service
uses a five percent drain margin. All service cells now also require zero
relay drops during their measured interval, while retaining startup and
transition drops separately.

Pacing counts the first actual wire envelope, including encryption overhead,
and credits it once. Cancellation releases a sequence's remaining shared
backlog. Service state is reference-counted with sequence lifetimes. The
ordinary per-sequence window still owns its existing memory/flight limit.
All model rates are application payload rates in a virtual FIFO, not host
or remote H1 measurements.

### Adjacent root causes

Following `CODESTYLE.md`, the review traced each root cause through sibling
call paths. The [review matrix](THROUGHPUTFIX-PR2.md#12-adjacent-root-cause-review-and-deterministic-completion-gate)
maps all earlier findings to their deterministic coverage.
`transfer_window_adjacent_test.go` adds these reproductions and boundary controls:

| Case | Before | Corrected behavior |
|---|---|---|
| Unbudgeted sender with a smaller known limit | Held 2 MiB despite 64/32 KiB peer/local limits | Holds the constant only within known limits |
| Later receive-capacity increase | Old delivery immediately reduced the new 2 MiB allowance to 256 KiB | Restarts the delivery evidence boundary on increases; repeated unchanged advertisements still converge |
| Window-limited idle inside a service interval | Read 31.5625 MB/s from a 125 MB/s active train | Sustained queue estimation requires continuous delivery across its interval |
| Standalone delivery history | Forgot 125 MB/s before feedback could return | Retains evidence for the stable feedback span and expires it afterward |
| Contract announcement multiplication | Overflow returned zero instead of 2 MiB | Uses wide multiplication and saturates the scaled result |
| Local pacing inside service RTT | A 10 ms pacing wait made a 100 ms path appear to take 110 ms | Samples first actual write through ACK arrival, excluding worker delay and ambiguous carrier/retransmission evidence |
| Zero advertisement | Previously inherited the 256 KiB working floor | Preserves only the queue's existing one-message progress allowance |

The large-target boundary control already passed; it is not counted as a
reproduced defect. The actual-write timestamp adds eight bytes per retained
send item, bringing the tested size to **576 bytes**. ACK arrival timestamps
remain local state and do not change the wire protocol.

The focused correctness selection, including these adjacent cases, passed
under the race detector (**90 top-level tests**). The final model selection
passed **213 paired cells** (**390 ledger rows**), including all **108 mismatch
cells** and **six live receiver-capacity changes**. The lowest model
candidate/reference ratio was **97.9%**; measured relay drops and receive
evictions were zero. Two earlier host-sensitive regression failures
(experimental lane recovery and a short-path throughput comparison) passed
when isolated; their failed full-sweep readings remain in the evidence.

### TCP startup and fixture limits

The first TCP upload pilot correctly reported stalled one/eight-flow cells
before the TUN ACK fix. They remain failures in the diagnostic record. After
the fix, short two-second samples still averaged below the large-window
ceiling. A ten-second instrument check exposed the ramp directly: the 100 ms,
eight-flow delivery arm read **626.2 Mb/s in its first measured second**, then
**939–942 Mb/s** for the remaining nine. Its ten-second mean was 908.9 Mb/s.
The ceiling also ramped, but earlier. The updated fixture adds two seconds of
TCP warmup, records its duration, and publishes one-second interval rates.
This changes the steady-state measurement boundary; it does not erase the
startup difference or justify a claim about first-byte/finite-transfer latency.

The unbudgeted default gVisor buffer range used here limits the 100 ms **single-flow** TCP cell to
roughly 161–185 Mb/s in healthy delivery and ceiling arms. A final candidate
upload also fell to 90.5 Mb/s and is retained as a regression to investigate,
not explained away by that limit. The ceiling is reported as an
instrument-capacity limit, with the window rule's effect unresolved above it.
Larger TCP buffers are a separately labeled capacity control.
The optional `CONNECT_WINDOW_TCP_BUFFER_MAX_MIB` control records an explicit
per-connection TCP maximum in every cell. A 48 MiB maximum matches the TUN
ceiling derived from a 384 MiB process budget, while keeping the Transfer
comparison's 48 MiB budget fixed.

The final confirmation ledgers are in `throughput-fix-2-results/tcp-final`,
`throughput-fix-2-results/tcp-capacity` and `throughput-fix-2-results/ack-final`.

## Server regression review

The review includes `../server/connect` at
`77201554c49ec05bde83ec038bba6c600972892c`, compiled against this branch using
its existing local module replacement. Server unit tests pass under the race
detector for frame limits, carrier compatibility, reliable receive pressure,
resident ingress retirement, H1 bounded batching/order/ownership and H3 ACK
reserve configuration.

At that review checkpoint, the sibling checkout also contained independently
edited resident lifecycle changes. The regression review included those
working-tree changes: lazy
forward-shard allocation, worker registration while a producer is admitted,
and a final drain after callback owners join. Their three new deterministic
tests passed in the expanded server race selection. The build manifest records
that checkpoint's dirty source hash. The later configured integration and
harness fixes are isolated on the server `throughput-fix-2` branch below.

There are two distinct server pressure boundaries: **256 callback-ingress
entries per destination shard**, which retire a full resident generation, and
**4,096 downstream forward entries**, which drop on full at production's
zero timeout. ACKs share those paths with data. Our finite FIFO reproduces
the latter mechanism; it does not prove which boundary failed on the remote
relay. The [server review](THROUGHPUTFIX-PR2.md#9-serverconnect-regression-review)
specifies the missing collision, reconnect and actual-relay experiments.

The deterministic race-enabled `server/connect` selection passed. The final
configured run used `server/connect/test.sh` and its `server/test-env.sh`
environment against the running local PostgreSQL and Redis services. The real
packages all passed: `server/connect` in 3,772.499 seconds,
`server/connect/perfvar` in 1,402.138 seconds and
`server/connect/sim-latency` in 35.928 seconds. This includes the H1/H3,
pool-balance, directional TCP and database-backed cases that the earlier
preflight could not start.

The first package-local script run then returned 1 because its unrestricted `find`
entered `sim-latency/baseline/v1/independent-references/validation`. That tree
is immutable evidence with its own minimal module; its README explicitly says
repository test discovery must exclude it, and the official top-level
`server/test-dirs.sh` does. This was a runner-discovery failure after all three
real packages had passed. The official baseline verifier passed, and the one
remaining legitimate directory selected by `server/test-dirs.sh`, the
`resource-bomb` fixture, passed all three tests under `-race`.

Server commit `21acdcb5` replaces the package-local `find` with the canonical
top-level selector, limits it to the exact connect subtree, propagates selector
failures before running a partial selection and preserves caller arguments.
Three deterministic roots failed before the fix. The final rebased branch
passes 21/21 race executions and its official no-test traversal selects exactly
`connect`, `connect/perfvar`, `connect/sim-latency` and `resource-bomb`, without
entering baseline or evaluator artifacts. That final validation is
`/tmp/throughput-fix-2-terra-server-rebase-final-1789623466`; the harness and
traversal log SHA-256 values are
`cf9ecead3960400f0ace275aa2570c2119f49e32ac2a6aa7d1c7143950110963` and
`6f5b7dc78c12d4a2abd271e6bd8ff32c4067c05139edfa1c75589759fa5c2bc8`.
The multi-hour payload was not repeated at that checkpoint; its four legitimate
packages had already passed against the same product source. The final-source
campaign below repeats it.

The connect log is
`/tmp/throughput-fix-2-terra-server-integration-1789614340/retry-direct/connect.log`,
SHA-256 `30f239b098aeae9755f2f515c77297142789493c0f2ddb6d221beb291a94211a`.
It started at load 5.06, 7.02 and 9.76 and finished at 10.32, 10.59 and
11.75. Baseline verification is in
`/tmp/throughput-fix-2-terra-server-integration-remaining-1789620455/baseline.log`,
SHA-256 `fb9006c5b441d37e42e754ec19ecd5276ae075db7450e94bde548537da9d6906`.
The resource fixture log is
`/tmp/throughput-fix-2-terra-server-integration-bash-1789620755/resource-bomb.log`,
SHA-256 `9bf011559b43dfefd4e74ef371b286b5031f6f63751c6adf4d0d0bf3d0c9014e`.

All three earlier 64 MiB directional TCP trials are retained here, including
startup. Their source and environment differ from final validation; they are
historical evidence and do not replace the final configured integration run:

| Direction | Trial 1 | Trial 2 | Trial 3 |
|---|---:|---:|---:|
| Upload, MiB/s | 13.06 | 110.45 | 105.75 |
| Download, MiB/s | 111.78 | 109.14 | 110.78 |

The test's maximum-of-three summary would hide the cold upload. These are
local integration regression measurements, without an old-window paired arm.
Upload counts completed application writes; download counts application reads.
The multi-client fixture permits direct-route negotiation, so its throughput
is not a forced-H1 measurement. The isolated TCP matrix counts origin reads
for upload and explicitly fixes its carrier to H1.
The pool-balance fixture also uses blocking forward admission, so it does not
measure production's drop-on-full queue under sustained pressure.

The server fixes are isolated on branch `throughput-fix-2`, rebased onto clean
server main revision `3a3cc698`. Earlier failed preflight logs remain historical evidence under
`throughput-fix-2-results/server-functional-final` and
`throughput-fix-2-results/server-connect-full-final`.

### `server/proxy`

The full proxy package passed in 316.690 seconds, including the two formerly
blocked database-backed handoff tests. Its package-local script then exposed a
pre-existing acceptance-wrapper regression: a terminal INT or TERM killed the
foreground `tee`/wrapper before the controlled runner completed cleanup. The
two existing deterministic roots failed before the fix. Server commit
`7e19ae5d` on its `throughput-fix-2` branch restores an owned FIFO logger,
forwards cancellation, joins runner and logger, and deletes credentials only
after both finish. A direct wrapper INT is normalized to TERM because Bash
background children inherit ignored INT; the wrapper still returns 130.

Nine deterministic signal, normal-exit, runner-failure and logger-failure roots
pass three times under `-race` (27/27). The rebased-branch rerun log SHA-256 is
`52ec43d33e6134d07c6462ee190fe574f114dddac5c0d99a7d5d8399e5742d89`.
No sibling wrapper has the same
foreground-tee pattern. That checkpoint's official `server/proxy/test.sh` run
passed both `github.com/urnetwork/server/proxy` in 316.690 seconds and
`github.com/urnetwork/server/proxy/acceptance` in 5.958 seconds. Its 332-second
artifact is `/tmp/throughput-fix-2-terra-server-proxy-final-1789622033`; the
log SHA-256 is
`3409fa1f46440b3e9eff31d935bb6baf8fcb5e7e3e0f85b2932dd11ade3ce31a`.
Load changed from 7.13, 8.14 and 8.99 to 5.02, 7.39 and 8.58.

### Final-source server rerun

The official connect tier was repeated against clean connect `f8261152` and
server `21acdcb5` snapshots. Connect, perfvar, sim-latency and resource-bomb
pass in 4,055.767, 1,437.994, 33.015 and 0.246 seconds. The artifact is
`/tmp/throughput-fix-2-final-server-integrations-complete.NsStqy`; its connect
log SHA-256 is
`b01672b716faf2039e3cbe55c4a22f93133b8d05df98c8a006203702ca479c24`.

Proxy initially could not compile because server `23135c01` reads six SDK
telemetry fields absent from committed SDK `0dd2943`. A clean detached SDK
snapshot contains exactly the four pre-existing tracked SDK diffs as commit
`17a7a332`; patch SHA-256
`d2adcf17943a1338faaa1b65b233cd0e82b43510724e941017e127ddac9db48d`.
Its focused SDK/proxy telemetry selection passes 21 race executions.

The first full run on that coherent pair returned zero but contained a
recovered nil-pointer panic from an incomplete memory-budget test fixture. The
old test reports three passes while emitting three panics. New fixture roots
fail 6/6 against the old shape, and its wrong-parent control fails 3/3. Server
commit `27b7dad9` fixes only the test fixture; its 22-test focused race
selection passes 220/220 without a recovered panic or race.

The corrected official `server/proxy/test.sh` passes the product package in
334.528 seconds and acceptance in 6.132 seconds. The clean source pair is
server `27b7dad9`, SDK `17a7a332` and connect `f8261152`. Artifact
`/tmp/throughput-fix-2-proxy-budget-fixture-integration.pY4zuf` has log
SHA-256
`38395dac8dca9013c13480e7f5304a4ad6d393daa50acff0c62071472a29d73f`.
Load changed from 4.55/4.85/5.07 to 5.01/5.15/5.21. The complete log has zero
recovered-panic, unexpected-error, nil-pointer, fatal, warning and race
matches. The live SDK checkout and its branch were not changed.

## Reproduction

Run from the connect checkout:

```sh
tools/throughput-fix-2.sh correctness /tmp/window-correctness
tools/throughput-fix-2.sh model /tmp/window-model
tools/throughput-fix-2.sh tcp /tmp/window-tcp
CONNECT_WINDOW_TCP_BUFFER_MAX_MIB=48 tools/throughput-fix-2.sh tcp /tmp/window-tcp-48mib
tools/throughput-fix-2.sh ack /tmp/window-ack
tools/throughput-fix-2.sh server-connect-deterministic /tmp/window-server-connect
tools/throughput-fix-2.sh server-proxy /tmp/window-server-proxy
```

For the server integration selection, use its environment-loading entry point:

```sh
cd ../server/connect
./test.sh -run '^(TestConnectH[13](Encrypted(AllowFallback)?)?|TestExchangeRelayPoolBalance|TestConnectMultiClientTcpDirectionalPerformance)$' -count=1
```

The final runs used the checked-in preflight and the running local services.
On this host, source `server/test-env.sh` only from Bash: an exploratory zsh
invocation left `BASH_REMATCH` empty and produced a false invalid-authority
error. The checked-in runners already use Bash.

The script writes a test-binary SHA-256, source SHA-256, revisions, Go/OS/CPU
manifest, explicit experiment environment, complete run log, exit status and
JSONL ledger. It records only experiment-related environment variables.
Source hashes are checked again after compilation to reject a mixed-source run.
The final host runs were started immediately under the existing host workload.
The original manifests did not sample host load. New runs record start/end
load averages, explicit run context and build flags. These are regression
readings with contention noted, not isolated capacity claims.
The Go toolchain changed externally from 1.26.3 to **1.26.7** during this work;
the confirmation manifests pin 1.26.7 on Darwin/arm64.

Useful overrides include `CONNECT_WINDOW_PATH_RTT_US`,
`CONNECT_WINDOW_PATH_FLOWS`, `CONNECT_WINDOW_PATH_ACK_MS`,
`CONNECT_WINDOW_PATH_SECONDS`, `CONNECT_WINDOW_PATH_REPETITIONS`,
`CONNECT_WINDOW_PATH_ARMS`, `CONNECT_WINDOW_PATH_LANES`,
`CONNECT_WINDOW_PATH_PAYLOAD`, and `CONNECT_WINDOW_PATH_DROP`.
`CONNECT_WINDOW_MODEL_RTT_US` narrows the deterministic matrix for diagnosis.
`CONNECT_WINDOW_TCP_BUFFER_MAX_MIB=48` selects the separate TCP capacity
control; its default value of zero retains the ordinary TUN settings.
Full carrier/SDK/native-TUN confirmation, longer reliability runs, multiple
peers, bidirectional saturation and actual resident queue competition remain
the broader campaign described in the research plan.

## Completion audit: host performance gate

The original host sweep failed only zero-progress cells. It logged throughput
ratios without asserting them, allowing a 291 Mb/s candidate beside a 937 Mb/s
ceiling to finish with a process pass. Ten final-source comparisons fell below
90% of their measured ceilings; four had no recorded instrument exclusion.
`transfer_window_performance_comparison_test.go` now covers calibrated slow
candidates, a capped fixture, drifting controls, independent matched controls,
loss/stalls, the exact ten-percent margin and missing controls. Five tests
fail with the original comparison function restored; all seven pass under
the race detector after the gate fix. The host runner fails recorded rate or
delivery regressions while retaining their censor reasons and all raw rows.

The full root regression run was **non-race**, with 2,936 top-level passes and
24 explicit skips. The focused correctness and server/connect selections used
the race detector. The earlier claim of a full race regression was incorrect.
