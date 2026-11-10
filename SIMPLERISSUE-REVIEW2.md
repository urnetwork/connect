# Independent review of SIMPLERISSUE.md (reviewer 2)

Date: 2026-11-09. Scope: the problem statement in `SIMPLERISSUE.md`, the source at `main` (`a321ff90`), the four commits of 2026-11-07/08 that touch receive admission, and the SDK memory policy. I did not read REVIEW1. Everything marked **verified** was read in code; everything marked **speculation** is inference.

## 1. Summary of the problem as I understand it

- A hosted H1 Fast5 on the receive-recovery source (`00d4f62b`/`84415854` era) delivered 320–428 Mbps on all ten windows (MEMSTEADY.md, "2026-11-08: paid receive recovery and matched Fast5 diagnostics").
- The "current production source" delivers one acceptable window and then decays monotonically (299 → 129 → 48 → 16 → 3 Mbps), with 5.8–9.2 s small-request stalls in the failed windows, and provider timeout-resend counters growing 61 → 90 → 132 → 697 → 987 across windows.
- The writeup attributes this to (a) restrictive RX/TX/Pack/Head leaf ceilings under the shared root and (b) inner TCP pure ACKs being head-of-line blocked behind missing DATA in the ordered Transfer sequence, and proposes a shared aggregate DATA budget plus a new "reliable immediate delivery" Pack option.

My reading: (b) is a real mechanism but is a **consequence**, not the origin, of the holes. The writeup never asks *where the holes come from on a reliable H1 carrier*. The source answers that question directly, and the answer is a change the writeup does not list among its "boundaries that changed".

## 2. The boundary the writeup omits: R14 made the shared receive pump drop on a reliable carrier

**Verified.** Commit `d90ac7f4` ("Keep shared receive admission from blocking independent ACK progress", 2026-11-08 06:20) changed the one shared `Client.run` pump for every carrier:

- `transfer.go:5154` — `self.receiveBuffer.Pack(receivePack, 0)`; before this commit the pump passed `packHandoffTimeout(transportType, carrierReliability)`, which for a reliable carrier (H1) was `ReliablePackHandoffTimeout = -1` (wait for capacity or cancellation; `transfer.go:1113`). Mobile also set `-1` (`sdk/mobile_memory_policy.go:218,426-427`). Those settings are now dead for the shared pump; the comment at `transfer.go:15356-15366` admits it.
- `transfer.go:4980-4984` — `ackMessageDetailed(..., 0)`; a busy `ackMutex` now returns `receiveAckHandoffQueueFull` and the ACK is **discarded** (`transfer.go:8386-8393`, `4441-4446`).
- `transfer.go:16630-16640` — `ReceiveSequence.Pack` with timeout 0 `TryLock`s `packMutex` and refuses if busy; `transfer.go:16806-16816` — with timeout 0 a failed count/byte reservation or a full `packs` channel is "ordinary nonblocking loss": the Pack is returned to the pool and counted in `receivePackHandoffDropCount` (`transfer.go:5157-5165`, `4409`). Nothing is sent to the peer; the sender finds out via SACK gap evidence or its resend timer.

MEMSTEADY.md:6827 confirms the timeline: "The R14 checkpoint includes the logical shared-Client admission correction ... Pack and ACK admission from the shared pump use zero time for every carrier ... This exact R3 composition is source-reviewed but **unexecuted**." The 320–428 Mbps reference predates R14; the decaying runs are on "current production source" = R14. The writeup's comparison section lists accounting mode, lane count, host construction and provider population as the changed boundaries, but not this one. That is the single most important gap in the document.

### Why this manufactures exactly the observed symptoms

**Verified sizing.** The per-sequence handoff channel is tiny on mobile and is divided by the lane count:

- `sdk/mobile_memory_policy.go:395-425` — `SequenceBufferSize ≤ 16`, `H1SequenceBufferSize ≤ 64` (scaled ×target/24 MiB above 24 MiB), `H1SequenceBufferByteCount ≤ 128 KiB`, and **all H1 adaptive deepening fields forced to 0** (`:406-411`), so `tryDeepenH1PackQueue` (`transfer.go:16534`) can never grow the queue on a phone.
- `transfer_logical_lane.go:42-48` — any nonzero lane gets `ceil(size/8)` slots. With eight negotiated lanes a Client32 data-lane receive sequence has a 2-slot base channel, ~11 H1 slots and ≤170 KiB of handoff bytes; at 300 Mbps that is ~4.5 ms of traffic. Any worker hiccup longer than that (GC, a slow `clientReceive` batch, lock contention) is now a Transfer loss instead of 4 ms of TCP backpressure on the H1 socket.

**Verified recovery cost of one refused reliable Pack.** The receiver silently drops it; subsequent Packs of that lane are held as future items (`transfer.go:18115-18290`) and the inner TCP segments inside them — DATA on the client side, pure ACKs on the provider side — cannot be delivered until the hole fills. Recovery paths:

1. SACK gap: three later deliveries, one immediate retransmit per item (`SelectiveAckGapThreshold: 3`, `transfer.go:942-947`). If the retransmit arrives while the same lane's channel is still saturated it is refused again, and "ordinary timeout/backoff owns any further loss".
2. Timeout resend: `ScaledRtt` (RttScale 2.0) with per-item exponential backoff to `MaxResendInterval = 8 s` (`transfer.go:906-914`, `9086-9111`), *plus* `DeferTimeoutResendWhileCumulativeProgress` deferrals (`TimeoutResendDeferLimit: 2`, `DeferTimeoutResendBackoff: true`; `transfer_recovery_selection.go:300-350`, `transfer.go:2821-2829`) that re-arm at 3× then 7× the interval while any cumulative ACK progress exists.

The observed stalls (5.80, 6.04, 6.31, 6.86, 7.29, 8.79, 8.86, 9.16 s) sit exactly in the 2 s + 4 s (+ deferrals) to 8 s band of that backoff ladder. A 60 s `GapTimeout`/`SelectiveAckTimeout` would show as ~60 s stalls, and genuine carrier loss on H1 is near zero, so a 5–9 s stall on H1 is almost a signature of "receiver refused a Pack and the sender had to time out".

**Why it decays across windows (speculation, mechanism verified).** The provider→client `SendSequence` is per destination and lane, and survives the per-window HTTP cancel/join. Its `RttWindow` keeps 128 samples / 60 s (`transfer.go:925-926`). RTT is measured to the *delivered* ACK, and delivery is stalled behind holes, so every HOL episode injects multi-second RTT samples; `ScaledRtt` then inflates the next timeout, which lengthens the next stall, which inflates RTT further. Per-item `sendCount` backoff resets per item, but the RTT window and the delivery-rate ring (`deliveredBytes`, `windowPacer.service`) do not. This is a positive feedback loop that persists across windows and matches "client RX drains to zero between windows but throughput keeps falling": the retained state is on the *sender* (provider), in RTT/pacing estimates, not in any memory budget. The writeup's suspicion in "What the repeated-window state does and does not show" is pointed at the right owner (provider-side state) but at the wrong kind of state (bytes rather than timers).

### Cheapest confirmation (no new code)

The counters already exist and are already exported by the mobile sampler the readbacks come from: `pack_handoff_drops`, `pack_handoff_drop_bytes`, `ack_handoff_queue_full_drops`, `provider_pack_handoff_drops`, `timeout_resend_writes`, `provider_timeout_resend_writes` (`sdk/memory_sampler.go:66-116`; `Client.ReceiveStats()` at `transfer.go:2615-2640`). Re-read the existing `WINDOW-STATE-READBACK.json` and `FINAL.json` for the More-RX64 run:

- If `pack_handoff_drops` (client) and/or `provider_pack_handoff_drops` are nonzero and step up at the same cuts as `timeout_resend_writes` (61 → 987), hypothesis 1 is confirmed without running anything.
- If both are zero across all five cuts, hypothesis 1 is refuted and I would move to hypothesis 2 below.

