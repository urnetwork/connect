# WINDOWSIZE review 2: independent analysis

Date: 2026-11-10. Subject: [WINDOWSIZE.md](WINDOWSIZE.md). Written without reading any
other review file. Background read: SIMPLERISSUE.md, SIMPLERISSUE2.md, SIMPLERISSUE3.md,
BULKSPEEDFINALIZE.md.

## 0. Bottom line

1. **The five-window "ramp" is not a window ramp.** The provider's reliable send window
   is effectively a constant: it opens at the configured 2 MiB as soon as the first ACK
   arrives and can only grow toward the client's advertisement
   (`transfer_window_size.go:35-53`, `transfer.go:13730`, `transfer.go:13758-13760`). Nothing
   in the loss path can shrink it. At 32 MiB it moves 2,097,152 -> 2,181,038 bytes (4%).
   Automatic window resizing is not what changes between window 1 and window 5.

2. **What changes is whether the client RX leaf is full, and that state is carried across
   the window boundary.** The report already contains the proof (section 3.1): every slow
   window either starts with the client receive budget full of the previous window's
   cancelled-flow data, or fills it and ends full; the fast windows are the ones in which
   it is at zero or drains to zero within the first half second. In the 2026-11-08
   300-400 Mbps reference the same gauge is ~0 at every boundary.

3. **The mechanism is a metastable overflow -> RTO limit cycle, verified in code** (section
   3.2): the advertised receive window is expressed in encoded bytes but sized to the
   physical RX total, which is charged at 2.93x encoded for a full H1 DATA pack; a
   window-limited sender therefore overruns RX by about 3x; the shared receive pump drops
   instead of blocking (since d90ac7f4); the receiver sends no refusal feedback; the sender
   recovers only by timeout with per-item exponential backoff (0.3, 0.6, 1.2, 2.4, 4.8 s);
   and all download data from one provider is one ordered sequence (lane 0), so one refused
   pack head-of-line blocks every flow on that provider, including the mandatory small GET.

