# Review 1: SIMPLERISSUE.md (shared admission and reliable immediate delivery)

Reviewer: independent, source-only review at `a321ff90` (2026-11-09). I did not run tests or read the private `/Volumes/data` evidence. Labels used below: **[verified]** means I read the code at the cited line. **[inferred]** means it follows from code I read but has not been executed. **[speculative]** means a hypothesis to test.

---

## 1. Summary

### What the problem is

Fast5 used to sustain 300–400 Mbps. Now it either runs slowly, or it starts near 300 Mbps and then decays geometrically across windows (299 → 129 → 48 → 16 → 3 Mbps). Small requests also stall for 6–9 s. Client RX drains to roughly zero between windows. Since the reference run, four things changed together: exact retained accounting, eight lanes, the mobile constructor with the pressure gate, and the set of providers. The writeup proposes four fixes: (a) one shared DATA root with no leaves, (b) a new reliable, unordered immediate-delivery option with terminal receipts, (c) a delivered-once marker, and (d) possibly removing lanes afterward.

### Where I agree

- **Pure TCP ACKs from the client are Transfer-reliable and ordered.** [verified]
  - `ipPacketTransferAckForRequest` forces `Ack=true` for every TCP packet (`ip_remote_multi_client.go:19030-19032`).
  - In `ReceiveSequence.receive`, a future, non-head item can only enter the hold (`transfer.go:18136-18284`). It reaches `receiveHead` only when the floor arrives (`transfer.go:18083-18100`).
  - So an inner pure ACK at n+1 waits behind a missing n.
- **The provider's native TCP already treats a pure ACK as order-independent.** `applyEstablishedPureAck` (`ip.go:4940-4950`) shows this. The extra ordering comes from the Transfer layer. [verified]
- **Hierarchical leaves strand root capacity.** `tryReserveWithLock` checks every ancestor (`transfer_memory_budget.go:271-287`), so a full RX leaf refuses a reservation even when the root has 90% free. [verified]
- **Lanes isolate ordering but not memory.** Every lane draws from the same RX leaf. The per-lane channel headroom is also divided by 8 (`transfer_logical_lane.go:41-46`). [verified]
- **Admitting what you don't know is correct.** The writeup is right to say the cumulative counters don't name an accumulating owner, and that provider TX and oldest-unacked age are missing.

### Where I disagree

1. **Proposal (b) rebuilds a primitive that already exists, at a much higher complexity cost.** Transfer already has an unordered delivery path: `NoAck`, handled by `ReceiveSequence.receiveNack` (`transfer.go:18391-18486`, dispatched at `transfer.go:17667`). It:
   - checks the contract,
   - debits the contract,
   - calls `receiveHead` directly, without consulting `nextSequenceNumber`.

   A pure TCP ACK does not need Transfer reliability:
   - It carries no bytes, so there is no custody to keep.
   - It is cumulative and monotonic, so ACK k+1 makes a lost ACK k redundant.
   - The provider TCP already recovers a lost final ACK. It has `ReturnResendTimeout` (1 s) and zero-window persist (`ip.go:494-497`, `ip.go:4155-4171`). [verified]

   So none of these are needed: Pack field 14, the terminal-immediate ACK outcome, domain-separated IDs, receipt ranges, or bounded terminal-boundary state. That design has the hardest open correctness questions in the writeup (lost immediate ACK, cumulative authority across early-retired IDs, worker recreation). **Sending validated pure ACKs as `NoAck` deletes all of them.** Details and caveats are in §4.1.

2. **Proposal (a), a single root with no leaves, is likely to make things worse unless you first add a per-sequence bound on out-of-order hold.** Today the RX leaf caps how much one stalled lane can starve everything else. If you remove leaves without a per-sequence cap on future bytes and a reserved credit for the head, one hole can absorb the whole 43 MB root. The real invariant is about **per-sequence hold bounds and guaranteed head admission**, not about aggregation (§3, H1).

3. **The diagnosis focuses on ordering. My code reading points more strongly at credit/admission mismatch and persistent sender state.** On hosted H1 the carrier is TCP, so it is lossless. Every Transfer hole is self-inflicted or comes from a relay. A healthy run should show a receive-drop count near zero. The growth in the resend counter (61 → 987) is the main symptom to classify. The writeup lists it as a possible symptom but proposes no way to attribute it.

---

## 2. Gaps and unverified assumptions in the writeup

