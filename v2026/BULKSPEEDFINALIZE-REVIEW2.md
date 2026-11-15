# BULKSPEEDFINALIZE — independent review 2

Reviewed: `BULKSPEEDFINALIZE.md` (snapshot 2026-11-09), the effective candidate
named by `R/immediate-hold-gap-fix-r1/{PRODUCTION,PERFVAR}.overlay.json`, the
reference tree `R/restore-300mbps-baseline-r1/tree`, canonical `main` (HEAD
`a321ff90`), and the retained measurement artifacts. `R` is
`/Volumes/data/urnetwork/temp/aggregate-flow-r15-r2`.

**What I ran:** nothing that executes product or test code. I diffed the
candidate against the reference and against `main`, read the receive/send paths,
and parsed the retained `stdout`/JSON reports with throwaway scripts. I also read
`uptime`/`sysctl` once. Every number below is recomputed from a retained
artifact (Appendix C lists the fields). I did not read any other review.

Tags: **[code]** verified by reading source; **[data]** recomputed from a retained
artifact; **[inferred]** follows from the two but not directly observed;
**[guess]** speculation.

---

## 0. Bottom line

1. **The decisive A/B already exists and is not in any writeup.** On 2026-11-08
   06:44–07:00 a matched cohort compared two binaries that differ in exactly one
   file, `connect/transfer.go`: R13 (`84415854`, sha `6a8fe012…`, byte-identical
   to the reference tree's file) versus R14 (`a321ff90`, sha `ee8cdca4…`, i.e.
   canonical `main`). R13 passed Fast5 at 254–407 Mbps (32 MiB) and 328–413 Mbps
   (64 MiB) plus full100. R14 failed all four fresh-process attempts: a TCP dial
   that had not returned after 5 s, an asset-handshake deadline, and two 5 s
   bulk stalls, with bulk at 4–10 Mbps where it is reported. The single change
   is commit `d90ac7f4`, the zero-time shared receive pump. **[data]**
2. **The candidate is not the measured foundation.** It carries that zero-time
   pump, plus a private exemption of the per-sequence H1 handoff byte cap, and it
   lost three narrow head-admission fixes when four receive files were restored
   wholesale. None of these is in the writeup's "what changed" list. **[code]**
3. **At 32 MiB the candidate is in stop-and-wait.** Ordinary RX sits at 99.98% of
   its 2,181,038-byte child from about t=1 s. Roughly 63–98% of delivered payload
   arrives through the head-recovery child, one original at a time. The "18 MiB
   of spare root" is not the relevant quantity. **[data]**
4. **The foundation itself collapsed on a phone.** The only device cell I found
   for the R13 build (Galaxy, Android 64 MiB, Wi-Fi, public providers) shows the
   same signature: RX at 4,361,776 of 4,362,076 bytes, thousands of handoff
   drops, Fast.com at 0.17–0.83 Mbps. Restoring the foundation can recover the
   synthetic number on a quiet host; it does not by itself reach 100 Mbps on a
   device. **[data]**
5. **Today's host cannot support a 300 Mbps comparison.** Load average was
   53/46/37 on 14 cores when I checked. GC stop-the-world time per cycle in the
   retained reports is 0.07–0.09 ms on Nov 8 and 0.5–34 ms in today's busy
   windows. **[data]**

What I would do next, in order: replay the two retained R13/R14 binaries
interleaved; re-apply the three dropped head-admission hunks (their tests are
already red); replace "zero-time then destroy" with a bounded lossless wait and
restore a per-sequence share cap; then address the immediate-send count cap and
RX sizing. Details in §8.

---

## 1. My understanding of the problem and the plan

Goal: sustained bulk near the earlier 300+ Mbps synthetic results and 100+ Mbps
on-device Fast.com, with fast overlapping browsing, inside the 32 MiB (iOS
surrogate) and 64 MiB (Android) profiles.

Plan in the writeup: keep the TransferMemoryBudget hierarchy, hold promises,
head/repair accounting and SDK mobile policy from the Nov 8 measured run; remove
first-DATA reservations, the provider accounting/lane override, early carrier
retirement and adaptive advertisement; keep protocol-only budget exclusion
(`NoBudget`), reliable immediate delivery for pure TCP ACKs, exact receipt
history, ACK/FIFO recovery changes, native batching and retained-SYN work.

Status in the writeup: a real hold-gap accounting bug was fixed; 64 MiB stopped
collapsing in one replay; 32 MiB still collapses to 2–12 Mbps with 2,000–3,000
provider timeout resends per window; the exact old binary is also slow today.

I agree the hold-gap fix is a real defect with a clean reproduction. I disagree
with the framing of what remains, for the reasons in §3.

---

## 2. Evidence table

All Fast5, `-test.cpu=2`, one process per row. "Head-rec" is cumulative
`ClientReceiveHeadRecoveryReservedByteCount` at the end of the run. "RX peak" is
the largest sampled ordinary receive use against the child ceiling.

| Run | Mbps per window | pure-ACK refused / offered at client bridge | provider timeout resends / initial writes | Head-rec MB | connects ≥2.9 s | RX peak / ceiling | GC STW ms per cycle |
| --- | --- | --- | --- | ---: | --- | --- | --- |
| Nov 8 measured, 32 | 388/292/361/405/298 | 301 / 160,088 | 56 / 237,097 | 0 | 0/33 | 1,225,184 / 2,181,038 | 0.07 |
| Old binary today, 32 | 46/14/15/18/19 | 41 / 10,540 | 120 / 17,803 | 0 | 0/37 | 641,424 / 2,181,038 | 0.5–34 |
| Candidate pre-fix, 32 | 30/3/3/3/3 | 3,921 / 5,922 | 10,976 / 9,979 | 96.0 | 26/34 | 2,180,816 / 2,181,038 | 0.14–0.33 |
| Candidate post-fix, 32 | 12/2/5/3/2 | 2,339 / 3,672 | 12,525 / 7,549 | 63.8 | 23/34 | 2,180,416 / 2,181,038 | 0.06–2.3 |
| Candidate diagnostic, 32 | 11/6/7/4/3 | 1,860 / 4,535 | 12,133 / 9,136 | 110.1 | 27/38 | 2,179,824 / 2,181,038 | 0.09–0.14 |
| Nov 8 measured, 64 | 358/369/391/304/406 | 412 / 166,744 | 0 / 192,649 | 0 | 0/34 | 1,394,624 / 4,362,076 | 0.08–0.09 |
| Old binary today, 64 | 178/162/181/117/89 | 140 / 62,757 | 24 / 104,993 | 0 | 0/34 | 796,496 / 4,362,076 | 0.7–6.0 |
| Candidate pre-fix, 64 | 170/146/33/3/2 | 42,342 / 47,775 | 8,565 / 54,427 | 51.2 | 17/35 | 4,362,000 / 4,362,076 | 0.18–14 |
| Candidate post-fix, 64 | 92/59/86/90/101 | 52,785 / 57,957 | 0 / 66,270 | 0 | 0/35 | 1,622,256 / 4,362,076 | 0.7–21.7 |

