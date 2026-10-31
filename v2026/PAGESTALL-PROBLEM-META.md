# Page-navigation stalls: independent meta-review

Date: 2026-10-23. Inputs: [the problem statement](PAGESTALL-PROBLEM.md), [review 1](PAGESTALL-PROBLEM-REVIEW1.md), [review 2](PAGESTALL-PROBLEM-REVIEW2.md), current Connect/SDK/gVisor source, and the retained qualified OLD/first-wake pair. Applicable ancestor `AGENTS.md` files were checked; none applied to this document. [CODESTYLE.md](CODESTYLE.md) was read. This review changed only this document and ran no tests, builds, devices, shared fixtures, or network jobs.

**The stall remains unresolved.** The strongest next experiment is bounded, persistent discovery demand within an already authorized acquisition, with an actual candidate/mint reservation and unchanged platform backoffs. That is a smaller first change than retaining and replaying packets. It is not yet a complete solution: demand arriving during rest, full ownership windows, and delay between admission and the application's next SYN remain distinct cases. If the unchanged navigation gate still fails, completing the original objective requires addressing the remaining measured cause, not declaring the narrower change sufficient because its unit tests pass.

Both reviews identify useful mechanisms, but neither establishes a passing fix. Review 1 is stronger on the extra application-retransmission delay and cross-window selection. Review 2 is stronger on keeping the first discovery experiment narrow and adding silent-race coverage. Its proposed renewable rest credit and several supporting factual claims are incorrect. Review 1's packet-retention design also needs more ownership and memory proof than byte-identical retransmission supplies.

Evidence labels below are **[R] retained runtime fact**, **[S] source fact**, **[D] deduction from those facts**, and **[H] hypothesis or untested proposal**. Existing tests cited here were inspected, not re-executed. A test reported as passing in another review is not a new result from this review.

## 1. What the evidence supports

The qualified pair is retained under:

`/Users/builder/Library/Caches/urnetwork-runall-owned/hosted-h1-current112.PREP.jxp9wXRI/full-correctness.9urtxbr6/native100-verdict-c3-pair.PREP.gf0f57bm/`

`NATIVE100.json`, `BROWSING.json`, `TRACE.json`, and `QUALIFICATION.json` agree on these results. **[R]**

| Observation | OLD | First wake |
| --- | ---: | ---: |
| Page 1 first byte | 17.545 s | 31.722 s |
| Required limit | 5 s | 5 s |
| Completed navigations / overlaps | 100 / 99 | 100 / 99 |
| HTML requests / asset attempts | 100 / 400 | 100 / 400 |
| Created / closed asset response bodies | 330 / 330 | 322 / 322 |
| Assets canceled before a response body | 70 | 78 |
| Held CSS bytes, original tuple and owner retained | 12,288; yes | 12,288; yes |
| Named visibility, discovery clock | 11.408621 s | 12.118460 s |
| Next quality discovery call, same clock | 25.856781 s | 30.333498 s |
| Admission, same clock | 26.890457 s | 31.795200 s |
| Named visibility to listing | 14.448160 s | 18.215038 s |
| Listing to admission | 1.033676 s | 1.461702 s |
| Dropped trace events: setup / composed | 14 / 5 | 14 / 58 |

Neither arm entered `waitDiscoveryExpandDeferral`'s actual wait: the wait-entry and wake counters are zero. The first wake change therefore did not exercise its intended transition. A `wait_exit` event does not contradict this: the helper can return without waiting. The pair demonstrates a stall and an ineffective intervention on this path; it does not establish that the intervention caused the larger FIX latency. **[R, D]**

The source supplies a credible explanation for the discovery gap. After an empty result, `randomEnumerateClientArgs` waits on `generatorMonitor`; an ordinary request in `expand` waits for args through its acquisition deadline. An unproductive expansion then schedules jittered exponential rest. Destination-local failure need not lower the window's selectable count or clear that rest. See `ip_remote_multi_client.go:11947`, `:12156`, `:12846`, `:13339`, and `ip_remote_multi_client_discovery_retry.go:42`. **[S]**

