package connect

import (
	"net/netip"
)

// TransportHint coerces the platform transports of one fixed destination's
// window clients (EXTENDER.md K10). It is client-side state: it rides a
// `ProviderSpec` but never the find-providers2 wire, and it does not take part
// in the spec fingerprint that scopes the window identities.
type TransportHint struct {
	// ExtenderIp, when valid, pins the destination to this extender address
	// and nothing else: its transports dial H1 through it only, with no direct
	// dial, no other extender and no H3. A failed dial waits for the address
	// on the transport's own schedule rather than the directory's hold.
	ExtenderIp netip.Addr
}

// The hint of the fixed spec whose client id is `destination`'s exit, nil for
// any other destination. A pinned stay is one fixed spec reached in one hop,
// so a multi-hop destination is never pinned.
func providerSpecsTransportHint(specs []*ProviderSpec, destination MultiHopId) *TransportHint {
	if destination.Len() != 1 {
		return nil
	}
	exitId := destination.Tail()
	for _, spec := range specs {
		if spec == nil || spec.ClientId == nil || spec.TransportHint == nil {
			continue
		}
		if *spec.ClientId == exitId && spec.TransportHint.ExtenderIp.IsValid() {
			hint := *spec.TransportHint
			return &hint
		}
	}
	return nil
}
