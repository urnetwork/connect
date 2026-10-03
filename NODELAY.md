# Connection and packet-flow delay audit

Source review: 2026-09-24. This audits the current Connect checkout, including
uncommitted dual-family DoH work; it is **not** a latency measurement or proof
that the reviewed code is deployed. The test hold was lifted on 2026-09-24;
each implementation still needs its own recorded RED/GREEN and race results.

The rule is to wait for a *state change* when state can wake the waiter. Keep a
timer as a deadline, pacing policy, retry backoff, or genuinely time-based
eligibility boundary—not as the only way a ready packet or connection is
noticed. An event conversion must atomically arm its notification with the
corresponding state read and must preserve cancellation, bounds, and ownership.

## Confirmed avoidable readiness polling

| Path | Current delay | Why it is avoidable | Correct direction |
| --- | --- | --- | --- |
| Required-encryption send gate | Up to one 20 ms poll interval plus scheduling after cipher readiness. [`SendSequence.Pack`](transfer.go) rechecks `Cipher()` on a timer; the default is in [`EncryptionSettings`](transfer_encrypt.go). | A newly usable cipher does not wake a blocked Pack. This affects a Required-mode send during establishment, not traffic already using a cipher. The numeric send budget can also be overshot by a poll interval unless its context expires first. | Add a session-owned readiness/state broadcast, subscribed atomically with the cipher/history read. Wake for cipher, signed-history, epoch, failure, and retirement transitions; retain the exact handshake retry cooldown and deadlines. An epoch-completion channel alone is wrong: it closes on failure too, and signed history can become valid later. |
| Empty provider window | Up to one 200 ms `FormationPollTimeout` interval after the first *eligible* provider is installed; zero configuration restores the 2 s `SendRetryTimeout`. [`sendPacket`](ip_remote_multi_client.go) waits only on cancellation/timer when its filtered offer is empty. | Candidate installation changes the client map without notifying the available `resizeMonitor`; that monitor's later client-death notification is not an offer-publication event. The first DNS/SYN can therefore sit after a usable provider appears. | Publish an owner-scoped eligible-offer generation/notification with install, removal, and policy changes that affect selection. Subscribe with the filtered window read, preserving lock order. Keep retry pacing for already-present but failed candidates and timers for genuine policy expiry. A nonempty raw client map is not proof of a usable offer. |

The first gate is at `transfer.go:6868–6924`, with the 20 ms setting at
`transfer_encrypt.go:713–718,783,2598–2607`. Cipher/history boundaries are at
`transfer_encrypt.go:2044,2051,2714` and
`transfer_key_history_session.go:262–299`. The second is at
`ip_remote_multi_client.go:7122–7155`; defaults are at `:192–195`, candidate
publication at `:11371–11379`, and the unrelated death notification at
`:11410–11415`. These are source facts, not incident attribution.

Deterministic controls should exercise the actual Pack and packet-send paths,
using barriers rather than sleeps. Hold a verified handshake or signed-history
transition while a send is parked behind an exaggerated poll interval; release
readiness and require admission before virtual time advances. Cover multiple
waiters, cancellation, zero-timeout refusal, stale epochs, retry cooldown, and
rekey continuity. For the provider window, hold the retry timer with no offer,
publish the first selectable candidate, and require the first DNS/SYN before
timer release. Wrong-family, filtered, removed, replaced, and already-failed
candidates are negative controls. See [CODESTYLE.md](CODESTYLE.md) for test
ownership and no-sleep requirements.

## Timers that are not DNS-answer delays

- The current dirty dual-family TCP DoH path launches the first usable A or
  AAAA answer immediately. Its 250 ms `DefaultDialFallbackDelay` is armed
  *after* the first address attempt and advances immediately on definitive
  failure (`net_dial_race.go:367–468`). Scalar UDP takes the first usable family
  (`:199–243`); multi-candidate H3/Alt retain late answers for fallback.
  This source behavior still needs exact-binary and deployment verification.
