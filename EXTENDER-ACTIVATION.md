# Extender activation: how it works, what Main shows, and the fix under review

For peer review. Written 2026-11-01.

**Under review:** branch `lagdev/activator-per-family`, commit `fbf99636` ("Give each extender family its own activation schedule"). It is cut from connect main `f7741e6d` and not merged. EXTENDER.md G3 is the normative text. This document explains why it changed and what Main showed.

**Already decided:** the owner agreed, 2026-11-01, that a family whose only addresses are private still gets one post at start and one a day (§4.2).

**Not decided:** everything in §5, and the questions in §6.

## 1. Summary

- **What activation is:** each extender tells the operator, per address family (IPv4, IPv6), that it serves at that family's address. The operator probes the address, records it, and publishes a signed record to the directory, which clients read.
- **What Main showed (2026-11-01):**
  - The median extender wrote 2 activation rows in 24 h; p90 was 14 and p99 53.
  - The healthy network `01994645` wrote 74 rows for its 74 extenders.
  - The excess has five drivers (§3). Two are bugs in connect's activator. One is host instability. One is our probe and revocation loop. One is a deployment that shares one identity among many clients.
- **The fix fixes the two activator bugs:**
  - Each family gets its own schedule and backoff.
  - The hourly address check is compared within the family the hello arrived on.
  - Expected effect: about 30% fewer activation rows fleet-wide, about 600–650 a day. Hosts with one family always refused go from about 70 rows a day to about 19.
- **The fix does not touch** restart loops, the revocation loop or shared identities. §5 proposes what would.
- **Where to look hardest:**
  - any path that could leave a family never activated again;
  - the worst-case delay for noticing a real address change;
  - the private-address policy (§6).

## 2. How activation works

### 2.1 The extender (connect `net_extender_activate.go`)

**Triggers.** A pass runs:
- at start;
- on a 24 h tick (`ActivateTimeout`);
- when the extender sees its own key revoked in the directory (`observeRevocation`, l.683);
- on a network change, and on `Reactivate`;
- when the hourly address check (`AddressCheckTimeout`, 1 h) sees a moved address.

The defaults are in `DefaultExtenderActivatorSettings` (l.272).

**A post** goes to the family's api url, for example the v4-only host, with the client jwt, through a direct-only client strategy. An activation relayed through another extender would publish that extender's address. The result's bootstrap records are applied to the local directory and the status is recorded (`applyRecords`, `recordSuccess`, `recordFailure`).

**Backoff:** 10 min doubling to 6 h (`MinBackoff`, `MaxBackoff`).

**The address check** is one hello to the plain dual-stack api url (`hello`, l.821). It compares the caller address the operator saw, by address and not port, with the last one.

**The measurement request** for lag.dev's certificate rides the first post of a pass that can carry it (lag.dev/PLAN.md C4, `followMeasurementCsr`, l.1004).

**Sleep:** the tick, the hourly check and the backoff count time the host slept. A minute after waking from 15 minutes or more of sleep, whatever came due is due.

**Old rule, connect main G3:** "a refusal of any attempted family holds the whole pass and the retry reissues both", and "the hourly address check is one hello for both families".

### 2.2 The operator (server)

**The handler** is `POST /network/extender-activate`, `controller/extender_controller.go` `ExtenderActivate` (l.432).
- The family's address is the caller's address.
- The activation probe dials it back with a 10 s budget (`ExtenderActivateProbeTimeout`, l.66) before anything is stored.
- The location comes from GeoLite2 for that address (`GetLocationForIp(clientIpStr)`, l.674).

**The model** is `model/network_extender_model.go` `ActivateNetworkExtender` (l.512).
- `network_extender` is upserted by key: `ON CONFLICT (public_key) DO UPDATE SET network_id, client_id, …, country_code, active = true` (l.557).
  - Any client holding the key can take the row over (§3, item 5).
  - `country_code` is one column both families write, so the family that posted last wins.
- `network_extender_address` is upserted per family, and the upsert resets `consecutive_probe_failures = 0` (l.626).
- Every activation writes a `network_extender_activation` row: ip_version, a peppered hash of the address, location ids, accuracy. It never stores the raw address.
- A new signed record is published.

