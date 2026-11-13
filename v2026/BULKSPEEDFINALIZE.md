# Bulk speed finalization: current problems for peer review

Snapshot: 2026-11-09 local time / 2026-11-10 UTC. This is a problem statement,
not a release qualification or an assertion that the slowdown has one cause.

## Latest state (follow-up to the reviewed snapshot)

- Reviews1/2 have been considered in `BULKSPEEDFINALIZE-META.md`. A retained single-file shared-pump ABBA was executed and closed with exact audits. R14 failed consistently; R13 initially reached207–291Mbps but also collapsed later. Reverting the pump alone is not a demonstrated complete fix.
- The prefix/duplicate aggregate passed250 selected normal roots and252 race roots. These are not full-module or device qualifications.
- The current aggregate fixes fresh DATA progress and a real DATA-proof-repair misclassification after payload elision. Its zero-growth version passes291 selected normal roots. Genuine protocol-opening fixtures now follow NoBudget semantics while retaining ordinary verifier and real handoff checks; current race/performance/device qualification remains pending.
- The most recent measured aggregate still collapses at both budgets. Pure ACK refusal was41% at32MiB and61% at64MiB. The complete eight-process16-slot protocol-flight comparison showed no measured benefit; that tuning is not selected. An instrumented owner-loop occupancy diagnostic is compiling to localize the persistent stall.
- New ordinary measurement profiles use1280 MTU. Historical frozen comparisons remain explicitly historical. Fresh real-device32/64MiB memory/performance and the complete correctness suite remain outstanding.

The sections below retain the original snapshot and appended chronological evidence. Use the following current maps rather than the historical48-binding diff.

## What we need to finish

Recover sustained bulk throughput toward the previously measured 300+ Mbps
synthetic results and the desired 100+ Mbps real-device Fast.com performance,
while keeping fast overlapping browsing and the required memory limits.

- iOS surrogate: 32 MiB profile on the allowlisted Android performance devices.
- Normal Android: 64 MiB profile. Both runtime and DeviceLocal budgets matter.
- Physical iOS total extension footprint must ultimately stay below 50 MiB;
  there is no physical iPhone on this host, so Android cannot qualify that claim.
- Keep reliable DATA ownership, truthful ACKs, bounded memory and independent
  healthy flows. Do not solve overload by tearing down a shared resident.
- Every actual bug needs a deterministic pre-fix failure and corrected test,
  including adjacent ownership, cancellation and generation cases, per CODESTYLE.md.

The user's current direction is to retain the memory-accounting foundation from
the successful runs, then add protocol exclusions and other improvements without
reintroducing the rejected memory model. Maintain one aggregate candidate.

## Source identity: do not review only canonical main

The measured aggregate is still private and has not been merged. Canonical main
is not its source. Read these maps and their donor files when inspecting code:

```
Artifact root R = /Volumes/data/urnetwork/temp/aggregate-flow-r15-r2
Current single diagnostic aggregate product: R/aggregate-core50-ack-progress-r4/PRODUCTION.overlay.json
Prior measured/control core: R/aggregate-data-repair-class-r2/PRODUCTION.overlay.json
Current single diagnostic benchmark: R/aggregate-core50-ack-progress-r4/PERFVAR.overlay.json
Current single diagnostic controls: R/aggregate-core50-ack-progress-r4/CONTROLS.overlay.json
ACK fix selected verification: R/ack-nonprogress-pending-fix-r1/runtime-r1/FINAL.json
Full combined suite is not green; see appended full-module outcome.
Last passed normal/race subset: R/immediate-recovery-prefix-fix-r2 and R/immediate-recovery-prefix-race-r1
Unqualified flight experiment: R/aggregate-data-repair-flight16-r2
Flight controls with restored coverage: R/aggregate-data-repair-flight16-controls-r4
Historical48-binding diff only: R/BULKSPEEDFINALIZE-CANDIDATE.patch
```

The current core has50 product bindings in Connect; the archived patch below has48. The underlying SDK mobile memory
policy and provider factory are restored reference files. The reference archive
uses Connect `00d4f62b27861d40feef120393d2a1e99dbc2471`, SDK
`2c4cb7e6f34ab994c1f90ab8ca65517d15cf7ae6`, Server
`050073d01f9c240938e58d611c8fd1b021acf734`, and gVisor
`c7ad46e9deeb918307f9c1a3efe6f9245f0b566b`, plus the four exact measured
receive-accounting/head corrections recorded in the reference provenance.
The patch is against that corrected reference, not today's unrelated upstream
Server changes. Do not roll back unrelated security or billing fixes when landing.

Exact historical benchmark executable SHA-256:
`dfeea223b2c5b4c57d953609e31e1d51398720f309f6f7888d489a0ecaa22316`.
Current uninstrumented aggregate executable SHA-256:
`4b14683d9dfd0d88d8ec4f43e02ac06c73658b9456983437d27a4699909e00e3`.
The original benchmark has two explicitly inactive SDK experiment-hook calls;
it is not a release-pure SDK build. Its source binding records that limitation.

## What changed, and what was rolled back

Removed from the current aggregate:

- First-DATA reservations.
- The newer provider retained-accounting/lane-policy override.
- Early carrier-buffer retirement.
- Adaptive receive advertisement.

Restored reference mechanisms include the existing TransferMemoryBudget hierarchy,
receive hold promises, head/repair accounting, prepared DATA ownership and the SDK
mobile/provider policies. This does not mean there are no lanes anywhere: inspect
the effective old factory settings rather than assuming a lane-free provider.

Retained improvements include protocol-only budget exclusions (`NoBudget`),
reliable immediate protocol delivery, exact receipt/deduplication history,
ACK/FIFO recovery changes, native batching and retained-SYN improvements.
Immediate IP policy currently selects pure TCP ACKs; SYN/FIN/RST remain ordered.
Protocol classification validates actual content; a mixed DATA Pack stays DATA.
The pool snapshot also changed to acquire one shard at a time, avoiding holding
several packet-pool shard locks during observation.

## Comparable speed evidence

These are original-workload **whole-run Mbps**, five correlated windows per fresh
process. They are synthetic results, not new on-device Fast.com measurements.

| Source / replay | 32 MiB windows | 64 MiB windows |
| --- | --- | --- |
| Historical successful candidate | 387.7, 292.2, 360.7, 405.0, 297.7 | 357.6, 368.9, 390.8, 303.9, 406.4 |
| Exact historical executable, replayed today | 45.6, 13.7, 15.0, 17.8, 19.1 | 178.4, 162.2, 180.9, 116.6, 89.0 |
| Restored accounting + retained improvements, before history fix | 29.6, 2.8, 3.0, 3.5, 2.7 | 170.2, 145.8, 33.3, 3.0, 2.3 |
| Same aggregate + secured-history fix | 12.2, 2.5, 5.4, 3.4, 1.9 | 92.2, 59.1, 85.5, 89.5, 100.8 |

