# Packet-flow regression review

Current update: the resident and exchange pre-enqueue refusal corrections are
integrated after deterministic counterfactuals and private normal/race proofs.
The P3 receipt/ACK-custody correction is also integrated after scoped current-source
correctness and local cost review. The portable fixture correction and default
Connect root normal/race passes are qualified; canonical recursive/full-runner
and device qualification remain pending.
The composed on-disk confirmation stopped at a native-write fixture boundary
error; it is not qualified. See the dated updates at the end. The initial
status and source findings below describe the earlier frozen review.

Status: sealed four-week review and scoped current-pressure audit, prepared privately for source-safe integration. All 2,274 inventoried commits have a per-SHA disposition; the finite production-relevance review has no pending assigned diff. Review depth varies explicitly below: generated equivalence, actual activation boundaries, ancestry merges and test/docs classification are not represented as manual audits of every line. Sol's deterministic OLD-source pacing executions reproduced three semantic failures with two controls passing in normal and race builds. That is the strongest current evidence, not a device incident or performance-fix result. No production change, qualified fix or remote deployment attribution is established here.

The current source contains several mechanisms capable of turning local pressure or delayed recovery into wider connection pauses. The strongest initial candidates are a selected H1 repair trapped behind its own pacing drain; complete-message queue overflow retiring unrelated server work; and receive-sequence cancellation after a bounded receipt or decode admission fails. There is also a distinct, intentional DNS policy change that can expose tunnel resolver latency. These mechanisms are separate from the previously reported H1 loaded-probe 0/64 outcome. None has yet been shown to cause the reported iOS website pauses.

## Evidence boundary

The host clock at review start was **2026-10-16T21:19:23Z**. The exact four-week interval is **2026-09-18T21:19:23Z through 2026-10-16T21:19:23Z**, inclusive, selected by committer timestamp. Author and committer timestamps, authors, parents, paths, and ancestry are retained in the inventories. The first two Connect partitions use exclusive upper bounds so the three partitions have no overlap.

| Scope | Frozen revision | Locally visible commits in window | In frozen HEAD ancestry |
| --- | --- | ---: | ---: |
| Connect | `620d2821660f8e7795f6c5d61ea74ac219a3ceed` | 997 | 865 |
| SDK | `c72729efe1bb6dff5e1aa557a035fda3a116b304` | 675 | 547 |
| Server, `connect/` path | `ec3dbdec6205fb3f946514837854f18b101794fb` | 602 | 518 |

Server counts use `--full-history` and include 255 ancestry-only merges with no first-parent `connect/` change. They are not equivalent to the smaller path-simplified `git log -- connect` count. `server/connect` is a directory in the Server repository, not an independent Git repository. Connect and SDK were clean at inspection; Server contained unrelated pre-existing payment-file changes outside this scope.

The census covers locally visible refs and linked-worktree HEAD ancestry. Some side-only objects are retained through linked worktree metadata rather than a current named branch. No fetch or pull occurred. Remote-tracking refs may be stale, and no local commit is evidence of what is deployed on the user's device, selected provider, or hosted proxy. Frozen main and later side candidates are distinguished throughout. In particular, Connect `69b04d6b` and SDK `cfbc384d` are unfinished legacy checkpoint branches, not qualified fixes or safe wholesale merges.

The reviewer read `connect/CODESTYLE.md` completely; no applicable `AGENTS.md` was found in the source trees or their ancestors. Research uses source and Git reads only. Authored artifacts stay in the private evidence directory. Builds, tests, device work, and runtime changes belong to the sole Sol executor. Any executor results added later must identify the exact overlay, source, selector, result and artifact digest. Reading a test or a side-branch commit is not a test result.

The user's symptom is a significant regression over the past two weeks, with reportedly 5–15-second website connection pauses on latest iOS main over WiFi. Whether the provider is hosted, local, or remote remains unresolved. WiFi does not identify the actual H1/H1+/H3/P2P lane.

## Full census and review coverage

All artifact paths below are relative to the private evidence root:

`/Volumes/data/urnetwork-h1-chronology.Y0OYTR/packetflow-regression-review/astra-review.AmbGZsAp/`

| Inventory | Coverage |
| --- | --- |
| `connect-early/inventory.json`, `connect-early/coverage-ledger.json` | All 254 identities and 83,317 path entries verified against complete Git output. The original 24 release manifests omitted 1,624 transitions; all were repaired and all 254 byte-hash comparisons now match. The 109 packet/control scopes close through 92 direct production diffs, three comment-only deltas, 12 exact equivalences, one identical move and one effective vendor baseline. The supplemental 23 messaging and 15 storage scopes, 24 release transformations and three installers are closed. All 15 actual merges and 26 generated updates have explicit combined/structural dispositions; 24 test-only and 18 docs/API changes are classified, not full test-quality audits. Categories can overlap. |
| `connect-middle/coverage-final.json` | All 426 entries and exact path bytes verified, including 12 large releases. The 157 broad executable/schema scopes comprise 82 full production deltas, 35 scoped/schema reviews, ten harness/comment/CI/docs boundaries, 17 actual historical activation-boundary exclusions and 13 generated structural reviews. All 158 merge combined paths and 12 normalized releases are closed; 99 other ordinary changes have explicit dispositions. The 17 excluded offline/platform/deleted interiors were excluded through actual constructor/import/init reachability, not filenames alone. |
| `connect-late/inventory.json`, `connect-late/coverage-final.json` | All 317 entries and 86,572 path rows verified: 205 ordinary non-merges, 69 merges, 21 normalized releases and 22 generated security updates. All 136 production-bearing ordinary commits have complete scoped production-path coverage; 69 ordinary nonproduction commits are classified. All 69 merges close as 47 nonoverlap, 18 overlapping without dense hunks and four dense patches read. The final union reconciles the Transport, Transfer, NAT/group and parent ledgers; their overlapping counts are not summed as distinct commits. Zero missing production paths. |
| `sdk-history/commit-inventory.jsonl`, `sdk-history/final-coverage.jsonl` | All 675 identities and complete path bytes verified, with zero missing/duplicate SHAs. All 340 ordinary semantic scopes close: 67 focused plus 273 broader (40 initial, 29 auth/control, 53 bindings/build, 55 messaging, 96 product). All 155 merges have dispositions: 123 empty combined, 18 test/docs and 14 targeted semantic. All 112 releases have 33,490 content comparisons and no unexplained executable residual; 68 test/docs/CI/static-payload changes are classified. Two retired messaging interiors retain explicit crypto/ABI implementation exclusions after full integration/current-absence checks. Dependency provenance and binary qualification remain separate limitations. |
| `server-history/inventory.json`, `server-history/commit-review-ledger.json` | All 602 entries close for serving applicability: 106 ordinary serving diffs fully read; 74 serving merges close 206 serving-file instances through 187 exact reviewed ordinary blobs and 19 reviewed combinations; 255 ancestry-only and 35 auxiliary merges are classified. All 364 nondense combined merges were checked. All 51 releases/1,205 serving-file instances are namespace-normalized; the 49 test and 32 harness/docs entries are path/build-boundary classified, not full test-quality audits. |

The five final coverage ledgers have SHA256s, respectively: `53f2617d749cf33b2771d79c85cfafd2dafbf00c6766cdc745ed39a714ce33a8`; `26ff44f84269f0a9cdf0d9b733adc46adcb21440e079c8d4d0592a086ea5470a`; `6d09d86abdff437498bef68b3cb30e6e775f198f5746d27f115bf611ea8ab994`; `53d2b8186fb4f1c4db73b6c5835da9198213b4049c0897f9a48f3a03c2202ad6`; and `ee3db839d40b063c662aaf20ede2df7808919325695fb9b566afce093fdfaba3`. `FINAL-REVIEW-MANIFEST.md` pins the report, inventories, supporting closures and integration payload.

Each inventory retains exact SHA, author/time, parents, paths, ancestry and a disposition. A generated copy, documentation change or ancestry merge is not counted as an independently read production implementation. There are no pending finite relevance assignments, but this is not a claim that every test, generated body, retired cryptographic implementation or possible multi-commit interaction was manually audited. SDK release provenance retains 64 gVisor local/pin uncertainties; SCTP/DTLS binary behavior was not qualified. No remote history refresh, deployed-version attestation, new runtime execution by this review subtree or comprehensive platform audit occurred. Independent coordinator notes in `ROOT-REVIEW-NOTES.md` corroborate current call chains but do not add another exhaustive history review.

## Prioritized current mechanisms

Severity ranks the conditional code impact; it does not estimate incident probability. “Source-confirmed” means the relevant state transition exists in the frozen code. It does not mean a phone reproduction or deployed incident has been observed.

### P1 — selected repair waits behind a drain that needs that repair

**High; source-confirmed conditional delay reproduced by deterministic OLD-source normal/race tests.** Current `connect/transfer_window_pacing.go:494` asks the nested recovery selector for an earlier deadline. `transfer_recovery_selection.go:126` records an eligible older retry in `pendingRecovery` but returns a zero deadline; an already selected retry also returns zero at line 65. The younger original remains inside its controlled pacing drain. The outer send worker at `transfer.go:9155` owns actual repair dispatch and cannot dispatch until the younger wait unwinds. A drain requiring progress from the selected missing tail can therefore postpone its own repair.

Prerequisites are a concrete selected H1 route, delivery-sized pacing, an outstanding tail, enough recent residence to enter a controlled drain, an eligible due older retry, and a later original occupying the pacing owner. This does not directly apply to an unpaced or native-datagram path. The original retains one paid pacing/FIFO reservation; a correction must preserve that debt and ownership rather than recursively dispatching through the occupied waiter.

`transfer_window_receiver_rtt.go:47` takes the drain maximum from ACK timeout: 60 seconds at default `transfer.go:892`. **The initial pacing wait is not covered by the default 15-second physical writer timer.** Both plaintext (`transfer.go:12958`) and encrypted (`12996`) branches call `paceWrite` before entering `writeWithLifetime`, where that timer is first created (`12759`). The remaining writer/caller budget is applied only to a newly required policy-revalidation pacing wait inside that function (`12784`). The initial wait instead ends at the earliest applicable drain deadline, retained ACK lifetime, cancellation, covering cumulative ACK or relevant route change. Later writer-phase waits additionally have the remaining physical-write budget. This corrects an earlier draft/coordinator-note overstatement of a universal15-second bound. Neither an unconditional60-second pause nor the user's5–15-second incident duration is established by these constants.

`c916751e227a53d396ccc3b0a83832a1382afd3b` (2026-10-10T10:53:14Z) introduced the incomplete nested-selection implementation. The pre-c916 pacing wait also lacked nested repair dispatch: the broader delay must not be claimed to have first appeared on October 10 without a pre-c916 counterfactual. Side `f4881058dff49f791281789a8ed000bad3ae50d0` adds a narrow ten-line dependent-drain abort, absent frozen HEAD and not qualified.

The corrected source analysis is `late-transfer/packetflow-pacing-stall.md`, SHA256 `4824c0de53545c37a2896628fb38eda5a416aaf1ada94457ebd3490062fe7ca3`. `late-transfer/overlay-tests/manifest.md` describes the test-only overlay against frozen HEAD. It adds five `TestWindowPacingOwnerDrain...` cases and adjusts only an existing fixture assertion so the intended ordering reaches production code. `overlay.json` SHA256 is `3706d8629870762c0fde1628fd906a816ccc108f1dd73bda68dcdee80cf01d50`.

