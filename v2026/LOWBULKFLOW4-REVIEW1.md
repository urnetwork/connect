# Low bulk throughput: independent review of LOWBULKFLOW4

Date: 2026-11-04. Reviewer: Claude (Opus 5.5). Reviews [LOWBULKFLOW4.md](LOWBULKFLOW4.md); follows [LOWBULKFLOW3-REVIEW1.md](LOWBULKFLOW3-REVIEW1.md).

**Goal this review is aimed at:** sustained hosted-H1 downloads above 100 Mbps, especially through one provider, without giving up browsing latency, affinity, reliability or the 32/64 MiB gates.

**Inputs**

- The brief.
- Source, unmodified: connect `49f285f1`, sdk `eb552288`.
- Retained evidence under the brief's two prefixes. The prefixes I checked matched the brief: `FINAL-RESULT-SEAL.json` 702de22a and `SOURCE-MAP.md` 228d48fa.
  - Connected run: `WINDOW-CORRELATION.json` (1db40869), `CONNECTED-ANALYSIS.json` (707556be) and `TIMING-GC-ADDENDUM.json` (4e94e270).
  - Raw files behind them: `fast-1.json` (9f5b499c), `telemetry.ndjson` and the teardown diagnostics `stdout.log` (b055a56c, 204 window rows plus 26 memory rows).
  - The two status snapshots bracketing the run.
  - Fixed-total plans: `MEASUREMENT-PLAN.json`, `REPORT.txt`; the NAT repair `REPORT.txt` and `FIXTURE.diff`.
  - Reverse-ACK `READY.json`/`AUDIT.json`; late-bootstrap r6 `CONTROLS114-AND-BODY-HANDOFF.json`.
- No new runs, builds or tests this time. Every number below is read from that evidence or source, or derived from them by the arithmetic in Appendix A.

**Evidence tags**

- **[E]** Read from retained evidence.
- **[C]** Read from source.
- **[A]** Arithmetic on [E]/[C] values (Appendix A).
- **[R3]** Measured in review 3 and carried forward with its original limits.
- **[H]** Hypothesis, not yet tested.

**Constraints honoured**

- No credentials, deployment, device or network operations.
- No identities: the two exits are called *speed-A* and *speed-B*. Their destination IDs differ in the diagnostics; I do not reproduce them.
- The only repository change is this file.

---

## 0. Summary

### 0.1 Answer to the headline question

The new phone pair cannot tell you which provider-side limit to fix, because the 22 Mbps run was not a single-exit, steady-state measurement. It shows three things the brief does not:

1. **It used two exits, not one** (§1.1). The two speed windows went to different destinations, and both carried thousands of download Packs.
2. **It never plateaued** (§1.2).
   - Page progress was about 5 Mbps at 4 s, 20 Mbps at 10–13 s and 30 Mbps at 13–17 s.
   - Fast.com reported 22 Mbps while the rate was still rising.
   - Averages: 16.5 Mbps over the whole page; 19.6 Mbps from the first origin byte; 27.8 Mbps over the last 6 s.
3. **Both directions of the path were badly out of order or lossy** (§1.3–§1.5).
   - At least 13,308 download Packs arrived ahead of a missing predecessor.
   - Wi-Fi carried about 14.6 MB (23% of its downstream bytes) beyond what framing explains.
   - On the reverse path, inner-TCP-ACK Packs needed 2,056 recovery writes for 3,638 originals.
   - 1,229 of those were selective-gap writes. Each required the provider to hold at least three later reverse Packs while missing an earlier one.

Small downstream packets add a fourth point. They averaged 855 B against the provider NAT's 1,100 B MTU. The NAT's budgeted chunk limit is a whole multiple of the segment payload, so runts arise only from small socket reads or small inner-window openings (§1.7). That points to NAT flows that were mostly window-limited or starved, not backlogged behind a full Transfer send leaf **[H]**.

The best-supported mechanism for this run is therefore **path disorder → Transfer holds and reverse-ACK head-of-line delay → window-limited inner flows with an inflated inner RTT** (§4, §5) **[H]**. Two other limits are still open:

- The provider's own capacity: a desktop provider with only about 0.9 MB of send budget for all its consumers, or a slow residential uplink.
- The mobile retained-charge limit.

The phone data cannot exclude either. The cheapest discriminants are header-only inner-TCP telemetry at the tun, Transfer RTT per window, and the provider's row (§9).

The software levers rank the same as in review 3, but each applies only to some provider rows (§2):

| Single-exit software ceiling, no other limit binding [A] | Flight (payload) | 50 ms | 100 ms | 150 ms | 200 ms |
| --- | ---: | ---: | ---: | ---: | ---: |
| Mobile64 provider, HEAD chunks, three-root charge | 627 KB | 100 | 50 | 33 | 25 |
| Mobile64, H1-eight chunks (N6) | 779 KB | 125 | 62 | 42 | 31 |
| Mobile64, fixed-total 2:1, H1-eight | 1,212 KB | 194 | 97 | 65 | 48 |
| Mobile64, one-root bound, H1-eight (×2.45, [R3]) | 1,910 KB | 306 | 153 | 102 | 76 |
| Desktop20 provider, whole send leaf, message bytes | 899 KB | 144 | 72 | 48 | 36 |
| Large-host miner, capped by the N6 phone's advertisement | 4,362 KB | 698 | 349 | 233 | 174 |

All values are Mbps. The desktop20 row is shared by every consumer of that provider. The miner row is capped by the consumer's advertised window, not by the miner.

What the observed rates imply:

- If held arrivals track bytes, the last 5 s split about 23 and 7 Mbps between the two exits (§1.1). At a plausible 150 ms Transfer RTT that is only about 130–430 KB of useful flight per exit.
- That is below every row's software ceiling unless the effective retention time was well above the RTT.
- Holds, leases and reverse-ACK delay are exactly what would stretch it.

### 0.2 Findings

