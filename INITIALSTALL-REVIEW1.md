# Initial navigation stall: independent review 1

Reviewer: Opus 5.5. Date: 2026-11-06. Reviews [INITIALSTALL.md](INITIALSTALL.md).

Scope: read-only analysis of the code and of the artifacts from the 6.439 s run. No replay or test was run, so every proposed saving is an estimate from one run.

Independence note: the analysis below was finished before the parallel Fable review reported, but this file was written after its short summary arrived. `INITIALSTALL-REVIEW2.md` was not opened, and nothing here is taken from that summary. The two reviews have not been reconciled.

## Summary

The brief's stage timestamps are accurate. Its reading of the critical path needs correcting in four ways:

1. **The lane-0 "contract wait" is probably platform-transport bring-up, not contract latency.** The contract was minted 87 ms after the request. The "contract set" log line comes after a blocking first write, and that write appears to wait for the new client's WSS transport.
2. **If so, provide registration (779 ms) is off the critical path in this run.** It runs in parallel with the transport, which finishes later. Shortening it would save about nothing.
3. **The largest unattributed span is after the SYN-ACK.** 5.249 s to 6.439 s is 1.19 s, and 878 ms of that is between the client receiving SYN-ACK and TLS completing. The brief's table leaves this unexplained.
4. **Most of the setup after the query does not depend on the destination.** Mint, transport dial and provide could overlap the refusal window. This is the largest safe lever I found: about 1.84 s, which would nominally give 4.6 s on this run.

There is also a lead on the field symptom. Of the 23 new clients in this run, 6 waited 5 s or more for their first lane-0 contract. That matches the reported 5–10 s pause much better than the page-1 path does. It is unproven, and it deserves as much attention as the page-1 trace.

## Method: IDs carry timestamps

`NewId` puts the wall-clock millisecond in the top 48 bits of each ID (`id.go:43`). All components in this replay share one host clock, so contract IDs give a server-side time inside spans the brief treats as opaque.

Checks on this method:

| ID | Decoded time | Independent time |
|---|---:|---|
| New client | 2.327 s | Inside the mint span, 2.253–2.412 s |
| Lane-0 sequence | 3.191 s | `initial_ping_begin` 3.191 s |
| Lane-7 sequence | 4.266 s | `admitted` 4.266 s |
| `[ncc]` connection IDs | Within about 7 ms of their log lines | Log timestamps |

Limits: I did not find the exact line where the server mints contract IDs, so the mint may not be the end of the handler. Empirically, the median time from mint to "contract set" is 81 ms across all 95 opening contracts, about one link traversal, so it is usually close to the end.

## Corrected critical path

Times are from the tap. "Bound by" is my interpretation.

| Span | Duration | Bound by | Status |
|---|---:|---|---|
| 0 → 1.476 s | 1,476 ms | All exits refusing, including two delayed by lane-7 contracts | Measured; the delay's cause is unknown |
| 1.476 → 2.253 s | 776 ms | Provider query | Measured |
| 2.253 → 2.412 s | 159 ms | Mint, one warm round trip | Measured |
| 2.412 → 4.094 s | 1,682 ms | **Platform transport bring-up** | Likely, not proven |
| 4.094 → 4.266 s | 172 ms | Ping round trip, including provider companion contract at 4.206 s | Measured |
| 4.266 → 5.164 s | 898 ms | Lane-7 contract: 822 ms before mint, 76 ms after | Measured; the 822 ms is unexplained |
| 5.164 → 5.248 s | 84 ms | SYN transit | Measured |
| 5.249 → 5.333 s | 84 ms | SYN-ACK return (`ConsumerSynAckNs`) | Measured |
| 5.333 → 6.211 s | 878 ms | **TCP-to-TLS completion** (`GotConnNs`) | Measured; cause unknown |
| 6.211 → 6.377 s | 166 ms | Request reaches handler (`HandlerNs`) | Measured |
| 6.377 → 6.439 s | 63 ms | Response first byte | Measured |

## Findings

### 1. The lane-0 wait is probably transport bring-up

Evidence for this page-1 client:

- The contract request began at 3.191 s and the ID was minted at 3.278 s. "Contract set" was logged at 4.094 s, **817 ms after the mint**, against a run median of 81 ms.
- The log line is emitted after `sendWithSetContract` returns (`transfer.go:11134`, log at `transfer.go:11139`). That call reaches the route write, so a wait for a route is counted as contract wait.
- The transport is created at `ip_remote_multi_client_api.go:1433`, before provide starts. The server registered the connection (`[ncc]`) at 3.790 s, 1.38 s after that.

Evidence across the run:

- Every opening contract with more than 200 ms between mint and set is a **client-role lane-0** contract, which is the first write on a new client. No data-lane opening contract shows it.
- Among the 23 new clients, most with a large post-mint delay reached "contract set" 28–80 ms after the nearest `[ncc]` line, about one link traversal.
- Client `7701` had no `[ncc]` in its window and waited exactly 5,001 ms, which matches `PingWriteTimeout` (`ip_remote_multi_client.go:253`).

**Caveat:** the page-1 client is one of the weaker fits. Its gap from `[ncc]` to "contract set" is 305 ms, not about 60 ms, and that extra time is unexplained. `[ncc]` IDs also cannot be joined to client IDs from this log, so the pairing is by time. A direct test is below.

Why it matters: the brief lists the 904 ms under evaluation and frames the remaining work around contracts. If this finding holds, the target is WSS setup: 1.68 s from client creation to first write, where roughly four round trips (TCP, TLS, upgrade, auth) would suggest about 0.5–0.6 s. The page-1 client's 1.768 s from mint to ready is exactly the median of all 23 clients, so this is typical of this run and not a page-1 accident.

### 2. Provide registration is probably not on the critical path

Provide committed at 3.191 s. Adding the contract round trip puts contract arrival at about 3.36 s, inferred from a 3.278 s mint plus about 80 ms. The first write then finished at 4.094 s. That leaves about 0.73 s of slack, so reducing the 779 ms would not move admission in this run.

The query's 776 ms remains on the path. Question 3 in the brief should therefore put transport timing ahead of provide timing.

### 3. The 1.19 s after SYN-ACK needs its own investigation

The brief's table ends at "Provider returns SYN-ACK 5.249 s" and "First HTML byte 6.439 s". `PAGE1-BOUNDARY-METADATA.json` already contains the split shown in the table above.

The 878 ms from SYN-ACK receipt to first server data compares with a `[firstload]` median of 431 ms over 15 connections (range 165–1,384 ms). It ranks 13th of 15, so it is slow but not unique. The minimum is about one round trip plus the origin's time, so several hundred milliseconds are unaccounted for.

I ruled out one explanation: the log shows only one provider-to-client sequence (lane 0, contract set at 4.206 s), so a second return-path contract wait is not visible here. Candidates that remain are first-Pack pacing or admission on the new lane, a loss or resend on a cold sequence, and link queueing behind unfinished page-0 assets. Nothing in the artifacts separates them.

### 4. Opening-contract latency has distinct modes, and the same delay also precedes the query

For the 73 client-role opening contracts, time from sequence start to mint falls into three groups:

| Group | Time to mint | Possible meaning |
|---|---:|---|
| 1 | 58–109 ms | Warm connection: one traversal |
| 2 | 182–292 ms | One extra round trip, such as a new TCP connection |
| 3 | 498–855 ms | Unknown |

There are also a few values at 1 s or more, up to 6.77 s.

Two patterns point at a shared resource and not at random retry jitter:

- At 0.481 s three clients started lane-7 sequences. One minted in 83 ms; the other two took 743 and 750 ms and completed 7 ms apart.
- At 6.440 s one client started three lanes. Two minted in about 90 ms and the third took 708 ms.

The second pattern fits Go's default of two idle connections per host. `net_http.go:2969` sets no `MaxIdleConnsPerHost`, and the fixture API is plain HTTP/1.1. But group 3 is too slow for a clean new TCP connection, and the mint at 2.25 s took only 159 ms, so link queueing does not obviously explain it either. Server-side lock contention also fits the "released together" shape. **The cause is open.**

