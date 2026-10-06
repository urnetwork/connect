# IPSECURITY-UPDATE4 — Admitting legitimate non-web encrypted application traffic without weakening provider protection

Status: proposal (research complete, no code changed) · Scope: `connect` data plane security policy, with sdk/app follow-ups noted · Companion to `docs/IP_SECURITY.md` (the authoritative spec; this document proposes its next revision)

Implementation status (branch `fix/ipsecurity-update4` in connect, sdk, localizations, android, apple, windows, linux):

- **Implemented:** Phase 0 (verdict-reason statistics, `ip_security_reason.go`; the fixture harness, `testdata/ipsecurity` — synthesized from the Appendix A specifications, not captures); Phase 1 (`ip_security_appstandard.go`: WireGuard, OpenVPN, RTMP, Levin, RakNet) and the §6.3 privileged-port BitTorrent signatures; Phase 2 (fail fast with TCP reset / ICMP unreachable, the policy-local hint cache, `BlockAction.Reason`); the sdk reason surfacing and verbose reason table; the app labels and the kill-switch text.
- **Implemented on `fix/ipsecurity-ethereum` (connect, merges after this branch):** the §10.2 decision — Ethereum devp2p discovery v4 and RLPx are admitted by cryptographic invariants (`ip_security_appstandard_ethereum.go`, `App.EthereumDiscv4`, `App.EthereumRlpx`, default on); discovery v5 is not detectable and stays dropped. See §10.2.
- **Not implemented:** the Noise "WA" detector (`NoiseIm`, gated on a capture that does not exist; no setting is shipped for it); §5 option B and the §6.5 curated-port quota (conditional on Phase 0 field data); §5 option D and every §10 open question (product/legal decisions); §9.3 rollout mitigations (a) and (b) — see the branch report: (a) needs a Phase 1 release version or hash to target and a per-flow placement preference inside the exit-selection machinery, (b) needs flow-level attribution of an aggregate per-source counter and cannot move a live TCP flow between exits, so neither can be done cleanly here; the `PacketStats` security/override split (not needed by the apps, which read `BlockAction.Reason`).

Research basis: the code at connect `8c76f568` (2026-09-30), sdk `8f4d101d`, android `d1979043`, apple `b2fc0802`; the git history of `ip_security*.go`; and the factury support inbox (7,853 items, 20 tagged `site-or-app-blocked`). Inbox items are cited by directory name under `/Users/brien/urnetwork/factury/support/inbox/`.

---

## 0. Summary

The triage brief says: "the provider's egress check drops fully encrypted traffic unless it is a recognised standard protocol (TLS, QUIC, DTLS, STUN, TURN, RTP); the only gaming exception is Steam," and attributes the Roblox / Monero / SimpleX / IPTV reports to it. Reading the code confirms the mechanism exactly (`ip_security.go:502-510` → `ip_security_dmca.go:331-349`), but **corrects the attribution**:

1. **Two different policies have dropped these apps, at different times.** From 2025-05-14 (`1c3ac5a8`) to 2026-06-27 (`ac91c55c`) the egress policy dropped **every destination port ≥ 10000 (then ≥ 11000)** regardless of payload. Every inbox report that names Monero (`2533`, 2025-07-09), IPTV (`2491`, 2025-07-06), SimpleX/Mihon (`2511`/`2512`, 2025-07-08), WhatsApp (`265`, 2025-07-06), Roblox (`7309`, 2026-05-05) and the connect#128 checklist (`100`, 2025-07-08) was filed **inside that window**. Monero p2p/RPC (18080/18081/18089), Session storage nodes (22021), Roblox game servers (UDP 49152-65535), Xtream-style IPTV portals (25461 etc.) and WebRTC media relays (high UDP ports) were all killed by the port rule, not by the entropy heuristic. The DPI that replaced the port rule (`ac91c55c`, hardened in `6a7d45ae`) is only in builds since the 2026-06/07 tags; providers that have not updated still run the port rule, and nothing in the inbox has been re-verified against the current code.
2. **The current heuristic has a different, real victim list.** It drops any protocol whose first three payload-bearing packets look random and are not TLS/DTLS/QUIC/STUN/TURN/RTP/RTCP, Steam or Telegram-reflector traffic, on destination ports ≥ 1024. From protocol facts that is: WireGuard, OpenVPN/UDP, ZeroTier, Tailscale direct paths, Mosh, Mumble voice, RTMP live-stream publishing, Noise-based messengers when they fall back to 5222 (WhatsApp, Threema), Ethereum devp2p, Bitcoin BIP324, and games with custom encryption (Xbox Live, PSN, Switch NEX, Genshin KCP, Call of Duty), plus any proprietary voice path that is not RFC 3550-shaped. Of the five apps on connect#128, **only X Spaces speaking (if its media is not STUN/SRTP-shaped) is plausibly affected by the current heuristic**; Monero (plaintext Levin + HTTP RPC), Session (HTTPS), SimpleX (TLS on 5223) and Mihon (HTTPS) should pass it as written, pending capture.
3. **The user-visible failure is made worse by how the client applies the verdict.** The same DPI runs on the client device before the provider (`ip_remote_multi_client.go:6009`). Because the policy is "allow until decided" (`docs/IP_SECURITY.md:312-314`), the first packets of a flow go to the provider and then, on the third encrypted packet, the client either silently blocks the rest of the flow (kill switch on) or — with the SDK default `DefaultRouteLocal: true` (`sdk/device_local.go:384`) — re-routes the rest of the flow out of the user's own IP (`ip_block_action.go:481-486`, `ip_remote_multi_client.go:6054-6066`). A TCP connection whose source IP changes mid-stream is dead either way, and no RST is sent (unlike the SMTP guard, `ip_smtp_policy.go:847-864`), so apps stall until their own timeout and then retry into the same cycle.
4. **There is one spec/code disagreement that weakens protection today:** `docs/IP_SECURITY.md:162` says BitTorrent over 443 is caught; `ip_security_dmca.go:601-603` skips all DPI, including the plaintext BitTorrent signatures, for every destination port < 1024, and `ip_security_dmca_test.go:435-440` pins that behavior.

**Recommendation** (§6): do not loosen the entropy heuristic and do not add a blanket or quota-based "unknown encrypted" allowance. Instead:

- **Phase 0 — measure.** Add a verdict-reason dimension to the local security stats (port-keyed, no IPs, no new wire data) and a committed packet-fixture probe harness, then re-test the connect#128 apps against the current code before changing any rule.
- **Phase 1 — positive detectors for the protocols the heuristic actually drops**, in the exact style `docs/IP_SECURITY.md:398-402` prescribes ("adding another legitimate encrypted protocol means adding a positive detector here, not loosening the entropy heuristic"): WireGuard, OpenVPN, RTMP, Levin (Monero), RakNet (Roblox / Minecraft Bedrock), and — after capture — the WhatsApp Noise framing. Each is a multi-byte protocol-fact fingerprint, most are confirmed across two packets, and none is forgeable by a stock BitTorrent client. Pair this with the compensating **tightening** of running the stateless BitTorrent signatures on privileged ports too, so net provider protection goes up.
- **Phase 2 — make the client's drop correct**: fail fast (TCP RST / ICMP unreachable to the app) at the first Drop verdict, and remember the destination in a short-lived policy-local hint so the app's retry is routed consistently (locally when the kill switch is off, refused immediately when it is on). This changes nothing on providers.
- **Defer** per-sender fan-out quotas and a provider opt-in tier (§5 options B and D): a quota does not prevent DMCA exposure (one monitored peer suffices) and an opt-in tier shifts rather than removes liability. Keep them as open questions with the mechanics sketched.

---

## 1. Problem statement

### 1.1 The brief, and what the code actually does

The brief points at `ip_security.go` "around lines 495-505". The decision is at `ip_security.go:468-512` (`inspectEgressForSender`):

```
474  if protocol.ProvideMode_Network == provideMode { return Allow }
481  if !isPublicUnicast(ipPath.DestinationIp)     { return Incident }
488  switch self.cfaa.inspect(destinationIp, port, protocol, version) {
489  case cfaaDrop:  return Drop
491  case cfaaAllow: return Allow
493  default:
502      switch v := self.dmca.classifyForSender(senderClientId, ipPath, payload); v {
503      case dmcaBittorrent:    return self.dmca.result(v)   // Incident by default
505      case dmcaDropEncrypted: return self.dmca.result(v)   // Drop by default
507      default:                return Allow
```

The comment at `:494-501` describes the intent correctly: a flow that looks fully encrypted is dropped *unless* it matched a sanctioned standard or the Steam exception. The actual classifier is `dmcaFlowState.advance` (`ip_security_dmca.go:270-355`), walked in §2.

So the description in the brief is **accurate about the mechanism**. It is **inaccurate about history and attribution**, which matters because the fix for "apps that failed in 2025" and the fix for "apps the current heuristic drops" are different.

### 1.2 Two policies, one symptom — the timeline

