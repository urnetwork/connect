# GEOMAP — one geolocation, refined by our own pings

Where every provider and extender *is*, from one source of truth (MaxMind
GeoLite2), one source of ip quality (our operator probes), and one source of
refinement (our own signed, co-signed pings). This document tracks the work:
what is built, what comes next, and the decisions to settle before code.

> **Status (2026-09-24):** Steps 1–7, the extender admission limits
> (`EXTENDER.md` A12) and the scale work (D26, D27) are built and tested,
> as §9 records; every decision in §8 is confirmed through D28. Committed
> on 2026-09-24 on the `geomap` branch of each repository (connect, sdk,
> server, operator-proxy, config, xops; the ur.io docs follow). Not
> verified on this host for want of disk at the time: the gated
> fraction-of-target derive job and the gated ping row-size run (§5.8).

Companions: `DESIGNNOTES4.md` (the probe protocol as built), `EXTENDER.md`.

---

## 0. The goal, stated once

After this program, the network locates itself from exactly three things:

1. **GeoLite2** — city, region, country, continent, lat/lon, accuracy radius,
   ASN. Free, one license, one `geoipupdate` run. It is the *genesis* of every
   location: the prior we start from and never fully abandon.
2. **Our operator probes** — the egress prober (`operator-proxy/egresshealth`)
   that routes lookups through a provider and cross-checks them, for ip
   *quality* (hosting, proxy, mobile) and egress country.
3. **Our own pings** — provider→extender and extender→extender round trips,
   each signed by the pinger and co-signed by the target, which *refine* the
   genesis lat/lon by least squares (§5).

Everything else goes: the DB-IP and ipinfo packaged databases, the
`city-list.yml` snapshot, and the public `/ip` privacy verdicts we can no
longer back.

## 1. The ping work so far (built)

Built and merged 2026-09-18 across connect, sdk and server; the protocol is
specified in `DESIGNNOTES4.md`. In brief:

> **As-built correction (2026-09-23).** The *server* half of this table —
> the hint endpoint, the `ContinentCode` stamping, the latency ingest and its
> sweep — was reverted on server `main` on 2026-09-20 (`d53eadcd`, a clean
> revert of `0ebffe76`; the user confirmed on 2026-09-23 that it was not
> intentional; `f08616b1` had restored the migrations, so the tables exist).
> The connect and sdk halves stayed. Step 2 (§7) restores the commit in full,
> keeps `/network/extender-latency` accepting for one release (D14) and adds
> the §2.5 ingest beside it; the stats gauges of the last row are repointed
> at `network_ping`.

| Piece | Where | What it does |
|---|---|---|
| Probe service | `ExtenderHeader.Service = 3`; `extender/extender_probe.go` | A probe rides the ordinary `POST /` envelope, so an extender stays indistinguishable from a misconfigured CDN. Any client may probe to rank. |
| Provider attestation | `net_extender_probe_latency.go` | Only a **provider** identifies itself (`ProbeClientId`). The extender answers with a fresh nonce; the provider returns its RTT under its client-key signature over fixed-width signing bytes (`ur-extender-probe-v1` ‖ client id ‖ extender key ‖ nonce ‖ rtt ‖ timestamp). |
| Gate | `gateProbeAttestation` | The extender accepts only `claim ≥ observed − 20 ms` (one-sided: the pinger cannot claim to be closer than it was seen to be) and cannot alter the signed number. Accepted claims are batched to the operator by the **extender** (`ExtenderLatencyReporter`, `POST /network/extender-latency`). |
| Operator ingest | `server/controller/extender_proximity_controller.go` (reverted by accident; restored in step 2) | Verifies the provider signature against the key store, attributes the claim to an extender the reporting client activated, refuses self-attestation, dedupes on `(extender, provider, nonce)`. Rows: `network_extender_latency(extender_id, client_id, probe_nonce, rtt_ms, probe_time, create_time)`, 30-day retention. |
| Continent prior | `GET /network/extender-hint`; `ExtenderRecordBody.ContinentCode` (server side reverted; restored in step 2) | Records are stamped with the continent at signing; a client orders candidates hinted-continent → measured latency → the old order. DNS bootstrap infers the hint when the operator cannot place the client. |
| Probe pass | `net_extender_network.go` | Up to *n* probes per extender until *m* are close enough (factor × best, or under a floor). Samples are per process and age out; `LatencyMaxAge` 24 h. |
| Activation history | `network_extender_activation` | One row per extender activation: ip family, address hash (same /29·/56 peppered hash a provider connection keeps), country, city/region/country location ids. The history a ping is later placed against. |
| Observability | `stats_collector.go`, `grafana/dashboards/extenders.json` | Pings / providers / extenders pinged over 24 h, a per-extender pings counter, and the provider pings row. |

What the built system does **not** do, and §2 changes:

- The target never **co-signs**. Its acceptance is implicit in forwarding, and
  a target that silently drops a claim is indistinguishable from a network
  failure. The pinger holds no evidence of its own.
- Only providers ping. Extenders — the nodes with the most stable positions —
  do not measure each other, so the graph has no extender↔extender edges.
- The reporting party is the target, not the pinger, which is backwards for
  evidence: the party with something to hide is the one holding the pen.

## 2. Step 1 — extenders ping each other, and every ping is co-signed

### 2.1 When an extender pings

An extender subscribes to its directory (`ExtenderDirectory.Subscribe`, the
same feed the gossip node applies into). On every applied record whose
identity key it has not pinged — or whose address set changed — it schedules
one ping per address family the record lists.

- **Spread, not burst.** A record released to gossip reaches every extender at
  once; if all of them pinged the newcomer immediately, the newcomer's
  per-source probe limiter (1/s, burst 10) would refuse most of them and the
  measurement would be of a queue, not a path. Each new-peer ping is delayed
  uniformly over `PeerPingSpreadTimeout` (default 1 h).
- **Refresh.** Every known peer is re-pinged every `PeerPingRefreshTimeout`
  (default 12 h, never later than 24 h), jittered, so the map follows
  re-activations and moves and every ping in the operator's one-day window
  (§5.7) is renewed before it expires. An extender with *P* peers therefore
  makes ~2*P*/day pings; at a thousand peers that is one ping every ~45 s
  per extender. Providers keep the same 12 h cadence for their probe pass
  (`LatencyMaxAge` on the network client).
- **Bounded.** `PeerPingConcurrency` (default 2) and the existing
  `ProbeCountPerExtender` (2, lowest kept). Settings, not constants.
- **Records for itself.** The extender keeps its own latency map: the directory
  sample (`RecordLatency`, as today) plus a bounded in-memory ring of results
  with the co-sign verdict, exposed on the extender status. Nothing persisted:
  yesterday's path is not today's.

An extender pings a **bounded peer set**, not every extender it knows:
`PeerSampleSize` (default 64) peers, the nearest by the continent hint first
and the rest a random slice that rotates every refresh, so over days a
source's samples spread across the fleet while any one day's pinging stays
linear in the fleet's size — E extenders make at most 64·E terms, never
E(E−1). A provider's probe pass is already bounded at
`ProbeMaxCandidateCount` (16). The client directory is bounded the same
way (`EXTENDER.md` E, `MaxActiveRecordCount` 512, continent first, sampled
from gossip), which is what a phone can hold and what the peer set is drawn
from. A ping is an action under the extender's admission limits (`EXTENDER.md`
A12): a target that has admitted its per-instance or per-subnet share for
the minute answers 429, the pinger backs off at random and tries other
extenders first, and nothing is recorded — a limit is never a refusal and
never evidence.

### 2.2 Wire: the pinger identifies as an extender

The probe header gains `ProbeExtenderPublicKey` (bytes, 32). Exactly one of
`ProbeClientId` / `ProbeExtenderPublicKey` is set; the target refuses both.
The attestation gains `PingerExtenderPublicKey` (bytes, 32) beside
`ProbeClientId`, again exactly one set.

The two kinds sign under **different domains**, so a provider claim and an
extender claim can never be confused for one another and the provider
format already in the field is untouched:

```
provider:  "ur-extender-probe-v1"      ‖ client id (16) ‖ target key (32) ‖ nonce (32) ‖ rtt_ms (4 BE) ‖ timestamp_ms (8 BE)
extender:  "ur-extender-peer-probe-v1" ‖ pinger key (32) ‖ target key (32) ‖ nonce (32) ‖ rtt_ms (4 BE) ‖ timestamp_ms (8 BE)
```

An extender signs with its identity key (the same ed25519 key that signs its
challenge responses); a provider signs with its client key, unchanged.

### 2.3 The fourth message: the target's verdict

After reading the attestation the target always writes one frame back on the
same stream (the 4-byte length prefix every extender frame uses), then closes:

```
message ExtenderProbeVerdict {
    bool   Accepted     = 1;
    uint32 Reason       = 2;   // 0 ok; 1 rtt below observed; 2 nonce; 3 wrong extender;
                               // 4 unknown pinger; 5 bad signature; 6 rate limited
    bytes  Cosignature  = 3;   // ed25519 by the TARGET's identity key over
                               //   "ur-extender-probe-cosign-v1" ‖ the attestation's signing bytes
                               //   (domain included) ‖ the pinger's signature
}
```

The verdict is sent to every pinger, provider or extender. A provider built
before it closes the stream after its attestation and never reads it, which
costs nothing; a pinger built after it waits for it within its probe budget,
and an old target that sends none is recorded as `unknown`.

The co-signature commits to the *exact* claim including the pinger's own
signature, so the pair (claim, co-signature) is one object: nobody can pair a
co-signature with a different number. Its meaning is precise and limited:
**"on nonce N, I observed an interval the claim does not undercut."** It says
nothing about *who* the pinger is — see 2.4.

The pinger waits for the verdict within its probe budget. Three outcomes:

| Outcome | Recorded as |
|---|---|
| verdict `Accepted`, co-signature verifies under the target's key | `cosigned` |
| verdict refused, with reason | `rejected(reason)` |
| no verdict (timeout, close, network) | `unknown` |

An extender that never co-signs anything, or refuses far more than its peers,
is what the operator sees as "rejecting valid readings" — the evidence the
user asked for. `unknown` is kept separate so a flaky path is not read as a
refusing extender.

### 2.4 Who verifies what

- **Target (B)** verifies: nonce, target key, `claim ≥ observed − tolerance`,
  signature *length*. For an **extender** pinger it additionally verifies the
  signature under the pinger's key **and** that the key is in its own
  directory as an active record — which the operator's root key already
  vouched for. For a **provider** pinger it cannot verify the signature (no key
  store on an extender); the operator does, as today.
- **Pinger (A)** verifies the co-signature under B's identity key, which it
  already holds from B's record (the same key the outer TLS leaf is checked
  against).
- **Operator** verifies both signatures at ingest: the pinger's against the key
  store (provider) or the extender table (extender), the co-signature against
  the target's stored public key. It also refuses a pinger and target that are
  the same extender, or an extender and its own provider client.

### 2.5 Who reports: the pinger

The **pinger** reports its measurements; the target stops forwarding. The
co-signature is what makes this safe: a report that carries one has had a
second check by the party that saw the round trip, and a report that does not
has had none — the pinger could submit anything, so an uncosigned report is
**never a measurement**. It is stored as the pinger's *claim* that the target
refused (with the reason the target gave, or `unknown` for no verdict), and
on its own it is not evidence against the target either, since the pinger's
word is all it is. Current aggregate target-refusal counts are diagnostic
only: they do not lower the target's reputation or exclude it. A future
target penalty requires independently identified, otherwise-consistent
reporters and an explicit corroboration policy; aggregate totals cannot
establish that independence. Reporter-side refusal evidence remains usable
alongside geometric consistency (§5.5).
Uncosigned reports are rate-limited per reporter at ingest, so nobody can
flood a smear.

- Extenders report over the activation credential, as the current reporter
  does (`ExtenderLatencyReporter` generalised to a `PingReporter`).
- Providers report over their client credential from the sdk provider role,
  batched the same way. (Today a provider never talks to the operator about
  pings; the reporter is small and the credential is already there.)
- Endpoint: `POST /network/ping-report`, accepting both pinger kinds.
  `/network/extender-latency` stays accepting for one release so extenders
  in the field keep working, then is removed.

### 2.6 Storage

A new table, since the old one's shape (target-reported, provider-only) is
what changes:

```
network_ping (
    ping_id uuid PK,
    pinger_kind smallint,          -- 1 provider, 2 extender
    pinger_client_id uuid NULL,     -- provider
    pinger_extender_id uuid NULL,   -- extender
    target_extender_id uuid,
    probe_nonce bytea,
    rtt_ms int,
    probe_time timestamp,
    cosign smallint,                -- 0 unknown, 1 cosigned, 2 rejected
    cosign_reason smallint,
    pinger_signature bytea,
    cosignature bytea NULL,
    create_time timestamp,
    UNIQUE (target_extender_id, pinger_kind, pinger_client_id, pinger_extender_id, probe_nonce)
)
```

Rows live one day (§5.7). `network_extender_latency` is read alongside it
(as co-signed-unknown rows) until its own retention drains it, then dropped.
Signatures are stored so a row is re-verifiable and so a dispute can be
shown, not argued.

### 2.7 What the dashboard adds

Co-sign outcome by pinger kind and reason; per-target refusal rate (topk,
bounded like the other per-extender series); extender↔extender pings per
hour; the "targets refusing more than X %" list that answers the question
this was built for.

**Counted from tallies, never the rows.** At the target a day's partition of
`network_ping` is 275 GiB, and the dashboard's day counts, the per-extender
hourly counters and the §2.19c signal's hourly shares each read a whole day
of it on every refresh. They read tallies the ingest keeps instead: every
stored report adds what it stored in the same transaction as its rows, so the
tallies are exact and cost almost nothing to read, and a replay, a refused
report or a rejected claim stores nothing and adds nothing.

- `network_ping_hour_tally`: pings per hour, pinger kind, relay, verdict and
  refusal reason, with the zero and beyond-half-planet round trips the signal
  watches. Each hour's rows are split over sixteen shards by the pinger's id,
  since every report of the fleet would otherwise update the same few rows
  under their locks.
- `network_ping_target_hour_tally`: pings and refusals per hour, target and
  pinger kind, which the per-extender counters and the refusal list read.
- `network_ping_pinger_day`, `network_ping_target_day`: the distinct pingers
  and targets of each utc day, so the "measuring (24h)" and "pinged (24h)"
  counts are index-only. They count over the utc days that cover the trailing
  24 hours, an upper bound at most a day wider than the window; the day
  counts of pings are the 24 clock hours up to the current one.

The derive job still reads the rows, since it needs every sample, and
streams them.

### 2.8 Record ttl, re-release, and what a client keeps

A signed extender record now lives **24 hours** (`ExtenderRecordExpireTimeout`,
down from fourteen days), the same day everything else in this document
lives. Three things follow.

- **The server re-releases before expiry.** The publish drip
  (`extender_publish_work.go`, every ten minutes, oldest publish first) is
  sized to rotate the whole active set within `ExtenderPublishRotationTimeout`,
  and any extender whose oldest publish is at least that old is re-released
  on the next tick regardless of batch. The rotation is set to **12 h** —
  half the ttl, the same proportion the seven-of-fourteen-day rotation had —
  so a record reaches every client with half its life left and an extender
  that stays up is never seen expired by a connected client. (A rotation of
  the full 24 h would put every client of an extender into the expired tier
  at once for the length of a tick; the half-ttl rotation is the safe
  default and the constant is one line to change.) Activation still
  re-signs on its own 24 h cadence.
- **The client keeps expired records, bounded.** A record that expires is
  not expelled. The directory keeps the **most recent N** expired identities
  (`MaxExpiredRecordCount`, default 64, newest expiry first) with their
  addresses and local evidence, evicts only beyond that, and offers them as
  a **last tier** of candidates — after every active verified address and
  after the manual ones — so a client that has lost every path to the
  platform, and therefore every refresh, can still try the extenders it
  last knew. An established dialer to a retained expired address is kept.
  Expired records never count toward the low-water mark (a re-bootstrap is
  still the right reaction), are never re-gossiped by a feed or a node, and
  are reported as `expired` in the status exactly as today.
- **Nothing else changes for a healthy client:** with a 12 h rotation it
  never dials the expired tier at all.

### 2.9 Probes through an NLayer chain

An NLayer extender (`EXTENDER.md` A11) relays every forward to one of its
hops. A probe is relayed the same way, to the **end of the chain**, so that
the round trip a pinger measures is the path a client of that extender would
use, and the extender that judges and co-signs the claim is the one that
terminates the chain. The front never answers a probe itself.

- **One in-flight probe per signed source.** The front takes the source
  identity from the header — `ProbeClientId` for a provider,
  `ProbeExtenderPublicKey` for an extender — and refuses (403) a probe from
  a source that already has one in flight through it. This is the probe
  analogue of the ClientHello-random check of A11: a probe that comes back
  around carries the same source, and a source cannot hold more than one
  relayed probe open at a time. The identity in the header is unsigned when
  the front reads it; it is the identity the attestation's signature must
  match at the end of the chain, so a spoofed header buys a refused claim
  and nothing else. Its limit is stated plainly: anyone can occupy a
  source's one slot through a front for the life of one probe, bounded by
  the end's attestation timeout. A ranking-only probe carries no identity
  and gets no entry; the `HopCount` bound is its loop guard.
- **The front dials its hop with the same header and `HopCount + 1`, and
  waits for the hop's response** before answering. It then answers the
  pinger with its **own** identity — its key, its challenge signature, its
  carriers, since the pin the pinger holds is the front's — plus the nonce
  the chain end issued, `HopCount` as the end saw it, and
  `ChainEndPublicKey`: the hop's `ChainEndPublicKey` if the hop relayed
  further, else the hop's own key. From there it relays bytes both ways
  until the streams close, exactly as for a forward.
