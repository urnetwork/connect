# Extender update 2: gated discovery, resilient bootstrap, and TCP splicing

Status, 2026-11-02: the code for sections 1, 2 and 3.2 to 3.4 is on `origin/main`
of connect, sdk and server, and nothing is turned on. Section 3.1 is paused and
awaits the owner, and everything live in section 3 waits on its list. No client
or miner release carries this work. "Implementation status" below gives the state
part by part. A quoted note at each section says what was built, what is left and
which decisions are the owner's.

Written 2026-11-01 as a proposal. Its text is unchanged apart from this paragraph
and the status above, so "proposed" below means the plan as written. This narrows
[EXTENDERUPDATE.md](EXTENDERUPDATE.md) to the first three censorship-resistance
priorities and incorporates the corrections in
[EXTENDERUPDATE-REVIEW.md](EXTENDERUPDATE-REVIEW.md).

| Priority | Update | Intended result |
| --- | --- | --- |
| 1 | Prefer gated discovery, with capacity limits and reliable refresh | Expose fewer durable extender addresses while preserving a usable recovery path |
| 2 | Add authenticated bootstrap outside the normal DNS path | Recover a verified extender connection when ordinary discovery fails |
| 3 | Supply measured borrowed-site lists and stage TCP splicing | Improve TCP resistance to active probing without stranding incompatible clients |

These three changes form one path: bootstrap establishes verified reach,
authenticated release supplies a bounded set of gated extenders, and compatible
extenders can serve the TCP camouflage carrier. The scope includes the
measurement and compatibility checks necessary to ship those changes. Enterprise
reach, WebRTC rendezvous, shared ingress, automatic address churn, QUIC
camouflage, new DNS carriers, and traffic shaping remain separate proposals.

## Implementation status (2026-11-02)

Read about 22:35Z. Each commit below was checked as an ancestor of `origin/main`,
at connect `1d39adab`, sdk `2ae5419a`, server `bcf9cfd60` and xops `ef85579`.
Defaults come from reading the code at those commits, versions and counters from
Mimir, and the origin's answer from one `GET /v1/seed.bin`. Counters read since
each process last started. No test was run for this note, so "tests on main" means
the tests exist and name that behavior. The vault wasn't read, so a default in
`extender.yml` is the code's default. The operations ledger and the deployed
counters agree with it.

### What landed

| Slice or part | Spec | connect | sdk | server | Control and default | Running now |
| --- | --- | --- | --- | --- | --- | --- |
| A: gated client | 1.1, 1.3 | `78cabce6` | `b05442ca` | | `NetworkSpaceValues.ExtenderGatedPreference`, off. No app or bundled space sets it | On no device: no release carries it |
| B: gated server | 1.1 to 1.3, 3.2 | `3ec6aeec` | `9743ff5d` | `73865a730` | No switch on the answers or the outage rule. `directory.capability_cohorts` in `extender.yml` is empty by default | Live on api and alt, with no caller: `urnetwork_extender_releases_total` reads 0 for every outcome |
| C1: seed envelope, bootstrap pass | 2.1 to 2.4 | `beb08bdb`, `5f048fc0` | | | A library that does nothing until a space has a source | Not running |
| C2: sdk seed lifecycle | 2.3, 2.4 | | `ba40352e` | | No mirrors and no embedded bundle by default | On no device |
| D: seed publisher, mirrors | 2.2, 2.3 | `31e5be19` | | `3a8d8ab10`, `dc384ab5c` | `seed.enabled` in `extender.yml`, off. The mirror play is xops `5252855` | Deployed, off: `urnetwork_taskworker_extender_seed_enabled` reads 0 and the origin answers 404. Mirror play written, not applied |
| F: splice rollout | 3.2 to 3.4 | `14ccdd6a`, `f10f75d8` | `e377fcea` | | `CamouflageEnabled` and `ExtenderCamouflageSplice`, off | Not running |
| 14b: camouflage key in records | 3.2 | `66625e14` | | `1e94b4d78` | Inert until a role posts a key | Migration 921 applied. Its release is built and not deployed (below) |
| Role wiring | 3.1 to 3.3 | `07ab63fc` | `b0997e46` | | `connectctl extender --camouflage` and `--camouflage_splice`, off. The sdk role has no exported switch | No host passes the flags |
| Reporting path | 3.3, gates | `bd52bd88` | `5333ae6f` | `b49349352`, `8473e4b45` | Off on both sides: `camouflage_report.enabled` in `extender.yml`, `ExtenderGatedPreference` on clients, `--camouflage_report` on hosts | Route deployed, off: `urnetwork_extender_camouflage_reports_total` reads 0 |
| Block report sender | 1.3 | `bf59a4da` | `eb72dc8c`, `9bc21470`, `4fbf1e3e` | `3d0c75fae` | `ExtenderGatedPreference`, off | On no device, so no client reports |
| Distinct-network threshold | 1.3 | | | `2204aacb9` | No switch | Live on api and alt, with nothing to count yet |
| Client log scrub | 1.3 | `ac200553` | `bdeb8be7` | | No switch | Reaches devices with the next client release |

