# Independent review 2 of SIMPLERISSUE2.md

Date: 2026-11-09. Reviewer: independent, not anchored on other reviews (REVIEW1 and the SIMPLERISSUE reviews were not read).

Source reviewed: `connect` at `a321ff90` (main) and `sdk` at `fe9f190f`. Line numbers below are from those trees. Everything marked **verified** was read in source; everything marked **speculation** was not.

## 0. One caveat that frames the whole review

**Verified:** none of the M2/M3 candidate mechanisms described in the writeup exist in the repository. `grep` for `DeliverImmediately`, Pack field 14, a held-protocol counter, or a first-DATA reserve finds nothing in `connect/*.go` or `protocol/transfer.proto`. The writeup's "37 production files" live outside the frozen tree. So (a) this review analyses the committed receive/send path, which is the base the candidate is built on, and (b) nobody reviewing from the repo can check the candidate's correctness claims. If reviews are wanted on the candidate, land it on a branch.

That said, the committed tree already contains what I believe is the dominant mechanism, and the candidate does not change it.

## 1. My understanding of the problem, and where I agree/disagree

### What the symptoms say

Across the writeup's own numbers, one correlation is near-perfect and the writeup does not name it:

| Window | Whole-run Mbps | Client RX leaf used at cut (of 4,362,076) | Provider timeout-resend delta |
| --- | --- | --- | --- |
| 1 | 41 | 4,339 KB (99.5%) | 1,963 |
| 2 | 13 | 4,355 KB (99.8%) | 2,069 |
| 3 | 16 | 4,335 KB (99.4%) | 1,390 |
| 4 | 43 | 2,418 KB (55%) | 787 |
| 5 | 304 | 428 KB (10%) | 10 |

Throughput is inversely proportional to provider *timeout* resends, and timeout resends track RX-leaf saturation. The root was never close to full (4.7 of 43.6 MB). The device run in SIMPLERISSUE.md showed the same: "as little as 348/92 bytes free under the 4,362,076-byte RX leaf, with substantial root capacity remaining".

The writeup treats this as "a direction, not a cause". I think the code makes it a cause, by the following chain, every link of which is verified in source.

### The chain (verified)

1. **The client advertises the whole RX pool to every sender sequence.** `receiveWindowAdvertisement()` returns `min(ReceiveQueueMaxByteCount, budget.TotalByteCount())` (`transfer.go:13253-13263`). On a pooled mobile client `ReceiveQueueMaxByteCount` is raised to at least the pool ceiling (`sdk/mobile_memory_policy.go:141-160`), so every sequence from every provider, on every lane, is told "you may have 4,362,076 bytes outstanding from the delivered point". With 12 providers x up to 8 lanes that is a permitted aggregate of tens of MB against a 4.36 MB shared leaf. The function's own comment admits it: "This legacy field does not promise retained-cost or shared-sequence credit. Physical-root admission ... may refuse before that encoded ceiling is reached" (`transfer.go:13245-13251`).

2. **The advertisement is in payload units, the charge is in retained units.** The scan charges `metadata + payloadRoot + carrierRoot`, each rounded up to a pool size class (`transfer_receive_memory_scan.go:143-155, 541, 559, 816-820`; `message_pool.go:889-905`; `decodedPackOwnerQueueByteCount = 1024`, `transfer.go:1459`). A 1,400-byte Pack costs roughly 2,048 (payload root) + 2,048 or the packet-pool class (carrier) + ~1.5 KB metadata, i.e. 3-4x its wire bytes. `sdk/mobile_memory_policy.go:130-140` documents the mismatch explicitly and says "the eviction notice turns [the refusals] into resends". That sentence is wrong for the path that matters (next point).

3. **A refusal at decode time is silent.** When `takeReceiveMemoryOwner`, held-head repair and head recovery all return nil, the frame is dropped: `requestReceiveMemoryPressure` + `recordReceivePackHandoffDrop`, return `(false, pressure=true)` (`transfer_receive_memory_decode.go:91-105`). The caller just returns the buffer and continues (`transfer.go:4772-4781`). No SACK, no eviction notice (the item was never held, so `evicted_sequence_numbers` cannot name it), no NACK. There is no refusal signal in the `Ack` message at all (`protocol/transfer.proto`, fields 7-12). The sender learns of the loss only when its retransmit timer fires.

