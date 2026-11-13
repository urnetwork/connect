# Extender hardening proposal (2026-11-01)

Status: proposal, not as built. This extends `EXTENDER.md`; it does not revise
it. Section tags in parentheses (A5, B3, P3, R, §6, …) refer to that document.
Written in the context of the recURd product work (the agent record and reach
layer that rides the network); the directions in Part 2 are motivated by the
outside-in reputation findings in
`../sidecar/research_notes/Sidecar pitch4 differentiation directions/L2-2.12-*.md`.

## Motivation: two adversaries, two jobs

The extender serves two unrelated threat models, and the mistake to avoid is
answering one with the other's tool.

- **Accessibility (Part 1).** A user is behind a network that wants to stop
  them reaching the outside at all — a national firewall, a captive ISP. Here
  the network operator is the adversary, and the extender's job is to be hard
  to classify, hard to enumerate, and hard to probe. Camouflage belongs here.

- **Enterprise reach (Part 2).** A team has bought recURd and *wants* its
  agents' traffic to flow. Here the customer's security team is the buyer, not
  the adversary. The thing blocking the traffic is a third-party reputation
  feed (Cisco Talos files every `bringyour.com` host as "Personal VPN";
  DigiCert/Vercara UltraDNS blocks it; one extender address is already on
  FireHOL's open-proxy list; HaGeZi lists all four apex domains). The fix here
  is the **opposite** of camouflage: be cleanly identifiable and allow-listable,
  so the buyer's own controls can knowingly permit the product they paid for.
  Camouflage used here would make the reputation worse (traffic that behaves
  like evasion gets tagged `anonymizer`/`proxy`, and that same signal is what
  Okta/Entra flag on sign-in) and would break recURd's promise that every way
  out is "private, approved, and on a record only your team can read."

§6 already states the Part 1 gaps honestly: camouflage is a classifier defense,
not an address defense; the directory stays enumerable; camouflage is tcp only
(P9); TLS-in-TLS detection applies to every carrier. Part 1 attacks those. Part
2 is new work that the EXTENDER.md threat model never covered, because it is
about reputation, not censorship.

---

## Part 1. Accessibility: strengthen against a hostile network

### 1. Address-harvesting resistance (the §6 core gap)

The deepest weakness is not classification but enumeration: a censor who
harvests the directory and blocks by address does not care how good the
camouflage is. China blocked hundreds of REALITY addresses in March 2026 by the
SNI-to-IP mismatch, not by fingerprint. The tiered directory (R: sacrificial
open tier, gated Lox/Salmon tier, canaries) is the existing mitigation. Push it
further:

1.1 **Make the gated tier (R3) the default reach path**, not an upgrade. The
open tier shrinks to a bootstrap-of-last-resort that hands out only
short-lived, high-churn addresses that are expected to burn. A client that can
reach the gated release route should never be dealt an open-tier address.

1.2 **Reputation-aware placement.** Extend R4's per-country blocked-state so the
release policy never deals an address a canary or enough distinct reporters have
shown blocked in the requester's country, and bias toward addresses whose
observed block age in that country is lowest. The signal already exists; this
spends it at deal time.

1.3 **Decouple the serving address from any published record.** Promote the
WebRTC carrier (S) from a NAT fallback for home extenders to a first-class
rendezvous for every tier: the client learns an extender's *identity* from the
directory and reaches its *address* through a short-lived signaled rendezvous,
so the long-lived serving address is never in an enumerable record. This is the
single highest-value change for a harvesting adversary.

1.4 **Fronting where a shared range exists.** The SNI-to-IP mismatch is only
exploitable because the borrowed name is not hosted at the extender's address.
Offer an optional carrier that reaches the extender *through* a large shared
range (a cooperating CDN or a refraction/decoy-routing deployment), so the SNI
and the address finally agree and address-harvesting stops working. This is
heavier and partner-dependent; scope it as a distinct carrier, not a change to
the direct ones.

1.5 **Ephemeral addresses.** On cloud-hosted extenders, rotate the public
address on a cadence shorter than a censor's harvest-to-block latency, and let
the record's short TTL and the drip (C4) carry the rotation. A harvested
address should age out before it is worth blocking.

### 2. Close P9: camouflage parity across carriers

Camouflage is tcp only. QUIC on udp 443 and the dns carrier (Q) stay
identifiable on the same address.

2.1 **QUIC authenticated first flight.** Bind a REALITY-equivalent auth tag into
the TLS ClientHello carried in the QUIC Initial, mirroring P1's construction on
the udp 443 carrier. The `fingerprint` package already targets the QUIC Initial
as a drift-conformance subject, so the conformance machinery is in place; the
work is the auth binding and an unauthenticated-probe response that looks like a
real h3 origin (the QUIC analogue of the P3 splice).

2.2 **DNS carrier posture.** The udp 53 carrier talks to the extender IP
directly and cannot be made to look like anything but a resolver speaking to a
non-resolver. Keep it strictly as a last-resort bootstrap, never a steady-state
reach path, and prefer a DoH/DoT-shaped carrier to a well-known resolver front
where one is reachable.

### 3. Active-probing and TLS-in-TLS hardening

3.1 **Make the splice the default once borrowed lists exist.** P3 already defeats
a naive active prober on tcp by splicing it to the real borrowed site; it is off
by default (P7) only until an operator accepts the posture and a measured
borrowed-names list (P5) exists. Section 5 closes that gap.

3.2 **Inner-stream shaping against TLS-in-TLS detection.** This is the one §6
admits applies to every carrier, camouflaged or not. The published detection
heuristics key on record-size and timing structure of the inner TLS leaking
through the outer. Add inner-stream padding and segmentation so inner record
boundaries do not map to outer segments, plus bounded burst/timing jitter.
Budget this against latency; make it a per-strategy setting, strongest on the
carriers used under active censorship.

3.3 **Fingerprint conformance on the carrier hello and the QUIC Initial.** Extend
the drift-conformance harness (the `fingerprint` package, which already states
the carrier hello and QUIC Initial as intended subjects) so a silent drift from
real Chrome fails a test rather than a censor's filter. This is maintenance the
harness was built for; keep it funded as browsers move.

### 4. Bootstrap resilience

§6: a client that never dials an unverified address has no extender at all if
`extender.<host>` carries no TXT — dns is the only bootstrap, and the feed is
reached through extenders. That single chokepoint is fragile under an adversary
who blocks the bootstrap resolver.

4.1 **Signed out-of-band bootstrap.** The share payload (K7) already carries
addresses and custom DoH servers in a signed `ur-ext:1:` blob. Generalize it
into an out-of-band bootstrap channel: a small, signed, rotating seed set
reachable by more than DNS — a fronted well-known URL, embedded fallback seeds,
or a DHT — so a blocked resolver is not a total loss. Addresses stay unverified
bootstrap entries that upgrade the moment a root-signed record names them (the
K7 trust model is unchanged).

4.2 **Multiple DoH providers by default**, with per-country selection, since the
default DoH set is itself a block target.

### 5. Borrowed-names measurement pipeline

P5's list is unbundled; camouflage degrades to the legacy terminate path
without it, so the strongest tcp posture is currently unreachable in practice.
Stand up the measurement pipeline P5 describes: per-country candidates that are
reachable, speak TLS 1.3, offer X25519/X25519MLKEM768, and are plausibly hosted
on arbitrary addresses (not CDN-pinned), refreshed on a cadence, shipped as the
`res/extender_borrow[_<cc>].bin` resource. Without this, sections 2.1 and 3.1
have nothing to splice to.

---

## Part 2. Enterprise reach: be allow-listable, not invisible

This part is for recURd deployments inside a company that wants the traffic to
flow. The goal is a clean, transparent, permit-able identity — the reputation
problem solved by hygiene, not disguise.

### 6. A distinct identity off the consumer pool

6.1 **Dedicated infrastructure under `recurd.dev`.** The enterprise relay runs
on its own names (`relay.recurd.dev`, `control.recurd.dev`, a separate marketing
site), never on `bringyour.com`, `ur.io`, `ur.network`, `ur.xyz`, the api /
connect / mcp / extender hosts, or the shared Hurricane Electric pool that DNS
ties to 37 consumer hostnames. Shared names and IPs are exactly why the
"Personal VPN" category and the blocklist hits transfer; a dedicated identity
stops the transfer.

6.2 **Dedicated IP ranges** on a reputable network, not the cheap VPS ranges
(Hostodo, ColoCrossing, Octopus, CentrixNodes) the public extenders borrow and
that proxy feeds already carry.

6.3 **A published, machine-readable allowlist** at a well-known `recurd.dev` URL:
domains, SNI names, IP CIDRs, ports. This is what a security team allow-lists,
and what a preflight (section 9) checks against.

6.4 **Every connection on the record names its relay.** recURd's signed receipt
for each connection carries the relay hostname, IP and operator, so a customer
can match actual traffic to the published allowlist and prove to themselves that
no consumer host, extender or provider exit was ever in the path. This is both a
trust artifact and a straight extension of the "admins answer" record — the
transparency is the product, not a concession.

### 7. Publicly-trusted certificate on the enterprise edge

The B3 private "Extender Root" CA (`O=Extender`, `CN=Extender Root`) is correct
for the camouflage carrier facing a censor — a client that holds the identity
key verifies the leaf under it, no roots consulted. It is wrong for an
enterprise edge: a TLS-inspecting proxy sees a private CA and flags it, and it
was a cited reason the Public Suffix List rejected `extender.bringyour.com`.

7.1 **The customer-facing recURd relay presents a publicly-trusted leaf**
(ACME/Let's Encrypt or the customer's own CA) on the path the enterprise
terminates or inspects. The private-CA REALITY path stays, but only on the
accessibility carriers of Part 1, which an enterprise deployment can disable
entirely (section 11). One product, two postures, selected by deployment.

### 8. Source and IP hygiene

8.1 Monitor the relay ranges against AbuseIPDB, FireHOL, Spur, Barracuda and the
Okta/Spur proxy feeds, and treat a listing as a page-able defect.

8.2 `recurd.dev`: DNSSEC, DMARC `p=reject`, SPF, a published abuse contact, and
publicly trusted certificates everywhere. These are standard inputs to
outside-in ratings; absent, they attach to anything that shares the name.

8.3 No crypto/wallet surface on the relay or control names, and fiat-only
billing on them — the "bandwidth sharing" / proxyware reading is what put the
consumer names in the VPN category in the first place.

### 9. Preflight and allowlist kit

9.1 Ship ready-made allowlist entries for Zscaler, Palo Alto, Netskope, Fortinet
and Cisco Umbrella, and for Okta network zones, generated from the section 6.3
allowlist.

9.2 An install-time preflight that dials the relay through the customer's own
egress and reports whether a web gateway or protective-DNS filter blocks or
recategorizes it, naming the observed category and the vendor's recategorization
route. "Blocked by default" becomes "here is the one rule to add, and here is
exactly what it permits."

### 10. Categorization and reputation program

10.1 Request correct categorization from Talos, Palo Alto, Zscaler, Fortinet,
Netskope and Cloudflare **before** design partners arrive — categorizers file by
what a name presents, so a developer-tool control plane and docs site read
differently from a VPN. Split the names the way ngrok does (marketing / control
plane / relay) so each is judged on its own content.

10.2 Pursue PSL listing for the shared relay domain only once it meets the
criteria the withdrawn `extender.bringyour.com` PR missed: scale, a registration
term over two years, publicly trusted certificates, and an abuse process. Until
then, default the enterprise edge to customer-owned domains.

---

## 11. What this explicitly does NOT do (the trust boundary)

recURd does not use the Part 1 camouflage carriers to evade a customer's own
security controls. The camouflage exists for the accessibility mission against a
hostile *network*, not to hide recURd from the *buyer* who deployed it. An
enterprise deployment:

- runs only the transparent carriers (publicly-trusted edge, section 7),
- advertises itself on the published allowlist (6.3) and on every connection's
  record (6.4), and
- can disable the private-CA REALITY path and the splice entirely.

A security product that hid from the security team would destroy its own
premise. The line between Part 1 and Part 2 is the product's integrity, not an
implementation detail.

---

## 12. Phasing

- **Near term (reputation, independent of the network code):** 6, 7, 8, 9, 10.
  These are infrastructure, DNS and certificate hygiene plus a client-side
  preflight; none touches the extender protocol, and they unblock enterprise
  design partners directly.
- **Mid term (accessibility, within the existing design):** 1.1, 1.2, 1.5
  (directory-release policy), 3.1 and 5 (turn the splice on with a measured
  borrowed list), 4 (bootstrap resilience). These are refinements of R, P and
  K7.
- **Later (accessibility, new mechanism):** 1.3 (WebRTC-first rendezvous), 1.4
  (fronting carrier), 2.1 (QUIC camouflage), 3.2 (inner-stream shaping). Each is
  a distinct carrier or a cross-cutting shaping layer and wants its own design
  note and the fingerprint harness extended first.

## 13. Open questions

13.1 **The architecture decision for recURd** (owner's call): does running
enterprise relays on dedicated `recurd.dev` infrastructure, off the consumer
network, still satisfy "recURd uses the network only for worldwide reach"? Part
2 reads it as yes — same network role, dedicated and allow-listable endpoints,
no consumer host or provider exit in the path — but 6.1 is a real change to how
the network is presented, and it needs an explicit yes before it ships.

13.2 QUIC camouflage (2.1): is an auth binding in the QUIC Initial achievable
without a distinguishing first flight, given how much of the Initial is in the
clear? Prototype against the fingerprint harness before committing.

13.3 Fronting (1.4): is there a CDN or refraction partner whose range we can
reach through? Without one, 1.4 is theory.

13.4 Measurement capacity (section 5): who runs the borrowed-names and
blocked-state measurement, and how often? The strongest tcp posture depends on
it.

## 14. Tests

Match the EXTENDER.md discipline: every behavior pinned, in process and
deterministic where possible.

- Directory-release policy changes (1.1, 1.2): extend
  `net_extender_directory_tiers_test.go` — gated-default reach, no blocked
  address dealt in-country, reputation-biased ordering.
- WebRTC-first rendezvous (1.3): extend the S test set — identity from the
  directory, address only via signaled rendezvous, no serving address in any
  record.
- QUIC camouflage (2.1) and inner-stream shaping (3.2): new in-process carrier
  tests plus fingerprint-harness goldens for the QUIC Initial.
- Splice-on default with a measured list (3.1, 5): the P10 properties, now with
  a non-empty bundled `extender_borrow` resource under test.
- Enterprise edge (7): the customer-facing relay presents a publicly-trusted
  chain and the camouflage path is absent when the transparent posture is
  selected.
- Preflight (9): against recorded gateway/DNS-filter responses, the category and
  recategorization route are reported correctly.
