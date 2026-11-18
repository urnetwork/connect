# Low bulk throughput: independent review brief

Updated 2026-11-03. **A provider-side retained-send custody defect is reproduced and fixed in the checkpoint candidate.** Full suites, public Fast.com attribution and final device memory qualification remain open. The original pre-fix review brief is preserved below; this update supersedes its unresolved-status statements.

## Checkpoint findings after independent review

The recorded qualification precedes the incoming-origin sync. The merged tree includes newer upstream changes and requires a fresh run; the historical receipts are not transferred to that new source closure.

`LOWBULKFLOW-REVIEW1.md` led to a deterministic reproduction: selection priced an unpinned logical group as one packet, then H1 serialization selected a larger chunk. A shared-budget reservation could also lose after selection. The old taking serializer returned the failed chunk and disposed the suffix after public admission. NAT retained the inner TCP bytes, but the missing chunk had no Transfer resend item; slow inner TCP hole recovery could therefore coexist with zero Transfer timeout resends.

The correction pins the actual chunk for eligibility and atomically prepares its full post-contract reservation **before** advancing the raw cursor or transferring frame/callback ownership. Transient capacity loss keeps accepted reliable inputs queued on the existing fair budget wake path. The same correction covers queued singleton and coalesced ACK-required sends. Requested no-ACK traffic keeps its existing semantics. No memory target, retransmission timer or transport pacing setting changed.

| Controlled measurement | Original | Custody-only R1 | Extended custody R4 |
| --- | --- | --- | --- |
| Five retained SDK/H1 downloads, Mbps | 5.741 / 13.631 / 2.385 / 12.268 / 8.205 | 324.246 / 280.363 / 281.069 / 216.582 / 279.705 | 215.534 / 281.070 / 218.339 / 219.073 / 216.347 |
| Recoverable provider send-admission events | 1,169 | 1 | 0 |

All fifteen workload runs completed with checked joins. The five runs within a process share the fixture; they are not five independent process replications. Carrying-provider counts differed between R1 and R4, so their aggregate rates do not establish a regression or statistical equivalence. The provider counts and byte/exposure vectors are retained. Both fixes leave the standalone provider retained budget at **2 MiB** and the SDK32 budgets unchanged. These are synthetic results, not new public Fast.com measurements or Go-runtime memory qualification.

The exact one-provider diagnostic measured 14.84 Mbps originally, 348.39 with accounting disabled diagnostically, 77.23 with larger inner replay capacity, and 80.63 with custody repaired and default recovery. Disabling retained accounting is not the fix. A separate H1 chunk-size experiment (8 frames / 8,800 bytes) improved six fresh pairs by a median **25.48%**, from 79.77–80.22 to 100.10–100.59 Mbps. That policy remains research, with no baseline promotion or shipping change.

The extended correction passed **160 distinct focused roots normally and under race**. An additional real signed-contract growth regression proves the old failure, normal/race recovery, exact debit, and final full-root release. Genuine sibling reservations force the races; tests check payload/order, real receiver ACKs, once-only callbacks, cancellation, mutable budget shrink/regrowth and healthy siblings. Live unrelated `TransferProvide` owners are accounted separately; the global budget must reach zero after every client joins. Do not use client-global frame counts as a unique payload ledger.

A separate predeclared R3/R4 allocation ABBA used four fresh processes and 24 real ACK-path measurements. Median singleton cost was 600.024→600.068 B/op and about 11 allocations/op; ready-16 cost was 7,698.928→7,699.316 B/op and about 36 allocations/op. Payload/ACK/order and final release checks passed. This scope shows no large descriptor-allocation increase; the speculative array optimization was not applied. Evidence: `custody-allocation-abba.PREP.3ushx3qt/READY.json`, SHA `72a64cee5c03ac1f5e680cdd3214d68e8a6795a1364f992a4695ebffdd8b7679`.

The realistic browsing test still completes 100 HTML requests, 400 assets and 99 overlaps but fails its unchanged five-second navigation gate. Clock evidence separates a 5.880-second successful-listing→client-arguments gap from a later 3.546-second admission→navigation-dial gap. A separate deterministic test proves that late provider publication does not wake an already-retained SYN on old code. Its fix and auth-path attribution are still being qualified; do not claim the browsing pause is solved.