| Date | Commit | Egress rule for destination ports ≥ 1024 | Who it drops |
|---|---|---|---|
| 2025-05-14 | `1c3ac5a8` "ip update security rules" | `dPort < 1024` drop except an allowlist; **`10000 <= dPort` → drop** | everything on 10000+ |
| 2025-06-05 | `c246c749` "ip security: update rules" | threshold raised to **`11000 <= dPort`** ("note many games use 10xxx so we allow this"; "FIXME turn this off when we have better deep packet inspection") | Monero 18080/18081/18089, Session 22021, Roblox UDP 49152+, IPTV portals on 25461/8880+, WebRTC/voice relays on high UDP, WhatsApp call media, most game servers above 11000 |
| 2026-06-27 | `ac91c55c` "update dpi to better separate web standards from dmca risks" | port rule removed; `cfaaPass` for all ≥ 1024; stateful DPI: BitTorrent signatures → Incident, **fully-encrypted non-TLS/DTLS/QUIC/STUN → Drop** | WireGuard, OpenVPN/UDP, RTMP, Noise-IM on 5222, custom game crypto, … (§3) |
| 2026-08-15 | `63dd3d4e` "complete sender-scoped SMTP and DMCA hardening" | flow table keyed by authenticated sender | — |
| 2026-09-01 | `6a7d45ae` "security: admit sanctioned realtime traffic" | adds TURN, RTP continuity, RTCP, Steam (AS32590 + remote ports), Telegram reflectors | narrows the victim list |
| 2026-09-04 / 09-17 | `d65a05e0`, `56232afd` | provenance, memory bounds (`MaxFlows` 64 under the provider budget, `ip_provider_memory.go:75-77`) | — |

`d4195dac` (named in the brief) is `fix(provider): add missing help description for cli options` (2025-07-03, `provider/main.go`) and is unrelated to the policy.

All of the app-specific inbox reports were created between 2025-07-01 and 2026-05-05 (see §3), i.e. under the **port rule**. The two reports created after `ac91c55c` (`7787-whatsapp-broken-on-ios`, 2026-09-13; `7630-some-apps-fail-vpn`, 2026-07-24) do not say which provider build served them; a provider on a pre-June-2026 build still enforces the port rule, and `IpProviderDiagnostics.build_version` (`protocol/ip.proto:31`) is the only way to tell.

### 1.3 Where the verdict is felt

The policy object is constructed from the same defaults in three places:

| Role | Construction | Inspect call | On Drop |
|---|---|---|---|
| Client device (production path) | `RemoteUserNatMultiClientSettings.SecurityPolicyGenerator = DefaultSecurityPolicyWithStats` (`ip_remote_multi_client.go:457`, built `:2457`) | `self.securityPolicy.InspectEgress(relationship, ipPath, payload)` (`:6009`), group path `:6397` | `blockActionApply` (`ip_block_action.go:478-514`): `localSecurityBypass` → send via `localUserNat` (`ip_remote_multi_client.go:6060`), else count as blocked and return false (`:6054-6058`). No RST, no ICMP. |
| Provider (exit) | `RemoteUserNatProviderSettings.SecurityPolicyGenerator = DefaultProviderSecurityPolicyWithStats` (`ip.go:7075`) = `Reverse(DefaultSecurityPolicyWithStats)` (`ip_security.go:388-390`); under a memory budget rebuilt from defaults with `MaxFlows = 64` (`ip_provider_memory.go:65-78`) | `inspectAndRefreshIngressForSenderBorrowed` on each tunnel packet (`ip.go:9817-9824`; fragments `:9712`) | silently dropped; `recordProviderBlock` (`ip_provider_diagnostics.go:108-127`) bumps per-source counters that are sent back to that client as `IpProviderDiagnostics` (`:143-150`, `protocol/ip.proto:30-38`); Incident → `client.ReportAbuse(source)` (`ip.go:9754`, `:9854`) → `PeerAudit{abuse=true}` to the control plane (`transfer.go:3004-3010`) with no reason or destination. |
| Single-destination client (`RemoteUserNatClient`) | `DefaultSecurityPolicy(cancelCtx)` (`ip.go:10136`) | `:10216` | same bypass rule (`:10284-10289`); not a production path (comment `:10247-10259`). |

Consequences visible to a user:

- **Client-side flip.** `advance` returns `dmcaInspecting` (→ Allow) for the first two encrypted packets and `dmcaDropEncrypted` on the third (`ip_security_dmca.go:348`, `EncryptedDecisionPackets: 3` at `:133`). Packets 1-3 have already left through a provider's IP. With the kill switch off (`routeLocal == true`, the SDK default at `sdk/device_local.go:384`, wired to `SetLocalSecurityBypass` at `:3033`), packet 4 onward leaves through the device's own IP. For TCP that is a dead connection; for UDP it depends on whether the protocol tolerates an endpoint change (WireGuard does — which means a WireGuard-in-URnetwork tunnel today silently ends up *outside* URnetwork). With the kill switch on, the flow stalls with no error until the app's own timeout.
- **Provider-side silence.** Even if a client did not run the DPI, the provider would reach the same verdict on the same bytes and drop silently. The counters it publishes are exposed to the SDK as `Exit.ProviderBlockIngressPacketCount` etc. (`sdk/reliability_controls.go:436-451`) but neither app reads them, and nothing tells the user *why* (there is no reason field on `BlockAction`, `ip_block_action.go:73-96`).
- **Existing workarounds leak.** Android excludes Session and Discord from the tunnel by default (`android/.../MainService.kt:58-65`, `addDisallowedApplication` at `:621-624`); per-app and per-host "route locally" rules exist (`sdk/device.go:200-215`, `device_local.go:6551-6633`); iOS/macOS have host rules only (`apple/.../SplitRulesView.swift:741`). Each of these makes an app work by taking it out of the VPN, which is the opposite of what a privacy user wants for a messenger.

### 1.4 Why this is worth a design pass rather than a one-line allowlist

The heuristic is doing its job: it is the only thing that catches MSE/PE-obfuscated BitTorrent and encrypted uTP when the plaintext control plane (DHT/tracker) is not in view (`docs/IP_SECURITY.md:488-490`). Any allowance has to be argued against that threat (§4). The design question is which *shapes* of encrypted traffic can be admitted with evidence that a stock BitTorrent client cannot produce, and how the client should behave for everything else.

---

## 2. Current policy walkthrough

### 2.1 Egress pipeline (client egress; provider ingress via `Reverse`)

1. **Relationship.** `egressRelationship(source, client)` (`ip_security.go:342-347`) is `Network` only when both sides are `ProvideMode_Network`; everything else, including `FriendsAndFamily` and `Stream`, is treated as Public. `ProvideMode` is a set of flags (`protocol/transfer.proto:319-332`); only `Network` is referenced by the security code (`ip_security.go:343, 433, 474, 578`).
2. **Public unicast only.** Private/loopback/link-local/multicast/unspecified and the reserved v6 prefixes → `Incident` (`:481-483`, `isPublicUnicast` `:797-813`). This is what kills multicast IPTV and LAN game discovery; it is correct for an exit node and not in scope.
3. **CFAA static layer** (`ip_security_cfaa.go:88-162`): blocklist IP → drop (`:93-107`); ICMP → allow (`:112-114`); Telegram reflectors → allow (`:120-122`); then the port table:
   - 6881-6889, 6969, 1337, 9337, 2710 → **drop** (`:125-127`)
   - 123, 500, 4500 → **allow, never inspected** (`:128-131`)
   - 53/udp → allow; 53/tcp → drop (`:132-139`)
   - 443, 853, 465, 993, 995, 587/tcp → **pass to DPI** (`:140-146`)
   - 80/tcp → pass; 80/udp → drop (`:147-154`)
   - any other port < 1024 → **drop** (`:155-157`) — SSH 22, RTSP 554, IRC 194, etc.
   - ≥ 1024 → **pass to DPI** (`:158-160`)
4. **DMCA stateful layer** (`ip_security_dmca.go:582-674`, `classifyForSender`):
   - not TCP/UDP → allow (`:590-594`)
   - **destination port < 1024 → allow with no inspection and no flow state** (`:601-603`). The comment says peers "run on ephemeral/high ports" and that this lets "Telegram MTProto on 443" through. This is why WhatsApp/Telegram/Threema on 443, OpenVPN/TCP 443 and anything else on a privileged pass-port is never entropy-dropped — and also why a BitTorrent handshake to a peer on 443 is never detected (§1, item 4).
   - flow state keyed by `(senderClientId, Ip6Path)` (`:158-164`), sharded, LRU-bounded, SYN replaces a stale generation (`:628-641`), FIN/RST retire (`:608-613`, `:658-667`), idle TTL 300 s (`:145`, `evictIdle` `:420-435`).
   - `advance` (`:270-355`), per payload-bearing packet, on the first `MaxInspectionPayload = 512` bytes:
     1. `detectBittorrentSignature` (`:303`, `:822-830`): BEP 3 handshake / HTTP tracker (TCP); DHT KRPC, UDP tracker connect, uTP carrying a handshake (UDP) → `dmcaBittorrent`.
     2. `isSanctionedGamingEndpoint` (`:306`, `ip_security_gaming.go:67-73`): Valve AS32590 prefixes (`:46-65`) × Steam remote ports (`:107-121`) → allow.
     3. `web.match` (`:312`, `ip_security_webstandard.go:59-76`): TLS ClientHello (`:101-112`), STUN stream, TURN ChannelData stream (TCP); DTLS ClientHello (`:115-129`), QUIC long header (`:132-154`), STUN (`:159-193`), TURN ChannelData (`:199-228`), RTCP (`:286-322`) (UDP) → allow.
     4. RTP header + two coherent packets per SSRC (`:318`, `observeRtp` `:216-254`, `parseRtpHeader` `webstandard.go:244-280`) → allow.
     5. `isHttpRequest` (`:323`, `:755-771`): plaintext HTTP/1.x request line (not CONNECT) on any port → allow.
     6. `payloadLooksEncrypted` (`:331`, `:838-850`): length ≥ 32, printable fraction ≤ 0.50, |popcount − 0.5| ≤ 0.10, normalized entropy ≥ 0.85 → `encryptedPackets++`; else if length ≥ 32 → `sawPlaintext`.
     7. Terminal (`:344-354`): `sawPlaintext` → allow; `DropUnsanctionedEncrypted && sawFlowStart && encryptedPackets ≥ 3` → **`dmcaDropEncrypted`**; `inspectedPackets ≥ 8` → allow (gave up); else keep inspecting.
   - `sawFlowStart` is the SYN for TCP and always true for UDP (`:284-288`), so a UDP flow re-inspected after a 300 s idle eviction is judged as if new.
