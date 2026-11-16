# WINDOWSIZE2 — independent review 2

2026-11-11. Reviewer 2. Written without reading any `*-REVIEW*.md` or `*-META.md`
file (including `WINDOWSIZE2-REVIEW1.md`, which appeared while I worked).

## 0. What I reviewed and what I ran

- **Writeup version.** `WINDOWSIZE2.md` as committed in `391c679c`: **206 lines**
  (sha256 `64f21a92…7942a`). The file was edited while I worked: an uncommitted
  section "Resumed post-merge investigation" brought it to 257 and then
  **259 lines** (01:14 local). I read that addendum too and comment on it in
  §4.9 and §7, but the body of this review targets the 206-line text.
- **Working tree changed under me.** At the start `git status` was clean. By
  01:05 it showed an uncommitted edit to `transfer.go` (`tryDeepenH1PackQueue`
  gains a `physicalBytePermission` parameter) and an untracked
  `transfer_shared_pump_small_group_count_test.go`. I did not touch either.
  **Line numbers below are for committed `HEAD` (`391c679c`)**; the working-tree
  `transfer.go` is shifted by +1…+5 lines after line 16881.
- **Background read:** `WINDOWSIZE.md` (full). Developer notes the addendum
  points to: `tests/research/resume-20261111/{RESUME.txt,ADAPTIVE-GROWTH.md,full10064-page1-existing-evidence.json}`.
- **Artifacts analysed:** `tests/testdata/window-size-checkpoint-95b8/` (matrix,
  archived fixture source, pending witness) and the raw run under
  `/Volumes/data2/urnetwork/temp/aggregate-95b8-retained-performance-diagnostic-r1/run/`
  (`fast32.stdout`, `fast64.stdout`, `full10032.stdout`, `full10064.stdout`,
  the stderr logs, `CONFIG.json`, `RUN-PERF.sh`).
- **Code read:** connect `transfer.go`, `transfer_send_scheduler.go`,
  `transfer_logical_lane.go`, `transfer_immediate_*.go`, `transfer_window_pacing.go`,
  `transfer_window_latency.go`, `transfer_window_size.go`,
  `transfer_receive_memory_decode.go`, `ip.go`, `ip_tcp_return_recovery.go`,
  `ip_native_ingress.go`, `ip_remote_multi_client*.go` (send path, affinity SYN,
  SYN retry, native ACK retry, collapse), `tun.go`; sdk
  `device_local_transfer_memory.go`, `mobile_memory_policy.go`; the gVisor fork
  (`reno.go`, `snd.go`, `protocol.go`); the perfvar fixture (bridge, counters,
  carrier construction).
- **What I ran: no Go builds and no tests.** Only read-only parsing/regression
  scripts over the existing JSON reports (kept in
  `/private/tmp/claude-501/-Users-builder-urnetwork-connect/5b991e4a-8699-464e-a6d4-9e0a0403d954/scratchpad/windowsize2-review2/scripts/`).
  Reasons: the host load average was ~150 on 14 cores at review time, and the
  package was being edited concurrently. Everything marked *verified* below is
  verified against the recorded data or by reading code, not by a new run.

Legend: **[V-data]** verified in this run's artifacts; **[V-code]** verified by
reading code; **[I]** inference that fits the data but is not proven;
**[S]** speculation.

## 1. Bottom line

1. **The W3 dip is not a receive-window, receive-backing or handoff-count
   event.** Every receive refusal/drop of the 32 MiB run happened in window 1.
   Windows 2–5 have zero `PackHandoffDrop`, zero Head-recovery reservations and
   `PackHandoffSaturationCount = 0`; the whole 64 MiB run has zero of each, yet it
   varies 198–275 Mbps too. [V-data, §3.2]
2. **The sparse-group / adaptive-count work (now in the working tree) cannot be
   the deterministic reproduction of this sequence.** The count limit was never
   reached in either run (max depth 182/256 and 153/512), and no shipping
   constructor enables adaptive growth, so the corrected helper branch is
   unreachable in product configurations. It is a valid latent-code fix; it should
   not be counted toward the Fast5 objective. [V-data, V-code, §4.9]
3. **What actually varied is each provider path's delivery rate**, and the
   split is fixed by sticky server-name affinity: from W2 on, origin0 → provider
   row 1 (2 flows), origins 1–3 → provider row 6 (6 flows). Provider 6 delivered
   181 / **49.5** / 114 / **45.0** MB in W2..W5; provider 1 delivered 205 / 128 /
   182 / 231 MB. W5 has the same provider-6 collapse as W3; it is hidden by
   provider 1's best window. [V-data, §3.1]
4. **The recoveries are additive and scale with the number of providers**
   (≈5–7 Mbps/s per active provider, R² 0.99 in the cleanest windows), which is
   the NewReno congestion-avoidance slope `MSS/RTT²` = 7.2 Mbps/s for one TCP
   connection at 40 ms. The fixture's H1 carriers are gVisor TCP with the default
   **Reno**, `ssthresh = FlightSize/2`, and slow-start restart after idle. A 10 s
   window is about one Reno recovery time at this BDP, so 2:1 window-to-window
   variation is the expected behaviour of the *fixture's outer TCP*, not evidence
   of a Transfer sizing defect. [I, strong; §3.4, §5 H1]
5. **The run is CPU-bound in its fast windows.** The tests run with
   `-test.cpu=2`; the process used 1.51 cores on average and ≈151 Mbit per
   core-second, i.e. a ≈300 Mbps ceiling — W2's 308.8. [V-data, §3.6]
6. **Two product-relevant defects are visible in the same data and are not in
   the writeup:**
   - *Connection setup starves under bulk ACK load.* 11 of 53 SYN offers (32 MiB)
     and 17 of 60 (64 MiB) were refused at native ingress; in 3 of the 10 windows
     a bulk flow needed ≥7 s to connect or never connected (one shows the exact
     1 s+2 s+4 s SYN backoff). The 5 s gate did not see them only because the
     probe usually rides a warm connection. Same failure class as the W2 stall in
     `WINDOWSIZE.md`. [V-data; refusal site V-code-plausible, §3.5]
   - *Standing queue sized by the window, not by the path.* The sender keeps
     `min(advertised credit, Σ inner TCP windows)` in flight regardless of path
     rate — the full 2.18 MB credit at 32 MiB with five or six flows — so
     latency for anything new on that provider is `in-flight ÷ rate`. This
     predicts, window by window, where `TimeoutResendDeferCount` is nonzero, and
     fits the 64 MiB loaded GETs being 2–5× slower than the 32 MiB ones. A bigger
     window makes this worse, not better. An opt-in bound for exactly this
     already exists (`WindowLatencyBudget`) and is off everywhere.
     [V-data, V-code, §3.3]
7. **The measured aggregate is already stale.** Upstream's pacer fix
   `116826e2` ("Bound compressed-turn service decreases by exact heads") is not
   an ancestor of `99c9d2a4`. Re-baseline on merged main before explaining more
   of the old aggregate. [V-code, §3.7]
