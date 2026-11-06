# Provider retained-send capacity experiment

Status: one qualified synthetic host pair; no default or real-device qualification.

The baseline delivered a median 132.3288 Mbps; the candidate delivered 258.3424 Mbps (+95.23%). Each arm ran five consecutive ten-second download windows in one fresh process, with retained state between windows. These five windows are correlated observations, not five independent trials. The candidate followed the baseline; reverse-order confirmation is pending.

Both arms used identical production code and a fixed provider transfer root of 43,620,760 bytes. The fixture moved 2,876,094 bytes from its mostly unused client parent to retained provider sends: send capacity 2,876,094→5,752,188 bytes, client parent 30,198,988→27,322,894 bytes, provider parent 6,710,886→9,586,980 bytes. Provider receive, NAT, Pack, P2P and total-root capacities were unchanged. This is a fixture allocation experiment, not an unconditional SDK policy change.

Mean encoded original queue bytes increased 785,652→1,563,828 (+99.05%). Mean retained send usage increased approximately 2.843→5.656 MB (+98.93%). Their ratio stayed approximately 3.62. Existing retention accounting reserves multiple rounded carrier roots and metadata, so retained memory cannot be read as payload bytes in flight. At observed useful throughput, the encoded queue turnover estimate is about 47 ms; that is a dimensional estimate, not a measured ACK RTT. The access link has a configured 20 ms delay in each direction.

All five workload windows per arm passed, all workers joined, and no link loss, queue drops, provider timeout retransmits or recovery errors were reported. All 85 loaded probes per arm completed, without cancellation or failure. Completed-probe TTFB median/p95/max improved from 65.108/178.806/213.427 ms to 54.209/164.072/208.804 ms. Allocation per useful byte increased 6.6462→6.7619 B/B (+1.74%). Sampled budget invariants and final ownership drains passed; these samples do not qualify mobile Go peaks or total iOS footprint.

The result supports retained provider-send capacity as a causal limiter in this private clean-link fixture. It does not establish that deployed MAIN providers use this constrained profile or that the same change fixes phone Fast.com results. The fixture mostly leaves its client parent idle. Before changing defaults, verify reverse order, actual constructor profiles, live resizing and simultaneous client/provider traffic, control responsiveness, cancellation and both device memory profiles. Do not cut a busy client budget unconditionally.

Evidence: provider-tx-one32-pair.cpuaudit/{SUMMARY.json,COMPACT-SPEED-SUMMARY.json,SEQUENCE-TERMINAL.json,CACHE-RELEASED.json}; exact per-arm REPORT.json and READBACK.json. Additional readbacks: ../c49-native-relocated-execution.PREP.dupk0plj/provider-tx-queue-analysis.READBACK.cpuaudit/{PAIR-QUEUE-FACTS.json,PROBES-MEMORY-FAIRNESS.json}.

## Reverse-order confirmation

The second pair ran candidate before baseline. Both five-window workloads and source/module/cleanup checks passed. Baseline median was 132.1696 Mbps, candidate median 257.8830 Mbps (+95.12%). The first pair improved 95.23%, so the observed effect persists across both arm orders. There are two independent process pairs, not twenty independent trials; device, real mixed-role fairness and actual default-policy qualification remain outstanding. See PROVIDER-TX-TWO-PAIR-COMPARISON.json.

## Hosted-provider applicability and checkpoint

The current Linux daemon uses desktop-profile DeviceLocal targets of 128 or 256 MiB. SN chooses a desktop target from host capacity/provider count, with a 64 MiB fallback when host sizing is unknown. Those paths do not use the explicit mobile transfer hierarchy exercised above. A desktop128 provider has independent send/receive budgets of 5,752,188 / 7,669,584 bytes, with retained accounting disabled. Process sizing also affects opening windows; the cohosted consumer32 process defaults must not be mistaken for a separate server process. The selected deployed MAIN provider revision and profile remain unknown. These results therefore establish a mobile-provider fixture bottleneck, not a hosted Fast.com fix.

The checkpoint includes an opt-in ProviderSendBorrowByteCount setting (default zero). It expands provider send/parent permission under an unchanged root, leaves client/NAT/receive limits unchanged, and restores ordinary ceilings when disabled without evicting existing owners. Forty positive test executions and two exact old-behavior negatives passed, including existing affected controls. This policy has not been performance-qualified; it is distinct from the client-parent reallocation measured above. Full SetProvideMode integration, actual cancellation wiring, mixed-role traffic and real-device 32/64 MiB qualification remain outstanding.

A desktop constructor reporter is source-reviewed but unrun. Next: qualify it, construct a desktop-provider reference with independently projected consumer32 settings, measure actual receive refusals/queue pressure, and compare candidates only within the same declared topology. The measured mobile policy must not be silently enabled or generalized to deployed providers.
