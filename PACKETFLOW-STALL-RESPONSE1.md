# Rapid-browsing packet-flow stall: independent review (response 1)

Reviewer: Opus 5.5 (claude-opus-5-5)
Date: 2026-10-21

**Examined revisions and overlays**

- Connect canonical `bc1bfbbf2275832f5120de3ad6b046461575e0df`. The working tree matched it
  apart from the untracked request doc and another session's MEMSTEADY.md edit. gVisor resolves
  through `replace => ../gvisor` to `/Users/builder/urnetwork/gvisor` at `5ac0102638ce2326730f1bef4d6e5147556ff232`.
- Server `20d0f074e9f6ac888bfc75270742d79c38f4fafd` (`connect/perfvar`); SDK `4b02b3822bb0db71aa97e3e532c2e3bf2b77381b`.
- Production overlay that ran in **both** failing arms. The recipes map identical production
  files, and the SHA-256 values below match the recipe pins:

  | Canonical path | Overlay file | SHA-256 prefix |
  | --- | --- | --- |
  | `connect/transfer.go` | `/private/tmp/urnetwork-browsing-combined-health-ack-pump-v1.UcI9oh/transfer.go` | `208b3c6a` |
  | `connect/ip_remote_multi_client.go` | `/private/tmp/urnetwork-browsing-combined-health-ack-pump-v1.UcI9oh/ip_remote_multi_client.go` | `1b6ac825` |
  | `connect/transfer_ack_identity.go` | `/private/tmp/urnetwork-selective-ack-health-fix-v1.LmuP0H/transfer_ack_identity.go` | `c296768e` |
  | `connect/transfer_peer_receipt.go` (new) | `/private/tmp/urnetwork-selective-ack-health-fix-v1.LmuP0H/transfer_peer_receipt.go` | `fd16d3d3` |
  | `connect/transport.go` | `/private/tmp/urnetwork-browsing-carrier-v3.D7EDUn/transport.go` | `3f596560` |
  | `connect/transport_h1_connection_observation.go` (new) | `/private/tmp/urnetwork-browsing-carrier-v3.D7EDUn/transport_h1_connection_observation_v3_1.go` | `d3289ad6` |
  | `connect/ip.go` | `/private/tmp/urnetwork-synack-return-recovery-v2.viU2p8/ip.go` | `d1ea9dd5` |
  | `connect/ip_tcp_return_recovery.go` | `/private/tmp/urnetwork-synack-return-recovery-v2.viU2p8/ip_tcp_return_recovery.go` | `77ab5f1f` |

- Test overlays read:
  - `/Volumes/data/urnetwork-strict-lane0-syn-v2.vinyoc/STRICT-LANE0-SYN-V2.overlay.json` (uninstrumented arm).
  - `/private/tmp/urnetwork-browsing-item-lineage-v2.maquYi/DIAGNOSTIC.overlay.json` (instrumented arm, `-tags=acklineagetrace`).
  - `/private/tmp/urnetwork-native-browsing-direct-null-v1.y2zPxH/DIRECT.overlay.json` (direct null).
  - Mapped drivers read: `native_browsing_merged_test.go` and `native_browsing_pressure_lane0_test.go`
    (uninstrumented arm); `native_browsing_item_*_afterimage_test.go` and `native_browsing_item_lineage_test.go`
    (instrumented arm).

Citations are `path:line` at canonical HEAD unless marked *(overlay)*. An *(overlay)* line
number refers to the overlay file of the same name in the table above.

---

## 1. Summary

The full tunnel does not fail through one stuck owner. The workload's critical path is serial
HTML navigations. It fails because per-navigation latency averages about 6.5× the direct control's (3.8 s vs
0.59 s per HTML row), with a heavy tail.

| Navigation time (HTML rows, incl. page 0) | Uninstrumented tunnel | Instrumented tunnel | Direct shaped |
| --- | ---: | ---: | ---: |
| Total HTML navigation time | 253.1 s / 67 rows | 255.7 s / 54 rows | 58.5 s / 100 rows |
| Rows > 5 s (share of navigation time) | 13 (56%) | 11 (64%) | 2 (27%) |
| Request-written → first byte, median / p90 | 0.84 s / 4.88 s | 0.92 s / 3.55 s | 0.15 s / 0.70 s |
| GetConn → GotConn, median / p90 | 0.98 s / 3.45 s | 1.14 s / 6.15 s | ≈0 / 0.16 s |

The terminal "page 66/53 deadline" is only where the 300 s budget runs out.

### Ranked explanations

1. **Inner feedback and request starvation on the Transfer *send* side of both endpoints.**
   Confidence: medium-high that it exists and is large; medium on the exact sub-mechanism. Admitted
   inner TCP packets wait seconds in the shared per-destination `SendSequence` before their
   first wire write. The doc's per-item stages start at `send_attempt`, which is emitted *after*
   the pacing wait, so they cannot show this residence. From the lineage (§3.3):
   - Pure ACKs that existed by ≈3.1 s were first written at 8.10–8.77 s.
   - Every traced device item after t≈7.7 s spent 143–426 ms between item creation (its ULID)
     and its wire write. The device loop was serialized: each item was created at the instant
     the previous one was written.
   - The page 2 asset 1 GET first reached the wire 5.78 s after `GotConn`.
   - SYN-ACKs for SYNs written at 16.15/16.55 s left the provider 7.5–8.3 s later (24.48/24.07 s).

   Inner RTT therefore inflates (SRTT 2.2–2.5 s, RTTVAR 2.8–3.4 s, RTO pinned at the 8 s cap).
   Window-limited return flows wait for late ACKs; gVisor's initial receive window is 20 MSS.
   GETs and SYNs arrive late, assets keep connections busy, and the HTML waits to acquire one.
   In a deterministic two-real-Client model, the shipping `WindowSizingFromDelivery` pacer
   alone (§2, R5–R7) slowed bulk bursts 1.7–1.8× and raised small-message latency, but only
   under jitter or feedback-path stalls. It is neutral on a clean path. It also converts
   bursts of small zero-wait offers into refusals (R8–R9): bursts of 48 on a *clean* path lose
   exactly the 16 beyond the 32 slots every time, while a constant window loses 0–7. That fits clean-link
   runs passing and the shaped tunnel failing, but the model did not reproduce the full 0.3 s
   per-item wait, so the cause of that wait is still open.

   Code reading, verified against the lines cited in §4, shows why a slow loop also refuses:
   - A pack holds one of 32 shared admission slots from acceptance until its paced write returns.
     So zero-wait offers fail at `pack-admission` whenever 32 packs are pending.
   - The pacer can latch a low rate. Any recovery write, or an RTT excursion above the minimum by
     more than max(2 ms, min/4) plus one burst time, clears both the discovery floor and the held
     floor. The pace then becomes 0.95–1.1× the *measured service* of an app-limited sequence.
     Lower measurements are accepted, and increases need guarded evidence.
   - A drain pause can also hold the next original write until the previous tail is acknowledged,
     for up to twice the residence.
