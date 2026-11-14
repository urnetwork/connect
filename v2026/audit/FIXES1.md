# FIXES1 — security fixes from the Cure53 audit preparation (peer review)

Preparing `audit/EVIDENCE.md` for the Cure53 audit (WP1 protocol and data
plane, WP2 session encryption and peer identity) surfaced seven defects. This
file is the review packet for their fixes. The follow-on review below records
two additional enforcement findings (F8–F9) and their implementation status.

| Set | State | Base | Commit |
|---|---|---|---|
| 1 (F1–F3) | Committed, merged to `main` as `69967d8a`, pushed | `03e98fe5` | `7dbcdd4c` on `fix/audit-contract-mac-receive-panics` |
| 2 (F4–F7) | Committed, merged to `main` with this document | `69967d8a` | `5a71282c` on `fix/audit-p2p-source-key-pins-extender-mac` |
| Follow-on (F8–F9) | Committed in connect and SDK; targeted and combined race validation passed | `6d4aeb60` | connect `1ffb741c`; SDK `095efb61` on `fix/audit-history-admission` |

Every fix has a deterministic regression test that **fails on the code before
the fix**. The failure output is quoted with each fix. Line references for
set 1 are at `7dbcdd4c`; line references for set 2 are at `5a71282c`.

## Summary

