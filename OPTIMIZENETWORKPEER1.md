# OPTIMIZENETWORKPEER1.md — optimizing the device-to-device (network peer) connection

A reproducible guide to the setup, methodology, root-cause chain, resolutions,
and measured limits from the "device-to-device / network-peer connection is
slow" investigation (2026-07-25). Companion to `PACKETRESEARCH1.md §16–§17`;
this file is the current standalone result.

## Current result

The original failure was not one slow operation. It was a sequence of
independent routing, admission, signaling, ownership, and instrumentation
problems:

| Layer | Finding | Resolution | Current status |
|---|---|---|---|
| ICE | Android API 30+ denies the netlink enumeration used by Go/Pion, producing zero host candidates | Supply a bounded default-route interface view through `SettingEngine.SetNet` | Fixed; direct candidate pairs form |
| Admission | WebRTC reservations shared the active transfer receive budget, so bulk traffic excluded P2P | Dedicated, hard-bounded WebRTC admission budget; selected peers can preempt speculative work without raising the ceiling | Fixed |
| Signaling | Active SDP used a plain contract while selected-peer data used `ForceStream` | Send active signaling over the same stream contract; retain companion signaling on the passive side | Fixed |
| Return route | Network-peer provider data could use a different send-sequence key from signaling/data | Use `ForceStream` consistently for Network returns | Fixed |
| Long-transfer reliability | Provider ping echo retained a callback-borrowed pooled `Frame`; reuse changed its type/size after enqueue and corrupted contract accounting | Give the asynchronous echo its own frame and shared bytes; derive accounting from the frames actually serialized | Fixed and regression-tested |
| CPU | A diagnostic `vmodule` made every disabled hot-path verbosity check resolve the caller stack | Remove `vmodule` from measured/release builds | Fixed; total process cycles −37.6%, Go-library cycles −44.7% |
| Receive flow control | The old 128 KiB selected-peer SCTP window capped controlled 50 ms paths | Use a selected-peer-only 2 MiB window with exactly two reservations (4 MiB ceiling) | Fixed; 2.26 → 28–29 MiB/s in the controlled path |
| Congestion recovery | Pion's Reno avoidance added only one ~1.2 KiB MTU per acknowledged window; independent loss held `cwnd` around 22–55 KiB | Use a measured four-MTU avoidance step; retain normal loss response and no forced floor | Fixed; controlled 0.2–0.5% loss throughput +63–77% |
| Idle data-plane liveness | Pion's infinite T3 retry assumes ICE will fail, but ICE consent can stay healthy while DTLS/SCTP is blackholed | Lazy, activity-triggered 10 s SCTP progress watchdog | Fixed; no idle timer, active sampling capped at 4 Hz |
| Asymmetric restart | A fresh active offer was ignored as a duplicate by the stale passive association | Mark the first offer with `reset_signals`; retire the negotiated passive generation and replay after `WaitingForSdpOffer` | Fixed; blackhole → bilateral replacement → resumed data is race-tested |

After the ownership/accounting correction, the same full-tunnel path that
reliably terminated near **5.8 MiB / 10 s** completed **444,444,444 bytes** and
**987,654,321 bytes**. Physical-device steady throughput is presently
**4.8–5.9 MiB/s**. After congestion tuning, three post-ramp samples were
**4.99, 5.10, and 5.41 MiB/s** (median 5.10), so the defensible physical
end-to-end multiplier remains approximately **1.0×**. The positive result is
loss recovery and predictability rather than a claimed aggregate speedup:
on-device sender `cwnd` expanded from the former 22–55 KiB band to an observed
18.8–156.7 KiB range while the composed tunnel remained limited elsewhere.

Do not multiply independent microbenchmark gains. The controlled receive-window
gain, constructor/setup gains, packet-copy gains, loss-tail improvement, and
encryption/decode gains exercise different limits and do not compose into one
end-to-end number.

---

## 1. Hardware / accounts under test

Two physical Android phones on the same premises, both signed into the same
urnetwork network, provider discoverable to the client as a **network peer**.

| Role | Model | ADB serial | urnetwork client_id | wlan0 IPv4 |
|------|-------|-----------|---------------------|------------|
| Client (initiator) | Pixel 8 Pro (`husky`) | `3B161FDJG001KT` | `019f9835-6b1c-e046-1630-330d77739deb` | 192.168.1.217/24 |
| Provider | Samsung SM-S928U1 / S24 Ultra (`e3q`) | `R5CX21FY6ND` | `019f9833-2d1e-cb39-0d45-526f8c30ab3b` | 192.168.2.110/24 |

