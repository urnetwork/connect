# Initial navigation stall: independent review 2

Reviewed 2026-11-06 against the working tree at `03e98fe5` and the raw artifacts under `/Volumes/data/urnetwork/`. Read-only; no replay was run. Line numbers are from the current tree; the measured build's overlay copies (`runtime-20261105/page1-original100-stages74.899g1_8f/formatted/0..9`) drift by a few lines (for example the log cites `transfer.go:11139`, now `transfer.go:11142`).

## 1. Summary of findings

1. **The brief's stage timeline is accurate, but its framing is wrong in one important way.** Every multi-hundred-millisecond span it attributes to "setup dependencies" (query 776 ms, provide registration 779 ms, lane-0 contract 903 ms, lane-7 contract 898 ms) is an out-of-band HTTP POST/GET to the API over the shaped link (`transfer_contract_manager.go:1943-1946` → `transfer_oob_control.go:105-116` → `api.go:501-515` → `net_http.go:3367-3401`). The same run shows the identical operation completing in 155-180 ms when it goes well (`output.txt` lines 320, 350, 352). The floor for one OOB round trip on this link is one RTT (~165 ms) plus server time. So the critical path is not four dependencies that cost ~800 ms each; it is four dependencies that should cost ~170 ms each and a transport/API path that frequently costs 5x that. The brief's own caveat ("do not assign to a backend bottleneck") is correct, but it understates how much of the 6.4 s is this one unexplained multiplier: roughly 2.4 s of the 4.3 s between query begin and lane-7 contract.

2. **The 5.333 → 6.439 s window the brief did not decompose contains an 878 ms TLS round trip that should take ~175 ms.** `REPORT-workload.json` `pressure.navigations[1].phase_ns`: connect_done 5332.6 ms, tls_done 6210.8 ms, wrote_request 6210.8 ms, first_byte 6439.4 ms. The SDK's own first-load hook confirms it: `[firstload]tcp 203.0.113.11:443 synack 3331ms firstbyte 4209ms` (output.txt:347, measured from the second race dial at 2.000 s; "firstbyte" is the first payload byte, i.e. the ServerHello, `net_first_load.go:5-11`). The TLS span was 175 ms in the historical run and 314 ms in the boundary run. ~700 ms of the stall is here, after every setup dependency was already satisfied, and the brief does not mention it.

3. **Cross-run comparison shows the extra second in 6.439 s is not a new serial dependency; it is where a ~700-900 ms anomaly landed.** Page-1 phases from the three runs' `REPORT-workload.json`:

   | run | p1 first byte | connect_done | TLS span | request→first byte |
   |---|---:|---:|---:|---:|
   | historical100-canonical74.4n1jirtf | 5.410 s | 4256 ms | 175 ms | **979 ms** |
   | page1-original100-boundary74.14v7h78c | 5.563 s | 5042 ms | 314 ms | 207 ms |
   | page1-original100-stages74.899g1_8f | 6.439 s | 5333 ms | **878 ms** | 229 ms |

   Each run has exactly one ~700-900 ms excess after the SYN-ACK, in a different place. Likewise the post-admission contract wait was 447 ms in the boundary run (`output.txt` +4.892 s, provider SYN at 4.972 s) and 898 ms in the stages run. The brief's "eliminating the 898 ms would nominally leave 5.54 s" arithmetic treats a sample from a wide distribution as a fixed cost.

4. **Contract acquisition in this environment has a heavy tail the brief does not report.** Over the whole stages run, `grep 'contract wait'` gives n=73 waits above the 50 ms log threshold (`transfer.go:956`): p50 654 ms, p90 2.97 s, max 13.7 s, with 7 waits over 5 s. On a 120 ms RTT link that is pathological, and it is a far better candidate for the field symptom ("several pages fast, then one pauses 5-10 s") than the synthetic all-exits-refuse scenario, because it needs no provider failure at all.

