# Five-window bulk ramp: evidence and questions for peer review

Date: 2026-11-10. Status: unresolved bulk-performance issue; no window-size root cause established.

The latest 64 MiB synthetic Fast.com sequence passed, but throughput progressed
from **75.2, 64.9, 112.1, 155.9 to 235.1 Mbps**. We want to understand why the
first two windows are slow and why the same aggregate still stalls a mandatory
small request under the 32 MiB client policy. The title is a research question:
we have not proved that automatic window resizing, rather than recovery,
provider selection, feedback admission or retained ownership, causes the ramp.

Please distinguish the advertised TCP window, Transfer send/recovery windows,
receive/reorder backing, contract credit and the MultiClient provider window.
They are different controls. We do not want another broad memory-model rewrite
or a larger timeout based on correlation alone.

## Latest experiment and scope

This is a local synthetic H1 download workload, not a browser measurement
against public fast.com and not a physical Android memory qualification.

- One retained client/provider fixture runs five consecutive 10-second windows.
  Workers are canceled and joined between windows; the fixture is not reset.
- TLS and HTTP/1 traffic uses four owned origins. Download concurrency grows
  adaptively from one to eight streams; warmup is disabled.
- A mandatory small GET starts about 500 ms into each window. Completed probe
  latency must stay below the original five-second gate; canceled probes have
  separate accounting. Gates and measurement durations were not extended.
- The access link is configured for 2 Gbps in each direction, 20 ms one-way
  delay, no injected loss/jitter/reordering, a 2 MiB queue and 64 KiB burst.
  Queue drops are permitted and counted. The provider link is clean-LAN,
  also configured for 2 Gbps. Inner MTU is 1280; outer MTU is 1500.
- Actual SDK mobile constructor branches use client targets of 32 or 64 MiB.
  Provider factories have the named Desktop128 target. The fixture supplies
  12 available exits: initial evaluated selection is quality 5 plus speed 1
  (six rows) at 32 MiB and quality 10 plus speed 2 (12 rows) at 64 MiB.
  Comparing profiles therefore does not isolate receive-memory size: their
  production selection policies differ too.
- Each profile starts in a separate process. The five windows within a profile
  share its fixture. The 64 MiB run therefore does not inherit the 32 MiB run's
  Go objects or receive queues.
- The host was busy, explicitly permitted by the operator. These are diagnostic
  measurements, not a quiet-host statistical baseline promotion or a paired A/B.
- The co-resident simulator contains client and provider runtimes. An SDK
  admission target is not a measured per-device Go or OS-footprint ceiling.
  Physical 32 MiB iOS-profile and 64 MiB Android qualification remains pending.

The aggregate retains the previous accounting model. It includes the qualified
bounded native window-reopening retry described below. The 32 MiB physical RX
and encoded advertisement remain 2,181,038 bytes; the experimental RX2X arm is
parked. We did not change capacity during this sequence.

## The 64 MiB progression

The requested rounded rates are the report's whole_run_mbps field. The
WholeMeasurementMbps values below use the measurement-byte accounting and
are almost identical. A denominator artifact does not explain the ramp.

| Window | Reported Mbps | Measurement Mbps | State | Largest completed loaded GET |
| --- | ---: | ---: | --- | ---: |
| 1 | 75.1638 | 75.1302 | valid | 4.229 s |
| 2 | 64.8673 | 64.8277 | valid | 3.059 s |
| 3 | 112.0801 | 112.0401 | valid | 0.607 s |
| 4 | 155.8981 | 155.8672 | valid | 0.390 s |
| 5 | 235.0944 | 235.0820 | valid | 2.433 s |

All five workers/windows joined and the original verifier passed. Whole case
duration was 59.37 s, including setup and final work. Each measured window was
approximately 10 s, not a progressively shortened denominator.

### Recovery and memory observations

