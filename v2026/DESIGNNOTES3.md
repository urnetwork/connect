# DESIGNNOTES3 — closing the operator key-substitution hole, client side

Design for the client half of signed client-key verification: what `connect`
verifies, what it does when the evidence is absent, and why the fallback has to
be a ratchet rather than a choice.

> **Status (2026-09-17):** DESIGN, not built. Supersedes `DESIGNNOTES2.md` §3.3
> Tier 0 as the next step — the server-side substrate that landed since
> DESIGNNOTES2 was written makes a stronger step available for the same client
> effort. Decisions taken: fallback is **accept-with-ratchet** (§4); enforcement
> is **`EncryptionModeRequired` only** (§5.2).

Companion to `DESIGNNOTES.md` §3 (end-to-end encryption) and §3.7 (the in-repo
threat model), and to `DESIGNNOTES2.md` (Finding 1, fixed; Finding 2, this
file's subject).

---

## 0. TL;DR

- **Finding 2's root cause is unchanged:** every defence in the sealed session
  verifies against `peerClientPublicKey`, and the shipping client takes that
  value from the operator-authored contract (`transfer.go:9560-9583` →
  `transfer_encrypt.go:2051`). The OOB cross-check exists and **logs only**
  (`transfer_encrypt.go:2109-2144`).
- **What changed since DESIGNNOTES2:** the operator now keeps a *signed,
  hash-chained, domain-bound* client-key history and can be made to answer for
  it (`server/model/st_client_key_history.go`,
  `server/controller/st_client_key_history.go`). DESIGNNOTES2 §3.3 assumed the
  only cheap move was comparing the operator against itself via `/key`. That is
  no longer true.
- **The upgrade in one sentence:** instead of comparing the contract key to
  another *unsigned* value from the same operator, compare it to a value the
  operator **signed**, inside a chain it cannot fork without leaving permanent
  divergent evidence.
- **Honest scope, unchanged from DESIGNNOTES2 §3.2:** this does not *prevent* a
  determined single operator from substituting once. It removes deniability and
  makes substitution attributable and detectable. Prevention still needs the
  plurality read across independent operators (`DESIGNNOTES2.md` status note).
  This is the prerequisite step for that, not a detour around it.
- **No new dependencies.** `secp256k1/v4` is already in the module graph and
  `golang.org/x/crypto/sha3` is already used by `transport_pt.go`. go-ethereum
  is **not** needed and must not be pulled in — the iOS gomobile build has a
  24 MiB Go limit.

---

## 1. Ground truth (verified in the trees, 2026-09-17)

**The registration record** (`sn/protocol/client_key_history.go:95-107`):

```go
type ClientKeyRegistration struct {
    Schema            string
    Domain            ClientKeyHistoryDomain     // chain id, genesis hash, netuid,
                                                 // coordinator, vault, deployment hash,
                                                 // policy hash, NoID
    ClientID          [16]byte
    NetworkID         [16]byte
    Generation        uint64
    Present           bool                       // false == signed tombstone
    PublicKey         [32]byte                   // the Ed25519 identity key
    PreviousHash      [32]byte                   // content hash of generation-1
    EffectiveBoundary ClientKeyEffectiveBoundary // epoch, block, block hash
    Signer            common.Address             // operator root signer
    Signature         [65]byte                   // secp256k1, canonical low-s
}
```

The invariants the protocol already enforces, and which the client re-checks
rather than trusts:

- `Digest()` (`:108-133`) refuses `Present == (PublicKey == zero)` and
  `(Generation == 1) != (PreviousHash == zero)`. A tombstone is signed; a
  present key is non-zero; generation 1 is the only chain root.
- `VerifySignature()` (`:178-185`) requires a **canonical low-s recoverable**
  signature that recovers to the stated `Signer`.
- `Follows(prior)` (`:207-228`) requires contiguous generation, `PreviousHash`
  equal to the prior's **content hash** (not its signing digest — the comment at
  `:189-190` warns these must never be interchanged), identical
  `Domain`/`ClientID`/`NetworkID`, a genuine value change, and a
  non-regressing `EffectiveBoundary`.
- `Domain.Digest()` (`:61-68`) names one immutable namespace and
  `Domain.Validate()` (`:39-45`) rejects any incomplete domain.

**The one thing the record does not self-authenticate**, stated by the protocol
itself at `sn/protocol/client_key_history.go:166-167`:

> *"Require canonical low-s recoverable signatures. **The caller must separately
> authenticate the expected signer in the operator's pinned chain version.**"*

Verifying the signature proves *somebody* signed it. It does not prove that
somebody is an authorised operator. The server resolves this by reading the
coordinator contract (`readBoundary` → `operator.RootSigner`). A client cannot,
and §3 is how it gets around that.

**The observation endpoint is validator-shaped and the client cannot use it.**
`ClientKeyObservationRequest.Validate()`
(`sn/protocol/client_key_history.go:243-248`) requires a non-zero
`ValidatorHotkey` (32-byte sr25519), `NativeBlock`, `NativeHash` and a
`DecisionBoundary` carrying an EVM block and hash. A consumer client has none
of these and is not chain-aware. `SnClientKeyObservation` is additionally gated
behind the full release-operator config
(`server/controller/st_client_key_history.go:47`), which needs chain RPC and
both signing keys. **So a new consumer-shaped read is required** (§6.1).

**The server already tiers its own answer** —
`server/model/network_client_key_model.go:23-43`. `GetClientPublicKey` consults
the signed SQL head first and falls back to Redis, and the file header says
why: *"Legacy clients retain their Redis source. Once signed operator history is
present, its durable SQL head is authoritative and Redis is only projection."*

---

## 2. Who can be registered — the question that sets the fallback

**A provider does not need to be registered with Bittensor.** The registration
is signed by the **operator's** root key; the provider is only its subject.
`StRegisterClientKey(ctx, clientID, publicKey)`
(`server/controller/st_client_key_history.go:128`) takes a client id and a key;
`NetworkID` is read from the operator's own `network_client` table
(`server/model/st_client_key_history.go:137`); the digest requires only
`ClientID`, `NetworkID`, `Generation`, `Signer`. No hotkey, no UID appears
anywhere on the path.

**The gate is the operator, not the provider.** `newStClientKeyAuthorityOwner()`
(`server/controller/st_client_key_history.go:47`) requires the operator's own
subnet config — enabled, netuid, chain id, coordinator address, RPC URLs, root
key, artifact key. An operator without it signs nothing, so *none* of its
clients reach tier S however Bittensor-registered they are.

Hence three tiers of evidence per peer, which the client must all handle:

| Tier | Evidence | Population |
|---|---|---|
| **S — signed** | Verifiable operator-signed, hash-chained registration history | Clients of a subnet-configured operator |
| **P — published** | Only the unsigned Redis `ckey_<clientId>` value | Legacy clients; every client of a non-subnet operator |
| **N — none** | Nothing published | Already failed under Required by the capability prefilter (`ip_remote_multi_client.go:13229-13236`) |

---

## 3. Where the trusted signer set comes from

This is the load-bearing design decision, because a signature checked against
an attacker-chosen signer proves nothing.

Three sources, composable, in increasing strength:

**(a) Build-pinned operator roots.** A list of `(domain digest, signer address)`
pairs compiled into the client. Simple, zero network, zero chain. Rotation
needs a release, which is a real operational cost but an acceptable one for a
set that changes rarely.

**(b) Trust-on-first-use, per peer.** The first chain that verifies for a peer
pins `(domainDigest, signer, generation, headKeyHash)`. Every later chain for
that peer must *extend* the pinned one — same domain, same signer (or a signer
change that the pinned chain itself attests), generation strictly greater,
`PreviousHash` linking back. An operator that substitutes after first contact
must fork its own signed log, and both forks are signed and attributable.

**(c) Plurality across independent operators.** The endgame from the
`DESIGNNOTES2.md` status note. Out of scope here, but (a) and (b) are exactly
the state a quorum reader needs anyway, so nothing built here is thrown away.

**Decision: build (a) + (b).** (a) makes first contact meaningful; (b) makes
every subsequent contact meaningful even for a domain not in the pinned set.
Together they give a real property without chain access from a mobile client.

---

## 4. The fallback must be a ratchet

The obvious fallback — "verify tier S when present, otherwise accept tier P" —
**reintroduces the hole it is closing.** A malicious operator does not
substitute; it omits the signed history and drops the client to tier P, which
is exactly the omission-cheaper-than-substitution move already documented for
certificates and keys (`DESIGNNOTES.md` §3.7; threat model §7.3). Omission is
quieter than forgery and needs no key material at all.

