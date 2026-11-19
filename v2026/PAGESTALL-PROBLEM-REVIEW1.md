# Page-navigation stall: independent review 1

Date: 2026-10-23. Reviewer worked independently of any other review of this brief.

Inputs examined:

- `PAGESTALL-PROBLEM.md`, plus the relevant parts of `PACKETFLOW-STALL.md` and `MEMSTEADY.md`.
- The connect working tree: HEAD `5485774e` plus the uncommitted discovery-retry wake change (the "first wake fix").
- The qualified C3 pair under `.../native100-verdict-c3-pair.PREP.gf0f57bm/`: `NATIVE100.json`, `BROWSING.json`, `TRACE.json`, `PHASE.json` and `stdout.jsonl` for both arms, and the staged fixture sources in `staged/OLD`.
- The candidate helper `/private/tmp/urnetwork-connect-discovery-attempt-stamp.PREP.f7jferjk/ip_remote_multi_client_discovery_demand.go`.
- gVisor (`../gvisor`) and the SDK (`829f2a92`).
- One new experiment (section 2.3). It ran entirely in a scratch module. No repository file other than this one was created or modified.

Evidence tags used throughout:

- **[S]** means source fact. Paths are relative to `connect/` unless prefixed.
- **[R]** means runtime fact, from the qualified artifacts or my experiment.
- **[H]** means inference or hypothesis, stated together with how to falsify it.

---

## 0. Summary

1. **The page-1 TTFB is discovery latency quantized by the application's SYN retransmission schedule.** It is not discovery latency alone.
   - With `DialFailureRerace` on, the SDK swallows the provider's refusal ICMP and keeps no copy of the SYN. Native ingress offers each packet once.
   - After an exit that can serve the destination is admitted, nothing happens until the app sends its next SYN.
   - The fixture's app TUN runs a two-attempt dial race with a 2 s stagger over gVisor's 1 s doubling backoff. It emits SYNs at nav +0, 1, 2, 3, 3, 5, 7, 9, 15, 17, 31, 33 s **[R, measured]**.
   - Page 1 connected at nav+17.243 s in OLD. That is attempt B's SYN at 17.006 s plus one RTT.
   - Page 1 connected at nav+31.305 s in FIX. That is attempt A's SYN at 31.006 s plus one RTT.
   - OLD's admission landed just before the 17 s SYN. FIX's admission came about 4.7 s later and fell into the 17→31 s gap.
   - **The 14 s TTFB difference between the arms is about 4.7 s of admission jitter, amplified by quantization.** It says nothing about the wake change.

2. **The 14.45 s (OLD) and about 18.2 s (FIX) gap between visibility and listing is not the 15 s resize cadence.** It has two parts:
   - The rest of an in-flight expand pass. That pass had already listed once (empty, before the provider existed), then waited out `WindowExpandTimeout` (15 s) without listing again.
   - The first backoff after an unproductive pass, jittered in [5, 10) s.

   In both arms the next pass started exactly 15.000 s after the productive listing. That confirms an expand pass occupies the resize loop for its whole acquisition window.

3. **The SDK already had a working exit for the stalled destination and never tried it.**
   - The fixture's speed provider (index 5) does not refuse: `rejectNavigationDial` refuses index < 4 only. It was ready and installed before page 1, and carried 0 dials over the whole run **[R]**.
   - Port 443 selects `[Quality, Speed]`. But `coalesceOrderedClients` returns the quality window's known-failed exits, because `filterDestinationServiceFailures` never returns an empty list when it has two or more candidates.
   - Spilling over to the speed window would have connected page 1 on the nav+1 s SYN.
   - The fixture's qualification currently forbids that recovery: `heldAtNextHtml` requires `page1Provider == 4`.

4. **The in-pass one-shot wake candidate cannot meet the gate in the full workload.**
   - The first refusal (about nav+0.25 s) happens before the late provider is even constructed (nav+0.75 to 0.94 s). Named-API visibility comes later still (about nav+1.5 to 2.0 s). The single credit is therefore always spent on an empty listing.
   - The wake does nothing while the window rests in backoff, and nothing when the window has no vacancy.
   - Even a perfect listing at visibility gives admission at about nav+2.5 to 3.5 s. That is a coin flip between the nav+3 SYN (TTFB about 3.6 s, pass) and the nav+5 SYN (about 5.6 s, fail).

5. **Recommended fix:**
   - **R2 (delivery).** Keep a copy of the latest SYN of an unestablished flow that has exhausted its exits. The copy is tiny and the set is bounded. Re-offer it as soon as the offer gains an exit outside the flow's failure set.
   - **R3 (acquisition).** Add a bounded, re-armable listing schedule driven by destination demand:
     - Key it on the destination service, not on per-flow SYN-attempt identity.
     - Allow it during an acquisition pass and during the unproductive backoff.
     - Cost is listing only until a candidate appears.
     - Never advance the backoff exponent.
   - **R1 (selection, independent and cheap).** Spill over to another window's untried exit before re-racing known-failed exits.
   - **R4.** Decide explicitly what to do when the window has no vacancy, which is probably the common field case.

6. **Further findings:**
   - Idle old exits were removed after the stall in both arms. This is consistent with signaled refusals feeding the `no-receive-syn` verdict **[H]**.
   - Silent cold races with several candidates never run the silence inference.
   - `expand` keeps the resize loop busy for about 14 s after its admit budget is spent.
   - `destinationServiceFailures` keys keep removed channels reachable for up to 10 min.