Group 3 matters beyond lane 7. Two exits received the SYN only at about 1.39 s because their lane-7 contracts were set at 1.318 s. The last refusal, and so the query start, was held back by the same delay. The 776 ms query and 779 ms provide are similar in size, which suggests a shared cause but does not establish one.

### 5. The fixture differs from production in both directions

- **API transport:** the fixture uses plain HTTP/1.1. Production uses HTTPS with `ForceAttemptHTTP2`, which multiplexes requests. If group 3 is an idle-pool effect, it may be a fixture artifact and the query, provide and lane-7 numbers overstate production.
- **Dial pacing:** the fixture sets `MinNextConnectDelay` and `MaxNextConnectDelay` to zero (`sdk_browsing_setup_fixture_test.go:116`). The production defaults are 100–1000 ms (`net_http.go:71`), applied through `NextConnectTime` to first dials (`transport_family.go:611`). So transport bring-up, the span I think is most important, may be **understated**.

Any improvement measured in this fixture needs to account for both.

### 6. Field-symptom lead: 5–14 s first-write waits

Six of the 23 new clients waited at least 5 s for their first lane-0 contract:

| Client | Wait | Mint to set |
|---|---:|---:|
| `c27e` | 5,000 ms | 4.910 s |
| `2bde` | 5,001 ms | 4.920 s |
| `7701` | 5,001 ms | 4.924 s |
| `7794` | 5,624 ms | 4.776 s |
| `2165` | 10,338 ms | 10.108 s |
| `9d8d` | 13,655 ms | 13.572 s |

The exact 5,000–5,001 ms values match the 5 s `PingWriteTimeout`. `2165` and `9d8d` ended in `queued Pack expired before serialization`. These were not on the page-1 path, and later pages stayed at or below about 2.5 s, so they did not affect a measured navigation in this run.

A navigation that depends on such a candidate would pause for 5–14 s, the same size as the reported symptom. Six of 23 comes from one synthetic run under load, so it is not a field rate. The brief says this scenario is not causally joined to the iOS session; this may be the better place to look for that join.

The run also contains two data-lane contract waits with 5.66 s and 6.77 s before mint. I did not explain those.

### 7. A hazard the brief does not mention

Flow 2's SYN offers were at 2.0, 3.0 and 5.0 s, and the flow that won HTTP was this one (`Selected 2`). The held SYN survived in the queue, so no retransmit penalty applied. If a held SYN is ever dropped, the next retransmit would be about 4 s later, so the cost of a drop is a step of seconds. Any queue or cancellation change should test this.

## Claims checked

| Brief claim | Result |
|---|---|
| Stage times and spans | Confirmed against the timeline artifacts |
| Offer wait of 2 µs | Confirmed |
| Prewarm is already enabled | Confirmed (`transfer.go:1002`); the lane-7 sequence starts at admission and prewarm runs at sequence start |
| Unadvertised nonzero lanes are refused and lane 0 is pinned | Confirmed (`transfer.go:6297–6312`) |
| `beginCreate` removes in-flight duplicates only | Confirmed (`transfer_contract_manager.go:2749`) |
| New API object does not imply a cold pool | Correct as code reading, but connection reuse is still unmeasured |
| API TLS cannot explain the spans | Confirmed for API calls. **WSS does use TLS**, and I think it is on the path |
| Lane-0 contract set belongs to the evaluation span | Timing correct; the interpretation is likely wrong (finding 1) |
| Removing 898 ms leaves about 5.54 s | Arithmetic correct, but one run's sample is not a fixed cost |

## Options, ranked

Savings are nominal for this run and are not additive.

| Rank | Change | Estimated saving | Main risk |
|---:|---|---:|---|
| 1 | Measure first (see below) | None directly | Small; it decides the rest |
| 2 | Keep one spare identity: mint, transport and provide started at the first refusal | About 1.84 s, giving about 4.6 s | Identity growth, which earlier outage work deliberately stopped |
| 3 | Start the provider query early, as read-only, without minting until refusal is complete | Up to about 0.73 s here, limited by API visibility at 0.742 s | Query load; must not change admission |
| 4 | Remove the lane-7 wait, by the brief's overlap or by pinning the first flow to lane 0 | About 0.82 s | See below |
| 5 | Fix group 3, once its cause is known | Possibly about 0.7 s in each of several spans | May be a fixture artifact |
| 6 | Shorten WSS setup | Up to about 1 s, if it approaches four round trips | Needs stage data first |

