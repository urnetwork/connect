# fingerprint — drift-conformance harness for the Chrome tcp/udp work

> Owner, 2026-10-07: "We need tests for the chrome tcp/udp work to prevent
> tcp/udp drift. Ideally the test would put chrome in a docker and capture the
> handshake packets, and compare them to our packets for the same local test
> endpoint."

This package is that harness. It fails a test — instead of a censor's filter —
when what connect emits drifts from what real Chrome emits: the TLS ClientHello
of the normal and resilient dialers (`net_tls_hello.go`), the QUIC Initial of
the udp carriers and h3 dialers (`net_quic_version.go`), and, as their branches
land, the extender camouflage carrier hello and the IPREAL egress SYN.

## Two layers, so CI stays hermetic

### Layer A — hermetic golden gate (normal `go test`, no Docker, no root)

A versioned golden fixture is captured once from a named Chrome version
(`testdata/fingerprints/chrome-<version>-clienthello.bin`). Fast deterministic
tests point connect's own dialer at the **same** local endpoint the golden was
captured from, capture the emitted bytes the same way, and compare the
**discriminating fields** — not raw bytes. GREASE (RFC 8701) is randomized per
connection and is normalized, never matched literally.

What Layer A gates today:

| Surface | Where | Status |
|---|---|---|
| TLS ClientHello (normal + resilient dialers) | `../net_tls_hello_conformance_test.go` | **wired** (merged) |
| QUIC Initial version/shape (QuicVersionPolicy) | `../net_quic_initial_conformance_test.go` | **wired** (merged); transport params + frame layout = next increment |
| Extender camouflage carrier hello | `../net_tls_hello_conformance_test.go` (`TestExtenderCamouflageHelloConformance`, `TestExtenderCamouflageCarrierHelloVsRealBrowser`) | **wired** (EXTENDER-UPDATE2 §3.4) — parrot gate vs the synthetic golden, and same-parrot-family vs the real golden |
| Real-browser hello (carrier + probe paths) | `real_golden_test.go`, `../extender/extender_camouflage_realhello_test.go` | **wired** — the committed real Chrome golden is fed through the actual peek, auth and splice paths |
| Egress SYN / IP TTL (JA4T) | `../net_tls_hello_conformance_test.go` (skipped) | placeholder — waits on the IPREAL egress impl (Layer B, root) |

The diff engine (`diff.go`) separates two kinds of field:

- **parrot fields** come from the uTLS Chrome profile: cipher suites, supported
  groups and their key shares (including the post-quantum `X25519MLKEM768`),
  signature algorithms, supported versions, the extension **set** and the
  GREASE / pre_shared_key structure. A drift here means the parrot fell behind
  real Chrome; the golden is ground truth.
- **dial-path fields** are connect's own, never Chrome's: the server name, the
  ALPN list, the application-settings (ALPS) extension and the pre_shared_key.
  connect sets these per dial path, so they are compared against what the path
  should present, not against the golden's one capture.

Chrome shuffles its **extension order** per connection, so the extension
comparison is set-based (sorted, GREASE-normalized) with structural GREASE/PSK
position checks — the JA4 approach. The cipher, group, signature-algorithm and
version lists, which Chrome does **not** shuffle, are compared in order
(GREASE-normalized): a changed GREASE value does not fail, a moved GREASE slot
does. Real Chrome 156 opens signature_algorithms with a GREASE value too, so
two captures of one Chrome diff clean there.

### Layer B — Docker refresh / drift alarm (opt-in; needs Docker, and root for SYN)

`capture/` (built behind the `fingerprint_capture` tag; `capture.sh` wraps it)
runs real headless Chrome from a pinned image against the same endpoint,
captures Chrome's TLS ClientHello and QUIC Initial **at the endpoint** (the raw
first flight on the accepted conn / the first UDP datagram — no tcpdump, no
root), regenerates the Layer-A goldens stamped with the Chrome version, and
diffs the current Chrome against the committed goldens, printing which fields
drifted. The TCP SYN + IP TTL layer is captured with tcpdump (this part needs
root).

## The shared local endpoint

`endpoint.go` (TLS/TCP) and `quic_endpoint.go` (QUIC/h3) are a TLS 1.3 + h2 +
http/1.1 (+ h3) server certified for the documentation name `endpoint.example`
by a private CA. It offers the default Go group set, which from Go 1.24 includes
`X25519MLKEM768`, so a client emits its post-quantum key share and completes in
one round trip. It records each connection's first flight before its server
reads it. Both real Chrome and connect's dialer hit it.

## Hard realities (honor these)

- **TLS ClientHello & QUIC Initial goldens = real Chrome, captured at the
  endpoint, hermetic.** High value, low cost. The QUIC long-header **version**
  is in the clear and is the primary udp drift signal (a version 2 Initial is
  not decrypted by the GFW/TSPU version 1 parsers; see `net_quic_version.go`).
- **QUIC transport parameters and CRYPTO-frame layout are inside the
  AEAD-encrypted Initial.** Reading them needs the Initial keys derived from the
  version salt and the destination connection id (RFC 9001 / RFC 9369). The
  wired QUIC test covers the version, the packet-type code and the 1200-byte
  padding, which are observable without decryption; the transport-parameter and
  frame comparison is the next increment.
- **TCP SYN / IP TTL (JA4T) ground truth is the OS KERNEL, not Chrome.** Docker
  shares the host (Linux) kernel, so Docker-Chrome yields ONLY the Linux SYN
  profile. Windows/macOS/Android/iOS JA4T goldens MUST come from real-OS
  captures or a published p0f/JA4T database. Do not claim Docker-Chrome produces
  a Windows SYN.
