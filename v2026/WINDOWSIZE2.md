# WINDOWSIZE2: remaining variation after the receive-backing checkpoint

2026-11-11. Peer-review request; no new causal claim or baseline promotion.

The aggregate passed both synthetic Fast5 cases. The remaining question is why
the 32 MiB sequence varies from **268.5, 308.8, 141.9, 236.8 to 220.4 Mbps**,
especially the third window, about 54% below the second. The original mostly
warm small-request probes passed; later review found cold bulk connections that
did not finish connecting within their measurement window. We want a deterministic reproduction of any avoidable protocol or
scheduling limit, followed by a measured fix. Increasing a deadline is not the
proposed remedy.

## Source and qualification boundary

These are full-TUN/H1 synthetic measurements, not a new Android Fast.com result.
The consumer used the SDK 32 MiB profile and the provider the desktop128 scenario.
Five consecutive measurement windows ran in one process, approximately ten
seconds each, with the original small-GET, payload and ownership gates.

The measured aggregate is
`95b8d9a744a9303e7b4983830cd1ff2a628a4b00fe337508b93b08b090551a7f`.
Its clean implementation was checkpointed before merging upstream:

| Repository | Pre-merge checkpoint |
| --- | --- |
| Connect | `99c9d2a4e8ea29c23099a310c23a73c9e3e82b30` |
| SDK | `bf8faf4e83f0235737090196cbaae984f9a132a5` |
| Server | `bec1c69e43fa89db960e766afd44ea6d5b8a5416` |
| Tests fixture archive | `2b3e00af07db17b4b270508bb64407f47ef06aba` |

The exact 157 instrumented fixture donors, positive-control disposition and
base-hash-checked overlay generator are in
`tests/testdata/window-size-checkpoint-95b8`. Diagnostic hooks are excluded from
the clean shipping projection. Later upstream merges have **not** been qualified
by these measurements. Upstream now requires Go 1.27.4; this host and the measured
run used Go 1.27.3. The archive deliberately refuses mismatched base files.

The host was busy, as authorized. There is no per-window CPU or scheduler trace,
and this is one process, not independent paired statistical evidence.

## Measurements

The TCP counters below are exported TCP-layer counters. Provider timeout resends
are Transfer-layer counts; they are separate quantities. Link queue drops are
aggregated across simulated links/directions and are not yet attributed to a
specific TCP flow or ACK.

| Window | Mbps | Max completed GET, s | Transfer timeout resends | TCP retransmits | TCP timeouts | Link queue drops | Sampled RX peak, MiB |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 1 | 268.527 | 0.443227 | 118 | 1,050 | 0 | 314 | 6.69 |
| 2 | 308.772 | 0.283313 | 8 | 1,079 | 0 | 0 | 2.17 |
| 3 | 141.930 | 0.650740 | 14 | 100 | 2 | 25 | 0.27 |
| 4 | 236.777 | 0.380191 | 12 | 523 | 0 | 0 | 1.32 |
| 5 | 220.430 | 0.255830 | 23 | 318 | 4 | 0 | 0.99 |

All five windows passed their original gates, which did not cover every cold
connection. Mean reported goodput was about
235.3 Mbps; the maximum completed GET was 0.651 seconds against the unchanged
five-second limit. The RX values are sampled managed peaks, **not exact lifetime
high-water marks**. Budget vector reads need not be simultaneous.

The 64 MiB sequence also passed: **214.265 / 258.764 / 197.916 / 266.504 /
275.106 Mbps**. Its maximum completed GET was 2.532 seconds; ordinary/final RX
funding refusals were zero. A single 32-versus-64 sequence is insufficient to
attribute a speed difference to memory sizing.

Other results from the same closed diagnostic:

- Fifteen perfvar normal controls passed. Prior closed Connect107/SDK28 normal
  passes are separate evidence. Full150 race and full RUN-ALL remain pending.
- The 32 MiB browsing case passed:100 HTML loads,400 assets,99 overlaps;
  maximum HTML first byte3.154785375 seconds, on page1.
- The 64 MiB browsing case completed the workload but failed page1:
  HTML first byte6.118869458 seconds, controller6.118876792 seconds, over5 seconds.
  This is an open failure, separate from the bounded Fast5 variation.