**The probes** run in `taskworker/work/extender_probe_work.go`.
- Every 5 min, each active address is dialed over its tcp carrier with a fresh challenge (l.42), with a 15 s budget per address (l.46).
- Six failures in a row, half an hour, deactivate the address. One success clears the count.
- Losing the last address revokes the extender. That is the only way a record is withdrawn before it expires.
- The extender sees its own revocation in the directory and re-activates at once.

## 3. What Main showed (2026-11-01, read-only)

The numbers come from `network_extender_activation`, `network_user_action_attempt` (refused attempts), `network_client_connection` (platform connections), `audit_extender_event`, lag.dev's `lag_result`, and read-only HTTPS probes of measurement names. A pass is the rows one extender wrote within 5 s of each other; a full pass is v4 then v6, about 0.5 s apart.

### Item 1. Restart loops (host)

Saginaw `01a17c4f`, US, network `01983769`: 51 rows in 24 h, about 26 passes.
- **Every attempt succeeded:** all 55 attempts. It has had no probe failures and no revocation since Oct 28. Its address hashes and country never changed.
- **Every pass from 13:15 to 20:53Z followed one sequence:** both platform connections retire, then 1.5 to 11 min later a pass runs, then new connections arrive 4 s after it. Fleet-wide over the same window, 523 of 590 passes ran while the process stayed connected.
- **One stall, captured with probes every 20 s:**
  - ping reports stopped after 21:03:43;
  - from 21:05:26 to 21:07:57, TCP to 443 connected in about 70 ms on both families, but TLS never answered;
  - connections retired at 21:05:37;
  - a pass ran at 21:08:15, and TLS answered at 21:08:18.

  A lag.dev health-check timeout at 19:59:18 fits the same pattern.
- **It tracks provider load.** On Nov 1 Saginaw carried 8k–34k contracts an hour from up to 29k sources. It ran 15 h without restarting at 3k–9k an hour. Its 7 siblings on the network carry about 5–40 an hour together, and never restart.
- **Others in loops:** `01a18c5e` entered the same loop after a surge to 12.8k contracts an hour. `01a1904e` (hk, since deactivated) restarted every ~3 min with almost no load.
- **Likely cause:** memory exhaustion followed by an OOM kill. The miner sets only a soft Go limit of 4/5 of host memory (`sn/miner/provider_memory.go`), and its systemd unit is `Restart=on-failure` with no watchdog. Unconfirmed: we have no host logs.
- **Impact:** about 28 stalls on Nov 1, each 2–15 min with TLS timing out on both families. lag.dev tickets issued within 15 min before a restart got results 29 of 47 times, against 39 of 50 otherwise. Saginaw published 62 records in 24 h, against 4.6 per extender on the healthy network.

### Item 2. Activator: a refused family re-posts the working one (fixed by this branch)

`FamilySupported(4)` counts a private RFC 1918 address as v4, so a NAT'd host tries v4 every pass, and the operator refuses it every time.
- **What the old rule did:** the refusal held the whole pass, so the working family was re-posted at 10, 20, 40, 80, 160 and 320 min, then every 6 h. Example: `01a17d0b` posts at 01:01:47, 07:01:47, 13:01:47 and 19:01:47 daily.
- **Scale:** 19 extenders had one family always refused. They wrote 70 rows in 24 h instead of about 19, plus 62 refused attempts, each costing the operator a probe of a dead address.

### Item 3. Activator: the hourly check flips family (fixed by this branch)

The hello goes to the plain dual-stack url, so it arrives over v4 one hour and v6 the next.
- The old check saw a moved address each time and ran a full pass.
- From 13:15 to 20:50Z, 79 passes on dual-stack hosts came right after a fully successful pass, excluding passes after a revocation. 45 of them came a whole number of hours after the previous pass, on 26 extenders: about 140 a day.
- The existing test `TestExtenderActivatorReactivatesOnAddressChange` covered one family only.
- Example, France `01a146e9`: low load and an unbroken connection. An hourly-check pass found v6 refused that one time, which started a 10/20/40 min chain of v4 re-posts: 11 v4 rows against 2 v6 in a day.

### Item 4. Probe revocation, then immediate re-activation

- **Scale:** 183 revocations in 24 h across 65 extenders; 174 were on network `01979e1c`. 57 were re-activated within 15 min.
- **Example:** `01a18644` was revoked 18 times in 14 h, each 33–36 min after its last activation. The activation probe passes, then six uptime probes in a row fail.
- **Background failure rate:** each probe sweep fails 59–82 of about 430 addresses.
- **Why it loops:** a revocation is the only way the operator tells an extender to re-activate, and the extender does so at once.

