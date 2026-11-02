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

Client-side, `connect.ExtenderTcpCamouflageClientStatsSnapshot()`:
`CamouflageWonCount`, `LegacyWonCount` (Phase A fallback), `BothFailedCount`
(the Phase B loss a skewed/legacy client meets), `RefusedCount`,
`CamouflageUnauthenticatedCount` / `CamouflageLeafRejectedCount` (how a
camouflaged attempt failed — Phase A terminate vs Phase B splice), and the
`LegacyOnly*` reasons a dial ran legacy alone.

Aggregate all of the above by country, address family, and client/resource
version. Never export a gated address, key, client or network id into telemetry.

## Reporting path (open gap)

Connect and the SDK expose these counts as in-process snapshots only; there is
no metrics export from client or extender to the server today. Fleet telemetry
and the per-target aggregate load (gate 7) need a reporting path added before
Phase B can be judged against the numeric gates. This is operational work, not
in this slice.

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

## Splice posture semantics (added 2026-11-02)

`ExtenderCamouflageSplice` is the posture until `SetCamouflageSplice` sets one.
After that, the run-time value wins over the setting, so a rollback through
`SetCamouflageSplice(false)` holds even if the setting still says on.
