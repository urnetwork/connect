# Throughput fix 2: local validation report

Date: 2026-09-16
Branch: `throughput-fix-2`
Original validation source revision: `b51530f3a402cb1dd5fe5d1daae344302a1f3069`
Original validation source manifest: `034a8ba28c61e70407d050228067a342b36f49505ae915cb99a7824a19bd90ee`
Server source revision reviewed: `77201554c49ec05bde83ec038bba6c600972892c`

The completion audit and combined RTT fix below were committed in `5a2f8a02`.
The original full-suite results do not validate those later production edits.

## Current follow-up

The hybrid keeps exact receiver ACK-delay measurements, a separately proved
unloaded RTT baseline, and the existing pacing/recovery limits. Service bytes
now reach the estimator at ACK arrival, even while a sender waits in the pacer.
The first retry clock starts at physical dispatch; confirmed shared raw RTT
may extend an ordinary timeout only after proven-loss checks.

| Gate | Latest recorded result |
| --- | --- |
| Static window mismatch | All 108 comparisons pass on service-credit source `8802ba4b` |
| Finite shared relay | 1, 100 and 1,000 Mb/s cells pass three times under race |
| Full correctness | 305 race passes on recovery source `44459e42`; later storm test-only correction passes nine focused executions |
| Scoped recovery correction | 99 focused race passes; original loss and lifetime bounds retained |
| Shrinking window and 1.2 s RTT growth | Still failing; experimental estimators remain isolated |
| Full server database integration | Deferred as authorized |

The complete static matrix retains all 216 readings. Its source precedes the
latest recovery correction; final combined regression and performance results
must be attributed to a new source snapshot.

### Evidence and remaining investigations

The window-size issue is not closed. The working hybrid combines measured
receiver delay with the established unloaded-path and pacing behavior.
Previously ACKs
advertised receive-window capacity and the maximum compression interval, but
did not report actual receiver delay. Optional field 12, its receiver producer
and its RTT consumers are now integrated on the working branch.
Per-sequence batch-end markers alone cannot resolve
the observed partial-turn failure: separate logical lanes send complete heads
at different times while sharing one sender service estimate. The new feedback
must preserve that service scope. The current sender-only candidates are frozen
as diagnostic alternatives; none establishes full performance acceptance.

The timing candidate uses `adjusted RTT = raw round trip - receiver delay`.
Both elapsed times come from their own side's local clock, so no synchronized
clock is required. Raw RTT continues to drive recovery. Window sizing keeps
the actual receiver wait and reserves any larger compression allowance carried
by that same ACK. For example, 12 ms raw RTT with 5 ms receiver delay gives
7 ms adjusted RTT; a 10 ms advertised compression limit requires 17 ms of
window residence. Exact message/tag matching and confirmed first writes prevent
retries or failed writes from supplying ambiguous timing. The implementation
publishes timing at ACK arrival even while the send worker is paused. Direct
receiver timing removes the need to infer receiver-held time. It still includes
carrier queueing, so an unloaded-path estimate requires separate evidence.
Older-peer fallback and the original performance gates remain explicit checks.

The final focused timing selection passes 162 executions under race. Full
correctness on copied source `733ee2d2` records 257 passes and the three known
absent-metadata drain failures. All 23 new wire/receiver/sender/RTT prefix tests
run and pass. That run also exposes two race warnings from unlocked drain-test
precondition reads. Locked fixture snapshots remove those warnings; the three
unchanged production roots still fail all nine executions in that control.
This is not a clean full correctness pass. A subsequent carrier-change root
finds shared H1 timing overriding another carrier's local measurement. The
working correction limits that shared timing to current H1-only policies,
preserves sibling state, and passes 42 isolated race executions.

A retry lifecycle root also finds that an already measured metadata SACK can
be sampled again through the legacy tag path after retry preparation. Keeping
the observed state for the message lifetime fixes all four forced variants;
the before selection fails three executions and the corrected timing selection
passes 72 executions under race. This change does not make retried copies valid
RTT observations: ordinary retries retain the original wire timestamp tag.

The same frozen `733ee2d2` campaign finishes with 3,101 root regression passes,
the same three drain failures and 25 skips. Its model selection records 21
passes, six failures and all 818 numerical rows. The failed tests are window
mismatch, changing window mismatch, repeated drains, SDK bidirectional traffic,
RTT growth beyond the old ring and services sharing a finite relay. These
results precede the carrier correction, locked drain fixture and the revised
unloaded-baseline candidate; they do not validate those later changes.

The bounded legacy drain correction is now integrated after 534 focused race
passes. It allows a drain to cover the latest observed raw residence, keeps its
deadline anchored to the original start and configured delivery lifetime, and
protects only the exact confirmed, unretried probe from a stale ordinary retry
timer. Proven lane loss and explicit recovery retain their original deadlines;
an unrelated message's retry no longer destroys this probe. Its full combined
model validation remains pending.

The unchanged 1.2-second RTT-growth gate still fails on the integrated timing
candidate: 9.482 Mb/s against a 95.771 Mb/s reference, with 328 measured relay
drops. Its final adjusted RTT has already risen to 1.868 seconds, so an old
short RTT floor alone does not explain the loss. Transition-level service,
burst and recovery/window evidence is the next diagnostic. Direct timing does
not yet establish performance acceptance.

A diagnostic trace identifies a design problem in the rolling-minimum
consumer: on the unchanged 0.3 ms path, its shared adjusted minimum reaches
269–276 ms as the queue grows. After the latency change, that inflated floor
also raises the permitted flight and lets pacing exceed the fixed serializer's
service. Receiver residence subtraction does not remove carrier queueing.
The timing metadata remains valid; using every rolling adjusted minimum as an
unloaded-path estimate, and disabling drains whenever metadata exists, is not
accepted. A constant-propagation/queue-growth root is being added before
revising the consumer. Preserve explicitly confirmed empty-flight measurements
as separate evidence rather than treating queued observations as path latency.

The comparison follows the requested hybrid approach: retain receiver ACK
delay and its validated message lifecycle, keep the established unloaded-path
and pacing logic, and substitute direct timing only where it supplies exact
evidence. Compare the broad timing consumer, the prior consumer and the hybrid
on identical frozen inputs and unchanged gates. An isolated shrinking-window
trace already shows a separate remaining problem: after a 2 MiB to 64 KiB
receiver change, a 100 Mb/s path is repriced from 12.5 MB/s to about 0.61 MB/s.
The model reaches 3.413 Mb/s versus a 4.584 Mb/s matched constant-window control,
with no drops. It confuses window-limited feedback gaps with serialization;
that root must be checked separately from queue-driven RTT inflation.

The first comparison uses identical copied inputs (`94b5e61a`) and the same
wire producer. The "prior consumer" arm uses the new physical raw timestamps
with the old consumer policy; it is not a checkout of an earlier commit.

| Consumer | 50 ms compression, receiver shrinks to 64 KiB | 1.2 s RTT growth | SDK bidirectional comparisons |
| --- | --- | --- | --- |
| Broad rolling timing | 3.413 / 4.598 Mb/s | 90.221 / 95.771 Mb/s aggregate; intervals 0, 0, 270.664 | 3 failed gates |
| Prior raw consumer | 3.413 / 4.615 Mb/s | 6.861 / 95.778 Mb/s | 1 failed gate |
| Separate unloaded baseline | 3.413 / 4.608 Mb/s | 12.397 / 95.771 Mb/s | All 18 comparisons pass |

Each pair is candidate/reference. All three retain the independent small-window
failure. The broad consumer's long-path aggregate passes the old gate through
late queued delivery, after 1,582 warmup drops; it does not establish sustained
capacity. The permanent long-path test now also checks each of the three
one-second intervals against its reference, preserving the original aggregate
and loss checks. The exact probe's common raw-residence bound fixes its forced
worker root but leaves the complete long-path model near 12.370 Mb/s. Further
timer exceptions are not justified by that result.

The hybrid baseline and common probe bound are now integrated. Ordinary paired
RTT observations size the window and may lower the unloaded baseline. Only an
exact, physically confirmed empty-flight observation may raise that baseline.
Queue growth therefore cannot enlarge pacing bursts by replacing propagation
time. A cumulative head may confirm delivery of an earlier probe, but its own
receiver delay cannot be borrowed for that earlier message. Deterministic
coverage includes queue retirement, delayed confirmation, future observations,
metadata expiry and older peers. The baseline selection passes 192 focused race
executions; its complete evidence archive retains 208 passes and 24 failures
across ten logs, including the pre-fix and failed model results.

The common probe bound removes the metadata-only exception from the existing
absolute recovery deadline. Exact head and SACK replies at 1.210 seconds, with
10 ms measured receiver delay, now refresh the baseline to 1.2 seconds without
an intervening ambiguous copy. Both roots fail three times before; 150 focused
race executions pass after. Proven lane loss, cancellation, shorter configured
lifetimes and absent replies retain bounded recovery. The integrated correctness
selection now passes all 291 tests under race, with zero failures or race
warnings, on source `fffee019`. Full root regression on that source finishes
with 3,133 passes, two failures and 25 skips, with no race warnings.
`TestSilentLaneLongerThanTheProbeCadenceStillDrains` refuses message 1,438;
`TestSingleReliableLaneQueueInflatedRttDoesNotStorm` no longer reproduces its
control-arm storm. A separate unchanged-binary replay passes the first case
once and repeats the second failure. Neither is erased by that replay. The
silent-lane admission failure remains under investigation; the storm test
requires a forced control precondition before it can validate a recovery fix.
This correctness pass does not close the failed performance cells.

The storm precondition is now corrected in its own test-only change. In virtual
time, the default pacer performs zero retries even when the older timeout defer
is disabled. The historical storm therefore requires the existing constant-
window option in both defer arms. That control yields 31–34 retries with defer
off and zero with defer on; the original ratio and alternate-recovery checks
remain intact. A separate default-pacer test requires delivery with zero timeout
retries and zero hidden deferrals. Three original-precondition failures, three
corrected-control passes and nine final race passes are retained. The initial
virtual fixture setup failures are excluded explicitly. The silent-lane failure
remains open: the bounded recovery-v3 replay still refuses message 1,335 after
77.02 seconds, rather than explaining the earlier message-1,438 failure.

Two root investigations remain separate. In the long-path transition, retries
of old flight tails prevent a proven drain before the next RTT probe. In the
small-window transition, 64,104 delivered bytes arrive within about 10 ms, but
the estimator rejects that span because compression can last 50 ms, then uses
the following 100 ms window-idle gap as service time. The forced small-window
root fails three times. A blanket gap/flight-size filter is rejected because it
breaks eleven existing slow-cycle and drained-train checks. Further changes
must distinguish complete service evidence from idle time without weakening
those controls.

The original hybrid also fails six static mismatch cells with 256 KiB send
windows, 100 ms RTT and 10 ms compression: about 17.287 Mb/s against 19.3 Mb/s
references, across three receiver sizes and one/eight flows. Its repeated-drain
model passes. Its finite shared-relay model fails only the 1 Mb/s cell, reaching
0.164 Mb/s against 0.957 Mb/s; the 100 Mb/s and 1 Gb/s pairs pass. These runs
precede the common probe correction and retain all 234 numerical readings.

The slow-relay root is delayed service accounting. A sender can wait in the
pacer while its ACK worker already knows bytes arrived. Waiting for the sender
to apply those bytes makes the service estimate fall, which extends the same
wait. A bounded candidate publishes each original envelope's delivery credit
at ACK arrival; logical retirement and callbacks remain with the sender. One
per-item bit deduplicates SACKs and cumulative heads. Prefix indexing visits
newly acknowledged items, worker fallback covers a temporarily removed retry,
and the service lock serializes cancellation against late credit. Send items
remain 584 bytes and no ACK wire field is added.

Three actual-worker roots fail all nine before executions. The candidate
passes nine root and 240 focused race executions, plus fifteen adjacent
ownership/index checks. Three unchanged slow-relay comparisons reach
0.952–0.963 Mb/s against 0.957–0.963 Mb/s references with no measured drops.
Publication takes 52.94 ns for one new item in a 512-item window, 85.11 ns in
an 8,192-item window, and 433.2 ns for a 32-item head, with zero allocations.
These measurements ran under concurrent host work. The final implementation is
now integrated after 738 race-test executions pass, including three repeats
of all three original finite-relay capacities. The same correction restores
all six failed static 256 KiB mismatch cells: 19.642–19.875 Mb/s against
19.302–19.368 Mb/s references, with no drops. This six-cell replay uses the
original helper and gates; the complete final matrix and combined validation
remain pending. A matched control on the integrated common-probe base still
fails all six at 17.278–17.287 Mb/s, isolating service credit as the correction.
The full correctness selection on integrated source `8802ba4b` passes all 299
executions under race with no failures, skips or race warnings. Shrinking-window
and long-path transitions remain open.

The long-path review also exposes premature retries when the first timer starts
before local pacing finishes, and when a sibling ignores a confirmed longer raw
RTT. The scoped correction is now integrated after 99 focused race passes:
retime the first recovery at physical dispatch, repair both queue heaps without
removing ACK lookup/ownership, and consult confirmed shared raw residence only
after the due worker classifies the timeout as ordinary and unproved. Existing
scale, maximum interval and ACK lifetime still bound it. A broader initial-RTO
replacement is rejected because a real second-message SACK no longer recovers
the first missing message promptly. That root fails three times under the
rejected candidate. The final before selection retains three passes and 27
failures. Four older fixtures now assert the unchanged 300 ms interval from
physical dispatch, including no retry one nanosecond before that boundary;
their prior offer-clock failures remain archived.

