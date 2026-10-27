# Rapid-browsing packet-flow stall: request for independent review

## 2026-10-23: integrated shutdown ownership; H1 stalls remain open

The SDK now registers both current and retired UpgradeMux pump joins with its
existing lifecycle owner. `Close` remains nonjoining; `CloseAndWait` waits for
borrowed pump callbacks to return. This joins only the IpMux pump, not all mux
workers. Two actual SDK old-code cases reproduced premature completion while
an exact pooled TCP/53 SYNACK callback remained held. The four corrected roots
passed normal, race, and fresh-process validation (12 passes). Canonical lifecycle replay passed all 15 checks (12 lifecycle roots plus
generator, C ABI, and mobile-source checks).

The internal DoH pump fix and atomic upstream replacement are now integrated.
The old ordinary-admission pump delayed a real TCP/53 reply; replacing an
upstream could also retain the previous batch sender. Two qualified old-code
failures and 39 corrected normal/race/fresh checks cover both root causes and
adjacent policies. The native pump uses ready-only remote admission; generic
senders retain their explicit send budget. Canonical and broad replay of this
combined integration remain pending.

H1 churn still has real failures despite its retry-tolerant PASS. One complete
HTTP-phase replay accepted all 120 legs but recorded 11 failed calls across
131 attempts in 119.856 seconds. Nine uploads finished the HTTP-client write,
then failed before response headers; two downloads timed out after 1,785,856
body bytes. The latter is also 109 TLS records and does not uniquely identify
a contract boundary. A subsequent cold snapshot joined a 15.001-second native
write timeout to its active upload, with no observed cleanup/cancellation.
The 317,736-byte stack was untruncated. Receive/send workers were in their
ordinary waits; observed TUN injections completed promptly. Neither full TUN
injection nor absent callback records prove TCP delivery or a drain defect.
The missing predecessor/ACK disposition remains unresolved.

The composed browsing fixture also needs a correction: its one-shot wait-entry
token was historical, while a later owner/count snapshot could observe another
retry generation. The old-code composed failure qualified, but the corrected
arm's initial-premise failure does not qualify a product regression. Full
100-page, performance, and device-profile qualification remain outstanding.

## Earlier 2026-10-21 investigation

Updated 2026-10-21. **Unresolved production fix; new causal lead:** the latest
baseline still fails, but a private pacing-only bypass completes all 100 pages
within the unchanged deadline (171.9 s, zero slow navigations, maximum HTML
first-byte 3.16 s). This is one intervention, not repeat/performance qualification.
The full tunnel has not yet been shown to complete the
realistic 100-page workload reliably within its unchanged 300-second deadline.
The same HTTP driver over a shaped direct/native path completes all 100 pages.
We need a causal explanation and a measured fix, not larger timeouts or a
weaker test. Suggestions and counterexamples are welcome.

## Latest discriminator: pacing-only bypass passes

The exact initial-admission candidate that previously timed out at 72 completed
owners / 75 HTML was rerun with only the existing private
`disableWindowPacingForTest=true` on both endpoint send defaults. Delivery-sized
admission, receive advertisements, explicit per-endpoint 2 MiB retained retry
budgets, lane 0, workload, link and deadlines remained unchanged. This is not
the broader constant-policy switch, which also changes receive behavior.

Result: **100/100 completed pages and HTML, 171.881 s workload, zero slow
navigations, 3.158934 s maximum HTML first-byte**. All 400 asset workers joined;
no asset retries or invalid statuses, no remaining pool roots/bytes. Source
before/after matched; harness and fixture cleanup passed with zero retained
resources. The private switch bypasses pacing route-policy and service waits;
this implicates that subsystem, not a specific arithmetic expression or drain.
Fresh paired repeats and a captured-state deterministic witness are next. Do
not ship an unconditional pacing bypass on the strength of this one run.

Evidence: `/Volumes/data/urnetwork-browsing-sizing-ab.SuqKJX/PACING-OFF-OUTCOME.v1.json`.
Review: `PACKETFLOW-STALL-META.md`. Host fixture heap is not mobile-memory
qualification, and this result does not qualify the iOS/Android memory gates.

A fresh pacing-on confirmation subsequently **fails at 46 completed pages /
49 HTML, 10 slow navigations, 63.078 s max HTML first-byte**, at the unchanged
deadline. All 196 started asset workers join, pools drain, source identity
matches and fixture cleanup passes; the known retirement-identity teardown
timeout also appears. Receipt in the same artifact directory:
`PACING-ON-OUTCOME.v1.json`. This is a fresh on/off contrast, not sufficient
repeat statistics. The controlled-drain-only variant is the next discriminator.

The controlled-drain-only variant subsequently **fails at 87 completed pages /
90 HTML, 12 slow navigations, 13.322 s max HTML first-byte**. It suppresses only
new drain creation while preserving ordinary rate/meter pacing, sizing and
budgets. All started asset workers join, pools drain, source matches and fixture
cleanup passes. Disabling drains is therefore insufficient; the remaining
rate/meter/feedback path needs actual-state capture. A single 87-versus-46
contrast is not a statistically qualified improvement. Receipt:
`/Volumes/data/urnetwork-browsing-sizing-ab.SuqKJX/NO-DRAIN-OUTCOME.v1.json`.

## Default-off implementation candidate

The operator chose to disable extra Transfer-level packet pacing across all
transports, leaving their internal flow/congestion control unchanged.
`SendBufferSettings.ApplicationPacing` now defaults to false and gates only
the extra scheduling loop. Unlike the earlier private bypass, this preserves
the route-policy and FIFO-repair eligibility checks. Delivery-sized windows,
receive-window advertisement, retained byte budgets and ACK/retry custody remain.
An explicit true value exists only for controlled pacing experiments.

The actual-worker regression `TestH1DefaultWriteDoesNotWaitForApplicationPacing`
fails on the earlier code with exactly five virtual seconds before physical
write, and passes without that delay after the change. Five new tests, including
explicit controller opt-in, default window policy, growth without pacing and
independent peer/budget caps, pass in three fresh normal processes. Existing
controller tests opt in explicitly without weakening their assertions.
Full correctness, the actual candidate's tracked 100-page test, bulk PERFVAR and
mobile memory qualification are still pending; the earlier private bypass pass
must not be presented as those results. After all required gates pass, the
requested postmortem will be written under `factury/postmortem/`.

## User-visible symptom and synthetic workload

On the latest iOS app over Wi-Fi, quickly opening successive pages starts fast,
then new links pause for roughly 5–15 seconds. This may involve provider/network
conditions; the synthetic reproduction is not proof of the user's exact session.

The synthetic workload deliberately overlaps navigation with unfinished assets:

- 100 successive pages, each 50–300 KiB including HTML and four assets.
- Four origins, four HTTP connections per origin, up to four live pages.
- Start the next navigation while prior JS/CSS/image-style bodies still stream;
  retire old page owners and cancel designated assets using the shared driver.
- Validate HTTP status, body length/content and cancellation/worker ownership.
  Do not silently turn failed requests into successful latency samples.
- 300-second global application deadline; the Go child has a separate
  600-second watchdog. Neither has been increased to obtain a pass.
- Cell-edge link: 5 Mbps download / 1 Mbps upload, 60 ms latency with 25 ms
  jitter per direction, 0.5% loss, 100 ms queue, profile seed 4013.
