# Window follow-up: implementation and local results

Branch: `throughput-fix-2`, based on `d9eadf29`.
Date: 2026-09-15. The delivery-sized window remains enabled.

Final evidence is summarized in [THROUGHPUT-REPORT-PR2.md](THROUGHPUT-REPORT-PR2.md).
The deterministic and host-local runs completed; configured server integration
is deferred because the running PostgreSQL credentials do not match the
checked-in `server/test-env.sh` fallback.

The independent tests confirm several mechanisms that the published report
could not separate: missing ACK residence in the window calculation, loss
after Transfer delivery, ACK overflow/order/cadence defects, and loss when a
deep H1 window reaches a finite relay queue. An additional upload experiment
exposed a TUN endpoint-lock cycle. These are local reproductions; the missing
native rig and remote packet traces prevent attributing every published
failure to a particular one of these mechanisms.

The [peer review and research plan](THROUGHPUTFIX-PR2.md) preserves the source
review, hypotheses, evidence limits and server follow-up work.

## Changes and failure-before evidence

| Change | Regression evidence |
|---|---|
| Include receiver ACK compression residence in both window terms | The target/delivery tests failed with the original RTT-only calculation. A 0.3 ms FIFO with 10 ms compression delivered **202.5 model Mb/s before, 958.2 after**. Raw RTT remains the basis for resend timers. |
| Advertise optional compression delay | Presence tests distinguish an old peer (10 ms), explicit zero, replacement values, and an absent field after an explicit advertisement. Native/legacy protobuf and encrypted-carrier tests include the added field. |
| Repair delivery sampling | Tests reject checkpoint byte/time mismatches, insufficient history spans and a 64-entry history too short for 400 ms. A large wire-valid compression value also checks overflow-safe multiplication. |
| Respect mismatched endpoint limits | New deterministic tests reproduced a 256 KiB working floor overriding 64/32 KiB advertisements. The effective floor now respects explicit peer and deployment limits before and after sampling. Live capacity changes and both mismatch directions have paired performance coverage. |
| Factor and bound ACK compression | Direct struct tests cover one newest head, ascending SACKs strictly above it, absorption when the head advances, ordered overflow, and an attempted count-limit bypass. Worker tests cover actual encoded response sizes, compression deadlines, early gap wakes, metadata on the cumulative head after SACK overflow, and final cancellation drain. |
| Bound eviction metadata | Before the fix, 4,096 maximal sequence numbers produced a **41,212-byte** carrier. Overflow now remains pending and each ACK carries at most 512 notices. Maximum-field tests cover both codecs, encryption and the 8 KiB minimum carrier contract. |
| Preserve gap proof across compression turns | Exact virtual-time tests reproduce the missing initial-head repair, repeated proof of an unchanged head, and evidence split across snapshots. The unchanged head gets one early wake; normal compression resumes afterward. |
| Notify send admission when capacity returns | The old 2 ms poll failed a virtual-time immediate-release test. The generation notification wakes admission without an artificial poll delay. |
| Recover inner TCP after Transfer success | Before the fix, exact middle/tail/wrap/EOF loss cases failed. The provider now retains bounded origin chunks through cumulative inner TCP ACK and replays the oldest missing segment. Tests verify exact bytes, healthy traffic without replay, IPv6, budget contention, cancellation and already-ACKed read-ahead. A further failure-before test prevents an unreserved replay copy while waiting for shared capacity. |
| Pace H1 window bursts using measured service | Target-only pacing collapsed a 100 Mb/s, 100 ms link to **9.5 model Mb/s**. Whole-window average pacing then underfilled 200/400 ms paths. The current implementation shares one bounded initial probe and pacing clock across logical sequences using the same service. It samples first-delivered wire bytes at ACK arrival and probes spare capacity only when the service is not backlogged. The detailed controls and final validation are described below. |
| Avoid the upload ACK handoff lock cycle | Both `Tun.Write` and `Tun.WriteBatch` failed while a test held the real gVisor endpoint lock. Pure ACKs now avoid that data handoff lock. The classification tests retain data/SYN/FIN/RST handling and cover both IP families, options and ECN. |
| Drain upload ownership after the producer stops | A completed TCP matrix failed teardown with 123 pooled roots outstanding. A virtual-time test then reproduced publication after the socket writer had already drained an empty queue. The producer now closes that queue on every exit and the writer drains through close before `Run` joins it. |
| Release standby after pinned-address failure | Adopted the PR's standby change and added wrapped-error, changed-evidence, H3, successful-reconnect and connected-pin controls. |

