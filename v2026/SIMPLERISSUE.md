# Peer review: simpler shared admission and reliable immediate delivery

Date: 2026-11-09. This is a problem statement for independent review, not a claim that the remediation is complete.

## What we need help deciding

We previously measured five consecutive synthetic speed windows at roughly **300–400 Mbps**, with loaded small requests completing promptly. The latest source/configuration instead has very low bulk throughput and repeated multi-second request failures. Increasing physical receive permission recovered approximately 300 Mbps in the first 64 MiB window, but throughput then fell sharply across the next four windows. It did not fix the failures.

The proposed simpler direction is:

1. One shared aggregate DATA-memory admission ceiling per lifecycle owner. Protocol traffic is excluded from DATA admission. RX, TX, queues, and lanes should not strand otherwise available aggregate capacity behind arbitrary partitions.
2. A new **reliable immediate-delivery Transfer option**: retain ACKs/retries but permit opted-in messages to reach the downstream receiver without waiting for missing sequence predecessors.
3. Track successful early delivery so each message invokes the downstream delivery callback **only once**, including when the receive floor later reaches it.
4. Measure whether those changes remove the practical need for multiple hashed lanes, rather than adding more lane-selection machinery first.

Please challenge both the diagnosis and this design. We particularly need an explanation/test that distinguishes resource admission, protocol ordering, retransmission state, and actual memory ownership. A plausible theory is not sufficient; we want a deterministic reproduction and a short discriminating performance experiment.

## Required outcome and constraints

- Restore sustained bulk performance and eliminate the request timeouts, not just produce one high peak.
- Complete five consecutive synthetic speed tests and 100 overlapping page navigations. Each page loads HTML plus assets; the next navigation begins while some previous assets are still loading.
- Preserve reliable delivery custody, authentication, contracts/debits, per-message deduplication, and correct TCP lifecycle behavior. Do not fix overload by terminating a shared resident/carrier.
- The iOS profile has a **32 MiB Go-runtime qualification gate**; normal Android uses **64 MiB**. Test the iOS profile on the allowlisted Android performance devices. A physical iOS extension footprint below 50 MiB is a separate unqualified requirement; no physical iPhone is available.
- The DATA ownership budget is not the same number as the process/runtime limit. Current provider-off shared roots are 21,810,380 bytes for the 32 MiB target and 43,620,760 bytes for 64 MiB. Pool/runtime/native overhead must still fit the process goal.
- Current performance scope is hosted H1. H3, DNS-alt-service, and P2P performance expansion are deferred.
- Host activity is permitted. These are noisy diagnostic observations, not statistically promoted baselines.
- Follow [CODESTYLE.md](CODESTYLE.md): deterministic root-cause tests, adjacent-path review, nonblocking shared receive callbacks, exact ownership, and explicit cleanup.

## Verified high-throughput reference

The 2026-11-08 retained Fast5 diagnostic used actual SDK Client32/Client64 constructors with independent Desktop128 provider factories:

| Client target | Five valid windows, approximate Mbps | Loaded small-request maximum |
| --- | --- | --- |
| 32 MiB | 428, 393, 340, 320, 366 | under 554 ms |
| 64 MiB | 362, 378, 311, 394, 402 | under 535 ms |

All ten windows passed their workload gates. This was not a masking microbenchmark or a direct/non-tunnel speed result. The raw whole-measurement metric confirms the same scale; the published whole-run metric differs by less than 0.08 Mbps in those reference rows.

Important limitations: one process per profile, five correlated windows, a declared test-only SDK shim, no statistical baseline promotion, and no per-phone memory qualification. The shim's head/live diagnostic switches were dormant in the recorded child environment.

The source comparison confirms byte-equivalent workload settings and the same reported 2 Gbps / 40 ms shaped links. However, this is **not yet a matched old/new source regression experiment**. At least these boundaries changed:

- Earlier Desktop128 provider policy used encoded queue accounting and lane zero. Current policy enables exact retained-byte accounting, eight negotiated lanes, and zero effective per-sequence byte floor. The configured TX/RX totals did not change in that comparison.
- Earlier host construction used `AllowProvider=false` and host/mobile-runtime selection. The current fixture constructs the real mobile policy, remains provider-capable, then switches providing off. This enables the mobile pressure gate and changes idle topology. The pressure gate recorded zero packet drops in the compared current five-window reports; its mere presence is not proof of causality.
- Actual carrying-provider populations differed. The 64 MiB reference carried traffic on ten distinct providers versus five in the later run. Configured provider counts are not actual carrying counts.

