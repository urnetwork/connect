# Independent review 2 of SIMPLERISSUE3: retaining 300+ Mbps without receive-admission stalls

Date: 2026-11-09. Independent review, not anchored on any other reviewer. I did not read `SIMPLERISSUE3-REVIEW1.md`, the four earlier review files, or the `*REVIEW-DECISIONS*` and `SIMPLERISSUE-REVIEW-VALIDATION.md` artifacts. `SIMPLERISSUE.md` and `SIMPLERISSUE2.md` were read as background only.

## 0. What this review is based on

- **Code read:** canonical `main` at `a321ff90`; the effective aggregate (`R/aggregate-core-reserve-immediate-r5/CANDIDATE.overlay.json`, 56 non-test mapped files resolved over `R/tree`); the frozen source of the historical fast reference; `git show d90ac7f4`. Where a function is the same in `main` and the aggregate I cite `main` as `file:line`. The aggregate's `transfer.go` carries the same text about 150 lines lower. `transfer_recovery_selection.go`, `transfer_fifo_repair.go`, `transfer_rtt.go`, `transfer_pending_eviction.go`, `transfer_receive_hold_promise.go` and `transfer_recovery_time.go` are byte-identical in both.
- **Artifacts read:** baseline `ACTUAL-COUNTER-READBACK.json` and raw `observed64.stdout`; cap `analysis/COMPARISON.json` and `RETAINED-CREDIT-SEAM.json`; `ACK-TIMING-READBACK.json` and the `analysis/root-*-wire.json` / `root-*-sender.json` traces; the shared-advertisement witness source and closure; `timeout-progress-freshness-r1/FIX.diff` and its control plan; the historical Fast5 JSON for both profiles; `sdk/mobile_memory_policy.go`.
- **Ran:** no Go build and no test. Only `git show`, directory listings, and read-only Python reductions over the JSON artifacts. Every number below is quoted from an artifact or is arithmetic on quoted numbers.
- **Tags:** **[code]** read in source, **[data]** read in an artifact, **[inferred]** reasoning from those, **[speculative]** plausible and unverified.

`R` = `/Volumes/data/urnetwork/temp/aggregate-flow-r15-r2`. `REF` = `/Volumes/data/urnetwork/synthetic-smoke-r1/recovery-performance-r1`.

## 1. Bottom line

1. **The fast reference predates the commit that made the client receive pump refuse instead of wait, and no writeup lists that as a known difference.** `REF` was frozen at 00:31 on 2026-11-08. `d90ac7f4` landed at 06:20 the same day and changed `Pack(receivePack, handoffTimeout)` to `Pack(receivePack, 0)`. The reference had lossless H1 reader backpressure and recorded zero gap recoveries on every provider. The current source refuses and drops instead. This is the cheapest causal experiment available: one line, diagnostic only. [code + data, section 3.1]
2. **Once the handoff can no longer wait, the advertisement has to be honest, and it is off by more than an order of magnitude.** Each lane-sequence is advertised 4,362,076 bytes, about 420 bulk originals. Its handoff queue holds 22 Packs, because eight lanes divide 170 slots. The shared RX pool holds about 143 such originals in total. [code + data, section 3.2]
3. **At 300 Mbps the receiver has 40 to 50 ms of slack; the path RTT is 40 ms.** So any scheme that never over-promises is capped near 215 Mbps on this rig with today's leaf, lane slices and cost ratio. "No refusals" and "300+ Mbps" cannot both hold unless the slack grows or refusals become cheap. This is arithmetic, not a missing mechanism. [inferred, section 3.4]
4. **The signed pending test does not fail because of the first stale deferral.** It fails because the ACK of the retransmitted head re-authorizes deferral of the last timed-out original, leaving a hole. Later originals then arrive as futures at a receiver that can take only current items, and each batch waits another full RTO. The deferral-off arm passes because it leaves no hole. [data, section 3.5]
5. **The freshness guard works in the fixture only at an exact-equality boundary created by virtual time.** In real time the ACK is applied after the send, the guard sees it as fresh, and the deferral stays. It is safe for healthy queues and close to a no-op in production. [code + data, section 3.6]
6. **The multi-second request tail looks like the 2 s cold RTO floor, per lane.** Every lane-sequence owns its RTT window, timers are armed at first write and never re-armed, and the provider now runs eight lanes. The passing baseline already contains 2.0x-second connection events. [data + inferred, section 3.7]
7. **About 40% of the retained cost is avoidable real memory.** The receive path keeps the carrier buffer and also a rounded-up copy of every frame. [code, section 3.8]

The smallest complete next change is therefore not a credit protocol. It is: confirm item 1 with a one-line diagnostic, then make the existing advertisement honest for the per-lane handoff and the shared pool, and make the remaining refusals cost one RTT instead of an RTO.

## 2. My understanding, and where I agree or disagree

