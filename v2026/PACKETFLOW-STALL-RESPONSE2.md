# Rapid-browsing packet-flow stall: independent review (response 2)

Reviewer: Fable 5.1 (claude-fable-5-1)
Date: 2026-10-21

Revisions examined:

- Connect `bc1bfbbf2275832f5120de3ad6b046461575e0df` (working tree matches except the
  untracked `PACKETFLOW-STALL.md` and an in-progress `MEMSTEADY.md` edit). All
  `path:line` citations below are to this revision unless marked `overlay`.
- Server `20d0f074e9f6ac888bfc75270742d79c38f4fafd` (`server/connect/perfvar`).
- SDK `4b02b3822bb0db71aa97e3e532c2e3bf2b77381b` (`DefaultDeviceLocalSettings().SendTimeout`
  = 5 s, `sdk/device_local.go:384`).
- Vendored gVisor at `/Users/builder/urnetwork/gvisor` (`go list -m` resolution), used
  for one fact about pure-ACK sequence numbers.
- Overlays, read as the code that actually ran:
  - Uninstrumented arm: `/Volumes/data/urnetwork-strict-lane0-syn-v2.vinyoc/STRICT-LANE0-SYN-V2.overlay.json`
  - Instrumented arm: `/private/tmp/urnetwork-browsing-item-lineage-v2.maquYi/DIAGNOSTIC.overlay.json`
    (referenced by `LINEAGE100.RECIPE.v1.json`)
  - Direct/native control: `/private/tmp/urnetwork-native-browsing-direct-null-v1.y2zPxH/DIRECT.overlay.json`
  - Replacement files diffed against canonical (my scratch copy of the diff:
    `/private/tmp/claude-501/-Users-builder-urnetwork-server-connect/87de3e83-caa3-4e17-8f8d-bd129c7da2ea/scratchpad/response2-fable/overlay-diffs.patch`):
    `/private/tmp/urnetwork-browsing-combined-health-ack-pump-v1.UcI9oh/{transfer.go,ip_remote_multi_client.go}`
    (fixes 1 and 2 plus peer-receipt plumbing),
    `/private/tmp/urnetwork-selective-ack-health-fix-v1.LmuP0H/{transfer_ack_identity.go,transfer_peer_receipt.go}`
    (fix 3), `/private/tmp/urnetwork-synack-return-recovery-v2.viU2p8/{ip.go,ip_tcp_return_recovery.go}`
    (fix 4), `/private/tmp/urnetwork-browsing-carrier-v3.D7EDUn/transport.go` (+ H1 observer, diagnostic only),
    and the perfvar fixture afterimages in `/private/tmp/urnetwork-browsing-item-lineage-v1.HQe9zO/`,
    `/private/tmp/urnetwork-browsing-item-lineage-v2.maquYi/`, `/private/tmp/urnetwork-browsing-strict-lane-pair-v1.ByD2oT/`.

Time convention: absolute timestamps from the instrumented trace are written as
seconds inside epoch `1792604000`, so `428.19` means `1792604428.19` (wall clock
2026-10-21 12:40:28.19 in the raw log). Wire ids: `cid` is the fixture's socket
registry id, `flow` the exact inner 4-tuple id from the lineage report.

---

## 1. Summary

The stall is not one missing packet. It is a throughput collapse of the two
Transfer lanes (device→provider and provider→device) that then expresses itself
through three inner-TCP mechanisms the direct arm does not have. Ranked:

1. **Both Transfer lanes run at ~6–7 Packs/s each while the outer carrier is
   healthy, and inner packets wait seconds between admission and wire write.**
   Observed, not inferred (section 3.2). The most likely throttle is the
   delivery-sized window pacer that is on by default and active in this fixture
   (`WindowSizingFromDelivery`, `transfer.go:763-779`; a 2 MiB `ResendQueueBudget`
   is attached by the fixture, which makes the rule active): per-message
   quantized dispatch at a rate learned from the lane's own tiny delivery
   (`transfer_window_pacing.go:1242-1345`), plus controlled drain pauses of
   2×residence spaced ≥5 s (`transfer_window_pacing.go:1400-1465`, with
   `drainMaximumTime = AckTimeout = 60 s`, `transfer_window_receiver_rtt.go:47-52`).
   Confidence that the lanes are throttled: high (0.85). Confidence that the
   pacer is the throttle: medium (0.5); it has not been observed directly because
   no run captured `PacingDiagnosticSnapshot` (`transfer_pacing_diagnostic.go:120`).
2. **Provider return recovery replays already-delivered data on every late,
   in-order inner ACK** (`ip_tcp_return_recovery.go:161-171, 226-260`): a fixed,
   non-adaptive 1 s timer (`ip.go:494`) starts "recovery" whenever inner ACKs are
   late by more than a second, and the partial-ACK rule then replays one segment
   per arriving ACK. Reproduced deterministically at HEAD by a test I wrote
   (section 2); the instrumented trace shows exactly this pattern (section 3.3).
   This doubles return traffic for the affected flow, generates device dup-ACK
   items into the already slow uplink lane, and keeps old asset connections busy.
   Confidence the mechanism occurs in the run: high (0.85); that it is a material
   contributor to page deadlines: medium (0.5).
3. **Native admission refusal under the throttled lane converts queueing into
   inner loss**, repaired at the inner RTO, which the tunnel's delay variance has
   inflated to the 8 s cap (`cid 6`: RTT 2.22 s, RTTVar 2.79 s, RTO 8 s, two new
   RTO expirations between asset and HTML acquisition). Native offers are
   ready-only (`ip_remote_multi_client.go:7296-7300`) against 32 shared pack slots
   (`transfer.go:63, 7039`) and the resend-window gate (`transfer.go:9328-9357`);
   23 refusals (8.8% of bridge batches) by page 3's first byte. Confidence: medium
   (0.55) as the proximate cause of individual 8 s stalls; it depends on (1) to fill
   the queue.