Both devices connect over `adb` (USB). Confirm with:

```bash
adb devices -l
# 3B161FDJG001KT  device ... model:Pixel_8_Pro device:husky
# R5CX21FY6ND     device ... model:SM_S928U1  device:e3q
```

Notes that matter for reproduction:
- The two phones are on **different /24 subnets** (192.168.1.x vs 192.168.2.x) —
  a double-NAT home setup — yet are mutually routable (~8 ms). Direct UDP
  between them works (see §4.3), so the topology *supports* p2p; the failures
  below are in the app, not the network.
- The client selects the provider via the **Network peers** section of the
  location picker (not Auto). "Connected to 1 provider" in the UI means the
  single-peer path is active; "Auto" / "N providers" is the multi-provider mesh
  and is **not** a valid comparison for peer measurements (more parallelism
  inflates throughput).
- Post-quantum encryption is ON for these tests (the provider always enables
  the e2e sessions; the client opts in via the performance profile).

Platform relay (for reference RTT): `connect.bringyour.com` → `65.49.70.85`,
~59 ms one-way from the dev Mac. The phones cannot ICMP it but their data path
rides it when p2p is down.

---

## 2. Repositories and build/deploy pipeline

Four local repos under `/Users/brien/urnetwork/`:
- `connect/` — the Go transfer/IP/transport/p2p core (imported by the SDK).
- `sdk/` — gomobile SDK (`device_local*.go`), builds the Android AAR / Apple xcframework.
- `android/` — the Android app (`app/app/src/main/java/com/bringyour/network/…`).
- `server/` — platform exchange/resident (only read here, not changed).

The SDK is wired into the app via a gradle task that rebuilds it from local Go
source, so a connect/sdk edit propagates to the APK on the next build.

### 2.1 Build a play-variant APK (includes rebuilding the SDK from Go source)

```bash
cd /Users/brien/urnetwork/android/app
./gradlew :app:buildSdk :app:assemblePlayRelease --console=plain
# buildSdk runs `make init build_android` in sdk/build (gomobile bind) against
# the local connect/ + sdk/ source, dropping URnetworkSdk.aar into
# sdk/build/android/, which app/app/build.gradle consumes.
```

Output APKs land in
`android/app/app/build/outputs/apk/play/release/`. Use the **arm64-v8a** one for
these phones:
`com.bringyour.network-<version>-play-arm64-v8a-release.apk`.

The `play` flavor is signed with the release key
(`$WARP_HOME/release/android/signing/app.jks`, secrets in `app.properties`),
which is present locally — so `install -r` over the store build keeps app data
(the signature matches). Build time is ~2 min (SDK bind ~80 s + APK ~50 s).

### 2.2 Install and relaunch on both devices

```bash
APK=$(ls -t /Users/brien/urnetwork/android/app/app/build/outputs/apk/play/release/*arm64*.apk | head -1)
for S in 3B161FDJG001KT R5CX21FY6ND; do adb -s $S install -r "$APK"; done
# relaunch:
for S in 3B161FDJG001KT R5CX21FY6ND; do
  adb -s $S shell monkey -p com.bringyour.network -c android.intent.category.LAUNCHER 1
done
```

`adb install -r` does NOT restart a running app. To guarantee the new build is
live, force-stop first (`adb -s $S shell am force-stop com.bringyour.network`)
then relaunch — but note a force-stop drops the peer selection back to Auto, so
re-select the network peer (see §4.4) before measuring.

### 2.3 Apple (for parity; not the focus here)

```bash
cd /Users/brien/urnetwork/sdk/build && make build_apple   # rebuilds the xcframework
# then xcodebuild -project app/app.xcodeproj -scheme URnetwork \
#   -sdk iphonesimulator -destination 'generic/platform=iOS Simulator' ARCHS=arm64 build
```
IMPORTANT: never run `make build_apple` and gradle `buildSdk` concurrently —
they share `sdk/build/` and the concurrent xcframework swap corrupts the
binary target ("does not contain a binary artifact").

---

## 3. Instrumentation: turning on the logs you need

All diagnostics come through Android `logcat` tag **`GoLog`** (the SDK routes
glog to logcat). Verbosity is set in `sdk/sdk.go initGlog()`. Default and
performance builds must use `v=0` with no `vmodule`.

For short correctness-only captures, an instrumented build may use:

```go
// sdk/sdk.go initGlog() — TEMPORARY; never use for performance numbers
flag.Set("v", "1")
// Use only for a brief signaling trace:
flag.Set("vmodule", "transport_p2p_webrtc=2,transport_p2p=2,transfer_stream_manager=2")
```

