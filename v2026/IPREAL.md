# Provider egress transport and IP realism (IPREAL)

Originally designed on 2026-10-07, at the owner's request: "Any work done to make
client-strategy tcp/udp connections look more like chrome/other browser/os
connections should also be done for connect/ip egress to make provider egress
tcp/udp connections look more like chrome/other browser connections. Keep a
design document for this in connect/IPREAL.md." The original document was
design only. The implementation status below records the subsequently
authorized subset; the remaining numbered items are future design. It is modeled on EXTENDER.md section P:
numbered items, a verified-facts block, phases, the deterministic tests the
implementation has to pass, and a decisions list with a recommendation each.

How to review the original design. Its code claims carry `verify <file>:<lines>`. The lines
were read on connect `origin/main` at `93d5b219` (the merge of the Chrome
client hello, 2026-10-07); sdk claims were read at sdk `92006578`, server
claims at server `fe8ef986`, and gVisor claims at the fork `../gvisor` at
`c0783dba2` (connect go.mod `replace gvisor.dev/gvisor => ../gvisor`, verify
go.mod:103). A reviewer checks each `verify` against those commits; a claim
with no `verify` is design, not fact. Kernel behavior is cited as literature
with the kernel source file named, since it is not in this repository, and
every network-stack profile value in R4 is a literature value that the
implementation must confirm by capture before the profile is enabled (R4,
T14).

## Implementation status, 2026-10-07

Initially implemented the small provider-egress subset on connect `f43d155a`,
then integrated it with the reviewed client-strategy changes at `3fd67304`
and the WebRTC/camouflage merges through `840e28d5`.
The owner explicitly selected **`EgressTtlModeMirror` as the default** for both
TCP and UDP, replacing the original shadow-first recommendation for this
subset. This is TTL/hop-limit mirroring, not a complete browser TCP profile.

- `TcpBufferSettings.EgressTtlMode` and `UdpBufferSettings.EgressTtlMode`
  select `mirror`, `shadow`, or `native`. Default constructors, including the
  provider profiles, select `mirror`. Empty or unknown values in custom
  settings retain native behavior. Shadow mode reads the actual socket TTL
  and counts matches/mismatches without setting any socket option.
- IPv4 TTL and IPv6 hop limit are retained in the ordinary NAT dispatcher
  and the separate reliable-provider TCP ingress parser. TCP captures the
  initial SYN's value; UDP captures the first datagram's value before either
  the shared or legacy socket lifecycle starts. Later UDP datagrams do not
  change the flow's target, and retransmitted TCP SYNs retain the initial
  target. Values 1..255 are preserved, including low probe
  limits; zero is counted as invalid and leaves the native option intact.
- A private dial-context target reaches `ConnectSettings.NetDialer()`'s
  pre-connect hook after its existing buffer and egress-interface controls.
  Context-preserving host wrappers reach that hook too. Truly opaque dials
  get a best-effort post-connect fallback: it cannot change an already-sent
  TCP SYN, but precedes the first ordinary UDP write. Socket-option refusal
  never fails the flow; existing control-chain errors retain their behavior.
  Ordinary client-strategy dials have no provider target and are unchanged.
- `TcpBufferSettings.ProviderKeepAliveConfig *net.KeepAliveConfig` optionally
  overrides keepalive after connect, including explicit disablement. Nil
  preserves existing behavior. No unverified Chrome timing is selected:
  default `ConnectSettings` still explicitly use 5-second idle/interval and
  one probe. Provider overrides do not change client-strategy recovery.
- `LocalUserNat.EgressRealismStats()` exposes fixed-size, cumulative atomic
  counters shared across protocols, families, and dispatch shards: observed
  flows, mode, invalid TTL, pre/post-connect sockets, native TTL agreement,
  failed reads/applies, unavailable sockets, and keepalive override failures.
  These are local aggregate diagnostics, not new per-source protocol fields.
  Socket counts can exceed flow counts if dialing creates multiple sockets;
  snapshots are concurrent-safe but not atomic across all fields.

Implementation: `ip_egress_realism.go`, the platform-specific
`ip_egress_realism_sockopt_*.go`, and the wiring in `ip.go`, `net.go`, and
`ip_provider_reliable_ingress.go`. Application TLS/QUIC payloads, TCP option
layout, MSS, window scale, DF, source-port policy, and the synthetic
client-facing stack are unchanged. Client-strategy Chrome ClientHello support
remains the existing normal/resilient dialer implementation.

Deterministic regression coverage in `ip_egress_realism_test.go` exercises real
NAT TCP/UDP sockets in both address families, reliable receipt delivery,
shared/legacy UDP lifecycles, first-datagram retention, the default settings,
shadow/native modes, invalid and low TTLs, host wrappers, opaque fallback,
refused options, and preservation of existing control failures. Actual
keepalive options are checked in `ip_egress_keepalive_unix_test.go` with
Linux/Darwin-specific option constants. `ip_egress_syn_linux_test.go` uses
`TCP_SAVE_SYN`/`TCP_SAVED_SYN` on loopback to distinguish pre-connect
mirroring from post-connect fallback for ordinary/reliable IPv4/IPv6 flows.
The dedicated
`TestProviderReliableDeliveryPreservesTtlBeforeSocketCreation` drives the
reliable receipt path to the actual upstream socket; ordinary packet delivery
cannot substitute for this test. Mutation validation removed only the two
TTL-capture assignments in `ip_provider_reliable_ingress.go`: the ordinary
TCP regression still passed, while the reliable regression failed with TTL
64 instead of 123. Both assignments were restored.

Validation so far on Darwin/arm64: the complete focused suite and focused
race suite pass (`go test [-race] . -run
'TestProvider(Egress|ReliableDeliveryPreservesTtl)' -count=1`). Linux/amd64
test-binary compilation passes, including the saved-SYN oracle. Windows/amd64
test-binary compilation also passes. Linux and Windows tests were compiled,
not executed, on this Darwin host.

The bounded broader check, `go test -short ./... -timeout=180s`, was not
green: the root package reported
`TestMultiClientPeerReplacementContinuesWhileMonitorObserverStalls` failing
to form initial peers, then exhausted its package time limit during
`TestFramerSpeedup`; `durablevolume` also exhausted its package time limit;
`mls.TestPinnedToolchain` requires Go 1.26.5 but the host uses Go 1.27.1.
The peer-replacement test passed when rerun in isolation (1.148 seconds),
so its broad-run failure was not reproduced there. These results do not
establish a passing full repository suite. The focused feature and race
results above remain the validation for this change; the broader failures
were not repaired as part of the TTL/keepalive subset.

The subsequent Astra max review fixes preserve this provider subset, including
the reliable-ingress TTL regression and the mirror default. Client-strategy
ranking now observes received HTTP and H1/WebSocket payload, captures the
attempt's network/configuration, and uses expiring delivery evidence rather
than lifetime handshake success. Existing network-change notifications isolate
new paths; hosts may supply a stable private identifier to revisit prior scores.
The extender-directory fixes make release admission atomic across replicas,
retain epoch disclosure budgets across eligibility changes, sign canary channel
restrictions, publish canary-only regional DNS sets, align feed sample/stream
partitions, and retain blocked reports through same-key eviction. Their
deterministic regressions live in `net_extender_review_regression_test.go`,
`net_extender_release_admission_test.go`, `net_strategy_delivery_test.go`,
`gossip/canary_channel_test.go`, and the server's release/publisher tests.
These changes do not establish a complete Chrome wire fingerprint. Server
migrations 794 and 795 remain pending for persistent databases; validation uses
leased private test databases.

Validation of the integrated review fixes on Darwin/arm64 passed: the focused
root regression/TTL race suite, the broader HTTP/H1/extender/client-strategy
suite, the complete gossip suite with and without the race detector, the
fingerprint suite, and server release-model/DNS-publisher tests. The server
checks exercised actual PostgreSQL transactions and publication with migrations
applied only to leased private test databases. Linux/amd64 and Windows/amd64
root test binaries also compile. This is affected-suite validation; the earlier
full-repository limitations above still apply.
An isolated Go overlay reverting the regional DNS guard makes
`TestReviewDnsCanaryIsPublishedWithoutOrdinaryRegionalPeers` fail with the
original omission. The working-tree implementation was not changed by this
counterfactual check.

The follow-up parallel Astra max implementation review covers the WebRTC and
camouflage merges through `840e28d5`, as well as the remaining strategy/DNS
issues. Its fixes preserve the provider behavior above:

- WebRTC stream reads and complete writes serialize independently. Close owns
  the drain reader, joins it, and distinguishes reversible deadlines from
  association failure. Deadline changes serialize with shutdown. Answer
  delivery and waiter closure share one nonblocking lifecycle boundary, and
  carrier-owned cancellation bounds outstanding offer exchanges.
- Camouflage shutdown retires lazy resolver admission safely. Cancellation
  interrupts hello reads and the initial borrowed-site write; relay ownership
  covers cleanup before that write, and short or zero-progress writes are
  handled explicitly. Relay ownership clears the inherited hello deadline,
  including when relay idle timeouts are disabled. Partial record reads retain
  every consumed byte for fallback replay. The TCP race transfers connection
  ownership only when the result is received, so terminal answers and
  cancellation close losing dials.
- Vless delivery evidence uses a private configuration identity, while public
  family telemetry remains `vless`. Replacement invalidates evidence, retired
  selections cannot stamp a new attempt, and configuration generations reject
  late completions even when an earlier configuration returns.