- **The pinger measures from header to response as before**; through a
  chain that is the whole chain. It binds the attestation to
  `ChainEndPublicKey` when present, else to the responder's key, so the
  end's "wrong extender" check holds and the co-signature it returns is
  under the key the report names. The latency sample is recorded against
  the address dialed — the front's — which is the path being ranked.
- **The end sees a direct probe.** Its interval, from its response to the
  frame's arrival, spans the same chain round trip the pinger measured, so
  the one-sided gate holds unchanged; it judges an extender pinger against
  its directory as for any probe; its nonce is on its own stack.
- **At the operator**, `network_ping.hop_count` records the depth. A relayed
  ping counts on the dashboard and in the co-signature evidence, but only a
  direct ping (`hop_count = 0`) is a solver term (§5.1): a relayed round
  trip overstates the distance to the end by the detour through the front,
  and the front is never the extender a claim names, so a front has no ping
  terms of its own and keeps its genesis location.

Two more limits, stated plainly. The end sees every relayed probe as coming
from the front's address, so its per-address probe limit (one a second,
burst ten) caps all of a front's pingers together; the pinger's own cadence
(§2.1) keeps an honest front far under it, and a probe refused for rate
(reason 6) is re-probed later. And since `HopCount` comes back unsigned, a
pinger whose response names a chain end other than the extender it dialed
records at least one hop whatever the field says, so a front cannot pass a
relayed ping off as direct.

Wire: `ExtenderResponse.HopCount` (5) and `ExtenderResponse.ChainEndPublicKey`
(6), both set by the end and copied back by every relay; `hop_count` on the
ping report (D22).

## 3. Step 2 — GeoLite2 replaces every packaged ip database

### 3.1 Updating

`vault/GeoIp.conf` already holds the MaxMind account, license key and
`EditionIDs GeoLite2-ASN GeoLite2-City GeoLite2-Country`. `xops/mmdb/update.sh`
becomes:

```
geoipupdate -f "$BRINGYOUR_HOME/vault/GeoIp.conf" -d "$ip_dir"
```

into `config/all/mmdb/<date>/` (`GeoLite2-City.mmdb`, `GeoLite2-ASN.mmdb`,
`GeoLite2-Country.mmdb`) — the same dated, LFS-tracked layout the current
databases use — followed by the export of §4.1. The first City database is
already in place at `config/all/mmdb/2026.9.22/GeoLite2-City.mmdb` (build
2026-09-22); ASN and Country arrive with the first `geoipupdate` run. The DB-IP and
ipinfo downloads, their capability files and `ipinfo.yml` go; so does the
ansible test that pins them. `geoipupdate` is not on the workstation today
(`brew install geoipupdate`, or the `ghcr.io/maxmind/geoipupdate` image; the
script checks and says which). MaxMind's GeoLite2 terms require attribution
where the data is shown and keeping the database current — the `/ip` page
carries the attribution line and the update runs weekly.

`arindb/arin.mmdb` is **ours** (built from ARIN bulk data) and is the only
input to the *foreign* score; it stays.

### 3.2 `server/ip.go`

- A `GeoLite2-City` schema type decoding `city.names.en`,
  `subdivisions[0].names.en` (region), `country.iso_code`/`names.en`,
  `continent.code`/`names.en`, `location.{latitude, longitude, time_zone,
  accuracy_radius}` and the three **geoname ids** (city, subdivision, country).
- No ASN reader: nothing in the server read `IpInfo.ASN` or
  `ASOrganization` (the egress probe supplies a provider's ASN and org
  itself), so `root/GeoIP.conf` lists `GeoLite2-City` alone and the update
  fetches one file. Adding `GeoLite2-ASN` later is one edition id and one
  reader.
- `IpInfo` loses `UserType`, `Hosting`, `Privacy`, `Virtual`, `Organization`,
  `ASN`, `ASOrganization`: GeoLite2 has no such thing, and nothing else will
  be packaged that does. `ConnectionLocationScores.NetTypeVirtual` is
  therefore never set any more (the columns stay, at zero), and a fresh
  egress probe's hosting/proxy flags are applied whether or not the probe's
  *location* wins over the database's — the probe is the only source of them
  now.
  `ConnectionLocationScores.NetType{Hosting,Privacy,Virtual}` are then fed only
  by the egress probe (`provider_egress_location.hosting / proxy`), and are
  zero — unknown, not clean — for a client the prober has not seen.
  `NetTypeForeign` is unchanged (ARIN).
- `IpInfo.AccuracyRadiusKm` is new and is the genesis confidence of §5.

The egress prober's own lookups (`operator-proxy/geolocate/sources.go`:
`ip.pn`, `freeipapi`, `ipinfo.io` as *web* sources, consensus across them)
are an operator probe, not a packaged database. See D5.

### 3.3 The public `/ip` surface

`GET /my-ip-info` returns only what we can stand behind:

```json
{ "info": { "ip": "…",
            "location": { "coordinates": { "lat": 51.5967, "lon": -0.1593 },
                          "city": "…", "region": "…",
                          "country": { "code": "gb", "name": "…" },
                          "continent": { "code": "eu", "name": "Europe" },
                          "timezone": "Europe/London" } },
  "connected_to_network": true }
```

`privacy` (vpn / proxy / tor / relay / hosting / service) is removed from the
result, the OpenAPI schema and the ur.io `/ip` widget
(`react/src/components/ip/useMyIp.js` colours its dot from `privacy` today;
it colours from `connected_to_network` alone). The schema's `landmarks`
array — documented, never returned by the controller — is removed with it.
Coordinates, continent and timezone stay: GeoLite2 carries all three per
network (`location.time_zone`, `continent.code`), and for a derived location
they come from the mapped city, whose timezone and continent the export
carries (§4.1). Continent is in any case a deterministic function of the
country (`model.ContinentCodeForCountry`, the mapping the geo dns and the
record tag already use), and timezone a deterministic function of the city,
so neither needs a lookup outside our own data and no timezone package is
added.

### 3.4 Documents that name the old sources

`mmm/ur.io/docs/THREAT-MODEL.md` describes the lookup as a bundled
`mmdb/ip-ipinfo.mmdb` read from disk (two passages, cited to `ip.go`); both
become GeoLite2 with the attribution line, and the privacy verdicts leave the
`/ip` description. The comparison docs cite IPinfo's *published study* of
other VPNs' locations as evidence; that is a citation, not a data source, and
stays.

## 4. Step 5 — one canonical place list, exported from the same database

Done *before* steps 3 and 4, because both reverse-map into it.

### 4.1 The export

A tool (`server/cli/geolite2export`, run by `update.sh` after each update)
walks every network in `GeoLite2-City.mmdb` and collects the distinct
`(country iso, subdivision, city)` triples keyed by **city geoname id**, with
a representative lat/lon and the continent. Measured on the 2026-09-22
build: 5.79 M networks, 4.53 M of them with a city, **77 753 distinct
cities**, and 8 402 of those (11 %) carry more than one coordinate across
their networks — up to 430 for one city, since MaxMind places some networks
at postal-code precision inside a city. The representative is therefore the
**mode** across the city's networks (ties to the smallest accuracy radius),
and the export also keeps the city's **spread** — the largest distance
between its variants — so the reverse geocoder knows how wide a city is when
two are close. Output, beside the databases it came from:
`config/all/mmdb/<date>/places.yml` — country → region → city →
`{geoname_id, lat, lon, spread_km, time_zone}`, each city's timezone being
the mode across its networks exactly as its coordinates are — plus a country
list with names and continent from `GeoLite2-Country`. `city-list.yml` (2023, 34 MB, no coordinates, and far
more places than any lookup can resolve to) and `iso-country-list.yml` are
deleted. The canonical set shrinks to what GeoLite2 can actually answer with;
rows the old list seeded stay resolvable (§4.2) but are no longer the target
of any lookup.

Because the export and every lookup come from the same file, an mmdb answer
and a seeded location agree by construction: same names, same ids, same
coordinates.

### 4.2 The seeder

`location` gains `geoname_id bigint NULL UNIQUE`. The seeder reads
`places.yml` and creates or updates rows by geoname id (name and coordinates
refreshed on each run; legacy rows get their id filled in), and never
deletes a place — a location a contract once referenced stays resolvable.
`CreateLocation` from an mmdb lookup matches by geoname id first.

**Matching is fuzzy before it is new.** Neither the old list nor a lookup
spells a place exactly one way — `Sao Paulo` / `São Paulo`, `Frankfurt` /
`Frankfurt am Main`, `Saint-Denis` / `St Denis` — so before a city or region
row is created, the seeder and `CreateLocation` look for an existing one:

1. within the same country (and, for a city, the same region), never
   across;
2. by **normalised** name first: NFKD with the combining marks stripped,
   case-folded, punctuation and runs of whitespace collapsed;
3. an exact normalised match **anchors**: an id-less row whose normalised
   name equals a GeoLite2 place's in that region *is* that place, and is
   never a candidate for anything else;
4. only a name that anchors to nothing is matched by **Damerau–Levenshtein
   distance** on the normalised names — at most 2, or 3 when the shorter
   name is 8 characters or longer, so a four-letter name cannot be
   "matched" to an unrelated one — and only when exactly **one** GeoLite2
   place in the region is within that distance; a second candidate in
   range, or a tie, is no match, and the row is left as it is.

A match adopts the row: it gets the geoname id and the coordinates if it
had none. Only when nothing matches is a row created.

Anchor first, fuzz only what is unanchored (confirmed 2026-09-23). The
distance rule alone, run over GeoLite2's 77,753 cities as if they were
id-less legacy rows, pairs 18,919 distinct real cities within their own
region — Cambridge and Uxbridge with Abridge, Melbourne with Aldbourne,
Clayton with Clanton — and the de-duplication below would have merged
10,031 rows into 6,894 groups on its first run. Anchoring makes that
zero by construction: a real name is never a misspelling of another real
name, so the distance rule only ever sees names GeoLite2 does not know
(`Kiev` → `Kyiv`, a mistyped long name), which is what it was for.

**The init task de-duplicates.** The same rule, run over the existing table,
resolves every id-less row against the place list — anchored or uniquely
fuzzed rows get their id, the rest are left alone — and the rows that
resolve to one id are one place. For each group the seeder keeps
one canonical row (the geoname-id row, else the most referenced, else the
oldest), repoints every foreign key that names a member of the group to it
— every `*_location_id` column in the schema is inventoried from the
migrations, not from a hand list, and the task refuses to run if a new one
appears that it does not know — then deletes the members, all inside one
transaction per group, logging what merged into what. Regions are merged
before cities, since a city's parent must be canonical before the city's
own match is judged.

Regions follow GeoLite2's first subdivision; a city GeoLite2 files under no
subdivision keeps today's convention of a region named for the country.

### 4.3 Reverse geocoding

`server/geo` (new): the export loaded once into a 1°-cell grid index
(~130 k cities), `NearestCity(lat, lon, countryCode)` by great-circle
distance, optionally restricted to a country. Used by §6.

## 5. Step 3 — derived locations by least squares

### 5.1 Nodes, edges, genesis

- **Nodes**: every provider (`client_id`) and every extender (`extender_id`)
  with at least one usable ping in the window (`DeriveWindow`, 24 h — the
  ping retention of §5.7, so the window is everything that exists).
- **Genesis** `g_i`: an extender's latest activation location; a provider's
  egress-probe location when fresh, else its control-address lookup. Each
  carries GeoLite2's accuracy radius `r_i` (km) — 5 km for a well placed
  residential prefix, 1 000 km for an anycast or hosting address the database
  can only put in a country (8.8.8.8 resolves to no city, radius 1 000) — so
  a coarse genesis is a weak anchor that the pings are free to move, and a
  precise one is not. A probed location uses a fixed `r` for "city confident"
  and a larger one otherwise.
- **Genesis is the lookup, never the published answer.** Once §6 writes a
  derived place into a connection's location, reading that row back as the
  next run's genesis would anchor each derivation to its own last answer and
  let a node walk away from GeoLite2 run by run. The connection row therefore
  keeps the lookup's own location beside the published one
  (`network_client_location.genesis_location_id`), and the job reads that.
- **A genesis placed only in a region or a country** carries no coordinates,
  so it stands at the city nearest the mean of that region's (country's)
  cities, anchored at least as wide as their RMS spread — 1 344 km for the
  United States, 251 km for Germany, 10 km for Singapore on the 2026-09-22
  list — which makes it the weak anchor it should be.
- **Terms**: co-signed, **direct** pings only (D8; a ping relayed through an
  NLayer chain is stored with its depth and never solved on, §2.9),
  aggregated per **ordered** pair
  (source `s` → destination `t`): `rtt_st` = the **median** of source `s`'s
  samples toward `t` over the window (`EdgeAggregate`, a setting). The two
  directions of a pair are two terms, deliberately: each belongs to the
  source that measured it and is weighted by that source's reputation
  (§5.5), so one bad source cannot contaminate the honest measurement of
  the same pair from the other side. The median is unbiased under symmetric
  noise and still shrugs off outliers; the minimum — the classic
  propagation-floor estimator, since queueing only ever adds delay — is
  selectable if real traffic proves queue-dominated, but it is biased low
  under the zero-mean noise the acceptance tests use (§5.6).
- **Implied distance** `d̂_st = k · max(0, rtt_st − o)`, with `k` = 100 km per
  ms of round trip (≈ two-thirds of c one way; `server.DistanceMillis` today
  uses c in vacuum and overstates what fibre can do) and `o` = 2 ms of fixed
  handling overhead. Both are settings; §5.4 calibrates them.

### 5.2 The objective

Each node gets a correction `Δ_i` (a north/east offset in km on the local
tangent plane at `g_i`, converted to a lat/lon delta when stored). With
`p_i = g_i ⊕ Δ_i` and `d(·,·)` the great-circle distance in km:

```
minimise   Σ_i  w_i · d(p_i, g_i)²                       genesis
         + Σ_{s→t} v_st · ( d(p_s, p_t) − d̂_st )²        pings, one term per source and destination
         + Σ_i  λ_region · h_region(p_i)²  +  λ_country · h_country(p_i)²      containment

w_i  = 1 / max(r_i, 5 km)²                   genesis: a wide accuracy radius is a weak anchor
v_st = q_s · n_st / max(d̂_st, 20 km)²        ping: relative error, more samples weigh more,
                                              the SOURCE's reputation (§5.5) scales its own terms
h_region(p)  = max(0, d(p, nearest city in g's region)  − d(p, nearest city outside it))
h_country(p) = max(0, d(p, nearest city in g's country) − d(p, nearest city outside it))
```

The first two terms are exactly what was asked: squared error to the genesis
and squared error between all measured pings, with the error a surface
distance. The genesis term is what makes the problem well posed — without it
the whole graph could translate or rotate freely — and it is also what
**bounds** a correction: a node can only move as far as its pings outweigh
its anchor, so a wide accuracy radius moves freely and a tight one barely at
all, and no separate cap on the shift is needed.

The containment terms bias a node toward staying in the region and country
its genesis placed it in, without forbidding a crossing the measurements
insist on. Each is a hinge on the same city set the reverse geocoder uses
(§4.3): zero while the nearest city to `p` is inside the genesis region
(country), growing with the margin by which an outside city has become
nearer once it is not. The hinge is continuous and piecewise smooth, so
Gauss–Newton takes it like the Huber term. `λ_country` is set well above
`λ_region` — a country border is a far stronger prior than a region line,
because GeoLite2's country placement is far more reliable than its city
placement — and both are settings calibrated in the same pass as `k` and
`o` (§5.4); the starting values are `λ_region = 1/(10 km)²` and
`λ_country = 1/(2 km)²`, so a node ten kilometres over a region line pays
what a tight-genesis node pays for a fifty kilometre shift, and a country
crossing five times that.

Two refinements are proposed, both off by default and both single settings
(D9): a **Huber** loss on the ping term so one wildly inflated ping cannot
drag a node, and an **asymmetric** ping term, since a measured RTT can
overstate a distance (detours, queues) but never understate it.

### 5.3 Solving

