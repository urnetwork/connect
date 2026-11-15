# WINDOWSIZE2 review 1 (independent)

2026-11-11. Independent reviewer. I did not read any `*-REVIEW*.md` or `*-META.md`.

**Version reviewed:** `WINDOWSIZE2.md` as committed in `391c679c`: 206 lines, 11,783 bytes,
SHA256 `64f21a92950d3056a99a9f50a47742386aa07fa53fd48fd17bf3d49d4cd7942a`.
Code citations point at Connect `HEAD` = `391c679c`, which is post-merge. Where the measured source
differs, I cite the donor under `tests/testdata/window-size-checkpoint-95b8/source/...`.
While I worked, the working tree changed (`transfer.go` modified in `tryDeepenH1PackQueue`, untracked
`transfer_shared_pump_small_group_count_test.go`). I did not use those changes, and I modified
no project files apart from this one.

**What I ran:** only read-only Python extraction over existing artifacts. I did no Go build or test,
no fixture run, and no device run. The scripts are in
`/private/tmp/claude-501/-Users-builder-urnetwork-connect/5b991e4a-8699-464e-a6d4-9e0a0403d954/scratchpad/windowsize2-review1/scripts/`
(`extract.py`, `flows.py`, `perprov.py`, `cuts.py`, `outline.py`). They read the raw
`run/fast32.stdout` and `run/fast64.stdout` under
`/Volumes/data2/urnetwork/temp/aggregate-95b8-retained-performance-diagnostic-r1/`.
I checked every SHA256 the writeup cites (matrix `d14fcbfc…`, summary `fce83d95…`, closure `7b3c8655…`,
fast32 stdout `f275d0b6…`, fast64 stdout `05b010af…`), and all match.

---

## 0. TL;DR

1. **Window 3 is mostly one provider path collapsing; it is not an aggregate phenomenon.** From window 2 on,
   destination affinity put origin `.10` on provider row 1 and origins `.11/.12/.13` on provider row 6.
   That gives 2 bulk flows on p1 and 5–6 on p6. In W3, p6 delivered **39.6 Mbps across 6 flows** (W2: 144.9 Mbps
   across 5) while p1 delivered 102.4 Mbps across 2. In W5, p6 fell to **36.0 Mbps** while p1 hit its best,
   **184.5 Mbps**. The writeup reports the per-flow bytes but not this provider decomposition.
2. **The best per-window discriminator in the existing data is the provider sender's timeout-deferral count**,
   which the writeup omits. On p6 `TimeoutResendDeferCount` is 370 / 0 / **697** / 0 / **535** per window, with
   `TimeoutResendWithRecentCumulativeProgress` nearly identical. On p1 it is 0 in every window. The same signature
   shows up on the slow providers of the 64 MiB run and in SIMPLERISSUE3's table. It means items wait longer
   than the sender's RTO while cumulative ACKs keep arriving: a **round-trip residence/queue on that provider's path**,
   not loss.
3. **Connection setup on p6 took about 1 s per phase in exactly W3 and W5:** dial about 0.95–2.6 s, TLS about
   0.76–1.18 s, one request-to-header gap of about 1.0 s. In W2 and W4 the same phases took 42–295 ms. In W5, two
   flows' dials and TLS completed at the same instants (2,446 ms, then 3,203/3,204 ms). That pattern points to a
   queue releasing, not to independent RTOs.
4. **The two W3 TCP timeouts (and all four in W5) are on the app's own gVisor stack**, which is client inner TCP
   on the upstream side. They are not outer-carrier RTOs. The per-stack TCP rows already in the raw report attribute
   them. Nearly all "TCP retransmits" are on outer H1 carriers and the edge, and W2 had 1,079 of them with zero
   link drops.
5. **Receive-side refusals and handoff-count pressure are ruled out for W2–W5 by data already captured.**
   Summed over all six consumer clients at each window cut, `PackHandoffDropCount` reaches 804 by the end of W1
   and stays flat. The decoder's all-funding refusal path increments that counter. Head reservations reach 16 × 30,544
   bytes in W1 and stay flat. `PackHandoffSaturationCount` is 0 everywhere, and the maximum handoff count is
   182 of 256 (64 MiB: 153 of 512, with zero drops). The committed matrix hides this because it reads only
   `Clients[0]`, whose order changes between cuts (§3.2).