8. **Recommended path:** stop treating per-window Mbps on this fixture as a
   product signal until three confounders are removed (carrier CC, CPU, affinity
   skew); reproduce the W3 shape deterministically with one scripted drop;
   fix SYN/control admission and bound the standing queue; treat the receive-side
   work as done-for-now robustness. Details in §9.

## 2. My understanding, and where I agree or disagree

The writeup reports a diagnostic replay of aggregate `95b8…` (Connect
`99c9d2a4`): both synthetic Fast5 profiles pass their gates, the 32 MiB sequence
reads 268.5 / 308.8 / 141.9 / 236.8 / 220.4 Mbps, and the question is whether an
avoidable protocol or scheduling limit explains window 3. It records that
earlier receive-side mechanisms (retained backing, handoff count 85→256) were
real, proposes a further count-pressure hypothesis for one-frame originals, and
asks six questions.

**Agree**

- The epistemic discipline: aggregate counters cannot order events; one busy-host
  process is not a baseline; 32-vs-64 does not isolate memory sizing.
- The static source findings about the handoff: count and bytes are independent
  (`transfer.go:17079`), nonzero lanes divide count by eight
  (`transfer.go:16732-16743`, `transfer_logical_lane.go:42-47`), the growth helper
  re-checks a local byte cap the caller already waived (`transfer.go:16938-16939`
  vs `:17107`), a threshold above one refuses the first saturated offer
  (`transfer.go:16908-16913`), and mobile policy zeroes all adaptive fields
  (`sdk/mobile_memory_policy.go:406-411`). All correct as code reading.
- The sizing arithmetic: `ceil(7L/2)` (`sdk/device_local_transfer_memory.go:16-26`),
  256/512 (`:28-32`), 2/7 advertisement (`:132-133`, `transfer.go:13478-13496`).
- Q5's caution that reproducing ordinary TCP slowdown is not a URnetwork bug.
  I think the data already shows that signature (§3.4).

**Disagree**

- "Exact per-window refusal totals are unavailable." They are derivable from the
  same report and are zero after W1 (§3.2).
- That the count hypothesis is "adjacent" to this dip. It is unrelated to it.
- "The dial distribution changed from 3/5 to 2/6" as a W3 observation. The
  origin→provider mapping is identical in W2..W5; the dial counts differ only
  through connection reuse and through a W2 flow that never finished its TCP
  connect (§4.3).
- Treating "all gates passed" as the state of small-request health (§3.5).
- The implied framing that the remaining problem is about windows being too
  small. Where window size matters in this data, the larger window is the
  harmful one (§3.3).

## 3. What the run's own artifacts say

### 3.1 The dip is a per-provider path rate; placement is fixed by affinity [V-data]

Per-flow `provider_mask` and bytes from `$.sequence.runs[*].workload.flows`
(app-validated MB; flow count in parentheses), with the per-window deltas of the
per-provider counters:

| 32 MiB | Mbps | prov 1 MB | prov 6 MB | prov-6 flow rates, Mbps | prov-6 `TimeoutResendDefer` | SYN offered/refused | RST offered/refused | app-TCP timeouts | link drops | retransmits edge / p1 / p6 carriers | receive drops |
| --- | ---: | ---: | ---: | --- | ---: | ---: | ---: | ---: | ---: | --- | ---: |
| W1 | 268.5 | 184.2 (4) | 151.3 (4) | 30–43 | 370 | 9 / 0 | 3 / 3 | 0 | 314 | 564 / 250 / 233 | 804 |
| W2 | 308.8 | 205.0 (2) | 181.1 (5) | 32–36 | 0 | 15 / 6 | 2882 / 1620 | 0 | 0 | 451 / 284 / 344 | 0 |
| W3 | 141.9 | 128.0 (2) | **49.5 (6)** | **8.0–12.4** | **697** | 11 / 3 | 4272 / 3011 | 2 | 25 | 46 / 39 / 13 | 0 |
| W4 | 236.8 | 181.8 (2) | 114.1 (6) | 16.6–22.3 | 0 | 8 / 0 | 2861 / 1607 | 0 | 0 | 128 / 236 / 159 | 0 |
| W5 | 220.4 | 230.6 (2) | **45.0 (6)** | **7.7–10.2** | **535** | 10 / 2 | 2989 / 1892 | 4 | 0 | 77 / 152 / 84 | 0 |

Readings:

- **Placement.** W1 (cold, raced): 4/4. From W2 on the mapping is origin0
  (flows 0, 4) → provider 1 and origins 1–3 (flows 1, 2, 3, 5, 6, 7) → provider 6,
  unchanged through W5. That is the server-name/cluster affinity
  (`servernameaffinitybridge=1 affinitymaxtime=60000` in the session line of
  `fast32.stderr`), not a per-window scheduling decision. Four of the six
  evaluated providers carry no bulk traffic at all.
- **Equal shares.** Flows on one provider get near-equal rates (W3: 8.0, 8.0,
  8.0, 8.4, 9.7, 12.4; W4: 16.6–22.3; W2: 32–36). Six flows are dividing one
  rate-limited FIFO path, each limited by the same advertised TCP window (raw
  4019 in the bridge's ACK snapshots; ≈514 KB at window scale 7, which is what
  a 4 MiB maximum buffer negotiates and what the ~100 Mbps single-flow ceiling
  implies). They are not individually slow; their shared path is.
- **W5 is W3 again on provider 6** (45.0 MB vs 49.5 MB, 535 vs 697 deferrals,
  second-scale connects, app timeouts) with *zero* link queue drops. Chasing the
  25 drops of W3 (Q1) will not explain W5.
- **Retransmit totals mislead.** The "TCP retransmits" column of the writeup
  sums four stacks. W2, the fastest window, has 1,079 with zero link queue
  drops; W3 has the fewest. Both "TCP timeouts" of W3 (and all four of W5) are
  on index 0, the *application* TUN (`synthetic_fast_native_test.go:956,972-976`
  orders `[appTun, deviceCarrier, provider carriers…]`); every carrier and the
  edge have zero timeouts in every window.
- **The provider that serves flow 0 grows across windows.** Flow 0 starts at
  t=0 on a reused connection, so that provider's carrier never idles:
  32 MiB provider 1 goes 136.9 → 192.8 → 244.2 MB (W3→W5, Transfer bytes);
  64 MiB provider row 2 goes 104.5 → 154.3 → 207.8 MB. +50 MB per window in two
  independent runs. This is state carried across windows (§3.4).

### 3.2 Receive memory and handoff count were inactive after window 1 [V-data, V-code]

`$.production_constructor_policies.receive_backing_window_cuts[w][0|1]` holds
before/after cuts for every window, including per-generated-client receive
stats and the budget counters:

- Per-client `PackHandoffDropCount` deltas: W1 = 575 (provider-1 client) +
  224 (provider-6 client) + 5; **W2..W5 = 0 for every client**.
- Every final funding refusal increments that counter
  (`transfer_receive_memory_decode.go:109-114`), so final refusals in W2..W5 are
  exactly zero.
- Head budget `ReservedByteCount`: +488,704 in W1 (= 16 × 30,544, the 16
  head-recovery fundings), **+0 in W2..W5**. So all 642 ordinary-RX refusals
  (626 final + 16 recovered) are W1 events.
