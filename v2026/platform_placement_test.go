package connect

import (
	"context"
	"net"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"time"
)

// A placement that offers fixed addresses and records the hosts it was asked
// for and the failures it was told of.
type testPlatformPlacement struct {
	stateLock       sync.Mutex
	targetIps       []netip.Addr
	askedHosts      []string
	failedTargetIps []netip.Addr
}

func newTestPlatformPlacement(targetIps ...netip.Addr) *testPlatformPlacement {
	return &testPlatformPlacement{
		targetIps: targetIps,
	}
}

func (self *testPlatformPlacement) PlatformPlacementTargets(host string) []netip.Addr {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.askedHosts = append(self.askedHosts, host)
	return slices.Clone(self.targetIps)
}

func (self *testPlatformPlacement) PlatformPlacementFailed(host string, targetIps []netip.Addr) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.failedTargetIps = append(self.failedTargetIps, targetIps...)
}

func (self *testPlatformPlacement) asked() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.askedHosts)
}

func (self *testPlatformPlacement) failed() []netip.Addr {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.failedTargetIps)
}

// An H1 transport with a placement dials the placed edge, with the platform
// hostname kept as the TLS server name and the HTTP host. The client keeps the
// placement: a reconnect returns to the same edge without asking again.
func TestPlatformTransportPlacementDialsThePlacedEdge(t *testing.T) {
	server := newTestMoveH1Server(t, testMoveHost)
	targetAddress := testMoveTargetAddress(testMoveTargetIpv4)
	seam := newTestMoveDialSeam(server.address, targetAddress, testMoveHostAddress())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	strategy := newTestMoveStrategy(t, ctx, server.roots, seam.dial)

	placement := newTestPlatformPlacement(testMoveTargetIpv4)
	tracker := NewResidentMoveTracker()
	settings := testingPlatformTransportSettings()
	settings.Placement = placement
	settings.ResidentMoveTracker = tracker
	transport := NewPlatformTransportWithTargetMode(
		ctx,
		strategy,
		NewRouteManager(ctx, "placement-h1"),
		"wss://"+testMoveHost,
		testingFamilyAuth(),
		TransportModeH1,
		settings,
	)
	defer transport.Close()

	first := receiveTestMoveValue(t, server.requests, "the placed edge was not reached")
	if first.host != testMoveHost {
		t.Fatalf("HTTP host = %q, want %q", first.host, testMoveHost)
	}
	if serverName := receiveTestMoveValue(t, server.serverNames, "no client hello"); serverName != testMoveHost {
		t.Fatalf("TLS server name = %q, want %q", serverName, testMoveHost)
	}
	if got := seam.dialedAddresses(); !slices.Equal(got, []string{targetAddress}) {
		t.Fatalf("first dial = %v, want the placed edge", got)
	}
	if got := placement.asked(); !slices.Equal(got, []string{testMoveHost}) {
		t.Fatalf("placement asked for %v, want the platform host once", got)
	}
	if got := tracker.placedTargetIps(testMoveHost, 0, time.Now()); !slices.Equal(got, []netip.Addr{testMoveTargetIpv4}) {
		t.Fatalf("kept placement = %v", got)
	}

	seam.resetDialedAddresses()
	first.conn.Close()
	receiveTestMoveValue(t, server.requests, "the reconnect did not reach the placed edge")
	if got := seam.dialedAddresses(); 0 == len(got) || slices.ContainsFunc(got, func(address string) bool { return address != targetAddress }) {
		t.Fatalf("reconnect dials = %v, want the placed edge only", got)
	}
	if got := placement.asked(); len(got) != 1 {
		t.Fatalf("placement asked %d times, want once: the reconnect keeps the placement", len(got))
	}
}

// A transport without moves (a probe, MOVE.md D15) is still placed, and still
// declares no move version.
func TestPlatformTransportPlacementWithoutMoves(t *testing.T) {
	server := newTestMoveH1Server(t, testMoveHost)
	targetAddress := testMoveTargetAddress(testMoveTargetIpv4)
	seam := newTestMoveDialSeam(server.address, targetAddress, testMoveHostAddress())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	strategy := newTestMoveStrategy(t, ctx, server.roots, seam.dial)

	settings := testingPlatformTransportSettings()
	settings.EnableMove = false
	settings.Placement = newTestPlatformPlacement(testMoveTargetIpv4)
	transport := NewPlatformTransportWithTargetMode(
		ctx,
		strategy,
		NewRouteManager(ctx, "placement-no-moves"),
		"wss://"+testMoveHost,
		testingFamilyAuth(),
		TransportModeH1,
		settings,
	)
	defer transport.Close()

	request := receiveTestMoveValue(t, server.requests, "the placed edge was not reached")
	if request.moveVersion != "" {
		t.Fatalf("move version header = %q without moves", request.moveVersion)
	}
	if got := seam.dialedAddresses(); !slices.Equal(got, []string{targetAddress}) {
		t.Fatalf("dials = %v, want the placed edge", got)
	}
}

