# Fast.com throughput and connection timeouts: fifth peer-review brief

Snapshot: 2026-11-05. Follows [LOWBULKFLOW4.md](LOWBULKFLOW4.md) and [its review](LOWBULKFLOW4-REVIEW1.md). Read this snapshot rather than treating the older briefs' measurements or unfinished experiments as current results.

## Questions for reviewers

How can we obtain sustained hosted-H1 downloads above **100 Mbps**, across repeated speed tests, while keeping browsing responsive and measured Go memory below the **32 MiB iOS-profile / 64 MiB Android-profile** gates?

Please focus on the **provider's useful flight, retained storage, ACK/recovery path and actual admission limits**, together with the receiver that feeds it. We now have three distinct problems to separate:

1. Real Fast.com runs show receive-leaf saturation despite substantial unused parent budget, plus local reverse-path gap-repair attempts. A larger leaf is an untested intervention, not an established cure.
2. A controlled 100-page recovery workload times out during TCP connect. Two smaller deterministic tests reproduce missing full-window discovery demand and missing retry when a fresh provider is published. One preliminary publication fix changes its test from FAIL to PASS; the complete fixes remain unqualified.
3. The physical speed-test harness can start another run without proving retirement of a timed-out browser target. This confounds the third Android64 result and needs its own repair.

Please propose the first deterministic failure test, smallest controlled intervention, measurements and a rejection criterion for each recommendation. Do not explain the observations away as a bad uplink: assume a good provider egress link for protocol experiments, then measure the deployed path separately.

## Scope and non-negotiable constraints

- Hosted **H1**, including supported H1+ framing. H3, DNS-alt and P2P performance are deferred. Ordinary DNS needed by an H1 workload remains relevant.
- The iOS profile is measured on the allowlisted Android phones. It does **not** establish an iPhone extension's total physical footprint below 50 MiB. Android64 and iOS32 are separate profiles, not one process-wide server limit.
- Preserve reliable TCP Transfer custody, actual downstream ownership before ACK, bounded roots/queues, resizing, cancellation and older-provider compatibility. Do not disable TCP Transfer ACKs to suppress retransmission counters.
- Preserve established-flow ownership and the original CSS connection in the browsing test. Do not switch a live flow's exit or globally poison an exit because one destination refuses it.
- Do not restore application pacing as a remedy. Keep sparse traffic and ACKs responsive.
- Distinguish configured caps, sampled live use, cumulative counters, allocation volume, Go-runtime memory and RSS. A free-pool cap is not a live Transfer flight budget.
- Every actual code fix needs a deterministic root-cause regression and adjacent-case audit under [CODESTYLE.md](CODESTYLE.md). Source review, compile success and one smoke pass do not replace complete qualification.

## Latest physical results

These are fresh retained-instrumentation runs on Pixel `3B161FDJG001KT`, Wi-Fi, hosted H1 with API-alt disabled. The accepted native APKs use frozen source epoch `807bf4e1` (Connect `b8a746af`, SDK `cf90cf6f`, Android `cf8ffe96`), not the new private receive-budget, pool-redistribution or connection-recovery candidates.

| Observation | iOS32 profile | Android64 profile |
| --- | --- | --- |
| Actual Go limit / tracked target | 33,554,432 / 33,554,432 B | 67,108,864 / 67,108,864 B |
| Fast.com displayed results | **52 / 41 / 18 Mbps**, all valid | **46 Mbps**, then two invalid timeouts |
| Three-result speed summary | Median **41 Mbps** | No valid three-run summary |
| Measured overall Go-runtime peak | **29,224,992 B / 27.87 MiB** | **40,382,496 B / 38.51 MiB** |
| Retained runtime events / sampled breaches | 159 / 0 | 210 / 0 |
| Quiet and teardown memory evidence | Eligible; owners joined | Eligible; owners joined |
| Complete workload qualification | **Passed this cell** | **Failed** on Fast2/Fast3 |

Pixel32 also completed five Wikipedia loads with median load time **618.3 ms**. This is a phone page-load metric, not the synthetic HTML-TTFB metric below.

