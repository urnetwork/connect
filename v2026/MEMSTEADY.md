# Memory-budget performance research

This is the working research document for converting bounded mobile memory
headroom into lower page TTFB and higher transfer speed while keeping the Go
runtime at or below a 24 MiB steady-state target. Its subject is the allocation
of memory budgets: what each budget admits or retains, the performance mechanism
it funds, the marginal result per MiB, and how that memory is reclaimed when the
device changes roles. Update the hypotheses, measurements, and decisions here
as experiments run; `LOWBAR.md` remains the full validation history.

The central question is not "how can every limit be smaller?" It is: **given a
fixed 24 MiB envelope, which movable bytes buy the most H1 TTFB and goodput?**
Reducing allocation churn creates spendable headroom; queue, root, carrier, and
topology budgets decide whether that headroom can do useful work.

## Scope and acceptance signals

The current iteration optimizes the default H1 carrier. H3 and tunneled DNS
need their own follow-up because their working sets and failure modes are
different; an H1 result must not be presented as H3/DNS evidence.

The performance goal is to recover the previously observed fast.com class of
40+ Mbit/s while making ordinary pages feel immediate. Each device run must
bracket the tunnel with a Direct measurement: a radio/ISP path below 40 Mbit/s
cannot prove that target, while a fast Direct result and a slow H1 result makes
the tunnel loss actionable. The checked-in 2026-08-23 record says fast.com
moved 40.4 MiB over H1 in about 60 seconds; that is traffic volume, not a
40.4-Mbit/s rate. The 40+ Mbit/s target comes from the prior product behavior
and must be re-established with elapsed-byte counters in the new campaign.

The primary memory signal is `goRuntimeBytes` from the SDK sampler. It is the
Go runtime's mapped/retained memory, not Android PSS and not iOS Network
Extension `phys_footprint`. The mobile acceptance rules are:

- five quiet connected minutes after a burst: runtime p50 and p95 <= 24 MiB;
- active H1 traffic should stay <= 24 MiB in the accepted profile;
- every sample above 28 MiB is a diagnostic failure and requires allocation
  attribution rather than a larger limit;
- no app, VPN, instrumentation, or carrier termination; all temporary clients
  must be released;
- compare page document TTFB, page load, per-request p95, 1 MiB transfer rate,
  allocation growth, packet-pressure drops, roots, and live exit count. A win
  in only one metric is not enough.

Physical runs use the long-lived Android acceptance session on `zandroid`, a
fresh Chrome process with stable DevTools probes five seconds apart, cache
disabled for every sample, seven Wikipedia navigations, repeated Cloudflare
1-MiB fetches (ten in the final ACK-density arms), and a canonical fast.com
navigation with a 60-second trailing observation window. Device identifiers and credentials are
never retained in checked-in results.

## Memory budget and provider state

At a 24 MiB target the SDK divides the tracked budget into 2.4 MiB DNS,
16.8 MiB client, and 4.8 MiB provider shares. These are admission ceilings,
not eager allocations.

When providing is enabled, the client transfer pair uses 16.8 MiB; half of the
provider share backs the provider client's transfer pair and half sizes the
provider egress NAT. When providing is disabled, the 4.8 MiB provider share is
currently folded into the client transfer pair, making its ceiling 21.6 MiB
(approximately 9.26 MiB resend and 12.34 MiB receive). The provider control
client remains alive so network peer/control state still works.

Merely increasing queue admission ceilings does not guarantee lower TTFB or
higher speed. The performance tier must spend provider-off headroom on a knob
that removes an observed H1 bottleneck, and it must return to the tighter
profile before or while provider work is admitted. Candidate uses are:

1. More H1 quality exits. This increases route choice and parallel-flow
   resilience and can reduce the two-second re-race tail. It also adds a
   persistent client/transport graph, so provider-on transition memory must be
   measured.
2. Byte-accounted packet ownership with an H1/provider-off ACK reserve. A
   256-byte pool class makes ordinary TCP ACK roots cheap while the 1-MiB base
   ceiling still rejects data pressure. The measured reserve ends at 2 MiB;
   3 MiB removed ACK drops but made bulk and Pack handoff performance worse.
3. Larger bounded per-flow packet groups. This amortizes parsing, locking, and
   transfer scheduling, but increases short-lived ownership. It should be
   changed dynamically with provider state, not by raising the global mobile
   ceiling.
4. More ready-only H1 WebSocket batching. It can reduce TLS/socket writes
   without adding a batching wait. The retained writer drains at most 32 ready
   messages, stops ordinary data at 12 KiB, and retains the same fixed 16-KiB
   wrapper. The count increase therefore targets ACK-sized bursts without a
   larger buffer.
5. Pool and GC tuning. Pools reduce allocation and GC churn only when their
   retained floor is bounded. Raising `GOGC` is not acceptable by itself:
   prior `GOGC=50` device arms reached 28.41--29.95 MiB. The accepted mobile
   pacing remains `GOGC=25` unless a complete physical run proves otherwise.

### Current budget ledger

The limits below are different kinds of budgets. An admission ceiling does not
allocate its full value; a retained floor does. Treating them as equivalent
would overstate both the cost of a candidate and the memory it can reclaim.

| Budget at the 24 MiB mobile target | Current value | Allocation behavior | Performance mechanism |
| --- | ---: | --- | --- |
| DNS share | 2.4 MiB | Shared cache/in-flight admission ceiling | Deferred this iteration. Do not borrow it until DNS is reworked. |
| Base client share | 16.8 MiB | Shared resend/receive admission ceilings | Allows active H1 transfers to retain ordered and retransmittable work. |
| Provider share | 4.8 MiB | Movable admission budget | Provider on: provider transfer pair + egress NAT. Provider off: currently all added to client queues. This is the main performance research budget. |
| Provider-off client pair | 21.6 MiB total | About 9.26 MiB resend and 12.34 MiB receive ceilings; not eager heap | Prevents individual flow queues from being the first aggregate limit, but does not bypass per-flow or packet-root gates. |
| Mobile Pack handoff | 1.68 MiB provider-on at the 24-MiB target (1.5-MiB floor); 2 MiB provider-off | Shared retained-byte admission ceiling across all flows; no eager allocation | Absorbs a bounded H1 reader/worker scheduling burst without multiplying a per-flow floor. |
| Mobile receive reorder | 1.68 MiB provider-on at the 24-MiB target (1.5-MiB floor); 2 MiB provider-off | Shared retained-allocation ceiling across all flows; payload remains subject to the independent 768-KiB per-flow protocol window | Charges encrypted outer roots, decoded message/contract roots, packet roots, and the owner envelope while preserving the useful logical receive window. |
| Platform carrier budget | 8 MiB in the Android surrogate | Process-wide carrier admission ceiling derived from the 32 MiB Go soft limit | An H1 carrier claims 256 KiB. The H1 q4 control used 5.5 MiB including the always-live provider control transport, leaving structural room for more H1 candidates. |
| Mobile packet-root gate | 1 MiB base; 2 MiB H1/provider-off ACK maximum | Samples exact owned bytes for the 256-byte and 2,048-byte packet classes | Prevents an asynchronous ingress wave. Data, SYN/FIN/RST, H3/Auto, and provider-on traffic stop at 1 MiB; only exact ACK-only TCP packets can use the second MiB. |
| H1 quality/speed topology | 4 / 1 | Persistent clients, goroutines, queues, and carrier claims | More healthy choices can reduce two-second re-races and spread parallel flows. |
| Per-flow packet transaction | 16 packets / 24 KiB | Short-lived ownership bound | Larger groups amortize parsing/locking/scheduling and can improve goodput if the root gate is not already dominant. |
| Packet pool warm floor | 256 KiB total, split small/full | Eagerly retained after reclaim | Avoids the next cold allocation/GC wave without preserving the burst high-water. More floor buys reuse but directly raises steady memory. |
| Go runtime soft limit | 32 MiB | GC/runtime control, not an allocation reservation | Emergency pacing boundary. It is deliberately above the 24 MiB steady acceptance target and must not be mistaken for permission to retain 32 MiB. |

### Provider-off spend ledger

The 4.8 MiB provider share is the movable source. The present policy assigns
most of it to queue admission, while the exact packet-byte gate and per-flow
group can stop traffic before those larger queue ceilings become useful.
Research candidates therefore reassign *effective* headroom without allowing
the sum of active working sets to escape 24 MiB:

| Candidate spend | Provisional charge | Expected return | Reclaim rule |
| --- | ---: | --- | --- |
| Two additional H1 quality exits (q4 -> q6) | 512 KiB of carrier claims plus measured client graph cost | Fewer empty/benched candidate fields, lower resource-tail TTFB, better parallel-flow placement | Shrink to q4 when provider enables; measure the drain transition because flow-bearing exits cannot be destroyed synchronously. |
| 256-byte packet class + H1 ACK ceiling 1 -> 2 MiB | At most one additional MiB, available only to ACK-only TCP roots | Preserve the browser ACK clock during overload without admitting another data burst | Disable immediately for Auto/H3 or provider-on; admitted roots drain naturally. The 3-MiB arm is rejected. |
| Packet group 16/24 KiB -> 32/48 KiB | At most +24 KiB per concurrently admitted group, plus slice metadata | Fewer group transactions and transfer scheduling calls | Atomic limit swap on provider-on; already-admitted groups drain. |
| H1 ready count 16 -> 32 | No additional buffer: byte stop remains 12 KiB and wrapper remains 16 KiB | Fewer TLS/socket writes for already-ready ACK-sized messages | Retain globally for H1; it cannot wait for another message and the byte bound continues to yield to control traffic. |