| # | Defect | Who could trigger it | Impact | Fix |
|---|---|---|---|---|
| F1 | `VerifyStoredContract` accepted the legacy contract MAC with no date gate | Any consumer that ever held a legacy-format contract for a provider | Forge arbitrary contracts for that provider (byte allowance; Network-mode roles and principal) | Legacy form verifies only before `NetworkEventTimeChangeHmac` |
| F2 | Nil dereference on a sealed inner frame without a transfer path | Any peer with an established session | Cancels the whole receiving client (one frame) | `TransferPathFromProtobuf` errors on nil |
| F3 | Explicit panics in `ReceiveBuffer.Pack` on the receive loop | Any peer sending packs (lane-flag reuse of a closing sequence's id) | Cancels the whole receiving client | Drop the other-lane pack; heal a stale head index |
| F4 | P2P-route frames could claim the platform (`ControlId`) or the receiving client as source | A P2P stream peer | Platform impersonation: control frames skip contracts and sessions and drive platform-only handlers | Drop and count them in `Client.run` |
| F5 | Signed-history fetch error accepted the contract key without consulting the pin | The operator (by returning an error instead of the history) | Substituted identity key accepted for a pinned peer | Refuse a contract key that contradicts the pin |
| F6 | Pin extension checked only the head at the pinned generation | The operator (a re-signed, forked history) | A fork that rewrote the pinned generation and appended one passed as a rotation | Require the pinned generation to still name the pinned key |
| F7 | Private-extender admission HMAC compared with `slices.Equal` | A client of a private extender | Timing side channel on a secret-keyed MAC | `hmac.Equal` |

---

## Set 1 — committed (`7dbcdd4c`)

### F1 — legacy stored-contract MAC accepted at any time

**Root cause.**

- The legacy signer is `mac.Sum(storedContractBytes)`. With no `Write` before
  it, `Sum` appends `HMAC(key, "")` to its argument, so the tag is
  `stored_contract_bytes ‖ HMAC(key, "")`.
- The only key-dependent part does not depend on the contract.
- `VerifyStoredContract` accepted that form "at all times", ignoring its
  `settings` argument.
- Consumers receive full contracts, including the tag (`CreateContractResult`).
  So anyone who held one legacy-format contract for a provider could append the
  32-byte suffix to *any* stored-contract bytes and pass the provider's MAC,
  path and expiry checks. This held for as long as the provider kept that key,
  and keys are persisted (`LoadProvideSecretKeys`).

**Fix** (`transfer_contract_manager.go:282-328`):

- The standard `HMAC(key, bytes)` always verifies.
- The legacy form verifies only before `NetworkEventTimeChangeHmac`
  (2026-09-01 by default), the instant signers stopped emitting it. It never
  verifies with nil settings.
- Empty contract bytes never verify, because their standard tag is the same
  key-only value.
- Adjacent bug fixed: both functions called `Sum` on the caller's slice, which
  wrote the HMAC into the caller's spare capacity. Both now pass a full slice
  expression (`b[:len(b):len(b)]`).
- The `NetworkEventTimeChangeHmac` comments were updated (`:342-350`,
  `:448-454`).

**Compatibility.**

- The server's signer (`server/controller/connect_controller.go:1130`) uses the
  resident's `ContractManagerSettings`, which are
  `connect.DefaultContractManagerSettings()` (`server/connect/resident.go:729`).
  No server code overrides `NetworkEventTimeChangeHmac`; the monitor
  (`server/monitor/signal_hmac_cutover.go:38`) only reads the default. So the
  server has emitted only the standard form since 2026-09-01.
- No other signer exists in the server, sdk or warp repositories.
- Server tests sign and verify with the same defaults.
- The server issues contracts with a 60-minute expiry
  (`DefaultContractExpiration`, `server/model/subscription_contract_expiration.go:14`),
  so every legitimate legacy-tagged contract expired shortly after the cutover.
- A provider whose clock reads before 2026-09-01 still accepts legacy until its
  clock passes the date.

**Tests** (`transfer_contract_hmac_test.go`), with the failure each showed
before the fix:

| Test | Before the fix |
|---|---|
| `TestLegacyContractHmacForgeryRejectedAfterCutover` | `:69 false does not equal true` (forgery verified) |
| `TestEmptyStoredContractBytesNeverVerify` | `:102 false does not equal true` |
| `TestStoredContractHmacLeavesCallerBytesUntouched` | Spare-capacity sentinel overwritten by `HMAC(key,"")` |
| `TestContractManagerRefusesForgedLegacyContract` | `:161 false does not equal true`: the provider's `ContractManager.Verify` accepted a 1 GiB contract forged from a 1 KiB one |

`TestStoredContractHmacCutover` asserted "accepts BOTH formats regardless of
the cutoff time", which was the bug; it was updated.

**Review questions.**

1. We found one signer, and it uses the defaults. Is there a deployment that
   sets a later `NetworkEventTimeChangeHmac`, or a signer outside these
   repositories?
2. Should provide secret keys created before 2026-09-01 be rotated anyway, as
   defense in depth for clients whose clocks are wrong?

### F2 — nil dereference on a sealed inner frame without a path

**Root cause.**

- `Client.run` checks the sealed inner path against the outer one with
  `TransferPathFromProtobuf(unwrappedTransferFrame.TransferPath)`.
- The decoder leaves `TransferPath` nil when field 1 is absent, and the function
  dereferenced `protoTransferPath.SourceId` directly.
- `Client.run` runs as `HandleError(client.run, cancel)`, so the panic cancelled
  the whole client.
- Any peer holding the session key could send it — for example a consumer
  against a provider.

**Fix** (`connect.go:81-96`):

- A nil path returns `errors.New("Missing transfer path")`.
- The existing mismatch branch then drops the frame and records a bad message.
- All callers already handle the error. `FilteredTransferPath`'s fallback
  already guarded nil.

**Test:** `TestSealedFrameWithoutInnerPathDoesNotStopTheClient`
(`transfer_receive_loop_crash_test.go`).

- It injects an established, identity-verified session with a known AES-GCM
  key, then sends two frames through a receive route: a sealed frame without an
  inner path, then a valid sealed frame. The second must be delivered and the
  client must stay alive.
- Before the fix: `runtime error: invalid memory address or nil pointer
  dereference` in `Client.run`, then "a sealed frame without an inner transfer
  path stopped the client".

### F3 — explicit panics in `ReceiveBuffer.Pack` on the receive loop

**Root cause.**

- `receiveSequenceId` omits the pack's lane flags (`force_stream`,
  `companion_contract`), but `receiveSequenceHeadKey` includes them.
- Suppose a pack reuses a live sequence's id under another lane while that
  sequence is closing: its `Pack` fails because its context is cancelled, but
  it is still indexed.
- The retry pass then asserted that the pack's lane head was that id and
  panicked with `[r]incorrect head sequence` on `Client.run`, cancelling the
  client.
