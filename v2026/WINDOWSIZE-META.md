# WINDOWSIZE review response: experiments and evidence

The configuration-only **32 MiB, one-provider advertisement-cap comparison has
run and closed** on the qualified current55 product. Reducing encoded permission
from 2,181,038 to 655,360 eliminated the observed RX funding refusals, but mean
goodput fell from 107.776 to 75.928 Mbps (29.6%). Both arms passed all five
windows and the original five-second probe gate. This supports an effect on
physical pressure, not adoption of the cap or statistical performance promotion.
The next running arm keeps encoded permission unchanged and raises only the
physical RX child allowance under the same root. The historical R13/R14 replay
already ran; no production memory model, timeout or shared-callback policy is
changed by this response.

Reviewed `WINDOWSIZE-REVIEW1.md` (`f29e2315…`) and `WINDOWSIZE-REVIEW2.md`
(`ef4d7ce1…`) against current code, `CODESTYLE.md`, the effective current55
overlay and retained closures. Both reviews explicitly ran no tests themselves.

| Review callout | Evidence now established | Discriminating test and status |
| --- | --- | --- |
| Encoded advertisement versus physical backing | `receiveWindowAdvertisement` takes the logical limit, RX total and optional cap; it does not convert retained cost or divide shared rights among sequences. The captured eight-frame refusal charges 30,544 retained bytes. | **Executed and closed:** fixed-provider cap A/B, physical RX 2,181,038 in both. Ordinary RX refusals 1,549→0; final funding refusals 1,075→0; provider timeout resends 1,146→0. All ten windows pass, but capped mean goodput is 29.6% lower. **Executed:** physical RX 7,633,633 removed refusals, but the first GET still failed at 5.350 s; see closure below. |
| Silent refusal and slow recovery | Decoder funding refusal records local pressure and a drop without an explicit refusal-ready wire notice. Existing eviction recovery acts on selectively acknowledged originals; it is not a notice for a never-admitted original. A finite-burst pass does not establish sustained recovery. | **Executed:** three sustained signed-fixture roots prove actual consumed advertisement, a binding encoded ceiling, continuous offered demand through one 80 ms pause, at least ten full refills after release, exact refused-owner recovery and full custody. A five-second miss is reported after actual terminal ACKs; setup/watchdog failure cannot qualify as the desired latency regression. |
| `d90ac7f4` changed shared admission from wait to refusal | The commit changes both Pack and ACK admission and their busy-lock handling. The retained pair differs in product content only in `transfer.go`. Its completed ABBA shows R14 consistently slow, but R13 also collapses. | **Executed and closed:** four complete historical SDK32 Fast5 processes, all failed the original qualification. Reuse this evidence; restoring a waiting shared callback is not a demonstrated fix and conflicts with current shared-receive policy. |

## Configuration-only pair

Source packet:
`/Volumes/data2/urnetwork/temp/window-cap32-fixed-r1/READY.json`, SHA256
`7757f313de40d7ca13adcf3515d9e743ef819c1d976e214f958e1b9767c41c56`.
`BASELINE.overlay.json` SHA256
`1d431b097075b192b1cb1fd7ace94ed311f2c86f4d1f9dc0a3537aae418ebea1`;
`CANDIDATE.overlay.json` SHA256
`5bde846a00cf06c60881772ac70c4207b3624a34385c38c423c90f022d942bcb`.

Both maps derive from current55 common map `1feaabf2…`, READY `c537b03f…`.
There are zero changed Connect/product bindings. The common fixture attachment
is in `COMMON-FIXTURE.diff`; `CAP-ONLY.diff` contains the sole arm difference:
`privateRxPhysicalFixtureEncodedCap`, 2,181,038 to 655,360. The fixture's SDK
constructor receipt and independently expected policy both use that declared
constant. The physical RX/root/Head/Pack/TX totals remain independently checked.

The same root runs in both images:

    TestFullTunH1SyntheticFastClient32Desktop128FixedProviderWindowCapFiveWindows

It retains five ten-second windows, the original mandatory five-second GET
gate, one initial stream growing to eight, the same four origins and H1 links,
and all six actual Desktop128 provider owners. A public named destination uses
provider 0; each policy cut requires actual one-provider membership. Every
provider still closes and refunds through the original guards. This isolates
one sender without changing provider memory or contracts.

