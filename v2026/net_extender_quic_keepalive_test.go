package connect

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"io"
	"math/big"
	"net"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/testutils/simnet"
)

// Every extender QUIC carrier sends a QUIC PING one ping timeout after the
// peer's last packet, whether or not a platform transport owns it, and the
// idle timeout is the same either way.
func TestExtenderQuicCarrierKeepsAPingTimeoutKeepAlive(t *testing.T) {
	settings := DefaultPlatformTransportSettings()
	connectSettings := DefaultConnectSettings()
	owned := newExtenderQuicMemoryPolicy(
		(&PlatformTransport{settings: settings}).dialContext(context.Background()),
		connectSettings,
	)
	standalone := newExtenderQuicMemoryPolicy(context.Background(), connectSettings)

	for _, c := range []struct {
		name   string
		policy extenderQuicMemoryPolicy
	}{
		{name: "a platform transport's carrier", policy: owned},
		{name: "a standalone carrier", policy: standalone},
	} {
		if c.policy.quicConfig.KeepAlivePeriod != settings.PingTimeout {
			t.Errorf("%s keeps a %s QUIC keepalive, expected the %s ping timeout", c.name, c.policy.quicConfig.KeepAlivePeriod, settings.PingTimeout)
		}
		if c.policy.quicConfig.MaxIdleTimeout != 4*settings.PingTimeout {
			t.Errorf("%s has a %s idle timeout, expected four ping timeouts", c.name, c.policy.quicConfig.MaxIdleTimeout)
		}
	}
}

// Drops every packet while drop is set.
type extenderQuicDroppingRouter struct {
	simnet.PerfectRouter
	drop atomic.Bool
}

func (self *extenderQuicDroppingRouter) SendPacket(p simnet.Packet) error {
	if self.drop.Load() {
		return nil
	}
	return self.PerfectRouter.SendPacket(p)
}

// A server certificate for the stand-in extender. The carrier's client skips
// verification here: the test is about liveness, not identity.
func extenderQuicKeepAliveTestTlsConfig(t *testing.T) *tls.Config {
	t.Helper()
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "extender.example"},
		DNSNames:     []string{"extender.example"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	certificateDer, err := x509.CreateCertificate(rand.Reader, template, template, &privateKey.PublicKey, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{certificateDer}, PrivateKey: privateKey}},
		NextProtos:   []string{"extender-keepalive-test"},
	}
}