// A placed edge that fails is reported to the placement, forgotten and held:
// the placement offers it again, but the next dial resolves the name.
func TestPlatformTransportPlacementFailureResolvesTheName(t *testing.T) {
	server := newTestMoveH1Server(t, testMoveHost)
	targetAddress := testMoveTargetAddress(testMoveTargetIpv4)
	// only the name is routable
	seam := newTestMoveDialSeam(server.address, testMoveHostAddress())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	strategy := newTestMoveStrategy(t, ctx, server.roots, seam.dial)

	placement := newTestPlatformPlacement(testMoveTargetIpv4)
	tracker := NewResidentMoveTracker()
	settings := testingPlatformTransportSettings()
	settings.Placement = placement
	settings.ResidentMoveTracker = tracker
	transport := NewPlatformTransportWithTargetMode(
		ctx,
		strategy,
		NewRouteManager(ctx, "placement-failure"),
		"wss://"+testMoveHost,
		testingFamilyAuth(),
		TransportModeH1,
		settings,
	)
	defer transport.Close()

	receiveTestMoveValue(t, server.requests, "the transport did not fall back to the name")
	dialed := seam.dialedAddresses()
	if len(dialed) < 2 || dialed[0] != targetAddress || dialed[len(dialed)-1] != testMoveHostAddress() {
		t.Fatalf("dials = %v, want the placed edge and then the name", dialed)
	}
	if got := placement.failed(); !slices.Equal(got, []netip.Addr{testMoveTargetIpv4}) {
		t.Fatalf("failures reported = %v, want the placed edge", got)
	}
	if got := tracker.placedTargetIps(testMoveHost, 0, time.Now()); 0 < len(got) {
		t.Fatalf("a failed placement was kept: %v", got)
	}
	if !tracker.TargetFailedWithin(testMoveTargetIpv4, settings.PlacementFailHold, time.Now()) {
		t.Fatal("the failed edge is not held")
	}
}

// A move's preference outranks the placement: the client dials the edge a
// move sent it to and does not ask the placement.
func TestPlatformTransportMovePreferenceOutranksPlacement(t *testing.T) {
	server := newTestMoveH1Server(t, testMoveHost)
	preferredAddress := testMoveTargetAddress(testMoveTargetIpv4Other)
	placedAddress := testMoveTargetAddress(testMoveTargetIpv4)
	seam := newTestMoveDialSeam(server.address, preferredAddress, placedAddress, testMoveHostAddress())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	strategy := newTestMoveStrategy(t, ctx, server.roots, seam.dial)

	placement := newTestPlatformPlacement(testMoveTargetIpv4)
	tracker := NewResidentMoveTracker()
	tracker.preferTargets(testMoveHost, []netip.Addr{testMoveTargetIpv4Other}, time.Now().Add(time.Hour))
	settings := testingPlatformTransportSettings()
	settings.Placement = placement
	settings.ResidentMoveTracker = tracker
	transport := NewPlatformTransportWithTargetMode(
		ctx,
		strategy,
		NewRouteManager(ctx, "placement-preference"),
		"wss://"+testMoveHost,
		testingFamilyAuth(),
		TransportModeH1,
		settings,
	)
	defer transport.Close()

	receiveTestMoveValue(t, server.requests, "the preferred edge was not reached")
	if got := seam.dialedAddresses(); !slices.Equal(got, []string{preferredAddress}) {
		t.Fatalf("dials = %v, want the move's preferred edge", got)
	}
	if got := placement.asked(); 0 < len(got) {
		t.Fatalf("placement asked for %v under a move preference", got)
	}
}

// A pinned transport dials the placed address of its family only, and the
// client keeps every family the placement offered for its other transports.
func TestPlatformTransportPinnedPlacementUsesItsFamily(t *testing.T) {
	restoreProbe := swapControlFamilyProbe(func(int) bool { return true })
	defer restoreProbe()
	server := newTestMoveH1Server(t, testMoveHost)
	ipv4Address := testMoveTargetAddress(testMoveTargetIpv4)
	ipv6Address := testMoveTargetAddress(testMoveTargetIpv6)
	seam := newTestMoveDialSeam(server.address, ipv4Address, ipv6Address)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	strategy := NewDirectClientStrategy(ctx, testMoveStrategySettings(server.roots, seam.dial), 6)
	defer strategy.Close()

	tracker := NewResidentMoveTracker()
	settings := testingFamilyTransportSettings()
	settings.IpFamily = 6
	settings.Placement = newTestPlatformPlacement(testMoveTargetIpv4, testMoveTargetIpv6)
	settings.ResidentMoveTracker = tracker
	transport := NewPlatformTransportWithTargetMode(
		ctx,
		strategy,
		NewRouteManager(ctx, "placement-pinned"),
		"wss://"+testMoveHost,
		testingFamilyAuth(),
		TransportModeH1,
		settings,
	)
	defer transport.Close()

	receiveTestMoveValue(t, server.requests, "the pinned transport did not reach its placed edge")
	if got := seam.dialedAddresses(); !slices.Equal(got, []string{ipv6Address}) {
		t.Fatalf("pinned ipv6 dials = %v, want the placed ipv6 edge only", got)
	}
	if got, want := tracker.placedTargetIps(testMoveHost, 0, time.Now()), orderDialAddrs([]netip.Addr{testMoveTargetIpv4, testMoveTargetIpv6}); !slices.Equal(got, want) {
		t.Fatalf("kept placement = %v, want both families %v", got, want)
	}
}