The existing receive-sequence snapshots must show the declared cap. A common
bounded provider recorder additionally requires an actual validated noncontrol
ACK carrying that cap; configured method values alone are not peer receipt.
`RUN-HANDOFF.json` gives exact build directory, selectors and verifier calls.
The first owner runner started at 00:14:44 UTC on November 11 in
`/Volumes/data2/urnetwork/temp/window-cap32-fixed-qualification-r1`
(owner 25363, supervisor 25361), then failed because its owned `run/gopath/bin`
directory was missing. No compile, control or workload ran. Source checks and
process/lease closure completed; module checks and image setup had not run.
That setup failure is closed (`CLOSED-HANDOFF.json` SHA256
`62883622358dd9c8ca3402ead0bd9444aa3cc579e0171d11f80ef12b0c1dfdc0`).
The corrected successor at `window-cap32-fixed-qualification-r2` completed two
normal PERFVAR builds, three controls per image and one A/B. Its runner and
sealed source pair were unchanged. A setup-only successor is not a workload
reroll. All source/module checks passed, processes joined, file descriptors
closed, leases released and the fresh APFS image detached normally.
`CLOSED-HANDOFF.json` SHA256:
`312083bd7ed0fd7652a3cd62c4862134ce82b66ff5ee6a798e94151662205d01`.

| Arm | Five window Mbps | Largest completed probe per window, seconds | Provider timeout resends per window |
| --- | --- | --- | --- |
| Encoded 2,181,038 | 82.392, 119.869, 87.160, 144.374, 105.087 | 1.386, 0.753, 0.557, 2.613, 1.405 | 0, 0, 0, 0, 1,146 |
| Encoded 655,360 | 89.543, 83.664, 42.817, 80.866, 82.750 | 0.468, 0.580, 1.067, 0.493, 0.338 | 0, 0, 0, 0, 0 |

The baseline completed 53 loaded probes with four canceled at window boundaries;
the cap completed 66 with three canceled. Both passed every original gate.
Ordinary RX refusals fell from 1,549 to zero, final all-funding refusals from
1,075 to zero, and provider selective-gap writes from 159 to zero. These are
different counters, not a per-packet conservation proof. Actual policy cuts
verified physical RX 2,181,038 in both images and the declared encoded caps;
validated peer ACKs carried those caps. Native flow attribution remains
incomplete in both runs. One process per arm on a shared host does not establish
a repeatable latency or throughput improvement, and the capped run's lower
goodput argues against selecting it as the default.

The now-retained baseline binary is
`window-cap32-fixed-qualification-r2/run/bin/baseline.test`, SHA256
`5ff8e40948dfb0fd2de14f95492de6b50977d728bc6923ae867115d0a4e000ce`.
The older `88f4…` binary lacks this fixed-provider root.

The minimal build body, inside that existing bounded owner, is:

    go test -c -mod=readonly -p=2 -tags=urnetwork_synthetic_fixture \
      -overlay=<BASELINE.or.CANDIDATE.overlay.json> -o <owned-output>/<arm>.test .

Use the maintained PERFVAR service environment and fresh owned APFS runtime,
CPU 2, source/module audits and unchanged `-test.count=1 -test.cpu=2
-test.parallel=1 -test.timeout=660s` body. Do not invoke a bare test binary from
an arbitrary working directory. The derived performance verifier only adds the
declared fixed-provider root/membership case; all original workload and owner
gates remain. The RX verifier requires the independently requested arm:
`verify(stdout, 2181038, expected_cap)`. Raw failures remain retained.

The observed refusal reduction supports the backing hypothesis for this
fixture; it does not attribute the earlier failed GET to a specific refusal.
Cold permission before the first ACK and per-original overshoot still need
separate attribution. A future production-selection arm cannot falsify the
single-sender hypothesis by itself: each additional active sequence shares RX.
The 64 MiB cap of 1,310,720 remains deferred while the same-flight backing arm
tests the throughput tradeoff in the 32 MiB fixture.

## Same-flight physical-backing arm: executed, not qualified

