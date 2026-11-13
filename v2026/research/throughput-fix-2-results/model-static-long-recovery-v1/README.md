# Static long-path recovery with the actual sender and pacer

Both new controls pass on the isolated common-delivery plus cumulative-prefix
candidate. The 400 ms one-lane cell delivers 30.639 Mb/s against 30.633 Mb/s;
the 1.2 s three-lane cell delivers 30.636 Mb/s against 30.635 Mb/s. Both have
zero measured relay drops and zero timeout resends. All four complete readings
and both outcomes are retained.

The physical service stays at 4,000,000 B/s and propagation never changes.
Candidate senders begin at 512 KiB. Real Transfer senders, ACK workers, shared
pacing and finite FIFO queues drive the feedback; no service, RTT, probe-credit
or learned-window value is injected. Memory and message ownership reconcile
through the existing fixture. This is a closed-loop static performance control,
not a reproduction of one isolated warm-bound defect or a native H1/TUN run.

Before any run, the tests specified 64 feedback turns of warmup (26.24 s and
77.44 s), eight more turns of measurement, a calibrated reference, 90 percent
of paired reference throughput, per-lane progress and hard bounds/drop checks.
Those long warmups establish settled performance. They do not establish quick
startup or resolve the separate, unchanged SDK short-warmup failure. No one
should substitute these passes for that failure or a complete core matrix.

Terra medium compiled and ran the normal binary once under concurrent host
load, with no quiescence wait. The status records load and immutable source,
manifest, binary and raw-log hashes. All 919 connect and 24 copied glog input
files are hashed in the source inventory. The local source/run directory is
preserved under `/tmp/throughput-fix-2-static-long-path-work`.

The initial build-helper overlay argument error is preserved. A later missing
input-inventory setup error survives only in the tool transcript because its
log was replaced by the final successful build log. Neither was a Go test
failure; the successful manifest and source inventory identify the executed
binary. No sampler correction was adopted into the live checkout by this run.

## Source adoption

The isolated test source `transfer_window_static_long_recovery_test.go` was
moved into the main test inventory on 2026-09-16 after review; the archived
results above were produced from the byte-identical isolated copy. The
`TestWindowPathService.*` runner selection now includes both controls.