Evidence: `synthetic-fast-r4-custody-analysis.PREP.knj1pmre` (READY SHA `f74f219b04dfba92eaac3d72926f25e063de623c8a7df1fb8260ccd95f54a9ee`), `custody-r4-full-1.PREP.z8ha3xnw`, `custody-growth-rev7-1.PREP.1o6s0_xt`, and `reviewer-h1-eight-six-pairs.PREP._dfh56b6`, retained under `/Volumes/data/c49-native-relocated-execution.PREP.dupk0plj`. Full runtime claims require their raw outputs, source mappings and checked ownership receipts, not just a `PASS` line.

## Original pre-fix review context

## Problem and goal

Users report lower Fast.com download speeds even though ordinary browsing/TTFB feels better. We previously observed more than 40 Mbps and want to exceed **100 Mbps** where the access link and provider support it. We need sustained throughput across repeated downloads, not just a fast first response or a single successful speed test.

A local synthetic workload also produces only about 2–17 Mbps. That gives us a controlled investigation path, but its configured link rate is not proof of actual simulator capacity. We must distinguish a production packet-flow limiter from a simulator/fixture limiter.

Memory requirements remain **32 MiB for the iOS profile and 64 MiB for Android**. The iOS profile is measured on Android as a surrogate. This does not establish an actual iOS extension footprint below 50 MiB. Do not increase the memory target to obtain a speed result.

## Original frozen source and receive candidate

Frozen canonical revisions used for these measurements:

| Repository | Revision |
|---|---|
| Connect | `92b77b7b88a98ecd7ef0bf0c464744044f62e4c9` |
| SDK | `2ae5419ad3bc9212ceeda065c5b1fdf5a1ecd3b2` |
| Server | `35654c4b3239b2b8f17a0d7c290afd14c6fef3d0` |
| Tests | `fd56ce2d40a647a421ba2d66b6da0445a4d585e7` |

Host: macOS ARM64, Go 1.27.3. The synthetic tests use `GOMAXPROCS=2`, `-p=1`, `-parallel=1`, fresh processes, and retained raw results. Shared host activity is allowed; these comparisons are diagnostic screenings, not statistically qualified baseline promotions.

The private candidate changes exactly three production files:

1. `connect/transfer.go`: releases a modern decoded message's serialized carrier after transport dequeue/accounting, while retaining independent decoded payload/identity ownership; charges retained decoded metadata; rejects ambiguous mixed modern/legacy bodies without retiring the resident connection.
2. `sdk/device_local.go`: enlarges the provider-disabled retained receive leaf to **5 MiB / 10 MiB** for the 32 / 64 MiB profiles.
3. `sdk/mobile_memory_policy.go`: preserves the existing logical receive advertisement rather than increasing peer credit with the physical leaf.

For SDK32 the managed Transfer root remains **21,810,380 bytes**, advertised logical credit **2,181,038 bytes**, and candidate receive leaf **5,242,880 bytes**. The root, Pack allowance and advertised credit are unchanged by this comparison.

**Scope:** the local candidate binary changes both SDK-client and provider Connect carrier handling. The provider constructor also receives the carrier-release default. This is a joint endpoint/client-policy experiment, not a client-only comparison against unchanged providers. The three postimages are still private, not a qualified bulk-speed fix.

## Synthetic Fast.com workload

The workload runs **five speed tests back-to-back in one retained fixture**. SDK, TUN, H1, provider state, four origins and HTTP transport survive between tests. There is no inter-run GC, provider reset or cache clearing. Requests use fresh finite measurement windows; the retained fixture does not guarantee reuse of each HTTP socket.

Download path:

```text
four owned TCP/TLS streaming origins
  -> provider local NAT TCP/socket-return path
  -> provider Connect Transfer send
  -> authenticated H1 / simulated outer TCP path
  -> SDK Connect receive / MultiClient
  -> DeviceLocal receive callback
  -> application gVisor TUN / HTTP client
```

ACK/request path:

```text
application gVisor TUN ReadBatch
  -> compact uint16-length-prefixed batch
  -> public DeviceLocal.SendPacketBatch
  -> native ready-only MultiClient admission
  -> reliable Transfer / H1
  -> provider NAT TCP flow
```

Important fixture settings:

- Ten-second measurement per run; adaptive 1 -> 2 -> 4 -> 8 streams.
- Fixed startup, header, run-drain and overall deadlines. Progress cannot indefinitely extend them.
- Bounded 32 KiB origin blocks with byte validation; no compression, response caching or whole-body buffering.
- TLS uses the owned test trust roots. This is not an external website simulation based on HTTP status inspection.
- Access profile configured at **2 Gbps**, **40 ms RTT**, no configured loss/jitter; provider profile configured at 2 Gbps with no added propagation delay.
- Bounded emulator queues: 2 MiB. Application/provider packet MTU is **1100**, outer MTU 1500.
- Default application resource profile: channel 4096, batch 64, **no artificial application delay**.
- Each provider has an explicit **2 MiB retained resend budget** and a **256 MiB contract fixture allowance**. These are declared fixture settings, not a claim about public provider defaults.
- SDK32 mobile admission policy is explicitly selected even on the host.
- Full body, cancellation, request/worker joins and post-teardown balanced ownership are checked. Invalid runs remain visible; they are not averaged away.

The private test entry points are `TestFullTunH1SyntheticFastFiniteIntegrity`, `TestFullTunH1SyntheticFastSdk32` and `TestFullTunH1SyntheticFastSdk64`. SDK64 bulk remains unrun. The source packet and overlay are listed below; these roots are not yet landed in canonical Server.

## Measurements

### Five consecutive SDK32 bulk runs

| Run | Baseline Mbps | Candidate Mbps |
|---|---:|---:|
| 1 | 2.3855 | 11.1673 |
| 2 | 16.4888 | 7.7327 |
| 3 | 8.0739 | 4.7968 |
| 4 | 17.0909 | 14.8109 |
| 5 | 12.5301 | 7.6282 |

All ten runs were valid and quiescent. Both fixtures fully joined and released their cache leases. The candidate does **not** demonstrate a consistent bulk improvement. One process pair is insufficient to declare a statistical regression either.

Whole-fixture native admission, including shutdown:

| Counter | Baseline | Candidate |
|---|---:|---:|
| Offered packets | 17,205 | 13,501 |
| Accepted packets | 16,643 | 13,498 |
| Refused packets | 562 | 3 |
| Pure-ACK-only batch refusals | 18 | 3 |
| Mixed/unknown batch refusals | 544 | 0 |

These are attempts, not unique useful bytes lost. Their timing does not distinguish active download from cancellation/drain. Mixed-batch aggregate acceptance cannot identify which individual packet was refused.

**Candidate run 3 is especially useful:** 4.80 Mbps, zero native refusals, zero *captured* gVisor retransmits/timeouts, and multiple flat payload-sample intervals of roughly 0.5–1 second. The sampling grid is 500 ms; those durations do not prove a 500 ms timer.

Both complete runs recorded zero emulator loss and queue drops. This does not prove that every higher-layer forwarding/admission stage delivered every item.

### Finite integrity and local churn improved

Four concurrent 256 KiB bodies completed with exact EOF/trailer validation and one origin attempt each:

| Last payload after start | Baseline seconds | Candidate seconds |
|---|---:|---:|
| Body 1 | 0.296 | 0.266 |
| Body 2 | 6.203 | 0.247 |
| Body 3 | 0.336 | 0.353 |
| Body 4 | 6.203 | 0.392 |

The controller deliberately retains its ten-second window, so the finite test's approximately 0.839 Mbps whole-window quotient is **not** a sustained-speed measurement.

Three matched `server/proxy/TestProxyContractChurnLoad` screenings also improved:

| Pair | Baseline load seconds | Candidate load seconds | Baseline failed attempts | Candidate failed attempts |
|---|---:|---:|---:|---:|
| 1 | 100.929 | 3.570 | 5 | 0 |
| 2 | 41.569 | 3.842 | 2 | 0 |
| 3 | 15.497 | 1.851 | 0 | 0 |

Baseline live TUN 15-second write timeouts were 3/2/0; candidate had zero. Workload, contract caps, request deadlines and ledger assertions were unchanged. These results support a real receive-pressure improvement, but do not explain the remaining bulk limiter.

### Physical observations: corroboration, not matched causal proof

Only these phones are permitted: Pixel `3B161FDJG001KT`, Galaxy `R5CX21FY6ND`.

Representative public Fast.com observations include direct 520–670 Mbps, baseline H1 13–41 Mbps, candidate iOS-profile 46 Mbps and candidate Android-profile 36 Mbps. Provider/CDN cohorts were not pinned, and the candidate46 run lacked the independent OS-route observer. Do not interpret these as a controlled performance gain.

Two further candidate iOS-profile tests in one VPN session completed at **1 Mbps and 37 Mbps**, with approximately **25.25-second and 9.10-second** byte plateaus and zero sampled receive refusals. Provider sets and CDN endpoints still differed. No qualified result above100 Mbps has been obtained.

### Memory qualification status

