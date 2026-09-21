# Memory-budget performance research

This is the working research document for converting bounded mobile memory
headroom into lower page TTFB and higher transfer speed while keeping the iOS
profile's Go runtime at or below a hard 24 MiB cap in every phase. Its subject is the allocation
of memory budgets: what each budget admits or retains, the performance mechanism
it funds, the marginal result per MiB, and how that memory is reclaimed when the
device changes roles. Update the hypotheses, measurements, and decisions here
as experiments run; `LOWBAR.md` remains the full validation history.

The central question is not "how can every limit be smaller?" It is: **given a
fixed 24 MiB iOS-profile runtime envelope, which movable bytes buy the most
useful TTFB and goodput across every live carrier?**
Reducing allocation churn creates spendable headroom; queue, root, carrier, and
topology budgets decide whether that headroom can do useful work.

## Physical Android device allowlist

Every MEMSTEADY device measurement is restricted to this exact allowlist:

| Role | Serial | Model |
|---|---|---|
| `device-a` | `3B161FDJG001KT` | Pixel 8 Pro |
| `device-b` | `R5CX21FY6ND` | Galaxy S24 Ultra |

Preflight must require both serials in `adb devices -l` with state `device`.
Per the audit owner's 2026-09-17 instruction, ignore all other attached serials
(including unauthorized or offline entries) and leave them untouched. Only the
two allowlisted phones belong to the audit. Drivers pass each serial explicitly; a missing,
unauthorized, or offline allowlisted device invalidates the block instead of
substituting another phone. Public notes use roles; the private manifest keeps
serials for reproducibility.

Verified 2026-09-04 with `adb devices -l`: both allowlisted phones were online
and no other attached serial was admitted to the memory cohort. The earlier
rule rejecting any additional attached serial was superseded on 2026-09-17;
additional devices remain outside the cohort and do not invalidate a block.

## Scope and acceptance signals

The current Android campaign is an **iOS memory-profile proxy**, not an audit of
Android's normal production memory allowance. The authoritative iOS profile in
`apple/app/extension/TunnelMemoryBounds.swift` passes a **20 MiB DeviceLocal
admission target** and a **32 MiB process/Go soft limit**. Its measured Go runtime
must never exceed **24 MiB**, including baseline, burst, drain, role transition,
and quiet recovery. These three quantities are different: the device target
sizes admission controls, the soft limit paces GC, and 24 MiB is the hard observed
runtime acceptance cap.

Normal Android builds retain their larger 28 MiB device target / 40 MiB process
soft limit. Audit APKs explicitly select the debug-only
`ios-memory-audit-v1` profile, reproduce iOS's 20/32 MiB inputs, and record the
profile, values, source revisions/patch hashes, and installed APK hash. Both
phones must report the selected profile and target; every diagnostic sample
must contain the expected 32-MiB `go_limit_bytes`. An ordinary Android-profile
APK cannot pass this campaign even when its sampled runtime happens to be low.
The historical 24-MiB target budget ledger below describes earlier calibration
arms; it must not replace the current iOS profile's 20-MiB admission target.

Android measurements establish proxy evidence about the shared Go runtime and
budget controls. They do not establish Android production-profile conformance
or replace the physical iOS `phys_footprint`/jetsam gate.

Performance comparisons remain carrier-specific: an H1 speed result is not
H3, DNS, alt, or extender performance evidence. Memory acceptance is no longer
H1-only, however. H1, direct H3, `H3Dns`, `H3DnsPump`, API `alt h3`, API
`alt whodis`, and the TCP+TLS, QUIC, and DNS extender carriers are live
production paths and all belong to this audit. Their direct inner carriers,
alt and extender outer carriers, QUIC receive windows and unacknowledged-send
retention, socket buffers, DNS translation/combine state, packet queues,
connection races, and bounded replacement overlap must be charged before
allocation to the shared carrier budget described below. The
receive-correctness contract is also carrier-independent: every exact reliable
stream/SCTP/framed-TCP lane preserves fixed-capacity backpressure, and every
true datagram lane stays bounded and zero-wait.

The performance goal is to recover the previously observed fast.com class of
40+ Mbit/s while making ordinary pages feel immediate. Each device run must
bracket the tunnel with a Direct measurement: a radio/ISP path below 40 Mbit/s
cannot prove that target, while a fast Direct result and a slow H1 result makes
the tunnel loss actionable. The checked-in 2026-08-23 record says fast.com
moved 40.4 MiB over H1 in about 60 seconds; that is traffic volume, not a
40.4-Mbit/s rate. The 40+ Mbit/s target comes from the prior product behavior
and must be re-established with elapsed-byte counters in the new campaign.

The primary memory signal is `goRuntimeBytes` from the SDK sampler. It is the
Go runtime's mapped/retained memory, not Android PSS and not iOS Network
Extension `phys_footprint`. The mobile acceptance rules are:

- every iOS-profile runtime sample, including baseline, active traffic, drain,
  role transitions, and five quiet connected minutes after a burst, must stay
  <= 24 MiB; report quiet p50/p95 as well as the maximum;
- any sample above 24 MiB fails and requires allocation attribution rather than
  a larger limit; there is no grace band above the cap;
- the carrier-budget matrix below passes at the actual 20-MiB device target and
  32-MiB process limit. A passing H1-only test or a test sized at the historical
  24-MiB target cannot certify this campaign;
- every carrier graph, including an unowned NetworkSpace API/feed/probe path,
  consumes the same process-root allowance. A fallback may be refused when the
  aggregate budget is full, but it may not allocate from a separate hidden
  allowance or be blanket-disabled merely because the iOS profile is active;
- no app, VPN, instrumentation, or carrier termination; all temporary clients
  and inner/outer carrier claims must be released;
- compare page document TTFB, page load, per-request p95, 1 MiB transfer rate,
  allocation growth, packet-pressure drops, roots, and live exit count. A win
  in only one metric is not enough.

Physical runs use the long-lived Android acceptance session on `zandroid`, a
fresh Chrome process with stable DevTools probes five seconds apart, cache
disabled for every sample, seven Wikipedia navigations, repeated Cloudflare
1-MiB fetches (ten in the final ACK-density arms), and a canonical fast.com
navigation with a 60-second trailing observation window. Device identifiers and credentials are
never retained in checked-in results.

## Memory budget and provider state

### Current iOS-profile admission ledger (2026-09-17)

The 20-MiB iOS DeviceLocal target now splits into 2 MiB of DNS, a 13-MiB
shared transfer/topology root, and 5 MiB of platform carriers. The transfer
root is stable across role changes; its children are overlapping admission
ceilings rather than additive reservations:

| Admission owner | Provider on | Provider off |
| --- | ---: | ---: |
| DNS | 2 MiB | 2 MiB |
| Shared transfer/topology root | 13 MiB | 13 MiB |
| &nbsp;&nbsp;Client send/receive, Pack, P2P, and peer identity pins | 9 MiB | 13 MiB |
| &nbsp;&nbsp;&nbsp;&nbsp;Fixed durable peer-pin child (inside client) | 1 MiB | 1 MiB |
| &nbsp;&nbsp;Provider send/receive and P2P child | 2 MiB | 640 KiB control floor |
| &nbsp;&nbsp;All fallback, remote, and retiring NAT generations | 2 MiB | 2 MiB |
| Shared platform carriers | 5 MiB | 5 MiB |

The child rows deliberately do not sum to 13 MiB. They describe which class
may borrow idle root capacity; every live reservation is charged atomically to
both its child and the one 13-MiB root. A role transition may leave old client
owners draining while provider or NAT work starts, but those generations may
not escape into independent pools or overdraw the root.

The mobile peer-identity pin store prepays **1 MiB inside the client group**
before allocating its fixed 256-entry table, 128-KiB-plus-one input/output
owner, path, or decode/serialization scratch. This is not an extra device or
process allowance: the iOS target remains 20 MiB, the process soft limit remains
32 MiB, and every accepted runtime sample must remain at or below 24 MiB.
The 28-MiB profile uses the same 1-MiB leaf inside its existing larger root.
At most **256 peers** and **128 KiB of persisted input/serialized output** are
supported; no pin is evicted. A full store can still verify/update an existing
peer, but refuses a new signed-pin commit before opening that session's Required cipher.
This cardinality is a fail-closed mobile persistence policy, not an LRU cache.

Missing input is empty only for a genuinely absent leaf in the existing
application-private directory. Unreadable, corrupt, oversized, symbolic-link,
and nonregular input fails DeviceLocal construction before provider creation.
Darwin/iOS and Linux/Android use no-follow, nonblocking open plus identity/type
validation, so a FIFO or leaf swap cannot block admission or redirect reads.
The private parent directory must have one LocalState owner; independent
processes/LocalState instances require externally joined ownership. A prepared
replacement reloads at publication, and independently generation-gated pin
ownership also covers empty-JWT devices, preventing stale-snapshot erasure.

Verified pin and global signed-history latch commit together through a
same-directory temporary file, sync, rename, and directory sync before the
in-memory state changes. Checked capacity/persistence/closed/superseded errors
withhold Cipher and emit the bounded **local pin-store unavailable** event,
never peer-key rejection/exclusion. A healthy store preserves the existing
network-fetch availability fallback. Legacy caller-supplied/server stores
keep their prior interface and ownership contract. The device-created store
is shared unchanged with the provider and every destination generation, and
its claim releases only after joined DeviceLocal teardown.

Host allocation regressions include a full 256-max-value-pin prepare plus
activation (about **909,000 bytes / 888 KiB**), a maximum-size
whitespace-padded valid file (about **176,000 bytes**), a hostile giant-key
refusal (about **821,000 bytes**), and a full-store replacement serialization
(about **51,800 bytes** beyond the fixed owner), all within the 1-MiB claim.
Input is validated before allocation-free in-place whitespace compaction;
otherwise a padded byte-array token made the standard decoder allocate
1,217,144 bytes across preparation/activation and failed this preflight.
These are fresh-process host allocation bounds, not device-footprint evidence.
`MemoryUsed` and the `memory_device_transfer` evidence part expose named
`peer_pin_*` budget/use/reservation/release and bounded count/error counters.
Budget root/child bytes are sampled atomically; logical counters are a separate
diagnostic sample and contain no peer identities or unbounded error strings.
The flightgate verifier requires a live 1-MiB pin claim at every sample, exact
reserve-minus-release balance, containment in both client and root usage,
at most 256 retained peers, and zero pin refusal/failure counters. It does not
require post-Close logging; host lifecycle tests prove joined release balance.

The NAT child now admits the provider's complete bounded topology, not only
its socket flows. Each budgeted `LocalUserNat` prepays 256 KiB before creating
contexts, maps, protocol buffers or workers. A budgeted provider prepays
480 KiB plus 4 KiB for each supported source, capped at 16 sources per
generation (544 KiB at the mobile default):

| Provider envelope partition | Prepaid bytes |
| --- | ---: |
| Four 8-KiB IPv4/IPv6 ingress/egress fragment caches, metadata and reconstruction | 128 KiB |
| Built-in DPI (64 flows), security stats (32 destinations per result), bounded snapshot copies | 96 KiB |
| Workers, channels, registrations, primary/transient source-retirement maps | 96 KiB |
| Two synchronous TCP return-callback/item workspaces | 64 KiB |
| Independent nonblocking ingress ACK/RST decode/group workspace | 32 KiB |
| Allocator/map-growth margin | 64 KiB |
| Lifecycle, ACK evidence, diagnostics, mode/priority maps and six protocol-leaf tombstones, 16 × 4 KiB | 64 KiB |
| Required SDK packet-stats registration, atomically admitted with the provider | 1 KiB |

Thus the supported worst overlap is one fallback NAT, one retiring provider's
local NAT, one replacement provider's local NAT, and both old/new providers
with their required stats subscriptions: **3 × 256 + 2 × (544 + 1) = 1858 KiB**.
It leaves **190 KiB** in the 2-MiB child for actual packet/flow admission.
The deterministic tests keep this graph live and complete a real UDP echo;
the SDK repeats the same graph and progress at the 20-MiB and 28-MiB targets
(the latter has a 2.8-MiB NAT child). These are overlapping claims against the
same root, never separate per-generation allowances.

TCP/UDP/ICMP flow envelopes, ingress packet roots, retained TCP return chunks,
asynchronous provider return items and packet/decode scratch are separately
admitted before their allocations. Each SMTP inspection flow prepays 160 KiB
for its bounded prefix, reusable TLS handshake buffer, heap-owned 8-KiB
extension bitset and metadata; concurrent inspectors cannot grow uncharged
caller stacks or allocate another flow after refusal. The NAT's 16-KiB
ACK/RST partition, provider's 64-KiB synchronous TCP return partition, and
independent 32-KiB ingress control workspace are already included in their fixed
claims, so replay pressure cannot consume the capacity needed to free replay
owners. The ingress workspace decodes one bounded ACK/RST frame at a time,
including controls in mixed Packs; a small owned copy prevents an ACK slice
from retaining its larger borrowed Transfer backing root. Additional stats registrations
claim 1 KiB each and remain charged through a captured callback after
unsubscribe.

Healthy idle source gates are reclaimed. Terminal tombstones and generations
with an unreachable-source retirement worker are not: at most 16 such source
identities and one primary plus 16 transient retirement owners can coexist
per provider. Saturation fails closed and triggers the existing SDK rotation;
it never evicts an authoritative tombstone to admit an unknown sender. The
fixed claim survives all workers, retirement owners and an outstanding
one-shot saturation handoff, including callback-initiated close. Packet-stats
callbacks use `RequestClose` for callback-safe shutdown; blocking `Close`
joins the stats worker and SDK calls it only outside the device state lock,
then merges the final post-drain counters once.

`TryNewRemoteUserNatProviderWithPacketStats` reserves the provider and required
subscription together, so a missing final 1 KiB creates no provider and cannot
self-trigger a build/retire retry loop. Capacity refusal defers construction;
permanent `NatMemoryPolicyError` for an opaque custom security factory does
not retry and releases the unused NAT. Unbudgeted/server policy factories and
configured source/queue defaults remain unchanged. Bounded construction,
source churn, dual-stack fragment pressure, SMTP concurrency, exact overlap,
callback capture and teardown have focused and race-detector regressions;
admission accounting is not a substitute for the independent runtime gate.

The 2026-09-17 host-only `TestDeviceLocalProviderMemoryUnderLoad` preflight is
still **failing**, not certified by those deterministic passes. Three candidate
fresh-process measurements peaked at 32.4, 32.5 and 32.1 MiB against the
unchanged 31-MiB Darwin ceiling. Clean detached exact-HEAD controls
(`connect` `b4aeac5b85e90821bba91a8353453115ead4cc2e`, `sdk`
`2943a312285be9d0d529622b738ceef6f98153ad`) also failed at 31.6, 32.3, 32.1
and 32.1 MiB. Both carried 208 idle / about 398 peak goroutines and about
10.3 MiB loaded heap. This six-in-process-gVisor-peer host signal therefore
has an exact-base failure and is not causal evidence of a provider-envelope
regression, but it is not a candidate pass either. Post-load sampled heap
profiles on both sides are dominated by runtime thread/goroutine allocation,
message pools and Transfer state; they show no candidate-specific provider
policy hotspot at the default sampling resolution and do not profile the
runtime peak itself. No ceiling was raised, and this check does not replace
the mobile all-sample 24-MiB runtime acceptance gate.

The 5-MiB carrier share is one aggregate per-DeviceLocal ceiling, not 5 MiB per
mode or per layer. Under the 32-MiB iOS process profile, every low-memory device
carrier budget is also a child of one 8-MiB / 16-carrier-slot process root. Standalone
NetworkSpace API, feed, probe, and extender claims attach directly to that root.
Thus two devices in one process, an inner carrier plus its extender, and an
Auto race cannot each spend an independent allowance.

Ordinary Android uses the same hierarchy rather than a separate code path: its
28-MiB device target yields a 7-MiB child and its 40-MiB process profile yields
a 10-MiB root (the 16-slot cap is unchanged). A slot admits one logical
PlatformTransport/carrier graph; it is not a raw file-descriptor count. Every
physical socket, concurrent dial candidate, and inner/outer layer in that graph
still contributes its full byte working set to the claim. Deterministic tests pin both
20/32 and 28/40. The physical acceptance threshold in this campaign remains
the stricter iOS-profile 20/32 inputs and all-sample 24-MiB runtime cap.

The 2-MiB DNS row covers DeviceLocal name-resolution/cache admission. It is not
an exemption for transport-over-DNS. `H3Dns`, `H3DnsPump`, and the DNS extender
must additionally charge their QUIC windows, UDP socket buffers, translation
combine state, fragment roots, and bounded packet queues to the 5-MiB child and
8-MiB process carrier ceilings. An extender's outer QUIC connection uses a
narrow bounded single-stream envelope; it must not silently duplicate the full
inner H3 window.

These are admission ceilings, not eager allocations or a proof that mapped
runtime fits. Runtime includes allocator spans, goroutine stacks, GC metadata,
and all simultaneously retained ownership, so the independent hard <=24-MiB
observed runtime gate still applies. The process GC soft limit is 32 MiB.

### Required live-carrier budget matrix

The mobile H3 claim includes **1600 KiB of fixed retained ownership**, additive
to connection receive credit:

| H3 non-receive ownership | Bytes |
| --- | ---: |
| UDP read and write socket envelopes | 2 × 64 KiB |
| Unacknowledged STREAM/DATAGRAM send roots and ACK/loss bookkeeping | 256 KiB |
| Bounded QUIC DATAGRAM queues and descriptors | 352 KiB |
| Application datagram reassembly and replay metadata | 96 KiB |
| TLS/QUIC control, stream, packet and worker envelope | 512 KiB |
| Application routes, hybrid stream queue, batching and held-message scratch | 256 KiB |
| **Fixed total** | **1600 KiB** |

The following are owner-target values, independent of the process budget that
parents the claim. A stream's receive credit is inside its connection credit,
not an additional allocation on top of it.

| Device target | Carrier child | Inner H3 claim | Connection receive credit | Stream receive credit |
| --- | ---: | ---: | ---: | ---: |
| iOS / audit Android: 20 MiB | 5 MiB | 3072 KiB (3 MiB) | 1472 KiB | 1104 KiB |
| Historical 24 MiB | 6 MiB | 3072 KiB | 1472 KiB | 1104 KiB |
| Normal Android: 28 MiB | 7 MiB | 5184 KiB | 3584 KiB | 2688 KiB |
| Finite 32 MiB owner | 8 MiB | 5696 KiB | 4096 KiB | 3072 KiB |

Without an explicit owner, `DefaultPlatformTransportSettings()` uses the
process target, so `MemoryBudget = 32 MiB` selects the 5696-KiB claim above;
the actual 20-MiB DeviceLocal uses `WithMemoryTarget(20 MiB)` and its 3072-KiB
claim under a 5-MiB child of the same 8-MiB process root.

This is a deliberate piecewise policy: targets through 24 MiB keep the
3-MiB inner claim and subtract the fixed envelope before advertising receive
credit. Above 24 MiB through 32 MiB, the claim instead includes the full
`target / 8` connection credit plus 1600 KiB. Unbudgeted and larger/server
profiles retain their historical receive-window policy; this finite-profile
ledger does not certify their unbounded working sets. Initial credit is also
owner-scoped: 128/256 KiB stream/connection on the finite profile and the
historical 256/512 KiB on larger owners, irrespective of process sizing.

The worst admitted iOS nesting is **3072 + 1408 + 2 × 144 + 256 = 5024 KiB**:
inner H3, outer QUIC, separate inner/outer DNS translation claims, and the
pending H1 carrier. It leaves **96 KiB** of the unchanged 5-MiB child. The
normal Android counterpart is **5184 + 1408 + 288 + 256 = 7136 KiB**, leaving
32 KiB of its 7-MiB child. Each live claim also draws on the process root.

The older THROUGHPUTFIX share table treated the entire H3 reservation as
receive credit. That receive-only equation is superseded on the finite mobile
surface, not restored by removing real retained owners. At the table's
200-ms carrier-loop design point and its existing framing factor, 1104 KiB
permits about **38.2 Mbit/s**, not the former 66/80-Mbit/s predictions for the
20/24-MiB targets; normal Android's 2688 KiB permits about **93.0 Mbit/s**.
These are arithmetic window bounds, not measured throughput or an acceptance
claim. Restoring the old 20-MiB connection credit while charging its fixed
owners would need a 4160-KiB inner claim and **6112 KiB** for the nested graph,
992 KiB beyond the child. The 20-MiB device target, 32-MiB process soft limit,
all-sample <=24-MiB runtime gate, and live-carrier progress tests are unchanged.

The deterministic gate and the Android campaign together cover both axes
below. The extender axis is the complete 4 x 3 cross-product of inner
`H1`, `H3`, `H3Dns`, and `H3DnsPump` with outer TCP+TLS, QUIC, and DNS
carriers (12 distinct arms), not three outer-only smoke tests. A mode counts
only when it opens the intended carrier, transfers bytes, and returns every
claim on normal close, dial failure, cancellation, and role teardown.

| Path under test | Ownership that must be admitted |
| --- | --- |
| Direct H1 | Inner TLS/TCP carrier claim and socket graph |
| Direct H3 | Inner QUIC connection/stream receive windows, UDP socket buffers, and stream/packet queues |
| `H3Dns` and `H3DnsPump` | Direct-H3 ownership plus the exact translation/combine, fragment, pump, and packet-queue bounds |
| API `alt h3` | Its process-root QUIC/socket claim, bounded HTTP/3 request-stream receive and unacknowledged-send retention, and request lifecycle |
| API `alt whodis` | The `alt h3` ownership plus bounded DNS translation/combine, fragment, and packet queues, all in the same process-root claim |
| Each of `H1`, `H3`, `H3Dns`, and `H3DnsPump` through the TCP+TLS extender | The selected inner claim plus the outer extender TLS/TCP claim for its full lifetime |
| Each of `H1`, `H3`, `H3Dns`, and `H3DnsPump` through the QUIC extender | The selected inner claim plus the narrow outer extender QUIC/socket receive and unacknowledged-send bounds and 2,050-byte framed-datagram scratch |
| Each of `H1`, `H3`, `H3Dns`, and `H3DnsPump` through the DNS extender | The selected inner and outer-QUIC claims plus bounded outer DNS translation/combine, fragment, and packet queues; an inner DNS mode retains its own separate translation claim too |
| Production Auto race and carrier replacement | Every simultaneous candidate; one explicitly paired H1-involved overage loan per level, bounded by that level's H1 overlap, with up to two distinct cross-level pair identities fully reflected in child/root evidence |
| NetworkSpace API/feed/probe without a DeviceLocal owner | A direct process-root claim; it must coexist or wait/refuse under the same 8-MiB / 16-slot root rather than escaping accounting or disabling restricted-underlay fallback |

Run the matrix with the exact 20-MiB device / 32-MiB process profile, not a
larger surrogate. For every row assert all of the following:

1. The complete claim is acquired before opening a socket, allocating a QUIC
   receive window, retaining QUIC data for send/retransmission, or retaining a
   DNS/packet queue. An admission refusal opens none of them and does not
   poison another healthy dial strategy.
2. Useful bytes make progress when the graph fits. In particular, first acquire
   the inner claim, then prove each intended outer carrier still fits; testing
   an outer claim alone misses a missized composite budget.
3. Outside a handoff, child use never exceeds 5 MiB and aggregate process use
   never exceeds 8 MiB or 16 carrier slots. Each level permits at most one
   H1-involved overage loan, bounded by its own reported old/new H1 overlap
   (at most 256 KiB and one logical slot in this profile). Independent carrier
   managers may leave two distinct pairs active across the child and root;
   both identities/classes/owners must cross-match in the primary and additional
   pair evidence. An additional pair proves ownership only: never sum its bytes
   or slots into the level's permitted overage. Ownerless root pairs are explicit
   and are not attributed to a device. Inactive evidence must be zero/empty;
   missing additional-pair fields from an older diagnostic build cannot pass.
   Multiple DeviceLocals and ownerless NetworkSpace work are exercised
   concurrently so an unparented budget cannot pass unnoticed.
4. Release, preemption, wake-up, and make-before-break handoff are balanced at
   both levels under ordinary and race-detector runs. Used bytes/count return to
   the pre-arm baseline after success, failure, cancellation, and teardown.
5. A deterministic forced arm proves the reported route identity for each row,
   opens its real loopback socket/QUIC/translation graph (a claim-only fake is
   insufficient), moves payload in both directions, and closes the path. In
   particular, `alt h3`, `alt whodis`, and every supported inner-mode ×
   extender-carrier composition are separate arms; success on one is not
   evidence for another. These arms are
   the mode-coverage gate; the phone block below is the production-Auto runtime
   gate and must report only the carrier it actually used.
6. The two-phone production-Auto blocks record process-root total/used bytes and
   carrier-slot counts in every diagnostic sample, exercise both provider/client role
   assignments, transfer payload, and remain under the all-sample 24-MiB runtime
   cap. Forced-mode results are reported separately so one winning fallback
   cannot be mistaken for coverage of the other live paths.
7. Every phone sample also records the 13-MiB transfer/topology root and its
   client, provider, NAT, and Pack subsets in a same-timestamp joined
   `memory_device_transfer` part. Root/NAT totals must be exactly 13/2 MiB;
   each stays within its cap and cumulative reserved minus released bytes
   equals current use. Every child stays within its own cap and root usage;
   child values are not added together or to the root a second time. Report
   baseline/peak/end use for all five budgets. Client, provider and Pack use
   must return to the pre-burst baseline at quiet-window end. For NAT and its
   parent root, also report the quiet minima and a timestamped recovery
   witness: a continuous interval of at least 20 seconds beginning no earlier
   than 30 seconds into quiet, with root and NAT strictly below their
   pre-burst baselines and every other transfer child at or below baseline.
   Diagnostic gaps over 20 seconds break the witness. An end value above
   baseline is accepted only after such a witness, with monotone balanced
   reserve/release counters proving new NAT admissions afterward and at least
   as many new reserved bytes at the root. Root excess cannot exceed NAT
   excess. Record the witness, post-witness reserve/release deltas and actual
   endpoint values; do not label them zero or discard later runtime samples.
   This distinguishes demonstrated aggregate burst recovery followed by new
   connected traffic from monotonically retained burst ownership. It does
   not identify individual flows: raw flow logs provide attribution, and the
   deterministic ownership/lifecycle gates remain required. A pre-burst,
   drain-only, early-quiet or single-sample dip cannot justify late growth.
   All-sample runtime/admission caps, carrier end checks, temporary-client
   release, traffic and five-minute coverage gates remain unchanged.
   The deterministic gate repeats this at the normal
   Android 28-MiB target, where the same ratios scale the root to 18.2 MiB and
   the NAT child to 2.8 MiB.

Classify any failure before changing a limit:

- **Budget escape:** retained ownership, a socket, or a queue exists before its
  claim, after its release, or outside both the DeviceLocal child and process
  root. Add ownership/accounting and a lifecycle regression; do not hide it by
  enlarging a budget.
- **Missized budget:** the complete required graph is accounted, but a live
  production path cannot fit the 5-MiB child or the shared 8-MiB root under the
  exact 20/32 profile. Reconcile the full inner-plus-outer working set and the
  ledger together; testing either layer alone is insufficient.
- **Inefficient algorithm:** accounting is complete and within its admission
  ceilings, but actual retained/runtime memory crosses 24 MiB or useful work
  cannot progress at a reasonable rate. Remove duplication, reduce retained
  roots/topology, or change the algorithm; admission bookkeeping alone is not
  a fix.
- **Unobservable result:** route identity, child/root accounting, payload
  progress, or lifecycle balance is absent or internally inconsistent. Treat
  the arm as invalid evidence and repair telemetry before interpreting memory.

### Historical 24-MiB admission calibration

The following research ledger records earlier 24-MiB DeviceLocal calibration
and a superseded share split. Keep it as history; the current iOS proxy uses the
20-MiB ledger immediately above.

At a 24 MiB target the SDK divides the tracked budget into 2.4 MiB DNS,
16.8 MiB client, and 4.8 MiB provider shares. These are admission ceilings,
not eager allocations.

When providing is enabled, the client transfer pair uses 16.8 MiB; half of the
provider share backs the provider client's transfer pair and half sizes the
provider egress NAT. When providing is disabled, the 4.8 MiB provider share is
currently folded into the client transfer pair, making its ceiling 21.6 MiB
(approximately 9.26 MiB resend and 12.34 MiB receive). The provider control
client remains alive so network peer/control state still works.

Merely increasing queue admission ceilings does not guarantee lower TTFB or
higher speed. The performance tier must spend provider-off headroom on a knob
that removes an observed H1 bottleneck, and it must return to the tighter
profile before or while provider work is admitted. Candidate uses are:

1. More H1 quality exits. This increases route choice and parallel-flow
   resilience and can reduce the two-second re-race tail. It also adds a
   persistent client/transport graph, so provider-on transition memory must be
   measured.
2. Byte-accounted packet ownership with an H1/provider-off ACK reserve. A
   256-byte pool class makes ordinary TCP ACK roots cheap while the 1-MiB base
   ceiling still rejects data pressure. The measured reserve ends at 2 MiB;
   3 MiB removed ACK drops but made bulk and Pack handoff performance worse.
3. Larger bounded per-flow packet groups. This amortizes parsing, locking, and
   transfer scheduling, but increases short-lived ownership. It should be
   changed dynamically with provider state, not by raising the global mobile
   ceiling.
4. More ready-only H1 WebSocket batching. It can reduce TLS/socket writes
   without adding a batching wait. The retained writer drains at most 32 ready
   messages, stops ordinary data at 12 KiB, and retains the same fixed 16-KiB
   wrapper. The count increase therefore targets ACK-sized bursts without a
   larger buffer.
5. Pool and GC tuning. Pools reduce allocation and GC churn only when their
   retained floor is bounded. Raising `GOGC` is not acceptable by itself:
   prior `GOGC=50` device arms reached 28.41--29.95 MiB. The accepted mobile
   pacing remains `GOGC=25` unless a complete physical run proves otherwise.

### Historical budget ledger

The limits below are different kinds of budgets. An admission ceiling does not
allocate its full value; a retained floor does. Treating them as equivalent
would overstate both the cost of a candidate and the memory it can reclaim.

| Budget at the 24 MiB mobile target | Current value | Allocation behavior | Performance mechanism |
| --- | ---: | --- | --- |
| DNS share | 2.4 MiB | Shared cache/in-flight admission ceiling | Deferred this iteration. Do not borrow it until DNS is reworked. |
| Base client share | 16.8 MiB | Shared resend/receive admission ceilings | Allows active H1 transfers to retain ordered and retransmittable work. |
| Provider share | 4.8 MiB | Movable admission budget | Provider on: provider transfer pair + egress NAT. Provider off: currently all added to client queues. This is the main performance research budget. |
| Provider-off client pair | 21.6 MiB total | About 9.26 MiB resend and 12.34 MiB receive ceilings; not eager heap | Prevents individual flow queues from being the first aggregate limit, but does not bypass per-flow or packet-root gates. |
| Mobile Pack handoff | 1.68 MiB provider-on at the 24-MiB target (1.5-MiB floor); 2 MiB provider-off | Shared retained-byte admission ceiling across all flows; no eager allocation | Absorbs a bounded H1 reader/worker scheduling burst without multiplying a per-flow floor. |
| Mobile receive reorder | 1.68 MiB provider-on at the 24-MiB target (1.5-MiB floor); 2 MiB provider-off | Shared retained-allocation ceiling across all flows; payload remains subject to the independent 768-KiB per-flow protocol window | Charges encrypted outer roots, decoded message/contract roots, packet roots, and the owner envelope while preserving the useful logical receive window. |
| Platform carrier budget | 8 MiB in the Android surrogate | Process-wide carrier admission ceiling derived from the 32 MiB Go soft limit | An H1 carrier claims 256 KiB. The H1 q4 control used 5.5 MiB including the always-live provider control transport, leaving structural room for more H1 candidates. |
| Mobile packet-root gate | 1 MiB base; 2 MiB H1/provider-off ACK maximum | Samples exact owned bytes for the 256-byte and 2,048-byte packet classes | Prevents an asynchronous ingress wave. Data, SYN/FIN/RST, H3/Auto, and provider-on traffic stop at 1 MiB; only exact ACK-only TCP packets can use the second MiB. |
| H1 quality/speed topology | 4 / 1 | Persistent clients, goroutines, queues, and carrier claims | More healthy choices can reduce two-second re-races and spread parallel flows. |
| Per-flow packet transaction | 16 packets / 24 KiB | Short-lived ownership bound | Larger groups amortize parsing/locking/scheduling and can improve goodput if the root gate is not already dominant. |
| Packet pool warm floor | 256 KiB total, split small/full | Eagerly retained after reclaim | Avoids the next cold allocation/GC wave without preserving the burst high-water. More floor buys reuse but directly raises steady memory. |
| Go runtime soft limit | 32 MiB | GC/runtime control, not an allocation reservation | Emergency pacing boundary. It is deliberately above the 24 MiB steady acceptance target and must not be mistaken for permission to retain 32 MiB. |

### Provider-off spend ledger

The 4.8 MiB provider share is the movable source. The present policy assigns
most of it to queue admission, while the exact packet-byte gate and per-flow
group can stop traffic before those larger queue ceilings become useful.
Research candidates therefore reassign *effective* headroom without allowing
the sum of active working sets to escape 24 MiB:

| Candidate spend | Provisional charge | Expected return | Reclaim rule |
| --- | ---: | --- | --- |
| Two additional H1 quality exits (q4 -> q6) | 512 KiB of carrier claims plus measured client graph cost | Fewer empty/benched candidate fields, lower resource-tail TTFB, better parallel-flow placement | Shrink to q4 when provider enables; measure the drain transition because flow-bearing exits cannot be destroyed synchronously. |
| 256-byte packet class + H1 ACK ceiling 1 -> 2 MiB | At most one additional MiB, available only to ACK-only TCP roots | Preserve the browser ACK clock during overload without admitting another data burst | Disable immediately for Auto/H3 or provider-on; admitted roots drain naturally. The 3-MiB arm is rejected. |
| Packet group 16/24 KiB -> 32/48 KiB | At most +24 KiB per concurrently admitted group, plus slice metadata | Fewer group transactions and transfer scheduling calls | Atomic limit swap on provider-on; already-admitted groups drain. |
| H1 ready count 16 -> 32 | No additional buffer: byte stop remains 12 KiB and wrapper remains 16 KiB | Fewer TLS/socket writes for already-ready ACK-sized messages | Retain globally for H1; it cannot wait for another message and the byte bound continues to yield to control traffic. |

Provisional charges are experiment bounds, not accounting facts. Each arm must
record runtime delta, live-heap delta, retained-pool delta, carrier claims,
roots, and exits. The accepted spend is the smallest budget that reaches the
performance plateau; unused provider-off share remains safety margin.

### How to rank a memory spend

For each isolated arm, compute changes against an immediately adjacent H1
control on the same underlay:

- `TTFB return / MiB` = reduction in median main-document TTFB divided by the
  increase in active runtime MiB;
- `tail return / MiB` = reduction in median per-page request p95 (and p95 load
  when enough samples exist) divided by active runtime MiB;
- `speed return / MiB` = increase in median Mbit/s divided by active runtime
  MiB;
- `pressure efficiency` = completed ingress bytes per pressure drop and per
  MiB of allocation growth;
- `steady tax` = change in five-minute runtime p50/p95 after roots and flows
  drain.

Reject a spend that only converts transient ownership into retained steady
memory, improves the median by worsening failures/tails, or requires crossing
24 MiB. Prefer a budget that can be lowered live and stops new admission
immediately over one that requires destroying active flows.

### Throughput-cliff investigation

The first hypothesis was that the former 512-root aggregate gate rejected phone
TCP ACKs while remote payload already occupied full-MTU roots. A deliberately
non-shippable 4,096-root diagnostic disproved that as the primary limiter:
pressure drops fell to zero and roots reached 763, yet median 1-MiB goodput was
only 0.63 Mbit/s versus 0.73 Mbit/s for the adjacent control. The gate is now
byte-accounted instead. Its 1-MiB base admits the largest ordered prefix, a
256-byte pool class charges small ACK/control roots accurately, and explicit
H1/provider-off traffic may rescue ACK-only packets up to 2 MiB.

That policy removed all ACK drops in the ten-transfer density phase and held
active runtime to 20.30 MiB, but median Cloudflare goodput was still only
1.695 Mbit/s. Raising the ACK maximum to 3 MiB also removed ACK drops, yet
median goodput fell to 0.92 Mbit/s and Pack handoff drops rose from 47 to 79.
The 3-MiB arm was reverted. ACK admission is useful overload protection, but
neither root count nor another MiB explains the Direct-to-H1 gap.

The mobile-wide sequence-depth experiments did find one real client limiter.
Global depth 32 and 64 improved pages, but receive-only/send+receive depth 128
did not reproduce the bulk result and crossed about 25.05 MiB under repetition.
Carrier-attributed telemetry then showed the H1 receive handoff reaching all 64
slots while the Transfer-ACK handoff had zero drops. The retained split gives
only H1 receive 64 messages / 128 KiB and lossless full-queue backpressure to
connection/sequence cancellation; send, Transfer-ACK, H3, forward, contract,
and control queues remain at 16. The count, logical-byte, and shared exact-byte
gates remain unchanged, so lossless means preserving an already-admitted
reliable message rather than allowing an unbounded queue.

The remaining bulk limiter is primarily provider-side. Same-session Direct
downloads reached 39--42 Mbit/s after the accepted run and 80--92 Mbit/s after
the NoAck diagnostic, while public H1 stayed near 1--2 Mbit/s. Local provider
grouping and direct TCP-ACK application remove measured message amplification,
allocation, and scheduler work, but public-device throughput cannot improve
until those changes run on a controlled/deployed provider.

### Iterative H1 receive deepening

An opt-in Connect diagnostic now tests the narrower hypothesis that only flows
which repeatedly fill their H1 Pack handoff should earn more depth. A flow
starts at 64 messages / 128 KiB. Two distinct full observations within 100 ms
earn one 16-message / 32-KiB step, up to 128 messages / 256 KiB. A lapse beyond
the window resets the saturation streak. H3, Transfer ACK, send, forward,
contract, and control traffic cannot deepen. The channel reserves pointer slots
to the configured hard maximum, but queued Pack ownership is still admitted
incrementally under the per-flow logical limits and the unchanged shared exact
retained-byte budget. Telemetry records saturation episodes, granted steps,
deepened flows, and maximum earned count/byte limits.

The first physical arm grew counts without growing the 128-KiB logical-byte
allowance. Two flows earned four steps in aggregate, but the largest flow
stopped at 92 queued Packs and 130,978 / 131,072 logical bytes; its maximum
earned count was only 96. This identified the fixed byte allowance as the next
local boundary, not a memory failure: runtime peaked at 21.39 MiB and recovered
to a 19.86 / 20.24-MiB p50/p95.

The paired count-and-byte arm removed that ambiguity. One flow earned the full
128-message / 256-KiB allowance and actually queued 128 Packs / 194,688 bytes.
Runtime still passed at a 22.45-MiB peak with a 20.58 / 20.91-MiB recovery
p50/p95 and no sample above 24 MiB. Performance did not improve: ten Cloudflare
1-MiB transfers had a 1.18-Mbit/s median, and fast.com moved only 1.77 MiB in
75 seconds (about 0.20 Mbit/s) while the adjacent Direct 4-MiB median was
87.4 Mbit/s. All four depth grants occurred during the Cloudflare phase; the
fast.com phase triggered no further growth. Timeout resends reached 620 across
the session. This is direct evidence that an H1 flow can consume the full
client receive allowance without unlocking the public-provider path.

The generic mechanism and schema-11 telemetry remain available for a fixed
provider A/B, but the production <=24-MiB mobile policy explicitly clears all
adaptive fields and stays at 64 / 128 KiB. Reaching 40 Mbit/s now requires
deploying the existing provider-return logical grouping and direct established
TCP-ACK application to a controlled nearby provider, pinning the device to that
exit, and alternating old/new provider binaries. Instrument client-to-server,
server-to-provider, and provider-to-origin goodput, frames/message, socket-read
batch size, CPU, route-write waits, and timeout resends; tune the first measured
boundary below 40 rather than buying another client queue.

The mechanism itself is not a hot-path performance regression. Ten 500-ms
uncontended Pack samples measured fixed/adaptive medians of 58.22/58.20 ns with
zero allocations. Server/default settings leave adaptive depth and retained
Pack scanning disabled. The final complete benchmark-only server tiers passed
190/10/20 samples: production full-payload/ACK-sized H1 TLS medians were
1,032/419.1 ns per frame with 17/10 B/op and two allocations, PERFVAR receive-
credit median was 783.1 ns, and proxy batch-64 median was 6,426 ns. These are
consistent with the adjacent accepted cohort; no server-specific reclaim or
depth override is indicated.

### Reliable-H1 synthetic-loss root cause

A pinned controlled provider finally exposed the first hop whose byte rate
diverged. The host's adjacent direct fast.com result was 1.3 Gbit/s, so neither
the origin nor host uplink was the ceiling. With the accepted fixed H1 receive
depth restored (64 configured slots, eight per negotiated nonzero lane), the
controlled Android run displayed 6.1 Mbit/s. During that single burst:

- the Android platform WebSocket reader refused 530 complete Transfer messages,
  totaling 1,186,363 bytes, because its bounded 32-message route was full;
- only two messages were later lost at the ReceiveSequence Pack handoff;
- the exact shared receive-reorder queue then pinned 1.993 of 2.000 MiB behind
  the resulting holes;
- the provider produced 1,357 timeout and 348 selective-gap retransmission
  writes while sending about 4.15 MiB of new return data; and
- Android Go runtime was 18.38 MiB, proving that this was a liveness/throughput
  failure inside an otherwise safe memory envelope.

This changes the diagnosis. The zero-wait WebSocket-reader handoff was
manufacturing packet loss *above* an ordered reliable TCP stream. Every later
message could arrive successfully yet remain unusable behind the missing
sequence number, and Transfer recovery had to enqueue duplicates behind the
same new traffic. Increasing Pack or reorder depth only stores a larger blocked
tail. The first corrective experiment keeps the channel capacity unchanged and
retains only the one message already read and already charged to the carrier:
when the H1 route is full, the reader waits for route space or connection
cancellation and lets TCP apply backpressure to the peer. The adjacent audit
found the same correctness boundary on H3/DNS QUIC streams and P2P SCTP, while
H3 DATAGRAM and native P2P remain deliberately nonblocking. This generalization
does not claim an H3/DNS performance win.

The first carrier-only candidate proved both the correction and the next
boundary. H1 carrier drops fell from 530 / 1,186,363 bytes to zero, with 25
bounded backpressure observations totaling 52,178 bytes. The unchanged 10-ms
ReceiveSequence Pack wait then dropped 24 messages (versus two in the adjacent
baseline), the reorder queue again pinned 1.994 MiB, provider recovery reached
1,544 timeout plus 564 selective writes, and fast.com displayed 3.5 Mbit/s.
This is not a reason to restore the upstream drops: it is the same synthetic
loss contract one hop later. The second candidate therefore uses the existing
negative reliable-lane handoff setting to wait until Pack capacity or
cancellation. It
does not add a slot or byte; per-sequence count/byte gates and the shared exact
2-MiB Pack budget remain the ownership ceiling. Exact H3/DNS stream and SCTP
handoffs use the same cancellation-bounded rule; H3 DATAGRAM, native P2P, and
unknown custom lanes stay zero-wait. H1 ACK handoff retains its separate compact
coalescing path.

The combined lossless H1 pipeline removed the cliff. Three consecutive
canonical fast.com runs through the pinned provider displayed 38, 41, and
52 Mbit/s. The runs completed in about 19.8, 48.7, and 14.8 seconds; the latter
two moved about 70.2 and 73.2 MB of new provider return traffic. After all three
runs and a seven-page cohort, Android had recorded 262 carrier-backpressure
observations / 687,039 bytes, zero carrier drops, and 762 Pack waits / 762
successes with zero Pack drops. The shared Pack and receive-reorder queues both
drained to zero. Go runtime peaked at 17.60 MiB, with no sample above 24 or
28 MiB. Provider selective-gap writes rose by 11 during the first run and zero
during each later interval; timeout recovery remains measurable (550 and 627
writes in the first two runs, then 253 across the third run plus page cohort)
but no longer pins a 2-MiB unusable tail. This is the first
controlled physical result to restore the requested 40-Mbit/s class without
buying queue depth or memory.

Page latency did not pay for the throughput win. All seven Wikipedia
navigations succeeded; median load/document TTFB/request p95 were
439.2/181.5/183.58 ms. The one fresh-connection sample loaded in 1,019.3 ms;
the six reused-connection samples loaded in 404.6--517.0 ms with no failed
requests. During the following 345-second quiet connected window, runtime
p50/p95/range/last were 17.57/17.61/17.23--17.61/17.41 MiB. Pack and reorder
ownership were zero in every recovery sample, retained pools peaked at
0.50 MiB, and neither forced GC nor idle trim ran.

Shared server performance remains neutral. Five 300-ms repetitions of every
benchmark in `server/connect`, `server/connect/perfvar`, and `server/proxy`
passed 190/10/20 samples. Production full-payload/ACK-sized H1 TLS medians were
896.2/373.0 ns with 17/10 B/op and two allocations, about +0.5%/+0.7% versus
the adjacent recorded 891.7/370.3-ns cohort. PERFVAR receive-credit improved
673.6 -> 636.8 ns and proxy batch-64 improved 5,406 -> 5,348 ns, with unchanged
allocation shapes. Those sub-percent H1 movements are process noise, not a
server regression. The DB-backed H1 PERFVAR correctness track was attempted
with its documented environment and remains externally unavailable because
the local Redis fixture is down; it is not counted as a passing gate.

### Cross-carrier remediation fast.com regression bracket

The exact-lane remediation was measured on the attached Pixel with explicit H1,
provider work disabled, a fresh authenticated process and Chrome process per
arm, two stable DevTools probes five seconds apart, cache disabled, and three
canonical fast.com runs. The order deliberately bracketed the retained
pre-change SDK AAR with the current build:

| Arm | fast.com displays | Median | Exact H1 ingress | Go runtime peak | Reliable-handoff result |
| --- | --- | ---: | ---: | ---: | --- |
| Current opening | 8.7, 6.9, 7.3 Mbit/s | 7.3 Mbit/s | 43.80 MB | 18.73 MiB | 19/19 Pack waits; zero Pack/route drops |
| Retained pre-change AAR | 84, 95, 1.2 Mbit/s | 84 Mbit/s | 223.70 MB | 19.25 MiB | 154/154 Pack waits; zero Pack/route drops |
| Current closing | 160, 130, 140 Mbit/s | 140 Mbit/s | 504.00 MB | 20.21 MiB | 5,769 route backpressures / 8,483,576 bytes; zero route drops; 1,804/1,804 Pack waits and zero Pack drops |

The public provider was not pinned across the three fresh clients, and the
opening-current plus baseline outliers expose that route variance. Do not use
the 140-versus-84 medians as a claimed product speedup. The closing current arm
is nevertheless a valid regression gate: it carried all three runs above the
40-Mbit/s target, exceeded the bracketed baseline median, moved 504.00 MB on the
H1 counter rather than leaking Direct, drained Pack/reorder use to zero, and
stayed below 24 MiB. Thus the lane remediation does not impose a systematic H1
fast.com ceiling. A precise percentage comparison still requires a pinned
provider and alternating old/current binaries on the same exit.

Keep this gate paired with queue evidence. For a future release candidate, run
at least three baseline and three current canonical samples, bracket each arm
with exact carrier counters, preserve every outlier, and reject a candidate
that cannot reach 40 Mbit/s on a path whose adjacent baseline can, or that
creates any reliable route/Pack drop. A displayed speed alone is insufficient
if traffic is not attributed to H1 or the fixed memory/ownership queues do not
drain.

The final pull added only generated IP-security and blocker data, but the
physical artifact was rebuilt and bracketed again so that result was not
assumed equivalent. On the now-degraded public route, the first three current
displays were 0.58, 27, and 52 Mbit/s (27 median); three preserved extension
samples were 17, 8.1, and 13. The retained AAR then displayed 28, 15, and 10
Mbit/s (15 median), and closing current displayed 0.65, 17, and 10 (10 median).
The current opening/retained/closing exact H1 deltas were 302.43 MB across six
runs, 147.21 MB across three, and 64.37 MB across three. Runtime peaks were
19.66, 17.70, and 19.89 MiB. All three arms had zero carrier and Pack drops;
current opening completed 165/165 Pack waits and current closing 38/38. The
same slow route also held the baseline below 40, while the candidate still
produced the only above-40 sample. Treat this as a neutral route-limited
current--baseline--current bracket, not as a replacement for the earlier
140-Mbit/s closing target gate. Its 14,057/330/6,926 timeout-resend counts expose
severe changing provider conditions and make a percentage comparison invalid.

The post-remediation server performance gate is also neutral. Before the pull,
all 210 `server/connect`, 10 PERFVAR, and 20 proxy samples passed at five
repetitions / 300 ms. After the generated-data rebase, the exact 30 affected H1
and queue-admission samples passed again. Production full-payload and ACK-sized
H1 TLS medians were 884.2 and 366.7 ns/op with unchanged 17/10 B/op and two
allocations, -1.34%/-1.69% versus the adjacent 896.2/373.0-ns cohort. The
pre-rebase PERFVAR receive-credit was 673.4 ns (+5.75%) and unchanged proxy
batch-64 was 5,444 ns (+1.80%); the small mixed directions are host noise rather
than a shared regression. Post-rebase direct hot-path benchmarks measured
reliable/unreliable fixed-queue admission at 31.17/32.67 ns and the complete
ResidentTransport admission wrapper at 40.47/38.06 ns, all with zero bytes and
zero allocations per operation. Reliable admission does not charge the ready
path for its cancellation-bounded full-queue behavior.

The deterministic root-cause gate is intentionally smaller than an Internet
benchmark and now covers the complete lane matrix:

1. Publish reliable-stream and DATAGRAM siblings under one H3 family and prove
   RouteManager returns the exact lane reliability with each message.
2. Fill a one-slot platform route. H1 and every H3/H3Dns/H3DnsPump QUIC stream
   must retain the exact second pooled message until capacity or cancellation;
   the corresponding DATAGRAM lanes must refuse promptly and return ownership.
3. Fill the ReceiveSequence handoff. H1, H3/DNS stream, P2P SCTP, and framed
   server routes use the cancellation-bounded reliable setting; H3 DATAGRAM,
   native P2P, and unknown/custom routes remain zero-wait. An artificial marker
   after a full H3 stream queue must emerge in order with no manufactured gap.
4. Split hybrid H3 without multiplying memory: the reliable route is
   unbuffered while DATAGRAM owns the existing payload queue. Stream-only H3
   retains the historical queue; the sum of route slots is unchanged.
5. Exercise P2P separately. SCTP must block only on its exact reliable route
   and return ownership on cancellation; native SRTP must keep its bounded
   nonblocking queue and counted drop. Constructor and connection updates must
   publish two immutable physical receive routes.
6. Saturate every reliable server socket/exchange boundary. It must propagate
   fixed-queue backpressure or retire the generation—never return a pooled frame
   and continue. Preserve the exact server H3 send cutoff through the gob
   exchange so only messages actually eligible for DATAGRAM consume unreliable
   flight.

The primary Connect cases are
`TestMultiRouteReaderReportsExactHybridReceiveLane`,
`TestPackHandoffTimeoutUsesExactReceiveLaneReliability`,
`TestReceiveSequenceReliableH3SaturationPreservesOrder`,
`TestPlatformTransportH3StreamLanesBackpressureWithoutDropping`,
`TestPlatformTransportH3DatagramLanesRefuseWithoutWaiting`,
`TestP2pSctpReceiveBackpressuresAndCancelsWithOwnership`, and
`TestP2pFastReceiveRefusesFullQueueWithoutWaiting`. The cutoff and memory gates
are `TestH3DatagramTransferFrameLimitMatchesLaneSelection`,
`TestTransferCarrierHybridSendCutoffClassifiesOnlyDatagramFrames`, and
`TestPlatformH3ReceiveLaneSplitKeepsOnePayloadQueue`. Run the isolated Connect
gate with:

```sh
go test . \
  -run 'Test(MultiRouteReaderReportsExactHybridReceiveLane|PackHandoffTimeoutUsesExactReceiveLaneReliability|ReceiveSequenceReliableH3SaturationPreservesOrder|PlatformH3ReceiveLaneSplitKeepsOnePayloadQueue|PlatformTransportH(1Receive.*|3(StreamLanesBackpressureWithoutDropping|DatagramLanesRefuseWithoutWaiting))|P2p(SctpReceiveBackpressuresAndCancelsWithOwnership|FastReceiveRefusesFullQueueWithoutWaiting)|H3DatagramTransferFrameLimitMatchesLaneSelection|TransferCarrierHybridSendCutoffClassifiesOnlyDatagramFrames|ProductionCarrierReadersUseModeSpecificReceiveAdmission)' \
  -count=50
```

Run the server generation/cutoff gate from the Server repository with:

```sh
go test ./connect \
  -run 'Test(SendPooledReceive.*|ReliableExchangeQueueSaturationPreservesFramedOrder|ExchangeGenerationRetiresAfterAnyUndeliveredFrame|ResidentTransportReceiveAdmissionUsesExactLane|ProductionSocketReadersDeclareExactReceiveLanes|ResidentForwardCallbackRetiresFullIngressWithoutWaiting|ExchangeHeaderUnreliableTransferGobCompatibility|ResidentTransportConstructorCarriesTransferProperties|ConnectH3TransferCarrierEnablesBoundedAckReserve)' \
  -count=50
```

Then run the SDK policy/telemetry gate from the SDK repository:

```sh
go test ./... \
  -run 'TestMobileLowMemoryClientSettingsBoundOwnership|TestTakeMemorySamplesJsonIsOneValidBatch|TestMobileDeviceMemorySampleHotPathDoesNotAllocate' \
  -count=10
```

These tests use filled one-slot queues, explicit release/cancellation barriers,
exact pooled-slice ownership, artificial sequence markers, and counters; they
need no Internet timing or scheduler race to reproduce the boundary. On a
physical H1 acceptance run, sampler schema 11 must show zero delta in
`platformH1ReceiveQueueDropCount`; a nonzero
`platformH1ReceiveBackpressureCount` is expected load evidence, not loss. Also
require the Pack-drop delta and final reorder bytes to return to zero, compare
provider timeout/selective recovery per MiB, and preserve <=24-MiB active and
five-minute steady runtime. A faster displayed result without those queue and
recovery invariants is not an accepted fix.

## Allocation findings and low-churn candidate

A 64 KiB-sampled diagnostic profile before this H1-only pass found these
short-workload allocation sources:

- `sdk.(*IoLoop).run`: 6.34 MiB flat / 12.11 MiB cumulative. Its local
  `[64][]byte` packet-slice storage escaped once per native read burst.
- `maps.clone`: 6.21 MiB, including 6.14 MiB from
  `sequenceAckWindow.Snapshot`. The ACK worker cloned the selective-ACK map to
  ask whether work was pending and cloned it again to consume it.
- message pool take: 3.31 MiB in-use; decoded packet-owner take: 0.63 MiB
  in-use and 3.02 MiB allocation-space.

The retained implementation hoists the TUN packet-slice storage out of the read
loop, copies each native packet into its exact 256-byte or 2,048-byte pooled
class, uses allocation-free ACK `Pending`/`Notify` checks, preserves the
borrowed outer-slice contract at the asynchronous NAT boundary, and admits the
largest ordered prefix that fits the 1-MiB packet-byte gate instead of dropping
an entire native batch. Pool telemetry now reports outstanding packet bytes as
well as roots, so admission observes allocation cost rather than treating a
60-byte ACK like a full-MTU packet.

The provider TCP path exposed another per-ACK allocation: every pure ACK built
a `TcpSendItem`, crossed the per-flow channel, and waited for the flow worker
even though it consumes no TCP sequence space. Established-flow pure ACKs now
apply their monotonic ACK/window/timestamp update directly and wake a blocked
socket reader. Pre-handshake packets and every SYN/FIN/RST/payload packet retain
the ordered queue. The exact local download benchmark reduced median time by
3.1%, bytes/op by about 52%, and allocations/op by about 20%; the direct path
itself is pinned at zero allocations. These changes lower churn without
enlarging a steady working set.

## ACK-path decomposition

There are two independent acknowledgement layers, and changing the wrong one
can add traffic without advancing the browser:

1. A `ReceiveSequence` emits cumulative/selective **Transfer ACKs** for reliable
   Transfer messages. Its worker publishes the first update after an idle
   period immediately, then enforces a 10-ms minimum interval while traffic is
   sustained. It writes one cumulative ACK plus any selective or missing-
   contract ACKs. The sender receives those frames
   through a per-`SendSequence` `AckBufferSize` channel. A full channel is a
   nonblocking handoff drop; the already-existing
   `ClientReceiveStatsSnapshot.AckHandoffDropCount` records it.
2. Android's TCP stack emits ordinary IP **TCP ACK packets** into the TUN. They
   traverse the client outbound `SendSequence` and the provider NAT applies
   their ACK number and advertised window before its upstream socket reader can
   emit more download data. TCP packets intentionally retain end-to-end
   Transfer recovery, even on H1, because a carrier reconnect cannot prove that
   the provider consumed a prior packet.

The global depth-32/64 experiments widened both data handoffs *and* the
Transfer-ACK handoff. Receive-only and send+receive depth 128 did not reproduce
the global-depth bulk result. The next carrier-attributed run resolved the
ambiguity: with an H1-only 64-message receive handoff, `AckHandoffDropCount`
stayed zero while Pack handoff loss reached 2,280. Increasing the ACK channel
therefore spends slots at a boundary that was not full. Top-level
`ClientSettings.SendBufferSize` and `MultiClientSettings.SequenceBufferSize`
are not active hot-path capacities in the present implementation;
device-side forwarding is also not the ordinary H1 client path.

The retained allocation is a carrier-specific H1 receive depth of 64 with the
same 128-KiB encoded-byte ceiling. H3, send, Transfer-ACK, forward, contract,
and control queues remain at 16. The first 1-ms H1-only handoff arm reduced
Pack drops from 82 to one through its page/Cloudflare phase. A later resource
timeout motivated an interim 10-ms Pack-only wait, which converted 9/11 brief
reader/worker scheduling mismatches without enlarging either queue. The pinned
provider A/B subsequently showed that any finite expiry can still manufacture
a permanent sequence hole. The accepted H1 Pack policy therefore waits for
capacity or cancellation under the same fixed count/byte gates. ACK admission
remains at 1 ms. The adjacent remediation applies this Pack rule to exact
H3/DNS QUIC-stream and SCTP lanes as well; H3 DATAGRAM, native P2P, and unknown
custom carriers remain zero-wait.

ACK scheduling has a separate low-memory win. Instead of delaying the first ACK
after an idle period by 10 ms, the worker now enforces the same 10-ms *minimum
spacing between writes*: an idle burst is acknowledged immediately, while a
sustained stream remains compressed to at most one cumulative write per
interval. This is a token-bucket/quick-ACK policy, not an ACK-every-packet
policy. It removes one 0--10-ms causal delay at sparse boundaries without
raising the sustained ACK rate or retaining another queue. Shortening the
sustained interval is not currently justified: at 40 Mbit/s, 10 ms is roughly
50 KiB, far below the 512-KiB resend ceiling, and the device saw no inbound ACK
handoff loss.

Packet formation used to work against both ACK layers. The TUN reader can
drain 64 ready packets and the mobile NAT groups up to 16 packets from an exact
flow. H1-only SendSequences now retain up to 16 already-ready frames and 3 KiB
of message bytes in one physical Transfer Pack; H3 and mixed routes keep the
two-frame/one-MTU DATAGRAM-safe bound. Sixteen small TCP ACK packets can
therefore use one H1 sequence number instead of eight. Above that boundary the
H1 WebSocket writer may combine up to 32 already-ready messages in one TLS
write while retaining its 12-KiB drain stop and 16-KiB storage. This preserves
every TCP ACK byte, order, SACK/ECN/window signal, and ordinary Transfer
recovery; neither layer waits for a batch to fill.

The corresponding download-direction batching regression is fixed locally.
Before the hybrid H3 packet lane, the reliable H1 coalescer admitted two frames and up
to 3 KiB of message payload because two MTU packets plus the envelope fit the
deployed 4-KiB H1 message limit. The H3 work lowered the shared byte ceiling to
one 1,100-byte MTU so a message stayed DATAGRAM-eligible, making default H1 pay
the same ceiling. Deployed providers still do: full return packets become one
Transfer message each. Provider socket drains now remain logical groups of
up to 16 frames / 24 KiB until the SendSequence pins its carrier generation.
H1 emits two full-MTU packets per bounded Pack; H3 still emits its existing
DATAGRAM-safe chunks. Contract-bearing and no-contract drains share this path,
and sparse singleton returns keep the raw-frame fast path. A new logical
sequence opens and samples its writer when its first group is selected, so the
first response burst receives the H1 bound rather than requiring a warm Pack.
Focused tests pin
route-generation changes, contract boundaries, retry identity, ownership,
partial admission, exact completion/accounting, and H1/H3 chunk bounds.

On the opposite direction, an established provider TCP flow no longer allocates
and enqueues a `TcpSendItem` merely to apply a pure ACK. The fast path takes the
same per-flow lifecycle lock, updates the reply lane, applies only the monotonic
ACK/window/timestamp state under the TCP mutex, wakes a socket reader waiting
for window room, and returns the packet owner. It cannot run before the SYN has
initialized the flow and excludes every segment that consumes sequence space
or changes connection state. This speeds the acknowledgement that actually
opens provider download progress without weakening Transfer recovery.

A deliberately unsafe diagnostic marked pure TCP ACK packets Transfer-NoAck on
H1. It reduced timeout resends to 21 during the Cloudflare phase and removed
thousands of ACK-of-ACK recoveries, but Cloudflare remained at 1.65 Mbit/s and
Wikipedia tails worsened. Under a hot fast.com workload runtime rose to
27.79 MiB. The shortcut was reverted: every tunneled TCP packet still requires
end-to-end Transfer commit across carrier disconnect and route replacement.
The result is useful because it also rules out downstream ACK-of-ACK head-of-
line blocking as the primary 40-Mbit/s limiter.

Deliberately not first-line changes:

- Dropping superseded pure TCP ACK packets risks changing SACK, ECN, duplicate
  ACK, zero-window, and ACK-clock behavior. Preserve them until packet traces
  prove redundancy under the exact semantics.
- Marking TCP ACK IP packets Transfer-NoAck was measured and rejected. It loses
  commit across an H1 reconnect and did not improve page or bulk performance.
- Adding an ACK batching timer below WebSocket would directly worsen sparse
  TTFB. Only ready-drain batching is eligible.
- A larger 4,096-root gate already produced zero pressure drops without a
  throughput gain, so class-aware control admission remains a tail-safety idea,
  not the explanation for the roughly 40-Mbit/s target gap.

### Logical bytes versus retained allocation bytes

The receive queue needs two simultaneous measurements. `MessageByteCount` is
the protocol payload used by the per-sequence 768-KiB flow-control limit and by
existing diagnostics. The shared mobile budget instead charges what a queued
`ReceivePack` keeps alive: the pooled backing class of the encrypted outer
Transfer frame, every decoded message and contract-frame root, and a rounded
1-KiB decoded-owner envelope. `MessagePoolRootByteCount` reports the actual
256/2,048/4,096/8,192-byte pooled class rather than the visible slice length;
non-pooled slices fall back to their visible length. Saturating addition makes
malformed or synthetic accounting fail closed.

Both totals live in `transferQueue`. Add, duplicate replacement, ordered-tail
eviction, remove, clear, and cancellation update them together under the queue
lock. The per-flow `CanAddWithQueueByteCount` check applies the logical total to
the useful protocol window and only the retained total to the shared budget.
An empty sequence preserves the existing one-item progress exception even when
another flow owns the aggregate window; it cannot admit a second item until
budget returns. The bounded mobile flow count therefore bounds this deliberate
overdraft instead of recreating the former 96-KiB-per-flow floor.
This separation is essential: payload-only charging produced 6.15 MiB of roots
behind a reported 2-MiB queue, while applying the retained charge to both limits
reduced a flow's useful payload window to roughly 250 KiB and stalled the second
Cloudflare sample. The final split held roots to 1.78 MiB, preserved all ten
payloads, and stayed below 24 MiB without a forced collection.

Retained-allocation accounting is explicitly mobile-policy opt-in. Server and
desktop defaults preserve their historical logical-byte queue behavior and do
not pay the root scan. Send queues continue to charge encoded frame length for
both their local limit and shared resend budget; a regression test pins that
the embedded queue item cannot accidentally bypass the `sendItem` override.

## Measurements

All MiB values below are `goRuntimeBytes / 1048576`. Network results are real
route observations and can vary; compare repeated distributions and retain
failed samples.

| Date / build | Carrier and profile | Wikipedia | Cloudflare 1 MiB | fast.com | Runtime / pressure | Decision |
| --- | --- | --- | --- | --- | --- | --- |
| 2026-08-24 `m24-control-recheck-20260824` | Auto, provider off, 4 quality / 1 speed, pre-low-churn control | load median 2773.6 ms; main TTFB 294.4 ms; request p95 median 2244.51 ms | median 9.134 s / 0.88 Mbit/s | load 1326.4 ms; TTFB 916.3 ms; 31 requests, 14 failed | fast max 20.55 MiB; final recovery 20.35 MiB; 2443 drops | Control only. Page tail and bulk rate leave room to improve. |
| 2026-08-24 `m24-prefix-admit-20260824` | Auto, provider off, low-churn + ordered-prefix admission | load median 955.6 ms; main TTFB 354.8 ms; request p95 median 553.56 ms | median 9.664 s / 0.83 Mbit/s, all completed | load 1191.6 ms; TTFB 785.5 ms; 30 requests, 14 failed | H1/Auto phases max 21.59 MiB; recovery live 8.17 MiB; 2036 drops | Keep as candidate. Large page-tail win; bulk is neutral within route variance. H3 result is deferred. |
| 2026-08-24 H1 q4 control, same candidate build | Explicit H1, provider off, 4 quality / 1 speed, 16 packets / 24 KiB group, 512-root gate | load median 2552.3 ms; main TTFB 223.1 ms; request p95 median 2206.95 ms | median 11.540 s / 0.73 Mbit/s; first-byte 760.74 ms | load 1400.9 ms; TTFB 935.8 ms; request p95 6006.72 ms | Wikipedia max 16.95 MiB; 1 MiB max 18.64 MiB / 448 drops; fast max 20.16 MiB / 956 cumulative drops; five exits observed | Fresh H1 control. Main-document TTFB is healthy, but parallel resources repeatedly stall near the 2 s send-retry cadence and bulk ingress hits the root gate. Test route capacity and pressure independently. |
| 2026-08-24 Direct bracket after H1 q4 | Direct Wi-Fi, no tunnel | one Wikipedia run: load 263.2 ms; main TTFB 98.7 ms; request p95 93.31 ms | 38.98, 42.20, and 20.68 Mbit/s; median 38.98 Mbit/s | not run as an acceptance sample | Tunnel runtime not applicable | The radio/origin path can still deliver the requested 40-Mbit/s class. The roughly 53-fold median bulk gap and roughly 10-fold page-load gap are inside the tunnel path or its selected exit, not the local Wi-Fi ceiling. |
| 2026-08-24 `h1-root4096-diag-20260824` | Explicit H1, provider off, q4 / s1, sequence/group 16, root gate 4,096; diagnostic only | not run after the bulk hypothesis failed | 0.63, 0.91, and 0.49 Mbit/s; median 0.63 Mbit/s | not run | zero pressure drops; max 763 roots; max runtime 19.03 MiB; max live 6.96 MiB | Reject. Removing the root gate did not restore throughput and spent memory. Restore 512 and move to sequence-window isolation. |
| 2026-08-24 `h1-seq32-diag-20260824` | Explicit H1, provider off, q4 / s1, global mobile sequence depth 32, group 16/24 KiB, root gate 512 | load median 543.7 ms; main TTFB 220.7 ms; request p95 median 247.34 ms. One cold-origin run was 2534.4 ms; the other six were 514.2--741.0 ms. | 4.65, 5.57, and 2.49 Mbit/s; median 4.65 Mbit/s | page load 1606.9 ms; main TTFB 1053.2 ms; request p95 3549.85 ms; 64 requests / 31 failed | max active runtime 20.34 MiB; max live 8.00 MiB; max pool outstanding 1,127; cumulative pressure drops 961 during fast.com | Strong keep/split signal. Versus the depth-16 H1 control, median 1 MiB goodput improved 6.4x and median page load improved 4.7x without crossing 24 MiB. Test depth 64, then assign the winning depth only to saturated H1 data queues. |
| 2026-08-24 `h1-seq64-diag-20260824` | Explicit H1, provider off, q4 / s1, global mobile sequence depth 64, group 16/24 KiB, root gate 512 | load median 457.6 ms; main TTFB 177.8 ms; request p95 median 230.86 ms. One 2368-ms tail remained. | initial 4.88, 1.66, and 1.17 Mbit/s; after 30 s, 5.17, 1.22, and 6.44 Mbit/s. A 4 MiB sample reached 4.14 Mbit/s and a second timed out. | not repeated; page-focused measurements already established the gain | max runtime 21.54 MiB; max live 8.60 MiB; max pool outstanding 1,118; retained pool 2.56 MiB; cumulative pressure drops 1,417 | Split rather than keep globally. Depth 64 improved median page load another 16% over depth 32 and stayed below 24 MiB, but bulk remained near a 5-Mbit/s plateau with TCP-setup stalls and less memory margin. Test a larger receive-only byte/count window while returning send/control queues to 16. |
| 2026-08-24 direct bracket after sequence diagnostics | Direct Wi-Fi, no tunnel | not repeated | 4 MiB downloads reached 62.06 and 54.50 Mbit/s; median 58.28 Mbit/s | not run | tunnel runtime not applicable | Confirms sustained 40-Mbit/s-class underlay during the same device session. The H1 plateau is not the radio or endpoint. |
| 2026-08-24 `h1-rx128-256k-diag-20260824` | Explicit H1, provider off, q4 / s1; receive sequence 128 / 256 KiB; send, ACK, forward, and control depths restored to 16 | load median 672.5 ms; main request-to-first-byte 219.5 ms; request p95 median 354.91 ms. Two of seven runs retained the roughly 2.25-s tail. | 2.01, 2.51, and 2.59 Mbit/s; median 2.51 Mbit/s | not run after the isolation failed | max runtime 20.09 MiB; max live 7.03 MiB; max pool outstanding 803; cumulative pressure drops 424; zero 28-MiB breaches | Reject receive-only as the explanation. It lost 46% bulk rate versus global depth 32 and 51% versus the settled global-depth-64 median. The H1 download still emits inner TCP ACK packets through the outbound SendSequence; isolate that sequence next and keep its ACK/control channel at 16. |
| 2026-08-24 `h1-tx128-rx128-diag-20260824` | Explicit H1, provider off, q4 / s1; send and receive data sequences 128 / receive handoff 256 KiB; Transfer-ACK, forward, contract, and control depths 16 | load median 561.1 ms; main request-to-first-byte 176.4 ms; request p95 median 284.35 ms. Two roughly 2.4-s tails remained. | 1.10, 1.16, and 1.16 Mbit/s; median 1.16 Mbit/s | not run after the bulk isolation failed | max runtime 19.35 MiB; max live 7.00 MiB; max pool outstanding 657; cumulative pressure drops 347; zero 28-MiB breaches | Reject. Widening both data sequence handoffs did not reproduce the global-depth result and was 75% slower than global depth 32. This points away from raw data-channel depth and toward one of the global constant's other consumers, especially the Transfer-ACK handoff. Public-provider variation remains a confounder, so subsequent A/Bs must use adjacent brackets or a fixed provider. |
| 2026-08-24 `h1-rx64-ack-coalesce-20260824` | Explicit H1, provider off; H1 receive 64 / 128 KiB; send and control 16; immediate-idle Transfer ACK and H1 logical grouping | load median 664.3 ms; main request-to-first-byte 305.0 ms; TTFB 312.1 ms; one 2.71-s tail | one run: 2.44 Mbit/s; first byte 758 ms | not retained | peak runtime 21.20 MiB; Pack handoff HWM 64 / about 98 KiB; 2,280 Pack drops; zero ACK-handoff drops | Keep receive depth 64, not a larger ACK channel. The full receive handoff and zero ACK loss identify the saturated boundary. |
| 2026-08-24 `h1-rx64-ackreserve-20260824` | Prior row plus provider-off H1 ACK-only packet-root reserve | load median 520.6 ms; main request-to-first-byte 197.4 ms; TTFB 202.5 ms; request p95 232.28 ms | median 1.57 Mbit/s | not retained | peak runtime 20.52 MiB; 361 reserve admissions; 76 ACK-root drops; 153 Pack drops | Keep only as overload protection. It protects TCP ACK progress and improves page tails, but alone is not a bulk-speed mechanism. |
| 2026-08-24 `h1-rx128-256k-ackreserve-20260824` | H1 receive 128 / 256 KiB plus ACK reserve; diagnostic | load median 481.5 ms; main request-to-first-byte 184.0 ms; TTFB 189.2 ms; one 3.07-s tail | median 6.73 Mbit/s | 0.99 then 1.2 Mbit/s | first phase peak 22.21 MiB; repeat reached about 25.05 MiB; 2,635 non-ACK root drops and 204 Pack drops | Reject. It crosses the 24-MiB active target and collapses under repeated load. Depth 64 is the memory/performance knee. |
| 2026-08-24 `h1-rx64-ackscan-20260824` | H1 receive 64; allocation-free ACK-only rescue from a rejected native-batch suffix | load median 750 ms; main request-to-first-byte 231.6 ms; TTFB 237.0 ms; no multi-second page tail | median 2.24 Mbit/s | fresh 9.1 Mbit/s; hot 0.52 Mbit/s | peak runtime 22.24 MiB; 266 reserve admissions after page/bulk; 82 Pack drops; hot pressure included 953 non-ACK drops | Keep suffix rescue, but not as a throughput claim. It preserves ACK progress without reordering any two admitted packets; the hot collapse remained upstream. |
| 2026-08-24 `h1-rx64-ackscan-wait1ms-20260824` | Retained candidate: H1 receive 64 / 128 KiB, H1-only 1-ms Pack handoff wait, ACK reserve/suffix rescue, quick Transfer ACK, H1 logical grouping | load median 521.5 ms; main request-to-first-byte 203.2 ms; TTFB 210.9 ms; request p95 227.43 ms; seven warm runs stayed 483--537 ms | median 1.58 Mbit/s; first byte 950.62 ms | fresh 1.2 Mbit/s; hot 0.94 Mbit/s | page/bulk peak 18.63 MiB with only one Pack drop; final peak 21.80 MiB; 19 samples, zero 28-MiB breaches; Pack waits 3 / successes 2 | Keep. The bounded handoff fixes the internal Pack-collapse mode and gives the best repeatable page distribution inside the memory target. The remaining bulk ceiling is not that queue. |
| 2026-08-24 `h1-rx64-acknoack-group-20260824` | Diagnostic only: retained candidate plus pure TCP ACK Transfer-NoAck; provider-return grouping was dormant on the unchanged public provider | load median 1,032 ms; main TTFB median 311 ms; request p95 retained 2.30--5.30-s tails | median 1.65 Mbit/s | canonical fresh 0.64 Mbit/s; an overlapping hot reload failed after about 80 s | Cloudflare peak 20.46 MiB and only 21 timeout resends; hot peak 27.79 MiB, 4,951 pressure drops, 621 timeout resends | Reject and revert. Removing thousands of ACK-of-ACK recoveries did not improve speed and weakened route-replacement delivery. Two fast.com query-cachebuster 404s were invalid harness attempts and are excluded. |
| 2026-08-24 direct bracket after ACK diagnostics | Direct Wi-Fi, no VPN; same attached device and endpoint | not repeated | 38.87, 80.04, 92.28, and 90.01 Mbit/s; middle-pair median 85.03 Mbit/s | not run | tunnel runtime not applicable | Confirms ample 40-Mbit/s-class underlay after the slow tunnel cohort. The retained client path is no longer dropping Pack handoffs during ordinary pages, so provider/exit deployment and controlled end-to-end relay tests are now the highest-value bulk work. |
| 2026-08-24 `h1-ack-smallpool-20260824` | Explicit H1/provider off; retained receive/wait policy plus exact packet-byte accounting, 256-byte ACK pool class, 1-MiB base gate, and 2-MiB ACK ceiling | seven-run load/request-to-first-byte/TTFB/request-p95 medians 363.9/119.2/142.4/177.77 ms; no failed navigation, with one 2.34-s tail | ten runs 1.20--2.90 Mbit/s; median 1.695 Mbit/s | canonical 30-s-settle load 727.8 ms; request-to-first-byte 104.9 ms; TTFB 445.2 ms; request p95 2,170.08 ms; 7/28 requests failed | peak runtime 22.19 MiB; runtime p50/p95 22.13/22.19 MiB; max packet ownership 2.19 MiB / 1,126 roots; 2,164 ACK-reserve admissions and 338 ACK drops across the full session | Keep the 2-MiB ACK ceiling. It improves page distribution and protects ACK progress without spending another data MiB, but 1.695 Mbit/s remains far below Direct. |
| 2026-08-24 `h1-ack-smallpool3m-20260824` | Prior row with a 3-MiB ACK-only ceiling; diagnostic only | load median 393.9 ms | ten-run median 0.92 Mbit/s | load 1,661 ms; TTFB 1,036 ms | peak runtime 21.74 MiB; zero ACK drops but 79 Pack handoff drops | Reject and revert. Eliminating the remaining ACK drops did not improve speed and displaced useful Pack progress. Two MiB is the measured knee. |
| 2026-08-24 `h1-ack-direct-batch32-20260824` | Prior retained client plus 32-message/12-KiB ready-only H1 WebSocket drain; public provider unchanged, so the direct provider ACK fast path was not deployed | seven-run load/request-to-first-byte/TTFB/request-p95 medians 787.5/343.0/349.2/388.84 ms; the excluded cold warm-up loaded in 2,021.8 ms | ten runs 1.22--3.13 Mbit/s; median 2.02 Mbit/s (+19.2% versus the adjacent exact-byte run) | the harness navigation loaded in 2,651.1 ms with request-to-first-byte 467.2 ms, TTFB 1,860.8 ms, request p95 2,325.05 ms, and 7/27 failed requests; a separate canonical page settled at 5.7 Mbit/s after 45 s and moved 28.74 MiB H1 ingress | 29.48-MiB peak runtime, 13.71-MiB peak live heap, 2,137 / 3.85-MiB peak outstanding pool, 6.48-MiB returned pool, 3,577 pressure drops, and three >28-MiB samples. After traffic stopped, one automatic rebuild dropped 5.98 MiB of pool ownership and runtime fell 29.48 -> 19.85 MiB, then held 20.15--20.48 MiB. | Physical speed signal only, not an accepted profile. Bulk median improved, but pages regressed and the sustained real fast.com burst failed the active/post-burst memory gate. The fixed-size ready drain is not a 6-MiB allocation; returned packet high-water and allocator-span retention explain the excess. Reduce that high-water or provider message amplification before release. |
| 2026-08-25 `h1-coalesce-pack2m-20260825` | 32-ready H1 plus one shared 2-MiB Pack-handoff budget; receive reorder still had an uncharged per-flow floor | pre-load Wikipedia median load/TTFB 368.2/143.6 ms; post-recovery median 639.3 ms with one 7.7-s resource tail | ten-run median 1.65 Mbit/s | at least 21.52 MiB ingress in the sampled window | runtime peaked at 30.39 MiB with three >28-MiB samples; packet roots reached 6.51 MiB while the Pack queue itself peaked at only 6.65 KiB; automatic recovery returned runtime to about 20.15--20.48 MiB | Reject. The Pack budget was not the retained owner: roughly 80 receive sequences could each retain their uncharged 96-KiB reorder floor. |
| 2026-08-25 `h1-rxbudget2m-wait5-20260825` | Shared 2-MiB receive budget, zero per-flow floor, and 5-ms H1 handoff wait, but accounting charged protocol payload rather than retained roots | pre/hot/post-recovery Wikipedia median load 573.2/497.7/497.8 ms; median TTFB 214.3/212.4/209.5 ms; no hot tail | ten runs 1.16--7.07 Mbit/s; median 2.62 Mbit/s | adjacent Direct 4-MiB samples were 27.22, 34.82, 46.04, and 41.53 Mbit/s | runtime still peaked at 29.59 MiB with two >28-MiB samples; logical receive use stopped at 2 MiB but packet roots reached 6.15 MiB; recovery p50/p95/last were 20.15/20.61/20.11 MiB; 3 Pack drops and 7 waits / 4 successes | Reject. Payload-byte accounting hid the pooled backing classes, encrypted outer frame, decoded contract/message roots, and owner envelope retained by each queued item. |
| 2026-08-25 `h1-rxalloc2m-wait10-20260825` | First exact retained-allocation charge and 10-ms H1 handoff wait; the same larger charge accidentally also constrained the 768-KiB per-flow logical window | pre-load Wikipedia median load/TTFB 566.7/231.6 ms; hot median 583.1 ms but 3.35/5.76/2.55-s tails | first 1-MiB object completed at 2.11 Mbit/s; the second aborted after 60 s and the remaining cohort was not retried | canonical fast.com completed | runtime passed at 22.83 MiB, roots stayed at 1.71 MiB, and recovery p50/p95/last were 20.67/20.88/20.68 MiB | Reject for performance. A flow saturated near 0.78 MiB of retained charge while it had only about 250 KiB of useful payload in flight. Separate logical flow control from aggregate retained-allocation accounting. |
| 2026-08-25 `h1-rxalloc-separate-wait10-20260825` | Final candidate: 32-ready H1, 64/128-KiB H1 receive handoff, 10-ms reliable-carrier wait, independent logical per-flow window, and exact shared 2-MiB retained-allocation budget | pre-load median load/request-to-first-byte/TTFB 455.1/164.5/169.6 ms; hot median 627.4/245.3/248.8 ms with one isolated reused-H2 5.3-s resource wait and no concurrent tunnel loss; post-recovery median 613.6/223.7/227.8 ms with 7/7 success and no multi-second resource tail | all ten full 1-MiB responses completed at 2.04--6.00 Mbit/s; median 2.78 Mbit/s and 554.22-ms median first byte | at least 20.53 MiB ingress in the inner 75-s counter bracket; public-provider rate remains far below the adjacent 41.53-Mbit/s Direct upper-pair median | active runtime peaked at 21.77 MiB with 8.91-MiB live heap, 1.78-MiB packet roots, exact receive use 2.00/2.00 MiB, and zero samples above either 24 or 28 MiB. Five-minute recovery p50/p95/range/last were 19.91/20.16/19.85--20.20/19.91 MiB with a 256-KiB warm set, zero queued receive bytes, zero forced GCs, and zero trims. Across the burst, 9/11 bounded Pack waits succeeded; two drops returned only 2,880 bytes and all payloads completed. | **Accept the client memory/performance profile.** Exact retained charging fixes the 29--30-MiB high-water without shrinking the protocol BDP window. Do not claim 40 Mbit/s until the retained provider grouping/ACK changes are deployed to a controlled exit and measured end to end. Physical iOS footprint validation remains separate. |
| 2026-08-25 `h1-adaptive-depth-20260825` | Diagnostic: H1 count depth starts at 64, requires two full observations within 100 ms, and grows by 16 toward 128; logical bytes remained fixed at 128 KiB | pre-load Wikipedia median load/TTFB 604.2/252.6 ms; post-recovery 463.8/196.2 ms after one 6.02-s cold restart | 7/10 completed before one 120-s stream abort; completed median 1.54 Mbit/s | 3.29 MiB ingress in 75 s, about 0.37 Mbit/s; adjacent Direct 4-MiB median 86.74 Mbit/s | runtime peak 21.39 MiB; recovery p50/p95 19.86/20.24 MiB. Thirteen saturations and four grants across two flows reached an earned maximum of 96, but actual HWM stopped at 92 Packs / 130,978 of 131,072 bytes. | Reject count-only deepening. It stayed inside memory, but the fixed logical-byte cap became the next boundary and performance remained public-provider limited. Test paired count/byte growth once. |
| 2026-08-25 `h1-adaptive-depth-bytes-20260825` | Diagnostic: paired 64/128-KiB -> 128/256-KiB H1 growth in 16/32-KiB steps under the unchanged exact shared retained budget | pre-load Wikipedia median load/TTFB 404.3/149.8 ms; post-recovery seven-run median 439.6/186.2 ms, including a 2.51-s cold connection setup | all 10 completed; median 1.18 Mbit/s and 476.82-ms first byte | 1.77 MiB ingress in 75 s, about 0.20 Mbit/s; Direct 4-MiB median 87.4 Mbit/s | runtime peak 22.45 MiB; 390-s recovery p50/p95/range/last 20.58/20.91/20.19--20.97/20.19 MiB; zero samples above 24/28 MiB. One flow earned 128/256 KiB and queued 128/194,688 bytes; all four grants occurred before fast.com, which added none. Session timeout resends reached 620. | Reject as the production mobile default. Full adaptive depth is memory-safe in this arm but does not improve bulk or fast.com and increases recovery work. Keep fixed 64/128 KiB; retain the generic opt-in and telemetry only for a controlled-provider A/B. |
| 2026-08-25 `h1-logical-lanes8-20260825` / adjacent lane-zero control | Explicit H1/provider off with fixed 64/128-KiB receive policy; eight bounded five-tuple lanes plus lossless Transfer-ACK overflow folding, followed by a rebuilt lane-zero arm in the same device session | lane 8 median load/TTFB 348.8/126.1 ms; lane 0 median 1,157.8/367.9 ms | not repeated; the earlier complete fixed-depth cohort remains the payload control | lane 8 displayed 10 then 3.6 Mbit/s; its exact repeat moved 3.06 MiB H1 ingress in 19.3 s. Lane 0 displayed 4.4 Mbit/s and moved 18.55 MiB in 34.7 s. Same-session Direct displayed 410 Mbit/s before the first arm and 1.1 Gbit/s after the second. | lane 8 runtime peak 20.60 MiB, 2.00/2.00-MiB receive use, zero >28-MiB samples, and 152 timeout resends in the exact repeat; lane 0 peak 19.43 MiB, 1.41/2.00-MiB receive use, zero >28-MiB samples, and 1,053 timeout resends. Both arms had zero Transfer-ACK handoff loss. | Keep eight lanes as a controlled explicit-H1 client/provider candidate, not as a public-provider speed claim. Client lanes materially improve request/inner-TCP-ACK isolation and page latency, but provider download data remained on lane zero. Enable the same negotiated lanes on a pinned provider sender and deploy provider grouping/direct ACK before the decisive end-to-end A/B. |
| 2026-08-25 `h1-lossless-20260825` | Pinned controlled provider, explicit H1/eight logical lanes, fixed 64/128-KiB H1 receive policy, lossless carrier-route and Pack backpressure, unchanged exact 2-MiB shared budgets | seven-run Wikipedia load/document-TTFB/request-p95 medians 439.2/181.5/183.58 ms; 7/7 success. The fresh connection loaded in 1,019.3 ms; six reused loads were 404.6--517.0 ms. | not repeated; fast.com is the decisive bulk workload for this root cause | consecutive displays 38, 41, and 52 Mbit/s; host-adjacent Direct was 1.3 Gbit/s. The latter repeats moved about 70.2 and 73.2 MB of new provider return traffic. | active runtime peak 17.60 MiB; zero samples above 24/28 MiB; cumulative 262 / 687,039-byte carrier backpressures with zero carrier drops; 762/762 Pack waits succeeded with zero Pack drops; Pack and reorder queues drained to zero. A 345-s recovery measured runtime p50/p95/range/last 17.57/17.61/17.23--17.61/17.41 MiB, zero queued ownership, 0.50-MiB maximum retained pools, and no forced GC/trim. | **Accept the lossless H1 pipeline.** It restores the 40-Mbit/s class and fast reused-page loads by removing synthetic gaps, not by spending more depth or memory. H3/DNS performance tuning remains a future iteration; its stream-versus-DATAGRAM correctness contract is now covered separately. iOS footprint remains a separate gate. |
| 2026-08-25 `lane-remediation-20260825` / retained AAR / `lane-candidate-close-20260825` | Current–pre-change–current public explicit-H1 bracket on the attached Pixel; fresh app/client/Chrome each arm; exact H1 counters | not repeated | not repeated | opening current 8.7/6.9/7.3; retained AAR 84/95/1.2; closing current 160/130/140 Mbit/s. Exact ingress was 43.80/223.70/504.00 MB. | Go-runtime peaks 18.73/19.25/20.21 MiB. Closing current recorded 5,769 / 8,483,576-byte route backpressures, zero route drops, 1,804/1,804 Pack waits, zero Pack drops, and zero final Pack/reorder use. | **No H1 fast.com regression detected.** The closing current median was 140 Mbit/s versus the bracketed baseline's 84 Mbit/s and all three current-closing samples exceeded 40. Preserve the opening/current and baseline outliers: unpinned public-provider selection makes this a target/regression gate, not a percentage speedup claim. |
| 2026-08-25 final rebased current / retained AAR / final rebased current | Post-pull explicit-H1 current–baseline–current bracket on the same Pixel, Wi-Fi route, Chrome version, and canonical harness; fresh app/client/Chrome each arm; the pull changed only generated IP-security/blocker data | not repeated | not repeated | current primary 0.58/27/52 Mbit/s (27 median), with preserved extension 17/8.1/13; retained AAR 28/15/10 (15 median); closing current 0.65/17/10 (10 median). Exact H1 deltas were 302.43 MB over six / 147.21 MB over three / 64.37 MB over three. | Runtime peaks were 19.66/17.70/19.89 MiB with zero >28-MiB samples. Every arm had zero carrier/Pack drops; current Pack waits completed 165/165 opening and 38/38 closing. Timeout resends were 14,057/330/6,926, proving that provider conditions changed sharply across arms. | **Neutral route-limited confirmation.** Baseline was also below 40 and current produced the only above-40 result; do not infer a percentage from this degraded public route. Together with the earlier 140-Mbit/s current closing arm and pinned 38/41/52 arm, it finds no systematic fast.com regression. The final APK was restored, all three clients released, and device credentials/artifacts removed. |