`/Volumes/data2/urnetwork/temp/window-rx35-fixed-r1/READY.json`, SHA256
`d73fb0132b1e537f06aaaf6486912a0ba91feda6f2f50a9f3e336399a8d2fc4f`;
candidate overlay SHA256
`b1bf60327a5334ed85cad27a24a2479b80882b39f5dc4899384d3d6d18542205`.
It starts from the closed cap experiment's **baseline** map and reuses its
baseline binary and observations. The sole capacity change is physical RX
**7,633,633 = 7 × 2,181,038 / 2**. Encoded cap remains **2,181,038**; root/client
remain **21,810,380**, Head/Pack remain **2,181,038**, and TX/provider/traffic
settings remain unchanged. The existing resize branch is reused. The complete
`PHYSICAL-ONLY.diff` contains the scalar, independently declared expected-RX
guards, the existing scalar admission-test expectation and diagnostic text.
There are zero changed Connect/product bindings and no new workload root.

The factor 3.5 exceeds the observed bulk retained/encoded cost near 3.3 and the
conservative inverse cap ratio 2,181,038 / 655,360 ≈ 3.328. Factors 2 or 3 would
leave that observed bulk geometry under-backed. This is not a universal bound
for all packets, multiple sequences or simultaneous sibling use. The root
remains authoritative; this allowance is not an eager memory allocation.

`RUN-HANDOFF.json` requests one new normal PERFVAR compile, the same three
controls and one unchanged five-window root inside the existing bounded owner
with fresh 64 GiB APFS and original five-second probe/closure gates. No baseline
rerun, ABBA, SDK compile or retry is requested. The performance/common observer
verifiers are unchanged. The independently declared RX gate is
`verify(stdout, 7633633, 2181038)`; the retained baseline uses
`verify(stdout, 2181038, 2181038)`. Source review precedes launch. Any decision
to retain a budget change still requires physical Android 32/64 MiB memory
qualification; this diagnostic does not qualify a shipping profile.

The separate sustained-source draft is
`/Volumes/data2/urnetwork/temp/shared-pump-sustained-cap-r1/source/connect/transfer_shared_pump_sustained_recovery_test.go`.
Its roots are `TestSharedPumpSustainedDefaultFlightMeetsLoadedRequestBound`,
`TestSharedPumpSustainedCappedFlightMeetsLoadedRequestBound`, and
`TestSharedPumpSustainedUnpausedFlightControl`. It reuses the signed fixture
with production advertisement handling, twenty-four default flights of offered
payload, at least ten full refills after one finite 80 ms callback pause, and
the original five-second recovery bound. Actual source demand and binding
permission are checked before, during and after the pause. It remains a
component test; its retained initial-window setting is not the complete SDK
provider constructor. Source review is complete; frozen READY SHA256 is
`3c03a9d5b5c3fe17879b00b195b5794aefc09133e71e49ad6b1cc7ee8d32ff32`.
Its first normal execution closed at
`/Volumes/data2/urnetwork/temp/shared-pump-sustained-C-qualification-r1`, SHA256
`956b18929244e892e6c9b5928506e423901b9b3e067dc5b6966fb97d4cee7e72`.
The capped and unpaused controls ACKed all 5,112 messages in 3.36 s and 2.92 s.
The default paused arm reached its 60 s watchdog with 4,717 ACKed; this is an
incomplete component execution, not a qualified five-second root-cause failure.
A diagnostic repeat also remained incomplete. It showed serial head repair,
fresh refill and retained future originals, with no ACK errors or withdrawal.

The existing-policy counterfactual setting
`DeferTimeoutResendWhileCumulativeProgress=false` completed all 5,112 messages
in 3.14 s from source start / 1.82 s after pause release. Closure
`shared-pump-sustained-no-progress-defer-C-qualification-r1`, SHA256
`01429b2f9404465d6534d6c939b9c85c19508fe04eaf5e40f4494e877d5486b4`.
This implicates that policy in this component contrast; it does not justify a
shipping default change. Healthy FIFO single-write controls must still pass.

The physical-backing arm subsequently ran all five windows at **248.2, 48.8,
99.2, 153.3 and 210.7 Mbps**. Ordinary RX and final funding refusals were zero;
provider timeout resends were 0/5/0/0/0. The first GET still took 5.349589541 s
against the original five-second gate. Closure
`window-rx35-fixed-qualification-r2`, SHA256
`65edd79d0ca0bbfd63eef83e36c1c9951a0790d1f41f1a1cb5e6a60be9856b48`.
Source/module audits, independent RX/common checks and cleanup passed. There
was no reroll or performance promotion. Extra backing alone is insufficient. Both slow probes spent almost all latency before the first
response body byte (5.349590 s and 4.940282 s); the remaining bodies/EOF took
0.179 ms and 0.077 ms. This frozen workload has no per-probe dial/TLS/header
observations, so that interval cannot yet be assigned to a queue or handshake.
Pure-ACK Pack-admission refusals persisted, but the retained first witness
belongs to a bulk connection and cannot attribute either slow GET. Evidence:
`window-rx35-fixed-qualification-r2/RX35-SLOW-GET-EVIDENCE.json`, SHA256
`ead5f5491b973dd091a3a7e4dd3a40f855273ad1f76a4c237f29eb05bf54ac05`.
The next aggregate workload must include the prepared GET phase capture.