5. **A second field-relevant mechanism is visible in the log and unmentioned:** at admission (output.txt:342, tap+4.266 s) the window logged `discovery_expand_deferred failures=1 delay=8846`. `ip_remote_multi_client.go:14580-14586` classifies the expand pass as unproductive when the refreshed update has not yet received inbound, and `ip_remote_multi_client_discovery_retry.go:42-70` then defers the next discovery by `DiscoveryExpandRetryMin`..`Max` = 10 s..2 min (`ip_remote_multi_client.go:330-331`), jittered. In this run the admitted client worked so the deferral was moot; if the first late candidate had failed, the next discovery would have waited ~9-10 s. That is exactly the field-symptom shape.

6. **Smaller corrections to the brief:** inner MTU is 1320, not 1280 (`REPORT-workload.json` `pressure.device_access.inner_mtu`, `route_test.go:829-831`); the second SYN flow at 2.000 s is the Tun dial race (`tun.go:126-127` DialRace 2 / DialRaceTimeout 2 s, `tun.go:1400-1410`, `1470-1560`), not a stack retransmission, and the race also applies to every OOB API dial because the strategy's `DialContext` is `clientTun.DialContext` (`route_test.go:724-735`); the query trigger at 1.476 s coincides to the millisecond with the `service-refresh` warning on exit `8a6bb014` with `dialfails=1` (output.txt:328, `ip_remote_multi_client.go:13990-14000`), so "81 ms after the last refusal" is a coincidence of the refusal sequence, not a gate that was measured.

## 2. Verification of the brief's claims

| Claim | Status | Evidence |
|---|---|---|
| 6.439 s TTFB on page index 1, five-second gate failed | Confirmed | `REPORT-workload.json` pages[1].first_byte_ns=6439425833; `TTFB-DISTRIBUTION-NS.json` max 6439425833, stalledPages [1]; gate is `sdk.DefaultDeviceLocalSettings().SendTimeout` = 5 s compared to HTML TTFB (`native_browsing_test.go:635`, `sdk/device_local.go:413`) |
| All 100 navigations completed | Confirmed | `REPORT-workload.json` completed_navigations=100, overlap_proofs=99 |
| Stage timeline (query 1.476→2.253, mint →2.412, offer 2 µs, provide →3.191, ping 3.191, admitted 4.266) | Confirmed | `GENERATOR-STAGES-TIMELINE.json`; Skipped=0, Overflow=0 |
| Lane-0 contract set 4.095 s, lane-7 contract set 5.164 s | Confirmed | output.txt:331-332 ("contract wait 903ms"), 344-345 ("contract wait 898ms"); tap = 10:11:45.755133 local |
| Provider sees SYN 5.248 s, SYN-ACK 5.249 s | Confirmed | `PAGE1-BOUNDARY-METADATA.json` Flows[0].Providers[4] SynNs 5248239833, ReturnSynAckNs 5249247958; Dials[0..1] start 5.2486 s end 5.2491/5.2493 s |
| Offer/consume boundary fast | Confirmed | offer_begin→args_consumed 7 µs |
| Prewarm already enabled | Confirmed | `transfer.go:1002` |
| Nonzero lanes need capability evidence | Confirmed in code, not exercised by data | `transfer.go:11236-11245` (prewarm take), lane checks in `createSendSequence` |
| API endpoint is plain HTTP; TLS verification not a factor | Confirmed | `sdk_browsing_setup_fixture_test.go:119` `http://edge:apiPort` |
| "Query and registration spans do not distinguish socket/link/API/DB" | Confirmed, and understated | see §3.1 |
| Late provider constructed 0.448 s, API-visible 0.742 s, last refusal 1.395 s | Confirmed | `REPORT-full-window.json` late_constructed_ns, late_api_visible_ns, first_refusal_ns; refusal_counts [4,4,1,4,0,1,3] |
| Inner MTU 1280 | **Wrong** | profile inner_mtu 1320 |
| "The measured delay is distributed among discovery, identity/registration, evaluation, and a later data-lane dependency" | Misleading | each named span is one OOB HTTP call; §3.1 |
| Nothing said about 5.249→6.439 s | **Gap** | §3.2 |

## 3. What the brief missed

