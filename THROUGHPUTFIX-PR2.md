# Window rule on real paths: peer review and research plan

Status: peer review and research plan, updated 2026-09-16. Implementation and
local verification are recorded on `throughput-fix-2`. Sections 1–7 preserve
the original hypotheses and acceptance criteria; §8 records their current
disposition and §§9–10 include the requested `server/connect` and
`server/proxy` regression review.
See [THROUGHPUT-PR2-RESULTS.md](THROUGHPUT-PR2-RESULTS.md) for measured outcomes
and [the run script](tools/throughput-fix-2.sh) for reproduction.

The user explicitly directed us to proceed with our own tests without the
native rig. Its missing source and P37–P44 ledger remain limits on attribution
to the published measurements, rather than prerequisites for this work.

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
| Sibling server checkout | `77201554c49ec05bde83ec038bba6c600972892c`, `../server/connect/resident.go` and `../server/proxy`. The checkout is dirty from external work; this review did not modify it. |

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

1. Preserve our own raw run ledger, harness and configuration artifacts. If
   the original ledger becomes available, recompute its medians, paired ratios
   and failure counts separately. Preserve binary hashes,
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

1. **Evidence freeze:** publish our ledger/harness, pin builds and relay revision,
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

## 8. Implementation disposition

| Mechanism | Deterministic discriminator and current change |
|---|---|
| ACK residence missing from the window | `transfer_window_residence_test.go` checks both the target and delivery terms, absent/zero delay advertisements, timestamp-correct rate samples, sufficient history at 400 ms, and overflow-safe arithmetic. Both terms use minimum RTT plus the receiver's advertised compression bound. Resend timers retain the path RTT. |
| ACK order, redundant cumulative ACKs, sustained gap wakes | `sequenceAckWindow` is factored into `transfer_ack_compression.go`. Direct tests coalesce interleaved heads into one head, send only ascending SACKs above it, and absorb unsent SACKs when it advances. Gap evidence survives drains, with one early proof per unchanged head. |
| ACK response size and pacing | The compressor itself enforces a count bound. Worker tests decode actual wire responses, check the head/SACK invariant across overflow, repeat the cumulative head when metadata follows a SACK batch, and use virtual time to assert compression deadlines. Each response reserves at most 8 KiB and 32 entries; metadata reduces the usable count. Eviction lists retain overflow and send at most 512 sequence numbers per carrier. Both codecs and encrypted wrappers are checked with maximal varints. |
| Fixed 2 ms admission polling | `transfer_capacity_wake_test.go` holds admission closed and proves that capacity release wakes a waiting publisher without advancing virtual time. Capacity changes now signal a generation channel. |
| Loss after successful Transfer delivery | `ip_tcp_return_recovery_test.go` discards exact inner TCP segments after the Transfer success boundary. Middle, tail, wraparound, IPv6, EOF and healthy-flow controls verify exact bytes. A bounded provider replay cache retains origin chunks through the inner TCP ACK; duplicate ACKs or a bounded timer replay the oldest missing segment. |
| Deep-window relay bursts | A finite FIFO test reproduces refusal and gap-repair amplification with pacing disabled. H1 data and recovery writes share a service clock and bounded opening probe. Pacing uses delivery evidence, probes spare capacity and leaves drain capacity when backlogged, capped by the configured target. Slow links, feedback delay and independently compressed sibling ACKs have separate controls in §10–11. This identifies the queue mechanism in our fixture; it does not identify the remote relay's drop site. |
| Upload ACK handoff cycle | `tun_ack_handoff_test.go` holds a real gVisor endpoint lock and proves that pure ACK injection through both `Write` and `WriteBatch` returns. Pure ACKs no longer enter the data handoff's endpoint lock; IPv4/IPv6, options, ECN, payload and lifecycle flags are classified separately. |
| Upload shutdown ownership | A TCP performance run exposed pooled packets retained after cancellation. `TestTcpSequenceCancelBeforeWritePublicationReturnsPacket` forces the producer to publish after the old consumer's empty drain. The producer now closes on every exit and the writer drains through closure before the worker join. |
| Provider standby after unusable pinned addresses | Adopted PR standby changes are supplemented by changing-evidence tests for wrapped DNS errors, temporary failures, success, H3 and already-connected pins. |

The default remains the delivery-sized window. A new optional wire field
advertises compression delay: absent means the historical 10 ms, while
explicit zero means immediate ACKs. Old decoders ignore the added field.

The local performance fixture covers nine RTTs (0.3–400 ms), one/eight flows,
and immediate/10 ms ACKs with virtual-time FIFO serialization. Its separate
TCP workload includes a loopback socket origin, provider NAT, Transfer and
gVisor TUN in both directions. It counts delivered application bytes over a
common interval and retains A/A controls, ceiling controls and stalled runs.
These fixtures do not substitute for native kernel TUN or deployed relay
measurements. Remaining campaign dimensions in §5 keep that distinction.

## 9. `server/connect` regression review

Reviewed sibling server revision `77201554c49ec05bde83ec038bba6c600972892c`
against this local connect branch via its existing `go.mod` replacement.
Server files were reviewed without changing the sibling checkout.

### Forwarding has two separate saturation boundaries

1. `Resident.handleClientForward` shares a frame into destination-stable
   callback ingress. Production defaults divide 4,096 entries over 16
   shards: **256 entries per shard**, shared by destinations hashing there.
   Full ingress returns ownership, increments the receive-boundary drop
   metric and cancels the resident generation. The existing deterministic
   `TestResidentForwardCallbackRetiresFullIngressWithoutWaiting` pins that
   behavior.
2. The owned `processClientForward` worker feeds a **4,096-entry**
   `ResidentForward.send` queue. Production `ForwardTimeout=0` makes a full
   queue increment `forwardDroppedCounter` and discard the frame. Transfer
   must discover and recover this loss above the reliable socket.

Both paths carry forwarded ACKs and Packs, and the relay can receive encrypted
frames. A proposed server ACK-priority queue therefore needs authenticated,
usable classification; it cannot assume plaintext visibility. The local FIFO
test reproduces the second boundary. It does **not** cover shard collisions,
resident generation retirement, contract lookup delays or multiple residents
competing for an actual exchange writer. Server batching can also change
messages/second at equal byte rate, so bytes alone cannot size these queues.

**Follow-up experiment:** drive the actual resident with one/eight logical
sequences and multiple destinations (including deliberate shard collisions),
at the same link rates and RTTs as the local matrix. Record both boundary
counters, reconnects, per-destination progress and ACK residence. Test loss
and recovery at ingress and downstream queues separately before changing
server admission or scheduling. Keep shared callbacks nonblocking.

### Message-size and carrier compatibility

`DefaultConnectHandlerSettings` gives the server framer the connect runtime's
`MinimumMessageLenLimit()`; the WebSocket read limit includes its four-byte
framing prefix. Resident exchange framing uses those same settings.
`TestResidentAdmitsMinimumMessageLenLimit` passes against the branch. Combined
with the connect tests of encoded ACKs and encrypted wrappers, this covers
the producer/receiver size contract. H1 ready batches keep messages separately
framed: their batch count/byte policy is distinct from ACK compression.

Focused server tests for framing, exact carrier properties, exchange batching
and ownership passed. A **race-enabled deterministic unit run passed**, including
reliable receive saturation, cancellation, resident ingress retirement,
bounded H1 batching/FIFO order and H3 ACK reserve configuration. The exact
selection is reproducible with
`tools/throughput-fix-2.sh server-connect-deterministic`.

The local environment from `server/connect/test.sh` / `server/test-env.sh`
was attempted with fail-fast enabled and the documented local endpoints. The
launcher `ready` marker was absent, so the run used the permitted local
service override. The Go integration preflight then rejected the checked-in
test credential at the local PostgreSQL endpoint before creating its disposable database.
The six H1/H3 variants, pool-balance test and directional TCP test are
therefore **deferred**, rather than reported as passing. This is an environment
credential mismatch; it does not exercise or falsify the window changes.

The earlier three-trial server TCP readings remain retained as historical
evidence, but are not promoted to final validation because their source and
environment differed. Re-run them after the local launcher credentials are
reconciled, with the all-run ledger policy in §5.

## 9a. `server/proxy` regression review

The same sibling revision was reviewed in `../server/proxy`. The deterministic
selection covers device memory admission, borrowed packet ownership, WireGuard
and TUN handoff boundaries, manager close/join, drain coordination, lifecycle
metrics, window identity restore and bounded traffic metrics. It passes without
the race detector, matching `server/test.sh`'s explicit wall-clock proxy tier;
reproduce it with `tools/throughput-fix-2.sh server-proxy`.

The two database-backed WireGuard handoff tests were attempted separately and
stopped at the same PostgreSQL authentication preflight. Keep that attempt as
environment evidence and defer it with the `server/connect` integration run.
The sibling proxy checkout remains unmodified.

## 10. Service-pacing review prompted by deterministic tests

The initial service-pacer hypothesis was incomplete. A configured-target
limiter fixed the fast finite-relay row but collapsed a 100 Mb/s path. A
whole-window delivery average avoided that collapse but imposed a second
startup ramp at 200/400 ms. The next experiments therefore test the service
estimator and the send schedule independently, followed by actual Transfer
clients over deterministic serializers.

The follow-up now includes 54 slower-service cells, six capacity changes and
four logical sequences sharing one finite relay. The shared-path control
exposed multiplied per-sequence startup allowances, ACK arrival/processing
confusion, and independently compressed ACK phases being mistaken for a much
faster serializer. The implementation uses one service budget per live logical
sequence class, one bounded initial probe, original arrival intervals, and
first-delivery wire-byte accounting. The probe can span two compressed reply
intervals, capped at twice the ordinary initial window. A backlogged service
leaves five percent of measured capacity for draining its queue; otherwise it
may probe ten percent above observed service. Fresh cumulative progress protects a slow H1 FIFO
from rewriting its initial train behind itself.

Review and retain the controls for these mechanisms before interpreting host
throughput: exact byte/time envelopes, rate transitions, initial and idle
probe limits, shared-producer timing, cancellation and reference lifetime,
ACK compression phase, SACK/head double-counting, stale fast-rate evidence,
and a repaired head after slow service. `TestWindowPathGapDeadline` additionally
pins an exact-deadline receiver spin found during the shared-service run.

The final deterministic matrix and regression reruns are recorded in
`THROUGHPUT-REPORT-PR2.md`; the earlier full
suite failed the then-current long-RTT pacer and a structural ownership check.
The [results report](THROUGHPUT-PR2-RESULTS.md) records the failed hypotheses and
will distinguish final capacity results from startup/transition loss and the
remaining actual-server pressure experiments in §9.

## 11. Mismatched endpoint windows

The original local fixture configured identical endpoint budgets. That cannot
show whether a large sender respects a small receiver, or whether pacing adds
a second throughput limit when a small sender targets a large receiver.

The new matrix independently configures sender capacity and advertised receive
capacity at **256 KiB, 2 MiB and 48 MiB**, in all nine ordered combinations.
Crossing these with **0.3/100/400 ms RTT**, **0/10 ms ACK compression** and
**one/eight flows** gives **108 cells**. Six further cells change a live receiver
between **64 KiB and 2 MiB**, in both directions, with 0/10/50 ms compression.
The estimator tests additionally cover 32 KiB advertisements, limits below the
sender's working floor, initial sampling and later delivery sampling.

Each performance cell has a constant-window reference clamped to the smaller
endpoint. Its attainable rate must also agree with the physical service or
window/residence bound; an underfilled reference cannot validate the candidate.
The candidate must deliver at least 90% of that reference, make progress on
every offered flow, respect both limits and avoid steady-state relay loss or
receive-queue eviction. Window-limited measurements span at least twenty
residences so a partial flight at an interval edge cannot dominate the result.
The service and mismatch fixtures offer flows round-robin within each logical
sequence; independent logical sequences retain separate producers.

These controls reproduced the following defects:

- A 256 KiB working floor overrode a peer advertising only 64 KiB, before and
  after delivery sampling. An explicit peer/deployment ceiling now bounds the
  working floor; the shared budget's existing guaranteed-memory policy remains.
- A small immediate-ACK train could fit wholly inside a sampling bucket, or a
  larger opening train could straddle its boundaries. Keeping paired first and
  last arrival checkpoints prevents measuring the following idle gap as service.
  A separate compressed-tail control covers a full interval followed by a
  partial final reply, alongside sparse slow replies and independently phased
  sibling compression.
- Excess flight alone did not establish a queue: it could still be propagating
  after a fast opening train. Before one service residence has been delivered,
  the probe-stop condition also requires excess observed ACK residence.
- A correct 125 MB/s observation expired after only 40–50 ms, before sends
  using that rate could return ACKs over a 400 ms path. Retain the peak for a
  full feedback interval based on stable minimum RTT, not growing queue RTT.
- While a queue is observed, compressed ACK peaks can keep the sender above
  actual service. Use a sustained average over a feedback interval and several
  compression turns, then leave capacity to drain the queue. Tests at four
  service rates check that newly paced writes actually permit that drain.

An attempted compressed-tail extrapolation was rejected. Moving the previous
reply's bytes into the following reply's interval doubled a controlled slow
service estimate from 125,000 to 250,000 B/s, and a shared 1 Mb/s trial fell to
0.717 Mb/s with recovery traffic. The estimator retains the conservative tail
observation; queue evidence determines whether further capacity probing stops.

The deterministic mismatch/model/race reruns and ACK benchmark passed. Host TCP
controls completed, but the later audit found missing performance assertions
and slow candidates; see §13. Preserve every reading and failure-before log. The
At this checkpoint, the configured server/connect and server/proxy integration
tiers remained deferred until the local launcher and test resources agreed on
the PostgreSQL credential. Section 40 closes that deferral.

## 12. Adjacent root-cause review and deterministic completion gate

This review follows `CODESTYLE.md`'s bug-fix and test rules: inspect sibling
paths for the same reasoning error, then force the broken state transition
before changing production behavior. New positive cases use plain table loops;
time-dependent cases use virtual time and lifecycle cases use explicit barriers.

| Root-cause family | Adjacent paths reviewed and regression coverage |
|---|---|
| ACK residence and delivery arithmetic | Target and delivery window terms, contract announcement lead time, immediate/absent/maximum compression advertisements, checkpoint pairing and history span. A new long-residence contract case produced zero instead of 2 MiB before the sibling multiplication was fixed. |
| Window limit ordering | Explicit local ceilings, peer limits, unsampled and unbudgeted fallbacks, legacy/zero advertisements, and changing receiver budgets. New tests reproduced unbudgeted 2 MiB sends against 64/32 KiB limits and stale delivery undoing a later 2 MiB capacity increase. Zero capacity retains only the queue's separate one-item progress allowance. |
| Service sampling | Shared and standalone histories, bucket boundaries, short trains, partial compressed tails, delayed/out-of-order application, duplicate SACKs and cumulative absorption. A new standalone-history test reproduced the four-sample expiration defect. |
| Queue detection and pacing | Propagation versus queue residence, startup and idle credit, capacity changes, sibling services, cancellation, recovery writes and every byte/time prefix. A new controlled idle-gap case read 31.5625 MB/s for a 125 MB/s active train before the sustained estimate rejected window-limited gaps. |
| Send/receive clocks | First physical write, ACK arrival, worker application and retransmitted copies. A real paced-write/handoff test measured 110 ms for a 100 ms path because the wire tag preceded a 10 ms local wait. Service RTT now uses first actual write time; ambiguous retransmissions and changed/unreliable carriers cannot establish it. |
| ACK response bounds and ordering | Struct drain, worker timer/drain, both codecs, encrypted wrappers, eviction metadata and contract-recovery requests. Existing deterministic cases enforce one head plus bounded oldest-first SACKs, absorption, overflow, maximum fields, deadlines and final owned drain. |
| Gap recovery and admission wakeups | First head, repeated proof, split snapshots, exact gap expiry, related versus unrelated pending ACKs, and capacity notification. Existing tests force each boundary without wall-clock polling. |
| Inner TCP loss and replay ownership | Middle/tail/EOF loss, wraparound, IPv6, healthy traffic, partial ACKs, already-ACKed read-ahead, shared budget, reserve-before-copy and cancellation. The retained chunk and replay callback boundaries have exact byte and pool-ownership checks. |
| Upload/TUN lifecycle | Both single and batch TUN paths, pure ACK versus data/control flags, busy endpoint locks, cancellation before payload publication, producer close, writer drain and worker join. Sibling TCP/UDP input queues close under their producer lock before draining; they do not have the late-publication defect fixed in the upload queue. |
| Standby recovery | H1/H3 dial evidence, wrapped DNS errors, temporary failures, changed evidence, successful reconnect, connected and absent pins. The deterministic transition test checks wakeups as well as the fallback predicate. |
| Server ingress and lifecycle | Current sibling checkout's lazy forward-shard start, producer admission fence, worker registration, close/join and final late-callback drain; bounded H1 batching and H3 carrier configuration. The expanded server selection passed under the race detector after correcting the script's package working directory. |

The adjacent tests are in `transfer_window_adjacent_test.go`. A large-target
boundary control already passed and is not counted as a reproduced defect.
Preserve the failure-before logs separately from passing controls.

The earlier full sweep exposed an experimental lane-recovery completion-bound
failure and a short-path host-throughput failure. They passed when isolated
and in the final non-race root selection. Their failed earlier runs remain in
the evidence. The scoped race and model checks passed; the separate host TCP
performance acceptance gap below remains open. Configured server integrations
retain the user's explicit deferral.

## 13. Completion audit: host performance acceptance

Inspecting the final-source ledger contradicted the earlier host summary.
The host gate failed only zero-progress cells; it logged ratios without
rejecting large regressions. Ten of 48 comparisons have candidates below 90%
of their measured ceilings, including four with no calibration/A/A exclusion.
The 48 MiB, eight-flow short-path download reached only 291–628 Mb/s. A default
100 ms single-flow upload declined to 7.7 Mb/s in its final measured second.
Zero relay drops and receive evictions do not establish an optimal rate.

The new deterministic comparison tests restore the old comparison function
and reproduce five failures: slow candidates, a capped fixture, drifting
controls, an independently stronger matched control and delivery loss/stalls.
The corrected gate enforces the declared ten-percent rate margin while keeping
both failed results and instrumentation censor reasons. New manifests record
build flags, start/end load averages and explicit concurrent-run context.

