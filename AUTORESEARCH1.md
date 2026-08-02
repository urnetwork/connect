# AUTORESEARCH1 — optimization techniques applied 2026-07-23 → 2026-07-24

Executive digest of the auto-research campaigns run over the last two days
across `connect`, `sdk`, the apps (`apple`, `android`), and `server/proxy`.
Method for every campaign: measure on an env-gated kernel → record settings +
result → make one educated adjustment → re-measure → keep or revert, until no
further educated adjustments. Full experiment logs, failed branches, and
design rationale live in `PACKETRESEARCH1.md` (§ references below). All work
is uncommitted, pending review.

## Headline results (loopback kernels; on-device validation pending)

| Area | Metric | Before | After |
|---|---|---|---|
| Transfer pipeline | throughput (6-peer kernel) | 44.3 MiB/s | **92.00 MiB/s median (+108%)** |
| Transfer pipeline | 12-peer | — | 72.7 MiB/s |
| Transfer decode | owned v2 frame | 372–385 ns, 488 B, 9 allocs | **300–301 ns, 0 B, 0 allocs** |
| Exact provider profile | allocated objects / bytes | 908,035 / 202.53 MiB | **555,887 / 170.63 MiB** |
| Dead-peer recovery | detect | 30.7 s | 11.76 s |
| Dead-peer recovery | traffic gap / 90% recovered | never (regressed) | 12.00 s / 12.90 s |
| DNS (isolated query, dead first pick) | answer latency | 812 ms | 53–55 ms |
| DNS (warm burst, stale primary) | p95 / max | 924 / 1,604 ms | **274 / 303 ms** |
| Window formation | kernel pre-phase rate | 104 pps | 200 pps (target) |
| iOS footprint | total memory budget | one opaque knob | 32 MB total, per-area budgets |
| Provider load | goroutines peak / loaded / final | ~1,018 / 1,002 / — | **639 / 623 / 473** |
| Host physical-memory check | Go peak / footprint / RSS | — | **26.1–26.8 / 36.50–36.67 / 55.70–56.34 MiB** |

## 1. Memory architecture (PACKETRESEARCH1 §1–§3)

- **Two explicit knobs replace one opaque one.** Process-global
  `SetMemoryLimit(total)` sizes only the shared message pools (packet 12 :
  large-object 2, of 34) + the Go soft limit; per-device
  `MemoryTargetByteCount` (default 20 MB; iOS apps set 32 MB total / 20 MB
  device; proxy hosted devices 32 MB) splits dns 2 : client 14 : provider 4.
- **Live byte budgets, not static caps**: `MemoryTarget` (nil-safe
  acquire/release) + `TransferMemoryBudget` with floors that guarantee
  progress; budgets count only above-floor borrowing. Instances *admit
  against* budgets at creation (e.g. p2p streams decline when the client
  budget is exhausted) rather than failing mid-flight.
- **Dynamic reallocation**: provide-mode off folds the provider share into
  the client share live (`applyProvideMemorySharesWithLock`); hosted devices
  never allocate a provider share.
- **Pool discipline**: `{2048,4096,8192}` classes, drop-on-return, constant
  byte floors, and packet-only prewarm bounded by min(cap/4, 1 MiB).
  Each class has four exact-cap free-list shards; the immutable buffer id
  returns it to its acquisition shard. Ownership is documented and
  enforced: send = handoff; retain = share-read-only/copy — including slices
  crossing send boundaries.
- **GC pacing per platform**: GOGC iOS 10 / Android 50 / else 100. Lesson:
  pool caps behave as live heap for GC purposes; oversizing them triggers
  assist + pressure drains (the first on-device regression).
- `MemoryUsed()` / `DeviceLocalMemoryUsage` expose per-area tracked usage.

## 2. Transfer pipeline throughput (§4–§5)

- **Leak fix on the 4-arm receive path** (delivered check + pool return).
- **Bounded ingress backpressure**: `IngressDispatchTimeout` 5 ms.
- **Tier-2 structural: multi-frame wire Packs** — `SendPack.Frames`,
  `SendMultiWithTimeout`, provider `ReceiveBatch`, batched sequence read-loop
  drains. Validation found the original ≤16 frame / ≤32 KiB benchmark bypassed
  the production 4 KiB framer. The retained bound is **≤2 frames / ≤3 KiB**,
  and `SendSequence.Run` opportunistically coalesces a compatible ready
  neighbor without adding a fill delay.
- **Two-channel selects** where Go special-cases them.
- Harness correction mattered as much as code: the original kernel
  under-measured by timing the wrong span.

## 3. Dead-peer recovery (§10, §13.1)

- **Busy-flow liveness probe** (`CPingBusyStaleTimeout` 5 s): a flow that is
  sending but has received no ack probes the peer with a control ping on a
  snappy ack budget; unanswered → error the client → flows RST → window
  refills. Detection 30.7 s → ~11.8 s without touching `AckTimeout` (false
  positives avoided).
- **Queue-wedge hardening** (the §13 regression fix): "busy" = recent send
  *or outstanding unacked bytes* (`sendNackCount`) — under backpressure the
  resend queue blocks new sends exactly when the probe matters; probe writes
  fail fast (max(1 s, stale/4)) instead of blocking 15 s; two unsendable
  probes in one continuously stale episode error the client instead of
  silently exiting the ping loop; the counter resets in healthy episodes.
  Lesson: state-based signals beat rate-based ones under backpressure, and
  probe constants couple to buffer sizing (the memory rebalance had flipped
  the race).
- **Suspect-state routing** (§13.4): while a probe is outstanding the client
  is suspect; new flows race healthy siblings first.
- **Comparative blackhole** (§13.6): a silent first connect next to
  traffic-passing siblings is cut at 10 s instead of 30 s.
- **Fail-fast RST on removal** (pre-existing, verified): evicted clients'
  flows get synthesized RSTs; apps re-dial instead of timing out.

## 4. DNS flow (§11)

- **Hedge-on-quiet** (`DohServerRaceMaxInFlight` 4): an isolated query races
  its fanned-out servers immediately (812 → 58 ms when the first pick just
  died); a burst keeps the 750 ms stagger so it does not double its own
  volume on the shared h2 pipe. Admission now uses one shared atomic active
  resolution count instead of the racy length of an HTTP semaphore: eight
  concurrent queries with raceMax=2 admit exactly 8 primaries + 2 hedges.
- **Wave-capped burst concurrency**: min(32, byte-target-derived) primary /
  min(12, …) fallback — bursts complete in waves the pipe can carry; the
  byte budget stays the memory governor. The current production-shaped
  64-query kernel peaks at exactly 32 reservations.
- **Widened shared pipe**: mux DoH gvisor TCP buffers 16 → 64 KB (UDP 32 KB).
- **16 KiB response ceiling doubles as the per-request budget reservation**,
  making parallelism byte-budget-governed (`target / 16 KiB` floor).
- Methodology lesson (§11): loopback kernels are blind to shared-bottleneck
  dynamics — the first stagger-0 change shipped a real on-device first-load
  regression; volume-multiplying changes need a modeled narrow pipe or
  on-device validation.

## 5. First-load program (§12)

1. **DoH connection pre-warm** at connect: warm queries park on the
   still-forming tunnel and complete the TCP+TLS+h2 handshakes at the first
   usable moment — self-timing, off the user's critical path.
2. **TLS session resumption** (`ClientSessionCache` per dial path; 1-RTT
   re-dials) + idle timeout 5 → 15 min. Separate caches for tunnel vs host
   dialers so ticket reuse cannot link the real IP to the tunnel egress.
3. **Adaptive local-fallback handicap**: 250 ms while the tunnel-DoH is
   unproven or stalled (≥2 consecutive failures), 5 s once proven; an
   unraced 2 s warm probe re-proves recovery so the mux can never pin cold.