| Window | Sampled client receive accounting peak, MiB | Sampled shared-root peak, MiB | Provider Transfer timeout resends | TCP retransmits | Link queue drops |
| --- | ---: | ---: | ---: | ---: | ---: |
| 1 | 4.156 | 5.441 | 1,591 | 1,519 | 1,408 |
| 2 | 4.158 | 4.522 | 3,066 | 163 | 0 |
| 3 | 4.227 | 4.549 | 371 | 343 | 0 |
| 4 | 0.929 | 1.197 | 4 | 199 | 0 |
| 5 | 0.396 | 0.664 | 4 | 141 | 0 |

The client shared-root budget is 43,620,760 bytes, about 41.6 MiB. It is far
from exhausted in these samples. The Head recovery budget is 4,362,076 bytes.
Its cumulative reserved/released bytes show substantial real use early in the
sequence; zero instantaneous use is not evidence that recovery was unavailable.

These managed peaks are sampled, component-wise maxima, not a transactional
budget partition or an exact physical RX capacity. ClientReceiveByteCount sums
ordinary RX, shared Head recovery and separately owned Pack memory. It is an
occupancy observation, not an advertised wire-window measurement. Do not
sum separate peaks or infer a budget violation from their ordering. Exact
physical RX window cuts are unavailable for the 64 MiB arm in this report.

Provider Transfer timeout resends and TCP retransmits are different layers;
they must not be added together as a unique lost-packet count. LinkLossDrops
was zero in all five windows. LinkQueueDrops was nonzero in window 1, so this
run is not evidence of an entirely lossless path. Window 2 was slow without
new link queue drops, but could still inherit unresolved ownership/recovery
from window 1.

### Connection phases and selection also change

- Window 2's TCP dials mostly finish in about 41–136 ms, but recorded TLS
  handshake intervals range from about 123 ms to 4.553 s. This is not simply
  a slow initial provider lookup or TCP setup for every flow.
- Windows 3–5 use the same workload-origin-observed provider bitset, but throughput still
  increases from 112 to 156 to 235 Mbps. A bitset is not a connection-weighted
  performance measurement and does not prove identical flow allocation.
- Connection reuse counts across the eight bulk streams are 0, 0, 1, 2, 2.
  Reuse could contribute but does not by itself explain the entire change.

The following indices refer only to synthetic fixture rows, not deployed exits.
They come from origin-observed workload requests, not every evaluated exit or
every TCP/TLS attempt. The evaluated MultiClient bitset is 8175 throughout the
64 MiB run; a failed TLS attempt may never reach an origin and thus lacks this
observation. Do not infer its chosen exit from absence in this table.

| Window | Workload-origin-observed provider rows | Reused bulk connections |
| --- | --- | ---: |
| 1 | 0, 3, 6, 7, 8, 9 | 0 |
| 2 | 3, 6, 7 | 0 |
| 3 | 7, 8, 9 | 1 |
| 4 | 7, 8, 9 | 2 |
| 5 | 7, 8, 9 | 2 |

The report does not expose remaining contract credit. Initial client contract
size is 1 MiB and standard client/provider contract size is 128 MiB; these
settings are evidence, not proof of credit exhaustion. Per-provider configured
retained-send limits are recorded as 5,752,188 bytes for active rows. Current
per-sequence outstanding bytes, oldest age and effective peer credit would be
stronger evidence than configured limits or lifetime maxima.

## The 32 MiB companion still fails

| Window | Reported Mbps | State |
| --- | ---: | --- |
| 1 | 15.7174 | valid |
| 2 | 4.7903 | invalid: mandatory small-request stall |
| 3 | 12.2806 | valid |
| 4 | 216.8644 | valid |
| 5 | 155.3464 | valid |

Window 2's small GET starts about 500 ms into its 10-second window and never
completes. It is canceled after 9.500678 s. This is a real failed original gate,
not final owner-join time or an inflated startup denominator. The 4.7903 Mbps
number is preserved diagnostic output, not a qualified speed score.

Five bulk TCP dials finish in roughly 42–80 ms, but their TLS handshakes never
finish by the window end. Another bulk stream reuses its connection and writes
its request in roughly 56.5 microseconds, yet receives no response header.
Only two streams assigned to origin 3 deliver bytes in this window, totaling
about 5.988 MB. Origin and fixture joins report no errors.