6. **The sparse-group count hypothesis is not implicated by this run.** It may be a real latent defect, but this
   run shows no count saturation. Expect it to change nothing in Fast5 variance.
7. **Before blaming the protocol, test the fixture's outer TCP.** Every simulated stack (app, device carrier,
   provider carriers, edge) is gVisor **Reno** without pacing (`tun.go:281`, gVisor `protocol.go:564-565`). The
   access link is 2 Gbps with 40 ms RTT, so BDP is about 10 MB against a 2 MiB queue. Each provider path has its
   own pair of outer H1 connections, because each generated client gets its own platform transport
   (`ip_remote_multi_client_api.go:1433`). The post-dip ramps (W3 about +14.7 Mbps/s, W4 about +11 Mbps/s)
   are numerically consistent with Reno congestion avoidance on two 40–45 ms connections (2 × MSS/RTT² ≈ 9–14.5 Mbps/s).
   That is consistent, not proven.
8. Upstream changed pacing service-rate decreases after the measured checkpoint (`transfer_window_pacing.go`
   `headBoundedDecrease`, +22 lines). That is directly in one of the candidate mechanisms, so results must be requalified post-merge.

---

## 1. My understanding, and where I agree or disagree

**Problem.** After the receive-backing checkpoint (`99c9d2a4`), the 32 MiB Fast5 sequence passes but varies
268.5 / 308.8 / 141.9 / 236.8 / 220.4 Mbps. The developer wants a deterministic reproduction of any avoidable
protocol or scheduling limit behind the W3 dip, then a measured fix, with no deadline increase. The writeup's
plan has three parts: correlate queue drops and timeouts with flows (Q1); classify the service-rate loss (Q2);
and run a prepared sparse-group count witness before tuning depth (Q3). Q4–Q6 cover per-window refusal deltas,
controlled impairment replay with a direct-TCP reference, and repeatability.

**Agree (verified):**
- Every table value I checked matches the raw report: per-flow bytes, initial writes and bytes, reuse counts,
  pending-zero maxima, link settings, sampled RX peaks, refusal totals 642/626, and pure-ACK Pack refusals of 0.
- "Lower initial-write rate and sampled RX use are compatible with reduced upstream service/progress"
  (WINDOWSIZE2.md:109-112). Yes, and per provider the effect is sharper (§2.1).
- Refusing to raise deadlines, and insisting on old-fail/new-pass deterministic tests.
- The 2/7 advertisement math. `connect/transfer.go:13478-13497` scales the advertised window by 2/7 of physical
  RX, and `sdk/device_local_transfer_memory.go:131-134` sets RX = ceil(7L/2) with lane-0 H1 count 256/512.
  `transfer_logical_lane.go:39-46` gives nonzero lanes ceil(count/8), i.e. 32/64. This matches WINDOWSIZE2.md:118-146.
- "A single 32-versus-64 sequence is insufficient…" (WINDOWSIZE2.md:62-63). Per-provider data reinforces it:
  doubling the advertised credit (2,181,038 to 4,362,076 bytes) did not raise the best per-provider rate
  (32 MiB best 184.5 Mbps on 2 flows; 64 MiB best 154.7 Mbps on 2 flows). Receive credit is not the binding
  per-path limit in these runs.

**Disagree, or would reframe:**
- The unit of analysis should be the **provider path** (generated client plus provider), not the window aggregate.
  Each path has its own outer connections, Transfer sequence (all Auto download data is lane 0), pacing service,
  and RTT estimator. The aggregate sums 2 (32 MiB) or 3–4 (64 MiB) independent, high-variance pipelines with an
  uneven flow split set by affinity.
- The writeup leans toward correlating the 25 queue drops (Q1) and the sparse-group witness (Q3). W5 shows the
  same p6 collapse with **zero** link queue drops, and the run shows no count saturation. Neither is on the critical path.

---

## 2. New evidence from the existing artifacts

All values below come from the raw report JSON (`sequence.runs[i]`, `BeforeDetails`/`AfterDetails`,
`production_constructor_policies.receive_backing_window_cuts`). Rates are validated bytes over the 10 s window.

### 2.1 Per-provider decomposition, 32 MiB (provider rows by mask bit: p1 = mask 2, p6 = mask 64)