4. **Provider-side controls wait behind data in the throttled lane and Multi's
   3 s dial-failure inference then re-races/unbinds the flow**: SYN-ACKs for the
   page-3 speculative dials were written by the provider 7.5–8 s after the device
   wrote the SYNs; "receive adopted a flow with no race and no client" is logged
   at the exact `connect_done` instant (`ip_remote_multi_client.go:8986`,
   `:10010-10019`, `:7347-7361`). This is why connection acquisition, not the
   reply, dominates every slow navigation in both full-tunnel runs (section 3.1).
   Confidence: high that it occurs (0.75), medium as a deadline contributor (0.6).
5. Outer-carrier loss recovery (0.5% loss, 40 RTO timeouts over the run) inflates
   inner RTT variance and the pacer's residence estimates, but it is not the
   stall driver: during page 3's 12.8 s stall the device's H1 socket had one
   retransmit and zero timeouts. Confidence as a direct cause: low (0.2).
6. The shared return-retention budget blocking SYN-ACK retention (overlay fix 4
   path, `retainReturnChunkWithFlags`) is possible on a budgeted provider but not
   in this fixture (64 MiB default, `ip.go:486-493`); confidence low (0.1).

**Single next experiment.** Re-run the identical frozen 100-page fixture (same
overlay, same seed, same deadlines) as a pair: (a) unchanged, (b) with one call
`clientconnect.SetWindowSizing(clientconnect.WindowSizingConstant)` at test start
(`transfer.go:784-786`; process-wide, no memory, depth or timeout change; the
constant window stays `MemoryScaledByteCount(2 MiB, 256 KiB)`). In both arms add
the existing `RemoteUserNatMultiClient.PacingDiagnosticSnapshot(origin)` to the
written / 5 s-crossing / first-byte boundaries, and count native refusals by
`sendAdmissionFailure` reason (`transfer.go:7564, 7573`). If (b) completes the
workload or moves the acquire/reply medians toward the direct arm's, the pacer is
the throttle and fix F2 is the target; if not, (a)'s diagnostic tells where the
lane time goes, and F1/F3/F4 remain.

---

## 2. What I examined and what I ran

Examined (read, not run): the doc, `CODESTYLE.md`, the four OUTCOME/ROOT-OUTCOME
receipts, `PRESSURE.v1.json`, `ITEM-LINEAGE.v1.json`, the three raw `go test -json`
logs, the overlay JSONs and every replacement file diffed against canonical, the
perfvar fixtures (`topology_test.go`, `bridge_send_tracker_test.go`,
`workload_test.go`, the driver/pressure/lineage afterimages, the direct-null test),
and the canonical sources named in the doc plus `transfer_window_pacing*.go`,
`transfer_pacing_diagnostic.go`, `ip_provider_reliable_ingress.go`,
`ip_native_ingress*.go`, `tun.go`. Prior notes were searched, not read end to end;
`PACKETFLOW-REGRESSION.md` P1 ("selected repair waits behind a drain", the
`f4881058` dependent-drain abort) is the only prior item that overlaps mechanism 1.

Commands that bear on a conclusion (all from `/Users/builder/urnetwork/connect`,
Go 1.27.2 `go1.27.2 darwin/arm64`):

1. Overlay-versus-canonical diffs (what actually ran in the failing arm):
   `git diff --no-index -U6 <canonical> <overlay>` for the seven replaced
   production files. Result: `transfer.go` +72/−31 (Client.run handoff timeout
   forced to 0, peer-receipt plumbing), `ip_remote_multi_client.go` +44/−14
   (pure-ACK bypass of collapse coverage, receipt cohorts, blackhole guard uses
   `unacknowledgedProviderSince`), `transfer_ack_identity.go` +5/−3,
   `transfer_peer_receipt.go` new, `transport.go` +8 (observer only), `ip.go` +15
   (SYN chunk reserved before the SYN-ACK callback), `ip_tcp_return_recovery.go`
   +19/−3 (SYN chunk kind and `synAckWithSequence` replay). Nothing in the overlay
   touches pacing, native admission, or the return resend timer.
2. Evidence extraction (Python, `-I`): `slownav.py` over the three runs' pressure
   JSON. Output saved at
   `/private/tmp/claude-501/-Users-builder-urnetwork-server-connect/87de3e83-caa3-4e17-8f8d-bd129c7da2ea/scratchpad/response2-fable/slow-navigations.txt`.
   Results are in section 3.1.
3. Existing deterministic tests at HEAD (to pin the semantics I cite):
   `go test -count=1 -v -run '^(TestTcpReturnBurstRecoveryFollowsPartialAcks|TestTcpReturnHealthyPathDoesNotReplay|TestTcpReturnTailBurstRecoveryNeedsOnlyInitialTimer|TestWindowPacingLongResidenceArrivesDuringControlledDrain|TestWindowPacingOwnerDrainReleasesSelectedRecovery|TestWindowPacingNaturalQueueDrainDoesNotStopService)$' .`
   Result: all six PASS, `ok github.com/urnetwork/connect 0.543s`.
4. A new deterministic witness for mechanism 2, added only through `-overlay`
   (file `/private/tmp/claude-501/-Users-builder-urnetwork-server-connect/87de3e83-caa3-4e17-8f8d-bd129c7da2ea/scratchpad/response2-fable/overlay/response2_return_delayed_ack_witness_test.go`,
   overlay `.../overlay/witness.overlay.json`, mapped to
   `connect/zz_response2_return_delayed_ack_witness_test.go`):
   `TestResponse2ReturnDelayedInOrderAcksDoNotReplayDeliveredData`. It drives the
   real `runReturnRecovery` worker with `testing/synctest` virtual time, the same
   harness shape as `ip_tcp_return_burst_recovery_test.go:20-106`: a 16-byte flight
   retained and fully delivered, no inner ACK for one `ReturnResendTimeout` (one
   timer replay accepted), then the four in-order cumulative ACKs arrive late one
   at a time. Desired: no further replays. Result at HEAD:
   `go test -count=1 -v -overlay=... -run '^TestResponse2ReturnDelayedInOrderAcksDoNotReplayDeliveredData$' .`
   → `--- FAIL ... RETURN_SPURIOUS_PARTIAL_ACK_REPLAY: 3 already-delivered segments
   were replayed on delayed in-order ACKs: [{seq:105 bytes:4} {seq:109 bytes:4} {seq:113 bytes:4}]`;
   identical under `-race` (`FAIL github.com/urnetwork/connect 0.731s`, no race
   report). The test is written as the post-fix assertion, so it fails before the
   fix and must pass after it (section 6, F1).