- Application TUN MTU 1100; TCP maximum RTO 8 seconds. Full-fixture dial race
  count 2, stagger 2 seconds, route-readiness dial allowance 60 seconds.

This is synthetic HTTP over native TCP, not a Chrome/Fast.com or hosted-media
test. HTTP connection reuse and speculative dialing are real Go HTTP behavior.

## Relevant pipeline and ownership

Application HTTP / gVisor TCP → native TUN output → Multi TCP collapse/admission
→ Client Transfer send/window/pacing → outer H1/H1+ → peer Transfer receive and
delivery custody → provider local-NAT TCP sequence → origin socket.

Return traffic goes through the provider's retained inner-TCP return chunks,
Transfer and outer transport, then the application TUN. There are three distinct
acknowledgements: outer TCP ACK, Transfer ACK, and inner TCP ACK. They do not
prove the same delivery boundary.

TCP IP packets use Transfer ACKs. Multi suppresses covered source TCP retries
while Transfer owns the original packet; failed admission must not commit that
coverage. Provider return bytes remain retained until the inner TCP acknowledges
them, providing recovery for loss after Transfer delivery. Pure inner TCP ACKs
are ordinary IP traffic, not Transfer control ACKs.

Shared receive callbacks must refuse immediately under overload without tearing
down unrelated flows. A refusal must preserve retry custody and must not publish
a false delivery ACK. Dedicated per-flow socket/recovery workers may wait for
bounded admission. See CODESTYLE.md for the complete ownership rules.

## Measurements

These are individual diagnostic runs, not a statistical performance comparison.

| Arm | Completed page owners | Valid HTML first-byte rows | Outcome |
| --- | ---: | ---: | --- |
| Full tunnel, combined private fixes, strict retained accounting, lane 0 | 63 | 66 | Page 66 deadline; maximum HTML first byte 36.323 s |
| Same full tunnel with bounded item tracing | 50 | 53 | Page 53 deadline; maximum HTML first byte 43.858 s |
| Full tunnel plus post-replay admission-clock fix only | 76 | 79 | Deadline; maximum HTML first byte 20.425 s; single run, not a qualified improvement |
| Full tunnel plus initial-admission tracking and replay-clock fix | 72 | 75 | Deadline; maximum HTML first byte 13.269 s; still no passing workload |
| Direct/native, clean link, same HTTP driver | 100 | 100 | Pass, 0.51 s test elapsed |
| Direct/native, shaped cell-edge link, same HTTP driver | 100 | 100 | Pass, 163.43 s test elapsed |

Completed owners and HTML-first-byte rows differ because assets outlive HTML.
Counts include zero-indexed page 0. The instrumented full-tunnel timing is not
qualified for comparison against uninstrumented speed.

The shaped direct/native control has one slow navigation and maximum HTML
first byte 9.557 s. All 400 asset client operations and server attempts finish;
zero active server attempts, invalid statuses or asset retries remain. Native
TCP still reports client 164 retransmissions / 15 timeouts and peer 752 / 102.
Thus retransmissions alone are not a sufficient explanation for the failed
full-tunnel workload. Both native pre-link output-drop counts are zero.

Important null-control differences: it removes Multi, Transfer, outer H1 and
provider NAT, uses a second gVisor TUN as origin rather than kernel origin
sockets, and omits the clean provider-access leg. Identical profile/seed does
not imply identical packet-loss realization after packetization changes. A null
pass narrows investigation to the added path; it does not identify H1 alone.

## Strongest observations from the failing trace

First slow navigation, page 3:

- 9.231 s acquiring an eventually reused connection, then 3.551 s from request
  write to first byte (12.781 s total).
- Actual GotConn identity proves connection 6 previously served page 2 asset 1.
  Its native retransmission/timeout counters increase from 1/1 at that asset's
  acquisition to 3/3 at HTML acquisition. These are two additional established
  TCP timeouts on the same connection, not merely a pending SYN's retries.
- The asset body-close observation is 102 microseconds after HTML GotConn.
  This correlates the asset/reuse interval; it does not prove that the observed
  Close call caused the socket release.
- A later speculative ConnectDone is not the chosen reused connection. Go's
  HTTP transport can race an idle connection against an independent dial.
- Two captured source Transfer items, 101 and 103, secure downstream custody
  promptly and finish their Transfer ACK processing in approximately 141 and
  208 ms. They are not evidence of a multi-second receive-custody hold.
- At crossing/write/first-byte snapshots: no Client-to-Pack handoff drops/waits,
  no pre-Multi TUN output drops, and zero Transfer timeout-resend writes on
  both endpoints. These snapshots do not account for every packet's history.
- 23 native bridge refusals occurred globally. Only four refused batches are
  retained in the pressure summary, none matching the selected flow; 19 earlier
  failures are omitted. They cannot yet be attributed to connection 6.

The bounded tracer drops 107 observations under contention and overwrites
4,544 ring entries. Selected connection identity becomes known after the
five-second cut; emitted pre/post timelines are also capped. Missing events
are **not** proof of physical loss or absent custody. The per-item trace ends
with page 3 and cannot identify the terminal page-53 failure's owner.

`RouteUnacknowledgedDuration` and its paired retained-item count are historical
maximum-silence observations, not a live outstanding-debt snapshot. Their
unchanged values must not be interpreted as a currently stuck item.

## Proven adjacent defects and current candidates

Four independently tested private fixes are in the latest full-tunnel arm:

1. Shared Client receive-pump handoff uses immediate refusal rather than parking
   unrelated sequences behind a downstream wait, preserving retry ownership.
2. Pure inner TCP ACKs bypass TCP payload duplicate suppression, allowing real
   duplicate ACKs to trigger recovery rather than hiding them.
3. A validated selective Transfer receipt contributes health evidence separately
   from final credit/ownership release, preventing a false blackhole verdict.
4. Provider retains/replays SYN-ACK sequence-space custody until the inner ACK;
   a lost SYN-ACK after Transfer delivery otherwise conflicts with source SYN
   duplicate suppression.

Each has focused deterministic evidence; **their combination still fails 100
pages**. Passing their tests must not be reported as fixing this stall. They and
the newer reproduction fixtures remain private overlays, not canonical code.

Two further investigations are active:

- **Native admission refusal versus accepted-but-held custody.** Force an exact
  real HTTP/native TCP segment to be refused by a full Transfer window, release
  capacity, and require a real same-sequence TCP retry to be admitted and finish
  HTTP. Contrast an admitted packet whose downstream publication is held:
  duplicate suppression should remain valid while its retry owner still exists.
  No stale-coverage defect has yet been demonstrated in this new witness.
- **Provider return replay timer ages during admission.** In
  `ip_tcp_return_recovery.go:runReturnRecovery`, `returnProgressTime` and attempt
  count advance before synchronous `receivePacket`. The callback may block in
  `retryReturnSend` waiting for Transfer admission. After a long wait the next
  retry interval may already be spent, causing an immediate redundant replay.
  The minimal deterministic test now reproduces this on the pre-fix candidate
  in both normal and race processes: after a three-second callback admission
  hold, the next replay starts zero seconds after completion rather than
  allowing the existing two-second backoff interval. It drives the actual
  recovery worker with a channel barrier and virtual time, not a full provider
  or network. The minimal post-admission timer fix now passes that same test
  normally and under race detection; the full 17-root `TestTcpReturn` cohort
  also passes in both modes. Initial retention before initial-send admission is an adjacent
  case under audit. The local timer behavior is reproduced; its contribution
  to the 100-page failure and any production fix remain unqualified.

