# H1+: authenticated HTTP/1.1 upgrade to URnetwork Framer

Status: implemented, opt-in and **disabled by default**, 2026-09-22. Native H1
and native RPC can negotiate the two authenticated custom carriers below;
Connect and proxy RPC servers accept them when their independent setting is
enabled. Deterministic, real server, RPC, PERFVAR and real NGINX qualification is
recorded below. Device performance, deployed ingress and the iOS-profile
24-MiB absolute gate still gate broader rollout.

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

Implemented negotiation requirements:

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

The original private benchmark and nginx seam test use a fixed synthetic
`X-UR-Network-Auth` value solely to verify the pre-101 gate. They do not test
production JWT, revocation, or signed proxy-id authentication. The new
`TestConnectH1PlusAuthenticationBefore101` and `TestProxyDeviceRpcH1Plus` use
the production endpoint gates and validate those independently.

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
five-minute, at-most-256-origin capability cache avoids an extra RTT on
every reconnect to known-old servers. Cache keys exclude credentials and paths
and are capped at 512 bytes. Only explicit capability responses or invalid
selections are cached; EOF, timeout, 429/503, cancellation, auth and TLS failures
do not suppress a later probe. Capability misses remain distinct from
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

| Component | Implemented H1+ integration | Framer integration |
| --- | --- | --- |
| `connect/transport.go` | Native, V2-header-auth H1 dial/receive/send branch, fresh WS fallback, shared ready limits and cancellation | Compact Framer pooled reads and `WriteBatchWithStorage`; existing H3 paths retain their storage |
| `server/connect/transport.go` | Accept WS and compact Upgrade; authenticate custom path before 101; share resident lifecycle | Ready compact data and heartbeats use bounded writer storage |
| `server/connect/resident.go` | No H1+ negotiation here: internal Exchange TCP remains Framer | Singleton writes now lazily reuse at most 2 KiB; ready multi-message batches retain existing TCP `writev`; one-shot header keeps `Write` |
| `sdk/device_rpc_*` | Native hosted RPC offers XL; pinned-mTLS local RPC also supports XL; fresh WS fallback; browser stays WS | Shared FramedMessageConn, direct pooled reads, ready-only writes and existing independent mux budgets |
| `server/proxy/device_rpc_handler.go` | Accept XL and WS after the same signed-id/device admission gate; preserve session diagnostics | Same XL carrier; observation wrapper forwards pooled reads and ready batches |
| standalone `proxy` repository | No current Framer migration needed | No current Framer call sites |

RPC is **not** a drop-in Framer substitution. `deviceRpcSettings` currently permits
3-MiB WebSocket messages, including the one-byte forward/reverse stream tag;
Framer permits only 65,535 payload bytes. `sdk/rpc` is a logical subsystem, not
an existing directory. Relevant files are `device_rpc_transport.go`,
`device_rpc_platform_native.go`, `device_rpc_platform_js.go`, and `socket_rpc.go`.

Changing a site-local `MaxMessageLen` setting alone cannot solve this: current
Framer encodes the length as uint16 and explicitly rejects larger writes. A
site-local cap is necessary for admission but cannot change the wire format.

RPC uses **`FramerXl`, a separate implementation and wire type**,
with the exact custom Upgrade token **`urnetwork-framerxl/1`**:

| Type | Exact header | Configured payload cap |
| --- | --- | --- |
| Current `Framer` / `urnetwork-framer/1` | `[uint16 big-endian length][uint16 split hint]` (4 bytes total) | Explicit site limit, at most 65,535 bytes |
| `FramerXl` / `urnetwork-framerxl/1` | `[uint32 big-endian payload length]` (exactly 4 bytes total) | Independent explicit site limit, safely representable on the host; RPC keeps its existing 3-MiB cap |

FramerXl has no split-hint field. The headers have the same total size but are
not compatible and require distinct parsers/encoders selected by the successful
upgrade. Never sniff length bytes, silently change v1, or interpret missing
negotiation as XL. Normal connect H1+ attempts only `urnetwork-framer/1`.
Native SDK RPC offers `urnetwork-framerxl/1`; server/proxy's RPC endpoint
accepts it in addition to `websocket` after the same authorization gate. An
unsupported native attempt uses fresh standard WS fallback; browser/JS directly
uses WS. Both native paths require explicit opt-in. Future
incompatible XL changes need a separately negotiated token/capability; do not
change the meaning of `urnetwork-framerxl/1` silently. The `/1` is the protocol
version in HTTP Upgrade's protocol-name/version syntax.

