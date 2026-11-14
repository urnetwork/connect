# EVIDENCE — URnetwork `connect` security audit (Cure53)

| | |
|---|---|
| Repository | `github.com/urnetwork/connect` |
| Commit | `5a71282c69b02d92be15738fb064fc9ad7a39bc8` (2026-11-06), which contains both sets of fixes in §1.4 |
| Prepared | 2026-11-06 |
| Toolchain | Go 1.27.3 (`go.mod`: `go 1.27.3`) |
| Work packages | **WP1** connect protocol & data plane · **WP2** session encryption & peer identity |
| Focus | The end-to-end **encryption seal** (per-peer session encryption) and the peer identity it binds to |

Every `path:line` reference points at the commit above. The design rationale for
the seal is in `DESIGNNOTES.md` §3, `DESIGNNOTES2.md` (fail-closed mode and the
operator-MITM analysis) and `DESIGNNOTES3.md` (signed client-key history). Where
a comment or note disagrees with the code, the code is authoritative; §4.9 lists
the comment drift we know about so it does not cost review time.

This document states what we claim, where the code that implements each claim
lives, and where the claims stop. The limitations in §1.3 and §9 are not
oversights we are hiding; they are the areas we most want reviewed.

## Contents

1. Summary for the auditors
2. System map and trust boundaries
3. WP2 — the encryption seal
4. Seal threat analysis, by audit focus area
5. WP1 — protocol and data plane
6. Audit-scope cross-reference
7. Third-party dependencies and supply chain
8. Storage, logging and memory hygiene
9. Consolidated limitations and open items
10. Reproducing the evidence
- Appendix A — seal constants and defaults
- Appendix B — seal file index

---

## 1. Summary for the auditors

### 1.1 The seal in five points

1. Each pair of peers runs **TLS 1.3 inside the connect transfer protocol**. The
   handshake bytes travel as `EncryptedControl` frames inside ordinary reliable
   packs. TLS is used only for key agreement; no application data crosses the
   TLS record layer.
2. Application `TransferFrame`s are sealed with **AES-256-GCM** under a 32-byte
   key exported from that TLS session
   (`ExportKeyingMaterial("urnetwork-connect-aead", nil, 32)`), with a fresh
   random 96-bit nonce per frame.
3. The TLS session is authenticated by each peer's long-lived **Ed25519 identity
   key**, not by X.509. Each side signs a second exporter value
   (`"urnetwork-connect-identity-proof"`), and the AEAD is withheld until the
   peer's signature verifies.
4. Policy is a tri-state: **Off** (library default), **Opportunistic** (seal when
   possible, plaintext otherwise) and **Required** (fail-closed in both
   directions). The consumer's "Post Quantum Encryption" profile selects
   Required.
5. The operator distributes the peer's identity key (in contracts and through
   `GET /key/<clientId>`). Under Required, that key is also checked against an
   operator-signed, hash-chained registration history with a local ratchet.

### 1.2 Properties we claim

| # | Property | Evidence |
|---|---|---|
| P1 | Confidentiality and integrity of sealed frames against any party that does not hold the session's TLS secrets | §3.3 |
| P2 | The AEAD is never used before the TLS handshake has completed **and** the peer's identity proof has verified | §3.5 |
| P3 | An active MITM that does not control identity-key distribution cannot obtain a usable session: the proof is bound to the TLS exporter of the session it signs | §3.4.3, §4.3 |
| P4 | Under Required, no plaintext application data in either direction, including during rekeys | §3.7 |
| P5 | Post-quantum hybrid key agreement (X25519MLKEM768) is preferred, and the negotiated group is observable | §3.2, §4.1 |
| P6 | The single receive loop never blocks on TLS state, so one peer cannot stall all peers | §4.7 |
| P7 | The inner (sealed) `TransferPath` is authenticated; tampering with the outer routing path is detected | §3.3 |

### 1.3 Known limitations we ask you to review explicitly

| # | Limitation | Where |
|---|---|---|
| L1 | The operator is the identity-key distributor. An operator that is malicious from a client's first contact, and consistent about it, can substitute keys ("Finding 2"). The signed history makes later substitution attributable; it does not prevent first-contact substitution. | §3.4.5, §4.3, `DESIGNNOTES2.md` §3, `DESIGNNOTES3.md` §8 |
| L2 | Opportunistic mode is downgradable by design: anyone who can drop or alter `EncryptedControl` frames can force plaintext. Required exists for this reason. | §3.1, §4.1 |
| L3 | The signed-history gate applies only under Required. When the history fetch errors, it accepts the contract key for an unpinned peer (availability policy), but refuses a key that contradicts a pin. | §3.4.5 |
| L4 | AES-GCM uses random 96-bit nonces and there is no per-key invocation counter or volume/time rekey trigger. | §4.8.1 |
| L5 | Some traffic is plaintext by design inside the transport: the control plane, `EncryptedControl` frames, acks, the outer routing header, and frame sizes and timing. | §3.8, §4.6 |
| L6 | The seal always uses AES-GCM, so CPUs without AES instructions use Go's table-based software AES. | §4.4 |
| L17 | The seal has no replay window of its own. Replay handling is at the sequence layer, which does not deduplicate no-ack packs and can re-open a removed sequence from a replayed head pack. | §4.2, §5.2 |

The complete list, including the transport, storage and data-plane items
(L7–L23), is in §9.

### 1.4 Fixes made while preparing this document

Preparing this evidence surfaced seven defects, fixed in two sets. Each has a
regression test that fails on the code before its fix. The first set is
commit `7dbcdd4c` (previous code `03e98fe5`):

| Defect at `03e98fe5` | Fix | Evidence |
|---|---|---|
| `VerifyStoredContract` accepted the legacy contract MAC at any time. That tag is the contract bytes followed by `HMAC(key, "")`, a value of the key alone, so one legacy tag let its holder tag arbitrary contract bytes under the same provide key. | The legacy form verifies only before `NetworkEventTimeChangeHmac` (2026-09-01 by default), never with nil settings, and never for empty bytes. Neither function writes into the caller's spare capacity. | §5.3; `transfer_contract_hmac_test.go` |
| A sealed inner frame without a transfer path made `Client.run` dereference nil. Any session peer could cancel the receiving client this way. | `TransferPathFromProtobuf` returns an error for a missing path, and the frame is dropped and audited. | §5.1; `TestSealedFrameWithoutInnerPathDoesNotStopTheClient` |
| `ReceiveBuffer.Pack` panicked on the receive loop for a pack that reused a closing sequence's id under another lane, and asserted on a stale head index. Either one cancelled the client. | The pack is dropped and a stale index is healed. | §5.1; `TestPackOfAnotherLaneForAClosingSequenceDoesNotStopTheClient`, `TestReceiveBufferHealsAStaleHeadIndexForThePacksOwnId` |

The second set is commit `5a71282c` (previous code `7dbcdd4c`, merged to
`main` as `69967d8a`):

| Defect at `7dbcdd4c` | Fix | Evidence |
|---|---|---|
| A P2P stream peer could send frames naming the platform (`ControlId`) or the receiving client as their source. The receive loop took them as control-plane or self traffic: no contract, no session, and platform-only handlers (stream open/close, sequence close, resident migration, peer list) acted on them. | A P2P-route frame naming either source is dropped and counted. | §2.3; `TestP2pRouteFrameClaimingPlatformOrSelfIsDropped` |
| On a history-fetch error, the signed-identity gate accepted the contract key without consulting the peer's pin. An operator could answer with an error instead of with nothing to get a substituted key past a pinned peer. | A pinned peer whose contract key contradicts its pin is refused. An unpinned peer, or a pinned peer presenting its pinned key, still falls back. | §3.4.5; `TestSignedIdentityFetchErrorRefusesAChangedPinnedKey` |
| Pin extension compared only the head, and only when it sat at the pinned generation. A validly signed chain that rewrote the pinned generation and appended one more passed as a rotation. | The pinned generation must still name the pinned key. | §3.4.5; `TestPinRefusesForkThatAdvancesPastThePinnedGeneration` |
| The private-extender admission header compared its secret-keyed HMAC with `slices.Equal`, an early-exit comparison. | `hmac.Equal`. | §5.7; `extender/extender_secret_compare_test.go` |

---

## 2. System map and trust boundaries

### 2.1 Layers

```
  real internet  ┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄
       │ egress sockets (provider side)
  ┌────┴───────────────────────────────────────────────────────────────┐
  │ IP / TUN data plane     ip*.go, tun.go          (WP1)              │
  ├────────────────────────────────────────────────────────────────────┤
  │ Contracts               transfer_contract_manager.go  (WP1)        │
  ├────────────────────────────────────────────────────────────────────┤
  │ END-TO-END SEAL         transfer_encrypt.go, transfer_key*.go (WP2)│
  ├────────────────────────────────────────────────────────────────────┤
  │ Reliable sequences      transfer.go             (WP1)              │
  ├────────────────────────────────────────────────────────────────────┤
  │ Routing                 transfer_route_manager.go (WP1)            │
  ├────────────────────────────────────────────────────────────────────┤
  │ Transports              transport*.go, net*.go  (WP1)              │
  │   platform H1 (WebSocket) / H3 (QUIC), WebRTC P2P, DNS PT, extender│
  └────────────────────────────────────────────────────────────────────┘
```

Sealing happens at the moment a `TransferFrame` is written to the multi-route
writer, and unsealing happens when an inbound `TransferFrame` is parsed in the
receive loop. All reliability machinery (sequence numbers, acks, retransmits,
contracts) operates on unsealed bytes (`DESIGNNOTES.md` §3.1).

`ControlId` is the zero `Id{}` and denotes the platform (`connect.go`); the
platform runs the same `Client` code.

### 2.2 Adversaries and what the seal gives against each

| Adversary | Position | What the seal provides |
|---|---|---|
| Passive network observer | Between a client and the platform or an extender | Sees only transport TLS (§5.4). Even with transport TLS removed, sealed frames stay confidential (P1). |
| Active network attacker | Same | Transport TLS first (§5.4). Under the seal: cannot complete the identity proof, so no cipher is ever exposed to it (P2, P3). Can force plaintext only in Opportunistic mode (L2). |
| Relay or intermediary | Forwards frames (platform forwarders, TURN-style relays) | Sees the outer routing header and the plaintext surfaces in §3.8; cannot read or modify sealed frames (P1, P7). |
| Malicious peer | Is the other endpoint | The seal protects a pair's traffic from third parties, not from the peer itself. |
| The operator (platform) | Issues contracts and distributes identity keys | Not defended against first-contact key substitution (L1); see §4.3. |

### 2.3 Where a frame's source identity comes from

The receive loop reads the outer `TransferPath.SourceId` from the frame bytes
(`transfer.go:4651-4688`). connect does not authenticate it cryptographically,
and the route layer does not compare it with the transport that carried the
frame. The code expects the route to have done source checks
(`transfer.go:4647-4650`).

- For **sealed** frames, the AEAD key is bound to the peer's identity key by the
  identity proof (§3.4.3), and the sealed inner path must equal the outer path
  (`transfer.go:4766-4796`). A frame that decrypts under a session for peer *X*
  and carries *X*'s inner path is authenticated as *X*'s.
- For **plaintext** frames (control plane, `EncryptedControl` frames, acks,
  Opportunistic-mode data before sealing), source attribution is only as strong
  as the transport that delivered the frame.
  - Frames relayed by the platform are attributed by the platform (server side,
    outside this repository).
  - On a direct WebRTC P2P route, a frame that names the platform
    (`ControlId`) or this client as its source is dropped and counted
    (`ClientReceiveStats.ForgedSourceDropCount`, `transfer.go:4664-4680`). A
    peer route never carries either, and either would otherwise skip contract
    and session checks.
  - Other source ids on a P2P route are not compared with the peer at the far
    end. A multihop stream endpoint knows only its adjacent hop, while
    forwarded frames name the stream's origin (§5.4, L7).

---

## 3. WP2 — the encryption seal

### 3.1 Policy modes and defaults

| Mode | Behaviour | Evidence |
|---|---|---|
| `EncryptionModeOff` (zero value, library default) | Session manager inert; all I/O unsealed | `transfer_encrypt.go:470-476`, default at `:788` |
| `EncryptionModeOpportunistic` | Seal once a session is established; plaintext until then, or permanently if establishment never completes | `transfer_encrypt.go:477-483` |
| `EncryptionModeRequired` | Never send or accept plaintext application data for a peer for which a session is expected | `transfer_encrypt.go:484-497`; gates in §3.7 |

