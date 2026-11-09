package connect

import (
	"context"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// The egress hook's dial fence (sidecar design §17.6's C5 contract, D-BRIDGE-21):
// no byte reaches a target after its flow's rule was revoked, a dial in
// progress included.
//
// register  a flow records its decision (verdict, target, rule tag, policy
//           generation, idle) before it dials, opens its socket or sends its
//           first echo, under its own context (ip_egress_hook.go)
// publish   a revocation swaps in the hook's new policy first, then calls
//           `CloseFlows` for the revoked rule's tags, which answers and ends
//           every flow registered with a tag below the new generation
// recheck   a flow whose dial completed reads the published generation once
//           more before its first byte; a newer one resolves the flow again,
//           and only the same verdict and target keep it
//
// A revocation between a flow's resolve and its register is the gap `CloseFlows`
// cannot see; the recheck closes it. Nothing here runs per packet: the recheck
// is one atomic load per flow, and the flows' idle timeouts are read once when
// their loops start.

// The flows of one NAT, as a hook reaches them.
type EgressFlowCloser interface {
	// Ends every flow registered with `ruleTag` at a policy generation below
	// the hook's published one, dials in progress included, each after an
	// answer to its source: a reset for TCP, an unreachable for UDP, nothing
	// for an ICMP echo. Returns how many it ended. Safe for concurrent use,
	// but not from a packet path: it scans every flow.
	CloseFlows(ruleTag uint64) int
}

// Implemented by a hook that wants to reach every NAT it is installed on, as a
// revocation must when provider generations overlap. Optional.
type EgressHookBinder interface {
	// Called when the hook is installed on a NAT, with that NAT, and outside
	// every lock of it. The returned function, if not nil, is called once when
	// the hook leaves the NAT: replaced, removed, or the NAT ended. Neither may
	// install a hook on the NAT they are given.
	BindFlowCloser(closer EgressFlowCloser) (unbind func())
}

// The NAT's `EgressFlowCloser`: answers and ends every flow registered with
// the tag below the installed hook's published generation, dials in progress
// included, scanning each send shard's buffers under their own mutexes. With no
// hook installed every flow registered with the tag ends, since no generation
// is published to keep one.
func (self *LocalUserNat) CloseFlows(ruleTag uint64) int {
	box := self.egress.hook.Load()
	var published uint64
	if box != nil {
		published = box.hook.PolicyGeneration()
	}
	revoked := self.egress.flows(func(state *egressFlowState) bool {
		registration := state.registration.Load()
		return registration != nil && registration.decision.RuleTag == ruleTag &&
			(box == nil || registration.box != box || registration.decision.PolicyGeneration < published)
	})
	closedCount := 0
	for _, handle := range revoked {
		if handle.closeEgress(true) {
			closedCount += 1
		}
	}
	return closedCount
}

// After a flow's dial completes and before its first byte. A flow that
// registered no decision passes unless an egress close ended it, so a flow of a
// NAT that never had a hook behaves as before, teardown included. Otherwise
// one atomic load of the published generation; when it moved past the
// decision's, the hook decides the flow again with its contract's principal,
// and only the same verdict and target keep it (plan-BRIDGE-small-readings
// #10), under the new decision. False ends the flow: the caller answers its
// source through its deny, which a close that ended it first has already done.
func (self *egressFlowState) recheck(
	ctx context.Context,
	source TransferPath,
	transferKey TransferKey,
	provideMode protocol.ProvideMode,
	ipPath *IpPath,
) bool {
	ended := func() bool {
		select {
		case <-ctx.Done():
			return true
		default:
			return self.closed.Load()
		}
	}
	registration := self.registration.Load()
	if registration == nil {
		return !self.closed.Load()
	}
	for {
		if ended() || self.nat.hook.Load() != registration.box {
			// a replaced hook's install closes this flow
			return false
		}
		published := registration.box.hook.PolicyGeneration()
		if published <= registration.decision.PolicyGeneration {
			return true
		}
		flow := EgressFlow{
			Source:        source,
			ProvideMode:   provideMode,
			ContractClass: self.source.contractClass,
			SourceSubject: self.source.sourceSubject,
			SourceNetwork: self.source.sourceNetwork,
			Principal:     registration.principal,
			Sealed:        self.source.verifiedClientKey != nil,
			Protocol:      ipPath.Protocol,
			From:          egressAddrPort(ipPath.SourceIp, ipPath.SourcePort, ipPath.Protocol),
			To:            egressAddrPort(ipPath.DestinationIp, ipPath.DestinationPort, ipPath.Protocol),
		}
		decision := registration.box.hook.ResolveEgress(&flow)
		dial, ok := egressDial(&flow, decision, ipPath.Version)
		// a decision older than the generation it answers is the hook's error,
		// and would ask again forever
		if !ok || decision.Verdict != registration.decision.Verdict || dial != registration.dial ||
			decision.PolicyGeneration < published {
			return false
		}
		registration = &egressRegistration{
			box:       registration.box,
			decision:  decision,
			principal: registration.principal,
			dial:      dial,
		}
		self.registration.Store(registration)
		// a revocation published since is caught by the loop's next load, or
		// by its CloseFlows, which now sees this registration
	}
}

// The flow's idle timeout: its decision's, else its NAT's.
func (self *egressFlowState) idleTimeout(natIdleTimeout time.Duration) time.Duration {
	if registration := self.registration.Load(); registration != nil && 0 < registration.decision.IdleTimeout {
		return registration.decision.IdleTimeout
	}
	return natIdleTimeout
}