4. **The sender's timer is tuned to assume silence means queueing, not refusal.** `DeferTimeoutResendWhileCumulativeProgress=true`, `TimeoutResendDeferLimit=2` (`transfer.go:973-974`). `shouldDeferTimeoutResend` defers a due timer whenever *any* cumulative ACK arrived after `sendTime - scaledRtt` and after the last deferral (`transfer.go:3292-3315`). On a sequence that is still receiving ACKs for the Packs that *were* admitted, every refused Pack gets up to two extra intervals with exponential backoff (`transfer_recovery_selection.go:329-347`, `deferredResendInterval` at `transfer.go:2823-2829`). Cold floor is 2 s (`MinResendInterval`, `transfer.go:906`), max 8 s (`:914`). The §13.5 comment says "deferring is right while the queue drains and wrong once the Pack is gone" — and after a silent refusal the Pack is gone.

5. **Nothing bounds the aggregate.** `tryReserveWithLock` walks the parent chain (`transfer_memory_budget.go:267-283`), so the leaf is a hard ceiling for all sequences together, while each sequence has been told it alone may fill it. There is no per-sequence fair share, no free-space signal, and no backpressure other than drop.

Put together: once the RX leaf fills, every provider sender is pushing into a receiver that drops a fraction of every window without saying so, and recovers each drop at 2-8 s cadence. That is exactly the RTO-paced goodput collapse in windows 1-3, and it recovers only when the leaf drains (window 5).

### Where I agree with the writeup