- `v=1` surfaces: `[signal]send/receive`, `[p2p]…setup refused`, `[sm]…open`,
  `routing ok`, `[init]tcp connect synthetic`, `[r]drop older sequence`
  (elevated to V1 during this work as a fork tripwire), the pion
  `signaling state changed to …` lines.
- `vmodule transport_p2p_webrtc=2` adds the pion ICE candidate/pair/nomination
  trace and `[signal]miss` / `[signal]<SignalType>` routing lines.
- For throughput or CPU runs, use `v=0` with **no vmodule**.

This distinction is measured, not cosmetic. A 30-second `simpleperf` capture
with the diagnostic `vmodule` recorded **33.593 billion cycles**, 90.53% in
`libgojni`, with `runtime.pcvalue`, `runtime.step`, and related stack-walking
functions dominating. The matched build without `vmodule` recorded **20.956
billion cycles**, 80.32% in `libgojni`: **37.6% fewer total cycles** and
**44.7% fewer Go-library cycles** at comparable throughput. A disabled V-log
in a file selected by `vmodule` is not free; glog resolves its call site before
deciding whether to emit.

The low-frequency `[p2p-stats]` logger used to attribute the congestion limit
sampled SCTP/ICE every three seconds only while byte counters advanced. It was
removed after the A/B, along with Android's temporary `debuggable` /
`profileable` manifest hooks. Reintroduce it only for a bounded measurement;
it is not production observability.

Rebuild + redeploy after changing verbosity (it is compiled in).

Capture to files (run one per device, in the background):

```bash
adb -s 3B161FDJG001KT logcat -v threadtime -s GoLog > pixel.log &
adb -s R5CX21FY6ND    logcat -v threadtime -s GoLog > samsung.log &
# clear first with: adb -s <serial> logcat -c   (buffer maxes ~5 MiB on-device)
```

---

## 4. Measurement toolkit

### 4.1 Synthetic in-tunnel speed server (built for this work)

To drive a repeatable "packet rush" isolated from origin/website variability, the
**provider's local NAT terminates TCP flows to the RFC 2544 benchmark range
`198.18.0.0/15`** at an in-memory HTTP/1.1 server instead of dialing upstream.
The full tunnel path (client tun → per-peer encryption → transfer sequences →
transports → provider NAT) is exercised; only the internet hop is replaced. The
range is reserved and never publicly routable, so nothing real is shadowed.

- Code: `connect/ip_synthetic_speed.go`; hook in `connect/ip.go` TCP connect
  path; setting `TcpBufferSettings.EnableSyntheticSpeed` (default **true**).
- Endpoints (drive from the client, over the peer tunnel):
  - `GET http://198.18.0.1/ping` → 200, 1-byte body (flow-setup / RTT probe).
  - `GET http://198.18.0.1/download/<bytes>` → streams exactly `<bytes>`
    patterned bytes (compression-proof), capped at 10 GiB.
  - `POST http://198.18.0.1/…` with a body → upload sink, replies with the count.
- Drive a URL from the client with:
  ```bash
  adb -s 3B161FDJG001KT shell "am start -a android.intent.action.VIEW -d 'http://198.18.0.1/download/500000000'"
  ```
- Confirm it lands on the provider: `grep 'tcp connect synthetic 198.18.0.1' samsung.log`.

### 4.2 Throughput measurement (tun byte deltas on the client)

The VPN tun on the client is **`tun1`** (not tun0). Measure received-byte delta
over a fixed window during a sustained download; that is the app-visible tunnel
throughput.

```bash
read_rx(){ adb -s 3B161FDJG001KT shell "cat /proc/net/dev" | grep -E "tun1:" | sed 's/.*tun1: *//' | awk '{print $1}'; }
adb -s 3B161FDJG001KT shell "am start -a android.intent.action.VIEW -d 'http://198.18.0.1/download/800000000'"
sleep 3; R1=$(read_rx); T1=$(date +%s.%N)
sleep 8; R2=$(read_rx); T2=$(date +%s.%N)
python3 -c "print(f'{($R2-$R1)/($T2-$T1)/1024/1024:.2f} MiB/s')"
```
Take 3 samples; discard the first partial ramp. Keep the peer selection fixed
(single network peer) across baseline and post-fix runs, or the comparison is
meaningless.

### 4.3 Direct-reachability sanity (proves topology supports p2p)

