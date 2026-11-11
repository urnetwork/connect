# Peer review 3: retaining 300+ Mbps without receive-admission stalls

Date: 2026-11-09. This report supersedes the status in [SIMPLERISSUE2.md](SIMPLERISSUE2.md). It describes measured mechanisms and an unfinished remediation, not a release-qualified solution. Please follow [CODESTYLE.md](CODESTYLE.md).

## The question for reviewers

We now have two concrete findings:

1. Two real Transfer sequences can remain within their individually advertised encoded windows while exhausting their shared physical RX budget. A real original is refused and recovers only four seconds later.
2. A cumulative ACK that is stale at the recovery decision can authorize a timeout deferral from two to four seconds. An existing signed-transfer test fails its full-payload bound with that deferral and passes when deferral is disabled diagnostically.

A small advertised-window experiment almost eliminates receive-side drops, but reduces bulk throughput to approximately **22–26 Mbps**. The unchanged baseline can reach **306 Mbps**, with thousands of receive-side drops in its slower windows. We need a fix that retains high sustained throughput and prompt small requests, rather than choosing between losses and an artificially small flight.

The intended direction remains **one shared DATA resource, protocol excluded from DATA admission, and reliable immediate delivery for opted-in protocol messages**. The current aggregate still has restrictive RX/recovery/TX children and lanes; it is not yet a complete implementation of that simpler resource model.

Please help identify the smallest complete next change, with a deterministic failure and a short falsifying experiment. In particular, challenge how to reconcile encoded sender permission with complete physical retained ownership across multiple active senders.

## Constraints that must survive

- iOS-surrogate Go-runtime gate: **32 MiB**. Android profile: **64 MiB**. Test the iOS profile on the allowlisted Android devices; no physical iPhone is available to prove an extension footprint below 50 MiB.
- Current provider-off DATA roots are **21,810,380 / 43,620,760 bytes** for 32/64 profiles. These ownership budgets are not the process/runtime limits.
- Use available shared capacity efficiently. Do not replace one restrictive partition with arbitrary per-lane or per-flow partitions that strand idle capacity.
- Keep protocol outside DATA admission. This does not make every protocol original or receipt physically free or automatically bounded.
- Keep Transfer reliability for immediate delivery: exactly-once downstream delivery, real receipt authority, authentication, contract/debit checks, and correct cancellation/refunds.
- Initial automatic immediate IP delivery is validated pure TCP ACK only. SYN/FIN/RST remain ordered.
- Do not discard already acknowledged custody, invent free headroom, move established flows between exits, block the shared receive callback, or tear down a resident on ordinary pressure.
- Performance scope is hosted H1. H3/DNS-alt/P2P expansion is deferred. Keep the five-second loaded-request gate and full100 workload/oracles unchanged.
- All numbers below are noisy diagnostic observations, not statistically promoted baselines. Source/cleanup qualification is separate from performance qualification.

## Current performance: three relevant measurements

### Historical high-throughput reference

The earlier synthetic Fast5 reference used actual SDK client profiles with Desktop128 providers:

| Profile | Five valid windows, approximate Mbps | Maximum loaded-request latency |
| --- | --- | --- |
| 32 MiB | 428, 393, 340, 320, 366 | under 554 ms |
| 64 MiB | 362, 378, 311, 394, 402 | under 535 ms |

This was a tunnel workload, not a direct-path or microkernel benchmark. However, it is not a matched old/new causal comparison: encoded versus exact retained accounting, lane configuration, mobile constructor policy, and carrying-provider populations changed. Do not restore an undercounting mode merely because that reference was faster.

### Latest unchanged-policy baseline

The latest corrected observed baseline used the same M3 production as the earlier failing measurements. It completed all five windows without crossing the five-second loaded-request gate:

| Window | Whole-run Mbps | Same-instance client Pack-handoff drops | Provider timeout retries | Provider timeout deferrals |
| --- | ---: | ---: | ---: | ---: |
| 1 | 39.68 | 2,691 | 1,667 | 3,412 |
| 2 | 70.12 | 1,666 | 1,276 | 1,591 |
| 3 | 305.94 | 311 | 14 | 0 |
| 4 | 231.05 | 36 | 14 | 218 |
| 5 | 254.68 | 20 | 52 | 206 |

Maximum measured first payload was **2.268 seconds**. Twelve actual retained Client instances matched across each window's start/end. An additional new client in windows 1 and 4 was excluded from subtraction; its observed end drop count was zero. Missing or recreated identities are not treated as zero.

