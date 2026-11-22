# Low bulk throughput: independent review of LOWBULKFLOW2

Date: 2026-11-03. Reviewer: Claude (Opus 5.5). Reviews [LOWBULKFLOW2.md](LOWBULKFLOW2.md); follows [LOWBULKFLOW-REVIEW1.md](LOWBULKFLOW-REVIEW1.md).

**Goal this review is aimed at:** Fast.com on real devices at 100 Mbps or more.

**Inputs**

- The brief.
- Source at the brief's canonical snapshot: connect `49f285f1`, SDK `eb552288`, and the server working tree for `server/proxy`.
- The team's retained evidence under `/Volumes/data/c49-native-relocated-execution.PREP.dupk0plj`:
  - `reviewer-bulk-independent.41ueajsf/current-r4-sdk32-FIVE/FIVE-REPORT.json` and `reviewer-bulk-independent.41ueajsf/CURRENT-R4-SDK32-FIVE-RESULT.json`
  - `placement-v2-execution.PREP.zlwwfpxk/04-sdk32-scoped-placement-FIVE-baseline/{FIVE-REPORT,PLACEMENT-REPORT,DIAGNOSTIC-SUMMARY}.json`
  - `n5-pixel-memory-trace.PREP.kzzi50sb/{ANALYSIS.json,REPORT.txt}`
  - `synthetic-fast-scoped-placement-v2.PREP.71hm52z0/source/*`
  - `FAST-RECEIVE-MEMSTEADY-NOTES.md`
- Ten connect-only measurements I ran on `49f285f1` (Appendix A).

**Evidence tags**

- **[M]** I measured it during this review.
- **[E]** Read from the team's retained evidence.
- **[C]** Read from source.
- **[H]** Hypothesis, not yet tested.

**Constraints honoured**

- I used no production credentials, deployed nothing, used no phone, and changed no memory target.
- The only repository change is this file. Experiments ran as `go test -overlay` scratch files outside the repository.

---

## 0. Summary

### 0.1 The answer

Bulk throughput in this stack is limited **per exit**, not per flow and not per client.

The cause is retained accounting. It charges a provider's resend budget about five bytes for every byte of payload an H1 return Pack carries. So an exit can keep only about **0.20 × B** of payload unacknowledged, where B is its retained resend budget. One exit therefore delivers at most about:

```
C_exit ≈ 0.20 × B / (RTT + ~3 ms)
```

The fixture gives each exit B = 2 MiB at a 40 ms RTT. That predicts about 76–80 Mbps per exit. The measured value is **76.5–80.0 Mbps per carrying exit in all ten runs of both five-run processes** (§2).

- The "slow first run" carried its flows on **one** exit; the later runs used 3–4.
- The instrumented process's slower run 2 (218.8 Mbps) used **three** exits; its other runs used four.
- Flows that share an exit split that exit's ceiling. One flow alone on an exit reaches the same ceiling as three or eight flows do.

**The Pixel's three Fast.com tests were each carried by a single exit:** the iOS profile's one-exit speed window (§3).

That single-exit outcome is not an accident of cold start; it is the current design.

- The mobile speed window holds exactly one exit.
- In the quality window, all Netflix CDN hosts share one affinity name (`nflxvideo.net` → `netflix.com`). That pins every concurrent Fast.com flow to one exit for 60 s.
- The synthetic fixture names its four origins `origin0.test`…`origin3.test`. Those are four different affinity groups, so the synthetic can spread across exits in a way real Fast.com cannot.

**The brief's question needs a different framing.** Comparing 200–280 Mbps synthetic with 19–40 Mbps public compares 3–4 exits at 40 ms with 1 exit at a public RTT. Each of those exits sits behind the same kind of ceiling.

**What 100 Mbps requires.** On a real device it takes either:

- **One exit that can carry 100 Mbps.** That needs η·B ≥ 12.5 MB/s × (RTT + 3 ms), where η (eta) is the payload-to-charged-byte ratio. With today's retained accounting:

  | RTT | Retained budget the exit needs |
  |---|---|
  | 40 ms | ≥ 2.6 MiB |
  | 80 ms | ≥ 5 MiB |
  | 120 ms | ≥ 7.3 MiB |

  A mobile SDK exit today has 1.0–2.7 MiB (§4).
- **Or a deliberate decision to stop pinning Fast.com to one exit.** That is safe only if Fast.com's CDN URLs are not bound to the requesting IP, which I could not establish.

### 0.2 Findings

1. **[M][C] The per-exit ceiling, isolated on `49f285f1`** (§1).

   Setup: one real `RemoteUserNatProvider`, two Transfer clients, a gVisor app stack, and 10 s runs.

   | Setup | Mbps |
   |---|---|
   | 2 MiB retained, 20 / 40 / 80 / 120 ms | 149.8 / 79.8 / 41.1 / 27.7 |
   | 40 ms, 640 KiB / 1 MiB / 4 MiB | 25.1 / 40.0 / 159.5 |
   | 40 ms, 2 MiB, retained accounting off | 350 |
   | 40 ms, 2 MiB, 1 flow (vs 8) | 79.8 |
   | 40 ms, 2 MiB, SDK32-like client | 79.8 |

   - I fitted two values on two runs: payload in flight (427 KB per 2 MiB) and a 2.8 ms delay term. That fit predicts the other six retained-accounting runs within 0.5%.
   - The custody fix holds. All three disposal paths I counted read zero: no admission failures, no group callback errors, and no unwritten packets.
   - The ceiling is now a clean in-flight cap rather than a burst/crawl.

2. **[M] The ceiling is invisible to the counters the brief relies on.**

   - The provider budget sits at 99.3–99.98% of 2,097,152 bytes from the second 0.5 s sample to the end of the run.
   - Over the same run, `ReliableAdmissionWaitDuration` stays at 0, as do timeout, selective-gap and eviction resends, every drop counter, and the callback errors.
   - Nothing in the current diagnostics says "this exit is budget-limited".

3. **[E] The synthetic five-run processes are N × the ceiling** (§2).

   - Per carrying exit, the steady 8-stream rate is 80.0, 79.6, 76.6, 76.5 and 76.8 Mbps (earlier process), and 76.6, 78.6, 76.5, 76.5 and 76.5 Mbps (instrumented process).
   - Every carrying exit wrote 86.7–104.1 MB per 10 s run at 2.66 frames per write. That holds whether it carried 1, 2, 3, 4 or 8 flows.
   - A byte balance pins down which exit carried which stream in instrumented runs 1 and 2. In run 1, two exits each carried **one** stream, about 83 MB each. The other two exits each carried **three** streams, about 26–42 MB per stream. All four exits delivered 83–93 MB.

