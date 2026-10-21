# Rapid-browsing packet-flow stall: meta review

Date: 2026-10-21. Connect revision examined:
`bc1bfbbf2275832f5120de3ad6b046461575e0df`.
Line references below describe that baseline unless otherwise noted. This review
also considers the root's current, unqualified `ApplicationPacing`
candidate; it is not the source used by the earlier full100 experiments.

**New result: the pacing-only bypass completes the unchanged 100-page workload.**
The root-owned run records 100 completed owners, 100 valid HTML rows, no
classified slow navigation, maximum HTML first byte 3.159 s and workload elapsed
171.881 s. All 400 asset workers finish, with no asset retry or invalid response
status, zero retained pool roots/bytes, and successful fixture cleanup. This is
strong positive evidence that the pacing subsystem contributes to the failing
workload, compared with the earlier matched-source pacing-enabled arm's 72
owners/75 HTML rows at the 300 s deadline. It is one intervention, not fresh
paired statistical confirmation or a qualified production fix.

**The fresh pacing-enabled confirmation fails, and disabling controlled drains
alone is insufficient.** The new pacing-enabled arm reaches 46 owners/49 HTML
rows with maximum first byte 63.078 s. The arm that disables only creation of
controlled drains reaches 87 owners/90 HTML rows with maximum first byte
13.322 s, but also fails the unchanged deadline. The full bypass still has the
only passing result among these three arms. These single runs strengthen the
subsystem attribution without proving a particular service-rate, meter or
feedback defect, or qualifying the no-drain arm's apparent improvement.

Both independent reviews correctly prioritize sender-side delay and a controlled
Transfer-pacing intervention. Response 1 adds useful item-creation timestamps
and real-Client model experiments, while more carefully distinguishing pure TCP
ACKs from source-retryable payload. Neither establishes its complete causal
account, and both contain consequential source or inference errors. In
particular, pacing meters bytes rather
than charging every small Pack as a maximum-sized message; Transfer ACKs do not
pass through the opposite Pack pacer; and the delayed-inner-ACK witness does not
give the provider enough information to distinguish delayed delivery feedback
from genuine tail loss. These corrections change which experiment and which
regression assertions are appropriate. They do not exonerate pacing or provider
replay amplification.

**The user has now chosen to disable extra application pacing by default for all
transports.** The root's candidate adds default-false `ApplicationPacing` and
gates the existing pacing wait loop, retaining route-policy generation checks
and FIFO-repair eligibility. Qualifying that actual candidate is now the first
priority; diagnosing the controller's exact internal defect is useful follow-up,
not a prerequisite to an explicitly chosen default-off policy. In particular,
the old private bypass pass does not qualify this new, narrower implementation.

The successful earlier arm uses the latest
initial-admission/replay-clock overlay with only the existing private
`disableWindowPacingForTest` switch enabled on both endpoint send settings.
Delivery-sized admission, receive-window advertisements, explicit budgets,
strict retained accounting, traffic and deadlines remain configured identically.
A whole-policy `SetWindowSizing(WindowSizingConstant)` comparison is now lower
priority because it changes additional behavior.

## Scope and limits of this review

I read `CODESTYLE.md`, `PACKETFLOW-STALL.md`,
`PACKETFLOW-STALL-RESPONSE1.md` and `PACKETFLOW-STALL-RESPONSE2.md` completely,
inspected the relevant canonical implementation, the latest return-admission
afterimages, both reviewers' new test sources, response 1's recorded model logs,
and the cited receipts and selected trace records. For the new result I
also compared both overlay maps and the complete changed-source diff, read the
root receipt and checked the raw test's successful terminal events. For the
fresh pacing-enabled and no-drain results, I checked their receipts, raw terminal
failures and the exact drain-creation source diff. I did not
run a build, test, benchmark or workload. Existing test results below belong to
their recorded executions; source inspection is not a new successful run.

Response 1 was absent during the initial review and arrived later at 877 lines.
It is now included, with its claims checked independently rather than inferred
from agreement between reviewers. Its SHA-256 at examination was
`1066985b82482555302b574636c8e329e513a9077296d83fea0c393ed494467e`.

Canonical HEAD alone is not the failing experiment. The instrumented trace used
the four earlier private fixes. The uninstrumented initial100 arm additionally
used the two provider return-admission fixes through
`/Volumes/data/urnetwork-return-initial-admission100-prep.fWJmE5/INITIAL100.overlay.json`.
The passing pacing-only arm preserves that closure except for the private
pacing switch described below.
The reviews' claims about the older trace must not silently become observations
of that later arm. No source or document other than this meta review was edited
by this reviewing agent.

## What is established