## Bounded-deferral hypothesis: rejected by healthy controls

The advertised-peer two-deferral candidate reached runtime in
`shared-pump-advertised-deferral-C-qualification-r2`. Both mandatory public
healthy FIFO controls failed because an original was rewritten before its ACK.
The sustained default completed all 5,112 ACKs, but took **54.364 s** from source
start and failed its unchanged five-second bound. Slow-service, capped and
unpaused controls passed. The two new due-policy roots in both arms instead
failed setup: their sender lookup assumed lane zero while their actual wire
uses lane one. A one-line lookup repair is retained separately; those setup
failures are not a qualified old-code differential.

The healthy-control veto rejects this production predicate regardless of that
fixture repair. Do not rerun it just to manufacture an old/new red/green pair.
Closure SHA256 `c2e6f1ea37009904f44dd3af5ee49ea12a871f0eb80a835f2a3edb0c39321d25` records exact source/module pre/post audits and complete process/FD/lease cleanup. The next hypothesis
holds new ordinary DATA against a fixed existing recovery-cohort boundary,
while preserving protocol, ACK, retry, expiry and cancellation progress. It
requires its own deterministic qualification and aggregate measurements.

## Fixed recovery-cohort admission: measured improvement, still incomplete

The first executed fixed-cohort candidate held new ordinary DATA until its
fixed pre-existing recovery cohort retired. It kept retry clocks, advertised
credit, physical budgets and the original sustained workload unchanged.
Closure `shared-pump-fixed-cohort-C-qualification-r2`, SHA256
`0da45ec66f088555d7a35c06c090373b5afa7c82852bbe76576a18e3675b6ba9`,
records **20 candidate passes and six failures**, with exact source/module
pre/post audits and complete cleanup. Healthy FIFO controls passed.

The sustained default ACKed all 5,112 originals / 40,896 frames in **8.84 s from
source start / 7.52 s after pause release**. This is an improvement over the
previous incomplete 60 s default, but still fails the unchanged five-second
bound. Final refused originals fell to 200; physical retries were 209 (178
timeout and 31 selective). The remaining old-hole recovery is serial. Neither
this result nor the healthy passes qualify the candidate for adoption.

Five new public/prepared controls, and the old-code public mechanism root,
failed a fixture premise: fully acknowledging the warm original makes the next
original legitimately a Head. They did not exercise their intended promoted
non-Head cohort. The fixture is being corrected with a real retained anchor;
those setup failures are not an old/new root-cause proof. Seven state controls
and the existing protocol, NoAck and prepared guards passed.

The next cheap capacity discriminator is the same sustained workload with
physical RX backing 7,633,633 and encoded cap 2,181,038 under the unchanged
shared root. It must remain a separately named diagnostic control; the known
original-default failure stays visible. Any increased production allowance
still requires the 32/64 MiB device qualification.

## Qualified admission differential; remaining handoff boundary

The corrected real-anchor public test now **fails on old code and passes on
the fixed-cohort candidate**, after actual protocol/DATA ACKs and complete
custody cleanup. All five signed public/prepared controls passed on the
candidate. R3 closure `shared-pump-fixed-cohort-C-qualification-r3`, SHA256
`364784cd85c5a758d01b73b9d0f083d98f9f3a2aff57d71f9716c64f4c2556b7`,
records candidate **25 PASS / 2 FAIL**, with exact audits and cleanup.

The original default workload still failed its five-second bar despite all
5,112 ACKs: **9.14 s source / 7.82 s after release**, with 275 refused originals,
275 handoff drops and 286 retries (252 timeout / 34 selective). The changed hole
population versus the prior 200-refusal run shows that the concurrent workload
is not an exact deterministic milliseconds-per-hole decomposition. The signed
admission differential is the deterministic root-cause proof.

