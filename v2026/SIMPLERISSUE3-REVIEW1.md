# Review 1 of SIMPLERISSUE3: receive-admission stalls versus 300+ Mbps

Date: 2026-11-09. Independent review of [SIMPLERISSUE3.md](SIMPLERISSUE3.md). I did not read any other review file. SIMPLERISSUE.md and SIMPLERISSUE2.md were read only for background, and claims quoted from them were checked against code and data where possible.

## 0. Method and provenance

- **Source reviewed: the effective M4 aggregate, not canonical `main`.**
  - I built a frozen copy of `/Volumes/data/urnetwork/temp/aggregate-flow-r15-r2/tree/connect`, overlaid with the 36 `connect/` donor files listed in `aggregate-core-reserve-immediate-r5/PRODUCTION-MAP.json`. All donor SHA-256 values matched the map's `afterSha256`, and READY.json matched `42f2397f`.
  - Unless stated otherwise, `connect/*.go` line numbers refer to that effective tree:
    - `transfer.go` is the r2 donor, sha256 `eb50186201e1…`.
    - `transfer_receive_memory_head_recovery.go` is the `receive-ordinary-prefix-fix-r1` donor.
    - `sdk/` citations are from `tree/sdk`.
  - Canonical `main` differs for several of these files.
  - Caution: while I copied it, the shared `tree/` was being re-composed by another process. Overlay symlinks appeared and disappeared. Anyone reviewing "the tree" live can see transient files.
- **Evidence parsed:**
  - M3 r5 baseline:
    - `CLOSED-READBACK.json` `3f71d804`
    - `ACTUAL-COUNTER-READBACK.json` `c4bcb0a0`
    - the raw `run/observed64.stdout` perfvar record `3dfb3989…`, which holds per-0.5 s throughput samples, per-flow provider masks and the native-bridge admission counters
  - Cap run: `CLOSED-READBACK.json` `18b66d18`, `run/FINAL.json` `ea3456b5`, and the `run/cap64.stdout` perfvar record.
  - Two-sequence witness: `CLOSED-READBACK.json` `9f4288ed`, its raw stdout, and its test source `receive-shared-advertisement-witness-r1/connect/transfer_receive_shared_advertisement_test.go`.
  - ACK-timing reduction `2d153300`, including `analysis/root-2-sender.json` and `analysis/root-3-sender.json`.
  - Historical reference `RESULTS-runtime-r1.json` `29d92b62`, plus its `fast5-64-desktop128/stdout` (go-test JSON).
- **I ran no Go tests and changed no source.** All numbers below are read or derived from those files by parsing JSON.
- Labels used below:
  - **[V]** verified in code or data.
  - **[I]** inferred from verified facts.
  - **[S]** speculation that needs a test.

## 1. Bottom line

1. **[V] Both stated mechanisms are real. Neither is the whole story, and the second is smaller in production than the report implies.**
   - Each receive sequence is advertised the *entire* shared RX total in encoded bytes, while each 8-frame H1 Pack is charged about 2.9x its encoded size. Finding 1 is therefore true by construction; the code comment says the field is not a physical promise.
   - The "2 s then 4 s" numbers in both runtime witnesses come from synthetic time. Every RTT sample is 0 there, so `ScaledRtt` falls back to the 2 s cold floor.
   - In production, each of those steps costs one `ScaledRtt`, i.e. max(300 ms, 2 × mean RTT), unless the sequence has no samples yet.
2. **[V] The most informative data was not used in the report: the 0.5 s time series inside each window. It shows a metastable collapse, not a slow steady state.**
   - Window 1 ran at 96-166 Mbps until the adaptive client went from 4 to 8 streams at t = 2.0 s.
   - It then fell to 2-6 Mbps and never recovered. The streams launched at 2.0 s waited 2.74 s, 3.52 s and 4.20 s for their first payload.
   - Window 2 began with 4.35 MB of RX still occupied from window 1. It ran at 1-6 Mbps for 2.5 s and recovered only after about 6 s.
   - Once RX saturates, the system stays saturated for seconds. That points at **recovery dynamics after a silent receiver drop** (cold 2 s floor per new lane sequence, deferral, exponential backoff, windows that never shrink), as much as at admission arithmetic.
3. **[I] The decision-time freshness fix is probably correct but is not the bulk lever.**
   - In window 1, provider timeout decisions with *fresh* cumulative progress (3,853) outnumber all deferrals (3,412).
   - At most about 1,226 deferrals (about 36%) could have been stale, and possibly none were. The bound for window 2 is about 780 (about 49%).
   - Most deferrals therefore happened with fresh progress, where the guard changes nothing. My hypothesis is that deferral is wrong for a refused item *regardless* of freshness, because "the prefix is still draining" does not mean "this item is queued" once the receiver drops items silently.