The exact old executable is slower now too. Therefore new product code cannot
explain the entire historical gap. Other large host jobs and concurrent tests
are permitted; CPU scheduling, thermal state and harness observation work are
not yet isolated causes. Keep all failures. These N=1 process comparisons are
not randomized statistical evidence and must not update a performance baseline.

However, there is a distinct new collapse signal: before the history fix, the
64 MiB aggregate's provider timeout resends were `8, 4, 3541, 3621, 1391`.
After the fix they were `0, 0, 0, 0, 0`; all five loaded-request gates passed.
This is encouraging evidence, not proof of a general throughput improvement.
The 32 MiB arm still had `2216, 3142, 2141, 2039, 2986` timeout resends and four
loaded-request failures, including uncompleted GETs. It remains the critical case.
TCP retransmissions also remain nonzero in the successful 64 MiB run; zero
Transfer timeout resends does not mean the entire packet path has no loss.

The workload uses native TUN, real SDK MultiClient/provider constructors, H1-family
transport, provider/NAT forwarding and local TLS origins. It preserves quality=5,
speed=1, production contracts and owner guards. Five 10-second windows share one
retained fixture; streams adapt from 1 to at most 8. Each window includes a
mandatory loaded small request with a 5-second guard. Origin bandwidth is 2 Gbps,
access propagation is 20 ms each way, and intentional random loss is zero.
Shaped queues can still overflow: zero configured random loss is not zero drops.
The fresh comparisons use the same workload; they do not yet cover all current
browser cancellation/protocol fidelity cases or old-provider fallback.

## A proven integration bug, now fixed

Dense hold-gap accounting did not account for separately recorded immediate
receipts. A sequence with one missing DATA original, 128 successfully delivered
protocol originals and a later DATA original treated 129 positions as missing
DATA. It retained the later original but withheld its selective ACK despite
125952 bytes of spare receive credit.

The public-path reproduction fails cleanly on the old aggregate and its identical
no-protocol-train control passes. The fix subtracts secured same-generation
immediate positions from the missing count in both committed-prefix paths:

```
real missing = ordinal distance - held positions below - secured immediate positions
```

Pending callbacks and foreign-generation history confer no credit. Counting clips
ranges to the actual interval and does not mutate replay state. Budgets and
ownership lifetimes are unchanged. The reproduction, five adjacent count controls
and the previous regression subset pass: **189 top-level tests normally and under
race detection**, 238 passes including subtests in each run.

Relevant current source and tests:

- `transfer.go`: `commitHeldPrefix`, both fast and scanned prefix paths.
- `transfer_immediate_hold.go`: `heldDataMissingBelow`.
- `transfer_immediate_hold_gap_test.go`: real reliable DATA / protocol / DATA path.
- `transfer_immediate_hold_count_test.go`: pending, generation, clipping and limits.
- Old proof: `R/immediate-hold-gap-rootcause-r3/runtime-r1/EXPECTED-FAILURE.json`.
- Positive proofs: `R/immediate-hold-gap-fix-r1/runtime-r1/FINAL.json` and
  `R/immediate-hold-gap-fix-race-r1/runtime-r1/FINAL.json`.

This fixes a real defect. It has not solved the 32 MiB bulk collapse.

## New 32 MiB diagnostic evidence

The instrumented replay measured `10.89, 5.97, 6.62, 3.98, 3.18 Mbps`, with provider
timeout resends `2045, 2463, 2259, 3233, 2133`. Windows 3–5 failed loaded-request
latency at approximately 5.86, 5.87 and 6.89 seconds. Instrumentation disqualifies
its timings from baseline comparison. It captured 96 bounded diagnostic samples.

Existing ownership limits and observed peaks:

| Item | Bytes | Interpretation |
| --- | ---: | --- |
| Shared client/root ceiling | 21810380 | about 20.8 MiB inside the 32 MiB profile |
| Ordinary RX ceiling | 2181038 | about 2.08 MiB, shared across window clients |
| Head-recovery ceiling | 2181038 | distinct recovery permission, not free ordinary RX |
| Sampled peak total root use | 2239368 | only about 2.14 MiB; substantial parent capacity unused |
| Sampled peak recovery use | 30544 | most recovery capacity also unused at sampled cuts |

The largest observed sum of the six exposed window receive-counter snapshots was:

- `PackHandoffDropCount=12980`, `PackHandoffDropByteCount=116845208`.
- `PackHandoffSaturationCount=0`, `ReceiveDecodePressureDropCount=0`.
- `ReceiveQueueDropCount=0`, acknowledged eviction count=0.
- Tentative eviction count=49, committed held count=741, commitment shortfalls=0.
- ACK handoff drops=0 and ACK route-write errors=0.

These are cumulative exposed counters, not unique dropped application bytes.
Repeated arrivals contribute again. Importantly, the owned decoder's failed
admission can call `recordReceivePackHandoffDrop` too. Thus zero mailbox saturation
and zero dedicated decode-pressure counters do **not** exonerate decoding/admission.
We need exact refusal reasons and current per-sequence ownership, not a conclusion
from a misleading aggregate counter name.

The decode path tries ordinary ownership, held-head repair and session-bound head
recovery before recording refusal. Review those predicates against actual missing
heads, ingress publication, immediate history and contract/session authority.
The opportunistic recovery-prefix proof still has its original 64-wire-position
bound and 256-entry scan bound. It has not been widened or established as a cause.

The collector explicitly reports the mobile sampler as unavailable in this
cohosted fixture. Managed ownership is observable; this is **not** a 32 MiB Go
runtime or physical-device memory qualification. Fresh device runs remain required.

## Browsing and correctness status

The latest original 32 MiB 100-page case **passes**: 100 completed HTML navigations,
99 overlap proofs, median TTFB about 1.034 seconds, maximum 3.241 seconds and zero
navigation stalls. Total case time was 222.43 seconds. Asset cancellation/overlap
and the unchanged deadlines remain enabled. This one pass does not prove all
historical or actual-device browsing stalls resolved; fresh 64 MiB/current-fidelity
and on-device qualification remain outstanding.

The full Connect main-module race run (`go test -json -race ... ./...`) has
now finished on a physical copy with selected overlays materialized, so source
readers and the compiler see the same candidate. It records **9825 pass events,
69 fail events and 16 skips**, including subtests: **67 failing top-level roots**
and two failing H1-residence subcases. Source/module checks are exact, children
joined and leases released. It is not green. The complete name list is in
`R/BULKSPEEDFINALIZE-GO-SNAPSHOT.json`; the terminal owner is
`R/restored-hold-fix-connect-all-go-r2/run/FINAL.json`.