- Synthesized DNS responses retain the peer's latest EDNS capability after
  queued request headers drain. Peer state is bounded, expires on the existing
  state timer, handles transitions to legacy independently per endpoint, and
  is released when its translation closes.
- The signed-record wire fields are distinct: `WebRtcClientId` remains 14,
  `RealityPublicKey` remains 15, and the unpublished `CanaryChannel` uses 16.
  Generated bindings preserve those assignments.

Permanent deterministic regressions are in
`net_extender_webrtc_lifecycle_regression_test.go`,
`net_extender_camouflage_lifecycle_test.go`,
`extender/extender_camouflage_lifecycle_test.go`,
`net_strategy_vless_identity_test.go`, and
`transport_pt_edns_state_test.go`, with signed-record wire coverage in
`extender_record_wire_test.go` and `protocol/extender_wire_test.go`.
Each root cause was reproduced against the pre-fix mechanism before checking
the corrected behavior; concurrent ownership tests use barriers or Go's virtual
clock rather than scheduler luck. Adjacent write, drain, deadline, and stale
configuration failures receive their own regression coverage.

Final follow-up validation on Darwin/arm64 passed: the combined
strategy/DNS/directory/provider regression race gate, the full WebRTC stream
and signaling race suite, extender camouflage and WebRTC integration race
suites, protocol wire tests, and the complete gossip/fingerprint suites with
the race detector. The broader affected root suite passed with `-short`
(142.616 seconds), covering HTTP/H1, extenders, client strategy, DNS codecs,
queues, packet-translation lifecycle, and provider regressions. The extender
WebRTC integration race suite passed in 23.979 seconds. Server release-model
and DNS-publisher checks passed against leased PostgreSQL databases after
adding the merged WebRTC dependency to the server module checksums.
Linux/amd64, Windows/amd64, and JavaScript/Wasm root test binaries compile;
they were not executed on this host. `git diff --check` passes in both repos.

A broader run without `-short` reached its 180-second package limit in the
DNS/QUIC stress paths. The bounded short suite skips those long stress loops;
its passing result does not certify that stress run or resolve the earlier
full-repository limitations above. Persistent server migrations 794 and 795
remain pending; private test migration application does not deploy them.

Continuation checks: the complete provider-egress TTL/keepalive suite also
passes on Linux/arm64 in an isolated local Docker container, with no skipped
tests. This executes `TestProviderEgressSavedSynCarriesClientTtl` against the
kernel's saved SYN headers for ordinary/reliable IPv4/IPv6 flows and opaque
dials, rather than merely compiling that oracle. Windows and Linux/amd64
remain compilation-only checks. Standalone `TestPtDnsEncodeDecode` passes all
32 iterations without retries (85.561 seconds). Combined with the affected
short suite's 142.616 seconds, that test alone exceeds the earlier combined
180-second budget; the aggregate timeout does not establish a DNS defect.

These correctness fixes do not complete the phased server WebRTC acceptance,
production rendezvous wiring, server publication of the camouflage key, SDK
provider activation, or population of the borrowed-domain resource. Those
rollout dependencies remain explicit in `EXTENDER.md`; an enabled local carrier
or passing fixture does not establish end-to-end production availability.

Not implemented: the R4 profile registry/capture corpus, full SYN fingerprint
matching, MSS/window clamping, UDP DF/PMTU feedback, source-port policy,
Source P signaling, B-lite/B-full, gVisor profile encoders, and per-source
protocol publication. No full phase in the original plan is marked complete
by this narrower subset.

Review corrections to the remaining design:

1. A socket has one `SO_MARK`; B-lite must preserve the host's routing
   exclusion identity and coordinate a discriminator with host policy rather
   than overwrite that mark with the distinct value proposed in R10.1.
2. Go's socket `Control` hook precedes bind/connect. An automatically
   allocated source port is unavailable there; the proposed B-lite SYN-table
   registration requires a reservation or another correlation mechanism.
3. nftables queue `bypass` covers an absent queue listener. Queue overflow
   needs `NFQA_CFG_F_FAIL_OPEN`; a pure-byte test cannot verify kernel overflow
   behavior. A live stalled listener also needs an explicit lifecycle policy.
4. R3.5's per-source profile ID alone cannot distinguish synthesized flows
   from device flows sharing that source. Source P needs an explicit selector
   and ordering before the initial SYN.
5. `applyPathMtuFor` consumes client-side ICMP feedback to reduce the return
   path's packet size. It does not construct a client-directed ICMP error for
   upstream `EMSGSIZE`; that future work needs its own constructor/MTU source.
6. Port-range policy must run before bind. Applying it after `ListenUDP`
   cannot alter the already-allocated source port.

## R0. Verified facts

- R0.1 The provider egress is `LocalUserNat`: "The UNAT emulates a raw socket
  using user-space sockets" (verify ip.go:27; the type at ip.go:757). It
  terminates the client's TCP in user space and re-originates the stream to
  the destination through `self.tcpBufferSettings.DialContext(self.ctx, "tcp",
  self.IpPath().DestinationHostPort())` (verify ip.go:5459-5463), after the
  client's SYN has arrived and before the synthetic SYN-ACK is sent ("connect
  to upstream before sending the syn+ack", verify ip.go:5447; the SYN wait
  loop at ip.go:5395-5425, `initializeSynWithLock` called at ip.go:5416, the
  SYN-ACK built at ip.go:5421 and delivered at ip.go:5530). UDP opens one
  socket per flow through `self.udpBufferSettings.DialContext(self.ctx, "udp",
  ...)` in `openSocket` (verify ip.go:3544-3577), called from the shared
  lifecycle at sequence creation (verify ip.go:3027, ip.go:3591-3592) or from
  `Run` before the first datagram is read (verify ip.go:3641). ICMP has its
  own backend (R0.9).
- R0.2 `DialContext` here is `ConnectSettings.DialContext` (verify
  net.go:217-241), embedded in `TcpBufferSettings` and `UdpBufferSettings`
  (verify ip.go:2176 for UDP; the embedded field closes the TCP struct that
  starts at ip.go:4011). It builds a fresh `net.Dialer` per dial through
  `NetDialer()` (verify net.go:231), which is `egressDialer(&net.Dialer{...,
  Control: self.DialControl})` (verify net.go:282-302, `Control:` at
  net.go:300). A host-supplied `DialContextSettings.DialContext` replaces that
  dialer entirely (verify net.go:228-229), so neither `DialControl` nor the
  egress binding runs on it ("Not applied to a host-supplied
  DialContextSettings dial", verify net.go:115-118).
- R0.3 The egress Control hook: `egressControl` applies the egress interface
  binding at socket creation (verify egress.go:41-56), `egressDialer` chains it
  after any existing `Control` (verify egress.go:58-70), `applyEgress` applies
  it to an already-created socket (verify egress.go:72-85). The binding is a
  no-op unless an egress index is set, and is implemented only on Windows
  (`IP_UNICAST_IF`, verify egress_windows.go:11-19 and :43-83, where a failure
  fails the dial, :76-82) and macOS (`IP_BOUND_IF`, verify
  egress_darwin.go:20-48); elsewhere it is inert (verify egress_other.go:5-10).
  The fwmark is NOT applied by this hook: on urnetwork-linux it is stamped by
  a cgroup-BPF program at socket creation (verify egress_resolver_linux.go:84-90),
  and the one in-process `SO_MARK` is on the resolver's socket, swallowing
  `EPERM` because it needs `CAP_NET_ADMIN` (verify egress_resolver_linux.go:236-260).
  The task file's "already pins the socket to the egress interface/fwmark" is
  therefore half right: interface on Windows/macOS, fwmark by BPF, nothing on
  Android/iOS/plain Linux.
- R0.4 A pre-connect socket-option hook already ships on the TCP egress leg:
  `DefaultTcpBufferSettingsWithBufferSize` sets
  `tcpBufferSettings.ConnectSettings.DialControl = upstreamSocketBufferControl(...)`
  (verify ip.go:527-533), which sets `SO_SNDBUF`/`SO_RCVBUF` before connect
  only when the request beats the kernel's autotuning ceiling (verify
  upstream_socket_buffer.go:72-92; THROUGHPUTFIX.md §15.2 at
  THROUGHPUTFIX.md:1222-1259). Its header states the fact this design leans
  on: "A receive pin is applied only before connect, through the dialer's
  control hook, because a post-connect receive pin freezes the window clamp at
  its SYN-time value on some kernel generations" (verify
  upstream_socket_buffer.go:14-17). After connect, `configureUpstreamTcpConn`
  sets keepalive and no-delay and, on an opaque host dial, only the send pin
  (verify ip.go:5508-5517, upstream_socket_buffer.go:94-112; the guard test
  upstream_socket_buffer_opaque_dial_test.go:11-40).
- R0.5 The client-facing emulated stack is NOT the egress leg and nothing of it
  reaches the destination: `TcpBufferSettings.WindowScale`, `MinWindowSize`,
  `InitialWindowSize`, `MaxWindowSize` (verify ip.go:4136-4145) and their
  defaults (verify ip.go:467-535) size the synthetic SYN-ACK and window
  toward the CLIENT; `synAckWithSequence` writes that SYN-ACK's MSS, timestamp
  and window-scale options (verify ip.go:6879-6940); the synthesized IPv4 and
  IPv6 headers toward the client carry TTL/hop limit 64, IP id 0 and flow
  label 0 (verify ip.go:2454-2474, :2465-2466; ip.go:2478-2490, :2486-2487).
  This document changes none of that.
