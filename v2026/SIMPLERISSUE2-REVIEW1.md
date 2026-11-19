# Independent review 1 of SIMPLERISSUE2.md

Date: 2026-11-09. Reviewer: independent. I did not read SIMPLERISSUE-REVIEW1/2 or SIMPLERISSUE2-REVIEW2. I read SIMPLERISSUE2.md, skimmed SIMPLERISSUE.md for background, and read source in three places:

- `main` at `a321ff90` (`/Users/builder/urnetwork/connect`, `/Users/builder/urnetwork/sdk`).
- The M2/M3 production donor tree that matches every `afterSha256` in `R/aggregate-core-reserve-immediate-r3/PRODUCTION-MAP.json`: `R/aggregate-core-reserve-immediate-r2/source/connect/` (37/37 hashes verified). Lines cited as `M3:transfer.go:N` refer to that tree. Files outside the 37 come from `main`.
- `R/aggregate-m2-fast64-diagnostic-r2/ACTUAL-FAST64-READBACK.json` (per-window fields).

`R` = `/Volumes/data/urnetwork/temp/aggregate-flow-r15-r2`. "Verified" means I read it in code or data. "Hypothesis" means I did not run anything.

---

## 1. Summary

The writeup is careful about what it has not proved, which is good. My main criticism is that it treats the evidence as weaker and more ambiguous than it is. Taken together, the readback and the code point at one mechanism with a feedback loop:

> **Shared RX is oversubscribed. When it is full, the receiver drops future DATA silently (no SACK and no notice). The sender learns about these drops only through stacked timers (2 s cold/quality floor + §13.5 deferral + backoff, so 2–8 s). The held items above the resulting holes keep RX pinned until those timers fire. That produces more refusals, so the system stays in a degraded state until something drains it.**

This one mechanism accounts for the low-throughput windows, the 4 s "re-arrival" in the signed trace, and plausibly the 6.8 s loaded-request stall. Most of the independent reserves and recovery predicates added so far are local patches for symptoms of it. I agree with the direction (one shared DATA resource, protocol outside DATA admission, reliable immediate ACKs). I disagree that root-only admission is blocked on the full-root counterexample. That counterexample is an artifact of having no flow-control credit. A credit-based window plus one exact-current reserve resolves it more simply (§5).

### Where I agree (verified)

- The default is `ReliableLaneProvenRecovery: false` (`M3:transfer.go:1019`). The ordinary cumulative-ACK path gives the newly exposed head a probe only when `(head.deferralOutstanding || head.sendCount > 1)` **and** `stablePacedFifo(head)` hold (`M3:transfer.go:14381-14398`). An untouched initial original keeps its timer. The writeup's source reading is correct.
- The prefix helper accepts only recovery-paid predecessors. In `receiveConsumerRecoveryPrefixWithLock`, `item.memoryBudget != budget` → skip (`transfer_receive_memory_head_recovery.go:128`). It also refuses spans > 64 (`:109`) and stops scanning after 256 visits (`:119`). So the writeup's Q2 premise is correct.
- `establishedCurrent` admits an exact-current, non-Head DATA original with no contract id or frame on an established contract (`transfer_receive_memory_head_recovery.go:60-63`). The writeup correctly says a non-Head re-offer would qualify. Note that a **Head-promoted** retry carries a contract frame or id, so it does *not* match `establishedCurrent`. It is admitted by a different branch, or by ordinary RX once RX has room.
- Provider drop counters are zero, and client receive counters were not captured. `clientReceiveCounterAvailable` is `false` in all five windows of the readback.

### Where I disagree or think the writeup undersells the evidence

1. **"Root capacity was not exhausted" is true but beside the point. The RX leaf was ~99.5% full in every slow window.** `ClientReceiveByteCount` = 4,338,624 / 4,354,720 / 4,335,136 / 2,417,760 / 427,616 against a 4,362,076 leaf. Throughput was 41 / 13 / 16 / 43 / **304** Mbps. The leaf is pinned in every slow window and nearly empty in the fast one, while the root sits at ~11% use. This is the "leaf strands root" failure that motivates the whole design, observed directly.
   - Note that `4,362,076` in the readback is `PackQueueCapacityByteCount`, not the RX leaf total. I checked that the mobile RX leaf has the same value: `mobileReceiveQueueBudgetByteCountForTarget` = `min(2 MiB×64/24, max(1.5 MiB×64/24, clientShare/10))` = clientShare/10 = 4,362,076 (`sdk/mobile_memory_policy.go:76-89`). The number is right but the field is not. The fixture should record `ReceiveQueueBudget.TotalByteCount()` explicitly.
