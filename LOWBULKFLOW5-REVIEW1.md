# Low bulk throughput: independent review of LOWBULKFLOW5

Date: 2026-11-05. Reviewer: Claude (Opus 5.5). Reviews [LOWBULKFLOW5.md](LOWBULKFLOW5.md); follows [LOWBULKFLOW4-REVIEW1.md](LOWBULKFLOW4-REVIEW1.md).

**Goal this review is aimed at:** sustained hosted-H1 downloads above 100 Mbps across repeated runs, with responsive browsing, inside the 32/64 MiB Go gates.

**Inputs**

- The brief.
- Source, unmodified: Connect `4e625240`, plus the tests and Android harness in the sibling repositories.
- Primary evidence under the brief's prefixes. I re-derived the tables below from raw files rather than from the brief's summaries:
  - Both cells' `fast-{1,2,3}.json`, `telemetry.ndjson` (per-second interface counters), `workloads.json` and `workload-assessment.json`.
  - Teardown diagnostics `stdout.log` (5-second per-window, per-destination rows; 3,928 rows on Android64).
  - Teardown memory primitives (15-second rows).
  - `SOURCE-MAP.md`, the SDK constructor receipt (`1f7d0a78`, matches the brief), `THREE-PAIRS.json`, the four-cell `COMPACT-READBACK.json`, `MINIMAL-FAILURE-HANDOFF.json`.
  - RX candidate `POLICY.diff`, `PLAN.json` and `BENCHMARK.diff`.
  - Publication `REPORT.txt`, full-window `V2-ONLY.diff`.
- No runs, builds or tests. Every number is read from that evidence or source, or derived by the arithmetic in Appendix A.

**Evidence tags**

- **[E]** Read from retained evidence.
- **[C]** Read from source.
- **[A]** Arithmetic on [E]/[C] values.
- **[H]** Hypothesis, not yet tested.

**Exit labels.** Destination IDs are not reproduced. Labels are per cell and are not comparable across cells, because the brief confirms the two cells share no identities.

| Label | Cell | Window | Role in the data |
| --- | --- | --- | --- |
| 32-S | iOS32 | speed | Carries traffic through all three runs |
| 32-Q | iOS32 | quality | Carries bulk in Fast1 and briefly in Fast2 |
| 64-S | Android64 | speed | Sole bulk carrier for the first 14 s |
| 64-Qa | Android64 | quality | Bulk from about t=15 s |
| 64-Qb | Android64 | quality | Bulk from about t=20 s; large packets |
| 64-Qc | Android64 | quality | Carries reverse traffic during the Fast2 tail |

Time `t` is seconds from the start of that cell's Fast1 child.

**Constraints honoured:** no credentials, deployment, device or network operations; no identities; the only repository change is this file.

---

## 0. Summary

### 0.1 Answer

The three problems in the brief are closer to one chain than three, and two mechanisms that drive the phone numbers are not in the brief:

1. **Which exit carries the flows explains the 52 → 41 → 18 sequence better than anything else in the data** (§1). 32-S plateaus at about 21 Mbps. The run reads high only while 32-Q also carries bulk. Android64's first 14 s show the same plateau on a different exit. Neither plateau touches the receive leaf.
2. **On Android64 the RX leaf did saturate, and what followed was a collapse, not a cap** (§2). Tun delivery peaked at 80.5 Mbps at t=19.4, then fell to a mean of about 6 Mbps for 20 s. Wi-Fi kept receiving at about 20 Mbps throughout, and 69 MB of Fast1's 181 MB of Wi-Fi bytes is unexplained by framing. Three things produce this together: one shared leaf with no per-sequence floor, an advertisement that promises the whole leaf to every sequence in the wrong unit, and refused Packs that wait out per-item backoff.
3. **The tail of a finished speed test outlives the test by about 40 s and overlaps the next one** (§3). RX was pinned at its cap at t=24.5, 39.5, 54.5 and 69.5, and Fast1 ended at t=43.4. All of the brief's 10,016 "Fast2" drops are this tail, or Fast1 itself. So "Fast2 followed a successfully cleaned Fast1" clears the browser, not the tunnel.

A larger RX leaf would help reorder absorption somewhat. As specified it does not address any of these three, and it may lengthen the tail (§6).

**Reaching 100 Mbps.** The phone has already shown 117 Mbps on Wi-Fi and 80 Mbps on the tun, in one sample each, with three exits active. The shortest path looks like: stop the collapse, drain the tail, then spread new flows over more than one healthy exit. A single exit above 100 Mbps is a separate and harder target, and the deployed builds probably cannot reach it (§4).

### 0.2 Findings

| # | Finding | Class |
| --- | --- | --- |
| 1 | Fast results follow exit mix: 32-S alone gives about 21 Mbps with zero drops. The 18 Mbps run was drop-free | [E] |
| 2 | RX pressure cannot explain the 18 Mbps run. The brief says that run has no RX sample, but the 5-second rows bound RX at or below 1.44 MB of 2.18 MB | [E]/[A] |
| 3 | Two packet-size modes are two exits, and the large mode exceeds today's `DefaultMtu`. `DefaultMtu` was 1440 until 2026-08-19, so 64-Qb is probably an older build | [E]/[C]/[H] |
| 4 | Dropped Packs average one tun packet in size on both exits, so deployed Packs look like one packet each. The synthetic three- and eight-frame gains would not apply to these exits | [E]/[C] |
| 5 | The RX leaf is shared across windows and has no per-sequence floor. 64-Qb had 12,325 drops but only 1,257 holds. The brief's speed-only totals omit 14,458 quality-window drops | [E]/[C] |
| 6 | The advertisement is per sequence, in message bytes, and equals the whole leaf; the leaf charges retained bytes. Twelve windows can each be promised the same 4.36 MB, of which about 0.65 MB of 64-S data fits | [C]/[A] |
| 7 | Refused Packs get no feedback, and gap repair retries an item once. This is the likely source of the zero-progress 5-second buckets | [C]/[E]/[H] |
| 8 | Dead flows keep consuming the leaf: RX stayed full for at least 26 s after Fast1 ended | [E] |
| 9 | Replies to dead flows leave on a different exit: from t=55 s the 1:1 reverse packets went out on 64-Qc while data committed on 64-S. Source shows how | [E]/[C]/[H] |
| 10 | Go memory rose about 1.7–2.0 bytes per RX byte, so the brief's 29.79 MiB estimate for the RX candidate is low: about 31.1–31.7 MiB | [A]/[H] |
| 11 | The brief's Fast2 and Fast3 bracket totals double-count or mislabel (§9) | [E] |
| 12 | Fast3 looks like a page that loaded and got no working speed target, not a throughput failure. CDP can report the status class without any packet inference | [E]/[H] |
| 13 | A lingering Fast2 target cannot have competed for bandwidth: the tun was almost silent before Fast3 began. It may still hold browser state | [E]/[H] |