- R0.6 The client's own SYN travels verbatim through the tunnel: the envelope
  is a raw packet, "the provider is expected to do its own parsing to confirm
  the data verbatim" (verify protocol/ip.proto:7-16; decoded at ip.go:9704-9705).
  The provider parses it with `parseTcpPacket`, which keeps the flags, the
  16-bit window and the raw options slice `tcp.options =
  transport[20:headerByteCount]` (verify ip.go:2330-2352, :2351), and
  `parseTcpOptions`, which extracts MSS, window scale (clamped to 14) and
  timestamps (verify ip.go:2360-2406); `initializeSynWithLock` consumes them
  into `receiveWindowScale`, `enableTimestamp`, `peerMss`, `receiveWindowSize`
  (verify ip.go:5261-5293). The IP TTL (IPv4 byte 8, IPv6 byte 7) and the DF
  flag (IPv4 bytes 6-7) are NOT retained for TCP or UDP: `parseIpv4` returns
  protocol, addresses and the transport slice only, and its fragment check
  lets "df and the reserved bit pass" (verify ip.go:2251-2273, :2263-2266;
  `parseIpv6` ip.go:2282-2310). They ARE retained for ICMP (`ttl uint8`,
  verify ip_icmp.go:61-62) and passed to the echo socket: "ttl passthrough, so
  ttl-limited probes do not dishonestly reach the destination. best-effort: a
  platform that refuses the option still sends with its default" (verify
  ip_icmp_egress_unix.go:84-101). That ICMP path is the in-tree precedent for
  R3's mirror.
- R0.7 Flow identity is the client 4-tuple plus the source:
  `BufferId4{source, sourceIp, sourcePort, destinationIp, destinationPort}`
  (verify ip.go:2054-2060), one egress socket per flow (UDP `sharedSocket`,
  verify ip.go:3264; TCP `socket` per `TcpSequence`, verify ip.go:5449). The
  first UDP datagram is in hand when the socket opens (`UdpSendItem.ipPacket`,
  verify ip.go:3862-3869; `StreamState.ipPath` is "primed by the first call,
  which happens at sequence setup (DialContext)", verify ip.go:3871-3885).
- R0.8 Tunnel MTU: `DefaultTunnelMtu = 1280`, "the interface mtu every native
  tunnel and the gVisor tun configure" (verify ip.go:45-53), exported to the
  apps as `GetDefaultTunnelMtu` (verify sdk/device.go:531-537). A client OS
  therefore advertises MSS 1240 (IPv4) or 1220 (IPv6) in the SYN it sends into
  the tunnel (derived: 1280 minus 40 or 60; confirm by capture, T14). The
  provider-side packetizer size `DefaultMtu = 1100` is unrelated to the egress
  SYN (verify ip.go:35-43). Today's egress SYN advertises the provider host's
  own MSS, which is the right value; R3.4 is about not replacing it with the
  tunnel-derived one.
- R0.9 ICMP egress is one unprivileged datagram ICMP socket per flow (verify
  ip_icmp_egress_unix.go:15-27). It already mirrors the client's TTL (R0.6) and
  needs nothing from this design.
- R0.10 The sibling client-strategy work is merged: `ConnectSettings.TlsClientHelloFingerprint`
  (verify net.go:176-184; default `TlsClientHelloFingerprintChrome`, verify
  net.go:106; values `"chrome"` and the kill switch `"go"`, verify
  net_tls_hello.go:55-59; profile `utls.HelloChrome_133`, verify
  net_tls_hello.go:61-64), applied to the normal and resilient dialers (verify
  net_http.go:322, net_resilient.go:95). Its commit says "The extender, VLESS
  and alt dialers, DoH and the streaming post are unchanged" (verify commit
  871a2c5d message). It changes the TLS layer only: its diff adds no
  `Control` hook, `IP_TTL`, `TCP_MAXSEG` or socket option of any kind (verify
  `git show 871a2c5d` touches net.go, net_http.go, net_resilient.go and the
  new net_tls_hello*.go files only). So today "client-strategy realism" is TLS,
  and the TCP/UDP/IP layer is open on BOTH the client-strategy sockets and the
  provider egress. This document specifies that layer once and applies it in
  both places (R5.7).
- R0.11 A fingerprint-name vocabulary exists for the VLESS strategy:
  `"chrome"`, `"firefox"`, `"safari"`, `"ios"`, `"android"`, `"edge"`,
  `"360"`, `"qq"`, `"randomized"`, `"random"` (verify vless_reality.go:51-84).
  R4 names network-stack profiles so each TLS fingerprint name maps to one.
- R0.12 The mux Tun is a user-space tun on the gVisor netstack (verify
  tun.go:3-7, `stack.New` at tun.go:267-270). Its DoH cache dials through
  `Tun.DialContext` with `dohTun: true` (verify tun.go:1000-1018, :1015-1018;
  `DialContextSettings.dohTun` at net.go:210; `Tun.DialContext` at tun.go:1380),
  so the SYN a DoH flow sends INTO the tunnel is gVisor's: "Emulate linux
  option order" (verify ../gvisor pkg/tcpip/transport/tcp/connect.go:772-814,
  `makeSynOptions`), TTL 64 (verify ../gvisor pkg/tcpip/network/ipv4/ipv4.go:58-59).
  The DoH client is `net/http.Transport` over crypto/tls (verify
  net_http_doh.go:356, :377), i.e. Go's hello (R0.10).
- R0.13 Host wiring. The sdk builds the provider NAT with
  `providerLocalUserNatSettings` (verify sdk/device_local.go:4227-4255),
  optionally installing `DeviceLocalSettings.ProviderDialContextSettings` on
  both the TCP and UDP settings (verify sdk/device_local.go:652-659,
  :4248-4252, call at :5884-5888; the fallback NAT at
  sdk/device_local_provider.go:251-258). The server wraps every dial in a
  `DialContextSettings` (`ForceIPv4ConnectSettings`, verify server/sdk.go:30-41).
  The server wrapper delegates to a copied `base.DialContext`, so a normal
  base still reaches `NetDialer` and its control hook (verify server/sdk.go:30-41).
  A host-supplied dial is opaque only if its implementation bypasses that
  hook; the presence of `DialContextSettings` alone does not establish this.
- R0.14 Memory scaling: `MemoryScaledByteCount` and `MemoryScaledCount`
  (verify memory_budget.go:105-117); the provider flow cost model
  `providerUdpFlowByteCount = 2 KiB`, `providerTcpFlowByteCount = 8 KiB`
  (verify ip.go:579-580) sizes the provider caps from the memory target (verify
  ip.go:634-671).
- R0.15 Socket-option constants in the pinned `golang.org/x/sys v0.47.0`
  (verify go.mod): Linux `IP_TTL`, `IPV6_UNICAST_HOPS`, `TCP_MAXSEG`,
  `TCP_WINDOW_CLAMP`, `IP_MTU_DISCOVER`/`IP_PMTUDISC_DO`,
  `IPV6_MTU_DISCOVER`/`IPV6_PMTUDISC_DO`, `IP_LOCAL_PORT_RANGE` (0x33),
  `TCP_SAVE_SYN` (0x1b), `TCP_SAVED_SYN` (0x1c), `SO_MARK`; Darwin `IP_TTL`,
  `IPV6_UNICAST_HOPS`, `TCP_MAXSEG`, `IP_DONTFRAG`, `IPV6_DONTFRAG`; Windows
  `IP_TTL`, `IPV6_UNICAST_HOPS`. Not exposed: Windows `IP_DONTFRAGMENT`/
  `IPV6_DONTFRAG`, Linux `IPV6_FLOWLABEL_MGR`/`IPV6_FLOWINFO_SEND`; those take
  local constants the way egress_windows.go:16-19 defines `ipUnicastIf = 31`
  (verify x/sys zerrors files in the module cache).
- R0.16 Test-harness precedents: a loopback origin, a crafted SYN through
  `LocalUserNat.SendPacket`, and the `afterUpstreamConnectForTest` seam that
  hands the test the socket the provider actually proxies through (verify
  ip.go:4186, :5518-5520; ip_upstream_flow_buffer_linux_test.go:26-93); a
  `getsockopt` through `rawConn.Control` with a raw syscall (verify
  ip_udp_socket_drops_linux.go:12-30); per-platform sockopt files
  (upstream_socket_buffer_sockopt_{unix,windows,other}.go).
- R0.17 The egress resolver files (egress_resolver_linux.go,
  egress_resolver_windows.go, egress_resolver_other.go; the design in
  egress_dial.go:13-38) are the provider's CONTROL-PLANE name resolution, the
  escape for the process's own platform dials around the tunnel it provides
  (verify egress_dial.go:64-87, :107-117). They are never on a client flow's
  packet path (R7).