We must separate these effects before blaming one commit or dismissing the gap as provider/host variation.

## Latest controlled receive-permission comparison

Same current production source, Desktop128 provider policy, shipping mobile construction, original workload/timers, and original loaded-request gates. The only release-code difference is three SDK files that increase physical RX permission by about 28.2%, while retaining the old encoded advertisement, aggregate root, Head, Pack, and TX ceilings. A tagged test oracle records the distinct physical/encoded limits.

| Target | Ordinary RX | Experimental RX | Aggregate root, unchanged |
| --- | ---: | ---: | ---: |
| 32 MiB | 2,181,038 B | 2,796,202 B | 21,810,380 B |
| 64 MiB | 4,362,076 B | 5,592,405 B | 43,620,760 B |

Results, one process per arm/profile, no rerolls:

| Arm | Valid windows | Invalid windows / small-request stall |
| --- | --- | --- |
| Baseline32 | 1, 2 | 3: 8.79 s; 4: 6.04 s; 5: 7.29 s |
| More-RX32 | 1, 2, 4, 5 | 3: 9.16 s |
| Baseline64 | 1, 4 | 2: 5.80 s; 3: 8.86 s; 5: 6.31 s |
| More-RX64 | 1, 2, 3, 4 | 5: 6.86 s |

The More-RX64 report's approximate whole-run throughput is **299.55 → 129.08 → 48.28 → 15.85 → 3.13 Mbps**. The fifth row is invalid and is not accepted as a completed speed measurement. Baseline64's valid windows were about 30.28 and 7.42 Mbps.

Thus more RX permission improved valid-window counts from 2/5 to 4/5 in both profiles, but **all four Fast5 roots failed**. It did not restore stable throughput, and in Client32 it did not move the first failed window. Do not adopt this permission change as a complete fix.

The experimental candidate did pass 94 focused normal/race correctness executions. All performance processes, source/module guards, resource leases, and final budget refunds closed. Correctness qualification does not turn the failed performance experiment into a success.

### What the repeated-window state does and does not show

For More-RX64, request-drained boundary samples show:

- RX leaf used: **2,275,792 → 0 → 0 → 30,544 → 30,544 bytes**.
- Head and Pack used: zero at every before/after boundary.
- Shared-root used: **2,701,368 → 396,576 → 440,076 → 340,120 → 586,620 bytes**, out of 43,620,760.
- No active origin handlers at those boundaries.
- SDK provider-window generation stayed 1, with ten quality plus two speed local providers. This is not proof of Transfer-generation/peer-instance continuity.
- Sampled RX peaks were below the old RX cap, but sampling was coarse (500 ms) and cannot exclude brief pressure.
- Provider timeout-resend event counters at after-cuts were 61, 90, 132, 697, 987. RST-class native refusal counters rose too. These are cumulative events, not current retained debt or active-flow counts.

**We do not have provider TX used, exact outstanding encoded debt, oldest-original identity/age, or per-cut active NAT/socket-close state in these reports.** We cannot name an accumulating owner from them. They do not demonstrate a client RX leak. HTTP cancellation/join occurs per window, while shared SDK/provider/NAT/transport owners remain live until all five windows finish.

A working hypothesis is persistent recovery/congestion/retention state between windows, but it remains unproved. The growing retransmit count may be a symptom rather than the initiating fault.

## Actual-device evidence

Latest completed Pixel Android64 Wi-Fi / US / H1 / provider-off Fast5: one valid 3.5 Mbps result, four approximately 90-second completion timeouts. These are Fast.com completion deadlines, not four proven socket failures.

The retained DOM polls remained nonterminal. Worker-aware CDP observations distinguish the failures:

| Window | Observed behavior at cutoff |
| --- | --- |
| 1 | No positive download bytes for 21.289 s |
| 2 | Positive download bytes 22 ms before cutoff; still streaming |
| 3 | No download bytes for 41.351 s, but control bytes continued |
| 5 | Download bytes 2.424 s before cutoff; page first response at 32.741 s and first download data at 55.574 s |

Coverage is partial. No screenshots/raw HTML/computed CSS were retained, so a UI/completion-detector problem cannot be independently proved or excluded.