I did not run the 100-page fixtures, the direct-null fixture, or any campaign
runner, per the host rules. No repository file was modified; the only file I
created outside my scratch directory is this response.

---

## 3. Reading of the evidence

### 3.1 What the three runs establish (observed)

From the pressure JSON of each run (`slownav.py`), navigations with HTML first
byte ≥3 s or acquisition ≥3 s:

| Arm | Completed | Acquire median / max (s) | Reply median / max (s) | Page completion median / max (s) |
| --- | ---: | ---: | ---: | ---: |
| Full tunnel, uninstrumented | 63 | 0.959 / 31.652 | 0.835 / 10.250 | 2.27 / 35.30 |
| Full tunnel, instrumented | 50 | 1.289 / 21.068 | 0.920 / 22.791 | 2.27 / 43.75 |
| Direct, shaped (same driver) | 100 | 0.000 / 1.124 | 0.150 / 9.557 | 2.00 / 21.43 |

Every slow navigation in both full-tunnel runs has all four origin connections
busy (`active: 4`, `prior_assets: 4`; page 3 had 3) and in 20 of 24 the
acquisition phase (pool wait plus the speculative dial) is the larger term;
speculative `connect_done` arrives 5–35 s after `connect_start` (uninstrumented
page 10: acquire 31.65 s, connect_done 35.31 s; instrumented page 25: acquire
21.07 s, connect_done 43.36 s, reply 22.79 s). The direct arm's one slow page is a
pure reply stall (9.56 s on a reused connection), i.e. an inner RTO episode on
the shaped link, and its acquisition median is zero.

Two facts this table adds to the doc's reading:

- The tunnel's *steady-state* reply cost is ~0.9 s median against 0.15 s direct,
  six times the direct median and about seven outer round trips (outer RTT
  120–135 ms per the carrier TCPInfo). A physical path that healthy does not
  explain a 0.75 s per-request penalty; a lane that releases one Pack per
  dispatch interval does.
- Old asset connections stay busy because their bodies crawl (page completion max
  35–44 s against 21 s direct), and new connections are slow because SYN/SYN-ACK
  control packets wait like data. Both halves are needed for the HTML `GetConn`
  to wait 9–32 s behind `MaxConnsPerHost: 4` (driver afterimage line 296).

### 3.2 Lane cadence during the page-3 stall (observed)

From `outer_carrier.first_slow` and the endpoint samples in `ITEM-LINEAGE.v1.json`
(Transfer counters at 436.88 crossing, 441.11 written, 444.66 first byte):

| Interval | Device→provider Pack writes | Provider→device Pack writes (frames, bytes) | Device Transfer ACK writes |
| --- | ---: | ---: | ---: |
| 436.88 → 441.11 (4.23 s) | 26 (6.1/s) | 22 (54 frames, 55 KB; 13 KB/s) | 22 |
| 441.11 → 444.66 (3.55 s) | 30 (8.5/s) | 27 (86 frames, 77 KB; 22 KB/s) | 27 |

During the same window the device's H1 socket reports RTT 119–132 ms, cwnd 15–19,
retransmits 21→22, timeouts 3→3 (`outer_carrier.first_slow.{crossing,written,first_byte}`),
Transfer `timeout_resends` 0 on both sides, `PackHandoffDrop/Wait` 0 on both sides,
`AckRouteWriteBlockedCount` 0, provider `pack_waits`/`ack_blocked` 0. So the carrier
and the receive side were not the bottleneck; the lanes simply wrote ~6–8 Packs
per second each, symmetric in both directions, carrying 2–3 % of the 5 Mbps
downlink. The whole-run goodput logged by the reliability heartbeat is 15–82 KB/s
(`[rel] event=reward ... goodput=` lines at 12:41:16–12:44:16).

Historical maxima in the same samples: `RouteUnacknowledgedDuration` 12.56 s on
the device lane and 13.59 s on the provider lane, `ReliableLaneLongestAckGap`
12.68 / 13.59 s, `ReliableLaneStallOnsetOutstanding` 23, all already set by the
436.88 cut, i.e. the lanes each went 12–13 s without a Transfer ACK at some point
in the first 20 s of the workload. The doc is right that these are maxima, not a
live snapshot; they still prove at least one 12 s lane stall happened while the
outer socket had only three RTO timeouts.

### 3.3 The selected flow (`cid 6` = flow 1) re-read with sequence arithmetic

Facts from the lineage (positive):

- Provider SYN-ACK item 82 at 421.280 (seq/ISN 3220379585, ack 3220379586),
  delivered to the device at 421.365; `GotConn` for page 1 asset 2 at 421.3655.
- Device items 79 (421.374) and 80 (421.395), the handshake ACK and first request,
  reached the provider at 422.821: a 1.45 s uplink stall, matching the carrier's
  third RTO timeout. The provider replayed the retained SYN-ACK as item 84 at
  422.155 (overlay fix 4, fixed 1 s timer, `ip_tcp_return_recovery.go:226-227`),
  and the device answered it with item 81 at 422.228. Redundant, not harmful.
- Page 1 asset 2 is 30,976 bytes (`nativeBrowsingPageBytes`, driver afterimage
  176-185); its response of 31,080 bytes occupies exactly [3220379586, 3220410666),
  and the fetch completed (full length, hash-validated) at 423.100.
- Page 2 asset 1 (49,664 bytes) got `cid 6` at 425.2748, page 2's HTML first byte
  being 425.274.