- The consumer "Post Quantum Encryption" performance profile sets Required for
  every window client: `ip_remote_multi_client.go:17186-17197`.
- The provider side runs Opportunistic so it can keep serving consumers that do
  not seal (`DESIGNNOTES2.md` §8; set in the SDK repository).
- `OpportunisticEstablishHold` (`transfer_encrypt.go:590-607`, default 0) can
  hold Opportunistic sends while a first handshake is in flight. It narrows the
  plaintext window; it does not close it.
- Control-plane and self traffic never has a session in any mode
  (`transfer_encrypt.go:3663-3686`; local guards at `transfer.go:7456-7472` and
  `transfer.go:16305-16316`).

### 3.2 Key agreement: TLS 1.3 inside the transfer protocol

**TLS configuration (defaults)**

| Setting | Value | Evidence |
|---|---|---|
| Versions | TLS 1.3 only (`MinVersion = VersionTLS13`) in both roles | `transfer_encrypt.go:408`, `:428` |
| Groups | `[X25519MLKEM768, X25519]`, hybrid first | `transfer_encrypt.go:382-385`, `:410`, `:431` |
| ALPN | `urnetwork/sequence` | `transfer_encrypt.go:409`, `:430` |
| Client verification of the server cert | `InsecureSkipVerify: true` (by design, see below) | `transfer_encrypt.go:427` |
| Server verification of the client cert | `ClientAuth = RequireAnyClientCert`, so both sides present certificates | `transfer_encrypt.go:407`; the client presents the same self cert, `:3395-3407` |
| Resumption | Server `SessionTicketsDisabled: true`; the client config has no session cache, so there is no PSK resumption and no 0-RTT | `transfer_encrypt.go:411-415` |
| Certificate | Ephemeral self-signed ECDSA P-256; 64-bit serial from `crypto/rand`; validity −1 h … +24 h | `transfer_encrypt.go:435-466` |
| Persisted certificate | When reloaded from PEM, the same TLS 1.3 / groups / no-tickets settings are applied | `transfer_encrypt.go:4147-4185` |

**Why `InsecureSkipVerify` is acceptable here.** X.509 path validation is not
the trust mechanism. Peer authentication is (a) an exact-DER match of the peer's
leaf against a certificate the peer signed with its Ed25519 identity key
(§3.4.2), and (b) the identity proof over the TLS exporter (§3.4.3), which is
the binding property. Certificate validity dates, KeyUsage and EKU are not
enforced by either side (§4.8.6).

**Caller overrides.** `EncryptionSettings.ClientTlsConfig` and
`ServerTlsConfig` replace the defaults as given (`transfer_encrypt.go:4126-4131`,
`:4148-4151`). They are embedding-application settings, not peer-controlled
input; connect's own call sites use the defaults.

**Carrying the handshake.** The TLS state machine runs over an in-memory
`net.Conn` (`sequenceTlsTransport`, `transfer_encrypt.go:158-275`). Bytes the
state machine writes are drained by `outboxLoop` (`:1626-1682`) into
`EncryptedControl{Handshake}` messages. They are queued through
`SendBuffer.SendEncryptedControl` (`transfer.go:6554`) with
`ForceUnwrapped: true` (`transfer.go:6624`), so every send and resend of a
handshake frame stays plaintext (`DESIGNNOTES.md` §3.5).

Inbound handshake frames are delivered in two ways:

- **In order:** `ReceiveSequence.deliverEncryptedControlFrames`
  (`transfer.go:18396-18425`) → `EncryptionSessionManager.DeliverEncryptedControl`
  → `deliverHandshake` (`transfer_encrypt.go:2380-2445`).
- **Optimistically** from the receive loop (`transfer.go:4953-5013`), but only
  for a TLS-server session in the window between sending its flight and
  receiving the client `Finished`, only for record types 20 or 23 (never a
  ClientHello), and only for the exact epoch id
  (`transfer_encrypt.go:2603-2664`).

**Roles.** Roles follow direction, not client id. The TLS client is the side
whose send sequence created the session; only the client role starts or restarts
a handshake (`transfer_encrypt.go:1376-1400`, `:3724-3758`). The server role
follows the peer's ClientHello; a ClientHello arriving after completion starts a
new epoch (`:2404-2413`). The ClientHello test is structural: record type 22 and
handshake type 1 (`:1700-1708`).

**What the TLS record layer protects.** Only the handshake itself.
Post-handshake, nothing is written to `tls.Conn`. `closeTls` deliberately does
not send `close_notify` (`transfer_encrypt.go:2996-3005`).

### 3.3 Record protection (the AEAD)

| Item | Implementation | Evidence |
|---|---|---|
| Key | `ExportKeyingMaterial("urnetwork-connect-aead", nil, 32)`; requires `HandshakeComplete` | `transfer_encrypt.go:89-92`, `:284-292` |
| Cipher | `aes.NewCipher` (32-byte key → AES-256) + `cipher.NewGCM` (12-byte nonce, 16-byte tag) | `transfer_encrypt.go:293-301` |
| Nonce | 12 bytes from `crypto/rand.Read` per frame, prefixed to the ciphertext | `transfer_encrypt.go:304-311` (`Seal`), `:338-362` (`SealOuterFrame`) |
| AAD | None (`nil`). The routing header is bound by the inner-path check below. | `transfer_encrypt.go:311`, `:356` |
| Plaintext | The entire inner `TransferFrame`: path, pack or ack, sequence id/number, contract frame and message frames | `DESIGNNOTES.md` §3.2 |
| Wire | Outer `TransferFrame{transfer_path (clear), encryptedTransferFrame = nonce ‖ ct ‖ tag, session_role?, session_companion?}` | `protocol/transfer.proto:37-60` |
| Open | Rejects input shorter than nonce + tag; GCM verifies before any plaintext is returned; the pooled buffer is released on failure | `transfer_encrypt.go:314-331` |

**Receive side.** `Client.unwrapFrame` (`transfer.go:2487-2522`) chooses the
candidate local sessions from the unauthenticated `session_role` and
`session_companion` hints. Those hints only select which keys to try; a wrong
hint simply fails authentication. Each candidate's `[established, prior]`
ciphers are tried (`transfer_encrypt.go:2922-2938`). The call is wait-free; on
failure the frame is dropped (`transfer.go:4740-4763`).

**Inner/outer path check.** After decryption the inner frame is decoded, and its
`TransferPath` (source, destination and stream) must equal the outer path. A
mismatch drops the frame and records a bad message in the peer audit
(`transfer.go:4766-4796`). This defeats re-routing and reflection of a sealed
frame under a modified outer header.

**Acks.** Acks are sealed with the receive sequence's session when the acked
data arrived sealed (`transfer.go:16984-17006`); see §3.8 for plaintext acks.

### 3.4 Peer identity binding

#### 3.4.1 The identity key

- Long-lived **Ed25519** keypair in `ClientKeyManager`
  (`transfer_key.go:68-178`). The key comes from
  `ClientSettings.ClientKeySeed` (32 bytes) or
  `ed25519.GenerateKey(crypto/rand.Reader)` (`:124-147`). A seed of the wrong
  size is a hard construction error, so a corrupted store cannot silently fork
  a new identity (`:131-141`).
- The private key never leaves the process. `Seed()` exists only so the
  embedding app can persist it (`:193-205`). `SetSeed` rotates the key and
  republishes it (`:228-271`).
- The public key is published to the platform with a `ClientKey` control
  message (`:368-416`). The `SkipClientKeyPublication` opt-out is ignored
  whenever any reader of the key exists, including any encryption mode other
  than Off (`:100-117`).

#### 3.4.2 Defence 1 — certificate binding

- The manager signs its PEM certificate chain with the identity key and
  publishes `(chain, signature)` in `EncryptedKey`
  (`transfer_encrypt.go:3437-3479`). The signed bytes are the PEM blocks
  concatenated in order (`transfer_key.go:321-345`).
- Contracts carry `destination_client_public_key`, `provide_tls_certificate` and
  `destination_client_key_signed_tls_certificate`. The values inside the
  platform-HMAC'd `StoredContract` are preferred; the outer `Contract` copy is
  used only when the stored value is empty (`transfer.go:19056-19076`). Both
  are passed to the session on every contract (`transfer.go:11957-11979`).
- `AddTrustedPeerCertChain` admits a chain only if its signature verifies under
  the committed peer key; otherwise the chain is dropped and logged
  (`transfer_encrypt.go:3064-3135`).
- Before the first sealed write on a non-companion send sequence,
  `verifyPeerCertAgainstContract` requires the established epoch's peer leaf to
  match a trusted certificate by exact DER
  (`transfer.go:14687-14761`, `transfer_encrypt.go:4310-4347`). An empty
  trusted set skips the check without latching. This is deliberate; the
  identity proof below is the binding control (`DESIGNNOTES.md` §3.6).

#### 3.4.3 Defence 2 — the identity proof (the binding property)

- When the handshake completes, the epoch derives the AEAD and a second
  exporter value, `ExportKeyingMaterial("urnetwork-connect-identity-proof", nil,
  32)`. Both are held privately on the epoch (`transfer_encrypt.go:1823-1860`,
  fields `:940-968`).
- Each side signs that 32-byte exporter with its identity key and sends it as
  `EncryptedControl{IdentityProof}` (`:1941-1982`). The proof is resent every 2 s,
  doubling, for the establishment window (`:1984-2047`).
- Verification is `VerifyClientKeySignature(peerPub, exporter, sig)`, which
  checks the key and signature lengths and then calls `ed25519.Verify`
  (`transfer_key.go:347-359`, call at `transfer_encrypt.go:2095`).
- Receive rules (`transfer_encrypt.go:2463-2564`):
  - A proof for an **older** epoch is ignored; a proof for a **newer** epoch
    triggers convergence (`:2482-2504`).
  - There is a single pending-proof slot; a second proof is ignored (`:2526-2528`).
  - A proof of the wrong length is a **terminal** failure for the epoch
    (`:2529-2541`).
  - A proof that fails verification is a **terminal** failure for the epoch
    (`:2118-2132`).
  - An identity-proof **timeout** is not terminal. A late proof that verifies
    against the same exporter heals the epoch (`:2512-2525`).
- Because the exporter is derived from the full TLS transcript, an attacker that
  terminates TLS on each leg gets two different exporters and cannot produce
  either peer's signature over either one (`transfer_key.go:53-67`).

#### 3.4.4 Where the peer's key comes from

- **From contracts** (`transfer.go:11972-11975`), stored value preferred (§3.4.2).
  Contracts reach the sender from the platform API: `POST
  {apiUrl}/connect/control`, authenticated with the client JWT, over the pinned
  platform TLS (`transfer_contract_manager.go:1984`, `api.go:508`; §5.3, §5.4).
  The sender cannot verify the contract MAC, so the key is exactly as
  trustworthy as the platform API.
- **From the out-of-band API** only when no contract has supplied a key
  (certless or contract-free relationships). The same trust root applies
  (`transfer_encrypt.go:4564-4631`).
- **First write wins:** a different key arriving later in the session is logged
  and ignored (`transfer_encrypt.go:2179-2222`).
- **Cross-check:** when a fetcher is configured, the contract key is compared
  with `/key/<peerId>`. A mismatch is **logged only**; the contract key is still
  used (`transfer_encrypt.go:2228-2241`, `:2250-2292`). Because both values come
  from the same operator, this check only catches an *inconsistent* platform
  (`DESIGNNOTES2.md` §3.2).

#### 3.4.5 Signed client-key history (Required only)

Implemented in `transfer_key_history.go` (verifier) and
`transfer_key_history_session.go` (policy). Design: `DESIGNNOTES3.md`.

**Gate.**

- The gate is armed in the same critical section that commits the contract key,
  but only when the mode is Required and a history fetcher is configured
  (`transfer_encrypt.go:2204-2213`; `transfer_key_history_session.go:100-111`).
- While pending, rejected or store-unavailable, `Cipher()` returns nil
  (`transfer_encrypt.go:2784-2803`).

**Verifier** (`transfer_key_history.go`):

- Canonical JSON only: unknown fields, trailing values and non-canonical
  re-encodings are refused (`:355-378`).
- secp256k1 recoverable signatures in canonical low-s form only: `r, s ∈ [1,
  n-1]`, `s ≤ n/2`, `v ∈ {0, 1}`. The address is the last 20 bytes of the
  Keccak-256 of the uncompressed key (`:270-304`).
