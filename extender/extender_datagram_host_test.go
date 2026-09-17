package extender

import (
	"context"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// The destination an h3 client asks an extender to relay to is a NAME, carried
// in the extender header inside the outer tls, and resolved at the extender.
//
// Why it has to be a name. An extender's destination whitelist is a list of
// operator patterns -- `<host>` and `*.<host>` for the network space hosts --
// so an ip literal matches nothing and is refused outright. And resolving on
// the client needs working dns for the alt host on a client that may have
// none, which is half of what an extender is for.
//
// A loopback fixture cannot show any of this: its destination is already an ip
// and its whitelist is that same literal, so a client that resolved locally
// would look correct. These tests use a name the whitelist admits and no
// resolver can answer, which is the case that separates the two.

// hostCapture is an extender whose udp egress records what it was asked to
// reach, and answers with a socket that echoes.
type hostCapture struct {
	mutex     sync.Mutex
	addresses []string
}

func (self *hostCapture) record(address string) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	self.addresses = append(self.addresses, address)
}

func (self *hostCapture) seen() []string {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return append([]string(nil), self.addresses...)
}

// newHostCaptureExtender runs an extender on loopback that admits allowedHost
// and records every datagram destination it is asked for, without resolving
// it.
func newHostCaptureExtender(
	t *testing.T,
	ctx context.Context,
	allowedHost string,
) (*connect.ExtenderConfig, *hostCapture) {
	t.Helper()

	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatalf("release probe listener: %v", err)
	}

	capture := &hostCapture{}
	settings := DefaultExtenderSettings()
	// Stand in for the extender's own resolver and egress. Recording the
	// address it was handed is the whole assertion: a client that resolved
	// first would show an ip here.
	settings.DialPacketContext = func(
		ctx context.Context, network string, address string,
	) (net.Conn, error) {
		capture.record(address)
		return net.Dial("udp4", mustEchoAddr(t, ctx))
	}

	secret := "host-capture-secret"
	server := NewExtenderServer(
		ctx,
		[]string{secret},
		[]string{allowedHost},
		map[int][]connect.ExtenderConnectMode{port: {connect.ExtenderConnectModeTcpTls}},
		&net.Dialer{},
		settings,
	)
	go func() {
		_ = server.ListenAndServe()
	}()
	t.Cleanup(func() { server.Close() })
	// ListenAndServe binds asynchronously, so dialing straight after it is a
	// race the client loses by connection-refused. Wait for the carrier.
	waitForExtenderCarrier(t, port)

	return &connect.ExtenderConfig{
		Profile: connect.ExtenderProfile{
			ConnectMode: connect.ExtenderConnectModeTcpTls,
			ServerName:  "spoof.invalid",
			Port:        port,
		},
		Ip:     netip.MustParseAddr("127.0.0.1"),
		Secret: secret,
	}, capture
}

// mustEchoAddr is a udp socket that discards; the relay only needs somewhere
// to send.
func mustEchoAddr(t *testing.T, ctx context.Context) string {
	packetConn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("echo listen: %v", err)
	}
	go func() {
		<-ctx.Done()
		packetConn.Close()
	}()
	return packetConn.LocalAddr().String()
}

// The header carries the name, and the extender resolves it.
func TestExtenderPacketDialSendsTheHostnameInTheHeader(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const altHost = "alt.example"
	extenderConfig, capture := newHostCaptureExtender(t, ctx, altHost)

	connectSettings := connect.DefaultConnectSettings()
	packetConn, err := connect.NewExtenderPacketDialContext(connectSettings, extenderConfig)(
		ctx, "udp4", net.JoinHostPort(altHost, "443"),
	)
	if err != nil {
		t.Fatalf("extender packet dial: %v", err)
	}
	defer packetConn.Close()

	// The relay dials its destination when the request is accepted, which is
	// before any datagram flows.
	deadline := time.Now().Add(10 * time.Second)
	for len(capture.seen()) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}

	seen := capture.seen()
	if len(seen) == 0 {
		t.Fatal("the extender was never asked to reach a destination")
	}
	want := net.JoinHostPort(altHost, "443")
	if seen[0] != want {
		// An ip here means the client resolved first, which the whitelist
		// would refuse in production.
		t.Fatalf("extender was asked for %q, want the unresolved name %q", seen[0], want)
	}
}

// The reason the name matters: an extender that admits an operator name does
// not admit the address behind it, so a client that resolves first is refused.
func TestExtenderWhitelistRefusesAResolvedAddress(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	server := NewExtenderServerWithDefaults(
		ctx, nil, []string{"alt.example", "*.bringyour.com"}, nil, &net.Dialer{},
	)
	for _, allowed := range []string{"alt.example", "connect.bringyour.com"} {
		if !server.IsAllowedHost(allowed) {
			t.Errorf("%q should be an allowed destination", allowed)
		}
	}
	// What a client that resolved locally would send.
	for _, refused := range []string{"127.0.0.1", "10.1.2.3", "2001:db8::1"} {
		if server.IsAllowedHost(refused) {
			t.Errorf("%q is an address, not an operator name, and must be refused", refused)
		}
	}
}

// waitForExtenderCarrier blocks until the tcp carrier accepts, so a test dial
// cannot arrive before the bind.
func waitForExtenderCarrier(t *testing.T, port int) {
	t.Helper()
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp4", address, time.Second)
		if err == nil {
			conn.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("extender carrier on %s never accepted", address)
}