The timer-only uninstrumented full replay still fails (76 completed owners /
79 HTML rows). It does not resolve the workload, and the difference from the
earlier 63-owner run is not statistically qualified. A separate actual
`TcpSequence.Run`/origin-socket test now reproduces a parallel replay of the
same bytes while their **initial** callback is held: the replay starts one
second after initial entry, before first admission completes. Its candidate
fix passes the same reproduction, and 30 TCP-return tests pass normally and
under race detection, including batch/wrap/SYN/FIN and inline-ACK controls.
The measured return descriptor remains 48 bytes on this arm64 host. Its full
replay nevertheless fails at 72 owners / 75 HTML rows. This is distinct from rearming after
a replay callback completes; both boundaries need correct admission ownership.

The real HTTP/native refusal witness now passes both arms normally and under
race detection: refused payload remains eligible, a real same-range TCP retry
is admitted and genuinely Transfer-ACKed, and HTTP completes with reuse of the
same connection. Accepted-but-held payload retains duplicate coverage and its
original retry owner. This does not reproduce stale coverage at those tested
boundaries; it does not exclude transient admission delay or pure ACK loss.

### New suspect: refused pure ACK / lost receive-window reopening

Source inspection distinguishes pure inner TCP ACKs from payload. gVisor emits
ACK-only segments without putting them in its retransmission list; the resend
timer does not regenerate an otherwise idle source's lost pure ACK. Native
ready-only refusal consumes that packet before Transfer owns it. Passing the
payload-refusal test therefore does not establish recovery for pure ACK loss.

A potentially stronger case is a lost window-reopening ACK: the provider's
reader waits on `receiveAckCond` with window zero, while all previously emitted
bytes are already inner-ACKed and the return-retention queue is empty. The
existing return-recovery worker has no retained head to time, and no provider
persist probe was found in the inspected implementation. A deterministic
actual-Run/socket test now reproduces the gap in both normal and race processes:
the actual reader is observed entering Cond.Wait at window 0, ACK=next=105,
zero retained bytes/chunks, and no probe across two virtual seconds. Only the
equivalent reopening ACK is then delivered; the same reader resumes the exact
HTTP response and all retention retires. The positive release control passes
in both modes. The bounded persist candidate now passes 51 focused return,
sequence and zero-window tests in both normal and race processes. A separate
actual gVisor/TUN peer control passes normally and under race: the probe elicits window 0
before application drain and a nonzero window afterwards, without changing
the cumulative ACK or corrupting either of two HTTP responses on the same
connection. This qualifies neither full-run attribution
nor the performance fix; the peer control does not exercise provider scheduling.

Focused race evidence:
`/Volumes/data/urnetwork-return-clock-root.eCgiNb/persist-all-race.jsonl`.
Native peer evidence:
`/private/tmp/urnetwork-tun-zero-window-peer-v1.m3KmSx/root-normal-v1.jsonl`.
Race evidence: the same directory's `root-race-v1.jsonl`.

Important counterargument: the synthetic application starts with a 1 MiB TCP
receive buffer and continuously reads individual assets much smaller than that.
We have not yet observed a zero-window closure in the failing 100-page trace.
Missing persist may be an adjacent correctness issue rather than this workload's
limiter. Capture exact refused ACK/window values before claiming otherwise.
The local gVisor reference (`snd.go:sendZeroWindowProbe`) probes with one junk
byte at the already-ACKed sequence `SndUna-1`; a candidate must verify the actual
TUN peer's response without delivering that byte to HTTP or advancing credit.

Five existing failed-send/collapse regressions pass on canonical and combined
candidate code normally and under race detection. This argues against the
already-covered failed-completion bug, but does not prove every still-owned,
not-yet-written packet has bounded latency.

## Questions for reviewers

1. Where can accepted inner TCP work remain owned but not make forward progress
   long enough to inflate native RTO and keep an old HTTP asset connection busy?
2. Can any native refusal, post-Transfer refusal, ACK coalescing or TCP reorder
   path release retry authority too early, suppress its only viable retry, or
   unnecessarily replay work still waiting for first admission?
3. Do provider return replay timers distinguish retention, admission, physical
   write, Transfer receipt and actual inner TCP progress correctly?
4. Is there an admission/ACK dependency cycle or pacing/window policy that only
   appears with several overlapping short flows and unfinished asset bodies?
5. What smallest deterministic discriminator would falsify your hypothesis?
   Please distinguish positive observed evidence from missing trace coverage.

Please do not suggest raising memory/depth/timeouts without identifying the
blocked consumer or retry owner and measuring the effect on unchanged traffic.
The intended outcome is a root-cause regression test plus a fix that completes
the full workload, followed by uninstrumented paired performance validation.

## Source and reproduction pointers

Canonical source revisions used in these experiments:

- Connect: `bc1bfbbf2275832f5120de3ad6b046461575e0df`
- Server: `20d0f074e9f6ac888bfc75270742d79c38f4fafd`
- SDK: `4b02b3822bb0db71aa97e3e532c2e3bf2b77381b`

Inspect `ip_remote_multi_client.go`, `ip_remote_multi_client_tcp_recovery.go`,
`transfer.go`, `transfer_recovery_selection.go`, `ip.go`,
`ip_tcp_return_recovery.go`, and `tun.go`. Server fixture entry points include
`connect/perfvar/topology_test.go` and `bridge_send_tracker_test.go`.

The failing root is `TestFullTunH1NativeRapidBrowsingPressure100Pages` in
`server/connect/perfvar`. Its current fixture/fixes require the private overlay;
checking out only canonical HEAD does not reconstruct this experiment. Exact
overlay, recipe, source hashes, raw logs and joined JSON reports are referenced
by these local evidence receipts:

- Uninstrumented full tunnel:
  `/Volumes/data/urnetwork-strict-lane0-syn-v2.vinyoc/OUTCOME.v1.json`
- Instrumented full tunnel:
  `/Volumes/data/urnetwork-browsing-item-lineage100-prep.ynVfVT/OUTCOME.v1.json`
  (SHA-256 `a437999c9122bec888bb79c2cac1ff6751194a154544802e9bed28aed2c086da`).
- Direct/native controls and full 100-page result:
  `/Volumes/data/urnetwork-native-browsing-direct-null-controls-v1.jSZw5a/ROOT-OUTCOME.v1.json`
  (SHA-256 `716fef7aae32a49f8fa43b6a38bbe076efd7c8d3ca3e4f041618d7ff488e5294`).
- Return-timer deterministic pre-fix reproduction:
  `/Volumes/data/urnetwork-return-clock-root.eCgiNb/old-normal.jsonl` and
  `old-race.jsonl`; exact root
  `TestTcpReturnRecoveryAdmissionWaitDoesNotSpendNextRetryInterval`, failure
  marker `TCP_RETURN_ADMISSION_AGED_RETRY`. Frozen test/overlay manifest:
  `/private/tmp/urnetwork-tcp-return-admission-clock-proof-v1.eC6wT2/FREEZE.proof.v1.json`.
  OLD/FIX and 17-root compatibility receipt:
  `/Volumes/data/urnetwork-return-clock-root.eCgiNb/OUTCOME.v1.json`
  (SHA-256 `84cdf3109d0fd4071e6424350c35c3742d797db51b7fd92f327f8edf4e7b0d74`).