Both backed arms reached a different refusal boundary and stopped at an
inherited predecode-only fixture assertion. `PackHandoffDropCount` also records
post-decode zero-wait `ReceiveBuffer.Pack` refusals. That count alone does not
identify the count, byte or quota limit. A backed-only diagnostic continuation
must preserve strict zero loss, all ACKs and the five-second bar.

Source policy calibrates H1 count 64 at 24 MiB and scales it to floor(64*32/24)
= **85** at 32 MiB. The full RX35 run observed a consumer handoff maximum of 82
with zero handoff saturation/drop and receive-queue drops; its configured count
was not serialized. Its 60 aggregate link-queue drops started in window 2 and
are not attributed to the slow GET. Evidence SHA256
`857f1e6e27f2581f1f8ad0e7366ccfd4f9a09c9878ace1072dea49d3136b216e`.
The next focused run keeps backed85 and adds a separately named backed256
counterfactual. Neither changes a shipping policy or memory qualification.

## Existing feedback fields do not mean RX refusal

A source audit of `protocol/transfer.proto` and sender recovery handlers found
no current ACK field that means an exact original was never admitted.
`evicted_sequence_numbers` withdraws previously selective-ACKed custody; its
handler ignores unSACKed originals. `missing_contract_id` requests contract
recovery, including a full Head, and has different lifetime semantics.
`receive_window_byte_count` advertises capacity rather than current free credit.
`Pack.nack` requests no delivery ACK. A current receive floor also cannot prove
that every later original is absent: the diagnostic retained 71 tentative
future owners. Reusing those meanings as a generic refusal cue would be wrong.

A distinct bounded non-delivery cue would require explicit protocol design and
legacy handling. It is not implemented or justified as the next step: first
measure adequate physical backing with the corrected ACK path and unchanged
encoded credit. Existing ACK/coalescer machinery may be reusable later, but
custody and refusal semantics must stay separate.

## Historical pair: already executed

Retained binaries, now at
`/Volumes/data/urnetwork/temp/synthetic-smoke-r1/shared-pump-performance-r1/compile-r1/bin/`:

- R13 `baseline.test`: SHA256
  `22e6a005c39a9f75ea62f80a379eaeebe442793ecd71acf1184be1f5a3f182c3`.
- R14 `candidate.test`: SHA256
  `bec61e0201117cf2b5d17ea69411029d296b97ff67e0aec6026fe094e5f1e6b1`.

Replay directory:
`/Volumes/data/urnetwork/temp/aggregate-flow-r15-r2/shared-pump-retained-abba-r1/run`.
`FINAL.json` SHA256
`4e2cf9c1976062d1e9c60f7da299d94fae81b9fa7b1f37d832a7dc4f5ede3672`;
`ABBA-SCALARS.json` SHA256
`1fbab46e65541f09541dbe7b403abdecd2e3a75bfe0888bca99e9ef586b2fe6c`.
Source/module audits match; children joined, image closed and leases released.
`success=false` is retained: normal closure does not mean performance passed.

| Process | Five window Mbps | Provider timeout resends |
| --- | --- | --- |
| A1 R13 | 206.8, 257.2, 290.8, 32.3, 3.7 | 8, 4, 4, 1455, 1814 |
| B1 R14 | 3.1, 0, 4.8, 1.4, 0 | 459, 375, 1000, 721, 466 |
| B2 R14 | 6.1, 1.3, 1.1, 1.5, 2.3 | 2521, 1057, 454, 753, 545 |
| A2 R13 | 64.1, 0.2, 0.7, 0.8, 0.4 | 1322, 1038, 784, 574, 628 |

This is historical frozen-profile evidence, not current normalized-profile
qualification. The old runner records pre-relocation binary paths; use the
verified paths above for any future replay justified by a new question.

## Limits on the reviews' causal claims

- The near-limit boundary gauge includes RX, Head and Pack ownership. Its
  carry-over is real; the 64 MiB gauge alone does not prove ordinary RX is full.
  Canceled HTTP workers also do not identify every remaining Transfer byte as
  dead-flow DATA. The first refusal's empty held queue does not prove a
  contiguous ingress prefix without its identities.
- Slow TLS intervals and provider-coincident releases favor shared ordering,
  but the failing GET still needs its own flow/ordinal attribution. Neither a
  first TLS flight nor aggregate ACK-refusal counts prove that every relevant
  TCP right edge was unexhausted.
