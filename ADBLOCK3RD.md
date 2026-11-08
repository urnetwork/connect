# Third-party ad-blocking DNS presets plan

Status: draft, 2026-11-08 (phase 1). The design is proposed, not decided.
Provider facts are `TODO(research)` placeholders until the research report
lands (`factury/reports/Ad blocking DNS resolver presets.md`, notes in
`factury/research_notes/Ad blocking DNS resolver presets/`). Section 10
indexes every placeholder. Section 8 lists the decisions the owner needs to
make.

Line numbers refer to connect `bc3fec12`, sdk `90b7a59f` and server
`562ead790` unless a section says otherwise.

## 1. Original request

Owner decisions, 2026-11-08:

- Remove Google from URnetwork's default DNS resolver lists. This is in
  progress on `fix/no-google-dns` and is not part of this plan, but this plan
  builds on it (section 7).
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

## 2. Goals and non-goals

Goals:

- G1. A user picks a third-party ad-blocking resolver with one tap in the DNS
  settings of every app: apple, android, windows and linux. The web and the
  extension control hosted devices, which take no resolver settings (3.9).
- G2. Each provider offers its variants (ad blocking, family, security,
  unfiltered) as separate picks where the provider has them.
- G3. A user can enter a personal configuration ID (NextDNS, Control D custom
  resolvers, AdGuard private DNS) as a path ID, validated in the sdk.
- G4. A preset resolves on the same path as the defaults: DoH from the
  device, through the tunnel, egressing at the exit provider. It is encrypted
  end to end, the resolver's own name is never looked up in plaintext, and
  nothing resolves off the tunnel unless the user turned on Fast DNS on
  connect.
- G5. When the chosen resolver fails, DNS fails closed and the user is told.
  Nothing switches the resolver silently.
- G6. One catalog, one selection model and one set of validation rules, in
  the sdk. The apps keep screens only (owner rule, 2026-11-03: app logic goes
  in the sdk).
- G7. Deterministic tests for every rule above, in connect and the sdk.

Non-goals:

- N1. No preset becomes a default. The defaults stay URnetwork's built-in
  list (without Google once `fix/no-google-dns` lands).
- N2. The control-name (ingress) DNS does not change. Presets never touch
  the bootstrap DoH servers, the connection region or the bootstrap plain
  fallback (3.4). An ad-blocking resolver adds nothing there, and a filter
  that blocks VPN domains would break sign-in.
- N3. No OS-level DoH or DoT. The platforms keep advertising the plain-DNS
  stand-in that the mux claims (sdk `tunnel_dns.go:5-29`).
- N4. No linked-IP (source address) provider configurations (D2).
- N5. No DoT or DoQ upstream and no plaintext presets. The existing regional
  recommendation (plain DNS in cn, ru, ir and others) stays as it is.
- N6. No network-wide settings sync. Logout still wipes local DNS settings.
- N7. No presets for hosted proxies or hosted devices in this plan. Those
  devices refuse resolver settings today (3.5).
- N8. No change to the built-in blocker or its lists, and no URnetwork-side
  allowlist for a third party's blocks.
- N9. No server-side telemetry of which preset a user picks.

## 3. What already exists

### 3.1 Default resolver lists

- `DefaultDnsResolverSettings` (`net_http_doh.go:140-201`) turns on remote
  DoH and local plain DNS. Remote DoH v4 (`:159-164`) and v6 (`:165-170`)
  name Cloudflare, Google, Quad9 and OpenDNS. The remote plain-DNS lists
  (`:176-187`) name the same four. The local plain-DNS lists (`:192-199`)
  name Quad9 and Cloudflare.
- The operator pair tables `knownDohUrlPairs` (`:1503-1516`) and
  `knownDnsServerPairs` (`:1518-1532`) include Google's pairs. A url outside
  the tables pairs by list index with another url outside them
  (`familySiblings`, `:1534-1580`), and `dualStackList` drops repeats
  (`:1598-1629`).
- Google also appears in the regional lists for kz and tm
  (`net_http_doh_regional.go:28,30`), `ip_probe_targets.go:46-49,174-175,
  206,245-246`, `ice_net.go:171-174`, `egress_resolver_linux.go:152` and
  `egress_resolver_windows.go:55`. Which of these `fix/no-google-dns` changes
  is that branch's scope. As read for this draft, uncommitted and subject to
  change, it drops Google from the default DoH and plain lists, keeps the
  pair tables so a user who names Google still races both families, gives kz
  Cloudflare and Quad9 in Google's place, and leaves tm with Turkmentelecom.
  It does not touch the sdk, so a saved record that holds Google keeps it
  (3.5, E4). TODO(no-google): record the final change once it merges.

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
   first NOERROR or NXDOMAIN (`:2186-2215`). Consequence for this plan: a
   filtering resolver mixed with an unfiltered one does not filter.
6. An NXDOMAIN becomes an empty NOERROR for A and AAAA (`buildDnsResponse`,
   `ip_mux_upgrade.go:2344-2381`). An answer of `0.0.0.0` or `::` counts as
   records (`parseDohWire`, `net_http_doh.go:2217-2283`), is recorded in the
   reverse index (`ip_mux_upgrade.go:1852-1854`) and is bound to the exit that
   carried the DoH flow (`net_http_doh.go:1181-1189`, `bindDnsResultToExit`
   `ip_mux_upgrade.go:2734-2756`). A REFUSED or SERVFAIL is a resolver failure,
   not an answer (`net_http_doh.go:2227-2234`).
7. On failure the mux sends no answer and the client retries
   (`ip_mux_upgrade.go:1822-1830`). A cached answer can be served up to five
   minutes stale while fresh resolution fails (`net_http_doh.go:44-56`). A cold
   prober re-proves the path with backoff from 2 s to 5 min
   (`ip_mux_upgrade.go:227-233`).
8. A hostname-form DoH url resolves its own host over remote plain DNS
   through the tunnel, even with remote DNS disabled (`net_http_doh.go:439-443,
   682-704, 1168-1173, 1208-1229`). The tun dial resolves through the same
   cache (`tun.go:1631-1648`). That is a plaintext lookup of the resolver's
   name. The production defaults are ip literals and never use it.
9. DoH TLS uses the system roots; nothing pins certificates in production
   (`net_http_doh.go:33`, `:359-361`, `:397-406`). The client speaks HTTP/2
   only (`:407-418`) and sends RFC 8484 GET requests
   (`:2005-2066`). Each dial path has its own TLS session cache, so a ticket
   from the host path is never redeemed through the tunnel (`:664-669`).
10. A settings change swaps the tun's DoH cache, with an empty answer cache,
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
  sdk value from the live mux settings. Any sdk-only field added to the
  struct is dropped by both unless they are changed (E3).
- The record codec carries a `FallbackOptInVersion` (`local_state.go:596-626`).
  "the dns editors save every field" (`:597-598`), so a saved record keeps
  the lists of its day, Google included, whatever the defaults later become.
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

