# IP Security — connect data plane

Status: implemented · Scope: `connect` data plane · Source: `ip_security.go`, `ip_security_cfaa.go`, `ip_security_cfaa_block.go`, `ip_security_dmca.go`, `ip_security_webstandard.go`, `security/main.go`

This is the authoritative specification of the `connect` IP security pipeline. It
consolidates the formerly separate CFAA and DMCA security docs into a single
reference. It describes the **intended** behavior; the code is the implementation of
this spec. Where the two disagree, that is a bug in one of them.

The pipeline has two layers, evaluated cheapest-first:

1. **CFAA — static endpoint reputation** (`ip_security_cfaa.go`): a stateless,
   per-packet check of the destination/source against a blocked-IP table plus a
   fixed port/protocol policy.
2. **DMCA — stateful deep-packet inspection** (`ip_security_dmca.go`): a per-flow
   state machine that catches BitTorrent (and unsanctioned encrypted egress) from
   the packet *payload*, independent of port.

---

## 1. Goals and motivation

Exit / provider nodes forward user traffic to the public internet under the
provider's own IP address. Two distinct harms follow, and each layer targets one:

- **Computer-fraud / abuse (CFAA-class).** A provider's address must never
  originate (or receive) traffic to known-malicious or hijacked address space
  (botnet C2, malware hosts, hijacked netblocks), nor speak known-abused or
  insecure-privileged ports. The static reputation layer handles this with a
  binary-search IP lookup plus a constant-time port check per packet.
- **Copyright / DMCA.** BitTorrent swarm participation from the provider IP
  generates copyright-infringement complaints. Port rules alone are insufficient
  (BitTorrent uses random high ports and 443), so a stateful payload detector
  suppresses BitTorrent on **any** port.

Design rationale for the two-layer split: the static layer is cheap and
unambiguous, so it runs first and filters the clear cases; the stateful detector is
more expensive (per-flow state, payload inspection) and only sees what the static
layer has no opinion on.

---

## 2. Architecture

### 2.1 Interface and results

```go
type SecurityPolicy interface {
    Stats() *SecurityPolicyStatsCollector
    Inspect(provideMode protocol.ProvideMode, ipPath *IpPath, payload []byte) (SecurityPolicyResult, error)
}
```

| Result                         | Value | Meaning |
|--------------------------------|-------|---------|
| `SecurityPolicyResultDrop`     | 0     | Silently drop the packet. |
| `SecurityPolicyResultAllow`    | 1     | Forward the packet. |
| `SecurityPolicyResultIncident` | 2     | Drop **and** report abuse (ReportAbuse). |

`payload` is the L4 payload (may be nil for header-only inspection) and is valid
only for the duration of the call — detectors copy out anything they retain and
never alias the shared (recycled) packet buffer.

Three implementations exist: `egressSecurityPolicy`, `ingressSecurityPolicy`, and
`disableSecurityPolicy` (always Allow; for testing / opt-out).

### 2.2 Egress pipeline (destination)

For each egress packet, in order:

1. **Network bypass** — `provideMode == ProvideMode_Network` (same-network
   relationship) → **Allow**.
2. **Public-unicast** — destination not public unicast (private / loopback /
   link-local / multicast / unspecified) → **Incident**.
3. **CFAA layer** — `cfaa.inspect(destIp, destPort, proto, version)`:
   - `cfaaDrop` → **Drop**
   - `cfaaAllow` → **Allow** (definitive; skip DPI)
   - `cfaaPass` → hand to the **DMCA layer**.
4. **DMCA layer** — `dmca.inspect(ipPath, payload)` → Allow / Drop / Incident.

### 2.3 Ingress pipeline (source)

Ingress mirrors only the static **drops**, evaluated on the *source* endpoint, and
does **not** run payload inspection:

1. **Network bypass** — `ProvideMode_Network` → **Allow**.
2. **CFAA layer** — `cfaa.inspect(srcIp, srcPort, proto, version) == cfaaDrop` →
   **Drop**; otherwise **Allow**. (Ingress does not distinguish `cfaaAllow` from
   `cfaaPass` — only the drops are enforced.)

### 2.4 Where it runs

The egress policy runs wherever the data plane hands user traffic to the public
internet:

- `RemoteUserNatProvider.ClientReceive` (`ip.go`) — the exit/provider forwarding a
  remote client's packets to the internet. **This is the DMCA-critical path.**
- `RemoteUserNatClient.SendPacket`, `RemoteUserNatMultiClient.SendPacket`
  (`ip.go`, `ip_remote_multi_client.go`) — a client emitting toward a provider.

#### 2.4.1 Client handling of a Drop

The policy allows a flow while it is inspecting (§4.1), so on the client the
first packets of a flow have already gone to a provider when the encrypted
heuristic drops it. `RemoteUserNatMultiClient.securityRoute` therefore:

- **Fails fast.** When the packet that decides `dropEncrypted` would move a
  provider-routed flow elsewhere, it is blocked and the app is told: a TCP reset
  (`deliverTcpPolicyReset`, as for SMTP rejects) or an ICMP port unreachable for
  UDP (`icmpUnreachableForPolicyReject`: v4 type 3 code 3, v6 type 1 code 4).
  This applies with the kill switch on or off: with it off, continuing the flow
  from the device's own address cannot work for TCP and misleads UDP. A flow the
  user already routes locally never reached a provider and is left alone.
- **Hints the destination.** The (address, port, transport) is kept in a bounded
  local cache (`PolicyHintTtl` 10 min, `PolicyHintMaxCount` 1024). Every later
  packet to it is handled as that drop from its first packet: routed locally when
  the local security bypass is on (kill switch off), so the app's retry works
  outside the tunnel as the bypass promises; blocked and rejected at once when it
  is off. An incident (BitTorrent, non-public destination) never creates a hint
  and is never routed locally.
- **Names the reason.** `BlockAction.Reason` is `security-encrypted`,
  `security-bittorrent`, `security-port`, `security-ip`, `security-smtp`,
  `security` (a custom policy), `blocker` or `override`, so the apps can label
  blocks by the safety rules and offer the existing route-local override.

None of this changes what a provider accepts or sends anything off the device.

