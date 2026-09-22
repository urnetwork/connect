# H1+: authenticated HTTP/1.1 upgrade to URnetwork Framer

Status: design and measured prototype, 2026-09-22. H1+ is **not implemented or
enabled in production**. The separate, general `Framer.WriteBatchWithStorage`
singleton improvement and its existing-carrier call-site changes do not enable
H1+. This document distinguishes those changes from the proposed transport.

## Purpose and scope

Offer native H1 clients a more efficient message carrier when the server and
HTTP proxy support it: try `Upgrade: urnetwork-framer/1`, then use the existing
four-byte Framer format directly over the upgraded TLS stream. Otherwise open a
fresh, ordinary RFC-compliant WebSocket connection. Retain message boundaries,
authentication, transfer semantics, deadlines, bounded memory, and backpressure.

H1+ is an implementation capability inside the H1 transport family, not another
user-selected H1/H3/DNS mode. Both choices use HTTP/1.1 over TLS before upgrade.
The Upgrade token and post-101 bytes are distinguishable to TLS terminators;
this is not an identical wire signature or a censorship-resistance claim.
Browsers cannot expose a raw upgraded stream through the WebSocket API and must
skip the attempt and continue using standard WebSockets.

The objective is less CPU per transferred packet and fewer unnecessary copies,
pool operations, and stream handoffs. Removing XOR alone does not remove copies.
The benchmark measures a complete carrier substitution, not the isolated cost
of masking, mobile battery, end-to-end VPN speed, or fast.com.

## Upgrade and authentication contract

The versioned token is exactly `urnetwork-framer/1`. Use an existing authorized
endpoint and its existing TLS identity and credentials, for example:

```http
GET /<existing-connect-endpoint> HTTP/1.1
Host: connect.example
Connection: Upgrade
Upgrade: urnetwork-framer/1
Authorization: Bearer <existing-connect-credential>
X-UR-InstanceId: <existing-instance-id>
X-UR-AppVersion: <existing-app-version>
X-UR-TransportVersion: <existing-transport-version>

```

Only after validating authentication and admission does the server send:

```http
HTTP/1.1 101 Switching Protocols
Connection: Upgrade
Upgrade: urnetwork-framer/1

```

