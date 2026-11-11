# WINDOWSIZE2: remaining variation after the receive-backing checkpoint

2026-11-11. Peer-review request; no new causal claim or baseline promotion.

The aggregate passed both synthetic Fast5 cases. The remaining question is why
the 32 MiB sequence varies from **268.5, 308.8, 141.9, 236.8 to 220.4 Mbps**,
especially the third window, about 54% below the second. Small requests still
passed. We want a deterministic reproduction of any avoidable protocol or
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

All five windows passed their original gates. Mean reported goodput was about
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

The second and third windows both launched eight bulk flows and recorded eight
provider dials. The provider mask stayed66; the dial distribution changed from
3/5 across the two providers to2/6. This was a fixed synthetic provider
population, not uncontrolled public-provider pairing.

| Observation | Window 2 | Window 3 |
| --- | ---: | ---: |
| Provider initial writes | 42,276 | 20,354 |
| Provider initial message bytes | 410,053,012 | 193,201,214 |
| Exported native offers | 43,251 | 22,899 |
| Reused bulk connections | 1 | 3 |
| Maximum pending zero-byte streams | 1 | 4 |
| Flows with zero bytes at termination | 1 | 0 |
| Provider selective gap writes | 0 | 0 |
| Provider reliable-admission wait count/duration | 0 /0 | 0 /0 |

Window3's per-flow delivered bytes were approximately77.1/14.4/7.8/10.8/50.9/
5.8/4.8/5.9 MB. Window2 had116.3/42.3/36.5/37.3/88.7/32.1/32.9/0 MB.
Two flows dominated window3 while several others made much less progress.
The pending-zero-byte maximum also rises in other windows, so it is not by
itself an explanation. Recorded stops were measurement-window cancellation;
these are not eight independent unexpected connection failures.

Configured link rates remained2 Gbit/s, burst64 KiB, queue2 MiB/4096 packets.
Access delay was20 ms each direction; provider delay was zero. Application MTU
was1280 and outer MTU1500. Injected random loss, jitter, reorder and duplication
were disabled. **Queue drops were allowed and occurred.** A clean configured
loss model therefore does not mean the observed run was lossless.

The lower initial-write rate and sampled RX use are compatible with reduced
upstream service/progress. They do not identify its cause. Twenty-five queue
drops and two TCP timeouts could matter, but the current aggregate counters
cannot establish which came first or which flow lost data or feedback.

## Receive mechanisms already established

Focused tests previously demonstrated two distinct constraints:

1. Encoded credit needed enough retained receive backing. For logical quota
   `L`, the candidate uses `ceil(7*L/2)` physical allowance and a2/7 conversion
   when advertising the attached total, preserving `L` and the old Head/Pack
   quotas. At32 MiB:physical RX7,633,633 bytes, encoded credit2,181,038 bytes,
   shared root21,810,380 bytes. Those are overlapping allowances, not eager
   allocations or a complete Go-runtime footprint bound.
2. Independent queue-count pressure caused46 refusals/retries at count85 with
   approximately5 MB of RX capacity still available. Matching the measured
   eight-packet-group flight to count256 eliminated loss/retries in that focused
   sustained case. It did not prove every legal grouping shape is covered.

The aggregate also retains exact advancing TCP feedback when an initial native
offer is refused and bounds fresh DATA refill while an existing recovery cohort
retires. These have separate deterministic mechanism tests. A proposed broader
resend adjustment was rejected because it duplicated healthy traffic.

The 32 MiB process accumulated642 ordinary RX refusals and626 final-funding
refusals. First-refusal witness slots appeared in window1. Exact per-window
totals are unavailable; absent window2/3 witness slots do **not** establish zero
refusals there. These totals must not be assigned to window3 by assumption.
Pure-ACK Pack refusals were zero in both Fast5 profiles.

## Specific adjacent hypothesis awaiting a runtime test

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
Paired unpaused controls are included. It compiled, but its test body was canceled
for this checkpoint: **all four runtime outcomes remain unrun**.

Enabling existing adaptive count alone may be insufficient: its growth helper
still checks a local encoded-byte cap even when ordinary Pack admission has
already recognized exact paid-H1 ownership. A follow-up must preserve that exact
authority, shared quotas, finite channel ceiling and memory bounds. A growth
threshold above one also intentionally refuses the first saturated offer.
No adaptive change or new memory model is implemented here. Recheck these source
findings after the upstream merge before treating them as current-main defects.

## Questions and next experiments for reviewers

1. Which link/direction and TCP tuples account for window3's25 queue drops?
   Can a bounded event ring correlate them with ACK progress, the two TCP
   timeouts, Transfer retransmits and the four pending zero-byte streams?
2. Is the lower service/offered rate caused by TCP recovery, a provider/flow
   scheduling boundary, connection reuse/cancellation state, or host scheduling?
   Compare per-flow/per-provider timelines; count agreement alone is insufficient.
3. Run the prepared sparse-group witness before tuning depth. If it reproduces,
   add a deterministic old-fail/new-pass fix with actual constructor wiring,
   healthy sibling/protocol progress and32/64 memory qualification.
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
Exported native offer/refusal counters have separate semantics that should be
audited before interpreting them as user-packet loss.

## Evidence and resume state

The committed matrix is
`tests/testdata/window-size-checkpoint-95b8/fast32-five-window-matrix.json`,
SHA256 `d14fcbfc7f705ae6fce4f2b5540865dc2a9a89cd0521d5e56516f5f826a0b8d3`.
It records source paths/hashes and separates cumulative counters from window
deltas. Summary and closure are under
`/Volumes/data2/urnetwork/temp/aggregate-95b8-retained-performance-diagnostic-r1`:
summary SHA256 `fce83d95d8238f8efd8438a82ae7253792d80a1e312bc81d53bb87fddd4728d3`,
closure SHA256 `7b3c8655e4769e51ee89ff071559f8d51efabb1af0e0c1ce7218bcead1004443`.
Source/module audits, process joins, FDs/leases and normal APFS detach passed.

All runs were closed for checkpointing. Work is to pause after this document is
committed and pushed. Resume with reviewer feedback, the sparse-group witness,
the64 MiB page1 failure and physical memory qualification. Do not silently reuse
old source inventories against the newly merged main branches.
