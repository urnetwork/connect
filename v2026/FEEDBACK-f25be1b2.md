# Review of f25be1b2 and follow-up packet-flow fixes

Reviewed 2026-11-04 by Astra max against the four commit diffs and Connect `49f285f1edd4bac4fd0273c5dfdc05a798fa4c97`. This is a source review: no new tests or benchmarks were run for this review. Existing test names below describe coverage, not a new execution claim.

## Recommendation

**Keep the direction and the four-commit series; do not revert it wholesale.** The changes address credible premature sequence expiry, resend amplification, and unrelated-flow cancellation. They are not a complete remediation: the initial review found two adjacent failure paths, and follow-up execution confirmed a third lookup/cancellation path. Several original tests check helpers rather than the composed failure.

| Commit | Decision | Reason |
| --- | --- | --- |
| `f25be1b2` | Keep with its follow-ups; complete the pending-ACK case. | A progressing cumulative head is evidence that the sequence is draining. The implementation misses progress that has arrived but has not yet been applied by `Run`. This commit should not be taken alone without the next two corrections. |
| `1c0e74fb` | Keep. | Keeping applied progress out of the pending-feedback channel prevents the forced-expiry scan from repeatedly reconsidering the same renewal. |
| `77b2dd2d` | Keep; strengthen tests. | Resend scheduling must use the effective lifetime, otherwise an already-expired original deadline defeats backoff and causes repeated writes. |
| `b74a5089` | Keep; extend the refusal classification to memory pressure. | A regenerable orphan reset refused by an open return path should not cancel an otherwise useful incoming sequence. An admitted reset must still wait for its real Transfer ACK. |

No new runtime correctness regression introduced by these four commits was confirmed by this source review. The findings below are **incomplete fixes of pre-existing failure paths**, not evidence that reverting would improve behavior. This review does not qualify Fast.com speed, the 100-page workload, or mobile memory usage.

## Findings

### P2: Pending cumulative progress can still expire an uncovered tail

