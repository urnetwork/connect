# P2PGIG — a path to gigabit P2P throughput

- Status: engineering research and design recommendation, not a throughput claim
- Research snapshot: 2026-08-09
- Target: sustained 1 Gbit/s of useful inner IP payload on a clean, capable direct path

## Executive conclusion

Connect will not reach gigabit P2P throughput by tuning the current reliable
WebRTC DataChannel/SCTP path. The dominant limit is architectural:

1. Connect carries TCP, QUIC, and other IP traffic inside a reliable SCTP
   association.
2. SCTP fragments each ordinary tunnel packet into multiple small UDP writes,
   acknowledges it, applies a shared Reno-style congestion window, and
   retransmits loss.
3. Connect's Transfer layer also sequences, acknowledges, and may retransmit
   the same data.
4. The app, Transfer, crypto, SCTP, UDP, and TUN boundaries do not preserve
   batches end to end.

The local same-host WebRTC route currently reaches about 32–35 MiB/s with the
production 3 KiB Transfer message size. A CPU profile attributes 47.89% of all
samples to the raw syscall boundary and about 38% to the SCTP → DTLS → ICE →
UDP write chain. Increasing the route queue from 1 to 32 did not improve
throughput. This is direct evidence that the immediate ceiling is not too
little queueing.

The recommended design is a capability-negotiated **P2P datagram data plane**
for native peers:

- retain the existing reliable WebRTC/Transfer path for signaling, identity,
  contracts, key establishment, accounting checkpoints, control messages, and
  fallback;
- send each inner IP packet as one independently authenticated datagram, with
  no Connect or SCTP data retransmission;
- preserve Connect policy, IP-security checks, replay protection, and contract
  accounting;
- batch packets across the complete TUN → policy → route → crypto → UDP path;
- use platform UDP and TUN batching where the OS exposes it;
- keep legacy WebRTC DataChannel behavior for browsers and old clients.

This is the same fundamental performance model used by WireGuard and Tailscale:
one encrypted IP datagram remains one UDP datagram, while the implementation
batches the system calls used to move many datagrams. It does **not** mean
copying Tailscale's control plane or treating WireGuard as a drop-in protocol.

Desktop and Linux gigabit is a credible target after this work, provided the
unencrypted underlay sustains materially more than 1 Gbit/s. Gigabit on mobile
is conditional on the radio, device, OS packet API, CPU, and thermal state; it
must not be inferred from a desktop result.

---

## What “gigabit” means

One decimal gigabit per second is:

- 1,000,000,000 bits/s;
- 125,000,000 bytes/s;
- 119.2 MiB/s.

The target in this document is **useful inner IP payload**, not UDP wire rate or
an isolated codec benchmark. Protocol overhead means the physical path must
normally sustain at least 1.1–1.3 Gbit/s to deliver 1 Gbit/s of inner payload.
An implementation stage should sustain 150–200 MiB/s in isolation so normal
composition does not consume all available headroom.

The result must also preserve latency, loss behavior, security, policy, and
accounting. A benchmark that reaches line rate by building a standing queue,
disabling authentication, or omitting contract accounting does not meet the
target.

## Current native P2P data path

The native path is:

    app TUN
      → SDK packet callback
      → IP parsing, policy, routing, and contract selection
      → Transfer Frame / Pack envelope and optional outer AEAD
      → reliable unordered WebRTC DataChannel
      → SCTP fragmentation, SACK, congestion control, and retransmission
      → DTLS
      → ICE
      → UDP socket
      → peer, followed by the reverse path

Provider traffic then crosses the userspace TUN and gVisor network/NAT path.
The same-network direct route still uses Transfer contracts and security
policy; P2P is not a bypass around those controls.

The important current settings are:

| Area | Current behavior | Consequence |
|---|---|---|
| DataChannel | Unordered, but reliable because MaxRetransmits is unset | Removes ordered delivery head-of-line blocking, but retains SCTP ACKs, retransmission, flow control, and congestion control |
| SCTP outgoing MTU | Pion starts at 1,191 bytes | A 1,440-byte inner packet plus its envelope requires at least two SCTP packets |
| SCTP selected-peer receive window | 2 MiB | Better than the public 256/512 KiB window, but below the bandwidth-delay product of a gigabit path at ordinary WAN RTT |
| SCTP congestion control | Shared Reno-style association window with a measured 8-MTU avoidance step | Multiple DataChannels still share one association window |
| Transfer message | At most two frames and 3 KiB in the normal hot path | Reduces some envelope work, but normally produces three SCTP/UDP writes |
| Transport queue | Four messages | Measured queue depths from 1 through 32 did not materially change throughput |
| Transfer reliability | ACK enabled by default; UDP/ICMP ACKs remain enabled unless collapse prevention is selected | Adds a second recovery and accounting loop around reliable SCTP |
| TUN MTU | 1,440 bytes | Reasonable for the current encapsulation, but must be recalculated for a new IPv4/IPv6 datagram header |