The failures need separation into actual invariant regressions and fixtures that
still assert the removed model. They are not all classified as harmless:

- Size expectations: `TestLandingStructsMatchMergedLessTheDeferState` and
  `TestTransferPayloadOwnerSnapshotFencesAndConcurrency` still price the removed
  first-DATA owner's 16 bytes. Actual sendItem is 600 bytes rather than 616.
  Related size witnesses need checking; do not restore removed fields for them.
- Protocol admission/custody: `TestProviderTcpReplacementRetiresBlockedPureAck`,
  `TestProviderTcpCancellationRetiresBlockedPureAck`, encoded-provider ACK and
  pressure cases expect a pure ACK to be a data-byte admission waiter or a
  fully prepaid DATA owner. One pressure assertion says a full DATA budget must
  refuse another SYN. Those premises conflict with protocol exclusion, but the
  cancellation, retirement and downstream-ownership invariants must survive
  through an actual blocked boundary. Do not simply delete the tests.
- Discovery/local refusal: `TestBusyProbeRealLocalAdmissionRefusalsPreserveWrittenFlow`
  and five `TestMultiClientExpand*` cases do not observe the retained-capacity
  waiter/refusal their fixtures expect.
- SYN/collapse/persist cases include
  `TestNativeSynMetadataRefreshExactBudgetKeepsPaidHistory`, several
  `TestNativeSynRetry*` readiness boundaries and
  `TestTcpPersistNativeRefusedReopenRecoversExactHttp`. Fresh-source adoption,
  retry ownership and cancellation still require careful composed-path triage.
- Receive-decoder current-switch, established-head, fresh-opening and adjacent
  cases still expect newer physical-admission behavior. Some assertions may be
  tied to the rolled-back model. Valid progress, duplicate handling, MAC,
  identity and signed-expiry invariants remain required; do not discard those
  merely because the implementation strategy changed.
- `TestClientReceivePackHandoffDoesNotBlockUnrelatedSource` observes different
  handoff counters; check which boundary its frame actually traverses.
- `TestSendBufferCallerCancelWhileWaitingForCreationLock` reports that a canceled
  caller acquired sequence ownership. This needs direct investigation; it has
  not been explained away as protocol-budget fixture drift.
- `TestMultiClientH1ResidenceHealthAndSamePeerReplacement` fails blackhole and
  shared-congestion subcases. Healthy residence/replacement remains a real
  required behavior, not something to waive for a speed score.

All-repository correctness and complete PERFVAR/MEMSTEADY/LOWBAR remain unqualified.

## MTU clarification and required alignment

Devices use packet/TUN MTU **1280**, which must be the ordinary synthetic default.
The retained reports label clean-LAN inner MTU 1440. However, both the historical
and current SDK native application TUN constructors call
`resolvedFullTunApplicationMtu`, which clamps that to `DefaultMtu=1280`.
Consequently that profile label alone does not prove the measured app used 1440,
and MTU has not been established as the cause of the speed gap.

There was a real fixture inconsistency: generic direct-calibration TUNs used the
profile value directly while the SDK application helper clamped to 1280. Ordinary
profiles contained 1440/1400, and cell-edge/focused profiles subtracted an 80-byte
allowance. The next fixture revision now sets ordinary inner/TUN profiles to
`clientconnect.DefaultMtu` (1280), including focused outer-MTU profiles. Physical
outer-link MTUs remain independent. Three new tests pin registered profile values,
real direct/SDK constructor parity and independence from outer-link MTU. All three
fail against the old definitions and pass after normalization. The complete
15-test adjacent control group passes after fixing an old resource test that
incorrectly expected a 1200-byte QUIC UDP payload to exceed the 1280-byte IP MTU;
its packet must include IP/UDP headers when comparing sizes. The three new
regressions fail cleanly on the old profile definitions. Receipt:
`R/mtu1280-alignment-r2/actual-r1/FINAL.json`.

Keep explicit historical and boundary witnesses labeled separately; do not rewrite
old measurements. New profile hashes require fresh measurements/baselines. These
normalization changes are in canonical `server/connect/perfvar` and captured in
`R/BULKSPEEDFINALIZE-MTU.patch`; they were not used for the speed table above.
No post-normalization speed improvement is claimed.

## Questions for reviewers

1. Where does the 32 MiB path lose forward progress while its shared parent has
   roughly 18 MiB spare? Identify the exact failed admission predicate and owner.
   Can its next real DATA head use the permission reserved for recovery?
2. Are advertised wire credits, retained physical-byte charges and multiple
   concurrent receiver sequences in consistent units? Could fast protocol ACKs
   now drive a sender flight that the shared RX child cannot sustain?
3. Are immediate histories accounted for everywhere else that assumes dense DATA
   positions, especially ingress-floor/prefix proofs, gap expiry and worker
   replacement? Suggest deterministic pending/secured and generation controls.
4. Is ACK-only early delivery sufficient for the IP path? Could ordered SYN/SYNACK,
   FIN or RST sit behind unrelated DATA holes? Any broader policy needs real
   lifecycle/adoption and same-tuple reuse tests, not an assertion that it is safe.
5. Which retained protocol/FIFO/batching changes could explain thousands of
   provider retries absent in the fresh exact-old-binary control? Separate real
   packet/ACK loss from stale lifetime, fairness and counter-attribution errors.
6. What small deterministic or short synthetic case would distinguish these
   causes before another full five-window/100-page run? Prefer one aggregate,
   exact refusal reasons and bounded observation over a new memory subsystem.
7. Can old budget ceilings be better balanced within the unchanged root and Go
   limits after protocol exclusion? Propose a measurement and peak-memory proof;
   do not assume spare managed credit means spare physical runtime memory.
8. What host or harness effects could explain the old binary's lower speed today?
   Account for CPU2 cohosting, other builds and observation work. A noisy N=1
   result cannot settle a small percentage regression or promote a baseline.
9. How should modern/legacy-provider compatibility be qualified? The retained
   workload reports `old_provider_fallback=false`; public providers may be older.
   Existing wire controls alone are not a full old-provider performance result.

Please distinguish proven bugs, plausible hypotheses, intentionally conservative
limits and missing measurements. For a proposed fix, name the smallest observable
root-cause test, the pre-fix failure and the expected performance/memory tradeoff.

## Evidence index

All paths below are relative to R defined above:

