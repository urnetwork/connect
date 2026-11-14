# MULTIFIX3: leaving dead exits in seconds

Status: approved by the owner on 2026-11-14 at about 01:35Z ("go ahead and
fully implement all 5"). In progress. Each fix lands on main with
deterministic root cause tests that fail before the fix and pass after it, as
CODESTYLE.md requires. This document is updated with the merge SHAs and test
names as each fix lands (section 6).

## Source

The owner's iOS report of 2026-11-13: the app was "slow, lots of pauses in
loading content", with repeated "Can't reach URnetwork" banners.

- **Logs:** uploaded at 21:25Z. They cover two runs of the VPN extension
  (20:48:28 to 21:07:28Z and 21:07:44 to 21:25:24Z) plus the app process.
- **Device build:** nightly 2026.11.13+1097983310 (connect 7e1ef70b, sdk
  967c7070).
- **Device settings:** post-quantum off, the default DNS preset, IPv4-only
  Wi-Fi, provide off.
- **Comparison baseline:** stable v2026.10.27-1083160450.
- **Line numbers:** from connect f0dae96e unless noted. They may have moved
  since.

## 1. What the logs show

Ranked by how much each contributed to the pauses.

1. **Exits that stop answering keep their flows until a timer fires.** Then
   every flow on them is reset.
   - 40 exit removals in 37 minutes reset 1,567 flows.
   - 15 exits hit the 30 s send-ack lifetime. Each message was sent 7 to 9
     times and never acked. Those removals reset 1,095 flows (70%), each after
     a stall of up to 30 s.
   - 7 blackhole verdicts, each after about 10 s, reset 72 flows.
   - 4 receive gaps froze an exit's whole return stream for 60 s each
     (`GapTimeout`, `transfer.go` around :1110). One of those exits carried 65
     flows.
   - The timer values haven't changed since stable: window `AckTimeout` is
     30 s, `GapTimeout` 60 s and the DoH budget 60 s.
2. **DNS rides the same exits.**
   - **Timeouts:** in-tunnel DoH timed out on 19% of A/AAAA lookups and 31%
     of other types in run 1, and on 9% and 13% in run 2. Each timeout is a
     60 s wait. A and AAAA get no reply at all; other types get SERVFAIL.
   - **They follow exit failures:** 96% and 94% of the timeouts fall in minutes
     with an exit failure. DoH sent from the extension over Wi-Fi had no
     failures, so the resolvers themselves are fine.
   - **Why one stall costs 60 s:** the per-request timeout equals the flight
     budget (`ip_mux_upgrade.go` `ResolveTimeout` 60 s, `dohRequestTimeout`
     around :525). Retries happen only after an early return
     (`resolveTunnelDoh` around :2574), so one stalled exit spends the whole
     budget.
3. **Two whole-tunnel rebuilds** came 30 to 50 ms after the app called
   `SetPerformanceProfile`, at 20:53:31Z and 21:08:55Z. Together they removed
   17 exits and reset 400 flows, 26% of all resets.
4. **The iOS extension sits at its memory edge.**
   - With the 32 MiB Go soft limit, phys_footprint held at 35 to 40 MiB, and
     the GC ran continuously.
   - At 21:07:28Z the log stops about 5 s after a 48.3 MiB peak, which is
     consistent with a jetsam kill. It cost about 30 s without service.
   - After the restart, a restored exit that was already stalling before the
     kill absorbed SYNs for 10 s.
5. **Contract waits on new exits:** p50 216 ms, p90 2.3 s, max 15.0 s (n=73).
6. **Memory-pressure frees:** 669 `FreeMemory` calls, with 8.1 s of forced GC
   in total. A minor factor.
7. **Server side during the session.** These have since been resolved or are
   being fixed.
   - The GOMAXPROCS A/B drains force-closed client transports on three hosts.
   - The edge-0 A/B instance refused contract-allowance forwards on Redis
     errors from 20:40 to 21:35Z.
   - A hosted-proxy demand surge from a bot wave raised client transports from
     157k or less to 266k.

   The A/B ended and was reverted on 11-13. The bot-wave fixes are a
   per-source sign-up limit and hosted proxies that open their device on first
   use.
8. **The banners** in these logs came from the app's own api calls through the
   tunnel: 11 failed dials at 7 separate moments. Each one raised the banner on
   a single failure.

## 2. Already landed (2026-11-13/14)

| Change | Merge | Effect |
|---|---|---|
| Platform-unreachable hold | connect 0d6d5d00 | A window waits 30 s before it reports platform-unreachable, so a connect restart or one platform timeout no longer raises the banner |
| Can't-reach debounce | sdk edf2342d | The banner needs 20 s of can't-reach failures within 60 s, with no answer from URnetwork in between |
| FreeMemory coalescing | sdk 05477a90 | At most one forced release every 30 s |
| Restored exclusions | connect f9439015 | A removed provider never comes back through a restored identity |
| Weighted race merge | connect bc776f20, sdk d47cccdf | One merged race order, with each window's top first. The limited window is the same size as the normal one; iOS stays at 2 |

## 3. The five fixes

### Fix 1: leave a dead exit in seconds

**Root cause.** A stalled exit keeps its flows and its DNS until a 30 s or
60 s timer fires (section 1, items 1 and 2).

**Change: DNS**
- **Per-request cap:** once the tunnel is warm, each DoH request is capped at
  4 to 5 s inside the flight, then retried through another exit. The 60 s
  budget still applies to the whole flight, and to a cold tunnel.
- **Connection timers:** in-tunnel DoH dial 30 s to 5 s, TLS 30 s to 8 s,
  HTTP/2 health check 30+15 s to 10+5 s.
- **Stale answers:** in the RFC 8767 style, a cached answer (even an expired
  one, within a bounded window) is served after about 1.5 s when the live
  lookup hasn't returned. The refresh continues in the background.
- **More caching:** HTTPS/SVCB answers are cached like A/AAAA.
- **TTL to the OS:** the upstream TTL is passed to the OS resolver, capped at
  300 s instead of 60 s.
- **Bug fixed:** `attachDnsResponder` checks the 96-question cap before the
  cache, so cached names are dropped at the cap. The cache is now checked
  first.
- **Bug fixed:** IPv6 DoH endpoints were dialed through exits with no IPv6. In
  run 1 that gave 47 timeouts against 50 successes, at 30 s each. Endpoints
  now follow the exit path's IP family.

**Change: exits**
- **Timers:** window `AckTimeout` 30 s to 10 to 15 s, and `GapTimeout` 60 s to
  15 to 20 s with an eviction notice, so flows re-home promptly. This applies
  to iOS through the sdk's mobile policy, and to Android and desktop unless a
  reason not to turns up. Providers and hosted proxies keep their values.
- **Flow placement:** new flows and affinity donation stay off any exit whose
  receive gap is older than about 3 s, or that has made no ack progress for
  3 s or more. The exit is used again once progress resumes.

**Guard.** These rules act only on exits that have stopped making progress,
never on exits that are merely slow, and only once the tunnel or window is
warm.

**Root cause tests.** Each one fails on main and passes with the fix:
- On a warm tunnel, a stalled exit holds a lookup for the whole 60 s budget;
  it must now answer through another path within 5 s.
- What the OS resolver receives for A/AAAA, and when, during a stall.
- Cached names are dropped at the 96-question cap.
- A v6 endpoint is dialed through a v4-only exit.
- No stale answer is served while the live lookup hangs.
- The OS gets a 60 s TTL cap.
- An exit that never acks holds its flows for 30 s.
- A receive gap freezes the exit for 60 s.
- New flows keep landing on an exit stalled for 3 s or more.

Guards that pass both before and after: the cold-tunnel budget, and a slow
but progressing exit that is never evicted.

### Fix 2: immediate ACKs only for young holes

**Root cause.**
- connect ba795437 (2026-11-10) makes providers deliver pure ACKs and SYN-ACKs
  immediately, ahead of a receive gap (`ip.go` around :9632 and :9752;
  `ip_immediate_ack.go`).
- It has no effect yet, because providers still run the 10-27 release.
- Once providers update, an exit wedged behind a gap will still complete new
  handshakes. It lifts its quarantine and looks healthy for up to 60 s while
  its data flows stall, which would undo fix 1.

**Change.**
- A provider delivers immediately only while the hole is at most 2 s old.
  After that, delivery goes back to in order.
- On the client, receives made only of immediately delivered control packets
  don't count as progress for health, quarantine, stall detection or
  readiness.

**Root cause tests.**
- A hole older than 2 s still lets a handshake through ahead of it.
- The client lifts quarantine on immediate-only receives.

Guard: a hole younger than 2 s still gets immediate delivery.

**Ships.** This must be in the next provider build the owner publishes.

### Fix 3: platform-unreachable only when every window says so

**Root cause.** The merged window status takes the sharpest window. On iOS the
speed window holds a single exit, so when that one exit's transports drop, the
merged reason turns platform-unreachable while the quality windows are still
trying. A test currently pins "sharpest wins".

An earlier version of this section blamed the 2-exit limited window. That was
wrong: the limited window only contributes availability and never reaches the
merged reason, and a test of it alone passes on main.

**Change.** Platform-unreachable wins the merge only when every trying window
reports it, as `Failed` already works.

**Root cause test.** The iOS speed window's single exit alone turns the merged
status to platform-unreachable.

### Fix 4: don't restore stalled exits

**Root cause.** After a restart or a kill, saved exits are restored whatever
their last state was. One that was already stalling absorbed SYNs for 10 s
(section 1, item 4).

**Change.**
- Exits that were held, warned, gapped or ack-stalled when they were saved are
  skipped on restore.
- The same work closes two gaps left by f9439015:
  - a) A provider removed while its candidate is being evaluated can still be
    installed.
  - b) A saved identity with an empty destination is returned, and the window
    would panic on it.

