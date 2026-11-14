//go:build linux && !android

// The egress mark on Linux (egress_linux.go, QD-800) at every socket site the audit in egress_dial_sites_test.go
// says reaches the egress machinery, read back off each socket with SO_MARK.
//
// Setting a mark needs CAP_NET_ADMIN, or CAP_NET_RAW on a kernel that admits it. Where the process has neither, a
// dial must refuse rather than make a socket that follows the tunnel's route, and a datagram endpoint goes on unmarked
// and says so, as its bind failure is not fatal (transport_family.go); the tests check that instead. Both runs, from
// connect/, on a host with Docker:
//
//	CGO_ENABLED=0 GOOS=linux GOARCH=<the engine's arch> go test -c -o <dir>/connect.test .
//	docker run --rm --name sidecar-test-egress-mark --cap-add NET_ADMIN -v <dir>:/t:ro alpine:3.22 \
//	    /t/connect.test -test.run 'EgressMark|ApplyEgressInterface|SocketSites' -test.v
//	docker run --rm --name sidecar-test-egress-mark-unprivileged --cap-drop NET_ADMIN --cap-drop NET_RAW \
//	    -v <dir>:/t:ro alpine:3.22 /t/connect.test -test.run 'EgressMark|ApplyEgressInterface|SocketSites' -test.v

package connect

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	quic "github.com/quic-go/quic-go"
	"golang.org/x/sys/unix"
)

// Whether this process may set a socket's mark.
func testEgressMarkPermitted(t *testing.T) bool {
	t.Helper()
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	return unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_MARK, 0x7572) == nil
}

// The mark on conn's socket.
func testSocketMark(t *testing.T, conn syscall.Conn) int {
	t.Helper()
	raw, err := conn.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var mark int
	var readErr error
	if err := raw.Control(func(fd uintptr) {
		mark, readErr = unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_MARK)
	}); err != nil {
		t.Fatal(err)
	}
	if readErr != nil {
		t.Fatal(readErr)
	}
	return mark
}

// Sets the process's egress mark for the test, putting the previous one back at its end.
func setTestEgressMark(t *testing.T, mark uint32) {
	t.Helper()
	previous := EgressMark()
	SetEgressMark(mark)
	t.Cleanup(func() { SetEgressMark(previous) })
}

// A socket marked once it exists, its mark read back; a mark that fails is an error, never an unmarked socket.
func TestApplyEgressInterfaceMarksTheSocket(t *testing.T) {
	const mark = 0x7572
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	read := func() int {
		value, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_MARK)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	// no mark: nothing set, and the interface indexes stay inert on Linux
	AssertEqual(t, applyEgressInterface(uintptr(fd), 7, 7, 0), nil)
	AssertEqual(t, read(), 0)
	err = applyEgressInterface(uintptr(fd), 0, 0, mark)
	if testEgressMarkPermitted(t) {
		AssertEqual(t, err, nil)
		AssertEqual(t, read(), mark)
	} else {
		if err == nil {
			t.Fatal("a mark the process may not set was taken as set")
		}
		AssertEqual(t, read(), 0)
	}
	if err := applyEgressInterface(^uintptr(0), 0, 0, mark); err == nil {
		t.Fatal("an invalid socket was marked")
	}
}

// One site the audit says reaches the machinery.
type testEgressMarkSite struct {
	name string
	// makes the site's socket and answers the mark on it, closing it; an error where the site refused to make one
	open func(t *testing.T) (mark int, err error)
	// whether the site refuses a socket that cannot carry the mark, as a dial does, rather than going on unmarked
	refuses bool
}

