# Local SCTP dependency

This directory is the unmodified source distribution of
`github.com/pion/sctp v1.11.1`, except for the narrowly scoped change in
`association.go` and the added `association_cwnd_limited_test.go`.
The upstream MIT license and source headers are preserved.

Blocking SCTP writes admit one message at a time. A writer scheduling gap can
empty the internal pending queue while an application still has a bounded
backlog. Using only that instantaneous queue as the slow-start growth signal
suppresses growth under normal per-write service costs. The local change
records the last actually sent TSN when sending is blocked by cwnd (not rwnd),
retains that observation through its cumulative acknowledgement, and clears
it when cwnd decreases. It changes no initial/minimum cwnd, ACK policy,
reliability, packet format, congestion-avoidance increment, or loss timer.

Every main module consuming Connect must explicitly replace
`github.com/pion/sctp` with this directory; Go does not inherit replacements
from dependency modules. Connect, SDK main/build/cgo/js, server, proxy,
operator-proxy, and sn declare the replacement. Do not patch the Go module
cache or rely on an untracked
temporary modfile. Before distributing Connect outside this workspace,
publish/pin the reviewed upstream or maintained fork revision, or carry an
equivalent explicit replacement in that consumer.

Run `go test -race -short ./...` here as well as Connect's
`TestProviderUdpSctpCompactQueueAdmitsColdHighRttFixedOffer`,
`TestProviderUdpSctpCompactQueueServiceCostPreservesFixedOffer`, and loss/RTT
controls. The detailed experiments, rejected alternatives, and remaining
physical/profile requirements are recorded in `../../MEMSTEADY.md` and
`tests/PERFVAR-MEASUREMENTS.md`. This local integration is not a physical
performance or 24-MiB iOS-profile qualification.