A mobile client receives bulk downloads from several providers over hosted H1. All provider sequences share one client RX budget (4,362,076 B at 64 MiB), charged in exact retained bytes. A Pack that cannot be funded at predecode, or that finds its sequence's handoff queue full, is dropped and counted in `PackHandoffDropCount`. The sender recovers by SACK evidence or by timer. Runs are bimodal: about 300 Mbps with under 1% of originals dropped, or 4 to 70 Mbps with 15 to 38% dropped.

**Agree:**

- Two sequences can stay inside their advertised windows while shared RX is full. The witness sets RX to 2 x 30,544 and asserts each sequence is advertised the whole 61,088. [code + data]
- The 10,412 to 30,544 cost is real. I reproduce it from code: 1,872 metadata for eight frames, eight 2,048-byte frame roots (`transfer_receive_memory_scan.go:143-161`), one 12,288-byte carrier root (`transfer_receive_memory_scan.go:820`). [code]
- The cap can be crossed by one original: the send loop passes arrival 0 (`transfer.go:10316-10320`) to a strict `<` (`transfer_queue.go:282-284`). [code]
- `PackHandoffDropCount` mixes causes. [code: one predecode site and four handoff sites in the aggregate]
- Splitting free space equally among all live sequences would strand capacity.
- Root-only admission needs a progress guarantee at a full root.

**Disagree or would reframe:**

- *The list of known differences from the fast reference is incomplete.* Section 3.1.
- *"Encoded permission versus physical ownership" as the core mismatch.* The unit ratio is one of three factors. The advertisement is a static constant equal to the whole pool, per sequence (`transfer.go:13253-13262`), and it ignores the per-lane handoff bound entirely. With a ratio of 1.0 the witness still fails at three sequences.
- *"Stale progress grants an extra two-second deferral" as the cause of the 42,880 / 65,536 failure.* Section 3.5.
- *The freshness guard as the correction.* Section 3.6.
- *"Four seconds" as a property of the mechanism.* Both four-second numbers are 2 s + 2 s, where 2 s is `MinResendInterval`, the cold floor (`transfer.go:906`). The fixture's RTT is zero and `scaledRtt` treats a zero mean as no samples (`transfer_rtt.go:375-377`). On a warm 40 ms path the same events are 0.3 s + 0.3 s.
- *Strict retained-cost credit as the design to build.* `RETAINED-CREDIT-SEAM.json` proposes reserved, identity-bound grants. Section 3.4 shows a reserving scheme has a hard ceiling below the target at the current slack.
- *The cap experiment as support for ranking physical admission first.* It shrinks every queue about 100-fold, so it cannot tell RX exhaustion from handoff saturation or lane effects.

## 3. Evidence the writeup does not draw out

### 3.1 The reference predates the zero-wait pump [code + data]

- `REF/freeze-r1` is dated 2026-11-08 00:31 -0600 and `RESULTS-runtime-r1.json` 00:50. Its pump computes `handoffTimeout := ...packHandoffTimeout(transportType, carrierReliability)` and calls `self.receiveBuffer.Pack(receivePack, handoffTimeout)` (`REF/freeze-r1/source/connect/transfer.go:5144-5156`). For a reliable carrier that returns `ReliablePackHandoffTimeout`, default -1: wait for capacity or cancellation.
- `d90ac7f4`, 2026-11-08 06:20 -0600, "Keep shared receive admission from blocking independent ACK progress", replaced it with `self.receiveBuffer.Pack(receivePack, 0)` (`transfer.go:5154` today). The same commit deleted the comment "This restores stream backpressure without enlarging any queue."
- The SDK still sets both handoff timeouts to wait (`sdk/mobile_memory_policy.go:426-427`). The constant's comment records an earlier measurement of exactly this failure: a finite timeout "dropped 24 messages and pinned the 2-MiB reorder budget" (`sdk/mobile_memory_policy.go:213-218`). That setting is now inert for the shared pump.
- Reference run: `SelectiveGapWriteCount` is 0 on every provider in both profiles, so there were no receive-side holes at all. Protective deferrals: 724, 385 and 327 on three providers, with zero timeout writes on them.
- Current runs: `PackHandoffWaitCount` is 0 everywhere, and even the fastest window has 311 handoff-saturation drops.

What this means [inferred]. Before the commit, a full handoff queue stalled the H1 reader, TCP flow control pushed back through the relay, and the flight waited in socket and relay buffers that the Go budget never paid for. Nothing was dropped, so nothing needed Transfer-level recovery; the client's sampled RX peaks stayed at or under 183,264 bytes [data]. After the commit the pump keeps reading, so the same overload becomes drops, and it also keeps decoding into RX while the workers are behind.

I am not arguing the commit was wrong. `CODESTYLE.md` gives the reason, and the writeup lists "do not block the shared receive callback" as a constraint. The point is that the reliability layer above was tuned for a lossless reliable carrier: static advertisement, timer-led recovery, deferral that assumes a queue is draining. With waiting removed, those three assumptions are what now fail.

