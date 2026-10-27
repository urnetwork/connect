# Receiver service feedback: bounded design review

Status: proposal for review, not a production patch. No schema, shared source,
sampler policy, performance gate, or ACK scheduling change was made. The only
executable experiment adds hypothetical unknown protobuf fields in a frozen
test overlay to measure wire size.

## Findings

1. A cumulative receiver counter plus the timestamp of the exact ACK-named
   Pack can describe a complete receive interval across lanes. It removes the
   prototype's global message-ID lookup and cannot double-credit logical bytes.
2. The current producer's clock is **Client ingress observation**, after the
   multi-route reader returns. It is not a kernel or carrier arrival timestamp.
   Local queue drainage and real forward-delay changes can stretch/compress this
   interval. The tuple supplies evidence; it is not a capacity certificate.
   Root owns the independent local-queue proof and dual-clock counterfactual.
3. Physical retry copies belong in the receiver counter even when a retry
   cannot be an eligible endpoint. Logical delivery credit remains once-only.
4. Opaque generation inequality is insufficient for adopting a new generation.
   Delayed old ACKs must not reset a new anchor. NewId ordering is guaranteed
   inside a process, not across a receiver restart with a changed wall clock.
5. Exact nanoseconds fit the existing wire reserve. Naively copying a complete
   tuple into both existing Pack envelopes exceeds their 1-KiB owner charge.
6. One tuple is only an anchor. Receiver-feedback availability must not become
   true until there is an eligible complete pair. The diagnostic's
   `latest == 0 -> (0, serviceHoldRate, true)` branch suppresses legacy startup
   discovery without any receiver rate evidence. This is a separate confirmed
   root under review by the relay agent.

## Proposed optional tuple

Reserve three still-unused Ack fields as one all-or-none semantic tuple:

| Field | Proposed type | Meaning |
| --- | --- | --- |
| 13 receiver_service_generation | optional bytes, exactly 16 | Unique immutable receiver service instance; zero/invalid length is unavailable. |
| 14 receiver_service_wire_bytes | optional uint64 | Inclusive cumulative original outer Transfer-frame bytes observed by this instance through this named Pack copy. |
| 15 receiver_service_ingress_nanos | optional uint64 | Receiver-local monotonic elapsed nanoseconds captured for that same copy; zero at the clock origin is valid. |

Use scalar varints initially: maximum incremental size is 40 bytes and typical
counters/timestamps are smaller. Fixed64 fields 14/15 save four bytes in the
worst case but are not needed for the present reservation. A nested message is
also possible but adds framing and a generated wrapper; three scalar presence
checks are sufficient. A malformed/incomplete tuple makes rate evidence
unavailable while the otherwise valid logical ACK still completes normally.

The identity is already Ack.sequence_id/message_id and the receiving
SendSequence's service. No Pack field, new per-attempt nonce, transport-path
key, extra cumulative head, or additional SACK is required by this proposal.
Field12 remains exact-message receiver ACK residence in microseconds; do not
derive the new timestamp from ACK encoding time minus rounded field12.

## Receiver producer and ownership

Capture original outer frame length and the local stamp immediately after
Client.run's successful read, before decryption replaces the buffer. Publish
only after decoding validates the peer, message/sequence identity, local role,
companion and logical-lane range. A service key contains SourceId, ForceStream,
CompanionContract, local encryption role and encryption companion; remove only
LogicalLane. StreamId, route identity and the receiving ClientId do not belong
in that map key: the registry is an instance field of this Client/ReceiveBuffer.
Roles map to the sender's complementary session exactly as today.

Start with the prototype's explicit H1, expects-ACK Pack scope. Unknown carrier,
H3, no-ACK Packs, ACK frames and undecodable/unverified input do not contribute.
Each eligible physical copy increments its service's counter once, including
duplicates/retries and a valid copy subsequently refused by the receive queue.
This counts the physical work already observed, not application delivery.
Capture the inclusive counter with that copy's ingress stamp; never sample the
current counter later when its ACK is encoded. Retries are not deduplicated.

Attach the service when ReceiveBuffer creates an admitted ReceiveSequence;
share it across the logical lanes, and retain it through the worker's **done**
boundary after final ACK and Pack cleanup. Do not use public index removal or
Run's earlier exit as last-owner completion. One state per active logical
service, at most one more owner per existing active sequence, and no historical
peer/message map are needed. This is a lifecycle bound proportional to existing
live sequence owners, not a new absolute peer-count limit. If an absolute cap is
required, its memory allowance and omission fallback need explicit agreement;
the diagnostic's arbitrary 128/4096 limits are not proposed defaults.