- `PackHandoffSaturationCount = 0` and `PackHandoffDepthGrowCount = 0` for every
  client at every cut, in both profiles. That counter is bumped on every H1
  count- or byte-limit hit (`transfer.go:16891`, called from `:17079-17090` and
  `:17107-17118`). **The 256/512 count limit was never reached in either run.**
- `PackHandoffMaxCount`: 182 and 103 (of 256) at 32 MiB, both set in W1 and never
  exceeded; 153 (of 512) at 64 MiB.
- 64 MiB: zero drops, zero refusals, zero saturations for the whole run, with
  windows of 214 / 259 / 198 / 267 / 275 Mbps and a 2.53 s GET.

Two side observations from the W1 episode, since it is the only place the
receive mechanism was live:

- 804 drops vs 626 final funding refusals leaves **178 drops with another
  cause** (not count/byte saturation, which is zero). Candidates in code:
  `packMutex.TryLock` busy, `bindReceiveMemoryOwner` false, lifecycle paths at
  `transfer.go:16162/16227`. These may be the *initial* gap that the funding
  refusals then amplified. Worth a reason code.
- The first refusal has `Rx.Used = 7,609,728` of 7,633,633 with **two**
  providers active. See §4.6: the backing covers one sequence's advertisement,
  not two.

### 3.3 In-flight is set by windows, not by the path; latency = in-flight ÷ rate [V-data, V-code]

Mechanics in code:

- The provider's inner TCP toward the client has **no congestion window**. It
  emits whenever the client's advertised receive window has room
  (`ip.go:6288`); recovery is one packet per partial ACK or a 1 s timer
  (`ip.go:494-496`, `ip_tcp_return_recovery.go:287-292`). A new flow can put a
  full ~514 KB into the provider's return path in its first round trip.
- The Transfer send window is the learned window clamped by the peer's
  advertisement (`transfer.go:13784`); the learned window only grows unless a
  remeasurement is requested (`transfer.go:13646-13649`,
  `transfer_window_size.go:36-52`).
- The advertisement is a constant per sequence: "follows capacity, never
  current free bytes" (`transfer.go:13478-13496`). Every generated client
  reports `AdvertisementMinimum = AdvertisementMaximum = 2,181,038` (4,362,076
  at 64 MiB).

So the amount in flight on a provider is `min(advertised credit, Σ inner TCP
windows)`, independent of how fast the path is draining, and the Transfer
sojourn time is `in-flight ÷ rate`. Each inner window is ≈0.5 MB on a fresh
connection and auto-tunes toward ≈2 MB (the reused connections advertise
16307≪7).

*32 MiB, where the credit binds* (five or six flows, Σ windows > 2.18 MB). The
resend floor with RTT samples is 300 ms (`transfer.go:921`), so timer firings
("deferred because cumulative delivery advances") should appear exactly when a
provider's rate is below `2.18 MB / 0.3 s` = **58 Mbps, i.e. ≈73 MB per
window**:

| Provider-window | Transfer MB | Predicted | `TimeoutResendDeferCount` |
| --- | ---: | --- | ---: |
| 32 p6: W2 / W3 / W4 / W5 | 193 / 56 / 123 / 51 | no / yes / no / yes | 0 / 697 / 0 / 535 |
| 32 p1: W2 / W3 / W4 / W5 | 217 / 137 / 193 / 244 | no ×4 | 0 ×4 |

