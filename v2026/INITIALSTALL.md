# Initial navigation stall: peer-review brief

Updated 2026-11-06. **The stall is reproduced; a performance fix is not yet established.**

We are investigating the pause when a new website connection needs a newly discovered provider. The latest overlapping 100-page replay completed its workload but measured **6.439 seconds to first HTML byte on page index 1**, failing the existing five-second latency gate. Bounded instrumentation shows several serial setup dependencies. It does not show a multi-second candidate-offer/channel wait.

We want reviewers to identify safe ways to shorten this critical path, and deterministic tests that distinguish those improvements from provider variation or weakened admission rules.

## Scenario and relevance

The field symptom is that several pages load quickly, then a new navigation pauses for roughly 5–10 seconds. This synthetic scenario exercises one possible mechanism: destination-specific failure among resident exits followed by recovery through a new provider. It has not been causally joined to the reported iOS session.

The test is `TestPrivateInitialCohortDestinationOutageNatural100Pages` in [server/connect/perfvar/sdk_browsing_admission_test.go](../server/connect/perfvar/sdk_browsing_admission_test.go). It uses:

- Actual public SDK/device factory, native packet ingress, TUN, H1 transport, provider stack, and API/contract paths.
- The 32 MiB iOS-profile budget policy in a host simulator. This run does **not** qualify actual mobile Go memory or iOS extension footprint.
- 100 HTML requests and 400 asset requests, with 50–300 KiB total page sizes. Navigations overlap unfinished assets: four live pages, four origins, four H1 connections per origin, TLS 1.3, and 99 overlap proofs.
- A cell-edge access profile: 5 Mbit/s down, 1 Mbit/s up, 120 ms RTT, 25 ms jitter, 0.5% independent loss, and 100 ms queueing; provider egress uses the clean-LAN profile. The inner packet MTU is 1280. This is a constrained synthetic link, not a measurement of the user's Wi-Fi.
- A resident window of five quality exits plus one speed exit. Initial exits refuse the new destination while an existing CSS connection must retain its original owner and continue progressing. A healthy late provider is constructed after the first refusal and becomes discoverable through the fixture API.

All 100 navigations completed. The original CSS owner/tuple was preserved, its final 12,288-byte body verified, native calls reconciled, and teardown owners returned to zero. The five-second number is a **latency acceptance gate**, not a request timeout: crossing it records a performance failure even when the request eventually succeeds.

## Measured critical path

Times below are relative to the navigation tap, rounded to milliseconds. Stage callbacks are paired by the actual minted client source and destination. Contract-set times come from log emission and have a different observation boundary.

| Event | Time from tap | Measured span or interpretation |
|---|---:|---|
| Late provider constructed | 0.448 s | Fixture lifecycle event |
| Observational API probe sees it | 0.742 s | Visibility does not mean admission |
| Last initial origin refusal | 1.395 s | Not the processed remote-RST standing timestamp |
| Actual quality query begins | 1.476 s | 81 ms after the last origin refusal |
| Query returns the late provider | 2.253 s | Query: **776 ms** |
| New identity mint completes | 2.412 s | Mint: **159 ms** |
| Candidate offered and consumed | 2.412 s | Offer: **2 microseconds** |
| Mandatory provide registration commits | 3.191 s | Registration: **779 ms** |
| Initial evaluation ping begins | 3.191 s | Optional key registration was not required |
| Initial client-role lane-0 contract set | 4.095 s | About 904 ms after ping-stage entry |
| Client admitted | 4.266 s | Evaluation/admission span: **1,075 ms** |
| Client-role data-lane-7 contract set | 5.164 s | About **898 ms after admission** |
| Healthy provider observes matching SYN | 5.248 s | Actual provider receive callback |
| Provider returns SYN-ACK | 5.249 s | About 1 ms after SYN reception |
| First HTML byte | 6.439 s | Existing five-second gate fails |

The offer/consume boundary was fast. The measured delay is distributed among discovery, identity/registration work, evaluation, and a later data-lane contract dependency. API visibility at 0.742 s is insufficient evidence of a discovery timer bug: this full-window recovery path requires refusal evidence for the compatible offered exits, and the actual query began shortly after the last observed origin refusal.

The query and registration spans do **not** yet distinguish socket acquisition, shaped-link delay/retransmission, API queueing, or server/database execution. They should not be assigned to a backend bottleneck without those measurements.

## Evidence limits

- The stage collector recorded 13 rows with zero sampled skips/overflows. Zero counters do not prove every possible event was observed.
- The packet-boundary collector reported contention 4, frame overflow 2, unknown provider 1, and `WinnerAttributionKnown=false`. Complete HTTP-winner attribution remains unproven.
- Peer, source client, flags, complementary sender role, and logical lane 7 match the relevant contract-set class. Packet-to-`SequenceId` attribution is unavailable. Source ID alone also matches unrelated later lanes and must not be used to assign their waits to page 1.
- The receive callback's reply/encryption role is complementary to the original sender role; treating their numeric role values as identical gives an incorrect join.
- Earlier unchanged-workload results were 5.410 s and 5.563 s. The 6.439 s instrumented run is **not** a paired A/B performance regression or improvement result.
- Current measurements use the pre-checkpoint source-74 closure and private diagnostic overlays. The subsequent checkpoint includes a separately qualified SYN-cache correctness fix and upstream dependency updates. A fresh source-bound replay is required before making a claim about the synced build.

## Findings that constrain proposed fixes