### 0.3 Next steps, in order

1. **Add an idle-gap arm to the device workload** (§3.4): the same three runs with a quiet gate between them. It changes only the harness, and it directly tests whether the tail causes the Fast2 failure.
2. **Add client-side counters** (§8.1): per-window received Packs, bytes and frames; held bytes; drop reason at each of the three drop sites. No provider change is needed.
3. **Log browser request categories through CDP** (§8.3): page, API or speed target; status class; failure text; connect timing. This separates Fast2 from Fast3.
4. **Write the two deterministic tests that should fail today**: shared-leaf starvation (§2.5) and refusal tail (§2.6).
5. **Write the retired-flow reply test** (§3.3).
6. **Survey deployed exits' packet and Pack size classes** (§4.3) from counters you already collect.
7. **Run the RX candidate only after 2 and 4**, with the changes in §6.

---

## 1. What the fresh phone runs show

### 1.1 The displayed number tracks the exit mix

Per-child means from `telemetry.ndjson` **[A]**. The Wi-Fi/tun ratio should be about 1.10–1.12 from framing alone (review 4).

| Run | Display | Elapsed | Tun mean | Wi-Fi rx | Tun rx | Wi-Fi/tun | Bytes beyond framing |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| iOS32 Fast1 | 52 | 15.2 s | 38.8 Mbps | 89.8 MB | 73.7 MB | 1.22 | 7.2 MB |
| iOS32 Fast2 | 41 | 9.6 s | 28.5 Mbps | 42.0 MB | 34.2 MB | 1.23 | 3.7 MB |
| iOS32 Fast3 | 18 | 9.6 s | 17.0 Mbps | 25.9 MB | 20.4 MB | 1.27 | 3.1 MB |
| Android64 Fast1 | 46 | 43.4 s | 18.4 Mbps | 180.9 MB | 99.6 MB | 1.82 | 69.4 MB |

Reverse frames approximate which exit is carrying flows, because each data flow's inner ACKs leave on its own exit. This is an inference: the frame ratio is not a byte split. iOS32 in 5-second buckets **[E]**:

| Bucket end (t) | Run | 32-S frames | 32-Q frames | 32-S drops | Tun rate in bucket |
| ---: | --- | ---: | ---: | ---: | --- |
| 8.3 | Fast1 | 2,867 | 2,325 | 515 | Ramp to 66 Mbps |
| 13.3 | Fast1 | 1,291 | 9,584 | 1,628 | 58–64 Mbps |
| 18.3 | Fast1/2 | 2,155 | 9,814 | 851 | 56–64 Mbps, then the Fast2 gap |
| 23.3 | Fast2 | 4,166 | 40 | 0 | 21–32 Mbps |
| 28.3 | Fast2/3 | 3,411 | 2,704 | 0 | 43–45 Mbps while 32-Q is active |
| 33.3 | Fast3 | 5,393 | 36 | 0 | 20–24 Mbps |
| 38.3 | Fast3 | 3,469 | under 20 | 0 | 18–19 Mbps, then test end |

The pattern is direct: 32-S alone gives about 21 Mbps. The rate only rises when 32-Q joins. Fast2 displayed 41 because 32-Q joined late in the window, and Fast3 displayed 18 because it never did.

Android64 agrees on a different exit. With 64-S alone from t=3.6 to t=13.6, tun samples ranged 8.8–28.4 Mbps with a mean of about 20, again at about 755 B per packet **[A]**.

So the run-to-run decline is **not evidence of accumulating damage or leak**, at least on iOS32. A rise in the plateau needs one of: better placement of new flows, or a faster S-class exit.

### 1.2 The S plateau is not an RX problem

- 32-S logged zero receive drops in every bucket after t=18.3 **[E]**.
- The brief says the third run has no primitive RX-occupancy sample. That is true of the 15-second primitives, but the 5-second `memory_device_transfer` rows do cover it. `client_transfer_used_bytes` includes send and a 1,048,576 B base, so it gives an upper bound. At t=28.3 and t=33.3 it read 1,345,420 and 2,485,868 B, which bounds RX at those two instants at no more than about 1.44 MB of 2.18 MB **[A]**.
- The primitive at t=23.1 read 581,120 B and the one at t=38.1 read 0 **[E]**.

The brief's first mechanism, a saturated receive leaf, therefore cannot be why the third run was slow. Its own remark that the final low-speed intervals had no new RX drops is correct and deserves more weight: a leaf doubling cannot lift this plateau.

### 1.3 What the plateau could be

Measured facts: about 3,000–4,000 tun packets per second at about 750 B, flat for 6 s or more, on two unrelated exits **[E]**. About 40–45% of the Packs were held out of order (7,453 held of about 18,200 packets on 32-S, 7,731 of about 17,650 on 64-S) **[A]**.

The candidates are not separable with today's data:

| Candidate | Prediction | Discriminant |
| --- | --- | --- |
| Per-Pack charge on a mobile provider | Rate scales with mean Pack bytes at fixed RTT | Compare same-exit rate at different Pack sizes (§4.2) |
| Message-byte flight on a desktop provider | Rate independent of Pack size, inverse in RTT | RTT sweep |
| Inner flow window or inner RTT inflated by reordering | Rate tracks advertised inner window ÷ RTT | Tun header telemetry from review 4 §9.1 |
| Per-Pack service limit | Packets per second constant across RTT | RTT sweep |

One arithmetic coincidence is worth testing, not believing. A mobile64 send leaf of 2,876,094 B at 8,228 B per small Pack holds about 349 Packs. At 756 B each and a 100 ms RTT that is 21.1 Mbps **[A/H]**, which is the observed plateau. The exit's provider type and RTT are both unknown, so this is a prediction only. If it holds, packing is worth far more than review 4 assumed (§4.2).

---

## 2. Android64: saturation followed by collapse

### 2.1 Timeline

**[E]** from telemetry, 5-second rows and 15-second primitives:

| t (s) | Tun | Wi-Fi | Packet mean | State |
| --- | --- | --- | ---: | --- |
| 3.6–13.6 | 9–28 Mbps | 11–49 Mbps | 755 B | 64-S alone; no drops; RX 0.43 MB at t=9.5 |
| 15–23 | 14–80 Mbps, mostly above 30 | 50–117 Mbps | 880–1,430 B | 64-Qa, then 64-Qb join; drops begin in the bucket ending 19.8 |
| 24.5 | — | — | — | RX 4,360,880 / 4,362,076 B |
| 24–43 | 0–19 Mbps, in about 2-second bursts | 3–40 Mbps | up to 1,434 B | Collapse |
| 43.4 | — | — | — | Fast1 child ends |
| 44–83 | 0–6 Mbps | 0–14 Mbps | — | Tail, inside Fast2 |
| 84.5 | — | — | — | RX 0 |
| 85–133 | About 0 | About 0 | — | Silent; Fast2 still waiting |

The Wi-Fi peak of 116.8 Mbps and tun peak of 80.5 Mbps occurred in the same 0.9-second sample (t=19.4) **[A]**.

During the collapse 64-S had buckets with **zero commits and zero ACK writes** while drops continued: t=29.8 (2,109 drops), t=54.8 (2,271), t=69.8 (470) **[E]**.

### 2.2 The leaf is shared and one sequence can starve another

The source (`transfer.go:17198`, `:17317`) refuses a held arrival when the shared budget has no room. A per-sequence floor field exists (`ReceiveQueueMinByteCount`, default 320 KiB, `transfer.go:1105`), but the mobile policy forces it to zero (`sdk/mobile_memory_policy.go:230`, `:429`), and logical lanes zero it too (`transfer.go:15584`) **[C]**. So on a phone no sequence has any guaranteed hold.

The data shows this happening **[E]**:

| Exit | Holds (cell total) | Drops | Mean dropped Pack |
| --- | ---: | ---: | ---: |
| 64-S | 28,652 | 25,929 | 759 B |
| 64-Qb | 1,257 | 12,325 | 1,425 B |

From t=25 to t=45, 64-Qb had thousands of drops and almost no holds: 3,595 drops with 0 holds in the bucket ending 29.8, and 2,717 with 0 in the next. An exit that can hold almost nothing has every arrival after its first gap refused. That is the signature of the leaf being held by someone else **[H]**, and 64-S is the only exit with a large hold history. Per-window held bytes would confirm it (§8.1).

iOS32 shows the same asymmetry at a smaller scale, the other way round: while 32-Q ran at high rate, 32-S took all 2,994 drops and 32-Q took none **[E]**. So the drop victim is whichever exit is reordering at the moment the leaf fills.

### 2.3 The advertisement promises what the leaf cannot keep

`receiveWindowAdvertisement` returns `min(ReceiveQueueMaxByteCount, budget.TotalByteCount())` (`transfer.go:12446`) **[C]**. Two consequences:

- **It is not a share.** All 12 windows can each be told the full leaf.
- **It is in the wrong unit.** The sender counts message bytes. With `ReceiveQueueRetainedByteAccounting` the leaf is charged retained bytes (`transfer.go:1419`), 5,120 B for a one-packet Pack of about 760 B: a 2,048 B carrier root, a 2,048 B frame root and a 1,024 B owner. (Review 4's 1.8–3.6 KB was too low for this size; the second review's scratch test measured 5,120 B, and the source components agree.) A 4.36 MB leaf then holds about 852 Packs, or 0.65 MB of 759 B Packs **[A]**.

The comment above that function says a sender keeping to the advertisement "can never force an eviction". That holds only for one sender, in matching units. Neither condition holds on a phone. A sender that honours the advertisement can overrun the receiver by a factor of about 6.7 before any second window is counted.

Exits built before window sizing may not read the advertisement at all **[H]**, which is the brief's provider question 3.

### 2.4 Refused Packs have no fast way back

**[C]**

- A refused Pack gets no ACK and no notice (`transfer.go:17317`).
- A true head is not refused at this stage on the phone: the prepaid check only classifies `ToProvider` TCP (`transfer_receive_delivery_dispatch.go:19`), and an in-order head goes straight to `receiveHead`. So a stall means the head has not reached this code. It may be late at the sender, or delayed before this point: `platformH1ReceiveBackpressureCount` rose from 392 to 6,113 between t=9.5 and t=84.5 **[E]**, and the brief does not mention it. Whether H1 read backpressure delays a head behind refused Packs is an open question.
- Gap repair needs three later selective ACKs and allows "one immediate retransmit per item" (`transfer.go:923` region). Later losses fall to ordinary timeout.
- Ordinary timeout backs off per item, up to `MaxResendInterval` of 8 s (the defaults at HEAD; deployed senders may differ, see §4.1).
- A refused Pack earns the sender no selective ACK, and refused Packs are exactly the ones that sit above a full hold.