Provisional charges are experiment bounds, not accounting facts. Each arm must
record runtime delta, live-heap delta, retained-pool delta, carrier claims,
roots, and exits. The accepted spend is the smallest budget that reaches the
performance plateau; unused provider-off share remains safety margin.

### How to rank a memory spend

For each isolated arm, compute changes against an immediately adjacent H1
control on the same underlay:

- `TTFB return / MiB` = reduction in median main-document TTFB divided by the
  increase in active runtime MiB;
- `tail return / MiB` = reduction in median per-page request p95 (and p95 load
  when enough samples exist) divided by active runtime MiB;
- `speed return / MiB` = increase in median Mbit/s divided by active runtime
  MiB;
- `pressure efficiency` = completed ingress bytes per pressure drop and per
  MiB of allocation growth;
- `steady tax` = change in five-minute runtime p50/p95 after roots and flows
  drain.

Reject a spend that only converts transient ownership into retained steady
memory, improves the median by worsening failures/tails, or requires crossing
24 MiB. Prefer a budget that can be lowered live and stops new admission
immediately over one that requires destroying active flows.

### Throughput-cliff investigation

The first hypothesis was that the former 512-root aggregate gate rejected phone
TCP ACKs while remote payload already occupied full-MTU roots. A deliberately
non-shippable 4,096-root diagnostic disproved that as the primary limiter:
pressure drops fell to zero and roots reached 763, yet median 1-MiB goodput was
only 0.63 Mbit/s versus 0.73 Mbit/s for the adjacent control. The gate is now
byte-accounted instead. Its 1-MiB base admits the largest ordered prefix, a
256-byte pool class charges small ACK/control roots accurately, and explicit
H1/provider-off traffic may rescue ACK-only packets up to 2 MiB.

That policy removed all ACK drops in the ten-transfer density phase and held
active runtime to 20.30 MiB, but median Cloudflare goodput was still only
1.695 Mbit/s. Raising the ACK maximum to 3 MiB also removed ACK drops, yet
median goodput fell to 0.92 Mbit/s and Pack handoff drops rose from 47 to 79.
The 3-MiB arm was reverted. ACK admission is useful overload protection, but
neither root count nor another MiB explains the Direct-to-H1 gap.

The mobile-wide sequence-depth experiments did find one real client limiter.
Global depth 32 and 64 improved pages, but receive-only/send+receive depth 128
did not reproduce the bulk result and crossed about 25.05 MiB under repetition.
Carrier-attributed telemetry then showed the H1 receive handoff reaching all 64
slots while the Transfer-ACK handoff had zero drops. The retained split gives
only H1 receive 64 messages / 128 KiB and lossless full-queue backpressure to
connection/sequence cancellation; send, Transfer-ACK, H3, forward, contract,
and control queues remain at 16. The count, logical-byte, and shared exact-byte
gates remain unchanged, so lossless means preserving an already-admitted
reliable message rather than allowing an unbounded queue.

The remaining bulk limiter is primarily provider-side. Same-session Direct
downloads reached 39--42 Mbit/s after the accepted run and 80--92 Mbit/s after
the NoAck diagnostic, while public H1 stayed near 1--2 Mbit/s. Local provider
grouping and direct TCP-ACK application remove measured message amplification,
allocation, and scheduler work, but public-device throughput cannot improve
until those changes run on a controlled/deployed provider.

### Iterative H1 receive deepening

An opt-in Connect diagnostic now tests the narrower hypothesis that only flows
which repeatedly fill their H1 Pack handoff should earn more depth. A flow
starts at 64 messages / 128 KiB. Two distinct full observations within 100 ms
earn one 16-message / 32-KiB step, up to 128 messages / 256 KiB. A lapse beyond
the window resets the saturation streak. H3, Transfer ACK, send, forward,
contract, and control traffic cannot deepen. The channel reserves pointer slots
to the configured hard maximum, but queued Pack ownership is still admitted
incrementally under the per-flow logical limits and the unchanged shared exact
retained-byte budget. Telemetry records saturation episodes, granted steps,
deepened flows, and maximum earned count/byte limits.

The first physical arm grew counts without growing the 128-KiB logical-byte
allowance. Two flows earned four steps in aggregate, but the largest flow
stopped at 92 queued Packs and 130,978 / 131,072 logical bytes; its maximum
earned count was only 96. This identified the fixed byte allowance as the next
local boundary, not a memory failure: runtime peaked at 21.39 MiB and recovered
to a 19.86 / 20.24-MiB p50/p95.

The paired count-and-byte arm removed that ambiguity. One flow earned the full
128-message / 256-KiB allowance and actually queued 128 Packs / 194,688 bytes.
Runtime still passed at a 22.45-MiB peak with a 20.58 / 20.91-MiB recovery
p50/p95 and no sample above 24 MiB. Performance did not improve: ten Cloudflare
1-MiB transfers had a 1.18-Mbit/s median, and fast.com moved only 1.77 MiB in
75 seconds (about 0.20 Mbit/s) while the adjacent Direct 4-MiB median was
87.4 Mbit/s. All four depth grants occurred during the Cloudflare phase; the
fast.com phase triggered no further growth. Timeout resends reached 620 across
the session. This is direct evidence that an H1 flow can consume the full
client receive allowance without unlocking the public-provider path.

The generic mechanism and schema-11 telemetry remain available for a fixed
provider A/B, but the production <=24-MiB mobile policy explicitly clears all
adaptive fields and stays at 64 / 128 KiB. Reaching 40 Mbit/s now requires
deploying the existing provider-return logical grouping and direct established
TCP-ACK application to a controlled nearby provider, pinning the device to that
exit, and alternating old/new provider binaries. Instrument client-to-server,
server-to-provider, and provider-to-origin goodput, frames/message, socket-read
batch size, CPU, route-write waits, and timeout resends; tune the first measured
boundary below 40 rather than buying another client queue.

The mechanism itself is not a hot-path performance regression. Ten 500-ms
uncontended Pack samples measured fixed/adaptive medians of 58.22/58.20 ns with
zero allocations. Server/default settings leave adaptive depth and retained
Pack scanning disabled. The final complete benchmark-only server tiers passed
190/10/20 samples: production full-payload/ACK-sized H1 TLS medians were
1,032/419.1 ns per frame with 17/10 B/op and two allocations, PERFVAR receive-
credit median was 783.1 ns, and proxy batch-64 median was 6,426 ns. These are
consistent with the adjacent accepted cohort; no server-specific reclaim or
depth override is indicated.

### Reliable-H1 synthetic-loss root cause

A pinned controlled provider finally exposed the first hop whose byte rate
diverged. The host's adjacent direct fast.com result was 1.3 Gbit/s, so neither
the origin nor host uplink was the ceiling. With the accepted fixed H1 receive
depth restored (64 configured slots, eight per negotiated nonzero lane), the
controlled Android run displayed 6.1 Mbit/s. During that single burst:

- the Android platform WebSocket reader refused 530 complete Transfer messages,
  totaling 1,186,363 bytes, because its bounded 32-message route was full;
- only two messages were later lost at the ReceiveSequence Pack handoff;
- the exact shared receive-reorder queue then pinned 1.993 of 2.000 MiB behind
  the resulting holes;
- the provider produced 1,357 timeout and 348 selective-gap retransmission
  writes while sending about 4.15 MiB of new return data; and
- Android Go runtime was 18.38 MiB, proving that this was a liveness/throughput
  failure inside an otherwise safe memory envelope.

This changes the diagnosis. The zero-wait WebSocket-reader handoff was
manufacturing packet loss *above* an ordered reliable TCP stream. Every later
message could arrive successfully yet remain unusable behind the missing
sequence number, and Transfer recovery had to enqueue duplicates behind the
same new traffic. Increasing Pack or reorder depth only stores a larger blocked
tail. The first corrective experiment keeps the channel capacity unchanged and
retains only the one message already read and already charged to the carrier:
when the H1 route is full, the reader waits for route space or connection
cancellation and lets TCP apply backpressure to the peer. H3/DNS/P2P keep their
existing nonblocking policies until their separate iteration.

The first carrier-only candidate proved both the correction and the next
boundary. H1 carrier drops fell from 530 / 1,186,363 bytes to zero, with 25
bounded backpressure observations totaling 52,178 bytes. The unchanged 10-ms
ReceiveSequence Pack wait then dropped 24 messages (versus two in the adjacent
baseline), the reorder queue again pinned 1.994 MiB, provider recovery reached
1,544 timeout plus 564 selective writes, and fast.com displayed 3.5 Mbit/s.
This is not a reason to restore the upstream drops: it is the same synthetic
loss contract one hop later. The second candidate therefore uses the existing
negative H1 handoff setting to wait until Pack capacity or cancellation. It
does not add a slot or byte; per-sequence count/byte gates and the shared exact
2-MiB Pack budget remain the ownership ceiling. H3/unknown handoff stays
zero-wait and H1 ACK handoff retains its separate compact coalescing path.

The combined lossless H1 pipeline removed the cliff. Three consecutive
canonical fast.com runs through the pinned provider displayed 38, 41, and
52 Mbit/s. The runs completed in about 19.8, 48.7, and 14.8 seconds; the latter
two moved about 70.2 and 73.2 MB of new provider return traffic. After all three
runs and a seven-page cohort, Android had recorded 262 carrier-backpressure
observations / 687,039 bytes, zero carrier drops, and 762 Pack waits / 762
successes with zero Pack drops. The shared Pack and receive-reorder queues both
drained to zero. Go runtime peaked at 17.60 MiB, with no sample above 24 or
28 MiB. Provider selective-gap writes rose by 11 during the first run and zero
during each later interval; timeout recovery remains measurable (550 and 627
writes in the first two runs, then 253 across the third run plus page cohort)
but no longer pins a 2-MiB unusable tail. This is the first
controlled physical result to restore the requested 40-Mbit/s class without
buying queue depth or memory.