connect and sdk are compiled into the server services, so "not running" means
nothing calls it there, not that the code is absent from the build.

### What runs where

| What | Version | Built from | This work in it |
| --- | --- | --- | --- |
| api (20), alt (2), taskworker (8) | `2026.11.2-outerwerld+1088438700` | server `3e4d2a32d`, connect `5ecdeb65`, sdk `826df3eb` | Everything above except 14b |
| connect (20), proxy (20) | `2026.11.2-outerwerld+1088460100` | server `1e94b4d78`, connect `b13c6d5a`, sdk `826df3eb` | Everything above. From reading the code, these services neither sign records nor probe extenders, so 14b does nothing there |
| gossip (4) | `2026.10.27+1083160450` | Predates this work | None, so not the publish guard of section 1.3 |
| Built, not deployed: api, alt, taskworker | `2026.11.2-outerwerld-1088515650` | server `bcf9cfd60`, connect `d7bcc04e`, sdk `2ae5419a` | Adds 14b, inert until a role turns camouflage on |
| Clients and miners | Latest published build release, `v2026.10.27-1083160450` | connect `9b0afcca`, sdk `d2458d31` | None. The `v2026.10.30` drafts and the build repo's current pins have none either |

The database is at head 921 according to the operations ledger. It wasn't read
again for this note.

### Paused

Slice E, the borrowed-site measurement of section 3.1, was stopped by a safety
check and awaits the owner. It delivered nothing, and nobody is working on it.
Don't reassign it without the owner's word. Everything live in section 3 waits on
its list.

### Not part of this proposal

The recycle trial was listed beside these decisions, but it isn't extender work.
It is the stuck provider recycle of server `CPUOPT3.md`: api and connect now have
the code, its flags are off, and trying it is the owner's decision.

### Follow-ups and test notes

`ur.io/react/scripts/publishable-spec.mjs` removes `x-internal` operations but leaves
their schemas, so ur.io's public `/openapi.yml` names `tcp-camouflage-v1`, the
camouflage counters and the splice status reasons. The same text is public in the
connect repo. Dropping the orphan schemas isn't started.

From the release notes, not run again here. The full `./extender` package failed
one different test per run on three merged-tree runs at load above 150
(`TestProbeExtenderLatencyCosignsAnActivePeer`,
`TestExtenderStatsAreSafeToReadWhileRelaying`,
`TestExtenderNetworkClientSamplesAgainWhenHelloInstallsKeys`), which is likely load
and unproven. `TestExtenderCamouflageTargetLoadMeasured` flakes too.
`TestExtenderCommandServesAndActivates` flakes under parallel load, more often on
the carrier branch than before it, unexplained.

## Existing behavior and the implementation gap

The baseline is the local connect checkout at
`69297f0558a7097c3b459639271255192905c040`, with the corresponding server and SDK
sources inspected for integration. [EXTENDER.md](EXTENDER.md) sections R, K7,
and P provide the existing directory, share, and camouflage contracts.

| Area | Already present | Work proposed here |
| --- | --- | --- |
| Gated release | Authenticated server route, deterministic partitions, disclosure ledger, country limits, and blocked-record filtering | Complete client release integration, prefer usable gated candidates, and define capacity/failure handling |
| Directory refresh | Gated records can be reissued within an epoch; open channels exclude gated records | Schedule authenticated refresh before record expiry and recover when the control path fails |
| DNS bootstrap | Multiple default DoH providers, regional bootstrap selection, and signed TXT records | Add a bootstrap source that can work without the usual resolver path |
| Sharing | `ur-ext:1:` carries unsigned address hints and optional explicitly imported settings | Introduce a separate authenticated seed bundle with independently verified open records |
| TCP camouflage | Authenticated hello, splice implementation, migration flags, and resource limits | Maintain measured targets and enable splicing only for compatible deployments |

The proposal preserves identity verification and disclosure accounting throughout
recovery. It aims to reduce harvesting and improve TCP probe behavior; it does
not make an admitted extender address secret from its client or an on-path
observer.

## 1. Prefer gated discovery without making an empty release an outage

### 1.1 Client selection and release integration

Add a gated-preferred policy for extender selection. Preference applies when an
extender is needed; it does not force healthy direct connections through one.
The release route continues to return gated records only. Open fallback is a
client decision using the separately maintained open directory.

The client follows these rules:

1. Use an eligible, verified cached gated record when one is available. A
   healthy connection does not wait for a directory refresh.
2. When authenticated control reach exists, fetch the gated release with the
   device's existing identity. Validate each record and apply it with
   `ExtenderSourceRelease`. Account/network changes cancel the old fetch and
   prevent its response from entering the new space.
3. Give the gated attempt a bounded connection budget. If the release is empty,
   limited, unavailable, or its candidates fail, allow verified open candidates
   under the accessibility fallback policy. A reachable release API alone does
   not suppress this fallback.
4. If the control route itself requires an extender, use a verified cached open
   record or the bootstrap in section 2 to obtain control reach, then request the
   gated release. Recovery must not depend exclusively on the failed route.
5. Record why open fallback was selected and retry gated refresh with backoff.
   Prefer gated for subsequent connections once it recovers; drain working
   connections naturally rather than interrupting them merely to change tier.