Sol executed that unchanged OLD production source once normal and once with `-race`, selector `^TestWindowPacingOwnerDrain(ReleasesSelectedRecovery|PreviouslySelectedRecovery|AckPreservesProbe|SelectedAckPreemptsCopy|CancellationKeepsDebt)$`. Both runs compiled and reached all five roots, with **three FAIL, two PASS, zero SKIP**. `ReleasesSelectedRecovery` and `SelectedAckPreemptsCopy` reported a selected repair trapped for **39.512 seconds of fake-clock time**, with zero fresh physical writes and zero copies. `PreviouslySelectedRecovery` did not reach the expected fresh-write barrier. `AckPreservesProbe` and `CancellationKeepsDebt` passed. Package elapsed times were 0.644 seconds normal and 0.784 seconds race; no data-race report or stderr output was reported. These are constructed-state semantic failures, not wall-clock traffic latency, phone results or proof of a new October onset. Raw evidence lives in sibling directory `../pacing-oldhead-exec.ZEVVuT/`: `normal.go.json` SHA256 `1357dfa4d0bfcfff45fc1bcd8588bc415db72f1978a352a7108b0487a56a07ff`; `race.go.json` SHA256 `522300b6d1a1fef18029aa199c8401b7adfda2153cc0d35ddb9e7952623c5f5d`.

The ten-line candidate is **not a qualified fix**. Its exact current-source full file is `late-transfer/overlay-tests/pending-candidate/transfer_window_pacing.go`, SHA256 `853c296a2a8e5b268bc6a78ec51b1a0d1326c01f03ed3d0687074cb98ddeb6d6`, Git blob `b1d90604d855b68a496b907e1ddd7bb91a856285`; only one +10/-0 hunk differs from current blob `c388d8b49199df5c374adaac155e25c9978fbfa6`. The guard verifies a pending same-sequence H1 tail but not that the selected retry lies at or below that recorded tail. A mixed H1/non-H1/H1 history can leave an older H1 tail pending while the selected repair is newer and irrelevant to that tail's drain. Feedback between selection validation and the guard can also discard a viable measurement, though outer dispatch still revalidates the retry. Required counterfactual controls cover actual mixed-carrier history, sibling-only drains, earlier-hole/later-tail dependency, ACK timing, unchanged debt/cooldown/FIFO and cancellation. The interaction review is `pending-candidate/interaction-review.md` (within the same overlay directory), SHA256 `55eee3489ce74644a56ea64791d6452f6173c0a4708947a10e86f4318f19128c`. Candidate execution is not implied by the OLD result.

The separate eight-root adjacency packet is now sealed at `late-transfer/adjacency-tests/manifest.md`: five new tests plus three existing controls. Test source SHA256 is `fb0759b50bb607d15576d395a16e52685210234e07264f7eb490c81beb8ba35f`; test-only overlay SHA256 is `48eda7013a012b2d01e296cc622bdd87f358151b4d6687e6bf6953d7e99b8450`. It constructs real Client routes, retained owners and ACK admission without editing the tail map. The mixed-history selective feedback is protocol construction, not a paired receiver observation. Its200-millisecond constructor test documents initial pacing preceding physical WriteTimeout; it does not redefine enqueue/acceptance timeout as an end-to-end promise. The packet is **uncompiled and unexecuted**; ACK arrival in the narrower validation-to-service-lock interval remains a coverage gap. `PARENT-STATIC-REVIEW.md` records the independent source review and limitations. The original OLD overlay is unchanged.

A dependency-specific source-only design is sealed at `late-transfer/pacing-remediation-design.md` (SHA256 `84e4cd52d6d355d2b207fb9a83bbe93783e5fee63b25b1fb0528b930190fcaa3`). It derives the tracked tail's number by exact identity lookup in the same sequence's existing retained owner, requiring `selectedRepair.number <= tail.number < currentOriginal.number` plus unchanged service/drain generation and pending physical eligibility. It revalidates feedback before the decision, changes only drain epoch/deadline/wake, preserves FIFO/debt/cooldown and revalidates again before actual dispatch. No ULID ordering, extra queue or unbounded scan is proposed. The suggested broad ACK-mutex linearization is not accepted: ACK publication holds that mutex across nested buffer/queue/service locks and diagnostic ProgressObserver calls (`transfer.go:7681`, `7787`, `7856`; `transfer_progress_trace.go:114`). Those diagnostic callbacks have a bounded/nonblocking contract and compile out of ordinary builds, so this is not a newly proven default-build callback deadlock. Nevertheless, a short new critical-section body cannot establish bounded lock acquisition or no reentrancy. A narrower ACK-window decision still needs a wake protocol: merely failing TryLock and returning could miss an unrelated holder's unlock and trap the repair again. This is **not an implemented or qualified replacement candidate**. Adding numeric tail metadata is another bounded alternative with a map-memory accounting obligation, not assumed free or unacceptable. Separately authorized private fix research does not change this report's frozen-source findings or qualify a candidate.

The separate follow-on receipt is `late-transfer/pacing-followon.ipEVtIcB/lock-proof-and-test-proposal.md`, SHA256 `dcd0fb7f0002163505ebf29ac5ef1bc20adb5df26241353ab7b356f5f2b8f349`. Its additional test-only overlay (`901d36064753ecbd727085021dde3b0715fab5cdd7c68e3df448908438525149`) adds ACK-after-decision and cancellation/debt controls through the existing physical barrier. It is uncompiled/unrun, supplies no production candidate and does not cover the narrower preliminary-validation-to-commit interval. Numeric-tail metadata accounting and the precise ACK decision protocol remain separate remediation research; the historical review does not wait for them.

### P2 — one complete-frame overflow retires a server resident

**High; source-confirmed cross-flow amplification, introduced before this window.** At frozen Server `connect/resident.go:4360`, `handleClientForward` makes a zero-wait offer of a complete serialized Transfer message to a destination shard. When full it returns the extra pooled share, counts refusal and calls `self.cancel()`. Resident cancellation drains every ingress shard (`4033`, `4906` and `resident_payload_owners.go:172`), cancels destination forwards and reverse attachments, and forces reattachment for unrelated traffic. `receive_queue_policy_test.go:335` explicitly expects this teardown.

This is not a truncated byte-stream fragment. The message was already framed. Dropping that complete message cannot corrupt the next frame. Connect's forwarding branch (`transfer.go:4888`) bypasses endpoint `ReceiveSequence` handling and emits no new end-to-end Transfer ACK at the relay. The originating reliable sender remains the retransmission owner. Physical TCP/QUIC acknowledgements only establish carrier delivery and cannot undo an application discard. No-ACK traffic needs its own loss accounting; it does not justify retiring unrelated sources.

The user explicitly requires zero-wait refusal/counting while keeping unrelated work alive. The old rationale at `resident.go:4380` and its test confuse complete-message refusal with raw stream truncation. A source-confirmed amplifier is present, but its introducing commit `34b953a620a783d480d6d1ece35b46ebe904800b` is August 25, outside the review window. Side `a7921567267a464ae12646c71f60065cf9ecf705` demonstrates a narrow refusal counterfactual; it has not been qualified and is not permission to merge a branch.

A recent possible exposure is per-offer contract-hole authorization. `3a13e513ac95fa644fe074f98453e14b5d899e18` (October6) moved authorization ahead of live-forward reuse; `ec8a66b08bf6bd728443f5ee49479a9d8e1293c2` made packet policy Redis-only. A cold/expired lease performs a synchronous bounded read on the shard worker (`resident.go:4430`, `resident_contract_manager.go:74`, `model/subscription_contract_hole.go:25`, `104`). The positive cache lasts at most5 seconds; half-window refresh is asynchronous, same-pair concurrent checks refuse instead of building waiters, and no PostgreSQL packet fallback remains. A one-second cold read can delay other destinations hashing to that shard and increase overflow probability. This composition is plausible, not measured; do not bypass authorization to reduce latency. The configured reconnect interval is one second with full-jitter remaining-attempt semantics, not a mandatory one-second sleep. Transfer resend defaults are2 seconds cold,300 milliseconds warm, capped at8 seconds. Those constants do not by themselves predict the observed pause duration.

Deterministic discriminator: hold one destination worker at a lease-lookup barrier, fill its exact shard, then offer one more complete retriable frame while another destination is active. Assert immediate refusal, one pool return, one refusal count, resident and unrelated flow remain live, and the original sender can retry the exact refused sequence. Add a same-shard healthy destination control and a no-ACK loss-accounting control.

### P3 — pressure cancels a receive sequence after advancement

**High; receipt-admission correction integrated after scoped correctness and local cost review; full qualification remains pending.** In the frozen pre-fix source, `transfer_receive_delivery.go:86` can refuse a delivery receipt on count, retained-byte or shared-budget capacity. `transfer_receive_delivery_dispatch.go:111` cancels both the delivery queue and receive sequence. The next sequence number has already advanced (`transfer.go:15963`). Simply removing cancellation could cause a retransmission to be treated as an old duplicate and acknowledged without delivery. Admission, sequence advancement, receipt custody and selective/cumulative ACK state must be treated as one transition.

A second site is `ip_provider_reliable_ingress.go:54`: transient decode workspace exhaustion, including its busy fixed 32 KiB fallback, reaches a deferred `claim.complete(false)` and cancellation in `transfer_receive_delivery.go:181`. Its unit is a complete **ACK-backed** Transfer Pack, not necessarily a Pack whose cumulative delivery ACK has escaped, and not a raw fragment. The blast radius is every flow mapped to that source/session/logical receive lane, not the entire Client. Side candidates `0fd6b1ba` and `1c6b36a9` address these respective boundaries; no runtime result is claimed.

The exact ownership audit is in `late-transfer/pressure-matrix.md`. `prepareReceiveDeliveryCredit` runs before the ledger's closed/count checks, but is idempotent metadata/charge preparation, not itself a new root allocation or reservation. Actual budget acquisition follows capacity checks; returning the item must release any earlier reservation once. Secured later receipts can issue SACK; only the contiguous secured prefix may issue cumulative ACK. After an append refusal, the already-advanced head has **no ledger identity**. A duplicate below `nextSequenceNumber` is not redispatched, and `sendDeliveredDuplicateAck` sends a normal cumulative ACK for that unknown identity. Thus merely removing cancellation would incorrectly acknowledge undelivered data. Move admission before head/contract advancement, or retain an exact bounded head owner without ACK, with subscribed capacity edge and duplicate identity; preserve or explicitly withdraw any prior selective promise. Decode contention similarly needs a deferred frame/claim operation, not false-success completion. Existing downstream NAT/final-TCP-capacity handling already retains and retries a claim without closing the sequence.

Current pressure cancellation does not establish the auth rejection tombstone: after cleanup, an originating retained retry can reform the sequence. Lost SACK-held successors can nevertheless remain deferred for the default60-second selective lease. This is prolonged recovery and wider teardown, not generic permanent framing corruption. Current gap-expiry cleanup can similarly clear SACKed hold without withdrawal; side `1992860c`/`170756ba` address that separate promise lifecycle, not frozen main.