2. **Provider return recovery misreads late feedback as loss, then backs off to 30 s.**
   Confidence: medium as a contributor, low as the sole cause.
   - The base timer is a fixed 1 s, independent of the measured inner RTT.
   - A timer-started recovery replays at **every** later partial ACK even when nothing was
     lost. My deterministic test reproduces 5 redundant replays for a lossless 10-segment
     flight (§2, R2). The lineage shows exactly this pattern at t=8.19–8.85 s.
   - Unanswered replays back off 1→2→4→8→16→30 s (R4), against the device's deliberately
     capped 8 s TCP RTO.
   - With the timer-aging bug the doc already found, this adds redundant load to the shared
     return FIFO. When feedback is lost it adds 16–30 s waits.
3. **Zero-wait native refusal removes inner packets whose only recovery is a long timer.**
   Confidence: medium. Refusals are 23/262 offers by page 3 and 44/749 by page 10.
   - A refused request, SYN or FIN waits for the source RTO (2.9–8 s, doubling).
   - A refused **pure ACK** has no retry owner at all: gVisor never retransmits it, Multi
     keeps nothing, and Transfer never saw it. For a window-limited return flow, only the
     provider's 1–30 s replay timer regenerates the lost feedback.
   - Boundary and class of these refusals are unrecorded, the single biggest trace gap.
4. **The single outer H1 carrier and shallow uplink seed variance but do not drive the long stalls.**
   Confidence: high for the stated limitation.
   - The uplink carrier retransmits 5.2% of segments, with 47 RTOs over the run.
   - One 1.45 s uplink head-of-line stall at t≈1.4–2.8 s is visible in the lineage.
   - Across the 36 s page 9→10 stall, the outer carrier added **1 retransmit, 0 timeouts** and
     the link **0 queue drops**. The long stalls are timer-driven inside the tunnel endpoints.
5. **No per-flow scheduling on the shared H1 lane 0 FIFO adds roughly 0.4–0.6 s to median reply.**
   Confidence: low-medium. Median reply rises from 0.55 s (prior remaining body < 100 KiB) to
   0.95–1.13 s (≥ 100 KiB), but correlation is weak (r≈0.22). This is a median contributor,
   not the tail.

### Single next experiment

Rerun the unchanged 100-page workload once with the existing `acklineagetrace` recorder and
three observation-only additions. The first two use APIs that already exist:

1. **Pacing snapshots.** At the carrier trace's existing written, five-second-crossing and
   first-byte phases, record `RemoteUserNatMultiClient.PacingDiagnosticSnapshot`
   (`transfer_pacing_diagnostic.go:120`). It gives per-sequence pacing, service and held rate,
   backlogged and discovery flags, admission count and capacity, channel count, `drain_until`
   and meter state. Add an equivalent read for the provider client.
2. **Refusal labels.** The fixture already installs `SendPackLifecycleObserver` on the device
   through `devicePackSends`, but its tracker keeps only counts. Have it count terminal events
   by `SendPackAdmissionError.Boundary` × packet class (pure ACK, data, SYN, FIN), plus Multi's
   `TcpCollapseDropCount` for collapse refusals.
3. **Residence stamps and replay counters.** Stamp `SendPack` at successful
   `SendSequence.Pack` admission. The item's ULID already gives dequeue time and the wire
   observer gives write time. Also count provider replays by trigger (timer, duplicate ACK,
   partial ACK).

Run it in two arms that differ only in the device's and provider's `SendBufferSettings.WindowSizing`:

- **A:** `WindowSizingFromDelivery`, the shipping default.
- **B:** `WindowSizingConstant`, the documented one-call, byte-for-byte rollback (`transfer.go:763–787`).

This one experiment answers three questions:

- Where admitted packets wait: the pack queue, the pacing wait, or neither.
- Which refusal classes occur.
- Whether the pacing policy is causal.

If B removes the 0.14–0.43 s per-item pre-wire waits and the multi-second ACK residence,
pacing is the leading root cause and §6 F3/F4 apply. If not, the pre-dequeue gate holds the
queue: `sendEligible` with resend capacity, retained-memory waiter or flight gates
(`transfer.go:9328–9400`). The added admission stamp separates queue residence from per-item
wait.

---

## 2. What I examined and what I ran

**Read (targeted ranges):**

- The request doc and `CODESTYLE.md` (whole).
- Both failing-arm receipts, recipes, overlays, raw `stdout.log` (go test JSON), `PRESSURE.v1.json`
  and `ITEM-LINEAGE.v1.json`.
- The direct-control receipt and raw log.
- Overlay diffs of all eight production files against HEAD.
- Native ingress, the Multi collapse/lease and coverage code, `SendSequence.Pack` admission
  gates, the send loop's item creation and paced write, window sizing and pacing rate.
- Provider TCP return emission, `retryReturnSend`, `runReturnRecovery`, and gVisor's initial
  receive window.
- The perfvar bridge and driver.
- Prior notes, searched rather than read whole: MEMSTEADY.md (2026-10-21 sections),
  PERFVAR-MEASUREMENTS.md (2026-10-21 sections, including results newer than the request doc)
  and PACKETFLOW-REGRESSION.md (2026-10-17 physical stack captures).

**Helper.** A read-only helper on the same model statically enumerated the zero-wait refusal
conditions, pacing waits and existing counters for this path. I re-verified every line I cite
from it: slot lifetime, FIFO head-only selection, drain pause, floor-loss conditions, gate
arithmetic, and the existing `PacingDiagnosticSnapshot` and lifecycle-observer labels.

**Ran.** All runs used Go 1.27.2 darwin/arm64 with `GOWORK=off go test -count=1` from
`/Users/builder/urnetwork/connect`. Test files were added only through `-overlay` JSON in
`/private/tmp/claude-501/-Users-builder-urnetwork-server-connect/87de3e83-caa3-4e17-8f8d-bd129c7da2ea/scratchpad/response1-opus/`
(abbreviated `$S` below). No 100-page, campaign or flightgate runs. The final consolidated
`-run '^(TestReview.*)$'` run (`$S/FINAL-all-overlay-normal.txt`) also matched six
pre-existing canonical `TestReview*` tests unrelated to this work, all of which passed.

Overlays:

- `PROD.overlay.json` maps exactly the eight production files above.
- `REVIEW2.overlay.json` adds my two test files to that.
- `CANON-REVIEW2.overlay.json` adds only my test files to canonical production.