4. **[V] The cap experiment is confounded in two ways the report does not mention.**
   - **Carrying providers changed.**
     - Baseline masks: 13, 9, 2049, 5, 5 (2-3 providers, with up to 6 streams on one provider).
     - Cap masks: 3790, 1672, 648, 648, 2572 (3-8 providers).
   - **The upload ACK path changed.**
     - The baseline's native ingress refused 92,950 packets in pure-ACK-only batches (of 116,359 pure ACKs offered) and 10 of 52 SYN offers.
     - The cap run refused 0 pure ACKs and 0 SYNs.
   - Cap throughput is exactly window/RTT: about 4 MB per flow per 10 s ≈ 16 KiB per 40 ms. So the run shows the expected flight ceiling, but it cannot rank RX against the ACK path or provider TX.
5. **[V] An upload-side regression is unexamined.**
   - In the 64 MiB historical reference, native ingress refused only 656 packets in pure-ACK-only batches out of 174,962 pure ACKs offered. Provider timeout resends were 0, 0, 0, 8, 4, and each window kept ramping to 400-500 Mbps.
   - The current baseline refuses about 80% of the app's pure ACKs at a *ready-only* native offer, and 19% of SYN offers.
   - [I] The 1.04 s TCP setups in windows 4-5 match gVisor's 1 s initial SYN retransmit timeout after one refused SYN.
6. **[V] Physical amplification has a cheap, local cause: the decrypted carrier stays alive for the item's whole queue residence.**
   - After decode, the carrier is redundant with the per-frame payload copies; only diagnostics read it.
   - Releasing it at handoff cuts the measured Pack from 30,544 to 18,256 retained bytes (-40%). No wire change is involved.
7. **Recommended reframing.**
   - Stop trying to make admission lossless with exact physical credits. Admission already guarantees memory safety, because refusal is always safe before a SACK.
   - What is missing is that a refusal must be **rare** and **cheap**:
     - **Rare:** lower the amplification, let RX use more of the 89%-idle root, isolate per-sequence out-of-order occupancy, and use an approximately right advertisement.
     - **Cheap:** recover a refused original in about 1 RTT, not 2-8 s.
   - That avoids nearly every hard problem listed under "Shared credit must represent complete retained cost".

## 2. What I verified, with citations

### 2.1 Advertisement versus physical charge (Finding 1)

**Advertisement.**
- [V] `receiveWindowAdvertisement` (transfer.go:13401-13417) returns `min(ReceiveQueueMaxByteCount, receiveQueue.Budget().TotalByteCount(), cap)`. Every sequence on a pooled client is therefore advertised the whole RX total.
- Its comment states the field "does not promise retained-cost or shared-sequence credit".
- [V] The SDK policy comment at `sdk/mobile_memory_policy.go:132-140` acknowledges the unit mismatch. It argues the overflow "is bandwidth after a loss… the eviction notice turns into resends".

**Why that argument fails for refusals.**
- [V] A pre-admission refusal emits no eviction notice.
- Even if one were sent, current senders drop notices for un-SACKed items: `resendEvictedItemWithLock` returns early when `!item.selectiveAcked` (transfer_pending_eviction.go:143).
- Refused originals are therefore *silent*, and recovery is purely timer-driven unless later items are SACKed.

**Where the 30,544-byte charge comes from.** [V] It is reproduced exactly from code for an 8-frame, 10,412-byte H1 Pack:

| Component | Bytes | Source |
| --- | ---: | --- |
| Decrypted carrier root (pool class) | 12,288 | `MessagePoolRootByteCount(carrier)`, transfer_receive_memory_scan.go:824 |
| 8 payload copies, each ≤ 1,440-byte inner-MTU packet rounded to 2,048 | 16,384 | copy classes at transfer_receive_memory_scan.go:144-162; copies at frame_protobuf.go:1050 (`f.MessageBytes = MessagePoolCopy(v)`) |
| Owner metadata for 8 frames: 1,024 + 6×128 + (8×8+16) | 1,872 | transfer_receive_memory_scan.go:169-201; transfer.go:1471 |
| **Total** | **30,544** | ratio 2.93 of encoded |

**The carrier is redundant after decode.**
- [V] The carrier is copied onto the queued `receiveItem` (`transferFrameBytes: receivePack.TransferFrameBytes`, transfer.go:18271). It is released only when the whole owner is released (frame_protobuf.go:772-790; transfer_receive_memory.go:327-383).
- After the payload copies, I found no receive-side reader of `receiveItem.transferFrameBytes` except diagnostics (`ReceiveQueueSizeAndMessageTypes`).
- The held-head repair path already returns the carrier at handoff (transfer.go:5143-5150). Early release is therefore already an accepted lifecycle.

**How RX is sized.**
- [V] RX is exactly one tenth of the DATA root: 43,620,760 / 10 = 4,362,076. This follows from `mobileReceiveQueueBudgetByteCountForTarget` taking clientShare/10.
- Sampled root use peaked around 4.7 MB, about 11% of the root. The RX leaf is the restrictive child; the root is not.

