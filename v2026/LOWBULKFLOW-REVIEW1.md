# Low bulk throughput: independent review 1

Date: 2026-11-03. Reviewer: Claude Opus 5.5 (`claude-opus-5-5`). I worked independently of any other review of this brief.

## Inputs examined

- `LOWBULKFLOW.md`, the brief.
- Source at the brief's frozen revisions:
  - Connect `92b77b7b`. The tree is clean apart from the untracked brief.
  - SDK `2ae5419a`.
  - Server `35654c4b`, specifically `server/connect/perfvar`.
  - gVisor: the `../gvisor` fork that `connect/go.mod` replaces in, at `57c9e13`.
- The V5 fixture packet `synthetic-fast-v5-final.PREP.d_1e3c37/`.
- `synthetic-fast-sdk32-pair-analysis.PREP.c1bdihd0/PAIR-ANALYSIS.json`.
- Both raw five-run logs, decoded with the brief's record-concatenation rule:
  - `synthetic-fast-v5-sdk32-five.PREP.mo_z47ck/stdout.jsonl`
  - `synthetic-fast-candidate-five-startup60.PREP.poqtvdcy/stdout.jsonl`
- `PACKETFLOW-STALL-RESPONSE2.md`. Its M2 and F1 already describe the provider NAT's fixed-timer, one-segment recovery, which is unchanged at this revision.
- New experiments. All are connect-only unit tests added with `go test -overlay` from a scratch directory. No repository file other than this one was created or modified. Sources and commands are in Appendix A.

## Evidence tags

- **[S]** Source fact. Paths are in `connect/` unless prefixed.
- **[R]** Runtime fact, from the fixture artifacts or from my experiments.
- **[H]** Hypothesis, stated with how to falsify it.

---

## 0. Summary

### 0.1 What limits throughput

1. **The low averages come from a duty cycle, not a slow rate.** [R]
   - Each new stream delivers about one receive window and then crawls for seconds.
   - Most bytes arrive in a few 500 ms samples that run at 40–90 Mbps.
   - Between bursts, each stream gains about one 1,048-byte TCP segment per round trip. That matches how the provider NAT repairs a hole in the inner TCP stream: one segment per partial ACK, with no SACK.
   - So the runs spend most of their time repairing losses that no captured counter shows.

2. **The losses: Transfer drops provider return packets after admitting them.** [S][R]
   - The fixture gives each provider a 2 MiB resend budget with retained-byte accounting.
   - `SendSequence` materializes each provider return group in chunks of about 3 KiB.
   - If a later chunk cannot reserve its retained charge, the sequence drops that chunk, disposes of the rest of the group, and still reports success (`transfer.go:8289-8291`, via `11265-11306`).
   - The only signal is `ErrSendPackNotAdmitted` on the group's ack callback. `RemoteUserNatProvider` passes `func(err error) {}` for that callback (`ip.go:9531`). It assumes that admission moved custody to Transfer (`ip.go:9362-9365`).
   - No counter records the disposal.
   - The NAT has already advanced its sequence and kept a copy. The app therefore sees a multi-segment hole, which only the NAT's slow recovery repairs.

### 0.2 Evidence

3. **I reproduced it with the production provider path and no server, H1 or emulator.** [R]
   - Setup: a real `RemoteUserNatProvider`, a real `LocalUserNat`, two Transfer clients and a gVisor app TUN. RTT 40 ms, 2 Gbps links, `GOMAXPROCS=2`.
   - Without the fixture's provider budget policy this setup runs at 344 Mbps (2 flows, no budget). With only that policy applied, 8 flows ran at 2.1, 12.5, 20.0 and 21.8 Mbps over four runs. The burst/crawl shape matched the fixture.
   - Between 11% and 42% of the packets the provider reported as sent were never written by Transfer.
   - The same 2 MiB budget with plain accounting ran at 348 Mbps.
   - A 32-packet recovery flight, with the losses still present, ran at 79 Mbps.

4. **The fixture's own counters already record the same losses.** [R]
   - In 20 of the 25 carrying-provider rows across the ten runs, the return packets reported sent exceed Transfer's `InitialFrameCount` by more than 512 frames.
   - 512 is the most a provider sequence can hold admitted but unwritten at teardown: 32 pack slots × 16 frames.
   - The overall gap is about 12% in both arms. My reproduction gave 11.6%.

5. **Why the captured counters show nothing.** [S]
   - The app TUN is the receiver of the download, so its retransmit and timeout counters cannot see download loss.
   - A disposed chunk never becomes a Transfer item, so it never appears as a Transfer resend or a refusal.
   - NAT replays are not counted at all.

6. **Why the candidate did not help.** [S] All three of its changes are on the receive side. The losses happen on the provider's send side.

### 0.3 Other limits and relevance

7. **There is a second limit, separate from the losses: the fixture's provider budget caps each provider at about 80 Mbps.** [R][S]
   - Retained accounting charges a pack of 1–4 frames 4.6–8.5× its payload.
   - So 2 MiB holds only about 250–550 KB of payload in flight.
   - In the reproduction, 2 and 4 flows both ran flat at 82 Mbps.
   - That is below the 100 Mbps goal even with no stalls.

8. **Neither CPU nor the core packet loop is the bottleneck.** [R]
   - The existing connect TCP harness uses the same NAT, Transfer and TUN code. At 40 ms with `GOMAXPROCS=2` it runs 816 Mbps with 2 flows and 917 Mbps with 8.
   - It omits encryption, H1, the server and the emulator, so it says nothing about their capacity.
   - But the collapse reproduces without any of them.

9. **This probably matters in production.** [S][H]
   - Every SDK-built provider has a parented send budget (`sdk/device_local_transfer_memory.go:28-30`).
   - `transfer.go:7137-7144` forces retained accounting on for any parented budget.
   - So this path is live for app-based providers in the field.
   - The physical 25 s and 9 s plateaus are consistent with hundreds of disposed segments repaired one per RTT. The providers in those sessions were not identified, so this is unverified.

### 0.4 Confidence and next step

10. **Confidence.**
    - High: admitted provider returns are disposed of mid-run in the fixture, and that disposal alone produces the fixture's shape.
    - Medium-high: it is the dominant cause in the fixture. It has not yet been A/B tested in the fixture itself, and other hole sources are not excluded.

11. **Cheapest confirmations in the real fixture** (Section 6):
    - Set `providerReturnRecoveryMaxPacketCountForTest = 32`. This is an existing plan knob. Prediction: several times faster, with the losses still present.
    - Set provider `ResendQueueRetainedByteAccounting = false` at the same 2 MiB. Prediction: the collapse disappears.
    - Add a disposal counter at `transfer.go:8290` and `11296`. Prediction: nonzero for every carrying provider.

---

## 1. What the samples show

