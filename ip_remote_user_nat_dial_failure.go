package connect

// ip_remote_user_nat_dial_failure.go — the single-destination client's fast
// dial failure (IPV6.md C7).
//
// A provider whose upstream TCP dial fails answers the source's SYN with an
// ICMP destination-unreachable (ipOosUnreachable). ParseIpPath drops every
// ICMP, so the local stack never heard it: a family the provider cannot dial
// cost the full race fallback delay before the other family started. This
// gate admits exactly that signal and nothing else. The packet must be the
// exact signal a provider builds for the TCP flow its embed names
// (providerDialFailureEgressPath: code, outer addresses, family, lengths and
// checksums), sent by the provider the SYN went to, and the flow must be an
// outstanding SYN of this client by 5-tuple; providers embed sequence zero.
// Admission consumes that SYN; any reply on the flow clears it. Every other
// ICMP stays dropped. The multi client applies the same signal rule to its own
// intercept (ip_remote_multi_client.go).

import (
	"net/netip"
	"sync"
	"time"
)

const (
	// A SYN is outstanding only until its first reply, so the table holds
	// concurrent connects. A connect beyond the bound is not tracked and keeps
	// the delayed fallback.
	remoteUserNatPendingSynLimit = 256
	// A SYN older than this is no longer a live connect, and its unreachable
	// is late. This is the default tun dial timeout.
	remoteUserNatPendingSynLifetime = 30 * time.Second
)

// The flow of one outgoing SYN, source to destination.
type remoteUserNatSynKey struct {
	source      netip.AddrPort
	destination netip.AddrPort
}

type remoteUserNatPendingSyn struct {
	provider Id
	expires  time.Time
}

// Outstanding SYNs of one client. Safe for concurrent use; the zero value is
// ready.
type remoteUserNatDialFailures struct {
	stateLock sync.Mutex
	pending   map[remoteUserNatSynKey]remoteUserNatPendingSyn
}

func remoteUserNatSynKeyOf(source []byte, sourcePort int, destination []byte, destinationPort int) (remoteUserNatSynKey, bool) {
	sourceAddr, sourceOk := netip.AddrFromSlice(source)
	destinationAddr, destinationOk := netip.AddrFromSlice(destination)
	if !sourceOk || !destinationOk {
		return remoteUserNatSynKey{}, false
	}
	return remoteUserNatSynKey{
		source:      netip.AddrPortFrom(sourceAddr.Unmap(), uint16(sourcePort)),
		destination: netip.AddrPortFrom(destinationAddr.Unmap(), uint16(destinationPort)),
	}, true
}

// syn records an outgoing bare SYN sent to provider. A retransmission
// refreshes it; any other packet is ignored.
func (self *remoteUserNatDialFailures) syn(ipPath *IpPath, provider Id) {
	if ipPath.Protocol != IpProtocolTcp || !ipPath.Syn || ipPath.Ack {
		return
	}
	key, ok := remoteUserNatSynKeyOf(ipPath.SourceIp, ipPath.SourcePort, ipPath.DestinationIp, ipPath.DestinationPort)
	if !ok {
		return
	}
	now := time.Now()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.pending == nil {
		self.pending = map[remoteUserNatSynKey]remoteUserNatPendingSyn{}
	}
	if _, tracked := self.pending[key]; !tracked && remoteUserNatPendingSynLimit <= len(self.pending) {
		for pendingKey, pending := range self.pending {
			if !now.Before(pending.expires) {
				delete(self.pending, pendingKey)
			}
		}
		if remoteUserNatPendingSynLimit <= len(self.pending) {
			return
		}
	}
	self.pending[key] = remoteUserNatPendingSyn{
		provider: provider,
		expires:  now.Add(remoteUserNatPendingSynLifetime),
	}
}

// reply clears the SYN that an incoming SYN-ACK or reset answers.
func (self *remoteUserNatDialFailures) reply(ipPath *IpPath) {
	if ipPath.Protocol != IpProtocolTcp || !ipPath.Syn && !ipPath.Rst {
		return
	}
	key, ok := remoteUserNatSynKeyOf(ipPath.DestinationIp, ipPath.DestinationPort, ipPath.SourceIp, ipPath.SourcePort)
	if !ok {
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	delete(self.pending, key)
}

// admit reports whether packet, sent by provider, is the dial-failure signal
// for an outstanding SYN, and consumes that SYN. The returned path is the
// failed flow in the ingress direction.
func (self *remoteUserNatDialFailures) admit(provider Id, packet []byte) (*IpPath, bool) {
	egress, ok := providerDialFailureEgressPath(packet)
	if !ok || egress.Protocol != IpProtocolTcp {
		return nil, false
	}
	key, ok := remoteUserNatSynKeyOf(egress.SourceIp, egress.SourcePort, egress.DestinationIp, egress.DestinationPort)
	if !ok {
		return nil, false
	}
	now := time.Now()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	pending, tracked := self.pending[key]
	if !tracked || pending.provider != provider {
		return nil, false
	}
	delete(self.pending, key)
	if !now.Before(pending.expires) {
		return nil, false
	}
	return egress.Reverse(), true
}