**Option 2.** `NewClientArgsContext` and `NewClientContext` take no destination, and parked identity handling exists (`ip_remote_multi_client_api.go:1136`). Provide uses `ProvideMode_Network` with no provider involved. I checked the generator only, not whether `newMultiClientChannel` or `transportHint` binds a destination earlier. It should be limited to one spare, created only after a real refusal with a pending flow, and returned on the existing paths. Starting at the first refusal (about 0.43 s) and taking the 1.77 s median finishes about when the query returns, so the margin is thin.

**Option 4.** Pinning the flow to lane 0 needs no extra paid contract, since lane 0 already has an acknowledged one. It gives up head-of-line isolation for that flow, and the flow must stay pinned for life because the lane hash must not move a live flow (`transfer_logical_lane.go:12`). `logicalLaneExplicit` is the nearest existing mechanism, but I have not checked that it fits this use.

Rough combinations: 2 and 4 together give about 3.7 s. Adding 3 gives about 3 s. All of them keep the 1.275 s tail from contract set to first byte unless finding 3 is also solved.

## Answers to the brief's questions

**1. What can overlap, and what needs the preceding result?**
Mint, WSS dial and provide need no provider result, so they can overlap discovery, subject to the channel check above. The query needs no refusal evidence to be safe if it stays advisory. The evaluation ping needs both the provider and a live transport. The lane-7 Pack needs the lane-0 capability ACK.

**2. Can the data-lane contract be prepared safely?**
Probably, under the brief's guards, but it is worth about 0.82 s and adds paid ownership. Compare it with the lane-0 pin before building it. Either should come after the transport work, because it does not address the larger span.

**3. What observations separate transport from API work?**
Add these, in this order:

- Split the "contract set" timing: contract taken, first write entered, first write returned.
- Time the WSS stages: pacing wait, TCP, TLS, upgrade, auth sent, auth reply, route active.
- Use `httptrace` on the same API calls for `GotConnInfo.Reused`, `IdleTime` and dial times.
- Record server handler entry and exit. `BeginContractCreationTiming` already exists (`connect_controller.go:767`), so contract calls may need only a read-out.
- For lane 7 after SYN-ACK: record first Pack write, its ACK, and resend counts.

**4. Is there a safer way to reuse authorized setup?**
Yes: reuse destination-independent setup through one spare identity (option 2). It leaves per-lane ordering, legacy peers and sibling flows untouched, because it does not change any lane or contract rule.

**5. Which counterfactuals would disprove this explanation?**

| Test | Prediction if this review is right | Disproved if |
|---|---|---|
| Delay only the new client's WSS auth reply by 500 ms | Contract mint unchanged; "set" and admission move about 500 ms | Admission does not move |
| Delay only provide by 500 ms | Admission moves little, because of about 0.73 s of slack | Admission moves about 500 ms |
| Raise idle connections per host, or use HTTP/2 for the fixture API | Group 3 largely disappears | Group 3 is unchanged, which points to the server or link |
| Restore production dial pacing in the fixture | Transport span grows by the paced delay | No change |
| Run with no competing page assets | The 878 ms TLS span falls toward the minimum | It stays high, which points to the tunnel |
| Make a candidate's transport unavailable for 6 s | Ping fails at exactly 5 s with the wait seen in finding 6 | A different failure or timing |

Use paired runs with the same seed for any performance claim. The two earlier runs at 5.41 s and 5.56 s show that single-run differences of this size are not reliable.

## Limits of this review

- One run, with private diagnostic overlays and a pre-checkpoint source. I read the current checkout, which may differ from that source in places, and line numbers refer to the current one.
- The transport finding is inferred from timing and code paths. I did not trace the full route-write wait.
- Log lines are incomplete at this verbosity. `[family]dial` has no line for the page-1 client, so the WSS start is inferred from code.
- `WinnerAttributionKnown=false` still applies to the HTTP-level attribution.
- Savings assume the remaining spans do not change, which is unlikely on a shared link.