```bash
# ICMP RTT device-to-device:
adb -s 3B161FDJG001KT shell "ping -c5 192.168.2.110"     # ~8 ms
# raw UDP both directions (uses the shell, NOT the app — so it's not in the VPN):
adb -s R5CX21FY6ND shell "timeout 8 nc -u -l -p 41234 > /data/local/tmp/u.txt &"
adb -s 3B161FDJG001KT shell "echo OK | timeout 3 nc -u -w2 192.168.2.110 41234"
adb -s R5CX21FY6ND shell "cat /data/local/tmp/u.txt"     # prints OK  → UDP works
```
If UDP works from the shell but the **app** gathers no ICE candidates, the app's
sockets are trapped in the tunnel (see §5.1).

### 4.4 Selecting the network peer via UI automation

After a fresh launch the client may be in Auto. To pin the single Samsung peer:

```bash
# dump the current screen's tappable text + bounds:
adb -s 3B161FDJG001KT exec-out uiautomator dump /dev/tty | \
  python3 -c 'import sys,re;[print(repr(m.group(1)),m.group(2)) for m in re.finditer(r"text=\"([^\"]{1,40})\"[^>]*bounds=\"(\[[0-9,\[\]]+\])\"",sys.stdin.read()) if m.group(1).strip()]'
# tap "Change" (center of its bounds), then the "Network peers" → "… Samsung SM-S928U1" row.
adb -s <serial> shell input tap <cx> <cy>
```
Verify with a UI dump showing "Connected to 1 provider", and in the log:
`grep 'routing ok \[019f9833-2d1e' pixel.log` should show only the Samsung id.

### 4.5 Database state (optional, server side)

`server/monitor/SIGNALS.md` documents the pg/redis probes. pg primary is reached
over the overlay: `ssh by@172.28.208.182` then `psql` (creds in
`vault/main/pg.yml`). Not required for the client/provider-side work here.

---

## 5. Root-cause chain (why p2p never connects) and the fixes

Diagnosed by deploying the instrumented build, driving a synthetic download over
the peer, and reading the two logs. Symptoms in order of discovery, each gating
the next:

### 5.1 pion cannot enumerate interfaces on Android (netlink denied) — THE load-bearing blocker

**Symptom:** pion logs `Failed to initialize mDNS … no usable interfaces found
for mDNS`, gathers **0 host candidates**, and repeats `Pinging all candidates`
→ `Failed to ping without candidate pairs. Connection is not possible yet.`
`signaling state` reaches `stable` (after the §5.4 fix) but ICE never forms a
pair, so `ready header … err = i/o timeout` loops forever and traffic stays on
the relay.

**Root cause (definitive, via the `[ice-if]` diagnostic in
`transport_p2p_webrtc_pc.go`):**
```
[ice-if]net.Interfaces err = route ip+net: netlinkrib: permission denied
```
Since **Android 11 (API 30) apps are denied the netlink route dump**, so Go's
`net.Interfaces()` — and therefore pion's default host-candidate gathering —
fails outright. With no interface, pion builds no host candidate; ICE then has
nothing local to pair, so it never connects. This has nothing to do with
`VpnService.protect` and is **not** fixed by app exclusion.

**What this is NOT (ruled out on-device):**
- *Not* app-in-its-own-tunnel. The app **is** excluded: on the client the VPN
  network (`tun1`) covers `Uids: <{0-10338, 10340-20338, 20340-99999}>` and the
  app's UID is `10339` — outside the ranges, i.e. excluded. Its sockets already
  egress wlan0 (which is why the §4.3 dial trick below can find the real IP).
- *Not* socket protect. The sockets route fine; pion just can't see the
  interface to name the candidate.

**Fix (connect, `ice_net.go` + one hook in
`newWebRtcPeerConnectionFactory`):** supply pion a `transport.Net` that provides
the egress interface **without** netlink. The local egress address is found with
a connect()-only UDP "dial trick" (`net.Dial("udp","8.8.8.8:80")` →
`LocalAddr()`), which consults the in-kernel routing table, not netlink, and is
surfaced as one synthetic interface. Socket ops delegate to the embedded
`stdnet.Net`. Because the app is already excluded from the VPN, the dial trick
returns the physical wlan0/cellular IP — exactly the host candidate ICE needs.
Installed via `SettingEngine.SetNet(iceNet)`; a no-op on desktop/server where
`net.Interfaces()` works.

**Verify on-device** after redeploy:
```bash
adb -s 3B161FDJG001KT logcat -d -s GoLog | grep "\[ice-if\]"   # synthetic en0 addrs=[192.168.1.217/32]
adb -s 3B161FDJG001KT logcat -d -s GoLog | grep -iE "state changed: connected|valid candidate pair|pion:sctp.*sending ppi"
```