---

## 1. The problem, restated precisely

**Field symptom.** On iOS over Wi-Fi, quick successive navigations are fast at first. Later, new links pause for roughly 5–15 s. The cause has not been attributed.

**Fixture.** `TestPrivateFullTunH1Sdk32LateQualityAdmissionNatural100Pages`, from the staged sources in `staged/OLD/` **[S]**:

1. Page 0's CSS (`/asset/0/0`) is held mid-body on its original tuple and owner.
2. At page 1's tap, `beforePage` (`private_sdk_discovery_native100_test.go:199-255`) does four things:
   - waits for the CSS prefix, suffix write, held read and JS completion;
   - sets `rejectNew` (line 243);
   - arms the trace, so its stage becomes `composed`;
   - closes idle connections and starts `buildLate` (line 249).
3. Page 1's HTML dial goes to `203.0.113.11:443` (`sdk_six_provider_browsing_test.go:154-156`). It runs through the app gVisor TUN with production dial settings: `DialRace` 2, stagger 2 s, dial timeout raised to 60 s (`tun.go:127-129`; `PACKETFLOW-STALL.md`, "Full-fixture dial race count 2, stagger 2 seconds, route-readiness dial allowance 60 seconds").
4. Quality providers 0–3 fail any new dial to `203.0.113.11:*` with a generic error (`private_sdk_discovery_admission_test.go:136-142`, `index < 4`). The provider sends a capacity-class ICMP (`ip.go:5537-5577`, `ip.go:11102-11121`). **The speed provider (5) does not refuse.**
5. The first refused dial releases `buildLate` (`:262-302`). It constructs provider 4, checks the relationship, and confirms visibility with a FindProviders2 lookup by `ClientId`.
6. Page 1 qualifies only if its HTML is served by provider 4 (`:441`, `page1Provider == 4`), with the CSS still held, within a 5 s first byte.

**Failing condition.** For about 17–31 s the SDK routes every page-1 SYN only to exits that refuse it. Two exits that would accept it are left unused:

- speed provider 5, which is live throughout and never tried;
- provider 4, once admitted, but only for the app's next SYN.

**TTFB model.** TTFB ≈ (first app SYN at or after a usable exit becomes selectable) + RTT (about 0.25–0.3 s) + TLS (about 0.16 s) + response (about 0.15–0.26 s).

---

## 2. Reconstructed timeline

### 2.1 Clock anchors [R]

- **Nav-1 start (wall clock).** `BROWSING.json` `first_slow_or_error.at_ns` is captured in `finishNavigation` when page 1's HTML completes. Subtracting `page1_original_complete_ns` gives nav-1 start:
  - OLD: 13:46:39.201 − 17.545 = **13:46:21.656**
  - FIX: 13:50:34.818 − 31.722 = **13:50:03.096**
- **Late provider construction.** The only provider construction during the run appears in server stdout as `[family]dial tag=platform` plus a new network client:
  - OLD at 13:46:22.408 (+0.75 s after nav start)
  - FIX at 13:50:04.038 (+0.94 s)
- **Trace clock.** The trace origin is the first trace event. Mapping it through the late-provider construction (to `late_relationship_public`) and through p4's transport dial (to `setup_started`) puts nav-1 start at:
  - about 9.9–10.0 s on the OLD trace clock;
  - about 10.0–10.15 s on the FIX trace clock.

  Uncertainty is about ±0.2 s. None of the conclusions depend on it.

### 2.2 Page-1 timeline (seconds after nav-1 start unless marked)

| Event | OLD | FIX | Evidence |
| --- | ---: | ---: | --- |
| First SYN (attempt A) refused by quality 0–3 | ~0.25 | ~0.25 | [H] one RTT |
| Late provider construction starts | 0.75 | 0.94 | [R] stdout |
| `late_relationship_public` | ~1.15 | ~1.3 | [R] trace |
| `late_named_api_visible` (by ClientId) | ~1.45 | ~2.0 | [R] trace |
| Quality pass-2 listing (empty), trace clock | ~3.0 | ~5.4 | [H] inferred, §2.4 |
| Pass-2 acquisition window ends, trace clock | ~18.0 | ~20.4 | [H] |
| Productive listing | ~15.9 (trace 25.857) | ~20.2 (trace 30.334) | [R] |
| Provider 4 admitted | ~16.96 (trace 26.890) | ~21.7 (trace 31.795) | [R] |
| Next app SYN after admission | 17.006 (attempt B) | 31.006 (attempt A) | [R] §2.3 |
| `connect_done` | 17.243 | 31.305 | [R] `BROWSING.json` phase 3 |
| First byte | 17.545 | 31.722 | [R] |

From productive listing to admission took 1.034 s in OLD and 1.462 s in FIX **[R]**:

| Step | OLD | FIX |
| --- | ---: | ---: |
| API listing | 0.284 s | 0.293 s |
| Mint | 0.112 s | 0.142 s |
| Setup + ping | 0.638 s | 1.027 s |

### 2.3 Experiment: the app-side SYN schedule [R]

A throwaway test ran in a scratch module, outside the repository, through a `go.work` that uses this connect tree. It:

- created `connect.CreateTun` with `DefaultTunSettings()` and `DialTimeout = 60s`;
- dialed `203.0.113.11:443`;
- recorded every pure SYN the stack emitted;
- never answered any of them.

| Attempt | SYN send times (s) |
| --- | --- |
| A (sport 21696, one ISN) | 0.000, 1.002, 3.002, 7.003, 15.005, 31.006 |
| B (sport 54125, its own ISN) | 2.001, 3.003, 5.003, 9.004, 17.006, 33.007 |

This matches gVisor's handshake timer. It starts at `InitialRTO = 1s` (`gvisor/.../tcp/connect.go:41`) and doubles on every reset (`connect.go:730-737`). It is created with the package `MaxRTO` (`connect.go:181`), so the fixture's 8 s `TCPMaxRTOOption` does not shorten the SYN schedule.

**Prediction (falsifiable).** Across repeats of the unchanged OLD arm, page-1 TTFB clusters near 15.5, 17.5, 31.5 and 33.5 s, and almost never falls between 18 and 30 s. Setting `DialRace = 1` on the same timings should move OLD's TTFB from 17.5 s to about 31.5 s.

### 2.4 Discovery timeline, with code [S]+[R]

1. **Vacancy.** The quality window's target equals its hard max, both 5: 4 × 32/24 at 32 MiB (`sdk/mobile_memory_policy.go:237`, `:292-299`, `:535-556`). Four initial providers leave one permanently open slot.
2. **Pass 1 (n = 5).**
   - It requests 4 candidates and admits p0–p3.
   - Each admission calls `resizeMonitor.NotifyAll()` (`ip_remote_multi_client.go:13156-13159`). That closes the pass's own `update` channel (`:12166`), so pass 2 starts as soon as pass 1 returns (`:12936-12941`).
3. **Pass 2 (n = 1).**
   - Its first request iteration calls `generatorMonitor.NotifyAll()` (`:13355`).
   - The enumerator lists once (`:12001`), gets an empty result, and parks on `generatorNotify` (`:12156-12160`).
   - `expand` waits for args until `requestEndTime = start + WindowExpandTimeout` (15 s; `:13339-13345`, `:13778`).
   - **The late provider appears during this wait, and nothing lists again.** The only other generator notifiers are the IPv6 probe (`ip_remote_multi_client_family.go:269`) and the outcome rebuild (`ip_remote_multi_client_outcome.go:658-660`). Neither fires in this fixture.
4. **Backoff.**
   - Pass 2 returns 0, so `noteDiscoveryExpandUnproductive` sets `notBefore = now + jitter(10s)`. The jitter is uniform in [5, 10) s (`ip_remote_multi_client_discovery_retry.go:42-75`; `ip_remote_multi_client_fixed_retry.go:42-72`; defaults at `ip_remote_multi_client.go:298-299`).
   - The next pass runs immediately, sees the deferral, skips `expand`, and sleeps until `notBefore` (`:12849-12855`, `:12925-12928`).
5. **Productive listing.**
   - At `notBefore` the pass lists at 25.857 (OLD) or 30.334 (FIX), finds p4 and admits it.
   - The pass has `requestCount` 2: a second listing appears 0.16 s after p4's args (OLD 26.41). It then waits for a surplus candidate until the acquisition deadline.
   - So the next pass starts exactly 15.000 s later, at 40.857 (OLD) and 45.334 (FIX).
6. **Corroboration from trace totals.**
   - Setup stage: exactly 3 quality listings in each arm. These are the enumerator's start, its coalesced re-list after the four offers, and pass 2.
   - Composed stage: no listing before the productive one.
   - The setup stage dropped 14 events, so pass 2's own events are not retained. Its timing is inferred.
7. **Decomposition of the gap.**
   - OLD 14.45 s ≈ (18.0 − 11.41 ≈ 6.6 s left in pass 2) + 7.86 s of jitter.
   - FIX 18.2 s ≈ (20.4 − 12.12 ≈ 8.3 s) + about 9.9 s of jitter.
   - Pass 1 likely ended about 2.4 s later in FIX: a slow fourth setup. That setup is among the dropped setup-stage events.
   - **None of this involves the wake change.**

The bound grows with the unproductive count: [5, 10), [10, 20), [20, 40), [40, 80), then [60, 120) s at the 2 min cap. Each pass with an empty listing also costs 15 s.

- A window that has held a vacancy against a sparse pool for a few minutes lists only once every 75–135 s.
- A window with no vacancy never lists at all.

---

## 3. Root-cause analysis

### 3.1 Delivery: the app's SYN backoff schedule quantizes TTFB

**[S] Rerace swallows the ICMP.** With `DialFailureRerace = true` (the default, `ip_remote_multi_client.go:262`), `clientDialFailure` unbinds the flow and drops the ICMP (`:9249-9257`). Its stated premise is "the app's retransmit drives recovery... ~1s instead of 3-63s".

**[S] That premise fails when every selectable exit has failed the destination service.**

- `filterDestinationServiceFailures` re-races the oldest failed exits (`ip_remote_multi_client_service_failure.go:119-165`).
- Each app SYN is therefore refused again.
- The swallowed ICMP leaves the app in exactly the exponential backoff that the provider-side comment warns about (`ip.go:5538-5543`: "17/33/65 seconds in Chromium").

**[S] Native ingress keeps nothing.**