2. **Head recovery tracks provider timeout resends almost one-for-one.** Head-recovery reserved deltas per window are ≈17.7 / 19.2 / 14.4 / 11.5 / 0 MB. `ProviderTimeoutResends` deltas are 1963 / 2069 / 1390 / 787 / 10. That is ~9–14 KB of head-recovery admission per timeout resend, and zero of both in the fast window. Timeout resends were **27% / 61% / 43% / 12% / 0.02%** of `ProviderInitialWrites`. This strongly suggests that originals were refused by ordinary RX and later admitted only through the recovery child after a *timeout*. A selective gap recovery would not show up this way: refused items are never SACKed, so the gap rule cannot fire. The writeup has all these numbers but does not join them.
3. **The slow-window throughput is close to "one RX leaf per cold RTO".** 4.36 MB × 8 / 2 s ≈ 17 Mbps, against the observed 13 and 16 Mbps. This could be coincidence (hypothesis H2 below gives a 10-minute test). If it holds, the slow regime is timer-bound rather than path-bound.
4. **The 2.09–2.10 s TCP establishment times in windows 1 and 5 are suspiciously close to `MinResendInterval` = 2 s** (`M3:transfer.go:914`). They may be one refused or lost SYN-bearing Pack recovered by the cold floor. Inner TCP SYN retry schedules are another explanation (1 s/3 s on Linux; macOS differs), so check which OS the synthetic inner stack uses. Either way the writeup should not dismiss it as "cold TCP setup".
5. **The 6.769 s request-write → response-header delay in window 4 matches a 2 s + 4 s backoff chain** (cold or quality floor, then one doubled retry) plus an RTT. This is a hypothesis, but it costs nothing to check against `TimeoutResendDeferCount` and per-item `sendCount` once client and provider counters are joined.
6. **The "+28% RX headroom" experiment does not rule out an RX-leaf explanation.** That experiment raised the leaf 28% while keeping a 10% leaf, the same per-sequence advertisement (see §2.1), and the same silent-refusal plus timer recovery. A degraded state that sustains itself (§3 H1) predicts exactly "fast first window, then collapse" whenever headroom only delays the first saturation. "Not monotonic accumulation at the cuts" is also expected: the cut shows the steady pinned level, not a ramp.
7. **"Head=true is correlated with the retry" can be explained without Head being an admission condition.** The 4 s retry is when the *sender's timer* fired (§2.2). Head promotion happens on that same resend path because `sendItems[0]` is the item (`transfer_recovery_selection.go`, `setHead` after the deferral block). So Head and late arrival are both effects of "the timer fired", not cause and effect of each other.

---

## 2. What the code says about the two open mechanisms

### 2.1 The receiver advertises the whole shared leaf to every sequence (verified)

`receiveWindowAdvertisement()` returns `min(ReceiveQueueMaxByteCount, budget.TotalByteCount(), AdvertisedReceiveWindowMaxByteCount?)` (`transfer.go:13253`; `M3:transfer.go:13409`). Its own comment says it is "independent of free space" and "does not promise retained-cost or shared-sequence credit."

For the 64 MiB mobile profile:
- The per-sequence max is `max(768 KiB×64/24, 2 MiB×64/24)` ≈ 5.59 MB (`sdk/mobile_memory_policy.go:144-162`).
- The leaf is 4.36 MB.
- So **every receive sequence advertises 4.36 MB, the entire shared leaf.**

Receive sequences are keyed by `LogicalLane` as well (`transfer.go:15523`, `ReceiveBuffer.Pack` at `:15740`). With 12 providers and up to 8 lanes plus lane 0, senders are collectively permitted **K × 4.36 MB** toward one 4.36 MB leaf. The sender's window rule uses the advertisement as a permission ceiling (`transfer.go:13548`).

The units also differ. The advertisement is in encoded bytes, while admission charges exact *retained* bytes (`ReceiveQueueRetainedByteAccounting = true`, `sdk/mobile_memory_policy.go:394`). So the overcommit is larger than the byte counts suggest.