- The chain must start at generation 1 and be contiguous. Each link checks the
  predecessor's content hash (not its signing digest), the same domain, client
  and network, a genuine change, and a non-regressing effective boundary
  (`:321-353`).
- The history must name the requested client (`:446-449`). Every generation's
  signer must be authorised by the pinned set or the peer pin (`:491-508`). A
  tombstoned head is rejected (`:468-474`). The history length is bounded by
  `MaxClientKeyHistoryGenerations` (default 64, `transfer_encrypt.go:803-809`;
  check at `:430-433`).
- Against a pin, the chain must reach the pinned generation, and that
  generation must still name the pinned key (`:510-533`). A rotation appends
  generations; a chain that rewrites the pinned generation is refused even
  when it also advances past it.

**Decision table** (`transfer_key_history_session.go:205-276`, fetch errors
`:175-203`):

| Evidence | Outcome |
|---|---|
| Verified chain, and its head key equals the contract key | Commit, pin, open the gate |
| Verified chain, but its head key differs from the contract key | **Terminal reject** |
| Chain does not verify | **Terminal reject** |
| No history, and the peer was previously pinned | **Terminal reject** (per-peer ratchet) |
| No history, and the operator has served signed history before | **Terminal reject** (per-operator ratchet) |
| No history, and nothing has ever been pinned | Accept (unsigned path) |
| Fetch **error**, peer pinned, contract key differs from the pin | **Terminal reject**: a pinned key may change only through a signed rotation, which an unavailable history cannot show |
| Fetch **error**, otherwise | Accept the contract key (availability, `DESIGNNOTES3.md` §5.3) — unless the checked pin store itself errors, which is terminal |

A terminal rejection cancels the epoch and emits
`EncryptionEventKeyIdentityRejected` (`:280-319`, `:332-337`).

**Limits we want reviewed (L1, L3):**

- The scheme is enforced only under Required (`:100-111`).
- With an empty `TrustedClientKeySigners`, the first verified chain is trusted
  on first use (`transfer_key_history.go:502-505`).
- An unpinned peer's contract key is accepted when the history fetch errors.
- A pin records the key at one generation, not the content hash of the pinned
  record. A chain that keeps the pinned key but rewrites other fields of the
  pinned or earlier generations passes.
- A single operator that is malicious from first contact signs one coherent
  chain and is not detected (`DESIGNNOTES3.md` §8).

### 3.5 The cipher-exposure gate

`Cipher()` (`transfer_encrypt.go:2762-2841`) returns a non-nil AEAD only when all
of the following hold:

1. The signed-history state is not pending, rejected or store-unavailable
   (`:2795-2803`).
2. An **established epoch** exists (`:2804-2827`). An epoch is promoted to
   established at exactly one place: successful verification of the peer's
   identity proof against that epoch, and only if it is still the current epoch
   (`:2100-2117` → `markEstablishedWithLock`, `:2940-2956`). Promotion therefore
   implies all of:
   - the TLS handshake completed (the exporter exists),
   - the peer key is known (32 bytes),
   - the proof verified.

The derived AEAD is held on the epoch (`derivedTlsCipher`, `:940-945`) and is
not reachable through `Cipher()` before promotion. If the server role has no
certificate, the epoch fails closed (`:1514-1525`). Test:
`TestPeerSessionCipherGatedOnIdentityVerified`.

### 3.6 Session lifecycle, epochs and rekey

- Sessions are keyed by `(peerId, companion, role)` (`transfer_encrypt.go:146-156`)
  and reference-counted by the sequences that use them (`:3688-3722`).
  Unreferenced sessions are reaped after `IdleTimeout`; the client default is
  `max(send idle, receive idle)` (`transfer.go:674-677`;
  `transfer_encrypt.go:1251-1322`).
- Each handshake runs in a resettable **epoch** (`:906-997`). The client role
  mints an epoch id; the responder adopts it (`:1499-1503`, `:1801-1816`).
  Controls for an older generation are dropped; a newer generation causes a
  reset (`:2380-2403`).
- **Rekey triggers:**
  - every new client-role send sequence (`restartHandshake`, `:1376-1400`);
  - an inbound ClientHello after completion (server role, `:2404-2413`);
  - unknown-wrap recovery (§3.9).
  
  There is no time- or volume-based trigger (§4.8.1).
- **Gap-free rekey:** the established epoch keeps serving `Cipher()` in both
  modes while a replacement handshakes (`:2828-2840`). The receiver keeps
  `[established, prior]` for decryption; an older prior is cancelled
  (`:2922-2956`). Test: `TestCipherContinuityDuringRekey`.
- **Bounded establishment:** `TlsTimeout` (default 60 s, `:796`) bounds the TLS
  handshake and the identity proof together (`:1543-1553`, `:1561-1624`).
  Initial failures back off exponentially with a deterministic per-peer stagger,
  capped at 5 min, with a saturating counter (`:827-904`, `:1414-1443`).

### 3.7 Fail-closed enforcement under Required

| Gate | What it does | Evidence |
|---|---|---|
| Send entry gate | Before a sequence number is assigned, an application pack waits for `Cipher() != nil` within the caller's budget. A budget of `0` or an exhausted budget refuses with `ErrEncryptionRequiredNotEstablished`: unsent, never plaintext. Handshake controls (`ForceUnwrapped`) pass. A parked sender nudges the client-role handshake to retry. | `transfer.go:7825-7947`; error `transfer_encrypt.go:500-508` |
| Write backstop | An application frame that reaches the writer without a cipher is refused, not written in plaintext | `transfer.go:14491-14504` |
| Contract-control pin | A contract-only open pack generated inside the sequence is pinned plaintext while the cipher is down, so establishment is never wedged behind it. A data-bearing head never gets this exemption. | `transfer.go:14478-14486`; `transfer_encrypt.go:2907-2920` |
| Receive gate (ack-and-discard) | Plaintext application frames from a session-expected peer are discarded after `EncryptedControl` frames are routed to the session. The item still advances and is acked; a peer-audit bad message is recorded. | `transfer.go:18252-18281` |
| Rekey continuity | No plaintext window during rekeys | `transfer_encrypt.go:2828-2840` |
| Observability | Edge-triggered `EncryptionEvent`s and `PeerEncryptionStates` (sealed, establishing, negotiated group, failure reason) | `transfer_encrypt.go:510-580`, `:4010-4116` |
| Candidate prefilter | Under Required, a provider for which the key API positively reports *no* key is rejected; fetch errors never reject | `ip_remote_multi_client.go:17360-17364` |

The ack-and-discard design and the head-of-line invariant it preserves are
explained in `DESIGNNOTES2.md` §8.1–§8.2.

### 3.8 What is intentionally not sealed

| Surface | Why | Evidence |
|---|---|---|
| Control-plane traffic to or from `ControlId` (the platform) | The platform terminates control traffic itself; no session is ever created for `Id{}` | `transfer_encrypt.go:3663-3686`, `:3698-3700` |
| `EncryptedControl` frames: handshake bytes, identity proofs, unknown-wrap nacks | They bootstrap the cipher; they are pinned plaintext on every send and resend | `transfer.go:6624`. The identity proof is a signature over an exporter value under a label distinct from the AEAD key's, so publishing it does not expose the AEAD key (§4.8.3). |
| Acks | An ack mirrors the sealed state of the data it acknowledges. The ack receive path accepts plaintext acks in every mode, so ack integrity is transport-level. A forged ack can cause loss or stall, not disclosure. | `transfer.go:16984-17006`, `:4882-4925` |
| Opportunistic-mode traffic before sealing | Policy (L2) | §3.1 |
| Outer header: `TransferPath` (source, destination, stream ids), `session_role` / `session_companion` hints | Needed for routing and session selection | `protocol/transfer.proto:37-60` |
| Sizes and timing | No padding: ciphertext length = plaintext length + 28 bytes | `transfer_encrypt.go:309`, `:344` |

### 3.9 Desync recovery: the unknown-wrap nack

- A receiver that cannot open a wrapped frame carrying a role hint sends
  `EncryptedControlUnknownWrapNack`, naming the epoch it can read. Emission is
  rate-limited to once per `UnknownWrapNackMinInterval` per session (default
  1 s) (`transfer_encrypt.go:4401-4477`, default `:817`).
- The sealing side acts only in the client role, only when it has an established
  epoch, only when the nack names *no* epoch, and at most once per
  `UnknownWrapNackRestartCooldown` (default 5 s). It demotes its epochs and
  re-handshakes (`:4479-4562`, default `:818`).
- In Opportunistic mode, a forged nack can open a plaintext window until
  re-establishment. This is documented at `:4486-4494`. In Required mode the
  cost is a rate-limited re-handshake.
- Tests: `TestForgedNackCausesNoOutage`, `TestNackDemoteChurnIsBounded`,
  `TestUnknownWrapNackEmissionIsRateLimited`, `TestNackStormDuringRecoveryConverges`,
  `TestNackWithStaleRealEpochDoesNotDemote`.

### 3.10 Test evidence for the seal

All 192 tests in the seal-related test files pass: **192 PASS, 0 FAIL, 0 SKIP**
(root package, macOS arm64, Go 1.27.3, 37.7 s on `03e98fe5`). They were run
again within the full-suite run on the fixed code (§10). The command is in §10.

| Property | Representative tests |
|---|---|
| Cipher gated on identity (P2) | `TestPeerSessionCipherGatedOnIdentityVerified`, `TestPeerSessionIdentityVerifiedOnProof` |
| Identity-proof validation | `TestPeerSessionIdentityProofValid`, `…Invalid`, `…Malformed`, `…SecondIgnored`, `…ArrivesBeforeKey`, `…ArrivesBeforeExporter`, `TestVerifyClientKeySignatureMalformedInput`, `TestSetPeerClientPublicKeyWrongSize` |
| Epoch scoping of controls | `TestIdentityProofForOlderEpochIgnored`, `TestIdentityProofForNewerEpochConverges`, `TestStaleEpochProofDoesNotConsumePendingSlot`, `TestHandshakeForOlderEpochDropped`, `TestHandshakeForNewerEpochResets`, `TestMalformedEncryptedControlEpochCannotActAsLegacy` |
| Optimistic path filter (P6) | `TestOptimisticallyDeliverHandshakeFilter`, `TestOptimisticHandshakeRejectsForeignWireGenerations`, `TestIsAwaitingClientFinished`, `TestIsAwaitingClientFinishedClientRole` |
| Required fail-closed (P4) | `TestRequiredEncryptionEstablishesAndDelivers`, `TestRequiredEncryptionFailsClosedAgainstPlaintextPeer`, `TestRequiredSendRefusalTypedErrorAndEvent`, `TestRequiredGateBoundedBudgetRefusesUnsent`, `TestRequiredGateNonBlockingSendRefusesPreCipher`, `TestRequiredContractFreeWithoutKeySourceFailsClosed`, `TestRequiredCipherWake*` (10 tests) |
| Rekey continuity | `TestCipherContinuityDuringRekey`, `TestContractDataHeadRetainsCipherDuringRekey`, `TestContractOpenDuringRekey*` |
| Post-quantum (P5) | `TestRequiredEncryptionEstablishesAndDelivers` (asserts `KeyExchange == tls.X25519MLKEM768`), `TestEndToEndHandshakeRecordsItsKeyExchange`, `TestServesPostQuantumEncryptionNeedsTheHybridGroup`, `TestPostQuantumKeyExchangeClassification`, `TestMultiClientChannelPqe` |
| Identity key | `TestClientKeyManagerLoadSeedWrongSize`, `TestClientKeyManagerCertChainSignature`, `TestCanonicalCertChainBytes`, `TestClientKeyPublicationSkipYieldsToEveryKeyReader` |
| Signed history: verifier | `TestForgedSignatureRejected`, `TestHighSSignatureRejected`, `TestSignatureRejectsNonCanonicalValues`, `TestSignatureAcceptsBoundaryLowS`, `TestBrokenChainLinkRejected`, `TestGenerationRollbackRejected`, `TestChainLinksOnContentHashNotDigest`, `TestDigestCoversEveryStructField`, `TestNonCanonicalEncodingRejected`, `TestHistoryBoundEnforced`, `TestUnknownSignerRejected`, `TestDomainMismatchRejected`, `TestPinRefusesForkAtTheSameGeneration`, `TestResignedHeadVerifiesAsChainAndIsCaughtElsewhere` |
| Signed history: policy | `TestSignedIdentitySubstitutionRejected`, `TestSignedIdentityDowngradeRejectedPerPeer`, `TestSignedIdentityDowngradeRejectedPerOperator`, `TestSignedIdentityFetchErrorFallsBack`, `TestSignedIdentityNotGatedUnderOpportunistic`, `TestSignedIdentityRejectionIsTerminal`, `TestSignedIdentityCheckedStoreFailureIsLocalAndTerminal` |
| Nack recovery | see §3.9 |