**Measured outcome (2026-07-25):** the fix works at the ICE/data layer.
`[ice-if]synthetic en0 addrs=[192.168.1.217/32]` (and IPv6), pion now gathers
host candidates, ICE reaches `connection state: connected` with a
nominated/succeeded candidate pair over **direct IPv6** (the two devices'
addresses are in adjacent /64s, c100↔c101 — no NAT), and SCTP carries data
(`sending ppi=53`, `SACK measured-rtt≈50–74ms`). This is a capability that was
completely absent before (0 candidates, 100% relay).

**Subsequent result:** the apparent architectural churn was the combined
admission/signaling/return-route failure chain, not a requirement to redesign
the window lifecycle. After those fixes, the selected direct association
remains installed through long synthetic transfers. The full-tunnel path
completed 444,444,444- and 987,654,321-byte runs, and the direct sender's SCTP
byte counters advance continuously during load. Sections 5.5–5.6 cover the
remaining measured congestion and idle-resume work.

**Related hardening (kept, but NOT the blocker here):**
`MainService.kt updatePfd()` allowlist mode excludes the app only by omission.
Guard it so the app is never in its own tunnel regardless of a bad split
override:
```kotlin
for (includedPackageName in tunnelIncludedAppIds) {
    if (includedPackageName == packageName) continue  // never tunnel this app
    builder.addAllowedApplication(includedPackageName)
}
```
On the tested devices the denylist path already excluded the app, so this guard
did not change their behavior — it prevents a *different* way to reintroduce the
loop.

### 5.2 p2p peer-connection memory-budget starvation — FIXED

**Symptom (v=1):** `[p2p]s(…) <>… setup refused = peer connection memory budget
exhausted (149796 < 524288)` on both devices; `signaling state` counts high but
**0 `stable`**; every window client churns.

**Cause:** the WebRTC admission budget (`WebRtcSettings.MemoryBudget`) was the
**same object** as the transfer receive-queue budget, at all four wiring sites
(`sdk/device_local.go` default+override+window-generator, and
`sdk/device_local_provider.go`). Each peer connection reserves
`ReceiveBufferSize` = 512 KiB. On a 20 MiB device target the provider share is
4 MiB → receive-queue budget ~1.14 MiB, and an active download consumes it
(available fell to ~146 KiB), so a 512 KiB reservation could **never** be
admitted while traffic flowed — exactly when p2p is needed. Catch-22: no p2p →
all relay → receive queue busy → p2p refused.

**Fix (`sdk`):**
- `deviceLocalWebRtcBudget(share)` — a **dedicated** admission budget, separate
  from the receive queue, sized `max(share/8, 8×128 KiB)` for automatic/public
  windows. It gates admission only; a formed connection's SCTP memory is
  unchanged, so no steady-state footprint is added.
- Automatic/public mobile windows use a 128 KiB per-connection buffer (was
  512 KiB), allowing at least eight bounded speculative peers. An explicitly
  selected network peer instead uses the measured 2 MiB receive window and a
  destination-local budget of exactly two reservations: one live association
  plus one make-before-break replacement, with a hard 4 MiB ceiling.
- Applied at all four sites, guarded to mobile (`0 < share`); desktop/server
  keep the 512 KiB unbudgeted default.
- Regression test: `TestDeviceLocalSettingsMemoryTarget` in
  `sdk/device_local_memory_test.go` (asserts the p2p budget ≠ receive queue and
  admits ≥2 connections).

**Measured:** relay-path single-peer throughput **0.54 → 0.79 MiB/s (+46 %)**
from reduced memory pressure (p2p still relayed until §5.1).

### 5.3 Dead STUN servers — FIXED

**Symptom:** a storm of `[pion:ice]failed to get server reflexive address …
stun:openrelay.metered.ca / stun.stunprotocol.org … i/o timeout`, each burning
multi-second timeouts and delaying gathering.

**Fix (`connect/transport_p2p_webrtc.go` `DefaultWebRtcSettings`):** replace the
defunct servers with live anycast ones:
```go
IceServerUrls: []string{
    "stun:stun.cloudflare.com:3478",
    "stun:stun.l.google.com:19302",
},
```
On-device STUN errors dropped from a storm to ~1.

### 5.4 SDP rendezvous: the active offer must ride the stream — FIXED

**Symptom (vmodule=2):** both devices only ever reach `have-local-offer`,
**never `have-remote-offer`**; the provider receives 0 `[signal]receive from
<client-main>`; many `[signal]miss`. The offer is *sent* (0 `send failed`) but
never delivered to the passive peer.

