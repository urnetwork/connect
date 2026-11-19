# Low bulk throughput: independent review of LOWBULKFLOW3

Date: 2026-11-03. Reviewer: Claude (Opus 5.5). Reviews [LOWBULKFLOW3.md](LOWBULKFLOW3.md); follows [LOWBULKFLOW2-REVIEW1.md](LOWBULKFLOW2-REVIEW1.md).

**Goal this review is aimed at:** Fast.com on real devices at 100 Mbps or more, within the 32 MiB (iOS profile) and 64 MiB (Android) gates.

**Inputs**

- The brief.
- Source at connect `49f285f1` (unmodified), the gVisor fork the module replaces (`../gvisor`), and the server `connect/perfvar` working tree for the fixture's link and TUN construction.
- The team's retained evidence under `/Volumes/data/c49-native-relocated-execution.PREP.dupk0plj`. Every SHA-256 prefix in the brief's evidence map matched.
  - The three same-origin `FIVE-REPORT.json` files.
  - `h1-eight-current-sdk.PREP.4aj71tet/source/transfer.go`, the H1-eight production candidate. It differs from HEAD only in the chunk constants (`transfer.go:1346-1355`, `:3592`).
  - `retained-invariants-v4-execution…/ACTUAL-RESULTS.json`, `h1-retained-shape-proof…/{READY.json,REPORT.txt}`, `sethead-wire-rewrite-audit…/REPORT.md`.
  - `n5-fast64-offline-analysis…/REPORT.txt`; `n5-pixel64-explicit-cdp…/body/fast-1.json`.
- 24 connect-only measurements I ran on `49f285f1`, plus nine failed attempts at a receiver-less memory test (Appendix A).

**Evidence tags**

- **[M]** I measured it during this review.
- **[E]** Read from the team's retained evidence.
- **[C]** Read from source.
- **[H]** Hypothesis, not yet tested.

**Constraints honoured**

- No credentials, no deployment, no device operations, no memory target changed.
- The only repository change is this file. Every experiment ran through `go test -overlay` with scratch files outside the repository.
- One experiment charges only the retained root (§2.4). It is a measurement-only bound, not a proposal; §4 explains why it would be unsafe as a product change.
- Another developer's `-race` suite was running on this host. I interleaved the arms (HEAD, H1-eight, HEAD, H1-eight) so that background load affects both alike.

---

## 0. Summary

### 0.1 Answer to the headline question

In both the component harness and the fixture, one exit's sustained H1 throughput is set by how many payload bytes the provider's retained budget can keep unacknowledged:

```
C_exit ≈ B × η × (app bytes / message bytes) / (RTT + δ)
```

- **B** is the retained budget.
- **η** is message bytes per charged byte, computed for the provider's actual chunk shapes.
- **δ** is about 2.5 ms in my harness.

One δ predicts every 40 ms run here within 2.5%: twelve runs covering HEAD and H1-eight chunks, each under the three-root charge and under a one-root bound (§2).

Three things follow for the brief's question:

1. **The H1-eight gain is +22–24% like-for-like, not 33.6%** (§1).
   - The original arm's 71.90 Mbps median includes two tests in which a newly chosen exit ramped linearly from about 45 to 82 Mbps over about 6 s. Both then sat at the same ~80 Mbps plateau as the warm exits.
   - Warm exit against warm exit: 79.4 → 97.3 Mbps steady (+22.4%), and 78.1 → 96.3 Mbps whole-run (+23.2%).
   - My harness: 81.9 → 103.9 Mbps (+26.8%). The corrected charge model predicts +26.6%.

2. **Packing is nearly exhausted for full-MTU groups** (§3).
   - Under the three-root charge no packing can exceed η = 1/3.
   - For a 16-original group, the best split that fits the 16 KiB envelope is 8+8 (9+7 ties).
   - On public streams with runt packets, the eight-frame cap gains only about 4% over HEAD. The Pixel averaged 752 B per TUN packet.

3. **The remaining large lever is where rewrite scratch is charged, and it can be funded without borrowing** (§4).
   - `setHead` rewrites only `sendItems[0]`, on the sequence goroutine (`transfer_recovery_selection.go:363-378`).
   - At steady state every retained 3-frame Pack held exactly one pooled root. I counted 127–131 roots for about 130 Packs, 519–528 for 524, and 298–300 for 300.
   - No bulk run made a head rewrite.
   - Charging the retained root alone raises one exit at 40 ms and 2 MiB from 79.8 to 183.3 Mbps (HEAD chunks) and 254.1 Mbps (H1-eight chunks). The model predicts both.
   - A per-sequence high-water reserve of 2 × the largest root (4× for legacy encoding) keeps the full-budget repair guarantee and would approach those numbers.

**For real devices** the same model gives the following single-exit ceilings, in Mbps. Each cell is full-MTU / runt-heavy streams; B is max(target × 3/70, 256 KiB).

| Exit | Chunks and accounting | 40 ms | 80 ms |
|---|---|---:|---:|
| Mobile, 32 MiB target (1.37 MiB) | HEAD | 56 / 54 | 29 / 28 |
| | H1-eight | 69 / 57 | 36 / 29 |
| | Per-sequence reserve, H1-eight chunks | 175 / 139 | 90 / 71 |
| Mobile, 64 MiB target (2.74 MiB) | HEAD | 112 / 109 | 57 / 56 |
| | H1-eight | 139 / 113 | 71 / 58 |
| | Per-sequence reserve, H1-eight chunks | 349 / 277 | 180 / 143 |

- At 40 ms, 64 MiB exits already clear 100 Mbps; 32 MiB exits need the per-sequence reserve.
- At 80 ms, only 64 MiB exits with the reserve clear it.
- These are upper bounds. They assume δ = 2.5 ms, and that the exit's own upstream, its CPU and its other clients do not bind first.
- Public exits gain anything provider-side only as they update.

**The two invalid Android64 runs and the start of the 11 Mbps replay share one failure pattern, separate from throughput** (§6):

- In each, the page and API requests completed while the OCA downloads made no measurable progress. The replay shows zero OCA bytes for 9 s; Fast 2 and Fast 3 stayed at display 0.
- One candidate cause is cheap to test. The affinity alias table has no `fast.com → netflix.com` entry, so the page and API lineage and the OCA download lineage can leave from different exits.
- That cannot happen when Fast.com runs in the 32 MiB profile's one-exit speed window, which is where the earlier Pixel captures placed it. All three 32 MiB runs were valid.

### 0.2 Findings

1. **[E] Tests 3 and 4 of the original arm are ramps, not a lower ceiling** (§1.2).
   - The newly chosen exits (p3, p2) climbed from about 45 to 82 Mbps over 6 s, then held about 80 Mbps like the warm exit p1.
   - The slope is one outer-TCP MSS per RTT.
   - [H] The fixture's outer H1 carrier is gVisor TCP, which defaults to Reno (`gvisor/pkg/tcpip/transport/tcp/protocol.go:565`). A carrier whose congestion window was left below the exit's budget ceiling regrows at exactly that rate.