Under this design, refusals under multi-provider bulk load are guaranteed, not incidental. Both known differences from the 300–400 Mbps reference move in the bad direction:
- Lane 0 → 8 lanes multiplies K.
- Encoded → exact retained accounting increases the charge per Pack.

The writeup lists both differences but does not connect them to a mechanism.

### 2.2 Why "DATA waits for re-arrival after becoming current" takes ~4 s (verified paths, hypothesized composition)

When ordinary RX refuses a future DATA Pack, the only effect is `receiveQueueDropCount++` (`M3:transfer.go:18472-18484`, the `!receiveDeliveryFits` branch, and the final `drop ack cannot queue` branch). No SACK, NACK, or notice reaches the sender. The code already acknowledges this in `laneTimerVerdictFor`'s comment: "an arrival above a hole it cannot queue is dropped unacknowledged" (`transfer.go:3112-3124`).

The sender therefore runs only on timers, and on the H1 path at least three of them stack:

1. **Quality/cold floor.**
   - `sharedRawRecoveryInterval` returns `MinResendInterval` (2 s) for a first write sent while a window-quality round trip is pending (`transfer_recovery_time.go:21-26`, `transfer_window_quality.go:204-211`).
   - `RttWindow.scaledRtt` also floors at `minScaledRtt` (2 s) whenever `qualityPending` (`transfer_rtt.go:375-380`).
   - `WindowQualityRemeasureInterval` is 5 s (`M3:transfer.go:1003`), so a quality-pending period recurs regularly.
2. **§13.5 deferral.**
   - When that timer fires, `shouldDeferTimeoutResend` (`transfer.go:3292`; call at `transfer_recovery_selection.go:334`) sees that the cumulative ACK advanced within one scaled RTT of the item's send. In the trace, the ACK through #3 arrived at time zero.
   - It therefore defers by `deferredResendInterval(item, …)` = `resendIntervalForItem(item, sendCount+deferCount)`, which is another ~2 s under the same floor (`M3:transfer.go:2841-2849`).
   - This deferral exists on the theory that "the lane is alive and the queue is deeper than the estimate." For a receiver-*refused* item that theory is wrong: the item is not queued anywhere.
3. **No early probe.** The cumulative-ACK head probe (`M3:transfer.go:14381`) is skipped because the item is an untouched initial write.

2 s + 2 s ≈ **4.0 s**, which matches the trace's retry at ~4 s. I have not executed this. §3 H3 gives a one-run discriminator.

**The answer to the writeup's question "why does useful re-arrival wait until the timeout":** nothing other than a timer can cause re-arrival, and the timer applies a deferral meant for *queued* items to *refused* ones.

---

## 3. Root-cause hypotheses, ranked

**H1 (most likely): a self-sustaining degraded state from silent RX refusal plus multi-second timer recovery, triggered by advertisement oversubscription of the 10% RX leaf.**

- The loop: refused Packs create holes → the items held above the holes keep RX pinned → new arrivals, including the loaded request's response, are refused → recovery only at 2–8 s.
- It explains 41/13/16 Mbps with RX pinned and 304 Mbps with RX empty, the ~1:1 timeout-resend to head-recovery ratio, the 4 s trace, and the multi-second request stalls.
- It also explains why window order does not matter: the system leaves the degraded state only when load or provider churn lets RX drain.
- **Test (no code change, same source):** run Fast64 with the client capture of `Client` stats that already exist: `ReceiveQueueDropCount` (`transfer.go:1804`, `:2631`), `TimeoutResendDeferCount` (`:2014`), `TimeoutResendWithRecentCumulativeProgress` (`:2081`). Also run them on providers.
  - Confirm if: client `ReceiveQueueDropCount` delta is roughly ≥ provider `TimeoutResends` in slow windows and near 0 in fast ones.
  - Refute if: client drops are ~0 while RX is pinned. Then the pinning is delivery-side (downstream write is slow), not reorder holds.
- This is the single highest-value measurement. The current readback has `clientReceiveCounterAvailable: false`.

**H2: in the degraded state, throughput ≈ RX leaf ÷ recovery interval.**

- **Test:** a three-arm run changing only `MinResendInterval` (1 s / 2 s / 4 s), with no other policy change. This is a diagnostic, not a proposed fix.
- If degraded-window throughput scales roughly 1/interval, timers are the binding constraint and H1's loop is confirmed. If it does not move, the bottleneck is elsewhere (provider TX retention, origin, link). `linkQueueDrops` is 285 in *every* window, which looks cumulative or static; verify what that field means.

