# Extender TCP splice rollout runbook (EXTENDER-UPDATE2 §3)

Append-only operational notes for staging the TCP camouflage splice. This is
the pilot runbook EXTENDER-UPDATE2 §3.2–§3.4 and the delivery-gates table ask
for. It names the controls, the readiness gates to record before each cohort
expands, the telemetry to watch, and the rollback. It does not restate section
P (connect `EXTENDER.md`), which it builds on.

Status 2026-11-02: camouflage is off everywhere. `CamouflageEnabled` defaults
off and no production role sets it, so not even Phase A is running. No
borrowed-site list is bundled yet (`res/extender_borrow.bin` is an empty
placeholder), so no extender has a verified splice target and no client claims
the capability. What this delivery ships is inert until these exist:

1. The measured list and its manifest (slice E, paused). The list's loader
   populates `ExtenderBorrowList`; today every list is unmeasured.
2. The server signing `RealityPublicKey` into records (14b).
3. A role that turns camouflage on (14c in EXTENDER.md for the sdk provider
   role; `connectctl extender` for operator hosts, which are the gated cohort).
   It derives the key and sets the activator's `RealityPublicKey`, verifies
   borrowed names at start and every revalidation interval, and feeds
   `CamouflageBorrowNames`.
4. That role wiring the safe-flip gate below: `CamouflageSpliceGate` set to
   `ExtenderCamouflageSpliceSettle.Open`, fed by `ObserveActivation` from
   `OnActivated`. The gate exists and is tested, and no production role uses it
   yet.

Status update 2026-11-02: items 3 and 4 are on main, off unless a host opts in
(Role options under Controls). Items 1 and 2 remain. Until the list exists, a
host that passes `--camouflage` verifies nothing, posts no key and reports
`no_verified_target`, so Phase A has no effect on any client yet.

## Controls

Per-extender (connect `extender.ExtenderSettings`, set by the provider role):

| Control | Meaning |
| --- | --- |
| `CamouflageEnabled` | recognize authenticated hellos, publish `RealityPublicKey`, offer the authenticated http/1.1 config. Off = no camouflage at all. |
| `ExtenderCamouflageSplice` | the initial splice posture (Phase A off / Phase B on). |
| `ExtenderServer.SetCamouflageSplice(bool)` | flip the posture at run time; takes effect for new connections, leaves working splices running. The rollback lever. |
| `CamouflageSpliceGate func() bool` | extra admission gate; splice runs only while it returns true. The role wires it to the safe-flip rule (below). |
| `CamouflageBorrowNames` | the role-start-verified borrowed names this extender splices to. Empty ⇒ no coverage. |
| `ExtenderCamouflageSpliceMaxCount` (256), `...MaxPerTarget` (32) | per-extender concurrent-splice caps. |
| `ExtenderCamouflageTimeWindow` (2m), `...ReplayTagCount` (65536), `...HelloMaxByteCount` (16 KiB) | the hello time window, the replay cache size, the peek byte bound. Preserve these. |

Role options (added 2026-11-02). `connectctl extender`, for operator hosts, takes
these as flags, and each is off unless given. The role behind them is
`extender.ExtenderCamouflageRole`.

| Option | Meaning |
| --- | --- |
| `--camouflage` | Sets `CamouflageEnabled`, so Phase A. Verifies the borrowed names and publishes the key while one verifies. Needs a kept identity. |
| `--camouflage_splice` | Sets `ExtenderCamouflageSplice`, so Phase B once this host's records settle. Needs `--camouflage`. |
| `--camouflage_revalidate_interval=<duration>` | How often the names are verified again. Needs `--camouflage`. Default `extender.DefaultExtenderCamouflageRevalidationInterval`, 6 h, at least 10 m. **The value is the owner's decision** and goes in the rollout config (gate 6 below). A list whose manifest asks for less gets less. |
| `--nlayer-hop=...&key=<hex>&camouflage_key=<hex>` | The camouflage key of a tcp hop, beside the identity key its leaf check needs. A hop that splices can be reached only with it. |