Android64 Fast2 timed out at display polling after **89.829 s**: one observed page request, no completed/failed request callbacks, no completed-response bytes. Fast3 timed out after **89.795 s**: 12 requests, 11 completed, one connection reset and 51,115 completed-response bytes, with no valid speed result. Empty/zero displays are invalid observations, **not 0-Mbps measurements**. Completed-response-only byte counts exclude partial transfers and are not body throughput.

The two profiles used different speed-destination sets, with no identity intersection. This is **not a matched-provider 32/64 A/B**. Within Pixel32, one speed destination remained unchanged across all three runs, without adjacent counter resets. The Android64 speed series had two destinations; its quality identities churned, so those quality deltas are excluded from causal comparisons. A destination ID is not proof of a distinct physical host or egress IP.

The four-Wi-Fi actor stopped after the failed Android64 cell. Galaxy `R5CX21FY6ND` cases remain unrun. Separately, a bounded Galaxy Wi-Fi-off diagnostic found no validated cellular default and public HTTPS failed DNS resolution; Wi-Fi was restored. Do not turn unavailable cellular coverage into a pass or an API-regression claim.

The older **590-Mbps direct / 22-Mbps connected** reference in brief 4 remains historical. It was not time-matched or provider-matched to these runs. There is no fresh direct control for this new pair.

## Receive pressure: stronger evidence, still not a complete explanation

Retained primitives from the actual provider-disabled client show:

| Profile / interval | RX permission | Sampled RX use | Root use / root ceiling |
| --- | ---: | ---: | ---: |
| Pixel32 Fast1 | 2,181,038 B | 2,151,680 B (**98.7%**) | Peak 6,587,528 / 21,810,380 B |
| Pixel64 valid Fast1 | 4,362,076 B | 4,360,880 B (**99.97%**) | About 7.34 MB / 43.62 MB |
| Pixel64 stalled Fast2 | 4,362,076 B | 4,361,280 B | About 6.10 MB / 43.62 MB |

Aggregate root exhaustion is **not observed**. Source confirms that the child can refuse while the parent has spare permission; the near-cap samples and refusal counters support investigating that ceiling. They do not establish time-at-cap or identify every refusal as byte-bound rather than count-bound. A synchronized admission-reason/needed-credit trace is still missing.

For Pixel32, speed-window receive-drop brackets were **2,994 / 851 / 0**; local selective-gap write brackets were **379 / 608 / 763**; local timeout resends were zero. These are approximately 20/15/15-second brackets with roughly two seconds of lead and three to four seconds of tail. They can overlap and are not exclusive per-Fast totals. The third run has no primitive RX-occupancy sample. GC deltas were 118/97/74, with no forced GC or reclaim.

Pixel64 Fast1's broader 45-second bracket recorded **17,876** speed-window receive drops. The stalled Fast2's 95-second bracket added **10,016**. Fast3 had zero occupancy across six retained primitives, zero new speed drops and almost no speed sender progress—19 writes / 544 message bytes over its bracket. Thermal state stayed zero; maximum observed battery temperature was 26.2 C.

Receive-drop counters count refused arrivals, potentially including repeated retries of the same Pack, not unique lost packets or proven application data loss. Counter direction matters: `SelectiveGapWriteCount` increments after a **local sender's gap-repair write attempt** (`transfer_recovery_selection.go`, `transfer.go`). It is not SACK-feedback issuance, a remote provider's retransmit count, proof of delivery, or a unique-loss count. The receive drops concern incoming data; the local send-recovery counters concern the reverse side, including possible inner TCP ACK/control traffic. We have not classified every frame.

The final low-speed/timeout intervals with **no new RX drops** argue against an RX-drop-only explanation. Investigate recovery tails, missing fresh-flow progress and the browser confounder as well as immediate capacity refusal. Existing telemetry does not expose actual remote H1 Pack geometry or native-read wait duration; client `InitialFrameCount` must not be mislabeled provider download packing.

### Small controlled receive-budget experiment, prepared but unrun

Current SDK policy scales the mobile receive leaf and constrains it relative to the client pair, leaving the observed 2,181,038 / 4,362,076-byte permissions. The parent/root already benefits from provider-off borrowing; this child ceiling can still prevent using that headroom.