Caveat: other changes also postdate the reference (`84415854`, the R15 tree, the overlays, the provider policy). This is the most consequential unlisted one and the cheapest to test. It is not yet proven to be the cause.

### 3.2 What one lane-sequence is promised and what it can hold [code + data]

64 MiB profile:

| Quantity | Value | Basis |
| --- | --- | --- |
| Advertised window, per sequence | 4,362,076 B, about 420 bulk originals | Static `min(ReceiveQueueMaxByteCount, pool total)`, `transfer.go:13253-13262` |
| Handoff queue, per data lane | 22 Packs | 170 scaled slots divided by 8, `transfer_logical_lane.go:39-47`, applied at `transfer.go:16396`. Observed `PackHandoffMaxCount` = 22, maximum bytes 219,106 to 227,622 |
| Shared physical RX, all sequences | 4,362,076 B, about 143 bulk originals | 30,544 retained each |

- Adaptive deepening is switched off on mobile (`sdk/mobile_memory_policy.go:406-411`), so each saturation event is one refused Pack. That is why drops equal the saturation count in fast windows.
- The lane division is a fixed slice. A busy lane gets 22 slots while idle lanes strand the other 148. This is the kind of per-lane partition the writeup's constraints say to avoid.
- The 32 MiB profile has 11 Packs per lane and half the pool.
- Time to fill at 300 Mbps over eight lanes: a lane receives about 450 originals per second, so 22 Packs is 49 ms. The shared pool is 4,362,076 / (2.93 x 37.5 MB/s) = 39.7 ms. For 32 MiB both halve.

So the sender is told it may have roughly 19 times more outstanding on a lane than the lane's handoff can hold, and the receiver's total slack at the target rate is about one RTT.

### 3.3 The baseline counters show two regimes [data]

From `ACTUAL-COUNTER-READBACK.json`; provider side unless marked.

| Window | Mbps | Originals written | Client drops (share) | Saturation count | Timeout writes | Deferrals | Gap writes |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 1 | 39.7 | 7,059 | 2,691 (38.1%) | 191 | 1,667 | 3,412 | 1,077 |
| 2 | 70.1 | 10,758 | 1,666 (15.5%) | 74 | 1,276 | 1,591 | 873 |
| 3 | 305.9 | 41,773 | 311 (0.74%) | 311 | 14 | 0 | 310 |
| 4 | 231.1 | 31,282 | 36 (0.12%) | 36 | 14 | 218 | 39 |
| 5 | 254.7 | 34,313 | 20 (0.06%) | 19 | 52 | 206 | 19 |

- **Fast windows:** drops equal handoff-saturation events (311 / 311, 36 / 36, 20 / 19). Later Packs on the lane are accepted and SACKed, and a selective-gap write repairs the hole in about one RTT (310 gap writes for 311 drops in window 3).
- **Slow windows:** about 93% and 96% of drops are not saturation, so they are predecode refusals or the other handoff paths. In window 1, 58% of recovery writes waited for a timer, with two deferrals per timeout write.
- **Nearly every timeout write in a slow window was a real loss:** 2,891 recovery writes against 2,691 drops in window 1. The deferrals there were almost pure delay. `SelectiveGapWritesOfDeferredItems` = 472 confirms items were deferred and then proven lost.
- **In healthy windows deferral does its job:** 218 deferrals and 14 timeout writes in window 4.

Reading: the regimes differ in whether later Packs on the same sequence are accepted. When RX has room they are, SACK evidence exists, and repair takes one RTT. When RX is full they are refused too, no SACK evidence exists, and the timer path with its deferrals takes over. Saturation drops are the seeds; a full RX turns them into a collapse. That is a metastable system, which is why one source gives both 306 and 3 Mbps. [inferred]

### 3.4 The arithmetic that bounds every honest window [inferred from data]

Rig: access link 20 ms each way (`access_round_trip_ns` = 40,000,000), ACK compression 10 ms. Cycle T is about 50 to 60 ms.

- Flight for 300 Mbps: 37.5 MB/s x T = 1.9 to 2.25 MB encoded, which is 5 to 6.7 MB retained at 2.6 to 3.0x. The RX leaf is 4.36 MB.
- Ceiling when outstanding permission plus live owners never exceeds RX: RX / (ratio x T) = 4.362 / (2.93 x 0.055) = 27 MB/s = **217 Mbps** (194 to 268 across those ranges). At 200 ms RTT it is about 57 Mbps. Halve for 32 MiB.
- Cross-check from the cap run: 22 to 26 Mbps at an effective window of about 26.8 KB is 0.8 to 1.0 Mbps per KB of per-sequence window. An honest window of 22 Packs, about 200 KB, predicts 165 to 195 Mbps. 300 Mbps needs about 300 to 350 KiB per active sequence.
- The reference's 300 to 428 Mbps therefore ran with more in flight than the receiver could absorb, and was safe only because the reader could wait.

