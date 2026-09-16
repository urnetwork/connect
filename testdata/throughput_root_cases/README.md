# Throughput failure conditions

These are regression assertions, including failures that still need fixes.
They use explicit worker barriers, virtual time or exact timestamp sequences.
No failing case is skipped or converted into an expected-pass wrapper.

## Production cases

The 33 package tests added in this change were run three times under race on
commit `17780670` plus the tests. Thirteen pass all repetitions and twenty fail
all repetitions: 39 passes, 60 failures, no skips or race warnings. This source
excludes the uncommitted drain correction and every experimental receiver-service
estimator. The earlier 22-case run remains archived separately.

| Failure condition | Test file | Required controls |
| --- | --- | --- |
| A small compressed reply opens the next flight before the old tail, letting a window gap lower measured service | `transfer_window_compressed_flights_test.go` | Exact outstanding bytes and physical-tail ownership |
| Retaining a partial-feedback read poisons the rate saved by a successful drain | `transfer_window_successful_drain_feedback_test.go` | Receiver timing present/absent, no intermediate read, read-only statistics, identical successful drain and RTT proof |
| A first write waits beyond its original ACK lifetime | `transfer_window_initial_lifetime_test.go` | Recovery before/after lifetime; non-regenerable recovery remains retained |
| A retry, younger record, FIFO head, controlled drain or late timer wake hides expiry | `transfer_window_pacing_worker_lifetime_test.go` | Actual send worker, distinct older/younger deadlines, cancellation, live sibling and physical proof preserved |
| A drain waits after retry, carrier change or cancellation makes its physical delivery proof impossible | `transfer_window_pacing_unprovable_drain_test.go` | Invalidation before/during the wait, fresh-tail recovery, repeated notifications, unrelated probe ownership |
| A paused carrier reader compresses observed delivery and inflates the next burst | `transfer_window_pacing_carrier_buffer_test.go` | Fixed queues; 0/5/10/20/50 ms release delays; no drops/retries; unstalled and genuinely faster serializers |
| Short 10 Mb/s cells with 50 ms ACK compression lose service | `transfer_window_service_failure_cells_test.go` | Both one and eight flows; original warmup, serializer, reference and finite-queue gates |

Run these families from the repository root:

```sh
go test -race -count=3 -run '^TestWindowPacing(CompressedFlights|Initial|Carrier|Lifetime|ShortCompressedService|SuccessfulDrain|RetriedTail|InvalidatedTail|UnconfirmedCarrier|CanceledTail|FreshTail|AbortedDrain|RepeatedTail)'
```

The unchanged broad performance matrix remains a separate gate. The eleven
proved-RTT history tests committed in `17780670` already cover stale delivery,
delayed physical confirmation, read ordering, fixed bounds and other carriers.
The working drain correction passes its eleven liveness roots, but that
correction is excluded from the committed-source baseline above. Its wider
performance acceptance remains open.

## Existing root and adjacent coverage

These earlier tests remain part of the acceptance gates; the new cases extend
their coverage rather than replacing their assertions.

| Failure family | Existing package test files |
| --- | --- |
| One cumulative head, oldest-first SACKs above it, head absorption, pacing and maximum response size | `transfer_ack_compression_test.go`, `transfer_ack_bounds_test.go` |
| Window mismatch, peer/configured/memory limits and fresh evidence after capacity changes | `transfer_window_mismatch_test.go`, `transfer_window_adjacent_test.go`, `transfer_window_clamped_fixture_test.go` |
| Per-service byte/time burst bounds, isolation, cancellation and delayed wakes | `transfer_window_pacing_test.go`, `transfer_window_pacing_wakeup_test.go`, `transfer_window_pacing_flight_test.go` |
| Bucket boundaries, zero hold, burst resets, late observations and source idle | `transfer_window_bucket_stats_test.go`, `transfer_window_burst_stats_test.go`, `transfer_window_host_feedback_test.go` |
| Physical retry timing, failed writes, carrier changes and original lifetimes | `transfer_window_retry_physical_time_test.go`, `transfer_window_retry_physical_adjacent_test.go` |
| Receiver timing identity, queued RTT versus unloaded RTT, delayed confirmation and long replies | `transfer_window_receiver_rtt_test.go`, `transfer_window_receiver_baseline_probe_test.go`, `transfer_window_receiver_baseline_order_test.go`, `transfer_window_pacing_paired_probe_worker_test.go` |
| Proved RTT changes invalidating old window history, with carrier and fixed-bound controls | `transfer_window_refill_proof_test.go`, `transfer_window_refill_adjacent_test.go`, `transfer_window_refill_order_test.go`, `transfer_window_refill_carrier_test.go` |

The broad model, TUN and server regression gates remain separate. Deferred
database-backed server setup is not counted as a passing or failing Go test.

## Experimental receiver cases

The `receiver/` sources are a runnable research fixture, not production code.
The runner applies checked-in patches to pinned commit `272f95e5` in a new
directory, freezes the local glog dependency, and records source, binary and
log hashes. It leaves the working checkout untouched. Receiver tuples still
use a bounded test-only lookup; no new wire fields or overhead are claimed.

```sh
python3 tools/replay-throughput-fix-2-receiver-roots.py /tmp/receiver-roots --variant hybrid
```

Use a fresh output directory for each run. `--count` defaults to 3. The default
selection runs 36 roots and controls; `--run` can select a particular case or
an included affected-cell model. A nonzero exit means a real failing assertion.

| Variant | Pass/fail over three repetitions | Failure conditions preserved |
| --- | --- | --- |
| `early-handoff` | 84 / 24 | One receiver endpoint suppresses ordinary discovery; queue and forward-delay failures |
| `hybrid` | 87 / 21 | Client queue, upstream carrier queue, bucket placement, paced/buffered source clocks, forward-delay crossing and peak expiry |
| `queued-endpoints` | 90 / 18 | Client queue omission passes; upstream buffering, scalar queue controls and forward-delay failures remain |
| `sender-confirmed-rise` | 93 / 15 | Bucket-shifted buffering still inflates both clocks; cold discovery and genuine increases regress; forward-delay failures remain |

Every variant also checks physical retry-byte accounting, once-only logical
credit, ambiguous retry endpoints, shared-service and lane isolation, reordered
ACKs, source/window/controlled pauses, immediate wakeups, active siblings,
genuine slowdowns, the sender's 0.95 pacing factor, and reverse ACK compression.
Forward-delay cases require retaining known service until a fresh post-change
pair can distinguish unchanged capacity from a genuine decrease.

The two full model runs that pass all 27 tests and 818 readings do **not** pass
all these roots. The queued variant's model success does not establish that a
userspace read timestamp measures physical arrival under upstream buffering.

Complete outcomes are in `throughput-fix-2-results/root-condition-complete`,
the earlier `throughput-fix-2-results/root-condition-final`, and
`throughput-fix-2-results/receiver-root-replay`. Raw logs stay in the local run
directories. The first experimental replay export had a duplicate test
definition; that setup failure is excluded from runtime results.