**Opening-contract prewarm is already enabled.** Toggling `PrewarmOpeningContract` is not a new remedy. The question is whether the exact pending flow's contract can be prepared earlier.

**Nonzero logical lanes require real capability evidence.** `SendBuffer.createSendSequence` refuses an unadvertised nonzero lane and pins the live acknowledged lane-0 generation. Creating lane 7 during the initial ping cannot bypass this protection. A prospective backend grant is a separate ownership problem from permission to emit a nonzero-lane Pack.

**A new API object does not imply a cold HTTP pool.** `NewBringYourApi` stores context/JWT/strategy references; OOB clients share the generator's strategy-backed HTTP client pool. Closing an API object cancels its context, not the pool. The discovery fixture and SDK network space have separate strategy owners, but both were used during readiness. Actual connection reuse still needs measurement.

**The synthetic API endpoint is plain HTTP over the edge TUN.** H1 platform WSS uses TLS separately. API TLS certificate verification cannot explain these particular query/registration spans.

**Mandatory registration and authorization must remain intact.** Earlier discovery must also preserve bounded identity creation, retry backoff, healthy existing flows, and full-window admission rules. The earlier main-outage work deliberately stopped unproductive identity minting.

## Next causal test and candidate direction

The smallest proposed control uses real signed H1 contracts and the maintained logical-lane fixtures:

1. Establish lane 0 and obtain its real capability ACK; keep an existing lane-1 flow progressing.
2. Offer the exact native TCP tuple whose normal routing hashes to lane 7.
3. Hold that lane's actual backend opening grant. Prove the SYN remains blocked at this dependency while the existing flow retains its owner and progresses.
4. Release only that grant and require the actual SYN and completion callbacks.
5. In a cancellation arm, retire the exact requesting source before grant delivery. Verify once-only closure of the actual queued/taken grant and no effect on sibling lanes.

This control is implemented privately and under review, **not runtime-qualified**. Its cancellation check must observe the actual contract-close authority: a counter for opened contracts does not necessarily count a queued grant closed during cancellation.

A candidate is to overlap **one known waiting service flow's** backend contract preparation with candidate evaluation. It needs bounded paid ownership, exact flow/queue-generation handoff, cancellation, and legacy-peer handling. Creating a placeholder sequence, warming all eight lanes, duplicating the request when normal prewarm starts, or flushing an adopted grant on cancellation would be incorrect.

Even eliminating the measured 898 ms serial dependency would nominally leave about **5.54 s** in this run. That optimization alone cannot establish the five-second goal; the other serial setup spans also need investigation.

## Questions for reviewers

1. Which setup dependencies can safely overlap, and which genuinely require the preceding authenticated result?
2. Can the pending data-lane contract be prepared without early lane authorization, duplicate escrow, extra workers, or speculative identity growth?
3. What minimal HTTP/request/handler observations would separate transport scheduling from API/backend work in the 776/779 ms spans?
4. Is there a safer way to reuse already-authorized setup work while preserving per-lane ordering, legacy peers, healthy siblings, and cancellation custody?
5. Which additional deterministic counterfactual would disprove the proposed explanation before a larger implementation?

Candidates must improve the unchanged workload in paired measurements, retain the original failure accounting, and pass normal/race/cancellation/legacy controls. Real-device 32 MiB iOS-profile and 64 MiB Android-profile checks remain required. Sustained Fast.com throughput is a separate open investigation; this setup trace does not explain its bulk-speed limit.

## Code and evidence map

- [Multi-client window](ip_remote_multi_client.go): `randomEnumerateClientArgs`, `expand`, candidate evaluation/admission, and full-window recovery eligibility.
- [API generator](ip_remote_multi_client_api.go): `NewClientArgsContext`, `newClientArgsForDestinationContext`, `NewClientContext`, and mandatory provide registration.
- [Transfer](transfer.go): `createSendSequence`, `updateContractForMode`, capability checks, and opening-contract prewarm.
- [Contract-manager tests](transfer_contract_manager_test.go): `TestLogicalLanesUseIndependentContractQueueGenerations`.
- [SDK browsing fixture](../server/connect/perfvar/sdk_browsing_setup_fixture_test.go), [discovery fixture](../server/connect/perfvar/sdk_browsing_setup_discovery_test.go), [workload](../server/connect/perfvar/sdk_browsing_admission_test.go), and [profile documentation](../server/connect/perfvar/PERFVAR.md).

Private artifacts on the measurement host, under `/Volumes/data/urnetwork/`:

- `runtime-20261105/page1-original100-stages74.899g1_8f/FINAL-ORIGINAL100-STAGES-CUSTODY.json` — final custody `9984f790`; contains references to full workload, stage, boundary, and ownership reports.
- `c49-native-relocated-execution.PREP.dupk0plj/page1-stage-actual-readback.1uw11ocp/SUMMARY.json` — `b3441cc0`; `READBACK.json` — `752e8526`; provenance binds the raw input and parser.
- `runtime-20261105/page1-stage-r6-remaining74.ws8be_vw/FINAL-STAGE-FOCUSED-QUALIFICATION-CUSTODY.json` — `37f24c7e`; normal/race and deliberate-fault diagnostic controls.
- `pending-service-lane-grant.SOURCEONLY.cpuaudit/` — proposed gated contract control and overlap constraints, source-only.

The diagnostic executed through the guarded PERFVAR owner with two Go processors and private pinned modules. The private overlay is required to reproduce these stage rows; running the canonical test alone does not activate all instrumentation. Preserve the original selector, profile, workload, five-second verdict, source/module bindings, and teardown checks when reproducing it.