2. **[M] H1-eight in isolation** (§2.1).
   - 103.8 / 103.9 Mbps against HEAD's 81.7 / 82.2 at 40 ms with 2 MiB.
   - 196.2 / 52.7 Mbps at 20 / 80 ms.
   - One flow gives 103.8 Mbps, the same as eight.
   - The budget sat at 2,087,880 of 2,097,152 bytes, with zero resends.

3. **[C][M] Corrected charge model** (§2.2).
   - A 16-original return group splits 3+3+3+3+3+1 under HEAD. That is 2.667 frames per Pack; the measured value is 2.664–2.666.
   - The one-frame Pack is charged 8,228 B, so HEAD's η is 0.218 rather than the 0.20 I used before.
   - H1-eight's 8+8 gives η 0.276, a predicted ratio of 1.266. Measured: 1.268.

4. **[M] Scratch roots are reserved, never resident, in bulk** (§2.3).
   - Each retained 3-frame Pack holds one pooled root.
   - No run had a timeout resend, so no head rewrite occurred.

5. **[M] One-root bound** (§2.4): 183.3 / 188.9 Mbps with HEAD chunks (×2.29–2.30) and 254.1 / 259.9 Mbps with H1-eight chunks (×2.45–2.49), at 40 ms and 2 MiB.

6. **[C] Rewrite scratch is needed for at most one item per sequence** (§4.1).
   - Rewrites touch only the oldest unacknowledged item, at most once, on the sequence goroutine.
   - A failed rewrite exits the sequence (`transfer_recovery_selection.go:370-375`). That is why the scratch must stay funded.
   - Today's charge is released when an ACKed item is returned, even if a carrier still holds a share of its root (`transfer.go:14129-14131`).

7. **[C][M] Packing** (§3).
   - Fewer chunks always cost less, unless a small chunk lands just under a pool class.
   - 12+4 costs 12% more than 8+8: its 4-frame suffix lands in the 8 KiB class.
   - With runts, the frame cap is the wrong control.

8. **[M][C] H1-eight memory churn** (§2.5, §7).
   - It allocates 46% more bytes and runs 46% more GC cycles per useful MB.
   - Each 8-frame Pack makes about two unpooled 8–16 KiB allocations.
   - On the phone's receive path, both `Framer.Read` (`message_framer.go:103`) and AEAD open (`transfer_encrypt.go:321`) allocate above the 8 KiB class for an 8-frame Pack.

9. **[E] The 9.079 s plateau is a zero-OCA-byte start, ended by Fast.com's own cancel and retry** (§6.1).
   - Until 13.19 s the page had received exactly 225,164 B from its single h2 endpoint (12 responses) plus one 1,734 B http/1.1 response.
   - The four endpoints that later delivered 22.4 MB had delivered nothing.

10. **[C][H] The `fast.com` lineage can differ from the `netflix.com` lineage** (§5.1).
    - `domainAffinityAliases` (`ip_remote_multi_client.go:4725-4745`) maps `nflxvideo.net`, `nflximg.net` and `nflxso.net` to `netflix.com`, but not `fast.com`.

11. **[C] Refused heads back off to 8 s** (§6.3).
    - Resend intervals double per send up to `MaxResendInterval` = 8 s (`transfer.go:8658-8678`, `:891`).
    - Under receive-leaf pressure, a refused head stalls every flow on its sequence for up to 8 s while its successors fill the shared leaf.

12. **[C] Zero priors and lineage age** (§5.2).
    - The donor check passes whenever the advertised prior is ≤ 0.
    - A lineage keeps its oldest creation time, so a retry cannot extend a pin past 60 s.

13. **Corrections** to the brief and to my previous review are in §9.

### 0.3 What I would do next

| Order | What | Cost | Result if the reading here holds |
|---|---|---|---|
| 1 | Test the lineage hypothesis on the next Android64 replay. Record which exit carried `api.fast.com` and which carried the OCA SYNs, plus CDP resource timing per request. | Host collector, plus native per-flow exit attribution if not already logged | Zero-byte OCA phases coincide with a lineage mismatch |
| 2 | Add the alias `fast.com → netflix.com` and rerun Android64 × 3 | One line in the client | No display-0 runs and no zero-byte start |
| 3 | In the fixture, log TCPInfo (cwnd, ssthresh, srtt) for each window client's outer connection at every test start | Test-only | Ramp tests start with cwnd below the budget ceiling |
| 4 | Prototype the per-sequence scratch reserve behind its full-budget rewrite tests (§4.3) | Connect, test-first | About ×2.3–2.5 per-exit ceiling at the same budget |
| 5 | Phone memory, HEAD against H1-eight: `MessagePoolUnpooledCounts()` and the runtime/metrics in §7.2 at the existing sampling points | Telemetry | Decides whether H1-eight survives the 32 MiB gate |
| 6 | Run the planned ABBA comparison, but control exit warmth: same exit, warm, both arms | Fixture | Removes the ramp confound from the primary statistic |

---

## 1. The three same-origin processes, re-read [E]

### 1.1 Per test and per exit

"Written" is the carrying exit's Δ`InitialMessageByteCount` × 8 / 10 s, from `provider_recovery`. "Steady" is the median of 0.5 s samples at the run's maximum launched streams. H1-eight test 5 launched four streams.

| Arm | Test | Carrying exits | Whole-run Mbps | Steady Mbps | Written per exit, Mbps | Frames / bytes per write |
|---|---:|---|---:|---:|---|---|
| Original | 1 | 5 | 296.18 | 338.4 | 52.4, 65.2, 67.6, 56.2, 72.6 | 2.65–2.66 / 2,889–2,909 |
| Original | 2 | p1 | 78.43 | 79.7 | 83.2 | 2.664 / 2,909 |
| Original | 3 | p3 | 65.96 | 72.1 | 69.9 | 2.665 / 2,910 |
| Original | 4 | p2 | 64.87 | 71.6 | 68.7 | 2.665 / 2,909 |
| Original | 5 | p1 | 77.85 | 79.2 | 82.3 | 2.665 / 2,909 |
| Affinity 0 | 1–5 | 4 each | 269.5–279.1 | 304.3–308.5 | 68.0–81.1 | 2.645–2.666 / 2,888–2,911 |
| H1-eight | 1 | 4 | 334.32 | 372.8 | 81.8–95.1 | 7.63–7.94 / 8,330–8,664 |
| H1-eight | 2–5 | p0 | 95.98–97.03 | 96.7–97.5 | 101.9–103.2 | 7.879–7.899 / 8,604–8,624 |

In tests 2–5 of all three arms, every carrying exit recorded:

- zero timeout resends, zero selective-gap writes and zero eviction resends;
- zero `ReliableAdmissionWaitCount`.

### 1.2 Tests 3 and 4 are ramps on the newly chosen exit

Validated Mbps per 0.5 s:

```
test 2 (p1): 76 80 74 80 80 81 80 80 81 83 77 80 75 82 78 82 79 79 79
test 3 (p3): 45 48 48 54 57 61 62 66 69 71 73 77 82 79 81 78 80 82
test 4 (p2): 42 46 47 52 54 58 64 66 63 70 73 78 80 79 79 81 79 82
test 5 (p1): 80 80 81 78 79 78 83 75 79 79 79 80 79 82 78 80 83 77
```

**What the series show:**