- `restore-300mbps-baseline-r1/REFERENCE-READY.json`: reference reconstruction.
- `restore-300mbps-replay-r1/run/FINAL.json`: exact old executable replay.
- `stable-memory-protocol-performance-r1/run/FINAL.json`: restored pre-history-fix.
- `immediate-hold-gap-performance-r1/run/FINAL.json`: corrected 32/64 replay.
- `restored-hold-rx32-diagnostic-runtime-r1/run/FULL100-REPORT.json`: browsing pass.
- `restored-hold-rx32-diagnostic-runtime-r1/run/RX32-REPORT.json`: bounded snapshots.
- `restored-hold-rx32-diagnostic-runtime-r1/run/FINAL.json`: both cases joined;
  source/module checks exact, image closed, overall result failed on bulk.
- `restored-hold-fix-connect-all-go-r2/run/FINAL.json`: full Connect race run
  completed with the failures above and exact source/module/cleanup checks.

Original successful measurement artifact:
`/Volumes/data/urnetwork/synthetic-smoke-r1/foreign-sequence-performance-r1/RESULTS-runtime-r1.json`.
Fresh real-device qualification for this restored aggregate has not occurred.


## Follow-up candidate after this review snapshot

An independent part of the older head-recovery improvement was omitted along
with the larger memory changes: the reference fallback accepts an established
current original only when it is not Head and contains neither a contract ID nor
a full proof. Actual quiet/promoted sends can be Head with a compact reference,
and a midstream contract switch can carry a full proof without Head.

The next single aggregate is `R/restored-current-head-aggregate-r1/`. It restores
only that bounded current-head shape allowance using the **existing** recovery
child and parent caps. It does not restore fresh-generation reservations, carrier
retirement, provider lane overrides or adaptive advertisement. Physical storage
still grants no proof, delivery or ACK authority; Required encryption context,
current ordinal, duplicate, ownership and ordinary verifier checks remain.
Incremental patch: `R/BULKSPEEDFINALIZE-CURRENT-HEAD.patch`. Existing public signed
wire and shape/authority controls pass in an expanded 233-test subset, normally
and under the race detector. A fresh 1280-MTU five-window replay is running;
no speed result is claimed for it.
This aggregate also includes the MTU-normalized fixture defaults.

The canceled-caller failure above has a more precise interpretation now: its
recorded snapshot still had exactly one worker and one lifetime admission. The
only failed term was a hard-coded 16 channel-slot bytes; bounded protocol delivery
adds another 16 bytes for the accepted sibling's channel. The follow-up test
compares the complete Send ownership group against the accepted sibling's actual
pre-cancellation snapshot, preserving the invariant that cancellation adds or
removes no worker, queue ownership or admission. Product cancellation was not
shown broken by that failure. Other failure groups remain under investigation.

### Latest 1280-MTU aggregate replay

The 49-binding restored-accounting/current-head candidate completed both retained five-window roots with source/module pre/post identity, joined children, released leases, and normal owned-image closure. Neither profile passed the loaded-request gate. Whole-run Mbps were **10.10, 0.32, 3.02, 2.57, 2.94** for SDK32 and **32.44, 7.98, 8.53, 4.17, 4.45** for SDK64. These are failed, noisy diagnostic measurements, not baseline qualifications. The SDK64 loaded probes in windows 2–5 took 5.13–8.35 seconds. MTU normalization and the current-head allowance are therefore insufficient.

Evidence: `/Volumes/data/urnetwork/temp/aggregate-flow-r15-r2/restored-current-head-performance-r1/run/FINAL.json`; SDK64 raw report extracted without changing its original stdout: `fast64-RAW-REPORT.json`.

A separate diagnostic build compiled successfully and is running one complete SDK32 five-window series with at most 64 scalar refused-decode shape records. It preserves the same 49 product bindings, budgets and admission decisions. The observer distinguishes contract/reference/head shape, current receive floor, generation ownership and available ordinary/recovery/root bytes. Its results must not be used for performance or memory qualification. Artifact: `current-head-rx32-observed-runtime-r1` under the same evidence root.

### Refused-decode diagnostic: delivered duplicates also need recovery

The bounded SDK32 diagnostic finished with source/module identity and cleanup checks intact. It recorded 36 distinct scalar refusal shapes; 19 were same-generation packets below the current receive floor, two were at the current floor, and two had no existing receive sequence. These are unique shape samples, not event-frequency counts. Several already-delivered retransmissions were promoted compact Heads. Ordinary RX was near its 2,181,038-byte limit while the recovery child was empty and the 21,810,380-byte shared root had substantial spare capacity.

The current recovery predicate excludes past ordinals. Full payload decoding is attempted before the normal worker's `sendDeliveredDuplicateAck` path, so RX pressure can prevent a delivered retransmission from reaching re-ACK logic. This is a causal candidate for resend amplification, not yet proof that it accounts for the full bulk slowdown. A deterministic public-wire regression is being compiled: deliver an original, drop its ACK, fill RX with a foreign reservation, retry the original, and require a repeat ACK without a second delivery. The identical no-pressure case is its control.

Evidence: `current-head-rx32-observed-runtime-r1/run/OWNED-DECODE-TRACE.json` and `FINAL.json`; regression preparation: `delivered-duplicate-pressure-rootcause-r1`. Diagnostic Mbps were 14.23, 3.04, 5.25, 2.31, 3.95, with loaded-request stalls in windows 2–5. Instrumented measurements are not baseline evidence.

The delivered-duplicate regression now reproduces deterministically on the 49-binding candidate: `TestDeliveredDuplicateRecoversLostAckUnderReceivePressure` fails with floor=1, one delivered original, RX=131072/131072 and recovery=0/131072. The matching no-pressure control passes. Both cases finish normally with exact source and released leases. Evidence: `delivered-duplicate-pressure-rootcause-r1/runtime-r1/{FINAL.json,protocol-memory-normal.stdout}`.

A private fix candidate is compiling in `delivered-duplicate-recovery-fix-r1`: let an established past ordinal obtain bounded physical custody from the existing recovery child, and avoid prepaying downstream DATA delivery for an ordinal that can only repeat an ACK. It preserves the same shared root/child caps and ordinary worker receipt checks. It is not yet correctness-, race-, or performance-qualified and is not installed in the canonical product checkout.

The first duplicate-recovery candidate did not pass the deterministic test: it required `establishedContract`, but valid no-contract peers never set that flag. The next candidate (`delivered-duplicate-recovery-fix-r2`) instead requires an existing live sequence and a past receive ordinal; ordinary RX decode already had this duplicate behavior whenever capacity was available. Unknown sequences, closed/canceled sequences, incompatible/unsafe wire shapes, NACK/ahead/control envelopes, malformed proof shapes, invalid roles and exhausted child/root capacity remain refused. New tests explicitly check those boundaries, exact ACK identity, one sender settlement and no repeat delivery. Normal controls and the full Fast5 benchmark are compiling independently in their two owned cache namespaces. No performance result exists for this fix yet.

