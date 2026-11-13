# Page-navigation stalls: problem for independent review

Status: 2026-10-23. **The 100-page stall is reproduced and remains unresolved.** This brief separates measurements from hypotheses. It is a request for root-cause and design review, not a claim that the current experimental fix works.

## User-visible problem and goal

On iOS over Wi-Fi, browsing several pages quickly initially feels fast, then new links sometimes pause for roughly 5–15 seconds before opening. Pages may still be downloading assets when the next navigation begins. Providers on Main may contribute; the original device session has not been attributed to a particular local or remote fault.

We need a realistic synthetic regression that reproduces this pattern, deterministic tests for the responsible transitions, and a measured fix. Preserve working connections and their exit IPs. Do not solve a new navigation's problem by tearing down a shared resident/provider or moving an established media/HTTP connection to another exit.

Memory goals remain **32 MiB for the iOS profile and 64 MiB for Android**. The iOS profile is measured on the allowlisted Android phones for MEMSTEADY. Host simulations containing several provider processes do not qualify mobile memory or an iOS Network Extension's total footprint. Increasing memory has not been demonstrated to solve this stall.

## Current reproducible workload

The private test is `TestPrivateFullTunH1Sdk32LateQualityAdmissionNatural100Pages`, compiled in `server/connect/perfvar` through a Go overlay. It uses the actual SDK constructor, TUN/socket path, MultiClient, H1 carrier, provider Transfer/NAT, and verified TLS 1.3 / HTTP/1 origins. The control plane, provider pool and origins are local fixtures; this is not a production-provider or Fast.com measurement.

- SDK 32 MiB low profile; provider disabled; H1; no H3 or DNS-alt experiment.
- 100 navigations, 100 HTML requests and 400 asset attempts. Each page plans 50–300 KiB, with HTML, JS, CSS and image traffic.
- 99 overlaps: the next navigation starts while the previous page still has asset work. Cancellation is intentional and accounted for.
- Unchanged 300-second application deadline and a **5-second navigation first-byte limit**.
- Initially four quality exits and one speed exit. A fifth quality provider is physically constructed after the affected navigation reaches an old provider. No fixture timer aging, forced generator notification, forced exit removal, or retry-counter reset advances discovery.
- Old providers reject new connections to the affected documentation origin while still carrying established traffic. A 12,288-byte CSS response must finish on its original TCP tuple and owner, held until the next HTML is served by the newly admitted provider.
- Exact bodies/hashes, cancellation dispositions, original CSS custody, connection cleanup and worker joins must pass. A slow navigation is not qualified if those checks fail.

The old-provider refusal is an immediate generic dial error in the fixture's provider dial hook. `TcpSequence.Run` passes it to `classifyDialFailure`, which normally produces a capacity-class ICMP unreachable for a non-timeout error within its signal horizon. It is **not deliberately silent SYN loss**. Actual cancellation, delay or loss could still prevent a particular signal from reaching the client; existing reports do not establish every signal's timing.

## Qualified measurements

Both arms below completed the entire workload and failed only the named navigation-latency assertion. They are one matched pair, **not a statistically sufficient performance comparison**.

| Measurement | OLD | First wake fix |
| --- | ---: | ---: |
| Affected page first byte | 17.545 s | 31.722 s |
| Navigation limit | 5 s | 5 s |
| Median first byte, all 100 pages | 911 ms | 1,094 ms |
| Completed navigations / overlaps | 100 / 99 | 100 / 99 |
| HTML requests / asset attempts | 100 / 400 | 100 / 400 |
| Actual asset response bodies created / closed | 330 / 330 | 322 / 322 |
| Assets canceled before a response body existed | 70 | 78 |
| Original CSS body and owner retained | Yes | Yes |

Only page 1 exceeded the limit in either arm. OLD's next-slowest page was 3.048 seconds. The body completed eventually; the reproduction isolates a connection/provider-recovery pause, not evidence of a growing download-body memory backlog.

Earlier runs had test-harness defects: a final verdict could consult a normally canceled request context after successful completion, and a parser incorrectly demanded 400 body closes even when some requests were canceled before a body existed. Those defects have deterministic controls and were corrected before the qualified pair above. Earlier raw timing observations must not be promoted into qualified results.

## What the trace establishes

