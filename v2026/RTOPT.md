# Retransmission optimization: TUN TCP and client Transfer

Status: research in progress, 2026-09-25. No production TCP RTO, Transfer resend, ACK policy, or reconnect default has been changed by this study. The client-TCP-NoAck direction was explicitly superseded after a partial diagnostic screen; ACK-required 3s-TUN/3s-Transfer is the active candidate. Do not promote pilots or screens as baselines.

## Scope and ownership

`TunSettings.TcpMaxRto` is already a stack setting, defaulting to 8 seconds; `TcpMinRto` is optional. `newTunStack` applies them as gVisor TCP transport options (`tun.go`). The cap bounds **one retransmission interval**, not the cumulative time needed to deliver a byte. A lower cap can help TUN-owned TCP connections, but can also generate extra retransmissions on slow or congested paths. TCP's congestion-window reduction after real loss is not a setting this work should disable.

Transfer has a separate, higher-layer timer: `DefaultSendBufferSettings().MaxResendInterval` is 8 seconds for reliable-carried Packs, while `UnreliableMaxResendInterval` already caps unreliable-carrier retries at 2 seconds. A 2-second Transfer cap is a separate candidate, **not** the same as the TUN TCP RTO cap. On a reliable H1 stream, an extra Transfer resend may duplicate a Pack already queued for TCP recovery; on a route change it may instead aid delivery. The H1 raw-residence and hybrid-reliable paths also reference the setting. Evaluate its effects on logical ACK lifetime, duplicate writes, queue/cwnd pressure, throughput, and memory in a separately frozen 8-versus-2-second A/B (and then a factorial interaction check with the TUN setting). Do not infer a Transfer default from the TUN-only screen below.

Keep the lifetimes distinct: raw `DefaultSendBufferSettings().AckTimeout` is 60 seconds, whereas the affected MultiClient H1 path uses a 30-second ordinary provider-failure/ACK bar. The observed 30-second expiry is not evidence that the global Transfer default is 30 seconds; any Transfer A/B must report the effective lifetime per route so changing the resend cap is the only treatment.

## Client Transfer resend research

The current production default is `MaxResendInterval = 8s` (`transfer.go`), but that value is **not** a guaranteed physical write every eight seconds. It caps the ordinary per-item exponential interval and also feeds `NewRttWindow`, which influences scaled/deviation/probe RTT and mixed-lane late-not-lost grace. Actual writes can be postponed or preempted by cumulative ACK progress, ACK-coalescer state, pacing/admission, and writer bounds. The H1 raw-residence recovery bound uses the same maximum; a global change would also shorten H3 hybrid-reliable retry spacing. The unreliable-carrier cap is already 2 seconds. Ordinary P2P NoAck probe writer/head-of-line stalls are outside this resend loop, so a smaller `MaxResendInterval` cannot fix that root cause.

An isolated future Transfer experiment should hold TUN TCP RTO fixed and vary only the client Transfer maximum (8s, 4s, then 2s) on H1 reliable routes. Measure original-stream correctness, ACK expiry, physical and logical duplicate Pack writes, TCP retransmits/timeouts, cwnd/queue pressure, TTFB, throughput, peak heap and allocations. Include clean, low-edge, heavy-loss, and a high-RTT **no-loss** negative that can expose premature duplicates; preserve effective 30-second MultiClient ACK lifetime in both arms. A later H1 carrier-TUN factorial can reveal whether the two layers help independently or retransmit the same data twice, but that simulator outer-carrier result is not an Android/iOS OS-socket default. Evaluate H3 hybrid and P2P as separate scope before considering a global default. No isolated Transfer A/B measurements have been collected yet.

This timer map needs several qualifications before an experiment changes a global default. The default Transfer ACK lifetime is 60 seconds; MultiClient stamps 30 seconds on its generated exit client (`ip_remote_multi_client.go`), and an item that actually used an unreliable carrier can extend to 90 seconds. A reliable/fast hybrid is not H3-only: P2P Auto fallback can be classified hybrid too. The Transfer maximum also affects H1 raw-residence protection, H3/hybrid first retry, RTT/probe timers and mixed-lane grace. It is not a promise of one physical retry each N seconds: ACK progress, pacing, admission and writer bounds can move or suppress a write. In an isolated cold/no-progress model, the 8-second cap attempts near 2, 6, 14 and 22 seconds; 4 seconds near 2, 6, 10 and 14 seconds; 2 seconds near 2, 4, 6 and 8 seconds. More copies can consume wire/queue capacity even though Transfer identity deduplicates delivery. Same-stream H1 copies cannot bypass an earlier missing TCP segment. An H1-scoped test should precede any H3/P2P/global change.

### Directional IP ACK policy: why TCP remains ACK-required

The frozen 2026-09-25 source does **not** set ordinary TCP IP packets to Transfer `NoAck`: `ipPacketTransferAckForRequest` forces TCP ACK-required at both singleton and grouped MultiClient send boundaries (`ip_remote_multi_client.go`), even if a caller requests NoAck. UDP/ICMP can use NoAck under the direct/collapse policy. Provider-return TCP likewise forces Transfer ACK (`ip.go`). Consequently, the current client Transfer resend maximum can affect outbound TCP sending; it is not currently receive-only.

An opt-in client → provider TCP `NoAck` policy was built and verified in a **diagnostic-only** clean fixture, then **rejected as the current product direction by the user**. The receiver does not have a separate Transfer receive window that can reliably serialize arbitrary missing IP packets. A successful NoAck route write is not peer receipt; holes and out-of-order packets can multiply at the IP receiver. Keep MultiClient TCP IP packets Transfer-ACK-required so one exact packet remains owned and recoverable until the receiver acknowledges it. The provider may also have consumed upstream socket bytes before return delivery, so its TCP return remains ACK-required with retained exact bytes. The provider additionally has a bounded inner-TCP return cache and lost-middle/tail/EOF/IPv6 tests (`ip_tcp_return_recovery_test.go`), but that does not remove the current Transfer ACK contract.

TCP collapse prevention is the intended decoupling from an inner TCP sender's retransmits: while the exact ACK-required Transfer item is still recovering, MultiClient can suppress a repeat with the same covered sequence/ACK/window state instead of multiplying it into another Transfer Pack. SYN/RST and genuine sequence progress pass; `TcpCollapseMaxHold` bounds the suppression so the sender's only recovery is not hidden forever. `canSendPacket` already checks the selected client's ACK policy before collapsing (`ip_remote_multi_client.go`). This coupling, including singleton and grouped final send boundaries, must be pinned by deterministic tests and explained in code comments. Do **not** retain an outbound-TCP-NoAck production setting merely because a small pilot looked fast. UDP/ICMP have a separate current NoAck policy on some direct/collapse paths; no conclusion about changing that policy follows from this TCP decision.

The next fixture exercises production ownership: `Tun.DohCache` → `Tun.DialContext` → client inner TCP → actual MultiClient/device Transfer → fixed H1 route → provider NAT/return Transfer → TLS/HTTP/2 DoH server. No host-dial shortcut may satisfy a cell. It must assert **TCP Transfer ACK-required in both directions on the wire**, preserve one exit/egress and inner TCP identity through bounded loss/reconnect, and check full DNS answer/correctness, ACK lifetimes, collapse-drop counts, duplicate Pack/wire bytes, TCP retransmits/timeouts, TTFB, completion, allocations, peak heap and RSS. The active A/B is Ack/8s-TUN/8s-Transfer versus Ack/3s/3s, with Ack/3s/8s and Ack/8s/3s component controls. The 3s Transfer arm sets device and provider send caps while keeping their effective ACK lifetimes unchanged. Compare Ack/2s/2s only after the 3s/3s result; 3s/6s and 2s/4s are older hypotheses, not defaults. DoH answer latency/correctness can justify some bounded retry cost, but wire and memory cost must be reported.

The existing direct DoH/proxy TUN fixture isolates the **application TUN** and has no Transfer layer. A simulated carrier-TUN/H1 factorial would instead alter the outer H1 TCP owner and is not evidence that normal Android/iOS H1 platform sockets share that cap. Do not conflate these fixtures. Pinned gVisor declares `TCPMaxRTOOption` as a socket option, but its TCP endpoint `SetSockOpt` has no handling case; the sender reads the stack transport option at initialization. A per-DoH-connection cap therefore cannot currently be selected merely by setting the socket option on `TunTcpConn`. A separate DoH stack or gVisor change would need a measured memory/complexity case. Check the already dedicated DoH-TUN owner before allocating another stack.