### ACK compression contract

`sequenceAckWindow`, in [transfer_ack_compression.go](transfer_ack_compression.go),
owns the independently testable coalescing state. The worker owns timers and
wire emission. Its response begins with at most one cumulative head, followed
by bounded, oldest-first SACKs above that head. A newer head absorbs pending
SACKs at or below it. Contract-recovery requests remain separate evidence.

Every response reserves at most **8 KiB and 32 entries**. The fixed per-entry
reservation is 320 bytes; eviction metadata reduces the usable entry count
(25 entries when no eviction metadata is present). Overflow drains in bounded
responses without adding another compression interval per SACK chunk. New
bursts wait for the normal deadline, apart from the tested gap-proof wake.
If only eviction metadata remains, another carrier waits for the next normal
deadline and repeats the last cumulative head, even when the prior bounded
turn ended with a SACK. The regression
reproduced eight immediate head carriers before this correction. Terminal
cancellation drains already-owned evidence in bounded responses before joining.
The tests check serialized bytes as well as counts, using maximal varints and
encrypted wrappers rather than assuming fresh, small sequence numbers.

## Instruments and performance interpretation

The raw instrument connects two actual Transfer clients through a finite FIFO
that serializes messages at 1 Gb/s and models propagation separately from
queued bytes. It has one serialization worker per direction, avoiding one
goroutine/timer per packet. Virtual time makes its results reproducible.

The TCP instrument adds a **real loopback kernel socket origin**, provider
`LocalUserNat`, and gVisor TUN/application sockets. It preserves packet batches
at the NAT boundary and counts rejected admissions. Production
`RemoteUserNatProvider.ClientReceive` also groups allowed packets into flows
before NAT admission; the fixture bypasses its policy/control overhead. This
is an isolated TCP data-path instrument. It does not include native kernel
TUN, a live WebSocket server relay, WAN conditions or SDK orchestration.

All arms share their packet shape and path settings. `matched` keeps the
2 MiB constant window with the same send/receive budgets and advertisement;
`constant` additionally restores historical unbudgeted/no-advertisement
settings. `ceiling` uses a 48 MiB constant window. `path-rtt-only` isolates the
old ACK-residence assumption, and `unpaced` isolates H1 pacing. `delivery`
includes the fixes. Small private test seams configure these before workers
start.

Each host comparison brackets the candidate with matched A/A runs. The JSON
ledger retains every arm and comparison; it marks A/A drift above 10%, a
ceiling below 90% of configured link capacity, and stalls. These flags limit
interpretation and do not discard failed runs. Three-second, three-repetition
final confirmation cells are regression evidence, not precise estimates of
rare stall rates.

### Deterministic matrix and service-pacing controls

The final deterministic model selection passed **213 paired cells**, with
**390 ledger rows** and no relay drops in any delivery arm. Earlier target-rate
pacing passed the 36-cell RTT matrix but failed a slower-service control.
Subsequent experiments exposed long-RTT underfilling, queue loss and mismatch
failures. Those failed experiments remain part of the evidence.

| Model matrix | Cells | Lowest candidate/reference throughput |
|---|---:|---:|
| Original RTT/flow/compression matrix | 36 | 99.9% |
| 1/10/100 Mb/s service rates | 54 | 97.9% (above the 90% acceptance threshold) |
| Service capacity changes | 6 | 100.0% |
| Four sequences sharing one service | 3 | 99.9% |
| Ordered endpoint window sizes | 108 | 99.3% |
| Live receiver-window changes | 6 | 98.8% |

These are measured virtual-FIFO ratios, including ordinary interval-edge
effects, rather than estimates of remote throughput. The complete ledger and
source/binary manifest are in `throughput-fix-2-results/model-final`.

The current model selection includes the original **36 RTT/flow/compression
cells**, **54 cells at 1/10/100 Mb/s**, six 10↔100 Mb/s capacity changes, and
three shared-service rates with four logical sequences and eight flows.
It also includes **108 ordered window-mismatch cells** and six live receive
window changes. Sender/receiver capacities of 256 KiB, 2 MiB and 48 MiB are
crossed with 0.3/100/400 ms RTT, 0/10 ms compression and one/eight flows.
The live receiver changes cover 64 KiB ↔ 2 MiB with 0/10/50 ms compression.
Each mismatch reference is capped to the smaller endpoint; the test checks its
calibration as well as candidate throughput, both bounds, per-flow progress,
steady-state relay loss and receive-queue eviction. Window-limited intervals
span twenty residences to bound measurement-edge effects. These fixtures offer
flows round-robin within each logical sequence, with separate lane producers.
The FIFO itself has deterministic tests for serialization, propagation, queue
bounds, and rate changes while frames are queued. Shared-service calibration
uses two BDPs per sequence, with a 256 KiB floor, instead of injecting minutes
of low-rate traffic into the large-window control. Every calibration setting
and the slow-link opening-train warmup are recorded in the ledger.