Block Gauss–Newton with a Levenberg–Marquardt damping: each sweep solves
every node's own 2×2 normal equations from the terms it touches, warm-started
from the previous run's corrections and capped at a fixed sweep count. Block
sweeps alone crawl — a short link is thousands of times stiffer than a link
to an anchor, so a cluster drifts back toward the truth by a fraction of a
percent per sweep — so each sweep is followed by a line search along the
direction from two iterates back (the parallel-tangents step), accepted only
when the objective falls; that brings the perfect-ping test from 5 km of
residual error to under 0.3 km in about twelve sweeps. Measured on the real
place list with the host loaded: 1,100 nodes and 33k terms in 1–2.5 s,
3,200 nodes and 192k terms in about 10 s, with containment adding roughly
two thirds; the later reputation rounds can reach the sweep cap without
meeting the step tolerance, which is a calibration item, not a correctness
one, since every accepted step lowers the objective. The work is one
taskworker job (`ScheduleDeriveLocations`, every 8 h — three derivations
inside every ping's one-day life).

**Scale (D27).** The cost is the terms. A sweep is about half a
microsecond per term and a few microseconds per node on one core; a run is
three reputation rounds of at most a hundred sweeps, typically about twelve
each. With `PeerSampleSize` 64 and `ProbeMaxCandidateCount` 16, a million
extenders and a million providers (half reconnecting in a day) make about
72 million terms and two million nodes — half an hour typical and hours
worst case on one core, and seven gigabytes at a hundred bytes a term. The
job does not partition; it parallelises, because the sweep is embarrassingly
parallel and the taskworker host has the cores and the memory (96 and
16 GB) to use:

- **Colour-ordered Gauss–Seidel sweeps over a worker pool.** The nodes are
  greedily coloured so that no two of a colour share a term; each colour's
  2×2 steps are computed in parallel across the workers (`Workers`,
  `ParallelChunk`) and applied before the next colour, so the result is
  exactly a one-at-a-time Gauss–Seidel sweep. Plain Jacobi — every node
  stepping from the previous sweep's positions — was built first and
  rejected: both ends of a stiff 93 km pair closed the gap in the same
  sweep and the pair landed 72 km off where one-at-a-time sweeps reach
  0.11 km, and neither a line search along the Jacobi step nor
  double-counting the shared curvature fixed it. The sweep is
  deterministic — the nodes are chunked in a fixed order and every partial
  sum is combined in that order — so a solve gives the same answer on one
  core as on ninety-six, bit for bit, which the tests assert at 1, 2, 3, 7
  and 10 workers and under shuffled terms.
- **Stopping.** A run stops on the first of three: every node's step under
  `MinStepKm`; the objective's relative improvement over
  `StagnationSweeps` (20) consecutive sweeps under
  `StagnationRelativeImprovement` (1e-3, 0 turns the stop off); the sweep
  cap. The stagnation stop is what keeps the target run near its typical
  cost — without it every large run hit the cap while hundreds of weakly
  determined providers kept sliding by more than `MinStepKm` after the
  objective had flattened, so typical equalled worst; with it the target
  run takes 145 sweeps instead of 300 and the acceptance numbers are
  unchanged to three figures, at the cost of about 2% fewer nodes
  published (the ones still moving, below). Tighter settings (10 sweeps,
  or 5) cost 3–13% of the published nodes for a faster stop and were
  rejected. Because a weakly determined node may still be moving when the
  run stops, the solver reports each node's last step
  (`NodeResult.LastStepKm`) and refuses to publish a node whose last step
  exceeds `PublishMaxLastStepKm` (0.01 km, equal to `MinStepKm`) with its
  own reason, `still_moving`, tallied with `few_pings`, `few_peers` and
  `no_improvement` in `Result.PublishRefusals`, which the derive job
  records with the run and the §2.19c signal reads.
- **Parallel reductions** for the objective, the line search, the residual
  statistics and the reputation statistics, and parallel containment
  lookups, so nothing in a sweep is serial but the combine.
- **Compact terms.** A term is two `uint32` node indexes, a `float64`
  round trip, a `uint16` sample count and a `float32` weight — 24 bytes,
  plus 4 for the incoming index; a `float32` round trip was tried and moved
  a genesis-at-truth solve by 2.1e-5 km, over test 1's 1e-6 km bar — and
  the per-pair samples the median needs are kept in a bounded reservoir
  (`PairSampleReservoir`, 16). Seventy-two million terms are about two
  gigabytes, with room in sixteen. The reservoir's replacement
  draw is seeded from the pair's identity (a process-independent hash of
  the two node ids, fixed when the pair is created), never from
  aggregator-local indexes or anything else that depends on which rows an
  aggregator happened to see first, so a pair's reservoir is a function of
  its samples alone.
- **Parallel ingest of the day.** The job streams the day's pings over
  `DeriveReadCursors` (16) concurrent cursors split by pinger-id hash range
  and aggregates into terms as rows arrive, never holding rows; four
  hundred million rows read in minutes instead of half an hour. The split
  keeps every (source, target) pair whole inside one cursor, so the merge
  of the cursors' terms is exact, and the terms are sorted canonically
  before the solve; the terms and the solution are identical whatever the
  cursor count, which the job's tests check at 1, 4 and 16 cursors with
  pairs above the reservoir size.

At the target a sweep is under a second, a run a minute or two typical and
a few minutes worst case, on the taskworker host. A **planning step** still
projects every run from the last run's measured counts and costs (terms,
nodes, seconds per term-sweep and per node-sweep at the cores available,
bytes per term, sweeps taken — recorded per run) against `MaxSolveSeconds`
(600) and `MaxSolveBytes` (8 GiB), and the projection is watched (§2.19c):
it is the capacity alert, not a switch. Partitioning by continent, with
cross-partition terms anchored at the other side's last published
position, remains the design on paper if the fleet ever outgrows the host,
and is not built.


### 5.4 Publishing, and refusing to publish

A correction is published only when it improves on genesis:

- the node's RMS ping residual with the correction is below its residual at
  genesis;
- the node has at least `MinDerivePings` (default 3) co-signed pings to at
  least `MinDerivePeers` (default 3) distinct peers. Two peers can fix a
  position only along the line through them — the node's mirror image
  across that line is as far from each and explains its pings as well — and
  a third peer off the line breaks the collinearity (D11, raised from two on
  2026-09-24).

Otherwise the node keeps genesis. There is deliberately no cap on how far a
correction may move a node: the genesis term already makes every kilometre
of shift compete with the measurements (§5.2), and the containment terms
make a region or country crossing cost more still. A crossing that survives
both is therefore strong evidence, not an error — it is counted under
"crossed region" / "crossed country" and shown on the dashboard, because a
rising count is how a wrong genesis, a bad `k`, or a colluding cluster
first shows. The `k` / `o` constants are
calibrated against the pairs whose genesis we trust most (extenders on known
hosts) by minimising the same residual; the job logs the residual so drift is
visible.

Output:

```
derived_location (
    node_kind smallint, node_id uuid,          -- 1 provider client, 2 extender
    genesis_latitude, genesis_longitude double precision, genesis_accuracy_km real,
    delta_latitude, delta_longitude double precision,
    latitude, longitude double precision,      -- genesis ⊕ delta
    ping_count int, peer_count int, residual_km real,
    crossed_region bool, crossed_country bool,  -- the mapped place differs from genesis
    location_id uuid, city_location_id uuid, region_location_id uuid, country_location_id uuid,
    update_time timestamp,
    PRIMARY KEY (node_kind, node_id)
)
```

### 5.5 Source reputation: reversion to the mean

Every ping has two parties and either can misbehave. A pinger can sign an
inflated round trip — it cannot deflate one, the gate stops that, but it can
wait before sending its attestation — and a target can co-sign a claim it
should have refused, or refuse claims it should have co-signed. A
co-signature is a second check, not a proof, so the solver weighs each
**source** (a provider or an extender, as pinger and as target) by how much
it looks like everyone else, and lets what does not look like everyone else
count for less.

After each solve, every node gets, over its edges in the window, five
statistics, each expressed as a z-score against the population mean:

- **scatter** — the RMS residual `d(p_s, p_t) − d̂_st` over the terms it is
  the source of, against the population RMS;
- **bias** — the mean *signed* residual: whether its round trips run
  systematically long against the geometry the rest of the graph agrees on;
- **coverage** — the share of the peers it was *expected* to sample that it
  has co-signed pings with: for an extender, the smaller of the peers
  available and `PeerSampleSize`; for a provider, the smaller of the
  extenders available and `ProbeWindowCount` (4) — the client's probe pass
  stops once that many candidates are close enough and reaches
  `ProbeMaxCandidateCount` (16) only in a badly connected region, so four
  distinct targets is a provider's designed coverage and more is not
  better; measured against 16, a well-connected provider would sit at a
  quarter coverage, be marked down to `q_min`, and be excluded on scatter
  or bias the round after. Every source
  subsamples by design, and a source is never marked down for doing what
  every source is expected to do; the statistic catches the source that
  measures far fewer than its expected sample — a hand-picked subset while
  its peers sample fully — which is unlike the mean source before a single
  residual is looked at, and a colluding pair is exactly such a subset. The
  expected count is an input to the solve (`Node.ExpectedPeers`), set by the
  derive job from the same sample-size settings the pingers use;
- **refusal rate as pinger** — the share of its attestations that came back
  refused;
- **refusal rate as target** (extenders) — the share of attestations it
  allegedly refused, recorded as diagnostic evidence only.

The node's weight is `q_s = 1 / (1 + z_max²)`, clamped to `[q_min, 1]`
(`q_min` 0.05): a node that looks like the mean keeps full weight, a node two
sigma out keeps a fifth, and a node past `ExcludeZ` (default 4) on scatter,
bias or a reporter-side refusal rate is left out of the solve entirely and listed on the
dashboard. Three readings fix the arithmetic: only the bad side of a
statistic counts (a source with *less* scatter or *fewer* refusals than the
mean is not unlike it), except bias, which counts both ways; **coverage
lowers the weight but never excludes**, because against a population that
measured everything one subset-only source sits at `−√(N−1)` sigma by
arithmetic alone, which would exclude the truthful subset source of §5.6;
and every z-score divides by at least a spread floor (`ScatterFloorKm` and
`BiasFloorKm` 10 km, `CoverageFloor` and `RefusalFloor` 0.05), so rounding
noise in a perfect population and one lone refusal do not read as many
sigma. Measured on synthetic honest populations, about a third of honest
sources sit below `q` 0.5 — that is the intended shape of `1/(1+z²)`
against the honest spread, and the floors are the knob if it proves too
steep. The weight scales every
term the node is the **source** of — its own measurements, kept separate
from anyone else's measurements of the same pairs — and nothing else: the
destination's reputation does not touch a term, because the destination did
not produce the number, it only co-signed that the number was not undercut.
The genesis term is never reweighted: the anchor is the anchor.

The solve runs `ReputationRounds` (default 3) times — solve, score, reweight,
repeat — which is iteratively reweighted least squares, and it converges in a
few rounds because each round only moves weights toward what the previous
geometry already agreed on. That is the reversion to the mean: the consensus
geometry is the mean source, and a source is trusted in proportion to its
distance from it.

Only a refusal that speaks to honesty counts: an rtt below the observed
interval (reason 1), a bad nonce (2), a claim bound to the wrong extender
(3) or a bad signature (5). A pinger the target does not know (4) and a
probe refused for rate (6) say nothing about either party — the first is
usually a directory that has not caught up, the second the shared address
of an NLayer front (§2.9) — and are excluded from both refusal rates.
Refusal rates are reported for both sides, but only the reporter-side rate
can currently affect weight or exclusion. A pinger reporting refusals from
many targets may be penalized; one pinger cannot make a target appear bad by
concentrating uncosigned claims against it. Even a high target-side rate
remains diagnostic until per-pair evidence, independently identified peers,
and a corroboration policy are implemented. Population normalization of an
aggregate target count alone is not corroboration.

Reputation is recomputed from scratch on every run from the window's pings.
Nothing accumulates, so a node that stops misbehaving returns to full weight
as its bad pings age out of the window — and nothing is ever *added* to a
node's standing by good behaviour, only *not subtracted*.

### 5.6 Acceptance tests

The solver is a pure package with no database behind it, so these run as
unit tests on synthetic geometry. Each uses a set of true coordinates
(`p*_i`) spread over a continent, `k` and `o` as configured, and pings
derived from them.

1. **Perfect pings.** `rtt_ij = d(p*_i, p*_j) / k + o` for every pair, no
   noise. With genesis equal to the truth the solver moves nothing. With
   genesis perturbed by tens of kilometres at a wide accuracy radius, and a
   few anchors left at the truth with a tight radius, the derived
   coordinates reproduce the truth to numerical tolerance (under a
   kilometre) — the pings fix the geometry and the anchors fix the frame.
2. **Zero-mean noise.** The same, with independent zero-mean noise on every
   sample and many samples per edge. As the sample count grows the derived
   coordinates converge on the truth; the test asserts the error at a fixed
   sample count is below a tolerance and falls as the count rises. (This is
   the test that rules out the minimum as the per-edge aggregate.)
3. **A malicious source.** Honest sources ping every peer truthfully (with
   noise); one source pings only a small subset and reports round trips
   that place it somewhere else. Its reputation weight must come out well
   below the honest sources' (both from coverage and from residuals), the
   honest nodes' derived coordinates must stay within the noise-only
   tolerance of the truth, and the report must list the source. A variant
   where the subset-only source is truthful checks that coverage alone
   lowers the weight but never excludes.

4. **No pings.** A node with no samples at all — and a node with fewer
   than `MinDerivePings`, or with pings to a single peer — converges to its
   genesis exactly: with only the genesis term the objective's minimum is
   the genesis, so the correction is zero, and it must be zero also when
   the solve is warm-started from a stale correction of hundreds of
   kilometres. Such a node is never published; a derived row it had from an
   earlier run is removed by the next run, and by the sweep if no run
   comes; and the read precedence of §6 then answers the egress probe or
   the lookup again. A quiet day must leave a node exactly where GeoLite2
   put it, not where yesterday's pings did.

Passing all four is the bar for turning the derive job on. Once it is on,
`server/monitor/SIGNALS.md` §2.19c watches what the tests cannot: that the
job runs, that co-signed pings keep arriving and are not being refused or
left without verdicts, that the residual improves on genesis, that
crossings and exclusions stay rare, that the solve converges, and that
published nodes do not rest on the minimum peer count — each with a
threshold that is a setting, so a parameter drifting off shows as an alert
and not as quietly wrong positions.

### 5.7 Retention and cadence

Nothing here is a permanent record; it is a rolling measurement, and the
timeouts are chosen so the three cadences interlock:

| What | Lives for | Because |
|---|---|---|
| a ping (`network_ping`) | 24 h; the table is **partitioned by day** and the sweep drops whole partitions, never deletes rows | a path measured yesterday says little about today — and at fleet scale the table takes hundreds of millions of rows a day, which row deletes bloat (the `client_reliability` lesson) and partition drops do not |
| a derived location (`derived_location`) | 24 h from its `update_time`, swept by the same job | a node that stops pinging must fall back to genesis on its own, without anyone noticing it stopped |
| the derive job | runs every 8 h | three derivations inside every ping's life, so a fresh ping is used before it expires |
| a source's re-ping | every 12 h, never later than 24 h | a node that keeps measuring always has a derived location under a day old |

The sweep (`ScheduleRemoveExpiredPings`, hourly) drops a ping partition once
its upper bound is older than the ingest's keep span — the job also creates
the partitions two days ahead, and an insert that finds no partition for
its day creates it and retries once, so an insert never fails for want of
one — and deletes derived rows past theirs (one row per node, never a
bloat). The keep span (`ExtenderPingReportSettings.KeepTimeout()`) is the
larger of the retention and the two clock skews together, plus one sweep
interval: 25 h 5 min at the defaults (retention 24 h, backward skew 24 h,
forward skew 5 min, sweep hourly). It is the same span the ingest looks
back over for a replayed nonce, so a claim's original row always outlives
every replay of it — the reason the forward skew is five minutes and not a
day: a claim posted a day ahead of the operator's clock would otherwise be
accepted again after the day holding its original copy was dropped. A row
therefore lives between the keep span and the keep span plus a day, since
whole days are dropped. The tallies of §2.7 follow the ping days: the three
kept by day (the distinct pingers and targets, and the per-target hour tally,
millions of rows a day at the target) are partitioned like `network_ping` and
dropped with its days, and the fleet's hour tally, tens of thousands of rows
a day, is deleted a whole day at a time behind the oldest ping day the sweep
keeps; the derive job bounds its own read by `create_time`
over the retention, so its window is the retention exactly whatever the
sweep has or has not dropped yet, and the §2.19c signal judges the sweep by
the partitions themselves (a partition past the keep span plus a sweep
interval is an overdue drop; no partition covering tomorrow is an overdue
creation), never by row ages, which the day boundary makes ambiguous. A
derived row is never older than one day, and is usually under eight hours
old.

### 5.8 Scale tests (D27)

The target is encoded so that it cannot silently regress. Each test
measures at a scale a unit test can afford, derives the per-unit cost, and
asserts the extrapolation to a million extenders and a million providers
against a budget; one test per layer runs the real thing at a fraction of
the target behind `GEOMAP_SCALE=1`.

1. **Solver** (`server/geo/solve`): synthetic graphs at 10k and 100k nodes
   with 64 extender peers and 16 provider peers per source; measure sweep
   time per term and per node on one core and the speed-up across the
   host's cores, and bytes per term; assert the extrapolated target run —
   72 million terms, two million nodes — solves under `ScaleBudgetSeconds`
   (600) typical on `ScaleBudgetCores` (96) and fits `ScaleBudgetBytes`
   (8 GiB); assert a parallel solve equals the single-worker solve bit for
   bit; gated: the full target — a million extenders and a million
   providers — generated and solved on the host the test runs on, asserting
   the budgets scaled to that host's cores.
2. **Derive job**: a synthetic day of a million rows streamed through the
   parallel cursors and the aggregation, asserting memory proportional to
   pairs, not rows, and identical terms whatever the cursor count; the
   planner's projection against seeded run history, and its capacity
   finding within the margin; gated: the full job over a generated day at a
   tenth of the target's ping volume against the local database, within
   the task cap.
3. **Ingest**: a benchmark of report verification (two signatures each)
   asserting at least 10 000 reports a second per core, against the
   target's ~4 000 a second; the report rate limit against the posts an
   extender makes at 64 peers.
4. **Ping table**: partitions created ahead and dropped behind at the
   target's daily row count expressed as a table growth model, and a test
   that the sweep never issues a row delete on `network_ping`.
5. **Directory and pinger** (connect): a million gossip records into a
   directory capped at 512, asserting bounded memory and time and the
   continent-first retention; a pinger over 100k known peers pinging
   exactly 64 per refresh with the random slice rotating.
6. **Reputation**: a population where every source sampled its expected
   set shows no coverage penalty at any fleet size.

## 6. Step 4 — mapping a node to a place

