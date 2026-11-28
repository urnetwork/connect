// Package lagdriftsim is the drift test's calibration study
// (lag.dev/DRIFTTEST.md T3): a simulation that measures the test's error
// rates against the authoritative model, github.com/urnetwork/connect/lagdrift,
// and sets the early-stop threshold k_early (V10's EarlyK).
//
// Each simulated visitor sits at a known point, with an address that either
// tells the truth about their country or claims one 300, 1,000 or 3,000 km
// away. For each of up to 3 runs, the server's choice of 12 extenders around
// the address's place (E2) is pinged 4 times a second along paths built from
// V2's model: a route stretch drawn once per path, the extender's place
// error, the visitor's last mile, queueing on a Wi-Fi-like link with spikes,
// and lost pings, with far sessions opening late, as on the real stack. The
// frame's fit over its usable extenders is evaluated every 250 ms by V10's
// rules, as lagdrift.EvaluateStop decides them, and the server's verdict pools
// the runs (V7) and is judged as lagdrift.Decide judges it (V6), at V11's k
// for the run's confidence, 95% or 99%, and the address's continent. The
// parts that diagnose the textbook region judge at its k, at 99.9% too.
//
// Every draw comes from a generator keyed by the study's seed and by what it
// draws for, so a study reproduces exactly whatever order its workers run in.
// cmd/drift-sim runs the study and writes REPORT.md from end to end: its
// method, its summary and the k_early its rule picks, its findings and its
// tables all come from the program and the run.
package lagdriftsim