- **The key.** It derives from the identity seed, so there is no second file: it
  is as stable and as well kept as `--extender_key_file` or the `--state_dir`
  key file, and it rotates with the identity. A host that opts in with an ephemeral
  identity, or a key file that does not read, exits at start with a message saying
  so, before it binds anything.
- **What the activator posts.** The key is published only while at least one name
  verifies (P5). A host with none serves the legacy path alone and reports
  `no_verified_target`, which is not an error. A name that verifies after a post
  went out with no key is posted at once. Losing every name changes the record at
  the next activation, not before, so one bad pass that a retry repairs changes
  nothing. With server 14b, a post with no key clears the stored key, and the record
  then carries neither the key nor the requirement.
- **Verification.** A name is dialed over the path the splice uses and must pass on
  every family the host can dial, so a dual-stack host drops a name one family
  cannot reach. A failed name is dropped at once, counted in `BorrowVerifyFailCount`,
  and retried alone from 5 m, doubling within the interval. Each pass is a tls
  handshake to a third party per name and family, which the fleet multiplies
  (gate 7).
- **The gate.** Every role sets `CamouflageSpliceGate`, whatever the posture, so
  `SetCamouflageSplice(true)` cannot pass it either.
- **Rollback on an operator host.** connectctl has no run-time lever:
  `SetCamouflageSplice` is not reachable from outside the process. Remove
  `--camouflage_splice` and restart, which also drops that host's working sessions,
  and only then take the key out of the cohort. Not the other way round: a record
  signed without the requirement can reach a legacy client while the host still
  splices, until its next activation closes the gate.
- **The sdk provider role** takes the same settings in
  `deviceLocalExtenderSettings` (`CamouflageEnabled`, `CamouflageSplice`,
  `CamouflageRevalidationInterval`). The provider fills none of them and the sdk
  exports no switch, so an embedder cannot turn it on yet. That takes one exported
  `DeviceLocalSettings` field and its generated bindings, when the owner wants it.

Per-cohort (server, slice B, vault `extender.yml` `directory.capability_cohorts`):
the set of extender public keys eligible for the `tcp-camouflage-v1` capability.
A client that sends the capability is issued those extenders' records; a client
that does not is never issued a record that `RequiredCapabilities` gates
(`ExtenderRecordBody.RequiredCapabilities = 18`). Capabilities change
eligibility only — never the disclosure budget, partition or identity.

Client capability (connect, no per-client flag): the client claims
`tcp-camouflage-v1` (`connect.ExtenderClientCapabilities`) only when its
fingerprint switch is the Chrome hello (not `"go"`), it has a borrow list for
its country with a selectable name, and that list is measured and within its
maximum measurement age. `connect.ExtenderTcpCamouflageReadiness` reports the
reason when it does not.

## The safe Phase-A → Phase-B flip

Legacy clients cannot be told from probers by their untagged hello, so flipping
splice on strands any legacy client still holding a record that does not require
the capability — its legacy hello is spliced away and fails its identity check.
The flip is therefore gated, not a bare toggle:

1. The cohort's extenders carry `RealityPublicKey` in their signed record (14b)
   and the issuer (slice B) adds `tcp-camouflage-v1` to `RequiredCapabilities`
   only for records that carry the key.
2. Wait **one full record lifetime** (24 h at the current issuance) after the
   requirement first appears, so every legacy client's cached record — issued
   before the requirement — has expired and been refreshed. Only a client that
   re-fetched with the capability holds a current record, and such a client
   sends authenticated hellos.
3. Only then set `CamouflageSpliceGate` to open (and/or `SetCamouflageSplice(true)`).