| Claim | Assessment |
| --- | --- |
| Shaped direct/native completes 100 pages in 163.43 s; pacing-enabled initial100 completes 72 owners and 75 HTML rows before the unchanged deadline | Recorded results, not a statistically qualified performance comparison. The null removes several layers and changes the origin TCP implementation. |
| The private pacing-only bypass completes all 100 pages in 171.881 s of workload time | Positive intervention with matched source/settings except for the pacing bypass. It is one passing run against an earlier failed baseline, not a fresh paired distribution. |
| A fresh pacing-enabled arm fails at 46 owners/49 HTML rows | Confirms failure after the bypass pass. Large variation from the previous 72/75 arm argues for preserving every outcome and repeating pairs. |
| Disabling only controlled-drain creation fails at 87 owners/90 HTML rows | Drain removal alone is insufficient. It does not exonerate drains or establish a particular rate defect. |
| Connection 6 is reused for page 3 after page 2 asset 1; its timeout counter rises from 1 to 3 | Positive identity and counter evidence. It does not identify which copy was refused or where an ACK waited. |
| Initial-item counter growth is sparse during the selected page-3 interval | Confirmed deltas, but these counters increment before pacing/write, not at physical departure. They do not measure ready backlog or exact wire service. |
| Some old inner ACKs reach the Transfer-write observation several seconds after their acknowledged response was consumed | Strong sequence/HTTP evidence for substantial pre-observation delay, with the qualifications below. The exact admission boundary was not traced. |
| Selected message IDs precede their route-attempt observation by 143–426 ms | Independently decoded from response 1's cited records. This narrows part of the delay to after item creation, not exclusively to pacing or exactly from dequeue. |
| Provider emits already-consumed response sequence ranges again | Positive evidence of redundant inner traffic. Its aggregate byte share, trigger and effect on the deadline are not measured. |
| A recovery callback's admission wait spent the following retry interval, and initial admission could overlap a replay | Deterministically reproduced and corrected in private candidates. Their combination still fails full100 with pacing enabled. |
| An empty-retention provider can park at zero receive window without a persist owner | Deterministically reproduced; private candidate and actual TUN-peer controls have passing recorded results. No zero-window episode has been established in full100. |
| Pacing contributes to the full100 failure | Strongly supported by the new passing intervention. The responsible rate, meter, drain or feedback transition remains unidentified; repeatability and a production correction are not qualified. |

These findings justify further isolation. They do not justify the categorical
opening of response 2 that the stall *is* a collapse caused by its proposed three
mechanisms, or its numerical confidence assignments as measured probabilities.

## The new pacing-only result

The receipt is
`/Volumes/data/urnetwork-browsing-sizing-ab.SuqKJX/PACING-OFF-OUTCOME.v1.json`
(SHA-256
`1853e139bfcfd0c68366cc34d06822b86462877a3dd5740234daa4d7468d6feb`).
Both the baseline and passing overlay have 17 replacement mappings. Their only
different mapping is `connect/transfer.go`; the replacement differs from the
baseline afterimage by a comment and
`settings.disableWindowPacingForTest = true` immediately after
`settings.ApplyWindowSizing()` in the default send settings constructor.
The full100 fixture still attaches the same two explicit 2 MiB retained budgets.
No workload deadline or queue was enlarged.

The raw Go test reports the exact full100 root passing in 177.97 s, including
setup and cleanup; 171.881 s is the reported workload interval. The direct
control's 163.43 s is a test-elapsed result, so those two numbers must not be
presented as identical timing boundaries. The harness receipt records identical
source state before and after, fixture cleanup exit 0, no retained fixture and
bridge exit 0.

The bypass removes pacing reservation/wait and its physical-write enrollment,
so it can also change later controller feedback and effective learned admission
windows. Preserving sizing configuration does not freeze those dynamic values.
It also makes `routePolicyEnabled` false (`transfer.go:12887`), selecting the
writer's compatibility path instead of its acquired-generation policy path.
The result localizes the problem to behavior affected by the pacing subsystem;
it does not distinguish an underestimated service rate, a meter/FIFO wait, a
controlled drain or feedback coupling between them.

The arm retains the existing partial-ACK return-replay rule and does not add the
new zero-window persist candidate. Those changes were therefore not required
for this successful run. They can still be adjacent correctness or amplification
issues. The result does not establish their frequency in the failing run, and
it does not validate response 2's incorrect packet-quantization explanation.

## Fresh confirmation and the no-drain result

All three arms below use the same workload and 300 s application deadline.
These are individual diagnostic results, not comparable outcome distributions.

| Arm | Completed owners / valid HTML | Slow navigations | Maximum HTML first byte | Outcome |
| --- | ---: | ---: | ---: | --- |
| Full pacing bypass | 100 / 100 | 0 | 3.159 s | Pass; workload 171.881 s |
| Fresh pacing enabled | 46 / 49 | 10 | 63.078 s | Deadline at page 49 |
| Pacing enabled, new controlled drains disabled | 87 / 90 | 12 | 13.322 s | Deadline at page 90 |

The pacing-enabled receipt is `PACING-ON-OUTCOME.v1.json` under
`/Volumes/data/urnetwork-browsing-sizing-ab.SuqKJX/`, SHA-256
`7e50a0082c37961ba873997cc68d0567413a0c1b5f366c9d8ad1e248bb643283`.
The no-drain receipt is `NO-DRAIN-OUTCOME.v1.json` in that directory, SHA-256
`31e9d3a1406586d889c7dfb2e3180c873ac38047b2f6d9980077eca87ad2ee8f`.
Their exact full100 Go roots fail in 302.57 s and 302.62 s respectively. The
reported workload intervals of approximately 296.9 s are different timing
boundaries, not changed application deadlines.

