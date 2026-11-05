# Low bulk throughput: fourth peer-review brief

Date: 2026-11-04. Follows [LOWBULKFLOW3.md](LOWBULKFLOW3.md) and [its independent review](LOWBULKFLOW3-REVIEW1.md).

## Question for reviewers

**How do we get sustained hosted H1 downloads above 100 Mbps, especially through one provider, without sacrificing browsing latency, session affinity, reliability or the memory budgets?**

The latest Pixel Wi-Fi reference is **590 Mbps direct versus 22 Mbps connected through United States H1 speed-window routes**. Those starts were 7 minutes 51 seconds apart; this is evidence of available access capacity and a slow connected run, **not a controlled same-provider comparison or proof of a particular bottleneck**. The connected report contains two distinct speed-window destination IDs carrying traffic; they are not proof of two distinct physical hosts or egress IPs. The displayed22 Mbps is an aggregate, not a single-exit steady-state rate. No new provider-side performance change is qualified.

Please concentrate on the provider's actual useful bytes in flight: its retained Transfer sender, NAT TCP return retention, peer credit, ACK timing, and sharing across flows/clients. Distinguish those limits from a configured opening window or a phone's aggregate receive budget. We need a falsifiable mechanism and a small causal experiment, rather than another unconditional buffer increase.

## Constraints and experimental scope

- The iOS-profile reported Go-runtime gate is **32 MiB**, measured on the Android performance devices; normal Android is **64 MiB**. Neither is a whole multi-device server-process cap. Android measurements do not qualify an iPhone extension's total physical footprint below 50 MiB.
- Preserve reliable TCP packet custody and downstream ownership before ACK, bounded memory, cancellation, sibling flows, window resizing and older-provider compatibility. Do not turn off TCP Transfer ACKs to hide the problem.
- Preserve the default one-minute `AffinityMaxTime` and explicit session pins. Never switch an established flow's exit. Related new connections can also require a common egress IP; globally disabling affinity is not an accepted remedy.
- Current performance work is hosted **H1**. H3, DNS-alt and P2P performance are deferred. Ordinary DNS used by the H1 workload is still part of the path.
- Do not add application pacing as the proposed solution. Keep sparse traffic, ACK latency and repeated browsing responsive.
- Separate host-local synthetic throughput, public Fast.com, memory qualification and model predictions. A correct constructor graph is not proof that traffic traversed that graph.

## What the real phone currently shows

The latest pair used allowlisted Pixel `3B161FDJG001KT`, Wi-Fi, the same attested N6 app source, and Android64. The connected run verified US/H1, provider disabled, and API-alt disabled. All owned instrumentation/Chrome-forward processes joined and cleanup completed.

| Observation | Direct | Connected |
| --- | --- | --- |
| Fast.com displayed result | **590 Mbps** | **22 Mbps** |
| Valid speed interval | 8.229 s | 19.237 s |
| Fast collection valid/completed | Yes | Yes |
| Sampled Go-runtime peak | Not a memory qualification | **35.06 MiB**, below Android64 |

Important measurement details (updated after [review4](LOWBULKFLOW4-REVIEW1.md)):

- Exact N6 source confirms `ReceiveQueueCommitCount` counts committed held items above the delivery frontier on this retained-accounting path. The13,308 commits are evidence of protocol holding/disorder, not ordinary in-order receive activity. These counters remain client-wide; a fraction of *Fast.com download* Packs cannot be established from an estimated denominator or unknown remote packing.
- The review finds a ramp rather than an established plateau. A displayed Fast.com rate must not be substituted for stable path capacity. Wi-Fi/tun byte excess is a signal to investigate, not proof of duplicate Transfer payload: outer TCP retransmission, framing and traffic scope must also be separated.
- `RouteRetainedItemCount` is a historical snapshot taken at a new maximum silence, not a current outstanding-item gauge. Old correlation labels implying current/end-of-run retained items require an immutable correction addendum. It has no production decision consumer in the audited source.