`extender.ExtenderCamouflageSpliceSettle` encodes step 2 as the value of
`CamouflageSpliceGate`. It opens only after the extender's own activation records
have carried its key and required the capability, unbroken, for the longest
record lifetime in the run plus a margin (1 h by default, so about 25 h today). A
record without either, an activation whose record does not verify, or the latest
record expiring closes it at once and restarts the wait. It keeps no state across
a restart, so a restart of a cohort host restarts the wait and the host runs as
Phase A for that long. Rollback is the reverse: turn the extender flag off first
(`SetCamouflageSplice(false)`), then remove the key from the cohort config; the
gate also closes when the requirement leaves the record. Posture changes apply to
new connections; working splices drain.

## Readiness gates to record before each cohort expands

Record these in the cohort's rollout config before Phase B, per §3.1–§3.4 and
the delivery-gates table. A cohort expands only when all hold.

1. **Baseline** (measured in Phase A, per country and address family):
   connection success rate, time to first usable connection, gated/open
   selection and fallback reasons, release denials, refresh lateness.
2. **Observation period**: the minimum time the cohort runs at its current size
   before expanding (at least one record lifetime so legacy clients retire;
   owner sets the numeric value).
3. **Numeric acceptance thresholds** (owner-set; floors, not guesses):
   - compatible-client connection success ≥ baseline minus an agreed margin;
   - authenticated-outcome share of tcp connections ≥ an agreed floor;
   - `SpliceFallbackTerminatedCount` rate and `SpliceDialFailedCount` rate below
     agreed ceilings (the degraded state must be rare);
   - `BorrowVerifyFailCount` low enough that verified-target coverage holds.
4. **Rollback thresholds** (owner-set): the target-availability and
   client-success levels below which the cohort rolls back to Phase A. Crossing
   either triggers the rollback above.
5. **Co-hosted carrier inventory** (§3.3): list every UDP/DNS carrier sharing
   the extender's IP (quic 443, dns 4053/53). A recognizable UDP/DNS response on
   the same IP can still expose the extender; this is a TCP-only delivery and
   makes no endpoint-wide claim. Record which carriers co-reside and the owner's
   decision on each before Phase B.
