# Security policy fixtures

Each `*.json` file is the opening of one client flow: the transport, the
destination port, and up to eight client payloads (hex), plus the verdict the
egress security policy must reach on the last payload.

- `expect_before`: the verdict with application standards and the
  privileged-port BitTorrent signatures disabled, i.e. the policy as it was
  before IPSECURITY-UPDATE4.
- `expect_after`: the verdict with the default policy.

`ip_security_fixture_test.go` replays every fixture through `InspectEgress`
the way `RemoteUserNatMultiClient.SendPacket` does (a real IP packet per
payload, parsed with `ParseIpPathWithPayload`, a SYN first for TCP).

## Provenance

These fixtures are **synthesized from public protocol specifications, not
captured from real apps**. Each fixture's `provenance` field names the facts it
encodes (WireGuard whitepaper, OpenVPN protocol, Adobe RTMP 1.0, Monero
`levin_base.h`, RakNet `MessageIdentifiers.h`, devp2p `discv4.md`/`rlpx.md`
and EIP-8, the BitTorrent BEPs, RFC 8446).
Fields that are random on the wire (keys, ciphertext, padding, ids) are a
SHA-256 counter stream keyed by the fixture name, so the files are
reproducible. The Ethereum fixtures are complete protocol instances built from
that stream: the discv4 packets carry real keccak256 hashes and deterministic
(RFC 6979) secp256k1 signatures from a stream-derived key, and the RLPx auth is
genuinely ECIES-encrypted to a stream-derived recipient key. Endpoint fields in
the discv4 packets are RFC 5737 documentation addresses:

    go run testdata/ipsecurity/generate.go

No address, hostname, key, or identifier from a real device is retained. Packets
are addressed with RFC 5737 documentation addresses by the harness.

Captures from the real apps (Roblox, WhatsApp on 5222, X Spaces, Xbox/PSN,
Genshin, Zoom) are still needed to settle the protocols the specifications
cannot (see IPSECURITY-UPDATE4.md section 10). A capture added here must be
reduced to protocol-fact bytes before it is committed.