| # | Claim or assumption | Problem |
|---|---|---|
| G1 | "Confirmed protocol-ordering mechanism" is the lead suspect | It was not shown that **client→provider** holes exist in the Fast5 runs. In a download test that direction is mostly ACKs. A hole there would itself usually be an ACK pack, and the next ACK supersedes it. The mechanism only matters if something creates holes in that direction. That something is most likely provider-side RX refusal (see H1/H2), so the cause should be fixed first. |
| G2 | "More RX permission improved 2/5 → 4/5" read as "RX is not the cause" | It actually supports an **admission-unit mismatch**. The advertisement stayed encoded and unchanged, while physical permission grew 28%, so fewer arrivals were refused. A partial gain from loosening one side of a mismatch is what you'd expect (H1). |
| G3 | Geometric decay is treated as "maybe retention" | A leak or retention usually shows a cliff or linear decline. A roughly constant per-window ratio (~0.4) points to **multiplicative feedback in learned state**: RTT mean, learned send window, pacer, or backoff. The writeup never tests whether the decay survives a state reset (H3). |
| G4 | The 6–9 s request-stall band is not explained | The resend backoff is `min(scaledRtt<<(n-1), 8 s)` (`transfer.go:9086-9106`, `MaxResendInterval 8s` at `transfer.go:914`). Starting at 0.3 s gives cumulative waits of 0.3, 0.9, 2.1, 4.5, 9.3 s. Four or five consecutive refused retransmits of one pack land exactly in 6–9 s. The band itself is evidence: the stalled pack's retries were refused or lost repeatedly. Normal TCP loss on a lossless carrier would not do this. |
| G5 | "Configured TX/RX totals did not change" | Totals can stay the same while **effective** capacity changes a lot. Under exact accounting each TCP pack is charged `320 + root(≥256+meta) + sizeof(TcpSendItem) + 128` plus metadata (`transfer_receive_delivery_dispatch.go:19-56`, `transfer_retained_budget.go:57-64`). For a ~52-byte pure ACK that is about 1–2 KB, roughly 20–40× its wire size [inferred, sizes not measured]. With the same total, the number of small packs in flight can be an order of magnitude lower. |
| G6 | Lanes compared as "0 vs 8" | Each lane is a separate ReceiveSequence that advertises the **whole** shared share: `min(ReceiveQueueMaxByteCount, budget.TotalByteCount())` (`transfer.go:13253-13262`). With 8 lanes × N providers, the total advertised credit is about 8N × the leaf. The code comment admits the advertisement "does not promise retained-cost or shared-sequence credit". Going to 8 lanes therefore multiplied oversubscription by 8 [verified for the formula; whether `Budget()` is the RX leaf or a per-sequence child should be confirmed]. |
| G7 | "Removing leaf ceilings must preserve progress at a full root" (55,296 B counterexample) | The concern is valid. But the counterexample adds escrow and promise machinery to make SACK-before-funding safe. The simpler rule the writeup already mentions ("refuse before SACK if repair cannot be funded") should be adopted as the rule, not kept as a fallback (§4.3). |
| G8 | Budgeted-provider reserve candidate | This adds a fourth mechanism (first-DATA reserve) for what looks like the same root cause: retained TX pinned by SACK-leased or unacked originals. `SelectiveAckTimeout` is 60 s and SACKed items are skipped by resend (`transfer.go:18176-18183`, `942`). Retire it unless it fixes something that H1 and H2 do not. |
| G9 | Evidence granularity | 500 ms RX sampling, cumulative counters, and no join by (provider, client, lane, sequenceId, generation) means there is not enough data to discriminate. §5 lists the exact snapshot needed. |

---

## 3. Root-cause hypotheses, ranked

### H1 (most likely): credit/admission mismatch causes self-inflicted holes on a lossless carrier, which become backoff stalls

**Mechanism:**
1. Senders size flight from the encoded, per-sequence advertisement (`transfer.go:13253`).
2. The receiver admits in retained bytes from a leaf shared by 8 lanes × N providers. The head itself can be refused: `prepareReceiveDeliveryCredit` returns false and the pack is dropped (`transfer.go:17989-17993`, `transfer_receive_delivery_dispatch.go:46-50`). So can `prepareReceiveDelivery` (`transfer.go:17998-18003`). Future items can be refused at `transfer.go:18138` and `18275-18283`.
3. Each refusal is a hole.
4. Holes fill the hold, which causes more refusals on *other* lanes that share the leaf.
5. Refused retransmits back off exponentially, producing the 6–9 s stalls.

It fits:
- the RX leaf being nearly full (92–348 B free) while the root was mostly empty,
- the partial improvement from more physical RX,
- the regression appearing when exact accounting and 8 lanes were turned on.