- Device pure ACKs 93–97 were *written to the wire* at 428.10, 428.45, 428.77,
  429.09, 429.39 with TCP seq 3220379688 and ack 3220399498 → 3220402642 →
  3220408930 → 3220410666 → 3220410666. The vendored gVisor sends pure ACKs with
  `SndNxt` (`gvisor/pkg/tcpip/transport/tcp/snd.go:436-438`), so seq 3220379688 means
  the device TCP had not yet transmitted anything beyond request 1 when it built
  these ACKs, and ack 3220410666 is the end of response 1, which the HTTP client
  had completely consumed by 423.10. These four ACKs were therefore generated no
  later than ~423.1 and written 5–6 s later.
- Provider items 112, 113, 114 (`send_count 1`) at 428.19, 428.51, 428.85 carry
  seq 3220399498, 3220402642, 3220408930 with ack 3220379688: each is the segment
  immediately after the cumulative ACK that had just arrived. That is the
  return-recovery partial-ACK rule (`ip_tcp_return_recovery.go:169-171, 228-260`)
  replaying data the device already held. Item 97 is the device's dup ACK to the
  third replay.
- Request 2 (102 bytes at 3220379688) appears as items 101 (431.05) and 103
  (431.73), both admitted and wire-written within 8 µs of their first send attempt
  (`send_attempt` = `packet` timestamps), both Transfer-acknowledged in 141/208 ms.
  `cid 6`'s endpoint counters go from retransmits 1 / timeouts 1 at 425.27 to 3 / 3
  at 441.11, and request 2 is the only data `cid 6` sent in that window: it was
  transmitted at least three times, two of them on RTO expiry. The first copy is
  not in the ring.
- Page 2 asset 1 completed (49,664 bytes validated) and `Body.Close` ran at
  441.1115, 102 µs after `GotConn` for page 3 at 441.1114 (Go hands an idle
  connection to the waiter at body EOF; the order is expected and causal).
- Page 3's speculative dial: `connect_start` 431.88; device SYNs for flows 8 and 9
  written 436.15 and 436.55; provider SYN-ACKs written 444.074 (flow 9, item 196)
  and 444.481 (flow 8, item 199); `connect_done` 444.126; Multi logs
  `receive adopted a flow with no race and no client` at 444.126081
  (`ip_remote_multi_client.go:8986`). Provider item 197 at 444.223 carries three
  consecutive 1048-byte segments from 3220410666 with ack 3220379790 (request 2
  acknowledged, the page-3 HTML request written at 441.11 not yet); HTML first byte
  444.662.

Inferences (clearly marked as such):

- The device→provider lane lagged generation by ≥5 s around 423–429 (the ACK
  timestamps) and ≥3 s around 441–444 (the HTML request is still unacknowledged
  in the provider's ack field at 444.22 while the reply arrives at 444.66). Whether
  request 2's missing first copy was refused at the bridge (an RTO expiry then
  explains item 101 at +5.8 s) or sat before dispatch cannot be told from the
  ring; both readings put the loss of time before Transfer's first write attempt,
  where no counter in these runs looks ("pre-dispatch admission/pacing remains
  unknown" in the lineage scope note is exactly this gap).
- Item 197 cannot be the first transmission of response 2, which the client had
  fully received 3 s earlier; it is a batch of return-recovery replays of response
  2's head, i.e. at 444.22 the provider had not yet applied any inner ACK for
  response 2. Each such replay re-enters Transfer as a fresh item and is paced
  like data.
- The page-3 HTML reply of 3.55 s decomposes into ≥3.1 s of request transit to the
  provider's NAT and ~0.4 s of origin plus return; the acquisition of 9.23 s is
  entirely waiting for `cid 6`'s body to crawl to EOF while the speculative dial's
  SYN-ACK sat in the provider lane for 7.5–8 s.

### 3.4 Where the doc over- or under-reads

- "No Client-to-Pack handoff drops/waits, no pre-Multi TUN output drops, zero
  Transfer timeout-resend writes" is correct and important, but these counters
  all sit *after* Transfer admission or *before* the bridge. The lag demonstrated
  above lives between native admission and the first write attempt (pack slots,
  resend window gate, pacer), which none of the captured counters cover. The
  absence of drops there is not evidence of absence of delay there.
- "Native retransmissions alone are not a sufficient explanation" is right, and
  the direct arm shows why: it has 164 client / 752 peer retransmits and still
  passes because each flow's RTO stays near its own RTT. In the tunnel the inner
  RTO is driven to the 8 s cap by lane delay variance, so each inner loss (native
  refusal) costs 8 s rather than 0.3 s.
- Items 101/103 "secure custody promptly" is correct and I agree they refute a
  receive-side hold; they do not speak to where request 2's first copy went.
- The 23 refusals are treated as unattributable to `cid 6`; the endpoint counters
  (two RTO expirations on the only outbound segment) make it more likely than not
  that at least one copy of request 2 was refused, though not proven.
- The doc's two active investigations (refused-vs-held admission; return timer
  aging during admission) are real but second order here. The witness in section
  2 shows the return replay defect does not need an admission wait at all: late
  ACKs alone produce it.
- Provider `provider_admission_wait_ns` and `ack_blocked` being zero should not be
  read as "the provider lane was free": those count `retryReturnSend` failures
  (`ip.go:8969-9027`), not pacing waits inside the SendSequence write path.

---

## 4. Candidate mechanisms

### M1. Transfer lane throughput collapse through delivery-sized window pacing

Mechanism. Every TCP inner packet is sent with a Transfer ACK
(`ipPacketTransferAckForRequest`, `ip_remote_multi_client.go:16880-16882`), so every
such Pack is an `expectsAck` item and passes `paceWrite` before its first write
(`transfer.go:12906-12974`). Pacing is enabled by default
(`init()` stores `WindowSizingFromDelivery`, `transfer.go:763-779`;
`ApplyWindowSizing` sets `DeliverySizedWindowScale = 2` and the goodput target,
`transfer.go:795-842`), and in this fixture the rule is fully active because the
pressure test attaches a 2 MiB `ResendQueueBudget` to both endpoints and turns on
`ResendQueueRetainedByteAccounting` (pressure afterimage lines 434-450). The pacer
shares one `windowPacingService` per client across its sequences and:

- dispatches at most one physical message per interval once the estimated rate
  per interval is below one message: `estimateBytes = maxMessageByteCount;
  interval = max(interval, maxMessage/estimateRate)` (`transfer_window_pacing.go:1322-1332`);
- learns `estimateRate` from the service's own measured delivery
  (`transfer.go:12213-12231`, `12257-12267`), ends probe credit permanently after
  any congestion evidence (`queueObservedAt`, `transfer_window_pacing.go:1246-1251, 1293-1295`),
  and a recovery write counts as congestion;
- pauses all new (non-resend) writes of the service for `2×residence` (bounded by
  `drainMaximumTime`, which production sets to `AckTimeout` = 60 s,
  `transfer_window_receiver_rtt.go:47-52`) whenever mean residence exceeds
  min + 25 % and no natural drain is observed, with a cooldown of
  `max(5 s, 8×span)` (`transfer_window_pacing.go:1400-1465`,
  `transfer_window_pacing_drain_progress.go:11-27`).

A lane whose traffic is mostly 151-byte ACK Packs measures a tiny byte rate;
quantization then releases one Pack per `maxMessage/estimateRate`, and the
residence it measures (Transfer ACK round trip, inflated by the other lane's
pacing) feeds drain pauses of seconds. The two lanes price each other: slow ACK
delivery on one lowers the measured service on the other. This is a plausible
closed loop that settles at a few Packs per second per lane.

How it produces the observed behavior. Inner ACKs and requests queue for
seconds before their first write (3.3), the inner RTT estimate and RTO inflate
(RTT 2.2 s on `cid 6`), the pack slots fill and native offers are refused (M3),
asset bodies crawl so connections stay busy (3.1), SYN-ACKs queue behind data
(M4).

Positive evidence: the symmetric ~6–8 writes/s cadence on both lanes with a
healthy carrier (3.2); ≥5 s and ≥3 s admission-to-write lags on the device lane
(3.3); the 12–13 s route ACK gaps; the 0.9 s versus 0.15 s median reply.
Inference: that the pacer, rather than another pre-dispatch wait, is the cause.
Missing coverage: no `PacingDiagnosticSnapshot` (`transfer_pacing_diagnostic.go:120`,
fields `pacing_bytes_per_second`, `service_bytes_per_second`,
`drain_until_elapsed_ns`) was captured in any run; the SendSequence pack queue
depth and `sendAdmissionFailure` reasons at refusal time were not recorded.

Smallest deterministic discriminator. Unit: construct a `windowBurstPacer`
with a service whose measured rate is a few KB/s and `maxMessageByteCount` ≈ 1.2 KB
(the existing fixtures in `transfer_window_pacing_drain_arrival_test.go` build
services with virtual time), offer twenty 151-byte ACK-sized Packs, and assert
the release spacing; HEAD will space them by `maxMessage/estimateRate`. Workload:
the A/B in section 1 with `SetWindowSizing(WindowSizingConstant)`.

### M2. Provider return recovery: fixed timer plus replay-per-partial-ACK

Mechanism. `runReturnRecovery` (`ip_tcp_return_recovery.go:211-278`) arms a fixed
`ReturnResendTimeout` (1 s, `ip.go:494`) scaled only by attempt count, never by a
measured inner ACK RTT. When it fires it enters recovery with
`returnRecoveryEnd = receiveSeq` (229-232) and replays the oldest segment; then
`acknowledgeReturnWithLock` turns every *advancing* ACK below that frontier into
`returnRecoveryReady = true` (169-171), and the worker immediately replays the
segment after the new ACK point (228, 234-237). The rule is NewReno's partial-ACK
rule, valid after three duplicate ACKs (loss evidence), but it is applied equally
to timeout-triggered episodes, where a late train of in-order ACKs is the common
case on this path. There is no spurious-timeout detection although the provider
already parses the device's timestamp echo (`parsedTcp.timestampEcho`,
`ip.go:2249, 2423`) and stamps its own segments (`ip.go:6914-6921`).

How it produces the observed behavior. Each late ACK generates one redundant
1048-byte return segment, which the device answers with a dup ACK (item 97); both
ride the throttled lanes, lengthening the asset body's wire life and adding uplink
ACK Packs. The overlay's SYN-ACK retention (fix 4) uses the same timer, so a
handshake ACK delayed >1 s produces a redundant SYN-ACK (item 84) and another
device ACK (item 81).

Positive evidence: items 112–114 and 197 in 3.3; my witness test fails at HEAD with
exactly three spurious replays (section 2). Inference: the share of the 13–22 KB/s
return rate that was replay rather than new data (not countable from the ring).
Missing coverage: per-flow counts of timer-triggered versus dup-ACK-triggered
replays and of replays whose seq was below the device's `rcv.nxt`.

Discriminator: the witness test (fails before, passes after F1), plus the existing
`TestTcpReturnBurstRecovery*` roots which must keep passing (loss-triggered
repair stays one segment per partial ACK).

### M3. Native refusal converts queueing into inner loss; inner RTO is at its cap

Mechanism. `SendNativePacketBatchWithResults` → `sendParsedPacketGroupAttempt`
forces `timeout = 0` (`ip_remote_multi_client.go:7296-7300`) → `SendSequence.Pack`
refuses at once when either the resend-window gate is closed
(`awaitResendCapacity`, `transfer.go:7189-7246`; the gate is stored from
`CanAddWithQueueByteCount(..., sendWindowEstimate().Window)` at `transfer.go:9328-9357`)
or the 32 pack slots are taken (`acquirePackAdmission`, `transfer.go:7254-7303`;
`defaultTransferBufferSize = 32`, `transfer.go:63`, wired at `:7039`). Refusal is
correct per CODESTYLE (immediate, retry custody stays with the source TCP,
`ip_native_ingress.go:11-16`). The cost is set by the source TCP's RTO: gVisor's
RTO here is SRTT + 4·RTTVar capped at `TcpMaxRto` 8 s (`tun.go:163-169`), and on
`cid 6` the estimator read RTT 2.22 s / RTTVar 2.79 s, i.e. the cap.