// Every site the audit (allowedSocketSites) says reaches the egress machinery.
func testEgressMarkSites(t *testing.T) []testEgressMarkSite {
	ctx := context.Background()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()
	tcpAddress := listener.Addr().String()
	dialed := func(conn net.Conn, err error) (int, error) {
		if err != nil {
			return 0, err
		}
		defer conn.Close()
		return testSocketMark(t, conn.(syscall.Conn)), nil
	}
	return []testEgressMarkSite{
		{
			name: "net.go: ConnectSettings.NetDialer, tcp",
			open: func(t *testing.T) (int, error) {
				return dialed(DefaultConnectSettings().NetDialer().DialContext(ctx, "tcp4", tcpAddress))
			},
			refuses: true,
		},
		{
			name: "net.go: ConnectSettings.NetDialer, udp",
			open: func(t *testing.T) (int, error) {
				return dialed(DefaultConnectSettings().NetDialer().DialContext(ctx, "udp4", "127.0.0.1:9"))
			},
			refuses: true,
		},
		{
			name: "net.go: ConnectSettings.DialContext",
			open: func(t *testing.T) (int, error) {
				return dialed(DefaultConnectSettings().DialContext(ctx, "tcp", tcpAddress))
			},
			refuses: true,
		},
		{
			name: "transport_family.go: openH3PacketConn",
			open: func(t *testing.T) (int, error) {
				transport := &PlatformTransport{
					settings:       DefaultPlatformTransportSettings(),
					clientStrategy: &ClientStrategy{settings: &ClientStrategySettings{EnableNormal: true}},
					log:            loggerOrDefault(nil),
				}
				packetConn, _, pinned, err := transport.openH3PacketConn(ctx, TransportModeH3, "platform.invalid",
					&net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 443})
				if err != nil {
					return 0, err
				}
				defer packetConn.Close()
				mark := testSocketMark(t, packetConn.(syscall.Conn))
				// pinned says whether the mark took, and only then
				if pinned != (EgressMark() != 0 && mark == int(EgressMark())) {
					t.Errorf("openH3PacketConn says pinned=%t of a socket marked %#x", pinned, mark)
				}
				return mark, nil
			},
		},
		{
			name: "net_http_alt.go: the alt api's QUIC socket (openExtenderPacketConn, then applyEgress)",
			open: func(t *testing.T) (int, error) {
				altCtx := (&PlatformTransport{settings: DefaultPlatformTransportSettings()}).dialContext(ctx)
				connectSettings := DefaultConnectSettings()
				policy := newExtenderQuicMemoryPolicy(altCtx, connectSettings)
				read := errors.New("the socket was read")
				mark := -1
				wrap := func(_ context.Context, packetConn net.PacketConn) (net.PacketConn, error) {
					mark = testSocketMark(t, packetConn.(syscall.Conn))
					return nil, read
				}
				_, err := dialAltQuicAttempt(altCtx, connectSettings, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 443},
					wrap, &tls.Config{}, &quic.Config{}, policy)
				if !errors.Is(err, read) {
					return 0, err
				}
				// the attempt closed its socket on the way out, after wrap read its mark
				return mark, nil
			},
		},
		{
			name: "egress_net.go: egressNet.ListenUDP",
			open: func(t *testing.T) (int, error) {
				iceNet, err := newEgressNet(nil)
				if err != nil {
					t.Fatal(err)
				}
				conn, err := iceNet.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
				if err != nil {
					return 0, err
				}
				defer conn.Close()
				return testSocketMark(t, conn.(syscall.Conn)), nil
			},
		},
		{
			name: "egress_net.go: egressNet.ListenPacket",
			open: func(t *testing.T) (int, error) {
				iceNet, err := newEgressNet(nil)
				if err != nil {
					t.Fatal(err)
				}
				conn, err := iceNet.ListenPacket("udp4", "127.0.0.1:0")
				if err != nil {
					return 0, err
				}
				defer conn.Close()
				return testSocketMark(t, conn.(syscall.Conn)), nil
			},
		},
		{
			name: "egress_resolver_linux.go: egressResolverDial's marked path",
			open: func(t *testing.T) (int, error) {
				// a resolver at an address of this host's own, so the dial needs no route out
				server := testEgressLocalIpv4(t)
				path := filepath.Join(t.TempDir(), "resolv.conf")
				if err := os.WriteFile(path, []byte("nameserver "+server.String()+"\n"), 0600); err != nil {
					t.Fatal(err)
				}
				previousPaths := egressResolvConfPaths
				egressResolvConfPaths = []string{path}
				forgetEgressDnsServers()
				defer func() {
					egressResolvConfPaths = previousPaths
					forgetEgressDnsServers()
				}()
				if !egressProcessMarked() {
					t.Fatal("a process with its egress mark set reads as unmarked")
				}
				return dialed(egressResolverDial(ctx, "udp", "127.0.0.53:53"))
			},
			refuses: true,
		},
	}
}

// An IPv4 address of this host's own, not loopback.
func testEgressLocalIpv4(t *testing.T) net.IP {
	t.Helper()
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	for _, address := range addresses {
		ipNet, ok := address.(*net.IPNet)
		if ok && ipNet.IP.To4() != nil && ipNet.IP.IsGlobalUnicast() {
			return ipNet.IP.To4()
		}
	}
	t.Fatal("this host has no IPv4 address but loopback, which the egress resolver never dials")
	return nil
}

// With the egress mark set, every site's socket carries it; where the process may not set a mark, a dial refuses
// and a datagram endpoint goes on unmarked. With the mark cleared, every site makes its socket as before, unmarked.
func TestEgressMarkLandsAtEverySocketSite(t *testing.T) {
	const mark = 0x7572
	permitted := testEgressMarkPermitted(t)
	sites := testEgressMarkSites(t)

	setTestEgressMark(t, mark)
	for _, site := range sites {
		got, err := site.open(t)
		switch {
		case permitted && err != nil:
			t.Errorf("%s: %v", site.name, err)
		case permitted && got != mark:
			t.Errorf("%s: its socket carries the mark %#x", site.name, got)
		case permitted:
		case site.refuses && err == nil:
			t.Errorf("%s: made a socket that cannot carry the mark", site.name)
		case !site.refuses && err != nil:
			t.Errorf("%s: %v", site.name, err)
		case !site.refuses && got != 0:
			t.Errorf("%s: an unprivileged socket carries the mark %#x", site.name, got)
		}
	}

	SetEgressMark(0)
	for _, site := range sites {
		if site.name == "egress_resolver_linux.go: egressResolverDial's marked path" {
			// with no mark the resolver takes the system's own path, which egress_resolver_linux_test.go covers
			continue
		}
		got, err := site.open(t)
		if err != nil {
			t.Errorf("%s with no mark: %v", site.name, err)
		} else if got != 0 {
			t.Errorf("%s with no mark: its socket carries %#x", site.name, got)
		}
	}
}

// The process reads as marked to the resolver and the control-dial evidence while its egress mark is set, and not
// once it is cleared (where no cgroup program marks it).
func TestEgressMarkMarksTheProcess(t *testing.T) {
	cgroupMarked := egressSelfMarked()
	setTestEgressMark(t, 0x7572)
	AssertEqual(t, egressProcessMarked(), true)
	AssertEqual(t, egressBound(), true)
	SetEgressMark(0)
	AssertEqual(t, egressProcessMarked(), cgroupMarked)
	AssertEqual(t, egressBound(), cgroupMarked)
}
