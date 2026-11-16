# Peer review 2: shared DATA admission, reliable immediate delivery, and bulk stalls

Date: 2026-11-09. This report supersedes the implementation/status portions of [SIMPLERISSUE.md](SIMPLERISSUE.md). It is a review request, not a claim that the performance problem is fixed. Please also read [CODESTYLE.md](CODESTYLE.md).

## Decision we need help making

We want sustained bulk throughput comparable to the earlier **300–400+ Mbps synthetic candidate**, without multi-second loaded-request stalls, while simplifying memory admission. The latest aggregate candidate can still reach 304 Mbps, but its five consecutive windows are inconsistent and two fail the unchanged latency gate.

The intended design remains:

1. One shared DATA-memory resource per lifecycle owner, rather than restrictive role/lane leaves that strand available root capacity.
2. Protocol traffic excluded from DATA admission, with actual retained metadata and ownership still understood and bounded.
3. Reliable immediate delivery for explicitly opted-in messages: retain Transfer ACK/retry semantics, permit downstream delivery across an earlier sequence hole, and deliver each original exactly once.
4. Initially apply automatic immediate delivery only to validated pure TCP ACKs. SYN/FIN/RST remain ordered.

**The current candidate does not yet implement the complete one-resource design.** It still has RX/TX/Pack/recovery children and negotiated lanes. Reliable immediate delivery and several ownership fixes have been implemented and tested, but root-only DATA admission has not been adopted because a deterministic full-root progress counterexample remains.

Please review the actual current state below, rather than assuming either that the simpler design is fully implemented or that one high-throughput window qualifies it.

## Measured comparison

All synthetic results below are noisy diagnostic runs, not statistically promoted baselines. They are tunnel workloads, not a masking microbenchmark. Windows within one process are correlated.

| Candidate | Client profile | Five whole-run throughput windows, Mbps | Loaded-request outcome |
| --- | --- | --- | --- |
| Earlier high-throughput reference | 32 MiB | approximately 428, 393, 340, 320, 366 | All valid; maximum under 554 ms |
| Earlier high-throughput reference | 64 MiB | approximately 362, 378, 311, 394, 402 | All valid; maximum under 535 ms |
| Earlier current-source RX-headroom experiment | 64 MiB | approximately 299.55, 129.08, 48.28, 15.85, 3.13 | Four valid; window 5 failed at 6.86 s |
| **Latest aggregate M2** | **64 MiB** | **41.43, 12.92, 15.88, 43.46, 303.58** | **Windows 1 and 4 invalid; windows 2, 3, 5 valid** |

Invalid windows are shown to expose failures; they are not accepted speed results. The latest aggregate's active-transfer rates were 42.75, 13.36, 17.89, 51.86, and 314.24 Mbps. Whole-run throughput is the comparison metric in the table.

The earlier reference and latest aggregate are **not a matched old/new causal experiment**. Known differences include:

- Earlier Desktop128 provider policy used encoded queue accounting and lane zero. Current policy uses exact retained-byte accounting and eight negotiated lanes, with a shared TX pool and zero effective per-sequence floor.
- The earlier client fixture used host/mobile-runtime selection with `AllowProvider=false`; current fixtures construct the real mobile policy, then turn providing off.
- Carrying-provider populations differed. Configured provider count is not equivalent to actual carrying count.
- The latest aggregate includes reliable immediate delivery, bounded protocol-reorder accounting, and budgeted-provider first-DATA reserve fixes absent from the older reference.

We must not attribute the difference to one of those factors without a discriminating test. The latest fifth window also shows this source has no universal hard ceiling near 15–40 Mbps.

## Latest aggregate: what the measurements establish

The completed M2 Fast64 run used the actual SDK mobile64 client policy and Desktop128 providers. The M3 successor has the **same 37 production files**; its difference is a test cleanup correction. All five windows and workers joined, with no origin/fixture join errors. The provider target remained twelve; that does not prove twelve providers carried traffic.

### Stalled request timing

Window 1's canceled-tail request took **8.242 seconds**:

- TCP establishment: 2.094 s.
- TLS: 1.347 s.
- Request write completed 5.199 s after the window began.
- No response header arrived during the following 4.801 s before cancellation.

Window 4's completed request took **7.905 seconds**:

- TCP establishment: 41 ms.
- TLS: 1.095 s.
- **Request-write to response-header: 6.769 s.**
- First payload followed the header approximately 13 microseconds later.

Window 5 reached 303.58 Mbps despite another approximately 2.101-second TCP establishment. Therefore neither cold TCP setup alone nor a universal configured throughput cap explains all these results. In particular, the window 4 delay occurs after the connection and TLS are established.

### Memory and recovery observations

At the latest five end-of-window cuts:

- Client RX used approximately 4.339, 4.355, 4.335, 2.418, and 0.428 MB, against a **4,362,076-byte RX leaf**.
- The DATA aggregate root is **43,620,760 bytes**; sampled root use was approximately 4.74, 4.77, 4.72, 2.70, and 0.71 MB. Root capacity was not exhausted at these cuts.
- Head-recovery use was zero at the cuts, but cumulative reservations/releases increased from approximately 17.7 to 62.7 MB. Zero at a cut does not mean recovery never ran.
- Provider handoff/drop/ACK-full/decode/receive-queue drop counters were zero in all five windows.
- Provider timeout-resend **deltas** were 1,963 / 2,069 / 1,390 / 787 / 10.

These observations support receive-pressure/recovery as an investigation direction. They do **not** prove the initiating cause. Client live drop measurements were unavailable; zero provider drops must not be generalized to zero client drops. We lack selected-flow, directional, exact-generation evidence tying the stalled request to a particular current hole, retained original, or admission refusal.

The older RX-headroom experiment is also important counterevidence: increasing physical RX permission approximately 28%, without increasing wire advertisement or the aggregate root, initially reached approximately 300 Mbps and then decayed to approximately 3 Mbps. Its request-drained RX/root cuts did not show monotonic accumulation. Merely increasing an RX leaf is not a demonstrated complete fix.

## What has actually been implemented and tested

### Reliable immediate delivery

The generic API retains reliability and delivery custody; it does not mean NoAck or automatic DATA-budget exemption. Pure TCP ACK classification selects both protocol admission and immediate delivery on the IP path. Generic immediate DATA remains DATA-budgeted.

The implementation includes:

- An explicit immediate-delivery wire request and terminal-immediate receipt, without falsely acknowledging a missing cumulative prefix.
- Exactly-once downstream delivery through duplicates, floor advancement, and relevant receive-worker recreation.
- Lost-terminal-receipt recovery for interior originals in a long train, using authenticated identity/ordinal state rather than trusting arbitrary interior IDs.
- Bounded terminal-boundary state so a later cumulative ACK can still retire predecessors even when its named original was already terminally retired.
- Normal authentication, contract/debit, and receive authority checks before early delivery.
- No success receipt after a refused downstream callback.

Component qualification completed **42 normal/race executions**: nineteen Connect roots plus two SDK roots in each mode. This includes a 10,003-message lost-middle/exactly-once/recreation case and a signed real-NAT ACK-across-hole case. Those results prove scoped invariants, not the complete performance outcome.

The full aggregate's five additional cadence/comparator cases have not produced valid performance measurements yet: their synthetic return route also carries legitimate `ControlSyncProvide` traffic, while the fixture assumed every return was an ACK. The fixture is being repaired to use a real ControlId client and match actual source/destination/generation/message identities. We will not synthesize ACKs, remove contract checks, or change flight limits to make those tests pass.

### Bounded protocol reorder metadata

Excluding protocol from DATA admission does not, by itself, bound every retained owner. We found that a bounded handoff mailbox could drain future non-immediate protocol messages into uncharged reorder structures without a retained-count bound.

The aggregate adds an O(1) held-protocol counter at the existing queue Push/Pop/Clear ownership points. It refuses an unfunded future before committing custody; current, duplicate, and immediate paths retain their intended behavior. Deterministic positive and mutation controls ran in the early aggregate tests.

