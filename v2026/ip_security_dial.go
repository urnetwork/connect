package connect

// The provider's destination rules for a socket a remote client has this
// host open by name, where no packet passes the provider policy. The
// device-hosted extender forwards a client's stream or datagrams to a host
// name it allows (EXTENDER.md A5, A7, G2), the operator's own or one under its
// wildcard, and the name resolves on this host, through this host's resolver.
// Whatever address that returns, private, loopback, or this host's own
// public one, the forward would reach this host's services or its LAN. So the
// check is the dialer's control: it judges the address each socket is about
// to connect to, after resolution, and a name rebound between a lookup and
// the dial changes nothing.
//
// The rules are the provider role's (docs/IP_SECURITY.md §2.2.4): the address
// must be public unicast (§2.2.1) and not provider-local (§2.2.2). The
// provider-local half reads the process snapshot under a guard held for the
// check, so with no provider policy live the check reads this host's
// addresses first, and with no successful read it fails closed.

import (
	"fmt"
	"net"
	"net/netip"
	"syscall"
)

// ProviderDialRefusedError is the error of a socket the provider dial control
// refused before it connected. Its text leaves the address to the dialer,
// whose error names it.
type ProviderDialRefusedError struct {
	// the address the socket was to connect to, as the dialer gave it
	Address string
	// not-public, provider-local or provider-local-unknown
	Reason SecurityPolicyReason
}

func (self *ProviderDialRefusedError) Error() string {
	return fmt.Sprintf("refused by the provider security policy: %s", self.Reason)
}

// ProviderDialControl is the control of a dialer (net.Dialer.Control,
// ConnectSettings.DialControl) whose destinations a remote client names: it
// refuses a socket whose address is not public unicast, or is local to this
// provider, before the socket connects, with a *ProviderDialRefusedError.
func ProviderDialControl(network string, address string, c syscall.RawConn) error {
	return processProviderLocalNetworks.inspectDial(address, DefaultProviderLocalSecurityPolicySettings())
}

// inspectDial judges the address a socket is about to connect to, "ip:port" as
// a dialer's control receives it, at the IPv6 width of settings. It holds a
// guard for the check, so a check that is the only guard reads this host's
// addresses first.
func (self *providerLocalNetworks) inspectDial(address string, settings *ProviderLocalSecurityPolicySettings) error {
	refuse := func(reason SecurityPolicyReason) error {
		return &ProviderDialRefusedError{
			Address: address,
			Reason:  reason,
		}
	}
	addrPort, err := netip.ParseAddrPort(address)
	if err != nil || !isPublicUnicastAddr(addrPort.Addr()) {
		return refuse(SecurityPolicyReasonNotPublic)
	}
	release := self.acquire()
	defer release()
	guard := &providerLocalGuard{
		settings:       settings,
		networks:       self,
		ipv6PrefixBits: settings.effectiveIpv6PrefixBits(),
	}
	switch guard.inspect(net.IP(addrPort.Addr().AsSlice())) {
	case providerLocalYes:
		return refuse(SecurityPolicyReasonProviderLocal)
	case providerLocalUnknown:
		return refuse(SecurityPolicyReasonProviderLocalUnknown)
	default:
		return nil
	}
}