### 2.5 Statistics

`SecurityPolicyStatsCollector` accumulates outcome counts. Egress uses
`AddDestination`, ingress `AddSource`. By default counts are keyed by `(version,
protocol, port)` only; `includeIp` additionally keys by IP. `Stats(reset)` returns
`map[SecurityPolicyResult]map[SecurityDestination]uint64` and optionally clears.

Egress decisions also record a **verdict reason** (`SecurityPolicyReason`,
`ip_security_reason.go`): the rule that decided the packet (`cfaa-drop-ip`,
`cfaa-drop-port`, `cfaa-allow`, `bittorrent`, `drop-encrypted`, `inspecting`,
`allow-privileged`, `allow-gaming`, `allow-web-standard:<kind>`, `allow-rtp`,
`allow-http`, `allow-plaintext`, `allow-budget`, …). `Reasons(reset)` returns
`map[SecurityPolicyReason]map[SecurityDestination]uint64`, reset independently of
`Stats`. Reason counts are **always keyed by `(version, protocol, port)` only**,
even when `includeIp` is set, share the result table's per-key cardinality bound,
and are local diagnostics: nothing about them is sent off the device.

### 2.6 Files

| File | Hand-written? | Role |
|------|---------------|------|
| `ip_security.go`             | yes | Interface, results, egress/ingress/disable policies, `isPublicUnicast`, stats. |
| `ip_security_reason.go`      | yes | Verdict reasons recorded with the egress statistics. |
| `ip_security_cfaa.go`        | yes | CFAA detector: settings, verdicts, port policy, `cfaaBlockedIp4`/`cfaaBlockedIp6` range lookups. |
| `ip_security_telegram.go`    | yes | Exact Telegram call-reflector endpoint/port exception. |
| `ip_security_gaming.go`      | yes | Provider-prefix + transport + documented-remote-port gaming exceptions. |
| `ip_security_cfaa_block.go`  | **generated** | Packed IPv4 + IPv6 blocked-range tables + source attribution. |
| `ip_security_dmca.go`        | yes | Per-flow DPI state machine, including BitTorrent signatures, RTP probation, and the encrypted heuristic. |
| `ip_security_webstandard.go` | yes | TLS/DTLS/QUIC/STUN/TURN/RTP/RTCP framing and header classifier. |
| `ip_security_appstandard.go` | yes | WireGuard/OpenVPN/RTMP/Levin/RakNet application-standard detectors. |
| `ip_security_appstandard_ethereum.go` | yes | Ethereum devp2p discovery v4 (keccak256 packet hash) and RLPx auth (length-exact, on-curve secp256k1 key) detectors. |
| `security/main.go`           | yes | Generator for the block tables (`go generate ./...`). |
| `*_test.go`                  | yes | Behavior, invariant, cross-check, and zero-alloc tests. |

---

## 3. Layer 1 — CFAA static endpoint reputation

`cfaaDetector.inspect(ip, port, protocol, version)` returns a three-way verdict so
the caller can tell a definitive decision from "no opinion":

| Verdict     | Meaning                                                            | Egress effect    | Ingress effect |
|-------------|-------------------------------------------------------------------|------------------|----------------|
| `cfaaDrop`  | Blocked IP, or abused / privileged-non-whitelisted port           | Drop             | Drop           |
| `cfaaAllow` | Structured system protocol that must **not** be entropy-inspected | Allow (skip DPI) | Allow          |
| `cfaaPass`  | No static verdict                                                 | Hand to DPI      | Allow          |

`cfaaAllow` exists so plaintext/structured protocols (NTP, IKE, plain DNS), plus
explicit endpoint exceptions such as Telegram call reflectors, are never handed to
the DMCA encrypted-payload heuristic. When `CfaaSecurityPolicySettings.Enabled` is
false, `inspect` always returns `cfaaPass`.

### 3.1 Decision procedure

1. Detector disabled → `cfaaPass`.
2. **IP reputation takes precedence over the port policy.** If the address is in the
   blocked-range table (§3.3) → `cfaaDrop` — IPv4 via `cfaaBlockedIp4`, IPv6 via
   `cfaaBlockedIp6`, each a binary search over its packed range table.
3. If `AllowTelegramCalls` is enabled and the endpoint is one of Telegram's
   published call-reflector IPv4 addresses on TCP or UDP 596–599, or the exact
   official protocol-v12 fallback at `91.108.9.38:595/TCP` → `cfaaAllow`.
4. Otherwise apply the port policy:

| Port(s)                                | Protocol | Verdict     | Rationale |
|----------------------------------------|----------|-------------|-----------|
| 6881–6889, 6969, 1337, 9337, 2710      | any      | `cfaaDrop`  | BitTorrent and unofficial BitTorrent-related ports |
| 123, 500, 4500                         | any      | `cfaaAllow` | NTP, IKE / NAT-T (wifi calling) — see Apple HT103229; high-entropy IKE must not be entropy-dropped |
| 53                                     | UDP      | `cfaaAllow` | plain DNS — structured, low-entropy (FIXME: upgrade to DoH inline) |
| 53                                     | TCP      | `cfaaDrop`  | DNS-over-TCP not whitelisted |
| 595                                    | TCP      | `cfaaAllow` | only for Telegram's exact `91.108.9.38` protocol-v12 fallback |
| 596–599                                | TCP/UDP  | `cfaaAllow` | only for exact published Telegram call-reflector IPv4s; all other hosts retain the privileged-port drop |
| 443, 853, 465, 993, 995                | any      | `cfaaPass`  | HTTPS/QUIC, DoT, secure email → DPI's privileged-port rule (§4.0): stateless BitTorrent signatures → Incident, anything else Allow |
| 80                                     | TCP      | `cfaaPass`  | HTTP → DPI (catches HTTP-tracker announces; plaintext web passes; FIXME upgrade to HTTPS inline) |
| 80                                     | UDP      | `cfaaDrop`  | not HTTP |
| < 1024 (any other privileged port)     | any      | `cfaaDrop`  | e.g. 22 (SSH), 179 (BGP) — insecure low ports stay blocked |
| ≥ 1024 (any other user/ephemeral port) | any      | `cfaaPass`  | handed to DPI on egress |

