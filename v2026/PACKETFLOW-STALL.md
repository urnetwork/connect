# Rapid-browsing packet-flow stall: request for independent review

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
Its focused compilation and ten small controls passed; the full pressure run
has not yet qualified the production default-off application-pacing change.

The research root records slow navigations rather than aborting at the first
one, so a passing Go test alone is insufficient evidence of stall remediation.
Qualification must inspect the emitted observation for all 100 successful HTML
requests and zero navigations reaching the existing five-second send-timeout
threshold, in addition to completion, overlap, ownership and cleanup checks.
Keep acquisition delay separate from request-written-to-first-byte delay.
After the unchanged research replay establishes the comparison, pin this
latency requirement as a regression assertion; do not silently redefine success
as merely finishing within 300 seconds.

The canonical diagnostic campaign is prepared under
`/Volumes/data/urnetwork-perf-defaultoff-prep.QFE7Ee`. Its A/A configuration can
validate the candidate and characterize variability, but cannot establish an
old-versus-new performance improvement. Full correctness, physical memory
qualification and the final postmortem remain pending.