Eight of eight. (W1's 370 on provider 6 is the refusal episode.)

*64 MiB, where either term can bind* (one to four flows per provider against a
4.36 MB credit). The threshold then sits somewhere between ≈55 and 116 Mbps
depending on flow count and auto-tuning. The data is consistent with that band,
not a sharp test of it:

| Provider-window | Transfer MB (flows) | `TimeoutResendDeferCount` |
| --- | --- | ---: |
| row 2: W1..W5 | 79 (3) / 108 (2) / 105 (2) / **154** (2) / **208** (2) | 276 / 702 / 325 / **0** / **0** |
| row 3: W1..W5 | 60 (1) / 47 (2) / 63 (2) / 133 (4) / 75 (4) | 212 / 464 / 41 / 55 / 315 |
| row 0: W1..W5 | 107 (3) / 77 (2) / 101 (4) / 72 (1) / 89 (2) | 459 / 33 / 449 / 0 / 253 |
| row 1: W1 / W2 | 44 (1) / 112 (2) | 179 / 0 |

The two provider-windows above 120 Mbps have none. Of the fifteen slower ones,
ten have 179–702, three have 33–55, and two have none — both with only one or
two fresh connections, i.e. little in flight.

The same model explains loaded-GET latency, which is one queue transit per
request on a warm connection:

- 32 MiB: 2.18 MB ÷ 130–200 Mbps ≈ 90–130 ms. Observed probes: mostly 80–150 ms.
- 64 MiB: 1–4.4 MB ÷ 55–110 Mbps ≈ 70–630 ms. Observed W1 probes: 205, 71,
  **1481, 906, 794**, 406, 583, 861 ms; later windows 130–600 ms.

It also accounts for the second-scale phases of late flows on the slow provider:
32 MiB W3 flows 5–7 have dial 1.06–1.78 s, TLS 0.96–1.18 s and request→header
0.57–0.99 s; W5 flows 5–6 have dial 945 ms, which is *below* a 1 s SYN
retransmit, so that one is pure queue transit. The SYN-ACK is "immediate" at the
Transfer reorder layer (`ip_immediate_ack.go:18-20`) but still rides the same H1
byte stream behind the standing queue.

Consequences:

- The 64 MiB profile, with twice the credit and its traffic spread over three
  or four slower provider paths, did not reach a higher peak than 32 MiB (both
  near 360–380 Mbps) and has 2–5× the loaded-GET latency. (Its 2.53 s worst GET
  is a different mechanism, §3.5.)
- If a provider path drops to ~5 Mbps (as in `WINDOWSIZE.md` 32 MiB W2, 4.79
  Mbps), one transit is `2.18 MB ÷ 5 Mbps ≈ 3.5 s`; a cold connection needs about
  three transits. That is a 10 s handshake with no packet lost anywhere. I think
  this, not a lost ACK, is the general shape of the "TLS never finishes, GET
  never completes" failure. [I]
- Reverse coupling: client→provider pure ACKs are "immediate" packs limited to
  `immediateSendLimit = ceil(SequenceBufferSize/8)` outstanding per lane
  (`transfer.go:7567-7573,7645`, `transfer_immediate_option.go:26-28`), and a slot
  is returned only when the Transfer receipt comes back
  (`transfer.go:12820-12822`, `payload_owner_ledger.go:158-164`). That receipt
  travels provider→client **behind the same standing queue**. So when the
  forward path is bloated, the inner-TCP ACK clock of every flow on that
  provider slows by the same sojourn. [V-code; magnitude I]

An opt-in fix for the forward half exists and is unused:
`SendBufferSettings.WindowLatencyBudget` (`transfer.go:5667-5672`,
`transfer_window_latency.go:1-28,142-197`, commit `91850a92`, in the measured
checkpoint). Its header describes this exact pathology on a production provider
("eight parallel downloads to one client … every packet to that client,
datagrams included, waited about seven seconds") and says the bound "held the
window near 4 MB and the datagram round trip near 250 ms, with the same
throughput"; the field comment names 50 ms as the budget used. No non-test code
in connect, sdk or server sets it.

### 3.4 Recoveries are additive at about `MSS/RTT²` per provider [V-data for the numbers; I for the cause]

Least-squares slope of the 500 ms throughput samples for t ≥ 2.2 s
(`workload.samples`):

| Run / window | active providers | slope, Mbps/s | R² | per provider |
| --- | ---: | ---: | ---: | ---: |
| 32 W3 | 2 | 13.5 | 0.99 | 6.8 |
| 32 W4 | 2 | 12.1 | 0.88 | 6.1 |
| 64 W1 | 4 | 19.2 | 0.93 | 4.8 |
| 64 W3 | 3 | 17.6 | 0.99 | 5.9 |
| 64 W4 | 3 | 14.6 | 0.88 | 4.9 |

(The other windows are plateau-then-dip or ramp-then-decline and fit no line.)
NewReno congestion avoidance for one connection with MSS 1448 grows at
`MSS·8/RTT²` = 7.2 Mbps/s at 40 ms, 6.6 at 42 ms, 5.7 at 45 ms.

Why I think the carrier TCP is the governor:

- The fixture's carriers are gVisor stacks built by
  `clientconnect.CreateTun(DefaultTunSettingsWithBufferSize(4096))`
  (`perfvar/route_test.go:707-713`), and `tun.go:281` registers
  `tcp.NewProtocol`, which is Reno in the fork
  (`gvisor/pkg/tcpip/transport/tcp/protocol.go:575-577`). No file in the
  perfvar package sets a congestion-control option (grep for
  `CongestionControl`, `cubic` is empty) [V-code].
- Each provider reaches the client through its own generated client and H1
  transport (`PlatformTransportUsedCount: 7` for six window clients plus
  control), so I take it there is one edge→client TCP connection per provider,
  RTT 40 ms [I].
- At 150 Mbps and 41 ms, cwnd ≈ 530 segments; one halving takes ≈265 round
  trips ≈ **10.9 s** to undo. The window length equals the recovery time
  constant, so window averages between 0.5× and 1.0× of peak are expected from
  sawtooth phase alone. That is the observed 142–309 range.
- `reduceSlowStartThreshold` sets `ssthresh = Outstanding/2`
  (`reno.go:72-78`): a loss detected while little is in flight (window
  boundaries, handshakes, a path draining after cancellation) collapses ssthresh
  to a few segments. `sendData` also resets cwnd to 10 after an idle period
  longer than RTO (`snd.go:1113-1120`). Together these make "start the window
  near zero and climb linearly" a normal outcome for any provider whose first
  flow starts at 0.5 s. A ramp from zero at 7.24 Mbps/s for 10 s delivers 45 MB;
  provider 6 delivered 49.5 MB (W3) and 45.0 MB (W5).
- A zero-parameter model — provider-6 rate = 7.24 Mbps/s × (t − first payload),
  shared equally among started flows — gives, for the three late flows,
  W3: 5.78 / 4.84 / 5.84 MB vs observed 5.77 / 4.76 / 5.87;
  W5: 6.33 / 6.23 / 4.66 vs 6.46 / 6.31 / 4.25.
  Caveat: early flows are under-predicted (they ran before the collapse), and a
  flat ~39 Mbps shared path also fits the six W3 numbers within ~10%. The
  per-flow fit proves equal sharing of one limited path; it only suggests the
  ramp.
- The multiplicative alternative (the Transfer pacer's ×1.1 probe,
  `transfer_window_pacing.go:291-340`) predicts growth proportional to the level.
  The two high-level windows (32 W4 at 217→304, 64 W4 at 243→348) grow at 12–15
  Mbps/s, not 25–30. Percentage growth varies 4.8–9.9 %/s across the five
  windows; absolute per-provider growth varies 4.8–6.8. Additive fits better.

What I cannot show from the recorded data: cwnd/ssthresh themselves, which link
dropped, or what triggered each loss episode. §5 lists the experiments.

### 3.5 Native ingress: SYNs and RSTs compete with a saturating ACK stream [V-data; site V-code-plausible]

Bridge counters (`bridge_admission`, per-window deltas of `Before/AfterDetails.Bridge`):

- 92–95% of everything the app TUN emits is refused by `SendPacketBatch`:
  32 MiB 153,888 of 166,201; pure-ACK-only batches 145,350 refused. Accepted
  packets are only 200–330 per second in total.
- SYN-only batches refused: 32 MiB **0 / 6 / 3 / 0 / 2**, 64 MiB **1 / 0 / 5 / 9 / 2**
  per window (lower bounds: only all-SYN batches are attributed). W1, where new
  flows are raced across providers, has almost none; windows with established
  affinity have many.
- RSTs offered per window after W1: 2,861–4,272 (32 MiB), 2,051–6,816 (64 MiB),
  55–70% refused. These are replies to data that keeps arriving for the bulk
  connections cancelled at the previous window boundary.

Longest TCP connect of any flow, per window (`OwnedDialStart`→`OwnedDialDone`):

| | W1 | W2 | W3 | W4 | W5 |
| --- | ---: | ---: | ---: | ---: | ---: |
| 32 MiB | 0.25 s | **never (>8.5 s), flow 7** | 1.78 s | 0.29 s | 2.57 s |
| 64 MiB | 2.26 s | 0.32 s | **7.23 s, flow 6** | **never (>8.5 s), flow 7**; probe 2.53 s | 2.63 s |

7.232 s is 1 s + 2 s + 4 s of SYN backoff plus 232 ms: three consecutive SYNs
went nowhere and the fourth connected at once. Under a 5 s limit on *any*
flow's connect, 3 of these 10 "passing" windows fail. The existing gate samples
one small GET every ~0.6 s, almost always on a warm connection.

Code path that fits [V-code]:

- Native ingress is a single zero-timeout offer; a refused packet is "the source
  TCP's recovery responsibility" (`ip_native_ingress.go:11-13`,
  `ip_remote_multi_client.go:7908-7912`).
- As I read `sendAffinityClientPath`, a SYN whose flow resolves to a live
  affinity selection is offered only to that bound provider, and "a selected
  client owns this attempt even when bounded admission reports backpressure" —
  no race fallback (`ip_remote_multi_client_affinity_syn.go:86-104`, and the
  comment after `sendBoundClient` in `sendParsedPacketGroupAttempt`). I did not
  trace the destination→exit lookup itself; the per-origin stickiness in §3.1 is
  the evidence that it applies here.
- Zero-payload SYN, FIN, RST and pure ACK are all "protocol-only"
  (`transfer_protocol_classification.go:173-182`) → `NoBudget`
  (`ip_remote_multi_client_protocol_control.go:8-17`) → the lane sequence's
  `protocolAdmission`, capacity `ceil(SequenceBufferSize/8)`
  (`transfer.go:7645-7649`; the mobile cap is a scaled 16,
  `sdk/mobile_memory_policy.go:199`, so two or three slots per lane —
  `WINDOWSIZE.md` reports three), with **no per-flow reservation**: "A protocol
  Pack has no per-flow slot restriction" (`transfer_send_scheduler.go:90`,
  `transfer.go:7982-7986`).