4. **The native window-reopen retry is a real fix for a mechanism this failure does not
   exercise.** In the failing 32 MiB run the fixture's own ACK-progress counter reports
   `ReopeningWindowRefusals = 0` (of 44,272 classified pure-ACK refusals; 4,909 more had no
   accepted baseline to classify against). The stalled window-2 flows are in TLS handshake
   with a few KB outstanding; no TCP right edge is anywhere near exhausted. Extending
   retry eligibility to advancing ACKs (the writeup's next step) is, in my assessment, not
   on the critical path. See section 6, Q1.

5. **Two changes separate today's candidate from the fast reference, and both are visible in
   the artifacts**: (a) commit d90ac7f4 changed the shared pump's Pack handoff from the
   policy timeout (wait, i.e. lossless backpressure on H1) to zero (drop and rely on sender
   retry); (b) the client now delivers far fewer inner TCP ACKs to the provider: about
   200-2,400 accepted per window versus about 31,000-39,000 in the reference, i.e. 15-30x
   fewer at 64 MiB and 30x or more at 32 MiB.

6. **Recommended unblock** (section 8): run one cheap unit-consistency experiment using
   knobs the fixture already has (advertised cap = RX/3, or physical RX = 3x, or provider
   retained limit below RX capacity); build the deterministic sustained-source version of the
   existing paused-worker test; then fix the control loop (truthful advertisement, a
   "ready, resend #N" notice on the existing eviction field, no backoff accrual on
   receiver refusals) before touching ACK retry eligibility again.

Confidence: items 1-3 are verified against code and the raw reports; item 4 is verified
for the zero-window variant and inferred for the advancing variant; item 5a is verified
by diff, 5b by counters; the trigger of the very first overflow in window 1 is not
established (section 5, H5).

## 1. What I did and did not do

- **Ran nothing from the project.** No build, no test, no Fast5. All conclusions come
  from reading source and from offline analysis of existing artifacts with throwaway
  scripts (kept in my scratch directory only).
- Source read: the working tree at `/Users/builder/urnetwork/connect` and the effective
  candidate defined by `/Volumes/data/urnetwork/temp/native-window-retry-common-r1/CANDIDATE.overlay.json`.
  I diffed the two: of 442 product files, 428 are byte-identical. The candidate adds
  `ip_remote_multi_client_native_ack_retry.go` and private diagnostics, and differs in
  `transfer.go` (six small hunks: retry fences and test hooks), `ip_remote_multi_client.go`,
  `_collapse.go`, `_group_completion.go`, and `transfer_receive_memory_decode.go`
  (observation hooks). **Line numbers below are working-tree lines**; the cited sites are
  identical in the candidate apart from small offsets.
- Artifacts analysed: `native-window-retry-loaded-only-r1/run/fast32.stdout` and
  `fast64.stdout` (the writeup's runs), and the 2026-11-08 reference at
  `/Volumes/data/urnetwork/temp/synthetic-smoke-r1/recovery-performance-r1/runtime-r1/fast5-{32,64}-desktop128/stdout`
  (428/393/340/320/366 and 362/378/311/394/402 Mbps).
- Every run cited is N=1 per profile on a busy host. I use them for mechanism evidence
  (exact counters, boundary gauges, phase timestamps), not for throughput statistics.

## 2. My understanding of the writeup, and where I agree

The writeup reports one 64 MiB Fast5 that passes with 75/65/112/156/235 Mbps and one
32 MiB Fast5 that fails window 2 (mandatory small GET never completes, cancelled at
9.5 s). It describes a qualified fix (retain one refused zero-window-reopening pure ACK
and reoffer it when a protocol slot opens), states that it does not explain the Fast5
failure, and proposes as the next falsifier a "lost advancing feedback" eligibility gap,
plus better per-window and per-probe diagnostics.

I agree with, and verified:

- The numbers in both tables (throughput, RX peaks, timeout resends, TCP retransmits,
  link queue drops, per-window RX drops 2,122/988/2,248/54/0). The per-window drops
  sum to 5,412, which equals `FundingAfterOrdinaryRxRefusal[all-funding-refused]`.
- The description of retry eligibility. In the donor source
  (`/Volumes/data2/urnetwork/temp/native-window-update-recovery-r1/source/connect/ip_remote_multi_client_native_ack_retry.go:53-69`)
  a refused packet is retained only if it is a singleton pure ACK with nonzero window,
  the last admitted state has window 0, and the ACK number is unchanged.
- That an exhausted positive grant with retained return chunks waits for the provider
  NAT's data resend timer, not persist: persist requires `receiveWindowSize == 0`,
  `receiveSeqAck == receiveSeq` and no retained chunk (`ip.go:6312-6316`,
  `ip_tcp_return_recovery.go:341-343`); with retained chunks the delay is
  `ReturnResendTimeout` (1 s) doubling per attempt, one packet per flight
  (`ip_tcp_return_recovery.go:347-357`, `ip.go:494-496`).
- The discipline: no new lanes, no timeout extension, no broad memory rewrite, keep
  failed runs, require an old-fails/new-passes test.
- "Raw refusal totals alone cannot explain the failure" for pure ACKs: correct, and the
  data are stronger than the writeup says. The fast windows have the *highest* pure-ACK
  refusal rates (95-96% at 32 MiB windows 4-5 versus 59-69% in windows 1-3).

Where I disagree is the choice of next step and one framing point: the writeup says
"no window-size root cause established" and lists receive backing among five candidate
controls of equal standing. I think the artifacts already single it out.

## 3. Main finding

### 3.1 The slow windows are the windows with a full client RX leaf, and the state crosses window boundaries

Composite client receive gauge (`sequence.runs[i].{Before,After}Details.Counters.sdk.device.ClientReceiveByteCount`;
RX + head recovery + pack queue) at the start and end of each window, with the exact
final-refusal count from `rx_physical_window_cuts` (32 MiB only) and provider Transfer
timeout resends:

| Profile | Window | Mbps | Receive gauge at start | at end | Final RX refusals | Provider timeout resends |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| 32 MiB (cap 2,181,038) | 1 | 15.7 | 0 | 2,160,608 | 2,122 | 1,578 |
| | 2 | 4.8 (invalid) | 2,160,608 | 1,997,232 | 988 | 844 |
| | 3 | 12.3 | 1,997,232 | 2,178,496 | 2,248 | 2,047 |
| | 4 | 216.9 | 2,178,496 | 0 | 54 | 406 |
| | 5 | 155.3 | 0 | 0 | 0 | 8 |
| 64 MiB (cap 4,362,076) | 1 | 75.2 | 0 | 4,360,016 | n/a | 1,591 |
| | 2 | 64.9 | 4,360,016 | 4,431,872 | n/a | 3,066 |
| | 3 | 112.1 | 4,431,872 | 0 | n/a | 371 |
| | 4 | 155.9 | 0 | 0 | n/a | 4 |
| | 5 | 235.1 | 0 | 678,288 | n/a | 4 |
| Reference 2026-11-08, 32 MiB | 1-5 | 428-320 | 0 | 0 to 61,088 | n/a | 16/8/12/12/16 |
| Reference 2026-11-08, 64 MiB | 1-5 | 362-402 | 0 to 332,800 | 0 to 271,712 | n/a | 0/0/0/8/4 |

Reading:

- Windows 2 and 3 at both profiles, and window 4 at 32 MiB, **begin with the receive
  budget already 91-100% full** although the previous window's workers were cancelled and
  joined and `requests_quiescent` is true. That occupancy can only be data for flows that
  no longer exist. New flows' first DATA packs then arrive at a full RX and are refused.
- Timing matters for reading the "start" column: the fixture starts window N+1 only
  0.5-9 ms after window N's measurement ends (owner start minus previous measurement end
  in the report), so the start gauge is the state at the cancel instant. A window that
  ends clean leaves 0-0.7 MB there (the current fast windows and all ten reference
  windows); a collapsed window leaves the leaf pinned, and the next window does not drain
  it for seconds. There is no quiescent interval in which stale state could clear.
- The "ramp" is the release from that state: 64 MiB drains during window 3, 32 MiB during
  the first half second of window 4 (its first sample is 6 Mbps, then 114 Mbps).
- The writeup's table shows sampled *peaks* and says the shared root is far from
  exhausted. Both true, but the peak column hides the boundary values, and the root is
  not the binding budget; the RX leaf is, in exactly the slow windows.
- The writeup says exact RX cuts are unavailable for the 64 MiB arm. The composite gauge
  above is in the 64 MiB report and is sufficient to show the carry-over.

So the five windows of one process are one trajectory, not five samples. Any analysis
that treats window N as starting clean (the writeup's connection-phase section does,
implicitly) will misattribute.

### 3.2 The mechanism, link by link (each link verified in source)

1. **Advertisement is in encoded bytes and equals the physical RX total.**
   `receiveWindowAdvertisement` returns `min(ReceiveQueueMaxByteCount, budget.TotalByteCount(), optional cap)`
   (`transfer.go:13396-13405`). The report confirms it: every window client has
   `ConfiguredAdvertisement = AdvertisementMinimum = AdvertisementMaximum = 2,181,038`
   with `PhysicalRxTotal = 2,181,038`.
2. **Physical RX is charged at retained cost, 2.93x encoded for a full pack.**
   `scanReceiveMemoryPack` charges metadata + one pool-class root per frame + the carrier
   root (`transfer_receive_memory_scan.go:144-162`, `:563`, `:824`). For the standard
   8 x 1280-byte H1 envelope that is 1,872 + 8 x 2,048 + 12,288 = **30,544** bytes for about
   10,412 encoded. The report's first refusal has `RetainedBytes = 30544, FrameCount = 8`.
   RX therefore holds exactly **71 full packs at 32 MiB** (71 x 30,544 = 2,168,624, the
   number BULKSPEEDFINALIZE's paused-worker test also found) and 142 at 64 MiB, i.e.
   about 0.74 MB / 1.49 MB of encoded data. The SDK comment at
   `sdk/mobile_memory_policy.go:132-140` acknowledges this unit mismatch and calls its cost
   "bandwidth after a loss, never occupancy". The measured cost is time, not bandwidth.
3. **The same full advertisement is given to every sequence sharing that RX.** All
   window clients are stamped with one receive budget (`sdk/device_local.go:5318-5319`).
   `rx_physical_latest` shows 6 clients and 12 receive sequences at 32 MiB, each
   advertising 2,181,038 against one 2,181,038-byte leaf. SIMPLERISSUE3 proved the
   two-sequence case; the single-sequence case is already overcommitted 2.9x.
4. **The sender's window opens to 2 MiB on the first ACK and never shrinks.**
   `estimate.Window = min(max(initial, floor), ceiling)` with `initial =
   ResendQueueMaxByteCount` = 2 MiB for a Desktop128 provider (`transfer.go:994`, `:13559`,
   `:13730`); the learned value is `max(window, candidate)` except inside an externally
   triggered quality-remeasure interval (`transfer_window_size.go:35-53`,
   `transfer_window_quality.go:45-81`). No timeout, refusal or resend feeds it. The
   delivery-based candidate (`transfer.go:13914-13920`), which in the collapsed state would
   land on the 256 KiB floor, is computed and then discarded as "retained window"
   (`transfer.go:13763-13764`). Admission compares
   with `arrival = 0` (`transfer.go:10438-10442`, `transfer_queue.go:285-287`), so the
   flight is window plus one original. The report's sender snapshots are exactly that:
   221 items / 2,098,841 bytes, later 234 items / 2,181,564 bytes, against a receiver that
   holds 71.
5. **There is no pacing.** `ApplicationPacing` is set false in defaults
   (`transfer.go:1055`), is never set true in non-test code in connect or sdk, and gates
   the whole pacer (`transfer.go:14564`). The 2,673-line pacing machinery and the
   uncommitted "ready train" work in the tree are dormant in the measured configuration.
   A window-limited sender writes its window at carrier speed.
6. **A full RX drops rather than back-pressures.** Funding order is ordinary RX, held-head
   repair, head recovery, fresh opening; if all fail the pack is dropped
   (`transfer_receive_memory_decode.go:97-115`). The handoff that follows is called with a
   literal zero timeout (`transfer.go:5192-5196`). `git show d90ac7f4` shows that line was
   `self.receiveBuffer.Pack(receivePack, handoffTimeout)` with
   `handoffTimeout = packHandoffTimeout(transportType, carrierReliability)`; under the
   mobile policy that is -1, "wait for capacity" (`sdk/mobile_memory_policy.go:213-218`,
   `:426-427`), now inert for the shared pump.
7. **The receiver tells the sender nothing about a refusal.**
   `requestReceiveMemoryPressure` only records local state and may reserve credit for a
   retry of that exact message (`transfer_receive_memory_pressure.go:42-76`, `:304`,
   `:355-358`). The reserved credit then waits for the sender's timer.
8. **The sender can only find out by timeout.** SACK-gap recovery needs three later
   selective ACKs and repairs four holes per pass (`transfer.go:954-957`, `:9013-9031`); a
   refused *tail* has no later SACKs, and a full RX cannot produce them. The timer is
   `min(ScaledRtt << (sendCount-1), 8 s)` with a 300 ms warm floor and 2 s cold floor
   (`transfer.go:914-922`, `:9204-9224`), plus up to two deferrals while cumulative progress
   continues (`transfer.go:981-982`, `:3314-3337`, `transfer_recovery_selection.go:328-352`).
   A non-head original refused on each retransmission accrues 0.3 + 0.6 + 1.2 + 2.4 + 4.8 =
   9.3 s before its fifth retry. When the floor finally reaches it, RX has room and the
   receiver may even hold reserved credit, but the sender's timer is seconds away. That
   sum is the same size as the 32 MiB window-2 probe's 9.5 s; I have not tied the probe to
   a specific original, so read it as a scale match, not an identification.
9. **One ordered sequence per provider for all download data.** The provider factory is
   not under the mobile policy, so `LogicalDataLaneCount` stays 0 (`transfer.go:1050`;
   lanes are set only by `sdk/mobile_memory_policy.go:472-486`). The first-refusal record
   has `Lane = 0` for provider-to-client DATA while the client's SYN identity has
   `Lane = 8`. Every flow on a provider, and any probe placed there, shares fate.
10. **Cancellation does not release anything.** Cancelled flows' originals stay in the
    provider's window and must still be delivered in order; TCP-socket-originated returns
    are retained past the ACK timeout (`ip.go:9590-9598`). That is the stale occupancy in
    section 3.1.

### 3.3 The arithmetic matches the collapsed rates

A window-limited sender delivers roughly one RX-full of packs per recovery cycle:

- 32 MiB: 71 packs x about 10.2 KB = 0.73 MB per about 0.3 s cycle = about 19 Mbps. Observed
  collapsed windows: 15.7, 12.3, and 4.8 with handshake stalls. Window 1 check: provider
  row 3 alone wrote 22.9 MB in 10 s (2.3 MB/s), i.e. one RX-full per 0.32 s.
- 64 MiB: 142 packs = 1.46 MB per cycle = about 39 Mbps per collapsed provider; windows 1-2
  ran 65-75 Mbps with two to six providers in mixed states.

In the clean state the flight is rate x round trip (about 27 MB/s x 50 ms = 1.35 MB at
217 Mbps), below the 2.18 MB window, so the sender is not window-limited and RX holds
only pipeline occupancy. That is why the same configuration can run at 200-400 Mbps for
whole windows and then fall to 5-15 Mbps: the fast state is stable only while the
receiver's queue never exceeds 71 (or 142) packs. Any excursion (a delivery stall of
about 25 ms at 27 MB/s, a carrier stall followed by a burst, synchronized bursts after a
head-of-line release) flips it, and nothing flips it back until the backlog drains.
This is consistent with two facts the earlier writeups could not reconcile: the exact
old executable collapsing when replayed on a busier day, and R13 (blocking) being fast
and then collapsing after a GC pause. Note that "not window-limited in the fast state"
is my inference from rate x round trip; the report has no sender snapshot from a fast
window (section 9).

The first refusal recorded in the 32 MiB run supports "overflow, not reorder": at
t = 0.675 s, with a single bulk flow active, ordinal 493 is refused with `Floor = 416`,
`IngressOwners = 78`, `HeldQueueItems = 0`, `PaidLaterOwners = 0`, RX 2,179,856 / 2,181,038.
Seventy-eight in-order packs were waiting for the receive worker; there was no hole yet.

### 3.4 Head-of-line fingerprints in the connection phases

Independent flows to different origins unblock in the same millisecond, which a
per-flow cause cannot produce:

- 32 MiB window 1: streams 5, 6, 7 (three origins) all get their first header byte at
  5,459 ms; streams 2 and 3 both at 2,513 ms.
- 64 MiB window 2: streams 2, 3, 6, 7 (provider row 3) finish TLS at 4,146-4,147 ms after
  dialing in about 41 ms; streams 0 and 4 (row 7) at 3,472 and 3,564 ms.
- 32 MiB window 2: provider row 3 took six of the window's eleven dial events; five bulk
  TLS handshakes never finish and the connection reused on row 3 gets no header, while
  both flows on row 2 progress. The stalled flows' provider is not recorded (they
  delivered no bytes), so "on row 3" is the likely reading, not a measured one. Row 3 in
  that window: 528 initial writes, 395 timeout resends, 936 return-send failures.