### Item 5. One identity shared by many clients

- **Scale:** on network `01990418`, 63 provider clients connect from one IPv4 for extender `01a1871a`. 32 of them posted 142 v4 activations of that one key in 24 h, and 110 succeeded.
- **Symptom:** the row's `client_id` changes hands; it was seen changing between two reads. Across the network, 4 extenders got 162 activations a day from other clients.
- **What allows it:**
  - the sn miner gives every slot the same `.provider.extender.key`;
  - the operator lets any client re-activate a key (§2.2).

### Side effects worth knowing

- **Records:** every activation publishes a new signed record, even when nothing changed. That's the gossip churn above.
- **The probe count:** an activation resets `consecutive_probe_failures`. So an extender that restarts more often than every 30 min is never deactivated, however long it stalls between restarts.
- **Country:** for the 39 extenders whose families geolocate to different countries, the stored `country_code` is the family that posted last, which in practice is v6, since v6 posts second. lag.dev's place fix (server branch `lagdev/extender-place`) stops lag.dev depending on it. The column itself is unchanged.

## 4. The fix under review: `lagdev/activator-per-family` (`fbf99636`)

### 4.1 What changed (`net_extender_activate.go`, `control_family.go`, EXTENDER.md G3)

- **Schedules:** each family has its own `extenderActivateSchedule` (l.379): a 24 h tick, and a hold of 10 min doubling to 6 h (`tick` l.387, `hold` l.394).
  - A refused family holds only itself, so it never re-posts the family that works.
  - A success moves the family back to its tick and resets its hold.
  - `dueScheduleKeys` (l.730) and `nextActivateWait` (l.742) pick what's due.
- **Triggers that post every family,** as before: start, an observed revocation, a network change and `Reactivate` (`scheduleEveryFamily`, l.722).
- **A family with no global address** on the host (`FamilySupported`, `control_family.go` l.550) isn't posted. It's looked at again on its own hold, so one that gains an address after start is posted within 10 min, not at the next 24 h tick.
- **The address check** (`checkAddress` l.752, `addressChange` l.777) is still one hello to the plain url, now compared within the family it arrived on.
  - These count as unchanged: an address the operator published for any family, and the last address that family's hello saw. So a dual-stack host whose hello alternates families posts nothing, and a host whose hello leaves from another of its addresses than its activation posts once, not hourly.
  - A family that really moved posts that family alone.
  - A family that isn't activated is left to its schedule.
- **The measurement follow-up** goes only to the families the pass activated. So a new identity whose v4 is refused sends its request on v6 at once.
- **Fallback mode** (an operator with no family api hosts) keeps one schedule, posting the plain url once a cycle.
- `Status()`, its change monitor and every existing setting are unchanged.

### 4.2 Private addresses: one try at start, then the daily tick (decided)

`FamilyPublic` (`control_family.go` l.579) treats a family as private when all its global addresses are RFC 1918, RFC 4193 (ULA), carrier-grade NAT (100.64/10), link-local or the 464XLAT range.
- **What happens:** a private family is still posted at start, on its tick and on every trigger, like any family. If it's refused, it waits for its next tick instead of the backoff, since a NAT that doesn't forward the carrier ports refuses every retry alike. For a NAT'd v4 that's one refused try per start plus one a day, down from 4–9 a day.
- **Why not skip private families entirely:** that would cut off every host whose only v4 is private but does forward 443, such as home routers and docker-bridge providers with published ports. EXTENDER.md C2a and S plan for NAT hosts, including later UPnP/PCP mapping. Main has such candidates today: `orochimura`, whose v4 serves only tcp, and `ur712`, which is v4 only.
- **The owner agreed to this on 2026-11-01.** Skipping instead is a one-line change in `activateFamily` (l.951): return skipped when `!ipVersionPublic`.

### 4.3 Tests

New, in `net_extender_activate_family_test.go`, on a fake clock:

| Test | Line |
|---|---|
| `TestExtenderActivatorHoldsARefusedFamilyAlone` | l.65 |
| `TestExtenderActivatorIgnoresAHelloOnTheOtherFamily` | l.126 |
| `TestExtenderActivatorPostsTheFamilyWhoseAddressMoved` | l.147 |
| `TestExtenderActivatorPostsAnotherSourceAddressOnce` | l.192 |
| `TestExtenderActivatorPostsEveryFamilyOnRevocation` | l.213 |
| `TestExtenderActivatorHoldsARefusedPrivateFamilyForTheDay` | l.248 |
| `TestExtenderActivatorKeepsAnAcceptedPrivateFamily` | l.287 |
| `TestExtenderActivatorPostsAFamilyOnceTheHostHasAnAddress` | l.315 |
| `TestExtenderActivatorMeasurementCsrFollowsOnTheActivatedFamily` | l.348 |
| `TestFamilyPublicReadsTheInterfaceTable` | l.371 |

All 27 existing `TestExtenderActivator*` tests pass with their assertions unchanged. One fixture changed in `net_extender_activate_test.go`.

| Command | Tests | Result |
|---|---|---|
| `GOWORK=off go test -run 'TestExtenderActivator\|Activat\|TestFamilySupported\|TestFamilyPublic\|TestProbeFamily' -count=1 -v .` | 49 | pass |
| `GOWORK=off go test -run 'Family\|ControlFamily\|ControlIp\|Demot\|TestExtenderActivator\|Activat' -count=1 .` | 182 | pass |
| `GOWORK=off go test -run 'TestLagCert' -count=1 ./extender/` (two drive the real activator) | 20 | pass |
| sdk extender-role tests against this branch, through a scratch `go.work` | 122 | pass |

`go vet` is clean for `.`, `./extender/` and `./connectctl/`. The full root suite (over 30 min) wasn't run.

### 4.4 Expected effect on Main once released

Main wrote 795 rows from 13:15 to 22:15Z, about 2,100 a day.

| | Rows removed or new rate |
|---|---|
| Whole fleet | about 241 per 9 h (600–650 a day, roughly 30%) |
| — hour-aligned passes from the family flip | 129 per 9 h |
| — re-posts during the other family's hold | 112 per 9 h |
| Hosts with one family always refused (19) | 70 a day to about 19, and refused attempts to one a day plus one per restart |
| France `01a146e9` | about 13 a day to about 2 |
| Restart loops, revocation loops, network `01990418` | unchanged |

### 4.5 Rollout

- **No code change elsewhere:** the sdk extender role, the sn miner and connectctl leave `IpVersionSupported` and `IpVersionPublic` unset, so they take the activator's new defaults. Mobile apps have no extender role.
- **sdk and sn** use `replace github.com/urnetwork/connect => ../connect`.
- **The build repo** pins connect, sdk and sn as submodules for each release. The current release is `v2026.10.27-1083160450`.
- **The path:** merge to connect main, then the next build release takes the new pin. Miners installed as a service update within a day through their 6-hour update timer.
- **Watch on Main afterwards:** the 6-hour single-family rows and the hour-aligned full passes should disappear. The queries are in the appendix.

## 5. What this branch does not fix (proposals, not implemented)

### Item 1. lag.dev's offers skip unstable extenders

Server branch `lagdev/offer-stability`, in progress: `GET /lag/extenders` skips an extender in a restart loop, or one whose last uptime probe failed, with thresholds from a 48 h replay. It's a lag.dev mitigation, and it doesn't stop the loops.

### Item 2. Restart loops

- Ask the operators of networks `01983769` and `01979e1c` for `journalctl`, `dmesg` (OOM) and host memory.
- Add a restart count and the previous exit reason to the activation args, so restarts show up directly rather than inferred from connections.
- Profile the provider at about 20k sources an hour.
- The miner's supervisor (`operators_supervisor.go`) restarts every child when the memory share changes. Rule that out as a cause.

### Item 3. The revocation loop

- **Diagnose first:** find out why uptime probes fail right after a passing activation probe on about 56 hosts on `01979e1c`. Log probe errors at V(1) for those addresses for a while, and check A12 admission (8 actions per subnet per minute, `connect/extender/extender.go` l.115).
- **Then one of:**
  - **Operator side:** after a probe revocation, store the re-activation but publish its record only after the next uptime probe passes (`model/network_extender_model.go`, `taskworker/work/extender_probe_work.go`).
  - **Extender side:** re-activate after a revocation on the backoff instead of at once (`observeRevocation`).
- **Risk:** slower recovery after a real outage.