The first four-cell **clean pilot** of the coupled fixture completed on a frozen 2,716-file source/test-env manifest (2026-09-25), before the NoAck direction was superseded. All four cells returned the same correct DNS answer/hash over the native client application TUN, same active device/provider identities and H1+ route, with two HTTP/2 requests and no workload Pack failure, ACK expiry, or TCP retransmission/timeout. Wire records showed actual DNS request payload NoAck only in the opt-in arms; provider return payload stayed ACK-required in every arm. One fresh process per cell gave TTFB: Ack/8s/8s **5.209 ms**, NoAck/8s/8s **4.900 ms**, Ack/3s/3s **5.207 ms**, NoAck/3s/3s **5.140 ms**. This is **n=1 scope/correctness evidence**, not a speed win or baseline. Its NoAck arms are historical diagnostics, not an active candidate. The whole fixture process's memory is not the iOS 24 MiB app profile. Raw pilot JSONL and source/protocol manifests: `/tmp/urnetwork-doh-transfer.RdgPzy/`.

The subsequent frozen 40-cell NoAck-oriented screen was **stopped by the user-directed policy change**, after its already-running cell reached terminal state. Preserve its 26/40 records: 20 correct original-stream DoH cells, six shared cold-3-second-RTT H1 setup failures (four clean and two one-loss; no measured DoH query or injected fault), zero invalid records; 14 cells were never started. Source/protocol hashes passed after the clean stop. These partial results must not be promoted or silently pooled with the new ACK-only timer screen. Artifacts: `/tmp/urnetwork-doh-transfer-screen.Y66xFF/`.

The six cold high-RTT failures expose a separate outer-H1 setup limit: `DefaultConnectSettings.HandshakeTimeout` is five seconds (`net.go`), and `DialFramedUpgrade` bounds TCP+TLS by that timeout (`http_upgrade.go`). At three seconds of outer RTT, TCP+TLS can exceed five seconds before HTTP upgrade; all four policy/timer arms failed before DoH in the clean cold profile. This is not evidence against inner application-TUN3 or Transfer3. For the next high-RTT timer test, establish the fixed H1 carrier under clean access first, transition only that established carrier to three-second RTT, then open/warm the inner DoH connection. Version this established-route scenario separately; retain the cold setup failures as their own issue.

### ACK-required 3s/3s established-route high-RTT validation (one paired index)

The separately versioned high-access-RTT fixture established H1 under clean access, then transitioned only the exact device carrier to a nominal three-second RTT while preserving device/provider identity. Both clean/no-loss arms kept the original native inner/upstream DoH TCP socket, returned the correct answer, and had no workload Pack/ACK failure: 8s/8s TTFB **6.009 s**, 3s/3s **6.007 s**. The 3s/3s arm nevertheless counted three inner-application-TCP retransmits and two timeouts versus zero/zero at 8s/8s, plus one collapsed inner resend versus zero. The observed inner TCP RTT before measurement was about 13.7 seconds, not three; report this profile as *three-second access RTT*, not calibrated inner-TCP RTT. These are one-sample mechanism observations, not a paired speed claim.

With exactly one targeted old-H1-carrier TCP byte transmission dropped, the **8s/8s** control returned the correct DoH answer on the original inner/upstream TCP socket in **6.307 s**, with zero workload Pack/ACK failures, zero application-TCP retransmits/timeouts, and one collapse drop. In the **3s/3s** cell, MultiClient issued a `Blackhole no-send-ack` health verdict at **6.113 s**, canceled its parent sequence, and moved the flow to fallback; four workload Pack failures followed. This is a **health-policy preemption**, not an observed Transfer ACK failure and not evidence that a 3-second per-retry cap implies a six-second total failure time. The candidate also left five provider-return Packs pending at terminal join, so it is invalid for completed-query interval performance accounting. The one-drop target and no explicit carrier close were verified. Different setup/health phases (about 61.49 s candidate versus 56.41 s control) mean one pair does not prove the timer pair alone caused the verdict.

Keep three clocks separate in every subsequent record: (1) inner TCP current/backed-off RTO and its maximum **per retransmission interval** (3s in the candidate); (2) MultiClient no-send-ACK health threshold, configured at 5s and observed firing after polling near 6.1s here; (3) effective Transfer ACK lifetime, 30s on MultiClient-generated device clients and 60s raw/provider default unless separately configured. A health verdict canceled this trial before clock (3) reached its terminal deadline. A separate deterministic no-health-verdict test must measure Transfer ACK terminal time and packet retry sequence under 3s/3s; it must not infer that total from the 6.1s observation. The failed trial's wire bytes are an incomplete prefix, not comparable to completed-query wire efficiency.

These are **independent intervals**, not a coupled 3+3-second deadline. The next trace must timestamp packet admission, first physical write, every TCP RTO/retransmission, every Transfer resend, peer Transfer ACK, each health evaluation/verdict, and terminal Transfer ACK failure. It must report elapsed time from the appropriate start for each clock and whether one clock preempted another. Keep production thresholds unchanged during the diagnostic; disabling only the early health verdict inside a test is a measurement isolation, not a proposed user setting.

The adjacent root cause to test deterministically is the current short no-send-ACK health rule: `blackholeReasonFromStats` uses an approximately five-second `BlackholeTimeout` when no send ACK has arrived and does not hold that verdict for a reliable H1 carrier still recovering a missing TCP byte. A successful route write is not proof of remote receipt. The selected remediation is to align the default no-send-ACK blackhole threshold with the H1 transport's default **30-second read timeout**, and pin that equality in a configuration test. This is a liveness boundary, not a new TCP RTO or Transfer resend interval. The no-send-ACK clock currently comes from a roughly 30-second rolling stats window that omits its two newest buckets; a naive 5→30-second constant change may make the verdict unreachable. Its setting also controls polling and comparative freshness. The implementation and deterministic tests must preserve a reachable deadline without growing that window or unintentionally changing those adjacent behaviors, and cover both one-loss recovery and a genuinely dead exit. This four-cell validation is not a release or default qualification. Source/protocol hashes and raw records: `/tmp/urnetwork-doh-ack-timers.KHUT2y/`.

Focused remediation check: the original deterministic test was red for both the **5s vs 30s default mismatch** and the no-ACK verdict disappearing when the rolling telemetry bucket aged out with one send still outstanding. The fix shares the default read interval, reads each API-generated client's effective copied transport interval, and uses a persistent outstanding-send progress clock; the old 1.25s detector poll and 5s comparative ACK freshness are independent. A successful NoAck UDP route write still contributes completion accounting but cannot reset true peer-ACK liveness; the 20s no-return branch requires real peer send-ACK evidence and cannot serve as an early no-ACK substitute. Deterministic Transfer tests pass with a peer ACK at 6.3s or just before 30s, a silent 30s boundary, custom read intervals 15s and 45s, and an independent 30s Transfer ACK expiry under read45s. The related Connect suite passed 10 repetitions, race passed three, and server DoH/wire/fault units passed 10. Logs and commands: `/tmp/urnetwork-h1-read-liveness.Yj184b/CHECKS.md`.

The exact established-route four-cell replay after the fix was source-frozen and all records were valid, correct, on the original H1+/DoH stream, and free of Pack/ACK failures, expiry, health verdicts, or pending terminal ownership. The formerly failing one-loss 3s/3s arm recovered in **6.311s**, versus the old early health cancellation at 6.113s. This qualifies the health remediation for that fixture; it does **not** establish a 3s/3s performance win. Measurements (one fresh process per cell):

| Established 3s access RTT | Arm | DNS TTFB | App TCP retransmits / timeouts | Transfer timeout writes device / provider | Carrier wire |
| --- | --- | ---: | ---: | ---: | ---: |
| No loss | 8s/8s | 6.007s | 0 / 0 | 1 / 0 | 10,440 B |
| No loss | 3s/3s | 3.005s | 2 / 2 | 2 / 2 | 15,364 B |
| One pinned H1 byte lost | 8s/8s | 9.856s | 2 / 1 | 2 / 0 | 23,906 B |
| One pinned H1 byte lost | 3s/3s | 6.311s | 4 / 3 | 14 / 4 | 43,623 B |

The timeout-write and TCP counters cover the measurement through application retirement, not just pre-answer query traffic. The 3s/3s cells were faster in this single run but used more retransmissions and **47% more clean / 82% more loss-profile wire bytes**. Warm inner-TCP RTT varied around 11.48–14.07s, and the previous clean pair was approximately 6.01s in both arms; timing phase matters. Do not promote 3s/3s defaults on n=1 or conflate whole-simulator memory with the iOS 24 MiB app profile. Full per-cell memory, exact counters, hashes and artifacts: `/tmp/urnetwork-doh-read-liveness.eAMHyQ/RESULTS.md`. A predeclared paired repeat and component controls are required before a timer-default decision.