- Timer-only full replay:
  `/Volumes/data/urnetwork-return-admission-core100-prep.71ija4/OUTCOME.v1.json`
  (SHA-256 `feca263bc126142085afce9199492bdf1514c72a34c03804545cae946849ffdd`).
- Combined initial/replay admission full replay:
  `/Volumes/data/urnetwork-return-initial-admission100-prep.fWJmE5/OUTCOME.v1.json`
  (SHA-256 `c778db5a2b01b71c67c38a8fd0997fd448aa141f6b5774c71f66662144ea5213`).
- Initial-admission 30-root normal/race and actual HTTP source-custody results:
  `/Volumes/data/urnetwork-return-clock-root.eCgiNb/INITIAL-AND-SOURCE-OUTCOME.v1.json`
  (SHA-256 `ab2b1110dbea442ef6d7919e4ddf6644b22968a90bbf1f90d02937b003282f27`).
- Provider zero-window persist-gap reproduction:
  `/Volumes/data/urnetwork-return-clock-root.eCgiNb/ZERO-WINDOW-PROOF-OUTCOME.v1.json`
  (SHA-256 `35c82f952bc27e01d7e732ff67e07aacd40af510e4f3212616f76f65b5aa1c74`).

For this host's frozen direct overlay, run from `server` with Go 1.27.2:

```sh
GOMAXPROCS=2 go test -tags=acklineagetrace -count=1 -parallel=1 -p=1 \
  -timeout=600s \
  -overlay=/private/tmp/urnetwork-native-browsing-direct-null-v1.y2zPxH/DIRECT.overlay.json \
  -run '^TestNativeBrowsingDirectNullStreaming100Pages$' ./connect/perfvar
```

Private paths are host-local, not portable dependencies. Ask for the matching
frozen overlay/afterimages before replaying elsewhere; do not approximate the
workload or silently compare a different source closure. Additional measurements
and prior hypotheses are in MEMSTEADY.md and tests/PERFVAR-MEASUREMENTS.md.

All cited full runs drain pool roots to zero and join their fixture cleanup.
That is not peak-memory qualification: 32 MiB iOS-profile and 64 MiB Android
device memory qualification remains separate and incomplete.

### Canonical regression acceptance (2026-10-21)

The tracked `server/connect/perfvar` pressure root is now
`TestFullTunH1NativeRapidBrowsingPressure100Pages`. It preserves the frozen
four-live-page workload, streaming older assets, 300-second deadline and shared
2-MiB retained retry budget per endpoint, without private production overlays.
The pre-sync canonical candidate passed the full pressure workload: 100
navigations, zero five-second stalls, maximum HTML first byte 2.71711175 s,
169.473434875 s workload elapsed (175.66 s test). All 400 asset workers and
server attempts joined; pool roots/bytes drained to zero. Exact fixture cleanup
passed and source-before/after receipts matched. No private production overlay
was used. Receipt:
`/Volumes/data/urnetwork-native100-canonical.PREP.nZ1mNs2a/OUTCOME.json`.

After that unchanged research replay, the tracked pressure root gained an
explicit latency assertion: exactly 100 completed navigations, no row at or
above the existing SDK send-timeout threshold (currently five seconds), and
present pressure evidence with zero slow navigations. Eight deterministic
variations pin the inclusive boundary, first/middle/last pages, row and aggregate
verdicts, incomplete completion and missing evidence. The eleven-root focused
cohort passes normal and race validation on Go 1.27.3. Keep acquisition delay
separate from request-written-to-first-byte delay; completion inside 300 seconds
alone is not success.

The canonical diagnostic campaign is prepared under
`/Volumes/data/urnetwork-perf-defaultoff-prep.QFE7Ee`. Its A/A configuration can
validate the candidate and characterize variability, but cannot establish an
old-versus-new performance improvement. Full correctness, physical memory
qualification and the final postmortem remain pending.

Checkpoint sync subsequently integrated newer transport/window/SDK changes
(Connect `8040392a`, Server `46f24c981`, SDK `829f2a92`). The earlier successful
snapshot is not qualification of this merged source. Its first focused retry
stopped before fixture construction because the harness required a `toolchain`
directive removed by the new Go 1.27.3 release policy. Preserve that pre-traffic
refusal separately from packet-flow failures; repair source selection while
retaining exact compiler verification before rerunning.

The repaired-selector post-sync replay subsequently passed on Go 1.27.3,
including the permanent latency assertion: 100 navigations, zero slow/stalled
rows, maximum HTML first byte 3.094035291 s, workload 179.321613750 s (test
185.58 s), and 95 overlap proofs. All 400 asset clients/server attempts joined;
pool roots/bytes drained to zero. The wrapper exited zero after exact fixture
cleanup and matching source-before/after receipts; no production overlay was
used. Evidence:
`/Volumes/data/urnetwork-native100-postmerge-fixed.PREP.vCUWKke0`.
These two passing source snapshots are not three same-source confirmations or
full-suite/memory qualification. Broader correctness and PERFVAR remain pending.

The first broader correctness replay stopped in Connect with seven failing
top-level roots (15 signatures including subcases). SDK, Windows, Proxy and
Server did not run. The same seven roots fail on the complete pre-pacing
upstream snapshot `b351f70e`; do not attribute them to the default-off change.
Terminal evidence and cleanup receipts are preserved at
`/Volumes/data/urnetwork-pacing-firstfive-terminal.MsXwLjVw`.

The two health fixtures used obsolete setup/assertions: DNS hints lacked the
immutable selection provenance now required by production, and the residence
fixture expected a sibling-gate log although an unresolved real probe callback
now holds the verdict earlier. Test-only repairs use actual DNS publication and
check exact probe custody, balanced lifecycle and unchanged timing bounds. The
15-root adjacent cohort passes three fresh processes (normal/race/normal).
The five group-recovery fixtures are still under review. A proposed pending-SYN
ABA production bug was disproved through the public retained/ACKed send path;
its private production candidate is withdrawn, not included in the fix.

A follow-up production call-site audit found no service-wait entry bypassing
`ApplicationPacing`: the only production call into
`waitForServiceReservation` is inside that explicit settings gate in
`SendSequence.writeMaybeWrappedBytes`. The standalone pacer helpers have no
other production callers. No production constructor in Connect, SDK,
Server/Connect or Server/Proxy enables `ApplicationPacing`. Route-policy and
FIFO-repair checks deliberately remain enabled independently; disabling the
additional scheduling wait must not disable their correctness protections.

The remaining winner-promotion fixture exposed a second setup distinction:
its one-slot source bound counts admitted-but-unwritten work, not merely items
still sitting in the channel. Holding the split data item before dequeue did
not free admission for the separate SYN. A private state/lifecycle trace proved
that SYN was refused at zero-timeout pack admission before the tested close
transition. The corrected fixture must let data reach its initial write, then
hold the genuinely admitted SYN at its own source boundary; it must not treat
the public batch count as proof of downstream SYN custody.
With that barrier corrected, all eight focused group roots pass in a fresh
normal process (0.659 s), including all five original failures and three
adjacent controls. Expanded race and same-final-source confirmation remain
required before the next full-suite replay.

The final group cohort subsequently passed all 105 roots in three fresh
normal/race/normal processes (0.628/2.058/0.618 s), with unchanged final source
hashes, zero skips and zero production changes. Receipt:
`/Volumes/data/urnetwork-group-cohort-diagnosis.9EGSfKig/OUTCOME.json`.

