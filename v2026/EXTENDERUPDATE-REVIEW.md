# Review of the extender hardening proposal

Reviewed 2026-11-01. Proposal: [EXTENDERUPDATE.md](EXTENDERUPDATE.md).

**Recommendation: keep the separation between accessibility and enterprise
reach, but revise the architecture and rollout claims before implementation.**
Dedicated enterprise infrastructure, explicit customer permission, measured
borrowed-name lists, and more resilient bootstrap are useful directions. The
proposal currently overstates what rendezvous, certificates, receipts, and
reputation hygiene establish, and describes some existing behavior as new work.

This is a design review grounded in the local connect checkout at
`69297f0558a7097c3b459639271255192905c040`, the corresponding implementation and
test sources, [EXTENDER.md](EXTENDER.md), and the
[outside-in reputation research](<../sidecar/research_notes/Sidecar pitch4 differentiation directions/L2-2.12-how-do-outside-in-security-controls-classify-urn.md>).
The local `origin/main` reference was 12 commits ahead; this review did not move
the checkout. External protocol claims were checked against the primary sources
linked below. No runtime tests, live reputation scans, or censorship experiments
were run for this review. Proposed acceptance tests below are work to add, not
results already obtained.

P1 findings affect whether the promised behavior can work or safely become the
default. P2 findings correct scope, evidence, and validation requirements.

## Where the findings went (2026-11-02)

Added after the review, whose text below is unchanged.
[EXTENDER-UPDATE2.md](EXTENDER-UPDATE2.md) took the accessibility findings into a
narrower proposal and carries their status section by section, with the owner's open
decisions. Commits were checked as ancestors of `origin/main` about 22:35Z. No test was
run for this note, and it adds no runtime, reputation or censorship evidence. The code
below is off by default where it has a switch, and no client or miner release carries
any of it.

| Finding | Where it went | Status |
| --- | --- | --- |
| 1. Enterprise transport and trust | recURd design track | Not started. Waits on the owner's §13.1 decision in [EXTENDERUPDATE.md](EXTENDERUPDATE.md) |
| 2. Enterprise routing policy | recURd design track | Not started. Waits on §13.1 |
| 3. WebRTC rendezvous | Outside EXTENDER-UPDATE2 | Not started |
| 4. Unsigned share, seed trust | EXTENDER-UPDATE2 §2 | Landed, not enabled. A separate signed envelope, verified under roots the client already holds, with every record verified again (connect `beb08bdb`, `5f048fc0`; sdk `ba40352e`; server `3a8d8ab10`, `dc384ab5c`). K7 stays unsigned. Open decisions are at §2.3 |
| 5. Gated default against capacity | EXTENDER-UPDATE2 §1 | Landed, off (connect `78cabce6`, `3ec6aeec`; sdk `b05442ca`; server `73865a730`). Gated is preferred, with a defined open fallback, not absolute. The capacity model isn't run, since no host is in the gated tier. Address rotation stays out of scope |
| 6. A measured list isn't enough to splice | EXTENDER-UPDATE2 §3 | The compatibility gate, staged rollout, telemetry and rollback are built, off (connect `14ccdd6a`, `f10f75d8`, `07ab63fc`, `bd52bd88`; server `1e94b4d78`, whose release isn't deployed). The measured list, §3.1, is paused by a safety check and awaits the owner. The carrier inventory isn't done, and the claim stays TCP only |
| 7. QUIC authentication field | Outside EXTENDER-UPDATE2 | Not started. No QUIC camouflage was built |
| 8. Blocked state and DoH | EXTENDER-UPDATE2 §1.3 | Blocked state: the outage rule (connect `3ec6aeec`, server `73865a730`), the block report sender (connect `bf59a4da`, sdk `eb72dc8c`, off) and the distinct-network threshold (server `2204aacb9`) landed. No block-age score was added. DoH: no change, so measured regional coverage is still to do |
| 9. Fingerprint harness | EXTENDER-UPDATE2 §3.4 | TCP landed: a real Chrome 156 capture runs through the actual carrier and probe paths (connect `14ccdd6a`). The parrot still trails it in two fields, an owner decision. Expanded QUIC parsing and the shaping experiment aren't started |
| 10. Reputation and PSL claims | recURd design track | Not started. [EXTENDERUPDATE.md](EXTENDERUPDATE.md) still reads as reviewed |
| 11. Preflight attribution | recURd design track | Not started |
| Carrier questions: shared ingress, DNS replacement | Outside EXTENDER-UPDATE2 | Not started |