The bridge column counts packets refused in pure-ACK-only batches against all
pure-ACK packets offered, so it slightly understates refusal.

Reading across rows: collapse, a pegged RX child, thousands of timeout resends,
megabytes of head recovery and 3–7 s connects appear together and only together.
Pure-ACK refusal (41–91%) is present in every candidate row, including the two
64 MiB windows that still ran at 146–170 Mbps. **[data]**

---

## 3. Findings

### 3.1 The measured foundation had a lossless receive pump; the candidate does not

The reference client reads each carrier with
`handoffTimeout := …packHandoffTimeout(transportType, carrierReliability)` and
`self.receiveBuffer.Pack(receivePack, handoffTimeout)` (reference
`transfer.go:5144-5156`). For a reliable carrier that is
`ReliablePackHandoffTimeout`, which the SDK mobile policy sets to "wait"
(`sdk/mobile_memory_policy.go:218`, applied at `:427`). The comment directly
above that constant records why:

> A controlled H1 provider proved that a finite handoff timeout merely moves
> synthetic loss downstream: after the carrier route became lossless, the 10-ms
> boundary dropped 24 messages and pinned the 2-MiB reorder budget.

The candidate calls `self.receiveBuffer.Pack(receivePack, 0)` for every carrier
(candidate `transfer.go:5192-5196`), does the same for ACK handoff (`:5009-5015`),
and `ReceiveSequence.Pack` treats a zero timeout as "a failed reservation and a
full channel are both ordinary nonblocking loss" (`:17047-17060`). The paragraph
that still sits a few lines below (`:17073-17078`) describes the behaviour that
was removed: "This turns a short scheduler mismatch into transport backpressure
rather than a synthetic Transfer loss/retransmission cycle." The SDK setting is
still restored and is now inert for the shared pump. **[code]**

This is commit `d90ac7f4` (R14) in canonical `main`. I compared all 421 non-test
Go files of the root `connect` package: `main` HEAD differs from the reference
tree in `transfer.go` only, and that difference is `d90ac7f4`. So **canonical
`main` = measured reference + the zero-time pump, nothing else.** **[code]**

The matched cohort is at
`/Volumes/data/urnetwork/synthetic-smoke-r1/shared-pump-performance-r1/`. Its
`freeze-r1/READY.json` state is `FROZEN_SOURCE_ONLY_R13_VS_R14_LOGICAL_PERFORMANCE`;
the baseline and candidate overlays differ in one target, `connect/transfer.go`
(I compared the two overlay maps and the hashes against `git show`).

| Arm | Result |
| --- | --- |
| R13 baseline | Fast5-32: 254/407/368/349/317 Mbps. Fast5-64: 328/329/364/341/413. full100 GREEN. Head-recovery reservations: 0 bytes in all six arms (both smokes, injected stall, both Fast5, full100). |
| R14 candidate | Smoke prerequisite: page-0 TCP dial did not return in 5 s, fault never armed. 32 MiB: `ASSET_HANDSHAKE_STALL`. 64 MiB: `BULK_STALL gap=5.057s`. 32 MiB repeat: `BULK_STALL gap=5.088s`. Bulk in the last three: 9.3 MB in 7.7 s, 22.4 MB in 20.5 s, 10.3 MB in 19.0 s. Head-recovery reservations in the four attempts: 2.8, 5.4, 17.3 and 8.3 MB. Fast5 and full100 arms `NOT_RUN`. |

Sources: `RESULTS-RUNTIME-R1.json`, `RESULTS-NORMAL-DIAGNOSTIC-R1.json` (state
`CLOSED_THREE_NORMAL_FAILURES_NO_LOGICAL_PERFORMANCE_QUALIFICATION`), and the
per-case `stdout`. **[data]**

The baseline arm ran minutes earlier on the same host, so host load does not
explain the difference. `MEMSTEADY.md` and `tests/PERFVAR-MEASUREMENTS.md` record
R14 as "source-reviewed but unexecuted"; neither mentions this cohort, and
`grep` finds no reference to it in any `SIMPLERISSUE*` or `BULKSPEEDFINALIZE`
writeup. An earlier MEMSTEADY entry (near line 6682) says the next step would
"compare finite/nonblocking waits with short lossless scheduling stalls … before
selecting a policy" and that the zero-wait controls "do not prove zero-wait has
no retransmission cost". That comparison is the missing experiment.

Both binaries are retained at `shared-pump-performance-r1/compile-r1/bin/`
(`baseline.test`, `candidate.test`), so this can be replayed without compiling.

### 3.2 Two more unlisted receive-path deltas

**(a) Per-sequence H1 byte cap is bypassed for paid owners.** Candidate
`transfer.go:16907-16920` introduces `physicalBytePermission` and `:16996` skips
the byte limit when it is set. This identifier exists in neither the reference
nor `main`. In the reference a 32 MiB client's sequence handoff stops at 85 Packs
*or* 174,762 encoded bytes (128 KiB × 32/24), then waits. In the candidate it
stops at 85 Packs only, and never waits. Eighty-five Packs at the 30,544-byte
retained cost of the eight-frame witness is 2.6 MB, more than the whole
2,181,038-byte RX child. **[code]**

The diagnostic shows exactly that: by t=0.5 s one sequence's handoff had reached
`PackHandoffMaxCount=77`, `PackHandoffMaxByteCount=709,055`, and the first 244
refusals had happened. `PackHandoffSaturationCount` stayed 0, so the handoff
limits were never the refusal; the refusals are RX exhaustion at decode.
**[data]** I do not know why that worker was stalled long enough to accumulate
77 Packs.