Before that paired repeat, audit MultiClient TCP collapse prevention. Its default `TcpCollapseMaxHold` is 1.5s, after which one same-sequence retransmission is deliberately admitted per hold interval even if an ACK-required Transfer item may still be recovering. A 3s inner-TCP retry can therefore pass that gate. This is a provable policy behavior, **not yet an attribution** of the extra 3s/3s wire bytes: the four-cell records do not capture per-IP sequence/ACK/window lineage. Test the actual final-send singleton/group/race boundaries, older-hole recovery and stalled-Transfer escape before changing the hold; preserving the sender's only recovery path remains essential. The 40-cell paired cohort is paused pending this audit.

### TCP collapse and provider ingress ownership audit (partial fix; receiver remediation pending)

The 1.5s hold was introduced in commit `1f3e2f58` to avoid the earlier indefinite high-water gate discarding a stalled flow's only TCP retries for up to the 30s Transfer ACK lifetime. That commit explicitly said the field freeze motivating the escape had **not** been reproduced at runtime. The setting is therefore a safety hypothesis, not a measured optimum. `canSendPacket` checks flow sequence/ACK/window high-water, then `releaseSequenceHold`; it does not consult the exact pending Transfer Pack or its peer ACK. At unchanged state an inner-TCP retry every 3s passes the 1.5s escape by design. A group passes whole if **any** member advances, even if other members are duplicates. These are code facts, not proof of which packets caused the measured wire increase.

Deterministic RED cases now reproduce deeper correctness gaps (no production collapse/backpressure fix has passed yet):

- With a lower TCP segment refused by selected-client admission and a higher segment subsequently accepted, the lower segment's identical immediate retry is falsely collapsed by the high-water gate. This occurs in singleton, batch and mux paths, with both a zero (indefinite) and 1.5s hold. High-water does **not** prove that every earlier byte range was owned.
- After a duplicate is allowed by the elapsed hold but its selected queue refuses the send, the hold clock has already restarted; the identical immediate retry is then collapsed despite the failed admission. A failed send must not commit either byte coverage or a hold-escape decision.
- The ordinary first-attempt selected-queue refusal already lets an identical packet retry immediately; this existing correct behavior passes through the public singleton, batch and mux paths. The defect is specifically the previously committed high-water/hole case and the hold-escape mutation before a refused send. Keep those cases separate in regression reporting.
- With the provider local-NAT queue full, actual `Client.receive`/`ReceiveSequence.flushDeliver` records `UpdateDelivered=1` for the exact ACK-required Pack even though the packet has no downstream owner. The provider's `ClientReceive` callback returns `void`; `LocalUserNat.sendTransferPacketsWithTimeout(..., 0)` can refuse under queue or memory pressure, and the callback still returns to Transfer's deliver-before-ACK loop. Successful Transfer ACK therefore currently proves callback return, **not** durable TCP ingress ownership.

Even a successful local-NAT queue handoff is not the final ownership boundary: its asynchronous shard can refuse `TcpBuffer.tcpSend` packet memory or time out/refuse `TcpSequence.send`, after which `runSendShard` returns the packet to the pool. Blocking only the first queue is insufficient. ACK-required TCP backpressure must hold the receive owner until the downstream packet is secured, using bounded memory and cancellation-safe waits; a full queue must not become ACK-and-drop. Tests must cover saturation/release, cancellation/shutdown, impossible-to-fit budgets, IPv4/IPv6, grouped admission and no leak/deadlock under a shared memory parent. Legitimate semantic rejects (e.g. malformed packet, retired source, orphan flow with RST) need separate explicit policy rather than being hidden as congestion.

The intended collapse contract after remediation is: (1) a failed packet send leaves the identical packet immediately eligible through the gate; (2) a SYN with a **different** initial sequence number passes and resets the connection generation only after successful admission; (3) a same-ISN SYN retransmission is subject to the duplicate gate; and (4) a true peer ACK may justify suppressing its exact already-owned packet, not an unowned lower hole. An indefinite flow-lifetime collapse hold is unsafe under the current ACK-and-drop and pre-serialization-expiry paths, so do not set `TcpCollapseMaxHold=0` until those are fixed and the exact coverage tests pass. Afterward compare indefinite/exact-owner policies for correctness, memory and wire cost before resuming the 40-cell 3s/3s paired cohort.

The public-path new-ISN/same-ISN SYN tests currently fail against production behavior: a same-ISN SYN is allowed through unconditionally, and `resetSequenceGroup` runs before admission, so a rejected different-ISN SYN can erase old-flow collapse state. The desired rule is a successful different-ISN generation reset; a same-ISN retransmission is gated; a failed new SYN leaves the committed generation unchanged. The new-SYN test uses a new data segment at sequence 51, below the old committed high-water 102, to prove generation reset rather than merely later sequence progress.

Two further **real-path RED** tests complete the ownership boundary. With an empty NAT ingress but a held, full one-slot `TcpSequence` queue, the ingress accepts the payload, the TCP shard then rejects and frees it, and Transfer still marks the exact item delivered/ACKed; the ingress-drop counter remains zero because the loss is downstream. Separately, a real singleton packet admitted through the public MultiClient collapse/commit path expires before Transfer serialization at 100 ms; both an indefinite (0) and 1.5s hold retain false coverage and reject the immediate identical TCP retry. These are not mocked callback failures. Raw tests: `/tmp/urnetwork-tcp-collapse-audit.Y2NN6M/red-final-boundary.log`.

Transfer's receive sequence can selectively ACK out-of-order items under its committed-prefix policy. Consequently, just canceling a failed `flushDeliver` after one downstream refusal, or withholding only its final cumulative ACK, is not a complete fix: a per-item ownership receipt and the sequence's existing selective-ACK state must agree. No production receiver edit or collapse default change has yet been qualified. The next implementation design must preserve exact item identity through local-NAT and TCP-sequence admission, transfer prepaid packet memory rather than double-reserving, and distinguish secured, canceled and impossible-fit outcomes before publishing delivery evidence.

The proposed bounded implementation is a private per-item receipt and ordered receive ledger, leaving public `ReceiveFunction` unchanged. Provider TCP carries each receipt plus its prepaid packet claim through NAT/shard to `TcpSequence.send` (or final pure-ACK application); only a contiguous secured prefix can advance a cumulative Transfer ACK. Later controls must still dispatch and may receive selective feedback without claiming the earlier pending data. Exact retransmits consult the pending ledger rather than the past-sequence shortcut. Cancellation/failure drains claims without promoting them. A same-root memory-budget claim move can be made under the existing root admission lock, checking any target-only child ceilings without transiently double-charging the shared root; that behavior needs its own success/refusal/accounting tests. This is a design candidate, not a qualified fix.

The claim-move primitive has deterministic unit tests: `TestTransferMemoryReservationMoveAtFullRoot`, `TestTransferMemoryReservationMovePreservesTargetCeiling`, `TestTransferMemoryReservationMoveParentChildAndCancel`, and `TestTransferMemoryReservationConcurrentMovesStayBounded` passed focused repeats (`/tmp/urnetwork-tcp-collapse-audit.Y2NN6M/receiver-credit-move-unit.log`). Those primitive results alone did not resolve the receiver REDs; the integrated matrix below is the later evidence.

Claim-moving at the production boundary needs stricter proof than a generic `queueByteCount >= packet cost + 512` test: grouped or decoded Transfer frames may keep independent roots after a NAT packet is handed off. The first raw-root-only shortcut was rejected; the primitive being correct does not establish that any particular received item has transferable spare credit. Any valid path must identify a distinct prepaid claim and preserve the full charge for all still-live receive roots.

The current strict candidate is to prepay a separately labeled downstream NAT packet/envelope and receipt/operation allowance on retained provider TCP items *before* publishing an out-of-order selective ACK. Only that labeled extra credit may move atomically from the receive child to the NAT child; the original receive roots remain fully charged. In-order receipts reserve the same envelope before callback. This avoids both a full-root second-reservation deadlock and the unsafe guessed residual, but raises retained bytes. Deterministic prepay-failure/no-SACK, full-root cross-child transfer, target-child ceiling, grouped/decoded and cancel-balance tests plus the iOS-profile 24 MiB gate are required before keeping it.

First strict child/root measurements (`receiver-shared-root.log`): for the 1,200-byte raw-packet fixture, retained charge is 5,996 B = 2,048 B original receive root + 2,796 B explicitly movable downstream credit + 1,152 B receipt/operation envelope. `TestProviderReliableTcpPrepaidSharedRootHandoff` passes IPv4/IPv6 with a full shared root and with both permissive and refusing target-child ceilings; `TestProviderReliableTcpPrepayRefusalBeforeSelectiveAck` passes, withholding selective feedback when credit cannot be prepaid. `TestReceiveDeliveryHeadPrepayParticipatesInByteLimit` passes the in-order ledger admission ordering. These focused results are not a real iOS-profile peak or an end-to-end speed result.