5. **Result mapping** (`result`, `:699-717`): `dmcaBittorrent` → Incident (or Drop if `ReportBittorrentIncident=false`), `dmcaDropEncrypted` → Drop, both → Allow under `LogOnly`.

The group path (`inspectAndRefreshEgressGroupBorrowed`, `ip_security.go:426-462`) folds per-packet classify results conservatively (`conservativeSecurityPolicyResult`, `:200-219`).

### 2.2 Ingress pipeline

`inspectIngress` (`ip_security.go:573-589`) applies only the CFAA *drops* on the source endpoint. Return traffic is never entropy-inspected; a game server replying from a high port passes, a server replying from an unlisted privileged port (e.g. 554) is dropped.

### 2.3 What "fully encrypted" accepts and rejects today

| Opening payload | Verdict path | Result |
|---|---|---|
| TLS/DTLS/QUIC/STUN/TURN/RTCP first packet | `web.match` | allow (terminal) |
| RTP/SRTP, two coherent packets | `observeRtp` | allow |
| Plain HTTP request on any port | `isHttpRequest` | allow |
| Anything with ≥ 50 % printable or skewed popcount or low entropy, ≥ 32 bytes | `sawPlaintext` | allow |
| Payloads < 32 bytes | inconclusive | consume budget; 8 of them → allow |
| 3 random-looking payloads ≥ 32 bytes before any plaintext one | heuristic | **drop** |
| Anything on a CFAA pass-port < 1024 (443, 80, 465, 587, 853, 993, 995) | privileged skip | allow, uninspected |