### 3.1 Every long "setup" span is one OOB HTTP call, and the call latency is bimodal

Path: `SendSequence` → `ContractManager.CreateContract` (`transfer.go:11274-11280`) → `ClientOob().SendControl` (`transfer_contract_manager.go:1943`) → `ApiOutOfBandControl.SendControlWithCtx` → `BringYourApi.connectControlWithCtx` → `httpPostWithStrategyRaw` → `ClientStrategy.HttpSerial` (`net_http.go:2100-2160`) over an `http.Transport` with keep-alive, `ForceAttemptHTTP2` (no effect on plain http), and **no `MaxIdleConnsPerHost`** (`net_http.go:2969-2985`; Go default 2). The provide registration (`ip_remote_multi_client_api.go:1465`) and `FindProviders2` (`ip_remote_multi_client_api.go:937-945`) take the same HTTP path. All of it traverses the gVisor client Tun and the shaped link (`route_test.go:724-735`, `754-758`).

RTT arithmetic for this link (`REPORT-workload.json` `device_access`): base delay 60 ms each way, jitter 25 ms, forward queue 12,500 B at 1 Mbit/s (≤100 ms), reverse queue 62,500 B at 5 Mbit/s (≤100 ms), 0.5 % independent loss each way, `allow_queue_drops: true`. One RTT is 120 ms unloaded and at most ~370 ms with both queues full. Measured one-way hops in the artifact are 84 ms (lane-7 contract set 5.164 → provider SYN 5.248; provider SYN-ACK 5.249 → consumer SYN-ACK 5.333), and request→handler was 166 ms (6.211 → 6.377). So an OOB call on a warm keep-alive connection should be ~170-200 ms, and the ~160-180 ms contract waits in the same log are exactly that.

The 776 / 779 / 903 / 898 ms spans on page 1's path, and the 836 / 837 / 790 / 807 / 841 ms waits on unrelated sequences in both diagnostic runs, are 4-5 RTTs. Candidate explanations, none excluded by the data:

- **Idle-pool miss.** With the default two idle connections per host and concurrent control calls (several lanes' contracts, the probe sweep's traffic, provide registration, FindProviders), the third concurrent call dials a new TCP connection: +1 RTT, through `raceTunDialContext`, which fires a second SYN if the first takes 2 s.
- **Loss on a fresh gVisor connection.** gVisor's initial RTO is 1 s and the SYN RTT is not sampled when timestamps are absent; a single lost request segment on a new connection costs ~1 s. 0.5 % per direction per packet does not explain a cluster at 0.8-0.9 s on its own, but the shaper's 9-packet forward queue with drops allowed can push the effective loss much higher during the probe sweep (output.txt:327, 348, 353: two exits probed with 68 and 127 DNS packets each between tap+1.3 s and +7 s) and the page-0 asset ACK stream.
- **Server work.** `controller.CreateContract` (`../server/controller/connect_controller.go:760-852`) records `server.BeginContractCreationTiming` to a metric, not to the log (`grep -c 'contract][req]' output.txt` = 0 at this verbosity). Nothing in the artifacts bounds the DB time.
- **CPU starvation.** `EXECUTION-BINDING.json` shows `GOMAXPROCS=2` for a single process hosting the SDK, seven providers, the platform, four TLS origins, the shaper and the test; `REPORT-workload.json` reports 1,165-1,591 goroutines. Shaper timer slip inflates every measured "RTT" uniformly, which would also inflate the TLS span in §3.2.

Until one of these is measured (question 3 below), "overlap the dependencies" is optimizing the wrong variable: cutting the OOB call back to one RTT saves ~2.4 s on this trace; overlapping two 170 ms calls saves 170 ms.

### 3.2 The unexamined 5.333 → 6.439 s window

From `REPORT-workload.json` navigations[1] and `PAGE1-BOUNDARY-METADATA.json`:

| event | ms from tap | note |
|---|---:|---|
| consumer SYN-ACK (device ingress hook) | 5332.6 | 84 ms after provider sent it |
| ClientHello sent (tls_start) | 5332.6 | |
| ServerHello at device (`[firstload]` firstbyte) | 6209 | **878 ms** round trip |
| tls_done / got_conn / wrote_request | 6210.8 | |
| origin handler entered (HandlerNs) | 6376.7 | 166 ms, one RTT |
| first byte | 6439.4 | 63 ms, one hop |

The provider's return path is not a contract dependency: the only provider-side contract for this pair is lane-0 companion at tap+4.206 s (output.txt:333, `role=server companion_contract=true logical_lane=0`), and the SYN-ACK traversed it in 84 ms. So the ~700 ms excess in the TLS round trip is after both directions were proven open. Plausible causes, all unmeasured: a lost ClientHello (gVisor RTO is still 1 s on this connection because the SYN was retransmitted twice and Karn's rule discards those samples), a lost ServerHello segment retransmitted by the provider-side user-space TCP, head-of-line blocking on the H1 WSS connection shared with the three live page-0 asset streams (`prior_live_assets: 3`), or shaper/scheduler slip. Note `Contended: 4, FrameOverflow: 2, UnknownProvider: 1` in the boundary collector: the collector itself dropped frames in this window.

### 3.3 SYN handling between 1.0 s and 5.25 s

`PAGE1-BOUNDARY-METADATA.json` Flows[0] (port 62605): SYNs read at 0.274, 1.000 (Accepted 0), 3.002 s; the six initial exits received 10 SYN deliveries between 0.428 and 1.395 s (the multi-client re-offers the same SYN across exits after each refusal); the late provider received exactly one SYN for this flow, at 5.248 s. Flows[1] (port 64476, the race dial): SYNs read at 2.000, 3.002, 5.002 s; late provider SynCount 2, both at 5.2483 s. So the SDK held the 3.002 s SYNs for 2.2 s and released them, together with the 5.002 s one, 84 ms after the lane-7 contract set. This is consistent with the brief's model, with two caveats: (a) gVisor's SYN backoff is 1 s, 2 s, 4 s, so a SYN that is dropped instead of held is next retried at ~7 s; the hold is what keeps the stall at 6.4 s rather than 7+; (b) the measured build predates `10f848ff` "Fix native packet ownership and SYN cache publication", which changes exactly this hold/publish path (`ip_remote_multi_client.go` 91 lines, `ip_remote_multi_client_collapse.go`), so the brief is right that a fresh replay is required before any claim about the synced build.

### 3.4 Discovery trigger and retry (field relevance)

The query at 1.476 s was started by the service-refresh reservation path, not by a count of refusals: output.txt:328 at 10:11:47.231 (`event=warn exit=8a6bb014 from=none to=service-refresh flows=0 sources=2 dialfails=1`) is the same millisecond as query_begin. `ip_remote_multi_client.go:13990-14000` sets that warning when `client.serviceRefreshReservation` is non-nil; `14545-14586` runs the expand pass. The brief's statement that recovery "requires refusal evidence for the compatible offered exits" may be policy, but the artifact does not show which refusal was decisive, and the first refusal was at 0.428 s. Whether the window could legitimately start discovery after the first destination-specific refusal (saving ~1.05 s) is a policy question the brief should state as such rather than fold into "no timer bug".

The `discovery_expand_deferred failures=1 delay=8846` line (§1.5) is more important for the field. The pass that admitted the late client was still booked as unproductive because `refresh.update.receivedInbound` was false at that instant (`ip_remote_multi_client.go:14580-14586`); the next discovery was pushed out ≥ 8.8 s. Any field case where the first discovered candidate is refused, rate-limited (`createContractRateLimitHeld`, `transfer_contract_manager.go:1898-1908`), or slow to ping would then idle for 10 s to 2 min. That is the shape of the reported iOS symptom and is testable deterministically (§6, T8).

### 3.5 Does the scenario represent the field symptom?