The next full correctness attempt stopped in Connect on 17 additional roots:
six DNS-affinity/dial-health/stall-conviction cases and eleven public TCP
ACK/reorder-recovery cases. Its first failure is
`TestBloombergDnsHintSkipsExternallyRejectedDonor`. These new failures are under
investigation; do not yet classify them as stale fixtures or pacing regressions.
The source audit reported no changes and no race-log deltas. SDK, Windows,
Proxy and Server again did not run. Terminal evidence and exact cleanup receipts:
`/Volumes/data/urnetwork-pacing-firstfive-final-terminal.J281YiCs`.
All 17 additional roots subsequently reproduced with the same signatures on
the complete pre-pacing `b351f70e` snapshot. The eleven TCP roots fail while
admitting the opening SYN, before their recovery/reorder stimulus. Their
factories construct an unhealthy accounting channel; a healthy-only setup
discriminator is being checked without disabling affinity or safety guards.
The six health roots also remain under repair; reproducing them upstream does
not by itself prove that each failed assertion is obsolete.
The TCP discriminator then passed all eleven unchanged roots (0.597 s) with
only `healthy=true` added to each of their two fixture factories. It did not
disable affinity, collapse prevention, ACK handling or memory accounting.
Negative eligibility controls and expanded normal/race confirmation are still
required for the final test-only repair.

### Additional retry-path regression exposed by realistic fixture health

With real closed traffic buckets proving a healthy incumbent, the updated
eight-root health cohort exposes one production failure: the silent-provider
connection completes on SYN 4 at seven seconds instead of SYN 3 at three
seconds. The other seven roots pass, with balanced pool ownership. OLD evidence:
`/Volumes/data/urnetwork-connect-health-six.wDnTj2da/contract-eight-canonical-red-v2.log`.

The suspected mechanism is separate from application pacing: the group is
marked as reusing the incumbent before silence inference runs. A successful
inference then unbinds that incumbent, but fresh race copies retain its
reuse-only flag. The final source-admission check can consequently reject the
healthy replacement because it is not the old selected exit. A narrowly scoped
private correction is under validation; no canonical production change has
been applied for this finding yet. Do not attribute the original user session
or the whole 100-page stall to this additional defect without measurement.

The private final-boundary trace confirms `affinitySynReuse=true` while the
selection is nil on both three-second replacement attempts. A private patch
clearing that flag only when `clientDialFailure` successfully unbinds the
incumbent passes the same eight-root cohort (0.652 s), including the original
three-second recovery assertion. Normal incumbent reuse still performs its
final health/age validation. The explicit real-source custody regression and
expanded race qualification are pending; this private pass is not integration
or full-suite qualification. Logs and patch:
`/Volumes/data/urnetwork-connect-health-six.wDnTj2da`.

The explicit `TestDialFailureInferredRetryUsesFreshSourceAdmission` now pins
the real downstream boundary. With the same final test source, OLD fails at
three seconds with one inferred rerace and zero replacement Pack starts. The
private fix passes all nine focused roots (0.603 s), with exactly one real
ACK-required start, route write and terminal completion at three seconds. While
its producer is held, the actual provider SYN-ACK remains buffered by the race;
release commits that same flow owner and delivers the reply without an extra
application retry. Pool ownership reconciles without leaks or violations.

The three-line correction was then integrated into canonical
`ip_remote_multi_client.go`; its SHA256 is
`3d1a8b9d0d8b8f13551741d86ce497a2bf73894c7a729096aa7b9b0751d338b2`,
identical to the tested private afterimage. It clears only the former
incumbent's reuse flag after a successful guarded unbind. No read, inference,
retry or affinity timeout changed. Expanded normal/race, full correctness,
fresh 100-page replay and PERFVAR qualification remain pending for this new
production snapshot.

### Latest qualification: a remaining connection-establishment stall

The integrated correction passed the expanded 180-root health cohort in
normal/race/normal processes; the adjacent TCP cohort passed 181 roots in each
mode. Complete Connect short coverage also passed (6,854 passes, 95 short-mode
skips). These are not substitutes for the full browsing latency contract.

The fresh canonical full100 replay on this snapshot **failed** that contract:
all 100 pages completed, but page 2 first byte took 14.115321459 s. The actual
tunnel TCP dial consumed 12.408397 s, before HTTP dispatch. There was no DNS or
TLS in this synthetic inner request. Link drops and Transfer retransmissions
increased during the interval; handoff waits/drops, blocked ACK writes and
missing-contract counters did not. Aggregate counters cannot identify the
delayed SYN/return packet, and established-data TCP counters do not account for
handshake retransmissions. Do not attribute this new failure to RR12, a pacing
formula, memory pressure or the original iPhone session without further proof.

The unchanged workload retains 100 pages, four origins/four connections, four
live pages, seed 4013, 2 MiB per-endpoint retry budgets, its 300 s application
deadline and the permanent 5 s latency assertion. All 400 client/server asset
attempts joined; pools drained to zero; source identities, all input pins and
cleanup passed. Exact retained evidence:
`/Volumes/data/urnetwork-native100-rr12.PREP.oYf0CDMJ/{OUTCOME,PAYLOAD}.json`.
A private bounded first-three-page trace of the full workload is being reviewed;
it is diagnostic instrumentation, not a performance or memory qualification.

The official full correctness replay also found a separately reproduced race
failure in `TestTheEvictionNoticeStillServesAReceiverThatEvicts`. A helper that
keeps only the last ACK may hide an earlier notice-bearing carrier; deterministic
ordering and actual-wire preservation remain to be proved before fixing or
classifying that failure. The campaign stopped; the other selected suites did
not run. Evidence and limited bootstrap measurements are recorded in
`tests/PERFVAR-MEASUREMENTS.md`.

The user additionally reports new-provider establishment difficulty on iOS from
the previous committed build. The four-root local bootstrap screen passed all
12 normal/race/normal observations, but uses controlled peers and does not prove
dynamic provider addition under active traffic or the iOS 32 MiB profile.
Discovery, registration, shared resource admission, initial real ping ACK and
first downstream delivery need their own composed qualification. Healthy-only
affinity adoption must not be confused with fresh-provider probe eligibility.

### Follow-up evidence: carrier recovery and eviction wake (2026-10-21)

The bounded first-three-page trace passed the unchanged full 100-page workload,
but page 2 reused an existing connection. It therefore did not reproduce or
resolve the failed fresh dial above. Its 85 prefix queue drops included 46 data
intervals and 39 pure ACKs; the later cumulative-ACK repair bound of 486.331 ms
applies to the 46 data intervals only. Receipt:
`/Volumes/data/urnetwork-native100-first3-trace.9b3eWSqN/OUTCOME.json`.

A separately declared composed witness kept four real old asset bodies active
(123,904 bytes), required a genuinely new HTML connection, and preserved the
same seed, link shape, two 2 MiB retry owners and 5 s latency gate. Its no-forced-
loss and one-forced-segment-loss arms passed at 3.779405 s and 0.799052 s. These
two observations are diagnostic, not a performance comparison or full100 pass.
All 36 recorder/fault controls passed normal/race/normal; source pins, owners,
pools and cleanup reconciled. Evidence:
`/Volumes/data/urnetwork-native-fresh-html-gap.4PwIqrGZ/NETWORK-OUTCOME.json`.

