# Memory-budget performance research

## 2026-10-24 UTC: revised 100-page overlap qualification

The `next-navigation-image-v2` fixture now proves an actual unfinished original
image body at every next-HTML handoff, rather than relying on historical prefix
tokens. Only its final 1 KiB waits; other bytes and assets stream. The native
pressure and shared SDK paths retain the original page sizes, four-page bound,
network shape, CSS custody, cancellation and timing contracts.

Qualification passed 15 normal and 15 race roots, with two intended behavioral
failures when restoring the old writer. One uninstrumented SDK run then passed
100 HTML / 400 assets / 99 overlaps: median TTFB 1.101654 s, p95 2.239751 s,
maximum 4.233728 s and no samples at least five seconds. All 99 handoffs had
more than 1 KiB of unfinished body, median 124,672 B; this does not identify
native queue occupancy. The checked SDK post-join usage and pending counters
returned to zero, while global unpublished-offer quiescence remains unknown.

This is one changed-workload run, not a statistical improvement or an observed
32 MiB process-memory pass. Six providers share the host process. Real-device
MEMSTEADY/PERFVAR must still qualify the 32 MiB iOS surrogate and 64 MiB Android
profiles. Production budgets, routing and the five-second engineering target
are unchanged. Measurements, preserved failure and cleanup-control details are
in [the PERFVAR measurement record](../tests/PERFVAR-MEASUREMENTS.md).

## 2026-10-24 UTC: retired-channel cache ownership and C10 follow-up

Service-failure cache keys retained retired channel pointers after flowless
removal, same-identity replacement and window shutdown. Its 4,096-entry bound
was not a bound on reference lifetime: ten-minute expiry was only visited-key
policy, with no guaranteed visit after retirement. The cleanup now prunes the
exact channel generation at those lifecycle edges and rejects late writes from
a canceled owner under the existing parent lock. Live replacements, provider
events and established-flow policy are preserved. No retired-owner map, worker,
timer, payload copy or ordinary per-packet map sweep was added.

The private current-source proof passed two compile checks, five isolated causal
controls and 600 positive executions (300 normal, 300 race; 273 distinct roots),
with no skips or race reports. This proves release of the service-cache root;
it does not measure retained bytes, establish whole-object collection, or
attribute the browsing delay to this defect. Full-suite and physical 32 MiB
iOS-surrogate / 64 MiB Android qualification remain pending.

The first fresh uninstrumented C10 confirmation completed 100 HTML and 400
assets but failed its selected five-second performance budget: page 1 first
byte was 5.597088041 s, inclusive tap duration 5.885931375 s, TCP acquisition
5.278528875 s. Every other page stayed below five seconds; SDK post-join
ownership drained to zero. The separate overlap count was 98 rather than 99;
existing rows locate the missing premise at page 28. Prefix tokens had proved
earlier streaming progress, not a body still unfinished at the next tap.
The original failure remains retained; confirmations 2 and 3 were not run.

One later instrumented observation completed 100/400/99 with page 1 at
2.720150125 s inclusive. Its complete first-phase SDK ledger covered 13 calls,
nine unmatched data/ACK calls, with a maximum 130,334 ns and no pre-tap call in
flight. Actual guard reads rejected the failed donor for the second source
connection; it reached the owned speed exit. No multi-second producer or
control-publication delay was observed in this run. This does not explain the
earlier uninstrumented interval, and the observation is neither a matched
performance comparison nor a product qualification. Unsupported global
generation joins remain unknown. Five seconds is an engineering target, not a
protocol correctness guarantee or a timer that caused the request to stop.

Evidence: `c10-retention-proof.PREP.n2vxvw2m`, proof-summary SHA-256
`aff24a0cb474c45a78b0d2ebe3bdad70f7041cefb882126a6bc0c402faa11002`,
all-joined receipt `9201a2f0d1becaeb986ddccf3541a612585272b671ec3432d3c21f1fa431bed6`.
C10 confirmation raw SHA-256
`ca6d26b45ffebafe66cf56fc9b7e8fcb9914104d427be5fe084fe16037a4ffac`;
observation raw SHA-256
`c5172146afcb5c3c323558376f5da7590a678a9e35c9a535540d94ee59916e22`.
All use checkpoint Connect `e90d59eb`, Server `f101b3190`, Tests `0efc83c`
(with the explicitly recorded private cleanup or observation overlay).
No performance baseline was promoted.

## 2026-10-24 UTC: full-window service refusal and recovery

A matched, sequential host replay used the public SDK with a 32 MiB admission
target, five quality owners, one speed owner, H1, and eight Go execution cores.
The synthetic access link was 5 Mb/s down / 1 Mb/s up with 0.5% independent loss.
Old quality exits refused the next destination while the existing speed exit
could serve it. This is the separate service-recovery case; the strict
late-provider discovery diagnostic remains unchanged.

| Measurement | C7 original | Service/health selector fix |
| --- | ---: | ---: |
| Page 0 HTML first byte | 2.190 s | 2.209 s |
| Page 1 HTML first byte | TCP dial timed out | 5.675 s |
| HTML / asset attempts | 2 / 4; page 1 failed | 100 / 400 completed |
| Overlapping navigations proved | Incomplete | 99 |
| Median HTML first byte | Not comparable | 1.136 s |
| Navigations at or above five seconds | Incomplete | 1 of 100 |

The fix lets a fresh, unpinned TCP attempt try a healthy permitted second
window before retrying exits with failures for that exact destination service.
It preserves the original fallback when no better candidate exists, fixed and
pinned routing, established flows, ownership caps, and UDP policy. The native
root-cause test fails on C7 and passes with the fix in normal, race and fresh
processes; 159 positive safety/affected checks and two candidate-health causal
controls also completed as expected.

Recovery is improved but **the unchanged five-second gate still fails**:
page 1 took 5.674793 s from the controller tap. Its TCP establishment took
5.281492 s; the remaining delay needs investigation, not a larger timeout.
The complete original 12,288-byte CSS stream kept its socket and public owner,
and asset body ownership drained. No statistical baseline promotion follows
from one pair. No fast.com throughput or real-device memory qualification is
claimed: the host process includes the providers and fixture, so its heap is
not the iOS-profile runtime footprint.

Evidence: OLD raw SHA-256
`74f603849a5d37f5d2397b369459b055f5f92bc1fa97bf9603aeb6b27fcf7e7b`;
FIX raw SHA-256
`7d185a982a0ad65aaed20b794f52a955dfb363c2ec2b672b937f4c89d5824802`.
The original OLD classifier stop (one-space versus two-space log formatting)
is retained with a separate read-only adjudication; OLD was not rerun.

## 2026-10-23: bounded discovery during a live acquisition

One instrumented SDK32 host diagnostic compared original C6 routing with the
bounded discovery-demand candidate. The original code exceeded the unchanged
five-second HTML first-byte gate; the candidate completed the same workload.

| Measurement | Original C6 | Candidate |
| --- | ---: | ---: |
| Page 0 HTML first byte | 7.199 s | 1.668 s |
| Page 1 HTML first byte | 15.649 s | 3.644 s |
| Maximum HTML first byte | 15.649 s | 3.644 s |
| Named provider API visibility to discovery-wrapper entry | 12.609 s | 0.187 s |

Both arms completed 100 HTML and 400 asset attempts with 99 overlap proofs.
Each created and closed 321 asset response bodies; 79 attempts were canceled
before a response body existed. The original 12,288-byte CSS stream retained
its source tuple and positive public flow owner through the next HTML response,
which came from the physically late fifth quality provider. The candidate used
one additional discovery read and one mint within the existing acquisition and
admitted that provider before the source's unchanged three-second SYN retry.
No memory budget, ACK policy, source retry schedule, workload or latency gate
was relaxed. The 38 focused and 186 affected roots passed normal, race and fresh
runs: 672 corrected checks in total, alongside four isolated negative controls.

The mechanism is bounded by the active acquisition, its existing vacant request
slot, at most three extra reads separated by at least one second, and at most
one demanded mint. It preserves platform retry authority, ordinary resize rest,
current source/cohort proof, IP-family eligibility and established healthy flows.
Silent eligibility observes the existing three-second inferred-failure age; it
does not itself create a failure verdict, strike or negative service record.

These are measurements from one instrumented matched diagnostic, not a promoted
performance baseline or device-memory qualification. The retained trace has
explicit omissions; reserved read and SYN records establish the narrower
admission sequence. Original page 0 spent 5.148 s in TLS, but the cumulative
retransmit/drop counters do not identify its delayed packets. Its 0.551 s TLS
span in the candidate does not establish that discovery demand caused the
improvement. The measured source clock/stamp adds 32 bytes per flow in the host
unit layout; that is not an RSS or total footprint measurement. Fresh 32 MiB
iOS-profile and 64 MiB Android-profile device runtime checks remain pending.

Evidence: `native100-level-v2.PREP.fs9t_n7q` (original raw run and separate
report-boundary assessment), `native100-level-fix-only.PREP.7bckqti7` (candidate),
and `cooperative-demand06.PREP.Isujjg` (component controls). The original parser
outcome and earlier negative evidence remain preserved.

## 2026-10-23: pump ownership at shutdown

The integrated SDK lifecycle now joins current and retired IpMux pumps before
`CloseAndWait` completes. Previously a borrowed pooled TCP/53 reply could remain
live after successful SDK completion, invalidating a teardown ownership claim.
Two old-code reproductions and 12 corrected normal/race/fresh checks qualified
the narrow fix; canonical lifecycle replay passed all 15 checks; physical replay remains pending.
`Close` stays nonjoining so callbacks can request shutdown without self-joining.

This is an ownership correction, not a new memory-budget qualification. The
required physical cohorts remain the 32 MiB iOS profile on the allowlisted
Android devices and the normal 64 MiB Android profile. No physical iPhone result
or under-50 MiB Network Extension footprint is established. The DoH ready-only pump policy and atomic upstream replacement are now
integrated after two qualified old-code failures and 39 corrected checks.
Their canonical replay and complete performance/memory qualification remain pending.

## 2026-10-23: individual device cells and packet-reader fixes

On the allowlisted Pixel, one hosted H1 Android-profile cell stayed below its
64 MiB runtime gate: peak **37.36 MiB**, quiet peak **31.94 MiB**, teardown
**30.42 MiB**, with zero breach samples. All five Wikipedia and three Fast
workloads completed, but Fast measured **18/19/25 Mbps**, below the unchanged
40 Mbps goal. This is one memory-eligible cell, not full profile qualification.
An earlier 32 MiB iOS-profile cell on Android peaked at **30.08 MiB**, but an
invalid Fast child stopped that cohort. Neither result qualifies the newly
integrated source or measures physical iOS extension footprint.

Source closure for those cells was
`f74cd26556634fb80f66c18c4554a4b4490e739789ba2ddf851e589fd48e400b`.
The subsequent packet-flow fixes need fresh qualification. In particular,
the proxy now returns every pooled packet when cancellation follows a TUN
batch read, and clears consumed batch references. Its forced three-buffer
leak reproduced before the fix; 12 normal/race/fresh controls passed afterward.
This fixes ownership rather than increasing download or connection budgets.
Keep the iOS-profile 32 MiB and Android-profile 64 MiB gates unchanged while
measuring the combined fixes. No budget redistribution or baseline promotion
is supported by these individual observations.

## 2026-10-20 checkpoint: hosted variability remains unqualified

Fresh common-source Pixel hosted-US H1+ diagnostics produced one completed
**57 Mbps** result (53.470 s), between two 90-second nonterminal timeouts
(raw displays 1.3 and 31 Mbps; neither is a valid throughput measurement).
The private SDK receive-budget redistribution was **not included**. A later
owned iOS-profile workload retained two more timeouts and one completed
**11 Mbps** result; there is no valid three-attempt median.

Provider pairing or current MAIN variability remains plausible, not proven.
The fresh default logs contain no busy-probe convictions in these intervals;
cold companion contract failures also occur during the successful run.
Do not attribute the remaining stalls to the corrected local-expiry verdict
or promote a performance baseline. At the user's direction, further hosted
comparisons are deferred and passing focused synthetic changes are checkpointed.

The owned 32-MiB replay was interrupted during quiet because observed host
disk growth exceeded its temporary resource allowance, **not** an app-memory
breach. Its retained Go-runtime peak was 31,199,264 bytes (29.75 MiB), but
quiet/teardown did not complete: this is **not memory qualification**.
The 32-MiB iOS-profile and 64-MiB Android-profile qualifications remain open;
Android cannot prove an iOS extension's total physical footprint. Approved
receipt-bound Pixel recovery completed with verified APK hashes and absent
staged credentials; the interrupted session's evidence was preserved.

The checkpoint includes the local unwritten-probe correction, confirmed by
44 deterministic roots in three fresh normal and three race processes.
Separate private local-admission, replay-namespace, CDP-observer, and budget
candidates remain unintegrated and lack device-performance qualification.
Source-only successor test recipes must not be counted as executed coverage.
Raw receipts and corrected default-log analysis are retained under
`/Volumes/data/urnetwork-h1-chronology.Y0OYTR/full-campaign-current.Fxlt45/`.

### Local correctness follow-up: probe admission refusal

The real Transfer regression also reproduced two synchronous local admission
refusals retiring a channel with written data pending. The correction now holds
an exact `false, nil` refusal without adding an error strike, inventing an ACK,
or refreshing the pending clock; owner cancellation does not judge peer health.
Unknown nonnil-error and admitted-probe timeout policies remain separate.
The same 48 roots passed normal3/race3 privately and again directly against the
integrated files, with zero failures/skips/race reports. This does not explain
the fresh Fast stalls (no such convictions were observed), qualify performance,
or complete the outstanding 32/64-MiB memory checks. Receipts are retained in
`local-admission48-postcheckpoint.20261020` and
`local-admission48-shared-confirm.20261020` under the artifact root above.

This is the working research document for converting bounded mobile memory
headroom into lower page TTFB and higher transfer speed while keeping the current
iOS v2 profile's Go runtime at or below a hard 32 MiB cap in every phase. Its subject is the allocation
of memory budgets: what each budget admits or retains, the performance mechanism
it funds, the marginal result per MiB, and how that memory is reclaimed when the
device changes roles. Update the hypotheses, measurements, and decisions here
as experiments run; `LOWBAR.md` remains the full validation history.

The central question is not "how can every limit be smaller?" It is: **given a
fixed 32 MiB iOS-profile runtime envelope, which movable bytes buy the most
useful TTFB and goodput across every live carrier?**
Reducing allocation churn creates spendable headroom; queue, root, carrier, and
topology budgets decide whether that headroom can do useful work.

Current profiles, reconfirmed 2026-10-06: iOS uses **32/32 MiB** DeviceLocal
admission/Go soft limit; normal Android uses **64/64 MiB**, with both inputs
clamped to the effective three-quarters-memory-class allowance. An unavailable
or invalid Android memory class retains the 24/24-MiB fallback. The debug-only
Android iOS surrogate is now **`ios-memory-audit-v2`**, with the independent
observed Go ceiling **32 MiB = 33,554,432 bytes**. Full PERFVAR also independently
measures normal `android` with **64/64/64 MiB** target/soft/observed-runtime
policy. It preserves the entire original iOS census and repeats it at Android's
normal policy: 280+280 observations at seven blocks, or 440+440 at eleven,
with separate profile/arm builds and profile-bound session/gate/comparison
identities. Missing normal-Android steady or performance evidence fails full
qualification; startup-only checks and iOS proxy samples cannot supply it.
Historical v1 retains its
20/32/28-MiB meaning. No historical artifact is requalified by this change.

The admission partition remains DNS 10%, shared transfer/NAT 65%, carriers 25%.
Mobile queue/window caps scale from their 24-MiB calibration and retained-byte
accounting stays enabled above it. Explicit mobile H3 accounting now also stays
enabled above 32 MiB: the full target-scaled receive window plus 1,600 KiB of
send/socket/control/application ownership forms one composed carrier claim.
Previously a 64-MiB target silently disabled those mobile accounting controls.
Desktop constructors retain their existing classification. Returned pools remain
256 KiB packet + 512 KiB large objects; GOGC remains 25 pending measurement.

These are admission and GC settings, not a guarantee that every byte of live
runtime fits those numbers. DNS singleton progress, carrier handoff overlap,
runtime metadata, returned pools, and native/OS memory require separate telemetry.

### Shared iOS / hosted-proxy low profile (2026-10-14)

The SDK now exposes `DefaultLowMemoryDeviceLocalSettings()` and the shared
`LowMemoryDeviceTargetByteCount = 32 MiB`. iOS defaults and `server/proxy`
explicitly select this per-owner policy. Merely setting a numeric target on
Linux previously left hosted devices on the desktop queue/window policy.
Normal Android's explicit 64-MiB policy remains unchanged; ordinary desktop
constructors still do not opt into the low profile.

The proxy default, `config/main/proxy.yml`, and canonical monitor expectation
change from 24 to 32 MiB. Positive configured overrides retain the low policy;
metrics report their actual targets. An intentional nondefault target therefore
requires reconciling the monitor warning, not a runtime admission waiver.
With providing disabled, the source-derived 32-MiB capacities are a 21,810,380-B
shared transfer root, 2,181,038-B Pack handoff child, and 8,388,608-B carrier
budget. Child capacities overlap, so they must not be added to their root.

This selection does **not** impose a 32-MiB server process limit, retune global
pools, enable mobile-only runtime samplers on servers, or alter separate proxy
TUN socket buffers. Those allocations and total RSS remain outside the SDK
tracked-use sum. Profile parity is not a measured total-memory guarantee.

An adjacent settings-ownership defect was fixed: constructors now clone mutable
client-settings containers before assigning owner budgets. Two deterministic
controls fail on the old implementation because caller settings are mutated
and a second construction replaces the first live owner's budget pointers.
The new tests also saturate one owner's Pack budget and verify the other owner
can still admit, release, and survive the first owner's close.

Initial focused native-host validation passed **7 SDK, 41 proxy, and 10 monitor
tests in each of normal and race-enabled fresh processes**, with no failures or
skips; proxy also has five nested checks. Old proxy/default and monitor controls
fail semantically as expected. This is one normal/race pair, not three-run
confirmation, full parent qualification, or a performance result. Smaller
queue/window caps can change throughput and latency. Fresh proxy/PERFVAR
comparisons and complete Android-device iOS32/Android64 MEMSTEADY qualification
remain required; no speed improvement, baseline promotion, or native iOS
`phys_footprint <50 MiB` result is claimed.

Current physical teardown qualification requires actual DeviceLocal cancellation
and joined shutdown, not stopped roles, AM exit or a successful `finish` status.
The test-owned native observer keeps the existing 15-second sampler cadence plus
immediate begin/cancel/join/terminal observations. Its fixed primitive ring is
sized from a 150-second lifetime (currently 16 entries); its allocation is part
of observed memory. One owner serializes reads and publication. Drops, timeouts,
export failures and incomplete joins fail. Join outside the device worker tree
and UI callbacks, then drain the original ring, release scoped Go/Java device
holders, and force a fresh terminal read. App Close stays asynchronous; do not
force GC, change the selected 32- or 64-MiB policy/rate0, subtract observer cost
or reuse old quiet rows.
After each completed native read the sole owner publishes a copied immutable
header over the never-rewritten, capacity-limited prefix of that same fixed
ring. A blocked later read or caller timeout exports only this completed prefix
as failed/unjoined evidence; it does not erase earlier highs, read mutable state,
fabricate a terminal sample or become successful after a late reader returns.

Retain four separate runtime-event scopes: `physical-memory.ndjson` device
primitives, `physical-memory-teardown.json` process-only lifecycle primitives,
`physical-memory-status.ndjson` existing GetMemoryStats calls, and the existing
`physical-diagnostics.ndjson` atomic batches' `part=memory` rows. The fourth
scope copies the actual profiling rate from the same native runtime snapshot;
it adds no native call. Gate every retained total-minus-released value against
the selected absolute ceiling: 33,554,432 bytes for iOS-v2 or 67,108,864 bytes
for normal Android. Status's separate same-snapshot MemStats Sys minus HeapReleased
also enters the peak and has its own snapshot count. Retained last-trim before/
after values and diagnostic owner-census runtime values are auxiliary retained
representations: report their values/counts/breaches separately, including
repetition and any prior-profile ambiguity. A readable high value survives an
invalid neighboring field. Never invent unique-read/independence counts or
silently assign an earlier maintenance value to the current profile. Retain
identity-bound producer-summary maxima from the existing summary file and
native envelope as separately counted, possibly duplicate auxiliary evidence;
they can be the only surviving high after append/flush failure. Emergency native
receipts are archived and never qualify. Select only one exact-identity native
stream; nonduplicate conflicting or foreign readable native values remain
auxiliary, with foreign provenance explicitly unqualified. None pads primary
or quiet counts, and a low later receipt cannot erase a known earlier high.

Unrecorded internal pressure-policy reads are outside this instrumentation;
observed sampling is not an unsampled continuous-peak guarantee. Quiet still
requires the original uninterrupted >=21 device primitives spanning >=300,000
ms; none of the other scopes or auxiliary values pads that count. The schema-4
eligible live gate binds immutable memory/status/diagnostic prefixes and counts.
Offline qualification additionally requires the exact finish/observer identity,
complete native-input and instrumentation-owner proof, normal host join, and
the fresh native terminal receipt. A failed live proof cannot be requalified.
All-phase sampler drops and every writer open/append/flush/close failure remain
sticky. Summary failure still attempts the raw native receipt independently.
New native methods/fields require fresh writer-before/after/current-source,
writer/consumer and all-ABI APK/AAR linkage attestation. Historical artifacts
stay historical; the normal Android 64/64 profile remains a separate cohort.
The signed iOS Network Extension **kernel lifetime `phys_footprint` peak must
remain strictly below 50 MiB (52,428,800 bytes)**. No current physical-iPhone
run qualifies that requirement. Android proxy and host measurements cannot do so.

Deterministic profile/allocation/overlap tests cover 32 and 64 MiB. The new host
H1 screen compares 20/32, 32/32, 28/64 and 64/64 at fixed GOGC/pools; loaded
physical profile and performance qualification remain open. Dated results below
retain their original inputs and verdicts.

### 2026-09-30 full-target profile screens and quiet-reclaim candidate

These are **host diagnostics, not PERFVAR results or physical memory passes**.
The first eight-process screen used five in-process providers and began traffic
after only five clients existed. Although every process delivered its echoes,
Android 64/64 was still forming its larger topology during both measured routes.
Those rates cannot establish a steady old/new comparison. Preserve that entire
denominator in `/tmp/urnetwork-mobile-profiles-v2.bKYkJH`; no failed qualification
was converted into a pass by dropping a cell.

The corrected screen used twelve identical in-process providers in every arm.
Before **each** measured route it required both target-scaled windows to have
their exact healthy/present client counts, without warnings, done/quarantined
channels or duplicate identities, continuously for one second. The merged
`MinSatisfied` flag alone is insufficient because its two window values merge
by OR. A timeout or sampled/terminal readiness loss invalidates the cell. The
old five-client predicate failed deterministic readiness tests; the corrected
gate passed normal/race tests and three independent fresh normal processes.

All **8/8** corrected cells passed the declared delivery/readiness checks, in
palindromic order v1, v2, prior Android, full Android, full Android, prior Android,
v2, v1. Every quality/speed route sent 64 flows × 1,024 packets × 1,200 payload
bytes. GOMAXPROCS=2, GOGC=25, returned pools=256/512 KiB, 100-ms active samples,
20 seconds natural quiet, then separately forced GC/scavenge and resume were
fixed. The observations are UDP echo payload goodput and full echo RTT over H1,
not HTTP TTFB. UI/screen-sharing host noise was recorded before every cell.

| Target / soft MiB | Ready quality + speed windows | Mean quality / speed Mb/s | Mean quality / speed p95 RTT, ms | Maximum sampled Go MiB |
|---|---|---|---|---|
| iOS v1 control 20/32 | 4 + 1 | 351.98 / 355.09 | 3.183 / 3.001 | 25.820 |
| iOS v2 32/32 | 5 + 1 | 347.29 / 354.06 | 3.176 / 3.083 | 27.008 |
| Prior Android 28/64 | 4 + 1 | 354.90 / 358.84 | 3.180 / 3.021 | 26.453 |
| Full Android 64/64 | 10 + 2 | 342.40 / 340.69 | 3.561 / 3.536 | 32.722 |

The iOS mean changes were −1.33% quality / −0.29% speed; Android changes were
−3.52% / −5.06%, with p95 RTT +12.0% / +17.0%. These two repetitions per arm
on a noisy host establish neither a speed win nor a regression bound. In
particular, larger admission did not make this workload faster. Its stop/wait
generator keeps only one 1,200-byte packet in flight per flow: it does not fill
multi-MiB transfer queues. Maximum sampled transfer-root use was 8.30%, 5.15%,
5.97%, and 3.04% respectively; the root includes a fixed 1-MiB peer-pin
reservation. Carrier use reached 25%, 18.75%, 21.43%, and 43.75% respectively,
including formation observations. Returned retention stayed at or below
512 KiB of the 768-KiB total cap. No client/provider handoff drops or runtime
GC-limiter activation were observed.

Active GC cycles were 787/788, 765/783, 767/776, and 633/632; mean GC CPU seconds
were 2.443, 2.504, 2.447, and 2.553. Mean active allocation was 2,555, 2,584,
2,563, and 2,607 MiB respectively. These include the loopback providers and
cannot be assigned to a mobile client alone. The larger Android topology
retained more live owners even though its admission limits were unsaturated.

Both full-Android cells performed one natural quiet trim, after one settled-owner
deferral, at quiet second 18 or 19. Go runtime fell from 31.41–31.78 MiB to
25.89–26.04 MiB, spending about 0.023 GC CPU seconds; returned pool retention
fell from 512 to 256 KiB. Other cells recorded no quiet trim within this short
window. The outstanding-owner guard is **not** an absolute veto: it already
allows a settled count after a two-second retry. Zero trim counts in the short
first screen therefore prove neither that guard is blocking nor that the
24-MiB reclaim threshold is appropriate for Android64.

The next single-variable candidate scales the Go-runtime quiet-reclaim trigger
above a 32-MiB soft limit to three quarters of the actual effective limit:
**Android64: 48 MiB; iOS32: unchanged 24 MiB**. Smaller-limit cases preserve
the historical 24-MiB trigger. This is not an admission cap or physical-memory
guarantee. The independent 40-MiB physical-pressure trigger, one-minute cooldown,
settled-owner guard, GOGC=25, and returned pools remain unchanged. Deterministic
boundary, no-early-trim, limit-change, latch, and independent physical-pressure
tests precede the controlled 24/48/48/24-MiB threshold comparison. That comparison
uses 60 seconds natural quiet and a natural resume **before** separately forced
collection; its result is not yet a physical qualification.

The predeclared threshold comparison completed **4/4 fresh processes**, in
24/48/48/24-MiB order, with the same Android64/64 inputs, 10+2 ready windows,
twelve providers, payload, GOGC=25 and returned-pool caps. The two binaries
differed only in the pure Go-runtime reclaim-threshold function. All routes
retained readiness and had zero client/provider handoff drops.

| Reclaim threshold | Natural forced trims, runs | 60-second quiet GC CPU seconds | Final quiet Go MiB | Natural-resume p95 / max echo RTT, ms, runs | Resume pool creations, runs |
|---|---|---|---|---|---|
| Historical 24 MiB | 1, 1 | 0.0601, 0.0707 | 28.269, 26.980 | 0.577 / 1.623; 0.964 / 2.280 | 44, 41 |
| Scaled 48 MiB | 0, 0 | 0.0242, 0.0322 | 29.004, 29.152 | 0.538 / 1.994; 0.567 / 1.606 | 0, 0 |

Quiet GC cycle counts were 3/3 versus 1/2; natural/background GC still ran in
the candidate. Returned retention at quiet end was approximately 254 KiB in
the controls and 510 KiB in the candidates, below the unchanged 768-KiB cap.
Candidate maximum sampled Go was 33.301/33.465 MiB, versus 34.160/32.832 MiB
in the controls, all below the **Android64** soft setting. These are not iOS32
samples and must never be offered to its observed-Go gate. Natural resume ran
before the separately labeled forced collection/scavenge; the latter cannot
explain the natural-resume counters.

Retain the narrowly scaled Android quiet threshold: it avoids premature forced
collection and pool rebuilding inside the expanded allowance while preserving
the iOS32 and independent physical-pressure rules. This is a maintenance-policy
decision, **not a performance-baseline promotion**. Active quality/speed means
were 339.10/347.74 Mb/s for control and 315.55/346.26 for candidate; the noisy
quality outlier prevents a speed or comparability claim. Longer physical
MEMSTEADY/PERFVAR work remains necessary. No returned-pool or GOGC tuning is
supported by this small host experiment.

Threshold receipts are under `/tmp/urnetwork-mobile-reclaim-ab.7NVpbH/`.
Results SHA-256:
`cd4268b9e9c9080960836d20a56bf33d9bca1c7218a05b36750d0d7421b4ba19`.
Control/candidate binary SHA-256:
`2bbfe40072cab42ccfaf11eb75dced0afa8e45ed035a78d8b1ead3f0c8c040e7` /
`7fa200e0278f7207c65503a166f67d8ee2a8738491e7ed735e1133629c6146a6`.
SDK/Connect Go-source manifest SHA-256:
`2db254d198a51af4f57e5e2488e0eb3ce7aeda3fcf4b691f1b5d5a9c8cd551e7` /
`a772874e4d76c9c58e0c801e482502477e35566cd090ef698fa1f8d6c8c62ee4`.
Every source, plan, overlay and binary pre/post check passed. Deterministic
threshold tests first reproduced the premature 24-MiB trim, then passed the
candidate, a race run and three independent fresh normal processes.

Corrected-screen receipts: `/tmp/urnetwork-mobile-ready-v2.dbUM40/`
contains the frozen plan, eight complete logs, owner/GC/pool observations,
`cells.json`, and exact `results.json`. Results SHA-256:
`a0045ec4464b89b45ed696ad54d4947dfd88b1946d9caa1822fb15197d761fd0`.
Binary SHA-256:
`2f8bed92b0d45c568ff99d5fe72ad306bb3cea662cd182a9716574ff1430a610`.
SDK/Connect Go-source manifest SHA-256 values:
`125b8f8235fd64e7b09faf3af2860de13b06cf37e3b9e7c24d2c3d3e2f071436` /
`a772874e4d76c9c58e0c801e482502477e35566cd090ef698fa1f8d6c8c62ee4`.
All pre/post source, overlay, plan and binary checks passed. SDK `c50e10db`
and Connect `a89ce36f` were dirty, source-attested trees, not clean revisions.
The host-only overlay changes only the mobile-platform predicate; the idle
trimmer uses that same predicate without changing production GOOS behavior.

Neither screen supplies the five-minute quiet physical gate, native/OS
footprint, a loaded Android startup/profile proof, or signed iPhone evidence.
The iOS v2 all-phase observed-Go gate remains <=33,554,432 bytes, and the actual
Network Extension kernel lifetime `phys_footprint` peak must remain strictly
<52,428,800 bytes. The latter remains unqualified until real-device testing.

### Earlier 2026-09-30 evaluation: 32-MiB Go ceiling / below-50-MiB extension footprint

Historical proposal and evidence review. Its stated 20/32 inputs and 28-MiB
current gate were superseded by v2 above; its missing physical-iPhone evidence
and requirement to measure kernel lifetime peak remain unresolved.

**Proposal only; the current 28-MiB observed gate and 20/32-MiB iOS
admission/soft inputs remain unchanged.** A 32-MiB observed Go ceiling would
be exactly 33,554,432 bytes; the separate requested Network Extension
`phys_footprint` bound is strictly below 50 MiB (52,428,800 bytes). A larger
acceptance ceiling alone changes neither allocation nor performance.

The retained physical iPhone baseline in
[`sdk/build/APPLE_EXTENSION_MEMORY_BUDGET.md`](../sdk/build/APPLE_EXTENSION_MEMORY_BUDGET.md)
is a signed Release build on iPhone 16 Pro Max / iOS 26.6, dated 2026-08-15:
12 connected samples over 55 seconds, maximum Go runtime **19.635 MiB** and
extension `phys_footprint` **20.876 MiB**, with 20/32-MiB inputs. It does not
exercise Go use near 32 MiB, sustained traffic, recovery, or current source.
The difference between those maxima is not a fixed native-overhead allowance.
On this review's read-only CoreDevice inventory, the two listed iPhone 17 Pro
entries were `shutdown`; no available physical iPhone was established. A real
signed Network Extension run is therefore an outstanding prerequisite, not
something the Android surrogate or an iOS simulator can replace.

Apple DTS reports 50-MiB packet-tunnel limits on its tested iOS 16 devices and
the same values for iOS 17/18, while explicitly requiring device/OS testing;
that is not a universal guarantee for every later or older device.
[Apple's provider-limit discussion](https://developer.apple.com/forums/thread/73148)
and [Go's soft-limit contract](https://go.dev/doc/gc-guide#Memory_limit) also
preclude treating a 32-MiB Go setting as a hard 32-MiB process footprint. Go
accounting excludes native allocations and other OS-accounted memory, and the
runtime may exceed its soft limit to make progress. Nominal 50-minus-32
headroom is not a demonstrated safety margin.

At the start of this review, `ExtensionMemoryMonitor.swift` retained only
`TASK_VM_INFO.phys_footprint` every five seconds plus lifecycle/pressure events;
that sampled maximum could miss a burst. The monitor now uses the implemented
`ExtensionMemoryFootprint` decoder in `TunnelMemoryBounds.swift` to retain
current footprint, kernel lifetime peak, and available-memory metadata from
one `task_info` call. Returned counts and success gate each field; failures
remain explicitly unavailable, never a fabricated zero. The existing SDK
recorder still receives current footprint, not the historical kernel peak.
Its sampled peak remains distinct from the newly logged kernel value. The
installed iOS SDK exposes `ledger_phys_footprint_peak` at `TASK_VM_INFO_REV3`
and `limit_bytes_remaining` at REV4. Apple's
[XNU task-info implementation](https://github.com/apple-oss-distributions/xnu/blob/main/osfmk/kern/task.c)
reads a ledger lifetime maximum for the former. This implementation and its
failure/count/availability tests have offline validation only; no new signed
physical-iPhone extension capture is qualified. The five-second schedule is
unchanged. App-page footprint is a different process and cannot be used in its
place. Termination or a missing final sample remains a failure.

Qualification required before adopting 32 MiB: source/build-pinned signed
physical-iPhone sessions, at least three independent fresh extension processes,
actual 20/32 inputs and rate-zero instrumentation, H1/default-path page and
bulk traffic, bounded concurrency/reconnect/underlay transitions, drain, five
quiet connected minutes, and teardown. Retain paired Go/current/kernel-peak
footprint samples with phase/process identity and availability, all-sample Go
max <=33,554,432 and kernel lifetime footprint max <52,428,800, quiet p50/p95/max,
no sampler gaps/reset hidden by a new process, and no jetsam or watchdog death.
Preserve any [jetsam report](https://developer.apple.com/documentation/xcode/identifying-high-memory-use-with-jetsam-event-reports)
with its actual page size/build identity. A workload staying below 28 MiB
does not demonstrate safe use of the proposed extra 4 MiB. Existing 40-MiB
footprint-triggered quiet reclaim is only a provisional 10-MiB margin, not an
active-load hard cap; no empirically safe minimum headroom is established yet.

The proposed verifier must reject any absent/unavailable kernel peak, malformed
or incomplete phase record, process-identity change, decreasing lifetime peak,
or missing terminal observation; none becomes numeric zero or a fresh baseline.
For one fresh, identity-bound extension lifetime, require positive current
footprint <= kernel lifetime peak <52,428,800 bytes and positive observed Go
runtime <=33,554,432 bytes throughout the predeclared phases. Preserve kernel
peak even if every current sample missed the burst. Remaining-memory bytes are
advisory metadata, not an alternative threshold or evidence of safety; reported
zero and unavailable are distinct. Publish the maximum and quiet distribution,
the complete failure denominator, source/build/PID-generation binding and
cleanup/termination outcome. No offline parser is claimed to qualify an iPhone
capture until that capture provides these identity and phase fields.

Normal Android's 40-to-64-MiB process-limit change is separate. Its DeviceLocal
target remains 28 MiB, and the iOS audit profile remains 20/32. Focused profile,
memory-class clamp, HTTP/WebSocket-boundary tests and one Galaxy startup pass;
the host-only measurements below show no meaningful H1 improvement. Loaded
physical memory, GC cost and device performance remain unqualified.
`Sdk.setMemoryLimit` also changes constructor
sizing through `connect.SetMemoryBudget` (40/64 to 64/64 scale), so using that
API is not solely a GC change. Current mobile returned-buffer pools remain
capped at 256/512 KiB at either limit. Historical 64/20 Android peaks of
43.16/50.94 MiB had unequal workloads and older source; they motivate a fresh
40/64 comparison, not a current causal claim. Require actual effective limit
after the Android memory-class clamp, H1 TTFB/goodput/GC-cost evidence, whole-app
memory/pressure/recovery observations, and unchanged iOS-profile isolation
before claiming a performance or loaded-memory benefit from the larger allowance.

### 2026-09-30 Android 64-MiB host screen and startup-policy proof

This is **non-PERFVAR host diagnostic evidence, not a device-speed win or a
physical memory qualification**. The opt-in SDK test
`TestDeviceLocalH1AndroidMemoryExperiment` ran six fresh processes in fixed
order **40/40, 40/64, 64/64, 64/64, 40/64, 40/40** (process constructor-sizing /
Go soft limit, MiB). Every arm kept target 28 MiB, GOGC 25, two Go cores, the
same mobile-policy-only host overlay and five in-process providers. Each
quality/speed route echoed 64 flows × 256 packets × 1,200 payload bytes over
real H1 WebSockets, followed by 12 seconds of natural quiet and a separately
labeled forced collection/scavenge and resume. No public endpoint or HTTP
request was used; latency is full echo RTT, not HTTP TTFB.

All **6/6 PASS**, with zero client/provider handoff drops. Two repetitions per
arm on an uncontrolled noisy host are not a formal improvement test. Values
below are rounded; exact per-cell values are retained in `results.json`.

| Sizing/soft MiB | Mean quality / speed payload Mb/s | Maximum sampled Go MiB | 12-second quiet Go MiB, range | Active GC cycles, runs | Active GC CPU seconds, range |
|---|---|---|---|---|---|
| 40/40 | 365.76 / 395.94 | 23.148 | 20.898–22.664 | 224, 221 | 0.649–0.668 |
| 40/64 | 362.48 / 391.12 | 23.387 | 20.195–22.855 | 221, 222 | 0.658–0.677 |
| 64/64 | 365.50 / 398.55 | 23.535 | 22.023–22.597 | 220, 218 | 0.655–0.671 |

GC assist CPU ranges were 0.375–0.388, 0.377–0.394 and 0.379–0.392 seconds;
active allocation ranges were 641.272–650.476, 640.494–641.282 and
641.536–647.460 MiB respectively. The GC limiter's last-enabled cycle was zero
in every cell. Actual 64/64 versus 40/40 means were **−0.07% quality / +0.66%
speed**, noise-scale differences, not a demonstrated improvement. This load
gave no evidence that the 40-MiB soft limit was binding. Runtime values are
100-ms post-warmup samples, include loopback providers and exclude non-Go
process memory; they can miss short peaks. Twelve seconds of quiet is not the
five-minute physical steady-state gate.

Private artifacts: `/tmp/urnetwork-android64-h1.8v2Sta/{PLAN.md,handoff.json,
results.json,cell-01-40-40.log,...,cell-06-40-40.log}`. SDK HEAD `c50e10db`
and Connect HEAD `a89ce36f` had recorded source deltas, not clean trees.
The test source SHA-256 is
`ea3a044229ee492a892fbcd400b457bfeb61f6eaa5c6805704cd5f18cbd5edaa`;
binary SHA-256 `887da17dffea6f68f91ab964cb41def7fb5e56f59d9353115f495931d100305e`.
SDK/Connect source-manifest hashes and the sole mobileRuntime overlay are in
the handoff; source and binary postchecks passed. The results SHA-256 is
`966e9e897b2b7c469c8d0483067ee409f39e1e3d7bf2afb4feaad27715ffebce`.

Separately, the allowlisted Galaxy S24 Ultra passed the opt-in no-login
`AndroidMemoryStartupTest` **1/1** after fresh normal-profile SDK/app/test
builds. Actual `ActivityManager.memoryClass=256` MiB; expected and observed Go
soft limit were **67,108,864 bytes (64 MiB)**, and configured DeviceLocal target
was **29,360,128 bytes (28 MiB)**. No activity/login/credential helper, traffic,
or DeviceLocal instance was used. This proves startup policy, not a loaded
footprint or speed result. The Pixel was untouched. Exact command, APK/AAR
hashes and numeric receipt:
`/tmp/urnetwork-android64-galaxy-startup-20260930.md`. The current iOS observed
28-MiB gate and 20/32 inputs are unchanged; its 32/<50 proposal remains open.

## Physical Android device allowlist

Every MEMSTEADY device measurement is restricted to this exact allowlist:

| Role | Serial | Model |
|---|---|---|
| `device-a` | first in `android.performance_device_serials` | Pixel 8 Pro |
| `device-b` | second in `android.performance_device_serials` | Galaxy S24 Ultra |

The serials live in tests.yml (`vault/main/tests.yml`, beside
`android.unlock_code`) and are read through `tests/read-tests-config.sh`; no
repository writes them.

Preflight must require both serials in `adb devices -l` with state `device`.
Per the audit owner's 2026-09-17 instruction, ignore all other attached serials
(including unauthorized or offline entries) and leave them untouched. Only the
two allowlisted phones belong to the audit. Drivers pass each serial explicitly; a missing,
unauthorized, or offline allowlisted device invalidates the block instead of
substituting another phone. Public notes use roles; the private manifest keeps
serials for reproducibility.

Verified 2026-09-04 with `adb devices -l`: both allowlisted phones were online
and no other attached serial was admitted to the memory cohort. The earlier
rule rejecting any additional attached serial was superseded on 2026-09-17;
additional devices remain outside the cohort and do not invalidate a block.

## Scope and acceptance signals

The canonical physical campaign has two required independent profile axes.
The **iOS memory-profile proxy** uses the authoritative iOS profile in
`apple/app/extension/TunnelMemoryBounds.swift` passes a **32 MiB DeviceLocal
admission target** and a **32 MiB process/Go soft limit**. Its measured Go runtime
must never exceed **32 MiB**, including baseline, burst, drain, role transition,
and quiet recovery. These three quantities are different: the device target
sizes admission controls, the soft limit paces GC, and 32 MiB is the hard observed
runtime acceptance cap.

Normal Android builds use a 64 MiB device target / 64 MiB process soft-limit
cap, both clamped to three quarters of Android's memory class. The full campaign
requires both allowlisted phones under exact `ios-memory-audit-v2` 32/32 MiB and
normal `android` 64/64 MiB builds. The latter has its own absolute 64-MiB observed
runtime ceiling, including every baseline, burst, drain, transition, quiet and
teardown value. Clamped fallback settings do not satisfy these exact inputs.
Record each selected profile, values, source revisions/patch hashes and installed
APK hash. Every primitive/status/diagnostic/native representation must match
its expected profile, DeviceLocal target and soft limit; unknown, missing,
cross-profile or stale evidence fails. An ordinary Android-profile APK cannot
qualify the iOS axis even when its sampled runtime happens to be low, and an
iOS-profile APK cannot qualify normal Android. Comparison pairs remain separate
within each profile, never seven plus seven pooled as fourteen. Carrier roots
are respectively 8/16 MiB with 16 slots in both; target-scaled H1 claims are
256/512 KiB, transfer/NAT shares keep exact independent integer rounding, and
the existing pinned accounting and recovery-witness requirements remain.
The historical 24-MiB target budget ledger below describes earlier calibration
arms; it must not replace the current iOS v2 profile's 32-MiB admission target.

The iOS-profile Android measurements establish proxy evidence about the shared
Go runtime and budget controls, not normal Android conformance. The separate
normal-Android axis measures that policy's steady/runtime and performance gates.
Neither replaces the signed physical-iOS `phys_footprint`/jetsam gate. Version-2
physical coverage/receipts reject historical single-profile results; re-freeze
sources and obtain fresh compatible A/A/baseline evidence without reclassifying
old ledger entries. This protocol change itself claims no new physical pass.

Performance comparisons remain carrier-specific: an H1 speed result is not
H3, DNS, alt, or extender performance evidence. Memory acceptance is no longer
H1-only, however. H1, direct H3, `H3Dns`, `H3DnsPump`, API `alt h3`, API
`alt whodis`, and the TCP+TLS, QUIC, and DNS extender carriers are live
production paths and all belong to this audit. Their direct inner carriers,
alt and extender outer carriers, QUIC receive windows and unacknowledged-send
retention, socket buffers, DNS translation/combine state, packet queues,
connection races, and bounded replacement overlap must be charged before
allocation to the shared carrier budget described below. The
receive-correctness contract is also carrier-independent: every exact reliable
stream/SCTP/framed-TCP lane preserves fixed-capacity backpressure, and every
true datagram lane stays bounded and zero-wait.

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

- every iOS-profile runtime sample, including baseline, active traffic, drain,
  role transitions, and five quiet connected minutes after a burst, must stay
  <= 32 MiB; report quiet p50/p95 as well as the maximum;
- any sample above 32 MiB fails and requires allocation attribution rather than
  a larger limit; there is no grace band above the cap;
- the carrier-budget matrix below must pass at the actual 32-MiB device target and
  32-MiB process limit. A passing H1-only test or a test sized at the historical
24-MiB target cannot certify this campaign;
- every carrier graph, including an unowned NetworkSpace API/feed/probe path,
  consumes the same process-root allowance. A fallback may be refused when the
  aggregate budget is full, but it may not allocate from a separate hidden
  allowance or be blanket-disabled merely because the iOS profile is active;
- no app, VPN, instrumentation, or carrier termination; all temporary clients
  and inner/outer carrier claims must be released;
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

### Current mobile admission ledger (2026-09-30, v2)

The 32-MiB iOS DeviceLocal target splits into approximately 3.2 MiB of DNS,
20.8 MiB of shared transfer/topology ownership, and 8 MiB of platform carriers.
The 64-MiB Android target doubles each area to 6.4/41.6/16 MiB. The transfer
root is stable across role changes; its children are overlapping admission
ceilings rather than additive reservations:

| Admission owner | Provider on | Provider off |
| --- | ---: | ---: |
| DNS | 3.2 MiB | 3.2 MiB |
| Shared transfer/topology root | 20.8 MiB | 20.8 MiB |
| &nbsp;&nbsp;Client send/receive, Pack, P2P, and peer identity pins | 14.4 MiB | 20.8 MiB |
| &nbsp;&nbsp;&nbsp;&nbsp;Fixed durable peer-pin child (inside client) | 1 MiB | 1 MiB |
| &nbsp;&nbsp;Provider send/receive and P2P child | 3.2 MiB | 640 KiB control floor |
| &nbsp;&nbsp;All fallback, remote, and retiring NAT generations | 3.2 MiB | 3.2 MiB |
| Shared platform carriers | 8 MiB | 8 MiB |

The child rows deliberately do not sum to 20.8 MiB. They describe which class
may borrow idle root capacity; every live reservation is charged atomically to
both its child and the one 20.8-MiB root. A role transition may leave old client
owners draining while provider or NAT work starts, but those generations may
not escape into independent pools or overdraw the root. Integer byte rounding
is retained: iOS DNS/root/carrier are exactly 3,355,443 / 21,810,380 / 8,388,608
bytes (one target byte left); Android uses 6,710,886 / 43,620,760 / 16,777,216
bytes (two target bytes left). Android provider/NAT/client shares double, while
the fixed 1-MiB durable pin owner, 640-KiB idle provider control floor, and
process returned-buffer caps do not multiply with every child or generation.

The mobile peer-identity pin store prepays **1 MiB inside the client group**
before allocating its fixed 256-entry table, 128-KiB-plus-one input/output
owner, path, or decode/serialization scratch. This is not an extra device or
process allowance: the iOS target and process soft limit are 32 MiB, and every
accepted v2 runtime sample must remain at or below 32 MiB.
The Android profile uses the same 1-MiB leaf inside its larger root.
At most **256 peers** and **128 KiB of persisted input/serialized output** are
supported; no pin is evicted. A full store can still verify/update an existing
peer, but refuses a new signed-pin commit before opening that session's Required cipher.
This cardinality is a fail-closed mobile persistence policy, not an LRU cache.

Missing input is empty only for a genuinely absent leaf in the existing
application-private directory. Unreadable, corrupt, oversized, symbolic-link,
and nonregular input fails DeviceLocal construction before provider creation.
Darwin/iOS and Linux/Android use no-follow, nonblocking open plus identity/type
validation, so a FIFO or leaf swap cannot block admission or redirect reads.
The private parent directory must have one LocalState owner; independent
processes/LocalState instances require externally joined ownership. A prepared
replacement reloads at publication, and independently generation-gated pin
ownership also covers empty-JWT devices, preventing stale-snapshot erasure.

Verified pin and global signed-history latch commit together through a
same-directory temporary file, sync, rename, and directory sync before the
in-memory state changes. Checked capacity/persistence/closed/superseded errors
withhold Cipher and emit the bounded **local pin-store unavailable** event,
never peer-key rejection/exclusion. A healthy store preserves the existing
network-fetch availability fallback. Legacy caller-supplied/server stores
keep their prior interface and ownership contract. The device-created store
is shared unchanged with the provider and every destination generation, and
its claim releases only after joined DeviceLocal teardown.

Host allocation regressions include a full 256-max-value-pin prepare plus
activation (about **909,000 bytes / 888 KiB**), a maximum-size
whitespace-padded valid file (about **176,000 bytes**), a hostile giant-key
refusal (about **821,000 bytes**), and a full-store replacement serialization
(about **51,800 bytes** beyond the fixed owner), all within the 1-MiB claim.
Input is validated before allocation-free in-place whitespace compaction;
otherwise a padded byte-array token made the standard decoder allocate
1,217,144 bytes across preparation/activation and failed this preflight.
These are fresh-process host allocation bounds, not device-footprint evidence.
`MemoryUsed` and the `memory_device_transfer` evidence part expose named
`peer_pin_*` budget/use/reservation/release and bounded count/error counters.
Budget root/child bytes are sampled atomically; logical counters are a separate
diagnostic sample and contain no peer identities or unbounded error strings.
The flightgate verifier requires a live 1-MiB pin claim at every sample, exact
reserve-minus-release balance, containment in both client and root usage,
at most 256 retained peers, and zero pin refusal/failure counters. It does not
require post-Close logging; host lifecycle tests prove joined release balance.

The NAT child now admits the provider's complete bounded topology, not only
its socket flows. Each budgeted `LocalUserNat` prepays 256 KiB before creating
contexts, maps, protocol buffers or workers. A budgeted provider prepays
480 KiB plus 4 KiB for each supported source, capped at 16 sources per
generation (544 KiB at the mobile default):

| Provider envelope partition | Prepaid bytes |
| --- | ---: |
| Four 8-KiB IPv4/IPv6 ingress/egress fragment caches, metadata and reconstruction | 128 KiB |
| Built-in DPI (64 flows), security stats (32 destinations per result), bounded snapshot copies | 96 KiB |
| Workers, channels, registrations, primary/transient source-retirement maps | 96 KiB |
| Two synchronous TCP return-callback/item workspaces | 64 KiB |
| Independent nonblocking ingress ACK/RST decode/group workspace | 32 KiB |
| Allocator/map-growth margin | 64 KiB |
| Lifecycle, ACK evidence, diagnostics, mode/priority maps and six protocol-leaf tombstones, 16 × 4 KiB | 64 KiB |
| Required SDK packet-stats registration, atomically admitted with the provider | 1 KiB |

Thus the supported worst overlap is one fallback NAT, one retiring provider's
local NAT, one replacement provider's local NAT, and both old/new providers
with their required stats subscriptions: **3 × 256 + 2 × (544 + 1) = 1858 KiB**.
At the historical 20-MiB calibration target, it leaves **190 KiB** in the
2-MiB child for actual packet/flow admission.
The deterministic tests keep this graph live and complete a real UDP echo;
the SDK repeats the same graph and progress at the 20-MiB and 28-MiB targets
(the latter has a 2.8-MiB NAT child). These are overlapping claims against the
same root, never separate per-generation allowances.

TCP/UDP/ICMP flow envelopes, ingress packet roots, retained TCP return chunks,
asynchronous provider return items and packet/decode scratch are separately
admitted before their allocations. Each SMTP inspection flow prepays 160 KiB
for its bounded prefix, reusable TLS handshake buffer, heap-owned 8-KiB
extension bitset and metadata; concurrent inspectors cannot grow uncharged
caller stacks or allocate another flow after refusal. The NAT's 16-KiB
ACK/RST partition, provider's 64-KiB synchronous TCP return partition, and
independent 32-KiB ingress control workspace are already included in their fixed
claims, so replay pressure cannot consume the capacity needed to free replay
owners. The ingress workspace decodes one bounded ACK/RST frame at a time,
including controls in mixed Packs; a small owned copy prevents an ACK slice
from retaining its larger borrowed Transfer backing root. Additional stats registrations
claim 1 KiB each and remain charged through a captured callback after
unsubscribe.

Healthy idle source gates are reclaimed. Terminal tombstones and generations
with an unreachable-source retirement worker are not: at most 16 such source
identities and one primary plus 16 transient retirement owners can coexist
per provider. Saturation fails closed and triggers the existing SDK rotation;
it never evicts an authoritative tombstone to admit an unknown sender. The
fixed claim survives all workers, retirement owners and an outstanding
one-shot saturation handoff, including callback-initiated close. Packet-stats
callbacks use `RequestClose` for callback-safe shutdown; blocking `Close`
joins the stats worker and SDK calls it only outside the device state lock,
then merges the final post-drain counters once.

`TryNewRemoteUserNatProviderWithPacketStats` reserves the provider and required
subscription together, so a missing final 1 KiB creates no provider and cannot
self-trigger a build/retire retry loop. Capacity refusal defers construction;
permanent `NatMemoryPolicyError` for an opaque custom security factory does
not retry and releases the unused NAT. Unbudgeted/server policy factories and
configured source/queue defaults remain unchanged. Bounded construction,
source churn, dual-stack fragment pressure, SMTP concurrency, exact overlap,
callback capture and teardown have focused and race-detector regressions;
admission accounting is not a substitute for the independent runtime gate.

The 2026-09-17 host-only `TestDeviceLocalProviderMemoryUnderLoad` preflight is
still **failing**, not certified by those deterministic passes. Three candidate
fresh-process measurements peaked at 32.4, 32.5 and 32.1 MiB against the
unchanged 31-MiB Darwin ceiling. Clean detached exact-HEAD controls
(`connect` `b4aeac5b85e90821bba91a8353453115ead4cc2e`, `sdk`
`2943a312285be9d0d529622b738ceef6f98153ad`) also failed at 31.6, 32.3, 32.1
and 32.1 MiB. Both carried 208 idle / about 398 peak goroutines and about
10.3 MiB loaded heap. This six-in-process-gVisor-peer host signal therefore
has an exact-base failure and is not causal evidence of a provider-envelope
regression, but it is not a candidate pass either. Post-load sampled heap
profiles on both sides are dominated by runtime thread/goroutine allocation,
message pools and Transfer state; they show no candidate-specific provider
policy hotspot at the default sampling resolution and do not profile the
runtime peak itself. No ceiling was raised, and this check does not replace
the then-current mobile all-sample 24-MiB runtime acceptance gate.

### Historical v1 carrier calibration

The following 20/32-MiB iOS and 28/40- or 28/64-MiB Android calculations
retain their original calibration inputs. They are not the current 32/32-MiB
iOS or 64/64-MiB Android policy, and cannot qualify a current MEMSTEADY block.

The historical 5-MiB carrier share is one aggregate per-DeviceLocal ceiling,
not 5 MiB per mode or per layer. Under the 32-MiB iOS process profile, every low-memory device
carrier budget is also a child of one 8-MiB / 16-carrier-slot process root. Standalone
NetworkSpace API, feed, probe, and extender claims attach directly to that root.
Thus two devices in one process, an inner carrier plus its extender, and an
Auto race cannot each spend an independent allowance.

Historical ordinary Android's **28/64-MiB** inputs yield a **7-MiB explicit
device carrier budget / 16-MiB process-sized default carrier budget**, both
with the unchanged 16-slot cap (`memory_budget.go` uses one quarter of the
respective target here). The former **28/40 → 7/10-MiB** mapping is historical.
Current constructors return independent lifecycle-owner admission roots;
the shared-process-root description above is an older design, not the current
`SetMemoryBudget` contract. A slot admits one logical
PlatformTransport/carrier graph; it is not a raw file-descriptor count. Every
physical socket, concurrent dial candidate, and inner/outer layer in that graph
still contributes its full byte working set to the claim. Deterministic tests pin both
20/32, historical 28/40, and historical 28/64. Those comparisons used
the iOS-profile 20/32 inputs and all-sample 28-MiB runtime cap.

The 2-MiB DNS row covers DeviceLocal name-resolution/cache admission. It is not
an exemption for transport-over-DNS. `H3Dns`, `H3DnsPump`, and the DNS extender
must additionally charge their QUIC windows, UDP socket buffers, translation
combine state, fragment roots, and bounded packet queues to the 5-MiB child and
8-MiB process carrier ceilings. An extender's outer QUIC connection uses a
narrow bounded single-stream envelope; it must not silently duplicate the full
inner H3 window.

These are admission ceilings, not eager allocations or a proof that mapped
runtime fits. Runtime includes allocator spans, goroutine stacks, GC metadata,
and all simultaneously retained ownership, so the independent hard <=28-MiB
observed runtime gate applied to that historical profile. Its process GC soft
limit was 32 MiB.

### Required live-carrier budget matrix

Current qualification uses `ios-memory-audit-v2`: 32-MiB DeviceLocal admission,
32-MiB Go soft limit, and an all-sample 32-MiB observed Go ceiling on both
allowlisted Android PERF phones. The current admission ledger above is
authoritative. The older window calculations below are retained as calibration
history; the current qualification matrix and assertions follow them.

#### Historical carrier-window examples

The mobile H3 claim includes **1600 KiB of fixed retained ownership**, additive
to connection receive credit:

| H3 non-receive ownership | Bytes |
| --- | ---: |
| UDP read and write socket envelopes | 2 × 64 KiB |
| Unacknowledged STREAM/DATAGRAM send roots and ACK/loss bookkeeping | 256 KiB |
| Bounded QUIC DATAGRAM queues and descriptors | 352 KiB |
| Application datagram reassembly and replay metadata | 96 KiB |
| TLS/QUIC control, stream, packet and worker envelope | 512 KiB |
| Application routes, hybrid stream queue, batching and held-message scratch | 256 KiB |
| **Fixed total** | **1600 KiB** |

The following are owner-target values, independent of the process budget that
parents the claim. A stream's receive credit is inside its connection credit,
not an additional allocation on top of it.

| Device target | Carrier child | Inner H3 claim | Connection receive credit | Stream receive credit |
| --- | ---: | ---: | ---: | ---: |
| iOS / audit Android: 20 MiB | 5 MiB | 3072 KiB (3 MiB) | 1472 KiB | 1104 KiB |
| Historical 24 MiB | 6 MiB | 3072 KiB | 1472 KiB | 1104 KiB |
| Normal Android: 28 MiB | 7 MiB | 5184 KiB | 3584 KiB | 2688 KiB |
| Finite 32 MiB owner | 8 MiB | 5696 KiB | 4096 KiB | 3072 KiB |

Without an explicit owner, `DefaultPlatformTransportSettings()` uses the
process target, so `MemoryBudget = 32 MiB` selects the 5696-KiB claim above;
the historical 20-MiB DeviceLocal uses `WithMemoryTarget(20 MiB)` and its 3072-KiB
claim under a 5-MiB child of the same 8-MiB process root.

That historical finite-profile policy was piecewise: targets through 24 MiB
kept the 3-MiB inner claim and subtracted the fixed envelope before advertising
receive credit. Above 24 MiB through 32 MiB, the claim included the full
`target / 8` connection credit plus 1600 KiB. Unbudgeted and larger/server
profiles retained their historical receive-window policy; this finite-profile
ledger does not certify their unbounded working sets. Initial credit was also
owner-scoped: 128/256 KiB stream/connection on the finite profile and the
historical 256/512 KiB on larger owners, irrespective of process sizing.

The historical 20-MiB iOS nesting is **3072 + 1408 + 2 × 144 + 256 = 5024 KiB**:
inner H3, outer QUIC, separate inner/outer DNS translation claims, and the
pending H1 carrier. It leaves **96 KiB** of the unchanged 5-MiB child. The
historical 28-MiB Android counterpart is **5184 + 1408 + 288 + 256 = 7136 KiB**,
leaving 32 KiB of its 7-MiB child. Each live claim also draws on the process root.

The older THROUGHPUTFIX share table treated the entire H3 reservation as
receive credit. That receive-only equation is superseded on the finite mobile
surface, not restored by removing real retained owners. At the table's
200-ms carrier-loop design point and its existing framing factor, 1104 KiB
permits about **38.2 Mbit/s**, not the former 66/80-Mbit/s predictions for the
20/24-MiB targets; historical Android's 2688 KiB permits about **93.0 Mbit/s**.
These are arithmetic window bounds, not measured throughput or an acceptance
claim. Restoring the old 20-MiB connection credit while charging its fixed
owners would need a 4160-KiB inner claim and **6112 KiB** for the nested graph,
992 KiB beyond the child. These calculations used the 20-MiB device target,
32-MiB process soft limit, and <=28-MiB observed runtime gate; they do not
replace current v2 qualification.

#### Current v2 qualification matrix

The deterministic gate and the Android campaign together cover both axes
below. The extender axis is the complete 4 x 3 cross-product of inner
`H1`, `H3`, `H3Dns`, and `H3DnsPump` with outer TCP+TLS, QUIC, and DNS
carriers (12 distinct arms), not three outer-only smoke tests. A mode counts
only when it opens the intended carrier, transfers bytes, and returns every
claim on normal close, dial failure, cancellation, and role teardown.

| Path under test | Ownership that must be admitted |
| --- | --- |
| Direct H1 | Inner TLS/TCP carrier claim and socket graph |
| Direct H3 | Inner QUIC connection/stream receive windows, UDP socket buffers, and stream/packet queues |
| `H3Dns` and `H3DnsPump` | Direct-H3 ownership plus the exact translation/combine, fragment, pump, and packet-queue bounds |
| API `alt h3` | Its process-root QUIC/socket claim, bounded HTTP/3 request-stream receive and unacknowledged-send retention, and request lifecycle |
| API `alt whodis` | The `alt h3` ownership plus bounded DNS translation/combine, fragment, and packet queues, all in the same process-root claim |
| Each of `H1`, `H3`, `H3Dns`, and `H3DnsPump` through the TCP+TLS extender | The selected inner claim plus the outer extender TLS/TCP claim for its full lifetime |
| Each of `H1`, `H3`, `H3Dns`, and `H3DnsPump` through the QUIC extender | The selected inner claim plus the narrow outer extender QUIC/socket receive and unacknowledged-send bounds and 2,050-byte framed-datagram scratch |
| Each of `H1`, `H3`, `H3Dns`, and `H3DnsPump` through the DNS extender | The selected inner and outer-QUIC claims plus bounded outer DNS translation/combine, fragment, and packet queues; an inner DNS mode retains its own separate translation claim too |
| Production Auto race and carrier replacement | Every simultaneous candidate; one explicitly paired H1-involved overage loan per level, bounded by that level's H1 overlap, with up to two distinct cross-level pair identities fully reflected in child/root evidence |
| NetworkSpace API/feed/probe without a DeviceLocal owner | A direct process-root claim; it must coexist or wait/refuse under the same 8-MiB / 16-slot root rather than escaping accounting or disabling restricted-underlay fallback |

Run the matrix with the exact 32-MiB device / 32-MiB process profile, not a
larger surrogate. For every row assert all of the following:

1. The complete claim is acquired before opening a socket, allocating a QUIC
   receive window, retaining QUIC data for send/retransmission, or retaining a
   DNS/packet queue. An admission refusal opens none of them and does not
   poison another healthy dial strategy.
2. Useful bytes make progress when the graph fits. In particular, first acquire
   the inner claim, then prove each intended outer carrier still fits; testing
   an outer claim alone misses a missized composite budget.
3. Outside a handoff, child use never exceeds 8 MiB and aggregate process use
   never exceeds 8 MiB or 16 carrier slots. Each level permits at most one
   H1-involved overage loan, bounded by its own reported old/new H1 overlap
   (at most 256 KiB and one logical slot in this profile). Independent carrier
   managers may leave two distinct pairs active across the child and root;
   both identities/classes/owners must cross-match in the primary and additional
   pair evidence. An additional pair proves ownership only: never sum its bytes
   or slots into the level's permitted overage. Ownerless root pairs are explicit
   and are not attributed to a device. Inactive evidence must be zero/empty;
   missing additional-pair fields from an older diagnostic build cannot pass.
   Multiple DeviceLocals and ownerless NetworkSpace work are exercised
   concurrently so an unparented budget cannot pass unnoticed.
4. Release, preemption, wake-up, and make-before-break handoff are balanced at
   both levels under ordinary and race-detector runs. Used bytes/count return to
   the pre-arm baseline after success, failure, cancellation, and teardown.
5. A deterministic forced arm proves the reported route identity for each row,
   opens its real loopback socket/QUIC/translation graph (a claim-only fake is
   insufficient), moves payload in both directions, and closes the path. In
   particular, `alt h3`, `alt whodis`, and every supported inner-mode ×
   extender-carrier composition are separate arms; success on one is not
   evidence for another. These arms are
   the mode-coverage gate; the phone block below is the production-Auto runtime
   gate and must report only the carrier it actually used.
6. The two-phone production-Auto blocks record process-root total/used bytes and
   carrier-slot counts in every diagnostic sample, exercise both provider/client role
   assignments, transfer payload, and remain under the all-sample 32-MiB runtime
   cap. Forced-mode results are reported separately so one winning fallback
   cannot be mistaken for coverage of the other live paths.
7. Every phone sample also records the 20.8-MiB transfer/topology root and its
   client, provider, NAT, and Pack subsets in a same-timestamp joined
   `memory_device_transfer` part. Root/NAT totals must be exactly
   21,810,380 / 3,355,443 bytes (the current 32-MiB target's integer shares);
   each stays within its cap and cumulative reserved minus released bytes
   equals current use. Every child stays within its own cap and root usage;
   child values are not added together or to the root a second time. Report
   baseline/peak/end use for all five budgets. Client, provider and Pack use
   must return to the pre-burst baseline at quiet-window end. For NAT and its
   parent root, also report the quiet minima and a timestamped recovery
   witness: a continuous interval of at least 20 seconds beginning no earlier
   than 30 seconds into quiet, with root and NAT strictly below their
   pre-burst baselines and every other transfer child at or below baseline.
   Diagnostic gaps over 20 seconds break the witness. An end value above
   baseline is accepted only after such a witness, with monotone balanced
   reserve/release counters proving new NAT admissions afterward and at least
   as many new reserved bytes at the root. Root excess cannot exceed NAT
   excess. Record the witness, post-witness reserve/release deltas and actual
   endpoint values; do not label them zero or discard later runtime samples.
   This distinguishes demonstrated aggregate burst recovery followed by new
   connected traffic from monotonically retained burst ownership. It does
   not identify individual flows: raw flow logs provide attribution, and the
   deterministic ownership/lifecycle gates remain required. A pre-burst,
   drain-only, early-quiet or single-sample dip cannot justify late growth.
   All-sample runtime/admission caps, carrier end checks, temporary-client
   release, traffic and five-minute coverage gates remain unchanged.
   Historical calibration also exercised the Android 28-MiB target, where the
   same ratios scale the root to 18.2 MiB and the NAT child to 2.8 MiB. Those
   inputs are not the current normal Android 64-MiB target or the v2 surrogate.

Classify any failure before changing a limit:

- **Budget escape:** retained ownership, a socket, or a queue exists before its
  claim, after its release, or outside both the DeviceLocal child and process
  root. Add ownership/accounting and a lifecycle regression; do not hide it by
  enlarging a budget.
- **Missized budget:** the complete required graph is accounted, but a live
  production path cannot fit the 8-MiB child or the shared 8-MiB root under the
  exact 32/32 profile. Reconcile the full inner-plus-outer working set and the
  ledger together; testing either layer alone is insufficient.
- **Inefficient algorithm:** accounting is complete and within its admission
  ceilings, but actual retained/runtime memory crosses 32 MiB or useful work
  cannot progress at a reasonable rate. Remove duplication, reduce retained
  roots/topology, or change the algorithm; admission bookkeeping alone is not
  a fix.
- **Unobservable result:** route identity, child/root accounting, payload
  progress, or lifecycle balance is absent or internally inconsistent. Treat
  the arm as invalid evidence and repair telemetry before interpreting memory.

### Historical 24-MiB admission calibration

The following research ledger records earlier 24-MiB DeviceLocal calibration
and a superseded share split. Keep it as history; the current iOS proxy uses the
32-MiB v2 ledger above.

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

### Historical budget ledger

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
cancellation and lets TCP apply backpressure to the peer. The adjacent audit
found the same correctness boundary on H3/DNS QUIC streams and P2P SCTP, while
H3 DATAGRAM and native P2P remain deliberately nonblocking. This generalization
does not claim an H3/DNS performance win.

The first carrier-only candidate proved both the correction and the next
boundary. H1 carrier drops fell from 530 / 1,186,363 bytes to zero, with 25
bounded backpressure observations totaling 52,178 bytes. The unchanged 10-ms
ReceiveSequence Pack wait then dropped 24 messages (versus two in the adjacent
baseline), the reorder queue again pinned 1.994 MiB, provider recovery reached
1,544 timeout plus 564 selective writes, and fast.com displayed 3.5 Mbit/s.
This is not a reason to restore the upstream drops: it is the same synthetic
loss contract one hop later. The second candidate therefore uses the existing
negative reliable-lane handoff setting to wait until Pack capacity or
cancellation. It
does not add a slot or byte; per-sequence count/byte gates and the shared exact
2-MiB Pack budget remain the ownership ceiling. Exact H3/DNS stream and SCTP
handoffs use the same cancellation-bounded rule; H3 DATAGRAM, native P2P, and
unknown custom lanes stay zero-wait. H1 ACK handoff retains its separate compact
coalescing path.

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

### Cross-carrier remediation fast.com regression bracket

The exact-lane remediation was measured on the attached Pixel with explicit H1,
provider work disabled, a fresh authenticated process and Chrome process per
arm, two stable DevTools probes five seconds apart, cache disabled, and three
canonical fast.com runs. The order deliberately bracketed the retained
pre-change SDK AAR with the current build:

| Arm | fast.com displays | Median | Exact H1 ingress | Go runtime peak | Reliable-handoff result |
| --- | --- | ---: | ---: | ---: | --- |
| Current opening | 8.7, 6.9, 7.3 Mbit/s | 7.3 Mbit/s | 43.80 MB | 18.73 MiB | 19/19 Pack waits; zero Pack/route drops |
| Retained pre-change AAR | 84, 95, 1.2 Mbit/s | 84 Mbit/s | 223.70 MB | 19.25 MiB | 154/154 Pack waits; zero Pack/route drops |
| Current closing | 160, 130, 140 Mbit/s | 140 Mbit/s | 504.00 MB | 20.21 MiB | 5,769 route backpressures / 8,483,576 bytes; zero route drops; 1,804/1,804 Pack waits and zero Pack drops |

The public provider was not pinned across the three fresh clients, and the
opening-current plus baseline outliers expose that route variance. Do not use
the 140-versus-84 medians as a claimed product speedup. The closing current arm
is nevertheless a valid regression gate: it carried all three runs above the
40-Mbit/s target, exceeded the bracketed baseline median, moved 504.00 MB on the
H1 counter rather than leaking Direct, drained Pack/reorder use to zero, and
stayed below 24 MiB. Thus the lane remediation does not impose a systematic H1
fast.com ceiling. A precise percentage comparison still requires a pinned
provider and alternating old/current binaries on the same exit.

Keep this gate paired with queue evidence. For a future release candidate, run
at least three baseline and three current canonical samples, bracket each arm
with exact carrier counters, preserve every outlier, and reject a candidate
that cannot reach 40 Mbit/s on a path whose adjacent baseline can, or that
creates any reliable route/Pack drop. A displayed speed alone is insufficient
if traffic is not attributed to H1 or the fixed memory/ownership queues do not
drain.

The final pull added only generated IP-security and blocker data, but the
physical artifact was rebuilt and bracketed again so that result was not
assumed equivalent. On the now-degraded public route, the first three current
displays were 0.58, 27, and 52 Mbit/s (27 median); three preserved extension
samples were 17, 8.1, and 13. The retained AAR then displayed 28, 15, and 10
Mbit/s (15 median), and closing current displayed 0.65, 17, and 10 (10 median).
The current opening/retained/closing exact H1 deltas were 302.43 MB across six
runs, 147.21 MB across three, and 64.37 MB across three. Runtime peaks were
19.66, 17.70, and 19.89 MiB. All three arms had zero carrier and Pack drops;
current opening completed 165/165 Pack waits and current closing 38/38. The
same slow route also held the baseline below 40, while the candidate still
produced the only above-40 sample. Treat this as a neutral route-limited
current--baseline--current bracket, not as a replacement for the earlier
140-Mbit/s closing target gate. Its 14,057/330/6,926 timeout-resend counts expose
severe changing provider conditions and make a percentage comparison invalid.

The post-remediation server performance gate is also neutral. Before the pull,
all 210 `server/connect`, 10 PERFVAR, and 20 proxy samples passed at five
repetitions / 300 ms. After the generated-data rebase, the exact 30 affected H1
and queue-admission samples passed again. Production full-payload and ACK-sized
H1 TLS medians were 884.2 and 366.7 ns/op with unchanged 17/10 B/op and two
allocations, -1.34%/-1.69% versus the adjacent 896.2/373.0-ns cohort. The
pre-rebase PERFVAR receive-credit was 673.4 ns (+5.75%) and unchanged proxy
batch-64 was 5,444 ns (+1.80%); the small mixed directions are host noise rather
than a shared regression. Post-rebase direct hot-path benchmarks measured
reliable/unreliable fixed-queue admission at 31.17/32.67 ns and the complete
ResidentTransport admission wrapper at 40.47/38.06 ns, all with zero bytes and
zero allocations per operation. Reliable admission does not charge the ready
path for its cancellation-bounded full-queue behavior.

The deterministic root-cause gate is intentionally smaller than an Internet
benchmark and now covers the complete lane matrix:

1. Publish reliable-stream and DATAGRAM siblings under one H3 family and prove
   RouteManager returns the exact lane reliability with each message.
2. Fill a one-slot platform route. H1 and every H3/H3Dns/H3DnsPump QUIC stream
   must retain the exact second pooled message until capacity or cancellation;
   the corresponding DATAGRAM lanes must refuse promptly and return ownership.
3. Fill the ReceiveSequence handoff. H1, H3/DNS stream, P2P SCTP, and framed
   server routes use the cancellation-bounded reliable setting; H3 DATAGRAM,
   native P2P, and unknown/custom routes remain zero-wait. An artificial marker
   after a full H3 stream queue must emerge in order with no manufactured gap.
4. Split hybrid H3 without multiplying memory: the reliable route is
   unbuffered while DATAGRAM owns the existing payload queue. Stream-only H3
   retains the historical queue; the sum of route slots is unchanged.
5. Exercise P2P separately. SCTP must block only on its exact reliable route
   and return ownership on cancellation; native SRTP must keep its bounded
   nonblocking queue and counted drop. Constructor and connection updates must
   publish two immutable physical receive routes.
6. Saturate every reliable server socket/exchange boundary. It must propagate
   fixed-queue backpressure or retire the generation—never return a pooled frame
   and continue. Preserve the exact server H3 send cutoff through the gob
   exchange so only messages actually eligible for DATAGRAM consume unreliable
   flight.

The primary Connect cases are
`TestMultiRouteReaderReportsExactHybridReceiveLane`,
`TestPackHandoffTimeoutUsesExactReceiveLaneReliability`,
`TestReceiveSequenceReliableH3SaturationPreservesOrder`,
`TestPlatformTransportH3StreamLanesBackpressureWithoutDropping`,
`TestPlatformTransportH3DatagramLanesRefuseWithoutWaiting`,
`TestP2pSctpReceiveBackpressuresAndCancelsWithOwnership`, and
`TestP2pFastReceiveRefusesFullQueueWithoutWaiting`. The cutoff and memory gates
are `TestH3DatagramTransferFrameLimitMatchesLaneSelection`,
`TestTransferCarrierHybridSendCutoffClassifiesOnlyDatagramFrames`, and
`TestPlatformH3ReceiveLaneSplitKeepsOnePayloadQueue`. Run the isolated Connect
gate with:

```sh
go test . \
  -run 'Test(MultiRouteReaderReportsExactHybridReceiveLane|PackHandoffTimeoutUsesExactReceiveLaneReliability|ReceiveSequenceReliableH3SaturationPreservesOrder|PlatformH3ReceiveLaneSplitKeepsOnePayloadQueue|PlatformTransportH(1Receive.*|3(StreamLanesBackpressureWithoutDropping|DatagramLanesRefuseWithoutWaiting))|P2p(SctpReceiveBackpressuresAndCancelsWithOwnership|FastReceiveRefusesFullQueueWithoutWaiting)|H3DatagramTransferFrameLimitMatchesLaneSelection|TransferCarrierHybridSendCutoffClassifiesOnlyDatagramFrames|ProductionCarrierReadersUseModeSpecificReceiveAdmission)' \
  -count=50
```

Run the server generation/cutoff gate from the Server repository with:

```sh
go test ./connect \
  -run 'Test(SendPooledReceive.*|ReliableExchangeQueueSaturationPreservesFramedOrder|ExchangeGenerationRetiresAfterAnyUndeliveredFrame|ResidentTransportReceiveAdmissionUsesExactLane|ProductionSocketReadersDeclareExactReceiveLanes|ResidentForwardCallbackRetiresFullIngressWithoutWaiting|ExchangeHeaderUnreliableTransferGobCompatibility|ResidentTransportConstructorCarriesTransferProperties|ConnectH3TransferCarrierEnablesBoundedAckReserve)' \
  -count=50
```

Then run the SDK policy/telemetry gate from the SDK repository:

```sh
go test ./... \
  -run 'TestMobileLowMemoryClientSettingsBoundOwnership|TestTakeMemorySamplesJsonIsOneValidBatch|TestMobileDeviceMemorySampleHotPathDoesNotAllocate' \
  -count=10
```

These tests use filled one-slot queues, explicit release/cancellation barriers,
exact pooled-slice ownership, artificial sequence markers, and counters; they
need no Internet timing or scheduler race to reproduce the boundary. On a
physical H1 acceptance run, sampler schema 11 must show zero delta in
`platformH1ReceiveQueueDropCount`; a nonzero
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
remains at 1 ms. The adjacent remediation applies this Pack rule to exact
H3/DNS QUIC-stream and SCTP lanes as well; H3 DATAGRAM, native P2P, and unknown
custom carriers remain zero-wait.

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
| 2026-08-25 `h1-lossless-20260825` | Pinned controlled provider, explicit H1/eight logical lanes, fixed 64/128-KiB H1 receive policy, lossless carrier-route and Pack backpressure, unchanged exact 2-MiB shared budgets | seven-run Wikipedia load/document-TTFB/request-p95 medians 439.2/181.5/183.58 ms; 7/7 success. The fresh connection loaded in 1,019.3 ms; six reused loads were 404.6--517.0 ms. | not repeated; fast.com is the decisive bulk workload for this root cause | consecutive displays 38, 41, and 52 Mbit/s; host-adjacent Direct was 1.3 Gbit/s. The latter repeats moved about 70.2 and 73.2 MB of new provider return traffic. | active runtime peak 17.60 MiB; zero samples above 24/28 MiB; cumulative 262 / 687,039-byte carrier backpressures with zero carrier drops; 762/762 Pack waits succeeded with zero Pack drops; Pack and reorder queues drained to zero. A 345-s recovery measured runtime p50/p95/range/last 17.57/17.61/17.23--17.61/17.41 MiB, zero queued ownership, 0.50-MiB maximum retained pools, and no forced GC/trim. | **Accept the lossless H1 pipeline.** It restores the 40-Mbit/s class and fast reused-page loads by removing synthetic gaps, not by spending more depth or memory. H3/DNS performance tuning remains a future iteration; its stream-versus-DATAGRAM correctness contract is now covered separately. iOS footprint remains a separate gate. |
| 2026-08-25 `lane-remediation-20260825` / retained AAR / `lane-candidate-close-20260825` | Current–pre-change–current public explicit-H1 bracket on the attached Pixel; fresh app/client/Chrome each arm; exact H1 counters | not repeated | not repeated | opening current 8.7/6.9/7.3; retained AAR 84/95/1.2; closing current 160/130/140 Mbit/s. Exact ingress was 43.80/223.70/504.00 MB. | Go-runtime peaks 18.73/19.25/20.21 MiB. Closing current recorded 5,769 / 8,483,576-byte route backpressures, zero route drops, 1,804/1,804 Pack waits, zero Pack drops, and zero final Pack/reorder use. | **No H1 fast.com regression detected.** The closing current median was 140 Mbit/s versus the bracketed baseline's 84 Mbit/s and all three current-closing samples exceeded 40. Preserve the opening/current and baseline outliers: unpinned public-provider selection makes this a target/regression gate, not a percentage speedup claim. |
| 2026-08-25 final rebased current / retained AAR / final rebased current | Post-pull explicit-H1 current–baseline–current bracket on the same Pixel, Wi-Fi route, Chrome version, and canonical harness; fresh app/client/Chrome each arm; the pull changed only generated IP-security/blocker data | not repeated | not repeated | current primary 0.58/27/52 Mbit/s (27 median), with preserved extension 17/8.1/13; retained AAR 28/15/10 (15 median); closing current 0.65/17/10 (10 median). Exact H1 deltas were 302.43 MB over six / 147.21 MB over three / 64.37 MB over three. | Runtime peaks were 19.66/17.70/19.89 MiB with zero >28-MiB samples. Every arm had zero carrier/Pack drops; current Pack waits completed 165/165 opening and 38/38 closing. Timeout resends were 14,057/330/6,926, proving that provider conditions changed sharply across arms. | **Neutral route-limited confirmation.** Baseline was also below 40 and current produced the only above-40 result; do not infer a percentage from this degraded public route. Together with the earlier 140-Mbit/s current closing arm and pinned 38/41/52 arm, it finds no systematic fast.com regression. The final APK was restored, all three clients released, and device credentials/artifacts removed. |

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
| Reliable Pack handoff finite -> cancellation-bounded | A 1-ms arm left a rare timeout; at 10 ms, 9/11 waits succeeded. On the pinned provider, the finite boundary then dropped 24 messages and fast.com measured 3.5 Mbit/s. Waiting to capacity/cancellation produced 38/41/52 Mbit/s with 762/762 successful waits and no added slot or byte. | Keep for every exact reliable H1, H3/DNS stream, SCTP, and framed-server lane. ACK handoff remains 1 ms; H3 DATAGRAM, native P2P, and unknown custom lanes remain zero-wait. |
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

## 2026-08-26 cross-project and two-device research pass

This pass tested the eight limiter directions above on both attached Android
devices, then compared the results with the designs used by WireGuard,
wireguard-go, Tailscale, gVisor, DPDK, VPP, quic-go, and BoringTun. The useful
lesson is not that this data path should imitate a kernel VPN wholesale.
Android `VpnService` exposes one IP packet per TUN read and accepts one IP
packet per TUN write, so Linux-only TUN GSO/GRO, `sendmmsg`, kernel DCO, and
busy polling are not available at this boundary. The transferable ideas are
bounded ownership, draining all work that is already ready, fixed worker
pools, flow locality, and avoiding allocation on packet/ACK accounting paths.

### What the other implementations establish

| Project / technique | Primary-source result | Transfer to this mobile H1 path |
| --- | --- | --- |
| [WireGuard paper](https://www.wireguard.com/papers/wireguard.pdf) and [kernel-integration paper](https://www.wireguard.com/papers/wireguard-netdev22.pdf) | GSO super-packets let routing and network-stack work be reused across a packet cluster; the paper reports sending gains around 35% from batching and cache locality. WireGuard uses fixed rings and balances single-flow locality against multiflow parallelism. | Keep one ready drain together through parse/policy/Transfer admission and cache immutable per-flow decisions. Do not add a wait to create a batch. Android does not expose the Linux vnet/GSO metadata needed to create true super-packets. |
| [wireguard-go Android queue policy](https://github.com/WireGuard/wireguard-go/blob/master/device/queueconstants_android.go) and [device workers](https://github.com/WireGuard/wireguard-go/blob/master/device/device.go) | Android deliberately uses smaller fixed queues; buffers come from preallocated pools, and crypto work runs through a fixed worker set rather than one worker graph per packet. | The fixed-capacity byte gates and warm pools are directionally correct. The provider NAT's per-UDP-flow goroutines are the remaining mismatch: flow fanout scales stacks even when packet buffers are bounded. |
| [Tailscale TSO/GRO/mmsg work](https://tailscale.com/blog/throughput-improvements) and [UDP/QUIC follow-up](https://tailscale.com/blog/quic-udp-throughput) | Moving more packets per I/O produced a best-case 2.2x wireguard-go gain, up to 33% on Tailscale Linux, and a 4x UDP gain on bare metal after end-to-end offload work. | The SDK already performs one blocking TUN read, up to 63 nonblocking reads, then one `sendPacketsNoCopy` call. Provider socket reads already drain into bounded logical groups. TUN injection must remain one write per IP packet; `writev` would erase packet boundaries. |
| [gVisor buffer-pooling work](https://gvisor.dev/blog/2022/10/24/buffer-pooling/) | Netstack attributed 20--30% of processing time to allocation/GC; explicit ownership and tiered pools removed 99% of allocations and improved throughput by more than 30%. | Continue tiered exact-size pools and ownership/leak tests, but cap the returned warm floor. This pass removes the last per-ACK RTT heap node; increasing retained pool capacity is rejected because physical spikes were not caused by returned-pool bytes. |
| [DPDK ring/burst API](https://doc.dpdk.org/guides/prog_guide/ring_lib.html), [DPDK poll-mode guidance](https://doc.dpdk.org/guides/prog_guide/ethdev/ethdev.html), and [VPP vectors](https://docs.fd.io/vpp/24.02/aboutvpp/scalar-vs-vector-packet-processing.html) | Fixed rings, bulk/burst operations, run-to-completion processing, and vectors amortize atomics, queue crossings, and instruction-cache work. VPP processes vectors of up to 256 packets. | Use small ready-only bursts at every existing ownership boundary. Mobile fairness and memory bounds matter more than a datacenter-sized vector: the retained provider group remains 16 frames / 24 KiB after the 32/48 candidate failed to show a physical speed win. |
| [Linux receive/transmit scaling](https://docs.kernel.org/networking/scaling.html) | Flow-to-CPU locality reduces cache misses and queue-lock contention; software steering can instead add inter-processor interrupts. | Do not add workers merely because CPUs exist. Pin a flow to one ordered shard and increase worker count only after block/CPU profiles prove one shard saturated. The current one-shard NAT default measured neutral versus eight shards in earlier tests. |
| [quic-go flow control](https://quic-go.net/docs/quic/flowcontrol/) and [GSO notes](https://quic-go.net/docs/quic/optimizations/) | Receive windows must cover BDP, but aggregate connection limits bound memory; Linux GSO can submit up to 64 KiB. | At 40 Mbit/s and 200 ms, BDP is about 1 MiB. Existing shared resend capacity (about 9 MiB provider-off), 768-KiB per-flow logical receive, and multiple H1 flows already cover the target class. Earlier 128-depth experiments consumed more memory without increasing physical speed, so no new window spend is retained. |
| [Linux `MSG_ZEROCOPY`](https://docs.kernel.org/next/networking/msg_zerocopy.html) | Copy avoidance generally pays only above roughly 10 KiB and adds completion/ownership work. | It does not fit MTU-sized Android TUN writes or the current H1 message envelope. Exact pooled copies are cheaper and easier to bound here. Revisit only with a negotiated larger H1 envelope on a platform that exposes completion semantics. |
| [BoringTun](https://github.com/cloudflare/boringtun) | Its portable core is deliberately separate from platform tunnel/network-stack integration and is deployed on iOS and Android. | Preserve the same separation: optimize the shared Transfer core where measured, but keep Android TUN and iOS NetworkExtension constraints explicit. Android runtime measurements predict Go allocation behavior; they do not substitute for iOS `phys_footprint`. |

WireGuard's own [performance roadmap](https://www.wireguard.com/performance/)
lists GRO, lock-free queues, core autoscaling, packet locality, and fair queue
management, while warning that its published benchmark table is old. The
applicable ordering here is locality and bounded bursts first, worker scaling
second, and queue growth only after a measured saturated boundary. Linux
offloads and queue disciplines are useful controls, not directly shippable
Android changes.

### Eight-direction result

| Direction | Measurement in this pass | Decision |
| --- | --- | --- |
| 1. End-to-end hop timing | Schema 12 separates client and provider Pack, ACK-write, initial-write, timeout, and recovery counters. In the reverse physical topology the provider accumulated 7,576 timeout recovery writes while its runtime reached 30.43 MiB; the client phases stayed below 24 MiB. | Keep the primitive provider telemetry. The next timestamp work should be sampled at ownership boundaries rather than adding a clock read to every packet. |
| 2. ACK/RTO behavior | A deterministic snapshot/arrival barrier reproduced a due resend after its covering ACK was already in the coalescer. Rechecking the exact item before the recovery write yields one initial H1 wire write, zero recovery writes, and one recorded preemption. Replacing the RTT pointer heap with a fixed ring plus monotonic minimum deque changed the per-ACK median from 43.64--44.92 ns and 64 B / one allocation to 20.26--20.32 ns and 0 B / zero allocations, about 54% less time. | Keep both changes. The exact check ignores unrelated cumulative/selective progress, so a real hole cannot be postponed indefinitely. |
| 3. Provider-controlled A/B | The devices were cross-pinned by exact peer identity and alternated provider/client roles. This removed public provider selection but not the two different cellular/Wi-Fi exits. The controlled same-device campaign immediately before this pass remains the valid 40-Mbit/s proof: 38/41/52 Mbit/s with zero reliable handoff drops. | Keep exact peer pinning in the procedure. Do not convert the variable two-phone Internet results into a code speedup/regression percentage. |
| 4. Effective BDP/window | No client phase filled a memory ceiling; maxima were 18.57 MiB on the Galaxy and 23.04 MiB on the Pixel. Previous iterative 64 -> 128 depth reached its earned maximum but did not improve fast.com. | Keep H1 receive 64/128 KiB and existing shared byte budgets. Reject further static or iterative depth for production until a fixed route shows outstanding useful bytes actually capped there. |
| 5. fast.com flow distribution | Existing eight-lane work improves page parallelism, but a public provider can still put its return data on lane zero. In this exact-peer matrix, fast.com varied 0.61--7.6 Mbit/s through the tunnel while adjacent Direct Wi-Fi medians were 390 and 960 Mbit/s. | Provider-side symmetric lane deployment and per-lane useful-byte telemetry remain required. Client-only lane or queue expansion is not retained as a 40-Mbit/s fix. |
| 6. Provider return batching | For one 64-packet drain, 16 frames / 24 KiB produced four groups at a 4,959--4,981-ns median, 9,424 B, and 98 allocations. A test-only 32/48 shape produced two groups at 4,371--4,391 ns, 8,424 B, and 82 allocations: about 12% less local time. The physical matrix did not establish an end-to-end speed improvement and the larger owner increases fairness/memory exposure. | Keep the benchmark helper and production-bound regression test; retain production 16/24. A local microbenchmark win alone is insufficient to spend mobile burst memory. |
| 7. Provider IP stack | A fresh synthetic provider run moved 4.5 MiB of echoed TCP at 49.4 MiB/s and measured 30.7 MiB peak runtime / 13.6 MiB peak live heap. After 192 short UDP flows, 621 goroutines remained: 192 socket readers plus 192 per-flow send/idle loops. The physical provider similarly reached 748 goroutines. | Highest-value memory direction is a shared nonblocking UDP socket poller or bounded receive-worker set, not smaller packet pools. It is research-only until it preserves datagram order, idle/lifecycle behavior, and measured UDP latency/throughput. |
| 8. Conditional provider-off budget | Both physical client roles remained below 24 MiB without consuming the extra budget through deeper queues. Direct-versus-tunnel gaps persisted even with ample memory. | Leave unused provider-off headroom as safety margin. Spend it only on a boundary that first reports saturation and then improves a same-route A/B. |

Only the RTT representation, exact ACK-pending resend preemption, schema-12
provider attribution, and their deterministic tests remain in the candidate.
The 32/48 provider group, deeper windows, larger pools, shorter provider idle
timeouts, extra NAT shards, and zero-copy paths are not enabled in production.
The complete benchmark-only server gate then passed all 190
`server/connect`, 10 `server/connect/perfvar`, and 20 `server/proxy` samples.
The production ACK-sized H1 TLS median was 371.5 ns with 10 B and two
allocations per operation; PERFVAR receive-credit was 681.0 ns, and proxy
batch-64 was 5,526 ns. These are current-source guard values, not a detached
baseline comparison; the changed RTT path is exercised in Connect, not in the
server relay/proxy packet loops.
The complete Connect and SDK short suites passed in 198.041 and 99.190
seconds, both affected vet trees passed, and focused normal/race repetitions
passed. The Android schema parser's ten privacy/eligibility tests also passed.

The deterministic root-cause gate is:

```sh
go test . -run '^TestH1ReadyHotPathAckSuppressesEveryRecoveryWrite$' -count=100
go test -race . -run '^TestH1ReadyHotPathAckSuppressesEveryRecoveryWrite$' -count=10
go test . -run '^TestSequenceAckWindowDueDispositionIgnoresUnrelatedProgress$' -count=100
go test . -run '^$' -bench '^BenchmarkRttWindowCloseSendTime$' -benchmem -count=7
```

The first test places a cumulative ACK after the sender's empty snapshot and
before its already-due recovery write. It must observe exactly one physical
write for the H1 Pack, zero recovery writes, an empty resend queue, and exactly
one `ack_pending_resend_preempts` event. The second test proves unrelated ACK
progress does not suppress a real hole. The benchmark must stay at zero B/op
and zero allocations/op. On a physical clean H1 phase, compare counter deltas,
not cumulative totals: carrier/Pack drops and recovery errors must remain zero;
timeout writes should be explained by actual missing receiver progress, while
ACK-pending preemptions identify races safely avoided. This combination catches
both forbidden duplicate writes and an over-broad optimization that hides loss.

### Two-device physical matrix

Both devices ran the final schema-12 artifact as long-lived authenticated
sessions. Each became provider once; the other device then alternated Wi-Fi,
same-LAN P2P, and cellular. Chrome used cache-disabled DevTools navigation to
real Wikipedia pages and canonical fast.com runs. Direct controls ran after
disconnecting the tunnel. One cellular page attempt ended when Chrome closed
the DevTools WebSocket; it is recorded as a harness failure and excluded from
the stable seven-page cohort, not silently retried into that cohort.

| Provider -> client / path | Wikipedia median load / document TTFB | fast.com displays | Runtime result |
| --- | ---: | ---: | --- |
| Pixel -> Galaxy, Wi-Fi H1 | 766.3 / 182.1 ms | 0.61, 3.6, 5.0 Mbit/s | Galaxy client max 17.19 MiB |
| Pixel -> Galaxy, same-LAN P2P | 1,025.1 / 290.2 ms | 2.7 Mbit/s | Galaxy client max 18.57 MiB |
| Pixel -> Galaxy, cellular H1 | 543.1 / 242.3 ms | 4.7, 7.6, 9.4 Mbit/s | Galaxy client max 18.46 MiB |
| Galaxy -> Pixel, Wi-Fi H1 | 6,459.3 / 927.3 ms | 0.73, 0.58, 0.61 Mbit/s | Pixel client max 22.78 MiB |
| Galaxy -> Pixel, same-LAN P2P | 1,451.3 / 236.6 ms | 2.9 Mbit/s | Pixel client max 23.04 MiB |
| Galaxy -> Pixel, cellular H1 | 720.2 / 301.2 ms | 1.1, 6.5, 5.3 Mbit/s | Pixel client max 22.85 MiB |
| Pixel Direct cellular | 359.7 / 165.3 ms | 32, 34, 1.5 Mbit/s | Tunnel stopped |
| Pixel Direct Wi-Fi | 293.5 / 115.4 ms | 390, 350, 390 Mbit/s | Tunnel stopped |
| Galaxy Direct cellular | 266.0 / 96.0 ms | 1.4, 1.5, 1.5 Mbit/s | Tunnel stopped |
| Galaxy Direct Wi-Fi | 164.4 / 63.6 ms | 730, 960, 960 Mbit/s | Tunnel stopped |

The Wi-Fi controls prove both radios and fast.com targets had far more than
40 Mbit/s available. The cellular controls also show why a displayed speed
alone is not a stable cross-arm benchmark: one carrier/target cohort was only
1.5 Mbit/s even though page TTFB was 96 ms. The exact-peer tunnel results are
therefore useful failure attribution, not evidence that the previously
measured 38/41/52-Mbit/s lossless-H1 result disappeared.

Across 71 Pixel and 70 Galaxy samples, Pixel runtime peaked at 26.12 MiB with
five samples above 24 MiB and none above 28 MiB. Galaxy peaked at 30.43 MiB
with 16 samples above 24 MiB and ten above 28 MiB. Every client/P2P/cellular
phase stayed below 24 MiB; all violations were provider work:

- Pixel provider: 26.12-MiB runtime max, 11.23-MiB live-heap max, 1.32-MiB
  packet-outstanding max, and 4,458 provider timeout writes.
- Galaxy provider: 30.43-MiB runtime max, 13.99-MiB live-heap max, 1.78-MiB
  packet-outstanding max, and 7,576 provider timeout writes. At the maximum,
  returned packet-pool storage was only about 0.21 MiB while 748 goroutines
  were live.
- After provider work stopped, Galaxy first recovered to 19.89 MiB before
  automatic reclaim; its final finish sample was 16.95 MiB after one idle
  trim/forced GC. Pixel's final finish sample was 19.91 MiB with no forced GC.
  The active excess was live provider/NAT/Transfer state plus
  goroutine/allocator-span retention, not a returned-pool high-water that a
  harsher reclaim timer alone could solve.

This final physical matrix therefore fails the active provider <=24-MiB and
zero->28-MiB gates even though client mode passes. It also does not provide a
new 40-Mbit/s public-route pass. Release work must preserve the already proven
lossless-H1 throughput while replacing per-flow provider scheduling state with
bounded workers and then repeat provider-on/off/on steady recovery on both
devices. An iOS Network Extension `phys_footprint`/jetsam run remains the final
memory authority.

### Next measured research order

1. Add primitive provider TCP/UDP/ICMP flow counts and socket-worker counts to
   the sampler. Validate that the counter path allocates nothing.
2. Prototype one provider-only UDP poller/sharded event loop. Against the
   existing 192-flow fixture, require fewer than half the current 384 UDP
   flow goroutines, at least 2 MiB lower peak runtime, identical payload/order,
   and no loss in UDP requests/s or p95 latency. Keep the current design if it
   misses any condition.
3. Deploy the retained provider ACK/group/reliable-handoff code to one fixed
   exit and alternate old/current/current/old. Record useful bytes per lane,
   timeout writes, ACK-pending preemptions, and CPU per delivered MiB.
4. Add sampled stage deltas for TUN read -> client Pack -> H1 write -> server
   relay -> provider Pack -> NAT socket -> return H1 -> TUN write. Sampling
   must be deterministic and below 0.5% CPU in the local throughput benchmark.
5. Only if H1 socket writes remain limiting, negotiate a larger carrier
   envelope and sweep 4/8/16/32 KiB under the same 24-MiB gate. Do not infer a
   win from logical grouping without physical-write and device goodput deltas.
6. Profile route/policy lookup reuse across one ready burst, following
   WireGuard's once-per-cluster route-cache pattern. Retain only a cache with
   explicit invalidation and an end-to-end CPU/TTFB gain.
7. Measure scheduler locality before changing shard counts. A new worker must
   reduce block/mutex/CPU cost on both phone classes and must not increase
   active runtime or sparse request TTFB.
8. Re-run H3/DNS separately in the future iteration. Their UDP/QUIC flow
   control and datagram loss semantics differ from default H1; none of the H1
   Internet rates in this pass are H3/DNS performance evidence.

## 2026-08-26 CNN H1 poison recovery and P2P-memory follow-up

This pass used the two attached Android phones as long-lived, production-rate
(`memprofilerate=0`) sessions. Each phone used public United States H1 over
Wi-Fi and cellular, and each direction of an exact-ID same-LAN P2P pairing.
Chrome loaded the real CNN Nepal flooding live-news page and started its main
video. Both public paths and both P2P directions advanced through preroll into
news footage. A reused Chrome media session deliberately carried across an
egress change returned CNN error 1400899; a fresh page connection on the now
stable P2P route played. That is the intended boundary: never change egress IP
inside an established media connection; retire a confirmed poisoned H1
connection and let Chrome establish a healthy one.

No local client or provider security-block counter advanced during any
successful playback. Provider diagnostics now cross the complete
Connect -> SDK -> RPC/mobile boundary: availability, provider build, effective
policy hash, source-scoped ingress/egress packet and byte counters, and
publication sequence. The physical harness records only availability/count
aggregates, not provider/client identities or policy strings.

### Remediation and deterministic gate

A sustained no-receive quarantine now performs one bounded recovery action:

- erase DNS-name/address hints, app affinity and site affinity associated with
  the poisoned exit;
- make every new handshake scatter instead of inheriting a quarantined donor;
- rebind only established UDP/443 flows that can move safely;
- remove established TCP flow/path bookkeeping and synthesize a source-side
  RST so Chrome retries, rather than silently waiting on the old H1 socket;
- retain established-session egress identity until that explicit teardown;
- bound one exit's sticky graph to 64 flows, retiring only its oldest idle
  entries after the 30-second idle threshold;
- expose affinity invalidations, quarantine TCP resets and sticky retirements
  in reliability metrics.

The provider publishes identity on first authenticated source traffic and
republishes only when that source's block generation changes. Diagnostics are
strictly lower priority than a synthesized policy RST. The deterministic SMTP
test uses a one-slot paused send sequence; the RST must occupy it and the
diagnostic attempt must never displace/drop the reset.

The root-cause commands are:

```sh
go test . -run '^TestH1DashBlackholeQuarantineResetsAndRebinds$' -count=100
go test . -run '^TestFreshHandshakeNeverFollowsQuarantinedAffinityDonor$' -count=100
go test . -run '^TestFlowReaperBoundsStickyExitWithOldestIdleFlows$' -count=100
go test . -run '^TestProviderSmtpRejectionReturnsTcpReset$' -count=100
go test . -run '^(TestSecurityPolicyHashIdentifiesEffectiveRules|TestProviderDiagnosticsAreSourceScopedAndGenerationGated|TestProviderDiagnosticsFrameAndChannelOrdering)$' -count=100
go test -race . -run '^(TestH1DashBlackholeQuarantineResetsAndRebinds|TestFreshHandshakeNeverFollowsQuarantinedAffinityDonor|TestProviderSmtpRejectionReturnsTcpReset|TestProviderDiagnostics.*)$' -count=10
```

The DASH test establishes several CNN-shaped H1 media flows through one exit,
blackholes it after establishment, matures the no-receive verdict, and requires
DNS/site-affinity invalidation, prompt TCP RSTs, removal of every poisoned flow,
and a fresh handshake on the healthy exit. It fails if an established H1 flow
is silently rebound (mid-session IP change), if a new flow follows the
quarantined donor, or if recovery waits for ordinary TCP timeout.

### Physical playback and memory measurements

The sessions ran for about 27 minutes each and finished normally. Wi-Fi was
restored, both temporary authenticated clients were released, and credentials,
peer pins and device test artifacts were removed.

| Role and real workload | Go runtime | Live heap | Goroutines | Packet-pool ownership | Result |
| --- | ---: | ---: | ---: | ---: | --- |
| Pixel provider -> Galaxy P2P client, CNN playback | 38.84 MiB peak | 21.54 MiB peak | 1,346 peak | 0.39 MiB outstanding; 0.25 MiB returned | Playback completed; provider memory fails 24/28-MiB gates |
| Galaxy P2P client | 23.40 MiB p50, 23.95 MiB p95, 24.02 MiB max | 9.25 MiB peak | 209 peak | 0.006 MiB outstanding; 0.25 MiB returned | Playback completed |
| Galaxy provider -> Pixel P2P client, CNN playback | 39.26 MiB peak | 22.35 MiB peak | 1,071 peak | 1.31 MiB outstanding; 0.25 MiB returned | Playback completed; provider memory fails 24/28-MiB gates |
| Pixel P2P client | 23.46 MiB p50, 23.73 MiB p95, 23.74 MiB max | 8.35 MiB peak | 263 peak | 0.29 MiB outstanding; 0.25 MiB returned | Playback completed |
| Pixel public cellular H1, 35 samples | 23.89 MiB p50, 24.23 MiB p95, 24.39 MiB max | 8.47 MiB peak | 264 peak | 0.04 MiB outstanding; 0.25 MiB returned | The initial spinner advanced into CNN footage; zero block counters |
| Final disconnected state, Pixel / Galaxy | 23.70 / 22.38 MiB | 7.39 / 7.95 MiB | 208 / 193 | 0.25 / 0.25 MiB returned | Both below 24 MiB after teardown |

An exact-final-artifact smoke then rebuilt the SDK and APKs with provider build
`cnnfixmem-20260826-d` and repeated the real CNN page in both P2P directions.
Both phones advanced through a video advertisement and into/through moving
media. Each H1 client saw one provider diagnostic with a nonempty build and
policy hash; local client, local provider and source-scoped remote provider
block counts were all zero. Galaxy-provider -> Pixel-client moved 7.16/2.67
MiB through the provider and left provider/client runtime at 29.34/19.78 MiB;
Pixel-provider -> Galaxy-client moved 5.52/1.80 MiB and left them at
30.38/21.83 MiB. The complete 5.8-minute role-reversal sessions peaked at
32.23 MiB on Pixel and 31.16 MiB on Galaxy, with 4/11 samples over 28 MiB,
0.39/0.28 MiB maximum packet ownership, 0.25 MiB returned packet pools, and
zero packet-pressure, H1 receive-queue-drop or H1 receive-backpressure events.
This confirms the final mobile telemetry surface and playback remediation, but
also independently reproduces the provider-memory failure on a short run.

The subsequent pull brought two generated IP-security/blocker revisions, so a
new pushed-head artifact was mandatory rather than treating the prior APK as
final. Build `cnnfixmem-20260826-e` used Connect `b35e03f`, SDK `702356e`, and
Android `683d0d3e`. Fresh cache-busted CNN sessions again reversed the exact
same-network provider/client roles; both phones progressed through advertising
and into moving CNN footage. Each client saw one diagnostic with the stamped
build and the new effective-policy hash, and every local/provider block counter
remained zero. Pixel/Galaxy client snapshots were 19.09/21.98 MiB. The complete
4.1-minute sessions peaked at 32.02/31.96 MiB runtime, 17.30/17.37 MiB live
heap, and 1,032/1,066 goroutines, with 4/5 samples over 28 MiB. Packet ownership
peaked at only 0.28/0.32 MiB, returned pools at 0.25 MiB, and packet-pressure,
H1 queue-drop and H1 backpressure counters remained zero. This pushed-head run
supersedes the earlier artifact for functional release evidence and preserves
the same provider-memory failure.

A final generated IP-security/blocker revision (`8b1d9bf`) landed during
closeout, so build `cnnfixmem-20260826-f` was rebuilt from that exact policy and
run in a fresh Galaxy-provider -> Pixel-client P2P session. The real,
cache-busted CNN page loaded over explicit H1; its preroll advanced into moving
CNN flood footage. The client published one provider diagnostic with nonempty
build and effective-policy hashes, and local client, local provider, and
source-scoped remote-provider block counters were all zero. At the playback
snapshot the client/provider runtimes were 22.56/33.85 MiB. Across the complete
162/168-second instrumentation sessions they peaked at 23.31/39.41 MiB, with
zero/seven samples over 28 MiB, 210/1,392 goroutines, only 0.33/0.28 MiB maximum
packet ownership, 0.25 MiB returned packet pools, and zero packet-pressure,
H1 receive-drop, or H1 receive-backpressure events. Both tests passed, both
temporary clients were released, and private artifacts were removed. This
latest-policy check closes functional source parity; the 39.41-MiB provider
crest again fails the memory gate and reinforces per-flow/provider concurrency,
not returned pools or security drops, as the remaining streamline target.

Final-source correctness and adjacent server performance gates are green. The
complete Connect package passed in 440.220 seconds, all remaining Connect
subpackages passed, the affected blackhole/affinity/sticky/provider-diagnostic
selection passed three times under the race detector, and the complete short
SDK suite passed in 100.040 seconds. All 210/10/20 benchmark samples in
`server/connect`, `server/connect/perfvar`, and `server/proxy` passed. Current
production H1 full-payload/ACK-sized medians were 908.5/371.4 ns with unchanged
17/10 B/op and two allocations, +2.75%/+1.28% versus the adjacent pre-change
cohort. PERFVAR receive-credit improved 673.4 -> 651.0 ns (-3.33%), while proxy
batch-64 moved 5,444 -> 5,523 ns (+1.45%) with identical allocation shapes.
The small opposing host shifts and unchanged allocations do not identify a
performance regression from the recovery/diagnostic work.

After the pull, the complete benchmark tiers again passed all 210/10/20
samples. The thermally later absolute medians were 962.0/388.5 ns for
production H1 full/ACK-sized work, 664.1 ns for PERFVAR receive-credit, and
5,848 ns for proxy batch-64, with unchanged allocation shapes. To distinguish
host drift from the change, a clean parent/current/parent bracket compared
Connect parent `dacaa01` with `b35e03f` for seven 500-ms samples per benchmark.
Parent full-payload medians opened/closed at 945.8/955.1 ns; current was 956.6
ns, +0.65% versus the interpolated parent. Parent ACK-sized medians were
383.7/385.3 ns; current was 387.8 ns, +0.86%. All arms retained 17/10 B/op and
two allocations. Those sub-1% bracketed shifts are within host noise and show
no material shared-server performance regression.

The Pixel session recorded 20 samples over 28 MiB and the Galaxy 15. They were
provider-active or provider-span-recovery samples, not client-only P2P steady
state. A later Pixel public-Wi-Fi phase inherited the earlier provider arena
high-water and reached 25.63 MiB; therefore this long-session result does not
claim that switching provider off instantly restores the clean client ceiling.
It does show normal teardown eventually returning both devices below 24 MiB.

The earlier approximately 49-MB provider / 29-MB client observation is not
reproduced as an exact magnitude: this run reached about 41.2 MB provider and
25.2 MB client in decimal bytes. Real ad/media fanout changes between runs, so
that difference is not attributed to a code improvement. The important result
is directionally stable: the provider role, not returned pools and not the
client role, owns the large crest.

### What is allocated

A fresh current-source synthetic provider profile moved 4.5 MiB of echoed TCP
at 53.4 MiB/s, then left 192 short UDP flows alive. It measured 30.9 MiB peak
Go runtime, 14.8 MiB peak heap, 10.5 MiB loaded heap and 621 loaded goroutines
(179 before load). A repeat reached 31.2 MiB and tripped the deliberately tight
31-MiB host regression ceiling, confirming that this is active headroom, not a
comfortable pass. The grouped goroutine profile is decisive:

- 192 goroutines were blocked in each connected UDP socket's `Read`;
- 192 more were in each `UdpSequence.Run` send/idle loop;
- only four goroutines served the already-shared ordered receive dispatcher;
- the remainder was gVisor/Transfer/device/provider fixed work.

At a 64-KiB heap-profile sampling rate, loaded in-use attribution included
about 3.12 MiB from live message-pool acquisitions, 1.02 MiB from the bounded
warm set, 0.64 MiB in UDP reader buffers, 0.44 MiB in UDP sequence construction,
0.39 MiB in receive-dispatch structures and 0.19 MiB in the sequence run loop.
Those samples are directional rather than exact accounting, but they agree
with the topology. `messagePool.take` means a live acquisition attributed to
that allocator; it is not evidence that the returned free list owns 3.12 MiB.

The device's exact pool counter resolves that ambiguity: total returned pool
storage at the provider peaks was only 0.26--0.29 MiB. Detailed post-peak
snapshots instead showed:

- Pixel provider: 30.35 MiB runtime = 10.83 MiB live heap, 7.34 MiB stacks,
  6.06 MiB heap fragmentation, plus runtime metadata; returned packet pool
  0.25 MiB.
- Galaxy provider: 34.70 MiB runtime = 16.32 MiB live heap, 5.88 MiB stacks,
  6.67 MiB heap fragmentation, plus runtime metadata; returned packet pool
  0.25 MiB.

Therefore a harsher pool reclaim or a larger pool does not address the active
crest. Pools remain useful for hot allocation/GC cost, but the reclaimable
free list is two orders of magnitude smaller than the approximately 15-MiB
reduction needed to move a 39-MiB provider below 24 MiB. The active excess is
the combination of per-flow socket/read/send state, hundreds of goroutine
stacks, live packet/Transfer state and allocator spans created by that fanout.
Outer H1 does not eliminate this: Chrome can still create inner DNS and
UDP/443/QUIC attempts while the tunnel carrier itself is H1.

### Resolution candidates, in measured order

1. **Connected-socket readiness poller.** Keep one connected UDP socket/NAT
   port per flow, but replace its two parked goroutines with Android/Linux
   epoll and iOS kqueue readiness shards. A bounded worker drains every ready
   socket nonblocking, then performs the existing bulk callback. This preserves
   tuple identity and reply demultiplexing. A naively shared unconnected socket
   does not: two inner source ports contacting the same remote tuple become
   ambiguous. Prototype platform implementations behind the same interface.
2. **Collision-safe socket sharing.** As a harder second prototype, use a
   small socket/port set plus a remote-tuple map, falling back to a distinct
   socket whenever two inner flows would collide. This can reduce descriptors
   as well as goroutines but must prove source-port/NAT behavior, ICMP error
   attribution, IPv4/IPv6 parity and sender isolation. Do not ship it merely
   because a single-server benchmark passes.
3. **Adaptive UDP lifecycle.** Preserve the 300-second provider timeout for
   VoIP, games and long-lived UDP. Measure a short post-response timeout only
   for classifiable one-shot DNS/probe flows, and prompt retirement for a
   quarantined exit. A blanket shorter timeout is rejected: it can break a
   quiet healthy session and does not reduce memory while traffic is active.
4. **Flatten per-flow live objects.** The 192-flow heap profile gives a much
   smaller but real second target: make the send queue lazy/smaller only after
   queue-HWM telemetry proves the slots unused; keep addresses inline; avoid
   closures/timers created per flow; and isolate the socket-reader's blocking
   frame from deep callback frames. Retain a change only if p95 UDP latency and
   goodput do not regress.
5. **Fragmentation-aware construction.** Batch or arena-like ownership is safe
   only inside one flow lifecycle and with an exact release point. Measure
   size-class/span changes; do not introduce an unbounded object pool. Forced
   GC/trim can return idle spans but is not an active-memory fix and can worsen
   page TTFB.
6. **TCP and Transfer fanout accounting.** Add allocation-free primitive
   counts for live TCP/UDP/ICMP flows, socket readers and return workers. The
   current synthetic result isolates UDP, while CNN also creates TCP/media and
   Transfer state. The device sampler must show which population tracks each
   crest before shrinking a functional cold-page floor.

The first retained prototype must reduce the 192-flow incremental goroutine
count from 384 to at most 32, cut fresh-process peak runtime by at least 4 MiB,
preserve every datagram and its per-flow order, and match or improve UDP p95
latency/requests per second. That is only the first step: the physical release
gate remains active/steady provider p95 at or below 24 MiB, zero samples above
28 MiB, successful CNN playback in public Wi-Fi/cellular and both P2P
directions, and no regression from the controlled H1 fast.com 38/41/52-Mbit/s
cohort. Reclaim, flow lifetime and object-layout candidates should be combined
only after each wins its own same-route A/B.

## 2026-08-27 fresh-flow placement: affinity as evidence, not assignment

The Bloomberg device trace exposed a routing mistake adjacent to the provider
memory work. A long-lived page H2 connection was acting as a hard IP/domain
affinity donor for later media connections. That policy bypassed the provider
race before quality, external reputation, or endpoint-specific history could
matter. It also retained DNS name/address-to-channel maps that were not useful
once ordinary flows stopped consuming hard affinity.

The transport boundary is exact:

- an established TCP/TLS tuple cannot move because providers terminate TCP;
- six HTTP retries multiplexed on the same Chrome H2 connection cannot invoke
  MultiClient placement again;
- a new source port/SYN is a fresh placement opportunity;
- explicit app/host pins remain the opt-in when one egress IP is required;
- ordinary fresh flows now reach the provider race by default. Setting
  `FreshFlowAffinity=true` restores legacy hard IP/domain inheritance for an
  A/B run, but is not the production policy.

`DestinationAffinity` remains enabled as bounded grouping metadata. A group is
now a performance key, not permission to assign a provider. With hard fresh
affinity off, DNS-exit hints are neither written nor read; this avoids retaining
up to two 4,096-entry, one-hour maps of provider channel references. Existing
flow tables and exact-tuple routing are unchanged.

### TLS-blind performance model

For TCP/443, the client measures cumulative inner-TCP ACK sequence progress.
The first ACK is only the origin's random sequence baseline. Duplicate,
reordered and regressing ACKs contribute nothing; signed 32-bit deltas preserve
sequence wrap. One-second peak-rate buckets and a 250-ms floor on a final
partial bucket reject compressed ACK bursts without delaying a fresh-flow
decision.

Evidence is keyed by both canonical domain constellation and exact destination
IP. Domain evidence carries a result to the next selected video, while exact-IP
evidence prevents a fast page endpoint from hiding a weak media endpoint. The
conservative score is the minimum matching posterior. An unmeasured provider's
score is its advertised `EstimatedBytesPerSecond`, the null hypothesis requested
for this research. For one provider/destination:

```text
weight = min(round(active time, 100 ms), 10 s)
       + min(ACKed bytes / advertised bytes-per-second, 10 s)

posterior = (advertised rate * 1 s + sum(peak rate * weight))
          / (1 s + sum(weight))
```

Combined evidence is capped at 30 seconds. The session-local table is capped at
128 entries and expires after ten minutes. Candidates within the configured
10% placement hysteresis of the best score remain in the race. Equal short
histories therefore remain exactly tied, and if every provider is equally weak
the field fails open instead of making the site unroutable. Established flows
are never moved or closed by this learner. External provider probes remain the
only safe source for domain-specific HTTP challenge reputation because Connect
does not decrypt TLS.

Completed history alone is insufficient for a browser that leaves its page H2
connection open. At each fresh TCP/443 race, the scorer therefore also walks
the existing per-provider `clientUpdates` set (normally bounded by the
16-flow exit cap) and snapshots matching live flows. This is not a new retained
index: it uses the exact-IP/canonical-group keys already owned by each flow,
takes only the existing leaf flow lock, and never adds work or a shared lock to
the ACK hot path. A low-rate still-open page/media connection can therefore
anti-bias the next selected video's genuinely fresh socket; equal low-rate live
connections still have equal weight. Requests reused on that same H2 transport
remain outside the placement boundary.

### Deterministic and cost gates

The current tests cover:

- default ordinary-flow race, explicit-pin inheritance, and legacy A/B restore
  for IPv4 plus DNS hints;
- first-ACK baseline, duplicate/reorder suppression, sequence wrap, and
  partial-window ACK-compression capping;
- advertised-rate null, a low measured provider losing to an unmeasured peer,
  equal short outcomes retaining the whole field, and a fast result remaining
  eligible;
- a still-open H2 flow steering the next fresh full-field race, equal live
  outcomes preserving the full race, a live donor check in legacy mode,
  established-flow immobility, bounded table/TTL, metrics reset/snapshot, and
  Connect-to-SDK round trips;
- six opaque TLS response bursts on one established flow, matching Chrome's
  same-H2 retries and proving they are progress rather than a blackhole or new
  placement event.

Five 500-ms M4 Pro repetitions measured median advancing/duplicate ACK costs of
9.426/1.003 ns/op. Cold, completed-history, and still-open-live fresh-race
scoring measured 137.9/195.7/198.7 ns/op. Every benchmark was 0 B/op and zero
allocations. The advancing production path reuses the timestamp already
required by sequence accounting; a duplicate does not read the clock. Per live
flow the meter is only counters/timestamps. The history is bounded, stores
stable provider IDs instead of channel pointers, and allocates lazily after
completed evidence. The candidate scorer uses a fixed 16-entry stack array and
fails open for an unexpectedly wider runtime override rather than turning exit
count into an allocation vector.

### Physical failure and memory bracket

The pre-policy two-device trace must remain the control, not be rewritten as a
fix result:

| Role/path | Samples | Runtime peak | Live-heap peak | >28-MiB samples | Goroutine peak | Packet-root peak | Returned packet storage |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| Pixel client, public H1 and P2P | 76 | 20.26 MiB | 6.96 MiB | 0 | 257 | 0.39 MiB | 0.25 MiB |
| Galaxy, including P2P provider | 76 | 30.38 MiB | 15.52 MiB | 7 | 740 | 1.05 MiB | 0.25 MiB |

Direct Galaxy cellular playback advanced to 1.674 seconds in five seconds with
13.34 seconds buffered. Public United States H1 on both devices and same-LAN
P2P remained at time zero; P2P nevertheless returned about 10.5 MiB and had
zero local/provider security blocks. The provider memory crest is again live
heap plus flow/goroutine topology, not a returned-pool high-water. The default
affinity change should reduce stale DNS reference retention and give every new
media connection another provider-selection opportunity, but neither playback
nor memory improvement may be claimed until the rebuilt artifact is measured.

### Current-source two-device result

The rebuilt default-off policy was measured in two approximately 20.25-minute
Android sessions. Each phone used validated Wi-Fi and validated cellular in
opposite arms, then one phone provided an exact-ID same-LAN P2P exit to the
other. These are variable public-route observations, not a paired provider
benchmark:

| Client path | Wikipedia median document TTFB | Wikipedia median load | fast.com samples | fast.com median |
| --- | ---: | ---: | --- | ---: |
| Pixel Wi-Fi, public United States H1 | 153.1 ms | 550.6 ms | 61 / 40 / 110 Mbit/s | **61 Mbit/s** |
| Galaxy cellular, public United States H1 | 355.2 ms | 1,886.0 ms | 0.63 / 0.76 / 0.68 Mbit/s | 0.68 Mbit/s |
| Pixel cellular, public United States H1 | 453.8 ms | 979.7 ms | 6.3 / 0.55 / 9.6 Mbit/s | 6.3 Mbit/s |
| Galaxy Wi-Fi, public United States H1 | 174.3 ms | 402.0 ms | 18 / 0.96 / 4.4 Mbit/s | 4.4 Mbit/s |
| Galaxy provider to Pixel client, exact P2P H1 | 168.7 ms | 1,233.8 ms | 3.5 / 3.5 / 3.5 Mbit/s | 3.5 Mbit/s |

The 61-Mbit/s median demonstrates that the policy does not impose a 40-Mbit/s
ceiling and restores the requested class on one real public route. The other
arms demonstrate why this is not a universal 40-Mbit/s claim: exit quality and
radio path still dominate. After public traffic, the two clients reported
395/3,129 and 319/7,280 performance samples/candidates-filtered respectively.
The learner was therefore active in the fresh races. Donor-bypass remained
zero, as expected when legacy hard inheritance is disabled rather than entered
and rejected.

Bloomberg playback succeeded on the Galaxy Wi-Fi public-H1 arm: the media clock
advanced from 8.718 to 9.724 seconds with `readyState=4` and about 20.26 seconds
buffered. Five encrypted Fetch responses with status 403 occurred on one reused
H2 connection during that successful playback. The Pixel cellular arm did not
advance and had seven 403 Fetch responses on one reused H2 connection. Forcing
a genuinely fresh Chrome transport afterward produced a top-level document
403 on a new H2 connection, which Chrome did not retry. The P2P exit likewise
returned a fresh document challenge while reporting a nonempty provider build
and policy identity and zero provider-side block counters. This proves all
three boundaries: same-H2 HTTP retries cannot be reraced; fresh-flow placement
does get another chance; and another provider may still have the same
destination-specific reputation. Public exits exposed no build/policy
diagnostics, so deployment of external reputation metadata remains unverified.

Memory remained streamlined for the client but not for the provider:

| Role | Samples | Runtime peak / p95 | Quiet-window p50 / p95 / range / last | >24 / >28-MiB samples | Live-heap peak | Goroutine peak | Packet-root / returned-pool peak |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| Pixel client | 81 | 22.00 / 21.61 MiB | 21.02 / 21.70 / 20.51--22.00 / 20.57 MiB | 0 / 0 | 7.97 MiB | 233 | 0.35 / 0.25 MiB |
| Galaxy, including provider | 81 | 29.45 / 26.74 MiB | 24.01 / 25.20 / 23.10--25.20 / 23.56 MiB | 27 / 2 | 11.97 MiB | 340 | 1.22 / 0.25 MiB |

Each quiet window contains 20 primitive samples. The client passes the active
and steady 24/28-MiB gates. The provider does not: even after traffic quieted,
11 of its 20 samples remained above 24 MiB. At the 29.45-MiB runtime crest the
live heap was about 11.90 MiB, 304 goroutines were live, packet roots were only
about 0.57 MiB, returned packet storage was about 0.25 MiB, one automatic GC
had occurred, and queues were empty. Across both sessions there were zero
packet-pressure drops and zero H1 receive-queue drops. This repeats the live
provider flow/goroutine-topology attribution and rejects both hard-affinity
removal and more aggressive returned-pool reclaim as its fix. The bounded
shared provider UDP poller/lifecycle experiment remains the highest-priority
provider-memory direction.

The exact source gates passed: Connect `go test ./... -short -count=1` and
`go vet ./...`, the focused routing tests under the race detector, the complete
short SDK suite, focused server model tests and vet, the current SDK AAR plus
stamped Android app/test/unit build, and all 13 dependency-free Android script
tests. The server database integration fixture remained unavailable because
the documented `WARP_ENV`/vault PostgreSQL inputs were absent; the pure
provider-metadata assembly tests passed.

Release acceptance for this direction is: no fresh ordinary flow directly
inherits an IP/domain donor; retry on an existing H2 connection remains
untouched; a fresh SYN can select another provider; no fast.com or page-TTFB
regression; client steady p95 at or below 24 MiB with zero >28-MiB samples; and
the existing provider-memory gate remains separately open until the shared UDP
poller/lifecycle work reduces its live topology.

## 2026-08-27 provider topology and fresh-race correction

This pass closed the first three provider-memory research directions, then
investigated a real fast.com regression introduced by making TLS-blind
performance evidence load-bearing. Every candidate used a fresh child process;
physical arms used one attached Android phone, validated Wi-Fi, explicit H1,
the production United States pool, a fresh app/client/Chrome session per arm,
the canonical fast.com harness, and primitive Go/queue counters. Public exits
were not pinned, so the physical results are target and regression gates, not a
causal percentage benchmark between provider sets.

### Direction 1: shared provider UDP socket lifecycle

The retained constrained-provider profile replaces one blocking reader, send
loop, send channel and idle timer per UDP flow with:

- one OS-readiness worker per existing receive-dispatch shard and address
  family (`epoll` on Linux/Android, `kqueue` on Darwin/iOS, portable fallback
  elsewhere);
- direct serialized datagram writes, which preserve UDP message boundaries;
- the existing bounded receive dispatcher, which drains all immediately ready
  datagrams in one callback batch; and
- one buffer-level idle-deadline worker.

Construction remains lazy, so a TCP-only provider allocates no poll backend.
The profile is enabled only by provider settings with a nonzero memory target;
generic callers and the target-zero server defaults keep the established
portable topology. Readiness callbacks use `syscall.RawConn.Read` to pin the
descriptor through a drain: an event queued before unregister/close cannot read
a newly reused descriptor into the old flow.

Five fresh 16-packet/24-KiB SDK provider-load repetitions gave:

| Arm | TCP echo median | UDP median | Loaded runtime / live heap | Goroutines peak / loaded | Runtime peak | Decision |
| --- | ---: | ---: | ---: | ---: | ---: | --- |
| Per-flow reader/send/timer control | 53.1 MiB/s | 13,960.4 round trips/s | 27.9 / 10.4 MiB | 641 / 621 | about 31.1 MiB | Control |
| Shared readiness + direct write + shared idle lifecycle | 54.1 MiB/s | 15,414.3 round trips/s | 25.8 / 9.4 MiB | 356 / 242 | about 30.9 MiB | **Keep**: TCP +1.9%, UDP +10.4%, loaded runtime -2.1 MiB, loaded goroutines -61.0% |

All five retained-candidate repetitions stayed below the synthetic 31-MiB
host ceiling. Allocation/CPU profiles explain the direction: sampled GC drain
fell from 0.48 to 0.33 cumulative seconds, and the loaded `time.NewTimer`
allocation sample disappeared. Profiling overhead itself can cross the memory
ceiling, so profile runs remain attribution-only. Ordering, idle reap, absent
per-flow queues, descriptor unregister, race behavior, Linux/Android and
Darwin/iOS compilation are deterministic gates.

### Directions 2 and 3: provider group bound and telemetry

The local 32-packet/48-KiB provider group reduced admission time from 4,929 to
4,344 ns/op (-11.9%) and allocations from 98 to 82 in the narrow microbenchmark.
The complete provider path reversed that result: TCP median fell from 54.1 to
52.7 MiB/s (-2.6%), and two of five repetitions crossed 31 MiB. Production
therefore remains 16 packets/24 KiB. A larger logical group is rejected until a
controlled physical provider proves a complete-path win.

Schema-12 already exposes ACK/recovery writes, route-write wait, Pack handoff,
carrier/queue drops and pressure. The profile separated timer/GC work without a
new ACK-hot-path atomic, so no additional sampled counter was retained in this
pass.

### Fast.com regression root cause and retained correction

The active performance learner was not merely ordering the fresh TLS field. It
could reduce a four-to-six-exit quality race to one provider. That is unsafe in
a rough public pool: a posterior is evidence about prior flows, not proof that
the next origin socket will remain healthy. The regression was visible without
carrier or Pack loss; candidate removal, timeout recovery and the loss of
parallel tail insurance were the limiting boundary.

Separately, completed evidence is now published only if the exact route still
has an active transport, has no warning/quarantine verdict, is not canceled,
and remains present in a live window. Channel state is rechecked after the
window scan. This cold retirement-path guard adds no ACK work and prevents a
retired route's teardown interval from poisoning a later decision. It was
correct but did not by itself restore speed: the guarded hard-filter arm still
accepted 67 healthy samples and removed 1,706 candidates.

The measured exploration sweep was:

| Fresh TCP/443 policy | fast.com displays | Median | Runtime result | Placement/recovery evidence | Decision |
| --- | --- | ---: | --- | --- | --- |
| Installed hard-filter control | 110 / 55 / 66 Mbit/s | 66 Mbit/s | 21.91-MiB peak | 55 samples; 502 candidates removed | Regression control; variable route still reached 40, but filtering was already heavy |
| Learner disabled / full field | 130 / 110 / 140 Mbit/s | 130 Mbit/s | **30.43-MiB peak** | zero learner work; 7,629 timeout resends; 10--13 failed page requests/run | Reject: fast but outside 24/28 MiB and less reliable |
| Active/healthy guard, hard filter | 0.91 / 6.2 / 4.9 Mbit/s | 4.9 Mbit/s | 24.23-MiB peak | 67 samples; 1,706 removed; 991 pressure drops; zero carrier/Pack drops | Reject hard collapse; keep lifecycle guard independently |
| Minimum two candidates | 15 / 0.52 / 89 Mbit/s | 15 Mbit/s | 20.47-MiB peak | 60 samples; 1,249 removed; zero carrier/Pack drops | Reject: memory-safe and better than the adjacent hard arm, but bad tail |
| Minimum three candidates | 0.68 / 0.89 / 0.95 Mbit/s | 0.89 Mbit/s | 18.83-MiB peak | 57 samples; 282 removed; zero pressure/carrier/Pack drops; two page-target transfers independently measured about 29 Mbit/s | Reject: canonical displayed result did not improve |
| **Minimum four candidates** | **47 / 100 / 110**, then **68 / 85 / 130 Mbit/s** | **92.5 Mbit/s across six** | first cohort 24.30-MiB peak; hot repeat 22.75-MiB peak | more than 1.1 GB exact H1 ingress; 2,243/2,243 Pack waits at the observed maximum; zero carrier/Pack drops | **Keep**: all six exceed 40 Mbit/s; warm 24.30-MiB crest remains disclosed below |

The four-route floor keeps the highest posterior first and retains three escape
routes; fields wider than four can still discard clearly weaker candidates.
It sorts the placement-local slice using a fixed stack array. Current M4 Pro
medians were 132.4 ns for a cold race, 309.1 ns with completed evidence, and
279.5 ns with live evidence, all 0 B/op and zero allocations. The deterministic
six-candidate regression test verifies that a strong route plus three stable
alternatives survive while the two weakest routes are removed.

The first four-route burst had one 24.30-MiB sample, 304 KiB above the nominal
24-MiB line, then returned to about 20.3 MiB. After a quiet interval, the next
three fast.com runs peaked at 22.75 MiB rather than growing. Five subsequent
cache-disabled Wikipedia runs peaked at 20.92 MiB and measured 478.3-ms median
load, 195.2-ms median document TTFB, and zero failed requests. Thus the steady
24-MiB goal and 40-Mbit/s target pass on this Android surrogate, but this is not
an absolute all-samples <=24-MiB claim. Do not spend throughput to erase one
non-repeating 304-KiB GC-phase crest without a paired improvement; physical iOS
`phys_footprint`/jetsam remains the release gate.

The complete Connect/SDK short suites, both vets, focused race-detector tests,
iOS/Linux cross-builds, and final Android AAR/app/test/unit build passed. Every
temporary production client, on-device credential file and private session
artifact was removed after its arm.

## Experiment queue

### Rejected packet-allocation candidate (2026-09-20)

The `appendIpPacketGroupBounded` ownership deferral and bounded
`WindowStats` source-count scratch reuse were tested together. They improved
their deterministic microbenchmarks (multi-packet grouping avoided discarded
canonical-path allocations; repeated 64/256-destination stats reads avoided
temporary count slices), and focused normal/race tests passed. The required
fresh physical H1 iOS-profile arm did not validate the direction: the full
20/32-MiB profile and 315-second quiet window peaked at 26,517,536 bytes,
above the absolute 24-MiB barrier and 1.3% higher than the 26,177,568-byte
owner-fix replay. Its Fast.com displays were 0.79/28/31 Mbps, not a speed win.

Do not retain or retune this pair on the basis of allocation microbenchmarks.
The result reinforces that the active excess is retained runtime/span/stack or
traffic-lifecycle memory outside these short-lived packet/stat scratch objects.
Future candidates must first attribute the physical peak by Go runtime class
and traffic phase, then prove an improvement against the same guarded
collector/workload/quiet protocol.

### Idle API ownership; rejected stack experiment (2026-09-20)

The two valid H1 artifacts above peaked at 26,177,568 and 26,517,536 bytes.
At those samples, packet/TUN roots and tracked Transfer queues were empty;
free packet/large-object pools held only 442,368 and 385,024 bytes. The newer
peak contained 10,513,960 bytes of heap objects, 5,648,856 bytes of occupied-span
slack, 770,048 free heap bytes, 3,178,496 stack bytes, and 6,406,176 other runtime
bytes. Lower allocation rate alone does not clear this retained-memory floor.

`ClientStrategy` did not subscribe to memory shedding, leaving idle API HTTP,
alt-QUIC and internal-DoH socket graphs outside the existing SDK reclaim pass.
The scoped fix registers/unregisters with owner lifetime and closes only idle
connections. It preserves active responses, the reusable HTTP transport and
path-local TLS tickets; H1 data carriers, budgets and reclaim timing are unchanged.

Deterministic loopback tests cover global dispatch, idle/native HTTP and QUIC,
active-response protection, registration/cancellation races, and TLS resumption.
A four-idle-QUIC-owner sample released all 6,815,744 **reserved** transport-budget
bytes, 48 goroutines and 188,400 heap bytes, versus no closed carriers and
23,728 heap bytes reclaimed on baseline. Both loopback endpoints are in-process:
these are not predictions of device heap savings, and reserved bytes are not
physical memory. Six paired/reverse-order benchmark repetitions kept allocations
unchanged; warm API latency changed -0.22%, resumed re-dial +0.50%, and H1
saturated/ACK-sized/sparse latency medians -0.60% to +0.82%. Focused normal/race
tests and vet pass. Re-dial still has a cost after genuine pressure; do not turn
this cleanup into a per-request or periodic forced pool reset.

Separately, removing blank admission-owner stack temporaries reduced the local
ARM64 `SendSequence.Run` frame from 4,640 to 4,048 bytes; extracting its cold
timeout log reduced it to 3,872. Neither changed the fresh-process retained-stack
measurement: 64 real idle sequence/ACK-worker pairs used 1,277,952 incremental
stack bytes in every arm. Both production refactors were rejected.

The API cleanup is a locally verified resource-lifecycle fix, **not a physical
24-MiB pass**. No physical arm or baseline promotion was performed for it.
The next qualifying arm must retain full active/quiet coverage, the absolute
24-MiB ceiling and unchanged Fast.com/page performance gates. Aggregate evidence
and exact commands are retained privately in the retained-owner analysis report.

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
2. **Retained mobile profile.** Keep H1 receive 64 / 128 KiB and lossless exact
   reliable-lane carrier/Pack backpressure to cancellation; keep every other
   Transfer/control count at 16, ACK handoff at
   1 ms, the packet-root gate at 1 MiB base / 2 MiB exact H1 ACK maximum, and
   the exact provider-aware shared receive-allocation budget (1.68 MiB on and
   2 MiB off at the 24-MiB target). Leave adaptive H1 depth disabled in the
   mobile policy; its full-depth physical arm did not improve throughput.
   Repeat provider off/on/off under traffic so the provider share transition
   cannot retain an old H1 burst.
3. **Provider logical-group bound (closed locally).** Keep 16/24 KiB. Although
   32/48 KiB won the admission microbenchmark by 11.9%, it lost 2.6% TCP on the
   complete provider path and crossed the synthetic memory ceiling in two of
   five runs. Reopen only for an exact controlled-provider physical A/B.
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
    as the regression control: every iOS-profile runtime sample <=24 MiB,
    five-minute quiet coverage with maximum/p50/p95 <=24 MiB, full payload
    completion, and packet roots <=2 MiB. Re-open
    reclaim tuning only if a controlled provider deployment recreates the old
    29.48-MiB crest.

## Notes and pitfalls

### 2026-09-20: on-demand owner census for the attested H1 breach

The fully attested native H1 arm still failed the absolute 24-MiB barrier:
29,900,832 bytes (28.52 MiB), while Fast.com completed at 65/35/65 Mbit/s.
The peak sample was 274,324 ms into `connect-h1`, with 11,594,104 heap-object
bytes, 6,805,128 bytes of in-use span slack, 1,851,392 free-but-unreleased heap
bytes, 3,244,032 stack bytes, 243 goroutines, and 201 client flows. Returned
pool retention was only 557,056 bytes; live packet roots and tracked transfer
queues were zero at that sample. Admission claims were 2,097,152 bytes across
eight slots, **not** an estimate of the underlying heap. This is not the older
unattested arm's allocation-class breakdown, even though the total peak
coincided. No cap, queue budget, reclaimer threshold, or baseline is changed.

New opt-in diagnostics read existing owners: active Transfer workers including
canceled/index-detached workers; channel capacities and pacing services;
flow/affinity/cache counts; IpAssoc raw scratch; API pools/connections; DNS
caches; bounded Transfer free lists; and claims by transport class. The SDK's
non-iOS `DeviceLocal.WriteMemoryOwnerCensus` exclusively creates an aggregate
mode-0600 JSON file with before/after runtime values and allocator size classes.
There is no packet hook, registry, background sampler, forced GC, or cache
release. Known struct/slice bytes are intentionally incomplete ownership
accounting, not a heap total or a span-slack attribution. Native HTTP/TLS/QUIC
object graphs still require paired private profiles.
The census scopes current indexed device owners; already-unlinked client
generations and other NetworkSpaces are not discoverable without those
profiles. Zero current-owner counts therefore do not prove a process-wide
absence of leaks.

Deterministic tests cover workers remaining after index removal and reaching
zero only after joined teardown; receive workers; canceled flow reaping while
preserving live TCP; scratch capacity and pressure release; API active/idle
and closed-but-rooted discrimination; actual automatic closed-API retirement;
DNS shared-cache deduplication and release; transport claim classes; bounded
topology truncation; privacy/exclusive files; and concurrent reads/teardown.
The actual API close callback released its pool entry: this is negative leak
evidence, not justification for additional connection churn.

Local Apple M4 Pro / Go 1.26.7 measurements: a 400-flow census took a median
2.62 microseconds (10 runs); an empty-device primitive census 52–56 ns; a
runtime-plus-owner report 43.9–45.9 microseconds (five runs). All three measured
0 B/op and 0 allocs/op after existing-owner initialization. File/JSON writing
is excluded and deliberately diagnostic-only. Focused normal repetitions,
race tests, vet, Android helper unit tests, and acceptance Kotlin compilation
passed. These are overhead/lifecycle checks, **not** a physical 24-MiB pass.

Next diagnostic protocol: explicitly rebuild and attest SDK/AAR/native/APKs
with iOS profile and startup heap sampling 65536; require the owner-census
capability preflight, then pair pre-GC owner census, heap profile, post-GC
census, and last-of-all private goroutine stacks at idle/post-traffic/quiet
boundaries. See `android/app/scripts/PHYSICAL_LOWBAR.md` for exact command and
privacy rules. Raw profiles/stacks never leave private artifacts; report
aggregate allocation/function/state owners only. Keep this diagnostic arm
separate from unprofiled performance acceptance. No additional physical arm
has been run for the census, and no concrete remaining allocator owner has
yet been proven causal.

### Owner-census physical diagnostic — 2026-09-20

A fresh, private Android diagnostic arm used the pinned Pixel 8 Pro with the
iOS-audit profile (20-MiB admission / 32-MiB runtime limit), 64-KiB profile
sampling, matching freshly built SDK AAR / app APK arm64 native-library digests,
and a successful schema-1 owner-census preflight. The diagnostic artifacts,
including raw pprof and goroutine stacks, are private mode-0600 evidence. This
was intentionally a profiled attribution arm, not a qualifying 24-MiB or
throughput run.

The native runtime was 17.36 MB at no-traffic preflight, 20.90 MB shortly
after H1 connect, 30.91 MB after the joined real-site workload, 29.33 MB after
the explicitly forced heap-profile collection, and 27.99 MB after 180 seconds
of still-connected quiet. Thus it remains above the absolute 24-MiB ceiling
even after natural release; no baseline or budget changes are justified.

The census materially narrows the hypothesis. Live client flows rose from 6
after connect to 225 after traffic, then fell to 3 at the quiet boundary;
reverse-DNS entries fell from 300 to 11. Transfer queues were empty at every
post-traffic census, and pool known-retained structs fell from 53.6 KiB to
15.4 KiB. The quiet sample still had 8.96 MB heap objects, 6.29 MB in-use heap
slack, and 3.15 MB stacks. Consequently, neither pooled packets nor unreaped
flow/reverse-DNS maps explain the remaining steady runtime by themselves.

The private post-traffic heap profile had 9.43 MB sampled live heap. Its
largest aggregate allocations were buffered writers (0.85 MB), source-event
buckets (0.67 MB), message-pool warm storage (0.32 MB), and message-pool
construction (0.23 MB); these are incomplete sampled heap attribution, not
permission to tune a pool. The large residual is allocator span slack plus
stack/runtime overhead. A privacy-safe aggregate of the private stack capture
counted 230 goroutines (107 plain `select`, 16 poll waits, and most remaining
tops in Connect); it did not reveal one unexpectedly rooted post-traffic
worker class. The next accepted research step is therefore a stack-size-class
and allocation-lifetime comparison around the 180-second flow-release boundary,
then a deterministic release test for any concrete owner that remains high.
Do not trim, close active H1 sessions, or lower packet/sequence budgets until
that causal release test demonstrates a steady-memory reduction without page
or Fast.com regression.

The same diagnostic workload recorded Fast.com 16/32/54 Mbps and observed
both requested media probes without clock progress; both media child exits
were retained. Because profiling perturbs the run and the host collector's
external VPN eligibility was invalid for this arm, these are diagnostic
observations only—not performance, provider, or video-regression verdicts.

### H1 destination retirement: missing carrier join — 2026-09-20

A deterministic actual-H1 test found a narrower lifecycle defect, not yet an
explanation for the still-connected memory peak. The SDK already joins its
retired multi-client and owned API generator. However, generator retirement
only canceled the external `PlatformTransport`; `Client.CloseAndWait` does
not own that carrier. Holding the H1 teardown after route removal therefore
allowed generator `CloseAndWait` to return success while carrier workers
were still alive. `TestApiMultiClientGeneratorJoinsActualH1Transport` fails on
the old code and passes after joining the carrier in the existing asynchronous
retirement worker, outside generator locks. Caller cancellation remains
bounded; ownership continues until actual teardown, and a later join succeeds.
Identity removal remains after joined carrier/client/OOB cleanup. No active
H1 path, queue, timeout, admission budget, or memory ceiling changed.

The SDK `TestDeviceLocalH1OwnerLifecycle` uses local auth/discovery, a real H1
websocket and API generator, eight echoed packet flows, and production
`DeviceLocal.CloseAndWait`. In 20 healthy repetitions per arm, cold traffic
median was 205.139 → 205.142 ms and close median 0.575 → 0.576 ms (p95
0.872 → 0.857 ms). Both arms cleared current flows/clients and generated
send/receive/pacing owners; there was no observed meaningful healthy-close
regression. Normal/race repetitions and Connect/SDK vet passed. These local
latencies are not Fast.com or physical-device performance measurements.

Immediate process-wide heap snapshots did not demonstrate heap reclamation:
the fixture deliberately retains closed clients for owner assertions and
keeps its server/provider alive, and identity cleanup itself allocates.
Likewise, the H1 admission claim can reach zero before the carrier's `Done`
edge; it is not proof that socket workers or buffers have finished. The fix
repairs false teardown completion, but does not establish a leak among the
five expected live H1 exits or a physical 24-MiB pass. Keep connected-owner
allocation/stack attribution and transport-budget release ordering as
separate research questions. Local evidence is retained in the private
`urnetwork-h1-carrier-join.CFVklV` artifact; no device arm or baseline change
was made for this fix.

### Block-action history: release verified, policy change rejected — 2026-09-20

The owner-diagnostic post-traffic heap profile attributed approximately
0.94 MiB to `blockActionCollector.flush` → SDK row conversion, principally
IP strings and exported-list storage. That call stack identifies allocation
origin, not a collector-owned leak. The physical SDK history fell from
1024 rows / 1183 slots to 35 / 60; its later sample was about 363 seconds after
the pre-GC post-traffic sample, beyond the existing 300-second history window.
Runtime still measured 27,990,296 bytes, 2,824,472 above the absolute ceiling.

New deterministic tests keep production policy unchanged: Connect's collector
flush releases both epoch aggregates and unretained emitted actions; SDK
expiry releases every old row, shrinks backing storage, preserves surviving
decisions/counts, and releases all slots when empty. A consumer-held snapshot
correctly retains its rows only until that snapshot is released. Tests use
weak references and virtual time, not sleeps or shortened production windows.

Five fresh-process release trials (1024 rows, 16 IPs and one synthetic host
per row) measured median full-history live heap of 856,160 bytes above empty.
Expiring to 35 rows released 826,760 bytes, leaving 29,400 bytes of live heap
and 90,112 in-use heap bytes above empty; expiring all left only 320 live-heap
bytes above the initial diagnostic snapshot. Process runtime stayed higher
and variable despite released owners, demonstrating why runtime alone is not
proof of retained action rows. Forced GC/scavenging here is test-only, not a
proposed production remedy or a physical-memory pass.

A 64-row history update with 16 IPs/row measured a median 43.079 microseconds,
94,264 B/op and 2,244 allocations/op (five benchmarks, Apple M4 Pro / Go
1.26.7). Conversion/trim churn is real, but no experiment yet ties it to the
remaining multi-megabyte quiet excess. No cap/window/security or production
code change is retained. Connect normal x10/race x3, SDK release normal
x5/race x5, and both vets passed. Evidence:
`urnetwork-block-action-retention.e0MzEc/REPORT.md`.

Remaining question: cross-owner span fragmentation or a separately held UI
snapshot requires a paired quiet heap profile and controlled allocation-layout
experiment. These isolated release tests do not rule those out. The Android
projection caches Kotlin value rows rather than Go BlockAction wrappers;
that read-only source observation is not a JNI-lifetime measurement. Lowering
history retention without that causal evidence would reduce observability
without establishing a 24-MiB fix.

### Go soft limit 24 versus 32 MiB: blanket change rejected — 2026-09-20

Ten fresh-process, alternating local H1 arms compared **only** the runtime
soft limit: five at 32 MiB and five at 24 MiB. Device admission stayed 20 MiB,
Connect process sizing stayed 32 MiB, GOGC stayed 25, and mobile pool/queue
policy stayed identical. An explicit test-only platform overlay enabled the
mobile branches on the darwin host. A real DeviceLocal/API generator and H1
WebSocket carried 16,384 echoed 1200-byte payloads per arm through the Auto
four-quality-plus-one-speed topology. All ten arms retained five clients and
72 indexed flows and passed correctness. No public endpoint/device was used.

| Median measurement | 32-MiB soft limit | 24-MiB soft limit |
| --- | ---: | ---: |
| Sampled traffic runtime peak | 25,976,598 B | 23,609,110 B |
| Natural connected quiet runtime | 24,510,230 B | 23,314,198 B |
| Quiet heap / in-use span slack | 7,288,664 / 4,480,836 B | 6,493,584 / 4,280,624 B |
| Quiet stack bytes | 3,080,192 B | 3,244,032 B |
| Connected runtime after diagnostic GC | 22,454,038 B | 21,794,598 B |
| Local payload echo rate | 280.20 Mbit/s | 180.67 Mbit/s |
| Packet RTT p50 / p95 | 1.489 / 6.426 ms | 2.228 / 12.629 ms |
| Traffic GC cycles / summed pauses | 177 / 26.183 ms | 357 / 39.410 ms |
| Traffic GC CPU / mark-assist CPU | 0.880 / 0.113 s | 1.735 / 0.433 s |

The quiet-runtime median improved by 1,196,032 bytes (1.14 MiB), but natural
quiet ranges overlapped: 23,306,006–25,562,902 B at 32 versus
23,002,902–23,469,846 B at 24. Throughput regressed 35.5%; every 24-MiB sample
(174.67–225.98 Mbit/s) was below every 32-MiB sample (251.07–347.78 Mbit/s).
The lower limit roughly doubled collections and GC CPU and raised mark-assist
CPU 3.84-fold for the same useful traffic. It primarily reduces allocation
float/free heap, with only a small median slack reduction—not a demonstrated
resolution of the physical arm's 6.29-MB span slack.

**Rejected as a blanket production change** under the requirement to preserve
H1 performance. Keep the existing 20-MiB target / 32-MiB soft-limit policy;
this does not accept its known physical breach of the absolute 24-MiB barrier.
Only reusable tests and research evidence are retained. Exact per-arm JSON,
statuses, commands and ranges: `urnetwork-h1-softlimit.n4sTod/REPORT.md` and
`results.jsonl`. SDK lifecycle normal x5/race x3, the opt-in experiment under
race, vet and diff checks passed.

Scope: 12 seconds of natural quiet (last six samples summarized), then three
explicit diagnostic GCs and one diagnostic scavenge. Flows did not expire;
this is not the physical five-minute quiet gate. Loopback providers/server
share the measured process; the carrier is local `ws`, not upstream TLS, and
echoed UDP packets do not model browser TCP congestion. These rates are not
Fast.com speeds or a prediction of the phone's regression. No physical iOS,
Android, absolute-peak, or release-readiness claim follows from this test.

Follow-up fixture caveat (same date): the local provider relay used an untyped
gateway with unknown receive reliability. A deterministic queue-saturation
test now proves that this permits a Pack handoff drop, whereas an explicitly
reliable H1 relay backpressures and preserves the packet. The ten historical
soft-limit arms completed, but did not collect this provider-side drop guard.
Keep those numbers as historical surrogate observations, not a complete
bidirectional production-H1 validation or proof that 24 MiB cannot work with
other allocation changes. No soft-limit policy change was made.

### Live H1 topology 4+1 / 3+1 / 2+1: not retained — 2026-09-20

The opt-in SDK `TestDeviceLocalH1TopologyExperiment` varies only the number
of quality slots through test overlays. Speed stays one, process sizing and
soft limit stay 32 MiB, device target stays 20 MiB, GOGC stays 25, and all
mobile pools/queues and reliability timeouts remain unchanged. The real
DeviceLocal/API generator/H1/Transfer path carries equal useful echo traffic
on quality-first port 443 and speed-first port 123. Five local provider
fixtures exist in every arm. Before traffic, exact owner count **and** Added
provider count must match the requested topology.

The first partial cohort used unknown provider carrier metadata and is
`INVALID_FIXTURE`, preserved separately as `urnetwork-h1-topology.LfDvS7`.
`TestH1OwnerFixtureReliableProviderHandoff` reproduces its saturation failure:
the old relay drops one of three packets; the corrected H1/reliable relay
waits and delivers all three in order. This is a test-only relay repair,
not a production transport change. Normal x5 and race x3 passed.

The corrected balanced cohort completed all 15 process-level records. Every
row had zero provider/client Pack handoff drops, but traffic correctness was
only **3/5, 2/5, 2/5** respectively: baseline and candidates had intermittent
mid-flow echo timeouts or packet-admission refusals. Do not discard failed
rows, rank survivor-only medians as a clean A/B, or update a baseline.

| Observed measurement | 4 quality + 1 speed | 3 quality + 1 speed | 2 quality + 1 speed |
| --- | ---: | ---: | ---: |
| Connected goroutines, all five arms, median | 265 | 236 | 210 |
| Connected runtime, all five arms, median | 18,120,470 B | 17,497,878 B | 17,612,566 B |
| Completed traffic/recovery arms | 3/5 | 2/5 | 2/5 |
| Quiet runtime, completed arms only | 29,069,094 B | 27,316,006 B | 26,562,326 B |
| Quiet allocated heap / span slack, completed only | 11,294,928 / 3,753,776 B | 9,785,096 / 4,219,128 B | 7,928,896 / 4,924,352 B |
| Quiet stack bytes, completed only | 3,604,480 B | 3,088,384 B | 3,096,576 B |
| Quality / speed echo Mbit/s, completed only | 642.80 / 412.13 | 425.34 / 348.90 | 598.46 / 408.24 |
| Quality packet RTT p50 / p95, completed only | 0.739 / 1.680 ms | 1.069 / 3.420 ms | 0.832 / 1.831 ms |
| Provider blackhole removal, completed only | 30.18–30.93 s | 30.18–31.43 s | 30.17–30.67 s |

Even the lowest completed natural-quiet runtime was 26,287,894 B, above
25,165,824 B (24 MiB). The completed 2+1 arms show fewer owners and about
0.48 MiB less stack space, but span slack increases; reducing client count
does not itself resolve allocator slack. Survivor-only quality throughput
also falls (~6.9% at 2+1, ~33.8% at 3+1). These are observations, **not**
statistically established deltas given the incomplete traffic cohorts.
No smaller production topology is retained.

One predeclared bounded baseline discriminator subsequently passed with zero
SDK pressure drops, NoAck refused/discard, prewire expiry, receive-queue drops,
and ACK/Pack handoff drops. It did not reproduce the remaining failures;
their source boundary remains open. The opt-in test retains those primitive
counters for the next failing capture. Do not infer a root cause or relax
timeouts from a passing diagnostic. No further diagnostic/device arm ran.

Scope: 64 concurrent flows x 1,024 x 1,200-byte echoes per traffic phase
(157,286,400 useful outbound bytes total), 12 seconds connected quiet, then
test-only GC/scavenge and a single-provider blackhole. All seven completed
arms removed that unavailable provider under unchanged production timers
and delivered 1,024 fresh recovery packets. Co-resident providers, plaintext
local `ws`, synthetic UDP payloads and retained 136 flows differ from phone
TCP/TLS/fast.com and five-minute quiet. These are not website TTFB, physical
speed, iOS footprint, absolute-peak acceptance, or WAN-diversity guarantees.
Raw per-arm GC/pause/traffic/heap data and statuses:
`urnetwork-h1-topology-reliable.RDM5c1/{REPORT.md,results.jsonl}`.

### UDP SCTP ACK scheduling: insufficient for the cold source gate — 2026-09-20

Retained deterministic research only; no production or memory-policy change.
`TestProviderUdpSctpImmediateAckCannotRemoveColdRttRefusal` connects the actual
provider/Transfer/P2P writer to the pinned SCTP with its existing blocking,
reliable-unordered semantics and unchanged 32/four-slot queues. A lossless
virtual-time wire isolates constant RTT from jitter, link serialization,
ICE/DTLS, encryption, and device scheduling. The source offers 468 distinct
1,000-byte UDP payloads at 3.75 Mbit/s after a fully ACKed setup packet.

| Ten normal repetitions | 2-ms RTT control | 120-ms RTT | 120-ms immediate-SACK upper bound |
| --- | ---: | ---: | ---: |
| Source admitted / refused | 468 / 0 | 300 / 168 | 317 / 151 |
| Refused before first possible returned ACK | 0 | 16 | 16 |
| First refusal | none | 87.467 ms | 87.467 ms |
| First returned SACK | 4.133 ms | 122.133 ms | 120.000 ms |
| SACKs, median (range) | 234 (234–234) | 95.5 (93–98) | 199.5 (197–203) |
| DATA retransmits / lost admitted identities | 0 / 0 | 0 / 0 | 0 / 0 |

At both high-RTT first-refusal edges, three DATA chunks have been written,
cwnd is still 4,380 B, SCTP pending/inflight is 4,536 B, and the route is 4/4.
The receive window stays at least 432,434 B in the normal cohort. The limiting
boundary is the **cold congestion window plus the round trip**, before any
ACK policy can return progress. This is separate from the previously fixed
shared UDP writer-lock blockage and from loss/retransmission stalls.

The test-only I-bit arm requests immediate SCTP ACKs on test-owned packet
copies. It admits 17 more later offers but approximately doubles SACK traffic
and cannot pass the frozen all-source-admitted gate. There is no production
Pion setting for this experiment; no dependency was patched. Do not infer a
safe network throughput improvement from a lossless wire without reverse
bandwidth limits. Race-mode later counts vary with ACK coalescing; the
pre-first-ACK failure boundary remains invariant.

Adjacent review ruled out the userspace UDP batching queue for legacy SCTP:
DTLS/SCTP uses `writeDirect`; only SRTP is queued. Earlier ready-drain batching
reduced post-stall writes but not source refusals, and wider frames increase
per-slot roots. Do not disable blocking SCTP, make public UDP callbacks wait,
increase cwnd/queues, or relabel source refusals to force a green result.
No eligible production candidate survived, so no new fixture/device A/B ran;
the current-source PERFVAR UDP failure and absolute 24-MiB gate remain open.

Normal ×10, race ×10 and vet pass. Exact commands, primitive per-repetition
results, preserved initial test-race failures and source boundaries are in
`urnetwork-udp-sctp-ack.5UikD8/REPORT.md`; campaign notes are in
`tests/PERFVAR-MEASUREMENTS.md`. This is not a physical memory measurement,
website speed measurement, or baseline promotion.

### Ordinary UDP coalescing: the pre-write ownership boundary — 2026-09-20

The follow-up uses actual SCTP rather than the earlier fixed writer-barrier
model. `TestProviderUdpSctpReadyDrainKeepsColdAdmissionBoundary` exercises the
existing larger H1 envelope through a **test-only** carrier adapter, while
keeping the physical legacy P2P writer, 32/four-slot queues, zero-wait provider
callbacks, and the same 468-packet offer. This is not a production P2P policy.

| Ten normal repetitions, median (range) | Production envelope | Wider ready-drain upper bound |
| --- | ---: | ---: |
| Admitted / refused | 300 / 168 (298–300 / 168–170) | 310 / 158 |
| Admitted / refused before the first possible ACK | 41 / 16 | 41 / 16 |
| Physical writes | 300 (298–300) | 234 (231–234) |
| Sampled outstanding pooled-root peak | 77,824 B | 88,064 B |
| DATA retransmits / lost admitted identities | 0 / 0 | 0 / 0 |

First refusal remains 87.467 ms, before the first returned SACK at 122.133 ms.
Larger ready-drain calls therefore improve later service only, leaving the
cold admission failure intact. The pooled-root peak rises **13.2%** despite
unchanged queue slot counts; this does not count Go metadata or SCTP internals
and must not be reported as the complete runtime footprint. A fresh six-run
encoder benchmark improves median time from 1.5085 to 0.8861 microseconds per
30 already-ready packets, zero allocations/op in both arms, but its wire root
grows from 2 to 4 KiB. CPU savings alone do not qualify this change.

Source review places the missing operation **before** the sequence parks in
`route.Write`: a ready-only drain cannot consume later source arrivals while
that owner is blocked. A dedicated compact admission batch was considered
but **not implemented or qualified**. It needs a new bounded owner covering
the compact arena, every original callback/lifecycle record, and any overlap
with frozen/serialized copies. Existing logical groups do not provide that
ownership transfer for independently admitted callbacks; simply merging or
releasing their slots would hide a payload-capacity increase. Pooled arena
subviews also cannot be treated as independently returnable packet roots.
No safe minimal candidate with a demonstrated memory-neutral cap emerged;
this is a scoped rejection, not proof that all compact-queue designs fail.

Normal and race tests ×10, vet, and the encoder benchmark pass. The earlier
synthetic test's assertion of equal **post-release** sampled peaks was too
strong: a preserved race-mode repetition observes 81,920 versus 77,824 B.
It now asserts equal **pre-release** ownership only and records the later
peak as a measurement. No acceptance/memory threshold was relaxed.

Retained changes are tests and research notes only. No new fixture/device A/B
ran because no eligible production candidate reached that stage. The frozen
PERFVAR source gate, absolute 24-MiB ceiling, and baselines remain unchanged.
Current helper hashes, all repetitions, failed draft assertion, and commands:
`urnetwork-udp-sctp-batch.0C7JHL/REPORT.md` (private 0700 directory).

### Legacy SCTP compact backlog and blocked-flight service research — 2026-09-20

The 256-KiB compact legacy send queue added in `018e56d9` is **not yet
qualified by physical/profile or unchanged PERFVAR replay**. It reserves its
fixed owner before creation, charges every retained root against the peer's
shared WebRTC budget, keeps small controls/probes synchronous, and joins its
physical writer before returning the fixed reservation. H1 and the negotiated
native fast lane do not use this backlog. The separate absolute **24-MiB
iOS-profile** gate remains mandatory; none of the following pool measurements
is whole-runtime or iOS physical-footprint evidence.

Repetition exposed two deterministic adjacent defects. A full queue previously
flushed the entire backlog instead of admitting again after one root was
released; `TestP2pLegacySendQueueRefillsAfterOneRelease` fails before the repair
and passes after it without increasing any cap. Async ownership also consumed
an extra readiness probe behind a held small control write. Queue creation now
waits for a bulk message greater than 256 bytes, and small controls remain
synchronous even after bulk use. The existing probe-pressure assertions and
`TestP2pLegacySmallControlKeepsSynchronousOwner` preserve the original bounds.
Broader P2P/WebRTC race validation passes (11.723 s); H1/legacy lifecycle
isolation race ×3 passes (2.281 s).

The unchanged 468-packet/3.75-Mbit/s, 120-ms-RTT fixture revealed a second
limiter after the refill fix. Normal compact arms admit all 468 packets, but
race instrumentation admits only 411–468 in a ten-run untraced cohort. The
pinned SCTP checks whether its *internal pending queue is nonempty* when a SACK
advances cumulative progress. With one-message blocking writes, that queue can
be momentarily empty despite a saturated application backlog. A test-only
logger observes 42–86 skipped-growth ACKs under race versus 0–5 normally.
Yielding the writer does not fix the gate (425–468 admissions under race).

This is not merely a race-detector artifact: a deterministic **50-µs per-write
service cost**, more than 42 times faster than the source's 2.133-ms interval,
reproduces 369–383 admissions and 126–131 skipped-growth ACKs in three normal
runs. Later instrumentation controls measure 376–418 admissions. Offered
traffic and terminal refusals are unchanged; every admitted identity arrives
once, with zero DATA retransmits. Do not infer statistical improvement by
comparing these separate instrumented cohorts.

| Candidate, 120-ms RTT + 50-µs service | Admission / 468, three normal repetitions | Decision |
| --- | ---: | --- |
| Fixed SCTP windows 16 / 32 / 64 / 96 KiB | 307 / 344 / 389 / 363 | Reject: up-front service reservations steal cold backlog capacity. |
| Dynamically charged windows 64 / 96 / 128 KiB | 338 / 377 / 368 | Reject: lower retained ownership, but still fails full offer. |
| Instantaneous pre-SACK full-flight proxy | 390–418 | Reject: ACKs arriving after a service gap still lose the growth signal. |
| Flight-scoped record of an actual cwnd-blocked send | 468 in every run | Promising isolated dependency candidate; loss/protocol and integration validation pending. |

Both service-window sweeps use the *same* 256-KiB free shared budget. Fixed
windows reserve twice their payload cap plus an 8-KiB owner; the dynamic arm
charges twice each live single-fragment payload plus that owner and releases
on acknowledgement. This is conservative test accounting, not a production
fragmented-message ownership proof. Dynamic shared claims stay at or below
786,344 B versus 786,432 B total, and return exactly to the preexisting 512-KiB
owner. No nonblocking SCTP policy or service-window increase was retained.

The flight-scoped experiment records the last TSN actually sent when another
pending DATA chunk is blocked by cwnd, retains eligibility only through that
flight's cumulative acknowledgement, and clears it on a congestion decrease.
It does not set a cwnd floor or alter ACK generation, reliability or loss
timers. In the initial isolated fork, normal ×3 and race ×10 admit **468/468**
at all tested 0 / 50 / 200 / 500 / 1,000-µs service costs, with no retransmits.
At 50 µs under race, pooled roots peak at 245,760–266,240 B and concurrent
pool + SCTP payload at 313,038–320,350 B, versus later blocking controls'
323,584-B pool and 365,542–382,552-B combined peaks. SCTP chunk metadata,
runtime spans, and device memory are not included in that combined number.
The protocol tests cover app-limited/expired-flight no-growth,
fast-recovery suppression, receiver-window versus cwnd marking, TSN wrap,
and clearing eligibility on congestion reduction. Final tracked tests pass
race ×20 (1.251 s). Full upstream SCTP short tests pass normal (38.000 s)
and race (37.824 s). Initial draft protocol fixtures omitted timer teardown;
their preserved failures were repaired with owned pipe/timer cleanup before
these full-suite runs, not waived as harmless leaks.

Loss controls at 2 / 50 / 120 ms, normal and race ×3, preserve exact identity
and byte-budget accounting. At 120 ms with 1% deterministic loss, the candidate
admits all 468 offers and retransmits exactly the four dropped DATA packets.
Dropping the entire initial three-packet flight still gives 248 admitted /
220 refused in both arms, then exactly three retransmits and complete delivery
of admitted identities. That no-ACK interval remains a bounded source refusal,
not a hidden success. The loss regression now asserts exactly one retransmit
per deliberately dropped single-DATA packet; no spurious retransmits are
allowed on the lossless reverse path.

The measured candidate is now tracked as `sctp`, pinned to the
complete upstream v1.11.1 source with only the flight-state repair and its
tests. Explicit replacements in Connect, SDK main/build/cgo/js, server,
proxy, operator-proxy, and sn prevent Go's non-inherited-replacement rule from
silently selecting different implementations. All nine main modules resolve
to this same source. The unchanged upstream cache is not patched. A release
outside this workspace needs an upstream/maintained-fork pin or an equivalent
explicit consumer replacement; installing a new receiver alone does not fix
an older remote sender's congestion controller.

`TestProviderUdpSctpCompactQueueServiceCostPreservesFixedOffer` is red against
unmodified v1.11.1: 383/468 at 50 µs, 422/468 at 1 ms. Integrated focused
normal ×10 PASS (4.515 s), race ×10 PASS (68.050 s); final strict loss,
H1/legacy lifecycle race ×3 PASS (7.566 s); broad P2P/WebRTC race PASS
(10.079 s). SDK memory/P2P/H1/lifecycle normal ×3 PASS (10.918 s), race ×3
PASS (13.931 s), Connect/SDK vet PASS. Server connect/perfvar, SDK build/cgo,
proxy/operator-proxy, and Connect under sn compile checks pass; SDK JS passes
wasm cross-compilation (not a browser runtime test). The operator-proxy check
also required recording the already-used secp256k1 v4.4.1 checksum/indirect
dependency; no dependency version was upgraded. Physical/unchanged PERFVAR
replay may now start against this exact tree, but it has not run here and
neither a baseline nor the iOS-profile 24-MiB gate is promoted.

Reproduce controls with `TestProviderUdpSctpServiceSchedulingExperiment`,
`TestProviderUdpSctpServiceDelayExperiment`,
`TestProviderUdpSctpBoundedServiceWindowExperiment`, and
`TestProviderUdpSctpDynamicServiceWindowExperiment`, using `GOMAXPROCS=4`,
`-p=1`, normal and `-race` repetitions. Keep the existing
`TestProviderUdpSctpCompactQueueAdmitsColdHighRttFixedOffer` assertion unchanged.
Loss/RTT comparisons use `TestProviderUdpSctpGrowthLossSafetyExperiment` and
must report physical retransmits separately from source refusals. Initial
service/lifecycle artifacts are in `urnetwork-p2p-legacy-compact.BBojQF`;
the isolated dependency, modfile, protocol tests, final integrated report and
before/after outputs are in `urnetwork-p2p-sctp-growth.krgD0Z`. Installed module
cache, physical devices, memory policy and baselines are unchanged.

### Unlinked H1 migration owners — 2026-09-20

The current-owner census cannot rule out a retiring, already-unlinked carrier.
An actual H1 regression test now demonstrates a distinct teardown gap after
the earlier current-carrier removal join: `MigrateClientTransport` published
its replacement, called `Close` on the old carrier, and ended its creation
owner without joining old socket/receive cleanup. The pre-fix generator join
returned success with zero indexed clients/workers while an old H1 writer
still held a **16,384-byte** batch buffer. The healthy replacement delivered
traffic during that barrier. This is premature lifecycle completion, not
proof that five healthy connected exits are a leak.

The narrow repair joins every unlinked or discarded migration carrier in the
existing admitted migration worker, outside transport/policy locks. It adds
no worker, queue, budget, timeout, or packet-path operation. Caller deadlines
bound their wait without abandoning retirement ownership. Deterministic
tests cover blocked actual H1 writer and receive cleanup, a healthy
replacement, failed-replacement timeout/cancellation/lost-generation paths,
and SDK `DeviceLocal.CloseAndWait` after an actual H1 migration. At final
completion all captured carrier `Done` channels are closed, the held write
buffer is released, and carrier admission claims are zero.

Five fresh loopback processes per arm each performed eight migrations with
eight distinct payloads; all 80 payloads arrived and all final claims were
zero. Both arms measure full old-carrier completion, so the old early return
is not counted as a speed advantage. Process-median migration times span
425.86–724.34 ms before and 520.47–770.97 ms after (medians 570.20 and
625.63 ms), dominated by unchanged 100–1000 ms startup jitter. The pooled
nested median is 612.18 → 613.67 ms but is **not** 40 independent samples.
These results do not establish statistical speed equivalence or improvement.
Healthy final generator close medians are 1.657 → 1.280 ms. Three fresh SDK
processes per arm give eight-echo traffic medians 204.053 → 203.969 ms and
close medians 0.917 → 1.111 ms, with overlapping close ranges. No hot-path
regression was observed; public Fast.com/TTFB has not been remeasured.

Focused Connect normal ×10, race ×5, opt-in timing under race, SDK migration
normal ×5, SDK lifecycle/migration race ×5, and both package vets pass.
Private evidence, raw per-process timing/resource samples, source hashes,
commands, and invalid draft-fixture attempts are preserved in
`urnetwork-h1-retired-carrier.W4YZai/REPORT.md`.

This validates lifecycle correctness and release of a known buffer, **not** a
multi-MiB steady-memory saving. The immediate fixture heap snapshots retain
test-owned closed clients for assertions and are not post-GC retention
measurements. Existing physical heap evidence has roughly 74 KiB of H1 batch
buffers at each boundary, consistent with healthy active exits. A newly
native-attested, 65,536-byte-rate owner diagnostic is the next attribution
step; it cannot qualify the absolute **24-MiB** gate or promote a baseline.
No memory policy or acceptance threshold changed.

### Fresh iOS-profile burst versus quiet, and an independent history-owner fix — 2026-09-20

The native-attested `urnetwork-cleanarm.kqVGmc` H1 arm still **fails the
absolute 24-MiB gate**: its whole-run peak is **26,353,696 B (25.13 MiB)**,
1,187,872 B over the limit. All three over-limit samples precede quiet.
The valid 339,574-ms quiet boundary contains 22 samples spanning 315,002 ms,
with **zero quiet breaches**, peak **24,723,488 B (23.58 MiB)**, median
23,486,496 B, and final 22,904,864 B (21.84 MiB). The second offline
“quiet-teardown” evaluation uses the same original interval and global peak;
it is not an independent quiet period or evidence of `DeviceLocal.Close`.
The host gate now reports separate whole-run/quiet peaks and breach counts,
with a deterministic regression preserving the global failure even when
every quiet sample passes. No acceptance rule has been relaxed.

At the **349,469-ms** global peak, schema-13 primitives identify 10,566,744 B
of heap objects, 5,252,008 B of in-use span slack, 1,048,576 B of free
unreleased heap, 3,342,336 B of stacks, and 6,137,677 B of other runtime
classes (plus 6,355 B of profiling buckets). There are 257 goroutines,
4 quality + 1 speed clients, and 164 flows, but zero outstanding packet/pool
owners and zero tracked/resend/receive/Pack bytes. The 344,064 B returned
pool is already part of heap, not an additional category. The peak follows
video-site flow fanout and is about 23 seconds after joined browser cleanup.
The next sampled reclaim reports 26,353,696 → 23,470,112 B, a 2,883,584-B
reduction; no later sample breaches the cap. This supports investigating
post-burst object/span lifetime, not an established queued-packet leak.
There is no paired heap profile/census in this rate-zero arm, so its exact
heap owners are still unproven. Five active exits are not post-close residue.

A separate deterministic source audit **does** prove long-session retention
in `IpAssoc.blockWithLock`: slicing off expired blocks left their pointers
in the backing array, keeping matrices reachable outside the visible history.
Clearing only that dropped prefix before advancing the slice releases these
owners without changing retained history, affinity/scoring, memory policy,
queues, or the packet hot path. Default eight 300-second blocks mean first
eviction is around 40 minutes: **this cannot explain the fresh arm's 349-s
peak and is not a fix for its remaining 24-MiB failure**.

The regression uses unchanged production bounds (2,048 entities / 16,384
associations per block), an injected block clock, and weak-owner/GC checks.
Before the fix, eight logically expired matrices remain reachable; after,
none do, while live matrices/names and pressure release remain correct.
Three fresh processes per arm show retained heap deltas **4,271,016–4,276,336 B
→ -1,880–3,440 B** after test-only GC/scavenge (about **4.07 MiB** released;
small signed residuals are baseline noise). Runtime deltas are
5,480,448–5,701,632 → 868,352–1,236,992 B, not a physical-memory measurement.
Five rare-rotation microbenchmarks give median **93.58 → 104.10 ns/op**,
207–208 B/op and 3 allocations/op in both arms: +10.52 ns on block rotation,
normally once per 300 seconds, not a packet-path or H1 throughput claim.

Connect normal ×10, race ×5 and vet; SDK lifecycle/reclaimer normal ×3,
race ×2 and vet; and all **219** Android host tests pass. Raw local repetitions,
source hashes and commands are in `urnetwork-cleanarm-memory.yC94iJ/REPORT.md`.
The observed Fast.com displays were **71 / 59 / 19 Mbps**, all completed;
there is no paired control or baseline promotion. CNN's child exit 2 remains
a remote/media nondeterministic result pending reproduction, not evidence
for a TLS/403 interpretation or a tunnel repair.

Next attribution needs a separately authorized native-attested diagnostic
with 65,536-byte heap sampling and paired owner census before video, after
video, and after joined browser cleanup, including pre/post-reclaim
boundaries. That diagnostic cannot qualify the memory gate. No new build,
device action, policy change, baseline promotion, or commit was performed
for this analysis. The **whole-run 24-MiB barrier remains unresolved**.

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

### iOS burst attribution, scoped measurement plan — 2026-09-20

The `kqVGmc` receipt/sample join places the three absolute-cap breaches at
319,471 / 334,468 / 349,469 ms. Browser cleanup completed at 326,655 ms, so
the last peak was **22,814 ms after cleanup**. Fast.com had completed at
244,056 ms; its last nearby sample was 22,986,784 B. Subsequent flow fanout
reached 301 flows. The profile had a 32-MiB soft Go limit and startup heap
sampling disabled, consistent with the existing iOS audit profile; that soft
limit does not enforce the separate 24-MiB absolute acceptance ceiling.

At the three breach samples, residual runtime classes stay within 8 KiB
(6,129,485–6,137,677 B), and stacks fall from 3,440,640 to 3,342,336 B.
Heap objects, in-use span slack and unreleased free pages carry the excess.
Allocation continues at 263,447 B/s in the final 15-second pre-reclaim sample
interval despite browser cleanup, with three natural GCs. This is cumulative
allocation rate, not an equal increase in retained memory. The existing
15-second quiet debounce begins reclamation only after traffic settles and
runtime exceeds its target; recovery after a breach cannot satisfy the
absolute-cap requirement. Reducing windows or forcing GC earlier is not yet
an attributed fix.

Historical, non-qualifying `arm.DHKCh0` pprof callgraph inspection narrows
three candidates for fresh paired attribution. Of 868,708 B sampled under
`bufio.NewWriterSize`, **801,103 B belong to glog file writers**, not H1
carrier buffers. The current logger reserves 256 KiB per opened severity;
first ERROR logging can open INFO/WARNING/ERROR writers and retain 768 KiB.
A mobile-specific smaller logging buffer is therefore a candidate that does
not spend the packet/sequence window. The profile also attributes 593,802 B
of exported string-list backing to `DeviceLocal.updateBlockActions`, 416,600 B
to gomobile reference tracking from `NewStringList`, and 688,349 B to
`multiClientChannel.addSourceToEventBucketWithLock`. These are historical
sampled owners, not proof of the current breach. Event-bucket retirement
already clears removed pointers. Current paired profiles must establish each
owner's growth/release before retaining a remediation.

The next diagnostic uses only the in-scope Wikipedia/Fast.com H1 workload;
public-egress media, including CNN, is excluded. P2P playback remains a
separate device role/workload. No new host runner is required: the retained
workload owner supports an explicit `wiki,fast-1,fast-2,fast-3` child list plus
verified Chrome cleanup. Build/attest a fresh native 65,536-byte-rate iOS
diagnostic, keep the existing continuous collector and owner receipts, and:

1. Require schema-1 census preflight, then pair idle pre-GC census, heap
   profile, post-GC census and last-of-all private stacks.
2. Run the existing Wikipedia five-load and three Fast.com commands. During
   traffic, collect at most one census-only `active-highwater` when a primitive
   sample first reaches 23 MiB or 128 flows; do not force GC or restart work.
3. At joined browser-cleanup +0 / +15 / +30 seconds collect census only,
   preserving the original +23-second peak window and automatic reclaim.
   Record command times, sample/GC counts and reclaim counters throughout.
4. After +35 seconds collect the full paired `post-burst` boundary; repeat
   at +180 seconds of connected quiet. These interventions are diagnostic
   and cannot qualify the cap or promote performance measurements.
5. Disconnect the route, await the exact command completion, collect a full
   pair after 15 seconds, then finish/join the owners. This command confirms
   route disconnect, **not** `DeviceLocal.CloseAndWait`; avoid a false
   process-wide teardown inference.

Publish aggregate owner/size-class/function deltas only; keep profiles/stacks
private. A breach before intervention remains failure evidence. Absence of a
breach in this narrower diagnostic does not repair the historical failure.
Any retained candidate needs an owner-specific deterministic test, measured
allocation/lifetime improvement, and a fresh rate-zero physical comparison
preserving H1 TTFB/Fast.com performance and every sample ≤25,165,824 B.

### Clean physical P2P fixed-offer replay — 2026-09-20

A fresh native-attested P2P arm on the allowlisted Pixel 8 Pro and Galaxy S24
Ultra completed provider start, exact-peer H1 connection, bidirectional client
probe, provider proof, and joined teardown without retries. Both devices used
the iOS-memory-audit profile. The client recorded 19 samples with a maximum
Go runtime of 20,086,816 B; the provider recorded 21 samples with a maximum of
18,546,720 B. Neither had a sample above 24 MiB or a packet-pressure drop.

This keeps the P2P fixed-offer queue remedy compatible with the mobile memory
envelope on the clean-LAN role split. The device harness cannot force the
legacy SCTP lane or inject 120-ms RTT, so it does not replace the deterministic
120-ms service cohort; that cohort admits 468/468 offers in ten normal runs
with zero refusal/retransmission. It is also not evidence about public H1
TTFB, Fast.com, or the unresolved short-lived iOS-profile burst.

### Scoped H1 diagnostic profile versus rate-zero qualification — 2026-09-21

The freshly native-attested `urnetwork-iosscoped-aapt.paZ8U8` Wikipedia and
three-Fast.com H1 arm used the correct 20-MiB admission / 32-MiB runtime policy,
but **all 45 primitive samples had heap-profile rate 65,536**. Native
before/writer/after provenance and the initial live status agree on that rate:
this was a diagnostic configuration, not a propagation failure or rate-zero
qualification. The old profile preflight validated only the two budgets and
incorrectly allowed the diagnostic build into the qualification sequence.

The measured maximum remains **25,442,584 B (24.264 MiB)**, 276,760 B above the
absolute cap, with four over-limit samples. All four are inside this arm's
quiet boundary (23 samples spanning 330,002 ms). The peak at 499,474 ms has
8,513,768 B allocated heap, 4,355,864 B in-use span slack, 1,089,536 B free
unreleased heap, 3,309,568 B stacks, **1,850,363 B profiling buckets**, and
6,323,485 B other runtime classes. Returned pools account for 164,096 B within
heap; outstanding pooled roots are 114,432 B and tracked transfer memory is
1,318,612 B. These categories are not additive to heap a second time.
The retained session produced no paired heap profile/owner-census artifacts,
so individual live-heap owners at the peak remain unproven.

The profiling-bucket cost alone exceeds the overshoot, but subtracting it from
runtime is **not** a valid production measurement or permission to claim a
pass. Sampling also affects allocation timing and GC, while the historical
rate-zero breach remains unresolved. Neither the 24-MiB limit nor production
queue/pool/GC policy changed for this finding.

`physical_memory_profile.mjs` now defaults to qualification and requires the
live numeric rate to be zero before traffic. Explicit diagnostic mode requires
65,536 and always reports `qualificationEligible=false`. The quiet gate
independently requires rate zero at both status boundaries and in every active,
quiet, and teardown sample; missing/string rates fail rather than defaulting to
zero. A memory breach stays visible with its original byte count even when
the profile is invalid. Deterministic tests reproduce this 20/32/65,536
misclassification, diagnostic opt-in, rate changes outside quiet, missing
evidence, and the absolute gate without subtracting profiling buckets.

For the next qualification, freeze `NATIVE_PROFILE_RATE=0`, pass matching zero
to native and app/test builds, and require the updated ready gate before the
unchanged workload. A future owner diagnostic instead needs fresh paired
pre-GC census, heap profile, post-GC census, and last-of-all private stacks at
matched idle, post-burst, and quiet boundaries. A completed session cannot
retroactively provide peak-owner evidence. Public-egress video remains outside
this scoped arm; no physical performance baseline is promoted by this result.

### Valid rate-zero scoped H1 arm: post-traffic burst remains — 2026-09-21

The canonical `physical_h1_arm.mjs` arm `VtKzNI` completed on the allowlisted
Pixel over Wi-Fi with providing disabled, explicit H1, freshly attested native
and APK inputs, the 20-MiB device admission target, 32-MiB Go soft limit, and
heap-profile rate **zero**. All four workload children and browser cleanup
joined successfully. Five cache-disabled Wikipedia loads measured **450.6 ms
median load / 176.6 ms median TTFB**. The three canonical Fast.com displays
were **90 / 54 / 41 Mbps**, median **54 Mbps**, with all three at least 40 Mbps.
The 384 radio/thermal collector samples were eligible. This is one valid
observation, not a paired performance comparison or a new baseline.

The absolute memory gate **fails**: 2 of 27 primitive samples exceed
25,165,824 B. Both are inside the 21-sample connected quiet interval spanning
300,002 ms. The whole-run and quiet maxima are **29,405,216 B (28.043 MiB)**,
**4,239,392 B above the cap**. The peak occurs at 94,362 ms, **3,617 ms after
joined Chrome cleanup**; calling that phase quiet does not imply all VPN work
has drained. At that instant the components are:

| Runtime class | Bytes |
|---|---:|
| Allocated heap objects | 12,178,752 |
| Unused space inside occupied heap spans | 5,941,952 |
| Free heap pages not yet released | 1,351,680 |
| In-use goroutine stacks | 3,276,800 |
| Other runtime classes, including 6,355 B profiling buckets | 6,656,032 |
| **Go runtime total** | **29,405,216** |

The last GC marked 11,447,120 B live. There are 279 goroutines and 101 indexed
flows. Returned message buffers retain only 373,248 B, outstanding packet roots
account for 240,640 B, and tracked resend ownership is 1,433,844 B; receive and
Pack handoff queues are empty. These owner counters overlap heap objects and
are not additive runtime classes. Nine platform claims total 3,801,088 B;
claims describe reserved permission, not measured allocations.

At the next sample, tracked queues have drained and packet roots fall to 256 B,
but runtime still reaches 25,268,256 B. Occupied-span slack has risen to
6,527,224 B as objects die. The sole automatic reclaim subsequently reports
25,333,792 -> 22,982,688 B. The remaining 19 quiet samples range from
23,474,208 to 24,064,032 B, and the final sample is 23,646,240 B. Recovery
after a breach cannot satisfy an absolute gate; further free-list trimming
alone cannot reclaim space inside still-occupied allocator spans.

This rate-zero arm has no paired heap profiles or owner censuses. It proves
the unresolved burst exists in the Wikipedia/Fast.com scope without public
video or profiling overhead; it does not identify the allocations pinning
those spans. Preserve this failure and collect the separate diagnostic
boundaries described above, then use an owner-specific lifetime/allocation
reproduction before retaining a production remedy. Queue/window sizes,
timeouts, GC settings and all acceptance thresholds remain unchanged.
Normal finish and joined cleanup released all five created clients and their
five markers, removed the staged credentials, and left the target app stopped.

### Canonical owner attribution and packet-allocation corrections — 2026-09-21

A separate native-attested 65,536-byte-rate diagnostic completed the scoped
H1/Wi-Fi Wikipedia/five-load and three-Fast workload on the allowlisted Pixel,
with connected-idle and immediate-post-cleanup pre-GC census → heap profile →
post-GC census → stacks, followed by 45 seconds of retained connected observation.
It is not rate-zero qualification. Wikipedia median load/TTFB was
429.5/164.4 ms; Fast.com displayed 51/85/7.5 Mbps, median 51 (2/3 ≥40).
Primitive sampling observed 26,765,592 B maximum and five samples above
25,165,824 B; profiling overhead remains included and must not be subtracted
to qualify the result. The slow third Fast sample must not disappear behind
the median, nor may this profiled run promote a performance baseline.

Matched post-GC censuses increased runtime from 22,915,352 to 26,446,104 B,
heap objects from 7,777,800 to 9,467,872 B, occupied-span slack from 3,248,632
to 4,827,168 B, and stacks from 2,949,120 to 3,276,800 B. Goroutines increased
236→249. The post-traffic census retained 76 application flows, 34 DNS cache
entries and 37 IpAssoc entities. Receive slots stayed at 640; the eight H1
claims remained a 2-MiB reservation, not additional live heap. The sampled Pack
queue peak was only 23,816 B and packet-pressure drops remained zero. Do not
reduce these throughput windows to explain unrelated allocation/span growth.

The matched private allocation-space profile identified repeated raw protobuf
wrappers (`FromFrame`), reverse-path objects and canonical packet-group address
copies. Current production had reusable raw-byte/value helpers, but these three
call sites still allocated per packet. Narrow corrections now:

- Read raw provider-frame bytes directly through the existing compatible
  helper; keep legacy protobuf decoding unchanged.
- Use a stack value for committed-flow reverse lookup; retain a separately
  owned reverse path on the queued/unknown-flow fallback.
- Copy canonical group addresses only when starting a new group. Appending
  another packet preserves the already-owned group path and all ordering/caps.

Host arm64 microbenchmarks (`GOMAXPROCS=10`, five repeats, 200 ms each) measured
the same fixtures before and after; times below are medians, not phone speeds:

| Operation | Before ns/op | After ns/op | Before→after B/op | Before→after allocs/op |
| --- | ---: | ---: | ---: | ---: |
| Existing-group append | 49.33 | 23.88 | 136→0 | 2→0 |
| Committed single-packet batch | 138.0 | 127.0 | 152→24 | 2→1 |
| Raw channel receive | 193.9 | 169.3 | 280→168 | 6→4 |

`ip_packet_memory_regression_test.go` failed all three exact allocation guards
on the old implementation. It covers raw/legacy/malformed delivery, rare-flow
ownership, and IPv4/IPv6 TCP reset delivery before shared-reaper retirement.
Reproduce the performance guard with
`go test -run '^$' -bench '^BenchmarkPacketMemoryRegression$' -benchmem -count=5`.
These measured local improvements do not yet establish a lower device peak,
unchanged Fast.com performance, or passage of the absolute iOS-profile gate.

Other retained owners remain research candidates, not fixes: the current
profile corroborates three glog file buffers of 256 KiB each (first ERROR
opens two additional severity writers); full-table provider probes temporarily
added hundreds of path entries; the decoded-owner pool grew to its bounded
working set. These need separate measured remedies rather than a blanket
flow/window reduction. Post-profile Go GC/scavenging and the background probes
also prevent treating census differences as an isolated live-object leak.

The arm's only terminal harness failure was retained-client cleanup. A separately
authorized cleanup replay released all six remaining clients and removed their
six markers. Original API/transport details had been discarded, so the failure
cannot honestly be assigned a specific remote cause. The cleanup helper/runner
now retain an exclusive mode-0600 sanitized failure receipt with fixed operation,
HTTP/known transport classification and partial counts; no raw exception text,
client identifiers, credentials or endpoints are published. The original arm
remains non-qualifying and its failed terminal record is not rewritten.

### Rate-zero repeat variance and mobile logging retention — 2026-09-21

Two fresh allowlisted Pixel/Wi-Fi, provider-off H1 arms used the same
`2db4f81e` packet-allocation build, 20-MiB admission target, 32-MiB Go soft
limit and profile rate zero. They must be recorded together:

| Metric | Arm A (`Ujt2U0`) | Immediate repeat (`hABWOK`) |
| --- | ---: | ---: |
| Whole-run runtime maximum | 24,891,424 B | 25,235,488 B |
| Margin below 25,165,824-B cap | 274,400 B | **−69,664 B** |
| Samples above cap / primitive samples | 0 / 27 | **1 / 34** |
| Quiet runtime maximum | 24,563,744 B | **25,235,488 B** |
| Final runtime sample | 23,973,920 B | 24,047,648 B |
| Wikipedia median load / TTFB | 540.8 / 209.1 ms | Not qualified |
| Fast.com median / runs ≥40 Mbps | **24 Mbps / 0 of 3** | Not qualified |
| Workload/qualification | Valid; sampled memory passes | One workload child failed; memory fails |
| Session cleanup | Complete | Complete |

Arm A does not establish the 40-Mbps speed goal or justify baseline promotion.
The repeat's workload failure makes its throughput comparison invalid, but
does not erase the independently observed memory breach. Its maximum occurs
3,805 ms after joined browser cleanup, with 43 indexed flows and 273
goroutines. At that sample returned pools retain only 278,528 B, packet roots
1,280 B, and tracked resend/receive/Pack ownership zero. At the next sample
runtime falls to 24,703,008 B without an idle trim. No threshold is relaxed,
quiet sample excluded, or forced collection inserted to claim a pass.

The repeat peak is 344,064 B above arm A's peak, despite fewer flows and less
allocated heap. The exact non-overlapping class comparison is:

| Runtime class | Arm A peak B | Repeat peak B | Repeat minus A B |
| --- | ---: | ---: | ---: |
| Allocated heap objects | 9,458,688 | 9,117,872 | −340,816 |
| Unused space in occupied spans | 5,229,568 | 5,316,432 | +86,864 |
| Free unreleased heap pages | 1,261,568 | 1,327,104 | +65,536 |
| In-use goroutine stacks | 3,080,192 | 3,342,336 | +262,144 |
| Other runtime classes | 5,861,408 | 6,131,744 | +270,336 |
| **Total** | **24,891,424** | **25,235,488** | **+344,064** |

These peaks occur at different traffic/lifetime points. The counters identify
insufficient runtime/allocator headroom, not a newly proven retained-object
leak, and do not justify shrinking H1 windows. In particular, rate-zero
counters cannot assign the stack/runtime delta to one production allocation.

The earlier paired owner diagnostic does establish a separate removable fixed
owner: INFO, WARNING and ERROR each retain a 256-KiB `glog` file buffer after
the first ERROR opens the latter two. `glog_file.go` now selects **32 KiB only
for Android and iOS** from immutable `runtime.GOOS`; Darwin desktop, Windows,
Linux/server and other platforms remain at **256 KiB**. Three open severity
buffers retain 98,304 instead of 786,432 B: an exact **688,128-B (672-KiB)
capacity reduction**. This is not an equal-sized guarantee of a sampled
Go-runtime reduction: span reuse and scavenging determine when physical
runtime pages change. No logging severity, routing, flush interval, immediate
high-severity flush request, disk-sync behavior, queue/window or GC policy is
changed. Large entries continue through the writer without growing its buffer.
Filling a smaller buffer can issue an underlying write sooner; the continuous
benchmark below deliberately includes that cost.

Host arm64 real-file fixtures, five 200-ms repeats with `GOMAXPROCS=10`, used
256-B log lines and bounded temporary files. Median costs, not phone speeds:

| Fixture | 256-KiB default ns/line | 32-KiB mobile ns/line | Mobile extra ns/line |
| --- | ---: | ---: | ---: |
| Buffer only; flush each 32 lines | 26.40 | 28.01 | +1.61 |
| Buffer only; continuous logging | 11.37 | 13.29 | +1.92 |
| Complete file sink; flush each 32 lines | 58.91 | 59.16 | +0.25 |
| Complete file sink; continuous logging | 43.54 | 46.13 | +2.59 |

All fixtures measured zero B/op and zero allocs/op. Continuous file-sink
logging is about 5.9% slower in this deliberately saturated microbenchmark;
the memory saving is not presented as a logging speedup. Phone disk-write
latency, application TTFB and Fast.com impact still require a fresh arm.
The complete-sink fixture includes locking, existing-file checks, severity
routing and the rotation check, but excludes message formatting and fsync;
neither of those excluded operations was changed.

### Post-glog rate-zero scoped H1 qualification — 2026-09-21

A fresh canonical, provider-off H1-over-Wi-Fi qualification on an allowlisted
Android phone rebuilt and attested the `glog` mobile-buffer change
(`80a11b4`) together with the Connect documentation/state change
(`325e952a`). It used the unchanged 20-MiB mobile admission target, 32-MiB Go
soft limit, rate zero, five Wikipedia loads, three Fast.com loads, and the
five-minute quiet window. It is the first post-glog physical result; its raw
artifacts, device identity, credentials, profiles, and logs remain private.

| Evidence | Result |
| --- | ---: |
| Primitive samples / profile rate | 28 / 0 |
| Whole-run Go-runtime maximum | 23,646,240 B (22.55 MiB) |
| Absolute 24-MiB margin / samples above cap | 1,519,584 B / 0 |
| Connect-phase maximum | 23,646,240 B across six samples |
| Quiet maximum / final quiet sample | 23,466,016 B / 22,695,968 B across 21 samples |
| Peak live heap / stack in-use | 8,720,192 B / 3,178,496 B |
| Wikipedia load / TTFB median | 355.3 / 127.7 ms (five samples) |
| Fast.com | median 40 Mbps; 3/3 met the 40-Mbps policy |
| Packet pressure, H1 receive drops, ACK drops, Pack drops | 0 / 0 / 0 / 0 |
| H1 backpressures / timeout resends | 5,057 / 24 |
| Cleanup / terminal class | complete / `SCOPED_H1_COMPLETE` |

This clears the absolute iOS-profile memory gate for this scoped observation:
every primitive sample, including connect and quiet samples, is at or below
24 MiB. It also meets this arm's Fast.com policy. Compared with the preceding
packet-allocation arm A (24,891,424-B maximum, 24-Mbps Fast median) and its
invalid repeat (25,235,488-B maximum plus a workload failure), the maximum is
1,245,184 B lower than arm A and 1,589,248 B lower than the repeat. This is
evidence that the fixed logging capacity supplied meaningful headroom; it is
not a statistical baseline promotion or a claim about a public exit fleet.

`glog_buffer_test.go` reproduces the old mobile budget failure and guards
unchanged desktop/server capacities, INFO-through-FATAL fan-out/flush requests,
large ERROR contents, unchanged buffer capacity after large writes, rotation
and pending-byte preservation/old-file closure, and zero-allocation steady
ERROR delivery. Normal and race suites pass; repeat the focused cases with
`go test -race -run '^Test(LogFileBuffer|MobileFileSink|LogFileRotation)' -count=5`.
Run both cost fixtures with
`go test -run '^$' -bench '^(BenchmarkLogFileBufferPolicy|BenchmarkFileSinkBufferPolicy)$' -benchmem -benchtime=200ms -count=5`.
This source fix remains **device-peak unqualified pending a fresh rate-zero
arm**, with the same absolute 24-MiB gate and unchanged workloads.

### Go 1.27 portable-SIMD research: H1 masking and CPU work — 2026-09-22

**Research only; no production source, module version, buffer/window setting,
or baseline was changed.** Connect `2201b2cc` / server `d8cc7a28` were measured
on Darwin/arm64, Apple M4 Pro (14 logical cores), `GOMAXPROCS=10`.
`GOTOOLCHAIN=go1.27.0 GOEXPERIMENT=simd` built and ran the portable API with
native 128-bit vectors; disassembly confirms NEON XOR. Both comparison arms
used that same toolchain/experiment. A private workspace and overlays replaced
only Gorilla's mask loop in the candidate; the dependency cache was untouched.
Candidate mask-source SHA-256:
`a19e51b27d89fee7ee9191903952b4a92f65c8f3e1173b96bef3931ef3c2d56a`.
The [portable API](https://pkg.go.dev/simd) remains experimental; see the
[Go 1.27 release notes](https://go.dev/doc/go1.27).

CRC32C is not an unoptimized byte loop: Go's canonical Castagnoli table already
dispatches to hardware CRC on supported ARM64/x86 CPUs. The existing SCTP
checksum's 128/1280/16384-B medians were 10.87/103.0/1385 ns under Go 1.27,
zero allocations. A private outgoing-only one-pass version measured
5.993/100.8/1387 ns; it cannot replace immutable incoming verification and
showed no bulk advantage. Negotiated SCTP zero-checksum avoids the work on
compatible DTLS peers. No CRC change is retained. Profiled SHA work was the
fixture's content verification; SACK interval processing is not a contiguous
SIMD kernel. Neither justifies a new product SIMD loop.

#### What H1 masking covers

Gorilla masks client-to-server data/control writes and unmasks server reads;
server-to-client frames are unmasked. Thus an application's upload masks on
the device and unmasks at exchange; its download masks on the provider and
unmasks at exchange. The downloading device masks returning ACK/control
traffic, not the bulk download. These are H1 roles, not H3/P2P payload paths.
Large TLS batches do not imply one large mask call: with the measured 4-KiB
WebSocket buffer, 12/16-KiB messages make three/four 4-KiB client mask calls.
Server reads can split these further. Mobile buffer/profile choices still
need their own measurement.

#### Forced-mask, high-volume production H1 socket fixture

The fixture uses the production client WebSocket → ready-batch → TLS → real
TCP path, checks every payload byte and framing/completion, and exercises
1280/12288/16384-B messages. Ten fresh alternating A/B process pairs per size,
two-second timed samples, delivered 31.36/41.08/35.32 GB in the scalar arms
and 32.50/41.25/35.47 GB in the SIMD arms, excluding benchmark calibration.
CPU is whole-process user+system time for both endpoints, not kernel-loop time
or measured battery energy. Point estimates below use paired median log
ratios; CIs are 20,000 paired-bootstrap resamples, initial seed `20260922`.
CPU reduction means `1 - candidate/control`, positive is less work.

| Message bytes | Throughput MB/s scalar → SIMD | Paired throughput gain, 95% CI | CPU ns/message scalar → SIMD | Paired CPU reduction, 95% CI |
| --- | ---: | ---: | ---: | ---: |
| 1280 | 1310.045 → 1355.200 | +3.52% [2.01, 4.36] | 2650 → 2581 | +2.67% [2.11, 3.21] |
| 12288 | 1694.755 → 1719.205 | +1.69% [1.16, 2.24] | 20738.5 → 20466.5 | +1.52% [0.98, 2.10] |
| 16384 | 1474.855 → 1500.635 | +1.55% [−0.89, 2.47] | 30496 → 30065.5 | +1.39% [−0.49, 2.03] |

CPU ns/delivered byte were 2.0703→2.0164, 1.6877→1.6656 and
1.8613→1.8351 respectively. Normalizing that same whole-process work by the
two mask/unmask passes gives 1.0352→1.0082, 0.8439→0.8328 and
0.9307→0.9175 ns/masked byte; these are **not mask-kernel-only costs**.
CPU fell in all ten 1280-B and all ten 12-KiB pairs. The conservative exact
sign-permutation test of the median statistic gives raw throughput p-values
0.03125/0.03125/0.13672; Holm-adjusted across the three payloads:
0.09375/0.09375/0.13672. CPU raw values are 0.03125/0.03125/0.125
(adjusted 0.09375/0.09375/0.125). This is a small CPU-efficiency research
signal, not a promoted statistically/practically qualified product gain.

All 60 final size/arm/process observations passed, at unchanged **57 B/op and
3 allocs/op**. The mask kernel itself remains zero-allocation. Maximum sampled
host runtime (`Sys-HeapReleased`, both endpoints) was 20,860,637 B scalar /
20,815,565 B SIMD; no statistically established memory increase was observed.
This is not an iOS-profile device qualification. Separate 1-in-256 call timing
and exact byte/count instrumentation reconciled masked/unmasked bytes and
estimated 6.3–7.1% mask-loop share of this fixture's process CPU. That estimate
includes timer overhead and is not a hardware-cycle measurement.

#### Unchanged complete PERFVAR result and guardrails

With the attested local test environment, five fresh processes per side each
ran five 32-MiB `exchange-h1/clean-lan/tcp` repetitions in both directions,
one hop, no extenders, `mobile-surrogate`, seed `20260810`. All 100 runs were
payload-correct; all measured timeout, carrier-change, selective-gap, tail and
cumulative recovery writes were zero, and pool outstanding ended at zero.
Preserve calibration-invalid downloads: scalar **7/25**, SIMD **2/25** failed
the unchanged requirement that underlay be at least 10% faster. No invalid
numeric throughput was included; all 50 uploads were valid.

| Direction | Median process goodput Mbps scalar → SIMD | Paired effect, 95% CI |
| --- | ---: | ---: |
| Download | 805.995 → 807.683 | −0.219% [−0.960, +0.618] |
| Upload | 478.449 → 478.432 | −0.193% [−0.360, +0.096] |

The full-stack throughput result is **indistinguishable**, not a speed win.
All-endpoint simulator heap+stack peaks were 143,925,248/144,547,840 B and
both arms recorded 100 samples above the existing 24-MiB counter. Preserve
those observations: fixture memory is not a phone/extension memory value and
cannot qualify the absolute iOS gate. Allocation and host-memory comparisons
did not establish a regression, but neither arm is a memory qualification.
A separate instrumented complete run counted 476,392 mask/unmask calls and
743,198,518 masked bytes. Sample extrapolation was 44.54 ms of mask time versus
38.63 s process CPU, about **0.115%**, including timer overhead and full
setup/calibration. This attribution helps explain the flat full-stack result;
it does not assert every real workload has the same share.

Actual overlaid Gorilla bounds/alignment/key-position/fragment-continuation,
framing/control/EOF and zero-allocation tests pass normally and under race
three times. Its entire upstream suite has a pre-existing Go 1.27
`rand.Seed` no-op incompatibility in `TestPreparedMessage`, reproduced with
unmodified control; both full suites pass with test-only
`GODEBUG=randseednop=0`. Timing arms did not use that override. Android/arm64
and iOS/arm64 candidate library cross-builds pass (`CGO_ENABLED=0`); this is
not a gomobile/app release build. A bounded scalar `xctrace` CPU Counters
capture completed but spent over two minutes finalizing; no paired
instruction/cycle or energy result is claimed. All raw artifacts stay private.

#### Next: Android CPU/energy qualification, not production adoption yet

The SDK's gomobile/Go 1.27 `gotypesalias=0` toolchain boundary is resolved
(sdk `f36da2ab`, 2026-10-05: the mobile binds no longer set that GODEBUG).
Keep SIMD helpers out of exported bindings. Use only the existing
two serial-allowlisted phones, with identical attested Go 1.27 builds differing
only in masking. Predeclare CPU/energy metrics, practical margins and cohort
size from pilot variance, then run order-balanced paired sessions on each
Wi-Fi/cell underlay. Separate device upload, device download and provider H1
roles; measure both fixed offered rate (including 40 Mbps) and saturation.
Match useful bytes, TLS/frame distribution, thermal state, charge state,
display/background load and radio conditions. Collect scoped app CPU time
per delivered byte, cycles/instructions where available, and per-app/rail
energy only where attribution is reliable; otherwise label CPU an energy
proxy, not a battery claim. Keep all correctness/recovery/TTFB/Fast.com gates
and the **unchanged 25,165,824-B absolute iOS-profile gate**. A repeatable CPU
or energy saving can justify further work even below 5% wall-time improvement,
but this host result alone does not authorize a production change.

### 2026-09-23: short-H1 duplex burst scheduling, without larger windows

The eight-core SDK replay exposed a real edge at 300 us RTT: one direction
delivered 861.337600 Mbps against the unchanged 861.428736-Mbps gate (90% of
its 957.143040-Mbps constant-window control). This is `synctest` virtual
goodput, not a host CPU-speed measurement. Both directions share ordinary
data/ACK FIFOs. The reverse window was full in 82/100 traced samples and below
the existing pacing flight allowance in all 100; no controlled drain, drop,
or retransmit explained the interval. A ten-millisecond measurement bucket
also admitted a ten-millisecond physical burst, delaying opposing ACKs.

The candidate separates measurement from physical dispatch. An adequately
provisioned H1 flow releases at most one quarter of the measurement interval
on short paths (2.5 ms), scaling up with one quarter of measured path RTT
without exceeding the old burst. Measurement qualification, ACK protocol,
window retention, peer permission, shared memory limits and serialization
payment remain unchanged. The old feedback-jitter allowance is retained:
smaller local bursts do not prove that an opposing sender has also shortened
its bursts. Tight hard windows below one configured-target ACK-compression
interval keep their original batching; splitting those batches can starve
per-lane cumulative ACK credit. Unknown permission/timing also keeps the old
behavior. The quantum is cached on feedback, with no per-packet ring scan,
new timer, payload allocation or per-lane multiplication of burst permission.

Broad testing exposed another necessary bound: fine dispatch cannot assume
better timer precision than the host supplies. With forced 3-ms timer
lateness, a fixed 2.5-ms quantum reduced measured service until an indivisible
frame waited on every late wake (42.13 Mbps at 16 KiB, 152.22 Mbps at 64 KiB).
The candidate now amortizes actual pacing-timer lateness with a decaying peak,
at most the original burst horizon. ACK, route and FIFO waits are not inputs,
and punctual wakes restore fine dispatch. The same twelve-second tests then
delivered 993.03 and 997.85 Mbps, respectively, with all per-second intervals
healthy. A deterministic direct test covers recovery, the old burst cap and
indivisible-frame progress; existing delayed-wake/debt tests remain intact.

Rejected counterfactuals were important: adding a full burst to retained
windows restored reverse speed but reduced forward/aggregate goodput;
continuous early head ACKs inflated retained queue residence, and using a
proved unloaded residence did not recover the fixed-phase aggregate. A
uniform 1-ms release regressed a constrained eight-flow upload, while uniform
2.5-ms releases regressed constrained provider duplex. Hard-window scoping
preserves those cases; even scoped 1-ms release missed one forced-phase gate.
No ACK-format, ACK-timer, RTT-sizing or window-budget change was retained.

Pilot measurements on Go 1.26.7, darwin/arm64 (M4 Pro), `GOMAXPROCS=8`, use
the original 1,280-byte packet stream, 300-us RTT, one-second measurement and
unchanged warmup. Five declared initial-ACK phases expose startup coupling;
these are ordered before/after observations, not independent repetitions or
a statistical superiority result. Aggregate modeled Mbps for SDK device H1
with providing disabled, one flow each direction:

| Initial ACK release | Old burst | Scoped quarter burst |
| --- | ---: | ---: |
| Unforced | 1,667.13 | 1,829.43 |
| 205 ms | 1,499.17 | 1,718.01 |
| 207.5 ms | 1,462.84 | 1,718.06 |
| 210 ms | 1,763.51 | 1,691.20 |
| 212.5 ms | 1,410.73 | 1,760.84 |

The five-phase median increased 14.6% (1,499.17 to 1,718.06 Mbps), with one
phase 4.1% slower. Candidate reverse directions ranged 925.21–957.31 Mbps;
all unchanged per-direction/aggregate gates passed. The candidate forward
permission stayed 1,635,778 B, reverse retained windows 1,966,786–2,301,257 B,
and maximum FIFO occupancy 1,148,525–1,515,073 B, below the unchanged 8-MiB
queue cap. All five recorded zero drops and timeout retransmissions. Adjacent
constrained SDK provider H1 eight-flow upload delivered 957.35 Mbps and
bidirectional traffic 754.49/956.75 Mbps; all five provider startup phases
passed. This does not establish physical Fast.com speed or iOS qualification.

Deterministic regression coverage now checks shared one/eight-writer prefix
envelopes, hard-window eligibility boundaries, unchanged feedback-flight
tolerance, preserved payment when a quantum shrinks, and indivisible frames.
`TestWindowPathSdkDuplexShortDispatchPhases` keeps both the device and
constrained-provider five-phase cohorts under the original gates. Run it
alongside `TestWindowPacing*`, `TestWindowPathSdkBidirectional*` and
`TestWindowPathRetainedSdkCumulativePacingShort`, with `GOMAXPROCS=8`,
`-count=1` and race separately; never select a lucky repeat or extend warmup.

The complete-policy SDK provider memory cohort passed all three fresh two-core
processes: sampled Go runtime peaks 28.039, 28.035 and 29.207 MiB. The earlier
scoped-quarter cohort, before timer-lateness adaptation, also passed at
27.648, 27.562 and 26.769 MiB; this is not a paired memory-impact comparison.
Each
completed 4,718,592 TCP bytes and 192 UDP round trips under unchanged GOGC=10,
32-MiB soft limit, 20-ms sampling and 31-MiB synthetic Darwin peak gate.
This process includes six local peers and is **not** an iOS extension
footprint or a waiver of its absolute 24-MiB gate. No H1+ memory/energy
attribution follows from this result. Broad normal/race and isolated
reservation CPU/allocation checks must also pass for this candidate.

An isolated reservation microbenchmark (same host/Go version,
`GOMAXPROCS=2`, five 300-ms repetitions per arm) measured median 49.12 ns/op
before and 50.16 ns/op after, a 1.04-ns increase; both were 0 B/op and
0 allocs/op. These are ordered pilot runs, not a statistically established
CPU regression or saving. The benchmark excludes the timer callback and
whole-stack packet processing; virtual goodput is not an energy measurement.
The extra policy state is bounded per existing service/waiter, not per packet.
Qualification is not complete. The final broad normal run retained one
failure in `TestWindowPathServiceRoundTripGrowthBeyondOldRing`: after a
300-us to 1.2-s RTT change, its three measurement intervals were
4.98/380.66/66.79 Mbps against approximately 95.77 Mbps per control interval.
The aggregate therefore hid a real sustained-delivery gap. Five focused
candidate runs, ten old-burst counterfactual runs and a declared three-cohort,
eight-change-phase probe all passed; none erases that failure or proves its
attribution. Quality-change/recovery triage and the affected/adjacent race
suite remain open. Do not declare release readiness from the short-path win.

The follow-up isolated a concrete estimator defect, rather than explaining
the bad interval as host load. `TestWindowPacingRecoveredPrefixCannotRaiseService`
supplies eight lanes with one MiB each, independently serialized at
12.5 MB/s, loses each lane's first identity, changes path quality, then releases
the repaired cumulative prefixes 20 ms apart. The cumulative heads retain
validated receiver waits of 1.13–1.27 s, far beyond the advertised 10-ms ACK
compression. Previously the estimator correctly refused to retime the entire
prefix using only its head's wait, but then discarded the evidence of that
wait. Raw release arrivals falsely raised shared service to **52.4288 MB/s**.
All eight MiB were credited exactly once; the error was capacity inference,
not duplicate ownership. This is a deterministic estimator reproduction, not
proof that every detail of the earlier intermittent worker failure is identical.

The narrow repair keeps a fixed-size provenance boundary for a newly credited
multi-item prefix whose validated head wait exceeds ordinary compression.
Its bytes still repay ownership and keep their raw arrival clocks, but an
interval touching that release cannot raise service; the same ambiguous bytes
cannot seed the shared aggregate sizing fallback. Independent later pairs can
raise capacity, and measured slowdowns remain valid. The regression now holds
12.5 MB/s through all eight repaired prefixes and then accepts a fresh
25-MB/s serialized train. Controls preserve ordinary compression, zero wait,
legacy peers without metadata, unvalidated tags, selective-only credit,
already-SACKed prefixes, prior-generation filtering and reordered callbacks.
No ACK format/timer, loss gate, window ceiling, memory budget or warmup changed.

On this candidate the complete normal window/ACK selection passed in 267.168 s
at `GOMAXPROCS=8`. The original short duplex cell delivered 884.43/942.17 Mbps
(1,826.60 aggregate), and the long-RTT worker case delivered
95.76/95.78/95.76 Mbps against approximately 95.77-Mbps control intervals,
with zero measurement drops. A bounded unit/adjacent race selection covering
`TestWindowPacing*`, `TestWindowQuality*`, ACK compression, burst tails and
receiver timing passed in 4.442 s. The earlier overbroad race attempt was
explicitly stopped after approximately fourteen minutes: it is **incomplete,
not a passing run**. The replacement bounded worker race selection passed in
147.644 s: the exact short-duplex cell, long-RTT growth, delayed pacing wakes,
and constrained initial/later-feedback cases. Runner-owned three fresh
confirmations and the complete parent campaign remain required; no physical
speed or iOS memory qualification is implied by these virtual-time results.

Reproduce the prefix mechanism directly, then retain actual-worker qualification:

```sh
GOWORK=off GOMAXPROCS=8 go test -count=1 -run '^TestWindowPacingRecoveredPrefix' .
GOWORK=off GOMAXPROCS=8 go test -count=1 -run '^(TestWindowPathServiceRoundTripGrowthBeyondOldRing|TestWindowPathDispatchQualityPhaseProbe)$' .
```

The final three-process SDK memory cohort also passed: runtime peaks
28.109/28.531/28.152 MiB, 4,718,592 TCP bytes and 192 UDP round trips in every
child, fixed two cores, GOGC=10 and unchanged 31-MiB synthetic ceiling. The
20-ms sampler's largest TCP gaps were 20.34/20.50/21.20 ms. These runs overlapped
bounded validation work, not paired idle-host energy measurements. A separate
five-by-300-ms, two-core microbenchmark after those connect/runner tests ended
reported medians of 50.23 ns for dispatch reservation, 179.7 ns for service
estimation, 898.8 ns for one-head coalescing and 1,273 ns for a 32-envelope
head. All four were **0 B/op, 0 allocs/op**. The coalescing/estimation figures
are candidate-only measurements, not a before/after CPU claim. Provenance adds
one timestamp per existing shared service, not payload storage or a new queue.
An additional full-history rejection benchmark measured the guard while every
candidate interval is ambiguous: median 5.602 us/read, still 0 B/op and
0 allocs/op (same five-by-300-ms protocol). This bounded recovery scan is more
expensive than ordinary estimation and must not be described as a CPU saving;
it does not add an unbounded history or move estimation into every packet write.
The fresh SDK memory cohort and its telemetry helpers also passed with the
race detector (13.854 s); instrumented memory values are diagnostic, not an
alternative to the normal-build ceiling.

### 2026-10-08 H1 recovery and physical qualification checkpoint

Current qualification still uses the 32/32-MiB iOS profile on the allowlisted
Android devices and the separate 64/64-MiB normal Android profile. No new
physical steady-memory result is qualified: the latest Pixel arm failed during
authentication discovery, before traffic or runtime samples. Its normal
instrumentation exit is not a passing workload or memory measurement.

Private H1 recovery research exposed a deterministic mixed-prefix defect.
When a cumulative ACK covers an accepted retry followed by an unresent
original, treating any recovered item as permission for another early retry
erases genuine original progress deferral. The RED control failed with a new
grant and immediate retry; the complementary recovered-final-position control
passed. Evidence:
`/private/tmp/urnetwork-h1-progress-v16-exec.tmRTCM/`. The proposed correction
uses the newest cumulatively covered position per exact route for early-retry
funding, while keeping legacy lane promotion separate. It adds no payload
queue or memory allowance; correctness and measured performance approval
remain required before adoption.

The preceding full private Connect parent failed twelve tests and reached its
overall 20-minute bound. Focused ownership/race passes therefore do not qualify
that candidate. See `tests/PERFVAR-MEASUREMENTS.md` for the failure matrix and
raw evidence. Startup diagnostics and narrowly owned failed-arm cleanup are
being verified before a fresh native device build; they do not substitute for
active, burst, drain and quiet-window measurements under the profile ceilings.

### 2026-10-08 physical iOS32 subgate and unresolved traffic collapse

Fresh native/AAR/APK inputs were attested for the Pixel iOS-v2 32/32-MiB,
rate-zero arm. It passed startup, profile and H1 readiness, and collected real
traffic plus a complete 21-primitive quiet window spanning 300,001 ms. The
retained memory subgate passed:

| Retained measurement | MiB |
| --- | ---: |
| Overall runtime peak | 24.500 |
| Quiet peak | 24.313 |
| Teardown peak | 22.883 |

There were 159 retained events across primary/status/diagnostic/teardown scopes,
zero observed breaches and zero sampler/observer drops. Device, observer and
drainer joins/flushes completed. This is sampled evidence, not a continuous
peak or iOS kernel-footprint guarantee. Artifacts:
`/private/tmp/urnetwork-h1-ios32-pixel-wifi-v4.MDIwEN/`.

The full arm nevertheless failed and is **not qualified**. Five Wikipedia
loads completed (median load/TTFB 1006.1/531.8 ms). Fast #1 displayed 11 Mbit/s
and its whole-interface interval received about 23.68 MB; Fast #2/#3 exhausted
their unchanged 90-second display deadlines, with only about 1.82/0.20 MB
received. These interval totals are not page-exact. Page-only CDP throughput
(for example 0.05 Mbit/s) excludes worker downloads and must not be reported
as Fast's measured speed. A separate host API connection timeout left one
owned client-cleanup marker; normal credential cleanup succeeded.

Wi-Fi/VPN identities stayed stable and thermal status remained zero. No
recorded packet-pressure, H1 queue/Pack handoff drop or route-write error
explains the decline. Carrier reservation approached 8,339,456/8,388,608 bytes,
while transfer ownership peaked around 4.18/21.81 MB. ACK-stall/retransmit
observations are not bound to the failed page flows, so neither carrier pressure
nor a specific provider is yet established as the cause. Investigate exact
claim/flow lifetime and preserve fixed request-error categories before changing
budgets, timeouts or routing. This is meaningful memory evidence under some
delivered load, not a completed full-workload or Android64 qualification.

Private deterministic capacity research also isolated an inherited H1 limiter:
after a 64-KiB-to-2-MiB permission increase, compressed startup feedback kept
pacing near 705,144 B/s. RED reproduced the release failure. The reviewed
owner-local requalification candidate delivered about 95.83 Mbit/s versus the
old 46.63 Mbit/s in the unchanged compressed-ACK growth cell; all six cells and
adjacent negatives passed ordinary/race, with zero measurement/relay drops.
It retains the 90% criterion, shared service evidence, congestion revocation
and budgets. Struct/allocator measurements remain 2,432/2,688 bytes, one
allocation per sequence, by using existing padding. Evidence:
`/private/tmp/urnetwork-h1-permission-green-exec.IYDGLk/`. This is a private
component improvement; full-parent, repeat confirmations and real-device
performance/memory qualification still precede adoption.

### 2026-10-08 fresh-process capacity confirmation

The private H1 permission-growth correction passed three additional
fresh-process ordinary/race confirmation rounds: 258 top-level checks across
12 processes, no failures/skips, unchanged six-cell 90% throughput criterion,
and no relay/measurement drops or timeout resend writes. Resource guards held.
Evidence: `/private/tmp/urnetwork-h1-permission-confirm3.gseMwJ/`.

This establishes repeatability of the component result, not live adoption.
The candidate includes other accumulated private H1 corrections, so integration
must isolate the intended production changes and qualify their composed parent
and physical workload. Do not copy diagnostic overlays wholesale into live code.

### 2026-10-08: cold flight size trades loaded latency for bulk speed

Four private, source-frozen H1 diagnostic cells used the unchanged 65,536-byte
download, original application MTU, 8-second TCP/Transfer retransmit caps and
version-2 probe deadlines. The treatment changed only the provider's configured
resend-window maximum/minimum from 2 MiB/256 KiB to 16 KiB/16 KiB.
This PERFVAR provider has **no memory budget**, so the treatment is a fixed
window, not an adaptive opening. Pure controls proved that delivery evidence
and a larger peer advertisement do not make this nil-budget window grow.

| Diagnostic cell | Bulk duration | Goodput, MB/s | Loaded probes, successful/offered | Successful loaded p95 |
| --- | ---: | ---: | ---: | ---: |
| Cell-edge control | 10.292 s | 0.006368 | 32/61 | 6.075 s |
| Cell-edge fixed 16 KiB | 12.765 s | 0.005134 | 75/75 | 3.178 s |
| Clean control | 7.163 ms | 9.149 | 8/8 | 4.673 ms |
| Clean fixed 16 KiB | 55.973 ms | 1.171 | 56/56 | 4.595 ms |

All cells delivered the correct payload with complete, zero-overflow causal
recorders. The clean control nevertheless **failed calibration**: its direct
underlay was slower than its tunnel. Preserve that exit-1 record; the clean pair
is not qualified performance evidence. All four are traced, single-process
diagnostics, not statistical comparisons or baseline promotions. Probe counts
depend on bulk duration, so the changed denominators are not themselves wins.

The impaired-link timing does demonstrate earlier reply interleaving. Before
the first reply existed, control wrote 23 original bulk frames (71,624 bytes);
treatment wrote one (3,444 bytes). The first reply's edge-write-to-device-read
age fell from about 9.266 s to 0.619 s. The treatment still had packet loss and
more inner TCP repeated bytes; it did not win merely by eliminating retries.
Provider Transfer resends remained zero in both cells. Keep inner TCP repeats,
Transfer retries and outer TCP retransmissions as separate counters.

There is also a coupled effect: changing the window reduced the derived startup
pacing allowance from 2,958,580 to 32,768 bytes. This experiment does not isolate
flight size from that allowance. A small established service-rate snapshot does
not prove the body is paced at that rate while startup allowance remains.

**Reject fixed 16 KiB as a product fix.** It improved impaired-link latency but
slowed that bulk transfer about 24%, and the clean diagnostic shows a substantial
startup cost. Next research must distinguish a bounded startup allowance from an
adaptive opening that demonstrably grows under a matched explicit owner budget.
Reuse the separately qualified permission-growth component where applicable;
do not combine it with accumulated private overlays without composed validation.
Require trace-off repeats, sustained clean throughput and physical measurements
before adopting a change. No production window/default changed in this study.

Raw evidence beneath `/private/tmp/urnetwork-h1-round03-replay.lXjhtE/`:
`fixed16k-control-loaded.5mOBxD/traffic.json`,
`fixed16k-loaded.XnaKIB/traffic.json`, `control-clean.s9xBnI/traffic.json`,
and `fixed16k-clean.r4dWFD/traffic.json`. Source plan:
`/private/tmp/urnetwork-h1-fixed16k-diagnostic.CCgroC/plan.json`.
These host results do not qualify the iOS 32 MiB or Android 64 MiB device limits.

### 2026-10-08: startup capacity correction exposes adjacent owner waits

Two deterministic tests now reproduce startup defects on unchanged production:
opening credit is still spent after queued service ends discovery, and sparse
drained control traffic establishes capacity from lifetime bytes rather than
the selected measurement epoch. In the latter fixture, 4,896 control bytes
produce a held estimate of 510 B/s and pace of 561 B/s, pricing the first bulk
message at **6.139037433 seconds**. These are actual semantic failures, not
build, timeout or resource failures.

The isolated private correction passes all 17 focused controls. Two unchanged
clean first-flight controls also pass: a 65,536-byte body completes in about
2.544 ms before its first ACK, with and without a preceding tiny control.
This is a deterministic finite-flight model, not measured website speed or
statistically qualified throughput.

The broader candidate run is **not qualified**: 399 top-level tests ran,
397 passed and two failed; all 24 nested subtests passed. The failures are
`TestWindowPacingH1CreditAdmissionCannotStarveResend` and
`TestWindowPacingH1CreditAdmissionCannotHideRouteChange`. An exact no-overlay
comparison on unchanged production passes both. Thus the candidate introduces
these assertion failures; the focused pass cannot replace the parent gate.

The nested pacing wait observes cancellation, service changes and ACK lifetime
but not the immutable route-generation notification. The route replacement
fixture consequently sends zero messages after H1 retirement versus ten on
production. A minimal route-wakeup correction and adjacent wait audit are open.
The retry failure needs a physical-write chronology before attributing scheduler
starvation: recovery is correctly anchored to first physical write plus its
existing interval, whereas the fixture observes offer plus three seconds.
Do not increase timers or force estimate refresh merely to make that test pass.

All processes joined and tested source pins held. Production Go source remains
unchanged. Raw evidence under `/private/tmp/urnetwork-h1-round03-replay.lXjhtE/`:
`startup-live-red2.nlYW8h/causal.json`, `startup-live-clean2.XSAvh6/clean.json`,
`startup-green17.vKeFV3/focused.json`,
`startup-ordinary399-1.iFychM/ordinary.json`, and
`startup-live-h1-admission2.Kdn7Hx/baseline.json`.
Fresh normal/race confirmations, end-to-end trace-off performance, and physical
32/32-MiB iOS-surrogate and separate 64/64-MiB Android qualification remain open.

### 2026-10-08: recovery ownership and trace-off loaded diagnostics

The route-eligibility correction passes 83 focused top-level controls, including
zero-allocation ready-send checks. That correctness result is not a performance
clearance. Original-workload trace-off, single-process diagnostics yielded:

| Source | Reported transfer duration | Goodput, MB/s | Loaded probes successful/offered |
| --- | ---: | ---: | ---: |
| Unchanged LIVE | 9.399 s | 0.006972 | 37/56 |
| Unchanged LIVE repeat | 9.437 s | 0.006944 | 32/56 |
| Private eligibility/startup candidate | 14.515 s | 0.004515 | 52/86 |
| Known-bad opening-credit counterfactual | 8.237 s | 0.007956 | 41/49 |

Each delivered the correct body. These are diagnostic arms, not a statistical
comparison or baseline update. The candidate is held out: its reported duration
increased and goodput decreased. The counterfactual restores unconditional
opening credit after queue evidence and deterministically fails the adaptive
reservation-price invariant; it must not be integrated despite its faster arm.

Reported download duration includes the reverse completion-byte exchange, not
just body delivery. Separate three-event captures both failed calibration and
remain unqualified. Their body-to-server-completion intervals were 0.817 s LIVE
and 1.124 s candidate; the additional 0.307 s did not explain their full reported
duration difference. Sampled body delivery also slowed. Do not attribute the
whole post-workload sampler tail to body transfer or use these captures as a win.

Deterministic timing tests confirm an independent recovery scheduling defect:
a younger charged pacing reservation due at 3 s hides an older accepted Pack's
physical-write-plus-2-s recovery boundary. Original 60 s ACK lifetime and debt
remain intact. Fixes must select recovery at its own due time, preserve the
younger FIFO reservation, and prove eventual actual retry after service is paid.
Selection alone or an extended timeout is not a sufficient regression test.

The ownership successor passes 56 focused tests in three fresh normal processes
and one race process. It remains private and needs composed validation and real
retained-allocation measurements. Budget charges are not measured heap usage.
No result here qualifies the 32 MiB iOS profile or 64 MiB Android device profile.

Receiver-baseline ACK ordering is the next deterministic hypothesis: production
observes receiver RTT before confirming the exact unloaded probe, so one ACK may
first retire startup credit as congestion and later raise the unloaded baseline.
The proposed correction stages only that exact tuple until successful H1/ACK
confirmation; genuine prior or sibling congestion must remain sticky. No proven
root cause or performance improvement is claimed before execution.

### 2026-10-09 checkpoint: held-service investigation and dual-profile coverage

The composed private root-package run recorded 7,536 PASS, 37 SKIP and zero FAIL
out of 7,573 top-level tests. This is not a canonical runner or device result.
Six fresh modeled arms (ABC/CBA) retained all 98 rows per arm. At 1 Mbps with
400 ms RTT and 50 ms ACK compression, delivery was 0.94208/0.96256/0.90112/
0.90112/0.96256/0.94208 Mbps. Candidate C is slower and remains private.

Bounded B/C observers reproduced 0.96256 versus 0.90112 Mbps. C accepts a current
106840 B/s credit pair, then retains that service while confirmed packet offers
occur every 22.727273 ms and compressed ACKs arrive every 50 ms. Captured growth
pairs fail coverage. This supports investigating proved offer continuity, not
weakening coherent decrease checks or substituting an aggregate maximum. The
omitted middle prevents attributing the first cross-arm divergence. Instrumented
runs are diagnostic only; no H1 performance improvement is qualified.

Canonical physical PERFVAR now requires the complete matrix independently for
both iOS32 and Android64: 280 observations per profile (560 total) at seven
blocks. Every retained runtime sample, including prequiet and teardown peaks,
must satisfy its selected 32/64 MiB absolute gate. Fresh physical qualification
is still pending. Android iOS-profile results cannot establish the signed iOS
extension's total `phys_footprint` below 50 MiB.

### 2026-10-10: loaded H1 repeat census and reference-calibration correctness

Ten fresh, uninstrumented host processes compared private D and E on the same
original Run1 H1 workload, in order D,E | E,D | D,E | E,D | D,E. Both include
the same 13-file production composition; E changes only pacing sample selection
to preserve independently covered service evidence ahead of an offer-only
lower bound. This was not a LIVE-versus-candidate or physical-device comparison.

All ten bodies were correct. Nine tests passed; process 05/D failed the
unchanged direct-underlay headroom gate and makes pair 3 performance-ineligible.
No attempt was retried or removed. All ten rows, probe denominators, latency
percentiles and retained evidence fingerprints are in
[the measurement ledger](../tests/PERFVAR-MEASUREMENTS.md#2026-10-10--h1-de-run1-20261010--complete-loaded-diagnostic-census).

The observations are mixed, with **no qualified performance win**. Across all
five scheduled attempts per side, including invalid 05, E's median reported
duration was 10.360 s versus D's 11.038 s, but its mean was 12.218 s versus
11.408 s. These are descriptive totals, not a qualified aggregate. E was faster
in both E-first pairs; E-second pairs had mixed signs. Probe offers continue
until the complete download helper returns, including the reverse completion
byte, so longer duration changes the cohort. More completed probes does not
establish lower latency; successful-probe p50/p95 and failures must stay visible.
No cause or repair of the historical 0/64 observation is established.

Separately, eleven deterministic reference-calibration processes completed:
two expected RED arms each had two semantic failures; nine GREEN arms totaled
42 passing tests, with no skips. Three fresh default and three fresh tagged
confirmations passed. The constant-flight reference uses the existing 256-KiB
floor; delivery-arm settings, production budgets, timers, workload and the 90%
capacity/fairness/queue gates are unchanged. This corrects reference-fixture
validity, not the loaded H1 result, and does not qualify a product speedup.
The default guards still require integrated, default-source parent validation;
tagged diagnostics cannot stand in for that coverage.

Fresh composed canonical correctness and full physical qualification remain
pending independently. The iOS profile must satisfy its 32-MiB observed-runtime
gate in every retained phase, and normal Android its separate 64-MiB gate,
including prequiet and teardown peaks. Host samples and regression proofs
qualify neither profile nor a signed iOS extension's below-50-MiB total
`phys_footprint`. No baseline is promoted.

### 2026-10-10: bounded H1 phase attribution

Four fresh instrumented Run1 processes completed in D/E then E/D order.
Seven recorder controls passed in each of four separate normal/race D/E
processes. All traffic traces were complete and non-overflowed; source,
native, resource and six-receipt paired runtime checks held. The original
workload, 6.416-second probe deadline and calibration gates were unchanged.

| Arm | Body phase (s) | Completion return (s) | Bulk duration (s) | Loaded successes/offers | Qualification |
| --- | ---: | ---: | ---: | ---: | --- |
| 09 D | 8.665750 | 4.416964 | 13.082919 | 30/77 | Test passed; diagnostic only |
| 10 E | 8.425805 | 1.605492 | 10.031358 | 41/59 | Test passed; diagnostic only |
| 11 E | 3.938436 | 0.935818 | 4.874323 | 29/29 | Headroom gate failed |
| 12 D | 11.020256 | 14.378473 | 25.399066 | 43/149 | Test passed; diagnostic only |

Body phase is loaded-start to verified body, not exact network-byte service
time. Completion return is successful local one-byte write to origin read,
not a Transfer ACK. The separate probe-settlement tails were 3.516163,
2.050852, 1.346478 and 4.563837 seconds; they are outside bulk duration.
Recorder overhead is unknown: do not subtract it or pool these observations
with the ten earlier uninstrumented runs. Arm 11 remains invalid performance
evidence despite its correct body and successful probes.

All 314 loaded offers reconcile to 143 successes, 170 expirations and one
late response. Every offer eventually returned. Maximum reader-to-owner
handoff was 0.221 ms, so it does not explain the multi-second misses. Arm 10
had return-side delays; arm 12 also had outbound delays (failed-probe
offer-to-echo median 9.398 s). Aggregate drop/recovery counters cannot identify
the delayed packet or its active flow.

Source audit found a production-reachable candidate: native packet readers
wait for synchronous batch sends, whose flow groups are admitted serially.
An earlier TCP group waiting for reliable retention can delay later UDP
groups. Production's default send timeout is five seconds, unlike this
fixture's indefinite wait. Grouped NoAck traffic also excludes the scalar
direct-write bypass. A forced exact-peer-ACK barrier test is the next step;
these traces do not prove that admission was their stalled boundary.

Any isolation or grouped fast-path candidate must retain bounded message/byte
ownership, per-flow order, whole-group/partial-success accounting and reliable
TCP delivery. Enlarging queues or changing timeouts is not a demonstrated fix.
The reviewed correctness composition is now integrated, but default-source
parents and fresh physical iOS32/Android64 qualification remain pending.

### 2026-10-13 UTC: first-cell collector publication failure; qualification pending

The fresh canonical physical lane reached its first iOS32/control Wi-Fi cell
but readiness exited 2 with `collector-session-owner-required` before workload
collection. Its retained launcher published the required owner receipt
429.134 ms after the failed check, then later joined with exit 0. The reader
had no publication handshake across the launcher's asynchronous role/identity
checks. This is a harness startup-ordering failure, not a demonstrated memory
budget failure.

The retained partial summary peaked at 18,792,464 B (17.92 MiB), with zero
sampled 32-MiB breaches, but it is **INELIGIBLE**:
`eligible=false` / `INCOMPLETE_TEARDOWN`. Its 29 retained runtime reads are
not independent workload observations or a completed steady/teardown proof.
Physical, LOWBAR and MEMSTEADY checks failed; owned cleanup passed. Cleanup
success must not be used to waive missing live-gate/prefix/teardown evidence.

The private repair keeps the actual retained launcher identity while awaiting
publication, including the adjacent instrumentation-owner ordering, within the
original collector 15-second and instrumentation remaining 180-second budgets.
Observed owner/terminal/identity and invalid completed-prefix failures still
refuse; no memory or teardown gate changes. Repair/fresh-parent qualification
and integration remain provisional here, not a physical acceptance claim.

This attempt contributes **zero qualified cells out of 168 and zero required
observations out of 560**. Fresh full qualification still requires seven blocks,
280 observations independently for iOS32 and normal Android64, both arms and
phones, Wi-Fi/cellular/P2P roles, workload/quiet/lifecycle/teardown coverage,
and every retained runtime peak at or below its matching 32/64-MiB limit.
The DeviceLocal target and Go soft limit remain 32/32 MiB and 64/64 MiB;
there is no budget tuning or measured Android64 performance gain in this record.
Android surrogate measurements cannot prove a signed iOS extension's total
`phys_footprint` below 50 MiB. No baseline is promoted.

Source epoch, exact chronology and retained incident fingerprints are recorded
in [the measurement ledger](../tests/PERFVAR-MEASUREMENTS.md#2026-10-13-utc--pcp-7kv2yp-01--collector-publication-failure).

### 2026-10-13 UTC: collector repair private QA accepted; device replay pending

Root and the sole executor accepted all 27 private repair-qualification arms
with natural joins and intact source/native/resource/cleanup custody.
The three original causal controls retained exactly 2/2/3 expected semantic
failures. Candidate Node focused 18, whole-file 59 and complete 26-file
preflight parent 536 each passed initially and in three fresh confirmations.
Candidate Go focused 4 passed initial normal/race and three fresh normal/race
pairs. Unfiltered backend normal/race each passed 328 top-level plus 601 nested
tests; the six-file outer runner normal/race each passed 95 top-level plus
180 nested tests. Candidate runs had no test failures, skips or race reports;
earlier failed attempts are preserved, not relabeled.

These receipts qualify the exact private XrQpLJ/MbXTNN source combination,
not a new default-source/full-runner or real-device run. Fresh default-source
validation and full iOS32/Android64 physical qualification remain pending.
The earlier attempt remains INELIGIBLE at 0/168 cells and 0/560 required
observations; its 17.92-MiB partial peak is not a memory PASS.
The seven-block dual-profile census and every 32/64-MiB all-phase gate are
unchanged. There is no Android64 performance/utilization result, baseline
promotion or signed-iOS total-footprint claim.

See [the accepted private-QA ledger addendum](../tests/PERFVAR-MEASUREMENTS.md#collector-publication-repair--accepted-private-qa-not-device-qualification)
for exact scopes and retained source/evidence references.

## Public-playback failure and lost H1 bracket — private, unqualified proposal

The frozen 2026-10-13 UTC attempt
`physical-first-runner-setup-collector-checkpoint.TnULxC/campaign.current/round-perf-collector-01`
failed its first Pixel/iOS32/control/Wi-Fi public H1 bracket at 03:23:35.
The physical report retained only the preceding Direct observation; it
reported failed workload children and an incomplete matrix. Both public
CNN/Bloomberg children completed with exit 2, committed navigation and observed
nonempty samples, but no playback progress. Their raw failures remain failures
in the retained child denominator. The three H1 fast.com readings were 76,
36 and 1.5 Mbps: the 36-Mbps median also fails the unchanged 40-Mbps floor.

The session retained 71 primitive memory samples, peaking at 26,320,912 bytes
(25.10158 MiB). The auxiliary/combined retained peak was 27,009,040 bytes
(25.75783 MiB). The passing H1 live quiet gate reported a 26,009,616-byte peak
over 22 samples spanning 315 seconds. These sampled values were below 32 MiB;
they are not a qualified memory result. The workload failure occurred before
the H1 observation/bracket was registered, so the finalizer omitted H1's
bracket-specific teardown replay. The retained eligible Direct replay is not
an H1 replay or a full-session/full-matrix substitute. Raw memory and teardown
artifacts remain available; the frozen failed report is not rewritten.

The proposed source packet `review-physical-public-playback.djHZWR` is PRIVATE
and UNQUALIFIED: no candidate formatting, tests, device run or performance
improvement is claimed. It proposes only the previously user-authorized,
fixed public-playback policy: completed owned exit-2 nonprogress is diagnostic
when navigation is committed and samples are observed/nonempty, without
signals or interruption. Both public probes still run; raw child exits,
results and failed-child counts remain, with a separate diagnostic count.
Launch/deadline/identity/invalid-result/cleanup failures remain fatal.
P2P video playback remains mandatory. The H1 speed floor remains mandatory,
so this policy alone would not make the frozen 36-Mbps bracket pass.

The separate retention repair proposes registering failed public brackets
and invalid observations before final teardown checks, preserving late peaks
and replay failures under both exact budgets. A peer failure before quiet
retains both roles' raw evidence without fabricating an uncollected quiet
window. Physical coverage/row version 3 would bind the exact policy
`public-observed-nonprogress-diagnostic-v1`; historical strict version-2
results stay immutable and cannot be relabeled.

Fresh full physical qualification is pending: zero qualified cells out of
168 and zero qualified required observations out of 560. Both profiles retain
seven blocks/280 observations each, separate control/candidate builds,
both allowlisted phones, Wi-Fi/cellular and P2P roles, all memory phases and
absolute 32/64-MiB runtime/teardown gates. There is no Android64 measured
performance/utilization improvement, baseline promotion, or signed-iOS
total-`phys_footprint` claim from this Android surrogate.

See [the matching measurement-ledger incident note](../tests/PERFVAR-MEASUREMENTS.md#public-playback-failure-and-lost-h1-bracket--private-unqualified-proposal)
for the retained failed-run and private source-proposal scope.

### Harness repair qualification update (2026-10-13)

The public-playback policy and failed-bracket retention repair is now integrated.
Its deterministic pre-fix controls reproduced both defects. Private candidate
validation passed the 20 focused tests in eight fresh normal/race processes,
the full backend (337 top-level and 601 nested tests) in normal/race mode, and
the outer runner (95 top-level and 180 nested tests) in normal/race mode.
Live code matches those tested postimages. This qualifies the harness repair,
not physical performance or memory: the fresh full 168-cell/560-observation
campaign for the 32 MiB iOS surrogate and 64 MiB Android profile is still pending.
The prior 36 Mbps H1 median remains below the unchanged 40 Mbps floor; neither
this repair nor Android measurements establish native iOS footprint below 50 MiB.

### 2026-10-13: first 32 MiB physical H1 session — memory pass, speed miss

The fresh full runner `round-full-go1272-tools-01` measured the allowlisted Pixel
(device-a) on Wi-Fi, control arm, `ios-memory-audit-v2`. Both the observed
Go soft limit and DeviceLocal target were 33,554,432 bytes. These are Android
surrogate observations, not native iOS total-footprint measurements.

| Measurement | Direct baseline | H1 |
| --- | ---: | ---: |
| Fast.com display, three runs (Mbps) | 890 / 930 / 1100 | 18 / 38 / 38 |
| Display median (Mbps) | 930 | 38 |
| Wikipedia median load, five runs (ms) | 251.4 | 611.4 |
| Wikipedia median TTFB (ms) | 73.8 | 263.6 |
| Quiet Go peak (MiB) | 16.66 | 25.55 |

The H1 live memory gate passed after a 331,161-ms quiet boundary, with 68
device primitives and zero recorded 32 MiB breaches. Retained session-wide
Go peak was 29,335,568 bytes (27.98 MiB); final teardown passed, with four
teardown primitives and a teardown peak of 24,887,312 bytes (23.73 MiB).
The final ledger conservatively assigns the session-wide producer peak to
both observations, including late retained events. These are sampled runtime
observations, not proof of an unsampled instantaneous maximum.

Fast.com's display is the existing authoritative speed metric. The separate
page-target byte counter reports only completed responses, omitting canceled,
unfinished and worker-target traffic; its H1 values 0.05 / 8.74 / 0.05 Mbps
must not be interpreted as aggregate exit throughput. Native H1 counters
increased by 216,399,702 ingress bytes across the whole workload, corroborating
substantial traffic but not attributing bytes to individual Fast repetitions.

The cell failed the unchanged 40 Mbps H1 floor. It is not a qualified baseline
or a statistically established code regression. Canonical first-failure stop
retained only two of 560 required observations; the other 167 cells, Galaxy,
cellular/P2P, candidate arm and Android 64 MiB profile remain unmeasured here.
No memory/performance improvement or baseline promotion is claimed. A narrow
safe continuation policy for validated speed misses is under review, not
implemented; memory, ownership and malformed-workload failures remain fatal.
The full runner was stopped at a correctness-command boundary, with raced
commands recorded CANCELED and canonical cleanup PASS. See the matching
[measurement ledger](../tests/PERFVAR-MEASUREMENTS.md#2026-10-13--round-full-go1272-tools-01--first-physical-h1-memory-result).

### 2026-10-14: continuation quiet-memory results — shared teardown failed

Fresh PERF-only continuation on allowlisted Pixel (device-a), Wi-Fi, control,
retained exact 32 MiB DeviceLocal target, Go soft limit and observed-runtime
gate. The full device plan still requires both phones, Wi-Fi/cellular/P2P,
control/candidate and separate iOS32/Android64 profiles. Numeric H1 speed misses
now permit safe evidence collection; memory/ownership/cleanup failures remain fatal.

| Live quiet-gate measurement | DIRECT | H1 | Auto |
| --- | ---: | ---: | ---: |
| Observed Go peak (MiB) | 17.63 | 24.86 | 27.62 |
| Quiet Go peak (MiB) | 16.94 | 24.86 | 27.26 |
| Quiet samples / recorded 32 MiB breaches | 22 / 0 | 22 / 0 | 22 / 0 |
| Fast.com median display (Mbps) | 910 | 12 | 0.6 |
| Wikipedia median load / TTFB (ms) | 230.4 / 55.1 | 1057.3 / 402.4 | 1066.3 / 452.5 |

All three live gates passed their five-minute intervals. These are partial
sampled-runtime observations, not final session peaks or qualified profiles:
H3 timed out waiting for provider-window admission before any egress probe,
finish/shared teardown failed, and all four retained observations are invalid.
Android64 was not reached. No native iOS under-50-MiB footprint claim follows.

Auto initially selected H1 and later H1+H3, predominantly H1 by ingress bytes.
One Fast3 interval had a seven-second overall-TUN RX plateau while Wi-Fi counters
advanced; continuous per-flow offered load is unproved. Its enclosing interval
recorded only 630.218 ms total Go STW pause, not a seven-second GC freeze.
Missing provider and sender-side ACK/window/pacing diagnostics prevent causal
attribution. Do not tune timers or spend additional budget based on these samples
alone; paired performance/utilization qualification remains pending.

Canonical cleanup passed and all owners joined; correctness separately found a
test-fixture pool-ownership error. Bounded readiness diagnostics and deterministic
fixture regression tests are under review. The failed evidence remains preserved,
with no baseline promotion. See the [matching measurement ledger](../tests/PERFVAR-MEASUREMENTS.md#2026-10-14--device-continuation-round-02--partial-quiet-passes-failed-cell).

### 2026-10-15: fresh synced run — partial iOS32 observation, Android64 pending

Pixel/Wi-Fi/control H1 retained 22 samples across five quiet minutes, with
26.22 MiB maximum observed Go-runtime memory and no sampled 32 MiB breach.
H3 then timed out on public-window readiness and terminated instrumentation;
the cell is invalid despite its partial quiet pass. Android64 was not reached.
Neither profile is qualified, and Android does not prove the native iOS
extension's under-50-MiB footprint.

The tunnel Fast.com display helper accepted stable but still-running values;
its deterministic repair now requires the terminal indicator without changing
the 90-second deadline or 40 Mbps gate. Preserve old failed measurements.
Independent TUN RX intervals were also below 40 Mbps, so the display bug alone
does not explain the slowdown. Sampled packet/Transfer budgets were not full
and no forced GC was recorded; increasing memory is not justified by this run.
Next: trace native read/batch-send admission, then rerun the complete iOS32 /
Android64 device matrix. See the [measurement checkpoint](../tests/PERFVAR-MEASUREMENTS.md#2026-10-15--synced-runner-checkpoint-incomplete-physical-qualification).

### 2026-10-16: deterministic H1 retained-root admission mismatch

`TestPlatformH1GraphRetainedReceiveFitsAdmission` exercised real in-memory
H1 and H1+ constructors at explicit 32 and 64 MiB targets. A held receiver
retained 33 distinct message roots: 540,672 bytes for H1 and 541,068 for H1+,
against active claims of 262,144 and 524,288 bytes respectively. All four
cases completed cancellation and pool/claim reconciliation before reporting
`H1_GRAPH_RETAINED_BYTES_EXCEED_CLAIM`. The source-bound diagnostic is retained
as `merged-h1-graph-diagnostic.1Ki7Tb/go.json`; these are independent receive
allocations, not duplicate charges for sender/Transfer shares.

The correction keeps the 32-message channels and 16-KiB message maximum.
Separate send, receive, control, and enabled acknowledgement byte lanes are
prepaid in the graph claim. Writer ownership lasts until its share is returned;
the private Client receive receipt lasts through decode and final Pack handoff.
An unadmitted Pack follows the exact physical lane's cancellation, while work
already accepted by Pack remains owned by the logical receive sequence.
`H1BudgetByteCount` remains the base connection/scratch row, including when
used for an outer raw tcp/tls carrier; application H1 claims use
`H1MemoryReservationByteCount()`.

The isolated Connect witness has no acknowledgement companion: its new claims
are 656,512 / 918,656 bytes. Actual mobile SDK construction adds its 10 / 21
acknowledgement slots, producing 697,592 / 1,004,924-byte claims. The normal
6 / 12 direct-H1 public graphs, three provider control graphs and one replacement
fit the unchanged 8 / 16 MiB carrier roots. Outer extender claims remain additive.
Byte pressure does not reduce channel
counts, borrow sibling control allowance, or silently discard a reliable frame.
Zero/one-count custom lanes still admit one maximum-sized frame.

Reproduce the independent root oracle with the canonical Go confirmation
runner's normal and race selections for
`^TestPlatformH1GraphRetainedReceiveFitsAdmission$`; adjacent controls are
`^TestPlatformH1Retained` and `^TestH1ReceiveCarrier`, plus the SDK constructor
test `^TestDeviceLocalH1RetainedGraphProfileComposition$`. This source change
and its new controls await executor qualification. Selector microbenchmarks
measure host overhead separately; no device performance gain is claimed.
The witness is not a measured process-cap breach, a continuous memory peak,
or an attribution of the observed phone stall. The 32/64 MiB memory gates,
full physical matrix and native iOS footprint requirements remain unchanged.

The first corrected focused diagnostic passed all 13 H1/receive-lifecycle
tests and all 13 carrier-budget overflow tests. An immutable overlay restoring
only the old budget implementation failed the exact semantic assertion
`class h1 bypassed limit 8 with a wrapped live sum`; it compiled and cleaned up
without a panic. Evidence is retained in
`h1-retained-focused-final.FogUDP/{h1-lifecycle.gSlRv0,overflow.0m1tvC,overflow-old-control.z2o3z9}`.
These are single fresh-process diagnostics, not canonical three-process
normal/race confirmations or device qualification. Two earlier attempts are
preserved separately: an unpooled-16-KiB test assumption and a commented-out
helper reference in its repair. Neither failed attempt is counted as a pass.

A 100-ms selector allocation screen used 1,200-byte packets, both profiles,
and one/two/three routes (48 result rows). Ready reads and writes added no
allocations relative to the unaccounted control; the synthetic eight-slot
priority case remained zero-allocation. Full-route, already-cancelled writes
showed extra allocation from retained-byte admission:

| Routes | Control B/op, allocs/op | Accounted B/op, allocs/op |
| --- | --- | --- |
| 1 | 16, 1 | 128, 2 |
| 2 | 144, 5 | 368, 7 |
| 3 | 1,008, 13 | 1,976, 21 |

Both targets produced the same allocation counts. This cancellation case is
not a throughput test. The single short host screen is not a statistical
speed comparison or a baseline update. Its measured allocation increase
motivates examining eager credit-notification creation and the three-route
reflective selector before retaining a performance optimization. Raw results:
`h1-retained-focused-final.FogUDP/selector-bench.mpasgW/go.json`.

Follow-up candidate A reserves bytes or subscribes under one lock; it creates a
notification only after byte refusal and preserves an existing waiter's channel
on success. The same 48-case screen reduced accounted cancelled-write allocation
to 16 B / 1 allocation (one route), 144 B / 5 allocations (two), and 1,640 B /
18 allocations (three), identically at 32/64 MiB. Other 42 rows had unchanged
allocation counts. Retain A for this measured allocation reduction; the separate
three-route reflective-selector cost is not fixed here. Single short host timings
still do not establish an end-to-end speed improvement.

The widened focused normal selection passed 61 top-level tests (120 including
existing subtests). Restoring only the old eager-subscription block through an
immutable overlay produced exactly six
`H1_FUNDED_SELECTOR_ALLOCATED_BYTE_NOTIFY` failures after all three route-count
cases joined; the other-waiter preservation test passed. Evidence:
`h1-candidate-a-diagnostic.fZbQNm/{h1-normal.fElknp,selector-bench.UIkXCv,old-prepare-execution.hhWi7B}`.
The pre-Go Bash-3 `mapfile` setup error is preserved separately and uncounted.
Final resource sampling passed after the last command; the observer itself was
interrupted during post-sample sleep (exit 1), not a clean supervisor exit.
Canonical normal/race confirmation will use a graceful stop-request owner.
Full parent and physical 32/64 qualification remain pending.

### 2026-10-16: focused accounting and lifetime confirmation

All eight focused confirmation groups now passed three fresh processes each:

| Scope | Normal | Race |
| --- | --- | --- |
| Connect H1 retained roots, carrier lifetime and transport budgets | 3/3 | 3/3 |
| Actual SDK H1 graph composition | 3/3 | 3/3 |
| SDK memory and adjacent H3-window policy | 3/3 | 3/3 |
| Extender mobile-memory composition and H1 lifetimes | 3/3 | 3/3 |

The extender compile-only check also passed. The preceding race failure was a
test observation without the lane lock, not a reported production-to-production
race. The repaired fixture copies live counters under that lock, verifies its
virtual-time admission/deadline boundaries, and joins rollback before returning
the pooled root. Production helper `ae927399` is unchanged; repaired test
`0e59e5b4` passed the fresh normal/race confirmations. A subsequent command-path
setup failure occurred before any test and remains retained separately; its
corrected invocation used a fresh output directory.

Evidence is under
`postmerge-full-current-01/h1-canonical-repaired-confirm.jHRYxX` in the retained
chronology. All eight runner summaries report `confirmed=true` and
`consecutive_passes=3`. The 11-repository before/after source inventories matched
(`eb77f14260b1e9fd23bfef8076e23bf9fb35c88aa2e2dfc9ae35f60ad3ad8e59`).
The resource observer accepted 26 samples and joined with exit 0; its final
sample ended at 14:16:01 UTC, after the final Go command at 14:15:24 UTC.

These are focused correctness/lifetime results, not an end-to-end performance
comparison or physical memory qualification. The full affected parents and
ALL/MAIN/PERF campaign remain pending. In particular, the 32 MiB iOS profile on
Android and the normal 64 MiB Android profile still require the complete device
workload; neither native iOS footprint nor a new throughput baseline follows
from these passes.

### 2026-10-17: partial Pixel iOS32 physical collection — not qualified

The fresh canonical PERF attempt `attempt-02-dm6pile763dc` collected only the
Pixel `3B161FDJG001KT` control/Wi-Fi DIRECT and H1 observations before failing
closed. Source heads were Connect `620d2821660f8e7795f6c5d61ea74ac219a3ceed`,
Server `ec3dbdec6205fb3f946514837854f18b101794fb` and SDK
`c72729efe1bb6dff5e1aa557a035fda3a116b304`. The profile was
`ios-memory-audit-v2`: 32-MiB DeviceLocal target and 32-MiB Go soft limit on
Android. This is not a native iOS footprint measurement.

| Observation | Measured result | Qualification |
| --- | --- | --- |
| DIRECT fast.com | Three completed displays: 510, 550, 460 Mbit/s | Valid control observation; not a tunnel result |
| H1 fast.com | Timeout, valid 40-Mbit/s display, timeout; the two 90-second timeouts displayed invalid 30 and 7.2 Mbit/s | Invalid H1 observation; do not average failed displays into a speed |
| H1 active Go-runtime peak | 28,676,128 B (27.35 MiB) | Below 32 MiB in this limited sampled interval only |
| H1 quiet maximum / p50 / p95 | 26,775,584 / 25,501,728 / 26,161,184 B (25.54 / 24.32 / 24.95 MiB) | Recovery observations, not a complete profile pass |
| H1 final quiet sample | 24,875,040 B (23.72 MiB), zero outstanding packet-pool owners | No retained pool-owner leak shown at this boundary |
| Overall reported runtime peak, including auxiliary observations | 30,003,232 B (28.61 MiB) | Do not substitute this for the active-only peak or native footprint |

Five H1 Wikipedia navigations completed. Cold document TTFB/load were
2,934.7/3,243.4 ms; medians were 209.9/560.4 ms. DIRECT medians were
47.5/222.2 ms. These are one control bracket, not a paired candidate comparison.
The single valid H1 Fast result took 64.205 seconds and reported
131,220,598 completed page-target encoded bytes, yielding 16.35 Mbit/s over
that observed window. Its displayed 40 Mbit/s is a different estimator.
Completed page-target bytes exclude worker-owned and partly canceled transfers;
the small byte count in the third failed probe is not total tunnel traffic.

The H1 collector retained 713 eligible samples and a 331.222-second quiet
window. Tunnel counters advanced to roughly 396.56 MB ingress, proving traffic
progress but not locating either Fast stall. Quiet-window counter edges occur
after Chrome cleanup and cannot identify the failed active interval. The next
bounded diagnostic must retain browser completion state and packet/trace
measurements bracketed around the actual Fast attempt.

The physical phase explicitly reports `collection_only=true`, only 2/560
observations, `physical=false`, `lowbar=false`, `memsteady=false`, and cleanup
true. Its required Fast child and coverage gates failed. No Galaxy, cellular,
P2P, candidate arm or Android64 result was qualified; no baseline is updated.
The independent correctness phase also failed before product assertions on a
cached Docker image-reference spelling mismatch. Its verified repair and a new
full physical replay are separate requirements, not reasons to relabel this
attempt successful. Always-cleanup and runner/observer joins completed.

Evidence: retained chronology
`go1272-qualification.NmFy9i/continuous-offer-h1/canonical-integrated-parents-v1/postmerge-full-current-01/physical-full-current.Xq5xVg/attempt-02/`,
especially `perf/phase-physical.json` and its linked raw workload/telemetry
artifacts. Fresh full Android-surrogate iOS32 and normal Android64 MEMSTEADY /
PERFVAR qualification remains required; Android cannot prove the signed iOS
extension's kernel lifetime footprint stays below 50 MiB.

### 2026-10-17: pacing/receive diagnostics do not qualify mobile memory

The private OLD/numeric H1 Record25/run1 pair measured the aggregate simulator
host process, not either mobile profile. Its 42 samples per arm reported
heap-plus-stack p95/max of 70,508,544/70,574,080 B for OLD and
69,664,768/69,877,760 B for numeric. Both exceeded the historical diagnostic
24-MiB ceiling in every sample. These are neither Android process/profile
measurements nor native iOS extension `phys_footprint`; they cannot qualify or
disqualify the explicit 32/64-MiB mobile policies. The small difference between
two active-host samples is not attribution to the numeric metadata.

Ten counterbalanced fresh microbenchmark processes produced 140 rows, all
reporting 0 B/op and 0 allocs/op. That is a per-operation microkernel result,
not a zero retained-memory-cost claim. Numeric tail metadata increases a
retained map value by eight bytes on the measured 64-bit layout, from 32 to
40 bytes; map backing, size classes and owner high-water still matter.
Descriptive median original-tail and retry/fresh-tail costs increased by
2.05 ns and 7.9 ns respectively. No statistical speed claim or blanket absence
of performance regression follows.

At this earlier checkpoint the P3 reference had a separate, source-known
healthy 32-item burst cost: preparation can change one flush/two slice-clone
calls into 32 flushes/64 calls, with possible extra queue pumping. The
nil-observer actual-owner allocation/wall-time benchmark was still an open
adoption gate; the later P3 entry below records its completed comparison. Its fixture correction must retain equivalent ACK, callback, final-owner
and cancellation boundaries in both arms; virtual-time elapsed or overlapping
goroutine allocation samples would not establish that cost.

The P3 cleanup failures also do not establish retained heap leakage:
10,239/12,288-byte ordinary buffers are outside the pool's largest 8,192-byte
class. V4 checks actual unpooled frame/reference removal after full ownership
joins while preserving the pooled last-owner oracle. Its normal 30-case pass
does not erase the three race-process fixture cancellation failures. Later V5
reproduced the exact V4 cancellation-order defect with the new control, then
passed that root and all 31 reference cases in both normal and race builds.
These are one normal/race execution of each corrected selection, not a measured
allocation/throughput comparison or a full-package qualification. No forced-GC
or claim of collected memory substitutes for reference/owner closure.

Evidence, relative to `/Volumes/data/urnetwork-h1-chronology.Y0OYTR/`:
raw prefix
`go1272-qualification.NmFy9i/continuous-offer-h1/canonical-integrated-parents-v1/postmerge-full-current-01/`,
children `pacing-numeric-bench-run.byqQ3o`,
`h1-go1272-replay-run.zqnfyd`, `p3-v3-run.FXRozK`,
`p3-v4-run.VID8EP` and later `p3-v5-run.6XGiOd`. The H1 interpretation is sealed in
`packetflow-regression-review/astra-review.AmbGZsAp/h1-pair-rca.a6XeeCK7/RCA.md`,
SHA-256 `207f7060f8e2ac0ac94a88d83fb0101c3b60d1f2c2581e8c4bca98e2b7aa9568`;
its `SHA256SUMS` is
`f4546a55cb98c122d0aec227bcf5d7cc675fd0bffbca9516c45cb7f196d46f69`.
The adjacent packet-flow follow-on entry records exact counterfactual and
fixture seals.

The preceding partial physical attempt remains failed, collection-only
evidence, not a 32-MiB pass. Fresh full unchanged-source A/A qualification is
still required for both configured phones and both 32-MiB iOS-surrogate and
64-MiB Android profiles. The recorded installed-version audit requires the
supported Pixel maintenance path: four preparatory arm/profile builds, the
explicitly approved helper reinstall, joined supported control release, then
four fresh canonical builds. This is eight builds, not reuse of build-only
attestation as canonical install authority. Retained APKs/receipts do not
replace a fresh source freeze; retained artifacts/caches plus the next build
working set remain charged to capacity. Neither mobile profile, even if later
qualified on Android, proves the signed native iOS extension's total lifetime
footprint remains below 50 MiB. At that checkpoint numeric H1 and P3 production
remained private; no memory baseline or budget was changed.

### 2026-10-18: P3 local cost gate cleared, mobile qualification pending

Later build checkpoint: the canonical full 32-repository build completed all
four profile/arm APK/AAR pairs on the source identity below. The build receipt
reports `build_attested=true`, no failures and joined process owners; all input
guards matched. All four native writers used four workers and profile rate zero.
Conservative sampled build storage, including outside-cache growth and Git
administration, reached approximately 24.35 GiB within the 64-GiB build allowance;
this is host build storage, not app memory or a proven instantaneous peak.
Receipt SHA-256: `57ceaa5e8d10a7886eeef6aefa9247db29e262ac1843205b90d0427ab6e8a017`;
resource summary: `2c3673debf914379148d52d0c408a8977a46f2dd8c05791b84edc5b3aae9eecc`.
These builds are explicitly not install- or device-qualified. Fresh 32-MiB
iOS-surrogate and 64-MiB Android traffic, quiet and teardown measurements remain
pending. The prepared full runner was held before execution for the requested
Git checkpoint; any subsequent source sync requires refreshed source-bound evidence.

The current-source OLD receipt-pressure root failed semantically and the
reference passed. On the frozen pre-integration source, all 31 focused cases
passed in three fresh normal and three fresh race processes, and all 197
existing affected delivery/ACK parents passed normally and under race.
Healthy and queued-cancel benchmark-owner controls passed in both modes for
each arm. The exact three production and five fixture/benchmark postimages are
now integrated. The first integrated full-root attempt exposed a portable
fixture error: a new unconstrained test called a helper defined only under
`!js`, so actual js/wasm test compilation failed. That run later stopped for
a separate capacity breach; both failures remain retained. Moving the same
helper body into common `ip_packet_pool_fixture_test.go` corrected the boundary
without production changes or matrix/skip changes. The existing six-platform
test, including js/wasm root-test compilation, passed in three fresh processes.

The post-helper-move 32-repository identity is
`76fcf6ad45627ab26ed65d9a3019b40282113e5134f3377492a0c08321184493`.
Only Connect content changed; prior private-overlay results retain their
original `66a10a38...` identity and are not integrated full-parent results.
A fresh full-root normal process then passed in 1459.088 seconds: 5890
top-level runs, 5855 passes, 35 top-level skips plus two DNS subtest skips,
zero failures. Fresh root race then passed in 6237.322 seconds: 5890
top-level runs, 5854 passes, 36 top-level skips plus two DNS subtest skips,
zero failures and zero reassembled data-race warnings. Both owners joined
with child/after-guard exits zero, unchanged guarded inputs and empty stderr.
The extra race skip, `TestIpMuxSafeReceiveNoAlloc`, reports unstable allocation
counts under race and passed normally. All 37/38 normal/race skip reasons
remain in the ledgers; opt-in, host/mobile-conditional and pending
implementation cases are coverage gaps, not verified invariants.

Four joined A-B-B-A processes measured 16 fresh 32-Pack lifecycles each through
real receive/dispatch, final TCP admission, cumulative wire ACK and full owner
cleanup, with the progress observer disabled. Reference-versus-OLD means were
lifecycle time +3.03%, allocated bytes -21.10%, allocations +2.04% (+89 per
burst), delivery time -0.44% and joined time -0.22%. The observed 12–15%
within-arm timing spread exceeds the mean time difference. This clears the
local cost-review gate for the correctness fix, not statistical equivalence
or an end-to-end speed claim. B/op is allocated bytes across the full fresh
lifecycle, not retained heap, runtime peak or native process footprint.

The known extra flush/clone opportunities remain a source-level explanation
of possible cost, not measured call counts or a 32-fold slowdown. Two processes
per arm do not establish tail latency; retained observer scans could overlap.
Earlier fixture/control-capacity failures and the separate post-A1 metadata
failure remain preserved, with no benchmark measurement retry. At this
checkpoint the integrated default root normal/race gate is passed, not the
canonical recursive/full-runner gate. Complete 32-MiB iOS-surrogate / 64-MiB
Android MEMSTEADY/PERFVAR qualification also remains pending. No baseline,
budget or threshold is changed. Historical H1 0/64 and partial physical
attempts remain unqualified; native iOS lifetime footprint remains separate.

The exact four rows, source transitions, portable compilation and full-root
normal/race receipts (including all skip reasons) are in
`PACKETFLOW-REGRESSION.md`'s P3 integration entry. Under the retained raw prefix
above, `p3-current-custody-admission.5JVBwT/timing/abba-result.json` has SHA-256
`a0a6b1eff64b4d016530dddc2b32f2733b8f4495c3503cdcf5e97d0a6c6d1f74`.

### 2026-10-17: pressure refusal must preserve healthy owners

A deterministic real-origin test confirmed that a full resident forwarding
shard returned and counted its refused complete message but then canceled the whole
resident. Both an original-data refusal and genuine reverse-ACK refusal
reproduced this defect at the default 256-slot shard capacity. The integrated
one-hunk correction keeps the existing queue limits, nonblocking offer and
extra-share return; it no longer uses unrelated-flow teardown as pressure
handling. Seven retry/loss/close/ownership roots passed three fresh normal and
three race processes. Full on-disk parents remain pending; receipts and limits
are recorded in Tests `PERFVAR-MEASUREMENTS.md` and `PACKETFLOW-REGRESSION.md`.

Pool/ledger reconciliation is not a runtime-memory qualification. Keeping
healthy flows alive can retain useful state that the broken teardown discarded;
no extra queue or payload allowance was added, but steady and burst memory
still need measurement under the complete 32-MiB iOS-surrogate and 64-MiB
Android workloads. A previous partial H1 quiet gate peaked at 28.57 MiB and
passed its local gate, but its cell subsequently failed and cannot qualify
either full profile. No memory/performance baseline is promoted by this fix.

### 2026-10-17: ownership-fixture validation before physical replay

Partial native writes may complete zero whole frames while still making valid
positive progress. The corrected oracle/adjacent 27-root group passed three
fresh normal and three race processes. A separate teardown watcher incorrectly
classified a child transport's expected close before root cancellation as a
live failure; its first-close snapshot correction and 13-root group also
passed three fresh normal and three race processes. The deterministic control
reproduced the old behavior, and the reviewed test-only changes are integrated.
See `PACKETFLOW-REGRESSION.md` and Tests `PERFVAR-MEASUREMENTS.md` for receipts.

These results verify ownership and fixture behavior, not memory use. Queue
sizes, deadlines and the 32-MiB iOS-surrogate / 64-MiB Android policies remain
unchanged. Both profiles still require fresh complete Android-device
MEMSTEADY/PERFVAR collection; native iOS total footprint below 50 MiB remains a
separate, unverified requirement. No memory or throughput baseline is promoted.

### 2026-10-17: physical H1 diagnostic and recovery findings

The completed, single-attempt Pixel Wi-Fi/H1 diagnostic displayed **33 Mbps**
on Fast after 88.761 seconds. Its page-target byte accounting was incomplete;
the derived `0.01 Mbps` field is not aggregate Fast throughput. A separate,
intrusive three-stack diagnostic hit the unchanged 90-second Fast deadline;
its provisional 13-Mbps display is invalid as a completed speed measurement.

That intrusive run observed a Go-runtime peak of **26,996,768 bytes
(25.75 MiB)** and zero observed breaches of 33,554,432 bytes. Its 61 retained
runtime events are not independent samples, and its memory summary is
explicitly diagnostic/ineligible. Neither this result nor the completed
33-Mbps attempt qualifies full MEMSTEADY/PERFVAR coverage. The complete
32-MiB iOS-surrogate and 64-MiB Android profiles remain outstanding; no native
iOS under-50-MiB total-footprint claim follows from Android.

Two active snapshots locate TUN producer waits at Transfer Pack admission,
with the dependent sequence worker in service pacing and H1 writers waiting
for input. They do not capture the exact maximum-duration send call or prove
a pacing defect. Do not increase queue/rate limits or impose an accepted-group
lifetime on this evidence alone. Bounded, diagnostic-only pacing/admission
state is being prepared to distinguish legitimate debt from a stalled owner.

Separately, a deterministic actual Multi/Transfer/provider test reproduced a
TCP retry suppression bug after bounded provider reorder refusal. Its repair
is not yet qualified and is not established as the phone pause's cause.
Cleanup now explicitly stops the joined test probe and proves process absence;
that harness repair passed focused and adjacent checks, including 483 backend
race roots. It changes neither memory budgets nor packet queue capacities.
See the [measurement ledger](../tests/PERFVAR-MEASUREMENTS.md) and
`PACKETFLOW-REGRESSION.md`; all failed attempts remain retained.

### 2026-10-19: caller-buffer retention behind closed TCP endpoints

The unchanged host `TestDeviceLocalLoadStability` exceeded its existing
6-MiB warm-heap growth band in the full SDK race process. An isolated normal
diagnostic did not reproduce it; a matching-race diagnostic did: warm
iterations 2–6 grew by 8,243,416 B (7.86 MiB), while dangling endpoints grew
from 7 to 65, all in `FIN_WAIT_2`, with none in `TIME_WAIT`.

The retained chain was
`Endpoint.limRdr.R -> bytes.Reader -> caller backing array`.
Although the write path copied payload bytes into owned packet buffers, its
reusable `io.LimitedReader` kept the exhausted reader and the caller's entire
128-KiB allocation reachable while the endpoint remained alive. The 58 extra
warm-region endpoints correspond to 7,602,176 B (7.25 MiB) of such arrays:
a quantitative fit, not a complete allocation attribution.

The narrow gVisor correction clears only `io.LimitedReader.R` on return from
`Buffer.WriteFromReaderAndLimitedReader`, preserving limiter reuse, its
remaining count, copied bytes and original errors. TCP close/FIN/reset
semantics, orphan timers, GC policy, pool/queue capacities, device budgets,
test workloads, deadlines and memory bands are unchanged.

After a fixture-only list-API correction, old production failed five
retained-reference roots while the uncached-reader control passed. With the
correction, all six roots passed in three fresh normal and three fresh race
processes. They cover chunked limiter reuse, partial copies/errors, zero-read
cleanup, the uncached path, and actual `Endpoint.Write` through copied-packet
verification, acknowledged-data release and synchronous `FIN_WAIT_2`.
Joined teardown removes the dangling endpoint without shortening its timer.
The unchanged SDK load root also passed three fresh normal and three fresh
race processes, with zero skips and completed ownership/source guards.

One matched post-fix race diagnostic passed the unchanged band: warm heap
spread fell to 567,664 B (0.54 MiB), while iteration-6 `FIN_WAIT_2` endpoints
remained at 61 rather than 65. Pool capacity stayed 2,091,520 B; warm-region
retained pool bytes were 417,280–453,632 B after versus 370,944–414,464 B before.
Both attempts ended with zero dangling endpoints and zero outstanding packet
ownership. This source-consistent differential strongly supports releasing
the stale reader reference, not changing TCP lifetime or shrinking pools.
It does not reconstruct every allocation in the original full-process
failure or assign its remaining post-teardown heap.

These are host live-heap measurements at the test's existing collection and
settling boundaries, not physical-device all-phase Go-runtime peaks,
300-second quiet-window qualification, throughput or native footprint.
Both affected gVisor packages passed their full normal and race runs; full SDK
and three-lane campaign qualification remain pending.
Complete 32-MiB iOS-surrogate and 64-MiB Android MEMSTEADY/PERFVAR coverage is
still required; Android/host results cannot prove native iOS lifetime
`phys_footprint <50 MiB`. No physical/performance baseline is promoted, and
all failed attempts remain retained.

A separate short-EOF zero-progress issue in the copy helper has an untested
private candidate, excluded from this repair. Current transport callers
request no more than their payloader's declared remaining length; no ordinary
production trigger or connection to this heap incident is established.

### 2026-10-20: installed-build hosted H1 Fast.com diagnostic

Local-probe remediation: deterministic pre-fix unit and real Transfer tests
reproduced false retirement from exact pre-wire queue expiry; the real test
canceled the client with a written data Pack still pending. The candidate
passed all 44 new/adjacent roots in three fresh normal and three fresh race
processes, zero skips/race reports. Tests pin same-sequence data/ACK recovery,
unchanged pending/ACK clocks, bounded held logging, cancellation, mixed error
classification, and genuine written-probe timeout. An initial synthetic
nil-context fixture panic is preserved separately; successor fixtures only
give the test siblings explicit contexts. Production and both corrected tests
are now integrated byte-for-byte with the qualified private afterimages.
No rebuilt APK or post-fix Fast.com improvement is qualified yet.
Focused closure: `pixel-busy-probe-local-expiry.LQJEjM/FINAL.OUTCOME.json`,
SHA-256 `02e07eedc8143597bda52b8096a4f9c14d30bd1723fd1c1b250e4c12c7521010`.

Post-refresh default-log review found a local false-health-verdict event
inside the measured interval. At 21:13:31.927865 UTC, a busy liveness probe
convicted its exit after `queued Pack expired before serialization: send Pack
was not admitted`. This is a local pre-wire failure, not proof that the peer
failed to answer a transmitted probe. Two sequence cancellations followed;
one recorded 38 pending items. A deterministic root test and narrow fix are
being prepared; no fix or causal resolution of the entire Fast timeout is
qualified yet. The second redacted bundle is retained at
`fast-simple-pixel-diagnostics/urnetwork-diagnostics-20261020-211705-redacted.zip`
under the artifact directory below, SHA-256
`d96de812cea02b924c0e8e89acef0dab00343681b6415f17222130188891f9c7`.

US reconnect follow-up: a fresh session showed H1+ 100% before traffic;
Fast.com displayed **50 Mbps** but remained nonterminal at the unchanged
90-second deadline. Invalid/incomplete, not a completed speed measurement.
Run start was 21:13:18 UTC; receipt `FAST-SIMPLE-PIXEL-H1-REFRESH-01.json`
is retained alongside the receipts below. Provider identity changes were
not verified, so this neither confirms nor rules out a bad-provider cause.
Owners joined, test targets checked absent and owned forward removed.

Pixel `3B161FDJG001KT`, installed app `2026.10.19` / code `1076002883`,
ordinary signed-in UI, United States, showed H1+ 100% immediately before
VPN traffic. No experimental SDK budget treatment was installed.

- Direct, VPN disconnected: **330 Mbps**, completed in 10.252 seconds;
  spinner stopped, valid result, cleanup complete.
- Clean US H1 repeat, one active Fast.com test: **800 Kbps displayed** at
  the 90-second deadline, indicator nonterminal, document complete.
  Invalid/incomplete result, not a completed throughput measurement.
  Page-target counters: 26,315,570 bytes, 32 cancellations, all other fixed
  failure categories zero. Worker traffic may be omitted; these are not
  authoritative speed totals or proof of the stall's cause.
- Earlier VPN run: 8.4 Mbps displayed but timed out with a competing manual
  Fast.com tab; exclude from comparisons. Two setup retries failed during
  navigation without an attributed DNS/TLS/reset category.

This reproduces a substantial hosted-H1 symptom, not attribution to a local
change or remote provider. No fresh Go-runtime memory samples were available;
neither the 32 MiB iOS nor 64 MiB Android profile is qualified by this run.
All owners joined; diagnostic tabs were checked absent, the owned ADB forward
removed, and Pixel left connected to US H1. This is diagnostic evidence only,
not full PERF or baseline promotion.

Raw receipts: `FAST-SIMPLE-PIXEL-{DIRECT-01,H1-CLEAN-01}.json` under
`/Volumes/data/urnetwork-h1-chronology.Y0OYTR/full-campaign-current.Fxlt45`.
Installed base APK SHA-256:
`e1844698948e1eec2544bcfbea84ef4feb5f46feb648d66a978cdf4f7b0a743c`.

### 2026-10-20: receiver-head continuity metadata and bounded host replay

The exact two-file upstream H1 correction
`e65dc397816ffd0cdefc3eef0f94f4d0159b6f57`, authored by Bitprecipice, was
tested as an overlay on Connect `1311aa6b`, not as the complete merged
`58786550` tree or the larger private paid-response composition. It keeps
the full receiver-head denominator while tracking actual gaps between heads.
The deterministic OLD continuous-train root failed; the genuine-hole control
passed. The seven-root candidate set passed three fresh normal and three
fresh race processes.

`TestWindowServiceRecoverySuffixMemoryBound` measured coverage 168 B,
sample 360 B and service 25,688 B in all six processes, below the unchanged
192/512/32,768-B caps. The new scalar adds 8 B per supply and 16 B per sample;
the 67 embedded samples account for 1,072 B of fixed service growth.
The existing owner census charges the service through `unsafe.Sizeof`.
The scalar-only metadata and zero-allocation suffix-merge checks passed;
they do not measure transport-path allocations, retained heap, runtime
peaks or native footprint.

The unchanged eight-offset parent passed three fresh normal processes.
Across 72 one-second bins, the minimum was 95.76448 Mbps and all measurement
relay drops were zero. Nonmeasurement relay drops reached 373 and end capacity
debt reached 14,481,920 B; all debt records completed and settlement error
fields were empty. The fixture's 48-MiB budget/debt cap remained unchanged.
It is not a waiver of the 32-MiB iOS-surrogate / 64-MiB Android memory gates.

Exact source scope, raw receipt hashes, nonmeasurement loss/debt rows and
limitations are in the 2026-10-20 continuous-receiver-head entry in
`PACKETFLOW-REGRESSION.md`. This finite host result does not qualify parent
race, full PERF/MAIN/ALL, physical Fast, either complete device-memory profile
or native iOS lifetime `phys_footprint <50 MiB`. It does not causally resolve
the separate historical loaded-probe 0/64 result. Private grace/full-turn
research is deferred with its evidence retained, not incorporated or
qualified by this result.

### 2026-10-21: busy-probe custody and harness confirmation

Two deterministic pre-fix witnesses reproduced premature channel retirement:
an admitted probe remained unwritten when its local evaluation timer expired;
an already completed error verdict canceled a new pending-send episode after
real ACK progress. Each witness retained an unchanged-failure control that passed.

The integrated correction treats unanswered bounded evaluation as unknown, not
remote failure. One exact callback owner survives the wait, preventing repeated
probe admission. Late results retain their actual callback timestamp; a pending
generation check and terminal error commitment share the channel lock. Physical
read/no-send-ACK and terminal Transfer failure authorities remain independent.

Private and direct shared-source qualification each passed three fresh normal
and three fresh race processes: 85 top-level tests plus 22 subtests per process,
zero failures/skips. Retained evaluation measured zero allocations; owner and
completion values measured 24 B and 40 B, with channel size 1,008 B. These are
bounded structural measurements, not whole-process memory or throughput results.
The first private attempt's one-allocation failure and source-overlay inspection
failure remain recorded; neither was retried unchanged or omitted.

A separate test-only Android fixture correction pins synthetic GOFLAGS rather
than inheriting unrelated compiler flags. Real native provenance still rejects
unsupported flags. Private and integrated 58-test sets passed three fresh
processes each; the full 27-file helper parent then passed 594/594 tests with
the previously failing ambient flags. All owners joined and input guards matched.

Evidence is retained under `full-campaign-current.Fxlt45/` in
`busy-stale-verdict-OLD.zqOyaA`, `busy-custody-v2-resumed.WdTeoP`,
`busy-custody85.qjmUkO`, `busy-custody85-direct.7E4t7n`,
`native-fixture-direct.5tSqTZ`, and `helper-parent594.aFlpO4`.
This documentation append follows completed guards. It does not qualify Fast.com,
the historical 0/64 loaded-probe result, full ALL/MAIN/PERF, complete Android
32-MiB iOS-profile / 64-MiB Android-profile memory, or native iOS total footprint.

### 2026-10-21: rapid-navigation admission hypothesis (source audit only)

Reported symptom: several same-site pages load quickly, then later links pause
for 5–10 seconds. No captured user-session evidence yet attributes this to a
memory limit. Keep the 32-MiB iOS / 64-MiB Android targets unchanged while testing.

Apple `PacketTunnelProvider.readToDevice` calls SDK `sendPacketBatch`
synchronously before rearming the next native packet read. SDK `SendTimeout`
defaults to five seconds. Same-batch ready offers let unrelated already-read
groups advance, but cannot admit a later batch that the native reader has not
read. `TestNativeBatchNextReadRemainsBehindPriorCall` deterministically
characterizes that boundary with retained TCP credit. The existing split-group
scheduler test also permits two consecutive five-second budgets for one flow;
that exact split must not be assumed on the ordinary UpgradeMux path, which
directly forwards groups rather than nesting another whole-batch scheduler.
The native codec may emit multiple frames synchronously, each starting a new
SDK call. These are plausible blocking mechanisms, not incident attribution.

`MaxFlowsPerExit=16` is a placement preference, not a hard connection-admission
limit. Increasing it or adding buffers is therefore not a demonstrated remedy.
More queued bytes can postpone pressure while increasing latency and retention.
Before changing capacity, measure native read/admission/physical-write/first-byte
boundaries, actual root occupancy, refusals and retransmissions using a fixed
healthy H1 provider. Compare a tiny request in the current batch against the
next native read while holding real retained credit; include request-only
cancellation versus actual FIN/RST and require complete ownership reconciliation.

Potential experiments are targeted zero-wait *pre-Transfer* remote admission or
a fixed-byte/count-bounded fair native intake adapter. Neither is implemented or
recommended from this audit alone. Do not globally set send timeout to zero,
drop accepted reliable owners, fabricate ACKs, or add an unbounded async queue.
Installed-app source parity must also be checked before attributing a symptom
to fixes present only in this workspace. This append follows completed run guards.

### 2026-10-21: native-input admission isolation, first causal proof

The pre-Transfer native-input wait is now pinned by
`TestNativeIngressNextReadPrecedesUnrelatedCreditRelease`. It holds actual
Transfer resend credit for one destination, then models sequential native reads:
the first batch targets that blocked destination; the next targets an
independently ready route. The old implementation fails with
`NATIVE_INGRESS_NEXT_READ_HELD_BY_UNRELATED_CREDIT`, while the ordinary blocking
API control passes. This is a causal admission/read-loop defect, not proof that
memory capacity caused the reported physical browsing session.

A private candidate gives native remote groups one ready-only admission offer,
without adding a queue, worker or timer. Local protocol owners and explicit
generic Go senders retain their existing timeout. An unavailable encryption
session remains a temporary refusal rather than a connection reset. Refusal
occurs before Transfer ownership; it must not commit TCP-collapse suppression,
invent an ACK or discard an already accepted reliable original.

Both causal/control roots passed in three fresh normal and three fresh race
processes, covering direct and mux routes, independent wire progress before
credit release, unchanged retained-owner accounting, and successful retry after
credit release. All owners joined and input guards matched. Existing SDK smoke
also passed 13/13 after correcting a candidate compile error; that earlier
build failure remains preserved. Evidence:
`/Volumes/data/urnetwork-native-ingress-connect-proof.QAU94k/` and
`/Volumes/data/urnetwork-native-ingress-sdk-smoke-v2.Y2wD90/`.

The actual SDK packed-call and native-fd regressions subsequently reproduced
both expected old-code blocking failures. The candidate passed both roots in
three fresh normal and three fresh race processes, with exact pooled-root
count/byte balance after joins. The initial packed fixture incorrectly placed
gVisor's custom, non-durable waits inside `synctest` and timed out at 120 seconds;
that result is preserved and is not causal evidence. Its test-only successor
uses a bounded two-second watchdog against the unchanged five-second send
budget, with the actual remote/mux/local-DNS owners retained.
Closed SDK proof:
`/Volumes/data/urnetwork-native-ingress-sdk-proof-v2.ny28zy/OUTCOME.json`, SHA-256
`5bbee0e7e37c777cf4903e67955489da170777a857d3a1b15b02fb7402a06582`.

The first ordinary PERFVAR old/new comparison completed 100 rapid pages across
four synthetic sites, each totaling 50–300 KiB of HTML, JS, CSS and images.
Both versions passed: 99 proved overlap transitions, 100 actually canceled
assets, 17,879,040 planned body bytes, 13,495,296 completed body bytes and
102,400 canceled-prefix bytes. Prior assets were held while the next HTML
started; work stayed bounded to two pages/eight asset handlers. The normal
fixture bridge remains unchanged unless this workload opts into native input.

| Per-page timing | Old | Candidate |
| --- | ---: | ---: |
| HTML TTFB p50 | 7.091 ms | 7.235 ms |
| HTML TTFB p95 | 14.297 ms | 13.668 ms |
| HTML TTFB maximum | 29.723 ms | 32.986 ms |
| Asset completion p50 | 13.845 ms | 13.187 ms |
| Asset completion p95 | 26.583 ms | 25.569 ms |
| Five-second navigation stalls | 0 | 0 |

This clean-network pair did not reproduce the reported browsing pause and does
not establish a statistically significant speed change. The separate causal
old-code failures remain the evidence for the admission/read-loop defect.
Sampled host-process heap peaks were 116,141,000/116,503,168 bytes, including
the fixture/provider/HTTP workload: these are not mobile 32/64-MiB measurements
or absolute peak bounds. No capacity increase or deployed-app fix is claimed.
Both owners, fixture cleanup and source/input checks passed. Closed outcome:
`/Volumes/data/urnetwork-native-browsing100.PREP.Tf7V4t/OUTCOME.json`, SHA-256
`d315bacd60f7b61c9b1b727b1c6655bff147fb2e565527adcec2691986e5b4cf`.
# Native admission follow-up: adjacent checks and fixture failures (2026-10-21)

The private native-admission candidate passed eight roots in three fresh normal
and three fresh race processes (48/48, no skips). Allocation census measured
one extra allocation per native batch at both one and 64 packets; bytes were
not measured. Evidence: `/Volumes/data/urnetwork-native-ingress-adjacent.uTqbVV/OUTCOME.json`.
The run exceeded its nested build/cache work allowance; its outcome preserves
that resource failure separately from test correctness.

The first ordinary PERFVAR unavailable-route control is **not qualified**:
OLD hit the expected next-read marker but also a synctest cleanup panic; NEW
failed its return-identity assertion and had the same panic. Both owners joined
and input guards matched. Evidence:
`/Volumes/data/urnetwork-native-browsing-boundary.HCoJ8W/OUTCOME.json`.
Fixture repairs must precede integration; these are not additional passing
production tests. A separate audit found that the 100-page canceled-asset branch
could mask a joined prefix-validation error with context cancellation. Existing
clean OLD/NEW passes remain recorded, but do not prove that negative invariant.

The native candidate and regression sources were integrated into Connect, SDK,
and ordinary Server PERFVAR on 2026-10-21. Fifteen afterimages matched
`/tmp/urnetwork-native-ingress.jmLXFw/INTEGRATION.v2.MANIFEST.json`;
existing dirty busy-probe changes were preserved. The successor fixtures warm
the process-owned pool before synctest, distinguish forward callback identity
from the parsed return tuple, and independently reject canceled corrupt bodies.
These test-only corrections still require execution. Integration is not a
full-run, physical-device, memory-budget, deployment, or speed qualification.

Direct post-integration verification then passed the repaired PERFVAR boundary
and canceled-prefix roots (2/2). Nine Connect roots, including the ownership
heading audit against actual source files, passed in three fresh normal and
three fresh race processes (54/54). All owners joined and input guards matched.
Receipt: `/Volumes/data/urnetwork-native-ingress-integrated-confirm.2PNKHU/DIRECT.CLOSED.json`.
The repaired PERFVAR OLD arm is still not run; no new OLD-fails/NEW-passes claim
is attached to these direct results. Earlier separate Connect/SDK causal proofs
remain the evidence for the admission defect.

The repaired ordinary PERFVAR boundary now has its own clean causal proof:
exact reconstructed OLD Connect and SDK preimages produced only
`NATIVE_PERFVAR_NEXT_READ_WAITED_FOR_REMOTE`; integrated NEW passed. The same
two NEW PERFVAR roots passed in three fresh normal processes. The earlier OLD
compile failure (missing the SDK preimage in its overlay) remains retained with
zero test credit. All final owners joined and guards matched. Final receipt:
`/Volumes/data/urnetwork-native-ingress-integrated-confirm.2PNKHU/OUTCOME.FINAL.json`
(SHA-256 `8d2576001a0d1ddaf0a4d5e4080bc802d8ac4f7fd05c8534abef51c34bb3eb27`).
This qualifies the local admission-isolation fix, not physical pause attribution
or a speed/memory improvement. Ready cold SYNs can still enter the existing
race; unavailable cold flows now rely on source retransmission rather than
blocking unrelated native reads. No additional queue or mobile budget change
was introduced. The 100-page successor and complete runner still need replay.

Post-merge focused confirmation passed on the synced checkpoint: nine Connect
and 15 SDK roots, each in three fresh normal and three race processes (144
passes, zero failures/skips). All owners joined and input guards matched.
Receipt: `/Volumes/data/urnetwork-postmerge-native-controls.wEP9uh/OUTCOME.json`,
SHA-256 `edbb79b7af0a1707b41866bddc432ba7257c54b87811f1bb380f4ae57cb795c9`.

The clean 100-page simulation is not a realistic accumulating-download test:
it joins the prior page every iteration, limits overlap to two pages/eight
handlers, and parks image bodies after 1 KiB until retirement. It also uses
plain HTTP over loopback/random-port logical destinations, bypasses UpgradeMux,
and does not enforce SDK mobile memory admission. Its fixed provider already
selects the Quality window, so random ports alone do not imply Speed routing.
These are coverage gaps, not evidence of a production failure. A separate
flowing-body, four-page, shaped-link workload is being developed; no realistic
pause reproduction is claimed yet.

### Flowing 100-page pressure workload: pauses reproduced (2026-10-21)

The private successor streams image suffixes immediately after their prefix,
cancels selected prior-page requests at the next tap, and retains at most four
page owners. It uses the existing seeded 5 Mbps-down / 1 Mbps-up, 120 ms-RTT
profile with jitter/loss, unchanged production limits, and exact response-byte
validation. Four scheduler/trace/body-flow/cleanup controls passed in three
normal and three race processes (24 passes).

| Observation | OLD admission | NEW admission |
| --- | ---: | ---: |
| Completed page owners before workload deadline | 86 | 88 |
| Navigations taking at least 5 seconds to first byte | 11 | 6 |
| Largest observed navigation time | 15.54 s | 18.89 s |
| Cumulative native bridge send-call time | 64.536 s | 0.246 s |
| Maximum active HTTP requests | 5 | 6 |
| Result | FAIL | FAIL |

Both hit the unchanged shared five-minute deadline and retained a separate TUN
generator-retirement teardown error. OLD stopped its planned pair; NEW was a
separately authorized diagnostic run, not a qualified paired performance test.
All owners joined, input/source guards matched, and fixture-owner cleanup
passed. Do not interpret the lower NEW stall count as a statistical win or the
native admission fix as a complete browsing remedy. Actual overlap remained
below the four-page ceiling; no artificial maximum backlog is claimed.

OLD's first pause coincided with inner TCP retransmissions/timeouts and native
admission delay, without corresponding outer TCP timeouts. NEW still paused
with very little native send-call time; its first slow navigation used a reused
connection and waited both for acquisition and after request write. Aggregate
carrier loss/recovery counters also advanced. Memory exhaustion is not proven.
Speculative dial completion may occur after a pooled connection is acquired;
raw dial events must not be assigned to that request's selected connection.

Receipts: `/Volumes/data/urnetwork-browsing-pressure-pair.ILs2ex/OUTCOME.json`
(SHA-256 `31481e92b08b9f75ae59a7c16d69d9dea4596fbffd79e56757e9d9d7d97b10c8`),
`/Volumes/data/urnetwork-browsing-pressure-NEW-only.mRO7uC/OUTCOME.json`
(`197873175949ec9a8e9a248151ce7b973a62c8516b97cbf31bcd5c12f40db5c3`).
Next: bind TCP/refusal diagnostics to the actual selected connection and test
whether collapse filtering suppresses duplicate-ACK fast-recovery feedback.
That is a hypothesis, not yet the demonstrated cause of these pauses. The
simulation still uses host budgets, plain HTTP, one provider, and no DNS/SNI;
it does not qualify the 32 MiB iOS or 64 MiB Android profiles.

### Duplicate-ACK recovery proof and negative performance screen (2026-10-21)

A composed deterministic test now confirms a distinct recovery defect: after
return bytes are lost downstream of an acknowledged Transfer delivery, three
new identical pure TCP ACK events are suppressed by Multi's collapse gate.
With collapse enabled, all three are refused, the provider receives zero
duplicate ACKs, and fast replay does not start. Disabling collapse alone makes
the same fixture recover before its resend timer. Actual gVisor TUN output
independently confirms the unchanged sequence/ACK/window feedback premise.
The initial witness had an unhealthy mock provider and failed before SYN
admission in both arms; that failure is retained, not counted as causal proof.

The private six-line candidate permits pure ACK events through the shared
collapse gate while retaining payload/SYN/FIN/RST suppression and ordinary
Transfer ACK ownership. It introduces no queue, memory allowance, or timer.
Four focused roots passed three normal and three race processes (24 passes).
Two older tests expected repeated ACK suppression; stronger replacements
explicitly check source-ordered ACK/window state and real repeated delivery.
Those and an admission ownership control passed normal/race (six passes),
after correcting a mock-accounting recovered panic. Broader verification is
not green: 16 other selected tests also fail on unchanged production source
and remain separately tracked. Do not count a recovered panic as a clean pass.

The diagnostic collector passed 42 normal/race control cases. Its selected
connection identity is explicit; speculative dials are not attributed to the
request. Diagnostic report storage is bounded at 128 KiB, not a production
memory or protocol-frame change. Its identical pressure workload produced:

| Single traced observation | Unchanged code | Private ACK candidate |
| --- | ---: | ---: |
| Completed page owners | 70 | 54 |
| Completed navigations taking at least five seconds | 18 | 16 |
| Largest completed first-byte time | 15.516 s | 19.231 s |
| HTML page at global deadline | 73 | 57 |
| Retained pool roots after teardown | 0 | 0 |
| Result | FAIL | FAIL |

Both use the same five-minute context and seeded link. The unchanged report's
`slow_navigation_count=19` additionally includes its final failed HTML request;
do not confuse that denominator with 18 completed slow rows. This one-sample
screen does **not** demonstrate a performance improvement, nor statistically
establish a regression. The candidate remains private/unintegrated; no speed
baseline is promoted and no mobile memory qualification is implied.

At the first slow boundary, both arms' actual selected sockets show a two-
segment congestion window. Their last-healthy samples use different socket
IDs, so RTO/cwnd values cannot be interpreted as before/after on one flow.
The candidate also shows a coincident outer-device TCP timeout. Missing
matching entries in the four-receipt refusal ring cannot exclude older refused
packets because omissions are explicit. These facts justify further carrier
recovery tracing, not a proven explanation for every pause or more memory.
Next deterministic coverage includes three ACK events collected in one bulk
call, including provider replay and refusal/retry ownership.

Evidence: `/Volumes/data/urnetwork-return-feedback-core-confirm.fPvpS6`,
`/Volumes/data/urnetwork-return-feedback-v4-focused.rNN9fB`, and
`/Volumes/data/urnetwork-browsing-trace-v2-fixed-observation.LUxwod/RUNTIME-OUTCOMES.v2.json`
(SHA-256 `96064bf7ad97f3c8013364fae7b5260f617228a0a14b9340e4291f0a9559638f`).
Both measurement owners joined, source/input guards matched, and fixture
cleanup succeeded. A separate initial launch failed exact-toolchain preflight
before traffic; it is preserved separately from both workload failures.

Bulk feedback follow-up also has causal proof: the unchanged-production
three-ACK batch root refuses all three events (`admitted=0`, no wire frames).
With the same frozen witness and private fix, two roots pass three normal and
three race processes (12 passes). Public batch/native/mux paths preserve three
matching frames, each physical Pack receives its genuine peer ACK, and the
provider fast-replays the retained hole before its timer. IPv4/IPv6 bulk
refusal/retry ownership is pinned too. An earlier witness incorrectly required
one Pack; the generic writer's existing two-frame limit correctly emits 2+1.
That fixture failure is preserved, with no batching-policy change. Evidence:
`/Volumes/data/urnetwork-return-feedback-v6-bulk.yxXXxn`.
The compact outcome index was extended in place before freezing this version;
its earlier published hash no longer resolves to that index. The versioned V2
receipt is now immutable, and all underlying raw attempts remain preserved.
This proves the recovery invariant, not a browsing-performance win. Next
diagnostics must identify the actual outer H1 carrier across request phases;
connection-acquisition waits are not equivalent to response-transit stalls.

### Device lane-rule discriminator and shared-pump proof (2026-10-21)

The device-only `ReliableLaneProvenRecovery=true` diagnostic is a negative
screen, not a proposed default. Three existing lane controls pass normal/race
(six checks), but the unchanged pressure workload fails after about246seconds
with page2 asset0 receiving24invalidbytes. The synthetic server's duplicate-
request HTTP409 body has exactly that length; status/body were not recorded,
so that attribution remains an inference pending a deterministic retry test.
Six HTML responses returned, with first-byte times3.211,50.763,105.677,37.862,
0.895 and1.068seconds. Only two page owners finished retirement. Do not report
the completed-owner maximum50.763seconds as the largest observed HTML delay:
that is105.677seconds. No traffic, queue, profile, timer or memory limit changed.
This arm neither qualifies the lane flag nor proves retry amplification caused
the baseline pauses. All owners joined, source guards matched, fixture cleanup
passed, and pooled roots/bytes drained to zero. Frozen result:
`/Volumes/data/urnetwork-browsing-device-lane-pressure.BfY7mr/OUTCOME.v1.json`
(SHA-256 `1bc7a5e37b0a1d786b034596914066719b81c906e9e2013f39122fc1c11a2edb`).

A separate real-Client deterministic witness confirms shared receive-pump
head-of-line blocking. With the reliable default handoff, sequenceA waits at
capacity and sequenceB's genuine ACK remains ready on another route but cannot
dispatch. Releasing A retires B exactly once, proving ACK validity. Zero-wait
and explicit-unreliable controls immediately refuse A, dispatch B's ACK, and
preserve exact A retry/deduplication. Normal result is one expected failure and
two passes, not an overall passing suite. Evidence:
`/Volumes/data/urnetwork-shared-pump-ack-proof-v2.D9WBjt/OUTCOME.v1.json`
(SHA-256 `88149c872a98b3d092102cb3d6822388deb3228a2a3a598ec3b47a65e9ca52ba`).
No production fix is integrated yet. First-slow handoff waits were zero in the
browsing sample, so do not label this its demonstrated cause. A private fix
will enforce zero-wait at shared dispatch while preserving dedicated-owner
wait APIs and ACK/retry custody. A bounded actual-H1-carrier diagnostic must
also capture terminal workload failures, not just the first slow HTML. Per
user instruction, ongoing debugging/fixing now uses Sol6.1 at maximum reasoning.

2026-10-21 continuation: the private shared-pump zero-wait fix has 18 core
and 42 adjacent normal/race passes, including exact refusal/retry custody,
healthy siblings and balanced budget/credit checks. An initial adjacent
cancellation fixture failed because an already-admitted old item could drain;
the revised fixture isolates worker retirement and asserts payload identities.
No production integration or successful 100-page replay is claimed yet.
Evidence: `/Volumes/data/urnetwork-shared-pump-adjacent-v2.dpjtv2/OUTCOME.v1.json`
(SHA-256 `34fc613139696ecd7dac6db0db7c6d784df99186e570e3afc8c71765602faa26`).

The browsing driver also rejected valid retried GETs as duplicates. A real Go
Transport replay deterministically produces the guarded handler's 409/24-byte
failure; bounded retry-aware handler ownership and unchanged exact body
validation pass 54 normal/race roots. This removes a test artifact, not the
observed slow-page cause. Previous first-slow zero-wait counters cover device
clients, not provider wait/success/saturation. The next diagnostic includes
those provider scalars to avoid excluding provider receive-pump starvation
without evidence. Mobile memory limits and the shaped-link workload remain
unchanged throughout this investigation.

#### 2026-10-21: realistic browsing screen still fails; memory is not established as cause

The retry-aware V3.2 diagnostic passed120 focused normal/race roots, then ran
three unchanged-profile observations: baseline81 retired page owners/84 valid
HTML rows/max12.556 s first-byte; shared-pump fix78/81/max13.848 s; pure-ACK
correction34/38/max42.290 s. First two reach the original five-minute deadline;
the last fails early on an HTTP asset reset. No candidate demonstrates a
performance win, and one observation per arm is not statistical qualification.
Full counts, failures and exact provenance are in tests/PERFVAR-MEASUREMENTS.md
and `/Volumes/data/urnetwork-browsing-pureack-carrier-retry.PREP.jQH82R/THREE-ARM-OUTCOME.v1.json`
(SHA-256 `ebe792d07b229fe1bdf6742ec87727975c8b2ee04a1301185ab8ce8359fdbbf7`).

Both client and provider receive waits/saturation/drop counters stay zero.
Baseline first slow acquisition is11.227 s; its same actual outer H1 socket
has no new TCP timeout and roughly200 ms RTO across that interval. Another
arm has a6.595 s post-write reply wait without new outer timeout. These are
not evidence that more memory or a larger connection cap fixes the problem.
Provider inner TCP replay counts as a new Transfer send, so zero Transfer
timeout-resend counts cannot exclude its1/2/4/8 s recovery path.

The pure-ACK run removes a provider on30.334 s no-send-ACK health and tears
down10 flows before its asset reset. A separate deterministic real-client
test now proves Multi can ignore a fresh valid selective receipt: receipt age
2.51 s, retained Transfer lifetime27.49 s remaining, but hard health age31.5 s
causes blackhole. Causal RED plus6 controls GREEN. Receipt health must remain
distinct from final delivery/RTT/byte credit; no timer extension is proposed.
Receipt `/Volumes/data/urnetwork-selective-ack-health-proof-v4.lQSuyY/OUTCOME.v1.json`
(SHA-256 `ee094fbaf4fd8764d0e880d7a48e4cd4d4d79acd6d0a05e7b220e831c2203408`).
Its contribution to the observed session still needs verification. Repairs
and an actual HTTP-asset/pool-reuse root-cause test are private work in progress;
no100-page pass, production integration or mobile memory qualification claimed.

#### 2026-10-21: HTTP ACK-collapse repair validated without more memory

A real HTTP/gVisor/Multi/Transfer/provider witness now demonstrates a return
hole whose three genuine duplicate TCP ACKs are suppressed by the old collapse
gate. The Transfer Pack has already been peer-ACKed, so Transfer resend is not
the missing recovery owner. Allowing pure TCP ACK feedback produces one fast
provider replay, completes the exact 4,352-byte asset, and reuses the same HTTP
connection before the provider recovery timer is released. A second case starts
the next navigation while that asset is incomplete and verifies its subsequent
same-connection completion. Both cases have collapse-disabled controls, causal
OLD failures, and 12 GREEN root passes each across three normal/three race
processes. No memory/connection cap, profile or timeout was increased.

Evidence and the V1.2 initial-log overwrite disclosure are recorded in
tests/PERFVAR-MEASUREMENTS.md. V2.1 repeat raw evidence is preserved separately at
`/Volumes/data/urnetwork-http-return-feedback-v2-1-repeat.Cz3cpu`.
This supports repairing recovery feedback before allocating additional memory;
it does not yet qualify the full 100-page workload or either mobile memory gate.

TCP attribution caveat: current installed-socket diagnostics measure the device
endpoint, not the opposite edge sender of downloads. Its low RTO/no new timeout
cannot exclude edge-side download recovery. The provider carrier is a different
TCP connection. Edge-side measurements are needed before attributing the
remaining waits to an inner flow, acquisition policy or memory pressure.

#### 2026-10-21: combined fixes still do not clear the pressure workload

The health/pure-ACK/shared-pump composition passes 48 focused normal/race
checks but still fails the unchanged 100-page deadline: 77 owners completed,
maximum valid HTML TTFB 12.364 s. The next run adds bounded whole-edge atomic
TCP counters (22 collector checks pass) and also fails: 65 owners, maximum
16.021 s. Its first slow reply waits 12.595 s with no edge retransmit/timeout
counter increase and continuing edge traffic. Later edge timeouts do not explain
that earlier interval. Both runs drain pooled ownership to zero and pass source
and fixture-cleanup guards; neither qualifies performance or mobile memory.
Exact measurements/receipts are in tests/PERFVAR-MEASUREMENTS.md.

The new health receipt path is allocation-free in focused measurement, about
208 ns/op; generic cumulative ACK handoff costs about six additional ns/op.
Inline ownership sizes grow and are charged by retained sizeof accounting;
unchanged allocator classes for sendItem/Pack/sequence do not mean zero total
cost (ACK overflow and channel classes grow). No memory limit was raised.

The next lane-isolation experiment must share an explicit aggregate retained
retry budget across lane zero and all data lanes, identically in both arms.
Default nil budgets otherwise add a separate fallback data-lane pool when lanes
are enabled. Lazy lane metadata/goroutines remain a separately measured cost;
equal retry budgets are not equal whole heaps or a 32/64 MiB mobile proof.

A real HTTP custody test separately confirms a missing SYN-ACK recovery owner:
after genuine Transfer ACK and downstream loss, the source's actual same-SEQ
SYN retry is collapsed while the provider has no retained return chunk and the
inner handshake remains unacknowledged. The held-original control completes
HTTP/reuse. Provider SYN-ACK retention is being investigated with its metadata
charged to existing return budgets; no extra queue or timer limit is proposed.
This is not yet attributed to the 100-page session and is not memory qualification.

The explicit strict-budget zero/eight-lane pair completed on 2026-10-21. With
one shared 2 MiB retained retry budget per endpoint in either arm, zero lanes
completed 87 page owners before page 90's HTML deadline; eight completed 43
before page 46's deadline. Maximum valid HTML TTFB was 17.494 versus 30.414 s.
Both drained pool roots to zero and joined cleanup; neither passed the unchanged
five-minute workload. This single pair does not support a lane-count increase.
It does not qualify either mobile profile or establish equal whole-process cost.

In strict lifetime accounting, `setLifetimeBudget` clears legacy per-sequence
budget floors. Increasing `LaneFloorByteCount` is consequently inert here;
legacy floor exemptions must not be used to overdraw the aggregate memory cap.
Current retained admission already grants oldest-fitting queued reservations
under the root lock before waking waiters. A new reserve/partition policy would
need independent causal and performance evidence, not an assumption that the
old broadcast-only admission still exists.

Raw paths and source/cleanup attestations are recorded in
`/Volumes/data/urnetwork-health-ack-pump-combined-v1.3uEcVK/STRICT-LANE-PAIR-OUTCOME.v2.json`
(SHA-256 `43baf1ea41dc9adbc15db15f8330958303de910af71f8d5bd05893b18842937c`).
V2 corrects HTML-row counts to include zero-indexed page 0; completed-owner
counts and timing are unchanged. The original V1 evidence remains preserved.

Adding the separately proven SYN-ACK retention fix to the zero-lane candidate
still fails the same 100-page workload: 63 completed owners / 66 HTML first-byte
rows before page 66's deadline. Page 10 waits 31.652 s acquiring a connection,
then 4.671 s for its reply. The eventual connection is reused; a later speculative
dial completion belongs to a different attempt. No memory or connection limit
was raised. Both the new 11-root normal/race cohort and the existing 73-root
OLD/FIX normal/race comparison pass, but neither establishes browsing speed.
The full run drains pool roots to zero and passes source/fixture cleanup guards;
it is not a mobile memory qualification or a demonstrated performance win.
Receipt: `/Volumes/data/urnetwork-strict-lane0-syn-v2.vinyoc/OUTCOME.v1.json`
(SHA-256 `344db16f5242a85211a7506310bae27d9330ddff43862edb5ccb1306311c2e31`).

The bounded item-lineage replay on 2026-10-21 still fails: 50 completed owners /
53 valid HTML first-byte rows before page 53's deadline. Instrumentation makes
this diagnostic-only, not a speed or mobile-memory qualification. It proves
that the first slow HTML reused the earlier asset's actual TCP connection, with
two additional native retransmission timeouts on that connection before reuse.
Sampled pre-Multi output drops and Client-to-Pack handoff drops/waits are zero.
Recorder drops/overwrites and late selected-connection binding prevent assigning
the original missing packet or custody boundary. No memory increase follows
from this evidence. Pool roots drain to zero and source/fixture cleanup guards
pass; the 32 MiB iOS-profile / 64 MiB Android-profile gates remain unqualified.
Receipt: `/Volumes/data/urnetwork-browsing-item-lineage100-prep.ynVfVT/OUTCOME.v1.json`
(SHA-256 `a437999c9122bec888bb79c2cac1ff6751194a154544802e9bed28aed2c086da`).

The same-driver direct/native null subsequently completes all 100 pages on
the shaped cell-edge link in 163.43 s, with all 400 asset operations joined,
one slow HTML navigation (maximum first byte 9.557 s), and zero retained pool
roots after drain. No app memory/connection/window limit was increased. It
removes Multi, Transfer, outer H1 and provider NAT, and uses a gVisor origin peer;
the packet-loss realization therefore differs despite matching link settings.
The result narrows the remaining full-tunnel stall investigation but is neither
a tunnel performance win nor a 32/64 MiB mobile qualification. Native TCP
retransmissions still occur in this passing control; counts alone cannot assign
the failing tunnel's cause. Receipt:
`/Volumes/data/urnetwork-native-browsing-direct-null-controls-v1.jSZw5a/ROOT-OUTCOME.v1.json`
(SHA-256 `716fef7aae32a49f8fa43b6a38bbe076efd7c8d3ca3e4f041618d7ff488e5294`).

### 2026-10-21 — pacing-only full100 discriminator

Without raising any configured queue or memory budget, the private initial100
candidate with only the existing pacing bypass enabled completes 100/100 pages
in 171.881 s, with zero slow navigations and 3.158934 s max HTML first-byte.
All 400 asset workers join and pooled roots/bytes drain to zero. Source and
cleanup guards pass. Delivery sizing, receive advertisement and explicit 2 MiB
retry budgets remain configured. The historical pacing-on base failed at
72 owners/75 HTML before 300 s. This is one intervention, not a repeat-qualified
production fix; dynamic feedback differs. The host multi-process fixture heap
is not mobile-profile memory usage, and zero pool retention does not qualify
the iOS 32 MiB or Android 64 MiB steady/peak gates. Next isolate controlled
drains versus rate/meter behavior without enlarging memory. Receipt:
`/Volumes/data/urnetwork-browsing-sizing-ab.SuqKJX/PACING-OFF-OUTCOME.v1.json`.

Fresh pacing-on confirmation fails at46owners/49HTML,10slow,max63.078s;
source/cleanup guards pass and pooled roots/bytes drain to0. This strengthens
the subsystem lead without requiring a larger memory budget. Receipt in the
same directory: `PACING-ON-OUTCOME.v1.json`. Controlled-drain isolation is next.

Controlled-drain-only isolation also fails:87owners/90HTML,12slow,max13.322s.
All started workers join,pools0,and source/fixture guards pass. This keeps byte
pacing and budgets unchanged, so removing drains alone does not fix browsing.
The next target is rate/meter/feedback capture, not a larger queue. Receipt in
the same directory: `NO-DRAIN-OUTCOME.v1.json`. No mobile memory qualification.

### 2026-10-21 — actual default-off pacing candidate

The canonical implementation, without private production overlays, completed
the unchanged overlapping 100-page workload: 100 navigations, zero five-second
stalls, maximum HTML first byte 2.71711175 s, workload 169.473434875 s (test
175.66 s). All 400 asset workers/server attempts joined, no pending owners or
body-close errors remained, and pool roots/bytes drained to zero. Exact fixture
cleanup and matching source-before/after checks passed. Receipt:
`/Volumes/data/urnetwork-native100-canonical.PREP.nZ1mNs2a/OUTCOME.json`.

Unlike the earlier broad experimental bypass, this candidate gates only the
extra application pacing wait. Window resizing, route-policy checks, ACKs and
hard byte ownership budgets remain enabled; configured memory was not enlarged
to obtain this result. It is one focused host diagnostic, not statistically
qualified throughput or 32-MiB iOS / 64-MiB Android memory evidence.

The pressure root now rejects five-second navigation stalls explicitly, with
eight deterministic acceptance variations; its eleven-root small cohort passes
normal/race on Go 1.27.3. Newer upstream transport/window/SDK changes were then
merged for the test-build checkpoint, so a fresh full100/full-suite replay is
still required. The first post-sync attempt refused the stale harness toolchain
directive contract before traffic; do not count that as a packet-flow failure.

After repairing the selector, the merged-source canonical replay also passes
with the permanent latency gate: 100 navigations, zero five-second stalls,
maximum HTML first byte 3.094035291 s, workload 179.321613750 s (test 185.58 s),
95 overlap proofs. All 400 asset clients/server attempts join; pool roots/bytes
drain to zero. Wrapper exit, exact fixture cleanup and source-before/after
identity checks pass, with no production overlay. Evidence:
`/Volumes/data/urnetwork-native100-postmerge-fixed.PREP.vCUWKke0`.
The 169.47-s and 179.32-s workloads use different source snapshots, so their
difference is not a statistical regression comparison. Full-suite and mobile
memory qualification remain outstanding.

### 2026-10-21 — avoid confusing retry-state stalls with memory pressure

Broader correctness exposed another bounded-flow defect after the fixtures
were given realistic positive provider health. Silence inference withdrew an
incumbent at three seconds, but its group retained the incumbent's affinity
reuse flag. Fresh replacement offers then failed the final reuse check and
waited for the application's next SYN at seven seconds. Adding memory would
not repair that stale placement decision.

The explicit real-source regression is OLD RED with zero replacement Pack
starts at three seconds. Clearing only that flag after a successful guarded
unbind makes the nine-root cohort pass (0.603 s): the same third SYN reaches a
real replacement source at three seconds with exactly one ACK-required start,
route write and terminal completion. Held-producer response custody and pool
reconciliation also pass. No queue, memory budget, timeout or ACK policy changed.
Evidence: `/Volumes/data/urnetwork-connect-health-six.wDnTj2da`.

The minimal fix is integrated, but expanded race/full correctness and another
canonical 100-page replay remain pending for that source. These ownership
measurements are not peak runtime/footprint qualification of either mobile
profile, nor proof that this defect caused the reported real-device session.

### 2026-10-22 — scoped hosted H1 mobile-profile measurements

Two single-cell WiFi diagnostics used the allowlisted Android device, provider
mode off, hosted H1, five Wikipedia loads, three Fast.com measurements, and a
330-second quiet window. These are separate provider-paired runs, not an A/B
performance comparison or the full physical PERFVAR matrix.

| Profile | Peak Go runtime | Quiet maximum | Fast.com Mbps | Verdict |
| --- | ---: | ---: | --- | --- |
| iOS 32 MiB surrogate | 28.54 MiB | 24.41 MiB | 13 / 41 / 10 | Memory passes; median 13 misses 40 Mbps goal |
| Android 64 MiB | 39.03 MiB | 31.36 MiB | 40 / 84 / 13 | Memory passes; median 40 meets goal |

Both final memory summaries report zero cap breaches. Android teardown peaked
at 30.12 MiB; its quiet window lasted 332.882 seconds. Its Wikipedia median
TTFB/load was 126.8/486.1 ms. Source identities, native-input proofs, cleanup,
and process joins passed. Do not infer a benefit from the larger budget from
these unpaired samples. Android does not qualify iOS extension `phys_footprint`
below 50 MiB, and these cells do not qualify the entire dual-profile matrix.

Receipts: `/Volumes/data/urnetwork-scoped-h1-workerdiag-v3.PREP.p0hkU5rH/replay-executor/RECEIPT.json`
and `/Volumes/data/urnetwork-scoped-h1-workerdiag-v4.PREP.FHYINzAU/replay-executor/RECEIPT.json`.

The separate natural SDK32-admission 100-page host test also passed: 158.69 s,
100 navigations, zero slow navigations, and all 400 asset operations joined.
It retained overlapping unfinished assets, the inclusive five-second gate,
source10/provider0 constructor settings, and unchanged budgets; no packet hole
or budget redistribution was injected. This is admission-policy evidence,
not host/mobile peak-memory qualification. Two matched traced 8.75 MiB
downloads passed, but the earlier independent 30-second partial-body failure
remains unexplained. Neither extra memory nor a browsing-stall fix is proven
by these passes. Combined-fix/full-suite requalification remains outstanding.

## 2026-10-24: first passing public SDK 100-page recovery replay

The combined service-affinity and canceled-resize fixes passed one uninstrumented
PERF8 replay of `TestFullTunH1Sdk32ServiceRefusalRecovery100Pages`. The existing
five-second navigation, 300-second workload, original CSS socket/body ownership,
and 30-second SDK join requirements were unchanged. This is a first successful
replay of the bundle, not an isolated donor effect or statistical qualification.

| Measurement | Prior selector-only failing replay | Combined candidate |
| --- | ---: | ---: |
| Page 1 first byte | 5.674730208 s | 3.565715708 s |
| Page 1 inclusive tap to first byte | 5.674792875 s | 3.565773750 s |
| 100-page median first byte | 1.135640146 s | 0.981621958 s |
| Candidate p95 / maximum first byte | — | 2.163823292 / 3.905678417 s |
| Pages at or above five seconds | 1 | 0 |
| Completed HTML / assets / overlapping navigations | 100 / 400 / 99 | 100 / 400 / 99 |

The candidate retained all 12,288 bytes of the held CSS response on the original
socket and retained its public owner across navigation. Its 7,680 native packet
operations completed, all origin dials matched closes, the late constructor
joined, and the successful SDK post-join ownership snapshot reported zero total,
transfer-root, platform-transport, and pending-handoff usage. The failure-only
stack capture was not invoked. The test process exited zero with one run/pass,
no skips or assertions, and its process group joined. Final enclosing resource
monitor cleanup is tracked separately.

Deterministic cause: ordinary fresh IPv4/IPv6 affinity could inherit a healthy
provider already rejected for the exact destination service. Apply the existing
service-failure history while ranking donors and recheck it at final affinity
adoption. Keep explicit pins, established data, UDP policy, expired/missing
history, and the ordinary all-failed fallback unchanged. The private fix passed
756 positive checks with two causal OLD failures. A separately reproduced resize
cancellation defect could re-enter maintenance after its owner was canceled;
its loop-head cancellation guard passed 189 positive checks. The merged bundle
passed another 52 focused normal/race checks and two compile checks before the
full replay.

The aggregate fixture heap includes six in-process providers, SDK, origins and
workload owners. Its 321,002,640-byte sampled heap peak is not an isolated iOS
profile measurement. This run does not qualify the 32 MiB iOS surrogate or
64 MiB Android profile, nor establish iOS total physical footprint. Message-pool
roots and bytes drained to zero. Fresh physical builds and unchanged full112
qualification remain necessary after final source integration.

Evidence: `donor-ctx-stack-candidate.PREP.WkV6x5/PERF8-full100-original-once`,
started 2026-10-24 05:19:21.798 UTC, closed 05:22:50.045 UTC; actual Go package
elapsed 200.509 s. Raw SHA-256:
`b6988b9501ee2c7d336550ef8a5df8b4740e3865faa21c17eebc1d498b72c853`.
Finite measurement SHA-256:
`254b0b7cdc8838553c9fa2a7ce59ade5dcf51b2a1eefefce9aff3628759ad062`.
Source: Connect `897b2321`, Server `cf024ffef`, Tests `8fe35c2` plus reviewed
private donor/resize/capture overlay `iayrfa82`; source25
`e894f8c2260db9e14580882c821edecf7cfe06c0938d6f90d4ebe71f429a0a98`.
Full correctness, fresh confirmations, statistical PERF comparison, and physical
memory qualification remain open. Do not promote a performance baseline from
this single replay.

The subsequent adjacent selector change prefers an ordinary healthy offer in a
permitted window over a first window containing only warned or quarantined
providers. Its two actual-provider causal controls reproduced the old first-choice
error with empty service-failure history; 120 patched normal/race checks passed.
Pins, established ownership, UDP, and the all-benched availability fallback stay
covered. This later coalescer change was not present in the first full100 result
above; confirmation on the final integrated source remains required. The
mandatory functional100 receipt guard separately passed 24 patched normal/race
executions against four causal OLD failures. Explicit provider4 research remains
manual, separate from mandatory functional recovery.

## 2026-10-24: fresh canonical SDK 100-page functional replay

The C13b canonical run of
`TestFullTunH1Sdk32ServiceRefusalRecovery100Pages` passed once with
100 HTML requests, 400 asset attempts and 99 next-navigation overlaps.
It used `next-navigation-image-v2`, the public SDK/native TUN/H1/provider
path, CPU8, the unchanged 300-second workload budget and five-second
navigation target. There was no instrumentation overlay or workload retry.

| All-page first-byte measurement | Result |
| --- | ---: |
| Median | 0.858013459 s |
| p95, nearest rank | 2.065520583 s |
| Maximum | 3.780069917 s |
| Pages at or above five seconds | 0 / 100 |

The original 12,288-byte CSS stream retained its TCP tuple and public owner
through the next HTML handoff. All checked native packet entries and
origin connection owners completed; the late constructor joined. The
checked SDK post-join memory ownership fields were zero. Unpublished
offers/global publication quiescence remain unknown. Packet-entry counters
are not API-call counts and do not prove individual SYN acceptance.

The v2 fixture verifies that an original image is unfinished at each
next-navigation handoff, then releases its final 1 KiB before fetching
the next HTML. This guarantees overlap, not native-queue backlog. Historical
v1 runs retain their original weaker overlap meaning. This single pass is
a functional recovery result, not a statistically qualified improvement,
a Fast.com measurement, or proof of the cause of the reported device pauses.

The fixture cohosts SDK, providers and origins. Its aggregate Go memory
cannot qualify isolated 32 MiB iOS-profile or 64 MiB Android-profile usage.
Those physical-device startup/active/quiet/teardown checks remain pending;
actual iOS extension total physical footprint is also unqualified.

Source: Connect `15bdd250f22976923e4b7f6bd7366eab859fe584`,
SDK `3bd24c6865ff92dc61bf764465cf60775134a304`,
Server `936c63f651444797775a7b44a76a6088b375c546`,
Tests `4ba46b48426831831173103811152a67dae80504`; whole-source fingerprint
`6d1a60145c675d44176cdf5969f2d1a3714025517ba7d6860fcb470f74b7f464`.
Actual root RUN 17:03:53.881705 UTC; PASS 17:07:19.598488 UTC.
One root RUN/PASS, zero FAIL/SKIP; root elapsed 205.72 s.

Evidence owner: `h1-disjoint-controls.PREP.7tk80_6o/c13b-sdk100-once.PREP._rdr6c11`
under the retained current112 evidence root.
`C13B-SDK100-MEASUREMENTS.json` SHA-256:
`aac7ff9e7342289adf2ab741253920287d5e057af543332e7cf48abdef1a9622`.
Raw Go JSON SHA-256:
`fa9d7608905f6e5419386db698a83fa4dc32b822b88857d70b073d8454ca7124`.
Final all-joined custody receipt SHA-256:
`b2e5e5c1a4864f242f439e6458913421e403a277c5a704c224548e2235da8644`.
Full correctness and complete simulator/benchmark/device qualification
remain open; no performance baseline is promoted.
