//go:build windows

package connect

import (
	"context"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// The Windows half of the egress resolver: NetDialer must install it, and its
// Dial must substitute an egress-reachable DNS server for whatever poisoned
// server the Go resolver read out of the system configuration (on the
// tunnel-providing machine that configuration includes the tun's own mask
// resolver), on a socket that carries the forced-interface bind.

func TestNetDialerInstallsEgressResolver(t *testing.T) {
	settings := DefaultConnectSettings()
	if got := settings.NetDialer().Resolver; got != egressBoundResolver {
		t.Fatalf("NetDialer must install the egress-bound resolver on windows, got %v", got)
	}
	if !egressBoundResolver.PreferGo {
		t.Fatal("the egress resolver must use the in-process Go resolver; GetAddrInfoW queries are issued by svchost and follow the tun route")
	}
	custom := &net.Resolver{}
	settings.Resolver = custom
	if got := settings.NetDialer().Resolver; got != custom {
		t.Fatalf("a configured resolver must still win, got %v", got)
	}
}

// startDnsResponder serves one-shot A answers on loopback UDP and returns its
// port.
func startDnsResponder(t *testing.T, answer [4]byte) int {
	packetConn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		packetConn.Close()
	})
	go func() {
		buffer := make([]byte, 1500)
		for {
			n, remoteAddr, err := packetConn.ReadFrom(buffer)
			if err != nil {
				return
			}
			var query dnsmessage.Message
			if err := query.Unpack(buffer[:n]); err != nil || len(query.Questions) == 0 {
				continue
			}
			q := query.Questions[0]
			response := dnsmessage.Message{
				Header: dnsmessage.Header{
					ID:            query.Header.ID,
					Response:      true,
					RecursionDesired:   query.Header.RecursionDesired,
					RecursionAvailable: true,
				},
				Questions: query.Questions,
				Answers: []dnsmessage.Resource{
					{
						Header: dnsmessage.ResourceHeader{
							Name:  q.Name,
							Type:  dnsmessage.TypeA,
							Class: dnsmessage.ClassINET,
							TTL:   60,
						},
						Body: &dnsmessage.AResource{A: answer},
					},
				},
			}
			packed, err := response.Pack()
			if err != nil {
				continue
			}
			packetConn.WriteTo(packed, remoteAddr)
		}
	}()
	return packetConn.LocalAddr().(*net.UDPAddr).Port
}

func loopbackInterfaceIndex(t *testing.T) uint32 {
	interfaces, err := net.Interfaces()
	if err != nil {
		t.Fatal(err)
	}
	for _, ifc := range interfaces {
		if ifc.Flags&net.FlagLoopback != 0 && ifc.Flags&net.FlagUp != 0 {
			return uint32(ifc.Index)
		}
	}
	t.Fatal("no loopback interface")
	return 0
}

func TestEgressResolverDialSubstitutesAndBinds(t *testing.T) {
	port := startDnsResponder(t, [4]byte{127, 0, 0, 42})
	loIndex := loopbackInterfaceIndex(t)

	log := newRecordingLogger()
	SetDefaultLogger(log)
	t.Cleanup(func() {
		SetDefaultLogger(nil)
	})

	SetEgressInterfaceIndex(loIndex, 0)
	t.Cleanup(func() {
		SetEgressInterfaceIndex(0, 0)
	})
	// the discovered-server cache stands in for the egress adapter's
	// configured resolvers
	egressDnsServerCache.Store(&egressDnsServerList{
		index4:  loIndex,
		index6:  0,
		servers: []string{"127.0.0.1"},
		expires: time.Now().Add(time.Minute),
	})
	t.Cleanup(func() {
		egressDnsServerCache.Store(nil)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// 192.0.2.1 plays the poisoned server the Go resolver read from the
	// system configuration (TEST-NET, nothing listens); the dial must go to
	// the substituted server instead, on a socket that took the bind
	conn, err := egressResolverDial(ctx, "udp", net.JoinHostPort("192.0.2.1", "53"))
	// port substitution keeps the resolver's port; the responder is not on
	// :53, so exercise the wire exchange against the responder's port
	conn2, err2 := egressResolverDial(ctx, "udp", net.JoinHostPort("192.0.2.1", itoa(port)))
	if err != nil || err2 != nil {
		t.Fatalf("bound dial failed: %v %v", err, err2)
	}
	defer conn.Close()
	defer conn2.Close()
	if got := conn2.RemoteAddr().String(); got != net.JoinHostPort("127.0.0.1", itoa(port)) {
		t.Fatalf("the poisoned server was not substituted: dialed %s", got)
	}

	// the substituted, bound socket must carry a real DNS exchange
	query := dnsmessage.Message{
		Header: dnsmessage.Header{ID: 7, RecursionDesired: true},
		Questions: []dnsmessage.Question{
			{
				Name:  dnsmessage.MustNewName("capture-hole-test.example."),
				Type:  dnsmessage.TypeA,
				Class: dnsmessage.ClassINET,
			},
		},
	}
	packed, err := query.Pack()
	if err != nil {
		t.Fatal(err)
	}
	conn2.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := conn2.Write(packed); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 1500)
	n, err := conn2.Read(buffer)
	if err != nil {
		t.Fatal(err)
	}
	var response dnsmessage.Message
	if err := response.Unpack(buffer[:n]); err != nil {
		t.Fatal(err)
	}
	if len(response.Answers) != 1 {
		t.Fatalf("expected one answer, got %v", response.Answers)
	}
	a, ok := response.Answers[0].Body.(*dnsmessage.AResource)
	if !ok || a.A != [4]byte{127, 0, 0, 42} {
		t.Fatalf("unexpected answer %v", response.Answers[0])
	}

	// and the exchange left the control-dial evidence line
	if lines := log.linesWith("tag=dns"); len(lines) == 0 {
		t.Fatal("expected a tag=dns evidence line")
	}
}

func TestEgressResolverDialPassthroughUnbound(t *testing.T) {
	port := startDnsResponder(t, [4]byte{127, 0, 0, 43})
	SetEgressInterfaceIndex(0, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	// unbound (no tunnel provided by this process): the requested server is
	// dialed as-is, the plain platform behavior
	conn, err := egressResolverDial(ctx, "udp", net.JoinHostPort("127.0.0.1", itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if got := conn.RemoteAddr().String(); got != net.JoinHostPort("127.0.0.1", itoa(port)) {
		t.Fatalf("unbound dial must not substitute: %s", got)
	}
}

func TestEgressAdapterDnsServersWalksTheRealTable(t *testing.T) {
	// exercise the GetAdaptersAddresses walk against every adapter on the
	// machine; whatever it returns must have survived the usability filter
	interfaces, err := net.Interfaces()
	if err != nil {
		t.Fatal(err)
	}
	for _, ifc := range interfaces {
		servers, err := egressAdapterDnsServers(uint32(ifc.Index), 0)
		if err != nil {
			t.Fatalf("adapter walk failed for %s: %v", ifc.Name, err)
		}
		for _, server := range servers {
			if strings.HasPrefix(server, "127.") || server == DefaultDnsUpgradeMaskAddress {
				t.Fatalf("unusable server %s survived the filter on %s", server, ifc.Name)
			}
		}
	}
}

func itoa(port int) string {
	return strconv.Itoa(port)
}
