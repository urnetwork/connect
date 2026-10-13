// IPV6.md C7: TunSettings.DialFirstIpFamily through two real bridged tun
// stacks. The resolver is a host-dialed loopback DoH server and the bridge is
// in memory, so nothing leaves the process.
package connect

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"
)

// packetIsTcp reports a TCP segment; gVisor's SYNs carry no extension header.
func packetIsTcp(packet []byte) bool {
	switch packetIpVersion(packet) {
	case 4:
		return 20 <= len(packet) && packet[9] == 6
	case 6:
		return 40 <= len(packet) && packet[6] == 6
	}
	return false
}

// The left tun dials the right one by name. The right listens on the given
// families at one port; the bridge records which family the left's first TCP
// segment used and black-holes the dropped family in both directions.
type tunFirstFamilyFixture struct {
	ctx      context.Context
	left     *Tun
	right    *Tun
	host     string
	port     string
	firstTcp atomic.Int32
	tcp      [7]atomic.Int64
}

func newTunFirstFamilyFixture(t *testing.T, leftSettings *TunSettings, listen []int, drop int) *tunFirstFamilyFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	t.Cleanup(cancel)
	self := &tunFirstFamilyFixture{ctx: ctx, host: "first-family.tun.test"}
	var err error
	self.right, err = CreateTun(ctx, tunTestSettings(6))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = self.right.Close() })
	right4 := tunTestLocalAddress(t, self.right, 4)
	right6 := tunTestLocalAddress(t, self.right, 6)
	dohServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeDohWire(w, r, []netip.Addr{right4, right6}, 60, false)
	}))
	t.Cleanup(dohServer.Close)
	leftSettings.Log = NewNoopLogger()
	self.left, err = CreateTunWithResolver(ctx, leftSettings, &DnsResolverSettings{
		EnableLocalDoh:   true,
		LocalDohUrlsIpv4: []string{dohServer.URL},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = self.left.Close() })

	keep := func(packet []byte) bool { return packetIpVersion(packet) != drop }
	bridgeTunFilter(ctx, self.right, self.left, func(packet []byte) bool {
		if packetIsTcp(packet) {
			version := packetIpVersion(packet)
			self.firstTcp.CompareAndSwap(0, int32(version))
			self.tcp[version].Add(1)
		}
		return keep(packet)
	})
	bridgeTunFilter(ctx, self.left, self.right, keep)

	port := 0
	for _, version := range listen {
		listener, err := self.right.ListenTCP(&net.TCPAddr{IP: net.IP(tunTestLocalAddress(t, self.right, version).AsSlice()), Port: port})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = listener.Close() })
		port = listener.Addr().(*net.TCPAddr).Port
		go func() {
			for {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				_ = conn.Close()
			}
		}()
	}
	if port == 0 {
		port = 443
	}
	self.port = itoa(port)
	return self
}

// Warms both answers so neither family's DNS can decide the order.
func (self *tunFirstFamilyFixture) warm(t *testing.T) {
	t.Helper()
	for _, recordType := range []string{"A", "AAAA"} {
		if _, authoritative := self.left.DohCache().QueryResult(self.ctx, recordType, self.host); !authoritative {
			t.Fatalf("failed to warm the %s answer", recordType)
		}
	}
}