- Tests 3 and 4 end at the same ~80 Mbps as tests 2 and 5. The per-exit ceiling is the same in all four tests.
- The ramp is linear. It gains +3.4 Mbps per 0.5 s validated, which is about +7.2 Mbps/s written.
  - At a 40 ms RTT, that is 1.46 KB of extra in-flight data per RTT.
  - That is one MSS for a 1,500 B outer MTU.
- Idle time does not predict the ramp.
  - p3 (idle for one test) and p2 (idle for two) ramped.
  - p1 in test 5, also idle for two tests, started at full rate.
  - What differs is history. p1 had carried test 2 alone at its ceiling. p3 and p2 had only shared test 1 at 56–68 Mbps each.
- No test in the H1-eight arm ramped. The same exit carried tests 1–5.

**Mechanism [H].**

- The fixture runs H1 over gVisor TCP on both ends of its packet link. The edge side is `perfvar/route_test.go:459-464` and `CreateTun`; the TUN stack is built in `connect/tun.go:253-312`.
- gVisor's default congestion control is Reno (`protocol.go:565`). Reno adds one MSS per RTT in congestion avoidance.
- I found no idle restart in gVisor's sender.
- The managed peaks report `PlatformTransportUsedCount = 6`, which I read as one H1 connection per window client.
- The run-1 edge stack recorded 143 retransmits. An exit whose carrier cwnd was left below its budget ceiling would regrow at exactly the measured slope.

**Check.** Log TCPInfo (cwnd, ssthresh, srtt) for each window client's edge-to-device connection at each test start. The prediction: ramp tests start with cwnd × MSS / RTT below about 84 Mbps written, and the ramp ends when the retained budget binds.

**Why it matters beyond the fixture.** On a real device, each exit's download also rides a small number of outer TCP connections per hop, shared by all of that exit's flows. After any loss, their regrowth limits the exit, whatever the retained budget allows (§10).

### 1.3 Like-for-like H1-eight effect

| Statistic | Original, warm exit (tests 2, 5) | H1-eight (tests 2–5) | Change |
|---|---:|---:|---:|
| Steady median, mean | 79.43 | 97.25 | +22.4% |
| Whole-run, mean | 78.14 | 96.30 | +23.2% |
| Exit written Mbps, mean | 82.75 | 102.28 | +23.6% |

- **Prediction.** §2.2's model predicts +24–25% for the fixture, if its contract proto is 150–200 B. Each unpooled H1-eight root carries the contract bytes; HEAD's class-rounded roots absorb them.
- **Why the 33.6% figure overstates the gain.** The brief's 33.6% compares H1-eight with a median that includes the two ramp tests.
- **The planned ABBA.** It should hold exit warmth equal, for example by running a warm-up test on the same exit before each arm. Otherwise the primary statistic, the median of tests 2–5, inherits whichever arm happened to switch exits.

---

## 2. Isolation on `49f285f1` [M]

### 2.1 H1-eight in the harness

**Setup:**

- REVIEW1's harness, unchanged: the real `RemoteUserNatProvider` return path, two Transfer clients over FIFO H1-typed links at 2 Gbps, a gVisor app TUN and a loopback origin.
- `GOMAXPROCS=4`; each run 10 s.
- Arm B uses the team's H1-eight candidate `transfer.go`, byte-identical (SHA-256 prefix `1fab6298b99edf37`). Arm A is HEAD.

| Run | Chunks | RTT ms | Flows | Accounting | Mbps | Frames / bytes per write | Budget used |
|---|---|---:|---:|---|---:|---|---|
| A1, A2 | HEAD | 40 | 8 | retained | 81.67, 82.17 | 2.666 / 2,912 | 2,096,660 in most samples |
| B1, B2 | H1-eight | 40 | 8 | retained | 103.78, 103.93 | 7.99 / 8,730 | 2,087,880 in most samples |
| B3 | H1-eight | 20 | 8 | retained | 196.19 | 7.99 / 8,733 | 2,087,880 |
| B4 | H1-eight | 80 | 8 | retained | 52.72 | 7.98 / 8,717 | 2,083,488–2,089,344 |
| B5 | H1-eight | 40 | 1 | retained | 103.77 | 7.96 / 8,694 | 2.07–2.10 MB, two dips to 1.90 and 1.93 MB |
| B6 | H1-eight | 40 | 8 | plain | 357.66 | 8.00 / 8,736 | 1.84 MB, not at cap |

**What these runs show:**

- **Fit.** C = P / (RTT + δ) gives P = 552 KB of app payload and δ = 2.49 ms. It predicts 53.5 Mbps at 80 ms; B4 measured 52.72 (1.5% off).
- **One flow equals eight**, as with HEAD.
- **Every run was clean:** zero timeout, selective-gap and eviction resends; zero admission failures; zero `ReliableAdmissionWaitDuration`.

### 2.2 The charge model, corrected

**Group shape.**