The pair analysis reports flat intervals. The raw sample series shows the shape more clearly. Each line below is KiB delivered per 500 ms sample; the stream count in effect is in brackets. [R]

```text
candidate run 3 (4.92 Mbps): 1664[1] 416[2] 32 0 32 32 32 32 0 2944 64 0 64 0 0 480 0 64 0
candidate run 1 (11.49 Mbps): 416[1] 416[2] 0 64 0 32 32 0 2944 3296 1120[4] 0 96 128 2496[8] 128 192 928 64
baseline run 1 (2.47 Mbps):   416[1] 416[2] 0 32 32 32 32 0 832 864 32 0 64 0 32 32 0 64 0
baseline run 4 (17.39 Mbps):  416[1] 1792[2] 480[4] 2048[8] 32 192 480 512 2016 1952 2112 2240 2432 2112 64 64 192 672 448
```

1. **Each new stream delivers about one window, then stops.**
   - In 8 of the 10 runs, the first sample is 416–448 KiB. That is one opening window from the app's gVisor stack: a 1 MiB default receive buffer, half of it advertised.
   - The second sample is usually the second stream's first window.
   - Progress stops as soon as a stream needs anything beyond its opening window.

2. **The path is not slow when it moves.**
   - Burst samples carry 2.4–3.6 MiB, and up to 5.5 MiB (90 Mbps).
   - The averages are low only because those samples are rare.

3. **The crawl has a specific rate.**
   - Between bursts, 2-stream runs average about 23 KiB per sample.
   - One 1,048-byte segment per round trip of about 45 ms, per stream, gives 2 × 1,048 × 500/45 ≈ 22.7 KiB.
   - Crawl samples with 4–8 streams scale the same way, to 64–128 KiB.
   - Other mechanisms don't produce this rate. A zero-window probe carries 1 byte. A window-limited flow whose application keeps reading does not settle at exactly one MSS per RTT.
   - Partial-ACK hole repair does produce it (Section 2.5).

4. **"Flat" is quantized.**
   - Every sampled `validated_bytes` value is a multiple of 32,768. The per-flow totals are exact.
   - So a "flat 500 ms interval" means less than one 32 KiB block across all streams, not zero bytes.
   - Most of the crawl falls below that resolution.

5. **The adaptive ramp mixes up speed with stream count.**
   - Each added stream contributes a first-window burst before it stalls.
   - So 8-stream runs score higher than 2-stream runs even though each stream behaves the same way.
   - Fixed-stream runs would separate the two (Section 6).

---

## 2. Mechanism (source walk)

### 2.1 The provider NAT emits whole windows and keeps a copy [S]

- The socket reader packetizes data up to the app's advertised window (`ip.go:6115-6141`). There is no congestion window.
- It copies each chunk into its own replay store (`ip.go:6155`, `retainReturnChunk`) and then advances `receiveSeq`.
- When the window is exhausted, it parks on `receiveAckCond` (`ip.go:6152`) until an inner ACK opens the window again.

### 2.2 The return path treats admission as custody [S]

- Socket-owned returns wait synchronously for Transfer admission and retry it (`ip.go:8799-8821`, `9290-9335`).
- Batches go to Transfer as logical groups of at most 16 frames or 24 KiB (`ip.go:9281-9282`, `9468-9595`).
- The group's ack callback is `func(err error) {}` (`ip.go:9531`).
- The comment at `ip.go:9362-9365` states the assumption: "Admission moves the only recoverable copy into the bounded resend queue."

### 2.3 Transfer can dispose of the rest of an admitted group [S]

1. **Admission bound.** Each sequence admits at most `SequenceBufferSize` packs, which is 32 for providers (`transfer.go:7193`, `transfer_send_scheduler.go:72-83`).
2. **Chunking.** On H1, a group is cut into chunks of at most 16 frames and at most 3 KiB, or 3 × MTU once established (`transfer.go:1346-1352`, `3590-3596`).
   - For 1,100-byte packets that is 2–3 frames per chunk.
   - The fixture's providers show 2.35 frames per write; the reproduction shows 2.7.
3. **Selection.** The fit test prices only the pack's next chunk (`transfer.go:9703-9708`, `transfer_retained_budget.go:133-155`).
4. **Materialization.** Each chunk reserves its retained charge again when it is serialized (`transfer.go:11261-11283`).
   - If that reservation fails, the chunk's frames go back to the pool and the chunk reports `ErrSendPackNotAdmitted` (`11284-11306`).
5. **Disposal.** `processLogicalGroupChunk` then disposes of the rest of the group and returns `complete=true, success=true` (`transfer.go:8289-8291`).
   - This is deliberate: "A rejected chunk ends the raw remainder immediately" (`transfer_group_disposition.go:146-147`).
   - Only no-ack discards and deadline drops are counted (`transfer.go:9771-9773`, `11302`).

Between selection and materialization, other flows' groups are interleaved chunk by chunk ("Physical chunks may rotate between active flows", `transfer.go:9811-9815`). All of them draw on the same budget.

[H] That interleaving is the most likely reason the reproduction loses packets at 8 flows but not at 2–4. To falsify it, count reservation failures per scheduling key.

The window check itself is in protocol bytes and does not bind here (`transfer_queue.go:229-284`). The retained budget binds first.

### 2.4 Retained charge per pack [S][R]

`retainedMemoryByteCount` charges three pool-rounded roots of the serialized frame, plus an owner envelope rounded up to 1 KiB (`transfer_retained_budget.go:54-83`, `117-123`).

I measured it with the production function for 1,100-byte packets:

| Frames per pack | Charge | Payload | Charge ÷ payload | Payload that fits in 2 MiB |
|---:|---:|---:|---:|---:|
| 1 | 8,228 B | 1,048 B | 7.85 | 266 KB |
| 2 | 14,372 B | 2,096 B | 6.86 | 304 KB |
| 3 | 14,372–26,660 B | 3,144 B | 4.57–8.48 | 245–456 KB |
| 4 | 26,660 B | 4,192 B | 6.36 | 327 KB |
| 16 | 60,580 B | 16,768 B | 3.61 | 570 KB |

The 3-frame row is a range because the frame lands in the 4 KiB or the 8 KiB root class depending on whether contract bytes are added.

### 2.5 How a disposed chunk becomes a multi-second stall [S][R]

1. The app's TCP receives the segments after the hole and buffers them out of order. The gVisor fork keeps up to 75% of the receive buffer out of order (`../gvisor/pkg/tcpip/transport/tcp/rcv.go:504`). The app sends a duplicate ACK for each segment.
2. After three duplicate ACKs, or after the fixed 1 s timer, the NAT replays **one** segment (`ip_tcp_return_recovery.go:347-405`). The defaults are `ReturnResendTimeout` 1 s and `ReturnRecoveryMaxPacketCount` 1 (`ip.go:494-495`).
3. Each partial ACK below the recovery frontier readies exactly one more replay (`ip_tcp_return_recovery.go:177-186`).
4. The NAT's SYN-ACK offers MSS, timestamps and window scale, but not SACK-permitted (`ip.go:7169-7239`).
   - So the NAT cannot learn how large the hole is.
   - A hole of *k* segments therefore costs about *k* round trips.