// Dials through the tun's public entry point and returns the landed family.
func (self *tunFirstFamilyFixture) dial(t *testing.T, network string, host string) int {
	t.Helper()
	conn, err := self.left.DialContext(self.ctx, network, net.JoinHostPort(host, self.port))
	if err != nil {
		t.Fatalf("%s dial to %s: %v", network, host, err)
	}
	defer conn.Close()
	remote, err := netip.ParseAddrPort(conn.RemoteAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	return addrIpFamily(remote.Addr())
}

func tunFirstFamilySettings(first int) *TunSettings {
	settings := tunTestSettings(6)
	settings.DialFirstIpFamily = first
	return settings
}

// IPV6.md C7. With both answers ready, the named family's segment is the
// first one the tun sends, and the connection lands on it.
func TestTunDialFirstIpFamilyLeads(t *testing.T) {
	for _, first := range []int{4, 6} {
		self := newTunFirstFamilyFixture(t, tunFirstFamilySettings(first), []int{4, 6}, 0)
		self.warm(t)
		if landed := self.dial(t, "tcp", self.host); landed != first || int(self.firstTcp.Load()) != first {
			t.Fatalf("first v%d: first segment v%d, landed v%d", first, self.firstTcp.Load(), landed)
		}
	}
}

// IPV6.md C7. A black-holed first family is tried first and the connection
// falls back to, and lands on, the other family.
func TestTunDialFirstIpFamilyFallsBackFromBlackhole(t *testing.T) {
	for _, first := range []int{4, 6} {
		self := newTunFirstFamilyFixture(t, tunFirstFamilySettings(first), []int{4, 6}, first)
		self.warm(t)
		if landed := self.dial(t, "tcp", self.host); landed != otherIpFamily(first) || int(self.firstTcp.Load()) != first {
			t.Fatalf("first v%d black-holed: first segment v%d, landed v%d", first, self.firstTcp.Load(), landed)
		}
	}
}

// IPV6.md C7. A refused first family (no listener, so a reset) falls back to
// the other family.
func TestTunDialFirstIpFamilyFallsBackFromRefusal(t *testing.T) {
	for _, first := range []int{4, 6} {
		self := newTunFirstFamilyFixture(t, tunFirstFamilySettings(first), []int{otherIpFamily(first)}, 0)
		self.warm(t)
		if landed := self.dial(t, "tcp", self.host); landed != otherIpFamily(first) || int(self.firstTcp.Load()) != first {
			t.Fatalf("first v%d refused: first segment v%d, landed v%d", first, self.firstTcp.Load(), landed)
		}
	}
}

// IPV6.md C7. Pinned families are unchanged by a first family: a narrowed
// network, an IP literal and an IPv4-only tun send no segment of the other
// family and land on their own.
func TestTunDialFirstIpFamilyKeepsPinnedFamilies(t *testing.T) {
	ipv4Only := tunFirstFamilySettings(6)
	ipv4Only.Mtu = DefaultMtu
	for _, test := range []struct {
		name     string
		settings *TunSettings
		network  string
		literal  int
		want     int
	}{
		{name: "tcp4 with first v6", settings: tunFirstFamilySettings(6), network: "tcp4", want: 4},
		{name: "tcp6 with first v4", settings: tunFirstFamilySettings(4), network: "tcp6", want: 6},
		{name: "v4 literal with first v6", settings: tunFirstFamilySettings(6), network: "tcp", literal: 4, want: 4},
		{name: "v6 literal with first v4", settings: tunFirstFamilySettings(4), network: "tcp", literal: 6, want: 6},
		{name: "ipv4-only tun with first v6", settings: ipv4Only, network: "tcp", want: 4},
	} {
		self := newTunFirstFamilyFixture(t, test.settings, []int{4, 6}, 0)
		host := self.host
		if test.literal != 0 {
			host = tunTestLocalAddress(t, self.right, test.literal).String()
		}
		if landed := self.dial(t, test.network, host); landed != test.want || self.tcp[otherIpFamily(test.want)].Load() != 0 {
			t.Fatalf("%s: landed v%d, other-family segments %d", test.name, landed, self.tcp[otherIpFamily(test.want)].Load())
		}
	}
}

// IPV6.md C6, C7. Every tun but the validator's keeps the shared race: the
// defaults name no first family.
func TestTunDefaultSettingsKeepSharedDialRace(t *testing.T) {
	if first := DefaultTunSettings().DialFirstIpFamily; first != 0 {
		t.Fatalf("DefaultTunSettings().DialFirstIpFamily = %d, want 0", first)
	}
	tun, err := CreateTunWithDefaults(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tun.Close()
	if first := tun.settings.DialFirstIpFamily; first != 0 {
		t.Fatalf("default tun DialFirstIpFamily = %d, want 0", first)
	}
}
