# Extender activation: how it works, what Main shows, and the fix under review

For peer review. Written 2026-11-01.

**Under review:** branch `lagdev/activator-per-family`, commits `fbf99636` ("Give each extender family its own activation schedule"), `5037a818` ("Hold a private family for the day only until it first activates"), which fixes what review found, `5f28f120` ("Count a family as activated when the directory holds its own record"), which the re-review asked for, and `a2b72f19` ("Read only the newest own record when counting a family as activated"), which fixes the one low finding of its review (§4, Review). It is cut from connect main `f7741e6d` and not merged. EXTENDER.md G3 is the normative text. This document explains why it changed and what Main showed.

**Already decided:** the owner agreed, 2026-11-01, that a family whose only addresses are private, behind a NAT that never forwards, still gets one post at start and one a day (§4.2). After review that applies only while the family hasn't activated, in this run or, as far as the directory's own record shows, before it.

**Not decided:** everything in §5, and the questions in §6. §7 describes what was built after this review for §5 item 2.

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
- when the extender sees its own key revoked in the directory (`observeRevocation`, l.699);
- on a network change, and on `Reactivate`;
- when the hourly address check (`AddressCheckTimeout`, 1 h) sees a moved address.

The defaults are in `DefaultExtenderActivatorSettings` (l.273).

**A post** goes to the family's api url, for example the v4-only host, with the client jwt, through a direct-only client strategy. An activation relayed through another extender would publish that extender's address. The result's bootstrap records are applied to the local directory and the status is recorded (`applyRecords`, `recordSuccess`, `recordFailure`).

**Backoff:** 10 min doubling to 6 h (`MinBackoff`, `MaxBackoff`).

**The address check** is one hello to the plain dual-stack api url (`hello`, l.841). It compares the caller address the operator saw, by address and not port, with the last one.

**The measurement request** for lag.dev's certificate rides the first post of a pass that can carry it (lag.dev/PLAN.md C4, `followMeasurementCsr`, l.1055).

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

## 4. The fix under review: `lagdev/activator-per-family` (`fbf99636`, `5037a818`, `5f28f120`, `a2b72f19`)

**Review (2026-11-01).** An independent review of `fbf99636` found three problems. All three are fixed in `5037a818`, with tests (§4.3); the five tests that cover them fail on `fbf99636`.
1. **One refusal held a private family for 24 h.** It applied to any refusal of a family whose addresses are all private. That includes docker bridges, pods and cloud VMs reached through a 1:1 NAT, not only home routers, and most refusals aren't a NAT's: the operator's rate budget, its configuration or store, a probe flake, the re-post after a revocation. A v4-only private extender whose re-post after a revocation was refused once went unposted for a day, where the old loop retried in 10 min. **Fixed:** only a private family that hasn't activated since start waits for the tick; once it has activated, its refusals use the backoff. An interface table that can't be read counts as public for this.
2. **The per-family hello memory went stale.** The address the last check saw on a family stayed "unchanged" after an activation moved the family's published address. So a host whose v4 moved on a network change, whose checks then came over v6, and whose v4 egress returned to the old address kept a record naming the other one until the tick. **Fixed:** an activation that moves a family's published address forgets that family's last check.
3. **The no-address hold doubled.** A family with no global address was looked at again on a hold that doubled to 6 h, and that hold carried into its first refusal once it had an address. **Fixed:** it's looked at every 10 min without doubling, and its first failure holds 10 min, so "posted within 10 min" holds however long the address was missing.

**Re-review (2026-11-01).** The re-review confirmed the three fixes and found nothing new. It asked for one improvement, made in `5f28f120`: `activatedInRun` starts false at every start, so a private family that does forward, but whose first post of a run the operator refused for its own reasons (a rate budget, a store error, a probe flake), still waited for its tick, and the sdk dropped its mesh address for the day. Now a family also counts as activated when the directory holds this extender's own current record naming an address of the family. The two tests that cover it fail on `5037a818`. Its review found one low issue, fixed in `a2b72f19`: the check read the directory's address entries, which keep the addresses older records of the key named, so v4 still counted after a newer record dropped it; it now reads the newest valid record alone (`CurrentRecordNamesFamily`).