| # | Command (abbreviated) | Result |
| --- | --- | --- |
| R1 | `go test -overlay=$S/PROD.overlay.json -run '^(TestNativeIngressNextReadPrecedesUnrelatedCreditRelease\|TestNativeIngressKeepsGenericBlockingContract\|TestNativeIngressColdFlowThenReadyOriginal\|TestNativeIngressResultsKeepExactMembership)$' -v .` | 4/4 PASS. Confirms the designed contract: a native TCP original refused at the resend-capacity gate is not retained by Multi, and only a later source retry is admitted. |
| R2 | `go test -overlay=$S/REVIEW2.overlay.json -run '^TestReviewReturnRecoveryLosslessDelayedAcksReplayEveryPartialAck$'` (normal, `-race`, and canonical via `CANON-REVIEW2.overlay.json`) | FAIL by design, marker `TCP_RETURN_SPURIOUS_PARTIAL_ACK_REPLAY`, identical in all three. Ten emitted segments, nothing lost, inner RTT 1.5 s: the 1 s timer replays seq 101 at 1.0 s, then every partial ACK replays already-delivered data: 121@1.5, 141@1.6, 161@1.7, 181@1.8 s. Five replays. |
| R3 | `…-run '^TestReviewReturnRecoveryTailLossRepairsOneHolePerPartialAck$'` (normal, race) | PASS. Tail loss of three segments at 0.5 s RTT is repaired one hole per round trip (171@1.5, 181@2.0, 191@2.51 s), the behavior any fix must keep. |
| R4 | `…-run '^TestReviewReturnRecoverySilentSourceReplaySchedule$'` (normal, race) | PASS (characterization). With no inner ACK, replays occur at 1, 3, 7, 15, 31, 61 and 91 s. Intervals are 1, 2, 4, 8, 16, 30 and 30 s. |
| R5 | `…-run '^TestReviewAppLimitedUplinkPacingDelay$'` | PASS (characterization). Real sender/receiver Clients, in-order H1 relays (60 ms ±25 ms, 1 Mbps forward / 5 Mbps reverse), app-limited 150-byte Ack=true messages at zero timeout, 120 s. Without stress, constant vs from-delivery p50/p90 were 75–79/102 ms vs 86–89/149–195 ms. With reverse bulk plus 1.4 s forward stalls, p50 was 91 ms vs 125 ms. **Zero refusals in every arm.** |
| R6 | `…-run '^TestReviewBulkDrainPacingLong$'` (2× normal, race, canonical) | Over 300 s, 300 KB reverse bursts every 5 s. Constant: 60 bursts, drain p50 0.58 s, max 1.50 s. From-delivery: 55 bursts, drain p50 1.12–1.15 s, max 2.25–3.59 s, total drain 60–67 s vs 42–43 s. Uplink p50 133–164 ms vs 85–87 ms. Same direction in all four runs (2× normal, race, canonical). |
| R7 | `…-run '^TestReviewBulkDrainPacingFactorial$'` (2× normal, race, canonical) | 120 s, from-delivery vs constant drain p50. Clean path: 0.581 vs 0.565 s, neutral. Jitter only: 0.69–0.70 vs 0.58 s, uplink p90 0.19–0.23 vs 0.10 s. Feedback-path stalls only: 1.00–1.03 vs 0.565 s. Both: 0.99–1.02 vs 0.58 s. Penalty appears only with jitter or feedback stalls. |
| R8 | `…-run '^TestReviewHeavierUplinkPacingLatch$'` (2× normal, race, canonical) | Heavier app-limited uplink: 20 small messages/s plus 32-message bursts, with stress, 120 s. Zero-wait refusals: from-delivery **31–51**, constant **0** in all four runs. Uplink p90 1.0–1.05 s vs 0.90 s. |
| R9 | `…-run '^TestReviewHeavierUplinkRefusalFactorial$'` (2× normal, race, canonical) | 60 s per cell. Robust cells:<br>• **Burst 48, clean path:** from-delivery refuses **176 in all four runs** (the 16 offers beyond the 32 slots in each of 11 bursts); constant refuses 0–7.<br>• **Burst 48 with stalls:** from-delivery 83–155, constant 0–86.<br>• **Burst 48 with jitter and stalls:** from-delivery 127–151, constant 0–82.<br>• **Burst 32:** only from-delivery refuses (0–32).<br>Not robust: **jitter-only at burst 48**, where ranges overlap (from-delivery 69–143, constant 0–95). Constant-arm refusals under stress are timing-sensitive (all 0 under race). |

Minor run-to-run variation in R5–R9 comes from goroutine order within one virtual instant; the
direction never changed. **R5–R9 model the Transfer pacer and admission only.** They have no
inner TCP, Multi, provider NAT or real link loss.

- **Reproduced:** R8–R9 show that the shipping pacer turns page-boundary bursts of small
  zero-wait offers into `pack-admission` refusals. It releases a burst at roughly the
  sequence's measured trickle rate while each pack holds its slot until written, whereas a
  constant window drains the same burst at link speed.
- **Not reproduced:** no model produced the full fixture's ≈0.3 s per-item pre-wire wait, so
  §1's experiment must still locate that residence.

Scratch hashes: `$S/tests/ip_tcp_return_review1_test.go` `c3690e4a…`,
`$S/tests/transfer_app_limited_pacing_review1_test.go` `0ccce991…`. Run logs are in
`$S/final_overlay_normal.txt`, `$S/final_overlay_race.txt`, `$S/final_canonical.txt`,
`$S/run_pacing_latch.txt` and `$S/run_refusal_factorial*.txt`.

**Data extraction (not tests).** Python `-I` scripts in `$S` extracted the go-test JSON to text.
They also tabulated navigation rows, computed interval rates between boundary snapshots, and
decoded ULID timestamps of Transfer message ids (first 48 bits = creation milliseconds) from
`ITEM-LINEAGE.v1.json`.

---

## 3. Reading of the evidence

### 3.1 Where the time goes

- **Serial navigation dominates.** The pressure driver fetches each HTML synchronously. It then
  launches four assets, joins the retired page (N−3), and waits for the origin to report the
  1 KiB held prefix of the current page's assets 2 and 3. That prefix wait requires those GETs
  to cross the uplink.
- **Navigation is 85% of elapsed time** (253 s of 297 s uninstrumented). Acquire totals 134 s
  and reply 111 s.
- **Acquisition means waiting for a busy connection.** 61 of 66 valid HTML navigations reused
  a connection, after a median wait of ≈1 s. Only 5 of 66 (5 of 54 instrumented) found an
  idle connection (acquire < 10 ms), against 65 of 100 in the direct control. Page N's HTML
  uses origin N mod 4, the same origin as page N−1 asset 1 (`nativeBrowsingURL`). The lineage
  proves the hand-off for page 3: connection 6 went from page 2 asset 1 to page 3 HTML. In the
  direct control, acquisition is ≈0 because assets finish before the next tap.
- **Fresh dials are slow through the tunnel.** They take 0.14–3.2 s against 0.12–0.16 s
  direct, because the SYN/SYN-ACK round trip is the inflated inner RTT.
- **Throughput is low relative to the link.** The run moved 8.88 MB of bodies in 297 s
  (≈240 kbit/s of body bytes). The payload-only serialization floor is 14.2 s. The direct
  control moved 14.6 MB in 163 s (≈716 kbit/s). The workload is latency-bound, and the tunnel's added latency
  is the whole problem.

### 3.2 Inner versus outer TCP

**Observed.**

| | Inner app-TCP (selected sockets) | Outer device H1 carrier |
| --- | --- | --- |
| RTT | 0.44–2.5 s smoothed, RTTVAR 0.27–3.4 s | 125–136 ms |
| RTO | 1.58 s → 8 s (cap) | ≈200 ms |
| Other | cwnd/ssthresh 2 on the slow sockets | |

- App-TCP retransmissions are RTO-driven: 14 retransmits and 14 timeouts by page 3
  (instrumented); 18 and 16 by page 10 (uninstrumented). The selected sockets show 0 fast
  retransmits.
  The client issues a request and then only ACKs, so a lost or late request is always
  recovered by RTO.
