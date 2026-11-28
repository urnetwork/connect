# Affinity checkpoint — unqualified test build

This checkpoint includes the donor policy and the current executable same-tuple
SYN prototype. It is not a completed or performance-qualified feature. At source
integration, these changes had been formatted but not compiled or tested.

Subsequent checkpoint checks compiled Connect and SDK `./...` and the PERF
harness with Go 1.27.2 and `-run '^$'`. SDK headers and platform localization
resources were regenerated through their sanctioned generators; packaging and
localization checks passed. No behavioral tests, mobile AAR/framework build,
PERF comparison or device-memory qualification is implied by these checks.

The 29 Go-file payload is pinned by private `syn-checkpoint-v1.txt`, SHA256
`75dac540c50af34212d69451272b24ff3447a260a68ef5115cde8b3ee3bd1f0e`.
Its 14 existing-file preimages matched before integration; unrelated contract
fixtures and the independently owned H1 Transfer changes were not overwritten.
The private donor-v1–v5 and earlier unsafe prototype evidence remain preserved.

## Intended behavior in this checkpoint

- `AffinityMaxTime` replaces `FreshFlowAffinity`. The default is one minute;
  nonpositive values disable ordinary inheritance. This enables bounded
  inheritance by default, unlike the previous default-off fresh-flow setting.
- Age starts at update creation, including provider setup, and inherited flows
  retain the earliest lineage creation time. Activity, DNS/index refresh,
  same-object rebind and SYN bookkeeping cannot renew it.
- Fresh inheritance requires positive current health, a live client/carrier and
  target-family support. Explicit pins exempt age, not health or lifetime.
- A bound, canonical TCP flow's new SYN or unestablished same-ISN retry applies
  the cap and health gate. An established same-ISN replay is not distinguishable
  from a newly reused identical ISN without an observed close, so it stays with
  the established owner. Ordinary TCP/UDP data does not age-migrate.
- Renewal reserves the canonical tuple through actual source admission.
  Refusal preserves the incumbent and caller-owned packet. Application delivery
  leases prevent renewal from overtaking an executing old callback.
- Generic and sticky idle defaults are five minutes; TCP remains ten minutes.
  Explicit overrides remain authoritative. Half-close retention and admitted
  RST/fully acknowledged FIN cleanup have authored regression coverage.
- SDK compatibility is `AffinityMaxTimeMillis` ↔ `AffinityMaxTime time.Duration`,
  with a default of 60000 ms. The optional fresh-destination generator interface
  is additive; ordinary generator behavior is unchanged.

## Known functional and correctness gaps

1. Fresh-identity acquisition is **not connected to window capacity/setup**.
   Renewal excludes all previously admitted authenticated source/provider keys.
   An initial six-source fanout can therefore exhaust every available source.
   The six-established-sibling test expects bounded refusal and preservation,
   not successful recovery. Sole-provider recovery and page-delay impact remain
   unresolved; no seventh client or established-sibling cancellation was added.
2. Explicit-close/canceled-map replacement still uses the previous lookup path.
   Once the canonical entry is removed, its source history is unavailable to
   the next tuple. Mixed accepted RST → new SYN and post-reaper recreation do
   not yet have a complete composed provider-generation proof.
3. Provider RST policy is unchanged. The current `TcpBuffer` reset branch
   cancels/deletes a tuple before the SYN sequence comparison; a new stream with
   the same authenticated source is not a new NAT namespace. Separate, unrun
   LocalUserNat source-isolation positive/negative controls are preserved in
   private evidence. Any RST-policy change needs separate legitimate,
   in-window/out-of-window reset and stack-semantics review.
4. Young healthy same-provider generation bookkeeping retains the previous
   path. Full composed quarantine/removal/cancellation/ABA, blocked NAT control,
   provisional recovery-budget reconciliation and mixed partial-outcome proofs
   are still required. Component test seams do not establish those proofs.
5. Health and route checks are separate snapshots. They do not form an atomic
   route/health/queue transaction; later lifecycle changes use existing rules.

## Native batch contract

The native multi-client and `IpMux` stages split a same-tuple burst at a new
client-SYN generation boundary. Old data/ACK/FIN/RST prefixes and later SYN
segments receive separate indexed outcomes. Same-generation duplicates still
coalesce. Each resulting low-level group retains all-or-none admission.
`SendPacketBatch` still consumes all inputs and returns the accepted count;
`SendPacketBatchWithResults` still reports each original input position.
The coalescing, allocation and latency costs remain unmeasured.