Generation is immutable while any owner is active. Last-owner completion
removes the registry entry; a recreated service receives a new nonzero Id and
starts its counter at zero. Receiver restart does the same. Counter overflow or
unrepresentable/non-monotonic local time disables **new** snapshots for that
instance until it retires; never silently wrap or mutate the generation under
already queued items. Existing immutable snapshots remain valid. No extra
goroutine, timer, queue, per-packet allocation, or receive callback wait is
needed; short counter/map locking must not be held over admission/callbacks.

One compact storage option reuses existing receivedAtNanos:

- ReceivePack adds counter uint64 and a service provenance pointer. Count a
  physical Pack once even if ReceiveBuffer retries its closed-worker lookup.
- receiveItem adds only counter uint64; its ReceiveSequence owns the immutable
  generation. If handoff changed service instance, omit the carried snapshot
  instead of relabelling it or counting the same physical frame twice.
- sequenceAck adds only counter uint64. Its receiver already owns the immutable
  generation, while receivedAtNanos supplies the named Pack's stamp.
- ACK encoding copies generation/counter/stamp into scalar fields. No pointer
  escapes into the channel, ACK compressor, sender, or wire owner.

Current measured sizes are ReceivePack216, receiveItem232, decodedPackOwner1000,
sequenceAck96, receiveAckMessage104 and decodedTransferFrame680 bytes. The above
layout is expected to add 24 bytes to decodedPackOwner (1024 exactly) and eight
to sequenceAck (104); compile-time exact-size tests must verify it before
acceptance. The compact sender ACK needs an inline 32-byte tuple (about136 total)
and owned decoder needs scalar/Id backing storage plus generated fields. These
are explicit memory costs, not claims of unchanged footprints. Preserve the
1-KiB owner charge only if the final measured layout fits on supported targets.

## Head/SACK and retry semantics

Keep the named receipt attached through ReceivePack -> receiveItem ->
sendAckAt/commitHeldItem/flushDeliver -> sequenceAckWindow -> ACK writer. A newer
cumulative head carries only its own tuple while absorbing lower ACKs. Never
combine another Pack's counter/time with its message ID, tag or field12.
Oldest-first above-head SACKs keep their own tuples; quiet-head and final-drain
paths copy the same value. Same-message replacements replace the entire tuple,
not independent maxima. Missing-contract requests and past-delivery untagged
recovery replies omit it. A duplicate old Pack may trigger retransmission of
the current head, whose original tuple must remain unchanged.

No first-ingress message-ID history is necessary: each retained copy carries
its own receipt. If the sender has retried the named item, its original
physical-send stamp no longer names a unique receipt. Reject that endpoint
using the existing retry/Karn state, even if Tag is unchanged. Retry bytes still
enter the cumulative counter and appear in a later unambiguous interval. The
tuple is not proof that earlier copies on another route have left the service;
it must never set drained, refresh an unloaded probe, or defer a recovery timer.

## Sender acceptance and generation fencing

Consume at bounded ACK coalescing/physical-confirmation time, while the exact
item remains queue-owned and before delivery release. Require matching live
sequence/message/tag, successful actual H1 first-write confirmation, the
existing current H1 policy, and an unambiguous original write. A changed-carrier
lane must not consume shared H1 evidence or invalidate other H1 siblings.

ACK-before-write completion needs one pending tuple in the sequence's existing
pending physical-write record. Publish only after successful H1 confirmation;
failure, retry preparation or changed disposition discards it. Do not use
`!serviceCreditObserved` as the sole once-only guard: logical service credit
may already have been published when physical confirmation arrives. Reuse the
one-shot physical observation lifecycle and explicitly test both orders.

For one generation, require increasing receiver counter and timestamp before
forming a pair. Older/equal ACKs cannot rewind the anchor or produce another
sample. Simultaneous timestamps produce no zero-duration rate. Use checked
unsigned differences and checked rate arithmetic; never cast an overflowing
remote duration directly to time.Duration. Keep a bounded anchor and existing
bounded sample history, with O(1) updates/O(slots) reads and read-only stats.

A conservative ACK-only generation transition can use one local causal fence:

1. First valid generation establishes an anchor and stores the ACK's actual
   local arrival time as the generation-adoption fence. It produces no rate.
2. A different generation is accepted only from an unambiguous original Pack
   whose physical first write occurred strictly after that fence. If not, use
   its logical ACK normally but withhold its rate tuple.
3. On acceptance, discard the old anchor/history, retain the prior valid rate
   as zero-order hold, and advance the fence to this ACK's local arrival.
4. A delayed ACK from the retired receiver state necessarily refers to a write
   before the new state's adoption; it cannot pass that new fence. No unbounded
   retired-generation set or cross-host clock comparison is needed.