> **Behavior note.** The `cfaaPass` set for `≥ 1024` replaces an older blanket drop
> of ports `≥ 11000`. Legitimate plaintext / web-standard high-port traffic (games,
> WebRTC, QUIC/HTTP-3) now passes (and is then judged by DPI on its payload), while
> BitTorrent and sketchy-encrypted non-web traffic on those ports is still dropped.

The exhaustive, authoritative port→verdict table is `TestCfaaPortClassification`.

The Telegram exception is intentionally separate from the generic port table in
`ip_security_telegram.go`. Its compact ranges are an exact snapshot of
`https://core.telegram.org/getReflectorList` (154 IPv4 addresses / 616 endpoint
pairs as verified 2026-09-01), not Telegram's broader service networks. The
separate `91.108.9.38:595/TCP` entry is the protocol-v12 fallback in the current
official Telegram-iOS `OngoingCallContext`. The IP reputation check runs first
and therefore still wins over this exception. Set `AllowTelegramCalls=false` to
restore the ordinary privileged-port drop.

### 3.2 The blocklist: feeds, classes, attribution

The IPv4 blocklist aggregates public threat-intelligence feeds. For an egress
**destination** filter, the C2 / malware / hijacked-netblock class carries the
strongest signal; attacker-source lists are kept for breadth.

| Feed | Class | Format | License / attribution |
|------|-------|--------|-----------------------|
| The Spamhaus DROP list | hijacked/criminal netblocks | CIDR | Free; **credit + © + date retained** in the generated header |
| The Spamhaus DROPv6 list | hijacked/criminal IPv6 netblocks | CIDR (IPv6) | Free; © The Spamhaus Project |
| FireHOL level1 | composite (dshield + feodo + fullbogons + spamhaus_drop) | CIDR | Free (per-source terms) |
| abuse.ch ThreatFox | active malware C2 | CSV | CC0 |
| abuse.ch Feodo Tracker | botnet C2 | host | CC0 |
| stamparm/ipsum (level ≥ 3) | 30+ feed aggregate, confidence-thresholded | host | Unlicense |
| AlienVault OTX, Binary Defense, Blocklist.de, CINS Army, BruteForceBlocker, CruzIt, NiX Spam, Emerging Threats | attacker source / compromised / spam | host | public |
| abuse.ch SSLBL, darklist.de | (C2 SSL / attacker source) | host | **dead** — empty since early 2025; kept (deprecated) so the generator reports emptiness instead of silently shipping nothing |

Feed selection originally derives from `github.com/Naunter/BT_BlockLists`.

> Licensing: this is *factual IP data fetched at build time* from public feeds, not
> ported code, so it does not implicate clean-room concerns. The only binding
> obligation is preserving the Spamhaus DROP credit line, which the generator
> captures verbatim from the live feed into the output header.

### 3.3 Generation, representation, and lookup

`security/main.go` (run via `go generate ./...`) produces — and **only** ever
writes — `ip_security_cfaa_block.go`:

1. **Fetch** feeds concurrently (descriptive custom UA, retries).
2. **Parse** — strip `#`/`;` comments; host IPs and CIDRs (IPv4 **and** IPv6) from
   line feeds, the IP column (minus `:port`) from CSV feeds; **octet-validated** via
   `net/netip`; **CIDR preserved** as a `[lo,hi]` range; reject any prefix broader
   than `/8` (IPv4) or `/16` (IPv6).
3. **Per-feed sanity** — each active feed has a min-count floor, and an
   IPv6-specific feed may additionally require an IPv6 entry floor. Any fetch error
   or below-floor active result is excluded from aggregation, and normal generation
   **aborts** without writing (protection never silently shrinks). `-allow-stale`
   permits a diagnostic reduced-data emission with an explicit disposition, but
   it is forbidden for release generation. A deprecated feed may be accepted
   empty only after a successful fetch; its fetch failures abort normally and
   remain visibly unavailable in diagnostic output.
4. **Merge** into a minimal sorted, pairwise-disjoint range set (overlapping and
   adjacent ranges coalesced).
5. **Subtract reserved space** — non-global IPv4 (RFC 6890: `0/8, 10/8, 100.64/10,
   127/8, 169.254/16, 172.16/12, 192.0.0/24, 192.0.2/24, 192.88.99/24, 192.168/16,
   198.18/15, 198.51.100/24, 203.0.113/24, 224/4, 240/4`) and non-global IPv6
   (`::/8, 64:ff9b::/96, 100::/64, 2001:db8::/32, 2002::/16, fc00::/7, fe80::/10,
   ff00::/8`). The egress path already refuses these before the table; clipping also
   strips the large reserved blocks feeds (e.g. FireHOL fullbogons) sometimes include.
6. **Aggregate sanity** — fail under `-min-ranges` (default 10k IPv4 ranges),
   under `-min-ranges6` (default 100 IPv6 ranges), or over `-max-coverage`
   (default 64M IPv4 addresses; poison guard).
7. **Emit** packed data, deterministic input provenance v1, and `gofmt`.

Each provenance record is emitted in declared feed order and contains the feed
name and exact URL, `content_sha256` and `content_bytes` for the decoded response
body before parser normalization (Go may transparently decompress HTTP responses),
the format-specific parsed counts, and a deterministic disposition. It contains
no fetch timestamp, response headers, or raw content. Dispositions are
`accepted_block`, `accepted_allow`, `accepted_deprecated`,
`skipped_below_min_count`, and `unavailable`.
Active feeds always require positive content bytes; a successfully fetched empty
deprecated feed is the sole accepted zero-byte case.

For release review, every configured feed must have exactly one record: each
nondeprecated security feed must be `accepted_block` with parsed IPv4-plus-IPv6
entries at or above its configured floor, and each deprecated feed must be
`accepted_deprecated`. No `skipped_below_min_count` or `unavailable` record is
releaseable. The hermetic generator tests enforce this contract against the
checked-in generated file. Release generation must never use `-allow-stale`.
Provenance hashes identify parser inputs, but this repository does not archive
or redistribute raw feed bodies; the checked-in generated tables plus the exact
Connect source hash in `release.lock` are the deployable authority.