In qualified OLD, on one consistent discovery-trace clock:

| Event | Elapsed |
| --- | ---: |
| Late provider becomes visible through the named API | 11.408621 s |
| Next ordinary discovery call | 25.856781 s |
| Candidate args received | 26.252934 s |
| Candidate admitted | 26.890457 s |

The visibility-to-listing gap is **14.448160 seconds**, close to the default 15-second window resize cadence. The HTTP phase uses a different clock origin. Do not subtract these timestamps from the page's 17.545-second TTFB to invent a SYN-to-admission timeline.

The first wake fix targeted a producer already parked inside discovery-expansion deferral. **Neither arm entered that wait**: `wait_enter`, `wait_wake_retry`, `wait_wake_timer`, `wait_wake_network` and `count_clear` were all zero. That fix does not address the ordinary acquisition path exercised here. The larger FIX TTFB does not, by itself, prove that change caused a regression.

## Relevant existing behavior

1. A SYN attempt records exits that failed that attempt. Same-ISN retransmissions retain the failure cohort; repeated failures from the same exit do not become unlimited fresh strikes.
2. Destination-specific failure can re-race a flow without globally warning an exit. Global dial starvation requires multiple failures spanning multiple destination IPs and no recent proven connects. One rejected site must not evict an exit still serving healthy connections.
3. Consequently, all exits for one destination can fail while the window's globally healthy/selectable count does not fall. There may be no exit-loss event to reset discovery pacing.
4. Candidate discovery and minting are bounded. Unproductive expansion backoff was added after excessive identity creation contributed to a Main outage. A fix must not restore a busy discovery/mint loop.
5. The SDK's effective 32 MiB quality target is five in this fixture. Four initial quality exits therefore leave a genuine vacancy. The target for selectable exits and the hard cap on all retained owners are distinct.
6. The no-response race timer is not a generic five-second TCP winner timer: the TCP SYN path is guarded by `dialProbeAwaitsResponse`. A fully silent cold TCP race can remain uncommitted. Silent peers need separate coverage from the immediate-refusal case above.

## Current experimental direction — not qualified

A private candidate permits one coalesced generator wake **during an existing, still-live non-fixed acquisition pass**, when a single current SYN attempt has exhausted the current healthy, IP-family-compatible exits.

It does not start another expansion, raise the target/hard cap, clear backoff, directly call the platform API, or mint an identity from the receive callback. All destinations share one credit per window acquisition. It rechecks the flow attempt, owner lifetime, owned-member snapshot and vacancy. Warned/incompatible retained owners cannot be adopted, but must not incorrectly make the selectable cohort appear unexhausted; they still consume hard-cap ownership.

The first implementation passed 14 normal deterministic controls, including real public `SendPacket` → provider refusals → actual enumeration/evaluation → fifth-provider admission without advancing fake time. Four healthy sibling flows kept their original owners. However, its race build failed that same admission control (`owned=4`, expected 5); the other 13 controls passed and there was no Go data-race report. Later fresh/affected tests stopped. **This candidate has not passed the 100-page workload.**

The deterministic public control has a fifth provider already available before the refusal. The full workload constructs it after the first old-provider attempt. A wake may therefore arrive too early and spend its credit before the provider becomes API-visible. Likewise, an acquisition-only wake cannot help while the resize loop is resting between passes. Neither possibility is settled by the normal control's success.

## Attempt-identity issues found during review

The candidate added a stamp to reject stale failure observations. Two over-invalidations are source-established:

- Bumping it in `resetSynGenerationWithLock` couples logical connection identity to delayed collapse/replay bookkeeping. An initial or new-ISN source completion can happen after valid failure evidence was recorded. Forced same-ISN candidate projection can also replay an already observed generation.
- Bumping it unconditionally in `forgetDialFailedClient` lets retirement of an unrelated/unadmitted candidate revoke an unchanged flow's failure cohort.

These are bugs in the **unqualified candidate**, not proven causes in the released app. The original race report did not serialize the exact guard decision, so attribution of that particular failed run remains uncertain.

The correction is under review: preserve logical attempt identity across bookkeeping replay; advance it for actual attempt/owner changes and actual failed-member removal. It must respect existing canonical reset semantics: **same-ISN SYN replay alone must not reopen a reset cohort**. New-ISN / observed ABA transitions, delayed old RST replay, source-reset flags and owner cancellation require explicit deterministic tests. Discovery must not redefine the TCP collapse contract merely to make its wake test pass. Eight additional source-projection controls are being prepared; they have not run.