Next, re-run the affected short-path download and long-path upload cells
immediately under current host load. Resolve persistent regressions using
controlled service/ACK schedules, and compare paced/unpaced arms and socket
buffer limits before assigning the cause to pacing, TCP or host scheduling.
Record every pair; a favorable repeat alone cannot close the negative result.

## 14. Busy-path RTT changes and refreshed window residence

A new deterministic change matrix exposed a second feedback loop: service RTT
kept a historical minimum on a continuously busy path, so increased propagation
was treated as queueing. Pacing and delivery-sized admission then shrank
together. Changing 0.3 ms to 100 ms RTT produced 7.148 Mb/s on a 100 Mb/s service
and 4.055 Mb/s on a 1 Gb/s service, without measured relay drops.

The combined correction refreshes RTT from the first physical write following
cumulative delivery of every sibling sequence's tail, and makes window sizing
use that refreshed service minimum while its own older RTT samples age out.
The ACK worker retains the earliest covering arrival before a newer head can
absorb it. Actual successful H1 carrier confirmation, retransmission exclusion,
per-sibling cumulative proof and cancellation cleanup bound this evidence.
`transfer_window_pacing_probe_test.go` pins the state transitions; the four RTT
change cells now match their 95.805/958.095 Mb/s references within rounding.
Capacity-change and shared-service controls also pass. The complete mechanism
and validation limits are documented in `THROUGHPUT-REPORT-PR2.md`.

The adjacent fixture review found that a service-rate change overwrote queued
frames' individually chosen propagation delays. A new combined rate/propagation
test failed at 11.5/12 s instead of the required 11 s FIFO arrivals. Frames now
retain their propagation delay during reserialization, and both increasing and
decreasing delay controls pass.

The user's time-bucket/zero-order-hold suggestion has seven deterministic
primitive tests. Empty buckets hold the last measured bucket mean; measured
zero, partial buckets, unknown startup, late updates and long idle expiry remain
distinct. Using average RTT directly for the flight bound regressed shared
services and was rejected. Further estimator integration must pass those
controls as well as RTT changes and window mismatch.

Remaining gates: run the full model and scoped race selection on the combined
fix, retain focused failure-before mutations, repeat affected host comparisons,
and review both server deterministic tiers. Database-backed server integration
remains explicitly deferred by the user.

## 15. Physical flight, queued reservations and message granularity

The first post-RTT-fix host run reproduced a 75.856 Mb/s upload against its
175.282 Mb/s reference, ending below 1 Mb/s. Deterministic follow-up found that
queue detection counted pacing reservations before they reached the writer and
did not allow for indivisible message sizes. The resulting false backlog could
multiply an already low service estimate by the drain factor repeatedly.

`transfer_window_pacing_flight_test.go` reproduces all three boundaries before
the correction: one large message, two concurrent unsent reservations, and a
message crossing the continuous residence bound by less than its own size.
The corrected bound subtracts unsent reservations and permits one message in
addition to rate times residence. Both backlog detection and sustained-service
selection use it. Cancellation releases each reservation once while preserving
its already incurred pacing debt. A previously single-message queue stimulus
now uses four 50 kB messages, so its 200 kB flight is demonstrably beyond both
propagation and one-message rounding.

The new 32-cell large-message matrix covers 16/64 KiB payloads at four service
rates, two RTTs and one/eight flows. It passed the first flight correction.
Twelve targeted host comparisons also passed, with three capped-reference
exclusions retained. The final source adds the explicit rounding boundary and
is running the full model, complete host matrices, scoped race selection,
root regression and environment-configured server deterministic tiers.

## 16. Estimated burst limits and delayed timer dispatch

The requested contract is one byte/time estimate: **B bytes over T time**.
The burst byte ceiling is **B**; its duration ceiling is **k × T**, with the
working candidate using `k = 2`. Neither a time multiplier nor a faster
exploration refill rate multiplies an available byte estimate. A physical
message establishes the minimum representable byte/time estimate. Both bounds
belong to the shared service, including retransmissions and cancellation.

The old two-millisecond expiry misclassified a three-millisecond scheduler
delay as idle, reducing isolated admission to 41.1% of its configured rate.
The end-to-end model compounded that error to 17.367 Mb/s against 993.657 Mb/s
in four RTT/flow cells. The explicit burst candidate restores 99.7% in the
isolated test and passes all eight delayed-wakeup cells. The adjacent review
also reproduced combined timer releases exceeding the byte ceiling, a refill
clock moving backward, and a startup probe enlarging a known byte estimate.
These now have independent deterministic checks in
`transfer_window_pacing_wakeup_test.go`.

The first byte-meter checkpoint passed 126 focused race tests and six
short-path host upload comparisons. The newer strict whole-message and flight
bounds retain passing slow-rate, capacity-change, large-message, wake-delay and
live-window-change controls, but expose two further hypotheses to resolve:

1. **Continuously occupied flight never supplies a drained RTT probe.** The
   100 Mb/s, 0.3-to-100 ms case retains its 0.514 ms baseline and falls to
   3.809 Mb/s. Add a controlled baseline-refresh protocol that can obtain fresh
   evidence without depending on an accidental idle gap. Test genuine queue
   growth, propagation changes in both directions, lost probe replies and
   cancellation separately. An increased queued RTT alone must not authorize
   a larger standing queue.
2. **Shared release fairness and ACK grouping on the slowest service.** The
   1 Mb/s shared cell has a zero-progress flow despite aggregate capacity.
   Deterministically hold an older reservation while later producers wake;
   prove bounded progress and cancellation of a preceding waiter. Separately
   replay the ACK timing that produces a 250 kB/s estimate on a 125 kB/s
   serializer. Check whether time-bucket aggregation can remove grouping bias
   while preserving capacity evidence across window-limited gaps.

Reproduction diagnostics are opt-in with `CONNECT_WINDOW_PACING_TRACE=1` and
`CONNECT_WINDOW_PACING_TRACE_CASE=rtt-growth` or `shared-slow`, running
`TestWindowPathPacingStartupTrace`. The trace retains physical flight, pending
reservations, applied and known-delivered bytes, RTT baselines, burst credit and
serialization debt. A passing trace process is diagnostic output, not a
throughput acceptance gate.

The corrected burst-ring candidate passes the four RTT-change and three shared
service performance pairs after the final admission and byte-debt corrections.
After the adjacent reset-order correction, all 81 focused pacing/statistics
tests and all 147 tests in the full correctness selection pass under the race
detector on runner source SHA-256
`fab6a0ab0f065bf4dd6a04d71976cdb5e4d5a4475508ebfa97601f06533a3d82`.
Deterministic tests separately reproduce the stale
RTT floor, sparse-head rate doubling, overtaking a delayed writer and phantom
ACK credit at the admission/write handoff. The ring resets on newer burst
evidence with the preceding burst mean held until completed new buckets take
over; old burst IDs cannot replace it. A queued mean triggers a bounded drain
experiment and does not itself raise the propagation floor.

The handoff test uses the real send method and ACK coalescer with an explicit
channel barrier. Adjacent failure-before tests cover reordered current-burst
samples, dispatch epochs after delayed timers, changing byte limits and a late
release crossing a nominal serialization deadline. Estimate changes preserve
already spent bytes and their original serialization cost; they do not turn
unissued credit into false debt. Missing replies, cancellation, reused FIFO
waiters, duration overflow and partial/empty statistics buckets have separate
deterministic controls. These run in the normal correctness selection.

Four added large-message capacity-change pairs cover 16/64 KiB payloads at
1↔10 Mb/s, 100 ms RTT, 10 ms compression and eight flows. Measurements cover
at least 64 payloads to retain meaningful per-flow progress on the slowest
case. All four initial pairs pass, with zero measured drops. These complement
the 32 steady large-message pairs and small-frame capacity-change matrix.

Do not accept the candidate or claim all cells optimal until these two cases,
the full mismatch matrix and both host TCP buffer configurations pass on the
same source. Database-backed server integrations remained deferred at this
checkpoint as requested; section 40 records their later completion.

## 17. Preserve service evidence across a deliberate drain

The broad burst-ring checkpoint exposed a separate deterministic failure at
100 Mb/s, 400 ms RTT, eight flows and 50 ms compression: 66.847 Mb/s against
95.826 Mb/s. Replaying its exact warmup and one-second measurement reproduced
the loss. A controlled drain coalesced a large final ACK; when its preceding
checkpoint expired, the next small probe over a 400 ms idle gap appeared to
establish a service near 6.7 kB/s. That false measurement reserved another long
wait which later ACK traffic could not undo.

The correction records a new ACK-time service epoch only after a probe from a
deliberate pacing drain is confirmed. It holds the established pre-probe service until fresh checkpoints
replace it, while late old ACKs still release delivered-byte ownership. The hold
is captured before the probe is exposed so ACK-before-write-confirmation order
cannot overwrite it with the invalid gap measurement. Direct and covering ACKs,
changed carriers, missing replies, late old evidence and replacement by slower
fresh service have separate deterministic tests. The isolated model now reaches
95.846 Mb/s with zero measured drops.

Six additional pairs measure twelve seconds at 0.3 ms RTT and 10 ms compression,
crossing 16/64 KiB messages and 0/1/3 ms dispatch delays. They span more than two
drain cooldown intervals and retain every one-second reading. They pass, but do
not reproduce the older host-only download collapse. The optional trace now
observes both data and TCP-feedback services during warmup and measurement,
including drain deadlines and a copied ring mean; trace reads cannot advance
the production ring.

Final confirmation uses source SHA-256
`b5b407364a99cbb0a022b5de897fc5eb8bade0593541ddd2e0b658636c692207`.
The full correctness, model, root regression and both host TCP configurations
start immediately under recorded concurrent load. Keep the earlier failed
checkpoint and separate a proven model cause from an unproven host explanation.

Adjacent review also confirmed that the largest-message floor may remain after
that message is acknowledged while smaller traffic keeps the service occupied.
It must cover older queued physical messages. No failing performance case has
yet shown that this conservative retention needs a production change; a mixed
large-then-small traffic experiment should precede any attempt to shrink it.

## 18. Preserve natural serialization while excluding deliberate pauses

The adjacent review found that the first service-epoch correction also reset
history on naturally empty flights. An established sparse sender then repeatedly
discarded the two ACK checkpoints needed to discover a faster service. With
64 KiB messages, eight flows, 100 ms RTT and 10 ms compression, a settled
1→10 Mb/s change remained near 1.1 Mb/s and one flow made no measured progress.
The pre-epoch implementation reached 9.531 Mb/s in the same diagnostic interval.

The original large-message matrix changed capacity at 4 s, during a slow
opening train that can last about 33 s. Add a separate matrix with the change at
40 s. Move its measurement forward by the same 36 s so the post-change settling
allowance, minimum 64-message sample, reference and 90% gate remain identical.
Retain the earlier ten-second-after-change diagnostic as adaptation evidence;
steady-state acceptance must not imply identical recovery speed.

Only a deliberate pacing pause marks the next physical write for a service
epoch reset. Natural probes still refresh RTT and preserve their serialization
checkpoints. The marker must follow the service pause across a late dispatch
or cancellation of a waiting writer. The first physical write consumes it;
retry, ambiguous delivery and canceled tail ownership cannot certify a drain.
Test both ACK/write-completion orders, slower fresh evidence, successful delayed
dispatch, abandoned drains and FIFO-head cancellation before accepting the fix.

The corrected source `f60cf11db4f0c7a928144ad78d1b9cebe96bbf8916fa620d1b58d01598fc613a`
passes 89 focused race-enabled tests and all nine focused performance pairs.
The full correctness selection also passes all 156 tests under the race detector.
The settled 64 KiB increase now reaches 10.000 Mb/s, with 1.250 Mb/s per flow;
the 400 ms RTT/50 ms compression case retains 95.846 Mb/s. Preserve the
committed-source and intermediate-correction failure-before evidence separately.
The shorter recovery diagnostic reaches 7.34375 Mb/s with the initial
controlled-only correction versus 9.53125 Mb/s before service epochs. Reaching
the full service estimate at 11.41 s does not prove full goodput then; a separate
convergence comparison must retain the lower interval readings.

## 19. Retain delivery evidence across bucket-duration changes

The dedicated transition test now records twenty full one-second intervals
after the settled 1→10 Mb/s change. It compares seconds 10–14 against the
constant-window reference and reports completion of three consecutive intervals
above 90% of that reference. The controlled-drain source fails the fixed gate:
7.733248 versus 9.961472 Mb/s, with recovery reported at 16 s.

The root is a bookkeeping reset. When faster ACKs reduce RTT, the service ring's
bucket duration can change; clearing the ring then erases the adjacent delivery
checkpoints needed to replace the old held rate. Rebucket retained aggregates
by their real arrival timestamps while preserving the 64-slot bound and the
zero-order hold. Merge first-arrival bytes once, route reordered observations
into an existing aggregate's real span, and exclude expired or mixed-epoch
evidence. Shrinking and growing the interval need separate boundary controls.

The direct faster-pair test fails before the correction. Adjacent review also
reproduced rate inflation in the initial rebucketing attempt: an old aggregate
spanned two new buckets, and a late ACK created overlapping sums. The corrected
tests cover first-byte collisions, reordering, retention and epoch cutoffs.
The fixed transition averages 10.092544 Mb/s against 9.961472 Mb/s; reported
recovery improves to 13 s. The pre-service-epoch control reports 4 s, so the
remaining adaptation difference stays open. Do not change the gate or substitute
the later steady-state measurement for this recovery evidence.

Final rebucketing source `56eec7b1968bac2214224b2d7975f849d0fe6efef53cd9322eb11a01b31bc6ef`
passes all 160 race-enabled correctness tests and 23 targeted performance pairs
across seven tests. The exact earlier 10–13.3554432 s diagnostic now reaches
10.000 Mb/s, and the 400 ms/50 ms control retains 95.857 Mb/s against 95.846 Mb/s.
Keep the direct pre-fix failure and intermediate overlapping-aggregate failure
separate from the final results.

The preceding controlled-drain source `f60cf11d` has also completed its full
model run: 21 passing tests, 272 paired cases and 508 retained ledger rows.
That result does not validate the later rebucketing source.

## 20. Pin resolved SDK settings before adding budget cells

The existing 48 MiB performance fixture does not exercise actual SDK constructor
budgets. `tools/throughput-fix-2-sdk-settings.py` captures the real constructors
and sizing helpers through an overlay without creating network clients or
changing the sibling SDK checkout. The initial eight-profile capture is retained;
the expanded capture has 11 profiles covering connect's zero/384 MiB process
budgets, desktop/mobile device/provider defaults with providing on or off, and
explicit mobile H1 policy. Nil pools remain distinct from non-nil zero budgets.

The captured desktop device target is 20 MiB. The selected mobile profile uses
a 24 MiB device target with a 32 MiB process budget, smaller queue bounds and
retained receive accounting. The expanded archive pins SDK revision `7fe75c69`
and connect source `56eec7b1`; the earlier archive retains source `5f28f158`.
Both are host-selected policy observations, not evidence from a mobile runtime
or application override. Actual helper order matters: provider send/receive
pools are independent, Pack budgets are shared with the device, and explicit
mobile H1 selects eight lanes with divided count queues and unchanged byte caps.
The performance fixture preserves these Transfer limits after policy selection.

Two new normal model tests use the pinned 40-field fixture. Every profile is
paired with the 384 MiB server in both directions at 0.3/100/400 ms RTT and
one/eight flows (132 pairs); explicit mobile H1 profiles add simultaneous
bidirectional traffic (18 pairs). The first run passes the one-way test but
fails the provider's 0.3 ms/eight-flow duplex cell: its reverse direction drops
from 587.20256 to 428.41088 Mb/s with zero recorded drops. All 300 readings
remain in `sdk-transfer-model-first` on source `3242678e`.

The current gates check each direction against its own constrained reference,
both arms' flow progress and receive refusals, the reference's window/feedback
lower bound, and release of every attached pool at shutdown. They must be rerun
on the final source. Investigate service sampling and shared-pool contention
with forced ordering before changing production limits. Keep short-cell failures
when adding longer diagnostics. Physical H1 priority queues, native TUN and
multiple peers sharing SDK pools need separate coverage; the direct Route model
does not exercise those boundaries.

## 21. Separate inner-TCP replay from sustained service evidence

An 80 ms inner-ACK producer pause induces a host collapse on the controlled-drain
source even without the diagnostic RTT override. Detailed tracing shows that
a small NAT replay crosses application idle and replaces the useful service
estimate; the reopened inner TCP window then waits behind a long pacing debt.
The bucket duration remains 10 ms, separating this cause from section 19.

An overlay regression using the real `TcpSequence.runReturnRecovery` worker
reproduces the boundary three times under virtual time. A 1,100-byte replay
reduces a 125 MB/s service estimate to 3,536 B/s; after inner ACK progress
reopens the sender, its next 70 KiB write waits 18.430482186 s. Preserve this
failure-before evidence alongside the corrected normal test. The correction
records locally observed idle only when all physical tails are ACKed and
H1-confirmed and no pacing producer is reserved. A later reservation checks
the gap before any timer/FIFO wait and marks the resumed service probe.
Existing write/ACK confirmation commits the new epoch while preserving the
last useful rate. Already-waiting demand retains natural serialization;
fresh slower evidence must replace the hold.

Nine normal tests now cover the real replay worker, both confirmation orders,
failed H1, queued demand, head/interior/last cancellation, delayed drain
observation, retry invalidation, a real Pack blocked before reservation, and
all eight shared tails. Six boundaries fail three times each before the fix;
92 focused tests pass three times afterwards. The replay retains 125 MB/s and
the next 70 KiB write has no virtual pacing delay. Source `5605efa7` passes
the full 169-test correctness selection under the race detector; seven scoped
model controls also pass, retaining 50 readings.

After the deterministic fix, rerun affected host comparisons with their controls
under the current load. These induced diagnostics do not identify every earlier
paired host failure, and a favorable rerun cannot erase those failed outcomes.

## 22. Keep probe confirmation and service evidence ordered

The full rebucketing model on `56eec7b1` completes 21 passes and one failure.
Eight flows across four service lanes on a 1 Mb/s, 100 ms RTT, 10 ms compression
path achieve 0.82944 Mb/s against 0.94720 Mb/s for the reference. An isolated
repeat of the same immutable binary also fails, so another test's global state
is not required. Preserve this failure separately from the source-idle root.