6. **Max measurement age** and **revalidation interval** of the borrowed-site
   list (from slice E's manifest). The client stops claiming the capability when
   its list is older than the max age; the extender revalidates targets at role
   start and every revalidation interval. Record both numbers here before
   Phase B. (Pending slice E.)
7. **Aggregate borrowed-target load** (§3.3): per-extender caps do not bound a
   fleet's traffic to one site. Before expanding, confirm the summed
   per-target load (below) across the cohort is within what each borrowed site
   can absorb.
8. **One holder per cohort identity.** An activation that posts no camouflage key
   clears the stored one (14b), so a network whose clients share one identity on
   mixed client versions flips the key on and off with each activation. One
   network shows 12 keys changing holder 332 times in 48 h (2026-11-02). That is
   harmless in Phase A. In Phase B the record loses its key, and with it the
   capability requirement, whenever an old client activates, so a client issued
   that record dials the legacy hello alone and a splicing holder sends it to the
   borrowed site. The safe-flip gate does not catch this: a camouflaged client
   sees only its own activations' records, and each carries the key it just
   posted. Before adding a key to a cohort, confirm that one client holds it, for
   example with a read-only count of distinct `client_address_hash` per
   `ip_version` for the extender in `network_extender_activation`. More than one
   in a family means a shared identity or a host that moved.

## Telemetry to watch

Per-extender, `ExtenderServer.CamouflageStats()` (fixed label sets, no
per-client/address labels):

- `AuthenticatedCount`, `SplicedCount`, `TerminatedCount` — the outcome
  partition of tcp connections;
- `SpliceFallbackTerminatedCount` — splice on but no reachable target; the
  degraded exposure of §3.3, counted apart from the Phase-A `TerminatedCount`;
- `ReplayRefusedCount`, `TimeWindowFailedCount` — replay and skew refusals;
- `SpliceDialFailedCount`, `BorrowVerifyFailCount` — target failures;
- `TargetLoads[name] = {SpliceCount, RelayedByteCount}` — per-borrowed-name
  load, so a fleet collector sums one target's load across extenders (gate 7).

`ExtenderServer.CamouflageStatus()` gives the honest coverage verdict:
`Covered` is true only in the splice pilot with verified targets, the gate
open, and no running fallback; otherwise `DegradedReason` names why
(`camouflage_off`, `splice_off`, `splice_gate_closed`, `no_verified_target`,
`splice_fallback_active`). **A deployment whose `CamouflageStatus().Covered` is
false must not be reported as providing camouflage coverage.**

Added 2026-11-02: with camouflage on, `no_verified_target` takes precedence over
the posture and the gate, since a role with no name posts no key and its gate is
closed because of that. Under a role, `coverage_incomplete` says the splice runs
but a name this extender's supported clients select did not verify, or a list is
unmeasured or past its maximum age. A prober replaying that name meets a
substitution, which is degraded (`connect.EvaluateExtenderCamouflageCoverage`).
Roles log the verdict and the counts as one line, which is the only place an
operator can read them on a host outside a reporting cohort (Reporting path, below):

    extender camouflage: authenticated_rollout, not covered: splice_off; 3 verified names, 0 failed verifications; 12 authenticated, 0 spliced, 40 terminated, 0 fallback terminated

`not covered: splice_off` is the correct line for a healthy Phase A host.

Client-side, `connect.ExtenderTcpCamouflageClientStatsSnapshot()`:
`CamouflageWonCount`, `LegacyWonCount` (Phase A fallback), `BothFailedCount`
(the Phase B loss a skewed/legacy client meets), `RefusedCount`,
`CamouflageUnauthenticatedCount` / `CamouflageLeafRejectedCount` (how a
camouflaged attempt failed — Phase A terminate vs Phase B splice), and the
`LegacyOnly*` reasons a dial ran legacy alone.

Aggregate all of the above by country, address family, and client/resource
version, which the reporting path below does. Never export a gated address, key,
client or network id into telemetry.

## Reporting path

Client and extender post what they counted to the operator, which adds it to
Prometheus counters and keeps nothing else. No table and no migration.

Status 2026-11-02: built and off on both sides. Nothing is sent until a cohort is
turned on and the operator's switch is on. Dashboard: `urnetwork / extender
camouflage` (`extender-camouflage.json`).

**Turning it on.** All of these, in this order:

1. Operator: `camouflage_report: { enabled: true }` in vault `extender.yml`, then a
   rolling api restart, in waves. It is read once per process. Off, the default, the
   route answers `disabled` and counts nothing. **This is the kill switch**: turn it
   off and restart, and senders go away for about six hours at a time.
2. Clients and SDK provider roles: `NetworkSpaceValues.ExtenderGatedPreference`, the
   gated preference's cohort control. Every cohort enabled for gated selection reports,
   and no new value reaches the exported SDK surface.
3. Operator hosts: `connectctl extender --camouflage_report`. A host has no network
   space, so the flag is its cohort control. Off by default.

**What is sent.** `POST /network/extender-camouflage-report` under the sender's client
JWT, with a random report id and the increase since its last report of
`ExtenderTcpCamouflageClientStatsSnapshot()` and, for an extender,
`CamouflageStats()`, the load per borrowed name, and the `CamouflageStatus()` verdict.
It also carries the build, the borrow list version and the families it dials. It never
carries a gated address, a key, a client id or a network id. Client and extender run
the same sender (`connect.ExtenderCamouflageReporter`).

**When.** One loop, one post at a time. The first look is 5 to 15 minutes after the
sender starts, then hourly within 15 minutes either way. A client that never dials an
extender never posts; an extender gives its verdict each hour. A failed post is
retried at most three times over a doubling, jittered backoff from 5 minutes, then the
report is given up, and each one given up in a row doubles the wait, to six hours. An
operator gone for a week costs about four posts every six hours per sender. An account
change cancels what is out and drops everything counted so far.

**Counted once.** The gates can read these as lower bounds. Every failure undercounts
and none overcounts:

- A retry posts the same id and the same bytes. The operator claims an id in Redis for
  three hours and a sender stops at two, so a retry of a post whose answer was lost
  answers `duplicate` and adds nothing. What is counted meanwhile waits for the next
  report under a new id.
- A restart starts the counters and the record of what was taken at zero together, both
  in process memory, so nothing can be sent twice and what was not delivered is lost.
- Senders in one process share that record, so one count goes to one sender.
- A report given up is lost, not merged into the next, since merging could count it
  twice if a post of it had landed.

**What the server does with it.** It authenticates the client JWT, checks the payload
(16 KiB, the bounds in `controller/extender_camouflage_report.go`, unknown fields
ignored), spends the client's budget of 12 reports an hour, and claims the id. Then it
increments counters, and that is all it writes. The identity is not recorded: the
budget's Redis key is a keyed hash of the client id that expires with its window, and
the id's claim holds nothing about the caller.

| Metric | Labels |
| --- | --- |
| `urnetwork_extender_camouflage_reports_total` | `outcome`: how each report ended |
| `urnetwork_extender_camouflage_client_outcomes_total` | `outcome`, `country`, `family` |
| `urnetwork_extender_camouflage_client_version_outcomes_total` | `outcome`, `client_version`, `resource_version` |
| `urnetwork_extender_camouflage_extender_outcomes_total` | `outcome`, `country`, `family` |
| `urnetwork_extender_camouflage_extender_version_outcomes_total` | `outcome`, `client_version`, `resource_version` |
| `urnetwork_extender_camouflage_status_reports_total` | `verdict`, `country`, `family` |
| `urnetwork_extender_camouflage_status_version_reports_total` | `verdict`, `client_version`, `resource_version` |
| `urnetwork_extender_camouflage_target_splices_total` | `target` |
| `urnetwork_extender_camouflage_target_relayed_bytes_total` | `target` |

Every label value comes from a fixed set the server holds. `target` is a name in the
api build's own bundled borrow lists, else `other`, and with today's empty lists that is
all of it: gate 7 can be read per site only once the api build carries the measured
list. `client_version` is a calendar release day while recent and its month after;
`connectctl` hosts read `unknown` or `other`, so read hosts by `resource_version`.
`resource_version` is `none`, a version within four of the api's own, or `other`.

**Reading the numbers against the gates.**

- `country` is the server's own lookup of the caller's address, never the report's
  word. A report that crossed an extender reads `relayed`, whose country says nothing of
  the client, so a client in a blocked country that reaches the api only through
  extenders is in `relayed`, not in its country. Its counts still reach the fleet totals
  and the version series. A relay the operator does not know, a VLESS server or a user's
  proxy, reads as that relay's country.
- Clients that cannot reach the api at all are missing, and a process that exits before
  its first look is missing. A real outage reads better than it is, so read
  `both_failed` and the release denials together, and never take a quiet series as good
  news: check `reports_total` first.
- A client's counts are its own word. One identity can add at most 65,536 per counter
  per report and 12 reports an hour, enough to move a small cohort's rate, so a jump in
  one series needs reading against `reports_total` before it moves a gate. An extender's
  part counts only from a client that activated an extender that is active now, so a
  stranger cannot trip a rollback threshold through it. That check reads the fleet into
  process memory once per process per ten minutes.
- A verdict goes out about once an hour per extender, so
  `status_reports_total` over an hour is about a count of extenders by verdict, and only
  of those that report. Only `covered` is coverage.
- At full reporting the claims take about 120 bytes of Redis per report for three hours,
  and a budget key about 100 bytes, so about 500 bytes per reporting sender. Check it
  before a cohort of millions.

## Operator probes in Phase B (added 2026-11-02)

A Phase B extender splices every unauthenticated tcp hello to its borrowed site,
the operator's own probes included. A probe that dials the legacy hello alone
fails the B3 leaf check, so the extender's activation is refused and its uptime
probe fails, and it drops out of the directory. Before any cohort enters Phase
B, confirm all of these:

1. The operator runs the camouflage-key probes:
   `connect.ProbeExtenderCarrierWithCamouflageKey` and
   `connect.ProbeExtenderForwardWithCamouflageKey`. Server 14b uses them, with
   the posted key for the activation probe and the stored key for the uptime
   probe.
2. The api and taskworker builds carry the measured borrow list. The probe's
   camouflaged attempt fronts a name of the operator's bundled global list, and
   with no list the probe is legacy alone.
3. Each cohort extender's record carries its `RealityPublicKey`. The uptime
   probe can only use a key the operator stored.

In Phase A the key changes nothing a probe proves. A wrong key still passes
over the legacy attempt, so a passing Phase A activation is not proof that the
extender holds its camouflage key. In Phase B only the extender's own key
passes.

An NLayer hop URL (`connectctl extender`) has no camouflage key parameter, so a
hop dials its tcp carrier with the legacy hello. A Phase B extender cannot serve
as a tcp hop until the hop URL gains one.

Added 2026-11-02: the hop URL has one now, `camouflage_key=<hex>` beside `key=<hex>`
(Role options). The dial is then the client's race, fronting a name of the front
extender's bundled global list, so the front's build must carry the measured list.
With no list it is the legacy hello alone and a Phase B hop splices it away.

## Splice posture semantics (added 2026-11-02)

`ExtenderCamouflageSplice` is the posture until `SetCamouflageSplice` sets one.
After that, the run-time value wins over the setting, so a rollback through
`SetCamouflageSplice(false)` holds even if the setting still says on.

## Turning on Phase A on one host (added 2026-11-02)

Phase A changes nothing for a legacy client: it still reaches the host on the legacy
hello. Do this only once all of these hold:

- Server 14b is merged, its migration applied, and api and taskworker deployed, so
  the key is stored and signed. (Without it the host still runs: the server ignores
  the posted key, and no client learns it.)
- The measured list and its manifest are bundled (slice E), and the `connectctl`
  and client builds in use carry them.
- The host's `connectctl extender` already passes `--extender_key_file` or
  `--state_dir`. Do not change which: a new file is a new identity.

Then, through xops (host changes never go ad hoc):

1. Add `--camouflage` to that host's arguments. Leave out `--camouflage_splice`.
   Leave out `--camouflage_revalidate_interval` unless the owner has set a value.
2. Deploy the build and restart the extender on that host.
3. Read the host's log. Expect `extender camouflage: on, splice false, ...`, then
   `extender camouflage: authenticated_rollout, not covered: splice_off; N verified
   names, ...` with N at least 1. If the reason is `no_verified_target`, no name
   verified and no key was posted: check the host's egress on each family before
   going on. Failed verifications are retried from 5 m.
4. Confirm the record, with a read-only query that prints no key (`SET LOCAL
   statement_timeout` first):
   `SELECT reality_public_key IS NOT NULL FROM network_extender WHERE public_key = decode('<identity public key hex>', 'hex');`
   The identity key is the `extender public key:` line of the log.
5. Over the next record lifetime, as clients with the list refresh it, `authenticated`
   in the log line rises. `terminated` keeps counting legacy clients and probers,
   which is what Phase A is for.

Rollback: remove `--camouflage` and restart. The next activation posts no key, and
the server clears it. A client still holding a record with the key fails its
camouflaged attempt and wins on the legacy one.

Moving that host to Phase B is a separate step, under every gate above: add its
identity public key to `directory.capability_cohorts` in vault `extender.yml`, confirm
the probes and the list on the operator's side (Operator probes in Phase B), and add
`--camouflage_splice`. Nothing splices until the host's records have carried the key
and the requirement for one lifetime and the margin, about 25 h, and each restart
restarts that wait. Until then the log says `splice_gate_closed`.