---

## 4. Seal threat analysis, by audit focus area

### 4.1 Downgrade

- **Policy downgrade.** In Opportunistic mode, a party that can drop, delay or
  corrupt `EncryptedControl` frames keeps the session cipher-nil, and traffic
  flows in plaintext (L2, `DESIGNNOTES2.md` §2 and §4). A forged unknown-wrap
  nack can reopen that window after sealing (§3.9). Required removes both
  (§3.7). Required is what the consumer post-quantum profile selects.
- **Protocol version.** TLS 1.3 only, in both roles (§3.2).
- **Key-exchange group.**
  - X25519MLKEM768 is offered first; X25519 is accepted for peers that do not
    offer the hybrid.
  - TLS 1.3 authenticates the negotiated group through the transcript, and the
    identity proof signs an exporter derived from that transcript. An active
    attacker therefore cannot steer two hybrid-capable peers to X25519 without
    breaking X25519 during the handshake.
  - There is no "post-quantum only" setting (`DESIGNNOTES2.md` §8.4). The
    negotiated group is exposed as `PeerEncryptionState.KeyExchange`
    (`transfer_encrypt.go:573-576`).
- **Identity-evidence downgrade.**
  - Signed → unsigned: refused per peer and per operator (§3.4.5).
  - Error-induced fallback: L3.
  - Empty certificate commitment: the certificate check is skipped, but the
    identity proof remains mandatory (§3.4.2).

### 4.2 Replay

- **The AEAD layer has no replay window**, because nonces are random. A replayed
  sealed frame from the current or prior epoch decrypts, and replay handling
  falls to the reliable sequence layer (§5.2):
  - Ack-mode packs are delivered at most once per sequence number while their
    `ReceiveSequence` is alive.
  - No-ack packs are not deduplicated.
  - After a `ReceiveSequence` is removed (idle 120 s / gap 60 s), a replayed
    head pack carrying its full contract frame can open a new sequence and be
    delivered again, within the contract's validity and while the epoch key is
    still held.
  
  Replays are confined to the original source/destination path by the sealed
  inner path (§3.3). This is limitation L17.
- Frames from other epochs or sessions fail authentication. Each `(peer, role,
  companion)` session and each epoch is a different TLS session with a
  different exporter, and only `[established, prior]` are retained
  (`transfer_encrypt.go:2922-2956`).
- Identity proofs are bound to one exporter. A proof replayed into another
  session does not verify; a proof for another generation is ignored or causes
  convergence, and never consumes the pending slot
  (`TestStaleEpochProofDoesNotConsumePendingSlot`).
- Replayed handshake controls for an older generation are dropped
  (`transfer_encrypt.go:2385-2399`). A replayed ClientHello after completion
  starts a new responder epoch, but the established epoch keeps serving
  (`:2404-2413`, `:2828-2840`); the cost is a handshake attempt that times out.
- Nack replay is rate-limited at both ends (§3.9).

### 4.3 Interception and MITM

- **Transport-level MITM** (no control of key distribution): it terminates TLS
  on each leg, gets two exporters, and cannot sign either → no cipher (P3). In
  Required mode the result is denial of service; in Opportunistic mode it can
  force plaintext (L2).
- **Certificate substitution alone:** defeated by the Ed25519 signature over the
  certificate chain (§3.4.2) and by the identity proof (§3.4.3).
- **Operator substitution** (key, certificate and signature in lockstep): passes
  every check today, because all checks root in the contract-supplied key
  (`DESIGNNOTES2.md` §3.1).
  - Under Required with signed history, substitution after first contact
    contradicts a signed, hash-chained record and is rejected and attributable.
  - First-contact substitution by a consistent operator is not detected (L1).
  - The planned direction is a plurality read across independent operators
    (`DESIGNNOTES2.md` status note, `DESIGNNOTES3.md` §10).

### 4.4 Timing and constant-time behaviour

- **AES-GCM** comes from Go's standard library
  (`crypto/internal/fips140/aes`).
  - Hardware AES is used on amd64 with AES-NI and arm64 with ARMv8 AES, as
    detected at runtime.
  - Otherwise the generic implementation uses T-tables (`aes_generic.go`) and
    is not constant-time with respect to cache timing. GHASH's generic path is
    constant-time (`gcm/ghash.go`).
  - The seal always uses AES-256-GCM, with no ChaCha20-Poly1305 alternative, so
    devices without AES instructions (32-bit ARM builds; ARMv8 cores without
    crypto extensions) use software AES (L6). Exploiting this needs code
    co-resident on the victim's CPU.
- **Ed25519** signing and verification, **ECDSA P-256** certificate keys and
  **ML-KEM-768**: Go standard-library implementations with constant-time
  arithmetic.
- **secp256k1** (decred `dcrec/secp256k1/v4`) is used only for public-key
  recovery over public data (`transfer_key_history.go:270-304`). There is no
  secret-dependent timing.
- **Comparisons in the seal** use `bytes.Equal` on public values only: public
  keys (`transfer_encrypt.go:2215`, `:2282`; `transfer_key_history_session.go:255`)
  and certificate DER (`transfer_encrypt.go:4333`). No secret value (AEAD key,
  exporter) is compared; the exporter is only signed, and signatures are only
  verified.
- **Trial decryption:** the number of AEAD attempts per frame depends on hints
  and local session state, not on secrets (§3.3).

### 4.5 Random number generation (seal)

- Every security value in the seal comes from `crypto/rand`:
  - AEAD nonces (`transfer_encrypt.go:306`, `:352`),
  - certificate private keys and serials (`:436-453`),
  - the Ed25519 identity key (`transfer_key.go:143`),
  - TLS internals.
- In Go 1.27, `crypto/rand.Read` never returns an error. It terminates the
  process if the OS source fails (`$GOROOT/src/crypto/rand/rand.go`), so a
  nonce can never be silently left zero.
- Protocol identifiers (`NewId`: sequence, message and handshake-epoch ids) are
  ULID-shaped: a 48-bit millisecond timestamp plus 80 bits from `math/rand/v2`
  (`id.go:9`, `:17-75`). Within one millisecond, consecutive ids differ by a
  random step of at most 2³² (`id.go:55`). They are routing discriminators, not
  secrets, and the seal does not rely on their unpredictability. Epoch ids are
  visible in plaintext `EncryptedControl` frames anyway.

**Repository-wide RNG inventory** (outside the seal).

`crypto/rand` is used for every key, secret and certificate:

- provide secret keys (`transfer_contract_manager.go:1361`, `:1533`);
- extender identity seeds and challenges (`extender_record.go:84`, `:154`);
- the directory partition secret (`net_extender_directory.go:610`);
- probe nonces (`net_extender_probe_latency.go:388`);
- unpredictable IP fragment ids (`ip_fragment.go:545`);
- the WebRTC DTLS certificate key (`transport_p2p_webrtc_pc.go:172`);
- extender certificates, lag-session ids and upload keys (`extender/`);
- the SCTP state cookie (`sctp/param_state_cookie.go:18`).

`math/rand` (v1 and v2 global sources: the runtime ChaCha8 generator,
auto-seeded, never re-seeded in this repository) is used for jitter, sampling,
shuffling and censorship-evasion shaping. Uses that touch the wire or a check:

| Use | Evidence | Note |
|---|---|---|
| All `Id`s, including the private-extender header nonce and P2P probe nonces | `id.go:49-55`; `net_extender.go:514`; `transport_p2p_probe.go:594-632` | Identifiers, not secrets |
| SCTP verification tag and initial TSN (in-repo fork of pion/sctp) | `sctp/association.go:32-37`, `:782`, `:5162` | `pion/randutil`, a `math/rand` generator seeded from `crypto/rand` with a time fallback |
| Probe SYN initial sequence number | `ip_remote_multi_client_probe.go:478` | Probe traffic |
| DNS pseudo-transport pump header (16 random bytes); DNS transaction id is a counter | `transport_pt.go:327-329`, `:399` | Transport framing |
| Which upload message is integrity-checked on CPUs without AES instructions (1 in 16) | `extender/extender_lag_api_stream.go:87`, `:125-128` | Extender measurement path |
| Spoof SNI choice, ClientHello fragmentation, padding lengths, uTLS fingerprint choice | `net_http.go:2743-2784`, `net_resilient.go:402-449`, `vless_vision.go:188-190`, `vless_reality.go:81` | Choices from public lists |

### 4.6 Side channels and information leakage

- **Metadata visible to on-path relays:** source, destination and stream ids;
  role and companion hints; frame sizes (no padding) and timing (§3.8).
- **`EncryptedControl` contents visible to relays:**
  - the ClientHello: key shares, plus the fixed ALPN `urnetwork/sequence` and
    SNI `urnetwork-sequence`;
  - identity-proof signatures;
  - nack epoch ids.
  
  TLS 1.3 encrypts the certificates during the handshake.
- Plaintext pooled buffers are not zeroed on return (`message_pool.go:272-275`,
  `:365`). Isolation between owners relies on the pool's ownership rules (§8).

### 4.7 DoS vectors in the seal

- **The receive loop never blocks on TLS state.**
  - Unwrap is wait-free (`transfer.go:4731-4763`).
  - Optimistic delivery only appends to an in-memory inbox, and only in the
    narrow server window (`transfer_encrypt.go:2958-2994`, `:2603-2664`).
  - The historical blocking wait was removed (`DESIGNNOTES.md` §3.5).
- **Bounded work per frame:** at most 4 sessions × 2 ciphers = 8 AEAD-open
  attempts for a frame with no hints (`transfer.go:2498-2520`).
- **Rate limits and single-flight:**
  - nack emission 1/s per session, restart cooldown 5 s (§3.9);
  - out-of-band key fetch and history fetch: single-flight, 1/s
    (`transfer_encrypt.go:4583-4605`; `transfer_key_history_session.go:120-140`);
  - initial-retry back-off saturates at 63 doublings and is capped at 5 min
    (`transfer_encrypt.go:1423-1430`, `:800`).
- **Bounded verification work:** at most 64 history generations
  (`transfer_encrypt.go:809`), and a per-record size bound
  (`MaxClientKeyRegistrationBytes`, `transfer_key_history.go:361`).
- **Not count-capped (for review):**
  - The number of sessions in a manager. Sessions are created on demand,
    including from inbound `EncryptedControl` frames and undecryptable wraps for
    the claimed source id (`transfer_encrypt.go:3790-3828`, `:4406-4419`);
    unreferenced sessions are reaped after `IdleTimeout`.
  - The per-epoch TLS inbox. It grows on delivery (`:189-197`), is drained by the
    TLS reader, and after completion only a ClientHello is accepted (`:2414-2421`).
- **CPU:** an ML-KEM-768 hybrid handshake is CPU-heavier than X25519. A peer can
  trigger responder handshakes by sending ClientHellos to its own sessions.

### 4.8 Cryptographic correctness notes

**4.8.1 Nonce budget (L4).**

- Nonces are random 96 bits per frame, and there is no per-key invocation
  counter.
- A key lives for one epoch. Epochs are replaced only by the triggers in §3.6;
  there is no time- or volume-based rekey.
- Both directions of one session use the same key: the initiator's data packs
  and the responder's acks.
- NIST SP 800-38D §8.3 limits random-IV GCM to 2³² invocations per key. At
  2³² frames the nonce-collision probability is about 2⁻³³. A long-lived,
  high-throughput sequence can approach that count: at roughly 90 k frames/s it
  takes about 13 h.

**4.8.2 Associated data.** AAD is empty. The routing header is protected
indirectly: the inner path must equal the outer path. The role and companion
hints are deliberately unauthenticated selectors (§3.3).

**4.8.3 Key separation.**

- The AEAD key and the identity-proof value come from distinct exporter labels
  (`transfer_encrypt.go:89-99`), which RFC 8446 §7.5 makes independent.
- Each `(peer, role, companion)` session and each epoch is a separate TLS
  session. The prior epoch is kept for decryption only.

**4.8.4 Ed25519 domain separation.** One identity key signs several message
kinds:

| Use | Signed bytes | Evidence |
|---|---|---|
| Identity proof | Raw 32-byte exporter output (no prefix) | `transfer_encrypt.go:1962`, `:2032` |
| Certificate binding | Concatenated PEM blocks (begin with `-----BEGIN`) | `transfer_key.go:325-345` |
| Original close reports, contract-creation originals, whole-work records | `<schema>\0` + fixed-width fields | `protocol/close_report_original.go:13-49`; `protocol/contract_creation_original.go:92-140`; `protocol/whole_work_*.go` |
| Provider extender-probe attestation | `ur-extender-probe-v1` domain + fields | `net_extender_probe_latency.go:424-454` |

The identity proof carries no context string. It is distinguishable from every
other kind by length and structure: the others are prefixed and longer than
32 bytes. Its inputs are uniformly random exporter outputs that an attacker
cannot choose.

**4.8.5 Proof symmetry.**

- Both peers sign the same exporter value. A peer's own proof reflected back to
  it fails, because it is verified under the other party's key.
- Neither role nor peer ids are signed. Binding relies on the exporter being
  unique to the TLS session and on each side verifying under the key it expects
  for that peer.

**4.8.6 X.509 is not a trust input.**

- Validity dates, KeyUsage and EKU are not checked (§3.2).
- Certificates are matched by exact DER, and only after their Ed25519 binding
  verifies.

**4.8.7 Post-quantum.**

- X25519MLKEM768 comes from Go's `crypto/tls`. Classical X25519 remains accepted
  (§4.1).
- `ServesPostQuantumEncryption` advertises the capability only when the hybrid
  is offered (`transfer_encrypt.go:3637-3661`).

**4.8.8 Out of the production path.**

- `stream_fast_path.go`: counter-nonce stream AEAD with HKDF-derived directional
  keys and a replay window. It is a prototype referenced only by its own tests.
- `vless_reality.go` uses an AEAD for the REALITY session id. That belongs to the
  extender transport (§5.4), not the seal.

### 4.9 Comment drift (to save review time)

| Location | Comment says | Code does |
|---|---|---|
| `transfer_encrypt.go:31-47` (file header), `:3395-3399` | Roles assigned by lexicographic client id; one session per destination mask | Roles follow direction; sessions are keyed `(peer, companion, role)` (§3.2, §3.6) |
| `transfer_encrypt.go:947-948`, `transfer_key.go:53-57` | Identity-proof label `urnetwork-sequence-identity-proof` | Constant is `urnetwork-connect-identity-proof` (`transfer_encrypt.go:97`) |
| `transfer_encrypt.go:614-623` | `TlsTimeout` "negative value (the default) disables" | Default is 60 s (`:796`) |
| `transfer_encrypt.go:390-399` | Mentions lexicographic roles for mTLS motivation | mTLS is still required; the motivation is now roles by direction |
| `DESIGNNOTES2.md` §1, `DESIGNNOTES3.md` | Line numbers | Superseded by this document's references |

---

## 5. WP1 — protocol and data plane

### 5.1 Framing and decoding of untrusted input

**Byte-stream framers** check the length before allocating:

- 16-bit framer: `message_framer.go:83-103`;
- 32-bit framer: `message_framer_xl.go:94-102`, with settings validated at `:72-77`;
- H1 framed connection: `framed_message_conn.go:128-190`.

Limits are listed in §8.4.

**Routing decode.** `FilteredTransferPath` (`frame_protobuf.go:1860-2012`) is a
hand-rolled, allocation-free reader of the outer path.

- Every length is checked before it is used (`:1875`, `:1904-1907`,
  `:1941-1944`, `:1966-1974`).
- It is more lenient than protowire in two ways: it ignores the high bits of a
  10th varint byte, and it does not reject field number 0. Groups fall back to
  `proto.Unmarshal` (`:2005-2012`).

**Frame decode.**

- `unmarshalOwnedTransferFrame` (`frame_protobuf.go:1680` onward) uses protowire
  and checks for errors at every step.
- Ids must be exactly 16 bytes (`:765-767`).
- The outer path is skipped. The inner (sealed) path is decoded only for sealed
  frames (`transfer.go:4696`, `:4769`).
- Inner `Pack`/`Ack` bodies use `proto.Unmarshal`, which copies byte fields
  (`message_pool.go:1395-1397`).
- A `TransferFrame` must carry exactly one of pack or ack (`transfer.go:4867-4880`).
- Some narrowing conversions are validated afterwards: logical lane ≤ 8
  (`transfer.go:15569`) and session role (`transfer_encrypt.go:135-143`).

**Robustness tests** (all pass on this commit):

- `TestDecodeTransferFrameRandomized`, `…Malformed`, `…EdgeCases`,
  `TestDecodeRoundTripFromMarshal`;
- `TestFrameCodecRandomized`, `TestAckCodecRandomized`;
- `TestEncryptedOuterFrameCodec`, `TestSequenceCipherSealOuterFrame`.

There is no coverage-guided fuzz target for the transfer-frame decoder yet;
the fuzz targets cover the IP data plane and the H3 datagram reassembler (§5.5).

**Panic containment.** A panic on the receive loop cancels the client (§5.7), so
any reachable panic in decoding or dispatch is a client-wide denial of service.
Inputs that reach the loop's own assertions are handled without one:

- A sealed inner frame without a transfer path fails the inner/outer path check
  and is dropped and audited. `TransferPathFromProtobuf` refuses a missing path
  (`connect.go:81-96`, used at `transfer.go:4783`). Test:
  `TestSealedFrameWithoutInnerPathDoesNotStopTheClient`.
- In `ReceiveBuffer.Pack`, a pack that reuses a live sequence id under another
  lane (force-stream / companion-contract flags) is dropped
  (`transfer.go:15632-15648`). A stale head index naming the pack's own id is
  healed (`transfer.go:15694-15701`). Tests:
  `TestPackOfAnotherLaneForAClosingSequenceDoesNotStopTheClient`,
  `TestReceiveBufferHealsAStaleHeadIndexForThePacksOwnId`
  (`transfer_receive_loop_crash_test.go`).

### 5.2 Reliable sequences, acks and replay

**Sequence numbers and dedupe.**

- Sequence numbers are `uint64` (`protocol/transfer.proto:95`).
- A head pack may advance the receiver's next sequence number forward, for
  receiver reform (`transfer.go:17780-17789`).
- Within a live `ReceiveSequence`:
  - an ack-mode pack below the next sequence number is dropped and re-acked
    (`transfer.go:17851-17855`);
  - the next number is delivered exactly once (`:17816-17818`);
  - later numbers are held until the gap fills.
- The guarantee is therefore **at-most-once per sequence number for the lifetime
  of that `ReceiveSequence`**.

**Where replay is still possible** (relevant to the seal, whose AEAD has no
replay window, §4.2):

- **No-ack packs are not deduplicated.** Each copy is delivered if its contract
  has room (`ReceiveSequence.receiveNack`, `transfer.go:18096-18192`).
- **After a `ReceiveSequence` is removed** (idle 120 s, gap 60 s:
  `transfer.go:1066-1071`), a later pack for the same sequence id creates a new
  sequence (`transfer.go:15754-15773`).
  - Only sequences that failed contract checks are tombstoned, in a FIFO capped
    at 1024 (`transfer.go:15392`, `:15522-15534`, `:15544-15552`).
  - A replayed head pack with its full contract frame passes the MAC, path and
    expiry checks again and is delivered; the contract is debited from zero in
    the new sequence.
  - Contract expiry is optional (`transfer_contract_expiration.go:15`).
- **Limits on replay:**
  - a sealed replay must still decrypt under the established or prior epoch
    (§3.6);
  - the sealed inner path pins source and destination (§3.3);
  - a newer live sequence for the same head key drops older ids
    (`transfer.go:15676-15681`).

**Acks.**

- An ack is routed to the send sequence by sequence id and must come from that
  sequence's destination (`transfer.go:6681-6682`, `:8264`).
- An ack for a message id that is not retained never advances delivery
  (`transfer_ack_identity.go:39`).
- Acks are cumulative (`transfer.go:13966-14057`).
- Plaintext acks are accepted in every encryption mode (§3.8).

### 5.3 Contracts: authorization and accounting

**Model.** A contract authorizes one source to send to one destination under one
provide mode, up to a byte count, optionally until an expiry.

- The platform issues it. It is MACed with the **provider's** per-mode provide
  secret key: 32 bytes from `crypto/rand`
  (`transfer_contract_manager.go:1352-1367`, `:1522-1537`), registered with the
  platform in a `ProvideKey` control message.
- The key is therefore shared between the provider and the platform.

**Provider-side verification** happens for every contract frame that reaches
the delivery head (`ReceiveSequence.registerContracts`, `transfer.go:18428-18504`):

1. `ContractManager.Verify` (`transfer_contract_manager.go:1601-1626`).
   - It selects the key by the wrapper's `provide_mode`. The mode is bound
     because each mode has its own key.
   - A paused provider accepts only Stream and Network contracts (`:1605-1609`).
   - `VerifyStoredContract` (`transfer_contract_manager.go:302-328`) compares
     with `hmac.Equal` (`:316`, `:324`).
   - The standard tag `HMAC-SHA256(K, stored_contract_bytes)` always verifies.
   - The legacy tag is the output of `mac.Sum(stored_contract_bytes)`, which is
     `stored_contract_bytes ‖ HMAC-SHA256(K, "")` (signer at `:282-300`). Its
     only key-dependent part does not depend on the contract.
     - It verifies only before `NetworkEventTimeChangeHmac`, the instant
       signers stopped emitting it: 2026-09-01 by default (`:319`, `:342-350`).
     - With nil settings it never verifies.
   - Empty contract bytes never verify (`:311`), because their standard tag is
     that same key-only value.
   - Neither function writes into spare capacity behind the caller's bytes.
   - Regression tests (`transfer_contract_hmac_test.go`):
     `TestLegacyContractHmacForgeryRejectedAfterCutover`,
     `TestEmptyStoredContractBytesNeverVerify`,
     `TestStoredContractHmacLeavesCallerBytesUntouched`,
     `TestContractManagerRefusesForgedLegacyContract`.
2. The stored path must be `(sequence source, local client)`
   (`transfer.go:18487-18494`).
3. The contract must not have expired (`transfer.go:18495-18504`).

On failure: the sequence exits, retransmits of that sequence id are rejected
(tombstoned), and a bad contract is recorded in the peer audit
(`transfer.go:18446-18494`).

**Sender-side trust.**

- Senders obtain contracts from the platform API (`ClientOob().SendControl` →
  `POST {apiUrl}/connect/control` with the client JWT, over pinned TLS):
  `transfer_contract_manager.go:1984`, `api.go:508`. The in-band receive path is
  disabled (`transfer_contract_manager.go:1183-1192`).
- The sender cannot verify the MAC, because the key is the provider's. It checks
  only that the contract parses and names it as the source
  (`transfer_contract_manager.go:1313-1315`, `:1845`).
- Identity material in the contract (§3.4.2) is therefore exactly as trustworthy
  as the platform API.

**Accounting and numeric handling.**

- `ByteCount = int64` (`connect.go:319`), but the wire field is `uint64`
  (`protocol/transfer.proto:595`).
- Conversions:
  - The signed size is converted without a range check
    (`transfer.go:19093`).
  - The admission bound is `ByteCount(float32(size) * fillFraction)`
    (`transfer.go:19094`). The fill fraction is 1.0 on receive
    (`transfer.go:18476`) and 0.9 on send (`transfer.go:994`).
  - float32 has a 24-bit significand, so sizes above 16 MiB round by up to a
    relative 2⁻²⁴. The default contract sizes, 1 MiB and 128 MiB, are exact.
- Receive-side check: `acked + unacked + max(minUpdate, n) <= effective`
  (`transfer.go:19113-19115`). It counts message-frame bytes per pack (minimum 1
  byte, `transfer.go:1098`) and debits at the delivery head.
- Underflow of the unacked counter panics, recovered per sequence
  (`transfer.go:19198-19200`).
- Counters are per sequence; there is no cross-sequence ledger on the receive
  side (see the replay note in §5.2).
- Other floats (`transfer_contract_retry.go:42`, pacing, scoring) do not gate
  authorization.

### 5.4 Transports and transport security

**Platform TLS (WebSocket/H1, H1+, API, H3).**

