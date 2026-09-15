# Throughput fix 2: final local validation report

Date: 2026-09-15
Branch: `throughput-fix-2`
Connect source revision: `b51530f3a402cb1dd5fe5d1daae344302a1f3069`
Connect source manifest: `034a8ba28c61e70407d050228067a342b36f49505ae915cb99a7824a19bd90ee`
Server source revision reviewed: `77201554c49ec05bde83ec038bba6c600972892c`

## Result

The new window mechanism and its adjacent root causes now have deterministic
coverage. The race-enabled correctness selection passed 89 top-level tests.
The virtual FIFO model passed 213 paired cells (390 ledger rows), including
window-size mismatch and live capacity changes. The host TCP matrix and the
separate 48 MiB TCP-buffer control each passed 168 ledger rows. ACK compression
tests and the allocation benchmark passed.

The missing native H1/TUN rig source and P37–P44 ledger were not available, so
these results are local reproduction and regression evidence. They do not
establish behavior on the reporter's deployed relay or every host network.

## Root causes fixed

- Window sizing now applies the working floor only after known local, peer and
  deployment ceilings. The unbudgeted branch also respects known limits.
- ACK compression residence is included in target and delivery terms. Wide
  arithmetic saturates contract announcements instead of wrapping to zero.
- Delivery evidence keeps paired first/last arrival checkpoints, spans a
  stable RTT/compression feedback horizon, accepts out-of-order application,
  and resets the evidence boundary when capacity increases.
- Service pacing has one bounded opening probe and one allowance per logical
  service. It uses the first physical write and ACK arrival clocks, excludes
  ambiguous/retransmitted carriers from relay RTT, and leaves drain margin when
  a queue is present.
- Upload TCP replay retains bounded origin chunks through cumulative inner ACK;
  TUN pure ACKs bypass the data handoff lock, and producer close is joined and
  drained before upload shutdown returns.
- ACK responses are owned by `sequenceAckWindow` and are independently tested.
  Each response has one cumulative head ACK and bounded oldest-first SACKs above
  that head; a newer head absorbs SACKs at or below it, and metadata after a
  SACK overflow repeats the cumulative head. The worker's 8 KiB/32
  entry reservation, eviction metadata, deadlines, gap wake and cancellation
  drain are covered for both codecs and encrypted wrappers.

## Deterministic coverage

| Area | Coverage and outcome |
|---|---|
| ACK coalescing | Head/SACK ordering, absorption, overflow, pacing, maximum serialized size, eviction notices and final drain; pass. |
| Window arithmetic and ownership | 89 top-level tests under `go test -race`; pass. |
| RTT/flow/compression model | 36 cells; minimum candidate/reference ratio about 99.9%; pass. |
| Service pacing | 54 service-rate cells, six rate changes and three shared-service cells; every cell passed the 90% acceptance threshold (the minimum paired delivery/ceiling ratio was 97.9%); zero measured relay drops. |
| Window mismatch | 108 ordered sender/receiver pairs (256 KiB, 2 MiB and 48 MiB), crossed with 0.3/100/400 ms, 0/10 ms compression and one/eight flows; minimum ratio about 99.3%; pass. |
| Live window changes | Six 64 KiB↔2 MiB changes with 0/10/50 ms compression; minimum ratio about 98.8%; pass. |
| Queue and recovery | FIFO bounds, gap deadline, contract lead, retransmission ownership, cancellation and no receive evictions; pass. |

The model ledger reports zero measured relay drops and zero receive-queue
evictions in delivery arms. The ratios compare a candidate with its paired
constant-window reference under the same virtual service; they are acceptance
thresholds for the fixture, not a universal optimum claim.

## Host TCP evidence

Both host runs used the loopback kernel socket origin, provider NAT, Transfer,
gVisor TUN and H1, with three repetitions, three seconds of measured time after a
two-second warmup, 0.3/100 ms RTT, one/eight flows and 10 ms ACK compression.
They were started immediately while other host work was running; this context
is retained in the manifests.

The ordinary run reached roughly 938–942 Mb/s on 0.3 ms paths. At 100 ms, the
default gVisor buffer capped single-flow arms near 148–166 Mb/s; the eight-flow
delivery arms reached about 942 Mb/s while the matched 2 MiB reference remained
near 148 Mb/s. The 48 MiB control removed that single-flow cap (about 936–942
Mb/s in delivery arms), while its 100 ms eight-flow upload varied from about
744–849 Mb/s under the active host load. No measured relay drops, NAT refusals
or receive evictions occurred. These finite samples verify regressions and
fixture behavior; they do not replace a native TUN or WAN campaign.

## Server regression review

The `server/connect` deterministic race tier passed. It covers reliable receive
pressure, resident ingress retirement, H1 batching/FIFO ownership, H3 ACK
reserve configuration, exchange framing and lifecycle joins. The
`server/proxy` deterministic tier passed in the non-race mode required by
`server/test.sh`; it covers memory admission, borrowed packet ownership,
WireGuard/TUN handoff, manager close/join, drain coordination, lifecycle
metrics, window identity restore and bounded traffic metrics.

The configured integration selections were attempted using the environment
loaded by `server/connect/test.sh` and `server/test-env.sh`, with
`WARP_TEST_ENV_FAIL_FAST=1` and the local endpoints. The launcher readiness
marker was absent. Once the local override was applied, the Go preflight
rejected the checked-in fallback PostgreSQL credential at `10.213.0.1:5432`;
the H1/H3, pool-balance, directional TCP and database-backed proxy handoff
tests stopped before creating their disposable database. Per the user's
direction, those integration runs are deferred for a later environment-correct
run. The sibling server checkout was not modified; it remains dirty from
external work.

## Reproduction and artifacts

The runner records source and binary hashes, revisions, Go/OS/CPU, selected
environment, complete logs, status and JSONL ledgers:

```sh
tools/throughput-fix-2.sh correctness /tmp/window-correctness
tools/throughput-fix-2.sh model /tmp/window-model
tools/throughput-fix-2.sh tcp /tmp/window-tcp
CONNECT_WINDOW_TCP_BUFFER_MAX_MIB=48 tools/throughput-fix-2.sh tcp /tmp/window-tcp-48mib
tools/throughput-fix-2.sh ack /tmp/window-ack
tools/throughput-fix-2.sh server-connect-deterministic /tmp/window-server-connect
tools/throughput-fix-2.sh server-proxy /tmp/window-server-proxy
```

Final collected evidence is under [throughput-fix-2-results](throughput-fix-2-results):

- `model-final` — deterministic model ledger and manifest;
- `tcp-final` and `tcp-capacity` — host TCP ledgers;
- `ack-final` — ACK benchmark and head-drain test;
- `server-connect-deterministic-final` and `server-proxy-deterministic-final` —
  passing server tiers;
- `server-functional-final`, `server-connect-full-final` and
  `server-proxy-integration-final` — deferred preflight attempts;
- `failure-before` — retained failure-before experiments.

The peer review and research plan remain in
[THROUGHPUTFIX-PR2.md](THROUGHPUTFIX-PR2.md), and the implementation/results
ledger is in [THROUGHPUT-PR2-RESULTS.md](THROUGHPUT-PR2-RESULTS.md).

## Remaining work

1. Reconcile the local launcher/container PostgreSQL credential and rerun the
   deferred `server/connect` and `server/proxy` integration selections through
   `server/test.sh`.
2. Obtain the native H1/TUN rig source, complete ledger and packet traces, or
   repeat that campaign with an equivalent published harness.
3. Run longer actual-relay pressure, shard-collision, bidirectional and
   multiple-peer campaigns before making a deployment-wide pacing claim.