The focused integrated receiver/primitive/shared-root family then passed ten repeats (0.629s) and three race repeats (1.843s) at `GOMAXPROCS=2`, `-p=2` (`receiver-integration-repeat.log`, `receiver-integration-race.log`). This does not substitute for the scoped iOS-profile 24 MiB peak or frozen paired 3s/3s performance comparison.

For a direct in-order head, receipt-ledger byte admission must include the newly prepaid amount before evaluating its own count/byte cap. An implementation that computes ledger `bytes` before increasing `item.queueByteCount` may pass the physical budget check but understate the ledger's bounded usage; a deterministic head/prepay-maxBytes case must pin this ordering.

The private receipt-ledger primitive also passed ten focused repeats (`receiver-ledger-unit.log`): `TestReceiveDeliveryLedgerSecuredPrefixAndDuplicate`, `TestReceiveDeliveryLedgerSealBeforePoolReturn`, `TestReceiveDeliveryLedgerFailureAndCancellationNeverPromote`, and `TestReceiveDeliveryLedgerCapacityAndConcurrentCompletion`. These pin cumulative-prefix versus later selective ACK, pending duplicates, borrowed-frame lifetime, failure/cancel, bounded count and exactly-once concurrent completion at the primitive boundary. The later provider-path matrix, not primitive tests alone, qualifies behavior.

The next focused primitive checks also passed: `TestReceiveDeliveryLedgerCancelJoinsBorrowedAndPendingOwners` verifies cancellation joins a callback-borrowed item with a pending downstream claim; `TestNatPacketReservationSplitAndMove` and `TestTcpPrepaidFinalAdmission` exercise IPv4/IPv6 final admission, full queue, cancellation, wrong-child and small-control-partition claims (`receiver-prepaid-unit.log`). The production NAT handoff was subsequently tested in the integrated matrix.

The scheduler reuses the existing ReceiveSequence worker rather than spawning per-packet goroutines or polling. Retained admissions remain in the bounded receipt ledger, are retried on coalesced NAT/TcpSequence capacity and budget-release notifications, preserve same-flow data order, and allow independent control/other-flow operations to progress. Cancellation must wake the worker and settle claims/source gates once. A notification is subscribed before its readiness check to avoid a lost wakeup, and a packet accepted by one layer is not re-enqueued there when the next layer refuses. Focused primitive and selected integrated tests cover these rules; broader performance and race qualification remain.

The receive worker currently has a fast path for draining Packs, then a blocking Pack/idle select. Capacity waiting should enter only when receipts are pending, leaving the ordinary hot select unchanged; multiple dynamic wakes need a bounded wait strategy rather than per-packet goroutines. A pending downstream owner also changes the meaning of idle retirement, so tests must pin whether that timer cancels the receipt safely rather than silently ACKing or leaking it.

The production wiring carries a private receipt through `Peer` with exact per-item application-frame counts, while preserving public `ReceiveFunction` and ordinary combined callback behavior. A provider TCP operation retains one original packet owner and its prepaid credit; NAT acceptance switches that same operation to final-TCP retry if needed. A pending-only slow path in `ReceiveSequence.Run` waits on bounded subscribed capacity channels (and lifecycle/Pack events). Tests that invoke `flushDeliver` without `Run` exercise the same pump explicitly or use the real worker; a test-only hidden worker would not qualify production behavior. Frame mapping must remain correct if `Client.dispatchSubprotocolFrames` filters frames before provider delivery. The full focused receiver/primitive family now passes, but repeated/race, strict shared-root and memory/performance gates remain.

The first integrated production-path boundary turned green in `TestProviderReliableTcpAckAfterFinalAdmissionWithoutRemoteAck/ipv4` (`receiver-integration-first.log`, 0.570s). The selected matrix then passed IPv4/IPv6 final admission, real-`Run` same-lane control with full NAT budget, two-item ACK barrier, pending duplicates and provider shutdown, prepaid NAT→TCP handoff, cancellation/impossible-fit and selective-lease retention (`receiver-integration-matrix.log`, 0.501s). With a full real TcpSequence queue, Transfer withholds its ACK; after capacity release the exact retained packet enters the queue and earns one exact ACK while upstream dials remain zero. The unsafe generic receive-credit split was removed pending strict shared-root proof. These selected tests are not a full suite/race or iOS-memory qualification.

The original NAT-ingress and final-TCP RED fixtures now also pass under the nonblocking receipt contract (`receiver-all-integration.log`, 0.556s; `GOMAXPROCS=2`, `GOWORK=off`, `-p=2`, `-run '^Test(ProviderReliableTcp|ReceiveDelivery)'`). The one pooled-root leak in the first adaptation was a test-fixture alias: its manual `deliverFrames` slice shared backing with `receiveItem.frames`, so clearing the batch erased the pointer that owned the borrowed raw frame. Separate slices keep that owner intact. This test repair does not weaken the production ownership assertions.

Before qualification, pin provider callback removal/re-registration on a reused Client. The proposed marker is intentionally sticky: once provider TCP requires receipts, removing its callback must fail closed (no generic void-callback ACK); a newly registered provider may claim subsequent items. A mixed ACK-required item containing TCP and non-TCP frames must require every TCP frame's claim while preserving the current deliberate UDP/NoAck semantics; do not describe this as new UDP reliability without a separate policy change. These are scope checks, not observed production failures.

The scheduler must also handle a ledger filled to its count/byte limit with pending data before a later same-lane ACK/control arrives. `TestReceiveDeliveryLedgerFullDataStillAdmitsBoundedControl` was deterministic RED (`receiver-control-reserve-red.log`): a ledger full by both count and bytes refused the control needed to release it. A separate two-owner/8-KiB control reserve, limited to small ACK/RST-only packets, now passes ten focused repeats along with `TestReceiveDeliveryLedgerControlReserveIsBoundedAndReusable` (64 consecutive applied controls) and `TestReceiveDeliveryLedgerControlReserveRejectsDataAndExcess` (`receiver-control-reserve-green.log`). Only adjacent already-secured controls compact; pending data remains the cumulative-ACK barrier. This is still primitive-level, not production-path or iOS-memory qualification.

`TestReceiveDeliveryRetryAllowsSynchronousCompletion` and `TestReceiveDeliveryRetryAllowsSynchronousRetry` provided deterministic RED evidence for the scheduler's reentrancy boundary (`receiver-reentrant-red.log`): the initial attempt ran under its operation mutex, so inline final-admission completion/retry would deadlock; inline retry could also be overwritten by the earlier attempt's return. The correction reserves an in-flight generation and invokes the attempt outside that mutex, reconciling cancellation/completion without releasing borrowed resources before the attempt returns.

The generation-reserved correction passes the full delivery-primitive suite ten times (`receiver-reentrant-green.log`, 0.551s), including both synchronous callback tests and borrowed-resource lifetime. These remain primitive-level checks; the integrated provider path has separate tests below.

RED log and test artifacts: `/tmp/urnetwork-tcp-collapse-audit.Y2NN6M/red-clean.log`; earlier four-cell counters and memory: `/tmp/urnetwork-doh-read-liveness.eAMHyQ/RESULTS.md`. The four-cell simulator's 64–65 MB heap and 149–151 MB process RSS include the entire fixture and are **not** iOS-app memory; they cannot qualify the iOS-only 24 MiB gate.

#### Deterministic invariant ledger

Each row requires a production-path, exact-item assertion; a callback mock or a final aggregate ACK count alone is insufficient. “Initial green” below is only the focused stage-1 result and does not waive the broader race, memory and performance gates.