**Cheap discriminating experiment (one run each, about 10 minutes):**
- Snapshot these `TransferStats` counters at each window boundary on **both** client and provider: `ReceiveQueueDropCount`, `ReceiveQueueEvictionCount`, `ReceiveQueueTentativeEvictionCount`, `ReceiveQueueCommitShortfallCount` (`transfer.go:1798-1834`, `2631-2637`).
- **Prediction if H1 holds:** drops are nonzero and grow with the provider resend counter, and window 1 of the reference config shows about zero drops.
- **Falsifier:** drops stay at 0 in every failing window. Then holes come from somewhere else (relay or route change), so go to H4.
- **Second arm:** set `AdvertisedReceiveWindowMaxByteCount = RX leaf / (lanes × expected providers)`. This field exists at `transfer.go:13258`. The prediction is that drops go to about 0 and the stalls disappear, at some cost to peak throughput. This is a configuration-only change with no new code.

**Deterministic reproduction (unit level):**
- Use two in-process Clients over a lossless in-memory transport, one RX leaf of 256 KiB, and two lanes.
- Lane A carries a bulk sender. Lane B carries a 2 KiB request/response.
- Do **not** inject any loss.
- Assert `ReceiveQueueDropCount == 0` and that B finishes in under 2 × RTT.
- I expect the current source to fail this with encoded advertisement and exact accounting, and to pass with a per-sequence advertisement equal to leaf/lanes.

The invariant this tests is: **on a lossless carrier, a sender that respects the advertisement must never be refused.** That is the falsifiable invariant the writeup asks for in Q8.

### H2 (likely, and it compounds H1): over-charging small packs, especially pure ACKs, at provider RX blocks inner ACKs, which pins provider TX

The ~20–40× retained charge per pure ACK (G5) is applied whenever `lifetimeBudget` is set. The Desktop128 provider policy enables it, and the reference did not. That means the provider's RX refuses client ACK packs well before the encoded window fills. Each refusal creates the client→provider hole that the writeup's ordering mechanism needs. The NAT return buffer then cannot release, provider TX stays full, and fresh DATA gets no admission. This is the "5.74 MB provider TX" finding.

**Experiment:**
- Same Fast5, with the provider in encoded accounting only (`ReceiveQueueRetainedByteAccounting=false`), lanes and client unchanged.
- On the provider, record `ReceiveQueueDropCount` for packs whose frames are all pure TCP ACKs (one counter, test build only).
- **Prediction:** most provider RX drops are pure-ACK packs, and the encoded arm restores the reference throughput.
- **Falsifier:** pure-ACK drops are near zero in the failing arms.

### H3 (likely cause of the across-window decay): persistent learned sender state ratchets down

These live on every SendSequence and survive between windows because providers and sequences stay alive:
- `rttWindow`: 128-sample mean × 2, 60 s timeout (`transfer.go:924-926`).
- The delivery-sized learned window (`transfer.go:13379-13382`, `13615-13617`).
- The pacer.
- SACK leases: 60 s, skipped by resend.
- Retained originals: `RetainedAckTimeout` of 240 s (`transfer.go:939`).

Window N's stalls feed slower estimates into window N+1. The ~0.4 per-window ratio is the signature of that feedback.

**Experiments (each is one Fast5 run, ordered cheapest first):**
1. **Reset test.** Run the five windows with a fresh client and providers, or a fresh process, before each window. If every window gives about 300 Mbps, the decay is persistent state and not per-window load. This single run tells you which half of the problem you are in.
2. **Localize.** Recreate only the client between windows, then in a second run only the providers. The side whose reset cures the decay holds the state.
3. **`SetWindowSizing(WindowSizingConstant)`.** This is the documented one-call rollback (`transfer.go:788-804`). If the decay disappears, the learned window or pacer is ratcheting.
4. At each boundary, dump `SendSequence.rttWindow.Estimate()` and the `SendWindowEstimate` (Window, LearnedWindow, Reason) per lane. A rising RTT mean or falling LearnedWindow across windows confirms H3.

### H4 (possible): the relay or route changes are a hole source outside the client and provider

On hosted H1 the platform forwarder can drop packs when queues are full, and route or carrier changes void SACKs (`CarrierChangeVoidsSelectiveAck`). If H1's drop counters are zero but resends still grow, holes come from the path.

**Experiment:** count `sendCount > 1` items whose sequence numbers the receiver never saw versus ones it refused. This needs a receiver-side "first seen" bitmap per lane in a test build. If the receiver never saw them, the loss is on the path.

### H5 (less likely as a primary cause): DATA-to-DATA head-of-line blocking on shared lanes