A known-service diagnostic improves from 72.687 to 90.986 Mb/s against
95.778 Mb/s, but pins service rate and is excluded from acceptance. Without
that input, the exact credit source reaches 4.137 Mb/s before and 6.369 Mb/s
after the recovery correction, against 95.771 Mb/s, with zero drops. Its three
intervals remain only 4.342, 7.537 and 7.229 Mb/s. Recovery reduces cumulative
retries from 457 to 137 but does not repair service-estimate collapse. The
analogous retry re-arming clock now has a separate actual-worker root: a retry
due at 300 ms waits in pacing until 1.2 s, but its next 600 ms backoff is already
due at 900 ms. It fails three times before correction. A narrow fix using the
existing physical waiter timestamp is under test, including unpaced/carrier-
change provenance and the original lifetime/backoff limits. The integrated
first-copy correction separately passes all 305 correctness checks under race
on source `44459e42`.

The small-window investigation rejects both a blanket gap filter and a later
first-offer rollover candidate: the latter passes its isolated short-train root
but still fails the complete shrink cell. Subtracting the named head's measured
receiver delay also fails that cell. A head timestamp cannot date every earlier
message it cumulatively covers. A matched pair consumer using field 12 plus physical-offer eligibility passes
the shrinking-window and six genuine capacity-change controls, but is rejected
as tested: two reverse-compressed ACKs produce a false 64.38 GB/s estimate in
three forced actual-worker executions. The original sampler passes that same
counterexample. Taking the larger physical-offer and corrected-ACK interval
is already in the rejected consumer; an instantaneous original burst supplies
no protection. Its long-path test also fails and records 19,339 warmup drops.
Receiver-local ingress snapshots are being evaluated as an optimistic
information-only comparison, with no extra modeled wire bytes. No additional
service-feedback wire schema has been added.

A read-only review of sibling `server/connect` and `server/proxy` finds no direct
consumer or reconstruction of `protocol.Ack` in their production code. This is
a compatibility review, not a new integration-test pass. Their full database
integration tiers remain deferred as authorized; the sibling checkout and its
local database configuration are unchanged.

The working ACK-tail and feedback-cycle changes remain provisional. The full
v10 model on `914a2a724a7e0c1a5681301ded65e89447699ca80505772f9a9f454865ef2246`
finishes with 21 passes, five failing tests and all 814 numerical rows retained.
The failures include window-mismatch and SDK capacity losses. A separate
long-gap partition test reproduces a tenfold rate error with identical delivered
bytes; its reviewed v11 summary correction is now applied, with five regressions
and 408 focused plus 408 race passes. Small-window sampling and a new 1.2-second
RTT-growth failure remain open investigations.

Review also found a duplex fixture defect: `Upload && Bidirectional` left one
physical direction unlimited. The correction now preserves both serializers
and adds per-direction upper-rate calibration. Earlier affected SDK pairs are
retained as miscalibrated evidence; the first forced control fails all three
attempts at roughly 805 Mb/s on a declared 100 Mb/s link. Both orientation and
rate-change tests pass three times under race, retaining all 24 readings.
The separate full-compression counterfactual
now passes three race executions with its original gates, as described below.

The v10 correctness selection records 213 race passes and one exact-size failure
from an unnecessary eight-byte field in `sequenceAck`. Passing delivery credit
directly into the compressor restores the original 96-byte record and unchanged
size assertion. All 40 selected ACK/size checks pass under race on source
`f9c0626b0a8a6582acb9a98d5613e60daeb7401bdb99450a9a429cc756fbf37f`.

The TUN correction, committed in `f55e5d41`, removes an endpoint-lock dependency from incoming
TCP data. Both deterministic roots fail three times against the preceding
code; the corrected selection passes 57 race executions, and the full
correctness selection passes all 182 tests under race. Physical duplex now
progresses but fails its calibration and lifecycle gates. Its copied-source
full root regression passes 3,026 tests with zero failures and 25 skips.
Details and retained failed readings are below.

The earlier pacing corrections, committed in `3ce605cb`, preserve service while old ACK bytes are
still being applied, and make public statistics reads independent of pacing
state. Source
`538e6248fb744795d9e829e97e503744ed70284d107ebad10da646b56bf6e460`
passes **177 correctness tests under the race detector**. All eight new root and
adjacent tests fail three times each against the preceding production files.
The combined focused selection passes 321 executions; the pending-probe
correction separately passes 37 targeted model comparisons. The complete
model on this frozen source passes **24 tests, 423 paired cases and 810 ledger
rows**, including the SDK cells. Separately forced SDK failures remain open.

After correcting the source-audit and short-path fixtures, source `7afd9e4b`
passes **3,021 root regression tests with zero failures and 24 skips**, plus
the **177-test race selection**. Production is unchanged from `538e6248`.
These runs compile and execute from copied source inputs.

The corrected host fixture now also passes a bounded **physical H1** check on
test-source `b6abfa4e`: a 100 Mb/s, 0.3 ms added-RTT, one-flow download over
owned TLS/WebSockets reaches 91.469 Mb/s against a 91.550 Mb/s A/A reference.
This uses a real TCP origin and userspace gVisor TUN. The new fixture/root tests
are described below; production remains unchanged from `538e6248`.

The corrected generic TCP matrix on the same source records eight comparisons:
six accepted and two inconclusive because the one-flow, 100 ms references do
not reach the link calibration threshold. No candidate fails its relative gate.
A separate, longer 48 MiB TCP-buffer control accepts both of those configured
cells, at 916.489 Mb/s download and 908.583 Mb/s upload. These FIFO/userspace-TUN
results and their distinct buffer and duration settings are preserved below.

The preceding source-idle correction, committed in `1bf11158`, retains measured
service across a proven sender pause. Its deterministic replay keeps 125 MB/s
and reduces the next write's virtual pacing delay from 18.430482186 s to zero.
Source `5605efa750834abb352be42548f7c20cd55f56d154a2837dc5996e8b580180f7`
passes 169 race-enabled correctness tests and records a 150-pair SDK test pass.
Nine of those pairs use the later-discovered unlimited duplex direction; that
pass cannot establish complete SDK capacity or supersede the separate failures.

**Performance acceptance remains open.** The candidate passes the earlier
forced SDK feedback recovery, RTT-growth and long-compression startup controls,
but the broader model exposes small-window and SDK losses, and the new
1.2-second RTT-growth control fails. Physical duplex references still fail
calibration and exhibit
inner TCP control refusals. An induced host replay confirms the preceding
estimator mechanism, while both before/after throughput brackets pass. The
older host failures and recovery-time differences remain open.
All campaigns start immediately under recorded concurrent work.

The preceding root regression records 3,011 passes, two failures and 24 skips.
One failure compared an old binary's ownership expectations with newly edited
runtime source. The other exposed a real-clock serializer calibration defect in
a legacy short-path test. Their evidence is preserved, and separate harness
corrections are described below. They do not establish another H1 pacing defect.

The controlled-drain correction distinguishes naturally empty flights from
deliberate pacing pauses. It passes **156 tests under the race detector** and
the full deterministic model: **21 tests, 272 paired cases and 508 ledger rows**
on source SHA-256
`f60cf11db4f0c7a928144ad78d1b9cebe96bbf8916fa620d1b58d01598fc613a`.
The settled 64 KiB capacity-increase failure improves from 1.094 to 10.000 Mb/s.
A further recovery test found that changing the statistics bucket duration
erased valid delivery evidence. The correction below preserves that evidence;
it passes **160 race-enabled correctness tests and 23 targeted performance
comparisons** on source `56eec7b1968bac2214224b2d7975f849d0fe6efef53cd9322eb11a01b31bc6ef`.
Its later full-model failure is detailed below. These source checkpoints are
separate from the latest source-idle correction and its validation.

The burst-ring follow-up adds deterministic failure-before tests for sparse
ACK timing, FIFO fairness, continuous-flight RTT changes, the production
send/ACK handoff, burst epochs and byte debt across changing estimates.
The committed follow-up passes all **152 tests under the race detector** in the
full correctness selection on source SHA-256
`b5b407364a99cbb0a022b5de897fc5eb8bade0593541ddd2e0b658636c692207`.
The full model also passes: **20 top-level tests, 268 paired cases and 500 ledger
rows**. The default and 48 MiB TCP runs completed with two and four failed
comparisons respectively, each also excluded by its controls. Root regression
passes 2,996 tests with 24 skips and no failures. These runs share the same
source and record concurrent host load. Earlier checkpoints exposed the
now-reproduced service-epoch defect and
uncensored short-path host download failures. The latter remain unattributed,
so host acceptance is still open. The sections below preserve source hashes,
failed comparisons and the boundary between completed and pending validation.

## Original checkpoint and acceptance audit

The new window mechanism and its adjacent root causes now have deterministic
coverage. The race-enabled correctness selection passed 90 top-level tests.
The virtual FIFO model passed 213 paired cells (390 ledger rows), including
window-size mismatch and live capacity changes. The full root regression
selection passed 2,936 top-level tests without the race detector, with 24
explicit skips. ACK compression tests and the allocation benchmark passed.

**Host performance acceptance remains open.** The TCP matrix and separate
48 MiB TCP-buffer control each completed 168 ledger rows, but their original
gate checked only nonzero progress. Ten of their 48 paired comparisons contain
a candidate below 90% of its measured ceiling. Four of those comparisons have
no recorded calibration or A/A exclusion. Earlier process passes and report
rate ranges did not establish that all host cells performed optimally.

The comparison gate now checks throughput against the measured ceiling and
both matched controls, along with stalls and measured delivery loss. Seven
deterministic comparison tests pass under `-race`; restoring the original
comparison reproduces five failures. Failed comparisons remain visible even
when calibration or A/A drift also makes attribution uncertain. Targeted host
repeats passed their new rate/loss gate before the RTT fix below; the earlier
slow readings remain evidence. Three repeated single-flow upload comparisons
remain inconclusive for line capacity because their references were capped.

The missing native H1/TUN rig source and P37–P44 ledger were not available, so
these results are local reproduction and regression evidence. They do not
establish behavior on the reporter's deployed relay or every host network.

## Root causes fixed

- Window sizing now applies the working floor only after known local, peer and
  deployment ceilings. The unbudgeted branch also respects known limits.
- ACK compression residence is included in target and delivery terms. Wide
  arithmetic saturates contract announcements instead of wrapping to zero.
- Delivery evidence keeps paired first/last arrival checkpoints, spans a
  stable RTT/compression feedback horizon, accepts out-of-order application,
  and resets the evidence boundary when capacity increases.
- Service pacing has one bounded opening probe and one allowance per logical
  service. It uses the first physical write and ACK arrival clocks, excludes
  ambiguous/retransmitted carriers from relay RTT, and leaves drain margin when
  a queue is present.
- Upload TCP replay retains bounded origin chunks through cumulative inner ACK;
  TUN pure ACKs bypass the data handoff lock, and producer close is joined and
  drained before upload shutdown returns.
- ACK responses are owned by `sequenceAckWindow` and are independently tested.
  Each response has one cumulative head ACK and bounded oldest-first SACKs above
  that head; a newer head absorbs SACKs at or below it, and metadata after a
  SACK overflow repeats the cumulative head. The worker's 8 KiB/32
  entry reservation, eviction metadata, deadlines, gap wake and cancellation
  drain are covered for both codecs and encrypted wrappers.

## Combined fix: refresh service RTT and use it for the window

### Failure and causal chain

A continuously busy path can change from 0.3 ms to 100 ms RTT without losing
capacity. The service estimator retained its old minimum RTT until either a
smaller observation arrived or ACK traffic stopped for a minute. On the longer
path, bytes legitimately in transit exceeded the flight expected from that old
minimum. The pacer treated them as a standing queue and reduced its rate. Lower
delivery then reduced the window, which further reduced delivery.

Two estimates needed correction. Refreshing only the service baseline left the
window using the sequence's older RTT sample minimum. Those old samples age by
sample count as well as time; a sender that has already slowed can retain them
long enough to remain at the 256 KiB working floor.

### The two changes

1. **Refresh service RTT from the first write after a drained burst.** Track the
   latest physical write for each logical sequence sharing the service. Every
   sequence's tail must have a cumulative ACK, and its actual write must have
   succeeded over H1, before the next new write becomes an RTT probe. Start its
   clock after local pacing. The ACK worker preserves the earliest arrival that
   covers the probe, even when the cumulative head names a later message. That
   arrival can refresh an obsolete minimum upward. Ordinary queued RTT samples
   retain their existing ability to lower the minimum.
2. **Use the refreshed service minimum in window sizing.** The window uses the
   larger of the sequence's sampled minimum and the service's current minimum,
   then includes the advertised ACK-compression residence. Its target ceiling
   and delivery horizon therefore use the newly observed propagation time while
   the sequence's older RTT samples age out. Peer and memory ceilings continue
   to bound admission.

The first change supplies current path evidence; the second lets the sender
admit the bytes that can be in transit on that path. Both are required to break
the collapse. This is a refresh triggered by delivery of the preceding burst;
a large average RTT alone cannot establish that the sender's queue drained.