### 2.2 Recovery timing (Finding 2 and its neighbours)

**Timer settings.**
- [V] Settings at transfer.go:912-922: `MinResendInterval` = 2 s (the cold floor), `RttMinResendInterval` = 300 ms, `MaxResendInterval` = 8 s.
- [V] At transfer.go:981-982 and 1017-1019: deferral is on, `TimeoutResendDeferLimit` = 2, `DeferTimeoutResendBackoff` = true, `ReliableLaneProvenRecovery` = false.

**Per-sequence RTT and the cold floor.**
- [V] Each `SendSequence` constructs its own empty `RttWindow` (transfer.go:7582). Every new lane or generation therefore starts on the 2 s cold floor until its first ACK sample.
- [V] An initial original is armed at `sendTime + ScaledRtt` (transfer.go:12718-12721).
- [V] `scaledRtt` uses the cold floor when the *mean* RTT is zero (`if useRtt == 0`, transfer_rtt.go:375), not only when there are no samples. Under `testing/synctest`, every sample is 0, so every "warm" sequence still runs on 2 s.
- The witness test runs inside `synctest.Test` (transfer_receive_shared_advertisement_test.go:20). Its raw output shows `elapsed=4s` for 0.00 s of wall time.
- In the ACK-timing sender rows (`analysis/root-2-sender.json`), `ScaledRttNs` is 2,000,000,000 at every decision, including after ACK #3.

**Deferral.**
- [V] The default deferral predicate is transfer.go:3312-3335, called from transfer_recovery_selection.go:327-351.
- The predicate checks freshness relative to `item.sendTime`, never the decision time. Finding 2 is correct.
- With `stablePacedFifo`, the deferral limit is bypassed (`pacedFifo || count < limit`).
- Each deferral backs off (transfer.go:2841-2849, 9228).

**Head re-arm.**
- [V] The cumulative-ACK head re-arm (transfer.go:14381-14399) pulls the new head's timer to `ack + probeRtt` only when `head.deferralOutstanding || head.sendCount > 1`. An untouched initial original keeps its send-time timer.
- [V] All SACK-driven probes require selective ACKs above the gap (transfer.go ~8900-9062). If everything after a refused head was also refused, none of them fire.

**Window growth.**
- [V] Per the comment at transfer.go:13569-13571, the sender window starts at the configured opening, and "ordinary feedback may grow it; only an explicit quality remeasurement may authorize a reduction".
- I did not trace every reduction path. As written, receiver refusals never shrink a sender's flight.

### 2.3 The receiver can already take a resent head

[V] `takeReceiveHeadRecoveryOwnerInSession` admits an established sequence's exact-current original from the Head child, "even without a later owner in its own list" (transfer_receive_memory_head_recovery.go:56-81, `establishedCurrent` at line 68).

[I] So once a refused head is *resent*, the receiver generally admits it, even when RX is full. The multi-second part of a refusal is spent waiting on sender timers.

### 2.4 What `PackHandoffDropCount` mixes

[V] There are five call sites:
- RX owner allocation refusal (transfer_receive_memory_decode.go:113).
- Fresh-opening key mismatch (transfer.go:5138).
- Zero-timeout handoff into a full per-sequence channel (transfer.go:5194-5199).
- Two generation-replacement paths (transfer.go:16077, 16142).

[V] Per-window data:
- In fast windows 3-5, drops equal saturation events almost exactly (311/311, 36/36, 20/19).
- Non-saturation drops are about 2,500, 1,592, 0, 0 and 1.
- That is a cleaner slow-window signal than the report allows, though still not a site attribution.

### 2.5 Send overshoot

[V] The send loop checks `CanAddWithQueueByteCount(0, …, window)` (transfer.go:10445-10449). That evaluates `debt + 0 < window` (transfer_queue.go:285-287). Overshoot is therefore at most one H1 envelope (16,384 encoded bytes) per sequence, as reported.

### 2.6 Native ingress is ready-only

**Code.** [V] `ip_native_ingress.go:1-14`: "Native TUN admission makes one ready remote offer without adding a packet queue… Refused remote TCP packets remain the source TCP's recovery responsibility." Everything not accepted is returned to the pool and dropped (ip_native_ingress.go:140-208).

**Data.** [V] Bridge counters:

| Run | Packets offered | Refused | Pure-ACK-only batch refusals | SYN offers refused |
| --- | ---: | ---: | ---: | ---: |
| Baseline M3 | 137,040 | 104,347 | 92,950 of 116,359 pure ACKs | 10 of 52 |
| Cap | 22,622 | 2,335 | 0 | 0 of 42 |
| 64 MiB reference | 221,343 | 25,264 | 656 of 174,962 pure ACKs | (not recorded) |

## 3. Gaps, unverified assumptions, likely-wrong conclusions

