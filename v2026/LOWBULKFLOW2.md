# Low bulk throughput: current state and peer-review brief

Date: 2026-11-03. This follows [LOWBULKFLOW.md](LOWBULKFLOW.md) and [LOWBULKFLOW-REVIEW1.md](LOWBULKFLOW-REVIEW1.md).

## Question to resolve

Why does real-device Fast.com still measure tens of Mbps when the composed synthetic download can sustain more than 200 Mbps? Why is the first synthetic speed test substantially slower than the next four?

We want sustained bulk throughput above 100 Mbps where the underlying connection and providers support it, without sacrificing browsing latency or exceeding the **32 MiB iOS-profile / 64 MiB Android-profile** memory gates. More memory is a possible treatment, not an explanation by itself. Android runs of the iOS profile do not establish an iPhone Network Extension's total physical footprint.

**The investigation is not complete.** A provider-side custody defect has been fixed and measured. Current cold-start/provider-selection behavior and the public-device bottleneck remain unproven. A separate SDK configuration defect has now been reproduced; its repair is prepared but not performance-qualified.

## Latest completed measurements

| Workload | Result | What it establishes |
| --- | --- | --- |
| Pixel, hosted H1, Wi-Fi, iOS-profile candidate; three real Fast.com tests | **19 / 40 / 22 Mbps**, median **22 Mbps** | Actual public workload remains slow. Exits were not pinned or attested to a deployed provider revision; no direct-link control establishes a VPN-only penalty. |
| Earlier current-source composed SDK/H1 synthetic, five speed tests back to back in one process | **76.987 / 221.601 / 272.219 / 280.563 / 281.708 Mbps** | The current composed path can sustain high throughput; this process's first run is distinctly slower. This is one diagnostic process, not five independent statistical repetitions. |
| Same synthetic process, measurement-window-only rates | **76.939 / 221.538 / 272.158 / 280.494 / 281.647 Mbps** | The first-run gap is not explained by whole-run versus measurement-window averaging. |
| Latest instrumented SDK/H1 synthetic, unchanged baseline policy, five retained runs | **278.596 / 218.834 / 280.463 / 281.102 / 281.085 Mbps** | All five valid and joined; the slow first-run pattern did **not** reproduce. This is instrumented diagnostic evidence, not a qualified speed improvement. |
| Historical matched synthetic custody-fix experiment | Five downloads **215.534–281.070 Mbps** after the fix; earlier affected runs were in single/double digits | Strong evidence for the historical custody defect and its remediation. These older source revisions do not qualify the latest source or public providers. |
| Historical six-pair H1 chunk-size screen | About **80 → 100 Mbps**, median paired improvement **25.48%** | Chunk sizing is worth studying separately. The larger-chunk policy is **not shipped or memory-qualified**. |

The earlier current-source synthetic five-run median is **272.219 Mbps**, with its first run below 100 Mbps. The latest instrumented process's median is **280.463 Mbps**, with all runs above 100 Mbps. Comparing either median directly with the public Pixel median does not isolate a cause: topology, provider build, capacity, RTT, CPU, memory configuration, and origin behavior differ.

The instrumented process finished while this brief was being written. Its source/capacity/custody checks passed before publication of this document; all 40 origin requests completed their declared measurement lifecycle, with zero capture-overflow counters. The speed values above are terminal results. Analysis of the complete placement trace remains pending.

## Confirmed historical root cause: accepted provider returns lost custody

An H1 logical group could pass an eligibility calculation using a smaller, unpinned chunk, then be materialized with a larger chunk. Shared-parent reservation contention and signed-contract envelope growth create adjacent ways for the actual reservation to fail.

The old path could advance/dispose the accepted group's raw suffix before those members became retained Transfer resend items. The provider NAT had already retained its own copy and advanced the inner TCP stream. Recovery therefore fell to the NAT's inner TCP replay, producing a burst/crawl duty cycle while Transfer timeout counters could remain quiet.

The committed remediation reserves the actual post-contract chunk before surrendering the original members. Transient refusal preserves their custody and order. Raw singleton and ready-drain coalesced paths have corresponding protection. It does not disable retained accounting, increase budgets, or change retransmission timers.

Current deterministic coverage includes:

- `TestReliableH1GroupKeepsCustodyAcrossChunkPlanningPressure`
- `TestReliableGroupKeepsCustodyAcrossSharedParentReservationRace`
- `TestReliableGroupCancellationPreservesSharedParentAndSibling`
- `TestReliableGroupLargerThanRetainedLeafDrainsByPeerAcks`
- `TestReliableGroupUnavailableChunkCancelsWithoutStrandingSibling`
- `TestReliableGroupRetainsCustodyAcrossLiveBudgetShrinkAndRegrowth`
- `TestReliableGroupCanceledAdmissionContextKeepsDeferredCustody`
- `TestReliableRawSingletonKeepsCustodyAcrossSharedReservationRace`
- `TestReliableReadyCoalescerRestoresEveryOriginalInOrderAfterReservationRace`
- `TestReliableRawContractGrowthRetainsOriginalAndRollsBackOnlyDataDebit`

Historical provider recoverable-admission events fell from **1,169 to zero** in the matched custody-fix evidence. Fresh current-source synthetic testing also reports zero in the available typed provider recoverable-admission counters. This does **not** mean every failure/drop counter is zero or prove that the fixes are deployed on public exits.

## What the current synthetic five-run process shows

This section describes the **earlier slow-first-run process**, not the newly completed instrumented process. The experiment uses real SDK/Transfer/H1 paths with eight HTTP flows and four distinct origin destination IPs/SNI names. Each run uses the same fixture, and the five runs share its accumulated connection/provider state. It is not an Android or public Fast.com measurement.

Before the first run and at each run boundary, the fixture reports **five quality and one speed resident providers**, with six local/physical residents and no sampled warnings or missing evaluations. Initial native offers, return starts, and origin dials are zero: there is no hidden native/origin warmup.

Nevertheless, the carrying-provider counts grow **1 → 3 → 4 → 4 → 4**. In run one, all eight actual origin dials belong to provider ordinal 3. That provider carries approximately **101.9 MB**; the other providers carry at most about **23 KB** each. All eight HTTP streams launch and receive HTTP 200 on one origin attempt each. There is no recorded 500 ms interval with zero aggregate validated bytes.

The first eight-stream tail is about **80.34 Mbps**, versus approximately **238–308 Mbps** later. Thus the first-run result is not merely startup duration diluting an otherwise equally fast tail.

The runs begin within roughly 40 seconds, against a 60-second affinity lifetime. Spreading in later runs therefore cannot be attributed solely to expiry of that affinity lifetime.

### Counters require careful interpretation

- Available typed provider recoverable-admission/workload-failure counters are zero; one provider ordinal has no applicable row.
- Generic lifetime return-failure counters are nonzero. A failed whole-item outcome can count every member, including an accepted prefix; cancellation and teardown contribute. These are **not exact unique dropped-packet counts**.
- Native offers: **159,768 offered / 155,058 accepted / 4,710 refused**. The refusals comprise **221 pure-ACK-only** and **4,489 mixed** groups, with no data-only group refusal in that classification. This does not identify every refused original packet or prove its later recovery.
- Per-run inner TCP retransmissions: **3 / 31 / 346 / 105 / 127**; timeout counts zero. Provider timeout resends: **16 / 8 / 12 / 12 / 8**.
- Sampled provider receive/handoff/eviction/deadline/ACK-write errors and link loss/queue drops are zero.
- Total reported wire traffic is about **3.38 GB**.
- Selected-client managed/root accounting is not a Go-runtime memory gate. The whole synthetic harness's heap includes multiple providers and origins; it cannot be used as the app's 32 MiB runtime measurement.

## Candidate cold-start limiters

Two mechanisms are supported by deterministic **placement** tests, but neither is yet isolated as the cause of the actual SDK run-one bottleneck:

1. **Fresh-flow affinity inheritance.** A destination's existing provider can be reused before a fresh race. Its historical useful-ACK peak is compared with the advertised/estimated null bandwidth. A provider can have a strong historical peak while currently sharing capacity among several flows.
2. **Effective-tier selection.** Actual returned IP traffic can establish a provider as proven; unproven providers receive an effective-tier penalty. A minimum-tier candidate field can then contain only the one proven provider, even when affinity is disabled. A SYN-ACK can establish that qualification before useful bulk-byte measurements exist.

The placement controls distinguish all-six-proven versus first-only-proven cohorts, affinity enabled versus disabled, strong/null/low useful-ACK evidence, and distinct destination keys. With all six proven and affinity disabled, actual offers can cover six providers while a controlled final winner still remains the same provider. **The same final winner/mask does not prove the same selection path.** With only one proven provider, disabling affinity alone still permits singleton offers.