The no-forced-loss trace locates its delay after admission: the provider wrote
the SYN-ACK promptly, but device H1 read followed 3.264061 s later. Fourteen
naturally queue-dropped downlink data intervals formed an adjacent TCP byte gap;
cumulative ACKs repaired that gap serially and the SYN-ACK became readable
immediately after the thirteenth repair. Actual ACKs carried a SACK block beyond
the gap. The encrypted outer-byte-to-inner-message mapping is unavailable;
the timing supports carrier-level head-of-line delay rather than proving that
exact byte mapping. It does not yet prove the original 12.4 s dial's cause, an RTO
defect, or a memory shortage. A single early forced loss did not reproduce it;
larger, explicitly positioned loss and sender recovery accounting need tests.

A deterministic gVisor test now reproduces a matching recovery defect on
unchanged production source. A real endpoint sends fourteen 1,000-byte segments
plus a 258-byte suffix; one actual queued suffix SACK and the production manual-
clock reorder timer mark all fourteen holes lost. At recovery entry, cwnd is
10 but pipe remains 15, so only the forced first retransmission is emitted; the
next expected repair is absent at 125 ms. The direct pipe-accounting control
also fails (a marked-lost original is still counted), while the pre-deadline
reordering control passes. All three ran without skips, source drift or retries.
This proves the TCP accounting defect, not yet its responsibility for the
original full100 failure or any candidate's performance improvement. Candidate
checks must also cover retransmitted flight counted once, split lost remainders,
and new loss while recovery is already active. OLD receipt:
`/Volumes/data/urnetwork-rack-pipe-old-terminal.QJOden8v/receipt.json`.

The eviction correctness investigation found two distinct defects. The fixture
discarded all but its last published ACK, losing independent eviction feedback;
it now preserves each physical carrier where that feedback matters. A further
production defect queued valid eviction metadata on an already-released ACK
carrier without waking the parked sender. A deterministic single-tail test
reached that exact stranded state without advancing virtual time or relying on
another ACK. The minimal wake fix passed three new and five adjacent roots in
fresh normal/race/normal processes (24 passes, zero failures/skips). Earlier
four-tail setup failures are retained, not counted as causal reproduction: normal
gap recovery had already queued two legitimate writes. The wake fix grants no
delivery credit and does not fix the separately identified unbounded pending-
metadata list. After exact integration, the same eight roots also passed
normal/race/normal without overlays (24 passes; package times
0.687/2.211/0.591 s), with unchanged source closure and input pins. Full-suite
and original full100 qualification remain pending.
Canonical confirmation receipt:
`/private/tmp/urnetwork-eviction-wake-canonical.cSuSpnJe/OUTCOME.json`.

The low32 SDK provider witness also passed on committed production code and
current race-enabled code: six distinct carriers, five quality plus one speed,
real initial ping admission and production NAT delivery, with the incumbent
flow alive during new-provider setup. This uses local NoContract/EncryptionOff
peers and is not physical iOS, account/encryption, runtime-memory or shipping-
binary qualification. Warned quality-owner replacement at the hard window cap
remains a separate open scenario. Receipt:
`/Volumes/data/urnetwork-provider-comparison-terminal.0j2ZseoW/receipt.json`.

### TCP recovery candidate: focused verification and unchanged full100 comparison

The private three-hunk RACK candidate refreshes pipe after loss detection,
excludes known-lost transmissions from pipe, and preserves the lost flag when
splitting a segment. The available Go TCP package passed 11 roots in normal,
race, and fresh normal processes (33 passes). This is not the entire upstream
Bazel TCP suite. Receipt:
`/Volumes/data/urnetwork-rack-pipe-v2-terminal.bc56h1x5/receipt.json`.

The unchanged full100 workload then ran four fresh processes in A/B/B/A order:

| Arm | Completed / slow | Maximum navigation | Workload elapsed |
| --- | --- | --- | --- |
| A1 canonical | 100 / 0 | 2.710 s | 170.077 s |
| B1 private RACK | 100 / 0 | 3.053 s | 175.837 s |
| B2 private RACK | 100 / 0 | 3.291 s | 164.053 s |
| A2 canonical | 100 / 0 | 2.997 s | 169.392 s |

All source and input pins matched across the four arms; all workers joined,
assets completed, pools drained, and cleanup passed. These four noisy-host
observations establish neither a speed improvement nor regression safety.
Both controls passed, so they do not demonstrate elimination of the historical
14.115-second navigation failure. No baseline promotion or canonical gVisor
integration follows from this comparison. Receipt:
`/Volumes/data/urnetwork-native100-rack-abba.PREP.fO9itfR5/ABBA-OUTCOME.json`.

Scope matters: this simulator explicitly uses gVisor TCP for the outer H1
carrier; shipping H1 normally uses the native dialer. A simulator recovery fix
is not proof of resolution for the reported native iOS carrier. Actual Tun TCP
consumers require their own composed qualification.

An adjacent private recovery control failed before its intended observation:
the expected first retransmission was absent. Execution stopped immediately;
neither subsequent arm ran. This is an unexpected fixture/setup failure, not
evidence of the hypothesized retransmission-loss congestion-response defect.
Receipt:
`/Volumes/data/urnetwork-rack-safety-v3-terminal.1E7o96z7/receipt.json`.

### Controlled late-gap comparison and corrected safety control

The private late-gap instrumentation passed 16 roots in normal/race/fresh-normal
(48 passes), including real Tun accept/TCP-info ownership. OLD and candidate
then each ran a fresh no-forced-gap path and a separate 14-drop path:

| Production | No forced gap, first byte | Verified 14-drop gap, first byte |
| --- | --- | --- |
| Canonical TCP | 4.016 s | 0.726 s |
| Private RACK candidate | 0.830 s | 0.675 s |

Both fault paths proved exactly 14 adjacent original losses before the matching
SYN-ACK physical read, with no premise flags. All paths were fresh connections
with four old bodies still active; source/pins and cleanup passed. The injected
gap starts near 64 KiB, earlier than the naturally observed thin-suffix tail
loss. It did not reproduce a five-second failure and can change burst shape;
the single pair is not a causal or statistical improvement qualification.
Receipt:
`/Volumes/data/urnetwork-native-late014.PREP.7LCf4E0i/NETWORK-OUTCOME.json`.

The unexpected safety-control failure above was traced to an unprimed initial
TCP recovery guard. A separately frozen successor sends and acknowledges real
warm-up data first, without editing sender state or congestion policy. Both
forced-first controls then passed. Its actual retransmission-loss control
reached the intended RED: while recovery remained active, a lost replay was
sent again with 10 repairs, one total congestion response, unchanged cwnd 10
and ssthresh 7, and no timeout. This establishes an adjacent response gap under
the private RACK candidate, not the original full100 failure's cause. No CC
fix is qualified. Receipt:
`/Volumes/data/urnetwork-rack-safety-v31-terminal.r51MF8i7/receipt.json`.

The unchanged late-gap diagnostic was extended to six fresh processes in
OLD/FIX/FIX/OLD/OLD/FIX order. No-forced-gap first-byte times were respectively
4.016/0.830/0.572/0.811/0.635/0.592 s; forced-gap times were
0.726/0.675/0.947/0.813/0.921/0.833 s. All twelve requests passed five seconds,
all six forced gaps were valid, and source/pin, ownership and cleanup checks
passed. The original code's fast repeats are retained: the initial separation
was not stable, and no statistical speedup or baseline promotion is claimed.
Combined receipt:
`/Volumes/data/urnetwork-native-late014-repeat.PREP.BKqoXbQj/SIX-PROCESS-OUTCOME.json`.