- The direct control had 15 client timeouts in 100 pages. The tunnel had 14 in 4 pages and
  16 in 11 pages: 10–20× more per page. Each also costs several times to an order of
  magnitude more, with RTO 2.9–8 s against roughly 0.2–1 s.

**Observed, uninstrumented page 9→10 interval (36.41 s):**

| Counter | Change |
| --- | --- |
| Outer carrier retransmits / timeouts | 95→96 / 3→3 |
| Link queue drops / loss drops | 226→226 / 9→13 |
| App TCP timeouts | 6→16 |
| Bridge offers / refusals | 130 / 13 |
| Device Multi Transfer initial writes | 119 (3.3/s) |
| Provider Transfer initial writes | 92 (2.5/s) |
| Link wire bytes, all directions | 19.4 KB/s |

**Inference:** this 36 s stall had a quiet, loss-free outer path. Flows were advancing on
timers: app-TCP RTO and provider replay. PERFVAR-MEASUREMENTS.md's earlier V3.3 run agrees: no
whole-edge retransmit or timeout increase across a 12.6 s reply interval.

### 3.3 The item lineage contains send-side residence the doc did not read

These are observed values from `ITEM-LINEAGE.v1.json`, times relative to
1792604420.000 s.

- **Item ULID vs wire write (device).** A Transfer message id is minted at dequeue time:
  `messageId := NewId()` in `sendWithSetContractDisposition` at `transfer.go:10797`
  (*overlay* 10838). The "packet"/`send_attempt` events are emitted inside `writeWithLifetime`
  only after `paceWrite` returns. That holds on the route-policy path (`paceWrite` at
  `transfer.go:13053` inside `writeWithLifetime`, observation at `:13010`; *overlay*
  13094/13051) and on the plain and wrapped paths (`transfer.go:13224`, `13262`).

  | Device item | Created → written | Pre-wire wait |
  | ---: | --- | ---: |
  | #79 | 1.365 → 1.374 | 9 ms |
  | #80 | 1.373 → 1.395 | 22 ms |
  | #81 | 2.227 → 2.228 | 1 ms |
  | #93 | 7.750 → 8.099 | 349 ms |
  | #94 | 8.099 → 8.448 | 349 ms |
  | #95 | 8.448 → 8.773 | 325 ms |
  | #96 | 8.773 → 9.090 | 317 ms |
  | #97 | 9.090 → 9.392 | 302 ms |
  | #101 | 10.628 → 11.054 | 426 ms |
  | #103 | 11.479 → 11.727 | 248 ms |
  | #120 | 16.009 → 16.152 | 143 ms |
  | #123 | 16.403 → 16.546 | 143 ms |

  Each of #94–#97 was created at the millisecond its predecessor was written, so the loop was
  serialized one item at a time. Device sequence numbers #81→#93 span 5.5 s, about 2 items/s.
- **Item ULID vs wire write (provider).** #112–#114 took 4–15 ms at t≈8.2–8.9, when the
  provider was app-limited and sending only replays. #196, #197 and #199 took 121, 149 and
  110 ms at t≈24. #197 was created at the instant #196 was written. Each message is 2.6–3.4 KB,
  so the paced rate was ≈23 KB/s on a 5 Mbps downlink.
- **ACKs held about 5 s.** Page 1 is 125,952 bytes, so each asset body is 30,976 bytes. Page 1
  asset 2's response on connection 6 therefore ends exactly at 3220410666: ISN+1 = 3220379586,
  plus a 104-byte header, plus the body. Its body was closed at 3.101 s and the connection
  reused at 5.275 s, so all of it had arrived by ≈3.1 s. Yet device pure ACKs carrying
  3220399498, 3220402642 and 3220408930 were dequeued at 7.750, 8.099 and 8.448 s and written
  at 8.099, 8.448 and 8.773 s. *Inference:* these ACKs were generated by ≈3.1 s and resided
  ≥4.6–5.7 s in the device send path. They cannot be ACKs elicited by later replays, because
  such ACKs would carry the newer edge 3220410666.
- **The provider replays every late ACK.** Provider data segments at 8.186, 8.505 and 8.851 s
  each start exactly at the device ACK written 57–87 ms earlier, and all carry the stale
  TcpAck 3220379688. *Inference:* these are partial-ACK replays (`ip_tcp_return_recovery.go:170`
  ready bit, `:228`/`:256` replay) of bytes the client had held since ≈3.1 s. R2 reproduces this
  pattern deterministically.
- **A GET took 5.78 s to reach the wire.** Page 2 asset 1 had `GotConn` at 5.275 s. Its GET is
  102 bytes at seq 3220379688, flags PSH|ACK. Its first Transfer write is item #101, created
  10.628 s and written 11.054 s, delivered at 11.121 s and Transfer-ACKed at 11.194 s. The
  provider's packets up to 8.851 s still acknowledge only 3220379688.
  - Connection 6's timeouts rose 1→3 during this asset. RTO ≈1.93 s doubling to 3.86 s puts
    retries at ≈7.2 and ≈11.06 s.
  - Two scenarios fit FIFO order and these times. In one, the 5.27 s original was admitted and
    held ≈5.4 s (the ≈7.2 s retry suppressed as a pending duplicate). In the other, the
    original was refused and the ≈7.2 s retry was admitted and held ≈3.4 s.
  - Either way the GET spent 3.4–5.4 s accepted but unwritten, or was refused first.
  - That interval, plus a slow response, explains most of page 3's 9.23 s acquisition.
- **Page 3 hit the same delays.**
  - Device SYNs #120 and #123 reached the wire at 16.152 and 16.546 s. Their provider SYN-ACKs
    sit inside multi-flow return messages #196 (created 23.953 s, written 24.074 s) and #199
    (created 24.371 s, written 24.481 s): 7.5 s for flow 9 and 8.3 s for flow 8. *Inference:* a
    loopback origin dial cannot take seconds, so most of this is residence before dequeue in
    the provider's sequence. There is no admission stamp, so late generation cannot be fully
    excluded.
  - Provider #197 at 24.223 s carries data from 3220410666 (the start of page 2 asset 1's
    response) with TcpAck 3220379790. So at 24.22 s the provider had seen neither the client's
    ACKs for a response completed at 21.11 s, nor the page 3 HTML GET written at 21.111 s.
    The HTML first byte arrived at 24.662 s.
- **The 1.45 s outer head-of-line event.** Device items #79 and #80, sent at 1.374 and
  1.395 s, and the device's Transfer ACK for provider #82, written at 1.365 s, all arrived at
  the provider at 2.821 s. This is the outer uplink stall that seeded connection 6's first
  spurious RTO.

### 3.4 Where I read the request doc differently

- **"Items 101 and 103 secure downstream custody promptly … not a multi-second receive-custody hold."**
  This is correct downstream, but those same items waited 426 and 248 ms before their own wire
  writes. The pure ACKs ahead of them waited ≈5 s before dequeue. The multi-second hold is
  **upstream**, in the sender's `SendSequence`. The doc's stage list begins at `send_attempt`,
  which is after pacing, so it cannot see it.