Both receipts report identical source state before/after, zero pool roots and
bytes, joined asset workers and successful outer fixture cleanup. The
pacing-enabled run also reports a secondary full-TUN retirement-identity context
deadline during teardown; preserve that failure rather than describing every
inner cleanup operation as successful. Its 196 asset workers joined. In the
no-drain run, all 360 client and 359 server asset workers joined; there are no
asset retries or invalid statuses in either arm.

The no-drain overlay adds exactly one replacement to initial100: a private copy
of `transfer_window_pacing.go` whose new-drain creation condition at line 1442 is
gated with `false`. Reservation serialization, FIFO ordering, burst credit,
ordinary service learning and route-policy dispatch remain enabled. Fresh
services cannot create a positive `drainUntil` under that change. Thus an active
controlled pause is not necessary for this residual failing run. The result
does not exclude effects from other drain/probe state: natural empty-flight
probes, source-idle handling and feedback epochs remain in the controller.

The 63 s first-byte maximum resembles existing long timeout/drain scales, but
there is no measured 60 s drain receipt for that navigation. The shorter maximum
and larger page count without forced drains are suggestive, not measured proof
of that causal decomposition. The remaining investigation must inspect the
actual owner and wait state rather than rename the residual failure a rate bug.

## Response 1: useful additional evidence and its limits

Response 1's strongest addition is the message-ID timing analysis. `NewId`
encodes creation milliseconds in its first 48 bits (`id.go:42`). Independently
decoding the selected trace reproduces approximately 349, 349, 325, 317 and
302 ms from creation to the recorded attempt for device items 93–97; item 101
has 426 ms, item 103 has 248 ms, and SYN items 120/123 each have 143 ms. Earlier
items 79–81 have only about 9, 22 and 1 ms. These are useful positive observations,
not merely an argument from the lack of another measured bottleneck.

However, the ID is allocated in `sendWithSetContractDisposition`
(`transfer.go:10797`), after dequeue/grouping and contract handling, not exactly
at dequeue. Memory-ownership commitment, framing, disposition callbacks,
selected recovery work and write setup still occur before the route-attempt
observation, as well as pacing. Locks and scheduling can also delay execution.
The timestamp has millisecond resolution and a process-local monotonic clamp
against backward wall-clock steps. It supports substantial post-creation delay;
it does not prove that pacing is the only possible blocking operation in that
interval. Nor is the observation a completed physical-wire receipt: it is
emitted before the selected route attempt. The newer successful pacing
intervention is stronger causal evidence than this exclusivity argument.

The real-Client tests R5–R9 are useful characterization. Their recorded normal,
race and canonical executions show that delivery-sized send policy can slow
burst drainage under jitter/feedback stalls, and can increase zero-wait refusals
in the tested scheduling model. The model uses actual Transfer clients and
bounded ownership, rather than a standalone imitation of the rate formula.
It also candidly does not reproduce the full multi-second browser failure.

Those tests are not yet the asserted regression contract proposed later in the
review. They log latency and refusal counts without asserting a latency bound,
a refusal bound, or that every accepted message was delivered before shutdown.
The 300 KB reverse offers are completion-coupled: the sender submits the burst
with blocking admission and then sleeps, so the arms do not necessarily receive
the same wall-clock offered-load schedule. The relays model serialization,
jitter and stalls, but not a real H1 socket, native TCP, Multi or NAT. Only
`SendBufferSettings.WindowSizing` is changed and reapplied; the receive settings
remain at their defaults in both arms. Thus these are send-policy comparisons,
not pure pacing-loop comparisons and not the broader process-wide constant
switch discussed in response 2.

In particular, 48 immediate zero-wait offers against 32 Pack slots can validly
refuse the excess while no worker is scheduled to release capacity. The clean
cell's 176 delivery-policy refusals versus 0–7 constant-policy refusals shows
different release/interleaving behavior; it does not itself prove a correctness
violation. A deterministic test needs a specified progress opportunity and
admission contract. Requiring no more refusals than the constant arm, or a p50
within 15% of that arm, is a proposed performance target, not an existing
protocol invariant established by those logs.

One source claim is incorrect: a `resend-capacity` refusal does **not** uniquely
mean aggregate retained-memory exhaustion. Even with strict retained accounting,
`transferQueue.CanAddWithQueueByteCountAndCredit` first checks
`transferQueueWindowAdmits` against the per-sequence protocol-byte window
(`transfer_queue.go:253`), then checks the retained budget. SendSequence also
combines reliable-admission and prepared-flight gates. The strict lifetime mode
removes an empty-queue exception; it does not remove that window test. Similarly,
occupied Pack slots identify the blocked admission boundary, not uniquely a
pacing cause. Record the actual gate and its operands before carving a reserve.

Several other conclusions need narrower wording:

- The selected 36 s interval is not demonstrated to be a quiet, loss-free outer
  path: the review's own table has link-loss growth and one new retransmission.
  No new outer timeout does not rule out carrier or feedback effects.
- Item 101 is the first **retained observed** copy in an incomplete ring. The
  precise earlier refusal/retry timeline cannot be recovered from two timeout
  increments, and the review's two example timelines are not exhaustive.