- It makes one ready-only offer with timeout 0 and drops the packet on refusal (`ip_remote_multi_client.go:7326-7330`, `:7886-7894`; `ip_native_ingress.go:40-61`).
- This is the path the fixture bridge and mobile packed ingress use (`sdk/device_local.go:5371-5417`).
- No component retains the SYN, so none can re-offer it when a new exit is admitted.

**[R] The connect times match the SYN schedule.** Both arms' `connect_done` times sit one RTT after a SYN in the measured schedule (section 2.3).

**[H] Field relevance.** XNU's SYN backoff table is `{1,1,1,1,1,2,4,8,16,32,64,...}` over a base RTO of about 1 s. That gives SYNs at about 0, 1, 2, 3, 4, 5, 7, 11, 19 and 35 s.

- An admission at nav+5.5 s connects at 7 s.
- An admission at nav+8 s connects at 11 s.
- An admission at nav+12 s connects at 19 s.

That is the reported 5–15 s shape. Verify on device by counting same-ISN SYNs per tuple in the Network Extension.

### 3.2 Acquisition: timer-bound gates with no demand input [S]

- **(a) One listing per request iteration.** Otherwise the enumerator waits on `generatorNotify` (`:11968`, `:12156-12160`).
- **(b) A blind 15 s acquisition wait.** The pass sits on that single listing for the whole wait (`:13347-13782`).
- **(c) Unproductive backoff.** It is jittered and doubles to a [60, 120) s cap. Only four things clear it:
  - an admission;
  - a drop in owned-exit count (`noteDiscoveryClientCount`, `:12846-12848`);
  - a network change;
  - an epoch rebuild (`ip_remote_multi_client_outcome.go:440`).
- **(d) No demand signal.** Nothing on the dial-failure path informs the window. `clientDialFailure` only records three things: the per-flow cohort (`dialFailureStanding`, `:10521-10556`), the destination-service failure (`ip_remote_multi_client_service_failure.go:64-100`) and dial strikes (`:9224-9238`).
- **(e) No vacancy means no expansion.** With `StrictWindowSizeHardMax` (mobile), warned and quarantined exits still occupy slots (`strictWindowAdmissionAllowed`, `:13951-13964`).

### 3.3 Selection: the cross-window fallback is short-circuited [S]+[R]

- `selectWindowTypes` returns `[Quality, Speed]` for port 443 (`:3175-3176`).
- `coalesceOrderedClients` returns the first window whose filtered list is non-empty (`:7873-7875`).
- `filterDestinationServiceFailures` returns every failed candidate, oldest first, when none is available (`ip_remote_multi_client_service_failure.go:154-165`).
- So the speed window is consulted for 443 only when the quality window has no live client at all.

**[R] Speed provider 5 sat idle.**

- It was ready before page 1: `last_healthy` ready mask 47 includes bit 5.
- It was installed: `SpeedClientCount` 1.
- It carried 0 TCP dials and 0 body bytes for the entire run in both arms.

**Related wrinkle [S].** `filterDestinationServiceFailures` returns early when given one candidate or none (`:125`). A single-exit window is never filtered, so it can never be recognized as exhausted. Any per-window exhaustion test must compute this explicitly.

### 3.4 Agreement and disagreement with the brief

| Brief statement | Assessment |
| --- | --- |
| Gap "close to the default 15-second window resize cadence" | **Disagree on mechanism.** The gap is the remainder of `WindowExpandTimeout` (also 15 s) plus U[5, 10) s of backoff (section 2.4). The resize sleep did not gate the listing. |
| "Do not subtract these timestamps... to invent a SYN-to-admission timeline" | Agree that naive subtraction is wrong. But `at_ns`, the stdout wall clock and the measured SYN schedule pin the mapping to about ±0.2 s. The conclusions hold anywhere in that range. |
| Neither arm entered the deferral wait | **Agree, and it is structural.** The enumerator is woken only by `expand`, the IPv6 probe or a rebuild. `expand` runs only after the deferral has expired, so `waitDiscoveryExpandDeferral` returns at once. The wake change fixes a real but different path: IPv6 probe or count drop during a deferral. It is inert here. |
| "The larger FIX TTFB does not, by itself, prove that change caused a regression" | **Agree, more strongly.** Pass-1 duration (+~2.4 s) and jitter (+~2 s), quantized across the 17→31 s SYN gap, fully explain it. The pair carries no information about the change. |
| The refusal is not deliberately silent SYN loss | Agree. Note, though, that the SDK swallows the ICMP, so the app experiences silence anyway (section 3.1). |
| "Current experimental direction" (one in-pass wake) | **Disagree that it can qualify** (section 0, item 4). Its exhaustion test also looks only at the window that owns the failing client (candidate `requestDiscoveryForExhaustedAttempt`). It would ask discovery for help while the speed window holds an untried exit. |
| Two attempt-stamp over-invalidations | Agree with both. Recommendation: take per-flow attempt identity out of the discovery decision entirely (section 7, Q3). |
| "Increasing memory has not been demonstrated to solve this stall" | Agree. The stall is bound by timers. R2 costs a few KiB at most. |

---

## 4. Failure modes and contributing factors the brief does not cover