This is stronger than attributing the gap to the numerically similar 15-second resize interval. It is weaker than the reviews' exact reconstructed pass histories. Setup events were dropped; the retained 1.700872-second OLD admission is not proven to be the last admission of that pass. The exact start/end of the empty acquisition and its sampled jitter were not recorded. Review 1 and review 2 infer different starts. Their common mechanism is plausible; neither decomposition is a measured timeline. **[R, D]**

Review 1's separately measured SYN schedule supports a second mechanism, and current gVisor source corroborates the distinction it makes. The handshake creates its backoff timer with `InitialRTO` and package `MaxRTO`, then doubles it (`gvisor/pkg/tcpip/transport/tcp/connect.go:181`, `:730`). `TcpMaxRto` reaches the ordinary sender option (`snd.go:321`); it does not by itself cap the handshake's SYN schedule at eight seconds. Review 2's eight-second SYN-cap argument is wrong. **[S]** The scratch schedule is not an original-run SYN trace. Approximate wall-clock alignment and matching `connect_done` times make retransmission alignment a strong hypothesis, not proof of the precise SYN that won either qualified run. Do not promote the proposed 3.5–4 s or 3.9–4.4 s fix timelines into results. **[D, H]**

Named lookup also differs from ranked discovery with exclusions. A provider being visible by client id does not prove that the device's exact ranked/family query would already return it. The later successful ranked listing proves availability then, not at the earlier named timestamp. **[S, D]**

## 2. Corrections that affect the design

### An early one-shot wake is a real hole, not a proven universal ordering

The fixture constructs provider 4 after the first old-provider dial; exhaustion requires failures from the relevant cohort. That construction dependency does not order delivery of the final failure signal against provider visibility. Callback delay, Transfer admission, concurrent dials, and scheduling can change that order. Neither retained trace timestamps all relevant refusals. Thus the reviews' claim that the credit is *certainly always* consumed before visibility is too strong. **[S, R]**

The design defect is nevertheless clear: a valid execution can consume its only wake on an empty listing, with no later discovery notification when a provider appears. A control with the provider available before the first SYN does not cover that execution. Force the empty-result-before-visibility ordering with barriers in a deterministic control; observe the ordering honestly in the natural workload. An acquisition-only mechanism also cannot wake a pass that does not exist during rest. **[D]**

The earlier candidate's attempt-stamp invalidations are source defects in that private candidate. `resetSynGenerationWithLock` can run during source-ordered completion or winner projection (`ip_remote_multi_client_group_completion.go:650`, `:989`), after the send path recorded failure evidence. Removal also calls `forgetDialFailedClient` for every flow, including flows that never recorded that channel. Neither operation necessarily begins a new logical attempt. These facts do not identify the exact losing guard in the earlier `owned=4` race-build failure; that decision was not captured. **[S, R]**

### Listing, minting, evaluation, and admission have different budgets

Review 2 says minting happens only inside `expand`. That is false for discovery windows. `randomEnumerateClientArgs` calls `NewClientArgsForDestination` or `NewClientArgs` before `offerClientArgs` (`ip_remote_multi_client.go:12085`). Fixed destinations have a separate deferred-mint path in `expand`. **[S]**

An empty listing creates no candidate identity, but an extra enumerator opportunity is not inherently a read-only operation. The producer can mint ahead of its consumer, retry a refused mint, hold args, time out an offer, or receive a late successful result that must be retired. `multiClientExpandPlan` bounds candidates consumed/evaluated by one expansion; it is not an independent bound on all mint calls, late results, or retirement work. **[S, D]** A safe design must count actual calls and prove their ownership, including simultaneous ordinary args and demand-driven results.

Preserve the separate platform retry semantics. `waitWindowRetry` ignores network changes for rate limits; a successful listing clears the error streak but intentionally preserves the rate-limit streak; a successful mint clears both (`ip_remote_multi_client_outcome.go:381–426`). It is insufficient to observe that a generator notification does not directly wake this wait. A new worker, parallel API path, canceled-wait restart, or new slot can bypass it indirectly. All such routes need the same admission authority. **[S, D]**

