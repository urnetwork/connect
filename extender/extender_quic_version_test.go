package extender

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"net"
	"slices"
	"sync"
	"testing"
	"time"

	quic "github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"

	"github.com/urnetwork/connect"
)

// The extender's side of the QUIC version offer (EXTENDER.md A13): the udp
// carrier's listener accepts the versions its policy lists and answers a
// Version Negotiation packet for one it excludes, the full client carrier
// handshakes on version 2 against it without a negotiation, and a hop dial
// inherits the extender's policy.

// A loopback udp endpoint that records the long-header version of every
// datagram it sends and receives, in order.
type testQuicVersionEndpoint struct {
	net.PacketConn

	stateLock       sync.Mutex
	writtenVersions []uint32
	readVersions    []uint32
}

// The version field of a datagram whose first packet has a long header,
// which every Initial and Version Negotiation packet has.
func testQuicDatagramVersion(datagram []byte) (uint32, bool) {
	if len(datagram) < 5 || datagram[0]&0x80 == 0 {
		return 0, false
	}
	return binary.BigEndian.Uint32(datagram[1:5]), true
}

func (self *testQuicVersionEndpoint) WriteTo(b []byte, addr net.Addr) (int, error) {
	if version, ok := testQuicDatagramVersion(b); ok {
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.writtenVersions = append(self.writtenVersions, version)
		}()
	}
	return self.PacketConn.WriteTo(b, addr)
}

func (self *testQuicVersionEndpoint) ReadFrom(b []byte) (int, net.Addr, error) {
	n, addr, err := self.PacketConn.ReadFrom(b)
	if err == nil {
		if version, ok := testQuicDatagramVersion(b[:n]); ok {
			func() {
				self.stateLock.Lock()
				defer self.stateLock.Unlock()
				self.readVersions = append(self.readVersions, version)
			}()
		}
	}
	return n, addr, err
}

func (self *testQuicVersionEndpoint) written() []uint32 {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.writtenVersions)
}

func (self *testQuicVersionEndpoint) read() []uint32 {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.readVersions)
}