**Root cause tests.**
- A stalled exit is restored after a restart.
- Follow-up a.
- Follow-up b, with no panic.

### Fix 5: app transport errors go through the sdk rule

**Root cause.** Some app errors raise the banner directly and bypass the sdk's
20 s/60 s rule:
- iOS's NSURLErrorDomain branch;
- Android's own exception classes: UnknownHost, SocketTimeout, Connect and
  NoRouteToHost.

**Change.**
- The apps pass these errors to the sdk rule.
- The logic and its tests live in the sdk.
- Windows, Linux and macOS are checked for the same direct raises.

**Root cause test.** At the sdk seam the apps call, a single error of each kind
raises the banner at once today. With the fix, it raises only under the
20 s/60 s rule.

## 4. Also in this round

- **Unchanged performance profile** (section 1, item 3). Applying a
  performance profile that hasn't changed, or that changed only in fields that
  don't affect windows, must not tear down any window. Root cause test: today
  it tears down every window. The investigation also records what triggered
  the two rebuilds.
- **Not changed.** The iOS limited window stays at 2, by the owner's decision
  of 2026-11-13, and race ordering stays as in bc776f20.

## 5. Shipping

- **Apps:** each merge rides the next nightly app build, which builds the sdk
  and connect from main. Store releases are manual.