**G1. The "4 seconds" and the failing eight-second companion bound measure a synthetic-time artifact.**
- [V] Both runtime witnesses run with zero RTT, so the initial timer and the deferral each equal the 2 s cold floor.
- The *mechanism* is proved. The *magnitude* is not.
- In production the same chain costs: initial `ScaledRtt` + one deferred `ScaledRtt`, with exponential backoff after that.
- Suggestions:
  - Run timing witnesses with a simulated non-zero RTT, such as a 20 ms forward delay in the fixture.
  - Or make `scaledRtt` treat "samples exist, mean 0" as warm. Today it does not (transfer_rtt.go:375); `RttEstimate`'s own comment warns about exactly this ambiguity.

**G2. The two-sequence witness demonstrates possibility, not frequency or classification.**
- [V] It sizes RX to exactly two owners (test lines 22-31). It deliberately withholds both current originals and parks two future tails at a worker barrier.
- The refused Pack is a *future* original (number tail+1, lines 210-216).
- In bulk, we do not know whether refusals hit heads or futures, which site refused them, or how long each refused original took to be readmitted. That is the missing measurement (section 7, P0).

**G3. The freshness correction has a small upper bound in bulk.**
- Arithmetic in Appendix A.
- The larger deferral population had fresh progress. With silent receiver refusals, fresh cumulative progress does not mean "this item is queued behind the prefix". It can mean "the prefix is being repaired while this item was dropped too".
- Each refused item in a burst can then be deferred again as its predecessors are repaired. That gives roughly one `ScaledRtt` (backed off) per refused position.
- [I] This matches window 1's roughly 2 deferrals per timeout write (3,412 deferrals against 1,667 writes).

**G4. The report reads only the end-cut RX and the window aggregates.**
- The start-cut RX together with the 0.5 s samples is more discriminating (Appendix B). Window 2 started with RX full and spent its first 2.5 s near zero.
- "An end-cut RX level alone does not distinguish slow from fast" is true, but carried-over occupancy plus the time series does.

**G5. The cap experiment cannot "support admission pressure as a mechanism" against the alternatives.**
- It changed provider placement and the upload ACK regime too (section 1, item 4).
- Its throughput equals N_flows × 16 KiB / 40 ms, the arithmetic ceiling. It says nothing about intermediate caps.

**G6. The 300-400 Mbps reference and the current runs differ in the very component that now overflows.**
- [V] `mobileMemoryPolicyEnabledForPlatform` requires `mobile == true` (sdk/mobile_memory_policy.go:268-272).
- [I] The reference used host/mobile-runtime selection on a darwin host (per SIMPLERISSUE.md), so it most likely ran *without* the 4.36 MB retained-accounted RX pool.
- The current fixture logs `"HostMobileRuntime":false,"MobileConstructor":true`.
- The earlier +28% RX experiment was far too small to test this. Each heavy sequence is allowed MBs of window, and a full window of out-of-order data at 2.9x is several MB.

**G7. The upload path is not held constant.**
- About 80% pure-ACK refusal versus 0.4% in the reference, plus SYN refusals.
- [S] This could cap the "fast" windows at about 300 Mbps (the reference ramps to 400-500 Mbps). It is also a second source of second-scale connection setup:
  - 1.04 s TCP setups in windows 4-5 match one refused SYN under gVisor's 1 s handshake timer (`tree/gvisor/pkg/tcpip/transport/tcp/connect.go:41`, with doubling backoff).
  - The 2.08 s setup in window 3 does not fit gVisor's 1 s then +2 s schedule. It does fit a Transfer cold-floor recovery (2 s + RTT) on the SYN/SYN-ACK path, but that is unproven.

**G8. Exact physical credit is framed as a precondition. It is not.**
- Memory safety already comes from admission. Liveness and performance need refusals to be rare and cheap, not impossible.
- Linux TCP faced the same "payload window versus truesize memory" problem. It advertises a *measured* payload/truesize ratio (`scaling_ratio`, formerly `tcp_adv_win_scale`), prunes out-of-order data under pressure, and relies on fast loss recovery.

**G9. Smaller items.**
- The proposed guard `0 <= cumulativeAge` treats an ACK applied *after* the decision timestamp was sampled as stale. That is the opposite of the intent; count negative age as fresh, or assert it is impossible.
- No window reduction on refusal (section 2.2) means senders refill RX with full windows as soon as it drains.

## 4. Root-cause hypotheses, ranked

### H1 (most likely: multi-second tails and collapse persistence): silent receiver drops recovered only by slow, growing sender timers

**Chain.**
1. RX saturates, typically at a ramp such as 4 → 8 streams.
2. Arrivals are refused silently, or the handoff channel is full.
3. If no later item is SACKed, recovery waits for:
   - (a) the item's send-time timer, which is 2 s on any new lane sequence and `ScaledRtt` otherwise;
   - (b) a deferral that is granted on stale *or fresh* prefix progress;
   - (c) exponential backoff when the retry is refused again.
4. Out-of-order successors hold RX meanwhile. Windows do not shrink, so senders refill RX as soon as it drains.
5. The state persists across windows (window 2).