The regression must exercise the real receive/dispatch entry, force capacity refusal before consumer acceptance, and prove retry of the same sequence delivers once with no ACK of undelivered data. Include an immediately following unrelated flow on the same lane, cancellation/pool reconciliation, selective ACK pressure, and ledger rollover. A helper-only queue test is insufficient.

### P4 — H1 synthetic speed mode intercepts real payloads and ACKs

**High conditional provider impact; source-confirmed, old origin.** Connect `transport.go:2887` starts speed mode; while active, lines 2924–2929 echo every incoming message larger than 16 bytes and continue before ordinary receive forwarding. Server `connect/transport.go:2044` intentionally interleaves ordinary resident messages between speed probes. Those real Transfer payloads and ACKs therefore do not reach their intended receiver during the test.

The reflected remote-source message is rejected by the server's source-identity fence (`resident.go:4328` and control fence at `4579`), so the usual outcome is loss/delayed retry, not accepted impersonation or receiver sequence advancement. The blanket echo originated in `7e745edd` in 2025. Recent `671a44c7` changes a full echo queue to a wait; `79cb9de6` changes retained H1 ownership. Their effect on exposure remains unmeasured.

Eligibility matters: current server enables synthetic tests for TransportVersion >= 2, uses two 512 KiB tests, and ordinarily starts them for a public provider after 60 seconds with passive rate below 4 MiB/s (`transport_announce.go:119`, `215`, `462`, `747`, `973`). Successful completion suppresses ordinary repeats; failures can be retried on eligible syncs. The periodic 12–24-hour path is separately gated and must not be assumed identical. Ordinary consumer-only iOS startup is not established as eligible; the selected public provider can still be affected.

Current `TestH1SpeedTestEchoBackpressureKeepsGenerationAndFrames` sends ordinary traffic only after speed stop, so it misses the overlap. Side `0161dfc7` recognizes tagged 1024-byte probes; a valid protobuf Transfer cannot begin with its zero tag marker, but legacy servers before tagged probes used entirely random probe bytes. A fix needs explicit compatibility tests or negotiation, not an unconditional new-marker assumption.

Discriminator: on the real H1 reader/writer pair, start speed mode and interleave a data Pack, an ACK and a tagged probe before stop. Each real frame must reach receive exactly once; only the probe should echo. Test legacy random probes, marker-shaped invalid input, full echo queue, cancellation and ownership.

### P5 — QUIC root pressure closes the complete carrier

**High conditional impact; source-confirmed guard path, pre-window origin.** `net_extender_quic_send.go:27` permits 64 writer roots. A 96-root hard limit at lines 487–498 invokes cancellation of registered writers and closes the entire QUIC connection (`131`). A loss/retransmission prefix split adds a root outside writer admission (`519`); 32 such splits on a 64-root flight can reach the hard limit. The wrapper is used by mobile H3, outer QUIC extenders and alternative H3 API traffic.

These roots are raw QUIC STREAM fragments with quic-go ACK/loss ownership. Silently discarding a tracked fragment is not the same as refusing a complete Transfer message before handoff. The correction should prevent over-admission or reserve fragmentation headroom, not suppress a guard while breaking ownership. Origin `56232afd` is September 17, before this window. Side `4191dc693107536e88004453ccbc021763ff4552` lowers writer admission to 47 and tests one split per root, but repeated fragmentation needs its own bound and qualification.

### P6 — bounded provider source pressure rotates all NAT flows

**High conditional blast radius; source-confirmed, pre-window SDK origin.** A memory-budgeted provider clamps source admission to 16 (`ip_provider_memory.go:21`). If that bounded owner reaches source capacity, `ip.go:7987` calls `SourceLifecycleSaturated`. SDK `device_local.go:6072` detaches and closes the whole RemoteUserNatProvider and LocalUserNat before rebuilding, affecting other healthy sources' flows.

The callback is conditional on a nonnil provider memory budget; this is not a claim about every provider or every 17th source. SDK `device_local.go:1436`, `1544`, `5940`, `5960` reaches this policy only for actual mobile or explicit LowMemoryProfile devices with providing enabled. A desktop's positive memory target alone is insufficient; hosted devices with AllowProvider=false cannot hit this locally. Connect inherits the exact NAT budget (`ip.go:7772`) and clamps MaxSourceCount at `7810`. Server serving code constructs Exchange/ConnectHandler, not LocalUserNat/DeviceLocal, so no in-scope default establishes this on public remote providers. Their deployed binary/profile remains unproven. The consumer iOS app with providing disabled needs that remote-provider chain, not a local assumption. SDK origin `c74d6e2b80f986e955ca6bc7a2370c0a8aaeb819` is September 8. A deterministic test should hold 16 healthy source owners, offer a 17th, and compare bounded refusal with continuity of the original flows; retain legitimate terminal authorization/tombstone behavior.

### P7 — signed expiry can reject a retained packet's first delivery

**High conditional reliability risk; source-backed hypothesis pending root reproduction.** Oct 6 `f1c7ff2facec4c13e9044efd7a542d98dc86d4cb` introduces signed absolute contract expiry. Current receiver rejects a Pack whose first delivery is after that deadline (`transfer.go:16580`, `16815`) and exits its sequence at `15671`. Sender retry/setHead retains the item's old contract (`10972`); contract-missing handling restores the same proof rather than obtaining fresh authority for that retained item. Existing lifecycle coverage loses only an ACK after payload was delivered before expiry, so it does not cover first delivery delayed across expiry.

Hold one admitted data Pack before the receiver until the signed deadline, then restore a healthy route and release it. Observe whether the same bytes can be delivered or a bounded explicit failure occurs, and whether successor traffic is pinned behind the old owner. Test a paired lost-ACK-after-delivery control. ACK lifetime defaults are 60 seconds, unreliable lifetime 90 seconds and retained silence 240 seconds, so this is not established as a 5–15-second explanation. Authorization deadlines must remain enforced; silently accepting expired authority is not a remedy.

### P8 — intentional DNS and retry policies expose longer connection waits

`fc49dc967e3121c7ed30d144df59f4ed13bdce99` removes default host-network DNS fallback from the tunnel resolver. `ip_mux_upgrade.go:122` now defaults to tunnel DoH only with a 60-second resolution timeout; previous fallback raced after 250 milliseconds cold or one second warm. Current comments record a historical 5.29-second stale tunnel resolver tail, not a measurement from this review. This privacy change is a duration-compatible performance exposure, not automatically a logic defect. Capture DNS start/completion and effective mux settings. Do not restore host DNS without the user's explicit privacy choice.

An adjacent pre-existing amplifier needs a saturated-cold-query control: `net_http_doh.go:1741` bypasses the hedge reserve for cold queries before global slot acquisition at `1755`; a burst of stalled primaries can occupy all8–12 mux slots and leave an otherwise healthy hedge queued. The reserve originated in September11 `588ad5b9`, before the window. Warm-query reserve and quiet-first racing are important counterevidence; existing tests cover those but not this exact cold-burst barrier. Any remedy must remain tunnel-only and preserve bounded capacity, rather than quietly restoring host DNS.

Oct 12 `c53e949947e167a3bced341a0eb8bd57b9b16d15` deliberately paces failed fixed-destination re-dials with a 7.5–15-second first jittered wait and exponential cap of 2.5–5 minutes; reliability refusal goes to the maximum. Ordinary dependency errors have a jittered backoff capped at 15 seconds. Working admitted fixed destinations clear the state; this change is relevant after setup failure, not proof of packet-path deadlock. Identity reuse and key reuse (`8594d784`, `6725a75c`) reduce repeated registration work. New evidence must distinguish an explicit retry policy from an unexpected queue stall.

Transport retry pacing also composes with an explicitly edge-triggered Kick. `3336ddc7bade32522ce58277724d03c4ab7d7599` adds failure backoff; a single Kick during the older H1/H3 dial (`transport.go:2293`, `3240`) can occur before the next wait subscribes (`2335`, `3292`) and does not necessarily interrupt that subsequent wait. Later jitter ranges grow from 2.5–5 to 5–10 and 10–20 seconds, capped at 30–60 seconds; first-failure grace, observed Kick and success reset matter. The API documents that in-flight dials are unaffected, and the existing Kick test repeatedly signals every 250 milliseconds. A single-Kick-at-held-failure test is needed to decide whether stateful wake semantics are desired. This is an explicit policy/edge exposure, not permission to remove outage pacing.

### P9 — NAT producer/idle-consumer lock cycle

**High conditional pause; source-confirmed lock dependency, pre-existing.** TCP `ip.go:5189` holds `sendMutex` across a full-queue wait, while the sole queue consumer's selected idle branch at `6697` waits for that same mutex before it can resume consuming. An established flow can therefore wait for its producer deadline rather than queue progress. Default TCP send timeout is 60 seconds; a negative caller timeout has no deadline. Legacy UDP (`3426`, `3844`) and ICMP (`ip_icmp.go:653`, `852`) have the same shape with 15-second defaults. The modern shared-UDP path bypasses the legacy UDP worker; reliable provider ingress uses zero-timeout admission, so route/caller applicability matters.

The analogous Transfer worker cycle was fixed by `fbace7618` on September 22, but these NAT siblings remain. NAT origin `d2bd1683` is in 2025; ICMP origin `e05ecee0` is August 2. `connect-early/idle-lock-test-design.md` pins actual constructor/branch barriers, exact queue capacity, finite/negative/zero timeout controls, unrelated-flow progress, and cancellation/pool joins. This review has not executed that reproduction. Increasing queue depth can defer the full condition without removing the cycle.

### P10 — failed NAT replacement admission has already canceled a live victim

**High conditional continuity defect; source-confirmed ordering, pre-window origin.** TCP `ip.go:4615` and `4643`, UDP `2966` and ICMP `ip_icmp.go:356` apply source/global LRU eviction before constructing the replacement. The replacement constructor then requests memory and can return nil (`ip.go:4657`–`4675`). The existing victim is already removed/canceled although no replacement is admitted. Its deferred memory cleanup requires the same buffer lock held across this decision, so its credit cannot simply fund that synchronous replacement. UserLimit confines victims to a source; GlobalLimit can affect sibling sources.

The fallible memory admission arrived in `56232afd` before this review window; LRU limits themselves are intentional. The discriminator must use the actual constructor, fill the configured flow cap with healthy connections, exhaust replacement memory, and offer a new SYN/datagram. A rejected newcomer must not evict the established victim; successful replacement, explicit close and security revocation remain controls. The remedy needs transactional/reserved replacement admission, not blanket removal of connection limits.

A private unexecuted TCP test packet is sealed in `connect-early/test-design/`: `astra_nat_flow_refusal_regression_test.go` SHA256 `5024967dded3c43f6f1d3cee654a2b5d23038cf3def18d418975691a15d56f92`; `overlay.json` SHA256 `278444000d9187481c3b70d216fb641fc89d3b7afa7a9165fa6ff788eefce46b`. It uses actual IPv4/IPv6 constructors and initiating SYN admission, with three expected baseline semantic failures and one sufficient-capacity replacement control. Its per-instance barrier retains the real generation **before Run**, so it does not yet prove established-socket continuity. Source/API and pooled-copy ownership were read; compilation, overlay discovery and execution remain unverified. Established TCP, UDP/ICMP, same-key generation replacement and terminal-source controls remain required before any fix qualification.