Read on main: apple `5465aca8`, android `0102ed06e`. Paths below are
relative to `apple/app/network/` and
`android/app/app/src/main/java/com/bringyour/network/`.

Common to apple and android:

- The traffic DNS editor, titled "Custom DNS", opens only from the connect
  drawer's "Custom DNS" card. Top to bottom it holds the connection region row
  (ingress, 3.4); a regional panel when the exit's country has a
  recommendation (the country is the selected provider's, so the exit's), or
  else a defaults panel with "Restore to most secure settings", which only
  fills the draft; four resolver toggles (remote and local DoH, remote and
  local plain DNS); Fast DNS on connect; "Suggested remote DNS servers" from
  `GetRegionalDnsServers`, every country listed, Google's kz and tm entries
  included; four editable lists (remote and local DoH urls, remote and local
  DNS servers, v4 and v6); and "Update", a compare-and-set against the
  settings the editor opened with.
- Custom entries are free-form and checked only in the app, weakly: apple
  requires https and a host
  (`Shared/Views/Stats/DnsSettingsView.swift:447-456`), android only
  `startsWith("https://")` (`ui/stats/DnsSettingsScreen.kt:447-449`).
  Neither checks the family; the sdk checks nothing. So a user can already
  type a hostname DoH url, a NextDNS personal url for example. Its host is
  then looked up in plaintext through the tunnel (3.2 step 8), and unless the
  user also removes the default servers, the fan-out mixes it with unfiltered
  ones (3.2 step 5), so filtering is unreliable. No personal-ID field exists.
- The card summarizes four On/Off rows (DNS over HTTPS, unencrypted DNS,
  local DNS, Fast DNS on connect) and shows a pill when the settings differ,
  by whole-value equality, from the exit country's recommendation or else from
  the defaults: "The default safe settings are not applied"
  (apple `Main/Connect/ConnectStatsSections.swift:302-359`, android
  `ui/stats/ConnectStatsSections.kt:465-536`). No provider or server name is
  shown anywhere.
- Both editors build a fresh `DnsResolverSettings` from their own draft
  model (apple `Shared/ViewModels/DnsSettingsStore.swift:72-89`, android
  `ui/stats/DnsSettingsViewModel.kt:197-210`), so a field the draft does not
  model is dropped on every save. Today that is only the mask, which the sdk
  maps back to the default; the new fields of E1 would be dropped the same way.
- The built-in blocker is its own card under the Custom DNS card
  (apple `ConnectStatsSections.swift:199-215`, android
  `ConnectStatsSections.kt:266-311`), not on the DNS screen.
- The bootstrap DoH editor (with "Use China resolvers") is under Account >
  Extenders and the login "Change Network API" sheet; the connection region
  picker also appears at login and on the can't-reach prompt.

Apple:

- `DnsSettingsView.swift` is a sheet (controls `:138-347`), reached from
  `Main/Connect/ConnectActions/ConnectActions.swift:276-283`, card
  `ConnectStatsSections.swift:158-197`.
- The app holds a `DeviceRemote`; the packet tunnel extension holds the
  `DeviceLocal`. An edit crosses the rpc and applies at once while connected:
  the extension re-applies its network settings on a DNS change
  (`app/extension/PacketTunnelProvider.swift:1248-1259`). While disconnected
  the remote queues the edit for the next connect. The app also mirrors the
  read-back settings into its own `LocalState` record
  (`DnsSettingsStore.swift:224-301`) and seeds the next device from it
  (`Shared/ViewModels/DeviceManager.swift:1745-1756`), covered by
  `DnsResolverSettingsMirrorTests`.
- `NEDNSSettings` carries the plain stand-in servers from
  `tunnelDnsAddressesIpv4/6` with `matchDomains = [""]`; OS-level DoH and DoT
  settings are deliberately not used (`PacketTunnelProvider.swift:1824-1837,
  2082-2102`).

Android:

- `ui/stats/DnsSettingsScreen.kt` is a full screen (controls `:93-441`),
  reached from the card (`ConnectStatsSections.kt:217-262`) and the tablet
  tiles (`ui/stats/ConnectStatsTiles.kt:159-184`).
- `MainService` and the `DeviceLocal` run in the app process.
  `applyDnsResolverSettings` writes to the live device, or without one to
  `LocalState` (`DeviceManager.kt:311-337`), and applies at once; `MainService`
  rebuilds the TUN only when the advertised DNS list changes
  (`MainService.kt:417-421`).
- Private DNS in strict mode bypasses URnetwork DNS; it is detected and shown
  as a notice while connected (`PrivateDnsMode.kt`,
  `ui/connect/ConnectActions.kt:628-660`).

Windows (`windows/app/src/App/`, main `77b73df0`):

- The editor is a dialog, `DnsEditorSheet` (`StatsSheets.cpp:1257-1741`),
  opened from the pane C "Custom DNS" group (`MainWindow.xaml:1775-1791`,
  `ConnectPage.cpp:4366-4368,4419-4433`), with the same controls as above.
  "Update" is a compare-and-set against a fresh read, on the write worker
  (`SdkHost.cpp:6455-6486`, `EditorApply.h:26-52`).
- The editor's `Draft` models the fields one by one and has no mask
  (`StatsSheets.h:274-289`, `StatsSheets.cpp:1259-1310`); the card's pill
  compares with `DnsSettingsEquivalent` (`ConnectPage.cpp:82-98,3943-3985`).
  Checks are app-only (`StatsFormat.cpp:153-191`).
- The GUI holds a `DeviceRemote` to the service's `DeviceLocal` over the mTLS
  device rpc; DNS reads and writes go only through it, apply at once, and
  queue while the rpc is down. The service gives the tunnel interface the
  sdk's `tunnelDnsAddresses`, flushes the resolver cache, and its firewall
  lets port 53 reach only those servers through the tunnel
  (`windows/app/src/Service/TunnelController.cpp:1153-1166,1215`,
  `WfpPolicy.cpp:583-620`).
- The blocker toggle is in pane A's connect options.

Linux (`linux/app/src/`, main `80ea2e0`):

- The editor is a modal window, `DnsSheet` (`DnsSheet.cpp:75-270`), opened
  from the pane C "Custom DNS" group (`ConnectPage.cpp:1653-1665,3895-3910`);
  it refuses to open without a reading. Its draft is the whole sdk struct
  (`DnsSheet.hpp:77-78`), so unknown fields survive; `SettingsEqual`
  (`DnsSheet.cpp:53-65`) compares field by field. "Update" is a
  compare-and-set (`SdkHost.cpp:4677-4690`). Checks are app-only
  (`Formatters.cpp:106-120`).
- The `DeviceLocal` runs in `urnetworkd` only while a tunnel runs; the GUI
  binds a `DeviceRemote` after `start_tunnel`. With no tunnel, reads and
  writes fall back to the GUI's own `LocalState` (`SdkHost.cpp:4658-4675`),
  and that copy is never applied to the daemon's device when it binds: the
  restore block applies the profile, provide mode, route-local and transport
  settings but not DNS (`SdkHost.cpp:3148-3180`). An edit made while
  disconnected is lost to the tunnel.