**Evidence.**
- The window 1/2 time series.
- Non-saturation drops only in slow windows.
- About 2 deferrals per timeout write.
- New-stream TTFB of 2.7-4.2 s.
- Per-sequence cold RTT windows.
- The Head child admits resent heads, so latency sits with the sender.

**Cheap experiments.**
1. **Instrumentation, no behavior change.** In a small fixed table, record (sequence, number) → first refusal time at each refusal site. On first successful admission of the same number, emit a histogram of refusal-to-admission delay, plus counts by site and by head/future.
   - Prediction if H1 holds: in slow windows the delay is at or above 1-2 s and dominated by future-then-head chains on recently created sequences.
2. **One diagnostic arm, window 1-2 cold start, three repetitions:**
   - set `MinResendInterval` to 300 ms (diagnostic stand-in for RTT seeding);
   - set `DeferTimeoutResendWhileCumulativeProgress=false`.
   - Prediction: no persistent collapse; any dip recovers within about 1 s; no new-stream TTFB above 1 s.
   - Falsified if collapse persists with similar duration.

### H2 (likely: why refusals happen at all, and the about 300 Mbps edge): effective RX ≈ 1 BDP while each sequence may flight several MB

**Arithmetic.**
- RX 4.36 MB at 2.93x ≈ 1.49 MB encoded.
- BDP at 300 Mbps × 40 ms ≈ 1.50 MB.
- Each sequence's window ceiling is the whole RX total, and the delivery-sized window rule targets about 2 × delivered per RTT.
- A single hole in one heavy sequence can therefore fill RX with its own out-of-order data and starve every other sequence's in-order arrivals.

**Evidence.**
- Exact cost decomposition (section 2.1).
- Cap run per-flow rates equal window/RTT.
- The SDK comment concedes the mismatch.
- The reference client likely lacked the pool (G6).

**Cheap experiments.**
1. **RX-share arm.** Raise the RX leaf to 3x (about 13 MB, still under 30% of the unchanged 43.6 MB root), leaving advertisement, lanes and timers unchanged, with placement pinned.
   - Prediction if H2 is primary: slow windows disappear and throughput approaches the reference.
   - Falsified if collapse persists. This is a diagnostic, not a proposal to raise limits.
2. **Advertisement-cap sweep** at {128 KiB, 256 KiB, 512 KiB} with placement pinned.
   - Prediction at 40 ms with about 8 active sequences: throughput ≈ min(path, 8 × cap / RTT), i.e. about 200, 400 and over 400 Mbps.
   - Drops should stay near zero below about 4,362,076 / (2.93 × 8) ≈ 186 KB per sequence and rise above it.
   - With carrier release (ratio 1.75), the zero-overflow cap moves to about 312 KB.

### H3 (plausible: the "fast window" ceiling and some loaded-request latency): ready-only native ingress refuses most upload ACKs and some SYNs under load

**Evidence.** Section 2.6 counters, and the 1.04 s setups.

**Experiment.** Add reason counters at the native offer (not ready, collapse, policy, other). Then run one arm where:
- pure ACKs that cannot be offered are kept in a per-flow single "latest ACK" slot, sent when the sequence becomes ready;
- SYN/FIN/RST get a bounded wait instead of ready-only refusal.

Prediction: the fast-window ceiling rises toward 400 Mbps and the 1 s setups disappear. Falsified if neither moves.

### H4 (confounder): provider placement and population

**Evidence.**
- Throughput clusters by the carrying set. Slot 3 appears only in slow windows 1-2.
- Current runs put up to 6 of 8 streams on one provider; the reference spread them over 3-6 providers.

**Experiment.** Pin placement (for example 4 streams × 2 fixed providers) in every arm. This is necessary for all the other experiments to be interpretable with one process per arm.

### H5 (real, minor in bulk): stale-progress deferral

**Experiment.** Add a counter for "deferral granted with decision-time age ≥ scaledRtt". If it stays well below total deferrals in slow windows, the freshness fix will not move bulk numbers, though it remains correct for the unit case.

## 5. New ideas and simpler alternatives

### 5.1 Make a refusal cost about 1 RTT (sender-side, no wire change)

1. **Seed new-sequence RTT from sibling sequences to the same destination.** Use other lanes or earlier generations, like a per-destination RTT cache. This removes the 2 s cold floor for new lanes whenever any sibling has samples. Put any new interval in settings, per CODESTYLE.
2. **Exposed-head rule for stable FIFO lanes.**
   - When a cumulative ACK exposes a head, re-arm it to max(ack time, its physical write + min RTT) + a small slack, *including* `sendCount == 1`. This is a pull-in, never a push-out.
   - Do not grant a timeout deferral to an exposed head while the carrier is demonstrably alive, meaning an ACK from that peer on that carrier arrived after the head's physical write.
   - Keep deferral only under carrier-wide silence, which is what protects the H1 TCP-stall case (Q1).
   - This subsumes the freshness guard for heads.