There is no `Sec-WebSocket-Key`, `Sec-WebSocket-Accept`, WebSocket opcode, mask,
or compression negotiation on this custom path. HTTP Upgrade is a general
mechanism, not exclusive to WebSocket. [RFC 9110, Upgrade](https://www.rfc-editor.org/rfc/rfc9110.html#section-7.8)

Requirements for production implementation:

- Require HTTP/1.1, GET, one supported Upgrade selection, a valid Connection
  upgrade token, no request body or ambiguous framing, bounded headers, and
  bounded handshake time. Do not accept bytes as application payload before 101.
- Share connect's real JWT audience, state/revocation, client membership,
  instance-id, rate-limit, and resource-admission checks. **Parsing a header is
  not authentication.** Existing H1 currently performs JWT validation after the
  WebSocket upgrade; the new custom branch must finish those checks before 101.
  Keep the existing WS first-message-auth compatibility path separate.
- For `/device-rpc`, use the existing signed proxy-id authorization and hosted
  device admission before either upgrade. Native clients should prefer the
  existing Authorization form; retain browser query credentials for WS without
  expanding access-log exposure.
- Reject failed authorization with an ordinary 401/403, never 101. Apply the same
  security checks to both carriers. Terminal authorization failure must not be
  hidden as a capability miss or cause repeated authentication attempts.
- Preserve the server hijacker's buffered reader and the client's HTTP-response
  buffered reader: the first Framer bytes can share a TCP/TLS read with 101.
  Losing these bytes causes a deterministic first-message failure.
- Check the response status, Connection token, and selected Upgrade token before
  handing the socket to Framer. Reject missing, unexpected, or ambiguous
  selection. Do not follow cross-origin redirects with credentials.

The private benchmark and nginx seam test use a fixed synthetic
`X-UR-Network-Auth` value solely to verify the pre-101 gate. They do not test
production JWT, revocation, or signed proxy-id authentication.

## Native fallback and compatibility

```text
H1 selected
  native + capability permitted -> authenticated custom Upgrade attempt
    exact valid 101 -> Framer for this connection's whole lifetime
    unsupported/rejected/mismatched -> close socket -> fresh normal WS attempt
    auth/TLS/security failure -> existing terminal/security handling
  browser/JS or kill switch -> ordinary WebSocket directly
```

Do not reinterpret a failed custom socket as a WebSocket, switch carriers
mid-session, or replay application bytes sent after a successful 101 onto a
fallback connection. Only the handshake is retried; application retry semantics
remain owned by the existing transfer/RPC layer. The ordinary WS client still
generates masking keys and performs normal server-handshake validation.

Old servers may return 400, 404, 426, a normal 200 page, or a mismatched upgrade;
these are capability failures, not data-path blackholes. Make one bounded custom
attempt, close the connection on failure, and use a fresh WS connection. A
temporary, bounded per-origin capability cache should avoid an extra RTT on
every reconnect to known-old servers. Keep capability misses distinct from
unhealthy-exit reputation and preserve the original overall connect deadline.
Do not downgrade certificate/hostname failures, use plaintext, or accept a 101
with a different protocol as fallback. Test a rejected authenticated custom
request followed by successful WS on a demonstrably different socket.

## Framer wire, ownership, and flow control

Each frame is four header bytes followed by exactly the declared payload:

| Offset | Size | Meaning |
| --- | --- | --- |
| 0 | 2 | Unsigned big-endian payload length, 0..65,535 bytes |
| 2 | 2 | Existing big-endian split-position hint; zero for coalesced writes |
| 4 | length | Application message bytes, unmodified |

The existing stream reader ignores the split hint and reads the complete body.
Legacy split writes can emit a nonzero hint; it is not an opcode, fragmentation
flag, or a second length. Do not repurpose it without a new protocol version.
There is no message compression or WebSocket framing inside a Framer payload.
One connect transport message maps to one Framer payload; this is not necessarily
one IP packet after Transfer encoding, encryption, or aggregation.

Each endpoint must configure its actual maximum payload, at most 65,535, before
reading. Validate bounds before allocation and before emitting any part of an
invalid outbound batch. Reject oversized or truncated frames and terminate the
stream after any failed/short write; retrying a partially written frame on the
same stream would desynchronize it. Transport EOF is terminal, not a resumable
frame boundary. No unbounded buffers, speculative reassembly, or retry queues.

One reader and one writer may use a Framer concurrently. Each direction has one
owner; simultaneous writers or simultaneous readers are not supported. The
writer's reusable storage must be exclusive until the call returns and must not
overlap any source message. Sources may share read-only backing with each other.
Framer does not take ownership of write inputs. The caller returns pooled
messages after synchronous completion, including errors. Successful reads return
one pooled message that the consumer must eventually release.

Use `WriteBatchWithStorage` for ready messages and sufficiently sized singletons:
it copies each payload once into bounded writer-owned scratch and makes one
stream write. Drain only immediately ready messages, respecting the existing
32-message/12-KiB readiness limit and 16-KiB storage where those H1 limits apply.
Do not wait to fill a batch, increase sequence depth, or enlarge queues to obtain
this efficiency. Large singletons can use the existing split fallback if the
scratch is insufficient; never retain a maximum-size buffer per idle socket
merely because the protocol permits large messages.

TCP/TLS backpressure, existing transfer ACK/pacing/window logic, cancellation,
and write deadlines remain authoritative. The carrier adds no new ACK layer.
Route keepalive through the same serialized writer: a zero-length Framer message
is a transport heartbeat/no-op (not forwarded as application data). Match the
existing peer liveness policy with read deadlines and periodic heartbeats;
do not create an unbounded ping queue or infinite ping echo. v1 has no reasoned
close control frame: cancellation, EOF, protocol error, or liveness timeout
closes the socket and releases all workers/messages exactly once. If negotiated
control messages become necessary, define them explicitly in a later version.

## Components and the RPC size incompatibility

| Component | Proposed H1+ integration | Current Framer audit |
| --- | --- | --- |
| `connect/transport.go` | Native H1 dial/receive/send carrier branch, fresh WS fallback, shared coalescer and cancellation | H3 data already uses storage; heartbeat now reuses it; one-shot H3 auth keeps `Write` |
| `server/connect/transport.go` | Accept both Upgrade names; authenticate custom path before 101; share resident lifecycle | H3 data already uses storage; heartbeat reuses it; one-shot auth replies keep `Write` |
| `server/connect/resident.go` | No H1+ negotiation here: internal Exchange TCP remains Framer | Singleton writes now lazily reuse at most 2 KiB; ready multi-message batches retain existing TCP `writev`; one-shot header keeps `Write` |
| `sdk/device_rpc_*` | Native RPC offers `urnetwork-framerxl/1`, otherwise fresh WS; browser stays WS | No current Framer call sites |
| `server/proxy/device_rpc_handler.go` | Accept `urnetwork-framerxl/1` and `websocket` after the same signed-id/device admission gate; preserve session diagnostics | No current Framer call sites |
| standalone `proxy` repository | No current Framer migration needed | No current Framer call sites |

RPC is **not** a drop-in Framer substitution. `deviceRpcSettings` currently permits
3-MiB WebSocket messages, including the one-byte forward/reverse stream tag;
Framer permits only 65,535 payload bytes. `sdk/rpc` is a logical subsystem, not
an existing directory. Relevant files are `device_rpc_transport.go`,
`device_rpc_platform_native.go`, `device_rpc_platform_js.go`, and `socket_rpc.go`.

Changing a site-local `MaxMessageLen` setting alone cannot solve this: current
Framer encodes the length as uint16 and explicitly rejects larger writes. A
site-local cap is necessary for admission but cannot change the wire format.

The RPC direction is **`FramerXl`, a separate implementation and wire type**,
with the exact custom Upgrade token **`urnetwork-framerxl/1`**:

| Type | Exact header | Configured payload cap |
| --- | --- | --- |
| Current `Framer` / `urnetwork-framer/1` | `[uint16 big-endian length][uint16 split hint]` (4 bytes total) | Explicit site limit, at most 65,535 bytes |
| Proposed `FramerXl` / `urnetwork-framerxl/1` | `[uint32 big-endian payload length]` (exactly 4 bytes total) | Independent explicit site limit, at most uint32; RPC initially keeps its existing 3-MiB cap |

FramerXl has no split-hint field. The headers have the same total size but are
not compatible and require distinct parsers/encoders selected by the successful
upgrade. Never sniff length bytes, silently change v1, or interpret missing
negotiation as XL. Normal connect H1+ attempts only `urnetwork-framer/1`.
Native SDK RPC offers `urnetwork-framerxl/1`; server/proxy's RPC endpoint should
accept it in addition to `websocket` after the same authorization gate. An
unsupported native attempt uses fresh standard WS fallback; browser/JS directly
uses WS. These are design requirements, not enabled production behavior. Future
incompatible XL changes need a separately negotiated token/capability; do not
change the meaning of `urnetwork-framerxl/1` silently. The `/1` is the protocol
version in HTTP Upgrade's protocol-name/version syntax.

FramerXl should provide the same `Read`, `Write`, `WriteBatch`, and
`WriteBatchWithStorage` API/ownership model, including one reader and one writer
concurrently, single ownership in each direction, exclusive non-overlapping
scratch, pooled-read ownership, short-write/error handling, and checked batch
size arithmetic. Convert wire lengths safely on all supported integer widths;
validate the explicit site cap before allocation. Keep independent per-direction
queue/in-flight byte budgets, bound concurrent large frames and retention, and
preserve cancellation/backpressure. A uint32 field must not imply permission to
allocate 4 GiB. Do not allocate maximum-frame-sized scratch for every idle socket;
use bounded storage with a measured large-frame path. RPC framing size and
memory admission stay endpoint-specific, not a global memory-limit increase.

Compared with FramerXl, v1 chunking avoids a new wire type and
keeps individual carrier buffers small, but adds per-chunk framing and requires
proof that tagged gob-stream readers do not depend on original message
boundaries. If whole-message reassembly is necessary, it also needs a bounded
envelope and explicit memory accounting. FramerXl preserves the existing
RPC message boundaries more directly, but can retain larger buffers unless
reading/consumption and queue budgets are deliberately bounded. Measure both
before choosing the implementation.

Stage transport H1+ on existing v1 first; leave RPC on WS until FramerXl and its
limits are implemented and qualified separately. FramerXl needs its own boundary,
malformed-length, overflow, ownership, duplex race, cancellation, and memory-bound
tests plus small/large-message benchmarks; this document does not implement it.
Never silently truncate RPC or lower its 3-MiB application limit. Browser/JS
continues to use WS with either native design. Keep separate send/receive byte
budgets and queue ownership to avoid bidirectional RPC deadlocks. Keep the
gomobile-bindable `DeviceRpcWs` interface and its richer companion separation,
or introduce a carefully compatible message-carrier interface rather than
pretending custom framing implements WebSocket control semantics.

## Nginx proxy contract and real-process tests

Nginx's HTTP tunnel mechanism is driven by a client Upgrade request and an
upstream 101 response. Upgrade and Connection are hop-by-hop headers and must
be explicitly forwarded; long-lived tunnels also need suitable idle timeouts.
The configured timeout must be longer than the heartbeat interval.
[Nginx HTTP upgrade proxying](https://nginx.org/en/docs/http/websocket.html)

Minimal shared custom/WS location (TLS termination and production auth routing
remain outside this excerpt):

```nginx
map $http_upgrade $connection_upgrade {
    default upgrade;
    ''      close;
}

location /<existing-endpoint> {
    proxy_pass http://authorized_backend;
    proxy_http_version 1.1;
    proxy_set_header Upgrade $http_upgrade;
    proxy_set_header Connection $connection_upgrade;
    proxy_set_header Host $host;
    proxy_buffering off;
    proxy_read_timeout 1h;
    proxy_send_timeout 1h;
}
```

Do not hard-code `Upgrade websocket`, strip authentication headers, add a cache
for the upgrade endpoint, or assume an HTTP/2 upstream permits this HTTP/1.1
handshake. Validate the actual deployed nginx build/config, including its TLS
frontend and load-balancer path; a direct Go loopback benchmark does not qualify
that deployment. Other intermediaries can reject unknown tokens, which is why
the WS fallback remains mandatory.

`server/connect/transport_h1plus_nginx_test.go` is a real-process protocol-seam
fixture, not a production H1+ endpoint. It starts an isolated nginx with a
minimal local authorized backend and checks custom frames in both directions,
the standard WS control, auth failure before 101, rejection/mismatch fallback,
and prefetched payload preservation. Add the exact `urnetwork-framerxl/1` token
and RPC-sized bidirectional frames to that seam matrix when FramerXl exists;
do not use the uint16 parser for that token. Use `NGINX_H1PLUS_BINARY` to explicitly
select a native build; invalid explicitly configured binaries fail. The test
skips only when its default pinned binaries are absent; a discovered but
unrunnable or incorrectly versioned build fails with an actionable prerequisite
message. It never replaces nginx with a Go proxy. CI qualification requires a
non-skipped run. The existing pinned source/build recipe is `warp/lb/Makefile`
(`make nginx_local`); foreign-architecture build artifacts are not runnable
local prerequisites. The minimal local test uses an HTTP frontend; real TLS
carrier performance is measured separately, and deployment TLS/nginx validation
is still required before rollout.

The initial actual-process run passed all five subcases on native nginx 1.31.4,
commit `11d11b5f0d3d8ace5215e1a77918e9dc219ce7db`. The source archive's SHA-256
matched the pinned `dbc96585a7ddc6f3c3a8faae9487ecdf5ad4e1e2eeb77a8b26e69d935434c9de`.
The host's existing repository-local binary was Linux/x86-64, so this run used
an isolated native build without overwriting it. Reproduce from `server`:

```sh
NGINX_H1PLUS_BINARY=/path/to/pinned/native/nginx \
  GOTOOLCHAIN=go1.27.1 GOMAXPROCS=4 go test -race ./connect \
  -run '^TestH1PlusNginxUpgradeAndFreshWebSocketFallback$' -count=3
```

## Measurements: continuous 1,200-byte packet stream

Private fixture: Go 1.27.1, darwin/arm64 Apple M4 Pro, `GOMAXPROCS=4`, real
loopback TCP/TLS 1.3, Gorilla WebSocket 1.5.3, local connect Framer and production
`WebSocketWriteBatchConn`. Both endpoints run in the measured process. Client WS
buffers are 2 KiB; server buffers are 4 KiB. Each synthetic 1,200-byte packet is
one application/carrier message, excluding actual Transfer/VPN overhead. A
64-packet patterned source ring is reused; receivers verify every payload and
message boundary. There is no application processing, Internet RTT, radio,
congestion, VPN stack, provider stack, or phone in this fixture.

Each arm transfers 1,500,000 packets per direction: 1.8 GB upload/download or
3.6 GB aggregate duplex payload (decimal). Ten paired blocks rotate direction
order and alternate mode order; the full 90-arm cohort transfers 180 million
packets/216 GB. Reported absolute rates are medians; improvements are geometric
means of paired ratios with 95% paired-block bootstrap intervals (20,000 draws).
CPU is process user+system time per delivered packet, **both endpoints**, not
sender-only CPU. CPU and throughput ratios need not equal ratios of medians.

| Direction | WS median Mb/s | H1+ median Mb/s | Throughput improvement, 95% CI | CPU reduction, 95% CI |
| --- | ---: | ---: | --- | --- |
| Upload | 15,211.72 | 24,705.43 | +62.61% [58.98%, 65.94%] | 39.01% [37.65%, 40.21%] |
| Download | 20,127.41 | 25,321.31 | +25.77% [22.80%, 28.49%] | 20.62% [18.80%, 22.17%] |
| Full duplex, aggregate | 25,101.53 | 37,245.41 | +43.60% [39.20%, 47.97%] | 31.52% [30.12%, 32.92%] |

WS CPU medians: 1,264.34 / 955.89 / 1,320.16 ns per packet (upload/download/duplex).
H1+: 775.28 / 758.51 / 881.19 ns. WS allocations: 3 / 2 / 2.5 per packet;
H1+: 1 per packet (the existing escaping four-byte Framer read header). Allocated
bytes: WS approximately 80.21 / 32.07 / 56.14 B per packet; H1+ about 4.02 B.

All modes use the same 16-KiB reusable write storage and ready-drain limits.
At 1,200 bytes, a flush contains 11 messages: 136,364 TLS writes per
unidirectional arm, 272,728 per duplex arm. The gain is not a larger coalescing
window. The bulk H1+ result already uses the existing multi-message
`WriteBatchWithStorage` API, not the new singleton optimization.

Copy audit: WS preserves its normal message buffering, per-message headers,
client masking, and final TLS-batch copy; TLS encryption adds its own work.
Framer's storage path copies each message into the batch once, then TLS consumes
it. Neither path is zero-copy. Merely calling ordinary `Framer.Write` repeatedly
through the same batch wrapper regressed download by 8.38% [6.49%, 10.10%] and
duplex by 10.69% [7.80%, 13.62%] versus WS despite improving upload; its per-message
pool operations and split writes remain costly. That direction was rejected.

Separate diagnostic 20-ms runtime sampling observed approximately 11.88 MiB WS
and 11.58 MiB H1+ peak Go runtime memory for both loopback endpoints. This is
not an absolute-memory qualification: sampling can miss spikes, the sampler
perturbs timings, and the mobile application/provider memory is absent.

### General Framer results retained independently of H1+

For sufficiently large caller-owned storage, singleton `WriteBatchWithStorage`
now uses the same one-copy/one-write encoder as a ready batch. Nil/undersized
singleton storage retains its historical split fallback. No new Framer buffer,
unsafe pointer checks, lock, or per-message allocation was added.

Ten paired 150,000-packet TLS upload arms at 1,200 bytes: 2,378.50 to
4,795.79 Mb/s median, +101.65% [98.45%, 104.71%]; CPU 7,742.85 to 3,916.74 ns
per packet, a 49.56% [48.83%, 50.26%] reduction. TLS writes fall from 300,000 to
150,000. Independent ready-batch controls were statistically neutral: upload
-0.50% [-2.54%, 1.48%], duplex +1.36% [-0.74%, 3.64%].

Bare writer microbenchmarks improve 1,200-byte singletons from 33.66 to 16.60 ns
and leave eleven-message batches around 148 ns. A 12-KiB singleton becomes
slower on a zero-cost memory writer (103.26 to 155.24 ns) because full copying
replaces half copying; reducing real stream writes is the reason to select this
API, not a claim every memory-only writer improves. Native TCP `writev` was not
faster than bounded storage for the packet singleton fixture and is not a
generic TLS/QUIC-wrapper optimization. Existing server Exchange ready-batch
`writev` stays in place.

Server Exchange's lazy 2-KiB singleton scratch pilot (five repetitions, sequential
baseline/candidate, **not** paired confidence intervals): median 64 x 1,380-byte
burst 283,541 to 147,001 ns, about 93% more throughput, unchanged allocations.
Ready-batch median 17,348 to 16,977 ns; first-singleton-plus-batch 19,752 to
18,208 ns. Receive-only, idle, oversized-only, and batch-only buffers do not
acquire singleton scratch; it never grows beyond 2 KiB.

A reader-owned reusable four-byte header eliminated one allocation in isolation,
but the combined carrier experiment had a small significant upload regression
(-1.82% [-2.79%, -0.33%]). It was not retained. This preserves the explicitly
tested one-reader/one-writer concurrency contract without introducing read
state for an unproven end-to-end gain.

Private raw artifacts and reproduction fixture for this investigation are in
`/tmp/urnetwork-h1plus-benchmark.oS1U1V` on the measurement host, including
`cohort.jsonl`, `run-cohort.sh`, `analyze.mjs`, and separate singleton/batch
controls. They are temporary, not a portable or committed acceptance harness;
promote the fixture and durable raw measurements before baselining CI.

## Deterministic qualification and rollout

Required test matrix before production activation:

- Supported/old/denied/malformed/mismatched upgrades, Connection-token parsing,
  auth state failure before 101, no cross-origin credential forwarding, bounded
  cancellation/timeouts, and fresh-socket normal masked WS fallback.
- Zero, tiny, 1,200-byte, near-storage-boundary, maximum and oversized messages;
  fragmented/coalesced reads; multiple frames in one TLS write; prefetched bytes
  after HTTP101 on each side; malformed/truncated bodies; exact payload and frame
  counts. No acceptance of body bytes on an unsuccessful upgrade.
- Sparse traffic flushes immediately. Ready batches preserve order and limits.
  Short writes, full-progress-plus-error, cancellation, pool ownership/canaries,
  read/write duplex under `-race`, and bounded idle/active/teardown retention.
- Current/old provider matrix, H1 transfer encryption/auth frames, resident
  handshakes, P2P, stream fairness, and retransmission regression controls.
- RPC over-65,535-byte values, simultaneous forward/reverse RPC, independent
  budgets, disconnect/reconnect, browser/native interoperability and gomobile
  builds before enabling the RPC custom path.
- Actual nginx process with pinned build, plus deployed TLS nginx config,
  ordinary WS control and unknown-Upgrade rejection. A skipped binary prerequisite
  is not passing qualification.

Initial rollout: production code disabled by default; land negotiation and
deterministic tests first, then server support, a small native cohort, and only
then broaden. Keep independent global/client/server kill switches that force
ordinary WS. Record bounded-cardinality carrier, selection result, fallback
reason, handshake latency, write calls, messages/bytes per flush, CPU per byte,
queue pressure, reconnects, and protocol/read/write failure classes. Never label
metrics with auth tokens, URLs, message contents, or unbounded device identities.

Acceptance uses paired host experiments plus PERFVAR/LOWBAR/MEMSTEADY on the
allowlisted real phones, including old providers and Wi-Fi/cellular/P2P. Require
no TTFB, throughput, retransmission, or recovery regression; lower CPU is useful
even where the link caps throughput, but battery benefit needs controlled device
energy measurements. The **24-MiB absolute limit applies to the iOS profile**;
measure active and burst peaks, not merely steady quiet windows. Do not enlarge
memory limits, queues, or pool retention to secure a speed win. Android/server
profiles retain their own documented limits.

Security/non-goals: this is a separately negotiated authenticated protocol, not
noncompliant unmasked WebSocket. WebSocket client masking has intermediary/cache
security motivations even with trusted application servers; do not disable it
on a WebSocket connection. Custom framing needs its own strict endpoint/auth,
cross-protocol, resource-limit, and intermediary threat review.
[RFC 6455, client masking threat](https://www.rfc-editor.org/rfc/rfc6455.html#section-10.3)
No TLS removal, unauthenticated raw TCP tunnel, weakened WS validation, packet
transport framing change, H3/DNS redesign, ACK policy change, egress affinity
change, or promised 40-Mb/s fast.com result is part of this design.