- **"Zero Transfer timeout-resend writes … no handoff drops."** This is consistent with
  send-side residence: nothing is lost, it is only late before the wire.
- **"Retransmissions alone are not a sufficient explanation."** I agree. The difference is the
  *cost* of each recovery (RTO 2.9–8 s, replay 1–30 s) and the late feedback that triggers
  spurious recoveries, not their count.
- **"The trace cannot identify the terminal page-53 failure's owner."** There is no special
  owner to find. The terminal HTML (page 66 uninstrumented) had used 8.4 s when the global
  budget expired. Effort is better spent on the per-navigation distribution.
- **RouteUnacknowledgedDuration / ReliableLaneLongestAckGap.** The provider's 13.59 s gap
  has no stall-onset reading (no resend timer fired), so it is an idle interval, not a
  stall. The device's 12.68 s gap had a timer firing with 23 items outstanding at t≈2.52 s,
  inside the startup outer stall. It is a client-wide maximum and conflicts with ACKs traced
  at 2.91 and 11.19 s, so it is unattributed (§8). I agree it must not be read as live debt.
- **Single runs are noisy.** Later results in PERFVAR-MEASUREMENTS.md, which the doc
  predates, show single runs of nearly identical code completing 63 to 87 owners: timer fix
  76, initial-admission fix 72, strict lane-0 pair 87. Differences of ±15 owners are
  within run-to-run noise, so §7 calls for paired runs.

---

## 4. Candidate mechanisms

### M1. Send-side residence of admitted inner packets in the shared per-destination `SendSequence`

**Mechanism.**

- Native ingress offers every app packet with zero wait: `ip_native_ingress.go:58–60`, and
  `ip_remote_multi_client.go:7296–7300` (canonical and *overlay*).
- On success the pack enters the sequence's 32-deep `packs` channel and 32-slot pack admission
  (`transfer.go:63`, `7037`, `7039`).
- The send loop dequeues one pack and creates the item and message id (`transfer.go:10797`).
  It then runs `writeMaybeWrappedBytes`. For an Ack=true item on an H1-only policy with
  `DeliverySizedWindowScale > 0`, that call waits in `paceWrite` →
  `windowBurstPacer.waitForServiceReservation` (`transfer.go:12906`, call at `:12954`). Only then does it
  write and observe the wire message.
- Every flow to the provider (ACKs, GETs, SYNs, FINs) shares that FIFO on lane 0. H1 has no
  flow isolation, so all packs share one admission key, and `TakeFifoEligible` considers only
  the oldest pack (`transfer_send_scheduler.go:277–297`).
- A pack keeps its admission slot through the paced write. The comment at `transfer.go:6693` says
  the slot pool counts "both channel-resident and scheduler-resident Packs". For logical groups
  the slot is released by `releaseRaw()` only after `sendGroupRecord` has written the item
  (*overlay* `transfer.go:8047`, `8081`). The channel and the slot pool are both 32, so the
  handoff refusal is effectively unreachable. A slow write loop surfaces as `pack-admission`
  refusals.
- How the pace can latch low:
  - A recovery write is congestion evidence and zeroes `heldPacingRate`
    (`transfer_window_pacing.go:1244–1251`).
  - `observeQueueWithLock` ends discovery when the latest RTT exceeds min + compression +
    max(2 ms, min/4). It zeroes the hold when the excess also exceeds one burst time
    (`:700–713`).
  - `windowPacingRate` then uses 1.1× (or 0.95× if backlogged) the measured service once that
    service is established at 4 KiB (`:291–342`, `transfer.go:12010–12017`).
  - For the device uplink, the RTT includes the provider's Transfer ACKs crossing the
    downlink FIFO, behind bulk return data. Downlink queueing alone can therefore trigger this
    and price the uplink at its own app-limited service.
- The drain pause (`transfer_window_pacing.go:1400–1462`):
  - Trigger: pending writes, latest RTT above min + max(2 ms, min/4), and the cooldown passed.
  - Effect: holds new original writes until the previous tail is cumulatively ACKed, or until
    `drainUntil`. That deadline is start + min(AckTimeout = 60 s,
    max(40 ms, 2 × residence)) (`transfer_window_receiver_rtt.go:48`).
  - Cooldown between pauses: max(5 s, 8 × span) (`transfer_window_pacing.go:147–148`).
- If each item waits 0.14–0.43 s, a FIFO of 10–15 items holds a pure ACK about 5 s.
- Ownership is consistent with CODESTYLE throughout. The item is owned by Transfer and Multi
  has committed collapse coverage, so the source's RTO retries are *correctly* suppressed as
  duplicates (`ip_remote_multi_client.go:7366`). The cost is that the source keeps doubling
  its RTO without RTT samples (Karn).

**How it produces the symptoms.**

| Symptom | Path |
| --- | --- |
| Inflated native RTO | Inner RTT samples include seconds of residence (SRTT 2.2–2.5 s, RTTVAR 2.8–3.4 s → RTO 8 s). |
| Old asset connection busy | Its GET arrives late. Its response is window-limited: the provider's emission is gated on the client's advertised window (`ip.go:5954`), and gVisor's initial window is `2 × InitialCwnd × MSS` ≈ 20 MSS (`gvisor/pkg/tcpip/transport/tcp/endpoint.go:1283–1290`), so it waits for late ACKs. |
| Slow acquisition | The HTML waits for that asset's connection, or for a dial whose SYN-ACK waits in the provider's FIFO (7.5–8.3 s observed). |
| Deadline | The accumulated per-page latency. |

**Positive evidence.** §3.3 (ULID vs wire gaps; ACKs written ≥4.6 s after they existed; GET
5.78 s after `GotConn`; SYN-ACK 7.5–8.3 s), and §3.2 (device and provider writing 2.5–3.3
messages/s during a loss-free stall). Physically, PACKETFLOW-REGRESSION.md's 2026-10-17 Pixel
stack captures put the TUN producer in `SendSequence.acquirePackAdmission` and its sequence
worker in `windowBurstPacer.waitForServiceReservation`, with all seven H1 writers idle at
input select.

**Inference.** The per-item wait is pacing. Between item creation and the wire
observation, the code runs these steps:

- frame marshal and encryption wrap;
- `openContractMultiRouteWriter`, which is non-blocking (`transfer.go:13462`);
- `paceWrite`;
- the selector's `beforeWrite`/`commitDispatch` callback. It is invoked before any route
  attempt (`transfer_route_manager.go:3058`), so selector waits come after the observation.

With no recovery writes recorded (timeout and gap resends zero by page 3), `paceWrite` is the
only step that can block for hundreds of milliseconds. R6–R7 show the shipping from-delivery pacer slows a sequence
1.7–1.8× under jitter or feedback stalls and is neutral on a clean path. Contributing code:

- `windowPacingRate` uses ×0.95 of measured service when "backlogged", and the held-rate floor
  applies only when not backlogged (`transfer_window_pacing.go:291–342`).
- `unqueuedWithLock` treats any latest RTT more than 2 ms above the minimum as queued
  (plus compression, `transfer_window_pacing.go:2525–2530`). That is nearly always true on a ±25 ms-jitter
  profile, so a sparse sender's small outstanding flight can exceed its tiny rate×residence
  bound and be priced down.