4. **Per-server DoH score persistence** (7 d staleness, seed clamped to 8.0):
   first fan-outs pick last session's fastest server instead of
   uniform-random.
5. **First-load timeline instrumentation**: per-first-flow dns
   query→answer, tcp/443 syn→synack, first byte (`[firstload]` logs +
   `GetFirstLoadTimelineJson`); self-deactivates to one atomic load per
   packet after 16 flows / 24 dns / 60 s.
6. **Window formation off the critical path**: formation logged
   (`[multi]window formed in Xms`); empty-window send retry 2 s → 200 ms
   (kernel pre-phase rate 104 → 200 pps); window identity persistence on
   mobile (owner + destination-fingerprint + 4 h TTL guards) skips one auth
   api round trip per window client at relaunch and lets provider NAT flows
   resume after a brief kill.

## 6. Mid-session stalls (§13)

- **`connect.NetworkChanged()`** process broadcast (AddMemoryShedder
  pattern): platform transports kick their live h1/h3 connection *and* their
  reconnect backoff (immediate re-dial over the new path); the mux drops
  pooled DoH connections, flips cold, re-warms. Wired: apple NWPathMonitor
  (interface-set signature dedup), android onAvailable (pending AAR).
  Replaces multi-second "waiting for ping timeout" hangs on wifi↔cell.
- **RTT-scaled resend floor**: cold floor 2 s only while no rtt samples
  exist; measured paths floor at 300 ms with scale 2.0 (jitter/ack-compress
  headroom); pre-existing per-item exponential backoff caps duplicates.
  Bounds both per-loss pauses and route-failover latency; throughput kernel
  unchanged (71 MiB/s).
- **Verified already-landed** (audit-before-build lesson — two of seven
  assessed "gaps" were done): provider unknown-TCP-flow RST
  (`EnableOrphanRst`, 256/s, PROXYDRAIN1) and CONNECTDRAIN2 make-before-break
  (ResidentMigrate frame + provider transport handoff + drain broadcast,
  default-on since 2026-07-19). The remaining window-client handoff was
  subsequently implemented in §10.5; excused reconnect remains its failure
  fallback, not its normal drain path.

## 7. Methodology techniques (§6, §9)

- **Env-gated measurement kernels** so suites stay fast: `URNET_AUTOTUNE`
  (memory/throughput), `URNET_RECOVERY` (dead peer), `URNET_DNSFLOW`
  (rtt/parallelism), each with env knob families for the loop.
- **One-change-at-a-time with revert**: every adjustment is recorded with
  its measurement; no-impact changes are reverted (targeted inverse edits —
  never `git checkout` on a shared tree).
- **Surrogate-blindness guard**: loopback results that multiply request or
  stream volume must be re-validated against a modeled narrow pipe or on
  device before becoming defaults.
- **Determinism over sleep-tuning in tests**: injected latencies, atomic
  toggles (e.g. `responseDelayMs`), state-based assertions.
- **Instrument before tuning**: the first-load timeline (§12.5) exists to
  validate every constant in §5–§6 on device before further adjustment.

## 8. Adjacent packet-path validation (§14)

- **Production envelope first**: a benchmark route that bypasses framing
  cannot validate message size. Two encrypted MTU packets are now tested
  against the 4 KiB minimum transport limit.
- **Allocation work with explicit ownership**: fused AEAD output and pooled
  decrypt are zero-allocation; decoded payloads are pooled through all exit
  paths; raw v2 IP extraction and borrowed synchronous path parsing avoid
  wrapper/address allocations while public APIs keep owned lifetimes.
- **Remove recurring synchronization objects**: the ack compressor uses one
  reusable capacity-one signal; route/TCP/UDP waits reuse timers.
  `time.After` allocation in the exact profile fell 2.748 MB → 25.5 KB, and
  the former ack notification hotspot (~23 MB / 216K objects) disappeared.
- **Match concurrency to necessary work**: UDP outbound writes run in the
  sequence goroutine and drain its existing bounded queue, removing one
  goroutine and one queue per UDP flow. Three fresh provider runs hold loaded
  heap at 7.3 MiB and loaded goroutines at 807–810.
- **Publish immutable startup state before workers run**: a focused race pass
  found multi-client window workers reading the performance profile while the
  constructor applied its direct-mode override. Profiles are cloned before
  atomic publication, which now covers initialization and runtime changes;
  pre-sharding post-fix throughput was unchanged (75.60–78.76 MiB/s).
- **Bounded pool sharding after measurement overturned arithmetic**: the
  current mutex profile attributed 605 ms release wait to the single class
  lock in a 1.23 s load. Four shards cut it to 103 ms (-83%) and total mutex
  delay 3.36 → 1.96 s; parallel pool operations are 169–175 ns (from
  202–211 ns), while exact retained-buffer caps are unchanged. Fixed
  bookkeeping costs ~54 KiB across all classes.
- **Current conservative result**: seven fresh-process autotune runs
  86.73–90.57 MiB/s (median 89.31), loaded heap 5.41–5.60 MiB. Exact
  allocation volume is ~212 MB versus ~272 MB at validation start; test and
  gVisor payload scaffolding dominate the remaining bytes.
- **Rejected**: aliasing individual generated-protobuf ID fields into a
  retained parent buffer collapsed throughput to 9–31 MiB/s and increased
  4 KiB pool misses. The next decoder optimization must be a whole-lifetime
  value design, not piecemeal aliasing.
- **Bound semantics**: pool caps limit retained free-list bytes, not in-flight
  traffic; `MemoryTarget` may admit one oversized acquisition when empty;
  Go's memory limit is soft and not RSS. These distinctions are required when
  claiming predictable memory.

## 9. Feedback round (2026-07-24): degraded mode + richer transition signals

- **Degraded performance mode** (`DeviceLocal.SetPerformanceDegraded` →
  `DegradedLivenessScale` 3.0): low power mode / thermal ≥ serious /
  constrained path (iOS), battery saver (android) ease the busy-stale probe
  windows so slow-but-alive devices are not removed as dead. Kernel:
  detect 30.8 s degraded vs 10.9 s normal, recovery intact.
- **More OS events into the SDK**: iOS `defaultPath` KVO + `wake()` (post-
  sleep re-dial — previously unhandled) + gateway-aware path signature;
  android link-properties fingerprint on the non-VPN internet request
  (the default-network callback sees only the VPN tun while up);
  `ClientStrategy` drops pooled api connections on NetworkChanged.
- Deferred: p2p conn kick on network change (per-conn lifecycle work).

## 10. Remaining-leads pass (§15)

This pass closed every remaining in-repository lead from §8. The common
design rule is explicit lifetime + explicit bound: reuse is not considered a
memory optimization unless its maximum retained state is known, and a
timeout is not considered a latency control unless it can begin while the
path under test is blocked.

### 10.1 Lifetime-safe value decode and receive ownership

Applied changes:

- `unmarshalOwnedTransferFrame` decodes the normal v2 wire shape into a
  `decodedPackOwner` instead of allocating a graph of generated-protobuf
  objects. Message, sequence, contract, and path IDs are exact-length
  validated and copied into inline 16-byte `Id` storage.
- The production maximum of two frames uses inline `protocol.Frame` values
  and inline pointer slots. More-than-two legacy/untrusted frames remain wire
  compatible and allocate only in proportion to that exceptional input.
- `Pack`, contract frame, `Tag`, path, carrier, role, and companion have one
  declared owner. `encrypted_transfer_frame` is the only borrowed wire slice;
  AEAD consumes it synchronously before the outer input buffer is returned.
  Queued/callback-visible message bytes remain owned through the bounded
  message pools.