- Queued immediate ACK packs cannot leave the scheduler while
  `immediateSendCount ≥ limit` (`transfer.go:10514,10532`), so they pin admission
  slots until a receipt returns (§3.3: delayed by the forward queue).
- Flows hash to one of 8 lanes by 5-tuple (`transfer_logical_lane.go:14-37`).
  With 5–6 bulk flows on a provider, a new flow lands on a lane already
  saturated by another flow's ACK stream with probability ≈ 5/8.
- A never-admitted first SYN has no retry owner. The capacity-refusal hook
  fires only for a group that is already a SYN retry or a native ACK retry
  (`transfer.go:8010-8021`); `retainSynRetry` runs only after a SYN was
  materialised (`ip_remote_multi_client_syn_retry.go:1-3,195`). The pure ACK got
  its bounded retry in this aggregate (`ip_remote_multi_client_native_ack_retry.go`);
  the SYN did not.

I have not proved that `protocolAdmission` is the gate that refused those
particular SYNs (the report has no reason code, and the fixture's
`PureAckAdmissionRefusals` is all zero, which I could not reconcile with 145 k
refused ACK batches). The data facts stand on their own; the site needs one
counter (§5 H5).

### 3.6 The fast windows are CPU-bound at two procs [V-data]

- `RUN-PERF.sh` runs the loaded roots with `-test.cpu=2`; `CONFIG.json` sets
  `GOMAXPROCS=2`. Client app, four-plus gVisor stacks, SDK client, 6–13
  providers, platform edge, TLS and origins share two Ps.
- `common_boundary_observation.RuntimeBefore/After`: 76.45 s of user CPU plus
  1.68 s of GC over 51.76 s of wall time → **1.51 cores average**, including a
  half-speed W3 and the gaps. 1,470 MB delivered → ≈151 Mbit per core-second →
  ≈300 Mbps at two cores. W2 (308.8 Mbps) sits on that line. 64 MiB: 175 Mbit
  per core-second.
- Both profiles peak at the same 360–380 Mbps in 500 ms samples. A window that
  is not CPU-bound would not look like that after doubling the credit.

At saturation on a busy host, goroutine and timer latency grows; in an
in-process network simulator that turns into late ACKs, bursts, RACK/TLP
firings and drops on the outer carriers [S], i.e. it feeds §3.4. The
"1,079 retransmits with zero link queue drops" in the fastest window is at
least consistent with that.

### 3.7 The measured source predates an upstream pacer fix [V-code]

`git merge-base --is-ancestor 116826e2 99c9d2a4` is false. That commit changes
`transfer_window_pacing.go` (+22) so a compressed ACK turn cannot be read as a
service decrease; its message describes pacing at 1.1× an under-measured rate
that "never filled the path again to re-measure it" (0.83 of ceiling on 1 Mb/s
cells). The 100-page cell-edge roots (1 Mb/s / 5 Mb/s, 60 ± 25 ms, 0.5% loss)
are close to that regime. Also newer than the checkpoint: `b5ee8360`
(receive-hold escrow ordering) and the `fix/rediscovery-and-race` series merged
in `bc3fec12` (`ad989329` "Leave providers a window found failing out of its
discovery for a while", `63739d75`, and others), which changes the
refusal-recovery behaviour the page-1 root exercises. Explaining residual
variance or the page-1 time on the old aggregate has low value now.

## 4. Gaps, unverified assumptions and likely-wrong conclusions

**4.1 Per-window refusal totals are available.** The "unavailable" statement
drives next-experiment 4 and the caveat "absent window 2/3 witness slots do not
establish zero refusals". The window cuts do establish it (§3.2).

**4.2 "Passed all gates" overstates small-request health.** Add to the report,
per window: max connect / TLS / first-byte over *all* flows, and the count of
flows that never connected. Today those numbers are 8.5 s+, 7.2 s and 8.5 s+ in
three windows and invisible.

**4.3 "What changed in window 3" compares the wrong things.**
- Dials 3/5 → 2/6 is reuse, not placement. Flows 0 and 4 reuse the previous
  window's idle probe connections to origin0; W2's eighth bulk flow never
  reached a provider.
- "Flows with zero bytes at termination: 1 (W2)" is a flow whose TCP connect
  never completed in 8.5 s, in the *fast* window. It deserves its own line.
- "Maximum pending zero-byte streams 1 → 4" is flows 2, 5, 6, 7 waiting
  1.2–3.9 s after launch for their first header byte on provider 6 (§3.3).
- Two flows dominate W3–W5 because they are the two on the uncontended
  provider, with reused connections whose receive window had auto-tuned to
  ≈2 MB (16307≪7) and whose carrier never idled. Per-flow byte skew is mostly a
  harness property.

**4.4 Windows are not independent samples.** Each window is cancelled
mid-transfer and the next starts within ~3 ms on the same fixture. Carried
over: up to one credit of data per provider still in flight, a 3–7 k RST storm,
carrier cwnd/ssthresh, the learned Transfer window, affinity, lanes created
lazily (new lane contracts appear at W2 t+0.09/1.59 s, W3 t+1.56/1.79 s, W4
t+0.67/1.70 s in `fast32.stderr`). Either quiesce between windows or treat a
five-window sequence as one observation.

**4.5 The TCP-layer table hides where the events are.** Report retransmits and
timeouts per stack (app, device carrier, each provider carrier, edge), and add
gVisor's `FastRetransmit`, `SlowStartRetransmits`, `FastRecovery`,
`SACKRecovery`, `TLPRecovery`, `SegmentsAckedWithDSACK`, `SpuriousRecovery`,
`SpuriousRTORecovery`, plus each TUN's `OutboundDropCount()`. Without DSACK and
TLP counts, "retransmit" cannot be read as "loss".

**4.6 The 7/2 backing covers one sequence, and the advertisement is per
sequence.** From the W1 witness and handoff maxima: an 8-frame pack is ≈10.3 KB
encoded and 30,544 bytes retained, a ratio of ≈2.97. `3.5·L` physical therefore
backs **one** sequence's full advertisement `L` with 18% margin. Two providers
each advertised `L` can need 5.9·L; the first W1 refusal is exactly that state
(RX 7.61 of 7.63 MB with two providers active). At 64 MiB with 3–4 active
providers the worst case is 39–52 MB against 15.3 MB. It works day to day only
because reorder queues are normally near-empty. The condition that fills
several at once is a receiver-side stall (blocked callback, GC, CPU), which is
correlated across providers by construction, and likelier on a phone than on
this host. The writeup presents mechanism 1 as established; I read W1 as
evidence it is still under-provisioned for multi-provider bulk.

The ratio also depends on grouping. The new witness's own receipt
(`RESUME.txt`: `incoming1381 physical5120`, and `rx_free` falling by exactly
257 × 5,120) gives **3.71** retained bytes per encoded byte for one-frame
originals. At that ratio 7/2 does not cover even a *single* sequence's
advertisement: 2,181,038 / 1,381 × 5,120 = 8.08 MB against 7.63 MB. So for
sparse originals the byte funding is short as well as the count; the count
simply binds first.

**4.7 The count witness proves a tautology.** Pause the callback, send
one-frame originals, and count binds before bytes: yes, by arithmetic
(256 × 1.4 KB ≪ 2.18 MB). The open questions are whether real traffic produces
that state (not in these runs) and what a refusal costs. The addendum reports
"recovered after four seconds of simulated time" as acceptable. I would treat
the four seconds as the finding: when a tail is refused and nothing later is
SACKed, the only repair is the sender's timer (300 ms floor with RTT samples,
2 s cold, doubling: `transfer.go:914,921`; the fixture's 4 s looks like the
cold floor plus one doubling, which I did not verify). That is the stall users
see, whichever limit triggered the refusal.