- Physical Android MEMSTEADY for the32 MiB iOS surrogate and64 MiB Android profile
  remains unqualified. APK preparation failed during read-only ADB observation
  before producing an APK pair. Android does not establish physical iOS footprint.

## What changed in window 3

The second and third windows both launched eight bulk flows. Their provider
mask stayed66. Bulk origin affinity stayed the same from window2 through5:
origin0 used provider row1, while origins1–3 used provider row6. Window2 had
two connected bulk flows on row1 and five on row6, plus one that never finished
connecting; windows3–5 had two and six. The old3/5 versus2/6 dial comparison
mixed probe dials, connection reuse and completed bulk dials; it did not show
a placement change. This was a fixed synthetic provider population.

| Observation | Window 2 | Window 3 |
| --- | ---: | ---: |
| Provider initial writes | 42,276 | 20,354 |
| Provider initial message bytes | 410,053,012 | 193,201,214 |
| Exported native offers | 43,251 | 22,899 |
| Reused bulk connections | 1 | 3 |
| Maximum pending zero-byte streams | 1 | 4 |
| Flows with zero bytes at termination | 1 | 0 |
| Provider selective gap writes | 0 | 0 |
| Provider bounded-reliable-admission wait (mode disabled) | 0 /0 | 0 /0 |

Window3's per-flow delivered bytes were approximately77.1/14.4/7.8/10.8/50.9/
5.8/4.8/5.9 MB. Window2 had116.3/42.3/36.5/37.3/88.7/32.1/32.9/0 MB.
Two flows dominated window3 while several others made much less progress.
The pending-zero-byte maximum also rises in other windows, so it is not by
itself an explanation. Recorded stops were measurement-window cancellation.
In particular, window2's zero-byte flow never completed its TCP connect in
the available8.5seconds. That unfinished phase must be reported separately from
completed-request latency. The disabled reliable-admission counters above do
not measure time blocked by the delivery window.

Configured link rates remained2 Gbit/s, burst64 KiB, queue2 MiB/4096 packets.
Access delay was20 ms each direction; provider delay was zero. Application MTU
was1280 and outer MTU1500. Injected random loss, jitter, reorder and duplication
were disabled. **Queue drops were allowed and occurred.** A clean configured
loss model therefore does not mean the observed run was lossless.

The lower initial-write rate and sampled RX use are compatible with reduced
upstream service/progress. They do not identify its cause. Twenty-five queue
drops and two TCP timeouts could matter, but the aggregate cannot establish
their ordering or the affected link. The raw indexed TCP rows do establish
that both window3 timeouts, and all four in window5, belong to the app stack;
they are not outer-carrier timeouts. SYN retries are not counted by those
timeout counters.

## Receive mechanisms already established

Focused tests previously demonstrated two distinct constraints:

1. Encoded credit needed enough retained receive backing. For logical quota
   `L`, the candidate uses `ceil(7*L/2)` physical allowance and a2/7 conversion
   when advertising the attached total, preserving `L` and the old Head/Pack
   quotas. At32 MiB:physical RX7,633,633 bytes, encoded credit2,181,038 bytes,
   shared root21,810,380 bytes. Those are overlapping allowances, not eager
   allocations or a complete Go-runtime footprint bound. The advertisement is
   a numerical encoded ceiling per sequence, not a guarantee of retained
   backing shared across all providers. Correlated receiver stalls and other
   legal grouping shapes still need qualification.
2. Independent queue-count pressure caused46 refusals/retries at count85 with
   approximately5 MB of RX capacity still available. Matching the measured
   eight-packet-group flight to count256 eliminated loss/retries in that focused
   sustained case. It did not prove every legal grouping shape is covered.

The aggregate also retains exact advancing TCP feedback when an initial native
offer is refused and bounds fresh DATA refill while an existing recovery cohort
retires. These have separate deterministic mechanism tests. A proposed broader
resend adjustment was rejected because it duplicated healthy traffic.