The service unit tests check every send prefix against the byte/time envelope,
rate increases and decreases, bounded idle credit, one shared probe, split
probe boundaries, cancellation, sibling isolation, and shutdown ownership.
They also cover these additional root causes:

- A canceled short write was admitted when it did not need a timer.
- A paced resend batch delayed application of newly arrived ACKs. The send
  loop now applies them between recovery writes.
- Requiring a whole initial window before trusting service allowed a slow
  relay to fill before the sender used valid delivery evidence.
- A slow H1 FIFO exhausted the two-deferral limit while still delivering.
  Extra deferrals now require fresh cumulative progress on an H1-only paced
  path; silence, mixed paths and unreliable delivery keep ordinary recovery.
- ACK processing time could double an otherwise steady service measurement.
  A local arrival timestamp survives handoff and coalescing.
- A head repairing a gap could count an already-SACKed suffix as new service.
  First-delivery accounting is independent of the retained-window total and
  of recovery's resettable SACK flag.
- Growing queue RTT extended old fast-rate evidence. Fresh samples now replace
  it within a feedback interval based on the service's stable minimum RTT.
  A four-bucket replacement limit was rejected: it forgot a correct 125 MB/s
  observation before faster sends could return ACKs over the 400 ms path.
- Independently compressed sibling ACKs inflated 12.5 MB/s of service to
  **781.25 MB/s**. Arrival buckets accept out-of-order worker application, and
  each measured rate covers at least one advertised compression interval.
- A receiver at its exact gap deadline repeatedly armed a zero timer. Expiry
  now includes equality and releases the held items at that deadline.
- Short opening trains could lose their first checkpoint at bucket boundaries,
  or fit entirely inside one bucket. A deterministic fast-train control read
  **4.26 MB/s instead of 125 MB/s** before preserving its endpoints. Small-window
  matrix cells exposed the same issue after a long idle gap.
- A proposed compressed-tail extrapolation was rejected: reusing the prior
  reply's bytes with the next reply's spacing doubled a controlled slow rate
  from **125,000 to 250,000 B/s**. Partial tails remain conservative observations;
  variable-sized replies, shared phases and replacement of old evidence are
  tested explicitly.
- Large flight alone was mistaken for a standing queue, stopping discovery on
  a long fast path. Before a complete service residence has been delivered,
  the stop condition also requires excess observed ACK residence.
- Compressed peaks could preserve a full shared queue. Queue evidence switches
  the estimate to sustained delivery over a feedback interval and multiple
  compression turns; pacing then leaves five percent of that capacity to drain
  it. Four deterministic byte/time controls verify actual drain capacity.

The opening probe covers up to two compressed reply intervals, bounded by
twice the ordinary initial window (at most 4 MiB with the default 2 MiB
initial). This remains one shared allowance and never replenishes on idle.
Ordinary discovery permits ten percent above measured service; queued service
uses a five percent drain margin. All service cells now also require zero
relay drops during their measured interval, while retaining startup and
transition drops separately.

Pacing counts the first actual wire envelope, including encryption overhead,
and credits it once. Cancellation releases a sequence's remaining shared
backlog. Service state is reference-counted with sequence lifetimes. The
ordinary per-sequence window still owns its existing memory/flight limit.
All model rates are application payload rates in a virtual FIFO, not host
or remote H1 measurements.

### Adjacent root causes