TCP connect succeeds quickly in these cases because SYN-ACK is now immediate; the first
DATA (ServerHello) is ordered behind the stale backlog. That is the "TLS interval of
123 ms to 4.553 s" in the writeup.

A second, separate fingerprint: **every dial of 2 s or more occurs in a window where the
bridge refused all-SYN batches**, consistent with the application stack's SYN
retransmission ladder (1 s, then 2 s more):

| Run | Window | SYN packets refused (all-SYN batches) / offered | RST packets offered | Slow dials |
| --- | ---: | ---: | ---: | --- |
| 64 MiB | 3 | 8 / 17 | 5,911 | streams 6, 7: 3,355 ms |
| 64 MiB | 4 | 2 / 12 | 7,117 | stream 0: 1,041 ms; stream 5: 2,178 ms |
| 64 MiB | 5 | 2 / 10 | 5,313 | streams 6, 7: 571 ms |
| 32 MiB | 3 | 1 / 6 | 2,576 | stream 7: 2,045 ms |
| 32 MiB | 5 | 2 / 10 | 5,018 | stream 6: 2,322 ms |
| either | 1 | 0 | 0 | none above 190 ms |

One exception in the other direction: 32 MiB window 4 has an 894 ms dial pair with no
bridge-level SYN refusal, so sub-second dial delay has at least one other source (my
guess: an admitted SYN ordered behind queued protocol originals).