Only partially. It reproduces "a flow must wait for a new provider", but it requires all six resident exits to refuse the same destination, which the brief itself calls unjoined to the iOS session. Two mechanisms visible in this run need no provider refusal at all and are therefore more likely field explanations: the OOB contract latency tail (§1.4: 7 waits above 5 s in one run; any sequence waiting that long stalls its flow), and the discovery retry deferral (§3.4). The 5 s `SendTimeout` (`sdk/device_local.go:413`) is also both the gate and a real behaviour: packets that wait longer than 5 s are dropped, so in the field a 5 s wait becomes a TCP retransmission at the next backoff step, which turns "5 s" into "5-10 s".

## 4. Answers to the reviewer questions

**Q1. Which dependencies can overlap, which need the preceding authenticated result?**

Chain as implemented: `FindProviders2` (needs refusal evidence) → `AuthNetworkClient` mint (needs destination id; `ip_remote_multi_client_api.go:1032-1088`) → `NewClientContext` (needs JWT) which already starts the platform transport (`:1414`) and provide registration (`:1465`) concurrently → ping waits for the provide ack → lane-0 opening contract is requested when the ping sequence starts (`transfer.go:11236-11245`, prewarm; the 903 ms wait begins at 3.191 s) → admission waits for lane-0 contract + pong → lane-7 contract → SYN.

Genuinely serial: query→mint (destination needed), mint→transport/provide (JWT needed), lane-0 contract → first ping Pack (authorization), admission → routing of user traffic (policy). Safely overlappable today:
- Lane-0 opening contract request at `NewClientContext` time (2.412 s) instead of at ping start (3.191 s). The server-side check is the source's relationship to the destination, which does not depend on this client's own provide registration. Saves up to ~780 ms on this trace (bounded by the provide span); zero extra identities, one contract that the sequence would request anyway.
- Transport establishment is already overlapped; confirm with a timestamp (the "transport down/restored" lines at 3.191/4.441 s are routing-loop pass times, not transport events).
- Nothing else overlaps without policy change: starting the query at the first refusal is a policy change (§3.4).

**Q2. Can the pending data-lane contract be prepared without early lane authorization, duplicate escrow, extra workers, or speculative identity growth?**

Yes, within the existing primitives, if it is scoped to the one flow already known to be waiting: at admission the multi-client knows the held SYN's tuple and therefore its lane (7). `ContractManager.createContract` already de-duplicates identical in-flight requests per key (`contractQueue.beginCreate`, `transfer_contract_manager.go:1917-1922`) and the sequence's prewarm path takes a queued contract before requesting (`transfer.go:11236-11245`). So "request the lane-7 contract into the queue at admission, before `createSendSequence`" needs no placeholder sequence, no capability bypass (the Pack is still not emitted until the lane's capability is acknowledged), and no duplicate escrow (the sequence's own request joins the pending one). Cancellation custody is the real cost: a queued contract for a flow that is retired before the sequence exists must be closed by the queue owner, which is the brief's point about the counter. Expected saving: one contract wait minus the time the SYN would have waited anyway, i.e. up to ~730 ms here and ~280 ms in the boundary run, but only ~0 if the OOB path is fixed to one RTT first. This should therefore be sequenced after Q3's measurement, not before.

**Q3. Minimal observations to separate transport scheduling from API/backend work in the 776/779 ms spans**

1. `httptrace.ClientTrace` on the OOB client's request context (`net_http.go:2134-2146`): `GetConn`/`GotConn{Reused, WasIdle}`, `ConnectStart/Done`, `WroteRequest`, `GotFirstResponseByte`. Log one line per control request with those offsets and the pack's frame types. This alone splits pool-miss (+1 RTT, Reused=false), dial (ConnectDone-ConnectStart), server (FirstByte-WroteRequest), and body.
2. Server: emit `server.BeginContractCreationTiming` results (`connect_controller.go:767-790`) and the HTTP handler's receive/finish timestamps at V(0) for the diagnostic build, keyed by client id, so WroteRequest→handler-receive (link) and handler duration (DB) are separable.
3. Shaper counters: queue drops and loss drops per direction for the device link during the page-1 window (`allow_queue_drops: true` is enabled and no drop counts appear in any artifact).
4. gVisor stats for the OOB connections: `tcp.Stats.Retransmits`, `Timeouts`, `FastRetransmit` deltas across the window (one `stack.Stats()` snapshot before and after).
5. Idle-pool size: log `MaxIdleConnsPerHost` effective value and the number of concurrent control requests in flight (`ApiOutOfBandControl.requests` admission count).