- The DNS mux's 250 ms cold local-fallback delay (1 s warm) deliberately
  prefers tunnel DoH over host-egress DoH, trading a bounded startup tail for
  reduced DNS leakage (`ip_mux_upgrade.go:168–173,1723–1726,1848–1867`). It
  is not an A/AAAA merge wait. A terminal tunnel failure currently has no
  immediate fallback wake; a faster failure transition would need a separate
  privacy-policy decision and causal test.
- DoH server hedging starts its first wave immediately. Additional servers
  have a 750 ms cold / 100 ms warm stagger, overridden to 350 ms warm in the
  mux; quiet lookups may race a bounded first set (`net_http_doh.go:110–113,
  1614–1623`; `ip_mux_upgrade.go:240,528–532`). When an entire first wave fails
  definitively, the next wave still waits for its hedge timer. Test whether
  failure-driven advance can preserve burst admission and negative-answer
  anti-filtering before changing this policy.
- The 250 ms TUN outbound-queue value is a *maximum* wait: capacity wakes
  the writer through `space` (`tun.go:821–842`). The transport's mode-start
  delay and reconnect jitter are fallback/load policies, not sleeps imposed
  on a successful first mode (`transport.go:1595–1612,1975–1985,2075–2120`).
  The repeated canceled-flight DoH 250 ms floor is anti-spin pacing, not a
  normal lookup wait (`net_http_doh.go:919–940`).

## Follow-up audit and acceptance

1. Instrument stage timestamps per logical attempt—DNS first answer, first
   dial launch, connected carrier, authenticated contract, cipher ready,
   eligible provider publication, first packet enqueue, and first response—
   without raw hostnames, addresses, or customer identifiers. Compare those
   same-attempt boundaries before attributing a user-visible delay.
2. Classify each remaining timer by *deadline*, *intentional hedge/pacing*,
   *idle detection*, or *readiness polling*. Review packet-flow waits in
   `transfer.go`, `ip_remote_multi_client.go`, `tun.go`, and the carrier
   adapters with their receive-callback backpressure contract. A timeout's
   presence alone is not a bug; deleting it can create hot loops, memory
   pressure, leaked DNS, or cross-flow head-of-line blocking.
3. Convert the two confirmed polls only with source-frozen RED/GREEN controls,
   focused normal/race tests, adjacent-path review, and platform validation.
   The existing source tests for formation defaults and Required-mode refusal
   do not establish immediate readiness wake. This source audit itself ran no
   tests; implementation results must be attached separately.

This is a bounded source review, not an exhaustive per-timer certification or
a claim that the two delays caused a particular Main or device incident.

## Implementation gate status (2026-09-24)

The Required-mode cipher wake has been implemented in the Connect worktree.
Independent focused RED/GREEN execution found six expected causal RED
failures while four healthy controls passed; the corrected source passed
focused normal and race tests, adjacent normal and race tests, and `go vet .`.
The immutable gate receipt is
`/Users/brien/urnetwork/monitor/nodelay-cipher-gate-sol-20260924.VNeCa6/receipt.md`
(SHA-256 `760b37cbffc274f0cd3094cfe1513dc77e45de7eb37fd81624f3ef038a1b7302`).
The provider-window wake also passed its independent causal RED/GREEN,
focused/adjacent normal and race, and vet gates. With both fixes in the
actual Connect worktree, the combined focused/adjacent normal and race tests
and `go vet .` passed under one source-frozen gate. Its receipt is
`/Users/brien/urnetwork/monitor/nodelay-combined-shared-sol-20260924.usnRdj/receipt.md`
(SHA-256 `d018c593644a6ee95c19321e214ded804bd6061e573789ae3396167ff64ad34c`).
This is local evidence only: full-package normal/race, exact binary
provenance, and deployment verification remain open.
