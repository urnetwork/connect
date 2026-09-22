# service shared aggregate boundaries v1

The new ring has exact arithmetic contracts for tied first bytes, earliest offers after late insertion and resizing, recent-window selection, unknown/future/pre-cutoff offers, modulo eviction and replacement of old fast history. Five pass on the first candidate. The sixth reveals missing pending-drain boundaries; the minimal guard makes all six pass and preserves the nine warm controls. These are new bounded-summary arithmetic/eligibility tests, not an actual controller-throughput oracle.

Original full source/build/runtime CWD, binary, status and raw-log hashes remain pinned by each normalized manifest. Original local files are unchanged.
