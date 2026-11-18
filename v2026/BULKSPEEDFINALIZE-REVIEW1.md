# BULKSPEEDFINALIZE: independent review 1

Snapshot: 2026-11-09 local. Independent of any other review; I did not open any
`*-REVIEW*.md` file other than writing this one.

## Scope, method and what I did not do

- Read `BULKSPEEDFINALIZE.md`, recent Connect history (`git log -20 --stat`,
  `git show d90ac7f4`), and the relevant parts of `MEMSTEADY.md`.
- Materialized the exact **reference** (`R/restore-300mbps-baseline-r1/tree` +
  `MEASURED-FIXTURE.overlay.json`) and the exact **aggregate** (the same tree +
  `R/immediate-hold-gap-fix-r1/PERFVAR.overlay.json`) Go sources and diffed them.
- Parsed the `[perfvar] synthetic-fast` JSON report of nine runs, the 96 RX32
  diagnostic samples, and the **final** results of the full Connect race run,
  which finished after the writeup's snapshot.
- **I ran no Go code**: no builds, tests or performance runs. Every number below
  comes from existing artifacts through read-only Python scripts (Appendix A).
  I did not read vault/config contents; one config diff was refused by a
  permission check (see section 3.2).

Notation used throughout:

- `R` = `/Volumes/data/urnetwork/temp/aggregate-flow-r15-r2`.
- `AGG file:line` = the materialized aggregate. Line numbers equal the donor
  files, e.g. `R/immediate-hold-gap-fix-r1/source/transfer.go`.
- `REF file:line` = `R/restore-300mbps-baseline-r1/tree/connect`.
- `SDK file:line` = `R/restore-300mbps-baseline-r1/tree/sdk`. The SDK is the same
  in both arms.

## 0. Bottom line

1. **The exact old binary replay is not a clean "the host got slower" control.**
   - **Contention.** Its GC stop-the-world pause averaged 7.05 ms (32 MiB) and
     3.18 ms (64 MiB) per cycle during the workload. The original run averaged
     0.071 and 0.085 ms: 40-100x more host contention.
   - **Different workload shape.** It routed all bulk streams of windows 2-5
     through a single provider (`provider_mask=1`); the original used 3.
   - **Harness differences.** Several remain unverified (section 3.2).
   - **What survives.** Under similar same-day conditions the aggregate shows a
     *different* failure signature: thousands of provider timeout resends,
     64-91% pure-ACK refusal, a saturated RX child. "New code cannot explain the
     entire gap" is true but not useful. Do not let it discount the code
     regression.
2. **The reference is canonical `84415854` byte for byte** (421/421 top-level
   production `.go` files).
   - Canonical main's production code differs from it **only** by `d90ac7f4`:
     zero-time shared-pump Pack/ACK admission.
   - The aggregate keeps that change, as do the other post-300 Mbps trees I
     checked (`R/tree`, `aggregate-core-reserve-immediate-r2`). The reference
     does not have it.
   - On a reliable H1 carrier the change converts reader backpressure into
     Transfer loss plus sender-timeout recovery. The restored SDK policy
     documents exactly that failure (`SDK mobile_memory_policy.go:213-218`).
   - This is the cheapest decisive A/B available, and MEMSTEADY records R14
     (this composition) as never performance-measured.
3. **Question 1, answered from data.** The binding limit is the **ordinary RX
   child** (2,181,038 B), not the parent.
   - It sits at ≥97% of its ceiling in 83 of 96 diagnostic samples.
   - Head recovery is non-zero in 1 of 96 samples.
   - The aggregate's own suite already holds **deterministic reproductions** of
     the stranding mode: ten `…UnderForeignRx` / fresh-head tests report
     `blocked=true drops=1`.
4. **Pure TCP ACKs are refused at the client bridge in every aggregate run**:
   64-91% at 32/64 MiB, versus 0.2-0.4% for the old code, historically and today.
   - Mechanism: the new immediate-delivery count gate is about 2-3 originals per
     H1 data lane at 32 MiB, held for a Transfer round trip.
   - Excluding ACKs from the byte budget freed memory but put an RTT-bound count
     throttle on the TCP ACK clock.
   - This plausibly caps the 64 MiB arm (59-101 Mbps with zero timeout resends).
5. **The full race run finished: 9,825 pass, 69 failing roots, 16 skips** (the
   snapshot showed 8 failures). Several are behavioral regressions in exactly the
   bulk-path mechanisms (section 3.6). Triage them before spending another
   five-window run.
6. **The hold-gap fix looks correct** as far as I could verify: disjoint spans, no
   mutation, pending and foreign generations excluded. It removed the 64 MiB
   timeout-resend collapse. It is necessary but not sufficient.

## 1. My understanding

The private aggregate rolls Connect back to the receive-memory foundation that
measured 292-406 Mbps, then re-adds:

- protocol-only budget exclusions (`NoBudget`),
- reliable unordered ("immediate") delivery for pure TCP ACKs,
- exact receipt/deduplication history,
- ACK/FIFO recovery changes,
- native batching and retained-SYN work,
- the new hold-gap correction.

