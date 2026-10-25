package connect

// platform_placement.go — the edge a platform-hosted client dials
// (server/connect/MOVE.md §13).
//
// A client that runs on the platform, such as a hosted proxy device or a
// provider probe, can read the platform's edge load model, so it does not leave
// its edge to DNS. route53 answers one weighted record per query and large
// resolvers cache that answer for the record's TTL, so a whole host of clients
// lands on whichever edge the cache held (MOVE.md §1). Such a client's
// transports ask a `PlatformPlacement` for the edge instead. The answer stands
// in for the platform host the way a move's target does (transport_move.go):
// the dial layers dial the literal while the TLS server name and the HTTP host
// keep the platform name, so a wrong or hostile answer can only fail a dial.
//
// The client keeps the answer, by family, for `PlacementPreferDuration`, so
// every transport of the client and every reconnect returns to the edge that
// holds its resident. A move's target or preference outranks it. A dial to a
// placed address that fails forgets the answer, reports the failure to the
// placement, and holds the address off for `PlacementFailHold`, so the next
// attempt asks again and, when every address offered is held, resolves the
// name.
//
// Placement applies to the carriers a move can redirect -- H1, and plain H3 on
// a space with no alt url -- and does not depend on `EnableMove`: a probe that
// declines moves (D15) is still placed when it connects.
//
// Concurrency: one `PlatformPlacement` serves every transport of its owner, so
// an implementation must be safe for concurrent use.

import (
	"net/netip"
)

// Chooses the edge a platform-hosted client dials. Asked on the dial path of
// every attempt that has no move target, move preference or kept placement,
// so it must answer from memory and not block.
type PlatformPlacement interface {
	// The edge addresses, at most one per family, to dial in place of
	// resolving `host`. None resolves the name.
	PlatformPlacementTargets(host string) []netip.Addr
	// A dial to `targetIps`, which the placement offered for `host`, failed.
	// The placement should stop offering them for a while.
	PlatformPlacementFailed(host string, targetIps []netip.Addr)
}
