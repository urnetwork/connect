# Low bulk throughput: third peer-review brief

Date: 2026-11-03. Follow-up to [LOWBULKFLOW2.md](LOWBULKFLOW2.md) and [LOWBULKFLOW2-REVIEW1.md](LOWBULKFLOW2-REVIEW1.md).

## What we need help with

**How can we raise sustained single-exit H1 throughput above the current roughly 97 Mbps, within the memory limits, while preserving IP-bound sessions and fast browsing?**

We now have a useful causal intervention: fresh-flow affinity concentrates concurrent downloads behind one provider's retained sender. Disabling that inheritance recovers roughly 278 Mbps by using four providers. However, applying that policy generally can break signed media sessions that require subsequent connections to share an egress IP. Larger H1 chunks improve the one-exit result without changing affinity, but have not yet passed paired allocation/GC or physical-device memory qualification.

Please distinguish an established cause in the controlled fixture from a hypothesis about public Fast.com. Real-device results remain variable, and two Android64 runs timed out. A subsequent valid 11 Mbps replay contains a **9.079-second observed byte-progress plateau**, despite zero receive-refusal counters in its retained native snapshots. Sustained throughput and multi-second liveness pauses may have different causes; neither public-path cause is established.

Constraints:

- iOS-profile observed Go-runtime gate: **32 MiB**; normal Android: **64 MiB**. The shared low profile also concerns hosted-proxy per-device budgeting, not a 32 MiB limit for an entire multi-device server process. Do not replace these with the obsolete 24/28 MiB gates.
- The iOS profile is measured on Android performance devices. It does not prove an iPhone Network Extension's total physical footprint is below 50 MiB.
- Preserve reliable TCP packet custody, actual downstream ownership before acknowledgement, bounded memory, cancellation, healthy siblings, window resizing and old-provider compatibility.
- Keep the default **one-minute `AffinityMaxTime`** and distinguish ordinary inheritance from explicit app/host pins. `AffinityMaxTime <= 0` disables ordinary fresh-flow inheritance. Never move an established TCP connection between exits.
- Changing a later connection's egress IP can also break a signed session. Preserving only the established socket is insufficient.
- Application pacing is not the proposed remedy. The transports already provide their own pacing; experiments must preserve sparse-traffic and ACK latency.
- Current physical research is hosted H1. H3/DNS-alt/P2P performance work is deferred.

## Latest controlled results

Each row below is one process retaining one composed fixture across **five consecutive ten-second speed tests**. The adaptive controller starts with one stream and permits eight; this is not five independent process repetitions. Values include normal startup/ramp effects.

| Experiment | Mbps, tests 1 through 5 | Actual carrying exits | Interpretation |
| --- | --- | --- | --- |
| Earlier instrumented four-origin baseline | 278.60 / 218.83 / 280.46 / 281.10 / 281.09 | Multiple exits | The composed path can sustain much more than 100 Mbps. This is not a public-device or one-origin comparison. |
| **One origin, ordinary affinity, original H1 packing** | **296.18 / 78.43 / 65.96 / 64.87 / 77.85** | Five in test 1; exactly one in each later test | Reproduces the retained-run collapse. Different exits win later tests; it is not one consistently bad provider. |
| **One origin, `AffinityMaxTime=0`, original packing** | **269.47 / 277.95 / 278.39 / 279.01 / 279.12** | Four in every test | Direct intervention supports concentration as a limiter in this fixture. All other public reliability settings and budgets match. |
| **One origin, one-minute affinity, H1 8/8800 packing** | **334.32 / 96.18 / 95.98 / 96.01 / 97.03** | Four in test 1; one in each later test | Improves the concentrated path without changing egress policy. Retained tests still miss 100 Mbps. |

For the original one-origin process, the median of tests 2–5 is **71.90 Mbps**. It is **96.09 Mbps** with 8/8800, approximately **33.6% higher**. These processes were not a freshly randomized or counterbalanced pair; this ratio is exploratory, not a promoted PERFVAR baseline.

