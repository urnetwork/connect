# Sampler test semantic audit

Audit only. Frozen tests and production are unchanged. The evaluated source is
`throughput-fix-2-receiver-sibling-capacity-before-pinned/source/connect`, whose
sampler is 3294610224c983316fc09191818e13f59bc964dcff6af76ab9db793cf8ad4f3a.

## Contract and classification

The agreed product policy retains learned window permission and adapts pacing
to qualified service, including sustained changes without an application signal.
It does not by itself specify a peak estimator, immediate acceptance after two
checkpoints, exact controller equality, or a numerical convergence deadline.

**Hard invariants:** credit each owned byte once; preserve raw physical, probe,
epoch and recovery clocks; use metadata from the credited head; keep first-byte
exclusion attached to the selected earliest checkpoint and sum all earliest
ties; reject nonpositive causal intervals; preserve queue provenance; statistics
must not commit controller state. A single cumulative receipt supplies no
serialization interval. Sorting cannot repair a causally reversed same-sequence
pair.

**Estimator choices:** when to accept sufficient evidence, how to smooth it,
whether to temporarily abstain, how to age a hold, and how quickly to converge.
Once an interval is selected, its byte/span arithmetic can be tested exactly at
the measurement layer. That does not automatically require the controller to
publish that number immediately.

## Disputed current roots

| Test or group | What is established | Classification and another valid response |
| --- | --- | --- |
| `ReceiverLateSiblingCompletesColdSerialization` | All three confirmed siblings are credited exactly once; the full fixed-path train mathematically supports 12.5 MB/s. The intermediate two-head observation supports only 6.25 MB/s. | The exact final estimator equality is policy-specific. Temporary zero, 6.25 MB/s, or a smoothed intermediate rate can be valid while collecting more evidence. Repeated complete owned trains must eventually influence pacing; this one-train test does not prove persistent failure. |
| `ReceiverLateSiblingCompletesEpochSerialization` | A real probe establishes the new epoch; complete sibling bytes support 12.5 MB/s; raw ownership is correct with and without a rolling tail. | Exact immediate recovery is policy-specific. Retaining the pre-epoch qualified rate, withholding the partial decrease, or bounded recovery after more complete trains are alternatives. |
| `ReceiverLateSiblingFindsSlowEpochSerialization` | The fixed-path owned train supports 1 MB/s, while its partial observation gives 500 kB/s. | Immediate exact 1 MB/s is policy-specific. A controller can approach it from either side. Sustained genuine slowdown must ultimately lower pacing; retaining 12.5 MB/s indefinitely would violate adaptation. |
| Ordered cold/epoch sibling controls | Same bytes and physical schedule with aligned receiver waits. | Useful positive controls for the selected estimator. Immediate equality is not a universal controller contract. Compare eventual behavior across accounting orders without insisting every intermediate read matches. |
| `ReceiverInvalidQueuedBoundCannotRaiseService` | The conservative maximum-wait bound is only an upper bound, and must not itself establish an increase. The fixture independently supplies 192 cycles of true 4 MB/s service while the old hold is 1 MB/s. | The global `rate <= 1 MB/s` assertion overconstrains independent measurement. A different exact, attributable corrected interval could validly raise service toward 4 MB/s. Preserve the branch's downward-only contract, not a permanent estimator cap. |
| `ReceiverInvalidShortQueueKeepsHold` | One short ambiguous train is insufficient for the existing conservative queued-bound branch. | Exact old-hold equality is branch/policy-specific. Independent fully qualified ordered timing may justify another rate; ambiguous arithmetic alone must not. |
| `ReceiverInvalidWaitAllowancePreservesFastService` | Receiver wait variation alone cannot prove a slower serializer; current fixed-path input remains 12.5 MB/s. | Protects a real measurement principle, but exact no-decrease at one controller read is stronger than a smoothing policy. A short conservative response can be valid if fresh supported capacity recovers within the agreed convergence budget. |
| `ReceiverInvalidLimitedRefillsKeepHold` | A one-byte outstanding tail does not prove the serializer was busy during application/refill silence. | The silence must not be published as measured serializer capacity. Holding the exact old number is one policy; explicit unknown service with bounded probing is another. |
| `ReceiverInterleavedSlowdownAdaptsWithInvalidRing` | Sustained 4 MB/s delivery, exact credit, a 5.44 MB standing queue, and no application signal. | Eventual adaptation is required. The last sixteen reads within 3.6–4.4 MB/s after about 0.8 s are a chosen convergence gate. The requirement that all 64 raw-attached buckets have `receiverInvalid` is a representation-specific fixture precondition, not a public API invariant. A separate exact receiver aggregate may legitimately qualify the same evidence. |