The fresh hosted-H1 memory matrix currently has **0/8 qualified workloads**. Its first cell stopped before instrumentation because a pre-existing app-private credential destination was present; seven cells were not run. This is a setup failure, not an observed memory/network failure. Cleanup completed; the affected Galaxy app/test pair was cleanly reinstalled from attested candidate APKs, and both phones now pass credential-absence checks. A fresh eight-cell32/64MiB Wi-Fi/cellular run remains required.

## Established receive-budget root cause

A deterministic SDK-policy/real-codec test learned actual peer credit, withheld a predecessor, then sent1,703 outstanding1,280-byte originals (2,179,840 logical bytes), within2,181,038 bytes of advertised credit.

- Old policy refused1,278 distinct originals because retained cost exhausted the leaf; no refused original was falsely ACKed.
- Candidate refused zero, then delivered every payload exactly once after predecessor release, advanced cumulative ACK and balanced all ownership at teardown.
- Candidate normal and race runs passed; root and advertised credit were unchanged.

This is a **declared one-peer/one-frame MTU1280 test**, distinct from the1100-MTU bulk fixture. It is not a guarantee that arbitrary tiny packets or multiple peers can each fill their logical windows inside one shared leaf. Pressure must remain bounded with truthful ACK/retry ownership. Adjacent small-packet/legacy-envelope recovery tests are drafted but unrun.

The fix is real; the five-run bulk result demonstrates it is not the complete explanation.

## Critical telemetry gap and next captures

The original synthetic counter list includes application TUN, device-carrier TUN and provider-carrier TUNs, but **omits `environment.edgeTun`**. The edge is the actual outer-TCP sender for H1 downloads.

Therefore, zero captured retransmits does **not** rule out outer downlink TCP retransmission/RTO/cwnd collapse. Provider custom NAT return replay is also distinct from these gVisor TCP counters. In candidate run3, providers carrying original requests had zero Transfer timeout resends; the aggregate12 came from other providers.

The next diagnostic pair will use identical instrumentation on both versions:

1. Request/TUN dial, connection, TLS, request-write, first-header and first-payload phase stamps.
2. Existing SDK Transfer/receive diagnostics enabled by `-X github.com/urnetwork/sdk.transferDiagLogSeconds=2`.
3. The missing edge TCP endpoint's counters, explicitly labelled, on both arms.
4. Per-run byte samples, real provider attribution, cancellation/drain disposition and final joins.

Diagnostic overhead must be acknowledged; these captures do not replace the uninstrumented measurements. SDK log timestamps mark collection start and fields are read sequentially. Use collection intervals and exclude ambiguous run edges when aligning counters to stalls. Periodic provider state and per-flow body slopes are still gaps unless explicitly added.

The edge-counter successor is source-ready at `synthetic-fast-phase-edge.PREP.kmubn2v6/` but is not compiled or measured yet. The attempted22 phase controls were interrupted by a capacity-observer path-classification error during compilation, before any test roots ran. That harness failure is preserved; no instrumented bulk pair has completed. It is not evidence of a packet-flow test failure.

A separate **same-shaped-path TCP/TLS capacity control** is also being prepared. The existing same-generator host-loopback result of22.6–23.1Gbps proves only that the unshaped host generator is not capped at100Mbps. It does not establish the capacity of the emulator, gVisor or H1 path. Base-delay scheduling is pipelined, not a sleep per message, but actual Go2 packet processing capacity still needs measurement.

The bounded access queue also creates a real ceiling:2MiB held through20ms propagation permits at most roughly839Mbps losslessly per directional link, even though the configured rate is2Gbps. That remains well above the100Mbps target and does not explain the measured2–17Mbps.

A standard bounded CPU profile is a proposed follow-up if phase/counter data remain inconclusive; no profile has been collected for these bulk runs.

## Source findings and hypotheses to review

- **Do not assume sequence depth32 is a flight cap.** The32-slot provider send channel frees its admission slot after serialization/route write. ACK-owned resend items are separate byte-bounded containers. H1 WriteMessages completes a local stream write, not a peer Transfer ACK.
- Default application pacing is off. `EstimatedBytesPerSecond` is a scoring prior, not an established traffic shaper. Increasing either without evidence is not a fix.
- Provider origin reads are64KiB; return retention is64MiB. `ReturnRecoveryMaxPacketCount=1` bounds replay, not ordinary download flight.
- Host application/outer TUN TCP default buffer is1MiB, maximum4MiB. gVisor advertises about half available receive memory; empty-window ideal ceilings at40ms are roughly105Mbps initially and419Mbps at the maximum. These are conditional calculations, not measured capacities or a demonstrated4.8Mbps ceiling.
- Pure TCP ACKs bypass duplicate-collapse gates, but native admission is zero-wait and reliable Transfer admission can refuse them. The SDK retains no retry continuation for a refused original. A final advancing ACK might await later traffic/RTO to regenerate; aggregate refusal counts do not establish that event. Existing tests already cover refused zero-window reopening/persist recovery. A nonzero-window final-ACK experiment is staged but unrun.
- Look at actual outer TCP ACK/RTO/cwnd state, Transfer credit/hold/service-window waits, ACK-write timing, H1 write/read batch cadence, CPU/locks, and provider TCP right-edge/return-read scheduling. Distinguish a full buffer from a stalled consumer; do not increase memory merely because throughput is low.