These controls use real Transfer clients and ordinary `SendPacket`; they do not substitute for actual native ready-only admission or a throughput comparison.

### Newly completed instrumented measurement

The instrumented SDK five-run diagnostic records bounded, opt-in evidence for:

- Original SYN tuple, ISN, and flow/cohort identity before its own SNI is learned.
- Affinity donor inputs and successful inheritance, separately from attempted adoption.
- Exact offered candidate IDs before/after performance filtering.
- Later, clock-bracketed public proven/tier/effective-tier/health readings, explicitly distinguished from the actual offered field.
- Actual decoded provider SYN/SYN-ACK receipt, owned origin dial, and per-stream HTTP provider attribution.

Its initial baseline changes **no routing policy**. Runtime factorial variants are rejected in this first packet. Initial terminal observations:

- All eight run-one constructors are unbound; run one actually carries HTTP across **four providers**, unlike the earlier one-provider run.
- Across 40 constructors, **16** successfully inherit ordinary affinity: four in each of runs 2–5, none in run one.
- There are **24** captured candidate offers: 23 fields of size four and one run-one singleton. Capture bounds were not exceeded.
- Two actual internal-configuration readings agree; the classifier setting and installed object remain enabled. Affinity lifetime is 60 seconds, with effective-tier selection and provider probes enabled.
- Forty native SYN identities, 216 provider-wire records, 108 dial records, and 106 public snapshots were captured. Candidate offers, later resident snapshots, actual wire receipt, and carrying HTTP providers must be joined carefully; their counts describe different events.

This weakens any claim that the earlier first-run bottleneck is an unavoidable budget ceiling or that affinity always makes the initial field a singleton. The first origin can legitimately receive a singleton offer while later flows race a wider field. It does **not** establish why the two processes differ: timing/instrumentation, placement randomness, and actual qualification transitions are still candidate explanations.

After analyzing that trace, the proposed comparison is affinity enabled/disabled × effective-tier selection enabled/disabled, with unchanged budgets and comparable throughput, TTFB, recovery, and memory measurements. No default change is justified yet.

The capture has explicit bounds and is diagnostic: instrumentation overhead and incomplete retry history prevent treating its speed as a new performance baseline. The existing fixture's ignored `appTun.WriteBatch` result remains an observation gap; an offered SYN-ACK is not automatically a confirmed native-TUN write.

## Newly found production SDK settings defect

During review of that diagnostic, we found that the public SDK reliability object exposes **45 of 51** Connect fields. `DeviceLocal.SetReliabilitySettings` previously converted it into a zero-based full Connect object. A read-modify-write through the public API therefore erased six unexposed fields.

The nonzero defaults lost are:

- `LightClassifier=true`, including removal of the installed classifier object.
- `UplinkStalenessMaxHold=60s`.
- `WindowOutcomeDeadline=45s`.
- `WindowOutcomeRebuildDeadline=45s`.

The other two omitted fields are `ScoredAffinityDonor` and `QuarantineReentryRamp`, currently zero by default but still meaningful when explicitly configured.

This is a production path: live routing-tier updates and the hosted proxy's evaluation-pool-size helper use public Get → copy → Set. SDK stores the resulting full override and reapplies it after reconnect. Zero outcome deadlines disable those deadline checks; losing the uplink hold cap changes the staleness-bound behavior. We have **not** measured the resulting Fast.com impact.

An actual setup-only negative test reproduced classifier-installed **1 → 0** with the old round-trip. The corrected diagnostic baseline avoids calling the setter and its positive internal-settings preservation test passes.

A separate private production repair copies the current complete Connect settings and overwrites only the 45 exposed values. Explicit public false/zero remain valid replacements; hidden custom values, offline ownership, reset, and reconnect behavior must survive. Six SDK regressions and one actual hosted-helper regression are prepared; their production-fix runtime qualification is pending. The first diagnostic's private getter hook must not be combined unchanged with this repair's device-lock path.

Independent review also noted a **pre-existing** concurrent store/apply ordering race between overlapping SDK setters/reset. It is not fixed by the narrow field-preservation repair and is not established as a bulk-performance cause.

## ACK refusal and automatic TCP recovery

We constructed a real composed test for a final advancing positive-window TCP ACK refused before native/Transfer admission:

1. Deliver actual HTTP data through the provider and real TCP stack.
2. Refuse the genuine advancing ACK without manually offering a replacement.
3. Observe the provider's actual default-RTO replay reach TUN.
4. Require fresh real TCP ACK generation, exact HTTP completion, and reuse of the same connection.

The corrected test **passes**: refusal-to-replay is approximately **999.8 ms**; refused-ACK-to-HTTP-completion is approximately **1.171 s**. No manual reoffer is used. This demonstrates an existing bounded recovery path, **not a new production fix or attribution of the phone's long plateaus**.

Several diagnostic mistakes were isolated rather than treated as network failures: a positive ACK self-matched a zero-window observer; a synctest/gVisor clock composition could not progress; and an assertion mistook the recovery worker's scheduling-clock renewal for peer progress. A separate deterministic production-worker control proves two natural retries renew that clock while actual ACK/window/retained bytes stay unchanged.

The adjacent six-case run passed five cases, including the three existing zero-window cases, but its cancellation fixture incorrectly requested a TCP delivery receipt for a generic `SimpleMessage`. Such a receipt is absent by design. A test-only repair using an actual TCP sibling and separate wire identity is prepared; its component, cancellation, and race qualifications remain pending. Do not call the full family green yet.

## Real-device memory and throughput evidence

The Pixel's completed iOS-profile candidate session includes five Wikipedia loads, three Fast.com tests, a **330-second quiet window**, and final teardown accounting:

- Go-runtime peak: **27.395 MiB**, below 32 MiB.
- Quiet-window peak: **24.574 MiB**.
- No recorded runtime-gate breach; final quiet/teardown qualification is eligible.
- Receive leaf: **5 MiB**; observed maximum use approximately **3.09 MiB**.
- **568** sampled receive-window records show no observed receive refusal, handoff refusal, or eviction.
- Wi-Fi/VPN validated, MTU 1280, thermal status 0.

During the speed window, outgoing Transfer diagnostics show **8,368 selective-gap recoveries / 19,274 initial frames**, 97 reorder-suspected events, and 28 ACK-pending preemptions, with no timeout-resend increment. These are not provider download retransmission counts and do not identify every inner TCP ACK's custody. They are a lead, not proof of a particular loss mechanism.

The subsequent Android 64 MiB cell stopped **before workload/quiet measurement** because a host ADB metadata observer exceeded its bounded lifecycle. This was not an observed app OOM. The host-only fix has now passed two deterministic old-code negative witnesses and **39 tests in three fresh normal/race/normal processes**, and the replacement backend builds. The phone retry is still pending; frozen APKs can be reused for that particular host-tool correction.

The public exit build/policy, exact incoming Pack shapes, direct-link capacity, and comparable exit RTT were not sufficiently captured. A low unpinned public result alone cannot distinguish app budget pressure, provider capacity, placement, TCP recovery, or Netflix/CDN effects. Conversely, synthetic high throughput does not rule out a live-path defect.

## Relationship to the 100-page stall work

The browsing experiment remains separate and unresolved. The latest completed 100-page diagnostic still exceeds the 5-second first-page gate: about **7.642 s** tap-relative TTFB, with all **100 HTML / 400 assets / 99 overlaps** completed. Late-provider admission-to-dial is about **0.537 s**; a late authentication POST spent about **1.927 s** waiting for its first response byte. Server-stage correlation is prepared to split handler work from network delay.

A retained-SYN wake/fairness repair passes **62 functional test executions across eight processes**, including race runs. Follow-up discovery controls initially failed because they required another SYN even though the original SYN/SYN-ACK had already completed through the new late provider. Exact components show the later duplicate correctly increments the TCP-collapse counter, without new source writes or loss of authority. Stronger early-recovery/duplicate-suppression assertions are being prepared before the next unchanged 100-page replay. No browsing-latency improvement is claimed from those functional passes alone.

## Peer-review requests

Please identify a falsifiable mechanism and its cheapest discriminating test, rather than recommending a budget/timer increase without a custody or capacity model.