Page latency did not pay for the throughput win. All seven Wikipedia
navigations succeeded; median load/document TTFB/request p95 were
439.2/181.5/183.58 ms. The one fresh-connection sample loaded in 1,019.3 ms;
the six reused-connection samples loaded in 404.6--517.0 ms with no failed
requests. During the following 345-second quiet connected window, runtime
p50/p95/range/last were 17.57/17.61/17.23--17.61/17.41 MiB. Pack and reorder
ownership were zero in every recovery sample, retained pools peaked at
0.50 MiB, and neither forced GC nor idle trim ran.

Shared server performance remains neutral. Five 300-ms repetitions of every
benchmark in `server/connect`, `server/connect/perfvar`, and `server/proxy`
passed 190/10/20 samples. Production full-payload/ACK-sized H1 TLS medians were
896.2/373.0 ns with 17/10 B/op and two allocations, about +0.5%/+0.7% versus
the adjacent recorded 891.7/370.3-ns cohort. PERFVAR receive-credit improved
673.6 -> 636.8 ns and proxy batch-64 improved 5,406 -> 5,348 ns, with unchanged
allocation shapes. Those sub-percent H1 movements are process noise, not a
server regression. The DB-backed H1 PERFVAR correctness track was attempted
with its documented environment and remains externally unavailable because
the local Redis fixture is down; it is not counted as a passing gate.

The deterministic root-cause gate is intentionally smaller than an Internet
benchmark:

1. Create a one-slot platform receive route and fill it.
2. Offer a second pooled 137-byte message as H1. Assert that the call remains
   pending, increments `QueueBackpressureMessageCount` once, transfers no pool
   ownership early, and records zero queue drops.
3. Drain the first slot. Assert that the exact second slice is delivered once,
   the call returns open/delivered, and the receiver returns its ownership.
4. Repeat with cancellation while full and assert prompt termination without a
   channel send. This pins teardown and pool-return liveness.
5. Repeat the full-route case as H3 DNS and assert the historical prompt drop
   plus exact drop counters. This prevents the H1 correction from silently
   changing unreliable-carrier semantics.

Those cases live in `transport_receive_policy_test.go` as
`TestPlatformTransportH1ReceiveQueueBackpressuresWithoutDropping`,
`TestPlatformTransportH1ReceiveBackpressureCancellationReturns`, and
`TestPlatformTransportH3DnsReceiveQueueRefusalDoesNotWait`. The existing
`TestReceiveSequenceH1HandoffWaitRescuesFullQueue` pins finite waiting, while
`TestReceiveSequenceH1HandoffNegativeWaitPreservesReliableBackpressure` pins
the lossless H1 setting selected after the carrier-only arm and
`TestReceiveSequenceH1HandoffNegativeWaitCancellationReturnsToCaller` pins
teardown and caller ownership. Run the isolated deterministic gate from the
Connect repository with:

```sh
go test ./... \
  -run 'TestProductionCarrierReadersUseModeSpecificReceiveAdmission|TestPlatformTransportH(1ReceiveQueueBackpressuresWithoutDropping|1ReceiveBackpressureCancellationReturns|3DnsReceiveQueueRefusalDoesNotWait)|TestReceiveSequenceH1Handoff(WaitRescuesFullQueue|NegativeWait.*)' \
  -count=50
```

Then run the SDK policy/telemetry gate from the SDK repository:

```sh
go test ./... \
  -run 'TestMobileLowMemoryClientSettingsBoundOwnership|TestTakeMemorySamplesJsonIsOneValidBatch|TestMobileDeviceMemorySampleHotPathDoesNotAllocate' \
  -count=10
```

These tests use filled one-slot queues, explicit release/cancellation barriers,
exact pooled-slice ownership, and counters; they need no Internet timing or
scheduler race to reproduce the boundary. On a physical
acceptance run, sampler schema 11 must show
zero delta in `platformH1ReceiveQueueDropCount`; a nonzero
`platformH1ReceiveBackpressureCount` is expected load evidence, not loss. Also
require the Pack-drop delta and final reorder bytes to return to zero, compare
provider timeout/selective recovery per MiB, and preserve <=24-MiB active and
five-minute steady runtime. A faster displayed result without those queue and
recovery invariants is not an accepted fix.

## Allocation findings and low-churn candidate

A 64 KiB-sampled diagnostic profile before this H1-only pass found these
short-workload allocation sources:

- `sdk.(*IoLoop).run`: 6.34 MiB flat / 12.11 MiB cumulative. Its local
  `[64][]byte` packet-slice storage escaped once per native read burst.
- `maps.clone`: 6.21 MiB, including 6.14 MiB from
  `sequenceAckWindow.Snapshot`. The ACK worker cloned the selective-ACK map to
  ask whether work was pending and cloned it again to consume it.
- message pool take: 3.31 MiB in-use; decoded packet-owner take: 0.63 MiB
  in-use and 3.02 MiB allocation-space.

The retained implementation hoists the TUN packet-slice storage out of the read
loop, copies each native packet into its exact 256-byte or 2,048-byte pooled
class, uses allocation-free ACK `Pending`/`Notify` checks, preserves the
borrowed outer-slice contract at the asynchronous NAT boundary, and admits the
largest ordered prefix that fits the 1-MiB packet-byte gate instead of dropping
an entire native batch. Pool telemetry now reports outstanding packet bytes as
well as roots, so admission observes allocation cost rather than treating a
60-byte ACK like a full-MTU packet.

The provider TCP path exposed another per-ACK allocation: every pure ACK built
a `TcpSendItem`, crossed the per-flow channel, and waited for the flow worker
even though it consumes no TCP sequence space. Established-flow pure ACKs now
apply their monotonic ACK/window/timestamp update directly and wake a blocked
socket reader. Pre-handshake packets and every SYN/FIN/RST/payload packet retain
the ordered queue. The exact local download benchmark reduced median time by
3.1%, bytes/op by about 52%, and allocations/op by about 20%; the direct path
itself is pinned at zero allocations. These changes lower churn without
enlarging a steady working set.

## ACK-path decomposition

There are two independent acknowledgement layers, and changing the wrong one
can add traffic without advancing the browser:

1. A `ReceiveSequence` emits cumulative/selective **Transfer ACKs** for reliable
   Transfer messages. Its worker publishes the first update after an idle
   period immediately, then enforces a 10-ms minimum interval while traffic is
   sustained. It writes one cumulative ACK plus any selective or missing-
   contract ACKs. The sender receives those frames
   through a per-`SendSequence` `AckBufferSize` channel. A full channel is a
   nonblocking handoff drop; the already-existing
   `ClientReceiveStatsSnapshot.AckHandoffDropCount` records it.
2. Android's TCP stack emits ordinary IP **TCP ACK packets** into the TUN. They
   traverse the client outbound `SendSequence` and the provider NAT applies
   their ACK number and advertised window before its upstream socket reader can
   emit more download data. TCP packets intentionally retain end-to-end
   Transfer recovery, even on H1, because a carrier reconnect cannot prove that
   the provider consumed a prior packet.

The global depth-32/64 experiments widened both data handoffs *and* the
Transfer-ACK handoff. Receive-only and send+receive depth 128 did not reproduce
the global-depth bulk result. The next carrier-attributed run resolved the
ambiguity: with an H1-only 64-message receive handoff, `AckHandoffDropCount`
stayed zero while Pack handoff loss reached 2,280. Increasing the ACK channel
therefore spends slots at a boundary that was not full. Top-level
`ClientSettings.SendBufferSize` and `MultiClientSettings.SequenceBufferSize`
are not active hot-path capacities in the present implementation;
device-side forwarding is also not the ordinary H1 client path.

The retained allocation is a carrier-specific H1 receive depth of 64 with the
same 128-KiB encoded-byte ceiling. H3, send, Transfer-ACK, forward, contract,
and control queues remain at 16. The first 1-ms H1-only handoff arm reduced
Pack drops from 82 to one through its page/Cloudflare phase. A later resource
timeout motivated an interim 10-ms Pack-only wait, which converted 9/11 brief
reader/worker scheduling mismatches without enlarging either queue. The pinned
provider A/B subsequently showed that any finite expiry can still manufacture
a permanent sequence hole. The accepted H1 Pack policy therefore waits for
capacity or cancellation under the same fixed count/byte gates. ACK admission
remains at 1 ms and H3/unknown carriers remain zero-wait.

ACK scheduling has a separate low-memory win. Instead of delaying the first ACK
after an idle period by 10 ms, the worker now enforces the same 10-ms *minimum
spacing between writes*: an idle burst is acknowledged immediately, while a
sustained stream remains compressed to at most one cumulative write per
interval. This is a token-bucket/quick-ACK policy, not an ACK-every-packet
policy. It removes one 0--10-ms causal delay at sparse boundaries without
raising the sustained ACK rate or retaining another queue. Shortening the
sustained interval is not currently justified: at 40 Mbit/s, 10 ms is roughly
50 KiB, far below the 512-KiB resend ceiling, and the device saw no inbound ACK
handoff loss.

Packet formation used to work against both ACK layers. The TUN reader can
drain 64 ready packets and the mobile NAT groups up to 16 packets from an exact
flow. H1-only SendSequences now retain up to 16 already-ready frames and 3 KiB
of message bytes in one physical Transfer Pack; H3 and mixed routes keep the
two-frame/one-MTU DATAGRAM-safe bound. Sixteen small TCP ACK packets can
therefore use one H1 sequence number instead of eight. Above that boundary the
H1 WebSocket writer may combine up to 32 already-ready messages in one TLS
write while retaining its 12-KiB drain stop and 16-KiB storage. This preserves
every TCP ACK byte, order, SACK/ECN/window signal, and ordinary Transfer
recovery; neither layer waits for a batch to fill.