## Questions for the reviewer

1. Which missing observation best distinguishes the low-rate plateau: outer TCP recovery, Transfer flow control, ACK scheduling, provider return flow control, or simulator CPU/service capacity?
2. Can any production gate hold throughput near these rates without a refusal/retransmit in the currently captured counters? Identify the exact ownership/wait boundary.
3. Is the five-run adaptive workload representative enough, and what minimal same-path capacity control would falsify a fixture-induced ceiling?
4. What deterministic test would reproduce your proposed mechanism through the public path, without a fake send failure, forced provider adoption or manual ACK/retry injection?
5. Which adjacent paths share the faulty reasoning, if a mechanism is found?

Preserve `connect/CODESTYLE.md`: shared receive callbacks do not block; complete recoverable messages may be refused without tearing down unrelated resident flows; ACKs require retained or downstream custody; failed admission must preserve retry eligibility. Any fix needs a deterministic root-cause regression and measured improvement within the memory target.

## Evidence and reproduction packet

On this host all relative artifact paths below are under:

`/Volumes/data/c49-native-relocated-execution.PREP.dupk0plj/`

| Evidence | Artifact |
|---|---|
| V5 source/commands/overlay | `synthetic-fast-v5-final.PREP.d_1e3c37/` |
| Exact private production candidate | `fast-native-candidate.PREP.1q7rs99c/PRODUCTION.overlay.json`, `SOURCE-PACKET.json` |
| Actual uninstrumented pair analysis | `synthetic-fast-sdk32-pair-analysis.PREP.c1bdihd0/PAIR-ANALYSIS.json` |
| Pair analysis SHA256 | `ce2c4267689000035085f3df8f600464fc038924d033ecbd8c4fc57df3f9d1ac` |
| Baseline five raw log | `synthetic-fast-v5-sdk32-five.PREP.mo_z47ck/stdout.jsonl` |
| Candidate five raw log | `synthetic-fast-candidate-five-startup60.PREP.poqtvdcy/stdout.jsonl` |
| Candidate finite raw log | `synthetic-fast-candidate-finite-g300.PREP.lzwv1j4x/stdout.jsonl` |
| Three local churn comparisons | `server-contract-matched-replicates.PREP.cgqc820r/ACTUAL-THREE-PAIR-ANALYSIS.json` |
| Qualified deterministic receive regression | `sdk-retained-mtu-window.PREP.dttbk0qy/revision4/QUALIFIED-REGRESSION.json` |
| Whole-binary candidate role audit | `fast-candidate-role-scope.PREP.wb6qkhas/` |
| Pure-ACK admission audit | `fast-native-pure-ack-audit.PREP.bsh4q9dp/REPORT.txt` |
| Phase source/review, before edge-counter addition | `synthetic-fast-phase-probe.PREP.zk056sqa/`, `fast-phase-probe-review.PREP.n9j0j08q/` |
| Physical same-VPN two-shot analysis | `fast-two-shot-aligned-v2.PREP.qihp79k5/` |

The private source packet's `COMMANDS.json` gives exact commands. Its performance roots require the owned local Server/Postgres/Redis fixture and SDK test configuration. Do not use production credentials or deploy anything to reproduce the local case. Transfer the reviewed source packet/overlays as well as this brief if the reviewer is on another host.

When parsing raw Go `-json` logs, concatenate `Output` records for the selected Test before decoding `[perfvar] synthetic-fast` JSON: testing splits the long report across records. Read the matching FINAL/child-custody receipts; a prepared command, compile failure, skipped test or canceled process is not a measured run.

The separate overlapping100-page stall objective remains open, but new runs are deferred to prioritize bulk speed. No claim of a complete packet-flow/performance fix or full MEMSTEADY/PERFVAR qualification is made.
