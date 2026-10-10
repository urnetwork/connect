# Cumulative head timing and destination ingress order

## Proven safety failure

Three confirmed initial H1 items contain 1,000, 7,900 and 100 bytes. Their total fits the 512 KiB sender permission, and every physical item is below 8 KiB. A shared 200,000 B/s serializer delivers the first checkpoint at 100 ms. Different relayed routes then deliver the final 100-byte head at 100.5 ms and its preceding 7,900-byte sequence item at 140 ms. This consumes exactly 40 ms for the remaining 8,000 bytes.

The receiver retains the early head behind the hole. It emits the cumulative ACK at 150 ms and reports the head's original ingress time through a 49.5 ms receiver wait. Its earlier selective ACK is delayed until after the cumulative ACK. These are legal first writes; no retry or fabricated timestamp is needed.

The baseline removes that wait from all 8,000 newly credited prefix bytes and publishes **16,000,000 B/s**, eighty times the physical rate. The head's exact identity establishes its own timestamp; it does not place the preceding prefix before that timestamp. A local H1 route cannot establish destination FIFO because relay forwarding may choose different routes. The baseline negative repeats three times under race instrumentation; ordered ingress and prior-selective-credit controls pass.

## Narrow correction

`initialH1` now names the existing physical-offer eligibility fact: every newly credited envelope was confirmed on its initial H1 write. This eligibility remains available to the independent raw aggregate history.

A nonzero receiver wait can retime credit only when the newly credited bytes belong to that exact head alone. A cumulative prefix containing other newly credited envelopes retains raw ACK timing. A zero wait removes no interval and remains usable. Prior selective credit can leave only the new head uncredited; its own wait remains usable. Unknown, failed, copied or non-H1 prefixes retain their prior raw behavior. Late repeated selective or cumulative heads do not add credit.

The correction changes neither physical credit nor receiver RTT observations. It does not add a route key, change the wire, or assume that a route defines sequence correctness.

## Matched results and disputed expectations

| Selection | Before | After | Interpretation |
| --- | --- | --- | --- |
| Original three ingress roots, count 3 | 6 pass, 3 fail | Included below | Direct baseline failure with two controls. |
| Expanded 29 definitions, count 3 | 84 pass, 3 fail | 81 pass, 6 fail | The ingress failure is fixed. Twenty-seven definitions pass after; two unchanged exact cold-rate definitions fail. |

The two after failures are `TestWindowPacingSdkColdReceiverWaitMeasuresShortTrain` and `TestWindowPacingSdkColdDrainedTrainUsesExactPair`. Their physical fixture can represent an ordered 125 MB/s train. Its first reply credits one envelope; the next cumulative reply credits the remaining multi-envelope prefix with a positive receiver wait. Both tests require the sampler to infer exactly 125 MB/s immediately from those two replies.

The sender-visible facts do not establish that all prefix bytes arrived before the final head. The new ingress counterexample disproves that general inference. Exact immediate serialization is therefore an observation-policy expectation, not a hard consequence of those ACK fields. A valid implementation may abstain from that exact sample and use later raw cumulative evidence. This does **not** mean that the physical fixture is unreachable or that poor eventual pacing is acceptable.

After the restriction, the short-train test first reports zero service and then reports a very low raw refill sample (27,692 B/s at 100 ms RTT, 6,735 B/s at 400 ms). Those readings remain visible. The component test alone cannot decide whether final pacing recovers. Unchanged static SDK and shared-pacer tests separately judge that behavior; their thresholds are not changed by this audit.

All 29 original definitions are preserved byte-for-byte in the matched before and after sources. No expected-failure wrapper, skip, or numeric gate change was used. The only new test premise correction before this pair was the separately archived, rejected queued-start cap; it is unrelated to this prefix proof.

## Proof boundaries

These are deterministic sender/coalescer component tests with independently supplied legal physical timestamps. They do not run a socket or establish closed-loop throughput. The normalized manifests retain exact original source, build, binary, status and raw-log hashes, confirmed race instrumentation and the relationship between runtime CWD and its copied source tree. Original local proofs are unchanged.