The original and affinity-zero processes each launched eight flows in all five tests, with all 40 origin handlers joined. The H1-eight process launched **8 / 8 / 8 / 8 / 4** flows, with all **36** handlers joined. Do not describe it as 40 requests, force eight streams retrospectively, or filter out the four-stream run. Its eight-stream retained sample intervals are also around 97–98 Mbps.

All three one-origin reports passed their declared synthetic correctness, worker/origin joins and source/custody checks. Their `qualified` field is scoped to that diagnostic fixture. It does not qualify mobile memory, a release, public throughput, or a formal baseline update.

### Fixture and source scope

- Real public SDK construction, TUN/native path, inner TCP/TLS/HTTP, provider IP processing and outer H1 are composed in one host process. Origins and providers are owned synthetic services, not Netflix or public exits.
- Access RTT is configured at **40 ms**, no configured loss, offered capacity 2 Gbit/s. There are four owned origin servers in both one-origin arms; all download URLs select origin 0. Origin authority, destination IP, SNI and connection-pool keys change together in the four-origin versus one-origin comparison.
- The client uses the 32 MiB profile's admission budgets. The whole fixture also contains providers, origins and instrumentation: its roughly 400 MiB process heap is **not** a phone memory result.
- The retained sender budget used in the component/fixture is **2 MiB per provider-side owner**, shared across that owner's flows. It is not a fresh 2 MiB allowance for every TCP flow. Signed performance contracts use 256 MiB of transfer credit; that number is not serialized contract-proof size.
- Discovery priors in these measured fixtures are zero. Missing speed rows can legitimately produce zero in production, but the prevalence of that state in the public provider pool is unknown.
- Timers, affinity, memory-charge factors and H3/mixed-route fallback stay unchanged in the H1-eight experiment.

Canonical source remains Connect `49f285f1`, SDK `eb552288`, Server `5bb50421`, Tests `a9f3cfee`, Go **1.27.3**. Experiments use pinned private source overlays; the eight-frame change is not merged. The latest physical APKs are the separately attested N5 receive-treatment build, not the H1-eight build.

## What has become clearer since the previous review

### 1. Ordinary affinity can collapse available parallel capacity

The original one-origin trace shows all validated payload in each retained test using one provider. The affinity-zero intervention preserves the workload and budgets, removes fresh inheritance, and carries traffic through four providers in every test.

The intervention uses a complete internal constructor-settings copy, changing only `AffinityMaxTime` **before** classifiers, indexes or windows are initialized. It does not use the public SDK getter/setter projection, mutate caller settings, or reset live donor/history state. Controls verified actual zero versus default 60 seconds and unchanged hidden settings.

Seven of eight fresh flows adopted ordinary affinity in each retained baseline run. The first flow can establish a new donor, after which later streams inherit it. An offered candidate is not necessarily an admitted SYN or the provider that carried the HTTP body; the analysis distinguishes those observations.

Two existing policy details matter:

- `AffinityStickyPastCap=true` intentionally exempts eligible ordinary donors from `MaxFlowsPerExit`. Merely reducing the flow cap from 16 to 2 does not guarantee distribution.
- Live performance-aware donor checks allow a donor when its advertised prior is nonpositive. The prior is stored discovery metadata, not continuously refreshed useful-ACK bandwidth. Completed histories can still influence subsequent races; it would be incorrect to say all performance scoring is disabled.

A source-only recent-bulk adoption veto has been staged using useful TCP ACK evidence, not TLS inspection or static domain classification. Its heuristic is at least 256 KiB in a current bucket spanning at least 250 ms, with useful progress less than one second old. It preserves pins and established flows, but **does not prove saturation or spare capacity** and can change the IP of a later ordinary signed-CDN connection. It is experimental, unrun and not approved as a general shipping default.

There is no authenticated, stable same-public-egress equivalence in the current discovery metadata. Provider IDs, ingress addresses, geography and diagnostics cannot safely authorize treating two providers as the same exit IP.

### 2. Per-provider retained capacity is a plausible one-exit ceiling