The corresponding download-direction batching regression is fixed locally.
Before the hybrid H3 packet lane, the reliable H1 coalescer admitted two frames and up
to 3 KiB of message payload because two MTU packets plus the envelope fit the
deployed 4-KiB H1 message limit. The H3 work lowered the shared byte ceiling to
one 1,100-byte MTU so a message stayed DATAGRAM-eligible, making default H1 pay
the same ceiling. Deployed providers still do: full return packets become one
Transfer message each. Provider socket drains now remain logical groups of
up to 16 frames / 24 KiB until the SendSequence pins its carrier generation.
H1 emits two full-MTU packets per bounded Pack; H3 still emits its existing
DATAGRAM-safe chunks. Contract-bearing and no-contract drains share this path,
and sparse singleton returns keep the raw-frame fast path. A new logical
sequence opens and samples its writer when its first group is selected, so the
first response burst receives the H1 bound rather than requiring a warm Pack.
Focused tests pin
route-generation changes, contract boundaries, retry identity, ownership,
partial admission, exact completion/accounting, and H1/H3 chunk bounds.

On the opposite direction, an established provider TCP flow no longer allocates
and enqueues a `TcpSendItem` merely to apply a pure ACK. The fast path takes the
same per-flow lifecycle lock, updates the reply lane, applies only the monotonic
ACK/window/timestamp state under the TCP mutex, wakes a socket reader waiting
for window room, and returns the packet owner. It cannot run before the SYN has
initialized the flow and excludes every segment that consumes sequence space
or changes connection state. This speeds the acknowledgement that actually
opens provider download progress without weakening Transfer recovery.

A deliberately unsafe diagnostic marked pure TCP ACK packets Transfer-NoAck on
H1. It reduced timeout resends to 21 during the Cloudflare phase and removed
thousands of ACK-of-ACK recoveries, but Cloudflare remained at 1.65 Mbit/s and
Wikipedia tails worsened. Under a hot fast.com workload runtime rose to
27.79 MiB. The shortcut was reverted: every tunneled TCP packet still requires
end-to-end Transfer commit across carrier disconnect and route replacement.
The result is useful because it also rules out downstream ACK-of-ACK head-of-
line blocking as the primary 40-Mbit/s limiter.

Deliberately not first-line changes:

- Dropping superseded pure TCP ACK packets risks changing SACK, ECN, duplicate
  ACK, zero-window, and ACK-clock behavior. Preserve them until packet traces
  prove redundancy under the exact semantics.
- Marking TCP ACK IP packets Transfer-NoAck was measured and rejected. It loses
  commit across an H1 reconnect and did not improve page or bulk performance.
- Adding an ACK batching timer below WebSocket would directly worsen sparse
  TTFB. Only ready-drain batching is eligible.
- A larger 4,096-root gate already produced zero pressure drops without a
  throughput gain, so class-aware control admission remains a tail-safety idea,
  not the explanation for the roughly 40-Mbit/s target gap.

### Logical bytes versus retained allocation bytes

The receive queue needs two simultaneous measurements. `MessageByteCount` is
the protocol payload used by the per-sequence 768-KiB flow-control limit and by
existing diagnostics. The shared mobile budget instead charges what a queued
`ReceivePack` keeps alive: the pooled backing class of the encrypted outer
Transfer frame, every decoded message and contract-frame root, and a rounded
1-KiB decoded-owner envelope. `MessagePoolRootByteCount` reports the actual
256/2,048/4,096/8,192-byte pooled class rather than the visible slice length;
non-pooled slices fall back to their visible length. Saturating addition makes
malformed or synthetic accounting fail closed.

Both totals live in `transferQueue`. Add, duplicate replacement, ordered-tail
eviction, remove, clear, and cancellation update them together under the queue
lock. The per-flow `CanAddWithQueueByteCount` check applies the logical total to
the useful protocol window and only the retained total to the shared budget.
An empty sequence preserves the existing one-item progress exception even when
another flow owns the aggregate window; it cannot admit a second item until
budget returns. The bounded mobile flow count therefore bounds this deliberate
overdraft instead of recreating the former 96-KiB-per-flow floor.
This separation is essential: payload-only charging produced 6.15 MiB of roots
behind a reported 2-MiB queue, while applying the retained charge to both limits
reduced a flow's useful payload window to roughly 250 KiB and stalled the second
Cloudflare sample. The final split held roots to 1.78 MiB, preserved all ten
payloads, and stayed below 24 MiB without a forced collection.

Retained-allocation accounting is explicitly mobile-policy opt-in. Server and
desktop defaults preserve their historical logical-byte queue behavior and do
not pay the root scan. Send queues continue to charge encoded frame length for
both their local limit and shared resend budget; a regression test pins that
the embedded queue item cannot accidentally bypass the `sendItem` override.

## Measurements

All MiB values below are `goRuntimeBytes / 1048576`. Network results are real
route observations and can vary; compare repeated distributions and retain
failed samples.

