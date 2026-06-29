# IP Mux Upgrade

`UpgradeMux` (`ip_mux_upgrade.go`, `ip_mux_upgrade_http.go`) interposes on the device's
egress path to intercept and optionally **upgrade** plaintext local protocols before they
leave through the tunnel:

- **DNS (UDP/53)** → resolved over DoH that egresses the tunnel.
- **HTTP (TCP/80)** → re-issued to the origin over **HTTPS**, with optional fallback to
  plaintext.

It also records resolved IP→hostname mappings so the multi-client can keep traffic for the
same base domain on the same exit (ServerName path affinity).

For pure pass-through (the server/proxy use case) **no mux is installed at all** (`nil`
settings), which avoids a per-device tun/stack.

---

## Position in the data plane

```
device OS tun ─▶ DeviceLocal ─▶ UpgradeMux ─▶ multi-client ─▶ exit (remote UserNat)
                                  │  ▲
                          onSend  │  │ onPump
                                  ▼  │
                          internal gVisor stack (private)
```

`UpgradeMux` is a concrete `IpMux` (a composable mux). It wraps the upstream send and
installs two hooks:

- **`onSend(packet)`** — inspects each send-path packet. It *claims* (terminates/transforms)
  DNS and HTTP per the settings; everything else returns `false` and flows upstream
  unchanged.
- **`onPump(packet)`** — intercepts packets the internal stack emits. Client-bound HTTP
  replies (source `muxAddr:80`) are un-NAT'd back to the original destination and delivered
  downstream; the stack's own upstream connections (HTTPS, DoH) flow out through the
  tunnel.

### Internal stack

The mux terminates client connections on its **own private gVisor stack** (`PrivateStack`),
so its replies are emitted via the tun (pump → un-NAT → downstream) rather than delivered
intra-stack to a peer that happens to share the process-wide stack. `muxAddr` is a single
address reserved from the link-local pool, used as the DNAT target.

Because this stack lives inside memory-constrained hosts (notably the iOS network
extension), its gVisor buffers are kept small — TCP `32KB` default / `64KB` max per
connection, UDP `64KB` — far below the shared/server data-plane stack (TCP up to `1MB`, UDP
`1MB`). The buffer Max applies *per connection*, so a large default would blow the
extension's memory limit under upgrade load.

---

## DNS upgrade (UDP/53)

When `Dns.Resolver` is set, `onSend` claims single A/AAAA queries to UDP/53:

1. Parse the query (`golang.org/x/net/dns/dnsmessage`).
2. Resolve via the tun's `DohCache` (DoH over the tunnel) — bounded by `ResolveTimeout`
   (default `5s`). The resolver fans out to multiple providers in parallel and the first to
   return records wins (default set: Cloudflare + Google via the JSON API, Quad9 via RFC 8484
   wire-format — each `DohServer` is tagged with the format it speaks). Concurrent identical
   queries coalesce onto one resolution (single-flight), concurrent resolutions are bounded
   (`MaxConcurrentResolutions`), and each DoH h2 connection is kept warm with keepalive PINGs so
   bursty lookups don't re-pay a handshake over the tunnel.