The independent review's local component logs show throughput scaling with retained budget and RTT, not with adding eight flows behind the same retained owner. An independently recomputed fit was approximately:

```
useful throughput ~= eta * retained budget / (RTT + extra delay)
eta ~= 0.204; extra delay ~= 2.8 ms
```

The largest checked relative fit error was about **0.64%**, rather than a literal less-than-0.5% bound. This is strong component evidence for that controlled setup, not proof that public exits use the same budget, RTT, code or efficiency.

Current send-item accounting prepays:

- modern encoding: three rounded wire-root envelopes plus owner metadata;
- legacy encoding: five such envelopes plus owner metadata.

That covers retained bytes and head/contract decode/rewrite scratch. It is an intentional recovery guarantee. A full budget must still be able to repair a head without discarding accepted originals or borrowing unaccounted memory.

The earlier provider-return custody defect is already remediated: actual post-contract chunks must reserve before originals are surrendered; transient refusal keeps the accepted suffix and order. Historical matched measurements improved from single/double digits to 215–281 Mbps. That prior fix does not eliminate the current concentrated-owner ceiling.

### 3. Larger already-ready H1 chunks help, but introduce memory questions

Original H1 physical policy allowed up to 16 descriptors but only 3072 payload bytes at opening, or 3300 bytes for an established contract-free envelope. The experiment keeps callback storage at 16 and uses **up to eight frames / 8800 payload bytes** for H1 physical chunks. Mixed, H3, P2P and unknown routes retain the conservative shared limits. No batching timer is added.

Actual body-provider counter deltas show about **2.664–2.665 frames / 2909 payload bytes per initial Pack** before, versus **7.879–7.899 frames / 8604–8624 bytes** afterward. Body providers had zero timeout rewrites in both cohorts; observed retries came from non-body probe providers.

This supports fewer physical Packs for the same originals. It does not separate CPU/context-switch savings from improved useful bytes funded by retained accounting. The reports lack actual per-item charge, serialized-proof and allocator/GC histograms.

The larger encrypted examples are approximately **9116–10224 bytes**, above the current 8192-byte pooling cutoff, and are deliberately unpooled. Sampled whole-fixture heap-inuse peaks were 427.8 MB before and 435.3 MB afterward, including all fixture services. Pool counters alone omit unpooled allocation churn. The physical iOS-profile peak is already close to 32 MiB, so a synthetic speed improvement cannot substitute for a new phone memory test.

The candidate passed **17 exact controls in three fresh normal/race/normal processes**, including original custody/order, genuine peer ACKs, full/shared budgets, sibling preservation, cancellation, encryption/legacy encoding, sparse traffic and flush/ACK fairness. Two shared-parent tests now verify an exact eight-original prefix and an exact 8+8 noncancel drain of all 16 originals; the earlier three-packet assertion failure is preserved. No cancellation or custody assertion was removed to make it pass.

## Latest physical Fast.com and memory evidence

Same allowlisted Pixel `3B161FDJG001KT`, Wi-Fi, hosted H1, provider disabled and API alt-service disabled. These are unpaired public routes with unknown deployed-provider provenance.

| N5 profile | Real Fast.com | Observed Go-runtime peak | Quiet maximum | Result |
| --- | --- | --- | --- | --- |
| iOS 32 MiB | **52 / 62 / 19 Mbps**, median 52 | **31.18 MiB** | **26.93 MiB** | Memory, 330-second quiet window and teardown passed; all three speed observations valid. |
| Android 64 MiB | **65 Mbps, then two invalid timeouts** | **46.78 MiB** | **35.68 MiB** | Individual memory/quiet/teardown gates passed; workload failed. |

The two Android64 failures expired in display-poll at roughly 90 seconds, with display `0` and empty units. They are **not zero-Mbps results**, and no three-run median is valid. The full two-cell owner failed its workload predicate despite passing the individual memory gates. Both owners and app sessions joined; no memory breach or host-observer failure was recorded.

This latest pass is Pixel/Wi-Fi evidence, not a completed two-device × Wi-Fi/cellular matrix. Galaxy `R5CX21FY6ND` remains the other allowlisted performance device. No physical iPhone footprint was measured.