FramerXl provides `ReadHeader`, `Read`, `Write`, `WriteBatch`, and
`WriteBatchWithStorage`, including one reader and one writer
concurrently, single ownership in each direction, exclusive non-overlapping
scratch, pooled-read ownership, short-write/error handling, and checked batch
size arithmetic. Convert wire lengths safely on all supported integer widths;
validate the explicit site cap before allocation. Keep independent per-direction
queue/in-flight byte budgets, bound concurrent large frames and retention, and
preserve cancellation/backpressure. A uint32 field must not imply permission to
allocate 4 GiB. Do not allocate maximum-frame-sized scratch for every idle socket;
use bounded storage with a measured large-frame path. RPC framing size and
memory admission stay endpoint-specific, not a global memory-limit increase.

The chosen XL implementation preserves complete existing RPC message boundaries,
so it adds no fragmentation or reassembly protocol. A connection lazily retains
at most 16 KiB of writer storage. A fitting frame or ready batch uses
`WriteBatchWithStorage` and one stream write. When a singleton exceeds supplied
storage, `FramerXl.Write` obtains its own complete four-byte-header-plus-payload
temporary buffer, copies once, performs one write, and releases the buffer on
success, short write, or error. Existing pool size classes retain only small
temporaries (up to 8 KiB in the measured configuration); larger frames are
frame-local allocations available to GC after the call. Neither the Framer nor
the connection grows its retained scratch to the maximum RPC frame size.

This follows the requested full-frame temporary-buffer policy and replaces the
initial 2-KiB-prefix/direct-tail prototype. A maximum RPC write temporarily owns
an additional 3 MiB + 4 bytes while it runs. The original pooled send message is
released by the mux only after the synchronous write completes. Send/receive
queue budgets still apply independently; they do not make this temporary copy
free or constitute an iOS memory qualification. A weak-reference regression
test proves the full-size temporary is reclaimable while the connection and
original input remain live; short-write/error tests prove one terminal write
without retry or reuse of undersized scratch.

Both compact and XL paths are implemented with independent enable settings.
FramerXl has boundary, malformed-length, overflow, ownership, duplex race,
cancellation and memory-retention tests plus small/large-message benchmarks.
Never silently truncate RPC or lower its 3-MiB application limit. Browser/JS
continues to use WS with either native design. Keep separate send/receive byte
budgets and queue ownership to avoid bidirectional RPC deadlocks. Keep the
gomobile-bindable `DeviceRpcWs` interface and its richer companion separation,
and the richer companion separation. The historical `DeviceRpcWs` name now
admits the binary FramerXl carrier; the mux uses serialized empty binary
heartbeats. FramerXl rejects WebSocket control writes and implements no masking,
compression, ping/pong or opcode protocol.

## Implementation controls and diagnostics

All enable settings default to false. Use independent controls for deployment:

| Scope | Control |
| --- | --- |
| Native Connect H1 client | `PlatformTransportSettings.EnableH1Plus`; also requires `V2H1Auth` and a compact-compatible site cap |
| Connect server | `ConnectHandlerSettings.EnableH1Plus` |
| Native SDK RPC client and local mTLS listener | `deviceRpcSettings.EnableH1Plus`; the bindable `sdk.SetDeviceRpcH1PlusEnabled` sets the default for subsequently created sessions |
| Hosted proxy RPC endpoint | `ProxySettings.EnableDeviceRpcH1Plus` |
| Process-wide emergency off | `connect.SetH1PlusDisabled(true)`; overrides all enabled endpoints/clients on future negotiations |
| Browser/JS | Always skips custom upgrade regardless of settings |

The SDK's local custom RPC carrier is available only with the existing pinned
mutual-TLS identity. Plain local RPC continues using its existing WS path.
Native hosted RPC now uses Authorization for both XL and WS; browser query
credentials remain supported. A configured HTTP proxy in Gorilla's dialer uses
the ordinary WS path rather than bypassing the proxy. Extender strategy tests
cover the existing H1 TLS/socket boundary with the custom carrier enabled.

Shared `DialFramedUpgrade`/`AcceptFramedUpgrade` preserve prefetched bytes;
`DialH1Messages` owns one custom attempt and a fresh WS fallback. The custom
handshake uses at most five seconds and at most half the remaining caller
deadline; both attempts share the original total deadline. Terminal 401/403,
redirects, certificate/hostname failures and outer cancellation do not downgrade.
The capability cache is distinct from provider/exit reputation.

`H1PlusStats.Snapshot` records bounded numeric selection, fallback-reason,
handshake-duration, payload, flush, actual stream-write and error counters.
`H1PlusStats` fields on client/server settings can isolate a test/cohort. Default
server collectors publish `urnetwork_connect_h1plus_*_total` and
`urnetwork_proxy_h1plus_*_total` with only the constant negotiated `protocol`
label. Existing queue-pressure/reconnect/transport diagnostics remain applicable.
CPU per byte is measured by the benchmark/profiler, not attributed from a
wall-clock packet timer. No credential, origin, URL or device identity becomes
a metric label.

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