### ACK and grouping microbenchmarks

The contract-safe provider grouping change has a deterministic H1 Transfer
boundary benchmark. On the Apple M4 Pro with `GOMAXPROCS=10`, seven 500-ms
samples compared one 16-packet, 1,500-byte provider drain represented as the
old singleton logical groups versus one retained logical group. Median time
fell from 25,579 ns to 9,105 ns (2.81x throughput, 64.4% less time), wire Packs
fell from 16 to 8, allocated bytes from 19,440 to 4,024 per drain (-79.3%), and
allocations from 147 to 51 (-65.3%). This is a same-process Transfer boundary,
not an Internet throughput claim; it proves that the batching change removes
real marshal/recovery work before deployment.

The provider pure-TCP-ACK fast path has a second exact local download result.
Across seven two-second samples, applying established ACK/window updates
directly changed median time from 56,712 to 54,935 ns (-3.1%), throughput from
577.8 to 596.5 MB/s (+3.2%), bytes/op from about 5,351 to 2,560 (-52%), and
allocations/op from 46 to 37 (-19.6%). The direct helper itself remains at zero
allocations. This is provider-process evidence; the attached device still used
unchanged public providers.

At the client H1 TLS boundary, raising the ready-only count from 16 to 32 while
retaining the 12-KiB byte stop changed ordinary full-payload median time from
948.1 to 949.5 ns (+0.15%, neutral). ACK-sized median time fell from 533.9 to
445.0 ns (-16.7%), byte throughput rose about 240 to 288 MB/s, and physical
writes/frame fell from 0.0693 to 0.0443 (-36%). Sparse arrival was neutral
(13,822 versus 13,807 ns), and allocation results stayed 58--81 B/op and 3--4
allocations/op for the corresponding shapes. The count increase therefore
removes ACK-sized writes without adding a timer, buffer, or sparse TTFB cost.

Every benchmark in `server/connect`, `server/connect/perfvar`, and
`server/proxy` then ran again after the ACK fast path and ready-drain changes.
All 175/10/20 current samples passed. Against the preceding small-pool cohort,
time geomeans moved +1.65%, +2.13%, and -3.79%; exact PERFVAR/proxy allocation
counts were unchanged, while Connect's heterogeneous bytes/op and allocation
geomeans moved +2.67% and +0.49%. The directions disagree across unaffected
benchmarks and are treated as run-order host noise; the exact affected local
benchmarks above are neutral or faster. The DB-backed H1 full-TUN campaign
remains blocked: the configured local Redis endpoint returned `host is down`,
and the broad short tier additionally requires unavailable local vault/DB
fixtures. The complete benchmark-only PERFVAR tier is green; no payload-
throughput result is inferred from the blocked fixture.

The final 2026-08-25 server isolation repeated the check after retained receive
accounting landed. The current benchmark-only tiers passed 190/10/20 samples
for `server/connect`, `server/connect/perfvar`, and `server/proxy`. A seven-run
same-binary 8/16/32 H1 sweep measured full-payload TLS at
1,305.04/1,385.02/1,473.51 MB/s and ACK-sized TLS at
167.03/235.18/309.30 MB/s. Thus the production 8 -> 32 ready cap improved
payload throughput 12.9% and ACK throughput 85.2%, reduced TCP writes/frame
25%/75%, and left allocations/op unchanged. A detached
baseline/current/current/baseline isolation then changed PERFVAR -0.46% and
proxy +0.64% overall; all B/op and alloc counts were identical. The roughly
20% previous-day absolute slowdown reproduced in the baseline and was host
frequency drift, not a code regression. Server queues leave retained-root
scanning disabled, so no server-specific reclaim behavior is needed.

The logical-lane follow-up also isolated the compact Transfer-ACK overflow
path. At a clean 50-ms RTT, fixed lane-zero H1 measured 134.551 Mbit/s before
and 134.529 Mbit/s after folding a full ACK handoff directly into the existing
monotonic cumulative/selective window (-0.016%). The fallback adds no queue,
wait, timer, or per-ACK allocation. In the impaired four-flow PERFVAR arm,
lane zero measured 20.247 Mbit/s on a 41.474-Mbit/s underlay and eight lanes
measured 29.058 Mbit/s on a 43.704-Mbit/s underlay: +43.5% raw goodput at
similar calibration. Order-balanced repeats were route-variable, so this is a
causal flow-isolation signal rather than a universal multiplier. It complements
the physical page/retransmission result; it does not overcome a provider that
still puts every return flow on lane zero.

Final-source validation passed the complete Connect and SDK short suites in
199.341 and 97.135 seconds, focused normal/race repetitions, all affected vet
tiers, and the Android AAR/app/test build. After stopping a stale campaign-owned
benchmark probe that had contaminated the broad host cohort, five clean-host
repetitions measured production H1 TLS at 891.7 ns full-payload and 370.3 ns
ACK-sized medians with two allocations/op; PERFVAR receive credits measured
673.6 ns and proxy batch-64 measured 5,406 ns. This rules out a shared-server
performance regression from the ACK fallback or explicit-H1 mobile policy.
The broad server correctness attempt remains externally blocked by unset
`WARP_ENV` and absent vault `pg.yml`; focused tests and benchmark-only packages
do not require those fixtures and are green.

### Current ACK decision matrix

| Option | Memory/performance result | Decision |
| --- | --- | --- |
| H1 receive depth 32 -> 64 | Page median improved from 543.7 to 457.6 ms in the global isolation; carrier-attributed runs filled all 64 slots while staying below 24 MiB. | Keep fixed 64 for H1 only, with the 128-KiB byte cap. |
| Iterative H1 receive 64/128 KiB -> 128/256 KiB | A flow reached the full earned limit and queued 128 Packs / 194,688 bytes at a 22.45-MiB runtime peak, but Cloudflare fell to a 1.18-Mbit/s median and fast.com moved about 0.20 Mbit/s. No depth grant occurred during fast.com and timeout resends reached 620. | Reject for the production mobile policy. Keep the generic opt-in for fixed-provider diagnosis; do not spend the client budget until a controlled provider A/B identifies this boundary. |
| Reliable Pack handoff finite -> cancellation-bounded | A 1-ms arm left a rare timeout; at 10 ms, 9/11 waits succeeded. On the pinned provider, the finite boundary then dropped 24 messages and fast.com measured 3.5 Mbit/s. Waiting to capacity/cancellation produced 38/41/52 Mbit/s with 762/762 successful waits and no added slot or byte. | Keep for every exact reliable H1, H3/DNS stream, SCTP, and framed-server lane. ACK handoff remains 1 ms; H3 DATAGRAM, native P2P, and unknown custom lanes remain zero-wait. |
| Transfer ACK handoff 16 -> 64 | Inbound ACK-handoff drops remained zero while Pack loss was high. | Do not spend memory here. |
| Full Transfer-ACK handoff -> shared ACK window | Clean 50-ms H1 changed 134.551 -> 134.529 Mbit/s (-0.016%). A saturated compact channel now folds progress into its existing monotonic cumulative/selective window without growing the queue or waiting. | Keep as lossless overload handling. This coalesces Transfer protocol ACK state, not inner TCP ACK packets. |
| First Transfer ACK | A fixed 10-ms delay is directly on sparse request/response turns. | Send the first after idle immediately; retain 10-ms sustained spacing. |
| Shorter sustained Transfer-ACK interval | The 512-KiB resend budget holds roughly ten times 10 ms of 40-Mbit/s traffic; no ACK-handoff loss was observed. | Do not add ACK traffic without a causal trace. |
| ACK-only packet reserve | Protects up to 256 extra H1/provider-off packet roots (about 512 KiB worst-case) and preserves exact packet order among admitted packets. | Keep as overload progress protection; disable for Auto/H3/provider-on. |
| ACK-only packet reserve 2 -> 3 MiB | Removed ACK drops but lowered ten-run bulk median 1.695 -> 0.92 Mbit/s and raised Pack loss. | Reject 3 MiB; retain the exact 1-MiB base / 2-MiB H1 ACK maximum. |
| TCP ACK coalescing or dropping | Can alter duplicate ACK, SACK, ECN, and advertised-window semantics. | Reject. Preserve exact packet bytes. |
| TCP ACK Transfer-NoAck | Removed most ACK-of-ACK retries but did not improve physical performance and breaks commit across route replacement. | Rejected and removed. |
| Established provider pure-ACK direct apply | Removes one `TcpSendItem`, queue crossing, and worker wakeup per browser ACK; local download work fell 3.1%, 52% B/op, and 19.6% allocs/op. | Keep with handshake, lifecycle-lock, exact-owner, monotonic-ACK/window, and zero-allocation tests. It requires provider deployment before a device throughput claim. |
| Provider return logical groups | Halves full-MTU H1 Transfer messages, cuts local boundary allocations 65%, and leaves H3 physical chunks unchanged. | Keep 16 frames / 24 KiB logical fairness bound; require deployment/full-TUN validation. |
| H1 logical lanes 0 -> 8 | On the physical client, Wikipedia median load improved 1,157.8 -> 348.8 ms and timeout resends fell 1,053 -> 152 while runtime stayed below 20.61 MiB. Controlled four-flow goodput improved 20.247 -> 29.058 Mbit/s at similar underlay capacity. Public fast.com did not improve consistently because its provider return sender remained on lane zero. | Keep for a symmetric explicit-H1 client/provider A/B. Byte budgets remain shared and nonzero channel capacity is bounded; do not enable default Auto until the provider result covers carrier transitions. |
| H1 ready-only WebSocket drain 16 -> 32 | ACK-sized host throughput rose about 20% and writes/frame fell 36%; full payload and sparse arrival were neutral with unchanged fixed storage. Exact receive-allocation accounting then held the final sustained device run to 21.77 MiB active and 20.16 MiB recovery p95. | Keep; byte stop 12 KiB, wrapper 16 KiB, and no batching wait. Attribute Internet goodput only after a controlled provider A/B. |
| Dedicated priority carrier lane for Transfer ACKs | Could bypass a full data FIFO, but requires a second ordered transport lane and lifecycle/backpressure design. The NoAck diagnostic rules out downstream ACK-of-ACK HOL as the present primary limiter. | Protocol/transport candidate only after direct ACK-write wait telemetry proves contention. |
| Piggyback cumulative Transfer ACK on reverse data | Can remove a separate frame on full-duplex flows, but changes the wire contract and needs downgrade/retry semantics. | Future negotiated protocol candidate. |
| H1 message cap above 4 KiB | A negotiated 16--32-KiB envelope could combine more than two full-MTU packets, but old servers reject it and larger receive buffers consume burst memory. | Server-first capability rollout and an exact 24-MiB device A/B; never enable unnegotiated. |

### Ranked H1 limiters after this pass

| Rank | Boundary | Evidence | Highest-value next action |
| ---: | --- | --- | --- |
| 1 | Reliable H1 carrier-to-route and Pack handoffs | A pinned provider showed 530 carrier drops creating permanent Transfer holes and a full 2-MiB reorder tail at 6.1 Mbit/s. Fixing only that boundary moved 24 drops to the finite Pack wait and measured 3.5 Mbit/s. Making both reliable boundaries lossless produced consecutive 38/41/52-Mbit/s displays, zero drop deltas, zero final reorder bytes, a 17.60-MiB active peak, and a 17.61-MiB recovery p95. | Retain fixed queue/byte gates and lossless H1 waits to cancellation. Page and recovery acceptance pass; use timeout-recovery per MiB—not more depth—as the next optimization signal. |
| 2 | Download Transfer-message amplification | The hybrid H3 ceiling made deployed H1 provider returns singleton full-MTU messages. The retained local group halves their H1 sequence numbers/wire Packs and cuts boundary allocations 65.3%. | Validate exact payload/goodput in PERFVAR when Redis/DB is restored, then on the attached device against that provider. |
| 3 | Public provider/exit deployment | Same-session public-provider results remained slow even after client lanes, while the new pinned current-code provider crossed 40 Mbit/s once both H1 handoffs were lossless. | Deploy the verified carrier/Pack behavior with the provider grouping/direct-ACK work, then repeat pinned old/new and public-exit cohorts. Do not infer public fleet speed from the local controlled provider. |
| 4 | Hot packet-root pressure | Fresh pages stay below the gate, but overlapping fast.com work drove mixed ACK/data drops and runtime growth. Raising roots to 4,096 removed drops without improving speed; receive 128 exceeded 24 MiB. | Reduce messages/allocations upstream with provider grouping. Do not buy speed by weakening the memory backstop. |
| 5 | WebSocket/TLS socket boundary | A 32-message/12-KiB client drain improves ACK-sized host work 16.7% and reduces writes/frame 36% while full payload and sparse traffic remain neutral. After exact receive charging, the final physical arm reached a 2.78-Mbit/s Cloudflare median at a 21.77-MiB active peak. | Retain the bounded ready-only drain with no timer. Attribute the remaining Internet gap only through a controlled old/new provider and adjacent device A/B. |
| 6 | GC/pool retention | Payload-only receive accounting still allowed 6.15 MiB of packet roots and a 29.59-MiB runtime crest. Charging every retained root/owner to one shared budget reduced roots to 1.78 MiB and runtime to 21.77 MiB; quiet p95 was 20.16 MiB without forced GC or trim. | Keep exact retained-allocation accounting and the 256-KiB warm set. Do not replace this bound with more aggressive periodic collection. |

The 40-Mbit/s target was not unlocked by one more ACK queue or deeper sequence.
It was unlocked by making both bounded handoffs above reliable H1 lossless.
The controlled provider moved from 6.1 Mbit/s with 530 carrier drops, through
3.5 Mbit/s when loss moved to the finite Pack boundary, to consecutive 38, 41,
and 52 Mbit/s with zero carrier/Pack drops and zero final reorder bytes. Runtime
peaked at only 17.60 MiB and recovery p95 was 17.61 MiB. Provider timeout recovery remains the next measured
efficiency target, but it is no longer evidence for buying more receive depth.
Public-fleet deployment and an iOS extension footprint run remain distinct
release gates.

## 2026-08-26 cross-project and two-device research pass

This pass tested the eight limiter directions above on both attached Android
devices, then compared the results with the designs used by WireGuard,
wireguard-go, Tailscale, gVisor, DPDK, VPP, quic-go, and BoringTun. The useful
lesson is not that this data path should imitate a kernel VPN wholesale.
Android `VpnService` exposes one IP packet per TUN read and accepts one IP
packet per TUN write, so Linux-only TUN GSO/GRO, `sendmmsg`, kernel DCO, and
busy polling are not available at this boundary. The transferable ideas are
bounded ownership, draining all work that is already ready, fixed worker
pools, flow locality, and avoiding allocation on packet/ACK accounting paths.

### What the other implementations establish

