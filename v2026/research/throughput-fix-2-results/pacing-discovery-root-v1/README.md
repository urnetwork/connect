# Pacing discovery floor and held pace — root evidence v1

Two pacing rules were added to `windowPacingRate` (transfer_window_pacing.go) and
wired through `SendSequence.estimateSendWindow` (transfer.go). Window sizing,
service qualification and the ACK contracts are unchanged.

- **R1 discovery floor.** Until the shared pacing service has observed queueing
  on this path, the pace is at least `Window / WindowRoundTrip`, where `Window`
  is the admitted window after retention and permission clamps. Queueing is
  observed when an RTT sample exceeds the unloaded minimum plus compression plus
  `max(2ms, minimum/4)` — the drain check's own margin — or when a recovery
  (resend) write is reserved. A minute of service silence replaces the path
  baseline and restarts discovery with it.
- **R2 held pace.** The pace last granted by an admitting (`retainService=true`)
  read is held on the service. While a read is not backlogged the pace does not
  fall below the held value. A backlogged read or a recovery reservation
  releases the hold. Statistics reads never write service state.

Production sites: `windowPacingService.{queueObservedAt, heldPacingRate}`,
`pacingHold`, `holdPacing`, `observeQueueWithLock` (called from `observeWithLock`
and `observeRoundTripWithLock`), the resend branch of `reserve`, the two new
floors in `windowPacingRate`, and the two reads/writes in `estimateSendWindow`.
The pace is now computed at the very end of the estimate's deferred finalizer,
after the admitted window is final, because the discovery floor releases exactly
that window over one residence.

## Runs

`before.log` was produced with the production code unchanged except the two
inert `SendWindowEstimate` fields; `TestWindowPacingServiceObservesQueueOnce`
did not exist yet (its file is part of the production change), so six tests ran.
`after.log` is the same command with the change in. No quiescence wait was used
before any run; another agent's test runner was active on the host throughout
(load average 6.6–10.3).

| test | before | after |
| --- | --- | --- |
| TestWindowPacingDiscoveryReleasesRetainedWindow | FAIL (4 of 6 cases) | PASS |
| TestWindowPacingHeldRateFallsOnlyWithCongestion | FAIL (3 of 5 cases) | PASS |
| TestWindowPacingServiceObservesQueueOnce | not present | PASS |
| TestWindowPathDiscoveryFillsRetainedWindow | FAIL | PASS |
| TestWindowPathDiscoveryStopsAtCapacity | PASS | PASS |
| TestWindowPathPermissionShrinkKeepsPace | FAIL | PASS |
| TestWindowPathCapacityDropStillLowersPace | PASS | PASS |

Before the change the 400 ms cell of `TestWindowPathDiscoveryFillsRetainedWindow`
did not run: `synctest.Test` calls `t.FailNow` when its bubble fails, so the
100 ms failure ended the test.

### Key numbers

`TestWindowPathDiscoveryFillsRetainedWindow`, sdk-device-default sender against
the budgeted server, window permission 7010478 bytes:

- 100 ms (residence 110 ms, window rate 63731618 B/s):
  before `Mb/s=68.328727 pace=9401920 window=1880749`;
  after `Mb/s=522.240000 pace=91980960 window=7010478 discovering=true drops=0`.
- 400 ms (residence 410 ms, window rate 17098726 B/s):
  after `Mb/s=131.171902 pace=38902741 window=7010478 discovering=true drops=0`.

`TestWindowPathPermissionShrinkKeepsPace` (2 MiB permission shrinking to 64 KiB,
100 ms RTT, 50 ms compression, eight flows):
before `ceiling=4.676 fixed=3.523 pace=587620`;
after `ceiling=4.598 fixed=4.669 pace=13750308 drops=0/0`.

The two capacity guards keep their behavior and now genuinely observe a queue:
`TestWindowPathDiscoveryStopsAtCapacity` after is
`Mb/s=30.670049 pace=3799985 discovering=false drops=0/0`, and
`TestWindowPathCapacityDropStillLowersPace` after is
`Mb/s=15.339520 pace=1891553 discovering=false`.

`sdk-after.log` is the affected SDK/mismatch regression selection, all passing:
`TestWindowPathRetainedSdkCumulativePacingLong` candidate 513.024000 Mb/s at
100 ms and 134.185210 Mb/s at 400 ms, both above their reference arms.

## Race correctness run

`tools/throughput-fix-2.sh correctness /tmp/throughput-fix-2-correctness-pacing-discovery-v1`
(not copied here). `status.json`:

```
{"exit_code": 1, "rows": 12, "comparison_count": 0, "failed_comparisons": 0, "censored_comparisons": 0, "finished_utc": "2026-09-16T23:50:06.617882+00:00", "host_load_average_at_finish": [8.29150390625, 8.26123046875, 7.30908203125]}
```

476 tests passed, 12 failed. Four are pre-existing and unrelated: the
`TestWindowPacingReceiverWait*` group in
transfer_window_service_receiver_interval_test.go and
transfer_window_service_refill_interval_test.go fails identically on a copy of
this tree with only the two pacing floors and their estimate wiring reverted.

