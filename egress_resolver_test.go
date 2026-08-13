package connect

import (
	"context"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"
)

// The egress-aware resolver exists to close the control-plane capture hole:
// bound sockets escaped the tun route, but the NAMES they dialed resolved
// through the OS resolver, whose wire query followed the tun default route to
// the tunnel's own (not yet working) resolver. These tests pin the portable
// half: the resolver selection, the server usability filter, and the
// control-dial evidence lines.

func TestEgressAwareResolverPrefersCustom(t *testing.T) {
	custom := &net.Resolver{}
	if got := egressAwareResolver(custom); got != custom {
		t.Fatalf("a configured resolver must win over the egress resolver, got %v", got)
	}
}

func TestUsableEgressDnsServer(t *testing.T) {
	mustAddr := func(s string) netip.Addr {
		addr, err := netip.ParseAddr(s)
		if err != nil {
			t.Fatal(err)
		}
		return addr
	}
	testCases := []struct {
		name   string
		addr   string
		index4 uint32
		index6 uint32
		usable bool
	}{
		{"public v4", "1.1.1.1", 16, 0, true},
		{"lan v4", "192.168.1.1", 16, 0, true},
		{"loopback stub", "127.0.0.1", 16, 0, false},
		{"unspecified", "0.0.0.0", 16, 0, false},
		{"the tunnel's own mask resolver", DefaultDnsUpgradeMaskAddress, 16, 0, false},
		{"v4 with no v4 bind", "1.1.1.1", 0, 23, false},
		{"public v6", "2620:fe::fe", 0, 23, true},
		{"v6 with no v6 bind", "2620:fe::fe", 16, 0, false},
		{"site-local auto-config junk", "fec0:0:0:ffff::1", 0, 23, false},
		{"v6 loopback", "::1", 0, 23, false},
	}
	for _, tc := range testCases {
		if got := usableEgressDnsServer(mustAddr(tc.addr), tc.index4, tc.index6); got != tc.usable {
			t.Errorf("%s: usableEgressDnsServer(%s, %d, %d) = %t, want %t", tc.name, tc.addr, tc.index4, tc.index6, got, tc.usable)
		}
	}
}

func TestWrapControlDialEvidence(t *testing.T) {
	log := newRecordingLogger()
	dialCount := 0
	dial := wrapControlDial("evtag-"+t.Name(), log, true, func(ctx context.Context, network string, addr string) (net.Conn, error) {
		dialCount += 1
		c1, c2 := net.Pipe()
		t.Cleanup(func() {
			c1.Close()
			c2.Close()
		})
		return c1, nil
	})

	// with no egress bound (mobile/app builds) the wrapper is silent
	SetEgressInterfaceIndex(0, 0)
	if _, err := dial(context.Background(), "tcp", "203.0.113.7:443"); err != nil {
		t.Fatal(err)
	}
	if lines := log.linesWith("[egress]dial"); len(lines) != 0 {
		t.Fatalf("no evidence line expected while unbound, got %v", lines)
	}

	// with an egress bound (the service in Connecting/Connected) each dial
	// logs once per tag+target per interval, then count-suppresses
	SetEgressInterfaceIndex(16, 0)
	t.Cleanup(func() {
		SetEgressInterfaceIndex(0, 0)
	})
	for range 3 {
		if _, err := dial(context.Background(), "tcp", "203.0.113.7:443"); err != nil {
			t.Fatal(err)
		}
	}
	lines := log.linesWith("[egress]dial")
	if len(lines) != 1 {
		t.Fatalf("expected exactly one evidence line (repeats suppressed), got %v", lines)
	}
	line := lines[0]
	for _, want := range []string{"tag=evtag-" + t.Name(), "203.0.113.7:443", "if=4:16/6:0", "bound=yes", "local="} {
		if !strings.Contains(line, want) {
			t.Fatalf("evidence line missing %q: %s", want, line)
		}
	}
	if dialCount != 4 {
		t.Fatalf("the wrapper must not swallow dials: %d", dialCount)
	}

	// the suppressed count surfaces on the next allowed line, matching the
	// existing "(N suppressed)" pattern
	throttle := controlDialThrottle("evtag-" + t.Name() + "|203.0.113.7:443")
	throttle.lastNanos.Store(time.Now().Add(-2 * controlDialLogInterval).UnixNano())
	if _, err := dial(context.Background(), "tcp", "203.0.113.7:443"); err != nil {
		t.Fatal(err)
	}
	lines = log.linesWith("(2 suppressed)")
	if len(lines) != 1 {
		t.Fatalf("expected the suppressed count on the next line, got %v", log.linesWith("[egress]dial"))
	}
}

func TestResolveEgressUDPAddrIpLiteral(t *testing.T) {
	// an ip literal never needs a resolver, bound or not
	SetEgressInterfaceIndex(16, 0)
	t.Cleanup(func() {
		SetEgressInterfaceIndex(0, 0)
	})
	udpAddr, err := resolveEgressUDPAddr(context.Background(), "9.9.9.9:443")
	if err != nil {
		t.Fatal(err)
	}
	if udpAddr.String() != "9.9.9.9:443" {
		t.Fatalf("got %s", udpAddr)
	}
}

func TestResolveEgressUDPAddrUnboundMatchesNet(t *testing.T) {
	SetEgressInterfaceIndex(0, 0)
	udpAddr, err := resolveEgressUDPAddr(context.Background(), "localhost:53")
	if err != nil {
		t.Fatal(err)
	}
	want, err := net.ResolveUDPAddr("udp", "localhost:53")
	if err != nil {
		t.Fatal(err)
	}
	if udpAddr.Port != want.Port {
		t.Fatalf("got %s want %s", udpAddr, want)
	}
}