1. **Cross-window short-circuit (section 3.3).** A working exit went unused for the whole stall.
2. **No-vacancy exhaustion (section 3.2e).** The fixture creates a vacancy on purpose. In a well-populated field pool, the quality window is probably full most of the time. Then destination exhaustion triggers no discovery at all, and neither the brief's candidate nor waking an acquisition pass can help.
3. **Silent cold races [S].**
   - The silence inference needs a bound client (`:7377-7394`).
   - A TCP SYN race with several candidates never commits on silence (`dialProbeAwaitsResponse`, `:7758-7774`).
   - So a flow raced to several silent exits gets no strike, no service-failure record and no demand. Every app SYN goes back to the same silent set.
4. **Signaled refusals feed the `no-receive-syn` verdict [S]+[H].**
   - The intercepted ICMP deliberately does not count as receive traffic (`:9139-9148`).
   - `unansweredConnectSince` counts SYNs sent without a SYN-ACK (`:17538-17546`).
   - The verdict fires for an exit with no receive traffic in its stats window once the connect bar is reached: 10 s comparative, 30 s full (`:17755-17777`).
   - **[R]** Provider 0 was removed at trace 40.857 (OLD); providers 0 and 1 at 45.334 (FIX). Both came at the first classification after the 15 s productive pass. OLD re-minted and re-admitted p0 by 43.8 s.
   - The window logger is a no-op in the fixture, so the removal reason was not recorded.
   - **[H]** These are idle exits executed for one destination's refusals. That costs teardown plus identity churn, which conflicts with the brief's invariant 2 in spirit.
5. **The resize loop stays busy after `expand` spends its admit budget [S].**
   - The request loop runs to `requestCount` (`:13348`) whatever `admitted` is. Both arms spent about 14 s after admission inside the pass.
   - During that time there is no classification, rotation, collapse or new pass.
   - On a 32 MiB device `EvaluationPoolMultiple` probably scales to 1 (`MemoryScaledCount(2,1)`, `:474`). That removes this post-admission hold but not the blind empty-listing wait.
6. **`destinationServiceFailures` pins removed channels [S].**
   - Keys hold `*multiClientChannel` (`ip_remote_multi_client_service_failure.go:19-25`).
   - `removeClients` deliberately forgets removed channels from the per-flow cohort (`:5341-5351`), but nothing removes these keys. They live until the 10 min TTL or the 4096-entry cap.
   - That is a possible retention problem in MEMSTEADY terms. `memory_owner_census.go:278` already counts the entries.
7. **`removeClients` walks every flow under the parent lock [S] (`:5341-5351`).** This is relevant to Q4 under heavy browsing, though it is not the cause here.
8. **Two definitions of "visible" [H].**
   - The fixture's `late_named_api_visible` looks up FindProviders2 by `ClientId`.
   - The window lists with `BestAvailable` / quality plus exclusions.
   - Discovery-visibility may lag named visibility, and that lag is unmeasured. It decides when listing can first succeed.
9. **Measurement design.**
   - TTFB is multimodal for any discovery change, so one matched pair cannot rank candidates.
   - The setup stage's 48-event trace buffer dropped exactly the pass-2 events this analysis had to infer.
10. **FIX-arm differences the change does not explain [R].**
    - A speed-window listing at trace 30.0 s; OLD had no composed-stage speed listing.
    - `provider_timeout_resends` of 435 against 25 at the first-slow boundary.
    - Neither is needed to explain page 1. Both are worth a look for unrelated churn.

---

## 5. Fix options, trade-offs and recommendation

### R1. Cross-window spillover before re-racing failed exits

**Change.**

- In `coalesceOrderedClients`, record whether filtering left any untried candidate for each selected window type.
- If a window has none, try the next window type.
- Only if no window has an untried candidate, return the first window's oldest-failed list. That is today's behavior.
- Do not spill pinned flows (site or app pins) or fixed-window profiles.
- Use an explicit "untried" predicate, because the filter returns early for one candidate.

**Effect.** In the fixture, page 1 connects on the nav+1 s SYN through provider 5, for a TTFB of about 1.5 s. This needs a fixture variant (section 6). In the field it helps whenever the speed window holds an exit outside the failure set; at 32 MiB that is one exit.

**Trade-offs.**

- The exit is bulk-oriented, so the speed of interactive HTTPS may suffer.
- The page's HTML egresses from a different IP. No established connection moves.
- More load lands on a single speed exit.
- No API calls and no memory cost. This is the cheapest change with the largest effect in this fixture.

### R2. Re-offer a kept SYN when the offer changes (decouples delivery from app backoff)

**Design.**

- **When to keep a copy.** A pure TCP SYN (no payload, or only a small TFO payload under a cap) of an unbound flow with no inbound data is offered, and no window has an untried candidate for its destination service. This is the R1 fallback.
- **How to store it.**
  - Keep a private, non-pooled copy on the flow update.
  - Replace it on a same-ISN SYN. Drop it when `noteConnectAttempt` sees a new ISN.
  - Add the update to a bounded set on the parent: about 32 entries, FIFO eviction, age cap about 20 s.
- **When to resend.** On any window publication that adds a selectable client: `admitCandidate` (`:13146-13203`), and a warning or quarantine being cleared during resize.
  - A worker re-offers each kept SYN through the ordinary native-ingress path. The worker must not be the resize goroutine or a receive callback.
  - Re-validate under the flow lock first: same update, same ISN, `!receivedInbound`, `!sourceReset`, no egress RST/FIN seen, not done.
  - The new client must be outside the flow's service-failure set.
  - One resend per kept SYN per publication.
  - Also re-check when a refusal arrives for a kept flow while an untried exit already exists. That covers the case where collapse judged the resend redundant against a still-pending offer.