| Date / build | Carrier and profile | Wikipedia | Cloudflare 1 MiB | fast.com | Runtime / pressure | Decision |
| --- | --- | --- | --- | --- | --- | --- |
| 2026-08-24 `m24-control-recheck-20260824` | Auto, provider off, 4 quality / 1 speed, pre-low-churn control | load median 2773.6 ms; main TTFB 294.4 ms; request p95 median 2244.51 ms | median 9.134 s / 0.88 Mbit/s | load 1326.4 ms; TTFB 916.3 ms; 31 requests, 14 failed | fast max 20.55 MiB; final recovery 20.35 MiB; 2443 drops | Control only. Page tail and bulk rate leave room to improve. |
| 2026-08-24 `m24-prefix-admit-20260824` | Auto, provider off, low-churn + ordered-prefix admission | load median 955.6 ms; main TTFB 354.8 ms; request p95 median 553.56 ms | median 9.664 s / 0.83 Mbit/s, all completed | load 1191.6 ms; TTFB 785.5 ms; 30 requests, 14 failed | H1/Auto phases max 21.59 MiB; recovery live 8.17 MiB; 2036 drops | Keep as candidate. Large page-tail win; bulk is neutral within route variance. H3 result is deferred. |
| 2026-08-24 H1 q4 control, same candidate build | Explicit H1, provider off, 4 quality / 1 speed, 16 packets / 24 KiB group, 512-root gate | load median 2552.3 ms; main TTFB 223.1 ms; request p95 median 2206.95 ms | median 11.540 s / 0.73 Mbit/s; first-byte 760.74 ms | load 1400.9 ms; TTFB 935.8 ms; request p95 6006.72 ms | Wikipedia max 16.95 MiB; 1 MiB max 18.64 MiB / 448 drops; fast max 20.16 MiB / 956 cumulative drops; five exits observed | Fresh H1 control. Main-document TTFB is healthy, but parallel resources repeatedly stall near the 2 s send-retry cadence and bulk ingress hits the root gate. Test route capacity and pressure independently. |
| 2026-08-24 Direct bracket after H1 q4 | Direct Wi-Fi, no tunnel | one Wikipedia run: load 263.2 ms; main TTFB 98.7 ms; request p95 93.31 ms | 38.98, 42.20, and 20.68 Mbit/s; median 38.98 Mbit/s | not run as an acceptance sample | Tunnel runtime not applicable | The radio/origin path can still deliver the requested 40-Mbit/s class. The roughly 53-fold median bulk gap and roughly 10-fold page-load gap are inside the tunnel path or its selected exit, not the local Wi-Fi ceiling. |
| 2026-08-24 `h1-root4096-diag-20260824` | Explicit H1, provider off, q4 / s1, sequence/group 16, root gate 4,096; diagnostic only | not run after the bulk hypothesis failed | 0.63, 0.91, and 0.49 Mbit/s; median 0.63 Mbit/s | not run | zero pressure drops; max 763 roots; max runtime 19.03 MiB; max live 6.96 MiB | Reject. Removing the root gate did not restore throughput and spent memory. Restore 512 and move to sequence-window isolation. |
| 2026-08-24 `h1-seq32-diag-20260824` | Explicit H1, provider off, q4 / s1, global mobile sequence depth 32, group 16/24 KiB, root gate 512 | load median 543.7 ms; main TTFB 220.7 ms; request p95 median 247.34 ms. One cold-origin run was 2534.4 ms; the other six were 514.2--741.0 ms. | 4.65, 5.57, and 2.49 Mbit/s; median 4.65 Mbit/s | page load 1606.9 ms; main TTFB 1053.2 ms; request p95 3549.85 ms; 64 requests / 31 failed | max active runtime 20.34 MiB; max live 8.00 MiB; max pool outstanding 1,127; cumulative pressure drops 961 during fast.com | Strong keep/split signal. Versus the depth-16 H1 control, median 1 MiB goodput improved 6.4x and median page load improved 4.7x without crossing 24 MiB. Test depth 64, then assign the winning depth only to saturated H1 data queues. |
| 2026-08-24 `h1-seq64-diag-20260824` | Explicit H1, provider off, q4 / s1, global mobile sequence depth 64, group 16/24 KiB, root gate 512 | load median 457.6 ms; main TTFB 177.8 ms; request p95 median 230.86 ms. One 2368-ms tail remained. | initial 4.88, 1.66, and 1.17 Mbit/s; after 30 s, 5.17, 1.22, and 6.44 Mbit/s. A 4 MiB sample reached 4.14 Mbit/s and a second timed out. | not repeated; page-focused measurements already established the gain | max runtime 21.54 MiB; max live 8.60 MiB; max pool outstanding 1,118; retained pool 2.56 MiB; cumulative pressure drops 1,417 | Split rather than keep globally. Depth 64 improved median page load another 16% over depth 32 and stayed below 24 MiB, but bulk remained near a 5-Mbit/s plateau with TCP-setup stalls and less memory margin. Test a larger receive-only byte/count window while returning send/control queues to 16. |
| 2026-08-24 direct bracket after sequence diagnostics | Direct Wi-Fi, no tunnel | not repeated | 4 MiB downloads reached 62.06 and 54.50 Mbit/s; median 58.28 Mbit/s | not run | tunnel runtime not applicable | Confirms sustained 40-Mbit/s-class underlay during the same device session. The H1 plateau is not the radio or endpoint. |
| 2026-08-24 `h1-rx128-256k-diag-20260824` | Explicit H1, provider off, q4 / s1; receive sequence 128 / 256 KiB; send, ACK, forward, and control depths restored to 16 | load median 672.5 ms; main request-to-first-byte 219.5 ms; request p95 median 354.91 ms. Two of seven runs retained the roughly 2.25-s tail. | 2.01, 2.51, and 2.59 Mbit/s; median 2.51 Mbit/s | not run after the isolation failed | max runtime 20.09 MiB; max live 7.03 MiB; max pool outstanding 803; cumulative pressure drops 424; zero 28-MiB breaches | Reject receive-only as the explanation. It lost 46% bulk rate versus global depth 32 and 51% versus the settled global-depth-64 median. The H1 download still emits inner TCP ACK packets through the outbound SendSequence; isolate that sequence next and keep its ACK/control channel at 16. |
| 2026-08-24 `h1-tx128-rx128-diag-20260824` | Explicit H1, provider off, q4 / s1; send and receive data sequences 128 / receive handoff 256 KiB; Transfer-ACK, forward, contract, and control depths 16 | load median 561.1 ms; main request-to-first-byte 176.4 ms; request p95 median 284.35 ms. Two roughly 2.4-s tails remained. | 1.10, 1.16, and 1.16 Mbit/s; median 1.16 Mbit/s | not run after the bulk isolation failed | max runtime 19.35 MiB; max live 7.00 MiB; max pool outstanding 657; cumulative pressure drops 347; zero 28-MiB breaches | Reject. Widening both data sequence handoffs did not reproduce the global-depth result and was 75% slower than global depth 32. This points away from raw data-channel depth and toward one of the global constant's other consumers, especially the Transfer-ACK handoff. Public-provider variation remains a confounder, so subsequent A/Bs must use adjacent brackets or a fixed provider. |
| 2026-08-24 `h1-rx64-ack-coalesce-20260824` | Explicit H1, provider off; H1 receive 64 / 128 KiB; send and control 16; immediate-idle Transfer ACK and H1 logical grouping | load median 664.3 ms; main request-to-first-byte 305.0 ms; TTFB 312.1 ms; one 2.71-s tail | one run: 2.44 Mbit/s; first byte 758 ms | not retained | peak runtime 21.20 MiB; Pack handoff HWM 64 / about 98 KiB; 2,280 Pack drops; zero ACK-handoff drops | Keep receive depth 64, not a larger ACK channel. The full receive handoff and zero ACK loss identify the saturated boundary. |
| 2026-08-24 `h1-rx64-ackreserve-20260824` | Prior row plus provider-off H1 ACK-only packet-root reserve | load median 520.6 ms; main request-to-first-byte 197.4 ms; TTFB 202.5 ms; request p95 232.28 ms | median 1.57 Mbit/s | not retained | peak runtime 20.52 MiB; 361 reserve admissions; 76 ACK-root drops; 153 Pack drops | Keep only as overload protection. It protects TCP ACK progress and improves page tails, but alone is not a bulk-speed mechanism. |
| 2026-08-24 `h1-rx128-256k-ackreserve-20260824` | H1 receive 128 / 256 KiB plus ACK reserve; diagnostic | load median 481.5 ms; main request-to-first-byte 184.0 ms; TTFB 189.2 ms; one 3.07-s tail | median 6.73 Mbit/s | 0.99 then 1.2 Mbit/s | first phase peak 22.21 MiB; repeat reached about 25.05 MiB; 2,635 non-ACK root drops and 204 Pack drops | Reject. It crosses the 24-MiB active target and collapses under repeated load. Depth 64 is the memory/performance knee. |
| 2026-08-24 `h1-rx64-ackscan-20260824` | H1 receive 64; allocation-free ACK-only rescue from a rejected native-batch suffix | load median 750 ms; main request-to-first-byte 231.6 ms; TTFB 237.0 ms; no multi-second page tail | median 2.24 Mbit/s | fresh 9.1 Mbit/s; hot 0.52 Mbit/s | peak runtime 22.24 MiB; 266 reserve admissions after page/bulk; 82 Pack drops; hot pressure included 953 non-ACK drops | Keep suffix rescue, but not as a throughput claim. It preserves ACK progress without reordering any two admitted packets; the hot collapse remained upstream. |
| 2026-08-24 `h1-rx64-ackscan-wait1ms-20260824` | Retained candidate: H1 receive 64 / 128 KiB, H1-only 1-ms Pack handoff wait, ACK reserve/suffix rescue, quick Transfer ACK, H1 logical grouping | load median 521.5 ms; main request-to-first-byte 203.2 ms; TTFB 210.9 ms; request p95 227.43 ms; seven warm runs stayed 483--537 ms | median 1.58 Mbit/s; first byte 950.62 ms | fresh 1.2 Mbit/s; hot 0.94 Mbit/s | page/bulk peak 18.63 MiB with only one Pack drop; final peak 21.80 MiB; 19 samples, zero 28-MiB breaches; Pack waits 3 / successes 2 | Keep. The bounded handoff fixes the internal Pack-collapse mode and gives the best repeatable page distribution inside the memory target. The remaining bulk ceiling is not that queue. |
| 2026-08-24 `h1-rx64-acknoack-group-20260824` | Diagnostic only: retained candidate plus pure TCP ACK Transfer-NoAck; provider-return grouping was dormant on the unchanged public provider | load median 1,032 ms; main TTFB median 311 ms; request p95 retained 2.30--5.30-s tails | median 1.65 Mbit/s | canonical fresh 0.64 Mbit/s; an overlapping hot reload failed after about 80 s | Cloudflare peak 20.46 MiB and only 21 timeout resends; hot peak 27.79 MiB, 4,951 pressure drops, 621 timeout resends | Reject and revert. Removing thousands of ACK-of-ACK recoveries did not improve speed and weakened route-replacement delivery. Two fast.com query-cachebuster 404s were invalid harness attempts and are excluded. |
| 2026-08-24 direct bracket after ACK diagnostics | Direct Wi-Fi, no VPN; same attached device and endpoint | not repeated | 38.87, 80.04, 92.28, and 90.01 Mbit/s; middle-pair median 85.03 Mbit/s | not run | tunnel runtime not applicable | Confirms ample 40-Mbit/s-class underlay after the slow tunnel cohort. The retained client path is no longer dropping Pack handoffs during ordinary pages, so provider/exit deployment and controlled end-to-end relay tests are now the highest-value bulk work. |
| 2026-08-24 `h1-ack-smallpool-20260824` | Explicit H1/provider off; retained receive/wait policy plus exact packet-byte accounting, 256-byte ACK pool class, 1-MiB base gate, and 2-MiB ACK ceiling | seven-run load/request-to-first-byte/TTFB/request-p95 medians 363.9/119.2/142.4/177.77 ms; no failed navigation, with one 2.34-s tail | ten runs 1.20--2.90 Mbit/s; median 1.695 Mbit/s | canonical 30-s-settle load 727.8 ms; request-to-first-byte 104.9 ms; TTFB 445.2 ms; request p95 2,170.08 ms; 7/28 requests failed | peak runtime 22.19 MiB; runtime p50/p95 22.13/22.19 MiB; max packet ownership 2.19 MiB / 1,126 roots; 2,164 ACK-reserve admissions and 338 ACK drops across the full session | Keep the 2-MiB ACK ceiling. It improves page distribution and protects ACK progress without spending another data MiB, but 1.695 Mbit/s remains far below Direct. |
| 2026-08-24 `h1-ack-smallpool3m-20260824` | Prior row with a 3-MiB ACK-only ceiling; diagnostic only | load median 393.9 ms | ten-run median 0.92 Mbit/s | load 1,661 ms; TTFB 1,036 ms | peak runtime 21.74 MiB; zero ACK drops but 79 Pack handoff drops | Reject and revert. Eliminating the remaining ACK drops did not improve speed and displaced useful Pack progress. Two MiB is the measured knee. |
| 2026-08-24 `h1-ack-direct-batch32-20260824` | Prior retained client plus 32-message/12-KiB ready-only H1 WebSocket drain; public provider unchanged, so the direct provider ACK fast path was not deployed | seven-run load/request-to-first-byte/TTFB/request-p95 medians 787.5/343.0/349.2/388.84 ms; the excluded cold warm-up loaded in 2,021.8 ms | ten runs 1.22--3.13 Mbit/s; median 2.02 Mbit/s (+19.2% versus the adjacent exact-byte run) | the harness navigation loaded in 2,651.1 ms with request-to-first-byte 467.2 ms, TTFB 1,860.8 ms, request p95 2,325.05 ms, and 7/27 failed requests; a separate canonical page settled at 5.7 Mbit/s after 45 s and moved 28.74 MiB H1 ingress | 29.48-MiB peak runtime, 13.71-MiB peak live heap, 2,137 / 3.85-MiB peak outstanding pool, 6.48-MiB returned pool, 3,577 pressure drops, and three >28-MiB samples. After traffic stopped, one automatic rebuild dropped 5.98 MiB of pool ownership and runtime fell 29.48 -> 19.85 MiB, then held 20.15--20.48 MiB. | Physical speed signal only, not an accepted profile. Bulk median improved, but pages regressed and the sustained real fast.com burst failed the active/post-burst memory gate. The fixed-size ready drain is not a 6-MiB allocation; returned packet high-water and allocator-span retention explain the excess. Reduce that high-water or provider message amplification before release. |
| 2026-08-25 `h1-coalesce-pack2m-20260825` | 32-ready H1 plus one shared 2-MiB Pack-handoff budget; receive reorder still had an uncharged per-flow floor | pre-load Wikipedia median load/TTFB 368.2/143.6 ms; post-recovery median 639.3 ms with one 7.7-s resource tail | ten-run median 1.65 Mbit/s | at least 21.52 MiB ingress in the sampled window | runtime peaked at 30.39 MiB with three >28-MiB samples; packet roots reached 6.51 MiB while the Pack queue itself peaked at only 6.65 KiB; automatic recovery returned runtime to about 20.15--20.48 MiB | Reject. The Pack budget was not the retained owner: roughly 80 receive sequences could each retain their uncharged 96-KiB reorder floor. |
| 2026-08-25 `h1-rxbudget2m-wait5-20260825` | Shared 2-MiB receive budget, zero per-flow floor, and 5-ms H1 handoff wait, but accounting charged protocol payload rather than retained roots | pre/hot/post-recovery Wikipedia median load 573.2/497.7/497.8 ms; median TTFB 214.3/212.4/209.5 ms; no hot tail | ten runs 1.16--7.07 Mbit/s; median 2.62 Mbit/s | adjacent Direct 4-MiB samples were 27.22, 34.82, 46.04, and 41.53 Mbit/s | runtime still peaked at 29.59 MiB with two >28-MiB samples; logical receive use stopped at 2 MiB but packet roots reached 6.15 MiB; recovery p50/p95/last were 20.15/20.61/20.11 MiB; 3 Pack drops and 7 waits / 4 successes | Reject. Payload-byte accounting hid the pooled backing classes, encrypted outer frame, decoded contract/message roots, and owner envelope retained by each queued item. |
| 2026-08-25 `h1-rxalloc2m-wait10-20260825` | First exact retained-allocation charge and 10-ms H1 handoff wait; the same larger charge accidentally also constrained the 768-KiB per-flow logical window | pre-load Wikipedia median load/TTFB 566.7/231.6 ms; hot median 583.1 ms but 3.35/5.76/2.55-s tails | first 1-MiB object completed at 2.11 Mbit/s; the second aborted after 60 s and the remaining cohort was not retried | canonical fast.com completed | runtime passed at 22.83 MiB, roots stayed at 1.71 MiB, and recovery p50/p95/last were 20.67/20.88/20.68 MiB | Reject for performance. A flow saturated near 0.78 MiB of retained charge while it had only about 250 KiB of useful payload in flight. Separate logical flow control from aggregate retained-allocation accounting. |
| 2026-08-25 `h1-rxalloc-separate-wait10-20260825` | Final candidate: 32-ready H1, 64/128-KiB H1 receive handoff, 10-ms reliable-carrier wait, independent logical per-flow window, and exact shared 2-MiB retained-allocation budget | pre-load median load/request-to-first-byte/TTFB 455.1/164.5/169.6 ms; hot median 627.4/245.3/248.8 ms with one isolated reused-H2 5.3-s resource wait and no concurrent tunnel loss; post-recovery median 613.6/223.7/227.8 ms with 7/7 success and no multi-second resource tail | all ten full 1-MiB responses completed at 2.04--6.00 Mbit/s; median 2.78 Mbit/s and 554.22-ms median first byte | at least 20.53 MiB ingress in the inner 75-s counter bracket; public-provider rate remains far below the adjacent 41.53-Mbit/s Direct upper-pair median | active runtime peaked at 21.77 MiB with 8.91-MiB live heap, 1.78-MiB packet roots, exact receive use 2.00/2.00 MiB, and zero samples above either 24 or 28 MiB. Five-minute recovery p50/p95/range/last were 19.91/20.16/19.85--20.20/19.91 MiB with a 256-KiB warm set, zero queued receive bytes, zero forced GCs, and zero trims. Across the burst, 9/11 bounded Pack waits succeeded; two drops returned only 2,880 bytes and all payloads completed. | **Accept the client memory/performance profile.** Exact retained charging fixes the 29--30-MiB high-water without shrinking the protocol BDP window. Do not claim 40 Mbit/s until the retained provider grouping/ACK changes are deployed to a controlled exit and measured end to end. Physical iOS footprint validation remains separate. |
| 2026-08-25 `h1-adaptive-depth-20260825` | Diagnostic: H1 count depth starts at 64, requires two full observations within 100 ms, and grows by 16 toward 128; logical bytes remained fixed at 128 KiB | pre-load Wikipedia median load/TTFB 604.2/252.6 ms; post-recovery 463.8/196.2 ms after one 6.02-s cold restart | 7/10 completed before one 120-s stream abort; completed median 1.54 Mbit/s | 3.29 MiB ingress in 75 s, about 0.37 Mbit/s; adjacent Direct 4-MiB median 86.74 Mbit/s | runtime peak 21.39 MiB; recovery p50/p95 19.86/20.24 MiB. Thirteen saturations and four grants across two flows reached an earned maximum of 96, but actual HWM stopped at 92 Packs / 130,978 of 131,072 bytes. | Reject count-only deepening. It stayed inside memory, but the fixed logical-byte cap became the next boundary and performance remained public-provider limited. Test paired count/byte growth once. |
| 2026-08-25 `h1-adaptive-depth-bytes-20260825` | Diagnostic: paired 64/128-KiB -> 128/256-KiB H1 growth in 16/32-KiB steps under the unchanged exact shared retained budget | pre-load Wikipedia median load/TTFB 404.3/149.8 ms; post-recovery seven-run median 439.6/186.2 ms, including a 2.51-s cold connection setup | all 10 completed; median 1.18 Mbit/s and 476.82-ms first byte | 1.77 MiB ingress in 75 s, about 0.20 Mbit/s; Direct 4-MiB median 87.4 Mbit/s | runtime peak 22.45 MiB; 390-s recovery p50/p95/range/last 20.58/20.91/20.19--20.97/20.19 MiB; zero samples above 24/28 MiB. One flow earned 128/256 KiB and queued 128/194,688 bytes; all four grants occurred before fast.com, which added none. Session timeout resends reached 620. | Reject as the production mobile default. Full adaptive depth is memory-safe in this arm but does not improve bulk or fast.com and increases recovery work. Keep fixed 64/128 KiB; retain the generic opt-in and telemetry only for a controlled-provider A/B. |
| 2026-08-25 `h1-logical-lanes8-20260825` / adjacent lane-zero control | Explicit H1/provider off with fixed 64/128-KiB receive policy; eight bounded five-tuple lanes plus lossless Transfer-ACK overflow folding, followed by a rebuilt lane-zero arm in the same device session | lane 8 median load/TTFB 348.8/126.1 ms; lane 0 median 1,157.8/367.9 ms | not repeated; the earlier complete fixed-depth cohort remains the payload control | lane 8 displayed 10 then 3.6 Mbit/s; its exact repeat moved 3.06 MiB H1 ingress in 19.3 s. Lane 0 displayed 4.4 Mbit/s and moved 18.55 MiB in 34.7 s. Same-session Direct displayed 410 Mbit/s before the first arm and 1.1 Gbit/s after the second. | lane 8 runtime peak 20.60 MiB, 2.00/2.00-MiB receive use, zero >28-MiB samples, and 152 timeout resends in the exact repeat; lane 0 peak 19.43 MiB, 1.41/2.00-MiB receive use, zero >28-MiB samples, and 1,053 timeout resends. Both arms had zero Transfer-ACK handoff loss. | Keep eight lanes as a controlled explicit-H1 client/provider candidate, not as a public-provider speed claim. Client lanes materially improve request/inner-TCP-ACK isolation and page latency, but provider download data remained on lane zero. Enable the same negotiated lanes on a pinned provider sender and deploy provider grouping/direct ACK before the decisive end-to-end A/B. |
| 2026-08-25 `h1-lossless-20260825` | Pinned controlled provider, explicit H1/eight logical lanes, fixed 64/128-KiB H1 receive policy, lossless carrier-route and Pack backpressure, unchanged exact 2-MiB shared budgets | seven-run Wikipedia load/document-TTFB/request-p95 medians 439.2/181.5/183.58 ms; 7/7 success. The fresh connection loaded in 1,019.3 ms; six reused loads were 404.6--517.0 ms. | not repeated; fast.com is the decisive bulk workload for this root cause | consecutive displays 38, 41, and 52 Mbit/s; host-adjacent Direct was 1.3 Gbit/s. The latter repeats moved about 70.2 and 73.2 MB of new provider return traffic. | active runtime peak 17.60 MiB; zero samples above 24/28 MiB; cumulative 262 / 687,039-byte carrier backpressures with zero carrier drops; 762/762 Pack waits succeeded with zero Pack drops; Pack and reorder queues drained to zero. A 345-s recovery measured runtime p50/p95/range/last 17.57/17.61/17.23--17.61/17.41 MiB, zero queued ownership, 0.50-MiB maximum retained pools, and no forced GC/trim. | **Accept the lossless H1 pipeline.** It restores the 40-Mbit/s class and fast reused-page loads by removing synthetic gaps, not by spending more depth or memory. H3/DNS behavior remains unchanged for its future iteration; iOS footprint remains a separate gate. |