**(b) Three head-admission fixes were dropped by the file restoration.** The
restored `transfer_receive_memory_head_recovery.go` is the Nov 8 file. The
aggregate tree's version (`R/tree/connect/…`) differs by 105 lines:

- `establishedContractShape` (aggregate file `:60-70`). Its comment reads "Quiet
  sends and promoted retries mark Head … Both need paid storage". The restored
  predicate (`candidate :60-63`) requires
  `!scan.head && !scan.contractIdSet && !scan.contractFrameSet`.
- `takeReceiveFreshOpeningOwnerInSession` and `bindReceiveFreshOpeningWithLock`
  (aggregate file `:91-185`), with the matching call in the decoder and bind in
  `transfer_receive_memory.go`. The candidate still declares and tests
  `receiveFreshOpening` (`transfer.go:5136`, `:19527`) but nothing sets it.
- The ordinary-RX prefix identity from `R/receive-ordinary-prefix-fix-r1/FIX.diff`
  (the "qualified narrower fix" of SIMPLERISSUE3). Candidate line `:128` is the
  old guard `item.memoryBudget != budget`.

These are not part of the rejected memory model. They add no budget and no
reservation; they only decide which arriving head may use the existing recovery
child. **[code]**

The candidate's own full Connect race run has finished and is worse than the
writeup's snapshot: 9,825 pass, 69 fail, 16 skip, **67 failing roots**
(`R/restored-hold-fix-connect-all-go-r2/run/connect-all-go-GO-RESULTS.json`).
Twenty-four are `TestReceiveConsumer*`. I read the output of seven of them; the
messages name this defect:

- `FOREIGN_RX_STRANDED_ESTABLISHED_PROMOTED_COMPACT_HEAD: C=1536 drops=1`
- `FOREIGN_RX_STRANDED_ESTABLISHED_QUIET_COMPACT_HEAD: C=1536 drops=1`
- `compact current Head lost independent physical admission`
- `FOREIGN_RX_STRANDED_NEW_FULL_PROOF_DATA_HEAD … real retry progressed after
  foreign predecessor drained`

**[data]**

### 3.3 What the 32 MiB run is doing

The 0.5 s samples in `RX32-REPORT.json` give this sequence: **[data]**

| t | root used | cumulative refusals | committed holds | note |
| ---: | ---: | ---: | ---: | --- |
| 0.0 s | 0 | 0 | 0 | |
| 0.5 s | 1,064,272 | 244 | 46 | one handoff already at 77 Packs |
| 1.0 s | 2,179,648 | 321 | 46 | RX child full |
| 1.5–6.5 s | 2,173,888–2,180,816 | 670 → 2,207 | 65 (unchanged) | nothing commits for 5 s |
| 49.5 s | 2,161,040 | 12,980 | 741 | never leaves the ceiling |

Root use is essentially ordinary RX use for the whole run (client send is under
60 KB and recovery holds at most one head at a sampled cut). The mechanism, as I
read it:

1. One sequence's handoff backlog consumes the RX child (3.2a).
2. From then on every non-head arrival for every sequence is refused at
   `transfer_receive_memory_decode.go:97-112`. On a reliable carrier each
   refusal destroys data that had already arrived. **[code]**
3. Tails that were admitted are committed and selectively acknowledged, so they
   cannot be evicted. They pin the child. **[inferred]**
4. Progress then needs the exact head of each sequence through the recovery
   child. The sender resends only the lane's oldest outstanding item per timer:
   "one write recovers a dropped batch a position at a time rather than a window
   at a time" (`transfer_recovery_selection.go:262-296`). **[code]**
5. A timeout resend of the oldest item is rewritten as a promoted head
   (`transfer_recovery_selection.go:362-381`). A promoted compact head of a
   sequence with no retained tail of its own fails the restored predicate
   (3.2b) and is refused again. **[code]**

Two numbers confirm the regime. In the pre-fix, post-fix and diagnostic 32 MiB
runs, cumulative head-recovery reservations are 96.0, 63.8 and 110.1 MB, about
2,100–3,600 admissions at 30,544 bytes. Divided by the 2.93 retained-to-encoded
ratio, that is 63%, 69% and 98% of the validated payload (52.0, 31.8 and
38.3 MB). And provider timeout resends (10,976–12,525) exceed initial writes
(7,549–9,979) in all three. **[data]**

So the recovery child looks idle at sampled cuts because it is used serially,
one head at a time, not because it is unused.

Why this also breaks small requests: TCP connect time through the app TUN is
under 0.51 s for all 33 flows on Nov 8 and under 1.7 s for the old binary today.
In the candidate at 32 MiB, 23–27 of 34–38 connects take 2.9 s or more, with
clusters at 3.03–3.05 s and 5.04–5.11 s. That quantisation is SYN
retransmission. **[data]** SYN, SYN-ACK, FIN and RST remain ordered, so either
leg waits behind the holes in its lane. **[inferred]**

### 3.4 The same signature on a phone, with the R13 build

`/Volumes/data/urnetwork/r13-galaxy-cell1-retained-context-r1/CONTEXT.json`
(Galaxy, Android 64 MiB, Wi-Fi, 12 window clients, public providers). MEMSTEADY
says the R13 APKs are bound to pre-R14 source.

| Attempt | Fast.com display | RX used max / capacity | root used max / total | handoff drops | head-rec reserved | Go runtime max |
| --- | --- | --- | --- | ---: | ---: | ---: |
| fast-1 | 830 Kbps, timed out | 4,361,216 / 4,362,076 | 6.68 / 43.62 MB | 3,860 | 0.45 MB | 41.3 MB |
| fast-2 | 170 Kbps, valid | 4,361,776 / 4,362,076 | 7.84 / 43.62 MB | 8,693 | 3.89 MB | 44.3 MB |
| fast-3 | 0, timed out | 4,361,728 / 4,362,076 | 8.19 / 43.62 MB | 14,337 | 2.85 MB | 44.5 MB |

`packHandoffWaitCount` is at most 13 across the cell, so the blocking wait
barely engaged; the drops are decode-admission refusals. **[data]**

This is one failed cell and the artifact itself declines causal attribution. I
read it narrowly: the pegged-RX, drop, head-recovery state is reachable on a
real device without R14, without immediate delivery and without any of the
later work. The blocking pump protects the synthetic fixture (one or two
Desktop128 providers, quiet host) better than it protects a phone with twelve
providers fanning in. The common weak point is that RX exhaustion at decode is a
destructive refusal followed by position-at-a-time recovery.