This is not proof that all protocol metadata is bounded. Serialized TX `NoBudget` originals and pending provider receipt cardinality remain separate audit questions. A small raw-send token limit is not automatically a bound on serialized originals retained awaiting ACKs.

### Budgeted-provider first-DATA progress

A real two-flow test showed retained originals from A exhausting provider TX capacity before fresh B DATA could be admitted. Adaptive chunk shrinking alone did not guarantee B's minimum owner could fit.

The aggregate includes a bounded first-DATA reserve under the unchanged TX ceiling, exact flow ownership, cleanup, and mixed prepaid/ordinary credit funding. Component qualification ran **174 positive executions and two old-source expected failures**. The mixed-credit repair ensures complementary prepared and ordinary portions can fund one complete owner rather than testing each portion as though it must fund the whole message.

This mechanism adds complexity and remains a candidate for simplification if shared admission plus immediate protocol progress makes it unnecessary. It is not established as the cause of MAIN CLI-provider stalls; deployed MAIN settings/revisions are unknown, and many providers are reported to run without an explicit memory limit.

## New concrete delay: DATA waits for re-arrival after becoming current

A signed pending-protocol trace gives a smaller boundary to investigate. The original uninstrumented aggregate run transferred only **42,880 of 65,536 bytes by its unchanged approximately 8-second bound**, while retaining the unrelated flow's owners correctly. That failure remains open.

A later instrumented diagnostic ran the same two existing SDK roots and both passed. It did **not** reproduce the original failure, but it exposed this timeline:

1. The primary case's first DATA was a Head immediately and completed in approximately 30 ms.
2. In the adjacent case, protocol predecessors delayed the receive floor. Original DATA #4–8 arrived at time zero as future, non-Head messages; no decoded DATA owner was admitted then.
3. A cumulative outer ACK through #3 was observed at time zero.
4. Original #4 next arrived as a Head-promoted retry at approximately **4 seconds**. DATA #4–19 then decoded during approximately 4.00–4.02 s.
5. The last 1,216 unique native bytes arrived in #19 at approximately 4.02 s, when inner ACK 67,537 was enqueued/sent. Original #20 carried authentic inner-TCP replay ranges and was admitted at approximately 4.62 s; it delayed outer-custody completion, not the unique-response suffix in this passing sample. The existing 6-second checkpoint observed all 65,536 bytes delivered.

**Head=true is correlated with the retry, not proved to be the necessary admission condition.** Current receive recovery already accepts a verified, established, exact-current non-Head DATA owner. The original bytes would qualify if reoffered after the receive floor advances. The question is why useful re-arrival waits until the timeout.

The shipping default has `ReliableLaneProvenRecovery=false`. Its ordinary cumulative-ACK recovery path requires, among other conditions, a successor with `deferralOutstanding` or `sendCount > 1`; an untouched initial DATA original may therefore retain its original timer. This is source evidence, not yet a proven bug. Promoting every initial ACK blindly risks duplicate storms on a healthy lane.

Two deterministic tests are being implemented:

- Fence the **actual sender handling** of ACK #3, prove the prefix is removed and #4 is the current sender owner, record all shipping recovery predicates, and compare that deadline with the actual receive-floor transition. An ACK observation before forwarding is insufficient.
- Hold a published current predecessor paid from ordinary RX, then offer a larger contiguous successor with insufficient ordinary RX but enough recovery/root credit. The current prefix helper appears to accept only recovery-paid predecessors. Test whether that unnecessarily forces a retry, with wrong-generation, gap, returned/unpublished owner, foreign-budget, and exhausted-root/recovery negatives.

For the second case, the live trace saw ordinary-RX DATA #19 at 4.02 s and successor #20 decode only at 4.62 s, but did not directly capture #20's original decoder floor. The exact refusal cause remains a hypothesis until the barrier-controlled test proves it.

Do not extend the eight-second test deadline, force a resend, or alter an RTO to hide this boundary.

## Why root-only admission is not yet safe