### ACK and grouping microbenchmarks

The contract-safe provider grouping change has a deterministic H1 Transfer
boundary benchmark. On the Apple M4 Pro with `GOMAXPROCS=10`, seven 500-ms
samples compared one 16-packet, 1,500-byte provider drain represented as the
old singleton logical groups versus one retained logical group. Median time
fell from 25,579 ns to 9,105 ns (2.81x throughput, 64.4% less time), wire Packs
fell from 16 to 8, allocated bytes from 19,440 to 4,024 per drain (-79.3%), and
allocations from 147 to 51 (-65.3%). This is a same-process Transfer boundary,
not an Internet throughput claim; it proves that the batching change removes
real marshal/recovery work before deployment.

The provider pure-TCP-ACK fast path has a second exact local download result.
Across seven two-second samples, applying established ACK/window updates
directly changed median time from 56,712 to 54,935 ns (-3.1%), throughput from
577.8 to 596.5 MB/s (+3.2%), bytes/op from about 5,351 to 2,560 (-52%), and
allocations/op from 46 to 37 (-19.6%). The direct helper itself remains at zero
allocations. This is provider-process evidence; the attached device still used
unchanged public providers.

At the client H1 TLS boundary, raising the ready-only count from 16 to 32 while
retaining the 12-KiB byte stop changed ordinary full-payload median time from
948.1 to 949.5 ns (+0.15%, neutral). ACK-sized median time fell from 533.9 to
445.0 ns (-16.7%), byte throughput rose about 240 to 288 MB/s, and physical
writes/frame fell from 0.0693 to 0.0443 (-36%). Sparse arrival was neutral
(13,822 versus 13,807 ns), and allocation results stayed 58--81 B/op and 3--4
allocations/op for the corresponding shapes. The count increase therefore
removes ACK-sized writes without adding a timer, buffer, or sparse TTFB cost.