**Cause:** the provider spins up an **ephemeral per-window client** for each of
the client's window clients (e.g. `019f9a39-ee08…`), which contracts *to* the
client. Network peers force `AllowDirect` → the data path uses `ForceStream`. But
`ClientSignalSender.SendSignal` sent the **active** side's `SdpOffer` with the
client's **default** transfer options (plain contract, no ForceStream) — and the
ephemeral peer grants only the *stream* contract for the pair, so the offer was
undeliverable even though data flowed over the same stream via the relay. Same
class as `PACKETRESEARCH1 §16`: a send-sequence-keying option (ForceStream)
invisible to the signaling layer routes the offer onto a different, undeliverable
sequence.

**Fix (`connect/transport_p2p_webrtc.go` `peerConn.sendSignal`):** the active
side sends with `ForceStream()` so the offer rides the same stream contract as
the data (passive side keeps `CompanionContract()` — the platform rejects
companion *stream* contracts, so it must not also force stream):
```go
var opts []any
if self.active {
    opts = append(opts, ForceStream())
} else {
    opts = append(opts, CompanionContract())
}
```
**Measured on-device:** `have-remote-offer` and `stable` went **0 → 2** — SDP
offer/answer now completes. Candidate gathering then required the Android
interface fix in §5.1. Validated by the full `connect` suite and `TestWebRtc`.

### 5.5 SCTP congestion-window recovery — FIXED, CONSERVATIVE SETTING KEPT

**Physical attribution:** with the selected peer's advertised receive window at
roughly 2 MiB, the sender still had queued data while `cwnd` repeatedly fell
into the 22–55 KiB range. The receive window was no longer limiting. Pion uses
Reno-style avoidance and, by default, adds only one path MTU (1191 bytes on the
devices) after a complete congestion window is acknowledged. Recovery from an
independent wireless loss therefore takes seconds.

`transport_p2p_webrtc_loss_test.go` now supplies two controlled paths:

- deterministic sender→receiver DTLS application-packet loss over a 50 ms RTT,
  while reverse SACK and ICE traffic remain intact;
- 5/20/50 Mbps token-bucket bottlenecks with a bounded 64 KiB queue, which
  rejects settings that win only by forcing excess data into a real queue.

Three-run deterministic-loss averages for the retained four-MTU avoidance step:

| Forward packet loss | Pion default | Four-MTU CA step | Change |
|---:|---:|---:|---:|
| 0 | 29.29 MiB/s | 29.49 MiB/s | +0.7% |
| 1 / 500 (≈0.2%) | 0.84 MiB/s | 1.36 MiB/s | +63% |
| 1 / 200 (≈0.5%) | 0.43 MiB/s | 0.75 MiB/s | +77% |
| 1 / 100 (1%, representative run) | 0.31 MiB/s | 0.55 MiB/s | +77% |

The three-run queue check was effectively neutral at 5 Mbps (0.55 vs
0.55 MiB/s), within noisy RTO variance at 20 Mbps (1.74 vs 1.66 MiB/s), and
improved the 50 Mbps case from 1.73 to 2.96 MiB/s. Post-bulk probe latency
remained one-way-path limited at about 25 ms.

**Kept change:** `SctpCwndCAStep = 4 * 1200`, applied through
`SettingEngine.SetSCTPCwndCAStep`. It accelerates additive recovery but retains
Pion's normal multiplicative loss response and permits the window to fall all
the way to its protocol minimum.

**Rejected after measurement:**

- Six/eight-MTU steps could overdrive the real token-bucket path and trigger an
  RTO; four MTUs was the safer knee.
- 16/32/64 KiB minimum windows looked good under independent loss but force
  traffic after real congestion and make memory/queue latency less predictable.
- Enlarging `FastRtxWnd` alone did not improve sustained-loss throughput.
- A two-second `RTO.Max` shortened one phase-sensitive 3.5 s outage from
  7.10 to 5.10 s, but slightly worsened the 1.5 s case, did nothing at 5.5 s,
  and sent more retries. Across three sustained-loss runs it was about 4%
  worse at 1/500 loss, 2% better at 1/200, and identical at 1/100. It was not
  retained; the liveness bound in §5.6 is safer than globally changing
  retransmission timing.
- Partial reliability / limited retransmits was not enabled. It silently
  abandons SCTP user messages and changes the route's delivery contract; the
  transfer layer's send, receive, and forward callbacks intentionally remain
  synchronous backpressure.