5. Throughout, the NAT's window stays exhausted, because everything after the hole is unacknowledged. New data waits at `ip.go:6152`.
6. When the repair passes the hole, the buffered out-of-order data and a fresh window are released together. That is the burst sample.

What the stalled reproduction looked like:
- The provider's Transfer queue held 2 items, using 16 KiB of its 2 MiB budget.
- The NAT held about 4.1 MB that the app had not acknowledged.
- A goroutine dump showed every flow's reader parked at `ip.go:6152` and its recovery worker waiting on its timer.

PACKETFLOW-STALL-RESPONSE2 M2 already flagged the fixed 1 s timer and the one-replay-per-partial-ACK rule. This review adds the missing piece: a steady source of multi-segment holes, which turns that recovery into the throughput limit.

---

## 3. Reproduction (connect only)

**Harness.** A copy of the existing `startWindowTcpWorkload` (`transfer_window_tcp_performance_test.go:40-219`), with boundary counters added. It runs:

- the real `LocalUserNat`;
- in the main rows, the real `RemoteUserNatProvider`, with its security policy disabled the same way `ip_provider_tcp_retirement_custody_test.go:114` does it;
- two Transfer clients over H1-typed FIFO links, 20 ms each way at 2 Gbps, with encryption off and no contracts, as in the existing harness;
- a gVisor app TUN with 4,096 slots;
- a loopback origin.

All runs used `GOMAXPROCS=2`, lasted 8–10 s and were sampled every 500 ms.

| Provider policy | Device client | Flows | Mbps | Shape | Sent by provider / written by Transfer / delivered to device |
|---|---|---:|---:|---|---|
| none (default) | default | 2 | 344 | smooth | 332,506 / not logged / 330,784 |
| 2 MiB, retained | default | 2 | 82 | smooth, budget-bound | 79,471 / not logged / 78,990 |
| 2 MiB, retained | default | 4 | 82 | smooth | 79,216 / 78,772 / 78,588 |
| 2 MiB, retained | default | 8 | 21.8 | burst/crawl | 31,151 / 27,549 / 27,535 (11.6% never written) |
| 2 MiB, retained | default | 8 | 2.1 | crawl at ~90 KiB/sample | 9,579 / 5,547 / 5,536 (42% never written) |
| 2 MiB, retained | default | 8 | 12.5 and 20.0 | burst/crawl | same pattern |
| 2 MiB, plain accounting | default | 8 | 348 | smooth | 336,128 / 335,834 / 334,463 |
| 2 MiB, retained, recovery flight 32 | default | 8 | 79 | near-smooth | 77,443 / 75,966 / 75,965 (losses remain, repaired 32 per RTT) |
| 2 MiB, retained | SDK32-like | 8 | 80 (5 of 5 runs, one with a 5 MiB leaf) | smooth | ~96,200 / ~95,900 / ~95,880 |

**Reading the table.**
- "Written by Transfer" is `SendRecoveryStats().InitialFrameCount`.
- In every lossy row, written ≈ delivered: once a frame is written, it arrives.
- So all of the loss sits between "the provider says sent" and "Transfer writes".

**Earlier isolation runs.**
- My first runs used my own NAT-to-Transfer adapter, which passes each batch of up to 64 packets as one group. They lost packets at 2 flows as well (2.5–5.7 Mbps).
- There, the frames in groups whose callback reported `ErrSendPackNotAdmitted` (3,156) matched the missing packets (3,158).
- With production-sized groups that adapter stayed clean, which is why the table uses the real provider.

**Controls in the same harness.**
- SDK32-like client settings without the provider change: 347–350 Mbps at 2 and 8 flows, with the receive leaf at either 2 MiB or 5 MiB.
- The unmodified existing matrix, `TestWindowTcpDownloadPerformanceMatrix`, on arm `unpaced` (the shipping delivery-sized window with pacing off): 816 Mbps with 2 flows and 917 Mbps with 8, flat second by second.

**Limits of this reproduction.**
- It has no encryption, contracts, H1 sockets, server or emulator.
- My "SDK32-like" client copies the receive and send settings. It does not reproduce the SDK's composed budgets or the MultiClient.
- With that client, the 8-flow case never entered the loss regime in five runs. Whether the regime is entered therefore depends on receiver-side dynamics that my approximation misses.
- The evidence that the real SDK client does enter it is the fixture's own counters (Section 4), not this harness.

---

## 4. The fixture already recorded the losses

**What the counters mean.** The fixture captures two counters per provider:
- `return_completed_packets`: provider returns that completed. Failures (`Sent=false`) are counted separately and were 0 apart from 121, 1 and 2 packets in three baseline rows, so these completions are Transfer admissions.
- `InitialFrameCount`: frames that Transfer actually wrote.

Their difference is the number of return packets that were admitted but never written. It slightly *under*counts, because written frames also include a few control frames. Non-carrying providers show −2 to −4 in every run. [R]

| Arm | Run | Carrying provider: admitted − written (share of admitted) |
|---|---:|---|
| baseline | 1 | p1 398 (11%), p3 272 (8%) |
| baseline | 2 | p0 **1,434** (11%), p2 **1,285** (15%), p3 **1,293** (9%) |
| baseline | 3 | p1 **2,664** (22%; 2,543 after removing its 121 failures), p3 **976** (11%) |
| baseline | 4 | p2 **1,454** (10%), p3 **915** (10%), p6 **1,055** (8%) |
| baseline | 5 | p0 **1,189** (10%), p1 **2,638** (30%), p3 **524** (6%) |
| candidate | 1 | p0 **2,315** (14%), p3 441 (9%), p6 360 (9%) |
| candidate | 2 | p0 **1,596** (11%), p1 **583** (11%) |
| candidate | 3 | p0 **772** (19%), p2 368 (6%) |
| candidate | 4 | p0 **2,043** (11%), p3 **1,132** (9%) |
| candidate | 5 | p0 **1,026** (12%), p1 **805** (14%), p3 **681** (14%) |

Totals: baseline 16,097 of 130,965 (12.3%); candidate 12,122 of 105,318 (11.5%).

**Teardown alone cannot explain the bold rows.**
- Cancelling a run's requests can dispose of return packs that were still queued.
- But a provider has one data sequence to its window client; provider lanes are 0 in this plan.
- That sequence admits at most 32 packs (`transfer.go:7193`, `transfer_send_scheduler.go:72-83`).
- A return pack carries at most 16 frames (`ip.go:9281`).
- So at most 512 frames can be admitted but unwritten at teardown.
- The 20 bold rows exceed that, so disposal happened during the runs. The other five rows are ambiguous.