**4.8 "Busy host, as authorized" is carrying too much.** No per-window CPU, no
host load record (`capacity.jsonl` is disk only), `-test.cpu=2`. With §3.6 this
is enough to explain the ceiling and plausibly the event triggers. I would not
accept any causal statement about a single five-window sample from this setup,
in either direction.

**4.9 The addendum (lines 207–259): valid local result, wrong target.**
- The fixed-count witness result is expected (4.7).
- The helper correction is consistent with the code and I see no problem with
  the diff itself. But `grep` over connect, sdk, server and proxy finds **no
  non-test assignment of any `H1SequenceBufferAdaptive*` field except the six
  zeroing lines in `sdk/mobile_memory_policy.go:406-411`**. With threshold 0 the
  helper returns before the corrected branch (`transfer.go:16893-16898`). The
  fix changes behaviour only under test-configured settings.
- One growth step from 256 to 257 admits one more one-frame pack. A legal
  sparse sender exceeds any finite growth; see §6.3 for a simpler bound.
- The addendum says this "does not yet establish a fix for the measured Fast5
  dip". Given §3.2 I would write it more strongly: it cannot, because the limit
  was never reached. If the user's priority is a deterministic
  current-fail/new-pass *for this sequence*, this pair does not satisfy it.

**4.10 Smaller points.**
- Every run carries a background DoH warm-up that dials an IPv6 resolver
  through the tunnel and times out after 30 s (`fast32.stderr` 23:31:03,
  `full10064.stderr` 23:39:09). Harmless here, but it is unowned traffic inside
  the measurement.
- `ReliableLaneLongestAckGap` on provider row 6 grew by 8.85 s during W4. If
  that lane is the bulk return sequence it is a stall hidden by averaging; if it
  is a control sequence, say so in the report.
- "Exported native offers" and "provider initial writes" halve in W3 because
  the byte rate halved; they add no information beyond Mbps.

## 5. Hypotheses, ranked, with cheap experiments

Probabilities are my judgement for "explains most of the W3 (and W5/p6)
shortfall in *this* run".

**H1 — Fixture carrier TCP (gVisor NewReno) recovery governs each provider
path; ≈60%.** Loss/reorder episodes on the edge→client connections (link
overflow when eight streams start, drain phases at window boundaries, timer
lateness) followed by additive recovery; slow-start restart and
`ssthresh = FlightSize/2` make "restart near zero" common for the provider
whose first flow starts late.
- *E1a (instrument, no behaviour change):* sample `TunTcpConn.TcpInfo()`
  (`tun.go:1685`) every 100–250 ms on the **edge-side** socket of each client
  carrier connection (that is the download sender; wrap the edge TUN listener's
  accept), and on the client side through the existing dial observer hook
  (`route_test.go:731-736`) for the receive window. Record cwnd, ssthresh,
  CcState, RTO. Prediction: slow windows are cwnd-limited with ssthresh far
  below the pre-loss cwnd and cwnd growing ≈25 segments/s.
- *E1b (deterministic reproduction of the W3 shape):* two windows on unchanged
  source; script one 25-packet tail drop on the access link at t = 1.75 s of
  window 2 (the link profile already has `drop_every_packet_count`/loss-model
  fields). Prediction: the same halving and a 12–14 Mbps/s linear recovery. This
  is the honest "current-fail" for this sequence.
- *E1c (falsifier by parameter):* halve the access delay to 10 ms each way.
  Reno's slope scales with 1/RTT² (×4, ≈29 Mbps/s per provider); a pacer-driven
  ramp scales with rate, not RTT.
- *E1d ("new-pass"):* switch the four fixture stacks to CUBIC
  (`tcpip.CongestionControlOption("cubic")`; the fork exports
  `NewProtocolCUBIC`). Production carriers are kernel stacks, so this is also
  the more faithful fixture. Prediction: the multi-second linear ramps and the
  window-over-window growth of the flow-0 provider disappear; window CV drops
  sharply.

**H2 — Affinity pins six of eight bulk flows to one path; contributor, verified
as fact.** It multiplies whatever H1 does to provider 6 by six flows and leaves
four providers idle.
- *E2:* log per-window flows-per-provider (derivable today). Then one arm with
  a least-loaded tie-break among eligible exits for a *new* destination, or a
  per-exit bulk-flow cap relative to peers. Prediction: 4/4 placement removes
  most of the W3/W5 asymmetry even with Reno carriers.

**H3 — CPU ceiling and scheduling noise at `-test.cpu=2`; contributor,
ceiling verified.**
- *E3:* same binary, `-test.cpu=2` vs `8`, quiet host, three runs each; read
  `runtime/metrics` CPU classes at every window boundary (the collector already
  reads them at start and end). Prediction: peak windows exceed ~310 Mbps at 8
  procs and carrier retransmits per MB fall.

**H4 — Window-sized standing queue (§3.3); verified mechanism. It explains
latency, handshake stalls, deferral counters and app-TCP timeouts, not the
Mbps.**
- *E4:* one fixture arm with `WindowLatencyBudget = 50 ms` on provider send
  settings (existing field, no new code). Predictions: `TimeoutResendDefer` → 0
  in every window; 64 MiB loaded GET median below ~150 ms; late-flow dial/TLS/
  header phases under ~200 ms; app-stack timeouts → 0; throughput unchanged
  within noise.
- *E4b (deterministic component test):* carrier pinned to a fixed service rate
  R with four inner flows; assert Transfer write→ACK sojourn ≤ 2·(RTT + budget).
  Today it should read `min(credit, Σ windows)/R`; with the budget it should
  pass. That is a product-relevant old-fail/new-pass using a knob already in
  the tree.

**H5 — Control packets starve in per-lane protocol admission; data verified,
site to confirm.**
- *E5a:* reason-coded native refusal counters: class (pure ACK, SYN, FIN, RST,
  data) × boundary (collapse gate, affinity single offer, protocol admission,
  pack admission, handoff, encryption). One atomic array.