- R0.18 Kernel facts used below, literature (Linux `net/ipv4/tcp_output.c`
  `tcp_select_initial_window`, `tcp_syn_options`; confirm on the reference
  kernel): the SYN option layout is fixed per kernel family and not settable
  per socket (no per-socket switch for timestamps or SACK on Linux); the SYN
  window is the receive space quantized to a multiple of the MSS and capped at
  65535; the window scale is `clamp(ilog2(min(max(space, tcp_rmem[2],
  rmem_max), window_clamp)) - 15, 0, 14)`, so `TCP_WINDOW_CLAMP` set before
  connect can only LOWER it; `TCP_MAXSEG` set before connect changes the
  advertised MSS and is capped at the interface MTU minus headers; an explicit
  `SO_RCVBUF` locks receive autotuning (the §15 rule, R0.4).

## R1. What the destination sees today, by layer

The destination's view of one client flow is three layers from two origins.
From the client, end to end and untouched by the provider: the TLS
ClientHello (JA3/JA4), the HTTP/2 SETTINGS and header order, the User-Agent,
QUIC's transport parameters and Initial, every application byte. From the
provider host's kernel: the TCP SYN (option layout, timestamps, SACK, window
scale, window, MSS), the IP TTL or hop limit, the IP id behavior, the DF bit on
UDP, the ephemeral source port, the IPv6 flow label, keepalive timing, the
retransmission schedule. The inner leg (R0.5) reaches nobody.

The tell is the pairing: a Chrome-on-Windows hello and User-Agent over a SYN
whose layout and TTL are a Linux server's. p0f-class tools key the SYN layout
and the TTL distance to an OS and flag a User-Agent that disagrees; the JA4+
suite pairs JA4 (TLS) with JA4T (TCP: `window_options_mss_wscale`). Which
bot-management stacks actually consult the TCP layer is not something this
document can verify and it does not claim it; the design makes the egress
consistent so that whoever checks finds nothing. The TLS and HTTP layers are
already the client's own, so the egress is the only inconsistent layer, and
the ClientHello itself is never rewritten (it is end to end).

Two things the design keeps in proportion. First, the SYN is sent before the
provider has seen one byte of the client's application data (R0.1), so any
scheme that needs the ClientHello to choose the SYN cannot work without
delaying the dial (R3.3). Second, "unknown" is not "wrong": a SYN that matches
no OS signature is weaker evidence than a SYN that matches the wrong OS, which
is what decides R3.6/D3.

## R2. Two fidelity levels

- R2.1 Level A, kernel socket plus socket options from the Control hook. No
  privilege, every platform, the provider keeps the kernel's TCP for data. It
  controls: TTL/hop limit (`IP_TTL`, `IPV6_UNICAST_HOPS`; all three platforms);
  MSS (`TCP_MAXSEG` before connect; Linux and Darwin; not settable on
  Windows); window scale on Linux (`TCP_WINDOW_CLAMP` before connect, lowering
  only, R0.18; R5.3 for the value); UDP DF (`IP_MTU_DISCOVER=DO`/
  `IPV6_MTU_DISCOVER=DO`, `IP_DONTFRAG`/`IPV6_DONTFRAG`, Windows
  `IP_DONTFRAGMENT` by local constant); the ephemeral port range on Linux 6.3+
  (`IP_LOCAL_PORT_RANGE`, per socket, unprivileged; R5.5); keepalive idle and
  interval (R5.6). It cannot control the SYN option LAYOUT, timestamp
  presence, SACK presence, the IP id policy, the timestamp clock, the ISN, ECN
  setup or the retransmission schedule, and it cannot raise the window scale
  above what the host's receive ceiling yields. The SYN window field is left
  to the host (R5.4): forcing it needs a pre-connect `SO_RCVBUF`, which locks
  autotuning and costs download throughput, the §15 finding (R0.4), so this
  design never pins the receive buffer for realism.
  What Level A achieves therefore depends on the provider host's kernel
  family. A Linux provider natively emits the Linux/Android layout
  (`2-4-8-1-3`, timestamps and SACK on), a macOS provider the Apple layout,
  a Windows provider the Windows layout (R4). When the target profile's family
  equals the host's family, Level A reaches a full JA4T match wherever the
  host's window and window-scale ceilings permit; across families it reaches
  the TTL, the MSS and (Linux) the window scale only: a partial match.
- R2.2 Level B, full match. Two constructions, both Linux only and both
  privileged, both degrading to Level A:
  - B-lite, SYN rewrite on the host path (R10.1). The egress socket stays the
    kernel's; only its outgoing SYNs are diverted through an NFQUEUE and
    rewritten in user space to the profile's layout: option order and padding,
    timestamps dropped or kept, SACK-permitted dropped or kept, and the window
    field lowered. It needs `CAP_NET_ADMIN` for the queue and one nftables
    rule, which the urnetwork-linux daemon can hold (it already attaches
    cgroup-BPF, R0.3). It reaches the full JA4T and the p0f layout for every
    profile, with the MSS and window scale still set by Level A because the
    kernel must agree with what the wire says (R10.1). It does not reach the
    IP id policy, the timestamp clock rate or the ISN.
  - B-full, a user-space egress stack over a raw link (R10.2): the gVisor fork
    with a profile-driven SYN encoder (its `makeSynOptions` is the one
    function, R0.12), an AF_PACKET link endpoint, and a kernel rule so the
    host does not RST the stack's flows. It needs `CAP_NET_RAW`, controls
    everything including the IP id, timestamp clock and ISN, and costs a
    second TCP implementation on the egress leg with its own memory and
    throughput profile.
- R2.3 Build order (D1). Level A first, on every provider: it closes the TTL
  tell everywhere, reaches the full match on same-family pairs, and is the
  mechanism B-lite still needs for MSS and window scale. B-lite second, on
  capable Linux providers, gated by a start-time capability probe and
  fail-open per flow (R10.1). B-full is not recommended now; it is kept as the
  escalation if measurement shows the IP id, timestamp clock or ISN being
  checked, which nothing in hand shows. Degradation is one direction: B-full
  unavailable means B-lite, B-lite unavailable (no capability, queue bind
  failure, rule install failure, queue full) means Level A for that flow, and
  Level A never fails a dial (R5.8). Realism never costs connectivity.

## R3. Consistency: where the egress learns its target

The target of a flow is the network-stack profile its SYN should match. Three
sources were considered.

- R3.1 Source M, mirror the client's own SYN. The tunnel already carries the
  client OS's real SYN (R0.6), and the tun is hop zero, so its TTL is the OS's
  initial value (64 for Linux, Android, macOS and iOS; 128 for Windows) and its
  options are the OS's own layout, timestamp and SACK choice, window scale and
  window. For device traffic this is the ground truth of the very stack that
  also produced the User-Agent and the ClientHello, so matching it is
  consistent by construction, needs no OS table and no signaling, and is
  exactly what a real NAT does: a NAT rewrites addresses and leaves TCP
  options alone. The same applies to UDP: the DF bit and TTL of the client's
  datagram are in the tunneled packet (R0.6). Failure modes: (i) the MSS is
  tunnel-derived, 1240/1220 (R0.8), a VPN tell if copied, so it is normalized
  (R3.4); (ii) the window of some OSes is derived from the MSS, so a copied
  window with a normalized MSS is itself inconsistent, handled in R3.4; (iii)
  a phone's window scale may exceed what the provider host can emit (Level A
  cannot raise it, R2.1), counted as degraded; (iv) a client whose packets
  come from a user-space stack rather than an OS (the mux Tun's DoH, R0.12; a
  hosted or synthesized device) mirrors that stack's SYN, which is a gVisor
  layout, not a browser's, which is what Source P is for; (v) a malformed or
  implausible SYN (window scale over 14, MSS under 536 or over the host
  maximum, TTL outside 32..255) falls back to native, counted (R8).
- R3.2 Source P, an explicit profile signaled by the client. The client knows
  its platform and its chosen TLS fingerprint, so it can name the network-stack
  profile its flows should wear, and the provider applies that profile instead
  of the SYN. Needed exactly where M is wrong, R3.1(iv): flows that connect
  itself originates with a browser hello from a stack that is not an OS. The
  signal rides a new message, not a per-packet field (R3.5). Failure modes:
  (i) an old provider ignores the unknown message and mirrors, non-fatal;
  (ii) an unknown profile id on the provider falls back to mirror, counted;
  (iii) a client that declares a profile its device traffic contradicts (a
  Windows profile while its apps send Safari hellos) would make the egress
  wrong for those flows, so P governs only synthesized flows and is never
  applied to a flow whose SYN came from the device tun unless the owner opts
  in (R11); (iv) version skew between the profile registry on client and
  provider is handled by stable numeric ids that are never reused.
- R3.3 Source H, infer the OS from the first ClientHello of each flow. The
  provider already parses the ClientHello for the SNI (`IpPath.ServerName`,
  verify ip.go:10630). Extending that parse to JA4 and mapping JA4 to an OS
  fails on two facts. First, order: the egress SYN is sent before the inner
  handshake completes and long before the hello arrives (R0.1), so H can
  only drive the SYN if the dial is delayed until the first data, which costs
  an inner round trip per flow and breaks server-first protocols, which the
  code explicitly supports ("The upstream may be server-first (SMTP/587, SSH,
  IMAP)", verify ip.go:5523-5526), and every non-TLS flow. Second,
  ambiguity: Chrome's hello is the same on Windows, macOS, Linux and Android
  (one BoringSSL configuration), Firefox's is the same everywhere, so the most
  common hellos do not determine the OS at all; only Safari (Apple family)
  and OkHttp (Android) do. H is therefore kept as an audit only: a counter of
  flows whose hello family contradicts the family of the SYN the flow was
  given, which is how a misdeclared Source P or a spoofing client shows up
  (R11.3, T9).