- `DefaultTlsConfig` trusts **only the embedded Let's Encrypt roots**, ISRG Root
  X1 and X2 (`net_tls.go:58-64`, `:166-197`), plus an optional file of extra
  roots named by `URNETWORK_CONNECT_EXTRA_ROOT_CA_FILE` (`net_tls.go:28`,
  `:133-155`). There is no SPKI or leaf pinning; standard chain and hostname
  verification apply.
- `MinVersion` is TLS 1.2. A shared session-ticket cache enables resumption
  (`net_tls.go:172-196`).
- The default ClientHello is uTLS Chrome 133 (`net_tls_hello.go:64`). It is
  used only when the config allows TLS 1.2–1.3 and has no custom suites or
  groups (`:75-90`), and it offers TLS 1.3 and 1.2.
- QUIC is TLS 1.3 only (enforced by quic-go). QUIC versions are v2 then v1
  (`net_quic_version.go:50-58`).
- Where the pinned roots do **not** apply:
  - DoH resolvers, extender seed mirrors and the work-capture uploader (system
    roots);
  - js/wasm builds, where the browser owns TLS (`net_http_platform_js.go:12-19`).

**Platform authentication.** A bearer JWT, with no client certificates.

- H1: `Authorization: Bearer` on the upgrade request (`transport.go:2515-2529`).
- H3: the first framed message on the first stream, echoed back by the server
  (`transport.go:3575-3587`, `:3688-3699`).
- API: an `Authorization` header (`net_http.go:3391-3393`).
- The client parses its own JWT **unverified**, only to read its `client_id`
  (`jwt.go:16-52`).
- URL schemes are not enforced. `ws`/`http` are accepted if configured
  (`http_upgrade.go:223-231`), and the defaults are `wss`/`https`.

**H3 early data.** On builds without H3 retained-byte accounting (desktop and
server), the H3 dial uses `DialEarly` (`transport_family.go:2054-2062`). The
single-candidate path does not wait for handshake confirmation
(`transport.go:3624-3625`) before writing the auth message carrying the JWT
(`transport.go:3650-3676`). With a resumable ticket that allows it, that message
can therefore travel as QUIC 0-RTT data. Whether the platform accepts 0-RTT is
decided server-side, outside this repository. Mobile builds use `Dial`.

**Downgrade between transports.**

- Mode preference is H1 < H3 < H3Dns < H3DnsPump (`transport.go:170-178`,
  `:2274-2298`). Every mode authenticates the platform over the pinned TLS
  above, and there is no plaintext mode.
- H1+ falls back to a normally validated WebSocket only on capability failure,
  never on an authentication or TLS failure (`http_upgrade.go:393-397`,
  `:430-436`).
- DNS falls back from DoH to plain DNS only after every encrypted resolver has
  failed (`net_http_doh.go:140-145`, `net_http_internal_doh.go:67-72`).

**Relays.** All carry the pinned platform TLS *inside* them.

- **Extender:**
  - The inner TLS runs end to end to the platform (`net_extender.go:350-374`).
  - The outer TLS uses `InsecureSkipVerify` with TLS 1.3, plus an Ed25519
    leaf-signature check against the extender key (`net_extender.go:423-459`).
    Extender keys come from root-signed records, and the root keys come from
    `GET {apiUrl}/hello` over pinned TLS (`extender_record.go:185`, `:319`;
    `net_extender_network.go:836-866`).
  - A manually configured extender without a key has an unauthenticated outer
    layer (`net_extender.go:164-166`).
  - Private extenders admit a client only with an HMAC header
    (`extender/extender.go:1457-1479`, §5.7).
- **VLESS/REALITY:**
  - The inner platform TLS is pinned (`vless_dial.go:128-146`).
  - The outer TLS honours the link's `allowInsecure` (`vless.go:417`). The uTLS
    presets it can select may offer TLS 1.0–1.2 (`vless_dial.go:201-205`).
  - REALITY requires the server to prove the configured key with an HMAC-SHA512
    over the leaf key (`vless_reality.go:126-134`, `:246-249`).

**WebRTC P2P.**

- Pion DTLS with STUN only; no TURN servers are configured
  (`transport_p2p_webrtc_stun.go:25-41`). mDNS is disabled, and SCTP
  zero-checksum is on, relying on DTLS integrity
  (`transport_p2p_webrtc_pc.go:126-141`).
- The remote DTLS fingerprint comes from SDP carried in `ExchangeSignals`
  frames over the transfer path (`transport_p2p_webrtc.go:214-216`,
  `:3809-3815`). Pion verifies the remote certificate against that fingerprint.
  There is no separate binding of the DTLS certificate to the client identity
  key.
- The send side selects the P2P route by destination peer
  (`transport_p2p.go:1734-1746`).
- The receive side delivers frames into the client's receive loop
  (`P2pReceiveTransport.MatchesReceive`, `transport_p2p.go:2345-2347`). There,
  a P2P-route frame naming the platform or this client is dropped. Other source
  ids are not compared with the far-end peer, because a multihop endpoint knows
  only its adjacent hop (§2.3).

### 5.5 IP/TUN data plane

The data-plane security policy is specified in `docs/IP_SECURITY.md`. That
document is authoritative, and its §2.6 file list is current.

**Egress order** (provider egress and client send) — `docs/IP_SECURITY.md`
§2.2:

1. Network-relationship bypass.
2. **Public-unicast check.** A destination that is private, loopback,
   link-local, multicast or unspecified, or one of the listed reserved IPv6
   prefixes, is an incident (`ip_security.go:886-905` and `isReservedIpv6`
   below it). A v4-mapped v6 destination is judged as its v4 address.
3. CFAA endpoint reputation: a generated, sorted, disjoint blocklist plus a
   port/protocol policy (`ip_security_cfaa.go`, `ip_security_cfaa_block.go`,
   generator `security/main.go`).
4. DMCA stateful payload inspection (`ip_security_dmca.go`).

**Which relationship applies.** Non-public destinations are reachable only when
**both** the packet source and the local client are in a `Network` relationship
(`egressRelationship`, `ip_security.go:378-390`). The source's provide mode comes
from its MAC-verified contract (§5.3).

**Per-source isolation at the provider.**
`RemoteUserNatProvider.clientReceiveAdmitted` does not treat control-plane
sources as IP traffic (`ip.go:10279`). Its state is keyed per source client
(`ip.go:10283-10288`).

**Fuzz targets:**

- `FuzzParseIpPathWithPayload`, `FuzzSecurityPolicyInspectEgress`
  (`ip_security_fuzz_test.go`);
- `FuzzAppStandardDetectors`, `FuzzWhatsAppNoiseDetector`,
  `FuzzEthereumDetectors`;
- `FuzzBlockHost`;
- `FuzzH3DatagramReassembler` (`transport_h3_datagram_test.go`).

For this document, `FuzzParseIpPathWithPayload`,
`FuzzSecurityPolicyInspectEgress` and `FuzzH3DatagramReassembler` were each run
for 30 s on the audited commit. No crashers were found; they executed about
136 k, 53 k and 127 k inputs respectively.

### 5.6 Control plane and no-contract peers

- `ControlId` (`Id{}`) is the platform. Traffic to and from it needs no contract
  and no session (`DESIGNNOTES.md` §3.9, §4.1).
  - Contract exemptions: `ContractManager.SendNoContract` and
    `ReceiveNoContract` (`transfer_contract_manager.go:1656-1684`).
  - Session exemptions: `SendNoSession` and `ReceiveNoSession`
    (`transfer_encrypt.go:3663-3686`).
- `AddNoContractPeer` extends the contract exemption at runtime
  (`transfer_contract_manager.go:1648-1654`).
- An item is admitted without a contract only when `ReceiveNoContract(source)`
  holds. Otherwise it must debit an open, unexpired contract
  (`ReceiveSequence.updateContract`, `transfer.go:18739-18765`). Items admitted
  without a contract are delivered with `ProvideMode_Network`
  (`transfer.go:18230-18247`).
- A frame addressed to another client is passed only to registered forward
  callbacks (`transfer.go:4431-4450`, `:5070-5091`). connect registers none
  itself: `AddForwardCallback` has no non-test caller in this repository.
- Network-mode `roles` and `principal` are read only from the MACed stored
  contract bytes (`transfer.go:19078-19084`). They are surfaced to receive
  callbacks as `Peer.Roles` / `Peer.Principal` (`transfer.go:18230-18239`).
  connect makes no authorization decision on them itself; the embedding code
  does.

### 5.7 Memory safety, constant-time comparisons and injection surfaces

**Memory safety.**

- connect is pure Go. There is no cgo (`import "C"` appears nowhere, including
  tests).
- The low-level surfaces are:
  - `unsafe.Sizeof` for memory accounting (many files, no pointer conversion);
  - OS calls through `unsafe.Pointer`:
    - Darwin `fsetxattr`/`getattrlist` through `//go:linkname syscallSyscall6
      syscall.syscall6` and assembly trampolines
      (`durable_attribute_darwin.go:27-47`, `durable_attribute_darwin_{amd64,arm64}.s`,
      `durablesys/durablesys_darwin*.go/.s`);
    - Windows `GetAdaptersAddresses` (`egress_resolver_windows.go:184`, `:201`)
      and `getsockopt` (`egress_windows.go:29-34`);
    - Linux `SO_MEMINFO` (`ip_udp_socket_drops_linux.go:29-38`);
    - Windows ICMP through iphlpapi (`ip_icmp_egress_windows.go:232-303`, `:454`).
      Here the reply payload slice is built from the OS-returned `data` pointer
      and `dataSize` (`:299`) without a check that they lie inside the reply
      buffer; the OS is trusted to return in-buffer values.
  - Generated protobuf code (`protocol/*.pb.go`).
- No `reflect` is used to bypass type safety.

**Memory corruption, Go-style.** The realistic class is buffer-ownership errors
in the shared message pool.

- Pooled buffers carry a reference count and an id. A negative count panics
  (`message_pool.go:325-365`).
- Payloads are not zeroed on return (`:272-275`, `:365`).
- The ownership rules are written down in `message_pool.go:26-38` and
  `README.md` ("Buffer reuse"). A use-after-return would show one owner's bytes
  to another; we consider this the highest-value memory-safety review target.
- `DebugTransferCopyOnWrite` exists as a diagnostic (`transfer.go:14521-14526`).

**Panics from malformed input.**

- The client's receive loop runs as `HandleError(client.run, cancel)`
  (`transfer.go:2424`). `HandleError` recovers the panic, logs it with a stack,
  and runs the handlers (`trace.go:58-97`).
- Session workers are wrapped the same way (`transfer_encrypt.go:1241-1247`).
- So a remotely triggerable panic cancels the whole client: a client-wide denial
  of service, not a process crash. Decoder robustness is therefore part of the
  DoS surface (§5.1).

**Constant-time comparisons across the repository.**

| Comparison | Primitive | Evidence |
|---|---|---|
| Stored-contract HMAC | `hmac.Equal` | `transfer_contract_manager.go:316`, `:324` |
| REALITY short id; REALITY certificate HMAC | `subtle.ConstantTimeCompare`; `hmac.Equal` | `extender_reality.go:196`, `:214`; `vless_reality.go:133` |
| Private-extender header HMAC | `hmac.Equal` | `extender/extender.go:1457-1479`; `extender/extender_secret_compare_test.go` |
| SCTP state cookie (upstream pion code) | `bytes.Equal` | `sctp/association.go:2352`, `:2356` |
| Platform auth echo; H3 datagram JWT echo | `bytes.Equal` / `!=` against the server's echo of the client's own token | `transport.go:2597`; `transport_h3_datagram.go:189` |

The seal compares only public values (§4.4).

**Injection-class surfaces: none in network-facing code.**

- `os/exec` appears only in the build-tagged `fingerprint/capture` tool
  (`//go:build fingerprint_capture`), with argv taken from CLI flags.
- No templates, SQL, `encoding/gob`, YAML, XML or plugin loading.
- File names in the contract stores are SHA-256 digests. They are validated and
  opened through `os.Root` without following symlinks
  (`transfer_contract_work_outbox.go:101-107`, `:503`;
  `transfer_contract_creation_store.go:265`, `:330`).
- Spoof-domain resources are read from `embed.FS` after the country code is
  normalized to `[a-z]{2}` (`net_extender_spoof.go:111-129`).
- JSON from the network is decoded into concrete structs. The exception is the
  signed key history, which uses canonical decoding with unknown fields
  rejected (§3.4.5).

## 6. Audit-scope cross-reference