## Existing safety and timing groups

* Receiver ordering upper bounds (interior reply, earliest-byte ownership,
  earliest ties, delayed credit, resize and warm interval) generally permit an
  alternative corrected-extrema representation without loosening assertions.
  They forbid counting bytes outside the chosen interval or inventing capacity.
* `ReceiverCreditNonpositiveIntervalCannotGrantCapacity` must remain a negative
  control: two causally ordered heads of one sequence cannot be sorted into a
  positive interval when correction makes the second equal to or earlier than
  the first. Sender/sequence attribution matters before pooling siblings.
* Cold short-pair and drained-pair roots establish exact mathematical rates in
  fixed-path fixtures. Prompt acceptance below the advertised compression cap
  is a chosen discovery policy. One-checkpoint, actual buffered-reader and
  legacy-compression controls still constrain unsafe inference.
* The outstanding cold sparse-service control proves a plausible slow path,
  not the unique interpretation of sparse feedback. Immediate exact acceptance
  is a policy choice; abstention followed by bounded probing is another. A
  permanently retained startup floor is not valid under sustained slow evidence.
* Pending-cycle, queued mean, fresh-summary and physical-cycle roots test that
  unequal receiver waits do not change the selected physical interval. Shared
  exact arithmetic belongs at the measurement layer; immediate estimator
  equality also fixes the current acceptance policy. Summary eviction must not
  silently change endpoint semantics or revive superseded epochs.
* Full physical drain proves delivery and ownership. It alone does not prove
  that all elapsed time was serialization. Queue evidence and genuine slow
  cycle controls remain necessary before pricing the whole gap.
* Partial controller-read and statistics controls correctly require reads not
  to manufacture evidence or alter ownership. A controller may accept partial
  evidence under an explicit policy, but statistics must remain observational
  and a later complete proof must not inherit an accidental read-order artifact.

## Finite-relay failure

The failing cell is 12.5 MB/s, four logical sequences, eight flows, 100 ms RTT,
10 ms compression, two-second warmup and two-second measurement. Delivery is
72.68352 versus 95.616 Mb/s; its two intervals are 59.92448 and 85.44256 Mb/s.
There are no drops/retries or hard queue bound violations. The final service is
11,094,498 B/s and pacing 12,203,947 B/s, so recovery is already visible. The
1 Mb/s and 1 Gb/s controls pass.

This is currently a bounded recovery/performance failure, not proof of lost
byte ownership or permanent undercapacity. The 90% gate remains unchanged.
A branch-local trace is needed before associating it with the partial-sibling
mechanism. The existing three one-train roots do not force sustained shared
relay recovery, so they do not substitute for that proof.

## Next validation shape

Keep all historical proofs unchanged. Separate exact interval arithmetic and
eligibility safety from controller response. Add sustained owned-sibling
feedback scenarios that expose accounting reordering while preserving sender
identity, genuine slow service, ties, same-sequence negative intervals, and
queue provenance. Set any convergence budget explicitly from the intended
controller policy and evaluate both ordered and reordered controls. Do not
silently weaken current gates to obtain a passing candidate.