The deterministic root now forces a resumed controlled-drain probe that is
physically confirmed but awaits its own ACK. Old coalesced heads apply only
part of the bytes already proven delivered. They lower the service used for
sibling reservations before later probe confirmation restores the hold.
The correction tracks the remaining old byte count, excludes its incomplete
train provisionally, and accepts complete old evidence or fresh post-probe
pairs. Only old-arrival timestamps pay off that count. A later probe ACK must
not restore a rate superseded by valid sibling evidence.

Five normal tests cover actual pacing waits, missing replies, natural drains,
failed/retried writes, cancellation, fresh slower service, completed faster old
trains and new bytes that must not complete an old train. They fail three times
each before the correction. All 37 targeted performance comparisons pass;
blanket hold alternatives were rejected because they regressed true RTT growth.
The complete model on source `538e6248` subsequently passes 24 tests,
423 paired cases and 810 rows. The separate SDK recovery failure remains open.

The SDK provider duplex failure also remains after the source-idle fix: one of
three exact-cell repetitions reaches 542.57664 versus 620.00128 Mb/s in the
return direction. A complete 150-pair rerun passes once on `5605efa7`, but does
not supersede those failures. Read-only tracing and forced feedback ordering
reproduce reverse deficits after the pending-byte correction too. A fixed
initial ACK-worker barrier yields about 431–447 versus 807–809 Mb/s three times,
with unchanged warmup, duration, settings and directional gate. Matching early
real ACK feedback across arms yields about 816 Mb/s; late offering lowers every
control. The local ACK-priority companion cannot bypass data already queued in
the opposite FIFO. Keep these phase and recovery controls separate and resolve
the deficits before claiming an optimum or changing any acceptance gate.

## 23. Public statistics must not change the pacing controller

`DestinationSendStats` previously used the retaining estimator. A deterministic
polled/unpolled pair captured 500 kB/s versus 10 MB/s in the next confirmed probe
from the same delivery history. Admission and statistics now use one arithmetic
implementation, with retention disabled for public snapshots. Both the service
hold and pending probe's saved rate remain unchanged by polling.

Three normal tests cover faster/slower evidence, nil/attached budgets, probe
confirmation, shared lanes and duplicate sequence inventory. They fail three
times each against pre-fix production. With the five pending-byte tests, the
exact final tests produce 24 failures before the fix; corrected `538e6248`
passes all 177 correctness tests under the race detector. The full model passes
24 tests and 423 paired cases on the same frozen source. Rerun affected host/SDK diagnostics using
this read-only interface; old traced passes cannot establish ordinary behavior.

## 24. Freeze source audits and calibrate the legacy serializer

The `5605efa7` root regression retains 3,011 passes, two failures and 24 skips.
One failure read edited source from `538e6248` after compilation. The runner
must compile and run in copied repository inputs so both relative reads and
`runtime.Caller` inspect that binary's source. The synthetic before/after test
edits the original input before both reads. Adjacent checks cover untracked
new tests, symlinked fixture contents, copied module replacement declarations
and rejection of changed snapshot files or source inventory. External local
module replacements and host services remain explicit live dependencies.
Use fresh output directories outside Git worktrees; direct, nested, aliased and
sibling-worktree paths are rejected before a snapshot can add duplicate Go files.

The other failure measured 9.9 versus 8.6 MB/s on a fake 100 Mb/s short path.
Its unknown carrier does not execute H1 pacing. Late real-clock timer wakes
reduced the serializer's effective capacity while its input was queued; an
explicit virtual late wake forces the false relative deficit three times.
The same actual-client fixture now uses virtual time and calibrates each arm,
retaining the 90% relative gate, sizing activation and peer-window clamp.
Equal underfilling must fail even when the two-arm ratio is one. The corrected
row and three adjacent controls pass 12 race executions. Preserve the initial
failure and forced reproductions, then repeat the broad root regression with
both harness corrections. This repeat now passes 3,021 tests with zero failures
and 24 skips on `7afd9e4b`, which also passes all 177 race correctness tests.
Its production is unchanged from `538e6248`. These fixes do not close host or
SDK acceptance.

## 25. Validate recovery against real RTT changes and carrier limits

A bounded head-only ACK-tail experiment improves the forced SDK feedback-delay
case, while preserving the full-compression deadline for SACKs and eviction
notices. It fails a separate 100 Mb/s, 0.3-to-100 ms RTT-growth cell at 6.8608
versus 95.8054 Mb/s. The first candidate remains rejected. Preserve all controls and
force the interaction between shortened cumulative feedback, old ACK-byte
application and service measurement before accepting another correction.

The isolated sampler reproduction now excludes pending probes and delayed
old-byte application. A 2,672-byte head after a 49.06 ms feedback gap replaces
11,913,027 B/s with 54,459 B/s. The ordinary RTT observation leaves the existing
propagation floor unchanged. The first returning portion of a new train is
being treated as a completed measurement.

Test the proposed symmetric cycle rule: keep a gap ACK as incomplete rate
evidence until its associated measurement cycle completes. Acknowledge its
bytes and release send capacity immediately; defer only replacing the service
estimate. State the completion boundary explicitly and retain the bytes and
timestamps needed to decide whether the gap belongs to the completed sample.
Cover both gap-then-cycle and cycle-then-gap orderings, tiny first or last heads,
and the same delivered train split across different cumulative ACKs. A lone
incomplete cycle must preserve the last measured bucket. A genuinely slower
completed cycle must replace it. Do not require the old rate's expected byte
count to arrive before recognizing a slowdown; include bounded adaptation,
small windows, shared services and compressed ACK bursts as controls. The
reviewed implementation and current validation status follow below.

The first cycle implementation still recomputed old samples using the current
flight and RTT. Excluding the new partial ACK alone therefore did not provide
zero-order hold. An isolated correction returns the last accepted rate unchanged
until a real timestamp pair or fully accounted physical tail completes the
cycle. It passes the exact RTT-growth case three times. Completion must use
actually proved and applied bytes: a drained flag alone precedes coalesced
ACK-byte application. Further review also requires a newer accepted cycle to
retire older incomplete evidence, so delayed old bytes cannot restore an old
rate. Force these orderings, epoch resets, read-only statistics, changed
compression intervals and cycles longer than the bounded ring before landing.

The wider controls expose a startup limit in that hold rule: a 400 ms RTT,
50 ms compression cell reaches 79.299 Mb/s against 95.826 Mb/s for its reference.
The trace holds an early 7,232 B/s control-message estimate even after
4,998,818 newly observed bytes over 400 ms supply much larger service evidence.
Test whether an incomplete cycle may raise the held rate using its bytes divided
by the complete elapsed interval, including the gap. It must still require a
completed cycle before decreasing the estimate, preserve the old-accounting
exclusion boundary, and leave statistics reads non-mutating. Preserve the
failed startup controls separately from the passing RTT-growth and SDK recovery
checks; v9 remains rejected.

The reviewed v10 correction is now applied on `914a2a72`. Its partial-cycle
increase uses only new bytes and the full pre-gap interval; the cycle remains
pending until actual timestamps or completely applied proved delivery finish
it. Decreases still require completion. It also pins compression provenance,
separates late old-byte accounting, and keeps stats-only reads non-mutating.
The final candidate passes 393 focused and 393 race executions, all nine
RTT-growth/long-compression/forced-SDK recovery executions, six service controls
and 18 SDK phase/control executions. Two startup roots fail six times against
v9; all preceding root and adjacent failures are retained in the 22-run evidence.
The normal correctness/model runner includes the new tests. The frozen combined
correctness run finishes with 213 passes and one strict size failure: the new
delivered-byte word makes `sequenceAck` 104 rather than 96 bytes. Passing that
count directly into the compressor removes the unnecessary per-record field,
restoring the original layout and exact-size assertion. The full model finishes
with 21 passes, five failing tests and all 814 readings retained. The direct-credit
change passes 40 selected ACK/size checks under race.
The model exposes three mismatch cells with zero drops but lower capacity;
retain the original comparison gates and force the cause. Preserve physical/host
acceptance as a separate gate.

Adjacent review reproduces an additional partition defect in v10 three times:
a completed summary beyond the ring measures 10,000 B/s when one timestamp's
bytes apply together, but 1,000 B/s when the same bytes apply as 1,000 plus
9,000. The first portion completes the cycle and freezes the fallback before
the later portion arrives. Correct fixed-summary accounting for eligible
same-timestamp additions, including fresh evidence accepted before old workers
finish. Cover controller reads between portions, stats-only reads, delayed
eligible timestamps and exclusion of superseded epochs. Preserve v10's failed
root and all already-started full-suite results; do not overwrite their source
provenance with the correction. The reviewed v11 correction now updates eligible
late bytes in the retained completed summary and preserves the accepted fresh
summary when retiring older evidence. It adds no fields or unbounded history.
Five new roots and 408 focused plus 408 race executions pass; all 12 runs and
their failed controls remain in `sdk-feedback-cycle-v11-evidence`.

Two remaining estimator investigations have deterministic reproductions:

- A small physically completed flight need not occupy the link for the entire
  ACK gap. Early head ACKs expose intervals spanning window waits, and can price
  a roughly 12.5 MB/s service at about 0.6 MB/s. Hold established service across
  observations without evidence of continuous delivery; permit conservative
  increases and require actual queue/service evidence for decreases. Test the
  equal 256 KiB cases and live 2 MiB-to-64 KiB shrink, with unchanged comparison
  gates. Preserve genuine slower-service, compression, local-observer-delay,
  pending-byte and shared-service controls. A queued local writer alone cannot
  prove that the physical link was occupied.
- A one-second drain cap expires before a proved 1.2-second path can acknowledge
  its tail. Force this through `waitForServiceMessage`, then distinguish drain
  expiry, a recent mean diluted by old short-RTT samples, and retransmission
  invalidating the resumed probe. Test a bound derived from the configured ACK
  lifetime, cancellation and overflow. A 60-second cap counterfactual fixes the
  expiry root but does not repair the long-path throughput control. Per-service
  lifetime plumbing and complete recovery remain proposals pending validation.
  A subsequent trace identifies the resumed probe itself being retried after
  316 ms, before its expected 1.2-second reply. Reproduce that ordering with the
  actual send/recovery worker before treating it as the remaining cause. The
  corrected test must preserve ambiguous-ACK rejection and demonstrate recovery
  to a later unambiguous RTT sample. Include other-message and other-lane
  retries, carrier changes, loss, cancellation and ACK/write ordering. Every
  accepted repair needs failure-before evidence and the original performance
  gates, not just a trace or improved throughput.

The three confirmed drain/retry roots are now permanent in
`transfer_window_pacing_drain_recovery_test.go` and selected by the regular
correctness runner. The actual-worker root seeds a stale 300 ms lane timer,
confirms the controlled probe's physical write, observes its premature retry,
then delivers its 1.2-second covering ACK; ambiguous-ACK rejection leaves the
floor at 1 ms. Each of the three roots fails three times before correction.
The first isolated repair passes those nine executions, but all three long-path
performance comparisons still fail. Retain this scoped progress separately
from performance acceptance and complete the adjacent recovery checks.
`long-drain-root-failure-before-evidence` retains nine complete runs, 28 numerical
readings and all 44 outcomes, including failed counterfactuals.
The original 1.2-second growth performance cell is also now permanent in
`transfer_window_service_long_rtt_test.go`, included by the existing model
selection. Its fixed-memory setup, warmup, measurement duration, reference
calibration and candidate gates are unchanged.

Adjacent review of the isolated small-window v4 candidate finds that its
exact-timestamp check depends on worker application order. Applying byte
accounting before RTT metadata, or interleaving a newer sibling RTT, reprices
12.5 MB/s as 1.28 MB/s. Changing only the compression hint from 50 to 10 ms can
also reinterpret old bytes as 500 kB/s. An older RTT must not move the evidence
boundary backward. The frozen v4 roots retain nine test executions (three
passes and six failures), nine failed assertion rows and three complete
numerical observation records in
`window-mismatch-v4-arrival-failure-before-evidence`.

Record eligibility with its timestamped RTT evidence and preserve the
compression allowance under which old bytes were observed. Verify bytes-first,
RTT-first, sibling interleaving, older-worker application, read-only statistics,
repeated reads after fresh slower evidence, and bucket resizing. These controls
must keep the real slower-service assertion; a passing mismatch matrix alone
does not validate this candidate. The correction remains isolated until those
checks and its performance controls pass.

The induced host replay experiment now records one actual NAT replay per arm.
Disabling only the source-idle delimiter drops the candidate's service estimate
from 116,086,811 to 19,038 B/s; the corrected arm holds its preceding rate.
Both full throughput brackets pass their original controls, so the experiment
establishes the sampler effect without proving a throughput improvement.
Keep the older host failures open and retain all readings in
`host-replay-confirmation`.

An actual H1 carrier test exposes a separate fixture problem: sending a socket
batch with `SendMultiWithTimeout` creates one Pack that can exceed H1's unchanged
8 KiB cap. The production NAT path groups packets into carrier-safe messages.
Use that same grouping in the host fixture, with a deterministic oversized-batch
failure before the correction and actual-carrier delivery after it. Check
ownership, rejection, replay and shutdown alongside message size. Keep generic
TCP fixture budgets unchanged; apply SDK NAT shares only to explicitly profiled
physical-carrier cases. Revalidate physical H1 with the corrected fixture and
record its distinction from native TUN and the remaining relay campaigns.

This fixture correction is now applied. The 16-by-1,100-byte batch fails three
times before grouping and arrives in six bounded H1 messages afterward.
Adjacent review forces a separate canceled-workload/live-client admission
failure three times; passing the workload context to group admission fixes it.
The three cap/refusal/cancellation tests pass nine race executions. Generic
48 MiB replay settings remain unchanged, while the explicitly profiled physical
fixture uses the constructor-sized 4 MiB provider replay budget.

The ordinary `physical-h1` runner passes its first bounded A/B/A comparison on
test-source `b6abfa4e`: 91.550/91.469/91.549 Mb/s on a 100 Mb/s, 0.3 ms added-RTT,
one-flow download. All readings, calibration, ACK cost and lifecycle checks are
retained in `physical-h1-smoke`. Expand this to the affected eight-flow duplex
cell with the server-budget peer and constrained H1 provider. Keep each
direction's acceptance gate and make the actual-carrier, userspace-TUN,
unauthenticated-relay and disabled-Transfer-encryption scope explicit.

The corrected generic TCP matrix now retains eight comparisons on `b6abfa4e`:
six accepted, two one-flow/100 ms reference-calibration exclusions, and no
candidate failures. A separate 48 MiB TCP-buffer control accepts both of those
configured cells with 12 seconds per arm. Keep all default and capacity-control
readings in `tcp-grouped-fixture` and `tcp-grouped-capacity`. The control changes
both the TCP buffer maximum and measurement duration, so it does not by itself
attribute default underfill to either setting. These generic FIFO/userspace-TUN
campaigns do not close physical SDK duplex or broader host confirmation.

## 26. Break the full-duplex TUN handoff dependency

The first physical duplex experiments cannot calibrate: reference arms can
stall as well as the candidate. A fresh process with only one constant-window
arm reproduces the stall, excluding prior-arm TCP port or TUN-address reuse
as a necessary trigger. A captured stack identifies this dependency:

1. A reliable Transfer receive worker injects TCP data carrying an ACK through
   `Tun.WriteBatch`, holds the GRO lock, and waits for the TCP endpoint lock in
   the explicit user-unlock handoff.
2. That endpoint's TCP processor is waiting for outbound TUN queue space.
3. The TUN drainer is waiting for Transfer send admission. Its capacity-release
   ACK is behind receive work blocked by the first worker.

The existing pure-ACK bypass tests do not cover data carrying ACKs. Force the
cycle for both `Write` and `WriteBatch` against unchanged production, then
review the explicit handoff against gVisor's own enqueue/unlock wake contract.
The fix must retain finite-response progress without waiting for an endpoint
lock inside the shared receive path. Keep outbound queues and any handoff state
bounded, preserve borrowed packet ownership, and join all lifecycle work.

Adjacent checks must include user-owned and processor-owned endpoint locks,
GRO enabled and disabled, partial and full bursts, IPv4 and IPv6, same-flow
ordering, unrelated flows sharing a shard, and cancellation with pending work.
Retain finite-tail delivery and provider replay tests so removing the lock wait
cannot silently reintroduce the original short-response failure. Update stale
comments that assume the NAT has no return replay. After deterministic checks,
repeat the same physical duplex controls with unchanged memory limits and
per-direction calibration; preserve every interrupted and excluded attempt.

The minimal correction is now applied on source `c07140f7`: remove the redundant
endpoint lock after synchronous injection/flush, retaining the existing queues,
flow ordering and producer cadence. Two final-shape root tests fail three times
each before the correction; 19 corrected tests pass three times under the race
detector. The established 2,000-byte finite-tail test observes the cumulative
TCP ACK before calling `Read`, so the checking syscall cannot supply the wake.
The regular correctness selection includes both roots and passes all 182 tests
under race. The unchanged physical duplex A/B/A now completes, but all three
arms refuse 52-byte provider return controls; the after-reference also records
nine gVisor outbound drops. Reference calibration and A/A consistency both fail.
All readings and failures are retained; no physical throughput improvement is
claimed. The copied-source full root regression finishes with 3,026 passes,
zero failures and 25 skips, retained in `regression-tun-duplex`.

The next bounded investigation is control admission during full duplex. Shared
provider callbacks enqueue controls without waiting, and the control worker's
zero-timeout send can refuse while socket-owned data occupies H1 admission.
Force that ordering and observe a real sender's cumulative progress and RTO
before attributing the physical deficit. Review adjacent control types and
preserve nonblocking callbacks, fixed memory bounds and all calibration gates.
Whole-arm refusal counters alone do not prove measurement-interval starvation.

## 27. Calibrate both directions of reversed duplex fixtures

The SDK eight-flow duplex cells set both `Upload` and `Bidirectional`. The upload
setup clears the forward serializer; the duplex setup reinstates only the
reverse serializer. Relative comparisons can pass with the same unlimited link
in both arms. A finite, asymmetric-window reproduction measures 804.465–805.888
Mb/s in the unlimited direction of a declared 100 Mb/s link and fails all three
attempts, with zero measured relay loss. Nine readings are retained in
`duplex-fixture-bounds-before`; the fourth declared cell in each invocation is
unobserved because the preceding assertion ends the test.

