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

## Consumer input and read-order audit

The first consumer fixture produced 0P/18F because current own cumulative evidence never qualified. It is retained as an invalid-precondition experiment. Version 2 produced 15P/3F but admitted 768,000 bytes into a 512 KiB lane and used one 12,000-byte physical item. Its sustained low hold is therefore unreachable-input evidence, not a proved production failure.

Version 3 keeps each lane within current permission, opens only 40 groups, and uses two confirmed 6,000-byte items for the largest cumulative group. Unchanged production passes all 18 checks. The warm hold reaches service 3,324,121 B/s and final pacing 3,157,914 B/s against its own 2,823,529 B/s cumulative delivery. The split prefix remains fully receiver-timing eligible under the confirmed-H1 AND guard.

Version 3 also consumes the controller before every physical refill; version 2 reads once after a batch of sibling ACKs. A legal single-frame discriminator uses 1,000/4,000/8,000-byte items, a 40-cycle opening and 3.25 ms physical cycles. Both per-ACK and batched controller schedules pass all 18 checks. It does not reproduce the historical hold. No sampler production change is justified by version 2 alone.

A separate matched cadence comparison retains version 3's legal 12,000-byte cumulative group as two confirmed 6,000-byte items. Its existing frequent-read control passes three repetitions. Changing only controller reads to the end of each sibling batch fails three repetitions: service stays at 1 MB/s and shared pacing at 950,000 B/s through both 192 and 384 cycles. All current-window, legal-frame, physical ownership and own cumulative qualification checks pass. This establishes read-order dependence in the estimate consumer for legal prescribed inputs.

The per-lane criterion is necessary but insufficient for aggregate utilization. Shared reservation advances one common serialization clock for every lane. Even the passing single-frame result selects about 3.157 MB/s globally against aggregate delivery of 4 MB/s. The 0.95 queue-drain margin and a stated convergence allowance belong in any separate aggregate target. Positive-service reservation and cold cumulative fallback are being audited separately; neither a per-lane pass nor private service abstention proves full path utilization.

The original interleaved-slowdown and InvalidQueuedBound fixtures also preoffer large flights without normal admission checks and use 12,000-byte single items. Their frozen outcomes remain producer-mechanism evidence; the matched legal rolling-flight audit now supplies separate justification. With the original bounded decrease absent, the batched legal cumulative-group case retains shared pacing of 13.75 MB/s against physical 4 MB/s at both policy checkpoints; the corrected source passes the same 4.4 MB/s convergence ceiling. The pair is 9P/3F to 12P/0F under race instrumentation. Legal single-frame, frequent-read, legacy and mixed controls pass in both versions.

The positive-service diagnostic also runs three actual shared pacers with those final estimates. It naturally spends their shared 1 MiB probe before measuring 256 cycles, checks all byte/rate serialization debt and empty reservation ownership, and includes the burst meter in actual release time. Before correction the batched case releases about 13.75 MB/s; afterward about 3.831 MB/s, while the frequent-read control stays near 3.158 MB/s. The matched static consumer evidence does not establish closed-loop path throughput or attribute the earlier model differences.

A proposed new queued-start test also failed on the original source, but its chronology preconfirmed a future write and its small corrected interval permits a genuinely recovering fast path. Its old-rate cap was rejected as a hard oracle. That 12P/6F proposal remains frozen, including the lawful batched failure; no production behavior was changed to satisfy the rejected cap. New ownership work retains cumulative-prefix, same-sequence causality and tied-clock safety tests, plus established actual buffered-carrier controls. Every fixture remains an open-loop estimate-consumer test: it does not execute the pacing timer and cannot establish actual closed-loop throughput. Original files, gates and raw evidence remain immutable.