| W | Total Mbps | p1 flows / Mbps | p6 flows / Mbps | p6 initial writes | p6 timeout deferrals (recent-progress) | p6 timeout resends | app-stack RTOs | link queue drops | SYN offered / refused |
|---|---:|---|---|---:|---:|---:|---:|---:|---|
| 1 | 268.5 | 4 / 147.4 | 4 / 121.1 | 16,047 | 370 (384) | 110 | 0 | 314 | 9 / 0 |
| 2 | 308.8 | 2 / 164.0 | 5 / 144.9 (+1 never connected) | 19,243 | 0 (0) | 4 | 0 | 0 | 15 / 6 |
| 3 | 141.9 | 2 / 102.4 | 6 / **39.6** | 5,694 | **697 (699)** | 6 | **2** | 25 | 11 / 3 |
| 4 | 236.8 | 2 / 145.4 | 6 / 91.4 | 12,273 | 0 (0) | 8 | 0 | 0 | 8 / 0 |
| 5 | 220.4 | 2 / 184.5 | 6 / **36.0** | 5,122 | **535 (536)** | 5 | **4** | 0 | 10 / 2 |

On p1, deferrals are 0 in every window, with initial writes 19,497 / 23,008 / 14,608 / 20,259 / 25,936.
Selective-gap writes are 0 on both providers in W2–W5. The p6 rows `RouteRetainedItemCount=4` and a
monotonically growing `RouteUnacknowledgedDuration` (6 → 14 → 22 → 38 → 46 s) show a long-lived stale
unacknowledged route item on p6. I did not determine its owner.

**Placement.** W2–W5 bulk flows follow destination affinity: s0 and s4 go to `.10` on p1, and every flow to
`.11/.12/.13` goes to p6. All 14–16 small-GET probes per window target `.10`, so they run on p1. The writeup's
"dial distribution 3/5 → 2/6" (WINDOWSIZE2.md:80-82) counts provider-side origin dials, and these include the
two probe dials per window. The bulk split was 2 : 5 (+1 flow that never connected) in W2 and 2 : 6 in W3–W5.
W1 was mixed (.12 and .13 split across providers). Affinity defaults are at `ip_remote_multi_client.go:277-284`
(`MaxFlowsPerExit 16`, `AffinityMaxTime 1m`, `PerformanceAwareAffinity`) and `:436` (`DestinationAffinity`).

### 2.2 The same signature in the 64 MiB run

| W | Mbps | provider: flows / Mbps / deferrals |
|---|---:|---|
| 1 | 214.3 | p0 3/80/459 · p1 1/32/179 · p2 3/57/276 · p3 1/45/212 |
| 2 | 258.8 | p0 2/57/33 · p1 2/85/**0** · p2 2/82/702 · p3 2/35/464 |
| 3 | 197.9 | p0 4/74/449 · p2 2/77/325 · p3 2/47/41 |
| 4 | 266.5 | p0 1/52/**0** · p2 2/116/**0** · p3 4/98/55 (one flow never connected) |
| 5 | 275.1 | p0 2/67/253 · p2 2/**155**/**0** · p3 4/53/315 |

The fastest per-flow paths are the ones with zero deferrals. The correlation is strong but not perfect
(W2 p2). SIMPLERISSUE3.md:47-55 shows the same monotone pattern on an older baseline: 0 deferrals in the 306 Mbps window,
thousands in the 40–70 Mbps windows. Treat **per-path deferral count as the primary slow-path marker** and explain it.

### 2.3 Connection-setup phases on the slow path (32 MiB, fresh p6 flows)

| W | dial (ms) | TLS (ms) | notes |
|---|---|---|---|
| 2 | 43–184 | 42–137 | s7 (.13) dial started at 1.5 s and **never completed**; 6 SYNs refused at native admission |
| 3 | s5 1,104 · s6 1,777 · s7 1,063 | 995 · 1,178 · 957 | s2 request→header ≈ 1,012 ms; first payloads at 4.1–5.5 s |
| 4 | 42–295 | 44–182 | — |
| 5 | s5 946 · s6 946 · s7 2,570 | 757 · 758 · 760 | s5/s6 dial-done and TLS-done at identical instants |

Probe GETs all ran on p1. In W3 they took 97–651 ms; in W5 they took 49–256 ms. p1 was also degraded in W3,
just less (its total fell from 164 to 102 Mbps).