A guard refuses to overwrite any file lacking a `DO NOT EDIT` marker, so the
generator can never clobber a hand-written source file.

**Representation.** The table is four constants — one `Count`/`Data` pair per family:

```go
const cfaaBlockedPrefixCount  = 64131
const cfaaBlockedPrefixData    = "\x01\x00\x9a\xb7\x01\x00\x9a\xb7" + ... // 8 bytes/record
const cfaaBlockedPrefix6Count = 214
const cfaaBlockedPrefix6Data  = "..."                                    // 32 bytes/record
```

Each `…Data` string holds `…Count` records, sorted ascending and pairwise disjoint —
IPv4 records are 8 bytes (big-endian `uint32` lo, then hi); IPv6 records are 32 bytes
(16-byte big-endian lo, then hi). Being string constants they live in the binary's
**read-only data segment**: package initialization performs **zero heap allocation**
and zero per-entry work. (Counts are illustrative — they drift with each regeneration.)

**Lookup.** `cfaaBlockedIp4(ip uint32)` and `cfaaBlockedIp6(ip [16]byte)`
(hand-written in `ip_security_cfaa.go`) binary-search the records **in place**,
decoding each record per probe via integer shifts — no allocation. The search finds
the first record whose lo exceeds the address; only the predecessor can contain it,
so it checks `addr <= hi` of that one record (~`log2(n)` comparisons over
cache-resident data). The IPv6 search (`cfaaSearch6`) compares 128-bit values as two
`uint64` halves.

> **Why ranges + binary search, not a map or trie.** The query is boolean
> membership, not longest-prefix-*value* lookup or live updates. Sorted merged
> ranges are the simplest structure that answers it, are naturally zero-allocation
> as static data, and are cache-friendly. The old `map[[4]byte]bool` could not
> represent CIDR at all and built itself with thousands of init-time `mapassign`
> allocations; a DIR-24-8 table needs ~16 MB; a Patricia/critbit trie adds
> pointer-chasing for no benefit here.

Regenerate: `go generate ./...` (or `cd security && go run .`). Flags: `-out`,
`-timeout`, `-allow-stale` (diagnostic only; forbidden for release),
`-max-coverage`, `-min-ranges`, `-min-ranges6`, `-max-bytes`, `-force`.

---

## 4. Layer 2 — DMCA stateful deep-packet inspection

Reached only when the CFAA layer returns `cfaaPass`. The detector keeps a small
per-flow (5-tuple) state object and advances it one packet at a time until a
**terminal verdict**, evaluated on the **outbound** direction.

### 4.0 Privileged destination ports

A destination port below 1024 that the CFAA layer passes (443, 853, 465, 587,
993, 995, 80/tcp) is a **trusted service port**: no flow state is created and the
encrypted heuristic does not run, so non-TLS encrypted services there (Telegram
MTProto, OpenVPN/TCP, Noise messengers on 443) are allowed. A peer can still
listen on such a port, so when `InspectPrivilegedSignatures` is set (default) the
**stateless BitTorrent signatures (§4.3) run on every payload** and a match is
`bittorrent` → **Incident**. TLS (`0x16`/`0x17`), QUIC and ordinary HTTP fail each
signature's first comparison. The entropy heuristic deliberately never runs on
these ports (that would break apps that work today; IPSECURITY-UPDATE4 §10.8).

### 4.1 Design principles

1. **Whitelist positive standards/endpoints; drop sketchy unidentified encrypted
   traffic.** Traffic positively identified as TLS, DTLS, QUIC, STUN/TURN, RTP,
   RTCP, or an exact provider-scoped gaming endpoint is allowed. Traffic that
   looks fully encrypted but matches neither is dropped. Plaintext that is not a
   BitTorrent signature is allowed.
2. **Allow until decided.** Packets pass while a flow is still being inspected;
   enforcement begins only at a terminal verdict. This bounds inspection cost and
   accepts a small bounded leak rather than buffering.
3. **Clean-room from public specs.** Every signature/constant derives from published
   protocol definitions (BitTorrent BEPs; TLS/DTLS/QUIC/STUN RFCs), never a
   third-party implementation. Byte signatures are non-copyrightable protocol facts.
4. **Egress only.**

### 4.2 State machine

Internal verdicts and their mapping (default settings):

| Internal verdict | Meaning | Maps to |
|------------------|---------|---------|
| `inspecting`     | Not yet decided | Allow (packet passes) |
| `bittorrent`     | Positive plaintext BitTorrent signature | **Incident** (ReportAbuse) |
| `dropEncrypted`  | Looks fully encrypted and is not a sanctioned standard/endpoint | **Drop** |
| `allow`          | Sanctioned standard/endpoint, plaintext-unknown, or budget exhausted | Allow |

`LogOnly` forces every verdict to Allow (still recorded);
`ReportBittorrentIncident=false` turns a BitTorrent verdict into a silent Drop.

On the **first** observation: TCP `sawFlowStart = (SYN present)`; UDP
`sawFlowStart = true`. Then per packet:

1. Empty payload (SYN / pure ACK): no signal; remain `inspecting`.
2. Else increment `inspectedPackets`, examine up to `MaxInspectionPayload` leading
   bytes:
   - BitTorrent signature (§4.3) → terminal `bittorrent`;
   - provider-scoped gaming endpoint (§4.4.1) → terminal `allow`;
   - flow already allowed by an application standard (§4.4.2) → `allow`, and
     terminal `allow` once the budget is spent (the BitTorrent check above keeps
     running until then);
   - whitelisted stateless standard (§4.4) → terminal `allow`;
   - single-packet application standard (§4.4.2), matched over the complete
     payload → `allow`, unless the bytes after its header carry any BitTorrent
     signature → terminal `bittorrent`;
   - a pending two-packet application candidate confirmed → `allow`; a failed
     candidate is judged normally below;
   - a packet that opens a two-packet application candidate → consumes budget,
     is **not** counted as encrypted, remain `inspecting`;
   - structurally valid RTP/SRTP → keep up to two SSRC candidates; two coherent
     observations → terminal `allow`;
   - looks encrypted (§4.5) → increment `encryptedPackets`;
   - else if `>= MinEncryptedPayload` → mark `sawPlaintext`;
   - else (too short) → inconclusive, only consumes budget.
