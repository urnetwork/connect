# Low-bar network delivery plan

Status: living implementation plan
Last updated: 2026-08-18

## Outcome

Make URnetwork feel materially more responsive and reliable than the current
VPN path on high-latency, low-throughput, burst-loss cellular links, while
keeping memory, battery use, and transmitted bytes bounded.

The radio's capacity cannot be manufactured by a tunnel. "Better than the cell
connection" therefore means that URnetwork adds less delay, avoids redundant
recovery, protects interactive traffic from bulk traffic, recovers quickly from
short outages and path changes, and approaches the direct-link bulk goodput. It
may beat a direct application flow in loaded latency or completion time when its
scheduler and recovery make better use of the same link; it must not promise
more raw capacity than the link has.

"One bar" is a product scenario, not a repeatable network measurement. All
engineering decisions and release gates use measured rate, RTT, jitter, burst
loss, queue delay, outage, MTU, and path-change traces.

The tunnel remains IPv4-only. Do not enable, advertise, route, or locally
blackhole IPv6 until remote providers can route it.

## How to maintain this document

This file is part of the implementation, not a one-time proposal.

- `[x]` is implemented and covered by a deterministic test.
- `[~]` is implemented experimentally or has incomplete validation.
- `[ ]` is proposed.
- `[!]` is a confirmed correctness or deployment issue.

Every low-bar change must update the applicable phase, findings, and results in
this file. Record the exact client/server revisions, PERFVAR profile and seed,
transport configuration, command or result artifact, run count, and whether the
result is cold or warmed. Append results; do not replace an unfavorable run.
Lock release thresholds after the Phase 1 baseline so they cannot be moved to
fit a candidate.

## Decisions

1. **Transfer owns end-to-end tunnel delivery.** Every tunneled TCP Pack uses a
   Transfer ACK, including over direct and nominally reliable carriers. Carrier
   reliability ends with that connection generation and cannot prove delivery
   across a disconnect or route replacement. QUIC DATAGRAM avoids a redundant
   carrier payload retry for the common packet path; the bounded large-message
   stream remains an in-generation retry layer, never a replacement for
   Transfer commit.
2. **QUIC still uses transport ACKs.** QUIC packet ACKs, loss detection,
   congestion control, pacing, path validation, and cryptographic handshake are
   fundamental to QUIC and cannot be disabled. "QUIC with no ACKs" means no
   QUIC application-data retransmission, which is the property provided by
   DATAGRAM, not an ACK-free QUIC connection.
3. **Use a reliable control/fallback stream.** Authentication, capability
   negotiation, connection-scoped control, contract-only Packs, and routed
   frames that do not fit one live DATAGRAM use the reliable QUIC stream. Small
   complete tunnel messages use DATAGRAM.
4. **Use a bounded hybrid.** With the global 1,100-byte tunnel MTU and QUIC's
   safe 1,200-byte initial packet, an ordinary worst-case encrypted IP Pack does
   not fit one DATAGRAM and uses the reliable stream; smaller frames that fit
   use one DATAGRAM. Production never fragments one Transfer message across
   multiple lossy DATAGRAMs. Transfer sequencing and ACKs do not change with
   the carrier lane. Explicit two-fragment controls remain available for
   compatibility tests and measurement, not production selection.