**Consistency check.** Carrying providers show no timeout resends beyond the 4–8 per run that providers 5 and 6 show in every run. That is expected, because a disposed chunk never becomes a resend item.

---

## 5. Answers to the brief's questions

### Q1. Which missing observation best distinguishes the plateau?

Packet accounting at the provider's admission/write boundary, sampled on the same 500 ms grid.

- **Best:** ack-required frames disposed after admission (`transfer.go:8290`, `11296`), per sequence.
- **Proxy available today:** provider returns admitted minus `InitialFrameCount`, per sample.
- **Next most useful:** NAT replay counts per flow, split by trigger: three duplicate ACKs, the timer, or a partial ACK.

Outer TCP recovery, Transfer flow control and simulator capacity are not needed to produce the plateau (Section 3). Edge counters are still worth adding, but they should not hold up the next run.

### Q2. Can a production gate hold throughput this low without a refusal or retransmit in the captured counters? Where exactly?

Yes. The ownership boundary lies between Pack admission and the per-chunk retained reservation at serialization:

1. `RemoteUserNatProvider` returns from `sendGroupWithTimeoutDetailed` believing that Transfer holds custody (`ip.go:9362-9365`, `9528-9541`).
2. `SendSequence` later rejects a chunk (`transfer.go:11265-11281`) and disposes of the rest of the group (`8289-8291`).

The NAT still holds a copy, so nothing is lost permanently. But recovery falls to the inner-TCP path at one segment per partial ACK. None of the captured counters observes either step.

### Q3. Is the workload representative enough, and what control would falsify a fixture-induced ceiling?

**Workload.**
- The adaptive ramp mixes stream count with per-stream behavior (Section 1).
- Add fixed 2-stream and fixed 8-stream diagnostic runs.
- Record per-flow byte samples, not only the aggregate, which is quantized to 32 KiB.

**Controls.**
- The existing connect harness already rules out a CPU ceiling for the NAT, Transfer and TUN code at `GOMAXPROCS=2`.
- The fixture's provider policy *is* a fixture-induced ceiling: about 82 Mbps per provider at 40 ms (Section 3). It is also what triggers the losses.
- A same-path control is the V5 fixture with providers on plain accounting, or with a budget sized from the measured charge (Section 2.4).

### Q4. What deterministic test reproduces the mechanism through the public path?

Turn the Appendix A harness into an assertion:

- **Setup:** real `RemoteUserNatProvider`, `LocalUserNat`, two Clients, a gVisor TUN and a loopback origin, with the fixture's declared provider policy (2 MiB, retained accounting) and 8 flows.
- **Assert:** every return packet the provider reports as sent is written by Transfer, or a new disposal counter stays at 0.
- **Assert:** a fixed byte count completes within a time bound.
- **Constraints:** real budgets only, with no fake send failure, no forced adoption, and no manual ACK or retry injection.
- **Current result:** it fails at this revision, with 11–42% unwritten and 2–22 Mbps.
- **Deterministic variant:** to avoid depending on load, drive a single flow whose window exceeds the budget's payload capacity, add a second flow that interleaves chunks, and assert zero disposals.

### Q5. Which adjacent paths share the reasoning?

See Section 8.

---

## 6. Recommended next steps

Run these in the real V5 fixture first. Each is cheap and each one discriminates between explanations. The predictions come from Section 3.

| Step | Change | Prediction |
|---|---|---|
| E1 – recovery flight | Set `providerReturnRecoveryMaxPacketCountForTest = 32`. This is an existing plan knob (`sdk_browsing_setup_fixture_test.go:197-203`). | Several times faster, and the long crawls disappear. The admitted-but-unwritten gap persists. |
| E2 – accounting | Run providers with `ResendQueueRetainedByteAccounting = false` and the same 2 MiB budget. | The gap falls to the teardown residue, and throughput rises far above the current runs. |
| E3 – direct counter | Count ack-required frames returned at `transfer.go:11296`, and frames disposed of at `8290` and `9760`, per sequence. Publish them in `SendRecoveryStats`. | Nonzero for every carrying provider, and growing during runs. |
| E4 – fixed streams | Run fixed 2-stream and 8-stream workloads with per-flow byte samples. | Separates per-stream behavior from the adaptive ramp. |
| E5 – physical sessions | For the 25 s and 9 s plateau sessions, identify each provider's software and collect its provider-side E3 counters. | SDK or app providers have retained accounting forced on, so they are the expected suspects. |

---

## 7. Fix directions

These are ranked. Each one keeps the CODESTYLE rules.

### F1. Transfer: never dispose of an admitted group's remainder because of capacity

This is the root fix. It restores the rule that a failed admission must keep the item eligible for retry. Either option works:

- **(a)** Price and reserve the whole group's retained charge when the Pack is selected, so that materialization cannot fail partway through.
- **(b)** When a chunk's reservation fails, keep the remainder at the head (`PushFront`) and wait for budget. Dispose of it only on cancellation or an explicit deadline.

The comment at `transfer.go:9753-9755` worries that a charged group could hold the very memory its own serializer is waiting for. For the provider's ack-required groups that cannot happen: the already-materialized chunks are released by peer ACKs, which do not depend on the rest of the group. One deadlock case remains, a budget too small for even one chunk. Guaranteeing that one chunk always fits an otherwise empty budget removes it.

### F2. Provider return path: treat a group callback error as a local loss

- `sendReturnBatchWithLimits` should map the failed range back to its flow.
- It should then wake that flow's recovery worker to replay those bytes immediately. The bytes are already in `returnChunks`.
- The wake-up is a non-blocking signal, so no shared callback waits on it.
- This is defense in depth for F1, and for any other refusal after admission.

### F3. NAT recovery

- Replay a known-lost range as one flight.
- Raise `ReturnRecoveryMaxPacketCount` above 1. With the same losses, 8 flows went from 7.5 to 79 Mbps.
- Offer SACK-permitted in the SYN-ACK. The gVisor and kernel receivers then send SACK blocks, which tell the NAT how large each hole is.
- Make the timer adapt to RTT (PACKETFLOW-STALL-RESPONSE2 F1).

Without F3, any other source of a multi-segment hole will produce the same crawl.

### F4. Retained charge and budget sizing

- Three full roots per small item, plus an owner envelope rounded up to 1 KiB, make a 2 MiB budget worth only about 250–550 KB of payload.
- That is a ceiling of about 80 Mbps per provider at 40 ms, below the target even after F1.
- Two ways to shrink the charge without raising the memory target:
  - Review whether every queued item needs the decode and rewrite scratch roots (`transfer_retained_budget.go:64-70`), or only the head item does.
  - Carry fuller packs. A 16-frame pack costs 3.6× its payload; a 1–2 frame pack costs 6–8×.