3. Terminal when: `sawPlaintext` → `allow`; or `DropUnsanctionedEncrypted` &&
   `sawFlowStart` && `encryptedPackets >= EncryptedDecisionPackets` →
   `dropEncrypted`; or `inspectedPackets >= InspectionPacketBudget` → `allow` (gave
   up). Otherwise remain `inspecting`.

Once terminal, the verdict is cached; subsequent packets take a lock-free fast path.

**Why `sawFlowStart` gates the encrypted drop.** The heuristic is only meaningful if
the detector saw the flow from the beginning (so it could observe a plaintext
handshake). A TCP flow joined mid-stream (e.g. after a hot reload) is all encrypted
application data and would falsely look unsanctioned; gating on `sawFlowStart`
prevents that for TCP. UDP has no equivalent signal (see §4.7).

### 4.3 BitTorrent signatures (→ Incident)

Position-anchored in the first payload-bearing packet/datagram; provenance is the
BitTorrent BEPs.

| Sub-protocol | L4 | Signature | Source |
|--------------|----|-----------|--------|
| Peer-wire handshake | TCP | First 20 bytes: `0x13` + `"BitTorrent protocol"` | BEP 3 |
| HTTP tracker | TCP | `GET` line with `info_hash=` and (`/announce` or `/scrape`) | BEP 3 |
| Mainline DHT (KRPC) | UDP | Bencoded `d1:ad2:id20:` / `d1:rd2:id20:`, or a `d`-dict with `1:y1:{q,r,e}` and a `1:t` key | BEP 5 |
| UDP tracker connect | UDP | 8-byte magic `00 00 04 17 27 10 19 80` then 32-bit action == 0 | BEP 15 |
| µTP carrying a handshake | UDP | v1 µTP header (first byte `(type<<4)\|1`, type ≤ 4, ext ≤ 2) whose payload at offset 20 begins with the peer-wire handshake | BEP 29 |

Bare µTP is only a weak structural hint (~2% of random datagrams match) and is **not**
a drop trigger by itself; encrypted µTP (MSE/PE) is left to the encrypted heuristic.
LSD (BEP 14) is org-local multicast and never traverses an exit node (rejected by the
public-unicast rule).

### 4.4 Whitelisted web and communication standards (→ Allow)

A flow positively identified as one of these is allowed terminally and never
re-inspected, so later high-entropy application-data packets are not mistaken for an
unsanctioned encrypted flow.

| Protocol | L4 | Signature | Source |
|----------|----|-----------|--------|
| TLS | TCP | Record `0x16`, version `0x03 0x0X`, first handshake byte `0x01` (ClientHello) | RFC 8446 / 5246 |
| DTLS | UDP | Record `0x16`, version `0xFE 0xFF`/`0xFE 0xFD`, handshake byte at offset 13 `0x01` | RFC 6347 / 9147 |
| QUIC | UDP | Long-header + fixed bit (`byte0 & 0xC0 == 0xC0`), recognized version (v1, v2, version-negotiation, greasing, IETF drafts) | RFC 9000 / 9369 |
| STUN / TURN control | UDP/TCP | RFC 7983 first byte 0–3, aligned and exact declared length, magic cookie `0x2112A442` | RFC 8489 / 7983 |
| TURN ChannelData | UDP/TCP | Channel `0x4000–0x4fff`; exact declared data length, optional UDP alignment padding / required TCP padding | RFC 8656 / 7983 |
| RTCP | UDP | Version 2, packet type 192–223, exact compound/reduced-size lengths, type-specific minima, final-packet-only padding | RFC 3550 / 7983 |
| RTP / SRTP | UDP | Version 2 and complete CSRC/extension header; then two packets with the same SSRC/payload type and coherent sequence/timestamp movement | RFC 3550 / 7983 |

The framing and header matchers live in `webStandardDetector`
(`ip_security_webstandard.go`), with RTP probation held in `dmcaFlowState` because
RFC 7983's RTP byte range alone covers one quarter of possible first-byte values.
Two candidate slots permit initial audio/video interleaving; duplicates and
out-of-order packets are ignored, while implausible forward gaps reset probation.
`WebStandardSettings` exposes `Tls`, `Dtls`, `Quic`, `Stun`, `Turn`, `Rtp`, and
`Rtcp` toggles, all enabled by default. **Adding another legitimate encrypted
protocol means adding a positive detector here**, not loosening the entropy heuristic.

#### 4.4.2 Application standards (→ Allow)

`ip_security_appstandard.go` positively recognizes non-web protocols whose
payloads look fully encrypted. Each is a fixed-format header or a cross-packet
invariant from a public specification:

| Protocol | L4 | Rule | Source |
|----------|----|------|--------|
| WireGuard | UDP | 148-byte initiation (`01 00 00 00`) opens; a transport datagram (`04 00 00 00`, ≥ 32 bytes, length ≡ 0 mod 16) confirms. Mid-stream: two transport datagrams with the same receiver index and a little-endian counter increasing by 1..2^20-1 | WireGuard whitepaper §5.4.2, §5.4.6 |
| OpenVPN | UDP, TCP (2-byte length prefix) | hard reset from the client (opcode 7 or 10, key id 0, ≥ 14 bytes) opens; a control/ack/reset packet (opcode 4, 5, 7, 10, 11) repeating the 8-byte session id confirms. Mid-stream: two `P_DATA_V2` (opcode 9) packets with the same key id and 24-bit peer id | OpenVPN protocol (`ssl_pkt.h`) |
| RTMP | TCP, first payload | `03`, 4-byte time, 4-byte zero word (the digest variant is not matched) | Adobe RTMP 1.0 §5.2 |
| Levin | TCP, first payload | signature `01 21 01 01 01 01 01 01`, `protocol_version == 1` (LE u32 at 29), body length ≤ 100,000,000 | Monero `levin_base.h` |
| RakNet | UDP | open connection request 1/2 (`05`/`07`) with the 16-byte offline magic at offset 1, or unconnected ping (`01`) with it at offset 9 | RakNet `MessageIdentifiers.h`, `RakPeer.cpp` |
| Ethereum discovery v4 | UDP, any packet while inspecting | `hash ‖ signature (65) ‖ packet-type ‖ packet-data`, 99–1280 bytes, type 1–6, data a canonical RLP list that fits; then `hash == keccak256(signature ‖ packet-type ‖ packet-data)` over the complete packet | devp2p `discv4.md` (Wire Protocol) |
| Ethereum RLPx | TCP, first payload | the whole segment is the initiator's auth: EIP-8 `auth-size (BE u16) == len − 2`, `auth-size ≥ 282` (ECIES overhead 113 + the minimal RLP auth body 169), then `04 ‖ x ‖ y` an uncompressed secp256k1 point (`x, y < p`, `y² = x³ + 7 mod p`); or the pre-EIP-8 auth, exactly 307 bytes starting with such a point | devp2p `rlpx.md` (Initial Handshake, ECIES), EIP-8, SEC 2 (secp256k1) |