### Review 2's renewable rest credit does not preserve pacing

Resetting a credit after every unproductive pass permits sustained demand to end *every* newly created rest. Increasing `failureCount` does not preserve backoff if `notBefore` is immediately overridden each time. With 15-second empty acquisitions at the ceiling, the natural cycle is roughly 75–135 seconds; repeatedly skipping its rest can reduce that to roughly 15 seconds. Faster failing passes can amplify further. There is no inherent two-times rate bound. **[D]**

Do not adopt that proposal. If a later experiment needs recovery during rest, give it a separate elapsed-time budget and finite episode lifetime which cannot be renewed by each failed pass, each retransmit, or each new destination/ISN. Preserve the ordinary backoff state and the platform's rate-limit deadline. A listing during rest also needs a bounded consumer/reservation; otherwise it can merely mint speculative args while ordinary acquisition is forbidden. **[H]**

### Silent demand needs a real attempt clock and must not invent failure evidence

`noteConnectAttempt` records SYN sequence, end sequence, and failed-exit membership. It has no first-SYN timestamp (`ip_remote_multi_client.go:10631`). Review 2 cannot calculate attempt age from the existing `dialFailedSynSeen/dialFailedSyn` pair. The existing `synWaitStart` serves a committed exit and is affected by binding/completion transitions; it is not automatically a lifetime clock for an unbound wide race. **[S]**

The single-candidate path can commit an unanswered SYN and later infer failure on another actual source probe after the per-exit threshold. A wide TCP race stays unbound without a response, so the bound-client inference does not run. Its no-response timer does not select a TCP winner (`:7377`, `:7546`, `:7760`, `:9294`, `:10104`). Actual race formation depends on family, health/rank, service filtering, affinity, caps, and configuration, not just min-tier load. A wide race reduced to one survivor is also not the original single-candidate path. **[S]**

An observational first-SYN clock can make both shapes eligible for bounded discovery demand. Define it at the actual source attempt boundary and preserve it across same-ISN retransmits and delayed completion. Treat it as evidence of unanswered demand, not proof that each provider failed. It must not add strikes, negative service entries, warnings, false receive evidence, or a forced winner. Record local admission/refusal separately so native queue refusal is not mislabeled as a remote timeout. Any touched legacy timeout tunable belongs in settings under CODESTYLE. **[H]**

### Negative service memory is preference, not a ten-minute ban

For two or more candidates, the service filter returns unmarked candidates if there are any; if all are marked it returns **all** of them oldest-first. With zero or one candidate it returns the input unchanged. The key includes channel pointer, destination IP/port, protocol, and family. A standing repeat can refresh the ten-minute timestamp even when strike and rerace accounting are deduplicated. A stale failure returns before that write. See `ip_remote_multi_client_service_failure.go:19`, `:76`, `:120` and `clientDialFailure`. **[S]**

Thus Review 2 overstates automatic exclusion after a three-second delay. Inference requires a later qualifying source probe and current binding; late valid inbound can still recover an inferred-failed attempt under the existing adoption guards. Concentration on unmarked alternatives is plausible, but a 3.264-second SYN-ACK observation elsewhere does not prove that inference or harmful cache concentration occurred in the qualified browsing pair. Do not shorten the TTL or remove inferred-failure writes on this evidence alone. **[S, D, H]**

`FlowsReraced - DialFailuresIntercepted` is not an inferred-failure count. Interception includes repeated, unmatched, stale, and wide-race candidate signals; rerace accounting is deduplicated and excludes ordinary wide-candidate withdrawal. It includes inference under different conditions. The subtraction can hide inference or underflow unsigned arithmetic. `FlowsReraced` also does not grow once per retransmit of the same flow/exit attempt. Observe `signaled`, standing, and `firstFromExit` at the actual decision instead (`ip_remote_multi_client.go:9135`, `:9196`, `:9250`; `reliability_metrics.go:636`). **[S]**