### 4.1 What changed (`net_extender_activate.go`, `control_family.go`, EXTENDER.md G3)

- **Schedules:** each family has its own `extenderActivateSchedule` (l.380): a 24 h tick, and a hold of 10 min doubling to 6 h (`tick` l.392, `hold` l.400).
  - A refused family holds only itself, so it never re-posts the family that works.
  - A success moves the family back to its tick, resets its hold and marks it activated in this run (`activatedInRun`).
  - The schedules move once every post of the pass has been answered (`activate`, l.934), so a record that another family's answer brings counts for the family it names.
  - `dueScheduleKeys` (l.746) and `nextActivateWait` (l.758) pick what's due.
- **Triggers that post every family,** as before: start, an observed revocation, a network change and `Reactivate` (`scheduleEveryFamily`, l.738).
- **A family with no global address** on the host (`FamilySupported`, `control_family.go` l.557) isn't posted. It's looked at again every 10 min without doubling (`recheck`, l.410), so one that gains an address is posted within 10 min however long it went without one, and its first failure after that holds 10 min.
- **The address check** (`checkAddress` l.768, `addressChange` l.797) is still one hello to the plain url, now compared within the family it arrived on.
  - These count as unchanged: an address the operator published for any family, and the last address that family's hello saw, while the family's published address is still the one that hello was read against. So a dual-stack host whose hello alternates families posts nothing, and a host whose hello leaves from another of its addresses than its activation posts once, not hourly.
  - An activation that moves a family's published address forgets that family's last hello (`activateUrl` l.1188, `recordSuccess` l.1275), so a host that returns to an address it left is posted at the next check.
  - A family that really moved posts that family alone.
  - A family that isn't activated is left to its schedule.
- **The measurement follow-up** goes only to the families the pass activated. So a new identity whose v4 is refused sends its request on v6 at once, and a refusal of the follow-up is held as an activated family's.
- **Fallback mode** (an operator with no family api hosts) keeps one schedule, posting the plain url once a cycle.
- `Status()`, its change monitor and every existing setting are unchanged.

### 4.2 Private addresses: one try at start, then the daily tick, while the family hasn't activated (decided)

`FamilyPublic` (`control_family.go` l.586) treats a family as private when all its global addresses are RFC 1918, RFC 4193 (ULA), carrier-grade NAT (100.64/10), link-local or the 464XLAT range.
- **What happens:** a private family is still posted at start, on its tick and on every trigger, like any family. If it's refused while it hasn't activated, it waits for its next tick instead of the backoff (l.966), since a NAT that never forwards refuses every retry alike. For a NAT'd v4 that's one refused try per start plus one a day, down from 4–9 a day.
- **What counts as activated:**
  - it activated since start (`activatedInRun`); or
  - the directory holds this extender's own current record naming an address of the family (`ownRecordNamesFamily`, l.991, through `ExtenderDirectory.CurrentRecordNamesFamily`, `net_extender_directory.go` l.2577): the operator published that family after its probes passed.
    - Only the newest record of the key counts, and only while it's valid, neither revoked nor expired. An address an older record named and the newest one dropped doesn't count, though the directory keeps it; nor does an address another key has claimed since.
    - Local dial evidence doesn't enter, so an address in warning or hold counts.