This argument requires one live receiver service authority per peer/key: old
state stops producing new observations before the replacement is adopted. If
two independent Client instances with the same peer identity can stay active
concurrently, opaque ACK-only generations cannot totally order them. Bind to
an existing authoritative session generation or leave replacement feedback
unavailable; do not pretend a random Id order solves that case. NewId alone is
not durable restart ordering. This transition rule is a proposal requiring
forced lifecycle tests, not an implemented or validated policy.

Last sender-sequence close destroys its service and pending observations.
ContainsMessageId/live sequence ownership rejects old ACKs after recreation.
Lost/coalesced ACKs are harmless: the next eligible cumulative snapshot includes
all intervening physical copies. A missing tuple is not a zero byte count or a
new epoch. Pure legacy peers follow the existing sampler unchanged. Mixed
omissions or old ACKs must not erase a valid generation. Fresh legacy-only
feedback after a restart needs an explicit ownership/fence rule for retiring
the optional anchor and returning to legacy samples; don't latch metadata mode
forever, and don't let one old omitted SACK reset shared state.

Until the **first eligible complete receiver pair**, keep the legacy estimator
available; a generation/anchor alone must not suppress ordinary discovery or
freeze a bootstrap rate as measured capacity. After accepted receiver evidence
exists, absent fresh evidence holds the last valid value. Epoch reset retires
the old pairing history without manufacturing a zero or a new sample. The
precise mixed-peer preference transition needs its own test rather than a
presence-only mode bit.

## Sender offered-interval eligibility remains necessary

Receiver elapsed time alone also includes source/window pauses and genuine
forward-delay transitions. Retain the diagnostic's causal all-worker pause
proof, attached at **physical first write** to each candidate endpoint. Do not
infer it from contemporary worker idleness when an ACK is applied: the retained
long root has continuously offered originals and a later unrelated writer gap.

Production registration must cover every service member before its first write,
and remove membership after close, including never-written, blocked, new and
changed-carrier siblings. The diagnostic registers only after first write and
uses capped global maps, so it does not prove those lifetime cases. A bounded
per-item train identifier or equivalent immutable provenance is an additional
sender cost that must be measured and accounted; do not reuse pacingBurst if
that would change its existing RTT meaning.

Receive order across lanes/routes need not equal physical-send order. A pair
whose matched offer endpoints are reversed does not establish an offered rate.
The diagnostic currently substitutes MaxInt64 when offeredSpan<=0; reject that
lower-rate proof rather than treating it as unlimited demand. Furthermore,
receiver counter delta can include old retry copies offered before the first
matched endpoint. It is valid receive occupancy, but is not automatically the
sender's byte count over the matched offer interval. Force this case before
using it as proof of sustained offered load. Keep any no-evidence read as hold.

The parent-owned dual-clock experiment is relevant here. The earlier max-span
counterfactual measured11,875,003 rather than12,500,000B/s for8019 bytes with
receiver641520ns versus paced sender675284ns. Its old raw-rate expectation is
not evidence that a max-span **plus retained lower-rate eligibility** policy
must fail. Preserve real rate-rise/decrease and queue-drain controls before
choosing consumer arithmetic; this review does not select a new rate policy.

The parent has since forced the queue case: max(sender span, receiver span)
repairs the continuously paced queue drain but still estimates250 MB/s from a
buffered original burst against a known12.5 MB/s service. Both software clocks
can compress the same interval. Existing own0.95-pacing, genuine slowdown and
capacity-rise controls passed in that experiment. The max-span candidate is
insufficient and unintegrated. Encoding more timestamp precision cannot fix
this missing physical-boundary information; a production consumer still needs
an independently justified eligibility contract for that case.

## Wire experiment

One isolated top-level test passed once, under current load, using canonical
overlay keys and a stable frozen836-file connect input plus24 glog files. It
compares the hypothetical fast bytes to generated protobuf's unknown-field
oracle and uses the real sequenceCipher outer wrapper. All existing optional
fields are maximal, including field12, full route IDs, tag, missing-contract
ID, capabilities and max-width eviction numbers. This deliberately overstates
the new tuple on missing-contract ACKs, where the design would omit it.

| Proposed scalar encoding | Plain v2/legacy | Encrypted v2/legacy | Reservation |
| --- | ---: | ---: | ---: |
| uint64 varints, no evictions | 203/206 | 294/297 | 320 |
| uint64 varints,512 evictions | 5326/5329 | 5417/5420 | 5440 |
| fixed64, no evictions | 199/202 | 290/293 | 320 |
| fixed64,512 evictions | 5322/5325 | 5413/5416 | 5440 |