- The fixture's 2 MiB should also be stated in payload terms.

### F5. Observability

- Add the E3 counters.
- Add NAT replay and hole counters per flow.
- Capture the app TUN's `Stats().DroppedPackets` and its per-endpoint `ReceiveErrors`.
- In the fixture, stop ignoring the return value of `appTun.WriteBatch` (`server/connect/perfvar/sdk_browsing_native_test.go:67-69`).

---

## 8. Adjacent paths with the same reasoning

- **MultiClient upstream groups.** `transfer.go:9753-9762` disposes of the unmaterialized remainder of "unfunded" parsed packet groups.
  - These groups are the client's upstream from the phone: uploads and inner ACKs.
  - They run under the SDK's parented budgets, where retained accounting is forced (`transfer.go:7137-7144`).
  - The client classifies `ErrSendPackNotAdmitted` as a packet-local failure (`ip_remote_multi_client.go:18963-18983`). Check that it offers the packet again rather than dropping it.
- **Pack deadline expiry for logical groups** (`transfer.go:9770-9781`) has the same post-admission shape. It is counted, but the provider ignores it in the same way.
- **Other group senders.** Any caller of `sendGroupWithTimeoutDetailed` or `SendMultiWithTimeout` that passes a callback which ignores errors while treating `true` as custody.
- **SDK provider mode on phones and desktop.** A small provider share plus forced retained accounting is exactly the configuration that triggers this. It probably affects real users who download through app providers.
- **The candidate's receive-leaf fix** is the mirror image on the receive side: there too, the retained cost far exceeded the logical credit. The send side needs the same audit.

---

## 9. Corrections to the brief's framing

- **"Zero captured gVisor retransmits/timeouts"** says nothing about download loss, because the app TUN is the receiver. Download loss is repaired by the provider NAT, whose replays are not counted.
- **"Providers carrying original requests had zero Transfer timeout resends"** is expected. Disposed chunks never become Transfer items.
- **"Do not assume sequence depth 32 is a flight cap"**: agreed. Here the flight cap is the retained budget. The 32-pack admission bound is what limits the teardown residue.
- **"`ReturnRecoveryMaxPacketCount=1` bounds replay, not ordinary download flight"**: true in general, but during a stall the replay rate *is* the download rate.
- **"2 MiB retained resend budget"** is roughly 250–550 KB of payload (Section 2.4).
- **Flat intervals** are quantized to 32 KiB (Section 1).
- **The outer-TCP and edge telemetry gap** is real, but the collapse reproduces with no outer TCP at all.

---

## Appendix A – reproduction

### A.1 Setup

The three test files below are added to the `connect` package through `go test -overlay`. They use unexported identifiers, so they must compile inside the package. The repository itself is not modified.

On this host they live in:

`/private/tmp/claude-501/-Users-builder-urnetwork-connect/31deb01d-2417-4a60-8aa7-6fab1905e12e/scratchpad/overlay/`

The run logs and the stall goroutine dump are in the sibling `runs/` directory.

`overlay.json` uses this format:

```json
{"Replace": {
 "/Users/builder/urnetwork/connect/zz_review_retained_test.go": "<dir>/zz_review_retained_test.go",
 "/Users/builder/urnetwork/connect/zz_review_bulk_test.go": "<dir>/zz_review_bulk_test.go",
 "/Users/builder/urnetwork/connect/zz_review_workload_test.go": "<dir>/zz_review_workload_test.go"
}}
```

### A.2 Commands

Run these from `connect/`:

```sh
OV=<dir>/overlay.json

# Capacity control: the existing, unmodified harness.
GOMAXPROCS=2 CONNECT_WINDOW_TCP_MEASURE=1 CONNECT_WINDOW_PATH_RTT_US=40000 \
  CONNECT_WINDOW_PATH_FLOWS=2,8 CONNECT_WINDOW_PATH_ACK_MS=10 \
  CONNECT_WINDOW_PATH_REPETITIONS=1 CONNECT_WINDOW_PATH_SECONDS=10 \
  CONNECT_WINDOW_PATH_ARMS=unpaced \
  go test -run '^TestWindowTcpDownloadPerformanceMatrix$' -count=1 -v .

# Retained charge per pack (Section 2.4).
go test -overlay=$OV -run '^TestReviewRetainedChargePerPack$' -count=1 -v .

# Main reproduction: real RemoteUserNatProvider plus the fixture's provider policy.
GOMAXPROCS=2 REVIEW_BULK=1 REVIEW_REMOTE_PROVIDER=1 REVIEW_SECONDS=8 \
  REVIEW_FLOWS=8 REVIEW_STEPS=provider \
  go test -overlay=$OV -run '^TestReviewBulkBisect$' -count=1 -v .
```

### A.3 Variants

These environment variables change the main reproduction:

| Variable | Effect |
|---|---|
| `REVIEW_FLOWS=2` or `4` | Changes the number of flows. |
| `REVIEW_NO_RETAINED=1` | Plain accounting at the same budget. |
| `REVIEW_RECOVERY_PACKETS=32` | Raises the NAT recovery flight. |
| `REVIEW_STEPS=provider,recv,send` | Adds the SDK32-like client settings. |
| `REVIEW_LEAF_KIB=5120` | Uses the candidate-sized receive leaf. |
| `REVIEW_PROVIDER_BUDGET_KIB=...` | Changes the provider budget. |
| unset `REVIEW_REMOTE_PROVIDER` | Uses my adapter instead of the real provider. |
| `REVIEW_PRODUCTION_GROUPS=1` | With the adapter, groups packets the way production does. |
| `REVIEW_DUMP=<file>` | Writes a goroutine dump during the stall. |

### A.4 Sources

`zz_review_retained_test.go`

```go
package connect

import (
	"testing"
	"unsafe"
)

// Review-only probe: the retained charge of one provider return Pack that
// carries k 1100-byte inner IP packets, under ResendQueueRetainedByteAccounting.
func TestReviewRetainedChargePerPack(t *testing.T) {
	t.Logf("sizeof(sendItem)=%d", unsafe.Sizeof(sendItem{}))
	for _, contractBytes := range []ByteCount{0, 200} {
		for _, k := range []int{1, 2, 3, 4, 8, 16} {
			// raw v2 frame: packet bytes plus a few bytes of protobuf framing per frame
			message := ByteCount(k*(1100+8) + 64)
			frame := addReceiveQueueByteCount(message, 512+32*ByteCount(k)+contractBytes)
			charge := (&sendItem{}).retainedMemoryByteCount(frame, k, false)
			payload := ByteCount(k * 1048)
			t.Logf("contract=%d k=%2d message=%6d frameEstimate=%6d charge=%7d payload=%6d charge/payload=%.2f packs-in-2MiB=%d payload-in-2MiB=%d",
				contractBytes, k, message, frame, charge, payload, float64(charge)/float64(payload),
				mib(2)/charge, mib(2)/charge*payload)
		}
	}
}
```