Status: 64 MiB passes its gates at roughly 60-100 Mbps; 32 MiB collapses to
2-12 Mbps with thousands of provider timeout resends and loaded-request failures.
The developer asks where 32 MiB loses forward progress, whether the units are
consistent, how immediate history interacts with dense-ordinal logic, and how to
separate code from host effects.

I agree with the writeup's caution on N=1 runs, on not promoting baselines, and on
not exonerating decode/admission from a misleading counter name. My main
disagreements:

- Which control is trustworthy (section 3.2).
- That the most suspicious retained change, the zero-time pump, is not named at
  all (section 4.2).
- That the 64 MiB "success" is limited by an ACK throttle the writeup does not
  mention (section 4.3).

## 2. Claim-by-claim verification

| Writeup claim | Status | Evidence |
| --- | --- | --- |
| 48 product bindings; patch against the corrected reference | **Verified**, with a nit | 47 files differ. `transfer_prepared_memory.go` is bound but byte-identical to REF (a no-op binding). |
| Reference = Connect 00d4f62b + four measured corrections | **Verified, and stronger** | The four donor files equal canonical `84415854` and `d90ac7f4` byte for byte. All 421 top-level production files equal `84415854`. |
| Speed table values | **Verified** | All 4 x 10 window values and the resend lists match the reports (Appendix B). |
| "Exact old executable is slower now too, so new code cannot explain the whole gap" | **Literally true, misleading** | Contention, single-provider routing and harness differences (section 3.2). Under like conditions the aggregate's signature differs qualitatively. |
| Immediate policy selects pure TCP ACKs only; SYN/FIN/RST ordered | **Verified** | `AGG ip_immediate_ack.go:20-30`; `AGG ip_remote_multi_client_protocol_control.go:8-17`. |
| Mixed DATA Pack stays DATA | **Verified** | Send: `protocolOnlyFrames` over all frames. Receive: `AGG transfer_receive_memory_decode.go:64-69`, `AGG transfer_receive_protocol.go:143-145`. |
| Hold-gap fix subtracts only secured same-generation immediate positions; no mutation | **Verified** | `AGG transfer_immediate_hold.go:7-33`. Spans are disjoint because `reserve` refuses an already-covered ordinal and `complete` merges only adjacent secured spans (`AGG transfer_immediate_receive.go:151-232`). |
| `PackHandoffSaturationCount=0` does not exonerate admission | **Verified, and it exonerates even less** | Incremented only inside `tryDeepenH1PackQueue` (`AGG transfer.go:16776-16780`). Not for mutex-busy, owner-bind, quota, channel-full or decode refusals. |
| Decode refusal also records `recordReceivePackHandoffDrop` | **Verified** | `AGG transfer_receive_memory_decode.go:103-111`. The zero-time pump refusal records the same counter (`AGG transfer.go:5196-5202`). `ReceiveDecodePressureDropCount` is a third gate, carrier-bound prepayment only (`AGG transfer.go:4790-4793`). |
| Peak root 2,239,368 B; recovery 30,544 B; counters 12,980 / 116,845,208 / tentative 49 / committed 741 / shortfall 0 | **Verified** | RX32 samples (section 3.3). The reading "substantial parent capacity unused" is true but beside the point: the child is pinned. |
| Application TUN MTU clamps 1440 → 1280 | **Verified (fixture level)** | `server/connect/perfvar/workload_test.go:212`; its test asserts 1440 → `DefaultMtu`. Original and replay used the same access profile (`inner_mtu: 1440`), so MTU cannot explain the delta. |
| Race run "8 fail events" | **Out of date** | Final: 9,825 pass / 69 failing roots / 16 skip (section 3.6). |
| Retained ACK/FIFO changes include `d90ac7f4` | **Not stated; it is present** | `AGG transfer.go:5011-5015` (ACK, 0) and `:5196` (Pack, 0). REF uses `packHandoffTimeout`/`ackHandoffTimeout` (`REF transfer.go:4987, 5156, 15490-15516`). |

## 3. Evidence from existing artifacts

### 3.1 Per-window comparison (all from the run reports)

| Run | Whole-run Mbps, windows 1-5 | Providers used per window | Provider timeout resends per window | Pure-ACK refusal at bridge | GC pause per cycle during workload |
| --- | --- | --- | --- | --- | --- |
| Original 32 | 387.7 / 292.2 / 360.7 / 405.0 / 297.7 | 5,3,3,3,3 | 16,8,12,12,8 | 301/160,088 = **0.19%** | **0.071 ms** |
| Old binary today 32 | 45.6 / 13.7 / 15.0 / 17.8 / 19.1 | 2,**1,1,1,1** | 17,8,14,12,69 | 41/10,540 = 0.39% | **7.05 ms** |
| Pre-fix aggregate 32 | 29.6 / 2.8 / 3.0 / 3.5 / 2.7 | 4,3,2,3,2 | 1648,3359,2347,2139,1483 | 3,921/5,922 = **66%** | 0.17 ms |
| Fixed aggregate 32 | 12.2 / 2.5 / 5.4 / 3.4 / 1.9 | 2,3,2,5,2 | 2216,3142,2141,2039,2986 | 2,339/3,672 = **64%** | 0.54 ms |
| RX32 diagnostic (instrumented) | 10.9 / 6.0 / 6.6 / 4.0 / 3.2 | 5,2,4,3,3 | 2045,2463,2259,3233,2133 | 1,860/4,535 = 41% | 0.135 ms |
| Original 64 | 357.6 / 368.9 / 390.8 / 303.9 / 406.4 | 3,4,4,3,3 | 0,0,0,0,0 | 412/166,744 = **0.25%** | **0.085 ms** |
| Old binary today 64 | 178.4 / 162.2 / 180.9 / 116.6 / 89.0 | 1,3,4,2,2 | 4,8,4,4,4 | 140/62,757 = 0.22% | **3.18 ms** |
| Pre-fix aggregate 64 | 170.2 / 145.8 / 33.3 / 3.0 / 2.3 | 3,3,5,3,2 | 8,4,3541,3621,1391 | 42,342/47,775 = **89%** | 5.39 ms |
| Fixed aggregate 64 | 92.2 / 59.1 / 85.5 / 89.5 / 100.8 | 4,2,3,3,4 | 0,0,0,0,0 | 52,785/57,957 = **91%** | 6.13 ms |