The 32 MiB receive occupancy is near its 2.18 MB child limit at specific early
cuts/samples while the shared root has about 19 MB spare. This is not continuous
occupancy evidence; a later window-5 sample is only 375,744 bytes. Same-client
generated-client PackHandoffDropCount deltas by window are
2,122 / 988 / 2,248 / 54 / 0, aggregated by client identity. ReceiveQueueDropCount
is zero at these cuts. Pack handoff, receive/reorder backing, decode and final
funding refusal are different boundaries; these counters must not be conflated.
The handoff drops support investigating a local bottleneck but do not prove
that simply increasing the RX child quota fixes it.

The counter is not a single queue diagnosis. Current canonical transfer.go
increments it for a fresh-opening identity mismatch, a failed nonblocking
ReceiveBuffer.Pack handoff, and zero-timeout handling of closing/replaced or
older receive-sequence workers. transfer_receive_memory_decode.go also
increments it when ordinary RX, held-head repair, Head recovery and fresh-opening
funding all fail, before ReceiveBuffer.Pack. Review all callers, including
that decoder path. Reason-specific sequence-generation and custody evidence
is needed to distinguish funding failure, handoff saturation and lifecycle churn.

A subsequent whole-run reconciliation narrows this run's drops: final
all-funding refusals total **5,412**, exactly matching the generated clients'
PackHandoffDrop delta of **5,412**; ReceiveQueueDrop remains zero. The other
**2,395** ordinary-RX refusals succeeded through Head recovery and are not final
drops. These independently timed aggregate counts strongly support decoder
funding refusal as the observed drop boundary. They do not identify the
mandatory window-2 GET or prove why its funding was unavailable. The scalar
readback, including exact field paths and census timestamps, is
`/Volumes/data2/urnetwork/temp/native-window-retry-pack-drop-readback-r1/READBACK.json`
(SHA `78b970375ba7ec8e3beabc3ff630644a88a639569830420441078ca6983cf2cb`).

The captured all-funding-refused witness is ordinal 493 while the receive
floor is 416: it is 77 messages ahead, not the current head. It has neither a
promise nor a later-owned original; ordinary RX holds 2,179,856 of 2,181,038
bytes and Head usage is zero. Refusal is consistent with preserving recovery
capacity for the missing prefix. This witness does not demonstrate a broken
current-head recovery path. The separately captured eight-DATA-frame current
head at floor 862 acquired 30,544 Head bytes and retired successfully. Current
head eligibility is not restricted to singleton DATA frames. The missing
window-2 probe still requires its own exact boundary capture.

Counterexamples matter:

- Window 2's Head path reserves and refunds about 21.17 MB cumulatively.
  A separately recorded window-1 current-DATA denial obtains Head funding and
  a real terminal ACK 21.929 ms later. That observed recovery works; it
  does not establish what happened to the missing window-2 probe.
- Window 2 counts 338 advancing-ACK refusals; fast window 4 counts 24,761.
  Raw refusal totals alone cannot explain the failure.
- Window 2 counts thousands of offered/refused RSTs, but their precise timing
  relative to cancellation is unknown. A faster window also has thousands.
  RST count alone is not a root cause.
- Missing phase/tuple information for the mandatory GET prevents identifying
  its exact stalled boundary. Bulk-stream traces do not substitute for it.
- The 32 MiB arm's whole-process Go memory-limit observation is MaxInt64 before
  and after, not a 32 MiB process cap. Across its 50 measured seconds, GC assist
  increases by 0.0453 CPU seconds and total GC by 0.8333 CPU seconds. These are
  co-resident whole-process totals, not per-window pause attribution; they do
  not prove or disprove a particular GC pause, but cannot justify blaming a
  configured Go soft-limit throttle that was not installed in this process.

## A real deterministic fix already exists, but is incomplete

The native cadence fixture deterministically reproduces this narrower bug:

1. A real TCP connection advertises zero window.
2. Finite pressure occupies the actual three immediate and three raw protocol
   owners. A real reopening ACK is refused before Transfer admission.
3. All six accepted originals and actual terminal receipts drain in about
   40 ms, with no permanent DATA gap.