`zz_review_workload_test.go`

```go
package connect

import (
	"context"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Review-only copy of startWindowTcpWorkloadWithNatSettings with boundary
// counters: packets the provider NAT emitted, packets the device injected into
// the app TUN, Transfer group admission failures, and the app TUN itself.
type reviewWorkloadCounters struct {
	natEmittedPackets   atomic.Int64
	natEmittedBytes     atomic.Int64
	admissionFailures   atomic.Int64
	deviceInjected      atomic.Int64
	deviceInjectedBytes atomic.Int64
	tun                 atomic.Pointer[Tun]
	callbackErrors      atomic.Int64
	callbackErrorFrames atomic.Int64
	errorKinds          sync.Map
}

func reviewStartTcpWorkload(t *testing.T, ctx context.Context, provider, device *Client, counts []atomic.Int64, natRefused *atomic.Int64, settings *LocalUserNatSettings, review *reviewWorkloadCounters) func() {
	t.Helper()
	tunSettings := DefaultTunSettingsWithBufferSize(4096)
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tun, err := CreateTun(ctx, tunSettings)
	if err != nil {
		listener.Close()
		t.Fatal(err)
	}
	review.tun.Store(tun)
	settings.Log = NewNoopLogger()
	settings.TcpBufferSettings.EnableSyntheticSpeed = false
	settings.TcpBufferSettings.DialContextSettings = &DialContextSettings{DialContext: func(dialCtx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(dialCtx, "tcp4", listener.Addr().String())
	}}
	nat := NewLocalUserNat(ctx, "review-tcp", settings)
	var workers sync.WaitGroup
	var sockets sync.Map
	writePacketsWithOptions := func(client *Client, destination Id, messageType protocol.MessageType, packets [][]byte, timeout time.Duration, options ...any) {
		frames := make([]*protocol.Frame, 0, len(packets))
		for _, packet := range packets {
			frames = append(frames, &protocol.Frame{MessageType: messageType, MessageBytes: MessagePoolShareReadOnly(packet), Raw: true})
		}
		frameCount := int64(len(frames))
		callback := func(err error) {
			if err != nil {
				review.callbackErrors.Add(1)
				review.callbackErrorFrames.Add(frameCount)
				key := err.Error()
				value, _ := review.errorKinds.LoadOrStore(key, &atomic.Int64{})
				value.(*atomic.Int64).Add(1)
			}
		}
		if messageType != protocol.MessageType_IpIpPacketFromProvider {
			callback = nil
		}
		options = append(options, Ctx(ctx))
		if success, _ := client.sendGroupWithTimeoutDetailed(frames, destination, callback, timeout, options...); !success {
			for _, frame := range frames {
				MessagePoolReturn(frame.MessageBytes)
			}
			if ctx.Err() == nil {
				review.admissionFailures.Add(int64(len(packets)))
			}
		}
	}
	writePackets := func(client *Client, destination Id, messageType protocol.MessageType, packets [][]byte) {
		writePacketsWithOptions(client, destination, messageType, packets, -1)
	}
	emit := func(source TransferPath, ipPath *IpPath, packets [][]byte) {
		for _, packet := range packets {
			review.natEmittedPackets.Add(1)
			review.natEmittedBytes.Add(int64(len(packet)))
		}
		if os.Getenv("REVIEW_PRODUCTION_GROUPS") == "" {
			writePackets(provider, source.SourceId, protocol.MessageType_IpIpPacketFromProvider, packets)
			return
		}
		// Mirror RemoteUserNatProvider.sendReturnBatchWithLimits: groups of at
		// most providerReturnBatchMaxFrames frames and providerReturnBatchMaxBytes.
		for start := 0; start < len(packets); {
			end, bytes := start, 0
			for end < len(packets) && end-start < providerReturnBatchMaxFrames &&
				(end == start || bytes+len(packets[end]) <= providerReturnBatchMaxBytes) {
				bytes += len(packets[end])
				end++
			}
			// Mirror the production options: the 30 s provider write timeout, the
			// socket-owned recovery option and the per-flow scheduling key.
			writePacketsWithOptions(provider, source.SourceId, protocol.MessageType_IpIpPacketFromProvider, packets[start:end],
				30*time.Second,
				sendPackRecoveryOption{upstreamRecoverable: true, retainAfterAckTimeout: true},
				sendSchedulingKeyOption{key: ipSendSchedulingKey(ipPath)})
			start = end
		}
	}
	remoteProvider := os.Getenv("REVIEW_REMOTE_PROVIDER") != ""
	var remoteNat *RemoteUserNatProvider
	if remoteProvider {
		providerSettings := DefaultRemoteUserNatProviderSettings()
		providerSettings.SecurityPolicyGenerator = DisableSecurityPolicyWithStats
		providerSettings.ReturnSendObserver = func(observation RemoteUserNatProviderReturnSendObservation) {
			if observation.Phase == RemoteUserNatProviderReturnSendPhaseCompleted {
				review.natEmittedPackets.Add(int64(observation.PacketCount))
				review.natEmittedBytes.Add(int64(observation.PacketByteCount))
				if !observation.Sent {
					review.admissionFailures.Add(int64(observation.PacketCount))
				}
			}
		}
		remoteNat = NewRemoteUserNatProvider(provider, nat, providerSettings)
	} else {
		nat.AddReceivePacketCallback(func(source TransferPath, _ protocol.ProvideMode, ipPath *IpPath, packet []byte) {
			emit(source, ipPath, [][]byte{packet})
		})
		nat.AddReceivePacketsCallback(func(source TransferPath, _ protocol.ProvideMode, ipPath *IpPath, packets [][]byte) {
			emit(source, ipPath, packets)
		})
	}
	provider.AddReceiveCallback(func(source TransferPath, frames []*protocol.Frame, _ Peer) {
		if remoteProvider {
			return
		}
		packets := make([][]byte, 0, len(frames))
		for _, frame := range frames {
			if frame.MessageType != protocol.MessageType_IpIpPacketToProvider {
				continue
			}
			packets = append(packets, MessagePoolShareReadOnly(frame.MessageBytes))
		}
		if len(packets) != 0 && !nat.SendPackets(source, protocol.ProvideMode_Network, packets, 0) {
			for _, packet := range packets {
				MessagePoolReturn(packet)
			}
			if ctx.Err() == nil {
				natRefused.Add(int64(len(packets)))
			}
		}
	})
	device.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, _ Peer) {
		packets := make([][]byte, 0, len(frames))
		for _, frame := range frames {
			if frame.MessageType == protocol.MessageType_IpIpPacketFromProvider {
				packets = append(packets, frame.MessageBytes)
				review.deviceInjected.Add(1)
				review.deviceInjectedBytes.Add(int64(len(frame.MessageBytes)))
			}
		}
		if _, err := tun.WriteBatch(packets); err != nil && ctx.Err() == nil {
			t.Errorf("TCP fixture TUN injection: %v", err)
		}
	})
	workers.Go(func() {
		packets := make([][]byte, 64)
		for ctx.Err() == nil {
			n, err := tun.ReadBatch(packets)
			if n != 0 {
				writePackets(device, provider.ClientId(), protocol.MessageType_IpIpPacketToProvider, packets[:n])
				for i := range n {
					MessagePoolReturn(packets[i])
					packets[i] = nil
				}
			}
			if err != nil {
				return
			}
		}
	})
	workers.Go(func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			if ctx.Err() != nil {
				conn.Close()
				return
			}
			sockets.Store(conn, true)
			workers.Go(func() {
				defer conn.Close()
				defer sockets.Delete(conn)
				var flow [1]byte
				if _, err := io.ReadFull(conn, flow[:]); err != nil {
					return
				}
				buffer := make([]byte, 64*1024)
				for ctx.Err() == nil {
					if _, err := conn.Write(buffer); err != nil {
						return
					}
				}
			})
		}
	})
	for flow := range counts {
		workers.Go(func() {
			conn, err := tun.DialContext(ctx, "tcp", "198.51.100.2:443")
			if err != nil {
				if ctx.Err() == nil {
					t.Errorf("TCP fixture dial: %v", err)
				}
				return
			}
			sockets.Store(conn, true)
			defer conn.Close()
			defer sockets.Delete(conn)
			if _, err := conn.Write([]byte{byte(flow)}); err != nil {
				return
			}
			buffer := make([]byte, 64*1024)
			for ctx.Err() == nil {
				n, err := conn.Read(buffer)
				counts[flow].Add(int64(n))
				if err != nil {
					return
				}
			}
		})
	}
	return func() {
		if remoteNat != nil {
			remoteNat.Close()
		}
		listener.Close()
		sockets.Range(func(key, _ any) bool { key.(net.Conn).Close(); return true })
		nat.Close()
		tun.Close()
		workers.Wait()
		joinCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		nat.CloseAndWait(joinCtx)
	}
}
```