`server/connect/transport_h1plus_nginx_test.go` is the original real-process protocol-seam
fixture, not a production H1+ endpoint. It starts an isolated nginx with a
minimal local authorized backend and checks custom frames in both directions,
the standard WS control, auth failure before 101, rejection/mismatch fallback,
and prefetched payload preservation. The additional
`transport_h1plus_helpers_nginx_test.go` uses the production shared helpers for
both exact tokens, including bidirectional 3-MiB XL frames and compact limits.
It covers real NGINX preservation of buffered bytes, auth-before-101 and fresh
WS fallback. The XL token uses the uint32 parser. Use `NGINX_H1PLUS_BINARY` to explicitly
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

### Implemented production helper cohort, 2026-09-22

The committed `BenchmarkH1PlusTLS1200` in
`transport_h1plus_benchmark_test.go` measures the production negotiation and
message-carrier implementations over real localhost TLS. Host: Apple M4 Pro,
darwin/arm64, Go 1.26.7, `GOMAXPROCS=3`; client WS buffers 2 KiB and server WS
buffers 4 KiB. H1+ retains the same inactive client batching wrapper supplied
by the actual strategy. Both modes have 16-KiB output scratch and identical
eleven-message ready batches. Handshake/warmup are outside the measured interval.

Ten fresh-process paired blocks alternate mode order; every direction sends
300,000 packets per arm (600,000 aggregate in duplex). All **60/60** observations
pass exact payload/order/boundary validation and the same **0.09091 TLS writes
per packet**. Percentage changes below are medians of paired ratios, with
20,000-resample paired bootstrap 95% intervals; absolute rates are separate
medians, so their ratios need not equal the paired estimates.

| Direction | WS median Mb/s | H1+ median Mb/s | Throughput improvement, 95% CI | CPU reduction, 95% CI |
| --- | ---: | ---: | --- | --- |
| Upload | 11,388.68 | 15,343.40 | +32.35% [32.01%, 43.04%] | 24.35% [23.40%, 30.90%] |
| Download | 13,875.60 | 16,265.00 | +18.68% [13.73%, 23.20%] | 16.78% [11.97%, 20.86%] |
| Full duplex, aggregate | 21,374.16 | 29,009.96 | +36.31% [32.50%, 37.71%] | 22.56% [20.93%, 22.78%] |

Process CPU medians, both endpoints: WS 1,662.5 / 1,369.0 / 1,251.5 ns per
packet; H1+ 1,231.5 / 1,157.0 / 973.95. Allocations per packet fall from
3 / 2 / 2.5 to 1 / 1 / 1; allocated bytes fall from 82 / 34 / 57 to 6 / 5 / 5.
Duplex benchmark operations contain two packets, so `B/op` and `allocs/op` are
divided by two for these figures. CPU includes both endpoints and payload
verification. These are carrier efficiency measurements, not Android energy,
Internet throughput, fast.com, or application TTFB measurements.

Reproduce the durable benchmark (use alternating fresh processes for paired
comparisons; a single `-count` command always retains the same mode order):

```sh
GOTOOLCHAIN=go1.26.7 GOMAXPROCS=3 go test -run '^$' \
  -bench '^BenchmarkH1PlusTLS1200$' -benchtime=300000x -count=1
```

The raw 60 observations, mode ordering, bootstrap method/seed, exact build
SHA-256 and result summary are in
`/private/tmp/urnetwork-h1plus-production-bench.PAZLCY/results.json` on this host.
This is a new production implementation cohort; the earlier private prototype
below used a different Go version/core count and is not its performance baseline.

### Large RPC frame policy measurement

`BenchmarkFramerXlTLSWrite` compares the initial bounded-prefix/direct-tail
prototype with the requested one-write full-frame temporary policy. Both arms
decode and verify every payload over localhost TLS; receive allocation is
included in both. Initial host medians: 64-KiB frame 25.831 to 28.355 microseconds
(9.8% more time); 3-MiB frame 0.9874 to 1.1322 milliseconds (14.7% more time).
Stream writes fall from two to one, but allocated bytes approximately double:
73.8 to 147.6 kB per 64-KiB frame and 3.158 to 6.312 MB per 3-MiB frame,
including the shared receive cost. This is a pilot, not a paired statistical
performance claim. The full-frame policy is retained as requested; fewer writes
do not offset its extra large-copy/allocation cost in this measured TLS case.