### Full windows and the unused speed exit are separate from late discovery

The 32 MiB SDK policy produces quality min=max=hardmax=5, speed=1, no standing reserve, and strict ownership caps (`sdk/mobile_memory_policy.go:237`, `:292`, `:535`). A fully selectable quality window needs no expansion. A retained warned owner may create selectable shortfall while still consuming ownership; ordinary acquisition can then be attempted but final admission refuses a new distinct owner at the ceiling. Same-identity replacement has its own guards. "A full window never expands" is therefore too broad; "demand cannot safely add a distinct owner beyond the hard cap" is the relevant invariant. **[S]**

Review 1's cross-window observation is source-supported. HTTPS normally selects quality then speed, but the all-failed quality fallback stays nonempty and can return before speed is considered (`ip_remote_multi_client.go:3156`, `:7820`). The fixture refuses providers with ordinal less than four; provider 5 is outside that hook. Its retained application dial/body counters are zero in both arms. **[S, R]** This establishes an unused potential route. It does not measure successful speed spillover or prove a 1.5-second TTFB.

Prefer an eligible untried exit in an already allowed window before retrying failed exits as a separate selector experiment. Preserve pinned/fixed policies, family, caps, and established bindings. Keep the original provider-4 admission case unchanged; add a distinct healthy-speed variant. Replacing `page1Provider == 4` with "any non-refusing exit" in the original case would stop testing its intended acquisition transition. **[H]**

All-refusal swallowing is current explicit behavior, not a newly discovered accidental omission: `TestDialFailureEveryExitFailsInWideRaceStaysBounded` expects no app packet before its timeout. `TestDialFailureEveryExitFailsReracesOncePerRetransmit` additionally rejects provider SYNs that do not correspond to the app's sent schedule. Those tests pin today's recovery policy. They do not make that policy an immutable product requirement; a new retry owner can change it deliberately if measurement demands that change and the new ownership contract is tested. **[S]**

### Removal, receive pressure, and retention need narrower conclusions

The later provider removals are recorded; their reason and affected flow state are not. `verdictAction` distinguishes flowless removal, loaded-owner quarantine, hard evidence, and explicit expiry policy. `TestDialFailureOnlyExitIsNotHealthyToBlackholeDetection` already pins the fact that refusal-only traffic does not prove useful receive progress. The trace cannot establish that a healthy carrying owner was removed or that its removal caused the stall. Preserve original CSS custody evidence and capture the actual verdict/flow count before changing health policy. **[S, R]**

Likewise, `sampled_receive_peak == 0` is accompanied by `UNMEASURED_HOST_RECEIVE_CAPACITY_AND_REFUSALS` in both `BROWSING.json` files. It is not evidence of an empty receive pipeline. There is no measured receive/lock delay that explains this particular gap, but these records do not rule one out. A reliable carrier also does not remove Transfer's delivery/ACK/retry responsibilities. **[R, D]**

A pass can retain evaluation ownership through acquisition + setup + initial ping, 75 seconds at the listed defaults. Review 2 overstates that as a guarantee that one dead candidate prevents every other candidate from being evaluated: setup is synchronous, but ping evaluation has asynchronous workers and another request slot may proceed before acquisition ends. Instrument the actual phase and remaining slots. The post-admission request wait is a useful separate efficiency question; breaking the loop early requires checking surplus-candidate and cleanup ownership, not a blind `admitted == budget` edit. **[S]**

Review 1's service-cache retention finding is real and stronger than its "up to ten minutes" wording. Channel pointers in removed-client keys can remain reachable **beyond** TTL: expiry is lazy, and `removeClients` does not prune this map. Cleanup occurs on matching lookup, a capacity-triggered sweep, exact success, or owner shutdown. There is a 4,096-entry bound, not a measured retained-byte bound. A separate lifecycle repair should test exact-channel eviction and a late standing callback attempting to reinsert after removal; pruning alone is not proof that the root stays absent. Other race/flow references must also drain before claiming channel memory was reclaimed. This is a valid memory investigation, not a demonstrated cause of page-1 latency. **[S, D]**