`SetConnectionLocation` precedence becomes: **derived** (row present — the
sweep of §5.7 removes one older than a day, so presence is freshness) →
**egress probe** (fresh, as today) → **genesis** (GeoLite2). The extender record's location follows the
same rule at signing, on every path that signs one — activation, the publish
drip, the geo DNS TXT set and the bootstrap sample — and the geo DNS
continent sets follow the record's country, so "close" still means one thing
on both paths. A provider's derived place takes effect at its next
connection, since the location is set at connect time.

A derived lat/lon is reverse-mapped with §4.3 to the nearest city anywhere.
It is the containment terms of §5.2, not the mapping, that keep a node in its
genesis region and country — so a derived point that maps outside them has
paid for the crossing in the solve and is reported as such. The mapped city,
region and country ids are stored on the derived row so the read path is one
lookup, and the row records whether they differ from genesis.

## 7. Order of work

1. **Step 2 + 5 together** — GeoLite2 in `server/ip.go`, `geoipupdate` in
   `update.sh`, the export, the seeder by geoname id, the `/ip` surface
   reduction, attribution. Nothing else can be validated without the
   canonical place set.
2. **Step 1** — the wire extension (extender pinger, verdict frame), the
   pinger-side reporter for extenders and providers, the peer pinger, the
   expired-record tier in the directory, `network_ping` and
   `/network/ping-report`, the 24 h record ttl and 12 h rotation, the
   dashboard.
3. **Step 3** — the derive job over `network_ping` with the reputation
   rounds of §5.5, published behind the §5.4 gates, the sweep and cadences
   of §5.7, and its own panels (residuals, excluded sources, refusal rates)
   before anything reads it.
4. **Step 4** — flip the read precedence, extender records included.
5. **Step 6** — the egress index (§10): the three rollup columns and the
   per-mode tier formulas behind the old-field fallback, the country gate
   made unconditional, the diagnostics, and one release later the drop of
   the generated columns.

6. **Step 7** — the prober (§11): first the check rules alone (spaced
   browser-shaped retries, tunnel re-creation, consecutive failures per
   provider, retries served first, both batch guards, concurrency), which
   is what turns half the fleet back on; then the site-only run with `/ip`
   geolocation, the pool as data with its refresh, incompatibility learning
   and the §2.19b watch; then the column drops with step 6's.
7. **Extender admission limits** — `EXTENDER.md` A12, connect and the sdk's
   settings and status; independent of the steps above.

Each step ships on its own; nothing in a later step is required by an earlier
one, except that step 6's index reads what step 7's runs produce, so step 6
is inert until step 7's runs reach fifty scored loads.

## 8. Decisions to confirm

Recommendation first in each.

- **D1 · Who reports pings — confirmed.** The *pinger* reports; the target
  stops forwarding (§2.5). An uncosigned report has had no second check, so
  it is never a measurement and never, on its own, evidence against the
  target: it is the pinger's claim, rate-limited at ingest and read only in
  aggregate by the reputation of §5.5.
- **D2 · Verdict frame always sent.** The target answers every attestation
  with an explicit accept/refuse verdict (§2.3), so a refusal is
  distinguishable from a lost connection. *Alternative:* silence on refusal.
- **D3 · One attestation message, two signature domains.** Providers and
  extenders share the message, and each kind signs under its own domain
  (§2.2), which separates them as firmly as a kind byte would while leaving
  the provider format already in the field untouched. *Alternative:* a kind
  byte inside one domain, which would re-sign every deployed provider.
- **D4 · Extender pinger verification at the target.** The target verifies an
  extender pinger's signature and membership in its directory (§2.4); it
  still cannot verify a provider, which stays the operator's job.
- **D5 · The egress prober stays as it is — confirmed.** Only the packaged
  ipinfo (and DB-IP) databases are dropped; the prober's consensus over
  `ip.pn`, `freeipapi` and `ipinfo.io` as web lookups *through the provider*
  is unchanged, and remains the source of hosting/proxy/mobile.
- **D6 · ARIN stays.** `arindb` is our own build and feeds only the foreign
  score; it is not one of the "other ip mmdbs".
- **D7 · Canonical key is the GeoLite2 geoname id.** `location.geoname_id`,
  unique; names and coordinates refreshed from each export; legacy rows
  matched by full name and back-filled.
- **D8 · Only co-signed, direct pings feed the solver — confirmed.** Rejected
  and unknown pings are inputs to reputation, never measurements; a relayed
  ping (D22) is evidence and a dashboard count, never a term.
- **D9 · Loss function.** Plain squared error as specified, with Huber and the
  asymmetric ping term available behind settings for the calibration pass.
- **D10 · Containment is a penalty, not a rule.** Two hinge terms in the
  objective bias a node toward its genesis region and, more strongly, its
  genesis country; the measurements can still move it across, and the
  mapping then follows the point (§5.2, §6). Crossings are counted and
  shown. *Alternative:* forbid crossing the country in the mapping.
- **D11 · Publish gates.** A correction is published only when it lowers the
  node's residual and only with ≥ 3 co-signed pings to ≥ 3 peers (§5.4). No
  distance cap: the genesis term bounds the shift by construction.
  *Changed 2026-09-24:* the peer gate (`MinDerivePeers`) was ≥ 2 and is now
  ≥ 3. Two peers can fix a position only along their line — the node's
  mirror image across it explains the pings as well, the ambiguity behind
  the 1.5 % of synthetic honest nodes that converged to a wrong place (§9)
  — and a third peer off the line breaks the collinearity. The cost is
  fewer published nodes in thin regions, where a node reaches only two
  peers; the `few_peers` refusal count that `server/monitor/SIGNALS.md`
  §2.19c carries with every run's refusals by gate makes it visible.
- **D12 · Ping cadence.** New peers spread over 1 h, every peer refreshed
  every 12 h (never later than 24 h), 2 concurrent, lowest of 2 probes
  (§2.1); providers re-probe on the same 12 h cadence.
- **D13 · `/ip` scope — confirmed.** Keeps ip, coordinates, city, region,
  country, continent, timezone and `connected_to_network`; drops `privacy`
  and the never-returned `landmarks` (§3.3). Continent and timezone come from
  our own data (country → continent, city → timezone), not a new package.
- **D15 · Reputation by reversion to the mean.** Every source is scored
  against the population on scatter, bias, coverage and two-sided refusal
  rate; its weight `1 / (1 + z²)` (floor 0.05) scales every term it is the
  source of, and terms are kept per (source, destination) so the two
  directions of a pair stay separate; three reweighting rounds per solve;
  recomputed from scratch each run (§5.5). *Alternative:* score on residuals
  only and treat refusals as a dashboard-only signal.
- **D16 · Exclusion.** A source beyond four sigma on scatter, bias or a
  refusal rate is left out of the solve and listed, rather than merely
  down-weighted; coverage only floors (§5.5). *Alternative:* never exclude,
  only floor. Confirmed 2026-09-24 with the follow-on dynamic stated: a
  source with genuinely low coverage loses weight, which biases its node
  toward genesis, and if that node is then excluded on scatter or bias the
  round after, that is judged on its own residuals; the bias toward
  genesis is the intended effect, not a defect (designed subsampling is
  exempt through `ExpectedPeers`, D26).
- **D17 · Per-edge aggregate is the median, as a setting.** Median by
  default; minimum selectable for queue-dominated paths (§5.1). The
  acceptance tests of §5.6 fix the bar.
- **D18 · Retention.** Pings and derived locations live one day and are
  swept hourly; the derive job runs every 8 h; `DeriveWindow` equals the
  retention (§5.7).
- **D19 · Record ttl 24 h, rotation 12 h.** Records expire a day after
  signing and are re-released within half a day (§2.8); the rotation is a
  constant and can be set to the full day if the expired tier is judged
  enough.
- **D20 · Expired records are kept, bounded, as a last resort.** The client
  keeps the newest 64 expired identities and dials them only when nothing
  fresher is usable; they never count as usable and are never re-gossiped
  (§2.8).
- **D21 · Fuzzy matching and de-duplication — confirmed, with anchoring.**
  A new city or region is matched to an existing row by normalised name
  first; an exact match anchors the row to that place. Only an unanchored
  name is matched by Damerau–Levenshtein distance (≤ 2, ≤ 3 for names of
  8+ characters) within the same country and region, and only to a unique
  candidate in range (§4.2). The init task de-duplicates with the same
  rule and repoints every foreign key. The measured reason for the
  anchoring is in §4.2: distance alone merges thousands of distinct real
  cities.
- **D14 · Transition.** `/network/extender-latency` accepted for one release
  after `/network/ping-report` ships; `network_extender_latency` read by the
  solver as unknown-cosign rows until its retention drains it, then dropped.

- **D22 · Probes relay to the end of an NLayer chain (2026-09-23).** A front
  with hops relays a probe — provider, extender or ranking-only — to the end
  of its chain and answers with its own identity, the end's nonce and the
  end's key; the pinger binds its claim to that key; the front keeps one
  in-flight probe per signed source (§2.9). Relayed pings are stored with
  their depth and shown, and never solved on. *Alternative rejected:* the
  front answering probes itself, which would measure the front alone and
  co-sign nothing about the path a client actually takes.
- **D23 · The egress index replaces the net-type score (2026-09-23,
  confirmed).** (1) A blackhole verdict or a TLS-authentication failure
  is a hard exclusion: the provider is absent from the results altogether —
  either mode, `force_minimum`, an explicit `client_id`, network-only
  admission, every count — until the verdict clears; a fresh probe observing
  the exit in a country other than the published one is a gate on both
  buckets and the counts, no longer behind the rollout flag. (2) The probe's failures, one per real-site load that
  failed every retry, weighted per class and capped, order the quality
  bucket. (3) There is no hosting or proxy verdict anywhere (ruled
  2026-09-23, replacing an earlier ruling that they disqualify): the sites
  the vendors guessed about are in the sample, and membership is the 90 %
  rule alone. (4) Speed is the gates plus the
  performance tests, nothing else. (5) The ARIN foreign flag is retired with the score. (6) Computed by the reliability rollup into
  nullable columns; the old fields stay authoritative wherever the new ones
  are empty, and are dropped one release later. (7) The rollout flag decides nothing new; the online bucket holds the unprobed. (8) Location counts
  are the gate-passing set. (9) Named the egress index; the API shape does
  not change. (10) A bucket short of the request's count is backfilled
  from the other bucket's ranking, borrowed providers tiered behind native
  ones, never across the exclusions (§10.3). (11) A third, online bucket
  holds the unprobed providers that pass the reliability minimums and the
  speed-mode score maximum — real traffic's own verdict, whatever their
  contract count; a provider no client ever measured is not online —
  ordered by reliability weight then client-measured performance, counted
  as supply, and borrowed last by either bucket (`BackfillTierOffset` 11,
  twice that for online); it replaces the rollout flag's decision about the
  unprobed and is what answers a mass probe failure. *Alternative rejected:* any use of a vendor's
  hosting or proxy flag, which on main marked 79 % of probed exits "proxy"
  because they provide.
- **D24 · The prober loads real sites, retries, and decides for itself
  (2026-09-23).** No ip-intelligence source is consulted for anything: the
  exit's location is the operator's own GeoLite2 lookup of the address the
  operator's own `/ip` sees through the tunnel; every load gets `n` tries spaced at random with a mean of five minutes
  between them, each shaped like a browser's, and fails only when every
  attempt failed; the site pool is
  data that a daily task refreshes from our own results, retiring sites that
  fail healthy exits; a provider is dark only
  after `k` consecutive failed checks on the same connection, spaced by a
  backoff, and a reconnect clears it; a batch that reports more than a fleet
  guard's share dark is discarded as the prober's own fault (§11).
  *Alternative rejected:* keeping the vendor consensus for geolocation,
  which is a second source of truth beside §3 for the same question.
- **D25 · Extender admission limits (2026-09-23, `EXTENDER.md` A12).** Every
  extender admits by the source's peppered subnet hash under two
  per-instance limits — distinct subnets a minute, actions per subnet a
  minute — answers a limit with an ordinary 429 and a random `Retry-After`,
  exempts sources it is configured to trust (recursively: each layer
  rate-limits for the one before it), and a client treats a limit as a
  backoff, never a failure; a limited ping records nothing. *Alternative
  rejected:* a distinct refusal frame, which would be a fingerprint.
- **D26 · Bounded pinging and a partitioned ping table (2026-09-23).** An
  extender pings a bounded, rotating peer set (`PeerSampleSize` 64) drawn
  from a bounded, continent-first client directory (`MaxActiveRecordCount`
  512), so pinging is linear in the fleet; the reputation coverage
  statistic is measured against the sample a source was expected to take,
  so subsampling is never penalised; and `network_ping` is partitioned by
  day with partitions dropped, not rows deleted, from the first release.
  *Alternative rejected:* deleting rows on a 24-hour window, which is the
  `client_reliability` bloat repeated at a far higher rate.
- **D27 · The scale target is one million extenders and one million
  providers (2026-09-23), encoded in tests.** The solve is one graph,
  parallelised across the taskworker host's cores with deterministic Jacobi
  sweeps and compact terms, not partitioned; the derive job reads the day
  over parallel cursors and aggregates as it streams, never holding a day
  of rows; a planning step projects every run against time and memory
  thresholds and is the capacity alert; and every layer carries a test that
  measures its cost at a reduced scale and asserts the extrapolation to the
  target against a budget, plus one full-scale run behind an environment
  gate (§5.8). *Alternative
  rejected:* partitioning by continent, whose cross-partition terms converge
  only across runs, when the host's cores and memory make one solve fit
  several times over.
- **D28 · Names follow GeoLite2 (2026-09-24).** When a new GeoLite2 build
  renames a country (Turkey to Türkiye, Czech Republic to Czechia,
  Swaziland to Eswatini), the stored country row follows the new name on
  the next seeder pass: matched by its geoname id, renamed in place with
  its full name and search entries refreshed, its id and code unchanged,
  so nothing filed under it moves; a plain lookup that spells a country
  differently never renames it. The same rule already applied to cities
  and regions. GeoLite2 is the source of truth for names. *Alternative:*
  keep the first name a row was created with — rejected, because apps
  would show a spelling the world stopped using while new cities filed
  under the same row.

## 9. As built

Running record, filled in as each step lands; committed on the `geomap`
branches on 2026-09-24 on the operator's word.