Rules: they run after the BitTorrent signatures, the provider exceptions and the web
standards; an allowed flow keeps checking the BitTorrent signatures until its
inspection budget is spent, and a single-packet match whose remaining bytes carry
a BitTorrent signature is `bittorrent`. A two-packet opener is not counted as
encrypted, so a random BitTorrent flow whose first blob happens to match an opener
(≈ 2^-32 per flow) is dropped one packet later; repeated openers end at the budget.
The WhatsApp Noise "WA" framing is **not implemented**: its bytes must first be
confirmed against a capture. Until then the Meta prefix exception (§4.4.1a) admits
WhatsApp's TCP/5222. `AppStandardSettings` (`Dmca.App`) has `Enabled`,
`WireGuard`, `OpenVpn`, `Rtmp`, `Levin`, `RakNet`, `EthereumDiscv4`,
`EthereumRlpx`, all on by default; nil disables them.

**Ethereum and the fan-out threat model.** Ethereum nodes talk to many peers on
user ports with payloads random from byte 0 — the flow-level profile of
encrypted BitTorrent. They are admitted per flow only by a cryptographic
invariant the flow itself carries, never by port, fan-out or entropy:

| Detector | Random payload | MSE/PE (`Ya` 96 + `PadA` 0–512) | Plain peer wire / tracker | µTP, DHT, UDP tracker |
|----------|----------------|---------------------------------|---------------------------|------------------------|
| discovery v4 | ≤ 2^-256 (keccak), and the hash only runs for the ≈ 1 in 200 datagrams that pass the structural checks | TCP: never (UDP only). Over µTP it is a UDP datagram: ≤ 2^-256 | never (TCP) | first bytes are fixed (`(type<<4)\|1`, `d1:`, the tracker magic) and would have to equal a keccak256 output: ≤ 2^-256; the signatures run first anyway |
| RLPx EIP-8 | 2^-16 (size) × 2^-8 (`04`) × ≈ 2^-255 (on curve) ≈ 2^-279 | same as random; the 148-byte and every length < 284 can never match | never: `13 42` declares 4,930 bytes and byte 2 is `i`; the signatures run first | never (UDP) |
| RLPx pre-EIP-8 | only at exactly 307 bytes: 2^-8 × ≈ 2^-255 | only `PadA = 211`: ≈ 2^-263 | never | never (UDP) |

A modified client can forge either invariant (compute a keccak, use a real
curve point), which is the accepted disguise class (§4.8); a stock BitTorrent
client cannot. The responder's ack/pong is ingress and is not inspected, so no
reverse-direction confirmation is used — none is needed at these
probabilities. **Discovery v5 is not detectable**: its header is AES-CTR
masked with the first 16 bytes of the *destination* node id, which the exit
never sees, so a v5 packet is indistinguishable from random; it shares the v4
socket, so a v5 datagram before any v4 packet on the same 5-tuple counts toward
the heuristic (a v4 packet while still inspecting admits the flow). An RLPx auth
split across TCP segments (MSS below ~500) is not matched and is dropped as
before.

#### 4.4.1 Steam/Valve provider exception (→ Allow)

`ip_security_gaming.go` allows Steam traffic only when all three dimensions match:

- destination belongs to Valve's published AS32590 Steam list (11 IPv4 prefixes or
  4 IPv6 prefixes, snapshot verified 2026-09-01);
- transport is TCP or UDP; and
- the destination port is labeled **remote** by Steam Support: TCP
  `80`, `443`, or `27015–27050`; UDP `3478`, `4379`, `4380`, or
  `27000–27250` (the union of its overlapping game/P2P ranges).

Steam's local Remote Play and dedicated/listen-server guidance is not independently
allowlisted. TCP 80/443 already follow the privileged/web policy, but remain in the
source-faithful remote-port matcher. For inspected high ports, the DMCA state machine
checks positive BitTorrent signatures first, then this exception, then protocol and
entropy classification. The CFAA IP blocklist is an earlier layer and remains
authoritative.