**Missing coverage.** There is no admission timestamp on packs, no per-item pacing-wait
duration, and no pacer-state snapshot in the runs.

- The per-item creation→write gap can only lie inside `paceWrite`: the serialization
  deadline, the FIFO turn, the drain pause or the burst meter. Which one, and at what rate, is
  unrecorded.
- The ≈5 s pre-dequeue residence of the ACKs could also include time with the dequeue gate
  (`sendEligible`) closed.
- My models never produced 0.3 s per item.

**Smallest discriminator.** Deterministic, and already in hand: R9's clean burst-48 cell
falsifies the pacing→refusal link if a fix leaves its 176 refusals. For the residence
magnitude, extend R5 with a gVisor client TCP and window-limited returns, and assert per-item
pre-wire wait ≤ max(2 ms, 2 × serialization time). Decisive
empirical: the §1 A/B (`WindowSizingConstant`), which uses an existing switch. If residence
persists under B, M1's pacing sub-hypothesis is false.

### M2. Provider return recovery treats late feedback as loss

**Mechanism** (`ip_tcp_return_recovery.go:161–204`, `:211–278`; *overlay* lines are +11):

- The base delay is fixed: `ReturnResendTimeout` = 1 s (`ip.go:494`).
- Each replay doubles the delay to a hard-coded 30 s cap (`:226`). It resets only on an
  advancing inner ACK.
- `returnProgressTime` and attempts are stamped *before* the synchronous admission callback
  (`:258–268`). This is the doc's finding.
- A timer-started recovery sets `returnRecoveryEnd = receiveSeq`. Every later advancing ACK
  below it sets `returnRecoveryReady` (`:170`) and immediately replays the segment at the new
  ACK. Nothing checks whether the ACK came from the original flight (no Eifel/F-RTO-style
  test), and nothing uses Transfer receipt or RTT.

**Symptoms.**

- When inner ACKs are late (M1), the 1 s timer fires and the provider replays roughly one
  segment per ACK of an already-delivered flight.
- Each duplicate draws an immediate duplicate ACK onto the congested uplink and adds bytes to
  the shared return FIFO.
- If a feedback ACK is refused (M3) or keeps arriving late, attempts climb: a window-limited
  flow then waits 8–30 s for its next replay.
- The doc's admission-aging bug makes a slow first admission spend the next interval early.

**Positive evidence.** R2 (5 redundant replays, lossless flight), R4 (1→30 s schedule), and the
lineage replays at 8.19–8.85 s and 24.22 s of bytes the client already had.

**Inference.** Spurious replays are a material share of return frames. Provider
`InitialFrameCount` was 484 at page 3 against ≈363 full data segments, but pure-ACK and SYN
frames also count, so the split is unknown.

**Missing coverage.** No counters for provider replays by trigger, redundant replay bytes, or
attempt-count histograms.

**Smallest discriminator.** R2 itself, which fails before the fix and must pass after, with R3
staying green. Empirically, count replays whose bytes the client had already acknowledged at
send time.

### M3. Zero-wait native refusal with no retry owner for feedback

**Mechanism.**

- Native offers use timeout 0 (`ip_remote_multi_client.go:7298`).
- Refusal can come from collapse (`:7366`), the resend-capacity gate (`transfer.go:7189` /
  `9328–9356`), pack admission (`:7254`), the 32-deep handoff (`:7620`, `:7634`), encryption
  readiness, or client selection.
- With strict retained accounting, the resend-capacity gate has no per-sequence floor
  (`transfer_queue.go:241–279`). It closes only when the shared budget's `Available()` falls
  below one minimum item, about 8.2 KB at protocol v2. That needs roughly 2 MB already charged
  across every owner of the 2 MiB pool, about 250 small items, so a few dozen pending ACKs
  cannot close it alone.
- A `resend-capacity` label therefore means aggregate retained exhaustion. A `pack-admission`
  label means the slow write loop of M1. The handoff label should not occur here.
- Refused originals leave coverage uncommitted, which is correct, and rely on source TCP
  retry.
- For pure ACKs, which since fix 2 bypass collapse but not admission, gVisor never retransmits.
  Their information returns only when the provider sends something new: a replay on a 1–30 s
  timer.

**Symptoms.** Requests, SYNs and FINs pay 2.9–8 s RTOs, doubling; pure-ACK loss pays M2's
backoff schedule.

**Positive evidence.** 23 refusals by page 3, 44 by page 10. 13 refusals and 10 app RTOs in the
quiet 36 s interval of §3.2. The four retained failed batches at each run's first-slow
boundary are single-packet.

**Deterministic.** R8–R9 reproduce `pack-admission` refusals from the pacer with real
`Client`/`SendSequence` code. The same burst under a constant window is admitted on a clean
path.

**Inference.** Some of the run's refusals are legitimate collapse suppressions of spurious
source RTOs (from M1). The rest are admission refusals, most plausibly `pack-admission`.

**Missing coverage.** The boundary and packet class of each refusal.

**Smallest discriminator.** Deterministic: hold the device sequence's pack admission full,
offer a pure ACK that advances a window-limited provider flow, release capacity, and assert
the provider learns the new edge without waiting for a replay. Today it does not. Empirical:
the refusal boundary × class counters from §1.

### M4. Single outer carrier on a shallow, lossy uplink (seed, not driver)

**Mechanism.** One H1 TCP carries every inner flow and every Transfer ACK. Each uplink loss
on the 1 Mbps link with a 9-packet queue stalls all of them for at least one outer RTT, or for
an RTO.

**Evidence.**

- Final uplink carrier retransmits 746 of 14,344 segments (5.2%), with 218 fast retransmits
  and 47 RTOs.
- The 1.45 s stall at t≈1.4–2.8 s seeded connection 6's first spurious RTO.
- Counter-evidence for driving the long stalls: +1 retransmit and 0 RTOs across the 36 s
  page 10 stall.

**Discriminator.** In R7, feedback-path stalls alone already double the from-delivery drain
time. Empirically, compare per-stall outer counters, as above.

### M5. Shared FIFO without per-flow scheduling (median contributor)

**Mechanism.** H1 lane 0 has no flow isolation. `acquirePackAdmission` uses a single
admission key unless flow isolation is set (H3 only), and dequeue is FIFO. A new HTML response
or a SYN-ACK queues behind earlier flows' return data.

**Evidence.** Median reply rises from 0.55 to 0.95–1.13 s with prior remaining body, r≈0.22.
The SYN-ACKs for flows 8 and 9 shared a message with other flows' return data, after a 7.5–8.3 s
wait.

**Discriminator.** A deterministic two-flow test: bulk return on flow A, then a 1-segment reply
on flow B. Measure B's first-byte delay against A's queued bytes, under FIFO and under per-flow
round robin.

---

## 5. Answers to the five questions

1. **Where can accepted inner TCP work remain owned without progress?**
   - In the sender's own `SendSequence`, after zero-wait acceptance and before the first wire
     write: in the 32-deep `packs` FIFO, and in the pre-wire pacing wait (`transfer.go:12906`
     onward). This holds on both endpoints (§3.3, M1).
   - Also in provider return chunks waiting on replay timers that are backed off or
     non-adaptive (M2).
   - Collapse suppression of the source's retries during that interval is ownership-correct.
     It is also what lets the native RTO grow to 8 s without RTT samples while the
     connection stays busy.