### Ordering and adjacent cases

- A SACK for the newest message does not establish that its earlier messages
  were cumulatively delivered. Sibling sequences must each clear their tail.
- ACK arrival and write completion may be observed in either order. Both are
  required before accepting H1 delivery evidence. A failed write or a different
  actual carrier cannot establish the refreshed baseline.
- Retransmitting the probe invalidates its sample because an ACK cannot identify
  which physical copy arrived. This invalidation happens at the common write
  boundary: an H1-to-H3 retry can bypass H1 pacing but still invalidates the
  original probe. Wrong-sequence ACKs, older heads, SACKs above the probe,
  duplicates and missing-contract replies cannot supply the missing proof.
- Tracking retains one tail per live sequence and one probe per service. Closing
  a sequence removes its tail and pending probe, including on repeated cleanup.
  Cancellation cannot certify physical delivery: an older sibling ACK cannot
  start a fresh probe until a write made after that cancellation is delivered.
- The two physical write branches, plaintext and encrypted, both confirm the
  actual carrier. RTT refresh happens before send-loop ACK compression can
  replace the probe's arrival timestamp with a later head.

Implementation: `transfer_window_pacing.go` owns shared probe state;
`transfer.go` records write/ACK boundaries and applies the refreshed minimum in
`sendWindowEstimate`. Focused tests are in
`transfer_window_pacing_probe_test.go`.

### Deterministic evidence and limits

The virtual FIFO test changes RTT at 4 s, warms up to 8 s, then measures one
second. It offers eight flows with 10 ms ACK compression and compares against
a constant-window reference at the final RTT. Results are payload Mb/s:

| Service capacity | RTT change | Before fix | Combined fix | Reference |
|---|---|---:|---:|---:|
| 100 Mb/s | 0.3 → 100 ms | 7.148 | 95.805 | 95.805 |
| 1 Gb/s | 0.3 → 100 ms | 4.055 | 958.075 | 958.095 |

Both reverse transitions also pass. All flows progress, with zero measured
relay drops in all four cells. The hardened implementation also passes the six
capacity-change controls and three shared-service controls. Five initial probe
tests, seven zero-order-hold bucket tests and the FIFO fixture checks pass under
the race detector. Disabling the two correction points reproduces three focused
failures. An additional cancellation test reproduced false drain proof after
releasing a sibling's unacknowledged tail; cleanup now requires fresh delivery
before probing again. The completed broad checkpoint is recorded below;
these results do not attribute every earlier slow host reading to this mechanism.

An earlier experiment used rolling average RTT directly as the permitted
in-flight residence. It fixed one RTT-change cell but regressed shared-service
controls, including measured relay drops, and was removed. The separately
tested bucket helper implements equal weighting of completed time buckets,
zero-order hold for empty buckets, and a real zero when zero is measured. At
this checkpoint it was not integrated. The burst-ring follow-up below uses
that history to request a drain experiment; it still requires drained-write
evidence before raising the propagation floor.

## Follow-up: count physical flight and message granularity

The RTT correction did not explain every slow host cell. Its first host rerun
still produced a default-buffer, 100 ms single-flow upload of 75.856 Mb/s against
a 175.282 Mb/s reference. The five measured intervals were 159.0, 183.2, 33.8,
1.06 and 0.85 Mb/s. The final window was 256 KiB, with a service estimate near
126 kB/s and a 120 kB/s pacing rate. A short-path download also had a brief
242.7 Mb/s interval between intervals near 942 Mb/s. Both failures remain in
the collected ledgers.

Two additional counting errors can falsely establish a standing queue:

1. A shared pacing reservation was charged to `sent` before its deadline, even
   though its bytes had not reached the physical writer. Concurrent producers
   waiting for future deadlines could therefore manufacture relay flight.
   Outstanding physical flight now excludes those reserved bytes. Releasing a
   reservation on success or cancellation preserves the existing pacing debt.
2. A continuous rate-times-residence bound ignores that messages are indivisible.
   One large message can exceed a small bandwidth-delay product. Even above that
   size, sending the next message while the previous one still propagates can
   cross the continuous bound by less than one message. Queue detection now
   permits **rate × residence + one observed message**, and still detects excess
   beyond that amount. The largest-message allowance resets with a fresh flight.

The same helpers now govern both the backlog flag and the decision to replace
compressed delivery peaks with a sustained rate. Otherwise those two decisions
could disagree about whether a real queue exists. ACK arrival can also prove
all sibling tails delivered before the pacing workers apply their byte credits.
That proof now supplies a lower bound on delivered wire bytes, survives the
next write, and ends the startup exemption for subsequent excess flight. Later
ACK application catches up without counting delivery twice. Canceling an owner
that changes the aggregate sent-byte coordinate invalidates the old lower bound.
Five deterministic flight tests cover these cases, including blocked concurrent
reservations and cancellation cleanup.

New performance coverage crosses 16/64 KiB payloads, 1/10/100/1,000 Mb/s service,
0.3/100 ms RTT and one/eight flows: 32 paired cells. The initial version of the
flight correction passed all 32 cells and all 12 targeted host comparisons
(six short-path downloads and six long-path uploads). Three upload comparisons
remain inconclusive for line capacity because the single-flow references are
capped. The completed broad checkpoint below includes the final one-message
rounding refinement; the subsequent ACK-arrival and carrier-retry guards have
focused validation and still require another full-source confirmation.

Broader validation also exposed two separate test outcomes. The compression
control reached about 474 Mb/s because refreshed service RTT already contains
some compressed residence. Its gate now requires at least a 40% loss relative
to the corrected arm while retaining the corrected arm's 850 Mb/s absolute
minimum; the control is no longer treated as an exact replay of the older RTT
estimator. A mixed-route attribution test failed once with more deferred direct
than relay recoveries and passed an immediate focused repeat under concurrent
load. That failed sweep remains evidence.

### Completed checkpoint and delayed-wakeup reproduction

The broad runs built source SHA-256
`55132a4dc44dbc9fdb486d5f7288ba5579370a0cb751f957bb6d365fd0d319b1`.
They started immediately with concurrent host work, without waiting for idle:

| Selection | Outcome |
|---|---|
| Focused correctness, race detector | 113 top-level tests passed. |
| Deterministic performance model | 16 top-level tests passed; 462 numerical ledger rows, including all 32 large-message and four RTT-change pairs. |
| server/connect deterministic, race detector | 33 top-level tests passed with `server/test-env.sh`. |
| server/proxy deterministic | 28 top-level tests passed with `server/test-env.sh`. |
| Full root regression, non-race | 2,958 top-level passes; one failed advertisement timestamp assertion. |
| Host TCP, default buffers | 24 comparisons; six failed the rate gate, 11 had instrument exclusions. |
| Host TCP, 48 MiB maximum | 24 comparisons; seven failed the rate gate, 10 had instrument exclusions. |

Failures and exclusions can overlap. Both host download selections passed their
rate assertions; upload failures remain open. An uncensored short-path,
single-flow upload reached 827.834 Mb/s against a 939.868 Mb/s ceiling with
default buffers, and 833.757 against 942.050 Mb/s with the larger TCP maximum.
These results do not establish that host contention caused the regressions.

The root regression's timestamp assertion was obsolete after the earlier
window-mismatch fix: a capacity increase deliberately starts a new delivery
measurement epoch. The original test sometimes hid that difference by reading
the same host clock tick twice. It now uses distinct virtual times and checks
missing, unchanged, decreasing, zero and increasing advertisements separately.

A new controlled pacing-wakeup test demonstrates a separate loss mechanism.
With one millisecond of dispatch delay, the existing pacer admits 99.9% of its
configured service; with three milliseconds, it admits only 41.1%. A delay
beyond two milliseconds discards elapsed pacing credit as though it were idle.
Feeding that delay into the end-to-end model then lowers the measured service
and compounds the loss: all four three-millisecond cells reach 17.367 Mb/s
against a 993.657 Mb/s reference, with zero measured relay drops. The matrix
crosses 0.3/100 ms RTT and one/eight flows. This proves the mechanism in a
controlled model; attributing the host failures still requires matching evidence.

### Separate burst byte and duration limits

The pacer committed in `5a2f8a02` follows the requested pair of limits. For an
estimate of **B bytes over T time**, the maximum bytes per burst is **B** and
the maximum burst duration is **2 × T**. The named multiplier is
`windowPacingBurstTimeScale`. The time multiplier does not enlarge the byte
ceiling. A burst ends when either bound is reached; the duration is an upper
bound, not a mandatory sleep.

The current estimate uses the service rate and its sampling interval, normally
10 ms. The estimated pair has a minimum of one whole physical message and its
serialization duration. This permits an indivisible message while charging
every byte against the burst limit. Exploration can refill faster than the
measured service; once a service estimate exists, its byte ceiling still applies
during the remaining startup probe. The original finite startup byte allowance
is not replenished by idle time.

Both limits and the serialization schedule are shared across sibling sequences.
An additional byte meter checks actual release after a timer wakes: reservation
limits alone allowed a 15 ms dispatch delay to combine two 10 kB reservations
into one 20 kB release. Estimate updates refill earlier elapsed time at the old
rate, never rewind the refill clock, and retain already spent credit. A waiting
large retransmission keeps the one-message minimum alive even though a retry
does not add first-delivery flight. Queue detection now uses propagation plus
the actual burst allowance; the allowance already includes packet quantization.

The controlled three-millisecond wake now admits 99.7% of the configured rate
instead of 41.1%. All eight delayed-wakeup performance cells passed the first
byte-meter candidate. That checkpoint also passed 126 focused correctness tests
under the race detector and all six short-path host upload comparisons, with
no instrument exclusions: candidate rates were 888.58–939.54 Mb/s. These runs
built source `38dca2f68b2050ac1a5807599980157e1661842c6497d936896d456bfb3b01d5`.
They precede the subsequent full-message and flight-bound refinements.

That checkpoint's completed root regression has 2,971 passes and 24 skips,
with no failures. Its full model has 16 top-level passes and one failed
compression-ablation control: the control's measured RTT had absorbed the
receiver's compression delay, so its omitted term no longer isolated the
original defect. The corrected paired fixture uses the known physical FIFO
RTT in both arms and checks that only the window's explicit compression term
differs. The receiver's real compression still informs service pacing in
both arms. Under the race detector, the isolated control reaches 202.8 Mb/s
and the corrected arm 958.3 Mb/s. The normal performance matrix continues to
measure RTT from actual feedback.

**Full acceptance remains open.** An intermediate whole-message and flight-bound
candidate kept the service-rate matrix, capacity changes, large messages,
delayed wakes and live window changes passing, but exposed two regressions:

- A continuously busy 100 Mb/s path changing from 0.3 to 100 ms RTT can retain
  its 0.514 ms service baseline because no complete tail drain occurs. The
  intermediate candidate reached 3.809 Mb/s against 95.805 Mb/s. The trace
  confirms that the opportunistic RTT probe never ran; a controlled baseline
  refresh must also cover continuously occupied flight.
- On the 1 Mb/s shared service, aggregate throughput can remain near capacity
  while one flow makes no measured progress. The trace also shows estimates
  switching between 125 kB/s and 250 kB/s while the physical service remains
  125 kB/s. Separate deterministic tests below reproduce release overtaking
  and sparse compressed heads doubling the measured rate. A finite shared
  burst must preserve reservation order.

Those failing runs are retained. The full model, root regression and host jobs
for the earlier checkpoint are still useful regression evidence, but they do
not validate the newer refinements. The refinements are committed in
`5a2f8a02`; all-cell performance acceptance remains open.

The corrected estimator resets its rolling ring for each newer burst,
carrying the preceding burst's mean as the zero-order hold. Independent tests
cover replacement by completed buckets, partial buckets, a measured zero,
late older burst IDs and reordered observations within the current burst.
The last case failed before preserving retained current-burst observations
without rewinding the arrival clock.

The ring is now integrated into the working candidate. Its recent residence
can request a bounded pause to let outstanding sibling tails drain. The ring
mean does not raise the propagation floor: only cumulative delivery of every
tail, followed by a successful H1 write and its unambiguous ACK, can do that.
This obtains fresh evidence even when an active producer never becomes idle.
The pause has a deadline and a cooldown; loss or cancellation cannot strand
the producer. The shared release meter now serves reservations in FIFO order.

The final focused run passed all four RTT-change pairs and all three
shared-service pairs after the admission-handoff and byte-debt corrections.
The previously failing 100 Mb/s RTT increase reached
95.805 Mb/s against 95.805 Mb/s; the 1 Mb/s shared service reached 0.963 Mb/s
with a minimum flow rate of 0.118 Mb/s and zero measured drops. All 80 focused
pacing and statistics tests passed under the race detector. Broader validation
uses the canonical runner source hash and records host load; focused passes
alone do not establish all-cell acceptance.

### Burst-ring broad checkpoint

The full model and both host TCP configurations built stable source SHA-256
`2b40106a41c1b5fa93dd13ed5c3092dddc0e7510f4a36a233e180ceafdc6abf0`,
before the subsequent ring-reset ordering correction. The simultaneous runs
recorded the active host workload rather than waiting for idle.