The sizing comment at `transfer.go:1059-1066` budgets an 8 MiB receive share in framed bytes. The mobile policy sizes the pool as `clientShare/10` clamped to 1.5 to 2 MiB scaled by target (`sdk/mobile_memory_policy.go:76-90`) and documents the unit mismatch as acceptable, "bandwidth after a loss, never occupancy" (`sdk/mobile_memory_policy.go:132-140`). That judgement was made while the handoff still waited.

The earlier "+28% RX" experiment moved the leaf to the clamp maximum, 5,592,405 B. Against a gap of this size it was too small to test the capacity hypothesis.

The cost ratio is shape-dependent: 2.93x for 1,300-byte frames, and 26,448 / about 4,450 = **5.9x** for the signed trace's 536-byte frames, because every frame from 257 to 2,047 bytes costs a 2,048-byte root. No fixed ratio is a safe bound; I agree with the writeup on that.

### 3.5 The failing default arm, event by event [data]

From `analysis/root-2-wire.json` (default) and `root-3-wire.json` (deferral off). Times are virtual seconds. "Decoded" means a `decoded_owner` row exists; every decoded DATA row is flagged current and paid from the head-recovery child.

| t | Default arm (fails) | Deferral-off arm (passes) |
| --- | --- | --- |
| 0.00 | #4-#8 arrive, none decoded. RX is 4,348,032 / 4,362,076, held by flow A. | Same. |
| 2.00 | All deferred. | #4 (Head), #5, #6 resent; ACK #4 forwarded; **#7 and #8 resent anyway.** All five decoded. #9 sent new, decoded. |
| 2.01-2.03 | - | #10-#21 sent new, all decoded as current. 65,536 bytes complete. |
| 4.00 | #4 (Head), #5, #6, #7 resent and decoded. ACK #4 forwarded. **The next wire event is new original #9, not #8.** #9-#12 arrive, none decoded. | - |
| 6.01-6.04 | #8 resent (Head), then #9, #10, #11, #12, one per ACK. New #13-#17 arrive, none decoded. | - |
| 8.01 | #13 resent. Delivered: #4-#13 = 10 x 4,288 = **42,880**. | - |

- With no hole, the receiver admits a long contiguous run through the head-recovery child while ordinary RX is full. The transfer finishes 30 ms after the first retry.
- In the default arm #8 is held back once the ACK for retransmitted #4 is applied, and new originals are released ahead of it. They are futures behind a hole, so they are refused.
- Each refused batch is then repaired one position per ACK, but only for items whose timer is already due. `applyFifoRepair` pulls a successor forward only if `item.deferralOutstanding` is set (`transfer_fifo_repair.go:158-160`), and `prepareFifoRepair` captures only `sendItems[1]` (`transfer_fifo_repair.go:78`). A fresh original refused at 6.01 keeps its untouched 2 s clock, which is why #13 goes out at 8.01.

The cadence is four or five originals per RTO, and 42,880 at the 8 s cut is what that predicts.

Not determined: which branch postponed #8. The sender rows cover only #4. The F12 branch fits, because after the ACK of retransmitted #4 `lastCumulativeAckTime` is newer than #8's `timeoutDeferAckTime` and the deferral count is unbounded for a paced FIFO (`transfer.go:3310-3314`). The probe-deadline branch (`transfer_recovery_selection.go:208-227`) is the other candidate. One sender row for #8 settles it. [inferred]

Also relevant: at time zero #4-#8 arrive before the worker has processed the sequence's own opening Packs. The head-recovery child needs the floor to have reached the arriving number and the contract to be published as established (`establishedCurrent`, aggregate `transfer_receive_memory_head_recovery.go:68-76`); neither is true yet, so the Packs must use ordinary RX, which is full. The same Packs are admitted as current when re-offered. Every new sequence's first DATA is in that position. [inferred from code and trace]

### 3.6 Why the freshness guard is a boundary artifact [code + data]

- The first timer is armed one `scaledRtt` after the write (`transfer.go:12670-12676`).
- `lastCumulativeAckTime` is when the sender applies the ACK (`transfer.go:14111`). The decision time is sampled after ACK application (`transfer.go:10111-10159`).
- So at the first firing, age = scaledRtt - (ackApplyTime - sendTime) + timer latency. Any cumulative ACK applied after the send by more than the timer latency is "fresh".
- In the diagnostic: `SendAtNs` 0, `LastCumulativeAtNs` 0, decision at 2,000,000,000, `ScaledRttNs` 2,000,000,000. The guard rejects on `2e9 < 2e9`. The control plan lists an ACK age of 1,999,999,999 ns as "keep deferral".
- On a real path the predecessor's ACK arrives about one RTT after the successor's send. At RTT 40 ms and a 300 ms floor the age is about 250 ms: fresh. Old and new code behave the same.

It does change one real case: an item armed with the cold 2 s floor whose lane has warmed by the decision retries at 2.0 s instead of 2.3 s.

**Safe?** Yes for the healthy deep queue. In the reference, every one of the 724, 385 and 327 deferrals coincided with `TimeoutResendWithRecentCumulativeProgress`, which is the guard's own predicate counted on the same branch (`transfer_recovery_selection.go:329-333`). [data]

