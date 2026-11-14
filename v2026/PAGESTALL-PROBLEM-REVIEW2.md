# Page-navigation stalls: independent review 2

Reviewed 2026-10-23 against `PAGESTALL-PROBLEM.md`, the working tree at Connect
`5485774e` plus the uncommitted "first wake" diff in
`ip_remote_multi_client_discovery_retry.go`, the qualified OLD/FIX pair under
`/Users/builder/Library/Caches/urnetwork-runall-owned/hosted-h1-current112.PREP.jxp9wXRI/full-correctness.9urtxbr6/native100-verdict-c3-pair.PREP.gf0f57bm/`,
and the private candidate overlays under
`/private/tmp/urnetwork-connect-destination-exhaustion.PREP.acntz2nm/` and
`/private/tmp/urnetwork-connect-discovery-attempt-stamp.PREP.f7jferjk/`.
`PACKETFLOW-STALL.md` and the 2026-10-23 `MEMSTEADY.md` additions were read
for context. No source file was changed; the only command with side effects
was `go test -run 'DiscoveryRetry|DiscoveryExpand|DialFailure' .`, which
passes on the current tree (2.659 s).

Claims below are tagged **[source]** (read from code), **[runtime]** (read
from the retained artifacts), or **[hypothesis]**.

Line numbers refer to the working tree at the time of review.

## 1. The problem, restated precisely

**User symptom.** On iOS over Wi-Fi, rapid successive navigations start fast,
then a new link sometimes pauses 5-15 s before the page opens; previous pages
may still be streaming assets. Not attributed to a specific local or remote
fault.

**Reproduction (`TestPrivateFullTunH1Sdk32LateQualityAdmissionNatural100Pages`).**
A quality window sized for the 32 MiB profile (target 5) holds 4 quality exits;
a fifth provider does not exist until page 1's first SYN reaches an old
provider. All four old providers refuse new connections to page 1's origin
with an immediate generic dial error, which `TcpSequence.Run` turns into a
capacity-class ICMP unreachable (`ip.go:5557-5574`, `classifyDialFailure`
`ip.go:11102-11121`), while they keep serving established traffic. Page 1's
HTML first byte took 17.545 s (OLD) and 31.722 s (FIX, the uncommitted first
wake diff) against a 5 s gate; every other check passed, and the next-slowest
page was ~3 s. **[runtime]**

**What the pause is.** The pause is the time between the late provider
becoming visible through the discovery API and the device's next discovery
listing (14.45 s OLD, 18.22 s FIX), plus evaluation (~0.6-1.5 s), plus the
wait for the application's next SYN retransmission to land on the newly
admitted exit. It is not a body/receive backlog. **[runtime + source]**

## 2. Timeline reconstruction (OLD arm) and the mechanism that produces it

Trace clock = SDK window construction. `TRACE.json` events for the quality
window (OLD): **[runtime]**

| t (s) | event | model annotation **[source]** |
| ---: | --- | --- |
| 1.701 | last setup admission (`admitted prov=2`); window at 4/5 | the resize pass that admitted the four ends; `resizeMonitor.NotifyAll` on admission (`ip_remote_multi_client.go:13157-13159`) makes the next pass start immediately (`:12939`) |
| ~1.7 | (not traced) pass 2 starts: `len(clients)=4 < target 5`, no deferral yet, `expand(needed=1)` (`:12849-12878`) | `expand` calls `generatorMonitor.NotifyAll()` **once per candidate slot** (`:13355`) then blocks on `clientChannelArgs` or `time.After(requestEndTime)` (`:13366`, `:13778`) with `requestEndTime = now + WindowExpandTimeout (15 s)` (`:13339-13345`, `:279`) |
| ~1.7 | (one of the 3 setup-stage `discovery_call`s) the enumerator lists, the API returns nothing new (all 4 excluded, `:11981-11991`) | with zero destinations the enumerator does nothing and parks on `generatorNotify` (`:12156-12160`); **nothing wakes it again inside this pass** |
| 11.107 | `late_relationship_public`; 11.409 `late_named_api_visible` | provider is listable; the live pass has ~5.3 s left but will not list again |
| ~16.7 | (not traced) pass 2 ends unproductive (`addedCount == 0`) | `noteDiscoveryExpandUnproductive` -> `failureCount=1`, `notBefore = now + jitter([5 s, 10 s))` (`ip_remote_multi_client_discovery_retry.go:42-75`, `fixedDestinationRetryDelay` + `retryJitter` `ip_remote_multi_client_fixed_retry.go:42-72`); resize timeout is `15 s - 15 s ≈ 0` so the next pass starts at once (`:12925-12934`), sees the deferral (`:12853`) and sleeps until `notBefore` (`:12926-12928`) |
| 25.857 | `wait_exit`, `discovery_call` | rest ended (jitter 9.16 s, inside [5,10)); pass 3 runs `expand`, wakes the enumerator, which lists |
| 26.253 | `args_received`, `setup_started`; 26.890 `evaluation_passed`, `admitted` | normal acquisition: ~1.0 s listing-to-admission |
| 40.857 | `removed prov=0` (reason not traced); 43.818 re-admitted | see open question Q7 |