The revised sequence, by order:

| Order | Status |
| --- | --- |
| 1 to 3 | Not started. recURd design track, behind §13.1 |
| 4 | Release and seed code landed, off. The capacity model and measured signal coverage aren't done |
| 5 | Client migration code landed, off. Borrowed-name maintenance is paused (§3.1) |
| 6 | Not started |

The acceptance table: Bootstrap has its tests on main, read and not run. So has Gated
default and rotation, for routine endpoint replacement, with automatic rotation out of
scope. Splice and QUIC has its splice half, with list cases on test lists. The other rows
aren't started. The operational experiments named after the table need a cohort, and no
owner or threshold has been named for them yet.

## Findings

### 1. [P1] Public certificates require a new enterprise transport and trust contract

**Proposal: §§7, 11, 12.** Moving the relay onto its own hostname and installing
an ACME certificate does not make the existing extender carrier compatible with
enterprise TLS inspection.

[`newExtenderTlsConfig` and `newExtenderLeafVerifier`](net_extender.go#L419)
disable ordinary chain verification and, when an identity key is present, require
the leaf certificate's signature to verify under that extender key. A normal
ACME leaf is signed by its issuer, and an inspecting proxy substitutes a leaf
signed by its own CA. Neither satisfies this identity check. This is a client
authentication change as well as a server certificate change. Palo Alto's
[pinning documentation](https://docs.paloaltonetworks.com/network-security/decryption/administration/troubleshooting-decryption/troubleshoot-pinned-certificates)
describes the same incompatibility with forward proxy decryption.

The TCP carrier also sends one HTTP request and then uses the connection as a
raw stream; see [`newExtenderRequest`](net_extender.go#L535) and
[`extenderStreamRequest`](net_extender.go#L724). An allowed HTTPS hostname does
not establish that this exchange works through a gateway enforcing HTTP
semantics. Explicit proxy discovery, proxy authentication, CONNECT or another
supported tunnel mechanism, idle timeouts, and UDP restrictions need a stated
support matrix.

**Change:** define the enterprise carrier before putting §7 in the near-term
infrastructure bucket. Choose the permitted inspection boundary and how relay
identity remains authenticated. Distinguish a publicly trusted origin
certificate from a customer-managed private CA; the latter is trusted only by
configured clients. State whether customer controls inspect just the outer
transport or also the inner payload, and how that interacts with the privacy
promise. An explicitly approved decryption exclusion is another deployment
option, but must be represented accurately in the allowlist kit.

### 2. [P1] Enterprise isolation needs enforced routing policy; a relay field is insufficient proof

**Proposal: §§6.3–6.4, 9.1, 11, 13.1.** Disabling REALITY and splicing does not
by itself restrict which endpoints or upstream paths the client may use. The
existing system has directory imports, bootstrap resolvers, direct paths,
multiple carriers, and retry/fallback behavior. A connection that starts on an
approved relay can still violate the promise if any later fallback or relay
upstream uses the consumer network.

**Change:** make enterprise posture an enforced policy with an explicit allowed
set of relay/control identities, resolution paths, carriers, and upstream
routes. Apply it before bootstrap and every retry or redirect, including cached
directory entries and imported settings. Failure to reach an approved endpoint
must produce a bounded, actionable failure rather than silently selecting an
unapproved route. Replace “can disable” camouflage with a required enterprise
invariant.

A signed receipt containing hostname, IP, and operator authenticates the
signer's assertion. It does not independently establish that no other logical
relay or provider exit participated. Define the signer, session binding, policy
version, observed peer identity, managed upstream hops, and evidence each
participant can actually attest. Scope the guarantee to those controlled hops.
Separate the relay the customer connects to from the final egress address a
destination or identity provider observes; those can require different
allowlists and reputation checks.

This makes the owner's decision in §13.1 concrete: is the enterprise path a
dedicated ingress to shared network services, or a dedicated path through final
egress? The current “no consumer ... provider exit” promise requires the latter
where traffic would otherwise traverse such exits.

### 3. [P1] WebRTC rendezvous reduces directory exposure but does not conceal a direct peer from a harvester

**Proposal: §§1.3, 14.** Removing addresses from signed records helps against
passive directory collection. An admitted attacker can still request a
rendezvous and learn the serving address. ICE exchanges connectivity candidates,
and a direct connection exposes its destination to the on-path network. This is
part of WebRTC's address model, described in
[RFC 8828 §3](https://www.rfc-editor.org/rfc/rfc8828.html#section-3).
Expiring signaling authorization does not expire an already learned static IP.

The current carrier's production signaling runs through the exchange:
[`net_extender_webrtc.go`](net_extender_webrtc.go#L5),
[`ExchangeOffer`](net_extender_webrtc_signal.go#L355), and EXTENDER.md S2. If the
client needs an extender to reach that exchange, making this its initial reach
path creates a bootstrap dependency that the proposal has not resolved.

**Change:** describe this as controlled address disclosure. Carry directory
admission and disclosure limits through signaling; merely publishing fewer
fields must not allow unlimited address discovery through another API. Specify
an independently reachable rendezvous bootstrap, authenticated identity binding,
replay/expiry rules, and bounded negotiation work. Cover UDP-blocked networks and
platforms where this native carrier is unavailable.

If hiding the extender's address from admitted clients is required, evaluate a
relay-only or cooperating shared-ingress design explicitly. That moves the
observable address and costs to another service; it needs its own capacity,
blocking, and abuse analysis. The proposed test that asserts “no serving address
in any record” cannot validate the broader harvesting claim. Inspect what an
admitted hostile peer learns from signaling and packets.

### 4. [P1] The existing share payload is unsigned; automated seed distribution needs a separate trust design

**Proposal: §4.1.** The statement that `ur-ext:1:` is signed is incorrect.
[`EncodeExtenderShare`](net_extender_share.go#L52) serializes a protobuf and
base64url-encodes it. [`ExtenderShare`](protocol/extender.proto#L233) has version,
network host, addresses, and optional settings; it has no signature. Encoding
and validation provide no issuer authentication.

K7 deliberately imports addresses as unverified bootstrap hints. Its optional
settings can replace DNS, gossip, and root keys and are accepted deliberately;
see the [share trust-model comments](net_extender_share.go#L16). Automatically
fetching such settings from a URL or DHT would be a different trust decision.
A signature is not useful if its sole trust anchor arrives in the same
untrusted object.

**Change:** either retain address-only, untrusted hint distribution with the
existing verification boundary, or specify a new signed seed envelope. For the
latter, define an independently provisioned verification key, network binding,
version, expiry, rollback protection, key rotation/revocation, and bounded
decoding and dialing. Reject automatic changes to trust anchors and control
settings unless authorized by the established trust chain. Seed signatures must
not promote an address to a verified extender record.

Test forged and cross-network seeds, expiry and clock handling, replay of older
sets, key rollover, oversized inputs, and attempted settings replacement with
deterministic keys and clocks.

### 5. [P1] Gated-default reach and rapid address rotation conflict with existing release constraints

**Proposal: §§1.1, 1.5, 12.** Reachability of the release route does not imply
that it can release a usable address. The
[current policy](net_extender_directory_release.go#L38) allows one record for a
new identity, three for a trusted identity, a seven-day epoch, and at most ten
distinct identities per extender per country over thirty days. Disclosure
history is retained even when records become blocked or unavailable.
EXTENDER.md R3 explicitly permits an empty answer until the next epoch.

With those defaults, N extenders can support at most 10N distinct identity
admissions in a country in the window if each needs just one record; providing
three distinct records consumes that capacity faster. Partitioning and failures
further reduce usable capacity. §1.1's absolute ban on open-tier selection can
therefore strand clients even while the release API is healthy.

Address rotation also cannot use C4 to update gated clients:
[EXTENDER.md R1](EXTENDER.md#L3399) explicitly excludes gated records from DNS,
public feeds, gossip, and the drip. Gated records refresh through authenticated
release requests. The seven-day epoch limits disclosure, not every refresh;
releases can reissue previously disclosed records within that epoch.

**Change:** size the fleet against admission demand, define empty/denied-release
behavior, and make any accessibility fallback policy explicit. Refresh rotated
gated endpoints over authenticated channels without bypassing disclosure limits
or publishing them to open channels. Specify old-address drain, recovery when
the cached address and control route both fail, client refresh cadence, and
IPv4/IPv6 behavior. Clarify who supplies the sacrificial rotating fleet: R1
currently assigns operator hosts to gated and volunteers to open.

Treat “rotation beats blocking” as a measured hypothesis. Prefix/ASN blocking,
address reuse, provisioning delay, and the cost of refreshing clients can defeat
it even when individual addresses change quickly.

### 6. [P1] A measured borrowed list is not sufficient to enable splicing by default

**Proposal: §§3.1, 5, 12.** EXTENDER.md P3's
[Phase A/B migration](EXTENDER.md#L3138) says why the flag is initially off:
legacy clients are indistinguishable from unauthenticated probers and stop
working when splicing is enabled. This behavior is deliberately covered by
[`TestExtenderCamouflageLegacyClientPhaseB`](extender/extender_camouflage_test.go#L523).
Clock-skew fallback also loses TCP in Phase B.

**Change:** retain a client compatibility and retirement gate, staged operator
rollout, failure telemetry, and rollback criteria in addition to the measured
list. Cover stale or unreachable targets and client/server list-version skew;
publish what happens when the system falls back to legacy termination. A
nonempty resource alone does not show that targets work from the deployed
extenders.

The endpoint also remains distinguishable if its other exposed carriers still
answer probes with the old behavior, which §2 already acknowledges. State the
scope as TCP protection, or disable/separate those exposed carriers until they
meet the same endpoint-level objective. Do not promote the TCP flag into a
claim that the address is now resistant to active probing on every port.

### 7. [P1] QUIC cannot copy the current TCP authentication field

**Proposal: §§2.1, 13.2.** The TCP construction puts its authentication tag in
the TLS legacy session ID; see
[`extenderCamouflageClientHandshake`](net_extender_camouflage.go#L222).
[RFC 9001 §8.4](https://www.rfc-editor.org/rfc/rfc9001.html#section-8.4)
prohibits requesting TLS middlebox compatibility mode in QUIC and specifies
that servers should reject a nonempty legacy session ID. Reusing that field
would create a protocol violation and a distinguishing first flight.
Furthermore, Initial protection uses publicly derivable keys rather than a
secret against observers; encrypting the Initial does not hide a novel tag
placement. See [RFC 9001 §7](https://www.rfc-editor.org/rfc/rfc9001.html#section-7).

**Change:** keep §13.2's research gate and remove the implication that the work
is a direct port of P1 plus a splice. Specify and review a compliant
authentication construction, replay handling, address validation, and bounded
unauthenticated work before committing to delivery. The negative path must
match the chosen HTTP/3 origin behavior, including version negotiation and
failure behavior; a TCP borrowed-site list is not evidence of HTTP/3 support.

### 8. [P2] Credit existing blocked-state and DoH behavior; define the actual new placement signal

**Proposal: §§1.2, 4.2, 14.** Two advertised improvements already exist:

| Proposed behavior | Evidence in the checkout | Actual remaining work |
| --- | --- | --- |
| Skip extenders blocked in the requester's country | [`Release`](net_extender_directory_release.go#L250) filters blocked candidates; [`TestExtenderReleaseSkipsARecordBlockedInTheCountry`](net_extender_directory_tiers_test.go#L723) covers it | Verify production signal coverage and define any new ranking or recovery policy |
| Multiple default DoH providers and country selection | [`DefaultDnsResolverSettings`](net_http_doh.go#L152) includes Cloudflare, Google, Quad9, and OpenDNS; [`RegionalControlDohUrls`](net_http_doh_regional.go#L64) and [`IngressDohSettings`](net_http_doh_control.go#L151) support regional bootstrap, currently with China presets | Expand measured regional coverage, maintain endpoints, and validate failure behavior |

“Lowest observed block age” is ambiguous: it could prioritize a recently blocked
address. The current release source exposes a blocked boolean, not the proposed
age/risk score. Define the observation, timestamp, confidence, expiry, recovery
rule, and treatment of missing data before choosing an ordering. Preserve the
partition and disclosure caps when reordering.

The existing [blocked-state computation](net_extender_directory_canary.go#L299)
requires distinct reports plus a recent successful operator probe. It is evidence
of a country-specific reachability problem, not conclusive attribution to a
censor. Keep outage, carrier failure, country/ASN scope, and malicious reports
distinct. A canary's leak attribution should not automatically become a
country-wide blocking verdict for other addresses.

### 9. [P2] The fingerprint harness does not yet establish real-browser conformance

**Proposal: §§2.1, 3.2–3.3, 14.** The
[golden provenance](fingerprint/README.md#L94) explicitly identifies the committed
Chrome 133 hello as synthetic, generated from the same uTLS profile being
tested. The [QUIC coverage](fingerprint/README.md#L78) checks version, packet
type, and minimum padding; decrypted transport parameters and CRYPTO-frame
comparison remain future work. This is useful regression infrastructure, but
does not establish that the carrier matches a current real browser.

**Change:** make independently captured, versioned browser fixtures and expanded
QUIC parsing prerequisites for the corresponding conformance claims. Record
capture provenance and OS scope. Exercise actual authenticated and rejected
carrier handshakes, rather than only a generic browser profile. Include malformed
hellos, wrong keys, replay, skew, unreachable borrowed targets, and fallback.

For shaping, distinguish byte-stream correctness from detection resistance.
Deterministic tests should prove bounded buffering, cancellation, backpressure,
and lossless framing under segmentation and padding. A separate reproducible
experiment must compare classifier performance and false positives, latency
tails, bandwidth, and CPU cost. Passing a golden or adding jitter does not
establish resistance to TLS-in-TLS classification.

### 10. [P2] Reputation separation and PSL inclusion are not guarantees of acceptance

**Proposal: motivation, §§1, 6.1, 8, 10.** Preserve the research dossier's
distinction between observations and inferences. Its Talos results concern the
queried hosts, and its VPN categories coexist with neutral or favorable
reputation. They do not establish a malicious verdict or a block by every
customer policy. The dossier does not demonstrate that fiat billing or a new
domain will cause a particular category assignment.

Dedicated names and addresses improve operational separation and allowlisting.
They do not guarantee that categorizers stop associating the service with its
operator or stop recognizing its proxy/tunnel function. Replace “stops the
transfer” with a testable goal, retain dated provider-specific evidence, and
measure the actual enterprise ingress and final egress separately. Page verified
abuse or customer-impacting regressions; classify informational proxy/datacenter
listings separately instead of treating every listing as the same incident.

Likewise, EXTENDER.md describes the March 2026 REALITY blocking account as
community reports with an inferred cause. The proposal upgrades it to a certain
causal claim. Restore that qualification unless a measurement establishes the
mechanism.

PSL inclusion addresses domain boundaries, particularly cookies and site
grouping; its PRIVATE section is for operators delegating subdomains to mutually
untrusting parties. It is not a reputation certification, and the guidelines
reject attempts to use it to circumvent vendor protections. See the
[PSL purpose](https://publicsuffix.org/learn/) and
[submission guidelines](https://github.com/publicsuffix/list/wiki/Guidelines).
Determine whether the planned tenant namespace actually needs this isolation.
Customer-owned domains can be a product option, but their necessity does not
follow from a pending PSL request. Cite the withdrawn request as its particular
review history, not a complete universal acceptance checklist.

### 11. [P2] Preflight cannot always identify a blocking vendor, category, or single corrective rule

**Proposal: §§6.3, 9, 14.** Recorded block pages can support recognized cases.
A DNS failure, timeout, reset, or certificate rejection often contains no
authenticated vendor/category information. Multiple controls can fail in
sequence. A public category lookup also need not reflect a customer's configured
policy.

**Change:** report the observed failure stage and evidence first, with explicit
unknown results where attribution is unavailable. Recognized responses may add
a vendor/category and a documented review route; do not infer those from a
generic error. Treat remediation as proposed scoped policy changes requiring
the customer's administrative workflow, not a guaranteed single rule.

The manifest should distinguish control, relay, resolution, and final-egress
requirements; include relevant proxy/inspection expectations, ownership,
versioning, and planned address-change overlap. Network-zone configuration for
an identity provider should describe the address it actually observes rather
than mechanically reuse the relay ingress CIDRs. Test from the deployed agent's
network and proxy context, which can differ from the installer's shell.

## Carrier questions that need separate design notes

**Shared ingress (§1.4):** a cooperating CDN can change the address an observer
can block and increase the collateral cost of doing so. It does not remove the
ability to harvest the exposed endpoint, block its SNI, or apply service-specific
controls. Define the partner's routing and certificate arrangement, the
observable identity, and the access-control boundary. CDN forwarding and
refraction routing are different deployment models; neither should inherit an
unqualified “address-harvesting stops working” claim.

**DNS replacement (§2.2):** distinguish fetching bootstrap records over DoH from
transporting the extender byte stream. Standard DoH carries DNS queries and
responses, not an arbitrary stream to a chosen backend
([RFC 8484 §4](https://www.rfc-editor.org/rfc/rfc8484.html#section-4)). Using a
public recursive resolver would require an actual recursive DNS tunnel and an
authoritative service; a cooperating HTTPS relay is another design. Specify
which is intended, its capacity and failure behavior, and what connects after
bootstrap when UDP 443 and direct TCP are unavailable. A “DoH/DoT-shaped” label
alone does not provide that path.

## Revised sequence and acceptance criteria

The proposal's near-term bucket combines infrastructure preparation with new
protocol and policy guarantees. Split those deliveries and gate defaults on
evidence:

| Order | Deliverable | Acceptance boundary |
| --- | --- | --- |
| 1 | Decide enterprise route ownership and inspection support; correct the as-built statements | Written trust and route contract, supported proxy modes, and precise receipt claims |
| 2 | Prepare dedicated infrastructure, domain/contact hygiene, and versioned allowlist data; begin vendor review | Measured baseline from target customer networks, with category and policy distinguished |
| 3 | Implement one transparent enterprise path and its routing policy | Approved route works; failures never invoke unapproved bootstrap, carrier, or egress paths |
| 4 | Improve existing release/resolver operations and define seed authentication | Capacity model, measured signal coverage, authenticated gated refresh, and bootstrap trust tests |
| 5 | Establish real capture/borrowed-name maintenance and complete client migration | Compatible clients, measured targets, staged splice rollout, and explicit rollback thresholds |
| 6 | Prototype rendezvous disclosure control, shared ingress, QUIC authentication, and shaping independently | Each passes its own threat-model experiment before becoming a default |

Extend §14 with deterministic tests at the actual failure boundaries:

| Area | Required regression or acceptance case |
| --- | --- |
| Enterprise transport | Local test CAs exercise the selected trust model: valid managed trust succeeds; wrong hostname, wrong relay identity, and unapproved inspection fail. Exercise proxy authentication, tunnel framing, half-close, and idle failure. Public issuance is a separate deployment check. |
| Enterprise isolation | A fake dial observer records every attempted destination. Force relay, DNS, and control failures; feed cached consumer records and imported settings; assert zero unapproved attempts and accurate failure receipts. |
| Gated default and rotation | Fake time and a bounded fleet exercise probation, exhausted country capacity, blocked last record, repeated refresh, epoch rollover, rotation/drain, and loss of the old control path. Assert no gated publication on any open channel. |
| Bootstrap | Fixed signing keys and clocks cover forged/cross-network/expired/rolled-back seed sets, unauthorized trust replacement, and address hints remaining unverified. |
| WebRTC | An admitted hostile peer attempts repeated rendezvous and inspects candidates. Verify disclosure limits, identity binding, replay refusal, cancellation, and cold-start behavior when the exchange is unreachable. |
| Splice and QUIC | Preserve legacy/skew migration tests; cover empty/stale/mismatched borrowed lists and rejected probes. For QUIC, validate protocol fields, replay and resource bounds, and both successful and failed flights against independent captures. |
| Shaping and preflight | Pin segmentation, buffering and cancellation behavior with deterministic inputs. Replay recognized block pages plus ambiguous DNS/TCP/TLS failures; ambiguous results must remain unknown. |

Keep operational experiments separate from these deterministic gates: endpoint
reachability by country/ASN, fleet admission capacity, harvest-to-block timing,
browser drift, reputation changes, and classifier resistance require maintained
measurements. Name an owner, cadence, success threshold, and rollback trigger for
each before claiming a default is ready.