The32MiB process accumulated642 ordinary RX refusals and626 final-funding
refusals. The raw receive cuts contain more evidence than the original matrix
used: summing the observed clients gives handoff drops0→804 in window1, then
804 at every later cut. Matched endpoint counters do not increase in windows2–5;
all observed saturation and growth counters are zero. Head reserved bytes rise
by488,704 in window1 and remain flat. These observations concentrate recorded
pressure in window1 and do not support count saturation as the measured dip's
mechanism.

They are not a complete lifetime census. Window5 replaces one client between
its cuts, so the departed client's final counters are unknown. Even matching
endpoint sets cannot exclude a transient client without lifecycle evidence.
The corrected extraction must distinguish matched endpoint deltas from whole
window totals, preserve read brackets, and never treat missing witnesses as
zero. Assigning all642/626 funding events to window1 requires that additional
coverage proof. The archived `PureAckAdmissionRefusals` counters were zero;
they classify whole pure-ACK groups at terminal unrecovered admission failure
and omit owner-recovered failures and capture overflow. The fixture recorded
many earlier native batch refusals. These are different boundaries and lifetimes.

## Sparse-group hypothesis and its scoped result

Static review found that legal sparse, single-packet originals can still exhaust
count256/512 while byte budgets remain free. Group sizes of eight are maxima;
the ready-drain path need not wait to coalesce another packet. Advertised credit
and sender admission use bytes, while handoff admission additionally uses count.
Existing explicit-H1 nonzero lanes divide configured count by eight, yielding
32/64; Auto's lane-zero case retains256/512. This is existing lane behavior.

A four-root public-path witness is preserved in the Tests archive's
`pending-small-group` directory. It warms a real signed ACK advertisement, blocks
one actual callback, sends one-frame originals individually, checks free encoded,
RX, root and Pack rights at the exact count refusal, then releases the callback,
allows natural retry and verifies every ACK/delivery and joined custody.
Paired unpaused controls are included. All four have since run; the paused
cases enforce the configured fixed count and the unpaused controls pass, as
recorded in the resumed section below.

Before the correction, enabling adaptive count alone was insufficient: the
growth helper rechecked a local encoded-byte cap even when ordinary Pack
admission had already recognized exact paid-H1 ownership. The fix must preserve that exact
authority, shared quotas, finite channel ceiling and memory bounds. A growth
threshold above one also intentionally refuses the first saturated offer.
The minimal helper correction and its component tests are now in the working
tree. Mobile policy still disables adaptive growth. This local result does not
establish the cause or a fix for the measured throughput variation.

## Original review questions and their updated disposition

1. Which link/direction and TCP tuples account for window3's25 queue drops?
   Can a bounded event ring correlate them with ACK progress, the two TCP
   timeouts, Transfer retransmits and the four pending zero-byte streams?
2. Is the lower service/offered rate caused by TCP recovery, a provider/flow
   scheduling boundary, connection reuse/cancellation state, or host scheduling?
   Compare per-flow/per-provider timelines; count agreement alone is insufficient.
3. The sparse-group witness has run. Its optional-growth defect remains latent
   component work; per-path capture and setup starvation now take priority for
   the measured sequence. Constructor, sibling and physical-memory qualification
   remain separate requirements for any future count-policy change.
4. Capture exact per-window refusal deltas, oldest outstanding age, current queue
   occupancy, peer credit and root/RX use at the event. Current samples cannot
   prove the queue state at a transient stall.
5. Replay one controlled impairment at the observed boundary, with an unpaused
   control and an equivalent direct/reference TCP path. Reproducing ordinary TCP
   slowdown alone does not establish a URnetwork protocol bug.
6. Confirm repeatability with frozen-source independent process runs. Keep all
   original GET,100-page,ownership and memory gates. Keep only improvements
   supported by measurements; do not promote this busy-host run statistically.

Exact retransmit bytes, a dedicated restart counter, dynamic limiter wait time,
exact budget high-water marks and per-window CPU attribution are unavailable.
Native refusal returns the packet to the pool and relies on source TCP recovery;
accepted means SDK custody, not delivery. Exact refusal-stage attribution and
the mismatch with the zero pure-ACK admission counter still need investigation.

## Evidence and resume state

