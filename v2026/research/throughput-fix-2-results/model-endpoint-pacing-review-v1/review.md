# Endpoint pacing review of the complete slowdown model

This is an offline review of the frozen experimental model in
[`model-receiver-invalid-slowdown-full-v1`](../model-receiver-invalid-slowdown-full-v1).
It is not a new run or validation of the working tree. Source hashes and the
derived numbers are retained alongside this note.

## Result and consequence

All 18 candidate directions below the existing 90% reference criterion end
with **positive** `ServiceByteRate`. The long-RTT failure also ends with positive
service: 836,957 B/s, pacing 920,652 B/s, and measured goodput 5.612 Mb/s.
The initial-feedback SDK failure's slow direction ends with service
1,605,801 B/s and pacing 1,766,381 B/s, delivering 13.537 Mb/s against its
754.647 Mb/s reference.

The newly proved cold cumulative fallback defect must be fixed, but changing
only the zero-service branch cannot directly reprice these terminal states.
The next combined candidate must also demonstrate recovery from a positive
low hold, then pass the original affected cells. This does not establish that
the low hold caused every loss: final snapshots cannot reconstruct earlier
controller transitions, and some cells have independent window or ACK/data
coupling limits.

## Derivation

Read all 824 ledger rows and the 30 top-level outcomes. Select the five failed
test groups. Pair `ceiling` and `delivery` rows by test name and the complete
cell description except `Arm` and `Drop`. Record each candidate direction
below 90% of its paired reference; direction zero uses `Window`, direction
one uses `ReverseWindow`. Preserve each profile's name, budget and policy
flags because names alone are not unique configurations. Record the changing
RTT cell separately: its reference starts at 1.2 seconds RTT, while the candidate
steps from 0.3 milliseconds to 1.2 seconds during the common warmup.

The SDK cells run captured constructor policies with synthetic Transfer/FIFO
traffic. They do not execute the dedicated provider TCP ACK worker. Neither
this review nor the optional ACK-retention integration pair supplies a native
provider-throughput result.

No performance threshold, fixture or production source was changed by this
review. The original complete failed run remains authoritative.