The full-root counterexample in the first report has now executed:

- Fixed root: **55,296 bytes**, two real signed lanes, actual selectively acknowledged future ownership and repair promises.
- An equal-size **5,120-byte** missing current owner progresses using its paid promise.
- A larger **30,544-byte** missing current owner fails to progress under the same full root.
- Normal/race runs reproduce the equal-size pass and larger-current expected failure.

No fake filler, live cap reduction, or eviction constructs the witness. The larger owner is a legal receive shape; the sender's usual grouping does not establish a universal receive bound.

A source-only conservative proposal refuses unknown/unfunded future DATA **before SACK**, preserving already paid custody. It has not been adopted or performance-qualified and may sacrifice useful selective-gap feedback. Please evaluate a simpler progress guarantee, but do not discard previously acknowledged originals, invent uncharged headroom, or silently increase the root.

## Correctness, pages, devices: current qualification limits

- The latest production aggregate is M3: 37 production files, byte-identical to the measured M2 performance source; one test cleanup changed.
- Its remaining **418 selected Connect normal roots passed: 418 PASS / 0 FAIL / 0 SKIP**. The test process joined; source/module checks, process joins, image detach, and resource cleanup all passed.
- This is not a full correctness green result. The six earlier SDK aggregate failures included the real signed partial-transfer failure plus five cadence/comparator fixture failures. Full race, all packages/repos, and counterfactual coverage remain incomplete.
- Earlier baseline15 full100 runs passed both profiles: 100 HTML, 400 assets, 99 overlaps; median TTFB approximately 896/1,053 ms and maximum 2.72/4.61 s for 32/64 respectively. They are **not fresh full100 qualification of M3**.
- The last completed actual Pixel Fast5 remains one valid **3.5 Mbps** result and four completion timeouts. Some timed-out windows were still streaming, so completion timeout does not by itself prove a dead socket. No fresh aggregate device speed result exists.
- Final eight-cell MEMSTEADY remains outstanding: Pixel `3B161FDJG001KT` and Galaxy `R5CX21FY6ND`, each Wi-Fi/cell, each iOS-surrogate32 MiB and Android64 MiB.
- A physical iPhone is unavailable; Android/simulator measurements cannot establish the iOS extension's under-50-MiB physical footprint.

Performance scope remains hosted H1. Do not expand this review into H3/DNS-alt/P2P performance work.

## Measurement currently being prepared

One fixed-policy, first-stall observed Fast64 run is launching, with the same production source, traffic, timers, and memory settings. It should capture current selected-flow state at the unchanged five-second stall cut, before cancellation.

The prior attempt stopped before traffic because the diagnostic recorder exceeded its own 8 MiB bound. The older recorder already used approximately 19 MB of dense copied rows; added fields brought it to approximately 20.1 MB. This was a diagnostic-storage failure, not a measured app memory-cap breach.

The corrected recorder retains the identity table but stores occupied first/latest records in a fixed 6,144-row arena. It preserves the 8 MiB guard, performs no producer allocation, copies snapshots under its observation lock, and reports explicit overflow. Overflow or missing joins must prevent a complete causal-attribution claim. Thirty-eight normal/race controls precede the single five-window diagnostic. **No result from that run is available at this report's cut.**

The useful trace must join actual client/provider, direction, Transfer generation, lane, original message, and native TCP connection/ISN. Tuple-only matches, window-generation numbers, historical maximum counters, and an ACK merely observed before forwarding do not prove actual delivery or sender progress.

## Questions for reviewers