- **Providers:** fix 2 ships with the next build nightly the owner publishes.
  It must be in that build.
- **Hosted proxies:** the server's proxy builds on connect, so it picks up any
  shared change with the next proxy build, which needs the owner's go.

## 6. Status

| Fix | State | Merges | Root cause tests |
|---|---|---|---|
| 1, DNS | in progress | | |
| 1, exits | in progress | | |
| 2 | in progress | | |
| 3 | landed 2026-11-14 | connect 18a7d6c9 | `TestLoneSpeedExitDropDoesNotTurnConnectionPlatformUnreachable`, `TestLimitedWindowDoesNotTurnConnectionPlatformUnreachable`, `TestMergedMonitorStallStatus` (updated); guard `TestDisabledWindowDoesNotVetoPlatformUnreachable` |
| 4 | in progress, with fix 1's exit work | | |
| 5 | landed 2026-11-14 | sdk 83e1b169, apple 9b8d024e, android 8b0a8631a | `TestAppTransportFailureRaisesOnlyWhenFailuresLast`, `TestAppleUrlErrorsThatSettleItRaiseAtOnce`; guard `TestOtherAppleUrlErrorsAreNotCantReach`. Windows and Linux already raise only through the sdk rule |
| Unchanged profile | landed 2026-11-14 | connect b843ee6f (main d953f4a2) | `TestProfileChangeDoesNotRetireWorkingExits`. Both rebuilds in the logs came from a user change to the connect options; a post-quantum toggle or turning Strong Anonymization on still rebuilds |