The committed matrix is
`tests/testdata/window-size-checkpoint-95b8/fast32-five-window-matrix.json`,
SHA256 `d14fcbfc7f705ae6fce4f2b5540865dc2a9a89cd0521d5e56516f5f826a0b8d3`.
It preserves source paths/hashes, but its `memoryCuts.queueCumulative` selected
`Windows.Clients[0]` even when client order changed. Those fields are invalid
as a cohort summary; the original remains immutable evidence. The identity-safe
correction is `FAST32-FIVE-WINDOW-MATRIX.corrected.rwq001-r1.json` beside the
temporary original, SHA256
`3011eb650d6cf20bfb1d235761709ce9315b39c80dc616971c53599656a4e72c`.
Its replacement fields preserve unknown lifetimes and unrelated historical
data. It does not correct other historical provider/flow attribution fields.
Summary and closure are under
`/Volumes/data2/urnetwork/temp/aggregate-95b8-retained-performance-diagnostic-r1`:
summary SHA256 `fce83d95d8238f8efd8438a82ae7253792d80a1e312bc81d53bb87fddd4728d3`,
closure SHA256 `7b3c8655e4769e51ee89ff071559f8d51efabb1af0e0c1ce7218bcead1004443`.
Source/module audits, process joins, FDs/leases and normal APFS detach passed.

All historical runs were closed for checkpointing. Investigation resumed on
2026-11-11 with both completed reviews. Source inventories must be regenerated
for the merged branches and subsequent changes; old performance receipts do
not qualify that source.

## Resumed post-merge investigation, 2026-11-11

The sparse-group witness has now run against merged Connect `391c679c`, using
Go1.27.4 and a private overlay that added only the test file. Both paused cases
refused the next original at count256/512, while their unpaused controls passed.
The refused packets retained real sender ownership and recovered after four
seconds of simulated time, with every original ACKed and custody joined. The
fixed-count policy deliberately disables growth: this result establishes count
pressure, not an implementation defect or attribution to window3.

A separate regression explicitly enabled one count-growth step before client
construction, without increasing the local byte limit. Current code still
refused the already-paid H1 original: the growth helper reapplied a local byte
cap that ordinary admission had already exempted using exact physical ownership.
Both32/64 paused regressions failed at that branch; both unpaused controls passed.
The minimal correction passes the existing paid-owner permission into the growth
helper and exempts only that same redundant byte check. Shared quota, finite
count/channel ceilings, ownership checks and configured fixed bounds remain.

The identical regressions pass with the correction: one growth step, no refusal,
and zero simulated recovery delay after releasing the callback, versus four
seconds before. Candidate qualification passed48 fresh checks:24 normal and24
race, including boundary controls and three independent confirmations of each
previously failing root per mode. No race, timeout or cleanup failure occurred.
The code correction and six positive regression tests are now in the working
tree. All six also pass normally and under race detection directly from that
canonical source, without an overlay:12 additional checks with clean custody.

This is **not a fix for the measured Fast5 dip**. Mobile policy
currently clears adaptive settings, so correcting the helper alone cannot
explain an improvement in those shipping32/64 profiles. No observed saturation
in the recorded consumer rows connects the helper to that dip. Constructor policy,
physical memory qualification and a source-bound synthetic replay remain open.
The next capture must preserve per-window events and connection/packet lineage
rather than assign cumulative refusals to window3.

The existing64MiB page1 trace also narrows the open browsing failure: TCP connect
took4.595s, TLS0.913s, and request-to-first-byte0.611s. Most of the6.119s delay
preceded connection establishment; these phases do not identify its root cause.

Current evidence is retained under `tests/research/resume-20261111`, including
the historical page1 extraction and source-pinned comparison/adoption packets.
Runtime receipts are in the separate temporary directories:

- `small-group-witness-20261111-401irk84` for the original fixed-count witness;
- `small-group-adaptive-old-20261111-v496fgbo` and
  `small-group-adaptive-candidate-20261111-siy2n9ug` for the paired regression;
- `small-group-adaptive-qualification-20261111-7jkvji3_` for48 selected checks;
- `canonical-adaptive-six-20261111-3wa4p9xf` for the12 canonical-source checks.