5. **Receive never waits for admission.** The invariant in
   [CODESTYLE.md](./CODESTYLE.md#receive-callbacks-must-not-block) applies to the
   shared Client receive pump, every callback handoff, and each carrier/socket
   reader handing data to a bounded route queue. A full destination is dropped
   and counted immediately. Sender-side backpressure may block.
6. **Preserve security and routing policy.** Transport changes do not weaken
   encryption, CFAA policy, SMTP policy, kill-switch behavior, provider
   eligibility, or route authentication.
7. **Keep fallback.** UDP can be blocked, rate-limited, or deprioritized by a
   carrier. Auto mode must retain the TLS WebSocket/H1 path and converge without
   repeatedly racing both transports over a scarce uplink.

## Current architecture and evidence

`TransportModeH3` is a custom protocol over QUIC, not HTTP/3 request semantics.
Legacy peers still serialize all platform messages on one reliable
bidirectional stream. New peers negotiate envelope version 2 in the
authenticated `Auth` exchange and split bounded routed Transfer frames onto
QUIC DATAGRAM while retaining that stream for authentication, liveness, and
larger routed frames. The production-selection stack is therefore transitional:

```text
inner application TCP/UDP
        |
IP mux + multi-TCP collapse prevention
        |
Transfer sequence, ACK, resend, and ordered receive
        |
H1: TLS/WebSocket/TCP     or     H3 legacy: one reliable QUIC stream
                              or H3 v2: control/large stream + Transfer DATAGRAM
        |
connect/provider
```

For an inner TCP flow, H1 can combine inner TCP recovery, Transfer recovery,
and outer TCP recovery. Current H3 replaces outer TCP with a reliable QUIC
stream but still combines inner TCP, Transfer, and outer-stream recovery.
Nested reliable transports can amplify retransmissions and queueing after loss;
the same class of TCP-over-TCP failure is documented for tunnel protocols in
[RFC 8229](https://www.rfc-editor.org/rfc/rfc8229.html#section-12.1).

Current code facts:

- H3 uses `quic-go`, one bidirectional control stream, and, only after explicit
  new/new negotiation, RFC 9221 DATAGRAM for complete routed Transfer frames.
  Either old endpoint stays on the existing stream path on the same connection.
- Auto mode enables H3 and H1 together at equal priority and retains every
  healthy carrier at that priority; DNS and DNS pump follow at priorities two
  and three. An ordered destination sequence stays on its first healthy H1 or
  H3 route instead of shuffling between two congestion controllers on every
  frame. A full preferred route backpressures the sender, and only actual route
  withdrawal moves the sequence to its surviving equal-priority peer.
  Production H3 reachability still depends on the UDP Proxy Protocol v2 ingress
  rollout.
- Legacy H3 writes ready-only batches of at most 16 messages / 64 KiB retained
  storage. Negotiated hybrid generations allocate that stream batch lazily only
  if a large routed frame selects the stream, and otherwise retain bounded
  DATAGRAM scratch and incomplete-message state.
- QUIC keepalive now owns connection liveness independently from the possibly
  blocked DATAGRAM writer. The retained application ping cadence still needs a
  radio-energy and bytes-on-wire experiment.
- Network-change `Kick` closes and redials instead of preserving a connection
  through QUIC migration.
- `Allow0RTT` is enabled. Authentication currently needs a replay-safety review
  before any credentials or state-changing control can be accepted as early
  data; TLS 1.3 explicitly requires replay defenses for 0-RTT
  ([RFC 8446](https://www.rfc-editor.org/rfc/rfc8446.html#section-8)).
- Transfer normally ACKs, orders, and retries Packs. Its cold resend interval is
  two seconds; sampled intervals use a bounded mean-RTT-derived estimator.
- Recovery now follows the lane that accepted each exact write, not the
  route-wide H3 capability. Only a successful DATAGRAM write consumes the
  unreliable flight and uses the two-second retry ceiling. A successful hybrid
  stream write remains Transfer-ACKed but leaves payload retry to QUIC for up
  to eight seconds; withdrawal of that exact route schedules immediate
  Transfer recovery on a replacement carrier. Adding a sibling route while the
  original remains active does not manufacture a duplicate.
- A normal Pack can coalesce at most two frames and 1,100 bytes of application
  messages. The global advertised IPv4 tunnel MTU is 1,100 bytes. Exact sizing
  produces 1,288 Transfer bytes (1,316 bytes with the H3 envelope) for one
  worst-case packet, so QUIC's safe 1,200-byte initial packet selects the
  reliable stream. A single-packet inner MTU of 944 bytes (934 for two
  coalesced packets totaling the MTU) is the exact one-DATAGRAM boundary. The
  observed 1,515-byte opening item is contract-only and also uses stream.
- Most ordinary IP traffic to one provider currently shares one ordered
  Transfer lane. A missing sequence number can therefore delay later Packs for
  unrelated inner flows even after all callback handoffs are nonblocking.
- Native P2P fast traffic now publishes its actual unreliable RTP/SRTP carrier
  semantics to Transfer. Transfer therefore keeps end-to-end ACK recovery but
  limits its unacknowledged flight instead of treating a live data channel as
  proof of delivery. The carrier readers enqueue without waiting into a
  16-message / 256-KiB queue; that count includes the item held by the one
  owned worker that may wait on RouteManager. The
  separate count and byte limits preserve the former worst-case payload memory
  while admitting the small ACK/control bursts seen on the one-bar trace.
- Transfer queues are sized in bytes, but at a 64 kbit/s uplink even 1 MiB is
  more than two minutes of serialization. A memory-safe queue can still be a
  catastrophic latency queue.

Existing PERFVAR measurements are evidence, not yet a complete mobile release
baseline. See
[PERFVAR.md](../server/connect/perfvar/PERFVAR.md) and
[MEASUREMENTS.md](../server/connect/perfvar/MEASUREMENTS.md).

| Existing result | Observation |
| --- | --- |
| Fixed TCP window, 256 KiB to 2 MiB | 41.045 to 402.276 Mbit/s in the recorded clean throughput case; retained 2 MiB was the useful plateau candidate. |
| Whole five-tuple groups vs singleton packets | 336.497 to 416.331 Mbit/s (+23.73%); send time fell 17.34% and carrier bytes fell 3.98%. Preserve grouping as long as it adds no batching wait. |
| Clean mobile-surrogate upload | P2P fast 453.382, H1 412.005, legacy 383.776, H3 213.393 Mbit/s. Current H3 was 48.2% below H1 in this case, so QUIC alone is not the answer. |
| Provider TCP timestamps at 1 s RTT / 64 KiB upload | 25.087 s to 3.081 s (8.14x). Endpoint TCP behavior can dominate tunnel results. |
| Provider window growth | Raising the provider maximum helped warmed tests; raising TUN capacity to 16 MiB overflowed the 4096-packet queue and timed out. More buffering is not a general low-bar fix. |
| Mixed Auto, 256 KiB warmed upload at 1/0.25 Mbit/s | A refreshed five-run current-tree matrix measured Auto at 14.983 s / 876,468 B, forced H3 at 15.268 s / 818,234 B, and forced H1 at 30.285 s / 1,976,566 B using all correctness-valid samples. Auto and H3 were about 50% faster and 56--59% lower-byte than H1. |
| Native P2P fast, same workload/profile | The initial current-tree baseline delivered only 1/5 exact payloads: 44.646 s / 561,768 B median, with small receive-queue drops in four runs. Publishing unreliable carrier semantics plus the byte-bounded receive queue delivered 5/5 in 22.921 s / 478,585 B with zero receive-queue drops: 48.7% faster and 14.8% lower-byte. |

The authoritative five-run Wi-Fi/LTE/mobile-poor campaign is incomplete. No
transport should ship from the historical clean results alone.

## Target recovery architecture

```text
                         one QUIC connection
                        /                   \
reliable control stream                    QUIC DATAGRAM
auth + capabilities              Transfer Pack / Transfer ACK / NoAck
QUIC retransmits control             QUIC does not retransmit payload
                                             |
                              Transfer is the end-to-end commit authority
```

QUIC streams are ordered byte streams. Different streams avoid transport
head-of-line blocking between streams, but bytes within a stream remain
ordered. QUIC DATAGRAM adds unreliable message delivery to the same connection
([RFC 9000](https://www.rfc-editor.org/rfc/rfc9000.html),
[RFC 9221](https://www.rfc-editor.org/rfc/rfc9221.html)). A packet containing a
DATAGRAM frame is ACK-eliciting for QUIC congestion control, but the DATAGRAM
frame is not retransmitted and its packet ACK does not prove application
delivery. Transfer's end-to-end ACK remains authoritative.

| Recovery mode | Carrier | QUIC payload retry | Transfer payload retry | Intended traffic |
| --- | --- | --- | --- | --- |
| `control_stream` | Reliable stream | Yes | Transfer ACK also applies to contract Packs; connection-only auth/ping has no Transfer retry | Auth, capabilities, contract-only control, connection control |
| `transfer_datagram` | DATAGRAM | No | Yes | Default reliable tunnel Packs and Transfer ACKs |
| `datagram_noack` | DATAGRAM | No | No | Traffic explicitly classified as NoAck / stale-is-useless |
| `quic_stream` | Reliable stream data lane | Yes | Yes for TCP and every other ACK-required Transfer Pack | Contract-only control and routed frames above the bounded packet-lane threshold |

Inner endpoint TCP still retransmits because it is the end-to-end transport
between the application and destination. The goal is not to alter endpoint TCP;
it is to prevent redundant copies of the same inner segment from being injected
while Transfer already owns its delivery. Multi-TCP collapse prevention is the
coordination point for that ownership.

### DATAGRAM sizing and fragmentation

QUIC DATAGRAM cannot fragment an application datagram. Its sender must honor the
peer's negotiated maximum DATAGRAM frame size and the current path payload
ceiling. QUIC itself starts with a minimum 1200-byte UDP payload capability, but
that is not 1200 bytes of Transfer content after QUIC framing.

The versioned envelope has bounded Transfer-aware fragmentation rather than
relying on IP fragmentation:

- derive a conservative fragment payload from the active QUIC path and
  negotiated DATAGRAM maximum;
- carry version, Transfer message identity, fragment index/count, and bounded
  integrity-checked length;
- cap fragments per message, reassembly bytes per peer and globally, and
  reassembly lifetime;
- accept duplicate and reordered fragments without extending their lifetime;
- never allocate from an attacker-controlled declared total before validating
  all limits;
- keep retransmission ownership in Transfer. Prefer making fragments selectable
  Transfer recovery units if measurements show that resending a whole Pack for
  one missing fragment wastes the constrained uplink;
- use Datagram Packetization Layer PMTU Discovery rather than ICMP assumptions
  ([RFC 8899](https://www.rfc-editor.org/rfc/rfc8899.html)); and
- benchmark one, two, three, and four-fragment Packs against the separate-stream
  candidate. Production permits one fragment only; the explicit fragmented
  controls keep multi-fragment framing and reassembly covered without selecting
  it for live traffic.
  `quic-go` documents that its DATAGRAM path is not optimized for high
  throughput, so this is a measurement gate, not an assumption
  ([quic-go DATAGRAM documentation](https://quic-go.net/docs/quic/datagrams/)).

The tunnel MTU is now globally 1,100 by product decision. Its exact worst-field
encrypted Transfer growth is pinned beside the carrier: one full 1,100-byte
packet and two coalesced half-size packets would require two DATAGRAMs at the
safe initial QUIC size and therefore select stream. Smaller complete messages
stay on the one-DATAGRAM lane. The former 1,440-byte packet exceeds even the
optimistic hybrid threshold.

### Hybrid selection

Size alone is not enough to choose DATAGRAM versus stream. Selection order is:

1. required recovery semantics;
2. logical Transfer lane;
3. current path DATAGRAM ceiling and fragment count; then
4. a measured stream threshold, if a stream candidate remains beneficial.

Carrier selection never changes Transfer sequence identity, acknowledgement,
or replay ownership. A streamed sequence item can therefore delay later
DATAGRAM items at the logical receiver, but it cannot create a silent delivery
hole when that stream connection disappears. Keep stream selection rare and
bounded, measure its nested in-generation retry cost, and safely fall back to
the legacy stream when either peer lacks the negotiated hybrid version.

## Receive-path correctness

Mechanical head-of-line blocking and logical ordered-sequence blocking are
different problems.

### Mechanical blocking

The shared receive pump must never wait for a destination queue or a retiring
worker. This is now corrected:

- [x] `Client.run` calls `receiveBuffer.Pack(receivePack, 0)`.
- [x] Inbound ACK handoff calls `sendBuffer.Ack(..., 0)`.
- [x] A zero-timeout Pack that encounters a retiring/replaced receive sequence
  cancels the old worker and drops instead of calling `WaitForExit` inline.
- [x] Pack/byte and ACK handoff drops are counted with power-of-two diagnostics.
- [x] Deterministic regressions fill Pack and ACK queues ahead of an unrelated
  source and verify that the unrelated source is delivered. A separate test
  pins a retiring generation whose exit never arrives and verifies immediate
  return.

Relevant tests are in
[`transfer_callback_backpressure_test.go`](./transfer_callback_backpressure_test.go):

- `TestClientReceivePackHandoffDoesNotBlockUnrelatedSource`
- `TestClientReceiveAckHandoffDoesNotBlockUnrelatedSource`
- `TestReceiveSequenceReplacementDropsWithoutWaiting`
- `TestReceiveSequenceClosingGenerationDropsWithoutWaiting`

Remaining work:

- [x] Export aggregate Pack/byte and ACK handoff drops into PERFVAR carrier
  observations for the device, provider, and stream-P2P intermediary Clients.
  Measurement boundaries read the existing lock-free `Client.ReceiveStats()`;
  no metrics callback or receive-path wait was added. Client-generation changes
  are explicit, retain both raw lifetime endpoints, and are never subtracted as
  if lifetime counters were continuous.
- [x] Audit forward, WebRTC signal/data, stream, device TUN, provider NAT, and
  RPC receive handoffs against the same rule. There are no production Client
  forward subscribers. Signal shards and the native RTP fast path drop on full;
  stream lifecycle work is bounded/asynchronous; provider datagrams use bounded
  zero-wait sender shards. Pion ICE publication now uses the receive-side
  zero-timeout marker. A saturated reliable RPC substream closes that RPC
  generation instead of blocking the shared websocket reader; it cannot skip a
  byte fragment and continue safely. The synchronous device-TUN injection and
  provider local-NAT TCP socket return remain the only documented exceptions,
  with end-to-end and dedicated-flow regressions.
- [x] Add structural regressions for the production Client receive-subscriber
  inventory, the absence of forward subscribers, direct blocking calls/channel
  sends, literal zero timeouts, and the SDK-owned provider subscriber. A new or
  changed callback now fails until its boundary is audited explicitly.

### Logical sequence blocking

`ReceiveSequence` intentionally holds a later reliable Pack behind a missing
`nextSequenceNumber`. That is correct inside one recovery lane. It becomes
cross-flow head-of-line blocking when many independent five-tuples share the
lane.

- [ ] Measure gap delay by inner five-tuple and identify which current
  `TransferKey` combinations share a sequence.
- [ ] Prototype a bounded number of logical data lanes: one control lane plus
  4 and 8 five-tuple-hash data-lane candidates. Do not create an unbounded
  sequence per flow.
- [ ] Add the lane as a logical routing key following every producer and
  consumer rule in `CODESTYLE.md`; never use a transport-local QUIC stream ID.
- [ ] Share byte budgets across lanes so lane count does not multiply memory.
- [ ] Verify that loss in one lane does not delay another, and that in-lane
  ordering, contract state, encryption role/companion, reform, replay, and
  fallback compatibility remain correct.

Queue admission must happen before Transfer assigns a sequence number. Dropping
an already-numbered item creates the very gap the scheduler is meant to avoid.

## Multi-TCP collapse prevention

The current collapse prevention is a useful starting point. It is enabled by
default and applies to every tunneled TCP route because TCP always requires
Transfer ACK recovery,
always passes SYN/RST and sequence/ACK/window progress, includes FIN in the
sequence edge, passes zero-window changes, suppresses identical retransmits,
and releases a held retransmit after a fixed 1500 ms. Direct TCP no longer
bypasses collapse; only non-TCP traffic can use the NoAck policy.

The next version should bind collapse to explicit recovery ownership rather
than only an ACK-required boolean:

- [ ] `transfer_datagram`: collapse eligible; link a held inner retransmit to
  the outstanding Transfer item that represents it.
- [ ] `quic_stream`: quantify simultaneous QUIC and Transfer retry while
  preserving Transfer commit across connection loss.
- [ ] `datagram_noack`: bypass collapse (TCP is never in this class).
- [ ] Replace the fixed hold with a bounded adaptive value derived from the
  Transfer loss/RTO state. Release immediately when admission, route selection,
  or write fails.
- [ ] Record suppressed segments, timer releases, failure releases, bytes
  avoided, and whether a released copy preceded successful progress.
- [ ] Preserve and extend the SYN/RST, FIN, zero-window, retransmission, route
  change, and direct-recovery regression suite.

This coordination does not make Transfer more reliable than inner TCP. It
prevents the tunnel from carrying redundant copies while Transfer is already
recovering a Pack.

## Queueing and scheduling

On a low-rate uplink, byte bounds alone are insufficient. Queue bounds must also
be expressed as estimated serialization time and packet age.

- [ ] Add a bounded flow-fair scheduler before Transfer sequence admission.
  Start with deficit round robin / FQ-CoDel-derived behavior, not a single FIFO;
  [RFC 8290](https://www.rfc-editor.org/rfc/rfc8290.html) describes the
  flow-queue and controlled-delay principles.
- [ ] Reserve small capacity for connection control, DNS, TCP SYN/FIN/RST, and
  small interactive traffic. Traffic class is a scheduling hint, never a
  correctness or security decision.
- [ ] Use control, interactive, default, and bulk service classes with a shared
  global byte ceiling. Per-flow and per-class ceilings must not multiply the
  process memory budget.
- [ ] Do not wait to form a batch. Group messages already ready at the same
  instant, retaining the measured carrier-byte and syscall win without adding a
  batching timer.
- [ ] Once reliable data is admitted and numbered, retain it until ACK,
  terminal failure, or connection-generation replay policy resolves it.
  NoAck/stale-is-useless datagrams may expire by age.
- [ ] Derive adaptive queue targets from estimated rate and a target queue delay
  while retaining hard byte and packet caps. A bad estimator must fail bounded.

## Transfer recovery work

Transfer remains the key differentiator, so tune it from measured low-bar
behavior rather than attempting to remove it.

- [~] Instrument original sends, ambiguous retransmit samples, selective ACKs,
  cumulative ACKs, gap duration, RTO fire, retry count, and useful recovery by
  lane. The database-free carrier A/B now records per-Pack attempt timelines,
  maximum retry gap/attempt count, Pack write span, and selective-gap,
  ACK-tail, and cumulative-probe writes. Lane attribution and useful-recovery
  classification remain.
- [ ] Compare the current mean-RTT estimator with an SRTT/RTTVAR estimator and
  Karn-style exclusion of ambiguous retransmit samples. RFC 6298 is the
  baseline, not a mandate to copy TCP constants unchanged
  ([RFC 6298](https://www.rfc-editor.org/rfc/rfc6298.html)).
- [ ] Measure a lower cold-start RTO only with safeguards against burst loss and
  asymmetric cellular delay. The current two-second cold retry is visibly long
  for an isolated loss but may prevent waste on a paused radio.
- [~] Tune selective-gap probing so a lost cumulative ACK cannot leave every
  item paused for the full selective-ACK window. A reordering-safe threshold of
  three later selective ACKs, four-hole burst bound, minimum-live-RTT probe,
  and once-per-item guards are implemented with deterministic regressions.
  They reduce the measured DATAGRAM tail but cannot compensate for the current
  oversized initial QUIC flight; keep this experimental until flight control
  and the final focused race run pass.
- [ ] Measure ACK compression on constrained uplinks. Prefer cumulative and
  selective information per byte; do not let ACK compression delay recovery of
  a sparse interactive Pack.
- [ ] Make recovery state survive a healthy transport path change where safe,
  rather than treating every `Kick` as loss of all connection context.

## QUIC work beyond DATAGRAM

- [ ] Capture qlog and correlate QUIC packet loss, PTO, congestion window,
  pacing, path validation, and migration with Transfer retries and inner TCP
  retransmits.
- [ ] Enable ECN where the platform and path support it, with validation and
  automatic fallback as required by QUIC.
- [ ] Use DPLPMTUD and expose the effective DATAGRAM payload ceiling.
- [ ] Replace the unconditional five-second application ping with a measured
  policy based on NAT lifetime, platform suspension, radio promotion cost, and
  idle reconnect latency. Do not run two keepalive mechanisms.
- [ ] Preserve session tickets across compatible load-balancer backends and
  measure cold handshake, resumed handshake, and path-change reconnect.
- [ ] Make authentication replay-safe and wait for 1-RTT for state-changing
  control unless a specific idempotent 0-RTT design is proven.
- [ ] Test QUIC connection migration and NAT rebinding before replacing the
  current close/redial path. Generic UDP load balancing may route a rebinding to
  another backend even though QUIC identifies connections independently of the
  five-tuple.
- [ ] Retain an H1 fallback and cache UDP-blocked evidence for a bounded period
  so Auto does not spend scarce bytes repeatedly probing an unusable mode.

`quic-go` currently exposes its RFC 9002 Reno controller rather than a stable
pluggable congestion-controller API
([quic-go congestion-control documentation](https://quic-go.net/docs/quic/congestion-control/),
[RFC 9002](https://www.rfc-editor.org/rfc/rfc9002.html)). Do not fork it as the
first optimization. Establish the DATAGRAM, scheduling, and recovery baseline
first. Then evaluate BBR, CUBIC, Sprout, or another cellular-oriented controller
only through fairness, outage, and loaded-latency A/B tests. BBR and Sprout are
research candidates, not preselected answers
([BBR paper](https://research.google/pubs/bbr-congestion-based-congestion-control/),
[Sprout paper](https://www.usenix.org/system/files/conference/nsdi13/nsdi13-final113.pdf)).

ACK_FREQUENCY may eventually reduce return-path QUIC ACK bytes, but it remains
an evolving draft and can reduce loss responsiveness. It is not a production
dependency
([current IETF draft](https://datatracker.ietf.org/doc/html/draft-ietf-quic-ack-frequency-14)).

## Measurement program

### Compared modes

Every candidate uses the same endpoint, provider, route, security policy, and
profile:

1. direct network without URnetwork, where the harness supports it;
2. current H1 TLS/WebSocket;
3. current custom QUIC reliable-stream H3;
4. QUIC DATAGRAM + Transfer fragmentation;
5. DATAGRAM + bounded logical lanes and scheduler; and
6. the explicit large-message stream hybrid, only after the DATAGRAM baseline.

### Profiles

Retain the existing clean, Wi-Fi-good, LTE, mobile-poor, and single-region
profiles. Add a curated stress grid rather than an unreviewable full Cartesian
product:

| Dimension | Required points |
| --- | --- |
| Down/up rate | 256/64 kbit/s, 1/0.25 Mbit/s, 5/1 Mbit/s, existing 10/2 and 50/10 Mbit/s |
| Base RTT | 120, 300, 800 ms, plus existing regional cases |
| Jitter | 25, 100, 300 ms |
| Burst loss | 0.5%, 2%, 5%, with independent and two-state burst models |
| Queue delay | 100, 500, 2000 ms |
| Disruption | 1/3/10 s outage, NAT rebind, address change, UDP blocked |
| Outer MTU | 1280, 1400, 1500 bytes and an MTU reduction during a connection |
| Rate change | fast-to-slow, slow-to-fast, and oscillating radio capacity |

Treat these as engineering stress profiles until field traces exist. Collect
privacy-safe physical traces containing timings and aggregate link/path
properties, never payload or destination identity. Make traces deterministic
and replayable in PERFVAR.

### Workloads

- sparse DNS plus one small HTTPS request;
- cold and warmed web page object sets;
- interactive request/response and RPC;
- one inner TCP upload/download;
- 4/16/64 parallel inner TCP flows;
- latency-under-load with a bulk upload and download;
- inner QUIC/UDP and NoAck traffic;
- SMTP negotiation and TLS flows already represented by product policy;
- mixed web, mail, and blocked traffic from the DeviceLocal/DeviceRemote
  synthetic path;
- short outage, handover, NAT rebind, MTU change, and UDP-blocked fallback; and
- long idle followed by one interactive request.

### Metrics

- DNS completion, connect time, TLS time, first byte, full completion;
- p50/p90/p95/p99 latency and maximum stall by workload and inner flow;
- goodput, completion ratio, and time spent below an application-useful rate;
- queue bytes, packets, oldest age, estimated serialization delay, and drops by
  scheduler class/lane;
- Transfer sends, ACKs, retries, RTO, gap time, and handoff drops;
- QUIC loss/PTO/cwnd/pacing/DATAGRAM drops and stream retransmitted bytes;
- inner TCP retransmits and collapse suppress/release outcomes;
- carrier bytes in each direction and useful-byte efficiency;
- process RSS, retained pool/queue bytes, allocation spikes, CPU, wakeups, and
  mobile energy proxy; and
- reconnection, migration, fallback, and time-to-first-useful-packet after a
  disruption.

Use at least five measured runs per candidate/profile cell after a separately
reported warm-up. Pin revisions, random seeds, and CPU/resource limits. Report
all runs plus median and tail, not only the best. Run the canonical commands in
`PERFVAR.md`; add any new command there and link its output here.

### Provisional release gates

Freeze exact gates after Phase 1 measures variance. Until then, the target is:

- zero packet corruption, policy bypass, encryption downgrade, sequence/lane
  cross-talk, unbounded allocation, or IPv6 advertisement;
- no shared receive handoff waits for queue space or worker exit;
- sparse interactive p95 at least 20% below current H1/H3 on mobile-poor and no
  more than 10% above direct when direct completes;
- loaded interactive p95 at least 25% below the better current tunnel mode;
- isolated bulk goodput at least 90% of direct and no more than 5% below the
  better current tunnel mode;
- recovery after connectivity returns within `max(3 * measured RTT, 2 s)` for
  an already-established session, excluding a required fresh authentication;
- hard byte, packet, fragment, lane, and age bounds under sustained overload;
  and
- no material battery/wakeup regression in long-idle and intermittent-traffic
  device tests.

If direct traffic fails a scenario, report that explicitly rather than treating
the tunnel's completion as an infinite percentage win.

## Implementation phases

### Phase 0 — correctness and observability

- [x] Enforce zero-timeout Pack admission in the shared Client receive pump.
- [x] Enforce zero-timeout inbound ACK admission.
- [x] Drop rather than wait on receive-generation replacement when admission is
  nonblocking.
- [x] Count Pack/byte and ACK handoff drops and add deterministic regressions.
- [x] Expose Pack/byte and ACK handoff drops through the lock-free
  `Client.ReceiveStats()` snapshot.
- [x] Make H1, H3, H3Dns, H3DnsPump, legacy P2P, and fast P2P carrier-reader
  route admission zero-wait; count mode-specific refused messages and bytes.
- [x] Make connect-server socket/exchange receive handoffs zero-wait and expose
  bounded-label refusal counters.
- [x] Move resident control throttling and forward construction/storage checks
  out of Client callbacks. Control remains ordered; forwards use bounded
  destination-stable worker shards. Reliable-control overflow retires the
  resident generation instead of silently skipping acknowledged state.
- [x] Record device/provider platform carrier refusals at PERFVAR's exact
  interval boundary and invalidate contaminated schema-5 measurements.
- [x] Complete the adjacent receive-callback audit. The shared Client pump,
  exact-delivery and encryption fixtures, control-sync collector, mux and
  multi-client provider echoes, contention benchmarks, WebRTC signal/data,
  stream lifecycle, SDK migration, RPC mux, and `connectctl sink` use bounded
  zero-wait admission, generation-local asynchronous work, or inline counters.
  Deliberately blocked callbacks remain only in hostile-callback
  isolation/lifecycle regressions and the two documented ownership exceptions.
- [ ] Add cross-layer IDs/timestamps that correlate an inner packet, Transfer
  item/fragment, QUIC send, ACK, retry, and collapse decision in test telemetry.

Exit: every shared receive path has a deterministic saturation test, and a
single trace can attribute each retry and queue delay to its owning layer.

### Phase 1 — reproducible baseline and field model

- [~] Finish the authoritative five-run current H1/H3/P2P campaign. The
  current static 256-KiB warmed-upload matrix now has five exact H1, H3, Auto,
  and corrected P2P samples on `cell-edge-1m-down-250k-up`. Direct calibration
  variance invalidated some aggregate comparisons, and directions, workloads,
  dynamic profiles, and physical radios remain.
- [~] Add a database-free, production-Transfer carrier A/B that can compare
  legacy H3 stream framing with negotiated H3 DATAGRAM on the same deterministic
  cell-edge link. The harness now connects two real `Client` instances through
  production gVisor TUNs and QUIC, exercises Pack/ACK/sequence/RTT/dedup/resend,
  verifies exact payload delivery, and reports link and carrier counters. It
  still needs the broader workload matrix and checked-in result artifacts.
- [~] Add the curated cell-edge profiles, disruptions, MTU changes, and
  variable-rate profiles to PERFVAR. Three static composite device profiles
  now cover 5/1 Mbit/s, 1/0.25 Mbit/s, and 256/64 kbit/s with coupled RTT,
  jitter, loss, queue, and MTU conditions. Three selectable one-hop schedules
  now isolate fast-to-slow-to-fast capacity, a one-second outage, and a live
  1,400-to-1,280-to-1,400 outer-MTU transition in direct calibration and all
  four current carriers. NAT rebind, address-change, UDP-blocked, oscillating
  capacity, and field-trace replay still need campaign integration.
- [ ] Add privacy-safe trace capture/replay and collect representative physical
  iOS and Android cellular traces.
- [ ] Measure direct traffic beside each tunnel mode and freeze release gates.

Exit: checked-in profiles and artifacts reproduce the current tails closely
enough to rank candidates consistently.

### Phase 2 — QUIC DATAGRAM prototype

- [x] Add authenticated, versioned capability negotiation. `Auth` carries a
  separate offer and server-accepted version, while QUIC transport parameters
  independently negotiate RFC 9221. An old server echoes the unknown offer but
  cannot set acceptance, so a new client remains on the stream without a
  reconnect; an old client sends no offer, so a new server also stays legacy.
- [~] Add bounded DATAGRAM framing, fragmentation/reassembly, duplicate and
  reorder handling, and Transfer ACK/retry integration. Hybrid envelope v2
  carries a connection-local message id, total length, CRC-32, fragment
  offset/index/count and uses a 1,360-byte target. The production limit is one
  fragment per message; explicit tests may raise it to eight. Other limits are
  8 KiB/message, 32 incomplete messages, 64 KiB/connection, 8 MiB/handler, and
  5 seconds. Production selection admits only complete frames that fit one live
  DATAGRAM; a worst-case 1,100-byte tunnel packet uses stream at the safe
  initial path size. Transfer retries the whole DATAGRAM-carried frame after
  carrier loss.
- [x] Send complete routed Transfer frames—including Packs and ACKs—over
  DATAGRAM; retain auth and ping on the reliable stream. A live local QUIC test
  verifies a multi-fragment Pack and a single-datagram ACK in opposite
  directions.
- [~] Support UDP-blocked and legacy-peer fallback without a reconnect loop.
  Mixed-version H3 fallback is implemented and the H1 path remains intact.
  Auto starts H3 and H1 together at equal priority and retains every healthy
  carrier at that priority; DNS and DNS pump follow at priorities two and
  three. Sticky path learning and production UDP activation remain open.
- [~] Add qlog/Transfer correlation and all fragment/drop counters. Lock-free
  client snapshots and bounded-label server Prometheus counters cover sends,
  receives, bytes, duplicates, malformed/checksum drops, timeouts, limits, and
  send errors. Cross-layer packet/Transfer/qlog correlation remains open.
- [~] Fuzz framing/reassembly and test loss, duplication, reordering, truncation,
  oversized declarations, timeout, cancellation, and memory recovery. A fuzz
  target and deterministic boundary, reorder, duplicate, overlap, corruption,
  timeout, per-peer/shared-budget, close-recovery, mixed-version, live Pack/ACK,
  and race tests are present. A sustained fuzz campaign plus deterministic
  last-fragment loss through full Transfer retry still remain.

Exit: Transfer is demonstrably the only packet-lane payload retransmitter and
the end-to-end commit owner on every lane, all buffers are bounded, and the
candidate beats current H3 without regressing H1 completion.

### Phase 3 — lanes, scheduling, and collapse ownership

- [ ] Add bounded logical lane negotiation and carry its routing key through
  every in-order, optimistic, batch, resend, replay, encryption, and reply path.
- [~] Add the pre-sequence flow-fair scheduler and control/interactive reserve.
  The sender now carries a stable scheduling key into a packet-aware per-flow
  scheduler, skips blocked flow heads without blocking receive callbacks, and
  retains one bounded ACK-required new-flow reserve on flow-isolating H3.
  Requested NoAck traffic can bypass a full recovery window only on an
  acknowledged, generation-matching contract that can debit the exact next
  bounded logical-group chunk. Contract rotation or exhaustion returns it to
  ordinary admission before serialization. Explicit service-class negotiation
  and a separately budgeted control lane remain open.
- [ ] Bind multi-TCP collapse holds to explicit Transfer recovery items and
  adaptive recovery timing.
- [ ] Run 1/4/8-lane A/B tests under loss and 4/16/64-flow workloads.

Exit: loss in one data lane does not stall another, memory stays within the
single shared budget, and interactive tails improve under bulk load.

### Phase 4 — recovery and hybrid optimization

- [~] A/B the RTO estimator, cold retry, ACK compression, and gap probing. The
  first full-Transfer A/B isolates a 13.5--14.0 second DATAGRAM completion tail
  versus 2.1--2.3 seconds for legacy H3 at 250 kbit/s upload. A one-DATAGRAM-per-
  Pack workload reproduces the tail with zero reassembly timeouts, pointing to
  coarse synchronized Transfer retry rather than fragmentation. Guarded,
  paced selective-gap recovery cuts representative DATAGRAM completion to
  4.3--5.8 seconds with the production 32-frame route boundary, but legacy is
  still typically 2.2--3.4 seconds. The next candidate is a byte-bounded,
  adaptive Transfer flight window used only for negotiated unreliable
  carriers. Two successful full-TUN runs with the 1,100-byte MTU, the earlier
  one-DATAGRAM data lane, and TCP always under Transfer ACK completed cold
  64 KiB uploads in 9.420 and 26.565 seconds. The spread is too large and the
  slower result is worse than the earlier candidate; this is a correctness
  milestone, not a performance win. DATAGRAM remains disabled for rollout
  until it wins the frozen multi-run A/B. Compact contract heads and bounded
  unreliable-carrier recovery now make the common case substantially faster,
  but an earlier set still spans 6.834--83.934 seconds and includes a separate
  65.280-second cold route-readiness timeout. Safe 1,200-byte QUIC startup and
  correcting only the access-side carrier TUN removed measurement-interval
  decrypt failures, but three stock-dependency runs still included a
  50.251-second route-readiness tail. Readiness telemetry localized the mirror
  failure to the edge/server physical TUN, which retained the same incorrect
  inner-MTU boundary. After correcting both physical endpoints, four cold,
  separate-process runs completed established transfer in 7.676--10.076
  seconds and 248,828--324,850 wire bytes; route readiness clustered at
  6.433--8.718 seconds with zero payload-decrypt failures. A subsequent frozen
  five-run reproduction tightened established transfer to 7.097--8.319
  seconds, wire cost to 248,735--273,208 bytes, and route readiness to
  6.787--8.847 seconds. All nine fully corrected cold runs completed without a
  startup or transfer tail. The refreshed TCP-ACK-invariant campaign then
  compared four same-profile cold controls: fragmented H3 median 8.413 seconds
  / 266,629 bytes, one-DATAGRAM hybrid H3 6.793 seconds / 243,633 bytes, H1
  5.180 seconds / 307,814 bytes, and legacy H3 stream 3.995 seconds / 339,130
  bytes. The hybrid is the only H3 DATAGRAM candidate that improves both time
  and bytes over fragmented H3, so its one-fragment limit is now the production
  default. It remains slower than both reliable-stream controls; broader
  profiles, workloads, and Auto-mode interaction remain release gates.
  Per-write lane classification then removed the route-wide unreliable policy
  from stream and H1 writes. The first retained five-run campaign measured a
  4.060-second / 208,684-byte median. After adding immediate exact-route
  retirement recovery, a second five-run campaign measured 5.000 seconds /
  209,654 bytes. The newer distribution is slower but remains 40.6% faster and
  21.4% lower-byte than fragmented H3, and all ten runs delivered the exact
  hash. Keep both distributions: the change is a strong original-baseline win,
  not proof that the remaining timing variance is solved.
- [x] Compare whole-Pack retry with selectable Transfer fragment recovery.
  Multi-fragment production selection is rejected, so fragment-selective retry
  is no longer needed on the live path. Transfer remains the whole-message
  recovery owner for the one-DATAGRAM lane.
- [~] Benchmark the explicit large-message stream lane against 1/2/3/4
  DATAGRAM fragments. Fragmenting the 1,515-byte contract-only Pack regressed
  route readiness to a 65.146-second timeout. On the corrected full-TUN path,
  the five-run one-fragment hybrid median was 6.793 seconds / 243,633 bytes,
  versus 8.413 seconds / 266,629 bytes for the two-fragment all-packet control.
  Production therefore keeps every message whole: one DATAGRAM or stream.
  Lane-accurate nested recovery reduced the first retained median to 4.060
  seconds / 208,684 bytes without removing the Transfer ACK. The post-failover
  validation median is 5.000 seconds / 209,654 bytes, so more repetitions and
  dynamic route-loss measurements remain required.
- [x] Prevent equal-priority H1/H3 congestion-controller competition inside an
  ordered Transfer sequence. The original Auto selector reshuffled routes for
  every frame; merely trying one preferred route first still spilled onto its
  sibling whenever the preferred queue filled. The retained policy makes that
  condition sender backpressure and changes carriers only after route
  withdrawal. Five fresh 256 KiB warmed Auto uploads measured a 15.190-second /
  864,267-byte / 40-drop median versus the original 27.780-second /
  1,502,911-byte / 158-drop reference. Forced H3 remains faster at a
  13.044-second / 797,068-byte / 19-drop median, while forced H1 is slower at
  24.461 seconds / 1,861,026 bytes / 40 drops. A client-wide first-carrier
  variant was rejected because connection ordering consistently selected H1.
  Keep affinity per destination-keyed sequence so H1 and H3 retain equal
  precedence and remain live in parallel. Flow-identity propagation through
  Transfer and physical-radio validation remain open.
- [~] Bound native P2P fast traffic from receiver evidence. The RTP/SRTP lane
  now advertises unreliable semantics through every connected/readiness route
  publication, so Transfer ACK flight control remains active. Carrier readers
  hand off without waiting into independent 16-message (including the
  forwarding item) and 256-KiB limits, and
  the sender reserves one slot outside its 15-message data flight for ACK and
  control traffic. The corrected five-run static campaign improved exact
  delivery from 1/5 to 5/5, median completion from 44.646 to 22.921 seconds,
  and median wire cost from 561,768 to 478,585 bytes, with no receive-queue
  drop. Physical-radio and disruption validation remain open.
- [~] Protect interactive flow admission during a saturated upload. In the
  first three-run diagnostic, flow-fair selection delivered 72/92 fixed-rate
  probes (78.3%) versus FIFO's 23/85 (27.1%), while bulk completion was about
  9.5% slower and used 2.3% more wire bytes. A later five-run A/B separated
  requested NoAck admission from the bounded ACK reserve: explicit NoAck
  admission delivered 145/164 probes (88.4%) versus 137/157 (87.3%) and used
  about 5.1% fewer wire bytes per successful probe, but its median bulk time
  regressed 8.0%. Retain the explicit, contract-safe NoAck semantics together
  with the bounded ACK reserve. Five fresh combined runs delivered every exact
  payload in 12.669--14.510 seconds and 820,723--887,071 bytes, with 13.270-
  second / 832,234-byte medians and 138/154 loaded probes (89.6%). That is 2.9%
  faster, 2.8% lower-byte, and 2.35 delivery points above reserve-selected
  NoAck. The provider recorded 116 explicit bypasses and no reserve use, while
  872 remaining ACK-flight waits and 93 timeout rewrites identify the next
  bottleneck. Hybrid single-message stream yielding was tested and removed
  after two exact runs regressed to 18.374--19.576 seconds and 901,528--907,662
  bytes. Multi-flow service-class and pre-QUIC-lane FIFO work remain open.
- [ ] Tune keepalive, resumption, migration, ECN, and DPLPMTUD.
- [ ] Evaluate alternate congestion control only if qlog shows the default
  controller remains the limiting layer.

Exit: the end-to-end commit owner and any nested in-generation recovery are
explicit for every mode, and no optimization wins only by increasing bytes or
hiding delay in a queue.

### Phase 5 — production UDP path

`vault/main/services.yml` declares UDP stream ports 443 and 8053 for connect in
its latest version. The binary and generated configuration path is implemented
in the working trees; publication, ingress rollout, and production validation
remain:

- [~] Build a minimal first-party NGINX in `warp/lb`. The source commit and
  archive bytes are pinned, the enabled-module list is retained in the build
  log, and the multi-architecture publication target requests maximum
  provenance and an SBOM. A local linux/amd64 image builds successfully.
  Registry publication, signature/verification, and a fully hermetic base and
  dependency pin are still required.
- [x] Pin the first upstream revision that supports `proxy_protocol v2` for UDP
  and prove it with deterministic load-balancer-owned source/module checks and
  a two-client, bidirectional datagram test that parses the emitted PPv2 header.
  The upstream change is
  ([NGINX commit 11d11b5](https://github.com/nginx/nginx/commit/11d11b5)) and is
  assigned to milestone 1.31.4
  ([NGINX issue 1061](https://github.com/nginx/nginx/issues/1061)). NGINX now
  officially documents `proxy_protocol v2;` as a 1.31.4 upstream feature
  ([stream proxy documentation](https://nginx.org/en/docs/stream/ngx_stream_proxy_module.html#proxy_protocol)).
  The image still builds the exact reviewed commit rather than floating on a
  release archive.
- [x] Give local and CI-style Connect tests the same pinned dependency. The
  `warp/lb` `nginx_local` target verifies the source archive SHA-256 and builds
  a minimal native binary under `warp/lb/build/nginx-local`; `connect/test.sh`
  builds that target before the first test and exports its absolute path.
- [x] Make `warpctl` emit explicit `proxy_protocol v2;`, UDP `reuseport`, a
  30-second pseudo-session timeout, and unlimited request datagrams. Preserve
  `proxy_protocol on;` for TCP because that directive still means v1. NGINX
  documents UDP `reuseport` as necessary for same-session packet affinity with
  multiple workers
  ([stream core documentation](https://nginx.org/en/docs/stream/ngx_stream_core_module.html)).
- [ ] Add measured UDP connection/rate/state-exhaustion controls before public
  rollout; the HTTP request-rate settings do not protect stream UDP state.
- [ ] Test NAT rebinding and address migration. Generic NGINX UDP sessions are
  five-tuple based, not QUIC-connection-ID aware; a changed tuple may reach a
  different connect backend. Use backend consistency where possible and retain
  bounded reconnect when migration cannot survive the load balancer.
- [ ] Canary UDP 443 independently from TCP 443 with rollback and saturation
  alarms.

After UDP Proxy Protocol v2 is proven on 443, use the same capability to restore
the two DNS-encapsulated QUIC transports (`H3Dns` and `H3DnsPump`) without making
connect bind the privileged service port:

```text
client H3Dns / H3DnsPump -> edge public IPv4 UDP/53
                             |
                  warpctl interface-scoped DNAT
                             v
                 active LB endpoint for UDP/8053
                             |
                  NGINX UDP + Proxy Protocol v2
                             v
                  connect server UDP/8053
                             |
             PPv2 decode -> DNS packet decode -> QUIC
```

- [x] Keep `PlatformTransportSettings.DnsPort` at public port 53. Change the
  connect server's `ListenDnsPort` from 53 to 8053; client configuration must
  not learn the internal port. Both defaults are pinned by tests.
- [x] Add UDP 8053 to the latest connect service's `udp_stream_ports` and add
  `8053: connect` to the latest load-balancer `udp_stream_port_services` in
  `vault/main/services.yml`. Do not restore the historical direct `53: connect`
  listener.
- [x] Make NGINX listen on UDP/8053 with `reuseport` and forward Proxy Protocol
  v2 using the explicit `proxy_protocol v2;` directive to the connect UDP/8053
  upstream. Preserve the existing server transform
  order: strip/validate PPv2 first, then run `PacketTranslationModeDecode53`,
  then hand the decoded packets to QUIC. The generated main-edge configuration
  passes `nginx -t` inside the pinned image.
- [x] Make Warp own the IPv4 public-port translation. The latest
  `vault/main/services.yml` declares `udp_forward_ports: {53: 8053}`; generated
  LB units pass the deterministic mapping to `warpctl`, which resolves the
  active LB endpoint and installs an exact
  `<interface IPv4>:53 -> <LB IPv4>:<active port for service 8053>` DNAT. The
  new rule is inserted before stale rules are removed, config withdrawal
  removes the owned alias, rules for another interface or unscoped deployment
  ports are left alone, the service target is not also emitted as a direct
  public alias, and no IPv6 or TCP port-53 rule is created. Config validation
  rejects missing targets, identity/chained mappings, and direct-port,
  allocatable-port-pool, forced-external-port, or per-interface conflicts. A
  separate edge firewall audit should still prove
  that no unrelated listener or incidentally allocated port exposes UDP/8053.
- [ ] Activation order is server listener, NGINX listener/upstream, internal
  health check, external UDP/53 DNAT, end-to-end health check, then client
  rollout. Rollback removes/withdraws the public DNAT first, lets Auto fall back,
  drains UDP pseudo-sessions, and only then removes the listener/upstream.
- [ ] Health-check an authenticated DNS-encoded QUIC exchange through public
  UDP/53 for both `H3Dns` and `H3DnsPump`; a plain DNS query is not sufficient.
  Verify that connect observes the original source address from PPv2 and that
  malformed/spoofed PPv2 is rejected.
- [ ] Add per-mode connection, handshake, packet, retry, fallback, byte,
  malformed-envelope, and rate-limit metrics. Keep them separate from ordinary
  QUIC/443 so port-53 interception or carrier behavior is visible.
- [ ] Exercise source spoofing, amplification limits, state exhaustion, DNS
  middlebox rewriting, fragments, truncation, NAT rebinding, deploy/conntrack
  draining, and a router or NGINX restart before canarying public UDP/53.

Exit: UDP Proxy Protocol v2 source identity, affinity, fallback, rate limits,
and rollback are proven in staging and canary production for 443; the DNS modes
ship only after their separate UDP/53-to-8053 gates pass.

### Phase 6 — rollout

- [ ] Gate DATAGRAM, fragmentation, lanes, scheduler, hybrid, and migration
  independently by negotiated capability and server rollout flag.
- [ ] Start with staff/canary, then low percentage, then cellular cohorts;
  compare against simultaneous H1 control cohorts.
- [ ] Auto-select from observed path behavior, not a platform label alone.
  Keep decisions sticky and probe with a strict byte budget.
- [ ] Verify iOS Network Extension memory and wakeups, Android always-on
  lifecycle, Windows service recovery, Linux service/keychain integration, and
  connect server limits.
- [ ] Verify IPv4-only settings on every app and provider throughout rollout.

Exit: release gates hold in device cohorts with no security, memory, battery,
or fallback regression.

## Required test matrix

- Nonblocking: full Pack, ACK, forward, signal, stream, TUN, and provider return
  handoffs; a blocked destination must not delay an unrelated one.
- Recovery: single loss, burst loss, ACK loss, duplicate, reorder, long gap,
  outage, and connection-generation replay.
- Fragmentation: every boundary size, last-fragment loss, duplicate fragments,
  inconsistent metadata, timeout, peer/global budget exhaustion, and fuzzing.
- Lanes: deterministic loss in lane A with progress in lane B; all routing-key
  delivery paths; legacy peer fallback; lane-count mismatch; shared budget.
- Collapse: SYN/RST, FIN, ACK/window progress, zero window, wraparound, held
  retransmit, Transfer success/failure, route change, direct, and NoAck.
- QUIC: cold/resumed/0-RTT-safe auth, UDP blocked, NAT rebind, address change,
  MTU reduction, idle wake, load-balancer backend change, and fallback.
- DNS transports: public UDP/53 to edge/server UDP/8053, both `H3Dns` and
  `H3DnsPump`, original-source PPv2 propagation, transform order, malformed
  envelopes, router DNAT activation/rollback, conntrack drain, and independent
  fallback from a blocked or rewritten port 53.
- Performance: all Phase 1 profiles and workloads with direct/H1/current-H3
  controls, at least five recorded runs.
- Memory: long DeviceLocal + DeviceRemote synthetic run with web, mail, blocked,
  bulk, loss, path churn, and post-burst recovery; assert byte/fragment/lane
  bounds and RSS recovery.
- Policy: encryption required/opportunistic/off matrices where supported, kill
  switch, CFAA/SMTP, provider eligibility, and no IPv6 advertisement or route.

All concurrency, lifecycle, generation, and saturation regressions use explicit
barriers or state transitions. Sleeps and scheduler luck are only safety
timeouts, following `CODESTYLE.md`.

## Open questions

1. What conservative DATAGRAM payload works across the actual iOS, Android,
   desktop, carrier, and NGINX paths, and how often does DPLPMTUD safely raise it?
2. Should fragments be individually recoverable Transfer units, or does whole
   Pack retry use fewer bytes once ACK overhead and bookkeeping are included?
3. Does `quic-go` DATAGRAM throughput remain sufficient after fragmentation on
   10/2 and 50/10 Mbit/s links, or is a separate large-message stream justified?
4. If the hybrid stream is retained, what exact end-to-end commit/replay rule
   survives QUIC connection loss without simultaneous periodic Transfer retry?
5. Are 4 or 8 logical data lanes enough to isolate common flows without
   multiplying ACK/control overhead?
6. Which packets qualify for interactive priority without allowing an
   application to monopolize the reserved class?
7. What queue-delay target balances radio batching/energy against interactive
   latency at 64 and 250 kbit/s uplinks?
8. Can a production NGINX UDP path preserve backend affinity through the path
   changes that matter, or must connect externalize resumption/replay state?
9. Which authentication messages, if any, are safe and valuable in 0-RTT?
10. How long should UDP-blocked evidence and successful transport choice remain
    sticky across network changes?
11. What authenticated health signal and conntrack-drain threshold should gate
    the now-Warp-owned UDP/53-to-8053 DNAT during activation and rollback?

## Findings log

| Date | Finding | Consequence |
| --- | --- | --- |
| 2026-08-17 | Current H3 is a custom QUIC transport using one reliable bidirectional stream, not HTTP/3 requests and not QUIC DATAGRAM. | Legacy H3 stacks reliable recovery with Transfer for every routed frame. Move common packet data to DATAGRAM while retaining Transfer as end-to-end commit authority across every carrier generation. |
| 2026-08-17 | Transfer Pack coalescing and the 1440-byte inner MTU can exceed a safe QUIC DATAGRAM payload. | Bounded Transfer-aware fragmentation or a measured MTU/stream alternative is required before DATAGRAM production use. |
| 2026-08-17 | `Client.run` passed the 15-second `BufferTimeout` to Pack and ACK handoffs despite the nonblocking receive invariant. Zero-timeout replacement could also wait for worker exit. | Corrected in Phase 0 with counted drops and deterministic unrelated-source progress tests. |
| 2026-08-17 | Nonblocking admission removes mechanical pump blocking, but one ordered Transfer sequence can still hold unrelated inner flows behind a missing sequence number. | Measure and prototype a small bounded set of logical lanes; do not use transport stream IDs as keys. |
| 2026-08-17 | Existing multi-TCP collapse prevention already suppresses duplicate inner retransmits only when Transfer ACK recovery owns the path. | Preserve it and bind its hold/release to explicit recovery mode and Transfer item state. |
| 2026-08-17 | Historical PERFVAR data shows grouping helps, large queues can fail, and current H3 can be far slower than H1 even on a clean mobile surrogate. | Treat QUIC as one component; prioritize recovery ownership, scheduling, measurement, and bounded queues. |
| 2026-08-17 | The NGINX upstream UDP Proxy Protocol v2 change exists, while the required release/deployment path is not yet established. | Use a reproducible first-party `warp/lb` build and canary; do not block the local prototype on production LB work. |
| 2026-08-17 | The server already has DNS-encoded QUIC listeners and applies PPv2 decoding before DNS packet translation, but its default still binds port 53; the latest load-balancer config exposes only UDP 443. | After PPv2 works on 443, keep clients on public UDP/53, DNAT at IPv4 ingress to edge UDP/8053, proxy with PPv2 to connect UDP/8053, and gate both DNS modes independently. |
| 2026-08-17 | Zero-timeout admission into a zero-capacity Pack/ACK rendezvous can repeatedly miss both workers under a bidirectional burst; the old exact-delivery stress test stalled even though neither shared pump blocked. | Production and broad end-to-end tests need a positive bounded admission capacity. Keep zero/full behavior in deterministic saturation tests, and use drops plus sender recovery rather than zero-capacity channels as the overload mechanism. |
| 2026-08-17 | The existing `mobile-poor` profile starts at 10/2 Mbit/s and does not cover the 64–250 kbit/s uplink corner. | Added three explicit `cell-edge-*` device profiles with one-packet startup burst credit and clean provider access; they are engineering stress points pending field-trace calibration. |
| 2026-08-17 | Several exact-delivery and integration fixtures violated the receive-callback rule by using zero-capacity Transfer queues, blocking callback collectors, direct reply sends, or goroutine-per-packet echoes. This made loss/retry tests look like production stalls and retained workers across the package run. | Converted the affected fixtures and `connectctl sink` to positive bounded queues, zero-wait handoffs with explicit drops, fixed sender workers, owned callback snapshots, and joined teardown. Keep overload behavior in focused saturation tests. |
| 2026-08-17 | Repeated complete non-short Connect runs now finish in 523–562 seconds after the callback/fixture corrections; the earlier ten-minute package timeout was not reproduced. | Treat the prior timeout row below as resolved for this working tree, while continuing the non-Client callback audit and dedicated long-duration memory measurement. |
| 2026-08-17 | PERFVAR's one-hop P2P fixture placed the device on the right endpoint but applied the application-oriented profile without translating directions. Static P2P upload therefore used the configured download link and vice versa, and direct calibration mirrored that inversion. | Corrected construction and calibration so forward/reverse always mean device upload/download; schedule version 2/schema 4 prevent mixing old directional records. |
| 2026-08-17 | Existing live-link primitives were confined to standalone correctness tests and updated every exchange access link, so they could not produce a fair device-only campaign trace. | Added hash-visible, measured-start dynamic profiles with targeted device-link updates, direct-P2P orientation translation, acknowledged event offsets/link names, payload-duration bounds, and incomplete-trace failure. Provider access remains clean. |
| 2026-08-17 | `Client.ReceiveStats()` exposed nonblocking Pack/byte and ACK admission loss, but PERFVAR did not snapshot it, so a low-bar run could not correlate application behavior with receive-handoff saturation. | Added interval-scoped device, provider, and stream-intermediary receive-handoff observations. The fixed-point baseline now retries if these counters or Client identities change across its reset pass. |
| 2026-08-17 | Locally generated ICE candidates were sent with ordinary sender backpressure directly inside Pion's candidate callback. A full Transfer queue could therefore park Pion event delivery even though inbound signal replies already used timeout zero. | Candidate-callback sends now carry `signalSendNonBlocking`; deterministic saturation verifies callback return and zero blocking sends. Sender-owned initial offer/control work retains sender backpressure. |
| 2026-08-17 | The device RPC websocket reader blocked first on a shared receive-byte budget and then on either logical stream's full queue. One stalled reverse-RPC consumer could therefore head-of-line block the forward RPC stream indefinitely. | Receive admission is now zero-wait. Because a reliable RPC byte fragment cannot be skipped, saturation closes and drains the complete mux generation so normal DeviceRemote reconnect can recover without silent stream corruption. |
| 2026-08-17 | H1/H3 carrier readers waited up to the read timeout on a full route, both P2P readers propagated route pressure, and connect-server socket/exchange readers also waited. | All receiver-owned route offers are now zero-wait and counted. Data refusal feeds Transfer recovery; reliable H1 control refusal terminates that carrier generation. |
| 2026-08-17 | Resident Client callbacks still slept in the control limiter or performed locks, active-contract storage checks, forward construction, and optional sender waits inline. | Callback ingress is now bounded and zero-wait. One ordered control worker and destination-stable forward shards own all slow work; control overflow forces replay via reconnect. |
| 2026-08-17 | NGINX's new backend directive keeps version 1 as the meaning of `proxy_protocol on;`; UDP source metadata requires explicit `proxy_protocol v2;`. The current warp template emits only `on`. | The pinned NGINX build and warpctl config must land together. Prove UDP/443 first, then reuse the same explicit v2 path for edge/server UDP/8053 behind public UDP/53 DNAT. |
| 2026-08-17 | The exact upstream commit `11d11b5f0d3d8ace5215e1a77918e9dc219ce7db` preserves two original client addresses and bidirectional payloads across 64 alternating 1400-byte UDP datagrams through two NGINX workers. The same test carries 1400-byte requests and replies through PPv2-before-DNS decoding for both `H3Dns` and `H3DnsPump` envelope modes. | UDP upstream PPv2 and the server transform order are now proven locally. Keep the test gated by the exact-capability binary until an official release containing the change is the production pin. |
| 2026-08-17 | The former 15-minute generic stream timeout could outlive the server's 45-second PP source mapping and make a live NGINX UDP pseudo-session lose its reply route. | UDP stream servers now use a 30-second timeout, while TCP retains 15 minutes; a server regression test pins the required timeout ordering. |
| 2026-08-17 | Client UDP/53, server UDP/8053, latest vault service allocation, generated NGINX PPv2 forwarding, and the pinned image config are wired and validated. The repository has only a prose EdgeRouter setup note, not versioned public 53-to-8053 ingress automation. | Do not enable the DNS modes yet. The IPv4 DNAT, direct-8053 firewall policy, authenticated health checks, metrics, abuse controls, canary, and rollback remain deployment gates. |
| 2026-08-18 | Warp now carries a validated, versioned `udp_forward_ports` mapping from public 53 to LB service 8053 and propagates it only to LB units. `warpctl` reconciles the exact interface-address DNAT add-before-delete, removes withdrawn/stale aliases, preserves other interfaces and unscoped deployment rules, and deliberately emits neither IPv6/53 nor a direct public target alias. | The ad-hoc ingress-router configuration gap is closed in code. Production activation still waits for authenticated public-path health, UDP abuse/state controls, conntrack-aware drain, edge firewall verification, a canary, and published/signed LB artifacts. |
| 2026-08-17 | Validating every generated main-edge config exposed two legacy blocks without capacity sizing. The generator consequently omitted NGINX's mandatory `events` section, making those historical configs invalid. | Legacy unsized blocks now emit the NGINX-default 512 worker connections. A regression test pins the fallback and all 13 generated main-edge configs pass the pinned image's `nginx -t`. |
| 2026-08-17 | The NGINX capability proof originally lived only with the connect server, so `warp/lb` could change its source or modules without a package-owned regression. | `warp/lb` now pins the capable commit, archive digest, checksum verification, stream module, image label, and source-build policy; its binary test independently parses PPv2 source metadata and verifies bidirectional UDP for two clients. |
| 2026-08-17 | QUIC transport-parameter negotiation alone is not enough to version the application fragment envelope, while changing an echoed `Auth` response unconditionally would break old clients. | Added separate authenticated offer/accepted fields. Old servers echo an acceptance of zero, old clients offer zero, and only matching new/new peers with bilateral RFC 9221 support move routed frames off the stream. |
| 2026-08-17 | `quic-go` copies `SendDatagram` input, blocks a sender only after its bounded 32-datagram queue fills, copies receive payloads into its bounded 128-datagram queue, and returns the current maximum in `DatagramTooLargeError`. | Reuse one bounded send scratch buffer (now a 1,360-byte target), permit sender backpressure, pool only complete reassembled Transfer frames, and retry one path-MTU shrink under a new carrier id. Transfer—not QUIC—recovers any partial or silently discarded message. |
| 2026-08-17 | A first bounded reassembler draft returned pooled buffers and adjusted its shared byte budget while holding the local state mutex. | Split state mutation from external allocation/release. Pool and shared-budget operations now occur outside `stateLock`; expired and corrupt ids enter a bounded retirement window so late fragments cannot resurrect their storage lifetime. |
| 2026-08-17 | A negotiated DATAGRAM generation no longer needs the legacy H3 writer's 64 KiB stream batch allocation. | Allocate that batch lazily only when the hybrid selects stream. The candidate retains a 1,360-byte sender scratch plus explicitly bounded incomplete-message metadata and payload bytes. |
| 2026-08-17 | A database-free full-Transfer A/B on `cell-edge-1m-down-250k-up` completed legacy H3 in 2.16--2.25 seconds but H3 DATAGRAM in 13.54--13.57 seconds over three repeated paired runs. First-message latency remained about 0.25 seconds and DATAGRAM used fewer wire bytes, but its Pack rewrites rose from 7--11 to 26--27. | H3 DATAGRAM is a release blocker, not a production optimization, until Transfer recovery improves; lower byte cost does not compensate for a roughly 6.1x completion regression. |
| 2026-08-17 | Keeping every Transfer frame below the 1,150-byte target still produced a 13.96-second DATAGRAM completion versus 2.22 seconds for legacy H3, with zero reassembly timeouts. Selective ACKs acknowledge later Packs, but there is no selective-gap fast retransmit; a missing earlier Pack follows the cold 2-second retry and exponential 4/8-second backoff, while a lost cumulative ACK can park selectively acknowledged state behind a coarse probe. | Fragmentation is not the primary tail cause. Prioritize a once-per-gap, reordering-safe, paced recovery signal and adaptive cumulative-ACK probe, then rerun the same A/B before lanes or rollout work. |
| 2026-08-17 | Guarded selective-gap recovery materially improves the DATAGRAM tail, but an aggressive automatic two-probe train reached 2.604 seconds only by issuing 28 gap plus 32 tail writes. Removing that retry train and retaining bounded receiver-evidenced recovery yields about 4.3--5.8 seconds with the production 32-frame carrier route, still slower than legacy. | Keep the reordering-safe scoreboard and telemetry, reject the retry-storm candidate, and solve admission/flight size before further shortening timers. Recovery that wins only by injecting duplicates does not meet the bytes or congestion release gates. |
| 2026-08-17 | `quic-go` v0.61.0 starts with a 32-packet congestion window and has a 32-entry DATAGRAM send queue. The `cell-edge-1m-down-250k-up` simulator queue holds about 13 1,280-byte packets (roughly 500 ms), so the initial DATAGRAM flight can overrun it before Transfer receives delivery evidence; measured runs show about 25 selective-gap recoveries and roughly 40 carrier queue drops. The legacy stream hides the same burst behind QUIC retransmission. | Recovery tuning alone cannot make DATAGRAM competitive. Add carrier-specific, byte-bounded adaptive Transfer flight control that stays below the initial low-bar queue, opens on ACK progress, reduces on gap evidence, never limits reliable carriers, and does not block receiver callbacks. `quic-go` exposes no public initial-congestion-window setting, so avoid a dependency fork as the first fix. |
| 2026-08-18 | `AllowDirect` previously converted tunneled TCP to Transfer `NoAck` on the assumption that its selected carrier was reliable. That guarantee ends when the carrier disconnects or a route is replaced, leaving no end-to-end commit for an accepted TCP packet. | TCP now always requires a Transfer ACK. The invariant is enforced both in route policy and at the final singleton/group packet-to-Transfer boundaries, so an explicit lower-level `NoAck` hint cannot bypass it. Direct TCP is consequently included in collapse prevention. |
| 2026-08-18 | The only stream traffic in successful cold 1,100-MTU full-TUN runs was an exact 1,515-byte contract-only Pack repeated 11 and 30 times; it contains no tunneled application frame. Moving that control item to two DATAGRAM fragments caused route readiness to time out after 65.146 seconds on the same seeded one-bar profile. | Keep application-bearing frames that fit on the one-DATAGRAM lane and keep contract-only/oversized frames on the stream lane. Do not infer that fragmenting a small control item is cheaper merely because Transfer can retry it. |
| 2026-08-18 | After TCP became ACK-required, the dedicated stall watchdog repeatedly held a slow-route verdict because no sibling proved the uplink, but the resize loop independently reread raw `sendStalled` state and removed the exit at 34.747 seconds. That bypassed the watchdog's busy-probe, uplink, shared-fate, and quarantine gates. | The watchdog is now the sole send-stall conviction owner. Resize reaps a canceled client but cannot manufacture a second ungated conviction; a source-anchor regression prevents that call path from returning. |
| 2026-08-18 | In negotiated hybrid mode, both endpoints retained an application read deadline on the otherwise idle reliable stream. QUIC DATAGRAM activity cannot satisfy that deadline, and the same writer that emits stream pings can block behind quic-go's bounded 32-DATAGRAM send queue. | Clear the post-auth stream deadline only for negotiated hybrid connections and enable QUIC-level keepalive on client and server. QUIC connection idle detection still closes a dead peer, while liveness no longer depends on the possibly blocked application writer. |
| 2026-08-18 | The remaining slow completion had 336 device-side successful `SendDatagram` queue admissions but only 139 provider-side complete receives, while reporting no DATAGRAM integrity/reassembly errors and no Client Pack/ACK handoff drops. The physical profile counted 19 loss and 15 queue drops on the constrained uplink, which does not by itself explain 176 Transfer timeout rewrites. | The current H3 `SentMessageCount` is queue admission, not proof that quic-go placed the DATAGRAM frame on wire. Add packet-emission and quic-go receive-queue visibility before changing Transfer recovery again; the residual tail is below the Transfer admission boundary. |
| 2026-08-18 | Exact qlog and raw-TUN fingerprints showed that every successfully received QUIC packet matched a client send, while decrypt failures matched neither client qlog sends nor complete client UDP payloads. The rejected packets were consistently 1,400/1,256-byte fragmentation artifacts; the same failures persisted when the first QUIC key update was delayed from 100 to 100,000 packets. | The earlier key-update correlation was false. Start QUIC at its legal 1,200-byte floor and keep DPLPMTUD enabled; do not raise the lower bound above a 1,280-byte cellular path's post-IP/UDP capacity. The dependency-only key experiments are fully reverted. |
| 2026-08-18 | PERFVAR configured both the device/provider access TUN and the edge/server mirror TUN with `networkProfile.InnerMtu` (1,200 bytes on the cell-edge profile) while separately enforcing a 1,280-byte outer link. This fragmented even a legal 1,200-byte QUIC UDP payload before the outer-MTU gate and hid the original packet from link counters. Correcting only the access side left device-side decrypt failures during route readiness; interval telemetry exposed the symmetric edge-side error. | Every physical carrier TUN now uses the smallest directional outer MTU; only the application TUN uses the nested VPN MTU. A deterministic helper test pins the distinction. This is a benchmark-fidelity correction, so historical, one-sided, and fully corrected results remain labeled separately. |
| 2026-08-18 | One worst-case 1,100-byte tunnel packet serializes to 1,288 encrypted Transfer bytes and 1,316 bytes with the H3 envelope. It cannot fit one QUIC DATAGRAM on a 1,280-byte path. The exact one-DATAGRAM inner-MTU ceilings are 944 bytes for one packet and 934 bytes for two coalesced packets totaling the MTU. | Keep the product MTU at 1,100. A global 900-byte MTU improved the all-packet H3 candidate but regressed H1 and legacy H3, so global MTU reduction is rejected. Select the carrier lane per complete Transfer message instead. |
| 2026-08-18 | Five refreshed cold runs with TCP always Transfer-ACKed measured fragmented H3 at 8.413 seconds / 266,629 bytes median and one-DATAGRAM hybrid H3 at 6.793 seconds / 243,633 bytes. Every hybrid run delivered the exact hash with both lanes active, one fragment per DATAGRAM message, and zero integrity or reassembly failure. | Set the production H3 fragment limit to one. Messages that cannot fit one current-path DATAGRAM use stream; retain multi-fragment support only as an explicit compatibility/benchmark setting. This improves the H3 candidate by 19.3% in completion time and 8.6% in wire bytes without changing Transfer ACK semantics. |
| 2026-08-18 | The same refreshed control set measured H1 at 5.180 seconds / 307,814 bytes median and legacy H3 stream at 3.995 seconds / 339,130 bytes. The hybrid saves 20.9% and 28.2% wire bytes respectively, but is 31.2% and 70.0% slower. | The hybrid wins over fragmented H3, not over every carrier. Keep H1 and H3 healthy in parallel in Auto and do not claim broad low-bar superiority until mixed Auto, multi-flow, direction, profile, and dynamic-path campaigns pass. |
| 2026-08-18 | Expanding the unreliable Transfer flight to 12 KiB after one acknowledged cold flight improved typical completion only by producing 9--27 queue drops and extra inner retransmits. At 1,100 MTU it was especially unsafe because one logical message could consume two DATAGRAMs. | Rejected and removed. Flight growth remains receiver-evidenced and loss-responsive. Any future optimization must use the actual selected carrier lane; it cannot infer that every write on a negotiated hybrid H3 connection is unreliable. |
| 2026-08-18 | Publishing negotiated H3 as route-wide `Unreliable` made Transfer count reliable hybrid-stream writes, and even writes actually accepted by an equal-priority H1 sibling, against the DATAGRAM flight and two-second retry policy. A first exact-write classifier improved time but still produced a 5.924-second / 299,907-byte median because messages admitted before quic-go's path-size feedback were misclassified and stream writes formed a duplicate Transfer retry train. | Carry the exact selected route's carrier disposition back to `SendSequence`. Probe quic-go's synchronous current DATAGRAM limit before route publication, update it atomically after path shrink, and count only writes that actually use DATAGRAM as unreliable flight. Reject the intermediate byte-regressing candidate. |
| 2026-08-18 | A hybrid stream write is still end-to-end Transfer-ACKed, but retrying it every two seconds duplicates QUIC's ordered reliable recovery while the constrained uplink drains. Deferring the Transfer retry to eight seconds produced a first five-run 4.060-second / 208,684-byte median with exactly 62 stream writes in every run, versus 6.793 seconds / 243,633 bytes before lane-accurate recovery. | Keep TCP `ack=true`; change only nested recovery timing. QUIC owns in-generation stream retry, while Transfer remains the commit owner and eventual recovery layer. The exact selected lane, never negotiated H3 capability alone, chooses the flight and retry policy. |
| 2026-08-18 | An eight-second Transfer interval is unsafe if the QUIC connection that accepted a hybrid stream write disappears: that retired generation cannot deliver its buffered bytes. Conversely, merely adding another equal-priority carrier does not prove loss on the original route. | Track the exact accepting route per pending hybrid-stream item. On route-generation change, immediately reschedule only items whose accepting route was withdrawn; do not retry when that route remains active. Focused normal/race tests pin both cases. The post-change five-run median was 5.000 seconds / 209,654 bytes, retaining the byte improvement and a large original-baseline win while exposing unresolved timing variance. |
| 2026-08-18 | Schema-6 outage telemetry showed that H3 eliminated the H1 control's 99 device-side timeout rewrites, but the H3 provider still entered the small-message flight barrier 137 times for 18.212 seconds. H3 completed in 43.298 seconds versus H1's 46.462 seconds, at 6,097,889 versus 6,023,722 carrier bytes. | Keep TCP Transfer ACKs and the current H3 hybrid. Treat provider flight recovery as an optimization opportunity, not proof that the safety bound is wrong; every candidate must pass both the 13-packet static queue and the live-outage trace. |
| 2026-08-18 | Raising the cold message flight from 8 to 12 improved the static five-run median to 3.933 seconds / 209,524 bytes, but two recorded outage traces took 45.118 and 44.887 seconds. Raising the loss floor from 4 to 8 then produced 257,383 bytes, 74 stream writes, and 30 inner-TCP retransmits in one static run. | Rejected and removed. Keep the 8-message cold limit and 4-message loss floor. A low byte count does not authorize a larger packet-count burst, and a transient-outage optimization may not regress the static one-bar queue. |
| 2026-08-18 | Draining up to eight ready small Packs into one DATAGRAM produced a 3.345-second static median, but one H3 run reached 6.151 seconds, another incurred 29 inner-TCP retransmits, and H1 wire bytes rose 2.9%. A four-frame bound repeated the correlated-loss failure with 25--29 retransmits. Doubling additive message recovery raised outage gaps from 3 to 18 and completion to 45.772 seconds. | Rejected and removed. Do not collapse multiple inner TCP ACK packets into one lossy fate or outgrow receiver evidence faster than the shaped uplink drains. Retain the original two-Pack opportunistic coalescer and +1 additive message recovery. |
| 2026-08-18 | In the first exact warmed rate-collapse pair, hybrid H3 completed in 44.832 seconds / 6,063,883 carrier bytes versus H1's 53.775 seconds / 6,805,198 bytes. H3 reduced forward queue drops from 364 to 201 and device timeout rewrites from 390 to 2. The cold pair was effectively tied at 49.373 versus 49.678 seconds. | Hybrid H3 has its first material current-tree dynamic win: 16.6% faster and 10.9% lower-byte on the warmed trace. Keep it diagnostic until five paired traces reproduce it, then continue through live-MTU, mixed-Auto, multi-flow, direction, and physical-radio gates. |
| 2026-08-18 | Two exact live-MTU pairs reduced the outer path from 1,400 to 1,280 bytes and restored it during active 2 MiB uploads. Cold H3/H1 completed in 38.136 / 48.555 seconds and warmed H3/H1 in 37.404 / 49.385 seconds. H3's largest carrier packet was 1,228 bytes with zero MTU drops; H1 submitted 1,384-byte packets and recorded 10--16 MTU drops. | The current hybrid has no evidenced live-MTU regression. H3 is 21.5--24.3% faster, 2.9--7.5% lower-byte, and has about half the queue drops in these pairs. Keep the one-complete-DATAGRAM rule and 1,200-byte QUIC floor; advance to mixed-Auto without changing Transfer ACK safety. |
| 2026-08-18 | A fresh five-run 256-KiB warmed-upload matrix measured all correctness-valid samples at Auto 14.983 s / 876,468 B, H3 15.268 s / 818,234 B, and H1 30.285 s / 1,976,566 B median. Auto remained H3-affine for payload while keeping H1 healthy; two Auto, three H3, and one H1 records missed only the conservative calibration-headroom rule. | H3 is 49.6% faster and 58.6% lower-byte than H1; Auto is 50.5% faster and 55.7% lower-byte than H1. Auto is within 1.9% of H3 time but uses 7.1% more bytes. Keep both equal-priority carriers live, do not stripe one ordered sequence, and report calibration-invalid counts separately from payload correctness. |
| 2026-08-18 | Native fast P2P was registered as if it were a reliable carrier, and the endpoint-readiness rematch replaced any initial carrier properties with their zero value. The initial five-run matrix consequently allowed 692--820 fast sends while its old four-entry receive route dropped small bursts; only 1/5 payloads were exact. | Publish the RTP/SRTP fast lane as unreliable on both connected and readiness updates. Keep Transfer ACKs, but activate the receiver-evidenced unreliable flight for this carrier instead of assuming transport-up proves delivery. |
| 2026-08-18 | A count-only P2P flight cap exposed a queue-shape conflict: four slots avoid large-frame memory growth but can reject tiny ACK/control bursts; reserving still more slots removed drops only by reducing throughput. | Separate receive count from bytes. Sixteen messages with a hard 256-KiB aggregate ceiling retain the former four-by-64-KiB worst-case payload memory, while a 15-message Transfer data flight leaves one untracked ACK/control slot. The retained five-run result is 5/5 exact, zero queue drops, 22.921 s / 478,585 B median. |
| 2026-08-18 | Loaded-latency instrumentation showed 20--24 H3 flow-reserve selections per run but zero reserve uses. Those selected Packs were requested NoAck logical groups: the reserve was acting as an implicit scheduler permission even though the messages never entered ACK flight. | Make NoAck admission explicit and contract-safe for the exact next serialized chunk. Keep the single bounded reserve for genuinely ACK-required new flows. Schema 9 records NoAck bypass, reserve selection/use, and both endpoints' H3 DATAGRAM/stream lanes so the two mechanisms cannot be conflated again. |
| 2026-08-18 | quic-go's packet packer takes one submitted DATAGRAM before retransmitted or new STREAM data, but URnetwork's H3 writer consumes both lanes from one FIFO and may block while handing a ready stream batch to QUIC. Disabling hybrid stream batching did not expose a latency win: two runs were 32--39% slower than the retained median and 8--9% higher-byte. | Keep stream batching. If lane submission is split, preserve one byte-bounded ownership budget and prove DATAGRAM progress while a stream handoff is blocked; do not approximate that architecture by shrinking batches. |

## Results log

| Date | Revision / candidate | Test or artifact | Result |
| --- | --- | --- | --- |
| 2026-08-17 | Working tree, Phase 0 receive admission | `go test ./... -run 'Test(ReceiveSequenceReplacementDropsWithoutWaiting\|ReceiveSequenceClosingGenerationDropsWithoutWaiting\|ClientReceivePackHandoffDoesNotBlockUnrelatedSource\|ClientReceiveAckHandoffDoesNotBlockUnrelatedSource)$' -count=1` | Pass. Full Pack and ACK queues plus replacement and closing generations do not block unrelated receive progress. |
| 2026-08-17 | Working tree, Phase 0 receive admission | Same four tests with `go test -race . -run ... -count=1` | Pass under the race detector. |
| 2026-08-17 | Historical baseline | `server/connect/perfvar/MEASUREMENTS.md` | Evidence summarized above; current authoritative mobile campaign remains incomplete. |
| 2026-08-17 | Earlier working tree (historical failure) | `go test ./... -count=1` | Failed: `TestMultiClientUdp4` mixed lifecycle-policy ICMP teardown with an exact-delivery routing fixture, then the package timed out. The exact-delivery repair and final non-short suite row below resolve this result. |
| 2026-08-17 | Working tree, exact-delivery fixture repair | `go test . -run '^Test(Client\|MultiClient)(Udp4\|Tcp4\|Udp6\|Tcp6)$' -count=1` | Pass: all eight variants. The fixture opts out of flow lifecycle policy, uses fixed joined echo workers instead of one blocking-send goroutine per echo, and isolates routing assertions from admission saturation. |
| 2026-08-17 | Working tree, Phase 0 receive stats | Four receive-admission regressions plus `go test -race . -run 'Test(ClientReceivePackHandoffDoesNotBlockUnrelatedSource\|ClientReceiveAckHandoffDoesNotBlockUnrelatedSource)$' -count=1` | Pass. Public snapshots report exact Pack/byte and ACK handoff loss without receive-path locking. |
| 2026-08-17 | Server working tree, static cell-edge profiles | `go test ./connect/perfvar -run 'Test(CellEdgeProfilesResolveExactDeviceAccessConditions\|SimulatorProfilesValidateAndHash\|PerfvarCellEdgeScenarioDefaults\|PerfvarDefaultPayloadsUseLongBulkTransfers)$' -count=1` and the same selection with `-race` | Pass. Direction, rate, RTT, jitter, loss, queue, MTU, provider-access, payload, and UDP pacing defaults are pinned. No five-run performance result yet. |
| 2026-08-17 | Server working tree, full PERFVAR package | `go test ./connect/perfvar -count=1 -timeout=10m` | Environment-blocked: integration/correctness fixtures require `WARP_ENV` and vault `pg.yml`, neither available in this workspace. The pure profile/scenario selection above passes normally and under race. |
| 2026-08-17 | Working tree, adjacent callback fixtures | `go test -race . -run '^(TestPackLaneCodecLegacyAbsent\|TestSendReceiveParallelLanes\|TestSendMultiWithTimeoutDeliversOneBatchAndOneAck\|TestUpgradeMuxMultiClientIntegration\|TestUpgradeMuxDefaultDnsThroughTunnel)$' -count=1` | Pass. Lane delivery, batch receive, provider echo, client/TUN collection, ownership, and teardown remain correct with bounded zero-wait callback handoffs. |
| 2026-08-17 | Working tree, pool-balance callback worker | `go test -race . -run '^(TestMultiClientLifecyclePoolBalance\|TestRemoteUserNatClientRawSendPoolBalance)$' -count=1` | Pass. The provider reply leaves the shared callback before its blocking send and returns pooled bytes on admission failure, queue overflow, and cancellation. |
| 2026-08-17 | Working tree, `connectctl sink` | `go test ./connectctl -count=1` and focused race tests | Pass. A full printer queue drops immediately, counts loss atomically, and reports it outside the receive callback. |
| 2026-08-17 | Full working tree, short suite | `go test ./... -short -count=1 -timeout=10m` | Pass. Main Connect package: 212.178 s; all tested subpackages green. |
| 2026-08-17 | Full working tree, non-short suite | `go test ./... -count=1 -timeout=20m` | Pass. Main Connect package: 523.092 s; `blocker`, `connectctl`, and `extender` green; no package timeout. |
| 2026-08-17 | Working tree, encryption callback audit | Five contract/encryption tests and five contract-free/gate tests, each selected normally and with `-race` | Pass. Message-only callbacks count inline; content collectors use bounded zero-wait handoffs; fixtures use positive bounded Transfer capacity. The five contract/encryption tests complete in about 15–17 s. |
| 2026-08-17 | Working tree, control-sync callback audit | `go test . -run '^TestControlSync$' -count=1` and the same test with `-race` | Pass in 75.137 s normally and 78.095 s under race. The 4,000-message collector snapshots indexes into an exact bounded queue and never waits in the callback. |
| 2026-08-17 | Working tree, contention benchmark callback audit | `go test . -run '^$' -bench '^(BenchmarkMultiClientEgressParallel\|BenchmarkMultiClientBidirectional)$' -benchtime=100x -count=1` | Pass. Provider echoes use four bounded sender workers; overflow/cancellation returns pooled packets, callbacks are unsubscribed before drain, and benchmark multi-clients close. |
| 2026-08-17 | Full working tree after adjacent callback audit | `go test ./... -count=1 -timeout=20m` | Pass. Main Connect package: 561.737 s; all tested subpackages green. The long integration tail varies, while focused corrected groups remain fast and race-clean. |
| 2026-08-17 | Server working tree, dynamic cell-edge definitions and scope | `go test ./connect/perfvar -run 'Test(DynamicCellEdgeProfileSchedulesResolveExactEvents\|PerfvarDynamicProfileScenarioDefaultsAndBounds\|ProfileScheduleRunnerCompletionAndEarlyFinish\|ApplyFullTunProfileEventScopesDeviceAndP2pDirections\|FullTunEffectiveRateAndAggregateTimeout\|SimulatorProfilesValidateAndHash)$' -count=1` and the same selection with `-race` | Pass. Exact rate/outage/MTU timing, profile hashing, minimum payloads, device-only exchange scope, P2P directionality, and runner cancellation are pinned. |
| 2026-08-17 | Server working tree, route-neutral dynamic replay | `go test ./connect/perfvar -run '^TestMeasurePerfvarUnderlayReplaysLiveProfileSchedule$' -count=1` and the same selection with `-race` | Pass (4.080 s normal; 7.031 s race on the first isolated runs). One exact TCP stream remained active through both scheduled changes; both directional links recorded two updates and the result retained acknowledged event scope. |
| 2026-08-17 | Server working tree, receive-handoff carrier telemetry | `go test ./connect/perfvar -run 'Test(SubtractPerfvarClientReceiveRequiresStableGeneration\|ObservePerfvarCarrierIncludesReceiveHandoffIntervals\|PerfvarCarrierBaselinePassStableCoversEveryRouteCarrier\|PerfvarCarrierGenerationStableRejectsPostBaselineSubmission\|PerfvarCarrierGenerationStableIgnoresJoinedBridgeBatch)$' -count=1` and the same selection with `-race` | Pass. Device/provider/intermediary interval subtraction is exact only for a stable Client identity; generation changes are explicit, and receive-counter activity invalidates a crossing baseline pass. |
| 2026-08-17 | Working tree, production callback policy | `go test . -run 'Test(ProductionClientReceiveCallbacksAreAudited\|SharedClientReceivePumpHandoffsUseZeroTimeout\|PionIceCandidateCallbackSendDoesNotBlock\|ReceivePathSignalSendsDoNotBlock\|TransferReceiveCallbackDispatchIsInline\|TransferForwardCallbackDispatchIsInline\|TransferCancelDoesNotJoinInFlightReceiveCallback)$' -count=1` and the same selection with `-race` | Pass. Every production Connect subscriber is inventoried, direct blocking constructs are rejected structurally, Pack/ACK timeout zero is pinned, and Pion callback sends drop rather than wait. |
| 2026-08-17 | Working tree, callback ownership boundaries | Provider NAT/TCP, stream lifecycle, signal-shard, and P2P probe saturation selections plus SDK `TestDeviceLocalIoLoopEndToEnd` and migration callback tests, normally and with `-race` | Pass. Datagrams and shared callbacks do not wait; the dedicated provider TCP socket reader and final device-TUN write retain only their documented lossless synchronous scope. |
| 2026-08-17 | SDK working tree, RPC and subscriber policy | `go test . -run 'Test(DeviceRpcReceiveByteBudgetRefusesWithoutWaiting\|DeviceRpcMuxReceiveQueueSaturationClosesWithoutBlocking\|SdkClientReceiveRegistrationsAreAudited\|DeviceLocalProviderMigrateReceiveCallbackDoesNotWait)$' -count=1` and the same selection with `-race` | Pass. Full RPC receive budgets/queues terminate instead of parking the shared reader, and SDK Client subscribers require explicit audit. |
| 2026-08-17 | Full working trees after production callback-policy enforcement | Connect and SDK: `go test ./... -short -count=1 -timeout=10m` | Pass. Main Connect package: 209.638 s; all tested Connect subpackages green. SDK: 92.467 s. |
| 2026-08-17 | Pinned NGINX UDP PPv2 candidate | `NGINX_UDP_PROXY_V2_BINARY=/tmp/urnetwork-nginx-udp-v2-full/sbin/nginx go test ./connect -run 'Test(DefaultWarpPpTimeoutOutlivesNginxUdpSession\|DefaultConnectHandlerDnsListenerUsesInternalPort\|PpNginxUdpV2)$' -count=1` in `server` | Pass. Original IPv4 address/port, payload integrity, two-client separation, replies through the NGINX UDP pseudo-session, and bidirectional `H3Dns`/`H3DnsPump` envelope transforms are verified. |
| 2026-08-17 | Client public-port invariant | `go test . -run 'Test(PlatformQuicConfigEnablesPathMtuDiscovery\|PlatformDnsTransportUsesPublicPort)$' -count=1` in `connect` | Pass. DNS-encoded QUIC clients remain on public UDP/53 while the server default is independently pinned to UDP/8053. |
| 2026-08-17 | First-party load-balancer candidate | Local linux/amd64 `warp/lb` build from NGINX commit `11d11b5f0d3d8ace5215e1a77918e9dc219ce7db`, source archive SHA-256 `dbc96585a7ddc6f3c3a8faae9487ecdf5ad4e1e2eeb77a8b26e69d935434c9de`; all generated `main` configs checked with the image's `nginx -t` | Pass for all 13 configs. The current edge configs contain generated UDP/443 and UDP/8053 PPv2 servers. This is a local artifact, not a published or signed production image. |
| 2026-08-17 | UDP PPv2 and 53/8053 regression boundaries | The focused Connect, server, and warpctl selections above repeated with `go test -race` and the pinned NGINX binary | Pass. All three packages are race-clean at the changed boundaries. |
| 2026-08-18 | Warp-owned UDP/53 forward-port lifecycle | `go test ./services -run 'ForwardPort' -count=1`; focused warpctl forward/redirect/systemd tests repeated 20 times; the services and warpctl boundaries repeated three times under `go test -race`; production `main` services loaded through `warpctl ls services main` | Pass. Schema validation, deterministic unit propagation, exact IPv4 interface scoping, active LB-port resolution, IPv6/TCP exclusion, add-before-delete replacement, stale/direct-target cleanup, config-withdrawal cleanup, and cross-interface/unscoped rule isolation are pinned. |
| 2026-08-17 | Broader post-change suites | `go test ./... -short -count=1 -timeout=10m` in Connect and warp | Pass. Connect completed in 207.980 s; all tested warp packages completed successfully. |
| 2026-08-17 | Server broader short suite | `go test ./connect -short -count=1 -timeout=10m` | Environment-blocked after 602.234 s: `TestConnectAuto` repeatedly requires `WARP_ENV` and vault `pg.yml`, neither available in this workspace, then the package timeout fires. The focused changed paths pass normally and under `-race`; do not treat this run as a product-path pass. |
| 2026-08-17 | Load-balancer-owned NGINX PPv2 regressions | `NGINX_UDP_PROXY_V2_BINARY=/tmp/urnetwork-nginx-udp-v2-full/sbin/nginx go test . -run 'Test(DockerfilePinsNginxUdpProxyProtocolV2Support\|NginxUdpProxyProtocolV2EndToEnd)$' -count=1` in `warp/lb`, then the same selection with `-race` | Pass. Static source/module pins and black-box PPv2 source, 1400-byte payload, two-client, and reply behavior are verified; the ordinary package run skips only the capability-binary test when the environment variable is absent. |
| 2026-08-17 | Repository-local NGINX 1.31.4 dependency | `make nginx_local` in `warp/lb`, `zsh -n test.sh` in Connect, then the server and load-balancer UDP PPv2 black-box tests with `NGINX_UDP_PROXY_V2_BINARY=warp/lb/build/nginx-local/sbin/nginx`, normally and under `-race` | Pass. The native build identifies commit `11d11b5f0d3d8ace5215e1a77918e9dc219ce7db`, includes `--with-stream`, and subsequent `make nginx_local` calls are incremental. Both tests preserve the original client tuple, 1,400-byte payloads, two-client separation, and replies. `connect/test.sh` now builds and exports this exact dependency before starting Go tests. |
| 2026-08-17 | H3 DATAGRAM envelope v1 | `go test . -run 'Test(H3Datagram\|PlatformTransportH3Datagram)' -count=1` and the same selection with `-race` in Connect | Pass. Auth negotiation, mixed capability fallback, every fragment boundary, reverse-order delivery, duplicate retirement, overlap, corrupt checksum, invalid declarations, hard expiry, shared budget recovery, sender refusal, and live Pack/ACK routing are deterministic and race-clean. |
| 2026-08-17 | Existing legacy H3 lifecycle after capability offer | `go test . -run 'TestPlatformTransport(H3PacketConnFactoryOwnsConnectedEndpoint\|CloseInterruptsBlockedH3Write\|H3CloseDrainsQueuedReceiveOwnership)$' -count=1` and the same selection with `-race` | Pass. A server without RFC 9221 acceptance keeps the current stream on the same connection; blocked-write cancellation, endpoint ownership, queued receive draining, and auth-frame pool ownership remain intact. |
| 2026-08-17 | Server DATAGRAM configuration and metrics | `go test ./connect -run 'Test(ConnectQuicConfig\|ConnectQuicAuthFrame\|ConnectH3DatagramCollector)' -count=1` and the same selection with `-race` | Pass. The rollout switch controls the QUIC transport parameter, pooled auth frames remain balanced, and both bounded-label Prometheus families report exact complete-message, fragment, and envelope-byte totals. |
| 2026-08-17 | Database-backed server H3 integration | `go test ./connect -run '^TestConnectH3$' -count=1 -timeout=3m` | Environment-blocked after its built-in five retries: `WARP_ENV` and vault `pg.yml` are absent. This does not contradict the local new/new QUIC round trip or focused server tests; a configured integration environment is still required before claiming the real resident path. |
| 2026-08-17 | Full Connect working tree after DATAGRAM v1 | `go test ./... -short -count=1 -timeout=10m` | Pass. Main Connect package completed in 209.892 s; `blocker`, `connectctl`, `extender`, protocol, and security completed successfully. |
| 2026-08-17 | Server working tree, full-Transfer fragmented cell-edge A/B | `go test ./connect/perfvar -run '^TestH3TransferCarrierCellEdgeComparison$' -count=3 -v -timeout=4m` | Pass and exact payload delivery in all six runs. Legacy H3 completed in 2.164--2.254 s; DATAGRAM completed in 13.545--13.568 s. First message was 0.246--0.285 s in both modes. DATAGRAM reduced observed wire bytes but caused 26--27 Pack rewrites and 3--5 reassembly timeouts versus 7--11 legacy rewrites. The focused race run also passed. |
| 2026-08-17 | Server working tree, single-DATAGRAM-per-Pack isolation | `go test ./connect/perfvar -run '^TestH3TransferCarrierCellEdgeSingleDatagramComparison$' -count=1 -v -timeout=4m` | Pass and exact 43,008-byte delivery. Legacy H3 completed in 2.221 s and DATAGRAM in 13.957 s; both delivered the first message in about 0.248 s. DATAGRAM had zero fragment reassembly timeouts but 39 Pack rewrites versus 16, isolating Transfer loss recovery as the dominant tail. |
| 2026-08-17 | Working tree, guarded selective-ACK recovery | Focused `SendSequence` scoreboard, burst-bound, reordering threshold, tail/cumulative probe, conservative follow-up, minimum-RTT, and end-to-end gap-recovery tests | Pass normally. The implementation limits immediate gap recovery to four proven holes, requires three distinct later deliveries, never re-arms the same immediate recovery, and keeps tail probes dormant on reliable ordered carriers. The focused race selection must be repeated after the final flight-control refinement. |
| 2026-08-17 | Server working tree, aggressive recovery experiment | Database-free full-Transfer `cell-edge-1m-down-250k-up` A/B | DATAGRAM reached 2.604 s versus 2.186 s legacy, but required 28 selective-gap plus 32 tail-probe writes. Rejected: the completion gain came from an unacceptable 60-write retry train. |
| 2026-08-17 | Server working tree, production-boundary recovery candidate | Database-free full-Transfer A/B with carrier route capacity corrected from 128 to production's 32 frames | Three representative paired runs completed legacy/DATAGRAM in 3.413/5.786 s, 2.242/4.296 s, and 3.069/5.672 s. DATAGRAM issued about 25 gap writes and 11--15 bounded tail writes. Exact payload delivery passed, but DATAGRAM remains a negative release result. |
| 2026-08-18 | Connect `c3bc4472b34a` + working tree, TCP end-to-end ACK invariant | Focused ACK-policy, direct-collapse, provider-return first-drop recovery, sole-watchdog source anchor, H3 hybrid, MTU sizing, and live H3 round-trip selection, normally and under `-race` | Pass. TCP is ACK-required in both directions for direct and platform routes, an explicit lower-level NoAck hint is overridden at the final singleton/group boundary, a dropped direct provider-return Pack is retried, direct retransmits remain collapse-controlled, resize cannot bypass watchdog gates, and oversized control/data select stream. Later exact geometry pinned the one-DATAGRAM inner-MTU limits at 944/934 bytes. |
| 2026-08-18 | Connect `c3bc4472b34a`, server `af8117e380a1` + working trees, one-DATAGRAM hybrid before the duplicate-verdict correction | Cold `TestH3LowBarFullTcpPacketTrack`, seed `20260817`, `cell-edge-1m-down-250k-up`, 64 KiB upload | One run passed exact hash in 9.420 s (0.056 Mbit/s), with 280,271 wire bytes, 15 loss drops, 14 queue drops, 220 DATAGRAM sends / 193 receives, and eleven exact 1,515-byte contract-only stream writes. A repeat was reset at 34.747 s by the resize-side stall-verdict bypass despite zero receive-handoff or DATAGRAM integrity drops; this exposed the adjacent health bug rather than establishing a stable performance win. |
| 2026-08-18 | Same revisions, rejected two-DATAGRAM contract experiment | Cold full-TUN route readiness under the same profile and seed | Failed before measurement: readiness read timed out after 65.146 s. The one-DATAGRAM/stream hybrid was restored; the unfavorable result is retained as the fragment-vs-stream gate. |
| 2026-08-18 | Same revisions after making the watchdog the sole send-stall conviction owner | Cold `TestH3LowBarFullTcpPacketTrack`, same seed/profile/64 KiB upload | Pass with exact payload/hash and no client replacement. Setup was 0.469 s, transfer 26.565 s (0.020 Mbit/s), carrier 27.260 s, wire 371,232 bytes, 19 loss drops, 11 queue drops, 275 one-fragment DATAGRAM sends / 207 receives, and 30 exact 1,515-byte contract-only stream sends/receives. The watchdog held the unproven-uplink verdict and later accepted a liveness response instead of resetting the TCP flow. Correctness improved; throughput remains a release blocker. |
| 2026-08-18 | Compact contract / bounded unreliable recovery candidate before hybrid-liveness correction | Cold `TestH3LowBarFullTcpPacketTrack`, same seed/profile/64 KiB upload | Common completions improved to 8.44--8.62 s and about 266--283 KiB with zero stream messages, versus the approximately 26.17 s / 393 KiB / 33-stream-message original compact/full-TUN baseline. The same candidate also produced 43.06 s and 79.43 s completions plus a timeout, so the typical-case gain was not a tail win. |
| 2026-08-18 | Hybrid stream-deadline removal plus independent QUIC keepalive | `go test -race -run '^(TestPlatformQuicConfigEnablesPathMtuDiscovery\|TestPlatformTransportH3DatagramRoundTrip)$' .` in Connect and the server QUIC-config race test | Pass. The live round trip leaves the reliable lane empty for three former read-deadline periods, then successfully exchanges both hybrid lanes. Client and server pin a connection-level keepalive independent of DATAGRAM-writer backpressure. |
| 2026-08-18 | Same hybrid-liveness candidate, post-change full-TUN repetitions | Four cold attempts of `TestH3LowBarFullTcpPacketTrack`, seed `20260817`, `cell-edge-1m-down-250k-up`, 64 KiB upload | Exact completions were 6.880 s / 247,389 B, 83.934 s / 569,418 B, and 6.834 s / 254,532 B; all used one-DATAGRAM packet traffic with zero stream messages. A fourth attempt timed out during route readiness after 65.280 s. The fast path is about 74% faster and 35--37% fewer bytes than the original baseline, but the 83.934 s tail and setup failure keep the candidate behind the release gate. |
| 2026-08-18 | Opt-in QUIC packet/drop/key telemetry on the full-TUN gate | Cold runs completed in 19.283 s / 364,362 B, 87.226 s / 720,458 B, and 8.021 s / 256,193 B. Every classified server-side QUIC drop was a payload-decryption failure, not a duplicate, DOS-prevention, header, connection-id, or application receive-queue drop. The failures occurred only after the connection's first key update; the 87.226-second run recorded 112 such drops. | The long tail is below the H3 application lane and is correlated with QUIC key-phase handling. Keep the qlog reducer opt-in and interval-scoped so further experiments can distinguish connection startup, key update/discard, transport loss, and application admission without production overhead. |
| 2026-08-18 | Rejected previous-QUIC-key retention experiment | A dependency-only A/B raised `quic-go`'s previous receive-key retention from three PTOs to at least 10 seconds. An initial run completed in 8.132 s / 247,740 B, but three cold validations produced 64.904 s / 564,898 B, 8.622 s / 267,175 B, and a 61.459-second route-readiness timeout. The slow completion still had 20 post-update payload-decryption drops and 24 inner-TCP retransmits; the failed setup repeatedly churned H3 auth/connect generations and hit QUIC no-recent-network-activity timeouts. | Reverted the module-cache experiment. Longer old-key retention alone does not remove the tail. Healthy established DATAGRAM transfer is still materially better than the approximately 26.17 s / 393 KiB original baseline, but the complete startup-plus-transfer distribution is not yet a win and remains blocked from rollout. |
| 2026-08-18 | Key-update correlation rejection and packet-MTU localization | Delaying the first key update to 100,000 packets still produced 4--47 pre-update decrypt failures. Corrected short-header and source-TUN fingerprints matched every accepted packet and none of the rejected packets. A representative slow run took 62.067 s / 566,886 B with 23 unmatched decrypt failures. | The decrypt tail was not stale key material. The rejected sizes and fragmentation boundary localized it to oversized padded QUIC packets below the application DATAGRAM layer. The temporary dependency checksum instrumentation and key interval are reverted byte-for-byte to the v0.61.0 module archive. |
| 2026-08-18 | Safe QUIC startup plus corrected PERFVAR carrier MTU | Client and server now use a 1,200-byte initial QUIC packet. Before fixing the carrier-TUN boundary, a run timed out with 75 exact 1,200-byte decrypt failures; after the harness fix, decrypt failures fell to zero and a stream-fallback diagnostic completed in 6.787 s / 269,827 B. | Both changes are necessary: the product configuration avoids fragmentation at a real 1,280-byte path, and the harness now models that path instead of an unintended 1,200-byte physical interface. |
| 2026-08-18 | Bounded two-DATAGRAM application candidate | Focused normal/race selection, sizing, path-shrink, fragmented retry, and live round-trip tests pass. A diagnostic full-TUN run completed in 7.246 s / 236,950 B with 105 classified IP sends on DATAGRAM, zero stream sends, zero decrypt failures, zero inner-TCP retransmits, and two bounded fragment expiries. | Keep the candidate for stock-dependency repetitions. The rejected 1,515-byte contract-control fragmentation experiment remains rejected; this candidate fragments only application-bearing frames below the hybrid threshold. |
| 2026-08-18 | Stock `quic-go` v0.61.0 cold repetitions after the MTU/fragment corrections | Three separate-process `TestH3LowBarFullTcpPacketTrack` runs completed exact 64 KiB uploads in 6.203, 7.816, and 9.812 s using 242,258, 261,179, and 267,921 wire bytes. All had zero IP stream messages and zero decrypt failures. Route readiness was 50.251, 6.743, and 8.275 s. | Established transfer is consistently about 62--76% faster and 32--40% lower-byte than the approximately 26.17 s / 393 KiB historical baseline. The 50.251-second readiness tail means end-to-end startup is not yet a release win; investigate H3 route/auth readiness next. |
| 2026-08-18 | Bilateral physical-carrier MTU correction with stock `quic-go` v0.61.0 | Four cold, separate-process `TestH3LowBarFullTcpPacketTrack` runs completed exact 64 KiB uploads in 10.076, 7.676, 8.791, and 8.813 s using 324,850, 248,828, 261,862, and 263,879 wire bytes. Route readiness was 6.433, 6.874, 8.718, and 8.465 s. Every run used the DATAGRAM packet lane for all IP traffic, used zero IP stream messages, and recorded zero payload-decrypt failures. The focused `TestFullTunExchangeH3MtuCorrectness` gate also passes in the configured local environment. | Median established transfer is 8.802 s / 262,871 B and median readiness is 7.670 s. Against the approximately 26.17 s / 393 KiB original baseline, individual runs are 61.5--70.7% faster and use 19.3--38.2% fewer wire bytes. This is the first corrected set with both common-case and startup-tail improvement; treat it as provisional until the frozen campaign reproduces it over more cold runs. |
| 2026-08-18 | Frozen packet-track reproduction, same revisions and stock `quic-go` v0.61.0 | Five new cold, separate-process `TestH3LowBarFullTcpPacketTrack` runs completed exact 64 KiB uploads in 7.097, 8.319, 8.228, 7.545, and 7.367 s using 248,735, 254,585, 266,195, 273,208, and 267,051 wire bytes. Route readiness was 8.458, 8.804, 6.787, 7.620, and 8.847 s. Every run passed with zero payload-decrypt, send, malformed, checksum, reassembly-limit, and lane-decode failures; all IP traffic used DATAGRAM and none used stream. | Median transfer is 7.545 s / 266,195 B and median readiness is 8.458 s. Individual runs are 68.2--72.9% faster and use 32.1--38.2% fewer wire bytes than the approximately 26.17 s / 393 KiB original baseline. Across all nine fully corrected cold runs, transfer is 7.097--10.076 s with an 8.228 s median, readiness is 6.433--8.847 s with an 8.458 s median, and no prior 40--65 s tail recurs. Advance to the same-profile direct/H1/legacy-H3 comparison; do not infer broad mobile release readiness from this single workload/profile. |
| 2026-08-18 | Rejected global 900-MTU and warm-flight experiments | Five cold one-DATAGRAM MTU-900 runs had a 6.604 s / 249,638 B median versus the frozen MTU-1100 fragmented median, but current H1 at MTU 900 regressed about 17% in time and 16% in bytes, and one legacy-H3 stream run regressed to 5.071 s / 347,087 B. A 10 KiB cold flight did not improve the distribution. An unbounded warm release was fast but used 264,938--278,277 B with 12--20 queue drops; bounded 12 KiB warm growth still produced queue/retransmit pressure and was invalid for two-fragment messages. | Keep global MTU 1,100, cold flight 8 KiB, retry ceiling 2 s, and receiver-evidenced flight growth. The warm shortcut is removed. |
| 2026-08-18 | Refreshed fragmented H3 control after the TCP-ACK invariant | Five cold `TestH3LowBarFullTcpFragmentedPacketTrack`-equivalent runs completed in 8.689, 8.909, 7.741, 8.413, and 8.305 s using 269,353, 283,341, 261,339, 259,251, and 266,629 B. Every run delivered the exact 64 KiB hash with all classified IP traffic on DATAGRAM. | Median 8.413 s / 266,629 B. This is the frozen control for the same-worktree one-fragment decision. |
| 2026-08-18 | One-DATAGRAM/stream hybrid candidate, MTU 1,100 | Five cold runs completed in 6.793, 6.958, 6.117, 8.333, and 6.159 s using 243,633, 263,847, 240,300, 252,826, and 226,561 B. Every run delivered the exact hash, used both IP lanes, emitted exactly one fragment per DATAGRAM message, and had zero malformed/checksum/reassembly failures. | Median 6.793 s / 243,633 B: 19.3% faster and 8.6% fewer bytes than fragmented H3. Promote the one-fragment maximum to the production default. |
| 2026-08-18 | Same-profile reliable-stream controls | Five cold H1 runs had a 5.180 s / 307,814 B median. Five cold legacy-H3 stream runs had a 3.995 s / 339,130 B median. All ten delivered the exact hash. | The hybrid trades completion for wire efficiency versus both controls. H1 remains the faster production companion in Auto; broader validation is still required. |
| 2026-08-18 | Production-default one-fragment verification | Focused H3 envelope/geometry tests and all low-bar compile gates passed after changing `DefaultH3DatagramSettings().MaxFragmentCount` to one. One cold `TestH3LowBarFullTcpProductionHybridTrack` completed in 4.812 s / 218,728 B with 105 messages/105 fragments, both IP lanes active, and no carrier-integrity failure. | The promoted default exercises the measured hybrid rather than the fragmented control. Keep the explicit fragment-count override tests to prevent framing/reassembly coverage loss. |
| 2026-08-18 | Rejected route-wide-to-per-write recovery intermediates, Connect `c3bc447` + working tree | The first five cold lane-classified runs completed in 5.947, 5.135, 7.162, 5.824, and 5.924 s using 273,281, 299,907, 403,494, 247,608, and 348,817 B (median 5.924 s / 299,907 B). Pre-publication path-size discovery improved a second five-run median to 5.376 s / 289,042 B, but stream frames still inherited the DATAGRAM two-second recovery cadence. | Both candidates improved time but regressed bytes versus the earlier 6.793 s / 243,633 B hybrid. Reject them. A performance candidate must improve both axes and preserve TCP's Transfer ACK. |
| 2026-08-18 | Lane-accurate nested recovery, Connect `c3bc447` and server `af8117e3` + working trees | Five cold `TestH3LowBarFullTcpProductionHybridTrack` processes completed in 4.109, 3.305, 3.701, 4.060, and 4.144 s using 210,349, 204,135, 203,021, 212,482, and 208,684 B. Median was 4.060 s / 208,684 B; every run delivered the exact hash and emitted exactly 62 hybrid stream messages. Focused TCP-ACK, flight, H3 sizing, path-shrink, and live round-trip tests passed normally and under `-race`. | Retain. Against the pre-lane hybrid median this is 40.2% faster and 14.3% lower-byte; against fragmented H3 it is 51.8% faster and 21.7% lower-byte. The optimization leaves TCP `ack=true` and changes only which successful physical writes consume unreliable flight or use the nested-recovery delay. |
| 2026-08-18 | Exact hybrid-route retirement recovery and post-change cold verification, same revisions + working trees | Focused tests prove that withdrawing the exact accepting H3 route retries immediately through its replacement, while adding a sibling route produces no write for 350 ms; both pass normally and under `-race`. Five new cold production-hybrid runs completed in 5.000, 5.173, 3.662, 5.143, and 3.962 s using 210,175, 204,672, 209,654, 208,631, and 209,948 B. Median was 5.000 s / 209,654 B; all exact hashes passed, with stream send counts 62, 62, 63, 62, and 62. | Keep the failover correction and record the unfavorable timing shift. The current median remains 80.9% faster and 47.9% lower-byte than the approximately 26.17 s / 393 KiB original reference, and 40.6% faster / 21.4% lower-byte than fragmented H3. The single extra stream write and bimodal timing keep variance/recovery attribution open. |
| 2026-08-18 | Schema-6 deterministic one-second-outage attribution | Current H3 completed in 43.298 s / 6,097,889 B; current H1 completed in 46.462 s / 6,023,722 B. H3 provider recovery recorded 17 timeout writes and 137 flight waits totaling 18.212 s; H1 recorded 17 provider timeouts and 99 additional device timeout writes, with no unreliable flight. Both exact hashes and schedule events passed. | H3 is 6.8% faster in this trace and avoids the nested H1 timeout train, but uses 1.23% more bytes. Keep the result diagnostic until the frozen dynamic campaign has five traces per route. |
| 2026-08-18 | Rejected message-flight 12/4 and 12/8 candidates | The 12/4 static campaign completed in 3.800, 4.294, 3.933, 3.572, and 4.591 s using 205,437--212,021 B (3.933 s / 209,524 B median). Its two recorded outage runs completed in 45.118 s / 6,145,020 B and 44.887 s / 6,047,656 B; provider waits varied from 22 / 3.464 s to 117 / 19.213 s. The 12/8 follow-up immediately amplified one static run to 257,383 B, 74 stream writes, and 30 TCP retransmits. | Reverted to 8/4. The static median alone does not outweigh neutral-to-worse outage timing or a reproducible loss-floor amplification. |
| 2026-08-18 | Rejected ready-drain coalescing and +2 additive recovery | Eight-frame coalescing had a 3.345 s / 209,166 B H3 median, but a 6.151 s tail and one 29-retransmit run; H1 measured 4.968 s / 316,607 B median versus 5.180 s / 307,814 B control. Its outage trace was neutral at 43.262 s / 6,071,794 B with 161 provider waits. Four-frame runs used 225,844--243,973 B with 25--29 retransmits. A +2 message-recovery outage took 45.772 s / 6,108,202 B with 18 selective gaps, 168 waits / 20.909 s, and 225 queue drops. | All branches reverted. Focused ownership, lifecycle, decoder, provider-return, TCP-ACK, and race tests passed during the experiment; the performance gate, not correctness, rejected them. |
| 2026-08-18 | Current dynamic rate-collapse diagnostic | The cold H3/H1 pair completed in 49.373 / 49.678 s using 6,199,953 / 6,150,535 B. The warmed pair completed in 44.832 / 53.775 s using 6,063,883 / 6,805,198 B; H3/H1 forward queue drops were 201 / 364 and device timeout rewrites were 2 / 390. Every exact hash and scheduled event passed. | The warmed H3 trace is 16.6% faster and 10.9% lower-byte, while the cold trace is neutral. This is the first material current-tree H3 win under dynamic rate collapse, but one pair is diagnostic rather than a release baseline. |
| 2026-08-18 | Current dynamic live-MTU diagnostic | Cold H3/H1 completed in 38.136 / 48.555 s using 5,898,076 / 6,074,454 B; warmed H3/H1 completed in 37.404 / 49.385 s using 5,884,945 / 6,365,014 B. H3/H1 forward queue drops were 153 / 309 cold and 131 / 266 warmed. H3 had zero MTU drops in both traces; H1 had 10 / 16. Every exact hash and event passed. | H3 is 21.5--24.3% faster and 2.9--7.5% lower-byte in the first two pairs. The warmed H3 record misses only the conservative 10% calibration-separation rule because it runs within 5.5% of underlay; retain the raw diagnostic and require five paired traces before a release claim. |
| 2026-08-18 | Equal-priority Auto route selection on Connect `c3bc4472b34a` and server `af8117e380a1` plus working trees | Five fresh-process `tcp-warmed` uploads used seed `20260810`, `cell-edge-1m-down-250k-up`, one hop, mobile surrogate, and 256 KiB. Strict per-sequence affinity completed in 14.883--15.657 s using 850,432--881,204 B with 29--68 queue drops; median was 15.190 s / 864,267 B / 40 drops. Five forced-H1 controls had a 24.461 s / 1,861,026 B / 40-drop median; five forced-H3 controls had a 13.044 s / 797,068 B / 19-drop median. Every exact hash passed. | Retain strict destination-sequence affinity: it is 45.3% faster, 42.5% lower-byte, and has 74.7% fewer queue drops than the 27.780 s / 1,502,911 B / 158-drop frame-shuffling Auto reference. Do not spill on queue pressure. Do not use the rejected client-wide choice, which pegged all sequences to startup-order H1. The measured Auto upload chose H3 in all five processes, but clean correctness runs prove selection can differ by sequence and direction; this is not yet an adaptive best-carrier policy. Most calibrations missed only the conservative 10% underlay-separation rule, so the exact tunneled results are correctness-valid but not aggregate-valid. |
| 2026-08-18 | Permanent empty-transport-set recovery | The full Connect suite exposed a provider-flapping case where an unanswered cping ended without conviction, the empty route set held every silence verdict, and aged zero/zero stats looked healthy; the already-recorded `transportDownSince` epoch was never consumed, so five dead providers remained `Added`. Empty transport sets now retain the existing migration grace but retire structurally after `StatsWindowKeepUnhealthyDuration` (60 s by default; 2 s in the stress fixture). The deterministic expiry boundary passed 20 repetitions, and the formerly failing 45-second flapping test passed with `stuck=0`. | Retain the non-convicting single-cping behavior for lossy links and keep route restoration eligible throughout the grace. A route set that never returns must construct a fresh client rather than remain selected forever. Race, full-suite, and physical migration validation remain open. |
| 2026-08-18 | Cold H3/H1 focused control, Connect `c3bc4472b34a` + working tree | Five alternating fresh-process pairs, seed `20260817`, `cell-edge-1m-down-250k-up`, 1,100 MTU, 64-KiB upload | H3 median 4.242 s / 215,626 B versus H1 5.127 s / 319,611 B: H3 was 17.3% faster and 32.5% lower-byte. H3's 6.674-second maximum correlated with 72 stream messages rather than the usual 62; retain that tail for lane attribution. |
| 2026-08-18 | Canonical static matrix, server `af8117e380a1` state `fcbcc99d`, Connect `c3bc4472b34a` state `8bba31c9` | Local artifact `lowbar-auto-matrix.ipiedL.log`; five fresh-process runs per `p2p-fast,exchange-h1,exchange-h3,exchange-auto`, seed `20260810`, `cell-edge-1m-down-250k-up`, `tcp-warmed`, upload, one hop, mobile surrogate, 256 KiB | All Auto/H1/H3 payloads were exact. All-sample medians: Auto 14.983 s / 876,468 B, H3 15.268 s / 818,234 B, H1 30.285 s / 1,976,566 B. Calibration-invalid counts were 2/5, 3/5, and 1/5 respectively. Initial P2P delivered only 1/5, at 44.646 s / 561,768 B median; four runs recorded 1--7 small fast-receive queue drops. |
| 2026-08-18 | Rejected native-P2P count-only bounds | Local artifacts `lowbar-p2p-flight-v2.Ie0qSF.log`, `lowbar-p2p-flight-cap.KGWFRJ.log`, and `lowbar-p2p-flight-reserve.kPSp5m.log` | Correct unreliable-carrier classification cut fast sends to roughly 391--433 and improved completion, but the uncapped queue still lost one tiny message in 3/5 runs. A four-message cap took 26.66--27.30 s and still lost a 91-byte provider message. Reserving at a three-message cap removed that drop but regressed the first run to 35.348 s / 502,865 B. Both count-only caps were rejected. |
| 2026-08-18 | Retained native-P2P byte-bounded queue, server `af8117e380a1` state `fcbcc99d`, Connect `c3bc4472b34a` state `e81cca11` | Local artifact `lowbar-p2p-byte-queue.1coBYz.log`; five fresh-process `p2p-fast` runs under the canonical static scenario | All five exact hashes and calibrations passed. Durations were 23.806, 21.611, 22.921, 23.551, and 22.682 s; wire counts were 478,585, 475,321, 485,332, 488,146, and 466,703 B. Neither endpoint recorded a receive-queue drop. Against the initial P2P matrix, median time improved 48.7%, median bytes 14.8%, maximum time 55.5%, and correctness 1/5 to 5/5. |
| 2026-08-18 | Native-P2P queue ownership and receive policy | Queue bounds, final pool drain, route publication/readiness rematch, fast-worker join, adaptive read, prefetch, fast-only round trip, and flight-controller selections; focused lifecycle/policy selection repeated 20 times and the changed boundaries run under `-race` | Pass. `run` and `runFast` contain no channel send; each offers zero-wait into the bounded adapter. Its sole forwarding worker is joined, exact pending bytes return to zero, and both the blocked carrier read buffer and queued frame return to the pool before `Done`. |
| 2026-08-18 | Full Connect short gate after retained P2P correction, before exact worker-held count closeout | `go test ./... -short -count=1 -timeout=10m` | Pass. Main package 206.863 s; `blocker`, `connectctl`, and `extender` green. The first attempt correctly exposed an overbroad structural source check; after scoping it to the actual carrier-reader functions, the complete rerun passed. |
| 2026-08-18 | SDK DeviceLocal + DeviceRemote + RPC memory gate before exact worker-held count closeout | `go test . -run '^TestDeviceLocalSyntheticDeviceRemoteMemorySoak$' -count=1 -v -timeout=8m` in SDK | Pass in 62.58 s across 95 web/mail/blocked/provider cycles. Peak heap was 9.1 MiB, peak runtime memory 24.3 MiB, recovered heap 4.7--6.8 MiB, and teardown returned to 4.1 MiB / 12 goroutines / 13 file descriptors / zero pooled buffers outstanding. |
| 2026-08-18 | Non-short Connect package gate before exact worker-held count closeout | `go test . -count=1 -timeout=20m` | Pass in 459.346 s. The complete package order reproduced neither the repaired P2P final-drain hang nor the earlier provider-flapping failure. |
| 2026-08-18 | Exact native-P2P retained-message accounting | Focused count/byte/refusal/final-drain/policy selection repeated 20 times, broad P2P selection, and changed boundaries under `-race` | Pass. The hard 16-message total includes the off-channel item held by the forwarding worker; a seventeenth small message drops immediately, all message/byte reservations return to zero, and the broad P2P selection completes in 1.078 s. |
| 2026-08-18 | Final-source full Connect short gate | `go test ./... -short -count=1 -timeout=10m` | Pass. Main package 209.460 s; `blocker`, `connectctl`, and `extender` green. |
| 2026-08-18 | Final-source non-short Connect package gate | `go test . -count=1 -timeout=20m` | Pass in 468.173 s. No P2P lifecycle, receive-policy, provider-flapping, Transfer, or encryption regression. |
| 2026-08-18 | Final-source SDK DeviceLocal + DeviceRemote + RPC memory gate | `go test . -run '^TestDeviceLocalSyntheticDeviceRemoteMemorySoak$' -count=1 -v -timeout=8m` in SDK | Pass in 62.09 s across 94 web/mail/blocked/provider cycles. Peak heap was 9.3 MiB, peak runtime memory 24.1 MiB, recovered heap 4.7--7.0 MiB, and teardown returned to 4.2 MiB / 12 goroutines / 13 file descriptors / zero pooled buffers outstanding. |
| 2026-08-18 | Packet-aware flow scheduler and contract-safe NoAck admission | Focused scheduler, full-flight logical-group, contract-transition, and exact-debit tests normally and under `-race`; exact server H3 carrier and schema-9 observation tests | Pass. A full ACK resend window cannot block an eligible NoAck flow, contract rotation/exhaustion defers before serialization, committed bypass counters are exact, and H3 retains its bounded ACK reserve. |
| 2026-08-18 | Loaded-latency admission A/B, frozen binaries `507a17a631f3` and `3f4eecc3d393` | Five interleaved fresh-process runs per candidate, seed `20260818`, `cell-edge-1m-down-250k-up`, `latency-under-load`, upload, 256 KiB | Both delivered 5/5 exact payloads. Reserve-selected NoAck delivered 137/157 loaded probes with 13.669 s / 855,869 B medians; explicit NoAck with the reserve disabled delivered 145/164 with 14.764 s / 844,047 B medians. The latter improved probe delivery 1.15 points and wire/probe about 5.1%, but regressed median bulk time 8.0%; retain its semantics, not its disabled-reserve policy. |
| 2026-08-18 | Combined explicit-NoAck plus bounded-ACK-reserve campaign, frozen binary `2dec572e9a2a` | Five fresh-process schema-9 runs under the same loaded-latency scenario | Every exact payload passed. Median was 13.270 s / 832,234 B; 138/154 loaded probes arrived (89.6%), and median p50/p95 were 0.974/1.902 s. Provider recovery committed 116 NoAck bypasses, zero reserve selections/uses, 872 flight waits / 60.39 s, and 93 timeout rewrites. All five records missed only calibration headroom, so retain the raw comparative result without promoting it to a release threshold. |
| 2026-08-18 | Rejected hybrid single-message stream-yield candidate, frozen binary `4c1c366e99b2` | Two fresh-process schema-9 loaded-latency runs | Exact payloads passed, but completion regressed to 18.374/19.576 s, wire bytes to 901,528/907,662 B, and loaded p95 to 2.625/2.410 s. Removed. quic-go already prioritizes submitted DATAGRAM frames; sacrificing stream batching did not solve the FIFO before lane submission. |
| 2026-08-18 | Full short Connect gate after packet-aware NoAck admission | `go test ./... -short -count=1 -timeout=10m` | Pass. Main package 212.963 s; `blocker`, `connectctl`, and `extender` green. |
| 2026-08-18 | Retained bounded H3 split-lane dispatcher, frozen binary `4f5706eeaee1998cb939aeada4095c0ac5e6386d3b05a69b32e2700a589d08b9` | Five fresh-process schema-10 `latency-under-load` uploads, seed `20260818`, `cell-edge-1m-down-250k-up`, one hop, mobile surrogate, 256 KiB | Exact payloads passed 5/5. Against frozen pre-split v10, median completion improved 3.44%, p50/p95 20.45%/2.88%, aggregate probe delivery 4.80 points, success rate 3.55%, and queue drops 25.64%; median wire bytes rose 0.27%. The stream queue hit its explicit 32-message / 65,920-byte bound, returned to zero, and had no oversize admission. Retain the count-plus-capacity-byte bound; a faster count-only intermediate was rejected because retained backing was not bounded. |
| 2026-08-18 | Recovery-attempt versus physical-carrier accounting | Transfer now records `recovery_write_error_count`; the severe 64-kbit/s carrier fixture separately observes resend attempts, physical repeated writes, route-admitted frames drained at teardown, and failed first admissions | Pass with exact reconciliation. A recovery can be admitted to the bounded route and still remain unwritten when the useful workload completes, so carrier repeats alone are not the recovery invariant. On the restored policy, legacy stream completed in 10.092 s / 91,846 forward bytes and hybrid H3 in 8.399 s / 56,481 bytes: hybrid was 16.78% faster and 38.51% lower-byte. Local zero-wait receive drops remain counted and exact end-to-end payload delivery remains mandatory. |
| 2026-08-18 | Rejected H1/legacy reliable-stream recovery-delay generalization | A fixed eight-second delay first improved an isolated severe carrier, but the ten-pair production-shaped H1 workload raised total bytes 1.6%, drops 7.2%, and device/provider timeout rewrites 6.5%/16.0%; only 6/10 time pairs and 5/10 wire pairs won. A scaled four-to-eight-second variant then regressed five-pair H1 median time 38.4%, wire bytes 10.8%, and queue drops 90%. | Both candidates were removed. Only negotiated H3 hybrid-stream writes use the delayed nested recovery and exact accepting-route retirement retry. H1/legacy retains normal Transfer ACK/retry. Transport-up is never treated as proof of delivery. |
| 2026-08-18 | Terminal raw `SendPack` pool-publication ownership | The broad PERFVAR race tier found sequence cleanup reading `SendPack.admission` after `releaseRaw()` had already published the object to the Client reuse pool and another provider-return sender had begun reinitializing it. `releaseRaw()` is now explicitly terminal; every redundant post-publication admission release was removed, leaving enqueue rejection as the only separate pre-publication release path. | Focused raw lifecycle/cancellation tests passed 20 times under `-race`; the exact P2P-fast MTU reproducer passed in 58.481 s; the complete PERFVAR short race tier passed in 398.440 s with no `GORACE` report. The earlier `becfc1575333` performance cohort is retained only as variance evidence because it predates this repair. |
| 2026-08-18 | Authoritative repaired-source loaded-latency A/B, frozen binaries `2dec572e9a2a` and `fb9907a00f08` | Five alternating-order, fresh-process pre-split/final pairs under the same schema-11 one-bar scenario; artifacts `/tmp/lowbar-openloop-v20-paired-{baseline,final}-{1..5}.log` | All ten exact payloads passed. Final won all five completion pairs: median 13.531 to 12.593 s (**6.93% faster**). Loaded p50/p95 fell 1.065/2.147 to 0.572/1.453 s (**46.22%/32.33% lower**), probe delivery rose 87.26% to 95.07% (**+7.81 points**), successes/s rose **7.54%**, queue drops fell 168 to 101 (**39.88% fewer**), provider waits fell 22.14% / 30.65% by count/duration, and timeout rewrites fell 15.0%. Median wire bytes rose 836,233 to 862,065 (**3.09% higher**) and won only 1/5 pairs, so this is a latency/delivery win with a small byte-cost regression. Four final calibrations were headroom-invalid; this is the current raw paired candidate comparison, not a release-throughput or physical-radio claim. Physical one-bar iOS/Android validation remains open. |

## References

- [QUIC transport — RFC 9000](https://www.rfc-editor.org/rfc/rfc9000.html)
- [QUIC loss detection and congestion control — RFC 9002](https://www.rfc-editor.org/rfc/rfc9002.html)
- [QUIC DATAGRAM — RFC 9221](https://www.rfc-editor.org/rfc/rfc9221.html)
- [CONNECT-IP — RFC 9484](https://www.rfc-editor.org/rfc/rfc9484.html)
- [FQ-CoDel — RFC 8290](https://www.rfc-editor.org/rfc/rfc8290.html)
- [Datagram PLPMTUD — RFC 8899](https://www.rfc-editor.org/rfc/rfc8899.html)
- [TCP retransmission timeout — RFC 6298](https://www.rfc-editor.org/rfc/rfc6298.html)
- [TCP-in-TCP considerations — RFC 8229](https://www.rfc-editor.org/rfc/rfc8229.html)
- [TLS 1.3 and 0-RTT replay — RFC 8446](https://www.rfc-editor.org/rfc/rfc8446.html)