// The listener accepts what the policy lists, version 2 and 1 both by default
// and under prefer-v2, and answers a client that offers only an excluded
// version with a Version Negotiation packet that names the accepted ones.
func TestExtenderQuicCarrierAcceptsTheVersionsOfItsPolicy(t *testing.T) {
	for _, c := range []struct {
		policy connect.QuicVersionPolicy
		client []quic.Version
		// a Version Negotiation packet is expected instead of a handshake
		negotiation bool
	}{
		{policy: "", client: []quic.Version{quic.Version2}, negotiation: false},
		{policy: "", client: []quic.Version{quic.Version1}, negotiation: false},
		{policy: connect.QuicVersionPolicyPreferV2, client: []quic.Version{quic.Version2}, negotiation: false},
		{policy: connect.QuicVersionPolicyPreferV2, client: []quic.Version{quic.Version1}, negotiation: false},
		{policy: connect.QuicVersionPolicyV1, client: []quic.Version{quic.Version1}, negotiation: false},
		{policy: connect.QuicVersionPolicyV1, client: []quic.Version{quic.Version2}, negotiation: true},
		{policy: connect.QuicVersionPolicyV2, client: []quic.Version{quic.Version2}, negotiation: false},
		{policy: connect.QuicVersionPolicyV2, client: []quic.Version{quic.Version1}, negotiation: true},
	} {
		fixture := newExtenderFixture(t, "127.0.0.1", func(settings *ExtenderSettings) {
			settings.QuicVersionPolicy = c.policy
		})
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		conn, err := quic.DialAddr(
			ctx,
			fixture.authority(fixture.quicPort),
			&tls.Config{
				InsecureSkipVerify: true, // the fixture's self-signed carrier leaf
				MinVersion:         tls.VersionTLS13,
				NextProtos:         []string{http3.NextProtoH3},
			},
			&quic.Config{Versions: c.client},
		)
		cancel()
		if c.negotiation {
			if conn != nil {
				conn.CloseWithError(0, "")
			}
			var negotiationErr *quic.VersionNegotiationError
			if !errors.As(err, &negotiationErr) {
				t.Errorf("%q against %v = %v, want a version negotiation error", c.policy, c.client, err)
				continue
			}
			// the packet names the accepted versions beside one greased entry
			accepted := c.policy.Versions()
			for _, version := range accepted {
				if !slices.Contains(negotiationErr.Theirs, version) {
					t.Errorf("%q against %v: the negotiation names %v, want %v", c.policy, c.client, negotiationErr.Theirs, accepted)
				}
			}
			if slices.Contains(negotiationErr.Theirs, c.client[0]) {
				t.Errorf("%q against %v: the negotiation names the excluded version: %v", c.policy, c.client, negotiationErr.Theirs)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q against %v: %v", c.policy, c.client, err)
			continue
		}
		if negotiated := conn.ConnectionState().Version; negotiated != c.client[0] {
			t.Errorf("%q against %v: negotiated %s", c.policy, c.client, negotiated)
		}
		conn.CloseWithError(0, "")
	}
}

// The full udp carrier against the extender, with the connect defaults: the
// first Initial is version 2, no Version Negotiation packet comes back, and
// the forward completes.
func TestExtenderQuicCarrierHandshakesOnVersion2(t *testing.T) {
	fixture := newExtenderFixture(t, "127.0.0.1", nil)
	var endpointLock sync.Mutex
	endpoints := []*testQuicVersionEndpoint{}
	connectSettings := fixture.connectSettings()
	connectSettings.DialContextSettings = &connect.DialContextSettings{
		DialContext: (&net.Dialer{}).DialContext,
		PacketConnFactory: func(context.Context) (net.PacketConn, error) {
			packetConn, err := net.ListenPacket("udp4", "127.0.0.1:0")
			if err != nil {
				return nil, err
			}
			endpoint := &testQuicVersionEndpoint{PacketConn: packetConn}
			endpointLock.Lock()
			defer endpointLock.Unlock()
			endpoints = append(endpoints, endpoint)
			return endpoint, nil
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	conn, _, err := connect.DialExtender(
		ctx,
		connectSettings,
		fixture.extenderConfig(connect.ExtenderCarrierQuic),
		&connect.ExtenderDial{DestinationHost: "dest.example", DestinationPort: 443},
	)
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	if _, err := fixture.nextForwardNetwork(); err != nil {
		t.Fatal(err)
	}
	endpointLock.Lock()
	defer endpointLock.Unlock()
	if len(endpoints) != 1 {
		t.Fatalf("endpoints = %d, want one for the udp carrier", len(endpoints))
	}
	if written := endpoints[0].written(); len(written) == 0 || written[0] != uint32(quic.Version2) {
		t.Fatalf("first datagram versions = %#08x, want version 2 first", written)
	}
	if read := endpoints[0].read(); slices.Contains(read, 0) {
		t.Fatalf("the extender answered a Version Negotiation packet: %#08x", read)
	}
}

// The defaults accept version 2 first, and a hop dial offers what its
// extender's own carriers accept, whatever the policy.
func TestExtenderNLayerHopDialOffersTheExtenderQuicVersionPolicy(t *testing.T) {
	if policy := DefaultExtenderSettings().QuicVersionPolicy; policy != connect.QuicVersionPolicyPreferV2 {
		t.Fatalf("DefaultExtenderSettings policy = %q", policy)
	}
	for _, policy := range []connect.QuicVersionPolicy{
		"",
		connect.QuicVersionPolicyPreferV2,
		connect.QuicVersionPolicyV1,
		connect.QuicVersionPolicyV2,
	} {
		settings := DefaultExtenderSettings()
		settings.QuicVersionPolicy = policy
		server := &ExtenderServer{settings: settings}
		if got := server.newNLayerConnectSettings().QuicVersionPolicy; got != policy {
			t.Errorf("hop dial policy under %q = %q", policy, got)
		}
	}
}