Five scalar fields per request; no change to policy or data path.

**Q4. Safer way to reuse already-authorized setup work?**

- Reuse the lane-0 acknowledged headroom to carry the opening Pack of lane 7 is what `transfer.go:11340-11345` already describes for the non-debit path ("the acknowledged predecessor's headroom carries this Pack"); verify with a unit test whether the held SYN actually rides that path, because the boundary run's SYN left 80 ms after a 447 ms contract wait ended, suggesting it did not.
- Keep the discovered client's contract queue warm across cancellation by closing queued grants from the queue owner (as the brief says), rather than draining on sequence retire.
- Do not widen the idle pool for all hosts; raise `MaxIdleConnsPerHost` only for the API host (or keep HTTP/1 pipelining off and use one HTTP/2 connection where TLS is in use). This preserves per-lane ordering because contract ordering is by `contractSeqIndex`, not by connection.
- Legacy peers: the lane-0 companion contract on the provider side (output.txt:333) is the only return-path contract observed; nothing in the proposal touches it.

**Q5. Additional deterministic counterfactuals**

See §6. The two that disprove the brief's explanation fastest are T1 (loss and jitter zero: if the four spans collapse to ~1 RTT, the "serial dependencies" were retransmissions) and T2 (OOB through local control: if they collapse, it was the link; if not, it was the server).

## 5. Ranked safe shortenings (estimated on this trace)

| rank | change | saves | risk / caveat |
|---|---|---:|---|
| 1 | Make OOB control calls one RTT: measure per §Q3, then fix the dominant cause (idle-pool size for the API host, or retransmit behaviour, or server time) | up to ~2.4 s (776+779+903+898 → ~4×170) | measurement first; no policy change |
| 2 | Request the lane-0 opening contract at `NewClientContext` instead of at ping start | ≤ ~780 ms (overlaps the provide span) | one contract that is requested anyway; needs close-on-cancel |
| 3 | Queue the known waiting flow's data-lane contract at admission (brief's candidate) | ~300-730 ms today; ~0 after rank 1 | cancellation custody; dedup exists |
| 4 | Decompose and fix the post-SYN-ACK 878 ms (TLS round trip) | ~700 ms on this trace, 0-800 ms on others | cause unknown; may be the same root as rank 1 |
| 5 | Start discovery on the first destination-specific refusal rather than after the service-refresh reservation | ~1.0 s | policy change; the brief explicitly keeps full-window rules |
| 6 | Do not book an admitted expand pass as unproductive when inbound has not yet arrived; or shorten `DiscoveryExpandRetryMin` for the first retry | 0 here; up to 10 s-2 min in the field | field-relevant, not this trace |
| 7 | Suppress the Tun dial race's second attempt while a held SYN for the same tuple exists | 0 latency; halves SYN traffic and provider dials | cosmetic |

Items 2 and 3 together cannot get this trace under 5 s (6.44 − 0.78 − 0.73 ≈ 4.9 s at best, before variance); item 1 can.

## 6. Deterministic tests that would falsify the explanation