Coalesce release requests per identity and network space. Path changes, low
watermarks, and simultaneous dials must share the same request and retry budget.
Use additive structured refusal/empty-result reasons and retry timing in the
server response; retain the existing response fields for older clients. The
client bounds any supplied retry delay and enforces its own deadline and rate
limits. Avoid exposing fleet details through diagnostic responses.

The server route is in
[`extender_release_controller.go`](../server/controller/extender_release_controller.go).
EXTENDER.md R5 identifies the SDK fetch as integration work, and the inspected
SDK has no call to `/network/extender-release`. This is a client/server delivery,
not just a change to candidate ordering.

> **Status, 2026-11-02: landed, off (slices A and B).** Rules 1 to 5, the shared
> request and retry budget, and the additive `outcome` and `retry_after_seconds`
> are on main. The policy is `ExtenderDirectory.SetGatedPreference`
> (`net_extender_directory_gated.go`), with a 10 s bounded attempt and a fixed set
> of fallback reasons. The fetch is sdk `extender_release.go`, so the sdk now makes
> the call named above. `ExtenderGatedPreference` turns both on together and
> nothing sets it, so a client asks for nothing and spends no disclosure. The
> deployed api answers with outcomes, and no deployed client calls the route.
> Design note: EXTENDER.md, "gated-preferred selection and the release fetch".
>
> **Owner decision open:** whether gated records survive sign-out. Today a sign-out
> keeps what that identity was released until another identity binds, which forgets
> them (`ForgetGatedRecords`).

### 1.2 Capacity and disclosure accounting

Keep the existing [release-policy defaults](net_extender_directory_release.go)
as the initial security boundary:

| Constraint | Current default |
| --- | --- |
| Identity probation | 30 days |
| Records disclosed per identity per epoch | 1 while new; 3 when trusted |
| Disclosure epoch | 7 days |
| Distinct identities per extender per country | 10 over 30 days |
| Request limits | 8 per identity/hour; 4096 per vantage/hour |

For a fixed fleet of N extenders, the country cap provides at most 10N distinct
identity/record assignments in the window. Serving every client three records
uses three assignments. Partitioning, address-family availability, maintenance,
and blocking reduce usable capacity further. Size the fleet and rollout cohort
against those constraints before enabling gated preference broadly.

Retries, changes of country or address family, newly blocked records, and record
removal must not refund disclosures or create a fresh identity. Preserve the
existing transactional ledger across server replicas. A fully spent epoch may
legitimately return no usable record; the client then follows section 1.1 instead
of requesting new partitions until one succeeds.

The initial rollout uses the current R1 division: operator hosts are gated and
volunteer extenders are open. Public seeds in section 2 use a small selected open
cohort. This proposal does not introduce an operator-run public churn fleet or
quietly reclassify durable hosts as open to satisfy demand.

> **Status, 2026-11-02: limits unchanged, capacity not sized.** The defaults above
> still hold (read in `net_extender_directory_release.go`). The ledger tests cover
> concurrent replicas and no refunds (server
> `model/network_extender_release_ledger_test.go`). There is no gated fleet to size:
> the runbook's read of Main on 2026-11-02 found every active address in the open
> tier, so a release answers `empty` for every identity (server
> `docs/operations/extender-gated-release.md`; not read again for this note).
>
> **Owner decision open:** moving hosts into the gated tier. None is there today.

### 1.3 Refresh and blocked-state handling

Refresh gated records through authenticated release. Never send them through DNS
publication, the public seed bundle, feed samples, gossip, or the C4 drip.
Retain this restriction even if a gated record was received from an unexpected
source. Public revocations remain possible because they need not disclose an
address.

Distinguish the disclosure epoch from record lifetime. The inspected server
issues records with a 24-hour lifetime; waiting seven days to refresh would be
incorrect. Schedule refresh from the earliest relevant expiry, leaving room for
bounded retries. A six-hour normal refresh interval is an initial candidate for
the present lifetime; jitter must remain within the expiry margin. Early
refresh triggers are coalesced and subject to the same request limits.

Routine endpoint changes use newly signed records on this authenticated path.
Keep an overlap/drain window where operationally possible and test recovery when
the old endpoint disappears before refresh. Account for IPv4-only and IPv6-only
clients. Automatic IP rotation is outside this proposal.

The release policy already skips records blocked in the requester's country.
Retain the existing distinct-reporter threshold and recent operator-success
check, and verify that deployed reporting/probing supplies those inputs. Treat
the result as reachability evidence; distinguish an outage or carrier failure
from a country-wide block. Canary leak attribution alone must not blacklist
unrelated records. Keep existing deterministic ordering among eligible records;
an undefined “lowest block age” score is not part of this change.