- Connected CDP `dataReceived` accounting recorded **39,706,199 decoded bytes**. Completed-response-only encoded accounting was just 108,511 bytes because nine requests were canceled. That latter number is **not** download body throughput.
- All **204 window rows** recorded zero receive refusals; no receive eviction, shortfall or Pack drops were reported. This weakens the phone-RX-capacity hypothesis for this run. It does not prove continuous low occupancy: RX high-water fields were absent and periodic occupancy sampling was sparse.
- During the approximately 20-second speed bracket, the two speed-window clients counted **428 timeout writes, 1,229 selective-gap writes, 398 tail probes, one cumulative probe and 563 deferrals**, with zero recovery-write errors. Quality-window clients separately counted 25 timeouts.
- These are **client-wide counters attached to a destination label**, not destination-filtered loss counters. They aggregate sequences, inner flows and control messages. A successful write attempt is not proof of peer delivery or a unique lost packet.
- Inner TCP ACK-only packets still require Transfer ACK and can generate local recovery. Transfer protocol ACKs use the separate ACK route writer and do not generate these resend counts. The speed clients recorded 14,310 such separate ACK writes.
- The 7,173 initial speed-client frames contained 566,465 message bytes, about **79 bytes/frame**. That is compatible with reverse TCP ACK/control traffic, but does not classify it.
- The 925 recent-cumulative-progress events increment before a possible deferral; they are not 925 additional physical writes.
- No forced GC/reclaim occurred. The broader 29.779-second status interval recorded 55 GCs and 171.934 ms cumulative GC pause; 41 GCs occurred inside Fast.com. This is not a CPU profile or continuous memory qualification.

Earlier same-source public connected results included **76 Mbps** and **64 Mbps**. A 64-Mbps run did show receive pressure: a sample reached 10,471,936 / 10,485,760 retained RX bytes and 231 refusals. The later 22-Mbps run did not. Provider/session variation remains unresolved; a larger receive budget cannot be called the common cause or a measured remedy.

The latest scoped Wi-Fi memory evidence separately recorded iOS32 at **30.754 MiB** and Android64 at **34.215 MiB**, including quiet samples. That is partial physical-matrix evidence, not completion of all Wi-Fi/cellular cells or qualification of a new throughput treatment. An RX10-to-14-MiB private candidate passed 29 targeted controls, but its APK/performance experiment is held because the latest slow run did not establish RX pressure.

## What carried forward from review 3

Historical controlled five-window results remain useful, with their original limits:

- One origin and ordinary affinity concentrated later windows behind one exit. Original packing settled around 80 Mbps; the H1-eight treatment around 97 Mbps. Review 3 correctly distinguished warm plateaus from cold-provider ramps; the earlier 33.6% aggregate comparison was not a randomized causal estimate.
- Affinity disabled spread the workload across four exits and reached roughly 278 Mbps. This demonstrates available aggregate capacity in that fixture; it does not justify breaking IP-bound sessions.
- The current private **N6** build includes up-to-eight-frame H1 chunks (8,800 payload bytes) and a 9,460-byte extended pool payload class, 9,472 bytes including pool metadata. Mixed/other transports retain their conservative handling. These are private experiment bytes, not canonical-main or deployed-provider provenance.
- **Per-item retained charging is still three rounded roots plus owner metadata for modern encoding, five for legacy.** Shared per-sequence rewrite scratch and the proposed wire-preserving rewrite are **not implemented in N6**. Review 3's predicted single-provider gains from amortized scratch remain a high-priority candidate, not a measured product improvement.
- Old retained/carrier roots may outlive an item ACK. Any scratch optimization must fund real overlapping roots, encryption and legacy/fallback paths at a full budget, without hidden borrowing or blocking unrelated sequences.

## Provider budget topology: do not assume every exit is a phone profile

The source audit found materially different entrypoints. These are source facts, **not identification of the deployed US speed-window providers**.