- The advancing-positive-ACK gap is now locally reproduced, superseding the
  reviews' “in principle” status. The corrected old-code fixture fails its
  responsiveness invariant after finite 40 ms pressure; retained DATA recovery
  occurs 958.818 ms after accepted owners drain. Exact-packet manual reoffer
  passes. Closure `native-positive-window-feedback-C-qualification-r2`, SHA
  `cee6589dfafb9b763db168e64c462632b28618cfc8f909814ff10fd04323af52`.
  This warrants its narrow correctness fix but does not attribute Fast5 or
  block the independent cap experiment.

The latest general-production-root loaded closure is
`native-window-retry-loaded-only-r1`, SHA
`402dfd989bb0c3a62f10c988b2c7ae618e0072c13f4a6e800eed16e0f129c7c3`:
Fast64 passes at 75.2/64.9/112.1/155.9/235.1 Mbps; Fast32 fails its window 2 GET
at 9.500678 s; both full100 profiles pass. The closed fixed-provider cap result
above does not supersede that broader production-root outcome. Sustained and
physical-backing arms must publish their own execution closures.

## 2026-11-11 UTC: RX backing and handoff depth are separate limits

A signed H1 sustained control sent 5,112 originals / 40,896 frames through one
80 ms receive pause, retaining the original five-second completion bar and
requiring all real ACKs, zero admission loss and balanced ownership. Encoded
permission stayed **2,181,038 bytes**, physical RX was **7,633,633 bytes**, and
the shared root stayed **21,810,380 bytes**. Head, Pack and independent TX
ceilings were unchanged. This is component evidence, not a measured SDK or
phone memory qualification.

| Handoff count geometry | Old product | Fixed-cohort product | Admission/recovery |
| --- | --- | --- | --- |
| 85 | 2.88 s source / 1.56 s post-pause | Same | 46 handoff refusals and selective retries; strict zero-loss FAIL |
| 256 | 2.84 s source / 1.52 s post-pause | Same | Zero refusal, drop or retry; PASS |

The first count-85 refusal occurred with actual count 85, **5,006,849 RX bytes
free**, and Pack quota 884,935 below its 2,181,038 ceiling. The decoded receive
callback stages both ran. Thus extra physical backing fixes the observed
latency collapse here, while an independent count ceiling still causes avoidable
loss. Matching depth to the permitted flight removes that loss; its observed
40 ms completion difference is not a statistical throughput promotion.

The separate count-256 fixture changes generic and H1 count fields before
construction; the exercised transport is explicitly H1. It is not yet a shipping
H1-only setting change. Its count is the next power of two above the measured
permission/payload group count plus a two-owner margin (215 -> 256). Smaller
packet groups and other profiles need their own bounds.

Closure: `shared-pump-backed-receive-boundary-C-qualification-r2`, SHA256
`d339dafb0e5fe23456e7922e065b60b2fda76c13dcfc9a5acc8ec3207190dea4`.
Source/module audits and complete process/FD/lease cleanup passed. Both products
had the same sizing result, so this comparison does not attribute the gain to
the cohort fix. Its separate old-code-fail/new-code-pass custody test and
under-backed recovery improvement remain distinct evidence.

Next: implement one aggregate SDK sizing candidate which gives physical RX
enough retained backing while explicitly preserving the encoded credit, Head,
Pack and immutable root. Existing SDK code copies RX capacity into Head recovery;
blindly multiplying RX would therefore change two permissions. Preserve legacy
peer behavior, validate actual 32/64 MiB constructor paths, then run aggregate
Fast5/full100 and on-device MEMSTEADY/PERFVAR. The iOS-profile **32 MiB** and
Android-profile **64 MiB** gates remain unqualified for this candidate.

### Counter semantics for the next replay

`ReceiveStats.ResendQueueUnackedItemCount` is a cumulative violation counter:
`addResendItem` increments it when an item expecting **no ACK** is incorrectly
placed into retention. It must remain zero. It is **not** the number of live
items awaiting ACKs; the older perfvar `ProviderUnacked` label copies this
counter and cannot prove an empty resend queue. Use actual owner/queue samples
when available, and label unavailable outstanding-owner data as unavailable.
The new normal receipt retains its already-sampled consumer Receive census;
consumer SendRecovery was not sampled and is not fabricated.
