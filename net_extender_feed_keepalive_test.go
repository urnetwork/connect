//go:build unix

package connect

import (
	"context"
	"net"
	"net/netip"
	"testing"
	"time"
)

// The feed keeps its own liveness: the extender keepalives an idle
// subscription every 30 s and the client drops one that is silent for
// SubscribeIdleTimeout (D4, E3), and a sample alone is bounded by the dial
// budget. So the carrier of a feed leaves TCP keepalive off, as a platform
// transport's does, rather than probing every idle subscription every few
// seconds. A subscription without the idle bound keeps it, since then the
// kernel's probes are the only thing that notices a dead extender.
func TestExtenderFeedDialLeavesTcpKeepAliveOffUnderItsOwnLiveness(t *testing.T) {
	cases := []struct {
		name                 string
		subscribe            bool
		subscribeIdleTimeout time.Duration
		expectedKeepAlive    bool
	}{
		{name: "a subscription with the idle bound", subscribe: true, subscribeIdleTimeout: 90 * time.Second, expectedKeepAlive: false},
		{name: "a sample alone", subscribe: false, subscribeIdleTimeout: 90 * time.Second, expectedKeepAlive: false},
		{name: "a subscription without the idle bound", subscribe: true, subscribeIdleTimeout: 0, expectedKeepAlive: true},
	}
	for _, c := range cases {
		// a stand-in extender that holds the connection once the outer
		// ClientHello arrives, so the dialed socket is still open to inspect
		// and its dial, keepalive decision included, is complete
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		helloConns := make(chan net.Conn, 1)
		go func() {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			if _, err := conn.Read(make([]byte, 1)); err != nil {
				conn.Close()
				return
			}
			helloConns <- conn
		}()

		networkClient, _, _ := newTestExtenderNetworkClient(t, newTestClock(), func(settings *ExtenderNetworkClientSettings) {
			settings.Subscribe = c.subscribe
			settings.SubscribeIdleTimeout = c.subscribeIdleTimeout
		})
		dialedConns := make(chan *net.TCPConn, 1)
		connectSettings := DefaultConnectSettings()
		connectSettings.DialContextSettings = &DialContextSettings{
			DialContext: func(ctx context.Context, network string, address string) (net.Conn, error) {
				// a plain dialer turns TCP keepalive on
				conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
				if err == nil {
					dialedConns <- conn.(*net.TCPConn)
				}
				return conn, err
			},
		}
		extenderConfig := &ExtenderConfig{
			Profile: ExtenderProfile{
				ConnectMode: ExtenderConnectModeTcpTls,
				ServerName:  "front.example",
				Port:        listener.Addr().(*net.TCPAddr).Port,
			},
			Ip: netip.MustParseAddr("127.0.0.1"),
		}
		feedDone := make(chan struct{})
		go func() {
			defer close(feedDone)
			networkClient.runFeed(connectSettings, extenderConfig, nil)
		}()

		var helloConn net.Conn
		select {
		case helloConn = <-helloConns:
		case <-time.After(10 * time.Second):
			t.Fatalf("%s: the feed did not reach the extender", c.name)
		}
		keepAlive := tcpKeepAliveEnabled(t, <-dialedConns)
		// the extender goes away, which ends the feed's outer handshake
		helloConn.Close()
		listener.Close()
		<-feedDone
		if keepAlive != c.expectedKeepAlive {
			t.Errorf("%s: TCP keepalive %t, expected %t", c.name, keepAlive, c.expectedKeepAlive)
		}
	}
}