The other eight are the `TestWindowPacingCumulative*` group in
transfer_window_cumulative_pacing_test.go, which passes on that same reverted
copy. Each pins an exact `PacingByteRate` for a fixture whose service has never
observed queueing, so R1's floor raises the pace to the admitted window over one
residence (1048576 / 10 ms = 104857600 B/s) instead of the pinned cumulative or
service value. Those tests were not modified; the interaction is reported to the
design owner.

## Contract re-pin

Two production refinements landed with the re-pin. `observeQueueWithLock` now
releases the held pace on *every* queued observation, not only the first: the
first sustained queue still ends discovery, and each later queued reply clears
`heldPacingRate` so a slower path lowers pacing even when the flight happens to
be drained at read time. And the discovery/hold pair is read and written only
for a sequence whose flight policy is H1-only, matching `paceWrite`, which
applies the pace only there; other carriers keep the service-relative value.

`repin-v2.log` is the targeted selection (21 tests, all PASS) and `sdk-v2.log`
the SDK/mismatch selection (5 tests, all PASS), both after the re-pin. No
quiescence wait; another agent's runner was active throughout (load ~5.3–6.8).

`TestWindowPacingServiceObservesQueueOnce` gained two steps: with discovery
already over, a queued 150 ms reply against the 100 ms baseline releases a held
6000000, and the following unqueued 100 ms reply keeps a held 7000000.

The eight `TestWindowPacingCumulative*` tests pinned the superseded contract.
The fixture's replies are a clean 10 ms round trip, so its service never
observes a queue and the pace released while discovering is the admitted window
over one residence — 1 MiB / 10 ms = 104857600 B/s, written in the test file as
`windowDiscoveryFloor(estimate)`.

| test | was | now |
| --- | --- | --- |
| CumulativeGrowthRaisesColdRate | pace 57671680 | pace = floor 104857600, discovering |
| CumulativeMissingHistoryKeepsRetainedWindow | pace 52428800 | pace = floor 104857600, service 0, delivery 0 |
| CumulativeStaleHistoryCannotReprice | pace 52428800 | pace = floor 104857600, delivery 0 |
| CumulativePermissionStepRejectsOldRate | pace 52428800 | pace = floor 104857600, delivery 0 |
| CumulativeSlowDeliveryLowersOnlyRate | pace 28835840 | split in two, below |
| CumulativeRateDefersToSlowService | pace 450560 | unchanged, now with queue evidence first |
| CumulativeH1SiblingOwnsItsFallback | fresh 57671680, idle 52428800 | fresh = floor 104857600, idle = fresh |
| CumulativeSharedServiceRejectsOtherCarriers | pace 52428800 | unchanged; the H1-only gate restores it |

`SlowDeliveryLowersOnlyRate` became two roots. `SlowUnqueuedDeliveryKeepsPace`
keeps the original setup: delivery falls to 26214400 B/s but the replies show no
queue, so the source rather than the path was the limit and the pace stays at
the floor 104857600. `SlowQueuedDeliveryLowersOnlyRate` injects one queued
reply (`observeRoundTrip(40ms, 0, at)`) before the read and keeps the original
pins, pace 28835840 with discovery over. `RateDefersToSlowService` likewise now
proves the slow service is the path's with a 10 ms baseline and a 40 ms queued
sample before its two 4096-byte observations; its sibling
`UnqueuedSlowServiceStillDiscovers` shows the same measured 409600 B/s service
without queue evidence still leaves the pace at the floor, because a measured
rate the pacer itself produced bounds capacity only from below.

In `H1SiblingOwnsItsFallback` the idle sibling's own window is Initial
(524288 bytes, its own floor 52428800), yet its pace is the source's 104857600:
the held pace carries the value the path already accepted across the shared
service, while the sibling's own window still bounds its flight.

### Race correctness run v2

`tools/throughput-fix-2.sh correctness /tmp/throughput-fix-2-correctness-pacing-discovery-v2`
(not copied here). `status.json`:

```
{"exit_code": 1, "rows": 12, "comparison_count": 0, "failed_comparisons": 0, "censored_comparisons": 0, "finished_utc": "2026-09-17T00:10:25.917862+00:00", "host_load_average_at_finish": [6.09765625, 6.42431640625, 6.52734375]}
```

486 tests passed, 4 failed. The eight `TestWindowPacingCumulative*` failures of
v1 are gone. The four that remain are the pre-existing, unrelated
`TestWindowPacingReceiverWait*` group already documented above:

```
transfer_window_service_receiver_interval_test.go:65: receiver wait replaced physical serialization: service=250000 want12500000
transfer_window_service_receiver_interval_test.go:81: receiver wait hid a corrected slower interval: service=248000 want12400000
transfer_window_service_refill_interval_test.go:101: opening exact receiver pair did not preserve serialization: 250000/250000
transfer_window_service_refill_interval_test.go:111: opening exact receiver pair did not preserve serialization: 250000/250000
```