The source data is Valve's
[`ipv4.txt`](https://as32590.net/ipv4.txt) and
[`ipv6.txt`](https://as32590.net/ipv6.txt), linked from the official
[Steam required-ports page](https://help.steampowered.com/en/faqs/view/2EA8-4D75-DA21-31EB).
`GamingSecurityPolicySettings.Enabled` and `AllowSteam` are both enabled by default;
the settings are nested under `DmcaSecurityPolicySettings.Gaming`. A nil `Gaming`
setting disables all gaming exceptions.

#### 4.4.1a Meta/WhatsApp provider exception (→ Allow)

`ip_security_messaging.go` allows WhatsApp's chat session only when all three
dimensions match:

- destination belongs to the address space Meta registers for its own AS32934
  (23 IPv4 prefixes or 5 IPv6 prefixes, snapshot 2026-10-04);
- transport is TCP; and
- the destination port is `5222`, the WhatsApp chat port. `5223` is not included
  until a capture shows WhatsApp using it.

WhatsApp runs Noise on 5222, and after its short framing header the payload is
random, so the encrypted heuristic dropped it after `EncryptedDecisionPackets`
payloads. The exception is evaluated right after the gaming exception. Unlike the
Steam allow it is not terminal on its first payload: like an application standard
the flow keeps checking the BitTorrent signatures for its whole inspection budget,
so a recognized BitTorrent flow to Meta on 5222 is still `bittorrent`. This
restores legitimate traffic the heuristic falsely drops; it disables no detector.

The source data is the `route`/`route6` objects for origin AS32934 in RADb
(`whois -h whois.radb.net -- '-i origin AS32934'`, the registry Meta documents for
its address space), keeping only the objects Meta maintains itself (RADb
`MAINT-AS32934` and the RIPE objects of its own maintainers), collapsed to their
covering prefixes. `MessagingSecurityPolicySettings.Enabled` and `AllowWhatsApp`
are both enabled by default under `DmcaSecurityPolicySettings.Messaging`; a nil
`Messaging` setting disables all messaging exceptions.

### 4.5 The "fully encrypted" heuristic

A payload is considered fully encrypted only if **all** hold over the inspected
prefix (clamped to `MaxInspectionPayload`):

- length `>= MinEncryptedPayload` (short payloads → inconclusive);
- printable-ASCII fraction `<= EncryptedMaxPrintableFraction`;
- set-bit fraction within `EncryptedPopcountBand` of 0.5;
- normalized Shannon entropy `>= EncryptedMinNormalizedEntropy`.

This is a *category* signal (random vs. not), **not** a positive BitTorrent
classifier — which is exactly why the positive standards whitelist is mandatory before it
can drop.

### 4.6 Configuration (`DmcaSecurityPolicySettings`)

| Setting | Default | Meaning |
|---------|---------|---------|
| `Enabled` | `true` | Master switch for payload inspection. |
| `LogOnly` | `false` | Evaluate + record, never enforce (dry-run). |
| `DropBittorrentSignature` | `true` | Enforce on positive BitTorrent signatures. |
| `ReportBittorrentIncident` | `true` | Signature hits → Incident vs. silent Drop. |
| `DropUnsanctionedEncrypted` | `true` | Enforce the encrypted heuristic. |
| `Gaming.Enabled` | `true` | Master switch for provider-scoped gaming exceptions. |
| `Gaming.AllowSteam` | `true` | Allow Valve-prefix + documented-remote-port Steam traffic after BitTorrent checks. |
| `Messaging.Enabled` | `true` | Master switch for provider-scoped messaging exceptions. |
| `Messaging.AllowWhatsApp` | `true` | Allow TCP/5222 to Meta's own AS32934 prefixes after BitTorrent checks (§4.4.1a). |
| `App.Enabled` | `true` | Master switch for application standards (§4.4.2). |
| `App.WireGuard`, `.OpenVpn`, `.Rtmp`, `.Levin`, `.RakNet` | `true` | Individual application-standard detectors. |
| `App.EthereumDiscv4`, `.EthereumRlpx` | `true` | Ethereum devp2p discovery v4 and RLPx auth (§4.4.2). |
| `InspectPrivilegedSignatures` | `true` | Run the stateless BitTorrent signatures on privileged ports (§4.0); false restores the uninspected allow. |
| `InspectionPacketBudget` | `8` | Max payload packets before giving up (→ Allow). |
| `EncryptedDecisionPackets` | `3` | Encrypted-looking packets required before the heuristic drops. |
| `MaxInspectionPayload` | `512` | Max leading payload bytes examined per packet. |
| `MinEncryptedPayload` | `32` | Min payload length before "encrypted" may apply. |
| `EncryptedPopcountBand` | `0.10` | Max \|set-bit fraction − 0.5\|. |
| `EncryptedMaxPrintableFraction` | `0.50` | Max printable-ASCII fraction to be "encrypted". |
| `EncryptedMinNormalizedEntropy` | `0.85` | Min normalized entropy to be "encrypted". |
| `MaxFlows` | `65536` | Upper bound on tracked flows (memory). |

**Recommended rollout:** enable with `LogOnly: true`, measure would-be drops
(especially `dropEncrypted`) against production traffic, tune thresholds and the
whitelist, then set `LogOnly: false`.

### 4.7 Lifecycle and resource bounds

- **Keying.** Flows keyed by 5-tuple via `Ip6Path` (IPv4 mapped into v6), one table
  for both families.
- **Sharding.** 16 FNV-hashed shards keep per-packet lookup off a single lock;
  existing flows take a read lock, inserts a write lock, decided flows a lock-free
  atomic fast path.
- **Memory bound.** Each shard caps at `MaxFlows / 16`; over the cap the oldest flows
  are evicted (LRU by last-activity, `applyLruUserLimit`). No timer; eviction is lazy
  on insert.
- **Buffer safety.** Payload is read synchronously and never retained; flow state
  stores counters, two compact RTP candidates, and the verdict.

### 4.8 What it catches, and what it doesn't

**Reliably caught (near-zero FP):** plaintext peer-wire handshakes on any port;
mainline DHT, UDP/HTTP tracker traffic, µTP carrying a plaintext handshake. Catching
the plaintext control/discovery plane suppresses a swarm even when the peer-wire
stream is MSE-encrypted.

**Heuristic (lower confidence):** obfuscated peer-wire (MSE/PE) or encrypted µTP with
no plaintext control plane in view — caught by §4.5 after a leak of up to
`EncryptedDecisionPackets` packets.

Plaintext BitTorrent signatures are also caught on privileged ports (§4.0), so a
peer listening on 443 or a tracker announce on 80 is an incident.

**Not caught / evasions:** forced encryption with DHT/LSD/PEX disabled behind a
whitelisted-protocol disguise (a fake TLS ClientHello, a WireGuard/OpenVPN/RTMP
header prefixed by a modified client, or a forged discv4 hash / RLPx curve
point); encrypted BitTorrent on a privileged port
(the entropy heuristic does not run there by design); a UDP flow joined
mid-stream (handshake missed; TCP is protected by `sawFlowStart`, UDP is not);
nested tunneling (VPN-over-VPN — WireGuard and OpenVPN are now admitted
deliberately: abuse inside them exits from the VPN server's address).

**Accepted FP surface (dropped by design):** fully encrypted protocols with no
plaintext structure on inspected ports: MSE/PE BitTorrent and encrypted µTP (the
target), Mosh, ZeroTier, Bitcoin BIP324 v2, Ethereum discovery v5 (masked with
the destination node id) and an RLPx auth split across TCP segments, obfs4, the
RTMP digest handshake, and
proprietary game/voice crypto outside a scoped exception. To spare one, add a
positive protocol detector (§4.4, §4.4.2) or a provider-prefix + remote-port
exception (§4.4.1, §4.4.1a). Quotas or a provider opt-in tier for the long tail are not
implemented (IPSECURITY-UPDATE4 §6.5, §10). Ethereum node operators keep peer
discovery by leaving discovery v4 enabled, which is admitted (§4.4.2).

---

## 5. Provenance and clean-room

- **CFAA blocklist** is factual IP data fetched at build time from public feeds (not
  ported code); only the Spamhaus DROP / DROPv6 credit must be retained.
- **DMCA detector** is a clean-room implementation from public protocol specs:
  BitTorrent BEP 3 / 5 / 14 / 15 / 29; TLS RFC 8446 / 5246; DTLS RFC 6347 / 9147;
  QUIC RFC 9000 / 9369; STUN RFC 8489; TURN RFC 8656; RTP/RTCP RFC 3550 and
  multiplexing RFC 7983; WireGuard whitepaper; OpenVPN protocol documentation;
  Adobe RTMP 1.0; Monero `levin_base.h`; RakNet message identifiers; Ethereum
  devp2p `discv4.md`, `rlpx.md` and EIP-8, with secp256k1 from SEC 2. Byte
  signatures and constants are functional protocol facts, not derived from any
  third-party implementation.
- **Steam exception** is factual prefix and port data published by Valve/Steam;
  no client or server implementation code is incorporated.
- **Meta exception** is factual prefix data from Meta's own AS32934 route
  registrations; no client or server implementation code is incorporated.

---

## 6. Limitations and FIXMEs

- **IPv6 reputation coverage is feed-limited.** Both families now have a reputation
  table, but public feeds carry far fewer IPv6 entries than IPv4 (currently ~200 v6
  ranges vs. ~64k v4), so IPv6 destinations lean more on the port policy in practice.
- **Plain DNS (53/udp) and HTTP (80/tcp) are allowed** pending inline protocol
  upgrade (DoH / HTTPS).
- **The CFAA policy is static.** Standing TODO: let a control message adjust the
  rules at runtime. DMCA settings are constructed at policy creation; threading them
  from higher-level data-plane settings is a follow-up if runtime config is needed.
- **Feeds are a build-time snapshot.** The block table is only as fresh as the last
  regeneration; there is no runtime feed refresh.
- **Application standards are spec-derived, not capture-verified.** The fixtures in
  `testdata/ipsecurity` are synthesized from the specifications; captures from the
  real apps (Roblox, WhatsApp on 5222, X Spaces, console/voice crypto) are still
  needed to confirm which of them trip the heuristic.
- **Provider exceptions are snapshots.** Valve may update AS32590 prefixes or Steam
  ports, and Meta its AS32934 registrations; refresh `ip_security_gaming.go` and
  `ip_security_messaging.go` from the cited sources when this policy is maintained
  (for example with the CFAA feed build).

---

## 7. Testing

`go test -run 'Cfaa|Dmca|WebStandard|Security|Fixture' ./` covers both layers:

- **CFAA:** `TestCfaaPortClassification` (full port table), `TestCfaaBlockedIps` (IP
  precedence), `TestCfaaDisabled`, `TestCfaaIngressMirrorsSourceDrops`,
  `TestCfaaBlockedPrefixInvariant` / `TestCfaaBlockedPrefix6Invariant` (packed data
  sorted/disjoint, v4 and v6), `TestCfaaBlockedIp4BruteForce` / `TestCfaaSearch6`
  (binary search vs. independent linear scan, v4 and v6), `TestCfaaInspectV6` (v6 port
  policy + IP block), `TestCfaaBlockedIp4ZeroAlloc` (allocation-free lookup).
- **Telegram:** exact range boundaries, holes, ports/transports, toggle behavior,
  and end-to-end ingress/egress coverage in `ip_security_telegram_test.go`.
- **Gaming:** exact Valve prefix snapshots, first/last/adjacent addresses, remote
  port boundaries, address-family/direction/toggle behavior, zero-allocation lookup,
  encrypted-flow allowance, and BitTorrent precedence in
  `ip_security_gaming_test.go`.
- **DMCA / communications:** signature, encrypted-heuristic, state-machine, and
  lifecycle tests in `ip_security_dmca_test.go`; strict STUN/TURN/RTP/RTCP parsing
  in `ip_security_webstandard_test.go`; stateful RTP continuity and near-miss
  enforcement in `ip_security_rtc_test.go`.
- **Application standards:** positive flows, near misses, toggles, BitTorrent
  precedence over every detector, MSE/encrypted µTP still dropped (including the
  148-byte variant), privileged-port signatures, zero allocation and a fuzz target
  in `ip_security_appstandard_test.go`; for Ethereum, positive discv4/RLPx
  flows, near misses of every structural and cryptographic check, every
  BitTorrent variant (MSE/PE paddings including 148 and 211, look-alikes that
  pass every structural check, encrypted µTP, the plaintext signatures on 30303
  and on privileged ports), random payloads, a differential fuzz target and
  zero allocation in `ip_security_appstandard_ethereum_test.go`.
- **Client fail fast:** `ip_remote_multi_client_policy_reject_test.go` (reset,
  unreachable, retry routing, incidents never hinted) and
  `ip_policy_hint_test.go` (hint ttl/bound with an injected clock, ICMPv6, block
  action reasons, override and batch paths).
- **Fixtures:** `ip_security_fixture_test.go` replays the packet fixtures in
  `testdata/ipsecurity/` (synthesized from protocol specifications; see its
  README) through `InspectEgress` as the multi-client send path does and checks
  each fixture's expected verdict. Reason attribution and bounds are in
  `ip_security_reason_test.go`.