- A peer controls when that close starts. For example, a sequence closes
  itself after `ReceiveBufferSettings.IdleTimeout` with an empty queue
  (`transfer.go:17539` at `7dbcdd4c`), so the peer only has to go quiet. The other-lane pack
  must then arrive before the sequence leaves the index. That is a narrow race,
  but the peer can retry it. The test holds the window open with a hook.
- The sibling assertion, `[r]upgrade older sequence … = …`, fired when a head
  index named the pack's own id with no live sequence, i.e. a stale index.

**Fix** (`transfer.go:15610-15626`, `:15672-15679` at `7dbcdd4c`):

- The other-lane pack is dropped and returned to the pool, logged at V(1).
- The stale index is deleted, and the loop continues to open the sequence, as
  the adjacent "missing older head" path already did.

**Tests** (`transfer_receive_loop_crash_test.go`):

- `TestPackOfAnotherLaneForAClosingSequenceDoesNotStopTheClient`:
  `beforeRunReceiveSequenceForTest` holds the first sequence's worker so it
  stays indexed; the test cancels it, then sends the same id on the
  force-stream lane. Before the fix the receive loop panicked
  `[r]incorrect head sequence 0000… != 01a1…` and the client stopped.
- `TestReceiveBufferHealsAStaleHeadIndexForThePacksOwnId`: before the fix,
  `panic: [r]upgrade older sequence X = X`.

**Review questions.**

1. Dropping is correct only if senders never reuse a sequence id across lanes.
   Send sequences are keyed by lane (`sendSequenceId` includes `ForceStream`,
   `CompanionContract` and `LogicalLane`, `transfer.go:5984-5988`), so they
   should not; confirm.
2. On the *first* pass, a lane-mismatched pack is still handed to the live
   sequence of the other lane, as before. This doesn't crash, but should it be
   dropped there too?

---

## Set 2 — committed (`5a71282c`)

### F4 — P2P peers could impersonate the platform or the receiving client

**Root cause.**

- `Client.run` attributes each frame to the `TransferPath.SourceId` it names.
  Nothing on the P2P path compares that with the WebRTC peer:
  - `P2pReceiveTransport.MatchesReceive` returns true;
  - the receive worker and the multi-route reader hand raw bytes through.
- A stream peer could therefore send frames naming `ControlId` (`Id{}`), which
  are treated as platform traffic. Such frames:
  - need no contract and have no session;
  - drive platform-only handlers:
    - `StreamManager` (stream open/close);
    - `sequenceCloser` (CloseSequences);
    - `PeerManager`;
    - `ContractManager` control frames;
    - on a provider, `RemoteUserNatProvider.senderDisconnected` (`ip.go:8781`),
      which "purges all state for a sender after an authenticated platform
      disconnect" for every client a `NetworkPeersUpdate` names as
      disconnected;
    - resident migration: `ip_remote_multi_client.go:21524` guards
      `TransferResidentMigrate` with `IsControlSource()`, which a spoofed frame
      satisfies.
- Frames naming the receiving client's own id are self traffic and are exempt
  from contracts too.

**Fix** (`transfer.go:4664-4680`):

- In `Client.run`, a frame delivered on a `TransportTypeP2p` route that names
  `ControlId` or this client is dropped and counted.
- The counter is `ClientReceiveStatsSnapshot.ForgedSourceDropCount`
  (`:1734-1736`, `:2142`, `:2549`).
- Both P2P lanes report `TransportTypeP2p`: the native fast lane is a
  `receiveLaneTransport` that inherits its base's type.

**Why not a full source check.**

- A full check would require every P2P frame to name the peer at the far end.
  That breaks multihop streams: an endpoint's P2P transport peers with its
  *adjacent* hop (`StreamOpen` carries only adjacent ids), while forwarded
  frames name the stream's origin. See
  `TestIntermediaryStreamSequenceMatchesAdjacentDestinationsBothDirections`,
  where the destination endpoint peers with the intermediary.
- Binding other peer ids needs the receive side to learn the verified far end
  per stream. Today only the writer side tracks it
  (`RouteManager.writerStreamAuthenticatedDestinations`).