### P11 — transient complete-datagram refusal can close a wider carrier/listener

**High conditional blast radius; error-classification defect candidate, runtime frequency unproven.** The DNS packet translator's worker exits on any underlying UDP `WriteTo` error (`transport_pt.go:358`, `536`, with forwarding/pump siblings at `428` and `571`). `encodeDns` defers cancellation at `297`; its AfterFunc at `245` invokes `close`, which closes the underlying PacketConn at `1154` before joining workers and releasing queued roots. On the extender DNS-server path, one translator wraps the shared UDP53 PacketConn (`extender/extender.go:1194`) and the QUIC listener consumes it (`1237`). Thus a zero-progress transient local pressure error such as a wrapped ENOBUFS may retire the entire listener and every client, not just the unsent complete datagram. This is a pre-existing 2025 path, not an identified new four-week regression.

Two analogous sites broaden the current audit: the asynchronous P2P batch writer (`transport_p2p_udp_batch.go:243`) closes the ICE socket on any write error, affecting its STUN/DTLS/SCTP users as well as complete SRTP/RTCP datagrams; and `extender/extender_datagram.go:90` unwinds an inner-QUIC datagram relay on a connected-UDP error after a complete outer frame. Typed transient zero-progress refusal, partial batch progress, timeout/cancellation and fatal socket closure must be tested separately. A complete datagram can be lost without corrupting framing, but already accepted datagrams must not be duplicated, and a truncated reliable outer frame is not safely skippable. Do not suppress every error or assume each OS returns the same errno. Use an actual constructor with an injected PacketConn that emits a bounded, typed error and then succeeds, and require unaffected peers to remain live with exact ownership/counters. Fatal errors and malformed-framing closes must still terminate.

### P12 — local admission failures can become remote liveness convictions

**High conditional cross-flow impact; source-confirmed classification gap.** `busyLivenessProbe` at `ip_remote_multi_client.go:15827` treats any ACK callback error as conviction, including `errSendPackExpiredUnwritten`, which certifies that the probe never reached the wire. Ordinary packet and cping paths at `16709` and `17835` explicitly exclude that marker. Once existing send-stall, active-carrier, healthy-sibling and quarantine/shared-fate gates also hold, `convictSendStalls` at `11354` cancels the admitted channel and its pinned flows. The two immediate queue-refusal paths at `15781` are separately documented policy choices, not this hidden error-marker inconsistency.

Initial evaluation also needs local-refusal/cancellation attribution: it can reject a candidate rather than an established channel. Side-only `f20ab057` captures cancellation at callback delivery before the pass lock; it is a counterfactual, not a current fix. Recent fixed-destination backoff in P8 can amplify any false failed-attempt classification. The deterministic test should use a real send sequence whose admitted probe expires unwritten, maintain every other conviction prerequisite, and verify no remote conviction or pinned-flow cancellation; real remote ACK failure and explicit synchronous-refusal policy are separate controls. Frequency and device relevance remain unmeasured.

### P13 — retired race candidates can retain already-terminal metadata charges

**Medium conditional admission-pressure defect; source-confirmed cleanup gap, unexecuted.** `c916751e` introduced retained collapse-promotion receipts; October 13 `4320b90d995a5cd8a770454a726a29cde36e22cc` added withdrawal of a failed candidate. A candidate receipt may already be both Terminal and Returned while its unanswered race remains live. `ip_remote_multi_client_group_completion.go:799` clears payload/source roots but retains the linked metadata and its charge for potential winner promotion. A later provider-signaled failure deletes that candidate from `race.clientStates` (`ip_remote_multi_client.go:10336`) without unlinking its now-ineligible receipt while another candidate remains. No completion callback remains to correct that order.

Final-candidate withdrawal calls `clearRaceWithLock`, which unlinks terminal receipts into `sequenceReleasedClaims`; however, `clientDialFailure` (`8887`) has no post-lock `releaseCollapseClaims`. The ICMP-only callback path (`18747`) bypasses ordinary packet-resolution cleanup. The send-path silence inference has an outer release defer and is an important different control. Later send/receive, explicit Close or the flow reaper can reclaim the charge; this is **stale metadata retention, not a permanent leak or retained raw payload leak**. The TCP idle policy defaults to 600 seconds, but no particular retention duration or memory-induced phone pause has been measured.

Regression design: use two real native candidate sends and actual Transfer ACKs, let both public returns and terminal callbacks finish, then deliver exact provider-failure signals first for A and then B with no ordinary packet interleaved. Require A's ineligible claim to detach and its charge to refund outside the leaf/parent locks without changing B's eligibility; require final withdrawal to drain detached charges immediately. Compare signal-before-terminal, signal-before-return, ordinary send-path inference, winner promotion and lifecycle Close, with exact pool/root/counter reconciliation. Current rerace tests use `sendGroupForTest` before real completion/retention preparation and do not cover this receipt lifetime.

### P14 — scalar batching can bypass a native group's capacity provenance

**High conditional composition risk; source-confirmed alternate consumer, unexecuted regression.** `c916751e` added native group range/typed-capacity obligations while the older scalar ready-drain compatibility at `transfer.go:9403` still permits a logical-group follower. The precise prerequisites are a scalar selected first, a wholly raw small group next in that same actual send sequence, matching ACK/unwrapped settings, combined size limits and safe contract state. Flow-isolated coalescing additionally requires the same scheduling key. Native scalar/group constructors can generate these matching options (`ip_remote_multi_client.go:16525`–`16645`), but negotiated nonzero lanes or role/companion/ForceStream differences can separate them; no default health-probe/group mixing is assumed.

The follower's frames and original ACK target enter ordinary `sendRecordsForSchedulingKey` (`9471`–`9485`), which supplies an empty `sendGroupDisposition` (`10560`–`10574`). Successful serialization still sends every raw frame and completes the callback; **do not claim generic payload loss or framing corruption**. However, no native materialization/range proof is published, so collapse duplicate suppression fails open. If another sequence spends shared memory between the advisory size check and actual reservation, the empty disposition emits generic `ErrSendPackNotAdmitted` (`10636`) instead of the native group capacity marker. The group's ACK-required completion then reaches `addError` (`ip_remote_multi_client.go:16791`–`16797`) rather than packet-local abandonment, marking the provider channel failed and exposing its other flows to subsequent retirement.

`late-transfer/group-ready-drain-boundary.md` (SHA256 `38c0b07f28b49b560231cbcf6b1770ce89d5c92ce4257c0026e1e16f0671f6c6`) records exact constructor and counterpath limits. Side `732ddec9` adds the follower exclusion, but that unfinished checkpoint is not a qualified fix. Test actual same-key constructors and prove the OLD coalescing premise first; use a second live sequence to spend memory between compatibility and reservation. Require exact frame returns, no sequence/contract gap, no provider-fatal classification for local capacity, and retained range proof on success. Different lanes/flags, oversized groups, normal room and genuine retained/contract failure are controls. No partially consumed cursor/pool replay defect is asserted without a reachable ordering.

## Additional scoped findings and policy exposures

These are retained because the user requested all-commit review, not because they are established causes of the reported incident. Their narrower activation requirements place them behind the demonstrated P1 source failure and the broadly relevant pressure boundaries.

| Mechanism and origin | Current evidence and prerequisite | Finite discriminator |
| --- | --- | --- |
| VLESS WebSocket cancellation, `c5a9589d72b4b665195fc8b32bc01abc8f5e27cc` | Explicit VLESS `ws` only. `vless_dial.go:282`–`315` hands a native socket to Gorilla without an inner physical cancellation guard. Manual cancellation can leave response I/O until the residual default five-second handshake deadline; strategy winner cleanup joins the loser (`net_http.go:1387`). Optional configuration is not observed on the affected device. | Hold the actual upgrade response, cancel the loser while a healthy competing route wins, and require physical interruption and joined ownership before the handshake deadline. Do not abandon worker joins. |
| VLESS httpupgrade late deadline mutation, same origin | `vless_dial.go:271`–`277` stops an AfterFunc without joining a running callback, then clears the deadline. A delayed callback can install an expired deadline after a successful handoff. Explicit httpupgrade only. | Barrier the real callback's SetDeadline before mutation, complete valid upgrade/cleanup, then release it. Returned streams must not receive a late deadline; cancellation and teardown remain exactly once. |
| WebRTC directory wake generation, `6b02e0c7c3cd5223fc238c246ad5db3c2c48d81d` | Setter at `net_extender_directory.go:2130` advances monitor value but not `self.version`; the next ordinary mutation at `2778` can publish the same value and miss notification. Production setter use was not found; direct state reads still work. | Toggle, subscribe, AddManual and require a new edge plus monotonic generation. Test persistence without another mutation or Close. |
| Fixed-IP sticky refreshed path, `270af0c604ed5922ffaab7025b8a48bf2986ddad` | `ip_remote_multi_client_sticky.go:138`–`177` discovers a fresh full path to the same exit but returns the saved old intermediaries. Requires fixed destination and changed multihop topology; single-hop control unaffected. | Old-hop/same-tail versus new-hop/same-tail discovery through the real transfer attempt; require fresh path use or explicit fallback. |
| Endpoint policy hint can reroute an established sibling, `dafd65af6cfbaf4fe5aefe4946d9ca6817478e0e` | Endpoint-wide hint at `ip_policy_hint.go:32` overrides per-flow routing before owner lookup (`ip_remote_multi_client.go:6177`). Requires eligible high-port encrypted policy; ordinary 443 cannot generate this hint. Default orphan RST makes reset/reconnect more likely than silence. | Establish a provider-routed sibling first, publish another flow's same-endpoint hint, then send scalar/grouped traffic. Define route-lifetime preservation or explicit termination; include UDP and hint expiry. |
| UDP NAT transient error retires one mapping, pre-window `f6682c52` | `ip.go:3518` closes on any shared-socket Write error; legacy batch sibling exits too. Only this flow/mapping is retired, but port replacement can disrupt QUIC. Not equivalent to whole-listener P11. | A typed zero-progress pressure error followed by success on the same actual mapping; preserve fatal/ambiguous/short-write controls. |
| Shared extender measurement/ordinary admission | Session/load/feed spend the ordinary subnet action bucket (`extender_lag_api_session.go:541`, `847`; `extender_lag_feed.go:79`). Defaults are eight actions/minute per subnet, one token per 7.5 seconds; repeated refusals can reject new TCP/QUIC connections before auth. Existing streams stay live. This is intentional abuse policy, not a proven defect. | Consume the measurement allowance in one subnet, compare new ordinary/signed-peer admission and a cold subnet, and assert documented refill/refusal with no established-flow cancel. Policy changes need an abuse model. |
| Refused private IpPing frame loses pool return, pre-window helper lineage | `multiClientChannel.SendDetailedMessage` (`ip_remote_multi_client.go:16802`) allocates its own frame, then forwards a take-on-success send result without returning that private frame on refusal/error. The caller cannot recover it. Even an empty IpPing has a pooled root. This loses pool reuse/accounting, not necessarily permanent retained RSS because GC can reclaim it. The direct-return helper predates the window; the full original ownership-contract onset is not established. | Invoke the real helper with deterministic refusal, error, success and cancellation for empty/nonempty pings. Reconcile root take/return counts exactly; success ownership must remain transferred once. |
| Local extender budget refusal becomes remote quarantine, pre-window feed/probe origins | `net_extender_memory.go:193` can reject before wire I/O. Feed `net_extender_network.go:1145` and probe `1927` exclude HTTP429 but not this local sentinel, reaching directory failure/backoff (`1295`, `1624`): initially ten minutes, potentially 40 minutes after three refusals. Direct strategy paths already exclude it. Requires the actual feed/probe owner to be exhausted or unable to fit the reservation; default unrelated budgets must not be conflated. Existing acquired HTTP leases need not be canceled. | Exhaust the exact inherited owner, prove the sentinel occurs before physical dial, release capacity and require reuse without remote penalty. Compare cancellation, HTTP429, genuine remote failure and mixed-cause errors. Do not bypass real peer failure policy. |
| Nil-result custom settings factory bypasses aggregate memory sharing, `e041eaf9a38b4d919c64d3a7b39a19d74f1ef2e4` | `ip_remote_multi_client_api.go:1293` creates defaults first; a nonnil factory returning nil retains a fresh default budget but skips the owning generator's budget branch. Multiple windows can therefore each have independent caps. This is an explicit custom-configuration path, not demonstrated default SDK behavior. | Two windows from the actual nil-result factory must share the intended root; after the first consumes it the second refuses. Nil factory, explicitly supplied custom budget and a separate generator are controls. |