Fast.com opens many parallel connections, so with 8 lanes nearly every lane carries a bulk flow, and small requests hash onto loaded lanes (`transfer_logical_lane.go:14-37`). This adds tail latency only *after* holes exist. Fixing H1 and H2 should mostly remove it.

**Test:** with H1's fix applied, compare small-request p99 latency for 1 lane versus 8 lanes. If they are the same, lanes are not needed (Q6).

---

## 4. Alternative and simpler approaches

### 4.1 Send pure TCP ACKs as `NoAck` instead of building reliable immediate delivery

Change: in `ipPacketTransferAckForRequest` (`ip_remote_multi_client.go:19030`), return `false` for a strictly validated pure ACK.

What counts as a pure ACK:
- `ack && !syn && !fin && !rst && len(payload)==0`,
- not a fragment,
- no SACK-dependent options needed.

Also send those packs without DATA budget. `UnreliableNoAckAdmissionBypassCount` suggests that bypass already exists.

Why this is safe [inferred from code]:
- **No custody.** The pack carries zero bytes.
- **Cumulative.** A later ACK supersedes a lost one. If the last ACK is lost, the provider retransmits after `ReturnResendTimeout` (1 s), or sends a persist probe on a zero window (`ip.go:494-497`). The real client kernel answers that with a fresh ACK.
- **Already order-independent at the provider.** `applyEstablishedPureAck` (`ip.go:4940-4950`), and `ackApplied` prevents double-applying (`ip.go:7211-7213`).
- **Reordering ahead of SYN is already handled.** `applyEstablishedPureAck` returns false before the handshake. Such a pure ACK falls back to the ordinary path, or is dropped harmlessly when NoAck.
- **Contracts and debits still apply.** `receiveNack` requires an open contract (`transfer.go:18467-18485`). NoAck packs that overtake a new contract use the 10% headroom from `ContractFillFraction` (FILLFRACTION.md). Losing one there costs at most one ACK.

Costs:
- A dropped final ACK can cost up to 1 s of provider return stall. Measure it, and lower `ReturnResendTimeout` if needed.
- Batching: grouped sends must not mix a pure ACK with DATA in one NoAck group. Split groups at that boundary.

Old receivers already accept NoAck, so **no wire change and no new decoder compatibility tests** are needed.

**Experiment:** run Fast5 with this one change. If ordering of inner ACKs (writeup §"Confirmed mechanism") matters, this captures all of the benefit of proposal (b) at almost no cost. If nothing changes, proposal (b) would not have helped either.

### 4.2 Replace "one root, no leaves" with "per-sequence credit equal to a partition of the root"

The rule that makes a shared resource safe is the classic one:
- **Sum of advertised credit ≤ capacity.**
- Each live sequence gets a small guaranteed floor that always admits its head. This is the legacy `ReceiveQueueMinByteCount` idea. The writeup notes the current policy has a "zero effective per-sequence floor", which is probably a regression.
- Above the floor, a sequence borrows from the shared pool, and the borrowing is advertised to the sender.

Without credit that is advertised and partitioned, any amount of aggregation still lets senders overrun the receiver, and every overrun becomes a hole on a lossless carrier.

Simplest version: advertise `min(per-seq max, floor + fair share of free pool)` and recompute it on ACK. The sender already obeys the advertisement, so no new protocol is needed.

### 4.3 Full-root gap repair: refuse before SACK

Adopt "never SACK what you cannot also pay to repair" as **the** rule. Drop the escrow, promise, and proof-repair layering.

With per-sequence credit (4.2), the head always has floor credit, so a missing current owner can always be admitted, whatever its size, up to the floor. That holds as long as the floor is at least the maximum accepted message size. The receiver also currently accepts larger shapes than the sender's 32-frame / 16 KiB grouping, so **enforce the sender grouping limit at the receiver**, which bounds the floor. That removes the 30,544-byte case.

### 4.4 Make the 6–9 s stalls impossible regardless of root cause

A receiver refusal is not network loss. Tell the sender:
- Add a NACK, or reuse the existing eviction-notice path (`noteEviction`, `transfer.go:13217`), for "refused for credit".
- On that signal, the sender pauses (follows the credit) **without** incrementing `sendCount` backoff.

That removes the exponential tail from self-inflicted holes.

### 4.5 Simplify accounting

Charge retained cost per **pack**, with one rounded root and one metadata block, not per inner frame times a speculative scratch/rewrite reserve (Q2). Reserve scratch once per sequence worker. Only one decode or rewrite runs at a time per worker, so per-original scratch over-reserves by about the in-flight count. The writeup's "three rounded roots plus graph metadata per original" should become live root plus amortized metadata, with scratch held per worker [inferred; confirm against the decode path].

