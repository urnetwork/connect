# Third-party ad-blocking DNS presets plan

Status: final plan, 2026-11-09. The owner decided every open question on
2026-11-09, in two rounds and a later answer on preset names; section 8
records the decisions, no decision is pending, and the sections below
follow them. Adblock provider facts come from
the ad-blocking DNS research of 2026-11-08 (the report and its notes, kept in
the private factury repo); this document cites the adblock providers' public
pages the report cites, and marks the report's own live probes as "probed
2026-11-08". Facts the research could not settle are listed in section 10
with the check that settles each.

Terminology (decision 18): an "adblock provider" is a DNS company whose
resolvers the catalog or the defaults name, such as AdGuard, Control D,
Quad9, Cloudflare, DNS4EU, NextDNS or DNS.SB. A bare "provider" means what it
means everywhere else in URnetwork, a bandwidth provider, such as the exit
provider. Code identifiers, key ids and quoted strings keep their names.

Line numbers refer to connect `bc3fec12`, sdk `90b7a59f` and server
`562ead790` unless a section says otherwise. The Google removal landed in
connect `b49cbc21`, sdk `92553ef0` and mmm `f7135bb6d`.
`fix/default-resolvers-v2` (the OpenDNS removal, Cloudflare's second address
and the policy-filter guard, 3.1) landed in connect `829a5a54`, sdk
`5c7a6eb7` and mmm `927c0bd04`. `feature/dns-defaults-v3` (DNS.SB among the
defaults, the sdk's `DefaultServersVersion` 3 with the kz migration, and
DNS4EU's family names; decisions 19, 20 and 22) lands in connect and the sdk
with the next build. `feature/dns-preset-names` (every preset's own name,
decision 24) landed in sdk `12199300`, localizations `276765f`, apple
`48459757`, android `549ad0107`, windows `fdcf359c`, linux
`c893407` and mmm `2f5499ee5`, and ships with the next build.

## 1. Original request

Owner decisions, 2026-11-08:

- Remove Google from URnetwork's default DNS resolver lists
  (`fix/no-google-dns`, 3.1). This plan builds on it.
- Add quick-pick presets for popular ad-blocking DNS resolvers in the DNS
  settings.

The trigger is support inbox 8236 (in-app feedback, in Spanish): use
Cloudflare and avoid Google DNS entirely for privacy, and add AdGuard DNS and
Control D to the DNS settings.

The item's honesty and safety review set three constraints. This plan treats
them as requirements:

- AdGuard DNS and Control D only as options a user chooses, never added to
  the defaults without a decision by the owner.
- Encrypted DNS stays encrypted: no plaintext fallback added to reach a newly
  offered resolver.
- A change to the default lists must not break bootstrap on networks that
  block some operators, or the regional presets.

Owner decisions, 2026-11-09 (section 8): the full first catalog, with the
"No filtering" picks and with DNS4EU added, while RethinkDNS stays out
because its resolver forwards to Google upstream; every adblock provider in
the catalog counts as approved, so no preset waits for an adblock provider's
answer; manual entry under "Custom" always stays; ad blocking is off by
default in every app, and URnetwork's default DNS stays the default
everywhere; a failing preset falls back or fails closed by purpose; Fast DNS
on connect uses URnetwork's DNS; the catalog and its addresses are built into
the sdk and change only with releases; and a local count of blocked lookups
ships in the first release. A second round the same day settled the
terminology, made DNS.SB a default resolver, named DNS4EU's two family
presets, let the personal ID show in its own edit field, and moves the old
Google-only kz recommendation to today's (decisions 18 to 23). Later that
day the owner gave every preset a short, descriptive name of its own, such
as "AdGuard Family" or "Cloudflare Malware Blocking" (decision 24).

## 2. Goals and non-goals

Goals:

- G1. A user picks a third-party filtering resolver with one tap in the DNS
  settings of every app: apple, android, windows and linux. The web and the
  extension control hosted devices, which take no resolver settings (3.9).
- G2. The picks are grouped by outcome, "Block ads and trackers", "Block
  malware", "Family" and "No filtering", and each says plainly what it
  blocks, including "not ads" for the malware and family filters that block
  no ads (4.A3).
- G3. A user with an account at NextDNS, Control D or AdGuard DNS enters a
  personal configuration ID, which travels only in the encrypted DoH path
  and is checked in the sdk (4.D).
- G4. A preset resolves on the same path as the defaults: DoH from the
  device, through the tunnel, egressing at the exit provider. It is encrypted
  end to end, the resolver's own name is never looked up in plaintext, and
  the preset is never queried off the tunnel: with Fast DNS on connect, the
  connect window uses URnetwork's default DNS (C7).
- G5. When the chosen resolver fails, the user is told, and nothing switches
  to plaintext or to the host network. Ad-blocking and no-filtering presets
  fall back to URnetwork's default DNS through the tunnel; family, malware
  and personal presets fail closed (O2, C1).
- G6. One catalog, one selection model and one set of validation,
  verification and status rules, in the sdk. The apps keep screens only
  (owner rule, 2026-11-03: app logic goes in the sdk).
- G7. Deterministic tests for every rule above, in connect and the sdk.

Non-goals:

- N1. No preset becomes a default (owner rule, 2026-11-09, for every app:
  URnetwork's default DNS is the default everywhere, ad blocking is off by
  default, and presets are only ever user-chosen). The defaults stay
  URnetwork's built-in list, Cloudflare (1.1.1.1 and 1.0.0.1) and Quad9's
  unfiltered 9.9.9.10 (`fix/default-resolvers-v2`, 3.1), then DNS.SB
  (185.222.222.222 and 45.11.45.11, decision 19), and the screen opens on
  it.
- N2. The control-name (ingress) DNS does not change. Presets never touch
  the bootstrap DoH servers, the connection region or the bootstrap plain
  fallback (3.4). An ad-blocking resolver adds nothing there, and a filter
  that someday categorizes VPN domains must never break sign-in.
- N3. No OS-level DoH or DoT. The platforms keep advertising the plain-DNS
  stand-in that the mux claims (sdk `tunnel_dns.go:5-29`).
- N4. No linked-IP, legacy, EDNS-option or plain-IP personal modes (D2).
- N5. No DoT or DoQ upstream and no plaintext presets. The existing regional
  recommendation (plain DNS in cn, ru, ir and others) stays as it is.
- N6. No network-wide settings sync. Logout still wipes local DNS settings.
- N7. No presets for hosted proxies or hosted devices in this plan (O7,
  later).
- N8. No change to the built-in blocker or its lists, and no URnetwork-side
  allowlist for a third party's blocks.
- N9. No server-side telemetry of which preset a user picks.
- N10. No URnetwork-run blocklist in the mux. The research's conclusion
  points there as the long-term answer (it removes the third-party terms
  question, not the Play one); it is a separate plan.

## 3. What already exists

### 3.1 Default resolver lists, the Google and OpenDNS removals, DNS.SB, and the guard

Line numbers in this section refer to connect on `fix/default-resolvers-v2`;
`feature/dns-defaults-v3` adds DNS.SB's entries at the end of each list.

- `DefaultDnsResolverSettings` (`net_http_doh.go:143-211`) turns on remote
  DoH and local plain DNS. Remote DoH v4 and v6 (`:171-180`) and remote
  plain DNS (`:186-195`) name Cloudflare at 1.1.1.1 and 1.0.0.1 and Quad9 at
  its unfiltered 9.9.9.10 (v6 `2620:fe::10`), interleaved (Cloudflare,
  Quad9, Cloudflare), then DNS.SB at 185.222.222.222 and 45.11.45.11 (v6
  `2a09::` and `2a11::`, decision 19); the local plain-DNS lists
  (`:200-209`) name Quad9's 9.9.9.10, both Cloudflare addresses, then
  DNS.SB's two. The remote DoH v4 list is `https://1.1.1.1/dns-query`,
  `https://9.9.9.10/dns-query`, `https://1.0.0.1/dns-query`,
  `https://185.222.222.222/dns-query`, `https://45.11.45.11/dns-query`, and
  the v6 list pairs with it: `https://[2606:4700:4700::1111]/dns-query`,
  `https://[2620:fe::10]/dns-query`,
  `https://[2606:4700:4700::1001]/dns-query`, `https://[2a09::]/dns-query`,
  `https://[2a11::]/dns-query`. The defaults do not filter, so Quad9's
  malware filter at 9.9.9.9 is not among them (2026-11-08).
- History: Google Public DNS left the defaults in connect `b49cbc21`
  (`fix/no-google-dns`), and kz's regional entry (Google) became Cloudflare
  and Quad9; tm kept Turkmentelecom alone (until 2026-11-11, see version 4
  below). OpenDNS left them on
  `fix/default-resolvers-v2`: it refuses French clients (REFUSED, EDE 16),
  has left Portugal and Belgium, answers blocks with ordinary records,
  forwards the client's /24 as ECS, and its free terms are marked expired.
  On the same branch Quad9 moved from 9.9.9.9 to 9.9.9.10: 9.9.9.9 answers
  a blocked name with NXDOMAIN and EDE 17, and 9.9.9.10 has the same
  operator, certificate and privacy terms without the filter. kz's regional
  plain-DNS entry for Quad9 moved to 9.9.9.10 too. DNS.SB (xTom GmbH,
  Düsseldorf) joined them on `feature/dns-defaults-v3`, after the three
  (decision 19). The research of 2026-11-08 found it the one new operator
  that passes every technical and privacy test: unfiltered (no blocks in its
  2019 to 2025 transparency reports), no query logs, no ECS, DNSSEC
  validation, and an ip-literal DoH certificate whose IP SANs carry
  185.222.222.222, 45.11.45.11, `2a09::` and `2a11::`, answering over
  HTTP/2. Its v6 endpoints were not reached live (section 10); its Let's
  Encrypt IP certificates last six days, so a failed renewal would take an
  endpoint down within days, which the race tolerates; and it has no point of
  presence in France, Portugal or Belgium. Its terms ask commercial users for
  prior authorization: the owner approved the change and sends the
  permission email (draft 03 in `factury/adblock/`). A third operator also
  lowers the share of cold lookups on a French exit whose two picks both fail
  once Quad9 leaves France on 2026-11-24 (the research's model, two picks
  from uniform cold weights: 6.7% to 2.2% on a dual-stack exit, 40% to 33%
  on a v4-only one). Everything derived from the default lists follows
  (tunnel DoH, the Fast DNS projection, the control-name DoH and its built-in
  copy, the Linux and Windows egress fallback in `egress_dial.go`).
- The operator pair tables `knownDohUrlPairs` and `knownDnsServerPairs`
  pair each operator's v4 and v6 endpoints, and keep the pairs of Quad9's
  filtering 9.9.9.9, Google and OpenDNS for a user who names them. DNS.SB's
  pairs are its two published servers, public-dns-a (185.222.222.222,
  `2a09::`) and public-dns-b (45.11.45.11, `2a11::`). A url outside the
  tables pairs by list index with another url outside them
  (`familySiblings`), and `dualStackList` drops repeats.
- The block-action ignore baseline and the blocker's resolver subtraction
  are a fixed set, `wellKnownResolverAddrs` (`ip_block_action.go`): every
  endpoint of the pair tables and of the defaults, so Quad9's 9.9.9.9,
  Google and OpenDNS stay in both, and DNS.SB's four addresses are in both
  (the shipped blocker table blocks none of them).
- The policy-filter guard (`dohBuiltInEndpoints`, `dohGuardedResult`,
  `dnsResponseFiltered`) covers the built-in endpoints only: the urls of the
  default remote and local DoH lists and of the regional bootstrap presets
  (`regionalControlDohServers`, which the ingress strategy and the bootstrap
  lists draw on), matched by url with the ip in canonical form and the
  default port and trailing slashes ignored. From a built-in endpoint a
  response with an extended DNS error of 4, 15, 16, 17 or 18 under any
  RCODE, or a NOERROR with only `0.0.0.0` or `::`, is neither an answer nor
  a miss: it never ends the race, earns no success credit and is never
  cached, and a `0.0.0.0` or `::` beside real addresses is dropped. When
  every server is filtered or failed the lookup is a SERVFAIL, with
  serve-stale. Any other server, one the user added or a preset (B4), is
  answered as it answers: its sink answers and extended-error NXDOMAINs are
  answers and misses, cached as usual. A user who names a built-in endpoint
  in their own list (1.1.1.1, say) gets the guard for that endpoint. Quad9's
  filtering 9.9.9.9 is not a built-in endpoint, so a user who names it gets
  its blocks. DNS.SB's four urls are built-in endpoints like the rest of the
  defaults; the extended error 29 it puts on a synthesized NXDOMAIN is no
  policy code, so its misses stay misses. Plain DNS has no guard: it runs
  through `net.Resolver`, which shows neither the extended error nor which
  server answered.
- Every DoH query carries an EDNS OPT record (UDP size 1232, DO clear). The
  mux returns a forwarded response without the upstream's OPT record to a
  client whose query had none (RFC 6891 §7, `dnsResponseForResponder`), and
  with it to a client that sent one. Its synthesized A and AAAA replies
  carry no OPT record for any client.
- A miss that one source alone reported, an operator's v4 and v6 endpoints
  counting as one source, and that source a built-in endpoint, is cached
  for `LoneMissExpiration` (30 s), not `MissExpiration`. A custom server's
  miss is cached as usual.
- `net_http_doh_removed_operators_test.go` reads every default, regional,
  ingress and bootstrap list, `net_http_doh_ede_guard_test.go` pins the
  guard and its scope, `ip_mux_dns_edns_test.go` pins the OPT rule, and
  `net_http_doh_dns_sb_test.go` pins DNS.SB on every list built from the
  defaults, its pairs, the guard on its endpoints and its lone-miss rule.
  Probe targets (`ip_probe_targets.go`) and ICE address discovery
  (`ice_net.go`) still name Google, and the probe pool names OpenDNS; they
  are not resolver lists.
- sdk migrates saved records. `DefaultServersVersion` 1 (sdk `d1e5e9fd`): a
  record without it whose list holds exactly the servers of a Google-era
  default list (2026-07-02 to 2026-11-08), in any order, loads with the
  current default list, keeping the saved order of the servers that remain;
  so did the old tm recommendation (Turkmentelecom and Google), until
  version 4. Version 2
  (`fix/default-resolvers-v2`): a record below version 2 holding exactly an
  OpenDNS-era default list loads with the current list too. In every
  migrated list Quad9's 9.9.9.10 takes the place of its 9.9.9.9, and the
  other servers keep their saved order. Every other list loads as saved, so
  a user who chose 9.9.9.9 outside an exact old default list keeps it, and a
  record at the current version never migrates. The editors start from the
  mux resolver, which has no local plain lists, so none of those migrates.
  A version 1 record whose remote plain lists hold exactly the version 1
  kz recommendation (Cloudflare and Quad9's 9.9.9.9, both families, in any
  order) loads with the current kz recommendation. A version 0 record with
  those lists keeps them as saved, 9.9.9.9 included, because before version
  1 kz recommended Google alone, so there they are the user's own choice
  (sdk `29df346d`).
- Version 3 (`feature/dns-defaults-v3`, decisions 19 and 22): a record below
  version 3 holding exactly a version 2 default list (Cloudflare at both
  addresses and Quad9's 9.9.9.10, in any order) loads with the current list,
  the saved order kept and DNS.SB's two servers after it, so the defaults
  of every earlier version chain to today's. The edit rule
  (`dnsResolverSettingsAsEdited`) reads a selection's lists of any earlier
  version as that release's derivation through the same table
  (`dnsResolverSettingsOldDefaultServerLists`), so an app's copy of
  `default` with the version 2 lists seeds a device as `default` with
  today's lists. A list the user edited loads as saved, and a reordered one
  keeps its order with DNS.SB after it; neither selects `default`.
- Also at version 3, the old kz recommendation moves (decision 22): a record
  below version 3 that holds exactly the kz recommendation of its time,
  Google alone, loads as the `regional-kz` selection, today's kz
  recommendation (Cloudflare and Quad9's 9.9.9.10, unencrypted). The
  variants an sdk applied are remote plain DNS alone with `8.8.8.8`
  (2026-07-10 to 2026-09-11) and with `8.8.8.8` and `2001:4860:4860::8888`
  (2026-09-11 to 2026-11-08), every other list empty, with any fallback
  choice and mask. The recommendation of February to July 2026 also turned
  on local plain DNS with 1.1.1.1, but no sdk saved DNS settings before
  2026-07-10, so no record holds it. The owner accepts that a user who typed
  exactly that Google-only list moves too. Any other list with Google loads
  as saved, and so does the Google-only list in a record at version 3, which
  can only be a choice made after the move.
- Version 4 (`fix/regional-dns-ir-tr-tm-1111`, 2026-11-11): ir's regional
  recommendation names Shecan and Electro instead of Shecan and 403.online's
  private 10.202.10.10, which no query through the tunnel reaches; tr's names
  Cloudflare and Quad9's 9.9.9.10, as kz's, instead of TTNET's and Turkcell
  Superonline's resolvers, which answer only their own customers; and tm has
  none, since Turkmentelecom refuses outside clients and answers the
  platform's own names with 127.0.0.1 (`net_http_doh_regional.go` records
  the measurements). In sdk a record from before the presets that holds an
  earlier ir, tr or tm recommendation exactly (remote plain DNS alone, the
  tm one with or without Google, servers in any order, any fallback choice
  and mask) loads as that country's regional pick, so ir and tr follow
  today's lists and `regional-tm` applies URnetwork's default with its
  notice. The edit rule reads a regional selection holding an earlier
  recommendation of its country as that release's derivation
  (`dnsResolverSettingsEarlierRegionalRecommendations`). The Google-era tm
  pair no longer migrates to the current tm list, which tm no longer has.
- mmm updated the ur.io threat model's resolver rows to match
  (`927c0bd04`); DNS.SB's rows are a follow-up there.

### 3.2 How a user's DNS flows while connected

1. The platform advertises a plain-DNS stand-in address to the OS
   (`DefaultDnsUpgradeMaskAddress`, `net_http_doh.go:324-337`; sdk
   `tunnel_dns.go:5-29`). Nothing listens there.
2. The `UpgradeMux` claims UDP and TCP port 53 inside the device, before a
   packet reaches the tunnel (`ip_mux_upgrade.go:3-7`, `onSend` at `:981`).
   It refuses DoT to its own stand-in so Android's automatic Private DNS
   falls back to port 53 at once; DoT to any other address passes through
   (`:1129-1151`).
3. `handleDns` (`:1622-1731`) answers in this order: the built-in blocker
   for every query type (`:1654-1680`), empty AAAA while no exit carries IPv6
   (`:1682-1698`), then A and AAAA are resolved and synthesized and every
   other type is forwarded opaquely (`:1700-1730`).
4. The mux resolver is remote DoH only (`DefaultUpgradeMuxSettings`,
   `:122-184`). The DoH requests are dialed through the mux's private gVisor
   tun (`tun.go:1018-1049`, `DialContextSettings{DialContext: tun.DialContext,
   dohTun: true}`), so the TLS connection to the resolver egresses at the exit
   provider. The resolver sees the provider's address, and the answers match
   the exit's location.
5. A query fans out to at most two servers (`ip_mux_upgrade.go:553`) in a
   weighted random order, hedged and staggered. The address path returns the
   first answer with records; "an authoritative miss is not short-circuited
   ... so a filtering resolver's NXDOMAIN can't override a server that
   resolves the name" (`net_http_doh.go:1894-1905`). The opaque path takes the
   first NOERROR or NXDOMAIN (`:2186-2215`). A filtering resolver mixed with
   an unfiltered one therefore does not filter. On `fix/default-resolvers-v2`
   the policy-filter guard goes further for the built-in endpoints (3.1):
   from them a response with an extended DNS error of 4 or 15-18, or only
   `0.0.0.0`/`::`, is never an answer on either path. A server the user added
   is outside the guard, so a list of filtering resolvers alone filters.
6. The resolver chain does not stop at a DoH NXDOMAIN: local DoH, remote
   plain DNS and local plain DNS run whenever DoH produced no addresses and
   their toggle is on, without checking the miss (`net_http_doh.go:1195,
   1208, 1231`). Only the bootstrap plain fallback checks it (`:1254`).
7. An NXDOMAIN becomes an empty NOERROR for A and AAAA (`buildDnsResponse`,
   `ip_mux_upgrade.go:2344-2381`). At `bc3fec12` an answer of `0.0.0.0` or
   `::` counts as records (`parseDohWire`, `net_http_doh.go:2217-2283`; on
   `fix/default-resolvers-v2` it is a filtered response from a built-in
   endpoint and still counts as records from any other server), is
   recorded in the reverse index (`ip_mux_upgrade.go:1852-1854`) and is
   bound to the exit that carried the DoH flow (`net_http_doh.go:1181-1189`,
   `bindDnsResultToExit` `ip_mux_upgrade.go:2734-2756`). A REFUSED or
   SERVFAIL is a resolver failure, not an answer (`net_http_doh.go:2227-2234`).
8. On failure the mux sends no answer and the client retries
   (`ip_mux_upgrade.go:1822-1830`). A cached answer can be served up to five
   minutes stale while fresh resolution fails (`net_http_doh.go:44-56`). A cold
   prober re-proves the path with backoff from 2 s to 5 min
   (`ip_mux_upgrade.go:227-233`).
9. A hostname-form DoH url resolves its own host over remote plain DNS
   through the tunnel, even with remote DNS disabled (`net_http_doh.go:439-443,
   682-704, 1168-1173, 1208-1229`); the tun dial resolves through the same
   cache (`tun.go:1631-1648`). That is a plaintext lookup of the resolver's
   name, readable by the exit operator. The production defaults are ip
   literals and never use it.
10. DoH TLS uses the system roots; nothing pins certificates in production
    (`net_http_doh.go:33,359-361,397-406`). The client negotiates HTTP/2 by
    ALPN, offering `h2` ahead of `http/1.1` (`:407-418`), and sends RFC 8484
    GET only (`:2031`). Each dial path has its own TLS session cache, so a
    ticket from the host path is never redeemed through the tunnel
    (`:664-669`). DoH error strings embed the server url
    (`:2033,2055,2059,2063`), and the warm failure line logs one
    (`:856,871`).
11. A settings change swaps the tun's DoH cache, with an empty answer cache,
    and the fallback cache (`UpgradeMux.SetSettings`,
    `ip_mux_upgrade.go:2783-2796`; `tun.go:1056-1062`).

### 3.3 Fast DNS on connect (the DNS fallback)

- Off by default (product decision, 2026-10-02):
  `DnsUpgradeSettings.Fallback` is nil (`ip_mux_upgrade.go:99-104,178-180`),
  and the sdk's `EnableFallback` is false (sdk `device.go:458-463`).
- When on, the fallback is the host-side projection of the resolver: remote
  DoH urls become host-dialed DoH urls (sdk `device_local.go:8326-8350`). It
  races a query the tunnel has not answered within 1 s, or 250 ms while the
  tunnel is cold (`ip_mux_upgrade.go:166-177`).
- Records saved before the opt-in version load with the fallback off (sdk
  `local_state.go:596-626`).

### 3.4 Regional presets and fallbacks

Egress (user traffic):

- `regionalDnsServers` lists plain resolvers for cn, ru, ir, tr, kz and tm
  (`net_http_doh_regional.go:17-31`). `RegionalDnsResolverSettings` turns them
  into unencrypted remote DNS only (`:80-98`), for exits in a region where DoH
  to the defaults is known not to work.
- The sdk exposes `GetRegionalDnsServers`, `HasRegionalDnsRecommendation` and
  `GetRecommendedDnsResolverSettings` (sdk `device.go:479-516`). The server's
  quality probe uses the same recommendation per provider country
  (server `qualityprobe/providertunnel/regional_dns.go:15-25`).

Ingress (the network space's own names, which this plan does not touch):

- Bootstrap DoH presets exist only for cn (Alidns, DNSPod), checked on
  2026-10-04 (`net_http_doh_regional.go:49-59`). ir and ru have plain
  recommendations and no DoH presets.
- A user can name bootstrap DoH servers, ip-literal urls only, which go ahead
  of the defaults (`net_http_doh_control.go:1-21,146-178`; sdk
  `control_doh.go`, `control_doh_ui.go`). The sdk maps connect's error codes
  to localization ids (`control_doh.go:38-60`).
- The connection region picks the ingress strategy
  (`IngressDnsStrategyForCountry`, `net_http_doh_regional.go:143-167`). When
  every encrypted resolver fails, the space's own names, and only those, fall
  back to plain DNS (`net_http_doh_bootstrap.go:12-64`).

### 3.5 The sdk settings model

- `sdk.DnsResolverSettings` (`device.go:449-477`) mirrors the connect lists
  and adds `EnableFallback`. `GetDefaultDnsResolverSettings` returns the mux
  defaults (`:518-530`).
- Set and apply: `SetDnsResolverSettings` saves the catalog preference
  `dns-resolver-settings` (`device_local.go:8352-8357`), whose file is
  `.dns_resolver_settings` (`device_local_preference_catalog.go:38`).
  `applyDnsResolverSettingsWithLock` (`device_local.go:8362-8388`) builds the
  mux settings and the ignore list and applies them to the live mux.
- Conversions: `dnsResolverSettingsFromConnect` and `toConnect`
  (`device_local.go:8243-8288`). `cloneDnsResolverSettings`
  (`:8390-8397`) round-trips through connect and restores only
  `EnableFallback`. `dnsResolverSettingsWithLock` (`:8406-8413`) rebuilds the
  sdk value from the live mux settings. Both drop any sdk-only field (E3).
- The record codec carries `FallbackOptInVersion` and, on
  `fix/no-google-dns`, `DefaultServersVersion` (`local_state.go:596-626`,
  3.1). "the dns editors save every field" (`:597-598`).
- Remote control: `DnsResolverSettingsRpc` and its conversions
  (`device_rpc.go:8293-8355`), `DeviceRemote.SetDnsResolverSettings` and
  `GetDnsResolverSettings` (`:6009-6074`), `DeviceLocalRpc` (`:12108-12120`)
  and the sync apply (`:10602-10606`). The rpc is gob.
- Hosted devices refuse resolver settings (`device_local.go:8359-8365`,
  `device_rpc.go:12108-12110`).
- The device's socket resolver converts the same settings with `toConnect`
  and folds the local entries into the remote ones (`socketResolverSettings`,
  `socket.go:147-160`, applied at `device_local.go:2497`).
- The resolver endpoints feed the block-action ignore list
  (`device_local.go:8415-8472`). Ignored destinations skip block actions and
  the blocker (`ip_remote_multi_client.go:6637-6646,7611-7660`); the default
  resolver addresses are always ignored (`ip_block_action.go:873-896`).
- Per-server DoH scores persist keyed by url (`local_state.go:695-742`).
- Logout wipes `.dns_resolver_settings` and `.doh_server_scores`
  (`local_state.go:1207-1219`), which is intended until network-wide settings
  exist.
- The reliability-settings round trip fixed on 2026-11-08 (sdk `56d48331`,
  merged in `f16a0a5a`) is the precedent for E3: a struct that does not carry
  every field drops it on get, change, set.
  `reliability_settings_roundtrip_test.go` fills every connect field and
  requires it back. DNS settings have no such test, and the rpc mirror test
  (`device_rpc_mirror_test.go`) does not cover `DnsResolverSettingsRpc`.

### 3.6 The built-in blocker

- Default off (sdk `BLOCKER.md:10-13`). It is consulted ahead of resolution
  for every query type, and its blocked answers never enter the reverse index
  or the cache (`ip_mux_upgrade.go:1654-1680`).
- Its lists are limited by license: the AdGuard DNS filter, OISD and HaGeZi
  are GPL-3.0 and excluded (`BLOCKER.md:15-20`). A third-party resolver brings
  its own lists without URnetwork redistributing them.
- Per-user unblocking works through `BlockActionOverride` for the built-in
  blocker only.

### 3.7 Hosted proxies

- A proxy is provisioned with `dns_resolver_settings`, connect's JSON
  (server `model/network_client_proxy_model.go:209-215`).
  `hostedDnsResolverSettings` keeps the remote lists as provisioned and drops
  local endpoints that are not connect defaults (server
  `proxy/proxy_device.go:1517-1560`). `connect.DnsResolverSettings` is
  therefore a wire format: a new field must keep old readers working.

### 3.8 The apps

Read on main: apple `5465aca8`, android `0102ed06e`, windows `77b73df0`,
linux `80ea2e0`. Apple and android paths are relative to
`apple/app/network/` and `android/app/app/src/main/java/com/bringyour/network/`.

Common to all four apps:

- The traffic DNS editor, titled "Custom DNS", opens only from the connect
  drawer's "Custom DNS" card. Top to bottom it holds the connection region row
  (ingress, 3.4); a regional panel when the exit's country has a
  recommendation (the country is the selected provider's, so the exit's), or
  else a defaults panel with "Restore to most secure settings", which only
  fills the draft; four resolver toggles (remote and local DoH, remote and
  local plain DNS); Fast DNS on connect; "Suggested remote DNS servers" from
  `GetRegionalDnsServers`, every country listed; four editable lists (remote
  and local DoH urls, remote and local DNS servers, v4 and v6); and "Update",
  a compare-and-set against the settings the editor opened with.
- Custom entries are free-form and checked only in the app, weakly: apple
  requires https and a host
  (`Shared/Views/Stats/DnsSettingsView.swift:447-456`), android only
  `startsWith("https://")` (`ui/stats/DnsSettingsScreen.kt:447-449`), windows
  and linux https and a host (`StatsFormat.cpp:153-191`,
  `Formatters.cpp:106-120`). None checks the family; the sdk checks nothing.
  A user can already type a hostname DoH url, a NextDNS personal url for
  example. Its host is then looked up in plaintext through the tunnel (3.2
  step 9), and unless the user also removes the default servers the fan-out
  mixes it with unfiltered ones (3.2 step 5), so filtering is unreliable. No
  personal-ID field exists.
- The card summarizes four On/Off rows (DNS over HTTPS, unencrypted DNS,
  local DNS, Fast DNS on connect) and shows a pill when the settings differ,
  by whole-value equality, from the exit country's recommendation or else from
  the defaults, "The default safe settings are not applied" (apple
  `Main/Connect/ConnectStatsSections.swift:302-359`, android
  `ui/stats/ConnectStatsSections.kt:465-536`, windows
  `ConnectPage.cpp:3943-3985`, linux `ConnectPage.cpp:2797-2841`). No
  adblock provider or server name is shown anywhere.
- Apple, android and windows build a fresh `DnsResolverSettings` from their
  own draft model (apple `Shared/ViewModels/DnsSettingsStore.swift:72-89`,
  android `ui/stats/DnsSettingsViewModel.kt:197-210`, windows
  `StatsSheets.h:274-289`, `StatsSheets.cpp:1259-1310`), so a field the draft
  does not model is dropped on every save; today only the mask, which the sdk
  maps back to the default. Linux keeps the whole struct as its draft
  (`DnsSheet.hpp:77-78`) and compares field by field (`DnsSheet.cpp:53-65`).
- The built-in blocker is its own control outside the DNS screen.
- The bootstrap DoH editor (with "Use China resolvers") is under Account >
  Extenders and the login "Change Network API" sheet.

Per platform:

- Apple: `DnsSettingsView.swift` is a sheet. The app holds a `DeviceRemote`;
  the packet tunnel extension holds the `DeviceLocal`. An edit crosses the
  rpc and applies at once while connected, and the extension re-applies its
  network settings on a DNS change
  (`app/extension/PacketTunnelProvider.swift:1248-1259`); while disconnected
  the remote queues it. The app mirrors the read-back settings into its own
  `LocalState` record (`DnsSettingsStore.swift:224-301`) and seeds the next
  device from it (`Shared/ViewModels/DeviceManager.swift:1745-1756`).
  `NEDNSSettings` carries the plain stand-in servers with
  `matchDomains = [""]`; OS-level DoH and DoT settings are deliberately not
  used (`PacketTunnelProvider.swift:1824-1837,2082-2102`).
- Android: `DnsSettingsScreen.kt` is a full screen. `MainService` and the
  `DeviceLocal` run in the app process; `applyDnsResolverSettings` writes to
  the live device, or without one to `LocalState` (`DeviceManager.kt:311-337`),
  and applies at once (`MainService.kt:417-421`). Private DNS in strict mode
  bypasses URnetwork DNS; it is detected and shown as a notice while
  connected (`PrivateDnsMode.kt`, `ui/connect/ConnectActions.kt:628-660`).
- Windows (`windows/app/src/App/`): `DnsEditorSheet`
  (`StatsSheets.cpp:1257-1741`) is a dialog. The GUI holds a `DeviceRemote` to the service's
  `DeviceLocal`; DNS reads and writes go only through it, apply at once and
  queue while the rpc is down. The service gives the tunnel interface the
  sdk's `tunnelDnsAddresses`, and its firewall lets port 53 reach only those
  servers through the tunnel (`../Service/TunnelController.cpp:1153-1166`,
  `../Service/WfpPolicy.cpp:583-620`).
- Linux (`linux/app/src/`): `DnsSheet` is a modal window. The `DeviceLocal`
  runs in `urnetworkd` only while a tunnel runs. With no tunnel, reads and
  writes fall back to the GUI's own `LocalState` (`SdkHost.cpp:4658-4675`),
  and that copy is never applied to the daemon's device when it binds: the
  restore block applies the profile, provide mode, route-local and transport
  settings but not DNS (`SdkHost.cpp:3148-3180`).

The sdk's cgo headers are generated and committed in the sdk
(`sdk/cgo/Makefile:18-22`, `make generate`; `sdk/cgo/README.md:80-89`, which
also regenerates the packaging bindings). Windows and linux fetch them at
build (`windows/app/tools/fetch-deps.ps1`, `linux/app/scripts/fetch-deps.sh`).

Adjacent defects the UI work should fix in passing: android's `apply`
returns true when nothing was applied (`DnsSettingsViewModel.kt:211-218`);
apple's "IP address" placeholder is passed as a `String` and never localized
(`DnsSettingsView.swift:312,320,404,468,510`); linux loses DNS edits made
while disconnected (above); no app tests its url and address checks.

### 3.9 The web and the extension

- ur.io's app runs a wasm `DeviceRemote` that controls a hosted
  `DeviceLocal` on a proxy host (`mmm/ur.io/APP.md:20-25`,
  sdk `device_rpc_extension_js.go:46-72`). Hosted devices run no upgrade mux:
  "the proxy egresses DNS and HTTP unchanged (pass-through)" (server
  `proxy/proxy_device.go:1317-1319`), and they refuse resolver settings (3.5).
  The web DNS card (`mmm/ur.io/react/src/app/connect/ConnectStats.jsx:213-330`)
  is a read-only summary that in practice shows "DNS settings unavailable".
- A hosted device's resolver can be set only at connect, through
  `proxy_config.initial_device_state.dns_resolver_settings`; when none is
  sent the server fills in the exit country's regional recommendation
  (server `model/network_client_model.go:506-509,1278-1286`). Nothing sends one
  today, and the sdk's `ProxyDeviceState` has no DNS field (sdk
  `proxy_config.go:90-99`).
- The extension has no DNS settings. It is an HTTPS proxy, so names resolve
  on the proxy host through the device tun's DoH cache (`tun.go:1018-1050`).
- sdk/js maps the DNS methods of `DeviceRemote` by hand (sdk
  `js/device_remote.go:424-444`, conversions `:905-963`, TS types
  `js/src/types.ts:979-1002`).
- Help pages live in `mmm/ur.io/docs`, served at `https://ur.io/docs/<slug>`.
  The platform tours have a Custom DNS section (`TOUR-IOS.md:118-147`,
  `TOUR-ANDROID.md:178-195`, `TOUR-MACOS.md:199-215`,
  `TOUR-WINDOWS.md:243-249`, `TOUR-LINUX.md:143-165`); no page covers
  adblock providers, and the tours still call Fast DNS on connect "Local DNS
  fallback". The apps already hard-code one help link,
  `https://ur.io/docs/api`.

### 3.10 Localization

`localizations/keys/*.yaml` is the single source of every interface string
(`localizations/README.md`). One file per key, the file name is the key id
(snake_case), and each key lists the platforms that use it. `npm run gen`
writes the android `strings.xml`, apple `Localizable.xcstrings`, windows
`.resw` and linux `.po` files into the sibling repos; `npm run check` fails on
drift; the release build regenerates on every build and commits the output
(`build/all/run.sh:926-939,1266-1299`). The store has 28 languages: windows
and linux ship all 28, apple 27, android 19
(`localizations/gen/store.mjs:23-85`), and a live key needs all 28
(`:267-273`). The apps map an sdk error id to its
key (apple `Shared/ViewModels/ControlDohSettingsForm.swift:48-78`, android
`ui/account/ControlDohLogic.kt:18-23,59-70`), and android's
`LocalizedUiTextTest` fails on any literal shown on screen that is not
allowlisted.

## 4. Decisions

### A. The preset catalog

A1. Location: the sdk, as the research also recommends ("the catalog, ID
parsing, validation, verification and status logic belong there").
`sdk/dns_presets.go` holds the catalog and the expansion into resolver
settings, and is built for every target, the iOS packet tunnel extension
included, because the device derives the resolver lists from the stored
selection when it applies them (A6). `sdk/dns_presets_ui.go`
(`//go:build !ios_extension`) holds what the screens call, as
`control_doh_ui.go` and `transport_settings_view.go` do. connect gets
generic mechanisms only (B). The catalog is built into the sdk only and
changes with app releases; there is no server-delivered copy and no withdraw
list (O13, A7).

A2. Data model, internal to the sdk:

```go
// A provider whose resolvers the catalog offers. Names are proper nouns and
// are not localized.
type dnsPresetProvider struct {
	providerId   string // stable: "adguard"
	name         string // "AdGuard DNS"
	// the name its presets' names begin with, where the name is longer than
	// they need: "AdGuard" (decision 24); empty uses name
	shortName    string
	operator     string // "AdGuard Software Ltd"
	jurisdiction string // "Cyprus"
	helpUrl      string
	privacyUrl   string
	termsUrl     string
}

// One selectable resolver.
type dnsPreset struct {
	presetId   string // stable and stored on devices: "adguard"
	providerId string
	// the localization key of the preset's own short, descriptive name, with
	// its adblock provider's short name as {provider}:
	// dns_preset_name_ad_blocking, "AdGuard Ad Blocking" (decision 24, A3)
	nameKey string
	// the outcome group: adblock, security, family, unfiltered or personal
	group string
	// whether it blocks ads; false shows "not ads" in the picker
	blocksAds bool
	// ip-literal presets: one url per address, the v4 and v6 lists in
	// operator pair order. hostname presets: the one url in both lists.
	dohUrlsIpv4 []string
	dohUrlsIpv6 []string
	// the addresses a hostname url's host is dialed at (B1); empty for
	// ip-literal presets
	dohPinIpv4s []string
	dohPinIpv6s []string
	// how a block is answered: "null" (0.0.0.0, ::) or "nxdomain"
	blockAnswer string
	// a name the preset is known to block, for verification (C6); "" for none
	canaryName string
	// whether a failing preset falls back to the default resolver (C1, O2)
	failover bool
	// nil unless the preset takes a personal id (D)
	personalId *dnsPresetPersonalId
	// the date the endpoints were last checked live (F3), as the regional
	// presets record theirs (net_http_doh_regional.go:49-53)
	checked string
}

type dnsPresetPersonalId struct {
	// the DoH url with one "{id}" path segment
	dohUrlTemplate string
	// anchored patterns an id must match after normalization (D4)
	idPatterns []*regexp.Regexp
	lowercase  bool
	// pasted forms with one capture group for the id: dashboard DoH urls and
	// DoT hostnames (D3)
	pastePatterns []*regexp.Regexp
	example       string // the provider's own example, e.g. "abc123"
	// how the provider answers an unknown or malformed id (C6, D5)
	unknownIdAnswer string // "unfiltered", "refused" or "unfiltered-or-servfail"
	idHelpUrl       string
	// free-tier note shown under the field, a localization key or ""
	quotaNoteKey string
}
```

Reserved ids, not catalog entries: `default` (URnetwork's built-in lists,
derived from `connect.DefaultUpgradeMuxSettings` when applied, so a device
on the default follows later default changes, E4); custom, stored as an
empty `PresetId` (the explicit lists of the advanced editor, the behavior of
every record today, which every app keeps offering, O1); and `regional-<cc>`
(the existing regional recommendation as a pick, O6, derived from
`connect.RegionalDnsResolverSettings`, labeled unencrypted, offered only
while the exit's country has a recommendation and never selected
automatically).

A3. The catalog. Values are verbatim from the adblock providers' pages
(AdGuard's [knowledge base](https://adguard-dns.io/kb/general/dns-providers/), Control D's
[free-DNS docs](https://docs.controld.com/docs/free-dns),
[Quad9](https://quad9.net/service/service-addresses-and-features),
Cloudflare's [setup](https://developers.cloudflare.com/1.1.1.1/setup/) and
[addresses](https://developers.cloudflare.com/1.1.1.1/ip-addresses/) pages,
and DNS4EU's [public service](https://www.joindns4.eu/for-public) page);
the Control D bootstrap pair is the one Control D's own client uses
([ctrld config.go](https://github.com/Control-D-Inc/ctrld/blob/37c33315591632c5f08df8062d1c77e07b3a465f/config.go#L52-L63)).

One-tap presets, in display order (O9): by group in the screen's order, then
by adblock provider name. Every adblock provider is approved (owner,
2026-11-09, O12), so no entry waits for an adblock provider's answer; each
still passes F3 before it merges. The No-filtering picks are in the catalog
proper (O1); `cloudflare` answers 8236's "use Cloudflare" directly.

| Id | Group | Name: picker copy | DoH urls | Pins | Block answer | Canary | On failure | Gate |
|---|---|---|---|---|---|---|---|---|
| `adguard` | adblock | AdGuard Ad Blocking: blocks ads, trackers, phishing | `https://dns.adguard-dns.com/dns-query` | `94.140.14.14`, `94.140.15.15`, `2a10:50c0::ad1:ff`, `2a10:50c0::ad2:ff` | `0.0.0.0` | `doubleclick.net` | fall back | approved (owner, 2026-11-09); F3 |
| `controld-ads` | adblock | Control D Ad Blocking: blocks ads, trackers, malware | `https://freedns.controld.com/p2` | `76.76.2.11`, `2606:1a40::11` | `0.0.0.0` | `doubleclick.net` | fall back | approved (owner, 2026-11-09); F3 |
| `dns4eu-noads` | adblock | DNS4EU Ad Blocking: blocks ads, trackers, malware, phishing | `https://noads.joindns4.eu/dns-query` | `86.54.11.13`, `86.54.11.213`, `2a13:1001::86:54:11:13`, `2a13:1001::86:54:11:213` | `0.0.0.0` | `doubleclick.net` | fall back | approved (owner, 2026-11-09); F3 |
| `cloudflare-security` | security | Cloudflare Malware Blocking: blocks malware, not ads | `https://1.1.1.2/dns-query`, `https://1.0.0.2/dns-query`; `https://[2606:4700:4700::1112]/dns-query`, `https://[2606:4700:4700::1002]/dns-query` | none | `0.0.0.0`, EDE 17 | none | fail closed | approved (owner, 2026-11-09); F3 |
| `dns4eu-protective` | security | DNS4EU Malware Blocking: blocks malware and phishing, not ads | `https://protective.joindns4.eu/dns-query` | `86.54.11.1`, `86.54.11.201`, `2a13:1001::86:54:11:1`, `2a13:1001::86:54:11:201` | `0.0.0.0` expected (section 10) | none | fail closed | approved (owner, 2026-11-09); F3 |
| `quad9` | security | Quad9 Malware Blocking: blocks malware and phishing, not ads | `https://9.9.9.9/dns-query`, `https://149.112.112.112/dns-query`; `https://[2620:fe::fe]/dns-query`, `https://[2620:fe::9]/dns-query` | none | NXDOMAIN, EDE 17 | `examplemalwaredomain.com` | fail closed | approved (owner, 2026-11-09); F3 |
| `adguard-family` | family | AdGuard Family: ads, trackers, adult sites, Safe Search | `https://family.adguard-dns.com/dns-query` | `94.140.14.15`, `94.140.15.16`, `2a10:50c0::bad1:ff`, `2a10:50c0::bad2:ff` | `0.0.0.0` | `doubleclick.net` | fail closed | approved (owner, 2026-11-09); F3 |
| `cloudflare-family` | family | Cloudflare Family: blocks malware and adult sites, not ads | `https://1.1.1.3/dns-query`, `https://1.0.0.3/dns-query`; `https://[2606:4700:4700::1113]/dns-query`, `https://[2606:4700:4700::1003]/dns-query` | none | `0.0.0.0`, EDE 17 | none | fail closed | approved (owner, 2026-11-09); F3 |
| `controld-family` | family | Control D Family: ads, trackers, malware, adult, drugs | `https://freedns.controld.com/family` | `76.76.2.11`, `2606:1a40::11` | `0.0.0.0` | `doubleclick.net` | fail closed | approved (owner, 2026-11-09); F3 |
| `dns4eu-child` | family | DNS4EU Child: blocks malware, adult sites, gambling, drugs, violence, not ads | `https://child.joindns4.eu/dns-query` | `86.54.11.12`, `86.54.11.212`, `2a13:1001::86:54:11:12`, `2a13:1001::86:54:11:212` | `0.0.0.0` expected (section 10) | none | fail closed | approved (owner, 2026-11-09); F3 |
| `dns4eu-child-noads` | family | DNS4EU Child, no ads: ads, trackers, malware, adult sites, gambling, drugs, violence | `https://child-noads.joindns4.eu/dns-query` | `86.54.11.11`, `86.54.11.211`, `2a13:1001::86:54:11:11`, `2a13:1001::86:54:11:211` | `0.0.0.0` expected (section 10) | `doubleclick.net` | fail closed | approved (owner, 2026-11-09); F3 |
| `adguard-unfiltered` | unfiltered | AdGuard No Filtering: no filtering | `https://unfiltered.adguard-dns.com/dns-query` | `94.140.14.140`, `94.140.14.141`, `2a10:50c0::1:ff`, `2a10:50c0::2:ff` | none | none | fall back | approved (owner, 2026-11-09); F3 |
| `cloudflare` | unfiltered | Cloudflare No Filtering: no filtering | `https://1.1.1.1/dns-query`, `https://1.0.0.1/dns-query`; `https://[2606:4700:4700::1111]/dns-query`, `https://[2606:4700:4700::1001]/dns-query` | none | none | none | fall back | approved (owner, 2026-11-09); F3 |
| `controld-unfiltered` | unfiltered | Control D No Filtering: no filtering | `https://freedns.controld.com/p0` | `76.76.2.11`, `2606:1a40::11` | none | none | fall back | approved (owner, 2026-11-09); F3 |
| `dns4eu-unfiltered` | unfiltered | DNS4EU No Filtering: no protective filtering; court-ordered blocks still apply | `https://unfiltered.joindns4.eu/dns-query` | `86.54.11.100`, `86.54.11.200`, `2a13:1001::86:54:11:100`, `2a13:1001::86:54:11:200` | court orders only; the form is unverified (section 10) | none | fall back | approved (owner, 2026-11-09); F3 |

What the presets were seen to do, probed 2026-11-08: AdGuard Default,
AdGuard Family, Control D p2 and DNS4EU's ad-blocking variant answered
`doubleclick.net` with `0.0.0.0`, while DNS4EU's Protective and Unfiltered
variants returned a real address; Quad9 answered `examplemalwaredomain.com`
with NXDOMAIN and EDE 17, and a genuinely nonexistent name without EDE; none
of the free tiers of Quad9 and Cloudflare blocked any of eight ad and
tracker domains, which is why their copy says "not ads"; Cloudflare 1.1.1.3
also forced Google SafeSearch and Bing strict mode by CNAME, which
Cloudflare does not document. DNS4EU's two child variants and Protective's
malware blocks were not probed, so their block answers are expected, not
seen (section 10). AdGuard accepted HTTP/2 and HTTP/1.1, Quad9 has required
HTTP/2 since 2025-12-15 (HTTP 505 on 1.1,
[FAQ](https://docs.quad9.net/FAQs/)), and DNS4EU refused a TLS client that
offered no ALPN, with a `no_application_protocol` alert, and answered over
HTTP/2. The research reports that every recommended preset answered RFC 8484
GET, and its DNS4EU probes were GET too. connect offers ALPN `h2` ahead of
`http/1.1` on every DoH client and sends GET only (3.2 step 10, section 7),
so all of them fit.

DNS4EU's DoH stamps carry no address, so its presets use hostname urls with
pins, the addresses its public-service page lists for each variant.
Court-ordered blocks apply to every DNS4EU variant ("Court-ordered domain
blocks apply to every variant, including this one",
[DNS4EU](https://www.joindns4.eu/for-public)): its compliance page lists
French ARCOM and Paris court orders and Russian state-media domains
([legal](https://www.joindns4.eu/legal-information-and-compliance)), which is
why its No-filtering copy says "no protective filtering". Its 14 resolvers
sit in 14 EU member states, and its policy aims the Public Service primarily
at individuals in the EU (H3).

Each pick shows a short, descriptive name of its own (decision 24), as the
name column starts: its adblock provider's short name, then what it does by
its group, "Ad Blocking", "Malware Blocking", "Family", "No Filtering" or
"Personal". DNS4EU's two family presets keep "DNS4EU Child"
(`dns4eu-child`) and "DNS4EU Child, no ads" (`dns4eu-child-noads`)
(decision 20), since two DNS4EU presets share Family. The short name is the
adblock provider's name, except "AdGuard" for AdGuard DNS, so the picks read
"AdGuard Family", not "AdGuard DNS Family". A name is a localization key
(`dns_preset_name_ad_blocking`, `_malware_blocking`, `_family`, `_child`,
`_child_ads`, `_no_filtering`, `_personal`; G1) with the short name as
`{provider}`: the purpose is translated, the adblock provider's name never
is (G2), and every language puts the name first, so a group stays in
alphabetical order. The sdk hands it as `NameKey` and `NameProvider` on the
preset, the summary and the status, and `DroppedNameKey` and
`DroppedNameProvider` for the dropped-preset notice (A5). The apps render
it on the row and pass it, rendered, as `{provider}` to the lines that name
the selected preset: the summary row, which is "DNS: <name>" for every
preset (`dns_status_provider`, the status line's text, since the name says
what the preset does), the status line, the blocked-lookups count, the
Fast DNS note, Android's Private DNS line and the dropped-preset notice.
The lines that name the adblock provider keep its name (`ProviderName`):
the disclosure, the personal ID field and the verification line with its
link. The `DnsPresetProvider` keeps "AdGuard DNS" and "DNS4EU". Catalog
tests require every preset to have a name of its group's purpose, no two
presets of the catalog to show the same name, DNS4EU's two child names to
say what they do about ads, and a short name to shorten its adblock
provider's name.

None of the filtering presets above names a built-in endpoint (3.1), so the
policy-filter guard leaves their blocks alone (B4). The `quad9` preset's
`9.9.9.9` became an ordinary endpoint when the defaults moved to Quad9's
unfiltered `9.9.9.10`.

Personal presets (D), the "Use my account" group, in display order, named
"AdGuard Personal", "Control D Personal" and "NextDNS Personal":

| Id | DoH form stored | Pins | Unknown or wrong id, probed 2026-11-08 | Free tier |
|---|---|---|---|---|
| `adguard-personal` | `https://d.adguard-dns.com/dns-query/<id>` | `94.140.14.49`, `2a10:50c0::ded:ff` | a well-formed unknown id answers unfiltered; a malformed one gets SERVFAIL | Starter: 300,000 requests a month; what happens at the cap is undocumented ([plans](https://adguard-dns.io/en/license.html)) |
| `controld-personal` | `https://dns.controld.com/<id>` | `76.76.2.22`, `2606:1a40::22` | REFUSED to every query | none: a 14-day trial, then from $3 a month |
| `nextdns-personal` | `https://dns.nextdns.io/<id>` | `45.90.28.0`, `45.90.30.0`, `2a07:a8c0::`, `2a07:a8c1::` | malformed ids (`zzzzzz`, `0x0x0x`) and no id: HTTP 200 with unfiltered answers; a well-formed unknown id was not probed, and the research expects the same | 300,000 queries a month, then "like a classic non-blocking DNS service" ([pricing](https://nextdns.io/pricing)) |

The NextDNS pins are the official CLI's bootstrap
([run.go](https://github.com/nextdns/nextdns/blob/a7e835115da831b0267c98dfd0be4de0175d9ad9/run.go#L605-L626));
the Control D custom pair is ctrld's
([config.go](https://github.com/Control-D-Inc/ctrld/blob/37c33315591632c5f08df8062d1c77e07b3a465f/config.go#L58-L90));
the AdGuard pair is what `d.adguard-dns.com` resolved to, and AdGuard's
router guide also lists `94.140.14.59` and `2a10:50c0::dad:ff` for private
servers ([KB](https://adguard-dns.io/kb/private-dns/connect-devices/routers/universal/)),
which F3 checks before they become pins.

Left out, with reasons (the research's, and for RethinkDNS the owner's):

| Adblock provider | Why not |
|---|---|
| CleanBrowsing | free tier "for parents, not organizations", a "Throttled Service", no selling access; Family blocks "proxy and VPN domains" ([terms](https://cleanbrowsing.org/terms/), [filters](https://cleanbrowsing.org/filters/)) |
| OpenDNS FamilyShield | returned block pages for `nordvpn.com`, `protonvpn.com` and `hidemyass.com`; free terms labeled "expired"; withdrawn from France and Portugal ([terms](https://umbrella.cisco.com/terms/)) |
| NextDNS without an id | filters nothing (probed) |
| Mullvad DNS | shut down 2026-11-02 ([announcement](https://mullvad.net/en/blog/shutting-down-our-public-encrypted-dns-servers-and-sponsoring-quad9-instead)); nothing in connect, sdk or the apps references it |
| RethinkDNS | its non-filtering resolver, `sky.rethinkdns.com`, forwards to Cloudflare's resolver and then Google's ([env.js](https://github.com/serverless-dns/serverless-dns/blob/b0b7fe52ecc053562d1fc425f9c5c6f984d37e11/src/core/env.js#L139-L154)), and 8236 asked to avoid Google; dropped by the owner (2026-11-09, O14), its blocklist stamps with it. Manual entry under Custom still works |
| LibreDNS, dnsforge.de, BlahDNS and other community resolvers | single-operator, non-anycast hobby servers (dnsforge caps 100 queries per 10 s, BlahDNS asks for no large traffic); custom url only |
| Quad9 with ECS (`9.9.9.11`) | it forwards a client subnet, which behind URnetwork is the exit provider's home network |

A4. Catalog rules, enforced by sdk tests (6.1):

- ids are unique, lower case, and never reused; a removed preset's id stays
  reserved (A7).
- Each url is https with no user info, query or fragment, and a path. A
  hostname url has pins of both families; an ip-literal url has none, and
  its v4 and v6 lists are in operator pair order, so `familySiblings` pairs
  them by index.
- Pins parse as addresses of their family.
- A personal template has exactly one `{id}` filling one whole path segment,
  and every id pattern is anchored and admits only `[A-Za-z0-9-]`.
- No preset is `default`, preselected or marked as recommended (H1).
- Groups are consistent: `adblock` and `unfiltered` presets fail over, and
  `family`, `security` and `personal` ones never do (O2); a preset whose
  `blocksAds` is false never sits in `adblock`.
- Every entry has a `checked` date (F3).

A5. The surface the apps call (gomobile, cgo and JS), in
`dns_presets_ui.go`:

```go
type DnsPresetProvider struct {
	ProviderId   string
	Name         string
	Operator     string
	Jurisdiction string
	HelpUrl      string
	PrivacyUrl   string
	TermsUrl     string
	Presets      *DnsPresetList
}

type DnsPreset struct {
	PresetId   string
	ProviderId string
	// the preset's own name, which the apps render: its localization key and
	// the adblock provider's short name (decision 24, A3); the summary and
	// the status carry it too, and the summary the dropped preset's
	NameKey      string
	NameProvider string
	// the adblock provider's name, for the lines that name it (A3)
	ProviderName string
	Group      string // adblock, security, family, unfiltered, personal
	BlocksAds  bool
	// whether a failing preset falls back to URnetwork's resolvers
	Failover           bool
	PersonalIdRequired bool
	PersonalIdExample  string
	PersonalIdHelpUrl  string
	QuotaNoteKey       string
}

// The providers in display order (O9), each with its presets.
func GetDnsPresetProviders() *DnsPresetProviderList

// nil for an unknown id.
func GetDnsPreset(presetId string) *DnsPreset

// The id from what a user typed or pasted (D3): trimmed, the provider's case
// rule applied, taken out of a dashboard url or DoT hostname. Input it
// cannot read is returned trimmed, for the validator to reject.
func NormalizeDnsPresetPersonalId(presetId string, input string) string

// The error id of a personal id for a preset, or empty when it is valid.
func ValidateDnsPresetPersonalId(presetId string, personalId string) (errorId string)

// base with the preset selected: PresetId and PresetPersonalId set, the lists
// derived, EnableFallback and the mask kept. nil when the preset is unknown
// or the id does not validate.
func DnsResolverSettingsWithPreset(base *DnsResolverSettings, presetId string, personalId string) *DnsResolverSettings

// base with Fast DNS on connect turned on or off; the selection is kept.
func DnsResolverSettingsWithFallback(base *DnsResolverSettings, enableFallback bool) *DnsResolverSettings

// base as custom settings for the advanced editor to start from: the lists
// in force, PresetId and PresetPersonalId cleared, every other field kept.
func DnsResolverSettingsAsCustom(base *DnsResolverSettings) *DnsResolverSettings

// A deep copy of every field.
func CopyDnsResolverSettings(settings *DnsResolverSettings) *DnsResolverSettings

// What the summary row shows, given the exit's country: the preset, the
// group, whether the selection is custom or unencrypted, the notice of a
// dropped preset (A7), which pill to show, if any (F2), and whether the
// screen shows the blocked-lookups count (I3).
func DescribeDnsResolverSettings(settings *DnsResolverSettings, exitCountryCode string) *DnsSelectionSummary

// Hides every preset that blocks ads, for the Play-only catalog if Play
// review objects (O15, H4). The android play flavor calls it at startup; a
// stored selection it hides is handled as a dropped preset (A7).
func SetDnsPresetsHideAdBlocking(hide bool)
```

The editors never build a `DnsResolverSettings` from scratch: they start
from one of these helpers, so a field an app does not model survives its save
(3.8, E3).

Error ids, which are localization keys as the bootstrap DoH ones are
(`control_doh.go:38-46`): `dns_preset_error_unknown`,
`dns_preset_error_personal_id_required`,
`dns_preset_error_personal_id_invalid` (with each adblock provider's own
format message, D4), `dns_preset_error_personal_id_other_provider` (a pasted
url or hostname of another adblock provider),
`dns_preset_error_personal_id_not_accepted`.

A6. Expansion: what a selected preset becomes in connect terms. The device
derives it each time it applies the settings. One conversion does it:
`DnsResolverSettings.toConnect()` (`device_local.go:8266-8288`) derives a
preset's lists and attaches the catalog pins. The mux settings
(`upgradeMuxSettingsWithDnsResolverSettings`, `:8303-8324`) and the socket
resolver (`socket.go:147-160`) already go through it. The host fallback
projection (`hostFallbackDnsResolverSettings`, `:8330-8350`) builds a new
struct from the result: while a preset is selected it is URnetwork's default
resolver instead, never the preset (C7), and for custom settings it must
copy the pins. The result:

- `EnableRemoteDoh` true; `EnableLocalDoh`, `EnableRemoteDns` and
  `EnableLocalDns` false. This makes the chain DoH-only, so a filter's
  NXDOMAIN can never be answered by a later step (3.2 step 6).
- `RemoteDohUrlsIpv4` and `RemoteDohUrlsIpv6` hold exactly the preset's urls
  (the personal id filled in). The lists are exclusive: no other server is
  added, because a hedge to an unfiltered server would return the names the
  preset blocks (3.2 step 5). A hostname url sits in both lists and is
  queried once (`dualStackList` drops repeats); it still works on a v4-only
  tun (`tun.go:1039-1049` drops only the v6 list) and with `IpVersion` 6.
- `DohServerHostAddrs` maps each hostname url's host to its pins (B1).
- `RemoteDnsIpv4` and `RemoteDnsIpv6` empty. With pins nothing needs them,
  and empty lists make a missing pin fail closed instead of falling back to a
  plaintext lookup (B3).
- `DnsUpgradeMaskAddress` and `EnableFallback` as the user has them. With
  `EnableFallback`, the mux settings' `Fallback` is URnetwork's default
  resolver over the host egress, with `FallbackUntilTunnelReady` set (C7).
- The ignore list (`dnsIgnoreHostValues`, `device_local.go:8427-8472`) also
  gets the pins, so block actions and the built-in blocker never capture
  resolver traffic.
- For a preset with `failover`, the mux settings also carry the default
  resolver as `Failover` (C2).

Pins follow the host, not the selection: whenever the device applies
resolver settings, preset or custom, every DoH url whose host the catalog
knows gets that host's pins. A copy of preset settings saved as custom (an
editor that rebuilds the struct, 3.8) therefore keeps working with no
plaintext lookup, and a user who types a known adblock provider's url into
the custom lists gets the same path. A hostname the catalog does not know
keeps today's lookup through the tunnel (3.2 step 9): the custom editor's
existing behavior, not a newly offered resolver.

The stored record keeps the derived lists as well as the selection. An older
sdk ignores the selection and uses the lists (E5).

A7. Catalog changes reach devices only with an app release (O13), because
the device derives the lists from the current catalog at every apply. When a
release drops a preset, the catalog keeps its id, with the name it showed
(its name key and short name, A3) and its adblock provider's name, as
reserved (A4). A device whose stored selection names a dropped id switches
to URnetwork's DNS: it applies `default` in its place, and the DNS screen
and the summary show a notice that names the preset as it showed, such as
"AdGuard Family is no longer available", and says its filtering is off,
with "Choose another preset" and "Enter it yourself under Custom" (O8). This holds for every group, family and personal included, and
never happens silently; dismissing the notice saves `default`.

### B. The resolution path (connect)

B1. Pinned addresses for hostname DoH servers. New field on
`DnsResolverSettings`:

```go
// The addresses a hostname-form DoH server is dialed at, by host (lower
// case, as in the url). A listed host is never looked up: not over remote
// plain dns, not with the host's resolver. TLS still verifies the server
// against the host name. Nil pins nothing.
DohServerHostAddrs map[string][]string `json:"doh_server_host_addrs,omitempty"`
```

- `DohCache.resolve` answers a pinned host from its pins, for the query's
  family, as authoritative, before the remote plain-DNS step
  (`net_http_doh.go:1168-1173,1208`). A pinned host with no pin of the family
  answers an authoritative empty result, so the dial uses the other family
  and never falls through. `DohServerResolvedCallback` still fires, so the
  reverse index names the pins (`:1276-1286`).
- The tun path needs no change: its dial resolves through the same cache
  (`tun.go:1631-1648`) and races the pins with the shared happy-eyeballs dial.
- The host path does: the local DoH client dials with the host's net dialer
  (`net_http_doh.go:669`), which would use the OS resolver. A wrapper dials a
  pinned host's pins with `dialAddrsRace` and passes every other address
  through.
- `builtInDohServerSettings` (`net_http_doh_control.go:185-207`) clears the
  map, since pins belong to custom servers. Copies that clone the lists
  (`internalDohSettings`, `net_http_internal_doh.go:73-102`, and the tun's
  copy) clone the map.

Dialing a pinned address, sending the hostname as SNI and verifying the
certificate against the hostname is how ctrld, the NextDNS CLI and Apple's
encrypted-DNS `ServerAddresses` work
([Apple payload](https://github.com/apple/device-management/blob/release/mdm/profiles/com.apple.dnsSettings.managed.yaml)).
It also keeps a preset alive through an outage of the adblock provider's own
zone, as on 2026-05-08, when `quad9.net` failed for under an hour and new
connections by name broke while `9.9.9.9` kept working
([Quad9](https://quad9.net/news/blog/analysis-of-outage-8-may-2026/)).

B2. Through the tunnel. A preset is queried by the mux's tun, the remote DoH
path, exactly as the defaults are (3.2 step 4). The resolver sees the exit
provider's address, answers match the exit's location (the 2026-10-02
rationale for tunnel-only DNS), and the local network sees only tunnel
traffic. A preset never takes the host-network path: Fast DNS on connect,
when the user turned it on, uses URnetwork's default DNS for the connect
window (C7).

B3. No plaintext. Preset settings have remote plain DNS off with empty
lists, and pins for every hostname. The resolver's name is never looked up
in plaintext on the tun path and never through the OS resolver on the host
path; when every pin fails, the request fails. The research notes that
presets could ship on today's client by letting hostnames resolve through
the tunnel's plain DNS until pins land; that is the plaintext lookup the 8236
safety constraint rules out, and it shows the exit operator which adblock
provider the user chose, so pins land first (section 7).

B4. Sink answers. AdGuard, Control D, Cloudflare and DNS4EU answer a
blocked name with `0.0.0.0`; Quad9 answers NXDOMAIN with EDE 17 (A3). The
mux delivers a `0.0.0.0` or `::` answer to the client but does not record it
in the reverse index or bind it to an exit: skip `reverse.record` in the
reply (`ip_mux_upgrade.go:1852-1854`) and the `DohResultCallback` in
`DohCache.resolve` (`net_http_doh.go:1181-1189`). This matches the built-in
blocker's rule (`ip_mux_upgrade.go:1658-1660`); without it every blocked name
lands on one reverse-index entry for `0.0.0.0`. The answer is still cached
for its TTL; a preset change swaps the cache (3.2 step 11).

Presets are outside the policy-filter guard (decided 2026-11-08 in the
review of `fix/default-resolvers-v2`). The guard (3.1) covers only the
built-in endpoints, so a preset's `0.0.0.0` sink answer and Quad9's
NXDOMAIN with EDE 17 reach the client as the answer and are cached as
usual; a preset needs no flag for it, and neither does a user's own
filtering list. The guard follows the endpoint, not the list: a preset url
that is also a built-in endpoint keeps the guard. No filtering preset in A3
names one. Since the defaults moved to Quad9's unfiltered `9.9.9.10`, the
`quad9` preset's `9.9.9.9` and `[2620:fe::fe]` are outside the guard and
deliver its blocks, as its secondaries do, which settles the choice this
paragraph used to leave to the owner. The unfiltered `cloudflare` variant
(O1) names built-in endpoints and keeps the guard, which takes nothing from
a resolver that does not filter.

A REFUSED or SERVFAIL is a resolver failure to the mux (3.2 step 7). No
one-tap preset uses them to block. A personal configuration can: AdGuard
private servers let the user pick "zero IP address" (the default), NXDOMAIN,
REFUSED or a custom IP ([KB](https://adguard-dns.io/kb/private-dns/overview/)),
and a Control D custom resolver answers REFUSED for an unknown id. The help
text names the responses that work, and REFUSED stays a failure (O11).

HTTPS and SVCB queries are forwarded opaquely (3.2 step 3), so an adblock
provider that blocks a name's A record but returns address hints in its
HTTPS record would let browsers connect anyway. The research did not test
this; F3 checks it for every preset before it ships.

B5. IPv6. Pins and urls carry both families. The tun dial races the pins; a
v4-only tun resolves only A (`tun.go:1632-1636`). No IPv6 endpoint of any
preset was tested live by the research (its probe host had no IPv6 route);
F3 does.

B6. Ip-literal urls where the certificate allows. Ip-literal urls need no
bootstrap and send no SNI. They are proven for `https://1.1.1.2/dns-query`
and `https://1.1.1.3/dns-query` (probed), for `9.9.9.9`, `[2620:fe::fe]`,
`1.1.1.1` and `[2606:4700:4700::1111]`, which the defaults used before
`fix/default-resolvers-v2`, and for the v4 defaults it adds. Probed
2026-11-09 from a host without an IPv6 route: `9.9.9.10` presented Quad9's
certificate, whose ip SANs include `9.9.9.10` and `2620:fe::10`, verified
for its ip and answered over HTTP/2, and connect's own DoH client resolved
through it; `1.0.0.1` presented Cloudflare's, whose ip SANs include
`1.0.0.1` and `2606:4700:4700::1001`, verified for its ip and answered over
HTTP/2. The v6 literals `[2620:fe::10]` and `[2606:4700:4700::1001]` carry
the SANs but were not reached. DNS.SB's `185.222.222.222` and `45.11.45.11`,
defaults since decision 19, presented a Let's Encrypt certificate whose ip
SANs include both and `2a09::` and `2a11::`, verified for their ips and
answered over HTTP/2 (probed 2026-11-08, also from a host without an IPv6
route, so `[2a09::]` and `[2a11::]` were not reached). The other Cloudflare
secondaries (`1.0.0.2`, `1.0.0.3`), Quad9's `149.112.112.112` and
`[2620:fe::9]`, and the other IPv6 literals pass F3 before they ship; a
literal that fails falls back to the adblock provider's hostname
(`cloudflare-dns.com`,
`security.cloudflare-dns.com`, `family.cloudflare-dns.com`,
`dns.quad9.net`) with the same addresses as pins. AdGuard, Control D and
DNS4EU were not tested by ip literal and use hostname urls with pins.

B7. Logs carry a DoH url as scheme and host only. Error strings that include
the url (`net_http_doh.go:2033,2055,2059,2063`) and the warm failure line
(`:856,871`) go through one helper, so a personal id in a path never reaches
a log or a feedback upload. Dial observation lines already log only
host:port (`net_http_doh_observation.go:258-266`), which for a path id holds
no secret.

B8. POST, later. The client sends GET only (`net_http_doh.go:2031`), and
the research reports that every recommended preset answered GET. BlahDNS accepts only POST since 2026-10-14, and
LibreDNS and dnsforge.de were probed only with POST, so the custom editor
needs a per-url POST option before it can promise those servers. It is not
needed for any preset and is outside this plan's first release.

B9. The blocked-lookups count (O16). The mux counts each client query it
answers with a recognizable block from a resolver: a `0.0.0.0` or `::`
sink, which B4 already tells apart from an address in the reply that skips
the reverse index, and an NXDOMAIN carrying extended DNS error 15 (Blocked),
16 (Censored) or 17 (Filtered), read as the guard reads its policy codes
(`dnsResponsePolicyEde`, `net_http_doh.go:2393` on `829a5a54`) but with the
owner's narrower set, so 4 and 18 do not count. The parsed result and its
cache entry carry whether a miss came with one of those codes, so a cached
block counts again when it answers another query, and an opaque reply (3.2
step 3) counts by the same rule. Not counted: an NXDOMAIN without one of
those codes (a personal configuration that blocks with plain NXDOMAIN, or a
name that does not exist), a REFUSED (a failure, C10), the built-in
blocker's answers (URnetwork's own, 3.6), and anything from a built-in
endpoint, whose filtered answers never win (3.1). The figure is therefore a
floor. New `UpgradeMux.DnsBlockedCount()` returns the count and
`ResetDnsBlockedCount()` zeroes it; the mux keeps no names with it and never
logs it. The sdk exposes it (I3).

### C. Failure behavior

C1. Split by purpose, per preset (the catalog's `failover`), as the owner
decided (O2):

- Ad-blocking and no-filtering presets fall back to URnetwork's default DNS
  while they fail from the current exit, with the status "Ad blocking
  paused: <Provider> isn't answering" (C8), and return to the preset on
  their own as soon as it answers again.
- Family, malware (`security`) and personal presets fail closed (C3), with
  a status line and a one-tap "Use URnetwork DNS", because a silent fallback
  would defeat their purpose (Apple's encrypted-DNS payload defaults
  `AllowFailover` to false for the same reason). Personal presets fail
  closed because the app cannot know whether the user's configuration is a
  parental one.

The split suits URnetwork's exits, residential connections in many
countries where per-exit failures are certain: AdGuard's DoH is blocked in
China, Control D's `freedns` is blocked there too, and Quad9 leaves France
on 2026-11-24 (C9). Failing every preset closed, the rejected alternative,
would turn an ad blocker that cannot be reached from an exit into "the VPN
doesn't work"; the fallback turns it into ads for a while, with a notice.
Both groups keep the safety constraint: the fallback is the built-in DoH
list through the tunnel, never plaintext and never the host network, and
never silent.

C2. The fallback mechanism, in connect:

- `DnsUpgradeSettings.Failover *DnsResolverSettings`: nil fails closed (the
  behavior of every mux today). When set, the mux builds a second tunnel DoH
  cache from it.
- The mux keeps one active resolver at a time. Queries go to the preset
  while it is healthy; once it is failing (C5) and the failover resolver has
  answered, new queries go to the failover. The two never share a fan-out
  (3.2 step 5).
- While failed over, the cold prober keeps probing the preset at
  `FailoverProbeInterval` (a settings field, much shorter than the cold
  backoff cap of 5 min); its first success switches back. A network change
  or settings swap starts again on the preset.
- Failures count only while the device reports the tunnel ready (a
  `SetTunnelReady(func() bool)` predicate the device installs, as it installs
  `SetIpv6Unroutable`, `ip_mux_upgrade.go:1172-1188`), so a tunnel that is
  forming or down never triggers a fallback.

C3. Fail closed (family, security and personal presets): the mux sends no
answer, as today; cached names can be served up to five minutes stale; the
cold prober keeps retrying the preset with backoff. Nothing falls back to
the default servers, to plaintext, or to the host network; Fast DNS on
connect stops racing once the tunnel is ready (C7). The status line offers
"Use URnetwork DNS", which selects `default` in one tap (C8).

C4. Encrypted stays encrypted, for every preset, by B3 and C2, with tests
that fail if any plaintext or OS lookup happens for a preset (6.2).

C5. Detection. The mux reports resolver health from state it already keeps
(`ip_mux_upgrade.go:306-316`: consecutive failures and last success per
resolver generation): new `UpgradeMux.DnsResolverHealth()` returning the
state (`ok`, `cold`, `failing`, `failedOver`), the consecutive failure count
and the last success time. A preset is failing after
`ResolverFailingMinCount` failed resolutions in a row over at least
`ResolverFailingMinDuration`, both settings fields per CODESTYLE "Tunables
live in settings", counted only while the tunnel is ready (C2). The sdk
publishes `DnsResolverStatus` (`GetDnsResolverStatus` and a change listener,
across DeviceLocal, DeviceRemote, the rpc and JS), `unknown` while the window
is forming.

C6. Verification. The research found that a wrong personal id usually fails
silently: NextDNS and AdGuard answer an unknown id unfiltered, and NextDNS
does the same after its free quota. So the device checks what it can and
says only what it knows:

- Format first, synchronously, with a message specific to the adblock
  provider (D4).
- After a preset is applied and the tunnel DoH path is proven, and again
  after each resolver generation change (at most once per
  `DnsResolverVerifyMinInterval`), the device sends one probe through the
  tunnel to every url of the preset: the preset's `canaryName`, or
  `example.com` for presets without one. New
  `UpgradeMux.ProbeDnsResolver(ctx, name)` returns each url's response code
  and addresses, bypassing the answer cache, the reverse index and the
  server scores.
- Results, published as `DnsResolverStatus.Verification`:

| Probe result | Verification | What the screen says |
|---|---|---|
| canary answered `0.0.0.0`, or NXDOMAIN with EDE 17 "Filtered", which Quad9 sends only on blocks (one-tap presets) | `confirmed` | the adblock provider's status line, no check mark needed |
| canary answered a real address (one-tap presets) | `notFiltering` | "<Provider> isn't filtering from this exit" |
| REFUSED (Control D personal) | `rejected` | "Control D doesn't recognize this resolver ID" |
| SERVFAIL (AdGuard personal) | `malformed` | "AdGuard DNS rejected this device ID" |
| a normal answer (personal presets) | `unconfirmed` | "Saved, but we can't confirm this profile. If the ID is wrong, <Provider> answers without filtering.", with a link to the adblock provider's dashboard, never a green check |
| no answer | the health state applies (C5) | the failing or paused status |

Echo services that could confirm a NextDNS or AdGuard profile exist
(`test.nextdns.io`, AdGuard's `dnscheck`) but neither is usable yet (section
10).

C7. Fast DNS on connect with a preset (O3). During the connect window,
lookups use URnetwork's default DNS; the preset applies once the tunnel is
connected, so the adblock provider never sees the user's own address and a
personal configuration is never tied to it. While a preset is selected:

- The host projection (`hostFallbackDnsResolverSettings`) is URnetwork's
  default resolver over the host egress (`DefaultDnsUpgradeFallbackSettings`,
  `ip_mux_upgrade.go:188` on `829a5a54`), never the preset or its pins.
- The fallback races only while the tunnel is not ready, connecting or
  reconnecting (the C2 predicate). Today it races any query the tunnel has
  not answered within `LocalFallbackTimeout` (1 s), connected or not
  (`ip_mux_upgrade.go:79-84` on `829a5a54`). With a preset that would let an
  unfiltered host answer bypass a family preset that fails closed (C3) and
  race the tunnel failover (C2). New
  `DnsUpgradeSettings.FallbackUntilTunnelReady`, which the sdk sets whenever
  a preset is selected, stops the race once the tunnel is ready.
- Lookups during the connect window are not filtered by the preset. The DNS
  screen says so next to the toggle while a preset is selected.

Default and custom selections keep today's fallback.

C8. What the user sees. The status line names the preset, by its own name
(A3, decision 24), and the state: "DNS: AdGuard Ad Blocking"; for a preset
that falls back, "Ad blocking paused: AdGuard Ad Blocking isn't answering"
(a no-filtering preset, which pauses no ad blocking, reads "Cloudflare No
Filtering isn't answering. Using URnetwork DNS."); for one that fails
closed, "AdGuard Family isn't answering" or "DNS4EU Child isn't answering".
Each comes with two actions, "Use URnetwork DNS" (selects `default` in one
tap) and "Change location". No local notification: this is not a
disconnect-class event. A personal preset shows its name, "NextDNS
Personal", never the id (D5). The verification line under it names the
adblock provider, whose answers it judges: "AdGuard DNS isn't filtering
from this location" (C6).

C9. Regional and court-order blocks. Inside a working tunnel a preset's
queries leave from the exit's country, so a user in Tehran exiting in Germany
is unaffected. Blocks bite when the exit itself sits in a censoring country;
Fast DNS on connect never sends a preset's lookups over the host network
(C7). What the research found (GreatFire, OONI and the adblock providers'
own notices; patchy and partly dated):

| Adblock provider | China | Iran | Russia | Withdrawals and orders |
|---|---|---|---|---|
| AdGuard | DoH "100% blocked"; `d.adguard-dns.com` blocked | `dns.adguard.com` poisoned and SNI-blocked (2021) | `adguard.com` blocked since July 2022 | none |
| Control D | `freedns.controld.com` `/p0` and `/p2` blocked; `dns.controld.com` not | no data | no data | none |
| NextDNS | blocked (OONI 153 of 159 anomalous) | OONI 158 of 223 confirmed blocked, June to November 2026 | failure reports from about 2026-10-28 | none |
| Quad9 | mixed (stale data) | DoT SNI blocked (2020), DoH blocked (2022) | not blocked (2021) | leaves France 2026-11-24 ([Quad9](https://quad9.net/news/blog/au-revoir-france/)) |
| Cloudflare | SNI blocking reported (2025, unconfirmed) | `cloudflare-dns.com` blocked since September 2022 | DoT to 1.1.1.1 reset since August 2026 | complies with some French orders; fined in Italy |
| DNS4EU | not yet tested (GreatFire) | no data | no data | court-ordered blocks on every variant: French ARCOM and Paris court orders, Russian state-media domains ([DNS4EU](https://www.joindns4.eu/legal-information-and-compliance)) |

The status reports such a failure per exit (C5), and the screen suggests
another location, `default`, or the regional pick for the exit's country.

C10. REFUSED is a failure, as today (O11). A REFUSED or SERVFAIL from a
preset is a resolver failure to the mux (3.2 step 7), never a block, so an
outage or a rate limit shows as one (C5, C8) and the preset's failure policy
applies (C1). A wrong Control D resolver id answers REFUSED to every query,
and verification shows "Control D doesn't recognize this resolver ID" (C6).
The personal-ID help text says to keep the adblock provider's block response
at "zero IP" (the default) or NXDOMAIN, since a configuration that blocks
with REFUSED reads as a failing resolver (B4).

C11. Built-in addresses only (O5). Pins ship in the catalog and change only
with an app release; nothing re-resolves an adblock provider's name,
encrypted or not. When an adblock provider moves its addresses, every pin
fails: a family, security or personal preset fails closed and an
ad-blocking or no-filtering one falls back (C1), until a release carries the
new addresses. F3 checks the addresses before every catalog change.

### D. Personal IDs

D1. Path ids only. In a DoH request the path travels inside TLS, so the
exit operator, a stranger who can read TLS SNI and plaintext DNS, sees only
the adblock provider's hostname. A DoT or DoQ hostname id
(`abc123.dns.nextdns.io`) is in the cleartext SNI and in any plaintext
lookup of that name; a per-configuration NextDNS IPv6 address carries the id
in the destination address itself. A harvested id would let an exit operator
track a user across sessions and exits, pollute their logs, or burn a free
plan's monthly queries, after which NextDNS stops filtering. So ids travel
only in the DoH path, and the app never uses DoT hostnames or
per-configuration IPv6 for them. All three adblock providers have a path
form (A3).

D2. Never linked-IP, legacy or plain-IP modes. NextDNS needs a linked IP
only for unencrypted IPv4, AdGuard requires a linked IP to be residential
and refuses one already in use, Control D authorizes source IPs for legacy
DNS (and logs them), and OpenDNS Home keys everything to a registered IP.
Behind a residential exit each would attach the user's queries to whatever
account the exit owner's IP is linked to, or link a stranger's household to
the user's account. AdGuard's DoH Basic authentication (the DeviceID as user
name) is safe but needs client support connect lacks; it is not in this
plan. The help text says why linking is not offered, since the adblock
providers' dashboards suggest it.

D3. Input. A "Use my account" row leads to the adblock provider choice and
then one field, "Profile ID or DNS address". It accepts the bare id or
whatever the adblock provider's dashboard shows: the DoH url or the Android
Private DNS hostname. The sdk extracts the id (`NormalizeDnsPresetPersonalId`)
and stores only the DoH form. The placeholder is the adblock provider's own
example (`abc123`, `abcd1234`, `1a2b3c4d`), and a line of help says where the
id is (NextDNS's setup page, Control D's "Show Resolvers" screen, AdGuard's
device page). The free-tier notes sit under the field: NextDNS stops
filtering after 300,000 queries a month, and AdGuard's free Starter plan has
the same cap.

D4. Validation in the sdk, after trimming:

| Adblock provider | Paste forms (capture the id) | Id pattern |
|---|---|---|
| NextDNS | `https://dns.nextdns.io/<id>[/<name>]`; `^(?:(.+)-)?([0-9a-f]{6})\.dns\.nextdns\.io\.?$` | lowercased, `^[0-9a-f]{6}$` (the official installers' check, [install.sh](https://github.com/nextdns/nextdns/blob/a7e835115da831b0267c98dfd0be4de0175d9ad9/install.sh#L732-L738)) |
| Control D | `https://dns.controld.com/<id>[/<client>]`; `^([a-z0-9]+)(?:-([a-z0-9-]+))?\.dns\.controld\.com\.?$` | lowercased, `^[a-z0-9]{4,32}$`, provisional: Control D publishes neither alphabet nor length, every example is short lowercase alphanumeric, and the id works as a DNS label, so it is case-insensitive |
| AdGuard | `https://d.adguard-dns.com/dns-query/<id>`; `^([A-Za-z0-9-]{1,63})\.d\.adguard-dns\.com\.?$` | a DeviceID, lowercased, `^[0-9a-f]{8}$` (the documented form; the server accepts any 1 to 8 character label, so stricter catches typos that would answer unfiltered), or an automatic-device id `^(adr\|gam\|ios\|lnx\|mac\|rtr\|stv\|win\|otr)-[0-9a-f]{8}-[A-Za-z0-9](?:[A-Za-z0-9-]{0,48}[A-Za-z0-9])?$` with no `---` and at most 63 characters, its name part kept as typed ([KB](https://adguard-dns.io/kb/private-dns/solving-problems/automatic-devices/)) |

On top of the patterns: ascii only, and never `/`, `.`, `%`, `?`, `#` or
whitespace inside the id, so it cannot leave its path segment; a pasted url
or hostname of another adblock provider in the catalog gets
`dns_preset_error_personal_id_other_provider`; the built url is parsed again
and must have the template's scheme, host and path with the id in place. A
device name in a pasted url or hostname is dropped (O4). The format messages
follow Chrome's split between "Enter a correctly formatted URL" and "verify
that this is a valid provider", e.g. "NextDNS IDs are six characters, digits
and a to f, like abc123"; the second tier is C6.

D5. Secrecy. Whoever holds a personal id can send queries under the user's
configuration: track the user across sessions and exits, pollute their logs,
and use up a free plan's quota (D1). It is never logged (B7), never sent to URnetwork's servers, stored
only in the local record (wiped at logout), and crosses only the local
device rpc. The summary shows the preset's name, "NextDNS Personal", not the
id (decision 24). The saved id may
show in its own edit field on every app, and nowhere else: never in a
summary, a status line or a log (decision 21). The derived urls
in the settings carry it too; nothing logs a settings value today (the rpc
logs only the event name, `device_rpc.go:14466`), and a 6.1 test keeps it
that way. NextDNS terms allow a user's own id in another product (§4(i)) and
forbid circumventing usage limits (§4(h)), so URnetwork never ships a shared
configuration ([terms](https://docs.google.com/document/d/1gG2nq9fQ5lOyTlWXo5GIm7kEpcgZxFc22w11qvkLZ0o/preview)).

D6. No device names in the first release (O4). Personal presets send only
the configuration id: no device name as a path segment (NextDNS `/<name>`,
Control D `/<client>`), and none of NextDNS's device query parameters or
`X-Device-*` headers
([NextDNS](https://help.nextdns.io/t/x2h76ay/device-information-log-enrichment)).
The screen has no device-name field, and a name in a pasted url or hostname
is dropped (D4).

### E. Settings storage and sync

E1. `sdk.DnsResolverSettings` gains two fields:

```go
// The selected preset (dns_presets.go), "default" for the built-in lists,
// "regional-<cc>" for a regional recommendation, or empty for the explicit
// lists below (the custom editor). When set, the device derives the lists
// and ignores the ones given.
PresetId string
// The personal configuration id of a personal preset, else empty.
PresetPersonalId string
```

E2. The selection is the source of truth. `SetDnsResolverSettings` with a
`PresetId` validates it (`validateLocalCatalogValue`,
`device_local_preference_catalog.go:316-325`: an unknown id or an invalid
personal id is refused, and the previous value stays) and derives the lists
(A6). With an empty `PresetId` the given lists apply, as today. The advanced
editor starts from `DnsResolverSettingsAsCustom`, so its saves are custom;
the Fast DNS toggle goes through `DnsResolverSettingsWithFallback`, so it
keeps the selection.

The selections, in the screen's order (O9): `default`, the default in every
app and for every new or migrated record (N1, E4); a preset from the
ad-blocking, malware, family, personal or no-filtering groups, only ever
chosen by the user; custom, today's editor of DoH urls and DNS servers,
which every app keeps offering (O1); and `regional-<cc>`, offered only while
the exit's country has a recommendation and never selected automatically
(O6). The adblock provider picks show names only, no logos (O10). Nothing
changes the selection on its own except the switch to `default` when a
release drops the selected preset (A7), and the one-time move of a record
that holds the old Google-only kz recommendation to `regional-kz` (E4,
decision 22), a pick that user had already made in its older form.

E3. Every crossing carries both fields, the status of C5 and C6, and the
count of I3:

- the record codec (`local_state.go:603-626`);
- `cloneDnsResolverSettings` (`device_local.go:8390-8397`), which must copy
  them instead of relying on the connect round trip;
- the read-back (`dnsResolverSettingsWithLock`, `:8406-8413`), from a
  DeviceLocal field that holds the selection beside `upgradeMuxSettings`;
- `DnsResolverSettingsRpc`, `newDnsResolverSettingsRpc` and
  `toDnsResolverSettings` (`device_rpc.go:8293-8355`), the sync apply
  (`:10602-10606`), the new status listener and the blocked-lookups count
  getter (I3). Gob ignores fields an older peer lacks;
- the hand-written JS mapping (`js/device_remote.go:905-963`,
  `js/src/types.ts:979-1002`), for completeness;
- the cgo headers and packaging bindings, regenerated and committed in the
  sdk (`make generate`);
- the app editors (3.8), each moved onto the A5 helpers.

A test fills every field and requires all of these sdk paths to return it
(6.1), as the reliability round trip test does.

E4. Migration. sdk `d1e5e9fd` already rewrites saved lists that are exactly
the old Google defaults (3.1). On top of it the record gains `PresetVersion`.
A record without it whose toggles equal the defaults' (remote DoH only, any
fallback choice) and whose lists equal the current default lists, after the
`DefaultServersVersion` migration, loads as `PresetId: default`, so default
users follow later default changes without another list migration. Any other
record loads as custom. The `DefaultServersVersion` 3 migration runs first
for every record written before it (3.1): version 2 default lists gain
DNS.SB, so a record of any earlier version holding its era's defaults loads
as `default` with today's lists, and a record that holds exactly the old
Google-only kz recommendation loads as `regional-kz` (decision 22), the one
selection a migration sets other than `default`. A record at the current
version never migrates.

E5. Downgrade. An older sdk ignores `PresetId`, uses the stored lists and has
no pins: with a hostname preset its resolver cannot look up the host (the
remote plain DNS lists are empty), so DNS fails closed until the user picks
again. Ip-literal presets keep working. This is accepted (section 9).

E6. Hosted devices keep refusing resolver settings; the apps hide the picker
for them. Logout wipes the record, personal id included. No server-side sync
(N6).

E7. The hosted proxy API's JSON gains `doh_server_host_addrs` through
connect's struct; old proxies ignore it, and nothing provisions it in this
plan (N7).

### F. Per-platform UI

F1. Layout, the same on every app, with content and rules from the sdk. The
screen opens on the current selection, which for nearly everyone is
"URnetwork (default)", the tunnel's encrypted, unfiltered resolver and the
default in every app (N1).

1. The picks, in this order (O9), adblock providers alphabetical within each
   group: "URnetwork (default)"; "Block ads and trackers" (AdGuard Ad
   Blocking, Control D Ad Blocking, DNS4EU Ad Blocking); "Block malware"
   (Cloudflare Malware Blocking, DNS4EU Malware Blocking, Quad9 Malware
   Blocking); "Family" (AdGuard Family, Cloudflare Family, Control D Family,
   DNS4EU Child, DNS4EU Child, no ads); "Use my account" (AdGuard Personal,
   Control D Personal, NextDNS Personal); "No filtering" (AdGuard No
   Filtering, Cloudflare No Filtering, Control D No Filtering, DNS4EU No
   Filtering); "Custom", today's editor, always there (O1); then "Regional
   (<country>), unencrypted", shown only while the exit's country has a
   recommendation and never selected automatically (O6).
2. Each pick shows its own short, descriptive name (A3, decisions 20 and
   24), the purpose translated and the adblock provider's name as it is,
   never a logo (O10), what it blocks (its picker copy, A3), the operator's
   jurisdiction and a privacy-policy link, as AdGuard's iOS app and Chrome
   do.
3. Under the selected pick: "Your DNS lookups go to <Provider> through the
   tunnel. <Provider>'s privacy policy applies.", with "Privacy policy" and
   "Help" links (H2). It names the adblock provider, "AdGuard DNS", since
   its policy applies whichever of its presets is picked.
4. The status line (C8) and the verification result (C6).
5. While a filtering preset is selected (the ad-blocking, malware, family
   and personal groups), the blocked-lookups count for this connection
   (O16, I3): "<count> lookups blocked by <Preset> this connection", with
   the preset's name, such as "120 lookups blocked by AdGuard Ad Blocking
   this connection", and a note that some blocks are not counted, so the
   figure is a floor.
6. The existing toggles stay: Fast DNS on connect, with the preset note
   (C7), and the built-in blocker, labeled as URnetwork's own. Both blockers
   can be on; the built-in one answers first (3.6).
7. Under "Custom", a note that a filtering server mixed with URnetwork's
   servers does not filter, because the fastest answer wins (3.2 step 5,
   O1).
8. A footnote: changes apply to new lookups at once, and apps may keep
   cached answers for about a minute (synthesized answers carry
   `ResponseTtl` 60 s, `ip_mux_upgrade.go:157`).

The summary row wherever DNS is shown today reads, for example, "DNS:
URnetwork", "DNS: AdGuard Ad Blocking", "DNS: NextDNS Personal", "DNS:
Custom" or "DNS: Regional (unencrypted)", from
`DescribeDnsResolverSettings`. A preset's row is "DNS: <name>", the status
line's text and key (`dns_status_provider`), since its name already says
what it does (decision 24); the per-group summary rows ("DNS: AdGuard DNS ·
Ad blocking") are retired.

F2. Per platform. The picker goes at the top of the existing Custom DNS
editor on every app, under the connection region row and the "Traffic DNS"
heading. On all four apps:

- The defaults panel ("Restore to most secure settings") becomes the
  "URnetwork (default)" pick and the regional panel the "Regional" pick. The
  four resolver toggles, the suggested servers and the four lists move under
  "Custom", seeded by `DnsResolverSettingsAsCustom`.
- The pill comes from `DescribeDnsResolverSettings` instead of each app's
  whole-value equality: no pill while a preset is selected (an explicit,
  encrypted choice; problems show as status); for `default` and custom,
  today's rules. Without this, every preset shows "The default safe settings
  are not applied".
- The editors build their saves from the A5 helpers, never a fresh struct.
- Adblock provider names, links and id formats come from the sdk, which also
  suits android's `LocalizedUiTextTest`.

Apple: the picker goes in `DnsSettingsView.swift`; `DnsSettingsStore` drops
its fresh-object `toSdk()` (`:72-89`), and its `LocalState` mirror carries the
new fields through the sdk record codec. The extension needs nothing beyond
the sdk. A line explains that system DNS profiles do not apply inside the
tunnel ("resolution within the VPN tunnel will use the VPN's DNS settings",
[WWDC20](https://developer.apple.com/videos/play/wwdc2020/10047/)).

Android: the picker goes in `DnsSettingsScreen.kt`; `DnsSettingsViewModel.apply`
(`:190-218`) builds from the helpers and reports whether anything was
applied. The strict Private DNS notice (`ConnectActions.kt:628-660`) gains a
line naming the selected preset, since strict mode overrides it. The Play
build shows the same catalog (O15): ad blocking stays off by default and an
explicit choice with its disclosure (H4). If review objects, the `play`
flavor (`android/app/app/build.gradle:327-342`, `BRINGYOUR_BUNDLE_STORE`
"play") calls `SetDnsPresetsHideAdBlocking` at startup (A5).

Windows: the picker goes in `DnsEditorSheet`; its `Draft` carries the new
fields or keeps the sdk struct whole as linux does, and
`DnsSettingsEquivalent` (`ConnectPage.cpp:82-98`) gives way to the sdk
summary. Strings can ship through `pages::Adv` until the store has them.

Linux: the picker goes in `DnsSheet`; `SettingsEqual` compares the new
fields. The GUI applies its stored DNS settings when it binds to the daemon's
device, as it does transport settings (`SdkHost.cpp:3148-3180`), so a pick
made while disconnected reaches the tunnel. `T_` English must match the store
byte for byte.

Web and extension: no picker (3.9, N7). If hosted presets come later, they
ride `initial_device_state.dns_resolver_settings` at connect, which the sdk's
`ProxyDeviceState` would have to model first.

F3. Catalog verification. Before a catalog change merges, a Go program
(`sdk/dnspresetcheck`, per CODESTYLE's rule for helpers) checks every preset
live from at least one dual-stack host: RFC 8484 GET over HTTP/2 at each url
and pin, through connect's own DoH client so the ALPN offer is the one users
send, the certificate verified against the url's host (an IP SAN for ip
literals), both families, a normal name resolving, the canary answered as
the catalog says, and the HTTPS record of a blocked name carrying no address
hints (B4). Results and the date go into each entry's `checked`. It never
runs as a unit test (tests touch no network); running it in the nightly as a
non-blocking report is a later step.

### G. Localization

G1. New keys in `localizations/keys/`, one file each, snake_case, tagged for
android, apple, windows and linux (draft names, in the store's `dns_*`
family):

- groups and picks: `dns_group_block_ads`, `dns_group_block_malware`,
  `dns_group_family`, `dns_group_my_account`, `dns_group_no_filtering`,
  `dns_provider_default`, `dns_provider_custom`, `dns_provider_regional`
  (country);
- each preset's name (decision 24), one key per purpose with the adblock
  provider's short name as `{provider}`: `dns_preset_name_ad_blocking`,
  `dns_preset_name_malware_blocking`, `dns_preset_name_family`,
  `dns_preset_name_child`, `dns_preset_name_child_ads`,
  `dns_preset_name_no_filtering`, `dns_preset_name_personal` ("{provider}
  Ad Blocking", ..., "{provider} Personal");
- picker copy, one key per preset (the adblock provider's name as a
  placeholder, so the name stays untranslated), e.g. `dns_preset_adguard_copy`,
  `dns_preset_quad9_copy` ("blocks malware and phishing, not ads");
- the account field: `dns_personal_id_label`, `dns_personal_id_where`
  (provider), `dns_personal_id_format_nextdns`,
  `dns_personal_id_format_controld`, `dns_personal_id_format_adguard`,
  `dns_personal_id_quota_nextdns`, `dns_personal_id_quota_adguard`,
  `dns_personal_id_linked_ip_note`, `dns_personal_id_block_response` (C10);
- verification: `dns_verify_unconfirmed` (provider),
  `dns_verify_rejected_controld`, `dns_verify_malformed_adguard`,
  `dns_verify_not_filtering` (provider);
- status: `dns_status_provider` (provider), `dns_status_unreachable`
  (provider), `dns_status_paused` (provider), `dns_status_fallback`
  (provider, a no-filtering preset's fallback, C8), `dns_status_use_default`,
  `dns_status_change_location`;
- the blocked-lookups count (I3): `dns_blocked_count` (count, provider),
  `dns_blocked_count_floor_note`;
- disclosure and notes: `dns_preset_disclosure` (provider),
  `dns_preset_privacy_link`, `dns_preset_help_link`,
  `dns_preset_fast_dns_note` (provider), `dns_preset_dropped` (provider),
  `dns_preset_dropped_choose`, `dns_preset_dropped_custom` (A7),
  `dns_custom_mix_note` (F1), `dns_preset_apply_note`,
  `dns_apple_profiles_note`;
- the error ids of A5.

G2. Adblock provider names, operators and jurisdictions are not translated;
group names, copy, messages and the purpose in each preset's name are (A3,
decision 24), so "AdGuard Family" reads "AdGuard Familie" and "DNS4EU
Child" reads "DNS4EU Kinderschutz" in German, the adblock provider's name
first in every language. A
string that names the kind of company says "adblock provider", never a bare
"provider", in every language (decision 18); the `{provider}` placeholder
and the key ids keep their names.

G3. English ships first in the apps; the other languages land through the
store before the stable releases. A live key needs all 28 languages. The
release build regenerates the app files, so the store change is the only
edit.

### H. Privacy, terms and legal

H1. Opt-in only (owner rule, 2026-11-09, every app): ad blocking is off by
default, URnetwork's default DNS is the default everywhere, and presets are
only ever user-chosen. No preset is a default, preselected, promoted or
ordered as a recommendation; within a group the order is alphabetical (O9).
Existing users keep what they have, and every preset is offered to every
user, never behind Pro (H3).

H2. Disclosure before use: the selected pick's line (F1 item 3) is on the
screen where the pick is made. What each adblock provider receives: every
name looked up while connected, its timing, and the exit provider's address,
never the user's own (C7); for a personal preset, the id. What each adblock
provider says it keeps, per the research:

| Adblock provider | Logging | Operator and law |
|---|---|---|
| AdGuard public | "We do not process any of your Personal Data when you connect to and use public AdGuard DNS"; aggregate metrics and "an anonymous database of domains requested in the last 24 hours" ([privacy](https://adguard-dns.io/en/privacy.html)) | AdGuard Software Ltd, Limassol, Cyprus |
| Control D free | "Not store logs (no individual browsing history, timestamps, or query logs)" ([docs](https://docs.controld.com/docs/free-dns)) | ControlD Inc, Ontario, Canada; founded by Windscribe's founders ([about](https://controld.com/about-us)) |
| Quad9 | "does not collect or record user IP addresses"; no ECS on `9.9.9.9` ([privacy](https://quad9.net/privacy/policy/)) | Quad9 Foundation, Zürich, Swiss law |
| Cloudflare | no querying IPs in non-volatile storage, truncated IPs deleted within 25 hours; KPMG examined the 1.1.1.1 controls, not named for the Families addresses ([privacy](https://developers.cloudflare.com/1.1.1.1/privacy/public-dns-resolver/)) | Cloudflare, Inc., San Francisco |
| DNS4EU | per-query logs (the query, the response, its type, a timestamp, the resolver, the ASN and more) kept "for up to six (6) months"; client IPs replaced on the resolver by an HMAC whose key is "regenerated daily"; anonymized logs shared with project partners "who train the machine learning models" ([policy](https://legal-documents-dns4eu.s3.fr-par.scw.cloud/DNS4EU-Public-DNS-Resolver-policy-2025.pdf)) | Whalebone, s.r.o., Brno, Czech Republic; Czech law and that of each member state where a resolver runs |
| NextDNS | "If not specifically requested by the user, no data is logged"; the user picks retention and region, and a Privacy Guides thread argues logging is effectively on for new configurations ([privacy](https://nextdns.io/privacy)) | NextDNS Inc., Delaware |
| Control D custom | analytics off, counts only, or full, at the user's choice ([levels](https://docs.controld.com/docs/analytics-levels)) | as above |
| AdGuard private | per the user's dashboard | as above |

H3. Terms. No adblock provider publishes terms for apps that preconfigure its
public resolvers, so each general term was read against URnetwork's case:

| Adblock provider | Terms | What URnetwork does |
|---|---|---|
| AdGuard | EULA bars users to "rent, lease, loan, export or sell access", and other uses need "written consent" ([EULA](https://adguard-dns.io/en/eula.html)); sample rate limit 300 queries per 10 s per IPv4 /24, production values unpublished | ships without waiting for consent (owner, 2026-11-09); free to all users, never Pro-only; draft 01 asks for consent and for limits on exit traffic |
| Control D | no reselling without "prior written authorization"; may "impose usage or service limits ... at our sole discretion" ([terms](https://controld.com/terms)) | ships without waiting for authorization (owner, 2026-11-09), and stays in the catalog though it is Windscribe's sister company (O1); draft 02 asks for authorization, limits and the partner route |
| DNS4EU | free "strictly for their use in a personal capacity", "designed for manual configuration", "not eligible for enterprises or connectivity providers" ([terms](https://legal-documents-dns4eu.s3.fr-par.scw.cloud/DNS4EU-Public-Resolver-Terms-of-Use.pdf)); the resolvers "are aimed to be used primarily by individuals (consumers)", preferably in the EU; response rate limiting "doesn't affect regular users" ([policy](https://legal-documents-dns4eu.s3.fr-par.scw.cloud/DNS4EU-Public-DNS-Resolver-policy-2025.pdf)) | ships without waiting for an answer (owner, 2026-11-09); free to all users, never Pro-only; draft 04 asks whether the use is acceptable, whether to offer it only on EU exits, and about rate limits |
| Quad9 | none found; aggregate data "may be licensed for a fee" | approved with the rest (owner, 2026-11-09); a courtesy note, and sponsorship as Mullvad does, stay optional |
| Cloudflare | operators "may wish to provide end users with options to change from the default 1.1.1.1 resolver"; "proxied traffic may be rate limited" ([guidance](https://developers.cloudflare.com/1.1.1.1/infrastructure/network-operators/)) | optional note to resolver@cloudflare.com |
| NextDNS | §4(i) allows integration, §4(h) bars circumventing limits | nothing for user-entered ids |
| DNS.SB (a default, not a preset) | "free of charge for personal and non-commercial use"; "Commercial use requires prior authorization", including "Integrating DNS.SB into software or hardware sold to customers" ([ToS](https://dns.sb/tos/)); no strict rate limits for normal use, and applications with high query volumes are asked to get in touch first ([FAQ](https://dns.sb/faq/)) | a default since decision 19 (3.1); the owner sends the permission email, draft 03 |

The owner treats all DNS usage in this plan as approved (2026-11-09:
"Consider all dns usage as approved for this implementation. We will go
ahead as if they are approved."), so no preset waits for an adblock
provider's answer. That is a decision to proceed despite terms that ask for
written permission: the AdGuard EULA prohibits using AdGuard DNS "in any way not
expressly authorized by AdGuard Software Limited with a written consent",
and Control D's terms bar reselling it without "prior written
authorization"; DNS4EU's terms describe a personal, manually configured
service. The permission drafts live in the private factury repo
(`factury/adblock/`: 01 AdGuard, 02 Control D and 04 DNS4EU for presets,
and 03 DNS.SB for the defaults, which DNS.SB joined with decision 19; 05
RethinkDNS no longer applies, since RethinkDNS left the catalog, O14), and
sending them is the owner's choice; the owner sends DNS.SB's. The preset
drafts promise that URnetwork removes the adblock provider "in the next
release" if asked, which a built-in catalog can keep (O13).

None requires attribution and none grants logo or trademark use, so the apps
show names only (O10). No affiliate or referral parameters.

H4. Google Play. The Device and Network Abuse policy lists "Apps that block
or interfere with another app displaying ads" as a violation, and the
VpnService policy says "Any manipulation of ads that can impact monetization
will result in rejection" and requires a declaration with the disclosure flow
([Play](https://support.google.com/googleplay/android-developer/answer/12564964)).
Proton and Windscribe ship DNS ad blocking on Android, which shows it is
tolerated in practice, not that it is safe. The owner chose the same catalog
on Play, carefully presented (O15): ad blocking is off by default and always
the user's choice, with disclosure; the Play listing and the VpnService
disclosure describe it as a user-selected DNS provider; and the VpnService
declaration and video are reviewed before the Android stable release. If
review objects, the Play build ships a Play-only catalog without the presets
that block ads (`blocksAds`: the four AdGuard and Control D presets and
DNS4EU's two ad-blocking variants), through the sdk flag of A5; malware,
family, no-filtering, personal IDs and Custom stay. Apple's stance was not
researched.

H5. URnetwork sends nothing new to its servers: the selection stays on the
device (N9), and so does the blocked-lookups count, which is counted on the
device only, never sent anywhere, not logged, and reset at each connect
(I3).

### I. Observability

I1. No queried names in logs or telemetry, as today: errors strip the
request url (`net_http_doh.go:2048-2056`), and observation lines carry the
dial path, outcome and address.

I2. Logs name a DoH server by scheme and host only (B7). The app log records
the applied selection, health changes and verification results,
`[dns]preset=<id> personal=<yes|no> state=<state> verification=<result>`,
which a feedback upload carries for support; never the id itself.

I3. The status and verification are local, and so is the blocked-lookups
count, which ships in the first release (O16). connect counts recognizable
block answers (B9); the sdk exposes the count as `GetDnsBlockedCount()` on
DeviceLocal and DeviceRemote, across the device rpc and JS, and resets the
mux's count each time the device connects and when the selected preset
changes, so the figure always belongs to one connection and the adblock
provider it names. The DNS screen shows it only while a filtering preset is
selected
(F1). It is counted on the device only, never sent anywhere, kept with no
names, and left out of logs and feedback uploads (H5, consistent with N9).
It is a floor: blocks answered with a plain NXDOMAIN or a REFUSED are not
counted (B9).

I4. No server-side counters of preset use (N9).

## 5. Wire, storage and API changes

| Surface | Change |
|---|---|
| connect `DnsResolverSettings` (Go and JSON) | `DohServerHostAddrs map[string][]string`, `doh_server_host_addrs`, omitempty |
| connect `DnsUpgradeSettings` | `Failover *DnsResolverSettings`; `ResolverFailingMinCount`, `ResolverFailingMinDuration`, `FailoverProbeInterval`; `FallbackUntilTunnelReady` (C7) |
| connect `UpgradeMux` | `DnsResolverHealth()`, `ProbeDnsResolver(ctx, name)`, `SetTunnelReady(func() bool)`, `DnsBlockedCount()`, `ResetDnsBlockedCount()` (B9) |
| connect logs | DoH urls as scheme and host |
| sdk `DnsResolverSettings` (gomobile, cgo, JS) | `PresetId`, `PresetPersonalId` |
| sdk `.dns_resolver_settings` record | the two fields, `PresetVersion`; `DefaultServersVersion` 3 (3.1) |
| sdk `DnsResolverSettingsRpc` (gob) | the two fields |
| sdk device rpc (gob) | the status listener and the blocked-lookups count |
| sdk new types and functions | A5, including `SetDnsPresetsHideAdBlocking`; `DnsResolverStatus` (state, verification, preset, since); `GetDnsResolverStatus`, `AddDnsResolverStatusChangeListener` and `GetDnsBlockedCount` on DeviceLocal and DeviceRemote |
| sdk settings | `DnsResolverVerifyMinInterval` |

All changes are backward compatible: an older reader ignores the new fields
and behaves as today (with the downgrade limit of E5).

## 6. Tests

All deterministic, no network, no sleeps as proof (CODESTYLE "Tests"). No
app test suites (owner rule, 2026-11-03): the logic and its tests live in
connect and the sdk. Fixtures use synthetic `.example` hosts and
documentation addresses (CODESTYLE), never the catalog's real endpoints;
only the catalog-integrity test reads the catalog itself.

### 6.1 sdk

- Catalog integrity (A4), table-driven over every entry, and every preset
  named by a key of its group's purpose and its adblock provider's short
  name, no two presets of the catalog showing the same name (A3, decisions
  20 and 24); every preset's English name pinned on its row, the adblock
  provider list, the summary, the status line and the dropped notice, and the name
  keys' English checked against the store when it is at hand.
- Normalization and validation per adblock provider, from D4: bare ids;
  dashboard urls with and without a device name; DoT hostnames with and
  without a device name; upper case; whitespace; a url or hostname of
  another adblock provider; `/`, `..`, `%2F`, `?`, `#`, non-ascii,
  over-long; a seven-hex AdGuard id (refused, though the server would take
  it); an AdGuard automatic-device id with `---` (refused); empty with a
  required id; an id for a preset that takes none.
- Expansion (A6): the lists hold only the preset's urls; ip-literal v4 and
  v6 urls pair by index; pins set; the chain is DoH-only; remote plain DNS
  empty; the fallback projection of a preset is URnetwork's default resolver,
  with no preset url or pin, and sets `FallbackUntilTunnelReady` (C7), while
  that of custom settings carries the pins; the ignore list carries hosts
  and pins; `Failover` is the default resolver exactly for presets with
  `failover`.
- Play-only catalog (O15): with `SetDnsPresetsHideAdBlocking(true)`,
  `GetDnsPresetProviders` lists no preset whose `blocksAds` is true and
  every other preset, and a stored selection it hides applies as `default`
  with the dropped-preset notice.
- Pins follow the host: custom settings that name a catalog host get its
  pins; preset settings copied as custom with a fresh struct, as today's
  editors build them, still carry no plaintext lookup.
- Helpers (A5): each keeps every field it does not set (the fill-every-field
  fixture below).
- The pill (F2): none while a preset is selected; the recommendation pill for
  `default` or custom when the exit country has an unapplied recommendation;
  the defaults pill for custom settings that differ from the defaults; none
  for `default` elsewhere.
- Round trips (E3): a test fills every `DnsResolverSettings` field with a
  distinct value and requires it back through set and get on DeviceLocal,
  the record codec, `cloneDnsResolverSettings`, the rpc mirror and gob, and
  DeviceRemote set and get; `device_rpc_mirror_test.go` extended to
  `DnsResolverSettingsRpc` and the status.
- Migration (E4): a record with the old Google defaults loads, through
  `d1e5e9fd`'s migration, as `default`; a record with the current defaults
  loads as `default`; a near-default (one operator removed) loads as custom;
  a record whose preset id a release dropped applies as `default`, and its
  summary carries the notice naming the adblock provider until it is
  dismissed, which saves `default` (A7).
- `DefaultServersVersion` 3 (3.1): the default lists of versions 0, 1 and 2
  chain to today's, with DNS.SB, as `default`; edited version 2 lists load
  as saved and reordered ones keep their order, both as custom; an app's
  copy of `default` with the version 2 lists seeds DeviceLocal, DeviceRemote
  and LocalState as `default`; each Google-only kz recommendation, from a
  record of every earlier version, loads as `regional-kz` with today's kz
  lists, on the DeviceLocal load path too, and every near miss (another
  toggle, another server, Google's other address, a changed v6 list, the
  February recommendation, a version 3 record) loads as saved.
- Refusals (E2): an unknown preset id or an invalid personal id is neither
  saved nor applied, and the previous resolver stays.
- Verification (C6), with a fake probe: each row of the table maps to its
  result; one-tap presets without a canary never report `unconfirmed`; a
  personal preset never reports `confirmed`.
- Status (C5): `unknown` while the window forms; `failing` only past both
  thresholds; `failedOver` only for presets with `failover`; recovery returns
  to `ok`. Thresholds from settings, clock injected.
- Blocked count (I3): with a fake mux count, `GetDnsBlockedCount` returns it
  through DeviceLocal, the rpc and DeviceRemote; the summary shows it only
  for a filtering preset, never for `default`, custom, regional or a
  no-filtering preset; a connect and a change of preset reset it, and a Fast
  DNS toggle does not.
- Hosted devices refuse a preset like any resolver settings.
- Logs: applying a personal preset and every status or verification change
  write no id to the sdk log, and the count never reaches it.

### 6.2 connect

- Pins, tun path: a hostname DoH url with pins resolves through an
  `httptest` TLS server whose certificate names `dns.example`, with a fake
  plain-DNS server (`newFakeDnsServer`, `net_http_doh_modes_test.go:86`) that
  must see zero queries, and the handler sees HTTP/2, so the pinned dial
  keeps the transport's ALPN (section 7).
- Pins, host path: the local DoH client dials the pins; a host resolver that
  fails the test if called.
- Pins fail closed: every pin refused by the tunnel dialer; no plain-DNS
  query, no OS lookup, no request to any other url; the resolution fails.
- TLS still verifies: a pin pointing at a server with a certificate for
  another name fails.
- Family: a v4-only pin set answers AAAA empty and the dial uses v4.
- DoH-only chain: with the preset expansion, a filtering server's NXDOMAIN
  reaches the client as NODATA and no later step runs (the fall-through at
  `net_http_doh.go:1195` cannot reach plain DNS).
- Exclusive filtering: in `newDnsClientHarness`
  (`ip_mux_upgrade_test.go:169`) with a fake filtering server that answers
  `ads.example` with `0.0.0.0`, the client gets `0.0.0.0`; with a second,
  unfiltered server in the list, the test documents that filtering is lost,
  which is why A6 forbids the mix.
- Sink answers (B4): `0.0.0.0` and `::` answers reach the client, enter no
  reverse-index entry and fire no `DohResultCallback`; a mixed answer is
  recorded as today.
- Failover (C2): a failing primary with a working failover moves new queries
  to the failover only after the thresholds and only while the tunnel is
  ready; no query ever fans out to both; the first primary success switches
  back; a network change starts on the primary; a nil `Failover` never
  switches.
- Probe (C6): `ProbeDnsResolver` returns REFUSED, SERVFAIL, NXDOMAIN and
  addresses per url from fake servers, and leaves the answer cache, the
  reverse index and the server scores untouched.
- Log redaction (B7): a failing personal-path url's warm failure line and
  error strings contain no path.
- Health (C5): consecutive failures and last success move as resolutions
  fail and succeed, per generation; a settings swap resets them.
- Blocked count (B9): in `newDnsClientHarness` with a fake filtering server,
  a `0.0.0.0` answer, a `::` answer and an NXDOMAIN with EDE 15, 16 or 17
  each count once per client query, a cached block included; a real NXDOMAIN
  without an extended error, an NXDOMAIN with EDE 4 or 18, a REFUSED, the
  built-in blocker's answers and a built-in endpoint's filtered response
  count nothing; `ResetDnsBlockedCount` zeroes it.
- Fast DNS window (C7): with `Fallback` and `FallbackUntilTunnelReady` set
  and a tunnel resolver that never answers, a query gets the fallback's
  answer while the tunnel-ready predicate is false and none once it is true;
  without the flag the fallback races as today.
- DNS.SB as a default (3.1, decision 19): the exact default lists; DNS.SB on
  every list built from them (the mux resolver, the Fast DNS projection, the
  control-name copies, the ingress lists, the egress fallback); its pairs,
  also in a user's list beside their own servers; its four urls guarded in
  every spelling, a filtered response from one never an answer or cached, a
  lone miss cached for `LoneMissExpiration` and its extended error 29 miss
  kept; its addresses in the ignore baseline, the blocker subtraction and
  the composed block-action path.

### 6.3 Regional failure and ingress

- A dialer that refuses the preset's pins (an exit network blocking the
  adblock provider, or pins gone stale, C11): a family preset stays failing
  with no fallback and no plaintext; an ad-blocking or no-filtering preset
  moves to the failover and back when the dialer heals; the sdk status
  follows (backoff and intervals from settings, clock injected).
- The same with Fast DNS on connect: while the tunnel is not ready the host
  projection queries URnetwork's default resolvers and never the preset or
  its pins; once it is ready nothing races, and the family preset stays
  failing (C7).
- A regional pick (`regional-cn`) expands to exactly
  `RegionalDnsResolverSettings("cn")`.
- Ingress is untouched: with any preset selected, the control-name DoH
  settings equal `IngressDohSettings` for the space (existing
  `TestIngressEgressDnsSeparate`, `net_http_doh_bootstrap_test.go:135`,
  extended), so a filter can never affect `api.bringyour.com`.

### 6.4 Apps

App suites are not run (owner rule, 2026-11-03); the app changes land after
a careful read. The apps' source-pinning wiring tests move with the editor
code they pin and are updated in the same change: windows
`tests/editor_apply_test.go` and `tests/dns_settings_test.go`; linux
`EditorSafetyWiringTest.cpp`, `FastDnsWiringTest.cpp` and
`ConnectFoldDoorsWiringTest.cpp`; apple `DnsResolverSettingsMirrorTests` and
`FastDnsOnConnectTests`; android `SettingsEditorWiringTest` and
`FastDnsOnConnectToggleTest`. The pill, editor, verification and status rules
have their deterministic tests in the sdk.

## 7. Rollout

1. `fix/no-google-dns` (connect `b49cbc21`) and `fix/default-resolvers-v2`
   (connect `829a5a54`, sdk `5c7a6eb7`) have merged; E4 builds on their
   migrations.
2. No permission gates: every adblock provider is approved (owner,
   2026-11-09, O12), and sending the drafts in `factury/adblock/` blocks
   nothing. The Play
   review stays: the VpnService declaration, the disclosure video and the
   listing text are reviewed before the Android stable release (O15, H4).
3. connect: B1, B4, B7, B9, C2, C5, the probe and C7's
   `FallbackUntilTunnelReady`, with 6.2. Inert until a resolver setting
   carries pins, a failover or the flag; the count changes no answer.
   HTTP/2 needs no new work for DNS4EU, which refuses a client that offers
   no ALPN: every DoH client in connect is built by `httpClientWithDialer`
   (`net_http_doh.go:687-688`, `:1360` and `:1394` on `829a5a54`), whose
   `http2.ConfigureTransports` call (`:433`) puts `h2` ahead of `http/1.1`
   in the ClientHello's ALPN list (x/net v0.62.0,
   `http2/transport.go:124-129`). B1's pin dialer stays a `DialContext`
   below TLS, so pinned dials keep that list, and 6.2's pin test checks that
   `h2` was negotiated. A connect merge reaches the provider fleet with the
   next finished nightly; providers run no `UpgradeMux`, so nothing changes
   for them, but the merge is still a miner release and is timed like one.
4. sdk: the catalog (after an F3 run), the selection fields and A5 helpers,
   derive on apply, migration, the dropped-preset notice, the Play flag,
   verification, status, the blocked-lookups count, rpc and the JS mapping,
   with 6.1 and 6.3; then `make generate`. Also reaches providers with the
   next nightly, inert there.
5. Apps: apple, android, windows and linux (F2), with English strings, each
   on a branch of the same name per repo. App builds reach testers in the
   nightlies; stable releases are manual per app repo, the Android one after
   the Play review.
6. Localizations: the G1 keys in all 28 languages, the count's strings
   included (O16), then the stable releases.
7. Help: an adblock providers section in `mmm/ur.io/docs` with an explicit
   `{#dns-providers}` anchor so translations keep it, the tours' stale "Local
   DNS fallback" renamed, and a link from the screens, as the apps link
   `https://ur.io/docs/api`. ur.io deploys are manual; the help lands with
   the next build (decision 23).
8. `feature/dns-defaults-v3` (decisions 19, 20 and 22), with the next build:
   in connect, DNS.SB in the default lists and the pair tables, with 6.2's
   DNS.SB tests; in the sdk, `DefaultServersVersion` 3 (the version 2 lists
   in the old default table, the edit rule over every earlier version, and
   the kz move) and DNS4EU's family names, with 6.1's tests. No exported
   sdk api changes, so no `make generate`. The connect change reaches the
   provider fleet with the next finished nightly, whose control-name DoH and
   egress fallback then include DNS.SB, so it is timed like a miner release.
   DNS.SB's rows in the ur.io threat model, and moving the hosted proxies in
   kz and tm off their older DNS settings (decision 22), are separate
   changes in mmm and the server.

Later, outside this plan: POST for custom urls (B8), presets for hosted
proxies (O7), device names on personal presets (O4), DoT or DoQ upstreams,
and a URnetwork-run blocklist in the mux (N10). There is no server-delivered
catalog (O13).

## 8. Owner decisions (2026-11-09)

The owner decided every question on 2026-11-09, and the sections above
follow these decisions. Later that day the approvals (O12) replaced the
permission parts of O1, O12 and O14, and then the owner dropped RethinkDNS
from the catalog (O14). A second round the same day, numbered 18 to 23 in
the owner's decision list, follows the O items, and decision 24, the
presets' names, came later that day.

- O1. The first catalog is the full set (A3): one-tap presets to block ads
  and trackers, to block malware and for families, the "No filtering" picks
  (Cloudflare 1.1.1.1, AdGuard Non-filtering, Control D p0) in the catalog
  proper, and the three personal-ID adblock providers. DNS4EU joins it, and
  RethinkDNS does not (O14). The owner added that manual entry stays
  available always, as today: "Custom" keeps today's editor of DoH urls and
  DNS servers beside Default, the presets and Use my account, and notes that
  mixing a filtering server with the defaults doesn't filter (E2, F1).
- O2. Failure policy: split by purpose (C1). Ad-blocking and no-filtering
  presets fall back to URnetwork's default DNS through the tunnel, with "Ad
  blocking paused: <Provider> isn't answering", and return on their own
  when the adblock provider answers. Family, malware and personal presets fail
  closed, with a status line and a one-tap "Use URnetwork DNS". Never
  plaintext, never the host network. Rejected: every preset fails closed.
- O3. Fast DNS on connect with a preset: during the connect window lookups
  use URnetwork's default DNS, and the preset applies once the tunnel is
  connected, so the adblock provider never sees the user's own address (C7).
  Rejected: querying the preset from the user's network, which this plan
  first recommended.
- O4. Device names on personal presets: not in the first release. Personal
  presets send only the configuration id (D6).
- O5. Addresses: built in only, "the simplest route at first". Nothing
  re-resolves an adblock provider's name; an address change waits for an
  app release (C11). Rejected: an encrypted re-resolution through the
  default DoH servers when every pin fails.
- O6. The regional recommendation is a pick in the new list, "Regional
  (<country>), unencrypted", shown only when the exit's country has a
  recommendation and never selected automatically (E2, F1).
- O7. Presets for hosted proxies through the proxy API: later, outside this
  plan (N7).
- O8. A preset dropped from the catalog in a later release: users on it
  switch to URnetwork DNS with a notice that names the adblock provider and
  says its filtering is off, offering "Choose another preset" and "Enter it
  yourself under Custom"; this applies to every group (A7). Rejected:
  keeping it working from the stored lists, which this plan first
  recommended.
- O9. Order: Default, Block ads and trackers, Block malware, Family, Use my
  account, No filtering, Custom, then Regional; adblock providers
  alphabetical within each group (A3, F1).
- O10. Names only, no logos. Each pick shows the adblock provider's name,
  what it blocks, its jurisdiction and a privacy-policy link (F1).
- O11. REFUSED from a preset is a failure, as today (C10). A wrong Control D
  id shows "Control D doesn't recognize this resolver ID"; outages and rate
  limiting show as failures; the personal-ID help text says to keep the
  block response at "zero IP" (the default) or NXDOMAIN.
- O12. Adblock provider permission: first decided as ask first, each
  adblock provider's presets waiting for its written OK. Replaced the same
  day: the owner treats all DNS usage in this plan as approved ("Consider
  all dns usage as approved for this implementation. We will go ahead as if
  they are approved."), so AdGuard, Control D and DNS4EU ship without
  waiting for answers. The drafts in the private factury repo
  (`factury/adblock/`) are the owner's to send (H3). DNS.SB as a built-in
  default was first a change to the defaults outside this plan; decision 19
  made DNS.SB a default (3.1).
- O13. Catalog delivery: built into the sdk only, changed by app releases;
  no server-delivered catalog and no withdraw list (A1, A7). The drafts
  promise removal "in the next release" on request. Rejected: a signed
  server copy that could withdraw ids without a release.
- O14. DNS4EU joins the first catalog with its five variants (A3). Both
  DNS4EU and RethinkDNS were first to be asked and to join in a later
  release; the approvals of O12 brought both into the first catalog. Then
  the owner dropped RethinkDNS ("yes drop it"): its non-filtering resolver
  forwards to Cloudflare and then Google upstream, and 8236 asked to avoid
  Google. It is in the "Left out" table (A3), and users can still enter it
  manually under Custom.
- O15. Play build: the same catalog, carefully presented. Ad blocking is off
  by default and always user-chosen, and the Play listing and the VpnService
  disclosure describe it as a user-selected DNS provider. If review objects,
  a Play-only catalog drops every preset that blocks ads (the four AdGuard
  and Control D presets and DNS4EU's two ad-blocking variants), and malware,
  family, no-filtering, personal IDs and Custom stay (H4). The owner's rule
  for every app: ad blocking is off by default, URnetwork's default DNS is
  the default everywhere, and presets are only ever user-chosen (N1, H1).
- O16. A local "lookups blocked" count ships in the first release (B9, F1,
  I3): a per-connection count on the DNS screen while a filtering preset is
  selected, counted on the device only, never sent anywhere and reset at
  each connect. It counts recognizable block answers, `0.0.0.0` or `::`
  sinks and NXDOMAIN with EDE 15 to 17; a block answered with a plain
  NXDOMAIN or a REFUSED is not counted, so the figure is a floor. connect
  counts, the sdk exposes it, and the apps show it, with new strings in all
  28 languages (G1).
- O17. OpenDNS in the default race: resolved outside this plan.
  `fix/default-resolvers-v2` removed OpenDNS from the defaults (3.1, connect
  `829a5a54`).

The second round, later on 2026-11-09:

- 18. Terminology: "adblock provider" (or "adblock providers"), never a
  bare "provider", for AdGuard, Control D, Quad9, Cloudflare, DNS4EU,
  NextDNS and the like, because in URnetwork "provider" already means the
  bandwidth providers. It applies to the user-facing strings, the ur.io
  help, this plan and the permission drafts; code identifiers and key ids
  stay (G2, and the note at the top).
- 19. DNS.SB becomes a built-in default resolver ("ok go ahead"), after
  Cloudflare and Quad9, at both of its published servers (N1, 3.1). The
  owner sends the permission email (draft 03). connect and the sdk carry it
  on `feature/dns-defaults-v3`, where the sdk's `DefaultServersVersion` 3
  moves saved version 2 default lists to the new ones.
- 20. DNS4EU's two family presets get names of their own, "DNS4EU Child"
  (`dns4eu-child`) and "DNS4EU Child, no ads" (`dns4eu-child-noads`), shown
  wherever a preset's name is (A3, F1, C8). The adblock provider's own name
  stays "DNS4EU".
- 21. The saved personal ID may show in its own edit field on every app,
  never in a summary, a status line or a log (D5).
- 22. With the new build, users in Kazakhstan who still have the old
  Google-only kz recommendation move to today's kz recommendation: the sdk
  loads a record that holds it exactly as `regional-kz` (3.1, E4), and the
  owner accepts that a user who typed exactly that list moves too. The
  hosted proxies in kz and tm also move off their older DNS settings, which
  is a server change, outside connect and the sdk (section 7).
- 23. The owner compiles the apps, with a build starting soon, and the
  ur.io help lands with it (section 7).
- 24. Every preset gets a short, descriptive name of its own (owner,
  2026-11-09). Asked whether the AdGuard, Cloudflare and Control D family
  presets should get names of their own, as DNS4EU's two did (decision 20),
  the owner answered: "Give each preset descriptive and short names." A
  preset's name is its adblock provider's short name and what it does:
  "AdGuard Ad Blocking", "Cloudflare Malware Blocking", "AdGuard Family",
  "Cloudflare No Filtering", "NextDNS Personal", with DNS4EU's family
  presets keeping "DNS4EU Child" and "DNS4EU Child, no ads" (A3). The
  purpose is translated through the localization store and the adblock
  provider's name never is (G1, G2). The picker row, the summary row ("DNS:
  <name>"), the status line, the blocked-lookups count, the Fast DNS note
  and the dropped-preset notice name the preset; the disclosure, the
  personal ID field and the verification line name the adblock provider
  (A3, A7, C8, F1). The Play listing and the VpnService disclosure keep the
  wording "DNS provider" (O15, H4 unchanged).

## 9. Known limitations

- Apps that do their own DoH (some browsers) and Android Private DNS in
  strict mode bypass the mux and the preset, as they bypass the built-in
  blocker (sdk `BLOCKER.md:178-182`). Apple's system DNS profiles do not:
  inside the tunnel the VPN's DNS settings apply (F2).
- OS and browser caches keep earlier answers for about a minute after a
  switch.
- Pins can go stale between releases, since addresses are built in only
  (O5, C11): a family, malware or personal preset then fails closed and an
  ad-blocking or no-filtering one falls back, until an update.
- DNS4EU's 14 resolvers are all in the EU, so exits elsewhere see longer
  round trips, and court-ordered blocks apply to every variant, Unfiltered
  included (A3).
- The blocked-lookups count is a floor: blocks answered with a plain
  NXDOMAIN or a REFUSED are not counted (B9).
- No adblock provider publishes numeric rate limits for its public tier.
  URnetwork spreads queries over many home addresses, which makes
  per-address throttling less likely than on a datacenter VPN, though a busy
  exit could trip it (the research's inference, not a measurement).
- A third party's blocks cannot be overridden on the device; a personal
  configuration manages its allowlist at the adblock provider.
- The status cannot tell "adblock provider down" from "exit network blocks
  the adblock provider".
- DNS.SB, a default since decision 19, has no point of presence in France,
  Portugal or Belgium, so a cold lookup on an exit there that draws it pays
  a longer round trip, and its six-day Let's Encrypt IP certificates need
  watching (3.1).
- Fixed (2026-11-09): a named bootstrap DoH server on a network that
  black-holes the defaults could be starved of the internal resolver's four
  request slots. A query launched its servers in the weighted random order,
  and a request to a black-holed default holds its slot until the query
  deadline, so a lookup that drew two default pairs ahead of the named
  server failed at its deadline. DNS.SB's longer lists made that more likely
  (`TestInternalDohResolvesThroughControlDohWhenTheDefaultsAreUnreachable`
  failed 7 of 3000 runs before DNS.SB and 20 of 3000 after; the seed of
  `ControlDohSettings` only made it rare), and the cn regional presets,
  which carry no seed, were starved on about half of the cold lookups. Now
  the named servers and the regional presets are lead servers
  (`DohSettings.NamedRemoteDohUrls`): every query launches them ahead of the
  defaults whatever the draw, and the defaults leave them a request slot
  (`DohSettings.DohServerLeadReserve`, one), so neither a draw nor a lookup
  collecting past an empty answer can give the defaults all four. The slot
  count is unchanged. The deterministic tests are in connect's
  `net_http_doh_lead_test.go`; the test above passed 3000 of 3000 runs
  after the fix.
- A wrong NextDNS or AdGuard id, or an exhausted free quota, answers
  unfiltered; the device can say only that it cannot confirm the profile
  (C6).
- Downgrading to an sdk without pins while a hostname preset is selected
  fails DNS closed (E5).
- The web and the extension offer no presets (3.9).
- The free resolvers are a bridge, not a foundation: every global ad-blocking
  resolver worth shipping now comes from two companies, one a competitor's
  sister company, and both reserve the right to limit or withdraw service.

## 10. Facts still to verify

The research could not settle these; each has the check that settles it
before the affected preset ships.

| Fact | Check |
|---|---|
| IPv6 urls and pins of every preset | F3 from a dual-stack host |
| Ip literals `1.0.0.1`, `1.0.0.2`, `1.0.0.3`, `149.112.112.112` and the IPv6 literals carry IP SANs | F3; else hostname plus pins (B6) |
| Control D's per-filter addresses (`76.76.2.2`, `76.76.10.2`, ...) and AdGuard's `94.140.14.59`, `2a10:50c0::dad:ff` serve their DoH hosts | F3; only then extra pins |
| HTTPS/SVCB answers for blocked names carry no address hints | F3 (B4) |
| Control D resolver id alphabet and length | a real id from a Control D account; until then the provisional pattern (D4) |
| What AdGuard's Starter plan does at its 300,000-request cap | ask AdGuard support; the free-tier note (D3) stays until then |
| A NextDNS or AdGuard echo that confirms a profile over DoH (`test.nextdns.io` returned an empty body; AdGuard's `dnscheck` hostname is unpublished) | engineering test; until then `unconfirmed` (C6) |
| How Quad9 implements its French exit from 2026-11-24 | watch Quad9's notice; the status reports it per exit either way |
| Apple App Review's view of DNS ad blocking inside a VPN | ask before the iOS release |
| Numeric rate limits | the drafts in `factury/adblock/` ask AdGuard, Control D and DNS4EU, if the owner sends them; the status reports a limit as a failure either way (C10) |
| DNS4EU's listed addresses serve each variant's DoH host, with a certificate for that host, over HTTP/2 negotiated by connect's ALPN | F3 at each pin; the DoT certificate on `noads.joindns4.eu:853` named `doh.joindns4.eu`, so the DoH certificate's names are F3's to read |
| DNS4EU's block answers for Protective's malware blocks and for the two child variants, and `doubleclick.net` as the child-noads canary | F3 with a blocked name per variant; only the ad-blocking variant's `0.0.0.0` for `doubleclick.net` was probed |
| How DNS4EU answers a court-ordered block, on every variant, Unfiltered included | F3 with a domain from its compliance page; the copy and the count (B9) follow what it sends |
| DNS.SB's v6 urls `https://[2a09::]/dns-query` and `https://[2a11::]/dns-query` serve DoH with a certificate for their addresses over HTTP/2 (the v4 certificate's IP SANs name them) | a probe from a dual-stack host; until then the race tolerates a failing endpoint, as for every default |
