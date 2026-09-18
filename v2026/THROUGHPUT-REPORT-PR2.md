# Throughput fix 2: local validation report

Date: 2026-09-17
Branch: `throughput-fix-2`
Original validation source revision: `b51530f3a402cb1dd5fe5d1daae344302a1f3069`
Original validation source manifest: `034a8ba28c61e70407d050228067a342b36f49505ae915cb99a7824a19bd90ee`
Original server source revision reviewed: `77201554c49ec05bde83ec038bba6c600972892c`

The completion audit and combined RTT fix below were committed in `5a2f8a02`.
The original full-suite results do not validate those later production edits.

## Current follow-up

**The grow-only window, adaptive pacer and network-quality phase are now
implemented together.** Ordinary evidence may grow the learned byte window but
cannot shrink it. `NetworkQualityChanged` opens one bounded remeasurement
generation in which qualified fresh service and RTT evidence may replace the
retained window. Pacing continues to adapt without a notification. A hard
`NetworkChanged` also invalidates quality once while retaining its transport
recovery behavior.

The public call is wired through the SDK and Android, Apple, Windows and Linux
hosts. A client applies a local event to all of its estimators and sends a
24-byte reliable reserved-subprotocol notification to known peers. A received
event is source scoped, deduplicated and never echoed. Provider receive-only
peers are included, so one `DeviceLocal` event reaches its local client and the
clients using that provider. A refused zero-wait notification remains queued
for bounded retry. Listener, peer and generation state all have
deterministic lifecycle and memory-bound tests.

The three formerly deferred path-change models now call the quality signal at
their programmed switch and run in both `model` and the compatibility
`model-core` mode. Their latest frozen checkpoint passes, as do all 18 core
quality/drain roots, the 12-cell correctness matrix and the full model's 858
rows. The final combined-source correctness, model and regression archive also
passes and is recorded below. Earlier experiment and deferral passages in
this report are chronological evidence and are superseded by the implementation
sections near the end.

The [shared-pacing review](throughput-fix-2-results/window-cumulative-shared-reserve-root-v3)
provided the deterministic consumer reproduction:
two- and four-lane cases underfill one common reservation clock when each lane
supplies its own cumulative delivery rate. Equal and unequal shares fail all
12 repeated utilization checks; the single-lane, idle/stale, independent-service
and original cumulative controls pass (45 passes total, no race warnings).
The fixture supplies bounded training feedback and then runs the actual pacing
waits and dispatch budget. It establishes the rate-scope mismatch, not a
closed-loop path-throughput result.

The production-credit version repeats the same 45 passes/12 failures using
real queued send items, cumulative heads, repeated heads and late SACKs; see
[the attribution proof](throughput-fix-2-results/window-cumulative-shared-credit-root-v4).
The [common delivery experiment](throughput-fix-2-results/window-cumulative-shared-delivery-cold-v2)
then improved the expanded matched selection
from 57 passes/15 failures to 72 passes, without race warnings. All four actual
shared-reservation cases and early-return, carrier-policy, stale, permission
and statistics controls pass. Its first new missing-history test incorrectly
required resetting an already learned window to the initial value. The corrected
test preserves the prior learned value; production and utilization thresholds
are unchanged, and the [original oracle failure](throughput-fix-2-results/window-cumulative-shared-delivery-oracle-v1)
is retained separately. These are component proofs with prescribed training.
The corrected shared consumer releases all requested bytes in about 19.2–19.9 ms
against the original 30.03–30.09 ms deadlines; the single-lane control remains
about 18.18 ms. This measures the reservation/dispatch rule after training.