The discriminating performance experiment is a one-hunk A/B: restore `packHandoffTimeout(transportType, carrierReliability)` at `transfer.go:5154` (keeping the `ackMessageDetailed(..., 0)` change if you like; it is nearly harmless, see §4) and re-run the identical More-RX64 harness. Expected result if the hypothesis is right: all five windows valid at ~300–430 Mbps again, `timeout_resend_writes` flat in the tens.

## 3. Where I agree and disagree with the writeup

| Claim | Verdict | Evidence |
| --- | --- | --- |
| Inner TCP pure ACKs are Transfer-reliable in both directions and are ordered behind DATA holes | **Agree, verified** | Client: `ipPacketTransferAckForRequest` returns true for all TCP (`ip_remote_multi_client.go:19030-19032`); provider return: `providerReturnIpTransferOptions` sets `Ack = true` for TCP (`ip.go:9447-9460`). Ordered future handling at `transfer.go:18115-18290`. |
| Outer Transfer ACKs already bypass the ordered queue | **Agree** | ACKs go to `sendBuffer.ackMessageDetailed` from the pump (`transfer.go:4980`), never through `ReceiveSequence`. |
| "NoBudget changes memory admission, not ordering" | **Cannot verify** | There is no `NoBudget` symbol anywhere in `connect/` or `sdk/` non-test source. It lives only in the private external candidate. The writeup should say so; a reviewer cannot audit it. |
| A new "reliable immediate delivery" Pack option is needed | **Disagree, see §5** | Immediate delivery already exists as `Nack` Packs: `receiveNack` delivers straight to `receiveHead` with no sequence check (`transfer.go:18391-18500`). What the writeup wants (reliable + unordered) is a third delivery class; it is not needed for pure ACKs, which are idempotent and superseded by later ACKs. |
| Restrictive RX/TX/Pack/Head leaves under the root strand capacity | **Partly agree, but not causal for the decay** | Verified tree: `sdk/device_local_transfer_memory.go:26-40` (root → client/provider/nat → RX/TX/Pack/headRecovery). Leaf ceilings are permission, not reservations. The More-RX experiment itself refutes RX as the decay cause: +28% RX gave the same first-window ceiling and the same decay. RX leaf used was 0 at boundaries 2 and 3 while throughput still fell. |
| First-window ceiling ~300–430 Mbps | **New observation** | `receiveWindowAdvertisement` = min(`ReceiveQueueMaxByteCount`, RX pool total) (`transfer.go:13253-13262`). At the 32 MiB target the RX leaf is 2,181,038 B; over the 40 ms shaped RTT that is 2,181,038 × 8 / 0.040 ≈ **436 Mbps**, which is the reference ceiling almost exactly. The 32 MiB profile is receive-window-bound by design; "one shared aggregate" will not raise it unless the per-sequence advertisement is also changed, and advertising the whole root per sequence would overcommit N× across sequences. Treat the ~430 Mbps as the expected ceiling, not as a baseline to exceed. |
| Lanes isolate flows | **Agree in principle, but they multiply the hazard** | Eight lanes divide the per-sequence handoff channel by eight (`transfer_logical_lane.go:42-48`) and create eight independent ordered heads, so a given rate of refused Packs produces eight independent HOL stalls instead of one. Under R14 this makes lanes net negative. Under the pre-R14 wait policy the division only costs backpressure granularity. |
| "Separate budgeted-provider finding" (A fills provider TX, B's first DATA has no admission) | **Plausible, not reviewed** | The fairness problem is real but orthogonal to the decay. I agree with the writeup's own instinct to not adopt the first-DATA reserve until the simpler fix is measured. |

## 4. Gaps and likely-wrong conclusions in the writeup

1. **The regression window is misidentified.** The source comparison enumerates accounting, lanes, host construction and provider count. It omits R14's shared-pump admission change, which MEMSTEADY itself records as "unexecuted". Any "matched old/new source regression experiment" must include `d90ac7f4` as a factor; everything else is second order until that factor is removed.
2. **"They do not demonstrate a client RX leak" is right, but the conclusion drawn ("persistent recovery/congestion/retention state") stops short.** The retained state that plausibly decays is the sender's RTT window and pacing/service estimate (`RttWindowTimeout 60 s`, `rttWindow`, `deliveredBytes`, `windowPacer`), which are not memory owners and are not in the readbacks. Add per-sequence `ScaledRtt()` and `sendWindowSnapshot()` to the window-boundary cut (see §7).
3. **The motivating R14 test is a one-slot composition (`private_h1_client_ack_progress_test.go:64-75`).** It proves that a blocked handoff can delay an ACK for *another* sequence on the same H1 stream. It does not measure what the fix costs under load, and the fix chosen — convert the wait into loss on a reliable carrier — trades a bounded multiplexing delay for a 2–8 s recovery ladder. The real problem that test exposes is a *slow or blocking downstream callback*, which CODESTYLE already forbids; dropping at the pump treats the symptom.
4. **ACK-drop on `TryLock` is mostly inert in production but is a latent bug.** `ackMutex` is held only inside `ackMessageDetailed` and briefly in `Run` as a barrier (`transfer.go:9860-9861`), and there is one pump per Client, so contention is rare. But when it does happen the ACK is simply lost; on a reliable carrier that is the one place a Transfer ACK can now vanish. Keep the TryLock if you must, but fall back to a bounded wait (the old `H1AckHandoffTimeout = 1 ms` on mobile) instead of dropping.
5. **The pure-ACK analysis has the wrong hole.** On the client→provider direction during a download, almost every Pack *is* a pure ACK. The predecessor a pure ACK waits for is usually another refused pure ACK, not "missing DATA". That matters for the remedy: these ACKs are redundant with every later ACK, so the correct policy is to not retransmit them at all (`NoAck`), not to invent reliable-unordered delivery for them.
6. **"RST-class native refusal counters rose too" is consistent with the same cause.** A provider TCP stack that stops receiving ACKs for seconds will RTO, shrink cwnd, and eventually reset; the client NAT sees RSTs for flows the browser has already abandoned. This is downstream of the holes, not a separate fault.
7. **The 90-second Fast.com device timeouts** ("no positive download bytes for 21.3 s / 41.4 s while control bytes continued") are what the backoff ladder looks like when several lanes stall in sequence on a device with 2-slot lane channels and a slower worker. They do not need a UI/detector explanation first.

## 5. Root-cause hypotheses, ranked

### H1 (most likely): R14 zero-time handoff converts H1 flow control into Transfer loss; HOL + resend backoff + RTT inflation produce the decay

Evidence: §2. Confirm/refute: read `pack_handoff_drops` / `provider_pack_handoff_drops` in the existing readbacks (zero cost); then the one-hunk A/B at `transfer.go:5154`. Expected: valid 5/5 windows, flat `timeout_resend_writes`.

### H2: Provider-side `ReceiveSequence` worker is intermittently slow, independent of R14 (e.g. NAT write contention), and R14 only exposed it

Evidence: the pre-R14 test needed the handoff to wait at all, which means the worker *was* falling behind in some runs. Confirm: with the wait restored, sample `PackHandoffWaitCount` / `PackHandoffWaitSuccess` and `platform_h1_receive_backpressure` per cut. If waits are frequent and long (hundreds of ms), the worker or its callback is the bottleneck and deserves a profile (`clientReceive` batch at `ip_remote_multi_client.go:21478`, provider NAT ingress). This is a second-order throughput ceiling, not the decay.

### H3: Sender-side estimator state (RTT window / pacing service) is poisoned and never recovers within a run

Evidence: mechanism verified in §2; whether it is the dominant cross-window carrier is speculation. Confirm: log `ScaledRtt()`, `deliveredRate`, and `sendWindowSnapshot()` per provider SendSequence at each window boundary. If `ScaledRtt` grows from ~80 ms to seconds across windows and does not decay during the drained gap, H3 is confirmed as the memory of the decay. Cheap mitigation if confirmed: exclude RTT samples whose ACK was delayed by receiver-side hold (the receiver already reports `receiverCompressionMicros`/`receiverAdjustedNanos`; a held-behind-hole item could carry a "delivered late" flag so the sender discards it as an RTT sample).

### H4: Eight lanes × tiny per-lane channels on mobile

Evidence: verified sizing (§2). On its own, with the wait policy restored, lanes cost only backpressure granularity, so this is a multiplier for H1, not a root cause. Confirm: after H1 is fixed, A/B lanes 0 vs 8 at matched settings (the writeup's planned factor). My expectation is that lane zero wins on a phone and the difference is small on Desktop128.

### H5: Leaf ceilings / full-root admission gap

Evidence against as a decay cause: §3 row 5. The full-root counterexample in the writeup is a real correctness concern for the *future* shared-root design, but there is no evidence it is active now (root used 0.4–2.7 MB of 43.6 MB at every cut). Park it behind a deterministic test; do not block the performance fix on it.

## 6. Simpler designs the writeup should consider before building "reliable immediate delivery"

1. **Restore carrier backpressure on reliable carriers; keep the pump nonblocking only where it must be.** Concretely: pass `packHandoffTimeout(transportType, carrierReliability)` again at `transfer.go:5154`. If the multiplexed-ACK starvation from the R14 test must be bounded, use a *short* wait (e.g. 5–20 ms) and then fall back to the H1 reader's own backpressure (stop reading the socket) rather than dropping. The sender's TCP already handles a stalled reader correctly; the Transfer resend ladder handles it terribly.
2. **Re-enable H1 adaptive deepening on mobile within `PackQueueBudget`.** The budget is 1.5–2 MiB (`sdk/mobile_memory_policy.go:36-37`); the per-sequence cap is 128–170 KiB; the adaptive machinery (`tryDeepenH1PackQueue`) already checks the budget. Zeroing the adaptive fields at `:406-411` leaves >1 MiB of already-reserved budget unusable for absorbing worker jitter.
3. **Make client→provider pure TCP ACKs `NoAck` instead of adding a third delivery class.** `receiveNack` is already immediate and unordered (`transfer.go:18391-18500`). Pure ACKs are idempotent and strictly superseded by any later ACK from the same flow; losing one costs at most one inner-TCP RTO *only if* no later ACK follows, and the client's TCP stack generates ACKs continuously while data arrives. Change `ipPacketTransferAckForRequest` (`ip_remote_multi_client.go:19030`) to return false for `IsTcpAckOnlyPacket` (`ip.go:11364`) packets, respecting the existing contract-headroom rule for NoAck (`transfer.go:18420-18460`). This gives the writeup's item 2 for the one traffic class that matters with zero protocol change, no dedup marker, no new ACK type, and no "lost immediate ACK" problem. Provider→client pure ACKs (upload case) can follow after measurement. Keep SYN/FIN/RST and DATA ordered, exactly as the writeup proposes. The one risk to test: the receiver drops NoAck Packs whose contract is not yet open (`transfer.go:18433-18450`); the FILLFRACTION headroom rule is designed to prevent that but needs a lane-zero test with a fresh contract.
4. **Do not pursue the "one shared aggregate" rewrite as a performance fix.** It is a reasonable long-term simplification, but (a) the advertisement would need a per-sequence share rule to avoid N× overcommit, (b) the full-root repair guarantee is unproven, and (c) nothing in the evidence points at leaf ceilings for the decay. Measure after H1 is fixed; if RX/TX leaves still bind, raise them in the existing tree.
5. **Lanes: decide by measurement after H1, and default to lane zero on mobile if the delta is small.** Fewer ordered heads is strictly better under any residual loss.

## 7. What to add to the window-boundary cut (instead of more hypotheses)

All of these exist today or are one-line reads; none require new workers, timers or per-packet logging:

- Client and provider `ReceiveStats()`: `PackHandoffDropCount`, `PackHandoffDropByteCount`, `PackHandoffWaitCount`, `PackHandoffWaitSuccess`, `PackHandoffMaxCount`, `ReceiveQueueDropCount`, `ReceiveQueueEvictionCount`, `ReceiveQueueTentativeEvictionCount`. The first two discriminate H1 from everything else.
- `AckHandoffQueueFullCount` (ACK TryLock refusals).
- Per provider→client `SendSequence` at each cut: `rttWindow.ScaledRtt()`, `rttWindow` sample count and age, `sendWindowSnapshot(now)` (Window/Reason/ServiceByteRate), resend queue size and oldest item `sendCount`/`timeoutDeferCount`. This names the "accumulating owner" the writeup says it cannot name.
- The split of `timeout_resend_writes` vs `selective_gap_writes` vs `ack_tail_probe_writes`. Growth dominated by `timeout_resend_writes` means SACK gap recovery is failing (retransmits refused again), which is the H1 saturation signature.

## 8. Prioritized next steps

1. **Today, zero code:** re-read the More-RX64 `WINDOW-STATE-READBACK.json` / `FINAL.json` for `pack_handoff_drops`, `provider_pack_handoff_drops`, `ack_handoff_queue_full_drops` per cut. Report the five values alongside `timeout_resend_writes`.
2. **One-hunk A/B:** `transfer.go:5154` back to `packHandoffTimeout(transportType, carrierReliability)`; rerun the exact More-RX64/Baseline64 harness (one process per arm, same gates). This is the "deterministic old-source failure vs small performance test" the writeup asks for in question 8. Keep R14's test (`private_h1_client_ack_progress_test.go`) but change its expectation to "ACK delivered within bounded time" rather than "third Pack refused".
3. If (2) restores 5/5, decide the bounded policy for the shared pump: short wait then socket backpressure, never drop on `CarrierReliabilityReliable`. Re-enable H1 adaptive deepening on mobile under `PackQueueBudget`. Rerun the mobile32/64 constructors on the allowlisted phones.
4. Add the §7 sender-side estimator fields to the cut. If `ScaledRtt` is still inflating after (3), implement the late-delivery RTT sample exclusion (H3).
5. Implement §6.3 (client pure TCP ACK → `NoAck`) behind a setting, with the lane-zero fresh-contract test and the writeup's "signed lane-zero DATA-hole" test re-targeted at NoAck instead of a new Pack field. Measure the loaded-small-request tail; this is where it should help most.
6. Only then run the lane factor (0 vs 8) and the accounting factor. Expect lanes to matter less once holes are rare.
7. Park the shared-root rewrite, the full-root counterexample, the first-DATA provider reserve, and "reliable immediate delivery" with its receipt/identity machinery until 1–6 are measured. The writeup already states the right principle ("do not retain overlapping mechanisms solely because work has been invested in them"); apply it to the proposal itself.

## 9. Answers to the writeup's numbered questions, briefly

1. Smallest discriminating experiment: §8 step 2. Repeated-window decay after RX drains: sender-side RTT/pacing memory fed by HOL-induced ACK delay (H3), seeded by refused handoffs (H1).
2. Retained-cost over-reservation: not reviewed in depth; it is not the active constraint (root used ≤ 2.7 MB of 43.6 MB). Measure after H1.
3. The reliable-immediate design is not needed for the traffic that motivates it; `NoAck` already gives immediate delivery with existing dedup semantics for idempotent pure ACKs. If a reliable-unordered class is ever needed for non-idempotent payloads, the simplest primitive is a separate logical lane per class (ordering is per lane already), not a new ACK type.
4. Bounding protocol in-flight metadata: moot for pure ACKs under `NoAck`.
5. Full-root repair: safe refusal before SACK is preferable to more token machinery; but defer.
6. Lanes after protocol delivery is fixed: measure; expect lane zero to be adequate on mobile.
7. Missing measurement: §7.
8. Simpler aggregate fix: restore reliable-carrier backpressure at the pump (§6.1). Invariant: *a Pack read from a reliable carrier is never discarded by the receiver for lack of local queue space.* Deterministic old-source failure: a one-slot sequence with a slow callback and a 32-Pack burst over H1; on R14 it must show `PackHandoffDropCount > 0` and a timeout resend, on the fixed source `PackHandoffWaitCount > 0` and zero resends. Falsifier: if `pack_handoff_drops` is zero in the decaying runs, this explanation is wrong.