3. Build a reply and inject it downstream (the question's reverse path). Records — or an
   authoritative no-record answer — reply `NOERROR` (`ResponseTtl`); a *resolution failure*
   (timeout / resolver error) replies **SERVFAIL** so the client retries, rather than `NOERROR`
   with no answers, which a client treats as an authoritative "no address" and gives up on.
4. Record the resolved IPs → hostname for ServerName affinity.

Non-A/AAAA queries, and all DNS when `Dns`/`Dns.Resolver` is `nil`, pass through to the
egress unchanged. The app default is **DoH-only** (no plaintext `:53` and no off-tunnel host
resolution), so a DoH failure fails the query rather than leaking DNS.

---

## HTTP upgrade (TCP/80)

> **Deprecated.** The app default is now plaintext passthrough (`HttpUpgradeUnencrypted`).
> Terminating a `:80` connection re-originates it from the mux (`muxAddr` source, a fresh
> upstream flow), so the origin sees a different egress identity than the client's direct
> traffic — which breaks IP-bound services (Pandora audio). No HTTP-level fix addresses this
> because the problem is the termination itself. The HttpUpgrade* modes below are slated for
> removal; passthrough is the intended behavior.

`Http.Mode` selects the behavior:

| Mode | Behavior |
|------|----------|
| `unencrypted` (and empty/unknown) | pass through to the egress unchanged |
| `https` | upgrade to HTTPS; **no** fallback (failure → 502) |
| `https_fallback` | upgrade to HTTPS; fall back to plaintext on failure |
| `block` | claim and drop |

For `https` / `https_fallback`:

1. **DNAT + terminate.** The claimed TCP/80 packet is rewritten `dst → muxAddr` (in-place
   RFC-1624 incremental checksum, with a gopacket fallback for unusual packets) and injected
   into the internal stack, where a listener on `muxAddr:80` terminates it. The flow's
   original destination IP is recorded, keyed by client `(ip, port)`.
2. **Re-issue.** Each request is re-issued to the **original destination IP** at the HTTPS
   port (`HttpsPort`, default `443`) — see *round-trip* below.
3. **Un-NAT.** Replies emitted by the stack (source `muxAddr:80`) are rewritten
   `src → original dst` in `onPump` and delivered downstream, so the OS never sees
   `muxAddr`.

The client connection is kept alive across requests; each request currently gets its own
one-shot upstream connection (upstream pooling is a future optimization).

The NAT record is **not** deleted when the terminated connection closes. The final response
and the TCP teardown (FIN, final ACK) are emitted by the stack *asynchronously, after the
handler returns*, and `onPump` needs the record to un-NAT them on the way to the client —
deleting eagerly drops a response-then-close (notably a strict-mode 502) most of the time.
The record is left to go idle and is reclaimed by the maintenance loop after `NatIdleTimeout`
(default `5m`).

### The HTTPS round-trip and fallback

`roundTrip` (`ip_mux_upgrade_http.go`) targets `originalDstIP:HttpsPort` and sets the TLS
**SNI to the HTTP `Host` header** (port-stripped); `ServerName` also drives certificate
verification. The original `Host` is preserved on the re-issued request.

The upgrade attempt is bounded by **`UpgradeTimeout`** (default `5s`) across three stages —
the mux ctx itself has no deadline, so without this a closed/filtered/stalled `:443` would
hang the request. The streamed **response body is not bounded** (long-lived downloads/
streams are fine).

Fallback (in `https_fallback` mode) covers every connection-level failure:

| Stage | Failure | `https_fallback` | `https` (strict) |
|-------|---------|------------------|------------------|
| dial | `:443` refused / unreachable / dial timeout | → plaintext | → 502 |
| TLS handshake | no TLS / bad cert / SNI mismatch / handshake timeout | → plaintext | → 502 |
| response header | stalls or errors within `UpgradeTimeout` | → plaintext *(GET/HEAD only — the request was already sent, so only idempotent, bodyless methods are safe to re-issue)* | → 502 |
| response received | HTTPS status ≥ 400 | → plaintext *(GET/HEAD only)*, host held off | used as-is |

A 4xx/5xx HTTPS response in `https_fallback` mode is treated as an upgrade failure for an
idempotent GET/HEAD: the request is re-issued over plaintext and the host is held off — a host
that errors the *upgraded* request but serves the content over plain HTTP (an HTTP-only CDN with
scheme-scoped signed URLs — what broke Pandora audio) would otherwise be relayed the error.
Non-idempotent methods, and strict `https` mode, relay the status as-is (logged). A 2xx/3xx HTTPS
response is always relayed as-is.

### Failure hold-off (negative cache + TTL)

Retrying the (potentially slow) HTTPS upgrade on **every** request to a server that does not
support HTTPS on the original IP is the main reason a naive fallback "doesn't work" in
practice — each request re-pays the failure cost. So in `https_fallback` mode, after an
upgrade fails for a server name, that name is **held off**: subsequent requests skip the
upgrade and go straight to plaintext for **`UpgradeFailureHoldOff`** (default `5m`).

`UpgradeFailureHoldOff` doubles as the **TTL** of the failure record:

- A record is dropped on a later check once it is older than the hold-off
  (`recentlyFailedUpgrade`).
- The single **maintenance goroutine** (`run`, started in `NewUpgradeMux`) clears expired
  records on the shortest-TTL cadence — so the map stays bounded even for server names that
  are never retried. The same loop reclaims the other TTL-bounded state: the NAT table
  (`NatIdleTimeout`) and the reverse affinity map (`ReverseTtl`). When the hold-off is
  disabled it idles at the default cadence and clears any records left over from a
  previously-enabled hold-off.

`UpgradeFailureHoldOff = 0` disables the hold-off (every request retries HTTPS).

### Diagnostics

Each upgrade outcome is logged with the server name and reason:

- `Info`: dial/handshake/response failure → fallback or 502; HTTPS connected but `status ≥ 400`.
- `V(1)`: hold-off skip; successful upgrade with status.

This makes a mis-behaving origin (e.g. one that handshakes but stalls, or answers 4xx only
over HTTPS) visible in logs without a packet capture.

---

## Settings

```go
UpgradeMuxSettings{
    Dns  *DnsUpgradeSettings  // nil → DNS passes through
    Http *HttpUpgradeSettings // nil → HTTP passes through
}

DnsUpgradeSettings{ Resolver *DnsResolverSettings; ResolveTimeout; ResponseTtl }

HttpUpgradeSettings{
    Mode HttpUpgradeMode      // string enum (above)
    HttpsPort int             // default 443
    TlsConfig *tls.Config     // base config (cert pinning / test roots); SNI set per request
    UpgradeTimeout            // bounds dial + handshake + response-header read (default 5s)
    UpgradeFailureHoldOff     // skip-upgrade window + failure-record TTL (default 5m)
}
```

`SetSettings` applies at runtime: the DohCache is rebuilt immediately; HTTP mode changes
apply to subsequent connections (the terminator binds lazily on the first HTTPS claim).

### Per-use-case defaults

- **App / device** (`DefaultUpgradeMuxSettings`): DNS intercepted and resolved over
  **DoH only**, egressing the tunnel; HTTP **passed through unchanged**. (The upgrade modes
  re-originate the connection from the mux, changing egress identity and breaking IP-bound
  services like Pandora — slated for removal.)
- **Server / proxy**: **no mux** — `nil` settings, pure pass-through to the egress. The
  proxy runs on the shared stack, whose buffers are sized for throughput (TCP up to `1MB`).

---

## Files

| File | Responsibility |
|------|----------------|
| `ip_mux.go` | the composable `IpMux` (onSend/onPump hooks, pump, upstream wiring) |
| `ip_mux_upgrade.go` | `UpgradeMux`: settings, DNS interception, ServerName reverse map, lifecycle |
| `ip_mux_upgrade_http.go` | HTTP upgrade: DNAT/terminate, round-trip + fallback + hold-off, address rewrite |
| `tun.go` | the internal gVisor stack/tun (`DialContext`, `ListenTCP`, `DohCache`, buffer config) |