### Receive pressure precedes the Android64 failures

Closed native captures establish the following, with roughly 1.1–1.3 seconds of clock uncertainty:

- During valid Android64 Fast1, current-window refusals reached **18,331 arrivals / 30,438,691 attempted bytes**. A sparse receive-leaf sample reached **10,478,864 / 10,485,760 bytes**, about 99.93% full.
- In the first roughly 20 seconds of invalid Fast2, counters reached **22,930 / 37,570,627 bytes**. They stayed unchanged through the rest of Fast2 and Fast3. Fast2 had a 9.65 MB leaf sample; Fast3's sparse leaf samples were zero.
- Android32's definite active batches showed zero sampled receive refusals; one receive-leaf sample reached about 4.23 / 5.24 MB. This is not continuous zero-loss proof, and the two profiles used different public route sets.
- The 64-profile ACK-route blocked-write count reached 146, with approximately **80 ms total cumulative wait**. This is not an incoming RTT or a seconds-long per-write stall.

Repeated cumulative counters are not additional drops; attempted bytes can include retransmissions. These observations support receive-pressure investigation, but do not establish that pressure caused the later liveness failure.

Basic Fast captures retain completed-request encoded bytes, request counts and cancellations. They omit partial/canceled response progress, endpoint/status tables and final official phase markers. Even a valid 19 Mbps run has a small completed-byte total, so those totals cannot diagnose a stalled download or exclude a UI-completion fault. Zero sampled ACK-handoff drops do not prove delivery of every inner TCP ACK.

A subsequent **same-APK Android64 CDP replay completed at 11 Mbps in 30.324 seconds**, with spinner-stopped completion and clean cleanup. The owner also passed final source/custody checks and joined. This is one explicitly instrumented public-path diagnostic, not a matched speed comparison or memory qualification. It did not reproduce the 90-second timeout.

The richer capture records 30 samples and about **22.65 MB of `dataReceived` bytes**, versus only 109,930 completed-request encoded bytes. Among the observed responses, all 78 were 2xx; no HTTP 4xx/5xx/429, TLS or connection failure was recorded, and the request failures were cancellations. This rules out interpreting that run's low completed-byte total as almost no download. It does not explain the earlier invalid runs or establish whole-browser/provider coverage. Endpoint/progress/native correlation is still being analyzed. The raw report declares its scope and coverage counters.

One useful new liveness observation: page `dataReceived` remained exactly **226,898 bytes from elapsed 4.112 through 13.191 seconds** (a **9.079-second observed plateau**), then reached 1,031,763 bytes by 14.197 seconds. Two requests remained active for much of the plateau; cancellations/replacement requests appear near its end. Request activity alone does not prove packet transfer, and these samples do not identify the delayed hop. This is not just a lower average from startup arithmetic: subsequent transfer remains around 11 Mbps.

The analyst observes zero receive-window refusal counters across this replay's retained native snapshots; exact active-clock correlation is still being finalized. This distinct run therefore does **not** repeat the earlier pressure-then-timeout sequence. It argues against treating receive saturation as a complete explanation for every observed slow run. Do not combine counters across these sessions.

The host collector passed 68 controls and a fresh backend build; it preserves the 90-second workload / 95-second child bounds and normal default command behavior. Actual native APKs and VPN-app configuration were unchanged. Default collector behavior was separately protected: an initial host rebase changed default argv, the existing test caught it, and the corrected producer now enables CDP only through an explicit flag.

## Directions worth reviewing next

### Packing must optimize whole groups and future recovery

The provider emits logical groups of at most 16 originals. Eight yields 8+8. Raising only the cap to twelve can produce **12+4**: the four-frame suffix can cross into an 8 KiB retained class, making the whole group more expensive despite an efficient twelve-frame prefix. Groups already containing eight originals cannot benefit from a twelve cap.

