# Performance checkpoint, 2026-11-06

This is a development checkpoint, not release qualification. Tests before synchronization used Connect 4e625240, Server 29f80efd, SDK cf90cf6f and Tests 463ed3df. Upstream changes merged during synchronization require fresh source-bound validation.

Included: retained-SYN publication and full-window refresh ownership fixes; test wire-ownership repairs; bounded first-page diagnostic and recorder controls; an opt-in provider-send borrowing policy (default zero); a desktop-provider constructor reporter; and host Fast.com protocol metadata collection. Reviewed diagnostic/reporting sources that have not executed are deliberately included for the next developer.

Evidence before synchronization:

- Recovery: 115/115 roots passed normally and with race detection. The historical workload's 11 controls also passed in both modes.
- Original 100-page outage: all 100 HTML requests, 400 assets and 99 overlaps completed, late provider 4 served page 1, and the held CSS owner survived. It still failed the original five-second gate: page-1 TTFB 5.937593 s, TCP connect 5.485661 s. Median TTFB 883.901 ms; p95 2043.970 ms. The limit is unchanged.
- Full Connect: the maintained runner stopped on five discovery admission assertions. The fail-fast exit was 143; artifact integrity, module/source checks and cleanup passed. The unrun remainder has no coverage credit.
- Provider borrowing: 28 SDK and 12 Connect positive root executions passed, with two exact old-behavior negatives. This is correctness evidence, not performance/memory qualification.
- Mobile-provider synthetic bulk: two opposite-order fresh process pairs improved median useful speed 132.33→258.34 and 132.17→257.88 Mbps, about 95%. Each arm had five correlated windows. This does not establish the hosted-provider cause; see 20261106-PROVIDER-TX-RESEARCH.md.
- Host Fast.com collector: 64 controls passed and the old missing-protocol handler failed its negative. Historical phone runs still have unknown inner HTTP protocol.

The five outstanding full-suite failures are TestDiscoveryDemandActualAckTimerPublication, TestDiscoveryDemandIpv6FindsProviderAfterEmptyRead, TestDiscoveryDemandIpv6ReassertsDuringInflightEmptyRead, TestDiscoveryDemandFindsProviderAfterEmptyRead and TestDiscoveryDemandReassertsDuringInflightEmptyRead. Failure-only observations preserve their original assertions. A similar full-window manual-winner assumption is an adjacent unqualified risk. Do not change production just to force a manual SYN to win when automatic recovery may already have succeeded.

After this checkpoint: run the preserved five-root diagnostic and verify exact flow ownership/response before correcting tests; qualify the three first-page recorder controls normally/race, then replay the original workload with CONNECT_PERFVAR_PAGE1_PHASE_DIAGNOSTIC=1. Trace results are diagnostic, not a performance baseline. Keep overflow/unknown observations explicit and distinguish staggered dial attempts by the actual GotConn tuple. Debug the remaining delay, add deterministic causal tests, then run fresh complete correctness, PERFVAR/LOWBAR and real-device MEMSTEADY for iOS32/Android64. Do not claim a physical iOS under-50 MiB footprint from Android.

A private strict manual-source test draft is preserved as research/20261106-manual-syn-source-first.patch, not applied. It remains unqualified pending actual five-root diagnostics. Its old base predates the failure-only observations; preserve those via local-hunk/three-way integration if the draft is later adopted.