Relevant implementation points are
[transport_p2p_webrtc.go](transport_p2p_webrtc.go),
[transport_p2p_webrtc_pc.go](transport_p2p_webrtc_pc.go),
[transport_p2p.go](transport_p2p.go), [transfer.go](transfer.go),
[ip.go](ip.go), and the SDK app bridge in
[device_local_ioloop.go](../sdk/device_local_ioloop.go).

## Findings

### P2PG-001 — reliable tunneling duplicates recovery

**Priority: highest**

Most useful tunnel traffic is already congestion-controlled and reliable at
the inner layer. TCP and QUIC detect loss, adjust their rate, and retransmit.
Putting that traffic inside reliable SCTP creates nested recovery loops:

- the inner transport waits for and reacts to loss;
- SCTP also waits, reduces its association window, and retransmits;
- Connect Transfer may independently retry an unacknowledged Pack.

The outer recovery can hide loss from the inner controller for a while, add
latency, and then expose a burst. One lost outer datagram also consumes SCTP
association state shared by unrelated inner flows.

[RFC 9484](https://www.rfc-editor.org/rfc/rfc9484.html) describes this nested
congestion-control and recovery problem for IP tunnels and recommends
unreliable datagrams where appropriate. [RFC 8085 section
3.1.11](https://www.rfc-editor.org/rfc/rfc8085.html#section-3.1.11) likewise
allows proportional-response IP tunnels to rely on inner congestion control,
provided they include aggregate safety mechanisms for traffic that is not
responsive.

**Conclusion:** ordinary inner IP packets must not be retransmitted by the P2P
data plane. The reliable path should carry control and cumulative accounting,
not the bulk data itself.

### P2PG-002 — small SCTP packets make the path syscall-bound

**Priority: highest**

Pion SCTP v1.11.1 uses an initial outgoing MTU of 1,191 bytes. After SCTP
headers and padding, an I-DATA packet carries about 1,156 bytes of application
payload. A normal 1,440-byte inner packet plus the Transfer envelope therefore
cannot fit in one SCTP packet. The current two-frame, approximately 3 KiB
Transfer message normally takes three.

At 1 Gbit/s of 1,440-byte inner packets, Connect must process about 86,806
inner packets per second. Even ideal two-packet Transfer coalescing produces
roughly 130,000 outbound SCTP/DTLS/UDP writes per second before SACK traffic.
Pion's association write loop calls net.Conn.Write once for each serialized
SCTP packet.

This matches the profile:

| CPU profile entry | Cumulative or flat share |
|---|---:|
| rawsyscalln | 47.89% flat |
| SCTP writeLoop | 39.31% cumulative |
| DTLS Write | 38.54% cumulative |
| ICE / UDP WriteToUDPAddrPort | 37.90% cumulative |
| sendto | 37.77% cumulative |
| receive syscall path | 9.99% cumulative |

The profile captured two local peers in one process, so it is not a
single-endpoint CPU budget. It is still decisive about where the composed path
spends its time.

Pion source:

- [SCTP MTU and payload sizing](https://github.com/pion/sctp/blob/v1.11.1/association.go#L66-L92)
- [one net.Conn.Write per raw SCTP packet](https://github.com/pion/sctp/blob/v1.11.1/association.go#L1189-L1204)

**Conclusion:** fewer small SCTP messages and deeper Go channels can make the
legacy path somewhat faster, but they cannot provide the syscall shape needed
for gigabit.

### P2PG-003 — packet batches are lost between layers

**Priority: highest**

Gigabit at ordinary MTUs is a packet-rate problem. The current layers usually
hand off one packet or one Transfer message at a time:

- the SDK I/O loop invokes one packet write callback per TUN read;
- the Apple extension receives an array from NEPacketTunnelFlow, then calls
  into Go once per packet;
- Apple writes a singleton packet array for each Go receive callback;
- Transfer writes each completed TransferFrame separately;
- Pion writes each SCTP packet separately;
- non-Linux UDP paths generally have no system-call batching.

Android is better than a JNI-per-packet design because Go owns the detached VPN
file descriptor after setup. Android's VPN TUN interface still exposes one
packet per read/write, so a Go-side drain or micro-batch is needed above that
boundary.

Connect already has useful pieces, including TUN batch/GRO support. They do not
form one continuous batch pipeline.

**Conclusion:** add explicit SendBatch and ReceiveBatch contracts across the
complete hot path. A batch must remain a collection of independent datagrams;
it must not become one oversized UDP datagram.

### P2PG-004 — the SCTP window cannot cover a gigabit WAN path

**Priority: high for the legacy path; avoided by the new datagram path**

The selected peer's 2 MiB receive window has these theoretical ceilings before
any other loss:

| RTT | 2 MiB window ceiling |
|---:|---:|
| 50 ms | 40 MiB/s, about 336 Mbit/s |
| 100 ms | 20 MiB/s, about 168 Mbit/s |

The receive-side bandwidth-delay product for 1 Gbit/s is about 5.96 MiB at
50 ms and 11.92 MiB at 100 ms. A reliable SCTP path would need at least about
8 MiB and 16 MiB respectively, plus sender congestion-window growth, to avoid
flow-control limitation.

The current 2 MiB selected window was a successful targeted correction:
controlled 50 ms throughput improved from 2.26 MiB/s to 28–29 MiB/s. Physical
Android measurements were still congestion-window limited, not receive-window
limited. Raising the window again is therefore necessary only if SCTP remains;
it is neither sufficient nor free, especially when every peer association
reserves mobile memory.

**Conclusion:** retain bounded selected-peer tuning for compatibility, but do
not make a large static SCTP window the gigabit design.

### P2PG-005 — per-packet Transfer metadata and ACK work remain material

**Priority: high**

Each hot-path TransferFrame can carry sequence, path, identity, contract, tag,
and encryption metadata. The receiver creates ACK/accounting work and the
sender retains retry state. These features are correct for a store-and-forward
Transfer protocol, but are unnecessarily repeated for every packet in an
established direct session.

The contract cannot simply be removed: current accounting advances with
delivered or acknowledged bytes. The fast path needs an equivalent monotonic
record without turning an accounting receipt into a data recovery protocol.

**Conclusion:** bind the contract and route once to an authenticated fast-path
session. Count accepted inner payload at the receiver and send cumulative
authenticated byte checkpoints over the reliable control channel.

### P2PG-006 — the composed provider path has a second ceiling

**Priority: high after the transport prototype**

The isolated gVisor TUN TCP test sustained 290.5–308.5 MiB/s in nine local
results, comfortably above gigabit. The optimized Transfer core measured
89.79–94.84 MiB/s with a 92.00 MiB/s median, while the composed provider load
test measured 54.6–58.8 MiB/s.

These harnesses exercise different boundaries, so their numbers and earlier
microbenchmark improvements must not be multiplied. They show:

- gVisor/TUN is not intrinsically capped below gigabit;
- composition, handoffs, and per-packet work lose a large fraction of the
  isolated capacity;
- after replacing SCTP, the full provider path must be profiled again rather
  than assumed solved.

Detailed prior measurements are in
[PACKETRESEARCH1.md](PACKETRESEARCH1.md) and
[OPTIMIZENETWORKPEER1.md](OPTIMIZENETWORKPEER1.md).

### P2PG-007 — encryption is not the primary bottleneck

**Priority: do not optimize first**

The current pooled AES-GCM benchmarks on the same Apple M1 Max measured:

| Operation | Throughput | Allocations |
|---|---:|---:|
| fused outer wrap | about 1,942 MB/s | 0 |
| pooled open | about 3,923 MB/s | 0 |

This is an isolated benchmark, not proof that all crypto composition is free.
It does establish that authenticated encryption itself has ample headroom for
1 Gbit/s on this machine. Removing encryption would weaken the product without
addressing the measured syscall ceiling.

### P2PG-008 — direct-path selection needs ongoing quality and MTU feedback

**Priority: medium**

ICE selects a working pair, but a gigabit implementation also needs to know
whether that pair remains the best path. Local/private endpoints, public
endpoints, interface changes, IPv4/IPv6, NAT behavior, loss, RTT, path MTU,
and relay fallback can all change after setup.

The current inner MTU of 1,440 bytes may fit a compact new header over IPv4 but
can exceed a 1,500-byte outer path over IPv6. Sending an oversized encrypted
UDP datagram and relying on IP fragmentation amplifies loss: one missing
fragment loses the entire inner packet.

**Conclusion:** use conservative initial MTUs, Datagram Packetization Layer
Path MTU Discovery, validated migration, quality hysteresis, ICMP Packet Too
Big synthesis or MSS adjustment, and a reliable fallback.

---

## Reproducible measurements

### Real detached WebRTC route

Run:

~~~sh
CONNECT_WEBRTC_ROUTE_THROUGHPUT_MEASURE=1 \
  go test -run '^TestWebRtcP2pRouteThroughputMeasurement$' -count=1 -v .
~~~

Results from the 3 KiB production-size message:

| Route queue depth | Throughput |
|---:|---:|
| 1 | 35.1 MiB/s |
| 4 | 32.1 MiB/s |
| 8 | 33.9 MiB/s |
| 32 | 32.5 MiB/s |

Prior larger-message experiments reached approximately 56–58 MiB/s at
12–48 KiB. That is useful evidence that fewer app handoffs help, but SCTP still
fragments those messages into roughly 1.2 KiB datagrams. It is not a route to
119.2 MiB/s useful payload.

### CPU profile

Run:

~~~sh
CONNECT_WEBRTC_ROUTE_THROUGHPUT_MEASURE=1 \
  go test -run '^TestWebRtcP2pRouteThroughputMeasurement$' \
  -count=1 -cpuprofile /tmp/connect-p2p.pprof .
go tool pprof -top -cum /tmp/connect-p2p.pprof
~~~

The captured run lasted 4.13 seconds and contained 7.81 CPU-seconds of samples
because both local endpoints were active. Its top path is summarized in
P2PG-002.

### Loss and delivery semantics

Run:

~~~sh
CONNECT_WEBRTC_LOSS_MEASURE=1 \
  go test -run '^TestWebRtcDataChannelLossHeadOfLineMeasurement$' \
  -count=1 -v .
~~~

After one deterministic dropped DTLS datagram:

| DataChannel mode | Second-message p95 / max |
|---|---:|
| ordered reliable | about 124.8 ms |
| unordered reliable, current production mode | about 558.8 µs |
| unordered, zero retransmissions | about 333.5 µs |

Reliable unordered delivery was a sound latency improvement. Partial
reliability is a useful compatibility experiment, but it still retains the
SCTP association, SACK processing, congestion window, fragmentation, and
one-write-per-packet loop.

### Crypto and TUN isolation

Run:

~~~sh
go test -run '^$' \
  -bench 'BenchmarkSequenceCipher(OuterWrap|Open)$' \
  -benchtime=1s .
go test -run '^TestTunTCPThroughput$' -count=3 -v .
~~~

The results support P2PG-006 and P2PG-007. They are diagnostic isolation
measurements, not end-to-end throughput claims.

---

## What Tailscale does differently

The source audit used Tailscale commit
[e1e5325c22a46a9df2e76d725f01f92065885138](https://github.com/tailscale/tailscale/tree/e1e5325c22a46a9df2e76d725f01f92065885138)
and its WireGuard-Go fork commit
[4affce44577c](https://github.com/tailscale/wireguard-go/tree/4affce44577c).
The audit focused on the data path, batching, path selection, and MTU behavior,
not just published benchmark numbers.

### One inner packet remains one encrypted datagram

WireGuard authenticates and encrypts an IP packet, then sends it as a UDP
datagram. It does not add reliable delivery, a byte-stream receive window, or
outer data retransmission. Inner TCP or QUIC sees loss and performs recovery.

WireGuard-Go moves packet containers through one encryption worker per CPU,
then serializes nonce/order-sensitive work per peer and sends a batch. Its
generic connection API exposes an
[ideal batch size of 128](https://github.com/tailscale/wireguard-go/blob/4affce44577c/conn/conn.go#L17-L26);
the [send pipeline](https://github.com/tailscale/wireguard-go/blob/4affce44577c/device/send.go#L481-L586)
separates parallel encryption from per-peer ordered sending.

This shape is directly applicable to Connect:

- parallelize independent packet parsing and encryption;
- preserve per-peer counter order in one bounded sender;
- submit many independent UDP datagrams in one OS operation.

### Linux uses the kernel's packet batching features

Tailscale's Linux batch connection uses:

- sendmmsg and recvmmsg;
- UDP Generic Segmentation Offload and Generic Receive Offload;
- scatter/gather to coalesce up to 64 equal-sized datagrams without copying
  their payload into one application buffer;
- socket overflow reporting;
- runtime fallback when offload is unavailable or fails.

See the
[Linux batching implementation](https://github.com/tailscale/tailscale/blob/e1e5325c22a46a9df2e76d725f01f92065885138/net/batching/conn_linux.go#L86-L130).
On non-Linux platforms, Tailscale's generic implementation reports an ideal
batch size of one:
[conn_default.go](https://github.com/tailscale/tailscale/blob/e1e5325c22a46a9df2e76d725f01f92065885138/net/batching/conn_default.go).

This distinction matters. Tailscale's multi-gigabit public results are
bare-metal or high-capacity Linux results. They demonstrate the architecture's
headroom, not that an iPhone or Android VPN API will deliver the same rate.

### Endpoint selection is continuous, not just setup-time

Tailscale's magicsock endpoint code scores working paths using properties such
as directness, latency, endpoint scope, relay state, and MTU, with hysteresis
to avoid unnecessary switching. It can briefly hedge traffic while a path is
being re-established. See the
[send/path selection](https://github.com/tailscale/tailscale/blob/e1e5325c22a46a9df2e76d725f01f92065885138/wgengine/magicsock/endpoint.go#L997-L1039)
and
[quality scoring](https://github.com/tailscale/tailscale/blob/e1e5325c22a46a9df2e76d725f01f92065885138/wgengine/magicsock/endpoint.go#L1709-L1768).

Connect should borrow the principle, not the control-plane coupling:
authenticate a candidate before migration, measure current path quality, prefer
direct/private paths where they are actually better, and retain a bounded
fallback.

### MTU behavior is conservative

Tailscale accounts for up to 80 bytes of WireGuard/UDP/IP overhead, probes
larger path sizes, and retains a safe baseline until Packet Too Big handling is
available. Its probe set includes 1,280, 1,360, 1,400, 1,500, 8,000, and 9,000
wire bytes. See
[Tailscale MTU policy](https://github.com/tailscale/tailscale/blob/e1e5325c22a46a9df2e76d725f01f92065885138/net/tstun/mtu.go#L64-L101).

The lesson is not “enable jumbo frames.” It is to avoid fragmentation, discover
the usable path size, and make a larger MTU an evidence-based per-path
optimization.

### Published Tailscale results

Tailscale reports:

- [5.36 Gbit/s after TSO, GRO, and message batching work](https://tailscale.com/blog/throughput-improvements);
- [13 Gbit/s on a bare-metal Linux test after UDP GSO/GRO and checksum work](https://tailscale.com/blog/more-throughput);
- [a roughly fourfold UDP/QUIC forwarding improvement on bare-metal Linux](https://tailscale.com/blog/quic-udp-throughput).

Their profiles found the same general cost seen in Connect: TUN and UDP system
calls dominate after obvious user-space allocations and copies are removed.
The useful comparison is the data-path structure and profiling method, not the
headline number.

---

## Recommended P2P v2 architecture

### 1. Keep the current connection as the control plane

The existing path already provides:

- ICE discovery and direct-path negotiation;
- peer identity and authenticated TLS exporter material;
- post-quantum-capable X25519MLKEM768 TLS negotiation;
- contracts and escrow state;
- signaling, liveness, and legacy interoperability.

Keep it reliable. Add a capability exchange that enables the datagram data
plane only when both native peers support the same version and security
features. Browsers and older clients continue using DataChannel/Transfer
unchanged.

Do not assume Pion's internal ICE socket can simply be wrapped. The production
fast path must expose the actual UDP socket and its out-of-band control data so
Linux GSO/GRO, ECN, path MTU signals, socket overflow counters, and batch I/O
remain available.

### 2. Establish a dedicated native datagram component

Use ICE candidate gathering and nomination to establish a dedicated UDP
component for native-to-native packet data. The exact integration can reuse
current candidate knowledge or add a separately negotiated component, but the
result must not pass bulk packets through SCTP.

Each wire datagram should contain exactly one inner IP packet:

    compact authenticated header
      + encrypted inner IP packet
      + AEAD tag

Candidate header fields:

- protocol version and flags;
- receiver or session index;
- 64-bit packet counter;
- key generation, if it is not implicit in the receiver index.

Peer IDs, route IDs, contract IDs, and long protobuf structures belong in the
authenticated session setup, not every packet. Header fields are AEAD
additional authenticated data.

### 3. Derive new directional keys

Derive independent send and receive keys from the existing completed,
identity-bound TLS exporter. Use a new domain-separation label and context that
includes both peer identities, session or contract identity, direction, and
key generation.

Do not reuse the current sequenceCipher wire format unchanged. It uses a fresh
random nonce per TransferFrame and no explicit replay window or authenticated
header. That is suitable for its reliable Transfer transport, but a datagram
protocol needs:

- monotonic per-direction counters;
- deterministic unique nonces derived from the counter and key generation;
- a sliding receive replay window;
- authenticated receiver/session routing;
- bounded current/previous-key overlap during rekey;
- hard packet/time limits before key rotation;
- no response to unauthenticated traffic;
- endpoint roaming only after successful packet authentication;
- rate limits and anti-amplification controls around handshakes and probes.

WireGuard's
[protocol](https://www.wireguard.com/protocol/) and
[paper](https://www.wireguard.com/papers/wireguard.pdf) are strong references
for receiver indices, counters, replay windows, rekey overlap, and endpoint
roaming. Connect should preserve its existing identity, contract, and hybrid
post-quantum key establishment instead of replacing those systems.

### 4. Separate accounting from retransmission

For each authenticated fast-path session:

1. bind one contract, peer pair, direction, and session epoch during setup;
2. count only authenticated, replay-accepted inner payload bytes at the
   receiver;
3. periodically send a monotonic cumulative receipt over the reliable control
   channel, triggered by both elapsed time and byte threshold;
4. let a later cumulative receipt subsume a lost earlier receipt;
5. reconcile a final receipt during orderly close, rollover, or timeout.

Duplicate, replayed, unauthenticated, rejected-policy, and malformed packets
must not increment the receipt. The sender must never interpret a receipt as an
instruction to retransmit missing data. Inner TCP/QUIC remains responsible for
recovery.

This preserves the current meaning of delivered/acknowledged contract bytes
without placing a reliable protocol around the data. It also reduces per-packet
protobuf, ID, tag, ACK, retry-queue, and timer work.

### 5. Batch across every hot boundary

Define a bounded packet batch type with explicit buffer ownership. Preserve
packet boundaries and carry only the metadata each stage needs.

Outbound:

    TUN batch
      → parallel parse and policy
      → route/session lookup
      → parallel AEAD seal
      → per-peer counter/order queue
      → UDP batch send

Inbound:

    UDP batch receive
      → session lookup and replay precheck
      → parallel AEAD open
      → replay commit, policy, and accounting
      → TUN batch write

Rules:

- queues are bounded by both packet count and byte count;
- receive callbacks never block; overload drops and increments a metric;
- encryption can run across CPU workers, while nonce allocation and final
  per-peer send order remain deterministic;
- route, contract, and security decisions may be cached only with explicit
  invalidation on policy/session change;
- packet buffers have one documented owner at every handoff;
- batch fallback is functionally identical to the fast path.

Platform work:

| Platform | Required work |
|---|---|
| Linux | Actual UDPConn; sendmmsg/recvmmsg; UDP GSO/GRO; TUN TSO/GRO where supported; BDP-aware socket buffers; SO_RXQ_OVFL; runtime offload fallback |
| Apple | Preserve NEPacketTunnelFlow read arrays in one SDK call; return batches to one writePackets call; avoid singleton crossing in both directions |
| Android | Keep Go ownership of the VPN fd; drain/micro-batch packets above one-packet TUN reads; batch subsequent policy/crypto/UDP work |
| Windows | Drain the Wintun ring into bounded batches and return batches through the ring API |

### 6. Add path MTU discovery and quality scoring

Start conservatively, likely near 1,400 inner bytes on ordinary 1,500-byte
paths, then validate the exact new header against both outer IPv4 and IPv6.
Use Datagram Packetization Layer PMTUD instead of IP fragmentation. Oversized
inner packets should produce correct Packet Too Big behavior or TCP MSS
adjustment.

Measure candidate RTT, recent loss/drop signal, delivery rate, path MTU, scope,
and relay/direct state. Switch only after the new endpoint is authenticated
and materially better for long enough to overcome hysteresis. Keep the
reliable WebRTC path available while a new datagram path is unproven.

### 7. Retain congestion safety without outer retransmission

An IP tunnel may carry UDP applications that do not respond to congestion.
The datagram path therefore needs:

- paced startup rather than an unbounded burst;
- bounded queues that drop instead of creating bufferbloat;
- aggregate path loss/RTT monitoring and a circuit breaker;
- explicit rate policy for persistently nonresponsive traffic;
- receiver overflow and socket-drop telemetry;
- no data retransmission at the tunnel layer.

This follows the proportional-tunnel guidance in RFC 8085. It avoids nested
recovery without treating all inner UDP as automatically safe.

---

## Transport choices

| Choice | Advantages | Limits | Recommendation |
|---|---|---|---|
| Tune current reliable SCTP | Small, compatible changes | Shared Reno window, fragmentation, SACK/retry work, and syscall shape remain | Maintain only as fallback |
| Unordered SCTP with zero retransmissions | Capability-negotiated stepping stone; tests failure semantics quickly | Still SCTP-fragmented, association-congestion-controlled, and one write per packet | Build as an experiment, not the final design |
| QUIC DATAGRAM | Mature Go implementation; authentication, migration, PMTUD, and Linux GSO support; explicitly designed for VPN-style unreliable payloads | QUIC DATAGRAM remains congestion-controlled; can create nested control; a generic PacketConn wrapper can hide GSO, ECN, and OOB data | Benchmark as the safest rapid prototype |
| Connect authenticated raw UDP | Exact semantics; no outer recovery; easiest to batch like WireGuard; can derive keys from existing PQ-capable identity session | Connect must implement replay, rekey, PMTU, pacing/circuit breaker, migration validation, and protocol review | Recommended production direction after prototype and review |
| WireGuard-Go data plane | Proven batching, queues, replay, counters, roaming, and high throughput | Not a drop-in match for Connect contracts, multi-consumer provider addressing, current identity/PQ session, or control plane | Reuse design and possibly isolated machinery, not the full system by assumption |

[RFC 9221](https://www.rfc-editor.org/rfc/rfc9221.html) explicitly identifies
VPNs as a QUIC DATAGRAM use case and confirms that DATAGRAM frames are
unreliable and not flow-controlled, while still participating in QUIC
congestion control. quic-go can use Linux UDP GSO and DPLPMTUD, but its
documentation warns that wrapping a UDPConn in a generic interface may hide
important capabilities:

- [quic-go Transport and socket requirements](https://quic-go.net/docs/quic/transport/)
- [quic-go performance optimizations](https://quic-go.net/docs/quic/optimizations/)

Multiple WebRTC DataChannels are not a solution. RFC 8831 defines them over one
SCTP association, whose congestion control is shared:
[RFC 8831](https://www.rfc-editor.org/rfc/rfc8831.html).

---

## Phased implementation plan

### Phase 0 — freeze a trustworthy baseline

- Add one command that records raw underlay iperf3 TCP and UDP capacity,
  current WebRTC route goodput, full-tunnel goodput, CPU, syscalls, drops,
  latency, and accounting delta.
- Record both directions and five fresh-process repetitions.
- Label same-host, virtual-network, and physical-device measurements
  separately.
- Do not report a gigabit tunnel result when the raw underlay is below
  1.2 Gbit/s.

### Phase 1 — test semantics on the existing association

- Capability-negotiate a second unordered DataChannel with MaxRetransmits set
  to zero for inner IP traffic.
- Keep reliable control on the existing channel.
- Disable Transfer data ACK/retry for that experimental lane.
- Add cumulative contract receipts.
- Test loss, duplication, reordering, closure, rollover, and old-client
  fallback.

This phase validates accounting and failure behavior. It is not expected to
reach gigabit because SCTP fragmentation, congestion control, and writes
remain.

### Phase 2 — build two datagram prototypes

- Prototype QUIC DATAGRAM over a socket path that retains actual UDP/OOB
  capabilities.
- Prototype the compact exporter-derived raw authenticated UDP format.
- Use the same packet-batch API and accounting receipt model in both.
- Compare transport-only throughput and behavior before integrating the
  complete provider.

Select the production transport using measured clean-path throughput, loss
behavior, implementation complexity, mobile support, and security review.

### Phase 3 — make batching end to end

- Add batch interfaces and buffer ownership tests in Connect and SDK.
- Implement Linux UDP and TUN batching/offload first because it provides the
  clearest gigabit validation platform.
- Preserve Apple packet arrays.
- Add Android and Windows bounded drain batches.
- Feed decrypted inbound batches into Tun.WriteBatch and outbound TUN batches
  directly to the crypto workers.

### Phase 4 — optimize the composed provider

- Profile the full native app → P2P → provider gVisor/NAT → Internet path.
- Remove remaining singleton handoffs, redundant parsing, and cache misses.
- Preserve all connect/ip_security decisions and contract enforcement.
- Consider an optional Linux kernel-forwarding provider only if the
  userspace path remains below target and after a separate privilege/security
  review.

### Phase 5 — path quality and rollout

- Add DPLPMTUD, endpoint-quality scoring, authenticated migration, and relay
  fallback.
- Roll out behind capabilities and environment flags.
- Compare accounting between old and new paths during a shadow/canary period.
- Keep immediate disable and legacy fallback controls.

---

## Deterministic validation matrix

### Network conditions

| Dimension | Values |
|---|---|
| RTT | 0, 10, 25, 50, 100 ms |
| independent loss | 0%, 0.01%, 0.1%, 1% |
| path rate | 100, 300, 1,000, 2,500 Mbit/s |
| queue | shallow and deep |
| inner MTU | 1,280, 1,400, and validated 1,500-path maximum |
| traffic | one, four, and sixteen TCP flows; QUIC; rate-limited UDP |
| direction | consumer → provider and provider → consumer |
| repetition | five fresh runs, report median and p95 |

Run on:

- Linux bare metal over Ethernet;
- macOS/Windows desktop over Ethernet and Wi-Fi 6E where available;
- physical current-generation iPhone and Pixel;
- same-LAN, NAT-to-NAT, IPv4, IPv6, network-change, and relay-fallback paths.

### Measurements

- useful inner payload and outer wire throughput;
- packets and syscalls per second;
- packets per batch distribution;
- CPU total and per core;
- allocations, GC, and retained memory;
- TUN, queue, socket, replay, authentication, and kernel overflow drops;
- RTT, loss, receive window, congestion window, retransmissions where
  applicable;
- p50/p95 application latency under bulk load;
- device thermal state and battery cost;
- sender bytes versus authenticated receiver accounting receipts.

### Exit gates

1. Every isolated hot stage sustains at least 200 MiB/s on the Linux reference
   machine.
2. The encrypted datagram transport sustains at least 150 MiB/s useful payload
   on a clean LAN.
3. The full tunnel sustains at least 119.2 MiB/s median useful payload when the
   measured raw underlay is at least 1.2 Gbit/s.
4. Bulk load does not materially regress p95 latency or create a standing
   queue relative to the available path.
5. Lossy and slow paths converge safely without retry storms, unbounded memory,
   or worse failure recovery than the legacy path.
6. Accounting is exact and monotonic under loss, reordering, duplication,
   replay, rekey, migration, receipt loss, contract rollover, and abrupt close.
7. Invalid authentication, replay-window, policy, IP-security, MTU, and
   anti-amplification cases have deterministic tests.
8. Old peers and browsers fall back without user-visible failure.

## Work that should not lead the project

- **Larger route queues.** Depths 1, 4, 8, and 32 measured the same range, while
  larger queues retain more memory and add queueing latency.
- **More DataChannels.** They share the SCTP association congestion window and
  write loop.
- **More fixed SCTP congestion-window tuning.** The repository already tested
  larger avoidance steps, beta changes, minimum windows, fast retransmit
  changes, and lower retry bounds. Aggressive settings traded improvement in
  one loss model for collapse or bufferbloat elsewhere.
- **Removing encryption.** Current AEAD throughput is well above the target.
- **One jumbo application UDP datagram.** It destroys packet independence and
  amplifies loss through IP fragmentation.
- **Decoder or protobuf micro-tuning alone.** Those paths are already heavily
  optimized and the composed profile is syscall-dominated.
- **Adding a second reliable protocol.** A new reliable stream with a larger
  window changes the implementation, not the nested-recovery problem.

## Existing improvements that must be preserved

The current code already contains important, measured corrections:

- egress-only ICE interface enumeration and Android path handling;
- selected-peer admission and a bounded 2 MiB receive window;
- live STUN endpoints and bounded gathering;
- reliable unordered DataChannel delivery;
- SCTP SNAP and negotiated zero checksum;
- the measured 8-MTU congestion-avoidance step;
- SCTP no-progress detection and network-change reconstruction;
- two-frame Transfer coalescing;
- pooled zero-copy decoding, sharded queues/timers/ACK state, and bounded
  ownership;
- gVisor TUN batch/GRO support;
- teardown, route recovery, and contract-accounting correctness work.

P2P v2 should replace the limiting bulk-data semantics without regressing
these setup, security, lifecycle, memory, and fallback properties.

## Final assessment

Confidence is:

- **High** that an unreliable authenticated datagram plane and end-to-end
  batching are necessary for gigabit.
- **High** that queue-depth increases, additional DataChannels, and removing
  encryption will not solve the measured ceiling.
- **Medium** that the current userspace Connect/provider architecture can reach
  gigabit on desktop/Linux after the datagram and batch work; isolated TUN and
  crypto measurements provide the required headroom, but the composed provider
  must prove it.
- **Conditional** for mobile gigabit because platform packet APIs, radios,
  thermals, and device CPUs vary substantially.

The shortest credible route is therefore:

1. preserve WebRTC/Transfer as the reliable control and compatibility plane;
2. prove cumulative contract receipts using a zero-retransmit experimental
   lane;
3. benchmark QUIC DATAGRAM and a compact exporter-derived authenticated UDP
   prototype;
4. select the transport from measurements and security review;
5. batch every layer, starting with Linux;
6. validate the complete provider path under a deterministic network matrix.

That path addresses the measured bottleneck and retains the properties that
make Connect more than a generic packet tunnel.
