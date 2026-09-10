# FLIGHTGATEFIX: peer review and research plan for "The Unreliable Flight Gate"

Status: research plan, 2026-09-10. Nothing in this document is implemented in
this tree. The reporter's commit 60edb09 (F1/F2), the F5 and F2c candidates,
the `transfer_flight_fallback_test.go` and `tun_outbound_wait_test.go` tests,
and the `xl2.sh` rig all live in the reporter's fork and are not in `connect`.

Sources reviewed: the report ("The Unreliable Flight Gate", 2026-09-09);
`transfer_flight.go`, `transfer.go` (send sequence, ack paths, ack worker),
`transfer_route_manager.go` (snapshot builder, weighted write order, carrier
affinity), `transport_p2p.go` and `transport_p2p_fast*.go` (carrier
properties, send loop, RTP fragmentation, reassembly, readiness),
`ip_remote_multi_client.go` (race-commit delivery), `tun.go` (outbound
backpressure), `LOWBAR.md` (the flight controller's design record),
`server/connect/perfvar` (the deterministic link harness) and
`server/proxy/proxy_device.go`.

## 1. Verdict in one paragraph

The report's central mechanism is real and is confirmed by the source: while
any unreliable carrier is active, `SendSequence` gates admission of every Pack
on one destination-wide flight whose items are only ever released by a
Transfer acknowledgement, and the flight is shared by writes that actually go
to the reliable relay. That contradicts the controller's own design record
("never limits reliable carriers", LOWBAR 2026-08-17), so F1 is aligned with
intent. But the report stops one layer short of root cause and leaves four
mechanisms untested that plausibly produce the same trace: acknowledgements
for anything received over p2p are pinned to the p2p lane and block the whole
ack worker; the loss signal that shrinks the window cannot distinguish
cross-carrier reordering from loss; every lost RTP fragment loses a whole
message, which amplifies packet loss into the 25-30 % message loss observed;
and the fast path has no liveness signal at all, so a dead lane is never
retired. Any of these can keep the window at its floor and the flight full
with zero real data loss. F1/F2 route around all of them, which is why the A/B
improved, and also why the A/B cannot tell us which one we fixed. F5 treats a
reentrancy bug as a queue-sizing problem and would also apply to the production
server proxy. The plan below turns each mechanism into a deterministic test
first, adds the missing mixed-carrier performance campaign, and only then
ranks fixes.

## 2. Claim-by-claim review

| # | Report claim | Verdict | Evidence in this tree |
|---|---|---|---|
| C1 | The gate is global: `canSendForKey` refuses any new Pack while the flight is full, including Packs bound for h1 | Confirmed | `transferFlightPolicy().limited = snapshot.unreliableTransferPath`, true when any active transport publishes `Unreliable` (`transfer_route_manager.go` snapshot builder). Admission is decided in the send loop before a carrier is chosen (`flightEligible`/`sendEligible`, `transfer.go` ~6100). |
| C2 | Loss only shrinks the limit; lost bytes stay in the flight until acknowledged | Confirmed | `trackUnreliableFlight` on an unreliable write disposition; the only releases are `releaseUnreliableFlight` from a selective or cumulative ack (`receiveAck`). A reliable resend of a tracked item does not release it (`observeCarrierWrite` keeps `unreliableCarrierObserved`). `reduceForLoss` halves to `activeMinimumByteCount`. |
| C3 | Floor is 8 KiB / 8 messages | Partly | Defaults: initial 8 KiB, minimum 8 KiB, maximum 256 KiB, initial 8 messages, minimum 4 messages, maximum 256. p2p tightens the maximum by its receive queue (`p2pUnreliableFlightByteLimit`, 256 KiB minus 16 KiB reserve, 255 messages). The trace's `m=6/6` and `b=7256/9840` fit an 8 KiB byte floor with a 4-message floor that has not yet been reached. |
| C4 | p2p is tried first (priority 0, weight 1.0) | Confirmed, with a sharper consequence | Write order is a weighted shuffle, not priority (`routeSnapshot.writeRoutes`); the writer match state is weighted (`NewRouteManager`). p2p `RouteWeight` 1.0 leaves every remaining route weight 0, so h1 receives a write only when the p2p route channel (capacity `ChannelBufferSize` = 4) is full. Every h1 write in the stock trace is therefore evidence that the p2p send goroutine was not keeping up. |
| C5 | Only writes that actually use the datagram lane are counted | Confirmed | `writeDisposition` classifies per write via `unreliableForMessageByteCount` = `FastPathReady()`. LOWBAR 2026-08-18 records this fix. The admission gate was never given the same treatment; that is the asymmetry F1 closes. |
| C6 | p2p lane loses 25-30 % of writes | Unverified; magnified by design | Every data-plane counter in the report is zero because `DataPlaneStats` is nil by default. A Transfer message is split into 1,188-byte RTP fragments with no fragment retransmit and a 64-slot, 2 s reassembler; losing one fragment loses the message. At 3 % packet loss a 10-fragment message is lost 26 % of the time. Packet loss, fragment count per message and reassembly evictions were not measured. |
| C7 | The p2p data plane stops delivering while probes keep passing | Unexplained, and nothing would notice | `FastPathReady` is codec bound + receive ready and never turns off. The SCTP lane has `SctpNoProgressTimeout`; the RTP lane has no acknowledgements and no watchdog, so a dead fast path is never retired and the flight is never reset by a route generation change. |
| C8 | The whole stream waits while h1 sits idle | Confirmed for admission; not the only reason acks stop | See §3.2: acks for p2p-received Packs are pinned to p2p. |
| C9 | F1 (reliable-only overflow) fixes the collapse | Plausible, not proven as root cause | The A/B is consistent with F1 removing the gate, and equally consistent with F1 moving the ack path off p2p (an h1-received Pack is acked over h1's priority lane). Needs the isolating tests in §5. |
| C10 | F2 (release on RTO, resend reliable-only) | Design risk | If the release reuses `acknowledgeForKey`, an RTO grows the window (acknowledge = delivery evidence). Needs a distinct forget primitive. Also removes the RTO congestion signal LOWBAR added on purpose (lost tail with no later selective ack). |
| C11 | F5 (250 ms outbound wait then drop) | Treats the symptom; affects production | The cycle is a reentrancy: the tun reader goroutine calls `SendPacket`, the race commit delivers buffered receive packets synchronously (`ip_remote_multi_client.go`, `commitRaceClientWithLock` then `deliverReceivePacket`), the callback writes into the same gVisor stack under the shard `writeLock`, netstack replies, and `tunLinkEndpoint.WritePackets` waits on `space` that only the blocked reader frees. The unbounded wait was added on 2026-08-02 for backpressure. `server/proxy/proxy_device.go` has the same shape (`ReadBatch` then `SendPacketsNoCopy` on one goroutine; the receive callback does `WriteBatch` into the same tun), so this is not rig-only. |
| C12 | F2c (single message in flight at the floor) | Premature | It patches the symptom of §3.3 and §3.4. Evaluate only after reordering and fragment loss are measured. |
| C13 | The phone SDK has the gate exactly as the socks client does | Confirmed for the gate; the rest is extrapolation | `DeviceLocal` uses the same `Client`. No device measurement exists; the device tun writes to the OS, so C11 does not apply there but does apply to the server proxy. |
| C14 | 5/8 stock collapses vs 0/8 fix | Accepted with caveats | Interleaved runs on one rig; "collapse" is a window under 5 Mb/s, and two fix runs died of F5 which the stock binary also carried. Per-window paired medians (64 vs 78 Mb/s) are a small effect on a noisy path. Not a phone or fleet result. |

## 3. What the source says the failure chain can be

Notation: a "lane" is one physical carrier (p2p fast, p2p legacy SCTP, h1
relay); "the flight" is `sendFlightController` on one `SendSequence`.

### 3.1 Route-wide gate, per-lane accounting

`updateActiveRoutesWithLock` sets `unreliableTransferPath` if any active
transport has `Unreliable` properties. The p2p send transport publishes that
whenever the fast path is merely negotiable (`supportsFastPath`), even before
`FastPathReady`. From then on every original Pack must pass
`canSendForKey`, which is decided before `writeDetailedWithCarrier` picks a
route. Only writes whose disposition is `unreliable` enter the flight. So the
flight fills with p2p writes and the gate blocks h1 writes too. When the flight
is full and the p2p items never get acked, throughput is the flight over the
ack RTT, which is the 1 Mb/s regime in the trace.

### 3.2 The acknowledgement path is pinned to the lane that received the Pack

`ReceiveSequence` records the transport type each Pack arrived on and its ack
worker writes the ack with `writeDetailedWithCarrierPreference(sendAck.transportType)`.
`affinityWriteRoutesByTransport` keeps every route of that type plus routes
that strictly outrank it. p2p has priority 0, so the eligible set for a
p2p-received Pack is `[p2p]` alone, written with the 15 s receive
`WriteTimeout`; for an h1-received Pack it is `[p2p, h1]`, tried non-blocking
in that order, then h1's ack-priority companion when the transport has one.
Consequences that the report did not consider:

- Acks for p2p Packs ride the lossy lane. A lost ack is an RTO on the sender,
  and every RTO on a tracked item calls `reduceForLoss`.
- The ack worker is one goroutine per receive sequence writing serially. When
  the p2p route channel (4 slots) is full because the p2p send goroutine is
  stuck in `WriteFastPathMessage` or `conn.Write`, one p2p-affine ack blocks
  every later ack of that sequence for up to 15 s. That alone explains
  "the gate never opens" with zero data loss.
- When the fast path is dead in one direction, acks for anything received on
  it are sent into the dead direction.

### 3.3 The loss signal is contaminated by cross-carrier reordering

`scheduleSelectiveAckRecovery` declares a gap after three later selective acks
(`SelectiveAckGapThreshold` = 3) and calls `reduceForLoss` when the gap item is
tracked. With p2p and h1 both live, adjacent sequence numbers take paths whose
latencies differ by a relay hop and whose queues differ by a 4-slot channel
versus a 32-slot route, so reordering across three positions is routine and
each such event halves the window and disables slow start. The F2c note that
F1 produced "900-2,800 gap resends per 2 s" is what a reordering-driven
scoreboard looks like, not what 25 % loss looks like.

### 3.4 Fragmentation amplifies packet loss into message loss

`writeMessage` splits a Transfer frame into `ceil(len / 1188)` RTP packets; the
reassembler needs all of them within 2 s and holds 64 slots indexed by
message id modulo 64. A coalesced 16 KiB Pack is 14 fragments. Message loss
is `1 - (1 - p)^n`, and a burst of more than 64 messages in flight evicts
incomplete slots. The flight controller's byte limit therefore sees far more
loss than the network has, and larger Packs, which the controller allows as
the window grows, are lost more often: a built-in oscillation toward the floor.

### 3.5 No liveness on the fast path

The SCTP lane has `SctpNoProgressTimeout` (forward acks). The RTP lane has
nothing: `FastPathReady` never becomes false while the codec stays bound, the
route is never retired, and the flight is never reset by a new route
generation. The report's "data plane stops delivering while probes keep
passing" is the expected behaviour of a lane with no evidence-based
liveness. The report leaves this open; it is the actual defect behind the
"never recovers" half on the provider side.

### 3.6 The tun reentrancy

Described in C11. Two independent facts: the race-commit delivery inside
`SendPacket` is synchronous (the code comments call it a deliberate exception),
and `WritePackets` waits without bound. Either fact alone is fine; together,
on any host where the tun reader and the receive callback share a goroutine,
they deadlock the stack. The hosted server proxy is such a host.

## 4. Hypotheses and the observation that separates them

Each hypothesis is stated so that a single deterministic test decides it.

| Id | Hypothesis | Decisive observation |
|---|---|---|
| H1 | The route-wide admission gate alone caps a mixed p2p+h1 sequence at the p2p window even with zero loss | Two routes, one unreliable that never acks, one reliable that acks everything: the second Pack must reach the reliable route without waiting. Fails today. |
| H2 | Ack pinning to the p2p lane stalls the ack worker and holds the flight full | Receive side with a p2p-received Pack whose only eligible ack route is blocked: later acks for h1-received Packs must not be delayed. Fails today. |
| H3 | Cross-carrier reordering with zero loss triggers `reduceForLoss` | Two lossless routes with different delivery delay: `UnreliableFlightGapCount` and `UnreliableFlightReductionCount` must stay 0. Expected to fail today. |
| H4 | Fragment loss, not packet loss, sets the observed 25-30 % message loss | Fast path over vnet with independent loss p and message sizes 1, 2, 8, 14 fragments: message loss must track `1-(1-p)^n`; then decide whether fragment-level recovery or size-aware admission is the right fix. |
| H5 | A dead fast path is never retired and the flight never resets | Fast path blackholed after readiness while STUN consent stays up: the route must be withdrawn within a bounded time and the flight generation must change. Fails today (no mechanism exists). |
| H6 | F1 fixes throughput on the rig by moving acks off p2p as much as by lifting the gate | Run H1's harness with F1 applied but acks forced to p2p affinity; then with the gate intact but acks forced to h1. Whichever alone recovers throughput identifies the dominant mechanism. |
| H7 | The tun stall is a reentrancy, not a queue size | Fill the outbound queue, then inject a packet that makes netstack reply from the injecting goroutine: with the reader parked, the write must return. Fails today with an unbounded wait. Same test with async race-commit delivery must pass without any drop. |
| H8 | The stock collapse needs p2p to be live | Already suggested by the report's 18-run set (clean runs had no p2p). Make it a hard control in the campaign: `exchange-h1` alone must never collapse under the same profile. |

## 5. Deterministic root-cause tests (connect, `go test ./`)

All build on `newTransferFlightTestClient` (route channels, the
`beforeResendCapacityWaitForTest` barrier, `SendRecoveryStats`) and the vnet
WebRTC factory (`newVnetWebRtcPeerConnectionFactory`). Names are proposals.

1. `TestSendSequenceUnreliableFlightDoesNotGateReliableSibling` (H1).
   Unreliable route `toPeerU` never acked, reliable route `toPeerR` acked by
   the test. Assert Pack 2 arrives on `toPeerR` without hitting the wait
   barrier, and `UnreliableFlightWaitCount == 0`. Today: the barrier fires.
2. `TestSendSequenceUnreliableFlightTracksOnlyUnreliableWrites` (H1 guard).
   Same routes, assert items written on `toPeerR` never raise
   `UnreliableFlightMaximumByteCount`. Passes today; keeps the F1 shape honest.
3. `TestReceiveSequenceAckAffinityDoesNotHeadOfLineBlock` (H2). Receive
   side: deliver Pack A tagged as received on p2p and Pack B tagged as h1 with
   the p2p route channel full. Assert B's ack is written within one write
   timeout of the p2p block, not after it. Today: serial ack worker blocks.
4. `TestReceiveSequenceAckPrefersReliableWhenUnreliableIsFull` (H2 fix
   contract). After the fix, a p2p-affine ack must fall through to h1 when p2p
   is full, and must still prefer p2p when it has room.
5. `TestSendSequenceReorderingAcrossCarriersIsNotLoss` (H3). Two lossless
   routes with the unreliable route delayed by N Packs relative to the
   reliable one. Assert no gap recovery and no reduction. Then the same with
   one real drop: exactly one gap recovery.
6. `TestFastPathMessageLossFollowsFragmentCount` (H4). vnet with independent
   loss `p` in {0.01, 0.03}; message sizes yielding 1, 2, 8, 14 fragments;
   assert the measured message loss is within a tolerance of `1-(1-p)^n`, and
   record reassembler evictions with `DataPlaneStats` enabled.
7. `TestFastPathBlackholeRetiresRouteAndResetsFlight` (H5). Mirror of
   `TestWebRtcIdleResumeSctpBlackholeReconnects` for the RTP lane: blackhole
   SRTP after readiness, keep STUN consent, assert the send route is withdrawn
   within the configured no-progress timeout and the sequence's flight
   generation changes. Today: no timeout exists; the test defines it.
8. `TestSendFlightControllerForgetDoesNotGrowWindow` (C10). Unit test for the
   forget primitive F2 needs: after `forget(bytes)` the byte count drops and
   `byteLimit` is unchanged; `acknowledge` still grows.
9. `TestTunInjectFromReaderGoroutineDoesNotDeadlock` (H7). Real `Tun`, fill
   the endpoint outbound queue by not reading, then from the same goroutine
   `Write` a SYN to a closed port so netstack replies with RST. Assert `Write`
   returns within 1 s. Run once against the reentrancy fix (async race-commit
   delivery) with no drop counter increment, and once against an F5-style
   bounded wait to characterise the fallback.
10. `TestMultiClientRaceCommitDeliversAsynchronously` (H7 fix contract). Using
    `testingNewMultiClient`, make the receive callback block on a channel and
    assert `SendPacket` still returns.

Every test runs under `-race`; 1, 3, 5, 7, 9 are expected to fail on the
current tree and are the regression guards for whichever fix lands.

## 6. Missing performance tests

### 6.1 In connect (`go test -bench`)

- `BenchmarkSendSequenceMixedCarriers`: one sender, two in-process routes
  (unreliable with configurable ack delay and drop, reliable with fixed
  delay). Report Packs/s and the fraction of loop iterations that were
  flight-blocked while the reliable route had channel capacity. This is the
  micro-benchmark for H1 and for F1's overhead.
- `BenchmarkReceiveSequenceAckWorkerUnderCarrierBlock`: ack latency
  distribution while one affine route is full. Micro-benchmark for H2.
- Extend `BenchmarkStreamFastWebRtcRoute` with vnet loss and fragment-count
  sweeps, `DataPlaneStats` on, reporting message loss and reassembler
  evictions per 10^4 messages (H4).

### 6.2 In PERFVAR (`server/connect/perfvar`)

`CONNECT_PERFVAR_ROUTE` today is exclusive: `p2p-fast`, `p2p-legacy`,
`exchange-h1`, `exchange-h3`, `exchange-auto`. The production condition in the
report, a pinned provider with p2p and the h1 relay active together, is not in
the matrix. Add:

- Route `p2p-fast+exchange-h1` (and `p2p-legacy+exchange-h1` as a control)
  with the same one-hop topology.
- Profiles: `clean-lan`, `cell-edge-5m-down-1m-up`, and two focused loss
  profiles on the p2p direction only (independent 1 % and 3 %, and one burst
  profile) with the relay direction clean, so the harness reproduces "lossy
  p2p, healthy relay".
- Schedule: a one-hop event that blackholes the p2p data plane at t+10 s while
  keeping ICE consent, and one that restores it at t+40 s (H5, recovery time).
- Workloads: `tcp-parallel` download (the report's four-stream shape) and
  `latency-under-load`.
- Metrics to add to the scenario record: per-window throughput with the
  report's 15 s windows and the under-5 Mb/s dead-window count; Transfer
  `UnreliableFlightWait*`, `GapCount`, `TimeoutCount`, `ReductionCount`; ack
  route-write wait by carrier; `P2pDataPlaneStats` fast/legacy send and
  receive counts and reassembly drops; count of send-loop iterations blocked
  while a reliable route had capacity (new counter, §7).
- Guards: the existing static low-bar matrix (`cell-edge-*` on `exchange-h3`
  and `p2p-fast`) must be INDISTINGUISHABLE or better for every candidate,
  because the flight controller exists for that regime and F1 changes how a
  hybrid connection behaves when H1 is also active.

Record campaigns in `tests/PERFVAR-MEASUREMENTS.md` under the PERFVAR
RUN-MAIN contract; five fresh repetitions, seed recorded, candidate and
control interleaved.

## 7. Instrumentation to add before any fix

Cheap atomic counters on `Client`, exported through `SendRecoveryStats` and
the receive stats, so the rig, the PERFVAR harness and a device can all
answer the same questions:

- `UnreliableFlightBlockedWithReliableCapacity`: iterations where
  `flightBlocked` was true and a non-unreliable route's channel had space.
  This single number decides how much of the stall is the gate.
- `ReceiveAckWriteWaitByTransport{p2p, h1, h3}` and
  `ReceiveAckWriteTimeoutByTransport`: ack pinning cost (H2).
- `UnreliableFlightGapReorderSuspected`: gap recoveries whose missing item was
  later acked without a resend having been written.
- `DataPlaneStats` enabled by default in the socks client, the server proxy
  and PERFVAR, plus a fragment histogram (1, 2-4, 5-8, 9-16, 17+) and
  reassembler eviction count.
- Fast path last-ack age per route (feeds H5's watchdog).

## 8. Fix candidates and how each is judged

| Id | Candidate | Addresses | Risk | Gate |
|---|---|---|---|---|
| A1 | Ack affinity falls through to a reliable route when the affine unreliable route is full, and never blocks the ack worker on an unreliable-only set | H2 | Acks for p2p Packs may take the slower relay; ordering is irrelevant for acks | Tests 3, 4; PERFVAR mixed route; low-bar unchanged |
| G1 | Admission gate applies only to writes that will use an unreliable lane: when a reliable route has channel capacity, admit and write reliable-only (the reporter's F1), with the flight untouched | H1 | On a constrained mobile link with H1 and hybrid H3 both active, overflow now goes to H1 TCP; LOWBAR's static queue gates must hold | Tests 1, 2; low-bar matrix; PERFVAR mixed route |
| L1 | Fast path no-progress watchdog: no Transfer ack progress on a route for T after outbound activity withdraws the route (same contract as `SctpNoProgressTimeout`), which resets the flight by generation | H5 | False withdrawal under long RTT; T must exceed the unreliable ack timeout floor | Test 7; PERFVAR blackhole schedule |
| S1 | Reordering-tolerant gap scoring across carriers: a gap counts as loss only when the later selective acks arrived on the same lane, or after one RTT of the slower lane | H3 | Slower loss detection on a single lossy lane | Test 5; low-bar H3 datagram medians |
| S2 | Size-aware unreliable admission: cap the datagram lane's per-message fragment count (or send only single-fragment Packs on the fast path when loss is observed) | H4 | Lower fast-path efficiency | Test 6; `BenchmarkStreamFastWebRtcRoute` sweeps |
| F2' | On RTO of a tracked item, forget it from the flight without window growth and resend on a reliable route if one has capacity | C2 | Loses the RTO congestion signal unless `reduceForLoss` still runs; must use a forget primitive, not `acknowledgeForKey` | Test 8; low-bar |
| R1 | Race-commit delivery becomes asynchronous (the removal-receive queue the report names), so `SendPacket` never injects into the caller's stack | H7 | Ordering of the first response packets relative to later ones; must keep the borrowed-packet ownership rule | Tests 9, 10; `tun_congestion_test.go`; server proxy soak |
| F5 | Bounded outbound wait then drop | H7 fallback | Changes backpressure that `tun_congestion` and TCP buffer tuning rely on; hides future reentrancy | Only as a guard after R1, with a drop counter that must read 0 in every campaign |
| F2c | Single message in flight at the floor | symptom of H3/H4 | Starves the lane that is supposed to prove itself | Not before S1/S2 are measured |

Ranking rule: a fix is accepted when its decisive test passes, no existing
test regresses, and the PERFVAR mixed-route campaign is IMPROVEMENT with the
static low-bar matrix INDISTINGUISHABLE or better. Run candidates one at a
time against the same seed set; do not land F1+F2 as a bundle.

## 9. Execution plan

Phase 0, instrumentation and reproduction (no behaviour change).
Add the §7 counters; enable `DataPlaneStats` in the socks client, server
proxy and PERFVAR; land tests 2 and 6 (pass today) and the failing tests
1, 3, 5, 7, 9 marked with the hypothesis they carry. Ask the reporter for
60edb09 as a patch against this tree, their windows/cdump files, and the
p2p ICE candidate pair type on the rig (host, srflx, relay).

Phase 1, isolate the mechanism on the rig and in PERFVAR.
Run the PERFVAR mixed route with stock, then with A1 alone, G1 alone, L1
alone, S1 alone. Report `UnreliableFlightBlockedWithReliableCapacity`, ack
wait by carrier, gap-reorder-suspected, and fragment loss per campaign. This
decides H6 and orders the fixes by measured contribution.

Phase 2, land in order of contribution, one at a time.
Expected order from the source reading: A1 (small, no policy change), L1
(bounded dead-lane), G1 (the gate, with the low-bar matrix as its guard),
then S1 or S2 depending on H3/H4, then F2' if RTO release still matters.

Phase 3, the tun reentrancy.
R1 with tests 9 and 10, then a server proxy soak. F5 only as a counter-backed
guard.

Phase 4, device confirmation.
One Android session block pinned to a provider with p2p live, before and
after, recording the same counters through the SDK, since the report's phone
claim is an extrapolation.

## 10. Open questions for the reporter

- Was `weightedRoutes` in effect on the rig binaries (it is in this tree), so
  that every h1 write is a p2p channel overflow? The TX split suggests the p2p
  send goroutine was blocked in the SRTP write; a goroutine dump during a
  collapse would show it.
- In the wedged dumps, what was the receive-side ack worker doing? A stack
  inside `writeDetailedWithCarrierPreference` with `preferredTransportType ==
  p2p` confirms H2 directly.
- Fragment count distribution of the Packs on the rig, and the selected ICE
  candidate pair. A relayed or symmetric-NAT path changes the loss model.
- Did any stock collapse occur with the p2p data plane provably delivering
  (fast receive counters advancing on the far end)? That separates H1/H2/H3
  from H5.