4. Old code still waits about one second for natural TCP persist. Reoffering
   the exact same packet after capacity opens avoids the wait.

The bounded retry fix keeps one exact unsent singleton reopening ACK on the
existing flow/source and retries on actual capacity. It does not change DATA
budgeting, create a worker/timer, invent ACKs or move a connection between exits.
New native feedback invalidates an unsent copy; final validation precedes
sequence ordinal assignment. Original refusal observations stay refused;
successful recovery creates a separate Transfer owner.

This passed 58 normal and 58 race roots, with complete ownership/source/module
closure. The corrected case completes HTTP and reuses the connection without
persist. Nonetheless, the fresh 32 MiB Fast5 failure above remains. This is a
qualified mechanism fix, not a complete explanation of the bulk progression.

Current eligibility is deliberately narrow: singleton pure ACK, last admitted
window zero, same ACK frontier. It does not recover every advancing positive-
window ACK or unsupported multi-message batch. We must test an actual missing
condition rather than assume the existing passing fixture proves completeness.

## Next deterministic falsifier and fix loop

The current candidate for reproduction is a lost-feedback eligibility gap:

1. Establish real TCP/HTTP and admit an earlier positive advertised right edge.
2. Deliver DATA until that right edge is exhausted. Record actual ACK frontier,
   DATA sequence end, outstanding ownership and subsequent TCP recovery type.
3. Finite protocol pressure refuses the actual advancing/zero feedback and a
   later real positive-window ACK. No synthetic ACK or permanent network hole.
4. Drain all accepted pressure owners and real receipts. Require prompt HTTP
   progress once the resource is available. Reoffer the identical real packet
   after one slot opens as the positive control.
5. A root-cause claim requires the current implementation to fail the progress
   invariant after clean joins and the fix to pass that same workload normally
   and under race. If current code passes, record that the proposed gap was
   falsified and investigate the next candidate. Do not manufacture a failure
   with extra loss/holds or count a compile/setup failure as the old-code failure.

An old positive grant with outstanding unacknowledged DATA may wait for DATA
RTO, not persist. The test must establish the actual predicates and recovery
event rather than labeling every stall a zero-window persist problem.

The fixture under construction captures two genuine positive ACKs with
different frontiers before the real zero-window feedback. To occupy the six
protocol owners without introducing duplicate-current-ACK fast recovery, it
uses byte-exact older ACK copies. Those pressure copies use the existing Client
entry on the observed Transfer source/key; replaying them through Multi would
also alter its native admission history and could manufacture the condition
being tested. Assert both the provider's applied frontier/right edge and the
client's admitted-feedback metadata before and after this pressure. The zero
and subsequent advancing feedback still use the actual indexed native Multi
entry. These are required premises, not an already reproduced failure.

If this fails as predicted, extend only the bounded eligibility needed for
actual advancing feedback. Preserve exact packet/options bytes, source and
generation, newest-feedback invalidation, failed-send semantics, final ordinal
fence and once-only ownership. Measure allocation/locking cost: broadening an
ACK retry path can turn a rare operation into a hot path. No new lanes, broad
memory-model change, timeout extension or unmeasured default-size increase.

### First falsifier execution: incomplete, not a qualified root cause

The first normal execution compiled successfully. The desired-progress root
completed the real 36,400-byte HTTP body and same-flow reuse, but observed DATA
recovery about 959 ms after the six accepted owners drained. It also incorrectly
classified an ordinary regenerated ACK with different timestamp bytes as a
changed retry. The manual control then had only one distinct positive ACK
before the zero-window boundary, failed its setup premise and panicked when
cleanup removed a flow before the native bridge inspected it. This attempt
does not supply the required failing-root/passing-control pair.

