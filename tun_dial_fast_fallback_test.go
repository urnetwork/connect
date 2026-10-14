// IPV6.md C7: an IPv6-first tun dial through the single-destination client
// to a provider that cannot dial IPv6. The provider is played in-process: it
// answers an IPv6 SYN with its own dial-failure signal (ipOosUnreachable) and
// carries IPv4 to a second tun. The DoH server is host-dialed loopback.
package connect

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

type tunFastFallbackFixture struct {
	ctx       context.Context
	left      *Tun
	host      string
	port      string
	stateLock sync.Mutex
	firstSyn  map[int]time.Time
}

// signalFrom names the provider the IPv6 dial-failure signal claims to come
// from; the client's own provider is the honest case, and the zero id sends
// no signal at all, as a provider from before the return-path fix.
func newTunFastFallbackFixture(t *testing.T, signalFrom func(provider Id) Id) *tunFastFallbackFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	t.Cleanup(cancel)
	self := &tunFastFallbackFixture{ctx: ctx, host: "fast-fallback.tun.test", firstSyn: map[int]time.Time{}}
	right, err := CreateTun(ctx, tunTestSettings(6))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = right.Close() })
	right4, right6 := tunTestLocalAddress(t, right, 4), tunTestLocalAddress(t, right, 6)
	listener, err := right.ListenTCP(&net.TCPAddr{IP: net.IP(right4.AsSlice())})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	self.port = itoa(listener.Addr().(*net.TCPAddr).Port)
	dohServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeDohWire(w, r, []netip.Addr{right4, right6}, 60, false)
	}))
	t.Cleanup(dohServer.Close)
	leftSettings := tunFirstFamilySettings(6)
	leftSettings.Log = NewNoopLogger()
	self.left, err = CreateTunWithResolver(ctx, leftSettings, &DnsResolverSettings{
		EnableLocalDoh:   true,
		LocalDohUrlsIpv4: []string{dohServer.URL},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = self.left.Close() })

	provider := NewId()
	remote := newDialFailureRemoteClient(t, provider, func(_ TransferPath, _ protocol.ProvideMode, _ *IpPath, packet []byte) {
		_, _ = self.left.Write(packet)
	})
	peer := Peer{ProvideMode: protocol.ProvideMode_Public}
	deliver := func(source Id, packet []byte) {
		frame, err := ipPacketFromProviderFrame(packet, DefaultProtocolVersion)
		if err == nil {
			remote.ClientReceive(SourceId(source), []*protocol.Frame{frame}, peer)
		}
	}
	source := SourceId(NewId())
	go func() {
		for {
			packet, err := self.left.Read()
			if err != nil {
				return
			}
			ipPath, err := ParseIpPath(packet)
			if err != nil || ipPath.Protocol != IpProtocolTcp {
				MessagePoolReturn(packet)
				continue
			}
			if ipPath.Syn && !ipPath.Ack {
				self.stateLock.Lock()
				if _, seen := self.firstSyn[ipPath.Version]; !seen {
					self.firstSyn[ipPath.Version] = time.Now()
				}
				self.stateLock.Unlock()
			}
			var egress []byte
			if ipPath.Version == 4 {
				egress = MessagePoolCopy(packet)
			}
			// the client records the SYN on its way to the provider
			if !remote.SendPacket(source, protocol.ProvideMode_Network, packet, 0) {
				MessagePoolReturn(packet)
			}
			switch {
			case egress != nil:
				// the provider's IPv4 egress works
				_, _ = right.Write(egress)
				MessagePoolReturn(egress)
			case ipPath.Syn && !ipPath.Ack:
				// the provider cannot dial IPv6 and says so, from its flow
				// path, which carries no sequence number
				flowPath := *ipPath
				flowPath.SequenceNumber = 0
				if signal, ok := ipOosUnreachable(&flowPath); ok && signalFrom(provider) != (Id{}) {
					deliver(signalFrom(provider), signal)
				}
			}
		}
	}()
	go func() {
		for {
			packet, err := right.Read()
			if err != nil {
				return
			}
			deliver(provider, packet)
			MessagePoolReturn(packet)
		}
	}()
	for _, recordType := range []string{"A", "AAAA"} {
		if _, authoritative := self.left.DohCache().QueryResult(ctx, recordType, self.host); !authoritative {
			t.Fatalf("failed to warm the %s answer", recordType)
		}
	}
	return self
}

// Dials by name and returns the landed family and the gap between the first
// IPv6 SYN and the first IPv4 SYN.
func (self *tunFastFallbackFixture) dial(t *testing.T) (int, time.Duration) {
	t.Helper()
	conn, err := self.left.DialContext(self.ctx, "tcp", net.JoinHostPort(self.host, self.port))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	remote, err := netip.ParseAddrPort(conn.RemoteAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	first6, tried6 := self.firstSyn[6]
	first4, tried4 := self.firstSyn[4]
	if !tried6 || !tried4 || !first6.Before(first4) {
		t.Fatalf("IPv6 tried=%t at %s, IPv4 tried=%t at %s: want IPv6 first", tried6, first6, tried4, first4)
	}
	return addrIpFamily(remote.Addr()), first4.Sub(first6)
}

// IPV6.md C7. The provider's signal fails the IPv6 connect at once, so the
// IPv4 SYN follows well inside the fallback delay and the dial lands on IPv4.
func TestTunDialFastFallbackOnProviderDialFailure(t *testing.T) {
	self := newTunFastFallbackFixture(t, func(provider Id) Id { return provider })
	landed, gap := self.dial(t)
	if landed != 4 || DefaultDialFallbackDelay/2 <= gap {
		t.Fatalf("landed v%d with IPv4 %s after IPv6, want IPv4 within %s", landed, gap, DefaultDialFallbackDelay/2)
	}
}

// IPV6.md C7. The same signal claiming another provider is ignored: the
// IPv6 connect waits out the fallback delay before IPv4 starts.
func TestTunDialFastFallbackIgnoresSpoofedSignal(t *testing.T) {
	self := newTunFastFallbackFixture(t, func(Id) Id { return NewId() })
	landed, gap := self.dial(t)
	if landed != 4 || gap < DefaultDialFallbackDelay*4/5 {
		t.Fatalf("landed v%d with IPv4 %s after IPv6, want the %s fallback delay", landed, gap, DefaultDialFallbackDelay)
	}
}

// IPV6.md C7. A provider from before the return-path fix drops its signal, so
// the IPv6 connect gets no answer and IPv4 starts after the fallback delay.
func TestTunDialFastFallbackWithoutSignalWaitsFallbackDelay(t *testing.T) {
	self := newTunFastFallbackFixture(t, func(Id) Id { return Id{} })
	landed, gap := self.dial(t)
	if landed != 4 || gap < DefaultDialFallbackDelay*4/5 {
		t.Fatalf("landed v%d with IPv4 %s after IPv6, want the %s fallback delay", landed, gap, DefaultDialFallbackDelay)
	}
}