- That remains open (EVIDENCE L7) and needs a design decision.

**Test:** `TestP2pRouteFrameClaimingPlatformOrSelfIsDropped`
(`transfer_p2p_forged_source_test.go`).

- Through a receive route typed `TransportTypeP2p`, it sends a frame naming
  the platform, one naming the client, then one from the peer.
- The peer's frame must be delivered, and `ForgedSourceDropCount` must be 2.
- With the check disabled (counter kept):
  `transfer_p2p_forged_source_test.go:89 2 does not equal 0`.

**Review questions.**

1. Is there any legitimate case of a platform-sourced or self-sourced frame on
   a P2P route? I found none.
2. Should the drop also feed the peer audit? There is no trustworthy source to
   attribute it to.
3. What design should the per-stream far-end binding (L7) use?

### F5 — signed-history fetch error ignored the pin

**Root cause.**

- `resolvePeerClientKeyHistory` fell back to the contract key on any fetch
  error, by design ("a fetch error is not a mismatch", DESIGNNOTES3 §5.3).
- For a *pinned* peer, though, the per-peer ratchet already refuses an empty
  history (§4).
- So an operator that wanted to substitute a pinned peer's key could answer
  with an error instead of with nothing.

**Fix** (`transfer_key_history_session.go`):

- The fetch-error decision moved into `applyPeerClientKeyHistoryFetchError`
  (`:175-203`).
- Pin lookup is shared with `applyPeerClientKeyHistory` through
  `peerClientKeyPin` (`:161-173`).
- A pinned peer whose contract key differs from the pin is rejected
  terminally.
- An unpinned peer, or a pinned peer presenting its pinned key, still falls
  back. The availability argument of §5.3 (one API blip must not exclude every
  provider) is untouched: only pinned peers whose key changed during the
  outage are affected.
- `DESIGNNOTES3.md` §5.2–§5.3 carry an amendment that records the decision.

**Test:** `TestSignedIdentityFetchErrorRefusesAChangedPinnedKey`
(`transfer_key_history_pin_test.go`).

- It pins the golden chain, then resolves with a failing fetcher, waiting on
  `beforeWorkerDoneForTest`.
- A changed key must be rejected, and the pinned key must still be verified.
- Before the fix: `transfer_key_history_pin_test.go:91: state = verified, want
  rejected (pinned key changed while the history was unavailable)`.

**Review questions.**

1. Is terminal rejection right, or should the session stay pending and retry?
   A legitimate rotation during an API outage loses that provider for this
   session only.

### F6 — pin extension accepted forks that advance past the pinned generation

**Root cause.**

- `ClientKeyPin.extendedBy` received only the head. Besides the domain, it
  checked that the head had not rewound below the pinned generation, and that
  the key matched *if* the head sat exactly at the pinned generation.
- A validly signed chain that rewrote the pinned generation's key and appended
  one more generation passed as a rotation.

**Fix** (`transfer_key_history.go:437`, `:465`, `:477`, `:510-533`):

- `VerifyClientKeyHistory` keeps the verified chain, which is bounded by
  `MaxClientKeyHistoryGenerations`.
- `extendedBy` requires all of:
  - the same domain;
  - a chain that reaches the pinned generation;
  - `chain[pin.Generation-1]` present and naming the pinned key (generations
    are contiguous from 1).
- A zero-generation pin is refused.
- The `ClientKeyPin` struct is unchanged, so pins persisted by the SDK
  (`sdk/peer_client_key_pin_store.go`) stay compatible.

**Test:** `TestPinRefusesForkThatAdvancesPastThePinnedGeneration`
(`transfer_key_history_pin_test.go`).

- It pins generation 2, then verifies two chains: a rotation that appends
  generation 3 (accepted), and a fork that rewrites generation 2 and appends
  generation 3 (refused).
- Before the fix: `transfer_key_history_pin_test.go:48: a fork that rewrites the
  pinned generation and advances past it was accepted, err = <nil>`.

**Review questions.**

