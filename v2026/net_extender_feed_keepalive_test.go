//go:build unix

package connect

import (
	"context"
	"net"
	"net/netip"
	"testing"
	"time"
)

// The feed's tcp carrier keeps the kernel keepalive of ConnectSettings (5 s
// idle, then one probe after 5 s), which notices a dead extender about 10 s
// after the last packet. Nothing else of the feed is that quick: a
// subscription waits SubscribeIdleTimeout (90 s) for the server's 30 s
// keepalive frame, and a sample its 30 s budget. So every feed dial keeps it,
// a subscription with or without the idle bound and a sample alone.
func TestExtenderFeedDialKeepsTcpKeepAlive(t *testing.T) {
	cases := []struct {
		name                 string
		subscribe            bool
		subscribeIdleTimeout time.Duration
	}{
		{name: "a subscription with the idle bound", subscribe: true, subscribeIdleTimeout: 90 * time.Second},
		{name: "a sample alone", subscribe: false, subscribeIdleTimeout: 90 * time.Second},
		{name: "a subscription without the idle bound", subscribe: true, subscribeIdleTimeout: 0},
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
		netDialer := connectSettings.NetDialer()
		connectSettings.DialContextSettings = &DialContextSettings{
			DialContext: func(ctx context.Context, network string, address string) (net.Conn, error) {
				// the platform dialer, with its keepalive
				conn, err := netDialer.DialContext(ctx, network, address)
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
		if !keepAlive {
			t.Errorf("%s: TCP keepalive off, expected on", c.name)
		}
	}
}