A W2 `MatchingSynack` witness shows the SYN-ACK traveling `Immediate=true, ProtocolOnly=true` on lane 0. If that
holds generally, SYN-ACKs already bypass Transfer DATA ordering. About 1 s per handshake phase then points to
residence **below** Transfer sequencing: the H1 socket or edge FIFO. This is a single witness, so treat it as a lead.

### 2.4 W3 aggregate time series (0.5 s samples, Mbps)

`76, 175, 152, 84, 96, 100, 110, 114, 128, 133, 131, 148, 149, 157, 170, 167, 176, 183, 194`

The dip at 1.5–2.0 s lands while only s0 (p1), s1, s3 (p6) and s4 (p1) were carrying data, so it hit both providers.
The increments afterwards are roughly constant (about +14.7 Mbps/s), which looks additive, not multiplicative.
W4 rises about +11 Mbps/s from 199 Mbps.

### 2.5 Per-stack TCP attribution (already in the raw report)

The rows of `tcp_sent_received_retransmits_timeouts` are app, device carrier, then provider carriers 0–15
(donor `synthetic_fast_native_test.go:939, 956`). `edge_tcp_…` is the outer sender toward the SDK.

| W | app rtx/to | p1 carrier rtx | p6 carrier rtx | edge rtx/to | link queue drops |
|---|---|---:|---:|---|---:|
| 1 | 0/0 | 250 | 233 | 564/0 | 314 |
| 2 | 0/0 | 284 | 344 | 451/0 | 0 |
| 3 | **2/2** | 39 | 13 | 46/0 | 25 |
| 4 | 0/0 | 236 | 159 | 128/0 | 0 |
| 5 | **4/4** | 152 | 84 | 77/0 | 0 |

App-stack `SegmentsSent` equals bridge `Offered` exactly (W3: 22,899), so the app row is the client's egress.
gVisor SYN retransmissions do not increment `Timeouts` or `Retransmits` (gVisor `connect.go:621-666`), so the
app RTOs are data-segment RTOs (TLS or request flights), and SYN losses are visible only through bridge SYN refusals.
Retransmits without link drops (W2: 1,079) mean drops or spurious retransmits occur elsewhere: gVisor segment-queue
drops (`endpoint.go:3002-3011` → `stack.Stats().DroppedPackets`), TUN outbound drops (`tun.go:1806-1822`),
or RACK/TLP. None of these are exported today.

### 2.6 Receive side across windows (sum over all consumer clients at each cut)

| cut | Σ PackHandoffDrop | Σ Saturation | max handoff count | Head reserved |
|---|---:|---:|---:|---:|
| W1 start | 0 | 0 | 0 | 0 |
| W1 end … W5 end (every cut) | 804 | 0 | 182 (cap 256) | 488,704 |

64 MiB: zero drops and zero saturation in every window, max count 153 (cap 512).

Head 488,704 = 16 × 30,544 matches `FundingAfterOrdinaryRxRefusal[2]=16`. All-funding refusal increments
`PackHandoffDropCount` in both the measured donor (`transfer_receive_memory_decode.go:128-136`) and main
(`transfer_receive_memory_decode.go:108-114`). So all 642 ordinary and 626 final refusals occurred in W1.
Cut times are about 1 ms after each window end and at the next start, and the census covers all six clients
(one identity swap in W5, carrying zero).

### 2.7 Nothing was wasted in W3: it simply sent less

Wire bytes per delivered byte were 2.315 / 2.273 / 2.334 / 2.282 / 2.291. Transfer initial-message bytes per
delivered byte were 1.057 / 1.062 / 1.089 / 1.066 / 1.071. Taken with no selective gaps and an empty RX, this rules
out duplication, dead-flow traffic and Transfer-level HOL as the W3 deficit. The sender was not offered, or not
permitted, more.

---

## 3. Gaps, unverified assumptions, likely-wrong statements

1. **"Exact per-window totals are unavailable" (WINDOWSIZE2.md:134-137, matrix `unavailable[1]`).** They are
   derivable (§2.6): W1 holds all 642 ordinary and 626 final refusals, and W2–W5 have zero. The conclusion
   "must not be assigned to window 3" is right, and in fact the W3 count is zero.