| Selection | Outcome |
|---|---|
| Focused correctness, race detector | 145 passes; the exact send-item size assertion failed until the additional eight-byte burst identity was explicitly accounted for. The later reset-corrected checkpoint passes all 147 tests. |
| Full deterministic model | 17 top-level passes, one failure; 486 ledger rows. Window mismatch, large messages, capacity changes, shared relay and RTT changes passed. |
| Full root regression, non-race | 2,988 passes, three failures and 24 skips. Failures: send-item size accounting, the historical relay-storm control and a WebRTC budget-admission assertion. |
| TCP, default buffers | 24 comparisons, two failures and seven exclusions. |
| TCP, 48 MiB maximum | 24 comparisons, two failures and two exclusions. |
| Server deterministic tiers | Initial builds detected concurrent server-source changes; immediate retries stopped at PostgreSQL preflight. No valid new server result. |

The model's failing cell is 100 Mb/s service, 400 ms RTT, eight flows and 50 ms
ACK compression: 66.847 Mb/s against a 95.826 Mb/s reference, with no measured
drops. The isolated current-source replay also fails at 66.806/95.846 Mb/s.
Its trace identifies a known idle gap after a controlled drain being interpreted
as a new service rate: one 2,671-byte probe ACK over 400 ms becomes 6,673 B/s,
scheduling another roughly 400 ms wait despite a busy physical pipeline.
The correction holds the preceding established service until fresh post-probe
checkpoints can measure a rate. It captures that rate before exposing the probe
so an ACK arriving before write confirmation cannot replace the hold with the
idle-derived rate. Missing or ambiguous probes retain the existing sampling
epoch. The isolated cell now passes at 95.846/95.826 Mb/s with zero measured
drops; final broad confirmation remains required.

Both host configurations have an uncensored short-path, single-flow download
failure: 682.808/940.206 Mb/s with default buffers and 813.778/942.388 Mb/s with
the larger maximum. The default run also has a long-path single-flow download
below the rate gate with a capped reference. The larger-buffer run's other
failure is a stalled flow in the upload reference; its candidate reached
939.200 Mb/s against 929.527 Mb/s. Failures and exclusions can overlap, and an
invalid reference is not evidence that the candidate is slow. All 168 ledger
rows from each host run remain in the archive.

### Committed service-epoch checkpoint

Source SHA-256
`b5b407364a99cbb0a022b5de897fc5eb8bade0593541ddd2e0b658636c692207`
is committed in `5a2f8a02`. The race-enabled correctness selection passes all
152 tests. The full deterministic model passes 20 tests and 268 paired cases,
with all 500 ledger rows retained. These results precede the adjacent correction
for naturally drained sparse traffic described below.

Both host configurations completed 24 comparisons and retained 168 ledger rows
each. Default buffers have two failed comparisons and ten exclusions; the
48 MiB maximum has four failed comparisons and seven exclusions. Every failed
comparison also has a control exclusion. These are failed performance gates
with uncertain attribution, not passing capacity results.

All failures are uploads with 10 ms ACK compression:

| TCP buffer maximum | RTT | Flows | Repetition | Candidate / ceiling, Mb/s | Control exclusion |
|---|---:|---:|---:|---:|---|
| Default | 0.3 ms | 1 | 2 | 600.483 / 937.515 | A/A drift |
| Default | 0.3 ms | 8 | 0 | 46.489 / 644.708 | A/A drift and low calibration |
| 48 MiB | 0.3 ms | 1 | 2 | 766.441 / 942.192 | A/A drift |
| 48 MiB | 0.3 ms | 8 | 0 | 403.946 / 663.407 | A/A drift and low calibration |
| 48 MiB | 100 ms | 8 | 0 | 755.906 / 852.429 | Low calibration |
| 48 MiB | 100 ms | 8 | 2 | 248.559 / 853.864 | Low calibration |

The runs started immediately under recorded concurrent workload. The manifest,
complete ledger, outcome excerpt and raw-log hash are retained in each
`*-burst-ring-final-service-epoch` archive. Root regression passes 2,996 tests
with 24 skips and no failures. A later correction needs its own validation; these completed
binaries continue to identify the source they actually tested.

### Adjacent fix: distinguish natural drains from deliberate pauses

Resetting the service history on every drained probe also discarded useful
serialization evidence from sparse large messages. After a settled 1→10 Mb/s
capacity increase, 64 KiB messages remained near 1.1 Mb/s, with no measured
progress for one of eight flows. The earlier capacity-change matrix switched
at 4 s, while the slow opening train could still be draining, so it missed this
established-sender case.

The correction marks only the first physical write after a deliberate pacing
pause for a service-history reset. Natural probes still refresh RTT, and their
adjacent ACKs can establish a faster service rate. Both confirmed and
ACK-before-write-completion reads follow that distinction.

The added four-cell matrix moves the capacity change from 4 s to 40 s and the
measurement forward by the same 36 s. It retains the original post-change
settling allowance, 16/64 KiB payloads, both rate directions, eight flows,
100 ms RTT, 10 ms compression, minimum 64-message sample and 90% reference
gate. Its earlier ten-second-after-change diagnostic remains separate recovery
evidence; passing after settling does not establish identical adaptation speed.

Review of the correction also caught two ways to discard a valid pause: a
late timer dispatch and cancellation of the first waiting writer. The marker
belongs to the shared service pause and survives both. Any intervening physical
write consumes it, while failed, retried or canceled-tail delivery cannot
certify a drained probe. These transitions have deterministic tests, including
the real FIFO cancellation handoff under virtual time.

The corrected source SHA-256 is
`f60cf11db4f0c7a928144ad78d1b9cebe96bbf8916fa620d1b58d01598fc613a`.
All 89 focused pacing/statistics tests pass under the race detector. The four
new settled pairs, four original large-message change pairs and exact
400 ms RTT/50 ms compression pair pass, with no measured relay drops. The
previously failing settled 64 KiB increase reaches **10.000 Mb/s**, with every
flow at 1.250 Mb/s, against the 10.000 Mb/s reference. The long-feedback case
retains 95.846 Mb/s against 95.846 Mb/s. Full correctness passes all **156 tests
under the race detector** on this source. Its full deterministic model has now
completed: **21 tests, 272 paired cases and 508 ledger rows**, all passing.
The earlier root regression and host runs remain attributed to `b5b40736`.

Recovery speed remains a separate open measurement. In the retained diagnostic
covering 10–13.3554432 s after the increase, the pre-service-epoch control reaches
9.53125 Mb/s and the initial controlled-only correction reaches 7.34375 Mb/s.
The latter's complete one-second intervals are 4.194304, 7.864320 and
8.912896 Mb/s. Its service estimate reaches the full rate at 11.41 s after the
change; that does not establish sustained 90% goodput at that time. The final
settled test begins 34.05432 s after the change. Its pass closes the permanently
held-rate failure while leaving convergence speed for a dedicated comparison.

### Preserve delivery checkpoints when bucket durations change

The new transition test retains twenty complete one-second intervals after a
settled 1→10 Mb/s increase. Before this correction, the candidate averages
7.733248 Mb/s during seconds 10–14, below 90% of its 9.961472 Mb/s reference.
Three consecutive intervals above the gate complete at 16 s. This exposes a
recovery defect that the later steady-state measurement cannot detect.

At 40.663548673 s in the diagnostic trace, a shorter RTT changes the bucket
duration from 10.243742 ms to 10 ms. Clearing the ring erases the preceding
delivery checkpoint just as a faster ACK arrives. The old 125,000 B/s hold
therefore survives despite new evidence of the increased capacity.

The correction places retained aggregates into the new 64-slot ring using
their actual arrival timestamps. Collisions preserve the first checkpoint's
bytes exactly once. When shrinking makes an old aggregate span several new
buckets, reordered arrivals join that aggregate instead of creating overlapping
sums. Expired samples and aggregates crossing a confirmed service epoch stay
excluded. The zero-order hold remains available when no new evidence exists.

The direct regression changes RTT between two delivery checkpoints and requires
the estimate to rise from 125,000 to 1,250,000 B/s. Adjacent tests cover shrinking
and growing buckets, merged first-arrival bytes, reordered ACKs, retention
expiry and epoch cutoffs. The transition candidate now averages 10.092544 Mb/s
over seconds 10–14 against the same 9.961472 Mb/s reference; packet boundaries
allow short intervals to exceed the nominal rate. Three consecutive accepted
intervals complete at 13 s. The earlier pre-service-epoch control recovered at
4 s, so this pass does not establish equal recovery speed.

Final validation uses source SHA-256
`56eec7b1968bac2214224b2d7975f849d0fe6efef53cd9322eb11a01b31bc6ef`.
All **160 correctness tests pass under the race detector**, including 93 focused
pacing/statistics tests. Seven targeted model tests pass all **23 comparisons**:
original and settled large-message changes, the recovery ramp, long compressed
feedback, capacity changes, shared service and RTT changes. Their 46 readings
remain complete. In the original 10–13.3554432 s diagnostic, the corrected
candidate reaches 10.000 Mb/s against its 10.000 Mb/s reference. The long-feedback
control retains 95.857 Mb/s against 95.846 Mb/s. These runs started immediately
under concurrent host work.

The subsequent full model on this same `56eec7b1` source completes **21 passing
tests and one failure**, retaining all 510 rows (273 paired cases).
`TestWindowPathServicesShareFiniteRelay` reaches 0.82944 Mb/s against a
0.94720 Mb/s reference at 1 Mb/s link capacity, 100 ms RTT, 10 ms compression
and eight flows across four lanes. Every flow progresses and recorded drops
are zero, but 87.6% of the reference fails the unchanged 90% gate. Repeating
the isolated test on the same immutable binary also produces a failure; this
is not explained by another test mutating global state. The failed run remains
in `model-burst-ring-rtt-resize`.

### Separate host feedback-idle failure

An induced 80 ms pause in inner TCP ACK production reproduces a sustained
host slowdown on the controlled-drain implementation. Delivery sizing with
the pause reaches 216.65 Mb/s; adding a 1.3 ms window-RTT override reaches
188.51 Mb/s. RTT-only and constant-window controls reach 936.01 and
934.66 Mb/s. These are diagnostic perturbations, separate from the historical
paired acceptance runs.

The detailed trace shows a small NAT replay replacing a prior 1.87 MB/s service
estimate with 3,722 B/s across application idle. Inner ACKs then reopen 717,904
bytes of receive space, but the next 70,646-byte Transfer write waits about
17 s. A deterministic test using the real `TcpSequence.runReturnRecovery`
worker reproduces the same mechanism three times: a 1,100-byte replay replaces
125 MB/s with 3,536 B/s and delays the next 70 KiB write by 18.430482186 s.
The original test and failed outcomes remain in `host-feedback-controlled-epoch`.
This cause is distinct from bucket resizing and does not attribute all earlier
host failures.

The correction records source idle only after every physical tail has both its
cumulative ACK and successful H1 write confirmation, with no pacing reservation
waiting. It uses the local observation time, so an old ACK timestamp cannot
invent idle. The next producer checks that boundary before reserving or waiting;
a gap of at least the sampling, bucket and ACK-compression intervals marks the
next write as a service-epoch probe. Existing confirmation logic retains the
last measured service until fresh evidence replaces it. Natural serialization
with an already-waiting producer keeps its original measurement span.

The replay regression now retains 125 MB/s and the resumed 70 KiB write waits
zero virtual time. Adjacent tests force ACK-before-write confirmation, failed
confirmation, head/interior/last reservation cancellation, delayed ACK handling,
retry invalidation, all eight sibling tails, and real `SendSequence.Pack` waiting
before its pacing reservation. A fresh slower train must replace the held value;
the correction does not restart at an optimistic target. The normal source-idle
tests and before/after evidence are in `transfer_window_host_feedback_test.go`
and `source-idle-evidence`; the full 169-test race run is in
`correctness-source-idle`.

The source-idle correction does not cover old coalesced ACK application before
a resumed probe confirms its new epoch. The next correction addresses that
separate interval. Host confirmation and SDK duplex behavior remain separate
gates.

A subsequent matched host experiment uses identical 80 ms inner-ACK pauses and
1.3 ms RTT overrides in every arm, with pacing-estimator tracing disabled.
Both brackets pass: before, 934.41 Mb/s candidate versus 933.92 Mb/s ceiling;
after, 929.87 versus 929.15 Mb/s. A/A drift is 3.34% and 1.53%, respectively.
Every arm records zero sampled NAT replay events, so these checks did not
trigger the replay root and cannot establish host repair. All eight readings,
96 intervals and comparison outcomes are retained in `source-idle-followup`.

A subsequent experiment forces one real recovery replay in every arm, after
an 80 ms inner-ACK pause and confirmed idle Transfer service. Three valid
duplicate frontier ACKs trigger the real NAT recovery worker; normal inner
feedback stays held until that replay receives its Transfer ACK. There is no
RTT override or periodic pacing trace. Disabling only the source-idle delimiter
reduces the candidate's measured service from 116,086,811 to 19,038 B/s after
the replay. With the correction, 25,252,229 B/s remains unchanged. Each arm
records exactly one replay and confirms the required idle boundary.

Both throughput brackets pass their original controls: before, 929.344 Mb/s
candidate versus 933.494 Mb/s ceiling; after, 912.565 versus 932.095 Mb/s.
A/A drift is 1.002% and 1.462%. This establishes the induced estimator effect,
not a throughput improvement or attribution of the older stalls. The fixture
uses a real loopback origin, userspace TUN and a FIFO Transfer carrier. Its
then-current `SendMultiWithTimeout` packet batching can exceed physical H1's
8 KiB message cap, so it cannot establish physical-H1 acceptance. Full readings,
all eight replay observations, source/binary hashes and the invalid initial
compile are retained in `host-replay-confirmation`. That initial compile used
noncanonical overlay paths and was rejected before execution.

