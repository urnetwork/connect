# Window rule on real paths: peer review and research plan

Status: research plan, 2026-09-15. The new tests and candidate fixes below are
proposed, not implemented. This review ran existing focused tests; it did not
reproduce the remote rig measurements.

## 1. Verdict

The report supplies credible evidence of a substantial regression on its
short H1 relay path, and useful evidence that receiver ACK handling contributes
to the cost of relay loss. It also confirms a single-flow benefit at 100 ms.
These results materially extend the original in-process work and justify
investigating the shipping default immediately.

They do **not yet establish which part of the window policy causes each
regression**, that the relay forward queue is the actual loss site, or that
either PR leaves the single-flow failure rate unchanged. The strongest next
step is to pin those distinctions with controlled tests before choosing a
replacement rule.

Priorities:

1. **P0: identify the permanent-hole mechanism.** TCP delivery must not silently
   stop after Transfer has relinquished responsibility for a packet.
2. **P1: separate the short-path delivery feedback from the target clamp and
   the policy's other changes.** Both can produce a small window.
3. **P1: locate relay loss and explain the eight-flow reversal at 100 ms.**
4. **P1: validate ACK recovery and its cost under sustained holes.** The PR's
   once-per-snapshot guard is not a once-per-compression-interval guard.
5. **P2: extend provider-standby tests to state transitions and H3.**

Default-off is a plausible temporary mitigation for the measured workload.
It trades away the reported long-path single-flow gain and also removes the
shared send-budget attachment and receive advertisement from newly constructed
default settings. Treat that as an explicit deployment trade. The report has
no traffic-distribution evidence supporting the PR comment that these are the
paths carrying most traffic, and it is not a basis for a universal policy yet.

## 2. Evidence and revision boundaries

Reviewed sources:

| Source | Revision and scope |
|---|---|
| `~/Desktop/Window Rule on Real Paths.html` | Saved presentation dated 2026-09-15. Its actual report is in the sibling `Window Rule on Real Paths_files/saved_resource.html` iframe. |
| [PR #213][pr213] | Head `b54f9f72bec116c0986e6c51ed13cc2f01805bee`, based on `3a59321612edfb564c049c1607883ead307fbf41`. Four commits: standby, receiver ACKs, default-off, research report. No review comments were present when checked. |
| [THROUGHPUT-RIG-REVIEW.md at that head][rig] | Published method and tables supporting the PR. |
| [THROUGHPUT-REPORT.md](THROUGHPUT-REPORT.md) | Especially §§3.23, 5–6, 8–9: in-process measurement limits, instrument calibration, compression residence, and test coverage. |
| [THROUGHPUTFIX.md](THROUGHPUTFIX.md) | Especially §§36–38, 49, 53: estimator design, delivery boundary, short-path prediction, design-point measurements. Later corrections supersede earlier claims. |
| This checkout | `d9eadf29`; nine commits ahead of its local `origin/main`. It has newer fixture repairs than the PR base. File line numbers below refer to this revision unless explicitly marked PR. |
| Sibling server checkout | `0d91e29d4fdca05fd66794a31f5a16de2d39edb2`, `../server/connect/resident.go`. Confirms a candidate code path, not what the remote relay ran. |

The saved presentation cites `REPORT-2026-09-15-UPSTREAM-WINDOW-RULE-AND-PRS.md`
and ledger P37–P44. Neither is included in the supplied HTML or the published
PR report, and the report filename was not present in the reporter's public
`beta/custom-server` tree when checked. Per-run traces, harness source, complete
binary/configuration manifests, and failing-path packet captures remain needed.
The presentation's deployed beta commit `1e34c6fc` is distinct from PR head;
do not use the labels beta, main, or option A as revision identifiers.

### What the measurements support

The same-binary on/off comparisons, alternating arms, fresh providers, binary
hashes, native kernel TUN, and published stalled runs are useful controls.
Receiver-only comparisons report consistent paired improvements: 6/6 at eight
flows and 4/4 at one. Provider startup has a separate ten-pair result.

Scope matters: this is download through an H1/WebSocket platform relay, a
custom client with a 384 MiB process budget, and a **synthetic origin inside
the provider**. It exercises a real client OS and network, but not a real
provider-to-origin kernel socket. It does not validate uploads, SDK memory
policy, mobile clients, H3, or direct paths. It complements the original
200/400 ms transfer-only tests; it does not invalidate their scoped result.

Use the on/off tables for policy effects, receiver-only pairs for receiver
effects, and the four-stack table for total user outcomes. The +18% receiver
result and +41% comparison against upstream with the policy rolled back come
from different comparisons; they are not interchangeable or additive.

## 3. Findings and falsifiable hypotheses

### H1 — Short-path loss has at least two plausible window mechanisms

**Confirmed in source:** `SendSequence.sendWindowEstimate`
(`transfer.go:10473`) uses minimum sampled Transfer RTT both in the delivery
term and in `TargetGoodputByteRate * RTT / goodputFactor`. The working floor
is 256 KiB. With sufficient memory and peer capacity, the relevant form is:

```text
delivery candidate = 2 * acknowledged framed-byte rate * R_min
target cap         = max(256 KiB, target goodput-byte rate * R_min / 0.845)
window             = delivery candidate clamped by floor, target, peer, budget
```

After an advertisement raises capacity, the delivery term waits for eligible
post-step rate history; legacy and still-silent peers take different branches.
Record that state instead of assuming the term has engaged. The rule already
uses RTT and delivery: the question is whether those measurements represent
the residence that actually limits progress, not whether RTT is consulted.

The existing report models residence as `R_min + c`, giving a window-limited
growth factor `2*R_min/(R_min+c)`. This is a useful hypothesis, with conditions:

- The shrink condition is **c > R_min**, not simply RTT below 10 ms. If c is
  approximated as 5 ms, a 5 ms RTT is the neutral boundary. Compression phase,
  packet bursts, queueing and loss can change c; the uniform 0–10 ms model is
  not an observed residence distribution on this rig.
- Network RTT of 0.3 ms is not the estimator's Transfer RTT. The latter
  includes processing and whichever ACK path and timestamp samples were used.
- Independently, at an actual Transfer minimum of 0.3 ms the 1 Gb/s target
  computes only about 44,379 framed bytes. The target cap therefore forces the
  256 KiB floor even before delivery feedback. This happens below about
  1.77 ms at that target. A floor reading alone cannot prove feedback collapse.

**Decisive experiment:** cross delivery feedback on/off with target clamp
on/off while holding budgets, receive capacity, advertisement, ACK behavior,
packetization and offered load fixed. Sweep controlled Transfer RTT and ACK
compression separately. Record every binding term and each item's residence.
Do not implement the feedback-off/target-on control by setting
`DeliverySizedWindowScale = 0`: that returns before the target calculation too.
A narrow test seam must bypass only the final delivery cap, with an assertion
that the other limits still execute. Configure each arm before workers start.

**Predictions:** feedback collapse persists with the target removed and moves
with measured c; target limiting persists with delivery feedback removed and
moves with the configured target. If neither predicts the observed window and
delivered bytes, investigate the rate sampler and another admission limit.

Do not choose a raw mean-RTT replacement solely from §8.3. The original design
explicitly rejected it because queueing created by a larger window can inflate
that mean and encourage further growth. Any residence-aware candidate must
pass a finite slow-queue test as well as a fast-path test.

### H2 — The rollback isolates a policy bundle, not just an estimator

**Confirmed in source:** `SendBufferSettings.ApplyWindowSizing`
(`transfer.go:787`) attaches a shared resend budget under the delivery policy
and clears it under Constant. `ReceiveBufferSettings.ApplyWindowSizing`
(`transfer.go:998`) enables advertisement and derives the hold/budget when
settings are constructed under the delivery policy. Constant constructors
use the older receive hold without that automatic shared-budget attachment.

At the reported 384 MiB process budget, an otherwise unmodified default
delivery-policy constructor draws 48 MiB for each Transfer direction. Constant
uses a 2 MiB send window and a 2.5 MiB receive hold. Attached SDK budgets or
custom settings can change that: log the final objects actually used.

**Hypothesis:** some of the long-path eight-flow reversal comes from the
opening advertisement step, pool contention, receive capacity, or changed
burst structure rather than steady-state rate sizing alone.

**Decisive experiment:** compare the full shipped switch and separately a
fixed-window arm retaining the same budget, hold and advertisement as the
delivery arm. Add sender-only and receiver-only switch arms. Trace the opening
burst, active sequence count, outstanding bytes, admission refusals, lendable
share, obtainable bytes and receiver advertised capacity.

Eight inner TCP flows are not automatically eight Transfer windows.
`sendSequenceId` (`transfer.go:5187`) is keyed by destination, lane and
protocol roles, not the TCP five-tuple. Establish the actual mapping on the
rig before invoking fairness or dividing a budget by eight.

**Refutation:** equal resolved limits, equal sequence topology and equal pool
pressure with the regression intact rule out those configuration explanations.
Retain mixed-version, small-receiver and never-evict guards under every arm;
turning off advertisement is not a reason to revive silent reneging.

### H3 — Relay queue loss is plausible, but has not been localized

The local server source contains the proposed mechanism: a 4096-message
default forward channel and `ForwardTimeout = 0` (`resident.go:320`, `:473`),
with nonblocking enqueue and `forwardDroppedCounter.Inc()` on exhaustion
(`:3987` onward). A successful provider route write does not prove relay
forwarding or destination receipt. The deployed relay revision, settings and
drop counter were not supplied.

The window sweep supports a burst/loss hypothesis. **Resends minus duplicate
arrivals is not a direct loss count**, however: ACK loss, reordering, multiple
retries and packets crossing the measurement boundary alter that difference.
Nor can message capacity be converted to bytes without Pack size and grouping.

**Decisive experiment:** correlate exact Transfer sequence/message identities
at provider write, relay ingress, relay enqueue/refusal, relay egress and client
ingress. Measure queue depth in both messages and bytes, service rate,
enqueue-to-write time, and both directions. Forwarding limit, cancellation,
transport replacement and other refusal sites need distinct dispositions.

Use a deterministic finite relay queue with its consumer held: fill exactly N
slots, offer N+1, release, and verify the actual drop and subsequent Transfer
recovery. Vary packet grouping at fixed byte rate separately from byte rate at
fixed grouping. Measure the real relay's counter in an owned integration rig.

**Predictions:** if the forward queue is causal, the missing identities match
its refusals; pacing or reducing message work changes queue occupancy and those
drops before throughput improves. If refusals remain zero during missing
deliveries, search the next boundary. Merely enlarging a queue can trade loss
for latency and memory; it is an experimental control, not the presumed fix.

### H4 — Delivery sampling may confuse recovery or idleness with capacity

`receiveAck` credits delivered bytes on cumulative release, while selective
ACKs retain items. The 64-entry rate ring advances at a 10 ms cadence
(`transfer.go:10123–10225`); the requested minimum rate horizon is
`max(2*R_min, 4*sampleInterval)`. Thus a submillisecond path is still sampled
over a much longer timescale, and a hole can delay cumulative credit for many
received items. The PR changes that credit's timing.

There is also a concrete coverage gap in that horizon: `deliveredRate` returns
the oldest available positive span even when it never reaches `minSpan`. With
dense samples 10 ms apart, 64 entries cover about 630 ms, less than the
800 ms requested at a 400 ms RTT. Establish whether a shorter history is an
acceptable fallback or must be marked insufficient; the present code does not
guarantee the stated two-round-trip horizon.

**Hypotheses to distinguish:** a recovery burst biases the sampled rate;
application-idle time pulls the window down despite available capacity; or
stale minimum-RTT/rate samples delay recovery after a path change. These are
suspicions, not established defects.

**Decisive tests:** controlled ACK traces just before/after sample boundaries;
one withheld head followed by selective progress and eventual repair; idle
then saturated traffic; and capacity/RTT changes in both directions. Compare
the production estimator to independently counted bytes over explicit virtual
time intervals. Record sample freshness and when the post-step horizon becomes
eligible. Include startup and dense ACKs at 400 ms, when history can be too
short. Do not assert only the estimator's formula against its own fields.

### H5 — Receiver ordering is promising; early-wake bounds need more tests

**Confirmed in the PR:** selective ACKs are sorted within each snapshot before
writing. That removes map-order inversions within a batch, which can otherwise
make unprocessed neighbor ACKs look like gaps to the sender. The reported
receiver-only gains and lower head-blocked time are consistent with this cause.
The shipped change is broader than the measured H1 path, so validate reorder
and recovery on every carrier class.

Three specific gaps in the new ACK tests:

1. **Repeated early wakes.** `Snapshot(true)` clears `gapWakeSignaled`
   (PR `transfer.go:15008` onward). Three more pending selective ACKs can wake
   the next wait immediately, with no elapsed-time limit. The default-setting
   comment's “at most one extra write per interval” does not follow from this
   implementation. Even the first snapshot can contain many individual ACK
   writes. The zero-allocation test covers steady cumulative ACKs, not a long
   hole, repeated snapshots, sorting, or retained scratch capacity.
2. **First cumulative head.** Hole-fill wake requires `hasHeadAck`. If initial
   Pack 0 is missing, selective ACKs for 1, 2, 3 are written, and then the
   first cumulative head arrives, that condition is false. It can wait for
   compression despite repairing the opening hole. All current worker tests
   prime a cumulative head first, so they do not exercise this branch.
3. **Threshold evidence.** The trigger uses `len(selectiveAcks)` even though
   head-absorbed entries are removed only in the snapshot filter. Test a head
   advance absorbing pending selective ACKs followed by new arrivals before
   the snapshot. Also test distinct later ACKs spread across snapshots: the
   sender retains evidence that the receiver's pending count resets.

**Decisive tests:** drive the actual receiver and sender through captured wire
writes. Hold one head, feed successive groups of three later items, and count
snapshots and ACK frames before one virtual compression interval elapses.
Explicitly choose an allowed recovery cadence, or correct the claimed bound
if the intended policy is unlimited per-gap progress. Measure that choice.

For sorting, arrange an adversarial ACK order and let the sender process each
prefix between writes. Assert which exact Packs are retransmitted: the true
hole should recover and its already-received neighbors should not be declared
lost merely because their ACKs follow within the same snapshot. Test snapshot
boundaries and ACK-route reordering separately; local sorting does not impose
global wire ordering across routes.

Preserve delivery-before-cumulative-ACK, plaintext/capability metadata,
contract-missing recovery, final ACK drain, blocked-write cancellation and pool
ownership. Include ACK-path loss, below-threshold tails, mismatched peer
thresholds, gap-wake disabled, and loss-free steady traffic as controls.

### H6 — The single-flow wedge is a delivery-boundary correctness problem

The report describes an idle provider and a client TCP receive hole, including
about 3.5 MB out of order. This is consistent with missing inner data, but
socket summaries alone do not identify the missing segment's last successful
handoff. `rcv_ooopack` is historical evidence of reordering, not proof of the
loss site. Healthy-run drop counters cannot exclude failed-run kernel drops.

**Confirmed in source:** `TcpSequence.Run` (`ip.go:5316` onward) packetizes
upstream bytes and explicitly relies on Transfer rather than retaining data
for inner TCP retransmission. `ReceiveSequence.flushDeliver`
(`transfer.go:14186`) ACKs after the application callback returns.
`RemoteUserNatClient.ClientReceive` (`ip.go:9488`) invokes a packet callback
without a success result, through `HandleError`. Callback return is not proof
that the client's TCP stack retained the bytes. Inspect the reporter's custom
TUN writer as well as the SDK path; its code was not supplied here.

**Decisive trace:** map inner `(flow, TCP sequence range)` to Transfer
`(sequence generation, logical lane, Pack number, message ID)` and follow it
through provider packetization, return admission, Transfer ACK/disposal,
client callback, TUN write result, kernel TCP acceptance, and inner cumulative
ACK. Dump this history when progress stops, before teardown resets counters.

Build controlled losses at separate boundaries:

- **Before Transfer delivery:** drop one Pack, including the first and a
  group containing multiple inner packets; verify exact byte recovery and
  never-evict behavior under a full receive hold.
- **During client injection:** simulate a short write, explicit error, partial
  batch and a callback that returns without accepting one packet. Establish
  whether the packet is nevertheless cumulatively ACKed and forgotten.
- **After successful TUN acceptance:** discard exactly one chosen inner TCP
  segment before TCP receives it in an owned Linux namespace. This distinguishes
  a downstream kernel loss from a callback/admission bug.
- **Reverse path:** withhold inner cumulative ACK/window updates and then
  restore them. Distinguish a missing data segment from a lost window reopen.

Use finite input with byte-content verification so the observation is not
just “NIC rate fell.” A 30 s run only proves a stall for its remaining duration;
follow through recovery/expiry with virtual time, then validate longer real
runs. Inspect per-sequence outstanding state and source TCP state together.

**Acceptance contract:** a transient recoverable loss completes the exact byte
stream; an unrecoverable failure produces an explicit bounded connection
failure. It must not be reported as successfully delivered while remaining
indefinitely idle. Budget bounds, other-flow progress, teardown, sequence wrap,
FIN/RST and duplicate suppression are part of any replay design.

Candidate directions depend on the lost boundary: retain/retry a rejected
local injection; preserve responsibility until a meaningful delivery boundary;
or retain a bounded provider replay history until inner TCP acknowledgement.
Successful TUN writes followed by kernel drops require more than retrying
failed writes. Do not add inner TCP replay before demonstrating where and why
the current reliability contract ends too early.

The statement “neither PR causes it” is too strong. Presence in every stack
shows a pre-existing failure class; it does not show unchanged frequency or
severity. ACK timing, larger bursts and grouping can change its probability.
Keep the stalled runs in user-visible goodput and completion-rate results.

### H7 — Standby classification needs a transition model

The positive startup result is plausible and independent of steady download
throughput. The PR observes typed per-attempt H1 dial errors before the strategy
flattens them, marks `DNSError.IsNotFound`, and releases standby when every
configured pin is unresolvable or held. H3 records its typed failure separately.

The new integration tests cover two misses, resolvable refused dials and one
resolvable pin. Missing coverage includes recovery after DNS becomes available,
H3, network/policy changes with an old mark, and overlapping attempt outcomes.
The observer records the last attempt to finish, which need not summarize all
currently viable attempts. Whether that matters with the actual pinned
strategy must be established before changing classification.

**Decisive tests:** use a scripted resolver/dialer and controlled group clock;
drive NXDOMAIN/NODATA, temporary failure, refused connection, delayed success,
held/unheld pins, and opposite completion orders. Assert when standby starts,
when a recovered pin takes over, that the group wakes, and that losers close.
Cover H1 and H3. Keep the reported startup group-rebuild tail (two fix runs
still near 16.6 s in the commit notes) as a separate hypothesis and metric.

## 4. Deterministic test work, in build order

Each proposed test family needs a fixed stimulus, observable transition,
independent outcome and named mutation that makes it fail. Use `testing/synctest`
for in-process time, counted route barriers and deterministic packet schedules.
Use real deadlines only to terminate hung integration tests, not to decide
whether the behavior was correct. Do not build new sleep-driven reproductions.

| Proposed test family (new names) | Controlled stimulus and assertion | Mutation/control it must distinguish |
|---|---|---|
| `TestWindowPolicyComponentsResolveIndependently` | Capture final settings at M=0, mobile budgets, 64 and 384 MiB, with absent and attached pools, in both directions and mixed peer policies. Prove intended limits/topology before traffic. | Accidentally comparing different pools/holds or two inactive rules. |
| `TestWindowShortPathCompressionFeedback` | FIFO finite-rate link; independently set RTT, timer and ACK phase. Remove target cap, offer above both possible window ceilings, trace successive windows and delivered bytes. | RTT replaced by resend floor; feedback or compression removed. Includes below, at and above measured c. |
| `TestWindowShortPathTargetClamp` | Same setup with delivery feedback held out; exercise target-derived limit above/below floor and delayed ACKs. Assert admission and receiver goodput. | Target disabled or target uses a different residence term. |
| `TestWindowRecoversAfterGapIdleAndPathChange` | Script one head loss, idle/resume and capacity/RTT steps; vary ACK times around ring boundaries. Verify bounded recovery and queue occupancy. | Stale/aliased rate evidence or self-inflating mean-RTT candidate. |
| `TestGapAckPrefixesRecoverOnlyMissingPacks` | One real hole, adversarial selective ordering, sender turns between ACK writes; repeat across snapshots and routes. | Remove sort; force descending order; disable gap wake; bypass sender evidence scope. |
| `TestGapAckWakeCadenceAndOpeningHole` | Repeated triples during one virtual interval; no initial head; absorbed pending entries; selective evidence split across snapshots. Assert explicit recovery/cost contract. | Reset wake allowance without intended limit; require an already-established head; count stale evidence. |
| `TestRelayForwardOverflowRecoversExactPack` | Hold actual forward consumer, fill N messages and overflow one, then drain. Reconcile ownership and recover exact missing bytes. | Disable refusal accounting; pretend enqueue success; lose or double-return a retained buffer. |
| `TestInnerTcpDeliveryResponsibility` | Boundary-specific drops/refusals from H6, actual callbacks and finite TCP payload. Assert exact recovery or bounded explicit failure. | Acknowledge rejected injection; forget still-needed replay; discard selectively acknowledged data. |
| `TestPinnedStandbyFailureTransitions` | Script family resolution, dial outcomes and group time, including H3 and reversed attempt completion. | Any-pin instead of all-pin; all errors treated as DNS misses; stale result; missing wake; no recovered-pin takeover. |

These are proposed families, not assertions that the tests already exist.
Keep mechanism tests separate from throughput benchmarks: virtual time proves
ordering and timing logic, not host CPU, kernel queue behavior or Mb/s.

### Existing tests to reuse and gaps to repair

- `transfer_window_round_fixture_test.go` now supplies captured owned wire
  frames and quiescence barriers. Reuse it and the existing FIFO lane fixtures
  instead of introducing another goroutine-per-frame measurement pipeline.
- `TestTheWindowRuleIsInertOnAShortPath` exists (`transfer_window_sizing_switch_test.go:437`).
  Its 5 ms link is capped at 100 Mb/s, deliberately below either window's
  useful limit. It is a good slow-link negative control, not evidence for
  high-capacity short-path safety. Retain it and add the missing regime.
- `TestDeliverySizedWindowRateFormHoldsAtAShortRoundTrip` permits a fourfold
  window spread and has no throughput assertion. A stable floor can pass it.
- `TestAtEquilibriumOccupancyIsHalfTheWindow` currently accepts 0.45–0.85 and
  averages sampled Q/W. It does not establish half occupancy or the report's
  residence explanation. Add time-integrated Q and W over identical intervals,
  report both `integral(Q)/integral(W)` and mean Q/W, and directly measure
  build-to-cumulative-release time. Correlation can affect the difference;
  its sign is not guaranteed merely by a variance argument.
- `TestSizedWindowIsComputedFromTheMeasuredRoundTrip` checks the estimator's
  own published arithmetic and an interior window. Retain that contract, but
  add independent delivery/residence assertions. Its wall-clock load-dependent
  regime is still worth making deterministic.
- Retain `TestReceiveAdvertisementStopsTheLossRetransmitStorm`, peer-branch,
  budget-contention, legacy-sender/hold-inversion, clamped-window, ACK-cancel,
  callback-backpressure and pool-ownership tests. Verify their exact current
  names before adding commands; §9 of the old report documents nonexistent
  test names in earlier plans.
- The PR's seven ACK tests are useful, but several compare 300/500 ms waits
  with a 2 s timer. Convert temporal contracts to virtual time. Their raw ACK
  window injection does not replace sender/receiver recovery tests.

## 5. Missing performance campaign

### Minimum experiment matrix

Start with causal cells, then expand only where the results distinguish a
hypothesis. Avoid an enormous Cartesian product whose comparisons cannot be
explained.

| Dimension | Required coverage and purpose |
|---|---|
| Revisions/arms | Pin PR base and PR head for reproduction. For fixes, use one current base with isolated sort-only, wake-only, combined receiver change and each window-policy component. Provider standby is a separate startup experiment. #214 grouping can be an additional independently identified arm after #213 is understood. |
| RTT and capacity | Submillisecond, 1, 2, 5, 10, 25, 100, 200, 400 ms where the rig supports them; include 100 Mb/s slow-link control and a calibrated fast path that exceeds the small window's effective ACK-limited rate. Label configured propagation and measured Transfer RTT separately. |
| ACK behavior | Production 10 ms, zero as a diagnostic control, intermediate delays, gap wake disabled/enabled; hold peer/sender thresholds fixed or explicitly mismatch them. |
| Path | Transfer-only FIFO fixture; owned finite H1 relay; real H1 with kernel TUN; real socket origin. Add H3 stream, H3 DATAGRAM/direct and mixed-route cells before a global receiver/default change. |
| Traffic | Download and upload; bidirectional TCP; 1 and 8 inner flows to one peer; separate multiple-peer/pool-contention cells; UDP/QUIC application traffic and small request/response traffic. Count logical sequences as well as flows. |
| Loss/reordering | No loss; exact seeded isolated and burst drops in data and ACK directions; bounded reordering without loss; held/recovered route; downstream inner-segment loss. Use identities as well as percentages. |
| Budgets | Unbudgeted process, the reporter's M=384 MiB, and actual SDK process/device/attached-pool configurations for phone and desktop. Verify current platform values from constructors at campaign time. Record provider budget as well as client budget. |
| Duration | Separate establishment, ramp and steady state. Reproduce 30 s tests, then add finite large transfers and longer reliability runs covering recovery timers and contract rotations. |
| Packet shape | Tunnel MTU-sized data, large Transfer payloads, small ACK/control messages and grouped Packs. Vary bytes/message and messages/s independently. |

Do not substitute the synthetic origin for the real-socket-origin cell. That
was the blind spot in the original provider-buffer investigation.

### Measurements every useful run must retain

- Immutable revisions, binary hashes tied to source, full resolved settings,
  enabled capabilities, endpoint roles, actual transport/lane choices, origin
  type, kernel/Go versions, MTU/offloads, memory budgets and relay topology.
- Application bytes received over a **common wall interval**, completion and
  exact byte integrity, time to first byte, ramp time, latency distribution,
  per-flow and aggregate goodput, stalls and time to recovery/failure.
- Simultaneous window, all candidate caps, binding reason, queue occupancy,
  RTT minimum/mean/age, delivery rate/span/age, pool usage and admission waits.
- Unique delivered/resend/duplicate Pack counts and bytes; cumulative,
  selective and contract ACKs separately; ACK frames and snapshots per second;
  gap age/head-blocked time; inner TCP sequence/ACK progress.
- Relay enqueue/refusal/service and queue-residence counters; client injection
  results and failed-run kernel drop evidence; CPU, allocations/GC, RSS,
  goroutines, wire bytes and messages. Snapshot counters before destruction.

Use bounded trace buffers triggered on the first progress failure. Measure
diagnostic-build overhead against the same production build; a heavily logged
path can change the burst or queue problem being measured.

### Statistical and instrument rules

1. Obtain the missing raw run ledger and harness/configuration artifacts;
   recompute medians, paired ratios and failure counts. Preserve binary hashes,
   pair identities and all stalled runs. The HTML's expected option-A
   single-flow speed of roughly 750–800 Mb/s must be labeled conditional on a
   healthy run; its four-stack measured median was 248 with 3/4 stalled.
2. Measure A/A controls and the instrument's capacity at each relevant payload
   and RTT. Use a window larger than the tested constraint, but verify that the
   control has not itself induced relay loss. Inspect byte-rate, message-rate,
   timer lateness and CPU limits. A ceiling inside the predicted effect makes
   that cell inconclusive, not a negative result.
3. Randomize or counterbalance paired arms within blocks, isolate the relay
   or record competing load, and separate startup from steady download tests.
   A fresh provider does not reset a shared relay's load or state.
4. Predeclare primary outcomes and an acceptable regression margin. Use the
   pilot A/A variation to size repetitions for that margin; start with at
   least 10–20 paired blocks for the primary replication rather than treating
   three or four medians as a precise effect estimate. Report paired effect
   intervals and every run, including negative and stalled outcomes.
5. Publish unconditional goodput/completion and conditional healthy-run rate
   separately. Report stall incidence with uncertainty and time-to-stall or
   recovery. Seeing zero stalls in a handful of runs cannot establish an
   unchanged rare-failure rate. Never remove a run because a candidate exposed
   the failure being investigated.
6. Define instrumentation-invalid runs in advance. Publish invalid counts per
   arm and rerun full affected pairs. A correctness failure is an outcome,
   not an instrument exclusion. Choose the final confirmation set before
   examining its results so repeated tuning does not select favorable noise.

## 6. How to evaluate fixes

Use one commit per mechanism and preserve the same deterministic stimulus
and performance configuration across candidates.

| Candidate | Required evidence before preferring it |
|---|---|
| Temporary full Constant rollback | Reproduces short-path recovery; quantify 100/200/400 ms single-flow loss, mixed-peer behavior, and aggregate retained-memory cost. Keep the policy reversible and the trade documented. |
| Correct delivery/target residence handling | H1 component tests fail on the old mechanism and pass on the candidate; fast short paths improve while slow finite queues do not inflate toward the memory ceiling. Test target cap and delivery term together after testing each alone. |
| RTT-related compression or bounded recovery wakes | Faster true-gap recovery with measured ACK/CPU/message cost under steady, reordered and sustained-loss traffic; preserves sparse latency and long-path throughput. |
| ACK snapshot ordering | Independent sender-side test eliminates the manufactured neighbor resends. Validate batch boundaries, routes, cancellation and metadata. |
| Relay pacing, service improvement or grouping | Directly measured refusal/queue reduction at equal byte load; bounded latency/memory, fairness and contract/ordering preservation. Larger buffers alone do not meet this criterion. |
| Inner-delivery recovery | Exact boundary test proves bytes are retained/recovered or failure becomes explicit; bounded memory and replay, correct FIN/RST, no unrelated-flow stall. |
| Earlier provider standby | Scripted lifecycle tests plus restart-to-first-fetch distribution, including tails; safe recovered-pin takeover on both H1 and H3. |

An isolated correctness pass is not a throughput result. A throughput gain is
not acceptable if it loses bytes, worsens persistent stalls, breaks an
advertised hold, increases memory without a bound, or shifts the cost into
unreported latency/ACK traffic. Conversely, do not reject a sound causal fix
because a different known limit masks its benefit: record that cell as bound
elsewhere and confirm below the instrument's limit.

## 7. Execution order and completion criteria

1. **Evidence freeze:** recover ledger/harness, pin builds and relay revision,
   produce the actual settings/sequence-topology table, reconcile reported
   outcomes and define stall/invalid-run rules.
2. **Correctness first:** build the H6 delivery-boundary discriminator and H5
   ACK ordering/cadence tests, then the H1/H2 component fixture and the finite
   relay queue test.
3. **Causal replication:** reproduce the short-path and 100 ms reversals with
   independent clamp, compression and loss traces. Each hypothesis gets a
   confirmed, refuted or unresolved disposition with its decisive artifact.
4. **Candidate evaluation:** run the deterministic suite and scoped race/pool
   checks, then the calibrated paired performance cells. Keep sort, wake,
   sizing, relay and inner-recovery changes independently evaluable.
5. **Confirmation:** rerun the predeclared winning comparisons on real kernel
   TUN plus real origin, actual SDK budgets, long design RTTs and additional
   carriers. Run the full required repository checks on the final candidate.

Research is complete when the loss boundary and the active limit behind each
headline regression are identified, their root-cause tests fail on the
relevant old behavior, candidate effects exceed the measured null band, and
correctness, recovery, memory, latency and long-path tradeoffs are reported.
An unresolved kernel hole or an uninstrumented relay drop remains open even
if a median rises.

### Verification performed for this review

- On an isolated archive of PR head, existing ACK sorting/gap-window tests,
  the three new standby tests and the two default-policy tests passed in one
  focused `go test ./ -count=1` run (package time 4.562 s).
- On local `d9eadf29`, one focused run of
  `TestSizedWindowIsComputedFromTheMeasuredRoundTrip`,
  `TestSizedWindowShrinksWhenThePathShrinks`,
  `TestAtEquilibriumOccupancyIsHalfTheWindow`, and
  `TestTheWindowRuleIsInertOnAShortPath` passed (package time 11.749 s).
  The occupancy row read 0.67 inside its broad existing band; the 100 Mb/s
  short-path control reported approximately 1.00x. Neither tests the missing
  high-capacity short-path regression.
- These were single non-race focused runs, not a full-suite or remote-rig
  validation. The PR author's fail-before/mutation/full-suite claims remain
  attributed to the author. The opt-in `TestTheChainAtTheDesignPoint` campaign
  was not run; its existing environment knobs can seed the later RTT,
  compression and budget sweeps, but it remains a transfer-only instrument.

[pr213]: https://github.com/urnetwork/connect/pull/213
[rig]: https://github.com/Ryanmello07/connect/blob/b54f9f72bec116c0986e6c51ed13cc2f01805bee/THROUGHPUT-RIG-REVIEW.md