Lower-priority bounded leads remain in the sidecars: answered-probe courtesy RST waits up to one second on a shared receive callback (`ip_remote_multi_client_probe.go:959`); native RPC getters can hold a shared device-state lock across a 60-second RPC; optional durable contract capture performs filesystem work before publication; score-store TTL removal only on key lookup can retain abandoned network-generation rows. The SDK async JWT lookup race predates this window (2024 import), not a newly introduced regression. None is a reason to remove authentication, lifecycle, raw-stream or anti-abuse boundaries.

## Historical repairs and rejected hypotheses

The review did not treat every changed queue or candidate branch as a current defect:

- The analogous Transfer idle-consumer mutex cycle was fixed by `fbace7618`; P9 concerns surviving NAT siblings. Late retained-ACK policy has a 240-second destination-silence cap (`1abca801`), so historical unlimited silence is not current behavior.
- `ec426ed0` is a test-only worker-park change, not a production fix. `79cb9de6`'s current H1 complete-root accounting and retired-carrier exits were examined; no separate current stall was demonstrated from that change alone.
- Historical H3 accepted-response metadata strictness from `9392512e` is corrected by current `d6bda5ea`; deployed remote version remains unknown. `671a44c7` changed a full H1 echo queue from immediate close to bounded-owner waiting; P4's real-frame interception is a distinct older behavior.
- Side `0c444e7f` fixes an orphan waiter in a side-only FIFO implementation. Frozen HEAD uses a different CapacityNotify path, so absence of that patch does not establish an orphan FIFO waiter on HEAD.
- The proposed native logical-group deadline-to-provider-failure chain was rejected: `sendGroupToTargetWithTimeoutDetailed` does not set a Pack deadline. A manually supplied deadline in a test does not establish native reachability; its acceptance timeout must not be reinterpreted as an end-to-end guarantee.
- The proposed native tracked-group prepared-credit failure chain was also rejected: `parsedPacketGroup` does not implement `preparedSendMemoryTarget`; the production implementation belongs to `providerDatagramAck`. These exclusions do not settle the separate scalar/group coalescing boundary.
- SDK H3 window limit is per quality/speed class and the monitor sums both. A target of two can correctly arise from two classes each capped at one; the provisional stale-test suspicion was withdrawn.

The per-SHA ledgers retain these dispositions, scoped generated/activation-boundary reviews and explicit exclusions. A commit's own claimed test results are not independent execution evidence.

## Expanded pressure-to-shutdown audit

The user's later instruction expands the task beyond the four-week changes to the current reachable transfer pipeline. The scoped current inventories are sealed: 40 early Connect rows, 12 Transfer rows, 20 transport rows, 13 Server rows and 16 SDK classes. These overlap and are not summed as unique defects. They record unit/ownership, triggering pressure or error, blast radius and valid security/lifecycle/raw-stream countercases. This is a finite reachable-path audit, not a claim that every Cancel/Close statement across three repositories has been manually proven. The main live boundaries are:

| Boundary | Unit and recovery owner | Current blast radius | Assessment |
| --- | --- | --- | --- |
| Server forward ingress full | Complete serialized Transfer; originating sender owns reliable retry; relay adds no endpoint ACK | Whole resident and unrelated destinations | Over-broad pressure teardown; P2 |
| Server control ingress full | Complete control message; endpoint ACK can follow callback handoff before durable apply | Whole resident | Requires separate application/Transfer ACK custody analysis; do not copy forward remedy blindly |
| Server pooled send timeout, `resident.go:2861`, `3193`, `3484` | Complete caller-owned Transfer returned on timeout before wire write | Internal exchange generation or one destination forward | Framing rationale is inapplicable; preserve exact retry ownership and count refusal |
| Receive receipt admission full | Complete Pack; advancement and ACK ledger already involved | Source/session/logical receive lane | Admission/advancement must be reversible or delayed; P3 |
| Provider decode workspace busy | Complete Pack with pending delivery claim | Source/session/logical receive lane | Temporary capacity must not become an unqualified terminal claim failure; P3 |
| QUIC send-flight split root bound | Raw STREAM fragment with QUIC ACK/loss owner | Entire physical carrier | Reserve headroom before admission; P5 |
| Bounded provider source count | Complete IP/Transfer source admission | Entire provider NAT generation | Conditional wider-than-refused-source rotation; P6 |
| NAT count-limit replacement before memory admission | Complete new flow admission; existing victim owns its independent TCP/UDP state | Victim flow, possibly another source under GlobalLimit | Rejecting newcomer can unnecessarily cancel victim; P10 |
| DNS translator UDP write error | Complete datagram or one DNS fragment of a QUIC packet; QUIC retains stream recovery | Shared DNS listener on server, individual carrier on client | Classify transient zero-progress pressure separately from fatal socket error; P11 |
| P2P asynchronous UDP batch error | Complete unreliable datagrams, with prefix progress possible | Shared ICE socket and other protocol users | Drop/count only proven unsent transient suffix; P11 |
| Extender nested datagram send error | Complete outer frame yielding inner UDP datagram | Inner QUIC carrier | Distinguish whole datagram refusal from outer-stream truncation; P11 |
| Busy probe expired before wire | Complete admitted probe; local expiry proves no remote observation | Selected channel and pinned flows | Exclude local unwritten outcome from remote conviction; P12 |
| Native group consumed by scalar coalescing | Complete group frames; generic consumer loses typed local-capacity disposition | Provider channel on reliable callback error, then other pinned flows | Preserve native disposition/capacity provenance; P14 |
| SDK RPC/browser mux overflow | Tagged carrier message containing arbitrary gob stream bytes, no per-chunk replay owner | Paired RPC stream/session | Raw serialization continuity differs from complete retriable IP/Transfer; cannot silently drop arbitrary chunks |
| Native SDK TUN packet gate | Complete IP packets, caller ownership retained/released according to result | Refused packet/batch | Current path returns false and consumes rejected buffers; does not cancel device |
| H3 datagram flight/reassembly pressure | Datagram or reassembly message | Message/fragment | Current code drops/counts or takes bounded stream fallback, not broad teardown |

Keep closes for authentication failure, terminal revocation, malformed framing, impossible ownership corruption, explicit lifecycle cancellation and actual socket failure distinct from ordinary queue pressure. No blanket removal of `Cancel`/`Close` is proposed.

The current CODESTYLE receive section overstates that loss of an already-read reliable message creates a permanent gap and treats some complete-message refusals as framing corruption. The user's correction takes precedence. Recommended wording for a later explicit documentation change: **At a complete independently framed message boundary, refuse immediately and count when the protocol still has a retry owner; leave unrelated flows live. Before refusal, identify whether an end-to-end ACK or application acceptance already transferred responsibility. Raw stream fragments without replay cannot be skipped. Carrier reliability alone establishes neither safe dropping nor a need to close the generation.** This report does not edit CODESTYLE while source controls are retained.

## Bounded sequence-depth experiment

The user proposes doubling sequence depth after frequent overflow. Connect already has an optional H1 adaptive stepping mechanism (`transfer.go:13563`, `14689`), introduced before the window in `3a87ab8d`: count/byte hard maxima, a distinct-call saturation threshold/window, and an unchanged shared Pack memory budget. SDK `mobile_memory_policy.go:406` explicitly disables those fields, including explicit caller values; tests assert the clamp. The deployed effective setting must be recorded after construction. Merely setting a field before policy application can produce no change.

Use a privately isolated comparison with the same source/runtime workload: baseline versus a bounded `N -> min(2N, Nmax)` policy, preserving both the **32 MiB / 64 MiB device memory targets and every actual ancestor/child budget**, not assuming a single32/64-MiB Transfer owner. The shared Transfer root is `floor(9T/20) + floor(4T/20)`: **21,810,380 / 43,620,760 bytes** (about20.8/41.6MiB). DNS, platform carrier graphs, native resources and process footprint have distinct ownership and must remain in the comparison.