Locations: [transfer_ack_lifetime.go:145](transfer_ack_lifetime.go#L145), [transfer_ack_lifetime.go:177](transfer_ack_lifetime.go#L177), [transfer_window_pacing.go:561](transfer_window_pacing.go#L561), and the pre-snapshot expiry and ACK application in [transfer.go:9474](transfer.go#L9474).

`ackProgressDeadline` reads `lastHeadAckTime`, which the send loop updates after taking its ACK snapshot. The nested pacing wait deliberately consumes only the ACK wakeup and leaves feedback pending. The expiry scan can recognize that a pending cumulative ACK delivered an earlier item, but does not use that head's advancement to renew a later, uncovered item.

A concrete ordering is two outstanding items with original deadlines at 30 seconds, a valid cumulative ACK covering the earlier item arriving at 29 seconds, and the send owner remaining inside a pacing reservation past 30 seconds. `nextAckLifetime` removes the covered prefix, then expires the uncovered tail using stale applied progress. The ordinary sequence can consequently cancel despite useful recent progress. The retained-item expiry check before snapshot application has the same missing evidence.

Reconcile valid pending head advancement before deciding tail expiry. Anchor renewal to the actual ACK arrival, not scan/application time. Unchanged, stale, unrelated, or repeatedly inspected feedback must not indefinitely renew a sequence. Preserve the separation introduced by `1c0e74fb`; simply feeding applied progress back into the pending-feedback result would restore its scan-loop problem.

### P2: Orphan reset memory refusal still cancels the shared incoming sequence

Locations: [ip.go:9968](ip.go#L9968), [ip.go:9986](ip.go#L9986), [ip.go:10067](ip.go#L10067), [ip_provider_memory.go:297](ip_provider_memory.go#L297), [ip_provider_memory.go:395](ip_provider_memory.go#L395), and [transfer_receive_delivery.go:296](transfer_receive_delivery.go#L296).

The new `releaseReturnItem` refusal path is reached only after reset scratch and return-item memory have been admitted. If either allocation reservation fails while the provider remains open, the function returns while `pendingTerminalReset` is still attached to the defer. That defer calls `complete(false)`, which rejects the reliable receipt and cancels the incoming sequence. The send-queue fix therefore does not cover the same failure under memory pressure.

One concrete trigger is an already-admitted reliable packet waiting behind a full TCP queue. Exhaust the remaining shared provider/NAT budget, retire that TCP owner, and wake its capacity retry. The packet becomes an orphan, but reset scratch admission fails. Unrelated traffic sharing its incoming sequence can again be canceled. This ordering is established by source inspection; an executed reproduction remains required.

Classify these open-provider capacity refusals using the same regenerable disposition. Do not blindly change the common defer: shutdown, canceled source, malformed/security-rejected input, and unsupported recovery modes must retain their explicit failure behavior. Ordinary retained socket payloads must never acquire synthetic delivery success.

### P2: TCP retirement between lookup and send can reject the shared sequence

Follow-up execution confirmed this adjacent, pre-existing path in `TcpBuffer.tcpSendWithOwner` (reviewed `ip.go` around lines 4847–4901). A reliable payload retry can look up a live TCP sequence, then lose that child before `sequence.send`. The send returns `Done`; the existing retry/recheck handles only an empty terminal control. Payload consequently fails its receipt and cancels the shared incoming `ReceiveSequence`, even though the provider, NAT, and sibling TCP flow remain healthy. This path occurs before orphan reset generation, so `b74a5089`'s return-admission handling cannot cover it.

The original queue-saturation fixture could expose this second ordering accidentally: filling the return queue repeatedly releases callback scratch; budget release wakes retained ingress retries, and flow cancellation can then race their lookup. The revised test separates these causes. Its quiescent return-queue control passed, while `TestProviderReliableOrphanPayloadRetirementAfterLookupPreservesSameSequence` uses the existing `beforeSequenceSendForTest` barrier and fails deterministically on unchanged production with `sequence=context canceled`, healthy provider/NAT/sibling contexts, and `afterLookup=1`. This was an assertion failure, not a compiler error, panic, or timeout.

Handle the retired generation through an authoritative flow/lifecycle recheck and the existing orphan disposition. Do **not** widen the empty-control retry into sending old application payload to a newer same-tuple TCP generation. Replacement, source retirement, NAT/provider shutdown, real ACK custody, and healthy same-sequence sibling progress need explicit controls.

## What the commits already protect

- Lifetime renewal is conditional on cumulative progress; stopped-head expiry remains finite.
- Both changed recovery call sites use the effective lifetime when clamping resends.
- The orphan-reset exception is narrow: a payload-bearing orphan with reliable receipt credit, a synthesized reset, and no final TCP owner.
- Once Transfer accepts that reset, its target is detached from the recycled return item; completion still depends on the actual Transfer ACK/failure callback.
- Closed return admission and lifecycle cancellation retain failure semantics. Normal retained socket returns do not receive the regenerable orphan target.

## Root-cause test analysis

| Commit | Included coverage | Important limitation |
| --- | --- | --- |
| `f25be1b2` | Adds `TestAckLifetimeFollowsCumulativeProgress` and `TestAckLifetimeWithoutHeadProgressKeepsItemDeadline`. Fixed times verify advancing/stopped/absent progress and lifetime-index arithmetic. | The fixture assigns `lastHeadAckTime` directly. It bypasses ACK ingress, pending snapshot ownership, pacing, and sequence teardown. |
| `1c0e74fb` | Adds no test, but existing `TestNonRetainedForcedAckTimeoutPreservesRetainedItems` explicitly establishes head progress and then forces expiry while retained siblings survive. | Relevant deterministic existing coverage, not proof from a new run. Add a focused scan-termination assertion so this dependency is obvious and bounded. |
| `77b2dd2d` | Adds `TestAckLifetimeResendDeadlineFollowsProgress`, including the no-new-progress deadline. | It tests the helper only. It still passes if either production resend call site reverts to the old clamp; no physical write count or recovery cadence is asserted. |
| `b74a5089` | Updates `TestProviderReliableOrphanPayloadResetFailedAdmissionThenFreshSuccess`: IPv4/IPv6 and protocol versions 1/2; an unbuffered send queue deterministically refuses; one attempt, no reset retry, settled input, and uncanceled sequence are checked. | Uses nil memory budgets. The later successful orphan uses a new incoming sequence, so healthy sibling progress on the original shared sequence is not proven. Actual provider return-worker saturation is separate. |

The staged H1 lifetime worker tests add useful coverage for applied head progress, stale/duplicate ACKs, and stopped-head expiry. They do not substitute for pending-feedback pacing or actual resend-cadence tests.

### Tests required to complete the remediation

1. Hold the actual pacing reservation across the original tail deadline. Publish a valid earlier cumulative ACK through the real ACK path, leaving the snapshot pending. Assert tail survival until one timeout after ACK arrival, then exact expiry when progress stops. Include unchanged head, stale ACK, unrelated SACK, cancellation, retained-item expiry, and balanced ownership controls.
2. Drive both actual recovery call sites beyond the original send-time deadline after cumulative progress. Assert scheduled backoff and physical write counts, including successful writes and immediate write refusal. Advance a controlled clock rather than using sleeps as the ordering proof.
3. Use real `ReceiveSequence.Pack` delivery and a finite shared budget to force reset scratch and return-item refusal separately. Keep a healthy sibling on the same incoming sequence active. Assert regenerable disposition, no shared cancellation, duplicate handling, no false ACK of live payload, and balanced packet/root memory. Include source/provider shutdown and real return-worker queue saturation controls.
4. Force TCP retirement after lookup but before send, and separately install a live same-tuple replacement. Verify the original receipt receives the intended orphan disposition without canceling shared work, and no stale payload enters or retires the replacement. Keep terminal-control and lifecycle rejection controls.

For each regression, demonstrate that the pre-fix implementation fails the intended assertion and that the corrected implementation passes. A timeout, compilation error, or unrelated failure is not the expected negative proof.

## CODESTYLE.md compliance

The implementation direction follows the rules on preserving shared packet flow, keeping recovery runnable under pressure, budgeting retry amplification, and retaining explicit ACK/buffer custody. The orphan exception is meaningfully narrower than treating all refused payloads as delivered.

Compliance is **partial**, rather than complete:

- [Lines 77–78](CODESTYLE.md#L77) require deterministic root-cause tests and an adjacent-issue search. The two residual paths above and helper-only resend test mean the remediation is not complete under that rule.
- [Lines 212–214](CODESTYLE.md#L212) require composed-path proof, overload isolation, and healthy siblings. Direct field assignment and nil-budget admission fixtures do not cover the missing real orderings.
- [Line 119](CODESTYLE.md#L119) says overloaded complete-message handoffs must not tear down shared work. `b74a5089` improves compliance for return admission refusal, but the memory-refusal path still violates the intended isolation.
- [Line 175](CODESTYLE.md#L175) requires both stalled-operation detection and healthy-operation tolerance. Progress-based renewal is appropriate, provided it uses actual arrival time and does not make duplicate feedback an unlimited extension.
- The new ACK test uses ordinary positive `t.Run` cases, contrary to [line 216](CODESTYLE.md#L216). The touched orphan test retains the same pre-existing pattern. Use top-level tests or a plain homogeneous table loop when revising them.
- `transfer_ack_lifetime_progress_test.go` lacks the file-level comment required by [line 72](CODESTYLE.md#L72).
- The commit messages include AI attribution/session trailers prohibited by [line 82](CODESTYLE.md#L82). The same rule says not to rewrite existing history; record this and follow the rule in future commits.
- The reviewed diffs pass whitespace checking. That is a formatting check, not a correctness qualification.

Two comments also need updating: [ip_provider_reliable_terminal_reset.go:69](ip_provider_reliable_terminal_reset.go#L69) still describes refusal as a failed receipt with no synthetic success; [ip.go:4810](ip.go#L4810) still says only reset confirmation settles it. Both should distinguish open-path refusal from accepted-reset ACK ownership.

## Conclusion

Retain the fixes, complete the pending-ACK, reset-memory refusal, and retired-flow admission paths, and add the composed deterministic regressions before calling this area fully remediated. Do not equate this source review with performance, mobile-memory, or full-suite qualification.

## Follow-up validation status

The user authorized Astra max to implement the findings. Both initial corrections are implemented and independently peer-reviewed; the coordinated checks passed 27 roots in three normal processes, then 74 new and adjacent roots in three normal processes. These are scoped correctness results, not full-suite or performance qualification.

Race validation is incomplete. Its first attempt exposed an unlocked inspection in the new test; that inspection now uses the queue lock and explicit owner joins. The expanded race attempt then exposed the distinct retirement-after-lookup failure above. The isolated control/forced-failure pair reproduced that mechanism, and its production correction is in progress. Evidence is retained under `canonical-orphan-after-lookup-before.zf5zpo8t` in `/Users/builder/Library/Caches/urnetwork-runall-owned/`.

A separate static concern about an already-cumulatively-acknowledged prefix failing during late pacing expiry was **not reproduced**: its strict new control passed three independent processes on unchanged production. The real acknowledged-write path returns to the normal ACK-snapshot pass before cancellation cleanup, preserving prefix completion. The positive control is retained; no speculative cleanup rewrite was applied.