Private candidate `consumer-rx-fixed-total-r2.SOURCEONLY.cpuaudit`, READY `c6617451`, changes only the existing provider-off, parented RX leaf to **4 MiB at 32 / 8 MiB at 64**. It keeps root, sender, Pack, carrier and global Go limits unchanged and restores ordinary provider-on policy. This is more child permission under the same parent, not extra process memory. Advertised receive credit may also increase until the unchanged per-sequence limit binds: the wire/credit effect must be reported.

The prepared screen uses matched providers and RTT, five consecutive windows, predeclared AB/BA/AB independent process pairs at each profile in clean/loss strata. Controls cover real refusal with a free parent, role transitions, sibling budgets, ordered head-gap recovery, natural retry, upload, reverse feedback, cancellation and joins. It is source-only, not a qualified default.

At 32, the observed 27.87-MiB peak plus the maximum extra1.92-MiB RX permission is about29.79 MiB before further allocator/stack variation. That leaves only nominal headroom; permission arithmetic is not a Go-memory proof. Device validation remains mandatory. Fewer drops without improved throughput, recovery tail or latency would reject the simple RX-capacity cure.

## Provider-side mechanisms to investigate

The return path has multiple independent retention/release events:

| Owner | What it retains | Release/progress event to measure |
| --- | --- | --- |
| Provider local NAT/TCP return path | Origin bytes and inner TCP return state | Inner TCP cumulative ACK / exact recovery lifecycle |
| Provider Transfer sender | Reliable Pack payload/wire and descriptors | Transfer ACK/frontier and sender ownership release |
| Client receive/reorder path | Held Packs/roots above delivery frontier | Ordered downstream acceptance and actual ownership release |
| Reverse Transfer path | Inner TCP ACK/control packets | Their actual provider delivery, independently of Transfer ACK writes |

A Transfer ACK does not mean the browser's inner TCP has acknowledged the provider's retained origin bytes. Local reverse-path recovery can delay that inner ACK and keep NAT storage occupied after Transfer progress. Measure both gates and the feedback path before attributing the limit to one queue.

The qualified synthetic provider constructor is explicit SDK64 mobile Low H1, not every deployed miner. Its current baseline receipt has root43,620,760 B, NAT/provider parent6,710,886 B, Transfer send2,876,094 B, Transfer receive3,834,792 B, resend maximum1,398,101 B and Pack queue4,194,304 B. These fields describe an overlapping owner graph; **do not add them as disjoint RAM**. Public providers' actual constructor, build, policy and effective windows are not attested by this receipt.

Earlier controlled evidence found that send leaf near its limit while NAT/root had room. Current retained-wire work improves the cost per useful byte, but we still lack deployed-provider measurements of effective useful flight, idle/lendable credit, pending admissions, ACK age and NAT retention at the same instants.

Current H1 chunk policy allows **up to eight already-ready frames / 8×1,100 message bytes** only when every active H1 route has a known positive complete-message ceiling, with proof/AEAD bounds enforced. Unknown/legacy routes retain narrower opening/established behavior. This adds no batching wait. The tun interface's1280 minimum does not change current `connect.DefaultMtu=1100`; do not assume eight1280-byte payloads or quietly relax a complete-wire cap.

Provider questions in priority order:

1. **Useful flight per retained byte.** Is NAT's inner-ACK retention or Transfer's payload/wire/scratch retention binding first on actual public providers? Record origin read, NAT outstanding, SendSequence effective window/remaining budget, physical H1 write, Transfer ACK and inner ACK together. Count distinct roots and sharing; do not equate total allocations with live retention.
2. **Reverse ACK delivery and recovery amplification.** Can ordered reliable delivery, colliding lanes, ACK scheduling or delayed frontier updates leave NAT blocked? Instrument local reverse gap attempts and actual provider receipt separately. Preserve reliability; test head gaps, cumulative/selective ACK interactions, cancellation and persistent sibling progress.
3. **Effective peer credit and older providers.** What was actually negotiated and used, including count depth, byte ceiling, resizing support and H1 packing? Test old fixed-offer peers explicitly. A configured maximum or resize flag is not an effective-flight measurement.
4. **CPU/service rate versus memory admission.** Distinguish H1 serialization/write-queue service, outer TCP recovery, native/Go copies and allocator churn from waiting for memory or ACK. Sweep RTT with the same provider/budgets: a fixed useful-flight ceiling predicts inverse-RTT throughput, while a serialization ceiling need not. Measure effective ACK RTT rather than substituting the configured access delay. The host's131–133Mbps does not prove the remote provider can sustain that service rate under real RTT or multiple clients.
5. **Fairness and borrowing.** Can unused provider/client memory safely serve a hungry return sender or consumer receive leaf without starving reverse ACKs, uploads or other clients? Keep root/control reserves, per-client fairness and role-change ownership explicit. Do not remove leaf bounds globally.

