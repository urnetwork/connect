package extender

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"syscall"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// TCP and UDP have separate port namespaces. Hold both sockets on one real
// numeric port, without depending on the ephemeral allocator to collide.
func sharedFixturePortSockets(t *testing.T, mode connect.ExtenderConnectMode) (net.Listener, net.PacketConn, net.PacketConn) {
	t.Helper()
	var listener net.Listener
	var shared net.PacketConn
	for attempt := 0; attempt < 16; attempt++ {
		var err error
		listener, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		shared, err = net.ListenPacket("udp", listener.Addr().String())
		if err == nil {
			break
		}
		listener.Close()
		listener = nil
		if !errors.Is(err, syscall.EADDRINUSE) {
			t.Fatal(err)
		}
	}
	if listener == nil || shared == nil {
		t.Fatal("could not reserve a shared TCP/UDP test port")
	}
	t.Cleanup(func() { listener.Close() })
	t.Cleanup(func() { shared.Close() })
	other, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { other.Close() })
	switch mode {
	case connect.ExtenderConnectModeQuic:
		return listener, shared, other
	case connect.ExtenderConnectModeDns:
		return listener, other, shared
	default:
		t.Fatalf("unexpected shared carrier %q", mode)
		return nil, nil, nil
	}
}

// Use the existing pre-construction setup seam. Close its original sockets
// before substituting the explicitly reserved ones; the regression exercises
// the common fixture's port-map construction, not a copy of that construction.
func replaceFixturePortSockets(t *testing.T, fixture *extenderFixture, settings *ExtenderSettings, tcp net.Listener, quic, dns net.PacketConn) {
	t.Helper()
	oldTcp, err := settings.Listen("tcp", fmt.Sprintf(":%d", fixture.tcpPort))
	if err != nil {
		t.Fatal(err)
	}
	oldQuic, err := settings.ListenPacket("udp", fmt.Sprintf(":%d", fixture.quicPort))
	if err != nil {
		t.Fatal(err)
	}
	oldDns, err := settings.ListenPacket("udp", fmt.Sprintf(":%d", fixture.dnsPort))
	if err != nil {
		t.Fatal(err)
	}
	oldTcp.Close()
	oldQuic.Close()
	oldDns.Close()
	fixture.tcpPort = tcp.Addr().(*net.TCPAddr).Port
	fixture.quicPort = quic.LocalAddr().(*net.UDPAddr).Port
	fixture.dnsPort = dns.LocalAddr().(*net.UDPAddr).Port
	settings.Listen = func(network, address string) (net.Listener, error) {
		if network != "tcp" || address != fmt.Sprintf(":%d", fixture.tcpPort) {
			return nil, fmt.Errorf("unexpected shared-port TCP bind %s %s", network, address)
		}
		return tcp, nil
	}
	settings.ListenPacket = func(network, address string) (net.PacketConn, error) {
		if network != "udp" {
			return nil, fmt.Errorf("unexpected shared-port packet network %s", network)
		}
		switch address {
		case fmt.Sprintf(":%d", fixture.quicPort):
			return quic, nil
		case fmt.Sprintf(":%d", fixture.dnsPort):
			return dns, nil
		default:
			return nil, fmt.Errorf("unexpected shared-port UDP bind %s", address)
		}
	}
}

func TestExtenderFixtureSharedTcpUdpPortKeepsEveryCarrier(t *testing.T) {
	for _, mode := range []connect.ExtenderConnectMode{connect.ExtenderConnectModeQuic, connect.ExtenderConnectModeDns} {
		t.Run(string(mode), func(t *testing.T) {
			tcp, quic, dns := sharedFixturePortSockets(t, mode)
			fixture := newExtenderFixtureWithSetup(t, "127.0.0.1", nil, nil, func(fixture *extenderFixture, settings *ExtenderSettings) {
				replaceFixturePortSockets(t, fixture, settings, tcp, quic, dns)
			})
			sharedPort := fixture.quicPort
			if mode == connect.ExtenderConnectModeDns {
				sharedPort = fixture.dnsPort
			}
			if fixture.tcpPort != sharedPort || fixture.quicPort == fixture.dnsPort {
				t.Fatal("regression did not force exactly one TCP/UDP numeric collision")
			}
			select {
			case <-fixture.server.Listening():
			case <-time.After(5 * time.Second):
				t.Fatal("fixture did not finish binding")
			}
			if got, want := fixture.server.Carriers(), []string{connect.ExtenderCarrierTcp, connect.ExtenderCarrierQuic, connect.ExtenderCarrierDns}; !slices.Equal(got, want) {
				t.Fatalf("shared %s port erased a carrier: got %v, want %v", mode, got, want)
			}
			// Successful real requests prove the retained bind is usable, not
			// merely advertised. Keep the fixture's 10s phase bounds unchanged.
			for _, carrier := range testProbeCarriers {
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				probe, err := connect.ProbeExtenderLatency(ctx, fixture.connectSettings(), testProbeConfig(fixture, carrier, nil), nil)
				cancel()
				if err != nil || probe == nil || probe.Rtt <= 0 || probe.Outcome != connect.ExtenderPingUnattested {
					t.Fatalf("shared %s/%s probe failed: probe=%+v err=%v", mode, carrier, probe, err)
				}
			}
		})
	}
}

// Prebinding reserves ports before the server exists. A rejected bind never
// transfers ownership, so the fixture must still release the reserved socket.
func TestExtenderFixtureClosesPreboundSocketWhenCarrierRefused(t *testing.T) {
	for _, carrier := range testProbeCarriers {
		var unclaimed interface {
			SetDeadline(time.Time) error
			Close() error
		}
		t.Run(carrier, func(t *testing.T) {
			sentinel := errors.New("injected refusal of prebound fixture socket")
			fixture := newExtenderFixtureWithSetup(t, "127.0.0.1", nil, nil, func(fixture *extenderFixture, settings *ExtenderSettings) {
				if carrier == connect.ExtenderCarrierTcp {
					listener, err := settings.Listen("tcp", fmt.Sprintf(":%d", fixture.tcpPort))
					if err != nil {
						t.Fatal(err)
					}
					unclaimed = listener.(*net.TCPListener)
					settings.Listen = func(string, string) (net.Listener, error) { return nil, sentinel }
				} else {
					port := fixture.quicPort
					if carrier == connect.ExtenderCarrierDns {
						port = fixture.dnsPort
					}
					address := fmt.Sprintf(":%d", port)
					listen := settings.ListenPacket
					packet, err := listen("udp", address)
					if err != nil {
						t.Fatal(err)
					}
					unclaimed = packet
					settings.ListenPacket = func(network, requested string) (net.PacketConn, error) {
						if requested == address {
							return nil, sentinel
						}
						return listen(network, requested)
					}
				}
			})
			select {
			case <-fixture.server.Listening():
			case <-time.After(5 * time.Second):
				t.Fatal("fixture did not finish its refused bind")
			}
			if !errors.Is(fixture.server.ListenErrors()[carrier], sentinel) {
				t.Fatal("regression did not refuse the selected prebound carrier")
			}
		})
		if unclaimed == nil {
			t.Fatalf("%s: regression did not retain a prebound socket", carrier)
		}
		// Fallback cleanup also keeps the pre-fix negative run leak-free.
		err := unclaimed.SetDeadline(time.Now())
		unclaimed.Close()
		if !errors.Is(err, net.ErrClosed) {
			t.Errorf("%s: fixture cleanup left its unclaimed socket open: %v", carrier, err)
		}
	}
}