The expected shape after a drain is therefore: the head arrives, the hold drains up to the first refused item, and that item is now a new head that may be on a multi-second timer. Meanwhile every other unacknowledged item is being re-sent into a leaf that is still full. That fits the 2-second tun bursts, the 5-second zero-progress buckets, and Wi-Fi running at several times tun rate **[H]**.

The brief's statement that local timeout resends were zero describes the phone's own sender. The relevant sender here is the provider, and its resends are invisible to the phone except as arrivals. Wi-Fi excess is the only available proxy.

### 2.5 First deterministic test: shared-leaf starvation

- **Setup:** one client with a small shared hold budget, two sequences A and B. Fill A's hold above a missing head. Then give B one missing Pack followed by a burst.
- **Assertions:** B's Packs above its gap are held up to a declared floor; B's drop count stays zero until its floor is used; B's cumulative progress completes within a bounded time once its gap fills.
- **Expected today [H]:** B refused with holds near zero.
- **Smallest intervention:** a hold floor drawn from the existing leaf, granted only to sequences that are actively receiving, in Pack-count units, and charged against A's headroom. Re-enabling the static `ReceiveQueueMinByteCount` is not the fix: 12 windows at 320 KiB would reserve 3.75 MiB of a 4.16 MiB leaf whether or not they are active, which is presumably why mobile zeroes it. Keep the existing tentative-eviction logic inside a sequence, and permit a tentative (never committed) item at the tail of an over-floor sequence to be evicted for an under-floor one.
- **Reject if:** on matched pairs the floor reduces drops without improving tun goodput or tail time, or a head-of-line sequence's own recovery gets slower by more than a declared margin.

### 2.6 Second deterministic test: refusal tail

- **Setup:** one sender, a receiver hold of M Packs, N > M Packs in flight, one early loss. Use a synctest clock.
- **Measure:** time from the first gap to full delivery, total duplicate arrivals, and the longest interval with no cumulative progress.
- **Expected today [H]:** drain time far above N/M × RTT, with intervals near the backoff ceiling. Measure the shape rather than assume it: the recovery code has several interacting timers.
- **Smallest interventions, in order:**
  1. Sender side: when a cumulative ACK advances, reset the timer of the new oldest unacknowledged item to one RTT. The existing `AckTailProbeLimit` path only runs while a selective-gap recovery is active and is limited to two probes.
  2. Receiver side, compatible with older senders: on refusal, rate-limit a repeat of the current cumulative ACK carrying the current effective advertisement. Older senders ignore what they do not read.
- **Reject if:** duplicate arrivals rise or the deterministic drain time does not fall by at least half.
- **Constraint check:** neither change disables Transfer ACKs, adds pacing, or weakens custody. A selective ACK is still only sent for a held, committed item.

### 2.7 Advertisement fix

- **Test:** a two-sender fixture in which both senders honour the advertisement exactly; assert the sum of retained charge never exceeds the leaf and no drops occur.
- **Expected today [H]:** drops.
- **Intervention:** advertise `leaf_available_retained × (observed message bytes / retained bytes per held Pack) ÷ active_receiving_sequences`, floored at a small constant so an idle sequence can start. Update on the existing ACK cadence. This is a smaller number than today's, and older peers keep their existing behaviour.
- **Reject if:** a single-exit run loses more than a declared share of throughput compared with the current whole-leaf advertisement, because it would mean the over-promise was buying useful flight.

---

## 3. The tail after a test

### 3.1 What happens

Fast1 ended at t=43.4 with 20 cancelled requests. RX samples at t=54.5 and t=69.5 were still 4.36 MB **[E]**. 64-S went on committing until the bucket ending t=84.8 and dropping until the one ending t=74.8; 64-Qb was similar **[E]**. Wi-Fi delivered about 25 MB in the Fast2 child, against 5.6 MB on the tun **[E]**.

Transfer is ordered and reliable per sequence, not per flow. A Pack belonging to a closed connection still has to be delivered in order, and once the leaf is full each one drains at the rate §2.4 describes. Nothing on either side discards it **[C]**.

Fast2 navigated at t=43.5 into a pinned leaf. That is a fact, and the brief's "Fast2 followed a successfully cleaned Fast1" does not address it. Whether this caused the stall is not proven, because the tunnel then had 49 quiet seconds in which Fast2 still made no progress (§3.5).

### 3.2 Replies to dead flows go to another exit

During the tail, tun receive and send packet counts are almost equal, about 1:1, at about 60 B per reply **[E]**. Examples: t=46.5: 564 in, 574 out; t=59.4: 827, 838; t=79.5: 552, 548.

Those replies did not leave on the exit delivering the data **[E]**:

| Bucket end | 64-S holds | 64-S reverse frames | 64-Qc reverse frames | 64-Qc bytes per frame |
| ---: | ---: | ---: | ---: | ---: |
| 59.8 | 746 | under 20 | 788 | 64 |
| 64.8 | 992 | under 20 | 849 | 60 |
| 74.8 | 530 | under 20 | 396 | 62 |

Source gives a direct path **[C]**:

1. The browser closes a socket and the kernel sends RST. `observeEgressTcpGroup` reports completion and `retireCompletedTcpFlow` cancels the update (`ip_remote_multi_client.go:8612`, `:5002`).
2. Late data for that path has no flow, and is delivered to the tun anyway: "incoming packets not in response to outgoing packets" (`:10050` region).
3. The kernel answers each segment.
4. `sendUpdate` sees `!ok || update.IsDone()` and creates a new update (`:5316` region), which is placed like any new flow, and can pick a different exit.

This is **[H]** until a header log shows what those 60 B packets are. Ordinary ACKs with options and an IPv6 RST are both plausible at that size. But the routing is observed. It does not move a live flow, so it does not breach the exit-ownership rule, but it does have three costs:

- It opens flow generations for connections that no longer exist.
- It sends unknown-flow control to an exit that owns nothing. 64-Qc is also the only window with notable contract-missing writes (17) and cumulative probes (47) **[E]**; a link between those and this traffic is **[H]**.
- It adds channel and window pressure at the moment a new test needs a fresh path, which links to the brief's second problem **[H]**.

### 3.3 Deterministic test: retired-flow reply

- **Setup:** bind a flow to exit A. Egress RST; assert retirement. Deliver late inbound data from A. Offer a non-SYN TCP reply on that path.
- **Assertions:** no new update; zero writes on any other exit; the reply is either dropped locally or sent to A; a real new SYN on the same 4-tuple still gets a clean generation.
- **Expected today [H]:** a new update and a write to a selected exit.
- **Smallest intervention:** retain a small bounded tombstone of recently retired paths and their former exit for the life of the retained tail. A non-SYN packet that matches is sent to the former exit or dropped; never placed.
- **Adjacent cases:** FIN versus RST retirement, port reuse, a SYN with a new sequence, tombstone overflow, and pinned flows.
- **Reject if:** provider-side close gets slower, or tombstones change any live-flow routing.

### 3.4 Device arm: quiet gate between runs

This is the cheapest test in the review. Run the same three Fast children with a gate between them: wait until RX is zero and Wi-Fi is quiet, or a declared maximum of 90 s.

Keep the back-to-back order as its own arm. A real user pressing Go again is a legitimate workload, and failed arms stay in the record.

| Outcome | Reading |
| --- | --- |
| Gated Fast2 succeeds, back-to-back fails, repeatably | Tail is causal; prioritise §2 and §3 |
| Both fail | Something else; go to Fast2 evidence (§8.3) and connection recovery |
| Both succeed | Failure is rare or exit-specific; collect more cells before fixing |

### 3.5 Reducing the tail itself

A tail exists because in-flight data outlives its flow. Three options, in rising risk:

1. Fix §2.4 so a full leaf drains at link rate. At 4.36 MB and 20 Mbps that is under 2 s, not 40 s **[A]**. This alone may be sufficient.
2. On the provider, when a flow closes, stop reading its origin data and release unsent NAT backlog. Retained Transfer Packs stay.
3. A sequence-preserving tombstone: release the payload of a held or retained Pack for a closed flow while keeping its sequence number and ACK identity. This must be reviewed against the custody rule. It is safe only for Packs where every frame belongs to a retired flow, which one-packet Packs make common. I would not start here.

---

## 4. Provider identity and useful flight

### 4.1 Packet size identifies build era

- `DefaultMtu` is 1100 today (`ip.go:44`) and was 1440 until commit `be7f1602` of 2026-08-19 **[C]**.
- 64-Qb's mean dropped Pack is 1,425 B and its period of dominance shows tun means of up to 1,434 B **[E]**. Both exceed 1,100 and exceed the phone's 1,280 tun MTU.
- The current packetizer builds packets of at most `DefaultMtu` (`ip.go:36–44`) **[C]**. In a limited search I found nothing in the client path that coalesces packets.

So 64-Qb is most likely running a build older than 2026-08-19 **[H]**. The same period's work on Transfer and H1 cannot be assumed there, and the brief's "older provider" question has an answer for at least one exit, without provider-side telemetry.

The small mode has its own signature. A tun mean of 750–756 B, flat across seconds, is what one read of about 1.4 KB gives when it is cut into 1,100 B plus about 410 B **[A/H]**. A flat mean means nearly every read returned one origin segment, which strengthens review 4's reading: this exit's NAT is not backlogged when it reads. Something downstream of the origin socket is not the queue that is full.

### 4.2 Deployed Packs look like one packet

`MessageByteCount` sums a Pack's frames (`transfer.go:19135`) **[C]**. Mean dropped Pack sizes are 762 B (32-S), 759 B (64-S) and 1,425 B (64-Qb) **[E]**. Each equals the tun packet mean on that exit, so those Packs carry about one packet.

`PackHandoffMaxBytes ÷ MaxCount` agrees: 915 B on 64-S and 1,539 B on 64-Qb. 64-Qa's 2,339 B suggests some exits pack a little more **[A]**. This covers dropped and queued Packs only, not all Packs, which is why §8.1 asks for received-frame counters.

Consequences:

- **The controlled +33% from eight-frame batching does not apply to these exits** until they run that code. Three-frame and eight-frame host tables are valid for what they measure, and they measure a different provider from the ones the phone used. Note `host_conditions_matched: false` in `THREE-PAIRS.json`, which the brief already reports honestly.
- **Packing may be worth far more on a mobile exit than the host test suggests.** At one 756 B packet per Pack the retained efficiency is about 0.092 (756 / 8,228), not the 0.218 review 4 used for three-frame packing **[A]**. If §1.3's per-Pack hypothesis holds, S-class exits are limited there.
- **Per-Pack cost is also on the phone.** Each held Pack costs a retained root regardless of size, so small Packs fill the RX leaf fastest.

### 4.3 Exit survey from data you already have

No provider telemetry is required for a first pass. Across existing retained sessions, per window: tun packet mean while that window dominates; mean dropped Pack bytes; `PackHandoffMaxBytes/Count`. Classify each exit: small or large packet era; one-frame or multi-frame.

If most exits are old-era, the provider-side interventions in the brief cannot reach them until they update, and the client-side fixes in §2 become the priority.

### 4.4 Provider intervention (request 3)

Review 4's funded scratch reserve still stands for mobile exits, and its invariants are unchanged. I would add a prior step and change the order:

1. **Before any reserve: make provider build and policy class visible** (§8.2).
2. **Sender timer reset on cumulative progress (§2.6).** It ships on providers alone and benefits every client, including those on old apps. Release semantics do not change: an item leaves only on cumulative ACK or its existing lease. Older peers see nothing new on the wire.
3. **Reserve, for mobile-class exits only**, with review 4's design. Matched RTT/provider process pairs, five consecutive windows, loaded small requests and joined cleanup, as the brief requires.

