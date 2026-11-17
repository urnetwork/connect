# WINDOWSIZE.md: independent review 1

Date: 2026-11-10. I did not read any other review file. Where WINDOWSIZE.md,
BULKSPEEDFINALIZE.md or BULKSPEEDFINALIZE-META.md quote earlier conclusions, I
treated them as claims to check.

**Scope and what I ran.** I reviewed WINDOWSIZE.md as it stood at 17:28 (364
lines, SHA `8f17d9af…`). It changed while I worked, and I also read the earlier
317-line version. I built nothing and ran no tests or workloads. I did the
following:

- Parsed `fast64.stdout` and `fast32.stdout` from
  `/Volumes/data2/urnetwork/temp/native-window-retry-loaded-only-r1/run`. Their
  SHAs match the writeup.
- Compared the measured overlay sources with the working tree. For the files
  that matter, measured `transfer.go` is the working tree plus the native-retry
  and diagnostic hooks. Other product files are identical, except
  `ip_remote_multi_client*` (native retry), the new native-retry file and the
  `private_*` diagnostics. Code citations below therefore describe what ran.
- Read the relevant code.

Section 4 and Appendix B say which numbers are mine and how I got them.
Anything marked **hypothesis** is not proven.

---

## 1. Bottom line

1. **There is no slow ramp in the 64 MiB run.** The 500 ms samples show
   collapse and recovery. W1 reaches **277 Mbps at 2.0 s**, then falls to
   **7–27 Mbps from 6.5 s on**. W2 sits at **2 Mbps from 2.0 to 3.5 s**. W5
   climbs steadily to **327 Mbps**. The window averages (75 → 235) measure how
   much of each window was spent collapsed. They do not show a window that grows
   slowly.
2. **"Collapsed" means the client's ordinary RX child is pinned at its limit.**
   - At 64 MiB the boundary cuts read 4,360,016 bytes (end of W1) and 4,431,872
     bytes (end of W2), against a 4,362,076-byte child. They read 0 at the end
     of W3 and W4.
   - At 32 MiB they read 2.16, 2.00 and 2.18 MB at the ends of W1, W2 and W3,
     then 0.
   - Provider resends as a share of initial writes are **13–92 % in every slow
     window** and **≤ 2 % in every fast window**.
3. **The mechanism is visible in code and in your own boundary record. The
   Transfer "receive window" is not a flow-control window.**
   - `receiveWindowAdvertisement` (transfer.go:13388-13405) advertises a
     constant: the whole RX pool, "independent of free space", in *encoded*
     bytes, to *every* receive sequence.
   - The sender clamps its window to that value (transfer.go:13691-13693) and
     reached it within 0.7 s. At +675 ms into 32 MiB W1, provider 3's lane-0
     sender held **221 items / 2,098,841 encoded bytes**, and later **234 /
     2,181,564**.
   - The receiver charges **30,544 physical bytes** for an 8-frame pack of
     about 9,323 encoded bytes: about **3.3×**. The 2,181,038-byte child
     therefore holds about **71 packs**, against about 234 the sender may have
     outstanding.
   - The overcommit is about 3.3× per sequence, multiplied by the number of
     concurrently sending sequences.
4. **Recovery turns each refusal into seconds of stall.**
   - The receiver sends no refusal notice.
   - Each item gets one selective-gap retransmit, then per-item exponential
     backoff (ScaledRtt × 2ⁿ, up to 8 s).
   - Paced H1 FIFOs defer timeouts without limit while cumulative progress
     continues.

   In the 32 MiB run, **Transfer recovery writes (5,715) ≈ final RX funding
   refusals (5,412)**. The link's 1,212 queue drops were repaired *below*
   Transfer, by the carrier TCP (1,291 edge retransmits). Almost all Transfer
   recovery is caused by the client refusing data it was told it could hold.
5. **Downloads get no lane isolation in this fixture.**
   - The Desktop128 provider never sets `LogicalDataLaneCount` (default 0,
     transfer.go:1050).
   - `providerReplyTransferKey` clears the reply lane (ip.go:9456-9463).
   - So every download flow of a provider shares one ordered lane-0 sequence.
     Your boundary record shows Lane 0 for the refused DATA and for the SYN-ACK;
     the client's SYN went out on lane 8.

   Two signatures of head-of-line release in your own data:
   - In 64 MiB W2, four provider-3 TLS handshakes started about 500 ms apart and
     all finished within 0.6 ms of each other, at 4,146 ms.
   - **The slow mandatory GETs finish on the same millisecond as bulk flows on
     one provider are released.** In 64 MiB W2 the GET finished at 3,564.2 ms,
     alongside provider-7 events (s4 TLS done and GotConn at 3,564.2, s0 header
     at 3,564.3). In 32 MiB W1, probe 2 finished at 5,459.3 ms, alongside three
     provider-3 response headers (5,458.9–5,459.5).