1. Should the pin also record the content hash of the pinned record? That would
   also catch forks that keep the pinned key but rewrite other fields or
   earlier generations, but it needs a pin-store format change in the SDK.

### F7 — private-extender admission HMAC compared without constant time

**Root cause.**

- `ExtenderServer.IsAllowedSecret` compared the expected
  `HMAC(secret, timestamp ‖ nonce)` with the client-supplied signature using
  `slices.Equal`, which stops at the first differing byte.

**Fix** (`extender/extender.go:1457-1479`): `hmac.Equal`.

**Tests** (`extender/extender_secret_compare_test.go`):

- `TestAllowedSecretComparesInConstantTime`: constant time can't be observed
  behaviourally, so the test parses the method's source, as
  `layering_test.go` does elsewhere, and requires `hmac.Equal` and no
  `slices.Equal`, `bytes.Equal` or `reflect.DeepEqual`. Before the fix:
  `extender_secret_compare_test.go:54: the admission signature is not compared
  with hmac.Equal` and `:58: … is compared with slices.Equal, which stops at
  the first differing byte`.
- `TestAllowedSecretRefusesPartialSignatures`: prefix, extension, empty and
  bit-flipped signatures are refused. This is a behavioural guard for the new
  comparison; it passes before the fix too.

**Not fixed here:** timestamp and nonce are MACed but not checked for
freshness, so a captured header replays (EVIDENCE L14).

---

## Verification

- **Regression tests:** twelve new tests and one updated test
  (`TestStoredContractHmacCutover`), in two packages. All pass with the fixes.
  Eleven were run without their fix and failed as quoted above. The twelfth,
  `TestAllowedSecretRefusesPartialSignatures`, is a behavioural guard that
  passes either way.
- **Seal tests:** the 192 seal-related tests pass with set 1 (EVIDENCE §3.10).
  The full-suite run below covers them for set 2.
- **Full root package, set 1:** passes except five tests in unrelated
  subsystems that fail identically on an unmodified `03e98fe5`:
  - deterministic: `TestCanonicalH1ChunkSettingsRealOwnersStayIndependent`,
    `TestPlatformWebSocketReadyDrainBalancesAcksAndPayloadBytes`,
    `TestWindowPathServicePerformanceMatrix`;
  - intermittent: `TestNatMemoryTcpAckProgressAtFullDataBudget`,
    `TestNatProviderMemoryTcpAckProgressAtFullDataBudget`.
- **Full root package and `./extender/`, set 2:** see the section below.
- **`govulncheck` v1.9.0** (Go 1.27.3): no vulnerabilities in called code. One
  module-level entry, GO-2026-5932 (`golang.org/x/crypto/openpgp`), is in a
  package connect does not import.

```sh
go test -count=1 -run 'TestLegacyContractHmacForgeryRejectedAfterCutover|TestEmptyStoredContractBytesNeverVerify|TestStoredContractHmacLeavesCallerBytesUntouched|TestContractManagerRefusesForgedLegacyContract|TestStoredContractHmacCutover|TestSealedFrameWithoutInnerPathDoesNotStopTheClient|TestPackOfAnotherLaneForAClosingSequenceDoesNotStopTheClient|TestReceiveBufferHealsAStaleHeadIndexForThePacksOwnId|TestP2pRouteFrameClaimingPlatformOrSelfIsDropped|TestSignedIdentityFetchErrorRefusesAChangedPinnedKey|TestPinRefusesForkThatAdvancesPastThePinnedGeneration' .
go test -count=1 -run 'TestAllowedSecretComparesInConstantTime|TestAllowedSecretRefusesPartialSignatures' ./extender/
```

### Set 2 full-suite result

Run at `5a71282c` on 2026-11-06 with Go 1.27.3, while other test runs shared
the machine:

- `go test -count=1 ./extender/`: ok (193.5 s).
- `go test -count=1 .`: everything passes except the three deterministic
  failures listed above, in 1607.5 s with no panics. Each fails with the same
  assertion as in the set 1 run, and all three also fail on an unmodified
  `03e98fe5`:
  - `TestPlatformWebSocketReadyDrainBalancesAcksAndPayloadBytes`:
    `net_websocket_batch_test.go:275: tunnel-MTU ready drain=(10, 12800B), want
    12 below 16KiB`;
  - `TestCanonicalH1ChunkSettingsRealOwnersStayIndependent`:
    `private_canonical_h1_chunk_settings_test.go:151: owner0 chunk0=6 want7 or
    premature completion`;
  - `TestWindowPathServicePerformanceMatrix`: the same two cells,
    `rate=125000/rtt=400ms/compression=50ms` with 1 and 8 flows, report
    `service pacing underfilled` (`transfer_window_service_performance_test.go:173`).
- The two intermittent NAT tests passed in this run.
- No failure is new.

## Follow-on review — signed-history enforcement (2026-11-06)

An independent review of `7dbcdd4c` and `5a71282c` at merged HEAD
`6d4aeb60` found two additional enforcement gaps around F5/F6. Both predate
those commits and remain reachable after them. Deterministic review probes
reproduced both; a second source review confirmed handshake reachability and
the shared SDK store's concurrency behavior.

| Item | Finding | Follow-on status |
|---|---|---|
| F8 | Required signed-history pending, rejected, or store-unavailable state blocks outbound `Cipher()` but still exposes inbound decryption ciphers and permits application delivery, including queued delivery. | Committed in `1ffb741c`; nine deterministic receive regressions and combined validation with F9 pass under the race detector. |
| F9 | Pin lookup, chain verification, and pin commit are not one atomic admission operation across sessions/managers sharing a store. A resolution verified against G1 can overwrite a newly pinned G2 with a G3 chain that forks at G2. | Implemented in connect and both SDK stores. Thirteen new connect regressions, the SDK pin-store group, and combined validation pass under the race detector. |

Targeted and combined results are recorded below. The implementation is
committed as connect `1ffb741c` and SDK `095efb61`; this does not claim a
deployed release.

The review found no newly introduced blocking defect in F1–F4 or F7. The
documented first-pass receive-lane mismatch remains a separate nonblocking
follow-up: a live sequence can receive a pack whose immutable lane flags do
not match its own before the closing-sequence guard is reached.

### F8 — inbound signed-history admission

The session's history predicate now gates outbound cipher access, inbound
current/prior decrypt candidates, and final application dispatch. Delivery
rechecks after queuing; receipt batches recheck each item. Admission is
linearized under the session lock before invoking a callback, with no lock
held through the callback. An already-admitted callback may finish after
rejection; later queued callbacks must recheck.

A refused decoded item retires only its receive sequence, releases its
reservations and pooled ownership, and withdraws held selective custody.
A sequence-local refusal latch prevents a later controls-only cleanup batch
from cumulatively acknowledging refused application data, even if history
opens before cleanup. Pending ciphertext refusal also suppresses the
unknown-wrap reset: local admission delay does not mean the peer lost its
cipher. Plaintext bootstrap and verified rekey overlap remain available.

The nine regressions in `transfer_key_history_receive_test.go` cover:

- `TestRequiredSignedHistoryBlocksInboundCipherCandidates`;
- `TestRequiredSignedHistoryReceiveRetriesAfterApproval`;
- `TestRequiredSignedHistoryRejectsAlreadyDecryptedPack`;
- `TestRequiredSignedHistoryRejectsAlreadyDecryptedNoAck`;
- `TestRequiredSignedHistoryRejectsBufferedHeldDelivery`;
- `TestRequiredSignedHistoryRefusalKeepsCleanupAckGap`;
- `TestRequiredSignedHistoryRejectsLaterReceiptDispatch`;
- `TestRequiredSignedHistoryKeepsPlaintextBootstrap`;
- `TestRequiredSignedHistoryBootstrapFinishesBeforeAdmission`.

Seven regression cases fail semantically against pre-fix production behavior:
exposed pending decrypt candidates, pending ciphertext delivery, decoded queued
delivery after rejection (including no-ACK), held-custody refusal, cleanup ACK
gap, and later receipt dispatch. The two bootstrap controls pass before and
after the fix. The red run overlays the three receive production files from
`6d4aeb60`, retaining only the new error symbol and P2 settings declaration so
the tests compile; it does not retain F8 behavior.