3. **No exponential backoff for heads under endpoint-drop evidence (alive carrier).** Backoff is for path silence. A receiver refusal is cleared by resending the head, which the Head child admits.
4. **RACK on stable FIFO lanes, plus a "TLP-head" probe.**
   - Mark lost any un-SACKed original physically written more than a small reorder window before an item that has been acknowledged (cumulatively or selectively). FIFO makes the reorder window close to the ACK-compression interval. Retransmit it without deferral, paced.
   - If no ACK arrives for about 2 × SRTT with data outstanding, resend the *head* as the probe. The Head child can admit it even when RX is full. Its ACK then RACK-marks every earlier-written, un-SACKed item.
   - A refused burst then recovers in about 2 RTTs instead of one backed-off interval per position.
   - This would also make lane-proven recovery (§34.3, off by default) and most of the F12 deferral unnecessary.
5. **Multiplicative window decrease on endpoint-drop evidence; growth from delivery.** Senders sharing one client RX then converge toward about RX/N without the receiver dividing credit, the way TCP converges on a shared bottleneck.

### 5.2 Make refusals rare (receiver-side, no wire change)

1. **Release the carrier at handoff.**
   - Better still, charge it to a pump-scoped transient reservation and exclude it from the owner's retained claim. That avoids adjusting loaned claims mid-life.
   - Result: 30,544 → 18,256 bytes (-40%). RX holds 1.67x more Packs at the same physical budget.
2. **Optional: a 1,536-byte copy class.** Payloads are at most 1,440 bytes (inner MTU 1,440). This gives 8 × 1,536 + 1,872 = 14,160 (1.36x encoded). It touches the global pool, so measure it.
3. **Let RX draw on the root minus a progress reserve.** This is the stated "one shared resource" direction. The root is about 89% idle at the measured peaks.
4. **Dynamic-threshold isolation for out-of-order data**, as in shared-memory switch buffers.
   - Admit a *future* original for sequence s only if `future_s + cost ≤ α × free_RX`.
   - Exact-current originals use the Head child, and in-order arrivals are exempt.
   - One sequence's hole can no longer refuse everyone else's in-order data, while a lone active sequence can still use most of RX.
5. **Approximately right advertisement.**
   - `W_s = clamp(ratio_s × α × RX_total / N_active, W_min, hold)`.
   - `ratio_s` is an EWMA of encoded/retained over admitted Packs.
   - `N_active` counts sequences with held bytes or arrivals within about one RTT.
   - Never retract a granted right edge; shrink only by withholding growth.
   - Current senders already clamp to the field, so no capability negotiation is needed.

### 5.3 Upload path

Replace ready-only refusal of pure ACKs with latest-wins per-flow coalescing: one slot per flow, bounded, replacing older ACKs, preserving new SACK/window information. Do not refuse SYNs ready-only; use a bounded wait or a tiny queue. Measure against H3.

### 5.4 Simplification path

Once 5.1 makes a refusal about 1 RTT, safe refusal before SACK becomes the default tool. The tail-sized hold promises, prefix loans, held-head escrow and provider first-DATA reserve then become optimizations to justify with counters, not liveness requirements (Q7).

## 6. Direct answers to the questions

**Q1. Does the freshness correction preserve healthy deep-queue protection? Which case shows unacceptable retransmission while fresh progress exists?**

It preserves the healthy case. In a draining FIFO, consecutive cumulative ACKs are milliseconds apart, far below the 300 ms floor. With *truly* fresh progress it behaves exactly as today.