- R3.4 MSS and window normalization under M. The egress MSS is never the
  client's: it is the profile's canonical value, 1460 for IPv4 and 1440 for
  IPv6 (an Ethernet or Wi-Fi link, the dominant case for every profile in R4),
  capped at what the provider host can carry (the kernel caps `TCP_MAXSEG` at
  its interface MTU minus headers, R0.18; a lower cap is counted as degraded,
  never an error). Because a Linux-family window is quantized to the MSS
  (R0.18) and the mirrored window was quantized to 1240, the window target is
  recomputed from the profile's window rule (R4: fixed, or a multiple of the
  MSS capped at 65535) rather than copied; under Level A it is informational
  anyway (R5.4), and under B-lite it is applied downward only (R10.1).
- R3.5 The signal for Source P. A new protocol message
  `IpNetworkStackProfile { uint32 profile_id = 1; }` in protocol/ip.proto with
  its own `MessageType`, sent by the client to a provider once after the route
  to that provider is established and again whenever the chosen profile
  changes, stored by the provider per source (`TransferPath`) in a bounded
  table that is dropped with the source's retirement (the per-source maps
  at ip.go:797-803 are the lifecycle precedent, verify). It is NOT a field on
  `IpPacketToProvider`: a per-packet field would be parsed on the hot path for
  every packet to carry a value that changes once per session. An unknown
  message type on an old provider is skipped as today. The id registry is
  R4.1; 0 means unspecified, i.e. mirror. This proposal is incomplete: a
  per-source value alone cannot enforce R3.2's synthesized-only scope for
  mixed flows. Specify a flow selector or separate source identity, and
  delivery ordering before the initial SYN, before implementation.
- R3.6 Recommendation (D2, D3). Source M is the default for every flow that
  arrives from a device tun; Source P for flows connect originates from a
  user-space stack with a browser hello, and as an owner override; Source H
  is audit only. Reasons: M is exact and free and covers the traffic that
  matters, P is needed exactly where M mirrors a non-OS stack, and H cannot
  drive the SYN. Across families, where Level A can match only the TTL, the
  MSS and perhaps the window scale, the recommendation is to apply what can
  be applied: the result matches no OS signature ("unknown") rather than
  reading as a Linux server under a Windows hello, and it passes a TTL-distance
  check; D3 records the alternative (apply only when the whole layout can be
  matched) and the shadow measurement that decides it.

## R4. Network-stack profiles

- R4.1 The registry `EgressStackProfile` (egress_profile.go, R5.1): a stable
  numeric id, a name, a kernel family (`linux`, `windows`, `darwin`), and the
  fields the appliers consume: `Ttl` (IPv4 TTL and IPv6 hop limit), `TcpOptionLayout`
  (the ordered option kinds including NOPs and EOLs, the JA4T options string),
  `Timestamps`, `SackPermitted`, `WindowScale`, `SynWindowRule` (`fixed(N)` or
  `mssMultiple(k)` capped at 65535), `Mss` (canonical, 1460/1440), `UdpDontFragment`
  (the datagram default when the client's own DF bit is not available, i.e.
  under Source P), `EphemeralPortRange`, `KeepAliveIdle`/`KeepAliveInterval`,
  `Ecn` (`off` or `setup`), `Ipv6FlowLabel` (`zero` or `random`). Ids: 0
  unspecified (mirror), 1 `chrome-windows`, 2 `chrome-android`, 3
  `safari-ios`, 4 `chrome-macos`, 5 `chrome-linux`, 255 `native` (the host as
  it is, the kill switch). The TLS fingerprint names map onto them: `"chrome"`
  resolves by platform to 1, 2, 4 or 5; `"safari"` and `"ios"` to 3;
  `"android"` to 2; `"firefox"` and `"edge"` to the platform's Chrome profile
  (same kernel, same SYN); anything else to 0.
- R4.2 Reference values. These are literature values (p0f signatures, the
  JA4T examples published with the JA4+ suite, kernel defaults). Each MUST be
  confirmed by a capture from a real device of that profile on an Ethernet or
  Wi-Fi link before the profile is enabled (phase 1, T14); a value this doc
  marks `?` is one the literature does not fix.
  - `chrome-windows` (Windows 10/11): TTL 128; layout `2-1-3-1-1-4` (MSS, NOP,
    WS, NOP, NOP, SACK-permitted); timestamps off; SACK on; window scale 8;
    SYN window 64240 (JA4T `64240_2-1-3-1-1-4_1460_8`); ports 49152-65535;
    keepalive 2 h idle unless the app sets it (Chrome sets 45 s, `?`); ECN off;
    IPv6 flow label random.
  - `chrome-android` (Android, Linux kernel): TTL 64; layout `2-4-8-1-3` (MSS,
    SACK-permitted, timestamps, NOP, WS); timestamps on; SACK on; window scale
    8 or 9 by device (`?`); SYN window 65535 (JA4T `65535_2-4-8-1-3_1460_8`);
    ports 32768-60999; keepalive by the app; ECN off; flow label random.
  - `safari-ios` (iOS; also macOS Safari, same XNU family): TTL 64; layout
    `2-1-3-1-1-8-4-0-0` (MSS, NOP, WS, NOP, NOP, timestamps, SACK-permitted,
    EOL, EOL); timestamps on; SACK on; window scale 6; SYN window 65535 (JA4T
    `65535_2-1-3-1-1-8-4-0-0_1460_6`); ports 49152-65535; ECN `setup` on some
    networks (iOS negotiates ECN opportunistically, `?`, so the profile carries
    it as a field and the capture decides); flow label `?`.
  - `chrome-macos`: the Apple family values above with ECN off.
  - `chrome-linux` (desktop Linux, ChromeOS): TTL 64; layout `2-4-8-1-3`;
    timestamps on; SACK on; window scale 7 (stock `tcp_rmem[2]` of 6 MiB);
    SYN window 64240 (stock `tcp_rmem[1]`; 65535 on hosts with a larger
    initial buffer, JA4T `64240_2-4-8-1-3_1460_7`); ports 32768-60999; ECN
    off; flow label random.
- R4.3 QUIC and UDP framing. The QUIC transport parameters, the Initial's
  TLS hello, the connection ids and the padding ride the client's payload and
  are forwarded verbatim (the provider does not reframe datagrams), so the
  only provider-owned fields of a QUIC flow are the IP envelope: TTL, DF, IP
  id, ECN bits, source port, flow label. Chrome sets DF on its QUIC sockets on
  every platform (`?`, confirm by capture), so under Source M the mirrored DF
  bit is the right value and under Source P `UdpDontFragment` is true for
  every browser profile. One UDP socket per client 4-tuple (R0.7) already
  matches a browser's one socket per QUIC connection; a client port change
  (QUIC migration) is a new flow and a new provider port, as it is behind any
  NAT.
- R4.4 Capture procedure (phase 1 deliverable). For each profile: one
  reference device on a plain Ethernet or Wi-Fi link, a capture of the SYN
  and of one QUIC Initial toward a test server the team controls, sanitized to
  RFC 5737/3849 addresses, stored as hex fixtures under the package's test
  data with the device, OS build and browser version in the fixture's name;
  T14 derives each profile's JA4T from its fixture and fails if the table
  disagrees. A profile with no fixture is not enabled.

## R5. Level A: where it plugs in and what it does

- R5.1 Files. `egress_profile.go` (the registry R4.1, the per-flow target
  `egressSynTarget`, the mirror derivation R5.2 and its validation R8, the
  counters R11.3), `egress_profile_linux.go`, `egress_profile_darwin.go`,
  `egress_profile_windows.go`, `egress_profile_other.go` (the appliers, split
  as egress_*.go and upstream_socket_buffer_sockopt_*.go are split, R0.16),
  and the B-lite files in R10.1. No change to net_http.go or net_resilient.go
  (the in-flight coordination rule): they consume `ConnectSettings.DialControl`
  through `NetDialer()` already (R0.2), which is where the client-strategy
  applier lands (R5.7).
- R5.2 The per-flow target. At `initializeSynWithLock` (verify ip.go:5261)
  the sequence derives `egressSynTarget{ttl, hopLimit, mss, windowScale,
  synWindow, optionLayout, timestamps, sackPermitted, ecnSetup, dontFragment,
  source}` from the parsed SYN (R0.6) under the sequence mutex, as the
  existing fields are, and keeps it on the `TcpSequence` (verify ip.go:4944)
  for the dial and for the counters. `parsedTcp` and `parsedUdp` gain `ttl
  uint8` and `dontFragment bool` filled by the callers of `parseIpv4`/`parseIpv6`
  from the header bytes they already hold (two byte reads, no allocation, no
  new parse). For UDP the target is derived once at sequence creation from
  the first datagram (R0.7) and kept on the `UdpSequence` (verify ip.go:3242);
  a later datagram whose DF differs updates the socket's DF option at write
  time only when the value changes, the `lastTtl` pattern of the ICMP backend
  (verify ip_icmp_egress_unix.go:85-101).