All ten coherent RX snapshots report **4,362,076 bytes** of RX capacity. Actual RX end use was approximately 4.351 MB, zero, 4.227 MB, zero, zero. Thus an end-cut RX level alone does not distinguish slow from fast: window 3 was fast despite a nearly full end cut.

Matched-client receive-queue, decode-pressure, and ACK-drop counters were zero. Provider Pack drops were 0 / 0 / 0 / 0 / 1. Provider selective-gap recoveries were 1,077 / 873 / 310 / 39 / 19. No request exceeded five seconds, so this run supplies no selected stalled-flow capture.

Earlier unchanged-source runs remain failures, including **36.77 → 9.58 → 7.12 → 5.13 → 3.05 Mbps** with 5.67/9.48-second request stalls, and **41.43 / 12.92 / 15.88 / 43.46 / 303.58 Mbps** with two invalid windows. The newer passing run does not erase them or establish that a production fix occurred.

### One-setting encoded-window experiment

The experiment changed only the existing client `AdvertisedReceiveWindowMaxByteCount` fixture setting to **16,385 encoded bytes**, against the exact corrected M3 baseline. It retained physical budgets, target64 construction, Desktop128 providers, timer settings, workload, and success gates.

| Window | Baseline Mbps | Cap Mbps | Baseline client drops | Cap client drops | Baseline provider timeout retries | Cap timeout retries |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| 1 | 39.68 | 26.06 | 2,691 | 10 | 1,667 | 20 |
| 2 | 70.12 | 22.53 | 1,666 | 1 | 1,276 | 10 |
| 3 | 305.94 | 21.50 | 311 | 0 | 14 | 11 |
| 4 | 231.05 | 22.48 | 36 | 1 | 14 | 7 |
| 5 | 254.68 | 26.21 | 20 | 0 | 52 | 7 |

All five cap windows passed. Maximum measured first payload was **1.630 seconds**. All ten normal/race fixture controls passed, and source/module checks, joins, leases, and image cleanup closed successfully. The matched baseline also passed all five windows, so this pair does not demonstrate curing a timeout; earlier failing runs remain separate evidence.

This is a useful discriminator, **not a shipping improvement**. It dramatically reduces receive-side drops/retries while imposing a low flight ceiling. It supports admission pressure as a mechanism, but does not prove that all earlier stalls have the same cause. There is one process per treatment, not a randomized statistical comparison.

Why 16,385? The actual H1 sender envelope is bounded at 16,384 bytes, and the generic window comparator uses strict `<`. But the actual send loop supplies **arrival=0** to that comparator before serializing the next original. Consequently, a learned cap can be crossed by one complete original. The cap is not a strict grant for the next frame, nor a guarantee of total physical memory.

At the ten traffic boundaries, **78–91 current provider sequences** had learned the cap, but only **7–8 had nonzero encoded queue debt**; 6–8 were already at/above their effective gate. Summed encoded debt was only approximately 142–169 KB. These are residual queue observations, not a complete active-producer census, but they show why dividing capacity equally among all live sequences could strand most capacity in idle owners. Sender TX retained charges are not the client RX cost.

The experiment observed 1,024 receiver-advertisement samples and 882 current known provider-gate samples, plus two unknown samples. First-wire history is loss-qualified and incomplete across generation replacement. It does not prove every startup write had already learned the cap. The aggregate number of possible lanes/generations and one-original overshoot must not be ignored in any safety calculation.

## What the drop counters mean

The new measurements close a harness gap: the client APIs existed, but previous reports did not retain their values. The corrected fixture takes actual start/end snapshots in every window and differences only known retained Client instances.

Do not equate `PackHandoffDropCount` with a single cause:

- Failure to allocate a complete retained RX/recovery owner increments it.
- Some Pack-handoff/quota/bind/channel failures and closing/replacement-generation paths also increment it.
- `ReceiveDecodePressureDropCount` instead covers the earlier carrier/decrypt reservation path. Its being zero does not prove retained RX admission succeeded.
- H1 saturation increments when the sequence count/byte limit invokes adaptive deepening. Deepening can succeed; other failures can drop without a saturation event.

Therefore handoff drops minus saturation is **not** an exact physical-RX refusal counter. The runtime signed witness below establishes one exact admission mechanism independently; the bulk counters show correlation, not a unique per-drop classification.