2. **Can a refusal or coalescing path release retry authority too early, suppress the only
   viable retry, or replay needlessly?**
   - No early release of payload retry authority was found. The collapse gate
     (`ip_remote_multi_client.go:7362`, via `canUpdateSequenceForClientWithLock`'s
     pending-coverage check at `:10686`) drops pending-covered retries before
     `reserveTcpRecoveryWithLock` can spend the one-per-ACK lease. The five collapse
     regressions and R1 agree.
   - A refused pure ACK has **no** retry owner, and its only regeneration is the provider's
     backed-off replay (M3).
   - Unnecessary replay is demonstrated: partial-ACK replay after a timer start (R2), the
     admission-aged timer (doc), and initial-admission replay (PERFVAR, 2026-10-21).
3. **Do return replay timers distinguish retention, admission, write, Transfer receipt and
   inner progress?** No:
   - Retention and replay construction stamp the progress clock before admission.
   - Physical write and Transfer receipt are never consulted.
   - The interval ignores inner RTT.
   - Partial-ACK continuation cannot tell spurious recovery from real holes.
   - Backoff runs to 30 s.
4. **Is there an admission/ACK dependency cycle or pacing/window policy specific to
   overlapping flows?** Yes.
   - The cycle: return data → client ACKs → shared uplink FIFO with per-item pre-wire waits →
     late ACKs → window-limited provider flows stall and the 1 s timer fires → redundant
     replays → more downlink queueing and more client duplicate ACKs → more uplink load.
   - Overlapping unfinished assets maximize the number of flows whose feedback shares one
     FIFO.
   - Page-boundary bursts (four GETs plus ACK bursts) are exactly when zero-wait admission
     refuses.
   - The from-delivery pacer's penalty appears only under jitter or feedback stalls (R7),
     which explains passing clean controls.
   - Its pacing of an app-limited sequence turns bursts larger than the 32 admission slots
     into zero-wait refusals even on a clean path (R9).
5. **Smallest discriminators:** listed per mechanism in §4. The positive evidence
   for M1–M3 comes from lineage timestamps and the deterministic tests R2 and R4–R9. The
   missing coverage is admission time, pacing wait duration, refusal boundary × class, and
   replay trigger counts.

---

## 6. Potential fixes

Each fix keeps the CODESTYLE invariants:

- shared receive callbacks and the shared TUN reader stay zero-wait;
- refused payload originals keep source-TCP retry custody (no collapse coverage is committed
  until Transfer acceptance);
- nothing synthesizes an inner or Transfer ACK for undelivered data;
- new tunables are settings fields.

**F0. Instrument first (observation only).**

| Change | Location |
| --- | --- |
| Stamp `sendPack.admittedAtNanos` on successful `SendSequence.Pack` | `transfer.go:7305` |
| Per-sequence atomic histograms: admission→dequeue, dequeue→pace end, pace end→write | `transfer.go:10797`, `12906` |
| `RemoteUserNatMultiClient.PacingDiagnosticSnapshot` at the existing carrier-trace phases (already exists) | `transfer_pacing_diagnostic.go:120`; fixture only |
| Native refusal counts by the existing `SendPackAdmissionError.Boundary` × packet class, plus `TcpCollapseDropCount` | fixture tracker (`send_pack_lifecycle_tracker_test.go`); Multi native path for class |
| Provider replay counters by trigger, plus an interval histogram | `runReturnRecovery` |

- No ring, so nothing is overwritten.
- Effect: decides M1 vs M3 vs M2 shares.
- Risk: atomic cost on the hot path; guard it like `ackLineageTraceEnabled`.

**F1. Provider replay correctness** (`ip_tcp_return_recovery.go`, `ip.go` settings).

*Changes:*

- **(a) Restamp the progress clock after the callback.** Stamp the replay clock after the
  synchronous `receivePacket` returns, if attempt, frontier and progress are unchanged.
  This is the doc's in-progress fix; extend it to initial admission.
- **(b) Detect spurious recovery.** Record the replay's emission time and TSval. On the first
  advancing ACK, if TSecr predates the replay (when `enableTimestamp`) or the ACK arrives
  sooner than ½ of the minimum observed inner RTT after it, mark the recovery spurious. Then
  clear `returnRecoveryActive/Ready` instead of replaying the next partial ACK.
- **(c) Make the timer RTT-adaptive.** Use per-flow SRTT/RTTVAR from non-replayed chunks
  (Karn) or TSecr. The interval is `clamp(SRTT+4·RTTVAR, ReturnResendTimeout,
  ReturnResendMaxTimeout)`.
- **(d) Move the cap into settings.** Replace the literal 30 s and 2^5 with settings fields.
  Default `ReturnResendMaxTimeout` to 8 s, mirroring the reasoning at `tun.go:160–169`.

*Ownership:* retained chunks are still released only by inner ACK; replays stay on the
per-flow recovery goroutine, which is the CODESTYLE exception.

*Measured consumer:* the window-blocked return flow and the shared return FIFO. Count
spurious and redundant replay bytes and the attempt histogram on unchanged traffic.

*Regression tests:*

- R2 must pass with at most one replay; R3 must remain three replays.
- Add a TSecr variant of R2.
- Add a case with a lost feedback ACK and a large inner RTT, checking that the next replay
  comes within 8 s.
- The existing `^TestTcpReturn` cohort must stay green.

*Risks:*

- A heuristic false-spurious verdict delays real recovery. Mitigation: fall back to duplicate
  ACKs and the timer.
- Raising the base on slow paths delays real tail repair. Mitigation: bounded by the 8 s cap.

**F2. Regenerable feedback custody for refused pure ACKs** (Multi native path).

*Change:*

- When Transfer admission refuses a payload-free inner TCP ACK, retain the source's own refused
  packet instead of dropping it. Keep at most three per flow: the latest cumulative ACK plus
  up to two identical duplicates, so fast-replay evidence survives. Hold them in a
  per-destination adapter with hard count and byte bounds.
- A newer cumulative ACK or window value for the flow replaces the retained set.
- Nothing is synthesized.
- One adapter-owned worker per destination waits on the sequence's existing capacity
  notification (resend-capacity gate or pack admission release) and re-offers.

*Ownership:* this applies, by analogy, CODESTYLE's "bounded carrier adapter" rule. That rule
is written for receive carriers; the native reader is likewise a shared producer that must
not block.

- The TUN reader's offer stays zero-wait.
- The waiting worker is not shared across readers.
- Cancellation joins it, and pooled buffers are returned before lifecycle completion.
- Retirement of the flow withdraws its entries, per "Queued work keeps its owner's lifetime".
- The slot memory is counted, per "Capacity changes need total-cost accounting".
- Only regenerable, payload-free ACKs are held. Payload originals keep source retry custody,
  and nothing is acknowledged that the source did not send.

*Effect:* removes "refused ACK → wait for a 1–30 s provider replay" stalls (M3).

*Risks:*