6. **Each window inherits the previous window's backlog of dead-flow data.**
   - RST offers are 0 in W1 of both profiles and **2,576–8,921 in every later
     window**.
   - In 64 MiB W2, providers 0, 8 and 9 (used in W1 but not for W2's new flows)
     wrote **73 new messages but 945 timeout resends**.
   - Reliable, ordered Transfer must deliver all of this data, which the app
     only answers with RSTs. It is delivered through the pinned RX and ahead of
     the new flows' TLS flights on the same lane-0 sequence.

   This is my primary **hypothesis** for the 32 MiB W2 failure. W1 put all 8
   streams on provider 3, and W2 sent 6 dials to provider 3. Five of those TCP
   connects completed in about 50 ms and their TLS never did.
7. **The new SYN-ACK-immediate policy removes the only early sign that a
   provider's return sequence is blocked** (`ip_immediate_ack.go`, uncommitted).
   - A connect through a blocked provider now succeeds in 40–80 ms and then
     stalls in TLS.
   - Together with the native ACK retry, this is essentially the whole product
     delta between the "near-300" aggregate53 run and this one. aggregate53 ran
     an identical `transfer.go` and the HEAD `ip_immediate_ack.go`. Its Fast64
     was 224/174/313/278/299; this run's is 75/65/112/156/235.
   - Both are N=1 on a busy host, so this proves nothing. But the paired A/B is
     cheap and should come before any further mechanism work.
8. **The planned advancing-feedback retry extension is unlikely to move these
   results.**
   - The stuck W2 TLS flights need no client feedback to be sent.
   - Pure-ACK refusal is about 83–92 % (64 MiB) and 59–96 % (32 MiB), and it is
   *higher* in the fast windows.
   - The NAT's own 1 s return timer (with backoff) bounds that mechanism.
   - Count NAT return recoveries before building it.

---

## 2. The controls you asked me to separate, and which one binds

| Control | Code | What this run shows | Binding? |
| --- | --- | --- | --- |
| Inner TCP advertised window (app gVisor stack → provider NAT) | tun.go:151-160 (auto-tuning, 4 KiB up to a budget share); the NAT has no congestion window (ip.go:486-497) | Not reported. App-side TCP retransmits are ≤ 3 per window. The NAT is not instrumented. | **Unknown.** It cannot explain the collapses, because a stuck server flight needs no feedback. It may explain the in-window ramp in healthy windows (H4). |
| NAT return recovery (inner download sender) | ip.go:494-497 (1 s timer, one packet, 1 s persist); ip_tcp_return_recovery.go:352 (backoff ×2ⁿ, capped at 30 s) | **Absent from the report.** The "TCP retransmits" column covers app, device-carrier, provider-carrier and edge stacks only. | Unknown. Instrument it (§9). |
| Transfer send window | transfer.go:13549-13922; transfer_window_size.go:35-52 (learned value is monotone; it shrinks only on a network-quality change, transfer_window_quality.go:45-81) | Provider-3 lane-0 sits at the advertisement (2.18 MB encoded) by +0.7 s | **Binds on "the peer's advertised capacity".** It is too large for the backing, not too small. It does not "auto-resize" slowly. |
| Transfer recovery window | transfer.go:912-960, 3287-3337, 9204-9224 | Resends are 13–92 % of initial writes in slow windows. Deferrals outnumber timeout resends 1.3–5×. | **The amplifier.** It turns a 100 ms overflow into seconds. |
| Receive/reorder backing (RX child plus Head recovery) | transfer_receive_memory_scan.go:144-202, 820-827; transfer_receive_memory_decode.go:101-117 | RX pinned at boundary cuts through every slow window. 5,412 final refusals at 32 MiB. | **The binding resource.** |
| Contract credit | ContractAhead counters | 7/1/2/3/3 switches (64 MiB), all acknowledged | No evidence that it binds. |
| MultiClient provider window | ip_remote_multi_client.go:4250-4262 (affinity first; a race binds the first responder); MaxFlowsPerExit 16 | 32 MiB W1: 8 of 8 streams on provider 3. 64 MiB: 6 → 3 → 3 providers. | **Indirect.** It concentrates flows onto one lane-0 sequence, and with immediate SYN-ACKs a blocked provider keeps winning new flows. |

---

## 3. Agreement and disagreement with the writeup

**I agree with:**

- Refusing a timeout increase or a memory rewrite justified only by correlation.
- Separating the controls.
- The point that raw ACK-refusal totals do not explain anything (§4.7: they
  correlate *positively* with throughput).
- "Window 2 … could still inherit unresolved ownership/recovery from window 1".
  The data are stronger than the writeup states (§4.5).
- The native zero-window retry being a real but narrow fix.
- Needing the GET's phases and identity.
- Treating the busy host as a reason not to promote a baseline.

**I disagree with:**

- **"No window-size root cause established" and the "automatic window
  resizing" framing.**
  - The Transfer send window is not growing slowly. It is clamped to the
    advertisement almost at once.
  - The window-size defect is the advertisement itself:
    - its units are encoded bytes, while the receiver charges retained bytes;
    - it ignores how many sequences share the pool;
    - it ignores free space.
  - The send path comment at transfer.go:13714-13724 (the measurement is quoted
    at 13720-13724) already recorded this exact failure in another form: "a 4 MiB initial against a 256 KiB advertised hold
    gave 35 evictions and 330 refused arrivals". The clamp fixed the encoded
    overrun. The advertised value is still about 3.3× the physical backing.
  - The ACK-path comment says the field is "what this receiver can still hold
    out of order" (transfer.go:17329-17331). The implementation contradicts that
    intent (transfer.go:13388-13395).
- **"Near its 2.18 MB child limit at specific early cuts … not continuous
  occupancy evidence".** The report contains Before/After occupancy cuts at
  every window boundary for **both** profiles (`AfterDetails.Counters.sdk.device`):
  - 32 MiB is at the limit at three consecutive boundaries, more than 20 s
    apart.
  - 64 MiB is at the limit at two.
  - The per-window managed peaks (32 MiB: 2.03–2.08 MiB in W1–W4) agree.

  That is sustained pinning, not one early sample. The writeup's statement that
  "exact physical RX window cuts are unavailable for the 64 MiB arm" is true of
  `rx_physical_window_cuts`, but these occupancy cuts *are* available.
- **"The shared root is far from exhausted" as a reassurance.** It is true and
  irrelevant: the ordinary RX child binds. Q3's phrase "without a shared-root
  shortage" frames this the wrong way. There *is* a shortage, of the RX child.
- **"RST count alone is not a root cause."** Agreed, but RSTs are the only
  direct measure in the report of dead-flow DATA reaching the app after
  cancellation. They are 0 in both W1s and thousands afterwards, which is strong
  timing evidence even without per-event timestamps.
- **"Not a paired estimate" as a reason to set aside the near-300 run.** The
  product delta to this run is three enumerable changes (§4.6). That makes it
  the cheapest high-value A/B available.
- **The table's "TCP retransmits" column.** It reads as inner-TCP loss but is
  almost all outer carrier and edge TCP (W1: edge 1,450 of 1,519; app stack 2).
- **"No new lanes".** Enabling the existing provider-side lane count is not a
  new lane design. The SDK itself says that download isolation needs it: "Download
  data needs the same setting on the provider sender" (sdk/mobile_memory_policy.go:206).

---

## 4. Evidence details (all verified in data or code unless marked)

### 4.1 Within-window collapse (64 MiB, Mbps per 500 ms sample)

- W1: 8, 153, 134, **277**, 213, 116, 102, 109, 63, 44, 84, 61, 22, **7**, 27, 13, 12, 23, 22
- W2: 0, 33, 63, 14, **2, 2, 2**, 87, 116, 136, 129, 146, 171, 16, 47, 45, 75, 100, 44
- W5: 43, 89, 159, 184, 184, 208, 217, 211, 229, 246, 259, 257, 276, 284, 286, 303, 311, 312, **327**
- 32 MiB W1: 18, **69**, 51, 25, 2, 21, 11, 23, 2, 2, 8, 36, 2, 7, 1, 1, 2, 3, 5
  (all 8 streams on provider 3, **zero link drops**)

The 32 MiB W4 and W5 windows ran at 217 and 155 Mbps *despite* 412 and 167
link queue drops. **Link loss is neither necessary nor sufficient for
collapse.**

### 4.2 RX state at boundaries, resend ratio, head recovery and RSTs

| Profile / window | Mbps | RX at end (bytes) | Resends / initial writes | Head-recovery reserved | RST offered / refused |
| --- | ---: | ---: | ---: | ---: | ---: |
| 64 W1 | 75.2 | 4,360,016 | 1,591 / 12,446 (13 %) | 131.8 MB | 0 / 0 |
| 64 W2 | 64.9 | 4,431,872 | 3,066 / 10,042 (31 %) | 57.9 MB | 8,921 / 4,532 |
| 64 W3 | 112.1 | 0 | 371 / 16,900 (2.2 %) | 2.6 MB | 5,911 / 2,780 |
| 64 W4 | 155.9 | 0 | 4 / 23,266 | 0 | 7,117 / 4,527 |
| 64 W5 | 235.1 | 678,288 | 4 / 33,970 | 0 | 5,313 / 3,630 |
| 32 W1 | 15.7 | 2,160,608 | 1,578 / 2,905 (54 %) | 14.3 MB | 0 / 0 |
| 32 W2 | 4.8 | 1,997,232 | 844 / 1,446 (58 %) | 21.2 MB | 4,269 / 2,109 |
| 32 W3 | 12.3 | 2,178,496 | 2,047 / 2,216 (92 %) | 31.1 MB | 2,576 / 1,539 |
| 32 W4 | 216.9 | 0 | 406 / 30,663 (1.3 %) | 4.0 MB | 3,273 / 2,071 |
| 32 W5 | 155.3 | 0 | 8 / 22,046 | 0 | 5,018 / 3,221 |

Both W1s start with RX at 0. "Head-recovery reserved" is the per-window delta of
the cumulative reserved bytes. A collapsed window runs largely on Head recovery,
one exact head per sequence at a time.

### 4.3 Advertisement versus backing (32 MiB boundary record, W1)

**Code:**

- transfer.go:13396-13405: `share := ReceiveQueueMaxByteCount`, then
  `min(share, budget.TotalByteCount())`, then the optional cap. There is no
  free-space or sequence-count term.
- transfer_receive_memory_scan.go:144-162 and 169-202, plus 820-827: the
  retained cost is
  - the carrier root (12,288 B class for this carrier),
  - plus a 2,048 B class per 1,280-MTU frame,
  - plus the 1,024 B owner (transfer.go:1473),
  - plus overflow metadata.

  For 8 frames that is about 30.5 KB. It matches the record's 30,544 bytes.
- The SDK says this explicitly (sdk/mobile_memory_policy.go:132-140): "an
  out-of-order run fills the pool before the advertised payload figure is
  reached … which the eviction notice turns into resends. That is bandwidth
  after a loss". As §4.4 shows, it costs far more than bandwidth.

**Data**, from `common_boundary_observation` in fast32:

- **First refusal at +675 ms** into W1:
  - non-head ordinal 493, floor 416, ACK head 414;
  - RX 2,179,856 of 2,181,038;
  - **78 live ingress owners, held reorder queue = 0**, Head recovery 0.

  **Nothing was missing from the sequence at that moment.** About 78 packs
  (around 116 ms of arrivals at that rate) were paid for but not yet released
  downstream. The trigger was a consumer-side lag, not loss. Which stage held
  them is unknown; §9 covers instrumenting it.
- At the same moment the sender for that sequence held 221 items / 2,098,841
  encoded bytes, and later 234 / 2,181,564. The receiver's capacity was about
  2,181,038 / 30,544 ≈ 71 packs.
- **First head refusal at +1,442 ms**: ordinal 862, 79 held successors. It was
  funded by Head recovery and its terminal ACK came 21.9 ms later. Head recovery
  works; it just moves one exact head at a time.

### 4.4 Recovery writes almost all come from receiver refusals (32 MiB, whole run)

- Provider recovery writes total 5,715:
  - TimeoutResendWriteCount 4,883;
  - SelectiveGapWriteCount 822;
  - tail and cumulative probes 10.
- Final all-funding refusals: 5,412. This matches the writeup's own
  reconciliation and the PackHandoffDrop delta.
- Link queue drops: 1,212. Edge TCP retransmits: 1,291. These were repaired
  inside the H1 carrier and never became Transfer holes.

The recovery machinery is tuned for rare path loss, and here it mostly services
local refusals:

- one immediate selective-gap retransmit per item, at most 4 per round
  (transfer.go:951-957);
- per-item exponential backoff up to 8 s (transfer.go:921-922, 9219-9222);
- timeout deferral while cumulative progress continues. On a stable paced H1
  FIFO the deferral is unlimited unless a later ACK on the same route exists
  (transfer.go:3287-3291, 3323-3336). A contiguous run refused while RX was full
  has no later ACK, so it is repaired serially.

There is no refusal notice. `noteEviction` (transfer.go:13360-13373) covers
evicted held items only. A decode-funding refusal
(transfer_receive_memory_decode.go:109-115) records a drop and a local pressure
request (transfer_receive_memory_pressure.go:42-76), and the sender learns
nothing.

### 4.5 Single ordered download sequence per provider; head-of-line signatures

- **Code.** `providerReplyTransferKey` sets `LogicalLane = 0` (ip.go:9461). The
  provider's own lane gate returns "zero count", meaning lane 0, when
  `LogicalDataLaneCount == 0` (transfer.go:6346-6354). The Desktop128 policy
  builds a linux-profile client and never sets it
  (`…/freeze-r1/shared/sdk/private_production_memory_policy.go:66-93`).
  Mobile sets 8 lanes for *its own* sends only
  (sdk/mobile_memory_policy.go:200-209, 473-485).
- **Data.**
  - The boundary record shows the refused DATA (lane 0) and the matching
    SYN-ACK (lane 0, Immediate) in the same provider→client sequence. The
    client's SYN went out on lane 8.
  - Simultaneous releases on one provider (my script, 3 ms tolerance, delays
    over 200 ms):
    - 64 W2: TLS done for s2, s3, s6 and s7 on provider 3 at 4,146.0–4,146.6 ms,
      after starts at 1,042 and 1,541 ms.
    - 32 W1: headers for s5, s6 and s7 on provider 3 at 5,458.9–5,459.5 ms,
      after requests written 2.8 s earlier.
    - The same pattern recurs in 32 W3 and W4 and in 64 W3–W5.
  - **The mandatory GET is caught in these releases.**
    - 64 W2: the GET started at 504.8 ms with 3,059.4 ms latency, so it finished
      at 3,564.2 ms. That is exactly s4's TLS done and GotConn (3,564.2) and
      s0's header (3,564.3), all on provider 7.
    - 32 W1: probe 2 finished at 5,459.3 ms, alongside the three provider-3
      headers above.

### 4.6 Cross-window dead-flow backlog, and the run-to-run product delta

- In 64 MiB W2, the per-provider deltas for the W1-only providers were p0
  62 initial / 246 timeout, p8 6 / 462, p9 5 / 237. That is **945 resends
  (about 8.8 MB) of W1 data while serving essentially no W2 flow**.
- In 32 MiB W2, provider 3 (W1's only exit) had 528 / 395. In W3, provider 2 had
  23 / 244.
- On RST the NAT stops its send loop (ip.go:6765-6771). Data already handed to
  Transfer stays retained, up to the lane-0 window (about 2.18 MB encoded) plus
  queued input, within the 5,752,188-byte TX budget.
- **The product delta from aggregate53 (near-300) to this run.** Comparing
  the overlays, the differences are:
  - `ip_immediate_ack.go`: HEAD versus SYN-ACK-immediate;
  - native ACK retry: `ip_remote_multi_client*`, the new retry file and the
    retry hooks in `transfer.go`;
  - diagnostic hooks and files.

  aggregate53's `transfer.go` is byte-identical to the working tree. The cold32
  observer attaches only to Fast32, so the Fast64 drop (224/174/313 → 75/65/112
  in W1–W3) leaves the SYN-ACK policy and the native retry as the product
  candidates. **Hypothesis**, N=1 each on a busy host.

### 4.7 ACK and feedback refusals do not separate fast from slow windows

- Pure-ACK batch refusal by window:
  - 64 MiB: 83, 86, 91, 92, 92 %;
  - 32 MiB: 64, 69, 59, **96, 95 %**.
- App-stack TCP retransmits: ≤ 3 per window.
- The 32 MiB W2 stuck flows completed TCP in about 50 ms and never received the
  server's first TLS flight. The NAT can send that flight under the initial peer
  window, so a lost client ACK cannot hold it back.

---

## 5. Gaps, unverified assumptions, likely-wrong conclusions

1. **Only window-boundary cuts and lifetime maxima exist; there is no
   per-sequence time series.** You cannot see the RX pin → refusal → floor-age →
   backoff chain develop. Sampling every 250 ms is the single most useful
   addition (§9, item 2).
2. **The inner download sender, the NAT, is invisible.** There are no counters
   for return recoveries or attempts, persist probes, peer window or bytes in
   flight. The TCP-retransmit column has been read as if it covered inner TCP.
3. **RX occupancy is not split by stage**: mailbox, held reorder, buffered
   batch, in-flight downstream delivery. The first refusal had 78 live owners
   and an **empty** reorder queue (§4.3), so "reorder backing" is not the right
   label for what pinned RX at that moment.
4. **The earlier "negative" paused-worker test checks the wrong property.**
   `TestSharedPumpPausedWorkerRefusedRunMeetsLoadedRequestBound` sleeps a fixed
   5 s of fake time and asserts only eventual completion
   (transfer_shared_pump_paused_worker_recovery_test.go:289-309). Its logged
   "elapsed" is that fixed bound. By your own account it reproduces the
   mechanism (96 refused → 144 timeout writes), but it never measures how long
   recovery took or what goodput was lost. It does not falsify the collapse
   mechanism; it demonstrates the mechanism and scores it as a pass.
5. **The SYN-ACK-immediate change is not discussed** as a possible regression
   that can mislead provider selection. "TCP dial 42–80 ms" is now no evidence
   that a provider's return path is healthy.
6. **Single-lane downloads are not mentioned.** All per-flow head-of-line
   reasoning changes once you know that every flow of a provider shares one
   sequence.
7. **Provider concentration is the larger difference between profiles in this
   run.** 32 MiB W1 used one exit for all 8 streams. The new note that the
   profiles differ in policy is right; concentration matters more here.
8. **There is no per-window host-contention metric.** A busy host is a plausible
   *trigger* for the consumer lag in §4.3. Scheduler latency from
   `runtime/metrics` (`/sched/latencies:seconds`) per window would show it.
9. **Dead-flow volume is inferred from RST counts**, not measured as bytes
   delivered for flows the app had already reset.
10. Flows that never got a byte have empty provider masks. Their dial-time
    provider is inferred only from `ProviderDials` deltas (32 MiB W2: 6 dials to
    provider 3).

---

## 6. Ranked hypotheses and cheap experiments to confirm or refute

**H1 (high): advertisement–backing mismatch plus refusal-driven recovery causes
the collapses.** Any burst or hiccup longer than about 70 packs' worth of
arrivals overflows RX. The roughly 3× excess becomes refusals, the refused runs
become serialized, backed-off resends, and the window collapses for seconds.

- **E1 (config only, one arm).** Set the consumer's
  `ReceiveBufferSettings.AdvertisedReceiveWindowMaxByteCount`, an existing cap
  (transfer.go:15672-15675) that is currently unset everywhere:
  - 32 MiB: 655,360 (about RX / 3.3);
  - 64 MiB: 1,310,720.

  Run unchanged Fast5 at both profiles, ABBA, N ≥ 2.

  **If H1 holds:** final refusals and Head-recovery reservations fall ≥ 10×,
  W1–W3 resends fall ≥ 10×, the multi-second dips disappear from the 500 ms
  samples, and W1/W2 approach late-window rates. Peak throughput is capped at
  about 105 Mbps (32 MiB) or 210 Mbps (64 MiB) per provider at a 50 ms
  residence.

  **If refusals persist, H1 is wrong.** Something other than sender flight
  overruns RX, such as the dead backlog (H2) or delivery lag with a small
  window.
- **E2 (deterministic, about an hour).** Re-score the existing paused-worker
  test. Poll in 10 ms virtual steps until the hot set is ACKed, then record
  T_recover, timeout writes and goodput. Run four arms:
  - the default;
  - the E1 cap;
  - `DeferTimeoutResendWhileCumulativeProgress=false`;
  - `MaxResendInterval` = 1 s.

  **Prediction:** the default recovers in seconds and the cap in about the
  pause plus 2 RTT. This gives you an old-fails/new-passes invariant: "a finite
  consumer pause of D ms causes no refusal and recovers within D + 2 RTT."

**H2 (high for W2 and the 32 MiB failure): the dead-flow backlog on a shared
lane-0 sequence blocks new flows.**

- **E3 (deterministic; answers Q3).** Use provider A and flows F1 and F2, in
  virtual time, with no permanent loss and no sleeps as proof:
  1. Load F1 until A's lane-0 sequence holds N retained or refused originals
     (pause the consumer or use the real pool).
  2. Close F1 through the real app stack, which sends an RST.
  3. Open F2 to A: SYN, then an immediate SYN-ACK, then the ClientHello.
  4. Release the consumer.

  **Assertions:**
  - F2's first server-flight ordinal is greater than A's receive floor;
  - F2's ServerHello arrives only after every dead F1 ordinal ahead of it has
    been delivered;
  - the delay scales with N.

  **Control:** F2 on provider B, or on A after it drains, connects immediately.
  The same fixture also answers Q5 if you refuse the RSTs at the bridge.
- **E4 (arm).** Set Desktop128 `LogicalDataLaneCount = 8` together with E1. The
  W2 TLS stalls should become lane collisions only: fewer of them, partial
  rather than per-provider.

**H3 (medium): SYN-ACK-immediate (and/or the native retry) regressed the early
windows.**

- **E5.** Run a paired ABBA of the attested aggregate53 binary against the
  current binary, Fast64 and Fast32, N ≥ 2. Or add an arm that reverts only
  `ip_immediate_ack.go`.
- **E6 (deterministic).** Run E3 with ordered versus immediate SYN-ACK and a
  2-provider race. With ordered SYN-ACK the race should bind to B; with
  immediate it binds to the blocked A.

**H4 (medium, but only for the gradual climb in healthy windows): the app
stack's receive-window auto-tuning, a NAT with no congestion window, and
connection reuse.** In 64 MiB W5, the reused connections (s0 and s4, 78 and
59 MB) carried 2–4× the fresh ones (18–23 MB on provider 9).

- **E7.** Sample the NAT's peer window and bytes in flight per flow every
  500 ms. Add an arm with app `TcpReceiveBuffer` Default = Max (no auto-tuning).
  If the in-window climb disappears, the TCP advertised window bound healthy
  windows.

**H5 (low): lost advancing feedback (your Q1).**

- **E8.** Add NAT counters for `returnAttempts` increments and persist probes
  per window. If slow windows show fewer than about 10 NAT recoveries,
  deprioritize the eligibility extension.

**H6 (trigger, not cause): host contention or consumer lag.**

- **E9.** Record the per-window scheduler-latency histogram. Replicate once on a
  quiet host, or under `GOMAXPROCS` pressure.

**H7 (low): contract startup.**

- **E10.** Report contract-wait time (`ContractWaitLogThreshold` already logs
  waits over 50 ms). Optionally add an arm with initial contract = standard
  contract.

---

## 7. New ideas and simpler alternatives

Smallest first.

1. **Make the advertisement honest, in one function**
   (`receiveWindowAdvertisement`). Convert to encoded units with a conservative
   physical:encoded factor derived from the pool classes, and divide by the
   number of sequences that currently hold data or have recent arrivals (with a
   floor). This is QUIC's model: connection-level credit plus per-stream credit.
   It adds no new memory model. The cost is a throughput ceiling of about
   pool / RTT whenever the pool is the only buffer, which is why item 4 matters.
2. **Add a refusal notice.** Reuse the eviction-notice field to report
   decode-funding refusals by ordinal. On receipt the sender:
   - skips backoff and deferral for those items;
   - resends them paced by receiver progress, not timers;
   - optionally shrinks the learned window. Today it can only shrink on a
     network-quality change (transfer_window_size.go:43-50).

   This keeps the optimistic window and makes overflow cost about one RTT
   instead of seconds.
3. **Handle dead flows.**
   - (a) Exempt RSTs from the pure-ACK protocol-slot contention at the bridge.
     About 50–64 % are refused, and each refusal keeps a dead flow alive at the
     NAT for another round.
   - (b) On RST, the NAT drops the flow's unsent return queue.
   - (c) Later: a partial-reliability "skip" for retained dead-flow originals,
     in the style of SCTP FORWARD-TSN.
4. **Compact RX storage.** Release the carrier root after an owned decode, and
   add an MTU-sized frame class (about 1,408 bytes). That takes the cost from
   about 30.5 KB to roughly 14–18 KB per 8-frame pack, so the same 2.18 MB holds
   about 1.7–2.2× more. This is the "early carrier-buffer retirement" that was
   rolled back. E1 measures its value first: if the cap removes the collapses
   but also caps peak throughput, this is what buys back the cap.
5. **Download lanes on the provider** (an existing setting), paired with item 1
   or E1 so that 8× more sequences do not multiply the overcommit.
6. **Data-path health in MultiClient placement.** Because SYN-ACK RTT no longer
   measures the return path, affinity reuse and race completion should
   deprioritize a provider whose receive floor is older than about 2 RTT or
   whose held bytes exceed some share of the pool.
7. **A simpler alternative for H1 carriers.** Transfer runs over TCP, so path
   loss is already repaired below it. Use Transfer timers only for receiver
   refusal (with item 2) and carrier change. That mirrors the hybrid
   H3-reliable branch, which already relaxes the timer (transfer.go:9238-9240).

---

## 8. Direct answers to the five questions

**Q1. Can lost advancing feedback strand the NAT, and does the narrow retry
reject the update that would help?**

Yes in principle. With outstanding data, the NAT waits 1 s, then 2, 4, …
(ip_tcp_return_recovery.go:352), retransmits one packet and needs that
retransmit's duplicate ACK to get through. At about 85 % refusal that can take
several rounds. The current eligibility (singleton, zero window, same frontier)
would indeed reject an advancing update.

The data do not point to this as the cause of either the ramp or the W2 failure:

- the stuck flights need no feedback;
- refusal ratios are higher in fast windows;
- app-stack retransmits are negligible.

Measure NAT recoveries first (E8).

**Q2. What predicate distinguishes the four cases?**

Evaluate per stalled flow F on provider P, at the provider's lane-0 return
sequence S:

- **(a) Lost feedback.** All of these hold:
  - the NAT has unsent bytes;
  - `snd_nxt − snd_una ≥` the applied peer window;
  - the client's last refused ACK of F has a right edge beyond the NAT's applied
    one;
  - **S retains no original carrying F**.
- **(b) Ordered DATA hole.** All of these hold:
  - the floor ordinal f of S is not held;
  - there are held successors;
  - the sender retains f with sendCount ≥ 1 and a future resend time.
- **(c) Retained-future debt.** All of these hold:
  - RX used ≥ limit minus one pack;
  - sender outstanding is much larger than the receiver's live owners;
  - refusals are rising;
  - the sender retains items above f that the receiver does not hold.
- **(d) Uncompleted first TLS flight.** F's first server-flight ordinal d_F
  satisfies d_F > f, and the ordinals in [f, d_F) belong to other flows,
  especially flows the app has reset.

To compute (d) and to quantify dead bytes, tag each return Pack with its flow
keys (the scheduling key already exists).

**Q3. Can cancellation and retained ownership isolate providers without a
shared-root shortage?**

Yes, and the evidence in §4.5 and §4.6 favours it. Note that the RX *child* is
short even though the root is not. The ordering that reproduces it without
permanent loss is E3:

1. dead F1 originals retained or refused on S;
2. F2's SYN-ACK overtakes them (immediate);
3. F2's server flight queues behind them (ordered).

**Q4. Which control separates the factors?**

There is no single counter. Run single-knob arms on the unchanged workload,
each with one predicted signature:

- receiver backing: E1 (or the parked RX2X), which predicts refusals vanish;
- Transfer recovery: E2 and a backoff or deferral arm, which predicts shorter
  dips with the same refusals;
- TCP state: E7;
- selection: a fixed-provider or reset-affinity arm, plus E5;
- contract: E10.

The 250 ms per-sequence time series is what makes each arm readable.

**Q5. Do failed or partial native groups lose feedback needed to release the
resource that blocks new handshakes?**

Yes: RSTs. About 50–64 % of RST batches are refused at the bridge, in
contention with the pure-ACK flood for protocol owners. Each refused RST keeps a
dead NAT flow alive and its data flowing toward a pinned RX. A second, more
important point is that the resource blocking new handshakes is S's ordered
backlog. No client feedback can release that backlog; only delivering it (or a
skip) does.

---

## 9. Prioritized next steps

1. **E2** (deterministic re-score of the existing paused-worker test, four
   arms). It is cheap, adds no new fixture, and gives you a red/green recovery
   invariant.
2. **Instrumentation for one attributed run.** Sample every 250 ms per
   provider→client sequence:
   - sender outstanding items and encoded bytes, plus `SendWindowEstimate`
     Window and Reason;
   - receive floor and floor age;
   - held items;
   - live owners split by stage;
   - RX and Head used;
   - deltas of refusals, resends and deferrals.

   Also add:
   - NAT `returnAttempts` and persist counts, peer window and bytes in flight;
   - app-stack TCP receive-buffer drop stats;
   - scheduler latency;
   - GET phases with the dial-time provider and the GET's first response
     ordinal against the floor (your plan already covers the phases).
3. **Arms**, run ABBA and N ≥ 2 on the unchanged Fast5 and full100 at both
   profiles:
   1. **E1 advertisement cap**;
   2. **E5 aggregate53 binary versus current** (or a revert of
      `ip_immediate_ack.go` only);
   3. **E4 provider lanes = 8 together with E1.**
4. **E3 and E6** deterministic tests for the cross-window head-of-line block and
   for the SYN-ACK policy.
5. **Choose a fix based on 1–4.**
   - If E1 wins: make the advertisement honest (§7.1).
   - If E1 caps peaks: add the refusal notice (§7.2) and compact storage
     (§7.4).
   - If E3 and E5 implicate SYN-ACK-immediate: keep SYN-ACKs ordered, or add
     data-path health to placement (§7.6).
   - Dead-flow purge on RST (§7.3) is worth doing in any case.
6. **Defer the advancing-feedback eligibility extension** unless E8 shows NAT
   recoveries in the slow windows.
7. Keep everything as diagnostic evidence until a quiet-host replicate exists.
   None of the above changes timeouts, and E1 lowers a default rather than
   raising one.

---

## Appendix A: other numbers I checked against the writeup

- Window Mbps, measurement Mbps, largest GET, resends, TCP retransmits, link
  drops, provider bitsets, reuse counts 0/0/1/2/2, the 32 MiB W2 GET (start
  500.1 ms, canceled at 9,500.7 ms), five TLS stalls after 42–57 ms dials, the
  56.5 µs reused write, origin-3 bytes of 5.99 MB (both flows on **provider 2**,
  which is the discriminator, rather than the origin), and the Head funding with
  ACK 21.929 ms later: **all match**.
- The 64 MiB W2 TLS range of 123 ms to 4,553 ms matches.
- The writeup's own correction that selection is 5+1 rows at 32 MiB and 10+2 at
  64 MiB matches `initial_window`.

## Appendix B: how I computed things

The scripts are in my scratch directory
(`/private/tmp/claude-501/-Users-builder-urnetwork-connect/5b991e4a-8699-464e-a6d4-9e0a0403d954/scratchpad/windowsize-review1/scripts/`).
They are ephemeral, and each step is easy to redo:

- `extract.py` pulls the `[perfvar] synthetic-fast` JSON.
- Per-window deltas are `AfterDetails` minus `BeforeDetails` for:
  - `Counters.sdk.device.ClientReceiveByteCount`;
  - `ClientReceiveHeadRecoveryReservedByteCount`;
  - `Counters.provider_recovery[*]`;
  - `Bridge.OtherTcpControlOfferedByFlag` and `RefusedOtherTcpControlBatchByFlag`;
  - `LinkQueueDrops`;
  - `edge_tcp_sent_received_retransmits_timeouts`;
  - `tcp_sent_received_retransmits_timeouts`, where row 0 is the app stack,
    row 1 the device carrier and rows 2 onward the provider carriers.
- Throughput per sample is the difference in `workload.samples[].validated_bytes`
  over 0.5 s.
- Head-of-line events are flow phases (`TlsHandshakeDone`, header, first
  payload) more than 200 ms after their predecessor that coincide within 3 ms
  across flows.
- GET completion is `loaded_probe_start_ns + loaded_latency_ns`, compared with
  flow events within 2 ms.
- The capacity ratio is `common_boundary_observation.FirstAnyRefusal.RetainedBytes`
  divided by `Sender.QueueEncodedBytes / Sender.QueueItems`.