How each column is derived:

- **Providers used** is the popcount of the per-run `provider_mask`: providers
  that served at least one bulk request, set at origin request routing.
- **Pure-ACK refusal** is `RefusedPureAckOnlyBatch / OfferedByType[PureAck]`
  from `bridge_admission`. Refusals inside mixed batches are excluded, so it is a
  lower bound.
- **GC pause per cycle** is `Δpause_total_ns / Δnum_gc` between `HarnessBefore`
  and `HarnessAfterJoin`. It is a contention *proxy* only, and noisy in slow runs
  that have just 6-7 collections.

Pattern: the old code never refuses ACKs and never produces resend storms, even
when starved. The aggregate refuses most ACKs in every run and storms at 32 MiB,
including runs whose GC-pause proxy is near normal (0.13-0.17 ms).

### 3.2 Why the old-binary replay is a weak control

1. **Host contention.**
   - GC STW pauses were 40-100x the original's (table above).
   - MEMSTEADY itself records a Java build at ~689% CPU near that replay
     (`MEMSTEADY.md:6860`).
   - The full Connect race run ran 00:55-01:23 UTC and overlapped the RX32
     diagnostic, which started at 01:05:37 UTC.
2. **A different workload shape.** With `provider_mask=1` in four of five
   windows, all eight bulk streams shared one provider, i.e. one window client's
   sequences and budgets. The original spread them over three providers. Per
   provider the replay is still about 6x slower, so contention is real, but this
   is not "same workload, slower host".
3. **Harness deltas.** Most are probably small; all are unverified.
   - The original ran under `go tool test2json` from `/Users/builder/urnetwork/server`,
     with ambient vault/config and `WARP_TEST_ENV_FAIL_FAST=1`
     (`…/foreign-sequence-performance-r1/runtime-r1/RUN.py:58`).
   - The replay runs the binary directly from `R/tree/server`. It uses TMPDIR and
     GOTMPDIR inside an owned APFS image, and a copied private fixture vault,
     config and site (`R/r10-runtime-fixture-r2/suite`); glog shows `settings.yml`
     loaded from there.
   - `test-env.sh` is byte-identical. Contract/target parameters and the access
     profile in both reports are identical.
   - I could not diff the fixture `config/settings.yml` against the ambient
     config. **Please diff it yourself.**
4. **Log signatures.** These are otherwise similar between original and replay
   (contract waits, sequence exits, mux backpressure). The aggregate stands out
   with **219** `TLS handshake error … EOF` lines versus 50 (original) and 9
   (replay).

Conclusion: until an A/A of the old binary on a quiet host reproduces at least
~250 Mbps, treat all "today" absolute numbers as host-confounded. Compare
candidates **only within interleaved pairs** (section 5, E0).

### 3.3 The 32 MiB RX child is continuously saturated (question 1)

Facts from the 96 RX32 samples taken every 500 ms (`RX32-REPORT.json`):

- `client_transfer_used_bytes` equals root use and is ≥2.0 MB in 87/96 samples
  and ≥97% of the 2,181,038-B ordinary RX ceiling in 83/96. 23 samples are
  *above* it (loans/recovery).
- `pack_queue_used_bytes` is 0 in every sample. The H1 paid owner bypasses the
  Pack byte budget (`AGG transfer.go:16916-16919`).
- Head-recovery use is non-zero in **1/96** samples (30,544 B), although
  `ClientReceiveHeadRecoveryReservedByteCount` totals about 18 MB. Recovery works
  only transiently.
- `PackHandoffWaitCount` is **0 in every sample**: the pump never waits.
  `PackHandoffDropCount` grows about linearly at ~260/s, reaching 12,980.
- **244 drops had already occurred at t=0.5 s, while RX was at only 1.06 MB.**
  The first holes therefore predate budget saturation (between-sample spikes
  cannot be excluded). That points to the zero-time handoff (queue/mutex) or
  per-sequence limits as the trigger, and RX saturation as what sustains it.

Managed-memory peaks show the old code's shape was different:

- Original 32: `ClientReceiveByteCount` 1,225,184 (RX not saturated);
  `ClientSendByteCount` **8,859,500**.
- Aggregate 32: receive 2.17-2.30 MB (saturated); send 0-87 KB.

So the historical run spent ~40% of the client share on retained, budgeted pure
ACKs and never filled RX. The aggregate frees that send memory but never lets
ACKs through (section 4.3), and its RX is pinned.

### 3.4 Pure-ACK refusals happen at the sender, not the provider

Provider-side receive counters (`provider_receive`, cumulative at the end):

- **Old code:** `PackHandoffWaitCount = WaitSuccess` (25/25, 7/7, 4/4, 16/16),
  zero drops. Backpressure was used and it was lossless.
- **Aggregate:** zero waits, zero drops, and **zero ACK-handoff refusals** in
  every run.

So the refusals happen at client admission, and the `d90ac7f4` ACK `TryLock`
drop path is not active in these runs; I rank it low. Provider
`ReceiveQueueCommitCount` is 0 in old code and 71-240 in the aggregate: DATA
after immediate ordinals now goes through hold/commit, as designed.

### 3.5 Connection setup stalls are after the handshake

`[firstload]` lines (SDK first-load TCP timings):

- Original: SYN-ACK 42-435 ms, first byte 83-824 ms.
- Old binary today: SYN-ACK up to 1.67 s, but SYN-ACK→first byte stays about
  50-250 ms.
- Fixed aggregate 32: SYN-ACK 43 ms → first byte 713 ms; 174 ms → **4.09 s**;
  50 ms → **4.94 s**; 2.6 s → **16.5 s**; plus SYN-ACKs of 3.1 and 6.0 s.

Early DATA (TLS records, requests, first response bytes) is being lost or held
and recovered on timeout. That fits the loaded-GET failures and is relevant to
question 4. Loaded probes record only total and first-payload latency; add
dial/TLS/first-byte phases as bulk flows already have.

### 3.6 Final race run (`R/restored-hold-fix-connect-all-go-r2/run`)

Exit 1; 9,825 pass / 69 failing roots / 16 skip. Clusters by first assertion
line:

- **A. Head stranding under foreign RX (behavioral; the 32 MiB mechanism).**
  These tests come from `R/tree/connect`, i.e. regression tests for fixes the
  rollback removed:
  - `TestReceiveConsumerEstablished{Quiet,Promoted}{Compact,FullProof}HeadUnderForeignRx`
    and `…CompactUnknownIdUsesOrdinaryWorker`: `blocked=true … drops=1`,
    `FOREIGN_RX_STRANDED_ESTABLISHED_QUIET_COMPACT_HEAD`.
  - `TestReceiveConsumerFreshFullProofDataHeadWithoutOwnTailProgresses`:
    `FOREIGN_RX_STRANDED_NEW_FULL_PROOF_DATA_HEAD … progressed after foreign
    predecessor drained`.
  - `…EstablishedCurrentHead{PaysBothWireForms,KeepsRequiredContext}`:
    "current Head lost independent physical admission".
  - `…PublicNonHeadContractSwitchUnderForeignRxPressure`.
  - `…FreshOpeningDuplicateKeepsOnePaidOwner`: `initial_blocked=true`.

  The assertions are behavioral. A foreign sequence occupies RX, and the
  target's current head must reach delivery/ACK independently
  (`R/tree/connect/transfer_receive_consumer_established_wire_head_test.go:383-446`).
  The authority-guard tests (bad MAC, expiry, wrong source) fail at the
  `child_funded=false` precondition. That is not a security regression, but it
  means the guards are untested in this tree.
- **B. ACK custody premise conflicts.** Provider encoded/encrypted/mixed/UDP-head
  tests ("later pure ACK was not a real committed fully prepaid owner");
  `TestProvider{Replacement,Cancellation}RetiresBlockedPureAck`; five
  `TestMultiClientExpand…` waiter deadlines. These expect pure ACKs to be
  budgeted DATA owners. Rewrite them around a real blocked DATA boundary, as the
  writeup proposes.
- **C. SYN under a full data budget (policy change).**
  `TestPrivateTcpProfilePressure{32,64}{Steady,GenerationOverlap}` fail with
  "full data budget admitted another SYN". `NoBudget` now admits new connections
  when DATA is full: helpful for browsing responsiveness, harmful when the new
  flow's DATA cannot get RX. Decide this explicitly.