Its source/module checks and cleanup closed successfully; the failed test
results are preserved at
`/Volumes/data2/urnetwork/temp/native-positive-window-feedback-C-qualification-r1`
(`CLOSED-HANDOFF.json` SHA
`2978404ca42e2460f8c1647cd5cdea5b86783d5e42417f09b28d3ec30722b2af`).
The test-only successor stages two real origin response prefixes and waits for
each increasing native ACK's actual terminal receipt before sending the rest.
This fixes the setup ordering without manufacturing ACKs, TCP state or loss.
Exact-byte matching remains mandatory for the manual reoffer control; ordinary
new feedback is not mislabeled as a retained retry. A separate canceled-parent
offer test pins the fixture's buffer return when its flow has disappeared.
Successor review and execution are pending; production eligibility is unchanged.

### Corrected falsifier: local mechanism reproduced, Fast5 attribution pending

The test-only R2 successor produced the required failing-root/passing-control
pair on unchanged production. After finite 40 ms service, all six accepted
owners drained in 41.136 ms. Useful advancing feedback remained unowned while
the previously admitted positive TCP grant was exhausted. The provider resumed
through actual retained-DATA retransmission 958.818 ms after the drain. The
desired-progress root failed only its typed responsiveness invariant, after
completing the exact 36,400-byte HTTP body and same-flow reuse. Reoffering the
identical ACK restored progress without that recovery penalty. The canceled-
parent helper control also passed. There were no extra assertions or panics.

Closure:
`/Volumes/data2/urnetwork/temp/native-positive-window-feedback-C-qualification-r2/CLOSED-HANDOFF.json`,
SHA `cee6589dfafb9b763db168e64c462632b28618cfc8f909814ff10fd04323af52`.
Source/module audits and all process/lease cleanup passed. This proves a local
feedback-recovery gap, not that it explains the full Fast5 ramp or missing W2 GET.

The proposed bounded fix also accepts genuinely advancing positive-window
feedback, retaining the existing source/generation, newest-feedback and final
ordinal fences. Ordinary ACK preparation keeps only scalar/selection metadata;
the optional attempt is created only on actual raw admission refusal. New
controls cover IPv4/IPv6, ACK sequence wrap, exact retained bytes and no heap
allocation during ordinary preparation. This product successor is under review
and has not been executed; no speed improvement is claimed.

Separately, the unchanged common candidate completed all 36 race controls,
joining its earlier 36 normal passes. Closure:
`/Volumes/data2/urnetwork/temp/native-window-retry-common-race-qualification-r1/CLOSED-HANDOFF.json`,
SHA `9616a72b2791bd864e0840ad1aceb21678233952cbfa4fe0a116f8f2d490ddf1`.
Those 72 passes do not qualify the new product successor or erase Fast32's failure.

Both latest binaries initially waited before Go entry, then ran naturally within
the original owner deadlines. Read-only host evidence attributes C's startup
wait to a 142.371 s Gatekeeper/XProtect queue followed by a 0.852 s scan. The
test's approximately one-second TCP recovery penalty is separate from that
pre-Go delay. No security settings or active deadlines were changed.

In parallel, improve the diagnostic fixture before another attributed run:

- Capture bounded critical slots per window. All eight critical ACK snapshots
  and three first boundary slots in the current 32 MiB report belong to window
  1; none identifies the window-2 GET. Preserve cumulative ACK baselines and
  tag late old-flow events instead of assigning them to the new window.
- Capture the mandatory GET's dial/TLS/connection/request/header phases and
  exact synthetic flow identity, including failure before GotConn.
- Pin these observation boundaries with deterministic second-window/late-event
  controls. The 64 MiB arm lacks the common boundary trace entirely; its null
  consumer diagnostic samples mean unavailable data, not zero pressure.

Then use one aggregate candidate: focused old-fails/new-passes tests, a short
overlapping browsing/bulk check, unchanged five-window Fast5 and full100 at both
profiles, then real-device MEMSTEADY/PERFVAR. Keep failed attempts and compare
matched observation modes before claiming a performance gain.

## Questions for reviewers

1. Can finite lost advancing feedback leave the provider's usable TCP credit
   exhausted after our admission pressure drains, while the current retry's
   zero/same-frontier eligibility rejects the only useful update?
2. What exact owner/credit predicate would distinguish that from an ordered
   DATA hole, retained-future debt or an uncompleted first TLS flight?