Two consequences. First, the protocol exclusion is well motivated: the average
refused Pack in that cell carries 1.0–2.6 KB of message bytes, against 9.0 KB in
the synthetic diagnostic, so many of the refused Packs were small ones that had
no business waiting for DATA budget. Second, "return to the measured
foundation" is not a sufficient target for the device goal.

### 3.5 New count caps on the TCP ACK path

`immediateSendLimit: max(1, sequenceBufferSize)` (candidate `transfer.go:7638`)
and `protocolAdmission: newSendPackAdmission(max(1, sequenceBufferSize))`
(`:7642`). `sequenceBufferSize` is the raw handoff depth after lane division
(`:7560`, `transfer_logical_lane.go:42-47`). For the mobile client that is
16 × target/24 = 21 at 32 MiB and 42 at 64 MiB
(`sdk/mobile_memory_policy.go:199`, `:292-299`), divided over eight lanes:
**3 un-acknowledged immediate originals per data lane at 32 MiB, 6 at 64 MiB**
(lane 0 keeps the undivided 21 or 42). A further 3 (or 6) raw protocol Packs may
queue; native ingress always offers with zero wait
(`ip_remote_multi_client.go:7892-7896`) and anything beyond is refused. The gate
is `immediateSendAvailable` (`transfer_immediate_option.go:26-28`, used at
`transfer.go:10469` and `:10487`). **[code]**

In the reference the same pure ACKs were ordinary reliable Packs limited by
resend-queue bytes; `ClientSendByteCount` peaked at 8.86 MB (32 MiB) and
18.69 MB (64 MiB) on Nov 8. So the old path allowed thousands in flight and the
new path allows three per lane. Refusal at the bridge rose from 0.2–0.4% to
41–91%. **[data]**

I cannot show this limits bulk throughput. Pre-fix 64 MiB windows 1–2 ran at 170
and 146 Mbps with 92% of pure ACKs refused, against 178 and 162 for the old
binary the same evening. I expect it matters more for small-request latency and
for TCP burstiness than for steady bulk. **[guess]**

Two design points regardless of the measured effect:

- The cap reuses a handoff-queue depth chosen for memory reasons as a flight
  window. Those are different quantities.
- When full, the path refuses the newest ACK. Pure ACKs are cumulative, so the
  useful policy is the opposite: keep the newest per flow and discard the older
  queued one.

SYN, FIN and RST share the same three protocol slots per lane with no per-flow
key (`transfer.go:7961-7973`), so a bulk flow's ACK stream can refuse a new
flow's SYN on the same lane. **[code]** Whether that contributes to the slow
connects in 3.3 needs per-flow timestamps.

### 3.6 Host and harness

- `uptime` at 19:44 local: load averages 53.17, 46.42, 36.74. The machine is an
  M4 Pro with 14 cores (10 performance, 4 efficiency). **[data]**
- Each report carries `num_gc` and `pause_total_ns` before and after every
  window. Stop-the-world time per GC cycle is 0.07–0.09 ms in all twenty Nov 8
  windows. Today it is 0.5–34 ms for the old binary and 0.7–21.7 ms in the
  candidate's faster 64 MiB windows. A long pause per cycle means the runtime
  could not stop its own threads promptly, which is what OS-level descheduling
  looks like. **[data]** It also depends on how busy the process is, so treat it
  as a covariate, not a host meter.
- The same index was 0.09–0.15 ms at 13:02 today when an R14-lineage aggregate
  reached 306, 231 and 255 Mbps at 64 MiB
  (`aggregate-m3-first-stall-fast64-diagnostic-r5`). **[data]**
- The 32 MiB candidate windows are slow even when the index is near the Nov 8
  value (diagnostic run: 0.09–0.14 ms at 3–11 Mbps). So that collapse is not
  host noise. **[data]**
- The replay is not the Nov 8 harness. From the two `RUN-PERF.sh` files and the
  replay's environment record: the working directory is `R/tree/server` rather
  than the canonical server tree, the binary runs directly rather than under
  `go tool test2json`, and `TMPDIR` is on an attached sparse disk image.
  `test-env.sh` is byte-identical in both trees. I did not establish whether the
  Nov 8 run used the same detached supervisor and lock wrapper.
- The old binary's failure today has its own shape: window 1 starts at 120–144
  Mbps and decays after about 3 s; in window 2 every stream delivers almost
  exactly 2 MiB. It has zero head recovery and 120 timeout resends. That is a
  different failure from the candidate's. **[data]**

Conclusion: at 64 MiB I see no evidence that the candidate is slower than the
old binary on today's host; both are far below Nov 8 and the host is the
simplest explanation. At 32 MiB the candidate has a distinct, product-caused
collapse.

### 3.7 Units and sizing