Direct primitive RX gauges during windows 1/3 were nearly full: as little as 348/92 bytes free under the 4,362,076-byte RX leaf, with substantial root capacity remaining. This is a correlation, not selected-flow causal attribution. Live `clientReceiveBytes` is a composite of RX + Head + Pack and must not be compared directly with the RX-only cap. PID/session/build continuity exists, but per-budget/client-generation continuity and ACK deltas do not.

Most MAIN providers are reported to be CLI builds without an explicit memory limit. Do not generalize a constrained-provider test to those deployments. Current CLI source can still derive positive targets from host memory without `--max-memory`; deployed revision/settings remain unknown. Separate current-CLI and nil-configured-budget fixtures now pass 14 normal/race controls, but no loaded CLI workload has run. Nil configured budgets can also acquire lazy lane pools; actual constructor/sequence receipts must describe those honestly.

## Confirmed protocol-ordering mechanism

`NoBudget` currently changes memory/window admission, **not receive ordering**:

1. Automatic IP TCP sends, including pure TCP ACKs, remain Transfer-reliable (`Ack=true`).
2. The Pack gets a sequence number and a retry owner.
3. A pure ACK arriving after a missing DATA sequence is queued as future traffic.
4. It does not reach the native TCP/NAT callback until the missing DATA floor is filled.

Outer **Transfer ACKs** already bypass that queue. They are different from an **inner TCP ACK carried inside an IP Pack**.

The downstream native TCP implementation already accepts a valid pure ACK/window update independently of upload sequence position, bounds it to actually emitted bytes, and advances its window monotonically. The earlier Transfer ordering is the identified blocking layer.

Existing tests explicitly encode DATA-first delivery for future protocol Packs. Other pressure tests put controls and DATA on different lanes. Neither proves that a pure ACK can release a real NAT return buffer across a hole on lane zero.

This is a confirmed source mechanism, **not yet proof that it causes the observed Fast.com decay**. The hole's direction and actual retained NAT ownership matter; testing only outer Transfer ACKs would miss the issue.

## Proposed reliable immediate-delivery option

External implementation is in progress, uncompiled/unqualified as of this report:

- `DeliverImmediately()` requests reliable unordered delivery and retains ACK/retry semantics. Generic DATA using it remains DATA-budgeted; it does not imply `NoBudget`.
- Pack field 14 requests immediate delivery. A separate terminal-immediate ACK outcome and optional ordinal identify an actually delivered original without granting a false cumulative prefix.
- Old decoders ignore the new fields and retain ordered behavior. This fallback needs real compatibility tests, not only a schema argument.
- Normal source/session/encryption/contract/debit authority remains mandatory. An unknown or future contract cannot be activated early merely because a message requests immediate delivery.
- Successful delivery state survives payload return, receive-floor advancement, duplicate arrivals, and relevant same-sequence worker recreation. Later head retirement must not call the application again.
- The marker cannot point into a pooled `receiveItem`: returning the payload can recycle that owner.
- A callback refusal is not delivery. No terminal ACK or success marker follows a refusal.
- A future terminal receipt releases only that original. It must never cumulatively ACK missing DATA.

Two important reliability details are being addressed:

1. **Lost immediate ACK:** after a long train behind a DATA hole, retransmitting a middle original must recover its terminal receipt without another delivery. A compressed range storing only its boundary IDs cannot authenticate arbitrary random interior IDs. The current design uses a domain-separated identity for new immediate originals, derived from source/sequence/ordinal; ordinary IDs remain unchanged. Fixed receipt ranges compact consecutive trains and fall back safely when sparse capacity is exhausted. This needs adversarial identity, loss, replay, and memory tests.
2. **Later cumulative ACK:** if n+1 was terminally retired early, a later normal cumulative ACK naming n+1 must still retire predecessor n once it is delivered. Looking up only a still-retained n+1 payload would incorrectly ignore that ACK. Bounded authenticated terminal-boundary state must preserve this authority.

Protocol metadata must remain bounded. A new in-flight-count restriction must also avoid creating an ACK-bandwidth ceiling at high RTT. Measure count waits, batching, allocation and throughput; do not blindly reuse a small lane depth as the limit.

Initial automatic IP opt-in is **validated pure TCP ACK only**. SYN/FIN/RST remain ordered pending separate lifecycle proof. Earlier deterministic tests found that an early SYN overtaking prior same-flow DATA can misattribute bytes to a new TCP cohort. Generic API callers may intentionally opt in other messages, but that is not proof that every automatic IP control is safe. First-page SYN delay and DATA-to-DATA head-of-line blocking remain open coverage.