- A stale ACK field on an already-constructed return packet does not reveal the
  provider's current receive state at dispatch. This affects both reviews.
- Differences between distinct source arms do not establish a ±15-page
  same-arm noise bound. The fresh 46/49 and older 72/75 failures demonstrate why
  actual repeated pairs are needed.

Response 1 correctly separates lost pure-ACK custody from payload retry
ownership, and its initial-window observation refines the earlier 1 MiB-buffer
counterargument below. Its proposed fixes are still hypotheses: short-RTT
heuristics do not resolve the delayed-ACK/tail-loss ambiguity, an 8 s provider
replay cap is not validated merely because native TCP uses that cap, and a
bounded latest-ACK queue must preserve relevant duplicate-ACK and window-update
semantics. Keeping an advancing ACK plus two subsequent duplicates need not
produce a three-duplicate-ACK threshold. A control-priority queue also cannot
promise immediate service while the sole worker remains blocked on an already
selected paced data item. These require explicit ownership, cancellation,
ordering and real-loss controls, not just favorable characterization numbers.

## Corrections to the pacing account

Pacing is now the leading target. The strict lane-0 fixture explicitly attaches a
2 MiB resend budget to each endpoint after constructing defaults, and enables
retained-byte accounting. The delivery policy sets the scale and target; a late
attached budget can activate it. The H1 write path checks that scale and enters
`paceWrite` for ACK-requiring Packs (`transfer.go:12887`, `12906`). Inner TCP ACKs,
SYNs, requests and provider return packets are such data-plane Packs.

However, response 2's specific quantization explanation is incorrect as a
general statement. In `transfer_window_pacing.go:1322`, the minimum of one maximum
message applies to the **burst byte allowance** and its associated time span.
The actual reservation advances `self.next` by
`windowPacingSerializationTime(actualByteCount, rate)` at line 1341.
`windowPacingBurst.reserve` accumulates actual bytes (line 186), and
`windowPacingBurstMeter.wait` consumes actual bytes and refills at a byte rate
(line 249). A 1,200-byte allowance can fit several 151-byte Packs; it does not
automatically charge each of them 1,200 bytes or release at most one per the
1,200-byte interval. A service whose largest outstanding messages are themselves
151 bytes can have a one-small-message quantum, but its time cost then tracks
those 151 bytes. A retained larger message can affect burst boundaries and
backlog, so mixed-size tests remain useful.

The proposed unit test should therefore measure actual dispatch times and byte
credit for a mixed sequence, including prior debt and rate changes. An assertion
that twenty small Packs must reproduce maximum-message spacing merely assumes
the claimed bug. The existing meter and dispatch tests are relevant controls;
reading their assertions is not evidence that a new mixed workload passes.

Other source qualifications matter:

- A pacing service is shared by `sendSequenceId.logicalLaneBase()` inside a
  SendBuffer (`transfer.go:6051`), not indiscriminately by every destination and
  session of one client. The single-provider lane-0 fixture can still concentrate
  many inner flows in one service.
- `windowPacingRate` already has discovery, held-rate and service-establishment
  protections against interpreting tiny application-limited delivery as path
  capacity (`transfer_window_pacing.go:290`). A failure hypothesis must show how
  the run entered a state where those protections no longer apply. They are
  neither a proof of safety nor absent code.
- The controlled-drain code can impose a material delay, with the production
  maximum taken from `AckTimeout`. Its triggers also depend on pending writes,
  drain progress, RTT evidence and `canDrainWithLock`. Residence alone does not
  prove a drain occurred. Capture the actual drain state before proposing a
  different drain bound.

The review also conflates two ACK layers in its proposed feedback cycle.
`ReceiveSequence` builds **Transfer ACKs** in `writeAck` and writes them through
`ackMultiRouteWriter` (`transfer.go:15437`), with an H1 priority attempt and an
ordinary writer fallback (`15540`). They do not become ACK-requiring Packs in
the opposite SendSequence and do not directly take that sequence's pacer. Shared
carrier queues and receive work can still delay them. **Inner TCP ACKs**, by
contrast, are ordinary tunnel data and can queue behind the Pack pacer. A coupled
inner-TCP feedback problem remains plausible; the asserted direct cycle of two
paced Transfer-ACK lanes is not the implemented path.

Similarly, an inner provider replay is submitted as a **new Transfer item**.
The `alreadyCounted` argument passed into pacing is based on that item's previous
pacing/delivery state (`transfer.go:12942`). A fresh inner replay is not directly
marked as a Transfer retransmission and thus is not automatically the `resend`
congestion evidence in `reserveWithLock`. Response 2's F2 instruction to stop
counting those inner replays as such evidence targets a transition that has not
been demonstrated. Their extra traffic can still create genuine queue evidence.

Finally, the constant-policy switch is broader than disabling pacing.
`SendBufferSettings.ApplyWindowSizing` changes scale, target and budget defaults;
`ReceiveBufferSettings.ApplyWindowSizing` changes advertisement behavior and,
when a process budget exists, receive-budget/limit defaults
(`transfer.go:795`, `1019`). The explicit 2 MiB fixture resend budgets are attached
after defaults and remain a bound, but that does not make receive behavior or
effective send admission identical. A constant-policy pass implicates this
policy family. It does not prove byte quantization, controlled drains, or pacing
alone caused the failure.