- **D. SYN-retry/native gate premises.** `TestNativeSynRetry*` ("native offer
  bypassed the actual closed resend-capacity gate"), `TestNativeStaleBoundGate…`,
  `TestTcpPersistNativeRefusedReopen*`.
- **E. Needs real triage (possible correctness).**
  - `TestCanonicalH1PlatformClientKeyBeforeCloseUsesActualAck`: "key delivery
    without its platform ACK lost reliable ownership".
  - `TestCanonicalH1CloseLifecycleKeepsOriginalAndControlCustody`.
  - `TestSendBufferCallerCancelWhileWaitingForCreationLock`: "canceled caller
    acquired sequence ownership".
  - `TestMultiClientH1ResidenceHealthAndSamePeerReplacement`: `survived=false`.
  - `TestCanonicalDecodedMetadataFullBudget{Delivery,Cancel}`.
- **F. Size/memory witnesses.** `sendItem` 600 B. `payload_owner_ledger`
  "send=600". `TestTransferProductionAckOwnerDoesNotReserveCompatibilityQueue`:
  `data_channel_bytes=2097152`, plausibly because the aggregate doubled the send
  `packs` channel to `2*sequenceBufferSize` (`AGG transfer.go:7564-7570`). That
  is a real slot-memory increase worth confirming against the 50 MiB iOS budget.

## 4. Code findings

### 4.1 Source identity gives you a free single-variable A/B

- `REF` production == canonical `84415854` (421 top-level `.go` files compared;
  0 differ).
- `git diff 84415854 d90ac7f4 -- '*.go' ':!*_test.go'` touches **only**
  `transfer.go` (+52/-34): the zero-time pump.
- Therefore **canonical HEAD's production code = REF + zero-time pump**.
- The post-300 Mbps trees I checked all contain it (`R/tree`,
  `aggregate-core-reserve-immediate-r2`, the current aggregate); REF does not. I
  could not check the R10 source, whose `source/connect/transfer.go` is absent.
- MEMSTEADY lists the R14 composition as "source-reviewed but **unexecuted**"
  (`MEMSTEADY.md:6829`).

### 4.2 The zero-time shared pump (`d90ac7f4`) is the prime suspect

- DATA: `self.receiveBuffer.Pack(receivePack, 0)` (`AGG transfer.go:5192-5207`).
  It `TryLock`s `packMutex` (`AGG transfer.go:16867-16877`) and returns `false`
  on any failed reservation or full channel (`AGG transfer.go:17047-17060`). The
  pump records it as a handoff drop with no ACK.
- The code's own comment says the H1 wait exists to turn "a short scheduler
  mismatch into transport backpressure rather than a synthetic Transfer
  loss/retransmission cycle" (`AGG transfer.go:17073-17078`). With timeout 0 that
  branch is unreachable from the shared pump.
- The restored SDK policy sets `ReliablePackHandoffTimeout = H1PackHandoffTimeout
  = -1` because "a controlled H1 provider proved that a finite handoff timeout
  merely moves synthetic loss downstream" (`SDK mobile_memory_policy.go:213-218,
  426-428`). The pump now ignores that policy for every carrier.
- REF honored it: `packHandoffTimeout(transportType, carrierReliability)` and
  `ackHandoffTimeout(transportType)` (`REF transfer.go:4987, 5156, 15490-15516`).
- `d90ac7f4`'s goal, that a stuck callback must not stop ACKs or DATA for other
  sequences, is legitimate. Its fix trades head-of-line blocking for loss, and on
  a 2 MiB RX child each loss can cascade (section 5, H1/H3).

### 4.3 The immediate ACK path throttles the TCP ACK clock (question 2 and the 64 MiB ceiling)

- Pure TCP ACKs get `NoBudget` + `DeliverImmediately`
  (`AGG ip_remote_multi_client_protocol_control.go:8-17`).
- An immediate original is schedulable only while `immediateSendCount <
  max(1, immediateSendLimit)` (`AGG transfer_immediate_option.go:26-28`, used at
  `AGG transfer.go:10469, 10487`).
- `immediateSendLimit = max(1, sequenceBufferSize)` (`AGG transfer.go:7638`).
- The count increments on send (`AGG transfer.go:12731-12734`) and decrements
  only in `returnSendItem`, i.e. after the peer's Transfer ACK
  (`AGG payload_owner_ledger.go:157-164`).
- Per-lane `sequenceBufferSize = ceil(size/8)` for data lanes
  (`AGG transfer_logical_lane.go:42-47`, applied at `AGG transfer.go:7560-7563`).
- The mobile cap is 16, scaled above a 24 MiB target
  (`SDK mobile_memory_policy.go:14, 199, 292-299`), with 8 H1 lanes
  (`:209, 482-485`).
- So each data lane allows about **2-3 outstanding immediate originals at
  32 MiB** and roughly 4-6 at 64 MiB, depending on which target is passed.
  Confirm with the existing live sample fields `ImmediateOriginalCount/Limit`
  (`AGG transfer_send_live.go:173-174`).
- While blocked, these Packs also hold `protocolAdmission` slots of the same size
  (`AGG transfer.go:7956-7998`). Native ingress is ready-only, so the next
  offer is refused (`AGG ip_native_ingress.go:105-208`).
- At a 40-50 ms Transfer RTT that is about 40-75 immediate Packs/s per lane,
  against thousands of ACK packets/s at 100+ Mbps. Hence 64-91% refusals.
- REF had no such gate. ACK Packs were byte-budgeted and released from admission
  when written, so up to 8.9 MB were in flight (section 3.3).
- A receiver can also consume an immediate original **without any receipt**:
  history saturated (32 ranges), immediate queue full, or an unknown contract
  (`AGG transfer_immediate_receive.go:15, 354-361, 373-375, 386-389`). That pins
  one of the 2-3 sender slots until a timeout resend.

### 4.4 Advertisement units (question 2)

- `receiveWindowAdvertisement` advertises `min(ReceiveQueueMaxByteCount, budget
  TOTAL, cap)` per sequence. It is explicitly "independent of free space" and an
  encoded-byte ceiling (`AGG transfer.go:13381-13398`).
- With six window clients times up to nine lanes, the sum of grants is many times
  the shared 2.18 MB child.
- The retained charge per Pack exceeds encoded bytes. MEMSTEADY's 8-frame
  witness is 30,544 B at decode against 12,288 B encoded.
- So the units are **not** consistent, and the oversubscription is structural.
  REF had the same advertisement, but a full RX made the reader wait rather than
  drop.

### 4.5 Counter attribution

One counter (`recordReceivePackHandoffDrop`, `AGG transfer.go:4440-4450`) is fed
by:

1. decode-time owner refusal (`AGG transfer_receive_memory_decode.go:103-111`),
2. zero-time handoff refusal (`AGG transfer.go:5198-5202`),
3. fresh-opening key mismatch (`AGG transfer.go:5136-5144`),
4. protocol-path refusal via `packProtocol` (`AGG transfer_receive_protocol.go:48-76`).

Existing data cannot split them. A reason-coded counter array is a ~20-line
change for a diagnostic build.

### 4.6 Hold-gap fix review

- Correct as specified. The only remaining dense-ordinal count is
  `heldMissingBelow` (`AGG transfer_receive_hold_promise.go:182-187`), and both
  callers now use `heldDataMissingBelow` (`AGG transfer.go:18644, 18682`).
- Residual risks:
  - the recovery-prefix proof's **64-wire-position bound**, if it counts immediate
    positions (your own concern, untested);
  - lock order between `history.stateLock` and the callers' locks (document it);
  - history saturation at 32 interleaved ranges behind a long DATA hole (see 4.3).
- Design cost the fix does not remove: uncommitted ("tentative") held items get
  **no** SACK, and "a gap just below the boundary recovers on the paced resend"
  (`AGG transfer.go:18598-18619`). Commit requires reserving `missing ×
  maxHeldQueueByteCount`. With about 30 KB per Pack and a 2.18 MB child, roughly
  70 missing ordinals already make commitment impossible, and the sender then
  resends data the receiver holds.

## 5. Root-cause hypotheses, ranked, each with a cheap discriminating experiment

| # | Hypothesis | Likelihood | Key evidence | Cheapest experiment and prediction |
| --- | --- | --- | --- | --- |
| H1 | Zero-time pump turns transient queue/mutex fullness into holes. Holes fill the 2.18 MB RX with held data; decode refusals then sustain a timeout-resend storm. | **High** | Zero waits; ~260 drops/s; drops before RX saturation; SDK policy comment; the slow trees I checked have it, the fast one does not. | **E1:** in the aggregate, restore REF's two pump timeouts (`AGG transfer.go:5011-5015, 5196`). Run fast32 interleaved ABAB against the unreverted aggregate (2 pairs, 2 windows each). Predict resends fall more than 10x and RX drops below its ceiling. **E1′:** canonical HEAD vs REF binary, ABAB, which isolates `d90ac7f4` alone. **D1** (deterministic): real Client pump, H1-reliable carrier, two sequences, one consumer pausing 5 ms every N Packs. Assert zero drops and zero sender timeout resends with the old policy and drops with zero-time. Keep `d90ac7f4`'s HOL tests as the second invariant. |
| H2 | Rollback removed head-admission fixes; once RX is full, current heads strand until foreign data drains. | **High** (once RX saturates) | Ten deterministic `blocked=true` failures (section 3.6A); recovery use 1/96 samples. | **E2:** run the cluster-A tests on REF. If they fail there too, the historical run only avoided the condition. Then port the minimal "exact current head may use head-recovery permission" predicate from `R/tree` and re-run cluster A plus fast32. |
| H3 | Tentative held items never SACK when the hold promise cannot be reserved in a small child, so senders resend data the receiver holds. | Medium-high at 32 MiB | Comment at `AGG transfer.go:18616-18619`; 49 tentative evictions and 741 commits against thousands of resends. | **E3:** counters for commit attempts failing `holdPromiseFits` vs `reserveHoldPromise`, plus max `missing` and `maxHeldQueueByteCount`. Split provider resends by item class (DATA / immediate / ordered protocol) at the sender. |
| H4 | Immediate count gate (~2-3 per lane, held per RTT) throttles the TCP ACK clock and caps 64 MiB. | **High** for 64 MiB; secondary at 32 | 64-91% refusal against 0.2-0.4%; zero timeout resends at 64 yet 59-101 Mbps. | **E4:** one window with live diagnostics; read `ImmediateOriginalLimit` and `immediateWaitCount/Nanos`. Then fast64 ABAB with the limit decoupled (e.g. 64 per lane) or `DeliverImmediately` off for pure ACKs (keep `NoBudget`). **D4:** one lane, `SequenceBufferSize=2`, 40 ms fake RTT, 1,000 ACK offers/s; expect ≈95% refusal. |
| H5 | Host contention plus MultiClient window shrink explain the old-binary slowdown and amplify H1-H4 (timers fire under CPU starvation). | High for the control; medium as amplifier | GC pause 40-100x; one provider; 689% CPU Java build; race-run overlap. | **E0:** quiet-host gate (load average, no `go test -race`/java/xcodebuild in `ps`, a 1 ms ticker overshoot probe). Old-binary A/A twice. Record window membership per window. |
| H6 | Immediate receipts silently absent (history/queue/contract) pin sender slots. | Low-medium | Code paths in section 4.3. | Count the three `return true, false` exits. |
| H7 | ACK `TryLock` drops. | **Low** (refuted for these runs) | Provider and client `AckHandoff*` are all 0. | None needed; keep the counters in the report. |
| H8 | Advertisement oversubscription and unit mismatch. | Medium as amplifier only | Section 4.4; present in REF too. | After E1: A/B a cap of `child/active sequences`. |

## 6. New ideas and simpler alternatives

1. **Lossless, non-HOL pump.** Keep reliable-carrier backpressure, but never
   block behind a *stuck* consumer:
   - (a) a per-sequence one-slot "parked" decoded Pack (already paid; no new
     memory), refusing only if the slot is occupied; or
   - (b) a bounded wait that continues only while the target consumer has
     dequeued within the last few ms.

   Either satisfies both D1 and `d90ac7f4`'s tests. Prefer (b) for simplicity.
2. **Guarantee head progress cheaply.** Let every live sequence's exact
   `nextSequenceNumber` original borrow from the head-recovery child, which is
   2.18 MB and nearly idle. That is the property cluster A asserts; do not rebuild
   first-DATA reservations wholesale.
3. **Replace the ACK count gate with ACK compaction.** Keep one pending pure ACK
   per 5-tuple, the latest cumulative, and replace it until written (RFC 3449-style
   ACK filtering). Memory is bounded by flow count, with no RTT coupling.
   Alternatives: a per-client byte-bounded protocol pool decoupled from
   `SequenceBufferSize`/lanes, or NoAck for pure ACKs on reliable carriers (TCP
   tolerates ACK loss; keep window-opening ACKs reliable).
4. **Capacity without a bigger ceiling.** Early carrier-buffer retirement
   (30,544 → 18,256 B for the witness Pack) gives about 40% more effective RX at
   the same ceiling. It was rolled back for scope, not because it failed.
5. **Bisection instead of a monolith.** REF → +pump (= canonical HEAD) →
   +classification/`NoBudget` only → +immediate ACK → +FIFO/history →
   +native/SYN. Use one fast32 window per step with the existing fixed-provider
   treatment to remove MultiClient routing noise, in ABAB pairs.

## 7. Direct answers to the writeup's questions

1. **Where is forward progress lost?** At the ordinary RX child (2,181,038 B),
   pinned in 83/96 samples. The failing predicate is
   `takeReceiveMemoryOwner` → `tryReserveWithLock(remainder)`
   (`AGG transfer_receive_memory.go:107`). Then held-repair and session
   head-recovery decline (`AGG transfer_receive_memory_decode.go:97-111`). Also
   the zero-time handoff (`AGG transfer.go:17058-17060`). **Can the next head use
   recovery permission?** Not for established quiet/promoted compact/full-proof
   heads or fresh full-proof data heads: your own suite shows `blocked=true`
   (section 3.6A). The owner occupying RX is not attributable with current
   counters; add per-sequence RX attribution (queued vs held vs committed vs
   pending ingress) to the existing bounded sample.
2. **Units?** Inconsistent. Encoded-byte, per-sequence, total-capacity grants
   against a shared, retained-cost child (section 4.4). **Do fast ACKs over-drive
   the flight?** The data says the opposite for TCP ACKs: 64-91% are refused at
   the client (section 4.3). Over-driving comes from oversubscribed grants plus
   lossy admission, not ACK speed.
3. **Immediate history elsewhere.** The fix covers both commit paths and is
   correct. Remaining risk points:
   - the recovery-prefix proof's 64-wire-position/256-scan bounds,
   - history saturation at 32 ranges,
   - immediate receipts dropped for an unknown contract,
   - gap expiry/tentative eviction around immediate positions.

   Suggested controls:
   - DATA hole + 70 immediate + head under foreign RX (proof bound);
   - 33 non-adjacent immediate ordinals behind one DATA hole (saturation must
     not lose receipts or progress);
   - worker replacement after the history is secured but before the hole fills;
   - a contract rollover with an immediate Pack arriving before its ordered
     contract frame;
   - the same with a foreign generation and with a pending callback (you already
     have the last two).
4. **ACK-only early delivery sufficient?** For bulk, yes; ACKs are the clock. It
   is not sufficient for responsiveness. Section 3.5 shows multi-second SYN-ACK
   and post-handshake first-byte delays, consistent with ordered control/DATA
   sitting behind holes on hashed shared lanes (nine flows over eight lanes
   guarantees sharing). Before broadening: instrument loaded-probe phases, and
   log lane plus hole-below-at-arrival for SYN/SYN-ACK. A safer broadening is
   "immediate SYN only for a 5-tuple with no live state on that lane", with
   same-tuple reuse, FIN/RST-overtake and NAT adoption tests.
5. **Which retained changes explain thousands of provider retries?** In order:
   - the `d90ac7f4` zero-time pump;
   - the removed head-admission fixes;
   - tentative-without-SACK under a small child;
   - immediate receipts absent.

   To separate real loss from artifacts, split sender timeout resends by item
   class. At the receiver, classify each duplicate arrival as below-delivery-point
   (ACK lost/late), held-tentative, held-committed, or unknown (refused or lost),
   plus reason-coded refusals (section 4.5).
6. **Smallest discriminating case.**
   - First: the existing cluster-A tests, run on both REF and the aggregate.
   - Then D1 and D4 (deterministic, seconds).
   - Then one ABAB pair of single-window fast32 with the fixed-provider treatment
     for E1, before any five-window or 100-page run.
7. **Budget balancing.** After E1, measure before rebalancing. With `NoBudget`
   ACKs the send child is nearly idle (0-87 KB, where REF used 8.9 MB), so moving
   1-2 MiB from send to the RX child keeps the root unchanged. Prove the physical
   side with:
   - `HeapInuse` peak and MessagePool outstanding bytes by size class,
   - protocol/immediate Pack counts times pool class,
   - the doubled `packs` channel slots (section 3.6F),

   across RX = 2/3/4 MiB on fast32, then device RSS. Spare managed credit is
   indeed not spare physical memory: `NoBudget` Packs still hold pooled buffers
   outside the budget.
8. **Host/harness effects.** Section 3.2: contention (GC proxy, Java build,
   race-run overlap), single-provider routing, `test2json` vs direct, cwd,
   APFS-image TMPDIR, copied fixture config/site (unverified),
   `WARP_TEST_ENV_FAIL_FAST`. Use a quiet gate plus interleaved pairs, and report
   ratios to the in-session old binary.
9. **Old providers.** `DeliverImmediately` is emitted without any capability gate
   (`AGG transfer.go:12614, 12657`). An old receiver ignores field 14 and delivers
   in order. The client's immediate slot then frees only after ordered ACKs, so
   **ACK starvation behind upstream DATA holes is worse against old providers than
   in REF.** Qualify with a provider test knob that ignores fields 13/14 and never
   emits immediate receipts (`old_provider_fallback=true` arm), plus wire controls.
   Better still, gate immediate on an advertised capability as `contractAhead`
   already does.

## 8. Prioritized next steps

1. **P0** Update the writeup with the final race results. Mark cluster A as a
   deterministic reproduction of the 32 MiB mode and cluster E as possible
   correctness issues.
2. **P0** E1 (pump revert, ABAB) and E1′ (canonical HEAD vs REF). These are the
   highest information-per-hour runs available.
3. **P0** Reason-coded refusal counters and per-class resend counters in the
   diagnostic build. Read `ImmediateOriginalLimit`/wait stats in one window.
4. **P1** Choose the pump design (section 6.1) with D1 plus `d90ac7f4`'s HOL tests
   as the two invariants.
5. **P1** Port the minimal current-head admission predicate. Gate on cluster A,
   then fast32.
6. **P1** Fix the ACK throttle (compaction or decoupled limit) and A/B at 64 MiB.
   Add the capability gate before any public-provider exposure.
7. **P2** E0 quiet-host A/A of the old binary. Establish a noise band before
   comparing to 300 Mbps.
8. **P2** Decide the SYN-under-full-budget policy (cluster C) explicitly.
9. **P2** Budget rebalance with physical-peak proof. MTU normalization **after**
   the regression closes, so profile changes do not confound the A/Bs.
10. **P3** Old-provider arm, full100 at 32/64, then physical devices.

## Appendix A: scripts (read-only analysis; no product files touched)

All scripts are under
`/private/tmp/claude-501/-Users-builder-urnetwork-connect/5b991e4a-8699-464e-a6d4-9e0a0403d954/scratchpad/scripts/`:

- `extract_report.py`: extracts the report JSON from plain or `test2json` stdout.
- `tabulate_runs.py`: per-window table (section 3.1).
- `rx32_samples.py`: parses the 96 RX32 samples (section 3.3).
- `handoff_compare.py`: provider-side handoff counters (section 3.4).
- `race_fail_output.py`: first assertion lines of race-run failures (section 3.6).

The materialized trees are under `…/scratchpad/ref` and `…/scratchpad/agg`, and
the aggregate-vs-reference diff is at `…/scratchpad/transfer.diff`.

## Appendix B: report files used

- Original: `/Volumes/data/urnetwork/synthetic-smoke-r1/foreign-sequence-performance-r1/runtime-r1/candidate-fast{32,64}/stdout`.
- Old binary today: `R/restore-300mbps-replay-r1/run/fast{32,64}.stdout`.
- Pre-fix aggregate: `R/stable-memory-protocol-performance-r1/run/fast{32,64}.stdout`.
- Fixed aggregate: `R/immediate-hold-gap-performance-r1/run/fast{32,64}.stdout`.
- RX32 diagnostic: `R/restored-hold-rx32-diagnostic-runtime-r1/run/{rx32.stdout,RX32-REPORT.json}`.
- Race run: `R/restored-hold-fix-connect-all-go-r2/run/{connect-all-go-GO-RESULTS.json,connect-all-go.stdout,FINAL.json}`.