- Stale-ACK replay after flow reset. Mitigation: key the slot by flow generation and drop it
  on SYN/RST/close.
- Memory: one packet root per active flow. Count it in the device budget.

*Regression test:* the M3 deterministic discriminator. It fails today (the provider waits for
its replay timer) and passes with F2. Add controls: no slot for payload segments, slot
withdrawn on RST, and healthy-sibling isolation.

**F3. Keep feedback out of the pre-wire wait** (conditional on F0 locating it in pacing).

*Change:*

- Exempt Ack=true packs whose frames are all payload-free inner TCP ACKs or SYN/FIN controls
  from `waitForServiceReservation`. Charge their bytes to the service after the write, so
  measurement stays honest. This keeps FIFO order, so it helps only when the head item is
  itself control.
- Alternatively, dequeue payload-free pure-ACK packs ahead of payload packs within the same
  sequence. This is order-safe for two reasons:
  - the provider applies an ACK field independently of upload payload order (`ip.go:6744`);
  - Multi's source-ordered collapse and recovery state is updated at admission, before this
    reorder.

  Keep SYN/FIN/RST in FIFO order.

*Ownership:* unchanged. Items are still retained until their Transfer ACK.

*Effect:* removes seconds of inner-ACK and SYN-ACK latency on both endpoints.

*Risk:* control bursts slightly exceed the pacing target; the bytes are small.

*Test:* a two-flow `SendSequence` test with a bulk flow being paced. A pure-ACK pack offered
later must be written within one serialization time.

**F4. Pacing estimator robustness** (`transfer_window_pacing.go`).

*Changes:*

- Replace the fixed `+2 ms` "unqueued" margin with a jitter-aware margin, such as
  max(2 ms, 2 × mean RTT deviation from receiver-timed samples).
- Do not let feedback-path head-of-line stalls (ACK delay without data-direction residence)
  lower service.
- Exclude app-limited intervals from service measurement, BBR-style.
- Do not clear the held and discovery floors (`:700–713`) on an RTT excursion explained by
  reverse-direction queueing. Use receiver-timed data-direction residence instead.
- After a collapse, allow a bounded re-probe at the held or initial rate once the queue
  empties, so a latched low pace cannot persist on app-limited traffic.
- Skip the drain pause when outstanding flight is under a few messages. A one- or two-item
  tail cannot prove a standing relay queue.

*Effect:* closes the 1.7–1.8× penalty in R6–R7 without changing clean-path behavior.

*Regression test:*

- The R7 jitter-only and stall-only arms: from-delivery within 15% of constant on drain p50
  and uplink p90.
- The R9 clean burst-48 cell: today 176 refusals vs 0–7 constant. It must refuse no more
  than the constant arm.
- The existing `TestWindowPacing*` suite.

*Risk:* delayed reaction to real congestion. Mitigation: keep the ×0.95 drain behavior when
receiver-timed residence grows.

**F5. Per-flow fairness on H1 lane 0** (only if F0–F4 leave a median gap). Round-robin
scheduling keys within the existing 32-slot bound, with no new depth. Measure HTML first-byte
p50 against prior queued bytes.

I do **not** recommend:

- raising memory, depth, queue sizes or connection caps;
- re-enabling the blocking native path;
- enabling eight lanes. The strict lane pair recorded 43 vs 87 owners.

None of them addresses the identified wait.

---

## 7. Validation plan

1. **Root-cause regression tests** (deterministic, `synctest` or barriers, normal and `-race`):
   - R2/R3/R4 for F1.
   - The M3 feedback-custody test for F2.
   - The F3 two-flow pre-wire test.
   - The R7 factorial and the R8/R9 refusal cells for F3/F4.
   - The full-stack witness: one real `Client` pair with gVisor TUNs and the provider TCP
     sequence. A window-limited 50 KB response must complete within k×RTT when the
     uplink's ACKs are delayed only by the modeled link, with no spurious provider replay.
     This should fail before the fix and pass after.
2. **Full workload, unchanged deadlines** (300 s application, 600 s watchdog, seed 4013, lane 0,
   strict 2 MiB budgets):
   - Gate: 100/100 page owners, 400/400 assets validated, no deadline.
   - First run the §1 A/B and F0 instrumentation, then each fix singly and in combination.
3. **Uninstrumented paired performance validation:**
   - Interleave ABAB, at least 10 pairs per arm, across at least 3 link seeds, with fresh
     processes and fixed `GOMAXPROCS=2`.
   - Report failed runs beside the percentiles.
   - Metrics:
     - completed owners;
     - HTML first-byte p50/p90/max;
     - acquire and reply sums and the share in navigations over 5 s;
     - asset completion p50/p90;
     - app-TCP RTOs per page;
     - device and provider initial writes per second and the pre-wire residence histogram;
     - native refusals by boundary × class;
     - provider replays by trigger and redundant replay bytes;
     - outer retransmits/RTOs and link queue drops;
     - goodput;
     - pool roots and bytes after drain;
     - heap peak.
   - Controls: clean-link 100 pages and the direct null, both no worse. Run the 32 MiB iOS and
     64 MiB Android profiles separately before shipping F2 (per-flow slot memory).

---

## 8. Open questions and missing evidence

- **The 0.14–0.43 s per-item pre-wire wait.** Is it the pacer (rate, burst or turn), the
  dequeue gate, or something else? My deterministic model does not reproduce it, so F0
  timestamps or the §1 A/B are needed. A `PacingDiagnosticSnapshot` during a stall settles
  most of it: did the device lane's `pacing_bytes_per_second` latch near its offered rate,
  were `held_bytes_per_second` and discovery cleared, was `drain_until` active, and was the
  admission count at 32?
- **Refusal classes.** The boundary and packet class of the 23 and 44 refusals are unknown.
  So is how many are correct duplicate suppressions versus admission refusals of ACKs or
  requests.
- **Provider replay counts** by trigger, and the share of redundant return frames.
- **Window-limited provider flows.** Was the client's advertised window the binding limit for
  the slow assets? gVisor auto-tuning under seconds-long RTT has not been measured; a
  per-flow window snapshot at the provider would answer it.
- **The device's 12.68 s longest reliable-lane ACK gap.** It is client-wide, with a timer
  firing and 23 items outstanding at t≈2.52 s. Per-sequence attribution is needed; it may
  belong to a control or contract sequence.
- **Mobile relevance.** On iOS the outer TCP is the kernel's, not gVisor. Pacing and admission
  are the same code, but the HOL seed differs. A physical capture with F0 counters would
  close this.
- **Interaction with fix 1** (immediate refusal on the shared receive pump): its first-slow
  counters were zero, but a whole-run provider-side refusal count is still unreported.
- **Device uplink before 7.75 s.** From 2.2 to 7.75 s the device created 12 items in 5.5 s
  (#81→#93). It is unknown whether those gaps were per-item pacing or a closed dequeue gate.
- **Provider SYN-ACK delay.** Is the 7.5–8.3 s generation latency or queue residence? It
  needs the admission stamp.
- **The four private fixes.** Each addresses a distinct boundary: receive pump, collapse ACK
  feedback, health receipt, SYN-ACK custody. None touches send-side residence or the
  replay heuristics, which is consistent with their combination still failing.