## Runtime proof: encoded permission does not pay physical ownership

Two real signed public sequences learn windows through actual ACKs. They share one RX budget; the test uses real originals and ownership, no fake filler, cap shrink, seeded peer window, or fabricated receipt.

At the refusal:

| Quantity | Actual value |
| --- | ---: |
| Each advertised/effective encoded window | 61,088 bytes |
| Sender encoded debts | 31,256 and 20,844 bytes |
| Total encoded debt | 52,100 bytes |
| Physical RX used / capacity | 61,088 / 61,088 bytes |
| Incoming original encoded / retained cost | 10,412 / 30,544 bytes |
| Root used / capacity | 61,088 / 1,048,576 bytes |
| Recovery child used / capacity | 0 / 61,088 bytes |
| Predecode refusals / false receipts | 1 / 0 |

Both senders obey their learned encoded windows. Total encoded debt is even below the shared RX capacity, yet physical RX is full and a legal incoming original cannot be admitted. **A global encoded split alone does not solve the retained-cost mismatch.**

The same actual original recovers on a natural retry after **four seconds**. Both clients join; RX, recovery, root, and TX charges all refund to zero. This witness demonstrates a mechanism, not a shipping repair.

A separate actual small-cap control learns 16,385 from real ACKs. Two raw8 originals can cross that cap to 20,844 bytes of debt; the third stays off wire until a real prefix ACK reopens admission. The sibling still progresses, no physical refusal occurs, and all resources refund. This preserves the actual one-original overshoot rather than pretending the current gate is strict.

## Runtime proof: stale progress grants an extra two-second deferral

The existing signed provider test establishes a second flow behind held originals while retaining the first flow's ownership. Its unchanged pending-protocol companion expects all **65,536 native bytes** within four configured observation intervals, approximately eight seconds. This is distinct from the primary test's first-due sentinel.

The causal diagnostic observes actual sender application, not merely ACK forwarding or a terminal-immediate retirement hook:

1. Cumulative ACK through #3 is fully applied at time zero; the prefix is trimmed, and DATA #4 is the current retained original with `sendCount=1`, `timeoutDeferCount=0`.
2. The default first recovery decision occurs at 2 seconds.
3. The default code grants a deferral, sets `timeoutDeferCount=1`, and re-arms at 4 seconds despite no newer cumulative progress.
4. The exact DATA #4 retries at 4 seconds. The default companion fails: **42,880 / 65,536 bytes**, with wire/native ownership still retained.
5. A diagnostic arm changing only `DeferTimeoutResendWhileCumulativeProgress=false` retries #4 at 2 seconds and completes **65,536 bytes**, with native retained bytes zero.

The overall diagnostic is **2 PASS / 1 FAIL**, fully closed. The failing default is preserved, not reclassified as an expected passing test. The initial two-second floor is separate from the extra two-second deferral. A Head flag on the retry is a side effect, not proof of missing Head admission authority.

### Narrow correction being qualified

The old per-item predicate accepts progress near the original send:

```go
lastCumulativeAckTime.After(item.sendTime.Add(-scaledRtt))
```

That does not establish freshness at the actual recovery decision. The new source adds a guard using the already sampled decision timestamp:

```go
cumulativeAge := sendTime.Sub(self.lastCumulativeAckTime)
recentCumulativeProgress := 0 <= cumulativeAge && cumulativeAge < scaledRtt
if recentCumulativeProgress && self.shouldDeferTimeoutResend(item, scaledRtt) {
    // Existing deferral, ownership and rescheduling logic.
}
```

Here `sendTime` is the recovery decision's timestamp, not `item.sendTime`. No new clock read or configured interval is added. Deferral remains enabled. Existing per-item, since-last-deferral, FIFO/carrier, and lane-verdict rules remain.

This source is frozen but **not yet runtime-qualified** at this report's cut. Tests are being completed for stale/equal/fresh progress, SACK versus cumulative evidence, future-clock values, healthy queued drains, and custody. The original uninstrumented signed primary/pending tests must pass with default deferral ON before claiming this remedy works. Global deferral-off remains a diagnostic, not a proposed default.

## Qualified narrower fix already in the aggregate

An actual signed decoder test held a published current predecessor paid by ordinary RX. A real foreign tentative tail filled RX. The contiguous successor was refused even though recovery/root bytes were available, because the prefix helper recognized only recovery-paid predecessors.