- **When to release.** When the flow is bound or raced, when a newer SYN replaces it, on expiry, and on retirement.

**Effect.** TTFB ≈ admission + RTT + TLS + response, for any app backoff schedule. In the fixture that is about admission + 0.55 s.

**Why it is safe.**

- The resend is byte-identical to an app retransmission of the same attempt.
- It passes through the same `noteConnectAttempt`, collapse and race gates.
- It keeps the same-ISN cohort and never reopens a reset cohort, because `sourceReset` and a new ISN both discard the kept copy.
- It touches no established connection.

**Risks.**

- Collapse may suppress the resend as covered. That is the same outcome an app retransmission would get, and the re-check on refusal mitigates it.
- The provider may see a duplicate SYN while it is still dialing. Providers already handle this, since apps retransmit during dials.
- A stale resend after the app has given up is answered by the app stack's RST. The age cap bounds this.
- The two dial-race tuples each get one resend. That gives two provider connections, of which the app closes the loser, as today when both attempts succeed.
- Memory is at most about 32 × 200 B, held outside the MessagePool.

### R3. Bounded listing driven by destination demand

**Key.** Window × (destination IP, port, protocol, IP family). Not per-flow attempt identity.

**Triggers.**

- `clientDialFailure`, whether signaled or inferred.
- Send-path selection, when R1 finds no untried exit in any window.

There are two independent triggers. If a signal is lost or late, demand is delayed by at most one app SYN interval.

**Re-validation.** Check against the live offer under the window lock when deciding. A stale observation can at worst cause one extra bounded listing. It can never admit or tear down anything.

**While demand is live** (TTL about 20 s, refreshed by new failures):

- Wake the resize loop. If an acquisition pass is waiting, also wake the enumerator.
- Allow a per-episode listing schedule: E0, +1, +2, +4, +8 s (at most 5 listings).
- Allow listings both during an acquisition pass and during the unproductive deferral.
  - The enumerator's `waitDiscoveryExpandDeferral` must honor the demand override, and the uncommitted `retryChanged` subscription can carry that wake.
  - The deferral itself must not be cleared, so the exponent is not reset.
- Mint only for listed candidates, through the ordinary path, bounded by `requestCount` and identity reuse.
- Cap mints per episode at about 2, so a provider that keeps failing evaluation cannot recreate the 2026-10-21 mint loop.
- Demand listings that return nothing do not advance `discoveryRetry.failureCount`. A demand admission clears it as today.

**Demand clears on:**

- a connect success to that service (hook the `clearDestinationServiceFailure` call sites at `:7623`, `:8686`, `:9039` and `:9361`);
- TTL expiry;
- a newly admitted exit also failing the service. In that case suppress the key for about 10 min: the origin is probably down for everyone.

**Bounds.**

- At most 1 concurrent episode per window.
- A token bucket of about 10 demand listings per minute.
- A process-wide budget and jitter, for hosted proxies with many windows.

**Effect in the fixture.**

- Listings at about nav+0.25 (empty), 1.25 (empty) and 2.25 s; the last finds p4.
- p4 is admitted at about nav+3.3 to 3.8 s.
- With R2, TTFB is about 3.9 to 4.4 s.
- Without R2, the next SYN is at nav+5 and TTFB is about 5.55 s, which fails.
- A denser start, at 0.5 s steps, would sometimes beat the nav+3 SYN without R2, but only for OLD-like visibility.

### R4. No vacancy (field-critical; needs a product decision)

| Option | Assessment |
| --- | --- |
| (a) Accept the gap and document it | Leaves the likely field case unfixed. |
| (b) R1 spillover only | Free, but limited to one speed exit at 32 MiB. |
| (c) Swap out a flowless exit, modelled on `selectFamilySwapVictim` (`ip_remote_multi_client_family.go:413`) and the strict-ceiling "remove idle first" path (`:12770-12781`) | Only exits carrying zero flows may be swapped. At most once per demand key per suppression period, and once per window per N minutes. Never touches a flow-carrying exit. |
| (d) Temporarily admit one exit above the strict hard cap | Rejected for the 32 MiB profile unless measured. |

**Recommendation.** (b) plus (c) behind a flag, after measuring how often field windows are full during stalls.

### R5. Hygiene

- Break `expand`'s request loop once `admitted == admitBudget`.
- Stop counting provider-signaled refusals as unanswered connects for `no-receive-syn`. Keep the dial strikes.
- Delete `destinationServiceFailures` entries in `removeClients`.

### Not recommended

- **Shortening `WindowResizeTimeout`, `WindowExpandTimeout` or `DiscoveryExpandRetryMin` globally.** That raises listing and mint load for every vacancy and repeats the hosted-proxy outage risk.
- **Clearing the unproductive backoff on demand.** It resets the exponent and amplifies load.
- **Forwarding the ICMP to the app when the destination is exhausted.**
  - gVisor aborts a connecting endpoint on ICMP (`gvisor/.../tcp/endpoint.go:3050-3066`). Linux does the same. The result is a fast-fail loop with a fresh ISN each time.
  - Darwin treats the ICMP as a soft error until the fourth retransmission.
  - Neither recovers faster than R2.
