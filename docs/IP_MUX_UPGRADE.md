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

## HTTP (TCP/80)

Claimed plaintext HTTP is either passed through to the egress unchanged (`HttpUpgradeUnencrypted`,
the default) or dropped (`HttpUpgradeBlock`). `Http.Mode` selects which; an empty/unknown value is
treated as pass-through. The mux does **not** terminate or upgrade HTTP.

> An earlier HTTPS-upgrade-with-fallback path was removed. Terminating a `:80` connection
> re-originates it from the mux, so the origin sees a different egress identity than the client's
> direct traffic — which breaks IP-bound services (e.g. Pandora audio). No HTTP-level fix addresses
> that, because the problem is the termination itself.

---

## Settings

```go
UpgradeMuxSettings{
    Dns  *DnsUpgradeSettings  // nil → DNS passes through
    Http *HttpUpgradeSettings // nil → HTTP passes through
}

DnsUpgradeSettings{ Resolver *DnsResolverSettings; ResolveTimeout; ResponseTtl }

HttpUpgradeSettings{ Mode HttpUpgradeMode } // unencrypted (pass-through, default) or block (drop)
```

`SetSettings` applies at runtime: the DohCache is rebuilt immediately, and the HTTP mode change
takes effect on subsequent packets.

### Per-use-case defaults

- **App / device** (`DefaultUpgradeMuxSettings`): DNS intercepted and resolved over
  **DoH only**, egressing the tunnel; HTTP **passed through unchanged** (or dropped with `block`).
- **Server / proxy**: **no mux** — `nil` settings, pure pass-through to the egress. The
  proxy runs on the shared stack, whose buffers are sized for throughput (TCP up to `1MB`).

---

## Files

| File | Responsibility |
|------|----------------|
| `ip_mux.go` | the composable `IpMux` (onSend/onPump hooks, pump, upstream wiring) |
| `ip_mux_upgrade.go` | `UpgradeMux`: settings, DNS interception, HTTP pass/drop, ServerName reverse map, lifecycle |
| `tun.go` | the internal gVisor stack/tun (`DialContext`, `ListenTCP`, `DohCache`, buffer config) |