> **Status, 2026-11-02.**
>
> - **Refresh: landed, off.** Six hours after an answer that released a record, at
>   most 30 min earlier by jitter, and never later than 1 h before the earliest
>   expiry held. At most 4 requests an hour, none within 60 s of an answer.
> - **Publication: landed.** Every writer leaves gated and channel-restricted records
>   out, and the feed sample, the subscribers, the seed publisher and the K7 share
>   refuse them. gossip's own last guard (server `gossip/publish.go`) is on main, but
>   the deployed gossip predates it, so it should be deployed before any host moves into
>   the gated tier.
> - **Blocked state: rule live, sender off.** The outage rule (a failed probe sets
>   aside the reports made before it) and distinct networks in place of distinct client
>   ids are live on api, alt and the taskworker. The block report sender is on sdk main
>   under `ExtenderGatedPreference`, off. No client sends a report, so the blocked
>   state blocks nothing yet. No block-age score was added.
> - **Logs: merged.** The network client's default-level lines and
>   `ExtenderStatus.LastError` name no address (`extenderErrorText`). This reaches
>   devices with the next client release.
>
> Design notes: EXTENDER.md, "release outcomes, capability eligibility and the outage
> rule", "the block report sender" and "block reporters and what a client's errors
> say". Runbook: server `docs/operations/extender-gated-release.md`.
>
> **Owner decision open:** the K7 share change. A share now leaves out every address a
> gated or channel-restricted record names, where it carried them before, in use first.
> The list at the top of this section doesn't name K7, so this goes past its text.

### 1.4 Acceptance

Extend the existing tier tests and add client integration tests with fake time,
fake release responses, and observed dial attempts. Pin these cases:

- A usable gated record is preferred; empty, refused, timed-out, or failed
  releases lead to the defined verified-open fallback within a bounded time.
- A cold client can establish control reach before its first gated release;
  simultaneous requests do not multiply release calls.
- Country capacity exhaustion, probation, epoch rollover, and concurrent server
  admissions preserve every disclosure limit. Changing request properties or
  removing a record does not reset the budget.
- Refresh happens before record expiry; account changes discard stale responses;
  revoked or disallowed records never become new connection candidates.
- Gated and channel-restricted records never enter any public publication path,
  including the new seed publisher. Test routine endpoint replacement in both
  address families and recovery after loss of the old control path.

Reuse [net_extender_directory_tiers_test.go](net_extender_directory_tiers_test.go)
for policy invariants. SDK scheduling and server ledger concurrency require
their own integration coverage.

> **Status, 2026-11-02: tests on main for each case, read and not run.**
>
> - Preference and fallback: `TestExtenderGatedPreference*` (connect
>   `net_extender_directory_tiers_test.go`), `TestClientStrategyGatedPreference*`.
> - Cold start and coalescing: sdk `TestExtenderReleaseColdClientReachesControlFirst`,
>   `TestExtenderReleaseCoalescesTriggersIntoOneRequest`.
> - Disclosure limits: server `TestExtenderReleaseProbationCapacityAndEpochOutcomes`,
>   `TestExtenderReleaseConcurrentReplicasHoldTheEpochCount`,
>   `TestExtenderReleaseConcurrentReplicasHoldTheCountryCap`,
>   `TestExtenderReleaseRequestChangesNeverRefund`,
>   `TestExtenderReleaseRemovalAndBlockingNeverRefund`.
> - Refresh, account and revocation: sdk `TestExtenderReleaseRefreshesBeforeTheEarliestExpiry`,
>   `TestExtenderReleaseAccountChangeDiscardsTheStaleAnswer`,
>   `TestExtenderReleaseNeverDrawsARevokedRecord`.
> - Publication and endpoints: `TestExtenderGatedRecordsNeverLeaveOnAPublicPath`,
>   `TestExtenderGatedReleaseReplacesAnEndpointInEitherFamily`, server
>   `gossip/publish_tiers_test.go` and `taskworker/work/extender_publication_tiers_test.go`,
>   sdk `TestExtenderSeedRecoveryReachesTheGatedRelease`.
>
> These pin the specified behavior. Capacity and reach by country and family need a
> cohort, which doesn't exist yet.

## 2. Bootstrap through a small authenticated open-record bundle

### 2.1 Preserve the verification boundary

[`ur-ext:1:` is unsigned](net_extender_share.go). Its imported addresses remain
unverified, and optional settings can replace trust anchors only through an
explicit import decision. Keep that format and behavior separate from automated
seed fetching.