| Invariant | Deterministic test / required status |
| --- | --- |
| A refused first send, lower hole, or hold-escape attempt cannot commit collapse coverage; its identical packet retries immediately through singleton, batch and mux admission. | `TestTcpCollapseFailedAdmissionAllowsImmediateIdenticalRetry`, `TestTcpCollapseRefusedLowerHoleIsNotCoveredByLaterAdmission`, `TestTcpCollapseFailedEscapeDoesNotCommitHold`: initial green. |
| A packet expiring before real Transfer serialization revokes its coverage even when the completion callback races the admission commit. | `TestTcpCollapseUnwrittenExpiryAllowsImmediateRetry`, `TestTcpCollapseUnwrittenCompletionBeforeCommit`: focused green. |
| A same-ISN SYN retransmit is gated; a different-ISN SYN resets the generation only if admitted; a refused new SYN preserves the prior generation. | `TestTcpCollapseSynGenerationCommitsOnlyOnAdmission`, `TestTcpCollapseStaleCommitAndProviderRebindFailOpen`, `TestTcpCollapseConcurrentRefusalPreservesSuccessfulAdmission`: focused green. |
| A provider Transfer ACK for an ACK-required TCP item is withheld while the local-NAT ingress queue lacks ownership. Queue release must preserve the exact item, then permit its ACK. | `TestProviderReliableTcpAckWaitsForNatOwnership` passes on the integrated nonblocking path; intermediate queue ownership does not emit a final ACK, and disposing it fails the receipt without a pooled-root leak (`receiver-all-integration.log`). |
| Local-NAT ingress alone is insufficient: a full or refusing downstream `TcpSequence` queue must neither discard nor ACK the exact item. ACK follows final local TCP admission, not a remote website TCP ACK. | `TestProviderReliableTcpAckAfterFinalAdmissionWithoutRemoteAck` passes IPv4/IPv6 on the integrated path (`receiver-integration-matrix.log`): no early ACK, then exact packet/ACK after queue release with zero upstream dials. The original `TestProviderReliableTcpAckWaitsForFinalTcpAdmission` also passes (`receiver-all-integration.log`). |
| Out-of-order selective ACK and later cumulative ACK must never claim an item whose downstream admission is pending, refused or canceled; each receipt follows its own item identity even when `flushDeliver` combines multiple items' frames into one callback. | `TestProviderReliableTcpBatchAckStopsAtFirstUnsecuredItem` and `TestProviderReliableTcpSelectiveLeaseSurvivesFinalAdmissionPressure` pass IPv4/IPv6 on the integrated path (`receiver-integration-matrix.log`), after deterministic baseline REDs. |
| An exact retransmit of a pending or refused item cannot be auto-ACKed merely because `ReceiveSequence.nextSequenceNumber` advanced before `flushDeliver`; only a secured item may use the past-sequence ACK shortcut. | `TestProviderReliableTcpPendingDuplicateCannotAck` passes IPv4/IPv6, including after provider shutdown (`receiver-integration-matrix.log`), after baseline RED. |
| A later secured control item may progress while earlier data is pending, but it must not become a cumulative ACK across that data. | `TestProviderReliableTcpPendingDataAllowsSameLaneControl` passes IPv4/IPv6 on the real `Run` path, including a full NAT budget (`receiver-integration-matrix.log`), after baseline RED. |
| Backpressure must be bounded and cancellation-safe, including full shared memory budgets, impossible-to-fit packets and same-lane control packets needed to release pressure. Pending same-flow data must retain order; other flows/control must progress without a busy retry loop. No ACK-and-drop, leak or deadlock. | `TestProviderReliableTcpQueuedCreditNeedsNoSecondReservation` and `TestProviderReliableTcpCancellationAndImpossibleFitNeverAck` pass IPv4/IPv6 (full NAT root; provider/NAT shutdown and impossible-fit). Primitive scheduler order/progress/bounds/no-spin tests pass. A shared receive-child→NAT-child move at an already full root remains a stricter accounting/forward-progress case to prove; full suite and iOS memory are pending. |
| Ownership and collapse rules hold for IPv4/IPv6, FIN and sequence wrap, pure ACK/window updates, grouped admission, route rebinding and concurrent refusal. | `TestTcpCollapseCoverageWrapAndFin`, `TestTcpCollapseCoverageDoesNotInventHoles`, existing window/group tests and stale-rebind/concurrent-refusal tests are focused green; IPv4/IPv6 receiver and race suite remain pending. |

The stage-1 collapse fix uses a single contiguous accepted-byte interval and invalidates coverage on real unwritten expiry. Its expanded focused tests pass (`/tmp/urnetwork-tcp-collapse-audit.Y2NN6M/stage1-expanded.log`), as do ten focused repeats (1.948s) and three race repeats (2.288s) at `GOMAXPROCS=2`, `-p=2` (`stage1-repeat.log`, `stage1-race.log`); those early runs predated receiver integration. It adds 40 bytes per `multiClientChannelUpdate` and 16 bytes to each parsed packet/group descriptor. In five-repeat, 200ms/bench, archived-source-overlay microbenchmarks, gate/commit median was 108.0→108.7 ns/op, unchanged at 352 B/1 alloc; parsed singleton admission 593.9→534.3 ns/op, 464→496 B with 10 allocs unchanged; parsed eight-packet admission 1993→1518 ns/op, 1752→1896 B with 17 allocs unchanged. Ordered microbenchmarks (`bench-baseline.log`, `bench-candidate.log`) are not a statistical end-to-end performance win. A real iOS-profile 24 MiB qualification remains pending; whole-simulator or struct-size arithmetic is not that gate. **Do not promote the paired 3s/3s timer cohort until the strict shared-root, memory and performance gates are green.**

### Real Android platform TCP check (read-only, 2026-09-25)

The two allowlisted physical phones report the following **running-kernel** values in `/proc/net/snmp`, in milliseconds. These are platform TCP values, not our gVisor application-TUN setting. `ss -tin` observed current, adaptive established-socket RTOs around 230–260 ms; current RTO must not be mistaken for maximum RTO.

| Device | Running kernel | `Tcp RtoMin` | `Tcp RtoMax` | `tcp_retries2` |
| --- | --- | ---: | ---: | ---: |
| Pixel 8 Pro (Android 17) | 6.1.162-android14-11 | 200 ms | 120,000 ms | 15 |
| Galaxy S24 (Android 16) | 6.1.145-android14-11 | 200 ms | 120,000 ms | 15 |

