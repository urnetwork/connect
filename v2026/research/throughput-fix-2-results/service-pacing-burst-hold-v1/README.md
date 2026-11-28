# Held pacing through a permitted reverse burst

The deterministic controller root changes from failure to pass. The unchanged
SDK test passes five repetitions in **both** matched arms, so this comparison
does not establish a throughput improvement or deterministically reproduce the
earlier intermittent end-to-end SDK failure.

## Change and proof

An ACK can wait behind a lawful reverse-direction data burst. Previously the
same small RTT increase ended discovery and cleared the held pace, even when
forward flight was below its independently computed backlog bound. The explicit
10 ms reverse-burst test clears a 137,500,000 B/s hold and falls to 111,219,709 B/s
before the change. It preserves that hold afterward.

The candidate continues to end discovery at the original queue margin. An
RTT-only hold release also requires exceeding the existing maximum burst
duration. Physical forward backlog, a longer queue after flight drains, and
recovery reservations still permit an immediate decrease. No threshold in the
SDK test, window permission or physical byte allowance changes.

Both source arms include identical tests. Only `transfer_window_pacing.go`
differs, as shown in `hold-release-only.patch`:

| Arm | Complete Go/proto/module/profile source SHA-256 | Pacing file SHA-256 |
| --- | --- | --- |
| Before | `47d4586608450a46dfdc695054578292e996d440e9c08d2c18ed6a05524dd7a1` | `b6acc02099e55a24d7f73663bd5e2d97d88fc85f0b39ac2404cada74cb6ef755` |
| After | `a36793a9871306b3acbb87e2f0ddeb1d79cecaebf26c795c518acdf1ae27c04b` | `6d1fc9afcf378ff01ad419371408717745bd95be529c8579eb4fda7c5e8b3fda` |

The local `glog` replacement is copied and hashed in each arm. Every recorded
source file is checked again after build and execution. The source/build
manifests, complete logs, outcomes, service ledgers and status files remain in
`before/` and `after/`. Binaries stay in the original `/tmp` snapshot; their
hashes and build commands are recorded.

## Results

| Selection | Before | After |
| --- | --- | --- |
| Five controller definitions, three race repetitions | 12 passes, three failures | 15 passes |
| Short retained SDK duplex, both device roles, five repetitions | Five passing definitions; all ten role comparisons pass | Five passing definitions; all ten role comparisons pass |

The three new contracts preserve one bounded burst, release the hold for an
excess queue even after drain, and require excess forward flight to bypass and
replace the hold at 0.95 service. Existing held-rate and discovery/recovery
contracts also run. The SDK reference gate remains 90% in each direction,
with its original 0.3015-second warmup, one-second measurement, 300-microsecond
RTT, serializer and physical queue bounds.

Candidate upload ratios range from 90.48% to 93.92% for the non-providing role
and 92.22% to 94.56% for the providing role. Both arms report no relay drops.
The complete `summary.json` retains every direction and repetition. Preserve
the earlier 855.296-versus-957.2352 Mb/s failure in
`../service-receiver-prefix-live-sdk-v1`; these passing repetitions do not
erase or causally explain that whole end-to-end observation.

## Combined candidate validation

`model-core/` records 36 passing definitions and 846 service readings. It
excludes exactly the three user-deferred programmed propagation changes; the
deferred-test manifest is included. The isolated short SDK cell reaches
918.65088/957.21472 Mb/s (95.97%) in the non-providing role and
882.86208/957.19424 Mb/s (92.24%) in the providing role. Both pass, with no
relay drops.

`correctness/` records 512 passing race definitions, 12 ledger readings and no
failures or skips. This source includes all three new controller contracts.
The model snapshot preceded the third test addition; its production pacing
hash is identical. Full root regression was still running when this evidence
was collected, so it is not claimed here.

All runs started immediately under concurrent host load. Original manifests
and status files record load averages. No quiescence wait, host credential
change or server integration run is part of this comparison.

## Reproduction

`prepare.py` records the snapshot construction and `run.py` records the exact
build/run commands. Their original output was
`/tmp/throughput-fix-2-burst-hold-pinned-v1`; never overwrite it. For a new
experiment use fresh output paths and a source tree matching the desired
manifest. The original arm commands were:

```sh
env GOCACHE=/tmp/codex-go-cache python3 /tmp/throughput-fix-2-burst-hold-run.py before
env GOCACHE=/tmp/codex-go-cache python3 /tmp/throughput-fix-2-burst-hold-run.py after
```

The candidate combined runs used the standard runner modes `model-core` and
`correctness` with fresh output directories. Their original runner source,
source-input manifests, binary hashes, complete logs and statuses are included.
`archive-manifest.json` hashes every archived payload except itself.