## One aggregate resource versus restrictive leaves

The intended policy is one shared DATA resource, not fixed per-lane allocations. Current mobile code still has restrictive RX/TX/Pack/role children under a shared root, so spare root bytes do not necessarily admit an RX owner. Multiple lanes already share a TX pool with zero effective floor; removing lanes alone does not remove those admission constraints.

Removing leaf ceilings must also preserve progress at a completely full root. A source-reviewed, not-yet-executed counterexample exposes a potential gap:

- Fixed root: 55,296 bytes from construction, two signed lanes.
- Each real selectively acknowledged future tail costs 5,120 bytes, with a 5,120-byte missing-prefix promise and 17,408-byte proof-repair escrow.
- Those actual owners/promises exactly fill the root.
- A same-size missing current owner should use its paid promise.
- A 30,544-byte missing current owner is larger than the observed-tail-based promise and may require unavailable fresh root capacity, despite the future already having a selective lease.

No fake filler, live cap reduction, eviction, or timer constructs this witness. The first execution compiled but failed in fixture setup before pressure: a reused sender lookup only accepted lane zero, while this witness uses lanes one/two. An exact lane-aware fixture correction is being prepared; no runtime root-full verdict exists yet. Root-only SDK admission remains held until full-root progress is proved.

A valid repair may refuse a future **before** selectively acknowledging it if repair cannot be funded. It cannot discard already acknowledged custody, invent free head credit, or expand the hard root. The accepted receive envelope is not universally the sender's usual 32 frames / 16 KiB H1 grouping limit: the receiver currently accepts more shapes. A repair bound must be enforced or authenticated and include actual retained graph/pool costs.

## Separate budgeted-provider finding

A signed two-flow test established B's TCP handshake and then held A's real originals. A filled approximately 5.74 / 5.75 MB of shared provider TX while encoded debt remained below the normal wire window. B's fresh DATA had no wire admission. Adaptive smaller chunks alone failed: A consumed residual room, leaving 5,000 bytes against an actual B minimum owner demand of 8,228.

A private bounded first-DATA reserve candidate is being tested under the unchanged TX ceiling. It has explicit credit/ACK/item lifetimes, exact-flow ownership, ordinary-admission fallback, and per-flow-order-preserving selection for same-lane collisions. It must not be claimed as the MAIN CLI cause or adopted merely because its component passes.

A draft serialization bug omitted two prepaid-provenance fields during an existing whole-item rewrite. It caused partial payload progress and retained-credit leakage; the fields are now preserved and the regression is rerunning. Latest attempted runs still do not qualify the full candidate. This is a bug in the unshipped candidate, not established causality for the earlier Fast.com failure.

This reserve adds complexity. If shared admission plus immediate protocol delivery resolves the problem without it, prefer the simpler measured solution. Do not retain overlapping mechanisms solely because work has been invested in them.

## Targeted test plan

The short suite is source-frozen, not yet compiled/run. All cells use the same public fixed-provider selection so different exits cannot disguise a lane comparison. Actual mobile32/64 constructors and one identical Desktop128 TX pool/floor are retained.

- Factor accounting: encoded-only legacy **unsafe diagnostic** versus exact retained accounting.
- Factor lanes: actual lane zero versus eight, applied to the generated client window as well as provider configuration. Provider return traffic uses explicit client TransferKeys; changing provider lane count alone does not prove a lane-zero experiment.
- Short bulk traffic plus exact 2 KiB GETs on existing and fresh connections, actual cancellation/join/restart, current budget/debt/waiter/ACK/retry cuts, byte identity, and complete ownership refunds.
- A forced hash-collision/idle-alternative case and an all-lanes-busy control. Existing flows must not move lanes mid-session.
- A signed lane-zero DATA-hole test: retain real NAT B return bytes, withhold client DATA n, deliver pure ACK n+1 immediately, and prove actual NAT refund/window update plus exact B terminal receipt before releasing n. Duplicate before/after head must not duplicate callback, refund, or debit; A's original must not be falsely acknowledged.

An optional observation path was found to block the measurement callback; that is being corrected before qualification. Legacy accounting results cannot justify a production mode that undercounts retained memory.

Iteration order: deterministic mechanism/ownership controls → short targeted suite → original full100 and five retained speed windows → real-device Fast.com and eight MEMSTEADY cells (two phones × Wi-Fi/cell × 32/64 profiles). Final full correctness is also required. Both baseline15 full100 profiles currently pass 100 HTML / 400 assets / 99 overlaps, with median TTFB 896/1,053 ms and worst TTFB 2.72/4.61 s; final combined-source reruns remain outstanding.