The final implementation adopts that service scope with a six-entry exact
endpoint ring rather than the experiment's 64 entries. It counts confirmed H1
delivery once across sibling lanes and uses the result only when physical
serialization has no positive rate. Logical window sizing remains per lane.
The live equal/unequal, freshness, generation, read and policy roots are in the
canonical correctness selector. The final design and cost comparison are
reported in [Final shared cold pacing and pure-ACK admission](#final-shared-cold-pacing-and-pure-ack-admission).

The [adjacent prefix review](throughput-fix-2-results/service-receiver-prefix-timing-v1/contract-audit.md)
finds a separate timing defect: the newest sequence
number can arrive before a missing earlier message. Its ACK delay describes
that head alone; subtracting the delay from the entire newly credited prefix
can invent service capacity. A lawful two-route fixture reports 16 MB/s against
a 0.2 MB/s serializer, failing three repetitions while ordered and prior-SACK
controls pass six. The proposed correction keeps raw once-only delivery credit
and limits the nonzero receiver-wait correction to credit for the head itself.
The expanded matched comparison changes from 84 passes/three failures to
81 passes/six failures: the ingress defect is fixed, while two unchanged SDK
tests demand an immediate exact rate from insufficient cumulative-head timing.
Their original failures and raw refill readings are preserved. Valid single-head
timing, physical credit and eventual pacing remain required.
The [corrected contract proposal](throughput-fix-2-results/service-receiver-prefix-contract-correction-v3)
passes 60 race checks: the two cumulative-prefix
tests retain byte/drain/raw-clock bounds, and a new attributable single-head
test still requires exact short serialization. A [same-local-hop reorder proof](throughput-fix-2-results/service-receiver-prefix-local-hop-v2)
separately improves from 12 passes/six failures to 18 passes. These are isolated
source results. The exact-head restriction and accepted receiver-interval
contracts are now in the live tree. Six permanent prefix roots reproduce the
16 MB/s versus 0.2 MB/s overestimate before correction, including downstream
reorder behind a known local H1 route. The zero-delay cumulative-prefix control
requires the exact raw-clock rate. The 20 prefix, cold-refill, queued-timing and SDK timing
definitions pass three race repetitions, and 40 adjacent ACK compression and
metadata definitions pass a separate race run.

The matched live-source comparison isolates the same restriction at the
remaining 300-microsecond retained H1 device duplex cell. With the original
warmup and 90% per-direction reference gate, three exploratory runs per role
change device upload from 810.639–814.152 to 886.344–918.241 Mb/s and the
providing role from 837.734–851.077 to 864.901–931.840 Mb/s, against about
957.2 Mb/s reference. All six role comparisons fail before and pass afterward,
with no relay drops. A new permanent affected-cell test covers both roles.
The [complete source/build archive](throughput-fix-2-results/service-receiver-prefix-live-sdk-v1)
also retains a later pinned comparison: 45 race passes/15 failures become
60 passes, but the SDK selection changes from three failures to two passes
and one failure. The third non-providing after run reaches 855.296 Mb/s versus
957.2352 Mb/s reference. The six exploratory passes cannot replace this pinned
failure, which remains in the immutable archive.

The failing direction had no forward backlog, yet its held pace had fallen to
111.22 MB/s from a 101.11 MB/s service reading. The next candidate keeps a held
pace through RTT inflation that fits within one already permitted burst.
Blind discovery still ends at the original queue margin; physical backlog,
recovery and a queue beyond the burst duration still permit a decrease. An
explicit-clock root reproduces the prior drop from 137.5 to 111.22 MB/s after
one lawful 10 ms reverse burst. Adjacent controls require a longer queue and
excess forward flight to lower pacing. Five repeated SDK/congestion selections
pass. The [matched component and combined validation archive](throughput-fix-2-results/service-pacing-burst-hold-v1)
records 12 race passes/three failures before versus 15 passes after. Both SDK
arms pass all five repetitions, so this pair does not prove throughput uplift
or deterministically explain the whole earlier intermittent SDK observation.
The fresh candidate full core model passes 36 definitions and 846 readings;
its isolated device upload ratios are 95.97% and 92.24% in the two roles.
Combined correctness passes 512 race definitions. See
[the hold-release review](THROUGHPUTFIX-PR2.md#36-keep-held-pacing-through-one-permitted-reverse-burst).

The [final core regression and runner contracts](throughput-fix-2-results/core-regression-burst-hold-final-v1)
record 3,345 root passes, 25 existing skips and no failures, plus five passing
snapshot contracts and five passing server-environment contracts. The
[final scoped server archive](throughput-fix-2-results/server-scoped-configure-final-v1)
records 35 connect race passes and 28 proxy nonrace passes. The two audited
non-database modes use the original configuration function and default cgo
off; binary metadata confirms connect's race flag remains enabled. Full server
integration preflight and its user-authorized deferral remain unchanged.

Two cold-refill assertions had also equated observed service with pacing on
an unloaded path. They now require the exact 267,000 B/s service result and
adequate pacing while allowing the accepted discovery floor to send faster.
The unsupported-gap and genuine-slowdown assertions and all SDK performance
thresholds are unchanged. See [the live integration review](THROUGHPUTFIX-PR2.md#35-adopt-exact-head-receiver-timing-and-retest-short-duplex).

The [common-ring boundary review](throughput-fix-2-results/service-shared-aggregate-boundaries-v1)
also finds old physical offers leaking across a pending-drain boundary into
the warm offer bound. The narrow guard changes 15 passes/three failures to
18 passes while preserving all 27 warm controls. Cold raw history keeps its
independent qualification rules.

The [joined prefix/common-delivery source](throughput-fix-2-results/window-cumulative-shared-prefix-sdk-v1)
passed the unchanged short SDK selection; its long selection failed the
400 ms device-default cell at 51.607 versus 131.046 Mb/s. Its intervals rise
from 15.483 to 126.566 Mb/s, showing a prolonged ramp in this particular run.
The original 2.3-second warmup and throughput gate are unchanged.

A later [matched pacing experiment](throughput-fix-2-results/window-cumulative-shared-warm-pacing-v2)
includes the corrected contracts and drain guards in both arms. Taking the
greater of positive service and common delivery passes 183 race checks in
each arm, but both failed the unchanged long SDK selection at fixed
100 ms and 400 ms RTT. At 400 ms the candidate delivers 51.622 versus
131.035 Mb/s after the change. This maximum alone is not a startup fix and
has not been adopted. The following diagnostic isolated the startup pacing
limit addressed by the discovery policy in research plan section 34.

The historical 64-entry [common-ring cost comparison](throughput-fix-2-results/window-cumulative-shared-delivery-cpu-v1)
records zero allocations in all 54 benchmark readings. It adds 3,608 inline
bytes per service. Median real-head processing rises from 1,068 to 1,444 ns,
and estimation from 1,465 to 1,838 ns. Runs used the recorded concurrent host
load. This rejected storage shape motivated the final six-entry ring; its
matched structural and operation costs are reported near the end.

Two new [static long-path controls](throughput-fix-2-results/model-static-long-recovery-v1)
pass using real sender/ACK workers and pacing with a 512 KiB opening. Fixed
400 ms and 1.2-second paths reach about 30.636 Mb/s against equally fast
references, with zero measured drops or timeout resends. Their predeclared
26.24-/77.44-second warmups establish settled performance. They do not replace
the original SDK recovery gate or establish an isolated warm-bound cause.

The SDK model groups use SDK constructor settings with synthetic Transfer
traffic. They do not execute the provider's dedicated TCP ACK worker, so the
optional ACK-retention change cannot be credited with fixing those failures.

**Test-contract review is now part of the acceptance work.** Two concrete
assertions are too narrow: a global old-rate cap excludes an independently
qualified increase, and a new TCP-control test requires two ACKs even when one
newer cumulative ACK covers both. Exact rate responses after a few sibling
heads and the one-second RTT-growth gates also encode response-policy choices.
The audit in [research plan section 30](THROUGHPUTFIX-PR2.md#30-audit-test-contracts-before-further-estimator-changes)
separates these from byte ownership, protocol bounds, cancellation and the
agreed retained-window policy. Frozen proofs remain intact; performance
thresholds have not been changed.
The review also found two concrete test-fixture bugs: a provider-cancel test
shares its cancel function with the client while asserting independent
lifecycle, and a new test installs a worker hook after client workers start,
creating its own race. Correct their setup before attributing those failures
to production. The original failed/racy comparisons remain preserved.
After correcting only fixture construction, the ownership comparison changes
from 24 passes/27 failures before to 51 passes after, with no races. Its hard
caller-isolation fix has been validated independently of optional ACK retention.
The standalone comparison now passes all 24 repeated roots and 24 adjacent
checks after six passes/18 failures before. Only its shared send/forward caller
cancellation guards and standalone tests have been applied to the working tree.
A separate [frozen live-source comparison](throughput-fix-2-results/caller-cancellation-live-integration-v1)
gives the same six passes/18 failures
before, 24 passes after and 24 adjacent passes, without race warnings.
The [measurement-window review](throughput-fix-2-results/model-rtt-measurement-policy-review-v1)
also confirms that one borderline RTT run passes two-/three-second aggregation
while failing the original one-second gate; the severe losses fail either form.

The [expanded semantic audit](throughput-fix-2-results/service-receiver-semantic-audit-v1/semantic-audit.md)
also changes the interpretation of a suspected cold-estimator
failure. Zero service is valid when cumulative delivery supplies the pace.
The v2 sustained fixture passes that case; its sole repeated failure is a
held positive rate that overrides fresh own delivery and keeps pacing at
950,000 B/s versus 2,823,529 B/s observed delivery through 384 cycles. All three
repetitions reproduce the warm hold, while cold, fast, ordered, legacy and
mixed-feedback controls pass. However, v2 exceeds one lane's opening window
and models an oversized physical item. The v3 correction also consumes an
admission estimate before every offer; it gives 18 race passes without changing
production. The oversized v2 observation is withdrawn as production proof.
Legal single-frame controls also pass. A subsequent matched legal cumulative
group test isolates read ordering: three per-refill-read controls pass, while
three batched-read cases retain the same 1 MB/s service and 950,000 B/s pace.
The [common bounded delivery candidate](throughput-fix-2-results/service-shared-aggregate-warm-v1)
fixes that narrow case: its matched
selection changes from 24 passes/three failures to 27 passes without races.
These are estimate-consumer tests with prescribed input, not closed-loop
throughput. Their per-lane passing
criterion also needs an aggregate-service check: 3.157 MB/s pacing can exceed
each lane's share while still using only 79% of a shared 4 MB/s serializer.
An earlier consumer fixture lacked the required cumulative history and is
retained as invalid precondition evidence.

The first isolated per-sequence interval candidate does not yet fix the legal
batched case: the corrected matched selection remains 12 passes/three failures
before and after. Its prefix, causal-order and tied-clock controls pass. A
separately proposed old-rate cap was rejected because its input also permits a
genuinely faster path and preconfirms a future write; that failed proposal is
preserved without changing production to satisfy it.

The same [semantic audit](throughput-fix-2-results/service-receiver-semantic-audit-v1/semantic-audit.md)
now includes a legal rolling-flight slowdown comparison: nine passes/three
failures before, 12 passes after, with no race warnings. It uses legal physical
messages and checks each current admission window. After spending the shared
startup probe, an actual reservation/release diagnostic changes the batched
case from about 13.75 MB/s to 3.831 MB/s against 4 MB/s delivery. This supports
the isolated decrease correction; prescribed feedback still does not certify
closed-loop throughput or the combined implementation.

The latest [physical ACK-policy experiment](throughput-fix-2-results/physical-h1-ack-policy-experiment-v1)
also exposes a stale test assumption: final `Sized=false` does not mean the
configured policy is wrong when service evidence or retained capacity applies.
Its generic failure label includes recoverable provider-control refusals; no
separate teardown failure appears. Both physical runs remain uncalibrated and
underperform their references. This audit does not establish throughput closure
or justify adopting the optional ACK policy.
The smoke fixture's policy assertion is now corrected to inspect resolved
constructor and actual sequence settings. Eight deterministic contract tests
accept service-qualified and aged retained states while rejecting misconfigured
arms. The [isolated comparison](throughput-fix-2-results/physical-window-policy-gate-contract-v1)
improves from three passes/21 failures to 24 race passes. On the
[adopted live source](throughput-fix-2-results/physical-window-policy-live-integration-v1),
policy/caller integration passes 16 checks
and repeated policy coverage passes another 24; physical goodput has not been
revalidated by those unit checks.

**The sustained-slowdown correction passes 507 scoped race checks; core
performance acceptance remains open. Explicit RTT changes await the signal
phase.** Interleaved
receiver waits can invalidate all 64 exact intervals while a real standing
queue delivers 4 MB/s. The correction bounds service over a full continuous
feedback interval using its raw span minus the largest validated receiver wait,
counting every byte. This conservative bound can lower an established rate;
it cannot establish cold service or raise capacity. The corrected matched
comparison improves from 18 passes/three failures to 21 passes, with unchanged
short, unqueued, refill and buffered-peak controls. The original invalid
fast-control fixture remains separately recorded in
[the slowdown correction](throughput-fix-2-results/service-receiver-invalid-slowdown-v1).
The complete earlier candidate has 502 scoped passes/one failure, the same
slowdown root, in [the full scoped baseline](throughput-fix-2-results/correctness-receiver-service-v1).
Its complete nonrace regression ends with **3,334 passes, one failure and
25 skips**; the sole failure is again that slowdown root. Preserve all outcomes
in [the complete regression baseline](throughput-fix-2-results/regression-receiver-service-v1).
The corrected candidate's focused RTT-growth run loses throughput at **48.995
versus 95.771 Mb/s**. Its full model is now terminal: **25 passes/five failures**,
retaining all 824 readings in
[the complete slowdown-candidate model](throughput-fix-2-results/model-receiver-invalid-slowdown-full-v1).
The failures are SDK initial-feedback duplex recovery, SDK profiles, SDK duplex,
RTT growth and shared finite relay. Full-run RTT growth is 5.612/95.771 Mb/s;
the shared-relay 100 Mb/s cell is 72.684/95.616 Mb/s without drops or retries.
These are isolated sources, not an accepted combined fix.

An [offline endpoint review](throughput-fix-2-results/model-endpoint-pacing-review-v1/review.md)
finds positive service in all 18 directions below their paired reference gate,
and in the severe RTT-growth failure. The shared cold fallback fix therefore
needs separate warm-hold recovery work and full affected-cell validation;
the final snapshots alone do not establish each failure's cause.

Integration review also found an
accidentally omitted cumulative-delivery pacing fallback. Its twelve unchanged
roots, repeated three times, improve from 24 passes/12 failures to 36 passes
after restoring that previously accepted behavior. The restored source
`28e69ad2` has five passes/one RTT-growth failure in the targeted model; the
fallback remains restored. Preserve this comparison in
[the cumulative fallback restoration](throughput-fix-2-results/service-receiver-cumulative-restoration-v1).
Separately, the actual-coalescer slowdown test keeps 12.5 MB/s while a queued
serializer supplies 4 MB/s: all 64 paired intervals become invalid and fresh
evidence never replaces the held value. It fails three times; the identical
legacy and mixed-metadata controls pass six times. This independent root is in
[the interleaved slowdown proof](throughput-fix-2-results/window-receiver-interleaved-slowdown-root-v1).

The preceding consumer correction on frozen source `f305b789` passes all six targeted
nonrace throughput tests. Its matched race comparison improves ten roots,
repeated three times, from 12 passes/18 failures to 30 passes; the wider
selection improves from 194 passes/six failures to 200 passes. The correction
keeps receiver-adjusted intervals through cycle start, pending rate changes,
queued averaging and retained cycle summaries. Both comparison arms include
the same corrected-endpoint bounds helper. Results and the initial incomplete
root selector are preserved in
[the receiver-clock consumer comparison](throughput-fix-2-results/service-receiver-consumers-bounds-v1).
Four earlier mixed-feedback tests were absent from both copied sources; their
separate supplement now passes all twelve executions in each arm, retained in
[the mixed-feedback supplement](throughput-fix-2-results/service-receiver-mixed-supplement-v1).
That source's full model is terminal: **26 passes/four failures**, retaining
all 824 numerical readings in
[the full receiver-consumer checkpoint](throughput-fix-2-results/model-receiver-consumers-full-v1).
Failures cover SDK first-feedback duplex recovery, SDK profile and per-direction
duplex sweeps, and RTT growth (**19.558 versus 95.771 Mb/s**). Its omitted
cumulative fallback also prevents adoption. The restored complete-source
baseline finishes with **27 passes/three failures**: SDK profiles, SDK duplex
and RTT growth. Its RTT total is 91.655 versus 95.771 Mb/s, but the first
measurement interval fails at 83.415 versus 95.764 Mb/s. Severe SDK directional
losses remain, including 16.343 versus 749.855 Mb/s in the provider-H1
300 microsecond/eight-flow cell. All 824 readings are preserved in
[the restored full model](throughput-fix-2-results/model-receiver-restored-full-v1).
The slowdown correction's complete model above remains separate; the focused
six-gate pass does not establish consistency.

Physical H1 duplex now has a deterministic admission root. A dedicated TCP
worker generates a 52-byte pure ACK while both pre-sequence admission slots
are occupied and resend capacity remains open. After a slot returns, the ACK
never reaches Transfer without another inner TCP event. Both constant and
delivery sizing reproduce this in each of three repetitions; available-slot
and public zero-wait controls pass (six passes/three failures). See
[the admission proof](throughput-fix-2-results/physical-h1-control-admission-root-v1)
and [the bounded first-refusal trace](throughput-fix-2-results/physical-h1-first-refusal-diagnostic-v1).
Retaining bounded ownership on that dedicated ACK worker is a proposed liveness
improvement and must honor its flow cancellation. Ordinary TCP can recover
a refused control ACK; this result does not prove stream corruption or turn
the failed physical calibration arms into usable throughput comparisons.

The preceding source `ddef3374` also passes the six targeted gates, but its
completed full nonrace model has **28 passes and two failures**, with all
824 numerical readings retained in
[the full receiver-refill checkpoint](throughput-fix-2-results/model-receiver-refill-full-v1).
The 50 ms-compression window-reduction cell delivers **3.953 versus
4.543 Mb/s**, with zero drops. The 300 microsecond, single-flow SDK duplex
cell delivers **804.833 versus 957.235 Mb/s in one direction**, despite higher
aggregate throughput. Its RTT-growth test passes at 95.771 Mb/s for both
arms. The separate focused pass retains all 32 readings in
[the combined receiver-refill checkpoint](throughput-fix-2-results/model-receiver-refill-combined-v1);
it is insufficient for acceptance.

The preceding warm-only receiver-delay candidate records four passes and two
failures on frozen source `626dc7af`. The 50 ms compressed-ACK
receiver-window reduction delivers **3.618 versus 4.516 Mb/s**. Long SDK startup
delivers **92.959 versus 483.891 Mb/s** at 100 ms RTT and **33.677 versus
131.045 Mb/s** at 400 ms RTT. RTT growth, both capacity-change controls and
short compression pass. All 32 readings are retained in
[the combined receiver-interval checkpoint](throughput-fix-2-results/model-receiver-interval-combined-v1).

Receiver ACK metadata helps: subtracting the change in receiver waiting time
from ACK arrival spacing fixes the new warm serialization roots (three passes/
six failures before, nine passes after under race). Exact timing-credit controls
also improve from 18 passes/nine failures to 24 passes/three failures; the
remaining repeated failure is the short cold SDK train. These local proofs do
not resolve the full-path failures. A later trace finds that the corrected warm
pair expires while cycle-start and cycle-completion rules use different delay
allowances. Its new rolling-refill comparison changes three passes/three
failures to six race passes; the genuine slow-serializer control still adapts.

A subsequent isolated cold qualification candidate passes the affected short
SDK sweep and the 100/400 ms long-path cells, plus 39 repeated root checks and
93 adjacent race checks on inputs `231b6d23`. The combined candidates above
include this correction and the rolling-refill change. The subsequent ordering
review covers sibling replies that reorder corrected endpoints within one
raw bucket, including delayed interior points that remain valid. Broader
validation must preserve both rejection of invalid intervals and recovery from
later valid evidence.

The first retained-only version fails
the full model: 25 passing / 5 failing tests before, 20 passing / 10 failing
after. In particular, an opening limited to 2 MiB cannot supply a long enough
cumulative-delivery history to grow promptly on a 400 ms path. Merely preventing
shrinkage is insufficient.

The revised candidate also permits growth from qualified serialization rate
and window residence. A receiver advertisement remains permission, not a
capacity measurement. Its isolated growth root changes from three failures
to three passes; all 33 retained-policy executions pass. The combined candidate
passes all 108 static window-mismatch comparisons and the 36-cell deterministic
performance matrix. The unchanged long
compressed-feedback cell improves from 71.619 to 95.846 Mb/s, matching the
95.846 Mb/s reference. The first full combined model is terminal: **28 passing
tests, 3 failing tests, 832 numerical rows**. Its failures are changing receiver
windows, SDK profiles and the 1.2-second RTT-growth case. Independent
cold-start and successful-drain sampler corrections pass their root/control
comparisons. A subsequent isolated qualification correction also passes all 42
focused executions for compressed window gaps, buffered carrier reads and
adjacent conditions. Its broader selection improves from 146 passes/8 failures
to 153 passes/1 failure; the remaining short-compression failure uses the older
sizing core. Combined throughput validation remains open.
The subsequent combined sampler/cumulative-pacing selection records **13
passes, four failures and 312 numerical rows**. Four failing tests cover three
remaining model families: changing receiver windows, longer SDK paths and RTT
growth; the focused long SDK test repeats cells from the original SDK sweep.
No `NetworkQualityChanged` API, wire message or device hook is included.

The earlier gap-qualified six-gate selection improves RTT growth: **95.771 versus
95.778 Mb/s** without race instrumentation, and **94.816 versus 95.778 Mb/s**
under race. Every measurement interval passes. Both variants record four
passes and two failures on identical frozen inputs `0bb47657`: the remaining
failures are the 50 ms compressed-ACK window reduction and long-path SDK
profiles. Capacity-change and short-compression controls pass. All 64 readings
are retained in `model-service-gap-combined-v1`. This source precedes the
writer synchronization and empty-ring provenance follow-ups below; it does
not establish full combined acceptance.

The independent stats/writer synchronization correction has deterministic
before/after proof: both pointer races fail in each of three fresh processes,
then all nine corrected checks and 60 adjacent checks pass. The new empty-ring
queue-provenance correction passes all 12 repeated roots/controls, including
the abandoned-drain case that rejected its first version. Its isolated wider
selection records 162 passes and one known older-core failure. Full combined
validation of these later edits remains open.

The subsequent combined correctness checkpoint is terminal: **459 passes,
two failures, no race warnings** on source `a02246e3`. It includes the newly
completed ACK-phase root before that root's correction. The other failure is
the retained-window occupancy assertion, reproduced three times at a 0.20
ratio. The initial comparison used an earlier nonrace binary and was invalid
for source attribution. With race instrumentation matched, the earlier sampler
also fails three times at 0.21; no sampler regression is established by that
comparison. This wall-clock fixture's placement in the scoped race selection
was accidental: its policy rename added it to the retained-root prefix. The
matched four-arm comparison gives three failures for each race binary and
three passes for each nonrace binary; current nonrace occupancy is 0.47–0.48.
The runner restores this unchanged performance assertion to its original
nonrace regression scope. No threshold or production code changes for this
fixture. `correctness-retained-sampler-v4` preserves every original outcome and
all 12 service readings. The complete instrument-controlled comparison is in
[`occupancy-instrumentation-matrix-v1`](throughput-fix-2-results/occupancy-instrumentation-matrix-v1);
a new combined correctness result is still required.

The first combined race selection records 416 passes and three failures: the
two known sampler roots and the static single-owner check. The latter found a
diagnostic helper reading a window-setting ingredient outside its estimator
owner. Its correction passes the already-read limit into that helper; the
ownership assertion is unchanged. The corrected comparison remains separate
from this frozen failing run (`correctness-retained-service-v3`).
The source-pinned ownership follow-up records 81 passes/3 failures before and
84 passes after across the same 28 tests repeated three times under race.
Only `transfer.go` changes; the ownership test and allowed-owner lists remain
identical. Full root regression on that tested source is terminal: **3,252
passes, six failures and 25 skips**. Two failures are the sampler roots already
present in this older snapshot. Four assert the superseded shrink policy or
equate the effective retained window with its instantaneous candidate; their
arithmetic, clamp and numerical occupancy controls are preserved by the
revised assertions, which pass all 12 repeated nonrace executions on the same
production source. An intermediate added pacing assertion failed because its
gateway fixture uses an unknown carrier and never invokes the H1 pacer; that
failed attempt is retained, and this fixture is not pacing evidence. These
results do not include the later sampler or SDK fallback fixes.
`regression-retained-service-v3` retains all 3,283 outcomes and its numerical
readings on the original source.

The combined model includes one additional four-cell growth test that duplicates
cells from the complete mismatch matrix. Future full runs use the complete
matrix once; the smaller test remains available for focused comparisons. Two
negative controls use the explicit bootstrap conditions described below. This
combined run is not an isolated comparison of the sizing change alone.

The hybrid keeps exact receiver ACK-delay measurements, a separately proved
unloaded RTT baseline, and the existing pacing/recovery limits. Service bytes
now reach the estimator at ACK arrival, even while a sender waits in the pacer.
The first retry clock starts at physical dispatch; confirmed shared raw RTT
may extend an ordinary timeout only after proven-loss checks.
These hybrid, service-credit and first-copy recovery changes are committed in
`8cda0523`. The independent follow-up in `272f95e5` also starts each successful
paced H1 retry's next backoff at that retry's physical write, preserving the
original lifetime. Commit `17780670` independently retires pre-proof delivery
history after an exact unloaded-RTT increase; its 11 roots pass three times
under race on top of `272f95e5`, without the experimental drain changes.
Drain and service-estimator investigations remain separate
working changes; their functional passes do not establish performance acceptance.

| Gate | Latest recorded result |
| --- | --- |
| Static window mismatch | All 108 comparisons pass on service-credit source `8802ba4b` |
| Finite shared relay | 1, 100 and 1,000 Mb/s cells pass three times under race |
| Earlier full correctness | 338 race passes with the proved-RTT window-history correction on effective inputs `4219e5c5`, before the new failure assertions |
| Expanded correctness, lifetime v6 | 371 race passes and five service-root failures; this snapshot precedes the final contract-renewal correction |
| Expanded correctness, lifetime v7 | 377 race passes and six service-root failures, including the deterministic SDK cold-service root; no network-quality or retained-window change in this snapshot |
| Expanded root regression, lifetime v3 | 3,203 top-level passes, five service-root failures and 25 skips; this snapshot precedes the final route-write and cancellation corrections |
| Scoped recovery correction | 99 focused race passes; original loss and lifetime bounds retained |
| Retry clock follow-up | 138 focused race passes; four deterministic before failures each reproduced three times |
| Unprovable drain correction | Integrated after 621 focused race passes; original silent-lane case passes three times |
| Full performance model, original sampler | 24 passes and three failing tests; all 818 readings retained after the window-history correction on source `922fd96d` |
| Latest scoped correctness | Complete receiver-clock source has 502 race passes/one slowdown failure; the bounded slowdown correction has 507 race passes, no failures |
| Latest root regression | Complete restored receiver-clock baseline: 3,334 passes/one known slowdown failure/25 skips; this precedes the bounded slowdown correction |
| Latest combined performance | Complete slowdown candidate has 25 passes/five failures; restored baseline has 27 passes/three failures. All 824 readings per run retained; test-contract audit underway |
| Experimental receiver-clock hybrid | Narrower discovery handoff passes all 27 model tests and retains all 818 readings on effective inputs `b7e1d4b8`; the preceding version failed static mismatch and SDK bidirectional |
| Experimental receive-queue safeguard | Full model passes all 27 tests and 818 readings; new upstream-buffer roots still fail |
| New production failure cases | 33 tests repeated three times on `17780670`: 39 passes, 60 failures, no races; all twenty failing cases reproduce every time |
| Full server database integration | Deferred at this checkpoint; completed in the final server section below |

The complete static matrix retains all 216 readings. Its source precedes the
latest recovery correction; final combined regression and performance results
must be attributed to a new source snapshot.

The narrower hybrid preserves the full model's previously passing behavior.
It combines receiver service
observations with the existing unloaded-RTT proof and pacing limits, and retires
old delivery history only after a proved RTT increase. The scoped long-path
test matches its 95.77 Mb/s reference in every measured interval. The prior
prototype declared receiver evidence available after only one endpoint, which
froze ordinary discovery without a measured receiver rate. Waiting for a valid
pair restores the six 256 KiB-send-window cells and the affected SDK direction;
the complete original model now passes. It ran immediately under concurrent
work in 1,166.64 seconds, without a quiescence wait.

This remains an experimental result. Receiver observations travel through a
test-only lookup with zero added wire cost. An actual stalled Client can make
local queue drainage look like increased capacity. Withholding endpoints found
already queued fixes that deterministic root and passes eight affected model
tests, but its full matrix is separate. Buffering before the carrier reader,
bounded feedback ownership and real wire overhead remain acceptance gates.

### Root cases for every remaining failure condition

The new [failure-condition inventory](testdata/throughput_root_cases/README.md)
maps the observed failures to runnable tests and their positive controls.
Seven package files add 33 production tests. They reproduce compressed-flight
mispricing, a partial read poisoning a successful drain, initial/retry/older
record lifetime overruns, FIFO/drain/late-wake expiry, carrier-buffer bucket
placement, both short compressed-service cells, and drain liveness when retry,
carrier change or cancellation invalidates physical delivery proof. Their frozen
run on `17780670` plus tests records 39 passes and 60 failures over three
repetitions; all twenty failing tests fail each time, with no race warnings.
The working drain correction is excluded from that baseline. These failures
are open defects, not passing assertions that merely document a bad value.

The first expanded full correctness run, before the final worker-adjacent and
short-cell tests were added, records 348 passes and seven newly exposed failures.
Its earlier 338-pass result does not include the newly exposed conditions.
The earlier 22-case baseline remains archived with 33 passes and 33 failures;
the final inventory also includes the eleven previously uncommitted drain roots.

Experimental receiver failures are also checked in as a pinned replay suite.
Four variants run 432 root/control executions under race: 354 passes and 78
failures, no skips or races. They preserve the early-handoff regression,
forward-delay and peak-expiry failures, Client/carrier buffering, bucket phase,
and the rejected sender-confirmed increase's cold-start and genuine-increase
failures. The queued-endpoint variant also passes the full model, confirming
why model throughput alone is insufficient. No experimental receiver-service
consumer is accepted by these results.

### Additional lifetime roots and the SDK startup failure

The lifetime correction committed in `f903f59a` gives the send worker an independent index of
ACK deadlines. Recovery order, a younger message, service FIFO waiting and a
full route queue can no longer hide an older expiry. A queued reply can finish
or renew lifetime ownership without rewriting the original RTT tag timestamp.
An unissued retry keeps its ACK lookup published, so a reply to the first copy
can cancel that duplicate before physical dispatch. Route waits retain their
original writer timeout budget across an ACK-lifetime wake.

Adjacent tests exposed two more issues. Cancellation between pacing admission
and route publication could still write; the corrected boundary checks
cancellation again. Independently coalesced SACK and missing-contract replies
could hide the newer valid receipt; renewal now uses the latest applicable
receipt while preserving exact contract identity, once-only renewal and the
distinction between a recovery request and delivery.

The independent lifetime v6 comparison, excluding the working drain change,
records 45 passes/54 failures before and 99 passes afterward across three race
repetitions. Adding the contract-order root exposes three failures in v6;
the v7 correction passes all 111 focused executions with no races. These
results establish the lifetime correction's behavior, not service-rate or
full performance acceptance. Intermediate timestamp and equal-deadline
failures remain in `lifetime-owner-evolution`.

The broader model also exposed a 400 ms RTT, one-flow constrained H1 SDK
sender delivering 68.90 Mb/s against a 130.95 Mb/s reference. Separate frozen
before/current comparisons both reproduce that loss, so the lifetime changes
are not established as its cause. The first-ACK phase fixture forces a reply
at 205 ms and preserves the original gates, but the race run has two failures
and one pass; it is still an affected-cell reproduction, not a deterministic
root proof. The 220 ms control passes three times and the existing eight-lane
barrier control still passes. Retain all outcomes in
`sdk-initial-feedback-isolation` and `sdk-initial-feedback-roots`.

The v1 full model has 23 passing tests and four failing tests. Its ledger
contains **782 service-reading JSON records plus 36 compact model summaries**.
The SDK test exits early, so 818 total ledger rows must not be described as a
complete service-reading sweep.

The SDK startup failure now also has a deterministic sampler/pacer root.
A physically confirmed 328,656-byte opening drains at 405 ms. Its first
1,384-byte resumed reply arrives 400.011072 ms later; the sampler incorrectly
prices that turnaround as 3,459 B/s and imposes a 626 ms next-write delay.
The independent pre-fix comparison repeats this failure three times while
continuous-outstanding and fresh-pair slow-service controls pass six times.
`sdk-initial-service-root-before` preserves all nine outcomes and source pins.

### The six outstanding service-estimation assertions at lifetime v7

| Failure | Observed result |
| --- | --- |
| Compressed, window-limited flights | A window gap replaces 12.5 MB/s serialization with 0.605 MB/s |
| Buffered carrier reads across sampler buckets | A 12.5 MB/s path becomes 214.6 MB/s, increasing the burst from 125,000 to 2,146,000 bytes |
| Short paths with 50 ms ACK compression | One/eight flows each deliver 8.17 Mb/s against a 9.58 Mb/s reference; estimated service falls to 106,840 B/s on a 1,250,000 B/s serializer |
| Intermediate controller read during a successful drain | The held 12.5 MB/s becomes 62,248 B/s and survives the later exact RTT proof |
| The same successful-drain case with legacy ACKs | The same 62,248 B/s collapse without receiver timing metadata |
| Cold SDK opening drains before the resumed train | One resumed ACK invents a 3,459 B/s service estimate and 626 ms pacing delay |

All six have deterministic assertions in the failure inventory. The agreed `NetworkQualityChanged`
policy is now in the research plan: the learned window grows but never shrinks
between notifications; pacing still adapts both ways. A notification permits
qualified fresh measurements to shrink the window and requests faster rate/RTT
remeasurement. Receiver and memory bounds still limit effective admission.
Cell bars, cellular type and Wi-Fi signal bars use this soft notification.
The existing hard `NetworkChanged` hook reconnects transports and should also
request remeasurement. Provider egress changes must notify their consumers too.
A `DeviceLocal` serving both roles must affect its local clients and the
connected clients using its provider. Received hints stay peer scoped and are
never rebroadcast. Event plumbing is deferred until the core experiment passes.

### Core retained-window experiment, before event plumbing

The first candidate separates learned capacity from effective admission:

```text
cumulative sizing    = scale × delivered rate × window residence
serialization sizing = scale × measured service × window residence
qualified sizing = max(qualified cumulative sizing, qualified serialization sizing)
qualified sizing = min(qualified sizing, target rate × residence)
learned window   = max(previous learned window, qualified sizing)
effective window = min(learned window, peer / memory / configured byte limits)
pacing rate      = continuously measured service, capped by the target rate
```

The learned value begins at the configured bootstrap. A large advertisement
does not teach the full ceiling. Only admission learns growth; statistics are
observational. A temporary hard limit does not erase the learned value. The
RTT-derived target sizing term cannot silently shrink it. No quality API,
remote event propagation, or authorized-shrink episode is implemented in this
first comparison.

Six new deterministic policy tests fail 18/18 on the preceding code and pass
18/18 on the candidate under race. The unchanged short-path 50 ms compression
failure also now passes: one/eight flows each deliver 9.58464 Mb/s. The other
five sampler roots still fail. This does not establish the complete approach.

The separate unchanged 289-test comparison records 267 passes/22 failures
before and 259 passes/30 failures afterward. Fifteen newly failing assertions
encode the superseded shrink/advertisement policy. Their original outcomes
are preserved; replacements keep the exact candidate arithmetic, proof
timestamps, history qualification and hard limits while asserting retained
admission. Both variants deliberately
exclude the separate working drain correction. `window-retention-policy-v1`
contains the complete race comparison. The original full model comparison is
terminal: 25 passes/5 failures before and 20 passes/10 failures after. Six gates
newly fail and one previously failing SDK bidirectional gate passes. This
rejects retained-only sizing as a complete solution.

#### Growth must not depend on already having a large window

At 125 MB/s and 400 ms RTT, the 2 MiB opening produces a valid 16.8 ms
serialization train. It cannot fill the cumulative estimator's 800 ms
minimum history. The revised candidate can use that independently measured
service to grow within the existing peer, memory, configured-byte and target
sizing limits. A single reply supplies no rate pair and cannot authorize this
growth. Shared H1 service can qualify only current H1 lanes; unknown, H3, P2P
and mixed policies must not multiply an unrelated H1 rate by their local RTT.
Pacing continues using current service even when the learned byte window holds.

Two negative controls needed explicit starting conditions after removing the
old immediate jump to the advertised maximum. The compression-residence
comparison now starts both arms at 256 KiB: its RTT-only control delivers
202.55 Mb/s and the residence-aware arm 958.36 Mb/s. The finite-relay comparison
starts both arms at a configured 48 MiB opening, restoring actual overflow in
the unpaced arm; the paced arm delivers 958.1 Mb/s with no relay drops for one
and eight flows. Both tests pass before and after the service-growth change.
Their original warmup, duration, throughput and queue thresholds are unchanged.
The latter tests a large configured opening, including its larger startup
discovery allowance; it does not claim to measure a learned 48 MiB window with
the normal 2 MiB discovery allowance. The original failed stimuli remain archived.

The isolated race performance pair reached its 30-minute timeout during the
four long-RTT growth cells. Before completion it records 0 passes/3 failures
before and 1 pass/2 failures after; these are incomplete runs, not successful
full comparisons. The original compression and finite-relay negative stimuli
account for two failures in both variants. The combined non-race model
independently passes the complete static mismatch and deterministic matrices.
A process sample points to full-flight recovery/statistics scans as a host-CPU
concern on large windows; the research plan keeps this separate from virtual
throughput and from the sampler defects.

`send-scan-cost-v1` records 36 explicit-clock benchmark readings at 32, 1,024
and 16,384 retained messages. At 16,384 messages, healthy cumulative-only
recovery scanning costs 146–150 µs per call without race instrumentation and
1.25–1.33 ms with it. Repeating an unchanged route-stall observation costs
30.6–32.7 µs and 393.6–394.8 µs respectively. All runs allocate zero bytes per
operation and preserve recovery/diagnostic state. These functions are unchanged
from `f903f59a`; the measurements establish existing scaling cost, not a new
retention regression or a host throughput acceptance result.

The separate `service-qualification-cpu-v1` comparison retains 36 nonrace
microbenchmark readings for sampler `54b36054` versus `008e9d38`. All report
zero allocations. Full-ring estimator reads cost about 226–259 ns before and
240–270 ns after. Single-ACK publication in the 512-item fixture changes from
54.4–54.7 ns to 74.5–75.1 ns; its 8,192-item counterpart changes from
75.5–93.8 ns to 112.7–121.9 ns. The 32-message batch ranges overlap. These runs
started immediately under recorded concurrent load. The ACK publication fixture
does not populate receiver RTT history, so this comparison does not establish
that additional path's cost or full host throughput.

`service-receiver-credit-cpu-v1` fills that measurement gap with three new
benchmarks. Each measured operation refreshes a full 128-tuple receiver history
before publishing its ACK bytes, and checks exact cumulative credit afterward.
The 18 before/after readings retain zero allocations. Single-message publication
costs 448–451 ns before versus 795–802 ns after at 512 retained items; at 8,192
items it costs 493–498 ns versus 856–861 ns. A 32-message head costs 857–956 ns
versus 1,218–1,433 ns. This is a visible cost of the qualification change under
recorded concurrent load, not a host throughput result. The regular `pacing`
runner includes these cases.

The newer `service-receiver-coalescer-cpu-v1` comparison uses the real
`coalesceReceivedAck` entry point, including exact initial-H1 timing validation
and once-only delivery credit. All 27 readings allocate zero bytes, with
128 active RTT observations throughout. Current live production costs
1.004–1.027 µs for a single head over 512 retained envelopes, versus
1.053–1.164 µs for the warm receiver-delay candidate. For a 32-message head
over 8,192 envelopes, the ranges are 1.405–1.476 µs and 1.587–1.903 µs.
A third arm keeps the new timing-credit plumbing with inert sampler handling
to separate those two changes. These measurements ran under concurrent load;
they do not establish a host throughput result. Six initial fixture failures
are retained: their expected residence omitted the advertised compression
allowance; the corrected fixture separately checks raw and adjusted RTT.

The regular runner now saves these CPU measurements in its numerical ledger.
Its preceding collector omitted Go benchmark output even when the workload
ran successfully. `benchmark-ledger-preservation-v1` executes the old and new
collector programs against the same nine-row output: zero readings before,
all nine and every metric after. Four parser tests cover counters, repeated
rows, headers and malformed metrics; shell syntax also passes. The runner
copies the parser with the source and records its digest.

The full-ring supplement adds controller and statistics reads over the same
reordered receiver-clock inputs and valid, mixed and legacy controls.
`service-receiver-ring-cpu-v1` retains 48 complete comparison readings, all
with zero allocations and 64 populated slots. Invalid-ring median controller
cost is 8.611 µs before and 9.082 µs after the conservative slowdown correction;
valid-ring reads are about 0.78 µs. The returned invalid-ring rate changes
from 12.5 to 4.021 MB/s. These measurements ran alongside other work; direct
benchmark runs lack a separate per-run load/time manifest. The original empty
selector, two fixture failures and 18 partial readings remain preserved.
The internal `receiverInvalid` classification is a representation detail;
future implementations must be compared on the same physical inputs without
requiring them to keep that flag set.

The physical ledger had a second omission: its pattern recognized H1 smoke
rows but skipped the `physical-h1-duplex` prefix. Replaying one failed physical
log now retains all three arms and their censored comparison, zero rows before
and four after. `physical-duplex-ledger-preservation-v1` preserves the proof.
The extended parser has six passing tests and a passing shell syntax check;
exporting failed calibration does not make that calibration pass.

The first combined model checkpoint fails three performance gates. A receiver permission
decrease from 2 MiB to 64 KiB with 50 ms ACK compression delivers 3.413 Mb/s.
Nineteen SDK-profile cells fail their existing gates. In a short-path example,
the 512 KiB opening grows to about 1.43 MiB from cumulative delivery while
serialization is still unqualified; fallback pacing remains `Initial / residence`
at 50.8 MB/s, yielding 389 Mb/s against a 958 Mb/s reference. This is a separate
bootstrap dependency to test and correct without turning an advertisement or
one reply into measured capacity. In the long-growth case RTT has correctly
updated to 1.2002 s, but service falls to 765,123 B/s: throughput is 5.134 Mb/s
against 95.771 Mb/s. The three measured intervals all fail, so the aggregate
does not hide a successful sustained result. No original acceptance threshold
is relaxed for these failures.

#### Sampler corrections remain independently testable

The cold-start correction recognizes an actually drained opening even before
the controller has retained a service rate. One resumed ACK cannot price the
whole feedback gap as serialization. Valid unread pairs, ACK-before-write
confirmation, sibling ordering and genuinely slow fresh pairs remain controls.
The pinned comparison records 12 passes/12 failures before and 24 passes after.

The successful-drain correction preserves qualified service while a bounded
drain is unresolved and across the proof-to-dispatch handoff. A faster measured
prefix can raise it; timeout, abandonment and a fresh slow pair still permit
slowing down. Retired cycle summaries cannot resurrect an older rate. Twenty
targeted tests repeated three times improve from 51 passes/9 failures to 60
passes. The independent 126-test affected selection improves from 123 passes/3
failures to 125 passes/1 failure; its remaining short-compression performance
failure is addressed separately by retained-window sizing. All these runs use
stable copied sources under race, with no race warnings or skips.

The next correction distinguishes two misleading observations. An underfilled
rolling flight cannot price the gap before its refill as serialization. A
carrier reader draining already queued bytes cannot use its short timestamp
peak to raise service. Continuous slow pairs, queued slow serialization and
repeated slower refills still lower pacing without a notification or a global
drain. A queued increase qualifies after a complete sampler/compression
interval; genuine fast delivery can supersede the held rate.

Queue provenance is recorded at ACK arrival, before worker accounting. A later
clear-queue observation or timing-ring rotation cannot relabel the old queued
bytes as a clean fast train. Nine adjacent tests cover reordered accounting,
queue clearing, same-bucket holds, delayed accounting and both directions of
real service change. Together with five existing roots/controls, the pinned
race comparison improves from 21 passes/21 failures to **42 passes**. The wider
154-test selection improves from 146 passes/8 failures to **153 passes/1
failure**. Only `transfer_window_pacing.go` differs among the 820 copied inputs;
`service-qualification-correction` retains both sides. Its sampler hash is
`008e9d380488eda2912ca1eed937020df4bb143cfbb9c22c1a37cd583d9ce268`.
This comparison keeps the preceding sizing core, so its remaining
`ShortCompressedServiceKeepsMeasuredCapacity` failure is not a combined result.

An earlier qualification candidate rejected buffered peaks but also delayed
genuine capacity recovery. The final isolated comparison preserves the
original capacity-increase and settled-large-message controls and passes them.
The SDK fallback and old-bucket insertion corrections have separate proofs
below. The qualification comparison does not validate those later edits or
their combined throughput.

The old-bucket boundary now has an independent correction. A legacy worker may
publish an old queued RTT after newer ACK bytes have advanced the service ring.
Without the same retention check used for bytes, the modulo insertion erases
a current slot. The correction keeps the valid RTT observation but discards
its retired service-bucket marker. Exact 64/65-bucket eviction edges and the
63-bucket retained control run in both timing modes. The pinned race comparison
changes from three passes/three failures to six passes; the wider 156-test
selection changes from 154 passes/two failures to 155 passes/one failure.
Only the older-core short-compression failure remains. Evidence is in
`service-queue-horizon-correction`; combined performance remains separate.

#### Separate fresh serialization from propagation and refill gaps

Two actual-worker traces exposed adjacent qualification errors. After RTT
growth, a fresh fast train was averaged together with the preceding propagation
silence because the new residence enlarged the allowed gap. With a reduced
receive window, a modest queue indication disabled the limited-flight guard,
allowing a refill gap to masquerade as slow serialization.

The correction first checks whether a contiguous fresh train already supports
the held service before crossing an older silence. Inclusive endpoint bytes
provide only a phase-tolerant upper bound for that decision; the published
rate still excludes the first endpoint. Flight occupancy remains independent
of a modest queue indication. Sustained slower trains and genuinely queued
slow refills still lower service without a notification.

Ten roots and controls repeated three times improve from **24 passes/six
failures to 30 passes**. The isolated 160-test selection improves from 157
passes/three failures to **159 passes/one failure**; its remaining
short-compression failure uses the older sizing core. The unchanged capacity
recovery controls pass. `service-gap-qualification-correction` retains the
complete comparison.

The combined follow-through uses both race and nonrace binaries from the same
frozen inputs. RTT-growth throughput recovers to the reference in all three
intervals. The window-reduction cell still delivers 3.413 Mb/s against a
4.588 Mb/s nonrace reference. The 100 ms SDK cell delivers 97.260 versus
483.912 Mb/s; the 400 ms cell delivers 51.607 versus 131.048 Mb/s. These
remaining failures prevent acceptance. The first lost sample is now protected;
that does not prove every later feedback ordering is correct.

#### Synchronize writer publication with statistics

An actual-worker diagnostic also exposed a pre-existing race: statistics copy
the route-writer interface while the send worker publishes or clears it.
A leaf mutex now protects that copy and those stores. Route opening, closing
and policy calls execute outside the mutex, so a retained route writer cannot
hold statistics behind external retirement.

The tests rendezvous immediately before the conflicting accesses, after the
external selector operations. Both before and after sources contain identical
nil-by-default test hooks. In three fresh processes, both original races fail
every time: **three passes/six failures before, nine passes after**, with no
after race warnings. Another 60 related checks pass. The retirement control
holds a real writer reference until statistics complete. The earlier broad
start barrier missed one teardown reproduction and remains archived as an
insufficient deterministic proof in `window-writer-statistics-owner-v1`.
`window-writer-statistics-owner-v2` records the corrected proof.

Source comparison finds the same unprotected publication and policy methods
in committed `f903f59a`; no historical binary was tested for this attribution.
These results validate writer ownership only, not throughput acceptance.

#### Keep queue provenance when an RTT reset empties the byte ring

A confirmed drained probe can retire every old byte bucket while retaining
the measured service rate. If new queued RTT observations arrive before byte
accounting, the empty ring previously discarded their queue markers. Later
clear RTT tuples could then evict the evidence before the corresponding bytes
were applied, inflating a 12.5 MB/s service to 200 MB/s despite exact byte
accounting. The deterministic root covers legacy and receiver timing.

The first queued timing now initializes its marker even in an empty ring.
A marker alone is not byte evidence for a cold-start reset: that decision
also requires credited delivery or physically proved bytes still awaiting
accounting. Without this distinction, the first candidate incorrectly let an
abandoned drain affect a later unrelated probe. The unchanged abandoned-drain
test rejects that candidate.

The expanded four-test comparison records **nine passes/three failures before,
nine passes/three different failures on the rejected version, then 12 passes**.
The wider 163-test comparison records 161 passes/two failures, the same totals
with the substituted regression, then **162 passes/one failure**. Only the
known older-core short-compression failure remains. All six race runs and the
rejected candidate are retained in `service-queue-epoch-correction`.

#### Qualified cumulative delivery can replace cold pacing

The short SDK failure has a separate cause: cumulative delivery qualifies
window growth, but no physical serialization pair exists after fully drained
flights. Continuing to use `Initial / residence` then caps pacing at the small
opening despite fresh rate evidence. The candidate publishes a separate
`DeliveryByteRate` from the qualified cumulative interval and finalizes pacing
after that interval is known. It applies the existing ten-percent discovery
margin and target cap. A positive service measurement remains authoritative.
Retained bytes, advertisements, one reply, tiny control traffic and stale or
pre-permission-change checkpoints cannot supply this fallback.

Nine deterministic tests repeated three times improve from **21 passes/six
failures to 27 passes**. In a separate unchanged SDK subset, all eight affected
short-path cells now pass; the long-path test still fails. Both sides use the
preceding `54b36054` sampler, isolating this fallback change. The longer failures
already have positive service and need their own correction.
`window-cumulative-pacing-roots-v1` and `window-cumulative-pacing-sdk-v1` retain
the causal proof and all 40 before/after SDK readings.

The separate carrier-scope correction permits this fallback only for an
independent local service or a currently H1-only route policy. Unknown, H3,
P2P and mixed policies cannot contribute their logical delivery rate to the
shared H1 fallback. Twelve roots repeated three times improve from **33
passes/three failures to 36 passes** on the same preceding sampler. Positive
shared service remains authoritative, and each H1 sibling owns only its own
cold cumulative fallback. `window-cumulative-pacing-carrier-scope-v2` preserves
this attribution proof; it is not an actual non-H1 throughput measurement.

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
existing physical waiter timestamp is now integrated after 138 focused race
passes. It requires a fresh timestamp owned by this sequence and a successful
actual H1 write; unpaced, failed and changed-carrier attempts retain their
existing policy. The next 600 ms interval now expires at 1.8 s in that root.
The configured maximum and the message's original absolute ACK lifetime remain
unchanged. Two first-copy deadline sites also reconstruct the client's
monotonic clock instead of losing it through a wall-time conversion. The final
before selection records 12 passes and 12 failures, with no races. The
integrated
first-copy correction separately passes all 305 correctness checks under race
on source `44459e42`.

An adjacent lifetime check confirms that an initial physical write at 400 ms
still expires at its original 500 ms ACK lifetime, even with recovery scheduled
at 700 ms: the send loop has an independent lifetime wakeup. No additional
initial-recovery clamp is needed. A separate older defect remains open: when
pacing itself lasts 600 ms against a 500 ms lifetime, both current and
pre-physical-anchor sources write and retire at 600 ms. Each fails three forced
executions; the in-lifetime controls pass six. Any correction must bound the
local wait while preserving reliable-lane recovery exceptions and avoiding
per-write timer/context allocation.

An isolated timer-reuse candidate fixes the current message's initial, FIFO,
controlled-drain, retry and late-dispatch waits. Those roots fail fifteen times
before; 129 focused race executions pass after. The required adjacent review
keeps it unaccepted: a younger message sent at 100 ms can occupy the worker
until 600 ms, hiding an older record's 500 ms deadline. Both baseline and
candidate fail that schedule three times. Its full correctness selection has
334 passes and two failures in manual fixtures that omit the real producer's
`ackTimeout`; those fixture preconditions also need completion. The initial
unlocked FIFO-test read produced one race and is retained separately from the
corrected fixture. No lifetime-candidate source is integrated; all results are
archived in `pacing-lifetime-candidate-review-evidence`.

The silent-lane failure now has a virtual-time reproduction with the original
20-second impairment, 3,000-message workload and acceptance checks. The
checkpoint and the retry-clock follow-up each fail three times at message
1,445. A terminal-only trace shows the logical byte balance already delivered,
but one retransmitted physical tail remains ambiguous. The pacer starts a
40.512-second drain which that tail's ACK cannot prove complete; the caller's
30-second admission limit expires first. An isolated correction abandons
unprovable measurements without marking the relay drained or raising the RTT
floor. It passes the original case three times, recovering in 5.13 seconds
after the stall. The integrated implementation passes 621 focused race checks,
including eleven new tests for retry/carrier/cancellation transitions, fresh
proof, repeated invalidation and an unrelated live probe. The final before
selection records nine passes and 33 failures, including the original silent
case. Missing replies and SACK-only tails still wait for the original bound;
an existing carrier-change test now expects early abandonment and retains its
unchanged RTT-floor check. Initial eligibility scanning costs about 4 µs at
1,024 retained sequences. A count maintained at the existing tail mutations
replaces that scan: one integer per service, no new per-message or wire state.
Paired CPU measurements reduce rejected-candidate admission at 1,024 sequences
from 4,025 ns to 49.55 ns, with zero allocations. The fresh full correctness
selection passes all 327 race checks on source `23fb1ed0`; full regression and
model validation are recorded separately below. Maintaining the first counter implementation
raises ordinary tail-lifecycle CPU from 72.44 ns to 110.8 ns. Reusing the prior
tail value already read under the lock reduces that to 82.77 ns; a retry/fresh
cycle costs 241.1 ns against the original scan implementation's 209.0 ns.
This measured tradeoff removes the per-admission scan and adds one integer per
service. The lookup refinement passes 24 focused race checks and adds no
allocations. Its fresh full correctness run passes all 327 race checks on
source `7c0a2f4f`, separately from the `23fb1ed0` model/regression
campaign.
The original failures and intermediate scan candidate remain separate evidence.

The full root regression on `23fb1ed0` finishes with 3,963 passes, no failures
and 27 skips. Its full model finishes with 24 passes and three failing tests,
retaining all 818 readings. The 100 ms RTT/50 ms compression shrink cell reaches
3.413 Mb/s against 4.560 Mb/s. The long-RTT transition reaches only 0.437 Mb/s
against 95.771 Mb/s, with all three measured intervals below 0.472 Mb/s.
Two service-matrix cells also miss their delivery floor: 10 Mb/s service,
300 µs RTT and 50 ms compression deliver 8.120 and 8.172 Mb/s for one/eight
flows against a 9.585 Mb/s reference. Both have zero measured drops; the other
52 service-matrix cells pass. These performance failures remain acceptance
blockers. A subsequent matched comparison isolates an additional long-path
regression to the drain change, while the two short compressed cells already
fail on the committed checkpoint:

| Frozen production variant | Long-RTT delivery | 10 Mb/s, 300 µs RTT, 50 ms compression |
| --- | --- | --- |
| `8cda0523` checkpoint | 5.270 Mb/s | Both one/eight-flow cells fail |
| Checkpoint plus physical retry clock | 5.250 Mb/s | Both cells fail |
| Retry clock plus unprovable-drain change | 0.389 Mb/s | Both cells fail |

Long-path references remain approximately 95.77 Mb/s, measured drops are zero,
and the final RTT is correctly about 1.2 seconds in every arm. The final service
estimate falls from about 1.99 MB/s in the first two arms to 56,786 B/s with the
drain change. All 11 drain-liveness roots pass in that third arm. The short
cells deliver approximately 8.11–8.17 Mb/s against 9.585 Mb/s in all three.
The comparison retains all 18 readings and nine failed pairs in
`recovery-drain-ablation-evidence`. The long test and thresholds are unchanged;
an identical two-cell filter removes only unrelated service-matrix work.
The next hybrid must preserve physical retry timing and drain liveness while
separating known local supply pauses from capacity evidence. A bounded trace
finds no active-drain abort in the captured interval. The drain starts at
5.22199168 s and completes by ACK at 6.43654832 s, but service falls from
8,795,996 to 62,248 B/s at 5.23049480 s during that pause. A queue average divides
37,422 bytes by 601.17008 ms; the later successful probe captures the already
depressed hold. Only 8.50312 ms of that measured span overlaps the controlled
pause. A later trace matches ACK credit to its actual original physical writes:
the older checkpoint covers writes at 3.997722926–3.999416510 s and the newer
37,422-byte credit covers 4.000122755–4.010011118 s. Those offers are continuous
across the path-delay change, while their ACK endpoints span 603.394 ms. The
later contemporaneous sender idle is not the offer interval for these bytes.
In this schedule the first service collapse even precedes drain admission.
The estimator prices the propagation discontinuity into serialization; sender
idle classification alone cannot reject that pair correctly.

The forced sampling schedule fails three times on both the old pacer and the
drain correction. One retaining controller read during partial replies changes
a 12.5 MB/s hold to 62,248 B/s, which the later exact 1.2-second drained probe
preserves. The same physical/ACK schedule with no intermediate read, or a
read-only statistics read, retains 12.5 MB/s; both controls pass three times on
each source. This is a pre-existing sampling/read-order defect exposed by the
liveness change, not premature abort-marker clearing. Genuine slowdown and
completed-gap controls remain passing. A production correction is still open.

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

Further receiver diagnostics separate physical serialization from logical
credit: retry copies consume link capacity even though their ACKs must not add
delivery credit twice. Using physical receiver bytes/time fixes that isolated
root, and avoids capping receiver evidence by the sender's own paced rate.
It still fails the complete shrinking-window cell. Requiring offered rate to
reach the held estimate also fails a genuine capacity decrease when the sender
offers at its existing 0.95 pacing factor; that rule is rejected.

Receiver-local 10 ms occupied buckets are also insufficient on their own.
They fix a 10.12832 ms window-idle gap at all forty tested bucket phases, but
a 2 ms source pause inside a bucket lowers a 12.5 MB/s physical burst to
9.270764 MB/s at fourteen phases. Empty-bucket zero hold does not remove idle
time inside a nonempty bucket. The candidate retains low-capacity and reverse
ACK-compression controls but remains a rejected diagnostic. The next question
is whether actual source/window wait boundaries can identify usable receiver
samples even when logical flight never reaches zero. Bucket width and rate
threshold changes do not establish that distinction.

A worker trace identifies such a boundary in the original shrink cell. A
2,671-byte write is followed by a 10.12832 ms resend-window capacity wait,
despite twenty-four queued Packs. The resumed train writes 64,104 bytes and
produces one named ACK observation. Pairing that observation with the previous
train includes 15.04296 ms of receiver elapsed time and incorrectly reports
4.261395 MB/s. The complete-flight drain epoch does not change. Test zero hold
across this actual window wait, genuinely slow trains with multiple ACK turns,
and cold startup without an established estimate before selecting a consumer.

A receiver-clock diagnostic that pairs observations only within a continuously
supplied train now passes shrink, including the 0.95-paced genuine-decrease
controls. A sender-only reset using the original ACK clock still fails the
complete shrink cell in all three runs, despite passing its isolated roots.
The stronger receiver diagnostic was then applied to the frozen drain source,
with a separate boundary for an actual controlled pause, including one whose
RTT proof is later abandoned. That boundary changes rate-sample eligibility;
it does not create empty-flight or RTT proof.

On that source the isolated candidate passes all 99 focused race executions,
including all eleven unchanged drain-liveness roots. Shrink delivers
4.683/4.547 Mb/s; both short 10 Mb/s compressed cells deliver
9.15456/9.58464 Mb/s. The six capacity transitions and cold 64 KiB control also
pass. Long RTT improves to 89.23477 Mb/s, with a 12.5 MB/s held service estimate
and zero drops, but its intervals are 91.04384, 80.87552 and 95.78496 Mb/s.
The middle interval fails the unchanged sustained-throughput gate. This remains
an unaccepted information experiment. The prototype captures the receiver's
`Client.run` ingress clock and complete outer-message length before decoding,
then records eligible H1 Packs. The sender obtains the exact named observation
through a test-only lookup. That transfer adds no modeled wire overhead and
has no production wire schema or final lifecycle/memory design. Preserve that
limitation when comparing it with the production sampler.

A buffered trace of the residual long-path failure keeps the service estimate
at 12.5 MB/s and the unloaded RTT at about 1.2 seconds. Its final controlled
pause ends near 6.433 s, before measurement, and it records no retries in
11.5–15 s. An expanded trace identifies all eight lanes at 12.214–12.268 s:
delivery-history windows are 1.55–1.66 MB while the peer and configured ceilings
are 50.33 MB and obtainable memory is 39.2–40.1 MB. Their delivery samples hold
only 1.73–1.85 MB over 2.82–2.88 seconds.
Aggregate physical writes then stop for two 100 ms trace buckets. Receiver
delivery slows after the roughly 600 ms forward delay. Pre-proof delivery
history is limiting refill after the path becomes longer, despite correct
service and path timing.

The isolated correction applies the existing delivery-history step rule when
an exact physical probe proves an increased unloaded RTT. It preserves peer,
configured, target and memory bounds; it does not assign the entire shared
service rate to every lane. The first candidate passes the long gate at
95.771307 Mb/s, equal to its reference, with intervals of 95.76448, 95.78496 and
95.76448 Mb/s and zero drops. Review then forces a delayed-confirmation case:
a newer ordinary ACK equal to the old RTT floor can already own the current
baseline. Comparing at the earlier probe ACK time incorrectly opens a new
window generation. Comparing against the latest observed evidence fixes that
ordering. The corrected selection passes 174 focused race executions, including
fixed bounds, fresh slower delivery, first unknown baseline, stale observations,
read-only statistics and repeated confirmation controls.

This small window-history correction is now independently integrated with the
original service sampler and the optimized drain accounting. Eleven deterministic
roots include a current-non-H1 carrier control. The unchanged behavior fails
18 of 33 executions; the corrected behavior passes all 33. The full correctness
selector then passes 338 tests under race on the same binary. Before compilation
adds only a zero-returning observation getter so the roots can run, with no
behavioral correction. `window-proof-port-evidence` retains both controls and
the exact source/binary identities. Production performance validation is running
separately; this pass does not include receiver service feedback.

The corrected hybrid's full matrix finishes with 25 passes and two failing
tests. The first new failure
affects all six 256 KiB send-window / 300 µs RTT / 10 ms compression cells:
one/eight flows and 256 KiB, 2 MiB or 48 MiB peer windows. Delivery falls to
194.55–194.56 Mb/s against 594.36–594.41 Mb/s references, with zero drops and
a final service estimate near 25.398 MB/s. The isolated static trace has zero
eligible receiver pairs, 133 source resets, six window boundaries and one
controlled pause. The diagnostic nevertheless reports receiver evidence as
available and freezes an earlier legacy startup estimate. This is premature
estimator ownership, not a newly measured slow receiver rate. Test a narrow
handoff that preserves ordinary discovery until a usable receiver pair exists,
then retains established receiver evidence through later single-head trains.
Scoped shrink and long-path passes do not supersede the full-matrix failures.
The other failure is the mobile/providing SDK device H1 bidirectional,
300 µs, one-flow comparison (`Upload=false`): direction 1 delivers
768.01024 Mb/s against 957.19424 Mb/s. Its
combined throughput is higher, which is why the per-direction gate matters.
That second loss needs separate attribution before claiming the startup
handoff explains both failures.

The narrower ownership change now passes the six static cells and both the
ordinary/receiver SDK controls in all three repetitions: nine top-level passes,
48 readings, on effective inputs `0bdf52d0`. The SDK reverse direction recovers
to about 957.1 Mb/s. Its isolated before receiver arm fails three times while
the ordinary arm passes. This supports the ownership correction for the
observed SDK regression too; it does not establish complete performance
acceptance. The original full matrix is now running on that narrower hybrid.

Receiver timing also has two distinct limits. A forward propagation change can
stretch an otherwise continuously offered pair; forced old/new-path controls
must cover recovery, real slower capacity and controller-read order. Separately,
`Client.run` timestamps observations after the buffered carrier handoff. A
stalled client can drain that queue quickly and inflate an apparent receiver
rate. Moving the timestamp before that handoff may remove the local queue, but
does not by itself remove buffering before the socket reader. Test those
boundaries before interpreting an observation interval as physical service.
The first sampler-level queue controls retain all outcomes: the unbounded
receiver-clock candidate has 15 passes and six failures; taking the larger
matched sender/receiver interval gives 18 passes and three failures, all under
race with three repetitions. It fixes the continuously paced queue case and
preserves own-pacing, true slowdown and rate-increase controls, but buffered
original offers plus a compressed receiver drain still inflate 12.5 MB/s to
250 MB/s. That candidate is insufficient. These explicit-clock sampler controls
are not an actual forced `Client` queue/worker reproduction.

The subsequent real-worker root uses two Clients, the production reliable
carrier handoff, a fixed 32-message queue and an explicit Client barrier. A
24-message train contains 49,358 measured bytes over 3.94864 ms of serialization;
23 frames accumulate behind the barrier. The receiver-clock candidate then
retains 2.146 GB/s and raises its burst from 125,000 to 21,460,000 bytes. The
max-span variant still retains 214.6 MB/s and a 2,146,000-byte burst. Each arm
fails the root three times and passes six unstalled/faster-path controls under
race, with no drops or retries. These are actual worker/controller failures,
separate from the earlier scalar sampler controls.

A narrower diagnostic reports whether the selected route already had the
message queued at its first nonblocking read. The receiver still counts those
physical bytes, but withholds the queued message as a rate endpoint. The same
worker roots pass nine executions and the expanded receiver/reverse-compression
selection passes 75 under race. Performance validation is pending. This proves
only the local carrier-to-Client queue boundary; earlier socket/kernel buffering
remains outside the observation and no production schema has been added.

A read-only review of sibling `server/connect` and `server/proxy` finds no direct
consumer or reconstruction of `protocol.Ack` in their production code. This is
a compatibility review, not a new integration-test pass. At this checkpoint,
their full database integration tiers remained deferred as authorized; the
sibling checkout and its local database configuration were unchanged. Fresh
deterministic server/connect
and server/proxy runner attempts after the retry/drain correction stop before
Go compilation: the default bootstrap lacks a readable local launcher owner,
and the supported unmanaged portable-resource mode cannot reach PostgreSQL.
These four setup-only attempts do not establish new server test results.

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

The retained-window follow-up also reviews all 205 Go files in `server/connect`
and `server/proxy` at `154f575c`. Neither package directly consumes the new
window/rate diagnostic fields or owns the sizing implementation; the relevant
search matches are existing ACK-compression test settings and a performance
comment. The reviewed files remained stable during this read-only audit.
`server-retained-window-source-review` pins every reviewed file. This finds no
required server call-site update, but does not establish a new server build or
runtime result. The final configured server section records their later
completion.

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

## Network-quality estimator: bounded remeasurement and fresh evidence

The estimator part of `NetworkQualityChanged` is implemented. A quality event
starts a five-second remeasurement interval. It preserves the learned byte
window and last valid service/RTT estimates provisionally; the event alone
does not shrink a window. During that interval, admission may replace the
learned window only after both service and RTT have fresh evidence from the
new generation. After five seconds, the window returns to grow-only behavior.
Ordinary sustained feedback still changes pacing without any notification.
Reading statistics cannot commit a window change.

The shared physical service owns the generation and coalesces notifications.
Another event within five seconds of the previous notification does not open
a new interval; continuous listener noise cannot keep shrinking enabled.
A new generation requires five seconds of quiet. New sibling sequences inherit
the service's original cutoff and expiry, so joining later cannot renew the
permission. A generation check also rejects an old sizing computation that
was already in progress when another sibling reset the shared service; it
cannot publish either learned bytes or an old held pacing rate afterward.

Physical first-write time decides which generation owns feedback. Old replies,
late ACK-worker publication, pending receiver metadata and cumulative prefixes
containing old writes still retire delivery and repay physical service credit,
but cannot seed the new service or RTT measurements. Legacy echoed wall-clock
tags cannot relabel an old physical write after a clock change. Reset preserves
queued messages, pending writes, FIFO reservations, byte/burst bounds and their
ownership; it adds no send allowance. The cumulative head/SACK representation
and ACK message-size limits are unchanged.

### Transition recovery and a queue that is already draining

A short-to-long path transition exposed an adjacent deadline problem. The old
300 ms recovery floor retried a first new-path write before its 1.2-second reply
arrived, making the otherwise useful RTT ambiguous. While new-generation RTT
is pending, recovery uses the configured cold floor, capped by the existing
maximum. The narrow physical-service override applies only to a first reliable
write in the new generation. Its deadline remains anchored to the physical
write; repeated signals cannot extend it. Old, copied, unknown, unreliable or
changed-carrier writes do not borrow that override. Fresh RTT restores ordinary
sampled recovery.

Removing those copies then revealed a separate throughput loss: an obligatory
empty-flight RTT probe stopped service even though the existing queue was
already draining. The cold-recovery candidate's long-path model averaged
86.207147 Mb/s against 95.771307 Mb/s reference; its first interval delivered
67.072 versus 95.764480 Mb/s. It had no measurement drops or timeout copies.
The earlier retries had invalidated the drain proof and concealed this pause.

The pacer now compares physical outstanding bytes over a complete feedback
turn, at least 40 ms. A decline larger than one already permitted burst grants
one more turn for natural draining. A stall, increase or burst-sized fluctuation
cannot renew that grace, so the existing compulsory drain becomes eligible
again. This changes when a drain starts; it preserves its cooldown, absolute
deadline and physical proof. The deterministic pre-fix fixture starts a
3.4-second stop with 26 MB outstanding and falling; the corrected fixture keeps
service running, then restores the ordinary drain when repayment stalls.

### Deterministic and changed-path evidence

The core selection contains 18 deterministic top-level tests:

| Coverage | Root tests |
| --- | --- |
| Fresh evidence, expiry and ordinary grow-only behavior | `TestWindowQualityShrinkNeedsFreshEvidenceAndExpires`, `TestWindowQualityLearnedWindowShrinksOnlyDuringFreshRemeasurement` |
| Noise, shared ownership and sibling creation | `TestWindowQualityNoisyEventsNeedQuietBeforeNewGeneration`, `TestWindowQualityPeerScopeResetsSiblingServiceOnce`, `TestWindowQualityFreshSharedServiceResizesIdleSibling`, `TestWindowQualityNewSiblingCannotRenewSharedRemeasurement` |
| Stale RTT, receiver metadata and wall-clock ordering | `TestWindowQualityRttOldRepliesCannotSeedNewGeneration`, `TestWindowQualityPendingReceiverAckCannotCrossConfirmation`, `TestWindowQualityLegacyWallClockCannotRelabelOldFirstWrite` |
| Old/mixed credit and delayed publication | `TestWindowQualityOldAndMixedServiceCreditRepaysWithoutSampling`, `TestWindowQualityDelayedCreditKeepsOriginalGeneration`, `TestWindowQualityLogicalCreditRejectsOldWorkerAndMixedPrefix` |
| Physical ownership and an in-progress sibling computation | `TestWindowQualityPreservesPacingAndLifetimeOwnership`, `TestWindowQualitySiblingResetRejectsInProgressOldEstimate` |
| First new-path recovery and exclusions | `TestWindowQualityFirstNewPathReplyUsesColdRecoveryFloor`, `TestWindowQualityColdRecoveryIsOnlyForNewReliableFirstWrites` |
| Natural drain and its congestion control | `TestWindowPacingNaturalQueueDrainDoesNotStopService`, `TestWindowPacingBurstNoiseCannotSuppressDrainProbe` |

The complete selection passes three repetitions with `-race` in 1.814 seconds:

```sh
env CGO_ENABLED=0 GOCACHE=/tmp/codex-go-cache go test -race -count=3 \
  -run '^(TestWindowQuality.*|TestWindowPacing(NaturalQueueDrainDoesNotStopService|BurstNoiseCannotSuppressDrainProbe))$' .
```

The three formerly deferred models now invoke the estimator signal at the
programmed path switch: `TestWindowPathAckTailRoundTripGrowthControl`,
`TestWindowPathServiceRoundTripChanges` and
`TestWindowPathServiceRoundTripGrowthBeyondOldRing`. Their owned, cancellable
fixture worker signals both endpoints at that boundary. Static-path and
unnotified congestion/capacity controls receive no signal. The model uses the
estimator entry directly; public notification and peer propagation are separate
contracts, not extra traffic inserted into this fixed-capacity comparison.

All three models passed the frozen natural-drain checkpoint. The long-path
case delivered 95.771307 Mb/s versus 95.778133 Mb/s reference, with minimum
flow 11.898880 Mb/s, no measurement drops and the original interval gates.
Its local log is
`/tmp/throughput-fix-2-terra-quality-natural-drain-1789612075/run.log`, SHA-256
`972ccefc957ebe95aa264d2bbbc8d9c8820cced0fb59b7cb92c7a2a499456608`.
The earlier two-of-three cold-recovery result remains at
`/tmp/throughput-fix-2-terra-quality-core-candidate-v2-1789611632/run.log`, SHA-256
`5ede224314a7d75c5742aecafa6363817a41578f7548171ac6fb534920e3240e`.
The natural-drain checkpoint preceded the final two adjacent sibling-generation
guards. The 18-root race result covers those guards. These checkpoint logs do
not replace the combined validation below.

### Final combined local validation

The frozen v3 snapshot passes the corrected focused race selection, all three
changed-path models, the 12-row correctness ledger, the 858-row full model and
the broad regression. The first focused invocation contained an extra closing
parenthesis in its test regular expression and exited before running a test;
the retained `focused-rerun.log` is the corrected passing invocation. The
production source from this snapshot is committed as `5cb64f2b`; the adjacent
P2P fixture synchronization is `14aecd8f`.

| Gate | Final result |
| --- | --- |
| Focused quality, subprotocol and P2P roots | Pass; corrected log SHA-256 `a09f5bedd01f0d6bd4e5cda87edb3eaf86da3d93ed3a2f1723271db1f429f583`. |
| Three exact changed-path models | Pass in 1.42 s, 2.97 s and 37.31 s; log SHA-256 `4c49afc66fa2f9b21cfd9358ff7735c6d00fd239dfccbe768f7ee78ff6e34dd5`. |
| Correctness | Pass, 12 rows; manifest SHA-256 `3d8df45214b7d1b6ac47be9f0500ae80e27edf5fa23a54c46cb30344bde0f11c`. |
| Full model | Pass, 858 rows; manifest SHA-256 `0877825a09de397dbe3a17cc92413f0b8e493af6065ba864c6a2d22a52c1bee9`. |
| Broad regression | Pass, 12 ledger rows and no failed or censored comparisons; manifest SHA-256 `0a394ec2ef18a97eac768875691a1db972d67f031c8a1cea97201c54302b3769`. |

The archive is
`/tmp/throughput-fix-2-terra-final-v3-1789614907`. It records starting load
averages of 6.26, 7.08 and 9.62; the model finished at 11.94, 11.02 and 10.65,
and the regression at 5.39, 8.90 and 9.86. Tests ran immediately under that
concurrent load rather than waiting for quiescence.

The only later source change was the callback-storm test described below. The
v4 final-source quality selection passes three times under `-race`, including
that root. Its log is
`/tmp/throughput-fix-2-terra-v4-focused-1789616840/run.log`, SHA-256
`18acdd7a58804ac8357ef44320d2418bc6b744a1c40ec5c4cd78220b9472b9e4`.
It started at load 15.71, 21.64 and 17.63 and finished at 15.39, 20.98 and
17.55. Because the addition is test-only, the v3 model and regression exercise
the same production source now committed on the branch.

## Public propagation and platform notification hooks

`NetworkQualityChanged` is a distinct public signal. Its listeners enqueue work
and never reconnect a healthy transport. `NetworkChanged` invokes that quality
signal exactly once before its existing hard-network listeners. Each `Client`
owns one worker and one reserved subprotocol callback for its lifetime; close
unregisters both and joins the worker.

Repeated callbacks are inherently idempotent. A client admits only one local
generation until the listener has been quiet for five seconds, the shared
physical service accepts that generation once, and the single client worker
serializes estimator resets. Exact or older peer instance/generation messages
are discarded before they reach the worker. The deterministic callback-storm
root holds a live `DestinationSendStats` snapshot across reset, dispatches 128
concurrent notifications and reads 32 public statistics snapshots. It requires
one applied generation, valid snapshots under the race detector, and exactly
one later generation after the quiet boundary. Three race repetitions pass;
frequent cell-bar, cell-type, Wi-Fi-bar or link callbacks therefore do not
repeatedly clear measurement history or race statistics readers.

The peer message uses reserved subprotocol 1 and contains only a 16-byte client
instance id and an eight-byte generation. It is carried by reliable Transfer
with ACKs enabled. Local notification fanout includes send sequences and peers
observed only on receive, which covers a provider serving remote clients. The
receiver applies the hint only to the authenticated source peer. Instance and
generation ordering removes duplicates and stale restarts, and a received hint
is never broadcast again. Known peers, remote generations and pending sends
are each bounded to 1,024 entries. A zero-timeout send refusal retains the
pending generation and retries from the client worker after 100 ms.

Adding that internal registration exposed an adjacent API leak:
`QuerySubprotocols` returned reserved id 1 to applications. Public query
answers and received query results now filter every id below 1,024, while the
internal registry still retains them for wire dispatch. Registration, decoded
query and end-to-end query tests cover both directions of the boundary.

The broad regression also exposed an unrelated observation race in an existing
three-hop P2P test. Queue delivery can precede receive accounting, and peer
delivery can precede send accounting. The production P2P files involved were
unchanged from checkpoint `f6bd8662`; the quality path was not involved. Three
forced-order roots now prove both publication edges and the owned-connection
lifecycle. P2P fixtures stop their forwarders, join send workers, close their
owned receive peer, join receivers, and only then read final counters. The
three roots plus five real fast/legacy tests pass all five race repetitions
(40 executions, 11.496 seconds). Production behavior changes only by a nil
test barrier.

### Host call sites

| Host | Quality input and behavior | Local validation |
| --- | --- | --- |
| SDK | `DeviceLocal.NetworkQualityChanged` calls the connect signal; generated Go/C/C++ and gomobile bindings expose it. | Focused SDK tests pass; generated Android and Objective-C APIs contain the method. |
| Android | Same-default-network Wi-Fi bars and power-of-two link bands, plus independent cellular bar and displayed-type callbacks. Stale callbacks cannot change the current cellular state. | Tracker roots and the actual Github debug unit-test target pass. |
| Apple | Active cellular radio type, path flags and five Wi-Fi bars. Wi-Fi is sampled every five seconds because Apple exposes a snapshot rather than a bar-change listener; the first value is a baseline. Inactive cellular subscriptions cannot perturb a Wi-Fi path. | Modified Swift parses; an iOS 17 typecheck validates the native APIs and timer. Full Xcode build remains a native-CI gate on this host. |
| Windows | Native WLAN MSM signal notifications, reduced to five bars and coalesced on the watchdog SDK thread. The first sample is a baseline; teardown clears the callback before moving its owner. | Deterministic bucket/baseline roots and MinGW syntax compilation of `EgressMonitor.cpp` pass. Full MSVC/Windows execution remains a native-CI gate. |
| Linux | One-second physical-default polling, excluding the tunnel; carrier/interface changes call the hard signal, while five Wi-Fi bars and power-of-two link speed call the quality signal. | Five dependency-free parser/classifier roots pass. Full daemon execution remains a native-Linux gate. |

Apple's public APIs do not provide cellular signal bars to this extension, so
that host uses cellular radio type plus its available path and Wi-Fi signals.
Windows and Linux currently report the native radio/link information exposed
by their existing service architecture. Transport feedback remains mandatory
on every host: a missing OS notification cannot freeze pacing or make a partial
ACK sample reliable.

## Final shared cold pacing and pure-ACK admission

Commit `f8261152` closes the two remaining deterministic losses found by the
post-quality review.

### Shared cumulative delivery prices one physical clock

The learned window is still owned by each logical sequence, but sibling H1
lanes consume one service pacer. Before this correction, each lane could have
a healthy cumulative rate while the common pacer selected only one lane's
rate. In the controlled two-lane root, only 27,648 of 49,152 bytes were
released by the 30.03 ms deadline. Equal and unequal two-/four-lane schedules
failed in the same way after a quality reset; single-lane and independent
service controls passed.

The service now records exact once-only confirmed H1 delivery in a six-entry
ring. Original offer and arrival endpoints survive cadence changes and drains.
The first arrival group's bytes are excluded from the rate numerator, and a
qualified interval spans at least `max(2 * residence, 4 * cadence)`. Permission,
carrier and quality-generation boundaries reject old evidence. Endpoint
retention is anchored to the newest non-future arrival, while a separate idle
check expires the result immediately after its freshness allowance.

This rate is used only when no positive serialization rate exists. A measured
physical serializer still controls pacing. The common history never becomes a
lane's delivered-byte history, learned window, peer permission or memory
allowance. A quality reset clears the measurement but preserves reservations,
delivered-byte ownership and the already spent opening probe.

The permanent test set covers equal and unequal one/two/four-lane service,
new siblings, independent services, statistics reads, carrier and permission
changes, quality generations, tied/reordered arrivals, cadence changes, worst
endpoint phase, unknown offers, future data and exact retention/freshness
boundaries. The combined shared-delivery and quality selection passes 294/294
race executions.

The final fixed storage is 360 bytes per service rather than 3,608 bytes for
the reviewed 64-entry shape. All measurements allocate zero bytes per
operation:

| Operation | Six entries, median | 64-entry overlay, median | Observed delta |
| --- | ---: | ---: | ---: |
| Publish confirmed delivery | 375.5 ns | 387.0 ns | +3.1% |
| Estimate common rate | 450.0 ns | 600.6 ns | +33.5% |
| Rebucket after cadence change | 98.21 ns | 573.9 ns | +484.4% |

The live benchmark log is
`/tmp/shared-delivery-live-ring6-bench-count5-20260917-013528.log`
(`2749fb2ba89e3f624144f024dc7961bb18797d7a1dbdb9384c3a23c30a13da43`);
the exact 64-entry overlay is
`/tmp/shared-delivery-overlay-ring64-bench-count5-20260917-013550.log`
(`6c5c653f563949cb534fe714fb357bbb63315679246d3116e72c77ba7d9e48bc`).
They ran immediately under concurrent host load.
Load rose during the overlay arm, so the CPU percentages are local screening
evidence rather than a deployment prediction. The inline byte counts and zero
allocation results are exact for these builds.

### A per-flow pure ACK survives bounded admission pressure

The provider's TCP worker generated the correct 52-byte pure ACK, but then sent
it through the shared regenerable-control path. If both pinned H1 admission
slots were occupied, that path refused the ACK immediately. Releasing one slot
could not recover it; both constant and delivery-sized window roots failed,
while the available-slot and public-callback controls passed.

The ACK compressor's worker now uses an explicit dedicated TCP-control mode.
It may retain its one regenerable ACK while bounded Transfer admission waits on
that flow's goroutine. Provider cancellation interrupts and joins the wait and
returns the pool object. After admission the ACK retains the ordinary control
lifetime; consumed socket data alone receives the longer post-timeout replay
lease. Public callbacks, resets, unreachables and other shared synthesized
controls keep zero-wait refusal.

Progress arriving during the wait remains cumulative. The current worker may
send the retained head followed by its successor, or a later coalescer may
replace the pending head, but the newest cumulative byte is delivered once
without requiring an inner TCP retransmission. The five permanent roots pass
50/50 focused executions, and 18/18 adjacent retry, callback and cancellation
executions pass under the race detector. The failure-before log is
`/tmp/throughput-pure-ack-current.Jxvqdv/run.log`; the focused after log is
`/tmp/provider-pure-ack-count10-schedule-20260917-011645.log`
(`1a4ee7960c9d69f4d4297f8989b9c691aa95c3d78942c1552565924d1d8fcfe7`).

This changes provider admission ownership only. ACK compression still emits
one cumulative head plus bounded oldest-first SACKs above it, absorbs pending
SACKs at or below the head, paces SACK bursts and stays within the maximum
serialized message size.

### Frequent quality callbacks cannot churn statistics

One client worker serializes reset work. Notifications inside the five-second
listener interval extend the quiet boundary but do not create another
generation; the shared service also accepts the generation only once. A local
loopback sequence participates in the local reset and is excluded from
redundant peer fanout.

The deterministic storm root overlaps 128 concurrent notifications with 32
public statistics readers. It requires one generation, valid snapshots and
one later generation only after the quiet boundary. Ten race repetitions pass
without a warning, recovered panic, nil dereference or race diagnostic. The
log is `/tmp/network-quality-storm-loopback-race-count10-20260917-013457.log`
(`53193e4728e4b5b614a2f7223aa93332ad73a4c4272521e8befec1eda0d558d2`).

The final immutable campaign was built from clean commit `f8261152` with source
digest
`a22399024dd9b3547d0e82a8a6e217a2718ce5171f2bd82a8762dc9681bec850`.
Every manifest records that revision, `dirty=false`, and the same digest:

| Gate | Result | Artifact and run-log SHA-256 |
| --- | --- | --- |
| Correctness under `-race` | 566/566 pass; 12 rows; no failure, skip or diagnostic | `/tmp/throughput-fix-2-clean-correctness-1789630306`; `88122db98ed3132fa0e55f9b55211ff8b3f4d64c3f6d38f471c0a2077ecd0e8b` |
| Full model | 39 top-level tests and all 858 rows pass | `/tmp/throughput-fix-2-clean-model-1789627905`; `b28b0ab439a3487d6fcac68dc4aba96b9f45c096f9fa69753e2d5dfc153a144e` |
| Broad regression | 12 rows; zero failures or censored comparisons; 25 existing environment/candidate-gated skips | `/tmp/throughput-fix-2-clean-regression-1789628988`; `66463b8174cd28f23d5d593a0826ea78b830cd88419d5277d5d1417723eb67e7` |
| Pacing benchmarks | 43 rows pass; every operation reports 0 B/op and 0 allocs/op | `/tmp/throughput-fix-2-clean-pacing-1789630209`; `808f64a8e659d8862c1cca7c8c7969a2a4eb0bbca1cc5630ba22d34cd7453fb9` |
| Physical H1 smoke | Four rows and one calibrated comparison pass; 91.526 versus 91.552 Mb/s, ratio 0.99972 | `/tmp/throughput-fix-2-clean-physical-h1-final-1789630390`; `22a0b77f7a10180dc879ba7e67fa7aec5cd1d00ab039b71843999837c0e7febb` |

The gates ran immediately under the existing load. Their start/finish load
averages were 8.14/9.26/9.51 to 7.14/8.14/8.94 for the model,
6.91/8.06/8.90 to 6.39/5.67/6.49 for regression, 4.61/5.30/6.31 to
4.54/5.18/6.22 for pacing, 4.72/5.12/6.12 to 4.92/5.15/6.07 for correctness,
and 4.53/5.03/5.99 to 4.85/5.07/5.98 for physical H1.

## Final configured server integration

The running local PostgreSQL and Redis environment was exercised through the
checked-in Bash environment owner with the direct Xcode compiler wrappers
needed on this host. Tests started immediately under the recorded host load.
No credential, endpoint or service override was used.

Every legitimate `server/connect` directory selected by the official top-level
harness passed. The configured package run reported:

- `github.com/urnetwork/server/connect`: pass in 3,772.499 seconds;
- `github.com/urnetwork/server/connect/perfvar`: pass in 1,402.138 seconds;
- `github.com/urnetwork/server/connect/sim-latency`: pass in 35.928 seconds;
- the `resource-bomb` test package: three tests pass under `-race`; and
- the immutable sim-latency baseline manifest: pass.

The first package-local `connect/test.sh` returned 1 only after those first three
packages passed because its unrestricted discovery entered an immutable
baseline validation fixture. The baseline README requires that evidence tree
to remain outside repository test discovery, and the official top-level
`server/test-dirs.sh` excludes it. The verifier and the one legitimate package
that followed it in the official selection were run separately.

Server commit `21acdcb5` corrects the package runner by using the canonical
selector, filtering the exact connect subtree, preserving portable caller
arguments and propagating selector failure before any partial run. Three
deterministic roots failed before the correction. On the final rebased branch,
seven harness roots pass three times under `-race` (21/21), and the official
no-test traversal compiles exactly `connect`, `connect/perfvar`,
`connect/sim-latency` and `resource-bomb`. It does not enter baseline,
evaluator, build/profile, acceptance or sibling package trees. The final
branch-validation artifact is
`/tmp/throughput-fix-2-terra-server-rebase-final-1789623466`; harness and
traversal SHA-256 values are
`cf9ecead3960400f0ace275aa2570c2119f49e32ac2a6aa7d1c7143950110963` and
`6f5b7dc78c12d4a2abd271e6bd8ff32c4067c05139edfa1c75589759fa5c2bc8`.
The multi-hour payload was not repeated at that checkpoint because all four
legitimate packages had already passed against the same product source. The
later final-source rerun below repeats it. The checkpoint payload log is
`/tmp/throughput-fix-2-terra-server-integration-1789614340/retry-direct/connect.log`,
SHA-256 `30f239b098aeae9755f2f515c77297142789493c0f2ddb6d221beb291a94211a`.
It started at load 5.06, 7.02 and 9.76 and finished at 10.32, 10.59 and
11.75. Baseline and resource logs have SHA-256
`fb9006c5b441d37e42e754ec19ecd5276ae075db7450e94bde548537da9d6906`
and `9bf011559b43dfefd4e74ef371b286b5031f6f63751c6adf4d0d0bf3d0c9014e`.

The first full `server/proxy/test.sh` run passed the product package in
403.868 seconds, then exposed a pre-existing acceptance-wrapper regression.
A later server commit had replaced the wrapper's owned logger and join path
with a foreground `timeout | tee` pipeline while retaining the two signal
roots. Process-group INT or TERM could therefore close logging and delete the
credential file before the controlled runner completed cleanup.

Server branch `throughput-fix-2`, commit `7e19ae5d`, restores an owned FIFO
logger, cancellation forwarding, interrupted-wait retry and joined cleanup.
A wrapper-only INT is normalized to TERM because a Bash background child may
inherit ignored INT; the wrapper still returns 130. Nine deterministic roots
cover group and wrapper-only INT/TERM, repeated termination, normal completion,
runner failure, logger failure and combined failure. All 27 race executions
pass. The final rebased-branch log SHA-256 is
`52ec43d33e6134d07c6462ee190fe574f114dddac5c0d99a7d5d8399e5742d89`.
The sibling scan found no other wrapper with the same foreground-tee ownership
pattern.

That checkpoint's official proxy run passed both packages:

| Package | Result |
| --- | --- |
| `github.com/urnetwork/server/proxy` | Pass in 316.690 seconds, including the formerly blocked database-backed handoff tests. |
| `github.com/urnetwork/server/proxy/acceptance` | Pass in 5.958 seconds. |

The 332-second artifact is
`/tmp/throughput-fix-2-terra-server-proxy-final-1789622033`; its log SHA-256 is
`3409fa1f46440b3e9eff31d935bb6baf8fcb5e7e3e0f85b2932dd11ade3ce31a`.
Load changed from 7.13, 8.14 and 8.99 to 5.02, 7.39 and 8.58. The server main
worktree is clean at `3a3cc698`. Both server fixes are isolated on the rebased
`throughput-fix-2` branch. Its final revision after the diagnostic-cleanup
fixture correction below is `27b7dad9`.

### Final frozen-source rerun

The complete server tiers were rerun after the final connect production commit,
without waiting for host quiescence. The official `server/connect/test.sh`
selection passed from clean connect commit `f8261152` and clean server commit
`21acdcb5`:

| Package | Result |
| --- | --- |
| `github.com/urnetwork/server/connect` | Pass in 4,055.767 seconds. |
| `github.com/urnetwork/server/connect/perfvar` | Pass in 1,437.994 seconds. |
| `github.com/urnetwork/server/connect/sim-latency` | Pass in 33.015 seconds. |
| `github.com/urnetwork/server/connect/sim-latency/evaluator/container/testdata/resource-bomb` | Pass in 0.246 seconds. |

The artifact is
`/tmp/throughput-fix-2-final-server-integrations-complete.NsStqy`; the connect
log SHA-256 is
`b01672b716faf2039e3cbe55c4a22f93133b8d05df98c8a006203702ca479c24`.
All ten frozen dependency worktrees were clean. The campaign began at load
10.01/10.19/9.77 and its subsequent proxy compile check ended at
11.37/7.03/6.04.

That first frozen proxy check found a source-pair problem before running tests.
Server commit `23135c01` reads six `DeviceLocalMemoryUsage` telemetry fields
that have not been committed in the SDK repository. They existed in exactly
four pre-existing tracked files in the live SDK checkout. SDK commit `0dd2943`
therefore could not compile server `21acdcb5`; the failed build log is retained
with SHA-256
`2d74727becf3e38b0e1fdcd59e642d728b1419cb622bfdf33e81e18cfee28854`.

For validation, those exact four diffs were applied to parent `0dd2943` in a
clean detached snapshot. The resulting SDK commit is `17a7a332`, tree
`7ba4bc886d5311a7389de05b7dcb273d46fe1985`, and patch SHA-256
`d2adcf17943a1338faaa1b65b233cd0e82b43510724e941017e127ddac9db48d`.
It did not move or modify the live SDK branch or index. Nine SDK memory and 12
proxy aggregation race executions passed before the full run. The provenance
manifest is
`/tmp/throughput-fix-2-proxy-sdk-coherent-t00xfna4/manifest.json`, SHA-256
`88bf2a1049b7f843dba381a2a0f7f57b28e2d64cebd15df8b044dee3d67de18d`.

The first official `server/proxy/test.sh` execution returned zero from the
clean server and SDK snapshots:

| Package | Result |
| --- | --- |
| `github.com/urnetwork/server/proxy` | Pass in 337.806 seconds, including the database-backed handoff tests. |
| `github.com/urnetwork/server/proxy/acceptance` | Pass in 6.003 seconds. |

That execution is rejected as final evidence. Its retained log contains one
recovered nil-pointer panic, even though both Go packages reported PASS. The
record appears during `TestProxyClientReapSurvival`, but its stack belongs to
an asynchronous worker leaked by the earlier
`TestProxyDeviceMemoryBudgetReleasedOnDeviceClose`. That test published a
partial `ProxyDevice` without an SDK device, TUN, initial activity or
manager-owned context. `HandleError` recovered the worker panic and released
the reservation, allowing the root test to report a false pass.

The rejected artifact is
`/tmp/throughput-fix-2-proxy-sdk-coherent-integration-bash.38eEWy`; its log
SHA-256 is
`29d08a6712766d086d6106b8defbfd01663d7cbae1f5427187e77aaddf5c9a74`.
Load changed from 4.93/5.43/5.52 to 5.70/5.53/5.51. The checked-in environment
owner supplied the running PostgreSQL and Redis configuration; no credential,
endpoint or service override was used.

The deterministic comparison makes the false pass explicit. The original
test reports three passes while emitting three recovered nil panics. Two new
fixture roots fail 6/6 with its incomplete shape, and a wrong-parent context
control fails 3/3. The corrected fixture uses the existing initialized SDK
helper, an in-memory TUN, a current activity timestamp and `manager.ctx`.
Server commit `27b7dad9` changes only
`proxy/proxy_device_memory_budget_test.go`; production code is unchanged. The
22-test focused selection passes 220/220 under the race detector with no
recovered panic or race. The patch SHA-256 is
`742d2f9a1013c35d43bfcd921ea05b3244567e068cd7926dd03cc56e0c247d15`;
the complete proof is
`/tmp/throughput-fix-2-proxy-budget-fixture-xbmjfov6`.

The corrected official proxy run is diagnostically clean:

| Package | Result |
| --- | --- |
| `github.com/urnetwork/server/proxy` | Pass in 334.528 seconds, including the database-backed handoff tests. |
| `github.com/urnetwork/server/proxy/acceptance` | Pass in 6.132 seconds. |

It used clean server `27b7dad9`, SDK `17a7a332` and connect `f8261152`
snapshots. The artifact is
`/tmp/throughput-fix-2-proxy-budget-fixture-integration.pY4zuf`; its log
SHA-256 is
`38395dac8dca9013c13480e7f5304a4ad6d393daa50acff0c62071472a29d73f`.
Load changed from 4.55/4.85/5.07 to 5.01/5.15/5.21. The complete log has zero
recovered-panic, unexpected-error, nil-pointer, fatal, warning and race
matches. The fixture manifest SHA-256 is
`71d8ad6d629f38ffc370a80b566606b3d0d93e65da77bedbf0c27f1c2c2c5250`.

## Remaining work

The core design and the platform call sites are implemented. Preserve the
immutable archives and the earlier short-duplex failure. If that end-to-end
deficit recurs, force its physical ordering before attributing it; the matched
hold-policy SDK pair passes both arms and establishes no uplift.

1. Expand host comparisons with the corrected packet grouping beyond the
   passing physical H1 smoke. The induced source-idle replay confirms the
   estimator effect, but both throughput brackets pass and the older host
   failures remain unattributed. The TUN-corrected physical duplex reference
   still fails calibration. The deterministic full-admission root is fixed for
   both window policies, including newer cumulative progress and bounded
   cancellation; retain those invariants in any broader physical comparison.
   Keep the original gates and all failed/excluded comparisons under current
   host load.
2. Run the Apple, Windows and Linux changes on their native CI hosts. Local
   checks cover the owned classification rules and API shape, but do not replace
   a signed extension build, an MSVC service build or a Linux daemon run with
   real radio/path changes.
3. Broader physical SDK/H1 and native-TUN confirmation, longer actual-relay pressure,
   shard-collision and multiple-peer campaigns remain necessary
   before a deployment-wide claim. Continue with
   our own published fixtures as requested; obtaining the reporter's missing
   native rig is not a prerequisite for this work. Its missing traces still
   limit attribution of the reporter's specific failures.
4. Publish the six SDK memory-telemetry fields with server commit `23135c01`,
   or remove that dependency before selecting SDK `0dd2943` in a clean release
   pair. This campaign validates the exact four-file SDK patch, but does not
   take ownership of or commit the pre-existing live SDK changes.