1. What smallest deterministic change would prove or disprove an avoidable initial-DATA retry wait after cumulative ACK progress? How can it preserve healthy-lane duplicate suppression?
2. Can a contiguous, actually owned ordinary-RX prefix safely support recovery admission of its successor? What exact ownership/generation/cost checks are necessary, and what adjacent negative case would invalidate that idea?
3. How should one shared DATA resource guarantee progress when an unknown missing owner can exceed its observed-tail promise? Is refusal before SACK the simplest safe answer, and how can selective recovery performance be retained?
4. Do exact retained-owner charges include scratch/rewrite promises that could safely be shared? Identify simultaneously live costs and full-root progress obligations, rather than simply removing charges to recover the old speed.
5. Is reliable immediate pure-ACK delivery sufficient to make lanes unnecessary, or does ordered DATA isolation still justify them? Which short experiment separates this from accounting and client-constructor changes?
6. What actual bound should apply to serialized reliable protocol originals and pending receipts, without tying ACK throughput to a small flight count or putting protocol behind DATA again?
7. Which selected-flow observation would distinguish RX refusal/retry delay from provider TX retention, native TCP congestion, or provider population changes in the latest 41/13/16/43/304 result?
8. Can you propose a simpler aggregate fix that covers both loaded-request stalls and sustained bulk throughput? Please specify a deterministic old-source failure, necessary invariants, and a short experiment that could falsify it.

Please prioritize a complete, explainable mechanism over accumulating independent reserves and timers. Do not weaken latency gates, move established flows across egress IPs, terminate shared residents on ordinary pressure, or interpret encrypted HTTP statuses as network evidence.

## Evidence index

The narrative is intended to stand alone. Private host paths may be unavailable to reviewers on other machines. `R` below is `/Volumes/data/urnetwork/temp/aggregate-flow-r15-r2`.

| Evidence | Path | SHA-256 prefix |
| --- | --- | --- |
| Earlier high-throughput reference | `/Volumes/data/urnetwork/synthetic-smoke-r1/recovery-performance-r1/RESULTS-runtime-r1.json` | `29d92b62` |
| Latest five-window aggregate closure | `R/aggregate-m2-fast64-diagnostic-r2/CLOSED-READBACK.json` | `c81fe075` |
| Latest actual speed readback | `R/aggregate-m2-fast64-diagnostic-r2/ACTUAL-FAST64-READBACK.json` | `3ad763c3` |
| M3 source definition | `R/aggregate-core-reserve-immediate-r3/READY.json` | `0441ce0c` |
| M3 production map | `R/aggregate-core-reserve-immediate-r3/PRODUCTION-MAP.json` | `a008fc1f` |
| Signed partial-transfer trace | `R/provider-first-signed-pending-trace-qualification-r1/TRACE-READBACK.json` | `c6241fa6` |
| Signed trace joined closure | `R/provider-first-signed-pending-trace-qualification-r1/CLOSED-READBACK.json` | `acc03f11` |
| Original aggregate failure closure | `R/aggregate-master-qualification-r4/CLOSED-READBACK.json` | `a719afb3` |
| Independent latest Fast review, Astra | `R/aggregate-m2-fast64-astra-review-r1/REVIEW.json` | `d9a310c5` |
| Independent latest Fast review, Sol | `R/m2-fast64-independent-review-sol61-max-r1/READY-r2.json` | `fd1f8e3f` |
| Reliable immediate component closure | `R/reliable-immediate-qualification-r2/CLOSED-READBACK.json` | `f85b51de` |
| Provider reserve component closure | `R/provider-first-data-reserve-qualification-r6/CLOSED-READBACK.json` | `ecadfbdd` |
| Full-root witness | `R/shared-root-admission-r2` | See its closed results |
| Remaining418 normal joined closure | `R/connect-remaining418-normal-r1/CLOSED-HANDOFF.json` | `6eed157c` |
| Corrected observed Fast packet | `R/aggregate-m3-first-stall-fast64-diagnostic-r4/READY.json` | `b929662f` |

Related reports: [SIMPLERISSUE.md](SIMPLERISSUE.md), [SIMPLERISSUE-REVIEW1.md](SIMPLERISSUE-REVIEW1.md), [SIMPLERISSUE-REVIEW2.md](SIMPLERISSUE-REVIEW2.md), [MEMSTEADY.md](MEMSTEADY.md), and [PERFVAR measurements](../tests/PERFVAR-MEASUREMENTS.md).

This is an explicitly requested documentation checkpoint. It changes no production source, frozen candidate, memory policy, test deadline, or qualification gate.