- **Raising caps or memory, tearing down flow-carrying exits, or extending the latency gate.**

### Recommended package

1. **R2 + R3** as the primary fix for the brief's goal (late admission without disturbing existing connections).
2. **R1** as an independent, cheap improvement, with its own fixture variant.
3. **The R4 decision** before claiming the field problem is solved.
4. **R5** as follow-ups.

### Budget table (fixture timings; admission = listing + 1.03 to 1.46 s, from section 2.2)

| Design | Admission (nav-relative) | First SYN reaching a usable exit | TTFB | Gate |
| --- | ---: | ---: | ---: | --- |
| OLD | ~16.96 | 17.0 (B) | 17.55 | fail |
| One in-pass wake (candidate) | unchanged | ≥ 17 | ≥ 17.5 | fail |
| Perfect listing at named visibility, no R2 | ~2.5 (OLD) / ~3.5 (FIX) | 3.0 / 5.0 | ~3.6 / ~5.6 | flaky |
| R3 schedule, no R2 | ~3.3–3.8 | 5.0 | ~5.6 | fail |
| R3 + R2 | ~3.3–3.8 | at admission | ~3.9–4.4 | pass |
| R1 | n/a (provider 5) | 1.0 (A1) | ~1.6 | pass (if the predicate accepts provider 5) |

---

## 6. Verification plan

### Deterministic controls (each fails on OLD)

**C1 (R1).**

- Setup: two windows. Four quality exits signal dial failure for D:443. The speed exit is healthy.
- Pass: the same-ISN retransmission is offered to the speed exit.
- OLD: it goes only to the four quality exits.
- Variants:
  - a pinned flow does not spill;
  - a fixed-window profile does not spill;
  - if the speed exit has also failed, selection falls back to the oldest failed quality exit;
  - a single-exit window is recognized as exhausted.

**C2 (R2).**

- Setup: send a public `SendNativePacketBatch` SYN. All exits signal failure. Send no further app packets, then admit a new client without advancing fake time.
- Pass: the new client receives the original SYN bytes exactly once.
- OLD: it receives nothing.
- Negative controls:
  - the app sends an RST before admission;
  - a new ISN arrives on the same tuple (only the new SYN may be resent);
  - inbound data has already arrived;
  - the age cap expires;
  - the kept-SYN set evicts at its cap;
  - a pending same-ISN offer suppresses the resend exactly as it would suppress an app retransmission;
  - two dial-race tuples each resend at most once.

**C3 (R3).**

- Setup: a window with a vacancy. The generator is empty at E0. A provider becomes listable at E0+1.5 s.
- Pass: a listing happens by E0+2 s and the provider is admitted, while `failureCount` stays unchanged.
- OLD: no listing until the pass ends plus backoff (at least 15 s).
- Bounds:
  - a provider that is never listable gives at most 5 listings, 0 mints and an unchanged exponent;
  - a provider that lists but always fails evaluation gives at most 2 mints per episode;
  - a universally failing destination is suppressed after the new exit also fails it;
  - with N windows, the process budget holds.