Predicted listing window from the model: `1.7 + 15 + [5, 10) = [21.7, 26.7)`;
observed 25.857. The visibility-to-listing gap of 14.45 s is therefore the
**remainder of a 15 s expand pass that will not re-list, followed by the
full first-level unproductive-pass rest**. The FIX arm shows the same shape
(visible 12.118 s, listing 30.333 s, admitted 31.795 s); its trace dropped 58
composed events and its last setup admission time is not retained, so I can
only say it is consistent with the model if that admission was at ~5.3-5.5 s
(its `sdk-window-wait` completed at 5.556 s). The extra ~14 s of FIX TTFB is
within the jitter and phase variance of this mechanism and is not evidence of
a regression, which agrees with the brief.

Residual TTFB after admission (OLD: 17.545 s first byte vs. admission at
~tap+16 s) is the application's SYN retransmission schedule: the flow is
unbound after each refusal (`clientDialFailure` with `DialFailureRerace`,
`:9249-9257`) and **only the app's next SYN** re-races it (`:7386-7394`
comment, `:7530-7817`). The fixture's gVisor stack caps RTO at 8 s
(`tun.go:169`), so by the time the exit is admitted the app's SYN spacing is
already 4-8 s. **[source]**

## 3. Root causes

### RC1 (primary): discovery lists once per expansion slot and then goes deaf for the rest of the pass [source]

