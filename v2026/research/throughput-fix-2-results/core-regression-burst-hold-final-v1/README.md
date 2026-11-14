# Final core regression and runner contracts

The held-pacing candidate passes the full root regression: **3,345 top-level
passes, 25 existing skips, zero failures**, package exit 0. The complete raw log
is retained. One passing test prints `s` without a trailing newline before its
Go outcome, so an anchored `^--- PASS` count alone misses that pass.

The candidate source SHA-256 is
`a36793a9871306b3acbb87e2f0ddeb1d79cecaebf26c795c518acdf1ae27c04b`.
The pacing file SHA-256 is
`6d1fc9afcf378ff01ad419371408717745bd95be529c8579eb4fda7c5e8b3fda`.
The run started immediately under concurrent load, approximately
7.79 / 8.07 / 8.35, and finished at 6.84 / 8.85 / 8.55. The original manifest
and status retain full precision. No quiescence wait was used.

The regression mode excludes the model cells that run in their separate tier.
The [matched component and combined validation archive](../service-pacing-burst-hold-v1)
contains the same production candidate's 36 passing core-model definitions,
846 readings and 512 passing race correctness definitions. The model source
predates only the third burst/backlog test addition. Its three programmed
propagation changes remain deferred until `NetworkQualityChanged` is added.

The [final scoped server archive](../server-scoped-configure-final-v1) retains
35 connect race passes and 28 proxy nonrace passes, both with cgo disabled and
the exact reviewed server configuration. Full database-backed server tiers
remain user-deferred.

## Runner regression

The source-snapshot fixture had not copied the ledger parser that the runner
now hashes and imports. It therefore failed before its intended source-order
test could run. The fixture now includes that real dependency and additionally
replaces the original parser during its Go test with a controlled exception.
Successful post-run parsing proves that the runner still uses the immutable
copied parser after the live source changes.

The snapshot suite passes five tests, and the new scoped server environment
adapter suite passes five tests. Their complete logs, commands, exit statuses
and source hashes are in `harness/`. The server adapter controls prove that a
failed service preflight does not replace configuration correctness, and that
unknown source, function, selection and instrumentation changes are rejected.

## Reproduction and scope

The full run used:

```sh
env GOCACHE=/tmp/codex-go-cache tools/throughput-fix-2.sh regression /tmp/throughput-fix-2-terra-regression-candidate
```

`regression/` contains the original complete log, ledger, manifest, source-input
manifest and terminal status. `runner/` preserves the tested runner and parser
fixture correction. Use fresh output directories for repeats; do not overwrite
the original snapshots. The original binary remains in the `/tmp` run
directory, with its hash recorded in the manifest.

The prior short-duplex end-to-end failure remains in its original archive.
The component hold policy has a deterministic before/after proof; both arms of
the later matched SDK comparison passed, so this final passing regression
does not independently prove the cause of that entire intermittent symptom.
`archive-manifest.json` hashes every payload except itself.