2. **Matrix extraction defect.** `fast32-five-window-matrix.json` `rows[*].memoryCuts[*].queueCumulative` takes
   `Windows.Clients[0]` only, and client order varies between cuts. It reports W2 PackHandoffDrop 224 → 5 (an
   impossible decrease), W3 0/0 (an idle client) and W5 0 → 5. Sum over clients, or key by `ClientId`.
3. **"Current aggregate counters cannot establish … which flow lost data or feedback" (WINDOWSIZE2.md:110-112).**
   The indexed rows already say which **stack**: both W3 timeouts and all four W5 timeouts are app-side (§2.5).
4. **The "Provider reliable-admission wait 0/0" row is structurally zero.** `reliableAdmissionAvailable` returns
   `true` whenever `ReliableAdmissionBoundedByDelivery` is off (`transfer.go:14096-14102`). That mode and
   delivery window sizing are mutually exclusive (`transfer.go:826-846`). Both counters
   (`ReliableAdmissionByteLimitMinimum` and `ReliableAdmissionWaitCount`) are 0 in every provider row of both runs,
   so the bounded mode was not active. The row is no evidence of absent window pressure, because window-limited
   time is not exported.
5. **The 25 queue drops are not necessary for the slow state.** W5 reproduces the p6 collapse with zero link
   drops (§2.1). Correlating the 25 drops (Q1) is lower value than per-path state sampling.
6. **"Exported native offer/refusal counters have separate semantics" (WINDOWSIZE2.md:188-189).** I checked:
   refused packets are returned to the pool and dropped, not retried by the fixture
   (`server/connect/perfvar/sdk_browsing_native_test.go:127-136`). `connect/ip_native_ingress.go:11-13` states that
   refusal is the source TCP's recovery responsibility, and "accepted" means SDK custody, not delivery (ack_progress scope).
   About 92% of app egress is refused, mostly advancing pure ACKs (132,207 of 144,962 refused pure ACKs), in fast and
   slow windows alike. That makes the rate uninformative per window, but SYN and RST refusals matter (§2.3).
7. **The fixture's TCP is not production TCP.** Production H1 runs over kernel TCP: a Linux server with CUBIC or BBR
   and usually fq pacing, and an OS TCP on the phone. In the fixture every hop is gVisor Reno, unpaced, with a
   0.2×BDP access queue. The app stack advertises half its buffer (gVisor `rcvAdvWndScale = 1`, `endpoint.go:71`),
   so fresh flows start at about 512 KiB (ACK `Window=4019` × 128). Reused flows had autotuned to about 2 MB
   (`16307` × 128). Fresh-versus-reused matters, and the fast p1 flows are the reused ones. The writeup does not
   separate fixture TCP dynamics from protocol behavior; its Q5 direct-reference control is the right idea and should come first.
8. **The upstream merge touched candidate mechanisms:** pacing decreases (`transfer_window_pacing.go`), discovery
   cool-off (`ip_remote_multi_client.go`, +92), and held-item reservation order (`transfer.go`). WINDOWSIZE2 already
   says the merge is unqualified. Add that the pacing change sits directly in H1c below.
9. **Missing quantities the fixture can already read for free:** per-flow bytes per 500 ms sample (only totals are kept),
   per-provider sender window, pacing and RTT, per-link drop counters (summed today at donor `synthetic_fast_native_test.go:994`),
   gVisor recovery-episode and drop counters, and Go scheduler latency.

---

## 4. Root-cause hypotheses, ranked, each with a cheap discriminating experiment

**H1. Round-trip residence on the heavily loaded provider path. High likelihood as the proximate cause.**
The evidence is §2.1–2.3 and §2.7. On a credit- or window-limited path, throughput ≈ in-flight / residence.
For example, 2.18 MB / 0.45–0.5 s ≈ 36–40 Mbps, which is p6 in W3 and W5. The open question is **where the queue
sits**, ranked:

- **H1a. The outer H1 TCP for that provider path is congestion-limited after a loss** (gVisor Reno; drops from the
  link queue or from invisible gVisor/TUN drops), and the bytes above cwnd sit in the socket send buffer, which
  auto-tunes up to 4 MiB (`tun.go:151-160`). Fits: the per-path isolation, the additive ramps, the about-1 s handshake
  phases despite immediate SYN-ACK delivery, and the deferral storms.
