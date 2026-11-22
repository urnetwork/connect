// A physical P2P endpoint carries an application association only for its own
// live stream. Intermediaries and ordinary direct peers retain their wire form.
package connect

import (
	"context"
	"net"
	"sync/atomic"

	"github.com/pion/webrtc/v4"
)

// The stream registry publishes application authentication before enabling
// its readiness exchange. Every physical reconnect reads the same association.
type streamForwardingBinding struct {
	readiness   *p2pStreamReadiness
	association atomic.Pointer[streamForwardingAssociation]
}

// Both receipt lanes share a binding but keep the physical generation context
// and the complete-message ceiling of the actual carrier that delivered them.
type streamForwardingReceiveLane struct {
	streamId   Id
	adjacentId Id
	ctx        context.Context
	binding    *streamForwardingBinding
	wireLimit  int
}

// The caller owns wire throughout. A confirmed origin is the granted remote
// endpoint; the adjacent peer remains authoritative for ordinary raw traffic.
func (self *streamForwardingReceiveLane) open(wire []byte, localId, adjacentId Id) ([]byte, *streamForwardingAssociation, byte, bool) {
	if self == nil || self.ctx.Err() != nil || adjacentId == ControlId || self.adjacentId != adjacentId {
		return nil, nil, 0, false
	}
	association := self.binding.association.Load()
	if association == nil || association.grant.path.StreamId != self.streamId || association.receivePath.SourceId == adjacentId || association.receivePath.DestinationId != localId {
		return nil, nil, 0, false
	}
	frame, class, ok := association.open(wire, self.wireLimit)
	if !ok || self.ctx.Err() != nil {
		return nil, nil, 0, false
	}
	return frame, association, class, true
}

// Only a proved direct logical peer can use a legacy endpoint alias. Explicit
// stream control routing and compatibility transports retain their own match.
func (self *P2pSendTransport) matchesEndpointDestination(original TransferPath) bool {
	if !self.endpointDestinationRequired || original.DestinationId == (Id{}) && original.StreamId == self.streamId {
		return true
	}
	if self.forwarding == nil {
		return self.peerId != (Id{}) && original.DestinationId == self.peerId
	}
	association := self.forwarding.association.Load()
	return association.live() && association.grant.path.StreamId == self.streamId && original.DestinationId == association.sendPath.DestinationId
}

// The forwarded trailer fits inside each existing complete-message ceiling.
// Native fragmentation and the actual SCTP association can tighten that limit.
func streamForwardingWireLimit(conn net.Conn, settings *P2pTransportSettings, delay *p2pTransportDelay, fast bool) int {
	limit := max(0, settings.MaxMessageByteCount)
	peer, isPeer := conn.(*peerConn)
	if fast {
		limit = min(limit, p2pFastPathMaximumFragmentCount*p2pFastPathFragmentPayloadByteCount)
		if isPeer && peer.settings != nil && peer.settings.MaxMessageSize > 0 && ByteCount(limit) > peer.settings.MaxMessageSize {
			limit = int(peer.settings.MaxMessageSize)
		}
	} else if isPeer {
		if peer.settings != nil && peer.settings.MaxMessageSize > 0 && ByteCount(limit) > peer.settings.MaxMessageSize {
			limit = int(peer.settings.MaxMessageSize)
		}
		// Native Pion exposes the actual association maximum. The browser
		// implementation exposes no capabilities method.
		if peer.pc != nil && peer.pc.SCTP() != nil {
			if native, available := any(peer.pc.SCTP()).(interface {
				GetCapabilities() webrtc.SCTPCapabilities
			}); available {
				if negotiated := native.GetCapabilities().MaxMessageSize; negotiated > 0 && uint64(negotiated) < uint64(limit) {
					limit = int(negotiated)
				}
			}
		}
	}
	return max(0, limit-delay.headerByteCount())
}

// Selects a whole-message bound for the configured physical write policy.
func (self *P2pSendTransport) forwardingWireLimit() int {
	legacy := streamForwardingWireLimit(self.conn, self.settings, self.delay, false)
	if self.settings.DataPlaneMode == P2pDataPlaneModeLegacyOnly {
		return legacy
	}
	fast := streamForwardingWireLimit(self.conn, self.settings, self.delay, true)
	if self.settings.DataPlaneMode == P2pDataPlaneModeFastOnly {
		return fast
	}
	if _, capable := self.conn.(webRtcFastPathConn); capable {
		return min(legacy, fast)
	}
	return legacy
}

// The native writer lends its plaintext and charged contract only for the
// synchronous dispatch. The selected route receives the original wire share.
type streamForwardingWrite struct {
	inner      []byte
	contractId *Id
}

// A plaintext Pack or native Ack has to belong to the same live association
// that will protect the physical write. An opaque wrap needs the plaintext
// held by its local sealer. Unscoped callers can use an ordinary carrier.
func (self streamForwardingWrite) matches(binding *streamForwardingBinding) bool {
	association := binding.association.Load()
	if !association.live() {
		return false
	}
	class, reciprocal := association.classifyFrame(self.inner, association.sendPath, self.contractId)
	// The initial reverse acceptance has to reach the provider on a
	// verified platform lane. Its first authenticated return confirms it.
	return class == streamForwardingPack ||
		class == streamForwardingAck && (reciprocal == nil || reciprocal.peerConfirmed.Load())
}

// Apply scope before handing the selector's wire share to a carrier, even on
// a blocking retry. A missing eligible affinity or reliable route falls back
// as it would when that kind of carrier is absent. A full eligible queue
// keeps its existing backpressure.
func (self *routeSnapshot) forwardingWriteRoutes(preferred TransportType, reliableOnly bool, scope streamForwardingWrite, wireByteCount int) ([]Route, bool) {
	routes := self.writeRoutesFor(preferred, reliableOnly)
	if !self.hasForwardingRoute {
		return routes, true
	}
	eligible, oversized := self.eligibleForwardingRoutes(routes, scope, wireByteCount)
	if len(eligible) == 0 && reliableOnly && len(routes) < len(self.reliableRoutes) {
		eligible, oversized = self.eligibleForwardingRoutes(self.reliableRoutes, scope, wireByteCount)
	}
	if len(eligible) == 0 && len(routes) < len(self.routes) {
		// The original pinned/preferred set has no carrier for this wire.
		// Reapplying that pin would hide its eligible ordinary sibling.
		eligible, oversized = self.eligibleForwardingRoutes(self.shuffled(), scope, wireByteCount)
	}
	return eligible, len(eligible) > 0 || !oversized
}

// Return a contiguous view for the common one- and two-route cases. Only
// disjoint eligible runs need their own slice. Published slices stay immutable.
func (self *routeSnapshot) eligibleForwardingRoutes(routes []Route, scope streamForwardingWrite, wireByteCount int) ([]Route, bool) {
	var eligible []Route
	first, last := -1, -1
	oversized := false
	for index, route := range routes {
		properties := self.routeCarrierProperties[route]
		if binding := properties.forwardingSend; binding != nil {
			if !scope.matches(binding) {
				continue
			}
			if wireByteCount > properties.forwardingInnerLimit {
				oversized = true
				continue
			}
		}
		if first == -1 {
			first = index
		} else if eligible == nil && last+1 != index {
			eligible = make([]Route, 0, len(routes)-1)
			eligible = append(eligible, routes[first:last+1]...)
		}
		if eligible != nil {
			eligible = append(eligible, route)
		}
		last = index
	}
	if first >= 0 && eligible == nil {
		eligible = routes[first : last+1]
	}
	return eligible, oversized
}