Large frames are not retained in a global pool or per-connection scratch, but
their allocation can increase transient runtime memory and GC pressure before
reclamation. RPC XL remains independently opt-in; real-device peak-memory and
large-RPC workload qualification must account for this cost. Fitting caller
storage remains allocation-free on the write side. The compact 1,200-byte H1+
cohort does not use this large XL fallback and is unaffected by the policy.

### Earlier private prototype

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

Implemented coverage and observed results (2026-09-22):

- Shared upgrade/carrier tests pass normally and under race, including malformed
  selection/body/header bounds, cancellation, prefetched bytes on both sides,
  terminal auth/TLS handling, fresh masked WS fallback, bounded cache and transient
  failure retry. Real TLS tests verify HTTP/1.1 ALPN and certificate failure.
- Framer/XL tests cover empty through 3-MiB messages, truncated/malformed lengths,
  checked arithmetic, ownership, full/short/error writes, undersized storage and
  concurrent one-reader/one-writer use. Large XL lifetime tests prove the full
  temporary can be reclaimed while the original message and connection are live.
- Client custom ready-batch tests assert eight-ACK/ordinary fairness, the
  32-message and 12-KiB drain bounds, immediate sparse writes, closed/canceled
  lanes, control filtering and exact pool ownership on errors. Existing H1
  lifecycle, receive-backpressure and strategy race tests also pass.
- Actual Connect JWT admission rejects missing, malformed, user-only and removed
  client credentials before 101; an authorized client succeeds. Encrypted
  Transfer/resident exchange, transport reform, old-provider WS fallback and H1
  extenders pass the existing stress fixture (three custom/fallback cases,
  101.344 seconds total). Focused server H1 lifecycle/admission race cohort passes.
- Actual proxy TLS endpoint rejects bad signed IDs, accepts XL and WS, and serves
  the native hosted forward RPC and reverse event over XL. RPC tests additionally
  cover simultaneous forward/reverse large values, exact 3-MiB envelopes,
  over-limit disconnect/reconnect, mTLS/local compatibility, queue-byte-budget
  cancellation and heartbeat liveness. SDK native tests pass under race and on
  Go 1.26.7; the browser skip test runs under Node/Wasm.
- `TestFullTunH1PlusCorrectnessAndOldProvider` passes the full PERFVAR TUN/IP
  stack/Connect/exchange/provider path in both custom and old-provider modes:
  exact 256-KiB upload and download, actual carrier selection and **zero**
  timeout/carrier-change/selective-gap/tail/cumulative recovery writes in each
  measured workload (4.568 seconds total). This is a correctness gate, not a
  statistical full-VPN throughput comparison.
- Both original and production-helper real NGINX suites pass under race using
  the pinned native 1.31.4 build (2.022 seconds); XL bidirectional 3-MiB frames are
  included. No Go-proxy substitution or skipped NGINX prerequisite was counted.

Repeat scoped checks from the owning repositories. Server integration tests
require the live `server/local/run-local.sh` stack and sourced `test-env.sh`:

```sh
# connect
GOTOOLCHAIN=go1.27.1 GOMAXPROCS=3 go test -race . \
  -run 'Test(Framer|Framed|ReadH1|ValidateFramed|IsFramed|DialFramed|AcceptFramed|H1Plus|DialH1|WriteH1Framed)'

# sdk
GOTOOLCHAIN=go1.27.1 GOMAXPROCS=3 go test -race . \
  -run '^TestDeviceRpcH1Plus'

# server
source ./test-env.sh
GOTOOLCHAIN=go1.27.1 GOMAXPROCS=3 go test -race ./connect \
  -run '^TestConnectH1Plus|^TestConnectH1(User|Workers|Ready)'
GOTOOLCHAIN=go1.27.1 GOMAXPROCS=3 go test -race ./proxy \
  -run '^TestProxyDeviceRpcH1Plus$|^TestDeviceRpcObserved|^TestDeviceRpcHandlerAuth$'
GOTOOLCHAIN=go1.27.1 GOMAXPROCS=4 go test ./connect/perfvar \
  -run '^TestFullTunH1PlusCorrectnessAndOldProvider$'
NGINX_H1PLUS_BINARY=/path/to/pinned/native/nginx \
  GOTOOLCHAIN=go1.27.1 GOMAXPROCS=3 go test -race ./connect \
  -run '^TestH1Plus(ProductionHelpersThroughNginx|NginxUpgradeAndFreshWebSocketFallback)$'
```

Required matrix for later deployment/device activation:

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

Initial rollout: code and deterministic/server support are implemented and
disabled by default. Enable server support, then a measured native cohort, then
broaden only after the remaining device/deployment gates. Independent
global/client/server kill switches force
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