* `randomEnumerateClientArgs` captures `generatorNotify` before each listing
  (`:11968`) and, after an empty listing, waits only on that channel
  (`:12156-12160`). Commit `56648d5f` ("retry empty destinations on generator
  notify. do not poll", 2025-12-16) removed the former
  `WindowEnumerateEmptyTimeout` re-poll (`:283`, `:835` are the commented-out
  remains).
* `expand` wakes it exactly once per requested candidate (`:13355`) and
  otherwise blocks until args arrive or the 15 s request deadline (`:13366`,
  `:13778`). With `neededCount=1` and `EvaluationPoolMultiple =
  MemoryScaledCount(2,1)` (`:474`), `requestCount` is 1-2
  (`multiClientExpandPlan` `:12961-12982`): one or two listings at pass start,
  then up to 15 s of silence.
* Consequence: a provider that becomes visible at any time inside a pass is
  not seen until the next pass. Worst-case visibility-to-listing latency is
  `WindowExpandTimeout + DiscoveryExpandRetry(n)`: 20-25 s at `failureCount=1`,
  up to 15 s + 2 min at saturation (`:298-299`).

### RC2: the unproductive-pass rest is not ended by destination exhaustion, and the enumerator is not even parked where the first wake fix looks [source + runtime]

* The rest clears only on admission, selectable-exit-count drop, or network
  change (`discovery_retry.go:79-112`, `outcome.go:437-445`). Destination
  exhaustion changes none of those: refused exits are not warned
  (`dialStarved` needs 3 strikes over 2 destination IPs and no connect
  success in 60 s, `:16830-16854`; the fixture's old providers keep
  succeeding elsewhere), so `noteDiscoveryClientCount` sees no drop.
  The brief's items 1-3 are correct.
* During the rest the enumerator is blocked on `generatorNotify`, not inside
  `waitDiscoveryExpandDeferral` (it only reaches that wait on its next loop
  iteration, which needs a generator notify). That is why `wait_enter` is 0
  in both arms **[runtime]** and why the uncommitted first-wake diff (which
  wakes a producer parked in `waitDiscoveryExpandDeferral` via
  `resizeMonitor`) cannot engage here. I agree with the brief's conclusion
  that this fix is irrelevant to the exercised path; it is still a correct
  fix for the case it targets.

### RC3: recovery after admission is coupled to the application's SYN backoff, which the discovery delay has already pushed into its long phase [source]

With re-race on, the signal is swallowed and the app retransmit drives
recovery (`:9254-9256`). Nothing in the multi client re-offers a stuck SYN
when a new exit is admitted (`resizeMonitor` only wakes senders that have *no*
offer at all, `:7919-7929`). Every second of discovery delay therefore also
costs up to one RTO of alignment, and the RTO doubles while waiting.

### Where I agree and disagree with the brief's hypotheses

* Items 1-6 of "Relevant existing behavior": agree, all source-verified.
* "Neither arm entered that wait": agree (see RC2).
* The current candidate (one coalesced wake during a live acquisition, gated
  on `dialCandidatesExhausted`): the brief's worry that the wake "may arrive
  too early" is, in the natural workload, a **certainty**, not a risk.
  `MultiRaceClientCount` is 0 (`:355`), so a cold SYN races *every*
  selectable exit at once (`multiRaceClientCount` `:7285-7295`); all four
  refusals arrive within milliseconds of the first one; the late provider is
  only *constructed* after the first SYN reaches an old provider
  (`buildLate` waits on `state.navigationDial`) and is visible ~0.3 s later
  (`late_relationship_public` 11.107 s -> `late_named_api_visible` 11.409 s).
  The single credit is spent by the fourth refusal, before anything is
  listable, and `firstFromExit` is false on every later retransmit of the
  same attempt (`dialFailureStanding` `:10548-10551`), so the demand is never
  raised again. **[source + runtime]** The deterministic control passes
  precisely because its fifth provider exists before the SYN.
* The attempt stamp: agree with both over-invalidations. `resetSynGenerationWithLock`
  is reached from source-order completion replay
  (`ip_remote_multi_client_group_completion.go:650`, `:989`), i.e. after the
  send path already recorded the refusals, which explains the race-build
  `owned=4` failure (the stamp moved under the credit). The correction's bump
  sites (new ISN in `noteConnectAttempt`, explicit owner resets, and
  `forgetDialFailedClient` only when the removed exit was in the record) are
  the right ones *if* a stamp is kept. My recommendation (section 5) removes
  the need for one.

## 4. Additional failure modes and contributing factors the brief does not cover

**F1. Silent destinations never produce the candidate's demand signal.**
In a race of two or more candidates nothing commits on silence
(`dialProbeAwaitsResponse` `:10104-10109`, `:7758-7774`), the 2 s
`scheduleCompleteRace` only sets `responseWindowElapsed` (`:9336-9341`), and
the send-path inference requires a *bound* client (`:7377-7380`). So for a
silently dropped SYN there is no `clientDialFailure`, no strike, no
`dialFailedClients`, no service-failure entry; `dialCandidatesExhausted` can
never be true. Only a race of one (single under-cap min-tier exit) takes the
3 s inference path. Any demand signal built on the refusal record is blind to
the field case the brief itself flags (item 6). **[source]**

**F2. On the mobile profile there is normally no vacancy at all.**
`applyMobileLowMemoryMultiClientSettingsForPlatform` sets quality
min=max=hardmax=5 at 32 MiB (`sdk/mobile_memory_policy.go:544-551`,
`mobileQualityWindowSize=4` scaled by 32/24, `:237`, `:292-298`),
`StandingReserve=false` and `StrictWindowSizeHardMax=true` (`:535-536`). A
full window never expands; a warned owner that still carries flows blocks its
own replacement (`strictWindowAdmissionAllowed` `:13130`). When a destination
refuses all five exits on a phone, discovery latency is irrelevant: the flow
retransmits into a cohort that keeps refusing (the filter's oldest-first
fallback, `ip_remote_multi_client_service_failure.go:154-164`) until the OS
connect timeout, and the refusal that would let the browser fail fast or try
its next address is swallowed (`:9254-9256`). The reproduction cannot show
this because it has a vacancy by construction. **[source]**

**F3. The silence inference writes 10-minute negative memory from one slow
SYN-ACK.** An inferred failure (`signaled=false`) is recorded exactly like a
provider signal in `recordDestinationServiceFailureWithLock` (`:9199`) with
`destinationServiceFailureTtl = 10 min` (`service_failure.go:15`), keyed by
(exit, dst ip, dst port). `PACKETFLOW-STALL.md` measured a 3.264 s SYN-ACK
delay from carrier head-of-line under the same cell-edge profile; anything
over `inferredDialFailureTimeout = 3 s` (`:10052`) on a race of one strikes
the exit and removes that (exit, origin) pair from selection for 10 minutes
unless that same exit later connects to that same origin. With 4 origins and
5 exits, sustained rapid browsing under HOL can shrink each origin's selectable
set, concentrating load on fewer exits and feeding back into more HOL. This is
a **[hypothesis]**; a discriminator is in section 6.

**F4. Race-width asymmetry.** Whether a slow destination is handled by the 3 s
inference (race of one: unbind, strike, 10-minute memory) or by nothing at all
(race of several: retained race, re-sent each retransmit, no strike) depends
on how many min-tier exits are under `MaxFlowsPerExit` at that instant
(`raceCandidatesFrom` `:3707-3750`). Same destination behavior, two very
different outcomes; a source of run-to-run variance in any browsing fixture.
**[source]**

**F5. A single dead candidate pins a pass for up to 75 s.** With
`requestCount` 1-2, a candidate that is listed but never answers holds the
pass for acquisition + setup + ping (15 + 30 + 30 s, `:13339-13345`,
`:232-233`), during which no new listing happens and no other candidate is
evaluated. Not exercised by the reproduction, but the same "deaf pass"
property as RC1. **[source]**

**F6. Unexplained removals one resize interval after the late admission.**
OLD removed provider 0 at 40.857 s (admitted the fifth at 26.890 s); FIX
removed providers 0 and 1 at 45.334 s (fifth admitted at 31.795 s). Both are
~14 s after the admission. The trace has no removal reason and the SDK log is
not in `stdout.jsonl`. Removing a flow-carrying old provider is exactly what
the brief forbids a fix from doing, and it would cost the pages on it a
reconnect; it should be explained before any arm is compared on secondary
latencies. **[runtime; cause unknown]**

**F7. Signal-path timing is unmeasured but not implicated.** Provider
`receivePacket(..., receiveRecoveryModeRegenerableControl)` (`ip.go:5570`) can
wait on Transfer admission under receive pressure; the device intercept runs
on the channel's receive goroutine (`:19346-19365`) behind any blocked TUN
injection; `clientDialFailure` takes the parent lock then the flow lock
(`:9161-9217`). None of these is needed to explain the 14-18 s, and the
fixture's sampled receive pressure was zero (`BROWSING.json`), so I treat them
as instrumentation targets, not causes.

## 5. Fix proposals

### Design principle

A listing (`NextDestinations`) is a platform *read*; a mint
(`NewClientArgs`) is a platform *write*, and the 2026-10-21 outage was a mint
storm. Minting only happens for destinations the listing actually returns and
only inside `expand`. So the right lever is **bounded, demand-gated
re-listing** while the window has a vacancy, leaving the mint/evaluation
budget, the unproductive-pass backoff, the platform error/rate-limit backoffs
and every flow untouched. The demand must be **level-triggered** (re-asserted
while a connect attempt remains unanswered), not a one-shot credit, because
the signal that a provider is now listable does not exist on the device.

### P0 (recommended): level-triggered "stuck connect attempt" demand

Signal. On the send path, at each TCP SYN of an unestablished flow
(`dialProbePacket && Protocol==Tcp`, right after `noteConnectAttempt`
`:7354-7356`), compute under the flow lock whether the attempt is stuck:

* refused: `dialFailedClients` covers every currently offered,
  family-compatible exit of the window (the candidate's
  `dialCandidatesExhausted`, which is correct for this case); or
* silent: no inbound for this attempt and the attempt age (from
  `noteConnectAttempt`'s first SYN; the record already exists:
  `dialFailedSynSeen/dialFailedSyn`) is at least `inferredDialFailureTimeout`,
  regardless of race width.

Because the check runs on every retransmit (1, 2, 4, 8 s), a demand that was
raised before the provider was listable is simply raised again at the next
SYN; no stamp, no deferred observation, no re-validation at a later
linearization point. A spurious listing is harmless; only its *rate* needs
bounding.

Effect on the window (`multiClientWindow`, under `stateLock`):

1. If an expansion pass is acquiring (the candidate's
   `beginDiscoveryAcquisition/endDiscoveryAcquisition` bracket around the
   request loop is the right marker): `generatorMonitor.NotifyAll()`, subject
   to a per-window pacing of demand listings within one pass (e.g. at most 3
   extra listings, spaced 1 s, 2 s, 4 s from the first demand; the enumerator
   enforces the spacing so concurrent raisers coalesce). The enumerator
   already re-lists on any generator notify, so no change to its loop is
   required beyond honoring the spacing.
2. Else if the window is resting (`discoveryRetry.notBefore` in the future)
   and its last pass had a vacancy: end the rest **once per rest period**
   (`notBefore = now`, `resizeMonitor.NotifyAll()`; a `restCredit` flag reset
   in `noteDiscoveryExpandUnproductive`). The next unproductive pass still
   increments `failureCount`, so the backoff keeps doubling; worst case is
   twice the paced pass rate, never a busy loop.
3. Else (no pass, no rest: the window is full or satisfied): do nothing.
   This is the mobile steady state (F2) and must stay quiet.

Guards: TCP only at first; skip fixed-destination windows (their own pacing);
never wake `waitWindowRetry` (generator notify does not reach it, `outcome.go:411-426`,
so platform error and rate-limit backoffs are untouched); never mint from the
send path; respect `StrictWindowSizeHardMax` (no demand when
`len(clients) >= hardMax`); a settings knob (`DiscoveryDemandListingsPerPass`,
0 = off) for rollout.

Cost bound. Per window: at most `1 + 3` listings per 15 s pass while a flow is
stuck and a vacancy exists, and at most one early rest end per rest period.
No additional mints unless the listing returns a new destination, in which
case the existing pass budget (`multiClientExpandStepMax`, `:12959`) applies.
Hosted proxies normally have full windows, so the demand path is quiet there;
during a platform outage (vacancies everywhere) the extra load is bounded to
+3 reads per pass per window with a stuck flow.

Expected effect on the reproduction: visible at ~tap+0.5 s; the SYN
retransmit at ~tap+1.2 s raises demand; listing ~1.3 s; args ~1.7 s;
evaluation ~2.4 s; the SYN at ~tap+3.2 s races the new exit alone
(`filterDestinationServiceFailures` returns only the unfailed exit,
`service_failure.go:154-156`); first byte ~3.5-4 s. That passes the 5 s gate
but with little margin, and the margin is set by RC3 (SYN alignment), not by
discovery; see P3 and the measurement note in section 6.

Why not the candidate as-is (P2): it is the one-shot form of this design and
cannot pass the natural workload (section 3), and its stamp couples attempt
identity to collapse replay. Keeping its acquisition bracket and
`dialCandidatesExhausted`, dropping the credit and the stamp, and adding the
silent predicate converts it into P0 with little new code.

### P1 (simplest alternative, demand-agnostic): bounded empty-listing re-poll inside a live pass

Restore `WindowEnumerateEmptyTimeout` (e.g. 5 s) but only while an expansion
pass is acquiring and the last listing was empty. In the reproduction the
listing at ~11.7 s would have found the provider (admitted ~12.5 s). Cost: up
to 3 listings per pass for every short window, stuck flow or not, which is
exactly the poll `56648d5f` removed; it also does nothing during a rest.
Acceptable as a floor under P0 if the operator prefers a mechanism that does
not read flow state; not sufficient alone.

### P3 (separate decision, not part of this fix): decouple recovery from the app's SYN backoff

Two options, both outside the discovery subsystem:

* Fail fast when nothing can change (F2): if a signaled refusal arrives for an
  attempt that every selectable exit has refused and the window cannot
  acquire (full, or strict hard max saturated), forward the signal to the app
  as the `rerace=false` branch already does (`:9266-9275`) so the browser
  errors or tries its next address instead of spinning for the OS timeout.
  Risk: a visible error where a 10-20 s wait might have succeeded if a slot
  freed; on a full mobile window no slot frees without shedding an owner,
  which the brief forbids. Needs product sign-off and its own deterministic
  test.
* Proactive re-offer of a stuck SYN on admission. Would remove the RTO
  alignment cost entirely but requires synthesizing or retaining the SYN and
  touches the collapse/duplicate-suppression contract. Not recommended now;
  measure the alignment cost first.

### What not to do

Do not end an expansion pass early on an empty listing (it would make the
backoff reach its 2 min ceiling faster) and do not clear the backoff on
destination exhaustion without a per-rest credit (that restores the storm).
Do not change `resetSynGenerationWithLock`, `noteConnectAttempt` reset
semantics or `forgetDialFailedClient` for discovery's sake; P0 does not need
them.

## 6. Verification

### Deterministic tests (synctest, reusing `discoveryRetryTestWindow`, `listingCountDiscoveryGenerator`, `newReraceHarness`)

| test | setup | expected OLD | expected P0 |
| --- | --- | --- | --- |
| T1 live-pass demand | target 5, 4 exits, pass acquiring with empty listing; at +5 s virtual the generator gains a destination; real `SendPacket` SYN, all four refuse | one listing until +15 s; admission after the rest | second listing at the next SYN retransmit after the destination appears, admission before +15 s, no virtual advance to the pass deadline |
| T2 rest credit | unproductive pass, rest armed; provider appears during the rest; stuck SYNs continue | listing at `notBefore` | one early rest end; a second stuck SYN in the same rest does not end it again; the following unproductive pass sets `failureCount=2` |
| T3 silent cohort | four exits never answer the destination (no ICMP) | no listing, no strike | demand after attempt age >= 3 s; `DialFailuresIntercepted == 0`, `dialStarved()` false on all four, `destinationServiceFailures` empty |
| T4 coalescing | 8 stuck flows across 2 destinations in one pass | n/a | listings per pass <= 1 + configured cap; one early rest end total |
| T5 no vacancy | 5/5, or 4 healthy + 1 warned owner with flows under strict hard max | n/a | no listing, no rest change |
| T6 platform backoff | generator returns an error; stuck SYNs | n/a | `waitWindowRetry` not woken; listing count unchanged until the backoff ends |
| T7 custody | the private control's four healthy siblings plus the held CSS tuple | n/a | owners unchanged; flows unbound only for the refused destination |
| T8 fixed window | `FixedDestinationSize` generator | n/a | no demand path |
| T9 identity | `TestTcpGroupRecoverySameCohortResetSurvivesWinnerPromotion`, unwritten-ABA and advancing-ACK barriers | pass | unchanged, pass |

Run normal and `-race`; T1 is the root that must be red on OLD without any
fixture timer aging or generator poke, matching the brief's "no fixture wake"
requirement.

### Instrumentation for the 100-page workload

Add trace phases on the existing discovery trace clock: `demand_raised`
(flow key, reason refused|silent, attempt age, retransmit ordinal),
`demand_listing`, `rest_ended_by_demand`, `expand_pass_begin/end`
(the current trace cannot show when a pass starts; the model in section 2
had to infer it). Record the application-side SYN send times for page 1 and
the admission time, so the first-byte latency can be split into discovery
delay and RTO alignment; without that split a 4.9 s pass or a 7.5 s failure
would be attributed to the wrong component. Capture `event=warn` and
`event=removal` lines (`:12265-12273`, `logWarnTransition` `:16628-16659`) to
answer F6. Keep the OLD arm's `TRACE.json` as the regression baseline:
listing count between `late_named_api_visible` and `admitted` must go from 0
to >= 1 inside the live pass.

### Discriminators for the additional failure modes

* F1/F3 (silent and inferred): an arm where the old providers drop the SYN
  instead of refusing. OLD prediction: identical 14-25 s discovery shape, zero
  strikes, zero `DialFailuresIntercepted`; with the candidate's refusal-only
  predicate, no demand at all.
* F3 (inferred poisoning in ordinary browsing): on the canonical 100-page
  run (no refusals) report `FlowsReraced - DialFailuresIntercepted`
  (`reliability_metrics.go:641-642`) and the final size of
  `destinationServiceFailures`. If nonzero, run an arm where inferred
  failures are not recorded as service failures (or get a 30 s TTL) and
  compare per-origin exit spread.
* F2 (no vacancy): the same workload with five initial quality exits, no late
  provider, strict hard max. OLD prediction: page 1 stalls until the app's
  connect timeout with `FlowsReraced` growing once per retransmit; this is
  the arm that decides whether P3's fail-fast is needed for the phone.

## 7. Answers to the brief's questions

1. **Earliest safe recovery.** Safe for flows: any time (a listing touches no
   flow). Safe for the platform: bounded reads. Useful: the first listing
   after visibility. Waking a live acquisition is necessary but not
   sufficient; at `failureCount >= 2` the rest is longer than the pass, so a
   bounded (once-per-rest) early end is required too.
2. **One-shot wake vs. visibility.** It predictably precedes visibility in
   the natural workload (all refusals land within milliseconds of the first;
   construction starts after the first). Demand must survive an empty listing
   by being re-asserted from the live attempt at each SYN retransmit, with
   per-pass and per-rest caps; it must never mint or call the API itself.
3. **Attempt identity.** The send path owns it: `noteConnectAttempt` (new
   ISN starts, same ISN retains, `:10631-10647`) and explicit owner resets.
   `resetSynGenerationWithLock` is wire-generation bookkeeping for collapse
   and is replayed from completion (`group_completion.go:650`, `:989`); it
   must not define the attempt. With a level-triggered demand no stamp is
   needed; if one is kept, the correction's bump sites are the right ones.
4. **Delays in the signal path.** None evidenced; RC1-RC3 account for the
   measurement without any. Instrument provider `dial failed` ->
   device intercept -> `clientDialFailure` return per flow before looking
   further.
5. **A more plausible repeated-browsing bottleneck.** The reproduction
   requires provider refusals, which the iOS session has not shown. The
   candidates I would test first, preserving the 100 pages, overlaps,
   custody and timers: F3 (inferred 10-minute negative memory under carrier
   HOL) and F2 (full window, refusal or silence, nothing to discover), with
   the discriminators above.

## 8. Open questions

* Q1. Why were providers removed one resize interval after the late
  admission in both arms (F6)? Verdict class, flows carried, and whether
  page 2-5 latencies include those reconnects.
* Q2. What `EvaluationPoolMultiple` and `requestCount` actually were in the
  fixture (host `memoryScale` vs. 32 MiB profile), i.e. whether each pass
  listed once or twice. The trace's 3 setup-stage `discovery_call`s suggest
  two, but 14 setup events were dropped.
* Q3. Is a per-window demand read budget of 3-4 listings per pass acceptable
  to the platform team for hosted proxies during an outage (every window has
  a vacancy then)? If not, the cap can be 1 per pass plus the rest credit,
  which still covers the reproduction.
* Q4. Does the iOS Network Extension's SYN retransmit schedule (1 s steps
  early, exponential later) make RC3 negligible on the phone, or does the
  hosted-fixture's 8 s RTO cap overstate it? This decides whether P3 is worth
  anything.
* Q5. Should the silence inference stop writing `destinationServiceFailures`
  (F3), independent of this stall? It is a one-line change in
  `clientDialFailure` (`:9199`, gate on `signaled`) with its own test.
* Q6. The fixture's `late_named_api_visible` probes the API by client id;
  the device lists by rank with `excludeDestinations`. Is there any ranking
  or exclusion state (`runtimeExcludeClientIds`,
  `ip_remote_multi_client_api.go:637-680`) that could make a provider visible
  by id but absent from the ranked listing on Main? Not an issue in the
  fixture (the 25.857 s listing returned it), but relevant to the field.