Thus a 3-second **platform** maximum is not a valid assumption for either tested phone. Android common-kernel source for the matching 6.1 line defines `TCP_RTO_MAX = 120*HZ`; [Android GKI releases](https://source.android.com/docs/core/architecture/kernel/gki-android14-6_1-release-builds), [Android TCP source](https://android.googlesource.com/kernel/common/+/refs/tags/android14-6.1-2026-03_r1/include/net/tcp.h), and [Linux TCP sysctl documentation](https://kernel.org/doc/html/latest/networking/ip-sysctl.html) corroborate the distinction. No universal iOS maximum was established; do not infer it from Android. This platform check does not weaken the case for a 3-second **application-TUN** candidate on DoH/proxy, whose TCP stack we control.

The relevant users are different:

| Path | TCP owner | What a TUN RTO change can affect |
| --- | --- | --- |
| Remote DoH in `DohCache` | `Tun.DialContext` through `DohSettings.DialContext` | DoH request TCP/TLS/HTTP/2 recovery |
| Local/host DoH | host `net.Dialer` | Nothing; this is a negative control |
| `server/proxy` `ProxyDevice.DialContext` | private TUN | New proxy-device TCP streams and their recovery |
| Normal H1/H1+ access transport | platform dialer/OS TCP unless explicitly injected | Nothing directly; PERFVAR's carrier TUN is a simulator substitution and must be labeled separately |

An established proxy TCP stream cannot be transparently moved to a new connection. An early retry is only plausible for a bounded, idempotent DoH request; a proxy failure may allow the *application* to open a new stream, but the original stream still fails. Do not change a provider's egress IP mid-session.

## Why this was investigated

A PERFVAR `exchange-h1` low-edge run intermittently hit a live 30-second Transfer ACK lifetime and reset its TCP workload. In a fresh paired-process reproduction, Pack 52 remained pending after cumulative ACK 51 despite four accepted route writes. A failure-triggered, bounded 208,908-byte goroutine snapshot found H1+ on both ends, H1+ readers waiting in TLS/gVisor TCP header reads, idle writers and ACK workers, and zero logical receive/ACK backpressure drops. This locates the missing boundary below H1+ framing; it does **not** by itself prove a particular TCP RTO state in that failing process.

A separate real gVisor TCP + TLS 1.3 + H1+ controlled-loss test showed accepted application writes while TCP recovered. With ten encrypted-payload packet losses and the production 8-second RTO cap, TCP reached cwnd 1, an 8-second RTO, eight timeouts and nine retransmits at 28.8 seconds; all frames arrived around 36.8 seconds without a further application write. Thus the 8-second per-retry cap can still outlast the unchanged 30-second logical ACK lifetime. This is injected mechanism evidence, not retrospective attribution of the intermittent run.

## Experiment contract

The test-only PERFVAR A/B harness uses real `Tun.DohCache` over HTTP/2/TLS and a proxy-device-equivalent `Tun.DialContext` byte stream. It compares the current 8-second cap, 4 seconds, 2 seconds, and an 8-second no-first-response-byte retry on a new connection for idempotent DoH. The proxy retry arm keeps original-stream failure visible even if an explicitly opened replacement succeeds. A combined arm is not justified unless independent arms survive.

The injector drops the exact first *N TCP payload packets*, never SYN or ACK-only packets. Cells use matched scenario, profile, trace and source identities, `GOMAXPROCS=8`, fresh processes, and record useful-byte/hash correctness, TTFB, throughput, TCP retransmits/timeouts, peak Go heap, and process maximum RSS. Profiles include clean, clean 3-second RTT, one/three/six/ten payload drops, and low-edge. These are diagnostic cells; baseline promotion requires paired repeats and the full applicable PERFVAR/DoH/proxy/memory gates. The iOS-profile 24 MiB absolute gate remains separate; process RSS from this host is not an app memory qualification.

## Pilot measurements (one sample per cell; not promotion evidence)

The first 48 valid cells covered clean, 3-second RTT, and one/three/six/ten-drop profiles across both consumers and all four arms. All DoH answers were correct. The proxy early-retry ten-drop cell correctly marks the original stream failed while a new connection succeeded; it is **not** a successful original-stream result. One- and three-drop cells recovered before the cap mattered, with essentially identical TTFB across arms. Clean 3-second-RTT cells had zero measured retransmits/timeouts and approximately 3.00-second TTFB across arms.

| Consumer / loss | 8 s control TTFB | 4 s cap | 2 s cap | 8 s DoH retry or explicit proxy replacement |
| --- | ---: | ---: | ---: | ---: |
| DoH / six drops | 6.414 s | 6.413 s | 5.214 s | 6.414 s, no retry |
| Proxy / six drops | 6.412 s | 6.415 s | 5.212 s | 6.414 s, original intact |
| DoH / ten drops | 36.817 s | 22.418 s | 13.217 s | 8.224 s, two attempts |
| Proxy / ten drops | 36.817 s | 22.417 s | 13.218 s | 8.814 s replacement; **original failed** |

The ten-drop 2-second arm still used one connection with ten retransmits and nine timeouts. DoH's early-retry arm used two attempts and more TCP/TLS packets, so its apparent speed gain must be weighed against traffic, memory, and retry safety. No statistically supported winner exists yet.

The pilot's four attempted DoH low-edge cells are **invalid experiment setup**, not product failures: the selector used a profile catalog without cell-edge entries, yielding zero rate/MTU/seed. They are excluded. No proxy low-edge cell started. The selector has since been corrected to the cell-edge catalog with exact rate, MTU, seed, and loss-mode guards; those cells must be rerun before any default decision. The first 48 valid cells and invalid files are preserved, not overwritten.

## Corrected low-edge pilot and five-seed paired screen

The corrected low-edge pilot completed all eight arms (two consumers × four arms), plus a clean bootstrap, with full original-stream correctness. Its 8-second versus 2-second TTFB was 1.008 versus 1.009 seconds for DoH and 0.866 versus 0.866 seconds for proxy; the proxy 16 KiB completion time was 2.559 versus 2.558 seconds. Both caps recorded zero retransmits/timeouts in these two cells. The corrected profile is exactly 64 kbit/s up, 256 kbit/s down, 1,200-byte inner MTU, 1,280-byte outer MTU, seed 20260810.

The subsequent screen ran five matched fresh-process pairs per consumer/profile/arm, alternating arm order: 140/140 cells preserved expected bytes and hashes, with one intact connection and no application retry. The frozen 35-file source manifest was unchanged. These are diagnostic measurements, **not** baseline promotion or statistical confirmation. Selected median TTFB values are below; seconds unless marked otherwise.

| Consumer / profile | 8-second cap | 2-second cap | Interpretation |
| --- | ---: | ---: | --- |
| DoH / clean | 2.381 ms | 2.461 ms | Small, noisy increase; paired median time-saved estimate −3.49%, 95% bootstrap interval −12.85% to +4.13%. |
| Proxy / clean | 2.613 ms | 2.627 ms | Essentially tied; no retransmits in either arm. |
| DoH / 3-second RTT, clean | 3.001968 s | 3.002129 s | Essentially tied; no retransmits in either arm. |
| Proxy / 3-second RTT, clean | 3.001464 s | 3.002001 s | Essentially tied; no retransmits in either arm. |
| DoH / six drops | 6.417421 s | 5.216678 s | ~18.7% faster recovery with the same six forced drops. |
| Proxy / six drops | 6.416680 s | 5.217291 s | ~18.7% faster recovery with the original stream intact. |
| DoH / ten drops | 36.822729 s | 13.223971 s | ~64.1% faster, ten retransmits and nine timeouts in both arms. |
| Proxy / ten drops | 36.823692 s | 13.221509 s | ~64.1% faster, original stream intact; no transparent retry. |
| DoH / low-edge | 0.967744 s | 0.831655 s | Paired median ratio 0.9992, so unpaired medians overstate any effect. |
| Proxy / low-edge | 1.069993 s | 1.070038 s | Paired median ratio 1.0006; one-off tails require more samples. |

Every heavy-loss pair favored 2 seconds, but with only five pairs the minimum possible two-sided exact paired p-value is 0.0625; exploratory multiple-comparison adjustment is weaker still. The screen therefore does **not** establish a statistically better setting. Process Go heap peaks and host max RSS are diagnostic-process measures, not the iOS-profile 24 MiB app gate. Clean-path DoH microsecond-level differences and the low-edge tail need independent confirmation.

Memory is not yet a clean pass: for proxy on the clean 3-second-RTT profile, the 2-second arm's paired median peak Go heap was approximately 1.47 MiB (+13.33%) higher (arm medians 10.983 versus 12.497 MiB), despite zero measured retransmits/timeouts and a lower paired median process max RSS. Three candidate outliers had zero GC cycles while all five controls had one; total allocated bytes were essentially equal at about 4.79–4.80 MB and goroutine counts stayed bounded. This supports GC-phase variation, but it must remain an explicit confirmation guard. Preserve natural GC in repeats and report initial heap, total allocation, GC cycles, peak heap and RSS together. Do not infer app-memory safety from host-process RSS or from a single heap statistic.

A clean 3-second-RTT path is also an incomplete guard: it had no loss, so it did not test whether a 2-second cap causes premature retransmission at high RTT. The independent confirmation must include a matched 3-second-RTT **plus one payload-drop** adversarial cell for each consumer, and compare retransmissions, timeouts, correctness, memory, and latency tails. A lower cap must not be promoted on its burst-loss benefit alone.

## Independent 8s-versus-2s TUN confirmation (completed; mixed)

The predeclared 2026-09-25 confirmation used ten disjoint fresh-process pairs (run indices 11–20) for each of six profiles × two consumers: **240/240 full answers/streams correct**, all 35 frozen source hashes and retained artifact hashes verified. An administrative pause after 45 correct cells repaired only an exact-filename validator collision; all 45 original records were retained/revalidated, and only the remaining 195 cells resumed. The original plan, measurements, source and statistical criteria did not change. The host's two-TUN process is not the iOS 24 MiB app-memory qualification.

| Consumer / profile | 8s median TTFB | 2s median TTFB | Relevant cost |
| --- | ---: | ---: | --- |
| DoH / six exact payload drops | 6.414 s | 5.214 s | Same six retransmits/five timeouts. |
| Proxy / six drops | 6.415 s | 5.216 s | Same six retransmits/five timeouts. |
| DoH / ten drops | 36.823 s | 13.222 s | Same ten retransmits/nine timeouts. |
| Proxy / ten drops | 36.822 s | 13.223 s | Same ten retransmits/nine timeouts. |
| DoH / 3s RTT + one drop | 10.706 s | 10.706 s | **One → two retransmits; zero → one timeout; 843 → 1,227 median wire bytes.** |
| Proxy / 3s RTT + one drop | 9.206 s | 9.207 s | **One → two retransmits; zero → one timeout; 17,810 → 17,964 median wire bytes.** |

All four heavy-loss TTFB primaries met the predeclared paired significance test (exact two-sided p = 0.001953, Holm-adjusted p = 0.0078125); six-drop recovery saved about 18.7%, ten-drop about 64.1%. The 3-second-RTT/one-drop control exposed a significant extra retransmission, timeout and wire-byte cost in **both** consumers without a material median TTFB gain. Two allocation guards also flagged small (~2 KiB median) increases, and several heap/latency noninferiority bounds were not established. The frozen decision rule therefore yields **MIXED / no global 2-second default**. This is not a DoH-specific rejection: DoH may rationally trade a small number of extra packets for faster answers, but that requires the production-shaped, consumer-weighted fixture above. Production `TunSettings.TcpMaxRto` remains 8 seconds.

The high-RTT/one-loss extra retransmit (1→2) and timeout (0→1) occurred in **all ten pairs per consumer** (exact p = 0.001953). Paired wire-byte increases were 45.55% for DoH (95% interval 31.28–47.04%) and 1.009% for proxy (0.568–1.455%). DoH TTFB was not statistically worse, but its noninferiority upper bound was 1.079, above the predeclared 1.05 limit. Peak-heap noninferiority upper bounds were +0.589 MiB (DoH clean high RTT), +0.601 MiB (DoH high RTT plus loss), +0.696 MiB (proxy clean high RTT), and +1.479 MiB (proxy low-edge), all above the +0.5 MiB guard; these are unresolved bounds, **not** statistically proven heap regressions. Raw allocation guards flagged +2,696 bytes for DoH six-drop (p = .0410) and +3,356 bytes for proxy ten-drop (p = .0293), small effects whose median confidence intervals cross zero. No TTFB, completion-time, or host-RSS metric was statistically worse. The 4-second cap has only its one-sample pilot: ten-drop recovery near 22.42 seconds, with no six-drop gain; it is not a qualified fallback yet.

## Receiver-credit qualification and 3s/3s replay (2026-09-25)

The stricter receiver ownership tests now cover a *full shared memory root*,
not merely a full receive child. IPv4 and IPv6 both pass with a permissive or
refusing NAT-child ceiling: selectively acknowledged data prepays its eventual
NAT/TCP ownership before feedback, a refused target does not consume that
credit, and a same-root handoff neither grows the root nor releases a still-live
receive owner. A separate test proves that a prepay refusal cannot emit a
selective ACK; another pins the in-order byte-limit check. The focused suite
passed ten normal repeats and three race repeats. In its 1,200-byte fixture,
retained charge is 5,996 B (2,048 B original root, 2,796 B movable downstream
credit, 1,152 B metadata); this is an accounting measurement, not app memory.

A fresh, source-attested Android `ios-memory-audit-v1` H1/Wi-Fi client screen
passed the scoped 24 MiB hard gate: 28 runtime samples peaked at 22,949,920 B
(21.89 MiB), with zero over-cap samples; 21 quiet samples covered 300,000 ms
and peaked at the same value. The independent quiet-start boundary was
23,490,592 B (22.40 MiB). Five Wikipedia navigations had 260.8 ms median
document TTFB and 617.7 ms median load. Three valid Fast.com samples were
1.4, 0.520 and 1.7 Mbit/s (median 1.4 Mbit/s), far below the 40-Mbit/s goal.
This public-exit arm lacks a contemporaneous Direct or previous-source
control, so the speed result cannot be attributed to the receiver fix or a
timer setting. It does **not** qualify provider-role memory, all carriers, or
the full two-role iOS-profile gate.

The predeclared 40-cell paired 8s/8s versus 3s/3s DoH replay stopped at its
*first 8s/8s control*, before any 3s/3s cell. After successful low-latency
route readiness, the clean 3-second-access-RTT warm DoH produced no answer:
the TUN dial took about 22.5 seconds and TLS remained incomplete at the
roughly 30-second warmup failure. The one attempted cell was incorrect and
39 were not started. Its post-failure diagnostics show a pending device send
sequence, but do not establish which layer stalled. Early-return zero fields
are unavailable measurements, not zero retransmissions. Thus there is no
valid paired timer or end-to-end speed comparison and **no basis to change
either 8-second default**. Reproduce and attribute this control warmup first,
then rerun a fresh, frozen paired cohort with a matched Direct physical speed
bracket and the full two-role memory gate before promotion.

Evidence: `/tmp/urnetwork-receiver-qualified.MNgaxK/RESULTS.md` and its
per-cell log/JSON; physical arm `/tmp/urnetwork-h1-receipts.WUJihj/`.
All 3,985 source-input and 14 protocol hashes stayed unchanged through the
terminal attestation. These temporary local artifacts are not a baseline.

Two additional deterministic IPv4/IPv6 tests then exposed receiver-lane
continuity defects: a healthy burst of three pure TCP ACK controls exhausted
the two-slot emergency reserve before the normal ledger drained, and a
successfully handled existing-flow or orphan RST canceled the shared receive
sequence. Ordinary controls now use bounded regular-ledger capacity first,
with the unchanged two-slot/8-KiB reserve only under pressure; handled RSTs
complete their exact ownership without claiming rejected data. Tests also pin
the 4-regular-plus-2-reserve bound, refusal/no-ACK behavior, prepaid-budget
accounting and next-flow continuity. The expanded receiver, shared-root,
collapse and admission suite passed ten normal and three race runs. This is a
new source build, so the prior scoped physical memory pass does not qualify it.

One standalone, source-attested exact 8s/8s high-RTT control replay still
failed at DoH warmup, before the measured request. An opt-in failure-only
snapshot was complete (199,539 B in 6.588 ms, no truncation): the origin
accepted TCP but received no TLS ClientHello; the device cumulative Transfer
ACK head remained at 16. Both H1+ connections were live, with no link drops,
Pack/ACK expiry or current receiver-capacity wait. This localizes the observed
failure to the ordered packet/ACK path but does not prove whether an earlier
receipt cancellation/reformation or another head-gap mechanism caused it.
The bounded snapshot test passed ten normal and three race runs. The exact
Pack/ACK lineage remains the next diagnostic; the 3s/3s candidate and a new
end-to-end comparison remain unstarted. Replay artifacts:
`/tmp/urnetwork-doh-warm-control.5Ztomi/RESULTS.md`.

Subsequent bounded lineage showed the same logical head-gap class: a small
Pack repeatedly reached the provider callback without cumulative ACK while
later Packs were selectively ACKed. A separate exact failure-only metadata
replay identified a 52-byte IPv4 FIN+ACK, zero payload, rejected eight times
as `no_flow` after the local TCP flow had retired; this run's sequence number
was 17, so it does not retroactively identify the previous run's Pack 16
flags. Deterministic IPv4/IPv6 tests reproduced lane cancellation for empty
orphan ACK, FIN and FIN+ACK, including budgeted ownership; payload-bearing
orphan controls correctly remained unacknowledged. The narrow correction
consumes only an empty orphan control after the local terminal TCP decision,
leaving optional/rate-limited reset policy unchanged. Targeted positives and
payload/shutdown/refusal negatives pass; a broad ten-repeat normal run also
passes, as do three broad race repeats. The exact uninstrumented 8s/8s/run2
control then passed on the corrected build: warm and timed DoH answers were
authoritative on one original stream, with no Pack/ACK expiry or duplicate
query; measured TTFB was 6.007554 s, 13,258 B full-interval wire, and zero
application-TCP retransmits/timeouts. Its host-process heap/RSS are not iOS
profile measurements. This is one correctness control, not paired performance
or a default decision. The paired timer benchmark is still pending. Evidence:
`/tmp/urnetwork-doh-orphan-control.2WZTCH/`, `/tmp/urnetwork-doh-warm-lineage.xfQ8rO/` and
`/tmp/urnetwork-doh-rejection-lineage.4hoH4Z/`.

The first 48-cell timer cohort stopped at cell 2, an exploratory 3s/8s
component, before any 3s/3s primary cell. Its DNS answer and original stream
were correct, but a post-answer ACK-required item remained live past the
unchanged 30-second terminal ownership join. This invalid cell is not a speed
measurement. Bounded packet/TCP metadata isolated the item to a 24-byte
PSH+ACK sent after explicit DoH cache retirement: an earlier Pack carried the
same TCP sequence, payload length and hash plus FIN, and the provider ACKed
through the FIN before removing the flow. The later new Transfer Pack repeated
the already-consumed 24 bytes about 12.6 ms before the earlier Pack's Transfer
ACK reached the device; provider correctly refused it as orphan payload.
Blanket-ACKing orphan data would break the ownership invariant. This is a real
late-close retransmit/retired-flow issue, but not evidence that a live DoH
request fails under 3s/3s. Preserve the post-close test and failure. A
separately named, deterministic *keep-alive request-boundary* fixture will
measure the ordinary DoH pool before explicit cache retirement, then perform
unconditional cleanup outside the timed verdict. A fresh primary-only paired
cohort is required; the stopped cohort cannot be resumed or cherry-picked.
Evidence: `/tmp/urnetwork-doh-timer-pairs.4eAeTR/`,
`/tmp/urnetwork-doh-terminal-lineage.uunpm6/`, and
`/tmp/urnetwork-doh-terminal-tcp.hAElrA/`.

The fresh v4 keep-alive primary cohort also stopped early: three valid cells,
one invalid carrier join, 36 unstarted. Its one complete clean pair measured
8s/8s TTFB 3.004877 s versus 3s/3s 3.005649 s (ratio 1.000257, no useful
gain in this single pair). The 3s/3s arm used 33,596 B versus 49,087 B of
*inclusive* wire and fewer same-query Transfer duplicates, but had two
application-TCP retransmits and one timeout versus zero. The one-loss 3s/3s
cell was correct at 6.888722 s; its 8s/8s match is invalid and excluded, so
there is no loss-pair comparison. All four cells returned the exact answer on
the original stream with zero measured workload Pack failures. The invalid
cell failed only at the global carrier-idle join after its source and Pack
boundaries had joined. Maintenance `IpIpPing` expiries were present, but the
uninstrumented record cannot assign every retained physical packet to a
producer. Do not call this a timer regression or relax request ownership by
assumption. A versioned request-scoped physical-prefix fence is being tested:
it must finish all already-admitted request traffic and duplicates, recapture
source/Pack generations, retain failure gates, and expose later maintenance
backlog separately. The v4 global-idle scenario and its stopped records remain
unchanged. Evidence: `/tmp/urnetwork-doh-keepalive-pairs.d0SnlN/RESULTS.md`.

The v5 request-prefix cohort then completed **32 valid cells** (eight matched
pairs each for clean and one-byte-loss) before its 33rd launch failed before
fixture construction: the identity generator accepted run 31/32 while the
replay selector still required `1..30`. Seven cells were never started.
The stopped cohort is **partial, not the predeclared ten-pair final analysis**;
no invalid/missing cell was replaced. A deterministic behavioral RED reproduced
the identity/launch mismatch. Both now share a test-only `1..64` run-domain
validator; focused normal ten repeats and race three repeats pass. This later
repair does not retroactively complete the frozen cohort.

| Valid partial profile | Pairs | 3s/3s wins/losses | One-sided exact sign p | Median paired 3s/3s:8s/8s TTFB | Median paired TTFB delta |
| --- | ---: | ---: | ---: | ---: | ---: |
| Clean | 8 | 4/4 | 0.63671875 | 1.000118 | +0.354 ms |
| One-byte loss | 8 | 4/4 | 0.63671875 | 1.134652 | +849.678 ms |

Even two further wins could not meet the declared ten-pair sign threshold.
The eight-pair medians show no useful global 3s/3s gain; individual pairs
nonetheless ranged from large wins to large losses. Median paired candidate
costs were +1/+2 application-TCP retransmits, +274/+550.5 B same-query
duplicate wire, +3,619.5/+3,662.5 B *inclusive finite-prefix* physical wire,
and +767,276/+518,268 B host heap peak (clean/loss). These are diagnostic
host and finite-prefix counters, not request-exclusive network bytes or the
iOS-profile memory gate; no 3s/8s or 8s/3s component arm completed, so the
two timeout caps cannot be separated by this cohort.

All 32 executed requests returned the exact DNS bytes on the original stream
and passed source/Pack/captured-physical ownership with zero workload Pack
failures and active ACK expiries. Five all-Pack failures were independently
classified empty `IpIpPing` outside the measured DNS workload and retained in
the records. All explicit cleanup joins returned, but post-request callback
deltas were nonzero (device/provider all-Pack 36/196; workload 32/97). Their
individual causes were not established and are **not** proof of clean TCP
closure. The late-close duplicate-payload defect remains open, and the full
physical/two-role 24 MiB qualification is deferred by user priority. Keep
production TUN and Transfer defaults at 8s/8s. Full report and immutable
records: `/tmp/urnetwork-doh-prefix-pairs.CZ6q3i/RESULTS.md`.

The next ACK-required timer candidate is **5s/5s**, a middle arm between the
retained 8s/8s default and the inconclusive 3s/3s candidate. The DoH Transfer
fixture accepts `ack-5-5`; its identity/scope tests pin both the inner TUN TCP
cap and device/provider Transfer caps to five seconds. One fresh-process
same-seed pair per profile was run on the production-shaped DoH path. Both
arms returned exact DNS bytes on the original stream and passed the fixture's
ownership gate:

| Profile | 8s/8s TTFB | 5s/5s TTFB | Inner TCP retransmits / timeouts, 8s/8s → 5s/5s |
| --- | ---: | ---: | ---: |
| Clean | 4.858 ms | 5.014 ms | 0/0 → 0/0 |
| Established three-second access RTT, one pinned H1 byte lost | 12.503 s | 14.400 s | 1/1 → 2/2 |

The clean difference is tiny, while this one-loss trace is 1.897 s slower and
uses one extra inner-TCP retransmission/timeout under 5s/5s. These are **n=1
mechanism checks, not a statistically supported timer verdict**. Keep the
production 8s/8s defaults. A future decision requires interleaved matched
clean, loss and low-edge cohorts, same-query duplicate/wire and memory bounds,
and 5s/8s and 8s/5s component controls. Do not pool these records with the
old partial 3s/3s cohort.

Separately, normal established-flow UDP NoAck was tested in PERFVAR. The
initial provider-race send remains ACK-required. At H1 5-Mbit/s clean and
200-bp-loss offered-rate workloads, both ACK and NoAck delivered all 3/3 runs
in each direction; goodput sat at the fixed offer ceiling, so these are
correctness, not speed-win, records. At 1-MiB clean QUIC upload, 10-run H1
medians were 213.627 (ACK) versus 213.345 (NoAck) Mbit/s; P2P-fast three-run
medians were 256.042 versus 256.964 Mbit/s. A whole-logical-group caller-side
direct-write variant produced H3 10-run NoAck medians of 112.045 and 115.507
Mbit/s against ACK controls of 123.838 and 123.095 Mbit/s in reversed arm
order. Removing only that logical-group fast path restored NoAck to 122.483
Mbit/s against a same-source ACK control of 120.740 Mbit/s (10/10 correct in
each arm). Keep established UDP NoAck, but **do not bypass the SendSequence
for logical groups**: a safe all-or-nothing direct admission still disrupted
the H3 carrier batching/pacing path. Partially winnowing a group is additionally
incompatible with its single admission/ownership contract. A deterministic
test now pins logical groups to sequence admission. These same-host simulator
records do not qualify physical devices or the iOS-profile memory gate.

## Decision gates and next work

1. Keep the global TUN and Transfer defaults at 8 seconds. The ACK-required 3s/3s fixture is valid on clean high-access-RTT but exposed extra inner-TCP retries and a one-loss no-send-ACK **health preemption**, not an ACK deadline failure. Reproduce the health verdict deterministically, measure the independent Transfer ACK terminal time with health intervention disabled only inside a test, distinguish dead exit from recoverable reliable-carrier TCP loss, and qualify a bounded health fix before further timer A/B promotion. Then compare 3s/3s against 8s/8s with 3s/8s and 8s/3s component controls, actual wire-policy/collapse assertions, and clean/loss/low-edge/high-access-RTT conditions. A 2s/2s pair and the 3s/6s and 2s/4s hypotheses follow, not as unmeasured defaults. The diagnostic opt-in TCP-NoAck policy seam must remain removed.
2. Report DoH and proxy decision weights separately. For DoH, faster answers may justify bounded extra retransmission/wire cost; for proxy/bulk, additional copies without useful delivery are more concerning. Neither consumer can waive correctness, bounded memory, egress continuity or the iOS-profile 24 MiB app gate. Do not call an explicit replacement a recovered original proxy stream.
3. Keep early DoH retry independent. Require idempotence, at most one replacement, old-owner retirement, caller-cancellation behavior, and cache isolation before production use. Do not add universal proxy stream replay.
4. Run deterministic directional policy/loss/reconnect tests and applicable full PERFVAR, DoH/proxy and device-memory gates before any retained default change. The original intermittent H1 ACK timeout and complete static arm remain open until a clean qualification.

Raw pilot records, source/trace guards, and exclusions: `/tmp/urnetwork-tcp-recovery.JXKEUL/` (`pilot-driver.log`, `STATUS.md`, per-cell JSONL and logs). Failure snapshot and controlled H1+ loss artifacts: `/tmp/urnetwork-perf-h1h3.O4RGsr/`. These local paths are temporary research artifacts, not a durable baseline store.

Corrected low-edge and five-seed screen: `/tmp/urnetwork-tcp-recovery-repeat.T5mXd0/` (`paired-summary.csv`, `paired-summary.json`, `screening-statistics.json`, `source.sha256`, per-cell JSONL). These paths are also temporary and must not become the authoritative baseline.

Independent confirmation: `/tmp/urnetwork-tcp-recovery-confirm.nDk5TA/` (`PLAN.md`, `confirmation-summary.json`, original and repaired protocol manifests, `all-cell-artifacts.sha256`, retained original 45 artifacts and per-cell JSONL). This remains diagnostic evidence, not a canonical PERFVAR baseline.