- The daemon points the system resolver at the sdk's `tunnelDnsAddresses`
  (systemd-resolved with `dnsovertls no`, else resolvconf, else
  `/etc/resolv.conf`) and pins port 53 to them with nftables
  (`Tunnel.cpp:3705-3745,2436-2463`).
- The blocker toggle is in pane A's "More options".

The sdk's cgo headers are generated and committed in the sdk
(`sdk/cgo/Makefile:18-22`, `make generate`; `sdk/cgo/README.md:80-89`, which
also regenerates the packaging bindings). Windows and linux keep them out of
git and fetch them from the sdk's build zip (`windows/app/tools/fetch-deps.ps1`,
`linux/app/scripts/fetch-deps.sh`).

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
  is therefore a read-only summary that in practice shows "DNS settings
  unavailable". The web has no Fast DNS toggle by the owner's decision of
  2026-10-02 (mmm `63fe98616`).
- A hosted device's resolver can be set only at connect, through
  `proxy_config.initial_device_state.dns_resolver_settings`; when none is
  sent the server fills in the exit country's regional recommendation
  (server `model/network_client_model.go:506-509,1278-1286`). Nothing sends one
  today: the extension sends the location and performance profile only
  (`extension/src/utils/auth-params.ts:61-64`), and the sdk's
  `ProxyDeviceState` has no DNS field (sdk `proxy_config.go:90-99`).
- The extension has no DNS settings. It is an HTTPS proxy, so names resolve
  on the proxy host through the device tun's DoH cache, built from the
  provisioned settings (`tun.go:1018-1050`).
- sdk/js maps the DNS methods of `DeviceRemote` by hand (sdk
  `js/device_remote.go:424-444`, conversions `:905-963`, TS types
  `js/src/types.ts:979-1002`). A new field is invisible to JS until all three
  are edited. The regional and bootstrap functions are not exported.
- Help pages live in `mmm/ur.io/docs`, served at `https://ur.io/docs/<slug>`.
  The platform tours have a Custom DNS section (`TOUR-IOS.md:118-147`,
  `TOUR-ANDROID.md:178-195`, `TOUR-MACOS.md:199-215`,
  `TOUR-WINDOWS.md:243-249`, `TOUR-LINUX.md:143-165`) and the threat model
  covers DNS (`THREAT-MODEL.md:566-590`). No page covers DNS providers, the
  blocker or the connection region, and the tours still call Fast DNS on
  connect "Local DNS fallback". The apps already hard-code one help link,
  `https://ur.io/docs/api`.

### 3.10 Localization

`localizations/keys/*.yaml` is the single source of every interface string
(`localizations/README.md`). One file per key, the file name is the key id
(snake_case), and each key lists the platforms that use it. `npm run gen`
writes the android `strings.xml`, apple `Localizable.xcstrings`, windows
`.resw` and linux `.po` files into the sibling repos; `npm run check` fails on
drift; the release build regenerates on every build and commits the output
(`build/all/run.sh:926-939,1266-1299`). The web reads `keys/` directly. The
store has 28 languages: windows and linux ship all 28, apple 27, android 19,
and ur.io's site 6 (`localizations/gen/store.mjs:23-85,257`).

Existing DNS keys include `custom_dns`, `traffic_dns`, `fast_dns_on_connect`,
`fast_dns_on_connect_description`, `dns_pill_*` and `control_doh_error_*`.
The apps map an sdk error id to its key (apple
`Shared/ViewModels/ControlDohSettingsForm.swift:48-78`, android
`ui/account/ControlDohLogic.kt:18-23,59-70`), and android's
`LocalizedUiTextTest` fails on any literal shown on screen that is not
allowlisted.

## 4. Decisions

### A. The preset catalog

A1. Location: the sdk. `sdk/dns_presets.go` holds the catalog data and the
expansion into resolver settings, and is built for every target, the iOS
packet tunnel extension included, because the device derives the resolver
lists from the stored selection when it applies them (A6). The catalog is a
few dozen strings, well inside the extension's size budget.
`sdk/dns_presets_ui.go` (`//go:build !ios_extension`) holds what the screens
call, as `control_doh_ui.go` and `transport_settings_view.go` do.

Why not connect: the catalog is product policy (which providers are offered,
how an ID is entered) that changes with app releases. connect needs only
generic mechanisms (B), as it already has for the regional data the sdk
surfaces. The one place connect would otherwise need the list is the
blocker generator's resolver protection, which subtracts the default
resolvers from the blocker's tables (`blocker/main.go:766`); for presets the
ignore list covers it at run time (A6).

A2. Data model. Internal to the sdk:

```go
// A provider whose resolvers the catalog offers. The name is a proper noun
// and is not localized.
type dnsPresetProvider struct {
	providerId string // stable: "adguard"
	name       string // "AdGuard DNS"
	helpUrl    string // the provider's setup or product page
	privacyUrl string
	termsUrl   string
	// where a user finds a personal configuration id; "" when the provider
	// has none
	personalIdHelpUrl string
}

// One selectable resolver: a provider's variant.
type dnsPreset struct {
	presetId   string // stable and stored on devices: "adguard-adblock"
	providerId string
	variant    string // dnsPresetVariant...
	// The DoH url. A personal preset holds exactly one "{id}" path segment.
	dohUrl string
	// The addresses dohUrl's host is dialed at. The host is never looked up
	// (B1). Empty only when the host is an ip literal whose certificate
	// carries the address (B6).
	dohPinIpv4s []string
	dohPinIpv6s []string
	// Informational in this plan (shown on the help sheet, kept for a future
	// DoT or DoQ upstream): the DoT host ("{id}" for a personal preset) and
	// the plain resolver addresses.
	dotHost    string
	plainIpv4s []string
	plainIpv6s []string
	// nil when the preset takes no personal id
	personalId *dnsPresetPersonalId
	helpUrl    string
	// the date the endpoints were last checked live (F3), as the regional
	// presets record theirs (net_http_doh_regional.go:49-53)
	checked string
}

type dnsPresetPersonalId struct {
	required bool
	// anchored, ascii only, a single path segment (D4)
	pattern   *regexp.Regexp
	maxLength int
	// the dashboard urls a user may paste, each with one capture group for
	// the id (D3)
	pastedUrlPatterns []*regexp.Regexp
	// a placeholder in the provider's id format, e.g. "abc123"
	example string
}
```