## Authored case census

The donor-v5 component has 28 named roots: 25 age/lineage/health/DNS/bridge/
binding/lifetime/public-fresh-flow roots plus three fresh host/app/DNS pin-health
roots. The added SYN component has these 42 top-level roots. This is a source
census, not an execution result.

```text
TestAffinityRenewalActiveDeliverySurvivesRemovedMap
TestAffinityRenewalQueuedOldControlRejectsNewCanonical
TestAffinityRenewalRefusalRestoresDeliveryWithoutAba
TestAffinityRenewalDeliveryPanicReleasesOwner
TestAffinityRenewalDeliveryMapBoundAndUnknownOwner
TestAffinityRenewalCancellationJoinsExecutingDelivery
TestAffinityRenewalRemovalWorkerDropsOldOwnedPacket
TestAffinityRenewalNativeGroupingPreservesOrdinaryCoalescing
TestAffinityRenewalNativeGroupingSplitsResetThenSameIsn
TestAffinityRenewalNativeBatchMixedGenerationIndexedOwnership
TestAffinityRenewalNativeBatchCancellationKeepsPrefixOutcome
TestAffinityRenewalMuxBatchMixedGenerationOwnership
TestAffinityRenewalMuxBatchCancellationKeepsPrefixOutcome
TestAffinityRenewalFreshIdentityBypassesParkedSource
TestAffinityRenewalFreshIdentityKeepsLiveSibling
TestAffinityRenewalFreshIdentityBypassesRestoredSource
TestAffinityRenewalFreshIdentityRejectsUnsupportedGenerator
TestAffinityRenewalFreshIdentityRejectsReusedOwnership
TestAffinityRenewalFreshIdentityCancellationJoinsPartialOwner
TestAffinityRenewalFreshIdentityCannotOverwriteKnownGeneration
TestAffinityRenewalPublicHalfCloseAndFinalAckLifetime
TestAffinityRenewalPublicRefusedFinAndRstKeepOwner
TestAffinityRenewalPublicFullSixOccupiedRefusesWithoutMigration
TestAffinityRenewalPublicIp4AgeBoundaries
TestAffinityRenewalPublicIp6AgeBoundaries
TestAffinityRenewalPublicNonpositiveDisablesSynReuse
TestAffinityRenewalPublicSameIsnUnestablishedBoundary
TestAffinityRenewalPublicEstablishedReplayAndDataStayStable
TestAffinityRenewalPublicUnhealthySynRaces
TestAffinityRenewalPublicRefusalKeepsOldOwnerAndRetries
TestAffinityRenewalPublicDequeueCommitsBeforeProducerReturn
TestAffinityRenewalPublicFastOpenAndOldSourceIngress
TestAffinityRenewalPublicActiveCallbackRefusesThenRetries
TestAffinityRenewalPublicRefusedFastOpenRetryCannotExpandIngress
TestAffinityRenewalFastOpenAckBoundsRejectMalformedSpan
TestAffinityRenewalSourceFenceRequiresCumulativePeerCompletion
TestAffinityRenewalWrittenErrorCannotBecomeSourceFence
TestAffinityRenewalUnwrittenQueueCancellationSettlesSource
TestAffinityRenewalCumulativeCallbackBeforePublicReturnKeepsOfferLease
TestAffinityRenewalClosedEnrollmentPreservesCallerPacket
TestAffinityRenewalSourceSlotsAreBoundedAndEpochExact
TestAffinityRenewalPublicSixSourceFanoutKeepsEveryAdmission
```

## Required qualification

Compile the combined checkpoint first, then run the exact roots, adjacent
admission/recovery/pool/lifecycle suites, race detector and full suites. The
earlier independent OLD donor screen had six expected semantic failures and
one DNS fixture-premise error; the corrected OLD adapter still needs execution.
No NEW success or performance improvement is claimed here.

Measure actual allocation classes/retained roots, cap-zero overhead, source
ledger growth, receive-path locking and five-minute retention at the existing
32/64 MiB profiles. The source ledger is bounded at 64 scalar keys per flow;
the delivery map is bounded at 1024 currently executing tuples and retains no
history after callbacks return. These bounds are not memory qualification.

Use the existing two-provider PERFVAR fixture: A/A first, then balanced paired
old-default-off versus capped-on runs plus a cap-zero mechanism control,
reporting failures, setup/handshake count, fresh-flow latency and full memory/
CPU cost. Preserve existing correctness gates. Finish missing functionality
and evidence before claiming all requested cases or production readiness.