- Reliable immediate delivery of pure TCP ACKs is a real fix for a real head-of-line problem (SIMPLERISSUE.md's "confirmed protocol-ordering mechanism"). I did not re-verify it, but the design of ordered Transfer sequences carrying inner TCP ACKs behind DATA holes is visible in the receive queue structure.
- The "current prefix helper accepts only recovery-paid predecessors" hypothesis is **verified**: `receiveConsumerRecoveryPrefixWithLock` skips any predecessor whose `item.memoryBudget != budget` (the recovery child) or whose `ingressLoanByteCount != 0` (`transfer_receive_memory_head_recovery.go:124-128`). An ordinary-RX-paid contiguous prefix does not count. Also `establishedCurrent` requires `!scan.head` (`:60-61`), so a Head-promoted retry cannot use the established-current fallback and must qualify by promise/prefix/backlog instead.
- Head=true being correlated with, not necessary for, the 4 s re-arrival: agreed, and I can explain the 4 s exactly (section 3).
- The full-root counterexample is real: the promise reserves bytes sized from the observed tail (`transfer_receive_hold_promise.go:79-113`), so a larger missing predecessor at a full root cannot be funded. The design already admits this ("if the budget refuses it the shortfall is handled, and counted, at admission").

### Where I disagree

- **"Root capacity was not exhausted at these cuts" is presented as weakening the memory hypothesis.** It strengthens it: the leaf was exhausted while the root was 90% free, which is the exact failure mode the "one shared resource" design was meant to remove. The problem is not memory; it is that the leaf is a flow-control boundary that no one advertises and no one is told about.
- **"Client live drop measurements were unavailable."** They are exported today: `PackHandoffDropCount` (`transfer.go:1787, 2620`) is sampled into `pack_handoff_drops` in `sdk/memory_sampler.go:66,460`, and `ReceiveQueueDropCount`, `TimeoutResendDeferCount`, `TimeoutResendWriteCount`, `LaneHeadPromotionCount` exist on the client stats (`transfer.go:1804, 2010-2014, 2112`). If the harness did not record them, that is a harness gap, not an observability gap.
- **The reserve/promise/pressure/head-recovery machinery is treated as the way to make a full leaf safe.** It cannot be: the leaf fills because the senders were told it was theirs. Reserves decide who gets the last byte of a leaf that should never have been overrun. Each new reserve is a new partition of the same 4.36 MB and recreates the stranding problem one level down.
- **The writeup's "unit mismatch" and "legacy encoded advertisement" are mentioned only as a caveat in a comparison table.** They are the mechanism.

## 2. Gaps, unverified assumptions, likely-wrong conclusions

1. **No measurement of client-side refusals per window.** `pack_handoff_drops` delta per window is the single number that confirms or refutes section 1. It costs nothing.
2. **The 4 s re-arrival trace was never joined to the sender's timer state.** The writeup proposes a test for it; the arithmetic is already determinable from the defaults (section 3).
3. **"Provider timeout-resend deltas" are cumulative counts across all client destinations.** They should be split by `recoveryKind` (`recordSendRecovery`, `transfer.go:3317-3340`): timeout vs selective-gap vs probe. If timeouts dominate and gap recoveries are near zero, the receiver is not SACKing what it drops, which is the silent-refusal signature.
4. **The "more RX" experiment is read as disproving an RX cause.** Raising the leaf 28% without changing the advertisement does nothing to the overrun ratio (each sender still thinks it owns the whole leaf), so the first window benefits and the rest do not. That result is consistent with section 1, not against it.
5. **Decay across windows in the More-RX64 run with RX draining to zero between windows** is not explained by RX saturation alone. Something persists on the provider side or in the sender's RTT/backoff state (`RttWindowTimeout` 60 s, `SelectiveAckTimeout` 60 s, `GapTimeout` 60 s, `AckTimeout` 60 s). The 60 s constants all line up with roughly the inter-window period; I would check whether windows that start within 60 s of a bad window inherit a `qualityPending`/inflated `ScaledRtt` floor (`transfer_rtt.go:349-385`) or a resend queue full of SACK-leased items. Speculation.
6. **The sender never shrinks its recorded advertisement unless told** (`transfer.go:13185-13193` stores whatever the latest Ack says; the mobile policy resizes the pool on provide-mode changes, `setProvideActive`, `sdk/device_local_transfer_memory.go:95-110`). Minor, but it means a provide-on/off flip mid-run can leave senders with a stale, larger window.
7. **"Zero provider drops" is not evidence about the client**; the writeup says this itself. Good.
8. **The 8-lane setting multiplies the overrun.** Each lane is a separate `ReceiveSequence` with its own advertisement (`receiveSequenceId` includes `LogicalLane`, `transfer_receive_memory_decode.go:79-80`). Lanes were enabled on the client (`sdk/mobile_memory_policy.go:482-485`) in the same change set that moved to exact accounting. The writeup lists "lane zero vs eight" as a factor to separate; it can be separated by arithmetic: 8x the permitted aggregate. This does not require lanes to be bad; it requires the window to be honest.
9. **The committed `TestReceiveEncodedAdvertisementDoesNotPromiseDecodedPhysicalCapacity`** (`transfer_receive_encoded_physical_capacity_test.go:1-10`) encodes the mismatch as acceptable ("a capacity-contract witness, not a delivery or speed claim"). It should be read as the regression test for the bug, not as a contract.

## 3. The 4-second re-arrival, explained from defaults (verified arithmetic, unverified against the fixture's logs)

Trace: DATA #4 sent at t≈0; cumulative ACK through #3 arrives at t≈0; #4 is refused at the receiver (future, no owner admitted); #4 re-arrives at ~4 s with Head=true.

- At send, the item's first `resendTime` is one `ScaledRtt`. With no samples (or `qualityPending`) `ScaledRtt` is the 2 s cold floor (`transfer_rtt.go:367-385`; `MinResendInterval`, `transfer.go:906`). Timer due at 2 s.
- At 2 s, `shouldDeferTimeoutResend`: `lastCumulativeAckTime` (t≈0, set by ACK #3 at `transfer.go:14111`) is after `sendTime - scaledRtt` (= -2 s) and after `timeoutDeferAckTime` (zero); `timeoutDeferCount` 0 < 2. Deferred. `deferredResendInterval` = `resendIntervalForItem(sendCount 1 + deferCount 0)` = `ScaledRtt` = 2 s (`transfer.go:2823-2829`, `:9086-9106`). New due time 4 s.
- At 4 s, no newer cumulative ACK, so the deferral predicate fails; the item is written. It is `sendItems[0]`, so `setHead` runs and `Head=true` (`transfer_recovery_selection.go:363-379`).

So Head is a side effect of being oldest, and the 4 s is "cold floor + one deferral granted by an ACK that arrived at the same instant as the send". The deferral was designed for a deep queue and here it extended a drop. **Cheapest discriminating test:** rerun the same fixture with `DeferTimeoutResendWhileCumulativeProgress=false`; the re-arrival should move to ~2 s. Then rerun with the receiver's refusal replaced by admission (bigger fixture RX); the re-arrival should vanish. That isolates "silent refusal + deferral" from "Head authority".

Answer to Q1: the smallest change that proves it is the one above; the smallest *fix* that preserves healthy-lane duplicate suppression is not to touch the deferral at all but to make the refusal audible (section 5, idea B), so the deferral's premise ("the Pack is queued") is actually true when it is applied.

## 4. Root-cause hypotheses, ranked

### H1 (most likely): per-sequence advertisement of the whole shared leaf, in the wrong units, with silent refusal and RTO-only recovery

Verified in code (section 1). Explains: RX leaf pinned at 99%+ in slow windows; root mostly free; timeout resends inversely tracking throughput; "more RX" helping the first window only; the device runs with 92 bytes free in the leaf; high throughput only when few providers carry traffic (window 5, SIMPLERISSUE's "five carrying providers vs ten").

Experiments, cheapest first:
1. Record `pack_handoff_drops` and `ReceiveQueueDropCount` deltas per window on the client in the existing Fast64 harness. Prediction: thousands in windows 1-3, near zero in window 5, in proportion to the provider timeout-resend deltas. Zero cost.
2. Set `AdvertisedReceiveWindowMaxByteCount` (`transfer.go:13260`) on the client to `leaf / (providers x lanes) / 3` (3 = retained/payload ratio; about 50 KB for 12x8) and rerun. Prediction: drops go to ~0, no multi-second stalls; throughput may be capped by the small window, which is fine, it is a discriminating test not a fix. If stalls persist with zero client drops, H1 is refuted.
3. Same with lanes forced to 1 (`LogicalDataLaneCount=0`) and a correspondingly larger per-sequence advertisement. Separates lane count from window honesty.
4. On the provider, split the resend counter by `recoveryKind`. If `TimeoutResendWriteCount` dominates and `SelectiveGapWriteCount` is near zero while the client is dropping, the drops are pre-SACK (decode-time), confirming step 3 of the chain.

### H2: §13.5 timeout deferral converts each refusal into 2-3 RTOs

Verified predicate; see section 3. It is a multiplier on H1, not independent. Experiment: `DeferTimeoutResendWhileCumulativeProgress=false` on providers only, with everything else fixed. Prediction: stall tails shorten (fewer 6-8 s request-to-header waits) while the drop count is unchanged. If true, it tells you the stall *duration* is sender timer policy and the stall *frequency* is receiver admission.

### H3: hold promises and head-recovery reserves cannot fund a larger-than-estimated current owner at a full leaf

Verified in code (`transfer_receive_hold_promise.go:79-113`, `transfer_receive_memory_head_recovery.go:54-66`), and the writeup's witness executed. But it is only reachable because H1 lets the leaf fill. Under an honest window, the committed-prefix hold can never contain more than the sender has outstanding (the proto comment at `transfer.proto:213-216` states this invariant), so the promise is rarely needed. Experiment: run the full-root witness with the sender clamped to an honest window; if it cannot be constructed from legal sender behaviour, the counterexample is a legacy-peer problem, not a design blocker.

### H4: state persisting across windows (60 s timers) causes the decay even when RX drains

Speculation. Candidates: SACK leases (`SelectiveAckTimeout` 60 s, `transfer.go:942`) on items the receiver tentatively evicted (tentative evictions carry no notice, `transfer.go:18229-18233`); `GapTimeout` 60 s holding zombie reorder state; inflated `ScaledRtt` with `qualityPending` floor. Experiment: at each window boundary, read provider `ResendQueue` used bytes and oldest item age, and client `receiveQueueTentativeEvictionCount`; plot against the next window's first-second throughput.

### H5: lanes themselves (ordering isolation) are the issue

Low likelihood as a *cause*; they are an 8x multiplier on H1. The writeup's "is immediate pure-ACK delivery sufficient to make lanes unnecessary" (Q5) can be answered after H1 is fixed; before that, any lane experiment is confounded by the window.

### H6: provider TX pool exhaustion by retained originals (the first-DATA reserve finding)

Real as a two-flow test, but downstream of H1: originals are retained because ACKs do not come back because the client refused them. Fix H1 and remeasure before building the reserve.

## 5. Alternative approaches the writeup has not tried

### A. Make the window honest (the real "one shared resource" design)

The sender already clamps to the advertisement (`transfer.go:13492, 13548`). Make the advertisement mean something:

- Advertise **per-sequence share of free space in retained units**, not the pool total in payload units. The retained charge is a deterministic function of the wire bytes (`scanReceiveMemoryPack` is pure over the carrier), so the sender can compute the same cost for its own frames and count its outstanding in retained units. Alternatively advertise payload units but divide the pool by a measured retained/payload ratio (the receiver knows both for every admitted Pack).
- Share policy can be trivial: `free / max(1, activeSequences)` plus a small per-sequence floor (one max Pack), recomputed on each Ack. Receivers of an Ack that *shrinks* the window must be allowed to shrink (the proto says a window must never pull the right edge inward; that applies to the *delivered-point* edge, and the sender can honour a smaller limit by pausing new sends, which is what every TCP does).
- Keep the root; drop the leaf partitioning for DATA (the writeup's stated goal). With an honest window the leaf's only job (bounding what the senders can push) is done by the senders.

What it removes: most of the hold promise, pressure, head-recovery, consumer-prefix and first-DATA reserve machinery becomes unreachable in steady state, because the receiver is never asked to admit more than it has. Protocol metadata (Q6) stays bounded because protocol Packs are small and also counted in retained units.

Deterministic old-source failure for this: N sequences against one leaf, each sender filling its advertised window with 1,400-byte Packs; assert `pack_handoff_drops == 0` and no resend has `recoveryKind == sendRecoveryNone` before the first ACK. Old source fails at N >= 2 (or N = 1 at the retained/payload ratio).

### B. Make refusal audible (small, wire-compatible, independent of A)

Add `repeated uint64 refused_sequence_numbers` to `Ack` next to `evicted_sequence_numbers` (field 9 precedent, `transfer.proto:243-256`), filled from the decode-time refusal (`transfer_receive_memory_decode.go:104`) and the hold-level drop (`transfer.go:17990, 18001`). Sender treatment: clear any deferral, schedule at `probeRtt`, and do **not** advance `lastCumulativeAckTime`. Bounded the same way as eviction notices (packed, split across Acks). This alone turns a 2-8 s stall into one RTT and gives you the exact per-flow refusal evidence the writeup says it lacks (Q7), for free, on every run. It also keeps healthy-lane duplicate suppression intact because it fires only on an actual refusal.

### C. Stop granting §13.5 deferral to items that have never been acknowledged past

The predicate accepts an ACK that arrived up to one `ScaledRtt` *before* the item was sent as liveness evidence (`transfer.go:3313`). Require `lastCumulativeAckTime.After(item.sendTime)` for the first deferral. One-line change, measurable with the section 3 fixture. Less important than B.

### D. Simplify the receive-side admission once A or B lands

With A, the "committed prefix" policy's promise is only needed against legacy peers that do not read the advertisement. Consider keeping committed-prefix semantics (never discard an acknowledged item) but dropping the pre-reservation, and refuse a future before SACK only when the sender is legacy. This is the writeup's "refusal before SACK" option, scoped to where it is actually necessary.

### E. Instrument the one join that matters

Per window on the client: `pack_handoff_drops`, `ReceiveQueueDropCount`, `ReceiveQueueTentativeEvictionCount`, RX leaf high-water, number of live `ReceiveSequence`s and their advertised windows. Per window on each provider: resend writes by `recoveryKind`, `TimeoutResendDeferCount`, `ScaledRtt`, resend-queue used bytes. All exist as counters today except the per-sequence advertisement list and the RX high-water mark (one atomic max in `tryReserveWithLock`'s caller).

## 6. Brief answers to the eight questions

1. Section 3 and idea C; the preserving mechanism is B (refusal signal) rather than a timer change.
2. Yes: a contiguous, published, ordinary-RX-paid prefix should count for recovery admission of its successor; drop the `item.memoryBudget != budget` and `ingressLoanByteCount != 0` exclusions in `receiveConsumerRecoveryPrefixWithLock` but keep: same `ingressSequence`, `receivePublishedReliable`, `receiveConsumerData`, dense bitmap. Adjacent negative: a prefix paid by a *promise loan* that the worker later returns to the promise (`releaseReceiveMemory`, `transfer_receive_memory.go:319-355`) must not have funded a successor that outlives it; test by returning the predecessor before the successor delivers. But under A this path is rarely hit.
3. Do not make the receiver guarantee progress against an arbitrary missing owner; make the sender unable to create one by giving it a real window (A). Refusal-before-SACK is the right legacy-peer fallback only.
4. Yes, scratch/rewrite promises are over-reserved relative to simultaneously live cost, but the bigger win is not sharing them, it is not needing them (A). Do not remove charges to recover speed; the speed loss is RTO, not capacity.
5. Cannot be answered until the window is honest; lanes are currently an 8x multiplier on the overrun. Experiment: fix A, then lanes 1 vs 8 with equal aggregate window.
6. Bound serialized protocol originals by the same retained-unit window; protocol Packs are small, so a modest reserve share (e.g. one max Pack per sequence) keeps ACK throughput independent of DATA pressure.
7. `pack_handoff_drops` delta per window on the client, joined to provider timeout-resend writes by `recoveryKind`. If both are high together and SACK-driven recoveries are low, it is RX refusal; if provider TX used is pinned while client drops are zero, it is provider retention; if neither, look at native TCP.
8. Idea A (+ B as the safety net). Invariant: a receiver is never offered more retained bytes than it has advertised free. Deterministic old-source failure: section 5A. Falsifier: with the honest window in place, drops stay at zero but throughput and stalls do not improve; then the bottleneck is elsewhere (provider TX, native TCP) and the window was not the cause.

## 7. Prioritized next steps

1. **Today, no code:** rerun the existing Fast64 harness recording `pack_handoff_drops`, `ReceiveQueueDropCount` per window on the client and resend writes by `recoveryKind` on providers. This decides H1 in one run.
2. **One-setting experiment:** `AdvertisedReceiveWindowMaxByteCount` clamp on the client (section 4, H1 experiment 2). If stalls disappear with drops at zero, H1 is confirmed and the design direction is A.
3. **Section 3 fixture:** flip `DeferTimeoutResendWhileCumulativeProgress` and watch the re-arrival move from 4 s to 2 s; then enlarge the fixture RX and watch it vanish. Closes the "Head vs admission" question.
4. **Land B (refusal notice)** as a small, optional-field, wire-compatible change with the sender treating it as "resend at probe RTT, no progress credit". It is useful regardless of A and gives permanent per-flow evidence.
5. **Design A**: honest per-sequence advertisement in retained units (or payload units divided by the measured ratio), shrinkable, computed from free space over active sequences. Remove the RX leaf as a DATA partition once A is in. Keep root.
6. Only after 1-5: decide on lanes (Q5), and retire whichever of promise/pressure/head-recovery/first-DATA-reserve paths become unreachable under an honest window. Keep committed-prefix semantics for legacy peers.
7. Land the candidate on a branch so its immediate-delivery and ownership changes can be reviewed against source rather than prose.

## 8. What I did not verify

- The candidate's immediate-delivery, held-protocol-counter and first-DATA-reserve implementations (not in repo).
- Provider-side TX pool behaviour under load beyond reading `ip_provider_memory.go`'s structure.
- Exact `qualityPending` lifecycle; the 4 s arithmetic assumes `ScaledRtt` was at the 2 s floor when the deferral was computed, which the fixture's rttWindow state can confirm.
- Anything about Fast.com's detector, native TCP congestion, or H3/P2P paths (out of scope per the writeup).