### Item 4. Shared identities

- **Miner:** only the direct slot runs the extender role (`sn/miner/run.go`, `provideWithProxy`).
- **Operator:** refuse a key that a different client activated in the last 24 h, and add `client_id` to `network_extender_activation`.
  - It needs a migration, which blocks Main deploys until it's applied.
  - It needs a takeover rule for legitimate re-registration.

### Item 5. The probe-failure reset

Should an activation stop resetting `consecutive_probe_failures`? Today a restart every 25 min keeps a stalling extender active indefinitely.

### Item 6. Record churn (optional)

Skip publishing a new record when an activation changes nothing and the current record has more than 12 h left. It must still publish after a revocation.

## 6. Questions for reviewers

1. **Can any path leave a family never activated again?**
   - For example: a hold that's never re-armed after an error; a revocation arriving during a hold; a network change during a pass; a family whose address appears late; context cancellation.
   - An extender that stops activating drops out of the network.
2. **Is the detection delay for a real address change acceptable?**
   - A moved family is seen by the hourly check only when the hello arrives on that family. If hellos always arrive over v6, a v4 change waits for v4's 24 h tick.
   - The old check had the same blind spot, because the v6 hello address doesn't change. So this isn't a regression, but is 24 h acceptable?
   - Meanwhile the old v4 address fails its probes, and after 30 min it's deactivated. The v6 address keeps the extender's record alive.
3. **Private families:** is one try at start and one a day right, or should NAT'd families be skipped outright (§4.2)?
4. **Concurrency:** check the shared state between `run`, `observeRevocation`, `Reactivate` and the network-change observer, and the timer resets. Run the new tests with `-race`.
5. **§5 item 3:** for the revocation loop, do you prefer the operator-side fix or the extender-side one?
6. **§5 item 4:** what takeover rule would you accept for a key that moves between clients?
7. **§5 item 5:** should activation keep resetting the probe-failure count?

## Appendix: reproducing the numbers (Main, read-only)

Timestamps are UTC without a zone.

Activation rows per extender, last 24 h:

```sql
select extender_id, count(*) rows,
       count(*) filter (where ip_version = 4) v4,
       count(*) filter (where ip_version = 6) v6
from network_extender_activation
where activate_time > now() at time zone 'utc' - interval '24 hours'
group by 1 order by 2 desc;
```

Passes (rows within 5 s grouped), and whether the process stayed connected through each:

```sql
with a as (
  select extender_id, activate_time, ip_version,
         case when activate_time - lag(activate_time) over (partition by extender_id order by activate_time)
              < interval '5 seconds' then 0 else 1 end newpass
  from network_extender_activation
  where activate_time > now() at time zone 'utc' - interval '24 hours'
), p as (
  select *, sum(newpass) over (partition by extender_id order by activate_time) pass_no from a
), passes as (
  select extender_id, pass_no, min(activate_time) t0, max(activate_time) t1,
         string_agg(ip_version::text, ',' order by activate_time) fams
  from p group by 1, 2
)
select ps.*, exists (
  select 1 from network_client_connection c join network_extender e on e.client_id = c.client_id
  where e.extender_id = ps.extender_id and c.connect_time < ps.t0 - interval '5 seconds'
    and (c.disconnect_time is null or c.disconnect_time > ps.t1 + interval '5 seconds')) stayed_connected
from passes ps order by extender_id, t0;
```

Hosts with one family always refused, last 24 h:

```sql
with succ as (
  select extender_id, count(*) filter (where ip_version = 4) s4, count(*) filter (where ip_version = 6) s6
  from network_extender_activation where activate_time > now() at time zone 'utc' - interval '24 hours' group by 1
), att as (
  select user_id client_id,
         count(*) filter (where action = 'extender_activate_v4') a4,
         count(*) filter (where action = 'extender_activate_v6') a6
  from network_user_action_attempt
  where create_time > now() at time zone 'utc' - interval '24 hours'
    and action in ('extender_activate_v4', 'extender_activate_v6') group by 1
)
select count(*) filter (where s.s4 = 0 and a.a4 > 0 and s.s6 > 0) v4_always_refused,
       count(*) filter (where s.s6 = 0 and a.a6 > 0 and s.s4 > 0) v6_always_refused
from succ s join network_extender e using (extender_id) left join att a on a.client_id = e.client_id;
```
