# DELAY: per-message transport delay, throughput and quality

Status: implemented 2026-10-27 in connect, server and sdk, merged to main
2026-10-28; §15 records what was built. The decisions in §2 were settled in review on
2026-10-27. Open items are in §14.

## 1. Purpose and scope

Measure how long each message takes to leave the sender and how much standing
queue it meets on the way, in both directions, on every transport connection of
a non-provider client. Turn the worst recent measurement into a per-direction
quality score from 0 (normal operation) to 1 (worst). The first consumer is a
real-time notification on Apple and Android telling the user their internet
connection is poor.

In scope:

- An optional fixed-size **delay header** on every message of a negotiated
  connection: H1 WebSocket, H1+, the H3 stream and DATAGRAM lanes, DNS-carried
  H3, and P2P.
- Per-connection measurement, published once per interval to a callback on
  `Client`. `RemoteUserNatMultiClient` passes those callbacks through.
- `DeviceLocal` windowing (worst-of-N with timeout M), quality scoring with
  tunable thresholds, and a quantized `TransportDelayListener`, threaded
  through `DeviceRemote` and the device rpc.
- The server's half: negotiation, stamping and echoing.

Out of scope:

- Congestion control and pacing changes.
- Per-flow or per-destination quality.
- Provider-egress or end-to-end slowness. The transfer layer's RTT
  (`transfer_rtt.go`, `transfer_sender_ack_timing.go`) spans the server and the
  provider, so it cannot isolate the user's own link. This design measures only
  the first hop (client ↔ server, or client ↔ peer on P2P).
- Provider connections. They do not offer the header for now (D3).

## 2. Decisions

| # | Question | Decision |
|---|---|---|
| D1 | Which signal | Both: write blocking (throughput) and one-way standing-queue delay. Each direction's quality is the worse of the two. |
| D2 | Which transports | All transports measure. |
| D3 | Which connections | Only non-provider client connections, meaning the multi client's window clients. The device's provider client does not offer the header. Peers that a window client connects to (the server, and the provider at the passive end of P2P) stamp and echo when offered, but never consume stats. |
| D4 | Idle or stale | A direction with no measurement within M drops to 0. |
| D5 | Aggregation | Across all transports of all clients, combined per second so the reading describes the user's link: throughput is summed over the connections that were busy in that second, and delay takes the second-highest connection when two or more report (refined 2026-10-27, §10.1). |
| D6 | Tuning units | Throughput thresholds in bits/s. Delay thresholds are necessarily durations. |
| D7 | Callback rate | Fire only on a quantized change: zero to non-zero, or a step change above zero. |
| D11 | Window statistic | Score the second-worst of the last N sampled seconds within M, instead of the worst, so one stray second cannot start an episode (2026-10-27, from the threshold research). |
| D8 | Consumer | Display only, as a real-time notification on Apple and Android. No control behavior yet. |
| D9 | Plumbing | Every new multi-client window client adds the multi client as a subscriber. The multi client passes samples through unchanged to its own subscriber (DeviceLocal). |
| D10 | DNS fallback | When the worst reading comes from a DNS-carried transport (`h3dns`, `h3dnspump`), keep the indicator but say URnetwork is using a slower fallback connection, not that the user's internet is slow. The payload carries `Fallback` (§10.4). |

## 3. Changes from the original proposal

1. **Method generics are not expressible in Go.** Methods cannot declare type
   parameters, and Go has no generics over array length (`[N]byte` for any
   N). A generic function constrained to a union such as
   `~[8]byte | ~[12]byte` compiles only with `unsafe`, because a union has no
   core type and `h[:]` is rejected, and the constraint fixes the allowed sizes
   anyway. The header length is therefore fixed per framer, and callers pass a
   `[]byte` view of a stack array (§7).
2. **`(end − start) / size` on a stream measures backpressure, not
   throughput.** A TCP write returns as soon as the bytes are in the socket send
   buffer. A quic-go stream write returns once all but at most one packet of the
   data has been packed into packets (§4.1). DATAGRAM and UDP sends never
   block. So a sample means something only when the writer was busy (§4.1).
3. **The header carries `previousWriteMicros`, a duration, instead of the
   previous write's end time.** The receiver needs the duration. Measuring it
   on the sender with Go's monotonic clock keeps device sleep out of it, and the
   receiver does no arithmetic across two messages. The measurement is still
   delayed by one write.
4. **Frames of one physical write share `sendStartMicros`.** The receiver
   groups them as one write and pairs the group with the next write's
   `previousWriteMicros`. This works for batches and needs no size or sequence
   field (§5.3).
5. **The local-versus-remote time difference is useful.** One-way delay minus
   its running minimum is the standing queue, as in LEDBAT's base delay and
   CoDel's minimum over an interval. The unknown clock offset cancels, and
   drift is negligible over minutes. The up direction needs the peer's view, so
   the header carries one echo field (§4.2).
6. **The transport clock must count device sleep.** Go's monotonic clock stops
   during sleep on darwin (`mach_absolute_time`) and on linux/android
   (`CLOCK_MONOTONIC`). Phones sleep with the tunnel connected, and each sleep
   would add its length to the up-direction delay the server measures (§5.2).
7. **Measure per message, publish per interval.** A callback per message from
   the writer goroutine, through two callback lists and a device lock, puts
   avoidable cost on the hot path (§8).

## 4. What is measured

### 4.1 Write blocking: throughput

The writer records each physical write's byte count and duration, using Go's
monotonic clock. What can block a write depends on the carrier:

| Carrier | What blocks a write |
|---|---|
| TCP: H1 WebSocket, H1+ | A full socket send buffer. Once it is full, each write waits for the network to drain its own size. |
| QUIC stream: H3, H3 DNS, H3 DNS pump | quic-go `SendStream.Write` copies and returns only when at most `MaxPacketBufferSize` remains. Until then it waits for the packer to consume data, which is gated by the congestion window, pacing and stream/connection flow control (quic-go v0.61.0 `send_stream.go:246-301`). The H3 retained-send flight gate (`quicSendFlightForConn`, transport.go:3500) can also wait, and it tracks unacknowledged bytes. |
| SCTP data channel (P2P legacy lane) | BlockWrite waits until the previous message's chunks enter flight (min(cwnd, rwnd)). It returns before acknowledgment (§6.1). |
| QUIC DATAGRAM | On mobile, `trySendDatagram` is zero-wait and spills to the stream. On desktop and the server, quic-go's 32-frame queue blocks. The lane is lossy, so it gives the delay signal only. |
| SRTP fast lane (P2P), DNS packet translation | Only the 256-entry UDP batch queue and the kernel buffer (fast lane). PT pacing sits under QUIC, so it shows up as H3 stream blocking. The fast lane gives the delay signal only. |

Estimator, per connection and direction, per interval (default 1 s):

- A write is **blocked** if its duration is at least `BlockedWriteMinDuration`
  (default 250 µs). Copying into a socket buffer takes microseconds, while a
  real wait is an ACK or a pacing slot, which is milliseconds.
- `busy = Σ blocked durations / interval`. If `busy` is at least
  `MinBusyFraction` (default 0.5), the interval's throughput is
  `Σ blocked bytes / Σ blocked durations`. Otherwise there is no throughput
  sample: the transport did not limit the writer.
- During the first `Warmup` of a connection (default 2 s) there are no
  throughput samples, because of TCP and QUIC slow start.

This follows BBR's app-limited rule: a sample taken while the sender was not
continuously sending must not lower a capacity estimate. The busy gate also
discards stalls unrelated to the network, such as a GC pause or a scheduler
delay. Without it, a single small write held up 30 ms would read as a
260 kbit/s link.

### 4.2 One-way standing-queue delay

For the first frame of each received write group:

```
owdRaw = localArrivalMicros − remoteSendStartMicros    // unknown constant offset
base   = min(owdRaw) over the base history              // LEDBAT base delay
queue  = owdRaw − base                                   // offset cancels
```