Preserve both serializers whenever the workload is bidirectional, and apply the
single-direction upload swap only to one-way traffic. Check initial and changed
rates, drop policy, both orientations, and one/eight flows. Require each SDK arm
and direction to remain below the physical link rate plus a 1% interval-edge
allowance; keep all existing lower calibration, relative, window and refusal
gates. The correction and deterministic adjacent tests are applied. Both tests
pass three times under race, retaining all 24 readings with zero measured relay
drops in `duplex-fixture-bounds-after`.

The affected historical `Upload && Bidirectional` rows remain evidence of what
ran, but cannot establish two-direction performance. `sdk-transfer-source-idle`,
`model-pending-probe-observer` and `model-feedback-cycle` each retain 18 affected
rows (nine pairs). The original `sdk-transfer-model-first` used no reversed
duplex cells and is not invalidated by this specific fixture bug. Rerun affected
SDK cells after the estimator candidate is ready; preserve all earlier outcomes.

Separately, the old compression-residence counterfactual assumed full receiver
compression even after early-head ACKs became available. Its explicit fixture
now holds both arms to the advertised interval using the existing ACK hooks.
The original under-400/at-least-850 Mb/s and exact-residence gates are unchanged;
three race executions pass at 202.752 versus 958.259–958.362 Mb/s. All six readings
remain in `compression-residence-isolated-control`. Production ACK timing is
unchanged by this test isolation.

## 28. Measure receiver ACK timing directly

The requested direction is to add receiver information to ACKs and simplify
RTT/rate calculation where that information removes ambiguity. Freeze the
unlanded sender-only sampling and drain candidates as alternatives, retaining
their failure-before tests and failed performance controls. A passing isolated
window-mismatch cell is insufficient: the partial-turn defect crosses logical
lanes, and the long-path model still fails.

First add optional `Ack.receiver_ack_delay_micros` (field 12): the receiver's
elapsed time from ingress of the exact acknowledged Pack to encoding this ACK.
It includes receive handoff, ordering, application delivery and compression.
It excludes later outgoing-carrier waits. Zero is a valid immediate reply;
absence means unavailable. Use a receiver-local monotonic clock, round down to
microseconds and omit unrepresentable durations. Preserve the exact head's
ingress/tag association through compression; do not combine another message's
timestamp with the cumulative head. Missing-contract requests are not timing
or delivery evidence.

For a physically confirmed, single-copy message, calculate adjusted RTT from
the sender's actual first write, saved local ACK arrival and reported receiver
delay. No clock synchronization is needed. Keep raw residence separately for
recovery and delivery lifetime. A receiver delay cannot identify forward or
return carrier queue time, so do not call this a propagation-only measurement.
Older peers retain the existing fallback. ACK arrival must publish usable
timing even when the send worker is waiting on the pacer; explicit ownership
must also cover an ACK arriving before its physical writer returns.

Keep two measurements from the same validated message:

```
raw RTT      = local ACK arrival - local first physical write
adjusted RTT = raw RTT - receiver-reported delay
window time  = adjusted RTT + max(receiver-reported delay,
                                 that ACK's advertised compression bound)
```

For example, a 12 ms round trip containing 5 ms at the receiver gives a 7 ms
adjusted RTT. Recovery still observes 12 ms. If that same ACK advertises a
10 ms compression bound, window sizing reserves 17 ms; an actual 25 ms
application wait remains part of the reservation even with compression off.
Store those values as one observation. A later ACK's compression setting must
not reprice an older sample. Reject ambiguous or inconsistent timing while
still processing valid cumulative/selective delivery progress.

For metadata-capable peers, evaluate replacing the raw-RTT drain inference
with the bounded history of these direct observations. Keep the legacy drain
tests and behavior independently visible. Passing metadata tests must not
hide failures for peers without the optional field.

For service rate, define one bounded receiver collector across the logical
lanes that share the sender's service, with the same role/companion identity
and no transport-path identity. Count the original encoded envelopes in the
same units as pacing. Publish complete, identified samples rather than adding
separate lane fragments at the sender. Compare receive and matched send spans
before treating a lower offered rate as lower capacity; receiver timestamps
alone do not prove that a sender/window idle gap occupied the link. Evaluate
correlation using the acknowledged message's existing physical write record
before adding another Pack wire field. Keep sample history bounded and hold
the last valid sample when no new complete measurement exists.

Required deterministic checks:

- Wire presence, explicit zero, maximum values, malformed input, both codecs,
  pooled decoder reuse and an older peer ignoring the added field.
- Receiver queue/compression delays, head absorption, oldest-first SACKs,
  duplicates, retries, mixed peers, stale sequence/sample generations and
  exact message/tag pairing.
- ACK-before-write confirmation, cancellation, failed/carrier-changed writes,
  coalescer arrival during pacing, local worker delay and delayed observations.
- Fixed path with changed receiver delay versus a real path-RTT increase;
  unchanged raw recovery lifetime and bounded memory/message sizes.
- Partial sibling feedback, real slower service, window idle, rate changes,
  coalescing/reordering and zero-hold reads, preserving the original mismatch,
  400 ms and 1.2-second performance gates.

The first wire checkpoint adds field 12 without enabling timing behavior yet.
It passes 24 race executions and two non-race allocation checks on source
`8ae48d8a`, including maximum wrapped entry size against the unchanged
reservation. Compact send-item/sequence-ACK/receive-ACK sizes remain
584/96/104 bytes; the decoded frame owner is 680 bytes. Full results are in
`ack-receiver-delay-wire-checkpoint`.
Validate CPU cost as well as virtual-time goodput: the rejected v6 sender
candidate increases full-ring held reads from about 174 ns to 5,839 ns in the
paired under-load sample, with unchanged rates and zero allocations. This
measurement is retained in `pacing-estimator-v6-rejected`; it does not accept
that candidate. The receiver design should support constant-time completed
sample reads and avoid the rejected all-pairs scan.

The first shared timing prototype uses a single bounded history scan rather
than an all-pairs scan. On frozen candidate `12ea2bb4`, populated-history reads
take 405.4–472.7 ns/op and timing publication 285.2 ns/op, with zero bytes and
allocations per operation. All seven benchmarks pass; the six service reads
retain the same 12,500,000 B/s estimate. Preserve this measured simple version
until a real cost warrants added cache/deque machinery. Results are in
`receiver-timing-estimator-cpu`; combined performance remains unvalidated.

The receiver/sender/shared-timing implementation is now integrated. Its final
focused selection passes 162 race executions. Full correctness on copied
source `733ee2d2` retains 257 passes, three legacy drain failures and two test
fixture race warnings; it is not accepted as clean. A subsequent actual
H1-to-H3/mixed-route control reproduces incorrect shared timing selection and
passes after restricting shared metadata to the current H1-only policy.
Other H1 siblings retain their service history.

Compact send-item/sequence-ACK/receive-ACK records remain 584/96/104 bytes,
but total retained timing state does grow: 2 KiB per default 128-sample RTT
ring, 8 bytes for its monotonic observation bound, 32 bytes for the pending
exact reply, and a lazy 4 KiB shared history plus its owner and pointer. The
prototype owner was 72 bytes; retaining a separate unloaded baseline makes the
hybrid owner 112 bytes.
Ingress stamps add 8 bytes per receive record and the client clock origin
adds 24 bytes. Keep those costs distinct from wire size and hot allocations.

The unchanged long-RTT model still fails (9.482 versus 95.771 Mb/s, with 328
measured relay drops) even after adjusted RTT rises to 1.868 seconds. Trace
the first pacing/window divergence and reproduce its cause deterministically;
do not add another inferred-rate rule or relax the gate to hide this result.

The first transition trace rejects an assumption of the prototype: subtracting
receiver delay leaves carrier queueing in adjusted RTT. Its 128-sample minimum
can therefore grow with a standing queue (269–276 ms on unchanged 0.3 ms
propagation). Metadata presence alone cannot justify disabling empty-flight
measurement. Add paired constant-propagation/queue-growth and real-latency-step
roots, and separate observed adjusted residence from an unloaded-path floor.
Use an explicitly drained, physically confirmed message to refresh that floor;
its reported receiver delay can remove compression/application residence
without another inferred-delay rule. Keep the metadata wire/lifecycle tests
independent of this rejected consumer policy.

Adjacent lifecycle review finds a second-sampling defect: retry preparation
clears an already-observed metadata SACK, allowing a subsequent reply without
the optional field to enter the legacy tag sampler. Preserve the consumed
state for that message's entire lifetime. Four explicit preparation/success/
failure/unreliable retry variants fail in all three before executions; the
correction passes 72 focused timing executions under race. The sampler must
still reject an ambiguous retry that has never supplied valid exact timing.

The existing echoed `Tag.send_time` is not an attempt identifier. Ordinary
retries reuse the stored encoded Pack; a forced real sender retry 300 ms later
retains exactly the original tag. Promoted heads may receive a fresh tag, while
SACK processing refreshes local lifetime state without updating that stored
wire tag. Receiver delay therefore cannot make every retry unambiguous. Keep
exact first-write matching until an explicit per-attempt protocol is justified;
test common raw-residence recovery bounds against actual long replies, proven
lane loss, cancellation and the original absolute lifetime limits.

Hybrid comparison: keep the validated ACK timing producer and exact first-write
matching while comparing (1) the broad rolling-timing consumer, (2) the prior
raw timing consumer and (3) an unloaded-baseline consumer using exact receiver
delay. Use the same frozen source inputs and original gates to identify which
previously passing cells regressed. Do not combine carrier queue time with a
new propagation baseline or let metadata presence disable necessary drain
proof. Separately replay the 2 MiB to 64 KiB receiver change with 100 ms RTT and
50 ms compression: its unchanged 100 Mb/s service drops to a 0.61 MB/s estimate
after compressed, window-limited feedback. Establish that root before adding
another rate heuristic or broader receiver service schema.

The frozen hybrid passes all 18 SDK bidirectional comparisons; the broad and
prior raw consumers fail three gates and one gate respectively. All variants
still fail the shrinking-window cell. The broad consumer's apparent long-path
aggregate pass contains two empty one-second intervals and a final 270.664 Mb/s
release on a 100 Mb/s link. Require every measured interval to retain capacity,
alongside the existing aggregate, fairness and measured-loss checks.

The hybrid is integrated with deterministic unloaded-baseline and exact-probe
tests. Keep the two remaining investigations independent:

1. Force an old flight-tail retry during the drain, then a logical ACK that
   cannot prove the latest physical copy arrived. Evaluate explicit bounded
   attempt/probe feedback only if that proof shows the existing metadata is
   insufficient. A late echo may need to survive logical item release and head
   absorption, while preserving one head, bounded oldest-first SACKs, maximum
   encoded size and the original cancellation/recovery limits.
2. Preserve the compressed-small-flight replay: an opening reply releases
   another write before the preceding tail arrives, keeping logical flight
   nonempty despite a real idle gap at the serializer. The current estimate
   falls from 12.5 MB/s to 605,181 B/s in all three forced executions. A blanket
   gap/flight-size exclusion is rejected because eleven existing complete-slow-
   cycle and drained-train controls fail. Use matched offer/receive evidence
   before accepting lower capacity; no tuning-only exception closes this root.

The exact probe's existing raw-residence recovery bound now applies to both
metadata and legacy peers. Separate actual-worker head/SACK roots fail six
executions before and the corrected focused selection passes 150 under race.
The full long-path model still fails at about 12.37 Mb/s, so this bounded fix
does not establish completion of either investigation above.


### Adjacent delivery accounting and recovery checks

The hybrid comparison restores all SDK bidirectional gates but still fails
small-window, long-RTT and slow shared-relay cells. Keep separate deterministic
roots and original performance gates for each; an isolated root pass is not
permission to close its complete cell.

- **Publish service credit at arrival.** Force the sender worker to remain
  paused after real writes, then acknowledge those writes through the actual
  coalescer. The shared service must receive their original wire bytes at once,
  while callbacks remain pending. Test SACK duplication, cumulative absorption,
  temporarily removed retries, sparse/maximum heads and cancellation between
  taking and publishing credit. Nine before failures establish the root;
  nine root, 240 focused and fifteen adjacent race executions pass afterward.
  The integrated final candidate passes 738 race executions, including three
  repeats of every original finite-relay capacity, and restores all six failed
  static 256 KiB mismatch cells with their original gates. Large retained-window
  benchmarks show zero allocations and bounded-prefix work. No item or ACK-record
  growth is needed. Full combined validation and the shrinking-window/long-RTT
  transitions remain separate completion gates.
- **Start retries at physical dispatch.** Force a local pacing wait longer
  than the ordinary retry interval, then confirm the first physical write.
  No duplicate may occur until a full interval after that write. Updating an
  already queued deadline must repair both recovery heaps under their lock.
- **Share confirmed raw recovery evidence.** Let one H1 sibling establish a
  longer raw round trip while another retains a stale short local estimate.
  Re-evaluate scheduled recovery against that evidence within the original
  lifetime/cap rules. Force receiver-proven loss and explicit recovery at the
  due boundary; neither may be delayed by a healthy-sibling observation.
- **Separate missing information from consumer defects.** Test measured field
  12 before proposing more ACK fields. The named head's delay correction and
  first-offer rollover both still fail the unchanged shrinking-window model.
  Explore bounded receiver-local ingress counters/timestamps carried through
  actual ACK arrival in an isolated test ledger, with original encoded byte
  counts, no live receiver reads and no expected-rate input. Account for any
  proposed wire overhead before recommending a schema change.

Full regression on integrated hybrid/common-probe source `fffee019` records
3,133 passes, two failures, 25 skips and no races. Preserve the silent-lane
admission failure and the storm control's missing precondition as open items;
separate unchanged-binary replays do not supersede the failed campaign. The
earlier correctness selection was clean at 291 race passes. Service credit
raises that to 299; the integrated first-copy/shared-raw recovery correction
passes all 305 checks on source `44459e42`. Full static mismatch passes all 108
comparisons on preceding service-credit source `8802ba4b`.

The storm control is now deterministic and explicit: constant-window arms
preserve the original queued-window/defer mechanism; a separate default-pacer
arm requires no retries or timeout deferrals. Three precondition failures and
nine final race passes document that fixture correction. It does not explain
the still-failing silent-lane admission test. Also keep the analogous retry
clock open: a 300 ms due retry paced until 1.2 s schedules its next 600 ms timer
at the already-past 900 ms boundary, reproduced in three actual-worker runs.

The shrinking-window pair consumer using corrected ACK times is rejected despite
its isolated capacity passes. Two replies compressed on the reverse path can
produce an impossible 64.38 GB/s estimate even after bounding by the actual
offer span, because the initial writes shared a timestamp. Actual-worker tests
with 50 ms propagation in each direction and zero/50 ms receiver delay pass
six times on the original sampler and fail six times on this candidate.
Receiver-local ingress pairs pass the same six checks, but their optimistic
zero-added-wire-overhead long-path model remains below its unchanged gate.
No new schema is justified as complete until overhead, lifecycle, clock scope,
ACK bounds and all performance gates are tested.

### Hybrid follow-up: proved corrections and remaining gates

The physical retry clock is now integrated. A retry due at 300 ms but written
at 1.2 s must receive its existing 600 ms backoff through 1.8 s. Exact-carrier,
fresh-waiter, failure, cancellation, maximum-interval and original ACK-lifetime
controls pass, together with monotonic first-copy deadline coverage: 138 race
passes after twelve final-shaped before failures. This does not change the
service estimator or introduce a timer constant.

The silent-lane case now reproduces in virtual time with its original
20-second stall, 3,000 messages and recovery gates. Its root is an impossible
physical drain: all logical bytes are credited, but a retried tail cannot prove
which copy arrived. Waiting 40.512 seconds exceeds the caller's 30-second
admission limit. The integrated correction rejects that measurement and wakes
an active waiter when a retry, failed/changed carrier or pending cancellation
removes its proof. It does not mark physical delivery or change the RTT floor.
Maintain eligibility with one per-service count, updated at existing tail
mutations, so an ambiguous idle sibling cannot trigger repeated whole-map scans.

Eleven new tests cover before/during invalidation, restoration by a fresh tail,
repeated invalidation and preservation of an unrelated live probe. The original
silent case now recovers in 5.13 seconds after the stall in all three runs.
The final before selection has nine passes and 33 failures; the combined focused
selection has 621 race passes. Fresh full correctness has 327 race passes on
`23fb1ed0` and again after the accounting lookup refinement on `7c0a2f4f`.
Full regression on `23fb1ed0` finishes with 3,963 passes, zero failures and
27 skips. Its full performance model has 24 passes and three failing tests,
with all 818 readings retained and concurrent host work recorded. In addition
to shrink and long RTT, two 10 Mb/s, 300 µs RTT, 50 ms-compression service
cells miss their throughput floor. Matched checkpoint, retry-only and drain
variants now give 5.270, 5.250 and 0.389 Mb/s on the unchanged long-path gate,
respectively. All three already fail both short compressed cells. Preserve the
physical retry correction; isolate the drain admission/abort interaction with
service estimation before accepting that combined change. The third arm still
passes all 11 drain-liveness roots, so a replacement must keep both properties.
The first long-path trace locates the collapse during a successful drain,
before any abort: 37,422 bytes across 601.17008 ms replace the held service with
62,248 B/s. Force that exact averaging boundary in a deterministic root. Do not
attribute it to abort-marker clearing, which the trace does not observe.
The forced partial-feedback schedule reproduces on both old and corrected
drain implementations. Most of the sampled gap precedes the drain itself;
controller read order determines whether the successful probe captures an
already-collapsed hold. Preserve the same-write/same-ACK no-read and read-only
controls, and reject a blanket hold that only conceals the earlier gap.
An exact original-write trace further distinguishes the gap from sender idle:
the selected bytes were offered continuously across the propagation change,
despite a roughly 603 ms ACK separation. Preserve an explicit latency-step
control; contemporaneous waiting cannot relabel those earlier offers. Receiver
ingress timing removes reverse-path ambiguity, but a pair crossing a forward
delay change can still include that discontinuity and needs recovery coverage.