- **Across a restart:** an app's directory has a store, so it keeps the record. The miner's network space keeps no local state, so its directory starts empty and names the family only once the other family's answer in the same pass, or the feed, brings the record back. So a dual-stack miner whose v4 the operator still holds retries v4 in 10 min, while a v4-only miner whose first post of a run is refused still waits for the tick. Lifting the hold when a later directory change brings the record would close that; it isn't implemented.
- **Once it has activated,** something forwards to it: a home router's port forward, a published container port, a cloud VM's 1:1 NAT. Its later refusals — the operator's rate budget, its configuration or store, a probe flake, the re-post after a revocation — are held on the backoff as any family's are.
- **A refusal that names a wait** (`retry_after_seconds`: a spent activation budget, or a busy operator) takes priority over the one-day hold, whether or not the family is private, because the operator probed nothing, so the refusal says nothing about whether a NAT forwards. The family waits out the named wait instead. The owner's rule still covers refusals where the operator did probe (G3, `TestExtenderActivatorWaitsTheRetryAfterOfAPrivateFamily`).
- **A request that fails,** rather than a refusal, uses the backoff whether or not the family has activated.
- **An interface table that can't be read** counts as public here (`ipVersionPublic` l.1079, `familyPublicReadable` `control_family.go` l.593), so it never lengthens a hold. `FamilyPublic` itself still fails closed.
- **Why not skip private families entirely:** that would cut off every host whose only v4 is private but does forward 443, such as home routers and docker-bridge providers with published ports. EXTENDER.md C2a and S plan for NAT hosts, including later UPnP/PCP mapping. Main has such candidates today: `orochimura`, whose v4 serves only tcp, and `ur712`, which is v4 only.
- **The owner agreed to this on 2026-11-01.** Skipping instead is a one-line change in `activateFamily` (l.1002): return skipped when `!ipVersionPublic`.

### 4.3 Tests

New, in `net_extender_activate_family_test.go`, on a fake clock. Ten came with `fbf99636`. Eight more came with `5037a818`, which also renamed `…HoldsARefusedPrivateFamilyForTheDay` to `…HoldsANeverActivatedPrivateFamilyForTheDay`, three with `5f28f120` and two with `a2b72f19`. The rows marked "review" cover the review's findings and fail on `fbf99636`; "re-review" fail on `5037a818`; "newest record" fails on `5f28f120`.

| Test | Line | Covers |
|---|---|---|
| `TestExtenderActivatorHoldsARefusedFamilyAlone` | l.80 | a refused v4 holds alone, v6 posts once a day |
| `TestExtenderActivatorIgnoresAHelloOnTheOtherFamily` | l.141 | hellos alternating between the activated v4 and v6 post nothing |
| `TestExtenderActivatorPostsTheFamilyWhoseAddressMoved` | l.162 | a moved v4 posts v4 alone, a moved v6 posts v6 alone |
| `TestExtenderActivatorPostsAnotherSourceAddressOnce` | l.207 | a hello from another source address posts once |
| `TestExtenderActivatorPostsEveryFamilyOnRevocation` | l.228 | a revocation posts both families mid-hold |
| `TestExtenderActivatorHoldsANeverActivatedPrivateFamilyForTheDay` | l.265 | a private family refused before it activates, which the directory doesn't name, waits for the tick (decided) |
| `TestExtenderActivatorKeepsAnAcceptedPrivateFamily` | l.304 | an accepted private family keeps its tick |
| `TestExtenderActivatorPostsAFamilyOnceTheHostHasAnAddress` | l.332 | a family gaining an address is posted at the next 10-min look |
| `TestExtenderActivatorPostsAFamilyThatGainsAnAddressLate` | l.365 | review 3: after 12 h without one, within 10 min |
| `TestExtenderActivatorRetriesAPrivateFamilyThatActivated` | l.400 | review 1: accepted, refused once, posted again at +10 min |
| `TestExtenderActivatorRetriesAPrivateFamilyAfterARevocation` | l.432 | review 1: a revocation's re-post refused once, posted again at +10 min |
| `TestExtenderDirectoryCurrentRecordNamesFamily` | l.510 | the accessor over a record that drops v4, a revocation, an address another key claims, an expiry |
| `TestExtenderActivatorCountsOnlyTheNewestOwnRecord` | l.579 | newest record: records `[v4, v6]` then `[v6]`, a refused v4 waits for the tick |
| `TestExtenderActivatorRetriesAPrivateFamilyTheDirectoryNames` | l.620 | re-review: the directory names it, its first post after a restart is refused, posted again at +10 min |
| `TestExtenderActivatorRetriesAPrivateFamilyAnotherAnswerNames` | l.653 | re-review: the other family's answer in the same pass names it, posted again at +10 min |
| `TestExtenderActivatorDoesNotCountARevokedOwnRecord` | l.686 | a revoked own record doesn't count: the tick |
| `TestExtenderActivatorHoldsAFailedPrivateFamilyOnTheBackoff` | l.726 | a failed request of a private family uses the backoff |
| `TestExtenderActivatorIgnoresAHelloOnAFamilyNotActivated` | l.750 | a hello on a family that isn't activated posts nothing |
| `TestExtenderActivatorForgetsAHelloOnceTheFamilyMoves` | l.771 | review 2: the stale hello posts at the next check |
| `TestExtenderActivatorReadsTheHostsFamiliesByDefault` | l.817 | the real `FamilySupported` and public-address defaults, through the injectable interface table |
| `TestExtenderActivatorTakesAnUnreadableTableForPublic` | l.867 | review 1: an unreadable table counts as public |
| `TestExtenderActivatorMeasurementCsrFollowsOnTheActivatedFamily` | l.887 | the follow-up goes to v6, not the refused v4 |
| `TestFamilyPublicReadsTheInterfaceTable` | l.910 | `FamilyPublic` over every range |