So tier may only ever ratchet **upward**, never down:

- **Per-peer ratchet.** Once a peer has verified at tier S, that peer is never
  again accepted below tier S. The pin records the tier alongside the key.
- **Per-operator ratchet.** Once *any* peer from an operator has verified at
  tier S in this install, tier P from that operator is refused for all peers.
  This catches a blanket disappearance, which a per-peer pin alone would miss
  for peers never seen before.

Both are cheap: one persisted map, keyed by peer id, plus one per-operator flag.
`sdk/local_state.go` is the existing home for this class of state.

A pin is **per install**, not global. It cannot detect an operator that is
malicious from a client's very first contact and consistently so — that is (c),
and this design does not claim otherwise.

---

## 5. The resolve state machine

### 5.1 Shape

Today `SetPeerClientPublicKey` (`transfer_encrypt.go:2051`) commits the
contract key immediately and fires an async advisory cross-check. Under
`Required` it instead **stages** the contract key and commits only on a
verified resolution. No new blocking primitive is needed: the send entry gate
already parks application data until `Cipher() != nil`
(`transfer.go:6751-6800`), and `Cipher()` already returns nil until the epoch is
established. An unresolved key simply looks like an unfinished handshake, which
every caller already handles.

The fetch runs concurrently with a multi-RTT TLS handshake, so in the common
case it adds no wall-clock latency. Reuse the single-flight and rate-limit shape
already proven in `maybeFetchPeerClientPublicKeyForIdentity`
(`transfer_encrypt.go:4286-4330`) — that path exists precisely because a verify
gate retries on every proof resend and must not become a request storm.

### 5.2 Policy, under `EncryptionModeRequired`

```
resolve(peerId, contractPub):

  tier S available:
      verify chain:  every registration VerifySignature()
                     every link Follows(prior)
                     Domain constant and Validate()
                     Domain/signer against pinned set (§3a) or peer pin (§3b)
                     head.Present == true
      head.PublicKey == contractPub ?  commit, ratchet pin to S
                                    :  TERMINAL, exclude provider

      chain invalid (bad sig, broken link, domain mismatch,
                     unknown signer, non-extending vs pin)
                                    -> TERMINAL, exclude provider

  tier P only:
      pinned tier == S              -> TERMINAL   (per-peer downgrade refused)
      operator ever served S here   -> TERMINAL   (per-operator downgrade refused)
      else  contractPub == /key value ?  commit, pin P
                                      :  TERMINAL, exclude provider

  tier N:
      prefilter already failed this candidate

  fetch/transport ERROR:
      commit contractPub, log            (availability — unchanged behaviour)
```

Under `EncryptionModeOpportunistic` every branch above **verifies and logs
only**. Refusing there degrades to plaintext, which is what a substituting
operator wanted; enforcement is meaningful only where fail-closed already holds
(`DESIGNNOTES2.md` §4).

### 5.3 The distinction that must not be blurred

**A fetch error is not a mismatch.** The capability prefilter already made this
call and the rule must match it exactly
(`ip_remote_multi_client.go:13229-13236`):

```go
return mode == EncryptionModeRequired && fetchErr == nil && len(publicKey) == 0
```

`fetchErr == nil` is doing the load-bearing work. If an unreachable operator API
were treated as evidence of substitution, one API blip would exclude every
provider for every Required user simultaneously — a self-inflicted global
outage with the same shape as the blackhole this whole subsystem exists to
avoid. Availability failures fall back; only *positively verified disagreement*
is terminal.

### 5.4 Terminal failure shape

Reuse the existing identity-failure machinery rather than inventing a parallel
one: set the epoch terminal the way `identityFailedTerminal` does
(`transfer_encrypt.go:2009-2012`), cancel the epoch, emit a new
`EncryptionEventKeyRegistrationRejected`, and exclude the provider from the
window through the same `MultiClientGeneratorExcluder` path the prefilter uses.
`Cipher()` then stays nil forever for that peer, Required refuses its traffic on
both directions, and the window replaces it. Nothing downstream needs to learn a
new failure mode.

---

## 6. What gets built, where

### 6.1 `server` — a consumer-shaped history read

`SnClientKeyObservation` cannot serve this (§1). Add a read that returns the
signed registration history for a peer and nothing else:

- No validator hotkey, no native/EVM decision boundary, no nonce.
- **No signing at request time**, therefore no root-key or artifact-key access
  and no chain RPC on the request path. It returns records that were already
  signed when they were stored, so it is a pure `LoadStClientKeyHistory` read
  (`server/model/st_client_key_history.go:203`).
- Returns an empty history — not an error — when `StEnabled()` is false or the
  client has no head, so the client sees tier P cleanly rather than a transport
  failure.
- Bounded by the existing `MaxStClientKeyHistoryRegistrations` (1024) and
  `MaxStClientKeyHistoryBytes` (8 MiB) constants; the client applies its own
  much smaller bound (§6.2).
- Unauthenticated, matching `GET /key/<client_id>`
  (`server/api/api.go:222-223`) — the content is already public evidence and
  authenticating it would leak nothing but cost the client a round trip.

### 6.2 `connect` — verifier and policy

A new `transfer_key_registration.go` (name provisional) holding a **minimal,
dependency-light verifier written against the documented canonical encoding**,
not a port of the `sn/protocol` implementation:

- `ClientKeyRegistration` decode from canonical JSON, rejecting unknown fields
  and trailing values exactly as `decodeCanonicalClientKeyJSON` specifies
  (`sn/protocol/client_key_history_evidence.go:107-124`).
- Digest reconstruction over the fixed-width tagged encoding — the JSON
  spellings are explicitly *not* the authority
  (`sn/protocol/client_key_history.go:48`).
- secp256k1 recovery via `decred/dcrd/dcrec/secp256k1/v4`, low-s enforced,
  address derived as the last 20 bytes of Keccak-256 over the uncompressed
  public key minus its leading tag byte, using `golang.org/x/crypto/sha3`.
- Chain walk implementing the `Follows` invariants from §1.
- A hard cap on accepted generations, far below the server's 1024, sized in
  the settings struct.

Settings additions, in `EncryptionSettings` beside the existing
`NewPeerClientPublicKeyFetcher` — **tunables live in settings structs with
`Default*Settings` defaults, never package consts** (`CODESTYLE.md`):

```go
// NewPeerClientKeyHistoryFetcher, when non-nil, resolves a peer's signed
// client-key registration history. Same per-session factory shape and
// lifetime rationale as NewPeerClientPublicKeyFetcher.
NewPeerClientKeyHistoryFetcher func(peerId Id) func(ctx context.Context) ([][]byte, error)

// TrustedClientKeySigners pins (domain digest, signer) pairs that may sign a
// registration. Empty disables build-pinning and leaves only the per-peer
// pin (§3b).
TrustedClientKeySigners []TrustedClientKeySigner

// PeerClientKeyPinStore persists the per-peer and per-operator tier ratchet
// (§4). Nil disables the ratchet, which permits silent tier downgrade —
// acceptable only for tests.
PeerClientKeyPinStore PeerClientKeyPinStore

// MaxClientKeyHistoryGenerations bounds an accepted chain.
MaxClientKeyHistoryGenerations int
```

### 6.3 `sdk` — wiring and the pin store

- Construct `NewPeerClientKeyHistoryFetcher` against the new endpoint, next to
  the existing fetcher at `device_local_provider.go:875-885`.
- Implement `PeerClientKeyPinStore` over `local_state.go`, alongside
  `client_key_seed` (`:788`).
- Surface the verified tier and the pinned fingerprint through the existing
  `PostQuantumIdentity` surface (`sdk/post_quantum_identity.go`), so the apps
  can show *signed* versus *published* rather than only sealed-or-not. This is
  the user-visible half of the property and the reason the surface exists.

---

## 7. Interaction with the rest of the seal

- **Finding 1 is still the prerequisite.** All of this is bypassed by a
  downgrade unless the client is Required (`DESIGNNOTES2.md` §4). It is also
  bypassed in practice while the Post Quantum Encryption default ships `false`
  in both apps, which remains the single highest-leverage open item.
- **The prefilter composes and should not change.** It already rejects tier N
  under Required. This design adds the S/P distinction *after* a candidate is
  admitted; the prefilter's narrow "confirmed empty, no fetch error" rule stays
  exactly as it is.
- **The certificate path is unchanged.** Defence 1 keeps verifying the chain
  signature under `peerClientPublicKey` (`transfer_encrypt.go:2837-2882`). What
  changes is only where that key came from, which is the whole point: Defence 1
  and Defence 2 become meaningful precisely because their root is no longer a
  value the operator can choose freely and deniably.