| Cure53 scope item | Short answer | Sections |
|---|---|---|
| General injection / code execution (client and server) | connect has no interpreter, shell, SQL or template surface. Wire input is protobuf (`google.golang.org/protobuf`) or the hand-rolled frame decoder, which has randomized and malformed-input tests. Signed-history records are canonical JSON with unknown fields rejected. Server-side code is not in this repository. | 5.1, 5.7, 3.4.5 |
| ACL / RBAC issues, data leakage, PII leaks | Contracts authorize transfer and are MAC-verified by the provider. Network-mode roles and principal come only from MACed bytes. Non-public egress requires a Network relationship on both sides. The seal keeps payloads from relays. Logging is covered in §8. | 5.3, 5.5, 3, 8 |
| Insecure storage, memory leaks, application DoS | connect persists only signed contract evidence, in 0700/0600 stores under `os.Root`. Seeds and keys are handed to the embedding app. Inbound sizes are bounded before allocation. Some count caps are missing (L10). Panics on the receive loop cancel the client. | 8, 4.7, 5.1, 5.7 |
| Transport security, downgrade attacks, replay attacks | Pinned-root TLS to the platform on every path; TLS 1.3-only seal; Required mode against seal downgrade. Replay handling is at the sequence layer, with known gaps (L17). H3 early data is noted (L18). | 5.4, 4.1, 4.2, 5.2 |
| Interception / MITM attack surface | Transport-level MITM is defeated by the exporter-bound identity proof. The operator remains a trust root for identity keys (L1). | 4.3, 5.4, 2.3 |
| Logic bugs and authentication flaws; crashes and memory corruption | Cipher-exposure state machine and fail-closed gates; contract verification; decoder robustness; memory-safety surfaces | 3.5, 3.7, 5.1, 5.3, 5.7 |
| Functional correctness of designs, implementations and algorithms | TLS 1.3 exporter-keyed AES-256-GCM; Ed25519 binding; signed-history verifier | 3, 4.8 |
| Attacks from current academic research | Random-nonce GCM bounds; PQ hybrid and downgrade resilience; exporter-bound authentication (unknown key-share, reflection); cross-protocol signature reuse | 4.8, 4.1, 4.3 |
| Timing attacks; constant-time operations | Go stdlib primitives; hardware AES where available (L6); `hmac.Equal` / `subtle` for every secret-keyed MAC compared against peer input | 4.4, 5.7 |
| Random-number generation | `crypto/rand` for all keys, nonces and secrets; `math/rand` only for identifiers, jitter and shaping | 4.5 |
| Side channels, information leaks, secure storage and data processing | Metadata, sizes and timing visible to relays; no zeroization; logging review | 4.6, 8 |
| DoS vectors, information leakage, logic bugs | Wait-free receive loop; rate limits on handshake recovery; bounded trial decryption | 4.7, 5.1, 5.2 |
| Numeric values and floating point | Byte accounting is `int64`, with an unchecked conversion and a float32 bound (L21). Floats elsewhere only pace and score. | 5.3 |
| Third-party integrations and dependencies | Direct dependency inventory; in-tree and sibling forks; `govulncheck`: no reachable vulnerabilities | 7 |

## 7. Third-party dependencies and supply chain

**Direct dependencies with a security role** (`go.mod` at the audited commit):

| Module | Version | Role | Imported by (non-test) |
|---|---|---|---|
| Go standard library (`crypto/tls`, `crypto/aes`, `crypto/cipher`, `crypto/ed25519`, `crypto/ecdsa`, `crypto/hmac`, `crypto/sha256`, `crypto/rand`; ML-KEM through `crypto/tls`) | go1.27.3 | Seal, contract MACs, transport TLS | throughout |
| `golang.org/x/crypto` | v0.58.0 | `sha3` (Keccak-256 signer addresses), `cryptobyte` | `transfer_key_history.go`, `transport_pt.go`, `net_tls.go`, `net_resilient.go` |
| `github.com/decred/dcrd/dcrec/secp256k1/v4` | v4.4.1 | Public-key recovery for the signed client-key history | `transfer_key_history.go` |
| `github.com/quic-go/quic-go`, `quic-go/qpack` | v0.61.0, v0.6.0 | H3/QUIC platform transport | `transport.go`, `transport_authorization.go`, `transport_family.go`, … |
| `github.com/gorilla/websocket` | v1.5.3 | H1 WebSocket platform transport | `transport.go`, `framed_message_conn.go`, `transport_authorization.go`, … |
| `github.com/pion/webrtc/v4`, `dtls/v3`, `ice/v4`, `datachannel`, `transport/v4` | v4.2.18, v3.1.5, v4.4.1, v1.6.2, v4.1.0 | WebRTC P2P transport | `transport_p2p_webrtc*.go`, `net_extender_webrtc*.go` |
| `github.com/pion/sctp` → **`./sctp` (in-repo fork)** | replace | SCTP for WebRTC data channels | `sctp/` |
| `github.com/theodorsm/covert-dtls` | v1.5.1 | DTLS ClientHello mimicry | `transport_p2p_webrtc_dtls.go` |
| `github.com/refraction-networking/utls` | v1.8.2 | TLS ClientHello mimicry (platform and extender transports) | `net_tls_hello.go`, `net_extender_camouflage.go`, `vless_*.go` |
| `src.agwa.name/tlshacks` | v0.0.4 | ClientHello parsing | `net_tls.go`, `net_resilient.go`, `extender/extender.go` |
| `github.com/golang-jwt/jwt/v5` | v5.3.1 | Reading the client's own JWT | `jwt.go`, `transfer_oob_local_cleanup.go` |
| `gvisor.dev/gvisor` → **`../gvisor` (sibling fork)** | replace | Userspace TCP/IP stack for the IP data plane | `tun.go` |
| `github.com/gopacket/gopacket` | v1.7.1 | Packet parsing | `tun.go`, `ip_security.go` |
| `github.com/libp2p/go-libp2p`, `go-libp2p-pubsub` | v0.49.0, v0.17.0 | Extender gossip directory | `gossip/` |
| `google.golang.org/protobuf` | v1.36.11 | Wire messages (`ProtoUnmarshal` = `proto.Unmarshal`, `message_pool.go:1395-1397`) | `protocol/`, throughout |
| `golang.org/x/net` | v0.62.0 | DNS, ICMP, http2 helpers | `net.go`, `net_resilient.go`, `ip_sni.go`, `transport_pt.go`, … |
| `github.com/urnetwork/glog` → **`../glog` (sibling fork)** | replace | Logging | throughout |

**Supply-chain notes**

- Three modules are replaced with forks outside the module proxy:
  - `./sctp`, inside this repository;
  - `../gvisor` and `../glog`, sibling checkouts (`go.mod:98-102`).
  
  These forks are part of the trusted code base and are not covered by
  `go.sum` integrity checks against upstream.
- The most recent dependency security update is `golang.org/x/net` v0.62.0
  (commit `8649d2e3`, merged in `f2fd021c`), for GO-2026-6603, -6610, -6611,
  -6612 and -6617.
- **`govulncheck`** v1.9.0 (built with Go 1.27.3; vulnerability database
  updated 2026-11-05), run on `./...` at `69967d8a` (`7dbcdd4c` merged into
  `main`):
  - Symbol level: **no vulnerabilities in code this module calls.**
  - Module level: one entry, GO-2026-5932, which marks the deprecated
    `golang.org/x/crypto/openpgp` package as unsafe. connect does not import
    it; from `golang.org/x/crypto` it uses only `sha3` and `cryptobyte`.
  - The second set of fixes (§1.4) changes no dependencies.
- No cgo is used by the seal; see §5.7 for `unsafe`/assembly/cgo across the
  repository.

## 8. Storage, logging and memory hygiene

### 8.1 What connect persists

The library never stores the identity seed, TLS private keys or provide secret
keys itself. It hands them to the embedding application through accessors:

- `ClientKeyManager.Seed()` (`transfer_key.go:193-205`);
- `ProvideTlsPrivateKeyPem()` (`transfer_encrypt.go:3515-3525`);
- `GetProvideSecretKeys()` (`transfer_contract_manager.go:1335-1340`). This is a
  shallow map clone whose slices alias the live keys.

Window-client JWTs go to an embedder-supplied store
(`ip_remote_multi_client_identity.go:29`, `:435`). How these are protected at
rest is the embedding app's responsibility (SDK and apps, outside this
repository).

| Store | Path source and protections | Contents |
|---|---|---|
| Original-contract store (`transfer_contract_creation_store*.go`) | From settings only; must be absolute and canonical. The runtime never creates the directory or lock (`transfer_contract_creation_custody.go:34`). The path is walked component by component under `os.Root` with no symlink following (`transfer_contract_creation_store.go:59-88`). The directory must be 0700 and owned by the effective uid (`:98`). Files are created `O_EXCL` with mode 0600, then chmod 0400 and fsynced, along with the directory (`:330-363`). | Ed25519-signed request leaves and the raw `CreateContractResult` frames, which include `stored_contract_hmac`. File names are content SHA-256s, re-checked on read (`:304`). Not encrypted. |
| Whole-work outbox (`transfer_contract_work_outbox*.go`) | Same model (`transfer_contract_work_outbox.go:195`, `:244`, `:592-635`). Per-record SHA-256 in an index; the index SHA-256 in an xattr checkpoint (`:132`). The checkpoint itself is not MACed. | Signed work cuts containing stored contracts, creation originals and close-report inventories. Not encrypted. |
| Extender seed bundle (`net_extender_seed_store.go`) | Directory 0700, temp file 0600, then rename (`:282-322`). The signature is re-verified on every use (`:427`). | Signed bootstrap bundle |
| `connectctl` CLI | `--extender_key_file` written 0600 with a non-atomic write (`connectctl/extender.go:1236`) | Plaintext hex extender Ed25519 seed (operator tool) |
| `durablevolume/` | Offline preparation/restore library; no production import in this module | Byte-exact copies, digest-chained |

### 8.2 Logging

- **Secrets.** Library logs contain no JWTs, seeds, private keys, PEM blocks,
  provide secret keys, HMACs, exporter outputs or AEAD keys; only lengths are
  logged (for example `transfer_encrypt.go:1904-1909`). The `connectctl` CLI
  prints auth responses, including JWTs, to stdout (`connectctl/main.go:518`,
  `:526`). That is an operator tool.
- **User destinations (PII).**
  - Most lines that include destination IPs, ports or hostnames are behind
    `V(1)` or `V(2)`. Examples: `ip.go:2971-2992`, `ip_remote_multi_client.go:5923-5927`,
    `tun.go:1643-1767`, `net_http_doh.go:1226-1272`.
  - **Always-on exceptions:**
    - the stale-DNS line, which logs the queried domain
      (`net_http_doh.go:1036`, reached from `Tun` dials);
    - a throttled policy line with app id and destination port
      (`ip_remote_multi_client.go:4303-4312`).
  - glog writes only ERROR and above to stderr by default, but an embedding
    app's `Logger` (`log.go:41-47`) may persist Info.
- **Third-party text.** Pion's error messages are relayed verbatim
  (`transport_p2p_webrtc.go:1203-1207`).

### 8.3 Key-material lifecycle

Secret key material is **not zeroized** anywhere:

- the identity key and seed (`transfer_key.go:73`, `:126-128`, `:236`, `:256`, `:267`);
- AEAD keys and exporter outputs (`transfer_encrypt.go:289-293`, `:953`, `:1846`, `:1854`);
- the provide TLS private key PEM (`:3343`, `:3614`, `:4219-4220`);
- provide secret keys (`transfer_contract_manager.go:623`, `:1339-1347`).

Old epochs are released by dropping references (`transfer_encrypt.go:4555-4560`).
Go's garbage collector and the message pool give no zeroization guarantee. We
treat process-memory disclosure as out of scope for the library, and record it
here so it is an explicit decision.

### 8.4 Memory and DoS bounds for inbound data

**Frame and message limits**