## Questions for independent reviewers

1. What is the actual earliest safe recovery opportunity after one destination exhausts the existing exits? Is waking an already active acquisition sufficient, or is a bounded opportunity during resize rest necessary?
2. Can the one-shot wake predictably precede provider availability? How should demand survive an empty listing without restoring API/mint amplification or bypassing backoff?
3. Which source boundary owns logical SYN-attempt identity? How can failure evidence survive delayed successful admission/replay while stale RST, same-ISN retransmission and observed ABA remain correct?
4. Are any receive callbacks, flow locks, Transfer admission waits or queued owners delaying failure signals or discovery progress? Identify the exact boundary and recovery owner; do not assume a reliable carrier removes Transfer's retry contract.
5. Is the reproduction missing a more plausible repeated-browsing bottleneck? Propose a measured discriminator that preserves the 100 pages, overlaps, body/cancellation checks, original connection custody and original timers.

Please distinguish source facts, runtime facts and hypotheses. A useful proposal includes a deterministic ordering/control, its expected OLD failure, the minimal behavioral change, and an experiment that could falsify it. Do not merely extend the latency limit, increase memory, drop the overlaps, remove a healthy provider, or reroute an established connection.

## Code and evidence map

Primary production code:

- `connect/ip_remote_multi_client.go`: `clientDialFailure`, `noteConnectAttempt`, `resetSequenceWithLock`, `resetSynGenerationWithLock`, `forgetDialFailedClient`, `randomEnumerateClientArgs`, `expand`, `orderedClients`.
- `connect/ip_remote_multi_client_discovery_retry.go`: discovery deferral, retained candidate offers and wake subscriptions.
- `connect/ip_remote_multi_client_group_completion.go`: source-ordered control projection, delayed completion and forced SYN-generation handling.
- `connect/ip_remote_multi_client_outcome.go`: evaluation epoch/lifetime ownership.
- `connect/ip.go`: provider TCP dial and failure classification.
- Existing unbound-flow, re-race, collapse and TCP group/reorder recovery tests. In particular, preserve `TestTcpGroupRecoverySameCohortResetSurvivesWinnerPromotion` and the unwritten-ABA / advancing-ACK barriers.

The private workload and candidate are not yet durable PERFVAR coverage. A public-interface 100-page suite is prepared separately but still requires execution/integration. On this host:

- Qualified pair: `/Users/builder/Library/Caches/urnetwork-runall-owned/hosted-h1-current112.PREP.jxp9wXRI/full-correctness.9urtxbr6/native100-verdict-c3-pair.PREP.gf0f57bm/`. Each arm has `NATIVE100.json`, `BROWSING.json`, `TRACE.json`, `QUALIFICATION.json` and original exit/census receipts.
- Initial candidate/control source: `/private/tmp/urnetwork-connect-destination-exhaustion.PREP.acntz2nm/`.
- Logical-attempt correction under review: `/private/tmp/urnetwork-connect-discovery-attempt-stamp.PREP.f7jferjk/` (not qualified; consult its current review status).
- Further bounded full100 observation derivative: `/private/tmp/urnetwork-sdk-native100-demand-observation.PREP.tsuyw8mn/` (not yet qualified). It is intended to retain acquisition state, failure cohort, demand decision, listing and HTTP events with explicit clock boundaries/overflow.

Frozen C3 snapshot includes uncommitted changes; branch heads alone do not reproduce it. Whole-25-repo fingerprint: `5468a3a305372402c64610c8cb938a3d1056462d7ca03fb7f6c70a76db0ed9a3`.

| Repository | HEAD at the measured snapshot |
| --- | --- |
| Connect | `5485774e49c25bec995f6d164633bb9c29b893f2` |
| SDK | `829f2a9223f229130c4e4f8797d09fa3476ccae6` |
| Server | `ec0fa96beed55cd7f91d79df905352be7ba779e6` |

Use the retained source/overlay manifests, not just these HEADs, to reproduce the experiments. The document itself is being added after the measurements; a documentation-only source fingerprint change does not represent an app-code fix.