## What the selected trace does and does not show

The counter arithmetic used by both reviews is substantially correct, but the
measurement boundary needs correction. From the bounded endpoint samples,
crossing to request-written is about 4.23 s: device `InitialWriteCount` rises
129→155, provider count 156→178, and provider `InitialMessageByteCount`
333,054→388,252. Written to first-byte is about 3.55 s: device count 155→185,
provider count 178→205, and provider message bytes 388,252→465,703.

These counters increment while materializing the initial send item, **before**
`writeMaybeWrappedBytes`, pacing and the selected route attempt
(`transfer.go:11084`). `InitialMessageByteCount` uses
`MessageByteCount(sendFrames)`, not framed physical-wire bytes. Thus the observed
6–8.5 initial items/s is a sender-work cadence, not exact departure throughput.
A paced serial worker can cause that cadence, but the counters alone do not
identify its wait, prove sustained ready backlog or establish completed carrier
writes. Packs can contain several inner frames. Exact packet-attempt records
provide a stronger boundary than these aggregate counts, though even they are
not completed wire receipts.

The carrier is not proved healthy in the stronger sense used by the review.
The captured device H1 socket has no new timeout during these cuts and one new
fast retransmission. At first byte its `SndCwnd` is **5**, versus 15 and 19 at the
earlier cuts, and its congestion state is 3. Thus the review's summary of a
15–19-packet congestion window throughout omits the last sample. The pressure
report also has 38 cumulative link queue drops at that boundary. Neither fact
proves outer TCP caused the multi-second stall; conversely, one socket's sampled
RTT and timeout count do not exclude relay queues, receiver-window effects,
opposite-direction TCP state or earlier carrier-induced controller state.

For connection 6, device items 93–95 carry cumulative ACKs strictly below the end
of response 1, which the application had consumed by approximately epoch-offset
423.101 s. Assuming the retained flow join and sequence accounting are correct,
those ACKs were generated before that completion and reach the recorded packet
observation at 428.099, 428.448 and 428.773 s. This is strong evidence of at least
about five seconds of delay before that observation. It is **not** a direct
admission-to-physical-write measurement: generation, native offer, successful
Transfer admission and the physical carrier receipt were not all joined.
Items 96 and 97 ACK the end of response 1, so they could also be later duplicate
ACKs; their sequence fields alone do not bound their creation by body completion.

Provider items 112–114 begin at the cumulative ACK values of those preceding
device items and carry response-1 data after that response has been consumed.
Together with the recovery source this is persuasive evidence of redundant
return recovery. Exact timer-versus-partial-ACK trigger attribution remains an
inference without a recovery event. Item 197 likewise repeats response-2 ranges
after its body completion. One detail in response 2 is wrong: its three recorded
starts differ by **3,144** bytes, not 1,048, so they are not three consecutive
1,048-byte ranges.

The stale ACK field on item 197 does **not** prove that the provider had still
not processed the page-3 request at the later observation time. The packet could
have been constructed earlier and queued before its Transfer write. In fact,
queued prebuilt packets are central to the review's own hypothesis. It is
therefore unjustified to decompose the 3.55 s HTML reply into at least 3.1 s of
forward request transit and about 0.4 s of origin/return solely from this field.
Record packet construction and provider request arrival to make that inference.

The asset-close/GotConn correlation and HTTP reuse rules support waiting for a
busy connection to become reusable. They do not establish that the logged
`Body.Close` call caused the handoff. Nor do SYN-write→SYN-ACK-write intervals
locate the entire gap in the provider queue: SYN transit, provider admission,
origin dial, return construction and return admission are separate boundaries.
A loopback destination reduces the prior expectation of a slow dial; an absent
dial error does not measure successful-dial latency. The adoption log is evidence
of the adoption branch, not by itself proof that the 3 s inference branch caused
the preceding unbind.

The two extra inner TCP timeouts on connection 6 establish lack of timely inner
ACK progress. They do not make it more likely than not, on the supplied evidence,
that the missing request copy was one of the native refusals. An admitted request
held before dispatch or an ACK delayed on return can produce the same counters.
The global refusal total is positive evidence; its missing flow attribution is
still missing. The original request correctly preserves that uncertainty.

## The delayed-ACK witness is an ambiguity witness, not a fix oracle

Response 2 correctly reads `acknowledgeReturnWithLock`: an advancing ACK below
the active recovery frontier makes the next retained range eligible for replay.
This behavior survives in the latest initial-admission afterimage. A delayed ACK
train can therefore cause redundant traffic after a timeout. RTT-aware timing
and explicit spurious-recovery evidence are legitimate subjects for investigation.

But the new test
`TestResponse2ReturnDelayedInOrderAcksDoNotReplayDeliveredData` does not establish
that replaying the next range is a protocol error. It tells the human reviewer
that the receiver already has the whole flight. It gives the actual provider
only a timeout followed by ACKs 105, 109, 113 and 117. An ACK for 105 proves receipt
only through 104, not that bytes 105–116 have also arrived. The same provider
history occurs when the entire tail was lost and each ACK advances only because
the preceding replay repaired its first missing segment.