Every line written for this plan, in whichever repository, follows
`connect/CODESTYLE.md` (operator's instruction, 2026-09-24): acronyms as
ordinary words in identifiers we own while serialized names stay, `self`
receivers, `stateLock`, usage+type names, a doc comment on every file, type,
function and test that does not start with the declared name, no `t.Run` for
positive cases, and test data only from `.example` and the RFC-reserved ranges.
Before a step is reported done, an audit of the declaration sites the working
tree added relative to HEAD (acronym runs, receiver and mutex names, missing or
name-repeating doc comments, subtests, positional literals, shifts, timers,
unguarded verbose logs) and a scan of new test lines for real addresses and
hostnames both come back clean.

- **GeoLite2 (§3, 2026-09-23).** `server/ip.go` reads `GeoLite2-City` through
  `maxminddb-golang/v2`'s struct decoder; `IpInfo` carries the continent and
  country codes (lower case), region, city, coordinates, time zone, accuracy
  radius and the three geoname ids, and nothing of ipinfo's privacy or
  hosting fields. `ConnectionLocationScores` takes hosting and privacy from
  the egress probe alone; the ARIN net-type stays. `/ip` returns city,
  region, country, continent, coordinates, time zone and
  `connected_to_network`; `privacy`, `landmarks` and `flag_url` are gone from
  the API, the OpenAPI file, the ur.io page, the widget and the docs. The
  ipinfo credential is dropped from the monitor config, the suite manifest
  and the ansible secret inputs; `vault/main/ipinfo.yml` itself stays until
  the key is revoked at the provider. `root/GeoIP.conf` (moved from
  `vault/`, mode 600) drives `xops/mmdb/update.sh`, which now runs
  `geoipupdate` for `GeoLite2-City` only and then the export;
  `config/all/mmdb/2026.9.23/` holds `geolite2.mmdb` (LFS, like every mmdb)
  and `places.yml` (plain, like the old city list). `2026.7.2/` is removed.
- **Export, seeder, reverse geocoder (§4, 2026-09-23).** `server/cli/geolite2export`
  wrote `places.yml` from the 2026-09-22 build: 77,753 cities in 250
  countries, 386 of them under no subdivision, 8,402 with more than one
  coordinate, 11.6 MB, byte-identical across runs, 4.4 s. `server/geo` loads
  it in ~1 s (~10 MB retained) and answers `NearestCity` in ~10 µs off a
  1° grid. `location.geoname_id` (migrations 691–692) keys the seeder and
  `CreateLocation`; the first seeder run renames about fifteen legacy
  countries to GeoLite2's names (Türkiye, Ivory Coast, The Netherlands).
  `spread_km` is the maximum distance and so is dominated by a single stray
  network (Los Angeles: 10,687 km, one network in Calabria); nothing reads
  it yet. The matcher resolves a stored row to a place by geoname id, then
  by anchor, then loosely to a unique candidate (§4.2); a region row
  resolves against its country's regions, a city row against its resolved
  region or the whole country. Run over all 77,753 GeoLite2 cities as
  id-less rows it anchors every one and merges nothing (the distance rule
  alone merged 10,031); over synthetic variants, case and diacritics
  resolve at 99.9%, a one-character deletion at 78.6% with 0.05% wrong
  (the typo is itself another real place, which anchors by design) and the
  rest ambiguous, never merged. The index costs ~19 MB and ~1 µs a
  resolution. The de-duplication job inventories 27 foreign-key columns
  across 12 tables from the migrations, refuses to run on one it does not
  know, and repoints each column once per run for every group
  (`UPDATE … FROM unnest(members, canonicals)`, batches of 100k) — the
  `network_client_location` city/region/country indexes were dropped by
  migrations 2598–2606, so a per-group repoint would have been a full scan
  each. All of the seeder's database tests compile but ran nowhere yet:
  this host has no postgres. `city-list.yml`, `iso-country-list.yml` and
  their generator are removed; the sn server fixture and sim-testnet take
  `mmdb/places.yml` and `mmdb/geolite2.mmdb` instead.
- **Wire and client (§2, 2026-09-23, connect).** `ExtenderHeader.ProbeExtenderPublicKey`
  (10) identifies an extender pinger; `ExtenderProbeAttestation.PingerExtenderPublicKey`
  (7) names it in the claim; `ExtenderProbeVerdict{Accepted, Reason, Cosignature}`
  is the fourth message, with reasons 0–6 as §2.3. Domains
  `ur-extender-probe-v1` (provider), `ur-extender-peer-probe-v1` (extender),
  `ur-extender-probe-cosign-v1` (verdict). The pinger reports
  (`ExtenderPingReporter` → `POST /network/ping-report`); the target's
  `ExtenderLatencyReporter` is gone. `ExtenderPeerPinger`: spread 1 h,
  refresh 12 h with 0.1 jitter and never past 24 h, two concurrent, two
  probes per peer, `ProbePeerVerifier` against the directory's active keys.
  The directory keeps an expired tier (`MaxExpiredRecordCount` 64, last in
  candidate order, out of counts and gossip); `LatencyMaxAge` is 12 h.
- **sdk (§2.5, 2026-09-23).** The provider installs its attestor and reporter
  on the space, which re-installs them on the client a settings change
  rebuilds (before this, a settings change silently dropped the attestor);
  a hosted device installs none, since its space is shared across tenants;
  a device closing clears only its own. The native extender role runs the
  peer pinger and reports through its activation credential; the mobile
  builds carry no extender role. `ExtenderProvideStatus` gains the peer
  ping counts (total, co-signed, rejected, unknown) and the last ping time.
- **NLayer extenders (2026-09-23, connect; `EXTENDER.md` A11).** An extender
  configured with hops relays every forward to one of a preset list of other
  extenders, chosen at random among the healthy ones, preferring the
  client's address family; a hop whose dial fails is held for
  `NLayerHoldTimeout` and retried after, while a hop's 403 is only counted,
  and a local memory-budget refusal is refused at once and holds nothing.
  `ExtenderHeader.HopCount` (11) counts the extenders a request has crossed;
  a request that would be more than `NLayerMaxDepth` (4) deep is refused,
  and a chain of eight relays under a bound of eight, tcp, quic and dns
  hops alike, with datagram boundaries kept. An extender with hops peeks
  the inner ClientHello (`NLayerClientHelloTimeout` 2 s) and refuses a
  client random already in flight, which stops a cycle at its second entry.
  The probe, gossip and feed services are always served locally. Two
  fixes fell out of it: after an http/1.1 hijack the reader net/http hands
  back cancels the request context on any read error, so the take-over now
  copies out the buffered bytes and reads the connection directly; and
  the request context now follows the server's, so `Close` interrupts a
  dial in flight. connectctl takes `--nlayer-hop=<spec>` with the secret of
  a private hop in a `secret_file`, never in the argument. Probes relay to
  the end of the chain (§2.9, D22; `extender/extender_nlayer_probe.go`): the
  front admits the probe (rate limit, one identity), refuses a second
  in-flight probe per source at stage "nlayer probe", dials its hop with
  the read deadline lifted, and answers with its own identity, the end's
  nonce, `HopCount` and `ChainEndPublicKey`; the pinger binds and verifies
  against the chain end key and reports `hop_count`. Measured under the
  race detector: a provider probe through two extenders co-signed at hop
  count 1 (6.6 ms through the chain against 0.36 ms direct), through eight
  at hop count 7 (42.8 ms against 0.33 ms), a peer ping through a front
  co-signed at depth 1 with the sample on the front's address, and the
  a → b → a loop refused by source after two hop dials.
- **Server (§1 restored, §2.5–§2.8, 2026-09-23).** The accidentally reverted
  commit is back and adapted: `GET /network/extender-hint`, `ContinentCode`
  at signing, `POST /network/extender-latency` (kept one release, D14, now
  guarded against an rtt that does not fit its column and a timestamp more
  than a day off, so one absurd claim cannot make an old-binary extender
  retry a batch forever) and its 30-day sweep. `POST /network/ping-report`
  (`controller/extender_ping_controller.go`) verifies the pinger — a
  provider under its client key, an extender under its stored key and only
  for its own pings — refuses a target the reporting client activated
  (siblings under one client are one party), recomputes `cosign` under the
  target's stored key and never trusts the claimed outcome, keeps both
  signatures, stores `hop_count` as reported, caps uncosigned rows at 64 a
  post, and returns an error rather than a rejection when the key store
  cannot be read so the reporter retries. `network_ping` (migrations
  693–697) with a unique `(target, pinger kind, pinger, nonce)`;
  `accuracy_km` on `network_client_location` and `network_extender_activation`
  (698–699, written from `ConnectionLocationScores.AccuracyKm`, nil when the
  radius is unknown); `hop_count` (700). Record ttl 24 h, rotation 12 h
  (72 ticks, minimum batch 8, every stale extender released regardless of
  batch; "stale" is `record_issue_time` older than 12 h). Hourly
  `RemoveExpiredPings`. Metrics `extender_pings_24h{pinger_kind, outcome,
  relayed}`, `extender_ping_sources_24h{pinger_kind}`,
  `extender_pings_total{extender_id, pinger_kind}`,
  `extender_ping_rejections_total{extender_id}`; the ping row has thirteen
  panels including the targets refusing the most (rate among targets with
  at least ten pings). OpenAPI: `extenderPingReport`; route checker clean.
  Every database-backed test compiles and ran nowhere yet (no postgres
  here). Deploy rule: migration 698 before the binary that writes it.
- **Solver (§5, 2026-09-23, `server/geo/solve`, `server/geo/containment.go`).**
  Pure package, no database: `Solve(nodes, terms, refusals, containment,
  settings)`, `Publishable`, `DefaultSettings`, the aggregator (median
  default, min selectable), spherical offsets, the block solver with the
  line search of §5.3 and the reputation rounds of §5.5. The containment
  scans the place list directly in latitude order (2–8 µs a hinge, where
  `NearestCity` at 50–115 µs made a 1,100-node solve fourteen times
  slower). Acceptance (§5.6), 20 nodes over a Europe-sized box, four 5 km
  anchors, the rest at 1,000 km, three seeds: perfect pings reproduce a
  genesis perturbed by 20–60 km to 0.27 / 0.16 / 0.03 km; zero-mean noise
  of 0.2 ms per sample gives 3.2 / 2.6 / 2.7 km at 16 samples a term and
  0.72 / 0.84 / 0.70 km at 256 with the median, against 35–45 km at 16 and
  57–73 km at 256 with the minimum, which fails the bar; a malicious
  subset source (its four nearest peers, round trips of a point 800 km
  east) gets q 0.05 and is excluded on every seed (scatter, bias and
  coverage all at 4.36 sigma) with honest nodes within 2.5–3.4 km and the
  lowest honest q 0.94; the truthful subset source gets q 0.05 from
  coverage alone and is never excluded. Two-node, hinge, Huber and
  asymmetric cases match closed forms to 1e-6 km. Two findings for
  calibration: about 1.5% of synthetic honest nodes whose genesis was
  170–500 km off converged to a wrong place and were excluded (the
  mirror ambiguity of a node with few peers; `MinDerivePeers` rose from 2
  to 3 for it on 2026-09-24, D11), and the later reputation rounds can hit
  the 100-sweep cap at 3,000 nodes.
- **Derive job and precedence (§5.4, §5.7, §6, 2026-09-23).**
  `taskworker/work/derive_location_work.go` runs every 8 h (30 min cap): terms
  from direct co-signed pings per ordered pair, refusal rates from reasons 1,
  2, 3 and 5 only, relayed co-signed pings counted as attestations only (so a
  chain end's refusal rate is not inflated), genesis per §5.1 with a warm
  start from the stored row, the solve with place containment, publish gates,
  reverse mapping, crossing flags and `CreateLocation` once per city per run;
  each run replaces `derived_location` (migration 701) in one transaction and
  a cancelled run publishes nothing. `genesis_location_id` is migration 702.
  Metrics `derived_locations{node_kind}`, `derived_location_crossings{kind}`,
  `derive_excluded_sources`, `derive_residual_km{at}` and
  `derive_last_run_seconds` (the last three written by the job to Redis so a
  restart or an older host cannot publish stale values); a "derived
  locations" dashboard row with eleven panels, stale at 9 h and red at 16 h.
  Each run logs how many published nodes sit at exactly `MinDerivePeers`
  peers and the sweeps per reputation round, the two calibration signals of
  the solver entry above. Database tests ran against the local stack: on a
  seeded graph two providers with a wrong genesis derive to 1.2 km and
  0.3 km of their true cities with the crossings reported, and a warm
  second run takes sweeps [2 2 1] against [5 2 1] cold.
- **Findings for the calibration pass (2026-09-23).** Round trips travel and
  are stored in whole milliseconds, and one millisecond is 100 km of implied
  distance: on the acceptance network the median of whole-millisecond
  samples stays near 22 km RMS where continuous round trips reach 0.75 km, at
  any sample count. With the default λ the containment hinges also hold
  back legitimate crossings: a synthetic provider one region west of its
  genesis moved 197 of 249 km, one a country south 195 of 389 km, with four
  peers at eight samples a term. `online_extenders_by_country` still counts
  by activation country.

- **Prober module (§11, 2026-09-23, `operator-proxy`).** Built: the
  browser-shaped profile (Firefox on Windows; Range and Accept-Encoding
  never sent; TLS fingerprint and HTTP/2 unchanged), retries with an
  exponential five-minute mean capped at three times it under an
  injectable clock, the `/my-ip-info` warm-up and `ExitIp`, tunnel
  re-creation up to twice per run with unreached loads recorded as not
  measured, the vendor `geolocate` package deleted (12 files), the eight
  reputation sites folded into the site class, a fifty-load sample (dns 6
  of the seven DoH endpoints, connectivity 8, cdn 10, site 26), the pool as
  data with `FetchPool` and the built-in 139-entry table as seed and
  fallback, per-destination `incompatible` places and canaries, the due
  list carrying each provider's place, and `ShortClasses` when a place has
  too few compatible sites. The module's suite passes, race-clean; the
  server must adapt to its breaking API before it builds.
- **Extender admission limits (`EXTENDER.md` A12, 2026-09-23).** Built in
  connect, connectctl and the sdk, race-clean; see `EXTENDER.md` §7 for the
  as-built detail. The operator's probes sit far under the defaults.
- **Step 6, the egress index (§10, 2026-09-24, server).** Built as
  §10.3 says: `ComputeEgressIndex` over the last run's classes with the
  90% rule over `MinScoredLoads`, the rollup writing `egress_index`,
  `egress_quality` and `egress_evidence_time` (migrations 703–705), the
  quality and speed tiers with the old rules wherever the index is null,
  the hard exclusions on every path (`force_minimum`, `client_id` and the
  network-only path included), the country gate as a minimum, the online
  bucket under Option A (reliability floors and the speed-mode score
  maximum), backfill quality←speed←online and speed←quality←online at
  `BackfillTierOffset` 11 (online at 22) with the `provider_backfill`
  metric, the counts as the gate-passing set, `bringyourctl provider
  inspect`, the providers dashboard gauges, and `EgressIndexSettings` with
  `RequestSettingsMaxAge` in place of the last package constant. Config:
  `config/main/provider.yml` carries `egress_index: {min_scored_loads: 26}`
  with the note to raise it to the 50 default once the fifty-load runs
  cover the fleet. Tests through the lock: the ten backfill tests (mass
  quality failure, probe pipeline down, mass speed-cutoff failure, mixed
  natives and borrowed, both short-bucket orders, never across an
  exclusion, old rows by the old rules, a missing other-mode cache, the
  metric), the online membership, supply and ordering tests, the rollup,
  null-row, tier-formula, over-the-line, exclusions-everywhere,
  country-gate-minimum and unprobed-is-online tests, the counts, the stats
  refresh, 188 model tests, the whole monitor and api packages, the route
  check (187 routes, 187 documented) and the spec test.
- **Step 7, the server side of the prober (§11, 2026-09-24, server).**
  Dark is three consecutive failed checks at least thirty minutes apart
  under `FOR UPDATE` in `RecordProviderBlackholeChecks`, a check that
  measured nothing only reschedules, TLS failures stay immediately dark,
  passed `next_due_at` rows are served before first checks, and one SQL
  predicate (`ProviderBlackholeDarkSql`) is shared by the dark set, the due
  heads and the §2.19, §2.19b and §2.24 signals. The dark guard (0.2 over
  at least 10 measured checks) and the run guard (0.3 over at least 3
  runs) buffer a full batch and release it only past the guard, with a
  tripped batch reported as `run_batch_guard` attempts that the due query
  re-queues after the first backoff step; each trip is a metric and an
  alert-class error line. The exit is placed by GeoLite2 at ingest (city
  only under a 25 km radius, else region or country; the address is never
  stored; a submission without `exit_ip` is refused); hosting, proxy and
  mobile are neither written nor read and `applyProbedNetTypes` is gone,
  the net-type columns feeding only the generated score the client-score
  job reads as the fallback for rows without an index. The pool is data
  (`provider_egress_destination`, migration 710; `provider_egress_site_tally`
  711 and `provider_egress_place_tally` 712 count every load by day and
  place, healthy exits marked, because a run names only its failures and a
  site is drawn about one run in four), seeded from the module's built-in
  table with cachefly and cloudfront re-pointed at small static objects,
  served at `GET /network/provider-egress-destinations` with a marked site
  a canary in 5% of fetches, and refreshed daily by
  `RefreshEgressDestinations`; probation and incompatibility are filtered
  at ingest into `unscored_failed_names`, so step 6's rollup is unchanged.
  The §2.19b signal `egress-site-pool` carries its nine conditions and an
  unobservable class; §2.23 and §2.24 learned the new attempt classes and
  the three-failure dark rule (a single failed check no longer reads as
  dark on the HMAC cutover). The pin refresh is retired, not repurposed (a
  lagging pin would fail every warm-up on TLS and darken the fleet through
  the guard); HTTP/2 stays off. Five bugs found on the way, each with a
  regression test: a `%` in a formatted due query, an untyped-parameter
  CASE that compared text to a timestamp, a run with nothing left to score
  overwriting the last real run, a fan-out race writing a results map
  without the lock, and the outcome signal filing the new classes as
  unknown. Config: `config/main/provider_egress_probe.yml` (max time
  4500 s with a validated 4160 s floor, load attempts 3, retry mean 300 s,
  tunnel re-creation 2, dark rules and guards, full concurrency 8,
  blackhole concurrency 16) and the new `config/all/egress-sites.yml` (the
  refresh settings, the Firefox 157 profile — adopted on the operator's
  word on 2026-09-24 for the 2026-09-25 release, in the module default too,
  and to be moved forward at every four-weekly Firefox release, as the file
  says — 54 candidates fetched five times on 2026-09-24, four declared
  incompatibilities). Tests through the lock: the whole monitor, api and
  grafana packages, the model, controller, taskworker and handler tests
  named in the report, the sim-latency provisioning test, the route check
  and the spec test; the refresh race reproduced under `-race` against
  the pre-fix code through an overlay. Left for the dashboard pass:
  `egress-probes.json` panels that still read the retired flag, source and
  diagnostic metrics, and the proved-failures panel missing the new
  classes.
- **Solver at the target (§5.3 D27, §5.8 item 1, 2026-09-24,
  `server/geo/solve`).** Colour-ordered Gauss–Seidel over a worker pool
  (`Workers`, `ParallelChunk`), bit-identical at 1, 2, 3, 7 and 10 workers
  and under shuffled terms; 24-byte terms with a `float64` round trip
  (28.0 bytes a term and 787 a node measured from the problem's arrays,
  3.34 GiB at the target); the pair reservoir seeded by fnv-1a of the pair's
  ids after the derive job's cursor tests caught the index-seeded draw
  (283 of 300 terms differed at two parts before the fix); the stagnation
  stop at 20 sweeps and 1e-3; `NodeResult.LastStepKm`, `PublishRefusalOf`
  and `Result.PublishRefusals` (`few_pings`, `few_peers`, `still_moving`,
  `no_improvement`). Two general defects found by the no-pings tests and
  fixed without a branch: the damped step never took the full step (a node
  2,160 km stale stopped 2.2e-9 km off), and one global line search coupled
  independent components (a node with no terms was thrown 2,870 km off its
  genesis by another component's multiple); the line search now runs per
  connected component. Acceptance (seeds 1/2/3): perfect pings 0.258 /
  0.222 / 0.062 km from perturbed geneses; median at the production
  reservoir 3.25 / 2.62 / 2.70 km and 0.71 / 0.83 / 0.70 km at 256
  samples; the minimum 36–45 km, failing as it should; the malicious subset
  source excluded at q 0.05 in every seed with honest q ≥ 0.936, and the
  truthful subset source never excluded; no pings: exactly 0 km cold and
  warm, and 5.4e-20 km over 200 random problems. Coverage is now distinct
  targets over `Node.ExpectedPeers` capped at 1 (0 keeps the old reading):
  no penalty for a designed sample at 1k, 10k or 100k sources, a source at
  a quarter of its sample alone marked down. Scale: 243 ns a term at 10k
  nodes and 311 ns at 100k on one core; speed-up 2.00 / 4.00 / 3.98 / 6.94
  at 2 / 4 / 8 / 10 workers against a calibration kernel on a host at load
  120 (69% of ideal at ten cores); the budget states 50% efficiency on 96
  cores, checked only when the host gives the kernel 80% of its cores. Full
  target behind `GEOMAP_SCALE=1`: 2M nodes and 72M terms generated in 4 s,
  solved in 145 sweeps and 1,044 s on ten loaded cores (300 sweeps and
  2,926 s before the stagnation stop), 1,197,391 publishable, residual
  38.77 km against 121.94 km at genesis, 3.74 GiB held by the solve and
  7.74 GB peak resident. Left for the design owner: a genuinely
  low-coverage source drifts to genesis as its q falls and is excluded on
  scatter or bias the round after, which `ExpectedPeers` removes for
  designed sampling only; and the per-node `MinStepKm` criterion is a weak
  stop for under-determined providers, which is what the stagnation stop
  and the `still_moving` refusal are for.
- **Derive job at scale (§5.3, §5.5, §5.8 item 2, §2.19c, 2026-09-24).**
  Per-cursor state is the solver aggregator and two counters; node ids come
  from a sharded id table (`stateLock` a shard, four a cursor), the node
  sets and attestation counts are derived once at the merge, each cursor
  builds its terms in its own goroutine, the merge sorts the terms and
  panics if two cursors read one pair, and a failing cursor cancels the
  rest. Identical terms at 1, 4 and 16 cursors in the pure and the DB test,
  with pairs above the reservoir; a million rows copied in 8.6 s and read
  in 2.09 / 0.65 / 0.85 s at 1 / 4 / 16 cursors on a loaded host; memory
  follows the pairs, 213–258 bytes a pair and under 5 a row. The provider
  expectation is `ProviderProbeSampleSize` 4 (the client's
  `ProbeWindowCount`; four targets read coverage 1 and q 1, one target
  coverage 0.25 and q 0.09, not excluded); `ExtenderPeerSampleSize` 64.
  The warm-start workaround is gone (the solver's full step made it
  unnecessary). `DeriveLocationsSettings` holds every former constant
  (`MaxTime` 30 m, `DeriveReadCursors` 16, `MaxSolveSeconds` 600,
  `MaxSolveBytes` 8 GiB, `SweepSafetyFactor` 3, the planner's priors, the
  log caps). The run record (a Redis list, not a table) gains the refusal
  counts by reason, `Stagnated`, the sweep counts and the measured costs;
  the planner projects from them (a tenth-of-target record projects
  453.6 s at ten cores and 1.13 GB) and the §2.19c signal gained
  `derive-capacity` (seconds and bytes at or above 1 − 0.2 of the recorded
  budget), a `still-moving` part of non-convergence at 5% of solved nodes,
  and both runs' refusals by gate on a published collapse; gauges
  `derive_solve_seconds{at}`, `derive_solve_bytes{at}` and
  `derive_ingest_seconds` with six panels. The gated fraction-of-target job
  seeded 5.2M pings in 188 s and then stopped on the host's disk (the
  docker volume was full); it needs about 20 GB for a tenth of the target
  and remains unverified here.
- **Ping table partitions (§5.7, §5.8 items 3 and 4, 2026-09-24).**
  Migration 713 renames `network_ping` to `network_ping_legacy`, creates the
  range-partitioned table under its final name with named not-null
  constraints (PostgreSQL 18 records them by name, which the create-then-
  rename order would have left as `_new`), one partition for every day that
  already has rows plus yesterday through two days ahead, and copies every
  row; 714 drops the legacy table; the monitor contracts check the
  partition key, all thirteen columns, the three `ON ONLY` index
  definitions and that every partition is `network_ping_p<day>` with
  matching bounds. No primary key (a partitioned table cannot key on
  `ping_id` alone) and no `create_time` index (it only served the row
  sweep), so the hourly ping count scans its day's partition. The sweep
  keeps today and two days ahead, drops whole days past retention, and
  never deletes a row (a static test reads the source for it); an insert
  that finds no partition creates it and retries once, and the
  check-constraint error that `server.Tx` used to retry for a minute is
  raised as a typed error first. Replay: the unique key carries
  `create_time`, so `AddReportedNetworkPings` takes a per-pinger advisory
  lock at read committed, looks back over the key prefix and inserts in
  one transaction; a forced race is proved by row count. Measured: 94.8 µs
  of CPU a ping (2.04 ed25519 verifications), 10,549 pings a CPU-second
  here and 10,911 on the 45 µs reference core against the 10,000 budget
  and the target's 6,111 a second (0.61 core); 532 bytes a co-signed row
  (283 heap, 250 index) against 560 in the growth model; 528M pings a day
  at the target, 275 GiB a day, at most three partitions holding rows, a
  peak of 1.07 G rows or 558 GiB, and a sweep outage of 48 h still leaves
  every insert a partition; the worst hour posts 133 of 240 allowed. Open
  at the target, recorded here: the dashboard's `CountExtenderPings`, the
  §2.19c pings query and `GetNetworkPingTermRange` each read a full day and
  will need hourly rollups. Two defects fixed on review: the report limit
  was keyed by user, so one operator's fleet shared 240 posts an hour; it
  is keyed by the reporter's client id, and a test posts 240 from each of
  two extenders under one user. And a claim posted with the full 24 h
  forward clock skew could outlive the partition holding its original
  copy: `MaxForwardClockSkew` is 5 minutes, the sweep drops a day only past
  `KeepTimeout()` (25 h 5 min), the replay lookback covers the same span,
  and a test posts a claim a minute before midnight at the maximum forward
  skew, runs the real partition rule at every hourly sweep, and shows the
  replay refused at the last instant the claim is still in time while the
  bare retention would have dropped the day first. The former constants
  live in `ExtenderPingReportSettings` (rate limit 240 an hour, 256 pings
  a report, 64 uncosigned a post, skews, retention, sweep hourly, derive
  every 8 h); the §2.19c signal reads the same settings.
- **Directory cap and peer sample (D26, `EXTENDER.md` E6 and G5,
  2026-09-24, connect and sdk).** `MaxActiveRecordCount` 512 (zero or less
  unbounded; the own record and manual addresses counted but never
  evicted; a new record beyond the cap evicts a random record that is
  neither on the hinted continent nor measured, then the oldest measured,
  then the oldest same-continent; constant-time apply, evict and draw
  through an index of pools; a store saved under a larger cap loads within
  the current one), `PeerSampleSize` 64 (hinted-continent peers first, then
  a random slice redrawn every refresh, a member that leaves replaced by a
  fresh draw, a new same-continent peer joining at once; `Status` gains
  `SampleSize` and `SampledPeerCount`). The per-record address cap rose
  from 512 to 2048 so dual-stack records are not evicted by the address
  cap first. The A12 subnet widths and minimum subnet count became
  settings (`AdmissionIpv4PrefixBitCount` 29, `AdmissionIpv6PrefixBitCount`
  56, `AdmissionMinSubnetCount` 4096). Scale tests: a million records into
  the capped directory in 1.22 s holding 714 KB (250k in 1.99 s under
  -race, extrapolated); selection over 100k known peers in 93 µs of CPU a
  refresh, exactly 64 pinged with the hinted 20 first. Two ring buffers
  that shifted on every add (the directory's event ring and the pinger's
  ping ring) now move a head index. Eight test sleeps reviewed: four
  replaced by monitors and a `PassAfter` seam, four kept with a comment
  naming the signal that makes them safe.
- **Code style sweeps (2026-09-24).** operator-proxy: 292 audited findings
  cleared across its eight packages, every exported acronym identifier
  renamed (`ExitIp`, `PoolUrl`, `HttpClientForHosts`, `FailureNoExitIp`
  and the rest; JSON keys, flags, metrics and environment names unchanged),
  including pre-existing exported names in the touched packages; three
  package constants became settings; test data moved to `.example` and
  the documentation ranges; the module's tests pass race-clean and, for
  the eight TLS tests that skip on macOS, in a Linux container. server:
  262 findings cleared in the 51 files no phase agent owned (geo, GeoLite2,
  the seeder, the restored proximity controller, the extender handlers),
  with the `geo` package's import of its own `solve` child removed
  (`LatLon` moved up to `geo/latlon.go`), constants moved into
  `LocationDeduplicationSettings`, `ExtenderLatencyReportSettings` and
  `RemoveOldExtenderLatenciesSettings`, and test addresses replaced by
  MaxMind test-data or documentation addresses; connect and the sdk clean.
  A debugging test that looped over real public addresses was deleted from
  the prober's controller test. Left in HEAD code for the operator: a
  ULID-shaped client id in three operator-proxy credential tests that may
  be copied from a live deployment.
- **Place list in memory (§4, 2026-09-24).** Measured on the real list
  (11.6 MB of YAML, 77,753 cities): the parse allocates 315 MB and peaks
  near 285 MB in use, but what stays live is 9.6 MB for the list plus
  7–18 MB for the matcher's name index and 9.3 MB for the derive job's
  containment and representatives, about 37 MB; the ~330 MB seen resident
  is the parse's transient peak the runtime keeps mapped. Nothing loads
  the list at startup in any binary. The api and connect servers reached
  it through `CreateLocation` (a prober submission, an extender
  activation, a client connect) and now read it only for a place with no
  row under its geoname id — a country never, a city or region already
  stored never — behind one indexed existence check made before the
  transaction, so no connection is held during a load; the taskworker
  keeps it for the whole process because the derive job needs it every
  eight hours; bringyourctl loads it only for `locations add-default`;
  gossip, mcp, monitor, proxy and the competition binaries cannot reach
  it. The two independent loads (the seeder's and the lazy one) that could
  parse the list twice in a taskworker are one shared load now.
- **Ping tallies (§2.7, §5.7, 2026-09-24).** On the operator's ruling that
  hourly counts are useful, every dashboard and monitor ping count reads
  tallies the ingest keeps in the ping insert's own transaction: a
  fleet-wide hour tally keyed (hour, shard, pinger kind, relayed, cosign,
  cosign reason) with zero-round-trip and beyond-half-planet counts (the
  shard is the pinger id's last byte mod 16, so a fleet's reports do not
  queue on a few row locks), a per-target hour tally, and pinger-day and
  target-day tables for exact distincts; the three large ones are day
  partitioned on network_ping's days, the small one is deleted by whole
  day (migrations 715–718 with backfills checked against the rows,
  monitor contracts, SIGNALS.md rows). `CountExtenderPings`,
  `CountExtenderPingsByHour` and the §2.19c pings query read the tallies;
  the derive job stays on the rows. The rolling 24 h distinct gauges are
  exact over the UTC days covering the window, at most a day wide. A
  replayed or refused report changes no tally; a test seeds three days of
  pings of every kind and asserts each tally equals the direct count. A
  report now costs about seven statements instead of one insert.
- **Verification (2026-09-24).** operator-proxy: the whole module passes,
  race-clean, and its eight macOS-skipped TLS tests pass in a Linux
  container. connect: the full suite (three known-hanging tests skipped)
  passes but for the four root memory-accounting tests that fail at a
  clean HEAD export too and one dual-stack tun dial race that fails only
  under the host's load and passes alone. sdk: the full root suite passes
  but for one Happy Eyeballs socket test, which passes four of four with
  the working-tree sdk against a connect copy holding only this plan's
  changes and fails only with the other session's in-progress dial-race
  changes in connect, so it is not this plan's. server, one serialized
  run through the suite lock over every package this plan touched: root,
  geo and geo/solve, cli, model (58 minutes), api, taskworker and
  taskworker/work, monitor, grafana and bringyourctl all pass; controller
  fails only on the thirteen pre-existing competition-fixture tests in
  files this plan never touched, and no extender, ping, location, egress
  or derived-location test fails.
- **Peer gate of three (D11, §5.4, 2026-09-24, server).** `MinDerivePeers`
  is 3 in `solve.DefaultSettings()`, its one definition: the derive job
  gates, and counts the nodes published at exactly the gate, from the
  solver's settings, and the §2.19c signal's `MinDerivePeers` defaults to
  the solver's (its `at_min_peers` query now reads `peer_count <= 3`); no
  file under `config/` carries it. The thin-evidence finding, SIGNALS.md
  §2.19c and the extenders dashboard's derived-locations description say
  three. Tests: the gate's table asserts each case's refusal reason, two
  peers `few_peers` however many pings and three published; a solve with
  two pinned peers settles a wide-genesis node 0.05 km from the mirror image
  of its truth, 400 km off, with a 0.03 km residual, which a gate of two
  publishes and three refuses as `few_peers`, while a third peer off their
  line settles it 0.16 km from the truth and publishes it. The still-moving
  fixture gives every node three peers (the anchors ping each other, the
  weak node all three anchors and still slides 3.1 km in the third sweep),
  so its tests keep their expectations, and the refusal count gained a node
  with two peers. In the database, two providers at Middle placed at
  Westport, one pinging West and North, the other East as well: a gate of
  two publishes the first 434.6 km off, at the mirror, and the second
  6.49 km off; the next run under three refuses the first (`few_peers` 1 in
  the result and the run record, its row removed) and publishes the second,
  the one node at exactly the gate. Acceptance (§5.6) unchanged to the
  digits above.

## 10. Step 6 — the egress index replaces the net-type score

### 10.1 What the score is today, and why it stopped meaning anything

FindProviders2 hands the client one `tier` per provider for the rank mode it
asked for: `quality`, the default, or `speed`. The client-score job
(`UpdateClientScores` in `server/model/network_client_location_model.go`)
builds it from three things — the provider's **net-type score** for that
mode, its latency test and its throughput test:

```
score = min(20 · net_type_score[mode] + adjust, MaxClientScore)
tier  = score / 20
```

`adjust` grows by one per 20 ms (quality) or 5 ms (speed) of relative
latency past the mode's threshold, and by one per 200 KiB/s (quality) or
1 MiB/s (speed) of throughput short of it; a missing latency or throughput
test costs 40, two tiers, each; a result past the mode's cutoff excludes the
provider from that mode (score 0, tier at the maximum). The net-type score is
therefore the **base tier**, and the performance tests move a provider within
or past it. A tier of 0 means "nothing known against it, and fast", and the
client tests for exactly that (`Tier == 0`) before it lets a network's own
provider carry a fresh web session.

The net-type score is two generated columns on every connection row, rolled
up per provider as the maximum over its connections
(`network_client_location_reliability.max_net_type_score` and
`max_net_type_score_speed`):

```
net_type_score       = net_type_privacy + net_type_virtual + net_type_hosting + net_type_foreign
net_type_score_speed = net_type_privacy + net_type_virtual                    + net_type_foreign
```

After §3 those inputs are: `privacy` and `hosting` from the egress probe's
ip-intelligence verdicts and from nowhere else; `virtual` always 0, its one
source having left with ipinfo; `foreign` from the ARIN organisation country
disagreeing with the GeoLite2 country of the control address. An unprobed
provider scores 0 in both modes and looks clean; a probed hosted provider
scores 1 in quality and 0 in speed. The "connection type" the columns were
named for no longer exists, and the two buckets differ by one flag on a
minority of providers.

What **gates** a provider is separate from the score (`providerCountFilter`,
same file). A current blackhole verdict or a TLS-authentication failure
excludes it from both modes and from every count — but only as a *minimum*:
`force_minimum` on the request re-admits everything that failed a minimum,
hard failures included, and a provider named by `client_id` in a spec is
returned with no eligibility check at all. The broader rule —
at least 90 % of the scored destinations of its latest health run passing
(`10·ok ≥ 9·total` over the dns, connectivity, cdn and site classes) and the
exit **observed** in the country the provider is published under — sits
behind `providerEgressTestEnabled`, off by default, and fails closed for a
never-probed provider when on. The **reputation** class, whether large
vendors treat the exit as a datacenter address, is stored and never scored,
and its refused vendor names go to the client as `reputation_failed_names`;
the health model says why: that is a fact about a vendor's ip feed, not about
whether the provider carries traffic.

### 10.2 What the two buckets mean

- **Quality** is the providers that pass every probe that must pass and all
  or most of the rest: reachable and safe, and at most one in ten of the
  real sites the probe loaded through them failed after its retries. Within
  the bucket an index of what did fail orders them, and the performance
  tests order within that. There is no hosting, proxy or reputation
  verdict anywhere in this: what real sites do with the exit is the only
  fact (§11).
- **Speed** is every provider that is reachable and safe and not gated on
  its country, ordered by latency and throughput alone.
- **Online** is the third bucket, and the last resort: providers with no
  probe verdict at all — no negative mark but the exclusions, no positive
  one — that real traffic shows to be working. Its evidence is the
  reliability score real traffic has earned it: it passes the reliability
  minimums and the speed-mode score maximum the ranking already applies to
  every provider, so a provider no client has measured is not online. It is
  never asked for by a client; it backfills the other two when a probe
  outage or a thin location leaves them short. Hosting, proxy, reputation and a
  partial failure do not touch it: a user who asked for speed asked for the
  fastest working exit.

### 10.3 The rules (D23)

**Hard exclusions.** Reachability and security are not minimums. A provider
with either of these is **absent from the results altogether**, for as long
as the verdict is current:

1. a current dark verdict — the exit carried nothing on this connection
   across the consecutive checks of §11.3, not one failed check;
2. a TLS-authentication failure on its latest health run — a forged
   identity on one destination is not one failed destination among many.

Absent means absent: in either rank mode, with `force_minimum` set, when a
spec names it by `client_id`, as a network-only provider of the caller's own
network, in every location count, and under every fallback the count logic
has. The exclusion is applied where the result is assembled, not only where
the minimums are, so nothing downstream can re-admit it; it lifts by itself
when the next hourly blackhole check passes or the next health run carries
no authentication failure. A blackholed provider is unusable and a
TLS-intercepting one is unsafe, and no caller's preference changes either.

**Gate, both buckets.** A provider is out of both buckets and out of every
count, as a minimum, while a fresh probe (within
`ProviderEgressLocationMaxAge`, 7 days) observed the exit in a country other
than the one the provider is published under. This sits behind the flag today
and becomes unconditional: it is an honesty check, not a quality one, and it
needs no fleet-wide coverage to be fair, only a probe of that provider. It
stays a minimum rather than a hard exclusion — `force_minimum` and an
explicit `client_id` still reach the provider, which is reachable and safe,
just not where it is listed. Under the precedence of §6 a fresh probe *is*
where the provider is published, so this gate can only fire against a
derived location that crossed a border away from the observed exit. The
derive job therefore refuses to publish a crossing that contradicts a fresh
probe's country — one more gate in §5.4 — and the ranking gate is the
backstop.

**Quality membership.** Past the exclusions and the gate, a provider is in the quality bucket
only if its latest health run passes the 90 % rule: at most one in ten of
the sampled real-site loads failed after their retries (`10·ok ≥ 9·total`,
as today, now over loads that had every retry). A provider with no fresh
evidence is in or out by the flag (below). Nothing else decides membership:
the hosting and proxy verdicts the probe used to take from ip-intelligence
vendors are gone (§11), and the sites those vendors were guessing about are
in the sample themselves.

**The egress index** orders the quality bucket. It is a weighted sum of what
the probes found; the weights are settings (`EgressIndexSettings`,
`DefaultEgressIndexSettings`) with these defaults:

| Evidence, from the latest health run | Weight |
|---|---|
| each real-site load that failed every one of its retries, by class: dns, connectivity, cdn, site | 1 each (`ClassWeights`, per class) |
| cap on the sum of the row above | `MaxFailureIndex`, 6 |

The index is the probe's failures, directly: a load that failed after `n`
attempts is a site a user could not reach through that exit, whatever the
reason, and every such site counts one. The sites vendors used to score
"reputation" on — the ones that refuse addresses they take for datacenters —
are ordinary site destinations now (§11.1), so an exit they refuse pays for
it here exactly as it would for a site that timed out. The cap keeps a
broken run from burying the performance adjustment; the per-class weights
are settings so a dns failure can be made to cost more than a cdn one once
real data says it should. A provider with no evidence has no index: it is
in the online bucket (below), where the reliability score orders it, and
the ARIN foreign flag — the last vestige of guessing at an exit from its
address — is retired with the net-type score.

The index takes the net-type score's place in the quality formula; the speed
base is 0:

```
quality: score = min(20 · egress_index + adjust, MaxClientScore)      unless excluded
speed:   score = min(                     adjust, MaxClientScore)
```

Each mode's performance thresholds and cutoffs are unchanged, and the client's
`Tier == 0` checks keep their meaning: nothing found against it, and fast.

**The online bucket.** A provider with no probe evidence — none within
`EvidenceMaxAge`, or a run under `MinScoredLoads` — is in the online bucket
when it passes the exclusions and the gate and the minimums the ranking
already applies to every provider other than the probe ones: the
per-lookback `IndependentReliabilityWeight` floors of `PassesMinimums`, and
the per-lookback speed-mode score under the maximum (40) — whatever its
contract count or bytes. The reliability score is the strongest unbiased
signal there is about a provider — earned from real traffic, by every
client, over time — and it already decides whether a provider is offered
at all; the score maximum is the same rule the speed bucket applies, so a
provider no client has ever measured (the missing-test penalty alone
reaches the maximum) is not online, exactly as it was not offered before,
while a provider past a speed cutoff scores 0 and is. The bucket is
ordered by reliability weight, then by the speed-mode performance
adjustment (the client-measured latency and throughput the rollup already
holds). It replaces the rollout flag's job: an unprobed provider is in the
online bucket, not in quality, and not in speed's native set; the counts
include it (the count path still includes a never-measured provider, as
HEAD's did, so the count and the online answer differ for such a
provider).
`providerEgressTestEnabled` keeps only its old meaning for a database
migrated ahead of the rollup.

**Backfill.** Backfill makes every answer look full, so the operator's view
of it is the `provider_backfill{rank_mode}` metric and the sustained-backfill
alert of `SIGNALS.md` §2.19b, never the answer itself. A bucket that comes
up short of the request's count is filled from the others in a fixed order, so a location with few quality
providers still answers a quality request, a location with few fast ones
still answers a speed request, and a mass probe failure — every verdict
gone, or every verdict wrong — still answers from what real traffic
proves. Quality short: first the speed bucket's providers that quality does
not hold (over the one-in-ten line), in speed order; then the online
bucket. Speed short after its own performance cutoffs: first the quality
providers those cutoffs excluded, in quality order; then the online
bucket. A borrowed provider keeps its own tier plus `BackfillTierOffset`
(11 by default, never under 3; twice that for the online bucket), so every
native provider ranks ahead of every borrowed one and the borrowed keep
their order among themselves; the shape of the answer does not change. The
offset leaves room for the largest demerit a client adds to a tier after
the answer — 7, from connect's `effectiveTier`: unproven +1, dial-starved
+2, quarantined +2, unhealthy +1, busy probe +1 — on either side: a
demerited native (at most 2 + 7) still ranks ahead of every borrowed
provider (at least the offset), which needs 10, and a provider borrowed
past its cutoffs (3 + offset + 7) still ranks ahead of the online bucket
(twice the offset), which needs 11. Nothing crosses the exclusions or the
country gate.
`force_minimum` keeps its meaning as the caller's blanket override;
backfill is the default behaviour beneath it.

**Counts.** The number an app shows when a user picks a location is the
set past the exclusions and the gate — the supply a user can actually use — not the quality set; the online bucket counts, because a provider real
traffic proves is supply.

**The rollout flag.** The online bucket takes over what the flag decided:
an unprobed provider is in the online bucket and in the counts, borrowed
behind the probed when a bucket is short, and never fails closed.
`providerEgressTestEnabled` keeps only its old meaning for rows the new
rollup has not written, and goes with the generated columns.

### 10.4 Where it lives, and the transition

- **Computed by the reliability rollup** (`server/model/network_client_reliability_model.go`),
  per provider, from `provider_egress_health`, `provider_egress_location`,
  the blackhole set and the TLS set — the tables the gates already read —
  and stored on `network_client_location_reliability` as three nullable
  columns: `egress_index smallint NULL`, `egress_quality bool NULL` (passes
  the 90 % rule) and
  `egress_evidence_time timestamp NULL` (the newer of the two runs). The
  client-score job reads them; nothing new is written to connection rows.
  `net_type_hosting`, `net_type_privacy` and `net_type_foreign` are no
  longer read by anything (§11 and the online bucket) and go with the
  generated columns.
- **The old fields stay authoritative wherever the new ones are empty.**
  For any provider whose `egress_index` is NULL — a rollup that has not run
  with the new code, or a database migrated ahead of the binary —
  FindProviders2 ranks exactly as today: `max_net_type_score` as the quality
  base, `max_net_type_score_speed` as the speed base, today's membership
  rules. The two generated columns and the two rollup maxima are kept one
  release on that rule and dropped after it, when their last reader is gone.
- **Diagnostics.** `bringyourctl` shows the index and the exclusion reason
  per provider; the provider dashboard gains the index distribution per
  bucket and the excluded-by-reason counts — blackhole, TLS, country, health,
  unprobed — so a probe outage shows as a wave of "unprobed" and not as a
  silent drop in quality supply.
- **API.** No shape change: `rank_mode`, `tier` and `reputation_failed_names`
  stay as they are; only what `tier` means per bucket changes, and the spec's
  descriptions say so. The spec conformance registry is untouched.

### 10.5 Measured on main (2026-09-23, read-only)

The rules above, run as one read-only query against the production rollup
and probe tables (pool = connected, valid, active providers):

| | providers |
|---|---|
| pool | 91 026 |
| blackholed by the hourly check within 3 h | 41 654 |
| TLS-authentication failure | 45 |
| observed country differs from published | 0 |
| **speed bucket** (past the exclusions) | 49 372 |
| speed bucket passing the speed performance cutoffs | 2 990 |
| health run within 24 h / 7 d / ever | 7 649 / 10 159 / 11 153 |
| location probe within 7 d | 6 806 |
| **quality, flag off** (unprobed stay, two tiers down) | 46 285, of which 45 343 unprobed |
| **quality, flag on**, hosting *or* proxy disqualifying | 940 (262 past the quality cutoffs) |
| quality, flag on, only hosting disqualifying | 2 752 (643 past the cutoffs) |

Under the final rules, re-run the same evening (pool 92 069; the dark
exclusion bracketed, because the consecutive-check rule of §11.3 needs
history the single-check table does not have — the upper bound applies
today's single failed check, the lower bound counts a provider dark only
when its failed check ran on its current connection and nothing has flowed
through that connection):

| | darks as today | darks by the strict proxy |
|---|---|---|
| **speed bucket** | 51 626 | 91 928 |
| speed bucket passing the speed cutoffs | 3 291 | 3 605 |
| **quality, flag on** (90 % rule over every load, the four "reputation" sites included) | 616 | 1 088 |
| quality passing the quality cutoffs | — | 300 |
| **online bucket ceiling** (was "quality, flag off": every unprobed provider, before the reliability minimums) | 48 214 | 85 360 |
| probed and healthy but over one in ten loads failed, so speed only | — | 6 568 |

The four "reputation" sites decide almost all of it. A run samples about
26 loads (5 cdn, 5 connectivity, 4 dns, 12 site) plus those four; they
refuse three or four times for 95 % of providers, which is over the one-in-
ten line by itself: 5 760 of the 7 676 probed providers pass the rule on
the scored classes alone, 1 088 pass it with the four counted, and every
quality provider carries an index of 2 or 3 from them (10 at 1, 130 at 2,
948 at 3, none at 0). So the rule as it stands measures those four sites'
treatment of the probe's request shape more than anything else, and a
sample of 26 makes the one-in-ten line a two-failure line. Two settings
follow: the rule and the index read runs of at least `MinScoredLoads`
(default 50), and the retries and browser-shaped fetch of §11.3 ship before
the rule is turned on. Three things the numbers say that the design did not:

- **Half the connected pool is recorded dark.** The hourly check keeps one
  row per provider, and 45 689 of the 93 945 checked in the last three hours
  failed every destination (294 more on TLS). Today's FindProviders2 already
  drops them as a minimum; the hard exclusion changes only `force_minimum`
  and `client_id`. Whether that many providers truly carry nothing, or the
  checker's tunnel to an idle device reads as dark, is the first thing to
  verify when the exclusion becomes hard.
- **The proxy verdict was on 79 % of probed exits** (5 507 proxy-only, 443
  with hosting, 122 hosting-only, 1 631 neither, of 7 703 probed in 7 days):
  ip-intelligence vendors list a provider's address *because* it provides.
  With proxy disqualifying, quality would have held 940 providers; with
  hosting alone, 2 752. That is why the verdicts are gone (§11): the
  vendors were guessing at what the sites themselves now say.
- **The "reputation" sites refuse almost everyone.** Every run samples four
  of them, and among healthy providers 95 % are refused by more than half
  (akamai, epic-games, etsy, ecosia, reuters, canva and reddit each refuse
  over 2 600 of 6 577). Either those sites refuse every proxied request, or
  they refuse the probe's request shape rather than the exit; the retries of
  §11.3 and a browser-shaped fetch are what will tell. Until then they are
  ordinary site loads, and their refusals count in the index like any other
  failure — which means a majority of healthy providers start with an index
  of two to four from those sites alone, and the cap matters.

Today's score, for contrast: 97 % of the pool carries a net-type score of 1
or more (3 204 at 0, 30 500 at 1, 22 024 at 2, 46 924 at 3 over the
connected rollup), so the current tiers separate almost nothing.

### 10.6 Tests

Pure: the index from every combination of evidence (failed loads per
class, the cap, stale, missing, foreign on an unprobed provider), the
quality decision, both flag states, the weights as settings. Database: the
rollup writes the three columns from seeded probe rows; a provider with NULL
columns ranks exactly as before, on the old fields; an exit that failed more than
one in ten loads is out of quality and in speed; a blackholed or TLS-failed provider is absent
in both modes, with `force_minimum`, when named by `client_id`, as a
network-only provider, and from every count, and returns by itself once the
verdict clears; a country mismatch gates as a minimum and `force_minimum`
re-admits it; an unprobed provider under
each flag state; the counts equal the gate-passing set; the tier formulas of
each mode; the online bucket — a provider with no evidence that
passes the reliability minimums is online, one under them is not, one with
fresh evidence is not; it counts as supply; its order is reliability weight
then client-measured performance — and the backfill as protection
against a mass probe failure — every probed provider over the one-in-ten
line still answers a quality request in full from speed, then online;
every verdict gone (the probe pipeline down) still answers both requests in
full from online; every provider slower than the speed cutoffs still
answers a speed request in full from quality, then online; online is
borrowed last and tiered behind the other borrowed; two natives and eight borrowed keep
their order and tiers with no provider twice; an excluded provider is never
borrowed even when both buckets are empty, so the answer is short instead;
old rows with a NULL index are borrowed under the old rules; a missing
other-mode cache degrades to the native set without error; and
`provider_backfill{rank_mode}` counts the borrowed so a mass failure shows
as a wave of backfill; the derive job's refusal to publish a crossing against a fresh
probe.

## 11. Step 7 — the prober loads real sites, retries, and decides for itself

### 11.1 Historical baseline before Step 7

The pre-Step-7 egress prober (`server/taskworker/work/provider_egress_probe_work.go`
over the `operator-proxy` packages) opens a tunnel to a provider exactly as
a client would — a multiclient pinned to that one provider, a gvisor tun and
a packet pump (`providertunnel.Open`) — and measures through it. Two passes
share the recurring task:

- **The full run** samples about 131 destinations in four scored classes
  (dns, connectivity, cdn, site) plus a "reputation" class of sites known to
  refuse addresses they take for datacenters; the reputation tally is stored
  but never scored. It also asks a consensus of ip-intelligence vendors
  where the exit is and whether it is hosting, proxy or mobile, and stores
  that as the provider's probed location and flags. Each destination is
  fetched once.
- **The blackhole check** draws three connectivity destinations, fetches all
  three concurrently the instant the tunnel object exists, with one 15 s
  timeout each and no retry, and writes `ok = false` when none answered.
  Main runs it four shards wide at 52 tunnels each, 250 providers a batch.
  One failed check makes the provider dark for up to three hours, including
  after it reconnects.

This is the historical no-retry/single-failure baseline, not the current dark
rule. The current source retains measured blackhole verdicts for eight hours,
with passing checks still due after ninety minutes; ordinary negatives need
the consecutive-failure/span rule below, while TLS-authentication failure
remains immediate evidence. NotMeasured does not refresh an older measured
clock. The eight-hour retention change requires matching API/Taskworker and
monitor rollout; it is not a claim about the currently deployed artifacts or
about improved measurement throughput. The dated observations in §11.2 keep
their original three-hour measurement window.

### 11.2 Historical dark-cohort observation (main, 2026-09-23)

45 689 of the 93 945 providers checked in three hours were recorded dark,
every one of them "all destinations failed" over a tunnel that had opened.
These observations motivated the cold-start and retry changes. They do not
join each failed check to its exact provider connection, route, and deployed
artifact, so they do not prove most individual verdicts false or establish
the same cause for a later dark cohort:

-  98 % of the selected dark cohort (39 523 of 40 138) also had throughput
  samples for an observed connection in the inspected window; those samples
  are not joined to the failed probe attempt. Every connected provider's
  observed connection was under six hours old, after a
  platform restart between 21:00 and 24:00 UTC.
- The rate is uniform across all four edge hosts and every block, so the
  platform side is not the variable. It rises with distance — Vietnam 86 %,
  France 80 %, Britain 77 %, Japan 75 %, the United States 47 %, Germany
  36 % — with the connection's expected latency, and steeply with the
  connection's youth: 86 % for providers checked before their current
  connection existed or within its first ten minutes, 42 % past an hour.
- The same connectivity destinations answer 93 % of the time in full runs,
  so three independent failures should be near zero, not half the fleet.
  Real clients reported no provider dark in 24 hours.

One plausible mechanism is in the check: the tunnel's open returns
before any path to the provider exists, so the 15 s budget of the first and
only attempt must cover the provider window, the contract, the in-tunnel DNS
resolution and the TLS handshake, under 208 concurrent tunnels per prober
host — and the day's changes (per-tunnel transport and DNS state in the
prober, per-instance carrier limits and the H1+ default in connect) all
lengthen that cold start. A provider whose connection was mid-restart when
its check ran fails every attempt by construction. One attempt, no retry,
and a verdict that outlives a connection could turn a slow cold start into
a long-lived exclusion. Confirm an incident with attempt-level evidence,
deployment ancestry, and a matching causal test before assigning this cause.

### 11.3 The rules (D24)

**Only real sites, only our data.** The prober consults no ip-intelligence
source for anything. The reputation class is dissolved into the site class:
its sites are ordinary destinations, sampled and scored like the rest, and
their refusals are failures like any other. Hosting, proxy and mobile
verdicts cease to exist; the columns that carried them stop being written
and go one release later. Where the exit is comes from the operator's own
data: the prober fetches the operator's `/my-ip-info` echo (the public
surface of §3.3; written `/ip` elsewhere in this document for short)
through the tunnel,
which answers with the address the platform saw and its GeoLite2 place, and
that place — country, city, accuracy radius, geoname ids — is the probed
location. No vendor consensus, no second source of truth beside §3.

**Every load gets its tries, spread out, and looks like a browser.** A
destination is fetched up to `LoadAttempts` (default 3) times and fails only
when every attempt failed. The attempts are not fired back to back: after a failed attempt the next
one waits a random delay with a mean of `LoadRetryMeanInterval` (default
5 minutes; drawn from an exponential distribution and capped at three
times the mean), so a site's momentary block, a rate limit or a flapping
path is not hit three times in one second, and the requests to any one
site look like a person coming back to it, not a scanner. A run therefore
spans ten to fifteen minutes for a site that keeps failing; the tunnel
stays open for the run, the loads of a run interleave rather than queue,
and the prober's per-shard concurrency (`FullConcurrency`, `BlackholeConcurrency`)
is sized for tunnels that mostly wait — many open, few busy — and is a
setting to calibrate against the prober host's memory, since each tunnel
carries its own network stack. The
first fetch of a run is a warm-up of the `/ip` endpoint with its own
timeout, so the path is up before any scored load starts its clock. Every
request is shaped like a normal user's browser making a top-level
navigation: a current mainstream browser's user agent and the headers it
sends with it (Accept, Accept-Language, Accept-Encoding, the Sec-Fetch
family, Upgrade-Insecure-Requests), HTTP/1.1 for now — the tunnel's client is shared with the
bandwidth probe, which needs one stream per connection, so HTTP/2 waits
for a health-only client — and no Range header — the probe reads the first kilobyte and closes the body
instead, since a byte-range request is one of the things bot managers
refuse. The header profile is a setting (`BrowserRequestProfile`) refreshed
with the site pool (§11.4), so it does not go stale with the browsers it
imitates. Every destination carries the countries and (country, region)
pairs it is known not to work from, and the prober draws a provider's
sample only from the destinations compatible with the country and region
the provider is published under — the due list carries both — so a site
blocked in a country never counts against that country's exits; the server
ignores any such load that arrives anyway. The per-class sample sizes are
met from the compatible set, and a class too thin for a region is a signal
(§11.4), not a smaller sample. The stored result is per destination after
retries: `ok_count`, `total_count`, the class tallies and the failed names
mean "failed every attempt", which is what the index (§10.3) and the 90 %
rule count.

**A check is not a verdict.** The blackhole check keeps its three
connectivity destinations, each retried as above after the warm-up. Its three loads get their tries at the same mean spacing, so one check
takes up to about fifteen minutes when everything fails. A failed check
records a failure and schedules the next check after a backoff
(`DarkBackoff`: 5, 15, 30 minutes); a provider is **dark** only after
`DarkConsecutiveFailures` (default 3) failed checks in a row, spanning at
least `DarkMinimumSpan` (30 minutes). One passing check clears the count.
The due query interleaves existing due checks and first checks 1:1 whenever
both queues have work, with unused share lent to the other queue. Existing
checks keep oldest-`next_due_at` order (legacy rows use `checked_at + 90m`),
then client id; first checks keep client-id order. An existing due check leads
an odd-sized response, and a one-slot caller retains retry priority. A
one-slot caller therefore does not have the two-class fairness guarantee.
Each independent head reads at most the requested limit; a bounded stable
merge returns at most that limit and deduplicates category changes between
reads. This is a share of one caller's work, not a global budget or extra
parallelism. Backoffs, dark rules, NotMeasured preservation and eligibility
are unchanged. Existing NotMeasured-only rows still belong to the existing
queue, not the never-checked class.

Absolute existing-row priority previously excluded all first checks whenever
renewed due work outpaced the sweep. Equal sharing preserves admission for
both failed-provider recovery and first-check coverage; it cannot promise
their deadlines when throughput is insufficient. Monitor each class's backlog
and actual selected/completed evidence separately rather than treating a full
returned batch or a healthy retry as whole-fleet recovery.
`provider_blackhole_check`
gains `consecutive_failures`, `first_failed_at` and `next_due_at`; the
current-dark set of §10.3 is the rows whose count has reached the threshold
within `ProviderBlackholeCheckMaxAge`.

**A retry re-creates the tunnel.** A run or a check whose tunnel dies
part-way — the provider disconnects, the path is lost — does not fail the
loads it had not reached: the next attempt of every pending load first
re-opens the tunnel (a new proxy device to the same provider) and continues
through it. A run re-creates its tunnel at most `TunnelRecreateAttempts`
(2) times, since each re-creation is a new device and a new contract; only
when the tunnel cannot be re-created within that or the load's remaining
attempts are those loads recorded as *not measured* — never as failed — and a health run with unmeasured loads is submitted over the loads
it did measure, a check with none is simply rescheduled. Without this,
every provider that churns during a fifteen-minute run would fail the loads
it never got to, which is exactly the false dark of §11.2 in a new form.

**A check counts against the provider.** Consecutive failures and passes
are the provider's, whatever its connections did in between: a reconnect
is nothing the check needs to know about, since the retries and the
re-created tunnels already carry a check across one.

**The prober watches itself.** A batch whose dark share exceeds
`DarkBatchGuard` (default 20 %) is the prober's fault until proven
otherwise: its negative results are discarded, the batch is retried after
the backoff, and the guard trips a metric and an alert. The same guard
applies to full runs: a batch whose scored-load failure share exceeds
`RunBatchGuard` (default 30 %) is not submitted, because a CDN outage, a
broken request profile or a saturated prober host fails everyone at once
and would stamp every provider probed in that window with a bad index for
days. The fleet-wide dark share and failure share are gauges with the same
alert. Concurrency per shard drops from 52 to `BlackholeConcurrency`
(default 16) until the cold-start cost is measured; every number here is a
setting.

**Storage.** `provider_egress_health` keeps its shape with the new meaning
of its counts; `provider_egress_location` keeps `location_id`,
`country_code`, `city_confident` (now: GeoLite2 accuracy radius at or under
`CityConfidentRadiusKm`, 25 km), `asn` and `org` if the `/ip` answer carries
them, and loses hosting, proxy and mobile; `provider_client_verdict` is
unchanged. `applyProbedNetTypes` and the vendor geolocation package go.

### 11.4 The site pool refreshes itself

The verdicts are only as good as the sites the probe loads, and a site that
fails every exit says nothing about any of them. So the pool is data, not
code, and a task keeps it representative:

- **The pool.** `provider_egress_destination` holds the active sites per
  class with their load contracts (url, expected status or body check,
  byte cap) and a **candidate list** of representative sites by category —
  search, social, news, shopping, video, gaming, documentation — and by
  region, seeded from the prober's built-in table and extended in
  `egress-sites.yml`. The prober fetches the active set at the start of
  every pass (`GET /network/provider-egress-destinations`, under the
  operator secret the egress routes use) and falls back to the built-in
  table only when the server has none.
- **The refresh** (`RefreshEgressDestinations`, daily, `SiteRefreshInterval`)
  judges every active site by our own data: over `SiteWindow` (3 days) of
  runs on **healthy** exits — providers that passed nine in ten of the
  *other* sites of the same run after retries — the share of runs in which
  the site failed every attempt. A site above `SiteRetireShare` (50 %) over
  at least `SiteMinSamples` (200) such runs is retired with its reason, and
  the next candidate of the same class and category that the prober host
  itself loads cleanly, browser-shaped and with the same retries, is
  promoted in its place, so each class keeps `SitePoolSize` sites. A
  promoted site is on **probation**: it is loaded and recorded like the
  rest but neither the index nor the 90 % rule counts it until it has
  passed on at least `SiteProbationShare` (50 %) of `SiteMinSamples` healthy
  exits — a site that passes from the taskworker host can still fail
  through every tunnel, and a new site must earn the right to cost a
  provider a tier. A retired site returns to the candidate list after
  `SiteRetireCooldown` (30 days) and is dropped for good after
  `SiteMaxRetirements` (3), until the operator re-adds it. The task retires
  at most `SiteMaxRetirePerRun` (1) site per class per run, and skips the
  run entirely while the fleet-wide failure share is above the prober-fault
  line, so a fault on the prober cannot empty a class in a day and burn
  the candidates on sites that fail for the same reason. When too few exits
  qualify as healthy for a judgement — because the bad sites themselves
  drag every exit under nine in ten — the judgement falls back to the share
  over all runs and says so in the log. The task logs retired, promoted,
  on probation and candidates remaining per class, and emits the per-site
  failure share as a metric.
- **Where a site does not work.** Each destination — active or candidate,
  in the table and in `egress-sites.yml` — carries `incompatible`: the
  country codes and (country, region) pairs it is known to fail from. The
  refresh learns them: a site that fails at least `SiteRegionFailShare`
  (90 %) of healthy exits in one country or region over at least
  `SiteRegionMinSamples` (30) runs, while its fleet-wide share stays under
  the retire line, is not broken but blocked there, and that place is
  added to its list instead of retiring the site. A place where *every*
  site fails is never marked for any site: that is the prober's route to
  that place's exits or the `/ip` echo blocked there (the
  country-unreachable alert), and the task judges sites only in places
  where most sites pass. A marked place keeps a
  small canary: `SiteRegionCanaryShare` (5 %) of that place's runs still
  load the site, unscored, so a site that becomes reachable again is
  unmarked after `SiteRegionCooldown` (30 days) of canaries passing. When
  marking would leave a class's compatible pool for a place under its
  sample size, the task promotes candidates compatible with that place
  first, so every region keeps a representative sample.