**Effective in a collapsed window?** Mostly not. Window 1 has about 5,079 decisions (1,667 writes + 3,412 deferrals), 3,853 with recent progress. At most 1,226 of the 3,412 deferrals (36%) can change. Window 2: at most 780 of 1,591. Most harmful deferrals happen with fresh progress, which is the pattern in section 3.5. [data, bounds only]

### 3.7 Two-second quantization in the passing baseline [data + inferred]

Per-request phases from `observed64.stdout`:

| Event | Duration |
| --- | ---: |
| Window 3 loaded probe, TCP establishment | 2,079 ms |
| Window 3 bulk flow, dial | 2,091 ms |
| Window 1 bulk flow, TLS handshake on an established connection | 2,046 ms |
| Window 2 loaded probe, request write to header, reused connection | 2,019 ms |
| Window 4 / 5 loaded probe, TCP establishment | 1,045 / 1,041 ms |

`SIMPLERISSUE2.md` reports two more TCP establishments of 2.094 s and 2.101 s. The run's maximum first payload, 2.268 s, is the 2,079 ms establishment plus TLS and the request.

A 2 s event inside an established connection is unlikely to be an inner-TCP timer, which starts near 200 to 300 ms after a handshake sample. `MinResendInterval` = 2 s (`transfer.go:906`) is the only matching Transfer timer I found. Supporting code facts:

- Every `SendSequence`, so every lane, builds its own `RttWindow` (`transfer.go:7516-7524`). The aggregate's provider policy sets eight lanes (`R/aggregate-core-reserve-immediate-r2/source/sdk/device_local_provider_flow_isolation.go:21`).
- `resendTime` is set at first write and is not re-armed when the lane's first RTT sample arrives.
- Shared H1 evidence can only lengthen a first-write deadline (`transfer_recovery_selection.go:214-219`), and `sharedRawRecoveryInterval` deliberately keeps the cold floor for a first write (`transfer_recovery_time.go:19-27`).
- A new sequence's first DATA cannot use the head-recovery child until the worker has processed its opening (section 3.5), so under RX pressure it is the Pack most likely to be refused.
- Inner-TCP retries of the same bytes are ordered Packs behind the hole and cannot rescue it.

One refusal on a cold lane is about 2.05 s. Two are 2 + 4 = 6 s. Of the twelve stall durations reported across the three writeups, seven fall within 0.9 s above 6 or 8 s (6.04, 6.31, 6.77, 6.86, 8.24, 8.79, 8.86). The other five need a further one to two seconds from another phase. That is suggestive and not proof.

Caveats: I have not tied any stall to a cold sequence. The reused-connection case is not obviously a cold lane; a queue-inflated mean RTT would also give a 2 s `scaledRtt`. The roughly 1.04 s events look like a single inner SYN retry.

### 3.8 Where the retained bytes go [code]

`ReceivePack` keeps `TransferFrameBytes` until `messagePoolReturn` (`transfer.go:5131`). The comment at `transfer.go:1469-1472` says every queued Pack holds the carrier root, one pooled root per decoded frame, and the owner. The frames are already owned copies. The held-head repair path is a precedent for returning the carrier early and keeping only its encoded length (`transfer.go:5103-5110`).

| Variant, witness shape (8 x 1.3 KB) | Retained | Ratio |
| --- | ---: | ---: |
| Today: carrier + 8 frame roots + metadata | 30,544 | 2.93 |
| Return the carrier after owned decode | 18,256 | 1.75 |
| Keep the carrier and alias the frames | 14,160 | 1.36 |

## 4. Gaps and unverified assumptions in the writeup

1. **The old/new comparison omits `d90ac7f4`.** The stated differences are provider accounting, lanes, the mobile constructor and provider populations.
2. **No per-cause drop classification in bulk.** The ranking asked about in question 6 rests on it.
3. **The collapse trigger is unidentified and stochastic.** One process per treatment; the same source passes and fails. Nothing makes the old-source failure deterministic.
4. **Only one point on the window curve was measured** (16,385). The interesting region is 128 KiB to 512 KiB.
5. **The lane factor was prepared and never run.** `R/provider-accounting-lanes-qualification-r1` and `-r2` hold only run skeletons. Lanes multiply the number of full-pool advertisements, divide the handoff to 22 Packs, and multiply cold RTT windows.
6. **RTO state at a stall is not recorded:** cold or warm, the interval that armed the timer, deferral and send counts.
7. **The freshness controls run under virtual time at selection level.** None applies the ACK a nonzero interval after the send, which is the production case.
8. **The four-second recovery in the two-sequence witness is the cold-floor artifact,** and its RX is sized to exactly two originals. It proves a mechanism, not a magnitude.
9. **The strict-grant contract has not been checked against the throughput target.**
10. **Deployment asymmetry is not discussed.** Download-direction senders are providers, many on CLI builds with their own cadence. Receiver-only changes ship with the app.