## Questions for the reviewer

1. Which smallest experiment best separates provider accounting from lane ordering and the changed mobile constructor? What could produce the repeated-window decay after client RX drains?
2. Is the retained-cost model over-reserving serial scratch/rewrite capacity per original? Send accounting currently includes three rounded roots plus graph/owner metadata (five roots for legacy wrapping). Which costs are simultaneously live, which are future promises, and which could safely be shared without a full-root deadlock?
3. Does the proposed reliable immediate receipt/identity design preserve exact delivery and cumulative ACK authority under lost/reordered ACKs, duplicate IDs, cancellation, and worker recreation? Is there a simpler existing primitive we should reuse?
4. How should protocol in-flight metadata be bounded without throttling ACKs or putting them behind DATA admission again?
5. What is the simplest full-root gap-repair guarantee compatible with one shared resource and existing selective custody? Is safe refusal before SACK preferable to introducing more reservation/token machinery?
6. After protocol delivery is fixed, do lanes still improve meaningful DATA isolation? Would fair scheduling of unrelated unmaterialized flows be sufficient, and where does already-numbered reliable ordering remain unavoidable?
7. What missing current-state measurement would discriminate provider retention, flow cancellation, transport RTO/cwnd, and routing/provider changes? Specify exact owner/generation/direction joins; historical maxima and aggregate counters are insufficient.
8. Are we overlooking a simpler aggregate fix? Please identify the invariant, a deterministic old-source failure, and a small performance test that would falsify your explanation.

Please avoid extending deadlines, weakening success gates, moving established flows between exits, dropping paid originals, or inventing a TLS/HTTP status diagnosis from encrypted traffic.

## Source and evidence pointers

Repository context: [MEMSTEADY.md](MEMSTEADY.md), [LOWBULKFLOW5.md](LOWBULKFLOW5.md), [PACKETFLOW-STALL.md](PACKETFLOW-STALL.md), [PAGESTALL-PROBLEM.md](PAGESTALL-PROBLEM.md), and [PERFVAR measurements](../tests/PERFVAR-MEASUREMENTS.md). Historical documents contain earlier hypotheses; the scoped facts above take precedence for this review.

Private host evidence (paths may not exist on another reviewer's host; the report above is intended to stand alone):

| Evidence | Location / SHA-256 prefix |
| --- | --- |
| Verified high-speed Fast5 | `/Volumes/data/urnetwork/synthetic-smoke-r1/recovery-performance-r1/RESULTS-runtime-r1.json` — `29d92b62` |
| Historical/current source comparison | `temp/aggregate-flow-r15-r2/historical-fast5-comparison-r1/READY.json` — `9e249461` |
| Current baseline release-output map | `temp/aggregate-flow-r15-r2/source-update-r6/PRODUCTION-MAP.json` — `2445aba4` |
| Closed four-case RX comparison | `temp/aggregate-flow-r15-r2/receive-headroom-fast5-pair-r1/run/FINAL.json` — `6500f61e` |
| Window-state readback | same experiment, `analysis/WINDOW-STATE-READBACK.json` — `e1270ef1` |
| Device timeout readback, 46 rehashed inputs | `temp/ordering-fixed-fast5-r1/actual-r1/published-readbacks/timeouts-final/TIMEOUT-READBACK.json` — `4b307415` |
| Protocol ordering audit | `temp/aggregate-flow-r15-r2/protocol-immediate-source-audit-r1/READY.json` — `0cd35975` |
| Independent immediate-delivery review | `temp/immediate-delivery-review-20261109/r1/REVIEW.json` — `e8ac29e6` |
| Short accounting/lane suite | `temp/aggregate-flow-r15-r2/provider-accounting-lanes-perf-r1/READY-r2.json` — `4b742f56` |
| Signed NAT ACK-hole witness | `temp/aggregate-flow-r15-r2/provider-immediate-ack-hole-r1/READY.json` — `affc8bb6` |
| Full-root counterexample packet | `temp/aggregate-flow-r15-r2/shared-root-admission-r1/READY.json` — `64335962` |

Relative `temp/` paths in that table are under `/Volumes/data/urnetwork/`. All new implementation candidates remain external to the frozen source/native artifacts. This document is an explicitly requested documentation-only checkpoint, not a qualification or release commit.