Keep shrinking-window capacity and long-RTT sustained capacity open. Receiver
counter/clock diagnostics also expose physical-versus-logical byte confusion
and pacing feedback into the rate estimate. Correcting those isolated roots
does not pass the complete shrink gate. Reject a rule requiring offered rate
to reach the held rate: a genuine slowdown must still be learned while the
sender offers at its existing 0.95 pacing factor. Any receiver-local bucket
candidate must pass sparse delivery, true capacity changes, source/window idle,
bucket-phase shifts, retries and reverse ACK compression before proposing wire
fields. Include all added wire bytes in final performance and ACK-size tests.
The receiver-clock/same-train diagnostic passes shrink, both short compressed
cells, capacity changes and cold startup on the frozen drain source. Its initial
long-RTT miss is a delivery-history window clamp after a proved RTT increase.
Retiring only the pre-proof window history passes all three original long-path
intervals at about 95.77 Mb/s. The corrected probe-confirmation ordering and
adjacent controls pass 174 focused race executions. Keep service observations
separate from unloaded RTT proof, preserving peer, configured, target and memory
bounds and the original liveness and loss checks.

The independent production port of that history rule is integrated with the
original service sampler. Its eleven roots, including current-carrier isolation,
give 15 passes/18 failures before and 33 passes after, under race. Full
correctness passes 338 tests. Keep this proved correction separate from the
experimental receiver service consumer and validate production performance.

The full diagnostic matrix exposes a further regression: six static 256 KiB
send-window / 300 µs RTT / 10 ms compression cells deliver about 195 Mb/s against
594 Mb/s references. The final window is unchanged and the service hold remains
at an earlier 25.398 MB/s legacy startup estimate. The trace has zero usable
receiver pairs: declaring receiver evidence available freezes ordinary discovery
before the new estimator can measure anything. Force the ownership handoff and
preserve ordinary discovery until the first usable receiver pair, then preserve
established receiver evidence through single-head trains. A scoped long-path
pass cannot justify integrating a candidate that loses these previously passing
cells. The full diagnostic model finishes with 25 passes and two failing tests;
the other loss is one short-path SDK bidirectional direction at 768.01 Mb/s
against 957.19 Mb/s. Attribute that separately and retain its per-direction
gate even though combined throughput improves.

Add forced receiver-queue stall/drain coverage as well as reverse ACK
compression. The current observation clock is at `Client.run`, after a bounded
carrier handoff; neither exact nanosecond encoding nor receiver ACK delay removes
that queue's compression. A carrier timestamp needs a bounded per-message
handoff and does not remove earlier socket buffering. Evaluate matched sender
and receiver intervals against both continuously paced and buffered-burst
controls, preserving genuine capacity increases and decreases. The larger
matched interval fixes continuously paced queue compression but still fails
three buffered-burst controls; retain that negative result. Keep sampler
diagnostics distinct from an actual forced queue/worker reproduction.

The real-worker reproduction is now available: two Clients and the production
reliable handoff retain 23 of 24 serialized frames in the existing 32-message
queue. Releasing the Client barrier inflates the receiver estimate and its
actual next burst; raw and max-span consumers each fail three times while
unstalled and genuine rate-rise controls pass. The narrow queue-state candidate
withholds already-queued endpoints while still counting their physical bytes;
it passes nine worker and 75 expanded race executions. Check the unchanged
performance cells before adopting this rule, and separately audit buffering
before the carrier reader. A local-queue proof is not a kernel-arrival clock.

The narrower startup handoff completes the full original performance model:
27 passes, zero failures and all 818 readings on effective inputs `b7e1d4b8`.
The queued-endpoint safeguard separately passes eight affected performance
tests with 38 readings and then all 27 original model tests with 818 readings.
The explicit upstream carrier-reader stall still fails, including bucket-phase
changes. Requiring sender agreement fixes the initial placement but fails after
10/20/50 ms release delays, because both clocks can be compressed. It also
regresses cold discovery and a genuine rate increase in the isolated roots.
Neither candidate is accepted. A passing idealized serializer cannot establish
that a userspace read timestamp measures physical arrival under buffering.

The [failure-condition inventory](testdata/throughput_root_cases/README.md)
now links every current failure class to executable cases. Thirty-three new
production tests produce 39 passes/60 failures over three race repetitions on
`17780670` plus tests; twenty failing cases reproduce every time. This baseline
excludes the working drain correction and includes its eleven liveness roots.
The checked-in
receiver replay preserves four experimental variants, 36 selected roots and
controls per variant, and all 432 outcomes. Run these assertions before accepting
any further hybrid, lifetime or service-sampler correction. Keep the original
full model and all affected-cell gates unchanged.

The window-history correction is independently committed in `17780670` after
33 race passes on the prior commit without experimental drain changes. With
the working drain variant and original service sampler, full correctness
passes 338 tests but the full model still records the same three failing tests
and 818 readings. This isolates window-history correctness from service-rate
acceptance; it does not close the remaining performance findings.

Before promoting receiver service feedback, define bounded shared-source
ownership, counter reset/generation handling, and the named Pack's immutable
ingress tuple. Include physical retry bytes between valid endpoints while
retaining once-only logical ACK credit. Repeated heads, mixed cumulative
credit, carrier changes and retired or retried endpoint identities must not
cross an incompatible sender train. Test production ingress through both
decoders and actual ACK encoding; the current prototype conveys receiver
observations through a test-only lookup with zero wire cost. Require maximal
legacy/encrypted response-size tests, oldest-first SACK pacing, and complete
performance measurements with the added bytes before acceptance.

An independent timer already enforces a 500 ms ACK lifetime after a first write
at 400 ms, even if its recovery deadline is 700 ms. Preserve that passing
control. Separately fix the older pacing-wait overrun: a first write held until
600 ms currently writes and retires after a 500 ms lifetime on both current and
pre-anchor sources. Force expiry while waiting in both the service FIFO and
the active pacer, cancellation, shorter/longer lifetimes, carrier changes and
the existing reliable-lane retained-recovery exception before choosing a bound.
The initial diagnostic has six passing controls and six open before failures;
no production correction is included yet.

The subsequent isolated per-message timer bound passes its focused roots, but
the adjacent shared-worker schedule remains open: a younger pacing wait also
delays retirement of an older unacknowledged record. Force both records with
distinct send/deadline times, and retain non-regenerable recovery exceptions.
The before and candidate both fail that schedule three times. Avoid accepting
the narrow fix or adding a per-message timer/heap scan before reviewing the
worker's complete pending-deadline ownership.

Repeat combined validation after independently proved corrections are
integrated, without waiting for system quiescence. At this checkpoint, the
database-backed server tiers remained deferred; fresh deterministic server
runner attempts failed bootstrap before compilation and could not be reported
as test passes. Section 40 records the later configured runs.

### App-reported network changes and estimator stability

First validate the core window policy and adaptive pacing against the existing
deterministic roots and complete performance matrix. Event plumbing is deferred
until that comparison establishes viability. Then implement and evaluate the
agreed `NetworkQualityChanged` signal. Between notifications the learned window may grow from valid evidence
but must not shrink. Pacing still adapts in both directions from sustained,
valid feedback. A quality notification opens a bounded period in which fresh
measurements may also shrink the learned window. Peer receive limits, explicit
byte ceilings and available memory remain hard bounds on the effective window.
Do not learn the full advertised or memory ceiling merely because it is
available; start small and grow as the workload earns a larger allowance.

Preserve the last valid service estimate through partial feedback, window gaps
and bucket rollover. A reported path or radio-quality change should open a
bounded period of faster remeasurement.
The notification does not itself specify the new rate or prove that a queue
drained. Sustained congestion and capacity changes must still be detected when
no app signal is available.

The existing `DeviceLocal.NetworkChanged` / `connect.NetworkChanged` path
already carries app notifications into connect and causes transport reconnects.
Use that event for an actual path switch and also invoke quality remeasurement.
The separate `NetworkQualityChanged` entry point covers cell signal bars,
cellular technology/type changes and Wi-Fi signal bars. It requests estimator
remeasurement without reconnecting transports, resetting multi-client liveness
or rebuilding a working mux. Coalesce repeated notifications;
the host callback must remain nonblocking and subscriptions must end with their
owners.

Later event behavior, not yet implemented:

- On a new network generation, retire the old measurement history and treat
  the last valid estimate as a provisional zero hold. Permit a smaller learned
  window only after fresh evidence qualifies the new sizing period. Collect
  fresh service and unloaded-RTT evidence promptly within the existing byte, memory and peer
  bounds. Do not reset to the configured maximum or grant a full-window burst.
- Keep logical ACK credit, recovery ownership and lifetime deadlines intact.
  Delayed old-path feedback may acknowledge messages, but must not qualify the
  new path's rate or RTT. Keep independent services and unaffected paths scoped
  to their own evidence.
- Signal both traffic directions, including provider egress changes. A
  `DeviceLocal.NetworkQualityChanged` call applies to its local clients and,
  while providing service, also notifies clients using that provider. A device
  can hold both roles simultaneously; apply each event once per affected
  estimator. A received peer event affects only that peer's local senders and
  must not be rebroadcast. Prefer a bounded reliable peer control message so
  idle and send-only peers receive the hint without waiting for a new ACK.
  An event marker must not relabel old measurements as new merely because
  control or ACK encoding happened later.
- Return to ordinary stability after fresh evidence establishes the path.
  Missing, duplicate or noisy notifications must not hold discovery open,
  freeze a real slowdown or repeatedly replenish probe credit.

Required deterministic comparisons:

| Condition | Required result |
| --- | --- |
| Capacity increase/decrease and RTT increase/decrease, with and without a signal | Faster convergence after a signal; continued bounded adaptation without one |
| Sustained slowdown without a signal | Pacing slows; learned window does not shrink |
| Quality change followed by qualified lower capacity | Learned window can shrink, then resumes ordinary grow-only behavior |
| Workload growth below a large advertised ceiling | Window grows from evidence without immediately reserving the whole ceiling |
| Peer or memory limit falls without a signal | Effective admission obeys the new hard limit without treating it as a learned capacity decrease |
| Unchanged path plus an app signal | No invented capacity, queue drain or ACK credit |
| Repeated cell-quality signals during a burst or idle | Bounded probing and coalesced work; no cumulative burst allowance |
| Old ACKs arrive after a switch, reordered with new ACKs | Delivery remains correct; old evidence cannot seed the new estimate |
| Only one of several services/paths changes | Unaffected estimates and pacing remain stable |
| Local upload and remote-provider download | The appropriate sender receives the change indication |
| Provider egress changes, including an idle provider | Connected clients receive the same bounded remeasurement hint |
| One DeviceLocal is both client and provider | Its local clients and provider clients are notified once; other peers remain scoped and no echo loop forms |
| Legacy peer or lost notification | Correctness and ordinary congestion response remain functional |
| Maximum compressed ACK response | One head, bounded oldest-first SACKs, and all added metadata fit the carrier maximum |

Run all six lifetime-v7 service-root failures unchanged against this candidate.
The event can reduce the need to infer path changes from ambiguous feedback;
it cannot validate compressed carrier reads or incomplete ACK measurements.
Preserve the original small-window, genuine-slowdown, startup and finite-relay
performance gates. Keep this research separate from the independently proved
lifetime and ACK-identity corrections.

The first retained-only experiment is rejected as a complete fix: the original
model moves from 25 passing/5 failing tests to 20 passing/10 failing tests.
Long-RTT openings cannot grow promptly when only the cumulative history may
authorize growth. Test the revised candidate's independently measured service
rate as a second growth source, including a one-reply rejection control and
current-carrier ownership. Retention must never hide inflated service or freeze
the pacer's response to a real slowdown. Preserve failing candidates and
separate source pins for sampler fixes, sizing policy and their combined run.

Also measure sender CPU work as retained flight grows. Both isolated race
performance comparisons reached their 30-minute limits inside the four-cell
400 ms subset. A read-only process sample found active full-flight scans in
`scheduleSelectiveAckRecovery` and `observeRouteStall`; this is a cost concern,
not evidence of a new retained-policy regression, because the constant-window
reference and preceding source exercise those scans too. Add controlled
benchmarks across 32, 1,024 and 16,384 retained messages, separating an
ordinary cumulative ACK, actual SACK holes and route-stall observations. Record
allocations and per-operation work. Any optimization must retain exact recovery
ordering, retired-carrier behavior, ACK lifetime and route-attribution tests.
Virtual-time goodput alone does not establish acceptable host CPU cost.

## 29. Associate receiver waiting time with exact delivery credit

The remaining window-reduction trace shows why ACK arrival spacing alone is
insufficient. Two heads arrive 50 ms apart, but their measured receiver waits
change from 0.212 ms to 49.143 ms. The corrected interval is about 1.069 ms;
13,355 newly acknowledged bytes represent about 12.5 MB/s, not 0.267 MB/s.
A later held ACK must not make earlier refill gaps look like serialization.
The SDK cold-start trace exposes the dual qualification issue: useful first
train endpoints have measured waits of zero and 1 ms, while the advertised
10 ms timer rejects their shorter interval.

Test the following hypothesis using the existing receiver-delay metadata:

```text
delivery interval = (ACK arrival 2 - ACK arrival 1)
                  - (receiver wait 2 - receiver wait 1)
```

This requires exact attribution. `coalesceReceivedAck` currently publishes
timing and delivery credit separately, and sibling callbacks can interleave.
The newest service timing tuple cannot be borrowed for unrelated bytes. Carry
eligibility and receiver delay from the same validated message/tag into its
once-only ACK credit. Keep the raw arrival clock for recovery, physical drain
proof, expiry and ring ownership; use corrected intervals only where both
delivery endpoints qualify. Do not add receiver-clock synchronization or infer
a missing timing endpoint.

The new real-coalescer baseline uses confirmed first H1 writes and overlapping
flight. A 12.5 MB/s pair is misread as 0.25 MB/s, and a genuinely slower
12.4 MB/s pair is misread as 0.248 MB/s. Both fail three times under race;
the equal-wait 0.25 MB/s control passes three times. These roots establish
distorted capacity measurements, not acceptance of a replacement estimator.

The first warm candidate passes all nine repeated receiver-interval checks,
but its combined six-gate model still has four passes/two failures. In the
window-reduction trace, the corrected pair retains about 12.5 MB/s at 4.023 s;
the later loss begins only after that pair ages out. Cycle start still uses
the advertised 50 ms timer plus its gap allowance, while cycle completion
uses actual receiver waiting. Repeated immediate replies can therefore let
refill silence enter the rate after the qualified pair expires. Reproduce
that transition with overlapping sibling flight and retain a genuinely slow
serializer as the downward-adaptation control.

The first combined interval/cold/refill candidate passes its focused six-gate
selection, but its full matrix ends with 28 passes and two failures. The same
binary repeats the changing-window loss: 3.953 versus 4.543 Mb/s, without
drops. A 300 microsecond, single-flow SDK duplex cell also loses capacity in
one direction: 804.833 versus 957.235 Mb/s, despite higher aggregate throughput.
RTT growth passes in this full run. Preserve all 824 readings in
`model-receiver-refill-full-v1` and retain the per-direction gate. Treat the
focused result as insufficient. Also force a delayed prior ACK followed by immediate feedback:
a previous 49 ms receiver wait can hide a real 50 ms refill gap inside only
1 ms of raw ACK spacing. Cycle continuity must account for both exact endpoint
waits when they qualify, with legacy and genuine slow-serializer controls.

The paired-gap roots reproduce both false slow service and suppressed genuine
slowdown, failing all six repeated executions before correction. The combined
consumer comparison on identical corrected-endpoint bounds improves ten roots
from 12 passes/18 failures to 30 passes, and the affected race selection from
194 passes/six failures to 200 passes. The same after source `f305b789` passes
the six actual throughput gates. These results support using exact receiver
waits consistently across consumers; they do not replace the pending full
matrix or identify one individual consumer as the cause of RTT recovery.
Four accepted mixed-feedback root definitions omitted from both copied sources
now pass three times in both arms. Integration audit also finds an unintended
rollback of the accepted cumulative-delivery pacing fallback. Four unchanged
roots fail three times each on that snapshot; restoring only the helper makes
all 36 executions pass. Its six-gate performance follow-through still fails
RTT growth, so retain the restoration and investigate the remaining loss.

A further actual-coalescer root exposes an adaptation gap: interleaved exact
receiver waits can invalidate every raw-attached interval in the 64-bucket
ring. With continuously preoffered traffic and a physical 4 MB/s serializer,
the paired case keeps the old 12.5 MB/s rate through sixteen final controller
reads. Both callback orders fail in each of three repetitions. Shared and
per-sibling bytes, retained flight and queue preconditions pass; the identical
legacy and mixed-metadata controls adapt correctly. Preserve this before proof
in `window-receiver-interleaved-slowdown-root-v1`. A replacement must recover
from sustained genuine slowdown without accepting compressed short peaks or
discarding exact byte ownership. Do not infer that this mechanism explains
the separate RTT-growth throughput failure without matching trace evidence.

The bounded correction now passes its matched seven roots three times each
(18 passes/three failures before, 21 passes after), plus all 507 scoped race
checks. When every interval byte has its own validated wait but endpoint order
is invalid, a sustained continuously queued interval still supplies a conservative
rate ceiling: divide all interval bytes by the raw span minus the largest wait.
Require that shortened span to cover the existing full-feedback duration and
that flight still exceeds the measured bound. This may only lower an established
rate. Cold discovery, capacity increases, short peaks and refill gaps retain
their existing guards. Preserve both the initially invalid fast-control stimulus
and the corrected matched proof in `service-receiver-invalid-slowdown-v1`.
The separate RTT-growth model still fails at 48.995 versus 95.771 Mb/s; the
local proof does not establish complete performance acceptance. Add CPU coverage
for full invalid rings as well as valid, mixed and legacy controller/statistics
reads, retaining every metric and unchanged correctness gates.

Before accepting a fix:

- Preserve exact cumulative/SACK byte credit, duplicates, covering heads,
  receiver timing tag validation and original physical-write identity.
- Force sibling callback interleaving and delayed accounting. Recent metadata
  followed by legacy feedback must not lend its receiver wait to that feedback.
- Cover equal waits, changed waits, corrected slower service, nonpositive
  corrected intervals, cold startup and short qualified trains.