- **H1b. Host scheduling.** A starved goroutine (the device-side H1 reader for that generated client, the edge forwarder,
  or the provider sequence loop) makes the outer TCP receiver-limited. The host was busy. Total process CPU was about
  76 CPU-seconds over about 52 s.
- **H1c. Transfer pacing or learned-window lock-in.** `windowPacingRate` paces at 1.1× service, or 0.95× when
  backlogged (`transfer_window_pacing.go:291-330`). A path stuck "backlogged" can hold a low pace. The window only
  shrinks on explicit remeasurement (`transfer.go:13647-13649`). The additive, not ×1.1 per second, ramp shape
  argues somewhat against this as the primary cause.
- **H1d. Feedback starvation.** Transfer ACKs for the client→p6 upstream sequence ride the congested downstream FIFO,
  so the upstream sequence cannot free capacity, and ready-only native admission (`ip_remote_multi_client.go:7908-7912`,
  timeout 0) then refuses that path's app ACKs and SYNs. This is consistent with the p6 provider writing far fewer
  upstream ACKs in W3 and W5 (280 and 383, against 912 in W2 and 752 in W4).

*Experiment E1: one instrumented Fast5 run, about 1 minute, no production change.* Every 250 ms, per provider path:

- `providerClient.DestinationSendStats(generatedClientId)` (`transfer.go:5253`, fields `transfer.go:6872-6907`):
  Window, Ceiling, Pacing/Service byte rate, `ServiceBacklogged`, Rtt Mean/Min.
- The consumer's `PacingDiagnosticSnapshot` (`transfer_pacing_diagnostic.go:120`) for the upstream sequences.
- Per TCP endpoint on the edge and carrier stacks, via `stack.RegisteredEndpoints()` (gVisor `stack.go:2027`):
  `TCPInfoOption` (SRTT, cwnd, ssthresh, CcState) and `SendQueueSizeOption`/`ReceiveQueueSizeOption`. This needs
  one fixture-tag accessor, because `Tun.stack` is private.
- Per-flow validated bytes.

Predictions: H1a shows a cwnd cut followed by a multi-MB edge-side send queue on the slow path's device connection
at the onset. H1b shows a large device-side receive queue plus a scheduler-latency spike. H1c shows a small socket
queue with the pacing rate pinned at about 0.95× service and Backlogged=true. H1d shows the upstream sequence at
capacity with app-ACK and SYN refusals concentrated on that path. Add per-destination-IP bridge refusal counts by
class, so refusals map to providers through affinity.

*Experiment E2: A/B, fixture-only.* Give the edge and carrier stacks CUBIC (gVisor `CongestionControlOption("cubic")`),
or raise the access queue to at least the BDP (about 10 MB). Run Fast5 three times per arm. If W3-type slow-path
windows and the additive ramps shrink markedly, much of the variance is simulator outer-TCP dynamics. Separately,
**E3** is your Q5 direct-reference path: eight HTTP/1 flows, same link profile, no URnetwork. It measures the
fixture's own baseline variability.

*Experiment E4.* Constant window sizing (`WindowSizingConstant`) as a single A/B. Set it on the provider senders'
`SendBufferSettings.WindowSizing`, or process-wide with `SetWindowSizing` before construction; it is a documented
one-call rollback (`transfer.go:797-812`). Note that the process-wide form changes both directions. If slow-path
windows persist unchanged, H1c is out.

*Experiment E5.* Record runtime/metrics `/sched/latencies:seconds` and `/cpu/classes/total:cpu-seconds` at each cut
(`RuntimeBefore`/`RuntimeAfter` already read runtime/metrics). Optionally repeat at `GOMAXPROCS` 4 versus 8, or on
a quiet host.

**H2. Connection-setup packets dropped or deferred under load. Medium-high likelihood as a contributor; deterministic and fixable.**
Native admission refused SYNs in W2 (6; one flow never connected in 8.5 s), W3 (3) and W5 (2). In 64 MiB, W3's 5
refusals coincide with a 7.2 s dial (≈ 1 + 2 + 4 s SYN backoff) and W4's 9 with a never-connected flow. A refused
SYN costs at least 1 s, doubling each time, and gVisor does not count it as a timeout. The existing
`tcpSynRetry` covers "an explicitly failed unanswered SYN" (`ip_remote_multi_client_syn_retry.go:1-3`). Check
whether ready-only refusals are covered; the dial quantization suggests not.