Initial `h1EstablishedEnvelopeAvailable` checks acknowledgement, metadata generation and remaining contract credit. It does not prove serialized full-proof size. Later `setHead(forceFullContract)` can attach the item's historical contract, even after active-contract replacement, and logical chunk limits remain pinned. A twelve-MTU body leaves only about 3 KiB below a 16 KiB minimum transport envelope; a 256 MiB credit value does not bound that proof to 3 KiB.

Three source-only feasibility controls are prepared, **uncompiled/unrun**: actual whole-group `prepareSendItem` charges for 3/8/12 splits; real historical-contract rewrite/AEAD/transport size boundaries; and credit/generation versus proof-size separation. No twelve-frame production change is justified yet.

### Reduce genuine scratch, not accounting by assertion

A wire-preserving `setHead` rewrite looks feasible: scan borrowed protobuf fields, then emit directly into one replacement wire allocation, including legacy nested-length rebuilding and full-contract insertion. That could remove decoded payload copies and the legacy intermediate Pack serialization.

It is **not a one-total-root operation**. Old retained/carrier roots must remain immutable while the new root exists. Encrypted output and unknown-field/duplicate-message semantics also matter. A fallback to normal protobuf decoding still needs its current scratch at a saturated budget. Do not cut 3/5 to 1, or reduce credits before proving every reachable fallback/recovery path remains funded.

The current ownership controls now pass eight roots in fresh normal/race/normal runs. They demonstrate real old/new wire-root overlap, an old carrier outliving item ACK, exact full-budget natural recovery, and cancellation preserving a sibling. Prior test-only copy-mode and unsynchronized-inspection failures are preserved; they did not establish a production memory bug. A shared-scratch or wire-rewrite optimization remains unimplemented.

### Measurements and integration still required

A fresh three-block ABBA comparison is being prepared: 12 process units, six adjacent baseline/candidate contrasts, each process retaining its original five-test controller. The primary proposed statistic is the process median of tests 2–5; cold, all-five and actual adaptive stream counts remain secondary reported outcomes. Five tests inside one process must not be counted as independent process repetitions. Cumulative allocation/GC fields are proposed at the existing sampling points in both arms, without adding a new sampling cadence. This plan is not executed.

A separate next native build is being prepared to compare the N5 receive treatment against the same treatment plus qualified ACK-progress, SYN wake/fairness and SDK settings-preservation fixes. Common bounded test-side provider/build/charged-byte attribution is staged; its JVM controls and native build remain unrun. **H1-eight and experimental bulk-affinity changes are excluded from that bundle.** Do not attribute its future outcome to those policies.

Full correctness/performance qualification is incomplete. Existing focused positives do not imply all suites pass, and no new memory or throughput baseline has been promoted.

## Questions for reviewers

1. Is retained useful-payload efficiency actually the dominant concentrated-path ceiling? What minimal bounded measurement would separate item charge, contract size, allocator class, CPU service, useful-ACK timing and receiver credit?
2. Given complete logical groups of eight/sixteen originals, what packing minimizes total live reservation while preserving future full-contract recovery and the minimum transport envelope? Would another split beat 8+8 once suffix classes and callback costs are included?
3. Can the protobuf-preserving rewrite eliminate full-payload scratch safely for modern and legacy encoding? Please address duplicate submessage merging, replaced Tag semantics, unknown fields, malformed input, historical contracts, encryption and fallback at an already-full budget.
4. If scratch is amortized across items or sequences, how are concurrent rewrites, old carrier references, cancellation and unrelated owners funded without a global pool, hidden borrowing or head-of-line blocking?
5. Is there a principled fresh-flow policy that avoids bandwidth concentration while respecting later IP-bound signed-CDN connections? Current metadata cannot prove same-egress providers. Please make any required product-policy tradeoff explicit.
6. For zero advertised priors, can actual recent ACK evidence provide useful residual-capacity information without treating a lifetime peak as current spare capacity or biasing all equally short sessions differently?
7. What deterministic scenarios could distinguish both physical observations: (a) receive pressure followed by two display deadlines and zero later leaf occupancy; (b) a valid 11 Mbps run with a nine-second byte plateau and zero sampled receive refusals? Consider connection establishment/retry, lost progress/ACKs, provider failure, application behavior and browser completion separately.
8. What would make a batching gain fail the 32 MiB profile despite unchanged admission limits? Specify allocation/GC/allocator-span and native/physical measurements needed before retention.