All are beneath `/Volumes/data2/urnetwork/temp`. Historical measurements
above retain their original pre-merge source attribution. Full correctness,
Fast5/full100 replay and device qualification are not completed by these checks.

## Both reviews incorporated, 2026-11-11

Both reviews and the complete `CODESTYLE.md` were read before further fixes.
The reviewer files are unchanged: review1 SHA256
`15ac8e555c66064c02435b84ea13d78c6f5d59ded358964ebd0d932a67cf59f6`;
review2 SHA256
`27ef005f3c0f4733990172b10d86378530bd920bf4585db212d6874fdb4df751`.

The actionable ledger is
`tests/research/resume-20261111/review-work-queue.json`:57 deduplicated items,
each with exact review line anchors, evidence, a deterministic discriminator,
controls and explicit pending status. `review-coverage.json` maps all226 literal
review anchors, including repeated suggestions and headings. Independent
Sol6.1MAX review found no missing substantive suggestion. Inclusion in this
ledger does not mean an investigation is complete.

The leading investigation is provider-path service and residence, including
outer TCP, host scheduling, pacing and reverse feedback. Historical provider6
delivered about39.6Mbps in window3 and36.0Mbps in window5, with697 and535
timeout deferrals; window5 had no new link queue drops. These are descriptive
signatures, not proof of Reno recovery, CPU saturation or a particular queue.
Socket topology, actual flight/queue state and current-source observations
must discriminate them. A larger queue or CUBIC fixture arm cannot replace
the original stress profile or establish a shipping fix.

The first concrete correction is the identity-safe Go extractor under
`tests/research/resume-20261111/review-matrix`. It preserves the old matrix,
distinguishes counter sums from lifetime high-water maxima, and leaves retired
or transient lifetimes unknown. All16 focused tests pass normally and under
race detection. The actual hash-pinned old Python selector fails on the same
synthetic raw input with100→5 instead of the correct105→107. Execution and
independent output validation are retained in
`/Volumes/data2/urnetwork/temp/windowsize2-rwq001-execution-r1`; inputs and source
pins are unchanged, unrelated matrix fields are preserved, and all children
joined. Independent Sol6.1MAX closure review confirms this narrow correction.
Only `RWQ-001` is complete. Next are a bounded
periodic capture with exact scalar refusal reasons, supported per-path state
and all-flow connection phases; a current-source replay; and controlled
impairment/reference, congestion-control, delay, CPU and placement arms.

Product alternatives remain explicit: bounded first-SYN retry, lifecycle
capacity at the applicable raw and receipt-held gates, exact ACK coalescing, the existing
latency budget, socket backlog bounds, generation-safe cancellation cleanup,
aggregate receive backing/static count and prompt refused-tail recovery.
None is accepted merely because a reviewer proposed it. Existing SYN remote
exclusion and latency-bound floor/cold-start/path-change exceptions need tests
before policy changes. Deadline relaxation is outside the user's requested fix.

The new fixed-scalar recorder passed13 focused normal tests and13 race tests;
the receipt is `/Volumes/data2/urnetwork/temp/windowsize2-recorder-r3-execution-r1/FINAL.json`.
Its core covers bounded storage, ordinal deduplication and stop/join races.
Actual fixture wiring, old-collector discrimination and instrumentation overhead
remain open under `RWQ-007`. No private TCP accessor is being exported for this
capture; unavailable sender queue/cwnd/rwnd and consumer periodic pacing stay
explicitly unknown. Private endpoint mechanism tests remain in Connect `_test.go`.

The first-SYN experiment corrected an assumption in the initial investigation:
`ip_immediate_ack.go` leaves SYN-only/FIN/RST ordered. Only ACK/SYNACK use the
automatic immediate-delivery option, so a pure first SYN is not blocked merely
because ACKs fill `immediateSendCount`. The first control expected that wait and
failed; its immutable receipt is
`/Volumes/data2/urnetwork/temp/windowsize2-first-syn-component-r1/FINAL.json`.
That combined assertion does not establish which runtime condition failed and
is not evidence of a production defect. The revised r2 control passes normally
and under race detection: an ordered SYN writes while the actual ACK immediate
count stays3, and original receipts later retire it. In both modes, the separate
capacity1/3 raw-pressure roots fail only with the intended first-opening marker:
actual raw slots reopen without a SYN until a second native offer. The modeled
1s retry is a control, not measured application RTO. All source pins remained
unchanged and children joined in
`/Volumes/data2/urnetwork/temp/windowsize2-first-syn-component-r2`.
The new public fixture fills ACK capacity until refusal and accepts a legitimate
reservation policy while requiring cap1 ACK progress. Same-flow lifecycle ordering and transport
FIFO remain independent setup hypotheses.