That second case is already an explicit repository requirement:
`TestTcpReturnTailBurstRecoveryNeedsOnlyInitialTimer`
(`ip_tcp_return_burst_recovery_test.go:123`) requires a single timeout to start
ACK-paced recovery of the whole tail. The review's no-timestamp proposal to keep
partial-ACK repair only for episodes entered by duplicate ACKs would abandon
this timer-triggered repair and wait for further timers. Calling for both that
change and the existing tail test to keep passing does not resolve the conflict.

Response 1's R2 witness has the same limitation. It supplies a delayed advancing
ACK train and calls subsequent replays erroneous because the human model says
the original flight was delivered; the provider has no corresponding receipt
for the unacknowledged tail. Its R3 genuine-tail-loss control is valuable, but
does not turn receiver knowledge in R2 into usable sender evidence. R4's long
silent-tail schedule is characterization of the current timer, not an asserted
requirement that its particular times are wrong. Neither witness validates
suppressing timer-entered partial-ACK recovery without a usable discriminator.

A useful replacement experiment pairs two receivers with the same provider
state and ACK sequence: delayed-feedback delivery and genuine tail loss. First
show their indistinguishability without additional feedback. Then introduce only
the proposed usable discriminator, such as validated timestamp evidence, and
show both suppressed spurious work and prompt genuine-loss repair. Timestamps
require the usual negotiated-option, serial-number-wrap and attribution checks;
the witness as written contains no such discriminator. A timer based on measured
inner RTT must also define its start at admission/dispatch and exclude ambiguous
retransmitted samples. Adding a timestamp to a descriptor requires accounting
for its measured size, not assuming metadata cost is unchanged.

The two existing admission-clock fixes have a firmer causal foundation. Their
barriers force duplicate work while the *same local admission* is still pending,
or force that wait to consume a subsequent interval. The recorded before/after
tests distinguish those boundaries directly. Their unsuccessful full100 replay
also limits their claim honestly: they fix demonstrated local behavior without
yet fixing the workload. Describing them as second order before measuring their
contribution is speculation.

## Refusal, controls and zero-window recovery

The newer actual HTTP/native tests narrow the refused-payload hypothesis:
refused payload remains eligible, a real same-range TCP retry is admitted and
Transfer-ACKed, and HTTP completes on the same connection. Accepted-but-held
payload retains its original owner and duplicate coverage. This is a positive
result for those exact paths, not a proof of bounded latency everywhere.

Pure inner TCP ACKs require separate treatment. They are not source data with a
TCP retransmission-list owner. Native ready-only refusal can lose a window
update before Transfer owns it. Thus response 2's blanket language that native
refusal leaves retry custody with source TCP is too broad. An eventual duplicate
or later data exchange often supplies another ACK; an idle zero-window peer
cannot rely on that event occurring.

The provider persist-gap reproduction addresses exactly such a state: the actual
reader is waiting at zero window, ACK equals next sequence, and return retention
is empty. The equivalent reopening ACK releases it. Recorded private candidate
tests and the separate actual TUN-peer probe validate concrete recovery behavior.
The workload's configured receive buffer is 1 MiB and ordinary assets are
smaller and continuously drained, but buffer capacity must not be confused with
every advertised TCP window. Response 1 correctly identifies gVisor's initial
handshake window, capped at approximately 20 MSS (`endpoint.go:1283`). That cap
does not persist as a fixed flow window: subsequent ACK/data sends use
`receiver.getSendParams` and `endpoint.selectWindowLocked`, which can grow the
right edge using available receive-buffer space as the application consumes
data. Neither the 1 MiB capacity nor the initial 20 MSS advertisement proves the
steady-state window in the selected asset. Actual advertisements are needed.
No zero-window episode has been observed in the failing workload. Missing
persist should remain a demonstrated adjacent correctness issue until that
state is captured in full100.

Response 2's control-reserve analogy also crosses layers. The prepaid partition
at `ip.go:10086` is provider **ingress into local NAT**, conditional on a memory
budget, after a data group is refused. It is not an existing provider-return
Transfer priority class or a reserved device Pack slot. A control reservation
can be useful, but it must identify which gate actually refused the packet:
resend capacity, Pack admission, native preparation or downstream NAT admission.
Carving Pack slots cannot repair a closed resend gate. Size alone also does not
identify a TCP control; a small request can carry payload. Any priority proposal
needs explicit classification, bounded service fairness and an unchanged total
capacity. It should follow attribution, not precede it.

## Ranked experiments and their interpretation