| Project / technique | Primary-source result | Transfer to this mobile H1 path |
| --- | --- | --- |
| [WireGuard paper](https://www.wireguard.com/papers/wireguard.pdf) and [kernel-integration paper](https://www.wireguard.com/papers/wireguard-netdev22.pdf) | GSO super-packets let routing and network-stack work be reused across a packet cluster; the paper reports sending gains around 35% from batching and cache locality. WireGuard uses fixed rings and balances single-flow locality against multiflow parallelism. | Keep one ready drain together through parse/policy/Transfer admission and cache immutable per-flow decisions. Do not add a wait to create a batch. Android does not expose the Linux vnet/GSO metadata needed to create true super-packets. |
| [wireguard-go Android queue policy](https://github.com/WireGuard/wireguard-go/blob/master/device/queueconstants_android.go) and [device workers](https://github.com/WireGuard/wireguard-go/blob/master/device/device.go) | Android deliberately uses smaller fixed queues; buffers come from preallocated pools, and crypto work runs through a fixed worker set rather than one worker graph per packet. | The fixed-capacity byte gates and warm pools are directionally correct. The provider NAT's per-UDP-flow goroutines are the remaining mismatch: flow fanout scales stacks even when packet buffers are bounded. |
| [Tailscale TSO/GRO/mmsg work](https://tailscale.com/blog/throughput-improvements) and [UDP/QUIC follow-up](https://tailscale.com/blog/quic-udp-throughput) | Moving more packets per I/O produced a best-case 2.2x wireguard-go gain, up to 33% on Tailscale Linux, and a 4x UDP gain on bare metal after end-to-end offload work. | The SDK already performs one blocking TUN read, up to 63 nonblocking reads, then one `sendPacketsNoCopy` call. Provider socket reads already drain into bounded logical groups. TUN injection must remain one write per IP packet; `writev` would erase packet boundaries. |
| [gVisor buffer-pooling work](https://gvisor.dev/blog/2022/10/24/buffer-pooling/) | Netstack attributed 20--30% of processing time to allocation/GC; explicit ownership and tiered pools removed 99% of allocations and improved throughput by more than 30%. | Continue tiered exact-size pools and ownership/leak tests, but cap the returned warm floor. This pass removes the last per-ACK RTT heap node; increasing retained pool capacity is rejected because physical spikes were not caused by returned-pool bytes. |
| [DPDK ring/burst API](https://doc.dpdk.org/guides/prog_guide/ring_lib.html), [DPDK poll-mode guidance](https://doc.dpdk.org/guides/prog_guide/ethdev/ethdev.html), and [VPP vectors](https://docs.fd.io/vpp/24.02/aboutvpp/scalar-vs-vector-packet-processing.html) | Fixed rings, bulk/burst operations, run-to-completion processing, and vectors amortize atomics, queue crossings, and instruction-cache work. VPP processes vectors of up to 256 packets. | Use small ready-only bursts at every existing ownership boundary. Mobile fairness and memory bounds matter more than a datacenter-sized vector: the retained provider group remains 16 frames / 24 KiB after the 32/48 candidate failed to show a physical speed win. |
| [Linux receive/transmit scaling](https://docs.kernel.org/networking/scaling.html) | Flow-to-CPU locality reduces cache misses and queue-lock contention; software steering can instead add inter-processor interrupts. | Do not add workers merely because CPUs exist. Pin a flow to one ordered shard and increase worker count only after block/CPU profiles prove one shard saturated. The current one-shard NAT default measured neutral versus eight shards in earlier tests. |
| [quic-go flow control](https://quic-go.net/docs/quic/flowcontrol/) and [GSO notes](https://quic-go.net/docs/quic/optimizations/) | Receive windows must cover BDP, but aggregate connection limits bound memory; Linux GSO can submit up to 64 KiB. | At 40 Mbit/s and 200 ms, BDP is about 1 MiB. Existing shared resend capacity (about 9 MiB provider-off), 768-KiB per-flow logical receive, and multiple H1 flows already cover the target class. Earlier 128-depth experiments consumed more memory without increasing physical speed, so no new window spend is retained. |
| [Linux `MSG_ZEROCOPY`](https://docs.kernel.org/next/networking/msg_zerocopy.html) | Copy avoidance generally pays only above roughly 10 KiB and adds completion/ownership work. | It does not fit MTU-sized Android TUN writes or the current H1 message envelope. Exact pooled copies are cheaper and easier to bound here. Revisit only with a negotiated larger H1 envelope on a platform that exposes completion semantics. |
| [BoringTun](https://github.com/cloudflare/boringtun) | Its portable core is deliberately separate from platform tunnel/network-stack integration and is deployed on iOS and Android. | Preserve the same separation: optimize the shared Transfer core where measured, but keep Android TUN and iOS NetworkExtension constraints explicit. Android runtime measurements predict Go allocation behavior; they do not substitute for iOS `phys_footprint`. |

WireGuard's own [performance roadmap](https://www.wireguard.com/performance/)
lists GRO, lock-free queues, core autoscaling, packet locality, and fair queue
management, while warning that its published benchmark table is old. The
applicable ordering here is locality and bounded bursts first, worker scaling
second, and queue growth only after a measured saturated boundary. Linux
offloads and queue disciplines are useful controls, not directly shippable
Android changes.

### Eight-direction result

| Direction | Measurement in this pass | Decision |
| --- | --- | --- |
| 1. End-to-end hop timing | Schema 12 separates client and provider Pack, ACK-write, initial-write, timeout, and recovery counters. In the reverse physical topology the provider accumulated 7,576 timeout recovery writes while its runtime reached 30.43 MiB; the client phases stayed below 24 MiB. | Keep the primitive provider telemetry. The next timestamp work should be sampled at ownership boundaries rather than adding a clock read to every packet. |
| 2. ACK/RTO behavior | A deterministic snapshot/arrival barrier reproduced a due resend after its covering ACK was already in the coalescer. Rechecking the exact item before the recovery write yields one initial H1 wire write, zero recovery writes, and one recorded preemption. Replacing the RTT pointer heap with a fixed ring plus monotonic minimum deque changed the per-ACK median from 43.64--44.92 ns and 64 B / one allocation to 20.26--20.32 ns and 0 B / zero allocations, about 54% less time. | Keep both changes. The exact check ignores unrelated cumulative/selective progress, so a real hole cannot be postponed indefinitely. |
| 3. Provider-controlled A/B | The devices were cross-pinned by exact peer identity and alternated provider/client roles. This removed public provider selection but not the two different cellular/Wi-Fi exits. The controlled same-device campaign immediately before this pass remains the valid 40-Mbit/s proof: 38/41/52 Mbit/s with zero reliable handoff drops. | Keep exact peer pinning in the procedure. Do not convert the variable two-phone Internet results into a code speedup/regression percentage. |
| 4. Effective BDP/window | No client phase filled a memory ceiling; maxima were 18.57 MiB on the Galaxy and 23.04 MiB on the Pixel. Previous iterative 64 -> 128 depth reached its earned maximum but did not improve fast.com. | Keep H1 receive 64/128 KiB and existing shared byte budgets. Reject further static or iterative depth for production until a fixed route shows outstanding useful bytes actually capped there. |
| 5. fast.com flow distribution | Existing eight-lane work improves page parallelism, but a public provider can still put its return data on lane zero. In this exact-peer matrix, fast.com varied 0.61--7.6 Mbit/s through the tunnel while adjacent Direct Wi-Fi medians were 390 and 960 Mbit/s. | Provider-side symmetric lane deployment and per-lane useful-byte telemetry remain required. Client-only lane or queue expansion is not retained as a 40-Mbit/s fix. |
| 6. Provider return batching | For one 64-packet drain, 16 frames / 24 KiB produced four groups at a 4,959--4,981-ns median, 9,424 B, and 98 allocations. A test-only 32/48 shape produced two groups at 4,371--4,391 ns, 8,424 B, and 82 allocations: about 12% less local time. The physical matrix did not establish an end-to-end speed improvement and the larger owner increases fairness/memory exposure. | Keep the benchmark helper and production-bound regression test; retain production 16/24. A local microbenchmark win alone is insufficient to spend mobile burst memory. |
| 7. Provider IP stack | A fresh synthetic provider run moved 4.5 MiB of echoed TCP at 49.4 MiB/s and measured 30.7 MiB peak runtime / 13.6 MiB peak live heap. After 192 short UDP flows, 621 goroutines remained: 192 socket readers plus 192 per-flow send/idle loops. The physical provider similarly reached 748 goroutines. | Highest-value memory direction is a shared nonblocking UDP socket poller or bounded receive-worker set, not smaller packet pools. It is research-only until it preserves datagram order, idle/lifecycle behavior, and measured UDP latency/throughput. |
| 8. Conditional provider-off budget | Both physical client roles remained below 24 MiB without consuming the extra budget through deeper queues. Direct-versus-tunnel gaps persisted even with ample memory. | Leave unused provider-off headroom as safety margin. Spend it only on a boundary that first reports saturation and then improves a same-route A/B. |

Only the RTT representation, exact ACK-pending resend preemption, schema-12
provider attribution, and their deterministic tests remain in the candidate.
The 32/48 provider group, deeper windows, larger pools, shorter provider idle
timeouts, extra NAT shards, and zero-copy paths are not enabled in production.
The complete benchmark-only server gate then passed all 190
`server/connect`, 10 `server/connect/perfvar`, and 20 `server/proxy` samples.
The production ACK-sized H1 TLS median was 371.5 ns with 10 B and two
allocations per operation; PERFVAR receive-credit was 681.0 ns, and proxy
batch-64 was 5,526 ns. These are current-source guard values, not a detached
baseline comparison; the changed RTT path is exercised in Connect, not in the
server relay/proxy packet loops.
The complete Connect and SDK short suites passed in 198.041 and 99.190
seconds, both affected vet trees passed, and focused normal/race repetitions
passed. The Android schema parser's ten privacy/eligibility tests also passed.

The deterministic root-cause gate is:

```sh
go test . -run '^TestH1ReadyHotPathAckSuppressesEveryRecoveryWrite$' -count=100
go test -race . -run '^TestH1ReadyHotPathAckSuppressesEveryRecoveryWrite$' -count=10
go test . -run '^TestSequenceAckWindowDueDispositionIgnoresUnrelatedProgress$' -count=100
go test . -run '^$' -bench '^BenchmarkRttWindowCloseSendTime$' -benchmem -count=7
```

The first test places a cumulative ACK after the sender's empty snapshot and
before its already-due recovery write. It must observe exactly one physical
write for the H1 Pack, zero recovery writes, an empty resend queue, and exactly
one `ack_pending_resend_preempts` event. The second test proves unrelated ACK
progress does not suppress a real hole. The benchmark must stay at zero B/op
and zero allocations/op. On a physical clean H1 phase, compare counter deltas,
not cumulative totals: carrier/Pack drops and recovery errors must remain zero;
timeout writes should be explained by actual missing receiver progress, while
ACK-pending preemptions identify races safely avoided. This combination catches
both forbidden duplicate writes and an over-broad optimization that hides loss.

### Two-device physical matrix

Both devices ran the final schema-12 artifact as long-lived authenticated
sessions. Each became provider once; the other device then alternated Wi-Fi,
same-LAN P2P, and cellular. Chrome used cache-disabled DevTools navigation to
real Wikipedia pages and canonical fast.com runs. Direct controls ran after
disconnecting the tunnel. One cellular page attempt ended when Chrome closed
the DevTools WebSocket; it is recorded as a harness failure and excluded from
the stable seven-page cohort, not silently retried into that cohort.

| Provider -> client / path | Wikipedia median load / document TTFB | fast.com displays | Runtime result |
| --- | ---: | ---: | --- |
| Pixel -> Galaxy, Wi-Fi H1 | 766.3 / 182.1 ms | 0.61, 3.6, 5.0 Mbit/s | Galaxy client max 17.19 MiB |
| Pixel -> Galaxy, same-LAN P2P | 1,025.1 / 290.2 ms | 2.7 Mbit/s | Galaxy client max 18.57 MiB |
| Pixel -> Galaxy, cellular H1 | 543.1 / 242.3 ms | 4.7, 7.6, 9.4 Mbit/s | Galaxy client max 18.46 MiB |
| Galaxy -> Pixel, Wi-Fi H1 | 6,459.3 / 927.3 ms | 0.73, 0.58, 0.61 Mbit/s | Pixel client max 22.78 MiB |
| Galaxy -> Pixel, same-LAN P2P | 1,451.3 / 236.6 ms | 2.9 Mbit/s | Pixel client max 23.04 MiB |
| Galaxy -> Pixel, cellular H1 | 720.2 / 301.2 ms | 1.1, 6.5, 5.3 Mbit/s | Pixel client max 22.85 MiB |
| Pixel Direct cellular | 359.7 / 165.3 ms | 32, 34, 1.5 Mbit/s | Tunnel stopped |
| Pixel Direct Wi-Fi | 293.5 / 115.4 ms | 390, 350, 390 Mbit/s | Tunnel stopped |
| Galaxy Direct cellular | 266.0 / 96.0 ms | 1.4, 1.5, 1.5 Mbit/s | Tunnel stopped |
| Galaxy Direct Wi-Fi | 164.4 / 63.6 ms | 730, 960, 960 Mbit/s | Tunnel stopped |

The Wi-Fi controls prove both radios and fast.com targets had far more than
40 Mbit/s available. The cellular controls also show why a displayed speed
alone is not a stable cross-arm benchmark: one carrier/target cohort was only
1.5 Mbit/s even though page TTFB was 96 ms. The exact-peer tunnel results are
therefore useful failure attribution, not evidence that the previously
measured 38/41/52-Mbit/s lossless-H1 result disappeared.

Across 71 Pixel and 70 Galaxy samples, Pixel runtime peaked at 26.12 MiB with
five samples above 24 MiB and none above 28 MiB. Galaxy peaked at 30.43 MiB
with 16 samples above 24 MiB and ten above 28 MiB. Every client/P2P/cellular
phase stayed below 24 MiB; all violations were provider work:

- Pixel provider: 26.12-MiB runtime max, 11.23-MiB live-heap max, 1.32-MiB
  packet-outstanding max, and 4,458 provider timeout writes.
- Galaxy provider: 30.43-MiB runtime max, 13.99-MiB live-heap max, 1.78-MiB
  packet-outstanding max, and 7,576 provider timeout writes. At the maximum,
  returned packet-pool storage was only about 0.21 MiB while 748 goroutines
  were live.
- After provider work stopped, Galaxy first recovered to 19.89 MiB before
  automatic reclaim; its final finish sample was 16.95 MiB after one idle
  trim/forced GC. Pixel's final finish sample was 19.91 MiB with no forced GC.
  The active excess was live provider/NAT/Transfer state plus
  goroutine/allocator-span retention, not a returned-pool high-water that a
  harsher reclaim timer alone could solve.

This final physical matrix therefore fails the active provider <=24-MiB and
zero->28-MiB gates even though client mode passes. It also does not provide a
new 40-Mbit/s public-route pass. Release work must preserve the already proven
lossless-H1 throughput while replacing per-flow provider scheduling state with
bounded workers and then repeat provider-on/off/on steady recovery on both
devices. An iOS Network Extension `phys_footprint`/jetsam run remains the final
memory authority.

### Next measured research order

1. Add primitive provider TCP/UDP/ICMP flow counts and socket-worker counts to
   the sampler. Validate that the counter path allocates nothing.
2. Prototype one provider-only UDP poller/sharded event loop. Against the
   existing 192-flow fixture, require fewer than half the current 384 UDP
   flow goroutines, at least 2 MiB lower peak runtime, identical payload/order,
   and no loss in UDP requests/s or p95 latency. Keep the current design if it
   misses any condition.
3. Deploy the retained provider ACK/group/reliable-handoff code to one fixed
   exit and alternate old/current/current/old. Record useful bytes per lane,
   timeout writes, ACK-pending preemptions, and CPU per delivered MiB.
4. Add sampled stage deltas for TUN read -> client Pack -> H1 write -> server
   relay -> provider Pack -> NAT socket -> return H1 -> TUN write. Sampling
   must be deterministic and below 0.5% CPU in the local throughput benchmark.
5. Only if H1 socket writes remain limiting, negotiate a larger carrier
   envelope and sweep 4/8/16/32 KiB under the same 24-MiB gate. Do not infer a
   win from logical grouping without physical-write and device goodput deltas.
6. Profile route/policy lookup reuse across one ready burst, following
   WireGuard's once-per-cluster route-cache pattern. Retain only a cache with
   explicit invalidation and an end-to-end CPU/TTFB gain.
7. Measure scheduler locality before changing shard counts. A new worker must
   reduce block/mutex/CPU cost on both phone classes and must not increase
   active runtime or sparse request TTFB.
8. Re-run H3/DNS separately in the future iteration. Their UDP/QUIC flow
   control and datagram loss semantics differ from default H1; none of the H1
   Internet rates in this pass are H3/DNS performance evidence.

## 2026-08-26 CNN H1 poison recovery and P2P-memory follow-up

This pass used the two attached Android phones as long-lived, production-rate
(`memprofilerate=0`) sessions. Each phone used public United States H1 over
Wi-Fi and cellular, and each direction of an exact-ID same-LAN P2P pairing.
Chrome loaded the real CNN Nepal flooding live-news page and started its main
video. Both public paths and both P2P directions advanced through preroll into
news footage. A reused Chrome media session deliberately carried across an
egress change returned CNN error 1400899; a fresh page connection on the now
stable P2P route played. That is the intended boundary: never change egress IP
inside an established media connection; retire a confirmed poisoned H1
connection and let Chrome establish a healthy one.

No local client or provider security-block counter advanced during any
successful playback. Provider diagnostics now cross the complete
Connect -> SDK -> RPC/mobile boundary: availability, provider build, effective
policy hash, source-scoped ingress/egress packet and byte counters, and
publication sequence. The physical harness records only availability/count
aggregates, not provider/client identities or policy strings.

### Remediation and deterministic gate

A sustained no-receive quarantine now performs one bounded recovery action:

- erase DNS-name/address hints, app affinity and site affinity associated with
  the poisoned exit;
- make every new handshake scatter instead of inheriting a quarantined donor;
- rebind only established UDP/443 flows that can move safely;
- remove established TCP flow/path bookkeeping and synthesize a source-side
  RST so Chrome retries, rather than silently waiting on the old H1 socket;
- retain established-session egress identity until that explicit teardown;
- bound one exit's sticky graph to 64 flows, retiring only its oldest idle
  entries after the 30-second idle threshold;
- expose affinity invalidations, quarantine TCP resets and sticky retirements
  in reliability metrics.

The provider publishes identity on first authenticated source traffic and
republishes only when that source's block generation changes. Diagnostics are
strictly lower priority than a synthesized policy RST. The deterministic SMTP
test uses a one-slot paused send sequence; the RST must occupy it and the
diagnostic attempt must never displace/drop the reset.

The root-cause commands are:

```sh
go test . -run '^TestH1DashBlackholeQuarantineResetsAndRebinds$' -count=100
go test . -run '^TestFreshHandshakeNeverFollowsQuarantinedAffinityDonor$' -count=100
go test . -run '^TestFlowReaperBoundsStickyExitWithOldestIdleFlows$' -count=100
go test . -run '^TestProviderSmtpRejectionReturnsTcpReset$' -count=100
go test . -run '^(TestSecurityPolicyHashIdentifiesEffectiveRules|TestProviderDiagnosticsAreSourceScopedAndGenerationGated|TestProviderDiagnosticsFrameAndChannelOrdering)$' -count=100
go test -race . -run '^(TestH1DashBlackholeQuarantineResetsAndRebinds|TestFreshHandshakeNeverFollowsQuarantinedAffinityDonor|TestProviderSmtpRejectionReturnsTcpReset|TestProviderDiagnostics.*)$' -count=10
```

The DASH test establishes several CNN-shaped H1 media flows through one exit,
blackholes it after establishment, matures the no-receive verdict, and requires
DNS/site-affinity invalidation, prompt TCP RSTs, removal of every poisoned flow,
and a fresh handshake on the healthy exit. It fails if an established H1 flow
is silently rebound (mid-session IP change), if a new flow follows the
quarantined donor, or if recovery waits for ordinary TCP timeout.

### Physical playback and memory measurements

The sessions ran for about 27 minutes each and finished normally. Wi-Fi was
restored, both temporary authenticated clients were released, and credentials,
peer pins and device test artifacts were removed.

| Role and real workload | Go runtime | Live heap | Goroutines | Packet-pool ownership | Result |
| --- | ---: | ---: | ---: | ---: | --- |
| Pixel provider -> Galaxy P2P client, CNN playback | 38.84 MiB peak | 21.54 MiB peak | 1,346 peak | 0.39 MiB outstanding; 0.25 MiB returned | Playback completed; provider memory fails 24/28-MiB gates |
| Galaxy P2P client | 23.40 MiB p50, 23.95 MiB p95, 24.02 MiB max | 9.25 MiB peak | 209 peak | 0.006 MiB outstanding; 0.25 MiB returned | Playback completed |
| Galaxy provider -> Pixel P2P client, CNN playback | 39.26 MiB peak | 22.35 MiB peak | 1,071 peak | 1.31 MiB outstanding; 0.25 MiB returned | Playback completed; provider memory fails 24/28-MiB gates |
| Pixel P2P client | 23.46 MiB p50, 23.73 MiB p95, 23.74 MiB max | 8.35 MiB peak | 263 peak | 0.29 MiB outstanding; 0.25 MiB returned | Playback completed |
| Pixel public cellular H1, 35 samples | 23.89 MiB p50, 24.23 MiB p95, 24.39 MiB max | 8.47 MiB peak | 264 peak | 0.04 MiB outstanding; 0.25 MiB returned | The initial spinner advanced into CNN footage; zero block counters |
| Final disconnected state, Pixel / Galaxy | 23.70 / 22.38 MiB | 7.39 / 7.95 MiB | 208 / 193 | 0.25 / 0.25 MiB returned | Both below 24 MiB after teardown |

An exact-final-artifact smoke then rebuilt the SDK and APKs with provider build
`cnnfixmem-20260826-d` and repeated the real CNN page in both P2P directions.
Both phones advanced through a video advertisement and into/through moving
media. Each H1 client saw one provider diagnostic with a nonempty build and
policy hash; local client, local provider and source-scoped remote provider
block counts were all zero. Galaxy-provider -> Pixel-client moved 7.16/2.67
MiB through the provider and left provider/client runtime at 29.34/19.78 MiB;
Pixel-provider -> Galaxy-client moved 5.52/1.80 MiB and left them at
30.38/21.83 MiB. The complete 5.8-minute role-reversal sessions peaked at
32.23 MiB on Pixel and 31.16 MiB on Galaxy, with 4/11 samples over 28 MiB,
0.39/0.28 MiB maximum packet ownership, 0.25 MiB returned packet pools, and
zero packet-pressure, H1 receive-queue-drop or H1 receive-backpressure events.
This confirms the final mobile telemetry surface and playback remediation, but
also independently reproduces the provider-memory failure on a short run.

The subsequent pull brought two generated IP-security/blocker revisions, so a
new pushed-head artifact was mandatory rather than treating the prior APK as
final. Build `cnnfixmem-20260826-e` used Connect `b35e03f`, SDK `702356e`, and
Android `683d0d3e`. Fresh cache-busted CNN sessions again reversed the exact
same-network provider/client roles; both phones progressed through advertising
and into moving CNN footage. Each client saw one diagnostic with the stamped
build and the new effective-policy hash, and every local/provider block counter
remained zero. Pixel/Galaxy client snapshots were 19.09/21.98 MiB. The complete
4.1-minute sessions peaked at 32.02/31.96 MiB runtime, 17.30/17.37 MiB live
heap, and 1,032/1,066 goroutines, with 4/5 samples over 28 MiB. Packet ownership
peaked at only 0.28/0.32 MiB, returned pools at 0.25 MiB, and packet-pressure,
H1 queue-drop and H1 backpressure counters remained zero. This pushed-head run
supersedes the earlier artifact for functional release evidence and preserves
the same provider-memory failure.

A final generated IP-security/blocker revision (`8b1d9bf`) landed during
closeout, so build `cnnfixmem-20260826-f` was rebuilt from that exact policy and
run in a fresh Galaxy-provider -> Pixel-client P2P session. The real,
cache-busted CNN page loaded over explicit H1; its preroll advanced into moving
CNN flood footage. The client published one provider diagnostic with nonempty
build and effective-policy hashes, and local client, local provider, and
source-scoped remote-provider block counters were all zero. At the playback
snapshot the client/provider runtimes were 22.56/33.85 MiB. Across the complete
162/168-second instrumentation sessions they peaked at 23.31/39.41 MiB, with
zero/seven samples over 28 MiB, 210/1,392 goroutines, only 0.33/0.28 MiB maximum
packet ownership, 0.25 MiB returned packet pools, and zero packet-pressure,
H1 receive-drop, or H1 receive-backpressure events. Both tests passed, both
temporary clients were released, and private artifacts were removed. This
latest-policy check closes functional source parity; the 39.41-MiB provider
crest again fails the memory gate and reinforces per-flow/provider concurrency,
not returned pools or security drops, as the remaining streamline target.

Final-source correctness and adjacent server performance gates are green. The
complete Connect package passed in 440.220 seconds, all remaining Connect
subpackages passed, the affected blackhole/affinity/sticky/provider-diagnostic
selection passed three times under the race detector, and the complete short
SDK suite passed in 100.040 seconds. All 210/10/20 benchmark samples in
`server/connect`, `server/connect/perfvar`, and `server/proxy` passed. Current
production H1 full-payload/ACK-sized medians were 908.5/371.4 ns with unchanged
17/10 B/op and two allocations, +2.75%/+1.28% versus the adjacent pre-change
cohort. PERFVAR receive-credit improved 673.4 -> 651.0 ns (-3.33%), while proxy
batch-64 moved 5,444 -> 5,523 ns (+1.45%) with identical allocation shapes.
The small opposing host shifts and unchanged allocations do not identify a
performance regression from the recovery/diagnostic work.

After the pull, the complete benchmark tiers again passed all 210/10/20
samples. The thermally later absolute medians were 962.0/388.5 ns for
production H1 full/ACK-sized work, 664.1 ns for PERFVAR receive-credit, and
5,848 ns for proxy batch-64, with unchanged allocation shapes. To distinguish
host drift from the change, a clean parent/current/parent bracket compared
Connect parent `dacaa01` with `b35e03f` for seven 500-ms samples per benchmark.
Parent full-payload medians opened/closed at 945.8/955.1 ns; current was 956.6
ns, +0.65% versus the interpolated parent. Parent ACK-sized medians were
383.7/385.3 ns; current was 387.8 ns, +0.86%. All arms retained 17/10 B/op and
two allocations. Those sub-1% bracketed shifts are within host noise and show
no material shared-server performance regression.

The Pixel session recorded 20 samples over 28 MiB and the Galaxy 15. They were
provider-active or provider-span-recovery samples, not client-only P2P steady
state. A later Pixel public-Wi-Fi phase inherited the earlier provider arena
high-water and reached 25.63 MiB; therefore this long-session result does not
claim that switching provider off instantly restores the clean client ceiling.
It does show normal teardown eventually returning both devices below 24 MiB.

The earlier approximately 49-MB provider / 29-MB client observation is not
reproduced as an exact magnitude: this run reached about 41.2 MB provider and
25.2 MB client in decimal bytes. Real ad/media fanout changes between runs, so
that difference is not attributed to a code improvement. The important result
is directionally stable: the provider role, not returned pools and not the
client role, owns the large crest.

### What is allocated

A fresh current-source synthetic provider profile moved 4.5 MiB of echoed TCP
at 53.4 MiB/s, then left 192 short UDP flows alive. It measured 30.9 MiB peak
Go runtime, 14.8 MiB peak heap, 10.5 MiB loaded heap and 621 loaded goroutines
(179 before load). A repeat reached 31.2 MiB and tripped the deliberately tight
31-MiB host regression ceiling, confirming that this is active headroom, not a
comfortable pass. The grouped goroutine profile is decisive:

- 192 goroutines were blocked in each connected UDP socket's `Read`;
- 192 more were in each `UdpSequence.Run` send/idle loop;
- only four goroutines served the already-shared ordered receive dispatcher;
- the remainder was gVisor/Transfer/device/provider fixed work.

At a 64-KiB heap-profile sampling rate, loaded in-use attribution included
about 3.12 MiB from live message-pool acquisitions, 1.02 MiB from the bounded
warm set, 0.64 MiB in UDP reader buffers, 0.44 MiB in UDP sequence construction,
0.39 MiB in receive-dispatch structures and 0.19 MiB in the sequence run loop.
Those samples are directional rather than exact accounting, but they agree
with the topology. `messagePool.take` means a live acquisition attributed to
that allocator; it is not evidence that the returned free list owns 3.12 MiB.

The device's exact pool counter resolves that ambiguity: total returned pool
storage at the provider peaks was only 0.26--0.29 MiB. Detailed post-peak
snapshots instead showed:

- Pixel provider: 30.35 MiB runtime = 10.83 MiB live heap, 7.34 MiB stacks,
  6.06 MiB heap fragmentation, plus runtime metadata; returned packet pool
  0.25 MiB.
- Galaxy provider: 34.70 MiB runtime = 16.32 MiB live heap, 5.88 MiB stacks,
  6.67 MiB heap fragmentation, plus runtime metadata; returned packet pool
  0.25 MiB.

Therefore a harsher pool reclaim or a larger pool does not address the active
crest. Pools remain useful for hot allocation/GC cost, but the reclaimable
free list is two orders of magnitude smaller than the approximately 15-MiB
reduction needed to move a 39-MiB provider below 24 MiB. The active excess is
the combination of per-flow socket/read/send state, hundreds of goroutine
stacks, live packet/Transfer state and allocator spans created by that fanout.
Outer H1 does not eliminate this: Chrome can still create inner DNS and
UDP/443/QUIC attempts while the tunnel carrier itself is H1.

### Resolution candidates, in measured order

1. **Connected-socket readiness poller.** Keep one connected UDP socket/NAT
   port per flow, but replace its two parked goroutines with Android/Linux
   epoll and iOS kqueue readiness shards. A bounded worker drains every ready
   socket nonblocking, then performs the existing bulk callback. This preserves
   tuple identity and reply demultiplexing. A naively shared unconnected socket
   does not: two inner source ports contacting the same remote tuple become
   ambiguous. Prototype platform implementations behind the same interface.
2. **Collision-safe socket sharing.** As a harder second prototype, use a
   small socket/port set plus a remote-tuple map, falling back to a distinct
   socket whenever two inner flows would collide. This can reduce descriptors
   as well as goroutines but must prove source-port/NAT behavior, ICMP error
   attribution, IPv4/IPv6 parity and sender isolation. Do not ship it merely
   because a single-server benchmark passes.
3. **Adaptive UDP lifecycle.** Preserve the 300-second provider timeout for
   VoIP, games and long-lived UDP. Measure a short post-response timeout only
   for classifiable one-shot DNS/probe flows, and prompt retirement for a
   quarantined exit. A blanket shorter timeout is rejected: it can break a
   quiet healthy session and does not reduce memory while traffic is active.
4. **Flatten per-flow live objects.** The 192-flow heap profile gives a much
   smaller but real second target: make the send queue lazy/smaller only after
   queue-HWM telemetry proves the slots unused; keep addresses inline; avoid
   closures/timers created per flow; and isolate the socket-reader's blocking
   frame from deep callback frames. Retain a change only if p95 UDP latency and
   goodput do not regress.
5. **Fragmentation-aware construction.** Batch or arena-like ownership is safe
   only inside one flow lifecycle and with an exact release point. Measure
   size-class/span changes; do not introduce an unbounded object pool. Forced
   GC/trim can return idle spans but is not an active-memory fix and can worsen
   page TTFB.
6. **TCP and Transfer fanout accounting.** Add allocation-free primitive
   counts for live TCP/UDP/ICMP flows, socket readers and return workers. The
   current synthetic result isolates UDP, while CNN also creates TCP/media and
   Transfer state. The device sampler must show which population tracks each
   crest before shrinking a functional cold-page floor.

The first retained prototype must reduce the 192-flow incremental goroutine
count from 384 to at most 32, cut fresh-process peak runtime by at least 4 MiB,
preserve every datagram and its per-flow order, and match or improve UDP p95
latency/requests per second. That is only the first step: the physical release
gate remains active/steady provider p95 at or below 24 MiB, zero samples above
28 MiB, successful CNN playback in public Wi-Fi/cellular and both P2P
directions, and no regression from the controlled H1 fast.com 38/41/52-Mbit/s
cohort. Reclaim, flow lifetime and object-layout candidates should be combined
only after each wins its own same-route A/B.

## 2026-08-27 fresh-flow placement: affinity as evidence, not assignment

The Bloomberg device trace exposed a routing mistake adjacent to the provider
memory work. A long-lived page H2 connection was acting as a hard IP/domain
affinity donor for later media connections. That policy bypassed the provider
race before quality, external reputation, or endpoint-specific history could
matter. It also retained DNS name/address-to-channel maps that were not useful
once ordinary flows stopped consuming hard affinity.

The transport boundary is exact:

- an established TCP/TLS tuple cannot move because providers terminate TCP;
- six HTTP retries multiplexed on the same Chrome H2 connection cannot invoke
  MultiClient placement again;
- a new source port/SYN is a fresh placement opportunity;
- explicit app/host pins remain the opt-in when one egress IP is required;
- ordinary fresh flows now reach the provider race by default. Setting
  `FreshFlowAffinity=true` restores legacy hard IP/domain inheritance for an
  A/B run, but is not the production policy.

`DestinationAffinity` remains enabled as bounded grouping metadata. A group is
now a performance key, not permission to assign a provider. With hard fresh
affinity off, DNS-exit hints are neither written nor read; this avoids retaining
up to two 4,096-entry, one-hour maps of provider channel references. Existing
flow tables and exact-tuple routing are unchanged.

### TLS-blind performance model

For TCP/443, the client measures cumulative inner-TCP ACK sequence progress.
The first ACK is only the origin's random sequence baseline. Duplicate,
reordered and regressing ACKs contribute nothing; signed 32-bit deltas preserve
sequence wrap. One-second peak-rate buckets and a 250-ms floor on a final
partial bucket reject compressed ACK bursts without delaying a fresh-flow
decision.

Evidence is keyed by both canonical domain constellation and exact destination
IP. Domain evidence carries a result to the next selected video, while exact-IP
evidence prevents a fast page endpoint from hiding a weak media endpoint. The
conservative score is the minimum matching posterior. An unmeasured provider's
score is its advertised `EstimatedBytesPerSecond`, the null hypothesis requested
for this research. For one provider/destination:

```text
weight = min(round(active time, 100 ms), 10 s)
       + min(ACKed bytes / advertised bytes-per-second, 10 s)

posterior = (advertised rate * 1 s + sum(peak rate * weight))
          / (1 s + sum(weight))
```

Combined evidence is capped at 30 seconds. The session-local table is capped at
128 entries and expires after ten minutes. Candidates within the configured
10% placement hysteresis of the best score remain in the race. Equal short
histories therefore remain exactly tied, and if every provider is equally weak
the field fails open instead of making the site unroutable. Established flows
are never moved or closed by this learner. External provider probes remain the
only safe source for domain-specific HTTP challenge reputation because Connect
does not decrypt TLS.

Completed history alone is insufficient for a browser that leaves its page H2
connection open. At each fresh TCP/443 race, the scorer therefore also walks
the existing per-provider `clientUpdates` set (normally bounded by the
16-flow exit cap) and snapshots matching live flows. This is not a new retained
index: it uses the exact-IP/canonical-group keys already owned by each flow,
takes only the existing leaf flow lock, and never adds work or a shared lock to
the ACK hot path. A low-rate still-open page/media connection can therefore
anti-bias the next selected video's genuinely fresh socket; equal low-rate live
connections still have equal weight. Requests reused on that same H2 transport
remain outside the placement boundary.

### Deterministic and cost gates

The current tests cover:

- default ordinary-flow race, explicit-pin inheritance, and legacy A/B restore
  for IPv4 plus DNS hints;
- first-ACK baseline, duplicate/reorder suppression, sequence wrap, and
  partial-window ACK-compression capping;
- advertised-rate null, a low measured provider losing to an unmeasured peer,
  equal short outcomes retaining the whole field, and a fast result remaining
  eligible;
- a still-open H2 flow steering the next fresh full-field race, equal live
  outcomes preserving the full race, a live donor check in legacy mode,
  established-flow immobility, bounded table/TTL, metrics reset/snapshot, and
  Connect-to-SDK round trips;
- six opaque TLS response bursts on one established flow, matching Chrome's
  same-H2 retries and proving they are progress rather than a blackhole or new
  placement event.

Five 500-ms M4 Pro repetitions measured median advancing/duplicate ACK costs of
9.426/1.003 ns/op. Cold, completed-history, and still-open-live fresh-race
scoring measured 137.9/195.7/198.7 ns/op. Every benchmark was 0 B/op and zero
allocations. The advancing production path reuses the timestamp already
required by sequence accounting; a duplicate does not read the clock. Per live
flow the meter is only counters/timestamps. The history is bounded, stores
stable provider IDs instead of channel pointers, and allocates lazily after
completed evidence. The candidate scorer uses a fixed 16-entry stack array and
fails open for an unexpectedly wider runtime override rather than turning exit
count into an allocation vector.

### Physical failure and memory bracket

The pre-policy two-device trace must remain the control, not be rewritten as a
fix result:

| Role/path | Samples | Runtime peak | Live-heap peak | >28-MiB samples | Goroutine peak | Packet-root peak | Returned packet storage |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| Pixel client, public H1 and P2P | 76 | 20.26 MiB | 6.96 MiB | 0 | 257 | 0.39 MiB | 0.25 MiB |
| Galaxy, including P2P provider | 76 | 30.38 MiB | 15.52 MiB | 7 | 740 | 1.05 MiB | 0.25 MiB |

Direct Galaxy cellular playback advanced to 1.674 seconds in five seconds with
13.34 seconds buffered. Public United States H1 on both devices and same-LAN
P2P remained at time zero; P2P nevertheless returned about 10.5 MiB and had
zero local/provider security blocks. The provider memory crest is again live
heap plus flow/goroutine topology, not a returned-pool high-water. The default
affinity change should reduce stale DNS reference retention and give every new
media connection another provider-selection opportunity, but neither playback
nor memory improvement may be claimed until the rebuilt artifact is measured.

### Current-source two-device result

The rebuilt default-off policy was measured in two approximately 20.25-minute
Android sessions. Each phone used validated Wi-Fi and validated cellular in
opposite arms, then one phone provided an exact-ID same-LAN P2P exit to the
other. These are variable public-route observations, not a paired provider
benchmark:

| Client path | Wikipedia median document TTFB | Wikipedia median load | fast.com samples | fast.com median |
| --- | ---: | ---: | --- | ---: |
| Pixel Wi-Fi, public United States H1 | 153.1 ms | 550.6 ms | 61 / 40 / 110 Mbit/s | **61 Mbit/s** |
| Galaxy cellular, public United States H1 | 355.2 ms | 1,886.0 ms | 0.63 / 0.76 / 0.68 Mbit/s | 0.68 Mbit/s |
| Pixel cellular, public United States H1 | 453.8 ms | 979.7 ms | 6.3 / 0.55 / 9.6 Mbit/s | 6.3 Mbit/s |
| Galaxy Wi-Fi, public United States H1 | 174.3 ms | 402.0 ms | 18 / 0.96 / 4.4 Mbit/s | 4.4 Mbit/s |
| Galaxy provider to Pixel client, exact P2P H1 | 168.7 ms | 1,233.8 ms | 3.5 / 3.5 / 3.5 Mbit/s | 3.5 Mbit/s |

The 61-Mbit/s median demonstrates that the policy does not impose a 40-Mbit/s
ceiling and restores the requested class on one real public route. The other
arms demonstrate why this is not a universal 40-Mbit/s claim: exit quality and
radio path still dominate. After public traffic, the two clients reported
395/3,129 and 319/7,280 performance samples/candidates-filtered respectively.
The learner was therefore active in the fresh races. Donor-bypass remained
zero, as expected when legacy hard inheritance is disabled rather than entered
and rejected.

Bloomberg playback succeeded on the Galaxy Wi-Fi public-H1 arm: the media clock
advanced from 8.718 to 9.724 seconds with `readyState=4` and about 20.26 seconds
buffered. Five encrypted Fetch responses with status 403 occurred on one reused
H2 connection during that successful playback. The Pixel cellular arm did not
advance and had seven 403 Fetch responses on one reused H2 connection. Forcing
a genuinely fresh Chrome transport afterward produced a top-level document
403 on a new H2 connection, which Chrome did not retry. The P2P exit likewise
returned a fresh document challenge while reporting a nonempty provider build
and policy identity and zero provider-side block counters. This proves all
three boundaries: same-H2 HTTP retries cannot be reraced; fresh-flow placement
does get another chance; and another provider may still have the same
destination-specific reputation. Public exits exposed no build/policy
diagnostics, so deployment of external reputation metadata remains unverified.

Memory remained streamlined for the client but not for the provider:

| Role | Samples | Runtime peak / p95 | Quiet-window p50 / p95 / range / last | >24 / >28-MiB samples | Live-heap peak | Goroutine peak | Packet-root / returned-pool peak |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| Pixel client | 81 | 22.00 / 21.61 MiB | 21.02 / 21.70 / 20.51--22.00 / 20.57 MiB | 0 / 0 | 7.97 MiB | 233 | 0.35 / 0.25 MiB |
| Galaxy, including provider | 81 | 29.45 / 26.74 MiB | 24.01 / 25.20 / 23.10--25.20 / 23.56 MiB | 27 / 2 | 11.97 MiB | 340 | 1.22 / 0.25 MiB |

Each quiet window contains 20 primitive samples. The client passes the active
and steady 24/28-MiB gates. The provider does not: even after traffic quieted,
11 of its 20 samples remained above 24 MiB. At the 29.45-MiB runtime crest the
live heap was about 11.90 MiB, 304 goroutines were live, packet roots were only
about 0.57 MiB, returned packet storage was about 0.25 MiB, one automatic GC
had occurred, and queues were empty. Across both sessions there were zero
packet-pressure drops and zero H1 receive-queue drops. This repeats the live
provider flow/goroutine-topology attribution and rejects both hard-affinity
removal and more aggressive returned-pool reclaim as its fix. The bounded
shared provider UDP poller/lifecycle experiment remains the highest-priority
provider-memory direction.

The exact source gates passed: Connect `go test ./... -short -count=1` and
`go vet ./...`, the focused routing tests under the race detector, the complete
short SDK suite, focused server model tests and vet, the current SDK AAR plus
stamped Android app/test/unit build, and all 13 dependency-free Android script
tests. The server database integration fixture remained unavailable because
the documented `WARP_ENV`/vault PostgreSQL inputs were absent; the pure
provider-metadata assembly tests passed.

Release acceptance for this direction is: no fresh ordinary flow directly
inherits an IP/domain donor; retry on an existing H2 connection remains
untouched; a fresh SYN can select another provider; no fast.com or page-TTFB
regression; client steady p95 at or below 24 MiB with zero >28-MiB samples; and
the existing provider-memory gate remains separately open until the shared UDP
poller/lifecycle work reduces its live topology.

## 2026-08-27 provider topology and fresh-race correction

This pass closed the first three provider-memory research directions, then
investigated a real fast.com regression introduced by making TLS-blind
performance evidence load-bearing. Every candidate used a fresh child process;
physical arms used one attached Android phone, validated Wi-Fi, explicit H1,
the production United States pool, a fresh app/client/Chrome session per arm,
the canonical fast.com harness, and primitive Go/queue counters. Public exits
were not pinned, so the physical results are target and regression gates, not a
causal percentage benchmark between provider sets.

### Direction 1: shared provider UDP socket lifecycle

The retained constrained-provider profile replaces one blocking reader, send
loop, send channel and idle timer per UDP flow with:

- one OS-readiness worker per existing receive-dispatch shard and address
  family (`epoll` on Linux/Android, `kqueue` on Darwin/iOS, portable fallback
  elsewhere);
- direct serialized datagram writes, which preserve UDP message boundaries;
- the existing bounded receive dispatcher, which drains all immediately ready
  datagrams in one callback batch; and
- one buffer-level idle-deadline worker.

Construction remains lazy, so a TCP-only provider allocates no poll backend.
The profile is enabled only by provider settings with a nonzero memory target;
generic callers and the target-zero server defaults keep the established
portable topology. Readiness callbacks use `syscall.RawConn.Read` to pin the
descriptor through a drain: an event queued before unregister/close cannot read
a newly reused descriptor into the old flow.

Five fresh 16-packet/24-KiB SDK provider-load repetitions gave:

| Arm | TCP echo median | UDP median | Loaded runtime / live heap | Goroutines peak / loaded | Runtime peak | Decision |
| --- | ---: | ---: | ---: | ---: | ---: | --- |
| Per-flow reader/send/timer control | 53.1 MiB/s | 13,960.4 round trips/s | 27.9 / 10.4 MiB | 641 / 621 | about 31.1 MiB | Control |
| Shared readiness + direct write + shared idle lifecycle | 54.1 MiB/s | 15,414.3 round trips/s | 25.8 / 9.4 MiB | 356 / 242 | about 30.9 MiB | **Keep**: TCP +1.9%, UDP +10.4%, loaded runtime -2.1 MiB, loaded goroutines -61.0% |

All five retained-candidate repetitions stayed below the synthetic 31-MiB
host ceiling. Allocation/CPU profiles explain the direction: sampled GC drain
fell from 0.48 to 0.33 cumulative seconds, and the loaded `time.NewTimer`
allocation sample disappeared. Profiling overhead itself can cross the memory
ceiling, so profile runs remain attribution-only. Ordering, idle reap, absent
per-flow queues, descriptor unregister, race behavior, Linux/Android and
Darwin/iOS compilation are deterministic gates.

### Directions 2 and 3: provider group bound and telemetry

The local 32-packet/48-KiB provider group reduced admission time from 4,929 to
4,344 ns/op (-11.9%) and allocations from 98 to 82 in the narrow microbenchmark.
The complete provider path reversed that result: TCP median fell from 54.1 to
52.7 MiB/s (-2.6%), and two of five repetitions crossed 31 MiB. Production
therefore remains 16 packets/24 KiB. A larger logical group is rejected until a
controlled physical provider proves a complete-path win.

Schema-12 already exposes ACK/recovery writes, route-write wait, Pack handoff,
carrier/queue drops and pressure. The profile separated timer/GC work without a
new ACK-hot-path atomic, so no additional sampled counter was retained in this
pass.

### Fast.com regression root cause and retained correction

The active performance learner was not merely ordering the fresh TLS field. It
could reduce a four-to-six-exit quality race to one provider. That is unsafe in
a rough public pool: a posterior is evidence about prior flows, not proof that
the next origin socket will remain healthy. The regression was visible without
carrier or Pack loss; candidate removal, timeout recovery and the loss of
parallel tail insurance were the limiting boundary.

Separately, completed evidence is now published only if the exact route still
has an active transport, has no warning/quarantine verdict, is not canceled,
and remains present in a live window. Channel state is rechecked after the
window scan. This cold retirement-path guard adds no ACK work and prevents a
retired route's teardown interval from poisoning a later decision. It was
correct but did not by itself restore speed: the guarded hard-filter arm still
accepted 67 healthy samples and removed 1,706 candidates.

The measured exploration sweep was:

| Fresh TCP/443 policy | fast.com displays | Median | Runtime result | Placement/recovery evidence | Decision |
| --- | --- | ---: | --- | --- | --- |
| Installed hard-filter control | 110 / 55 / 66 Mbit/s | 66 Mbit/s | 21.91-MiB peak | 55 samples; 502 candidates removed | Regression control; variable route still reached 40, but filtering was already heavy |
| Learner disabled / full field | 130 / 110 / 140 Mbit/s | 130 Mbit/s | **30.43-MiB peak** | zero learner work; 7,629 timeout resends; 10--13 failed page requests/run | Reject: fast but outside 24/28 MiB and less reliable |
| Active/healthy guard, hard filter | 0.91 / 6.2 / 4.9 Mbit/s | 4.9 Mbit/s | 24.23-MiB peak | 67 samples; 1,706 removed; 991 pressure drops; zero carrier/Pack drops | Reject hard collapse; keep lifecycle guard independently |
| Minimum two candidates | 15 / 0.52 / 89 Mbit/s | 15 Mbit/s | 20.47-MiB peak | 60 samples; 1,249 removed; zero carrier/Pack drops | Reject: memory-safe and better than the adjacent hard arm, but bad tail |
| Minimum three candidates | 0.68 / 0.89 / 0.95 Mbit/s | 0.89 Mbit/s | 18.83-MiB peak | 57 samples; 282 removed; zero pressure/carrier/Pack drops; two page-target transfers independently measured about 29 Mbit/s | Reject: canonical displayed result did not improve |
| **Minimum four candidates** | **47 / 100 / 110**, then **68 / 85 / 130 Mbit/s** | **92.5 Mbit/s across six** | first cohort 24.30-MiB peak; hot repeat 22.75-MiB peak | more than 1.1 GB exact H1 ingress; 2,243/2,243 Pack waits at the observed maximum; zero carrier/Pack drops | **Keep**: all six exceed 40 Mbit/s; warm 24.30-MiB crest remains disclosed below |

The four-route floor keeps the highest posterior first and retains three escape
routes; fields wider than four can still discard clearly weaker candidates.
It sorts the placement-local slice using a fixed stack array. Current M4 Pro
medians were 132.4 ns for a cold race, 309.1 ns with completed evidence, and
279.5 ns with live evidence, all 0 B/op and zero allocations. The deterministic
six-candidate regression test verifies that a strong route plus three stable
alternatives survive while the two weakest routes are removed.

The first four-route burst had one 24.30-MiB sample, 304 KiB above the nominal
24-MiB line, then returned to about 20.3 MiB. After a quiet interval, the next
three fast.com runs peaked at 22.75 MiB rather than growing. Five subsequent
cache-disabled Wikipedia runs peaked at 20.92 MiB and measured 478.3-ms median
load, 195.2-ms median document TTFB, and zero failed requests. Thus the steady
24-MiB goal and 40-Mbit/s target pass on this Android surrogate, but this is not
an absolute all-samples <=24-MiB claim. Do not spend throughput to erase one
non-repeating 304-KiB GC-phase crest without a paired improvement; physical iOS
`phys_footprint`/jetsam remains the release gate.

The complete Connect/SDK short suites, both vets, focused race-detector tests,
iOS/Linux cross-builds, and final Android AAR/app/test/unit build passed. Every
temporary production client, on-device credential file and private session
artifact was removed after its arm.

## Experiment queue

### Rejected packet-allocation candidate (2026-09-20)

The `appendIpPacketGroupBounded` ownership deferral and bounded
`WindowStats` source-count scratch reuse were tested together. They improved
their deterministic microbenchmarks (multi-packet grouping avoided discarded
canonical-path allocations; repeated 64/256-destination stats reads avoided
temporary count slices), and focused normal/race tests passed. The required
fresh physical H1 iOS-profile arm did not validate the direction: the full
20/32-MiB profile and 315-second quiet window peaked at 26,517,536 bytes,
above the absolute 24-MiB barrier and 1.3% higher than the 26,177,568-byte
owner-fix replay. Its Fast.com displays were 0.79/28/31 Mbps, not a speed win.

Do not retain or retune this pair on the basis of allocation microbenchmarks.
The result reinforces that the active excess is retained runtime/span/stack or
traffic-lifecycle memory outside these short-lived packet/stat scratch objects.
Future candidates must first attribute the physical peak by Go runtime class
and traffic phase, then prove an improvement against the same guarded
collector/workload/quiet protocol.

### Idle API ownership; rejected stack experiment (2026-09-20)

The two valid H1 artifacts above peaked at 26,177,568 and 26,517,536 bytes.
At those samples, packet/TUN roots and tracked Transfer queues were empty;
free packet/large-object pools held only 442,368 and 385,024 bytes. The newer
peak contained 10,513,960 bytes of heap objects, 5,648,856 bytes of occupied-span
slack, 770,048 free heap bytes, 3,178,496 stack bytes, and 6,406,176 other runtime
bytes. Lower allocation rate alone does not clear this retained-memory floor.

`ClientStrategy` did not subscribe to memory shedding, leaving idle API HTTP,
alt-QUIC and internal-DoH socket graphs outside the existing SDK reclaim pass.
The scoped fix registers/unregisters with owner lifetime and closes only idle
connections. It preserves active responses, the reusable HTTP transport and
path-local TLS tickets; H1 data carriers, budgets and reclaim timing are unchanged.

Deterministic loopback tests cover global dispatch, idle/native HTTP and QUIC,
active-response protection, registration/cancellation races, and TLS resumption.
A four-idle-QUIC-owner sample released all 6,815,744 **reserved** transport-budget
bytes, 48 goroutines and 188,400 heap bytes, versus no closed carriers and
23,728 heap bytes reclaimed on baseline. Both loopback endpoints are in-process:
these are not predictions of device heap savings, and reserved bytes are not
physical memory. Six paired/reverse-order benchmark repetitions kept allocations
unchanged; warm API latency changed -0.22%, resumed re-dial +0.50%, and H1
saturated/ACK-sized/sparse latency medians -0.60% to +0.82%. Focused normal/race
tests and vet pass. Re-dial still has a cost after genuine pressure; do not turn
this cleanup into a per-request or periodic forced pool reset.

Separately, removing blank admission-owner stack temporaries reduced the local
ARM64 `SendSequence.Run` frame from 4,640 to 4,048 bytes; extracting its cold
timeout log reduced it to 3,872. Neither changed the fresh-process retained-stack
measurement: 64 real idle sequence/ACK-worker pairs used 1,277,952 incremental
stack bytes in every arm. Both production refactors were rejected.

The API cleanup is a locally verified resource-lifecycle fix, **not a physical
24-MiB pass**. No physical arm or baseline promotion was performed for it.
The next qualifying arm must retain full active/quiet coverage, the absolute
24-MiB ceiling and unchanged Fast.com/page performance gates. Aggregate evidence
and exact commands are retained privately in the retained-owner analysis report.

Run one change at a time where practical, then combine only independently
useful changes. The search covers the whole H1 path rather than assuming that
every slow result is a queue-size problem:

1. **Deployable provider A/B.** Put eight negotiated data lanes and the logical
   provider-return group/direct-ACK path on a controlled current
   server/provider, alternate old/new binaries, and run H1
   download in the full-TUN PERFVAR matrix plus the physical Direct/H1 bracket.
   Pin provider/exit selection; do not compare two public-provider races. The
   local Redis/DB fixture must be restored first. Record goodput separately for
   client-to-server, server-to-provider, and provider-to-origin, plus Transfer
   messages, frames/message, provider socket-read batches/windows, route-write
   wait, timeout resends, per-lane occupancy, exact payload, CPU, and allocated
   bytes. Pin explicit H1 for this experiment; production Auto remains unchanged
   because its Client settings can outlive an H1-to-H3 carrier transition.
2. **Retained mobile profile.** Keep H1 receive 64 / 128 KiB and lossless exact
   reliable-lane carrier/Pack backpressure to cancellation; keep every other
   Transfer/control count at 16, ACK handoff at
   1 ms, the packet-root gate at 1 MiB base / 2 MiB exact H1 ACK maximum, and
   the exact provider-aware shared receive-allocation budget (1.68 MiB on and
   2 MiB off at the 24-MiB target). Leave adaptive H1 depth disabled in the
   mobile policy; its full-depth physical arm did not improve throughput.
   Repeat provider off/on/off under traffic so the provider share transition
   cannot retain an old H1 burst.
3. **Provider logical-group bound (closed locally).** Keep 16/24 KiB. Although
   32/48 KiB won the admission microbenchmark by 11.9%, it lost 2.6% TCP on the
   complete provider path and crossed the synthetic memory ceiling in two of
   five runs. Reopen only for an exact controlled-provider physical A/B.
4. **ACK-write contention telemetry.** Measure direct Transfer-ACK route-write
   wait/failure by carrier before building a priority lane. Keep instrumentation
   sampled or test-only so an atomic on every ACK does not become the result.
5. **Negotiated H1 envelope.** Prototype a server-advertised H1 message cap and
   compare 4, 8, 16, and 32 KiB. Roll the receiver capability first, preserve
   old-server downgrade, and charge the larger in-flight message to the same
   24-MiB byte budgets.
6. **Exit topology and routing.** Inspect per-exit throughput, affinity,
   provider selection, re-races, and the two-second retry tail. The direct
   85.03-Mbit/s bracket plus 1.65-Mbit/s tunnel result makes provider/exit
   isolation higher value than another client queue increase.
7. **TCP/TUN flow control.** Attribute TUN read/write drops, local TCP ACK
   progress, NAT queue occupancy, receive reorder gaps, resend counts, and
   callback loss. The 4,096-root and Transfer-NoAck results have rejected both
   blunt root enlargement and ACK-of-ACK removal as the primary limiter.
8. **CPU, scheduler, and GC.** Collect Android CPU frequency/load, process CPU,
   goroutine/block/mutex profiles, GC count/pause, allocation rate, heap live,
   retained spans, and pool hit/miss/reclaim data. Repeat hot and thermally
   settled samples so GC improvement is not confused with radio variance.
9. **Server relay and proxy.** Keep the comprehensive server benchmark cohort
   beside each shared Connect change. Re-run full PERFVAR once fixtures are
   available; benchmark-only success does not replace exact payload delivery.
10. **Allocation re-profile.** Confirm that `IoLoop.run` and ACK map clones no
    longer dominate allocation-space. Profiling runs are diagnostic and are
    excluded from performance/memory acceptance comparisons.
11. **Sustained-burst pool high-water.** Retain the final exact-allocation arm
    as the regression control: every iOS-profile runtime sample <=24 MiB,
    five-minute quiet coverage with maximum/p50/p95 <=24 MiB, full payload
    completion, and packet roots <=2 MiB. Re-open
    reclaim tuning only if a controlled provider deployment recreates the old
    29.48-MiB crest.

## Notes and pitfalls

### 2026-09-20: on-demand owner census for the attested H1 breach

The fully attested native H1 arm still failed the absolute 24-MiB barrier:
29,900,832 bytes (28.52 MiB), while Fast.com completed at 65/35/65 Mbit/s.
The peak sample was 274,324 ms into `connect-h1`, with 11,594,104 heap-object
bytes, 6,805,128 bytes of in-use span slack, 1,851,392 free-but-unreleased heap
bytes, 3,244,032 stack bytes, 243 goroutines, and 201 client flows. Returned
pool retention was only 557,056 bytes; live packet roots and tracked transfer
queues were zero at that sample. Admission claims were 2,097,152 bytes across
eight slots, **not** an estimate of the underlying heap. This is not the older
unattested arm's allocation-class breakdown, even though the total peak
coincided. No cap, queue budget, reclaimer threshold, or baseline is changed.

New opt-in diagnostics read existing owners: active Transfer workers including
canceled/index-detached workers; channel capacities and pacing services;
flow/affinity/cache counts; IpAssoc raw scratch; API pools/connections; DNS
caches; bounded Transfer free lists; and claims by transport class. The SDK's
non-iOS `DeviceLocal.WriteMemoryOwnerCensus` exclusively creates an aggregate
mode-0600 JSON file with before/after runtime values and allocator size classes.
There is no packet hook, registry, background sampler, forced GC, or cache
release. Known struct/slice bytes are intentionally incomplete ownership
accounting, not a heap total or a span-slack attribution. Native HTTP/TLS/QUIC
object graphs still require paired private profiles.
The census scopes current indexed device owners; already-unlinked client
generations and other NetworkSpaces are not discoverable without those
profiles. Zero current-owner counts therefore do not prove a process-wide
absence of leaks.

Deterministic tests cover workers remaining after index removal and reaching
zero only after joined teardown; receive workers; canceled flow reaping while
preserving live TCP; scratch capacity and pressure release; API active/idle
and closed-but-rooted discrimination; actual automatic closed-API retirement;
DNS shared-cache deduplication and release; transport claim classes; bounded
topology truncation; privacy/exclusive files; and concurrent reads/teardown.
The actual API close callback released its pool entry: this is negative leak
evidence, not justification for additional connection churn.

Local Apple M4 Pro / Go 1.26.7 measurements: a 400-flow census took a median
2.62 microseconds (10 runs); an empty-device primitive census 52–56 ns; a
runtime-plus-owner report 43.9–45.9 microseconds (five runs). All three measured
0 B/op and 0 allocs/op after existing-owner initialization. File/JSON writing
is excluded and deliberately diagnostic-only. Focused normal repetitions,
race tests, vet, Android helper unit tests, and acceptance Kotlin compilation
passed. These are overhead/lifecycle checks, **not** a physical 24-MiB pass.

Next diagnostic protocol: explicitly rebuild and attest SDK/AAR/native/APKs
with iOS profile and startup heap sampling 65536; require the owner-census
capability preflight, then pair pre-GC owner census, heap profile, post-GC
census, and last-of-all private goroutine stacks at idle/post-traffic/quiet
boundaries. See `android/app/scripts/PHYSICAL_LOWBAR.md` for exact command and
privacy rules. Raw profiles/stacks never leave private artifacts; report
aggregate allocation/function/state owners only. Keep this diagnostic arm
separate from unprofiled performance acceptance. No additional physical arm
has been run for the census, and no concrete remaining allocator owner has
yet been proven causal.

### Owner-census physical diagnostic — 2026-09-20

A fresh, private Android diagnostic arm used the pinned Pixel 8 Pro with the
iOS-audit profile (20-MiB admission / 32-MiB runtime limit), 64-KiB profile
sampling, matching freshly built SDK AAR / app APK arm64 native-library digests,
and a successful schema-1 owner-census preflight. The diagnostic artifacts,
including raw pprof and goroutine stacks, are private mode-0600 evidence. This
was intentionally a profiled attribution arm, not a qualifying 24-MiB or
throughput run.

The native runtime was 17.36 MB at no-traffic preflight, 20.90 MB shortly
after H1 connect, 30.91 MB after the joined real-site workload, 29.33 MB after
the explicitly forced heap-profile collection, and 27.99 MB after 180 seconds
of still-connected quiet. Thus it remains above the absolute 24-MiB ceiling
even after natural release; no baseline or budget changes are justified.

The census materially narrows the hypothesis. Live client flows rose from 6
after connect to 225 after traffic, then fell to 3 at the quiet boundary;
reverse-DNS entries fell from 300 to 11. Transfer queues were empty at every
post-traffic census, and pool known-retained structs fell from 53.6 KiB to
15.4 KiB. The quiet sample still had 8.96 MB heap objects, 6.29 MB in-use heap
slack, and 3.15 MB stacks. Consequently, neither pooled packets nor unreaped
flow/reverse-DNS maps explain the remaining steady runtime by themselves.

The private post-traffic heap profile had 9.43 MB sampled live heap. Its
largest aggregate allocations were buffered writers (0.85 MB), source-event
buckets (0.67 MB), message-pool warm storage (0.32 MB), and message-pool
construction (0.23 MB); these are incomplete sampled heap attribution, not
permission to tune a pool. The large residual is allocator span slack plus
stack/runtime overhead. A privacy-safe aggregate of the private stack capture
counted 230 goroutines (107 plain `select`, 16 poll waits, and most remaining
tops in Connect); it did not reveal one unexpectedly rooted post-traffic
worker class. The next accepted research step is therefore a stack-size-class
and allocation-lifetime comparison around the 180-second flow-release boundary,
then a deterministic release test for any concrete owner that remains high.
Do not trim, close active H1 sessions, or lower packet/sequence budgets until
that causal release test demonstrates a steady-memory reduction without page
or Fast.com regression.

The same diagnostic workload recorded Fast.com 16/32/54 Mbps and observed
both requested media probes without clock progress; both media child exits
were retained. Because profiling perturbs the run and the host collector's
external VPN eligibility was invalid for this arm, these are diagnostic
observations only—not performance, provider, or video-regression verdicts.

### H1 destination retirement: missing carrier join — 2026-09-20

A deterministic actual-H1 test found a narrower lifecycle defect, not yet an
explanation for the still-connected memory peak. The SDK already joins its
retired multi-client and owned API generator. However, generator retirement
only canceled the external `PlatformTransport`; `Client.CloseAndWait` does
not own that carrier. Holding the H1 teardown after route removal therefore
allowed generator `CloseAndWait` to return success while carrier workers
were still alive. `TestApiMultiClientGeneratorJoinsActualH1Transport` fails on
the old code and passes after joining the carrier in the existing asynchronous
retirement worker, outside generator locks. Caller cancellation remains
bounded; ownership continues until actual teardown, and a later join succeeds.
Identity removal remains after joined carrier/client/OOB cleanup. No active
H1 path, queue, timeout, admission budget, or memory ceiling changed.

The SDK `TestDeviceLocalH1OwnerLifecycle` uses local auth/discovery, a real H1
websocket and API generator, eight echoed packet flows, and production
`DeviceLocal.CloseAndWait`. In 20 healthy repetitions per arm, cold traffic
median was 205.139 → 205.142 ms and close median 0.575 → 0.576 ms (p95
0.872 → 0.857 ms). Both arms cleared current flows/clients and generated
send/receive/pacing owners; there was no observed meaningful healthy-close
regression. Normal/race repetitions and Connect/SDK vet passed. These local
latencies are not Fast.com or physical-device performance measurements.

Immediate process-wide heap snapshots did not demonstrate heap reclamation:
the fixture deliberately retains closed clients for owner assertions and
keeps its server/provider alive, and identity cleanup itself allocates.
Likewise, the H1 admission claim can reach zero before the carrier's `Done`
edge; it is not proof that socket workers or buffers have finished. The fix
repairs false teardown completion, but does not establish a leak among the
five expected live H1 exits or a physical 24-MiB pass. Keep connected-owner
allocation/stack attribution and transport-budget release ordering as
separate research questions. Local evidence is retained in the private
`urnetwork-h1-carrier-join.CFVklV` artifact; no device arm or baseline change
was made for this fix.

### Block-action history: release verified, policy change rejected — 2026-09-20

The owner-diagnostic post-traffic heap profile attributed approximately
0.94 MiB to `blockActionCollector.flush` → SDK row conversion, principally
IP strings and exported-list storage. That call stack identifies allocation
origin, not a collector-owned leak. The physical SDK history fell from
1024 rows / 1183 slots to 35 / 60; its later sample was about 363 seconds after
the pre-GC post-traffic sample, beyond the existing 300-second history window.
Runtime still measured 27,990,296 bytes, 2,824,472 above the absolute ceiling.

New deterministic tests keep production policy unchanged: Connect's collector
flush releases both epoch aggregates and unretained emitted actions; SDK
expiry releases every old row, shrinks backing storage, preserves surviving
decisions/counts, and releases all slots when empty. A consumer-held snapshot
correctly retains its rows only until that snapshot is released. Tests use
weak references and virtual time, not sleeps or shortened production windows.

Five fresh-process release trials (1024 rows, 16 IPs and one synthetic host
per row) measured median full-history live heap of 856,160 bytes above empty.
Expiring to 35 rows released 826,760 bytes, leaving 29,400 bytes of live heap
and 90,112 in-use heap bytes above empty; expiring all left only 320 live-heap
bytes above the initial diagnostic snapshot. Process runtime stayed higher
and variable despite released owners, demonstrating why runtime alone is not
proof of retained action rows. Forced GC/scavenging here is test-only, not a
proposed production remedy or a physical-memory pass.

A 64-row history update with 16 IPs/row measured a median 43.079 microseconds,
94,264 B/op and 2,244 allocations/op (five benchmarks, Apple M4 Pro / Go
1.26.7). Conversion/trim churn is real, but no experiment yet ties it to the
remaining multi-megabyte quiet excess. No cap/window/security or production
code change is retained. Connect normal x10/race x3, SDK release normal
x5/race x5, and both vets passed. Evidence:
`urnetwork-block-action-retention.e0MzEc/REPORT.md`.

Remaining question: cross-owner span fragmentation or a separately held UI
snapshot requires a paired quiet heap profile and controlled allocation-layout
experiment. These isolated release tests do not rule those out. The Android
projection caches Kotlin value rows rather than Go BlockAction wrappers;
that read-only source observation is not a JNI-lifetime measurement. Lowering
history retention without that causal evidence would reduce observability
without establishing a 24-MiB fix.

### Go soft limit 24 versus 32 MiB: blanket change rejected — 2026-09-20

Ten fresh-process, alternating local H1 arms compared **only** the runtime
soft limit: five at 32 MiB and five at 24 MiB. Device admission stayed 20 MiB,
Connect process sizing stayed 32 MiB, GOGC stayed 25, and mobile pool/queue
policy stayed identical. An explicit test-only platform overlay enabled the
mobile branches on the darwin host. A real DeviceLocal/API generator and H1
WebSocket carried 16,384 echoed 1200-byte payloads per arm through the Auto
four-quality-plus-one-speed topology. All ten arms retained five clients and
72 indexed flows and passed correctness. No public endpoint/device was used.

| Median measurement | 32-MiB soft limit | 24-MiB soft limit |
| --- | ---: | ---: |
| Sampled traffic runtime peak | 25,976,598 B | 23,609,110 B |
| Natural connected quiet runtime | 24,510,230 B | 23,314,198 B |
| Quiet heap / in-use span slack | 7,288,664 / 4,480,836 B | 6,493,584 / 4,280,624 B |
| Quiet stack bytes | 3,080,192 B | 3,244,032 B |
| Connected runtime after diagnostic GC | 22,454,038 B | 21,794,598 B |
| Local payload echo rate | 280.20 Mbit/s | 180.67 Mbit/s |
| Packet RTT p50 / p95 | 1.489 / 6.426 ms | 2.228 / 12.629 ms |
| Traffic GC cycles / summed pauses | 177 / 26.183 ms | 357 / 39.410 ms |
| Traffic GC CPU / mark-assist CPU | 0.880 / 0.113 s | 1.735 / 0.433 s |

The quiet-runtime median improved by 1,196,032 bytes (1.14 MiB), but natural
quiet ranges overlapped: 23,306,006–25,562,902 B at 32 versus
23,002,902–23,469,846 B at 24. Throughput regressed 35.5%; every 24-MiB sample
(174.67–225.98 Mbit/s) was below every 32-MiB sample (251.07–347.78 Mbit/s).
The lower limit roughly doubled collections and GC CPU and raised mark-assist
CPU 3.84-fold for the same useful traffic. It primarily reduces allocation
float/free heap, with only a small median slack reduction—not a demonstrated
resolution of the physical arm's 6.29-MB span slack.

**Rejected as a blanket production change** under the requirement to preserve
H1 performance. Keep the existing 20-MiB target / 32-MiB soft-limit policy;
this does not accept its known physical breach of the absolute 24-MiB barrier.
Only reusable tests and research evidence are retained. Exact per-arm JSON,
statuses, commands and ranges: `urnetwork-h1-softlimit.n4sTod/REPORT.md` and
`results.jsonl`. SDK lifecycle normal x5/race x3, the opt-in experiment under
race, vet and diff checks passed.

Scope: 12 seconds of natural quiet (last six samples summarized), then three
explicit diagnostic GCs and one diagnostic scavenge. Flows did not expire;
this is not the physical five-minute quiet gate. Loopback providers/server
share the measured process; the carrier is local `ws`, not upstream TLS, and
echoed UDP packets do not model browser TCP congestion. These rates are not
Fast.com speeds or a prediction of the phone's regression. No physical iOS,
Android, absolute-peak, or release-readiness claim follows from this test.

Follow-up fixture caveat (same date): the local provider relay used an untyped
gateway with unknown receive reliability. A deterministic queue-saturation
test now proves that this permits a Pack handoff drop, whereas an explicitly
reliable H1 relay backpressures and preserves the packet. The ten historical
soft-limit arms completed, but did not collect this provider-side drop guard.
Keep those numbers as historical surrogate observations, not a complete
bidirectional production-H1 validation or proof that 24 MiB cannot work with
other allocation changes. No soft-limit policy change was made.

### Live H1 topology 4+1 / 3+1 / 2+1: not retained — 2026-09-20

The opt-in SDK `TestDeviceLocalH1TopologyExperiment` varies only the number
of quality slots through test overlays. Speed stays one, process sizing and
soft limit stay 32 MiB, device target stays 20 MiB, GOGC stays 25, and all
mobile pools/queues and reliability timeouts remain unchanged. The real
DeviceLocal/API generator/H1/Transfer path carries equal useful echo traffic
on quality-first port 443 and speed-first port 123. Five local provider
fixtures exist in every arm. Before traffic, exact owner count **and** Added
provider count must match the requested topology.

The first partial cohort used unknown provider carrier metadata and is
`INVALID_FIXTURE`, preserved separately as `urnetwork-h1-topology.LfDvS7`.
`TestH1OwnerFixtureReliableProviderHandoff` reproduces its saturation failure:
the old relay drops one of three packets; the corrected H1/reliable relay
waits and delivers all three in order. This is a test-only relay repair,
not a production transport change. Normal x5 and race x3 passed.

The corrected balanced cohort completed all 15 process-level records. Every
row had zero provider/client Pack handoff drops, but traffic correctness was
only **3/5, 2/5, 2/5** respectively: baseline and candidates had intermittent
mid-flow echo timeouts or packet-admission refusals. Do not discard failed
rows, rank survivor-only medians as a clean A/B, or update a baseline.

| Observed measurement | 4 quality + 1 speed | 3 quality + 1 speed | 2 quality + 1 speed |
| --- | ---: | ---: | ---: |
| Connected goroutines, all five arms, median | 265 | 236 | 210 |
| Connected runtime, all five arms, median | 18,120,470 B | 17,497,878 B | 17,612,566 B |
| Completed traffic/recovery arms | 3/5 | 2/5 | 2/5 |
| Quiet runtime, completed arms only | 29,069,094 B | 27,316,006 B | 26,562,326 B |
| Quiet allocated heap / span slack, completed only | 11,294,928 / 3,753,776 B | 9,785,096 / 4,219,128 B | 7,928,896 / 4,924,352 B |
| Quiet stack bytes, completed only | 3,604,480 B | 3,088,384 B | 3,096,576 B |
| Quality / speed echo Mbit/s, completed only | 642.80 / 412.13 | 425.34 / 348.90 | 598.46 / 408.24 |
| Quality packet RTT p50 / p95, completed only | 0.739 / 1.680 ms | 1.069 / 3.420 ms | 0.832 / 1.831 ms |
| Provider blackhole removal, completed only | 30.18–30.93 s | 30.18–31.43 s | 30.17–30.67 s |

Even the lowest completed natural-quiet runtime was 26,287,894 B, above
25,165,824 B (24 MiB). The completed 2+1 arms show fewer owners and about
0.48 MiB less stack space, but span slack increases; reducing client count
does not itself resolve allocator slack. Survivor-only quality throughput
also falls (~6.9% at 2+1, ~33.8% at 3+1). These are observations, **not**
statistically established deltas given the incomplete traffic cohorts.
No smaller production topology is retained.

One predeclared bounded baseline discriminator subsequently passed with zero
SDK pressure drops, NoAck refused/discard, prewire expiry, receive-queue drops,
and ACK/Pack handoff drops. It did not reproduce the remaining failures;
their source boundary remains open. The opt-in test retains those primitive
counters for the next failing capture. Do not infer a root cause or relax
timeouts from a passing diagnostic. No further diagnostic/device arm ran.

Scope: 64 concurrent flows x 1,024 x 1,200-byte echoes per traffic phase
(157,286,400 useful outbound bytes total), 12 seconds connected quiet, then
test-only GC/scavenge and a single-provider blackhole. All seven completed
arms removed that unavailable provider under unchanged production timers
and delivered 1,024 fresh recovery packets. Co-resident providers, plaintext
local `ws`, synthetic UDP payloads and retained 136 flows differ from phone
TCP/TLS/fast.com and five-minute quiet. These are not website TTFB, physical
speed, iOS footprint, absolute-peak acceptance, or WAN-diversity guarantees.
Raw per-arm GC/pause/traffic/heap data and statuses:
`urnetwork-h1-topology-reliable.RDM5c1/{REPORT.md,results.jsonl}`.

### UDP SCTP ACK scheduling: insufficient for the cold source gate — 2026-09-20

Retained deterministic research only; no production or memory-policy change.
`TestProviderUdpSctpImmediateAckCannotRemoveColdRttRefusal` connects the actual
provider/Transfer/P2P writer to the pinned SCTP with its existing blocking,
reliable-unordered semantics and unchanged 32/four-slot queues. A lossless
virtual-time wire isolates constant RTT from jitter, link serialization,
ICE/DTLS, encryption, and device scheduling. The source offers 468 distinct
1,000-byte UDP payloads at 3.75 Mbit/s after a fully ACKed setup packet.

| Ten normal repetitions | 2-ms RTT control | 120-ms RTT | 120-ms immediate-SACK upper bound |
| --- | ---: | ---: | ---: |
| Source admitted / refused | 468 / 0 | 300 / 168 | 317 / 151 |
| Refused before first possible returned ACK | 0 | 16 | 16 |
| First refusal | none | 87.467 ms | 87.467 ms |
| First returned SACK | 4.133 ms | 122.133 ms | 120.000 ms |
| SACKs, median (range) | 234 (234–234) | 95.5 (93–98) | 199.5 (197–203) |
| DATA retransmits / lost admitted identities | 0 / 0 | 0 / 0 | 0 / 0 |

At both high-RTT first-refusal edges, three DATA chunks have been written,
cwnd is still 4,380 B, SCTP pending/inflight is 4,536 B, and the route is 4/4.
The receive window stays at least 432,434 B in the normal cohort. The limiting
boundary is the **cold congestion window plus the round trip**, before any
ACK policy can return progress. This is separate from the previously fixed
shared UDP writer-lock blockage and from loss/retransmission stalls.

The test-only I-bit arm requests immediate SCTP ACKs on test-owned packet
copies. It admits 17 more later offers but approximately doubles SACK traffic
and cannot pass the frozen all-source-admitted gate. There is no production
Pion setting for this experiment; no dependency was patched. Do not infer a
safe network throughput improvement from a lossless wire without reverse
bandwidth limits. Race-mode later counts vary with ACK coalescing; the
pre-first-ACK failure boundary remains invariant.

Adjacent review ruled out the userspace UDP batching queue for legacy SCTP:
DTLS/SCTP uses `writeDirect`; only SRTP is queued. Earlier ready-drain batching
reduced post-stall writes but not source refusals, and wider frames increase
per-slot roots. Do not disable blocking SCTP, make public UDP callbacks wait,
increase cwnd/queues, or relabel source refusals to force a green result.
No eligible production candidate survived, so no new fixture/device A/B ran;
the current-source PERFVAR UDP failure and absolute 24-MiB gate remain open.

Normal ×10, race ×10 and vet pass. Exact commands, primitive per-repetition
results, preserved initial test-race failures and source boundaries are in
`urnetwork-udp-sctp-ack.5UikD8/REPORT.md`; campaign notes are in
`tests/PERFVAR-MEASUREMENTS.md`. This is not a physical memory measurement,
website speed measurement, or baseline promotion.

### Ordinary UDP coalescing: the pre-write ownership boundary — 2026-09-20

The follow-up uses actual SCTP rather than the earlier fixed writer-barrier
model. `TestProviderUdpSctpReadyDrainKeepsColdAdmissionBoundary` exercises the
existing larger H1 envelope through a **test-only** carrier adapter, while
keeping the physical legacy P2P writer, 32/four-slot queues, zero-wait provider
callbacks, and the same 468-packet offer. This is not a production P2P policy.

| Ten normal repetitions, median (range) | Production envelope | Wider ready-drain upper bound |
| --- | ---: | ---: |
| Admitted / refused | 300 / 168 (298–300 / 168–170) | 310 / 158 |
| Admitted / refused before the first possible ACK | 41 / 16 | 41 / 16 |
| Physical writes | 300 (298–300) | 234 (231–234) |
| Sampled outstanding pooled-root peak | 77,824 B | 88,064 B |
| DATA retransmits / lost admitted identities | 0 / 0 | 0 / 0 |

First refusal remains 87.467 ms, before the first returned SACK at 122.133 ms.
Larger ready-drain calls therefore improve later service only, leaving the
cold admission failure intact. The pooled-root peak rises **13.2%** despite
unchanged queue slot counts; this does not count Go metadata or SCTP internals
and must not be reported as the complete runtime footprint. A fresh six-run
encoder benchmark improves median time from 1.5085 to 0.8861 microseconds per
30 already-ready packets, zero allocations/op in both arms, but its wire root
grows from 2 to 4 KiB. CPU savings alone do not qualify this change.

Source review places the missing operation **before** the sequence parks in
`route.Write`: a ready-only drain cannot consume later source arrivals while
that owner is blocked. A dedicated compact admission batch was considered
but **not implemented or qualified**. It needs a new bounded owner covering
the compact arena, every original callback/lifecycle record, and any overlap
with frozen/serialized copies. Existing logical groups do not provide that
ownership transfer for independently admitted callbacks; simply merging or
releasing their slots would hide a payload-capacity increase. Pooled arena
subviews also cannot be treated as independently returnable packet roots.
No safe minimal candidate with a demonstrated memory-neutral cap emerged;
this is a scoped rejection, not proof that all compact-queue designs fail.

Normal and race tests ×10, vet, and the encoder benchmark pass. The earlier
synthetic test's assertion of equal **post-release** sampled peaks was too
strong: a preserved race-mode repetition observes 81,920 versus 77,824 B.
It now asserts equal **pre-release** ownership only and records the later
peak as a measurement. No acceptance/memory threshold was relaxed.

Retained changes are tests and research notes only. No new fixture/device A/B
ran because no eligible production candidate reached that stage. The frozen
PERFVAR source gate, absolute 24-MiB ceiling, and baselines remain unchanged.
Current helper hashes, all repetitions, failed draft assertion, and commands:
`urnetwork-udp-sctp-batch.0C7JHL/REPORT.md` (private 0700 directory).

### Legacy SCTP compact backlog and blocked-flight service research — 2026-09-20

The 256-KiB compact legacy send queue added in `018e56d9` is **not yet
qualified by physical/profile or unchanged PERFVAR replay**. It reserves its
fixed owner before creation, charges every retained root against the peer's
shared WebRTC budget, keeps small controls/probes synchronous, and joins its
physical writer before returning the fixed reservation. H1 and the negotiated
native fast lane do not use this backlog. The separate absolute **24-MiB
iOS-profile** gate remains mandatory; none of the following pool measurements
is whole-runtime or iOS physical-footprint evidence.

Repetition exposed two deterministic adjacent defects. A full queue previously
flushed the entire backlog instead of admitting again after one root was
released; `TestP2pLegacySendQueueRefillsAfterOneRelease` fails before the repair
and passes after it without increasing any cap. Async ownership also consumed
an extra readiness probe behind a held small control write. Queue creation now
waits for a bulk message greater than 256 bytes, and small controls remain
synchronous even after bulk use. The existing probe-pressure assertions and
`TestP2pLegacySmallControlKeepsSynchronousOwner` preserve the original bounds.
Broader P2P/WebRTC race validation passes (11.723 s); H1/legacy lifecycle
isolation race ×3 passes (2.281 s).

The unchanged 468-packet/3.75-Mbit/s, 120-ms-RTT fixture revealed a second
limiter after the refill fix. Normal compact arms admit all 468 packets, but
race instrumentation admits only 411–468 in a ten-run untraced cohort. The
pinned SCTP checks whether its *internal pending queue is nonempty* when a SACK
advances cumulative progress. With one-message blocking writes, that queue can
be momentarily empty despite a saturated application backlog. A test-only
logger observes 42–86 skipped-growth ACKs under race versus 0–5 normally.
Yielding the writer does not fix the gate (425–468 admissions under race).

This is not merely a race-detector artifact: a deterministic **50-µs per-write
service cost**, more than 42 times faster than the source's 2.133-ms interval,
reproduces 369–383 admissions and 126–131 skipped-growth ACKs in three normal
runs. Later instrumentation controls measure 376–418 admissions. Offered
traffic and terminal refusals are unchanged; every admitted identity arrives
once, with zero DATA retransmits. Do not infer statistical improvement by
comparing these separate instrumented cohorts.

| Candidate, 120-ms RTT + 50-µs service | Admission / 468, three normal repetitions | Decision |
| --- | ---: | --- |
| Fixed SCTP windows 16 / 32 / 64 / 96 KiB | 307 / 344 / 389 / 363 | Reject: up-front service reservations steal cold backlog capacity. |
| Dynamically charged windows 64 / 96 / 128 KiB | 338 / 377 / 368 | Reject: lower retained ownership, but still fails full offer. |
| Instantaneous pre-SACK full-flight proxy | 390–418 | Reject: ACKs arriving after a service gap still lose the growth signal. |
| Flight-scoped record of an actual cwnd-blocked send | 468 in every run | Promising isolated dependency candidate; loss/protocol and integration validation pending. |

Both service-window sweeps use the *same* 256-KiB free shared budget. Fixed
windows reserve twice their payload cap plus an 8-KiB owner; the dynamic arm
charges twice each live single-fragment payload plus that owner and releases
on acknowledgement. This is conservative test accounting, not a production
fragmented-message ownership proof. Dynamic shared claims stay at or below
786,344 B versus 786,432 B total, and return exactly to the preexisting 512-KiB
owner. No nonblocking SCTP policy or service-window increase was retained.

The flight-scoped experiment records the last TSN actually sent when another
pending DATA chunk is blocked by cwnd, retains eligibility only through that
flight's cumulative acknowledgement, and clears it on a congestion decrease.
It does not set a cwnd floor or alter ACK generation, reliability or loss
timers. In the initial isolated fork, normal ×3 and race ×10 admit **468/468**
at all tested 0 / 50 / 200 / 500 / 1,000-µs service costs, with no retransmits.
At 50 µs under race, pooled roots peak at 245,760–266,240 B and concurrent
pool + SCTP payload at 313,038–320,350 B, versus later blocking controls'
323,584-B pool and 365,542–382,552-B combined peaks. SCTP chunk metadata,
runtime spans, and device memory are not included in that combined number.
The protocol tests cover app-limited/expired-flight no-growth,
fast-recovery suppression, receiver-window versus cwnd marking, TSN wrap,
and clearing eligibility on congestion reduction. Final tracked tests pass
race ×20 (1.251 s). Full upstream SCTP short tests pass normal (38.000 s)
and race (37.824 s). Initial draft protocol fixtures omitted timer teardown;
their preserved failures were repaired with owned pipe/timer cleanup before
these full-suite runs, not waived as harmless leaks.

Loss controls at 2 / 50 / 120 ms, normal and race ×3, preserve exact identity
and byte-budget accounting. At 120 ms with 1% deterministic loss, the candidate
admits all 468 offers and retransmits exactly the four dropped DATA packets.
Dropping the entire initial three-packet flight still gives 248 admitted /
220 refused in both arms, then exactly three retransmits and complete delivery
of admitted identities. That no-ACK interval remains a bounded source refusal,
not a hidden success. The loss regression now asserts exactly one retransmit
per deliberately dropped single-DATA packet; no spurious retransmits are
allowed on the lossless reverse path.

The measured candidate is now tracked as `sctp`, pinned to the
complete upstream v1.11.1 source with only the flight-state repair and its
tests. Explicit replacements in Connect, SDK main/build/cgo/js, server,
proxy, operator-proxy, and sn prevent Go's non-inherited-replacement rule from
silently selecting different implementations. All nine main modules resolve
to this same source. The unchanged upstream cache is not patched. A release
outside this workspace needs an upstream/maintained-fork pin or an equivalent
explicit consumer replacement; installing a new receiver alone does not fix
an older remote sender's congestion controller.

`TestProviderUdpSctpCompactQueueServiceCostPreservesFixedOffer` is red against
unmodified v1.11.1: 383/468 at 50 µs, 422/468 at 1 ms. Integrated focused
normal ×10 PASS (4.515 s), race ×10 PASS (68.050 s); final strict loss,
H1/legacy lifecycle race ×3 PASS (7.566 s); broad P2P/WebRTC race PASS
(10.079 s). SDK memory/P2P/H1/lifecycle normal ×3 PASS (10.918 s), race ×3
PASS (13.931 s), Connect/SDK vet PASS. Server connect/perfvar, SDK build/cgo,
proxy/operator-proxy, and Connect under sn compile checks pass; SDK JS passes
wasm cross-compilation (not a browser runtime test). The operator-proxy check
also required recording the already-used secp256k1 v4.4.1 checksum/indirect
dependency; no dependency version was upgraded. Physical/unchanged PERFVAR
replay may now start against this exact tree, but it has not run here and
neither a baseline nor the iOS-profile 24-MiB gate is promoted.

Reproduce controls with `TestProviderUdpSctpServiceSchedulingExperiment`,
`TestProviderUdpSctpServiceDelayExperiment`,
`TestProviderUdpSctpBoundedServiceWindowExperiment`, and
`TestProviderUdpSctpDynamicServiceWindowExperiment`, using `GOMAXPROCS=4`,
`-p=1`, normal and `-race` repetitions. Keep the existing
`TestProviderUdpSctpCompactQueueAdmitsColdHighRttFixedOffer` assertion unchanged.
Loss/RTT comparisons use `TestProviderUdpSctpGrowthLossSafetyExperiment` and
must report physical retransmits separately from source refusals. Initial
service/lifecycle artifacts are in `urnetwork-p2p-legacy-compact.BBojQF`;
the isolated dependency, modfile, protocol tests, final integrated report and
before/after outputs are in `urnetwork-p2p-sctp-growth.krgD0Z`. Installed module
cache, physical devices, memory policy and baselines are unchanged.

### Unlinked H1 migration owners — 2026-09-20

The current-owner census cannot rule out a retiring, already-unlinked carrier.
An actual H1 regression test now demonstrates a distinct teardown gap after
the earlier current-carrier removal join: `MigrateClientTransport` published
its replacement, called `Close` on the old carrier, and ended its creation
owner without joining old socket/receive cleanup. The pre-fix generator join
returned success with zero indexed clients/workers while an old H1 writer
still held a **16,384-byte** batch buffer. The healthy replacement delivered
traffic during that barrier. This is premature lifecycle completion, not
proof that five healthy connected exits are a leak.

The narrow repair joins every unlinked or discarded migration carrier in the
existing admitted migration worker, outside transport/policy locks. It adds
no worker, queue, budget, timeout, or packet-path operation. Caller deadlines
bound their wait without abandoning retirement ownership. Deterministic
tests cover blocked actual H1 writer and receive cleanup, a healthy
replacement, failed-replacement timeout/cancellation/lost-generation paths,
and SDK `DeviceLocal.CloseAndWait` after an actual H1 migration. At final
completion all captured carrier `Done` channels are closed, the held write
buffer is released, and carrier admission claims are zero.

Five fresh loopback processes per arm each performed eight migrations with
eight distinct payloads; all 80 payloads arrived and all final claims were
zero. Both arms measure full old-carrier completion, so the old early return
is not counted as a speed advantage. Process-median migration times span
425.86–724.34 ms before and 520.47–770.97 ms after (medians 570.20 and
625.63 ms), dominated by unchanged 100–1000 ms startup jitter. The pooled
nested median is 612.18 → 613.67 ms but is **not** 40 independent samples.
These results do not establish statistical speed equivalence or improvement.
Healthy final generator close medians are 1.657 → 1.280 ms. Three fresh SDK
processes per arm give eight-echo traffic medians 204.053 → 203.969 ms and
close medians 0.917 → 1.111 ms, with overlapping close ranges. No hot-path
regression was observed; public Fast.com/TTFB has not been remeasured.

Focused Connect normal ×10, race ×5, opt-in timing under race, SDK migration
normal ×5, SDK lifecycle/migration race ×5, and both package vets pass.
Private evidence, raw per-process timing/resource samples, source hashes,
commands, and invalid draft-fixture attempts are preserved in
`urnetwork-h1-retired-carrier.W4YZai/REPORT.md`.

This validates lifecycle correctness and release of a known buffer, **not** a
multi-MiB steady-memory saving. The immediate fixture heap snapshots retain
test-owned closed clients for assertions and are not post-GC retention
measurements. Existing physical heap evidence has roughly 74 KiB of H1 batch
buffers at each boundary, consistent with healthy active exits. A newly
native-attested, 65,536-byte-rate owner diagnostic is the next attribution
step; it cannot qualify the absolute **24-MiB** gate or promote a baseline.
No memory policy or acceptance threshold changed.

### Fresh iOS-profile burst versus quiet, and an independent history-owner fix — 2026-09-20

The native-attested `urnetwork-cleanarm.kqVGmc` H1 arm still **fails the
absolute 24-MiB gate**: its whole-run peak is **26,353,696 B (25.13 MiB)**,
1,187,872 B over the limit. All three over-limit samples precede quiet.
The valid 339,574-ms quiet boundary contains 22 samples spanning 315,002 ms,
with **zero quiet breaches**, peak **24,723,488 B (23.58 MiB)**, median
23,486,496 B, and final 22,904,864 B (21.84 MiB). The second offline
“quiet-teardown” evaluation uses the same original interval and global peak;
it is not an independent quiet period or evidence of `DeviceLocal.Close`.
The host gate now reports separate whole-run/quiet peaks and breach counts,
with a deterministic regression preserving the global failure even when
every quiet sample passes. No acceptance rule has been relaxed.

At the **349,469-ms** global peak, schema-13 primitives identify 10,566,744 B
of heap objects, 5,252,008 B of in-use span slack, 1,048,576 B of free
unreleased heap, 3,342,336 B of stacks, and 6,137,677 B of other runtime
classes (plus 6,355 B of profiling buckets). There are 257 goroutines,
4 quality + 1 speed clients, and 164 flows, but zero outstanding packet/pool
owners and zero tracked/resend/receive/Pack bytes. The 344,064 B returned
pool is already part of heap, not an additional category. The peak follows
video-site flow fanout and is about 23 seconds after joined browser cleanup.
The next sampled reclaim reports 26,353,696 → 23,470,112 B, a 2,883,584-B
reduction; no later sample breaches the cap. This supports investigating
post-burst object/span lifetime, not an established queued-packet leak.
There is no paired heap profile/census in this rate-zero arm, so its exact
heap owners are still unproven. Five active exits are not post-close residue.

A separate deterministic source audit **does** prove long-session retention
in `IpAssoc.blockWithLock`: slicing off expired blocks left their pointers
in the backing array, keeping matrices reachable outside the visible history.
Clearing only that dropped prefix before advancing the slice releases these
owners without changing retained history, affinity/scoring, memory policy,
queues, or the packet hot path. Default eight 300-second blocks mean first
eviction is around 40 minutes: **this cannot explain the fresh arm's 349-s
peak and is not a fix for its remaining 24-MiB failure**.

The regression uses unchanged production bounds (2,048 entities / 16,384
associations per block), an injected block clock, and weak-owner/GC checks.
Before the fix, eight logically expired matrices remain reachable; after,
none do, while live matrices/names and pressure release remain correct.
Three fresh processes per arm show retained heap deltas **4,271,016–4,276,336 B
→ -1,880–3,440 B** after test-only GC/scavenge (about **4.07 MiB** released;
small signed residuals are baseline noise). Runtime deltas are
5,480,448–5,701,632 → 868,352–1,236,992 B, not a physical-memory measurement.
Five rare-rotation microbenchmarks give median **93.58 → 104.10 ns/op**,
207–208 B/op and 3 allocations/op in both arms: +10.52 ns on block rotation,
normally once per 300 seconds, not a packet-path or H1 throughput claim.

Connect normal ×10, race ×5 and vet; SDK lifecycle/reclaimer normal ×3,
race ×2 and vet; and all **219** Android host tests pass. Raw local repetitions,
source hashes and commands are in `urnetwork-cleanarm-memory.yC94iJ/REPORT.md`.
The observed Fast.com displays were **71 / 59 / 19 Mbps**, all completed;
there is no paired control or baseline promotion. CNN's child exit 2 remains
a remote/media nondeterministic result pending reproduction, not evidence
for a TLS/403 interpretation or a tunnel repair.

Next attribution needs a separately authorized native-attested diagnostic
with 65,536-byte heap sampling and paired owner census before video, after
video, and after joined browser cleanup, including pre/post-reclaim
boundaries. That diagnostic cannot qualify the memory gate. No new build,
device action, policy change, baseline promotion, or commit was performed
for this analysis. The **whole-run 24-MiB barrier remains unresolved**.

- A low live heap with a high runtime value usually means retained spans,
  stacks, or pool capacity, not a leak. Track `goHeapLiveBytes`,
  `goHeapRetainedBytes`, pool retained bytes, and topology beside runtime.
- `poolOutstanding` is live/in-flight ownership; `poolRetainedBytes` is reusable
  free-list ownership. A pool can reduce allocation rate and still make steady
  memory worse if reclaim keeps its burst high-water.
- `packetPressureDropCount` is cumulative backpressure evidence. It is not a
  leak, but a high count paired with low memory and poor throughput means the
  safety gate is probably the active speed tradeoff.
- Queue budgets are shared aggregate ceilings. Per-flow protocol bytes and
  aggregate retained bytes are deliberately separate: the former preserves a
  useful BDP window, while the latter charges pooled backing classes, encrypted
  outer frames, decoded roots, and owner envelopes already admitted
  asynchronously. Reusing one byte count for both caused the measured
  Cloudflare stall.
- Android PSS includes the app, JVM, graphics, mappings, and other native state;
  it cannot validate the iOS extension ceiling. Android is used here to learn
  Go allocation behavior and compare candidates. A physical iOS
  `phys_footprint`/jetsam pass remains mandatory.
- Do not hide route variance by retrying a failed sample. Alternate controls
  and candidates, preserve failures, and use medians plus tails.

### iOS burst attribution, scoped measurement plan — 2026-09-20

The `kqVGmc` receipt/sample join places the three absolute-cap breaches at
319,471 / 334,468 / 349,469 ms. Browser cleanup completed at 326,655 ms, so
the last peak was **22,814 ms after cleanup**. Fast.com had completed at
244,056 ms; its last nearby sample was 22,986,784 B. Subsequent flow fanout
reached 301 flows. The profile had a 32-MiB soft Go limit and startup heap
sampling disabled, consistent with the existing iOS audit profile; that soft
limit does not enforce the separate 24-MiB absolute acceptance ceiling.

At the three breach samples, residual runtime classes stay within 8 KiB
(6,129,485–6,137,677 B), and stacks fall from 3,440,640 to 3,342,336 B.
Heap objects, in-use span slack and unreleased free pages carry the excess.
Allocation continues at 263,447 B/s in the final 15-second pre-reclaim sample
interval despite browser cleanup, with three natural GCs. This is cumulative
allocation rate, not an equal increase in retained memory. The existing
15-second quiet debounce begins reclamation only after traffic settles and
runtime exceeds its target; recovery after a breach cannot satisfy the
absolute-cap requirement. Reducing windows or forcing GC earlier is not yet
an attributed fix.

Historical, non-qualifying `arm.DHKCh0` pprof callgraph inspection narrows
three candidates for fresh paired attribution. Of 868,708 B sampled under
`bufio.NewWriterSize`, **801,103 B belong to glog file writers**, not H1
carrier buffers. The current logger reserves 256 KiB per opened severity;
first ERROR logging can open INFO/WARNING/ERROR writers and retain 768 KiB.
A mobile-specific smaller logging buffer is therefore a candidate that does
not spend the packet/sequence window. The profile also attributes 593,802 B
of exported string-list backing to `DeviceLocal.updateBlockActions`, 416,600 B
to gomobile reference tracking from `NewStringList`, and 688,349 B to
`multiClientChannel.addSourceToEventBucketWithLock`. These are historical
sampled owners, not proof of the current breach. Event-bucket retirement
already clears removed pointers. Current paired profiles must establish each
owner's growth/release before retaining a remediation.

The next diagnostic uses only the in-scope Wikipedia/Fast.com H1 workload;
public-egress media, including CNN, is excluded. P2P playback remains a
separate device role/workload. No new host runner is required: the retained
workload owner supports an explicit `wiki,fast-1,fast-2,fast-3` child list plus
verified Chrome cleanup. Build/attest a fresh native 65,536-byte-rate iOS
diagnostic, keep the existing continuous collector and owner receipts, and:

1. Require schema-1 census preflight, then pair idle pre-GC census, heap
   profile, post-GC census and last-of-all private stacks.
2. Run the existing Wikipedia five-load and three Fast.com commands. During
   traffic, collect at most one census-only `active-highwater` when a primitive
   sample first reaches 23 MiB or 128 flows; do not force GC or restart work.
3. At joined browser-cleanup +0 / +15 / +30 seconds collect census only,
   preserving the original +23-second peak window and automatic reclaim.
   Record command times, sample/GC counts and reclaim counters throughout.
4. After +35 seconds collect the full paired `post-burst` boundary; repeat
   at +180 seconds of connected quiet. These interventions are diagnostic
   and cannot qualify the cap or promote performance measurements.
5. Disconnect the route, await the exact command completion, collect a full
   pair after 15 seconds, then finish/join the owners. This command confirms
   route disconnect, **not** `DeviceLocal.CloseAndWait`; avoid a false
   process-wide teardown inference.

Publish aggregate owner/size-class/function deltas only; keep profiles/stacks
private. A breach before intervention remains failure evidence. Absence of a
breach in this narrower diagnostic does not repair the historical failure.
Any retained candidate needs an owner-specific deterministic test, measured
allocation/lifetime improvement, and a fresh rate-zero physical comparison
preserving H1 TTFB/Fast.com performance and every sample ≤25,165,824 B.

### Clean physical P2P fixed-offer replay — 2026-09-20

A fresh native-attested P2P arm on the allowlisted Pixel 8 Pro and Galaxy S24
Ultra completed provider start, exact-peer H1 connection, bidirectional client
probe, provider proof, and joined teardown without retries. Both devices used
the iOS-memory-audit profile. The client recorded 19 samples with a maximum
Go runtime of 20,086,816 B; the provider recorded 21 samples with a maximum of
18,546,720 B. Neither had a sample above 24 MiB or a packet-pressure drop.

This keeps the P2P fixed-offer queue remedy compatible with the mobile memory
envelope on the clean-LAN role split. The device harness cannot force the
legacy SCTP lane or inject 120-ms RTT, so it does not replace the deterministic
120-ms service cohort; that cohort admits 468/468 offers in ten normal runs
with zero refusal/retransmission. It is also not evidence about public H1
TTFB, Fast.com, or the unresolved short-lived iOS-profile burst.

### Scoped H1 diagnostic profile versus rate-zero qualification — 2026-09-21

The freshly native-attested `urnetwork-iosscoped-aapt.paZ8U8` Wikipedia and
three-Fast.com H1 arm used the correct 20-MiB admission / 32-MiB runtime policy,
but **all 45 primitive samples had heap-profile rate 65,536**. Native
before/writer/after provenance and the initial live status agree on that rate:
this was a diagnostic configuration, not a propagation failure or rate-zero
qualification. The old profile preflight validated only the two budgets and
incorrectly allowed the diagnostic build into the qualification sequence.

The measured maximum remains **25,442,584 B (24.264 MiB)**, 276,760 B above the
absolute cap, with four over-limit samples. All four are inside this arm's
quiet boundary (23 samples spanning 330,002 ms). The peak at 499,474 ms has
8,513,768 B allocated heap, 4,355,864 B in-use span slack, 1,089,536 B free
unreleased heap, 3,309,568 B stacks, **1,850,363 B profiling buckets**, and
6,323,485 B other runtime classes. Returned pools account for 164,096 B within
heap; outstanding pooled roots are 114,432 B and tracked transfer memory is
1,318,612 B. These categories are not additive to heap a second time.
The retained session produced no paired heap profile/owner-census artifacts,
so individual live-heap owners at the peak remain unproven.

The profiling-bucket cost alone exceeds the overshoot, but subtracting it from
runtime is **not** a valid production measurement or permission to claim a
pass. Sampling also affects allocation timing and GC, while the historical
rate-zero breach remains unresolved. Neither the 24-MiB limit nor production
queue/pool/GC policy changed for this finding.

`physical_memory_profile.mjs` now defaults to qualification and requires the
live numeric rate to be zero before traffic. Explicit diagnostic mode requires
65,536 and always reports `qualificationEligible=false`. The quiet gate
independently requires rate zero at both status boundaries and in every active,
quiet, and teardown sample; missing/string rates fail rather than defaulting to
zero. A memory breach stays visible with its original byte count even when
the profile is invalid. Deterministic tests reproduce this 20/32/65,536
misclassification, diagnostic opt-in, rate changes outside quiet, missing
evidence, and the absolute gate without subtracting profiling buckets.

For the next qualification, freeze `NATIVE_PROFILE_RATE=0`, pass matching zero
to native and app/test builds, and require the updated ready gate before the
unchanged workload. A future owner diagnostic instead needs fresh paired
pre-GC census, heap profile, post-GC census, and last-of-all private stacks at
matched idle, post-burst, and quiet boundaries. A completed session cannot
retroactively provide peak-owner evidence. Public-egress video remains outside
this scoped arm; no physical performance baseline is promoted by this result.

### Valid rate-zero scoped H1 arm: post-traffic burst remains — 2026-09-21

The canonical `physical_h1_arm.mjs` arm `VtKzNI` completed on the allowlisted
Pixel over Wi-Fi with providing disabled, explicit H1, freshly attested native
and APK inputs, the 20-MiB device admission target, 32-MiB Go soft limit, and
heap-profile rate **zero**. All four workload children and browser cleanup
joined successfully. Five cache-disabled Wikipedia loads measured **450.6 ms
median load / 176.6 ms median TTFB**. The three canonical Fast.com displays
were **90 / 54 / 41 Mbps**, median **54 Mbps**, with all three at least 40 Mbps.
The 384 radio/thermal collector samples were eligible. This is one valid
observation, not a paired performance comparison or a new baseline.

The absolute memory gate **fails**: 2 of 27 primitive samples exceed
25,165,824 B. Both are inside the 21-sample connected quiet interval spanning
300,002 ms. The whole-run and quiet maxima are **29,405,216 B (28.043 MiB)**,
**4,239,392 B above the cap**. The peak occurs at 94,362 ms, **3,617 ms after
joined Chrome cleanup**; calling that phase quiet does not imply all VPN work
has drained. At that instant the components are:

| Runtime class | Bytes |
|---|---:|
| Allocated heap objects | 12,178,752 |
| Unused space inside occupied heap spans | 5,941,952 |
| Free heap pages not yet released | 1,351,680 |
| In-use goroutine stacks | 3,276,800 |
| Other runtime classes, including 6,355 B profiling buckets | 6,656,032 |
| **Go runtime total** | **29,405,216** |

The last GC marked 11,447,120 B live. There are 279 goroutines and 101 indexed
flows. Returned message buffers retain only 373,248 B, outstanding packet roots
account for 240,640 B, and tracked resend ownership is 1,433,844 B; receive and
Pack handoff queues are empty. These owner counters overlap heap objects and
are not additive runtime classes. Nine platform claims total 3,801,088 B;
claims describe reserved permission, not measured allocations.

At the next sample, tracked queues have drained and packet roots fall to 256 B,
but runtime still reaches 25,268,256 B. Occupied-span slack has risen to
6,527,224 B as objects die. The sole automatic reclaim subsequently reports
25,333,792 -> 22,982,688 B. The remaining 19 quiet samples range from
23,474,208 to 24,064,032 B, and the final sample is 23,646,240 B. Recovery
after a breach cannot satisfy an absolute gate; further free-list trimming
alone cannot reclaim space inside still-occupied allocator spans.

This rate-zero arm has no paired heap profiles or owner censuses. It proves
the unresolved burst exists in the Wikipedia/Fast.com scope without public
video or profiling overhead; it does not identify the allocations pinning
those spans. Preserve this failure and collect the separate diagnostic
boundaries described above, then use an owner-specific lifetime/allocation
reproduction before retaining a production remedy. Queue/window sizes,
timeouts, GC settings and all acceptance thresholds remain unchanged.
Normal finish and joined cleanup released all five created clients and their
five markers, removed the staged credentials, and left the target app stopped.