- Ownership now moves exactly
  `Client.run → ReceivePack → receiveItem → callback/ACK copy → release`.
  The common `ReceivePack` and `receiveItem` envelopes are embedded in the
  owner because their lifetimes are successive, never concurrent. The
  receive method value is cached instead of rebuilding a closure per packet.
- Release paths return decoded frame bytes on malformed input, admission
  failure, duplicates, queue drain, callback completion, and shutdown. The
  outer pooled frame is returned before the owner is made reusable, avoiding
  a reuse race found by the focused race pass.
- RTT tags are copied into `sequenceTag` values. ACK head/selective state and
  snapshots use `sequenceAck` values rather than per-packet pointers, and one
  capacity-one notification channel coalesces ACK wakeups without allocating
  a broadcast object per update.
- Reuse is four exact-cap shards, not `sync.Pool`: at most 256 combined
  protocol/receive owners (~232 KiB on measured arm64) plus 256 synchronous
  outer wrappers (≤96 KiB). The exact provider profile needed 166 owners, so
  the cap has 54% observed concurrency headroom while preserving a hard
  retained-state ceiling.

Validation/result:

- owned decode: **300.0–300.8 ns, 0 B, 0 allocs**, from
  372.2–385.4 ns, 488 B, 9 allocs;
- exact profile: **555,887 objects / 170.63 MiB**, from
  908,035 / 202.53 MiB — 38.8% fewer objects and 15.7% fewer bytes;
- `ReceivePack`, `receiveItem`, receive method closure, and `sequenceAck`
  allocation sites disappeared.

### 10.2 Sharded ordered UDP receive dispatch

Applied changes:

- Each IPv4/IPv6 `UdpBuffer` owns `ReceiveShardCount` shared FIFO dispatch
  queues; zero selects the measured default of four. Workers start lazily, so
  an unused address family retains queue storage but no worker goroutines.
- New flows are assigned round-robin to one shard for their full lifetime.
  Each socket has one read producer, which makes per-flow enqueue and callback
  order deterministic without sequence numbers or a reorder map.
- A worker batches only adjacent items for the same flow. The first
  different-flow item is held as the next pending FIFO item, preserving shard
  order rather than searching/reordering the queue for batching wins.
- Aggregate userspace receive capacity is exactly
  `ReceiveShardCount × SequenceBufferSize`, independent of live or historical
  flow count. Cancellation drains and returns every queued pooled packet.
  Directly constructed test sequences retain synchronous ordered delivery.
- The former receive channel and goroutine on every `UdpSequence` were
  removed. The already-applied outbound design still writes in the sequence
  goroutine and reuses its bounded send queue.

Validation/result:

- 12 flows / 2,400 packets preserve count and per-flow order at count 10 and
  under the race detector;
- assigning 10,000 flows does not change dispatch queue capacity;
- steady dispatch is 297–323 ns with zero steady-state allocations;
- provider goroutines fell from 826–827 peak / 807–810 loaded / 657–660
  final to **639 / 623 / 473**.

### 10.3 Warm-state DoH staggering with shared-pipe admission

Applied changes:

- `DohServerWarmStagger` defaults to **100 ms**. It is used only when
  `DohPathWarm` says the tunnel has answered and has not crossed its
  consecutive-failure threshold; cold, new, and stalled paths retain the
  conservative **750 ms** stagger.
- `UpgradeMux` supplies that state from its existing atomic tunnel-DoH
  proven/failure counters. Persisted server scores influence ordering but
  never declare a newly formed transport warm.
- `DohServerHedgeReserve` defaults to four. One primary semaphore is shared
  by the remote and local DoH clients and is clamped to leave at least one
  global HTTP slot. On a warm path, first-wave requests can use only
  `MaxConcurrentHttpRequests - reserve`; timed hedges can enter the reserved
  portion of the global HTTP semaphore.
- Existing quiet-query racing remains: no more than
  `DohServerRaceMaxInFlight` active resolutions bypass staggering. Burst
  admission still uses the shared atomic counter, HTTP request cap, and
  16 KiB-per-request memory reservation.

Validation/result from the four-slot/10 ms shared-pipe model:

- healthy 20/80 ms servers: 750 and 100 ms both produce p95 **174 ms** and
  72 requests;
- a near-immediate 1 ms stagger produces the same latency but 124 requests
  (**+72%**), so it was rejected;
- stale dead primary + 20 ms survivor: p95 **924→274 ms**, maximum
  **1,604→303 ms**, requests 103→105;
- without reserved hedge slots, a dead first wave filled the HTTP cap and
  only 3–4 of 64 queries completed by three seconds.

### 10.4 Route-full detection at admission

Applied changes:

- `SendSequence.packMutex` is now an `RWMutex`. Concurrent producers hold a
  read guard and wait on the bounded pack queue independently, so a finite
  liveness probe can start its own timeout even while an application writer
  is waiting indefinitely.
- Close first cancels the sequence, waking blocked producers, then takes the
  exclusive guard before closing the queue. This retains send/close safety
  without serializing unrelated admissions.
- Full queues are not treated as failed routes. Transient backpressure may
  still drain normally; client removal still requires two consecutive
  unsendable busy-stale probes in one stale episode.

Validation/result:

- deterministic queue test parks an infinite writer behind a full channel
  and proves a second writer independently observes a 25 ms timeout;
- the 64-frame route kernel changed from no recovery within 40 seconds to
  detect **11.76 s**, traffic gap **12.00 s**, recover90 **12.90 s**.

### 10.5 Window-client make-before-break

Applied changes:

- Added the optional `MultiClientGeneratorTransportMigrator` capability, so
  custom/in-process generators remain source compatible while
  `ApiMultiClientGenerator` can retain and replace platform transport
  handles.
- `multiClientChannel` accepts `ResidentMigrate` only from a control source
  and calls the migration capability when present. Data-plane peers cannot
  schedule transport replacement.
- The API generator keeps one bounded state entry per live window client:
  current transport/settings/auth, migration-in-progress state, and no
  unbounded history. Client removal removes the entry and closes its current
  transport.
- Duplicate migration frames coalesce. Absolute schedules beyond five
  minutes are clamped; replacement connect is bounded to 60 seconds.
- The replacement uses the same `Client` and `RouteManager`. Ownership is
  rechecked after the schedule wait and again before swap. The old transport
  closes only after `ConnectedNotify/IsConnected` proves the replacement has
  registered routes.
- Timeout, construction failure, or concurrent removal closes the
  replacement and leaves the old route intact. Temporary overlap is at most
  one replacement per migrating client; excused reconnect remains the
  server-supported failure fallback.

Validation/result:

- tests cover control-source authorization, schedule clamp, duplicate
  suppression, old-route survival before connect, successful swap, timeout
  retention, removal races, and race-detector execution.

### 10.6 Final performance and physical-memory measurements

- Five fresh-process autotune runs: **89.79–94.84 MiB/s**, median
  **92.00 MiB/s** (previous median 89.31); loaded heap 6.16–6.61 MiB.
- Three prebuilt provider-binary runs: **54.6–58.8 MiB/s**, Go peak
  `Sys-HeapReleased` 26.1–26.8 MiB, loaded heap 6.7–6.8 MiB.
- The same runs under `/usr/bin/time -l` measured macOS physical footprint
  **36.50–36.67 MiB** and RSS **55.70–56.34 MiB**. Prebuilding excludes the
  compiler/toolchain, but the process still includes peer/test scaffolding;
  these are regression measurements, not an iOS/Android capacity claim.
- The roughly 10 MiB gap between the Go gauge and physical footprint, and
  roughly 30 MiB gap to RSS, confirm that `debug.SetMemoryLimit` is a soft
  Go-runtime target—not an RSS, physical-footprint, jetsam, or LMKD ceiling.

## 11. Verification and remaining external work

Completed local verification:

- `connect`: repository-wide `go test ./...` passed (492.334 s);
- `sdk`: repository-wide `go test ./...` passed (432.396 s);
- the decoder lifetime, ACK snapshot, UDP ordering/capacity, warm DoH/reserve,
  route admission, transfer budget, and window migration tests passed under
  `go test -race` at count 3;
- all changed Go sources are `gofmt` clean and the changed-file
  `git diff --check` is clean.

The code-path leads are closed; these release gates require environments not
available to the host kernels:

- **Real-device memory:** sample iOS packet-extension
  `TASK_VM_INFO.phys_footprint` at 20–50 Hz and Android VPN-service total PSS,
  alongside RSS, Go gauges, goroutines, pool/budget use, native/code splits,
  available-memory pressure, and jetsam/`EXC_RESOURCE` or LMKD/
  `ApplicationExitInfo` evidence.
- **Device workloads:** cold and repeated warm first loads, 64 unique DNS
  names, provide-on TCP+UDP churn, wifi↔cell, wake, and live drain migration,
  followed by a 30-minute settle. Require no memory kill, no monotonic
  post-settle growth, and peak footprint/PSS below 80% of the measured kill
  threshold on the lowest supported device class.
- **Warm DoH A/B:** compare 750/100/0 ms on the same device/network. Keep
  100 ms only if healthy p95 changes <5%, stale-primary p95 materially
  improves, physical memory stays within run noise, and request/byte volume
  remains near 750 ms. The shared-pipe model predicts 0 ms fails volume.
- **First-load constants:** validate the 300 ms RTT resend floor, 2.0 RTT
  scale, 250 ms cold fallback, 200 ms formation poll, 10 s comparative
  blackhole, four-hour identity TTL, and two-query warm count from the
  on-device first-load timeline.
- Rebuild the Android AAR (`make build_android`) so Kotlin memory-target and
  `NetworkChanged` call sites ship with these Go changes.
- Server `TestExchangeDrainMigrateE2e` still requires the warp-vault
  environment; deterministic client-side handoff tests pass locally.
- P2P/WebRTC connections still need direct `NetworkChanged` kick lifecycle
  work; platform-route failover remains the current backstop.

## 12. macOS connect → disconnect → connect CPU/pause pass (2026-07-24)

This was a diagnostic/research pass; no Apple or SDK runtime change was
applied. The current macOS app builds successfully as a Debug arm64 app.
The host did not have a logged-in URnetwork profile or active/registered
URnetwork VPN service, so an end-to-end app + installed extension Instruments
recording could not be produced without changing host state. The conclusions
below therefore distinguish deterministic code/probe results from hypotheses
that still need the release-style on-device trace in §12.7.

### 12.1 Core SDK leak and pause hypotheses falsified

- `TestDeviceLocalLifecycleChurn` completed 60 full create → traffic → close
  cycles with goroutines **8 → 8**, heap **1.3 → 1.8 MiB**, file descriptors
  **8 → 8**, and about **40 MiB** peak process footprint. This does not support
  an old `DeviceLocal` or packet worker surviving every extension restart.
- Three uninstrumented `TestDeviceLocalReconfigurationChurn` runs passed:
  goroutines **41 → 37**, **42 → 41**, and **44 → 48**; FDs ended at 8, 9,
  and 9. A simultaneous CPU+heap+trace+`GODEBUG` run missed the strict settle
  threshold (**41 → 54**) but did not reproduce without the instrumentation,
  so it is recorded as probe perturbation, not a production leak.
- The 60-cycle prebuilt lifecycle profile held at 9 goroutines/9 FDs.
  Natural and test-forced Go stop-the-world edges were normally below 2 ms.
  A few forced-GC phases approached 16 ms; the test explicitly forces GC in
  its stability sampler, so those are not evidence of production pauses.
- Reconfiguration used about 800 ms of sampled CPU over 6.78 s (11.8%).
  The profile was GC, netpoll, syscall, and test-stability work rather than a
  hot SDK spin loop. The trace showed 242.59 ms cumulative cancellation delay
  over the whole churn run, not one 243 ms stop. macOS certificate verification
  accounted for 206.78 ms total during DoH/TLS warmup.
- `TestDeviceRemoteStaysConnected` and
  `TestDeviceRpcSetRpcServerIdempotent` both passed at count 3. A stable remote
  held one connection, and repeatedly applying identical RPC server settings
  did not add reconnects. A tight RPC-redial loop is not supported.
- The extension already rejects a duplicate start with identical
  configuration, invalidates packet-read generations before close, cancels
  subscriptions/path monitoring, and closes the device on stop. The old
  `readPackets` callback cannot recurse after its generation is stopped.

The Go dataplane can still consume legitimate CPU under traffic, but it is not
the leading explanation for CPU that grows after each lifecycle cycle.

### 12.2 Deterministic controller/window-monitor retention multiplier

The highest-confidence defect is shared UI/SDK lifecycle ownership:

- `NetworkApp.setupConnectViewModel` opens the intended connect controller.
  `MainNavigationSplitView.init` opens another controller as a side effect
  every time SwiftUI creates the value. `MainView` observes `DeviceManager`,
  so connect/reconnect publications can reconstruct this child. Commit
  `65e9a36` added the extra controller as a workaround for a macOS disconnect
  bug and left a TODO. `MainTabView.init` contains the same workaround, so the
  ownership bug is shared even though macOS residence/reconstruction makes it
  more visible.
- `viewControllerManager.OpenConnectViewController` retains every controller
  in `openedViewControllers`. Swift calls `controller.close()` directly rather
  than `device.closeViewController(controller)`, so the manager entry is not
  removed. This direct-close pattern also exists in the other stores.
- `ConnectViewController.Close` cancels the controller and removes only its
  connect-location subscription. It does not call `ConnectGrid.close`, so a
  current grid retains its window callback.
- `DeviceRemote.windowMonitor` retains every monitor by window ID.
  Unsubscribe removes the callback but does not delete an empty monitor. A
  reconnect serializes every retained monitor, including empty historical
  monitors, in `WindowMonitorEventListenerIds`; response-based sync trimming
  is commented out.
- `DeviceLocalRpc` then retains the supplied window/listener IDs. Each window
  event clones an O(window-count) `WindowIds` map before gob/RPC encoding, and
  `DeviceRemote` fans the event back out across the matching grids. Historical
  UI objects therefore become extension allocation/encoding work as well as
  app CPU and memory.

A disposable loopback probe modeled an unavailable/recreated extension with
12 retained connect controllers:

| state | opened controllers | location listeners | remote monitors | active window listeners | goroutines |
| --- | ---: | ---: | ---: | ---: | ---: |
| first connect | 12 | 12 | 12 | 12 | 21 |
| disconnect | 12 | 12 | 12 | 0 | 10 |
| reconnect 1 | 12 | 12 | 24 | 12 | 22 |
| reconnect 2 | 12 | 12 | 36 | 12 | — |
| reconnect 3 | 12 | 12 | 48 | 12 | — |
| direct `controller.Close` on all | 12 | 0 | 48 | 12 | 22 |

The exact UI-created controller count still needs a live app trace, but the
retention and per-restart multiplication are deterministic. They violate both
predictable memory and predictable CPU: work grows with historical view and
restart count rather than current visible state.

### 12.3 MainActor RPC and NetworkExtension preference fan-out

`VPNManager` is another high-confidence pause amplifier:

- Five SDK listeners (route-local, offline, connect, provide-paused, provide)
  independently enqueue `updateVpnService()` on the main queue. There is no
  pending-bit, desired-state equality check, or single-flight reconciler.
- Each update performs four synchronous `DeviceRemote` getter RPCs while on
  the MainActor; Debug performs two more. Each getter holds
  `DeviceRemote.stateLock` for the RPC reply, so a busy/restarting extension
  can turn these into blocked-main-thread pauses.