## 3. Recommended implementation sequence

### First: make the live acquisition hear continuing demand

Keep the normal enumerator as the sole platform-call owner. After an empty listing, permit a small paced number of additional actual reads while a live TCP attempt remains unanswered and a real acquisition slot is available. Reasserting demand from same-ISN retransmits can cover late visibility; a bounded level/lease can also remain pending across an empty result. Neither must depend on `firstFromExit` becoming true again. A notification is not a consumed API call, and an empty result is not completion of the application's demand. **[H]**

The private direction at `/private/tmp/urnetwork-connect-discovery-level-demand.PREP.icz9i9d8/` follows this narrower scope: acquisition/slot ownership, bounded reads, one demand mint claim, deadline-bound args, and receiver validation. Portions were inspected while the author was still editing. It is an unsealed draft, not an audited version or runtime result. Its value depends on the following controls actually passing.

| Resource or state | Required invariant |
| --- | --- |
| Listing | Count actual underlying calls, including family variants; coalesce concurrent flows; finite per-pass and elapsed-time limits; no work after cancellation/deadline. |
| Mint | Demand consumes an existing acquisition opportunity. A simultaneous ordinary handoff cannot authorize an additional mint. Count attempted, successful, refused, timed-out, reused, and late-return identities separately. |
| Evaluation/admission | Preserve request/admit budgets, evaluation epoch, current slot, family checks, and all-owned hard cap; no stale result transfers to a later slot/pass. |
| Platform errors/rate limits | Use the existing wait/streak authority. Demand cannot shorten, parallelize around, or reset it; successful listing cannot reset a refused-mint streak. |
| Cleanup | Every losing/late args object has exactly one retirement owner; candidate teardown and workers join; retained speculative args remain bounded. |
| Flow and packet state | Demand changes no binding, failed-service history, strike, reset, ACK promise, or existing packet custody. No packet is retained by this first change. |
| Bounds and lifetime | Settings own policy values. State/timers belong to the window or explicit parent owner; no per-SYN goroutine, unbounded destination map, or implicit process-global mutable budget. |

Per-window limits multiply across windows and hosted devices. Report total reads, mints, evaluations, cleanup, and outstanding work under concurrent demand; do not call a fleet outage safe merely because one window stays below a small cap. An explicitly passed common owner budget is different from the process-wide singleton suggested in review 1, which conflicts with CODESTYLE's instance ownership rule.

This first change must leave resize rest intact. That restriction makes its cost and causality easier to evaluate, but limits its recovery coverage. A provider appearing after its read budget or acquisition deadline can still wait for the ordinary next pass. Demand based only on source retransmits also inherits their gaps. State those limits in results.

### Then: address the remaining measured delay

Use the unchanged full workload to choose the next change:

- **Demand during rest:** design a separately budgeted recovery opportunity with a real consumer and mint reservation. Preserve the existing exponent and platform deadlines. Do not renew an override every failed pass.
- **Admission precedes a long wait for the next source SYN:** design a bounded packet retry owner, or another explicitly tested delivery mechanism. Do not assume earlier listing alone will meet five seconds.
- **No ownership vacancy:** first evaluate permitted untried exits already owned. A flowless replacement may be a separate policy, but discovery demand must not evict a carrying owner or exceed mobile caps. Full-window coverage cannot be satisfied by manufacturing a vacancy.
- **Local refusal, delayed signal, or carrier/lock contention dominates:** fix that measured boundary with its actual recovery owner; faster discovery is not evidence that this other path is correct.

Review 1's retained-SYN proposal is plausible but not a small read-only addition. A private byte copy can retain a flow, queue, worker, and client graph; “32 × 200 B” is not total memory. Re-offering bytes through the usual send entry does not by itself settle competing pending admission, duplicate coverage, source order, ACK ownership, or cancellation. It must handle new-ISN and observed A→B→A attempts, same-ISN after reset, delayed old RST/completion, a concurrent first answer, TFO payload bounds, cap eviction, expiry, and owner shutdown. A stale post-cancellation SYN being answered with an app RST is not a substitute for withdrawing the retry owner. **[H]**