Every benchmark in `server/connect`, `server/connect/perfvar`, and
`server/proxy` then ran again after the ACK fast path and ready-drain changes.
All 175/10/20 current samples passed. Against the preceding small-pool cohort,
time geomeans moved +1.65%, +2.13%, and -3.79%; exact PERFVAR/proxy allocation
counts were unchanged, while Connect's heterogeneous bytes/op and allocation
geomeans moved +2.67% and +0.49%. The directions disagree across unaffected
benchmarks and are treated as run-order host noise; the exact affected local
benchmarks above are neutral or faster. The DB-backed H1 full-TUN campaign
remains blocked: the configured local Redis endpoint returned `host is down`,
and the broad short tier additionally requires unavailable local vault/DB
fixtures. The complete benchmark-only PERFVAR tier is green; no payload-
throughput result is inferred from the blocked fixture.

The final 2026-08-25 server isolation repeated the check after retained receive
accounting landed. The current benchmark-only tiers passed 190/10/20 samples
for `server/connect`, `server/connect/perfvar`, and `server/proxy`. A seven-run
same-binary 8/16/32 H1 sweep measured full-payload TLS at
1,305.04/1,385.02/1,473.51 MB/s and ACK-sized TLS at
167.03/235.18/309.30 MB/s. Thus the production 8 -> 32 ready cap improved
payload throughput 12.9% and ACK throughput 85.2%, reduced TCP writes/frame
25%/75%, and left allocations/op unchanged. A detached
baseline/current/current/baseline isolation then changed PERFVAR -0.46% and
proxy +0.64% overall; all B/op and alloc counts were identical. The roughly
20% previous-day absolute slowdown reproduced in the baseline and was host
frequency drift, not a code regression. Server queues leave retained-root
scanning disabled, so no server-specific reclaim behavior is needed.

The logical-lane follow-up also isolated the compact Transfer-ACK overflow
path. At a clean 50-ms RTT, fixed lane-zero H1 measured 134.551 Mbit/s before
and 134.529 Mbit/s after folding a full ACK handoff directly into the existing
monotonic cumulative/selective window (-0.016%). The fallback adds no queue,
wait, timer, or per-ACK allocation. In the impaired four-flow PERFVAR arm,
lane zero measured 20.247 Mbit/s on a 41.474-Mbit/s underlay and eight lanes
measured 29.058 Mbit/s on a 43.704-Mbit/s underlay: +43.5% raw goodput at
similar calibration. Order-balanced repeats were route-variable, so this is a
causal flow-isolation signal rather than a universal multiplier. It complements
the physical page/retransmission result; it does not overcome a provider that
still puts every return flow on lane zero.

Final-source validation passed the complete Connect and SDK short suites in
199.341 and 97.135 seconds, focused normal/race repetitions, all affected vet
tiers, and the Android AAR/app/test build. After stopping a stale campaign-owned
benchmark probe that had contaminated the broad host cohort, five clean-host
repetitions measured production H1 TLS at 891.7 ns full-payload and 370.3 ns
ACK-sized medians with two allocations/op; PERFVAR receive credits measured
673.6 ns and proxy batch-64 measured 5,406 ns. This rules out a shared-server
performance regression from the ACK fallback or explicit-H1 mobile policy.
The broad server correctness attempt remains externally blocked by unset
`WARP_ENV` and absent vault `pg.yml`; focused tests and benchmark-only packages
do not require those fixtures and are green.

### Current ACK decision matrix

| Option | Memory/performance result | Decision |
| --- | --- | --- |
| H1 receive depth 32 -> 64 | Page median improved from 543.7 to 457.6 ms in the global isolation; carrier-attributed runs filled all 64 slots while staying below 24 MiB. | Keep fixed 64 for H1 only, with the 128-KiB byte cap. |
| Iterative H1 receive 64/128 KiB -> 128/256 KiB | A flow reached the full earned limit and queued 128 Packs / 194,688 bytes at a 22.45-MiB runtime peak, but Cloudflare fell to a 1.18-Mbit/s median and fast.com moved about 0.20 Mbit/s. No depth grant occurred during fast.com and timeout resends reached 620. | Reject for the production mobile policy. Keep the generic opt-in for fixed-provider diagnosis; do not spend the client budget until a controlled provider A/B identifies this boundary. |
| H1 Pack handoff wait 0 -> 10 ms | A 1-ms arm left a rare timeout and multi-second resource recovery. At 10 ms, 9/11 final-run waits succeeded; the two misses returned only 2,880 bytes and every payload completed. The wait does not enlarge either queue. | Keep for reliable H1 Pack handoff only; ACK handoff remains 1 ms and H3/unknown remains zero-wait. |
| Transfer ACK handoff 16 -> 64 | Inbound ACK-handoff drops remained zero while Pack loss was high. | Do not spend memory here. |
| Full Transfer-ACK handoff -> shared ACK window | Clean 50-ms H1 changed 134.551 -> 134.529 Mbit/s (-0.016%). A saturated compact channel now folds progress into its existing monotonic cumulative/selective window without growing the queue or waiting. | Keep as lossless overload handling. This coalesces Transfer protocol ACK state, not inner TCP ACK packets. |
| First Transfer ACK | A fixed 10-ms delay is directly on sparse request/response turns. | Send the first after idle immediately; retain 10-ms sustained spacing. |
| Shorter sustained Transfer-ACK interval | The 512-KiB resend budget holds roughly ten times 10 ms of 40-Mbit/s traffic; no ACK-handoff loss was observed. | Do not add ACK traffic without a causal trace. |
| ACK-only packet reserve | Protects up to 256 extra H1/provider-off packet roots (about 512 KiB worst-case) and preserves exact packet order among admitted packets. | Keep as overload progress protection; disable for Auto/H3/provider-on. |
| ACK-only packet reserve 2 -> 3 MiB | Removed ACK drops but lowered ten-run bulk median 1.695 -> 0.92 Mbit/s and raised Pack loss. | Reject 3 MiB; retain the exact 1-MiB base / 2-MiB H1 ACK maximum. |
| TCP ACK coalescing or dropping | Can alter duplicate ACK, SACK, ECN, and advertised-window semantics. | Reject. Preserve exact packet bytes. |
| TCP ACK Transfer-NoAck | Removed most ACK-of-ACK retries but did not improve physical performance and breaks commit across route replacement. | Rejected and removed. |
| Established provider pure-ACK direct apply | Removes one `TcpSendItem`, queue crossing, and worker wakeup per browser ACK; local download work fell 3.1%, 52% B/op, and 19.6% allocs/op. | Keep with handshake, lifecycle-lock, exact-owner, monotonic-ACK/window, and zero-allocation tests. It requires provider deployment before a device throughput claim. |
| Provider return logical groups | Halves full-MTU H1 Transfer messages, cuts local boundary allocations 65%, and leaves H3 physical chunks unchanged. | Keep 16 frames / 24 KiB logical fairness bound; require deployment/full-TUN validation. |
| H1 logical lanes 0 -> 8 | On the physical client, Wikipedia median load improved 1,157.8 -> 348.8 ms and timeout resends fell 1,053 -> 152 while runtime stayed below 20.61 MiB. Controlled four-flow goodput improved 20.247 -> 29.058 Mbit/s at similar underlay capacity. Public fast.com did not improve consistently because its provider return sender remained on lane zero. | Keep for a symmetric explicit-H1 client/provider A/B. Byte budgets remain shared and nonzero channel capacity is bounded; do not enable default Auto until the provider result covers carrier transitions. |
| H1 ready-only WebSocket drain 16 -> 32 | ACK-sized host throughput rose about 20% and writes/frame fell 36%; full payload and sparse arrival were neutral with unchanged fixed storage. Exact receive-allocation accounting then held the final sustained device run to 21.77 MiB active and 20.16 MiB recovery p95. | Keep; byte stop 12 KiB, wrapper 16 KiB, and no batching wait. Attribute Internet goodput only after a controlled provider A/B. |
| Dedicated priority carrier lane for Transfer ACKs | Could bypass a full data FIFO, but requires a second ordered transport lane and lifecycle/backpressure design. The NoAck diagnostic rules out downstream ACK-of-ACK HOL as the present primary limiter. | Protocol/transport candidate only after direct ACK-write wait telemetry proves contention. |
| Piggyback cumulative Transfer ACK on reverse data | Can remove a separate frame on full-duplex flows, but changes the wire contract and needs downgrade/retry semantics. | Future negotiated protocol candidate. |
| H1 message cap above 4 KiB | A negotiated 16--32-KiB envelope could combine more than two full-MTU packets, but old servers reject it and larger receive buffers consume burst memory. | Server-first capability rollout and an exact 24-MiB device A/B; never enable unnegotiated. |