Rejection for 3: useful flight per retained byte up, and throughput unchanged at matched RTT.

---

## 5. Ranking the mechanisms (request 1)

Ranked by how much of the observed shortfall each explains on current evidence.

| Rank | Mechanism | Evidence | Scope |
| --- | --- | --- | --- |
| 1 | Exit mix and S-class plateau, including provider retained-flight | §1 | Every run; caps iOS32 at about 21 Mbps per S-class exit |
| 2 | RX leaf **collapse**: shared leaf, over-advertisement, refusal tail | §2 | Android64 Fast1; a smaller form in iOS32 Fast1 |
| 3 | Tail outliving the test | §3 | Android64 Fast2 onset; likely any repeat at high rate |
| 4 | Fresh-connection recovery | Two real defects; unproven on phone | Controlled outage; possibly Fast2 and Fast3 |
| 5 | Reverse reliable ACK recovery | Gap writes rise with held Packs on 32-S (81–296 per bucket); no proof of cost | Secondary, unmeasured |

I rank plain "client RX leaf size" below all of these: the leaf matters through collapse, not through capacity.

Missing measurement for each pair:

| Pair | Measurement | Separates by |
| --- | --- | --- |
| Provider flight vs RX leaf | Per-window held bytes and drop reason, with tun rate | Plateau with low RX is provider; collapse with RX full is leaf (already seen once each) |
| Provider flight vs reverse ACK | Inner RTT from tun timestamps, per flow | Inflated inner RTT with idle provider points to reverse path |
| RX leaf vs reverse ACK | Reverse head-gap age alongside RX occupancy | Reverse gaps without RX pressure, or the reverse |
| RX leaf vs fresh connection | Per-request connect timing from CDP while RX is pinned | Connect delayed only while RX is full, or also after it clears |
| Provider flight vs fresh connection | Whether the new flow got a SYN-ACK at all, per exit | Failure before data is recovery; after, is flight |
| Reverse ACK vs fresh connection | First-packet wait on a fresh flow on an exit with no gaps | A delay without gaps rules out the reverse path |

---

## 6. The RX candidate (request 2)

**What the patch does.** `POLICY.diff` raises the leaf to 4 and 8 MiB for provider-off, parented, target 32 or 64 **[C]**. The patch is small, and the source-only plan is careful: arm order, loaded-latency requirement, constructor receipts and wire controls are all good.

**What it will not show.** The fixture is one fixed provider, 40 ms, 2 Gbps, 0 and 0.5% loss, no second sender and no back-to-back test **[E]** (`PLAN.json`). None of the three phone mechanisms exists in it: no multi-exit leaf sharing, no multi-carrier reordering at the observed 40–45% rate, no cancellation tail. A pass is therefore not evidence about the phone.

**Risks.**

- **Longer tail.** A leaf twice as large stores twice the dead data. Unless §2.4 is fixed first, the tail could lengthen **[H]**.
- **Larger overrun.** If the advertisement grows with the leaf, the unit mismatch grows with it **[C]**.
- **Memory.** On Android64 Go runtime rose 7.91 MB as RX rose 3.93 MB between t=9.5 and t=29.8, or 2.01 per RX byte. On iOS32 it was 1.71 **[A]**. Applied to the extra 1.92 MiB of permission, the estimate becomes 31.1–31.7 MiB, not 29.79 MiB, against a 32 MiB limit. Both are coarse (sampled peaks; other allocation overlaps). They are enough to say the nominal headroom is smaller than the brief suggests, and the Go soft limit may turn that into GC work before any breach shows.

**Telling credit from moved congestion.** Report these together, per window:

| Observable | Real improvement | Moved congestion or garbage |
| --- | --- | --- |
| Tun goodput ÷ Wi-Fi bytes | Rises | Flat or falls |
| Time with RX above 90% | Falls, or rises while goodput rises | Rises, goodput flat |
| Tail time after cancel | Flat or falls | Rises |
| Longest no-commit interval | Falls | Flat |
| Fresh small-request p95 while loaded | Flat or falls | Rises |
| Go runtime per RX byte | Stable | Rises |
| Reverse ACK age | Flat | Rises |

**Changes I recommend.**

1. Run it after the §8.1 counters exist.
2. Add a two-sender, reordering stratum and a cancel-then-new-test stratum. Declare the expected reorder share from the phone data.
3. Add a third arm: same leaf as baseline, with the sender timer fix. If that arm matches the candidate, the extra memory is not needed.
4. Add a device arm at the 32 MiB target, because a shared-host fixture cannot qualify the memory.

**Rejection:** as the brief says, fewer drops without better throughput, tail or latency rejects it. Add: a longer cancel tail rejects it at any throughput.

---

## 7. Recovery fixes (request 4)

I read the publication `REPORT.txt` and the full-window `V2-ONLY.diff`, not all of the 75–96 KB candidate diffs. I did not review them line by line. The checks below are what I would require, not defects found. The separate review directories under `C` and `final-publication-qualified`, which are newer than the brief, were not assessed.

**What is sound.** The two old-code failures are real and exact, and each has a named marker. The design choices (one coalesced event on the existing reaper, bounded batches, one `TryLock` attempt per unit, no new timer or worker) respect the memory and goroutine rules. Catching the send-only winner gap in review is the right outcome, and V2's `transferServiceRefreshBindingWithLock` is placed at that bind.

**Interactions the phone data adds.**