## 5. Root-cause hypotheses, ranked, each with a cheap test

**H1. Removing the reader wait turned transient overload into drops, and the layer above assumes a lossless carrier.** Likelihood: high as the origin of the regression, unproven.

- Test: on the current aggregate, restore the pre-`d90ac7f4` wait for the pump's `Pack` call only, leaving the ACK handoff at zero wait. Diagnostic only, one five-window Fast64 run, like the cap run. Prediction: client drops near zero, provider gap and timeout writes near reference levels, and no bimodal collapse. Throughput may sit below the reference, because a 22-slot lane now stalls the pump sooner than the reference's undivided lane-zero queue did. If the run still collapses, H1 is wrong and the search moves to the provider side.

**H2. The advertisement over-promises against both the per-lane handoff and the shared pool, so nothing slows senders when the receiver falls behind.** Likelihood: high as the enabling condition (code).

- Test: sweep the existing `AdvertisedReceiveWindowMaxByteCount` setting over 131,072 / 196,608 / 327,680 / 524,288. Predictions from the cap run's slope, assuming the same seven or eight active sequences: about 125 / 185 / 300 / path-limited Mbps. Saturation drops should stay near zero up to about 196,608 (22 Packs) and rise above it. If drops stay high at 131,072, standing window is not the cause.

**H3. Retransmission-driven cumulative progress re-authorizes deferral and lets new data pass an unrepaired hole.** Likelihood: high for the signed test (trace), probable as the amplifier in slow windows.

- Test A, the existing failing test:

| Variant | First retry | Completion of 65,536 bytes |
| --- | --- | --- |
| Old default (observed) | 4.00 s | fails, 42,880 at 8.01 s |
| Deferral off (observed) | 2.00 s | about 2.03 s |
| Freshness guard only | 2.00 s | not near 2.03 s. Same per-cycle pattern one RTO earlier; by my reading about 64,320 at the 8.01 s cut, so pass or fail by one original |
| FIFO write-order proof only (6.3) | 4.00 s | about 4.03 s |
| Both | 2.00 s | about 2.03 s |

  If the freshness-only arm completes near 2.03 s, H3 has the mechanism wrong.
- Test B: delay delivery of ACK #3 by 1 ms of virtual time. Prediction: old source 4 s; freshness guard still 4 s.

**H4. The cold per-lane RTO floor sets the multi-second quantum for the latency gate.** Likelihood: medium.

- Test: providers only, `MinResendInterval` = `RttMinResendInterval` (300 ms), one diagnostic run. Prediction: the 2.0x s connection events disappear and no request exceeds about 1.5 s, with bulk unchanged. Persisting 5 to 9 s stalls falsify it.

**H5. Eight lanes amplify H2 and H4,** through 22-Pack handoff slices and eight cold timers per provider. Likelihood: medium, untested. Test: the prepared lane-zero run.

**H6. A consumer pause of 40 to 50 ms is the trigger.** Likelihood: speculative. Go GC under a 64 MiB limit, scheduling, or a slow TUN write would do it. Test: record the largest gap between consecutive deliveries in the receive worker and GC pause totals per window. Deterministic form: the stall injection in question 8.

**H7. Provider TX retention or ACK-slot pressure as primary causes.** Likelihood: low. ACK drop counters are zero. `bridgeRefused` rises with throughput (4,171 in window 1, 34,907 in window 3), which is ordinary backpressure.

## 6. Proposals, smallest first

### 6.1 Make the existing advertisement honest (receiver only, existing setting)

Advertise, per sequence, no more than its handoff queue can hold minus one maximum original. With the current slices that is about 22 Packs, roughly 190 KiB for bulk. The existing `AdvertisedReceiveWindowMaxByteCount` setting (`transfer.go:15454-15457`) is enough to try it. Old senders already follow the field: `observeReceiveWindowAdvertisement` stores the latest value on every ACK (`transfer.go:13192`) and the estimate clamps to it. The call-site comment even calls the field "what this receiver can still hold" (`transfer.go:17070-17078`); only the implementation is a constant.

One limit: the handoff bound binds by count and the window is in bytes, so a run of small Packs can still exceed 22. That is acceptable for a first step, since bulk originals are near the envelope size and refusal remains safe.

Section 3.4 says this alone gives about 165 to 195 Mbps. To reach 300 with honest windows the receiver needs about 300 to 350 KiB of handoff per active lane, which leads to 6.2.

### 6.2 Give the slack back without raising the root

- **Share the handoff count across lanes** instead of dividing 170 by 8. The byte side is already a shared budget; the count slice is the stranding partition.
- **Return the carrier at owned decode** (2.93x to 1.75x). This is a real memory reduction.
- **Re-derive the RX leaf** from ratio x target rate x (RTT + ACK compression) inside the unchanged root, instead of `clientShare/10`. The root never exceeded about 4.8 MB of 43.6 MB at any cut. At 1.75x and 55 ms, 300 Mbps needs 3.6 MB; at 100 ms it needs 7.2 MB, 16.5% of the root.
- Later: add pool classes between 256 and 2,048 bytes, which matters for small segments (5.9x today).

