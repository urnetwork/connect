# MEMSTEADY recovery trajectory

`memsteady-nat-recovery-a.csv` contains all 197 provider transfer/runtime
samples within the measured interval of the physical Android role-A
`audit-nat-recovery-A` block on 2026-09-17. The values are copied from the raw
`memory` and `memory_device_transfer` diagnostic parts. Device clock correction
is applied and timestamps are offsets from burst start. Baseline starts at
-20,814 ms; burst ends at 60,699 ms; quiet starts at 71,296 ms and the block
ends at 371,818 ms.

Only numeric measurements are retained. Device/account identities, flow
destinations, credentials and raw log text are excluded. The test combines
this trajectory with the standard report fixture for unrelated connection,
carrier and traffic evidence; it is a recovery-verifier regression, not a
standalone physical acceptance artifact.

The provider's root/NAT baseline is 2,902,684 / 1,329,820 bytes. Both recover
below baseline during quiet, then new charged traffic raises the final
snapshot to 3,225,532 / 1,652,668 bytes. The original endpoint-only gate rejects
this trajectory despite the observed recovery. Mutated versions retain burst
claims, remove or shorten the witness, rewind ledgers, add unmatched root
claims, and breach runtime/admission caps; all must fail.
