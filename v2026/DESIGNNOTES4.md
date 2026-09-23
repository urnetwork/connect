# DESIGNNOTES4 — extender proximity: continent prior, latency probes, provider attestation

Design for how a client picks *close* extenders from gossip, and how a
provider's measured distance to an extender reaches the operator without either
party being able to lie about it alone.

> **Status (2026-09-18):** BUILT alongside this note. Decisions taken:
> attestation is **provider-role only** — ping never reports for a consumer
> client; the RTT is **signed by the provider**, so the extender can accept or
> reject it but cannot alter it; the extender's own observation is a **lower
> bound** the claim must clear.

Companion to `EXTENDER.md` (the extender protocol) and `DESIGNNOTES3.md`.

---

## 0. The problem

DNS bootstrap is regional by construction: Route 53 answers `extender.<host>`
per continent, and the TXT set vouches for exactly the addresses a continent
was answered with (`server/taskworker/work/extender_dns_publish.go:25-41`,
`:336-341`). Gossip is not. The record carries `CountryCode`
(`protocol/extender.proto`) and the directory stores it on the candidate — and
**nothing reads it to choose**: `SampleRecords` shuffles, `Candidates` orders by
verification and failure history, and a client bootstrapping from gossip gets a
uniformly random draw across the world.

Two things fix that, and they compose:

- a cheap **prior** — which continent the client is on, so same-continent
  extenders are tried first and the window fills without probing the world;
- a **measurement** — an RTT probe, because a continent is a coarse proxy and
  the thing that actually matters is the round trip.

A third thing rides on the measurement for free once it exists: a provider
probing extenders anyway can *attest* its measured distance to the operator,
which gives the operator a latency map of its public infrastructure.

## 1. Constraints that shape every choice below

1. **The extender must stay indistinguishable from a misconfigured CDN**
   (`extender/extender.go:35-44`). A distinctive probe endpoint is a censorship
   fingerprint. The probe is therefore a new `Service` value inside the
   existing TLS + `POST /` envelope, and its response is the ordinary
   `ExtenderResponse` frame plus one field.
2. **Attestation is provider-role only.** The extender is the first hop and
   sees the client's IP. A consumer client that identified itself to it would
   hand a durable id to the one party the threat model says learns "what your
   ISP already knows and nothing more" (§4), and the report would then reach
   the operator as `(id, extender, time)`. The provider role is different: its
   key is already durable and public (§9.2), and provider↔extender latency is
   operational data about public infrastructure. So: every client may *rank*
   by latency; only a provider *attests*. This is enforced in code — the
   attestor is installed by `DeviceLocal` when the provider role starts and
   removed when it stops — not left to callers.
3. **Neither party alone controls the number.** The provider signs the RTT;
   the extender gates it against its own observation. See §3.
4. **Bounded cost at the extender.** A probe must be cheap, stateless per
   probe, and rate-limited per source, or it is a DoS lever and an
   amplification lever toward the operator.

## 2. Wire format

`ExtenderHeader.Service = 3` (probe). `DestinationHost` is ignored.

`ExtenderHeader.ProbeClientId` (bytes, 16) — set only by an attesting
provider; empty for a ranking-only probe. Its presence is what asks the extender
for a nonce.

`ExtenderResponse.ProbeNonce` (bytes, 32) — returned when `ProbeClientId` was
set, this extender has an identity to bind the claim to, and it has somewhere
to report. Fresh random bytes per probe, held on the stack for the life of
that stream and compared there: no table, nothing that outlives the
connection, nothing to replay across streams.

After the response frame, on the same stream, an attesting provider sends
exactly one length-prefixed frame (the 4-byte big-endian prefix every extender
frame uses, bounded by `ExtenderMaxHeaderByteCount`):

```
message ExtenderProbeAttestation {
    bytes  ProbeClientId     = 1;   // the provider's client id
    bytes  ExtenderPublicKey = 2;   // the extender this was measured against
    bytes  ProbeNonce        = 3;   // echoed from the response
    uint32 RttMs             = 4;   // the provider's own measurement
    uint64 TimestampMs       = 5;
    bytes  Signature         = 6;   // ed25519 by the provider's client key over
                                    // "ur-extender-probe-v1" || body-without-signature
}
```

Binding the extender's public key stops a claim accepted at one extender being
replayed at another; the nonce stops replay in time; the client id says who.

After the frame the provider waits, within its probe budget, for the extender
to close the stream, which it does once it has read the frame. Closing first
would race the frame on the quic carriers, where a connection close discards
what the peer has not consumed.

`ExtenderRecordBody.ContinentCode` (string, field 12) — stamped by the
operator at signing from the same `model.ContinentCodeForCountry` the DNS sets
use, so "close" means the same thing on both paths. Old records decode with it
empty and sort last.

## 3. The attestation, and what each side can and cannot do