`receiveWindowAdvertisement` (candidate `transfer.go:13389-13398`) advertises
`min(ReceiveQueueMaxByteCount, pool total)` in encoded bytes, per sequence. The
pool is charged retained bytes and is shared by every sequence. The SDK states
the mismatch outright (`sdk/mobile_memory_policy.go:132-142`, "One unit mismatch
to know about … That is bandwidth after a loss, never occupancy"). **[code]**

Sizes for the 32 MiB profile:

- client share 13/20 × 32 MiB = 21,810,380; RX child = share/10 = 2,181,038
  (`sdk/mobile_memory_policy.go:63-88`).
- 2,181,038 / 30,544 = 71 originals ≈ 0.74 MB of wire data ≈ 20 ms at 300 Mbps.
  The fixture round trip is 40 ms.
- A Desktop128 provider's own retained send limit is 5,752,188 bytes, on the
  order of 150 originals in flight toward this client.

So the hold capacity is about half of one sender's flight and half of one round
trip. While nothing is ever refused, in-order data passes through and RX use
stays low (1.2 MB peak on Nov 8). After the first refusal the child cannot hold
the flight behind the hole, so selective-ACK gap repair has nothing to work
with. That is why the system has two states and little in between.

A strict credit scheme does not rescue this at the current size. Credit bounded
by the pool gives at most (2.18 MB / 2.93) per ACK round trip: about 119–148
Mbps at 40–50 ms, and about 85 Mbps at 70 ms. The 16 KB cap experiment in
SIMPLERISSUE3 was this effect at a smaller window.

### 3.8 Smaller risks noticed while reading

- **Ordered protocol originals have no retained-count bound on the sender.** Only
  immediate originals are counted. An ordered `NoBudget` Pack (SYN, FIN, RST,
  every control frame now sent with `NoBudget()`) bypasses resend capacity
  (`transfer.go:10486-10490`) and charges zero (`:15424`, `:15436`). The
  `protocolAdmission` slot is released at serialisation. Toward a peer that
  stops acknowledging, they accumulate until `AckTimeout` (60 s). The setting
  comment at `:983` acknowledges "NoBudget retained cardinality". The workload
  offers 2,500–8,300 SYN/FIN/RST packets per window after the first. **[code]**
  Not measured; needs a bounded test before any 50 MiB claim.
- **Protocol delivery receipts are uncounted on the receiver**
  (`transfer_receive_delivery.go:143`). Same class of question.
- **Dense-position assumptions remain** beyond the two call sites fixed: see Q3.
- **`TestClientReceivePackHandoffDoesNotBlockUnrelatedSource` fails** with zero
  drops recorded where it expects one. I suspect its payload now classifies as
  protocol and takes `packProtocol`, which has a different capacity. **[guess]**

---

## 4. Agreement and disagreement with the writeup

| Writeup statement | My view |
| --- | --- |
| Hold-gap accounting bug is real and fixed | Agree. `heldDataMissingBelow` is correct for the two prefix paths. |
| "This has not solved the 32 MiB bulk collapse" | Agree. |
| 64 MiB resends went from thousands to zero after the fix | The fix is right; the improvement is not established. The collapse is a state the run either enters or does not. Earlier aggregates show both outcomes at 64 MiB. |
| "Restored reference mechanisms include … receive hold promises, head/repair accounting … SDK mobile/provider policies" | Partly. The files are restored; the pump they were measured with is not, a byte cap is bypassed, and three head-admission fixes are gone (3.1, 3.2). |
| "Sampled peak total root use 2,239,368 … substantial parent capacity unused" | The binding limit is the 2,181,038-byte child, at 99.98%. Parent headroom is not reachable by ordinary DATA. |
| "most recovery capacity also unused at sampled cuts" | It is the main delivery path, used one head at a time (3.3). |
| "new product code cannot explain the entire historical gap" | Agree for 64 MiB. At 32 MiB the candidate is several times below the old binary on the same evening (2–5 against 14–19 Mbps in windows 2–5) with a different signature. |
| Retries are "absent in the fresh exact-old-binary control", so suspect retained protocol/FIFO/batching changes | The control differs first in the pump. The R13/R14 cohort reproduces the stalls with none of the protocol work present. The FIFO change probably raises the resend *count* by re-arming each exposed head after one probe interval (`transfer.go:14361-14381`); that is a symptom. |
| Zero saturation and zero decode-pressure counters do not exonerate admission | Agree, and the data says admission is the refusal site. |
| Provisional failing roots: eight events, seven roots | Stale. Final: 69 events, 67 roots. |
| MTU: the label says 1440, the app TUN clamps to 1280 | Agree. Provider bytes per frame are 1,264 on Nov 8 and 1,251 today, so the effective inner MTU did not change. |
| "The opportunistic recovery-prefix proof still has its original 64-position bound" | Agree it is not established as a cause. Do not widen it yet; restore the ordinary-RX identity first. |

---

## 5. Hypotheses, ranked, each with the cheapest test

| # | Hypothesis | Confidence | Cheapest discriminating test | Refuted if |
| --- | --- | --- | --- | --- |
| H1 | The zero-time shared pump converts short receive-side stalls into destroyed data, and the reference accounting cannot absorb that. | High. Single-file cohort already shows it. | Replay `shared-pump-performance-r1/compile-r1/bin/baseline.test` and `candidate.test`, Fast5-32, order A-B-A-B, same harness as the old-binary replay. Compare head-recovery MB, timeout resends, RX peak, connect histogram. No compile. | Both arms show zero head recovery, or both show megabytes. |
| H2 | Dropping `establishedContractShape`, fresh-opening and the ordinary-RX prefix identity strands heads under foreign RX pressure. | High that the defect exists (tests red). Medium on its share of the bulk loss. | Re-apply the three hunks; run the 24 red `TestReceiveConsumer*` roots; then one short 32 MiB run. | Tests stay red for another reason, or head-recovery admissions per timeout resend do not rise. |
| H3 | Bypassing the per-sequence byte cap lets one stalled sequence take the whole RX child. | Medium-high. | Diagnostic overlay removing `!physicalBytePermission &&` at `transfer.go:16996`; watch `PackHandoffMaxByteCount` and time of first refusal. | First refusal still occurs with every handoff under 175 KB. |
| H4 | Three immediate originals per lane throttles the ACK path enough to hurt small requests and add burstiness. | Mechanism certain, impact unknown. | Set the limit to 256 in a diagnostic overlay and export `SendLiveSample.Immediate*` (already implemented in `transfer_send_live.go:173-178`, not captured in any report). Compare pure-ACK refusal, loaded-probe maximum and connect histogram at 64 MiB. | Refusal falls to under 1% and neither latency nor throughput moves. |
| H5 | Slow connects are SYN or SYN-ACK waiting behind lane holes, not SYN refusal at the bridge. | Medium. | Reuse the INITIALSTALL stage collector: bridge accept time, provider SYN receive, SYN-ACK write, client SYN-ACK delivery, for one slow flow. | The first gap is at the bridge. |
| H6 | Host contention explains the old binary's slowdown. | Medium at 64 MiB, unknown at 32. | Rerun the exact old binary once with other suites paused; require GC STW per cycle ≤ 0.15 ms. | It is still far below 300 Mbps with the index at Nov 8 levels. Then look at the harness differences in 3.6. |
| H7 | Stop-and-wait persists because recovery exposes one owner per cumulative advance. | High as a description; untested as a lever. | Deterministic test in §7.4. | Recovery of K dropped originals is already much faster than K probe intervals. |

---

## 6. Answers to the nine questions

**Q1. Where does the 32 MiB path lose progress; which predicate; can the next
head use recovery?**
The first loss is ordinary RX exhaustion at
`transfer_receive_memory_decode.go:97-112`: `takeReceiveMemoryOwner` cannot
reserve, repair and head recovery do not apply, the frame is dropped. The
largest single owner I can identify at that first moment is one sequence's
handoff queue (77 Packs, 709 KB encoded); after that the owners are committed
holds. Both are inferred from maxima and counters, which is why step 4 in §8
asks for a first-refusal snapshot.
The next real head *can* use the recovery child, and usually does: that is how
most payload is being delivered. The cases that cannot are decided at
`transfer_receive_memory_head_recovery.go:60-72`:
- a promoted or quiet compact head, or a mid-stream contract switch, of a
  sequence with no retained tail of its own (`establishedCurrent` excludes
  `scan.head`, `contractIdSet`, `contractFrameSet`);
- the first full-proof Pack of a new sequence (no fresh-opening owner);
- a successor whose predecessor was paid from ordinary RX rather than recovery
  (`:128`).
All three were fixed in your own lineage. The first two are red in your own
full-suite run; I did not find the third's tests in that run's test set.

**Q2. Are advertised credits, retained charges and concurrent sequences in
consistent units? Could fast ACKs drive a flight the RX child cannot sustain?**
No, by a factor of about 2.9 per sequence times the number of active sequences
(3.7); the SDK documents it. The flight toward the client is set by the
provider's transfer window and resend budget, not by TCP ACKs. What the new ACK
path can do is make the origin burstier, because 41–91% of ACKs are discarded
and the survivors arrive in clumps. I have no measurement of that.

**Q3. Is immediate history accounted for everywhere dense positions are
assumed?** No. Places I found, each with a control to write:
- `receiveConsumerRecoveryPrefixWithLock` (`head_recovery.go:107-137`) builds a
  bitmap of consecutive positions from paid consumer owners. A secured immediate
  ordinal inside the span is never such an owner, so the proof fails. Control:
  floor DATA missing, floor+1 immediate and secured, floor+2 DATA under foreign
  pressure; then the same with the receipt pending, and with a foreign
  generation, both of which must still refuse.
- `promise.ingressFloor` is `nextSequenceNumber`, advanced over secured
  immediate ranges only when the worker runs `drainImmediateRanges`
  (`transfer.go:17806`, `:17830`). A DATA head that arrives first is "not the
  floor" for `establishedCurrent`, the backlog test and the pressure request
  (`transfer_receive_memory_pressure.go:186-187`, `:242`). Control: park the
  worker with a test hook, secure an immediate ordinal at the floor, deliver the
  DATA head under pressure.
- Gap expiry: when a range sits at the floor and cannot be drained the worker
  waits with `IdleTimeout`, not a gap deadline (`transfer.go:17830-17843`).
  Control: a pending receipt whose downstream claim never completes.
- Sender terminal ranges are capped at 32
  (`transfer_immediate_send.go:10`, `:104-108`). On saturation the receipt
  falls back to an ordinary SACK and the original keeps its immediate count
  until the cumulative ACK passes. With a limit of three, three such originals
  close the lane's ACK path. Control: 33 disjoint terminal ranges.
- Worker replacement seeds the floor from `history.cumulative`
  (`transfer.go:16170-16178`). Controls: exit with a pending receipt, with a
  secured non-floor range, and with an older and a newer sequence id.

**Q4. Is ACK-only early delivery sufficient? Can SYN, SYN-ACK, FIN or RST sit
behind unrelated DATA holes?** They can, and the connect-time clusters at 3.0 s
and 5.1 s say they do at 32 MiB (3.3). I would not widen immediate delivery to
fix it. If the receiver stops creating holes, an ordered SYN-ACK waits for queue
residence only. If you want something sooner, place a new flow's first packets
on the least-backlogged lane instead of the hashed one; that keeps ordering and
avoids the same-tuple reuse problem.

**Q5. Which retained change explains thousands of retries absent in the old
binary?** None of the listed ones as a first cause. The zero-time pump, the
byte-cap bypass and the dropped head fixes are sufficient, and the first of them
reproduces the stalls alone. See §4 on why the FIFO change inflates the count.

**Q6. What small case distinguishes the causes before another full run?**
- The binary replay in H1. It is the smallest possible experiment.
- A single 3-second 32 MiB window, judged on counters rather than Mbps. The
  signature is complete by t=1 s: refusals, timeout resends, head-recovery
  bytes, RX peak against ceiling, largest handoff, pure-ACK refusal, connect
  histogram. Counters are far less sensitive to host load than throughput.
- Deterministic tests in §7. The first row is already written and red.
- For exact refusal reasons: a fixed array of atomic counters indexed by reason
  at each `recordReceivePackHandoffDrop` site and inside the head-recovery
  predicate (ordinary full and not floor; floor but shape excluded; floor but no
  backlog and no promise; recovery child full; duplicate pending; promise
  closed; no sequence; bind refused; handoff count; handoff quota; mutex busy).
  Add one bounded snapshot at the first refusal per sequence: floor, arrival
  number, bytes in handoff, in hold and in delivery.

**Q7. Can the ceilings be rebalanced inside the unchanged root?** There is a
measured basis to try. On Nov 8 the managed root peaked at 10.08 MB at 32 MiB
(8.86 MB of it client send, mostly un-acknowledged ACK Packs) and 18.69 MB at
64 MiB. With protocol excluded the candidate peaks at 2.3 MB and 1.6–4.4 MB. The
RX child is still share/10. Raising it to roughly 3× (6.5 MB at 32 MiB) keeps
the managed peak below the Nov 8 figure and makes the hold larger than one
provider's flight. The proof has to be physical: device Go-runtime peak and
footprint with the sampler, at that setting, under the stop-and-wait workload
that actually fills it. The one Android 64 MiB cell on record peaked at
41.3–44.5 MB against a 64 MiB limit with RX pegged at 4.36 MB, so there is room
there. I have no 32 MiB device number.
The other half of the same lever is the 2.93 expansion. Early carrier
retirement took 30,544 to 18,256 bytes per original; that is a physical saving,
not a relaxation of accounting, and is worth reintroducing once the pump
question is settled.
Caveat: the uncounted protocol originals in 3.8 must be bounded first, or the
"freed" send budget is not really free.

**Q8. Host or harness effects on the old binary.** See 3.6. Concretely: run
performance under an exclusive host gate, record load average and GC STW per
cycle per window in the report, and refuse to compare a window whose index is
above about 0.15 ms against Nov 8. Check the launch path does not lower
scheduling priority and whether `TMPDIR` on the sparse image is touched during
the measured window.

**Q9. Legacy-provider qualification.** The fixture's `old_provider_fallback` is
a provider-selection scenario, not a protocol-version one, so nothing here
exercises an old peer. Two things to cover. Performance: against a receiver that
ignores `deliver_immediately`, the client's three-per-lane count is recycled
only by cumulative ACKs, so one hole on that lane stops all of its pure ACKs
until the hole is repaired. Correctness: receipts, deduplication and the
terminal-range path with a peer that never sends fields 13/14. The cheapest
honest test is a provider constructed with a "legacy peer" switch that disables
immediate receive, the new ACK fields and receive-side protocol classification,
run through the same Fast and full100 workloads. A cross-process run against a
binary built from the previous release is the stronger follow-up.

---

## 7. Alternatives and simpler designs

### 7.1 State the invariant, then pick the mechanism

For a reliable carrier, a DATA frame that has arrived should not be destroyed
because of a transient local condition. Destroying it turns a stall of
milliseconds into a recovery of at least one resend interval per lost position.
Every bad run in §2 and the phone cell in 3.4 is this conversion.

R14 chose zero wait to stop an indefinitely blocked callback from hiding ACKs.
That is a liveness property, and any finite bound provides it. I would test a
bounded wait before anything larger:

- Wait up to a small bound (start at 25–50 ms, well under the 300 ms resend
  floor) at both refusal points, the decode admission and the handoff, for DATA
  from reliable carriers.
- Skip the wait when the child is held by committed holds, since capacity will
  not appear until a head does.
- Keep a per-sequence share cap in retained bytes (for example a quarter of the
  child) so one sequence cannot take all of it.
- Leave protocol frames at zero wait; they no longer need DATA budget.

The 10 ms result quoted in the SDK dates from 2026-08-25; the head-recovery
child arrived on 2026-11-07. A refused head could not recover then and can now,
so that result is not a reason to skip this. If a bounded wait is not enough,
the structural
version is to carry ACK and protocol frames on a different transport from bulk
DATA and apply read backpressure only to the DATA transports.

### 7.2 Make the hold at least one flight

Rebalance the RX child from the send budget that protocol exclusion freed, and
cut the retained expansion (Q7). With a hold larger than the sender's flight a
single hole no longer overflows the child inside one round trip, and the
existing selective-ACK gap repair can complete.

### 7.3 Do not rely on credit alone

A shared credit in retained units is the right accounting, but at the current
child size its ceiling is below the 300 Mbps target and, at 70 ms, below 100
Mbps (3.7). It needs 7.2 to be useful.

### 7.4 Recover a run of heads, not one

The receiver already admits a contiguous run from the floor through the
recovery child: up to 64 positions, each successor justified by paid
predecessors (`head_recovery.go:107-137`). The sender does not use it; each
cumulative advance exposes one owner. After a timeout-resent head has been
cumulatively acknowledged, which proves the receiver is alive and was refusing,
resend the next K contiguous outstanding originals at once, with K bounded by
the prefix span and a burst limit. That changes the worst case from one original
per probe interval per lane to K per interval, using permissions that already
exist. As a rough check on the diagnosis: one 10 KB original every 65 ms or so
is about 1.3 Mbps per lane, the same order as the observed 2–12 Mbps plateau
over eight lanes. I did not measure the probe interval.
Test: refuse 32 consecutive originals at the receiver; today full delivery takes
about 32 probe intervals; expect 2–3.

### 7.5 Make scheduling jitter a workload parameter

The collapse is triggered by a receive worker falling behind. Add a
deterministic pause injector to the fixture (for example one worker paused 25 ms
every 500 ms, seeded) alongside the existing loss and delay profiles. Then
"tolerates 25 ms worker pauses at target rate without transfer-level loss" is a
repeatable gate, the result no longer depends on what else the host is doing,
and a loaded host stops being the only way to see the phone's regime.

### 7.6 ACK path

Latest-wins per flow at native ingress instead of refuse-newest, and a bound for
immediate originals that is a flight window (per client, in the hundreds) rather
than the handoff depth divided by eight.

### Deterministic tests, with their pre-fix failure

| Test | Fails today because | Passes when |
| --- | --- | --- |
| The 24 red `TestReceiveConsumer*` roots | Restored predicate excludes promoted, compact, contract-switch and fresh-opening heads | The three hunks are re-applied |
| One sequence cannot monopolise RX: block one worker, feed paid H1 Packs until refusal, then offer one Pack for a second sequence | Second sequence refused at decode; first holds ~100% of the child | Per-sequence share cap restored |
| Worker-pause tolerance: callback blocks 20 ms, sender bursts twice the handoff capacity over an in-memory reliable carrier | Drops > 0, sender timeout resends > 0, completion ≥ resend floor | Bounded wait: zero drops, completion ≈ 20 ms. Companion control: callback blocked forever, unrelated source and ACKs still progress within the bound |
| Immediate cap: delay the terminal receipts of three immediate originals on one lane, offer a fourth pure ACK | Refused at the bridge | Latest-wins or a real flight bound |
| Run recovery (7.4) | ~K probe intervals for K refused originals | ~2–3 intervals |
| Ordered protocol growth: receiver silent, offer 10,000 ordered protocol Packs | Retained count grows with no ceiling until `AckTimeout` | A count bound exists and refuses or supersedes |

---

## 8. Prioritised next steps

1. **Replay the retained R13 and R14 binaries, interleaved, Fast5-32** (H1). An
   hour, no compile. If R14 alone shows megabytes of head recovery and R13
   shows none on the same host, the pump is settled and the protocol work is
   exonerated as the first cause.
2. **Record that cohort in MEMSTEADY and PERFVAR-MEASUREMENTS**, and add the
   pegged receive queue to the phone cell's existing entry (it lists memory
   peaks and Fast results but not `receiveQueueUsedBytes`). Later decisions were
   made without either fact in the narrative.