1. **Qualify the user-selected default-off implementation.** The current root
   candidate introduces `SendBufferSettings.ApplicationPacing = false` and adds
   that positive gate only to the existing pacing loop. Inspection confirms
   `routePolicyEnabled` does not depend on the new field, unlike the older
   private bypass flag. Retain route-generation and FIFO-repair eligibility,
   delivery-sized admission, receive advertisements, byte budgets and ACK
   ownership. First require a deterministic real-worker default-policy test
   with a future pacing deadline: old-default red, new-default green, normal
   and race; retain explicit opt-in controls for controller tests. Separately
   assert that resizing and ownership remain active with pacing off. Such a
   test validates the newly chosen policy, not a specific estimator defect.
   Then run the unchanged, source-pinned full100 using this actual candidate
   composed with the same initial-admission fixes, **not** the old bypass
   overlay. Repeat the candidate/baseline comparison, alternate later pair
   order and preserve all outcomes before claiming performance qualification.
   Check bulk-transfer behavior and the outstanding memory qualification.
   A pass with this narrower gate also addresses the old route-policy-path
   confound; it still does not hold learned controller feedback constant.

2. **If further causal isolation is needed, capture the residual wait with
   controlled-drain creation disabled.** The
   root's prepared compact capture under
   `/Volumes/data/urnetwork-browsing-pacing-capture.vEpiDS` is a useful diagnostic
   follow-up; root reports its normal/race controls passed, but no full capture
   workload has run. Combine it with the no-drain source closure, keeping
   observer costs separate from uninstrumented performance claims. Record
   pacing estimate/meter/drain state and admission depth for **both endpoints**,
   plus every refused packet's flow, flags, ACK/window and actual refusal gate.
   The public `RemoteUserNatMultiClient.PacingDiagnosticSnapshot` covers its
   clients; it does not by itself inventory the standalone provider client.
   Join a selected packet's native offer, admitted owner, first dispatch,
   Transfer receipt, NAT application and inner ACK. Use a bounded retained event
   record with explicit overflow counts. This determines whether an accepted
   item is pending behind pacing, a window gate, physical transport or a peer
   action. Three navigation snapshots can miss a short drain; they are useful
   boundaries, not complete time attribution. Separate time spent waiting for a
   reservation deadline, FIFO turn, meter credit, controlled drain and carrier
   admission. Copy the selected estimate and byte count at reservation time as
   well as at release; a later snapshot cannot reconstruct which rate priced an
   old reservation. Existing cold snapshots deliberately do not read the
   worker-owned cached pacer rate. A low current estimate does not prove that an
   already-waiting reservation was priced at that rate, and a high current
   estimate does not erase that reservation's earlier low-rate deadline.

3. **Condition a controller intervention on the captured state.** If ready backlog
   coincides with a low release rate, a narrow private arm can retain the no-drain
   overlay, route-policy checks, pacing enrollment, FIFO, meter and service
   observation, but set only `windowPacer.rate` at the normal refresh point
   (`transfer.go:12932`) to the existing positive `estimate.PacingProbeByteRate`.
   Keep `estimateRate` and the estimator intact; explicitly record the override,
   since ordinary estimate snapshots would otherwise report the learned rate
   rather than the worker's actual rate. This tests the learned release-rate
   choice while preserving the surrounding path. It is a diagnostic override,
   not a proposed shipped rate floor. A pass would implicate the learned pace
   or its interaction with byte debt; it would not prove which sample was wrong.
   If captured rates are already ample but reservation or meter debt is long,
   reproduce that exact debt transition instead. Do not discard debt or grant
   uncharged credit merely to obtain a pass.

4. **Controller witness derived from the captured state.** Recreate the actual
   settings, sizes, ready backlog, admission state, service samples and drain
   transition under virtual time. Measure actual release and remaining byte
   ownership. Include inner controls, mixed frame sizes, a healthy sibling and
   genuine recovery. The assertion should be the violated progress requirement
   found in the trace, not a desired rate inserted into the test.

5. **Preserve adjacent controls without bundling causes.** Compare completion,
   latency, refusals, retransmissions and ownership drain across any further
   variants. The fresh enabled failure strengthens attribution but does not
   establish a repeatable bypass pass distribution. A whole constant-policy arm
   remains a useful lower-priority control, but the pacing-only result has already
   narrowed the search without changing receive advertisement configuration.
   Record all effective settings if that broader arm is run, and do not interpret
   a pass as proof of one-Pack-per-interval quantization.
   Pair genuine tail loss with
   delayed feedback as described above before changing recovery semantics.
   Separately capture zero-window closure and a lost reopening update in full100
   before attributing that workload to missing persist. These tests address
   different owners and should not be bundled into a single claimed cause.

The source audit identifies two concrete risks to test, not new proven bugs.
`observeQueueWithLock` (`transfer_window_pacing.go:700`) ends discovery on excess
residence and can clear the held pace. `windowPacingRate` then uses established
service, including 0.95 of service when backlogged. Its safeguards require
checking which byte/time interval qualified, which queue evidence removed the
hold, and whether the physical flight exceeded the corresponding allowance.
In this jittered, intermittent workload, confusing delayed feedback or source
idle with lower service would be consequential, but the code already contains
coverage, held-rate and recovery protections; their failure cannot be assumed.

Separately, reservation deadlines retain their original serialization cost
while the meter refills and estimates change. The worker refreshes its rate
only before a new reservation; an old wait is not automatically repriced by a
newer estimate (`transfer.go:12925`, `transfer_window_pacing.go:367`). This is
intentional debt preservation in the existing tests. A multi-second stale
reservation is a useful state to inspect, not by itself proof that debt should
be erased. Require an exact event sequence showing why a deadline or retained
rate no longer represents valid service before altering that behavior.