// Placement redirects only the platform host's edges, like a move: an alt url
// keeps plain H3 on alt, and without one plain H3 dials the placed edge on the
// platform port.
func TestPlatformTransportPlacementNeverRedirectsAnAltCarrier(t *testing.T) {
	settings := DefaultPlatformTransportSettings()
	settings.AltUrl = "https://" + testAltIp
	transport := newTestAltTransport(t, settings)
	placed := func() *platformMoveDialAttempt {
		return &platformMoveDialAttempt{
			override:  &moveDialOverride{host: testMoveHost, targetIps: []netip.Addr{testMoveTargetIpv4}},
			placement: true,
		}
	}
	attempt := placed()
	udpAddrs, _, err := transport.h3DialCandidates(attempt.withContext(context.Background()), TransportModeH3, testMoveHost)
	if err != nil {
		t.Fatal(err)
	}
	if got := testAltCandidateHostPorts(udpAddrs); !slices.Equal(got, []string{net.JoinHostPort(testAltIp, "443")}) {
		t.Fatalf("placement with an alt url: candidates = %v, want the alt address", got)
	}
	if attempt.dialedTargets() {
		t.Fatal("placement with an alt url: the attempt counts as a dial of its targets")
	}

	settings.AltUrl = ""
	attempt = placed()
	udpAddrs, _, err = transport.h3DialCandidates(attempt.withContext(context.Background()), TransportModeH3, testMoveHost)
	if err != nil {
		t.Fatal(err)
	}
	if got := testAltCandidateHostPorts(udpAddrs); !slices.Equal(got, []string{testMoveTargetAddress(testMoveTargetIpv4)}) {
		t.Fatalf("placement without an alt url: candidates = %v, want the placed edge on the platform port", got)
	}
	if !attempt.dialedTargets() {
		t.Fatal("placement without an alt url: the placed dial was not counted")
	}
}

// The tracker keeps placements apart from move preferences, per family, until
// they end, and a drop clears only the placement that names the failed address.
func TestResidentMoveTrackerKeepsPlacementsApartFromPreferences(t *testing.T) {
	now := time.Now()
	tracker := NewResidentMoveTracker()
	tracker.placeTargets(testMoveHost, []netip.Addr{testMoveTargetIpv4, testMoveTargetIpv6}, now.Add(time.Hour))
	if got := tracker.preferredTargetIps(testMoveHost, 0, now); 0 < len(got) {
		t.Fatalf("a placement reads as a move preference: %v", got)
	}
	if got, want := tracker.placedTargetIps(testMoveHost, 0, now), orderDialAddrs([]netip.Addr{testMoveTargetIpv4, testMoveTargetIpv6}); !slices.Equal(got, want) {
		t.Fatalf("placements = %v, want %v in dial order", got, want)
	}
	if got := tracker.placedTargetIps(testMoveHost, 4, now); !slices.Equal(got, []netip.Addr{testMoveTargetIpv4}) {
		t.Fatalf("ipv4 placements = %v", got)
	}
	if got := tracker.placedTargetIps("other.space.example", 0, now); 0 < len(got) {
		t.Fatalf("placements of another host = %v", got)
	}

	// a drop of another address keeps the placement of its family
	tracker.dropPlacedTargets(testMoveHost, []netip.Addr{testMoveTargetIpv4Other})
	if got := tracker.placedTargetIps(testMoveHost, 4, now); !slices.Equal(got, []netip.Addr{testMoveTargetIpv4}) {
		t.Fatalf("ipv4 placements after another address's drop = %v", got)
	}
	tracker.dropPlacedTargets(testMoveHost, []netip.Addr{testMoveTargetIpv4})
	if got := tracker.placedTargetIps(testMoveHost, 0, now); !slices.Equal(got, []netip.Addr{testMoveTargetIpv6}) {
		t.Fatalf("placements after the ipv4 drop = %v", got)
	}

	// past its end a placement is gone
	if got := tracker.placedTargetIps(testMoveHost, 0, now.Add(2*time.Hour)); 0 < len(got) {
		t.Fatalf("placements past their end = %v", got)
	}

	// a tracker built without its maps keeps a placement
	empty := &ResidentMoveTracker{}
	empty.placeTargets(testMoveHost, []netip.Addr{testMoveTargetIpv4}, now.Add(time.Hour))
	if got := empty.placedTargetIps(testMoveHost, 0, now); !slices.Equal(got, []netip.Addr{testMoveTargetIpv4}) {
		t.Fatalf("placements of a zero tracker = %v", got)
	}
}