Settings (`DmcaSecurityPolicySettings`, `ip_security_dmca.go:61-147`) are compile-time defaults; there is no control-plane or app path to change them (`docs/IP_SECURITY.md:524-526`; the only runtime hook is the Go-only `SecurityPolicyGenerator`, and the SDK's `SetClientSecurityPolicyGenerator`/`SetProviderSecurityPolicyGenerator` are `//gomobile:noexport`, `sdk/device_local.go:2122-2139`). The provider's memory-bounded path ignores custom generators and rebuilds from `Default*Settings` (`ip_provider_memory.go:71-77`), so any new setting must live in the defaults. `SecurityPolicyHash` (`ip_provider_diagnostics.go:27-72`) digests the settings JSON and both block tables, so adding a setting changes the hash reported to clients — useful for rollout (§9).

---

## 3. Per-app failure table

Legend for "rule that drops it": **PORT** = `11000 <= dPort` drop (2025-05-14 → 2026-06-27; still live on providers that have not updated); **ENT** = `dmcaDropEncrypted` (`ip_security_dmca.go:348`) in current code; **CFAA<1024** = privileged-port drop (`ip_security_cfaa.go:155-157`); **MCAST** = non-public-unicast Incident (`ip_security.go:481`); **none** = passes current code as written; **ROTATE** = multi-exit IP rotation, not the security policy. Confidence is about the *current-code* column and comes from protocol facts plus the code; "capture" means a packet capture is required to settle it (§6, Phase 0).

| App / protocol | Wire shape of the client's first payload-bearing packets (public protocol facts) | Historic rule (reports) | Current code | Confidence | Reports |
|---|---|---|---|---|---|
| **Roblox** game session | UDP to game server, dest 49152-65535 (Roblox's published port range). RakNet-derived: `ID_OPEN_CONNECTION_REQUEST_1` = `0x05` + 16-byte offline magic + protocol byte + zero padding to MTU; OCR2 structured. If the fork keeps the padding the first datagram is ~1400 bytes of zeros → `sawPlaintext` → allow. Join/API over HTTPS. | **PORT** (dest ≥ 11000) | **none** if RakNet shape retained; **ENT** if Roblox's fork sends high-entropy handshakes | medium → capture | `7309-roblox-error-with-vpn` (2026-05-05), `3042-games-not-working` |
| **Monero** p2p (monerod, wallet node sync) | TCP 18080. Levin: 8-byte signature `01 21 01 01 01 01 01 01`, u64 length, flags, `protocol_version = 1`; body is epee portable storage (`01 11 01 01 01 01 02 01`) with string keys — low popcount, many zero bytes → `sawPlaintext`. No transport encryption. | **PORT** | **none** (plaintext) | high | `2533-monero-wallet-sync-fails` (2025-07-09), `100-blocked-apps-support` |
| **Monero** wallet RPC (Monerujo remote node) | TCP 18081/18089 plain HTTP `POST /json_rpc HTTP/1.1` → `isHttpRequest`; or HTTPS/`--rpc-ssl` → TLS. | **PORT** | **none** | high | same |
| **SimpleX Chat** (SMP, XFTP) | TCP 5223 TLS 1.3 (ALPN `smp/1`) → `isTlsClientHello`; XFTP on 443 TLS; WebRTC calls via STUN/DTLS-SRTP. | neither (5223 < 11000) | **none**; the user's own follow-up says the problem is exit-IP stability ("work with fixed ip") → **ROTATE** | high | `2511-simplex-mihon-issues`, `2512-longer-node-connections-simplex` (2025-07-08), `100` |
| **Mihon** (manga reader) | HTTPS 443 / HTTP 80 to source sites and CDNs; occasionally HTTP on 8080-style ports → `isHttpRequest`. | neither | **none**; **ROTATE** and site-side bot checks on rotating IPs | high | `2511`, `2512`, `100` |
| **Session** (Oxen) | HTTPS to storage nodes on 22021 (and seed nodes on 4433), file server 443 → TLS on a high port → `web.match`. | **PORT** (22021 ≥ 11000) | **none** | medium-high | `100` only; Android excludes it from the tunnel by default (`MainService.kt:62-63`) |
| **X Spaces** | Listening: LL-HLS over HTTPS → none. Speaking: WebRTC — STUN binding first → allow; if media rides a TURN/UDP relay on a high port with non-RFC-3550 framing → ENT. | **PORT** (relay media on high UDP) | **none** for listeners; **ENT** possible for speakers | low → capture | `100` only |
| **IPTV** (Xtream/Stalker portals, HLS) | HTTP/HTTPS on 80/443/8080/8880/25461… → `isHttpRequest` or TLS. RTSP 554 → CFAA<1024. MPEG-TS over UDP multicast → MCAST. RTMP 1935 (some live sources) → see RTMP. | **PORT** for portals ≥ 11000 | **none** for HTTP/HLS; **CFAA<1024** for RTSP; **MCAST** for multicast; **ENT** for RTMP | medium | `2491-iptv-services-blocked` (2025-07-06), `7630-some-apps-fail-vpn` (TV/music apps, 2026-07-24) |
| **RTMP** publish/play (OBS → Twitch/YouTube/Facebook live, some IPTV) | TCP 1935: C0 `0x03` + C1 = 4-byte time, **4 zero bytes**, 1528 random bytes → first segment ≥ 500 random bytes → "encrypted" ×3 (C1 remainder, C2). RTMPS over 443 → fine. | PORT no (1935 < 11000) | **ENT** | high | `6937-live-stream-keeps-stopping` (Facebook Live, 2026-02-19) is consistent with it |
| **WhatsApp** chat | TCP 443 (privileged skip → allow) with fallback to **5222**: 4-byte `WA`+version header, 3-byte-length frames, Noise XX handshake (32-byte ephemeral, then encrypted static + payload) → random-looking ×3. | **PORT** for call media on high UDP | **none** on 443; **ENT** on 5222 | medium (the `265` fix-log probe claims 5222 is allowed — unverified; capture) | `265-welcome` (2025-07-06), `7787-whatsapp-broken-on-ios` (2026-09-13), `4846-4848` |
| **WhatsApp / Signal / FaceTime / Discord** calls | STUN to relays (3478 and high ports) → allow on first packet; SRTP (RTP-shaped) → allow after two packets. Discord's UDP IP-discovery packet is 74 mostly-zero bytes → `sawPlaintext`. Proprietary relay framing that is neither → ENT. | **PORT** for high relay ports | **none** for standards-shaped; **ENT** for proprietary media | medium → capture | `2798-discord-calls-windows-app` (2025-08-21), `2781-split-tunneling-rcs-broken` |
| **Telegram** | MTProto on 443 → privileged skip; on 5222/80 fallback → ENT (obfuscated MTProto is random); calls on 596-599 to reflectors → explicit allow (`ip_security_telegram.go`). | — | **none** on 443; **ENT** on 5222 | medium-high | none in inbox |
| **Threema** | TCP 5222 NaCl handshake (32-byte pubkey + nonce) → random; falls back to 443. | — | **ENT** on 5222, none on 443 | medium-high | none |
| **WireGuard** (incl. **Tailscale** direct, Mullvad/Proton WG) | UDP 51820/41641/any: Handshake Initiation = type `0x01`, 3 reserved zeros, 148 bytes, 128 of them random; then type-4 transport datagrams (len ≡ 0 mod 16). → "encrypted" ×3. | PORT for ≥ 11000 | **ENT** | high | none in inbox (users would see "VPN inside VPN does not connect") |
| **OpenVPN/UDP** | UDP 1194/any: `0x38` (HARD_RESET_CLIENT_V2) or `0x50` (V3) + 8-byte session id + (tls-auth HMAC / tls-crypt tag) + packet ids → random-looking. OpenVPN/TCP 443 → privileged skip. | — | **ENT** (UDP) | high | none |
| **ZeroTier** 9993/udp, **Mosh** 60000-61000/udp, **Mumble** voice 64738/udp | fully encrypted datagrams from the first packet (Mumble falls back to its TLS TCP tunnel). | PORT (≥ 11000) | **ENT** | high | none |
| **Ethereum** devp2p 30303 (RLPx auth is ECIES; discovery packets are hash+sig; discv4 and RLPx admitted since `fix/ipsecurity-ethereum`, §10.2), **Bitcoin** BIP324 v2 (64-byte ElligatorSwift key) | random from byte 0. Bitcoin v1 (`f9 be b4 d9` + `version`) is plaintext → allow; Core falls back to v1 after a v2 failure. | PORT (30303) | **ENT** | high | `7553-blockchain-access-lost` is a *website* (blockchain.com), not p2p |
| **Games**: Minecraft Java 25565 (plaintext handshake), Minecraft Bedrock 19132 (RakNet magic + zero padding), League/Valorant (ENet-style structured), Clash Royale | structured/plaintext openings → `sawPlaintext` | **PORT** for ≥ 11000 (Minecraft 25565, Bedrock 19132) | **none** | medium | `4232-4251-disconnects-while-gaming` (Clash Royale, 20 copies), `3042` |
| **Games with custom crypto**: Xbox Live 3074/49152+, PSN 3658/49152+, Switch NEX/PRUDP 45000-65535 (RC4 payload after tiny SYN packets), Genshin KCP 22101-22102 (20-byte handshake then XOR-encrypted data), Call of Duty (Demonware), PUBG Mobile, Zoom 8801-8810 | small structured first datagrams (< 32 bytes, inconclusive) followed by encrypted data | **PORT** for ≥ 11000 | **ENT** likely | low-medium → capture | `3042`, `1101-sugestao-de-melhoria` (Xbox Cloud) |
| **SSH** 22, **RTSP** 554, **IRC** 194, other privileged ports | — | CFAA<1024 (both eras) | **CFAA<1024** | high; product decision, out of scope (`364-questions`) | `364` |
| **BitTorrent** (any port), **DHT**, **trackers** | plaintext signatures → Incident; MSE/PE → ENT | drop (both eras) | **must stay dropped** | — | `156`, `248-urgent-issue-safety-precautions`, `1863-exclude-comcast-torrent-traffic`, `78`, `88` |

Takeaways:

- Of the connect#128 apps, none is dropped by the current code on protocol-fact grounds except possibly X Spaces speaking. All five were dropped by the port rule when the issue was filed. Session and Discord are *also* excluded from the Android tunnel by default, which hides whether they work through it.
- The current heuristic's confirmed-by-spec victims are VPN-in-VPN protocols, RTMP, and non-TLS messenger fallbacks; the uncertain set is proprietary game/voice crypto, which only captures can settle.
- SimpleX/Mihon are an exit-rotation problem (item `2512`), addressed by the existing affinity/`Pin` machinery (`ip_block_action.go:35-55`) and out of scope here except as an open question.

---

## 4. Threat model for providers

Providers are ordinary users' devices on home/mobile IPs (`docs/IP_SECURITY.md:24-37`; `protocol/audit.proto:7-15`). The policy exists to keep a provider's IP from being the visible origin of:

1. **DMCA / copyright complaints** — BitTorrent swarm participation. One completed connection to a *monitoring* peer is enough for a notice; it does not require many peers or much data. Stock clients announce over HTTP(S) trackers (HTTPS → TLS → allowed), DHT (plaintext → caught), PEX, and connect to peers on *random* high ports chosen by the peer, with optional MSE/PE encryption whose first message is a 96-byte DH key plus 0-512 bytes of random padding. Inbox: `248`, `1863`, `156`.
2. **CFAA-class abuse** — traffic to known-malicious/hijacked space (blocklist), scanning and brute force (privileged-port drops), amplification (UDP to reflectors — the 123/53 allowances are the residual exposure), spam (SMTP guard, `ip_smtp_policy.go`).
3. **Account/reputation harm** — the provider's IP ending up on bot/VPN lists (out of policy scope; see `82`, `2290`, `7313`, which also fail the honesty gate).

What "unrecognized fully encrypted traffic on a user port" can be, and what distinguishes it:

| Candidate | Distinguishing evidence available at the exit | Provider risk if admitted |
|---|---|---|
| MSE/PE BitTorrent peer-wire, encrypted uTP | random from byte 0; **many destinations per sender on peer-chosen random ports**; both sides' first messages are variable-length random blobs (96-608 bytes); no stable header bytes, no counters, no fixed lengths | DMCA — the whole reason the heuristic exists |
| WireGuard / OpenVPN / ZeroTier / Tailscale (nested tunnel) | fixed-format headers (type byte + reserved zeros + exact length 148 / opcode + repeated session id), 16-byte alignment, little-endian counters; **one destination**; the nested tunnel's own traffic exits from the *VPN server's* IP, not the provider's | **low**: the provider only ever talks to one VPN server; any abuse inside the tunnel is attributed to the VPN server's IP. The only cost is bandwidth, which the contract already prices. Nested tunnels do defeat the SMTP/BT DPI, but for traffic the provider's IP never originates. |
| Noise-based messengers (WhatsApp, Threema), MTProto fallback | fixed app header (`WA`+version, length-prefixed frames) or none (MTProto); 1-3 destinations in the vendor's published prefixes; per-vendor ports (5222) | low; the same traffic is already admitted uninspected on 443 |
| RTMP | `0x03` + zero word at a fixed offset; one destination | low |
| Custom game/voice crypto (Xbox, PSN, Genshin, Zoom) | vendor-owned prefixes; small structured openers; often documented ports; 1-3 destinations | low-moderate; the Steam precedent (prefix × port) applies |
| Mosh / Mumble / Ethereum / BIP324 | Ethereum: ~50 destinations on 30303 — a fan-out profile that *looks like* BitTorrent at the flow level; Mosh/Mumble: one destination on a documented port | Ethereum nodes are a genuine ambiguity at the flow level (many peers, random first bytes, user ports); resolved per flow by cryptographic invariants, not by fan-out (§10.2) |

Two conclusions drive the design:

- **A per-sender fan-out or rate quota cannot be the primary control.** It would still admit the first K encrypted peer connections, and K = 1 suffices for a DMCA notice. Quotas are only useful as a *secondary* bound on an allowance that is already protocol-fingerprinted (e.g. "a WireGuard flow to at most N distinct servers per hour", mostly to bound provider bandwidth), not as a substitute for identification.
- **Positive fingerprints are the right tool, and nested VPNs are the safest thing to admit.** Each detector must (a) be a protocol fact from a public spec (clean-room, as every existing detector is, `docs/IP_SECURITY.md:505-513`), (b) have a false-match probability on random bytes well below 2^-30 per flow or require cross-packet consistency, (c) not be producible by a stock BitTorrent client's handshake, and (d) be evaluated **after** the positive BitTorrent signatures so a BitTorrent flow cannot hide behind it (the Steam ordering, `ip_security_dmca.go:306-310`). Evasion by a *modified* client that prefixes a WireGuard header to BitTorrent is already in the accepted-evasion class ("forced encryption … behind a whitelisted-protocol disguise", `docs/IP_SECURITY.md:492-494`), exactly as a client could prefix a fake TLS ClientHello today.

Finally, the privileged-port skip (`ip_security_dmca.go:601-603`) is the largest existing "unknown encrypted" allowance — any protocol on 443/80/465/587/853/993/995 — and it skips the *positive BitTorrent signatures* too. Running the stateless signatures there (no flow state, no entropy) is a cheap tightening that this proposal pairs with its allowances so the net effect on provider protection is positive (§6.3).

---

## 5. Options considered

| # | Option | What it fixes | Provider risk | Complexity | Verdict |
|---|---|---|---|---|---|
| **A** | **Positive protocol detectors** for WireGuard, OpenVPN, RTMP, Levin, RakNet, Noise-IM framing (extend §4.4 of the spec with an `appStandardDetector`) | the spec-confirmed victims of the heuristic; keeps the traffic *inside* the tunnel (privacy preserved) | none beyond today's accepted disguise class; each detector individually reviewable and toggleable | low-medium; same shape as `ip_security_webstandard.go` | **Recommended (Phase 1)** |
| **B** | Bounded "unknown encrypted" allowance: per-sender distinct-destination quota (e.g. 4 per 10 min), optionally restricted to a curated list of registered ports (51820, 1194, 9993, 60000-61000, 64738, 3074, …) | long tail of protocols without fingerprints | **real**: K monitored peers = K notices; the curated-port variant is better (outbound BT goes to peer-chosen random ports) but still admits any random blob to those ports | medium (per-sender LRU sets in the detector) | **Not now**; keep the curated-port variant as a fallback if Phase 0 shows a long tail (§10) |
| **C** | **Client-side verdict correctness**: fail fast (RST / ICMP) on the first Drop; destination hint cache so retries route consistently; surface a reason on `BlockAction` | the mid-flow flip (§1.3); kill-switch stalls; user visibility | none (client only; providers unchanged) | medium | **Recommended (Phase 2)** |
| **D** | Provider opt-in tier ("accept unrecognized encrypted traffic"), advertised via the control plane; clients place such flows only on opted-in providers | everything in the long tail, for consenting providers | shifts liability to consenting providers rather than removing it; needs legal/product review; needs pre-classification on the client (only possible after a first failed attempt, i.e. requires C's hint cache); platform + protocol changes (`ProvideMode` is a contract relationship, not a capability — a new capability field is needed) | high | **Deferred**; open question |
| **E** | Blanket allow of unknown encrypted (`DropUnsanctionedEncrypted=false`) | everything | re-opens MSE/PE BitTorrent — forbidden by the fix rules | trivial | **Rejected** |
| **F** | **Telemetry + probe harness**: verdict-reason stats (port-keyed, local), committed packet fixtures for each app, re-test connect#128 on current code | the attribution gap; ongoing regression detection | none (no new data leaves the device; `includeIp=false` stays) | low | **Recommended (Phase 0)** |
| **G** | Tune the heuristic (raise `EncryptedDecisionPackets`, lower `MaxInspectionPayload`, tolerate one plaintext packet after two encrypted) | some protocols with a structured first packet | weakens MSE detection proportionally; the spec explicitly rejects this route | trivial | **Rejected** as a primary measure; one narrow tweak is folded into A (candidate-pending packets do not count as encrypted) |
| **H** | Provider-prefix × port exceptions per vendor (the Steam pattern) for Roblox (AS22697), Meta (AS32934) 5222, Xbox, PSN, Nintendo, Zoom | proprietary protocols that resist fingerprinting | low (vendor infrastructure) but a maintenance burden (prefix snapshots) and a weaker argument than a protocol fingerprint | low each | **Use sparingly**, only where Phase 0 captures show a high-entropy opener and the vendor publishes prefixes |

---

## 6. Recommended design

### 6.1 Phase 0 — measure before changing rules (connect, 1-2 days)

1. **Verdict-reason statistics.** Today `SecurityPolicyStatsCollector` records `(version, protocol, port) × {drop, allow, incident}` (`ip_security.go:969-1059`). Add a parallel bounded collector `DmcaVerdictStatsCollector` keyed by `(protocol, port) × reason` with reasons `cfaaDrop, bittorrent, dropEncrypted, allowPrivileged, allowGaming, allowWebStandard(kind), allowAppStandard(kind), allowRtp, allowHttp, allowPlaintext, allowBudget`. Same cardinality bound as `securityPolicyStatsMaxDestinationsPerResult`; port-only, never IPs. Expose via `SecurityPolicy.Stats()` → sdk `device_monitor.go` verbose print (`sdk/device_monitor.go:26-110`). No wire changes.
2. **Probe harness and fixtures.** Add `testdata/ipsecurity/<protocol>.json` holding the first ≤ 8 client payloads (≤ 512 bytes each, hex) plus transport/ports, captured from the real apps with IPs stripped, and a test helper `replayFixture(t, policy, fixture) SecurityPolicyResult` that drives `InspectEgress` exactly as `RemoteUserNatMultiClient.SendPacket` would (SYN first for TCP). Capture list for Phase 0: Roblox join + play, WhatsApp chat on 5222 (force by blocking 443), WhatsApp call, X Spaces speak, Session, SimpleX, Mihon, an IPTV HLS portal, OBS → RTMP, WireGuard, OpenVPN/UDP, Tailscale direct, Discord voice, Xbox/PSN party chat, Genshin, Zoom. The harness answers the "capture" rows of §3 and becomes the regression suite for Phase 1.
3. **Re-verify connect#128 on the current code** with the fixtures and record the answer per app in `100-blocked-apps-support/log.yml`.

### 6.2 Phase 1 — positive detectors (connect)

Add `ip_security_appstandard.go` with `AppStandardSettings` and `appStandardDetector`, mirroring `WebStandardSettings`/`webStandardDetector`, and wire it into `advance` between `web.match` and the RTP probation. Multi-packet confirmations keep small candidate state in `dmcaFlowState`, exactly as `rtpCandidates` does.

```go
// AppStandardSettings selects which non-web application protocols are
// positively recognized (and therefore allowed through the encrypted-traffic
// heuristic). Every detector is a clean-room protocol fact; see
// docs/IP_SECURITY.md §4.4.2.
type AppStandardSettings struct {
    Enabled   bool
    WireGuard bool // wireguard.com/protocol: initiation + transport framing
    OpenVpn   bool // OpenVPN reliability-layer opcodes + repeated session id
    Rtmp      bool // Adobe RTMP 1.0 §5.2 C0/C1 (version 3, zero word)
    Levin     bool // Monero Levin bucket head (signature, protocol_version 1)
    RakNet    bool // RakNet offline message id (OCR1/OCR2/unconnected ping)
    NoiseIm   bool // "WA"+version routing header with 3-byte length-prefixed Noise frames (confirm against capture before enabling)
}
```

Detector facts and acceptance criteria (each is "allow terminal" when satisfied; none is reached unless `detectBittorrentSignature` has already failed):

| Detector | Packet(s) | Rule | False-match on random bytes |
|---|---|---|---|
| WireGuard initiation → transport | UDP #1: `len == 148 && b[0] == 1 && b[1:4] == 0`. Then any later client datagram: `b[0] == 4 && b[1:4] == 0 && len >= 32 && len % 16 == 0`. | allow on the transport packet; the initiation alone marks a candidate. | 2^-32 for the header, × exact length; the follow-up is independent evidence |
| WireGuard mid-stream (flow re-inspected after idle eviction) | two client datagrams with `b[0] == 4`, reserved zeros, same 4-byte receiver index at `b[4:8]`, little-endian u64 counter at `b[8:16]` strictly increasing by < 2^20, both `len % 16 == 0`. | allow on the second. | < 2^-64 |
| OpenVPN/UDP | UDP #1: `b[0] in {0x38, 0x50}` (HARD_RESET_CLIENT_V2/V3, key id 0), `len >= 14`; store `sid = b[1:9]`. Next 1-2 client datagrams: opcode `b[0]>>3 in {4,5,7,10,11}` with `b[1:9] == sid` → allow. Mid-stream: two `P_DATA_V2` (`b[0]>>3 == 9`) with equal `b[1:4]` peer id and equal key id → allow. OpenVPN/TCP uses a 2-byte length prefix; apply the same after it. | allow on the confirming packet | 2^-64 (repeated session id) |
| RTMP | TCP first payload: `len >= 9 && b[0] == 0x03 && b[5:9] == 0`. (FP9 "digest" handshakes put a version in bytes 5-8 and are not matched; they fall through to the heuristic.) | allow | 2^-40 |
| Levin | TCP first payload: `len >= 33 && b[0:8] == 01 21 01 01 01 01 01 01 && le32(b[29:33]) == 1 && le64(b[8:16]) <= 100_000_000` | allow | 2^-96 |
| RakNet | UDP: `b[0] == 0x05 || 0x07` with the 16-byte offline magic `00 ff ff 00 fe fe fe fe fd fd fd fd 12 34 56 78` at `b[1:17]`; or `b[0] == 0x01` with the magic at `b[9:25]`. | allow | 2^-128 |
| Noise-IM (`WA`) | TCP: `b[0:2] == "WA"`, two version bytes, 3-byte big-endian frame length ≤ 64 KiB, frame starts with a protobuf field-1 length-delimited tag; confirmed on the second frame (also length-prefixed, length consistent). **Gate on the Phase 0 capture**; ship disabled if the capture disagrees. | allow on the second frame | ≈ 2^-40 |

Rules for `advance`:

1. Order: BitTorrent signatures → Steam → `web.match` → **`app.match` / `app.observe`** → RTP probation → HTTP → entropy. The BitTorrent precedence test pattern (`ip_security_gaming_test.go:211-260`) is reused for every new detector.
2. A packet that *opens* a multi-packet candidate (WireGuard initiation, OpenVPN reset, first `WA` frame) **does not increment `encryptedPackets`** but does increment `inspectedPackets`. The next packet either confirms (allow) or fails the candidate, in which case it is judged normally. Net effect on MSE detection: a BitTorrent flow whose first blob happens to be exactly 148 bytes starting `01 00 00 00` gets one extra packet of leak before the drop; probability ≈ 2^-32/513.
3. Candidates are bounded state: one `appCandidate{kind uint8; sid [8]byte; receiver uint32; counter uint64; packets uint8}` per flow (≈ 24 bytes), in the mutex-guarded block of `dmcaFlowState` (`ip_security_dmca.go:182-188`).
4. `AppStandardSettings` is nested under `DmcaSecurityPolicySettings.App` (like `Gaming`), defaulted in `DefaultDmcaSecurityPolicySettings` so the provider's memory-bounded rebuild (`ip_provider_memory.go:75-77`) and `SecurityPolicyHash` pick it up automatically.
5. `WireGuard`, `OpenVpn`, `Rtmp`, `Levin`, `RakNet` default **on**; `NoiseIm` defaults to the Phase 0 outcome.

Why these and not more: each has a fixed-format header or a cross-packet invariant, so no probabilistic judgement is involved; they cover the entire spec-confirmed victim list in §3 except Mosh/Mumble/ZeroTier/Ethereum/BIP324, which either have no plaintext structure at all (Mosh, ZeroTier, BIP324) or a fan-out profile (Ethereum) that the threat model cannot distinguish from BitTorrent at the flow level. Mumble falls back to its TLS tunnel on its own. Ethereum was later admitted per flow by cryptographic invariants rather than plaintext structure (§10.2); discovery v5 remains undetectable.

### 6.3 Phase 1 companion tightening — BitTorrent signatures on privileged ports

In `classifyForSender` (`ip_security_dmca.go:601-603`), replace the unconditional `return dmcaAllow` for `DestinationPort < 1024` with a stateless `if detectBittorrentSignature(ipPath, b) { return dmcaBittorrent }; return dmcaAllow` over the capped payload. No flow state, no entropy, no change for TLS (first byte `0x16`/`0x17` fails every signature's first comparison). Update `TestEgressSecurityPolicyDpi` (`ip_security_dmca_test.go:435-440`) to expect Incident, and `docs/IP_SECURITY.md` §4 so code and spec agree. This closes the "BitTorrent peer listening on 443" gap that the spec already claims is closed.

### 6.4 Phase 2 — client-side verdict correctness (connect; sdk/app strings)

In `RemoteUserNatMultiClient.SendPacket` (`ip_remote_multi_client.go:5945-6090`) and the fragment/group paths:

1. **Expose the transition.** Add `classifyForSenderDetailed` returning `(verdict, decidedNow bool)` so the policy can tell the first Drop on a flow from steady-state drops (`inspectEgressForSender` returns a `SecurityPolicyResult` only; add an in-package `inspectEgressDetailed` used by the multi-client, leaving the public interface unchanged).
2. **Fail fast on the first Drop.** TCP: deliver a synthesized RST to the app via `deliverTcpPolicyReset` (`ip_smtp_policy.go:847-864`, already used for SMTP rejects at `ip_remote_multi_client.go:5998-6007`). UDP: deliver an ICMP destination-unreachable/port-unreachable (v4 type 3 code 3; v6 type 1 code 4) through the same receive callback. `ip_icmp.go` only builds echo replies today (`echoReplyPacket`, `:874`), so this needs a small new builder, `icmpUnreachableForPolicyReject`, written like `tcpRstForPolicyReject`. This applies whether the kill switch is on or off: with it off, continuing the flow locally is pointless for TCP (dead connection) and misleading for UDP.
3. **Policy-local hint cache.** On that first Drop, record `(dstIp, dstPort, protocol)` in a bounded TTL cache (`policyLocalHints`, default 10 min, 1024 entries, approximate-LRU like `blockActionCache`, `ip_remote_multi_client.go:2468`). On a later *new* flow to a hinted destination: if `localSecurityBypass` → route local from the first packet (the retry works, outside the tunnel, as the kill-switch-off contract already promises); if the kill switch is on → block and RST/ICMP immediately (no stall). Never create a hint from `Incident` (BitTorrent) — those stay blocked in both modes, as `blockActionApply` already enforces (`ip_block_action.go:487-490`).
4. **Reason on `BlockAction`.** Add `Reason` (`security-encrypted`, `security-bittorrent`, `security-port`, `security-ip`, `blocker`, `override`) to `BlockAction` (`ip_block_action.go:73-96`) and the collector so the apps' split-rules screens (`android/.../SplitRulesScreen.kt:539`, `apple/.../SplitRulesView.swift`) can label "blocked by URnetwork safety rules" and offer the existing host/app route-local override. Follow-up in sdk/android/apple; the Android kill-switch exception text (`strings.xml:454`) should add that unrecognized encrypted protocols bypass the VPN when the kill switch is off.

Nothing in Phase 2 changes what a provider accepts or sends anywhere new: the RST/ICMP go to the local app; the hint cache is local; `BlockAction` already flows only to the local UI.

### 6.5 Rejected for now: fan-out quota and provider opt-in tier (not implemented)

See §5 B and D and §10. If Phase 0 shows a significant long tail that §6.2 does not cover, the curated-port variant of B is the fallback: allow `dmcaDropEncrypted`-classified flows only to a short list of IANA-registered ports owned by single-server protocols (Mosh 60000-61000, ZeroTier 9993, Mumble 64738) **and** only while the sender's distinct destinations on that port in the last hour are ≤ 2, implemented as a per-sender LRU set inside `dmcaDetector`. It must ship behind a default-off setting and after the stats from Phase 0 show what it would admit.

---

## 7. Concrete code changes

### 7.1 connect — Phase 0

| File | Change |
|---|---|
| `ip_security.go` | `SecurityPolicyStatsCollector` gains `addReason(protocol, port, reason)` and a `Reasons(reset)` snapshot with the same cardinality bound; `securityPolicy` records the reason at each return in `inspectEgressForSender` (`:474-510`) and the group path (`:433-456`). `SecurityPolicy` interface unchanged (reasons reachable through `Stats()`). |
| `ip_security_dmca.go` | `advance` returns `(dmcaVerdict, dmcaReason)` internally; `classifyForSender` propagates. `dmcaReason` enumerates the rows in §6.1 (1). |
| `ip_security_fixture_test.go` (new) | `loadSecurityFixture`, `replayFixture`; table test over `testdata/ipsecurity/*.json` asserting the expected verdict per fixture (initially: the current verdict, documenting what the code does; Phase 1 flips the expectations). |
| `testdata/ipsecurity/*.json` (new) | first-payload fixtures per app/protocol; IPs replaced by documentation addresses; a `README` stating capture provenance and that only protocol-fact bytes are retained. |
| `sdk/device_monitor.go` | print the reason table under verbose alongside `printSecurityPolicyStats` (`:110`). |

### 7.2 connect — Phase 1

| File | Change |
|---|---|
| `ip_security_appstandard.go` (new) | `AppStandardSettings`, `DefaultAppStandardSettings`, `appStandardDetector{settings}`, stateless `match(ipPath, payload) (appKind, bool)` for RTMP / Levin / RakNet; `open(ipPath, payload) (appCandidate, bool)` and `confirm(candidate *appCandidate, ipPath, payload) bool` for WireGuard / OpenVPN / Noise-IM. Constants: `wireGuardMessageInitiation = 1`, `wireGuardMessageTransport = 4`, `wireGuardInitiationLength = 148`, `openVpnOpcodeHardResetClientV2 = 7`, `...V3 = 10`, `rtmpVersion = 3`, `levinSignature`, `rakNetOfflineMagic`. |
| `ip_security_dmca.go` | `DmcaSecurityPolicySettings.App *AppStandardSettings` (default `DefaultAppStandardSettings()`); `dmcaFlowState.app appCandidate`; `advance` inserts `app.match` / `open` / `confirm` after `web.match` (`:312-317`) with the "candidate-opening packet does not count as encrypted" rule; `newDmcaDetector` takes the app detector (constructor signature change is in-package; `NewSecurityPolicy` builds it from `dmcaSettings.App`). Privileged-port branch (`:601-603`) runs `detectBittorrentSignature` stateless (§6.3). |
| `ip_security.go` | no interface change; `NewSecurityPolicy` passes `newAppStandardDetector(dmcaSettings.App)`. |
| `ip_provider_diagnostics.go` | nothing to do: the settings JSON already feeds `SecurityPolicyHash` (`:44-60`). |
| `docs/IP_SECURITY.md` | §3.1 row for 443 corrected; new §4.4.2 "Application standards" table; §4.8 "Accepted FP surface" rewritten to list what remains dropped by design (MSE/PE, encrypted uTP, Mosh/ZeroTier/BIP324/RLPx, obfs4); §6 limitations updated. |

### 7.3 connect — Phase 2

| File | Change |
|---|---|
| `ip_security.go` | in-package `inspectEgressDetailed(...) (SecurityPolicyResult, securityDecision)` where `securityDecision{reason; decidedNow bool}`; `reverseSecurityPolicy` passes it through for completeness. |
| `ip_remote_multi_client.go` | `policyLocalHints *policyHintCache` on the multi-client (built next to `blockActionCache`, `:2468`); in `SendPacket` consult the hint before `InspectEgress` for new flows; on `decidedNow && result == Drop` call `deliverTcpPolicyReset` / `deliverIcmpPolicyUnreachable` and `policyLocalHints.add(dst)`; same in `sendReassembledUdpFragments` (`:6100-6160`) and the group path (`:6397-6420`). |
| `ip_icmp.go` | `icmpUnreachableForPolicyReject(packet []byte) []byte` (v4 type 3/code 3, v6 type 1/code 4) mirroring `tcpRstForPolicyReject`. |
| `ip_block_action.go` | `BlockAction.Reason`, `blockActionCollector.add(..., reason)`; `blockActionApply` unchanged. |
| `sdk/device.go`, `device_local.go` | surface `Reason` on the exported `BlockAction`; `PacketStats` gains `BlockEgressSecurity*` vs `BlockEgressOverride*` split if the apps want it. |
| android / apple | label security blocks in the split-rules screens; extend the kill-switch exception text. |

### 7.4 Settings summary (new defaults)

| Setting | Default | Notes |
|---|---|---|
| `Dmca.App.Enabled` | true | master switch for application standards |
| `Dmca.App.WireGuard`, `.OpenVpn`, `.Rtmp`, `.Levin`, `.RakNet` | true | protocol-fact detectors |
| `Dmca.App.NoiseIm` | Phase 0 outcome | ship false if the capture does not confirm the framing |
| `Dmca.App.EthereumDiscv4`, `.EthereumRlpx` | true | §10.2, `fix/ipsecurity-ethereum` |
| `Dmca.InspectPrivilegedSignatures` | true | §6.3; false restores today's skip (for A/B only) |
| multi-client `PolicyHintTtl` / `PolicyHintMaxCount` | 10 min / 1024 | Phase 2 |

---

## 8. Test plan

All tests live beside the existing ones and follow their shape (`dmcaPath`, `encryptedPayload`, `steamTestPath`; `newDmcaDetector(nil, settings, web)` for pure state-machine tests; `DefaultSecurityPolicy(ctx)` for end-to-end). Every "pass after" fixture first asserts `payloadLooksEncrypted(fixture, settings)` so the test proves it exercises the heuristic (the Steam test's "fixture must exercise the encrypted heuristic" pattern, `ip_security_gaming_test.go:216-219`).

### 8.1 Fail before / pass after (Phase 1)

| Test | Fixture | Before | After |
|---|---|---|---|
| `TestDmcaWireGuardHandshakeThenTransportAllowed` | 148-byte initiation (`01 00 00 00`, random sender, 128 random bytes, 16 zero mac2) then a 96-byte type-4 transport datagram | third datagram → `dmcaDropEncrypted` | second datagram → `dmcaAllow`; steady state lock-free allow |
| `TestDmcaWireGuardMidStreamCountersAllowed` | two type-4 datagrams, same receiver, LE counters 17 then 18, lengths 112 and 144 | drop after third | allow on second |
| `TestDmcaWireGuardNearMissesDrop` | (a) initiation with `b[1] != 0`; (b) length 147; (c) transport with `len % 16 != 0`; (d) two transports with decreasing counters; (e) initiation followed by a 96-byte random blob | drop | **still drop** |
| `TestDmcaOpenVpnResetThenControlAllowed` | `0x38` + sid + 40 random, then `0x20`/`0x28` + same sid + random | drop | allow |
| `TestDmcaOpenVpnSessionIdMismatchDrops` | second packet with a different sid | drop | still drop |
| `TestDmcaRtmpHandshakeAllowed` | TCP SYN, then `03 <4 time> 00 00 00 00` + 1451 random, then 77 random, then 1536 random (C2) | drop at C2 | allow at C0/C1 |
| `TestDmcaRtmpDigestVariantNotMatched` | bytes 5-8 = `80 00 07 02` | drop | still drop (documented) |
| `TestDmcaLevinHandshakeAllowed` | Levin head + a dense portable-storage body crafted to pass the entropy gate | drop (if crafted dense) / allow by plaintext | allow via Levin, verdict reason `allowAppStandard(levin)` |
| `TestDmcaRakNetOpenConnectionAllowed` | `05` + magic + `06` + 1385 zero bytes; and a variant with random padding | allow (plaintext) / drop | allow via RakNet in both |
| `TestDmcaNoiseImFramesAllowed` (gated) | `WA` header + two length-prefixed Noise frames from the Phase 0 capture | drop | allow |
| `TestEgressSecurityPolicyAppStandards` | end-to-end through `DefaultSecurityPolicy` for each of the above on ports 51820, 1194, 1935, 18080, 49152, 5222 | mixed | allow |
| `TestProviderReversePolicyAdmitsAppStandards` | same flows through `DefaultProviderSecurityPolicy` with a sender id (pattern of `TestProviderReversePolicyCarriesSenderClientId`, `ip_security_dmca_test.go:231`) | drop | allow |

### 8.2 Negative tests — abuse traffic stays dropped (Phase 1)

| Test | Fixture | Expected |
|---|---|---|
| `TestDmcaBittorrentPrecedenceOverAppStandards` | each app-standard opener followed by / combined with `btHandshake()` on the same flow; a Levin-signature-prefixed packet whose remainder is a BEP 3 handshake; a RakNet-magic datagram carrying `d1:ad2:id20:` | `dmcaBittorrent` → Incident |
| `TestDmcaMseHandshakeStillDropped` | 96-byte random + random padding of lengths {0, 52, 148, 300, 512} (so one variant is exactly 148 bytes), three packets | `dmcaDropEncrypted` for every variant (the 148-byte one must not start `01 00 00 00`; a separate sub-test with that prefix asserts the drop still happens on the *fourth* packet — the one-packet leak is the accepted cost) |
| `TestDmcaEncryptedUtpStillDropped` | uTP ST_SYN then random ST_DATA ×3 | drop |
| `TestDmcaPrivilegedPortBittorrentIsIncident` | `btHandshake()` to TCP 443 and 80; DHT ping to UDP 443 | Incident (flips the existing `:435-440` assertion) |
| `TestDmcaPrivilegedPortTlsUnaffected` | `tlsClientHello()` to 443 then random app data ×8 | allow, no flow state created (`Testing_FlowCount() == 0`) |
| `TestDmcaAppStandardsDisabledRestoreDrop` | each positive fixture with `App.Enabled=false` and with the specific toggle off | drop (toggle behavior, like `TestDmcaStateMachineRtpToggleAndTransport`) |
| `TestDmcaAppCandidateDoesNotStallBudget` | 8 consecutive fake WireGuard initiations (random with `01 00 00 00`, 148 bytes) | terminal at `InspectionPacketBudget`, result allow-by-budget as today — and a companion assertion that `encryptedPackets` was not incremented by candidate openers, documenting the bounded leak |
| `FuzzAppStandardDetectors` | seeds: each positive fixture, each near miss | never panics; `match` never returns true for the near-miss seeds; no allocation (`testing.AllocsPerRun == 0`, as `TestSteamValveEndpointZeroAlloc`) |

### 8.3 Phase 0 and Phase 2 tests

- `TestSecurityPolicyReasonStatsBounded` — reason table respects the cardinality bound (pattern of `TestSecurityPolicyStatsCollectorBoundsDestinationCardinality`).
- `TestFixtureReplayMatchesExpectedVerdicts` — every `testdata/ipsecurity/*.json` carries `expect_before`/`expect_after`; the test runs both policies (toggle) and fails on drift.
- `TestMultiClientFirstDropResetsTcpAndHintsDestination` — a multi-client with a fake provider path: SYN + two encrypted segments go remote, the third produces a RST to the receive callback and no further remote send; a new SYN to the same destination routes local (bypass on) or is RST'd immediately (bypass off); an Incident flow never creates a hint.
- `TestMultiClientFirstDropSendsIcmpForUdp` — same for UDP with ICMP port-unreachable.
- `TestPolicyHintCacheExpiresAndBounds` — TTL and max-count.
- `TestBlockActionCarriesReason` — collector surfaces `security-encrypted` vs `blocker`.

### 8.4 Verification commands (read-only in the tree, run from a worktree)

```
go test -run 'Cfaa|Dmca|WebStandard|AppStandard|Security|Fixture|PolicyHint' ./
go test -run 'TestRemoteUserNatProviderEnforcesReversedPolicy|TestMultiClientFirstDrop' -race ./
go test -fuzz FuzzAppStandardDetectors -fuzztime 60s ./
```

---

## 9. Rollout and metrics

1. **Phase 0 ships first** (stats + fixtures). No behavior change. Metric: the reason table under verbose logging on a developer provider and on the probe devices; the per-app verdict matrix recorded in the inbox logs.
2. **Phase 1 + 6.3 ship together** in one connect release, so the hash reported in `IpProviderDiagnostics.security_policy_hash` changes exactly once and identifies "has application standards" providers. The client already receives the hash and build version per exit (`ip_remote_multi_client.go:8678-8685`, `sdk/reliability_controls.go:436-451`). Detectors only add allows and the tightening only adds Incidents, so there is no `LogOnly` canary period; the risk to watch is a regression in BitTorrent detection, measured as the `bittorrent` incident count not falling on the probe provider while the `dropEncrypted` count on 51820/1194/1935/18080/49152-65535 falls.
3. **Version skew.** Until providers update, a client with Phase 1 will still have its WireGuard/RTMP flows dropped by old providers (silently), and by pre-June-2026 providers for everything ≥ 11000. Two mitigations, both optional: (a) the multi-client prefers exits whose `ProviderBuildVersion` is at or above the Phase 1 release when a flow matches a Phase 1 detector on the *client* side (it has the verdict before the first remote send only for single-packet detectors — RTMP, Levin, RakNet; for WireGuard/OpenVPN the second packet decides, which is still before most data); (b) the hint cache in Phase 2 plus the provider's block counters let the client retry a destination on a different exit when `ProviderBlockIngressPacketCount` rises right after it opened a flow. Neither is required for correctness.
4. **Phase 2** ships after Phase 1 is on most providers, so that fail-fast does not turn "dropped by an old provider" into a confusing RST storm; metric: inbox reports of stalls, `BlockAction` reason counts in the apps.
5. **Spec.** `docs/IP_SECURITY.md` is updated in the Phase 1 commit (it is the authority; code/spec drift is a bug by its own rule, `docs/IP_SECURITY.md:8-11`).
6. **Inbox.** Re-triage the 20 `site-or-app-blocked` items against the Phase 0 matrix; most should move from "DPI drop" to "port rule, fixed by `ac91c55c`; needs the provider to update" or to "IP reputation / exit rotation", and only the RTMP/WireGuard-class ones to Phase 1.

---

## 10. Open questions (not implemented; decisions pending)

1. **Roblox, WhatsApp 5222, X Spaces, Xbox/PSN/Switch/Genshin/Zoom**: which of these actually trip the heuristic on current code? Only Phase 0 captures can answer; the §3 table records the hypotheses. If Roblox's RakNet fork drops the zero padding, the RakNet detector covers it only if the magic survives; otherwise the Steam pattern (Roblox AS22697 prefixes × UDP 49152-65535) is the fallback (§5 H).
2. **Ethereum and other many-peer encrypted protocols** — **resolved** (decision: "if we can allow Ethereum let's do it, but not at the expense of allowing BitTorrent"; no opt-in tier, no quota, no change to the default rules beyond protocol-specific detectors). Ethereum has no plaintext header, but every discv4 packet and every RLPx auth carries a *cryptographic* invariant a stock BitTorrent client cannot produce, so each flow proves itself individually and the fan-out profile no longer matters. Implemented on `fix/ipsecurity-ethereum` (`ip_security_appstandard_ethereum.go`, spec `docs/IP_SECURITY.md` §4.4.2):
   - **discovery v4 (UDP)**: `hash ‖ signature ‖ type ‖ data` with `hash = keccak256(signature ‖ type ‖ data)` over the complete packet (≤ 1280 bytes, type 1-6, data a fitting canonical RLP list). False match ≤ 2^-256 for random bytes and for every BitTorrent UDP variant (µTP, DHT, UDP tracker, MSE over µTP), whose fixed leading bytes would have to equal a keccak output. `App.EthereumDiscv4`, default on.
   - **RLPx (TCP, first payload)**: EIP-8 auth with `auth-size == len − 2` (≥ 282), byte 2 `0x04` and an on-curve secp256k1 point (≈ 2^-279 for random/MSE); or the 307-byte pre-EIP-8 auth starting with an on-curve point (≈ 2^-263, reachable only by MSE `PadA = 211`). Plain peer wire can never match (`13 42` declares 4,930 bytes, byte 2 is `i`), and the 148-byte and every MSE opener shorter than 284 bytes cannot match at all. `App.EthereumRlpx`, default on.
   - Precedence is unchanged: the BitTorrent signatures run first and keep running on an admitted flow for the rest of its budget, and the bytes after the recognized header (after the RLP list; after the ephemeral key) are checked for every signature. Forging the invariants requires a modified client — the accepted disguise class.
   - **Not admitted**: discovery v5, whose header is AES-CTR masked with the destination node id that the exit never sees (indistinguishable from random); an RLPx auth split across TCP segments. The responder's ack/pong is ingress and not inspected, so no reverse-direction confirmation is used; none is needed at these probabilities.
3. **Nested VPN policy.** Admitting WireGuard/OpenVPN is argued low-risk because abuse inside exits from the VPN server's IP. Product should confirm that "URnetwork as the outer hop of a double VPN" is a supported use; if not, `App.WireGuard`/`App.OpenVpn` default off and the heuristic keeps dropping them (and, with the kill switch off, leaking them locally as today).
4. **Provider opt-in tier (option D).** Whether to offer providers a "relay unrecognized encrypted traffic" setting at all is a legal/product decision (it shifts DMCA exposure to volunteers). The mechanics would need a provider capability advertised through the control plane (not `ProvideMode`, which is a contract relationship) and client-side placement that is only possible after a first attempt fails (Phase 2's hint cache). Not proposed here.
5. **Exit rotation for session-bound apps** (SimpleX, Mihon per item `2512`): the `Pin` route override exists (`ip_block_action.go:35-55`) but is manual. A default app-affinity policy for messengers is a separate design.
6. **Kill-switch-off semantics.** `DefaultRouteLocal: true` means policy-dropped traffic leaves via the user's own IP by default. Phase 2 makes that reliable; it does not change the default. Should the apps warn more prominently that "unrecognized encrypted protocols bypass the VPN", or should the default flip to kill switch on now that Phase 1 admits the common legitimate protocols?
7. **`IpProviderDiagnostics` reason split.** Adding "encrypted vs bittorrent vs port" block counters to the provider → client message would help clients retry smartly. It is a new field on an existing per-source message to the traffic's own sender (no third party) — acceptable under the data-flow rule, but not needed for Phases 0-2; decide with Phase 2.
8. **Privileged-port skip beyond signatures.** §6.3 adds the stateless BitTorrent signatures on < 1024. Should the entropy heuristic also run on 443 for *non-TLS* openers (MTProto, Noise on 443)? No — that would break the apps that work today; the 443 allowance is the deliberate "TLS-port trust" and should be documented as such.

---

## Appendix A — Protocol fact sources (clean-room)

- WireGuard: "WireGuard: Next Generation Kernel Network Tunnel" (whitepaper) §5.4.2 (Handshake Initiation, 148 bytes), §5.4.3 (Response, 92 bytes), §5.4.6 (Transport Data, 16-byte header, 16-byte-padded plaintext, little-endian counter); wireguard.com/protocol.
- OpenVPN: OpenVPN "Protocol" documentation (`ssl_pkt.h` opcodes: P_CONTROL_HARD_RESET_CLIENT_V2 = 7, V3 = 10, P_CONTROL_V1 = 4, P_ACK_V1 = 5, P_DATA_V2 = 9; opcode in the high 5 bits, key id in the low 3; 8-byte session id; 2-byte length prefix over TCP).
- RTMP: Adobe "Real-Time Messaging Protocol (RTMP) specification 1.0" §5.2.1-5.2.2 (C0 version 3; C1 time, zero, random 1528).
- Levin: Monero `contrib/epee/include/net/levin_base.h` (`LEVIN_SIGNATURE 0x0101010101012101`, `LEVIN_PROTOCOL_VER_1 = 1`, `LEVIN_DEFAULT_MAX_PACKET_SIZE 100000000`), `storages/portable_storage_base.h` (storage signature).
- RakNet: RakNet `MessageIdentifiers.h` (`ID_UNCONNECTED_PING = 1`, `ID_OPEN_CONNECTION_REQUEST_1 = 5`, `ID_OPEN_CONNECTION_REQUEST_2 = 7`), `RakPeer.cpp` (`OFFLINE_MESSAGE_DATA_ID`).
- WhatsApp: the Noise-based handshake and `WA`+version routing header are documented by open-source client libraries; confirm bytes against the Phase 0 capture before enabling.
- BitTorrent MSE/PE ("Message Stream Encryption" spec, Vuze wiki): `Ya` 96 bytes + `PadA` 0-512 random bytes.
- Ethereum: devp2p `discv4.md` ("Wire Protocol": hash, 65-byte signature, packet types 0x01-0x06, 1280-byte maximum, RLP packet data), `discv5-wire.md` (masked header: AES-CTR keyed by the first 16 bytes of the destination node id), `rlpx.md` ("Initial Handshake", "ECIES Encryption": `R ‖ iv ‖ c ‖ d`), EIP-8 (auth-size prefix, `auth-vsn`, padding); the Ethereum yellow paper appendix B (RLP); SEC 2 (secp256k1, `y² = x³ + 7`).

## Appendix B — Inbox items referenced

`100-blocked-apps-support` (connect#128), `265-welcome`, `2491-iptv-services-blocked`, `2511-simplex-mihon-issues`, `2512-longer-node-connections-simplex`, `2533-monero-wallet-sync-fails`, `7309-roblox-error-with-vpn`, `7787-whatsapp-broken-on-ios`, `2798-discord-calls-windows-app`, `3042-games-not-working`, `4232-4251-disconnects-while-gaming`, `6937-live-stream-keeps-stopping`, `7630-some-apps-fail-vpn`, `7553-blockchain-access-lost`, `1101-sugestao-de-melhoria`, `57-samsung-update-routing`, `2781-split-tunneling-rcs-broken`, `156-getting-security-detections-router`, `248-urgent-issue-safety-precautions`, `1863-exclude-comcast-torrent-traffic`, `364-questions`, `78-block-dht-nodes`, `88-bittorrent-detection-blocker`, `82-youtube-bot-check`, `2290-youtube-bot-verification`, `7313-youtube-ip-block` (the last three fail the honesty gate and are out of scope).

## Appendix C — Commits referenced

`1c3ac5a8` (2025-05-14, port table + `10000 <=` drop), `c246c749` (2025-06-05, `11000 <=`), `ac91c55c` (2026-06-27, DPI replaces port rule), `63dd3d4e` (2026-08-15, sender-scoped flows), `6a7d45ae` (2026-09-01, TURN/RTP/RTCP/Steam/Telegram), `d65a05e0` (2026-09-04, provenance), `56232afd` (2026-09-17, memory budgets), `d4195dac` (2025-07-03, provider CLI help text — unrelated).