The expanded normal run remains red: the exact-ACK pressure reproducer still fails, while its no-pressure control passes. The new recovery allocation guards pass, but an additional existing shape guard still has the obsolete blanket past-ordinal refusal premise. The reproducer's decoded envelope is canonical/safe, plaintext, nonlegacy, has no encrypted control or contract envelope, and its receive queue uses the intended lifetime RX budget. A further diagnostic (`delivered-duplicate-recovery-fix-r5`) compares direct allocation with the decoder's immutable sequence selector; this distinguishes lookup mismatch from allocation refusal. The Fast5 binary is compiled, but traffic replay is deliberately not launched while this correctness reproduction remains red. No fix is qualified.

### Second duplicate-recovery boundary: handoff classification

The selector diagnostic (`delivered-duplicate-recovery-fix-r5/runtime-r1`) showed direct recovery allocation succeeds for the exact retry and live sequence, with the configured child and uncanceled context. Inspecting `bindReceiveMemoryOwner` exposed a second gate: recovery-owned packets must match consumer/provider/opening application shapes before handoff. The public reproducer uses ordinary reliable DATA, so allocation alone cannot repair it.

The combined candidate (`delivered-duplicate-recovery-fix-r6`, 50 product bindings) adds a matching handoff allowance for an already-paid, registered past original that is neither NACK nor contract-ahead. It retains child/root limits, live-sequence binding, exact owner registration, zero loan, cancellation, and the ordinary worker's receipt/delivery checks. The two legacy blanket past-ordinal refusal assertions are replaced by the new paid-custody and refusal-boundary tests; no current/future proof test is waived. Correctness and benchmark binaries are compiling. This is still an unqualified candidate, not a measured bulk improvement.

### Delivered-duplicate recovery: normal controls pass, performance pending

The combined 50-binding restored-accounting candidate passes **239 top-level normal tests (300 including subtests), zero failures**. This includes the deterministic lost-ACK pressure case, its no-pressure control, exact ACK sequence/message identity and terminal kind, one sender settlement, no repeat application delivery, and bounded recovery/refusal guards. It fixes both physical decode admission and the subsequent handoff classification for a paid past duplicate; past ordinals also avoid unnecessary downstream DATA prepayment. Existing shared root and child byte caps are unchanged.

Evidence: `/Volumes/data/urnetwork/temp/aggregate-flow-r15-r2/delivered-duplicate-recovery-fix-r6/runtime-r1/FINAL.json` and `protocol-memory-normal.stdout`. The compiled complete production32/production64 five-window Fast5 replay is now running under `delivered-duplicate-recovery-performance-r1`; the same controls' race build is under `delivered-duplicate-recovery-race-r1`. Performance impact, full-suite correctness and device memory/performance qualification remain unproven. No baseline promotion or canonical product installation is claimed.

### Combined duplicate fix: correctness progress, bulk failure retained

The selected normal suite passes 239 top-level /300 including-subtest cases. The same race binary completed the group with one failed zero-allocation assertion in `TestNatProviderMemoryProtocolCallbacksOmitByteClaimsAndJoinClose` (two allocations while protocol callbacks were active); no data-race warning was recorded. That exact test then passed three isolated runs of the same race binary. Preserve the group failure; isolation does not make it green or prove its cause.

The complete uninstrumented SDK32 Fast5 measured **8.82, 1.91, 0, 1.41, 1.72 Mbps**, with loaded-request failures in every window. SDK64 measured **27.38, 12.28, 2.77, 34.12, 6.29 Mbps**, with an uncompleted loaded GET in window3 (9.50s). Thus repeat-ACK recovery is a deterministic correctness fix, but is not a sufficient bulk-speed remediation. No baseline update. The runtime owner is completing its final module/cleanup audit; authoritative final closure is `delivered-duplicate-recovery-performance-r1/run/FINAL.json` when present. Raw stdout/report extracts remain under the same run directory.

An adjacent deterministic test is compiling in `immediate-recovery-prefix-rootcause-r1`: a secured immediate protocol ordinal interleaved with a paid DATA prefix must not be counted as a missing predecessor by `receiveConsumerRecoveryPrefixWithLock`. Pending, unsecured and foreign-generation history must not grant the same right. This is an unproven additional hypothesis; the aggregate was not modified for it.

### Adjacent recovery-prefix defect reproduced

`TestReceiveConsumerRecoveryPrefixIncludesSecuredImmediateOrdinal` fails deterministically on the combined duplicate-fix aggregate: a paid DATA predecessor at ordinal0 plus a secured protocol receipt at ordinal1 does not authorize the otherwise-complete prefix before ordinal2. Pending, unsecured and foreign-generation controls pass. Evidence: `immediate-recovery-prefix-rootcause-r1/runtime-r1/{FINAL.json,protocol-memory-normal.stdout}` under the same artifact root.

`receiveConsumerRecoveryPrefixWithLock` only marks published DATA owners and ignores secured immediate receipt history. This differs from the corrected hold-promise calculation, which already excludes those received protocol ordinals from missing DATA. The private aggregate `immediate-recovery-prefix-fix-r1` now seeds the existing64-bit prefix mask from the bounded same-generation secured history through its atomic publication pointer, then adds paid DATA owners using the existing256-owner scan bound. No new history store, byte budget or wire credit is added. Correctness and benchmark builds are running; no performance improvement or release qualification is claimed.

The prior duplicate-recovery bulk replay's final audit is now complete: source/module pre/post exact, all children joined, leases released, owned image closed normally. Its performance failures remain retained.

### Secured protocol prefix fixed; mixed paid-prefix exclusion exposed

The new secured-protocol-prefix positive and pending/unsecured/foreign controls pass on `immediate-recovery-prefix-fix-r1`. Its broader normal run remains red on the existing `TestReceiveConsumerRecoveryPrefixAcceptsMixedExactRxAndHeadCustody`: published unloaned DATA predecessors paid in ordinary RX are ignored when mixed with predecessors paid in the recovery child. That deterministic test verifies exact root/child counts, removal and replacement of real owners, and refusal of an arbitrary sibling child.

The next aggregate (`immediate-recovery-prefix-fix-r2`) recognizes only the configured ordinary RX and recovery budgets as paid DATA-prefix sources, retaining all publication, sequence binding, loan, lifetime,64-position and256-visit fences. The same existing mixed-prefix test and negative controls are retained. Normal and performance builds are running. This is an independent prefix-custody correction, not restoration of the rejected new memory model; no byte limits or advertisements changed. The queued replay includes both Fast5 and both full100 profile roots after the normal controls pass.

### Peer-review-driven retained shared-pump ABBA

Both review files are now considered in `BULKSPEEDFINALIZE-META.md`. The retained baseline/candidate overlays were independently compared: only `connect/transfer.go` differs in content, and both executable SHA256 values match the frozen pair. The finite retrospective ABBA replay ran all four complete SDK32 Fast5 arms. Historical profile metadata is frozen; application TUN1280 is common to both. This is not qualification of the current normalized profiles.