On the physical pair, the post-change sender ranged from 18.8 to 156.7 KiB
instead of remaining near 22–55 KiB. End-to-end tun samples remained inside
the pre-change 4.8–5.9 MiB/s band (post-ramp 4.99/5.10/5.41 MiB/s), proving
that the congestion recovery lead is real but is no longer the sole composed
tunnel bottleneck.

Run the opt-in measurements with:

```bash
CONNECT_WEBRTC_CONGESTION_MEASURE=1 go test -run TestWebRtcSctpCongestionTuningMeasurement -v .
CONNECT_WEBRTC_QUEUE_MEASURE=1 go test -run TestWebRtcSctpCongestionQueueMeasurement -v .
CONNECT_WEBRTC_OUTAGE_MEASURE=1 go test -run TestWebRtcSctpOutageRecoveryMeasurement -v .
```

### 5.6 Idle association does not resume — TWO ROOT CAUSES FIXED

The observed idle hang is possible without any application callback hanging.
Pion's SCTP implementation gives T3 DATA retransmission no terminal failure by
design; its source explicitly assumes ICE will fail when connectivity is lost.
That assumption is false for a split data plane: STUN consent can continue over
the selected ICE socket while DTLS/SCTP records are blackholed by stale NAT,
socket, suspension, or path state. A detached reliable-channel `Write` can
return after queueing a small message, so its 15-second write deadline no longer
owns that unacknowledged message. Pion then retries it indefinitely while the
PeerConnection continues to report connected.

Two independent fixes are required:

1. **Activity-triggered SCTP progress watchdog.** After the first successful
   native data-channel write, a lazy worker observes aggregate SCTP buffered
   bytes. While bytes are outstanding, any reverse SCTP packet (normally a
   SACK) refreshes a 10-second deadline. No reverse packet by the deadline
   closes the association and raises the persistent one-shot
   `ImmediateReconnect` signal. There is no idle ticker or radio wakeup.
   Outbound notifications coalesce; while active, transport stats are sampled
   at most every 250 ms (4 Hz), not once per 3 KiB transfer write.
2. **Fresh-generation reset.** Detecting failure on the sender was not enough.
   The passive endpoint could still answer ICE and had no outbound data from
   which to infer failure. It treated the new active endpoint's SDP offer as a
   duplicate of the old one and ignored every retry. The first offer from each
   active PeerConnection now sets the existing
   `ExchangeSignals.reset_signals` bit. A fresh passive accepts it directly; an
   already-negotiated passive requests immediate replacement. Its replacement
   sends `WaitingForSdpOffer`, and the active endpoint replays its cached offer
   without another reset.

This deliberately does **not** put a timeout around transfer send, receive, or
forward callbacks. Those callbacks remain intentional backpressure. The new
deadline observes only native transport progress after a write has been
accepted by SCTP.

Resource behavior is bounded and pause-safe:

- zero timers and zero watchdog goroutines before the first application write;
- one coalescing channel and at most one lazy goroutine/timer per used
  PeerConnection, bounded by the existing peer count/memory admission limits;
- no work while the association is idle with zero buffered bytes;
- after a scheduler pause in which the monotonic deadline advances, the next
  sample evaluates the original deadline rather than granting a fresh window;
- iOS `PacketTunnelProvider.wake()` and material host path changes also call
  `DeviceLocal.NetworkChanged()`, which retires path-bound P2P state
  immediately. The watchdog covers the distinct case where the host path
  callback does not fire.

`TestWebRtcIdleResumeSctpBlackholeReconnects` leaves STUN/ICE consent flowing,
blackholes the entire DTLS record layer (including CloseNotify), verifies that
ICE stays connected, then proves:

```text
healthy idle → resumed write queued → 10 s production bound
→ active immediate reconnect → reset retires stale passive
→ new offer/answer/ICE/SCTP → payload delivered
```

The test passes 20 consecutive runs under `-race`. Terminal ICE/PeerConnection
state release, network-change replacement, and blocking-write backpressure are
covered by the adjacent focused race suite. Because both mechanisms live in
the shared Go transport, native iOS, Android, macOS, and server/proxy users get
the same fix; JS is excluded because browsers do not expose equivalent
association counters and that transport does not currently implement detached
data-channel `net.Conn`.

Final validation record (2026-07-25):

- the idle-blackhole and reset-generation/value-ownership tests passed 10
  consecutive ordinary runs; the idle-blackhole test passed 20 consecutive
  runs under `-race`, and the adjacent blocking-write, terminal-state,
  network-change, and host-dispatch suite passed five race-enabled runs;