All nine final regressions passed with:

```sh
env GOCACHE=/tmp/codex-go-cache go test -race -count=1 \
  -run '^TestRequiredSignedHistory' -timeout=120s .
```

An earlier 20-test race group also passed: the eight then-current new cases,
identity gating, Required establishment, rekey/data retention, receipt-merge
custody, and the pool-entry-point heading check. The explicit no-ACK case was
then added and passed in the nine-test run above. Combined F8/F9 validation
also passed after the atomic-store changes and fixture upgrades were applied.

### F9 — atomic pin and latch admission

`AtomicPeerClientKeyPinStore` adds a single snapshot of the peer pin, its
presence, and the global signed-history latch, plus conditional admission
against that exact snapshot. A non-nil next pin commits the verified head and
latch together. A nil next pin compares without writing; fetch-error fallback,
empty-history answers, and rejection decisions therefore cannot use stale
evidence either.

The resolver verifies outside session/store locks. A failed comparison causes
a new read and full verification against the current pin, not an unconditional
head overwrite. Genuine extensions and identical heads can converge; an
advancing fork is rejected against the intervening pin. Retries are bounded by
`EncryptionSettings.MaxClientKeyPinAdmissionAttempts` (default 4); nonpositive
values refuse configured-store admission. Cancellation, exhausted attempts,
an unchanged snapshot after a refused comparison, or a local store error never
opens the gate. Local failures retain the distinct store-unavailable outcome.

The successful comparison is the admission point. Later rotations do not
retroactively revoke an already admitted session. This is atomicity across
sessions and managers sharing the same store; existing external directory
ownership requirements remain, including the legacy store's single-owner
precondition. It does not add cross-process locking to that store.

**Store/API compatibility.** The old Go interfaces remain source-compatible.
Required signed-history resolution with a configured store now requires the
atomic interface; an old custom store must be upgraded at its shared owner or
admission fails closed. Independently wrapping one backing store per client
would recreate the race. A nil store retains the documented no-persistence
policy. Both SDK implementations are updated; their JSON format is unchanged.
The legacy SDK path now reports unreadable/corrupt existing state as a checked
failure instead of silently treating it as an empty first-contact store.

SDK commits stage complete bytes, sync and close, rename, and sync the parent
directory before publishing memory. A persistence error retains previous
memory and makes later checked admission fail closed. After a rename followed
by a directory-sync error, disk durability is uncertain, but the file is a
complete old or new state rather than a partial pin/latch update.

Two adjacent compatibility cases are covered:

- An old legacy writer could persist a valid pin and crash before its separate
  global-latch write. Both loaders recover the in-memory latch from a nonempty
  set of validated pins without changing the file format or requiring a write
  during load. A later normal commit persists the recovered latch.
- A build-authorized forward signer rotation must survive storage admission.
  The snapshot-bound path accepts a strictly forward signer transition after
  full chain/trusted-signer verification; unconditional setters keep their
  stricter signer guard. Real-signature tests verify an authorized A-to-B
  rotation, refusal without B's authority, stale-token refusal, and reload.

**Deterministic evidence.** Thirteen top-level tests in
`transfer_key_history_atomic_test.go` cover real
`SetPeerClientPublicKey` → fetch → resolve interleavings across sessions in
one manager and across managers sharing a store: advancing and equal-generation
forks, genuine extension retry, identical-head idempotence, fetch-error
acceptance/rejection races, empty-history peer/global-latch races, read-only
store errors, conflict/attempt limits, cancellation, unsupported legacy stores,
and nil-store policy. All thirteen pass under `-race`.

The advancing-fork, fetch-error, and empty-history regressions fail semantically
when an overlay restores the entire `6d4aeb60` resolver, retaining only the
additive public type declarations needed to compile the tests:

```text
concurrent fork: first=verified later=verified, want verified/rejected
fetch-error race: state=verified want=rejected
empty-history race: stale unsigned decision survived the ratchet
```