- **Chrome updates change the fingerprint by design.** Layer A pins a versioned
  golden; Layer B is the alarm that says "refresh the golden and the parrot." A
  drift failure is informative, not a flake — the message names the drifted
  field and the golden's Chrome version.

## Golden provenance

The committed golden `testdata/fingerprints/chrome-133-clienthello-synthetic.bin`
is **SYNTHETIC**: it is the uTLS v1.8.2 `HelloChrome_133` profile connect
parrots (`chromeClientHelloId` in `net_tls_hello.go`), generated by
`GenerateChromeHello`, **not** a capture of real Chrome. It is the parrot
reference the carrier-hello and dialer gates run against. It is **regression
coverage, not real-browser equivalence**: it pins "connect still matches the
uTLS Chrome_133 reference", which is not the same claim as "connect still
matches real Chrome."

The committed golden `testdata/fingerprints/chrome-156-clienthello.bin` **is**
a real-browser capture (EXTENDER-UPDATE2 §3.4): Google Chrome 156's ClientHello,
captured at the local fingerprint endpoint by the `-local-browser` mode of the
capture tool (no Docker, no root), with its version metadata in the sidecar
`chrome-156-clienthello.meta.json`. It is the real-browser evidence, exercised
offline by `real_golden_test.go` (the fixture is a genuine modern-Chrome hello)
and by `../extender/extender_camouflage_realhello_test.go` (the real hello is
fed through the extender's actual peek, authentication and splice paths). The
carrier-hello gate `TestExtenderCamouflageCarrierHelloVsRealBrowser` shows the
camouflage carrier drifts from real Chrome 156 by exactly the same fields the
normal dialers do — no extra tell from the camouflage layer.

Measured gap, uTLS `HelloChrome_133` parrot vs real Chrome 156 (recorded by
`TestSyntheticParrotIsNotRealBrowserEquivalent`): `signature_algorithms` (real
Chrome now sends a leading GREASE value and the ML-DSA scheme codepoints) and
the extension **set** (one added codepoint). The core fields a filter keys on
first — cipher suites, the group list and the post-quantum `X25519MLKEM768` key
share, supported versions, and the GREASE structure — still match, so the gap
is confined to those two fields; the test fails loudly if it widens into a core
field, which would be a real divergence calling for a parrot bump. This is why
the synthetic golden is **not** real-browser equivalence: a Chrome 156 visitor
and a connect dial differ in those two fields today.

Capture provenance: the real Chrome 156 golden was captured in this environment
with the `-local-browser` mode against the local endpoint. A Docker-Chrome
capture was not used (this host has no Docker available to the sandbox, and the
owner's rules forbid touching a shared stack); the local-browser path is the
hermetic, no-Docker, no-root equivalent and is the recommended refresh route on
a workstation. The runbook below covers both.

## Refresh runbook

Two routes capture the real TLS ClientHello golden: a **local browser** already
installed on the machine (no Docker, no root — the route used to capture the
committed Chrome 156 golden), or **Docker** Chrome. Both write
`testdata/fingerprints/chrome-<version>-clienthello.bin` plus a
`...-clienthello.meta.json` sidecar with the version metadata.

```sh
# From the connect module root.

# 0. Local-browser capture (no Docker): run a Chrome/Chromium already installed
#    on this machine against the local endpoint, read its version, write the
#    version-stamped golden + metadata, and diff vs the synthetic parrot golden.
#    "auto" finds Chrome/Chromium/Brave/Edge; or pass an explicit binary path.
go run -tags fingerprint_capture ./fingerprint/capture -local-browser auto
go run -tags fingerprint_capture ./fingerprint/capture \
    -local-browser "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"

# 1. Regenerate the committed synthetic golden (no Docker) — sanity only.
./fingerprint/capture/capture.sh --synthetic

# 2. Docker route: capture the real TLS ClientHello golden and diff vs the
#    committed golden. Writes testdata/fingerprints/chrome-<version>-clienthello.bin
#    and prints exactly which fields drifted.
./fingerprint/capture/capture.sh --chrome-version <version> --chrome-image <image@digest>

# 3. Also capture the QUIC Initial golden (forces Chrome onto h3).
./fingerprint/capture/capture.sh --chrome-version <version> --quic

# 4. The Linux SYN / IP TTL (JA4T) layer (root): capture the first SYN with
#    tcpdump and read its IP TTL + TCP options. Linux profile ONLY.
sudo ./fingerprint/capture/capture.sh --chrome-version <version> --syn
```

Then, in Layer A, point the gate at the new real golden (add a `GoldenRef` for
`chrome-<version>-clienthello.bin` in `golden.go` and use it in
`../net_tls_hello_conformance_test.go`), run `go test ./... ./fingerprint/`, and
if a parrot field drifted, bump `chromeClientHelloId` / the uTLS version in
`net_tls_hello.go` until the gate is green again.

## When Chrome drifts

The failure names the field and the golden's version, e.g.:

```
8 field(s) drifted from the chrome-133 synthetic golden (...):
  cipher_suites: got [...], want [0a0a 1301 ...]
  supported_groups.X25519MLKEM768: got absent, want present
  extension_grease_structure: got [...], want grease opens the extensions ...
```

That is the signal to refresh the golden (Layer B) and, if a parrot field moved,
bump the uTLS parrot. It is not a flake.