**C4 (in-pass, generalizing the brief's public control).** The candidate appears 1.5 s after the refusal. The brief already notes that its control had the provider present before the refusal, which hides the timing problem.

**C5 (no vacancy).**

- Setup: a full strict window with destination exhaustion.
- Pass: the documented behavior happens. Either nothing is admitted, or, with R4(c), exactly one flowless swap. Assert that no flow-carrying exit is touched.

**C6 (verdict).**

- Setup: an idle exit that receives only signaled refusals for longer than the comparative bar.
- Pass: today's behavior is asserted and documented. With R5, the exit is not executed.

**C7 (silent cold race).** Several silent candidates. Assert the inference or demand eventually reaches an untried exit.

### Full 100-page workload

**Fixture variants.**

- V1: the speed provider also refuses. This exercises R2 + R3 and keeps the provider-4 predicate.
- V2: the speed provider is healthy. This exercises R1. The predicate becomes "HTML served by any non-refusing exit, with CSS custody preserved".

**Metrics to report**, in addition to TTFB:

- `BestAvailable` visibility → listing. Add a probe to `buildLate` that polls the window's exact spec without minting.
- Listing → admission.
- Admission → first SYN offered to the new exit.
- Same-ISN SYN count per stalled connect.
- App-side SYN times, recorded in the bridge as in section 2.3.

**Statistics.** At least 10 paired runs per arm in alternating order. Report distributions.

**Tracing.** Log resize pass start and end, `expand` start and end with `requestCount` and `admitBudget`, and deferral `notBefore`. Alternatively, enlarge the setup-stage trace buffer.

### Experiments that would falsify this analysis

| ID | Experiment | Prediction |
| --- | --- | --- |
| E1 | OLD with `DialRace = 1` | TTFB about 31.5 s for the same admission time |
| E2 | Experimental `WindowExpandTimeout = 5s` | Gap shrinks by about 10 s; the productive pass is followed by the next one 5 s later |
| E3 | The candidate on the full workload with listing logs | An empty listing at E0, and an unchanged productive listing time |
| E4 | Enable the window logger in the fixture | `no-receive-syn` for p0's removal (OLD) |

### Field discriminator (Q5)

For each slow navigation, record:

- the stalled socket's same-ISN SYN count (Network Extension, per tuple);
- the SDK decision for each SYN: offered or refused natively, which exits it raced to, which signals came back;
- the window state: full or vacancy, deferral, pass active;
- admissions.

Classify each stall as one of:

- exhausted with a full window;
- exhausted with a vacancy;
- silent cold race;
- native refusal or backpressure leading to data-RTO backoff;
- other.

---

## 7. Answers to the brief's five questions

**Q1. What is the earliest safe recovery opportunity?**

For a new connection, it is the first moment an exit outside the destination's failure set is selectable *and* a live SYN for the attempt can be delivered to it. Today that opportunity is lost three ways:

- An existing untried exit in another window is ignored. R1 recovers it immediately.
- Delivery waits for the app's next SYN. R2 removes the wait.
- Acquisition is blind for 15 s and then backs off. R3 fixes this.

Waking an already-active acquisition is not enough. In this fixture the refusal always precedes the provider's existence. With a persistent vacancy, the window rests in backoff for 80–90 % of each cycle once the backoff has grown. With no vacancy there is no acquisition at all.

Yes, a bounded opportunity during the backoff rest is needed. It should be a demand schedule that never clears or advances the backoff, not a single wake.

**Q2. Can the one-shot wake predictably precede provider availability?**

Yes, and here deterministically: construction is triggered by the refusal and starts 0.5–0.7 s after it. Demand should survive an empty listing as a bounded, re-armable schedule (E0, +1, +2, +4, +8 s):

- listing-only until a candidate appears;
- mint-capped per episode;
- not counted toward the exponent;
- suppressed per destination after a fresh exit also fails it;
- under a process-wide budget.

Listing and minting need separate pacing. The 2026-10-21 outage was about identity creation and retirement, not reads.

**Q3. Which source boundary owns logical SYN-attempt identity?**

Two different questions are being treated as one.

- **For strike and adoption cohorts:** the owner is `noteConnectAttempt` (`:10631-10647`). It is the send-path entry, runs before the offer, and is keyed by ISN.
  - Its identity changes only on a new ISN for the tuple.
  - Same-ISN retransmissions and R2 resends keep it.
  - Collapse-generation resets (`resetSynGenerationWithLock`, `:10215-10237`, called from delayed group completion and candidate projection) must not change it.
  - Exit removal (`forgetDialFailedClient`, called for every flow on every removal at `:5341-5351`) changes cohort membership, not identity.
- **For discovery:** no per-flow identity is needed. Key demand on the destination service and re-validate against live window membership under the window lock. That removes the ABA, delayed-RST and replay hazards from the discovery decision entirely. The worst stale outcome is one bounded listing. The canonical TCP collapse contract stays untouched.

**Q4. Are receive callbacks, locks, Transfer admission waits or queued owners delaying signals or discovery?**

There is no evidence of that. The observed gaps are fully explained by timers (section 2.4), and listing → admission took 1.0–1.5 s in both arms.

The signal path has three stages:

1. The provider's `TcpSequence.Run` emits the signal through `receivePacket(..., receiveRecoveryModeRegenerableControl)`.
2. Provider→client Transfer carries it with ACK and retry. A reliable carrier does not remove this contract.
3. The client channel's intercept passes it to `clientDialFailure`, which takes the parent lock and then the flow lock.

Lateness in this path today delays only the per-flow re-race, which the app's SYN owns anyway. Under R3 the send path is a second demand owner, so a late signal costs at most one app SYN interval.

To rule out head-of-line delay behind bulk return data on the same exit, record three timestamps:

- the provider's ICMP build time;
- the time the client channel receives it;
- entry to and exit from `clientDialFailure`.

**Q5. Is a more plausible repeated-browsing bottleneck missing?**

Three candidates are more field-plausible than "vacancy plus sparse pool":

- a full window that exhausts one destination (section 3.2e);
- silent cold races (section 4, item 3);
- app TCP RTO backoff after native-ingress refusals. The fixture bridge refused 1,069 of 6,905 packets over the run.

The section 6 field discriminator separates these. It keeps the 100 pages, the overlaps, the body and cancellation checks, CSS custody and the original timers.

---

## 8. Open questions

1. During real stalls, how often is the quality window full rather than holding a vacancy? This decides whether R4 is the main field fix.
2. On Main, how far does `BestAvailable` discovery-visibility lag named visibility, or the provider's own registration?
3. What is the actual SYN schedule under an iOS Network Extension, including Safari's own connection racing? Measure same-ISN SYN gaps per tuple.
4. Should a refusal signaled by the provider ever count toward `no-receive-syn`? Which verdict actually removed p0, and p0 plus p1, in these runs?
5. Is the speed window an acceptable spillover for interactive HTTPS (product and egress-IP policy)? Is the fixture's provider-4-only predicate intended, or an accident of construction?
6. For hosted proxies: how many windows would raise demand at once for a popular dead destination? What process-wide budget keeps that below the 2026-10-21 load?
7. Does `TcpSequence`'s init loop absorb a resent duplicate SYN that arrives during an in-progress dial without a second dial? Confirm with a provider-side control before shipping R2.
8. Why did the FIX arm alone show a speed-window listing at 30.0 s, and 17 times more Transfer timeout resends at the first-slow boundary?