There is also a cold-start dependency to resolve: the
[directory's dialability check](net_extender_directory.go#L2232) excludes
unverified DNS/import entries. A seed containing only addresses cannot restore
reach when the feed needed to verify them is itself unreachable.

The new seed bundle therefore carries a small set of ordinary open-tier
`ExtenderRecord` objects, each with its existing operator root signature. The
bundle authenticates its distribution metadata; every contained record still
passes the existing root, network, expiry, revocation, and tier checks. An
envelope signature alone never makes a bare address dialable. The extender leaf
check and inner destination authentication continue to apply on connection.

> **Status, 2026-11-02: landed (slice C1), not in use.** A seed adds no address: its
> records enter only through `ApplyRecord`, each verified on its own, so the
> dialability rule is unchanged. `ur-ext:1:` stays unsigned, with `ur-seed:1:` beside
> it. Design note: EXTENDER.md, "authenticated seed bundle".

### 2.2 Proposed envelope and trust rules

Define a separate versioned seed-envelope message; do not reinterpret a K7
payload or add a signature whose semantics older readers would ignore. Its
signed body contains:

- Format version, network host, and a monotonically increasing sequence for that
  network's public seed feed.
- Issued-at and expiry times.
- The bounded set of serialized open extender records and any accompanying
  root-signed revocations.

The envelope carries the serialized body, signature, and signing-key identifier.
Reuse the existing Ed25519 signing helper with a new domain, proposed as
`ur-extender-seed-v1`, signing the exact body bytes. Only roots already authorized
by the network-space configuration or its authenticated update path may verify
it. The key identifier is a lookup hint, never a trust anchor. The bundle has no
fields that replace root keys, network hosts, gossip URLs, or resolver settings.

Persist the greatest accepted sequence and its body digest per network, using
the existing network-host normalization. Reject lower sequences and a different
body at the same sequence; accepting an identical bundle is idempotent. Validate
the complete bundle before applying it, and commit the accepted artifact and
sequence together so a restart cannot lose the usable bundle while retaining
only its higher sequence. Sequences continue across signing-key rotation.
Apply an authenticated root update before accepting bundles signed only by a new
key, and reject removed keys after that update. Root rollover/revocation uses
the established authenticated configuration path or an authenticated application
update, never authority supplied by a seed mirror. Invalid updates leave the
last valid state intact.

Initial proposed parser limits are 64 KiB per complete envelope, eight records,
32 total record addresses, and 32 revocations, with each record also bounded
within that byte cap. Check size before allocating decoded bodies, and
apply existing record validation limits as well. Publish a stable small cohort
between planned updates; fetching another mirror must not reveal another random
slice of the fleet.

The publisher and importer both require `ExtenderRecordOpen`, including an empty
`CanaryChannel`. Reject bundles containing gated or channel-restricted records.
Publish only addresses already approved for public disclosure. Mirrors serve
signed artifacts and never receive root private keys.

> **Status, 2026-11-02: landed (slice C1), not in use.** The envelope, the domain
> `ur-extender-seed-v1`, the sequence store and the limits above are on main
> (`net_extender_seed.go`, `net_extender_seed_store.go`). Limits added past the text:
> 8 KiB per record, 1 KiB per revocation, a 30-day maximum validity and a 5-minute
> clock allowance. A stored floor binds only while its envelope verifies under the
> keys in force, so a hostile K7 root can't pin one. The publisher (slice D) enforces
> the open-only rule too.

### 2.3 Distribution, expiry, and recovery

Provide two delivery paths for the same artifact:

1. A small configured set of HTTPS mirrors that can be contacted independently
   of an extender connection. At least one must have a DNS-independent dial
   route, such as a configured address with normal certificate verification for
   the intended server name. Bound redirects to configured destinations.
2. Import of the authenticated seed bundle from a file or pasted text. Application
   releases may carry a bundle as another source, subject to the same freshness
   and verification rules.

Use a shared bounded fetch budget, cancellation, backoff, and a deadline for the
whole bootstrap pass. One stalled mirror must not prevent the others from
running. The recovery route must be exercised with the ordinary DNS bootstrap
and control route deliberately unavailable.

Bundle validity must not outlive the usable records it promises. Refresh the
published bundle before that validity ends and monitor every mirror's sequence
and expiry. Old embedded bundles eventually become unusable; they are not a
permanent offline escape hatch. Reject future-dated or expired material using a
documented bounded clock-skew allowance. A bad local clock produces an explicit
clock/freshness failure rather than disabling verification. On a fresh install
with no sequence history, replay resistance is bounded by the validity window
and any minimum sequence supplied by trusted application configuration.

Apply root-signed revocations before selecting record candidates. If every
candidate is expired, revoked, or unreachable, retain the trust boundary and
report bootstrap failure; do not dial an unsigned hint as a substitute. Existing
record-expiry recovery behavior must not silently make an expired seed eligible
for a new cold start.

After an open seed establishes reach, fetch authenticated control state and
attempt the gated release. The seed mechanism supplements the existing multiple
DoH providers and regional selection; it does not change the user's resolver
configuration. A DHT or fronting partnership is unnecessary for this delivery.

> **Status, 2026-11-02: code landed, nothing enabled (slices C1, C2 and D).**
>
> - **Publisher:** the taskworker's seed chain, every 10 min. Deployed, off:
>   `urnetwork_taskworker_extender_seed_enabled` reads 0.
> - **Origin:** `GET /v1/seed.bin` on the api. Deployed. It answered 404 at 22:32Z,
>   counted as `disabled`.
> - **Mirrors:** xops `main/ansible/playbook-extender-seed-mirrors.yml` and
>   `EXTENDER-SEED-MIRRORS.md`, written and not applied. No mirror exists.
> - **Client:** the space value `ExtenderSeedMirrors`, with an empty bundled table;
>   `res/extender_seed.txt`, which carries no bundle; and `ImportExtenderSeed`,
>   `ImportExtenderSeedData` and `ImportExtenderSeedFile` for a file or pasted text. On
>   no device yet.
> - **Monitoring:** the seed chain checks every configured mirror's sequence and
>   expiry, and the monitor reports `extender-seed-off` while the publisher is off.
> - **Known limit:** with DNS black-holed and fewer than four usable seed records, the
>   network client's feed sample can wait out `HelloTimeout`, 30 s. Control reach and
>   the release request land first (EXTENDER.md, "the sdk seed lifecycle").
>
> **Owner decisions open** (reasons in the xops runbook, "Owner decisions"):
>
> - Turning the publisher on.
> - Record lifetime and bundle validity. 24 h is recommended, which keeps the seed's
>   exposure exactly a record's.
> - Whether releases embed a bundle, which only a longer lifetime makes usable after the
>   build.
> - Mirror certificates: keep connect's pinned Let's Encrypt roots, the conservative
>   default, or verify under the system roots, which is a client change.
> - Mirror hosts, addresses and names.
> - Whether the api origin goes in the client defaults. It shares the control route's
>   failure domain.

### 2.4 Acceptance

Use fixed signing keys, fake clocks, local mirror fixtures, and recorded dial
attempts to cover:

- Recovery with empty local state, DNS bootstrap blocked, control initially
  unreachable, and a valid bundle available through an independent path.
- A valid envelope containing an invalid record; a forged envelope; a foreign
  network; a gated/canary record; and an attempted settings/root replacement.
  None may cause an unverified dial or trust change.
- Expiry, future dates, identical replay, sequence rollback, conflicting bodies
  at one sequence, interrupted persistence/restart, and root rollover/removal.
- A signed revocation dominating a bundled record and an expired embedded bundle
  failing without bypassing the freshness check.
- Oversized inputs, excessive counts, malformed records, stalled mirrors,
  redirects, and cancellation staying within memory and connection budgets.

Retain [net_extender_share_test.go](net_extender_share_test.go) as compatibility
coverage for unsigned K7 imports. Add separate seed-envelope, publication, and
bootstrap integration tests; a successful K7 round trip is not a signing test.

> **Status, 2026-11-02: tests on main for each case, read and not run.** connect
> `net_extender_seed*_test.go` (forged, foreign, gated, settings replacement, limits,
> rollback, conflict, interrupted commit, root rollover and removal, revocation first,
> stalled mirrors, redirects, cancellation, hostile K7 root). sdk `extender_seed*_test.go`
> (`TestExtenderSeedRecoveryWithDnsAndControlBlocked`,
> `TestExtenderSeedRecoveryReachesTheGatedRelease`). server
> `taskworker/work/extender_seed_*_test.go`, `controller/extender_seed_test.go`,
> `api/extender_seed_route_test.go`. Recovery ran against local fixtures. It hasn't run
> against a real blocked network, because no mirror exists.

## 3. Enable TCP splicing through measured targets and client migration

### 3.1 Maintain a measured borrowed-site resource

Populate `res/extender_borrow[_<cc>].bin` from a repeatable measurement pipeline.
Keep a manifest beside each resource with its version, measurement time,
country/vantage coverage, tested names, relevant TLS results, and removal
reasons. Deliver updates through the authenticated software/resource release
process. XOR/gzip encoding is packaging, not authenticity.

Use the existing [borrowed-domain verification](net_extender_borrow.go) as a
starting point. Test reachability from the intended extender regions, TLS 1.3,
the supported X25519 key-share variants, and certificate validity for the exact
borrowed SNI. Assess whether the name's hosting pattern makes its use from an
extender address plausible; that assessment reduces a known mismatch risk but
cannot prove invisibility. Measure from deployed extenders as well as central
probes, and revalidate targets at role start and periodically thereafter.

Client/server resource versions must retain compatible target selection during
rollout: every name a supported client may select for an assigned extender must
be supported there. A partial intersection without selection rules is
insufficient. Stage additions before retiring old names and test country-list
differences explicitly. A nonempty resource is insufficient: an extender needs
reachable compatible targets, and a prober must see behavior appropriate to
the original SNI. Substitution of another hostname is a degraded outcome, not
evidence that the original site's TLS handshake was reproduced.

Assign an operational owner to refresh measurements and remove stale targets.
Record the maximum allowed measurement age and revalidation interval in the
rollout configuration before enabling Phase B. Target failure and exhaustion
must be visible in fleet telemetry.

> **Status, 2026-11-02: paused.** Slice E, which owned this section, was stopped by a
> safety check and awaits the owner. It delivered nothing, and nobody is working on it.
> `res/extender_borrow.bin` still decodes to an empty list, so no name is selectable, no
> extender has a verified target and no client claims `tcp-camouflage-v1`. Everything live
> in section 3 waits on this list.
>
> **Owner decisions open:**
>
> - Ownership of this section, the operational owner named above.
> - The maximum measurement age.
> - The revalidation interval. `DefaultExtenderCamouflageRevalidationInterval` is a 6 h
>   placeholder for the owner's number, with a 10 min floor.

### 3.2 Keep the two-phase compatibility rollout

Use the existing `CamouflageEnabled` and `ExtenderCamouflageSplice` controls:

| Phase | Server behavior | Gate to proceed |
| --- | --- | --- |
| A: authenticated-client rollout | Recognize authenticated hellos; keep splicing off so legacy clients still work | Working measured targets, compatible client coverage, verified clock handling, and acceptable connection success |
| B: scoped splice pilot | Enable splicing on a compatible cohort; unauthenticated clients/probes reach the borrowed target | Supported clients no longer depend on legacy termination; list overlap and failure tests pass |
| B: broader deployment | Expand only across cohorts that meet the same gates | Pilot success, bounded resource use, maintained target coverage, and a tested rollback |

Passing one release interval is not sufficient evidence that legacy clients have
retired. Legacy clients cannot be distinguished from probers by their untagged
hello. Keep any still-supported legacy cohort on Phase A, or retain an explicit
compatible fleet for it. Where releases select between those fleets, express
client capabilities in authenticated release eligibility without changing the
identity's disclosure budget. Never leave a supported client with only an
incompatible issued record.

Test supported clients with missing camouflage keys, list-version mismatch,
fingerprint overrides, and clock skew. The existing camouflaged-then-legacy race
is not a recovery guarantee in Phase B: the legacy attempt is spliced away from
the extender and fails its identity check. TCP-only networks make this failure
especially important. Clock correction must come from an authenticated source;
an unauthenticated failure must not set the clock or disable identity checks.

> **Status, 2026-11-02: landed, off. Not even Phase A runs.** No production role sets
> `CamouflageEnabled`.
>
> - **Capability:** `tcp-camouflage-v1` in the release request, and
>   `ExtenderRecordBody.RequiredCapabilities`, field 18. `ExtenderClientCapabilities`
>   returns nothing while the list is a placeholder. A cohort (`capability_cohorts`) is
>   configured for no key, per the server runbook, and
>   `urnetwork_extender_record_capability_requirements_total` reads 0.
> - **Safe flip:** `ExtenderCamouflageSpliceSettle` opens the gate after the extender's own
>   records have carried the key and the requirement for one record lifetime and a 1 h
>   margin, about 25 h.
> - **14b:** the server signs an activation's key only after the activation passes, and
>   only while the posting network holds the identity key. The operator's probes use that
>   key. Merged (server `1e94b4d78`), migration 921 applied, and built into a release that
>   isn't deployed. api, alt and the taskworker run without it.
> - **Roles:** `connectctl extender --camouflage`, `--camouflage_splice`,
>   `--camouflage_revalidate_interval` and an `--nlayer-hop` `camouflage_key`, each off
>   unless given. The sdk role takes the same settings, which the provider leaves unset.
>   A host that passes `--camouflage` today verifies nothing, posts no key and reports
>   `no_verified_target`.
> - **Clock:** the camouflage clock moves only on an authenticated `Date`: a response from
>   an extender that passed its leaf check, or a verified response of the platform api
>   (`net_extender_camouflage_clock.go`).
>
> Runbooks: `EXTENDER-SPLICE-ROLLOUT.md`, server `docs/operations/extender-capability-cohort.md`.
>
> **Owner decisions open:**
>
> - The sdk has no exported camouflage switch. It needs one `DeviceLocalSettings` field and
>   its bindings before an embedder can turn the role on.
> - Whether about 25 h in Phase A after each restart of a cohort host is acceptable. The
>   gate keeps no state across a restart, which errs toward compatibility.
> - The challenge-binding wire proposal. 14b makes no proof-of-possession claim: in Phase A
>   a wrong key still passes over the legacy attempt.

### 3.3 Preserve bounds and state the degraded behavior

Preserve the existing per-subnet admission, hello-size/time, replay-cache,
per-connection byte/idle, total-splice, and per-target limits. Measure aggregate
load on borrowed sites when increasing the deployment cohort; per-extender
limits alone do not bound a whole fleet's traffic to one target.

The current implementation falls back to legacy termination when no splice
target succeeds. Keep that behavior explicit in status and telemetry: it
preserves compatibility but exposes the old probe response. A deployment in
that state must not report successful camouflage coverage. Roll back the
affected Phase B cohort to Phase A when target availability or client success
fails its agreed threshold. Rollback changes posture for new connections and
preserves working sessions where possible.

This delivery protects the tested TCP path. Inventory co-hosted carriers during
the pilot: a recognizable UDP/DNS response on the same IP can still expose the
extender. Endpoint-wide probe resistance is not an acceptance claim for this
TCP-only change.

> **Status, 2026-11-02: landed, off.** The limits above are kept. A fallback terminate is
> counted apart (`SpliceFallbackTerminatedCount`), and `CamouflageStatus` says `Covered`
> only in the splice pilot with verified targets, the gate open and no fallback running.
> Load per borrowed name is counted (`TargetLoads`). The run-time lever is
> `SetCamouflageSplice`, which applies to new connections and leaves working splices
> running.
>
> Not done, since no cohort exists: the co-hosted carrier inventory, and fleet load on
> borrowed sites, which can be read per site only once the api build carries the list.
>
> **Owner decision open:** rollback on operator hosts. `connectctl` has no run-time lever,
> so a rollback is a restart without `--camouflage_splice`, which drops that host's working
> sessions and falls short of this section. Recommended: add a live rollback before Phase
> B.

### 3.4 Acceptance

Retain the existing Phase A/B tests in
[extender_camouflage_test.go](extender/extender_camouflage_test.go), including
legacy-client and skew behavior. Add deterministic regression coverage for the
new resource and rollout decisions:

- Compatible clients succeed in both phases; supported legacy clients retain a
  Phase A path; a client is never assigned only incompatible gated candidates.
- Missing, stale, unreachable, country-specific, or mismatched target lists
  produce the specified result and telemetry without leaking resources.
- Wrong-key, malformed, replayed, and skewed hellos take their expected paths;
  spliced handshakes are checked against the requested SNI and real target
  behavior, including target failure.
- Resource refresh and rollback preserve list overlap, active-session behavior,
  cancellation, admission limits, and per-target/fleet operational budgets.

For the TCP conformance claim, capture an independent, versioned real-browser
hello and exercise the actual authenticated carrier and probe paths against it.
The current [synthetic golden](fingerprint/README.md#L94) is useful regression
coverage but does not establish real-browser equivalence. This validation is
limited to the TCP work here; expanding QUIC conformance is separate work. Live
target measurement and captures produce maintained fixtures, while routine
regression tests remain deterministic and do not depend on public sites.

> **Status, 2026-11-02: tests on main, read and not run.** The Phase A/B tests are retained
> in `extender/extender_camouflage_test.go`, with compatibility, rollout and settle tests
> beside them. A real Chrome 156 ClientHello is committed with its version metadata
> (`fingerprint/testdata/fingerprints/chrome-156-clienthello.bin`) and fed through the actual
> peek, authentication and splice paths (`extender/extender_camouflage_realhello_test.go`).
> The list cases run on test lists, since no measured list exists.
>
> **Owner decision open:** the uTLS parrot refresh. The carrier hello is still
> `HelloChrome_133`, which trails Chrome 156 in `signature_algorithms` and the extension set
> (`TestSyntheticParrotIsNotRealBrowserEquivalent`).

## Delivery and rollout gates

| Delivery | Connect / SDK / server work | Gate before enabling |
| --- | --- | --- |
| Gated preference | Connect candidate policy; SDK authenticated fetch and refresh; server additive outcomes and eligibility; shared ledger coverage | Fleet capacity by country/family, bounded fallback, account isolation, and zero gated publication on public channels |
| Alternative bootstrap | Connect envelope validation; SDK cache/import/fetch lifecycle; server seed publisher; operations-managed mirrors | Cold-start recovery without ordinary DNS/control reach, verified records before dialing, expiry/rollback limits, and independently reachable distribution |
| TCP splice rollout | Connect measured resources and carrier tests; SDK client compatibility; extender Phase A/B controls; operations target monitoring | Compatible pilot clients, maintained target coverage, independent TCP captures, bounded target load, and a demonstrated rollback |

Develop the first two deliveries together: gated preference cannot become a
reliable default until its recovery path works. Ship observability and the new
bootstrap path first, then enable gated preference by cohort. Borrowed-name
measurement and compatible client rollout can proceed in parallel; Phase B
follows its own readiness gates.

Before each cohort expands, record its baseline, observation period, and numeric
acceptance/rollback thresholds. Track connection success and time to first
usable connection, gated/open selection and fallback reasons, release denials,
refresh lateness, seed freshness and failures, authenticated/spliced/terminated
TCP outcomes, legacy/skew failures, and target load. Aggregate by country,
address family, and client/resource version without exporting gated addresses
or secrets into public telemetry.

Every implementation bug fix must include a deterministic regression test that
exercises its root cause. Passing those tests establishes the specified behavior;
country reachability and resistance to harvesting/probing still require measured
deployment evidence. Completion means all three deliveries pass their gates and
their operational owners can sustain refresh, measurement, and rollback.

> **Status, 2026-11-02: no gate has passed, because no cohort exists. This isn't complete.**
>
> | Delivery | Where it stands |
> | --- | --- |
> | Gated preference | Code and tests on main, off. Fleet capacity can't be sized, since no host is in the gated tier |
> | Alternative bootstrap | Code and tests on main, off. No publisher is running and no mirror exists, so nothing is independently reachable |
> | TCP splice rollout | Waits on section 3.1. No client can claim the capability without the list. The independent TCP capture is in. Rollback is tested, not demonstrated on a host |
>
> The observability is built and off: the camouflage report and its dashboard
> (`EXTENDER-SPLICE-ROLLOUT.md`, "Reporting path"), the extenders dashboard's gated release
> panels and the seed dashboard. No baseline is recorded.
>
> **Owner decisions open:**
>
> - The numeric gates: baselines, observation periods, and acceptance and rollback
>   thresholds for each cohort.
> - Whether the reporting path gets its own control. Clients share
>   `ExtenderGatedPreference` today. Recommended: add one.
> - `relayed` as a country group. A report that crossed an extender reads `relayed`, so a
>   client that reaches the api only through extenders isn't counted in its country.
>   Recommended: accept it.
> - Trust in self-reported counts. One identity can move a small cohort's rate. Recommended:
>   treat them as signals, not gates.