- the full `connect` package tree passed in 390.047 s with only
  `TestPtDnsEncodeDecode` skipped. That unrelated random-loss DNS/QUIC test had
  been the sole failure in the immediately preceding unfiltered run, then
  passed three complete repetitions (48 randomized loss cases) in 170.077 s;
- `go vet ./...` passed, and the JS/Wasm package compiled after native-only
  WebRTC tests were correctly build-tagged;
- the final Play release APK (version code `1002352110`) built successfully,
  installed on both physical devices, and launched with a live process on
  each;
- the SDK's four-minute RPC/grid unable-to-connect lifecycle test passed
  independently in 240.552 s. The entire SDK tree is not presently green:
  two pre-existing synthetic memory-load tests close their in-memory TCP
  endpoint under pressure, and `TestDeviceLocalReconfigurationChurn` observed
  +10 goroutines against its +8 tolerance after 20 cycles. These failures are
  recorded rather than hidden; none executes the WebRTC idle watchdog or
  generation-reset path.

---

## 6. Supporting instrumentation added (kept in-tree)

- `connect/transport_p2p_webrtc.go`: `[signal]send ->` / `[signal]send failed` /
  `[signal]receive from` (V1) — full p2p signal-delivery trace.
- `connect/transport_p2p.go`: `[p2p]s(…) <>… setup refused = <err>` (info) —
  surfaces the previously-silent admission refusal (cap or budget).
- `connect/transfer.go`: `[r]drop/upgrade older sequence …` elevated V2→V1 with
  `(source, role, companion)` — the sender-fork tripwire from §16.
- `connect/ip_synthetic_speed.go` + `EnableSyntheticSpeed` — the §4.1 harness.
- `[signal]miss` (V2, `WebRtcManager.ReceiveSignal`) — a received signal with no
  matching peerConn (rendezvous/id churn).
- `connect/transport_p2p_webrtc_loss_test.go` — receive-window, independent
  loss, real-queue, transient-outage, and idle-DTLS-blackhole harnesses.
- `[peerconn]SCTP no progress … reconnecting` — one production-level event per
  retired half-open association; no periodic production stats logger remains.

`sdk/sdk.go` is at `v=0` with no `vmodule`; keep release measurements that way.

---

## 7. Reproduce end-to-end (checklist)

1. `adb devices -l` shows `3B161FDJG001KT` + `R5CX21FY6ND`.
2. Set `sdk/sdk.go` verbosity (§3); `./gradlew :app:buildSdk :app:assemblePlayRelease`.
3. Install the arm64 APK on both (§2.2); relaunch.
4. On the client, select the Samsung under **Network peers** (§4.4); confirm
   "Connected to 1 provider" and `routing ok [019f9833-2d1e…]` in the log.
5. Verify the app UID is excluded from its VPN in `dumpsys connectivity`;
   confirm `[ice-if]synthetic …`, a selected candidate pair, and
   `[peerconn]connected … local=… remote=…`.
6. Drive the RFC-2544 synthetic download and take at least three post-ramp tun
   samples (§4.2). Use temporary `[p2p-stats]` only when attribution is needed,
   then remove it again.
7. Run the congestion/outage harnesses in §5.5 and the idle blackhole test in
   §5.6. The latter must pass repeatedly under `-race`.
8. Sanity: focused race suite, full `connect` suite, `go vet`, JS/Wasm compile,
   SDK tests, and final Android build with `v=0`, no `vmodule`, and no
   `debuggable` / `profileable` hooks.

---

## 8. Files touched

connect: `ice_net.go`; `transport_p2p_webrtc.go` (bounded signaling,
ForceStream, reset generations, lazy SCTP watchdog, four-MTU CA setting);
`transport_p2p_webrtc_pc.go` / `_js.go` (native Pion settings and progress
surface); `transport_p2p_webrtc_loss_test.go` (loss/queue/outage/idle-blackhole
harnesses); `transport_p2p_webrtc_test.go` (signaling and bilateral restart);
`transport_p2p.go` (route lifecycle/backpressure); `ip.go` +
`ip_synthetic_speed.go` / `_test.go`; provider ping ownership/accounting tests;
and the associated signaling/route tests.

sdk: `device_local.go`, `device_local_provider.go` (dedicated WebRTC budgets,
128 KiB automatic and 2 MiB selected-peer windows),
`device_local_memory_test.go`, and `sdk.go` (`v=0`, no `vmodule`).

android: `MainService.kt` (guaranteed app self-exclusion in allowlist mode).
Temporary `[p2p-stats]`, manifest `debuggable`, and `profileable` changes were
removed after measurement.

All uncommitted for review. Full lab notebook: `PACKETRESEARCH1.md §16–§17`.