Astra MAX owns debugging and implementation, with Sol6.1MAX second opinion;
Sol6.1LOW executes frozen commands and collects evidence. The adaptive tests now
use a compliant external consumer callback and internal `_test.go` fixture bridge,
keyed profiles, declaration comments and an explicit zero-recovery assertion.
That rewrite passes all six roots normally and under race detection in aggregate
r4; composed sibling coverage remains open.
The prior48selected and12canonical checks remain immutable component evidence. The Android
diagnostic patch has focused host receipts but no APK or device qualification.
Current Fast5/full100, full correctness and physical32/64 qualification remain
open; Android cannot prove physical iOS footprint.

Qualification uses one authoritative source-pinned aggregate with every accepted
fix merged. New positive regressions join its selected suite and share each
build; one aggregate performance replay follows. There is no per-fix or
combinatorial performance candidate matrix. Superseded unrun preparation
generations are retained history. Minimal old-code and mechanism witnesses are
explicitly scoped evidence and do not create separate product candidates.


The first-opening remedy is implemented in the one aggregate; its qualification
is incomplete. At an exact raw protocol refusal, real retained-memory permission
funds a tentative packet owner and all configured source watches. Only an overall
false return after candidate cleanup can publish it. A newer ordinary offer
invalidates never-materialized custody, and exact source-ordinal/readiness
identity checks prevent stale wire or an old grant clearing a newer owner.
Actual materialization grants the existing SYN-cache authority; optional backing
exhaustion preserves the original false/source-retry fallback. The selected
batch has17first-SYN roots, six adaptive roots and five existing adjacencies.
No draft test or source review is counted as a passing execution.

The first aggregate batch compiled and reached the public root's same-time SYN
and later-ACK checks, then failed an inappropriate refusal-only health oracle.
A real peer ACK preserves `pendingSendTime=lastSendAckTime` even after outstanding
custody reaches zero; the actual stall predicates correctly treat that state as
idle. The fixture now checks the parked refusal against its exact prior ledger,
then checks successful packet/byte receipts and the actual idle/stall classifier.
Production accounting was unchanged. The r1 receipt remains in
`/Volumes/data2/urnetwork/temp/windowsize2-aggregate-connect-batch-r1`.

The second batch compiled and passed13 normal roots, including all three public
first-SYN roots and the backing, cancellation and source-boundary controls. It
then stopped in the stolen-vacancy fixture before a retry could dispatch. The
fixture waited for a competing ACK to reach retention while the first ACK still
owned the sole immediate slot. Its unreleased hook also blocked cleanup, causing
the secondary pool/deadlock failures. Both affected competitor controls now hold
the actual second dequeue, feed the first real peer receipt, and only then
release readiness. Exact raw capacity, retry/refusal counts, watch identity,
no-spin and peer-ledger checks remain; every blocking hook has a scoped release
and cancellation escape. Raw admission retires through `releaseRaw` and
`releaseAdmission`, not at dequeue. These are fixture corrections, with no new
production change. The r2 receipt and exact executed fixture are preserved.
The r3 batch passed14 normal roots, including the corrected stolen-vacancy
control, then timed out in the wide-candidate fixture. That fixture made the
healthy candidate wait for a typed terminal refusal, but those observations are
published only after all candidates return. The corrected fixture holds the
healthy candidate at an independent existing barrier and runs one owned native
offer. The main test observes real tentative credit while the first source is
parked, then releases and joins the offer before checking its exact result,
deferred recovered-refusal notification, zero leaked credit and one physical SYN.
Production observation ordering is unchanged. Sol6.1MAX reviewed the narrow
correction. The r3 failure receipt and executed fixture remain intact in
`windowsize2-aggregate-connect-batch-r3`.

