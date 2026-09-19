# Accepted cumulative-prefix test contracts

The original physical SDK train is legal, but its two ACKs do not prove destination FIFO. Exact125MB/s from a positive-wait cumulative prefix was therefore an insufficient-information assertion. Original source, exact failures and source-pinned before/after production results remain in `service-receiver-prefix-timing-v1`. This correction was explicitly reviewed and accepted before staging.

| Original name | Accepted replacement |
| --- | --- |
| TestWindowPacingSdkColdReceiverWaitMeasuresShortTrain | TestWindowPacingSdkColdReceiverWaitKeepsPrefixBounded |
| TestWindowPacingSdkColdDrainedTrainUsesExactPair | TestWindowPacingSdkColdDrainedPrefixKeepsRawClocks |

The replacements preserve exactly-once bytes, physical drain and raw RTT clocks, and forbid service above the independently specified physical upper bound. They permit measurement abstention. The new `TestWindowPacingSdkColdSingleHeadWaitMeasuresShortSerialization` uses two legal2,670-byte frames; its last ACK newly credits only its own head and therefore has an attributable exact receiver interval. It preserves the original positive short-serialization coverage at100/400ms RTT and both bucket phases.

All five other existing SDK cold definitions and all static SDK throughput thresholds are unchanged. Eventual pacing recovery remains a separate functional requirement; passing the revised component contracts does not close the static400ms long-profile failure.

The combined candidate passes20definitions times3 under verified race instrumentation: eight SDK cold, six prefix safety/ownership (including downstream reorder on one known local H1 route), and six aggregate arithmetic/lifecycle controls. This source includes the safe prefix restriction, raw aggregate cold/warm readers and pending-drain guards. It is a combined contract check, not an isolated production-effect comparison.

`test-contract.patch` and the two text-suffixed source copies make the exact reviewed oracle change inspectable without adding Go packages to the evidence tree. Manifests preserve original full source/build/runtime CWD/binary/status/raw-log hashes. No expected-failure wrapper or permanent skip was added.
