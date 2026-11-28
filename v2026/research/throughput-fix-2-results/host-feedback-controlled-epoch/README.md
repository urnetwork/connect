# Induced TCP feedback and replay collapse

**Status: demonstrated and unfixed.** These completed diagnostics use pacing
code pinned to commit `6a212aeebee10c127720f6696e5c8ee9dc146f9e`. A small inner-TCP
replay after source idle can replace an established service estimate with a few
kilobytes per second. When inner acknowledgments reopen the NAT receive window,
the next bulk write then waits many seconds at that artificial rate.

The deterministic reproduction failed **3/3**: a 1,100-byte packet from the real
`TcpSequence.runReturnRecovery` worker changed established 125,000,000 B/s service
to **3,536 B/s**; the next 70 KiB write waited **18.430482186 virtual seconds**.
The test explicitly releases the delayed inner acknowledgment after the replay
crosses the actual pacing boundary. It calls the pacer directly and does not
model Transfer framing overhead or run the entire gVisor stack.

## Completed host diagnostics

All rows use one download flow, 300 microseconds path RTT, 10 ms Transfer ACK
compression, a 1 Gb/s FIFO link, 2.3015 seconds warmup, and 12 seconds measurement.
The feedback perturbation holds the device's reverse producer for 80 ms on its
first write at or after workload time 5 seconds. The optional 1.3 ms override
affects the window estimator's RTT term. Runs used the active development
environment and immutable binaries.

| Run | Pacing variant | RTT override | Reverse pause | Mb/s | Observed outcome |
| --- | --- | --- | --- | ---: | --- |
| `combined` | committed | 1.3 ms | 80 ms | 188.51 | Final nine intervals about 1.7–3.3 Mb/s |
| `rtt-only` | committed | 1.3 ms | none | 936.01 | Sustained throughput |
| `reverse-pause-only` | committed | none | 80 ms | 216.65 | Sustained post-pause collapse |
| `ceiling-combined` | constant-window ceiling | 1.3 ms | 80 ms | 934.66 | Recovered after the pause |
| `reverse-pause-state` | committed, extra state trace | none | 80 ms | 906.61 | Recovered by about 5.5 s |
| `reverse-pause-no-hold` | no generic hold, extra state trace | none | 80 ms | 836.68 | Recovered; its baseline also recovered |
| `combined-state` | committed, extra state trace | 1.3 ms | 80 ms | 197.81 | Final eight intervals at 0 Mb/s |
| `combined-no-hold` | no generic hold, extra state trace | 1.3 ms | 80 ms | 204.64 | Final intervals about 2–4 Mb/s |

The no-generic-hold counterfactual changes only the final fallback in
`measured()` from `latest = hold` to `latest = 0 * hold`. It does not fix the
combined failure. There is **no after-fix result** in this archive.

All complete per-second interval arrays are in [readings.jsonl](readings.jsonl).
The rows retain the original comparison censor reasons: these single-arm runs
lack the full A/A-bracketed comparison and calibration protocol. Their Go test
exit status is PASS because those comparisons are censored; that status does
not assert acceptable throughput. Transfer/NAT drop totals and the last sampled
TUN, stack, and endpoint drop counters are zero in these rows.

## Causal trace and deterministic reproduction

In `combined-state`, ordinary forward traffic stopped near 5.79 s while the NAT
waited for inner acknowledgment progress. Its first replay near 6.115 s added
1,203 Transfer bytes. The isolated reply changed forward service from about
1.87 MB/s to **3,722 B/s**. Inner acknowledgment progress near 6.725 s reopened
the receive window; the trace then shows **717,904 bytes of available NAT
capacity**, zero forward physical flight, and a pending 70,646-byte write with
about **17 seconds of pacing debt**. Thus the reopened NAT window could not
restore bulk progress. Selected numerical points are in
[causal-points.json](causal-points.json).

[replay-root.json](replay-root.json) records all three deterministic failures,
the exact command, protocol, and original source/log/overlay hashes.
[replay-root-before.txt](replay-root-before.txt) contains the synthetic failure
output. [replay-root-test.go.txt](replay-root-test.go.txt) preserves the wholly
synthetic test source as non-Go text, so it does not run as an unfixed normal
test. To reproduce in a checkout of the pinned commit, copy that text into a
temporary `.go` file and use a Go overlay mapping the repository path
`transfer_window_host_feedback_test.go` to the temporary file. The original
overlay and its exact replacement hashes are recorded in the manifest.

The attempted full gVisor/net.Pipe virtual-time fixture timed out because
gVisor's custom parked workers did not permit Go's virtual clock to advance.
[virtual-fixture-limit.json](virtual-fixture-limit.json) records that diagnostic
limitation; it is not a product failure or a throughput result. The smaller root
test uses the real NAT replay worker, real pacing calls, and an explicit channel
handoff to force replay before window reopening.

## Provenance and limits

[manifest.json](manifest.json) pins the production source, diagnostic overlays,
overlay replacement sources, binaries, original logs, and run protocol. Raw
logs, binaries, environment files, and overlay implementation sources remain
under `/tmp/throughput-fix-2-host-feedback-6a212aee`; they are not copied here.
Absolute synthetic sequence numbers, client identities, addresses, and
wall-clock service-epoch timestamps are omitted from the numerical archive.

The additional state trace samples the real NAT sequence under its existing
mutex every 100 ms. It avoids gVisor `TcpInfo` and does not change protocol state,
but diagnostic scheduling can change which host ordering occurs. The host
collapse is variable; the replay-boundary failure is deterministic.

This root is distinct from the sample-rebucketing correction prepared after
`6a212aee`. No completed rebucketing result is represented here. These induced
failures also do **not** establish the cause of every historical paired host
failure, including rows ending near 300 Mb/s with a healthy reported service
rate. NAT recovery legitimately responds to missing inner ACK progress; the
demonstrated error is pricing feedback/source idle as serialization.

Remaining work is to preserve the correct service evidence across this idle
boundary, retain capacity discovery for naturally drained but continuously
offered traffic, and validate the correction against this reproduction and
fresh paired host runs. No production correction or runnable Go test was added
by this diagnostic archive.