The older939–942-Mbps result remains an important **local multi-flow upload** result under a different allowance/topology. It does not qualify this mobile/public download path.

## What has improved in controlled bulk tests

Three fresh process pairs comparing the combined guarded H1 three-frame versus eight-frame treatment produced:

| Pair | Three-frame median | Eight-frame median | Change |
| --- | ---: | ---: | ---: |
| 1 | 98.49 Mbps | 131.81 Mbps | +33.83% |
| 2 | 98.80 Mbps | 130.81 Mbps | +32.40% |
| 3 | 98.64 Mbps | 131.07 Mbps | +32.87% |

These are host synthetic comparisons, not public Fast.com rates; one candidate process overlapped unrelated compilation. The combined treatment differed in four production files (`transfer.go`, `transfer_h1_envelope.go`, `transfer_route_manager.go`, `transport.go`), so do not assign the entire gain to a single frame-count knob. Known-H1/envelope and custody guards remain essential.

The committed safe 9 KiB class has payload 9,216 B, visible cap 9,228 B and local allocator request 9,472 B. In one pool-only pair, allocation per useful byte fell13.2808→6.6170 and GC68→34; throughput131.72→132.22Mbps. Sampled whole-harness HeapInuse rose about1%, so live memory did not halve.

At the mobile 512 KiB free-pool allowance, legacy4/8KiB floors leave no retained9KiB roots. An initial32/16/28-slot redistribution improved eight-frame allocation but **regressed three-frame allocation16.78%** and was rejected for unconditional use.

Follow-up preserves the small-root burst: **64/16/14 slots** at4/8/9KiB,522,240 payload bytes under524,288. Four fresh cells, five consecutive windows each:

| Provider batch | Canonical → follow-up median Mbps | Allocated B / useful B | GC cycles |
| --- | ---: | ---: | ---: |
| Three frames | 97.9107 → 97.8572 | 9.8070 → 9.7610 | 38 → 38 |
| Eight frames | 131.5958 → 132.6447 | 14.7106 → 9.2205 | 75 → 48 |

All twenty windows passed; failed/unaccounted loaded probes were zero, with explicit owner cancellations retained. Allocation fell37.32% in the eight-frame pair without the earlier three-frame penalty. This is one process per cell; five nested windows are not five independent pairs, and the0.8% throughput change is not statistically qualified. No device or baseline promotion follows.

A deterministic64-root repeat-burst control makes the first redistribution allocate32 new roots on its second burst and the follow-up reuse all roots. Its48 normal/race controls passed. A monotone successor removes the 512 KiB + 1-byte quota cliff; two new growth/lifetime roots passed3 fresh normal processes and1race with an exact old-threshold negative. Full successor/larger-caller qualification is still pending. The larger screen uses actual desktop/server free-pool allowances and six-frame traffic to exercise the reduced8KiB class, without claiming full deployment or near-capacity stress.

## Connection timeouts: two deterministic recovery defects

### Controlled100-page failure on current code

Ordinary natural-provider H1 browsing completed100 HTML/400 asset outcomes/99 unfinished handoffs: median HTML TTFB457.84ms, p951517.56ms, maximum2176.99ms, no five-second stall. This does **not** clear the failure scenario.

The recovered all-initial-provider outage scenario failed again on Connect `4e625240`: page1 HTML timed out. Trace: connect_start at56µs, connect_done/error at**60.002s**, with no GotConn, TLS, request write or first byte. The complete root failed after80.45s including setup/cleanup. It blocks every initial provider only for new203.0.113.11:443 dials, while preserving the original12KiB CSS connection. Current topology has seven providers/six initial; that adaptation from the older six-provider fixture is explicit.