Actual trace geometry nevertheless localizes one recovery mechanism. The slow
OLD control lost 14 adjacent tail originals with a 275-byte surviving SACK
suffix; repair scheduling stretched to 3.141 s. The first FIX control lost
15 tail originals with a 130-byte initial suffix, but scheduled its first
11 repairs together and all 15 by 0.324 s. The matched SYN-ACK's local-write
to physical-read delay was 3.303 versus 0.348 s. Aggregate edge timeout deltas
were 7 versus 0; these counters are not per-socket attribution. Natural
packetization/flight state differed, so this is supporting mechanism evidence,
not an identical-loss comparison or proof of the original 12.408 s full100
dial failure. Detailed immutable trace analysis:
`/Volumes/data/urnetwork-native-late014.PREP.7LCf4E0i/ANALYSIS-014-v1.md`.

The fast OLD repeats had earlier split gaps and larger growing SACK suffixes,
not the same isolated large tail gap. Do not discard them, or assume every
small suffix must cause a stall independent of flight/recovery state. Also,
blindly forcing 14 packets at the historical byte offset can consume the
surviving suffix when packetization changes, testing a different loss case.

### Integrated RACK accounting correction

After independent source review, the exact three-hunk candidate was integrated
into the local gVisor checkout, with the same seven deterministic roots in
`pkg/tcpip/transport/tcp/rack_pipe_test.go`. Production and test hashes match
the qualified private afterimages. The full available Go TCP package then
passed without overlays or selectors in normal/race/fresh-normal processes:
11 roots each, 33 passes, no skips or race reports, unchanged source closure,
and joined cleanup. Receipt:
`/Volumes/data/urnetwork-rack-canonical-terminal.7lzC860B/receipt.json`.

This integrates only the flight-accounting/split/owner-refresh correction.
The separate active-recovery retransmission-loss congestion-response gap is
still private research, not fixed by these three hunks. Full100 replay,
broader correctness, and native/device memory-performance qualification remain
pending; neither the historical full100 dial nor the native iOS symptom is
declared resolved by this unit result.

The first integrated-code canonical100 replay then passed without an inner
overlay: all 100 navigations, zero five-second stalls, maximum navigation
3.324 s, workload 165.428 s. All 400 asset client/server operations joined,
pooled roots/bytes drained to zero, and source/pin/fixture cleanup checks
passed. One passing replay is not a statistical/full-PERF or physical mobile
memory qualification. Receipt:
`/Volumes/data/urnetwork-native100-integrated-v2.PREP.osCr7DkU/OUTCOME.json`.

A separately frozen private congestion-response candidate now passes the
unchanged active re-loss root: two real responses, cwnd10→4, ssthresh7→4,
four actual second repairs, and no repeat cut on the same ACK. Its basic
state/field-order roundtrip and footprint tests pass; on darwin/arm64, segment
struct size stays 248 B and sender size grows 856→864 B. This is compiler
layout, not a heap allocation or physical-device peak measurement. Available
Go TCP tests also passed 15 roots normal/race/fresh-normal (45 passes), for
48 total passes including the causal and two basic roots. No source drift,
skips or race reports; all owners joined. The response candidate remains
private/unintegrated pending adjacent tests and performance comparison.
Receipt:
`/Volumes/data/urnetwork-rack-reloss-v4-terminal.PonpU8wk/receipt.json`.

### Private response candidate: adjacent coverage and four-arm replay

Six additional deterministic roots cover real Reno/Cubic re-loss, old/new
loss epochs, reorder timers, RTO/TLP entry, clone/wrap/pool metadata, and
non-RACK behavior. The adjacent cohort plus all available 21 TCP roots in
normal/race/fresh-normal processes passed 69 tests, with no skips, races or
source drift. Receipt:
`/Volumes/data/urnetwork-rack-adjacent-v41-terminal.A8OUKWBp/receipt.json`.

The unchanged canonical100 replay then ran exactly A1/B1/B2/A2, where A is
integrated flight accounting and B adds only the four frozen private response
production files. All four completed 100 pages with zero >=5s navigation
stalls. Maximum full-navigation seconds were 2.878934/2.879816/4.004223/2.875193;
workload seconds were 164.330/162.892/169.692/180.218. All 400 assets per arm
joined, pool roots/bytes drained to zero, all eight source snapshots matched,
and pin/runtime/OS cleanup passed. No retries or extra arms. Receipt:
`/Volumes/data/urnetwork-native100-v4-abba.PREP.LEacEcHy/ABBA-OUTCOME.json`
(SHA-256 `8d8a6905ed9252a94d030564b5625929b8966f0a60d8733f66efc14246ef6e28`).

These four noisy-host observations establish neither a statistical speed win
nor elimination of the historical stall. The private response candidate is
still NOT integrated: independent source review identified an unexecuted
counterexample where an ACK crossing the original recovery boundary may
cause another congestion response for newer pending loss already charged
to an epoch. A real ACK/new-flight boundary regression test is being prepared
before accepting or repairing that candidate. This concern is separate from
the integrated flight-accounting correction.

The boundary concern subsequently reproduced in a real endpoint/manual-clock
test. Actual SACKs free old-flight slots, source writes send newer data beyond
the original recovery `Last`, and the real reorder timer charges its loss.
An ACK then completes only the original prefix at the same clock instant.
The private candidate incorrectly changes congestion responses 2→3 and epoch
2→3, with cwnd4→5 and an extra forced replay, despite no new loss transition.
The causal root fails at
`RACK_CHARGED_LOSS_COUNTED_AGAIN_AT_ORIGINAL_LAST`; the positive root for
genuinely new loss after recovery completion passes. Exactly two roots ran,
one expected failure and one pass, no skips, unchanged source/pins and joined
cleanup. Receipt:
`/Volumes/data/urnetwork-rack-last-crossing-terminal.HWNwAC6W/receipt.json`.
This confirms a defect in the still-private response candidate, not causality
for the original browsing session. A correction must avoid double charging
AND use the ACK-freed slot to repair already-charged pending loss; simply
suppressing recovery is not sufficient.

The correction advances the recovery completion frontier to the current sent
frontier when a genuinely newer loss window receives an additional congestion
response. The unchanged boundary test then preserves response2/epoch2/cwnd4
and performs exactly one repair in the ACK-freed slot; its new-loss positive
control also passes. Private verification passed 71 checks (causal2, then all
23 available TCP roots normal/race/fresh-normal). An executor-only misspelled
environment key stopped before starting the first full Go command; this is
retained separately, not a product test failure or a hidden retry.

The exact tested production/test afterimages were integrated locally using
byte comparisons. Canonical available TCP package verification WITHOUT an
overlay or selector then passed 23 roots in each normal/race/fresh-normal
process: 69 passes, no failure/skip/race, stable source/pins and joined cleanup.
Private receipt:
`/Volumes/data/urnetwork-rack-v5-terminal.r0mlUxVg/receipt.json`.
This closes the reproduced recovery-boundary defect, not the original full100
causal attribution, full-suite/PERF qualification or physical memory gates.

## 2026-10-22 test-build checkpoint