3. **Re-apply the three head-admission hunks to the candidate** and turn the 24
   `TestReceiveConsumer*` roots green. This is aggregate hygiene: the candidate
   currently fails tests written for defects it re-opened.
4. **Add reason counters and the first-refusal snapshot** (Q6), then one
   3-second 32 MiB run. This replaces the remaining inference in 3.3 with
   observation, including why the first worker stalled.
5. **Diagnostic overlays, one variable each, judged on counters:** byte-cap
   bypass removed (H3); bounded wait at handoff and decode (7.1); immediate
   limit raised with `Immediate*` samples exported (H4).
6. **Decide the pump policy** from step 5 and write the worker-pause test (7.5)
   as its gate. Keep R14's liveness tests, relaxed to the bound.
7. **Size the hold:** RX child rebalance and carrier retirement, with a device
   memory proof (Q7). Bound ordered protocol originals first (3.8).
8. **Run recovery** (7.4) so that an entered stop-and-wait state exits quickly.
9. **Triage the other 43 failing roots.** Start with the eight `TestNativeSyn*`
   and four `TestPrivateTcpProfilePressure*` (the one I read reports "full data
   budget admitted another SYN"), which look like direct consequences of
   protocol exclusion and need a decision on intended behaviour, not a test
   edit.
10. **Only then** a quiet-host five-window run, full100 at both profiles, the
    legacy-peer arm (Q9), and devices.

What I would not do yet: more memory-model variants, widening the 64/256 prefix
bounds, broadening immediate delivery to SYN/FIN/RST, or another five-window
run on the host in its current state.

---

## Appendix A. Code map

"cand" is the effective candidate file under the PERFVAR overlay; line numbers
are the donor file's. "ref" is `R/restore-300mbps-baseline-r1/tree/connect`.
"agg" is `R/tree/connect`. "sdk" is `R/restore-300mbps-baseline-r1/tree/sdk`.

| Topic | Location |
| --- | --- |
| Reference pump waits on reliable carriers | ref `transfer.go:5144-5156`, `:4982-4987`, `:1113` |
| Candidate pump is zero-time | cand `transfer.go:5009-5015`, `:5192-5196`, `:16863-16881`, `:17047-17060` |
| Backpressure rationale still in source | cand `transfer.go:17073-17078`; sdk `mobile_memory_policy.go:213-218`, `:427` |
| Byte-cap bypass | cand `transfer.go:16907-16920`, `:16996` |
| Decode admission order and drop | cand `transfer_receive_memory_decode.go:97-112` |
| Head-recovery predicate (restored) | cand `transfer_receive_memory_head_recovery.go:54-72`, prefix `:107-137` |
| Dropped fixes | agg `transfer_receive_memory_head_recovery.go:60-70`, `:91-185`; `R/receive-ordinary-prefix-fix-r1/FIX.diff` |
| Dead fresh-opening references | cand `transfer.go:5136`, `:19527` |
| Sender recovers one position at a time | cand `transfer_recovery_selection.go:262-296`, `:297-327` |
| Timeout resend of the oldest item becomes a head | cand `transfer_recovery_selection.go:362-381` |
| Head re-armed after cumulative progress | cand `transfer.go:14361-14381` |
| Immediate and protocol count caps | cand `transfer.go:7560`, `:7638`, `:7642`, `:7956-8003`, `:10469`, `:10487`; `transfer_immediate_option.go:26-28`; `transfer_logical_lane.go:42-47` |
| Native ingress offers with zero wait | cand `ip_remote_multi_client.go:7892-7896`; `ip_native_ingress.go:11-13` |
| Receiver immediate bounds | cand `transfer_immediate_receive.go:109-110`, `:151-170`, `:372-389` |
| Window advertisement | cand `transfer.go:13389-13398`; sdk `mobile_memory_policy.go:132-142` |
| Budget derivation | sdk `mobile_memory_policy.go:63-88`, `:199`, `:209-212`, `:292-299`; `device_local_transfer_memory.go:65-92` |
| Resend intervals | cand `transfer.go:914`, `:921-922`, `:954`, `:982` |
| Uncounted protocol originals and receipts | cand `transfer.go:10486-10490`, `:15424`, `:15436`; `transfer_receive_delivery.go:143` |

## Appendix B. Artifacts this review adds to the evidence index

- `/Volumes/data/urnetwork/synthetic-smoke-r1/shared-pump-performance-r1/`:
  `RESULTS-RUNTIME-R1.json`, `RESULTS-NORMAL-DIAGNOSTIC-R1.json`,
  `freeze-r1/PRODUCTION-BINDING.json`, `freeze-r1/{BASELINE,CANDIDATE}.overlay.json`,
  `compile-r1/bin/{baseline,candidate}.test`.
- `/Volumes/data/urnetwork/r13-galaxy-cell1-retained-context-r1/CONTEXT.json`.
- `R/restored-hold-fix-connect-all-go-r2/run/connect-all-go-GO-RESULTS.json`.
- `R/receive-ordinary-prefix-fix-r1/FIX.diff`.
- `R/aggregate-m3-first-stall-fast64-diagnostic-r5/run/observed64.stdout` (quiet
  window reference for the GC index).

## Appendix C. How the numbers were derived

From each `[perfvar] synthetic-fast {…}` report line:

- Mbps: `sequence.runs[i].workload.whole_run_mbps`; time series from
  `workload.samples[].validated_bytes`.
- Bridge: `bridge_admission.RefusedPureAckOnlyBatch` and `OfferedByType[1]`;
  per window from `BeforeDetails.Bridge` and `AfterDetails.Bridge`.
- Provider recovery: sum over `provider_recovery[]` of
  `TimeoutResendWriteCount` and `InitialWriteCount`, `After` minus `Before`.
- Head recovery: `After.sdk.device.ClientReceiveHeadRecoveryReservedByteCount`.
- RX peak: `per_run_managed_peak[].ClientReceiveByteCount` minus
  `ClientReceiveHeadRecoveryUsedByteCount`. The ceiling is read from
  `PackQueueCapacityByteCount`, which has the same value as the receive-queue
  child in these profiles (both are share/10).
- Connect time: `flows[].phase.OwnedDialDone.FirstNs` minus
  `OwnedDialStart.FirstNs`.
- GC index: (`pause_total_ns` after − before) / (`num_gc` after − before) from
  `BeforeDetails.Harness` and `AfterDetails.Harness`.
- 32 MiB timeline: the newline-delimited parts inside
  `consumer_rx_transfer_diagnostic_samples` in `RX32-REPORT.json`
  (`memory_device_transfer`, `window_receive`, `window_send`).
- Retained/encoded ratio 2.93 = 30,544 / 10,412, the eight-frame witness.

Limits of this review: I ran no tests and no benchmarks. The mechanism in 3.3
steps 3–5 is assembled from counters and code, not from a per-sequence trace.
The phone evidence is one failed cell. The host load figure is a single reading
taken after the runs.
