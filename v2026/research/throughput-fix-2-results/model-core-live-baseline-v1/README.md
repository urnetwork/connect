# Live-tree model-core baseline before the pacing discovery fix

`tools/throughput-fix-2.sh model-core` on the working tree at `f903f59a`
plus the uncommitted edits, run 2026-09-16 23:36 UTC under concurrent host
load (load average 9.8 at start, 8.0 at finish; no quiescence wait). The
compiled 830-file snapshot digest is `9d37f91c…`; see `RUNNER-DEVIATION.txt`
for why the live digest differed by one archived test source at the time.
That source has since moved into the main inventory and the runner's live
digest now skips `throughput-fix-2-results/`.

23 passes, 4 failures, 812 ledger rows; the three explicit propagation
transitions stay deferred (`deferred-tests.json`). Failed groups and their
comparisons are in `failed-cells.txt`:

| Test | Failing condition |
| --- | --- |
| `TestWindowPathSdkProfiles` | Nine cells: mobile-policy device senders at 100 ms and 400 ms deliver 20-53 percent of the paired reference; the device H1 sender at 100 ms reaches 89 percent. |
| `TestWindowPathSdkConstrainedLongWindow` | Device H1 sender at 400 ms: 59.6 versus 130.9 Mb/s. |
| `TestWindowPathSdkBidirectional` | Reverse direction from the device H1 sender at 100 ms: 207 versus 459 Mb/s. |
| `TestWindowPathWindowMismatchChanges` | Receive permission 2 MiB to 64 KiB at 50 ms compression: 3.543 versus 4.598 Mb/s, pace 0.59 MB/s. |

Every failure is the pacing controller following its own release rate or
the shrunken window's delivery rate; see research plan section 34.