### 6.3 Sender: FIFO write-order loss proof

On a stable H1 route, if the receiver acknowledged a copy written at time W, every Pack written before W on that route has reached the receive pump. If one is still neither acknowledged nor SACKed, it was refused. `transfer_fifo_repair.go` implements this for `sendItems[1]` only, and only to cancel an outstanding deferral.

- Keep the exact-envelope tag match as the proof condition.
- Record the proof as a fence time for the route generation, and treat every retained, un-SACKed item on that route written before the fence as passed. `shouldDeferTimeoutResend` already refuses to defer a passed item (`transfer.go:3306-3308`).
- Make a passed item due at once whether or not it holds a deferral, in sequence order, ACK-clocked, with a small burst bound.

Healthy queues keep their protection: with no retransmission there is no promoted envelope and no proof, and a spurious retransmit is acknowledged with the original copy's tag. Note that `laneAcknowledgedPast` compares sequence numbers (`transfer.go:3272-3278`); after a retransmission, sequence order and write order differ, and that is the gap. `ReliableLaneProvenRecovery` is off by default (`transfer.go:1011`), so the lane rules are not in the shipping path.

### 6.4 Sender: stop paying the cold floor on a warm destination

- When a sequence's RTT window gets its first sample, re-arm retained first-transmission items to `max(now, lastWrite) + scaledRtt`. One pass, once per sequence.
- Seed a new lane's RTO from the destination's shared H1 round-trip evidence when it exists. The lanes share one carrier and already share its pacing clock.

Test: two lanes to one destination, lane 1 warm, lane 2 fresh, 40 ms simulated RTT, first DATA on lane 2 dropped once. Old: retry at 2.0 s. New: at or under 0.4 s.

### 6.5 Receiver: a pressure level for the shared pool

- **Normal:** the honest per-sequence value from 6.1.
- **Pressure:** `heldEncoded + one original`. The sender's debt includes held SACKed originals, so this means "repair the hole, send nothing new past it". Retransmissions are not window-gated.
- Enter on low RX free space or any predecode refusal; leave above a high watermark.

Two constraints I checked in the sender:

- **Use hysteresis and few levels.** Every increase stamps `receiveWindowSetAtNanos` (`transfer.go:13185-13191`), and delivery-rate sizing discards samples older than that step (`transfer.go:13547-13551`, `13741-13747`). A value that rises on every ACK would freeze window learning on existing senders.
- **A window value is observed only on an ACK that names a still-retained item** (`transfer.go:8513-8531`). Leaving Pressure needs such an ACK, or the sender's next original, which is always allowed at debt 0.

### 6.6 Refusal notice with no schema change

At a predecode or handoff refusal the receiver knows the sequence and number, provided the receive sequence already exists. Put the number on the existing eviction list.

- **Old senders are inert:** `resendEvictedItemWithLock` returns unless the item is `selectiveAcked` (`transfer_pending_eviction.go:142-145`).
- **The notice survives an ACK whose message is no longer retained:** `coalesceReceivedAck` publishes eviction feedback on that path (`transfer.go:8514-8520`).
- **New senders** treat a notice for a retained, un-SACKed item as "not at the receiver": no backoff, immediate if it is the oldest unacknowledged item, otherwise when its predecessor is acknowledged. Ignore a notice arriving less than one minimum RTT after the item's last write.
- **Receiver:** drop a pending notice for a number it later admits; reuse the existing bounds (4,096 pending, 512 per ACK, `transfer.go:16161-16162`).

It retires, credits and acknowledges nothing, so it cannot become a false receipt. A stale notice costs one duplicate. It is the only repair here that helps a provider with no other traffic to the client.

### 6.7 What I would not build yet

The reserved-grant protocol in `RETAINED-CREDIT-SEAM.json`. It is exact, but it caps throughput at RX / (ratio x T), strands reserved bytes in transit by design, and needs negotiation. Predecode admission is already the safety authority, so a soft signal has nothing to reclaim or revoke.

## 7. Direct answers to the eight questions

**1. Does the freshness correction keep the healthy-deep-queue protection? Which case shows an unacceptable increase?**
It keeps it wherever cumulative ACKs keep arriving, which is the reference-run case. The regressing case: a healthy deep queue, one ACK silence of 1.5 x scaledRtt, no loss, sender still writing. Old source: zero timeout writes. Fixed source: every item written in the first half scaledRtt of the silence is retransmitted. At the 300 ms floor that is a 450 ms receiver pause. The larger issue is that the guard does not fix its own scenario once the ACK is applied after the send (3.6).

**2. Simplest global mechanism that prices retained ownership and uses idle capacity? Current peers or negotiation?**
An honest value on the existing field (6.1), a pressure level (6.5), and predecode admission unchanged as the authority. Works with current senders. Idle sequences hold permission, not bytes. Do not put retained pricing on the wire: the ratio runs from about 2.6x to 5.9x by frame shape today and depends on the receiver's allocator.