Variants: `adblock` (ads and trackers), `family` (adult content, safe search
where the provider enforces it), `security` (malware and phishing only),
`unfiltered`, and `personal` (the user's own configuration at the provider).
The display name is the provider name plus the localized variant label, e.g.
"AdGuard DNS · Ad blocking".

Reserved ids, not catalog entries:

- `default`: URnetwork's built-in lists, derived from
  `connect.DefaultUpgradeMuxSettings` when applied, so a device on the
  default follows later default changes (E4).
- `custom`: the explicit lists of the advanced editor, the behavior of every
  record today. Stored as an empty `PresetId`.
- `regional-<cc>`: the existing regional recommendation as a pick (O6),
  derived from `connect.RegionalDnsResolverSettings`. It is labeled
  unencrypted.

A3. Candidate providers. The request names AdGuard DNS and Control D. The
research decides the rest; the list below is the research's starting set,
not a recommendation. Every cell is `TODO(research)`.

| Provider | Variants | DoH url | Pins v4 / v6 | Personal id form | Blocking answer | Terms for listing |
|---|---|---|---|---|---|---|
| AdGuard DNS | TODO(research) | TODO(research) | TODO(research) | TODO(research): AdGuard private DNS path id | TODO(research) | TODO(research) |
| Control D | TODO(research) | TODO(research) | TODO(research) | TODO(research): custom resolver path id | TODO(research) | TODO(research) |
| NextDNS | TODO(research): personal only? | TODO(research) | TODO(research) | TODO(research): configuration id, device-name suffix | TODO(research) | TODO(research) |
| Mullvad DNS | TODO(research) | TODO(research) | TODO(research) | none | TODO(research) | TODO(research) |
| Quad9 | TODO(research): security; Quad9 is a default operator, whose malware NXDOMAINs the default fan-out deliberately outvotes (3.2 step 5) | TODO(research) | TODO(research) | none | TODO(research) | TODO(research) |
| Cloudflare for Families | TODO(research) | TODO(research) | TODO(research) | none | TODO(research) | TODO(research) |
| Others the research finds | TODO(research) | | | | | |

The research must also answer, per provider: whether the DoH host's
certificate carries the pinned addresses as IP SANs and the provider supports
ip-literal DoH (B6); HTTP/2 and RFC 8484 GET support; IPv6 coverage; per
source-address rate limits, which matter because one exit address serves many
URnetwork users; how HTTPS/SVCB (type 65) queries for a blocked name are
answered, since hints could bypass a null A answer; whether queries are
logged and for how long; EDNS Client Subnet behavior; and the provider's
terms for being listed in a commercial app (H3).

A4. Catalog rules, enforced by sdk tests (6.1):

- ids are unique, lower case, and never reused. A removed preset's id stays
  reserved (A7).
- Each `dohUrl` is https with no user info, query or fragment, and a path.
  A hostname host has pins; an ip-literal host has none.
- Pins parse as addresses of the list's family. A provider with IPv6 has at
  least one v6 pin.
- A personal preset's url has exactly one `{id}` filling one whole path
  segment; its pattern is anchored and admits only characters that need no
  escaping in a path (D4).
- No preset is `default`, and none is preselected or ordered as a
  recommendation (H1).
- Every entry has a `checked` date (F3).

A5. The surface the apps call (gomobile, cgo and JS), in `dns_presets_ui.go`:

```go
type DnsPresetProvider struct {
	ProviderId        string
	Name              string
	HelpUrl           string
	PrivacyUrl        string
	TermsUrl          string
	PersonalIdHelpUrl string
	Presets           *DnsPresetList
}

type DnsPreset struct {
	PresetId   string
	ProviderId string
	Variant    string
	// whether the preset takes a personal id, and whether it must have one
	PersonalIdAccepted bool
	PersonalIdRequired bool
	PersonalIdExample  string
	HelpUrl            string
}

// The providers in display order (O9), each with its presets.
func GetDnsPresetProviders() *DnsPresetProviderList

// nil for an unknown id.
func GetDnsPreset(presetId string) *DnsPreset

// The id a user typed or pasted, trimmed, with the id taken out of a pasted
// dashboard url of this preset's provider. Input it cannot read is returned
// trimmed, for the validator to reject.
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

// What the summary row shows for settings, given the exit's country: the
// preset and provider ids, the variant, whether the selection is custom,
// retired (A7) or unencrypted (regional, or custom plain DNS), and which
// pill to show, if any (F2).
func DescribeDnsResolverSettings(settings *DnsResolverSettings, exitCountryCode string) *DnsSelectionSummary
```

The editors never build a `DnsResolverSettings` from scratch: they start
from one of these helpers, so a field an app does not model survives its save
(3.8, E3).

Error ids, which are localization keys as the bootstrap DoH ones are
(`control_doh.go:38-46`): `dns_preset_error_unknown`,
`dns_preset_error_personal_id_required`,
`dns_preset_error_personal_id_invalid`,
`dns_preset_error_personal_id_other_provider` (a pasted url of another
provider), `dns_preset_error_personal_id_not_accepted`.

A6. Expansion: what a selected preset becomes in connect terms. The device
derives it each time it applies the settings. One conversion does it:
`DnsResolverSettings.toConnect()` (`device_local.go:8266-8288`) derives a
preset's lists and attaches the catalog pins. The mux settings
(`upgradeMuxSettingsWithDnsResolverSettings`, `:8303-8324`) and the socket
resolver (`socket.go:147-160`) already go through it; the host fallback
projection (`hostFallbackDnsResolverSettings`, `:8330-8350`) builds a new
struct from the result and must copy the pins. The result:

- `EnableRemoteDoh` true; `EnableLocalDoh`, `EnableRemoteDns` and
  `EnableLocalDns` false.
- `RemoteDohUrlsIpv4` and `RemoteDohUrlsIpv6` both hold exactly the preset's
  url with the personal id filled in. The lists are exclusive: no default
  operator is added, because a hedge to an unfiltered server would return the
  names the preset blocks (3.2 step 5). The same hostname url in both lists
  is queried once (`dualStackList` drops repeats) and works on a v4-only tun
  (`tun.go:1039-1049` drops only the v6 list) and with `IpVersion` 6.
- `DohServerHostAddrs` maps the url's host to its pins (B1).
- `RemoteDnsIpv4` and `RemoteDnsIpv6` empty. With pins nothing needs them,
  and empty lists make a missing pin fail closed instead of falling back to a
  plaintext lookup (B3).
- `DnsUpgradeMaskAddress` and `EnableFallback` as the user has them.
- The ignore list (`dnsIgnoreHostValues`, `device_local.go:8427-8472`) also
  gets the pins, so block actions and the built-in blocker never capture
  resolver traffic, whatever the blocker's tables hold.

Pins follow the host, not the selection: whenever the device applies
resolver settings, preset or custom, every DoH url whose host the catalog
knows gets that host's pins. A copy of preset settings saved as custom (an
editor that rebuilds the struct, 3.8) therefore keeps working with no
plaintext lookup, and a user who types a known provider's url into the custom
lists gets the same path. A hostname the catalog does not know keeps today's
lookup through the tunnel (3.2 step 8): that is the custom editor's existing
behavior, not a newly offered resolver.

The stored record keeps the derived lists as well as the selection. An older
sdk ignores the selection and uses the lists (E4).

A7. Catalog changes reach devices with a release, because the device derives
the lists from the current catalog at every apply. A stored preset id the
catalog no longer knows keeps working from the stored lists; the summary
marks it retired and the DNS screen asks the user to choose again. It never
switches silently (O8).

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

