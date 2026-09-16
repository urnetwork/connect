# Window rule on real paths: peer review and research plan

Status: peer review and research plan, updated 2026-09-15. Implementation and
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
test credential at `10.213.0.1:5432` before creating its disposable database.
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
configured server/connect and server/proxy integration tiers remain deferred
until the local launcher and test resources agree on the PostgreSQL credential.

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
same source. Database-backed server integrations remain deferred as requested.

## 17. Preserve service evidence across a deliberate drain

The broad burst-ring checkpoint exposed a separate deterministic failure at
100 Mb/s, 400 ms RTT, eight flows and 50 ms compression: 66.847 Mb/s against
95.826 Mb/s. Replaying its exact warmup and one-second measurement reproduced
the loss. A controlled drain coalesced a large final ACK; when its preceding
checkpoint expired, the next small probe over a 400 ms idle gap appeared to
establish a service near 6.7 kB/s. That false measurement reserved another long
wait which later ACK traffic could not undo.

The correction records a new ACK-time service epoch only after a drained probe
is confirmed. It holds the established pre-probe service until fresh checkpoints
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

[pr213]: https://github.com/urnetwork/connect/pull/213
[rig]: https://github.com/Ryanmello07/connect/blob/b54f9f72bec116c0986e6c51ed13cc2f01805bee/THROUGHPUT-RIG-REVIEW.md