- R5.3 Carrying the target to the socket. `net.Dialer.Control` has no flow
  context, and `NetDialer()` builds one dialer per dial (R0.2), so the target
  travels in the dial's context: the sequence dials with
  `context.WithValue(self.ctx, egressSynTargetKey{}, target)`, `ConnectSettings`
  gains `DialControlContext func(ctx context.Context, network, address string,
  c syscall.RawConn) error`, `NetDialer()` sets `net.Dialer.ControlContext`
  when it is non-nil (Go ignores `Control` when `ControlContext` is set, so
  the two static hooks are chained into it, buffer control first, profile
  applier last so its `TCP_WINDOW_CLAMP` and `TCP_MAXSEG` are the final
  word), and `egressDialer` chains the egress binding onto whichever of the
  two the dialer has (verify egress.go:58-70 chains `Control` only today).
  Minimal, localized edits to net.go and egress.go; neither is in the
  coordination exclusion. The applier reads the target from the context, and a
  dial with no target (a control dial, a test) applies nothing.
- R5.4 What the applier sets, per platform, before connect. TTL/hop limit:
  `IP_TTL` or `IPV6_UNICAST_HOPS` by the socket's family (both attempted, one
  must succeed, the darwin binding's shape, verify egress_darwin.go:20-39).
  MSS: `TCP_MAXSEG` on Linux and Darwin; skipped on Windows. Window scale:
  Linux only, `TCP_WINDOW_CLAMP = 2^(16+ws) - 1`, the largest clamp that still
  yields `ws` (R0.18), applied only when the host's own value would be higher,
  since the clamp cannot raise it; the host's value is read once from
  `tcp_rmem[2]` and `rmem_max` through the existing policy reader
  (`socketBufferPolicy`, verify upstream_socket_buffer.go:26-49) so the
  applier knows in advance whether the target is reachable and counts the
  shortfall instead of trying. SYN window: not set (R2.1); the applier
  predicts the host's value from the policy (quantized `tcp_rmem[1]` space,
  R0.18) and counts a mismatch with the target as degraded, which is what a
  capable host's operator reads to tune `tcp_rmem[1]` (D11). The egress
  binding runs in the same chain unchanged (R0.3); `SO_MARK` and
  `IP_UNICAST_IF` are untouched.
- R5.5 Source port. Linux 6.3+ `IP_LOCAL_PORT_RANGE` before connect, for TCP
  and UDP alike. Rather than a per-profile range, one range that is plausible
  for every profile: the overlap of the Linux default 32768-60999 and the
  Windows/Apple default 49152-65535, i.e. 49152-60999, intersected with the
  host's `ip_local_port_range`; an empty intersection or an `EINVAL`/`ENOPROTOOPT`
  leaves the kernel's choice, counted. 11,848 ports per destination tuple is
  ample. Nothing on Darwin and Windows, whose defaults already sit in that
  range. (D6.)
- R5.6 Keepalive. The default dialer explicitly configures 5-second idle and
  interval with one probe (verify net.go:94-100); the post-connect setup also
  enables keepalive (`tcpConn.SetKeepAlive(true)`, verify
  upstream_socket_buffer.go:106). A truly opaque host dial may use other
  timings. The future profile applier sets
  `KeepAliveConfig` from the profile (R4.2) after connect through the existing
  `configureUpstreamTcpConn`, with the profile's idle and the kernel's
  interval; a profile with no fixed value keeps a 45 s idle (Chrome's socket
  setting on Windows and Linux, `?`, requiring capture before adoption).
  The implemented subset exposes an independent optional provider override
  and preserves existing defaults rather than guessing that profile timing.