- Use consistent delay eligibility when starting and completing a feedback
  cycle. Audit pending-cycle increases, sustained queued averaging, completed
  cycle fallback and ring resizing so a later consumer cannot silently replace
  a corrected interval with its raw spacing.
- A cold short pair needs both validated endpoints. Keep one-reply and legacy
  controls, plus a real stalled-carrier-reader case: receiver waiting does not
  make buffered userspace reads a valid capacity increase.
- Retain buffered-carrier upward guards and legacy fallbacks. Receiver delay
  cannot explain queueing before the receiver takes its timing observation.
- Re-run changing-window and SDK cells with the RTT-growth, real slowdown,
  capacity-increase and finite-relay controls unchanged. Then run complete
  combined correctness/model/regression and CPU coverage on one frozen source.

Keep these changes separate from future `NetworkQualityChanged` events. An
external quality hint cannot make an incorrectly attributed interval valid.

## 30. Audit test contracts before further estimator changes

The user requested a review of whether existing tests admit all valid behaviors.
An unchanged test is evidence of compatibility with its assertion, not proof
that the assertion is the right contract. Preserve the frozen comparisons while
reviewing the expected behavior independently of each proposed implementation.
The agreed policy remains a learned window that grows between quality events,
effective admission within hard bounds, and pacing that eventually adapts in
both directions from sustained qualified evidence. It does not prescribe every
intermediate estimator value or a particular bucket representation.

| Assertion or family | Contract assessment | Follow-up |
| --- | --- | --- |
| One cumulative head; bounded oldest-first SACKs above it; head absorbs lower SACKs | Explicit protocol requirement | Keep exact ordering, pacing, count and encoded-size assertions, including metadata and wrapper overhead. |
| Exact once-only byte credit, receiver timing attached to its own head, pool ownership, hard memory/peer bounds | Correctness invariants | Preserve across every estimator representation and control path. |
| Learned window never shrinks without a quality event; pacing can still adapt | Agreed policy | Preserve retained-window and real capacity-change controls; distinguish learned capacity from effective admission clamps. |
| `ReceiverInvalidQueuedBoundCannotRaiseService` globally requires at most the seeded 1 MB/s after 192 physical 4 MB/s cycles | Overconstrains the complete estimator | The conservative decrease-only bound must not increase service. An independent qualified receiver-clock measurement may do so. Test the branch contract separately from sustained upward adaptation. |
| Interleaved-slowdown fixture requires all 64 internal samples to remain `receiverInvalid` | Representation-specific precondition | Retain it in the original causal proof. Acceptance of another representation should use the same physical inputs, exact credit and eventual rate bounds without requiring that internal flag. |
| New late-sibling roots require exactly 12.5 MB/s, or 1 MB/s, immediately after the last of a few heads | Exact sample arithmetic plus an unagreed response deadline | Exact arithmetic belongs in a qualified measurement test. The controller may briefly defer or smooth that evidence; separately test bounded convergence over sustained offered traffic. Original 6-pass/9-failure proof remains frozen. |
| Earliest endpoint owns its excluded bytes; tied earliest checkpoints exclude all tied bytes; an unproved same-sequence interval grants no invented sample | Measurement correctness | Preserve these bounds. Keeping separate receiver-clock extrema and byte ownership may satisfy them without declaring every raw/corrected ordering mismatch unusable. |
| Dedicated TCP control test requires the exact output sequence `[old ACK, new ACK]` | Overconstrains a valid cumulative protocol | Permit one latest cumulative ACK or old followed by latest. Require monotone heads, newest-byte coverage after capacity returns, bounded pending ownership and cancellation. |
| A refused pure TCP ACK is treated as protocol failure | Too broad | Ordinary TCP can recover it. Zero provider refusals and progress without another TCP event are performance/liveness objectives for this candidate, not proof that every refusal corrupts the stream. Review calibration separately. |
| Canceling one unadmitted caller retires a shared send or forward sequence | Independent lifecycle defect if reproduced | Require unrelated admitted work and shared lane ownership to survive; retain real sequence-closure/recreation controls. |
| New provider-cancel test expects the client to survive, but its fixture assigns provider and client the same cancel function | Invalid lifecycle fixture | Give the provider its own child context before starting workers. The failure appears before and after the candidate; production must not be changed to satisfy the contradictory setup. |
| New test writes the send-worker hook after `NewClient` starts the key-publisher worker | Test-created data race | Install the hook through settings before construction. Preserve the original race reports and repeat the same ownership/cancellation assertions on the corrected fixture. |
| SDK throughput after `300 ms + 5 RTT`, and finite shared relay after two seconds | Explicit convergence/performance targets | Keep original outcomes; characterize cold start, recovered steady state and time to recover separately before interpreting a miss as incorrect measurement. |
| RTT-growth throughput must reach 90% in each one-second interval after eight seconds of recovery | Stronger than aggregate throughput; each bucket is shorter than the 1.2-second RTT | Record phase sensitivity and intervals spanning complete feedback cycles. Any revised recovery target needs a stated rationale and a comparison against the original gate. |
| Duplex candidate must dominate 90% of each reference direction | Fairness/performance objective | Retain directional readings. Audit whether ACK/data coupling and shared limits make this a different objective from aggregate capacity; an aggregate improvement alone does not establish the desired directional behavior. |
| `Sized` must be true at the final physical-H1 snapshot to prove delivery sizing was enabled | Confuses current cumulative evidence with configured policy | Check resolved constructor/sequence settings separately. Qualified `ServiceSized` evidence and a previously learned window whose samples have expired are also valid states. Retain all original measurements and failures. |
| A cold receiver-service estimator must always return a positive rate | Overconstrains an intermediate result | Allow abstention when fresh qualified cumulative delivery supplies the actual pace. Test the consumer's `PacingByteRate`, evidence ownership and sustained adaptation instead. |

Concrete performance evidence illustrates the distinction. The restored
RTT-growth baseline reaches 91.655/95.771 Mb/s overall, but its first interval
is 83.415/95.764 Mb/s and fails the stricter interval rule. The later candidate
reaches only 5.612/95.771 Mb/s across all three intervals; the bucket-policy
question does not explain away that large loss. The finite shared-relay failure
is 72.684/95.616 Mb/s after a two-second warmup, with no drops, retries or hard
queue violation and substantial rate recovery by the end. That result proves
failure of the current recovery target, not permanent undercapacity or corrupt
byte accounting.

An offline [measurement-window comparison](throughput-fix-2-results/model-rtt-measurement-policy-review-v1)
uses five already recorded RTT-growth pairs, without changing or rerunning a
test. Each has three one-second bins and a configured feedback duration of
1.21 seconds. The restored baseline's minimum reference ratio is 87.10% for
one-second bins, 93.54% for every contiguous two-second mean, and 95.70% over
three seconds. The severe 19.558, 5.612 and 27.238 Mb/s outcomes still fail all
three aggregations. This confirms a policy difference in one borderline run;
it does not select a new acceptance rule. Longer averages can hide transient
stalls, and the existing data cannot reconstruct arbitrary subsecond phases.

For each disputed test, write down its independent inputs, required observable
property, alternative valid outcomes and qualification/convergence assumptions.
Then separate exact measurement mathematics from controller policy, add controls
for the alternatives, and run the old and revised assertions on the same frozen
sources. Preserve the original failure evidence and report any changed acceptance
criterion explicitly. No performance threshold has been changed during this
audit. No quality-event API or production fix is approved merely by reclassifying
a test.

The [detailed sampler audit](throughput-fix-2-results/receiver-test-contract-audit-v1/sampler-audit.md)
preserves all fifteen outcomes of the original sibling experiment and its test
source. Six controls pass and nine exact-response assertions fail; the hard
ownership preconditions pass. The TCP-owner v2 comparison records 20 passes/
31 failures before and 47 passes/four failures after, with race warnings in both.
The remaining after failures expose the two fixture bugs above; they do not
justify another production change. Keep v2 intact and obtain a clean matched
comparison after correcting test construction.

The corrected v3 fixture comparison now has 24 passes/27 failures before and
51 passes after, without race warnings. Production is unchanged from v2.
Eighteen before failures belong to six caller-isolation roots; nine belong to
the proposed ACK-retention/coverage/retry policy. Separate the caller fix from
the optional policy so its acceptance does not depend on treating recoverable
TCP control refusal as a protocol failure.

The [standalone caller-isolation comparison](throughput-fix-2-results/caller-cancellation-isolation-v2)
removes every provider and optional ACK-policy dependency. Eight roots repeated
three times give six passes/18 failures before and 24 passes after, with another
24 adjacent race passes. The two error-path guards and standalone tests have
been applied to the working checkout. A separate [frozen live-source comparison](throughput-fix-2-results/caller-cancellation-live-integration-v1)
repeats six passes/18 failures before, 24 passes after and 24 adjacent passes,
without races. It preserves the live sampler; the isolated proof above used
the experimental receiver sampler.
An unadmitted caller's cancellation cannot retire accepted siblings. Conversely,
actual shared-sequence closure must still recreate a sequence for a live caller.

The physical policy check now records resolved constructor settings and actual
destination sequence configuration. It accepts valid service-qualified and
aged retained-window states, and rejects the wrong mode, missing/disabled
instruments, other destinations and unknown arms. Its [isolated matched proof](throughput-fix-2-results/physical-window-policy-gate-contract-v1)
changes three passes/21 failures to 24 race passes. The adopted smoke check and
all eight caller roots pass together (16 checks), and the policy roots pass
three more repetitions (24 checks) on the same [frozen live binary](throughput-fix-2-results/physical-window-policy-live-integration-v1). The canonical
correctness selector includes these tests. Throughput, refusal and calibration
thresholds are unchanged; these checks do not rerun or certify physical goodput.

The [physical ACK-policy experiment](throughput-fix-2-results/physical-h1-ack-policy-experiment-v1)
preserves both unsuccessful A/B/A runs. Its generic fixture/carrier/lifecycle
label does not identify a teardown failure: all six arms release Transfer,
carrier and replay ownership, retain one connection per direction, and report
no reliable-carrier, TUN or stack drops. The original gates also reject provider
control refusals and `Sized=false` snapshots despite qualified `ServiceSized`
evidence. The observed directional throughput losses remain unresolved; failed
calibration and reference drift prevent claiming a measured gain from the
optional ACK-retention policy.

The [expanded semantic audit](throughput-fix-2-results/service-receiver-semantic-audit-v1/semantic-audit.md)
preserves all original and corrected consumer fixtures. These sustained tests
sharpen the estimator contract. With a rolling
flight and fresh own cumulative evidence, cold service zero permits
3,105,881 B/s pacing from 2,823,529 B/s delivery and passes. A held 1,000,000 B/s
service value instead forces 950,000 B/s pacing through both 192 and 384 cycles.
Cold abstention is valid. The repeated warm hold is a proposed regression target,
subject to reachable-input qualification. This is an estimate-consumer experiment
with offered writes prescribed independently of its output, not a closed-loop
throughput result. Its 64-cycle
opening also exceeds one lane's 512 KiB admission window and represents a
12,000-byte head as one item. The v3 follow-up keeps a 40-cycle opening inside
each actual window and represents the large cumulative head with two legal
6,000-byte messages. It passes all 18 repeated race executions without changing
production. Do not label v2's failure a reachable production bug on this
evidence. The correction also calls the consuming admission estimate before
each offer, whereas v2 consumed once per whole sibling cycle. That is a material
ordering change. Isolate legal message size, opening bounds and consumer-read
cadence before attributing the pass; an ACK publisher can run while the send
worker is blocked. The original split heads remain timing-eligible on code
inspection. Preserve the earlier invalid consumer experiment too: preoffering the
entire flight made queue residence grow beyond its available multi-RTT history,
and the legacy fixture omitted ordinary RTT closure. Those failed qualification
preconditions cannot be used as evidence that the production estimator failed.

The legal single-frame counterpart (1,000/4,000/8,000-byte items and a 40-cycle
opening) passes all 18 repeated controls under both read cadences. The legal
cumulative-group counterpart then isolates a narrower issue: with the same
two 6,000-byte messages per large head, reading before each refill passes three
times; reading once after the sibling batch fails three times. Its service
stays at 1 MB/s and pacing at 950,000 B/s despite the largest lane's fresh
2,823,529 B/s cumulative evidence. Window bounds, physical message sizes and
once-only credit pass in both arms. This is a component-level read-order
regression target; its prescribed offering still does not establish actual
closed-loop throughput or explain a particular model failure.

Also guard against a weak passing criterion: shared pacing must serve the
aggregate workload. The passing legal controls report roughly 3.157 MB/s paced
service against a 4 MB/s serializer. That exceeds each individual lane's rate,
but is only 79% of aggregate capacity. Every message advances the same service
reservation timeline, so per-lane comparisons are necessary controls, not full
utilization proof. Measure the aggregate reservation rate and actual throughput
separately. Audit earlier slowdown roots for the same opening/frame preconditions
before allowing an unreachable stimulus to drive another estimator change.

That lawful slowdown audit now changes nine passes/three failures before to
12 passes after under race instrumentation. Both arms use rolling admission,
legal single-frame or two-message cumulative groups, and the same 192-/384-cycle
convergence policy. The failing batched case retains 13.75 MB/s pacing against
4 MB/s delivery before the bounded decrease; single-frame, frequent-read,
legacy and mixed controls pass both arms. A diagnostic spends the shared probe
and then executes the real reservation/release calculation: the batched result
changes from about 13.75 to 3.831 MB/s, while the frequent-read control stays near
3.158 MB/s. Preserve these as component evidence in the semantic audit, without
attributing a full-model throughput change to them.

The first per-sequence interval candidate leaves the corrected legal warm-rise
selection unchanged at 12 passes/three failures. Its bounded-prefix,
same-sequence causality and tied-clock controls pass; the batched case still
fails. Keep that candidate isolated while diagnosing the acceptance path.
A proposed queued-start cap is excluded from the corrected contract: its
schedule preconfirmed a future write and also permits real fast-path recovery.
The original 12-pass/six-failure proposal is retained as rejected-oracle
evidence; do not introduce a production cap solely to satisfy it.

The cold cumulative fallback has a separate scope defect. With no qualified
serialization rate, each lane passes its own fresh delivery rate into the
same shared reservation timeline and dispatch meter. A corrected legacy
compression fixture spends the real startup allowance, supplies bounded
training flights, and then executes actual pacing waits. Separate equal-two,
equal-four, unequal-two and unequal-four roots each fail three repetitions;
single-lane, idle/stale, independent-service and all twelve original cumulative
roots pass, for 45 passes/12 failures without races. The four cases respectively
release 32,768/49,152, 32,768/98,304, 40,960/98,304 and 40,960/196,608 bytes within
the observed three-flight delivery interval. Frame, window, freshness, target,
probe and cleanup checks pass. Training is prescribed rather than closed-loop,
so this is consumer-accounting evidence, not an actual-path throughput claim.

Correct this using contemporaneous evidence owned by the shared service;
do not multiply by the number of lanes or sum differently aged lane estimates.
Preserve the ordinary single-lane fallback, stale/idle exclusions, once-only
SACK credit, service boundaries, epoch changes and configured rate cap. Review
the new receiver-interval candidate's physical ordering assumptions separately:
a sequence head names one message's ingress timestamp and is not itself proof
that every newly covered byte arrived after the previous head.

The owned-head proposal is not sound solely from local route continuity.
`ForwardSequence.Run` hands each packet to another multi-route writer; a single
first-hop H1 route does not prove destination ingress order. Keep that proposal
isolated. Prefer a bounded shared-credit history that can support a common raw
delivery fallback and, separately, a conservative physical-offer-to-ACK bound.
Every credited byte must retain its own timing eligibility, raw ownership and
epoch provenance. The current serialization ring is not sufficient unchanged:
it retains about one residence and resets at serialization probes, while the
accepted cumulative fallback needs multiple complete flights.

The [full-model endpoint review](throughput-fix-2-results/model-endpoint-pacing-review-v1/review.md)
also prevents a false closure: all 18 directions below their paired reference
gate have positive final service, as does severe RTT growth. A cold-only fix
cannot directly reprice those terminal states. Require a warm recovery proof
and the original affected performance cells after the unified candidate;
these endpoint snapshots do not by themselves supply causal attribution.

The common delivery candidate now has two passing component comparisons:
24 passes/three failures become 27 passes for the warm batched-read proof;
57 passes/15 failures become 72 passes for shared pacing and adjacent policy
controls. Both are race clean. The shared fixture now publishes real queued
send-item credit through cumulative heads, duplicates and late SACKs. Its
first missing-history expectation was wrong: loss of current evidence must
preserve the learned window, not reset it. Preserve that original failure and
the corrected matched comparison. Still require static-path recovery and
actual throughput; prescribed training cannot establish either.

The new [static recovery controls](throughput-fix-2-results/model-static-long-recovery-v1)
now pass at fixed 400 ms/one lane and 1.2 seconds/three lanes using actual
senders, ACK workers and pacing from a 512 KiB opening. Their predeclared long
warmups establish settled capacity. The unchanged short-warmup SDK check still
fails, rising through the measured interval; keep recovery speed as an explicit
core issue instead of replacing that failed comparison with these passes.

## 31. Separate notified path changes from core pacing validation

The user authorized deferring legitimate connection-quality-change tests until
`NetworkQualityChanged` is implemented, then calling that signal in the tests.
The propagation-step performance cases below model explicit changes to the
underlying path. Their rapid remeasurement belongs in that later signal phase.

| Test or condition | Current phase | Required follow-through |
| --- | --- | --- |
| `TestWindowPathAckTailRoundTripGrowthControl` | Deferred notified path change | Call the signal at the programmed 0.3 ms to 100 ms propagation switch; preserve physical write/ACK ownership and the original capacity gate. |
| `TestWindowPathServiceRoundTripChanges` | Deferred notified path change | Call the signal on each programmed propagation increase/decrease; retain both directions and rate cells. |
| `TestWindowPathServiceRoundTripGrowthBeyondOldRing` | Deferred notified path change | Call the signal at the 0.3 ms to 1.2 s switch; keep the complete reference and all recovery intervals. |
| Static long RTT, low initial estimate, SDK profiles/duplex, shared relay | Core | Fix underutilization without an artificial quality event. |
| Capacity-only change, standing queue or remote contention | Core without a signal | These can happen without an app notification; retain sustained upward/downward pacing adaptation. An additional explicitly notified capacity-change variant can be added with the signal. |
| ACK compression, delayed worker, batched estimator reads, receive-window permission changes | Core | These are not evidence of an underlying local network switch and cannot receive a synthetic quality event to hide an estimator bug. |
| FIFO propagation/rate-change fixture tests | Core | They test frame order, timestamps and ownership of the instrument, not estimator remeasurement speed. |
| Credit, cancellation, stale generations, hard bounds and statistics purity | Core | These invariants remain required across all timings. |