The late provider was constructed at+0.333s and API-visible at+1.119s, but had zero origin dials. Existing snapshots all precede its construction, so they cannot prove it was never admitted. Logs correlate a late-tail evaluated-child contract exchange roughly47–48s after setup, without directly identifying its first IpPing ACK or final admission. Do not confuse unlabelled Transfer control-ping ACKs with that provider's application-level evaluation ping.

### Proven smaller failures and private fixes

1. `TestDiscoveryDemandFullWindowServiceExhaustionFindsLateProvider`: actual five destination refusals, strict5/5 window, four established siblings and one idle failed owner. Old code makes **zero discovery reads/zero mint attempts**, installed5, and fails the exact `DISCOVERY_DEMAND_FULL_WINDOW_SERVICE_EXHAUSTION_DID_NOT_RELIST` marker. Ordinary demand requires spare capacity; destination-specific recovery has no qualified full-window path.
2. `TestDiscoveryDemandLatePublicationRetriesRetainedNativeSyn`: with an available slot and actual healthy publication, the original unanswered generation reaches **zero SYNs** on the new provider without another native offer. Old code fails `SYN_RETRY_FRESH_PROVIDER_PUBLICATION_WAS_NOT_CONSUMED`. Its target receipt is sampled before sibling traffic can wake anything. A preliminary publication fix passes the same root; this is one normal smoke, not final qualification.

Final private publication work replaces a synchronous all-flow scan with a coalesced callback and bounded paid-SYN index on the existing reaper. Full-window work uses destination-scoped, exact-owner reservation of an **idle** failed channel, with O(1) candidate→winner→parent-map custody, an ABA-safe owner token, existing retry authority and bounded failed-remote-tail history. It must not warn/cancel healthy CSS owners, repeatedly mint identities, or call a newly recreated local identity of the same failed remote a fresh exit.

These are native/Transfer component proofs with SDK-like strict settings, not complete SDK32 end-to-end results. The bounded publication successor is source-reviewed but unrun. Review of the full-window draft caught a send-only winner path missing the new count transfer. The source-only V2 successor corrects it and adds a native counterfactual; runtime qualification is still required. The combined100-page replay, lifecycle/race/shared32-budget guards and performance comparison remain outstanding. Neither defect is yet causally attributed to the phone's Fast2 timeout.

## Physical harness confounder: do not ignore it

After Fast2's deadline, `chrome_fast_benchmark.mjs` sends `Target.closeTarget` but cannot await its reply; final `cleanupComplete=false` means target retirement is unproven. `tests/runner/perf/physical.go` currently ignores that child failure and starts Fast3. Host-process join does **not** prove the Chrome target stopped. Receipt cleanup force-stops Chrome only after the three runs.

Therefore Fast3 may overlap a lingering Fast2 target. Actual overlap is not proven. Fast2 followed a successfully cleaned Fast1, so this does not explain away the first timeout. A deterministic actual-workflow regression is being prepared to prevent a new run when prior target retirement is unknown; teardown must preserve the original failure rather than silently converting it to success.

## Review requests and next discriminating tests

1. Rank **provider retained-flight**, **client RX leaf**, **reverse reliable ACK recovery**, and **fresh-connection recovery** as separate mechanisms. Identify the one missing measurement that best separates each pair.
2. Review the constant-root4/8MiB RX intervention and its reverse/upload/latency guards. What would distinguish improved advertised credit from merely moving congestion or increasing garbage?
3. Propose a provider-side intervention with precise ownership release and stable older-peer behavior. Require matched RTT/provider/process pairs and five consecutive speed windows; include small requests under load and complete cleanup.
4. Review bounded full-window replacement and publication-driven retained-SYN retry. Check idle/admission races, newly established owners, FIN/RST/new SYN, source cancellation, closed publishers, fairness, tail aliasing, backoff and memory-credit exhaustion.
5. Specify minimal bounded, payload-free provider diagnostics: build/policy identity, source/sequence attribution, useful flight, ACK frontier/age, NAT retention, admission refusal reason and write service time. We cannot safely infer HTTP403 or TLS/application errors from encrypted packet counters.
6. Identify missing realism beyond the local five-window and100-page tests. Preserve the browser-ownership confounder, warm/cold windows and failed arms. Lower retransmit counts or more successful constructor checks alone are not a performance win.