The narrow correction accepts the **exact configured ordinary RX identity** as well as the existing recovery identity. It retains same-generation, published consumer ownership, no-loan, bounded consecutive-bitmap, and complete atomic recovery/root charge requirements. Arbitrary sibling budgets and promise loans remain excluded.

The decisive unchanged-clock comparison:

- Old: RX 6,656; recovery 0; one refusal.
- Fixed: RX 6,656; recovery 5,120; root 11,776; no refusal.

**22 normal + 22 race tests passed**, and two isolated old-guard controls reproduced the sole typed failure after cleanup. Mixed RX/recovery prefixes, failed predecessor refunds, gaps, cancellation, and physical caps are covered. All guards/resources closed. This fixes a proven unnecessary retry boundary; it does not create aggregate credit or establish a bulk-speed cure.

## The aggregate and what is still missing

All completed implementation fixes are consolidated in one source lineage. Current frozen aggregate authority is `aggregate-core-reserve-immediate-r5/READY.json`, SHA prefix **42f2397f**: 38 production files, 161 default mappings. It includes:

- Reliable immediate-delivery/receipt and exactly-once ownership work.
- Bounded future protocol-reorder counting.
- Budgeted-provider first-DATA/mixed-credit ownership repairs.
- The qualified ordinary-RX prefix correction.
- Repaired real ControlId routing, pool-statistics test lifecycle initialization, signed advertisement witnesses, and regression tests.

Capture is off by default; observation and signed-ACK diagnostics are separate overlays on the same source. The freshness correction above will join this same aggregate lineage, not become another alternative product branch.

Important limits:

- One shared DATA root still has restrictive children. Complete shared-resource admission is unfinished.
- Reliable immediate component tests passed, but the complete repaired SDK cadence/lifecycle and full-B integration qualification is not green yet.
- Protocol excluded from DATA admission is not proof that serialized reliable originals and receipt metadata have no growth risk. Raw send-token release is not necessarily lifetime release of serialized owners.
- The full-root witness remains: an equal 5,120-byte current owner can use a paid promise, while a 30,544-byte current owner can fail at a full 55,296-byte root. Credit design must address current-size uncertainty and acknowledged future custody, including legacy peers.
- Selected 418 Connect roots passed normally and under race on the earlier source. This is not full correctness qualification of the latest aggregate. All six maintained repository tracks remain required.
- Fresh aggregate full10032/64 and Fast5 32/64 are prepared, not completed. Earlier baseline15 full100 passed 100 HTML / 400 assets / 99 overlaps, max TTFB 2.72/4.61 s; those results cannot qualify the new aggregate.
- No fresh aggregate device Fast.com or eight-cell MEMSTEADY result exists. The last completed Pixel Fast5 was one 3.5 Mbps result plus four completion timeouts, with mixed evidence of ongoing streaming. Do not compare it causally with local Desktop128 throughput.

## What we need the next design to explain

### Shared credit must represent complete retained cost

`free / activeSequences` in encoded units is not a physical guarantee. A correct design must account for:

- Complete legal original cost, including metadata, pooled roots, encryption/wrapping, and potentially different sender/receiver allocation policy.
- Existing grants and originals already in flight; an ACK shrinking a reported number cannot retroactively reclaim those bytes.
- SACKed future custody and the largest missing current original that must still progress.
- One-original overshoot in the current send admission path, and writes before a peer learns an advertisement.
- Generation/lane/role creation, cancellation, quiet owners, and returning unused permission without stranding the shared resource.
- Compatibility with existing peers that interpret the current field only as encoded debt.

An empirical retained/encoded ratio is not a worst-case safety bound. A fixed equal allocation can remove loss while recreating the low-throughput/stranded-capacity problem. We want to use the whole budget effectively, not reserve an idle slice for every possible lane.

### Refusal feedback is promising but must not become a false receipt

Existing eviction recovery applies to selectively acknowledged originals. Reusing those entries indiscriminately for pre-admission refusal risks a delayed refusal revoking a later valid SACK.

A new advisory, if needed, must be authenticated and generation-scoped, refer to an exact still-live unsacked original, preserve callbacks/bytes/lifetime, remain bounded/coalesced/rate-limited, and be inert after SACK/retirement/cancellation or on an unknown identity. It must not fabricate cumulative progress or acknowledgment of the refused payload. A scanner finding a source/contract field is not itself proof authority.