- **Base history:** the minimum per one-minute bucket, kept for
  `BaseHistoryCount` buckets (default 10, as in RFC 6817's BASE_HISTORY). A drop
  in the true base is learned immediately. A rise is learned only as old
  buckets age out.
- **Per-interval value:** the minimum `queue` within the interval. This is
  CoDel's standing-queue rule (RFC 8289): a queue that never drains during an
  interval is a standing queue, and transient bursts do not count. An interval
  needs at least `MinDelaySampleCount` samples (default 3) to report a value.
- **Down direction:** computed locally from the peer's `sendStartMicros`.
- **Up direction:** the peer computes `owdRaw` for our writes and echoes it in
  `echoOneWayMicros`, and we run the same base and interval filters on the
  echoed values. All the logic stays on the client. The server only stamps and
  echoes.
- **Drift:** typical phone oscillators are within 20–50 ppm, which is about
  1–3 ms per minute, or at most about 30 ms over a 10-minute history. That is
  well under the thresholds in §10.2. RFC 6817 discussion: drift matters only
  when drift × BASE_HISTORY approaches TARGET.
- **Receiver-limited censoring:** the arrival time is when our reader parsed the
  frame. If the reader had been blocked handing the previous message onward
  (receive queue backpressure, already counted by
  `PlatformTransportReceiveStats.recordQueueBackpressure`), the frame waited in
  the kernel buffer for a local reason. A group that arrives after a handoff
  wait longer than `ReceiverLimitedThreshold` (default 5 ms) gives no delay
  sample, and on the server it echoes 0.

### 4.3 Why both signals

Throughput catches a slow link only while it is busy. Delay catches bufferbloat
and RAN queues even at moderate throughput. A saturated deep-buffer cellular
uplink can show acceptable throughput with seconds of queue, which is the main
case for the delay signal. On H1 the delay signal also carries the down
direction, because nginx terminates TLS: the server's writes block only after
the load balancer's buffers and both sockets' buffers fill (§11). A thin link
under light load scores 0 on both signals, which is intended: it is not
limiting the user (D4).

## 5. Wire format: delay header v1

The header is 12 bytes, big-endian, and sits immediately before each message
payload. It is on **every** message in a negotiated connection: heartbeats
(zero-length payload), transport control messages, the 16-byte latency echo,
speed-test probes, and close notices.

| Offset | Field | Meaning |
|---|---|---|
| 0 | `sendStartMicros` uint32 | The sender's transport clock at the start of the physical write that carries this message. |
| 4 | `previousWriteMicros` uint32 | Duration of the previous completed physical write on this lane, from Go's monotonic clock, rounded up to at least 1. 0 means none: the first write, a failed write, or a write by an out-of-band writer. |
| 8 | `echoOneWayMicros` uint32 | `(sender arrival clock − peer sendStartMicros) mod 2^32` for the first frame of the most recent peer write group the sender accepted. 0 means no sample: none yet, or receiver-limited. |

### 5.1 Rules

- **Serial-number arithmetic** (RFC 1982). A uint32 in µs wraps every 71.6
  minutes. Every consumer differences only values within 35 minutes of each
  other: the base history is at most 10 minutes, intervals are seconds, and the
  receiver unwraps `sendStartMicros` into 64 bits by tracking its successive
  values. A legitimate echo of exactly 0 is read as "no sample", which costs
  one sample in 2^32.
- **One write, one `sendStartMicros`.** All frames of one physical write carry
  identical headers, so a batch writes the same 12 bytes before each frame. A
  new write's `sendStartMicros` differs from the previous one; the writer bumps
  it by 1 µs if the clock has not advanced. Receivers group by equality and do
  not require order, because out-of-band writers (§11) can interleave.
- **Lengths.** The header counts toward the frame length field and the
  WebSocket message, but is never part of the returned payload or of
  `MaxMessageLen`. Byte budgets that size messages to a carrier subtract it.
  Otherwise maximum-size messages move to another lane or split into one more
  fragment. The budgets are:
  - the compact Framer's uint16 frame limit;
  - the H3 DATAGRAM envelope threshold `1360 − 28`
    (transport_h3_datagram.go:42, 118, 131);
  - Transfer's per-message lane predicate (transport.go:3832-3842);
  - the DATAGRAM transfer limit `H3DatagramTransferFrameByteLimit`, used on
    both ends (server transport.go:619; transfer_route_manager.go:85-90);
  - P2P's fast-lane fragment (1188) and 64 KiB message cap (§6.1).
- **Lanes.** A connection with independent writers (H3 stream and DATAGRAM, P2P
  lanes) keeps one transport clock and one echo state. `previousWriteMicros`
  and receive grouping are per lane.

### 5.2 Transport clock

The transport clock is µs since the connection started, read from a clock that
is monotonic **and counts device sleep**:

- darwin: `CLOCK_MONOTONIC`, which keeps counting during sleep. Go's runtime
  uses `mach_absolute_time` (go/src/runtime/sys_darwin_arm64.s:147), which does
  not.
- linux and android: `CLOCK_BOOTTIME`. Go uses `CLOCK_MONOTONIC`
  (sys_linux_arm64.s:355).
- Both are read with `golang.org/x/sys/unix.ClockGettime`, which connect
  already depends on (v0.47.0, `zerrors_darwin_arm64.go:230`,
  `zerrors_linux.go:736`).
- windows: Go's monotonic clock is `QueryPerformanceCounter` on arm64
  (`os_windows_arm64.go:12`). Verify whether it counts sleep; if not, use
  `QueryInterruptTime`.
- js: Go's monotonic clock.

Keeping the hot path cheap: the clock is Go's monotonic reading plus a suspend
offset (sleep-inclusive minus Go monotonic). The offset is re-sampled only when
a write starts after the writer has been idle for at least 50 ms. A suspend can
only happen while the writer is idle or blocked. A write that spans a suspend
reports its monotonic duration, which excludes the suspend.

Why this matters: up-direction `owdRaw = serverArrival − clientSendStart`. If
the client's clock skipped a 30 s sleep, every later client timestamp would be
30 s behind, the server's `owdRaw` would jump by 30 s and stay there, and the
base could not fall to meet it. The up queue would read 30 s until the
pre-sleep buckets aged out, which is up to 10 minutes.

### 5.3 Grouping and the one-write delay

The receiver keeps one open group per lane: `sendStartMicros`, byte count, and
first arrival. For each frame:

- **Same `sendStartMicros`:** add the frame's bytes to the group.
- **New `sendStartMicros`:** close the previous group. If this frame's
  `previousWriteMicros` is not 0 **and the lane is reliable and ordered**,
  record a remote-write sample `(bytes, duration)`. Then open the new group and
  take its delay sample from this first frame.
- **Datagram lanes** (lossy and reorderable): delay samples only. Writes do not
  block, and pairing can be wrong after a loss.

A frame's byte count is payload + 12 + carrier framing: 4 for the Framer, an
estimated WebSocket header, and 28 for a DATAGRAM envelope.

### 5.4 Overhead

- **Wire:** 12 bytes per message. On near-MTU data frames (about 1.4 KB) that
  is about 0.9%. Small frames (transfer ACKs, control) pay proportionally
  more. Measure with the existing per-route ACK byte stats before enabling by
  default.
- **CPU:** one clock read per physical write (plus the occasional suspend
  sample) and one per received group.
- **Memory:** no allocation. Headers live in stack arrays, and payloads keep
  their pool buffers (§7).

## 6. Negotiation

The client offers only from the multi client's window transports (D3), and only
when the connection declares no provide intent. The server accepts only when
the connection declares no provide intent, whatever the client offers. Both
ends know provide intent at setup (`X-UR-Provide-Intent` or
`Auth.provide_intent`, transport_provide_intent.go). The version is an integer;
`1` is the 12-byte v1 header.

| Carrier | Offer | Accept | Old peer |
|---|---|---|---|
| H1 WebSocket (header auth) | Request header `Sec-WebSocket-Protocol: urnetwork-delay.1` (`TransportDelayH1Subprotocol`, `applyTransportDelayH1Offer`) | The server selects the same token on its 101 by passing `TransportDelayH1AcceptHeader()` to gorilla `Upgrader.Upgrade`; gorilla records the choice and the client reads `ws.Subprotocol()`. | An old server selects no subprotocol, so no delay header. |
| H1+ `urnetwork-framer/1` | The same request header, carried by the custom upgrade | The same token on the 101 through `AcceptFramedUpgradeWithHeader` (http_upgrade.go). The client stores the 101's `Sec-WebSocket-Protocol` on `httpUpgradeConn`, and `FramedMessageConn.Subprotocol()` exposes it. | The client's 101 validation already tolerates extra headers. If the token is absent, no delay header. |
| H1 legacy first-frame auth | Not offered. The auth echo must be byte-equal (transport.go:2501). | — | — |
| H3, H3 DNS, H3 DNS pump | `protocol.Auth.transport_delay_version = 11` | The server's Auth response sets `transport_delay_accepted_version = 12`. It must re-encode its response whenever it accepts, including the datagram-off branch that echoes raw bytes today (server transport.go:2699-2703). | An old server's raw echo leaves the accepted field at 0. The client validator compares named fields only (`ValidateH3DatagramAuthResponse`). The precedent is `h3_datagram_version` / `h3_datagram_accepted_version` (transfer.proto:302-309). |
| P2P (WebRTC) | `ExchangeSignals.transport_delay_version` on the active side's offer | The same field on the passive side's answer | The field is absent, so no header (§6.1). |
| H3 DNS packet translation | Covered by the H3 row. PT is a `PacketConn` under quic-go. | — | — |

**Switch point.** Both directions switch at a fixed boundary.

- H1: the first message after the 101. Header auth has no auth message.
- H3: the first message after the auth exchange. The auth frames are never
  stamped, and the DATAGRAM lane starts only after the exchange, so it is
  always stamped when negotiated.

### 6.1 P2P

P2P is client ↔ client over WebRTC. The window client sending to a stream
destination is the active side (`NewP2pConnActive`), and the provider is the
passive side (transport_p2p.go:767-777). Relays forward raw bytes between two
P2P transports (transfer_stream_manager.go:1557-1594), so the header is per
hop: each hop negotiates, stamps and strips on its own.

The passive provider end stamps and echoes when the active side offers, exactly
as the server does, but never publishes or consumes stats (D3). One PeerConnection
carries two lanes that share one transport clock and one echo.

- **Negotiation.** Add an optional field `transport_delay_version` to
  `ExchangeSignals` (protocol/transfer.proto:440-457), following
  `sender_generation_id` (field 4), which older peers ignore.
  - The active side sets it on its offer batch. The passive side sets it on its
    answer batch only if it accepts. Each side knows the other's answer before
    the data channel opens.
  - Since 2026-10-28 the active side offers on a new generation only while the
    latest answer of its client's edges to a delay header offer accepted it,
    and not before any edge has answered. The edges never see this exchange,
    so this is how their runtime switch (§11) reaches P2P.
  - Not through the "rdy" ready header: it is an exact match
    (transport_p2p.go:146), so an old peer would time out.
  - Not through `DataChannelInit.Protocol`: it only travels active to passive.
- **Switch point.** The first message after "rdy" in each direction. "rdy"
  itself is never stamped.
- **Legacy SCTP lane** (reliable, unordered).
  - A write blocks under SCTP BlockWrite until the previous message's chunks
    enter flight, which is min(cwnd, rwnd) admission
    (sctp/association.go:3936-3955). It returns before acknowledgment. That is
    a valid busy-gated throughput signal.
  - Each SCTP message is one physical write. Stamp inside `writeLegacy`
    (transport_p2p.go:1406-1433), which the legacy queue worker also calls.
    The two writers never overlap, so lane state needs only a mutex.
  - Delivery is unordered, so a remote-write pairing (§5.3) is accepted only
    when the closing frame's `sendStartMicros` exceeds the open group's by at
    least the reported duration. An occasional mispairing after loss is
    absorbed by the interval sums. If it proves noisy, the v2 fix is a one-byte
    write sequence.
- **Fast lane** (SRTP; unreliable, with no congestion control). It gives delay
  samples only. Writes block only on the 256-entry UDP batch queue and the
  kernel buffer.
  - The 12 bytes are the first bytes of the message inside the existing
    `'URD'` v2 fragment envelope (`msgLen` includes them), so the envelope
    version does not change and mixed peers keep the lane.
  - Fragment payload stays 1188 (transport_p2p_fast.go:20), so a frame within
    12 bytes of a fragment boundary takes one more fragment.
  - Stamp before `WriteFastPathMessage` (transport_p2p.go:1467). Strip in
    `runFast` (2083-2087) while copying out of the reassembler.
- **Classification.**
  - Stamp after `isP2pStreamProbe` and `messagePoolIsSmallUnordered`
    (transport_p2p.go:1452, 1522).
  - Strip before `messageHandler` (2041-2046) and in the ready-header prefetch
    loop (1987-1991). The 40-byte probe envelope is an exact-length check
    (transport_p2p_probe.go:742-744).
- **Size budgets.** P2P's 64 KiB message cap (transport_p2p.go:1446-1450) and
  the legacy queue's 8 KiB slab subtract the header.
- **JS builds.** They have no P2P data plane (transport_p2p_webrtc_pc_js.go:70-73),
  so there is nothing to stamp.

## 7. Framer API

The header length is fixed per framer and is part of the negotiated wire
format:

```go
type FramerSettings struct {
	// ...
	// HeaderByteCount is a fixed per-message header carried between the
	// length prefix and the payload. 0 is the legacy wire format.
	HeaderByteCount int
}

// Wire: [uint16 len(header)+len(message)][uint16 split][header][message].
func (self *Framer) WriteWithHeader(w io.Writer, header []byte, message []byte) error

// One physical write. The same header precedes every message: all frames of
// one write carry identical timing (§5.1).
func (self *Framer) WriteBatchWithHeaderStorage(w io.Writer, header []byte, messages [][]byte, storage []byte) error

// Returns a pooled payload without the header. header is written only after
// the complete frame was read without error.
func (self *Framer) ReadWithHeader(r io.Reader, header []byte) ([]byte, error)
```

At the call site the header is a stack array:
`var header [transportDelayHeaderByteCount]byte; framer.WriteWithHeader(w, header[:], message)`.
`len(header) != HeaderByteCount` is an error.

- **Pool ownership.** Never strip a header by subslicing a pooled buffer.
  `MessagePoolReturn` classifies by capacity (message_pool.go:1236), so a
  subslice would leave the pool. The header is read into the caller's array and
  the payload into its own pooled buffer.
- **FramerXl.** Connect transports never negotiate `urnetwork-framerxl/1`; it
  is device-rpc only (server agent finding; client dials `H1FramerProtocol`,
  net_http.go:2261). FramerXl gets the same three methods only if a carrier
  needs them.
- **WebSocket.** `ReadH1PooledMessageWithHeader(conn, limit, header)` reads the
  first 12 bytes of `NextReader()` into the header and the rest with
  `MessagePoolReadAllLimit`. Writes use `NextWriter` with two writes (header,
  then payload), so `WebSocketWriteBatchConn` still makes one physical flush
  per ready batch.
- **Classification after stripping.** Both H1 ends classify messages by length:
  0 ping, 5 control, 16 latency echo, other short messages dropped, 1024
  speed probe (client transport.go:3185-3232, server transport.go:1931-1955).
  That classification runs on the payload, after the header is stripped.
- **H3 DATAGRAM.** The fragmenter already copies each message into its
  envelope, so `SendHybridWithHeader` writes envelope + header + message in that
  one copy. `AcceptWithHeader(datagram, now, header)` skips the header while
  copying into the reassembled pooled message.

## 8. Per-connection measurement (connect)

A `transportDelay` exists per connection, or is nil when the header was not
negotiated. One implementation serves the client and the server, since the
server already uses connect's framing.

- **Writer side, per lane** (the lane's single writer goroutine, no lock):
  - `beginWrite() [12]byte` reads the clock and returns the header:
    `sendStartMicros`, the lane's `previousWriteMicros`, and the current echo.
  - `endWrite(byteCount, err)` feeds the interval accumulator. An error sets
    `previousWriteMicros` to 0.
  - Out-of-band writers stamp `previousWriteMicros = 0` and leave lane state
    alone (§11).
- **Reader side, per lane** (the lane's single reader goroutine):
  - `receive(header, byteCount, arrival, receiverLimited)` handles grouping
    (§5.3), the down `owdRaw`, the echoed up `owdRaw` (deduplicated by value),
    both base filters, and the interval minimum.
  - It also updates the echo the local writers will send back: one atomic
    uint32.
- **Interval accumulator:** a small `stateLock` struct touched once per write
  and once per received group. The first goroutine to see the interval end
  publishes one `TransportDelayStats` and starts the next interval. Nothing is
  published without activity.
- **Server:** the server stamps and echoes. It needs only the receive grouping,
  `owdRaw` and the receiver-limited flag, not the filters or publication.

```go
type TransportDelayStats struct {
	ClientId      Id
	ConnectionId  Id // one connection generation
	TransportType TransportType
	StartTime     time.Time
	EndTime       time.Time

	SendByteCount    ByteCount
	ReceiveByteCount ByteCount

	// Network-limited throughput (§4.1). 0 means no limited interval: the
	// carrier was not busy, idle, warming up, or cannot block.
	SendBitsPerSecond    int64
	ReceiveBitsPerSecond int64

	// Standing queue: the minimum within the interval of owdRaw - base (§4.2).
	SendQueueDelay         time.Duration
	SendQueueDelayValid    bool
	ReceiveQueueDelay      time.Duration
	ReceiveQueueDelayValid bool
}

type TransportDelayStatsFunction = func(stats *TransportDelayStats)
```

Settings, as one optional struct on `PlatformTransportSettings` (nil means do
not offer):

```go
type TransportDelaySettings struct {
	Version                  int           // 1
	Interval                 time.Duration // 1s
	BlockedWriteMinDuration  time.Duration // 250µs
	MinBusyFraction          float64       // 0.5
	Warmup                   time.Duration // 2s
	MinDelaySampleCount      int           // 3
	BaseHistoryCount         int           // 10
	BaseHistoryInterval      time.Duration // 1m
	ReceiverLimitedThreshold time.Duration // 5ms
}
```

## 9. Publication path

- **Route manager.** The client's `RouteManager` holds
  `transportDelayStatsCallbacks *CallbackList[TransportDelayStatsFunction]`.
  Every transport of a client, platform or P2P, already holds its client's
  route manager (transfer_route_manager.go:449), so no constructor signature
  changes.
- **Client.** `Client.AddTransportDelayStatsCallback(callback) func()` is the
  public entry, in the style of `AddReceiveCallback` (transfer.go:4366).
- **Multi client (D9).**
  - Right after `newMultiClientChannelClient` returns
    (ip_remote_multi_client.go:15571), the multi client subscribes itself:
    `client.AddTransportDelayStatsCallback(...)`.
  - It passes each sample unchanged to its own list, which is exposed as
    `RemoteUserNatMultiClient.AddTransportDelayStatsCallback`. That follows the
    `AddPacketStatsCallback` pattern (ip_remote_multi_client.go:7291).
  - The per-window fan-out pattern (`AddContractStatusCallback`,
    ip_remote_multi_client.go:2908) subscribes only the windows present at
    subscription time and would miss clients created later, so it is not used.
  - Unsubscribing when the channel closes is hygiene only: the client's list
    dies with the client.
- **Device.** DeviceLocal subscribes to each multi client it creates in
  `applyDestination` and drops the subscription on teardown (§10.4). The
  provider client is not subscribed (D3).
- **Threading.** Callbacks run on the transport goroutine that closed the
  interval and must not block. DeviceLocal takes a short lock, updates its
  window, and hands any listener notification to its own dispatch.

## 10. Device (sdk)

### 10.1 Window

The thresholds, the window statistic and the aggregation below come from the
threshold research, `~/urnetwork/reports/Transport delay quality thresholds.md`
(2026-10-27). The user accepted them.

- Keep the live stats of every connection, stamped with their `EndTime`.
  Ignore samples older than M (default `TransportDelayWindowTimeout` = 20 s),
  and forget a connection with no live sample.
- **Combine per second (D5).** Bucket the live samples by second. Per
  direction and second:
  - **Throughput** is the sum of the rates of the connections that were
    network-limited in that second, plus the byte rate of the other connections
    active in that second. The multi client runs 7–10 window transports over
    one access link, so one connection's rate is only its share of the link:
    six busy connections on a healthy 8 Mbit/s link would each read about
    1.33 Mbit/s. A second in which no connection was network-limited has no
    throughput sample.
  - **Queue delay** is the second-highest valid `QueueDelay` when two or more
    connections report one in that second, and the single value when only one
    does. A standing queue on the user's link shows on every connection, while
    a stalled server edge or a slow P2P peer shows on only one (§14.1).
  - **Fallback** is set when DNS-carried transports (`TransportTypeH3Dns`,
    `TransportTypeH3DnsPump`) carried most of that second's bytes (D10).
- **Score the second-worst sampled second (D11)** among the last N sampled
  seconds within M (default `TransportDelayWindowCount` = 10): the
  second-lowest throughput and the second-highest delay. With fewer than two
  sampled seconds a signal has no score. The payload's rates and delays report
  the same statistic, so the numbers shown match the level, and `Fallback`
  comes from the second that set the reported level.
- Re-evaluate on every sample, and on a 1 s tick while any sample is live, so a
  direction returns to 0 without new traffic (D4).

A sustained degradation appears about 2 s after it starts: the second-worst
needs two bad seconds. It clears once fewer than two of the last N sampled
seconds are bad, or after M (20 s) once traffic stops. In the research's
simulation, scoring the second-worst instead of the worst took notifications
caused by scattered single bad seconds from 1.15 to 0 per hour at a 1% bad-second
rate, and left long real slowdowns almost unchanged.

### 10.2 Quality

```
q_throughput(t) = 0                                  if no score, or t >= good
                = 1                                  if t <= bad
                = ln(good / t) / ln(good / bad)      otherwise   // ratio scale

q_delay(d)      = 0                                  if no score, or d <= good
                = 1                                  if d >= bad
                = (d − good) / (bad − good)          otherwise

q = max(q_throughput, q_delay)                       per direction (D1)
```

Default thresholds, calibrated by the research (the report's sources are
summarized below):

| Setting | Up | Down |
|---|---|---|
| Throughput where q reaches 0 (at or above) | 750 kbit/s | 1.5 Mbit/s |
| Throughput where q reaches 1 (at or below) | 175 kbit/s | 350 kbit/s |
| Standing queue where q reaches 0 (at or below) | 500 ms | 500 ms |
| Standing queue where q reaches 1 (at or above) | 2.5 s | 2.5 s |

The queue thresholds are separate settings per direction with the same
defaults, because the uplink is likelier to need a higher floor.

Because any non-zero q rounds up to at least the first step, the q = 0 points
alone decide whether a healthy network reads 0, and level 1 begins where q
passes 0.75. The resulting level edges are:

| Level | Down throughput | Up throughput | Standing queue, either direction |
|---|---|---|---|
| 0 | ≥ 1.5 Mbit/s, or no busy sample | ≥ 750 kbit/s, or no busy sample | ≤ 500 ms |
| 0.25 | 1.04–1.5 Mbit/s | 521–750 kbit/s | 0.5–1.0 s |
| 0.5 | 725 kbit/s–1.04 Mbit/s | 362–521 kbit/s | 1.0–1.5 s |
| 0.75 | 504–725 kbit/s | 252–362 kbit/s | 1.5–2.0 s |
| 1 | < 504 kbit/s | < 252 kbit/s | > 2.0 s |

Anchors:

- **The 500 ms queue floor.** Ordinary 5G and LTE queue hundreds of
  milliseconds when the user saturates their own link. Ookla's 2025 22-market
  data puts 5G loaded latency at 288–960 ms market medians, and a saturated
  8 Mbit/s LTE uplink measured about 330 ms of round trip against a 50 ms
  network RTT (Guo et al., IMC 2016). The original 200 ms floor (from the IETF
  responsiveness draft's "Poor" line) would have read normal cellular as slow
  during every download. Anything above 500 ms is already past ITU-T G.114's
  400 ms one-way planning limit once base delay is added, so level 0.25 never
  overstates the harm.
- **The 1.5 Mbit/s / 750 kbit/s zero points.** These are Opensignal's mobile
  sufficiency bar and sit about ten times below healthy cellular p10. The level
  bands then bracket app needs: HD calls are marginal in the 0.25 band, YouTube's
  480p and 360p floors fall in the 0.5–0.75 bands, and below 252 kbit/s up a link
  falls under Google Meet's floor for group-call video.
- **One bar reads 1.** Android's one bar is LTE RSRP −128 to −118 dBm. A
  saturated cell-edge link delivers hundreds of kilobits behind seconds of
  queue, because byte-sized cellular buffers grow delay as the rate falls (about
  1.3 s at 2 Mbit/s). Apple's Network Link Conditioner Edge preset reads 1, and
  its Wi-Fi and LTE presets read 0.
- **No direct one-bar data exists.** No public dataset bins throughput or loaded
  latency below −115 dBm, so these are evidence-weighted starting points for the
  shadow trial in §13.
- **Busy samples only.** A throughput sample exists only when the transport
  limited a busy writer, so an idle one-bar phone reads 0 by design (D4).

### 10.3 Quantization and hysteresis

- The step is s (default `TransportDelayQualityStep` = 0.25). The level is 0 if
  q == 0, and otherwise `ceil(q / s) · s`. So any non-zero quality is at least
  s, and the zero boundary is exact (D7).
- **Attack is immediate at every level.** A level rises as soon as q enters a
  higher band, including the first step out of 0.
- **Release has hysteresis.** With h (default 0.10), a level falls only once q
  drops at least h below its band. The narrower calibrated span needs the larger
  h: 0.10 is about a 15% throughput move, matching Android's bandwidth estimator
  (it reports on a 15% change). Applying h only on release keeps every
  calibrated boundary independent of the path the reading took.
- The listener fires when either direction's level changes, or when
  `Fallback` changes (so the copy can switch). Throughput and delay values ride
  along in the payload but do not fire it on their own.

### 10.4 API

Two precedents apply:

- **Payload and layering:** `ClientLimitStatus` (sdk/client_limit_status.go,
  added in sdk commits `35402994` sdk+rpc, `ddc43f6a` cgo+packaging, `9d80f29e`
  js). It is a never-nil, plain-field struct threaded through every layer with
  no `DeviceRpcVersion` bump.
- **Per-multi-client subscription:** `WindowStatus` (device_local.go:5027-5059
  and 5107-5128). The multi client is rebuilt on every destination change, so
  the subscription is per generation.

```go
// sdk/transport_delay.go

// TransportDelay is the device's current transport quality. Plain float64 and
// int64 fields so gomobile, cgo (JSON with Go field names as keys) and gob
// carry it as is.
type TransportDelay struct {
	// 0 (normal) to 1 (worst), quantized to TransportDelayQualityStep (§10.3)
	UpQuality   float64
	DownQuality float64
	// the worst network-limited throughput in the window. 0: none (§10.1)
	UpBitsPerSecond   int64
	DownBitsPerSecond int64
	// the worst standing queue in the window. 0: none
	UpQueueDelayMillis   int64
	DownQueueDelayMillis int64
	// the worst reading came from a DNS-carried fallback transport: the apps
	// say URnetwork is using a slower fallback connection (D10)
	Fallback bool
}

type TransportDelayChangeListener interface {
	TransportDelayChanged(delay *TransportDelay)
}

// Device
GetTransportDelay() *TransportDelay // never nil
AddTransportDelayChangeListener(listener TransportDelayChangeListener) Sub
```

DeviceLocal:

- **Storage.** `transportDelayChangeListeners
  *connect.CallbackList[TransportDelayChangeListener]`, and
  `transportDelayStatsSub func()` next to `windowMonitorSub`
  (device_local.go:947).
- **Subscription.** In `applyDestination`, next to the other multi-client
  subscriptions (device_local.go:5008-5088), call
  `multi.AddTransportDelayStatsCallback(...)`. The callback runs on a transport
  goroutine (§9). It checks the generation fence captured in its closure
  (`connectionGeneration` and the multi client's identity), appends to a
  `transportDelayWindow` guarded by that struct's own `stateLock` (not
  DeviceLocal's), and signals a one-slot notify channel.
- **Worker.** A lifecycle worker, `watchTransportDelay`, recomputes on each
  notify and on a 1 s tick while samples are live. It is joined through
  `lifecycleWorkers` and exits on `self.ctx`, like `watchClientLimitStatus`
  (device_local_client_limit.go:53-67). It applies quantization and hysteresis,
  then fires listeners outside any lock, with a clone per listener and each
  call wrapped in `connect.HandleError` (device_local_client_limit.go:41-47).
  Listeners never run on the transport goroutine, because DeviceLocalRpc's
  listener is subject to reverse-rpc back pressure.
- **Teardown.** Unsubscribe at the destination-change teardown
  (device_local.go:4715-4732) and in close (5740-5747). A rebuild drops the old
  generation's samples and recomputes, emitting if the level changed; this is
  the reset closure that runs after unlock (5107-5128). With no multi client
  (`remoteUserNatClient == nil`) the value is the zero struct.

Settings live in `DeviceLocalSettings` (device_local.go:551-775), with defaults
in `defaultDeviceLocalSettingsWithProfile` (369-460). Durations follow the
`//gomobile:noexport` plus `Get/Set…Millis` accessor convention (478-529) and
cross the C ABI as JSON nanoseconds. Settings are not sent over the rpc, so
they are set wherever the DeviceLocal is constructed (the network extension or
the service).

| Field | Default |
|---|---|
| `EnableTransportDelay bool` | true: offer the header on window transports and measure |
| `TransportDelayWindowCount int` (N) | 10 |
| `TransportDelayWindowTimeout time.Duration` (M) | 20 s |
| `TransportDelayGoodUpBitsPerSecond int64` | 750,000 |
| `TransportDelayBadUpBitsPerSecond int64` | 175,000 |
| `TransportDelayGoodDownBitsPerSecond int64` | 1,500,000 |
| `TransportDelayBadDownBitsPerSecond int64` | 350,000 |
| `TransportDelayGoodUpQueueDelay time.Duration`, `TransportDelayGoodDownQueueDelay time.Duration` | 500 ms |
| `TransportDelayBadUpQueueDelay time.Duration`, `TransportDelayBadDownQueueDelay time.Duration` | 2.5 s |
| `TransportDelayQualityStep float64` | 0.25 |
| `TransportDelayQualityHysteresis float64` | 0.10, release only |

`EnableTransportDelay` reaches connect through the generator's window transport
settings (`PlatformTransportSettings.TransportDelay`, §8). Measurement stays on
during the calibration trial (§13). The apps decide when to show notifications.

Naming: the API avoids "degraded". `TransportStatus.AutoDegraded`
(transport_status.go) is a memory constraint, and `SetPerformanceDegraded` is an
app input. Nothing in the sdk reports transport quality out today; this is the
first such output.

### 10.5 DeviceRemote, rpc, cgo and js

**DeviceRemote** (device_rpc.go), following ClientLimitStatus:

- A listener map and a cache, `lastTransportDelay`.
- **Add** goes through `addCompatibleListener` (2432-2453) to
  `DeviceLocalRpc.AddTransportDelayChangeListener` with
  `rpcCallVoidAllowMissingMethod`. With no live service it calls `Sync()`, so
  the id rides the next sync.
- **Get** reads through with `rpcCallNoArgAllowMissingMethod`, but follows the
  ContractStatus getter (1913-1940, 4536-4549): it does not hold stateLock
  during the call, and a revision counter stops a slow read from overwriting a
  newer push. That suits a value the notification may read often. It falls back
  to the cache, then the zero value, and is never nil.
- **Fire** updates the cache, snapshots the listeners under the lock, and calls
  them outside it, each with a clone.

**Wire.** No `DeviceRpcVersion` bump is needed: the listener is compatible, the
getter is AllowMissingMethod, and the sync field is optional. ClientLimitStatus
shipped the same way.

- `DeviceRemoteSyncRequest.TransportDelayChangeListenerIds []connect.Id`, next
  to 7622.
- DeviceLocalRpc entries mirroring 9505, 9557, 9881, 10528 and 10715: the ids
  map, the Sub, removal in closeService, re-adding in Sync, and level replay in
  SyncReverse.
- `AddTransportDelayChangeListener` and `RemoveTransportDelayChangeListener`
  subscribe to DeviceLocal on the first remote id and unsubscribe after the
  last. Plus `GetTransportDelay`.
- **Push path:**
  1. `DeviceLocalRpc.TransportDelayChanged`
  2. `reverseNotify("DeviceRemoteRpc.TransportDelayChanged", delay)`.
     `enqueueReverse` coalesces by method name with the last value winning,
     which is right for a level.
  3. `DeviceRemoteRpc.TransportDelayChanged`, then `dispatch`, then
     `deviceRemote.transportDelayChanged`.

  Never push nil (gob cannot encode it). The plain struct crosses as is, with
  no `*Rpc` mirror.

**Remote availability.** This follows the device-remote stat precedent: while
`GetRemoteConnected()` is false, the cached value is stale. The app hides the
quality notification on `remoteChanged(false)` instead of keeping the last
level up. A "poor connection" notification left showing while the network
extension isn't running would be wrong. No synthetic reset is pushed, which
matches the existing listeners: only `remoteChanged` and `tunnelChanged` fire
on disconnect.

**cgo.** No gen.go list changes are needed. Classification is automatic: an
exported struct crosses as JSON with Go field names as keys, and a listener
becomes a C callback (gen.go:520-558). Regenerate with `make -C cgo generate`
(`generated_currentness_test.go` fails otherwise), then
`go -C packaging run . generate` for the raw bindings. `urnet_abi_version` is
unchanged for additive exports. Optionally pin the names in
`cgo/gen/testdata/exported_symbols.txt`.

**gomobile.** The struct and the one-method listener bind as is, and
`build/cmd/mobileexports` (main.go:71-136) catches any omission. Avoid NSObject
names such as `description` and `hash` for fields.

**js.** Not built. There is no web consumer (apple/DELAY.md §10): the hosted
device's transports run between server machines, and hosted devices don't
measure.

## 11. Server

Server code: `server/connect/transport.go` `ConnectHandler`. H1 WebSocket and
H1+ are in `Connect()` (1356-2192). H3, H3 DNS and DNS pump share
`connectQuicWithCarrier` (2258-3220). The alt front reaches the same code
through `HandleQuicConn` (2365, server/alt/alt.go:444-474). Every framing
helper comes from connect (`replace github.com/urnetwork/connect => ../connect`).

- **Where headers live.** The edge stamps, strips and echoes. The resident only
  sees exchange frames (resident.go) and needs no change. If the resident ever
  wants the timings, they go in `ExchangeHeader` (gob, zero for older peers).
- **H1 writers.**
  - The user-data writer batches up to 32 messages / 12 KiB through
    `WebSocketWriteBatchConn` (WebSocket) or `FramedMessageConn.WriteMessages`
    (H1+). Each batch is one physical write with one header.
  - Control writes go through `write()` (2004-2021) and are single physical
    writes.
  - The out-of-band writers also stamp, with `previousWriteMicros = 0`:
    the client-limit close (1831), the authorization close (`writeTypedClose`
    1673), the moved close (transport_moved_close.go), and the refusal-hold
    keepalive (transport_refusal_hold.go:255).
  - WebSocket close frames are not binary messages and carry no header.
- **H1 reader** (1901-1988). Strip the header before the ≤16-byte control
  classification. The 1024-byte speed-probe check, `pingTracker` and
  `announce` byte counts all run on the payload.
- **H3 writers.** The stream writer batches up to 16 messages / 64 KiB through
  `WriteBatchWithStorage`; the heartbeat is a one-message batch. The hybrid
  dispatcher sends DATAGRAMs inline. That gives two lanes on two goroutines.
- **H3 readers.** The stream reader and the DATAGRAM pump, each a lane.
- **Negotiation.** See §6. The server knows provide intent before workers
  start (provider_intent.go:119-122).
- **nginx.** H1 TLS terminates at the nginx load balancer
  (warp/warpctl/config.go:2186-2220, buffering off), so the edge's H1 writes go
  to nginx. Backpressure reaches the edge only after the client-side socket,
  nginx's proxy buffer and the edge-side sockets fill. H1 down throughput is
  therefore a weak signal, and the H1 down direction relies on delay (§4.3).
  H3 and DNS pass through an nginx UDP L4 proxy (PROXY v2), and the edge
  terminates QUIC, so H3 write blocking does track the client path.
- **DNS pump.** Server-to-client packets wait for the client's next pump
  request (about 100 per second), which adds up to about 10 ms of one-way
  delay. The base filter absorbs the steady part.
- **Existing measurements are not replaced.** The 12–24 h latency test and the
  speed test (H1, `X-UR-TransportVersion` ≥ 2, public providers only;
  transport_announce.go) and the passive 5 s speed windows keep working on the
  stripped payloads.
- **Rollout.** Ship the server first (it accepts offers). Clients offer from
  the next release. Old clients never offer, and old servers never accept.
- **Runtime switch.** The field `transportDelay` of the Redis move control
  turns acceptance off on every edge, connect and alt, without a deploy:
  `redis-cli -c HSET '{connect_move}control' transportDelay off`. Turn it back
  on with `redis-cli -c HDEL '{connect_move}control' transportDelay`. The
  field only narrows `EnableTransportDelay`, so absent is the code default,
  on; a value that does not parse is off, and deleting the whole control also
  turns acceptance back on. Each handler reads the control every 10 s, so new
  connections follow a change within about 10 s, and none waits on Redis. A
  connection keeps what its 101 or auth response negotiated until it
  reconnects. A failed read keeps the last value, and a handler that has not
  read yet uses the code default. `urnetwork_connect_transport_delay_enabled`
  shows each process's value. P2P is negotiated between the clients (§6.1),
  and the edge only forwards their signals, so P2P follows the edges through
  the client: a new active P2P generation offers only while the latest answer
  an edge gave one of the client's own connections accepted the header. The
  switch reaches a client's new P2P generations once one of its edge
  connections negotiates after the change, and a P2P generation keeps what it
  negotiated.

## 12. Implementation map

| Repo | Change |
|---|---|
| connect | `transport_delay.go` (header, clock, lanes, filters, stats, settings); `message_framer.go` (§7); `framed_message_conn.go` and WebSocket helpers (header read/write); `http_upgrade.go` (extra 101 header, surface response headers through `DialerInfo`); `transport_h3_datagram.go` (header in the envelope copy, limits); `protocol/transfer.proto` (Auth fields 11, 12); `transport.go` (H1/H3 lanes, offer, censoring); `transport_p2p.go` (lanes, stamp and strip points), `transport_p2p_fast*.go` (header inside the fragment message), `transport_p2p_webrtc.go` and `protocol/transfer.proto` (`ExchangeSignals.transport_delay_version`), per §6.1; `transfer_route_manager.go` and `transfer.go` (callback list, `Client.AddTransportDelayStatsCallback`); `ip_remote_multi_client*.go` (subscribe at window client creation, pass-through list, offer on window transports). |
| server | `connect/transport.go` (accept, stamp, strip, echo, lanes), `alt/alt.go` (unchanged path through `HandleQuicConn`). |
| sdk | `transport_delay.go` (payload, listener, window, quality); `device.go` (Get/Add); `device_local.go` (settings, `applyDestination` subscription, worker); `device_rpc.go` (§10.5); regenerated cgo and packaging bindings. No JS bridge (§10.5). |
| apps | Apple, Android, Windows and Linux (apple/DELAY.md). Every surface hides while the remote is disconnected (§10.5). |

## 13. Tests and calibration

- **Framer.** Header round trip including the split path and batches; oversize
  frames; header-only and partial-payload reads (error, header untouched); pool
  capacity preserved (`allocation_count_test.go` style).
- **WebSocket.** The header path through `NextReader`/`NextWriter` with
  `WebSocketWriteBatchConn` still making one flush per batch.
- **Delay math.**
  - The uint32 wrap at the 71.6-minute boundary.
  - Strictly distinct `sendStartMicros`, batch grouping and the one-write
    pairing.
  - Echo deduplication.
  - Base-history aging, drift injection (±100 ppm), and a simulated suspend (an
    offset jump on one side, with and without the sleep-inclusive clock).
  - Receiver-limited censoring and the busy gate (a GC-like 30 ms stall
    produces no throughput sample).
- **sdk.** Window expiry, the worst-of-all aggregation, quantization and
  hysteresis (no event on a sub-step change), and the reset on rebuild. A
  generation fence drops late samples from a retired multi client. Remote
  delivery uses the ClientLimitStatus harness
  (`testExtenderStatusSyncedDeviceLocalRemoteWithSettings`,
  `testWaitRemoteDisconnected`, client_limit_status_test.go:420-462): the
  remote receives the change, the cache survives a disconnect, and reconnect
  replays the level.
- **Negotiation matrix.** New/new and new/old in both directions, for H1
  WebSocket, H1+ and H3 (datagrams on and off). With provide intent declared
  the header is never negotiated. Old peers keep working byte-for-byte.
- **Integration with emulated networks.** These follow the research report's
  lab plan.
  - Negative controls must read 0: the Wi-Fi and LTE link-conditioner presets,
    and high-rate shallow-buffer `tc netem` links under bulk load.
  - Positive controls must read 1: Edge- and 2G-class profiles, and
    rate-limited deep FIFOs. Size the FIFO as netem
    `limit` = rate × buffer time ÷ 12,000 bits; 208 packets builds 2.5 s at
    1 Mbit/s.
  - Fixed added delay never exercises the delay signal, because the base
    subtracts it.
  - Probe every level edge from both sides (§10.2), and the release edges at
    h = 0.10.
  - Four specific cases:
    - six bulk flows over one 8 Mbit/s bottleneck must read 0 once throughput
      is summed;
    - netem delay stepped from 30 to 300 ms mid-connection must read 0;
    - a 1–3 s stall on one of K server edges must not raise the level;
    - a fitted one-bar emulator (Pantheon-style five parameters, or Mahimahi
      cellular traces) must reach 1.
- **Shadow trial before notifications.** Ship the measurement with
  notifications off, as a dark launch to 1%, then 10%, then 100%.
  - The device scores three to five candidate parameter sets in parallel
    against the same samples: the current defaults, tighter and looser zero
    points, worst and second-worst, and two or three dwell variants.
  - It uploads daily, pre-aggregated counters per stratum with no identifiers.
    The strata are platform, network type, RAT, signal bin, transport type with
    `Fallback`, direction, live-connection count and a mobility flag.
  - The counters are histograms, exceedance and run-length counts at candidate
    thresholds, would-be notifications, and connected and sampled seconds.
  - Labels come from Android's raw RSRP through one fixed table (AOSP
    −128/−118/−108/−98 dBm), never the bars icon. NR standalone one bar gets its
    own stratum. iOS has no signal API, so it can only confirm the healthy
    false-positive rate.
  - Choose operating points Neyman–Pearson style: maximize one-bar detection
    under the notification budget.
- **Budgets.** Healthy networks get roughly one false notification per
  user-week, about 0.02 per connected hour. The in-app pill shows less than 1%
  of connected time on healthy networks, and the median busy one-bar window
  reads 1. The SDK-level transition rate is a diagnostic, not a gate.
- **Stratify by transport.** `sendStartMicros` is stamped before bytes enter
  the socket, so the tunnel's own send buffers (and the edge and nginx buffers
  on H1) count toward the up queue. If H1 reads systematically hotter than H3,
  cap unsent bytes at the source rather than loosening the floor.
- **Overhead.** H1+ and H3 throughput benchmarks with the header on and off
  (`contention_bench_test.go`), allowing at most a 1% CPU regression.

## 14. Risks and open items

1. **Server stalls read as a bad connection.** Flow-control blocking and the
   echo's arrival time both include server-side stalls. Mitigated by the
   per-second aggregation (§10.1): delay takes the second-highest connection,
   and summed throughput absorbs one slow edge, so one stalled edge among
   several connections does not raise the level. The server also censors its
   echo when its reader was backpressured. A single-connection session can
   still read a server stall.
2. **Cellular RAN buffering.** Delay-gradient detectors misfire on RAN jitter
   (arXiv 2507.00337). This design uses the *level* of a standing queue (an
   interval minimum), not a gradient, with thresholds in the hundreds of ms.
   Calibrate on cellular before enabling notifications.
3. **Server socket buffers and nginx can hold seconds of data** while write
   blocking looks fine. The delay signal covers this, which is the main reason
   for D1.
4. **A connection that starts congested** has too high a base until its queue
   drains once. Reconnects reset the base.
5. **DNS-carried H3 is slow by nature: decided 2026-10-27 (D10).** While it is
   the active mode, the score will usually be non-zero. The indicator stays,
   because the experience really is slow, and `TransportDelay.Fallback` switches
   the copy to "using a slower fallback connection" so it doesn't blame the
   user's network.
6. **Header overhead on small frames.** Measure it (§5.4).
7. **Windows sleep-inclusive clock.** Verify (§5.2).
8. **Provide-mode ordinal comparison: fixed 2026-10-27.**
   `provideModeIncludesPublic` (sdk/device_local_provider.go) used
   `ProvideModePublic <= provideMode`, which transfer.proto:337-342 forbids.
   A device in `ProvideModeStream` provides nothing publicly
   (`setProvideModeWithLock` maps only `ProvideModePublic` to public
   contracts), yet it declared provide intent and dialed direct without
   extenders. It now tests `== ProvideModePublic`, and
   `TestProvideModeIncludesPublic` expects stream to be false.
   `TestDeviceLocalProviderPublicToStreamRebuildsWithoutProvideIntent` pins that
   public → stream now rebuilds on the shared strategy and drops provide intent,
   and `TestDeviceLocalProviderProvideModeWithoutFlipDoesNotRebuild` uses
   network → stream instead. The provider, provide-intent and client-limit tests
   pass.

9. **Hosted devices (web) measure the wrong link.** A platform-hosted device's
   connect transports run from the proxy host to a connect edge inside the
   datacenter, not over the user's own internet. Hosted devices therefore set
   `EnableTransportDelay = false` (server/proxy/proxy_device.go). Decided
   2026-10-27: nothing is built for the web, because the connection that matters
   there is on our server machines (apple/DELAY.md §10).

## 15. As built (2026-10-27)

§6–§11 are implemented in connect, server and sdk, with the additions and
deviations below.

**Core (`transport_delay.go`).** The H3 DATAGRAM lane and the P2P fast lane use
`newUnreliableWriteLane()`. Their writes count bytes, which feed the device's
byte-rate term, but never blocked time. A lossy lane's write queues or drops
instead of waiting on congestion control (§4.1). This also keeps the busy
fraction at or below 1 when the stream and DATAGRAM lanes block at once.

**H1** (`transport_delay_h1.go`, `runH1`, server `Connect`).

- **WebSocket wrapper.** WebSocket conns are wrapped in
  `transportDelayWebSocketConn`.
  - It stamps single writes, and each ready batch through the
    `WebSocketWriteBatchConn` observer.
  - It strips in `ReadMessage`, `NextReader` and `ReadH1PooledMessage`.
  - H1+ stamps inside `FramedMessageConn`.
- **Read limit.** The wrapper's `SetReadLimit` adds the 12 header bytes to
  gorilla's message limit, so callers keep setting payload limits. Both ends
  re-apply their limit after wrapping.
- **Close frames.** `H1WebSocketConn(conn)` unwraps to the gorilla conn, so
  close frames still go out. The server's client-limit, authorization and moved
  closes use it; their concrete type check missed the wrapper.
- **H1+ accept.** `AcceptFramedUpgradeWithHeader` records the selected
  subprotocol on the server's own `httpUpgradeConn`. Without it, H1+ accept
  never switched to the header while the client did.
- **Server.**
  - `EnableTransportDelay` (default true) gates accept on H1 and H3. Since
    2026-10-28 the move control's `transportDelay` can turn it off at runtime
    (§11, "Runtime switch"; server `connect/transport_delay_control.go`).
  - The peer conn gets a zero client id, because the upgrade precedes auth, and
    it publishes nothing.
  - It censors by timing `sendReceivedMessage`.
- **Client censoring.** The client's reader notes the gap between the end of
  one read and the start of the next, not each `offerReceive` call. A source
  audit pins the exact `offerReceive` call count.
- **Known gaps.**
  - The 12 KiB batch drain limit ignores the header (at most 384 bytes per
    batch), so a full batch can overflow the 16 KiB coalescer and flush part
    early. One stamp still covers the whole batch.
  - nginx is configured to forward `Sec-WebSocket-Protocol` but untested live.

**H3** (`transport_delay_h3.go`, `transport_h3_datagram.go`, `runH3`, server
`connectQuicWithCarrier`).

- **State.** `TransportDelayH3` holds one ordered stream lane and one unreliable
  DATAGRAM lane. The server constructs it with
  `NewTransportDelayH3Peer(clientId, framerSettings)`.
- **Negotiation.**
  - An accepted version the client did not offer is a protocol error; the
    client would otherwise receive headers it never strips.
  - The server re-encodes its Auth response whenever it accepts DATAGRAM or the
    delay header (`writeConnectQuicAuthResponse`). Old clients still get the
    byte-identical echo.
- **DATAGRAM.**
  - The header sits right after the 28-byte envelope header, in the same copy.
  - It counts in the envelope's total length, offsets and checksum, and is
    skipped during reassembly.
  - The largest DATAGRAM message drops from 1332 to 1320 bytes
    (`H3DatagramTransferFrameByteLimitWithHeader`).
- **Stream.** Batch sizing counts 16 bytes per frame inside the unchanged 64 KiB
  storage. The resident's unreliable-frame limit is 12 smaller when negotiated.

**P2P** (`transport_delay_p2p.go`).

- **Negotiation.**
  - Once a side has offered (active) or accepted (passive), every outbound
    signal batch carries `transport_delay_version`.
  - Each side stores the peer's version from the first offer or answer batch it
    applies, before applying the description, so the data channel cannot open
    before the outcome is known.
  - The active side enables the header only if the answer carried exactly 1.
    The passive side enables it when the offer carried 1 or more, and answers
    1.
- **Edge gate (2026-10-28).** A new active generation offers only while the
  client's edges accept the header (§6.1, §11).
  - Every platform connection that offered, on H1, H1+ or any H3 carrier,
    stores the edge's answer (its 101 or auth response) on the client's
    `RouteManager`, which all of the client's transports share
    (`noteTransportDelayEdgeAnswer`). The latest answer wins. A connection that
    did not offer stores nothing.
  - `P2pTransport.run` reads the gate through `Client.transportDelayP2pOffer`
    once per generation. Before any answer, it offers nothing.
  - The passive side is unchanged.
  - Tests: `TestTransportDelayP2pOfferFollowsTheEdges`,
    `TestTransportDelayP2pEdgeDeclineStopsOnlyNewGenerations` and
    `TestTransportDelayP2pTransportOffersNothingBeforeAnEdgeAccepts`. The H1
    and H3 platform transport tests check the stored answer, including an old
    server's decline.
- **State.** Delay state is per connection generation, created right after
  "rdy". Frames prefetched during the ready exchange are stripped but excluded
  from measurement.
- **Legacy SCTP lane.**
  - The header and message share one pooled copy, stamped after the copy.
  - The 64 KiB cap and the queue's 8 KiB slab admission both drop by 12. The
    slab limit keeps the stamped copy in the 8 KiB pool class.
- **Fast lane.** The header rides inside the `'URD'` envelope without joining
  buffers. The fragment payload stays 1188.
- **Strip.** Stripping moves the payload over the header in place, so the
  buffer keeps its pool class (§7).
- **Passive side.** It has no settings and no publisher. The fast-lane handoff
  never waits, so it records no censoring.
- **Relays.** The header is per hop. In production only the source → relay hop
  negotiates, because providers don't offer.

**Multi client (§9).** The pass-through `transportDelayStats` is a
`newMultiClientWindow` argument, set before the window's goroutines start.
Each window client subscribes in `expand()` once its channel exists. Assigning
it after construction raced the first `expand()` and lost early clients'
stats.

**sdk (§10).**

- **Window additions.**
  - Samples age out on both the monotonic and the wall clock, so a phone sleep
    cannot hold a stale level.
  - The window keeps at most 2048 samples.
  - A 1 s re-tick runs while any sample is live.
  - Fewer than two sampled seconds score nothing.
  - The payload's numbers are the scored statistic.
- **Bucketing.** Seconds are bucketed by the wall-clock second of each sample's
  `EndTime`. A connection counts once per second, and its later sample wins.
- **Remote.** The ClientLimitStatus pattern, with no `DeviceRpcVersion` bump.
  Sync and SyncReverse replay the level.
- **Hosted devices.** `EnableTransportDelay = false`
  (server/proxy/proxy_device.go, with a test).
- **Known limits.**
  - The newest one or two seconds can be incomplete when scored, because
    connections report late. Second-worst absorbs one of them.
  - A backward wall-clock step can misorder seconds for up to M = 20 s.
  - The remote cache is not cleared on a downgrade to an older device process.

**Tests added.**

- connect: `transport_delay_test.go`, `message_framer_header_test.go`,
  `transport_delay_h1_test.go`, `transport_delay_h3_test.go`, and
  `transport_delay_p2p_test.go` (real WebRTC pairs).
- server: `connect/transport_delay_h1_test.go`, `connect/transport_h3_delay_test.go`
  (DB-backed, end to end over a real edge), and
  `proxy/proxy_device_transport_delay_test.go`.
- server, older h1+ clients (2026-10-28). A client from before the header
  never offers it, so it must get the legacy wire whatever the server's
  setting.
  - Hermetic, no database: `TestConnectH1TransportDelayResponseHeader` covers
    the handler's acceptance, now `connectH1TransportDelayResponseHeader`.
    `TestConnectH1PlusLegacyClientGetsNoDelayHeader` drives the handler's h1+
    steps to a legacy framed reader, which must get every message byte for
    byte, with an offering client as the control. Each fails if acceptance
    ignores the offer, or if the server's conn follows the request's offer
    instead of its own 101.
  - DB-backed: a declined client must see no subprotocol and several empty
    heartbeats. The exchange test gains an h1+ client that never offers,
    trading frames with one that does.
- **DB-backed runs (2026-10-28).** The server delay tests pass, and so do
  the 29 H1 regression tests. One of them,
  `TestConnectUpgradeNilFramedConstructorFailureClosesOwnedSocket`, had
  failed since 2026-10-22, independent of this work. Since the resident move
  primitives (`6f3edd764`), the handler registers a connection before the h1+
  upgrade, and the test's bare `Exchange` had no connections map. The fixture
  now creates one.
- sdk: `transport_delay_test.go`, plus the public → stream provider test
  (§14 item 8).

**Not verified.**

- Real lossy or slow networks: the §13 lab work and shadow trial.
- Interop with old peer binaries. Tests simulate old peers by stripping fields.
- nginx, live.

## 16. References

- RFC 6817, LEDBAT: base delay from per-minute minima, BASE_HISTORY, TARGET.
- RFC 8289, CoDel: the minimum sojourn time over an interval as the standing
  queue.
- draft-ietf-rmcat-gcc: delay variation between packet groups, and overuse
  detection.
- draft-ietf-ippm-responsiveness-07: working-latency bands (Poor above 200 ms /
  300 RPM, Good below 60 ms / 1000 RPM, Excellent below 10 ms / 6000 RPM).
- draft-cheng-iccrg-delivery-rate-estimation (BBR): app-limited samples.
- RFC 1982: serial number arithmetic.
- quic-go v0.61.0 `send_stream.go`: Write returns once all but one packet of
  data is packed.
- Apple `clock_gettime(3)`: `CLOCK_MONOTONIC` continues during sleep;
  `CLOCK_UPTIME_RAW` does not.
- arXiv 2507.00337: RAN buffer effects on delay-sensitive protocols.