- **The watch.** `server/monitor/SIGNALS.md` §2.19b alerts when an active
  site stays above the retire line for a day (the task is not doing its
  job), when the task is not running, when a class's pool is thin or its
  candidates are exhausted, when a site fails a whole country or region that
  the task has not yet marked, when a place's compatible pool for a class
  is under its sample size, when every site fails in one country at once
  (the country's exits cannot be reached at all — a route or capacity
  fault, or the `/ip` echo blocked there — not a site fault), when the
  blackhole retry queue starves (checks past their `next_due_at` by more
  than a backoff), and — read with §2.19a — when every site fails
  everywhere, which is the prober's fault and not the pool's. The monitor
  only watches; the healing is the task's, and it heals only as far as the
  candidate list reaches, which is why exhaustion is an alert and not a
  silent stop.

- **What it cannot heal, stated plainly.** A site behind bot management
  that fingerprints the TLS or HTTP/2 handshake refuses Go's client whatever
  headers it sends; such a site fails everyone, is retired, and the pool
  drifts toward sites that tolerate automated clients — so the pool
  measures reachability of representative sites, not how the biggest
  bot-managed sites treat an exit. A site blocked in a country is marked
  incompatible there and not loaded from it, so it cannot move a region
  past the one-in-ten line; what remains unhealable is a place where too
  few representative sites work at all, which the pool-thin alert names
  and the candidate list must answer. And quality is bounded by coverage: a provider nobody has
  probed within seven days is unprobed, whatever it would score.