- A focused real `DeviceLocal` ↔ `DeviceRemote` loopback probe measured the
  Auto-control sequence:
  connect emitted `provide, connect`; disconnect emitted
  `provide, connect, provide`; reconnect emitted `provide, connect`.
  The synchronous actions took 3.81, 3.20, and 5.45 ms respectively. A batch
  of the four getters had median **365 µs**, p95 **608 µs**, maximum
  **2.89 ms**. Loopback is a lower bound; the important result is the 2/3/2
  refresh multiplication before UI/controller fan-out.
- Every resulting start path begins
  `NETunnelProviderManager.loadAllFromPreferences`, constructs configuration,
  then saves, reloads, and starts. Stop begins another load and save.
  `tunnelInstance` prevents an obsolete completion from continuing, but it
  cannot cancel preference operations that have already started. Apple
  documents that [load completions run on the caller's main
  thread](https://developer.apple.com/documentation/networkextension/netunnelprovidermanager/loadallfrompreferences%28completionhandler%3A%29)
  and that
  [`saveToPreferences`](https://developer.apple.com/documentation/networkextension/nevpnmanager/savetopreferences%28completionhandler%3A%29)
  writes the NetworkExtension preference store. Repeating those operations for
  transient intermediate states is unnecessary system/`nehelper` work.
- macOS adds a platform-only synchronous-RPC surface: `MenuBarExtra` computes
  its image from `getConnectEnabled` and `getProvideMode`, then evaluates the
  same values again for its status rows. A scene-body evaluation can therefore
  issue at least four serial RPCs. SwiftUI body computation must read cached
  observable state, not synchronously query the extension.

This path explains both forms of the report: high CPU when callbacks and
preference work fan out, and a pause when the MainActor is off-CPU waiting for
an RPC/lock/system service. Apple explicitly recommends separating busy CPU
from blocked-main-thread hangs with Time Profiler, Hangs, and System Trace;
Time Profiler alone cannot expose an IPC wait.

### 12.4 Bounded extension-side settings pause

The extension has a separate bounded, but credible, pause source:

- Its `setLocal` closure calls `setTunnelNetworkSettings` without a settings
  signature, pending flag, or in-flight serialization.
- A disconnect can reach `setLocal` from connect-location nil, providing with
  no location, and connected-window becoming false. Those signals can converge
  on two or three equivalent settings applications on the extension main
  queue. Reconnect applies settings again when the provider window becomes
  connected.
- Apple documents that
  [`setTunnelNetworkSettings`](https://developer.apple.com/documentation/networkextension/nepackettunnelprovider)
  configures the virtual interface's addresses, DNS, routing, proxies, and MTU.
  It should be treated as an expensive state transition and coalesced by
  semantic settings identity, not as a harmless notification handler.

This is not an unbounded spin and the implementation is shared with iOS. It is
therefore ranked below controller/monitor retention, but redundant applies can
explain a finite extension CPU spike or traffic/UI pause around a transition.

### 12.5 Current UI guards and adjacent bounded CPU

Several historical UI problems are already guarded:

- the disconnected `repeatForever` view is mounted only while disconnected;
- the connecting grid's 60 Hz timer is single-instance, stops after animation
  settles, and is invalidated on disappear/deinit;
- the animated ellipsis invalidates its timer on disappear;
- logical grid signatures suppress redundant `@Published` grid rebuilds;
- contract-row recomputation is throttled/coalesced (the earlier
  `PLAN2.md` analysis recorded an RPC + full-row re-render storm and UI hangs).

Remaining bounded work should still be measured:

- every grid notification creates an unstructured Swift `Task`; signature
  equality prevents the final publish but not task creation, SDK reads,
  sorting, or signature construction;
- the throughput controller polls two stats RPCs every second, and
  `ThroughputStore` remaps and republishes both complete arrays;
- three connect-drawer charts are eagerly mounted in a non-lazy stack. Each
  can drive a 20 Hz `TimelineView` for the full 60-second traffic window,
  including after disconnect, even when below the visible scroll region.
  Reconnect renews that window. This is intentionally bounded, but it gives
  macOS a visible post-disconnect CPU tail.

The macOS/iOS difference is consistent with, but does not prove, a residency
effect: the macOS menu and window process remain active, while iOS commonly
leaves the active scene. Apple says inactive scenes should pause work and
background scenes are not visible; chart/timer/RPC work should observe
[`scenePhase`](https://developer.apple.com/documentation/swiftui/scenephase)
plus actual macOS window/tab/scroll visibility. Because the side-effectful
controller initializer exists on both platforms, it must be corrected on both.

### 12.6 Recommended correction order (not yet applied)

1. **Make controller ownership singular.** Remove
   `openConnectViewController()` from both navigation view initializers. The
   app-level `ConnectViewModel` owns exactly one controller per device and all
   views consume it.
2. **Make close transitive and idempotent.** `ConnectViewController.Close`
   must detach and close its current grid. All Swift stores should close via
   the device/view-controller manager, or the manager must receive an
   idempotent close callback. `DeviceRemote.Close` should close its controller
   manager.
3. **Bound monitor state to active listeners.** Delete a remote monitor when
   its last callback is removed; never serialize empty monitors during sync;
   trim stale generation/window IDs after reconnect. Add a 100-cycle invariant
   test proving controller, listener, monitor, goroutine, and FD counts return
   to one-cycle baseline.
4. **Replace `VPNManager` fan-out with one reconciler.** Listener payloads
   update a cached desired-state snapshot and schedule at most one MainActor
   reconciliation. Allow one preference operation in flight, retain only the
   latest desired state, and skip save/start/stop when configuration and status
   already match. Do not issue synchronous getter RPCs from SwiftUI bodies.
5. **Serialize extension settings.** Compute a semantic network-settings
   signature, allow one `setTunnelNetworkSettings` in flight, coalesce equal
   requests, and apply at most one latest-different follow-up.
6. **Give UI refresh work an explicit budget.** Coalesce grid tasks, publish
   status/throughput only on semantic change, share one visible chart clock,
   and pause charts/timers when the main window/tab/card is not visible.

### 12.7 Required macOS release validation

Run a signed, logged-in app with the real packet extension for at least 20
interactive and 100 automated connect → disconnect → connect cycles. Record
both the app and extension:

- Instruments SwiftUI, Time Profiler, Hangs/Hitches, System Trace, Allocations,
  and Points of Interest. Apple's
  [SwiftUI Instruments workflow](https://developer.apple.com/videos/play/wwdc2025/306/)
  correlates update groups and long body updates with CPU samples; use System
  Trace when a hang has low CPU because that indicates blocking/IPC rather than
  computation.
- Add `OSSignposter` intervals for user action, VPN reconciliation,
  load/save/start/stop, RPC sync/getter batch, and every network-settings
  application. Apple provides
  [`OSSignposter`](https://developer.apple.com/documentation/os/ossignposter)
  specifically to align these intervals in Instruments.
- At every transition log app-side opened controller/listener/monitor counts,
  extension-side remote window IDs/settings applies, callback queue depth,
  goroutines, Go heap, physical footprint/RSS, and CPU at 0/10/60/120 seconds.
- Acceptance: no count grows with cycle number; at most one VPN reconciliation
  and one settings apply are in flight; steady app/extension CPU returns to
  baseline after the explicit 60-second chart window; no main-thread interval
  exceeds 100 ms; no monotonic post-settle footprint growth; and packet
  forwarding remains live through the final reconnect.

## 13. macOS connect → disconnect → connect correction (2026-07-24)

The deterministic defects in §12 are now corrected. The ownership and idle
notification changes are in the shared Go SDK, the app-side fixes apply to
both macOS and iOS, and the analogous Android VPN-service/controller paths
were corrected as well. The Apple XCFramework and Android AAR were rebuilt
from the corrected SDK source, so the application builds below contain the
runtime changes rather than only compiling against an older binary.

This does not replace the signed, logged-in, real-extension Instruments run in
§13.7. Local tests prove the count/state-machine invariants and all target
builds pass; only a real installed extension can measure the final app,
extension, `nehelper`, RSS/physical-footprint, and traffic-pause distributions.

### 13.1 Cycle-bounded controller and window-monitor ownership

Applied shared SDK changes:

- `ConnectViewController.Close` is idempotent and transitive. It cancels the
  controller, marks it closed, detaches the connect-location subscription,
  clears the current grid, and closes that grid outside the controller lock.
  A close while connected therefore removes the active window callback rather
  than leaving it alive until a later disconnect.
- Replacing or removing a grid closes the prior grid outside the controller
  lock. A canceled grid refuses a late window subscription, so close versus
  reconnect races cannot resurrect a monitor.
- `DeviceRemote.windowMonitor` no longer registers an empty monitor eagerly.
  Registration happens with the first listener; removing the final listener
  deletes the monitor from `windowMonitors`. Reconnect sync omits empty
  monitors, so historical window IDs are no longer serialized to the
  extension.
- `DeviceRemote.Close` and `DeviceLocal.Close` are idempotent and close their
  view-controller managers. Remote controllers close while the RPC service is
  still available, allowing listener-removal calls to reach the hosted device.
  Local controllers close before the device state lock, avoiding transitive
  close deadlocks.

Applied Apple ownership changes:

- Removed the side-effectful extra `openConnectViewController()` from both
  `MainNavigationSplitView` (macOS) and `MainTabView` (iOS). The app-level
  `ConnectViewModel` is now the single connect-controller owner per device.
- `ConnectViewModel`, both App Intents, and the throughput, block-actions,
  peers, contract-details, and post-quantum-identity stores close controllers
  through `SdkDeviceRemote.close(_:)`, which both closes the controller and
  removes the manager ownership entry.
- Grid callbacks now use one lock-protected dirty/scheduled drain on the main
  queue. A burst produces at most one outstanding main-queue drain plus one
  follow-up for events that arrived during the read, instead of one
  unstructured Swift task per event.
- Connection status publishes only on semantic change, avoiding duplicate
  review checks and SwiftUI invalidations.

Applied Android ownership changes:

- `MainApplication` closes its account controller through the owning device
  before `DeviceManager.clearDevice`.
- The dynamically re-bound peers and post-quantum-identity view models retain
  the exact device that opened their controller and close through that owner
  on device replacement and `onCleared`.
- Other device teardown is also protected by the new shared
  `DeviceLocal.Close` manager close, so an activity disappearing after the
  device has already changed cannot retain an old controller indefinitely.

Deterministic validation:

- A new 100-cycle test drives one remote controller through
  connect → disconnect repeatedly. Every connected state is exactly baseline
  +1 monitor/+1 active callback; every disconnect returns exactly to baseline.
  Closing while connected returns the location listener, monitor, and active
  callback counts to baseline. Manager close returns opened controllers to
  zero.
- A separate test opens 16 managed controllers and proves
  `DeviceRemote.Close` leaves zero manager entries, zero monitors, and zero
  active monitor callbacks.
- The lifecycle tests passed under the race detector at count 5; the combined
  lifecycle/throughput set passed under the race detector at count 3.

This closes the unbounded multiplier from §12.2: retained work is now a
function of current controllers/windows, not historical connect cycles.

### 13.2 Single-flight Apple VPN reconciliation

`VPNManager` now treats SDK notifications as state updates, not imperative
requests to rewrite NetworkExtension preferences:

- Initial state is read once. Route-local, connect, provide, and
  provide-paused listeners thereafter consume their callback payloads instead
  of issuing four synchronous getter RPCs per callback. Explicit user/App
  Intent updates perform one refresh so they do not depend on callback timing.
- Listener updates use a bounded **20 ms** debounce. Initial and explicit
  user-initiated updates bypass it. This covers callbacks that arrive a few
  milliseconds apart rather than assuming every member of a logical burst was
  already present in one main-queue turn.
- One reconciliation can be in flight. During it, only the latest desired
  `shouldRun` bit is retained; an echo of the active state cancels an
  intermediate opposite state. Waiters complete only after the latest state
  settles, and close cancels all pending work/waiters.
- NetworkExtension start/stop identity is only
  `provideEnabled || connectEnabled || !routeLocal`. A provide ↔ connect
  handoff while that expression remains true is a no-op: it does not reload,
  save, or restart the VPN profile.
- `providePaused` now changes only the idle/automatic-termination policy.
  Repeated identical idle-policy writes are suppressed. It cannot start a
  preference transaction.
- iOS background refresh scheduling occurs only for an actual reconciliation,
  not for every listener echo. Existing preference reset/profile fallback and
  tunnel-state timeout recovery remain intact. Delayed health-check recovery
  re-enters the same reconciler with its reset/profile index; it cannot start
  a parallel preference operation.
- Reconciler state is generation-canceled on close, so an old completion
  cannot mutate the replacement manager.

The macOS menu bar no longer performs SDK/RPC getters while SwiftUI evaluates
its body. Connect state comes from `ConnectViewModel.connectionStatus`, and
public-provide state comes from `DeviceManager`'s listener-backed
`currentProvideMode`. `DeviceManager` also assigns provide/pause listener
payloads directly instead of reading them back synchronously.

Consequences for the measured 2/3/2 callback sequence in §12.3:

- there is no 4–6-getter batch per callback;
- callbacks inside one transition collapse to the final desired state;
- there is at most one live load/save/start-or-stop operation;
- a transient Auto-control provide/connect handoff cannot stop a tunnel that
  is still required by the final state.

### 13.3 Serialized packet-extension settings

`PacketTunnelProvider` now owns a bounded settings-application state machine:

- A plan contains the concrete `NEPacketTunnelNetworkSettings` plus a semantic
  signature of the tunnel IPv4 address, ordered DNS server list, and MTU (the
  routes and DNS match policy are static).
- Exactly one `setTunnelNetworkSettings` call can be active. An equal request
  joins its completion list; different requests retain only the latest plan
  and run it once after the active call. If the latest state returns to the
  active plan, the obsolete pending plan is discarded.
- A successfully applied signature makes later equal requests immediate
  no-ops. DNS, local-mode, window-connected, and initial setup all use the
  same path.
- Session generations invalidate an active or pending apply during stop or
  replacement. Pending completions receive a bounded cancellation error, and
  a late NetworkExtension completion cannot update the new session.
- Packet reads and settings work are invalidated before replacing an existing
  device; the new session begins with fresh generations. Initial-settings
  failure stops both before closing the device.
- The window listener uses its supplied `SdkWindowStatus` payload instead of
  immediately locking the device to read the same value again.

The hard bound is one active plan plus one latest-different pending plan,
independent of callback count. Equivalent disconnect signals no longer create
overlapping virtual-interface reconfigurations.

### 13.4 Android service reconciliation without TUN churn

Android had the same burst shape and a more direct pause source:
`startVpnServiceWithForeground` unconditionally called `stopVpnService` before
every start attempt. It is now corrected as follows:

- State requests coalesce for **20 ms** on the main looper and carry a teardown
  generation. Logout/stop invalidates a queued request so it cannot resurrect
  the service after device teardown.
- Starting the same already-active or pending service mode is an immediate
  no-op. `MainService` already rebuilds its TUN descriptor for material
  window, offline, DNS, and per-app split changes, so a listener echo does not
  need a service restart.
- A live service is promoted to or demoted from foreground in place. The
  provide ↔ connect handoff therefore preserves the TUN descriptor even when
  notification/wake-lock policy changes. Only an incomplete start or a
  rejected in-place foreground transition falls back to replacement.
- Exceptions from the asynchronously posted Android service start now clear
  optimistic active/pending/foreground state. Previously the outer `try`
  could not catch an exception thrown later inside the posted runnable.
- `MainService.stop` reports destruction/internal tunnel failure back to the
  application, clearing the active service and foreground mode. A subsequent
  tunnel-state reconciliation can restart it instead of being suppressed by a
  stale `serviceActive = true`.
- Wake/Wi-Fi lock acquisition remains tied to active, unpaused providing; it
  is independent of the idempotent service start.

This is shared Android behavior, even though the historical accumulating CPU
report was macOS-only.

### 13.5 Bounded idle UI work

- The shared `ContractViewController` still samples cumulative counters at the
  configured interval and retains its bounded 60-second series, preserving
  chart correctness and immediate activity detection.
- It now notifies UI clients while traffic is active and for exactly five
  trailing zero samples, enough to settle the five-bucket rolling labels.
  Further timestamp-only idle samples do not trigger Swift array remaps,
  SwiftUI body invalidations, or Android Compose updates. New non-zero traffic
  resumes notification immediately.
- The existing chart clock continues scrolling recent traffic and pauses when
  the 60-second window drains. Combined with the notification bound, idle UI
  work no longer continues once per second forever after disconnect.

### 13.6 Build and test validation

Completed on the final source:

- `sdk`: an isolated repository-wide `go test ./...` passed on the final
  source, including the bounded-idle change; an earlier full pass took
  **386.329 s**.
- Focused lifecycle and idle-notification tests pass under `go test -race` at
  count 3; the lifecycle-only pair also passed at count 5.
- `make -C build build_apple` rebuilt the iOS device, iOS simulator, and
  macOS arm64/x86_64 XCFramework.
- macOS Debug `URnetwork` (including `URnetworkVPN`) builds successfully with
  signing disabled against the rebuilt framework.
- generic iOS Debug `URnetwork` (including `URnetworkVPN`) builds successfully
  with signing disabled against the rebuilt framework.
- `make -C build build_android` rebuilt the arm64, armv7, and x86_64 AAR,
  passed the mobile-export validation, and produced Full RELRO/NX/PIE stripped
  libraries.
- Android `:app:compilePlayDebugKotlin` builds successfully against the rebuilt
  AAR.
- All four changed worktrees pass `git diff --check`.

The Apple builds still report the repository's generated gomobile nullability
warnings and the existing Swift-6 actor warning in
`PostQuantumIdentityShareSheet`; neither is introduced by this lifecycle
change and neither fails the current Swift language mode.

### 13.7 Remaining release-device gate

The code invariants are closed locally, but acceptance still requires the
signed real-device loop from §12.7:

- run at least 20 interactive and 100 automated cycles on macOS, and a smaller
  regression set on iOS/Android;
- prove one app controller, active-only window IDs, one VPN reconciliation,
  and one extension settings apply at a time with signposts/counters;
- use Hangs/System Trace to distinguish main-thread IPC wait from CPU, and
  Time Profiler/SwiftUI/Allocations for app plus extension;
- measure packets/traffic gap as well as UI responsiveness, especially across
  Auto provide ↔ connect handoff and foreground-policy changes;
- require no cycle-correlated count/RSS/physical-footprint growth, no
  post-window idle UI callback stream, steady CPU returning to baseline, and
  no observable TUN pause on a handoff that keeps `shouldRun == true`.

## 14. Multi-client suspend/stall root-cause correction (2026-07-24)

The reported iOS background symptom has a real causal path, but it was not a
single timer-loop deadlock. UIKit normally gives an app only a short period to
finish work before suspending it
([Apple background execution](https://developer.apple.com/documentation/uikit/extending-your-app-s-background-execution-time),
[background sequence](https://developer.apple.com/documentation/uikit/about-the-background-execution-sequence)).
The packet tunnel is a Network Extension, not the containing UI process
([`NETunnelProvider`](https://developer.apple.com/documentation/networkextension/netunnelprovider)),
but the extension had observer and reverse-RPC paths leading toward that
process. A parked observer could therefore transitively own the multi-client's
only resize/evaluation goroutine. The same design flaw explains the analogous
server/proxy symptom without requiring an iOS suspension: any slow remote
observer or identity store could produce it.

The correction is shared by macOS, iOS, Android, and server users of the Go
multi-client. The proxy additionally implements the new cancellation-aware
identity-store capability, so its Redis request is actually terminated by the
maintenance deadline.

### 14.1 Callback classification and the root causes

The audit first separated callback boundaries by contract:

- Transfer `AckFunction` (send completion), `ReceiveFunction`, and
  `ForwardFunction` are **intentional synchronous backpressure**. A stalled
  callback must stall that transfer path, retain ordering and buffer lifetime,
  and resume only when the consumer returns. They were not made asynchronous,
  lossy, or coalescing. The contract is now documented beside the types and is
  pinned by a regression test.
- Window/provider monitors, contract status/stats, peer-identity notifications,
  and app/UI reverse state are observations. They must never own packet
  maintenance and can safely retain a bounded latest-state representation.
- TCP resets synthesized solely because a window client was removed are
  advisory teardown packets. They must not own peer replacement when a TUN
  consumer is unavailable.

The deterministic fragile points were:

1. `RemoteUserNatMultiClientMonitor` called every observer inline from the
   window resize/evaluation publisher. A callback that blocked stopped all
   subsequent add/remove/evaluate work. A callback panic escaped into the
   resize error handler and could cancel the multi-client.
2. `removeClient` invoked the downstream packet callback inline for every
   synthesized TCP reset. A blocked TUN/socket consumer therefore stopped
   replacement. It also allocated a reset for every associated TCP flow before
   a bounded consumer could discard anything, creating an O(flow-count)
   recovery/GC pause.
3. The only candidate enumerator called provider discovery, client auth, and
   setup synchronously. The production HTTP layer had its own timeout, but
   maintenance did not own an authoritative deadline, and custom generators
   could wait indefinitely.
4. `windowIdentityState` called the optional external
   `LoadWindowClientIdentities` while holding its identity mutex. The proxy
   adapter can perform a remote Redis load, so one slow load blocked both
   quality/speed enumerators before provider discovery and also blocked
   identity mutation.
5. Contract-status delivery used a bounded channel with a blocking producer.
   Once a suspended observer filled it, `HandleControlFrame` parked. Contract
   stats and peer-identity callbacks were also synchronous on stats,
   encryption, or cleanup paths.
6. Cleanup emitted observer-only close events before releasing the client,
   platform transport, generator identity, and subscriptions. A parked
   observer could therefore retain one complete resource set per churned peer.
7. After process suspension or system sleep, liveness timers could all wake
   late and interpret old blackhole/ping evidence as simultaneous peer
   failures, causing mass reset/reconnect churn precisely on resume.
8. The SDK reverse-notification path previously let a suspended app-side reader
   block the state publisher. HTTP responses also had to remain independent of
   a parked state notification.

### 14.2 Applied maintenance and memory bounds

Monitor delivery:

- Each monitor listener now owns one serialized worker with at most one
  callback in flight and one coalesced pending event.
- Pending provider diffs are last-value by client ID and capped at **64 unique
  IDs** by default. Crossing the cap replaces them with the monitor's current
  reset snapshot; terminal events delete from a pending reset.
- Listener panic is isolated to that delivery and does not terminate the
  worker or multi-client. Unsubscribe/close never waits for a parked callback.
- A merged quality+speed monitor rebuilds a reset from **all** underlying
  windows, so a reset from one window cannot erase live peers in another.

Client removal:

- Only removal-generated resets use a dedicated fixed worker/queue
  (`RemovalReceiveQueueSize = 256`). Normal ingress and all transfer callbacks
  retain their direct backpressure path.
- Enqueue is non-blocking. Overflow is counted and logged at sparse
  power-of-two thresholds.
- `removeClient` always clears every flow association, but computes reset work
  from the queue's current free capacity and constructs no more packets than
  can be retained. Recovery allocation is therefore O(queue capacity), not
  O(flow count).

Generator and persisted identity work:

- Optional `MultiClientGeneratorContext` and
  `MultiClientGeneratorWithDestinationContext` capabilities give discovery,
  auth, destination-aware auth, and client setup caller-owned contexts.
- Production defaults are **20 s** per discovery/auth call and **30 s** for
  client creation. Client setup receives a separate call context; a successful
  client's lifetime remains parented to the persistent window context, not to
  the expiring setup deadline.
- API auth and `find-providers2` now pass those contexts through the actual HTTP
  requests.
- Identity loading starts when the store is attached, off the formation
  critical path when it completes early. It is asynchronous, single-flight,
  and never holds the identity mutex across external I/O.
- Optional identity restoration has a **5 s** default sub-budget and cannot
  suppress a known/fresh provider: timeout or store failure abandons
  continuity and continues the same discovery attempt. A late legacy result
  is discarded.
- `MultiClientIdentityStoreContext` lets a remote adapter terminate the
  underlying call. The server proxy implements it by passing the maintenance
  context into `GetProxyWindowIdentities`/Redis.
- A legacy non-context-aware store remains compatible and can retain at most
  one isolated load worker per identity state if it never returns. Restored
  input is capped at **64 identities**, preventing an unbounded remote snapshot
  from becoming retained window state.
- Enumeration timeouts end one attempt, not the enumerator: it backs off and
  retries discovery/auth.

Observer/control work:

- Contract status uses one fixed ordered ring plus latest-state map keyed by
  `ContractKey`; producer dispatch cannot block and retained unique keys are
  capped by the configured sequence buffer.
- Contract stats use a fixed ordered ring keyed by contract ID/direction.
  Coalescing retains the latest absolute state/sequence and accumulates byte
  deltas.
- Peer identity notifications use one in-flight callback plus one pending
  change signal.
- Essential cleanup now cancels the client, unsubscribes control/identity
  listeners, closes the platform transport, and removes generator state before
  emitting observer-only contract-close/identity notifications.
- The SDK reverse state-notification loop retains one latest operation per
  method and a merged window event; a parked reverse call applies backpressure
  to that one delivery worker without blocking local state setters. Direct HTTP
  responses do not queue behind it.

Resume behavior:

- `SchedulerPauseTolerance = 2 s` distinguishes a materially late timer wake
  from an ordinary peer timeout.
- Initial and continuous ping waiters give the already-outstanding probe one
  fresh normal budget after such a wake.
- Blackhole evaluation suppresses stale evidence for
  `SchedulerPauseRecoveryTimeout = 5 s`, allowing network-change handling and
  fresh traffic evidence before removing peers.

### 14.3 Adjacent ACK-state correctness and pause reduction

`go vet` exposed that `sequenceAck` copied a generated protobuf `Tag` through
ACK-window values, maps, range variables, and snapshots. `protocol.Tag`
contains `protoimpl.MessageState`, which includes synchronization state and
must not be copied after use.

The ACK window now retains only the tag's scalar `send_time` plus its presence
bit:

- values and snapshots are safely copyable and smaller;
- the hand-written protocol-v2 ACK encoder consumes the scalar directly, so it
  does not allocate a temporary protobuf tag;
- RTT closing consumes the scalar directly;
- legacy encoding constructs a protobuf tag only at its wire boundary; and
- the tagged steady-state ACK update/reset test remains **zero allocations**.

This removes the static correctness warning without weakening the existing
zero-allocation ACK-window optimization.

### 14.4 Reusable stall tests

`ip_remote_multi_client_stall_test.go` provides a reusable `stallGate` plus
bounded-return and must-remain-blocked assertions. It deliberately parks a
consumer and tests the producer according to the boundary's contract.

The suite covers:

- blocked and panicking monitor observers;
- full merged-window reset correctness;
- end-to-end peer replacement while a merged observer is parked;
- non-blocking, bounded advisory reset enqueue;
- a real **4,096-flow** client removal that clears every association while
  retaining/constructing only the two-reset test capacity;
- bounded contract-status and contract-stats observers;
- coalesced peer-identity observation;
- stalled legacy identity load, cancellation of a context-aware load,
  warm-load start, optional-load fallback, and the 64-identity retention cap;
- generator discovery/auth/setup deadlines, retry after a timed-out attempt,
  and successful-client survival beyond its setup deadline;
- essential cleanup before a blocked stats observer;
- scheduler-pause classification; and
- explicit send-completion, receive, and forward callback backpressure,
  including proof that cancellation does not falsely complete a currently
  parked receive callback.

The existing SDK suite independently parks a real reverse RPC over `net.Pipe`
and verifies bounded/coalesced state production, intentional reverse-call
backpressure, transport-error teardown, merged window events, and HTTP response
independence. Together these tests provide a pattern for future packet-flow
audits: inject a parked callback, state whether it is data backpressure or
observation, assert progress/non-progress accordingly, then inspect retained
queue/map/goroutine bounds.

### 14.5 Validation

Completed on the final source:

- The exact multi-client stall set passed under `go test -race` at count 3
  (**6.331 s**). The merged reset case separately passed under race at count
  10.
- Setup-deadline, pause, and real removal tests passed under race at count 5.
- ACK codec/randomized/window/RTT and transfer-backpressure tests passed under
  race at count 3; the tagged zero-allocation ACK-window test passed at count
  10.
- The SDK reverse-RPC suspended-reader set passed under race at count 3
  (**9.315 s**).
- `go test ./proxy -run '^$'` passed in the server repository, exercising the
  cancellation-aware Redis adapter's compile-time interface.
- `go vet .` now passes for `connect`.
- A repository-wide `connect` run produced one failure hidden inside an
  exceptionally noisy legacy stress stream. An immediate JSON-filtered
  full-package rerun passed, and fresh `blocker`/`extender` runs passed; the
  failure did not reproduce. A final all-package compile plus the selected
  final regressions also passed.
- `git diff --check` passes in `connect`, `sdk`, and `server`.

### 14.6 Remaining release-device and production gates

The deterministic ownership and progress invariants are fixed locally. The
remaining work is empirical validation, not another inferred timeout:

- On a signed iOS build, background/foreground the containing app repeatedly
  while keeping traffic active. Record extension and app process signposts,
  goroutine count, queue/drop counters, RSS/physical footprint, peer add/remove
  cadence, and the traffic gap. Verify the extension continues adding peers
  while the app is suspended and does not mass-evict them on resume.
- Repeat sleep/wake and connect/disconnect/connect on macOS, and background
  service cycling on Android, because the shared fixes apply there even though
  their process lifecycle differs.
- In the proxy, fault-inject Redis latency beyond the identity sub-budget and
  a permanently blocked observer. Verify the Redis context is canceled, fresh
  discovery proceeds in the same attempt, live peer/resource counts remain
  bounded, and no listener/goroutine count grows with churn.
- A custom generator that implements only the legacy, non-context-aware
  interface can still block its own call. Production API generators implement
  the bounded interface; other long-I/O generators should do the same.
- Go cannot forcibly terminate arbitrary user callback code. A permanently
  blocked observer therefore retains its one worker until it returns, but it
  cannot retain an unbounded queue or stop maintenance. Listener lifecycle
  tests must keep the number of live workers bounded.
- Because Go's memory limit is soft and not an RSS/jetsam ceiling, final iOS
  acceptance still requires on-device physical-footprint/jetsam measurement.
