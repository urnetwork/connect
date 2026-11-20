package connect

// The provider dial control (ip_security_dial.go): the address of each socket
// a remote client named, judged before the socket connects, by itself and
// as the control of the dialers the device-hosted extender forwards through.

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// testProviderDialControl is ProviderDialControl over injected networks.
func testProviderDialControl(networks *providerLocalNetworks) func(network string, address string, c syscall.RawConn) error {
	return func(network string, address string, c syscall.RawConn) error {
		return networks.inspectDial(address, DefaultProviderLocalSecurityPolicySettings())
	}
}

// assertProviderDialRefused checks that err is the control's refusal of
// address with reason, however the dialer wrapped it.
func assertProviderDialRefused(t *testing.T, err error, address string, reason SecurityPolicyReason) {
	t.Helper()
	var refused *ProviderDialRefusedError
	if !errors.As(err, &refused) {
		t.Errorf("%s: err = %v, want a provider dial refusal (%s)", address, err, reason)
		return
	}
	if refused.Address != address || refused.Reason != reason {
		t.Errorf("%s: refused %s for %s, want %s", address, refused.Address, refused.Reason, reason)
	}
}

// Every address the control receives: not public, provider-local (the host,
// its links, the /56 around its IPv6 address, an observed address in both
// forms, service anycast) and public.
func TestProviderDialControlRefusesNonPublicAndLocalAddresses(t *testing.T) {
	networks, _ := newTestProviderLocalNetworks(t, newTestProviderLocalRead(t, "2a01:4f8:c17:1234::10/64", "198.51.100.20/32"))
	networks.setObservedAddresses([]netip.Addr{netip.MustParseAddr("203.0.113.77")})
	control := testProviderDialControl(networks)

	for _, c := range []struct {
		address string
		reason  SecurityPolicyReason
		allowed bool
	}{
		{address: "10.0.0.1:443", reason: SecurityPolicyReasonNotPublic},
		{address: "192.168.1.1:80", reason: SecurityPolicyReasonNotPublic},
		{address: "127.0.0.1:6379", reason: SecurityPolicyReasonNotPublic},
		{address: "[::1]:443", reason: SecurityPolicyReasonNotPublic},
		{address: "169.254.169.254:80", reason: SecurityPolicyReasonNotPublic},
		{address: "100.100.100.100:53", reason: SecurityPolicyReasonNotPublic},
		{address: "[fd00::1]:443", reason: SecurityPolicyReasonNotPublic},
		{address: "[fe80::1%en0]:22", reason: SecurityPolicyReasonNotPublic},
		{address: "[::ffff:10.0.0.1]:443", reason: SecurityPolicyReasonNotPublic},
		// the synthetic speed server is served in memory, never dialed
		{address: "198.18.0.1:443", reason: SecurityPolicyReasonNotPublic},
		{address: "not an address", reason: SecurityPolicyReasonNotPublic},
		{address: "[2a01:4f8:c17:1234::10]:22", reason: SecurityPolicyReasonProviderLocal},
		{address: "[2a01:4f8:c17:1234::1]:80", reason: SecurityPolicyReasonProviderLocal},
		{address: "[2a01:4f8:c17:12ab::5]:443", reason: SecurityPolicyReasonProviderLocal},
		{address: "198.51.100.20:8080", reason: SecurityPolicyReasonProviderLocal},
		{address: "203.0.113.77:443", reason: SecurityPolicyReasonProviderLocal},
		{address: "[::ffff:203.0.113.77]:443", reason: SecurityPolicyReasonProviderLocal},
		{address: "192.0.0.9:5351", reason: SecurityPolicyReasonProviderLocal},
		{address: "[2001:1::1]:5351", reason: SecurityPolicyReasonProviderLocal},
		{address: "192.0.0.10:3478", reason: SecurityPolicyReasonProviderLocal},
		{address: "[::ffff:192.0.0.10]:3478", reason: SecurityPolicyReasonProviderLocal},
		{address: "[2001:1::2]:3478", reason: SecurityPolicyReasonProviderLocal},
		{address: "[2001:1::3]:53", reason: SecurityPolicyReasonProviderLocal},
		{address: "198.51.100.7:443", allowed: true},
		{address: "[2001:4860:4860::8888]:443", allowed: true},
		// global anycast no network in front of a provider answers: as112
		{address: "192.31.196.1:53", allowed: true},
	} {
		err := control("tcp", c.address, nil)
		if c.allowed {
			if err != nil {
				t.Errorf("%s: refused: %v", c.address, err)
			}
			continue
		}
		assertProviderDialRefused(t, err, c.address, c.reason)
	}
}

