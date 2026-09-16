# Throughput fix 2: local validation report

Date: 2026-09-15
Branch: `throughput-fix-2`
Original validation source revision: `b51530f3a402cb1dd5fe5d1daae344302a1f3069`
Original validation source manifest: `034a8ba28c61e70407d050228067a342b36f49505ae915cb99a7824a19bd90ee`
Server source revision reviewed: `77201554c49ec05bde83ec038bba6c600972892c`

The completion audit and combined RTT fix below were committed in `5a2f8a02`.
The original full-suite results do not validate those later production edits.

## Current follow-up

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
That validation is separate from the completed controlled-drain matrix.
Host performance acceptance remains open.

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
under concurrent host work; full model and host confirmation of this source
remain open.

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
The test and failed outcomes are retained as diagnostic evidence; the production
correction and normal regression test remain open. This cause is distinct from
bucket resizing and does not yet attribute all earlier host failures.

### Deterministic tests for the new failure cases

| Failure | Regression test and forced stimulus |
|---|---|
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
rejected the checked-in fallback PostgreSQL credential at `10.213.0.1:5432`;
the H1/H3, pool-balance, directional TCP and database-backed proxy handoff
tests stopped before creating their disposable database. Per the user's
direction, those integration runs are deferred for a later environment-correct
run. The sibling server checkout was not modified; it remains dirty from
external work.

## Reproduction and artifacts

The SDK settings probe captures eight profiles through the sibling SDK's actual
constructors and sizing helpers: unbudgeted and 384 MiB connect defaults, plus
desktop/mobile device and provider defaults with providing enabled or disabled.
Desktop defaults use a 20 MiB device target; the selected mobile profile uses
a 24 MiB target and 32 MiB process budget. Resolved send, receive and Pack pools,
queue limits and receive accounting are retained in `sdk-settings-current`.
This is constructor coverage on the host, not a mobile-runtime performance run.
Performance cells using those resolved profiles remain to be added.

```sh
python3 tools/throughput-fix-2-sdk-settings.py ../sdk /tmp/window-sdk-settings
```

The capture pins SDK revision `7fe75c6983dcea213a6f58d9cb0e9220be8b6534` and
connect source `5f28f1587548882900d65383a1ec176f5346df688c266366dcdf37d76e645c91`.
The latter includes the added recovery test and is distinct from the earlier
`f60cf11d` model source. Both checkouts were stable during the capture build;
the overlay leaves SDK files unchanged.

The runner records source and binary hashes, revisions, Go/OS/CPU, selected
environment, complete logs, status and JSONL ledgers:

```sh
tools/throughput-fix-2.sh correctness /tmp/window-correctness
tools/throughput-fix-2.sh model /tmp/window-model
tools/throughput-fix-2.sh regression /tmp/window-regression
tools/throughput-fix-2.sh tcp /tmp/window-tcp
CONNECT_WINDOW_TCP_BUFFER_MAX_MIB=48 tools/throughput-fix-2.sh tcp /tmp/window-tcp-48mib
tools/throughput-fix-2.sh ack /tmp/window-ack
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
- `host-feedback-controlled-epoch` — induced host diagnostics and an unfixed
  deterministic inner-TCP replay reproduction;
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

1. Confirm the full matrix after the bucket-resize correction and investigate
   the remaining recovery-time difference. The controlled-drain matrix is
   complete; each later correction still needs its own source-pinned validation.
2. Fix replay sampling across application idle, add its normal deterministic
   regression, and resolve slow host cells against the explicit comparison gate. Keep
   instrumentation limits, shared host load and candidate performance effects
   independently testable; retain failed and excluded comparisons.
3. The database-backed `server/connect` and `server/proxy` integration tiers
   are deferred by the user for a later environment-correct run through
   `server/test.sh`. Local credential repair is outside this run.
4. Resolved SDK-budget performance cells, longer actual-relay pressure,
   shard-collision, bidirectional and multiple-peer campaigns remain necessary
   before a deployment-wide claim. Continue with
   our own published fixtures as requested; obtaining the reporter's missing
   native rig is not a prerequisite for this work. Its missing traces still
   limit attribution of the reporter's specific failures.