Full correctness is not green. Test-fixture ownership fixes and stale pool/C++-fixture repairs have separate evidence; they must not be presented as production packet-flow fixes. No new receive-budget or final recovery default is promoted by this brief.

## Reproducibility and evidence map

Canonical source inspected for this snapshot: Connect `4e625240`, SDK `cf90cf6f`, Server `29f80efdd`. Device source is the separately attested epoch above. Exact public-provider revisions/policies and provider-side counters remain unavailable. No diagnostic production deployment is implied.

All current evidence is below `/Volumes/data/urnetwork`; obsolete cache/temp paths in older briefs may no longer exist. Prefixes:

```text
C = /Volumes/data/urnetwork/c49-native-relocated-execution.PREP.dupk0plj
D = /Volumes/data/urnetwork/runtime-20261105
P = D/native-current-r9.PREP.1e3qhz4_/memsteady-wifi4-checkpoint-e559-r2-run/outer-result/perf/scoped-h1
```

| Evidence | Location |
| --- | --- |
| Pixel32 complete / Pixel64 failed-workload receipts | `P/memsteady-cell-01.json`, `P/memsteady-cell-02.json` |
| Physical raw speed results | `P/<cell>/<cell>-h1/fast-{1,2,3}.json`; cells `ios-memory-audit-v2-p00-r3-device-a-candidate-wifi` and `android-p00-r3-device-a-candidate-wifi` |
| Memory/diagnostics and joined teardown | Each cell's retained/teardown memory-summary JSON, command receipts and retained session artifacts |
| Galaxy cellular availability | `D/galaxy-cell-health.ey3m2etr/RESULT.json` |
| H1 three independent process pairs | `C/h1-bulk-three-pairs.READBACK.20261105` |
| Safe9KiB pool-only pair | `C/h1-pool9216-pair.READBACK.20261105` |
| Follow-up four cells / exact class and probe counters | `D/preserve4k-fourcells-current4e.qw0w4p7i/COMPACT-READBACK.json` and per-cell `CELL-READBACK.json` |
| Repeat-burst48 controls / monotone two-root checks | `D/preserve4k-controls.p4b0p3tl/COMPACT-READBACK.json`; `D/monotone-pure2-current4e.68_e5_om/COMPACT-READBACK.json` |
| Actual SDK64 provider constructor | `C/current-sdk-provider-constructor-qualified.READBACK.w0jnjjoz/CONSTRUCTOR.json`, SHA prefix `1f7d0a78` |
| Ordinary100-page pass | `D/cold-fixed-and-current100.brka8s7m/ordinary100/COMPACT-READBACK.json` |
| All-initial outage failure and partial witnesses | `D/all-initial-outage-current4e.d6t7xhaz/MINIMAL-FAILURE-HANDOFF.json` |
| Full-window OLD / publication OLD / preliminary FIXED | `D/full-window-old-diagnostic.fnwj_hwi`; `D/late-publication-old-v3.illf3vcj`; `D/fixed-syn-publication-smoke.evqzk_kq/RESULT.json` |
| RX candidate, source-only | `C/consumer-rx-fixed-total-r2.SOURCEONLY.cpuaudit/READY.json`, `c6617451` |
| Full-window refresh, source-only | `C/full-window-service-refresh-v2.SOURCEONLY.wk0ndx1e/READY.json`, `33a13adb` (supersedes blocked V1) |
| Bounded publication successor, source-only | `C/native-syn-publication-settings-r2.SOURCEONLY.__5dis_p/READY.json`, `5024ad45`; source-reviewed, runtime pending |

Useful code entry points: [transfer.go](transfer.go), [transfer_retained_budget.go](transfer_retained_budget.go), [transfer_recovery_selection.go](transfer_recovery_selection.go), [ip_tcp_return_recovery.go](ip_tcp_return_recovery.go), [ip_provider_memory.go](ip_provider_memory.go), [ip_remote_multi_client.go](ip_remote_multi_client.go), [ip_remote_multi_client_syn_retry.go](ip_remote_multi_client_syn_retry.go), SDK `mobile_memory_policy.go` / `device_local_transfer_memory.go`, and Server `connect/perfvar` browsing/synthetic-fast fixtures. Keep peer identities, account data and raw private network logs out of review replies.