One uninstrumented full100 pass has now been obtained with the old private
bypass. The immediate completion gates are the actual default-off candidate's
deterministic policy controls, unchanged full100 outcome and repeatability,
plus bulk/memory qualification. An internal controller-defect witness remains
necessary for claiming a specific controller repair, but not for honestly
describing the user's chosen default-off policy. Neither a higher deadline nor
a larger queue is part of these experiments.

## Evidence checked

- Request and both reviews: `PACKETFLOW-STALL.md`,
  `PACKETFLOW-STALL-RESPONSE1.md` and `PACKETFLOW-STALL-RESPONSE2.md`.
- Passing pacing-only full100:
  `/Volumes/data/urnetwork-browsing-sizing-ab.SuqKJX/PACING-OFF-OUTCOME.v1.json`,
  `PACING-OFF.overlay.json`, `transfer_pacing_disabled.go` and
  `pacing-off-owned-v1/commands/lowbar-01-b498602db92cb84f/stdout.log`.
  The receipt preserves the recipe and raw-log hashes.
- Fresh pacing-enabled and no-drain full100:
  `/Volumes/data/urnetwork-browsing-sizing-ab.SuqKJX/PACING-ON-OUTCOME.v1.json`,
  `NO-DRAIN-OUTCOME.v1.json`, `NO-DRAIN.overlay.json`,
  `transfer_window_pacing_no_drain.go`, and the raw test paths named in those
  receipts. Both terminal root failures and the one-branch source delta were
  checked in this review.
- Pacing-enabled initial100 and overlay:
  `/Volumes/data/urnetwork-return-initial-admission100-prep.fWJmE5/OUTCOME.v1.json`
  and `INITIAL100.overlay.json`. Latest return source afterimages:
  `/private/tmp/urnetwork-tcp-return-initial-admission-fix-v1.MsuS8k/`.
- Instrumented receipt, recipe and selected records:
  `/Volumes/data/urnetwork-browsing-item-lineage100-prep.ynVfVT/OUTCOME.v1.json`,
  `LINEAGE100.RECIPE.v1.json`, `PRESSURE.v1.json`, `ITEM-LINEAGE.v1.json`.
  The trace reports 107 dropped observations and 4,544 overwritten ring entries;
  it cannot establish the terminal page-53 owner or fill missing packet history.
- Direct control receipt:
  `/Volumes/data/urnetwork-native-browsing-direct-null-controls-v1.jSZw5a/ROOT-OUTCOME.v1.json`.
- Admission and source-refusal results:
  `/Volumes/data/urnetwork-return-clock-root.eCgiNb/INITIAL-AND-SOURCE-OUTCOME.v1.json`.
- Persist-gap receipt and later race log:
  `/Volumes/data/urnetwork-return-clock-root.eCgiNb/ZERO-WINDOW-PROOF-OUTCOME.v1.json`
  and `persist-all-race.jsonl`. The receipt's `FIX_PENDING` status predates the
  later passing candidate log; it is not itself the candidate-pass receipt.
- Actual TUN peer logs:
  `/private/tmp/urnetwork-tun-zero-window-peer-v1.m3KmSx/root-normal-v1.jsonl`
  and `root-race-v1.jsonl`.
- Response 2 witness source and its overlay:
  `/private/tmp/claude-501/-Users-builder-urnetwork-server-connect/87de3e83-caa3-4e17-8f8d-bd129c7da2ea/scratchpad/response2-fable/overlay/response2_return_delayed_ack_witness_test.go`.
  Its reported normal/race failure was not rerun here. The source is sufficient
  to inspect the missing receiver discriminator and conflicting tail assertion.
- Response 1 test sources and recorded logs under
  `/private/tmp/claude-501/-Users-builder-urnetwork-server-connect/87de3e83-caa3-4e17-8f8d-bd129c7da2ea/scratchpad/response1-opus/`:
  `tests/ip_tcp_return_review1_test.go`,
  `tests/transfer_app_limited_pacing_review1_test.go`,
  `FINAL-all-overlay-normal.txt`, `final_overlay_normal.txt`,
  `final_overlay_race.txt`, `final_canonical.txt`, and the refusal-factorial
  logs. These executions were not repeated by this meta review.
- Canonical `transfer.go`, `transfer_window_pacing.go`,
  `transfer_window_pacing_delivery.go`, `transfer_window_receiver_rtt.go`,
  `transfer_pacing_diagnostic.go`,
  `ip_tcp_return_recovery.go`, `ip_tcp_return_burst_recovery_test.go`,
  `ip_remote_multi_client.go`, `ip.go`, `id.go`, `transfer_queue.go`, and local
  gVisor `snd.go:436`, `rcv.go`, `connect.go` and `endpoint.go`.

This version validates both reviews and includes one passing full bypass, the
fresh pacing-enabled failure and the failed drain-creation-only intervention.
It also records the user's default-off decision and checks the current
`ApplicationPacing` gate's separation from route policy. It does not report a
full100 pass for that new candidate. Compact workload capture, further paired
repeats, rate interventions and an internal controller-defect witness remain
outstanding.