### 11.5 Country-specific site sample

The 26 scored `site` loads in a full quality run are split evenly: 13
uniformly sampled from the general active site pool and 13 uniformly sampled
from the active list for the provider's **published** ISO 3166-1 alpha-2
country. DNS, connectivity, and CDN sample sizes do not change. The `/ip`
echo still independently checks the observed country; a mismatch does not
authorize silently switching to another country's list after the sample was
chosen. General and country lists have disjoint destination names and hostnames,
and a destination excluded for the provider's country or region is not eligible
in either half. Sampling uses one per-run random source, without replacement
within each half. A site in the country half is scored by the same load,
retry, canary, probation, and healthy-exit rules as a general site; list
membership does not make an inaccessible site a provider fault by itself.
The global and each country pool target 100 distinct, verified, active site
URLs, so both halves sample from comparable populations. The tunnel's
allowed-host set must include the selected global and country destinations
plus the `/ip` echo before the loads start; an omitted country host is a
prober configuration failure, not a provider failure.

`config/all/egress-sites.yml` carries a versioned country-list manifest,
separate from the ordinary refresh candidates. Each country record has its
country code, source identity and source period, curation and verification
timestamps, and a target of 100 distinct eligible site contracts. The source must support
the claimed *country-specific popularity*; a global top-site list, a regional
proxy, or a list inferred from a country's ccTLD is not evidence of that.
Curators exclude shared-pool hostnames, duplicate registrable domains,
malware/adult/piracy destinations, captive-portal or login-only pages, and
sites whose bounded GET contract fails independently from a normal host.
They retain source provenance and note any excluded ranking entries rather
than filling a short list with invented sites. An automated import may
propose candidates, but cannot declare them verified or score-bearing. The
ordinary refresh may retire or replace country sites only with verified
candidates for that *same* country and restore the 100-site target. A
country with fewer than 100 verified websites remains explicitly underfilled;
it is never padded with infrastructure domains or invented URLs.