**H3: the 4 s retry in the signed trace = quality/cold floor + §13.5 deferral.**

- **Test:** rerun the two signed SDK roots with `DeferTimeoutResendWhileCumulativeProgress=false`, as a diagnostic only.
  - If the retry moves to ~2 s, the deferral is the 2nd term.
  - Then set `MinResendInterval=1s`. If the retry moves to ~1 s, the floor is the 1st term.
- Record `TimeoutResendDeferCount` and the item's `sendCount`/`timeoutDeferCount` at the write.
- This answers Q1 deterministically without the barrier test the writeup is building.

**H4: lanes amplify oversubscription. The lane-count change, not reliable immediate delivery, is the main old/new difference.**

- **Test:** same M3 source and policy, with providers at `LogicalDataLaneCount=0`, five windows.
- Recovery toward the reference supports H1/H4. No recovery points toward accounting (retained vs encoded) or the constructor change, and the next arm is an encoded-accounting toggle.
- This also answers Q5 directly.

**H5 (less likely as the primary cause): provider TX retention or first-DATA reserve starvation.**

- The provider-side drop counters are zero, and the fast window used the same providers.
- **Test:** after H1's counters are captured, if client drops are low *and* provider `Refused`/`Offered` explains the slow windows, revisit. Note that `Refused` was 83% in the *fast* window, so it is not a failure signal by itself. Find out what it counts before using it.

**H6 (possible contributor to stalls): a protocol predecessor holds the floor on the same sequence.**

- In the trace, protocol predecessors delayed the floor and turned #4–8 into "future". Protocol and DATA still share one ordered sequence-number space per lane, so any protocol delay converts in-flight DATA into refusable future DATA.
- **Test:** count, per stalled flow, the time between DATA arrival and floor reaching it, split by whether the blocking predecessor was protocol or DATA. The recorder the writeup describes can do this if it joins the predecessor's message type.

---

## 4. Answers to the writeup's questions

**Q1 (initial-DATA retry wait).**
- Smallest deterministic proof: H3's two toggles on the existing signed roots, plus the counters.
- Smallest fix that keeps duplicate suppression on healthy lanes: **make refusal visible.** The eviction-notice channel already exists end to end:
  - receiver `noteEviction` → `takeEvictions` → `Ack.EvictedSequenceNumbers` (`M3:transfer.go:13373-13399`, `:17598`);
  - sender `observeEvictions` → `resendEvictedItemWithLock` (`transfer_pending_eviction.go:37-150`);
  - sender state is bounded (one marker per existing retry owner).
- Today `resendEvictedItemWithLock` ignores items that are not `selectiveAcked` (`transfer_pending_eviction.go:143`). The proposed change:
  - (a) The receiver calls `noteEviction(seq)` on every capacity refusal of an *authenticated, non-duplicate* DATA Pack.
  - (b) For an un-SACKed item, the sender treats a notice as "not held". It clears `deferralOutstanding`, sets a `receiverRefused` bit that makes `shouldDeferTimeoutResend` return false, and schedules the write at `max(now + probeRtt, when cumulative ACK reaches seq-1)`.
- Healthy lanes produce no notices, so they get no extra duplicates. If you want to keep the wire meaning distinct, add a sibling repeated field (`RefusedSequenceNumbers`) and keep the same bounded sender state.

**Q2 (ordinary-RX-paid prefix funding a recovery successor).**
- In terms of memory, this is safe: the recovery child's cap still bounds it.
- The adjacent negative case: a predecessor that is *published but later returned or refused by the worker* (contract failure, or a refused downstream callback). The successor is then a future item paid from recovery, and the `M3:transfer.go:18467-18476` branch drops it with no feedback. That is the same silent-hole problem again.
- Requirements if you do it: same `ingressSequence`, same contract generation, `receivePublishedReliable`, `ingressLoanByteCount == 0`, and not on a return path.
- My recommendation is **not** to do it yet. It is a fourth admission predicate on top of `promised`, `consumerPrefix`, `establishedCurrent`, and `headBacklog`. If H1 is confirmed, the refusal notice plus the credit window in §5 make it unnecessary.