Use `tools/throughput-fix-2.sh model-core <fresh-output>` for the current
performance acceptance phase. It retains the historical model selector and
explicitly excludes only the three named propagation-transition tests, writing
`deferred-tests.json` alongside the result. The original `model` mode remains
available with its full selection; no test body or numerical threshold is
changed. The previous full run remains 25 passes/five failures. One of its
failed groups is now deferred; four failed static-path groups remain open.
Do not describe the phase split as a passing full model or erase old outcomes.

When the event is wired, inject it at the actual model path-change boundary,
not after observing a failed estimate. Retain delayed old ACK and pending-write
controls around the event. An unnotified congestion control must still adapt
from sustained feedback, in line with the agreed design.

## 32. A cumulative head cannot time every newly acknowledged message

The head's sequence number proves coverage, not destination ingress order.
Relaying can reorder messages even if the sender used one local H1 route.
If a later head arrives before a hole, the receiver's head-wait metadata starts
before that earlier message arrives. Subtracting that wait from the entire
newly credited prefix invents a shorter serialization interval.

`TestWindowPacingReceiverEarlyIngressHeadCannotRetimePrefix` now reproduces
16 MB/s against an independently serialized 0.2 MB/s path using three legal
H1 frames and two routes. It fails all three race repetitions. The ordered
ingress and prior-SACK controls pass six repetitions. This defect predates the
rejected per-sequence interval proposal and is unrelated to a quality switch.

Research and acceptance steps:

1. Preserve exact once-only bytes and physical-offer eligibility. A nonzero
   head-wait correction may apply when this ACK newly credits only its head;
   it cannot be assigned to other newly credited prefix envelopes. A zero wait
   removes no interval. Raw common delivery must retain its independent facts.
2. Keep the reordered root, ordered and prior-SACK controls. Review mixed,
   unknown, copied and fallback credit, cumulative/SACK orderings, repeated
   heads and late worker publication. Add deterministic roots for any new
   failure at the layer that observes incorrect service or pacing.
3. Audit older cold exact-rate assertions separately. A precise rate after a
   few positive-wait cumulative heads may have depended on this invalid timing
   premise. Preserve failures and explain any contract correction; static-path
   throughput and valid single-head timing still have to work.
4. Run the combined core matrix with common delivery enabled. Its raw arrival
   and offer-to-ACK observations do not borrow destination FIFO and may provide
   policy recovery when exact serialization must abstain. Passing component
   roots does not establish that recovery or acceptable throughput.

The same-local-H1/downstream-reorder supplement changes 12 passes/six failures
to 18 passes under race instrumentation. This rules out local route continuity
as an exception to the prefix restriction. The accepted test-contract proposal
also passes 60 repeated checks while preserving the original failed comparison:

| Original underdetermined assertion | Replacement contract |
| --- | --- |
| `TestWindowPacingSdkColdReceiverWaitMeasuresShortTrain` | `TestWindowPacingSdkColdReceiverWaitKeepsPrefixBounded`: retain exact physical credit and raw RTT; the ambiguous cumulative prefix may abstain from an exact service rate. |
| `TestWindowPacingSdkColdDrainedTrainUsesExactPair` | `TestWindowPacingSdkColdDrainedPrefixKeepsRawClocks`: retain exact drain, byte count and raw timing without assuming the head encloses the prefix. |
| Attributable positive control | `TestWindowPacingSdkColdSingleHeadWaitMeasuresShortSerialization`: two legal frames and one newly credited head retain the exact short serialization proof. |

The low first-refill rates in the original restricted source remain a separate
gap/cycle qualification question. An upper-bound assertion alone does not
establish that those rates are reliable. Keep this review and actual SDK
recovery independent from the corrected cumulative-prefix timing contract.

## 33. Diagnose static startup separately from quality-change recovery

The [warm common-rate pacing experiment](throughput-fix-2-results/window-cumulative-shared-warm-pacing-v2)
does not close the static SDK failure. Both frozen arms contain the corrected
prefix contracts and pending-drain guards and pass 183 repeated race checks.
Both pass SDK Short and fail SDK Long at fixed 100 ms and 400 ms RTT. The only
runtime difference takes the greater of positive service and common delivery
before the existing pacing margin and target bound. It changes neither sizing
nor backlog classification. Preserve all 40 readings and original gates.

Next steps:

1. Trace the actual early limiter: own/common/serialization rates, candidate
   and retained window, hard permission/memory bounds, outstanding bytes,
   reservation debt and both service/common flight bounds. Keep the trace
   observational and record its separate source.
2. Reproduce any false cold first-refill rate with exact physical offers and
   once-only ACK credit. A new offer cohort's first receipt cannot establish
   its serialization rate by charging the whole inter-train feedback gap to
   one small head. Preserve controls for a genuinely slow preoffered train,
   later independent slow/fast evidence, and unknown or mixed offers.
3. If the trace instead establishes a controller-discovery limit, test that
   mechanism independently before changing policy. Do not replace the failed
   short-warmup comparison with the passing long-warmup static controls.
4. Rejoin accepted corrections and run `model-core`, scoped race checks and
   root regression. Static startup and unnotified congestion remain core;
   only the three explicit propagation transitions in section 31 are deferred.

The [ring cost comparison](throughput-fix-2-results/window-cumulative-shared-delivery-cpu-v1)
has no per-operation allocations but adds 3,608 bytes per service and measurable
CPU work. Include that cost in the final decision; component correctness alone
does not establish acceptable overall performance.

## 34. The pacer measures its own limit: discovery and held pace

The [SDK ramp trace](throughput-fix-2-results/sdk-warm-ramp-diagnostic-v1)
and the [live-tree model-core baseline](throughput-fix-2-results/model-core-live-baseline-v1)
localize the remaining static-path losses to one controller rule, not to the
estimator. `windowPacingRate` followed measured service at 1.1 (unqueued) or
0.95 (backlogged). On a path that has never queued, measured service is
whatever the pacer itself released one residence earlier, so it bounds
capacity only from below. The loop then grows by a constant increment per
residence: at 400 ms the traced pace rises 1.43, 1.76, 2.06, 2.35, 2.64,
2.94, 3.23, 3.53 MB/s while the learned window doubles the retained flight it
is never allowed to release. The 7 MB device window needs about 60 residences
to fill; the SDK gate allows about six.

The baseline's four failing groups share this root:

| Condition | Mechanism |
| --- | --- |
| SDK device/provider senders at 100 ms and 400 ms (`TestWindowPathSdkProfiles`, `TestWindowPathSdkConstrainedLongWindow`, the reverse direction of `TestWindowPathSdkBidirectional`) | Pacing grows additively from the one-residence bootstrap rate although the window rule already retains twice the measured flight. |
| `TestWindowPathWindowMismatchChanges`, receive 2 MiB to 64 KiB at 50 ms compression | The peer's permission shrinks; measured service falls with it because the sender is now the limit; pacing follows service down to 0.59 MB/s and trickles the 64 KiB window over 93 ms. The receiver's quiet-head early ACK needs a burst, so every flight waits the full 50 ms compression. The reference bursts the window and turns in about 14 ms. |

The 300 µs cells of the same senders pass on the live tree with a healthy
ramp (the traced pace reaches the target within 250 ms), so the earlier
300 µs failures belonged to an older source, not to this root.

### Rules

Two rules are added to the pace owner. They change no window sizing, no
service qualification, no ACK contract and no hard byte permission.

1. **Discovery floor.** Until the shared service has observed queueing on
   this path, the pace is at least `Window / WindowRoundTrip`, where `Window`
   is the admitted window after retention and permission clamps. The learned
   window already grows only from valid delivery evidence at the configured
   `DeliverySizedWindowScale`; releasing it over one residence lets that
   evidence drive growth instead of the pacer's previous release rate.
   Queueing is observed when an RTT sample exceeds the unloaded minimum plus
   compression plus `max(2 ms, minimum/4)`, the drain check's own margin, so
   a transient opening-probe bump on a fast path does not end discovery; a
   recovery reservation also ends it. Discovery restarts only when a minute of
   silence replaces the path baseline, or when a future
   `NetworkQualityChanged` remeasurement requests it. The transient queue
   during discovery is bounded by the window's growth step,
   `(scale-1)/scale` of the window, because the window never exceeds `scale`
   times the measured flight.
2. **Held pace.** The pace granted by an admitting read is held on the shared
   service. Without congestion evidence the pace does not fall below it. A
   backlogged read, queued reply and recovery reservation release
   the hold, after which the existing 0.95 drain rule may lower the pace. The
   RTT-only release condition is refined by the candidate in section 36. This
   is the agreed policy that pacing adapts downward only when feedback proves
   congestion; a smaller peer permission or a window-limited interval is not
   congestion.

Only sequences whose flight policy is H1-only read or write this state,
because only those sequences consume the pace. Statistics readers never
advance it; only the admitting read records the granted pace, and only once
it has a residence to hold the pace against.

### Deterministic roots and controls

[Before/after evidence](throughput-fix-2-results/pacing-discovery-root-v1)
with exact source hashes under concurrent host load:

| Test | Role | Before | After |
| --- | --- | --- | --- |
| `TestWindowPacingDiscoveryReleasesRetainedWindow` | Pure rate contract: the floor applies only while discovering, survives the flight-based backlog artifact, is capped by the target, and leaves the crawl case unchanged. | 4 of 6 cases fail | pass |
| `TestWindowPacingHeldRateFallsOnlyWithCongestion` | Pure rate contract: the hold survives a lower service reading, yields to a backlogged read, never exceeds the target. | 3 of 5 cases fail | pass |
| `TestWindowPacingServiceObservesQueueOnce` | Service state: RTT inflation and recovery reservations end discovery and release the hold; an unqueued reply keeps it; a minute of silence resets both. | new | pass |
| `TestWindowPathDiscoveryFillsRetainedWindow` | Closed loop, device sender to server, unloaded 1 Gb/s path, eight residences: the window reaches its 7,010,478-byte permission and is released each residence. | 68.3 Mb/s at 100 ms, window 1.88 MB | 522.2 Mb/s at 100 ms, 131.2 Mb/s at 400 ms, no drops |
| `TestWindowPathDiscoveryStopsAtCapacity` | Control: 4 MB/s path, discovery ends, pace within 0.9 to 1.15 of capacity. | pass | pass, discovery ended |
| `TestWindowPathPermissionShrinkKeepsPace` | The 2 MiB to 64 KiB, 50 ms mismatch cell alone. | 3.523 versus 4.676 Mb/s | 4.669 versus 4.598 Mb/s |
| `TestWindowPathCapacityDropStillLowersPace` | Unnotified congestion control: 12.5 MB/s falls to 2 MB/s and the pace follows within fifteen residences. | pass | pass |

The archived static long-path controls
(`TestWindowPathServiceStaticLongRoundTrip*`) moved into the main inventory
and pass. The model selection now also runs the retained SDK cells, the
discovery roots, the permission-shrink cell and the capacity-drop control.

### Contract corrections

Eight `TestWindowPacingCumulative*` contracts pinned the superseded rule that
retained bytes never price pacing. Their fixture's replies show a clean 10 ms
round trip, so the service never observes a queue and the new contract
releases the 1 MiB admitted window over that residence (104,857,600 B/s).
They were re-pinned to that floor, and the slow-delivery and slow-service
cases were split: an unqueued variant keeps the pace, and a queued variant,
with the queued reply a slower path necessarily produces, lowers the pace to
the original pins. Retained bytes still infer no serialization rate: cleared,
stale and permission-stepped histories report zero delivery and service
rates. The exact old and new values are in the root archive's README.

The three explicit propagation-transition tests remain deferred (section 31).

## 35. Adopt exact-head receiver timing and retest short duplex

The live checkout still applied a newly credited cumulative head's positive
receiver wait to every eligible envelope released by that head. A relay can
deliver the head before an earlier sequence hole, including when all original
writes used one known local H1 route. Removing the head's wait from those later
prefix bytes invents serialization capacity. Two deterministic ingress tests
reproduce 16 MB/s from independently serialized 0.2 MB/s inputs.

The adopted correction in `publishAckServiceCreditWithTiming` records the
head's own byte count while holding the retry-queue lock. Positive receiver
wait is applied only when all newly credited bytes belong to that head.
A zero receiver wait preserves the raw arrival clock for a cumulative prefix.
Already selectively credited heads cannot lend timing to a later prefix;
already credited prefix bytes do not prevent a new exact head from using its
own timing. Physical and logical byte ownership and raw RTT remain unchanged.

The permanent roots in `transfer_window_receiver_prefix_ingress_test.go`
cover early-head reorder across two routes, downstream reorder behind one
known local route, SACK-before-head and late-SACK orderings, ordinary ordered
delivery, zero-wait raw timing, and a new head following an already SACKed
prefix. The zero-wait control requires the exact 200,000 B/s raw-clock rate.
Together with the eight SDK receiver-interval contracts, five cold-refill
contracts and the existing cold queued-timing control, all 20 definitions pass three race-instrumented repetitions on
the corrected live source. A separate 40-definition race selection preserves
one head, bounded oldest-first SACKs, SACK pacing, encoded message limits,
metadata presence, physical confirmation, retries and malformed-tuple checks.

Two new cold-refill tests required an unloaded sender's pacing rate to equal
its 267,000 B/s observed delivery. That upper bound contradicted section 34's
accepted discovery policy: observed delivery can be limited by the sender's
previous releases. Their corrected contracts still require exact 267,000 B/s
service evidence and adequate pacing, while permitting faster discovery.
The unsupported-gap, genuinely slow preoffered train and sustained-slowdown
contracts retain their original assertions. No SDK throughput threshold or
measurement duration changes with this correction.

`TestWindowPathSdkRetainedDeviceDuplexShort` isolates both retained
`sdk-device-h1` provider roles at 300 microseconds RTT and one bidirectional
flow. A matched comparison changes only the prefix-timing production rule;
the original SDK warmup, per-direction 90% reference gate, physical link and
queue bounds remain in force. Exploratory repetitions give:

| Device role | Prefix timing before: upload Mb/s | Exact-head timing: upload Mb/s | Reference Mb/s |
| --- | --- | --- | --- |
| Not providing | 810.639–814.152; 3 failures | 886.344–918.241; 3 passes | about 957.2 |
| Providing | 837.734–851.077; 3 failures | 864.901–931.840; 3 passes | about 957.2 |

Both directions remain serialized, and these runs report no relay drops.
The [pinned source/build comparison](throughput-fix-2-results/service-receiver-prefix-live-sdk-v1)
records 45 passes/15 failures before and 60 passes after for the race roots.
Its SDK repetitions improve from three failing definitions to two passes and
one failure. Individual provider-role comparisons improve from four failures
out of six to one: the non-providing role reaches 855.296 Mb/s versus
957.2352 Mb/s reference in the third after repetition, below the unchanged
90% gate. Preserve this failure and diagnose its actual ordering before
changing pacing or test expectations. The exploratory passes do not replace
the repeated pinned result.

The post-prefix `model-core` run passes 35 definitions with 842 ledger readings
and no failures. The same production checkpoint passes combined correctness
and root regression; regression records 25 expected skips. These runs precede
the pacing candidate in section 36 and do not validate that later edit.
The three explicit propagation
transitions remain deferred as specified in section 31; their future
`NetworkQualityChanged` calls are unchanged.

## 36. Keep held pacing through one permitted reverse burst

The remaining pinned duplex failure ended with 101,108,827 B/s measured
service and 111,219,709 B/s held pacing on a 125,000,000 B/s serializer.
Forward flight was not backlogged. A reverse-direction data burst can delay
an ACK without proving that forward service has fallen. The prior controller
ended discovery and erased the held pace on the same RTT increase, even when
the extra residence fitted inside its existing permitted burst duration.
Once erased, the lower measurement could become its own sending limit.

The current candidate separates these decisions. Discovery still ends at
`max(2 ms, minimum RTT / 4)` beyond unloaded RTT and receiver compression.
An RTT sample alone clears the held pace only when that excess also exceeds
the current maximum burst duration, `2 × estimate interval`. This reuses the
existing burst limit; it adds no setting, bytes or probing allowance.
Physical forward backlog and recovery reservations still allow an immediate
pacing decrease, and a longer observed queue clears the hold even if all
flight has already drained. Both RTT publication and ACK-credit publication
use the same service-owned decision.

Three explicit-clock contracts in
`transfer_window_pacing_burst_queue_test.go` cover the root and its adjacent
congestion boundaries:

| Test | Required behavior |
| --- | --- |
| `TestWindowPacingOnePermittedBurstKeepsHeldRate` | A 1.25 MB reverse burst takes 10 ms on the independently specified 125 MB/s link. Inside the existing 20 ms allowance, it ends discovery but preserves a 137,500,000 B/s hold with no forward backlog. Before the change it deterministically falls to 111,219,709 B/s. |
| `TestWindowPacingQueueBeyondBurstReleasesHeldRate` | A 25 ms excess clears the held pace even after physical flight drains. |
| `TestWindowPacingPermittedBurstDoesNotMaskForwardBacklog` | Three MB outstanding exceeds 1.2875 MB residence plus 1.25 MB burst allowance. Admission must use the 0.95 service drain rate and replace the old hold, despite a return delay within the permitted burst. |

Five repeated candidate runs pass both SDK roles, capacity discovery, the
unnotified capacity drop, long compressed feedback and repeated-drain controls.
The [immutable matched comparison](throughput-fix-2-results/service-pacing-burst-hold-v1)
includes all three new roots plus existing held-rate and discovery controls:
12 race passes/three failures before become 15 passes after. The SDK test
passes all five repetitions in both arms, so this pair proves the component
correction but does not establish throughput uplift or deterministically
explain the entire earlier intermittent SDK failure. Preserve that distinction.