*Experiment E6: deterministic.* Pin the client→provider upstream sequence at capacity, offer a real SYN natively,
release capacity, and measure dial latency. Old code: about 1 s or more, via app RTO. Fix: retain one exact unsent
SYN per flow and retry when capacity opens, like the native ACK retry. This is the old-fail/new-pass pair you asked
for, and it plausibly matters for the 64 MiB page-1 TTFB failure as well (not analyzed here).

**H3. Placement imbalance from destination affinity. Medium likelihood, as an amplifier rather than a cause.**
Three of four origins stuck to one exit for the whole process (AffinityMaxTime is 1 minute versus about 52 s of
test). One slow path then carries 75% of the flows, while 4 of the 6 window exits carry no bulk traffic.
`PerformanceAwareAffinity` only acts in the race, which donors pre-empt (`ip_remote_multi_client.go:902-913, 935-940`).

*Experiment E7.* Fixed-provider Fast5 (`synthetic_fast_fixed_provider_test.go` exists) for per-path variance, plus an
eight-origin or `MaxFlowsPerExit=4` arm for balanced placement.

**H4. Cancellation residue from the previous window. Low likelihood.**
W3 shows the most app RSTs offered and refused (4,272 / 3,011, against about 2,900 / 1,600 in other windows).
But the fastest 64 MiB windows had more (6,816 and 6,649), and §2.7 shows no wasted bytes. Check this with E1's
per-flow series and per-IP refusal classes; don't chase it first.

**H5. Receive funding or handoff count (including the sparse-group shape). Ruled out for W2–W5 (§2.6).**
It can return in W1-type windows, where refusals were real. Keep the witness as a latent-correctness test only.

**H6. Transfer loss or HOL within the lane-0 sequence. Low likelihood for W3.**
Selective gaps were 0, p6 timeout resends were 6, RX was nearly empty, and the overhead ratios were flat. Lane-0
sharing still matters structurally (`sdk/mobile_memory_policy.go:200-209, 478`: provider-side lanes are off), but it
is not what W3 shows.

---

## 5. New ideas and simpler approaches

1. **Diagnose per path, in short loops.** One about-1 minute instrumented run plus one-knob A/Bs (E2, E4, E7) will
   discriminate H1a–H1d faster than another closed qualification cycle. Keep heavyweight qualification for candidate fixes.
2. **Keep bulk out of FIFO socket buffers.** If H1a holds, the cleanest product-level remedy is a small not-yet-sent
   cap on H1 sockets: `TCP_NOTSENT_LOWAT` on server sockets via `RawConn.Control`, or a modest send-buffer max. The
   backlog then stays in the Transfer layer, where SYN-ACK, handshake and ACK traffic can bypass bulk, and where the
   delivery-sized window and the RTO see true RTT, so deferral storms stop. That one mechanism is simpler and more
   general than per-packet retries. Validate it in the fixture with a smaller edge-stack send buffer before touching production.
3. **Make the fixture's server side production-like:** CUBIC for the edge stack, a queue of at least the BDP or
   emulated pacing. Otherwise label it explicitly as a harsh stress profile, so fixture artifacts are not optimized away
   in protocol code.
4. **Protect connection setup end-to-end:** never drop SYN, or the first client flight, on a ready-only refusal (H2).
   Continue the "immediate protocol" direction from SIMPLERISSUE3 for server handshake flights, but note that it
   cannot bypass a socket FIFO (idea 2 covers that).
5. **Placement:** when a donor exit shows a deferral storm or large RTT inflation, let new origins race instead of
   inheriting. Or reduce `AffinityMaxTime` for bulk-heavy groups. Either way the cost is egress-IP stability, so A/B it.
6. **Cheap visibility wins:** sum receive stats per client (§3.2); export per-link drops unsummed; add gVisor
   `DroppedPackets`, `FastRecovery`, `SACKRecovery`, `TLPRecovery`, `SpuriousRecovery`, `SegmentsAckedWithDSACK`
   and `Tun.LinkStats().OutboundDropCount` to each stack row; add a probe GET to an origin mapped to each provider,
   so loaded latency becomes a per-path residence probe.