- *E5b (deterministic):* using the existing pressure fixtures, fill one lane's
  three immediate owners with flow A's ACKs and withhold receipts; offer flow
  B's SYN natively on the same lane. Expect refusal. Release receipts and assert
  B's SYN is on the wire within one receipt round trip *without* a second native
  offer. I expect current code to fail that assertion. Control: B on another
  lane is admitted at once.

**H6 — Pacer under-estimate / slow re-probe; ≈10–15% for this run.** Disfavoured
by the additive slopes (§3.4), but the measured source lacks `116826e2`.
- *E6:* covered by re-baselining on merged main and by E1c. Also export the
  pacer's rate, held rate, `ServiceBacklogged` and time spent waiting in the
  pacer per provider per window (`transfer_pacing_diagnostic.go` exists).

**H7 — Receive count/byte/funding refusal; ruled out for 32 MiB W2..W5 and for
all of 64 MiB** by §3.2. Still live for W1 and for devices (§4.6).

**H8 — Inner-TCP loss at the client TUN with one-packet-per-partial-ACK
recovery; unknown, low here.** A burst of N lost inner segments costs N round
trips (`ReturnRecoveryMaxPacketCount: 1`). Nothing in the report can see it.
- *E8:* export per provider: return-recovery episodes by trigger (timer, third
  duplicate ACK, partial ACK), bytes replayed, persist probes.

The single most useful instrument across H1/H4/H6 is a **limited-time
breakdown** per provider sequence per window — time the send loop spent
pacer-limited, credit/window-limited, and application-limited — next to the
carrier's cwnd-/rwnd-/sndbuf-limited times. With that, every slow window names
its own limiting stage and no event ordering needs to be reconstructed.

What would change my ranking: if E1a shows the edge-side carriers are *not*
cwnd-limited during the linear ramps, or E1c shows the slope does not move with
RTT, H1 is wrong and the next suspects are the pacer (H6) and the reverse ACK
coupling of §3.3, in that order. If E1d (CUBIC) leaves the window variance
unchanged, the same.

## 6. Ideas and simpler designs

**6.1 Size the window in time, not bytes.** The byte credit is a memory
permission; the in-flight target should be `rate × (RTT + budget) × gain`,
which is what `boundWindowByLatency` already computes. Evaluate making a
nonzero `WindowLatencyBudget` the default for provider send settings. It
attacks the handshake-under-load failures directly and makes "32 vs 64" a
memory question again instead of a latency one.

**6.2 Make advertised credit sum to the backing.** Either advertise
`(2/7 · physical) / max(1, active data sequences)` or reserve per-sequence
physical. With two providers at 32 MiB that is ≈1.09 MB each: still ≈160 Mbps
per provider at a 55 ms Transfer round trip, half the standing queue, and
funding refusal becomes impossible by construction at the 2.97 ratio of
eight-frame packs. To hold for every legal grouping the conversion has to use
the worst shape (3.71 for one-frame originals, §4.6), i.e. about 1/4 instead
of 2/7.

**6.3 Replace the handoff count limit with a byte-derived static count.** With
retained accounting on, every decoded owner is charged its real footprint, so
RX bytes already bound the number of packs: `physical RX / minimum retained
charge per pack`. With the witness's 5,120 bytes per one-frame original that is
≈1,490 at 32 MiB and ≈2,980 at 64 MiB; the channel slots cost 12–24 KB. Then
count can never bind before bytes, no adaptive state machine or threshold is
needed, and the memory bound is unchanged because it was the byte budget all
along. This is simpler than enabling and qualifying growth.

**6.4 Make refusal cheap instead of rare.** The receiver has already parsed
message id and ordinal when it refuses (`scanReceiveMemoryPack`). Let it queue a
compact "resend when I next ACK" for refused ordinals, as the eviction notice
does for evicted ones, so repair costs one round trip after capacity returns
instead of a 300 ms–4 s timer. The existing pressure request
(`transfer_receive_memory_pressure.go`) makes room for the retry but, as far as
I can see, does not hasten it. This helps count, byte and funding refusals
alike.

**6.5 Give lifecycle control its own admission.** Options, smallest first:
(a) pure-ACK immediates may hold at most `capacity − 1` protocol slots when
capacity > 1, leaving one for SYN/FIN/RST — a few lines in the admission key
logic; (b) retain one refused first SYN per flow and re-offer on the existing
`protocolReadiness` gate, mirroring the native ACK retry; (c) do not offer a
pure ACK at all while its lane's immediate pipeline is full — overwrite the
per-flow latest-ACK slot instead. (c) also removes ~150 k wasted admission
walks per run, which is CPU on a phone.

**6.6 Stop the post-cancel RST storm at the client.** When the app's RST for a
flow is admitted, tombstone the 5-tuple for a few seconds and drop further
inbound packets for it before the TUN. That removes 3–7 k protocol-class offers
at each window start that compete with new SYNs for three slots. (I found no
equivalent in a quick search; `sourceReset` covers client-initiated teardown
only.)

**6.7 Spread bulk across more provider paths.** In this fixture each added
path adds one additive-increase ramp and one share of resilience; the 32 MiB
profile uses 2 of 6 exits. Placement by load for new destinations is the cheap
version.

**6.8 Fixture hygiene.** CUBIC carriers; `-test.cpu` ≥ 4 for anything called a
throughput measurement; quiesce between windows or declare the sequence one
sample; N ≥ 5 frozen-binary repeats before any comparison; report medians and
the all-flow connect maximum. For diagnosis (not qualification) runs, drop the
source-pin and overlay ceremony and use a throwaway branch with plain
`go test -run` under the fixture tag: the loop time matters more than the
receipt.

**6.9 Side note on product code.** `tun.go:281` gives every production gVisor
stack Reno. If any user-facing data plane (proxy, mux upgrade) sends bulk
through it over real WAN paths, CUBIC is a one-line improvement worth
measuring. Not part of this investigation.

## 7. Direct answers to the six questions (and the addendum)

**Q1. Which link/direction and tuples account for W3's 25 queue drops; can an
event ring correlate them?** Not recoverable from this report. I would not
prioritise it: W3 has the *fewest* carrier retransmits of any window; W5 shows
the same provider-6 collapse with zero queue drops; W2 has 1,079 retransmits and
no queue drops. The two TCP timeouts are application-stack RTOs, not carrier
events, and track the bloated provider-6 path (W3 and W5 only). If you add
anything, add per-link per-direction drop counters keyed by outer tuple and the
E1a TCP_INFO series; state beats events here.

**Q2. TCP recovery, provider/flow scheduling boundary, reuse/cancellation
state, or host scheduling?** All four, with different roles. Placement is fixed
and skewed 2/6 by affinity (verified). The shortfall is provider 6's path rate
(−132 MB vs W2) plus provider 1's (−77 MB). The recovery shape is additive at
the Reno slope per provider (inferred: carrier TCP recovery in the fixture).
Reuse matters because the provider serving the reused flow 0 never idles and
keeps its cwnd across windows; cancellation matters through carried-over
in-flight data and the RST storm. Host scheduling sets the ceiling and
plausibly triggers the loss episodes.