- A1 R13: **206.79,257.15,290.76,32.32,3.74 Mbps**; provider timeout resends **8,4,4,1455,1814**.
- B1 R14: **3.15,0,4.84,1.42,0 Mbps**; resends **459,375,1000,721,466**.
- B2 R14: **6.13,1.31,1.08,1.47,2.26 Mbps**; resends **2521,1057,454,753,545**.
- A2 R13: **64.10,0.16,0.67,0.78,0.41 Mbps**; resends **1322,1038,784,574,628**.

A1 window4 had24.08ms average GC pause per collection and then entered a resend storm. Other collapsed windows had much smaller pauses or no collections, so scheduling disruption can trigger a state that outlasts the disruption; these samples do not establish the exact triggering event. Provider distributions also vary. R14 fails consistently in this cohort, but R13 is fragile too: reverting the pump alone is not established as a complete fix. No statistical baseline update. Evidence: `/Volumes/data/urnetwork/temp/aggregate-flow-r15-r2/shared-pump-retained-abba-r1/run/ABBA-SCALARS.json`, raw reports and final owner audit when closed.

The current aggregate `immediate-recovery-prefix-fix-r2` passed **250 normal roots /314 including subtests**. The race variant, including additional mask-boundary tests, passed **252 roots /316 including subtests**, zero failures and no race warning. These results validate selected boundaries, not the full module suite or bulk performance. A narrowly restored fresh-opening allocator/decoder/bind extension is compiling at `restored-fresh-opening-aggregate-r1`; it does not restore first-DATA reservations or increase root/child limits.

### Fresh-opening expansion: broader controls remain red

The restored fresh-opening aggregate compiled and its actual signed/full-proof fresh DATA progress tests passed under foreign RX pressure. The broader ReceiveConsumer selection also exposed failures in the future invalid-MAC paid-owner witness and empty-opening context/duplicate fixtures, followed by a test-helper nil dereference and pooled-root cleanup failure. These results are retained in `restored-fresh-opening-aggregate-r1/runtime-r1` and are not waived.

Inspection found that `decodeConsumerAdjacentOwner` assumes the decoder has already initialized `owner.receivePack.Pack`. The protocol-only NoBudget decode path uses the inline owner but leaves this initialization to the real Client handoff; the helper omitted that step. `fresh-opening-adjacent-diagnostic-r1` now follows actual Client initialization and adds exact verifier/ownership failure details while preserving the assertions. It is compiling; whether the remaining failures are policy-premise conflicts or behavioral defects remains unproven. In particular, acceptance of an empty control envelope by decoding alone must not be mistaken for contract verification or delivery authority. No security result is claimed from the generic failure messages.

The peer-driven ABBA has now closed normally with exact source/module audits, all children joined, image closed and leases released. Every failed arm remains retained.

### DATA proof-repair classification: real defect, not just a stale witness

The expanded controls exposed a real classification bug. `decodeReceiveHeldHeadRepair` intentionally removes DATA payload frames and retains only a new proof for the already-held original. Calling `protocolOnlyPack` on that transformed empty Pack then routes it through protocol admission and marks the logical DATA repair NoBudget. The original invalid-MAC test still correctly rejected, sent no ACK and refunded bytes, but its DATA handoff/custody witness failed because the copy took the wrong admission class. The original DATA witness is restored; it must not be changed to accept protocol classification.

The first provenance fix passed every selected custody/verifier test, but the fixed-envelope test caught an8-byte owner expansion (1024-byte owner plus8-byte pool header exceeds the unchanged1024-byte claim). That prototype is obsolete and unqualified. `aggregate-data-repair-class-r2` instead uses the existing root-owned `heldHeadRepair.owner` identity to classify a live repair, with no new Pack field or state. Classification is computed before taking the paid root lock; the shared Pack path and receive/yield paths use the same classifier. Owner sizing and byte caps remain unchanged. Its normal controls are compiling.

True empty protocol openings remain exempt from DATA byte claims. Their test helpers now initialize Pack/decoded-owner pointers like the real Client, inspect actual wire message identity before worker initialization, retain bad-MAC/delivery/ACK/refund checks, and use the real closed-sequence handoff API. Those genuine-protocol changes are separate from the DATA-repair defect.

### Protocol ACK-flight candidate: deterministic bottleneck confirmed

The public-wire counterfactual (`protocol-flight-count-rootcause-r1/runtime-r1`) fails with **two** immediate originals written, versus the proposed16, on a logical lane while DATA credit is full and receipts are held. The prototype's test also requires bounded pending admission and one-for-one terminal-receipt recycling. This confirms coupling to the divided DATA depth; it is not yet a measured proof that16 is a good default.

The current experiment (`aggregate-data-repair-flight16-r2`) adds only the16-slot protocol floor to the zero-growth aggregate. Earlier flag-based benchmark prototypes are not qualified and will not be used for a memory claim. Lane0 and zero-buffer callers preserve their prior behavior. Current1280 paired Fast5, legacy-peer performance and actual32/64MiB device memory remain mandatory before keeping the tuning. Ordered NoBudget retained lifetimes also remain an explicit bound/audit obligation; the immediate-flight experiment does not claim to solve that separately reported issue.

### Zero-growth DATA-repair aggregate: normal controls green

`aggregate-data-repair-class-r2/runtime-r1/FINAL.json` is successful: **291 top-level normal tests /355 including subtests, zero failures**, exact source bodies and released leases. This includes the original DATA proof-repair custody witness, unchanged pooled-owner envelope, fresh full-proof DATA progress, bad MAC/expiry/wrong-source rejection, true protocol-opening NoBudget/closed-handoff behavior, and prior duplicate/prefix controls.

The zero-growth fix does not add or pack a flag: it uses the existing root-locked repair-owner identity. The earlier proposed flag exceeded the packed envelope and is obsolete. The flight16 experiment is rebased onto this same50-binding core at `aggregate-data-repair-flight16-r2`; its benchmark binary compiled, and its broad/public-wire correctness binary is building. No current-core bulk, full100, race or device qualification is claimed yet.

### Flight16 control run: two test expectations corrected, product unchanged

The first flight16 broad run failed two tests: the public recycling test required a selective immediate receipt even though its offered packet was at the ordered head (where an exact terminal cumulative ACK is valid); the cold-burst test assumed protocol count4 and FIFO8 were coupled to DATA depth4. The new controls (`aggregate-data-repair-flight16-controls-r3`) require exact ACK sequence/message identity and no missing-contract request, accept only terminal cumulative or selective-immediate receipts, preserve DATA capacity4, and explicitly verify protocol capacity16 / combined FIFO20, boundary refusal, no early write/false callback, actual payload delivery and completion after real ACK. The old two-slot counterfactual remains retained. Product code and compiled benchmark are unchanged; updated controls are compiling.