B2. Through the tunnel. A preset is queried by the mux's tun, the remote DoH
path, exactly as the defaults are (3.2 step 4). The resolver sees the exit
provider's address, answers match the exit's location (the 2026-10-02
rationale for tunnel-only DNS), and the local network sees only tunnel
traffic. The one host-network path is Fast DNS on connect, when the user
turned it on (C4).

B3. No plaintext. Preset settings have remote plain DNS off with empty
lists, and pins for every hostname. The resolver's name is therefore never
looked up in plaintext on the tun path and never through the OS resolver on
the host path. When every pin fails, the request fails.

B4. Sink answers. An answer whose addresses are all unspecified
(`0.0.0.0`, `::`) is how most filtering resolvers say "blocked"
(TODO(research): each provider's blocking answer). The mux delivers it to the
client but does not record it in the reverse index or bind it to an exit:
skip `reverse.record` in the reply (`ip_mux_upgrade.go:1852-1854`) and the
`DohResultCallback` in `DohCache.resolve` (`net_http_doh.go:1181-1189`). This
matches the built-in blocker's rule (`ip_mux_upgrade.go:1658-1660`); without
it every blocked name lands on one reverse-index entry for `0.0.0.0`. The
answer is still cached for its TTL; a preset change swaps the cache (3.2
step 10). HTTPS and SVCB queries are forwarded opaquely (3.2 step 3), so a
provider that blocks a name's A record but returns address hints in its
HTTPS record lets browsers connect anyway; such a provider is not shipped
(TODO(research), A3).

A provider that answers REFUSED for blocked names makes the mux treat every
block as a resolver failure: no answer, a client timeout instead of a fast
block, and failures that push the path cold (3.2 step 7). Presets ship only
with a null-address or NXDOMAIN blocking answer (TODO(research)); for personal
configurations, where the user picks the block response at the provider, the
help text names the responses that work (O11).

B5. IPv6. Pins carry both families where the provider has them. The tun dial
races them; a v4-only tun resolves only A (`tun.go:1632-1636`).

B6. Bootstrap alternatives considered:

- A plaintext lookup through the tunnel (today's hostname rule, 3.2 step 8):
  rejected by the safety constraint.
- An encrypted lookup through the default DoH servers: it sends the
  resolver's name to an operator the user did not choose, and pins make it
  unnecessary while they are current. Left as O5.
- Ip-literal urls: used where the provider's certificate carries the address
  as an IP SAN and the provider supports ip-literal DoH
  (TODO(research)), as the defaults do. They send no SNI. Otherwise a
  hostname url with pins, which works whatever the certificate's SANs and
  sends the SNI the provider expects for a variant.

B7. Logs carry a DoH url as scheme and host only. Error strings that include
the url (`net_http_doh.go:2033,2055,2059,2063`) and the warm failure line
(`:856,871`) go through one helper, so a personal id in a path never reaches
a log or a feedback upload. Dial observation lines already log only
host:port (`net_http_doh_observation.go:258-266`), which for a path id holds
no secret.

### C. Failure behavior

C1. Fail closed. When the preset does not answer, the mux sends no answer,
as today; cached names can be served up to five minutes stale; the cold
prober keeps retrying the preset with backoff. Nothing falls back to the
default servers, to plaintext, or to the host network unless Fast DNS on
connect is on.

C2. Encrypted stays encrypted, by B3, with tests that fail if any plaintext
or OS lookup happens for a preset (6.2).

C3. Detection. The mux reports resolver health from the state it already
keeps (`ip_mux_upgrade.go:306-316`: consecutive failures and last success
per resolver generation): new `UpgradeMux.DnsResolverHealth()` returning the
consecutive failure count, the last success time and whether the path is
cold. The sdk publishes `DnsResolverStatus` (`GetDnsResolverStatus` and a
change listener, across DeviceLocal, DeviceRemote, the rpc and JS) with
states `ok`, `unknown` and `failing`. It reports `failing` only while the
window has providers added and the resolver has failed at least
`DnsResolverFailingMinCount` resolutions in a row over at least
`DnsResolverFailingMinDuration` (settings fields, per CODESTYLE "Tunables
live in settings"), so a forming tunnel is not read as a resolver outage.

C4. Fast DNS on connect with a preset. The host projection
(`hostFallbackDnsResolverSettings`) queries the preset from the user's own
network address, with the pins (B1). That keeps filtering consistent during
connect, and it shows the user's real address to the provider, which for a
personal configuration ties the address to the user's account at the
provider. The DNS screen says so next to the toggle while a preset is
selected (O3).

C5. What the user sees. The DNS row shows the selection; while `failing` it
shows "<Provider> isn't answering" with two actions: "Use URnetwork DNS"
(selects `default` in one tap) and "Change location". Dismissing keeps the
preset. No local notification: this is not a disconnect-class event (compare
the out-of-balance rule, which alerts but never disconnects). Nothing
switches automatically.

C6. Regional blocks. Because the preset is dialed through the tunnel, its
reachability depends on the exit's network, not the user's: a user in a
censoring country reaches the provider from an exit elsewhere. An exit whose
country blocks the provider (TODO(research): known blocks per provider) shows
`failing`, and the screen suggests another location, `default`, or the
regional pick for that country. The regional recommendation stays an explicit
choice, labeled unencrypted.

### D. Personal IDs

D1. Path ids only. In a DoH request the path travels inside TLS, so neither
the exit provider nor anyone on path sees the id. A hostname id (the DoT
form, or a DoH host such as `<id>.example`) is in the TLS ClientHello's SNI,
which the exit provider, an untrusted third party, can read and replay.
Hostname ids are not accepted (TODO(research): confirm each provider offers a
path form for DoH; a provider without one gets no personal preset).

D2. Never linked-IP. The egress address belongs to the exit provider,
changes with the location, and is shared with other users of that provider:
linking it would let strangers query under the user's configuration and
would break at the next exit. The help text says so, since providers'
dashboards suggest linking.

D3. Input. One text field, "Configuration ID", under a provider with personal
configurations. For a provider whose personal preset is the only one
(TODO(research): NextDNS), it is required; elsewhere a "Use my own
configuration" pick reveals it. The field accepts the bare id or a pasted DoH
url from the provider's dashboard; `NormalizeDnsPresetPersonalId` extracts the
id. Errors show inline from the error id as the user types. The placeholder
shows the provider's format, and "Where do I find this?" opens
`personalIdHelpUrl`.

D4. Validation in the sdk:

- trimmed; the provider's case rule applied (TODO(research));
- the anchored pattern of the preset and its maximum length
  (TODO(research): each provider's charset and length);
- ascii only, and never `/`, `.`, `%`, `?`, `#` or whitespace, whatever the
  pattern says, so the id cannot leave its path segment;
- a pasted url must match one of the provider's dashboard url patterns,
  host and path shape, exactly; a url of another catalog provider gets
  `dns_preset_error_personal_id_other_provider`;
- the built url is parsed again and must have the template's scheme, host
  and path with the id in place.

A device-name suffix (TODO(research): NextDNS supports one) is not accepted
in this plan: it would send the device's name to the provider (O4).

D5. Secrecy. A personal id lets anyone send queries under the user's
configuration (TODO(research): what else it grants at each provider). It is
never logged (B7), never sent to URnetwork's servers, stored only in the
local record (wiped at logout), and crosses only the local device rpc. The
summary row shows "personal", not the id. The derived urls in the settings
carry it too; nothing logs a settings value today (the rpc logs only the
event name, `device_rpc.go:14466`), and the 6.1 log test keeps it that way.

### E. Settings storage and sync

E1. `sdk.DnsResolverSettings` gains two fields:

```go
// The selected preset (dns_presets.go), "default" for the built-in lists,
// or empty for the explicit lists below (the custom editor). When set, the
// device derives the lists from the catalog and ignores the ones given.
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

E3. Every crossing carries both fields:

- the record codec (`local_state.go:603-626`);
- `cloneDnsResolverSettings` (`device_local.go:8390-8397`), which must copy
  them instead of relying on the connect round trip;
- the read-back (`dnsResolverSettingsWithLock`, `:8406-8413`), from a
  DeviceLocal field that holds the selection beside `upgradeMuxSettings`,
  since connect's settings do not carry it;
- `DnsResolverSettingsRpc`, `newDnsResolverSettingsRpc` and
  `toDnsResolverSettings` (`device_rpc.go:8293-8355`), and the sync apply
  (`:10602-10606`). Gob ignores fields an older peer lacks;
- the hand-written JS mapping (`js/device_remote.go:905-963`,
  `js/src/types.ts:979-1002`), for completeness: the web cannot set a hosted
  device's resolver (3.9);
- the cgo headers and packaging bindings, regenerated and committed in the
  sdk (`make generate`); windows and linux fetch them at build;
- the app editors: apple's draft (`DnsSettingsStore.swift:72-89`) and its
  `LocalState` mirror, android's `apply` (`DnsSettingsViewModel.kt:190-218`),
  windows' `Draft` and `DnsSettingsEquivalent`, linux's `SettingsEqual`
  (3.8), each moved onto the A5 helpers.

A test fills every field and requires all of these sdk paths to return it
(6.1), as the reliability round trip test does.

E4. Migration. The record gains `PresetVersion` beside
`FallbackOptInVersion`. A record without it whose lists equal one of the
known historical default sets (the lists before and after
`fix/no-google-dns`; TODO(no-google): the exact sets) loads as
`PresetId: default`, so default users follow later default changes, Google's
removal included. Any other record loads as custom. A record written by an
older sdk after a newer one wrote it (a downgrade, then an upgrade) is read
the same way.

E5. Downgrade. An older sdk ignores `PresetId`, uses the stored lists and has
no pins: with a hostname preset, its resolver cannot look up the host
(remote plain DNS lists are empty), so DNS fails closed until the user
re-selects. This is accepted (section 9).

E6. Hosted devices keep refusing resolver settings; the apps hide the picker
for them. Logout wipes the record, personal id included. No server-side
sync (N6).

E7. The hosted proxy API's JSON gains `doh_server_host_addrs` through
connect's struct. Old proxies ignore it; nothing provisions it in this plan
(N7).

### F. Per-platform UI

F1. Layout, the same on every app, with content and rules from the sdk:

1. "DNS provider" list. "URnetwork (default)" first, then the providers in
   the catalog's display order, each with its variants as a segmented
   control or chips (Ad blocking, Family, Security, Unfiltered, Personal),
   then "Custom" (the existing editor), then "Regional (<country>)" when the
   sdk has a recommendation (O6).
2. The personal id field (D3) under a provider with personal configurations.
3. Under the selected provider: "Your DNS lookups go to <Provider> through
   the tunnel. <Provider>'s privacy policy applies.", with "Privacy policy"
   and "Help" links (H2).
4. The status line while `failing`, with its actions (C5).
5. The existing toggles stay: Fast DNS on connect, with the preset note
   (C4), and the built-in blocker, labeled as URnetwork's own blocker. Both
   blockers can be on: the built-in one answers first (3.6).
6. A footnote that changes apply to new lookups at once, and apps may keep
   cached answers for about a minute (synthesized answers carry
   `ResponseTtl` 60 s, `ip_mux_upgrade.go:157`).

The summary row wherever DNS is shown today: "DNS: URnetwork", "DNS: AdGuard
DNS · Ad blocking", "DNS: NextDNS · Personal", "DNS: Custom", "DNS: Regional
(unencrypted)", from `DescribeDnsResolverSettings`.

F2. Per platform. The picker goes at the top of the existing Custom DNS
editor (3.8) on every app, under the connection region row and the "Traffic
DNS" heading. On all four apps:

- The defaults panel ("Restore to most secure settings") becomes the
  "URnetwork (default)" pick and the regional panel the "Regional" pick. The
  four resolver toggles, the suggested servers and the four lists move under
  "Custom", seeded by `DnsResolverSettingsAsCustom`.
- The card keeps its rows but leads with the summary line; the pill comes
  from `DescribeDnsResolverSettings` instead of each app's whole-value
  equality. Its rule: no pill while a preset is selected (an explicit,
  encrypted choice; failures show as status, C5); for `default` and custom,
  today's rules (the exit country's recommendation first, then the
  defaults). Without this, every preset shows "The default safe settings are
  not applied".
- The editors build their saves from the A5 helpers, never a fresh struct.
- Provider names, links and the personal id format come from the sdk, which
  also suits android's `LocalizedUiTextTest` (runtime values, not literals).

Apple: the picker goes in `DnsSettingsView.swift`; `DnsSettingsStore` drops
its fresh-object `toSdk()` (`:72-89`) and its `LocalState` mirror carries
the new fields through the sdk record codec. The extension needs nothing
beyond the sdk, since it derives the lists on apply. iOS DNS profiles and
per-app DoH bypass the mux (section 9); the app shows no notice for them
today.

Android: the picker goes in `DnsSettingsScreen.kt`; `DnsSettingsViewModel.apply`
(`:190-218`) builds from the helpers and reports whether anything was
applied. The existing strict Private DNS notice (`ConnectActions.kt:628-660`)
also explains why a preset seems to do nothing; it gains a line naming the
selected preset.

Windows: the picker goes in `DnsEditorSheet`; its `Draft`
(`StatsSheets.h:274-289`, `StatsSheets.cpp:1259-1310`) carries the new fields
or keeps the sdk struct whole as linux does, and `DnsSettingsEquivalent`
(`ConnectPage.cpp:82-98`) gives way to the sdk summary. Strings can ship
through `pages::Adv` until the store has them.

Linux: the picker goes in `DnsSheet`; `SettingsEqual` (`DnsSheet.cpp:53-65`)
compares the new fields. The GUI applies its stored DNS settings when it
binds to the daemon's device, as it does transport settings
(`SdkHost.cpp:3148-3180`), so a pick made while disconnected reaches the
tunnel. `T_` English must match the store byte for byte.

Web and extension: no picker (3.9, N7). If hosted presets come later, they
ride `initial_device_state.dns_resolver_settings` at connect, which the sdk's
`ProxyDeviceState` would have to model first; the proxy's tun resolves with
those settings and the pins work there unchanged (B1).

F3. Catalog verification. Before a catalog change merges, a Go program
(`sdk/dnspresetcheck`, per CODESTYLE's rule for helpers) queries every
preset live: RFC 8484 GET over HTTP/2 at each pin, certificate verified
against the host, both families, a known name resolving, and the provider's
documented test name blocked. Results and the date go into each entry's
`checked`. It is a manual tool, never a unit test (tests touch no network).

### G. Localization

G1. New keys in `localizations/keys/`, one file each, snake_case, tagged
for android, apple, windows and linux (draft names, in the store's `dns_*`
family):

- `dns_provider_title`, `dns_provider_default`, `dns_provider_custom`,
  `dns_provider_regional` (with the country);
- `dns_variant_adblock`, `dns_variant_family`, `dns_variant_security`,
  `dns_variant_unfiltered`, `dns_variant_personal`;
- `dns_personal_id_label`, `dns_personal_id_use_own`,
  `dns_personal_id_where`, `dns_personal_id_linked_ip_warning`;
- `dns_preset_disclosure` (provider name), `dns_preset_privacy_link`,
  `dns_preset_help_link`, `dns_preset_fast_dns_note` (provider name),
  `dns_preset_retired` (provider name), `dns_preset_apply_note`;
- `dns_status_failing` (provider name), `dns_status_use_default`,
  `dns_status_change_location`;
- the error ids of A5.

G2. Provider names and product names are not translated. Variant labels are.

G3. English ships first in the apps; the other languages land through the
store before the stable releases (section 7). A live key needs all 28
languages (`localizations/gen/store.mjs:267-273`). The release build
regenerates the app files, so the store change is the only edit.

### H. Privacy and legal

H1. Opt-in only. No preset is a default, preselected, promoted or ordered as
a recommendation; the provider order is neutral (O9). Existing users keep
what they have.

H2. Disclosure before use: the selected provider's line (F1 item 3) is shown
on the screen where the pick is made, with the provider's privacy policy.
What the provider receives: every name looked up while connected, its
timing, the exit provider's address (the user's own address only with Fast
DNS on connect, C4), and for a personal preset, the configuration id.
TODO(research): each provider's query logging and retention, and EDNS Client
Subnet behavior.

H3. Provider terms. TODO(research), per provider: whether its public
resolvers may be listed as presets in a commercial app; whether a free tier
limits volume (TODO(research): NextDNS's free tier); per source-address rate
limits, given shared exit addresses; trademark use. Names only, no logos,
unless a provider grants it (O10). No affiliate or referral parameters.

H4. URnetwork sends nothing new to its servers: the selection stays on the
device (N9).

### I. Observability

I1. No queried names in logs or telemetry, as today: errors strip the
request url (`net_http_doh.go:2048-2056`), and observation lines carry the
dial path, outcome and address.

I2. Logs name a DoH server by scheme and host only (B7). The app log records
the applied selection, `[dns]preset=<id> personal=<yes|no>`, which a
feedback upload carries for support; never the id itself.

I3. The resolver status (C3) is local. A per-session count of sink answers
("lookups blocked by <Provider>") is possible from B4 and is left as O12.

I4. No server-side counters of preset use (N9).

## 5. Wire, storage and API changes

| Surface | Change |
|---|---|
| connect `DnsResolverSettings` (Go and JSON) | `DohServerHostAddrs map[string][]string`, `doh_server_host_addrs`, omitempty |
| connect `UpgradeMux` | `DnsResolverHealth()` |
| connect logs | DoH urls as scheme and host |
| sdk `DnsResolverSettings` (gomobile, cgo, JS) | `PresetId`, `PresetPersonalId` |
| sdk `.dns_resolver_settings` record | the two fields, `PresetVersion` |
| sdk `DnsResolverSettingsRpc` (gob) | the two fields |
| sdk new types and functions | A5, `DnsResolverStatus`, `GetDnsResolverStatus`, `AddDnsResolverStatusChangeListener` |
| sdk new settings fields | `DnsResolverFailingMinCount`, `DnsResolverFailingMinDuration` |

All changes are backward compatible: an older reader ignores the new fields
and behaves as today (with the downgrade limit of E5).

## 6. Tests

All deterministic, no network, no sleeps as proof (CODESTYLE "Tests"). No
app test suites (owner rule, 2026-11-03): the logic and its tests live in
connect and the sdk.

### 6.1 sdk

- Catalog integrity (A4), table-driven over every entry.
- Url building and validation: bare ids, pasted dashboard urls, whitespace,
  case, a pasted url of another provider, `/`, `..`, `%2F`, `?`, `#`,
  non-ascii, over-long, empty with a required id, an id for a preset that
  takes none.
- Expansion (A6): exclusive lists (no default operator present), pins set,
  remote plain DNS off and empty, the fallback projection carrying pins, the
  ignore list carrying host and pins.
- Pins follow the host (A6): custom settings that name a catalog host get
  its pins; preset settings copied as custom with a fresh struct, as today's
  editors build them, still carry no plaintext lookup.
- Helpers (A5): each of `DnsResolverSettingsWithPreset`,
  `DnsResolverSettingsWithFallback`, `DnsResolverSettingsAsCustom` and
  `CopyDnsResolverSettings` keeps every field it does not set (the
  fill-every-field fixture below).
- The pill (F2): none while a preset is selected; the recommendation pill for
  `default` or custom when the exit country has a recommendation that is not
  applied; the defaults pill for custom settings that differ from the
  defaults; none for `default` elsewhere.
- Round trips (E3): a test fills every `DnsResolverSettings` field with a
  distinct value and requires it back through set and get on DeviceLocal,
  the record codec, `cloneDnsResolverSettings`, the rpc mirror and gob, and
  DeviceRemote set and get; extend `device_rpc_mirror_test.go` to cover
  `DnsResolverSettingsRpc`.
- Migration (E4): each historical default set loads as `default`; a
  near-default (one operator removed) loads as custom; a record with an
  unknown preset id keeps its lists and reads retired (A7).
- Refusals (E2): an unknown preset id or an invalid personal id is neither
  saved nor applied, and the previous resolver stays.
- Status (C3): `failing` only with providers added and both thresholds met;
  recovery returns to `ok`; a forming window stays `unknown`. Thresholds from
  settings, clock injected.
- Hosted devices refuse a preset like any resolver settings.
- Logs: applying a personal preset writes no id to the sdk log.

### 6.2 connect

- Pins, tun path: a hostname DoH url with pins resolves through an
  `httptest` TLS server whose certificate names `dns.example`, with a fake
  plain-DNS server (`newFakeDnsServer`, `net_http_doh_modes_test.go:86`)
  that must see zero queries.
- Pins, host path: the local DoH client dials the pins; a host resolver that
  fails the test if called.
- Pins fail closed: every pin refused by the tunnel dialer; no plain-DNS
  query, no OS lookup, no request to any other url; the resolution fails.
- TLS still verifies: a pin pointing at a server with a certificate for
  another name fails.
- Family: a v4-only pin set answers AAAA empty and the dial uses v4.
- Exclusive filtering: in `newDnsClientHarness`
  (`ip_mux_upgrade_test.go:169`) with a fake filtering server that answers
  `ads.example` with `0.0.0.0`, the client gets `0.0.0.0`; with a second,
  unfiltered server in the list, the test documents that filtering is lost,
  which is why A6 forbids the mix.
- Sink answers (B4): `0.0.0.0` and `::` answers reach the client, enter no
  reverse-index entry and fire no `DohResultCallback`; a mixed answer
  (`0.0.0.0` with a real address) is recorded as today.
- Log redaction (B7): a failing personal-path url's warm failure line and
  error strings contain no path.
- Health (C3): consecutive failures and last success move as resolutions
  fail and succeed, per generation; a settings swap resets them.

### 6.3 Regional failure

- A dialer that refuses the preset's pins (an exit network blocking the
  provider): no fallback to the defaults, no plaintext, the sdk status turns
  `failing` after the thresholds, and turns `ok` when the dialer heals and
  the cold prober succeeds (backoff from settings, clock injected).
- The same with Fast DNS on connect: the host projection queries the preset
  at its pins and nothing else.
- A regional pick (`regional-cn`) expands to exactly
  `RegionalDnsResolverSettings("cn")`.
- Ingress is untouched: with any preset selected, the control-name DoH
  settings equal `IngressDohSettings` for the space (existing
  `TestIngressEgressDnsSeparate`, `net_http_doh_bootstrap_test.go:135`,
  extended).

### 6.4 Apps

App suites are not run (owner rule, 2026-11-03); the app changes land after
a careful read. The apps' source-pinning wiring tests move with the editor
code they pin and are updated in the same change: windows
`tests/editor_apply_test.go` (`TestEditorApplyWiring`) and
`tests/dns_settings_test.go`; linux `EditorSafetyWiringTest.cpp`,
`FastDnsWiringTest.cpp` and `ConnectFoldDoorsWiringTest.cpp`; apple
`DnsResolverSettingsMirrorTests` and `FastDnsOnConnectTests`; android
`SettingsEditorWiringTest` and `FastDnsOnConnectToggleTest`. The pill rule
and the editor rules have their deterministic tests in the sdk, behind
`DescribeDnsResolverSettings` and the A5 helpers.

## 7. Rollout

1. `fix/no-google-dns` merges first. E4 needs its default sets.
2. connect: B1, B4, B7, C3, with 6.2. Inert until a resolver setting carries
   pins. A connect merge reaches the provider fleet with the next finished
   nightly; providers run no `UpgradeMux`, so nothing changes for them, but
   the merge is still a miner release and is timed like one.
3. sdk: the catalog (after the research and an F3 run), the selection fields
   and A5 helpers, derive on apply, migration, status, rpc and the JS
   mapping, with 6.1 and 6.3; then `make generate` for the cgo headers and
   packaging bindings. Also reaches providers with the next nightly, inert
   there.
4. Apps: apple, android, windows and linux (F2), with English strings, each
   on a branch of the same name per repo. App builds reach testers in the
   nightlies; stable releases are manual per app repo.
5. Localizations: the G1 keys in all 28 languages, then the stable releases.
6. Help: a DNS providers section in `mmm/ur.io/docs` with an explicit
   `{#dns-providers}` anchor so translations keep it, the tours' stale "Local
   DNS fallback" renamed, and a link from the screens, as the apps link
   `https://ur.io/docs/api`. ur.io deploys are manual.

Later, outside this plan: presets for hosted proxies through
`dns_resolver_settings`, an encrypted re-resolution when pins fail (O5),
DoT or DoQ upstreams.

## 8. Open decisions for the owner

- O1. Which providers and variants ship first. Recommendation after the
  research (TODO(research)).
- O2. Failure policy: fail closed with a status and a one-tap "Use URnetwork
  DNS" (recommended), or fall back to the defaults automatically.
- O3. Fast DNS on connect with a preset: query the preset from the user's
  network (recommended, with the note of C4), use the defaults for the
  fallback, or turn the fallback off while a preset is selected.
- O4. Personal ids in the first release (recommended for NextDNS, Control D
  and AdGuard if the research confirms path forms), and whether to accept a
  device-name suffix (recommended no).
- O5. Pins only (recommended), or also an encrypted re-resolution of the
  resolver's name through the default DoH servers when every pin fails.
- O6. The regional recommendation as a pick in the same list (recommended),
  or kept where it is today.
- O7. Presets for hosted proxies through the proxy API, later.
- O8. A preset removed from the catalog: keep it working from the stored
  lists with a notice (recommended), or move the user to the default.
- O9. Provider order: alphabetical (recommended, neutral) or another order.
- O10. Provider names only, no logos (recommended), unless a provider grants
  logo use.
- O11. REFUSED from a preset resolver: a failure, as today (recommended), or
  a block, which would also hide real outages and rate limiting.
- O12. A local "lookups blocked" count on the DNS screen.

## 9. Known limitations

- Apps that do their own DoH (some browsers), Android Private DNS in strict
  mode and iOS DNS profiles bypass the mux and the preset, as they bypass the
  built-in blocker (sdk `BLOCKER.md:178-182`).
- OS and browser caches keep earlier answers for about a minute after a
  switch.
- Pins can go stale between releases; DNS then fails closed until an update.
- Exit addresses are shared, so a provider's per-address rate limits can
  apply to many users at once (TODO(research)).
- A third party's blocks cannot be overridden on the device; a personal
  configuration manages its allowlist at the provider.
- The status cannot tell "provider down" from "exit network blocks the
  provider".
- Downgrading to an sdk without pins while a hostname preset is selected
  fails DNS closed (E5).
- The web and the extension offer no presets: they control hosted devices,
  which run no DNS mux and take no resolver settings after connect (3.9).

## 10. Research TODO index

Every `TODO(research)` above, to fill from
`factury/reports/Ad blocking DNS resolver presets.md`, citing the report:

1. The provider list and variants (A3, O1).
2. Per preset: DoH url, pins v4 and v6, DoT host, plain addresses, help,
   privacy and terms urls (A2, A3).
3. IP SANs and ip-literal DoH support (B6).
4. HTTP/2 and RFC 8484 GET support (A3).
5. Blocking answer per provider, and HTTPS/SVCB handling of blocked names
   (B4, O11).
6. Personal id: path form, charset, length, case rule, dashboard url shapes,
   device-name suffix, what the id grants (D1, D3, D4, D5).
7. Rate limits per source address and free-tier limits (A3, H3).
8. Logging, retention and EDNS Client Subnet (H2).
9. Terms for listing in a commercial app; trademark use (H3, O10).
10. Known blocking of each provider by country (C6).

`TODO(no-google)` (3.1, E4) is filled once `fix/no-google-dns` lands.