### Pending old ACK bytes cannot reprice sibling reservations

A cumulative tail ACK can prove that old physical traffic has drained before
its send worker applies all of the coalesced bytes to the service sampler.
The slow shared-service trace had 16,153,090 physically drained bytes but only
16,112,995 applied bytes. The incomplete old train reduced the held rate while
the resumed probe awaited its own ACK. Later confirmation restored the old hold
after sibling writes had already been delayed.

A controlled probe now captures the bounded count of old bytes still pending.
Only observations timestamped at or before that probe's send time consume the
count. While it is nonzero, a provisional cutoff excludes the incomplete old
train without changing the committed epoch. A completed old train or a fresh
post-probe delivery pair can replace the held rate immediately. Accepted new
measurements also update the probe's saved rate, so its later confirmation
cannot restore superseded evidence.

Five deterministic tests cover the real sibling pacing wait, invalidation,
fresh slower service, complete faster old evidence and new bytes that must not
pay off the old count. The root test previously lowered 125,000 to 48,076 B/s
and extended a sibling wait from 18.181819 to 47.274172 ms. The targeted slow
shared-service model now delivers 0.96256 versus 0.94720 Mb/s. All 37 targeted
comparisons pass, including both RTT-change directions, large messages, delayed
wakes and capacity changes. Earlier blanket holds regressed the real
0.3-to-100 ms RTT transition; those alternatives were rejected. The subsequent
full `538e6248` model passes 24 tests and 423 pairs. These passes do not erase
the separate forced SDK feedback-recovery failures.

### Statistics reads cannot change pacing decisions

`DestinationSendStats` called the same retaining estimator as admission.
Polling could therefore change the rate saved by a subsequent probe. With the
same delivery history, a deterministic control retained 10 MB/s without polling
and 500 kB/s with polling. Diagnostic traces using this API could affect their
own outcome.

Admission and statistics now share the same window arithmetic, with retention
explicitly enabled only for the controller. A statistics snapshot reports fresh
evidence without updating either the service hold or a pending probe's saved
rate. Three tests cover slower/faster samples, missing and attached budgets,
probe confirmation, shared services and duplicate sequence inventory. All three
fail against the previous production files. Together with the five pending-byte
tests, the exact landed tests produce 24 failures before the correction; the
corrected source passes the full 177-test race selection. See
`pending-probe-observer-evidence` and `correctness-pending-probe-observer`.
This correction does not attribute every earlier host or SDK failure to polling.

### Keep source audits and short-path calibration reproducible

The `5605efa7` root regression completed 3,011 passes, two failures and 24 skips.
`TestTheWindowHasOneOwner` read `transfer.go` after the checkout had advanced to
`538e6248`, so its old ownership table did not describe the file it inspected.
The failure is retained as a runtime-source mismatch. The runner now copies
repository inputs, compiles inside that copy and executes from it. Both relative
reads and `runtime.Caller` resolve the build snapshot. A synthetic regression
edits the original file before both inspections: the old runner fails both,
while the corrected runner passes. External local module replacements and host
services are explicitly outside the snapshot boundary. Runs require a fresh
output directory outside Git worktrees, preventing copied Go files from entering
later source inventories. Deterministic cases cover direct, nested, aliased and
sibling-worktree output paths, as well as replacement declarations and added
snapshot inputs. The full race correctness selection passes 177 tests on the
resulting test-source checkpoint `7afd9e4b`; the same frozen binary also passes
all seven structural guards and the corrected short-path test. An initial
manual guard invocation used the snapshot parent instead of its package and is
retained as an invalid working-directory attempt. The broad root run now passes
3,021 tests with zero failures and 24 skips on `7afd9e4b`; its full outcomes and
source/binary provenance are in `regression-source-snapshot`.

`TestTheWindowRuleIsInertOnAShortPath` measured 9.9 versus 8.6 MB/s on a nominal
100 Mb/s, 5 ms path. Its untyped gateways do not run H1 pacing. The fake
serializer reset its departure after every late timer wake, charging scheduler
lateness as extra network serialization even with queued data. An explicit
100 microsecond virtual delay reproduces a 0.7705 candidate/reference ratio
three times with unchanged production code. The test now uses virtual time,
retains the 90% relative and peer-window gates, and independently calibrates
both arms against the configured rate. It also verifies sizing activation and
zero accepted-frame expiry. The corrected test and three adjacent controls pass
12 race-enabled executions. Equal underfilling fails the new calibration even
when its relative ratio is 1.00. Original failures and diagnostic limits remain
in `regression-source-idle` and `short-path-fixture-evidence`.

### Resolved SDK Transfer profiles and bidirectional traffic

The constructor capture now retains 11 profiles with 40 allowlisted fields,
including explicit mobile H1 policy, lane counts, queue limits, handoff settings
and independently allocated provider send/receive pools. The pinned fixture
`testdata/window_sdk_profiles.json` is included in the runner's source digest.
Profiles are applied after choosing the experimental policy, so the shared
48 MiB baseline cannot overwrite a constrained endpoint's actual limits.

`TestWindowPathSdkProfiles` compares every profile with the budgeted server in
both directions, at 0.3/100/400 ms RTT and one/eight flows: 132 paired cases.
`TestWindowPathSdkBidirectional` adds 18 pairs with both directions carrying
data and compressed ACKs through their finite FIFO. Each arm retains its
resolved settings, rates by direction, pool/queue limits and refusal counters.
The reference obeys the same memory and advertised receive ceilings.

The first complete run, source
`3242678e153855359b5c044d18b1dd1020184ee67c09b2b4db98cce0162a70ee`,
passes the one-way test and fails the bidirectional test. The mobile H1 provider
at 0.3 ms/eight flows reaches 428.41088 Mb/s in its return direction against
587.20256 Mb/s for the reference (73.0%). The forward direction remains about
957.47 Mb/s and all recorded drop counters are zero. Total throughput is
1385.88160 versus 1544.66304 Mb/s, also below the original aggregate gate.
All 300 readings remain in `sdk-transfer-model-first`.

Three exact-cell repeats after the source-idle correction still include one
failed return direction: 542.57664 versus 620.00128 Mb/s (87.5%). The other
two pass. These diagnostic repeats use an immutable overlay binary and retain
their separate effective-source manifest in `source-idle-followup`; they do
not replace the complete SDK campaign.

The current test strengthens this to a 90% gate for each direction, checks
both arms' progress and receive refusals, and calibrates the reference against
its window/feedback bound, including opposing FIFO serialization. It also
exercises reversed bidirectional stats and checks pool release after shutdown.
The subsequent complete SDK run on `5605efa7` records all 150 pairs passing and
retains 300 readings in `sdk-transfer-source-idle`. Later review finds that its
nine reversed duplex pairs left one direction unlimited. Those 18 rows cannot
establish calibrated duplex capacity. Exact-cell failures remain preserved;
a single full-matrix pass does not close a phase-sensitive regression.

The same fixture issue affects the 18 `Upload && Bidirectional` rows in each of
`model-pending-probe-observer` and `model-feedback-cycle`. The original
`sdk-transfer-model-first` has no reversed duplex rows and is unaffected by this
specific defect. The new regression uses finite, asymmetric windows to expose
the missing serializer: all three attempts measure 804.465–805.888 Mb/s on a
declared 100 Mb/s link. All nine observed readings remain in
`duplex-fixture-bounds-before`; three declared fourth-cell readings are missing
because each earlier assertion ends its invocation. The correction applies the
upload serializer swap only to one-way workloads. Both SDK arms now check each
direction against 101% of the physical rate, alongside the original lower and
relative gates. Adjacent tests cover both orientations and up/down rate changes.
The two fixture tests pass three times each under race on source
`00093323fc8563a7d64f999f11787d9083b3b1f48e1cd67884f7bf3fdf317ec9`;
all 24 readings are retained in `duplex-fixture-bounds-after`, with no measured
relay drops. A corrected SDK rerun must still precede a duplex capacity claim.

A read-only diagnostic reproduces reverse deficits with the provider's roughly
1.078 MB send pool full, all eight lanes capacity-blocked and usually no pacing
debt. Opposing forward data can place compressed heads behind a full FIFO
flight, turning a roughly 10 ms release cadence into 20 ms. Matching the first
real ACKs across arms yields about 816 Mb/s in the early phase, while matched
late offering lowers all three controls. Optional local ACK priority does not
preempt data already queued in the FIFO. A separate forced initial ACK-worker
barrier fails three times on the corrected `538e6248` production: roughly
431–447 Mb/s candidate versus 807–809 Mb/s reference, with zero drops and the
original warmup, measurement interval and 90% gate. This was the forced feedback
recovery root; it does not prove that every natural failure has that startup
trigger. No failed comparison is replaced by a matched-phase pass.
These fixtures cover Transfer settings on a host-selected SDK policy; they do
not exercise physical H1 priority queues, native TUN, or a mobile runtime.

The first bounded ACK-tail candidate releases one cumulative head after a
quiet interval, spending credit from newly delivered H1 bytes. It keeps SACKs
and eviction notices on their original full-compression deadline. Its forced
SDK controls improve to about 750–752 Mb/s against 700–751 Mb/s references,
but the genuine 100 Mb/s, 0.3-to-100 ms RTT-growth control fails: 6.8608 versus
95.8054 Mb/s with zero drops. The candidate is not accepted. Preserve this
failure and isolate its interaction with service sampling before changing
production or any performance gate.

The isolated sampler test reproduces the first collapse without a pending
probe or unapplied old bytes: 2,672 bytes after a 49.06 ms feedback gap replace
11,913,027 B/s with 54,459 B/s. Its bytes must acknowledge delivery immediately,
while rate evidence remains provisional until its measurement cycle completes.
The research plan records the symmetric gap/cycle and ACK-partition controls.

A later isolated cycle candidate provides actual zero-order hold: incomplete
feedback cannot recalculate an old mean using changed flight or RTT. Its final
focused block passes 129 tests three times, and both RTT growth and forced SDK
recovery pass three times. The wider 400 ms RTT/50 ms compression control still
fails at 79.299 versus 95.826 Mb/s, with further failures preserved. Startup
tracing finds a 7,232 B/s control-message estimate held despite 4,998,818 bytes
observed across 400 ms. This v9 candidate is rejected; its passing focused checks
do not replace the failed startup controls.

The rejected ACK-tail candidate's complete 14-run evidence, including all 122
numerical readings and the passing unchanged-production RTT controls, is in
`sdk-ack-tail-v3-evidence`.

### Combined ACK-tail and feedback-cycle correction

The reviewed v10 correction fixes both sides of that interaction:

1. **Release bounded cumulative progress after a quiet H1 burst.** Only first
   delivery earns credit. At least 32,000 newly delivered bytes authorize one
   early head after `ceil(AckCompressTimeout / 10)` of quiet, with the original
   compression deadline winning ties. The complete encoded head, including
   legacy/encrypted wrapping, is bounded by 320 bytes. Extra encoded feedback
   therefore costs at most 1% of credited bytes, with at most ten early turns
   per compression interval. Credit saturates at one turn and is consumed;
   duplicates, retries, no-ACK and non-H1 traffic cannot refill it.
   The delivery call passes its byte count directly to the compressor, which
   retains the bounded aggregate. Individual ACK records and the wire format
   gain no new field.
2. **Preserve the full response contract.** Each early response carries only
   one cumulative head and absorbs pending SACKs at or below it. Above-head
   SACKs remain bounded and oldest-first at the original absolute deadline,
   alongside missing-contract and eviction metadata. Early heads cannot keep
   postponing that deadline. Existing full-response count and byte limits stay.
3. **Treat the first feedback after a long gap as an incomplete cycle.** ACKs
   free capacity immediately. The estimator holds its last accepted rate and
   does not recalculate old samples using changed flight or RTT. Completion
   requires actual ACK timestamps spanning the pinned compression interval,
   or a physically ACK-proved tail with all of its bytes applied. A resumed
   write does not erase delayed old-byte accounting; new bytes cannot pay it.
4. **Allow fresh evidence to raise a startup estimate.** Newly delivered cycle
   bytes divided by the full interval from the preceding checkpoint, including
   the gap, can raise the held rate while the cycle remains incomplete. A
   decrease still needs completed evidence. Completion has no byte target
   derived from the old rate, so genuinely slower service remains measurable.

Three fixed timestamp/byte summaries retain current, completed and fresh-side
evidence across gaps longer than the existing 64-bucket ring. A controller may
accept a fresh pair before old workers finish, committing a boundary that keeps
late old bytes in accounting but out of the new rate. Statistics reads cannot
commit that boundary or alter the hold. Compression changes, ring resizing,
repeated drains, out-of-order application and repeated reads have direct tests.

The final isolated source passes 131 focused tests three times and the same
393 executions under race. Three repetitions each pass RTT growth (95.805 Mb/s),
400 ms RTT/50 ms compression startup (95.836–95.846 Mb/s), and forced SDK duplex
recovery (reverse 747.100–750.991 Mb/s, with per-direction reference gates).
Six service controls and 18 natural/early/late SDK and unpaced controls also
pass. Original sampler failures, rejected intermediate candidates and unchanged
controls remain in the complete 22-run record: 167 readings and 1,663 outcomes
in `sdk-feedback-cycle-v10-evidence`.