The existing response limit still uses min(32,floor((8192-evictionBytes)/320)),
so its8-KiB response cap and32000-byte early-head credit threshold need not
change. A real wire implementation must rerun full response/ownership tests;
this experiment checks sizing, not new decoders or consumer correctness.

Nanosecond encoding avoids a material new quantization error:1280 bytes at
1Gb/s take10240ns, so integer microsecond endpoints can report10us or11us;
52-byte intervals take416ns and may collapse to zero. Nanoseconds still require
zero/equal timestamps, coarse host clocks and buffered read bursts to be tested.

## Required implementation touchpoints

- protocol/transfer.proto and regenerated protocol/transfer.pb.go.
- frame_protobuf.go: sendAckFrame, sizeAck, appendAck; decodeAck and
  decodeAckOwned; decodedTransferFrame inline scalar/Id storage and pool reset.
  Validate both wire forms, unknown fields, duplicate-last-value behavior,
  malformed wire types/lengths, all-or-none presence and full uint64 bounds.
- transfer.go Client.run: preserve outer length/time through decrypt and legacy
  Frame decoding; ReceivePack/receiveItem handoff; ReceiveBuffer sequence
  creation/refcount/CloseAndWait; every ACK producer and both ProtocolVersion
  writer branches, plaintext and encrypted.
- transfer_ack_compression.go: whole-tuple identity through Update/UpdateDelivered,
  head absorption, duplicate SACK replacement, Snapshot, takeResponse,
  takeQuietHead, overflow and final shutdown flush. Preserve one head and
  bounded oldest-first above-head SACKs exactly.
- receiveAckMessageFromProtocol, compact ACK channel and saturated inline
  coalescer; transfer_sender_ack_timing.go's pending write/confirmation and
  sticky one-shot lifecycle; new service consumption before message release.
- transfer_window_service_credit.go: leave logical prefix credit once-only;
  receiver counter is independent physical observation, not extra ACK credit.
- Exact footprint tests: flight_gate_landing_size, receiver timing adjacent,
  frame_protobuf decoded-owner/compact ACK limits, pool ceilings, retained-byte
  accounting and allocation benchmarks. Update costs explicitly after measuring.
- Codec oracles/buildEquivalentAckFrame, Ack receiver-delay and eviction reserve
  tests, generated/fast/legacy interoperability and old-peer ignored fields.

## Predeclared deterministic test matrix

| Boundary | Required forced result |
| --- | --- |
| 1/8 lanes, interleaved heads/SACKs, dropped/coalesced ACK | One shared counter; exact endpoints; no fragment-as-whole or double credit. |
| Encryption/legacy wrapper/batch/group | Original outer bytes, not payload/decrypted length; equal units at sender and receiver. |
| Retry inside interval and retry at endpoint | Every physical byte counted; logical credit once; ambiguous endpoint rejected. |
| ACK before/after write return; write failure/carrier change | Identical valid result; failed/unconfirmed H1 produces none; no pending leak. |
| Older ACK/duplicate/SACK->head | No anchor rewind, duplicate sample or incorrect borrowed tuple. |
| Cross-lane reversed send/receive order; delayed old retry occupancy | No fictitious offered-rate proof; fresh ordered train restores measurement. |
| Last sequence close/reopen, last ACK still queued | Old generation stays attached to old ACK; new state starts counter0; no resurrection. |
| Receiver restart with backwards wall clock | Different opaque generation; delayed old ACK cannot switch it back. |
| Reset before/after sender generation fence; mixed legacy omissions | Logical ACK always works; unavailable tuples hold; fallback eventually works without false withdrawal. |
| New/closed/never-written sibling and active sibling | Exact worker membership; no false all-worker idle interval. |
| Source/window wait; successful/aborted controlled pause | Only physical endpoints across actual pause lose eligibility; old in-flight bytes aren't relabelled. |
| Local queued-reader drain, equal/ns-quantized timestamps | No infinite/burst-inflated capacity; distinguish clock precision from actual queueing. |
| Real rate rise/down, compression-only step, forward latency step | Existing gates unchanged; new evidence may adapt, no-evidence reads hold. |
| Counter/time overflow, partial/malformed tuple, generation collision | Omit invalid sample safely; no wrap, allocation blowup or delivery loss. |
| Stats polling/no polling | Identical retained control state and service decisions. |
| Maximal encrypted/legacy ACK and512 eviction entries | <=320 per entry plus eviction charge, <=8KiB response, fixed head/SACK policy. |
| Queue/owner teardown, canceled handoff, pool reuse and codec hot loop | No borrowed pointer escape, stale tuple reuse or new per-packet allocations; exact memory costs bounded. |

No performance acceptance claim follows from this design or wire-size test.