---

## 6. Direct answers to the writeup's questions

1. **Q1, which link and tuples hold the 25 drops, and can an event ring correlate them?** Not attributable from
   existing data, because links are summed (donor `synthetic_fast_native_test.go:994`). Keep `snapshotLinks()` per
   link and direction, and add a small first-N drop ring (link, direction, outer 5-tuple, size, queue bytes). The
   two TCP timeouts are already attributed: the app stack. But W5 collapses with zero drops, so prioritize per-path
   gauges (E1) over drop correlation.
2. **Q2, TCP recovery, provider/flow scheduling, reuse/cancellation, or host?** It is a provider-path limit with
   about 0.5–1 s residence. It is not receive memory, not wasted bytes, not Transfer loss, and not app-TCP recovery.
   Reuse matters indirectly: the fast flows are reused connections with autotuned app windows of about 2 MB, all on
   p1. Cancellation residue is weak evidence. Host scheduling is unmeasured. E1 and E5 decide between outer-TCP
   congestion and host starvation, with pacing as the runner-up.
3. **Q3, run the sparse-group witness first?** Run it if you like, but not as a prerequisite. This run shows zero
   saturation and a maximum count of 182/256 (153/512), so neither it nor the in-progress `tryDeepenH1PackQueue`
   change should be expected to move Fast5.
4. **Q4, per-window refusal deltas, oldest age, queue, credit, root/RX at the event?** The refusal deltas are already
   computable: W1 holds all, W2–W5 hold none (§2.6). For the rest, use `DestinationSendStats` plus the pacing
   snapshot, sampled periodically. Gauges every 250 ms beat event-only capture for stalls that last seconds.
5. **Q5, replay an impairment with a direct reference?** Yes, and run it first (E3), together with E2. If gVisor Reno
   alone reproduces additive multi-second recoveries after a small loss burst, many "variations" are fixture properties.
6. **Q6, repeatability?** Yes, three to five independent processes per arm, with statistics computed **per provider
   path** (rate against flows, deferrals and residence), not just per window.

---

## 7. Prioritized next steps

1. Fix the matrix extraction (sum or key by `ClientId`) and record the corrections in §3.1–3.4. This is minutes of
   work and removes misleading "evidence".
2. Run E1 (per-path gauges, socket/TCPInfo, per-flow series, gVisor drop and recovery counters) together with E5
   (scheduler metrics) once, on the current merged main, using the existing fixture overlay mechanism.
3. Run E3 (direct-TCP reference) and E2 (CUBIC or a BDP-sized queue) to size the fixture-induced component.
4. Based on 2–3, either adopt a fixture-fidelity change, or implement the not-yet-sent cap or socket-backlog limit
   (idea 2) and measure deferrals, handshake phases and per-path rate.
5. Separately, build the deterministic SYN-refusal test (E6) and a bounded SYN retain/retry fix. It is valuable
   regardless of the W3 outcome, especially for page-load TTFB.
6. Run E4 (constant window sizing) and E7 (fixed provider or balanced placement) as single A/Bs to close out H1c and H3.
7. Only then return to the sparse-group witness and the 64 MiB page-1 failure, using the same per-path instrumentation.

---

## Addendum: WINDOWSIZE2.md changed during this review

As I finished, the working-tree `WINDOWSIZE2.md` had grown to 259 lines / 15,131 bytes
(SHA256 `5748f365de8e1c2b2bd85ad0adc1e291a4caee9e34f95ac53e9e58a834d91b2a`, uncommitted). It adds
"Resumed post-merge investigation" at lines 207-259. The body of this review covers the committed 206-line version.
On the new section:

- **The witness and adaptive-growth correction** confirm count pressure under a *paused* callback, and that the
  growth helper reapplied an exempt byte cap. That is consistent with §6 Q3: a real latent defect, but this Fast5 run
  shows zero saturation, and, as the new text itself notes, mobile policy clears adaptive settings. Keep it decoupled
  from the W3 investigation.
- **The new 64 MiB page-1 trace** (TCP connect 4.595 s, TLS 0.913 s, request-to-first-byte 0.611 s) matches H2's
  signature: a multi-second connect, then about 1 s per handshake round trip. Check bridge SYN refusals and
  per-path residence (E1/E6) for that navigation before treating it as a separate class of failure.