All 27 `TestExtenderActivator*` tests that predate the branch pass with their assertions unchanged. The fixture in `net_extender_activate_test.go` gained a per-family refusal, a per-family published address and constructors that prepare the operator, or the directory, before start.

| Command | Tests | Result |
|---|---|---|
| `GOWORK=off go test -race -run 'TestExtenderActivator\|Activat\|TestFamilySupported\|TestFamilyPublic\|TestProbeFamily\|TestExtenderDirectoryCurrentRecord' -count=1 -v .` | 62 | pass, no races |
| `GOWORK=off go test -race -run 'Family\|ControlFamily\|ControlIp\|Demot\|TestExtenderActivator\|Activat' -count=1 .` | 195 | pass, no races |
| `GOWORK=off go test -race -run 'TestLagCert' -count=1 ./extender/` (two drive the real activator) | 20 | pass, no races |
| sdk extender-role tests against this branch, through a scratch `go.work` | 122 | pass |
| The five tests that cover the review's findings, against `fbf99636` | 5 | fail, as they should |
| The two tests that cover the re-review, against `5037a818` | 2 | fail, as they should |
| The newest-record test, against `5f28f120` | 1 | fails, as it should |

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
- Profile the provider at about 20k sources an hour.
- The miner's supervisor (`operators_supervisor.go`) restarts every child when the memory share changes. Rule that out as a cause. Its restarts report `stop` (§7).
- **Record how each run ended on a systemd host.** Run ids and, under a supervisor, exit reasons are built (§7), but the installed unit runs plain `provider start` with no supervisor (`sn/miner/cli_service.go`), so most hosts report no exit. The unit could add an `ExecStopPost=` that records `URNETWORK_PREVIOUS_EXIT` from `$SERVICE_RESULT` for the next start, with `oom-kill` mapping to `oom`. Only systemd knows that result, and it's the one source of `oom`. It only reaches hosts whose unit gets rewritten: the unit is written by `provider service install`, so updating the binary leaves an installed unit as it is.

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
3. **Private families:** is one try at start and one a day, until the family first activates, right, or should NAT'd families be skipped outright (§4.2)?
4. **Concurrency:** check the shared state between `run`, `observeRevocation`, `Reactivate` and the network-change observer, and the timer resets. Run the new tests with `-race`.
5. **§5 item 3:** for the revocation loop, do you prefer the operator-side fix or the extender-side one?
6. **§5 item 4:** what takeover rule would you accept for a key that moves between clients?
7. **§5 item 5:** should activation keep resetting the probe-failure count?

## 7. The run on each activation (built)

Branches `lagdev/activation-run-id` in connect, server and sn; EXTENDER.md G3 is the normative text. It covers part of §5 item 2: restarts show up directly rather than inferred from connections.

| Field | Format | Who supplies it | Stored as |
|---|---|---|---|
| `run_id` | 32 lower case hex characters, 128 random bits | connect, once per process start | `varchar(32)`, null unless 32 hex characters |
| `run_start_time` | RFC 3339, UTC | connect, the process start | `timestamp`, null unless from 2026-11-01 to an hour past the operator's clock |
| `previous_exit` | `oom`, `stop`, a signal such as `sigkill`, `exit-<code>` | the host, through `URNETWORK_PREVIOUS_EXIT` | `varchar(16)`, null unless one of the words |