After each cancellation the client stack emits thousands of RSTs (one per stale segment
arriving for a closed socket). They are ordered protocol packs competing with SYNs and
ACKs for three (32 MiB) or six (64 MiB) protocol slots per lane
(`transfer_logical_lane.go:42-47`, `transfer.go:7637-7642`, `sdk/mobile_memory_policy.go:199`,
`:352-363`). The capacity watcher is armed only for a group that is already a retained
SYN retry (`transfer.go:7998-8001`); BULKSPEEDFINALIZE itself notes "new never-admitted
SYNs are a separate case". This is a plausible source of the 2.4 s loaded-probe maximum
in the otherwise healthy 64 MiB window 5 (the probe's own phases are not recorded). I did
not trace the retained-SYN path end to end, so treat the code half of this as probable
rather than proven.

### 3.5 What changed since the 300-400 Mbps reference

| Quantity | Reference 2026-11-08 | Current candidate |
| --- | --- | --- |
| Pure-ACK packets refused at the bridge, whole run | 307 of 175,686 (32); 656 of 174,962 (64) | 49,303 of 53,223 (32); 77,909 of 86,299 (64) |
| Pure ACKs accepted per window | about 30,600-39,200 | 209-1,167 (32); 1,251-2,390 (64) |
| Maximum single bridge admission call | 1.95 ms / 1.72 ms | 5.99 ms / 34.88 ms |
| Client receive gauge at boundaries | about 0 | pinned at cap in slow windows |
| Client TX gauge peak | 9.3 MB (32), 18 MB (64) | 0.03-1.07 MB |
| Fastest single flow in a window | 161 MB | 78 MB |
| Pack handoff on full sequence | wait (policy -1) | drop (literal 0), d90ac7f4 |

Two observations follow.

First, the reference had the same RX sizing (its pack-queue capacity and head-recovery
budget read 2,181,038 and 4,362,076, the values the SDK derives from the same formula as
the RX leaf) and the same advertisement rule, so the 2.93x mismatch was latent then. What
most likely protected it was the blocking handoff: a reader that waits at a full
sequence stops decoding, so the outer carrier's TCP flow control becomes the real flow
control and the Transfer window is irrelevant. d90ac7f4 (2026-11-08 06:20; the reference
ran at 00:41 the same day) removed that protection for a legitimate reason (a busy
sequence must not stop the shared reader reaching ACKs) but nothing replaced it. The
R13/R14 ABBA in BULKSPEEDFINALIZE is consistent: R14 collapses immediately, R13 only
after a trigger, through spurious timeouts instead of refusals. I verified the diff and
the dates; I did not inspect the reference binary's source, so "the reference had the
waiting handoff" rests on that lineage.

Second, moving inner TCP ACKs to NoBudget plus reliable immediate delivery reduced the
ACK flight from several hundred budget-bound originals (the reference's client TX gauge
sits at 9.3 MB of a 9.35 MB budget; the gauge steps in 14,500-byte quanta, so roughly 640
small originals, my inference) to three or six receipt-gated originals per lane, with
zero-timeout refusal. The provider NAT has
no congestion control; it emits whenever the client's advertised TCP window allows
(`ip.go:6280-6307`). With one admitted ACK per flow every 33-68 ms (1,167 ACKs / 10 s / 8
flows at 32 MiB window 4) each admitted ACK releases a burst of roughly rate x interval:
about 7.6 MB/s x 68 ms = 0.5 MB = 50 packs for the fastest 32 MiB flow, against an RX that
holds 71. The reference's ACK cadence released about three packs at a time. I regard
this as a burst amplifier that consumes the margin, not as the root cause; see H2.

## 4. Gaps, unverified assumptions and likely-wrong conclusions in the writeup

1. **"No window-size root cause established."** The data in the cited report establish
   that the binding control is the client RX leaf, that the sender's flight is three
   times what it holds, and that the state persists across windows. What is not
   established is the trigger of the first overflow.
2. **Nothing shows the retry fix being exercised by the failing run.** Over the whole
   32 MiB run the bridge's ACK-progress observer reports `AdvancingAckRefusals = 44,264`,
   `ReopeningWindowRefusals = 0`, `SameCoreHeaderRefusals = 0`, `OtherComparedRefusals = 8`,
   and 4,909 refusals with no accepted baseline. The raw TCP window field in all eight
   critical snapshots is 3,948-4,096; no zero-window reopening candidate was observed. Two
   honest limits: that observer is marked `Incomplete`, and `PureAckRecoveredAttempts`
   reading zero means "not attached" in this collector, not "never ran". Even so, the
   retry's eligibility needs an admitted zero window, and I can find no sign of one. The
   58+58 qualification proves the mechanism in its fixture; it carries no information
   about Fast5.
3. **The next falsifier targets a condition the failing flows cannot be in.** The stalled
   window-2 flows completed TCP connect and are waiting for the first TLS flight (a few
   KB), or have just written a request on a reused connection. Their grants are not
   exhausted. A lost-feedback test that passes after the eligibility extension would not
   move this failure.
4. **"Window 2 was slow without new link queue drops, but could still inherit unresolved
   ownership/recovery from window 1."** This is stated as a possibility; it is directly
   measurable and true (section 3.1). It deserves to be the headline, not a caveat.
5. **"Windows 3-5 use the same aggregate provider bitset, but throughput still
   increases."** Window 3 at 64 MiB starts with RX full and row 7 still has 222 timeout
   resends; window 4's stream 5 dialed for 2.2 s and streams 6-7 spent 1.1 s in TLS;
   window 5 has two reused connections that are its two fastest flows (78 and 59 MB).
   The 112 -> 156 -> 235 progression is collapse release plus later flow starts plus
   connection reuse, not a single growing quantity.
6. **Treating the five named controls as symmetric.** From the code: the inner TCP
   advertised window is about 0.5 MB per flow (raw 4,019-4,025; scale 7 is my inference
   from the 1 MiB default receive buffer at `tun.go:151-155`, not measured) and shapes
   bursts; the Transfer send window is the constant in item 1; recovery is section 3.2
   item 8; receive backing is 71/142 packs; contract credit shows nothing unusual in the
   counters I looked at; the MultiClient provider window is unchanged (generation 1,
   target satisfied) though selection put all eight 32 MiB window-1 flows on one provider.
7. **The decision-time freshness guard from SIMPLERISSUE3 is not in the measured
   candidate.** The overlay binds `transfer_recovery_selection.go` to a donor path named
   `aggregate-core-reserve-immediate-r9/derived/OLD-FRESHNESS/` (byte-identical to the
   working tree), and `shouldDeferTimeoutResend` still uses the per-item predicate
   (`transfer.go:3332-3336`). SIMPLERISSUE3 proved that predicate adds a 2 s deferral to a
   refused head. If it was dropped deliberately, the writeup should say why.
8. **Probe diagnostics.** Agreed that the missing probe phases prevent naming the exact
   boundary. But the bulk flows on the same provider are an adequate proxy here, and the
   stronger missing diagnostic is sender-side: per provider-to-client sequence, queued
   bytes versus effective window and reason, head item age and `sendCount`.
9. **Reviewability.** The measured product is 138 overlay mappings over about twenty donor
   directories, with derived variants (`OLD-FRESHNESS`, `OLD-RECEIVER`,
   `OLD-PROTOCOL-COUNT`). The working tree lacks the retry file altogether. A reviewer
   has to reconstruct the source to check any claim; drift between "what was reasoned
   about" and "what was run" is a real risk at this point.

## 5. Root-cause hypotheses, ranked, each with a cheap discriminating experiment

**H1 (very likely, primary). Unit-inconsistent, non-adaptive flow control with
timeout-only recovery makes receiver-limited operation unstable.** Evidence: sections
3.1-3.3.

- Experiment E1, three interchangeable arms on the existing fixture, 32 MiB first,
  preferably with the fixed-provider treatment so one sender is isolated:
  - E1a: encoded cap `RxPhysicalEncodedCap` / `AdvertisedReceiveWindowMaxByteCount` =
    727,012 (RX/3), physical unchanged.
  - E1b: physical `RxPhysicalTotal` = 6,543,114 (3x; root unchanged at 21,810,380, of which
    about 19 MB is idle), cap unchanged. This is the parked RX2X arm taken to 3x.
  - E1c: provider retained-send limit (`retained_byte_count_per_provider`, the existing
    FixedProvider2MiB/4MiB treatment) at 512-700 KiB.
- Prediction: `all-funding-refused` near zero in all five windows; provider timeout
  resends near zero; receive gauge near zero at every boundary; no carry-over; loaded GET
  under 1 s; throughput flat across windows (E1a/E1c capped near cap / round trip, about
  115 Mbps per provider at 32 MiB; E1b uncapped).
- Falsifier: refusals persist at similar rates with one active sender and
  advertised x 2.93 <= physical. Then look at the handoff count/byte gates or the delivery
  callback instead.
- Two warnings. RX2X alone is predicted to fail (2 < 2.93), as the earlier +28% arm had
  to; do not read either as evidence against receive backing. With k providers active the
  condition is k x cap x 2.93 <= physical, so production selection needs either a per-sender
  share or cheap refusal recovery.
- Deterministic test T1: extend `testSharedPumpPausedWorkerRecovery`
  (`transfer_shared_pump_paused_worker_recovery_test.go`, already at real 32 MiB geometry)
  from a finite 1,720-packet run to a sustained source of at least ten windows with a
  consumer that is paused once or served at a fixed virtual rate. Assert goodput per
  virtual second and refusals per second. The finite version "falsified sufficiency of
  one burst" only because the source ran dry; the limit cycle needs a sender that refills.

**H2 (likely contributor). The ACK admission regime turns the provider into a burst
source at the scale of the RX buffer.** Evidence: section 3.5. Counter-evidence: the
flight16 cohort was no better and possibly worse.

- My reading of flight16: more admitted ACKs raise the offered load into a sender whose
  window is still 3x the buffer, so overflow gets worse; the three-slot limit is acting as
  an accidental rate limiter. That predicts ACK changes only help after H1 is fixed.
- Experiment E3: one diagnostic run with pure-ACK count limits lifted (or pure ACKs sent
  unacknowledged on H1), first with the current window, then combined with E1a. Read the
  fast-window RX baseline (today 0.4-0.9 MiB at 64 MiB, versus about 0 in the reference) and
  the largest per-flow volume.

**H3 (likely, explains probe tails in healthy windows). Cancellation aftermath: RST
floods starve SYNs of protocol slots, and stale data head-of-line blocks new flows.**
Evidence: section 3.4.

- Experiment: the existing `TestFullTunH1SyntheticFastCanceledRestartIos32/Android64`
  roots with two added scalars: receive gauge at restart, and restarted-flow time to first
  byte. Deterministic test T4: fill a lane's protocol slots with RSTs of a reset flow,
  offer a fresh SYN, release one slot, require the SYN to be written without an
  application retransmit.

**H4 (possible, secondary). The writeup's lost advancing inner-TCP feedback.** The
mechanism exists (section 6, Q1) and would add about 1 s NAT-timer stalls to
window-limited flows after bursty delivery. It is not evidenced in the failing window.

- Cheapest check before writing the falsifier: at a stall cut, on the provider NAT, is
  any flow at `receiveSeq - receiveSeqAck >= receiveWindowSize` with the Transfer queue to
  that client empty? If no flow is ever in that state during a failed window, drop H4.

**H5 (trigger, not cause). What tips window 1 over.** At 32 MiB the first refusal is at
0.675 s with one flow, 78 in-order packs waiting for the worker, and total GC pause in
that window of 0.26 ms, so stop-the-world GC is not it. Candidates: the window jumping
from the 320 KiB blind bet to 2 MiB on the first ACK (`transfer.go:13691-13704`) with no
pacing; the receive worker stalled in the delivery callback (the 6-35 ms bridge
admission maxima suggest the egress path can block, and inbound delivery and ACK emission
may share a call chain in the TUN stack); scheduling on a two-CPU co-resident process.
At 64 MiB window 1 there were also 1,408 access-link queue drops, i.e. a burst above the
2 MiB link queue.

- Instrument, bounded and passive: maximum and p99 duration of one receive-callback flush
  per window; maximum ingress backlog in packs. Compare with the old binary on the same
  day. Do not make fixing the trigger a goal; with H1 fixed the trigger is harmless.
- Experiment E2 (source-limited control): `OfferedBitsPerSecond` = 80e6 at 32 MiB and
  160e6 at 64 MiB, product unchanged. Prediction: no refusals, five flat windows at the
  offered rate. If collapse still occurs far below the client's capacity, H1's
  "receiver-limited" premise is wrong and the trigger is burst or cancel driven.

**H6 (unlikely). Contract-credit startup.** One arm with the initial contract set to the
standard size, as the paused-worker test already does. I expect no change.

**H7 (unlikely as cause, real as coupling). Provider selection.** Selection decides how
many flows share one lane-0 sequence; it does not create the refusals. Record the
per-window flow-to-provider map and rank weights; revisit after H1.

## 6. Direct answers to the writeup's questions

**Q1. Can finite lost advancing feedback leave the provider's TCP credit exhausted
while the retry's eligibility rejects the only useful update?**

Yes in principle. Eligibility returns early unless the last admitted window is zero and
the ACK number is unchanged (`ip_remote_multi_client_native_ack_retry.go:53-69` in the
donor), so a refused ACK that advances the frontier or extends a positive right edge is
never retained. The provider learns edges only from delivered ACKs
(`ip.go:7159-7215`). Protocol admission is first-wins: when the three or six raw slots of
the lane are held, the newest cumulative ACK is the one dropped
(`transfer.go:7956-8008`, `transfer_send_scheduler.go:76-88`), and slots recycle only on
terminal receipts (`transfer_immediate_option.go:26-28`). If delivery then pauses, the
client stack has no reason to emit another ACK, and the provider sits at a stale edge
until its 1 s resend timer replays one packet and elicits a duplicate ACK
(`ip_tcp_return_recovery.go:347-357`).

But it is not what failed here (section 4, items 2-3), and I would not extend
eligibility predicate by predicate. For cumulative feedback the correct policy is
latest-wins, which needs no predicate at all; see section 7.2.

**Q2. What owner/credit predicate distinguishes that from an ordered DATA hole,
retained-future debt or an uncompleted first TLS flight?**

Ask one question of the stalled flow: where is its next undelivered byte? Sample these
places at the unchanged 5 s cut:

| Location of the next byte | Predicate | Meaning |
| --- | --- | --- |
| Not yet read from origin; NAT blocked on TCP edge | `receiveSeq - receiveSeqAck >= receiveWindowSize`, retained chunks present, provider Transfer queue to this client empty, client's last *offered* right edge beyond the NAT's `receiveWindowEnd` | lost inner feedback (Q1) |
| NAT blocked on Transfer admission | provider sequence `queued bytes >= Window` | sender window full of other (possibly stale) data |
| In provider resend queue, sent, unacked | head item age > resend interval, `sendCount >= 2`; client `nextSequenceNumber` below that ordinal | ordered DATA hole / head-of-line |
| Refused at client | client RX used ~ total, final-refusal counter rising, `memoryPressure` pending or ready for that message id | retained-future debt / RX overflow |
| First TLS flight | NAT emitted only a few KB since SYN, window large, and those ordinals sit above the client's floor | head-of-line on the first flight, not a TCP credit problem |

Rows 2 to 5 are one condition seen from four places; only row 1 is the writeup's
hypothesis. The single most useful scalar per
provider-to-client sequence is `{queued bytes, Window, Reason, head age, head sendCount}`
from the existing `sendWindowSnapshot`, joined to the client's `{floor, highest received
ordinal, ingress owners, held items}` for that sequence id.

**Q3. Could cancellation/RST traffic and retained reliable ownership isolate providers
or flows without a shared-root shortage? Which ordering reproduces it?**

Yes, three ways, none needing a root shortage: stale originals occupy the provider's
window and the client's RX leaf (section 3.1); new flows' first bytes are ordered behind
them on lane 0 (section 3.4); RSTs take the protocol slots new SYNs need (section 3.4).

Deterministic ordering T2, on virtual time with barriers, no sleeps, no permanent loss:
(1) flow A on one signed provider-to-client lane-0 sequence fills the provider's window
while the client worker is parked, so the client refuses A's tail at real 32 MiB
geometry; (2) the client resets A and the provider NAT observes the reset; (3) flow B
opens (SYN and SYN-ACK complete immediately) and its single response pack is admitted
at an ordinal above all of A's; (4) release the worker. Require B's first payload within
a small multiple of the round trip. Current code should fail by at least one resend
interval per refused A original ahead of B, with backoff. Every A original is still
delivered or, in a fixed design, replaced by an empty tombstone, so custody is intact.

**Q4. Which measured control best separates receiver backing, Transfer recovery, TCP
congestion state, provider selection and contract-credit startup?**

E1 (any arm) crossed with E2, both under the fixed-provider treatment, read with the
exact counters rather than Mbps:

| Outcome | Interpretation |
| --- | --- |
| E1 removes refusals and resends; E2 also clean | receiver backing and window units (H1) |
| E1 has no effect; E2 clean | receiver-limited but not a units problem: look at handoff gates or delivery stalls |
| Neither helps | not receiver-limited: bursts or cancel aftermath (H2/H3) |
| Clean with fixed provider, not with production selection | multi-sender overcommit and selection coupling |

On "TCP congestion state": the provider NAT has none (it is a pure window sender) and
the carriers are reliable, so congestion state exists only on the origin edge and the
outer H1 connections; both already have retransmit counters in the report and neither
tracks the slow windows (64 MiB window 2: zero link drops, 163 retransmits, slowest
window). Contract startup: the H6 arm.

**Q5. Are there adjacent admission paths where failed or partial native groups lose the
feedback needed to release the resource blocking new handshakes?**

Yes. In order of importance:

1. The Transfer receiver's own refusal. The receiver knows the exact sequence number it
   dropped and may hold reserved credit for it (`transfer_receive_memory_pressure.go:304`),
   but never tells the sender. This is the feedback whose absence blocks new handshakes.
2. A fresh SYN refused at the protocol count is not retained (section 3.4).
3. Pure-ACK first-wins refusal at the same count (Q1).
4. Protocol-only bursts larger than the frame bound are chunked and, when a chunk is
   refused, the never-offered suffix is returned (working-tree `ip.go:1108-1137`). The
   suffix is the newest feedback. Same first-wins shape on another boundary.
5. Thousands of post-cancel RSTs are admitted as ordered protocol originals. Only the
   first per flow carries information.

## 7. Alternatives and simplifications

### 7.1 Make the receiver's window truthful, then make refusal cheap

- **Truthful advertisement.** The receiver already knows the cost model exactly; the scan
  computes it per pack. Advertise an encoded figure the leaf can actually hold: start
  with the static bulk ratio (RX / 3, the E1a arm), then divide by senders that delivered
  in the last few round trips rather than by all known sequences (SIMPLERISSUE3 showed
  78-91 known, 7-8 active). The sender already stores the latest advertised value,
  including decreases (`transfer.go:13335`, `:13693`), so this needs no wire change and
  works with deployed senders.
- **Reduce the ratio instead of the window.** Releasing the 12,288-byte carrier root once
  frames are copied (ratio 1.75) or slicing frames out of the carrier root instead of
  copying (ratio about 1.36) raises what the same leaf holds by 1.7x to 2.2x. I understand early
  carrier retirement was removed with the rejected memory model; it is worth re-measuring
  in isolation because it buys window without buying memory.
- **Use the root.** The RX leaf is 10% of a root whose sampled peak use is 11-13%
  (2.5 of 21.8 MB; 5.7 of 43.6 MB). A leaf of 30% makes today's advertisement safe for one
  window-limited sender. Managed credit is not process memory, so this needs the same
  device qualification as any budget change.
- **A refusal-ready notice on the existing eviction field.** ACKs already carry
  `EvictedSequenceNumbers` and the sender already resends on them
  (`transfer_pending_eviction.go:37-69`, `:142-151`), but only for selectively acknowledged
  items (`if item == nil || !item.selectiveAcked { return }`). Emit the refused ordinal
  when the receiver's pressure state becomes ready for it, and let a new sender treat a
  notice for an unacknowledged, un-SACKed item as "resend now, without backoff". Old
  senders ignore it by that existing guard. It is inherently rate-limited (one pending
  and one funded retry per sequence) and generation-scoped like evictions. The hazard
  SIMPLERISSUE3 raised (a late notice voiding a later SACK) costs at most one duplicate on
  an in-order carrier, or nothing if a separate field number is used. Predicted effect on
  the collapsed state: cycle time from at least 300 ms to about one round trip.
- **Do not accrue exponential backoff on receiver refusals.** Backoff is for path loss.
  At minimum, when a cumulative ACK makes an already-retransmitted, un-SACKed original
  the new head, send it immediately (the partial-ACK rule of NewReno). In a healthy flow
  the new head was sent less than a round trip ago, so the rule does not fire.
- **Let evidence shrink the window.** The delivery candidate already exists and is
  discarded. Allowing it to replace the learned value when a non-deferred timeout or a
  refusal notice occurred within the last round trip gives multiplicative decrease, with
  the existing doubling per round trip as recovery.

### 7.2 Inner TCP ACKs: latest-wins instead of bounded retry

A newer cumulative ACK supersedes every older one for the same flow. So keep at most
one pending pure ACK per flow, overwrite it on each refusal, and send it when a slot
opens. That is what the retry file already stores (one copy per flow, on the flow, drained
by the existing reaper), minus the eligibility predicate, which exists only because the
design is framed as recovering a specific lost event. Notes:

- Do not take the parent `stateLock` per refusal (`retainNativeAckRetry` does); at about
  1,000 refusals per second that is the hot-path cost the writeup worries about. A slot
  under the flow lock, with a reaper wake only on empty-to-pending, is enough.
- Consider whether pure ACKs need Transfer reliability on a reliable carrier at all. They
  are idempotent and regenerable; an unacknowledged latest-wins send needs no receipt,
  no slot and no retry, and removes the receipt-gated three-per-lane ceiling. The provider
  resend timer remains the backstop exactly as today.
- Order: after H1. On today's window, more ACKs mean more offered load into an
  overcommitted buffer.

### 7.3 Cancellation hygiene

- Deduplicate RSTs per flow at egress (first one, then at most one per interval).
- Give a fresh SYN precedence over a duplicate RST for a protocol slot, or retain a
  never-admitted SYN the way an admitted one is.
- When the provider NAT sees a reset for a flow, replace that flow's queued and unacked
  originals with empty packs at the same ordinals. The repository already rewrites
  originals in place for held-head repair. This removes both the stale backlog and the
  source of the RST flood. It also matters outside the benchmark: a browser cancelling
  assets on navigation is the same event.

### 7.4 What I would not do

- Revert d90ac7f4 as the fix. Blocking hides the mismatch and reintroduces the stall it
  was written to remove; R13 collapsed too.
- Enable provider lanes before the advertisement is truthful. Eight lanes are eight full
  advertisements against the same leaf.
- Turn on `ApplicationPacing` as the fix. A correct window makes it unnecessary for
  safety, and it is a large, currently unexercised surface.
- Use Fast5 Mbps as the discriminator for any mechanism change. Use
  `all-funding-refused`, provider timeout resends, the boundary receive gauge and probe
  latency. A stable configuration shows zero for the first three in every window.

## 8. Prioritized next steps

1. **Add one invariant to the Fast5 verifier (minutes):** at each window boundary with
   requests quiescent, the client receive gauge must be near zero, otherwise the window is
   labelled "started with stale receive occupancy". This turns section 3.1 into a
   standing check and stops slow windows being read as independent samples.
2. **Run E1** (one arm is enough to start; E1a needs no new code) at 32 MiB with the
   fixed-provider treatment, then with production selection. Read the three counters.
3. **Run E2** (offered-rate control) once per profile. Together with E1 this settles
   Q4.
4. **Write T1** (sustained-source paused/slow consumer) on the existing synctest
   fixture. It should fail on current code by entering the cycle and gives every later
   change an old-fails/new-passes gate that does not depend on host load.
5. **Product, smallest first:** truthful advertisement; refusal-ready notice plus
   no-backoff-on-refusal; then the head retransmit on cumulative advance as the
   legacy-peer fallback. Re-run T1, then Fast5 and full100 at both profiles.
6. **Then cancellation hygiene** (T2, T4): RST dedupe and fresh-SYN handling first, stale
   elision second.
7. **Then ACK cadence** (E3, T3): generalize the retained slot to latest-wins, or measure
   unacknowledged ACKs on H1. Retire the eligibility predicate rather than widening it.
8. **Housekeeping that affects trust in results:** state whether the SIMPLERISSUE3
   freshness guard is in or out and why; land the measured product on a branch so that
   the reviewed source is the run source; keep the planned probe-phase capture but add the
   sender-side scalars from Q2, which are the ones that would have named this.

## 9. Caveats on my own analysis

- I executed nothing. E1's prediction is arithmetic from verified constants; it could be
  wrong if another gate binds first, which is why it has an explicit falsifier.
- The inner TCP window scale (hence the 0.5 MB figure and the per-ACK burst estimate) is
  inferred, not measured.
- The claim that the sender is not window-limited in fast windows is inference from
  rate x round trip; the only sender snapshots in the report are from refusal instants and
  from the cancel instant.
- The fresh-SYN path was read only at the admission site; the correlation with slow dials
  is from data.
- H2's causal role is unproven and the flight16 cohort argues against treating it as
  primary.
- The reference comparison spans commits ba795437 and d90ac7f4 plus private donors; I
  attribute to the two changes I could see in code and counters, not to an exhaustive
  diff.
- All throughput figures are N=1 on a busy host. The boundary gauges, exact refusal
  counts and phase timestamps do not depend on host noise in the way Mbps does, which is
  why the argument rests on them.