The zero-window persist candidate is included in this checkpoint. A reader can
have pending bytes after all returned data is ACKed, while a refused pure
window-reopening ACK has no data retry owner. Bounded, regenerable persist
probes now recover that state without advancing stream or delivery custody.
Sixteen deterministic roots passed normal, race and fresh-normal execution
(48 passes), including the actual native Multi/Transfer admission boundary.
This proves the mechanism, not attribution of every observed browsing stall.

The pre-persist baseline completed the current realistic 100-page workload:
100 HTML responses, 400 assets drained, zero navigations at or above five
seconds, 173.626 seconds application time. The matched persist candidate arm
was canceled for this requested checkpoint before workload execution. There
is no matched performance conclusion yet. Evidence is retained privately in
`/Volumes/data/urnetwork-v5-persist-only100-prep.IDbLbm/PAIR.CHECKPOINT.v2.json`.

The full correctness run passed Connect and SDK, then failed Windows because
its complete C++ specification/mutation census exceeded Go's implicit ten-minute
package deadline. A replay reached a different test at the same deadline;
the originally named test passed alone and in the replay. The explicit
30-minute package budget preserves caller overrides, child bounds, the full
census and fail-fast behavior; deterministic old-red/fix-green checks passed.
Remaining correctness packages and full performance replay are still required.

The hosted-H1-only dual-memory harness is included for the 32 MiB iOS profile
on Android and the normal 64 MiB Android profile. Its scoped unit checks and
host-tool builds passed; native APK builds and physical measurements have not
run. Do not describe either memory profile as newly qualified or this
checkpoint as release-ready. Rebuild and rerun against the synced revisions;
pre-checkpoint source identities do not qualify post-merge source.

## 2026-10-22: six-provider SDK/native browsing screen

The private V4 screen reached the actual application workload using the public
32 MiB SDK constructor, five quality clients and one speed client, six owned
public providers, native TUN ingress and verified HTTPS/H1 origins. All 100
navigations completed, with 99 overlap proofs and 400 client/server asset
workers joined. Application time was 165.180 seconds; maximum navigation elapsed
was 2.961238 seconds, with zero navigations crossing the unchanged five-second
gate. All six providers carried body traffic. SDK teardown reported zero held
ownership and balanced root credits (63,799,648 bytes reserved and released).

This is a functional host screen, not a matched performance comparison or mobile
memory qualification. Sampled receive usage was zero: it does not prove behavior
under sustained receive-budget pressure or explain the reported browsing pause.
The next causal test must exercise actual receive ownership/admission pressure
while proving unrelated flows and recovery remain runnable.

Two setup errors were corrected privately without changing packet policy. Public
providers require a distinct provider account for the tested forward-contract
relationship. Also, installed clients and Added-provider monitor events are
independent pull views: an explicit real-admission barrier proved six clients can
coexist with five published Added events. Readiness now waits for both exact
six-provider views inside the original 180-second deadline; it still rejects
foreign ownership and never accepts missing, duplicate or unhealthy providers.
The prior failed run did not retain enough cardinality evidence to attribute its
failure specifically to this publication ordering.

V4 preserved the sole expected old-logic assertion failure and passed 48 fixture
checks plus the actual 100-page root (49 passes). Normal, race and fresh-process
controls passed without skips. All source-bound owners and monitors joined.
Receipt: `/Users/builder/Library/Caches/urnetwork-runall-owned/integrated-confirm.aZVJs0tw/sdk-six-fixture-v4.WidVDqdn/RECEIPT.json`
(SHA-256 `49f285c1b470273c45f990c4bde45d1824fa78547f6bfc575eaf556641a76827`).
Its source identity is `40ed03251f95db628ddd6d13a770ba4a9388439547a8fb1bf9b609723e8c8c19`;
later harness integration requires a fresh correctness/performance replay.
## 2026-10-23: distinct stall mechanisms and validation limits

These measurements use source closure `f74cd26556634fb80f66c18c4554a4b4490e739789ba2ddf851e589fd48e400b` and private candidate overlays. They do not qualify the eventual integrated source.

- **Shared relay idle:** the canonical proxy could close an active one-way transfer because its opposite-direction reader was idle. The shared-idle/cancellation repair passed 102 normal/race/fresh checks. A six-process A/B, B/A, A/B host screen retained all 30 benchmark results and crossed no preregistered regression threshold. Median bulk throughput improved 10.45% upload and 14.55% download; short lifecycle latency fell about 5%. These are `net.Pipe` results. In particular, its per-deadline timer allocation costs differ from OS sockets, so neither these speed gains nor the allocation savings establish real TCP/TLS or device gains.
- **Discovery wakeup:** clearing expansion backoff changed state without waking the enumerator already waiting inside that backoff. An actual enumerator regression reproduced the old failure; the wake-only repair passed 45 normal/race/fresh checks. Existing backoff, identity ownership and established flows remain part of its controls. The composed browsing test must leave a genuine provider ownership slot free and observe the inner wait; prequalifying all providers before browsing misses this condition.
- **Native Go reader admission:** actual SDK socket reads called the generic five-second batch send. A remote SYN could therefore delay a later independently serviceable local DNS packet. The old actual-socket root failed with its exact marker; the native-policy candidate passed 39 normal/race/fresh checks, including the unchanged generic blocking contract and ownership controls. The actual hosted proxy reader and binding omission checks are additional required coverage, not implied by that result. Mobile packed/descriptor ingress already used native admission; this finding does not mean those entry points still wait five seconds.
- **Proxy cancellation custody:** `Tun.ReadBatch` creates caller-owned pooled copies. Cancellation at the following activity check returned without releasing them. The actual-TUN old root leaked exactly three buffers, and the return-and-clear candidate passed 12 normal/race/fresh controls. Clearing the outer slice after a consumed batch separately removes bounded stale references.

An earlier actual churn trace returned only 1,083 of a 2,048-byte write after 15.000008 seconds, immediately before its application failure. Receive charge was near its approximately 2.18 MiB leaf limit. Retained return messages lacked callback-return receipts for roughly 5.4–6.0 seconds, but bounded trace membership could not locate that time conclusively inside or before a callback. A passing root with four retried failures is not a clean performance result. The next comparison preserves every request attempt and includes retry waits in complete-request latency.

The existing 100-page SDK screen establishes overlapping asset activity but not the full reported failure premise: all providers were ready before browsing, readers continuously drained bodies, and sampled receive pressure was zero. Stronger coverage needs a retained old asset, a fresh navigation, real admission/ownership pressure and explicit recovery evidence. Body closure alone is not proof of FIN/RST delivery or retained-credit release.

Memory evidence is likewise scoped. One hosted Android 64 MiB cell had a 37.36 MiB Go-runtime peak, a 31.94 MiB quiet peak and no cap breaches. Its Fast results were 18/19/25 Mbps, so the performance gate failed. A prior iOS-profile cell on Android peaked at 30.08 MiB below 32 MiB, but an invalid Fast child stopped that cohort. Neither is full profile qualification. Physical iOS Network Extension footprint below 50 MiB remains unmeasured.

The DoH internal TUN pump is an adjacent reachable five-second admission path; it requires its own actual-root proof. A local UDP reply that bypasses that pump is not a valid discriminator. WireGuard batch admission counts can also mislabel prefix-based diagnostics; its actual caller does not reoffer a suffix, so no duplicate-packet mechanism was established there.