The candidate production and regression files are applied; the runner includes
the ACK worker roots, exact RTT-growth cell and forced SDK barrier. Copied-source
correctness records 213 passes and the eight-byte record-growth failure
described above; the model completes with 21 passes and five failing tests.
These virtual-FIFO
results do not attribute the physical duplex refusals, historical host failures
or native-TUN performance to the estimator.

The wider v10 model also finds three failing mismatch cells: 256 KiB send/receive
windows at 100 ms RTT/10 ms compression reach 17.278 Mb/s against 19.358 and
19.377 Mb/s references for one/eight flows; a 2 MiB-to-64 KiB receive shrink at
100 ms/50 ms compression reaches 3.413 against 4.550 Mb/s. Recorded drops are
zero. Keep their gates and diagnose the reduced pacing/capacity separately.
A compression-residence counterfactual fails because its path-RTT-only arm now
reaches 607.8 Mb/s, above the old test's 400 Mb/s ceiling, while delivery reaches
958.3 Mb/s and passes the exact residence and 850 Mb/s candidate checks. Early
head ACKs had removed the full-compression condition assumed by that control.
The fixture now explicitly holds both arms to their advertised compression
interval using the existing ACK hooks. Its original throughput and exact
0.3/10.3 ms residence assertions are unchanged. Three race executions pass:
202.752 Mb/s for path-RTT-only and 958.259–958.362 Mb/s for delivery. All six
readings remain in `compression-residence-isolated-control`; production ACK
timing is unchanged by the fixture correction.

The first receiver-timing wire checkpoint adds optional field 12 for actual
receiver ACK delay. On source `8ae48d8a`, 24 race checks and two allocation checks
pass: both codecs, old-peer optionality, explicit zero, malformed fields,
decoder reuse, one-head/SACK ordering and pacing, and maximum wrapped size.
The retained send item, sequence ACK and compact receive ACK remain 584, 96 and
104 bytes. Receiver stamping and adjusted RTT consumption are separate work;
the codec result alone does not fix window throughput.

The isolated receiver implementation reports exactly 25 ms for a forced 20 ms
receive handoff plus 5 ms delivery callback, and preserves a present zero for
an immediate reply. Both roots fail before stamping and pass afterward, each
three times under race. A sender root separately forces a 40 ms local pacing
wait, a 12 ms physical round trip and a further send-worker pause; it fails
three times before timing is published by ACK arrival. These initial results
are retained in `receiver-ack-timing-root-evidence`. Combined adjusted RTT,
legacy fallback and performance validation remain pending.

The SDK failures also remain visible: at 400 ms, one-flow device H1 delivery
reaches 81.136 against 90.673 Mb/s, and eight-flow provider H1 reaches 0.052
against 20.153 Mb/s. Bidirectional failures include device H1 at 400 ms/one flow,
provider H1 at 0.3 ms/eight flows and provider H1 at 400 ms/eight flows. Exact
profiles, directional rows, counters and all passing cells remain in
`model-feedback-cycle`. The reversed eight-flow duplex rows also have the
serializer defect described above; one-way and one-flow duplex failures remain
valid affected cells. These results do not support a combined acceptance claim.

The subsequent adjacent timestamp-partition check rejects v10 as complete.
After a gap longer than the ring, 1,000 bytes at one second followed by 10,000
bytes at two seconds measures 10,000 B/s. Applying the same two-second bytes
as 1,000 then 9,000 instead measures 1,000 B/s, with identical delivery totals.
The completed fallback freezes at the first partial application. All three
forced repetitions fail without requiring a statistics read or scheduler race.
Check both completed and accepted fresh-side summaries, including application
before/after controller reads and late bytes across retired epoch boundaries,
before accepting the correction.

The bounded v11 correction now keeps eligible late bytes in the retained summary
until newer measured evidence supersedes it, and retains a fresh summary when
the controller commits its epoch. It adds no fields or history growth. Five
tests cover partitions, late interior timestamps, accepted fresh epochs,
superseded summaries, compressed suffixes and changed compression. The final
selection passes 408 focused and 408 race executions, nine existing recovery
executions, six service controls and 18 SDK phase/control executions.
All 12 runs, 111 readings and 1,278 outcomes remain in
`sdk-feedback-cycle-v11-evidence`, including 21 failures from pre-fix roots and
the newly declared long-path control.

That new control increases RTT from 0.3 ms to 1.2 seconds on a 100 Mb/s,
eight-lane service with unchanged 48 MiB budgets. Both arms warm up for 12 seconds
and measure for three. It is now a normal model test in
`transfer_window_service_long_rtt_test.go`, with the original gates unchanged.
All three v10 and three v11 attempts fail the existing
90% reference gate, with no measured relay loss. References reach about
95.77 Mb/s; candidate final RTT floors remain roughly 15–18 ms. The one-second
controlled-drain ceiling is under investigation; this is not resolved by the
summary-accounting correction. A deterministic `waitForServiceMessage` test
forces the one-second premature release before a 1.2-second tail ACK. Raising
the cap to the default 60-second ACK lifetime repairs that root, but the model
still fails. Extending the wait using the latest physical RTT also fails to
complete recovery. A trace then shows a successful drain followed by a probe
that is not confirmed and concurrent retries. Retry invalidation remains a
hypothesis in that trace. The actual-worker test now forces the same mechanism:
the resumed, confirmed probe is retried using a stale 300 ms lane timer before
its 1.2-second ACK. The floor remains 1 ms because the ACK correctly becomes
ambiguous. This root and the configured cap/mixed-mean roots each fail three
times against unchanged v11 production. Their permanent tests are in
`transfer_window_pacing_drain_recovery_test.go`, included by the normal
correctness selection. Unrelated retries, carrier changes, cancellation,
deadline bounds and late ACKs remain required adjacent controls.

`long-drain-root-failure-before-evidence` preserves nine complete runs, 28
numerical readings and 44 outcomes (21 passes and 23 failures), including
counterfactuals and all long-path failures. Early invalid fixture attempts are
explicitly excluded as causal proof. A subsequent isolated candidate passes the
three permanent roots three times each, but still fails all three 1.2-second
performance comparisons. It remains provisional; root-test success alone does
not close long-path performance acceptance. Subsequent race checks identified
missing synchronization in the worker fixtures' direct state reads. Explicit
write barriers and locked snapshots now repair the fixtures: the premature
retry root fails three times before correction with no race warnings, and the
isolated v2 selection passes 444 race executions. A different-message retry
also incorrectly invalidates the live probe and fails three times before its
identity correction. `long-drain-v2-recovery-evidence` retains all 19 runs,
102 readings and 2,278 outcomes, including the earlier fixture-race diagnostic
as excluded evidence. The 400 ms and 1.2-second performance failures remain;
the candidate is not eligible as the completed throughput fix.

The isolated small-window candidate passes its bounded mismatch and SDK controls,
but adjacent tests reject its dependency on RTT/byte application order and its
use of a changed compression hint to reinterpret old bytes. The frozen v4
archive retains nine executions (three passes, six failures), nine failed
assertion rows and three complete numerical observations. Timestamped
eligibility and compression provenance are under correction; the preceding
model passes remain diagnostic until those checks close.

### Physical H1 and carrier-safe host packet groups

The first owned TLS/WebSocket run exposed a host-fixture defect before any
measurement: `SendMultiWithTimeout` encodes a whole socket batch as one Pack,
which can exceed the actual H1 8 KiB read limit. The real provider uses logical
group admission and lets `SendSequence` split it into carrier-safe messages.
The common host helper now uses that same grouping path. A forced batch of
16 packets, each 1,100 bytes, fails three times with the old helper. Afterward
all 16 arrive through six physical H1 messages, largest 3,420 bytes, with the
8,192-byte read cap unchanged.

Adjacent review found that the helper's indefinite send waited on the client
lifecycle even after its workload had been canceled. A durably blocked,
zero-slot sequence admission reproduces that failure three times while the
client stays alive. Passing the workload's `Ctx` to group admission fixes the
wait. Three ordinary tests cover the carrier cap, already-canceled refusal
ownership and cancellation during admission; all nine focused race executions
pass. Callers keep their original packet buffers, admitted sends own shared
copies, and refused sends return those copies.

A final verified source copy passes those three tests plus
`TestTcpSequenceCloseDeliversFlowLifecycleOnce`, each three times under the race
detector: 12 passes. The prior reused-copy attempt also passed the tests but
failed source-inventory verification because an import generated a bytecode
file; it is retained as invalid provenance. Only the verified copy supports
this final checkpoint. Failure-before and ownership evidence is retained in
`physical-h1-fixture-evidence`.

The generic TCP workload retains its existing default settings and 48 MiB
replay pool. The physical SDK fixture separately supplies the actual provider
share: one fifth of a 24 MiB target, with a constructor-sized 4 MiB replay pool.
Its provider/device carrier ACK reserves remain zero/eight. Teardown reconciles
NAT replay, all six Transfer pools, both carrier leases and pooled buffers.

The normal runner's `physical-h1` mode compiles source `b6abfa4e` into frozen
binary `a4d95e75`. Its predeclared A/B/A run measures 91.550/91.469/91.549 Mb/s,
candidate/reference 99.912%, with A/A drift 0.001312%. Both references exceed
the fixed 90 Mb/s capacity calibration. The candidate records 493/494 Transfer
ACKs, costing 49,300/49,400 encoded bytes in the two directions over five
seconds. No recorded carrier, Transfer or NAT refusals occur; ownership and
budget checks pass. Host load averages are recorded at start and finish;
the run did not wait for quiescence. All three readings and the full comparison
are retained in `physical-h1-smoke`.

This is one 100 Mb/s download cell with 2.5 s warmup and 5 s measurement per
arm, an owned unauthenticated relay, Transfer encryption disabled and userspace
TUN. It does not complete native-TUN, actual-server, 1 Gb/s, bidirectional,
multiple-peer or long-duration acceptance. Physical reproduction of the
affected eight-flow SDK duplex cell remains under investigation.

The first duplex extension uses eight full-duplex TCP connections, a
server-budget peer and the constrained H1 provider. One attempt stops during
cleanup because the server profile has no Pack pool; it supplies no numerical
comparison. After correcting that fixture assumption, the direct-callback
and actual-provider A/B/A runs both fail their gates and are excluded by their
controls. The direct-callback references stall. With the real provider
dispatcher, the first reference reaches 539.159 Mb/s download and 471.976 Mb/s
upload, while the candidate and final reference stall. Transfer and H1 record
no drops, but TUN output and provider TCP-control return refusals occur. Pools
reconcile. All six readings, both failed/excluded comparisons and the initial
interruption remain in `physical-h1-duplex-diagnostics`; none is accepted
throughput evidence or an isolated pacing comparison.

A fresh-process, single constant-window arm also stalls, so prior-arm port or
TUN reuse is not required. Its stack identifies an independent full-duplex
dependency: the receive worker waits for a TCP endpoint lock after injecting
data; that endpoint's processor waits for outbound TUN space; the TUN drainer
waits for Transfer admission whose release feedback is behind blocked receive
work. The pure-ACK bypass does not cover data carrying ACKs. Deterministic
reproduction and the gVisor handoff review are tracked in research-plan §26.

The correction removes the extra `LockUser`/`UnlockUser` pair after injection.
gVisor already queues arriving segments, schedules processor-owned endpoints,
and requeues work when a user releases its endpoint. Synchronous injection and
GRO flushing still publish the complete batch before returning. Same-flow
ordering, fixed outbound queues, borrowed-buffer ownership and the existing
producer yield cadence remain in place; no new worker or queue is added.

`TestTunDuplexDataHandoffDoesNotCycleThroughAdmission` holds one outbound slot,
the real Transfer admission gate and an endpoint owner to force the dependency
for single and batch writes. Pure ACKs provide the control. The separate
`TestTunFiniteTcpTailProgressesAfterEndpointOwnerReleases` establishes a real
TCP connection, captures its final 2,000-byte response and injects no subsequent
payload or replay. The stack must emit its cumulative ACK before the test calls
`Read`, then deliver the exact bytes. Both tests cover user-owned and
processor-owned endpoint locks. Older shard-counter tests remain bookkeeping
checks, not evidence that a synthetic endpoint lock is required for progress.

The final test shape fails both roots three times with the old TUN code:
six expected failures, no race warnings. The corrected 19-test selection passes
three times: 57 race executions, including cancellation, outbound-close and
retained-reference controls. Frozen binaries are `b33705f1` before and
`312e6d60` after; their effective diagnostic source hashes are retained in the
TUN handoff evidence. Main source after applying the three guarded files is
`c07140f78cac0dc84c22ec06ac8635de755e925471d3bb8a0dbacf0d7737262e`.
The copied-source correctness selection passes all 182 tests under race.
The unchanged physical duplex A/B/A completes all three readings, but remains
failed and excluded: both directions miss calibration and exceed the 10%
reference-drift limit. Candidate aggregate throughput is 833.397 Mb/s against
662.786 Mb/s for the reference average; that ratio is not an accepted gain.
Every arm refuses provider return controls: queue/send packet counts are
1,231/17,196 before, 3,591/27,972 in delivery, and 7,249/18,919 after. All are
52-byte inner TCP controls; the after-reference also drops nine gVisor outbound
packets. H1 and Transfer receive drops/refusals remain zero and budgets balance
after close. These whole-arm counters do not isolate measurement-interval loss.
The full root regression passes 3,026 tests with zero failures and 25 declared
skips on the same `c07140f7` source. Evidence is retained in
`tun-duplex-root-evidence`, `correctness-tun-duplex` and
`regression-tun-duplex`. `physical-h1-duplex-tun-fix` preserves all physical
readings and an invalid pre-compilation setup attempt.