| Case | Why it matters | Required test |
| --- | --- | --- |
| Retained SYN retry while the shared leaf is pinned | A fresh exit's first reply is a head and passes, but its next out-of-order Packs are refused (§2) | Publish a provider during a pinned leaf; assert TLS-sized exchange completes, not just SYN-ACK |
| "Idle failed owner" with a live tail | An owner with no flows may still have held Packs on its sequence | Refresh while the failed channel holds Packs; assert release and no stranded promise credit |
| Retired-flow replies as demand | §3.2 traffic creates updates; it must not count as flow demand, service failure or an offer that claims a slot | Stream non-SYN replies into a full window; assert no reservation or discovery |
| Late RST racing retry | A browser that gave up sends RST as publication arrives | RST between the event and reaper dispatch; assert no SYN after RST, cursor restored |
| New SYN, same 4-tuple, during the retry | Must be a new generation, and the old retained SYN must be dropped | Existing generation check, with the event already queued |
| Closed publisher or removed client mid-batch | Cursor must not reference a retired client | Listed in the report; confirm it is run with the race detector |
| Remote tail aliasing after overflow | The report says overflow refuses automatic continuation. Confirm that means a visible refusal, not silent starvation | Assert a counter and a terminal state |
| Memory-credit exhaustion | Reservation must fail closed with the flow still eligible for ordinary retry | Exhaust the shared 32 MiB root during reservation |
| Fairness | Many stalled destinations must not monopolise the batch | Two destinations, one with many waiters; assert bounded wait for the other |
| Backoff | Publication churn must not bypass per-SYN pacing, or a flapping exit becomes a SYN storm | Repeated publish/withdraw; assert a bound on SYNs per generation |

**Causal link to the phone is open.** I agree with the brief that neither defect is attributed to Fast2. CDP connect timing (§8.3) would decide this.

The 100-page replay is the correct final gate, and the brief is right that a pass of the ordinary 100-page run does not clear the outage scenario.

---

## 8. Diagnostics (request 5)

### 8.1 Client side, first

All bounded, per window, payload-free, added to existing rows:

| Field | Purpose |
| --- | --- |
| Received Pack, frame and message-byte counts | Actual Pack geometry; fixes §4.2's dropped-only sample |
| In-order and held arrival counts | Held share without estimating from tun |
| Held bytes and held Pack count, sampled | Who owns the leaf |
| Drop reason, one counter per site: `delivery`, `window`, `budget` | The brief's "byte-bound or count-bound"; sites are `transfer.go:17076`, `:17198`, `:17317` |
| Duplicate arrival count and bytes | Provider resends as seen by the phone (`peerAudit.resend` exists) |
| Longest no-cumulative-progress interval since last row | Direct measure of stall |
| Effective advertisement sent, last and minimum | Separates configured from effective |
| Time with RX above 90%, as a cumulative duration | Brief's time-at-cap gap; 15-second samples cannot give it |
| Retired-path reply count, by outcome | Tests §3.2 |

### 8.2 Provider side

Bounded row per source/sequence, top N by retained bytes, N small:

| Group | Fields |
| --- | --- |
| Identity | Build commit prefix; policy class (mobile, desktop, miner); `DefaultMtu` in effect; retained accounting on or off; protocol version |
| Attribution | Hashed source and sequence, stable per session, never raw IDs |
| Flight | Useful in-flight bytes and Packs; retained bytes; effective window; peer advertisement; remaining send leaf |
| ACK | Frontier age; last cumulative ACK age; held selective leases |
| NAT | Outstanding origin bytes; inner-ACK age; flows closed with retained bytes |
| Admission | Refusal count by reason: leaf, ancestor, window, count, contract |
| Service | H1 write time p50 and max; write queue depth; origin read size histogram (four buckets) |

Report build and policy class to the client in an existing control exchange if possible: a client that knows an exit's class could make placement decisions, and no telemetry export is then needed for the survey.

### 8.3 Browser side: what encrypted counters cannot tell you

The brief is right that HTTP 403 cannot be inferred from packets. But the harness owns Chrome through CDP, where this is not inference. `Network.responseReceived` gives status and connection timing, and `Network.loadingFailed` gives the error text, without any payload or TLS interception.

The harness already enables the CDP `Network` domain, and its diagnostic record deliberately keeps no URLs (`chrome_fast_benchmark.mjs:84`). Keep that rule: classify each request in memory into a fixed set (`document`, `api`, `speed-target`, `other`) and store only: class, status class (2xx, 3xx, 4xx, 5xx, none), failure category, and connect, TLS, first-byte and finish offsets, in a bounded histogram.

This would answer Fast2 (did the document connect, and when?) and Fast3 directly.

Fast3 today reads as follows **[E]**: 12 requests, 11 completed, 51,115 B, one connection reset, display "0", no bulk traffic, speed sender almost idle. A page that loaded and never got a working speed target is the plain reading **[H]**. That is different from a throughput failure, and a destination refusal at the API layer would look just like this; only the browser can say.

---

## 9. Corrections to the brief

| Item | Brief | Evidence |
| --- | --- | --- |
| Fast2 drops | "stalled Fast2's 95-second bracket added 10,016" | Total speed drops after the 17,876 are 8,329. The 10,016 includes 1,687 from the bucket ending 44.8, already counted in the 17,876 [A]. All were tail, not Fast2 traffic |
| Drop scope | Speed-window totals | Quality windows add 14,458 in the same interval: 40,663 in total [E] |
| Pixel32 851 | Fast2's bracket | The bucket ends at t=18.3 and starts during Fast1 (ended t=15.2). The brief warns brackets overlap; this one should not be read as a Fast2 effect [E] |
| "Third run has no primitive RX-occupancy sample" | True for primitives | 5-second rows bound it at or below 1.44 MB [A] |
| RX sample meaning | Sample at 98.7–99.97% | Accurate; but the Android64 "Fast1" and "stalled Fast2" samples are one continuous pinned period, not two events [E] |
| "Fast2 followed a successfully cleaned Fast1" | Used to exclude a harness confound | True for Chrome. The tunnel had not drained [E] |
| Memory estimate | 29.79 MiB at iOS32 | Permission arithmetic assumes 1:1; observed 1.7–2.0 [A] |
| Fast3 overlap | Possible | Not bandwidth: tun near zero at t=120–133 [E]. Sockets or browser state could still be held [H] |

**Corrections to my own review 4.**