This changes the count policy being experimentally evaluated, not the memory ceilings or receipt truthfulness. No flight16 throughput, race, legacy-peer or device-memory result exists yet.

### Flight16 coverage audit and paired run

The flight16 controls-r3 run passed290 roots /354 including subtests. A comparison with the core's291 roots found an overlay filename collision: the added public capacity test replaced `transfer_protocol_flight_test.go`, omitting the pre-existing `TestProtocolNoBudgetDoesNotTakeDataFlightReserve` and `TestProtocolNoBudgetLossDoesNotContractDataFlight`. The product/benchmark overlays were unaffected. `aggregate-data-repair-flight16-controls-r4` restores that original file and uses the unique target `transfer_protocol_flight_capacity_test.go`; expected selected normal coverage is292 roots. Prior artifacts are preserved, and controls-r3 must not be cited as complete selected coverage. The currently compiling race cut is also partial until the restored cases are run.

The matched performance cohort is active at `aggregate-flight16-paired-r1`: current1280 fixture binaries differ in product content only at `connect/transfer.go`, with the same restored memory/provenance fixes. Eight fresh processes are predeclared:32MiB ABBA and64MiB BAAB, each process retaining all five windows. This gives N=2 processes per arm/profile, not ten independent samples. All failures remain visible; busy-host output is diagnostic and cannot promote a baseline. Full100 and device-memory qualification remain pending.

### Completed current1280 protocol-flight cohort: no measured benefit
All eight predeclared fresh processes finished; source/module audits were exact, children joined, leases released and the owned image closed normally. The only product difference was the logical-lane protocol-flight floor. Each row retains five correlated windows; N=2 processes per arm/profile. Busy-host results are diagnostic, not baseline qualifying.
| Profile / arm | Five-window whole-run Mbps |
| --- | --- |
| 32-1-base | 63.39, 233.54, 102.71, 122.4, 20.76 |
| 32-2-flight16 | 11.46, 12.19, 5.33, 37.02, 24.06 |
| 32-3-flight16 | 10.04, 13.86, 5.91, 6.77, 50.47 |
| 32-4-base | 43.21, 9.24, 158.1, 127.64, 70.94 |
| 64-1-flight16 | 62.02, 40.69, 14.69, 16.41, 106.37 |
| 64-2-base | 106.53, 234.38, 275.46, 271.05, 146.23 |
| 64-3-base | 204.85, 187.92, 201.0, 179.93, 232.34 |
| 64-4-flight16 | 84.61, 98.56, 147.08, 212.0, 150.21 |

Only32-3-flight16 and64-2-base passed workload/report validation. Other arms retained loaded-request failures. Increasing the floor to16 has **not demonstrated a throughput improvement** and is not selected for the aggregate. The deterministic two-slot limitation is real, but widening it alone is not a performance remedy. The unchanged-floor64MiB arm reaching234–275Mbps while its other replicate fails reinforces the need to locate the persistent stall state, rather than infer a simple bandwidth ceiling.

Evidence: `/Volumes/data/urnetwork/temp/aggregate-flow-r15-r2/aggregate-flight16-paired-r1/run/{FINAL.json,COHORT-SCALARS.json,*-RAW-REPORT.json}`. The restored-coverage control compile is running independently; it repairs a test-overlay collision, not the product. Full100, complete correctness and latest-device32/64MiB qualification remain outstanding.

### Follow-up execution and peer review

The restored flight16 control map now passes292 top-level normal tests, including the two previously omitted NoBudget tests. This repairs coverage accounting; it does not reverse the negative performance result or select flight16. Evidence: `aggregate-data-repair-flight16-controls-r4/runtime-r1/FINAL.json`.

Astra max is reviewing both peer reports against the latest core50 aggregate and the complete paired results. A private diagnostic compiled successfully and is running all five SDK32 windows: existing owner-loop SendLive snapshots expose immediate original count/limit alongside receive pressure. No byte budget, timeout, advertised credit or admission decision is changed by the observer. It is instrumented and cannot qualify throughput or device memory. The current core is also running full materialized Connect `./...` with `-race`; its result remains pending.

### Compact live diagnostic: retained-original gap with spare immediate slots

The first observer exceeded the existing64KiB output bound; that failed run is retained. The compact observer stayed within that bound and completed all five windows: **31.16,8.70,15.82,8.92,28.92Mbps**, instrumented. Runs2/5 retained loaded-request stalls5.69/5.87s.95 snapshot batches contain1936 selected send-owner rows (1911 sampled). Logical immediate limit3 was observed; saturation occurs, but is not the only stalled boundary.

The oldest consumer-uplink sample has19 originals,7 unacknowledged (2572encoded bytes), oldest5.956s, cumulative ACKhead21 unchanged4.05s, selectivehead40, and **immediate count0 / wait count0**. Thus this specific retained-original gap cannot be attributed to occupied immediate slots. The compact row does not distinguish ordinary DATA from ordered protocol originals; payload classification remains unproven. In the opposite receive direction, one window client reaches2638 lifetime handoff refusals; the snapshot does not distinguish their exact admission reason. Root usage peaks3.41MB and recovery child660KB; ordinary RX occupancy is not directly exposed by this collector, so these are not proof of free RX capacity.

All observed rows come from the existing owner-loop snapshot mechanism; max4 rows/window are selected, with total/omitted counts and sample ages explicit. Missing lanes cannot be inferred absent. Source/module guards passed, all children joined, leases released and image closed. Evidence: `aggregate-send-live-diagnostic-runtime-r2/run/{FINAL.json,RX-LIVE-REPORT.json,LIVE-PARTS.json,LIVE-SUMMARY.json}` under the artifact root. The sealed paused-worker public-wire reproduction is now compiling; it preserves all50 product bindings and real32MiB RX geometry.

### Paused-consumer refusal burst is not sufficient to reproduce the stall

The new public signed H1 synctest pair compiles and passes on the unchanged core50. With the actual32MiB geometry, the paused worker reaches first decode refusal atdistance70 with70queued, RX2168624/2181038, recovery0 and spare sharedroot. It refuses96 originals, retains48 later tails, then natural retry/ACK propagation completes1720 IP packets exactlyonce within the5s virtual test bound, with144 timeout writes and no retained originals. The unpaused control delivers the identical stream with0 retries. Cleanup joins and allclaimsrefund in both. The reported elapsed5s is the fixed observation bound, not measured recovery latency.