| Role | Relevant source behavior |
| --- | --- |
| Android app using the mobile64 hierarchy, providing enabled | Shared retained hierarchy. Provider Transfer send/receive leaves are **2,876,094 / 3,834,792 bytes**. Provider transfer and NAT have separate limits under the common hierarchy; free root capacity does not erase a leaf limit. |
| Hosted `server/proxy` resident device | Explicit Low32 **client-only** construction; provider share folds into client. This is not the remote exit's NAT provider. |
| Ordinary desktop SDK defaults / current swarm defaults | Target20, **unparented** budgets, not the mobile strict hierarchy. |
| Current standalone SN miner | Derives target/process plan from host/cgroup memory and provider count, or `--max-memory`; fallback64 and floor20. It uses ordinary desktop policy unless separately selected. It is not generally the mobile64 or default20 topology. |
| `server/connect` | Relay/control-client construction, distinct from SDK exit-provider/NAT ownership. |

Additional distinctions:

- `GOMEMLIMIT` or a Docker cap alone does not call `Connect.SetMemoryBudget` or select the mobile hierarchy.
- Exact retained-byte accounting is opt-in in this path. The mobile/Low policy enables it; do not apply the three/five-root charge model to an ordinary provider without checking its actual `ResendQueueRetainedByteAccounting` setting. Unparented topology and retained-accounting mode are separate facts.
- Provider-disabled memory lending to the client exists. The inverse idle-client-to-provider lending rule does not. However provider Pack handoffs already use client-child capacity, so not all nominal client capacity is inaccessible to a provider.
- Configured opening, dynamic send ceiling, receiver advertisement, ancestor availability and actual outstanding payload are different measurements. Delivery sizing uses queue `LendableByteCount`, optional configured ceiling and peer advertisement; the configured opening is not a permanent cap.
- The host constructor's process target0 can retain a **2,621,440-byte** receive advertisement where Android startup sets process64 and permits N6's **4,362,076-byte** advertisement. Preserve startup parity, or explicitly label that difference.

## Current fixed-total provider experiment: not yet a speed result

We prepared a **constant-total** provider Transfer split change, **send:receive 3:4 → 2:1**. At the Low64 target:

| Provider Transfer leaf | Baseline | Candidate |
| --- | ---: | ---: |
| Send | 2,876,094 B | 4,473,924 B |
| Receive | 3,834,792 B | 2,236,962 B |
| Combined | 6,710,886 B | 6,710,886 B |

The NAT budget, ancestor totals, consumer settings, opening policy, affinity, timers and transport limits remain unchanged. These fixtures explicitly use the **Low** hierarchy; Low20 is not ordinary desktop/miner topology. Shrinking provider receive capacity requires an upload guardrail, not just a better download number.

**120 targeted prerequisite executions passed:** 56 SDK constructor checks, 17 baseline normal controls, and 47 further normal/race admission, upload, recovery, cancellation and peer-evidence checks. These were not full correctness-suite or performance passes.

The first baseline five-window speed arm then failed **before payload**:

- All five windows measured zero body bytes; all eight requests/window were canceled at measurement end; no origin handler ran. All workers joined. The other 15 process arms were not launched.
- Provider H1/registration reached ready, provider0 appeared in candidate offers, native SYNs reached the bridge (16 tuples, 96 accepted offers, zero bridge refusals), and the consumer observed a provider ACK/receive advertisement. Provider-side consumer-ACK evidence was absent.
- Source investigation found a specific fixture composition error: it added a NAT memory budget while retaining a custom `sdkBrowsingProviderOriginPolicy` generator. Budgeted construction permits only the built-in policy generators and returned `ErrNatMemoryPolicy`. The legacy pointer-only constructor discarded the error; the fixture still marked the provider ready. All 95 sampled NAT states showed only the local fixed 262,144-byte reservation, consistent with the remote provider never being constructed.
- A source-only repair now keeps the **built-in default policy**, uses the error-returning constructors and refuses readiness on error/nil. Existing exact-owned origin dial translation remains. The policy whitelist and production security behavior are not weakened.
- Two new causal controls are prepared: deterministic rejection of the old policy/budget combination, and actual finite TCP/TLS body delivery through provider0 with ACK, origin, budget and ownership evidence. **Both controls subsequently passed normal/race**, including4,980,736 bytes of actual TLS/HTTP payload, exact provider0 origin traffic, bidirectional ACK evidence and joined budget accounting. A fresh standalone repaired baseline then passed all five windows; see below. The candidate comparison remains unrun.