- **T1 (link vs dependency).** Replay with `loss_probability=0`, `jitter=0`, same seed 4013 and workload. Prediction if the brief is right: the four spans stay ~800 ms. Prediction if §3.1 is right: they fall to ~170-350 ms and page-1 TTFB falls below 5 s with no code change.
- **T2 (link vs server).** Same replay with the OOB control bound to `NewApiOutOfBandControlWithLocalControl` (`transfer_oob_control.go:82-90`) so CreateContract bypasses the shaped link. Spans that remain ~800 ms are server work; spans that collapse were link/transport.
- **T3 (pool miss).** Unit test on `ClientStrategy.HttpSerial` with three concurrent POSTs to one host and a fake dialer that counts dials: assert the third request does not dial. Fails today (default idle pool 2).
- **T4 (CPU confound).** Replay at `GOMAXPROCS=8`. If the spans shrink, the 2-processor binding inflates every measured latency and the trace cannot be used for sub-second attribution.
- **T5 (uplink contention).** Replay with the prober budget zero during the first 10 s after tap. Isolates the 195 probe packets' effect on the 9-packet forward queue.
- **T6 (serial dependency cost).** Unit test: `SendSequence` with a fake OOB that answers CreateContract in a fixed 170 ms; admit a client with a held SYN; assert the SYN is emitted within 170 ms + ε of admission. Passing proves the dependency's intrinsic cost is one OOB round trip; the 898 ms is then not "the dependency".
- **T7 (SYN hold release).** Deterministic test around `10f848ff`: a SYN read before admission must be delivered to the admitted client within one contract round trip, and a SYN that is instead dropped must be shown to cost the next gVisor backoff step (4 s). Distinguishes "held" from "retransmitted" in future traces.
- **T8 (field mechanism).** Scenario variant where the first discovered late provider refuses its first SYN; assert whether the next discovery waits for `discovery_expand_deferred` (≥ 8.8 s). If it does, that is a reproducible 5-10 s stall with no all-exits outage required.
- **T9 (TLS window).** Replay with the inner TCP MSS reduced so the ServerHello flight is one segment, and with loss zero; if the 878 ms disappears only with loss zero, it is a retransmission; if only with the MSS change, it is HOL/queue.

## 7. Confirmed vs plausible

Confirmed by code or data: the stage timeline and contract-set times; that each long span is an OOB HTTP call over the shaped link; the 84 ms one-way hop; the 878 ms TLS round trip; the cross-run variance table; the contract-wait distribution; the dial race origin of the second flow; the `discovery_expand_deferred` deferral and its 10 s minimum; the MTU figure; the GOMAXPROCS=2 binding; the probe sweep traffic in the window; the absence of any provider-side lane-7 contract.

Plausible, unproven: which of pool-miss, loss/RTO, server time, or CPU slip produces the ~800 ms class; whether the held SYN rides lane-0 headroom; whether the TLS excess is a lost segment; whether discovery could policy-wise start at the first refusal; whether the field iOS symptom is the OOB tail, the discovery deferral, or the synthetic outage path.

## 8. Artifacts used

- `/Volumes/data/urnetwork/runtime-20261105/page1-original100-stages74.899g1_8f/{output.txt, REPORT-workload.json, REPORT-full-window.json, GENERATOR-STAGES-TIMELINE.json, PAGE1-BOUNDARY-METADATA.json, TTFB-DISTRIBUTION-NS.json, EXECUTION-BINDING.json, diagnostic.formatted.overlay.json, formatted/3, formatted/5, formatted/6}`
- `/Volumes/data/urnetwork/runtime-20261105/page1-original100-boundary74.14v7h78c/{output.txt, REPORT-workload.json, PAGE1-BOUNDARY-METADATA.json, REPORT-full-window.json}`
- `/Volumes/data/urnetwork/runtime-20261105/historical100-canonical74.4n1jirtf/{REPORT-workload.json, REPORT-full-window.json}`
- `/Volumes/data/urnetwork/c49-native-relocated-execution.PREP.dupk0plj/page1-stage-actual-readback.1uw11ocp/{SUMMARY.json, PROVENANCE.json}`
- Code: `transfer.go`, `transfer_contract_manager.go`, `transfer_oob_control.go`, `api.go`, `net_http.go`, `net_first_load.go`, `tun.go`, `ip_remote_multi_client.go`, `ip_remote_multi_client_api.go`, `ip_remote_multi_client_discovery_retry.go`, `ip_remote_multi_client_collapse.go`, `../sdk/device_local.go`, `../server/controller/connect_controller.go`, `../server/connect/perfvar/{sdk_browsing_admission_test.go, sdk_browsing_setup_fixture_test.go, native_browsing_test.go, native_browsing_tls_test.go, route_test.go}`.