If this mechanism is needed for the original latency objective, implement and test that ownership deliberately. The current app-driven architecture and tests above explain the added work; they do not justify abandoning the objective. Fail-fast ICMP is also a behavior change, not a successful page load, and OS-specific response predictions in the reviews are not a substitute for testing the supported stacks.

## 4. Attempt identity: keep discovery observational

There are three separate notions: the source's logical SYN attempt, its failed-exit evidence, and the collapse/admission generation that projects successful work in source order. Numeric ISN equality alone cannot represent an observed A→B→A sequence. Conversely, delayed projection of the current SYN must not invalidate its already recorded failures. **[S, D]**

For a precise per-attempt demand observation, read the source attempt under the flow lock and validate the observation when it is consumed. Bump observational identity for a real observed attempt/owner transition; do not bump it merely because `resetSynGenerationWithLock` is replayed or an unrelated candidate is retired. Removing a member actually present in the failed set changes the exhaustion evidence, even if the TCP attempt is unchanged. Keep that distinction explicit.

A coarser destination-level demand design can tolerate a stale bounded read without retaining per-flow authority. It still needs a finite lifetime, bounded key count, live acquisition authorization, and platform/mint admission. “No stamp needed” is a possible design tradeoff, not proof that all lifecycle/generation checks disappear. Neither approach may reopen a reset cohort, clear canonical failed-service evidence, or adopt stale responses to make a discovery test pass.

Preserve `TestTcpGroupRecoverySameCohortResetSurvivesWinnerPromotion`, the unwritten-ABA controls in `ip_remote_multi_client_group_generation_test.go`, and the advancing-ACK recovery controls. Add barriers at the actual source-admission/completion boundaries, not only tests of a helper stamp.

## 5. High-value verification

The following is a plan, not a report of completed tests. Use existing composed harnesses and real public send/receive entry points where the behavior crosses layers. Fake time can advance a declared timer in a deterministic control; it must not secretly age discovery state or wake the natural 100-page runtime. Negative assertions should follow explicit barriers and completed ownership transitions, not a short sleep.

| Case | Required observation |
| --- | --- |
| Late visibility inside acquisition | Initial ordinary listing empty; first demand listing also empty; provider becomes listable afterward; same current attempt produces another bounded read and normal admission before the original acquisition ends. No fixture generator poke. |
| Visibility races an in-flight read | Hold the API result, change listability on either side of its return, and prove pending demand is neither lost nor duplicated beyond its budget. |
| Demand in rest | For the first scoped change, repeated demand leaves `notBefore`, streak, listing/mint counts, and ordinary wake unchanged. If later supporting rest, test across many failed passes, not just one credit. |
| Never-listable provider / many flows | Concurrent attempts, destinations, retransmits, and new ISNs cannot reset budgets; no mint from empty results; bounded work and idle recovery after demand ends. |
| Nonempty but unusable provider | Bound actual mint attempts, evaluation, identity retirement, and retained args across repeated failures and pass boundaries. Admission count alone is insufficient. |
| Ordinary args versus demand args | Race both against the same slot; exactly the allowed slot/mint reservation wins. Reject late owner/slot results before evaluation, and retire them exactly once. |
| Platform error and rate limit | Test listing errors and mint refusals separately. Demand and successful listings cannot defeat the current rate-limit wait/streak; network change preserves the rate-limit rule. |
| Pass cancellation / deadline / rebuild | Hold listing, mint, offer, setup, and ping at separate barriers; retire late successes and join workers. No handoff to a new epoch or blocked receive callback. |
| Silent wide race | No explicit failure signals; demand after a real observational age without strikes, service-memory writes, synthetic success, or forced winner. Preserve late-answer recovery. |
| Single versus wide formation | Same unanswered source schedule, actual selected widths, wide-to-one survivor, and configured cap boundary; record which inference path really ran. |
| Full mobile ownership | Actual 32 MiB constructor, five owned quality exits carrying healthy siblings, including warned/incompatible retained owners; no sixth owner and no disturbance of original connections. Record attempted acquisition separately from installation. |
| Cross-window selection | Healthy speed alternative, then all windows failed; preserve pins/fixed/family/caps. Keep this separate from the original provider-4 acquisition proof. |
| Attempt/control order | Same ISN, new ISN, A→B→A, delayed successful completion, delayed old RST, explicit reset, cancellation, unrelated removal, and actual failed-member removal. Established inbound wins against stale demand. |
| Custody and memory | Original CSS tuple/owner and active siblings survive; complete-message refusal preserves actual Transfer retry/ACK responsibility; pooled ownership balances and all children drain. |
| Disabled / fixed / non-TCP | No accidental new demand path or changed retry policy. If an aggregate budget is used, one owner closing does not cancel another owner's work. |