### Generic TCP matrix after correcting packet groups

Frozen source `b6abfa4e`, binary `04930b13`, runs download and upload at 0.3 ms
and 100 ms RTT with one and eight flows, 10 ms ACK compression and three seconds
of measurement per arm. Each cell retains the matched/ceiling/delivery/matched
order. All 32 readings and eight comparisons remain in `tcp-grouped-fixture`.
Both top-level tests pass; six comparisons are accepted, two are inconclusive,
and none fails the candidate/reference gate.

| Direction | RTT | Flows | Ceiling Mb/s | Delivery Mb/s | Result |
|---|---:|---:|---:|---:|---|
| Download | 0.3 ms | 1 | 909.931 | 914.517 | Accepted |
| Download | 0.3 ms | 8 | 917.346 | 917.351 | Accepted |
| Download | 100 ms | 1 | 161.847 | 161.890 | Reference below link calibration |
| Download | 100 ms | 8 | 909.819 | 916.601 | Accepted |
| Upload | 0.3 ms | 1 | 915.755 | 914.731 | Accepted |
| Upload | 0.3 ms | 8 | 907.433 | 914.180 | Accepted |
| Upload | 100 ms | 1 | 177.884 | 174.705 | Reference below link calibration |
| Upload | 100 ms | 8 | 916.522 | 917.711 | Accepted |

The separate capacity control uses `CONNECT_WINDOW_TCP_BUFFER_MAX_MIB=48`
for the one-flow, 100 ms cells, with 12 seconds per arm and unchanged Transfer
budgets and acceptance gates. Binary `d1ff0199` on the same source records
917.344/916.489 Mb/s ceiling/delivery download and 916.338/908.583 Mb/s upload.
Both comparisons pass with no exclusions; all eight readings and two
comparisons remain in `tcp-grouped-capacity`. This establishes capacity for
that buffer/duration configuration. Since both settings differ from the short
default-buffer run, it does not isolate buffer size as the sole cause of the
earlier underfill or erase those inconclusive comparisons.

Both campaigns ran immediately alongside the sampler and physical-duplex
experiments. Their manifests record host load and verified source snapshots.
They use the generic FIFO, userspace TUN and owned TCP origin, not the constrained
mobile SDK profile or a physical H1 carrier. These single repetitions do not
close the older host failures or the broader confirmation campaign.

### Deterministic tests for the new failure cases

| Failure | Regression test and forced stimulus |
|---|---|
| A tiny head after a real feedback gap collapsed established service | `TestWindowPacingPartialFeedbackGapPreservesEstablishedService` replays the actual byte/timestamp deltas with no probe or delayed accounting. |
| A strict partial-cycle hold trapped a low startup estimate | `TestWindowPacingPartialCycleRaisesOnlyFromNewDelivery` and `PartialCycleIncreaseKeepsCompressionGap` require increases from actual new bytes across the complete gap while retaining completion for decreases. |
| Delayed ACK accounting or changing compression could reprice accepted evidence | `TestWindowPacingFeedbackCycle*` forces physical-tail application, old/fresh boundaries, repeated drains, ring expiry, compression changes and non-mutating statistics. |
| Early head feedback could spend old bytes or postpone SACKs | `TestReceiveSequenceBurstTail*` exercises the real ACK worker, one-head responses, absorption, oldest-first SACK deadlines, byte credit, quiet-time rounding, encoded size and cancellation. |
| Duplex data injection waited on an endpoint whose output needed a later Transfer ACK | `TestTunDuplexDataHandoffDoesNotCycleThroughAdmission` fixes the outbound slot, admission credit and endpoint ownership for single/batch writes and pure-ACK controls. |
| Removing an endpoint handoff could strand a finite response | `TestTunFiniteTcpTailProgressesAfterEndpointOwnerReleases` requires a real 2,000-byte TCP tail to be cumulatively ACKed before `Read`, with no later payload or replay. |
| A host socket batch exceeded the physical H1 message cap | `TestWindowTcpSocketBatchFitsPhysicalH1` sends 16 packets as one logical group through the actual TLS/WebSocket writer with an asserted 8,192-byte cap. |
| Rejected fixture admission could lose borrowed packet ownership | `TestWindowTcpCanceledBatchReturnsShares` closes the client before group admission and reconciles caller buffers and retained shares. |
| Workload cancellation left fixture admission waiting on a live client | `TestWindowTcpWorkloadCancelUnblocksGroupAdmission` holds the consumer before a zero-slot handoff, cancels only the workload and checks immediate release under virtual time. |
| Compressed sparse heads doubled 125 kB/s to 250 kB/s | `TestWindowPacingBackloggedSparseHeadsKeepTheirTime` replays exact byte/time pairs in both application orders. |
| A newer writer overtook an older reservation | `TestWindowPacingWaitingWritersKeepReservationOrder` blocks the older writer's timer dispatch at a channel barrier before starting its successor. |
| Continuously occupied flight retained a 1 ms RTT floor after a 100 ms change | `TestWindowPacingContinuousFlightRefreshesChangedRoundTrip` supplies explicit virtual-time feedback and cumulative tail delivery. |
| An ACK credited 2,000 bytes when only 1,000 had been written | `TestWindowPacingAdmissionCannotCreditAnUnbegunWrite` holds the real `SendSequence.writeMaybeWrappedBytes` at a channel barrier and injects the ACK through `coalesceReceivedAck`. |
| Resetting a burst ring discarded valid reordered samples | `TestWindowPacingBurstStatsAcceptRetainedCurrentBurst` supplies the same burst's samples out of order and checks the next burst's held mean. |
| A later burst's reset timestamp rejected an earlier arrival from that same burst | `TestWindowPacingBurstStatsAcceptRetainedAfterReset` applies burst two's 12 ms sample before its 11 ms or 9 ms sample, then checks retained buckets and the next burst's held mean. |
| Late timer dispatch split one physical burst across old reservation epochs | `TestWindowPacingBurstEpochFollowsActualDispatch` delays the timer by 30 ms, then checks a single 800-byte release and subsequent expiry. |
| A smaller estimate forgot bytes already emitted at a delayed wake | `TestWindowPacingChangedEstimateRetainsActualBurstCharge` changes the estimate after 40 bytes have been emitted and checks that one extra byte cannot escape the remaining allowance. |
| A nominal deadline forgave a late release before its bytes had serialized | `TestWindowPacingDecreasedEstimateCannotForgiveALateRelease` releases 8,000 bytes late, reduces the estimate and requires the original eight milliseconds of service before the next release. |
| A confirmed drain's idle gap replaced established service with one probe's apparent rate | `TestWindowPacingDrainedProbeHoldsServiceUntilFreshEvidence` forces the old checkpoint to expire, checks direct and covering ACKs, then verifies that fresh slower service replaces the hold. |
| Probe ACK processing outran confirmation of the physical write | `TestWindowPacingProbeAckBeforeWriteConfirmationHoldsService` observes and queries the ACK before confirming H1; unsuccessful confirmation retains the original sampling epoch. |
| Naturally empty flights repeatedly discarded faster serialization evidence | `TestWindowPacingNaturalProbePreservesSerializationEvidence` doubles observed service with direct and ACK-before-write-completion orders while retaining the RTT refresh. |
| An earlier drain could affect a later unrelated probe | `TestWindowPacingAbandonedDrainCannotResetLaterService` covers timeout, retry, carrier change and cancellation; the existing hold test also checks one-shot consumption. |
| A proposed deadline clear discarded a successful drain after a late wake | `TestWindowPacingLateDispatchKeepsSuccessfulDrainEpoch` ACKs the tail within the pause, then explicitly calls admission beyond its deadline. |
| A proposed cancellation clear discarded the successor's inherited pause | `TestWindowPacingCanceledHeadTransfersControlledDrain` forces two queued writers and a third sequence's tail, cancels the head, and completes the successor's real pacing handoff. |
| A changed bucket duration discarded the ACK pair proving faster service | `TestWindowPacingShorterRoundTripKeepsFasterServiceEvidence` changes RTT between exact delivery checkpoints and checks the new rate and subsequent hold. |
| Rebucketed aggregates could overlap, double-count first bytes or retain an old epoch | `TestWindowPacingResizedSamplesKeepFirstBytesAndReordering`, `TestWindowPacingResizedSamplesExpireRetainedTime` and `TestWindowPacingResizedSamplesRespectServiceEpoch` force those boundaries. |
| A small inner replay priced source idle as physical serialization | `TestTcpReturnReplayPreservesPacingServiceAcrossFeedbackIdle` runs the real recovery worker, reopens the inner window, then measures the next bulk pacing wait. |
| Source-idle confirmation, cancellation or shared-lane cleanup could discard held service | `TestWindowPacingSourceIdleRequiresConfirmationAndFreshEvidence`, `TestWindowPacingSourceIdleCancellationPreservesSuccessor`, `TestWindowPacingLastCanceledDemandStartsSourceIdle`, `TestWindowPacingPackWaitingBeforeReservationAcceptsFreshService` and `TestWindowPacingSharedSourceIdleRequiresEveryTail` force the adjacent boundaries. |

Each case has a recorded failure-before run. The byte meter preserves actual
spent bytes separately from credit withheld by an estimate increase. It also
retains each release's original serialization cost, including writes split
across the startup-probe boundary. Those timing checks pass alongside the new
estimate-change tests; neither a new nominal burst nor a reduced rate may erase
bytes released late.
Adjacent tests cover canceled and reused FIFO waiters, missing/selective or
changed-carrier tail replies, bounded drain deadlines, older burst IDs,
partial buckets and replacement of the held value. All of these belong to
the ordinary focused correctness selection, not an opt-in trace.

The post-reset case was found during the adjacent review: the first reordering
test had exercised only burst one, before a reset timestamp existed. Burst
identity now admits retained samples from the verified current burst while
ordinary timestamp-based ring callers still reject earlier-epoch data. The
exact `sendItem` size guard also accounts for the new eight-byte burst identity:
584 bytes total, with no change to the pool's item-count limit.

The broad root suite also exposed drift in a historical relay-storm control:
`reliableAdmissionUnbounded` disabled the old admission rule but still inherited
the new delivery-sizing policy and H1 pacing. Its expected unpaced storm no
longer followed from the settings. `TestRelayInflationUsesConstantSendWindow`
now checks both endpoints of all three historical arms under a forced delivery
default, and failed on all six before the fixture was corrected. Those arms
explicitly select the constant policy; ordinary mixed-lane fixtures retain the
shipping default. The existing storm gate remains at least 500 timeout resends
and passed three loaded runs with 1,455, 1,545 and 1,438. This repairs the
control's precondition; production pacing is unchanged.

The WebRTC admission failure was another uncontrolled test ordering.
Prioritizing the waiting network peer intentionally reclaims the first peer's
dedicated reservation; admission is valid if teardown already released it.
`TestWebRtcNetworkPeerAdmissionWaitsOnDedicatedBudget` now holds physical teardown
at its lifecycle mutex, checks refusal at the exact full budget, then releases
teardown and checks notification plus successful admission. It covers active
and passive setup without relying on a short negative timeout. Forcing the
released-before-admission ordering reproduced the old assertion three times;
the corrected test and five adjacent cases pass 20 repetitions under `-race`.

Four more performance pairs cross 16/64 KiB messages with 1↔10 Mb/s capacity
changes, 100 ms RTT, 10 ms compression and eight flows. Their first run passed:
the minimum candidate/reference ratio was 96.9%, every flow progressed and
there were no measured drops. Each slow interval covers at least 64 payloads
to avoid calling packet-sized serialization a stalled flow. These pairs now
run in the regular model selection alongside the 32 steady large-message
pairs and the small-frame capacity-change controls.

## Original deterministic coverage

| Area | Coverage and outcome |
|---|---|
| ACK coalescing | Head/SACK ordering, absorption, overflow, pacing, maximum serialized size, eviction notices and final drain; pass. |
| Window arithmetic and ownership | 90 top-level tests under `go test -race`; pass. |
| RTT/flow/compression model | 36 cells; minimum candidate/reference ratio about 99.9%; pass. |
| Service pacing | 54 service-rate cells, six rate changes and three shared-service cells; every cell passed the 90% acceptance threshold (the minimum paired delivery/ceiling ratio was 97.9%); zero measured relay drops. |
| Window mismatch | 108 ordered sender/receiver pairs (256 KiB, 2 MiB and 48 MiB), crossed with 0.3/100/400 ms, 0/10 ms compression and one/eight flows; minimum ratio about 99.3%; pass. |
| Live window changes | Six 64 KiB↔2 MiB changes with 0/10/50 ms compression; minimum ratio about 98.8%; pass. |
| Queue and recovery | FIFO bounds, gap deadline, contract lead, retransmission ownership, cancellation and no receive evictions; pass. |