| Bound | Default | Evidence |
|---|---|---|
| Byte-stream framer (16-bit length) | Length checked against `maxFrameLen` before allocation; platform default 16 KiB | `message_framer.go:83-103`; `transport.go:913`; `transfer.go:1727` |
| XL framer (32-bit length) | Checked before allocation | `message_framer_xl.go:95` |
| H1 framed connection and WebSocket read limit | 16 KiB | `framed_message_conn.go:55-60`, `:134`, `:186`; `transport.go:2547`, `:2764`, `:914` |
| QUIC | 8 incoming bidirectional and 8 unidirectional streams; flow-control windows bounded and memory-scaled | `transport.go:1090-1122`, `:1293-1317` |
| P2P message / receive queue | 64 KiB / 256 messages and 256 KiB | `transport_p2p.go:186-193`, `:42-104`; pion `MaxMessageSize` 64 KiB (`transport_p2p_webrtc.go:863`) |
| HTTP response / DoH response | 2 MiB / 16 KiB | `net_http.go:63`, `:1306-1312`; `net_http_doh.go:43` |
| Per-sequence receive queue | ≤ 2.5 MiB (memory-scaled), ≤ 4 open receive contracts | `transfer.go:1063-1115` |
| Logical lanes | ≤ 8, validated on receive | `transfer_logical_lane.go:8`; `transfer.go:15569` |
| Live stream sequences / stream authorizations / capability records | 1024 / 4096 and 128 / 1024 | `transfer_stream_manager.go:510`; `transfer_stream_authorization.go:20-21`; `transfer_peer_capability_observation.go:40` |

**No count cap (we want this reviewed):**

- the number of distinct receive sequences (sources) per client
  (`transfer.go:15367-15374`, `:15405`, `:15770`);
- the number of encryption sessions (§4.7);
- the per-epoch TLS inbox byte size (§4.7);
- provider NAT flow tables when no memory target is set. "No target: unlimited
  flow counts" (`ip.go:694-708`; `UserLimit` / `GlobalLimit` 0 = no limit,
  `ip.go:448`, `:4287-4293`). With a target, scaled caps apply (`ip.go:604-613`).

The process memory budget is advisory sizing, not admission control
(`memory_budget.go:7-38`; `transfer_memory_budget.go:15-17`).

## 9. Consolidated limitations and open items

These are known and deliberate, or known and open. Each one points to where it
is described.

| # | Item | Status | Where |
|---|---|---|---|
| L1 | The operator distributes identity keys; first-contact substitution by a consistent operator is not detected | Design limit. Next step: multi-operator plurality read | §3.4.5, §4.3; `DESIGNNOTES2.md` §3; `DESIGNNOTES3.md` §8, §10 |
| L2 | Opportunistic mode downgrades to plaintext under interference, including a forged unknown-wrap nack | By design. Required is the enforcing mode | §3.1, §3.9, §4.1 |
| L3 | Signed-history gate is Required-only. On fetch error it accepts the contract key without consulting the pin. Pin extension accepts any chain whose head is past the pinned generation (a rotation). TOFU when no signer is pinned. | Open — review requested | §3.4.5 |
| L4 | Random-nonce AES-GCM with no per-key invocation counter or volume/time rekey; one key serves both directions of a session | Open — review requested | §4.8.1 |
| L5 | Plaintext surfaces inside the transport: the control plane, `EncryptedControl` frames, acks (accepted plaintext in every mode), the outer routing header, sizes and timing | By design | §3.8, §4.6 |
| L6 | AES-GCM only. CPUs without AES instructions use table-based software AES. | Open — review requested | §4.4 |
| L7 | Plaintext-frame source attribution is transport-level: connect does not authenticate `TransferPath.SourceId` of plaintext frames. On P2P routes, frames naming the platform or this client are dropped, but other peer ids are not compared with the far-end peer (a multihop endpoint knows only its adjacent hop). | By design for platform-relayed frames. P2P: platform and self spoofing fixed (§1.4); per-peer binding open — review requested | §2.3, §5.4 |
| L8 | No post-quantum-only policy; X25519 is accepted for peers without the hybrid | Follow-up (`DESIGNNOTES2.md` §8.4) | §4.1 |
| L9 | X.509 validity, KeyUsage and EKU are not enforced on seal certificates (identity comes from the Ed25519 binding) | By design | §3.2, §4.8.6 |
| L10 | No count caps on sessions or receive sequences per client, and no byte cap on the per-epoch TLS inbox. Provider NAT tables are unbounded without a memory target. | Open — review requested | §4.7, §8.4 |
| L11 | Secret key material is not zeroized | Accepted, recorded | §8.3 |
| L12 | Contract stores hold contract HMACs in plaintext, integrity-protected by content digests and signatures only | Accepted, recorded | §8.1 |
| L13 | Always-on logging of stale-DNS query domains (`net_http_doh.go:1036`) | Open | §8.2 |
| L14 | Private-extender admission header: timestamp and nonce are MACed but not checked for freshness, so a captured header can be replayed (`extender/extender.go:1457-1479`). The HMAC comparison is now constant-time (§1.4). | Freshness open | §5.7 |
| L15 | `govulncheck`: no reachable vulnerabilities; one module-level entry (GO-2026-5932, `golang.org/x/crypto/openpgp`), a package connect does not import | Resolved | §7, §10 |
| L16 | Comment drift in seal files | Documentation | §4.9 |
| L17 | Replay at the sequence layer: no dedupe for no-ack packs; a removed `ReceiveSequence` can be re-opened by a replayed head pack carrying its contract; the seal itself has no replay window | Open — review requested | §4.2, §5.2 |
| L18 | Desktop/server H3 dials use `DialEarly`, so the JWT auth message can be 0-RTT data when resuming; acceptance is server-side | Open — review requested | §5.4 |
| L19 | Platform roots are pinned to ISRG X1/X2 (no SPKI pin); TLS 1.2 is allowed on non-QUIC platform paths; no `wss`/`https` scheme enforcement | Accepted, recorded | §5.4 |
| L20 | Manual extenders without a key have an unauthenticated outer TLS; VLESS links may set `allowInsecure` (the inner platform TLS stays pinned in both cases) | By design | §5.4 |
| L21 | Contract byte limits use an unchecked `uint64→int64` conversion and a float32 product | Open — review requested | §5.3 |
| L22 | WebRTC DTLS fingerprints arrive in platform-relayed signals; no binding to client identity keys | By design (operator trust) | §5.4 |
| L23 | Egress public-unicast check treats 100.64.0.0/10 (shared address space), 0.0.0.0/8 other than 0.0.0.0, 192.0.0.0/24, 198.18.0.0/15, 240.0.0.0/4 and 255.255.255.255 as public (`ip_security.go:886-905`). The CFAA blocklist generator subtracts the same reserved ranges (`security/main.go:124-138`), so neither layer refuses them on a provider. | Open — review requested | §5.5 |

## 10. Reproducing the evidence

```sh
# from the repository root at 7dbcdd4cd6923e7d8c6b101f4ad0be53af89b61e

# 1. The seal test set used for §3.10 (192 tests across the seal test files)
names=$(grep -h '^func Test' transfer_encrypt*_test.go transfer_key*_test.go \
  transfer_required_cipher_wakeup_test.go ip_remote_multi_client_pqe_test.go \
  ip_remote_multi_client_post_quantum_discovery_test.go \
  transfer_contract_manager_post_quantum_test.go \
  transfer_peer_capability_observation_test.go \
  | sed -E 's/^func (Test[A-Za-z0-9_]+).*/\1/' | sort -u | paste -sd'|' -)
go test -count=1 -timeout 40m -v -run "^(${names})\$" .

# 2. Decoder and data-plane robustness tests and fuzz targets
go test -count=1 -run 'TestDecodeTransferFrame|TestFrameCodec|TestAckCodec|TestEncryptedOuterFrameCodec|TestSequenceCipherSealOuterFrame' .

# 2a. Regression tests for the fixes in §1.4
go test -count=1 -run 'TestLegacyContractHmacForgeryRejectedAfterCutover|TestEmptyStoredContractBytesNeverVerify|TestStoredContractHmacLeavesCallerBytesUntouched|TestContractManagerRefusesForgedLegacyContract|TestStoredContractHmacCutover|TestSealedFrameWithoutInnerPathDoesNotStopTheClient|TestPackOfAnotherLaneForAClosingSequenceDoesNotStopTheClient|TestReceiveBufferHealsAStaleHeadIndexForThePacksOwnId|TestP2pRouteFrameClaimingPlatformOrSelfIsDropped|TestSignedIdentityFetchErrorRefusesAChangedPinnedKey|TestPinRefusesForkThatAdvancesPastThePinnedGeneration' .
go test -count=1 -run 'TestAllowedSecretComparesInConstantTime|TestAllowedSecretRefusesPartialSignatures' ./extender/
for f in FuzzParseIpPathWithPayload FuzzSecurityPolicyInspectEgress FuzzH3DatagramReassembler; do
  go test -run '^$' -fuzz "^${f}\$" -fuzztime 30s .
done

# 3. Known vulnerabilities in dependencies (govulncheck built with Go >= 1.27)
go install golang.org/x/vuln/cmd/govulncheck@latest
govulncheck ./...
```

The full root-package suite (`go test -count=1 -timeout 90m .`, about 28 min)
was also run on the fixed code. That tree differs from the header commit only
by `tun_ipv4_policy_test.go`, a test file merged from `main` that passes on its
own. Every test passed except five in unrelated subsystems, which also fail on
an unmodified `03e98fe5`:

- deterministic: `TestCanonicalH1ChunkSettingsRealOwnersStayIndependent`,
  `TestPlatformWebSocketReadyDrainBalancesAcksAndPayloadBytes`,
  `TestWindowPathServicePerformanceMatrix`;
- intermittent: `TestNatMemoryTcpAckProgressAtFullDataBudget`,
  `TestNatProviderMemoryTcpAckProgressAtFullDataBudget`.

---

## Appendix A — seal constants and defaults

| Name | Value | Evidence |
|---|---|---|
| AEAD key exporter label | `urnetwork-connect-aead` (32 bytes) | `transfer_encrypt.go:90-91` |
| Identity-proof exporter label | `urnetwork-connect-identity-proof` (32 bytes) | `transfer_encrypt.go:97-98` |
| AEAD nonce | 12 bytes, random per frame | `transfer_encrypt.go:92`, `:304-311` |
| TLS | 1.3 only; groups `[X25519MLKEM768, X25519]`; ALPN `urnetwork/sequence` | `transfer_encrypt.go:382-433` |
| `EncryptionSettings.Mode` | `EncryptionModeOff` | `transfer_encrypt.go:788` |
| `TlsTimeout` | 60 s (TLS + identity proof) | `transfer_encrypt.go:796` |
| `IdentityProofResendInterval` | 2 s, doubling, within `TlsTimeout` | `transfer_encrypt.go:797`, `:1994-2047` |
| `TlsInitialRetryInterval` / `Stagger` / `MaxInterval` | 60 s / 60 s / 5 min | `transfer_encrypt.go:798-800` |
| `RequiredCipherPollInterval` | 20 ms (retry floor only) | `transfer_encrypt.go:801` |
| `EncryptionControlUseCompanion` | `true` | `transfer_encrypt.go:802` |
| `MaxClientKeyHistoryGenerations` | 64 | `transfer_encrypt.go:809` |
| `UnknownWrapNackMinInterval` / `RestartCooldown` | 1 s / 5 s | `transfer_encrypt.go:817-818` |
| `IdleTimeout` | 0 standalone; `max(send idle, receive idle)` in `DefaultClientSettings` | `transfer_encrypt.go:823`, `transfer.go:674-677` |
| `OpportunisticEstablishHold` | 0 (disabled) | `transfer_encrypt.go:590-607` |
| Ephemeral certificate | ECDSA P-256, validity −1 h…+24 h, 64-bit random serial | `transfer_encrypt.go:435-466` |

## Appendix B — seal file index

| File | Contents |
|---|---|
| `transfer_encrypt.go` | Sessions, epochs, TLS configs, AEAD, identity proof, gates, nacks, manager |
| `transfer_key.go` | Ed25519 identity key, signing and verification helpers, publication |
| `transfer_key_history.go` | Signed client-key history verifier (secp256k1, Keccak, chain rules, pins) |
| `transfer_key_history_session.go` | Signed-history policy and cipher gate (Required) |
| `transfer_key_registration.go` | Processed client-key registration publication |
| `transfer.go` | Wrap/unwrap (`writeMaybeWrappedBytes`, `Client.run`, `unwrapFrame`), Required gates, ack sealing, contract → session wiring |
| `protocol/transfer.proto` | `TransferFrame` fields 6–8, `EncryptedControl`, `EncryptedKey`, `ClientKey`, contract fields |
| `ip_remote_multi_client.go` | Post-quantum profile → Required; candidate prefilter |
| `transfer_peer_capability_observation.go` | Read-only evidence of what a peer negotiated |
| `DESIGNNOTES.md` §3, `DESIGNNOTES2.md`, `DESIGNNOTES3.md` | Design and threat-model rationale |