// A dead extender is noticed within one ping timeout and the idle timeout of
// its last packet, 25 s by default, as with the QUIC keepalive every carrier
// had before CPUOPT2. QUIC closes a connection the idle timeout after the
// first ack-eliciting packet that follows the peer's last packet, and the
// carrier's QUIC PING sends that packet one ping timeout after the peer's last
// one.
//
// The carrier runs over a simulated link on the synctest clock, which drops
// every packet once the extender has acknowledged the carrier's last write.
// That write lands just after a wakeup instant. With owned, the carrier then
// writes the platform transport's pings, each due at the first wakeup instant
// a ping timeout after the transport's last write.
func expectExtenderQuicCarrierNoticesADeadExtenderInTime(t *testing.T, owned bool) {
	synctest.Test(t, func(t *testing.T) {
		settings := DefaultPlatformTransportSettings()
		dialCtx := context.Background()
		if owned {
			dialCtx = (&PlatformTransport{settings: settings}).dialContext(dialCtx)
		}
		quicConfig := newExtenderQuicMemoryPolicy(dialCtx, DefaultConnectSettings()).quicConfig.Clone()
		if quicConfig.MaxIdleTimeout != 4*settings.PingTimeout {
			t.Fatalf("the test assumes a carrier idle timeout of four ping timeouts, got %s", quicConfig.MaxIdleTimeout)
		}

		router := &extenderQuicDroppingRouter{}
		network := &simnet.Simnet{Router: router}
		linkSettings := simnet.NodeBiDiLinkSettings{Latency: time.Millisecond}
		clientPacketConn := network.NewEndpoint(&net.UDPAddr{IP: net.ParseIP("192.0.2.1"), Port: 50000}, linkSettings)
		serverPacketConn := network.NewEndpoint(&net.UDPAddr{IP: net.ParseIP("192.0.2.2"), Port: 443}, linkSettings)
		if err := network.Start(); err != nil {
			t.Fatal(err)
		}
		defer func() {
			clientPacketConn.Close()
			serverPacketConn.Close()
			network.Close()
		}()

		// the stand-in extender, with the extender's 30 s QUIC idle timeout
		serverTlsConfig := extenderQuicKeepAliveTestTlsConfig(t)
		listener, err := quic.Listen(serverPacketConn, serverTlsConfig, &quic.Config{MaxIdleTimeout: 30 * time.Second})
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()

		dialTimeoutCtx, dialCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer dialCancel()
		conn, err := quic.Dial(dialTimeoutCtx, clientPacketConn, serverPacketConn.LocalAddr(), &tls.Config{
			InsecureSkipVerify: true,
			NextProtos:         serverTlsConfig.NextProtos,
		}, quicConfig)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.CloseWithError(0, "")
		serverConn, err := listener.Accept(dialTimeoutCtx)
		if err != nil {
			t.Fatal(err)
		}
		defer serverConn.CloseWithError(0, "")
		stream, err := conn.OpenStreamSync(dialTimeoutCtx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := stream.Write([]byte{0}); err != nil {
			t.Fatal(err)
		}
		serverStream, err := serverConn.AcceptStream(dialTimeoutCtx)
		if err != nil {
			t.Fatal(err)
		}
		go io.Copy(io.Discard, serverStream)

		// the last write, just after a wakeup instant
		time.Sleep(time.Until(WakeupTime(time.Now(), settings.PingTimeout).Add(100 * time.Millisecond)))
		if _, err := stream.Write(make([]byte, 64)); err != nil {
			t.Fatal(err)
		}
		lastWriteTime := time.Now()
		// the extender acknowledges it within its 25 ms ack delay, then dies
		time.Sleep(100 * time.Millisecond)
		router.drop.Store(true)
		deadTime := time.Now()

		if owned {
			for conn.Context().Err() == nil {
				pingTime := WakeupTime(lastWriteTime.Add(settings.PingTimeout), settings.PingTimeout)
				select {
				case <-conn.Context().Done():
				case <-time.After(time.Until(pingTime)):
					if _, err := stream.Write(make([]byte, 8)); err == nil {
						lastWriteTime = time.Now()
					}
				}
			}
		} else {
			<-conn.Context().Done()
		}
		noticedAfter := time.Since(deadTime)
		t.Logf("the dead extender was noticed %s after its last packet", noticedAfter)

		var idleTimeoutErr *quic.IdleTimeoutError
		if !errors.As(context.Cause(conn.Context()), &idleTimeoutErr) {
			t.Errorf("the carrier closed with %v, expected an idle timeout", context.Cause(conn.Context()))
		}
		if bound := settings.PingTimeout + quicConfig.MaxIdleTimeout; bound < noticedAfter {
			t.Errorf("the dead extender was noticed %s after its last packet, expected within %s", noticedAfter, bound)
		}
	})
}

// A platform transport's carrier relied on the transport's ping alone before:
// due up to two ping timeouts after the transport's last write, it noticed the
// dead extender about 30 s after its last packet.
func TestExtenderQuicCarrierOfAPlatformTransportNoticesADeadExtenderInTime(t *testing.T) {
	expectExtenderQuicCarrierNoticesADeadExtenderInTime(t, true)
}

// A standalone carrier has no ping of its own, and took the platform config's
// keepalive of two ping timeouts before: it noticed the dead extender about
// 30 s after its last packet.
func TestStandaloneExtenderQuicCarrierNoticesADeadExtenderInTime(t *testing.T) {
	expectExtenderQuicCarrierNoticesADeadExtenderInTime(t, false)
}
