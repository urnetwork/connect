// The platform's ObservedAddresses: the public addresses it sees this client's
// transports connecting from (server connect/resident_observed_addresses.go).
// Behind a NAT these are the router's public addresses, and in a container
// the host's; neither is on any interface the provider-local guard can read
// (ip_security_local.go). The guard counts them as the provider's own
// addresses, so a consumer cannot reach the router by hairpin NAT to its
// public address.
package connect

import (
	"net/netip"

	"github.com/urnetwork/connect/protocol"
)

// receiveObservedAddresses replaces the provider-local guard's observed
// addresses with the platform's. A ReceiveFunction: frames are borrowed, and
// only the control source is trusted, so a peer cannot add or clear addresses
// through an ordinary data path. Every client applies the message, provider or
// not, so a provider started later on the same host starts with them. It never
// blocks: the guard republishes from its last interface read, and validates
// and bounds the addresses itself.
func (self *Client) receiveObservedAddresses(source TransferPath, frames []*protocol.Frame, peer Peer) {
	if !source.IsControlSource() {
		return
	}
	for _, frame := range frames {
		if frame.MessageType != protocol.MessageType_TransferObservedAddresses {
			continue
		}
		message, err := FromFrame(frame)
		if err != nil {
			continue
		}
		observedAddresses, ok := message.(*protocol.ObservedAddresses)
		if !ok {
			continue
		}
		addrs := make([]netip.Addr, 0, len(observedAddresses.Addresses))
		for _, addressBytes := range observedAddresses.Addresses {
			// a malformed address is skipped; the rest of the set stands
			if addr, ok := netip.AddrFromSlice(addressBytes); ok {
				addrs = append(addrs, addr)
			}
		}
		self.providerLocalNetworks().setObservedAddresses(addrs)
		if self.log.V(1).Enabled() {
			// the count, not the addresses: logs can leave the device in feedback
			self.log.Infof("[security]%s observed addresses count=%d\n", self.ClientTag(), len(addrs))
		}
	}
}

// The snapshot the platform's observed addresses feed: the process snapshot
// every provider policy reads, unless a test supplies its own.
func (self *Client) providerLocalNetworks() *providerLocalNetworks {
	if self.settings.providerLocalNetworksForTest != nil {
		return self.settings.providerLocalNetworksForTest
	}
	return processProviderLocalNetworks
}