```
provider                          extender
   │  1. header{probe, client id}    │
   │────────────────────────────────►│  t0: send response{nonce}
   │  2. response{nonce}             │
   │◄────────────────────────────────│
   │  RTT measured here              │
   │  3. attestation{rtt, sig}       │
   │────────────────────────────────►│  t1: receive; I = t1 − t0
   │                                 │  accept iff rtt ≥ I − tolerance
   │                                 │  batch → operator (authenticated)
```

- **The provider cannot deflate.** It could delay message 3 to inflate its
  RTT (no incentive; harmless), but it cannot claim less than `I`, because
  the extender saw when message 3 arrived. The check is one-sided on
  purpose: equality with tolerance would false-reject on ordinary jitter and
  buys nothing.
- **The extender cannot alter.** The number is under the provider's
  signature. It can accept, reject, or drop — and dropping is visible to the
  operator as an extender that never reports, not as a wrong number.
- **The operator verifies the provider's signature** against its key store
  (`/key/<client_id>`), so the extender does no key lookups: it checks the
  nonce it issued, compares against `I`, and forwards. Stateless and cheap.
- **A colluding provider and extender can assert any distance.** Two parties
  can always lie about their mutual proximity. That is inherent, it is the
  same non-collusion assumption the threat model already rests on, and the
  operator bounds it only with its own independent probes (the activation
  prober already runs).

## 4. Client-side selection

Order in `ExtenderDirectory.Candidates`, highest first:

1. verified before unverified (unchanged);
2. **same continent as the hint** before other continents before unknown;
3. **measured latency**, ascending, for addresses with a sample — an address
   never probed sorts after every measured one within its tier;
4. fewest consecutive failures, most recent success, ip (unchanged).

The prior comes from two places, either sufficient: `GET
/network/extender-hint` (unauthenticated; the operator derives the continent
from the connection's address, which it sees on every request, so nothing new
is disclosed and a client has it before it logs in), and
inference from the DNS bootstrap answer (the continent of the extenders the
geo-DNS handed back *is* the client's, as judged by the same database). A
client that has neither ranks by latency alone, which is today's behaviour
plus measurement.

The probe pass runs after bootstrap and before the feed dial: up to `n` probes
per extender, stopping once `m` candidates are "close enough" — within
`ProbeCloseFactor × best` or under `ProbeCloseFloor`, whichever admits more,
so a badly-connected region still fills its window. Same-continent candidates
are probed first, which is what makes the prior save pings rather than merely
reorder them.

## 5. What gets built, where

| Where | What |
|---|---|
| `connect/protocol/extender.proto` | fields and message above |
| `connect/net_extender.go` | `ExtenderServiceProbe`, `ExtenderDial.ProbeClientId`, `ExtenderRoundTrip` |
| `connect/net_extender_probe_latency.go` | `ProbeExtenderLatency`: dial, time, attest |
| `connect/net_extender_directory.go` | `ContinentCode` on candidates, `RecordLatency`, `SetContinentHint`, the ordering |
| `connect/net_extender_network.go` | probe pass, hint fetch, DNS-inferred hint, `SetProbeAttestor` |
| `connect/net_extender_proximity.go` | `GetExtenderHint`, `PostExtenderLatencyReport`, `ExtenderLatencyReporter` (the extender's batching poster) |
| `connect/extender/extender_probe.go` | probe service: nonce, measure, gate, batch, `ProbeReportHandler` |
| `connect/connectctl/extender.go`, `sdk/device_local_extender_native.go` | the reporter, posting batches under the activation credential |
| `server/controller/extender_controller.go`, `extender_proximity_controller.go` | `ContinentCode` at signing; hint; latency ingest with signature verification, ownership check and nonce dedupe |
| `server/db_migrations.go` | `network_extender_latency` |
| `server/taskworker/work/extender_latency_work.go` | 30 day retention sweep |
| `server/model/network_extender_activation_model.go` | activation history: one row per activation with the address hash and the city/region/country it resolved to, as a provider's connection keeps them, so a ping can later be placed against where its extender was |
| `server/controller/stats_collector.go`, `grafana/dashboards/extenders.json` | provider ping gauges (pings, providers, extenders pinged over 24 h) and a per-extender pings counter, on the extenders dashboard |
| `sdk/device_local.go` | install the attestor on provider start, clear on stop |
| `connect/api/bringyour.yml` | the two new endpoints |

## 6. Limits stated plainly

- The prior is a continent, not a metro. Two extenders on the same continent
  can be 100 ms apart; the probe is what tells them apart, and the prior only
  decides who gets probed first.
- A latency sample is per address and per process. It is not persisted:
  yesterday's path is not today's.
- The hint endpoint tells a client its own continent as the operator sees it.
  That is information the operator already holds and the client's own DNS
  resolver already acted on; it is not a new disclosure in either direction.
- The extender waits `ProbeAttestationTimeout` (5 s) for the attestation
  frame, then closes the stream. A probe that takes longer is refused and
  simply re-probes.
- The gate's tolerance (`ProbeRttTolerance`, 20 ms) is also the most a
  provider can deflate a claim by. It absorbs the jitter between the two
  round trips; a refused honest sample costs one probe.