**3. Can sender admission price the next original before publication?**
In encoded bytes, yes, to within a few bytes: frame lengths, chunk limits, the header and whether a contract frame is due are known before serialization. Use `debt + next <= window`, with one original always allowed at debt 0. Protocol Packs bypass as today. Unknown then: growth if a later retry is promoted to a full-contract Head, legacy wrapping, and the receiver's retained cost. The overshoot matters only when the window is small.

**4. Invariants for issuing and reclaiming credit.**
- I1. Predecode physical admission is the only admission authority. The advertisement creates no obligation, so nothing is revoked.
- I2. Never advertise below `heldEncoded + 1`. Held custody is never out of bounds.
- I3. Never advertise above what the sequence's handoff can hold.
- I4. An arrival that is its sequence's receive floor is fundable from the head reserve, sized by a maximum legal original that the receiver enforces.
- I5. Raise only across a hysteresis band; lower at once.
- I6. A quiet or new flow needs no grant: one original may always be sent at debt 0, and it arrives as the current head.
- I7. The value rides only on genuine ACKs and never names an unheld message.

**5. Is refusal feedback the smaller first repair for multi-second tails?**
For the tail, sender RTO hygiene (6.4) is smaller and needs no wire participation. Refusal feedback (6.6) is more complete. Coexistence: inert once the item is SACKed or retired; idempotent under duplication; a reordered notice costs one duplicate; it rides the compressed ACK cadence; old decoders are unaffected; bounded lists drop under pressure and the timer remains.

**6. Does the cap experiment justify prioritizing physical admission? Cheapest falsifier?**
Not alone. The baseline counters are better evidence: non-saturation drops are about 93 to 96% in slow windows and about 0% in fast ones. The cheapest falsifier of the whole ranking is the H1 diagnostic. Next is a drop counter split by cause. Lane count and physical admission are not independent: lanes set the handoff slice and multiply the advertisements.

**7. Removing overlapping mechanisms; which legacy fallback stays?**
Once 6.1, 6.3 and 6.6 are measured: tail-sized hold-promise reservations (use a fixed maximum-original head reserve), tentative-eviction memory pressure, the held-head repair escrow, the F12 special cases and the disabled lane rules, then the provider first-DATA reserve. Keep: safe predecode refusal, the static value for old receivers, eviction notices for SACKed custody, and the plain RTO as the last backstop.

**8. A short aggregate experiment before the full suites.**
Make the trigger deterministic. Same harness and policies, one bulk download across four providers, a 2 KiB GET every 500 ms alternating fresh and reused connections, 60 s, and a **150 ms stall injected into the client delivery callback at t = 5 s and t = 25 s**.

| Outcome | Pass condition |
| --- | --- |
| Byte identity | SHA-256 of every bulk body and GET matches the origin |
| No false receipts | Every sender-retired message has exactly one receiver delivery record; no ACK names a number that was refused and not later admitted |
| Bounded memory | RX, head-recovery and root peaks within caps; all budgets refund to zero at join; runtime sampler under the profile gate |
| Throughput | Mean at or above 250 Mbps; back to 80% of the pre-stall rate within 1 s of each stall |
| Latency | No GET above 1.5 s; no 2.0x s events |
| Old-source failure | After the stall: timeout writes in the hundreds, recovery longer than 3 s, at least one GET at or above 2 s |

If the old source does not fail under an injected stall, H6 is wrong about the trigger, which is worth knowing first.

## 8. Prioritized next steps

1. **Run the H1 diagnostic:** restore the handoff wait at the pump, five windows. It decides whether the rest of this review is aimed at the right layer.
2. **Run the advertisement sweep** (H2), the provider cold-floor run (H4) and the prepared lane-zero run (H5).
3. **Add instrumentation with no behavior change:** split `PackHandoffDropCount` by cause; record interval, RTT sample count, send and deferral counts at each timeout write; add a sender row for #8 in the signed diagnostic.
4. **Run the freshness-only arm and the 1 ms ACK-delay variant** of the signed test before investing further in that guard.
5. **Ship the honest advertisement** (6.1) with a one-lane deterministic test: worker held at a barrier, old source refuses the 23rd Pack, new source stops the sender at 22 and resumes within one ACK of release.
6. **Implement the FIFO write-order proof** (6.3) against the existing signed pending test, default deferral on, and the RTO re-arm and seeding (6.4) against the two-lane test.
7. **Return the carrier at decode and share the handoff count** (6.2), then re-measure the ratio and decide the leaf from 3.4.
8. **Add the pressure level and the refusal notice** (6.5, 6.6) with the two-sequence witness as oracle.
9. **Run the stall-injection experiment,** then the five-window, full100 and device suites.

Steps 1 to 4 are measurements. Each can remove a branch of this plan, so they come before any new mechanism.