**Q3 (shared-root progress guarantee).**
- The counterexample, where a 30,544 B current owner fails under a full root, exists because future DATA consumed the root with no credit discipline. Two simple invariants together guarantee progress:
  1. **Credit:** the sum over sequences of outstanding advertised windows, measured from each delivery point in retained-cost units, is ≤ the shared pool minus the reserve. A conforming sender can then never put a hole's owner outside memory, because the hole's bytes are inside the credit already granted.
  2. **Exact-current reserve:** a root-level reserve R ≥ (max retained Pack size) × (concurrently current sequences you guarantee), usable *only* by the exact-current sequence number of an authenticated established sequence. Enforce a hard protocol maximum retained size per Pack. The sender splits anything larger and the receiver refuses it with a notice, so R is exact rather than based on an "observed-tail promise".
- "Refuse before SACK" is then just "refuse beyond credit", which only a nonconforming or legacy sender ever hits. Selective recovery inside the credit is unaffected.
- This is charged headroom (R comes out of the root at configuration time), not invented headroom. It replaces the head-recovery child and several predicates.

**Q4 (scratch/rewrite promises).**
- I did not audit every charge. The question to settle is which charges are *simultaneously* live on the critical path.
- A decoded owner's retained bytes and its downstream-delivery prepayment for the same Pack are sequential in time and could share one reservation that transfers ownership, as `recoverProviderReceiveDeliveryCredit` already does for one case.
- Measure first: log the `retained/encoded` ratio per Pack class. If it is ≫1 for small Packs, that ratio multiplies the effective overcommit in §2.1.

**Q5 (are lanes still needed).**
- Run H4. With reliable immediate pure-ACK delivery, head-of-line coupling between ACKs and DATA is gone.
- Lanes would still isolate DATA of independent TCP flows from each other's holes. But if holes are repaired within an RTT (refusal notices), that isolation buys little and costs K× advertisement and K× per-sequence state.
- My prior: lanes are net-negative under a shared pool until the advertisement is credit-correct.

**Q6 (bounding protocol originals and receipts).**
- Bound by *count per sequence* with a separate small byte pool, independent of DATA. Pure ACKs are tiny and fixed-size, so a count bound (e.g. 64 per sequence) is a byte bound.
- Do not tie it to flight. A full protocol pool should coalesce: a newer cumulative TCP ACK supersedes an older one for the same 4-tuple. This makes the bound O(flows) rather than O(ACK rate). If coalescing exists, check whether it happens before Transfer custody.

**Q7 (selected-flow observation).**
- Per stalled flow, at the 5 s cut, record:
  1. the client receive sequence's floor, held count/bytes, and the oldest hole's age;
  2. client `ReceiveQueueDropCount` for that sequence since flow start;
  3. the provider send sequence's `sendItems[0]`: `sendCount`, `timeoutDeferCount`, `resendTime − now`, `recoveryKind`;
  4. RX leaf used/total.
- How to read it:
  - Refusal/retry: (2) > 0 and (3) shows a pending deferred timer.
  - Provider TX retention: (3) shows an unsent backlog with no timer.
  - Native TCP congestion: the receiver floor advances but the inner TCP cwnd is small.
  - Provider population change: (3) is missing or on a new sequence id.

**Q8 (simpler aggregate fix).**
- Proposal, in this order:
  - (i) Set the RX leaf to the client DATA root, removing the 10% leaf.
  - (ii) Use a credit-correct advertisement: `min(perSeqMax, (pool − R) / activeReceiveSequences)` in retained-equivalent units, recomputed per ACK.
  - (iii) Add the exact-current reserve R.
  - (iv) Send refusal notices on the eviction channel and suppress deferral for refused items.
- Deterministic old-source failure: two signed sequences share a pool with capacity C. Sequence A advertises C and fills holds above a hole at #k. Sequence B's exact-current Pack is refused, and B's sender retries only at ≥ 2 s.
  - Assert B is delivered within 2×RTT: this fails on the old source and passes after (ii)–(iv).
  - Also assert that A's held items are never withdrawn after SACK (no lease lie).
- Necessary invariants:
  - Σ credits ≤ pool − R.
  - Exact-current always fits R.
  - Every refusal of an authenticated non-duplicate yields a notice, or the item is within the bounded overflow counter.
  - No SACKed item is discarded without an eviction notice.
- Short falsifying experiment: Fast64 five windows. If degraded windows persist with client drops ≈ 0, the hypothesis is wrong.

---

## 5. Broader observations and simpler alternatives