1. **The connected run used two exits.** [E] The two speed-window clients had different destination IDs and both carried download traffic (§1.1). The brief's "through a United States H1 exit" should read "through two US H1 speed windows".
2. **The 22 Mbps is a mid-ramp estimate**, after a 3–4 s start with almost no origin bytes. [E/A] See §1.2.
3. **`ReceiveQueueCommitCount` counts out-of-order arrivals only.** On exact-accounting receivers it counts held Packs that arrived ahead of a gap; in-order heads are delivered without counting. [C] Speed windows: 13,308 held arrivals in 20 s, compared with an estimated 13,000–20,000 speed-window Packs in total. [E/A] See §1.3.
4. **Wi-Fi received 1.447× the tun bytes during the download.** [E] Framing and outer headers explain about 1.10–1.12× [A/H]. The remaining ~14.6 MB is most plausibly duplicate Pack deliveries [H] (§1.4).
5. **Reverse inner-ACK traffic was disordered at the provider.** [E/C] Recovery writes were 56.5% of initial writes, and 925 timeouts fired while the cumulative ACK was still advancing (the brief's M4 cascade). This is evidence, not just a plausibility argument, that the reverse path had holes (§5.1).
6. **The phone's own send leaf held 8.8 MB five seconds after the download ended**, against 0.17 MB on receive. [E]
   - A small reverse Pack is charged 8,228 B for about 156 B of messages [C/E], η ≈ 1.9%.
   - The 8.8 MB is about 1,070 small-Pack equivalents [A].
   - It did not bind the 18.7 MB Android64 leaf, but it is the leaf to watch on iOS32 (§1.6).
7. **Downstream packets averaged 855 B.** [E] The provider NAT segments at `DefaultMtu` 1,100 [C], while the phone's tun MTU is 1,280 [E]. Runts come from small reads or small window openings, which argues against a full Transfer send leaf being the binding gate in this run [H] (§1.7).
8. **Ordinary desktop and miner providers count message bytes, not retained roots.** Retained-root charging is enabled by the mobile policy, or forced when a budget has a parent [C]. The per-sequence scratch reserve therefore helps mobile Low exits only (§2).
9. **The correlation file's "retained send items" are historical.** It labels `RouteRetainedItemCount` as current retained items (`localRetainedSendItems`, `retainedSendItemsAtEnd`). That value is published only with a new longest-silence record. [C] See §11.
10. **Download data kept arriving for about 8 s after Fast.com finished**: about 3.9 MB at the tun, with 2.0× Wi-Fi overhead [E]. Something upstream of the phone held a deep backlog, or the page kept requests running; the current counters cannot separate the two (§1.2).
11. **The 2:1 experiment is well targeted for mobile64 providers only**, and its receive-side cut also reduces room for held reverse-ACK Packs. That is a download risk as well as an upload one (§6).
12. **The NAT fixture repair is sound.** The positive control should assert the default policy's *Allow* decision for the documentation origin rather than infer it from a completed dial (§10.3).
13. **The fixed-total screen can be falsified before the candidate runs** (§6):
    - The baseline arm must show the send leaf near full while admission is blocked; otherwise 2:1 cannot help that fixture.
    - If it binds, the predicted download ratio is ×1.556, well above the screen's 1.10 threshold.

### 0.3 Next steps, in order

1. **Header-only inner-TCP telemetry at the phone's tun (§9.1).** Per flow: SYN MSS/wscale, advertised receive window over time, inner RTT from timestamp echo, delivered bytes, and the window that carried it. This classifies each exit as window-limited or sender-limited from one public run.
2. **Export what Transfer already knows (§9.2).** Per window: Transfer RTT, the window estimate (Window, Ceiling, Reason), the received advertisement, negotiated lanes, duplicate receipts (`PeerAudit.resend`), held bytes high-water, retained send items and charge.
3. **Record the provider row** at window selection: capability bits and the platform's measured provider speed, without identities (§9.3).
4. **Run a deterministic two-carrier reorder test**, with `ReliableLaneProvenRecovery` off and then on, plus a reverse-hole injection (§4.4, §5.3).
5. **Before the 2:1 screen**, add the baseline binding check, a reverse-hold guardrail and a small-request TTFB probe (§6).
6. **Prototype the per-sequence scratch reserve** under the invariants in §3.2. It applies to mobile Low exits only.

---

## 1. The connected 22 Mbps run, re-read

### 1.1 Two exits, not one

[E] The teardown diagnostics carry one `window_send` and one `window_receive` row per window every 5 s. Two speed windows were active, with different destination IDs. Deltas over the 20.003 s Fast bracket:

| Counter (bracket delta) | speed-A | speed-B | Sum (brief) |
| --- | ---: | ---: | ---: |
| Held out-of-order arrivals (`ReceiveQueueCommitCount`) | 7,558 | 5,750 | 13,308 |
| Transfer ACK writes | 7,791 | 6,519 | 14,310 |
| Initial reverse writes / frames | 1,767 / 3,603 | 1,871 / 3,570 | 3,638 / 7,173 |
| Timeout resends | 97 | 331 | 428 |
| Selective-gap writes | 462 | 767 | 1,229 |
| Tail probes | 32 | 366 | 398 |
| Timeouts with recent cumulative progress (M4) | 225 | 700 | 925 |
| Timeout deferrals | 194 | 369 | 563 |

The rows here are the correlation file's rows 10 and 11. Their order is arbitrary.

Both exits carried thousands of download Packs: speed-B from 5 s, speed-A from 10 s. The 22 Mbps is therefore an aggregate over two exits, and an uneven one.

- In the last 5 s interval, speed-A recorded 4,894 of the 6,253 held arrivals.
- If held arrivals track bytes, that is about 23 Mbps on speed-A and 7 Mbps on speed-B, out of about 30 Mbps at the tun [A/H].

The two destination IDs could still be two provider slots on one host. A standalone miner can run several, per the source map.

### 1.2 Timeline: slow start, steady ramp, long tail

[E] Page data (`dataReceived`, CDP samples about 1 s apart, measured from page start):

| Page interval (s) | 3.1–4.1 | 5.1–6.1 | 7.2–8.2 | 9.2–10.2 | 11.2–12.2 | 13.2–14.2 | 15.2–16.2 | 17.2–18.2 | 18.2–19.2 |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| Mbps | 5.4 | 9.0 | 15.0 | 19.7 | 20.1 | 32.8 | 31.5 | 27.0 | 20.0 |

Startup:

- The first origin requests (an E3 POST and GET) started at 2.175 s and had no response at 3.1 s.
- The first origin response bytes had arrived by 4.06 s, on a GET started at 3.153 s.
- The oldest-four maximum header wait was 6.274 s: a POST to E3 started at 7.925 s and was still header-less at 14.2 s.

[E] Interface counters from `telemetry.ndjson`, about 1 s apart, measured from the Fast wrapper start:

| Wrapper time (s) | 5.1 | 7.2 | 8.2 | 10.1 | 12.2 | 14.1 | 16.1 | 18.2 | 20.2 | 22.2 | 24.2 | 26.2 | 28.2 |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| Wi-Fi rx Mbps | 19.1 | 26.8 | 30.5 | 29.5 | 28.5 | 47.7 | 41.1 | 39.5 | 27.7 | 10.8 | 7.0 | 9.2 | 6.9 |
| tun rx Mbps | 9.5 | 13.4 | 14.4 | 21.8 | 23.0 | 33.9 | 31.9 | 32.5 | 20.5 | 5.0 | 4.6 | 3.6 | 1.9 |

What this shows:

- The rate rose for 15 s and never settled. Fast.com's adaptive connection count (1→8) and its own estimator, not the path, decided when to stop.
- After the spinner stopped (19.24 s), the tun kept receiving about 4 Mbps for 8 s, about 3.9 MB in all.
- speed-B recorded 3,652 more held arrivals in the 10 s after the end, and its selective-gap writes rose from 770 to 1,266 (cumulative counters at 19.9 and 29.9 s).
- This tail is either the provider's backlog draining (reliable Transfer delivers everything already admitted) or the page's follow-on requests, such as the OPTIONS and POSTs issued at 19.2 s. Without per-flow accounting I cannot separate them.

### 1.3 Most speed-window Packs arrived out of order

[C] On an exact-accounting (mobile) receiver:

- `holdHead` returns false (`transfer_receive_delivery_dispatch.go:76-80`).
- An arrival with `sequenceNumber == nextSequenceNumber` goes straight to `receiveHead` with no counter (`transfer.go:16892-16908`).
- An arrival above the delivery point is held, and `commitHeldPrefix` → `commitHeldItem` increments `receiveQueueCommitCount` (`transfer.go:17029-17050`, `:17149-17151`).
- The snapshot's own comment says the same: "held items that crossed the boundary and were acknowledged" (`transfer.go:1768-1769`).

So the 13,308 commits are a **lower bound on out-of-order arrivals**. Tentative holds evicted before commit would add more; there were none in this run.

[A] The denominator is not exported, but it can be bounded:

- The tun received 52,098 packets during the download (3.2–20.2 s).
- HEAD packing caps a Pack at 3,300 B: three full-size packets, or about four runts [R3]. A 16-packet group of full-size packets averages 2.67 per Pack.
- That gives about 13,000–20,000 Packs across both speed windows.

A majority, possibly most, of download Packs were therefore held behind a missing predecessor. The brief lists the commits as plain receive activity. They are the strongest disorder signal in the run.

### 1.4 About a quarter of the Wi-Fi bytes are unexplained

[E] Same telemetry, matched spans:

| Span (s) | Wi-Fi rx | tun rx | Ratio | tun B/packet |
| --- | ---: | ---: | ---: | ---: |
| 3.2–20.2 (download) | 64,439,489 | 44,528,541 | 1.447 | 855 |
| 4.2–14.1 (ramp) | 35,372,450 | 22,422,644 | 1.578 | 849 |
| 14.1–20.2 | 28,107,270 | 21,368,440 | 1.315 | 865 |
| 20.2–29.2 (tail) | 7,917,686 | 3,944,961 | 2.007 | 753 |

[A/H] Expected overhead:

- Pack and TransferFrame headers plus the end-to-end AEAD come to about 5% for ~3.4 KB Packs.
- Outer TCP/IP headers on 1,198 B Wi-Fi packets add about 4.5%; the L2 header about 1%.
- That gives 1.10–1.12. Background traffic in the 22 s before the run averaged about 0.3 Mbps, under 1 MB over a span this long.

At 1.12, about 14.6 MB (23% of Wi-Fi bytes) is unexplained during the download. The most plausible cause is duplicate Pack copies: provider timeouts and gap resends for Packs the phone already held. This fits finding 3.

The receiver already counts duplicates (`peerAudit.resend` in `ReceiveSequence.receive`, `transfer.go:16866-16887`), but the window diagnostics do not export that count.

### 1.5 The reverse path had real holes

[C] A selective-gap write needs `SelectiveAckGapThreshold` = 3 later selective ACKs for an unacknowledged item (`transfer.go:923`, `:8467-8479`), and each item is gap-recovered at most once. [E] So in 20 s the provider's receiver held at least three later reverse Packs while missing an earlier one at least 1,229 times. That is about one hole per three initial reverse writes.

Further context:

- 925 timeouts fired while the cumulative ACK had advanced within one scaled RTT. The counter increments before the deferral decision (`transfer_recovery_selection.go:327-333`), as the brief says.
- `UnreliableFlightGapReorderSuspected` reached only 4 (speed-A) and 2 (speed-B) by the end of the download. It counts only a gap whose missing item was acknowledged *before* the resend was written, so it does not show that the holes were real losses.
- Over H1, a Transfer-level hole needs one of:
  - a drop at an endpoint queue (a full platform receive route, a provider receive refusal, a sequence restart);
  - a drop inside the platform;
  - interleaving across more than one carrier or route.

[C] `ReliableLaneProvenRecovery` defaults to false (`transfer.go:988`). With two reliable routes, a later ACK from either route counts as proof of a hole. A hole that is merely late on the slower route then gets a spurious gap write. That would produce exactly finding 4's duplicates without any loss **[H]**.

[E] One hint: the phone had 15 platform transports for 12 window clients plus its control client, and no handoff was active. So at least one client had more than one carrier. Which clients is not recorded.

### 1.6 The phone's send leaf held mostly reverse traffic

[E] Transfer graph diagnostics, 5 s cadence, wrapper time:

| t (s) | −0.1 | 4.9 | 9.9 | 14.9 | 19.9 | 24.9 | 29.9 | 34.9 |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| Client transfer used (MB) | 1.06 | 1.33 | 9.07 | 11.14 | 14.00 | 9.80 | 11.11 | 5.52 |

The status snapshot at 25.0 s had `clientSendBytes` 8,799,080 and `clientReceiveBytes` 169,216. The periodic leaf sample during the run had send at 10,144,932 and receive at 0. `ClientSendByteCount` is `ResendQueueBudget.UsedByteCount()` (`sdk/device_local.go:2505-2507`) [C].

[C/E] A small Pack's estimate is message + 512 + 32k + contract, which puts it in the 2,048 class.

- Charge: 3 × 2,060 + ceil1K(1,488) = 8,228 B.
- The provider-send leaf shows exactly 8,228 B for one 686 B item in this run's first memory row.
- So 8.8 MB is about 1,070 retained small Packs, roughly 6 s of the speed windows' reverse writes.

Retention that long requires the provider's cumulative ACK to have lagged the reverse stream by seconds, consistent with §1.5. Part of it could instead be quality-window items to slow providers; those windows' historical silence maxima were 15–30 s. The diagnostics do not split the charge by window; `ResendQueueSize(peer)` already exists.

Two consequences:

- **Memory.** The iOS32 client send leaf is smaller than Android64's 18.7 MB. Its 30.754 MiB qualification sample is a quiet-path figure, not one taken under reverse holes.
- **Reverse-ACK efficiency.** η for reverse traffic is about 1.9%. The per-sequence reserve (§3) halves the per-Pack charge. Opportunistic ACK coalescing (§5.3) divides the number of Packs.

### 1.7 Packet size says the NAT was mostly not backlogged

[C] Facts:

- The provider NAT reads the origin socket.
- For each chunk it computes `min(i + windowOpening, i + chunkLimit, n)` (`ip.go:6112-6131`) and splits that chunk independently at the MTU payload (`ip.go:7253-7283`).
- `DefaultMtu` is 1,100 (`ip.go:44`), so the payload is 1,048 B with timestamps. The phone's tun MTU is 1,280 (`DefaultTunnelMtu`, `ip.go:54`).
- In the budgeted path, `chunkLimit` is `natMemoryBatchCount ×` payload (`ip_nat_memory.go:164-177`), a whole multiple of 1,048.
- Unbudgeted, it is the 64 KiB read buffer, which leaves one runt per roughly 62 packets.

[A] So, apart from that, runts appear only when the read size `n`, or the inner-window opening, is not a multiple of 1,048:

| Read or opening | Packets | Average payload |
| ---: | ---: | ---: |
| 1,448 (one origin segment) | 2 | 724 |
| 2,896 | 3 | 965 |
| 4,344 | 5 | 869 |

A NAT blocked behind a full Transfer queue lets its socket buffer fill. Its next reads are large, giving full packets with about one runt per chunk.

The run averaged 855 B per tun packet across 52,098 packets [E]. That is the signature of a reader that is usually *waiting*:

- either for origin bytes,
- or, more likely given §1.5, for inner-window openings that arrive as a phone ACK covering one or two runt-sized segments, which then propagate.

**[H]** This argues against the provider's Transfer send leaf being the binding gate for these exits during most of the run, and for window-limited inner flows.

It is a source-plus-statistics inference, not an observation. §9.1 gives the direct test.

The same shape explains review 3's 752 B Pixel average. It also means public streams carry about 29% more packets than full-size segmentation needs: 52,098 against about 40,500 at 1,100 B.

### 1.8 What the run does and does not establish

**Established [E]:**

- Two exits.
- A mid-ramp result.
- Heavy forward disorder and probable duplication.
- Reverse holes at the provider's receiver.
- Phone receive leaf not under pressure.
- Phone send leaf heavily occupied.
- No receive refusals, evictions or Pack drops.
- ACK writes rarely blocked: 28 blocked writes totalling 20 ms.

**Not established:**

- Either provider's row, version, uplink or RTT.
- Where the disorder originates.
- Per-flow window or RTT.

The run also had different exits from the direct run, 7 min 51 s apart, as the brief says.

---

## 2. Provider topology and what each row is limited by

[C] I checked the source map's central claims in source:

- Retained-root accounting is enabled by the mobile policy (`sdk/mobile_memory_policy.go:362`, `:394`).
- Connect forces it whenever a budget has a parent (`transfer.go:7136-7143`, `:15326-15330`).
- An unparented desktop or miner budget therefore counts message bytes.
- With no budget at all, the send window holds the configured constant within known limits (`transfer.go:12413-12435`).
- Otherwise its ceiling is the queue's `LendableByteCount`, clamped by the peer's advertisement (`:12448-12497`).

| Row | Send-side quantity that binds one consumer | What raises it | What does not |
| --- | --- | --- | --- |
| Mobile Low provider (Android app, providing) | Retained charge in a 2,876,094 B leaf (64) or 1,438,047 B (32): three roots plus owner per item, η 0.218 HEAD / 0.271 H1-eight | Per-sequence scratch reserve (×2.29–2.49 bound [R3]); 2:1 (×1.556); H1-eight packing (×1.27 [R3]); fewer runts | Larger consumer receive budget |
| Desktop20 SDK provider | 898,779 B leaf in message bytes, unparented, shared by all its consumers with legacy floor semantics | 2:1 if applied here (×1.556); a larger desktop target; per-consumer fairness | The reserve (no roots charged) |
| Host-derived miner | Usually the consumer's advertisement: N6 4,362,076 B, or 2,621,440 B at host target0 | Consumer receive capacity and hold efficiency; NAT and inner-window health | Provider-side budget changes |
| Hosted proxy / server connect | Not an exit | — | — |

At the observed per-exit rates, only these would bind at plausible RTTs:

- a heavily shared desktop20 provider;
- a mobile provider whose items stay retained well beyond the RTT, through holds and selective-ACK leases;
- a provider whose access uplink is about 15 Mbps.

The brief does not consider the last one. A residential exit's uplink, not its software, is what serves downloads, and many uplinks are 10–40 Mbps. The platform's own provider speed measurement (`TransportControlSpeedStart`/`Stop`, `transport.go:145-146`) would expose it.

---

## 3. Request 1: retained-payload efficiency

### 3.1 Is three- or five-root scratch charging still the strongest single-exit limiter?

**For mobile Low exits, yes.** [R3] Charging one root per item lifted one exit at 2 MiB and 40 ms by ×2.29–2.30 (HEAD chunks) and ×2.45–2.49 (H1-eight chunks). The next levers are 2:1 (×1.556) and H1-eight (×1.27).

**For the public Fast.com case, not shown.**

- The public exits' rows are unknown.
- Desktop and miner rows do not charge roots.
- The phone evidence points first to path disorder (§1).

The reserve is the strongest software lever for one row, not a demonstrated fix for the public number.

### 3.2 A funded per-sequence scratch reserve

Facts carried from review 3 [C/R3]:

- `setHead` rewrites only `sendItems[0]`, once per item, on the sequence goroutine. `receiveContractMissing` is the other rewrite path.
- A `setHead` failure exits the sequence (`transfer_recovery_selection.go:370-374`).
- At steady state one pooled root is resident per item.
- The reservation is released at ACK even while a carrier still holds a share of the root.

**Design.** Each item is charged `root + owner`. Each sequence holds a scratch reserve `S = k × Rmax`, where:

- k = 2 for compact-head encoding, 4 for legacy.
- `Rmax` is the largest root class among the sequence's *currently* retained items. Track it with a per-class count: four pool classes plus the largest unpooled root.

**Ownership and accounting invariants**

| # | Invariant |
| --- | --- |
| O1 | S is reserved from the same leaf as the items and is visible in `Used`, exported separately as `ScratchReserveByteCount`. No hidden borrowing. |
| O2 | Admitting an item with root above `Rmax` first tops S up by `k × (root − Rmax)`. If that fails the item waits, as today. Today the same item would need `2 × root` more, so admission is never stricter than today. |
| O3 | A rewrite draws one slot of at most `Rmax` from S and never calls `Reserve` on the shared budget, so it cannot fail for budget reasons at a full leaf. The new root becomes the item's root. The old root's charge moves into the slot until its last carrier share returns. |
| O4 | Only the sequence goroutine draws; carriers only return shares (atomic decrement, then slot release). At most k slots are outstanding. If every slot holds an old root still shared by a carrier, the rewrite waits, bounded by the carrier `WriteTimeout` (15 s). It never borrows. |
| O5 | Legacy fallback: assume k = 4 until the peer's first ACK proves compact-head support, then release `2 × Rmax`. A later downgrade, such as a contract-missing reconstruct, re-tops S to k = 4 before rewriting, or waits. |
| O6 | Encrypted envelopes: if the seal output is a separate pooled root per write, it must be funded by S, by a per-carrier transient budget, or by the platform transport budget. This was still open in review 3 and needs a source audit before k is fixed. |
| O7 | Release: S drops to zero only when the sequence has no retained items, no occupied slots and no pending rewrite. Close releases it exactly once, after the existing owner wait. |
| O8 | Never more than today for a non-empty sequence: `Σ(root_i + owner_i) + k·Rmax ≤ Σ(root_i + owner_i) + k·Σroot_i`. Equality holds for one item. The one excess over today is deliberate: O3 keeps an old root charged until its last share returns, at most `k × Rmax` per sequence. That funds real overlapping roots, as the brief requires. |
| O9 | With many consumers the reserve totals `Σ k·Rmax`. With 40 active H1-eight consumers that is 40 × 2 × 8,204 = 656 KB of a 2.876 MB leaf, still less than today's per-item extra roots whenever each consumer has an item at `Rmax`. |

**Counters**, per client:

- `ScratchReserveByteCount` (current and high-water).
- `ScratchDrawCount`, `ScratchWaitCount` and `ScratchWaitDuration`.
- Rewrites by reason: setHead, contract-missing, legacy.
- Old-root share lifetime maximum.
- A live η: Σ retained message bytes / Σ charge, sampled with the leaf.

**First deterministic failure test.** At a leaf with `Used == Total`, held by one sequence's items plus another sequence's floor:

- Force a head rewrite after cumulative progress.
- Force a contract-missing reconstruct.
- Force a legacy re-encode.
- Keep the old root's carrier share outstanding across two rewrite intervals.

Assert all of the following:

- No `Reserve` call on the shared budget.
- No `head_rewrite` sequence exit.
- The third rewrite waits for a share rather than borrowing.
- `Used ≤ Total` throughout.
- After every ACK and share return, `Used` returns to its starting value exactly.

**Smallest controlled performance intervention.** Review 3's connect-only harness at 2 MiB and 40 ms, HEAD and H1-eight chunks, A/B today against the reserve. Prediction: within a few percent of the one-root bound (×2.29 and ×2.45). Then the fixed-total download-clean stratum.

**Reject the reserve if any of these happen:**

- The harness gain is below ×1.5, so the reserve is not recovering the scratch.
- Any rewrite fails or exits at a full leaf.
- The matched iOS32 cell's physical peak exceeds its gate.
- The fixture gains under 1.10 while its baseline send leaf sits above 90% full.

### 3.3 The wire-preserving rewrite

This is the alternative to funding scratch: rewrite the head in place. It changes no charge if the original root is reused. However, it touches the protobuf encoding of an already-shared root, and carriers may still be writing that root. It must wait for the last share before mutating, which is O4's wait without the reserve.

The reserve is simpler to prove. It also keeps the existing encoder.

---

## 4. Request 3: two provider retention gates

### 4.1 How they compose

[C] The two gates:

- **NAT.** Admits return data against the inner window, `rwnd − (receiveSeq − receiveSeqAck)` (`ip.go:6112-6131`). It keeps the bytes charged until the *inner TCP ACK* (`ip.go:6090-6091`) and stops reading when the window is closed.
- **Transfer.** Admits Packs against its send window and keeps them charged until the *Transfer cumulative ACK*. A selective ACK leases an item rather than releasing it (`transfer.go:16952-16984`, `:8523-8538`).

[A] For one flow f, the NAT's outstanding bytes are the sum of:

- Q_f, waiting before or inside Transfer admission;
- T_f, retained by Transfer: in flight, or held or leased at the phone;
- D_f, delivered to the tun but not yet inner-ACKed;
- A_f, the inner ACK in reverse transit, including reverse holes.

Each flow is bounded by `Q_f + T_f + D_f + A_f ≤ rwnd_f`. Transfer bounds the sum of T_f over the sequence by its window. The per-flow rate is `rwnd_f / τ_inner`, where τ_inner is the sum of those four residence times.

### 4.2 Where they feed back poorly

1. **Transfer-bound** (mobile leaf full):
   - Q grows to `rwnd − T − D − A`, a standing queue inside the provider charged to a NAT budget shared by all its users (6.71 MB at mobile64).
   - It inflates τ_inner for every flow sharing the sequence, which slows ACK-clocked growth and adds browsing latency behind bulk on the same sequence.
   - After cancellation, Transfer must still deliver everything already admitted: the tail in §1.2.
2. **Reverse-hole-bound** (§1.5):
   - A_f jumps by the hole duration for every flow on the reverse sequence or lane at once.
   - All their windows close together. Transfer's forward window drains with nothing to replace it.
   - When the hole fills, the window opens in ACK-sized steps, giving runt chunks (§1.7) and a burst.
3. **Hold-bound** (forward disorder, §1.3):
   - T_f includes the time the phone holds Packs behind a gap.
   - Leased items keep the provider's send leaf charged until the gap fills.
   - The effective `B × η / τ` falls with hole duration, even when the RTT is short.

The NAT gate binds only when τ_inner exceeds Transfer residence by a large factor. For mobile64 HEAD that factor is 6.71 MB / 627 KB ≈ 10.7. Reverse holes lasting seconds supply such a factor.

### 4.3 Direction-correct measurements

| Direction | Where | Measurement |
| --- | --- | --- |
| Forward data, provider → consumer | Provider NAT | Per-flow `receiveSeq − receiveSeqAck`; count and duration of "tcp receive window wait"; time blocked emitting into Transfer |
| Forward data | Provider Transfer | Send leaf Used/Total; time admission is blocked on budget; retained items; leased (selectively-ACKed) bytes |
| Forward data | Consumer receiver | Held arrivals (exists); hold residence p50/p95; duplicate receipts; held-bytes high-water |
| Reverse ACK, consumer → provider | Consumer sender | Per-item time from first write to cumulative ACK (histogram); recovery writes by kind (exists) |
| Reverse ACK | Provider receiver | Held reverse arrivals and their residence; time from Pack arrival to inner-ACK application at the NAT |

### 4.4 Deterministic delayed-ACK test

Path: native → Multi → Transfer → provider download, through the real H1 exchange, with a scripted phone-side TCP receiver. It advertises a fixed rwnd W (1 MiB, wscale 7) and ACKs every two segments.

Arms:

- (a) Delay only the provider-bound Transfer ACKs by D.
- (b) Drop the first copy of one reverse Pack carrying inner ACKs for two flows; recovery is the owner's natural retry.
- (c) Delay inner-ACK application at the NAT by D.

**Assertions:**

- NAT outstanding never exceeds W per flow, and Transfer retained never exceeds its window.
- In (b), both flows' inner-ACK frontier at the provider stays still until the hole is repaired, then jumps.
- During the hole, NAT outstanding saturates at W and window waits are recorded.
- After repair, chunk sizes equal the window openings. This is the runt signature from §1.7, now produced on purpose.
- After close, the NAT and Transfer ledgers return to baseline.

**Performance variant.** Sweep D and fit the rate to `min(W / τ_inner, B·η / τ_transfer)`.

**Reject the two-gate model** if the rate stays near `B·η / τ_transfer` while NAT outstanding is pinned at W. That would mean the window gate is not where flight is lost.

---

## 5. Request 4: reverse ACK head-of-line delay and lanes

### 5.1 The phone already shows the cost exists

The brief calls reverse-ACK head-of-line delay a plausible cost whose mechanism tests have not run. The phone evidence is already stronger than plausible [E]:

- About 61 proven reverse holes per second across the two speed windows (1,229 in 20 s).
- 428 timeout resends and 398 tail probes, against 3,638 original writes.

Each hole holds every later reverse Pack on that sequence or lane until it is repaired:

- a gap write, at least one RTT after three later ACKs;
- otherwise a timeout, at least `RttMinResendInterval` 300 ms, with backoff to `MaxResendInterval` 8 s (`transfer.go:890-891`).

What the phone cannot show is how long each hole was held at the provider, or how much forward throughput it cost.

### 5.2 Lanes

[C] N6 Android64 requests eight bounded hash lanes (`transfer_logical_lane.go:1-37`; the AUDIT finding). Whether the public exits negotiated them is not recorded.

[A] With five concurrent Fast.com flows hashed onto eight lanes:

- The chance of no collision is 8·7·6·5·4 / 8⁵ = 20.5%.
- So lanes leave at least two flows sharing a lane about 80% of the time.

Lanes help only when the hole and the stalled flow are on different lanes. They do nothing for same-flow delay, which ordered delivery requires.

Logical lanes carry zero receive floors in source (`transfer.go:15320-15322`), so their memory cost is channel headroom and per-lane state, not reserved bytes.

### 5.3 Changes that keep ordered reliable delivery

1. **Find the source of the holes first.** If it is multi-carrier interleaving:
   - turning on `ReliableLaneProvenRecovery` removes the spurious gap writes and their duplicates;
   - collapsing a window to one carrier removes the holes themselves.

   Test: the two-carrier fixture in §4.4 with one carrier delayed 40 ms. Predictions:

   - held arrivals ≈ the faster carrier's share of Packs;
   - duplicate receipts ≈ gap writes with proof off, and near zero with proof on;
   - no holds with one carrier.

   Reject if duplicate receipts stay high with proof on.
2. **Opportunistic reverse-ACK coalescing.** When the sender is about to write a Pack and more pure-ACK frames are already queued for the same sequence, put them in that Pack.
   - It adds no waiting, so it is not pacing.
   - The phone averaged 1.97 frames per write [E]. Each Pack costs 8,228 B of charge (§1.6) and is one more chance to lose or reorder.
   - Measure frames per reverse Pack, reverse Packs per second and send-leaf charge before and after.
   - Reject if p95 ACK-path latency at the provider rises.
3. **Writer priority for reverse pure-ACK Packs.** H1 already has a priority lane for Transfer ACKs (`writeH1FramedReadyBatch`). Giving pure inner-ACK Packs that priority removes *queueing* delay behind upload data, not loss delay.
   - This matters here: Fast.com POST bodies were outstanding on the same exits during the download (§1.2).

Do not turn off reliability, and do not let a later inner ACK bypass an earlier missing Pack. Both are excluded by the brief, and neither is needed for options 1–3.

---

## 6. Request 5: the fixed-total 2:1 rebalance

**Targeting.** [E/C]

- The screen's provider is the real mobile64 Low graph at a configured 40 ms access RTT, carrying all streams.
- That tests the right leaf for mobile exits.
- It says nothing about desktop20 exits, whose leaf is unparented and counted in message bytes. The SDK applies the same 3:4 split there, so 2:1 would also raise desktop20's send leaf to 1,398,101 B, but under different admission semantics.
- A desktop arm is needed before claiming a general gain.

**Prediction.**

- [A] If the send leaf binds, the download ratio is 4,473,924 / 2,876,094 = 1.556.
- [R3] With N6's H1-eight chunks, that is about 139 → 216 Mbps at 40 ms full-MTU. Review 3's harness reached 254–260 Mbps under the one-root bound, so the consumer and carriers should not cap first.

**Falsify before the candidate runs.**

- In the baseline arm, record send-leaf Used/Total and the fraction of time admission is blocked on budget.
- If the leaf is below 90% full, or admission is rarely blocked while the rate is flat, 2:1 cannot help this fixture.
- If the leaf binds and the candidate still gains under ×1.10, the mechanism is wrong.

**Guardrails the plan is missing:**

| Risk | Why | Guardrail |
| --- | --- | --- |
| Reverse-ACK holds on the provider | The provider's receive leaf holds out-of-order reverse Packs. Each is charged its transfer-frame root (256 or 2,048), 256 per frame and the 1,024 owner, about 1.8–3.6 KB (`transfer.go:1423-1451`). At 2,236,962 B that is about 620–1,240 held Packs, *shared across all consumers*. | Receive refusals and held-bytes high-water for reverse Packs, in the loss strata |
| Uploads | The advertisement becomes min(`ReceiveQueueMaxByteCount`, receive total) = 2.24 MB, about 421 Mbps at 40 ms, so not the binder. The hold and the NAT's 128 KiB upload acceptance matter more. | Already in the plan; add receive-leaf refusals |
| Small-request TTFB | A larger send leaf can deepen the queue in front of the carrier | A concurrent 16 KiB GET probe every 500 ms; reject if p95 TTFB rises more than 20% |
| Legacy peers | A consumer that ignores the advertisement can overrun a smaller receive leaf | One arm with a peer that sends no advertisement; count tentative evictions |
| Shared-parent pressure | Send and receive share the provider parent with P2P, and the root with NAT and client | Root Used peak and its refusals |

The screen design is honest: 16 processes, two independent pairs per stratum, confirmation by 12 fresh pairs. The 1.10/0.95 thresholds sit well below the predicted ×1.556, so a binding leaf would show clearly.

---

## 7. Request 2: topology, lending and fairness

**Which improvements help whom:** see the table in §2.

- One change helps every row that carries public streams: fewer runts. That means segmenting at the peer MSS on H1 paths, or deferring a sub-MSS tail while more data is in flight on that flow. It cuts packets, frames and charge per byte.
- The 1,100 B MTU probably exists for H3 datagram size. Any change must keep a flow's packets valid if its carrier later changes. **[H]**
- Test: a deterministic origin that writes 1,448 B segments at the public rate. Assert the packet-size distribution and byte-exact delivery, and count fragmentation under a later H3 carrier.

**Lending idle client capacity to a mobile provider.** Invariants:

| # | Invariant |
| --- | --- |
| L1 | Lend only headroom above the client's recent high-water (for example, the 60 s maximum) plus a fixed client floor |
| L2 | A provider item cannot be revoked before its ACK, so a lease may not exceed the provider's measured drain rate × the reclaim deadline |
| L3 | On client demand, stop provider admissions against the lease at once. The lease shrinks only as provider items ACK, and the client's admission wait is bounded by L2's deadline. |
| L4 | Role transitions (provide on/off, connect on/off) end the lease. Outstanding provider items keep their charge until ACK: no double free, no reassignment. |
| L5 | Generation overlap: when a provider's transport generation changes, old-generation roots stay charged until their last share returns (the O3 rule), so two generations cannot both claim the leaf |
| L6 | Per-consumer fairness: under contention, cap each consumer sequence at max(floor, lendable / active consumers) |

The source map is right that the provider already uses the client-group Pack budget for handoff. Lending must count that use as already borrowed.

**Test.** A two-consumer provider at a full leaf with an active lease, then client demand. Assert:

- client admission within the deadline;
- no provider sequence exits;
- an exact ledger after close;
- Jain fairness above 0.9 between the two bulk consumers.

**Reject** if client p95 TTFB rises more than 20%, or the provider gains less than the lent bytes predict through `B·η / τ`.

---

## 8. Request 6: one minimal measurement per mechanism

| Mechanism | Minimal measurement | Reads as binding when |
| --- | --- | --- |
| Retained budget | Time admission is blocked on budget, with Used/Total at the block | Blocked most of the time with Used/Total above 0.9 |
| H1 serialization | Carrier writer busy time / wall time, and batch size, per carrier | Busy fraction above 0.8 |
| Write and ACK queue delay | Enqueue-to-write latency on the carrier send channel (sampled); `AckRouteWriteWaitDuration` already exists | p95 above RTT/4. On the phone it was 20 ms total over 20 s [E], so not this. |
| Outer TCP cwnd and ramp | Outer carrier TCPInfo every 100 ms: gVisor `TCPInfoOption` in fixtures, kernel `TCP_INFO` on the app's own socket on devices | cwnd × MSS / srtt ≈ the observed rate while the window has room |
| Allocator churn | `/cpu/classes/gc/total:cpu-seconds` and allocated bytes per delivered MB | GC CPU above 25% of one core at the plateau |
| Provider uplink | Outer send-queue occupancy (`SIOCOUTQ`) plus Transfer RTT under load against idle | Queue persistently non-empty and RTT inflating with load |
| Path disorder | Held arrivals per Pack, duplicate receipts per Pack, hold residence p95 | Held fraction above 0.1, or duplicates above 0.05 |

---

## 9. Request 7: real-device discriminants

### 9.1 Inner TCP, header-only, at the tun

The VPN already parses every inner packet. Per flow, with no payload or TLS:

- SYN MSS and wscale.
- The phone's advertised window over time (raw << scale) and zero-window events.
- Inner RTT: when the phone sends an ACK carrying `TSval = x`, record the arrival of the first data segment whose `TSecr ≥ x`.
- Delivered bytes per second.
- Which window or exit carried the flow (hashed per run).

**Classification rule:**

- Per-flow rate ≈ rwnd / inner RTT → **window-limited**, by the phone's rwnd or an inflated inner RTT.
- Inner RTT ≫ Transfer RTT → **queue or holds inside the provider path** (§4.2).
- Rate ≪ rwnd / inner RTT with a flat Transfer RTT → **sender-limited** at the provider (budget, uplink or origin).

One public run classifies every exit it touches.

### 9.2 Transfer, per window

All of these exist in memory today; most need only export:

- Transfer RTT min/mean/scaled.
- The window estimate (Window, Ceiling, Reason).
- The received advertisement.
- Negotiated lane count.
- Held arrivals (exported) and duplicate receipts (`PeerAudit.resend`, not exported).
- Held-bytes high-water and residence p95.
- Retained send items and charge (`ResendQueueSize`).
- Time-to-cumulative-ACK histogram.

### 9.3 Provider row, bounded

At window selection:

- Capability bits seen in the provider's first ACKs: window advertisement present, compact head, lane support.
- The platform's measured speed for that provider.

These hint at version and capacity without identities and without assuming public provider versions. Do not infer HTTP status from encrypted packets.

---

## 10. Request 8: test realism and adjacent failures

### 10.1 What the five-window screen lacks compared with public Fast.com

| Public condition [E] | Fixture today | Add |
| --- | --- | --- |
| Two exits per Fast.com run | One provider carries all streams | A two-exit arm with ordinary affinity, reporting per-exit rates |
| 855 B packets from re-segmentation | Synthetic origin at an offered 2 Gbps; reads are probably backlogged, giving full packets | An origin that writes 1,448 B segments at about 15 Mbps per flow |
| Heavy forward disorder and reverse holes | Clean, or configured loss on the access link | A two-carrier arm with unequal delay |
| Transfer RTT ~150 ms+ | 40 ms access | A 150 ms stratum |
| Concurrent POSTs and quality windows | Download-only body streams | One concurrent POST flow plus one browsing probe |
| Reverse charge across 12 windows | Single consumer client | Twelve windows, two active |

Five consecutive windows in one process remain nested samples, as the brief says. Cold-provider ramps should stay in the record, not be trimmed.

### 10.2 The 100-page workload

Each page-1 failure needs:

- the carrying provider (hashed);
- proof that the origin handler ran;
- the first-ping ACK and admission timeline;
- complete ownership release.

The selected-client trace that completed every request, with a 12.233 s page-1 first byte, failed its SYN-attribution validity check, so it is not yet a latency measurement. Keep bulk and liveness causes separate, as the brief says.

### 10.3 The NAT fixture repair

[C/E] The diagnosis is consistent with source:

- `ErrNatMemoryPolicy` for a custom generator on a budgeted graph.
- The legacy constructor dropped the error.
- Ready was marked with a nil provider.
- Only the local 262,144 B reservation was ever present.

The amended fixture uses the error-returning constructors and refuses readiness on error or nil.

Using the built-in default policy should still admit the documentation-range origins:

- `security/main.go:122-137` subtracts 192.0.2.0/24, 198.51.100.0/24 and 203.0.113.0/24 from the blocklist.
- The egress pre-check refuses only private, loopback, link-local, multicast and unspecified destinations.

Assert the policy's *Allow* reason in the positive control anyway. A completed dial alone would not show that the old custom wrapper was not what allowed it.

---

## 11. Corrections to the brief and its evidence files

1. **The exit count.** "Connected through a United States H1 exit": the run used two speed windows with different destinations (§1.1).
2. **`ReceiveQueueCommitCount`.** The 13,308 commits are held out-of-order arrivals, a lower bound (§1.3). They are the run's main disorder signal and should be reported as such.
3. **"Retained send items" in `WINDOW-CORRELATION.json`.** `localRetainedSendItems` and `retainedSendItemsAtEnd` are `RouteRetainedItemCount`. That value is published only with a new longest-silence record (`transfer.go:2045-2055`, `:2938-2958`) and is historical, not current. Rename the fields or replace them with `ResendQueueSize`.
4. **Retained memory was the send side.** "Root/client totals combine directions": in this run the client total was mostly *send*. The 14.0 MB peak and the 8.8 MB at 25 s were reverse retention, not receive (§1.6).
5. **The headline number.** The 22 Mbps is Fast.com's estimate. Page averages were 16.5 Mbps (whole), 19.6 Mbps (from first origin byte) and 27.8 Mbps (last 6 s).
6. **Checked and correct:**
   - 79 B/frame: 566,465 / 7,173 = 78.97.
   - The M4 counter increments before deferral.
   - 35.06 MiB = 36,761,632 B.
   - 2:1 total 4,473,924 + 2,236,962 = 6,710,886.
   - 26.2 Mbps = 5 × 131,072 × 8 / 0.2.
   - 55 GCs and 171.934 ms over 29.779 s.

   The brief's audit is also right that no 128 KiB constant gates download flight; the NAT's flight is the phone's rwnd minus outstanding (§4.1).

---

## 12. Residual risks and what this review did not do

- No runs. Every mechanism here is [H] until §4.4, §5.3 and §9.1 produce data.
- The 1.10–1.12 framing estimate is a calculation, not a measurement. A loopback capture of the same build would turn §1.4's 23% into a measured duplicate fraction.
- The source of the disorder is unknown: carriers, platform or provider endpoint.
- The share of the 8.8 MB send-leaf charge belonging to speed windows rather than quality windows is unknown.
- The provider rows, uplinks and RTTs of the two exits are unknown. Any of them could make §2's slow-uplink row the whole answer for this run.
- Encrypted-envelope funding (O6) is still unresolved from review 3 and sets k for the reserve.

---

## Appendix A. Evidence and arithmetic

**Files read** (prefix H as in the brief):

- `H/n6-direct-connected-ready.PREP.ju1qn2ii/{FINAL-RESULT-SEAL,PAIR-RESULTS,CONNECTED-ANALYSIS,DIRECT-ANALYSIS,WINDOW-CORRELATION,TIMING-GC-ADDENDUM}.json`
- The same directory's `source/analyze_pair.py`.
- `…/02-connected-n6-android64/device-result/h1-ioloop-…/{telemetry.ndjson, body/fast-1.json}`
- Commands `00065`/`00073` status and `00086` teardown diagnostics.
- C-prefix provider budget map, fixed-total plan/report, NAT repair report and diff, reverse-ACK READY/AUDIT; H-prefix late-bootstrap r6 handoff.

**Arithmetic**

- Ceiling table (§0.1):

  ```
  flight = leaf × η × factor
  Mbps   = flight × 8 / RTT
  ```

  - η: 0.218 (HEAD), 0.271 (H1-eight) [R3].
  - Factor: ×1.556 (2:1); ×2.45 (one-root bound, H1-eight) [R3].
  - Desktop and miner rows use message bytes with η = 1.
- Page averages:
  - Whole page: 39,706,199 × 8 / 19.239 s = 16.5 Mbps.
  - From first origin byte: (39,706,199 − 218,886) × 8 / 16.130 s = 19.6 Mbps.
  - Last 6 s: (39,706,199 − 18,685,527) × 8 / 6.046 s = 27.8 Mbps.
- Reverse recovery: (428 + 1,229 + 398 + 1) / 3,638 = 0.565; frames per write 7,173 / 3,638 = 1.97.
- Small-Pack charge: 3 × (2,048 + 12) + ceil1K(592 + 128 × 6 + 128) = 6,180 + 2,048 = 8,228.
  - 8,799,080 / 8,228 = 1,069.
  - 10,144,932 / 8,228 = 1,233.
- Unexplained Wi-Fi bytes: 64,439,489 − 1.12 × 44,528,541 = 14.57 MB, which is 22.6% of the Wi-Fi bytes.
- Lane collisions: 6,720 / 32,768 = 0.205 with no collision.
- Packet minimum at full size: 44.5 MB / 1,100 B ≈ 40,500, against 52,098 observed (+29%).