- R5.7 The same applier on the client-strategy sockets. The owner's parallel:
  the normal and resilient dialers already dial through `NetDialer()` (R0.2),
  so installing the applier on `ConnectSettings.DialControlContext` with a
  static target chosen from the platform and `TlsClientHelloFingerprint`
  (R4.1's mapping) gives those sockets the same TTL, MSS, window scale and
  port range without touching net_http.go or net_resilient.go. The
  client-strategy UDP sockets (the H3 platform transport's `ListenUDP` plus
  `applyEgress`, verify transport_family.go:900-917; the alt api's packet conn,
  verify net_http_alt.go:341-350) can take the post-creation subset (TTL, DF)
  through an `applyEgressProfile(conn, target)` beside `applyEgress`. Source
  port-range policy instead requires a pre-bind control hook.
  Those sockets face the platform and the extender, not a bot-management
  stack; the value is that a client-strategy connection does not read as "a
  Chrome hello on a Go socket" on the wire.
- R5.8 Never fail the dial. Every applier call is best effort: a refused
  option increments that option's degraded counter at the NAT and the dial
  proceeds, the ICMP backend's rule (R0.6). The one existing exception, the
  Windows egress interface pin, which fails the dial on purpose because an
  unpinned socket would loop into the tunnel (verify egress_windows.go:30-42),
  keeps its behavior; the profile applier is not a reason to fail.
- R5.9 Opaque host dials (R0.13). A host implementation that bypasses
  `NetDialer` also bypasses the hook, so the SYN of such a flow is the host's.
  The server's context-preserving `ForceIPv4ConnectSettings` wrapper around a
  normal base is not such a bypass. `configureUpstreamTcpConn` gains the
  post-connect subset for that case, the way it has the send pin today
  (verify upstream_socket_buffer.go:94-112): TTL and keepalive after connect,
  which fixes the data packets but not the SYN, counted as `opaqueDial`. The
  real fix for a truly opaque host is to preserve the dial context and call
  through `NetDialer()` rather than replace it (phase 4, D12). The implemented
  TTL subset detects actual hook execution and already supplies the fallback.

## R6. UDP and QUIC egress

- R6.1 Per flow at `openSocket` (R0.1): TTL/hop limit from the target (R5.2),
  DF from the client's datagram under M or the profile under P
  (`IP_MTU_DISCOVER=IP_PMTUDISC_DO` and the IPv6 twin on Linux; `IP_DONTFRAG`/
  `IPV6_DONTFRAG` on Darwin; `IP_DONTFRAGMENT` on Windows), the port range
  (R5.5). The existing `SetReadBuffer`/`SetWriteBuffer` on the UDP socket
  (verify ip.go:3563-3575) stays; it has no wire visibility.
- R6.2 A DF datagram the path cannot carry fails the send with `EMSGSIZE`
  under `PMTUDISC_DO`, which is what the client's own OS would see as an ICMP
  too-big. `applyPathMtuFor` instead consumes client-side ICMP feedback and
  lowers the return-path packet size (verify ip.go:2747-2756); it does not
  generate this signal. Phase 3 must construct the client-directed ICMP
  response, with the path MTU and a quote of the offending packet, so
  the client's QUIC performs its own PMTU reduction as it would on a real
  network. Today's socket, with DF left to the kernel default, may fragment
  silently where the client expected a too-big signal; DF realism also fixes
  that.
- R6.3 Fields the provider cannot set at Level A on UDP: the IP id policy
  (Linux increments per connected socket, Windows globally, Darwin randomly;
  a DF datagram's id is observable), the IPv6 flow label (Linux and Windows
  10+ already randomize per flow, so the common case is consistent; Darwin
  `?`), ECN bits (Chrome QUIC does not set ECN by default, `?`). Listed in R12.

## R7. The DNS the provider emits

- R7.1 Control plane, scoped out. The egress resolver (R0.17) resolves the
  provider's OWN names (platform api, platform transport) over the provider's
  own adapter or public resolvers (verify egress_resolver_windows.go:50-57,
  egress_resolver_linux.go:146-154). Those queries carry the provider's
  identity, not a client's intent; no destination site ever sees them, and
  the resolvers that do see them see a Go program on the provider's OS, which
  is what it is. Changing them buys nothing for client realism.
- R7.2 Client DNS in the tunnel, covered by R5/R6. A client's plain DNS
  (UDP/TCP 53) and DoT (TCP 853) are ordinary flows: the SYN or datagram is
  the client OS's, mirrored like any other, so the resolver that answers sees
  the client's stack behind the provider's address.
- R7.3 The mux Tun's DoH, Source P territory. The Tun's DoH flows enter the
  tunnel from gVisor with a Linux-order SYN and TTL 64 (R0.12) and a Go hello
  (R0.10); mirrored, the DoH resolver sees a consistent "Go program on Linux".
  If the Chrome hello is later extended to DoH, that pairing becomes a Chrome
  hello over a gVisor SYN, so the two must move together: the Tun emits the
  profile's SYN (the fork's `makeSynOptions` made profile-driven and the
  stack's TTL set from the profile, R10.3) in the same change that gives DoH
  the Chrome hello, and the flow declares Source P. Until then DoH keeps Go's
  hello, which is consistent as it stands (D7).

## R8. Non-browser flows and the default policy

Mirror is the policy for every flow from a device tun: apps, api clients,
games, mail, ssh, and browsers alike, because mirroring the OS's own SYN is
correct for all of them and is what a NAT does. There is no application
classification on the egress side and no passthrough list. The only
exceptions are mechanical: a SYN the validator rejects (R3.1(v)) and the
synthetic speed range, which never dials (verify ip.go:5451-5457). `native`
(id 255) is the owner's kill switch, equal to today's behavior, and `off`
(R11.1) is the shadow posture that derives targets and counts without
applying.

## R9. Bindings, marks and budgets

- R9.1 The applier shares the Control chain with the egress interface pin and
  touches different options; order between them is irrelevant and the pin's
  semantics are unchanged (R0.3, R5.3). The cgroup-BPF fwmark on urnetwork-linux
  is stamped at `inet_create` before any hook runs and is untouched (R0.3).
  B-lite needs a discriminator on managed sockets to select their SYNs
  (R10.1). There is only one socket mark, so a distinct `SO_MARK` value must
  not overwrite the routing-exclusion identity. Coordinate the discriminator
  and host routing policy before choosing a mark mask or another mechanism.
- R9.2 Memory. Level A adds one `egressSynTarget` (under 32 bytes) per
  sequence and a fixed set of counters per NAT; no budget changes. The window
  clamp bounds the kernel's receive window at `2^(16+ws)`: 4 MiB for the
  Apple family (ws 6), 8 MiB for ws 7, above the stock 6 MiB ceiling for ws 8,
  so it never increases kernel memory per flow and at most trims it; the
  per-flow throughput bound it implies, clamp over the round trip, is
  4 MiB/100 ms, about 340 Mbit/s, above the product's per-flow envelope (D4).
  B-lite adds a bounded SYN table (at most `TcpBufferSettings.GlobalLimit`
  entries, or 65536 when that is 0) and one queue; B-full would add gVisor
  endpoints per flow and must raise `providerTcpFlowByteCount` (R0.14), one
  more reason it is deferred.
- R9.3 Platform scope of each piece: TTL everywhere; MSS Linux and Darwin;
  window scale Linux; DF everywhere (Windows by local constant); port range
  Linux 6.3+; B-lite Linux with `CAP_NET_ADMIN`; B-full Linux with
  `CAP_NET_RAW`. Android providers are Linux-family hosts with no privilege:
  TTL, MSS, DF, and the port range where the kernel is new enough.

## R10. Level B mechanics

- R10.1 B-lite, SYN rewrite. Files `egress_syn_rewrite_linux.go` and
  `egress_syn_rewrite_other.go` (stub). At provider role start, a capability
  probe: open an NFQUEUE (a pure-Go netlink binding, D9), install one
  nftables rule in the output chain, `meta mark == egressProfileMark tcp flags
  & (syn|ack) == syn queue num N bypass`, and remove it at role stop; any
  failure leaves B-lite off for the process, logged once. The discriminator
  must preserve the routing-exclusion behavior described in R9.1. Register
  the flow before its first SYN can be queued. The originally proposed
  `(sourcePort, destination) -> target` insertion in `Control` cannot work
  with automatic port allocation: Go calls the hook before bind/connect and
  the source port is still zero. Design a reservation or alternate correlation
  mechanism before implementing this path. Once correlated, the entry is
  removed when the dial completes or fails. The handler rewrites a queued SYN
  to the target: option order and padding exactly as the layout says,
  timestamps and SACK-permitted dropped when the layout omits them, the
  window lowered to the target when the target is lower, TCP checksum
  recomputed, IP total length and checksum fixed when the options length
  changed; verdict accept with the modified packet. It never raises the
  window (the kernel would drop in-window segments it did not offer), never
  changes the window scale or the MSS value (the kernel must agree with the
  wire on both; they come from Level A), and never touches a non-SYN packet.
  Kernel SYN retransmissions are queued again and rewritten the same way. A
  stripped timestamps or SACK option is safe: a compliant peer answers
  without the option and the kernel then runs the connection without it, the
  standard RFC 7323 and RFC 2018 negotiation. `bypass` on the rule covers an
  absent listener; queue overflow separately requires `NFQA_CFG_F_FAIL_OPEN`.
  A stalled live handler needs a lifecycle policy as well. Verify these
  fail-open paths with kernel integration tests, not only pure packet bytes.
  Queue length bounded at 1024; the handler goroutine takes the
  provider context and is joined at role stop.
- R10.2 B-full, user-space stack. The gVisor fork's netstack as the egress
  TCP, a profile-driven `makeSynOptions` (R0.12) with the layout, the stack's
  default TTL and IP id policy from the profile, an AF_PACKET link endpoint, a
  dedicated port range and an nftables rule dropping the host kernel's RST
  for that range. Only if measurement shows the IP id, timestamp clock or ISN
  being checked; not designed further here.
- R10.3 The Tun's SYN (Source P for synthesized flows, R7.3). The same
  `makeSynOptions` change serves the mux Tun inside the tunnel, where no
  privilege is needed because the Tun already is a user-space stack: the Tun
  takes a profile at construction, sets the stack's default TTL
  (`tcpip.DefaultTTLOption`, confirm the option's name in the fork), and the
  fork's encoder emits the profile's layout. Timestamps and SACK are
  per-stack options in gVisor and follow the profile. This is the
  client-strategy counterpart for flows the client synthesizes.

## R11. Settings, flags and observability

- R11.1 `TcpBufferSettings.EgressStackProfile` and
  `UdpBufferSettings.EgressStackProfile`, both `EgressStackProfileSettings{Mode,
  ProfileId, CrossFamilyPartial, PortRange, SynRewrite, KeepAliveIdle}`. `Mode`
  is `off` (derive targets, count, apply nothing: the shadow posture), `mirror`,
  `profile` (apply `ProfileId` to every flow, the owner override of R3.2(iii)),
  or `native` (nothing derived, nothing counted; today). The defaults in
  `DefaultTcpBufferSettingsWithBufferSize` and the UDP twin are `off` for the
  first full-profile release and `mirror` thereafter (D8). The implemented
  TTL-only subset instead defaults to `EgressTtlModeMirror` immediately,
  at the owner's explicit request. The sdk exposes one
  `DeviceLocalSettings.ProviderEgressRealism` in the shape of
  `DefaultProvideExtender` (EXTENDER.md F3, G1) that the provider wiring
  copies into both settings; the server's hosts opt in explicitly.
- R11.2 The client-strategy side: `ConnectSettings.EgressStackProfileId`,
  resolved at `DefaultConnectSettings` time from the platform and
  `TlsClientHelloFingerprint` by R4.1's mapping; `"go"` on the hello resolves
  to 255 (native) here too, so the one kill switch quiets both layers.
- R11.3 Counters on the NAT, one `atomic.Uint64` each, read into the provider
  diagnostics beside the existing per-source counters (`IpProviderDiagnostics`,
  verify protocol/ip.proto:27-31): flows by source (mirror, profile, native,
  rejectedSyn), per-option degraded (ttl, mss, windowScale, synWindow,
  dontFragment, portRange, keepAlive), opaqueDial, synRewrite (rewritten,
  bypassed, tableFull), helloFamilyContradiction (R3.3). Logging: one `[egress]`
  line per profile decision at V(1), never per flow at the default level
  (the `[egress]dial` line's throttle shape, verify egress_dial.go:292-334).

## R12. Known limitations

- The SYN window field under Level A is the host's (R5.4); only B-lite lowers
  it and nothing raises it. Same-family full match therefore needs the host's
  `tcp_rmem[1]` to produce the target window (D11).
- The IP id policy, the timestamp clock rate, the ISN, ECN setup and the SYN
  retransmission schedule are the host kernel's at Level A and B-lite.
- Mid-connection passive fingerprinting (delayed-ack cadence, window update
  pattern, PSH placement, MTU probing) is the kernel's at every level short of
  B-full.
- A provider whose interface MTU is under 1500 advertises a lower MSS than the
  profile (counted); a provider behind a CGNAT or a tunnel of its own shows
  that path's TTL decrement, which is also what a real client there would show.
- Windows providers reach TTL and DF only; Android providers reach TTL, MSS,
  DF and the port range on new enough kernels.
- A client that forwards traffic for another device (tethering) mirrors a
  decremented TTL, as a NAT in front of it would.

## Phases

Each phase: the design items are the contract, the implementation lands with
its tests per RULES.md "Root-cause tests" and CODESTYLE.md "Tests" (verify
CODESTYLE.md:152-175), the whole package suite passes, and a review closes it
before the next. Phases 1-3 are connect only. Phase 4 touches protocol, sdk
and server. Phase 5 is Linux only. Phase 6 touches the gVisor fork.

1. Facts and fixtures (connect). R4.4's capture corpus and fixtures; the
   registry R4.1 as data; `parsedTcp`/`parsedUdp` carry `ttl` and
   `dontFragment`; `egressSynTarget` derived at `initializeSynWithLock` and at
   UDP sequence creation with R8's validation; the counters, shadow mode (`off`)
   as the default. Nothing is applied. Acceptance: T0, T14; the targets a
   corpus of real SYNs produce match the table.
2. Level A TCP (connect). `DialControlContext`, `ControlContext` chaining in
   `egressDialer` and `NetDialer()`, the appliers (TTL, MSS, window scale,
   port range, keepalive), chained after the buffer control, the post-connect
   subset on opaque dials. Acceptance: T1-T5, T7, T8; the buffer rule's own
   tests still pass unchanged.
3. Level A UDP and QUIC (connect). The UDP target, DF and TTL at `openSocket`,
   DF updates on change, `EMSGSIZE` to the path-MTU signal. Acceptance: T6.
4. Source P and the hosts (protocol, sdk, server). `IpNetworkStackProfile`,
   the per-source table, the sdk's platform-and-fingerprint mapping and the
   `ProviderEgressRealism` default, the server and sdk hosts wrapping
   `NetDialer()` so the hook runs. Acceptance: T10, T11; an old provider
   ignores the message; an opaque host no longer counts `opaqueDial`.
5. B-lite (connect, Linux). The capability probe, the rule, the SYN table,
   the rewrite, fail-open. Acceptance: T12 as pure-bytes tests on every
   platform plus one privileged integration test that skips without
   `CAP_NET_ADMIN`.
6. Synthesized stacks (connect, gVisor fork). Profile-driven `makeSynOptions`,
   the Tun's profile, DoH's hello and SYN moving together (R7.3), the
   client-strategy UDP subset. Acceptance: T13.
7. B-full: not scheduled (D1).

## Tests

Every test is deterministic in the RULES.md sense: in-process loopback
listeners and injected sockets, explicit seams instead of sleeps, and an
assertion at the layer where the behavior is observable, the wire bytes of
the SYN or the socket's own options, never a downstream symptom. The SYN
oracle on Linux is `TCP_SAVE_SYN` on the test's listening socket (set in a
`net.ListenConfig.Control`) and `TCP_SAVED_SYN` read from the accepted socket
through `rawConn.Control` (the raw-getsockopt shape of
ip_udp_socket_drops_linux.go:21-30, verify): it returns the IP and TCP headers
of the SYN exactly as received, with no root and no capture library, and
`connect()` returning on the provider side guarantees the listener has the SYN
before the seam fires, so no ordering is left to the scheduler. On Darwin and
Windows the oracle is the dialing socket's own options read back with
`getsockopt` (`IP_TTL`, `TCP_MAXSEG`), the socket-options layer. Each test
fails on origin/main (or with only its applier reverted) for the stated root
cause and passes with the fix; the table names the observable that changes.

| Failure | Root cause | Test | Layer, discriminator |
|---|---|---|---|
| Egress SYN TTL is the host's under a Windows client SYN | TTL not derived or applied (R0.6, R5.4) | `TestEgressSynTtlFollowsTheClientSyn` (linux) | saved SYN IP TTL == 128; before: 64 |
| Egress SYN MSS is the tunnel's 1240 if copied, or the host's if ignored | no normalization (R3.4) | `TestEgressSynMssIsTheProfileValueNotTheTunnelMss` (linux) | saved SYN MSS == min(1460, hostMax) and != 1240 |
| Window scale above an Apple-family target | no clamp (R5.4) | `TestEgressSynWindowScaleClampMatchesTheTarget` (linux) | saved SYN WS == 6 when host WS > 6; before: host WS |
| A refused option fails the flow | applier not best-effort (R5.8) | `TestEgressProfileNeverFailsTheDial` | with a `setsockoptForTest` seam returning EPERM the dial succeeds and `degraded.ttl == 1` |
| Opaque host dial silently gets nothing | the post-connect subset missing (R5.9) | `TestAnOpaqueDialGetsThePostConnectSubsetOnly` | `IP_TTL` read back == target after connect; `TCP_MAXSEG` untouched; `opaqueDial == 1` (the shape of upstream_socket_buffer_opaque_dial_test.go) |
| UDP DF and TTL are the kernel defaults | not mirrored at `openSocket` (R6.1) | `TestUdpEgressDfAndTtlFollowTheClientDatagram` | `IP_MTU_DISCOVER == DO` and `IP_TTL == 128` read back through an `afterUdpSocketOpenForTest` seam; a later DF=0 datagram flips it |
| Source port outside every browser OS range | no port range (R5.5) | `TestEgressPortIsInThePlausibleOverlap` (linux 6.3+, skipped otherwise) | local port in [49152, 60999] over 64 flows; before: ports under 49152 appear |
| JA4T disagrees with the client SYN on a same-family pair | any of the above | `TestJa4tOfTheEgressSynMatchesTheClientSyn` (linux) | JA4T computed from the saved SYN equals JA4T of the crafted client SYN in mss/ws/ttl, and whole-string when the host's family and window permit (the test reads the policy to know) |
| A misdeclared profile is invisible | no audit (R3.3) | `TestHelloFamilyContradictionIsCounted` | a Safari hello over a Windows-layout SYN increments `helloFamilyContradiction`; a Chrome hello does not |
| A synthesized flow mirrors gVisor's SYN | Source P not applied (R3.2) | `TestProfileSignalOverridesMirrorForTheSource` | after `IpNetworkStackProfile{3}` the target is the iOS profile although the SYN is Linux-layout |
| Unknown profile id breaks egress | no fallback (R3.2(ii)) | `TestUnknownProfileIdFallsBackToMirror` | target == mirror-derived, `profileUnknown == 1` |
| B-lite rewrite wrong or unsafe | R10.1 rules | `TestSynRewriteProducesTheProfileLayout`, `TestSynRewriteNeverRaisesTheWindowOrChangesWsOrMss`, `TestSynRewriteBypassIsFailOpen` (pure bytes, every platform) | output options == `2-1-3-1-1-4`, no TS, valid checksums; window never above input; WS and MSS bytes identical; a queue error yields the input bytes and `bypassed == 1` |
| The Tun's SYN is gVisor's under a browser profile | encoder not profile-driven (R10.3) | `TestTunSynFollowsTheProfile` | the SYN read from the Tun's packet side carries the profile layout and TTL; before: Linux layout, TTL 64 |
| Table drifts from reality | no fixture check (R4.4) | `TestEgressProfileTableMatchesTheCaptures` | JA4T derived from each fixture equals the table row |
| Targets wrong in shadow mode | derivation (R5.2) | `TestEgressSynTargetDerivationFromRealSyns` (T0) | targets from a corpus of sanitized real SYNs match expected values; `off` applies nothing (socket options read back equal the kernel defaults) |

The client-facing half of this surface — that connect's own TLS ClientHello
and QUIC Initial do not drift from real Chrome — is gated by a separate
hermetic conformance harness, documented in `fingerprint/README.md`. Its
Layer A diffs connect's emitted hello against a versioned Chrome golden
(GREASE positions masked, `X25519MLKEM768` asserted) and its QUIC Initial
against the merged `QuicVersionPolicy`; its opt-in Docker Layer B refreshes
the goldens from real Chrome. The egress SYN/TTL (JA4T) consistency check
above is the provider-side complement: its per-dialer wiring in the harness
waits on the Level A egress implementation of this design, carried there as a
skipped placeholder (`TestEgressSynTtlConformance`) that names it, because the
SYN's ground truth is the OS kernel and the check needs root — a Layer-B, not
a hermetic `go test`.

## Decisions for the owner

- D1. Build order: Level A on every provider first, B-lite on privileged Linux
  providers second, B-full not scheduled. Recommendation: yes; A closes the
  TTL tell everywhere and is required by B-lite anyway (R2.3).
- D2. Target source: mirror the client's SYN for device flows, explicit
  profile for synthesized flows, hello inference audit-only. Recommendation:
  yes (R3.6); the alternative, profile-everything, makes device flows wrong
  whenever the declaration and the apps disagree.
- D3. Cross-family partial application (TTL, MSS, window scale when the layout
  cannot match): apply, or apply only on a full match. Recommendation: apply,
  because an unmatched SYN reads as unknown rather than as a Linux server,
  and it passes TTL-distance checks; ship it behind `CrossFamilyPartial` and
  compare block rates between cohorts before fixing the default (R3.6).
- D4. Window-scale clamp floor versus throughput: clamp to `2^(16+ws) - 1`,
  which is 4 MiB for the Apple family. Recommendation: accept; it is above
  the per-flow envelope and never raises kernel memory (R9.2).
- D5. MSS: canonical 1460/1440 capped by the host, never the tunnel's.
  Recommendation: yes (R3.4).
- D6. Source port: one overlap range 49152-60999 on Linux 6.3+, nothing per
  profile, nothing elsewhere. Recommendation: yes (R5.5).
- D7. DoH: keep Go's hello and the mirrored gVisor SYN until the Tun can emit
  a profile SYN, then move both together. Recommendation: yes (R7.3).
- D8. TTL subset decision: `EgressTtlModeMirror` by default immediately,
  selected explicitly by the owner; `shadow` and `native` remain available.
  Original full-profile recommendation: `off` (shadow counters) for one
  release, then `mirror` by default; `native` is the kill switch. The
  counters tell what the fleet's hosts can reach before anything changes on
  the wire.
- D9. B-lite dependency: a pure-Go netlink NFQUEUE binding and nftables
  client, under EXTENDER.md H's dependency rules (verify EXTENDER.md:1098).
  Recommendation: accept one pure-Go nfqueue binding; reject anything that
  needs cgo or libnetfilter; defer B-lite rather than take a cgo dependency.
- D10. Host-family affinity in provider selection (server): prefer providers
  whose kernel family matches the client's, which makes Level A a full match
  without B. Recommendation: shadow metric first (count the family pairing of
  served flows); it is a FindProviders change outside this design.
- D11. Host tuning for capable Linux providers (`tcp_rmem[1]` for a 65535
  window, `rmem_max`/`tcp_rmem[2]` for window scale 8): document in the
  urnetwork-linux installer, never set by connect. Recommendation: document
  only.
- D12. Truly opaque host dials: preserve context and wrap `NetDialer()` so
  the hook runs. The server's normal `ForceIPv4ConnectSettings` wrapper
  already delegates to the base dial and does not inherently need changing.
  Recommendation: yes, phase 4; until then they get the post-connect subset
  and a counter.
- D13. IPv6 flow label realism: defer until a capture shows a mismatch; Linux
  and Windows already randomize. Recommendation: defer.
- D14. Keepalive: expose an independent optional provider setting now
  (implemented); preserve the explicit 5-second default until a browser
  reference justifies changing it. Future profile timing requires a capture
  (R5.6).