`N` is the actual after-construction, profile/share-derived depth, not a universal32. SDK generic caps are21/42 and explicit-H1-with-ACK caps85/170; lane zero retains those undivided caps. Explicit-H1 setup requests10/21 logical lanes, Connect clamps them to8, then divides **nonzero** lane capacity with ceiling: at most generic3/6 and H1-with-ACK11/22. Existing smaller caller settings may remain smaller through `min`. Effective receive byte caps are174,762B and262,144B (the latter clamped by Connect's256KiB ceiling), not the unconstrained349,525B SDK intermediate. The actual mobile TUN gate remains1MiB and a conditional H1 ACK ceiling remains2MiB at both profiles; explicit hosted LowMemoryProfile alone does not activate the mobile TUN gate/sampler. Doubling count cannot repair a byte-budget or ACK-ownership bottleneck.

Connect preallocates the queue channel to the configured adaptive maximum at sequence construction (`transfer.go:14554`–`14629`), even before logical growth. This `make(chan *ReceivePack, max)` heap capacity is **not itself an admission against PackQueueBudget**. Enabling a larger maximum incurs descriptor capacity immediately; unchanged payload/root budgets alone do not prove unchanged total memory. Include channel slots/envelopes and their teardown lifetime in both target-profile and physical-footprint measurements. Do not assume adaptation allocates only after measured benefit.

Record separate refusal causes: count, encoded bytes, retained bytes/ancestor budget, decode workspace, receipt ledger, admission fairness and expired/canceled owner. Growth should require repeated count-limited overflow plus positive consumer progress, a bounded age target and available budget. Use a cooldown and shrink-on-sustained-low-occupancy hysteresis in the experiment; never grow a dead consumer. Protect a cold/new flow's minimum admission and control/recovery headroom across competing hot flows. Pointer-slot reservation and backing-array lifetime also count; a logical count increase does not create free memory.

Counterfactuals: a finite burst with a draining consumer should benefit; sustained offered load above service rate should remain bounded; a stopped consumer should not grow indefinitely; byte-limited and root-limited cases should report no useful depth gain; a hot flow must not starve a newly active sibling; cancellation and role/profile shrink must reclaim all ownership. Compare completed useful bytes, cold-flow first-byte latency, p95/max pause, refusals/retries, queue age, fairness and full memory reconciliation. Retain a runtime change only after measured improvement without a worse tail, ownership failure or budget violation. Increasing depth is not a substitute for removing erroneous teardown or resolving a self-dependent recovery wait.

## Device evidence and next discriminators

Only Pixel `3B161FDJG001KT` and Galaxy `R5CX21FY6ND` are authorized measurement devices. Sol is the sole executor. Current four-build receipts are build attestations, not install or performance results. A focused Android test cannot establish native iOS behavior or replace the full 32/64-profile WiFi/cellular/P2P qualification matrix.

The ordinary public physical arm starts after `connected` and `tunnelStarted` and runs direct/H1/auto/H3. Its fixed workload performs five page navigations, three speed probes and two video probes. The existing browser script can capture per-request DNS, TCP connection, TLS and first-header timings, but the ordinary recipe does not enable `--waterfall` or guarantee `--fresh-context`. Browser TCP timing is not packet-level SYN/SYN-ACK telemetry. The current setup also does not isolate pre-connected route/admission delay. Root released a fresh canonical full PERF-only campaign with four builds and the declared 168 cells/560 observations, but its physical phase failed collection after only two control observations; that is not a completed qualification matrix.

One terminal H1/WiFi control-cell triplet has now been read, with exact paths and SHA256s in `PHYSICAL-PROVISIONAL-EVIDENCE.md`. The matrix label is an iOS/32 MiB memory profile on the authorized Pixel Android device, not native iOS evidence. `fast-1` and `fast-3` are invalid, incomplete 90,002 ms display-poll timeouts with `cleanupComplete=false`; their displayed 30 and 7.2 Mbps are not qualified speeds. Cleanup shares the exhausted 90-second deadline, so that flag is not independent evidence of a leaked browser. `fast-2` is valid/completed with cleanup, displaying 40 Mbps while its page-byte rate is 16.35 Mbps. The respective `pageEncodedBytes` counts are 26,318,411; 131,220,598; and 89,476; these count completed page-target requests, **not all transport download bytes**, and omit worker-owned/partial canceled transfers. Every categorized request failure is cancellation; the valid probe also contains cancellations. Canonical collection did not enable final DOM/spinner diagnostics. These results establish intermittent probe completion failure in one control cell, **not a packet-layer diagnosis, a candidate comparison, or P1 causation**.

The terminal `phase-physical.json` (SHA256 `655dc8674c5775c287b8e79c89f742198e4a43e6268ab58193e559f2deadc1d0`) records `collection_only=true`, two observations (direct valid, H1 invalid), `cleanup=true`, and `physical/lowbar/memsteady=false`. The failed required child is `fast-1`. H1 active memory peaks at28,676,128B, quiet maximum26,775,584B, quiet p50 25,501,728B; overall peak30,003,232B includes auxiliary work. These reported runtime metrics are below32MiB in this scope, but **do not constitute a MEMSTEADY pass, full32/64 qualification or native iOS-footprint proof**. No candidate observation completed. The enclosing runner's later simulator/comparison/cleanup join is a separate source-release boundary; this review never uses physical-phase termination as permission to edit live source.

The smallest useful phone discriminator is a bounded current-source Android64 H1/direct comparison with two or three fresh navigations and timing waterfall, then a controlled provider/P2P comparison only if required by the observed phase. Preserve exact device/build/APK/module/source proof, selected physical lane and generation, provider role, underlay, and per-request failure denominator. Join queue/ACK/retained-memory diagnostics to timestamps; a throughput aggregate alone cannot distinguish DNS fallback, cold admission, pacing repair, provider rotation and resident reconnect. Waterfalls include URLs, so raw evidence stays private and must be sanitized before becoming a repository fixture.

The OLD P1 normal/race overlay has now reproduced the constructed-state failure. A finite next sequence, coordinated only through Sol and without overlapping the full campaign, is:

| Priority | Discriminator | Required outcome before any production claim |
| --- | --- | --- |
| 1 | P1 real-route mixed-history, sibling-only, earlier-hole/later-tail, ACK-interval, cancellation/debt/cooldown and initial caller-budget controls | Establish the exact dependency predicate; a broad abort that merely makes the positive test pass is insufficient. Keep original OLD overlay immutable. |
| 2 | Actual-constructor resident overflow, receipt/decode contention and failed NAT replacement admission | Zero-wait complete-message refusal with unchanged unrelated lifetime; no false cumulative ACK; identical retry delivers once; exact pool/budget reconciliation. No constructor bypass hiding effective capacities. |
| 3 | H1 speed/data/ACK overlap and typed UDP-error injection | Real data not reflected; legacy probes negotiated; transient zero-progress pressure contained without duplicating a sent prefix; fatal/raw-framing errors still close the proper owner. |
| 4 | Correlate fresh navigation phase timings with actual lane/provider/generation and queue/ACK events | Distinguish tunnel DNS, local admission, provider teardown and recovery timing. Android evidence stays Android evidence; do not infer iOS causality from aggregate throughput. |
| 5 | Baseline versus bounded count-depth doubling under attested effective32/64 settings | Measurable useful-throughput or tail-latency improvement, unchanged hard memory envelope and cold-flow fairness, stopped-consumer/byte-limited negative controls, no ownership regression. Otherwise do not retain the change. |

Remaining conditional leads are finite regression-test directions, not excuses to block the original history deliverable or to expand traffic indefinitely. Results and counterfactuals will be appended only when the execution owner supplies actual artifacts.

## Post-review validation — 2026-10-17

The original private report remains sealed at SHA256 `958623a230e33f9150634b6478fa78e0011f4725f19d26754644aabdf0588c02`; this section adds later execution evidence without rewriting that receipt.

Sol executed the separate eight-root OLD-source adjacency overlay once normally against unchanged Connect production code. All eight roots ran: five PASS, three FAIL, zero SKIP; package elapsed time was 0.632 seconds. `EarlierHoleReleasesLaterTail` reached its intended semantic premise and failed with selected sequence 0, a pending physical tail and 40.512 seconds of fake-clock drain remaining. This extends the constructed dependency evidence, not measured device latency or incident attribution.

The other two failures are fixture failures, **not additional demonstrated production defects**: `MixedHistoryPreservesIndependentTail` failed setup because route bytes did not match the recorded physical owner; `SiblingOnlyPreservesAttempt` failed setup because its third original did not park before physical dispatch. The race run was held rather than repeated. Astra is investigating a separately versioned fixture correction; the released original overlay and its failed result remain unchanged. No production pacing fix is qualified.

Raw result: `go1272-qualification.NmFy9i/continuous-offer-h1/canonical-integrated-parents-v1/postmerge-full-current-01/pacing-adjacency-old.omith3/normal.go.json`, relative to `/Volumes/data/urnetwork-h1-chronology.Y0OYTR/`; SHA256 `a70a3ae13add6881185a3ed20821209691b9deb6ecdeb8dae21e83f29b8f5f96`. The test owner and its independent resource observer joined before this documentation update.

## Follow-on validation — 2026-10-17: deterministic mechanisms, not incident attribution

This later checkpoint supplements the preceding report and preserves its failed
fixture attempts. At this checkpoint numeric pacing and receive-admission production
candidates remained private; neither was adopted or device-qualified. Deterministic
counterfactuals establish the tested mechanism, not deployment, onset, the
reported iOS website pauses or the historical loaded-probe 0/64 cause.

With V5's corrected fixture-publication barriers, the P1 OLD normal and race
selections each reached exactly three semantic failures and seven passing
controls. The numeric candidate passed all 13 focused cases normally and under
race. The combined affected pacing prefix then passed three fresh normal and
three fresh race processes, each with 456 top-level passes and no failures or
skips. These are not the full Connect package or ALL/MAIN/PERF. The 13-case
selection includes isolated metadata/predicate controls as well as real-client
histories; it is not 13 independent constructor-backed production reproductions.

The numeric predicate releases only the current, unambiguous, confirmed H1
measurement drain when the selected same-sequence repair R covers its retained
physical tail T before original O: `R <= T < O`, in the current drain
generation. Paid serialization, reservation/FIFO debt and ordinary repair
dispatch remain owned. Candidate source SHA-256 is
`634177fdd5c6959f3aac7ca76219e70c8bf527b22448b69689c2d3bd179d94be`.
The earlier broad pending-repair abort is not this candidate. Initial pacing
still precedes creation of the physical writer timer; these tests do not
establish a universal 15-second initial-wait bound.

P3 now has a scoped actual receive/dispatch counterfactual:
`TestReceiveReceiptAdmissionRefusalKeepsExactRetry` fails the intended
pressure invariant on OLD production and passes on the reference, normally and
under race. The refused complete, ACK-required Pack must retain exact retry
custody; it need not already have emitted a cumulative ACK. Advancing the head
and then merely removing cancellation is unsafe because an unretained duplicate
below that head can be cumulatively acknowledged without delivery. This result
does not convert complete retriable messages into unreplayed raw-stream
fragments or justify blocking a shared receive callback.

Keep the P3 fixture failures separate from that production root:

- The first attempt decoded every gateway message as an ACK and failed before
  the pressure oracle. The real provider emits a typed diagnostic Pack. V3
  validates offered ACK identities and that diagnostic separately, without
  disabling the publisher or inventing its peer ACK. Four five-root
  OLD/reference normal/race control processes passed; the ACK-only fixture
  counterfactual failed on the real 165-byte diagnostic.
- Reference V3 all-25 normal passed 23 and failed two witness checks on
  10,239/12,288-byte ordinary protobuf storage. Pool classes end at 8,192 bytes;
  `MessagePoolReturn(false)` for unpooled storage is not itself a leak.
  V4 preserves payloads and pooled-owner checks, and adds explicit unpooled
  reference-closure, boundary and full-owner-join controls, without forced GC.
- V4 all-30 normal passed, but race passed 27 and failed three fixture dial
  tripwires, with no data-race warning. The hook waited for ancestor cancellation
  while the TCP worker uses a descendant context; the ancestor can close first.
  The tripwire is not evidence of an actual network socket being opened.
  A separately sealed V5 waits for the worker's own cancellation and adds a
  deterministic propagation-barrier control. At that earlier checkpoint V5
  had not executed and V4 was not full P3 race qualification.

Later V5 execution independently reproduced the fixture defect: the exact V4
fixture with the new control failed only
`TestReceiveReceiptFixtureWaitsForOwnTcpCancellation`, at the assertion
`fixture released TCP worker before its own cancellation`. That is a fixture
counterfactual, not another production defect. The corrected root passed
normally and under race, then all 31 reference cases passed once normally and
once under race, zero failures/skips and no data-race warning. All five process
handles and the observer joined. This closes this focused fixture/control
selection; broader delivery/ACK parents, healthy-burst costs and device
qualification remain separate. No production bytes were integrated by this run.

The loaded H1 evidence is a different, unqualified experiment: one OLD/numeric
Record25/run1 pair on an active Go1.27.2 host. Both delivered the same 65,536
useful bytes. OLD had invalid calibration; its efficiency 1.099415 is
tunnel/direct, whereas the gate requires at most `1/1.10`. Numeric was valid
for that diagnostic but had only 6/59 successful loaded tunnel probes versus
OLD 40/49. Its successful-probe p95 was 6.412 seconds against a 6.416-second
deadline and excludes 53 failed attempts. Direct loaded successes were 47/54
and 42/45 respectively. One pair with an invalid OLD arm cannot establish a
causal or statistical source regression, improvement or baseline.

All eight windows were labelled dead by a fixed 5-Mbit/s threshold on a
0.256-Mbit/s shaped link; that classification is not independent stall proof.
Direct duration ends at body receipt, while tunnel duration includes the reverse
completion byte and server join. The 8.278-to-10.063-second reported tunnel
difference is not an isolated body-throughput measurement. Existing observations
also do not establish that the numeric dependent-drain action ran. The sealed
RCA proposes separately reviewed, bounded actual-action/probe/completion-phase
attribution; it neither reports that execution nor authorizes extra replays.
The numeric candidate is not performance-qualified for integration.

Evidence locators below are relative to
`/Volumes/data/urnetwork-h1-chronology.Y0OYTR/`. Raw run prefix is
`go1272-qualification.NmFy9i/continuous-offer-h1/canonical-integrated-parents-v1/postmerge-full-current-01/`:

- P1: `pacing-v5-run.mnO1dU` and `pacing-v5-full-confirm.BCgpJ9`.
  Sealed fixture manifest:
  `packetflow-regression-review/astra-review.AmbGZsAp/late-transfer/adjacency-v5.FkkBqNGJ/manifest.md`,
  SHA-256 `bfc19b2a490821e991528829d74d6b39733829f62e3ca0da6335c6f26604d4fc`.
- P3: `p3-v3-run.FXRozK`, `p3-v4-run.VID8EP` and later
  `p3-v5-run.6XGiOd`.
  V3 manifest SHA-256
  `3baef75bdb54927cdd2ce3b6cc3e9b351d310fa7fbafa10a1744915399bf6d34`
  in `remote-review-7days.M8OvPy/p3-gateway-fixture-v3.ERXs3jNv`;
  V4 handoff SHA-256
  `ee78e5defe0b26cfc434c568e7782035e1b0b54182b2971b517dba18253df512`
  in `remote-review-7days.M8OvPy/p3-witness-fixture-v4.5q2vlZxQ`.
  V5 fixture handoff, sealed before execution:
  `remote-review-7days.M8OvPy/p3-cancel-fixture-v5.IeaIsmVW/handoff.json`,
  SHA-256 `4f6efa56c96dbb054e8e95dbe1d1c9c2a1a602e2c149f7a0451df2f7371ede76`.
  V5 raw `fixture-v4-control-normal.go.json` SHA-256
  `b6220d35012b88145c897b5225bb6f19e753e56e79be38e15a4ddce1316e8f52`;
  `all31-v5-normal.go.json`
  `78d5c278321cc423eab31a4e10b365501040049c7f2b3b39e64921aada44ac85`;
  `all31-v5-race.go.json`
  `f0df93d9eac9b8ace670604815cd6cf38bc7408fdd7f29df2a4d013550056d3f`.
- H1: `h1-go1272-replay-run.zqnfyd/old.go.json`,
  SHA-256 `c35dce18333126295243e71a41a13673d4c737175589dcd16ce51056eeac7517`;
  `numeric.go.json` in the same directory,
  SHA-256 `6ef189eb2561a764df3a427dd542e72bbde54c5400834b465e148b98aeecfba0`.
  Sealed RCA:
  `packetflow-regression-review/astra-review.AmbGZsAp/h1-pair-rca.a6XeeCK7/RCA.md`,
  SHA-256 `207f7060f8e2ac0ac94a88d83fb0101c3b60d1f2c2581e8c4bca98e2b7aa9568`.

The counted test and observer owners for these reported runs joined. That
cleanup fact does not promote a candidate, identify the phone incident or
qualify the still-pending full parents, allocation/throughput gates and physical
32/64-MiB workloads.

## 2026-10-17 update: proven resident refusal defect

Subsequent validation deterministically reproduced P2 on current source. With
the real default 4,096-message ingress split into 16 shards, refusing either a
complete original or a genuine reverse ACK canceled the shared resident.
Both counterfactual roots failed at that exact transition, without setup or
timeout failures. The originating sender still owned retry; carrier delivery
had not transferred endpoint ACK custody to this forwarding callback.

The minimal production correction is now integrated in Server: preserve the
zero-wait refusal, extra-share return and counters, but do not cancel the
resident for ordinary queue saturation. Both newer fatal-consumer handlers
remain byte-for-byte intact. Seven real-origin roots passed in three fresh
normal and three fresh race processes, covering natural original retry,
genuine lost-ACK recovery/deduplication, NoAck loss, unrelated application
progress, and explicit-close ownership races. Test and observer owners joined
before integration. On-disk parent/AST checks remain required after formatting;
this is not a qualified performance or deployed-device result.

Evidence: the chronology prefix above,
`aa-postp2p-mixed-admission.20261017/r4-old-minimal-result.20261017/`,
`r4-fixed-seven-normal-result.20261017/`, and
`r4-fixed-seven-race-result.20261017/`. Normal/race terminal summary SHA-256:
`35c4412fbc6a820310d6b88a309d7ea9cc557fa512fc474826f8387ede649d24` and
`02bfe38a419ae8976dcb5ea222044407b4bc2163af863d049233536840079991`.

Adjacent review confirms the same incorrect framing rationale at two internal
exchange writer boundaries: ResidentTransport and ResidentForward retire their
ExchangeConnection when a complete message times out before socket-queue
enqueue. This attempted message has not reached socket framing. ACK-required
originals retain retry, reverse ACK loss uses duplicate feedback, and NoAck
uses counted loss. Actual socket/partial-write, authentication and lifecycle
failures remain distinct terminal conditions. A separate candidate exists,
but real-boundary regressions are not yet implemented or qualified; it is not
included in the integrated resident correction.

CODESTYLE's receive guidance has since been corrected to distinguish complete
retriable messages from unreplayed raw fragments; its earlier frozen wording
above is historical. Neither this fix nor the fixture-policy repairs establish
the cause of the iOS website pauses or qualify the historical H1 loaded-probe
failure. Fresh device 32/64-MiB and full-runner evidence remains outstanding.

## 2026-10-17 update: exchange correction and native-write oracle

The adjacent exchange correction is now integrated: a complete message refused
before socket-queue admission keeps the live ExchangeConnection generation.
Transport and forward paths count refused messages/bytes; Done, socket errors,
authentication failures and explicit lifecycle shutdown remain terminal. No
queue, batch, pacing, deadline or memory-budget increase is part of this fix.

The pre-fix counterfactual produced six real-boundary semantic failures with
five controls passing. The corrected twelve-root suite passed three fresh
normal and three fresh race processes privately. These tests exercise actual
Client/Resident construction and framing over controlled connections, not
radio, TLS or kernel throughput. Both refusal directions, original retry,
reverse ACK recovery, NoAck loss, unrelated delivery and teardown are covered.
The private normal/race summary hashes are respectively
`2cc18c45080a96e783bafce989af143ec88f9c8da1c033d6cab16cb381453e59`
and `60ec21044b9137c8361535aa421a2c9025fc4d6b2a863ff5cada4466c5b3df42`.

Adjacent review found a separate accounting error: after a partially written
middle frame, remaining bytes could incorrectly credit a smaller later frame.
The correction stops prefix accounting at the first incomplete frame while
still returning every input owner. A deterministic 23/41/7-byte fixture
reproduced the old two-frame credit instead of one; the correction passed
three fresh normal and three fresh race processes privately. Test cleanup also
now joins actual socket owners and reconciles never-offered sibling shares.
The cleanup test uses a controlled missing-registration negative, not an
assertion that the historical production code had that artificial mutation.

Native writev validation exposed two distinct fixture assumptions. First, a
single 256-KiB batch could complete despite requested small socket buffers;
waiting for connection termination did not prove a partial write. A finite
pressure fixture retains production-sized batches and the one-second write
deadline, queues at most 512 messages, and verifies predecessor bytes drained
by the peer. Its first diagnostic produced genuine 12,288-byte terminal
partial writes in both transport and forward paths; exact completed-prefix,
fallback-write, queue, vector-retention and pooled-ownership checks passed.

Second, the composed actual-source 26-root confirmation passed processes one
and two but failed process three. The forward terminal batch wrote 4,096
bytes before its real timeout, with a live parent and joined owners. That is
a valid header plus partial first payload, not a complete 8,196-byte frame.
Accounting correctly reported 65 complete frames / 524,553 wire bytes across
the singleton and two earlier complete batches. Only the fixture's extra
requirement of a complete frame inside the failed batch was violated. The
same invalid assumption exists in the shared transport fixture. Astra is
preparing a deterministic oracle regression; no retry or race/core continuation
was used to conceal the failure.

The failed actual-source confirmation summary has SHA-256
`5851f948a13366753d7337ee22130d7fd3d352222087874d2f9f9a0f2597e2fd`;
its third raw process has SHA-256
`c752df23cc88111cce3b0cdc2c8c66bd58a6ab0a09b05fe54d2227453608994a`.
All test/observer owners joined. These results qualify neither the composed
fix, the historical H1 0/64 loaded-probe outcome, nor the reported iOS pauses.
Fresh on-disk confirmations, full runner and physical 32-MiB iOS-surrogate /
64-MiB Android measurements remain required. No performance baseline is promoted.

The subsequent test-only correction is integrated but not yet executed: both
native fixtures use one shared oracle accepting strictly positive, incomplete
native progress with a timeout and live parent. Its completed-frame count is
the floor of written bytes divided by framed size, including zero for a
partial first frame. `TestExchangeWritevPartialTimeoutOracle` pins nineteen
valid/invalid boundaries, including the observed 4,096-byte outcome. Pressure,
deadlines, production code and ownership checks are unchanged. The fresh
confirmation count starts at zero; the failed epoch remains retained.

### 2026-10-17: partial-write and shutdown-fixture confirmations

The corrected partial-write oracle and adjacent ownership tests subsequently
passed three fresh normal and three fresh race processes, each selecting
exactly 27 roots, with no failures or skips. Their terminal summary SHA-256s
are `09d4e153b1d06b91389be7b2efa65f37612190e91b5627eaf6aec5accccabbd4`
and `f155a6d011e98cb23443fbc16de3d3561e2d7d7b122f187294822a16df5accb5`.
These are correctness results, not throughput or device-memory measurements.

A separate composed race run exposed a shutdown-fixture race: cleanup canceled
the exact child transport before publishing root cancellation, while the close
watcher treated a live root as evidence of an unexpected connection failure.
The test-only correction captures phase, owner cancellation, endpoint and
preceding refusal count at the first actual socket close. Expected teardown
is accepted only from that snapshot; late processing cannot erase an earlier
live failure. Adjacent admission/worker checks contain only canceled-child
Done during explicit teardown, not dropped work or other errors.

The deterministic control reproduced exactly 16 expected assertion mismatches.
The corrected 13-root group passed three fresh normal and three fresh race
processes, with no failures, skips or race warnings. Summary SHA-256s are
`bb3510fe102df03d2a6fb1add05d43ed1faf33e4ef078b57bcb25955c05a9779`
and `9167b9a893cab2f81ace023e241b9062fae1d6b8725ac33c0294a840d90d7e72`.
All test and resource-monitor processes joined. One earlier attempt stopped
before Go execution because the local launcher had exited; its failure is
retained separately, and the exact fixture was recovered preserving its data.

The qualified fixture postimages and one new deterministic root are now
integrated; canonical simulator coverage grows from 42 to 43 required Server
roots without changing repetitions, deadlines or worker policy. Shared-source
integration requires a fresh source identity and full runner replay. It does
not establish a fix for historical H1 0/64 probes or the reported iOS pauses.
Both physical memory profiles and performance qualification remain pending.

### 2026-10-17: physical wait localization and confirmed TCP composition bug

The single completed Pixel Wi-Fi/H1 Fast diagnostic displayed 33 Mbps after
88.761 seconds. Page-target accounting's derived 0.01 Mbps is not aggregate
throughput. The next diagnostic, with three intrusive stack captures, timed
out at the unchanged Fast deadline; its provisional 13 Mbps is not a valid
completed rate. Neither attempt promotes a baseline.

In two active captures the same TUN producer waits in
`SendSequence.acquirePackAdmission`; its exact sequence worker waits in
`windowBurstPacer.waitForServiceReservation`. All seven captured H1 writers
are at their input select, not socket writes. This localizes sampled upstream
admission/pacing pressure, not its cause. Different current Pack objects rule
out claiming one continuously stuck Pack. The run's 5.043-second maximum send
occurred between captures two and three; the maximum primary TUN read was
before workload launch. Do not conflate either maximum with the sampled stacks.

The matched client's 3,385 out-of-order hold refusals are attempts, not unique
lost packets. All 321 additional Pack-handoff waits succeeded. Fourteen ACK
queue waits totalled 8.354 ms (maximum 1.599 ms), with no reported errors.
Refused items retain a peer retry owner and do not advance local ACK state.
The independent receiver audit found no new custody defect or justified
capacity change; local resend counters describe the opposite direction.

A separate deterministic native composition test **did reproduce a bug**:
public Multi sends traverse real Transfer and provider TCP admission; the
provider retains two future segments, refuses a third at its bounded reorder
limit after Transfer acceptance, then drains an earlier gap fill. Its cumulative
TCP ACK reaches 1005, but Multi still suppresses the covered byte at 1005:
collapse drops increase 0 to 1 with durable coverage `[1001,1006)`. The test
uses synthetic data/identities and synchronous assertions, not a timeout as
the failure oracle. Existing lower-level retries bypassed Multi and missed
this composition. First-run evidence SHA-256:
`797ce1521f56385753d66147040dd05bd867aa5d4735c2967863a6f73ae57c0f`.

The proposed repair uses current-generation cumulative-ACK recovery authority,
not a timer expiry or no-ACK policy change. It remains private/unqualified;
independent review is checking source/peer sequence validation, stale
generations, token ownership, failed sends, group paths and wraparound.
This local bug is not yet attributed to the phone's pacing waits or historical
0/64 loaded probes. Additional flow-retirement cancellation testing is also
being prepared; its source-supported hypothesis is not a confirmed failure.

The stale Android test-probe cleanup bug was independently reproduced and its
five-file repair integrated. Three fresh normal and three race private runs
passed five roots each; the shared affected group passed 123 roots and the
entire backend race package passed 483 roots, all joined. Tested source identity
was `b250bc8a977ba463e315be2a7826b50f5b8182b4b5eed01588008990b6d898db`;
this subsequent evidence-only documentation append requires a new identity.
Full three-lane and real-device 32/64-MiB qualification remains required.

### 2026-10-18: P3 receipt/ACK custody integrated with default root coverage

On source identity `66a10a386bf4c466497b937e31f31bb3eb482c58567478d28bd253af5f0373c4`,
the exact OLD root again failed the receipt-pressure invariant and the reference
passed. The unchanged V5 fixture selection then passed all 31 roots in three
fresh normal and three fresh race processes. All 197 affected delivery/ACK
parent roots passed once normally and once under race. Both healthy-burst and
queued-cancellation owner controls passed normally and under race for each arm.
These qualified processes joined with unchanged inputs and clean guards; the
earlier fixture failures and separate control-phase capacity failure are retained.

The exact tested three production files and five fixture/benchmark files are
now integrated, with all eight postimages verified. Admission/reservation
precedes receive-head advancement or held-owner removal; refused complete
Packs remain retriable without an unowned ACK promise. Shared callbacks stay
zero-wait, and existing selective owners, secured cumulative prefixes, generic
oversize compatibility and cancellation/pool reconciliation remain covered.
This does not establish a fix for the separate decode-workspace boundary above.
The initial integrated 32-repository identity was
`c7cfdedb7dae6731a89f404ef3abbb8e551732b3e2ada20d490094a08dd4751c`.
The earlier private-overlay receipts retain their original source identity
rather than becoming on-disk parent results.

The first integrated full-root normal attempt failed the real js/wasm
test-compilation boundary: the unconstrained P3 fixture called
`poolBalanceUdp4Packet`, whose definition was in a `!js` test file. That
attempt later stopped for a separate capacity breach; it is not a passing
parent run, and neither failure is erased by subsequent qualification.
The helper's function body was moved unchanged into the unconstrained
`ip_packet_pool_fixture_test.go`. No production code, test skip or platform
matrix was changed. The existing `TestPlatformBuildVariantsCompile` then
passed in three fresh processes, exercising all six platform variants and
the actual js/wasm root-test compilation. The five P3 fixtures were also
audited for similar platform-excluded helper dependencies.

After this test-only move, the current code qualification uses identity
`76fcf6ad45627ab26ed65d9a3019b40282113e5134f3377492a0c08321184493`;
only Connect content changed in each transition. The fresh unfiltered Connect
root normal process passed in 1459.088 seconds: 5890 top-level runs, 5855
passes, 35 top-level skips and two DNS subtest skips, with no failures.
The fresh race process passed in 6237.322 seconds: 5890 top-level runs, 5854
passes, 36 top-level skips and two DNS subtest skips, with no failures or
reassembled data-race warnings. Both execution owners joined with exit zero,
child/after-guard exits zero, unchanged guarded inputs and empty stderr.
The extra race skip is `TestIpMuxSafeReceiveNoAlloc`: allocation counts are
unstable under race; the same test passed normally. The normal/race ledgers
retain all 37/38 exact skip reasons. Opt-in measurements, host/mobile conditions
and pending-implementation placeholders are coverage gaps, not verified
invariants. These are default root-package results, not canonical recursive
coverage. The previously failing platform test also passed within full normal.

The completed A-B-B-A comparison used two fresh processes per arm, 16 operations
per process, and 32 real complete Packs per operation. A is OLD; B is the
reference. Each operation constructs Client/provider/NAT owners, queues the
burst behind the receive-worker barrier, observes actual final admission and
the last cumulative wire ACK, then cancels and joins all owners with exact
pool/budget reconciliation. The progress observer is nil; held pre-Run TCP
workers open no live sockets.

| Arm | Lifecycle ns/op | B/op | allocs/op | Delivery ns/burst | Joined ns/burst |
| --- | ---: | ---: | ---: | ---: | ---: |
| A1 | 461,227 | 682,153 | 4,370 | 338,031 | 393,893 |
| B1 | 415,260 | 546,399 | 4,477 | 303,151 | 346,763 |
| B2 | 480,883 | 535,270 | 4,446 | 347,487 | 393,503 |
| A2 | 408,591 | 688,806 | 4,375 | 315,479 | 348,039 |

Reference-versus-OLD means were lifecycle time +3.03%, allocated bytes -21.10%,
allocations +2.04% (+89 per burst), delivery time -0.44% and joined time -0.22%.
The time difference is smaller than the observed 12–15% within-arm spread.
Lifecycle metrics include setup and cleanup; the two wall timers begin at
burst release. These are descriptive host measurements, not statistical
equivalence, throughput, tail-latency or steady retained-memory results.
SDK/counting owners were quiescent, but the retained observer/fixture remained;
observer scans could overlap. A post-A1 metadata failure is preserved separately
and no benchmark measurement was retried.

The local cost gate is cleared: this observed tradeoff supports the exact
custody correction without an unqualified batching redesign. The source-known
1-to-32 flush / 2-to-64 clone opportunities are not a measured 32-fold slowdown.
The integrated default root normal/race gate is now passed. Canonical recursive
coverage, full ALL/MAIN/PERF and complete 32-MiB iOS-surrogate / 64-MiB Android
device qualification remain pending. No baseline is promoted, native iOS
footprint is not qualified, and
neither the historical H1 0/64 result nor the reported website pauses is explained
or repaired by these measurements.

Evidence uses the retained raw prefix defined above; let
`S = p3-current-custody-admission.5JVBwT` and
`P = p3-shared-full-parent-prep`. Exact summaries link their raw rows:

- `S/initial-result.json`: `c0389980e93cd86c4df59ad4638d2fd4016c4dc3e79f8ce4eebe685d05966ee6`.
- `S/remaining-six-result.json`: `96088458d089400202e526e4c8b5b8282701615249c4098c0c6c63286eb66210`.
- `S/qualified-owner-controls-result.json`: `15250a51653fb810b18c8714ebc0137d78d64fc6832f7b2886345de03a751fb2`.
- `S/timing/abba-result.json`: `a0a6b1eff64b4d016530dddc2b32f2733b8f4495c3503cdcf5e97d0a6c6d1f74`.
- `p3-integrated-source-admission.nXKeP9/source-identity.json`:
  SHA-256 is the full `c7cfdedb...` identity above.

- `p3-integrated-source-admission.ZBXTY5/source-identity.json`:
  SHA-256 is the full `76fcf6ad...` identity above.
- `P/normal/failed-summary.json`:
  `7cf3b7e6d03e144a64ace8384526e76b65cf1b42ac1ac3fea6594b865ec63faa`.
- `P/platform-three-process-result.json`:
  `23b96a1b8b66e7090906a5209c632a9871690c0bcf4176b22239078e0110ce1b`.
- `P/portable-normal/result.json`:
  `4b890436dedfc13a57f9c32bd7b19cb47db6653db905794ae6c8e8221eb3964c`.
- `P/portable-normal/full-root-ledger.json`:
  `71ef4e2695d4bc5c322a4b152b026f58e8514239cb34d0cfd051df3d2c1c36d4`.
- `P/portable-race/result.json`:
  `4029aa1e74814b13e875993395f8f297eaeddda16217342e7931b1db1616d9ce`.
- `P/portable-race/full-root-ledger.json`:
  `57db167722494ed49dea7315f1a2ca9e06ad18ce905c051ceb772e9b56e0adf7`.
- `P/full-parent-normal-race-result.json`:
  `7f0e6603917092e2a1601129ae7f6510be79a20104eb9a898fdd57370e6dad99`.