Two cases regress:
- **Carrier-wide stall.** Write N items on H1, then stall *all* deliveries and ACKs for 1.5 × scaledRtt to simulate an underlying TCP RTO or HOL stall, then release.
  - The new guard sees stale progress and retransmits every item whose timer fires during the stall, up to N duplicates queued behind their originals.
  - The old rule defers most of them.
  - Fix: decide on carrier liveness (any ACK from the peer on that carrier after the item's write), not on this sequence's progress alone.
- **Negative age.** An ACK applied after the decision timestamp was sampled is treated as stale.

Also see G3: the guard is not where the bulk deferral population lives.

**Q2. What is the simplest global admission/credit mechanism that prices retained ownership and uses idle capacity, and does it need negotiation?**

No new credit protocol. Use:
- the 5.2 advertisement (ratio-scaled, active-share, monotone right edge);
- dynamic-threshold isolation at admission;
- the carrier release.

All work with current peers and the existing encoded field. Exact retained-unit windows would require negotiation and a shared, versioned cost function. They are not needed once refusal is cheap.

**Q3. Can sender admission price the next complete original before publication?**

Yes. Either:
- check `debt + 16,384 ≤ window`, the bounded H1 envelope, which is conservative and has no unknowns; or
- after selecting the Pack, check `debt + nextSerializedMessageByteCount() + bound(framing, contract, encryption) ≤ window`.

The unknowns at that point are all bounded: whether a head/contract rewrite is added (bounded by the current contract's size), the per-frame AEAD/framing overhead, and the group chunk (pinned by `pinLogicalGroupChunkLimits`). Protocol stays outside because `NoBudget` Packs are not windowed. For windows of 128 KiB or more, the overshoot is at most 12.5% and not worth much complexity.

**Q4. How should credit be issued and reclaimed? Invariants:**

- **(A) Safety.** Admission never exceeds a physical budget, and refusal is pre-SACK only. This already holds.
- **(B) Head liveness.** An established sequence's exact-current original is always admissible, via the Head child; add a small opening reserve for fresh sequences.
- **(C) No false receipts.** Refusals never produce cumulative or selective ACKs. Any notice is advisory and tied to an exact identity.
- **(D) Isolation.** Per-sequence out-of-order occupancy ≤ α × free RX.
- **(E) Monotone right edge.** Never retract (cumulative ACK + window). Reclaim by not advancing.
- **(F) Convergence.** Senders decrease multiplicatively on endpoint-drop evidence and restart from the initial window after idling longer than the RTO (in the style of RFC 7661). Stale grants of quiet flows then cannot burst, and new flows always get the initial window plus Head admission.

Legacy senders get (A)-(E) only.

**Q5. Is authenticated refusal feedback the smaller first repair? How does it coexist with SACK, compression, reordering, old decoders and pressure?**

The smaller first repair is RACK on FIFO plus the TLP-head probe: sender-only, no wire change, no new authentication surface.

If notices are added later:
- **Wire.** Use a new repeated Ack field of (sequence number, refused transmission's tag send time). Old decoders ignore it.
  - Reusing `EvictedSequenceNumbers` is inert for current senders, because they ignore un-SACKed items (transfer_pending_eviction.go:143), but it is semantically ambiguous.
- **Emission points.**
  - transfer_receive_memory_decode.go:109-114, only when `sequence != nil` and the frame was decrypted or the session does not require encryption.
  - transfer.go:5194-5199.
  - Both queue via a bounded per-sequence ring, like `noteEviction`, and wake the ACK worker.
- **Sender action.** Act only on a retained, un-SACKed item whose latest transmission tag is no newer than the notice; anything else is stale and ignored. The action is a paced resend with no backoff increment, plus a window decrease.
- **Pressure.** Under pressure, drop notices; timers remain the backstop. Coalesce into ranges and cap per ACK.

**Q6. Does the cap experiment justify prioritizing physical admission over lanes, provider TX or ACK-slot pressure? What one measurement most cheaply falsifies that?**

No; see G5. The cheapest falsifier is the RX-share arm from H2: RX alone relieved, everything else identical, placement pinned.
- If collapse persists, RX admission is not primary.
- Pair it with the H3 ACK-path arm to split the ceiling from the collapse.

**Q7. How can the overlapping promise/recovery/reserve mechanisms be removed, and which legacy fallback remains?**

1. Add "saves" counters to each mechanism: admissions that would otherwise have been refused.
2. With 5.1 in place, run the existing counterfactual overlays (the `OLD-*` pattern) one mechanism at a time.
3. Delete each one whose removal leaves refusal-to-admission latency and throughput unchanged.

What must remain:
- For **legacy senders**, which keep cold/backoff timers: Head-child admission for exact-current originals, and dynamic-threshold isolation.
- For **legacy receivers**: new senders must not depend on notices; RACK/timers are the fallback.

**Q8. Which short aggregate experiment should precede the full suites?**

- **Setup.** Cold start, two pinned providers, 4 streams at t=0 rising to 8 at t=2 s, 15 s, 3 repetitions per arm.
- **Arms.** Old M3 versus candidate.
- **Pass:**
  - after the ramp, sustained ≥ 250 Mbps with no 0.5 s sample below 50 Mbps;
  - new-stream TTFB < 1 s;
  - refusal-to-readmission p99 < 2 × RTT + 300 ms;
  - non-saturation refusals < 1% of Packs.
- **Correctness:**
  - per-stream SHA-256 byte identity;
  - zero false receipts, checked by joining every acknowledged message id to a receiver admission;
  - all budgets refund to zero at join, with RX, Head and root peaks recorded at ≤ 100 ms resolution.
- **Old-source failure.** M3 must reproduce the collapse (some 0.5 s sample below 10 Mbps after t = 2.5 s, or new-stream TTFB above 2 s) in at least one of three repetitions. If it does not, the fixture lacks power and the shape needs strengthening, for example 12 streams or a 2-provider squeeze.

## 7. Prioritized next steps

### P0: measurement only, no behavior change

- Per-site refusal counters (the five call sites), split head/future.
- Refusal-to-readmission delay histogram.
- Stale-deferral counter.
- Native-ingress refusal reasons.
- 100 ms RX/Head/root time series.
- Fixture provider pinning.

### P1: three diagnostic arms, windows 1-2, three repetitions each, pinned placement

- (a) 300 ms cold floor plus deferral off (H1).
- (b) RX leaf ×3 under the unchanged root (H2).
- (c) Advertisement-cap sweep at 128 and 256 KiB (H2 frontier).
- Then (d) ACK-path relief (H3) if (a)-(c) leave the fast-window ceiling near 300 Mbps.

### P2: code, each with a deterministic test that fails on old source

1. **Carrier release at handoff.**
   - Test: the existing two-sequence witness at RX = 2 × 30,544.
     - Old: the third original is refused.
     - New: it is admitted at 18,256 (3 × 18,256 = 54,768 ≤ 61,088).
     - A fourth original is still refused.
   - Byte identity and zero refunds must hold.
2. **New-sequence RTT seeding**, plus treating "samples exist, mean 0" as warm.
   - Test: a warm lane at 40 ms simulated RTT; a new lane's first Pack is refused.
   - The retry must come within about 300 ms, not 2 s.
3. **Exposed-head rule and no backoff under an alive carrier.**
   - Test: FIFO; receiver accepts 1-3 and refuses 4-10; RX frees after ACK 3.
   - Item 4 must be resent within `probeRtt` of ACK 3.
   - Include the carrier-stall control from Q1, where no spurious burst may occur.
4. **Fold the report's freshness guard into item 3**, with negative age counted as fresh.

### P3

- RACK on FIFO plus the TLP-head probe, and multiplicative decrease on endpoint drops.
- Revisit explicit notices only if a measured gap remains.

### P4

- Ratio-scaled active-share advertisement and dynamic-threshold isolation.
- Then the mechanism-retirement study (Q7).

Do not start full five-window, full100 or device suites until the Q8 short experiment passes with an old-source failure reproduced.

## Appendix A: arithmetic

**Cost ratio.**
- 30,544 / 10,412 = 2.93.
- Carrier released: 18,256 / 10,412 = 1.75.
- With a 1,536 class as well: 14,160 / 10,412 = 1.36.

**RX in encoded terms.**
- 4,362,076 / 2.93 ≈ 1.49 MB.
- BDP at 300 Mbps × 40 ms = 37.5 MB/s × 0.04 s = 1.50 MB.

**Cap run.**
- 16,385 B / 0.040 s = 3.28 Mbps per sequence.
- Observed per flow: 3.8-4.5 MB / 10 s ≈ 3.0-3.6 Mbps.
- About 7-8 flows give 22-26 Mbps, as reported.

**Freshness bound for window 1 (provider).**
- Timeout decisions reaching the deferral branch ≲ 3,412 deferrals + 1,667 timeout writes = 5,079.
- Decisions with fresh progress = 3,853. Stale decisions ≤ 1,226.
- Stale deferrals ≤ min(3,412, 1,226) = 1,226, i.e. ≤ 36%.
- Window 2: (1,591 + 1,276) − 2,087 = 780 stale, ≤ 49% of 1,591.
- Writes on cold sequences, with no cumulative ACK yet, bypass the branch, which only tightens these bounds.

**Native ingress.**
- Baseline: 92,950 / 116,359 = 79.9% of pure ACKs in refused pure-ACK-only batches.
- Reference 64 MiB: 656 / 174,962 = 0.37%.

## Appendix B: within-window evidence (Mbps per 0.5 s)

| Run | Providers | RX used at start | Samples |
| --- | --- | ---: | --- |
| M3 window 1 (cold) | {0,2,3} | 0 | 14, 132, 96, 166, **(4→8 streams)** 43, 2, 6, 41, 25, 51, 34, 48, 25, 7, 20, 6, 23, 26, 21 |
| M3 window 2 | {0,3} | 4,350,912 | 6, 2, 1, 5, 2, 17, 27, 30, 62, 23, 31, 101, 115, 127, 137, 139, 138, 157, 145 |
| M3 window 3 | {0,11} | 30,544 | 21, 103, 162, 267, 321 … 395, 408, 362, 377, 374, 385 |
| M3 window 4 | {0,2} | 3,732,912 | 40, 172, 196, 201, 218 … 206, 288, 295, 288 |
| Cap window 1 | 8 providers | — | 2, 5, 12, 25, 28, then a flat 29-31 |
| Reference 64 window 1 | 6 providers | n/a | 45, 166, 284, 363 … 432, 447 (later windows end at 418-516) |

**Provider recovery deltas, M3 windows 1-5.**

| Window | 1 | 2 | 3 | 4 | 5 |
| --- | ---: | ---: | ---: | ---: | ---: |
| Timeout writes | 1,667 | 1,276 | 14 | 14 | 52 |
| Deferrals | 3,412 | 1,591 | 0 | 218 | 206 |
| Selective-gap writes | 1,077 | 873 | 310 | 39 | 19 |

The reference's timeout resends were 0, 0, 0, 8 and 4.