4. **[E] The Pixel's Fast.com download went through one exit in every test** (§3.1).

   - Exit `47faee8f`, the speed window, recorded 7,780 / 14,639 / 4,948 receive commits in the three tests.
   - The only other exit with any commits in those windows, quality exit `c32dfc9d`, recorded 0 / 238 / 0.
   - The other four quality exits recorded none.
   - Each test held 8–13 concurrent requests.

5. **[C] Real Fast.com is single-exit by design; the fixture is not** (§3.2–3.3).

   - **Speed window.** `mobileSpeedWindowSize = 1` (`sdk/mobile_memory_policy.go:238`) gives one exit on the 32 MiB profile. A fixed one-exit profile turns on the single-IP affinity path (`connect/ip_remote_multi_client.go:4782-4792`).
   - **Quality window.**
     - `affinityNameForServerName` collapses names to their base domain and then applies `"nflxvideo.net": "netflix.com"` (`ip_remote_multi_client.go:4743,4767-4775`).
     - `AffinityStickyPastCap` is on and `MaxFlowsPerExit` is 16, above Fast.com's 8–13 requests (`ip_remote_multi_client.go:275-282`).
   - **Fixture.** The origins are `origin%d.test` (`synthetic_fast_native_test.go:255`), so each origin is its own affinity group.

6. **[C] On a real device, 100 Mbps is a single-exit capacity problem** (§4).

   - Read from the code, a mobile SDK exit's resend leaf is max(target × 3/70, 256 KiB): 1.03 / 1.37 / 2.74 MiB at 24 / 32 / 64 MiB targets (`sdk/device_local_provider.go:587-588`, `sdk/device_local.go:101-105`).
   - Mobile budgets are parented, which forces retained accounting (`connect/transfer.go:7137`).
   - The predicted ceiling for a 24 MiB-target mobile exit is 40 / 21 / 14 Mbps at 40 / 80 / 120 ms. The Pixel's 15–31 Mbps of TUN-level download fits that class at a public RTT. It also fits a capacity-limited exit, or an exit with the old custody defect. The current captures cannot tell these apart.

7. **[C][H] The largest code lever is how retained bytes are charged** (§1.2, L2).

   - Each retained Pack is charged **three** pool-rounded copies of its frame bytes plus an owner rounded up to 1 KiB (`connect/transfer_retained_budget.go:63-83`). Two of the three are scratch for a rewrite.
   - If real memory per retained Pack is closer to one root plus the owner, the scratch could be reserved once per writer instead of once per Pack. That would raise η from about 0.20 to about 0.45 and the per-exit ceiling about 2.2× at the same charged ceiling.
   - Measure first (T7).

8. **[C][E] The historical chunk screen is the same mechanism.**

   - The 8-frame / 8,800 B policy moves η from about 0.20 to about 0.27 (+31%). The screen measured +25.48% (80 → 100 Mbps).
   - Chunk size and the per-exit ceiling are one problem, not two.

9. **[C] Public exits are pre-fix.**

   - `49f285f1` landed today. Every public exit runs an older build.
   - On a retained-accounting exit, the old disposal path that REVIEW1 found is still live.
   - The phone result cannot reflect the custody fix, so variation like 19 → 40 → 22 Mbps may be partly that defect.

10. **[E][H] Secondary phone observations** (§3.5).

    - **TUN packet size.** Packets average 752 B in fast-1 and fast-3. That matches the provider NAT cutting each 1,448 B origin read into one full packet plus a runt (about 764 B average). The runts double the inner packet rate on Internet-origin paths, and the loopback fixture cannot show them.
    - **Uplink resends.** Selective-gap resends equal 43% of the phone's speed-route initial writes over a reliable, ordered carrier. That is unexplained, and unlikely to limit the download itself.

11. **[C] SDK settings repair coverage** (§6, Q6).
    - **Production read-modify-write callers to add.** Two production paths read the settings, change one field and set them back:
      - The hosted proxy helper runs on **every** connect (`server/proxy/proxy_device_reliability.go:28-51`).
      - iOS `DeviceRemote` re-applies its queued 45-field copy on every sync (`sdk/device_rpc.go:6310-6343,10537-10546`).
    - **A pre-existing transition defect.** Once any stored override exists, a routing-tier change made while disconnected is lost at the next connect. The window is built with the new tier (`sdk/device_local.go:5234`), and then the stored override replaces it (`sdk/device_local.go:5390-5392`).

12. **[E] Corrections to the brief's reading of its own evidence** (§7).
    - The native bridge counts are **packets, not groups**.
    - The 9,399 "control" packets offered in a download-only workload are probably teardown RSTs.
    - The instrumented run 2 explanation above.

### 0.3 What I would do first, cheapest first

| Order | What | Cost | Result if the model holds |
|---|---|---|---|
| 1 | **T1**: rename the four fixture origins under one registrable domain | Fixture-only change | Every run settles near 76–80 Mbps, carried by one exit — the phone's topology, reproduced |
| 2 | **T7**: measure real heap per retained Pack against `retainedMemoryByteCount` | Unit test, no devices | Either justifies L2 or rules it out |
| 3 | **T4**: Pixel as client pinned to the Galaxy as provider over the public platform, with the Galaxy's memory target and the Pixel-to-Galaxy RTT recorded | Both phones are on the permitted list; no deployment | Fast.com ≈ 0.20 × B_Galaxy × 8 / (RTT + 3 ms) |
| 4 | **T5**: add per-exit inner-TCP header logging and per-route RTT at the phone's TUN boundary | Client build only; no TLS interception | Identifies what limits the public speed exit |

Then pursue L1 (capacity-aware exit choice for a bulk affinity group) and L2 (reserve scratch roots per writer rather than per Pack).

---

## 1. The per-exit ceiling

### 1.1 Mechanism [C]

The chain from origin to app:

1. The provider NAT's TCP sends only within the client's advertised window and has no congestion control. It hands its return packets to Transfer through `RemoteUserNatProvider`.
2. With 49f285f1, a chunk that cannot reserve retained budget now **waits** with its custody intact (`transfer.go:8254-8266`), where it used to be disposed of.
3. That makes Transfer admission an in-flight cap. A Pack's charge is released only when the client's Transfer ACK arrives, so throughput = (payload that fits in the charged bytes) / (admission-to-ACK time).

**The charge per Pack** (`transfer_retained_budget.go:54-83`) is built in three steps:

- The frame estimate is the message bytes + 512 + 32 × frames + the contract's proto size (`retainedSendFrameByteCount`, `:117-124`).
- The estimate is rounded up to a message-pool class plus 12 bytes. The pool classes are small, packet, 4,096 and 8,192 (`message_pool.go:430`); anything larger is unpooled.
- The charge is three such roots plus an owner of sizeof(sendItem) (592) + 128 × (frames + 4) + 128, rounded up to a 1 KiB multiple.

**Pack shape:**

- The NAT segments at `DefaultMtu` = 1,100 B (`ip.go:44,512`).
- An established H1 chunk is at most 3 × 1,100 = 3,300 B (`transfer.go:1353`).
- So a full chunk is 3 packets, and a 3-packet chunk estimates to 3,908 B, which lands in the 4 KiB class.
- The fixture's exits wrote **2.66 frames and 2,908 B per write** in every run; my harness wrote 2.67 frames and 2,912 B. Both show the same shape.

### 1.2 Retained charge by Pack shape [C]

| Pack shape | Charge | TCP payload | η = payload / charge |
|---|---:|---:|---:|
| 3 × 1,100 B (full chunk) | 14,372 | 3,180 | 0.221 |
| Measured average: 2.66 frames, 2,908 B (fixture and harness) | 14,372 | ≈2,800 | ≈0.195 (fit: 0.203) |
| Phone runt pattern, 1,100 + 428 + 1,100 + 428 B (§3.5) | 14,372 | 2,896 | 0.201 |
| 8 × 1,100 B (the screen's 8 / 8,800 policy, unpooled root) | 31,812 | 8,480 | 0.267 |
| Hypothetical: one root + owner per Pack, average shape | 6,156 | ≈2,800 | ≈0.455 |
| Hypothetical: one root + owner per Pack, 8 / 8,800 shape | 12,652 | 8,480 | 0.670 |
| 3 × 1,100 B plus ≥189 B of contract (crosses into the 8 KiB class) | 26,660 | 3,180 | 0.119 |

**The class boundary is a sawtooth:** one byte past 4,096 in the estimate nearly doubles a Pack's charge. The fixture's measured ceiling matches the 4 KiB-class prediction, so its packs stay below the boundary. A public path whose contract proto exceeds about 188 B would halve its exits' ceilings. I did not measure public contract sizes.

### 1.3 Isolation measurements [M]

**Setup:**

- Harness: REVIEW1's harness at connect `49f285f1`, using `REVIEW_REMOTE_PROVIDER=1` (the real `NewRemoteUserNatProvider` return path), one provider, two Transfer clients over FIFO H1-typed links, and a 2 Gbps link.
- Client side: the gVisor app TUN, with a loopback origin.
- Runs: `GOMAXPROCS=4`, 10 s each.

**Model:** C = P / (RTT + δ), with P = 3,419 kbit (427 KB) for 2 MiB retained and δ = 2.82 ms. Runs a and d were used to fit P and δ; every other model value is a prediction.

| Run | RTT (ms) | Provider budget | Accounting | Flows | Client | Measured (Mbps) | Model (Mbps) |
|---|---:|---|---|---:|---|---:|---:|
| a | 40 | 2 MiB | retained | 8 | default | 79.84 | 79.84 (fit) |
| b | 40 | 2 MiB | retained | 1 | default | 79.80 | 79.84 |
| c | 80 | 2 MiB | retained | 8 | default | 41.10 | 41.28 |
| d | 20 | 2 MiB | retained | 8 | default | 149.82 | 149.82 (fit) |
| e | 40 | 4 MiB | retained | 8 | default | 159.50 | 159.70 |
| f | 40 | 1 MiB | retained | 8 | default | 39.98 | 39.92 |
| h | 40 | 640 KiB | retained | 8 | default | 25.11 | 24.95 |
| i | 40 | 2 MiB | retained | 8 | SDK32-like | 79.80 | 79.84 |
| j | 120 | 2 MiB | retained | 8 | default | 27.68 | 27.84 |
| g | 40 | 2 MiB | plain | 8 | default | 350.14 | (η_plain ≈ 0.89) |

**From first principles, unfitted:** the fixture should then run at 0.195–0.203 × 2 MiB × 8 / 42.8 ms = **76.4–79.6 Mbps per exit**. It measured 76.5–80.0.

**What else these runs show:**

- **One flow reaches the full ceiling (run b).** The ceiling is per exit, not per flow.
- **The SDK32-like client changes nothing (run i).** The client is not the limit.
- **Without retained accounting the same 2 MiB carries 4.4× more (run g).** The ceiling comes from the charge, not from memory.

### 1.4 What the counters show at the ceiling [M]

Run a, sampled every 0.5 s:

- **Provider budget used:** 1,733,192 at 0.5 s. Of the 19 later samples, 17 read 2,096,660 (99.98% of 2,097,152) and two read 2,082,288 and 2,090,516 (99.3% and 99.7%). The 4 MiB run sat at 4,187,176–4,193,320 of 4,194,304.
- **NAT return-queue bytes:** 2.0–2.6 MB.
- **Provider recovery, all zero:** timeout resends, selective-gap resends and eviction resends; `ReliableAdmissionWaitDuration` read 0 s.
- **Disposal, all zero:** send-pack deadline drops, no-ACK discards, admission failures, group callback errors and TUN drops. NAT-emitted vs device-injected packets matched (96,652 / 95,951; the difference is admitted-but-in-flight at teardown).

Every counter the brief lists as evidence reads clean while the exit is pinned at its cap. The gauges in L-obs (§4.3) would make this visible.

---

## 2. Re-reading the two synthetic five-run processes [E]

### 2.1 Per-run, per-exit rates

The steady-state median uses the 0.5 s samples taken after the eighth stream starts (12–16 samples per run). Per-exit MB is the change in `InitialMessageByteCount` per carrying exit (from the `provider_recovery` counters in `FIVE-REPORT.json`).

| Process | Run | Carrying exits | Whole-run Mbps | Steady median Mbps | Per exit | Per-exit MB written |
|---|---:|---:|---:|---:|---:|---|
| earlier | 1 | 1 | 77.0 | 80.0 | **80.0** | 101.9 |
| earlier | 2 | 3 | 221.6 | 238.8 | **79.6** | 92.0–104.1 |
| earlier | 3 | 4 | 272.2 | 306.4 | **76.6** | 86.7–95.2 |
| earlier | 4 | 4 | 280.6 | 306.2 | **76.5** | 88.9–99.2 |
| earlier | 5 | 4 | 281.7 | 307.2 | **76.8** | 89.8–99.8 |
| instrumented | 1 | 4 | 278.6 | 306.4 | **76.6** | 87.8–99.1 |
| instrumented | 2 | 3 | 218.8 | 235.7 | **78.6** | 91.3–103.1 |
| instrumented | 3 | 4 | 280.5 | 305.9 | **76.5** | 89.3–99.3 |
| instrumented | 4 | 4 | 281.1 | 306.2 | **76.5** | 89.1–101.0 |
| instrumented | 5 | 4 | 281.1 | 306.2 | **76.5** | 89.2–100.5 |

The developer's own notes already record "~70Mbps per observed provider" for an earlier custody run (`FAST-RECEIVE-MEMSTEADY-NOTES.md:173`). The brief does not carry it forward as the explanation.

### 2.2 Exits share their ceiling among their flows

Four of instrumented run 2's streams had single-exit SYN offers: s4 to exit 1, s5 to exit 3, and s6 and s7 to exit 2. For every exit, validated bytes run about 0.94 × the exit's written message bytes, which is IP/TCP/TLS overhead. Only one assignment of the raced streams satisfies that:

| Instrumented run 2 | Streams | Validated MB per stream | Exit total validated | Exit written | Ratio |
|---|---|---|---:|---:|---:|
| exit 1 | s0, s4 | 56.9, 40.1 | 97.0 | 103.1 | 0.94 |
| exit 2 | s2, s3, s6, s7 | 22.5, 22.3, 20.6, 20.4 | 85.8 | 91.3 | 0.94 |
| exit 3 | s1, s5 | 50.6, 40.2 | 90.8 | 96.6 | 0.94 |

- Streams on the four-flow exit got half the bytes of streams on the two-flow exits.
- Run 2 of the earlier process had the same mask (14) and nearly identical per-stream bytes: 56.0, 51.6, 23.5, 22.7, 41.8, 40.7, 20.6 and 20.1 MB.

Instrumented run 1 shows the same on a wider spread. Exits 2 and 3 each delivered about 83 MB validated (88.2 and 87.8 MB written). s2 and s3, each 83 MB, are the only single streams that size.

| Instrumented run 1 | Streams | Validated MB per stream | Exit total validated |
|---|---|---|---:|
| exit 2 or 3 | s2 | 83.3 | 83.3 |
| exit 3 or 2 | s3 | 82.9 | 82.9 |
| exit 1 | s0 (singleton offer) + two of s4–s7 | 41.7 + ~52 | ~93.8 |
| exit 0 | s1 + two of s4–s7 | 36.1 + ~52 | ~88.4 |

**A lone stream on an exit got about 74 Mbps; streams sharing an exit three ways got about 23–25 Mbps each.** Every exit delivered the same ceiling.

### 2.3 Why run one used one exit in the earlier process

The instrumented placement trace (`PLACEMENT-REPORT.json`) shows how a run can start on one exit:

- **0.48 ms.** At `run_begin`, one quality exit has effective tier 0: the exit that carried the 9.9.9.9 flow before the run. The other four quality exits and the speed exit are at tier 1.
- **0.58 ms.** s0's SYN is offered to one candidate, that exit. This is the run-one singleton the brief mentions.
- **500 ms.** Four quality exits are at tier 0, and s1's SYN gets a four-wide field. Every later SYN in that run got four candidates.

The earlier process recorded eight origin dials in run one, all from exit 3, which means eight singleton offers. Two conditions together would produce that:

- No second exit became proven before the later SYNs.
- The second flows to each of the four destinations adopted their destination's first-flow exit.

Its run one shows one more thing. The adaptive controller held at 4 streams from 1.0 s to 3.5 s before launching streams 5–8, because more streams on one capped exit did not raise throughput. The ramp's timing is a symptom of the ceiling and, in turn, moves the later SYNs.

### 2.4 Consequence for the brief's statement

The brief says the instrumented result "weakens any claim that the earlier first-run bottleneck is an unavoidable budget ceiling". Both processes show the same per-exit budget ceiling in every run. What differs is how many exits carried each run.

- With the four-name fixture, placement decides N, and N × 77 Mbps is the result.
- With real Fast.com, N is 1 by design (§3.2), so the ceiling is the result.

---

## 3. The Pixel result

### 3.1 One exit per test [E]

Data from `n5-pixel-memory-trace.PREP.kzzi50sb/ANALYSIS.json`, fast[i].nativeStrictInsideChildUnderClockModel:

| Test | Fast.com result | Speed exit `47faee8f` receive commits | Quality `c32dfc9d` commits | Other 4 quality exits | TUN rx in window | Avg TUN packet | TUN rx rate bounds | Peak parallel requests |
|---|---:|---:|---:|---:|---|---:|---|---:|
| fast-1 | 19 Mbps | 7,780 | 0 | 0 | 20.0 MB / 26,618 pkts | 752 B | 14.9–17.4 Mbps | 8 |
| fast-2 | 40 Mbps | 14,639 | 238 | 0 | 51.6 MB / 60,108 pkts | 858 B | 28.2–31.3 Mbps | 13 |
| fast-3 | 22 Mbps | 4,948 | 0 | 0 | 17.5 MB / 23,225 pkts | 752 B | 14.6–16.6 Mbps | 9 |

The analysis lists six active routes: five quality and one speed. That matches the 32 MiB profile, with a quality window of int(4 × 32/24) = 5 and a speed window of int(1 × 32/24) = 1. It is the same resident set the fixture reports.

### 3.2 Single exit by design [C]

**Window choice.** `selectWindowTypes` (`ip_remote_multi_client.go:3427-3450`) behaves like this:

- With a fixed performance profile, it returns only that window.
- Otherwise, port 443 tries quality first and then speed.

The Pixel's Fast.com flows ended up on the speed window. The capture does not record which path did it: a fixed Speed profile in the app, or the quality field filtered empty at SYN time (`ip_remote_multi_client.go:8371-8452`) with the flow falling through to speed. A one-line device check, `GetPerformanceProfile()`, settles which.

**Speed window.** The mobile speed window has exactly one exit (`sdk/mobile_memory_policy.go:238`), and a fixed one-exit profile uses a single global affinity path (`ip_remote_multi_client.go:4782-4792`).

**Quality window.** It spreads nothing for Fast.com either:

- `affinityNameForServerName` (`:4767-4775`) maps every `*.oca.nflxvideo.net` host to `nflxvideo.net`, then aliases it to `netflix.com` (`:4743`).
- A site's flows pin to one exit through `AffinityMaxTime` = 60 s.
- `AffinityStickyPastCap` keeps the site on its exit past `MaxFlowsPerExit` (16).

The SDK documents why: *"video cdns bind signed media urls to the client ip, and the cap veto was splitting exactly the sites that were busiest"* (`sdk/reliability_controls.go`, field `AffinityStickyPastCap`).

**On the 64 MiB Android profile** the multi-client's own speed window becomes int(1 × 64/24) = 2 exits. That does not by itself change the single-IP path, which is keyed to the user's performance profile (`FixedWindow()` with `FixedWindowSize == 1`), not to the memory profile. In either window, the `netflix.com` affinity group still binds to one exit.

**[H] Prediction for the pending Android 64 MiB cell:** Fast.com is still carried by one exit per test, so it gives no multi-exit gain.

### 3.3 The fixture does not model this [C][E]

The native synthetic's origins are `origin0.test`…`origin3.test` (`synthetic_fast_native_test.go:255`), whose eTLD+1 is each name itself, so they form four affinity groups. That is how the instrumented runs reached four exits, with four adoptions per run in runs 2–5, one per destination. Real Fast.com produces one group.

The fixture therefore measures a topology the product deliberately prevents for Netflix-hosted traffic. T1 is the fix.

### 3.4 What the 15–31 Mbps implies [C][H]

Under §1, a single exit delivers 0.20 × B / (RTT + 3 ms) when budget-limited. Table §4.2 gives the predicted ceiling per exit class. The Pixel's per-test TUN rates (15–31 Mbps) are consistent with any of three exits, and the current capture cannot tell them apart:

- **A mobile SDK exit on a 24–32 MiB target at roughly 55–120 ms of RTT.** Ceiling 14–35 Mbps.
- **An exit limited by its own upstream capacity.** Residential uploads of 10–40 Mbps are common, and a provider sends a client's download on its upload.
- **A pre-fix, retained-accounting exit with REVIEW1's disposal defect.** Burst/crawl would make results vary from run to run (19 → 40 → 22).

The phone's evidence can identify the class (T5):

- Only one of the six exits published diagnostics, so the speed exit's build is unknown.
- Every public exit is older than `49f285f1`.

### 3.5 Secondary phone observations

**Runt segments [E][H].**

- **The numbers.** fast-1 and fast-3 average 752 B per TUN packet, and fast-2, at a higher rate, averages 858 B. Packets per receive commit are 3.4 / 4.1 / 4.7.
- **The mechanism.** The NAT's origin read loop packetizes each `socket.Read` result separately into ≤1,100 B packets (`ip.go:6060-6136`, `DataPackets` at `:6132`). A 1,448 B Internet segment read alone becomes a 1,100 B packet and a 428 B runt, averaging (1,448 + 80) / 2 = 764 B per packet. Two such reads fill one 3,300 B chunk with four frames.
- **Why it matters.** Runts roughly double inner packets per byte, which doubles TUN writes, kernel TCP work and phone ACKs. They barely change η, because chunks are bounded by bytes.
- **Why the synthetic misses it.** The fixture's origin is on loopback, so its reads are large and runts do not appear.
- **The check.** A packet-size histogram at the phone's TUN, or T8.

**Uplink selective-gap resends [E].**

- The speed route's outbound counters show 8,368 selective-gap resends against 19,274 initial writes, plus 97 reorder-suspected and 28 ACK-pending preemptions.
- H1 is a reliable, ordered byte stream, so real loss on it is unlikely.
- The likely causes are a gap rule firing on cross-lane or cross-connection reordering (eight logical uplink lanes on mobile H1), or refusal at the provider's receive side.
- Uplink overhead is real: in fast-1, wlan0 tx was 4.17 MB against 0.74 MB of inner TUN tx. But the download data does not wait on these resends.
- **Check:** compare the same counter in the synthetic client, where the carrier is lossless by construction. Near zero there means a public-path cause.

---

## 4. Reaching 100 Mbps on a real device

### 4.1 Requirement [C]

The table assumes 100 Mbps of validated payload. It gives the payload in flight one exit needs, and the budget that implies at three values of η.

| RTT | Payload in flight | B at η = 0.20 (today) | B at η = 0.45 (L2) | B at η_plain = 0.89 |
|---|---:|---:|---:|---:|
| 40 ms | 537 KB | 2.56 MiB | 1.14 MiB | 0.58 MiB |
| 80 ms | 1,037 KB | 4.95 MiB | 2.20 MiB | 1.11 MiB |
| 120 ms | 1,537 KB | 7.33 MiB | 3.26 MiB | 1.65 MiB |

### 4.2 Predicted single-exit ceilings by exit class [C, predictions]

| Exit class (code path) | Resend budget B | Accounting | 40 ms | 80 ms | 120 ms |
|---|---|---|---:|---:|---:|
| Fixture exit | 2.00 MiB | retained | 78 | 40 | 27 |
| SDK mobile, 24 MiB target | 1.03 MiB | retained (parented, `transfer.go:7137`) | 40 | 21 | 14 |
| SDK mobile, 32 MiB target | 1.37 MiB | retained | 54 | 28 | 19 |
| SDK mobile, 64 MiB target | 2.74 MiB | retained | 107 | 55 | 37 |
| SDK desktop, 20 MiB default target, unparented | 0.86 MiB | plain | ~149 | ~77 | ~52 |

All ceilings are in Mbps. B is max(target × 3/70, 256 KiB) (`sdk/device_local_provider.go:587-588`), and the provider's own parent budget (`sdk/device_local_transfer_memory.go:38,72`) must also have room.

These are upper bounds. Several things push them lower:

- **Exit upstream capacity and CPU** can bind first.
- **Other clients** on the same exit share its budget.
- **Pre-fix exits** add disposal crawls.

Under current accounting, **only a desktop-class exit, or a 64 MiB mobile exit at about 40 ms, can carry Fast.com at 100 Mbps.** The desktop class assumes the desktop build keeps the 20 MiB default target; that is unverified.

### 4.3 Levers, in the order I would pursue them

**L1. Choose the exit for a bulk affinity group by measured capacity** (client only; ships with the app).

- Fast.com is pinned to whichever exit its `netflix.com` group binds to first, so the choice of that exit is the whole result.
- Estimate each exit's aggregate ceiling C_p from its own backlogged delivery (Q2), and bind a new bulk group to the exit with the highest expected share.
- On the speed window, allow replacing its one exit when the measured C_p is below the target.
- **Effect:** moves the phone from a median exit to the best exit available. How much that helps depends on the spread of exits, which T5 measures.
- **Risk:** low. It changes only which exit a fresh group binds to.

**L2. Stop charging scratch roots per retained Pack** (connect; reaches public exits only as they update).

- **Proposal:** reserve the decode/rewrite scratch once per serializing writer, sized to the largest chunk, and charge each retained Pack one root plus its owner.
- **Effect:** η rises about 2.2×, from about 0.20 to about 0.45, and so does every retained-accounting exit's ceiling at the same budget.
- **Precondition:** T7 must show the scratch is not actually resident per Pack. If it is not, the budget's ceiling on real memory is unchanged and the memory gates hold.
- **Tests per CODESTYLE:**
  - Negative and positive tests for a rewrite under a full budget, so a still-live owner is never released and no memory is borrowed.
  - Concurrent rewrites on two sequences sharing a parent.

**L3. Pool-aligned H1 chunk policy** (connect).

- **Effect:** +18–31% η. The 6-frame / 8 KiB-class chunk gives +18% and stays pooled; the 8 / 8,800 chunk gives +31% but is unpooled.
- **How to qualify:** T7 plus the Q5 method.
- **Smaller than L2, and it stacks with it.**

**L4. Relax site pinning for Netflix bulk** (product decision).

- **Precondition:** verify whether Fast.com's OCA target URLs are bound to the requesting IP. I searched and found no public documentation either way.
  - The SDK comment says some video CDNs bind URLs to the client IP.
  - Fast.com fetches its targets from `api.fast.com`, a different affinity group (`fast.com`) than the OCA hosts.
- **Effect if the URLs are not IP-bound:** about ×N for N exits.
- **Effect if they are bound:** this breaks Fast.com. Do not ship without a check that counts 403s and aborted targets.

**L5. Mobile provider budget shares** (SDK).

- Mobile exits get 3/70 of their target. Raising that costs memory on provider phones and conflicts with their gates.
- Consider it only after L2, which buys the same in-flight payload without more charged bytes.

**L6. NAT runt coalescing** (connect).

- Carry a sub-segment remainder into the next read when more data is pending.
- Halves inner packets on Internet-origin bulk flows.
- Needs an interactive-latency guard, for example only when the client window is not the limit and the remainder flushes within about 1 ms.

**L-obs. Observability** (connect and SDK; ship regardless).

- **Per sequence:**
  - retained bytes used and limit
  - payload bytes in flight
  - fraction of time at the cap
  - Transfer RTT
- **Per exit on the client:** delivered payload rate and the route's RTT.
- **Uses:** these make "budget-limited", "capacity-limited" and "loss/crawl" distinguishable in production, and they let L1 estimate C_p.

---

## 5. Cheapest discriminating tests

| Test | Change | Prediction if the per-exit model holds | What falsifies it |
|---|---|---|---|
| **T1** Fixture topology | Rename the four origins under one registrable domain, e.g. `o0`–`o3.oca.fastfixture.test`, or names under `nflxvideo.net` resolved locally. Nothing else changes. | All five runs carried by one exit, steady ≈ 76–80 Mbps; whole-run ≈ 75–78 | Runs 2–5 still reach 3–4 exits |
| **T2** Fixture budget | Each exit's retained send limit 2 → 4 MiB | Per-exit steady ≈ 150–160 Mbps; run one ≈ 155 if single-exit | Per-exit rate stays ≈ 80 |
| **T3** Fixture RTT | Access RTT 40 → 80 ms | Per-exit ≈ 40 Mbps | Per-exit rate stays ≈ 80 |
| **T4** Controlled public exit | Pixel as client pinned to the Galaxy as provider (both permitted devices). Record the Galaxy's memory target and build, the Pixel-to-Galaxy RTT through the platform, and Fast.com ×3. | Fast.com ≈ 0.20 × B_Galaxy × 8 / (RTT + 3 ms); for example, 24 MiB target at 80 ms gives ≈ 21 Mbps. Doubling the Galaxy's leaf, or L2, about doubles it. | No dependence on B or RTT |
| **T5** Phone instrumentation | Client build only, no TLS interception. Per-exit inner-TCP header log at the TUN boundary (seq, ack, length, window, flags, time) plus each route's Transfer RTT. | Budget-limited: rate × RTT ≈ constant ≈ η·B, no holes, RTT not inflated. Capacity-limited: rate constant, RTT inflates under load. Pre-fix disposal: holes, one-segment-per-RTT repair, 1 s replays. Client-limited: small or zero advertised window. | Whichever signature is absent |
| **T6** Direct control | Fast.com on the Pixel, same Wi-Fi, VPN off | Gives the access headroom; expected well above 100 Mbps on a good AP | — |
| **T7** Memory truth | Unit test: retain N Packs of each shape in §1.2; compare heap and pool bytes per Pack against `retainedMemoryByteCount` | About 1 root + owner, roughly 6 KB real against a 14.4 KB charge for the 4 KiB class | Real bytes ≈ charge; then L2 is wrong |
| **T8** Runts | Fixture origin writes 1,448 B segments over a 1,500 B-MTU shaped path instead of loopback | Average TUN packet ≈ 764 B; frames per Pack ≈ 4 | Average stays ≈ 1,090 B |

T1–T3 and T7 need no devices. T4–T6 use only the two permitted phones. None needs a deployment.

---

## 6. Answers to the brief's peer-review requests

### Q1. Cold placement

- **Tiering** explains the singleton field at the **first** SYN: the trace shows one tier-0 exit at 0.58 ms (§2.3).
- **All eight on one exit** also needs one of two things: no second exit proven before the later SYNs, or the repeat destinations (s4–s7 to the same four IPs) adopting their first flow's exit.

**The distinguishing event, per SYN:**

- `fresh_lookup.InitialAdoption` set, with no `candidate_offer`, means affinity inheritance.
- A `candidate_offer` with one candidate while two or more healthy exits exist, only one of them at minimum effective tier, means a tier singleton.

The instrumented capture already records both.

**First treatment to try: a race floor.** While fewer than k exits are proven, admit healthy tier-1 exits to a fresh SYN race up to k candidates; the race result proves them.

- **Expected in the current four-name fixture:** run one at about 300 Mbps.
- **For real Fast.com** this changes only which exit the `netflix.com` group binds to, not how many. That is why L1 matters more for the 100 Mbps goal.

### Q2. Shared capacity

**Use an exit-level aggregate, not a per-flow peak.** A flow's historical useful-ACK peak measures C_p / n_p at the time it was measured. In instrumented run 1 (§2.2), the same exits' ceilings showed up as one 74 Mbps stream or as three 23–25 Mbps streams.

**Estimate:**

- C_p = a decayed maximum of the exit's **aggregate** delivered rate over intervals in which it was backlogged, meaning payload in flight at its cap or data waiting.
- The expected share for a new bulk flow or group is C_p / (n_p + 1).
- n_p counts only backlogged bulk flows, those with outstanding bytes at least 1 MSS in the last 2 × RTT. That keeps short or idle flows from being counted against an exit.

**Preventing oscillation:**

- Decide only when a flow or group is born; never migrate a live flow.
- Require an expected gain of at least 1.5×.
- Decay C_p slowly.
- If the advertised bandwidth prior disagrees with a measured C_p, the measurement wins.

**Falsifiable check on existing data.** For instrumented run 2 the model predicts per-stream rates of 38.5 / 38.5 / 19.3 Mbps on the 2-, 2- and 4-flow exits. Measured: s4 37.7 Mbps and s6 19.4 Mbps over their active 8.5 s.

### Q3. Public versus synthetic

The minimum evidence, all without TLS interception or deployment:

1. **Which exit carried each Fast.com flow, and which window type and reason chose it.** The receive-commit data already shows one exit.
2. **That route's Transfer RTT from the client's own send side.**
3. **The inner-TCP view at the TUN (T5):** in flight, holes, retransmissions and the advertised window.
4. **A controlled exit of known class at a known RTT (T4)**, to calibrate η·B on the real path.
5. **A direct-link control (T6).**

With these, the four candidate limits separate cleanly:

| Limit | Signature |
|---|---|
| Budget ceiling | rate × RTT is constant at about η·B; no RTT inflation |
| Exit capacity | flat rate; RTT inflates under load |
| Loss and crawl | holes, plus repair at one segment per RTT or 1 s replays |
| Local admission or refusal | the client's own refusal and handoff counters, and its advertised window |

The class fingerprint from (2) × observed rate:

| Exit class | Payload in flight (about) |
|---|---:|
| 24 MiB mobile | 210 KB |
| 32 MiB mobile | 280 KB |
| Fixture-like | 410 KB |
| Desktop plain | 750+ KB |

### Q4. ACKs and mixed native groups

**No, the existing counters do not suffice.**

- **Units.** The fixture's bridge counters are **packets**, not groups (`synthetic_fast_bridge_test.go:75-99`).
- **Batch-level only.** They classify a refusal only by the composition of its batch. As the file itself notes, aggregate acceptance cannot identify which member was refused.
- **"Mixed" is not mostly ACKs.** Offered types were 150,169 pure ACK, 200 data and **9,399 control** (no payload, SYN/FIN/RST). That is far more control packets than 40 connections' handshakes need. **[H]** They are mostly RSTs the app stack sends at run teardown, against data that arrives for cancelled sockets. If so, most of the 4,489 refused packets in mixed batches happen after each measurement window and do not affect throughput.

**Cheapest check for that hypothesis:** timestamp each refusal against the measurement end, and classify refused packets by type.

**A trace that would prove recovery after suffix disposal:**

1. **Per-member admission outcome.** The accepted-prefix length is enough if admission is prefix-ordered; otherwise a bitmap. Record it with each member's flow tuple, seq, ack, length, flags and window.
2. **Refused pure ACK.** Find the first later *admitted* ACK on the same flow with ack ≥ the refused ack, and the delay to it.
   - Count tail refusals separately: refusals with no covering ACK before the exit's replay timer. These are the expensive ones, about 1 s each, per the brief's own ACK test.
3. **Refused data or control.** Find the app stack's retransmission of the same seq range, its admission, and the provider NAT's receipt of that range. The NAT logs by flow and seq.
4. **Downstream.** The NAT's ACK covering the range, injected at the TUN.

Join every stage on (flow, seq range). Materialization, route write and peer ACK already have Pack identities; carry the member indices through them.

### Q5. Chunking at equal retained roots

**The screen's gain is predicted by η** (§1.2): the 8 / 8,800 policy moves η from 0.203 to 0.267, +31% against +25.48% measured.

**To test it at an equal charge:**

1. **Deterministic.** Compute charge per payload byte for each policy from `retainedMemoryByteCount`, using the measured frame-size distribution (2.66 frames per write today). Use the phone's runt mix too.
2. **Equal η·B.** Run one exit at a fixed RTT with B scaled so η·B is equal across policies. The prediction is equal throughput. If the larger chunk still wins, the remainder is per-Pack CPU, not accounting.
3. **Memory truth.** Run T7 per policy. The 8,800 B chunk exceeds the 8,192 pool class, so every chunk is a temporary unpooled allocation; count allocations per MB and the GC cycles.
4. **Fairness and tails.** Chunks are per logical group, meaning one flow's return batch. A larger chunk delays other flows only by its serialization time (about 0.7 ms at 100 Mbps). Measure TTFB for a small flow concurrent with a bulk group at both policies.

### Q6. SDK repair coverage

**Field list.** The six fields are exactly right. connect's `ReliabilitySettings` has 51 fields and the SDK's has 45; the difference is `LightClassifier`, `QuarantineReentryRamp`, `ScoredAffinityDonor`, `UplinkStalenessMaxHold`, `WindowOutcomeDeadline` and `WindowOutcomeRebuildDeadline`.

**Production read-modify-write callers to include:**

1. `sdk/routing_tier.go:157-166` `applyRoutingTierWithLock`, for `SetRoutingTier` and catalog Load. It only takes effect while connected.
2. `server/proxy/proxy_device_reliability.go:28-39`, called from `ConnectChanged(true)` (`:48-51`). This runs on **every** connect of every hosted proxy device. It turns the first window's settings into a stored complete override, which is re-applied to every later window (`sdk/device_local.go:5390-5392`).
3. `sdk/device_rpc.go:6310-6343` and `:10537-10546`: iOS `DeviceRemote` keeps the 45-field struct queued and re-applies it on every sync and every extension restart.
   - After the repair it merges onto fresh defaults, which keeps hidden fields correct.
   - It still **reasserts stale exposed values**. For example, routing-tier knobs changed on the extension side after the snapshot are overwritten.
4. The developer menus: `android/.../DeveloperViewModel.kt:541-545` and `apple/.../ReliabilityStore.swift:501-510`.

**Transitions to add:**

- **Tier change while disconnected after an override exists (pre-existing defect).**
  - The next window is built with the new tier (`sdk/device_local.go:5234`).
  - The stored override, which holds the previous tier's `ScoredPlacement`, `RewardInstrumentation`, `PlacementHysteresisPct`, `PlacementDemoteConsecutive` and `QuarantineDampening`, then replaces it (`:5390-5392`).
  - The tier change is lost. Hosted proxy devices hit this after their first connect.
  - **Recommendation:** keep the tier as a layer separate from the developer override, rather than writing it into the override.
- **Merge base while disconnected.** `GetReliabilitySettings()` returns nil, so the repair must merge onto the stored override if one exists, else onto the next window's baseline. It must never merge onto zero.
- **Reset after a merged override.** The classifier is reinstalled through `classifierChanged` (`connect/ip_remote_multi_client.go:3739-3745`). Assert the object exists again, not only the flag.

**Lock composition.**

- The repair reads the live multi-client and stores under `stateLock`; it applies outside the lock.
- Two read-modify-write callers can interleave and lose an update, for example `ConnectChanged` and a tier change. That is the pre-existing race the brief notes.
- Write the regression so that it *shows* the lost update, not one that passes by timing luck.

**Experiment confound.** Until the repair ships, any device A/B that changes a reliability field through the public setter (for example `MaxFlowsPerExit`) also turns off the classifier and both outcome deadlines, and lifts the uplink staleness hold. Use the repaired setter, or the private hook, for any T4/L1 experiment.

### Q7. Remaining custody hypotheses

**Nothing in the current data requires a credit cycle.**

- **A credit cycle predicts** intervals with the budget at its cap and **zero** delivered bytes across two or more RTTs.
- **Neither data set shows one:**
  - My runs: budget at cap, steady progress, minimum 0.5 s sample 4,241 KiB in run a.
  - The fixture's 8-stream samples: one dip to 198.7 Mbps in earlier run 3, never zero.
- **The per-exit ceiling is fully explained by η·B / RTT.**

Deprioritize this for the 100 Mbps goal. If it stays on the list, define the detector as above and run it on the existing fixture with the L-obs gauges.

---

## 7. Corrections and clarifications to LOWBULKFLOW2

1. **Native refusals.** "4,489 mixed groups" means 4,489 refused **packets** in batches that were not pure-ACK-only. Those batches include 9,399 TCP control packets offered in a download workload (§6 Q4).
2. **"The slow first-run pattern did not reproduce."** The instrumented run one had four exits proven by its second SYN (§2.3). Its slow run was run 2, with three exits, at 218.8 Mbps; the per-exit rate is unchanged (§2.1).
3. **"Not an unavoidable budget ceiling."** The budget ceiling is per exit and appears in every run of both processes. Placement only sets how many exits carry a run (§2.4).
4. **"Comparing either median directly with the public Pixel median does not isolate a cause."** Agreed, and one difference dominates: **3–4 carrying exits against 1**. It is structural, because real Fast.com is single-exit by design and the fixture's origin names are not (§3).
5. **"Chunk sizing is worth studying separately."** It is the same mechanism as the per-exit ceiling (η, §1.2).
6. **Counters.** The brief's typed counters read zero both when nothing is wrong and when an exit is pinned at its budget for an entire run (§1.4).
7. **Public exits.** None runs `49f285f1`. On retained-accounting public exits the disposal defect is still live, so the phone result cannot reflect the custody fix.
8. **Reliability A/B on devices.** Any public-setter change of reliability fields before the SDK repair also changes four non-zero hidden defaults (§6 Q6).

---

## 8. Residual risks and adjacent issues

- **NAT recovery is unchanged.** It still replays one segment per partial ACK, with no SACK and a fixed 1 s timer (REVIEW1 F3). On a public exit, any loss — pre-fix disposal, an exit's own uplink, a Wi-Fi hiccup under a full window — still becomes a crawl of one segment per RTT.
- **Pool-class sawtooth.** A few bytes of contract or framing growth can double a Pack's charge (§1.2). The fix commit already handles reservation *failure* at the boundary. The *throughput* cost of crossing it is untested.
- **The CDN-pinning design trades throughput for correctness.** Any change that spreads a site across exits needs an explicit 403 and aborted-target check (L4).
- **An exit's budget is shared by all its clients.** Public exits serving other users give each client less than §4.2. L1's C_p estimate must come from live measurement, not from the exit's class.

---

## Appendix A. How the isolation runs were made

**Source.** Connect `49f285f1`, unmodified. The three overlay test files are byte-identical to [LOWBULKFLOW-REVIEW1.md](LOWBULKFLOW-REVIEW1.md) Appendix A.4 (`zz_review_retained_test.go`, `zz_review_workload_test.go`, `zz_review_bulk_test.go`). I checked this by extracting the appendix blocks and comparing them byte for byte.

**Build:**

```sh
cd connect
go test -c -overlay <dir>/overlay.json -o <dir>/connect-head.test .
```

**Run** (one line per configuration; 10 s each):

```sh
GOMAXPROCS=4 REVIEW_BULK=1 REVIEW_REMOTE_PROVIDER=1 REVIEW_SECONDS=10 \
  REVIEW_STEPS=provider REVIEW_RTT_MS=<20|40|80|120> REVIEW_FLOWS=<1|8> \
  REVIEW_PROVIDER_BUDGET_KIB=<640|1024|2048|4096> [REVIEW_NO_RETAINED=1] \
  <dir>/connect-head.test -test.run 'TestReviewBulkBisect$' -test.v -test.count=1 -test.timeout=120s
```

**Variants:**

- **Run i** used `REVIEW_STEPS=provider,recv,send`, which adds REVIEW1's SDK32-like client receive and send settings.
- **Every run** used the real `RemoteUserNatProvider` return path with `DisableSecurityPolicyWithStats` and a `ReturnSendObserver` that counts completed return packets.
- **The links** were 250 MB/s (2 Gbps) each way, with the RTT split evenly across them and 4,096-packet / 8 MiB queues.

**Retained charge probe.** `TestReviewRetainedChargePerPack` on `49f285f1` reproduces REVIEW1's per-Pack charges, for example 14,372 bytes for every 2–3-frame Pack in the 4 KiB class. §1.2 applies the same function to the fixture's 1,100 B packet shape.

**Evidence extraction.** The per-run and per-exit numbers in §2 and §3 were read with short Python scripts from the JSON files listed under Inputs:

- The steady-state medians use the samples with `launched_streams == 8`.
- Per-exit bytes are the before/after differences of `provider_recovery[i].InitialMessageByteCount`.
- Stream-to-exit attribution joins each flow's `FirstConnection` tuple to the `ProviderWire` SYN-ACK records. The byte balance in §2.2 then resolves the raced streams.