### Ranked H1 limiters after this pass

| Rank | Boundary | Evidence | Highest-value next action |
| ---: | --- | --- | --- |
| 1 | Reliable H1 carrier-to-route and Pack handoffs | A pinned provider showed 530 carrier drops creating permanent Transfer holes and a full 2-MiB reorder tail at 6.1 Mbit/s. Fixing only that boundary moved 24 drops to the finite Pack wait and measured 3.5 Mbit/s. Making both reliable boundaries lossless produced consecutive 38/41/52-Mbit/s displays, zero drop deltas, zero final reorder bytes, a 17.60-MiB active peak, and a 17.61-MiB recovery p95. | Retain fixed queue/byte gates and lossless H1 waits to cancellation. Page and recovery acceptance pass; use timeout-recovery per MiB—not more depth—as the next optimization signal. |
| 2 | Download Transfer-message amplification | The hybrid H3 ceiling made deployed H1 provider returns singleton full-MTU messages. The retained local group halves their H1 sequence numbers/wire Packs and cuts boundary allocations 65.3%. | Validate exact payload/goodput in PERFVAR when Redis/DB is restored, then on the attached device against that provider. |
| 3 | Public provider/exit deployment | Same-session public-provider results remained slow even after client lanes, while the new pinned current-code provider crossed 40 Mbit/s once both H1 handoffs were lossless. | Deploy the verified carrier/Pack behavior with the provider grouping/direct-ACK work, then repeat pinned old/new and public-exit cohorts. Do not infer public fleet speed from the local controlled provider. |
| 4 | Hot packet-root pressure | Fresh pages stay below the gate, but overlapping fast.com work drove mixed ACK/data drops and runtime growth. Raising roots to 4,096 removed drops without improving speed; receive 128 exceeded 24 MiB. | Reduce messages/allocations upstream with provider grouping. Do not buy speed by weakening the memory backstop. |
| 5 | WebSocket/TLS socket boundary | A 32-message/12-KiB client drain improves ACK-sized host work 16.7% and reduces writes/frame 36% while full payload and sparse traffic remain neutral. After exact receive charging, the final physical arm reached a 2.78-Mbit/s Cloudflare median at a 21.77-MiB active peak. | Retain the bounded ready-only drain with no timer. Attribute the remaining Internet gap only through a controlled old/new provider and adjacent device A/B. |
| 6 | GC/pool retention | Payload-only receive accounting still allowed 6.15 MiB of packet roots and a 29.59-MiB runtime crest. Charging every retained root/owner to one shared budget reduced roots to 1.78 MiB and runtime to 21.77 MiB; quiet p95 was 20.16 MiB without forced GC or trim. | Keep exact retained-allocation accounting and the 256-KiB warm set. Do not replace this bound with more aggressive periodic collection. |

The 40-Mbit/s target was not unlocked by one more ACK queue or deeper sequence.
It was unlocked by making both bounded handoffs above reliable H1 lossless.
The controlled provider moved from 6.1 Mbit/s with 530 carrier drops, through
3.5 Mbit/s when loss moved to the finite Pack boundary, to consecutive 38, 41,
and 52 Mbit/s with zero carrier/Pack drops and zero final reorder bytes. Runtime
peaked at only 17.60 MiB and recovery p95 was 17.61 MiB. Provider timeout recovery remains the next measured
efficiency target, but it is no longer evidence for buying more receive depth.
Public-fleet deployment and an iOS extension footprint run remain distinct
release gates.

## Experiment queue

Run one change at a time where practical, then combine only independently
useful changes. The search covers the whole H1 path rather than assuming that
every slow result is a queue-size problem:

1. **Deployable provider A/B.** Put eight negotiated data lanes and the logical
   provider-return group/direct-ACK path on a controlled current
   server/provider, alternate old/new binaries, and run H1
   download in the full-TUN PERFVAR matrix plus the physical Direct/H1 bracket.
   Pin provider/exit selection; do not compare two public-provider races. The
   local Redis/DB fixture must be restored first. Record goodput separately for
   client-to-server, server-to-provider, and provider-to-origin, plus Transfer
   messages, frames/message, provider socket-read batches/windows, route-write
   wait, timeout resends, per-lane occupancy, exact payload, CPU, and allocated
   bytes. Pin explicit H1 for this experiment; production Auto remains unchanged
   because its Client settings can outlive an H1-to-H3 carrier transition.
2. **Retained mobile profile.** Keep H1 receive 64 / 128 KiB and lossless H1
   carrier/Pack backpressure to cancellation; keep every other Transfer/control
   count at 16, ACK handoff at
   1 ms, the packet-root gate at 1 MiB base / 2 MiB exact H1 ACK maximum, and
   the exact provider-aware shared receive-allocation budget (1.68 MiB on and
   2 MiB off at the 24-MiB target). Leave adaptive H1 depth disabled in the
   mobile policy; its full-depth physical arm did not improve throughput.
   Repeat provider off/on/off under traffic so the provider share transition
   cannot retain an old H1 burst.
3. **Provider logical-group bound.** After the 16/24-KiB deployment A/B, compare
   16/24 KiB with 32/48 KiB on complete 64-KiB socket drains. Physical H1 wire
   chunks remain two full-MTU frames under the deployed 4-KiB envelope; a larger
   logical bound is useful only if fewer admissions/callbacks beat the longer
   retained owner without hurting cross-flow fairness.
4. **ACK-write contention telemetry.** Measure direct Transfer-ACK route-write
   wait/failure by carrier before building a priority lane. Keep instrumentation
   sampled or test-only so an atomic on every ACK does not become the result.
5. **Negotiated H1 envelope.** Prototype a server-advertised H1 message cap and
   compare 4, 8, 16, and 32 KiB. Roll the receiver capability first, preserve
   old-server downgrade, and charge the larger in-flight message to the same
   24-MiB byte budgets.
6. **Exit topology and routing.** Inspect per-exit throughput, affinity,
   provider selection, re-races, and the two-second retry tail. The direct
   85.03-Mbit/s bracket plus 1.65-Mbit/s tunnel result makes provider/exit
   isolation higher value than another client queue increase.
7. **TCP/TUN flow control.** Attribute TUN read/write drops, local TCP ACK
   progress, NAT queue occupancy, receive reorder gaps, resend counts, and
   callback loss. The 4,096-root and Transfer-NoAck results have rejected both
   blunt root enlargement and ACK-of-ACK removal as the primary limiter.
8. **CPU, scheduler, and GC.** Collect Android CPU frequency/load, process CPU,
   goroutine/block/mutex profiles, GC count/pause, allocation rate, heap live,
   retained spans, and pool hit/miss/reclaim data. Repeat hot and thermally
   settled samples so GC improvement is not confused with radio variance.
9. **Server relay and proxy.** Keep the comprehensive server benchmark cohort
   beside each shared Connect change. Re-run full PERFVAR once fixtures are
   available; benchmark-only success does not replace exact payload delivery.
10. **Allocation re-profile.** Confirm that `IoLoop.run` and ACK map clones no
    longer dominate allocation-space. Profiling runs are diagnostic and are
    excluded from performance/memory acceptance comparisons.
11. **Sustained-burst pool high-water.** Retain the final exact-allocation arm
    as the regression control: active <=24 MiB, zero >28-MiB samples, five-minute
    p95 <=24 MiB, full payload completion, and packet roots <=2 MiB. Re-open
    reclaim tuning only if a controlled provider deployment recreates the old
    29.48-MiB crest.

## Notes and pitfalls

- A low live heap with a high runtime value usually means retained spans,
  stacks, or pool capacity, not a leak. Track `goHeapLiveBytes`,
  `goHeapRetainedBytes`, pool retained bytes, and topology beside runtime.
- `poolOutstanding` is live/in-flight ownership; `poolRetainedBytes` is reusable
  free-list ownership. A pool can reduce allocation rate and still make steady
  memory worse if reclaim keeps its burst high-water.
- `packetPressureDropCount` is cumulative backpressure evidence. It is not a
  leak, but a high count paired with low memory and poor throughput means the
  safety gate is probably the active speed tradeoff.
- Queue budgets are shared aggregate ceilings. Per-flow protocol bytes and
  aggregate retained bytes are deliberately separate: the former preserves a
  useful BDP window, while the latter charges pooled backing classes, encrypted
  outer frames, decoded roots, and owner envelopes already admitted
  asynchronously. Reusing one byte count for both caused the measured
  Cloudflare stall.
- Android PSS includes the app, JVM, graphics, mappings, and other native state;
  it cannot validate the iOS extension ceiling. Android is used here to learn
  Go allocation behavior and compare candidates. A physical iOS
  `phys_footprint`/jetsam pass remains mandatory.
- Do not hide route variance by retrying a failed sample. Alternate controls
  and candidates, preserve failures, and use medians plus tails.