**Q3. Run the sparse-group witness before tuning depth?** It has now run and
behaved as arithmetic predicts. It is not connected to this sequence (zero
saturations). Keep the tests; do not spend the "deterministic fix" budget here.
Prefer §6.3 over adaptive growth if count ever needs to move.

**Q4. Capture per-window refusal deltas, oldest outstanding age, queue
occupancy, credit, root/RX at the event.** Refusal deltas you already have
(§3.2). For the rest, prefer a 100–250 ms per-provider time series (delivered
bytes, in-flight, advertised credit, oldest-outstanding age, pacer rate/held
rate/backlogged, carrier cwnd/ssthresh) and the limited-time breakdown over
first-event slots. The slow windows here contain no discrete refusal event to
snapshot.

**Q5. Replay one controlled impairment with an unpaused control and a reference
TCP path.** Yes — this is E1b, and I would make it the next run. Add the
reference arm as "same links, plain TCP bulk over the carrier TUNs without
URnetwork". If the reference shows the same halving and the same slope, the
dip is the fixture's outer TCP and the remedy is fixture-side (E1d) plus the
product mitigations in §6.

**Q6. Repeatability with frozen-source independent runs.** Necessary, and I
would do it before any further attribution. Expect a wide noise floor until
§6.8 is applied; decide in advance what counts as a pass (for example: min
window ≥ 0.6 × median across 5 runs, every flow connects within 2 s, zero
app-stack timeouts).

**Addendum.** See §4.9. Also note the page-1 decomposition it reports matches
what I extracted independently (§8).

## 8. The 64 MiB page-1 failure

From `sdk-browsing-workload.pressure.navigations[1]` and
`sdk-browsing-full-window` [V-data]:

- Phases: TCP connect **4.595 s**, TLS 0.913 s, request→first byte 0.611 s.
  The 32 MiB pass has the same shape with connect 2.711 s (total 3.155 s).
- This root is the cell-edge profile (1 Mb/s and 5 Mb/s, 60 ms ± 25 ms, 0.5%
  independent loss, 9- and 45-packet queues) with deliberate provider service
  refusals. `refusal_counts = [0,0,2,1,0,0,2,2,1,2,2,2,0]`: eight providers
  refused, six of them **twice**; first refusals arrive between 1.49 s and
  4.40 s after the tap; page 1 finally connected through provider ordinal 5.
  Twelve initial exits at 64 MiB versus six at 32 MiB is the obvious reason the
  walk is longer.
- `WINDOWSIZE.md` records 4.682 s for the same root on the previous aggregate.
  The margin to the 5 s gate was already 6% before this aggregate, on a
  two-proc busy host. One 6.1 s sample is not evidence of a regression.

So this is a connect-phase problem under refusal churn, in the same family as
§3.5, not a window-size problem. What to capture next: the SYN's wire history
for that flow (which exits it was offered to, when each refusal arrived, what
the client did between a refusal and the next offer, whether the native 1 s /
2 s SYN retransmits were what drove each re-offer). Design questions worth
testing: exclude an exit that explicitly refused this flow from its remaining
attempts (the double refusals suggest it is not), widen the race after the
first refusal, and re-offer on the refusal signal instead of on the next native
retransmit. Re-measure on merged main first: `116826e2` targets a 1 Mb/s
regime, and upstream's provider cool-off series (§3.7) may already change the
double-refusal walk.

## 9. Prioritised next steps

1. **Re-baseline on merged main** (Go 1.27.4) with the instrumentation in
   item 2, quiet host, `-test.cpu=2` and `8`, at least three runs each. Do not
   investigate the `95b8` aggregate's variance further.
2. **Instrument once, cheaply:** per-provider 100–250 ms series (delivered,
   in-flight, credit, oldest age, pacer state, carrier `TcpInfo`); limited-time
   breakdown; per-stack gVisor recovery/DSACK/TLP counters and TUN outbound
   drops; reason-coded native refusals; per-flow connect/TLS/first-byte for all
   flows; reason-coded handoff drops.
3. **Run E1b → E1c/E1d.** One scripted drop reproduces the W3 shape or it does
   not. This is the deterministic reproduction the sequence actually calls for,
   and it decides fixture-vs-product in one afternoon.
4. **Fix connection-setup starvation (E5a/E5b, §6.5, §6.6)** and add the
   all-flow connect gate. This is the defect most likely to be felt by users
   and the one that fails the 5 s gate when the probe is unlucky.
5. **Run E4** (`WindowLatencyBudget`) on both profiles and the 100-page roots;
   if it holds throughput, propose it as the provider default.
6. **Placement arm (E2)** if items 3–5 leave a provider-skew residue.
7. **Receive side:** land the helper fix with its tests as latent-code hygiene
   if you wish, explicitly outside the Fast5 claim. For real robustness prefer
   §6.2, §6.3 and §6.4, and add the reason code for W1's 178 unexplained drops.
8. **Page 1 at 64 MiB:** capture the SYN/refusal timeline on merged main
   (§8); decide whether the gate or the re-race policy moves.
9. **Physical devices.** Every sizing question that remains (multi-provider RX
   oversubscription, receiver stalls, loaded connect time) is more likely to
   show on a phone than in this fixture. Go there once items 3–5 have removed
   the fixture's own variance from the picture.

## Appendix A. Reproducing my numbers

Scripts (read-only, Python stdlib) in the scratch directory named in §0:
`extract.py` (pull the `[perfvar]` JSON out of the stdout logs), `flows.py`
(per-flow phases and samples), `bridge.py` (bridge and per-stack TCP deltas),
`prov.py` (per-provider recovery/receive deltas), `clients.py` (per-client
receive cuts), `slopes.py` (ramp regression, per-provider bytes), `rampmodel.py`
(shared additive-ramp model). JSON paths used: `$.sequence.runs[i].workload.{flows,samples,loaded_latency_ns}`,
`$.sequence.runs[i].{Before,After}Details.{Bridge,Counters}`,
`$.production_constructor_policies.receive_backing_window_cuts`,
`$.common_boundary_observation`.

## Appendix B. Limits of this review

- One run per profile; every statistical statement is descriptive.
- I did not execute anything in the repositories. Code claims are from reading
  `HEAD` `391c679c`; the measured aggregate is `99c9d2a4` plus fixture overlays.
  `git diff 99c9d2a4 HEAD` shows the files I cite are identical between the two
  except for hunks outside the cited logic: `transfer.go` (+25, observed-address
  plumbing and the receive-hold escrow order), `ip.go` (+30, synthetic-speed
  refusal), `ip_remote_multi_client.go` (+92, settings and window enumeration),
  `tun.go` (+1) and `transfer_window_pacing.go` (+22, `116826e2`). Line numbers
  therefore differ slightly from the checkpoint's.
- Not established: carrier cwnd/ssthresh (never recorded); which gate refused
  the SYNs; what created W1's first gap; what triggered each loss episode; why
  `PureAckAdmissionRefusals` reads zero.
- The per-flow ramp model has no free parameters but an alternative flat-rate
  model fits within ~10%; I rely on the aggregate slopes, not on that fit.
