# Final scoped server regression

Both audited selections pass on the candidate connect source. These are
synthetic ownership, lifecycle, queue and metrics tests; the user-deferred
database-backed server integration tiers are not claimed.

| Mode | Result | Instrumentation | Start load | Finish load |
| --- | --- | --- | --- | --- |
| `server-connect-deterministic` | 35 passes, zero failures/skips | `-race=true`, `CGO_ENABLED=0` | 6.64 / 7.77 / 7.97 | 12.03 / 9.34 / 8.55 |
| `server-proxy` | 28 passes, zero failures/skips | Nonrace, `CGO_ENABLED=0` | 13.25 / 9.83 / 8.74 | 11.94 / 9.85 / 8.79 |

Runs started immediately under concurrent load. The binary build information
in each manifest proves the actual instrumentation and cgo settings. The
connect source SHA-256 is
`a36793a9871306b3acbb87e2f0ddeb1d79cecaebf26c795c518acdf1ae27c04b`.
The untouched sibling server checkout is revision
`8ebcf6b0f76dc988fa1b0b62f34215d3c4f4d708`, source SHA-256
`7f4a4a85cf3b3063547cda485eb9413a4d165a9a57d2bd0e049de837a2ef1026`.

## Environment boundary

`server/connect/test.sh` sources `server/test-env.sh`. Its full entry point
configures exports and then attests/probes local services. The launcher owner
was unreadable on this host, so the former common preflight prevented even
the non-database selections from compiling.

For these two modes only, the runner reuses the exact `test_env_configure`
function with its original source path. The reviewed environment, sourced
launcher helper, test-file hashes and exact selected names are pinned in
`throughput-fix-2-server-tests.json`. A changed function boundary, source or
test selection requires a new review. The generated script changes only the
path used to locate the original server and the final function invocation.
It does not run or claim service preflight. Provenance and generated scripts
are preserved in each run directory. Integration modes still source the
original complete preflight; no server source or host configuration changed.

The first native build then stopped at an unaccepted Xcode license. The two
scoped modes default to `CGO_ENABLED=0`, matching the server production image's
build setting. Their fixtures use no cgo or native resolver API. The current
Go 1.26.7 Darwin/arm64 toolchain supports the existing connect race flag with
cgo disabled, as confirmed by the archived binary metadata. Proxy keeps the
documented nonrace policy from `server/test.sh`. An explicit caller cgo setting
is preserved. No license was accepted or host state changed by this work.

## Artifacts and reproduction

`connect/` and `proxy/` retain complete run logs, outcomes, source-input/build
manifests, configuration provenance and status. `runner/` retains the exact
runner, adapter, adapter tests and reviewed selection used by these runs.
Relative source inspection ran in each immutable build snapshot. External
local Go replacements and host resources are listed in the source manifest;
they are not claimed frozen.

The final invocations used fresh output directories and no caller cgo override:

```sh
env -u CGO_ENABLED GOCACHE=/tmp/codex-go-cache tools/throughput-fix-2.sh server-connect-deterministic /tmp/throughput-fix-2-terra-server-connect-final-cgo0
env -u CGO_ENABLED GOCACHE=/tmp/codex-go-cache tools/throughput-fix-2.sh server-proxy /tmp/throughput-fix-2-terra-server-proxy-final-cgo0
```

Use new output directories for any repeat. `archive-manifest.json` hashes
every archived payload except itself.