SDK regressions in `peer_client_key_pin_store_atomic_test.go` and
`peer_client_key_pin_store_rotation_test.go` exercise both shipping stores:
stale advancing forks, valid extension retry, read-only latch comparison,
identity/generation guards, staged persistence failures with and without a
prior pin, old-file recovery, authorized signer rotation, and bounded
capacity/lifecycle/auth-owner guards. The store group and device-level
propagation/replacement/close-race tests pass under `-race`.

Behavioral overlays that remove each store's snapshot comparison reproduce
stale fork/latch admission. Publishing legacy memory before persistence
reproduces a partial in-memory update after a write failure. Removing either
loader's latch recovery reproduces the old split-write downgrade. Restoring
strict signer equality in the legacy CAS reproduces refusal of a verified
forward signer rotation; the same probe fails for the bounded CAS under its
old guard too. These are assertion failures, not compile failures.

### Follow-on verification

Run on 2026-11-06 with Go 1.27.3, darwin/arm64, against the working tree based
on connect `6d4aeb60`. The SDK uses its existing local connect replacement;
no dependency pins, stored formats, or wire formats changed.

- All 54 tests then present in `transfer_key_history*_test.go` passed under
  `-race` after the core/fixture changes: 45 existing history tests and the
  nine F8 regressions (package 3.710 s).
- All 13 new `TestSignedIdentityAtomic...` tests passed under `-race`
  (3.425 s). Together with F8, there are 22 new connect regression/control
  tests, including two bootstrap controls that already passed before F8.
- The final combined connect race group passed (7.342 s), covering all history
  files, the original HMAC/receive-crash/P2P regressions, HMAC cutover,
  identity/Required handshake and rekey continuity, receipt-merge custody,
  pool ownership headings, and the package-layering gate.
- All 22 selected SDK store tests passed under `-race` after both latch and
  signer corrections (5.749 s), including ten new atomic/rotation tests.
  The five device-level pin-store propagation, refusal, replacement,
  caller-store and close-race tests also passed under `-race` (2.058 s).
- The original two extender admission regressions passed under `-race`
  (1.561 s).
- `gofmt` and `git diff --check` are clean in both repositories. `go vet`
  passed for connect and its extender package, and for the SDK root package.
- Pre-fix behavior overlays failed at the intended assertions: F8's seven
  receive cases; F9's three resolver cases; both stores' stale fork/latch
  admission; legacy memory publication before persistence; both loaders'
  old-file latch recovery; and both stores' authorized signer rotation under
  the old strict guard. Additive API declarations are kept where necessary
  for compilation; none of these red results is a compile failure.

Focused rerun commands, from the respective repositories:

```sh
# connect: the 22 new regression/control tests
env GOCACHE=/tmp/codex-go-cache go test -mod=readonly -race -count=1 \
  -run '^Test(RequiredSignedHistory|SignedIdentityAtomic)' -timeout=180s .

# sdk: store and actual device-constructor/wiring coverage
env GOCACHE=/tmp/codex-go-cache go test -mod=readonly -race -count=1 \
  -run '^Test(PeerPinStores|LegacyPeerPinStore|LocalStatePeerClientKeyPinStore|BoundedPeerPinStore|DeviceLocalPeerPinStore)' \
  -timeout=180s .
```

Validation was targeted; the full root and SDK suites were not rerun for this
follow-on. The historical set-2 full-suite results above remain scoped to that
commit. The first-pass live-lane mismatch, L7/L14, and the other documented
residuals below are not claimed fixed by F8/F9.

## Still open (documented in EVIDENCE §9, not addressed here)

| Item | Where |
|---|---|
| Per-peer source binding on P2P routes for non-platform ids | L7 |
| Private-extender header freshness (replay of a captured header) | L14 |
| Content-hash pinning of the signed key history | F6, question 1 |
| Sequence-layer replay gaps: no-ack packs; re-opened removed sequences | L17 |
| H3 `DialEarly`: the JWT can be 0-RTT data | L18 |
| Egress public-unicast check treats 100.64.0.0/10 and other reserved ranges as public | L23 |
| Random-nonce GCM with no per-key invocation limit | L4 |
| First-contact substitution by a consistent operator | L1 |