The model ledger reports zero measured relay drops and zero receive-queue
evictions in delivery arms. The ratios compare a candidate with its paired
constant-window reference under the same virtual service; they are acceptance
thresholds for the fixture, not a universal optimum claim.

## Host TCP evidence

Both host runs used the loopback kernel socket origin, provider NAT, Transfer,
gVisor TUN and H1, with three repetitions, three seconds of measured time after a
two-second warmup, 0.3/100 ms RTT, one/eight flows and 10 ms ACK compression.
They were started immediately while other host work was running; this context
was recorded during execution. The original manifests contain no host-load
samples. The updated runner records load averages at the start and finish,
build flags, and an explicit run-context note for new runs.

The complete candidate ranges below come from the final-source ledgers, over
three repetitions per cell. All rates are application payload Mb/s.

| Direction | RTT, ms | Flows | Default TCP buffers | 48 MiB TCP maximum |
|---|---:|---:|---:|---:|
| Download | 0.3 | 1 | 924–940 | 669–923 |
| Download | 0.3 | 8 | 907–942 | 291–628 |
| Upload | 0.3 | 1 | 929–942 | 940–943 |
| Upload | 0.3 | 8 | 794–941 | 910–928 |
| Download | 100 | 1 | 161–164 | 797–933 |
| Download | 100 | 8 | 863–938 | 942–942 |
| Upload | 100 | 1 | 90–181 | 942–943 |
| Upload | 100 | 8 | 448–747 | 750–805 |

The default run has eight censored comparisons out of 24; the 48 MiB control
has ten out of 24. Censoring limits causal interpretation, not the visibility
of slow outcomes. In particular, a default 100 ms single-flow upload decayed
from 167 to 96 to 7.7 Mb/s across its three measured seconds. Its final service
estimate was about 294 kB/s. That deserves a controlled investigation of
service sampling and pacing; host contention alone has not been proven causal.

No measured relay drops, NAT refusals or receive evictions occurred. These
finite samples do not replace a native TUN or WAN campaign, and a favorable
rerun cannot erase their rate regressions.

## Server regression review

The `server/connect` deterministic race tier passed. It covers reliable receive
pressure, resident ingress retirement, H1 batching/FIFO ownership, H3 ACK
reserve configuration, exchange framing and lifecycle joins. The
`server/proxy` deterministic tier passed in the non-race mode required by
`server/test.sh`; it covers memory admission, borrowed packet ownership,
WireGuard/TUN handoff, manager close/join, drain coordination, lifecycle
metrics, window identity restore and bounded traffic metrics.

The configured integration selections were attempted using the environment
loaded by `server/connect/test.sh` and `server/test-env.sh`, with
`WARP_TEST_ENV_FAIL_FAST=1` and the local endpoints. The launcher readiness
marker was absent. Once the local override was applied, the Go preflight
rejected the checked-in fallback credential at the local PostgreSQL endpoint;
the H1/H3, pool-balance, directional TCP and database-backed proxy handoff
tests stopped before creating their disposable database. Per the user's
direction, those integration runs are deferred for a later environment-correct
run. The sibling server checkout was not modified; it remains dirty from
external work.

## Reproduction and artifacts

The SDK settings probe captures 11 profiles through the sibling SDK's actual
constructors and sizing helpers: unbudgeted and 384 MiB connect defaults, plus
desktop/mobile device and provider defaults with providing enabled or disabled,
including explicit mobile H1 policy.
Desktop defaults use a 20 MiB device target; the selected mobile profile uses
a 24 MiB target and 32 MiB process budget. Resolved send, receive and Pack pools,
queue limits and receive accounting are retained in `sdk-transfer-profiles`.
This is constructor coverage on the host, not a mobile-runtime performance run.
The new Transfer performance cells use the pinned resolved profiles.

```sh
python3 tools/throughput-fix-2-sdk-settings.py ../sdk /tmp/window-sdk-settings
```

The capture pins SDK revision `7fe75c6983dcea213a6f58d9cb0e9220be8b6534` and
connect source `56eec7b1968bac2214224b2d7975f849d0fe6efef53cd9322eb11a01b31bc6ef`.
It used an isolated SDK checkout at that revision because unrelated local SDK
edits did not compile; the relevant constructor files were identical. Both
sources were stable during compilation. The earlier eight-profile capture is
preserved in `sdk-settings-current` with its own source manifest.

The runner records source and binary hashes, revisions, Go/OS/CPU, selected
environment, complete logs, status and JSONL ledgers:

```sh
tools/throughput-fix-2.sh correctness /tmp/window-correctness
tools/throughput-fix-2.sh model /tmp/window-model
tools/throughput-fix-2.sh sdk-model /tmp/window-sdk-model
tools/throughput-fix-2.sh regression /tmp/window-regression
tools/throughput-fix-2.sh tcp /tmp/window-tcp
tools/throughput-fix-2.sh physical-h1 /tmp/window-physical-h1
CONNECT_WINDOW_TCP_BUFFER_MAX_MIB=48 tools/throughput-fix-2.sh tcp /tmp/window-tcp-48mib
tools/throughput-fix-2.sh ack /tmp/window-ack
tools/throughput-fix-2.sh pacing /tmp/window-pacing
tools/throughput-fix-2.sh server-connect-deterministic /tmp/window-server-connect
tools/throughput-fix-2.sh server-proxy /tmp/window-server-proxy
```

Final collected evidence is under [throughput-fix-2-results](throughput-fix-2-results):

- `correctness-burst-ring-final-controlled-epoch` — 156 passing race-enabled
  correctness tests on the controlled-drain source;
- `model-burst-ring-final-controlled-epoch` — its full passing 272-pair model;
- `correctness-burst-ring-rtt-resize` — 160 passing race-enabled tests after
  preserving delivery checkpoints across bucket-duration changes;
- `rtt-resize-evidence` — direct and intermediate failures, 23 passing model
  comparisons and the separate recovery diagnostics;
- `model-burst-ring-rtt-resize` — completed full model, including its slow
  shared-service failure and all 510 readings;
- `correctness-source-idle` — 169 passing race-enabled tests on the latest source;
- `source-idle-evidence` — 18 before failures, 276 focused race executions and
  50 scoped model readings after the replay-idle correction;
- `source-idle-followup` — host brackets that did not trigger replay, plus
  three exact SDK duplex repeats including one failure after the correction;
- `host-feedback-controlled-epoch` — original induced host diagnostics and
  deterministic inner-TCP replay failure before its correction;
- `sdk-transfer-profiles` — 11 pinned constructor profiles with 40 fields;
- `model-pending-probe-observer` — 24 passing tests, 423 paired cases and all
  810 model rows on `538e6248`, including the SDK matrix;
- `pending-probe-observer-evidence` and `correctness-pending-probe-observer` —
  24 deterministic pre-fix failures, focused controls and 177 full race passes;
- `regression-source-idle` and `short-path-fixture-evidence` — both baseline
  regression failures and separate source/calibration diagnoses;
- `regression-source-snapshot` and `correctness-source-snapshot` — 3,021 root
  passes, 24 skips, and 177 race passes on the corrected test-source checkpoint;
- `host-replay-confirmation` — eight actual replay observations and both full
  passing host brackets, with estimator evidence and physical-carrier limits;
- `physical-h1-smoke` — the corrected fixture's three physical H1 readings
  and passing A/B/A comparison on `b6abfa4e`;
- `physical-h1-fixture-evidence` — deterministic message-cap and cancellation
  failures, the guarded handoff and 12 valid copied-source race passes;
- `tun-duplex-root-evidence` and `correctness-tun-duplex` — six pre-fix
  failures, 57 focused race passes and 182 full correctness race passes;
- `physical-h1-duplex-tun-fix` — all three TUN-corrected duplex readings,
  failed calibration/lifecycle gates and complete return-control counters;
- `sdk-ack-tail-v3-evidence` — the rejected ACK-tail experiment's complete
  14-run record, with 122 numerical readings and its real-RTT regressions;
- `sdk-feedback-cycle-v10-evidence` — 22 complete runs, 167 numerical readings
  and 1,663 outcomes, including 38 retained failures across roots and rejected
  controls; v10 remains provisional after the adjacent partition failure;
- `correctness-feedback-cycle` — the frozen combined v10 selection: 213 race
  passes and one strict struct-size failure, before its accounting update;
- `ack-credit-without-record-growth` — 40 race passes after moving delivery
  credit into the compressor call and restoring the original 96-byte record;
- `sdk-feedback-cycle-v11-evidence` — the bounded summary correction's
  12 runs, 111 readings and 1,278 outcomes, including all long-path failures;
- `model-feedback-cycle` — all 814 v10 model rows and 21 passes/five failures;
- `regression-tun-duplex` — 3,026 root passes, zero failures and 25 skips on
  the separate TUN-only production checkpoint;
- `compression-residence-isolated-control` — three race passes and six full
  readings with the original throughput/residence gates and explicit full ACK
  compression in both arms;
- `v11-feedback-summary-roots` — five race passes on the combined direct-credit
  and v11 summary source;
- `duplex-fixture-bounds-before` — three deterministic missing-serializer
  failures, nine observed rows and explicit missing-cell accounting;
- `duplex-fixture-bounds-after` — six race passes and all 24 direction/rate-change
  readings after restoring both serializers;
- `long-drain-root-failure-before-evidence` — three permanent deterministic
  roots, diagnostic counterfactuals, 28 readings and all 44 outcomes;
- `long-drain-v2-recovery-evidence` — corrected worker synchronization,
  isolated recovery controls and all 400 ms/1.2-second failures, with 19 runs;
- `pacing-estimator-baseline` — three full-ring CPU/allocation benchmarks,
  173.9–189.4 ns/op, zero allocations, each retaining 12.5 MB/s; these measure
  estimator cost separately from virtual-time throughput;
- `server-bootstrap-recheck` — normal environment bootstrap on server
  `0522f3f6`, stopped at launcher readiness before a test or binary was created;
- `window-mismatch-v4-arrival-failure-before-evidence` — ordering/compression
  failures in the isolated small-window candidate, with complete outcomes;
- `sdk-transfer-source-idle` — 150 recorded SDK pair passes, including nine
  reversed duplex pairs invalidated by the missing serializer;
- `sdk-transfer-model-first` — 150 SDK Transfer pairs, including the original
  mobile-provider duplex failure;
- `sdk-settings-current` — eight pinned, sanitized constructor profiles;
- `controlled-epoch-final` — focused passes, natural-drain and intermediate
  correction failures, settled-capacity ledgers and separate recovery diagnostics;
- `*-burst-ring-final-service-epoch` — the preceding source's complete
  correctness, model, root regression and host TCP campaign;
- `model-final` — deterministic model ledger and manifest;
- `tcp-final` and `tcp-capacity` — host TCP ledgers;
- `ack-final` — ACK benchmark and head-drain test;
- `server-connect-deterministic-final` and `server-proxy-deterministic-final` —
  passing server tiers;
- `server-functional-final`, `server-connect-full-final` and
  `server-proxy-integration-final` — deferred preflight attempts;
- `failure-before` — retained failure-before experiments.

The peer review and research plan remain in
[THROUGHPUTFIX-PR2.md](THROUGHPUTFIX-PR2.md), and the implementation/results
ledger is in [THROUGHPUT-PR2-RESULTS.md](THROUGHPUT-PR2-RESULTS.md).

## Remaining work

1. Finish deterministic root fixes for small-window sampling and long-RTT drain
   recovery, preserving genuine slowdown and retry-ambiguity controls. Earlier
   forced SDK recovery and RTT-growth passes do not close the broader failures.
   Retain all rejected candidates and the separate recovery-time difference.
2. Expand host comparisons with the corrected packet grouping beyond the
   passing physical H1 smoke. The induced source-idle replay
   confirms the estimator effect, but both throughput brackets pass and the
   older host failures remain unattributed. The TUN-corrected physical duplex
   reference still fails calibration and records many refused inner TCP
   controls; force the admission ordering before assigning a cause. Keep the
   original gates and all failed/excluded comparisons under current host load.
3. Validate accepted production changes with the complete SDK/model matrix,
   scoped race selection and root regression. Prior checkpoints passed all
   three; the combined ACK/estimator source needs fresh validation after its
   remaining corrections. The TUN-only checkpoint has 182 race passes and
   3,026 root passes with zero failures and 25 skips.
4. The database-backed `server/connect` and `server/proxy` integration tiers
   are deferred by the user for a later environment-correct run through
   `server/test.sh`. Local credential repair is outside this run. New attempts
   at the non-database scoped tiers stopped even earlier: `server/test-env.sh`
   requires a local launcher readiness attestation that was absent. No new
   server test binary was built; earlier passing tiers do not validate this
   later connect checkpoint. A fresh normal bootstrap on server `0522f3f6`
   confirms the same readiness failure; `server-bootstrap-recheck` retains its
   sanitized result. No credentials, host configuration or launcher policy
   were changed. Further attempts await external-state change.
5. Broader physical SDK/H1 and native-TUN confirmation, longer actual-relay pressure,
   shard-collision and multiple-peer campaigns remain necessary
   before a deployment-wide claim. Continue with
   our own published fixtures as requested; obtaining the reporter's missing
   native rig is not a prerequisite for this work. Its missing traces still
   limit attribution of the reporter's specific failures.