- **The timers are the complexity.** On the reliable H1 path, an unacknowledged Pack's next write can be decided by a cold floor, a quality floor, an H1 residence deadline, the §13.5 deferral, the §34.3 lane verdict (off by default), the tail probe, the cumulative probe, gap recovery, eviction resend, and carrier-change recovery. Each guards against a real duplicate-storm case, but their composition is what produces 4–8 s waits.
  - Rule of thumb: **every receiver discard must be signalled, and only unsignalled loss should need an RTO.** On a reliable carrier (H1), where wire loss is impossible below Transfer, almost every "loss" is a receiver discard. Making discards explicit should let you delete most of the deferral machinery instead of tuning it.
- **Advertisement before admission.** Most of the reserve/promise/prefix/backlog machinery exists to decide who may use memory *after* senders have already sent too much. Correct credit means the receiver rarely has to choose. This is how HTTP/2 and QUIC connection-level flow control avoid exactly this class of problem.
- **Experiment hygiene.** The writeup's process (frozen 37-file overlays, 418-root qualifications, recorder arenas) is rigorous but slow relative to the questions. H1–H4 need only config toggles that already exist: `MinResendInterval`, `DeferTimeoutResendWhileCumulativeProgress`, `LogicalDataLaneCount`, `AdvertisedReceiveWindowMaxByteCount`, and the RX budget size. They also need one fixture change: capture the client `Client` stats snapshot. That should come before the barrier-controlled tests.
- **Cheap no-code arm for (ii):** set the RX leaf = client root (43.6 MB) and `AdvertisedReceiveWindowMaxByteCount` = 43.6 MB / 12 ≈ 3.6 MB on the client. This approximates credit-correct shared admission using existing knobs (`transfer.go:13258`).
  - If this arm holds ≥ 300 Mbps across five windows with no request stalls, the design question is largely answered.
  - If it collapses, H1 is wrong in an informative way.
- **Check a possible fixture effect:** windows 1 and 5 both show ~2.1 s TCP establishment. Confirm whether every window opens fresh sequences or windows. If so, each window pays a cold-RTT period (2 s floors) with no samples, which would interact with H2.

---

## 6. Prioritized next steps

1. **Capture client counters (fixture-only).** Add the client `Client` stats snapshot (`ReceiveQueueDropCount`, `ReceiveQueueDropByteCount`, `TimeoutResendDeferCount`, `TimeoutResendWithRecentCumulativeProgress`, eviction counters) and the actual `ReceiveQueueBudget.TotalByteCount()` to the Fast64 readback. Re-run the existing M3 five windows. This confirms or refutes H1 in one run. Do it before or alongside the first-stall recorder run.
2. **H3 on the signed roots:** `DeferTimeoutResendWhileCumulativeProgress=false`, then `MinResendInterval=1s`. This answers Q1 without building the barrier test.
3. **H4:** provider `LogicalDataLaneCount=0`, same source, five windows.
4. **The no-code credit approximation arm (§5):** RX leaf = root, advertisement cap = root/12.
5. If 1–4 support H1: **implement refusal notices** on the existing eviction channel, plus a sender `receiverRefused` bit that suppresses §13.5 deferral. Write the two-sequence deterministic test from Q8 first, and confirm it fails on old source.
6. Then **replace the 10% leaf with credit-correct advertisement plus an exact-current reserve.** Remove the head-recovery child, the prefix helper, and possibly the provider first-DATA reserve, if the Q8 test and Fast64 stay green without them.
7. Only after that, revisit lanes, Q4 charge sharing, and the protocol-original bound (Q6, with coalescing).

### Do not spend time on yet

- Extending the recovery-prefix predicate (Q2).
- More reserves.
- Recorder arena tuning beyond what step 1 needs.

---

## Verification status

- **Verified in code:**
  - All file:line citations above.
  - Each receive sequence advertises the full 4.36 MB leaf.
  - Receive sequences are per-lane.
  - Refusal is silent.
  - The deferral, quality-floor, and probe gating logic.
  - The eviction channel exists, and the sender ignores notices for un-SACKed items.
- **Verified in data:** every per-window number quoted from `ACTUAL-FAST64-READBACK.json`.
- **Speculation, not executed:**
  - The 2 s + 2 s and 2 s + 4 s timer compositions.
  - Throughput ≈ leaf/RTO.
  - SYN cold-floor attribution.
  - That the credit + reserve design removes the counterexample. It follows from the invariants but has not been tested.