---

## 5. The missing current-state measurement (Q7)

At every window boundary (request-drained), and also at a 100 ms cadence during the 2 s before each stall, emit one row per **(clientId, peerId, direction, logicalLane, sequenceId, generation)**.

Send side (provider TX and client TX):
- resend-queue bytes, split into encoded and retained,
- number of items, and number of SACK-leased items,
- oldest unacked item: `sequenceNumber`, age, `sendCount`,
- `rttWindow.Estimate()` (mean/min),
- `SendWindowEstimate.{Window, LearnedWindow, Reason}`,
- current advertised window received from the peer.

Receive side:
- `nextSequenceNumber`, held count and bytes, highest held sequence number,
- advertisement sent,
- drops by reason (prepare-credit, prepare-delivery, fits, canQueue, full-hold), as separate counters rather than the one aggregate,
- the leaf and root used at that instant.

The join rule: the provider send row for (lane L, sequence S) pairs with the client receive row for the same (lane L, sequence S). If `oldest unacked seq` on the sender equals `nextSequenceNumber` on the receiver and the receiver's drop counter grew, H1 is confirmed for that stall, with no inference needed.

---

## 6. Answers to the eight questions, briefly

1. **Smallest separating experiment:** the H1 drop counters plus the H3 reset test. That is two Fast5 runs. Repeated-window decay after RX drains is most likely persistent sender state (H3) or provider TX pinned by leases (H2/G8). The reset-localization run tells you which.
2. **Over-reservation:** yes, likely. See G5 and 4.5. Live per original: one root plus metadata. Future promises: predecessor holds. Shareable: decode/rewrite scratch, held per worker.
3. **Immediate-receipt design:** it might be correct, but it is unnecessary for pure ACKs. Reuse `NoAck` (4.1). For generic DATA, don't offer unordered reliable delivery until a caller actually needs it.
4. **Bounding protocol in-flight metadata:** with NoAck there is no retained in-flight metadata. Bound only the NoAck fast-path write queue (it already drops at its deadline).
5. **Full-root guarantee:** refuse before SACK, plus a per-sequence head floor of at least the maximum accepted pack, with the maximum enforced at the receiver (4.3).
6. **Lanes after the fix:** measure with H5's test. My expectation is that, with credit-correct admission and NoAck ACKs, lane zero plus fair per-flow scheduling on the send side (which `sendPackAdmission` already does) is enough. Already-numbered packs still need ordering inside one lane.
7. **Measurement:** see §5.
8. **Simpler aggregate fix:** invariant *"advertised credit summed over live sequences ≤ admission capacity in the same units; on a lossless carrier, drops = 0"*. Deterministic old-source failure: the 256 KiB-leaf two-lane test in H1. Falsifying perf test: Fast5 with `AdvertisedReceiveWindowMaxByteCount` partitioned. If drops go to 0 but the stalls and decay remain, my explanation is wrong.

---

## 7. Prioritized next steps

1. **(About 1 hour, config only)** Fast5 with drop-reason counters snapshotted per window on both sides (§5 minimum subset). This decides H1 against H4.
2. **(About 1 hour)** Reset test, then client-only or provider-only reset. This decides whether the decay is persistent state (H3) and which side holds it.
3. **(About 1 hour)** Restore the reference config inside current source: encoded accounting, lane 0, the old construction path. If it restores 300–400 Mbps × 5, it is a config regression, so flip one factor at a time (accounting first, then lanes). If it does not, `git bisect` source with this fixed harness between the 2026-11-08 reference and HEAD.
4. **(Small code)** Pure TCP ACK as `NoAck` (4.1), behind a flag, with one Fast5 A/B run. Freeze the Pack-field-14 immediate-delivery work until this result is in.
5. **(Small code)** Partitioned advertisement (4.2) and the lossless zero-drop deterministic test (H1). Make "drops == 0 on a lossless carrier" a permanent regression test.
6. **(If needed)** A credit-refusal signal that does not trigger backoff (4.4).
7. **Defer or retire:** root-only admission without per-sequence bounds, the first-DATA reserve, and immediate-delivery receipts and identity. Revisit them only if steps 1–5 leave a measured residual.

A meta-observation: the project has accumulated many interacting mechanisms (holds, promises, escrow, SACK leases, eviction notices, learned windows, pacers, lane floors, reserves). Several recent fixes add a mechanism to protect another one. Before adding the next one, get the cheap discriminating runs in steps 1–3. They are likely to show that one unit mismatch (encoded credit against retained admission, multiplied by lanes × providers) is driving most of the symptoms.