### Update: valid repaired synthetic baseline

The fresh standalone baseline completed five valid windows at **72.351 / 73.348 / 73.924 / 73.531 / 74.108 Mbps**. The primary retained-four median is **73.727576 Mbps**; a pooled rate is a separate descriptive metric. Cold window: four streams; later windows: eight each. All36 origin handlers used provider0, with one constructed window client and no provider rotation. All workload/origin/fixture/worker joins and source/guard checks passed.

Of96 coherent graph samples,92 had provider send use at least99% of its2,876,094-byte leaf; maximum2,874,897 B, minimum remaining1,197 B. NAT maximum was2,232,720 /6,710,886 B and common-root maximum5,107,120 /43,620,760 B. This strongly supports investigating send-leaf admission in this controlled Low64 profile; it is not a measured time-at-cap or a directly observed blocked-admission predicate. Effective window, Lendable/floors/sequence counts and useful ACK RTT remain absent.

A fresh fixture-amended16-process epoch is prepared, with the same predeclared ABBA strata, workload and guardrails. The failed old epoch and this standalone baseline remain outside confirmation. No candidate gain is established. Assume a good egress uplink for the next protocol experiments; do not use unknown residential capacity to dismiss the observed holding/recovery signals.

This explains the failed synthetic arm, not public Fast.com's 22 Mbps. The normal SDK production path already uses the error-returning constructor and handles refusal. The separate 100-page fixture has no such NAT budget and has actual nonnull remote providers; this exact defect does not explain its timeout.

The predeclared screen is 16 processes: download/upload × clean/loss × ABBA, five consecutive windows per process. That is **two independent process pairs per stratum**, not 80 independent samples. It is a separate synthetic protocol, not standard PERFVAR's direct-reference qualification. No direct-reference gate has been waived. Before promotion, add or justify reverse-hold and small-request latency guardrails as well as the upload guardrail; reducing the provider receive leaf can affect reverse TCP ACK holding during downloads. Any fixture amendment requires a fresh baseline. Confirmation requires fresh predeclared randomized pairs and upload/memory/recovery guardrails; no default promotion follows from the screen alone.

## Other hot paths: findings and gaps

### Reliable reverse ACK head-of-line delay

Pure inner TCP ACKs bypass collapse suppression, but still travel in reliable Transfer Packs. A later Pack can be selectively ACKed and retained behind a missing predecessor before its inner ACK reaches provider TCP. Same-flow packets share that ordering; different flows can collide on a negotiated lane. A separate negotiated lane can isolate the gap.

Provider-side ACK/control fast paths operate **after** this ordered receive boundary. They do not prove timely delivery through it. This is an intentional reliability mechanism with a plausible cost, not yet a proven production bug.

Three private native/Transfer/provider mechanism tests are written and independently reviewed: same-flow gap, colliding-flow gap, and separate-lane progress. They require actual wire identities, a physically dropped first copy, the owner's natural retry, real feedback, byte progress and final ownership release. **Runtime remains pending.** They use an emulated inner TCP peer/net.Pipe and do not measure OS cwnd or public throughput. Do not propose simply disabling TCP reliability.

### Inner TCP return window and retained NAT data

The exact N6/canonical source audit found no missing scale shift or fixed128-KiB download cap:

- SYN scale is parsed/clamped to14; SYN/SYNACK windows remain literal16-bit; subsequent peer credit uses `rawWindow << peerScale`.
- The provider's initial128-KiB acceptance floor concerns **upload acceptance**, not its download flight.
- Budgeted4-KiB socket reads are chunks; repeated reads can fill the window/budget. NAT return data remains charged until the **inner TCP ACK**, independently of Transfer ACK and provider resend ownership.
- No deterministic native → Multi → Transfer → provider download test was found proving >128-KiB negotiated flight, a held cumulative ACK and a same-ACK window enlargement under the relevant budgets. A focused extension is being prepared. Scalar negotiation tests and tiny-window reopen tests are not that integration proof.

Five128-KiB windows at200 ms would yield about26.2 Mbps, but that arithmetic is only a hypothesis. The phone capture lacks per-flow raw/scaled rwnd, inner ACK RTT, outstanding bytes and blocked reason.

The historic939–942-Mbps result is primarily a **local eight-flow upload**; capacity fixtures supplied48-MiB NAT/Transfer allowances and optional larger gVisor buffers. It is important evidence that adaptive resizing can fill a fast path, but it does not validate mobile64 **download** behavior.

### Liveness must remain separate from bulk ceilings

The latest all-initial-exits outage variant of the100-page workload timed out page1 at60 seconds. Late-provider auth read all141 bytes promptly; client setup completed, but first-ping ACK/admission did not follow. Setup completion is not carrier readiness. Retained `route_written` means local route acceptance, not physical H1 delivery; `head_set=false` refers to sender ACK-window state, not the outgoing Pack's Head bit.

A targeted exact-client trace using existing carrier/Transfer hooks is now under validation: **57 normal and57 race controls passed**. Its first replay completed all100 HTML requests and400 assets, preserving the12,288-byte CSS body on its original connection, but page1 first byte was **12.233 seconds** and a separate SYN-attribution validity check failed. Selected-client bootstrap records are present and being analyzed; final source/custody qualification is pending. This is not a solved latency regression. The trace distinguishes carrier setup, dispatch, peer receipt, contract acceptance, ACK write and sender ACK processing. Do not assume the bulk and liveness failures share a cause.

## Review requests, in priority order

1. **Provider retained-payload efficiency.** Is three/five-root per-item scratch charging still the strongest single-exit limiter? Propose a funded per-sequence high-water scratch or wire rewrite with exact ownership, concurrency, legacy and full-budget fallback invariants. Which counters and intervention would prove the gain?
2. **Actual provider topology.** Which improvements help ordinary host-derived/unparented miners versus mobile Low providers? How should idle client capacity be lent safely, given provider Pack usage and role transitions? Identify per-client fairness and generation-overlap risks.
3. **Two independent provider retention gates.** Could NAT's inner-ACK retention and Transfer's transport-ACK retention bind or feed back poorly together? Specify direction-correct ACK/flight/blocked measurements and a deterministic delayed-ACK test.
4. **Reverse ACK reliability and lanes.** Can recovery or scheduling reduce ACK head-of-line delay without violating ordered reliable IP delivery, duplicating retransmissions, changing live-flow ownership or exceeding32/64MiB? Is a lane-count change useful only for colliding flows, and what negotiation/floor costs matter?
5. **Provider receive/send rebalance.** After the fixture repair, is the constant-total2:1 experiment well targeted? What would falsify it? How should upload, small-request TTFB, legacy peers and shared-parent pressure constrain adoption?
6. **Carrier CPU/queue/outer TCP.** What evidence separates retained-budget binding from H1 serialization service, write/ACK queue delay, gVisor outer-TCP cwnd/ramp and allocator churn? Please identify one minimal measurement per proposed mechanism.
7. **Real-device discriminants.** Which bounded metadata would most efficiently connect a public Fast.com flow to the actual provider's send/NAT pressure, negotiated window and useful ACK frontier? Prefer existing hooks; no TLS interception, payload logging or inference of HTTP403 from encrypted traffic.
8. **Test realism and adjacent failures.** What is missing from five consecutive adaptive speed windows and the overlapping100-page workload? Demand actual origin traffic, carrying-provider identity and complete ownership; never let constructor/settings checks stand in for those observations.