Aggregate r4 is terminal: all28 selected roots pass normally and under race
detection. The identical public fixture on exact prior production passes its
ordered-SYN control and fails the cap1/cap3 first-opening roots with only the
intended marker in both modes. All66 stages joined their children and process
groups, stderr is empty, and4879 canonical source pins plus seven old donors
remain unchanged. The receipt is
`/Volumes/data2/urnetwork/temp/windowsize2-aggregate-connect-batch-r4/FINAL.json`,
SHA256 `120da78bf174379049672c9a720e9d78992e6f56a126e2d610aa3daa1431b8bb`.
Astra, Sol6.1MAX and root independently checked this component evidence.
It qualifies optional paid first-SYN recovery and the selected adjacent controls;
it does not identify the historical Fast/full100 stall cause or qualify current
performance, full correctness suites or devices. `RWQ-024` remains open for that
workload evidence; only `RWQ-001` is fully closed.

Canonical Server `go.mod` now aligns to1.27.4 with no other module or sum change.
The namespace helper's r6 infrastructure controls pass51/51, including eight
actual Go command outcomes; the receipt is
`/Volumes/data2/urnetwork/temp/windowsize2-namespace-controls-r6/CONTROLS.json`.
This does not establish the final aggregate's dependency namespace, compilation
or runtime qualification. Diagnostic capture and unchanged original performance
roots must qualify the same source candidate; diagnostic green alone cannot
replace Fast5/full100 acceptance.


## Inherited-provider first opening and cleanup, 2026-11-11

The next original-root measurements collected all four gates on one unchanged
aggregate. Fast32 window 5 and Fast64 window 4 failed the unchanged 5 s small-request
guard; full100-client32 passed, while full100-client64 page1 took 6.430 s.
Fast32 spent 9.098 s in its owned dial; the failed Fast64 request never completed
its dial. Full100-client64 spent 4.587 s connecting and another 1.389 s in TLS.
These timings localize delays but do not identify the failed native endpoint or
its provider. The two-attempt dial schedule and SYN offer/refusal counts are
compatible with a setup retry explanation; overflow and missing tuple joins
prevent a claim that one mechanism caused every measured stall.

A new deterministic native-entry regression exposed a concrete missed wakeup.
A fresh tuple inherited a healthy provider before its first SYN, so the prior
unbound-only opening predicate excluded it from optional capacity recovery.
With a real raw gate full and ample paid backing, opening the gate produced no
SYN until an explicit second native offer. The fix carries the exact immutable
inherited-selection receipt through the existing opening/readiness/dispatch
path. It preserves the original false return, actual byte permission, final
provider health policy and the materialized retry's prior unbound policy.

Two further tests exposed optional packet/readiness credit retained after an
admitted replay was discarded before materialization, or after its selection
changed between worker eligibility and dispatch. Both produced the exact
6800-byte custody failure. The fixes retire only the matching current pending
owner and release credit outside the flow lock. They preserve refusal rearm,
newer owners and retained prefixes. Source-close tests distinguish closing a
shared physical source from damaging a sibling's logical owner; no provider
failure classifier was changed.

All 28 existing selected roots plus 8 new inherited-opening/lifetime roots pass
in one normal invocation and one race invocation: 36/36 in each, empty stderr,
unchanged source pins and all children joined. Exact failed and passing outputs,
fixture corrections and final five-product/two-test file pins are indexed in
`tests/research/resume-20261111/inherited-first-syn-r1/QUALIFICATION.r1.json`.
The normal receipt is
`/Volumes/data2/urnetwork/temp/windowsize2-inherited-first-syn-combined36-normal-r1/FINAL.json`;
the race receipt is the corresponding `combined36-race-r1/FINAL.json`.

This is component evidence. The next single aggregate build and all four
original performance gates remain pending at this snapshot. The existing 57-item
work queue and 226-anchor coverage are unchanged; `RWQ-024` remains open for
actual workload causation, and other provider, pacing, feedback, lifecycle and
full100 setup alternatives remain open. Full suites and devices are unqualified.