- **Never refused when well-typed:** the fields are diagnostics. A string that's malformed is stored as null, and the activation goes on. A value of another JSON type fails the request's decoding, as any field's does.
- **Only stored activations carry a run.** A process whose every post is refused, for a busy operator, a budget or a probe, leaves no row, so it doesn't show here.
- **Most hosts report run ids and no exit.** The installed unit (`sn/miner/cli_service.go`) runs plain `provider start`, and systemd passes nothing to the process it restarts. Only `provider start --all-operators` has a supervisor to set the variable. §5 item 2 proposes the unit recording it.
- **Under a supervisor, `previous_exit` is evidence, not a diagnosis:**
  - `sigkill` appears only when the supervisor survives the kill. An out-of-memory kill that takes the whole unit with it, which is what systemd's default `OOMPolicy=stop` does, restarts the supervisor, and its first child has no exit to report. So the absence of `sigkill` doesn't rule out an OOM kill, and a `sigkill` from outside the supervisor needn't be one.
  - `exit-2` is any Go panic or fatal runtime error, out of memory among them, so it isn't memory-specific.
  - `stop` is the supervisor's own restarts, including a stop it had to force with a kill.
  - Nothing sets `oom` yet: it is there for a host that knows.
- **Shared identities (§5 item 4):** the run belongs to the process, so the slots of one miner share it. Separate processes on one key are a run each, but what counts as a restart is a run that started in the window, so long-lived copies read as none. Copies that all start within a day, as a deployment's do, count for that day.
- **Rollout:**
  - Either side alone is harmless: an old operator ignores the fields, and an old extender omits them.
  - The one ordering is that the server merges before connect or with it, because its spec conformance test requires every field `api/bringyour.yml` lists to be on its args struct.
  - The server's migration blocks deploys on Main until it's applied.
  - connect and sn reach providers with the next build release. Rows carry a run once a deployed server meets a released connect.

**The gauges** (`controller/stats_collector.go`, the extenders dashboard's "restarting extenders (24h)"). They are plain counts with no labels, and a read that fails withdraws both, so an incident shows as a gap rather than a flat line:
- `urnetwork_stats_extenders_restarting_24h`: extenders with 5 or more distinct runs started in the trailing 24 h.
- `urnetwork_stats_extenders_reporting_runs_24h`: extenders with any run id in the trailing 24 h. Read the first beside this one: both are zero until a connect release with run ids reaches the extenders.

Restarts and exit reasons per extender, last 24 h. `runs` is every run seen in the window, and `restarts` the ones that started in it, the gauge's count; a run whose start was stored as null isn't among them. `exits` counts those restarts by how the run before each one ended, and `unreported` is a restart whose host named no exit, which includes the first run of every supervisor and every run under systemd:

```sql
with run as (
  select extender_id, run_id, min(run_start_time) run_start_time, min(previous_exit) previous_exit
  from network_extender_activation
  where activate_time > now() at time zone 'utc' - interval '24 hours'
    and run_id is not null
  group by 1, 2
), restart as (
  select * from run where run_start_time > now() at time zone 'utc' - interval '24 hours'
), exits as (
  select extender_id, coalesce(previous_exit, 'unreported') previous_exit, count(*) n
  from restart group by 1, 2
)
select r.extender_id, count(*) runs, count(x.run_id) restarts,
       (select string_agg(e.previous_exit || ' ' || e.n, ', ' order by e.n desc, e.previous_exit)
        from exits e where e.extender_id = r.extender_id) exits
from run r left join restart x using (extender_id, run_id)
group by r.extender_id
order by restarts desc, runs desc;
```

The runs of one extender in order, with how long each was seen and what ended the one before:

```sql
select run_id, min(run_start_time) run_start_time, min(previous_exit) previous_exit,
       min(activate_time) first_activate_time, max(activate_time) last_activate_time,
       count(*) activations
from network_extender_activation
where extender_id = '<extender_id>'
  and activate_time > now() at time zone 'utc' - interval '24 hours'
  and run_id is not null
group by run_id
order by first_activate_time;
```

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