Please identify a compatibility-safe wire representation and a trustworthy emission point, or explain why existing mechanisms can solve this without another wire extension.

## Questions for reviewers

1. Does the decision-time freshness correction preserve the intended healthy-deep-queue protection? Which deterministic case could show an unacceptable retransmission increase while fresh progress exists?
2. What is the simplest global admission/credit mechanism that prices retained ownership and uses otherwise idle capacity? Can it work with current peers and wire fields, or does it require explicit capability negotiation?
3. Can sender admission price the next complete original before publication, eliminating the measured overshoot without blocking protocol? Which batching/contract/encryption costs are unknown at that point?
4. How should credits be issued and reclaimed without overcommitting old grants, revoking paid custody, or starving a new/quiet flow? Give explicit invariants, not just a free-space division formula.
5. Is authenticated refusal feedback the smaller first repair for multi-second tails? How would it coexist with SACK, ACK compression, duplicated/reordered notices, old decoders, and memory pressure?
6. Does the cap experiment justify prioritizing physical admission over lane count, provider TX retention, or ACK-slot pressure? What one further measurement would most cheaply falsify that ranking?
7. How can we remove the now-overlapping promise/recovery/reserve mechanisms after a shared solution proves equivalent? Which legacy-peer fallback remains necessary?
8. What short aggregate experiment should precede the full five-window/full100/device suites? Please specify byte identity, no false receipts, bounded memory, throughput and latency outcomes, and an old-source failure.

Please challenge the diagnosis. We have stronger evidence now, but no complete performance fix. Avoid merely raising memory limits, weakening deadlines, removing exact charges, or treating a binding tiny window as acceptable performance.

## Evidence for local reviewers

`R` means `/Volumes/data/urnetwork/temp/aggregate-flow-r15-r2`. Reports above are intended to stand alone when these private paths are unavailable.

| Evidence | Path | SHA-256 prefix |
| --- | --- | --- |
| Historical high-throughput reference | `/Volumes/data/urnetwork/synthetic-smoke-r1/recovery-performance-r1/RESULTS-runtime-r1.json` | `29d92b62` |
| Corrected baseline closed result | `R/aggregate-m3-first-stall-fast64-diagnostic-r5/CLOSED-READBACK.json` | `3f71d804` |
| Baseline actual counter readback | `R/aggregate-m3-first-stall-fast64-diagnostic-r5/ACTUAL-COUNTER-READBACK.json` | `c4bcb0a0` |
| Independent baseline reduction | `R/m2-fast64-independent-review-sol61-max-r1/M3-R5-COUNTERS-READBACK.json` | `22919816` |
| Cap source | `R/receive-advertisement-cap-diagnostic-r1/READY.json` | `56b07ba6` |
| Cap closed result | `R/receive-advertisement-cap-runtime-r1/CLOSED-READBACK.json` | `18b66d18` |
| Cap final | `R/receive-advertisement-cap-runtime-r1/run/FINAL.json` | `ea3456b5` |
| Signed two-sequence witnesses | `R/receive-shared-advertisement-qualification-r1/CLOSED-READBACK.json` | `9f4288ed` |
| ACK-timing causal reduction | `R/provider-first-signed-ack-application-qualification-r1/ACK-TIMING-READBACK.json` | `2d153300` |
| ACK diagnostic closed failure | `R/provider-first-signed-ack-application-qualification-r1/CLOSED-READBACK.json` | `c38d0cf3` |
| Qualified prefix fix | `R/receive-ordinary-prefix-qualification-r1/CLOSED-READBACK.json` | `d6586342` |
| Current aggregate | `R/aggregate-core-reserve-immediate-r5/READY.json` | `42f2397f` |
| Frozen freshness product draft | `R/timeout-progress-freshness-r1/READY.json` | See its source/diff pins |

Production candidate sources are still private donor files named in the aggregate overlay/production map, rather than changes applied to canonical `main`. Review the effective mapped files, not only the canonical tree. This remains a reviewability limitation and an eventual checkpoint requirement.

Related context: [SIMPLERISSUE2.md](SIMPLERISSUE2.md), [review 1](SIMPLERISSUE2-REVIEW1.md), [review 2](SIMPLERISSUE2-REVIEW2.md), [MEMSTEADY.md](MEMSTEADY.md), and [PERFVAR measurements](../tests/PERFVAR-MEASUREMENTS.md).

This documentation-only review request changes no production source, running candidate, memory gate, timeout, or workload success criterion.