- A provider socket-return group holds at most 16 originals (the brief's sixteen-original bound in `ip.go`).
- The chunker fills each Pack up to the policy's frame and byte limits (`transfer.go:3460-3487`):
  - HEAD's established H1 limit is 16 frames or 3,300 B (`:1346`, `:1353`). A 16 × 1,092 B group becomes 3+3+3+3+3+1: 2.667 frames per Pack against a measured 2.664–2.666.
  - H1-eight's limit is 8 frames or 8,800 B, which gives 8+8. Measured: 7.99 in the harness and 7.88–7.90 in the fixture.

**Charge per Pack.** From `transfer_retained_budget.go:54-83` and `transfer_send_prepared_item.go:42-47`, with c the contract proto bytes.

| Chunk | Frame estimate | Root | Owner | Charge | Message bytes | η |
|---|---|---:|---:|---:|---:|---:|
| 3 × 1,092 B | 3,884 + c | 4,108 (for c ≤ 212) | 2,048 | 14,372 | 3,276 | 0.228 |
| 1 × 1,092 B | 1,636 + c | 2,060 | 2,048 | 8,228 | 1,092 | 0.133 |
| 8 × 1,092 B | 9,504 + c | 9,516 + c, unpooled | 3,072 | 31,620 + 3c | 8,736 | 0.276 (c = 0) |
| 16-original group, HEAD | | | | 80,088 | 17,472 | 0.218 |
| 16-original group, H1-eight | | | | 63,240 + 6c | 17,472 | 0.276 (c = 0) |

**Prediction against measurement** (harness, c = 0):

- Ratio: 0.2763 / 0.2182 = 1.266, against 1.268 measured.
- Absolute, with δ = 2.49 ms and the measured app/message ratio (0.949–0.950): HEAD 81.8 Mbps against 81.9; H1-eight 103.2 against 103.9.

**Correction to my previous review.** It charged every Pack 14,372 B. One Pack in six is a 1-frame Pack charged 8,228 B, so HEAD's η is 0.218 rather than 0.20. The +31% I predicted for 8 / 8,800 B is +26.6%. The team's earlier screen measured +25.48%, which the corrected model fits better.

### 2.3 What a retained Pack actually holds [M]

**Setup:** steady state, ACKs delayed 1 s and data 1 ms, so retained items wait at the sender rather than in the link; 8 flows; 10 s. Roots are `MessagePoolOutstandingCount()` − `MessagePoolPacketOutstandingCount()`, which leaves the 4 KiB and 8 KiB classes.

| Run | Budget used | Retained Packs (from charge) | of which 3-frame | 4 KiB and 8 KiB roots outstanding | Live heap after GC |
|---|---:|---:|---:|---:|---:|
| HEAD, 2 MiB | 2.088 MB | 156.5 | 130.4 | 127–131 | 12.4–12.7 MB |
| HEAD, 8 MiB | 8.38 MB | 628 | 524 | 519–528 | 17.3–17.7 MB |
| HEAD, one-root charge, 2 MiB | 2.093 MB | 360 | 300 | 298–300 | 14.8–15.2 MB |

**What this shows:**

- **One pooled root per retained 3-frame Pack.** The two scratch roots the charge prepays are not resident.
- **No head rewrites.** No run here, at 40 ms or at 1 s, had a timeout resend. The brief reports zero timeout rewrites for the fixture's body providers too.
- **Other memory grows with in-flight data.** The process's live heap rose 5.1 MB for 471 more retained Packs (2 → 8 MiB), about 10.8 KB per 2.9 KB Pack. Most of that is not the Pack: the provider NAT's retransmission copies and the client's uplink ACK items also grow with in-flight data. A higher per-exit ceiling therefore raises memory outside the retained budget as well (§7.3).
- **Not separated.** These counters do not separate the owner and metadata part of a Pack (`sendItem`, ACK records).

### 2.4 The one-root bound [M]

This is a measurement-only switch: charge root + owner, with no scratch. 40 ms, 2 MiB, 8 flows.

| Chunks | Three-root Mbps | One-root Mbps | Ratio | Model |
|---|---:|---:|---:|---:|
| HEAD | 79.78–82.45 | 183.27, 188.88 | ×2.29–2.30 | 187.3 |
| H1-eight | 103.78–104.39 | 254.05, 259.87 | ×2.45–2.49 | 259.5 |

Ratios pair each one-root run with the three-root run from the same binary and test: 183.27 / 79.78 and 188.88 / 82.45 for HEAD; 254.05 / 103.86 (mean of B1 and B2) and 259.87 / 104.39 for H1-eight.

At the 1 s ACK delay the switch gave 6.30 against 2.68 Mbps (×2.35) for HEAD and 8.77 against 3.38 Mbps (×2.59) for H1-eight.

**This switch is unsafe as a product change.**

- A head rewrite at a full budget would fail `reserveFrameRewrite` (`transfer_retained_budget.go:88-101`).
- A failed `setHead` exits the sequence (`transfer_recovery_selection.go:370-375`).
- The bound measures what a correctly funded reserve (§4) would approach.

### 2.5 Allocation per delivered MB [M]

**Setup:** 40 ms, 2 MiB; 8 s measured after a 2 s ramp; one process holding provider, client, NAT and app stack; encryption off.

| | HEAD | H1-eight | HEAD, one root | H1-eight, one root |
|---|---:|---:|---:|---:|
| Mbps | 82.45 | 104.39 | 188.88 | 259.87 |
| Allocated bytes per delivered MB | 3.15 MB | 4.59 MB | 3.42 MB | 4.54 MB |
| Allocated objects per delivered MB | 13,358 | 9,777 | 15,566 | 10,640 |
| Unpooled takes per delivered MB | 15.3 | 255 | 15.3 | 255 |
| 8–16 KiB objects per delivered MB | 0 | 240 | 0 | 240 |
| GC cycles per delivered MB | 0.194 | 0.284 | 0.178 | 0.243 |
| GC cycles per second | 2.0 | 3.7 | 4.2 | 7.9 |

- **Per useful MB, H1-eight costs 46% more** in allocated bytes and in GC cycles.
- **About two unpooled 8–16 KiB allocations per Pack** (240 per MB at about 120 Packs per MB).
- **Common to every arm:** the 15 objects per MB above 16 KiB.
- **GC CPU is small here, at ≤ 0.5% of four Ps**, because the live heap is 12–20 MB. A phone running near its limit is a different regime (§7).

---

## 3. Packing (Q2) [C][M]

η below is message bytes per charged byte at c = 200 B, modern encoding, from the charge function above (`packing.py`, Appendix A). "One-root" columns show the same split under §4's per-sequence reserve, which charges root + owner per Pack.

| Group | HEAD (16 fr / 3,300 B) | H1-eight (8 fr / 8,800 B) | 12 fr / 13,200 B | Fewest chunks ≤ 13,200 B | One chunk | One-root: HEAD / H1-eight / fewest |
|---|---|---|---|---|---|---|
| 16 × 1,092 B (fixture) | 0.218 (3+3+3+3+3+1) | 0.271 (8+8) | 0.241 (12+4) | 0.271 (8+8) | 0.290, exceeds 16 KiB | 0.501 / 0.683 / 0.683 |
| 16 alternating 1,100 / 428 B (runt pattern) | 0.213 (4×4) | 0.221 (8+8) | 0.253 (12+4) | 0.275 (16) | 0.275 | 0.496 / 0.542 / 0.696 |
| 10 × 1,092 B | 0.213 (3+3+3+1) | 0.234 (8+2) | 0.280 (10) | 0.280 (10) | 0.280 | 0.484 / 0.576 / 0.726 |
| 8 × 1,092 B | 0.203 (3+3+2) | 0.271 (8) | 0.271 | 0.271 | 0.271 | 0.473 / 0.683 / 0.683 |

1. **Every chunk pays a fixed cost.** Each chunk costs about 4.7–5.2 KB on top of its payload: three × (512 + 12 + 32 per frame), plus the owner rounded up to 1 KiB, plus class rounding. So fewer chunks always cost less, unless a smaller chunk happens to land just under a pool class. Callback cost sits in the owner term, which is per chunk, so it favours fewer chunks too.
2. **Full-MTU groups.** Under three roots η stays below 1/3 for any packing. For 16 full-MTU originals the best split that fits the envelope is 8+8; 9+7 ties at 63,240 + 6c. Only a single 16-frame chunk does better (6–7% less charge), and 17.5 KB does not fit 16 KiB.
3. **12+4 costs about 12% more than 8+8.** Its 4-frame suffix lands in the 8 KiB class: 26,660 B of charge for 4,368 B.
4. **On public paths the frame cap is the wrong control.**
   - The Pixel's 752 B average TUN packet fits the NAT cutting each 1,448 B read into 1,100 + 428 B (LOWBULKFLOW2-REVIEW1 §3.5).
   - Eight such frames hold only about 6.1 KB, which lands in the 8 KiB class. There, H1-eight is only +4% over HEAD.
   - A byte cap alone (16 frames, 8,800 B) does worse: it splits 11+5 with an 8 KiB-class suffix, η 0.207.
5. **The rule I would test:**
   - Use the fewest chunks under an envelope-safe byte limit L, split evenly, with the frame cap at the callback storage (16).
   - With L = 8,800 B this equals H1-eight on full groups.
   - It gains 20–24% over H1-eight on runt-heavy groups and on groups of 9–12 originals, but only when L covers the whole group (about 12.3–13.2 KB). That needs the full-proof bound that the team's shape-proof tests are built to establish.

**Answer to "would another split beat 8+8".**

- For full-MTU 16-original groups under the current charge: no.
- For partial and runt-heavy groups: yes, if L can be raised with a proven full-proof bound.
- Before choosing, measure the real distribution of group sizes and frame sizes. The fixture's loopback origin produces only the full-MTU case.

---

## 4. Scratch: where it is needed and how to fund it once (Q3, Q4)

### 4.1 Code facts [C]

**When a rewrite happens:**

- **Retry rewrites touch only the oldest item.** A Pack is rewritten as a head only when it is the oldest unacknowledged item (`sendItems[0]`), is being resent, and is not yet a head (`transfer_recovery_selection.go:363-378`).
- **Contract-missing requests are the one exception.** An explicit receiver contract-missing request also rewrites (`transfer.go:11707-11741`).
- **Both run on the sequence goroutine** and operate on its resend queue.
- **An item is promoted once:** `item.head = true` afterwards.
- **So at most one item per sequence is being rewritten at any moment.** The per-item prepay (two extra roots for modern encoding, four for legacy, `transfer_retained_budget.go:68-75`) funds an event that can involve at most one item per sequence.

**Why the scratch must stay funded:** a failed `setHead` exits the sequence (`transfer_recovery_selection.go:370-375`).

**What else holds memory per write:**

- Encryption is applied per write. `SealOuterFrame` allocates a separate outer root and shares it with the carrier (`transfer_encrypt.go:338-360`). Rewrites operate on the plaintext TransferFrame.
- The carrier receives a read-only share. `replaceSendItemFrame` returns only the sequence's reference (`payload_owner_ledger.go:173-182`).
- When an ACKed item is returned, `messagePoolReturn` drops the sequence's reference and releases the item's whole reservation in the same step (`transfer.go:14129-14131`). An old carrier share that outlives the ACK, which the team's V4 controls demonstrate, is therefore uncharged today as well.

### 4.2 A per-sequence reserve

**Rule:**

- Charge each retained item its root plus owner.
- Each sequence that holds retained items also holds a reserve S = k × max root, plus the full-contract growth bound. k is 2 for modern encoding and 4 for legacy.
- Total per sequence = Σ(rootᵢ + ownerᵢ) + k × max root.
  - Since max ≤ Σ, this is never more than today's Σ(3 rootᵢ + ownerᵢ), and it equals today's charge when the sequence holds one item.

**Mechanics:**

- Admitting an item whose root exceeds the current maximum reserves the item and the reserve's growth in one `TryReserve`. If that fails, the item is not admitted, and the raw frames stay queued exactly as today.
- S is held at its high-water mark until the sequence's retained set empties, then released.
- A rewrite draws from S. The draw covering the old root is held until the old root's last share is returned, which needs a release hook on the final reference, not merely `replaceSendItemFrame`.

| Obligation | How it is funded |
|---|---|
| Concurrent rewrites | One at a time per sequence (goroutine-serialized, oldest item only). Each sequence has its own S; there is no global pool. |
| Old carrier share outliving replacement or ACK | The draw is held until the last share returns. This covers a case today's per-item charge does not. |
| Encrypted outer copies (one per write, held until written) | Verify whether the H1 writer's pending bytes are already charged to the platform transport budget. If they are, nothing changes. If not, include writer depth × max outer root in S. |
| Full-contract growth | `reserveFrameRewrite` still reserves growth atomically. On failure, defer the rewrite: keep the old wire and retry at the next interval rather than exiting the sequence. |
| Fallback to a full decode at a saturated budget | Funded by S (k roots). The wire-preserving rewrite is not needed for the guarantee. |
| Cancellation | Draws return when their roots' last references drop. S is released after the last draw and item. Siblings are unaffected. |
| Unrelated owners | S is charged to the sequence's own leaf and parent, like its items. A full parent blocks new items, never a reserve that a sequence already holds. |
| Head-of-line blocking | Only that sequence's next rewrite can wait on a held draw, and rewrites are at least one resend interval apart. Initial sends and other sequences never wait on S. |

**Costs:**

- **One sequence.** For H1-eight chunks, S is about 2 × 9.6 KB plus contract headroom, about 1% of a 2 MiB budget.
- **A provider leaf shared by many active sequences:** S × the number of sequences, for example about 0.4 MB for 20 sequences.
  - The rule never charges more than today.
  - The saving only appears for sequences that hold many items, which is exactly the bulk case.

**Expected effect:** §2.4's ×2.29–2.30 (HEAD chunks) and ×2.45–2.49 (H1-eight chunks) per-exit ceiling at the same budget, less about 1%.

### 4.3 Tests (CODESTYLE)

1. At a full budget, a head rewrite drawn from S makes progress with no new reservation and no sequence exit.
2. An old carrier share held across a rewrite:
   - the next rewrite on that sequence waits;
   - the sibling sequence progresses;
   - the accounting identity holds.
3. Cancellation with an outstanding draw releases exactly once.
4. Full-contract growth beyond S at a full budget:
   - the rewrite is deferred;
   - the old wire, its ACK authority and custody are intact;
   - there is no sequence exit.
5. Legacy encoding with k = 4 at a full budget.
6. A parent shared by two sequences, one at cap: the other's rewrite proceeds from its own S.
7. At every quiescent point, budget used equals Σ(root + owner) + Σ S.

### 4.4 The wire-preserving rewrite (Q3)

**Its value depends on whether scratch is amortized:**

- **With S in place, it adds little throughput.**
  - It shrinks S from two roots to one per sequence, about 10 KB.
  - It removes decode copies on a path that ran zero times in every bulk run here and in the fixture's body providers.
- **Without S, it allows a two-root per-item charge.** That gives η 0.304 for HEAD and 0.395 for H1-eight, against 0.501 and 0.683 with S, and it carries the semantic risk the team's audit lists.

**If it is built anyway, add to the audit:**

- A differential test against the receiver's actual decode path, including duplicate submessages that the receiver merges, rather than generic protobuf equality.
- The fallback must stay funded by S.
- Encryption is applied per write to the plaintext TransferFrame, so the scanner need not touch AEAD. Still test that a rewritten frame round-trips through `SealOuterFrame` and open, including across the 8 KiB class boundary.

**Recommendation:** amortize first and defer the scanner.

---

## 5. Fresh-flow policy (Q5, Q6)

### 5.1 A gap in the existing same-egress design [C][H]

**The gap [C].**

- `domainAffinityAliases` collapses `nflxvideo.net`, `nflximg.net` and `nflxso.net` to `netflix.com` (`ip_remote_multi_client.go:4743-4745`). There is no `fast.com` entry.
- Fast.com's page and its target-list API (`api.fast.com`) therefore form one lineage, and its OCA downloads (`*.oca.nflxvideo.net`) another.
- A lineage is bound when its first flow picks an exit, and later flows inherit it (`ip_remote_multi_client_affinity_time.go:101-137`). In a window that offers more than one candidate, the two lineages can bind to different exits.

**Why it matters [H].**

- Fast.com generates its target list for the client that asks.
- If the downloads leave from another exit, they ask targets chosen for a different egress. That is the class of binding the alias table exists to protect.
- The failure need not be a 403. It can be a target that never answers that egress, so the page waits and then cancels.
- Verify both points from the CDP collector: the `api.fast.com` response's client and target fields, HMAC'd, together with the egress each OCA flow used.

**Consistent with the device results:**

- The 32 MiB profile's one-exit speed window cannot produce a mismatch, and the earlier Pixel captures placed Fast.com there. All three 32 MiB runs were valid.
- The 64 MiB windows can. Two of three runs were display-0 timeouts, and the valid replay began with 9 s of zero OCA bytes (§6).

**Policy I would recommend:**

1. **Alias `fast.com → netflix.com`.** It strengthens same-egress for one service and spreads nothing.
2. **Keep ordinary inheritance, and fix concentration by making the single exit faster (§4) rather than by spreading.**
   - Current metadata cannot prove that two providers share an egress.
   - A wrong guess fails silently.
3. **If spreading is ever wanted for a service, make it a per-service opt-in backed by evidence.**
   - The CDP collector can test Fast.com directly. Force the `api.fast.com` and OCA lineages onto different exits in a debug build, then read response statuses and first-byte times from CDP. No TLS interception is needed.

**The product tradeoff, stated plainly:**

| Option | Gain | Risk |
|---|---|---|
| Spreading | About ×N on multi-stream bulk tests | Silent failure for signed or IP-scoped sessions |
| A faster single exit (per-sequence reserve) | About ×2.3–2.5 | Changes no egress |

Only the second is safe with the metadata available today.

### 5.2 Zero priors and recent ACK evidence (Q6) [C]

**Today's behaviour:**

- The donor checks pass whenever the advertised prior is ≤ 0 (`ip_remote_multi_client_affinity_performance.go:546-548`, `:586-588`).
- The live check uses the donor flow's peak bucket rate (`:115-125`), a per-flow lifetime peak.
- A lineage keeps the oldest creation time in its chain and is never refreshed (`ip_remote_multi_client_affinity_time.go:113-137`, `:176-183`). A Fast.com retry storm therefore cannot hold a pin past 60 s.

**An estimator that avoids both failure modes in the question:**

- **Per exit, not per flow.** Sum the ACKed bytes of all the exit's flows in each bucket. A per-flow peak measures the exit's capacity divided by however many flows were sharing it.
- **Saturation evidence.** Count a bucket only when the exit had two or more concurrent bulk flows and its aggregate did not rise by more than 10% when the latest one joined. Only then is the aggregate the exit's capacity rather than its demand.
- **Unknown is not zero.** Without such evidence, use the median of the window's known estimates (shrinkage), neither "allow" nor zero.
- **Recency.** Use a decayed maximum with a half-life of about 30 s for spare capacity. Keep a slow peak only as an upper bound, never as current capacity.
- **Short sessions.** Keep the staged floor: at least 256 KiB in a bucket spanning at least 250 ms. Because the estimate is aggregate, one short flow cannot define an exit.
- **Use.** Bind new lineages, and choose the speed window's member, by estimate / (n + 1) with 1.5× hysteresis. Never migrate a live flow.

This picks the best exit available. It cannot raise any exit above its own ceiling.

---

## 6. The physical observations (Q7)

### 6.1 What the CDP replay shows [E]

Per endpoint (HMAC prefixes):

| Endpoint | Protocol | Responses | Data bytes |
|---|---|---:|---:|
| `5567f511` | h2 | 12 | 225,164 |
| `4ae687cf` | http/1.1 | 1 | 1,734 |
| `51aaee04` | http/1.1 | 3 | 0 |
| `52c86032` | http/1.1 | 1 | 0 |
| `6f851406` | http/1.1 | 22 | 13,570,050 |
| `1aa74466` | http/1.1 | 19 | 5,261,313 |
| `d6996978` | http/1.1 | 12 | 1,902,593 |
| `3e7e7815` | http/1.1 | 8 | 1,689,601 |

**Timeline:**

- **4.112 s.** 17 requests, 15 completed, 2 active. The total is 226,898 B, exactly 225,164 + 1,734.
- **4.1–13.2 s.** No further bytes.
- **12.183 s.** Two requests fail as cancellations and two new requests appear.
- **13.191 s.** 20 requests, 3 active.
- **14.197 s.** +804,865 B, then 10–13 Mbps to the end.

**Reading:**

- During the plateau, the four endpoints that later delivered 22.4 MB had delivered nothing.
- The plateau is the first OCA requests never delivering a byte, ended by the page's own cancel-and-replace. It is a first-byte failure at the start of the download, not a pause in a running transfer.
- Android64 Fast 2 and Fast 3 show the same outer signature:
  - 90,536 and 94,094 completed bytes, about the size of the page and API responses;
  - 21 and 26 cancellations;
  - display 0 for 90 s.

### 6.2 Candidate mechanisms and how to tell them apart

| Mechanism | Explains | Signature in captures | Deterministic fixture scenario | Falsifier |
|---|---|---|---|---|
| **M1** Target and egress mismatch: the `fast.com` and `netflix.com` lineages on different exits | (a) display-0; (b) zero-byte start; why 32 MiB runs were valid | OCA SYNs leave from a different exit than `api.fast.com`. Bytes start only when a replacement shares the API's exit. | An owned "issuer" origin binds a token to the egress that fetched it and blackholes downloads arriving through another provider. Run with and without the alias. | A mismatch in successful runs, or none in failing runs |
| **M2** Refused head plus resend backoff | (a) Fast 1's 53.5 s and Fast 2's first 20 s | Leaf near full; the refused head is retried at intervals that double to 8 s; stalls of up to 8 s + RTT, then recovery | Pause the client TUN consumer for 1–3 s during a 65 Mbps test with a 10 MiB leaf; time the first new byte after resume | Recovery within about one RTT of resume, or a stall far beyond 8 s |
| **M3** Exit sequence stopped while its flows stay bound | (a) after 25 s | No arrivals from that exit, no refusals, leaf at 0 | Force the provider's sequence to exit mid-transfer, for example through a failing head rewrite; measure client recovery | New flows on that lineage make progress |
| **M4** First SYN to an exit that cannot reach the target | (b) | SYN offered, no SYN-ACK, Android SYN retries; the page cancels at about 8 s | An owned origin unreachable from one provider in the race field | The handshake completes and then stalls |
| **M5** Page or UI completion fault | (a) | CDP shows network progress while the display stays 0 | Not needed: CDP data received against the display | CDP shows no bytes |

**Observations that separate these, without a device-code change:**

- **Host CDP collector.** Record, per request:
  - resource timing (DNS, connect, TLS, send, headers received);
  - the time of the first data received;
  - the endpoint HMAC;
  - the HMAC'd client and target fields of the `api.fast.com` response.

  "Never connected" points to M1 or M4; "connected, no body" points to M2 or M3.
- **Native side.** Log the exit at SYN time, the SYN-ACK time and the first-payload time per flow, joined on the HMAC'd destination.

### 6.3 Android64 Fast 1: what the receive-leaf numbers mean [E][C]

- **How much data that is.** 10,478,864 of 10,485,760 B is about 1.3 s of a 65 Mbps download held at the client. Packs wait there either behind a missing predecessor or for native/TUN consumption.
- **Why the stalls get longer.** The provider retries each refused arrival on the item's timer, which doubles per send up to 8 s (`transfer.go:8658-8678`; defaults at `:883-896`).
  - A refused head stalls every flow on its sequence for up to 8 s.
  - Meanwhile its successors keep filling the shared leaf.
  - That is a pressure loop.
- **How to tell the two causes apart.** Record the leaf's composition (waiting for a predecessor vs ready but not delivered) and the native TUN write latency.
- **What M2 does not explain.** It can explain Fast 1's 53.5 s and Fast 2's first 20 s. It does not explain zero arrivals after 25 s unless the exit's sequence never resumed (M3) or the new flows were going somewhere that never answered (M1).

---

## 7. Memory qualification (Q8)

### 7.1 How the batching gain can fail the 32 MiB profile with admission unchanged [C][M]

1. **Unpooled churn on the phone's receive path.**
   - An 8-frame Pack is 9.1–10.2 KB encrypted.
   - `Framer.Read` allocates `MessagePoolGet(messageLen)` (`message_framer.go:103`).
   - Open allocates `MessagePoolGet(plaintextLen)` (`transfer_encrypt.go:321`).
   - Both exceed the 8 KiB class, so both are fresh heap allocations (`message_pool.go:1222-1228`).
   - At 100 Mbps that is about 1,450 Packs/s and about 28 MB/s of garbage. HEAD's 3–4 KB Packs stay in the pooled 4 KiB class.
2. **Heap-goal overshoot.**
   - Garbage counts toward the heap until swept.
   - The iOS-profile peak is already 31.18 of 32 MiB.
   - Holding that peak needs more GC cycles: +46% per useful MB in my harness. If GC falls behind, the sampled peak rises.
3. **Size classes and spans** (Go 1.27.3, `internal/runtime/gc/sizeclasses.go`).
   - A 9,116 B buffer lands in the 9,472 B class (57,344 B spans of 6 objects).
   - 9,636 and 9,668 B land in 9,728 (49,152 B spans of 5).
   - 10,224 B lands in 10,240 (40,960 B spans of 4).
   - Churn through few-object spans shows up as `/memory/classes/heap/unused:bytes`, not as live heap.
4. **Provider phones pay the same on the send side:** an unpooled root per Pack, plus an unpooled `SealOuterFrame` copy per write.
5. **A higher per-exit ceiling raises other in-flight memory too.** NAT retransmission copies and uplink ACK items grow at unchanged budgets (§2.3). On the phone, the uplink ACK items are the part that grows.

### 7.2 Measurements before retention

Take these on the phone, per test, in both arms, at the existing sampling points:

- **Unpooled counts.** `MessagePoolUnpooledCounts()` deltas (`message_pool.go:1238`). The counter exists; the current reports do not include it.
- **runtime/metrics:**

  | Group | Metrics |
  |---|---|
  | Allocation | `/gc/heap/allocs:bytes`, `/gc/heap/allocs:objects`, `/gc/heap/allocs-by-size:bytes` (8–16 KiB buckets) |
  | GC | `/gc/cycles/total:gc-cycles`, `/gc/heap/goal:bytes` |
  | Heap classes | `/memory/classes/heap/objects:bytes`, `/memory/classes/heap/unused:bytes`, `/memory/classes/heap/free:bytes`, `/memory/classes/heap/released:bytes` |
  | CPU and pauses | `/cpu/classes/gc/total:cpu-seconds`, `/sched/pauses/total/gc:seconds` |
- **GC settings.** The configured `GOMEMLIMIT` and `GOGC` on the device.
- **Native memory.** Android PSS and RSS of the VPN process, sampled alongside.
- **Normalization.** Report per delivered MB and per Pack. Report peak objects and peak (objects + unused) separately.

**Mitigation to measure if churn is the failure.** Add a bounded 16 KiB pool class sized to the H1 envelope, so that `Framer.Read` and open reuse buffers. A free-list cap of a few hundred KiB keeps the retained pool footprint bounded.

### 7.3 What the per-sequence reserve changes

- **The retained budget still bounds the retained bytes**, because the measured residency is one root per Pack.
- **Total memory still grows with throughput.** At a fixed budget the reserve raises payload in flight about 2.3×, and NAT and client in-flight memory grows with it.
- **Qualify on devices as well:** the same per-test metrics, on a provider phone and on a client phone.

---

## 8. Answers to the brief's questions

### Q1. Is retained payload efficiency the dominant concentrated-path ceiling?

**Yes, for the component and for the fixture's warm exits.** The evidence:

- The budget sits at its cap.
- Throughput scales with B, 1/RTT and η.
- The model predicts H1-eight's gain to 0.2% in the harness and to about 1–2 points in the fixture.
- Removing the scratch roots from the charge gives the predicted ×2.29–2.30 / ×2.45–2.49.

**The fixture adds two effects on top:** the outer-carrier ramp (§1.2), and a per-exit rate 3.5–6.5% below the harness's, consistent with 1.5–2.4 ms more release delay.

**A bounded measurement that separates the six factors:**

| Factor | What to record | Signature if it binds |
|---|---|---|
| Item charge | Per Pack at `prepareSendItem`: frames, message bytes, contract bytes, charge; plus budget used | Budget at cap, and rate = B × η / T_release |
| Contract size | `proto.Size(sendContract.contract)` distribution and class crossings | η drops in steps at class boundaries |
| Allocator class | `cap(transferFrameBytes)` class, unpooled counts, allocs-by-size | Charge ≠ class × roots; heap unused grows |
| CPU service | Raise B at fixed RTT | If the rate does not scale with B, CPU or the writer binds |
| Useful-ACK timing | T_release per Pack (ACK time − admission time); δ = T_release − RTT | δ grows with load |
| Receiver credit | Client advertised window, leaf occupancy, refusals | Budget not at cap while the client window is closed |

### Q2. Packing

§3. 8+8 is optimal for full-MTU groups. The "fewest chunks under an envelope-safe byte limit" rule gains on runt-heavy and partial groups, if the full-proof bound allows a larger limit.

### Q3. Protobuf-preserving rewrite

§4.4. It is feasible, but adds little once scratch is per sequence. Build the reserve first.

### Q4. Amortized scratch

§4.2–4.3. Use a per-sequence high-water reserve, with draws held until each old root's last share returns. It is never more than today's charge.

### Q5. Fresh-flow policy

§5.1. Alias `fast.com` to `netflix.com`. Keep same-egress inheritance and make the single exit faster, rather than spreading.

### Q6. Zero priors

§5.2. Use an aggregate per-exit estimate counted only in saturated buckets, with unknown treated as the window median rather than zero.

### Q7. Physical observations

§6. Both observations show OCA downloads making no measurable progress while page and API requests complete. M1 to M5 have different signatures, and per-request CDP timing plus per-flow exit attribution separates them.

### Q8. 32 MiB failure modes

§7. The main risk is unpooled churn on the phone's receive path and the GC cost of holding the 31.18 MiB peak. Measure the §7.2 set before retaining H1-eight.

---

## 9. Corrections

### To the brief

1. **"Approximately 33.6% higher."** The like-for-like gain is +22.4–23.6% in the fixture and +26.8% in the harness. The original arm's median includes two ramp tests whose plateau equals the warm exit's (§1).
2. **"Different exits win later tests; it is not one consistently bad provider."** Agreed. But the lower later tests are not slower exits either; they are ramps to the same ceiling.
3. **"A 9.079-second observed byte-progress plateau."** It is a zero-OCA-byte start, not a plateau mid-transfer (§6.1).
4. **"Pool counters alone omit unpooled allocation churn."** `MessagePoolUnpooledCounts()` reports exactly that churn, cumulatively (`message_pool.go:1236-1240`).
5. **"That covers retained bytes and head/contract decode/rewrite scratch. It is an intentional recovery guarantee."**
   - Correct as policy.
   - But the guarantee needs scratch for one item per sequence, not for every item.
   - Measured residency is one root per Pack (§2.3, §4.1).
6. **Confirmed as stated:**
   - Legacy encoding prepays five roots (`transfer_retained_budget.go:71-75`).
   - `h1EstablishedEnvelopeAvailable` checks acknowledgement, metadata generation and credit, not proof size (`transfer.go:3543-3553`).
   - The donor check passes on a nonpositive prior.

### To my previous review (LOWBULKFLOW2-REVIEW1)

1. **"Predicts the other six runs within 0.5%."**
   - Wrong for two runs: h was 0.64% off and j 0.57%. The brief is right.
   - The corrected model in §2.2 keeps every 40 ms run here within 2.5%.
2. **"η ≈ 0.20" and "+31% for 8 / 8,800".**
   - One Pack in six is a 1-frame Pack charged 8,228 B, so HEAD's η is 0.218 and the predicted gain is +26.6% (§2.2).
   - The 8 / 8,800 entry in that review's §1.2 table should read η 0.276 with message bytes as the numerator.
3. **Lever L2 ("reserve scratch per writer").** It is the right lever, and §4 replaces it with a funded design. The ×2.2 estimate was close: ×2.29–2.30 measured with HEAD chunks.

---

## 10. Residual risks

- **Few outer TCP connections per exit per hop.**
  - All of an exit's flows share a small number of H1 connections on each hop; the fixture's managed peaks suggest one per window client. After any loss, each connection's congestion window regrows at its own pace.
  - The fixture shows the mechanism at Reno's one MSS per RTT (§1.2).
  - On a real device, check the server's per-connection TCP_INFO (cwnd, ssthresh, retransmits, delivery rate) for the phone's and the provider's H1 connections.
  - This is server-side observability; it needs no device change.
- **Public exits change only as they update.** The reserve (§4) and any chunk policy reach users only through provider builds. Client-side levers (the alias, exit choice) do not raise any exit's ceiling.
- **The runt shape is inferred, not measured, for the provider's groups.** Measure group-size and frame-size distributions on a real path before choosing between H1-eight and a byte-limit rule.
- **The lineage hypothesis (M1) is untested.** It rests on the alias gap and the 32/64 MiB contrast. The first step in §0.3 tests it cheaply.

---

## Appendix A. How the measurements were made

**Source.** Connect `49f285f1`, unmodified in the repository. Scratch files, under `/private/tmp/claude-501/-Users-builder-urnetwork-connect/31deb01d-2417-4a60-8aa7-6fab1905e12e/scratchpad/`:

| File | SHA-256 prefix | Role |
|---|---|---|
| `overlay/zz_review_bulk_test.go`, `zz_review_workload_test.go`, `zz_review_retained_test.go` | `c1c65be914395224`, `bbe9a4c1af55fa68`, `8015a39d243f623d` | REVIEW1's harness, byte-identical to its Appendix A.4 |
| `h1e/transfer.go` | `1fab6298b99edf37` | The team's H1-eight production candidate (8 frames / 8,800 B) |
| `l2/transfer_retained_budget.go` | `c57568442240b0f6` | HEAD plus a `REVIEW_ONE_ROOT` switch that charges root + owner; off by default |
| `l2/zz_review_steady_test.go` | `9714f5bdd1a33518` | Bulk harness with separate data and ACK delays, plus GC'd live heap and pool roots per 0.5 s |
| `l2/zz_review_alloc_test.go` | `b69ede1406f89dd9` | Bulk harness with runtime/metrics and unpooled counts over an 8 s window after a 2 s ramp |
| `l2/zz_review_live_test.go` | `ad64f00e09430782` | The receiver-less T7 attempt described below; it never reached the budget |
| `r3/packing.py`, `r3/perexit.py` | `ebba6fddc8b76dc4`, `c4a5ff56756f9698` | Charge-function packing table (§3); per-test, per-exit extraction from the FIVE reports (§1) |

**Build:**

```sh
cd connect
go test -c -overlay <dir>/overlay.json      -o <dir>/connect-head.test .
go test -c -overlay <dir>/h1e/overlay.json  -o <dir>/h1e/connect-h1e.test .
go test -c -overlay <dir>/l2/overlay-head.json -o <dir>/l2/connect-head-l2.test .
go test -c -overlay <dir>/l2/overlay-h1e.json  -o <dir>/l2/connect-h1e-l2.test .
```

**Runs** (`GOMAXPROCS=4`, 10 s, `REVIEW_REMOTE_PROVIDER=1 REVIEW_STEPS=provider`):

```sh
# §2.1, paired A1 B1 A2 B2 then B3–B6
REVIEW_BULK=1 REVIEW_RTT_MS=<20|40|80> REVIEW_FLOWS=<1|8> REVIEW_PROVIDER_BUDGET_KIB=2048 [REVIEW_NO_RETAINED=1] \
  <bin> -test.run 'TestReviewBulkBisect$' -test.v -test.count=1
# §2.3, steady state
REVIEW_STEADY=1 REVIEW_DATA_DELAY_MS=1 REVIEW_ACK_DELAY_MS=1000 REVIEW_FLOWS=8 REVIEW_PROVIDER_BUDGET_KIB=<2048|8192> [REVIEW_ONE_ROOT=1] \
  <l2 bin> -test.run 'TestReviewRetainedSteadyLive$' -test.v -test.count=1
# §2.4 and §2.5
REVIEW_BULK=1 or REVIEW_ALLOC=1, REVIEW_RTT_MS=40 REVIEW_FLOWS=8 REVIEW_PROVIDER_BUDGET_KIB=2048 [REVIEW_ONE_ROOT=1] \
  <l2 bin> -test.run 'TestReviewBulkBisect$|TestReviewBulkAlloc$' -test.v -test.count=1
```

**A failed attempt.** A first T7 attempt with no receiver (`zz_review_live_test.go`) stopped admitting after 32–46 groups. With no ACK at all, the sequence stopped at about 330 KB of unacknowledged message bytes (109 Packs). That held even with `ResendQueueMaxByteCount` and `ResendQueueMinByteCount` raised to 256 MiB, and the budget was far from full. I did not trace which limit binds without feedback, and replaced the test with the steady-state method above.

**Evidence extraction.**

- `perexit.py` reads each run's `BeforeDetails` and `AfterDetails` `provider_recovery` counters and the 0.5 s `samples`.
- The CDP timeline in §6.1 is `diagnosticNetwork.samples` and `endpoints` from `fast-1.json`.
- Go size classes are from `/usr/local/go/src/internal/runtime/gc/sizeclasses.go` (Go 1.27.3).