For each recommendation, please state whether it is source evidence, measured evidence or a hypothesis; the first deterministic failure test; the smallest controlled performance intervention; and a result that would make us reject it. Preserve cold windows, failed arms, incomplete observations and nested sample structure.

## Source and evidence map

Canonical working heads remain Connect `49f285f1`, SDK `eb552288`, Server `5bb50421`, Tests `a9f3cfee`; rechecked2026-11-04. Go1.27.3. The measured N6 APK and experiments use pinned private overlays/source copies; canonical heads alone do not identify those bytes or any deployed provider.

Relevant code: `connect/transfer_retained_budget.go`, `transfer_recovery_selection.go`, `transfer.go` (dynamic sizing and wire/progress observers), `ip_provider_memory.go`, `ip_tcp_return_recovery.go`, `ip.go`; SDK `device_local_transfer_memory.go`, `device_local_memory_profile.go`, `device_local.go`; Server `connect/perfvar/sdk_browsing_setup_fixture_test.go`.

Local evidence prefixes, with no credentials or payloads required:

```text
C = /Volumes/data/c49-native-relocated-execution.PREP.dupk0plj
H = /Users/builder/Library/Caches/urnetwork-runall-owned/hosted-h1-current112.PREP.jxp9wXRI
```

| Evidence | Path under prefix | SHA256 prefix / scope |
| --- | --- | --- |
| Direct590 / connected22 results | H/n6-direct-connected-ready.PREP.ju1qn2ii/FINAL-RESULT-SEAL.json | 702de22a; WINDOW-CORRELATION.json and TIMING-GC-ADDENDUM.json alongside |
| Scoped Pixel Wi-Fi memory | H/n6-pixel-wifi2-memory.PREP.knspxrz0/PIXEL-WIFI2-QUALIFICATION.json | 9edb244e; partial matrix only |
| Exact measured N6 source | C/fast-native-n6-doc3.PREP.tv5st56z/source-copy-v8/workspace-candidate | transfer.go907bd5bd; message_pool.go9867762b |
| Provider entrypoint/budget audit | C/provider-budget-source-map.SOURCEONLY.oufcrl9i/SOURCE-MAP.md | 228d48fa; READYf929731d |
| Fixed-total plan/source | C/provider-fixed-total-peer-window.SOURCEONLY.flpxuy9a/READY.json | e32e4fc1; MEASUREMENT-PLAN/CONFIRMATION-PLAN alongside |
|120 prerequisite and control custody | H/provider-fixed-total-root-controls.PREP._nh55yhc/FIXTURE-CACHE-RELEASE.json | e3d72222; scoped controls only |
| Failed first speed arm custody | H/provider-fixed-total-screen-r6.PREP.9_3xl2k5/FIXTURE-CACHE-RELEASE.json | 277c40ce; only01 executed |
| NAT fixture repair and two unrun controls | C/provider-fixed-total-nat-policy.SOURCEONLY.rx4xvmma/READY.json | 5e5ca1e8; REPORT.txt records cause |
| Reverse ACK mechanism tests, unrun | C/native-reverse-ack-gap.SOURCEONLY.6azonijx/READY.json | 84784e39 |
| Signed bootstrap tests, unrun | C/initial-ping-signed-bootstrap-r2.SOURCEONLY.0j12w26a/READY.json | 5b65df25 |
| Targeted bootstrap source / execution | C/late-bootstrap-selected-r3.SOURCEONLY.8g_teg_f/READY.json; H/late-bootstrap-selected-r6.PREP.qvp7pj2q | 40dc35e5; normal57 release19a4ee0e; CONTROLS114-AND-BODY-HANDOFF.json alongside |

The previous briefs/reviews remain the source for historical exploratory throughput numbers. Do not promote them, or the failed zero-byte screen, into a new baseline. The repaired standalone baseline's custody is H/provider-fixed-total-nat-controls-doc4-r6.PREP.3wvdbmw4/FIXTURE-CACHE-RELEASE.json (eff18d49); its analysis preserves all5 window rates and96 graph samples.