The initial source survey (2026-09-24) found 249 country/territory codes in
the checked-in GeoLite2 export and 238 directories in the public country
CrUX cache, covering 237 of those codes. The missing codes are `aq`, `bv`,
`cc`, `gs`, `hm`, `nu`, `pn`, `tf`, `tk`, `um`, `va`, and `wf`. This is a research
gap, not permission to use a neighboring country's ranking. Google's CrUX
popularity field is a coarse rank *bucket*, not an exact ordinal order, and
CrUX excludes destinations with insufficient eligible Chrome traffic.
The [CrUX dataset](https://developer.chrome.com/docs/crux/bigquery),
[ranking definition](https://developer.chrome.com/docs/crux/methodology/metrics),
and [public country cache](https://github.com/InternetHealthReport/crux-top-lists-country)
are candidate evidence, not load verification. Cloudflare Radar publishes
ordered per-country top-100 lists through an authenticated
[API](https://developers.cloudflare.com/radar/investigate/domain-ranking-datasets/);
the dedicated local `vault/cf-radar.yml` token was confirmed to authenticate a
read-only country top-list request on 2026-09-24. Radar ranks **DNS domains**,
not verified browser-loadable websites: its top results include CDN,
telemetry, and update infrastructure. The curated list must therefore
cross-check a candidate against country CrUX website origins and perform an
independent bounded page-load verification. Even a high Radar rank is not a
safety or load-success guarantee. Never copy the token, an Authorization
header, or raw API responses into the repository or alert ledger.
Build each 100-site list from verified Radar top-100 websites first, then
verified origins in that country's current CrUX top bucket as needed to fill
the target. Preserve each entry's source and rank or rank bucket; a CrUX
bucket must never be presented as an exact ordinal rank. Where both sources
lack 100 safe, loadable, country-observed sites, report the shortage and
leave the country underfilled. A country absent from CrUX may still be
curated from Radar only if 100 websites pass the same checks.
The first dedicated-token Radar sweep returned a valid response for all 249
GeoLite2 codes: 227 lists had 100 entries, Montserrat (`ms`) had 106, and 21
had no ranked domains. Eleven of the empty Radar countries also lack a CrUX
country list (`aq`, `bv`, `cc`, `gs`, `hm`, `nu`, `pn`, `tf`, `tk`, `um`, `va`),
so they are explicitly source-unavailable until a defensible source exists.
`wf` has Radar data despite no CrUX directory. These are source-coverage
counts, not counts of verified website URLs or score-bearing pools.

The GeoLite2 export defines the coverage universe. The pool publisher joins
its country codes with the manifest and reports each country as `ready`,
`underfilled`, `stale`, or `unavailable` with a reason. `ready` requires 100
current, independently verified websites, at least 100 compatible active
sites in that country and a comparable 100-site global pool, a
recent source period, and a successful publisher refresh. If a country is
not ready, a full run does **not** backfill its 13 country slots with general
sites, report a 26-site score, or mark the provider dark because the list is
missing. It records a distinct country-coverage gap, leaves the provider's
previous quality evidence to age normally, and continues blackhole checks
and any independently useful unscored general loads. This is a monitoring and
curation failure, not a negative verdict about the provider. Existing
general-only pools retain their old 26-site behavior until this feature is
explicitly enabled and the country manifest is served. A 249-country
manifest with approximately 25,000 entries must use a bounded per-country
fetch or equivalently bounded authenticated delivery; never expand the
existing 4 MiB response cap by accident. A mixed old/new
rollout must not silently change score denominators.

`server/monitor/SIGNALS.md` §2.19d watches the manifest source age, verification
age, counts and overlap, active compatible capacity by country, publisher
generation and fetch errors, country-half sample counts, and the share of
full runs skipped for missing coverage. An empty or unreadable source is
`unobservable`, never a healthy zero. Tests use synthetic countries and
reserved example domains: exact 13/13 sampling, disjointness, exclusions,
source staleness, missing/partial lists, old-pool compatibility, and a
provider that passes general sites but cannot obtain country coverage.

### 11.6 Order and tests

Ship the check rules (spaced retries, browser-shaped requests, consecutive
provider-wide failures, guard, concurrency) first and alone: they are
what turns half the fleet back on. Then the site-only full run with `/ip`
geolocation and the pool as data with its refresh task and §2.19b, then the
column drops with §10's.

Tests, pure: a load that fails twice and passes once is a pass; a provider
that fails two checks and passes one is not dark; three failures under the
minimum span are not dark; a reconnect does not clear a provider-wide count
but a passing check does; the guard
discards a 30 % batch and keeps a 10 % one; the `/ip` answer maps to a
probed location with the right confidence. Database: the check rows carry
the provider-wide count; the current-dark set honours the threshold,
the span, the max age and a passing-row override; the health counts store
after-retry outcomes; the index and the 90 % rule read them. Integration,
against the local stack with a fake provider: the warm-up precedes the first
scored load.