A root-cause regression should expose the old behavior under its real causal ordering; existing safety and characterization tests can pass on both versions. Neither review's statement that every proposed test must fail OLD is appropriate. Do not manufacture an OLD failure by changing timers, readiness, provider order, or instrumentation. The natural workload can legitimately pass OLD: report “no reproduction in this run,” keep the receipt, and do not retry until a desired RED appears.

### Preserve the original runtime contract

Keep 100 HTML requests, 400 asset attempts, 99 overlaps, 50–300 KiB per page, the existing 300-second application deadline, the five-second navigation limit, original transport/dial settings, body/hash checks, cancellation dispositions, connection cleanup, and worker joins. Keep late physical provider construction, original CSS custody and the provider-4 requirement. Do not require 400 response-body closes when some attempts were canceled before a body existed; created bodies must equal closed bodies with cancellations accounted for.

Run normal and race controls before the expensive workload. Then use a predeclared finite comparison plan on immutable, matched source/overlay snapshots. Preserve all qualified OLD outcomes and all failed/unqualified FIX outcomes. A deterministic test pass or one natural pass is not a distributional performance claim. The original 32 MiB iOS-profile and 64 MiB Android goals remain; host providers and a desktop TUN cannot qualify device footprint or an iOS Network Extension. No memory increase is supported by the current stall evidence.

### Record the missing causal boundaries

Use a bounded common monotonic trace or explicit same-event clock anchors for:

- actual source SYN observation and admission disposition, attempt ordinal, and retransmit ordinal;
- provider dial/failure production, client intercept, and `clientDialFailure` standing decision;
- actual selected cohort, selectable count, all-owned count, and family;
- acquisition/pass/slot start and end, deadline, remaining request/admit/mint budget, and rest state;
- demand reason and decision, every actual API/mint start/result, candidate offer/admission, and late retirement;
- first offered SYN on the new exit, first inbound, connect completion, TLS, and HTTP first byte;
- warning/removal reason and carried-flow count, plus bounded receive/queue/refusal evidence with explicit “unmeasured” states.

Record overflow. Totals can establish event counts after timestamp samples drop, but cannot reconstruct missing order. Use local ordinals instead of raw identities or credentials. Keep instrumentation nonblocking on shared callbacks and avoid turning an observer into a wake source.

These boundaries distinguish acquisition delay, platform visibility, setup/evaluation, application retransmission alignment, silent race behavior, and local backpressure without changing the workload to fit a preferred explanation. Only after that evidence and the unchanged gates pass should the synthetic stall be called fixed. Attributing the original iOS session still requires matching device evidence.

## 6. Provenance and limits

The inspected Connect HEAD was `5485774e49c25bec995f6d164633bb9c29b893f2`, with existing working-tree changes including the first-wake helper. The qualified C3 pair includes uncommitted source and overlays; its manifests and whole-workspace fingerprint in the problem statement, not HEAD alone, identify the measured programs. The private attempt-stamp and level-demand candidates are separate from those programs.

This document is a synthesis and source/evidence review. It supplies no new runtime qualification, no production-provider measurement, no device memory result, and no approval of an unsealed private patch. Adding it changes a source fingerprint but does not change application behavior.