// With no successful read of this host's addresses, every otherwise public
// address is refused as provider-local-unknown; the rules that need no read
// still give their own reasons.
func TestProviderDialControlFailsClosedBeforeTheFirstRead(t *testing.T) {
	read := newTestProviderLocalRead(t)
	read.fail(errors.New("netlink refused"))
	networks, _ := newTestProviderLocalNetworks(t, read)
	control := testProviderDialControl(networks)

	assertProviderDialRefused(t, control("tcp4", "198.51.100.7:443", nil), "198.51.100.7:443", SecurityPolicyReasonProviderLocalUnknown)
	assertProviderDialRefused(t, control("udp4", "192.0.0.9:5351", nil), "192.0.0.9:5351", SecurityPolicyReasonProviderLocal)
	assertProviderDialRefused(t, control("tcp4", "10.0.0.1:443", nil), "10.0.0.1:443", SecurityPolicyReasonNotPublic)

	read.set(t, "2a01:4f8:c17:1234::10/64")
	// the check is a guard: with no other guard live it reads before judging
	if err := control("tcp4", "198.51.100.7:443", nil); err != nil {
		t.Fatalf("a public address after a good read: %v", err)
	}
	if networks.guarded() {
		t.Fatal("the check kept its guard")
	}
}

// As the control of ConnectSettings, the dial the device-hosted extender
// forwards streams through: a name that resolves to loopback, to a private
// address, to this host's observed address or to port control anycast is
// refused before any socket connects, and the listener on loopback accepts
// nothing until a dial without the control reaches it.
func TestProviderDialControlRefusesResolvedNamesBeforeConnect(t *testing.T) {
	listener, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("no ipv6 loopback: %s", err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	accepted := make(chan net.Conn, 4)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			accepted <- conn
		}
	}()

	networks, _ := newTestProviderLocalNetworks(t, newTestProviderLocalRead(t, "2a01:4f8:c17:1234::10/64"))
	networks.setObservedAddresses([]netip.Addr{netip.MustParseAddr("203.0.113.77")})
	control := testProviderDialControl(networks)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, c := range []struct {
		name    string
		network string
		addr    netip.Addr
		reason  SecurityPolicyReason
	}{
		{name: "loopback.extender.example", network: "tcp6", addr: netip.MustParseAddr("::1"), reason: SecurityPolicyReasonNotPublic},
		{name: "lan.extender.example", network: "tcp4", addr: netip.MustParseAddr("10.0.0.1"), reason: SecurityPolicyReasonNotPublic},
		{name: "observed.extender.example", network: "tcp4", addr: netip.MustParseAddr("203.0.113.77"), reason: SecurityPolicyReasonProviderLocal},
		{name: "router.extender.example", network: "tcp6", addr: netip.MustParseAddr("2a01:4f8:c17:1234::1"), reason: SecurityPolicyReasonProviderLocal},
		{name: "pcp.extender.example", network: "tcp4", addr: netip.MustParseAddr("192.0.0.9"), reason: SecurityPolicyReasonProviderLocal},
	} {
		settings := DefaultConnectSettings()
		settings.Resolver = newFamilyTestResolverOwning(t, c.name, c.addr)
		settings.DialControl = control
		conn, err := settings.DialContext(ctx, c.network, net.JoinHostPort(c.name, strconv.Itoa(port)))
		if conn != nil {
			conn.Close()
			t.Fatalf("%s: connected to %s", c.name, c.addr)
		}
		assertProviderDialRefused(t, err, net.JoinHostPort(c.addr.String(), strconv.Itoa(port)), c.reason)
	}

	// the first connection the listener ever accepts is this one
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp6", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	select {
	case first := <-accepted:
		defer first.Close()
		if first.RemoteAddr().String() != conn.LocalAddr().String() {
			t.Fatalf("the listener accepted %s before the control dial %s", first.RemoteAddr(), conn.LocalAddr())
		}
	case <-ctx.Done():
		t.Fatal("the listener accepted nothing")
	}
}

// As the control of the datagram relay's dialer: a name that resolves to
// loopback gets no socket, and the listener's first datagram is one sent
// without the control.
func TestProviderDialControlRefusesResolvedDatagramNames(t *testing.T) {
	packetConn, err := net.ListenPacket("udp6", "[::1]:0")
	if err != nil {
		t.Skipf("no ipv6 loopback: %s", err)
	}
	defer packetConn.Close()
	port := packetConn.LocalAddr().(*net.UDPAddr).Port

	networks, _ := newTestProviderLocalNetworks(t, newTestProviderLocalRead(t, "2a01:4f8:c17:1234::10/64"))
	dialer := &net.Dialer{
		Resolver: newFamilyTestResolverOwning(t, "loopback.extender.example", netip.MustParseAddr("::1")),
		Control:  testProviderDialControl(networks),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := dialer.DialContext(ctx, "udp6", net.JoinHostPort("loopback.extender.example", strconv.Itoa(port)))
	if conn != nil {
		conn.Write([]byte("refused"))
		conn.Close()
		t.Fatal("the datagram dial to loopback got a socket")
	}
	assertProviderDialRefused(t, err, net.JoinHostPort("::1", strconv.Itoa(port)), SecurityPolicyReasonNotPublic)

	control, err := (&net.Dialer{}).DialContext(ctx, "udp6", packetConn.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	if _, err := control.Write([]byte("control")); err != nil {
		t.Fatal(err)
	}
	if err := packetConn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 64)
	n, _, err := packetConn.ReadFrom(buffer)
	if err != nil {
		t.Fatal(err)
	}
	if string(buffer[:n]) != "control" {
		t.Fatalf("the first datagram was %q", buffer[:n])
	}
}