The 32 slots are shared by every flow of the client, so one bulk flow's delayed
ACK burst (a 50 KB response is ~48 segments, ~24 delayed ACKs) can occupy them and
cause the refusal of another flow's SYN or 102-byte request. The provider side
already carves a prepaid partition for small NAT control packets when the data
admission refuses a group (`ip.go:10086-10099`); the device bridge has no such
partition.

Positive evidence: 23 refusals by 444.66 (5 by 425.27); `cid 6` two RTO
expirations between 425.27 and 441.11 with request 2 as its only outbound data;
RTO 8 s. Inference: which packets the 19 omitted refusals were. Missing coverage:
the refusal reason (`sendAdmissionResendCapacity` vs `sendAdmissionPack`), the
window estimate and queue depth at each refusal, and the refused packet's flow/flags.

Discriminator: a composed test at the real native entry (`SendNativePacketBatchWithResults`
with a `sendTransferForTest` barrier holding Transfer ACKs): fill the 32 slots with
one flow's ACK packets, offer another flow's SYN natively, assert it is refused at
HEAD (and admitted after F3's control partition), with the healthy-sibling,
no-false-ACK and pool-balance assertions CODESTYLE requires.

### M4. Control starvation in the provider lane and Multi's dial inference

Mechanism. A provider SYN-ACK is built after the origin dial and handed to the
same return path as data (`ip.go:5591-5598`; overlay additionally reserves its
sequence byte first), then paced in the provider→device sequence like any other
`expectsAck` Pack. Under M1 that lane releases a Pack every ~150–350 ms, with data
and replays ahead in FIFO (`waitForTurn`, `transfer_window_pacing.go:1355-1363`).
Meanwhile on the device, `inferredDialFailureTimeout` is 3 s
(`ip_remote_multi_client.go:10010-10019`): a SYN unanswered that long is treated as
a silent dial failure, the flow is unbound and re-raced (`:7347-7361`), and the late
SYN-ACK is later adopted (`:8930-8991`). The dial race (`DialRace 2`, stagger 2 s,
`tun.go:127-128`) and gVisor's SYN backoff add more SYNs into the throttled lane.

Positive evidence: SYNs written 436.15/436.55, SYN-ACKs written 444.07/444.48, the
adopt log line at `connect_done`, dial durations of 12–35 s in both runs, and
acquisition dominating every slow navigation. Inference: that the 7.5 s is lane
queueing rather than origin dial time (the origins are loopback listeners; the
provider's socket reader would have logged a dial error otherwise). Missing
coverage: provider-side timestamps for SYN arrival at the NAT, origin dial
completion, and SYN-ACK Transfer admission; the device's SYN emission times.

Discriminator: a provider-side test that holds the provider→device SendSequence
with N queued data Packs (pacing on, virtual time), then opens a new inner flow and
asserts the SYN-ACK's write order; at HEAD it is written after the N Packs. For
the inference side: a Multi test where the SYN item's Transfer ACK is delayed 4 s
with the exit otherwise live must not call `clientDialFailure`.

### M5. Shared return-retention budget blocking SYN-ACK retention (low)

With the overlay's `retainReturnChunkWithFlags(nil, initialSynSeq, tcpFlagSyn)`
(`overlay ip.go` after canonical `:5598`), the SYN-ACK waits in the shared budget's
arrival-order queue when the pool is exhausted by other flows' unacknowledged
chunks (`ip_tcp_return_recovery.go:101-113, 140-149`). In this fixture the provider
has no process memory budget, so the pool is 64 MiB (`ip.go:486-493`) and cannot be
the 7.5 s. On a budgeted provider (`max(320 KiB, share)`) it can be, and it is
worth a bound: a SYN-ACK is one 128-byte charge that should not queue behind data
chunks. Not supported by this evidence.

### M6. Outer-carrier loss recovery (present, not the driver)

0.5 % loss each way gives the device's H1 socket 628 retransmits / 40 timeouts
over 300 s; each RTO stalls both lanes for ≥200 ms and inflates every residence
estimate the pacer and the inner TCP see. During 425–444 it contributed one
retransmit and no timeouts, so it does not explain the page-3 stall; it does
explain why the pacer's residence baselines are noisy and why inner RTTVar is
large. The direct arm pays the same loss per flow without a shared choke point.

---

## 5. Answers to the five reviewer questions

1. **Where can accepted inner TCP work remain owned but not make forward
   progress long enough to inflate native RTO and keep an old asset connection
   busy?** (a) Inside the device's SendSequence between admission and first
   write: `awaitResendCapacity` / `acquirePackAdmission` succeed (the item is
   owned) and `paceWrite` then holds it for the pacer's turn, its quantized
   interval, or a drain pause (`transfer.go:12906-12974`,
   `transfer_window_pacing.go:367-441, 1400-1465`). Nothing is lost; everything is
   late, including the ACKs that clock the other direction. (b) Symmetrically in
   the provider's SendSequence, where data, replays and SYN-ACKs share the FIFO.
   (c) In the provider's per-flow return path, retained chunks stay owned until
   the inner ACK; while ACKs are late the fixed timer replays them and each
   replay re-enters the paced lane. These are the three owners; the bridge,
   TUN, Client receive pump and NAT ingress all show zero waits/drops in the
   captured snapshots and I found no code path there that holds a packet for
   seconds under these settings.

2. **Can any native refusal, post-Transfer refusal, ACK coalescing or TCP reorder
   path release retry authority too early, suppress its only viable retry, or
   unnecessarily replay work still waiting for first admission?** Release too
   early: I found none; native refusal (`ip_native_ingress.go:11-16`) and the
   overlay's zero-wait Client handoff (`overlay transfer.go` at canonical
   `transfer.go:4919`) leave custody with the source. Suppress the only retry:
   `pendingSequenceCoversWithLock` (`ip_remote_multi_client_group_completion.go:1001-1050`)
   suppresses only an exact duplicate of a still-owned original with unchanged
   ACK/window, which is safe while Transfer owns it; a packet refused at
   admission never becomes an owner, so its retry is not covered; the overlay's
   pure-ACK bypass (`overlay ip_remote_multi_client.go` at canonical
   `:10666-10670`) removes the one case that mattered. Unnecessary replay: yes,
   twice. The provider's return recovery replays delivered data on every late
   in-order ACK (M2, witnessed), and the overlay's SYN-ACK retention replays the
   SYN-ACK after 1 s while the handshake ACK is merely in transit (item 84). The
   provider's duplicate-ACK response to stale segments (`ip.go:6449-6463`) is
   correct TCP behavior and not a problem.

3. **Do provider return replay timers distinguish retention, admission, physical
   write, Transfer receipt and actual inner TCP progress correctly?** No. The
   timer is anchored at `returnProgressTime`, set on retention (chunk append,
   `ip_tcp_return_recovery.go:122`) and on inner ACK progress (168), then
   overwritten with `time.Now()` *before* the synchronous `receivePacket` (258-260),
   so a long admission wait is charged to the backoff (the doc's own finding, and
   the fix candidate it describes is right). More fundamentally the timer is a
   constant: it does not learn the inner ACK round trip, so on a path whose inner
   ACK latency exceeds 1 s it fires on every flight, and it treats a late ACK
   train as loss. Transfer receipt is correctly *not* used as delivery evidence
   (retention until inner ACK is right); physical write time is not recorded at
   all. The overlay's SYN-ACK chunk inherits every one of these properties.

4. **Is there an admission/ACK dependency cycle or pacing/window policy that only
   appears with several overlapping short flows and unfinished asset bodies?**
   Yes, the cycle in M1: the device lane's pacing rate is learned from delivery
   measured through Transfer ACKs that ride the provider lane, and vice versa;
   both lanes carry mostly small Packs in this workload, so both learn small
   rates, and each one's slowness lowers the other's measurement. Single bulk
   flows (the throughput campaigns) never see it because their Packs are
   MTU-sized and the delivered byte rate is high. Overlapping short flows add
   the second ingredient: a 32-slot pack queue shared across flows, so one flow's
   ACK burst refuses another flow's SYN or request (M3), and `MaxConnsPerHost: 4`
   turns a crawling asset body into a navigation wait (M4). The provider's return
   replay (M2) feeds the cycle with extra data and dup-ACK Packs.

5. **Smallest deterministic discriminator; positive evidence versus missing
   coverage.** For M2 it exists and fails at HEAD (section 2). For M1 the unit
   test in M1 plus the one-call `WindowSizingConstant` A/B (positive evidence so
   far: lane cadence and lags; missing: pacing diagnostic, queue depth). For M3
   the slot-exhaustion test in M3 (positive: refusal counts, RTO expirations;
   missing: refusal reasons and refused flows). For M4 the SYN-ACK ordering test
   (positive: 7.5 s SYN→SYN-ACK at the provider, adopt log; missing: provider
   NAT timestamps).

---

## 6. Potential fixes

Each fix keeps the ownership rules: shared callbacks still refuse at zero wait,
no retry owner is released early, no delivery ACK is published for undelivered
data, and no queue, budget or timeout is enlarged. None changes the 300 s or
600 s deadlines.

### F1. Provider return recovery: RTT-adaptive timer and spurious-timeout detection

Change (`ip_tcp_return_recovery.go`, `ip.go` TcpSequence):

- Measure the inner ACK round trip per flow: record first-send time per chunk
  (the chunk already exists; add a nanosecond stamp), sample on the cumulative ACK
  that first covers it, keep SRTT/RTTVar under the flow mutex; derive the resend
  delay as `max(ReturnResendTimeout, SRTT + 4·RTTVar)` with the existing
  exponential backoff and 30 s cap. Karn's rule: do not sample a chunk that was
  replayed.
- Eifel-style detection (RFC 3522): remember the timestamp value stamped on the
  replay (`timestampValue()`, `ip.go:6914-6921`); if the next valid cumulative ACK
  echoes a `timestampEcho` older than that value (`parsedTcp.timestampEcho`,
  `ip.go:2249, 2423`), the original was delivered and the timeout was spurious:
  set `returnRecoveryActive = returnRecoveryReady = false`, keep the chunks
  retained until acknowledged, do not replay. Without timestamps fall back to the
  rule "an ACK that advances beyond the end of the replayed segment ends a
  timeout-triggered episode"; keep the partial-ACK replay only for episodes that
  were entered on three duplicate ACKs.
- Anchor `returnProgressTime` after the synchronous callback returns, not before
  (the doc's own candidate), so admission waits are not spent from the backoff.

Ownership: the dedicated per-flow worker's wait is unchanged, retained chunks
remain retained until the inner ACK, no shared callback is touched, no ACK
semantics change. Expected effect: removes the replay-per-late-ACK storm and the
redundant SYN-ACK replays, cutting return bytes and uplink dup-ACK Packs during
lane congestion. Risk: a genuinely lost tail after a spurious-looking ACK pattern
must still repair; the dup-ACK path and the (now adaptive) timer cover it, and
the existing `TestTcpReturnBurstRecovery*` roots stay as controls. Regression
test: `TestResponse2ReturnDelayedInOrderAcksDoNotReplayDeliveredData` (fails at
HEAD, section 2), plus a timestamp-echo variant and a lost-tail control.

### F2. Window pacer: do not let control-sized Packs or ACK-only lanes starve

Conditional on the M1 discriminator. Candidate changes (`transfer_window_pacing.go`,
`transfer.go` `paceWrite`):

- Exempt Packs at or below a control size (e.g. ≤ 2× the H1 framing of a 60-byte
  inner packet) from quantized dispatch and drain pauses, or give the service a
  rate floor `ResendQueueMinByteCount / residence` so a lane can always carry its
  floor window per round trip; this is a floor on *release rate*, not on memory.
- Bound a controlled drain by `2×minimum round trip`, not `2×residence`, and skip
  drains when outstanding bytes are below one burst (nothing to drain).
- Do not count a return-recovery replay as congestion evidence that permanently
  ends probe credit; it is the inner TCP's recovery, not the carrier's.

Ownership: pacing sits after admission inside the send worker; refusal, custody
and ACK publication are untouched; the window byte bound is unchanged. Expected
effect: lane release rate no longer collapses to one Pack per interval on
ACK-dominated traffic. Risk: more small writes on the carrier (the H1 burst
coalescing still applies); bulk-transfer campaigns must be rerun to show no
throughput regression. Regression test: the M1 unit test (release spacing of
twenty ACK-sized Packs at a learned low rate) fails before and passes after.

### F3. Control-class admission on the device; control priority on the provider lane

- Device: reserve a small partition of the 32 pack slots for packets that are
  SYN, pure ACK, or ≤ a control byte size, keyed by the existing
  `sendSchedulingKey`, mirroring the provider's prepaid NAT control partition
  (`ip.go:10086-10099`). Refusal stays immediate; the total slot count does not
  grow (carve-out, not addition).
- Provider: give `receiveRecoveryModeDedicatedTcpControl` packets and SYN-ACKs a
  priority scheduling key that the SendSequence writes ahead of queued data of
  the same destination (the ACK path already has a priority route,
  `transfer.go:1755, 4300`).

Ownership: regenerable controls keep their regenerable semantics; retained
SYN-ACK custody (overlay fix 4) is unchanged; data custody is unchanged. Expected
effect: SYNs and requests are admitted during ACK bursts; SYN-ACKs stop waiting
7 s behind data. Risk: priority inversion must not starve data: bound the control
partition to a few slots. Regression tests: the M3 slot-exhaustion test and the M4
SYN-ACK ordering test.

### F4. Multi dial-failure inference must not fire on the lane's own queueing

Gate `synWaitExceeded` on the SYN item having been Transfer-acknowledged (the
exit has actually received it) or on the exit's receive clock showing silence
for the same flow, rather than on wall time since the native offer. Ownership:
no custody change. Expected effect: no unbind/re-race while the SYN is still in
the device's own queue. Risk: a genuinely dead exit is detected later by the
existing blackhole/receive-silence gates. Regression test: SYN Transfer ACK
delayed 4 s with a live exit → no `clientDialFailure`, no adopt log.

### F5. Bound SYN-ACK retention on a budgeted provider (M5, low priority)

Reserve the 128-byte SYN chunk from a small per-NAT control reserve instead of
the shared data pool's FIFO. Regression test: pool exhausted by data chunks, new
flow's SYN-ACK still sent within one scheduler turn.

### Instrumentation (no behavior change, needed by every item above)

Capture `PacingDiagnosticSnapshot` and the SendSequence queue depth at the
written / crossing / first-byte boundaries; record `sendAdmissionFailure` reason,
flow, flags and window estimate for every native refusal; per-flow return
recovery counters (timer-triggered, dup-ACK-triggered, Eifel-suppressed, bytes
replayed); provider NAT timestamps for SYN arrival, origin dial completion and
SYN-ACK admission. Each is a copied scalar on an existing lock-free or
per-flow-locked path, consistent with the existing diagnostic conventions.

---

## 7. Validation plan

1. Root-cause regressions, deterministic, normal and `-race`: F1's witness and
   its lost-tail/timestamp variants; the M1 release-spacing test; the M3 native
   slot-exhaustion test at the real `SendNativePacketBatchWithResults` entry with
   a healthy sibling, pool reconciliation and no-false-ACK assertions; the M4
   SYN-ACK ordering and dial-inference tests. All existing `TestTcpReturn*`,
   `TestWindowPacing*`, five failed-send/collapse regressions and the overlay's
   four fix tests remain controls.
2. The frozen 100-page fixture, unchanged deadlines, same overlay closure plus
   the candidate(s), run as single diagnostic runs first with the new
   instrumentation: completed owners (target 100), HTML first-byte median/p95/max,
   acquire and reply medians (targets: toward 0 s and 0.15 s), native refusals by
   reason, replays by trigger, lane writes/s, pacing rate and drain time per
   boundary, `cid`-level RTO/retransmit counters, dial durations, adopt-log count,
   pool roots drained to zero.
3. Uninstrumented paired performance validation: canonical versus
   canonical+fix, ≥5 runs each on the cell-edge and clean-lan profiles, reporting
   completion count, first-byte distribution, elapsed, goodput, outer/inner
   retransmits and peak memory; plus the existing bulk throughput cells (single
   flow, both directions) to prove F2 does not reduce bulk transfer; plus the
   32 MiB / 64 MiB memory qualification unchanged.

## 8. Open questions and missing evidence

- Direct measurement of the pacer state during a stall (rate, drain, queue
  depth); until then M1's attribution to pacing is inferred from cadence and lag.
- The first copy of request 2 on `cid 6` (refused, or held before dispatch?), and
  the flows/flags of the 19 omitted refusals.
- Provider-side timing of SYN arrival → origin dial → SYN-ACK admission → write
  for flows 8/9; whether the 7.5 s is entirely lane FIFO.
- Whether the SYN-ACK batching seen in items 196/199 (6–8 frames per Pack) means
  controls were coalesced behind data in one physical Pack, which would make F3's
  priority write the decisive change for acquisition.
- Whether the P1 dependent-drain trap (`PACKETFLOW-REGRESSION.md`) still has
  variants after `f4881058`; the 12–13 s route ACK gaps early in the run are
  consistent with it and were not captured by any drain counter.
- The user-visible 5–15 s pauses on iOS over Wi-Fi: this synthetic cell-edge
  reproduction has the same shape (acquisition-dominated), but the mobile build
  attaches its own budgets (`transferBudgetShareByteCount`), so the window floor,
  pool sizes and pacing ceilings differ; the A/B in section 1 should be repeated
  with the mobile resource profile before claiming the field cause.