1. **Cold placement:** can proven-provider tiering and historical-peak affinity explain all eight initial flows using one provider? Which actual event would distinguish the two, and what comparable policy treatment should be tried first?
2. **Shared capacity:** should a provider's historical useful-ACK peak be discounted by concurrent flows, recent delivered rate, or an independently measured capacity estimate? How can that avoid penalizing equally short/idle flows and prevent oscillation?
3. **Public versus synthetic:** what minimum provider/origin/direct-path evidence would distinguish a provider cap from local admission/refusal or TCP recovery? Prefer measurements available without TLS interception or a deployment.
4. **ACKs and mixed native groups:** do existing per-member admission/custody counters suffice to prove automatic recovery after mixed-group suffix disposal? Specify how to trace original ranges through admission, materialization, route write, peer ACK, disposal, and real upstream TCP regeneration.
5. **Chunking:** how can the historical chunk-size gain be tested at equal retained roots and runtime-memory limits, including small-packet fairness, tails, and temporary unpooled allocations?
6. **SDK repair:** are any fields, reset/reconnect transitions, lock compositions, or real production read-modify-write callers missing from the proposed coverage? Keep the separate concurrent-setter race visible.
7. **Remaining custody hypotheses:** is there a real multi-owner metadata-only credit cycle or automatic-regeneration failure? Demonstrate it with real groups/ACKs and a healthy sibling; low `Available` bytes alone are not proof.

Per [CODESTYLE.md](CODESTYLE.md), confirmed root causes need deterministic negative/positive tests, exact ownership and cleanup, and adjacent-case coverage. Do not mask a defect by extending deadlines, dropping an acceptance invariant, changing loss semantics, or claiming an offered packet was delivered.

## Evidence and reproduction map

Canonical snapshot used by the current synthetic/device fixture:

- Connect `49f285f1edd4bac4fd0273c5dfdc05a798fa4c97`
- SDK `eb552288df13f13fcdd2c7ff5bd8ff4d64a7e72c`
- Server `5bb504212e57b335f76b890a13def8b263e4bfc1`
- Tests `a9f3cfeead6ab00169706a1708f026a8f7390ed1`
- Go 1.27.3. The runtime Go2 experiments run on a noisy shared host; no baseline promotion is implied.

Retained evidence root on this host:

`/Volumes/data/c49-native-relocated-execution.PREP.dupk0plj`

Useful relative locations beneath it:

| Evidence | Location |
| --- | --- |
| Current synthetic five-run results/counters | `reviewer-bulk-independent.41ueajsf/CURRENT-R4-SDK32-FIVE-RESULT.json` |
| Cold-run analysis | `current-sdk32-five-cold-analysis.PREP.gbghwp5h` |
| Deterministic placement results | `reviewer-bulk-independent.41ueajsf/COLD-SELECTION-V3-RESULT.json` |
| Corrected scoped placement source, controls, exact command plan | `synthetic-fast-scoped-placement-v2.PREP.71hm52z0` |
| Scoped diagnostic execution | `placement-v2-execution.PREP.zlwwfpxk` |
| Latest instrumented five-run result and placement capture | `placement-v2-execution.PREP.zlwwfpxk/04-sdk32-scoped-placement-FIVE-baseline/FIVE-REPORT.json` and `PLACEMENT-REPORT.json` in that directory |
| REVIEW1 reconciliation and current custody-test map | `lowbulk-review1-reconciliation.PREP.d7t53tlu` |
| Actual final-ACK automatic recovery | `reviewer-bulk-independent.41ueajsf/LAST-ACK-CORRECTED-RTO-FIRST-RESULT.json` |
| ACK-family adjacent failure, preserved | `reviewer-bulk-independent.41ueajsf/LAST-ACK-CORRECTED-SIX-NORMAL-RESULT.json` |
| Pending sibling-receipt fixture correction | `last-ack-sibling-receipt.PREP.lo48bm6l` |
| Private production SDK repair and regression plan | `sdk-reliability-preserve.PREP.6mvi418x` |
| Physical Pixel workload/memory artifacts | `n5-pixel-wifi-memsteady.PREP.21r0xh5d` |
| Pixel offline memory/traffic analysis | `n5-pixel-memory-trace.PREP.kzzi50sb` |
| Qualified host observer backend delivery | `adb-observer-r2-backend-build-after-qualified-controls.PREP.swgokshv/HOST-OBSERVER-DELIVERY.json` |
| Completed early-handshake components | `reviewer-bulk-independent.41ueajsf/DISCOVERY-NEXT-SYN-TYPEFIX-COMPONENTS-RESULT.json` |

These private source packets contain overlays, manifests, exact selectors, and guarded invocation plans. Reproduction requires their matching source/tool/module closure and managed fixture; running a selector alone is not equivalent. Do not include credentials or raw private identities in review responses. Device tests use only Pixel `3B161FDJG001KT` and Galaxy `R5CX21FY6ND`; unrelated ADB devices are ignored.