- I wrote that most download Packs arrived out of order, estimating Packs from three-frame packing. If deployed Packs carry about one packet, the share in these runs is about 40–45%. That run's 13,308 may similarly be a minority.
- My ceiling table's HEAD efficiency of 0.218 assumed three frames per Pack. For one-packet Packs of 756 B it is about 0.092.
- I treated 855 B as a single exit behaviour under a 1,100 B MTU. It was more likely a mix of exit modes.
- My recommendation to check provider uplink stands as a separate deployed-path measurement, per the brief's instruction. It is not used as an explanation here, and the 117 Mbps Wi-Fi peak shows the phone's own link was not the limit in these runs.

---

## 10. Test realism (request 6)

| Missing from local tests | Phone evidence | Addition |
| --- | --- | --- |
| Two or more exits sharing one RX leaf | §2.2 | Two-provider fixture, one leaf |
| Mixed provider eras | §4.1 | A provider fixture with 1440 MTU and one-frame Packs |
| Sustained reordering at 40–45% of Packs | §1.3 | Multi-carrier reorder model, not only uniform loss |
| Cancel mid-transfer with immediate next test | §3 | Fast-like: 20 cancels, then new page within 150 ms |
| Back-to-back windows without settle | Harness gap between children is about 130 ms [E] | Keep windows adjacent, cancelling flows under load as Fast.com does |
| Flow placement changes during a test | §1.1 | Assert placement history as output |
| Cold browser, warm tunnel | Cache disabled, tunnel not fresh | Separate cold and warm in results |
| Browser socket limits | Fast3 confound | One real-Chrome test against a local origin |
| Per-Pack rate | §1.3 | Throughput reported with Pack size and count |

Keep the browser-ownership confounder as its own failing regression, keep cold and warm windows separate, and keep failed arms. Lower retransmit counts alone are not success; goodput per Wi-Fi byte and tail time should be first-class results.

**Harness fix.** I agree with the brief. `physical.go` builds the children as a fixed list and the script reports `cleanupComplete=false` when `Target.closeTarget` has no reply (`chrome_fast_benchmark.mjs:367–371`). One addition: when retirement is unknown, stop Fast N+1 with a distinct invalid state, and keep its evidence. Do not force-stop Chrome silently between runs, or you lose the warm-browser case.

---

## 11. Residual risks and limits of this review

- **Two cells, one phone, one time of day.** Findings about exit mix rest on a handful of runs. They are strong descriptions of these runs, and only hypotheses about other runs.
- **Reverse frames are a proxy** for which exit carries flows.
- **5-second rows and 15-second primitives** cannot show the order of events inside a bucket.
- **Pack geometry comes from dropped Packs and handoff peaks**, not from all Packs.
- **No RTT data exists.** Every flight argument is conditional on it.
- **The cause of 64-Qb's large packets is inferred** from a source history. A non-standard provider setting is also possible.
- **The 60 B reverse packets are unclassified.** The routing is observed; their type is not.
- **The Fast2 stall after t=85 is unexplained.** The tail ended and the page still did not progress. Tail may be the trigger, or unrelated. Do not close this on the basis of §3 alone.
- **Recovery candidates were sampled, not fully reviewed.** Newer directories than the brief were not assessed.

---

## Appendix A. Arithmetic

- **Tun mean** = tun rx bytes × 8 ÷ child seconds. Fast1 iOS: 73,738,856 × 8 ÷ 15.221 = 38.8 Mbps. Android: 99,607,028 × 8 ÷ 43.373 = 18.4 Mbps.
- **Bytes beyond framing** = Wi-Fi rx − 1.12 × tun rx. Android Fast1: 180,942,577 − 111,559,871 = 69.4 MB.
- **Drop accounting:** 3,901 + 4,812 + 2,109 + 2,781 + 2,586 + 1,685 = 17,874 on 64-S, plus 2 on the second speed exit = 17,876. All speed drops 25,929 + 276 = 26,205; 26,205 − 17,876 = 8,329; 10,016 − 8,329 = 1,687 = the last bucket's 1,685 + 2. Quality drops −1 < t ≤ 90: 14,458; total 40,663.
- **Mean dropped Pack:** 19,682,750 ÷ 25,929 = 759; 17,558,998 ÷ 12,325 = 1,425; 2,280,692 ÷ 2,994 = 762.
- **Handoff peak means:** 155,632 ÷ 170 = 915; 261,688 ÷ 170 = 1,539; 262,012 ÷ 112 = 2,339.
- **Held share:** 7,453 ÷ (3,086 + 4,025 + 3,391 + 3,832 + 3,876 = 18,210) = 41%; 7,731 ÷ (4,602 + 4,164 + 3,823 + 4,615 + 445 = 17,649) = 44%. Assumes one packet per Pack.
- **RX bound in Fast3:** 2,485,868 − 1,048,576 = 1,437,292 B.
- **Leaf capacity:** 4,362,076 ÷ 5,120 = 852 Packs; × 759 B = 0.65 MB; 4,362,076 ÷ 646,668 = 6.7.
- **Plateau coincidence:** 2,876,094 ÷ 8,228 = 349.5 Packs; × 756 B × 8 ÷ 0.1 s = 21.1 Mbps.
- **Retained efficiency:** 756 ÷ 8,228 = 0.092.
- **Go per RX byte:** Android64 (40,382,496 − 32,469,024) ÷ (4,360,880 − 429,312) = 2.01, using the 5-second Go value at t=9.8 and t=29.8 and RX primitives at t=9.5 and t=24.5. iOS32 (28,356,640 − 24,686,624) ÷ 2,151,680 = 1.71.
- **Memory estimate:** 27.87 + 1.92 × 1.71 = 31.1 MiB; 27.87 + 1.92 × 2.01 = 31.7 MiB.
- **Tail at link rate:** 4.36 MB × 8 ÷ 20 Mbps = 1.7 s.
- **Static floor cost:** 12 × 320 KiB = 3.75 MiB; leaf 4,362,076 B = 4.16 MiB.