Please propose a falsifiable mechanism, the smallest useful counterexample, and an old-versus-candidate measurement. Do not assume public provider versions, increase budgets to conceal a leak, relax custody/ACK guarantees, infer HTTP status from encrypted VPN packets, or change timers simply to make a test finish.

## Source and retained evidence map

Relevant production code:

- `transfer.go`: `sendPackChunkLimits`, `pinLogicalGroupChunkLimits`, `h1EstablishedEnvelopeAvailable`, `prepareSendItem`, `setHead`.
- `transfer_retained_budget.go`: `retainedMessageCapacity`, `retainedMemoryByteCount`, rewrite admission.
- `ip.go`: provider socket-return grouping, including the sixteen-original bound.
- `ip_remote_multi_client_affinity_time.go`: final affinity-adoption authority.
- `ip_remote_multi_client_affinity_performance.go`: useful-ACK measurement, prior/posterior and donor vetoes.
- `ip_remote_multi_client.go`: sticky caps, destination/host adoption, channel lifecycle and priors.

Private evidence root on this host: `/Volumes/data/c49-native-relocated-execution.PREP.dupk0plj`.

| Evidence | Relative path | SHA-256 prefix |
| --- | --- | --- |
| Same-origin original FIVE | `shared-epoch-fast-execution.PREP.x170r9em/02-sdk32-same-origin-retained-FIVE/FIVE-REPORT.json` | `6916e0ec2f774f57` |
| Affinity-zero FIVE | `same-origin-affinity0-execution.PREP.i4o7rer0/02-sdk32-same-origin-affinity0-retained-FIVE/FIVE-REPORT.json` | `cca972c33154b8b0` |
| H1-eight FIVE | `h1-eight-v2-execution.PREP.c55wc744/03-sdk32-same-origin-H1-eight-retained-FIVE/FIVE-REPORT.json` | `5237524c320723fa` |
| H1-eight source/tests | `h1-eight-current-sdk-v2.PREP.tht4y8_3/READY.json` | `95f1a598f228e76e` |
| Current ownership controls | `retained-invariants-v4-execution.PREP.j36ipqx_/ACTUAL-RESULTS.json` | `9b72cc3d7bd29cca` |
| Whole-group/recovery feasibility, unrun | `h1-retained-shape-proof.PREP.fjr5ywcy/READY.json` | `326040d69e40e8e8` |
| Wire-rewrite audit, source only | `sethead-wire-rewrite-audit.PREP.eiamzgim/READY.json` | `9803f2397c5f6e91` |
| Current Pixel memory/performance owner | `n5-pixel-wifi-global-docs.PREP.34k77syz/DELIVERY.json` | `d67145ff1aa9554f` |
| Closed Android64 analysis | `n5-fast64-offline-analysis.PREP.sgydyol9/REPORT.txt` | See its pinned READY/input manifest |
| Joined one-Fast CDP owner | `n5-pixel64-explicit-cdp.PREP.yc0yuv15/DELIVERY.json` | `ca423387eebd2b01` |
| New one-Fast CDP raw, 11 Mbps | `n5-pixel64-explicit-cdp.PREP.yc0yuv15/device-result/h1-ioloop-a5468ac1005a2951b9b72fd00b93af60/body/fast-1.json` | `dcd50d3141b65eb6` |

The same-origin original/H1-eight selector is `TestFullTunH1SyntheticFastSdk32SameOriginScopedPlacement` in `server/connect/perfvar`; affinity-zero uses `TestFullTunH1SyntheticFastSdk32SameOriginAffinityZeroScopedPlacement`. Reproduction must use the pinned arm overlay and current guarded performance lane, not an untracked source edit. The one-exit fixture and public Fast.com are complementary measurements, not interchangeable baselines.