3. Could old-window cancellation/RST traffic and retained reliable ownership
   isolate some providers/flows without a shared-root shortage? Which explicit
   ordering would reproduce it without permanent loss or sleeps as the proof?
4. Which measured control best separates receiver backing, Transfer recovery,
   TCP congestion state, provider selection and contract-credit startup?
5. Are there adjacent admission paths where failed/partial native groups lose
   the feedback needed to release the very resource blocking new handshakes?

## Other latest outcomes and historical context

Both full100 cases passed in this aggregate: 100 HTML requests, 400 assets and
99 overlapping navigations each. Maximum HTML TTFB was 2.669 s at 32 MiB and
4.682 s at 64 MiB; controller page 1 was 2.046 s and 4.682 s respectively.
Pre-navigation preparation is reported separately. These passes do not erase
the failed mandatory GET under bulk load or prove all observed device stalls
are resolved.

An earlier 64 MiB synthetic run recorded approximately
224 / 174 / 313 / 277 / 299 Mbps. It demonstrates useful historical headroom,
not a paired estimate of this fix: source/observation state, provider selection
and host activity differ. Real fast.com provider pairing can add variance, but
cannot excuse this local synthetic window-2 failure.

The independent full correctness run exposed a test-only retention-field race.
Its corrected worker-snapshot fixture passed 23 normal plus 23 race roots with
unchanged production. It is separate from the runtime bulk issue. Full
integrated correctness and physical memory/performance qualification remain
outstanding.

## Evidence and reproduction entry points

All paths below are local investigation artifacts, not release dependencies.
Preserve failed attempts; do not overwrite a frozen candidate or silently
promote a busy-host diagnostic as a statistical baseline.

- Latest four-case artifact root:
  /Volumes/data2/urnetwork/temp/native-window-retry-loaded-only-r1/run
- Fast64 raw report: fast64.stdout, SHA
  2dc8bcfd6b87e029757afd40d8ee553b4a9a5f821291c927bf1932fb8ce1d8c2.
- Fast32 raw report: fast32.stdout, SHA
  ee49035dd741ae8fb9b8b02b5879aa14f213508a09f8e90ef0af0cd1ca5c820f.
- Four-case closure: ../CLOSED-HANDOFF.json, SHA
  402dfd989bb0c3a62f10c988b2c7ae618e0072c13f4a6e800eed16e0f129c7c3.
  All cases ran once; source checks, process joins and image cleanup passed.
- Actual reused binary: native-window-retry-common-qualification-r2/run/bin/perfvar-normal.test
  under the same Data2 temp parent; SHA
  88f4a8800f2dc170649e3e44f23fb357c1661b817ad6ea0d2b9935d44ac931af.
- Current fixed/common source:
  /Volumes/data/urnetwork/temp/native-window-retry-common-r1/READY.json,
  SHA c537b03f9377f26d99f915d5d6b4e8a770ebc4f1717155d72d9ebaea8a658afb.
- Native retry source and old/fixed qualification:
  /Volumes/data2/urnetwork/temp/native-window-update-recovery-r1/READY.json,
  SHA 01bd26825efaa58deb40be2b195aa4bb539d5309ac72019f498a97d945ea6550;
  owner-r1/CLOSED-HANDOFF.json, SHA
  83e3c07a36efb40838f615325fa3238aa70e6203d2c18d6910b25ed673849b61.
- Historical near-300 run:
  /Volumes/data2/urnetwork/temp/aggregate53-fresh-synthetic-20261110/runtime-plan/CLOSED-HANDOFF.json.

Exact workload roots in the existing PERFVAR synthetic fixture package:

    TestFullTunH1SyntheticFastProductionClient32Desktop128FiveWindows
    TestFullTunH1SyntheticFastProductionClient64Desktop128FiveWindows
    TestFullTunH1Sdk32ServiceRefusalRecovery100Pages
    TestFullTunH1Sdk64ServiceRefusalRecovery100Pages

The source packet records the synthetic fixture build tag and overlays. Running
the root names against an arbitrary canonical binary is not an equivalent
reproduction. The loaded-only runner reuses the attested binary and original
verifier, creates a fresh owned runtime, and preserves all four verdicts.