Following `CODESTYLE.md`, the review traced each root cause through sibling
call paths. The [review matrix](THROUGHPUTFIX-PR2.md#12-adjacent-root-cause-review-and-deterministic-completion-gate)
maps all earlier findings to their deterministic coverage.
`transfer_window_adjacent_test.go` adds these reproductions and boundary controls:

| Case | Before | Corrected behavior |
|---|---|---|
| Unbudgeted sender with a smaller known limit | Held 2 MiB despite 64/32 KiB peer/local limits | Holds the constant only within known limits |
| Later receive-capacity increase | Old delivery immediately reduced the new 2 MiB allowance to 256 KiB | Restarts the delivery evidence boundary on increases; repeated unchanged advertisements still converge |
| Window-limited idle inside a service interval | Read 31.5625 MB/s from a 125 MB/s active train | Sustained queue estimation requires continuous delivery across its interval |
| Standalone delivery history | Forgot 125 MB/s before feedback could return | Retains evidence for the stable feedback span and expires it afterward |
| Contract announcement multiplication | Overflow returned zero instead of 2 MiB | Uses wide multiplication and saturates the scaled result |
| Local pacing inside service RTT | A 10 ms pacing wait made a 100 ms path appear to take 110 ms | Samples first actual write through ACK arrival, excluding worker delay and ambiguous carrier/retransmission evidence |
| Zero advertisement | Previously inherited the 256 KiB working floor | Preserves only the queue's existing one-message progress allowance |

The large-target boundary control already passed; it is not counted as a
reproduced defect. The actual-write timestamp adds eight bytes per retained
send item, bringing the tested size to **576 bytes**. ACK arrival timestamps
remain local state and do not change the wire protocol.

The focused correctness selection, including these adjacent cases, passed
under the race detector (**89 top-level tests**). The final model selection
passed **213 paired cells** (**390 ledger rows**), including all **108 mismatch
cells** and **six live receiver-capacity changes**. The lowest model
candidate/reference ratio was **98.8%**; measured relay drops and receive
evictions were zero. Two earlier host-sensitive regression failures
(experimental lane recovery and a short-path throughput comparison) passed
when isolated; their failed full-sweep readings remain in the evidence.

### TCP startup and fixture limits

The first TCP upload pilot correctly reported stalled one/eight-flow cells
before the TUN ACK fix. They remain failures in the diagnostic record. After
the fix, short two-second samples still averaged below the large-window
ceiling. A ten-second instrument check exposed the ramp directly: the 100 ms,
eight-flow delivery arm read **626.2 Mb/s in its first measured second**, then
**939–942 Mb/s** for the remaining nine. Its ten-second mean was 908.9 Mb/s.
The ceiling also ramped, but earlier. The updated fixture adds two seconds of
TCP warmup, records its duration, and publishes one-second interval rates.
This changes the steady-state measurement boundary; it does not erase the
startup difference or justify a claim about first-byte/finite-transfer latency.

The unbudgeted default gVisor buffer range used here limits the 100 ms **single-flow** TCP cell to
roughly 166 Mb/s in both delivery and ceiling arms. That is reported as an
instrument-capacity limit, with the window rule's effect unresolved above it.
Larger TCP buffers are a separately labeled capacity control.
The optional `CONNECT_WINDOW_TCP_BUFFER_MAX_MIB` control records an explicit
per-connection TCP maximum in every cell. A 48 MiB maximum matches the TUN
ceiling derived from a 384 MiB process budget, while keeping the Transfer
comparison's 48 MiB budget fixed.

The final confirmation ledgers are in `throughput-fix-2-results/tcp-final`,
`throughput-fix-2-results/tcp-capacity` and `throughput-fix-2-results/ack-final`.

## Server regression review

The review includes `../server/connect` at
`77201554c49ec05bde83ec038bba6c600972892c`, compiled against this branch using
its existing local module replacement. Server unit tests pass under the race
detector for frame limits, carrier compatibility, reliable receive pressure,
resident ingress retirement, H1 bounded batching/order/ownership and H3 ACK
reserve configuration.

The sibling checkout also contains independently edited resident lifecycle
changes. The regression review includes those working-tree changes: lazy
forward-shard allocation, worker registration while a producer is admitted,
and a final drain after callback owners join. Their three new deterministic
tests passed in the expanded server race selection. The build manifest records
the dirty source hash; this task has not modified the server checkout.

There are two distinct server pressure boundaries: **256 callback-ingress
entries per destination shard**, which retire a full resident generation, and
**4,096 downstream forward entries**, which drop on full at production's
zero timeout. ACKs share those paths with data. Our finite FIFO reproduces
the latter mechanism; it does not prove which boundary failed on the remote
relay. The [server review](THROUGHPUTFIX-PR2.md#9-serverconnect-regression-review)
specifies the missing collision, reconnect and actual-relay experiments.

The deterministic race-enabled `server/connect` selection passed. The full
integration selection was attempted with the environment loaded by
`server/connect/test.sh` and `server/test-env.sh`, fail-fast enabled, and the
documented `10.213.0.1` endpoints. Its launcher readiness marker was absent;
after the local override, every database-backed case stopped at PostgreSQL
authentication because the checked-in fallback credential does not match the
already-running container. The H1/H3 variants, pool-balance test and
directional TCP test are deferred for a later environment-correct run.

All three 64 MiB directional TCP trials are retained here, including startup:

| Direction | Trial 1 | Trial 2 | Trial 3 |
|---|---:|---:|---:|
| Upload, MiB/s | 13.06 | 110.45 | 105.75 |
| Download, MiB/s | 111.78 | 109.14 | 110.78 |

The test's maximum-of-three summary would hide the cold upload. These are
local integration regression measurements, without an old-window paired arm.
Upload counts completed application writes; download counts application reads.
The multi-client fixture permits direct-route negotiation, so its throughput
is not a forced-H1 measurement. The isolated TCP matrix counts origin reads
for upload and explicitly fixes its carrier to H1.
The pool-balance fixture also uses blocking forward admission, so it does not
measure production's drop-on-full queue under sustained pressure.

The sibling server checkout remains dirty from external work and was not
modified by this task. The failed integration logs are retained under
`throughput-fix-2-results/server-functional-final` and
`throughput-fix-2-results/server-connect-full-final`.

### `server/proxy`

The deterministic proxy tier covers memory admission, borrowed packet
ownership, WireGuard/TUN handoff, manager close/join, drain coordination,
lifecycle metrics, window identity restore and bounded traffic metrics. It
passed in the non-race mode required by `server/test.sh`; reproduce it with
`tools/throughput-fix-2.sh server-proxy`. Its two database-backed handoff
tests were deferred at the same PostgreSQL preflight and are retained as
`throughput-fix-2-results/server-proxy-integration-final`.

## Reproduction

Run from the connect checkout:

```sh
tools/throughput-fix-2.sh correctness /tmp/window-correctness
tools/throughput-fix-2.sh model /tmp/window-model
tools/throughput-fix-2.sh tcp /tmp/window-tcp
CONNECT_WINDOW_TCP_BUFFER_MAX_MIB=48 tools/throughput-fix-2.sh tcp /tmp/window-tcp-48mib
tools/throughput-fix-2.sh ack /tmp/window-ack
tools/throughput-fix-2.sh server-connect-deterministic /tmp/window-server-connect
tools/throughput-fix-2.sh server-proxy /tmp/window-server-proxy
```

For the server integration selection, use its environment-loading entry point:

```sh
cd ../server/connect
./test.sh -run '^(TestConnectH[13](Encrypted(AllowFallback)?)?|TestExchangeRelayPoolBalance|TestConnectMultiClientTcpDirectionalPerformance)$' -count=1
```

The endpoint and credential mismatch observed on this machine is recorded in
the server section above; the configured integration tier is deferred rather
than silently bypassing `server/test.sh`'s preflight.

The script writes a test-binary SHA-256, source SHA-256, revisions, Go/OS/CPU
manifest, explicit experiment environment, complete run log, exit status and
JSONL ledger. It records only experiment-related environment variables.
Source hashes are checked again after compilation to reject a mixed-source run.
The final host runs were started immediately under the existing host workload;
their manifests retain the process context. They are useful regression
readings with contention noted, not isolated capacity claims.
The Go toolchain changed externally from 1.26.3 to **1.26.7** during this work;
the confirmation manifests pin 1.26.7 on Darwin/arm64.

Useful overrides include `CONNECT_WINDOW_PATH_RTT_US`,
`CONNECT_WINDOW_PATH_FLOWS`, `CONNECT_WINDOW_PATH_ACK_MS`,
`CONNECT_WINDOW_PATH_SECONDS`, `CONNECT_WINDOW_PATH_REPETITIONS`,
`CONNECT_WINDOW_PATH_ARMS`, `CONNECT_WINDOW_PATH_LANES`,
`CONNECT_WINDOW_PATH_PAYLOAD`, and `CONNECT_WINDOW_PATH_DROP`.
`CONNECT_WINDOW_MODEL_RTT_US` narrows the deterministic matrix for diagnosis.
`CONNECT_WINDOW_TCP_BUFFER_MAX_MIB=48` selects the separate TCP capacity
control; its default value of zero retains the ordinary TUN settings.
Full carrier/SDK/native-TUN confirmation, longer reliability runs, multiple
peers, bidirectional saturation and actual resident queue competition remain
the broader campaign described in the research plan.