The actual default `ReliableLaneProvenRecovery=false` remains in place; probe/ride/promotion/gap counters are0. This falsifies sufficiency of one finite paused-consumer burst and does not justify widening recovery prefixes or changing that optional lane rule. It does not model repeated ongoing pressure or prove that every provider/shared-pump refusal is safe. Keep these regression controls and extend only when a distinct runtime boundary is identified.

Evidence: `shared-pump-paused-worker-rootcause-r1/{READY.json,runtime-r1/FINAL.json,runtime-r1/paused-worker-normal.stdout}`. Current full Connect race run has found failures in protocol-admission and native-SYN fixtures; its complete outcome is still pending. Fresh core32 full100 is executing. An initial64 fixture overlay failed compile because it copied unrelated newer fixture dependencies; a test-only geometry delta has now been ported onto the restored foundation, without product facades, and awaits compile/controls. No64 browsing or physical memory qualification is claimed.

### Actual mobile-constructor cohort and full correctness outcome

The restored old benchmark fixtures had omitted the later actual-mobile-constructor seam. Their captured mobile admission settings and model comparisons remain evidence within that host model; they do not attest execution of the mobile constructor/packet-pressure/sampler path. The earlier full10032 command's test passed but final qualification correctly failed its missing constructor receipt. That failed qualification remains retained.

`actual-mobile-core50-perf-w-r3` ports only the existing SDK64 geometry, true mobile constructor and qualified negative controls onto the restored foundation.19 scoped constructor/geometry/negative-control roots passed before traffic. All50 product bindings are unchanged; the mobile constructor really creates the idle provider/NAT then uses the maintained provider-off setters. This cohosted process still cannot qualify isolated device runtime memory.

| Actual mobile target | Five retained Fast windows, whole-runMbps | Full100 result |
| --- | --- | --- |
|32MiB|16.66,14.55,7.95,19.29,11.12|PASS:100HTML/400assets/99overlaps; controller first2.435s, maximum firstbyte3.868s|
|64MiB|18.14,9.70,28.15,56.04,65.89|PASS:100HTML/400assets/99overlaps; controller first3.628s, maximum firstbyte4.675s|

Fast32 retains loaded stalls5.32/5.14/6.88s. Fast64 retains a zero-completed mandatory probe with9.52s canceled age inwindow2 and5.006s stall inwindow3. The whole four-root cohort is FAIL, with no baseline promotion. All source/module audits are exact, children joined, leases released and the owned image closed. Full100 passes are current workload evidence, not proof that every historical browsing symptom has been causally fixed.

The main-module Connect `./... -race` run recorded9093 passing and43 failing test events (41 top-level failing roots, two subtest failures),15 skips, then hit its30-minute root-package timeout while `TestControlSync` had run21s. `TestPlatformBuildVariantsCompile` itself passed after385s. Complete correctness is unqualified; remaining tests need a full rerun with an adequate harness wall budget after repairs. Detailed failure outputs: `aggregate-data-repair-connect-all-go-r1/run/ROOT-FAILURE-OUTPUTS.json`.

Two pure-ACK retirement/cancellation failures were fixture premises: the paused fake source had no protocol-admission gate, so observing its DATA gate could not attest the real wait. The corrected two roots PASS with DATA2/protocol2/FIFO4, actual occupied protocol slots, exact blocked producer cancellation and original ownership/refund assertions. Their combined command remains FAIL because the separate paid-SYN metadata fixture still needs its53-byte early-DATA predicate correction; no broad green claim.

The exact coalescer overlap now has a deterministic RED on unchanged product code: while a captured cumulative prefix is applying and head2 is still retained, real duplicate-head or covered-SACK ingress requeues a false 'pending cumulative progress'. At actual due recovery for missing original3, each variant records one unnecessary preemption. Both helper tests also fail. Retired-identity replay and genuine advancing-prefix controls pass. This is an adjacent correctness bug, **not a proven cause of the six-second runtime stall**; the observed bad live sample had zero preemptions. A minimal independent pending-advance bit fix is compiling for normal/race controls.

The finite RX refusal burst and static cumulative21/selective40 tests are retained as passing recovery controls, not labeled root-cause reproductions. The primary stall still needs an actually failing causal test. The detailed attribution replay's first attempt never entered its test body (network-suite gate busy, exit75); it closed cleanly and is preserved. The next replay is now scheduled after that gate released. A report-only successor also restores all13 provider observations and actual upload packet-pressure counters; old seven-row or unavailable counters must not be read as zero.

Evidence: `actual-mobile-core50-perf-w-r3/run/{FINAL.json,COHORT-SCALARS.json,*-VERDICT.json}`, `protocol-admission-fixture-controls-r1/runtime-r1`, `ack-nonprogress-pending-red-r2/runtime-r1`, `ack-nonprogress-pending-fix-r1`, `provider-prefix-attribution-runtime-r{1,2}`, and `sdk-mobile-report-scope-port-r1`. Fresh physical32/64MiB MEMSTEADY/Fast qualification and all-repository correctness remain outstanding.

### Native SYN protocol-slot readiness: deterministic RED

`native-syn-protocol-readiness-red-r1` reproduces a real readiness gap on unchanged core50. Three actual pure-ACK groups occupy the source's separate protocol admission count. A cached native pure SYN is refused exactly once at that full source. Releasing the actual source then writes the queued ACKs and opens real capacity, but the retained SYN is not automatically reoffered: **NATIVE_SYN_PROTOCOL_READY_EDGE_LOST**, retries1/nativeoffers1, virtualelapsed0. No timeout was shortened or used to manufacture the failure. The independent cancellation case passes and retires its original.

Source matches the result: pure-SYN NoBudget admission bypasses paid resend-capacity waiting, and the protocol-count refusal never calls the readiness watcher. A fix is being prepared using existing readiness/reaper/generation machinery and a lazily attached protocol-capacity notifier. It must cover release-before-arm, stolen vacancy/rearm/no spin, ordinary waiter priority, cancellation, source closure and race-winner cleanup. This is a causal reroute/recovery bug; it has **not yet been tied to the recorded Fast5 stalls**. New never-admitted SYNs are a separate case from this already-credited retained SYN.

The minimal ACK pending-advance fix separately passes39 top-level normal and39 race roots (42 with subtests each), including the exact before-fix public preemption failure and genuine advancement/drain/restore controls. It is included in the single diagnostic aggregate `aggregate-core50-ack-progress-r4` (still50 product bindings; one existing source is replaced), with no claim of measured bulk improvement. The report-only provider/pressure additions compile; scoped runtime controls are running/pending.

A fresh Android diagnostic source/build lane is active for that same production graph, with matching SDK ABI headers, all3ABIs and1280MTU. It excludes private host-only SDK hooks. This is not a release or32/64MiB device-memory qualification. Exact build/source progress and hashes live under `core50-ack-native-source.SOURCEONLY.r1` and the native build lane; no physical traffic result exists yet.