The candidate's full `model-core` run passes 36 definitions and 846 readings.
Its isolated SDK cell reaches 918.65088/957.21472 Mb/s (95.97%) when not
providing and 882.86208/957.19424 Mb/s (92.24%) when providing. Both pass the
unchanged gate with no relay drops. Combined correctness passes 512 race
definitions with no failures or skips, including all three new roots. The
model snapshot preceded the third test addition; its production pacing file
is identical. [Full root regression](throughput-fix-2-results/core-regression-burst-hold-final-v1)
passes 3,345 top-level tests with 25 existing skips and no failures on the
candidate source that includes all three roots.

All runs use current host load without a quiescence wait. The three
quality-event deferrals in section 31 remain unchanged.

## 37. Run scoped server regressions with their real configuration

The current `server/connect/test.sh` sources `server/test-env.sh`. That script
both configures environment exports and attests/probes launcher-managed
PostgreSQL and Redis. Applying the full preflight to the runner's synthetic
ownership/lifecycle selections stopped them before compilation, on an
unreadable launcher owner, despite their having no service dependency.

The runner's two scoped modes now reuse the exact `test_env_configure`
function with its original source path. An audit file pins the environment
source, sourced launcher helper, selected test files and exact test names:
35 connect tests and 28 proxy tests. An environment/function/source change or
wider test selection requires another review. The recorded provenance states
that service preflight did not run. Every integration mode still sources the
original complete preflight, and the two previously excluded database-backed
connect tests remain excluded. No server source, credentials or host service
configuration changes.

Five deterministic synthetic runner tests cover a failing service preflight
with successful configuration, correct exports and quoted source paths,
environment/test/helper drift, changed function boundaries, wider/integration
patterns and an unreviewed nonrace mode. All five pass.

The first scoped build then hit the local unaccepted Xcode license. The server
production Dockerfile already builds with `CGO_ENABLED=0`, and these owned
fixtures use no cgo/native resolver API. The two scoped modes default to that
setting while preserving an explicit caller override. The current Go 1.26.7
Darwin/arm64 toolchain also builds connect with `-race=true` and cgo disabled;
its binary metadata confirms both and a diagnostic run passes all 35 tests.
Connect's race flag remains intact; proxy retains its documented nonrace flag.
The final runner records actual binary build settings and environment values.
The [final scoped runs](throughput-fix-2-results/server-scoped-configure-final-v1)
pass all 35 connect race tests and all 28 proxy nonrace tests, without failures
or skips. Both binaries confirm `CGO_ENABLED=0`; connect also confirms
`-race=true`. The sibling server checkout is unchanged. The user-deferred
full server integration tiers remain outside these results.

The separate runner regression exposed an incomplete synthetic fixture: it
did not copy the ledger parser now required by the runner's hash guard. The
repaired fixture copies that dependency and then deliberately changes the
original parser during its Go test. Successful post-run parsing proves that
the frozen parser remains the owner. All five snapshot tests and all five
server-environment adapter tests pass; their evidence is retained with the
final core regression.

## 38. Bound quality remeasurement and reject old-generation evidence

The core quality-event implementation now permits the previously planned
exception to grow-only window sizing. A notification starts a five-second
remeasurement interval; only a qualified fresh-generation service estimate
and RTT may replace the retained byte window during admission. The previous
window and estimates remain provisional until that evidence arrives. At the
fixed expiry, normal grow-only sizing resumes. Sustained congestion/capacity
feedback continues to adapt pacing without a signal, and statistics reads
remain non-mutating.

The physical service owns the generation for all its sibling sequences.
Notifications separated by at most five seconds are coalesced, including
notifications to a newly joined sibling. They do not extend the accepted
generation's shrink interval. A further generation requires five seconds of
quiet. A sizing calculation paused before commit must recheck the shared
generation before publishing learned bytes or held pacing; otherwise a sibling
reset could immediately be overwritten by old evidence.

Use the immutable physical first-write timestamp to classify ACKs, not arrival
time, delayed worker execution or the echoed wall clock. Old and mixed-prefix
ACKs must still retire messages and repay physical credit while contributing
no new-generation measurement. Reset preserves delivery ownership, pending
reservations, FIFO order and all physical byte/burst limits. Pending receiver
RTT metadata, legacy RTT tags and delayed service-credit publication each have
an explicit root test. No ACK encoding or compression allowance changes.

### Adjacent recovery and drain roots

The changed-path review found two interacting failures. First, an old short
recovery floor retried new-path writes before a long-path RTT could arrive.
Pending remeasurement now uses the existing configured cold recovery floor,
within the configured maximum, until fresh RTT qualifies. The physical
override is restricted to the first reliable write in the new generation;
the deadline stays anchored to that write and excludes old, copied, unknown,
unreliable and changed-carrier cases.

Second, eliminating the spurious retries exposed an unnecessary full-flight
drain during genuine queue recovery. The queue was already falling, but the
probe stopped writes and left the 1.2-second link idle a propagation turn
later. The deterministic root reproduces a 3.4-second stop while 26 MB of
flight is still declining. Compare complete feedback turns, at least 40 ms,
and grant one further turn only when physical flight falls by more than one
permitted burst. A stalled/growing queue or burst-sized variation cannot keep
the grace alive. Existing drain deadlines, cooldowns and proof remain intact.

### Source-specific validation

All 16 `TestWindowQuality*` roots plus the two natural-drain roots pass
`-race -count=3` (1.814 seconds). They cover finite shrink permission, quiet
coalescing, peer scope, sibling inheritance, old/mixed credit, delayed worker
and metadata ordering, wall-clock changes, ownership preservation, recovery
exclusions, and an explicitly paused sizing commit across a sibling reset.
The full names and command are in the
[core report](THROUGHPUT-REPORT-PR2.md#network-quality-estimator-bounded-remeasurement-and-fresh-evidence).

The three changed-path models from section 31 now signal at their programmed
switch, while static paths and unnotified congestion controls remain unchanged.
The initial cold-recovery checkpoint passed two of three: the 1.2-second growth
case delivered 86.207147 versus 95.771307 Mb/s, with its first interval at
67.072 versus 95.764480 Mb/s and zero timeout copies. After the natural-drain
correction all three passed; the long case reached 95.771307 versus 95.778133
Mb/s, minimum flow 11.898880 Mb/s, with zero measurement drops and unchanged
interval gates. The report records both complete local log paths and hashes.
That model snapshot predates the last two sibling-generation guards; the
18-root race run includes them. Keep final combined-source model/correctness/
regression results distinct from these checkpoint results. No source-specific
pass permits weakening a throughput or physical bound.

The three earlier quality-event deferrals describe the pre-event phase and
are superseded by these signaled model tests. Public API, peer propagation,
platform call sites and their lifecycle review are a separate part of the
same implementation and require their own validation record.

## 39. Propagate quality changes through clients, providers and native hosts

The public signal is deliberately smaller than hard network recovery.
`NetworkQualityChanged` enqueues estimator work; it does not tear down a
working transport. `NetworkChanged` calls the same quality invalidation once
and then performs its existing hard recovery. `DeviceLocal` exposes both
operations so host code can select the correct boundary.

Callback frequency is not a new measurement clock. The client state admits a
single generation until five seconds of listener quiet, and its one worker
serializes the reset. The shared service and every sequence independently
recognize that generation, so duplicate local fanout remains harmless. Remote
instance/generation ordering removes duplicate or stale provider messages
before estimator work is queued. A deterministic root holds a public window
statistics read across the reset while 128 notifications arrive concurrently;
it requires one generation, 32 valid snapshots under `-race`, and one new
generation only after the quiet boundary.

Each `Client` registers one process callback and one internal wire callback.
A local event resets that client's estimator generations and queues a reliable,
ACKed 24-byte message for every known destination. Peer discovery includes
ordinary send sequences and authenticated receive-only peers, so a provider
can notify the clients currently using it even if it has not created a return
sequence yet. One client-owned worker performs estimator and send work outside
OS and receive callbacks. Zero-timeout refusal retains the newest generation
for retry instead of blocking the shared callback.

The receiver validates a reserved-subprotocol payload containing the sender's
instance id and generation. It deduplicates monotonically within an instance,
accepts a newer instance after restart, rejects older instances, and applies
the event only to sequences for the authenticated source. It never echoes a
received hint. All remembered-peer, pending-send and remote-generation maps
have the same 1,024-entry bound and deterministic oldest-entry eviction.
Closing a client unregisters both callbacks, cancels the worker and joins it.

The internal registration found an adjacent discovery bug: reserved protocol
ids were included in the public subprotocol query. The registry now has an
explicit application-id view, and both outgoing answers and incoming results
filter ids below `SubprotocolReservedLimit`. Tests cover internal dispatch,
public discovery and a peer returning reserved ids.

Native hosts classify the available signals before calling the SDK:

- Android uses stable Wi-Fi bars, power-of-two link-rate bands, cellular bars
  and displayed cellular type. Separate baselines prevent the first telephony
  callback from acting like a change, and stale capabilities cannot change the
  current-network cellular gate.
- Apple uses active cellular radio type, path properties and Wi-Fi bars. Since
  the public Wi-Fi API is a snapshot, the extension samples it every five
  seconds. The first successful value is a baseline. Apple exposes no public
  cellular-bar API to this extension.
- Windows consumes native WLAN MSM signal-quality notifications, converts them
  to five bars and hands them to the existing watchdog thread. The first value
  is a baseline and teardown clears the callback before the monitor can outlive
  its controller.
- Linux polls the physical default path once per existing reaper tick. An
  interface/carrier transition uses hard recovery; a Wi-Fi bar or link-speed
  band transition uses quality remeasurement. The tunnel interface is excluded.

Platform signals are hints. Sustained qualified feedback must still adapt when
the remote path changes without an OS notification, and a hint cannot qualify
compressed, partial or old-generation ACK evidence. The detailed roots,
platform validation and native-CI limits are in the
[report](THROUGHPUT-REPORT-PR2.md#public-propagation-and-platform-notification-hooks).

## 40. Complete configured server integration

The local PostgreSQL and Redis services became available, so the earlier
environment deferral is closed. Using the checked-in Bash environment owner,
all legitimate `server/connect` directories pass: connect in 3,772.499 seconds,
perfvar in 1,402.138 seconds, sim-latency in 35.928 seconds, the three-test
resource fixture under `-race`, and the immutable baseline verifier. The first
package-local connect script returned 1 only because its broad `find` entered
an immutable baseline fixture that the official top-level directory selector
excludes. Server commit `21acdcb5` now uses that canonical selector, preserves
caller arguments and stops on selector failure. Its three deterministic roots
pass 21/21 race executions, and the final official no-test traversal selects
exactly the four legitimate packages without entering baseline or evaluator
artifacts. The full product payload was already green against the same source
and was not repeated at that checkpoint. The later final-source campaign below
repeats it.

The full proxy product package passes its configured integration in 316.690
seconds. Its first wrapper run then reproduced an independent server regression:
INT or TERM could terminate the foreground logger before the acceptance runner
finished cleanup. The retained deterministic signal tests failed on the
checked-in wrapper. Server branch `throughput-fix-2`, commit `7e19ae5d`, restores
owned logging, signal forwarding and joined cleanup, and adds adjacent direct
signal, repeated signal, normal-exit, runner-failure and logger-failure roots.
All nine roots pass three race repetitions. No sibling wrapper contains the
same foreground-tee pattern.

At that checkpoint, the official proxy script passed end to end: the product
package in 316.690 seconds and acceptance in 5.958 seconds. Its log SHA-256 is
`3409fa1f46440b3e9eff31d935bb6baf8fcb5e7e3e0f85b2932dd11ade3ce31a`.
The exact paths, connect hashes and host loads are recorded in
[the final report](THROUGHPUT-REPORT-PR2.md#final-configured-server-integration).
Both server fixes are rebased onto clean server main revision `3a3cc698`; the
server branch was then advanced by the test-fixture correction below.

The full connect tier was repeated after final connect commit `f8261152`.
All four official packages pass in 4,055.767, 1,437.994, 33.015 and 0.246
seconds; the combined log SHA-256 is
`b01672b716faf2039e3cbe55c4a22f93133b8d05df98c8a006203702ca479c24`.

The frozen proxy build first exposed a repository-pair mismatch: server
`23135c01` references six memory-telemetry fields present only in four
pre-existing uncommitted SDK files. A clean detached SDK snapshot pins exactly
that patch as commit `17a7a332`; its patch SHA-256 is
`d2adcf17943a1338faaa1b65b233cd0e82b43510724e941017e127ddac9db48d`.
Nine SDK and 12 proxy telemetry race executions pass.

The first full proxy execution on that coherent pair returned zero but is not
accepted: its log contains a recovered nil-pointer panic from an asynchronous
worker leaked by `TestProxyDeviceMemoryBudgetReleasedOnDeviceClose`. The
original fixture omitted its SDK device, TUN, initial activity and
manager-owned context. The test falsely passed three times while producing
three panics. New direct-run and activity roots fail 6/6 on that shape, and a
wrong-parent control fails 3/3.

Server commit `27b7dad9` replaces only that test fixture with initialized,
lifecycle-realistic owners. The focused 22-test selection passes 220/220 under
the race detector. The official proxy script then passes the product package
in 334.528 seconds and acceptance in 6.132 seconds. The final log SHA-256 is
`38395dac8dca9013c13480e7f5304a4ad6d393daa50acff0c62071472a29d73f`,
with zero recovered panic, unexpected-error, nil-pointer, fatal, warning or
race matches. Exact snapshots, loads and rejected evidence are recorded in
[the final report](THROUGHPUT-REPORT-PR2.md#final-configured-server-integration).

## 41. Close shared cold pacing and provider pure-ACK admission

The final adjacent review found two independent losses after the retained
window and quality-generation work.

### One physical service needs one cold pacing rate

Lane-local cumulative delivery is valid window-sizing evidence, but it cannot
independently price several logical lanes that consume one physical H1 pacing
clock. After `NetworkQualityChanged`, two or four lanes could each measure a
healthy local rate while their shared pacer saw only one lane's rate. The
deterministic consumer released 27,648 of 49,152 requested bytes by its
30.03 ms deadline. Equal and unequal lane shares reproduced the same scope
mismatch; a single lane and independent destinations were controls.

The correction keeps a small service-owned history of exact, once-only H1
delivery credit. It records original offer and ACK-arrival endpoints across
logical lanes and serialization drains. A qualified interval spans at least
the greater of two path residences and four sampling cadences. The first
arrival group's bytes remain outside the numerator. Rebucketting preserves
the raw endpoints, permission and quality-generation boundaries reject old
offers, and idle expiry remains separate from endpoint retention. Retention is
anchored to the newest non-future arrival, so a complete interval remains
valid at its exact freshness boundary and expires immediately afterward.

This common rate is a cold pacing fallback only when no positive physical
serialization rate exists. A measured serializer remains authoritative.
Logical delivery, learned window size, peer permission and memory accounting
remain per sequence. A quality event clears the common measurement history
without changing reservations, delivered-byte ownership or the already spent
opening probe.

Six fixed entries are sufficient for four measurement buckets plus both
endpoint phases. The structure is 360 bytes and adds 3,248 bytes less per
service than the reviewed 64-entry counterfactual. Both versions allocate
zero bytes per operation. Under the recorded concurrent host load, the
six-entry medians were 375.5 ns for publication, 450.0 ns for estimation and
98.21 ns for rebucketing. The 64-entry overlay was 3.1%, 33.5% and 484.4%
slower respectively.

Permanent roots cover one, two and four lanes; equal and unequal shares; a new
idle sibling; independent services; read-only statistics; permission,
carrier and quality-generation boundaries; tied and reordered arrivals;
cadence changes; worst endpoint phase; future/stale evidence; unknown offers;
and the exact retention/freshness edges. The focused shared-delivery and
quality selection passes 294/294 race executions.

### A generated pure ACK needs its per-flow owner through admission

The provider's exact 52-byte TCP pure ACK was generated on a dedicated flow
worker, then classified like a shared regenerable control. When the pinned H1
provider lane's two admission slots were full, the shared zero-wait path
refused it. Releasing a slot could not recover the already discarded ACK, and
the peer could remain stalled until it retransmitted. The failure reproduced
under both constant and delivery-sized windows while the available-slot and
public-callback controls passed.

The ACK worker now has an explicit dedicated-control recovery mode. It may
wait for bounded Transfer admission on that one flow's goroutine and is
cancelled and joined with the provider. Once admitted, the control retains the
ordinary regenerable ACK lifetime; only consumed socket data keeps the longer
post-timeout replay lease. Shared public callbacks, resets, unreachables and
other synthesized controls still receive immediate refusal when their bounded
workers are full.

If more TCP progress arrives during the wait, cumulative semantics remain the
contract: the worker may deliver the retained head and then the newer head, or
a future coalescer may replace it, but the newest cumulative byte must arrive
once without retransmission. The deterministic roots also prove bounded
cancellation, pool/admission reclamation, both window policies and unchanged
zero-wait public behavior. The focused five-test group passes 50/50, and its
adjacent provider recovery group passes 18/18 under the race detector.

### Repeated quality callbacks are idempotent

Platform listeners may report the same cell bar, cell type or Wi-Fi bar from
many threads. One client worker serializes estimator resets. Notifications
inside the five-second listener interval update the quiet boundary but do not
open another generation; the shared physical service also accepts a generation
only once. A local loopback destination participates in the local reset but is
excluded from peer wire fanout.

The permanent storm root runs 128 concurrent callbacks while 32 public
statistics readers overlap the reset. It requires one applied generation,
valid snapshots throughout, and exactly one later generation after the quiet
boundary. Ten race repetitions pass with no panic, recovered error, nil
dereference or race diagnostic. This bounds work and history replacement even
when a native listener is noisy; sustained ordinary feedback still adapts
without any notification.

The final immutable `f8261152` campaign passes 566 race-enabled correctness
tests, all 858 model rows, all 12 broad-regression rows, 43 allocation-free
pacing benchmark rows and the calibrated physical H1 comparison. Every
manifest records `dirty=false` and source digest
`a22399024dd9b3547d0e82a8a6e217a2718ce5171f2bd82a8762dc9681bec850`.

[pr213]: https://github.com/urnetwork/connect/pull/213
[rig]: https://github.com/Ryanmello07/connect/blob/b54f9f72bec116c0986e6c51ed13cc2f01805bee/THROUGHPUT-RIG-REVIEW.md