`zz_review_bulk_test.go`

```go
package connect

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Review-only bisection of the synthetic Fast.com bulk path. It reuses the
// existing window-path TCP workload (real provider LocalUserNat, two Transfer
// clients over FIFO H1-typed links, gVisor app TUN, loopback origin) and adds
// the fixture's provider and SDK32-like client settings one step at a time.
//
// REVIEW_STEPS (comma separated, cumulative by name):
//   provider  - provider resend budget 2 MiB with retained accounting
//   recv      - SDK32-like client receive: retained accounting, small leaf,
//               Pack budget 2,181,038, mobile H1 queue counts/bytes, waits
//   send      - SDK32-like client upstream: lanes, retained accounting, small
//               queues and resend window
func TestReviewBulkBisect(t *testing.T) {
	if os.Getenv("REVIEW_BULK") == "" {
		t.Skip("review-only")
	}
	steps := map[string]bool{}
	for _, step := range strings.Split(os.Getenv("REVIEW_STEPS"), ",") {
		if step = strings.TrimSpace(step); step != "" {
			steps[step] = true
		}
	}
	flows := envInt(t, "REVIEW_FLOWS", 2)
	rtt := time.Duration(envInt(t, "REVIEW_RTT_MS", 40)) * time.Millisecond
	seconds := envInt(t, "REVIEW_SECONDS", 10)
	leafKiB := envInt(t, "REVIEW_LEAF_KIB", 2048)
	providerBudgetKiB := envInt(t, "REVIEW_PROVIDER_BUDGET_KIB", 2048)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	provider := DefaultClientSettings()
	provider.Log = NewNoopLogger()
	provider.EncryptionSettings.Mode = EncryptionModeOff
	provider.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
	if steps["provider"] {
		provider.SendBufferSettings.ResendQueueBudget = NewTransferMemoryBudget(ByteCount(providerBudgetKiB) * kib(1))
		provider.SendBufferSettings.ResendQueueRetainedByteAccounting = os.Getenv("REVIEW_NO_RETAINED") == ""
	}

	device := DefaultClientSettings()
	device.Log = NewNoopLogger()
	device.EncryptionSettings.Mode = EncryptionModeOff
	device.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
	if steps["recv"] {
		receive := device.ReceiveBufferSettings
		receive.ReceiveQueueBudget = NewTransferMemoryBudget(ByteCount(leafKiB) * kib(1))
		receive.PackQueueBudget = NewTransferMemoryBudget(2181038)
		receive.ReceiveQueueRetainedByteAccounting = true
		receive.ReceiveQueueMinByteCount = 0
		receive.ReceiveQueueMaxByteCount = 2181038
		receive.AdvertiseReceiveWindow = true
		receive.SequenceBufferSize = 21
		receive.SequenceBufferByteCount = 170 * 1024
		receive.H1SequenceBufferSize = 85
		receive.H1SequenceBufferByteCount = 170 * 1024
		receive.H1SequenceBufferAdaptiveMaxSize = 0
		receive.H1SequenceBufferAdaptiveStepSize = 0
		receive.H1SequenceBufferAdaptiveSaturationThreshold = 0
		receive.H1SequenceBufferAdaptiveSaturationWindow = 0
		receive.H1SequenceBufferAdaptiveMaxByteCount = 0
		receive.H1SequenceBufferAdaptiveStepByteCount = 0
		receive.H1PackHandoffTimeout = -1
		receive.ReliablePackHandoffTimeout = -1
		receive.H1AckHandoffTimeout = time.Millisecond
	}
	if steps["send"] {
		send := device.SendBufferSettings
		device.SendBufferSize = 21
		send.SequenceBufferSize = 21
		send.AckBufferSize = 85
		send.ResendQueueRetainedByteAccounting = true
		send.ResendQueueMinByteCount = 85 * 1024
		send.ResendQueueMaxByteCount = 682 * 1024
		send.LogicalDataLaneCount = 10
		send.ResendQueueBudget = NewTransferMemoryBudget(4 * mib(1))
	}

	sender := NewClient(ctx, NewId(), NewNoContractClientOob(), provider)
	receiver := NewClient(ctx, NewId(), NewNoContractClientOob(), device)
	sender.ContractManager().AddNoContractPeer(receiver.ClientId())
	receiver.ContractManager().AddNoContractPeer(sender.ClientId())
	sendOut, sendIn, receiveOut, receiveIn := make(Route, 128), make(Route, 128), make(Route, 128), make(Route, 128)
	sender.RouteManager().UpdateTransport(NewSendGatewayTransportWithType(TransportTypeH1), []Route{sendOut})
	sender.RouteManager().UpdateTransportWithProperties(NewReceiveGatewayTransportWithType(TransportTypeH1), []Route{sendIn}, TransferCarrierProperties{ReceiveReliability: CarrierReliabilityReliable})
	receiver.RouteManager().UpdateTransport(NewSendGatewayTransportWithType(TransportTypeH1), []Route{receiveOut})
	receiver.RouteManager().UpdateTransportWithProperties(NewReceiveGatewayTransportWithType(TransportTypeH1), []Route{receiveIn}, TransferCarrierProperties{ReceiveReliability: CarrierReliabilityReliable})

	var workers sync.WaitGroup
	dataLink := windowPathLink{rate: 250000000, delay: rtt / 2, queueCount: 4096, queueBytes: mib(8)}
	ackLink := windowPathLink{rate: 250000000, delay: rtt / 2, queueCount: 4096, queueBytes: mib(8)}
	workers.Go(func() { dataLink.run(ctx, sendOut, receiveIn) })
	workers.Go(func() { ackLink.run(ctx, receiveOut, sendIn) })

	counts := make([]atomic.Int64, flows)
	var natRefused atomic.Int64
	natSettings := DefaultLocalUserNatSettings()
	if packets := envInt(t, "REVIEW_RECOVERY_PACKETS", 0); 0 < packets {
		natSettings.TcpBufferSettings.ReturnRecoveryMaxPacketCount = packets
	}
	review := &reviewWorkloadCounters{}
	cleanup := reviewStartTcpWorkload(t, ctx, sender, receiver, counts, &natRefused, natSettings, review)

	start := time.Now()
	var series []string
	previous := int64(0)
	total := func() int64 {
		sum := int64(0)
		for i := range counts {
			sum += counts[i].Load()
		}
		return sum
	}
	var budgetSeries []string
	dumped := false
	for i := 0; i < seconds*2; i++ {
		time.Sleep(time.Until(start.Add(time.Duration(i+1) * 500 * time.Millisecond)))
		now := total()
		series = append(series, fmt.Sprintf("%d", (now-previous)/1024))
		previous = now
		count, queued, _ := sender.ResendQueueSize(receiver.ClientId(), MultiHopId{}, false, false)
		used := ByteCount(-1)
		if budget := provider.SendBufferSettings.ResendQueueBudget; budget != nil {
			used = budget.UsedByteCount()
		}
		natUsed := ByteCount(-1)
		if budget := natSettings.TcpBufferSettings.ReturnQueueBudget; budget != nil {
			natUsed = budget.UsedByteCount()
		}
		tunStats := review.tun.Load().Stats()
		budgetSeries = append(budgetSeries, fmt.Sprintf("[q=%d/%dB used=%d nat=%d emitted=%d injected=%d tunDropped=%d tcpValid=%d tcpInvalid=%d]", count, queued, used, natUsed,
			review.natEmittedPackets.Load(), review.deviceInjected.Load(), tunStats.DroppedPackets.Value(), tunStats.TCP.ValidSegmentsReceived.Value(), tunStats.TCP.InvalidSegmentsReceived.Value()))
		if i == 8 && !dumped && os.Getenv("REVIEW_DUMP") != "" {
			dumped = true
			buffer := make([]byte, 8<<20)
			n := runtime.Stack(buffer, true)
			os.WriteFile(os.Getenv("REVIEW_DUMP"), buffer[:n], 0o600)
		}
	}
	elapsed := time.Since(start)
	bytes := total()
	recovery := sender.SendRecoveryStats()
	window := sender.DestinationSendStats(receiver.ClientId()).SendWindow
	receive := receiver.ReceiveStats()
	t.Logf("steps=%v flows=%d rtt=%s mbps=%.2f natRefused=%d", steps, flows, rtt, float64(bytes)*8/elapsed.Seconds()/1e6, natRefused.Load())
	t.Logf("KiB per 0.5s: %s", strings.Join(series, " "))
	t.Logf("provider resend queue/budget per 0.5s: %s", strings.Join(budgetSeries, " "))
	t.Logf("provider frames: initialFrames=%d initialMessageBytes=%d sendPackDeadlineDrops=%d noAckDiscards=%d",
		recovery.InitialFrameCount, recovery.InitialMessageByteCount, sender.ReceiveStats().SendPackDeadlineDropCount, sender.ReceiveStats().SendNoAckRefusedCount)
	t.Logf("provider: initialWrites=%d timeoutResends=%d selectiveGap=%d evictionResend=%d admissionWait=%s window=%d ceiling=%d floor=%d reason=%q",
		recovery.InitialWriteCount, recovery.TimeoutResendWriteCount, recovery.SelectiveGapWriteCount, recovery.SendEvictionResendCount,
		recovery.ReliableAdmissionWaitDuration, window.Window, window.Ceiling, window.Floor, window.Reason)
	tunStats := review.tun.Load().Stats()
	t.Logf("boundary: natEmitted=%d/%dB admissionFailures=%d deviceInjected=%d/%dB delivered=%dB tunDropped=%d ipDelivered=%d tcpValid=%d tcpInvalid=%d tcpSent=%d tcpRetransmits=%d",
		review.natEmittedPackets.Load(), review.natEmittedBytes.Load(), review.admissionFailures.Load(), review.deviceInjected.Load(), review.deviceInjectedBytes.Load(), bytes,
		tunStats.DroppedPackets.Value(), tunStats.IP.PacketsDelivered.Value(), tunStats.TCP.ValidSegmentsReceived.Value(), tunStats.TCP.InvalidSegmentsReceived.Value(), tunStats.TCP.SegmentsSent.Value(), tunStats.TCP.Retransmits.Value())
	kinds := []string{}
	review.errorKinds.Range(func(key, value any) bool {
		kinds = append(kinds, fmt.Sprintf("%q=%d", key, value.(*atomic.Int64).Load()))
		return true
	})
	t.Logf("provider group ack-callback errors: groups=%d framesInThoseGroups=%d kinds=%v", review.callbackErrors.Load(), review.callbackErrorFrames.Load(), kinds)
	t.Logf("device receive: drops=%d evictions=%d packHandoffDrops=%d packHandoffWaits=%d packSaturations=%d",
		receive.ReceiveQueueDropCount, receive.ReceiveQueueEvictionCount, receive.PackHandoffDropCount, receive.PackHandoffWaitCount, receive.PackHandoffSaturationCount)
	cancel()
	cleanup()
	workers.Wait()
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer closeCancel()
	sender.CloseAndWait(closeCtx)
	receiver.CloseAndWait(closeCtx)
	for _, route := range []Route{sendOut, sendIn, receiveOut, receiveIn} {
		for len(route) != 0 {
			MessagePoolReturn(<-route)
		}
	}
}
```