- **The `SendNoContract` / control-plane exemptions are untouched** and must
  stay verbatim (`DESIGNNOTES.md` §3.9, §4.1).

---

## 8. What this closes, and what it does not

**Closes.** A transport-level MITM with no key-API control. A compromised or
inconsistent contract-authoring path while the key store stays honest. An
operator that turns malicious after first contact with a peer, or that
substitutes for some clients and not others — both produce a signed fork.
Silent tier downgrade by omission (§4). Most importantly it makes any
substitution **non-repudiable and attributable**, which is what lets validators
already reading this log detect it.

**Does not close.** A single operator that is malicious from a client's first
contact and internally consistent about it. That operator signs one coherent
chain naming its own key, and a client with no independent view of who the
authorised signer is cannot tell. Closing it requires the plurality read across
independent operators, or self-certifying client ids
(`DESIGNNOTES2.md` §3.3 Tier 2/3). **Every document describing this work must
say so; "signed" is not "unforgeable by the signer".**

---

## 9. Test plan

Mirroring the Finding 1 tests, which are the model for pinning a security
property rather than an implementation:

| Test | Property |
|---|---|
| `TestSignedRegistrationVerifiesAndCommits` | The happy path commits and pins tier S |
| `TestSubstitutedKeyInSignedChainIsRejected` | Head key ≠ contract key ⇒ terminal, provider excluded |
| `TestForgedSignatureRejected` | Non-recovering / high-s signature ⇒ terminal |
| `TestBrokenChainLinkRejected` | `PreviousHash` not the prior content hash ⇒ terminal |
| `TestGenerationRollbackRejected` | Non-contiguous or regressing generation ⇒ terminal |
| `TestUnknownSignerRejected` | Signer outside pinned set and peer pin ⇒ terminal |
| `TestDomainMismatchRejected` | Registration from another deployment ⇒ terminal |
| `TestTierDowngradeRefusedPerPeer` | Peer pinned S, later served P only ⇒ terminal |
| `TestTierDowngradeRefusedPerOperator` | Operator once served S, later P for a new peer ⇒ terminal |
| `TestTierPAcceptedWithoutPriorSigned` | Non-subnet operator stays usable |
| `TestFetchErrorFallsBackAndDoesNotExclude` | Availability: transport failure ≠ mismatch |
| `TestOpportunisticVerifiesButNeverRefuses` | Mode scoping |
| `TestHistoryBoundEnforced` | Oversized chain refused without unbounded work |

**As built (2026-09-17): 41 tests across `transfer_key_history_test.go`,
`transfer_key_history_session_test.go` and
`transfer_key_history_crypto_test.go`**, the last of which builds arbitrary
chains with a local signer rather than relying on static vectors. Additions
beyond the plan above: address derivation against a published Ethereum test
vector, signature canonicalisation boundaries (including s = n/2 exactly),
signature splicing, digest field-binding for every field plus a reflection
guard against a future field escaping the signing payload, content-hash versus
signing-digest confusion, and per-generation signer authority.

**One result worth recording because it bounds the claim.** Chain verification
**cannot distinguish a rotation from a substitution**: re-signing the head
produces a genuinely valid chain, which is exactly what a legitimate rotation
looks like. The substitution is caught by the two checks layered on top — the
head must equal the contract-supplied key (§5.2), and the pin must not already
hold a different key at that generation (§4). `TestLongChainVerifiesEveryLink`
therefore corrupts only non-head generations, and
`TestResignedHeadVerifiesAsChainAndIsCaughtElsewhere` plus
`TestPinRefusesForkAtTheSameGeneration` pin where the real defence lives. Any
statement of the form "the chain verified, therefore the key is genuine" is
wrong.

---

## 10. Deferred

- The plurality read across operators (`DESIGNNOTES2.md` status note). This
  design is deliberately the substrate for it: the pinned
  `(domain, signer, generation, headKeyHash)` tuple is exactly what a quorum
  reader compares across operators.
- Self-certifying client ids (`DESIGNNOTES2.md` §3.3 Tier 2). Unchanged in
  scope and still the only complete answer to *substitution* for
  explicitly-addressed peers.
- Signer rotation attested by the chain rather than by a release. Build-pinning
  (§3a) is a deliberate simplification and should be revisited when the
  operator set starts changing at a rate a release cadence cannot track.
