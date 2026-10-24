package connect

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"slices"
	"syscall"
	"testing"
	"time"

	quic "github.com/quic-go/quic-go"
	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect/protocol"
)

// Documentation addresses (RFC 5737, RFC 3849) for every move target.
var (
	testMoveTargetIpv4      = netip.MustParseAddr("192.0.2.10")
	testMoveTargetIpv4Other = netip.MustParseAddr("198.51.100.20")
	testMoveTargetIpv6      = netip.MustParseAddr("2001:db8::10")
)

const testMoveHost = "connect.space.example"

func testResidentMigrate(
	kind protocol.ResidentMoveKind,
	moveId Id,
	targetIps ...netip.Addr,
) *protocol.ResidentMigrate {
	targets := []*protocol.ResidentMoveTarget{}
	for _, targetIp := range targetIps {
		targets = append(targets, &protocol.ResidentMoveTarget{Ip: targetIp.AsSlice()})
	}
	return &protocol.ResidentMigrate{
		MigrateTime:    uint64(time.Now().UnixMilli()),
		MoveId:         moveId.Bytes(),
		Kind:           kind,
		Targets:        targets,
		PreferDuration: uint64((6 * time.Hour).Milliseconds()),
	}
}

func testAdvisoryMove(targetIps ...netip.Addr) *ResidentMove {
	return NewResidentMove(testResidentMigrate(
		protocol.ResidentMoveKind_ResidentMoveKindAdvisory,
		NewId(),
		targetIps...,
	))
}

func testCompulsoryMove(targetIps ...netip.Addr) *ResidentMove {
	message := testResidentMigrate(
		protocol.ResidentMoveKind_ResidentMoveKindCompulsory,
		NewId(),
		targetIps...,
	)
	message.CloseTime = uint64(time.Now().Add(time.Minute).UnixMilli())
	return NewResidentMove(message)
}

// A move as a fixture transport sees it.
type testResidentMoveTransport struct {
	ipFamily     int
	connected    bool
	activeMode   TransportMode
	relayCarried bool
}

func (self *testResidentMoveTransport) IpFamily() int             { return self.ipFamily }
func (self *testResidentMoveTransport) IsConnected() bool         { return self.connected }
func (self *testResidentMoveTransport) ActiveMode() TransportMode { return self.activeMode }
func (self *testResidentMoveTransport) RelayCarried() bool        { return self.relayCarried }

func testDirectMoveTransport(ipFamily int) *testResidentMoveTransport {
	return &testResidentMoveTransport{
		ipFamily:   ipFamily,
		connected:  true,
		activeMode: TransportModeH1,
	}
}

// A drain migrate carries only its time. It decodes as the drain kind with no
// id and no targets, which migrates by name as before.
func TestResidentMoveDrainMigratesByName(t *testing.T) {
	migrateTime := time.Now().Add(time.Second).Truncate(time.Millisecond)
	move := NewResidentMove(&protocol.ResidentMigrate{
		MigrateTime: uint64(migrateTime.UnixMilli()),
	})
	if move.Kind != protocol.ResidentMoveKind_ResidentMoveKindDrain {
		t.Fatalf("kind = %s, want drain", move.Kind)
	}
	if !move.MigrateTime.Equal(migrateTime) {
		t.Fatalf("migrate time = %s, want %s", move.MigrateTime, migrateTime)
	}
	if move.MoveId != nil || 0 < len(move.TargetIps) || !move.CloseTime.IsZero() {
		t.Fatalf("drain decoded move fields: %+v", move)
	}
	if move.Moving() {
		t.Fatal("a drain is a move")
	}
	plan := PlanResidentMove(
		move,
		[]ResidentMoveTransport{testDirectMoveTransport(0)},
		DefaultPlatformTransportSettings(),
		NewResidentMoveTracker(),
		time.Now(),
	)
	if !plan.SameName || plan.Declined || plan.Dial != nil {
		t.Fatalf("drain plan = %+v, want a migrate by name", plan)
	}
}

// Targets keep one global unicast address per family, the first given, and an
// IPv4-mapped address counts as IPv4. Everything else is dropped.
func TestResidentMoveKeepsOneGlobalTargetPerFamily(t *testing.T) {
	message := testResidentMigrate(
		protocol.ResidentMoveKind_ResidentMoveKindAdvisory,
		NewId(),
		netip.MustParseAddr("127.0.0.1"),
		netip.MustParseAddr("fe80::1"),
		netip.MustParseAddr("::ffff:192.0.2.10"),
		testMoveTargetIpv4Other,
		testMoveTargetIpv6,
		netip.MustParseAddr("2001:db8::99"),
	)
	message.Targets = append(message.Targets, &protocol.ResidentMoveTarget{Ip: []byte{192, 0, 2}})
	move := NewResidentMove(message)
	want := []netip.Addr{testMoveTargetIpv4, testMoveTargetIpv6}
	if !slices.Equal(move.TargetIps, want) {
		t.Fatalf("targets = %v, want %v", move.TargetIps, want)
	}
	if !move.Moving() || !move.Advisory() {
		t.Fatal("an advisory move with an id is not a move")
	}
	if move.PreferDuration != 6*time.Hour {
		t.Fatalf("prefer duration = %s, want 6h", move.PreferDuration)
	}
	if !move.CloseTime.IsZero() {
		t.Fatal("an advisory move carries a close time")
	}
}

// A move whose id is not a ulid cannot be deduplicated or reported, so it
// migrates by name. A kind this build does not know does too.
func TestResidentMoveWithoutAValidIdMigratesByName(t *testing.T) {
	message := testResidentMigrate(
		protocol.ResidentMoveKind_ResidentMoveKindAdvisory,
		NewId(),
		testMoveTargetIpv4,
	)
	message.MoveId = []byte{1, 2, 3}
	if move := NewResidentMove(message); move.MoveId != nil || move.Moving() {
		t.Fatalf("malformed id decoded as a move: %+v", move)
	}
	message = testResidentMigrate(protocol.ResidentMoveKind(9), NewId(), testMoveTargetIpv4)
	if move := NewResidentMove(message); move.Moving() {
		t.Fatal("an unknown kind is a move")
	}
}

// Only a compulsory move carries its close time.
func TestResidentMoveCloseTimeIsCompulsoryOnly(t *testing.T) {
	closeTime := time.Now().Add(time.Minute).Truncate(time.Millisecond)
	for _, kind := range []protocol.ResidentMoveKind{
		protocol.ResidentMoveKind_ResidentMoveKindAdvisory,
		protocol.ResidentMoveKind_ResidentMoveKindCompulsory,
	} {
		message := testResidentMigrate(kind, NewId(), testMoveTargetIpv4)
		message.CloseTime = uint64(closeTime.UnixMilli())
		move := NewResidentMove(message)
		compulsory := kind == protocol.ResidentMoveKind_ResidentMoveKindCompulsory
		if compulsory != move.CloseTime.Equal(closeTime) {
			t.Errorf("%s close time = %s", kind, move.CloseTime)
		}
	}
}

// The tracker drops a repeated id among its most recent ones and forgets the
// oldest past its bound.
func TestResidentMoveTrackerDedupesRecentMoveIds(t *testing.T) {
	tracker := NewResidentMoveTracker()
	a, b, c := NewId(), NewId(), NewId()
	if !tracker.AdmitMoveId(a, 2) || tracker.AdmitMoveId(a, 2) {
		t.Fatal("the first id was not admitted once")
	}
	if !tracker.AdmitMoveId(b, 2) || !tracker.AdmitMoveId(c, 2) {
		t.Fatal("new ids were not admitted")
	}
	if tracker.AdmitMoveId(c, 2) || tracker.AdmitMoveId(b, 2) {
		t.Fatal("a recent id was admitted again")
	}
	if !tracker.AdmitMoveId(a, 2) {
		t.Fatal("the id past the bound was still remembered")
	}
	if !tracker.AdmitMoveId(a, 0) || !tracker.AdmitMoveId(a, 0) {
		t.Fatal("a non-positive bound dropped an id")
	}
}

// A preference serves its hostname and family until its end time; a pinned
// family sees only its own; dropping removes only the failed address; a newer
// move replaces an older preference of the family.
func TestResidentMoveTrackerPreferencesExpireAndDrop(t *testing.T) {
	tracker := NewResidentMoveTracker()
	now := time.Now()
	tracker.preferTargets(testMoveHost, []netip.Addr{testMoveTargetIpv4, testMoveTargetIpv6}, now.Add(time.Hour))

	if got := tracker.preferredTargetIps(testMoveHost, 0, now); !slices.Equal(got, []netip.Addr{testMoveTargetIpv6, testMoveTargetIpv4}) {
		t.Fatalf("family-agnostic preferences = %v", got)
	}
	if got := tracker.preferredTargetIps(testMoveHost, 4, now); !slices.Equal(got, []netip.Addr{testMoveTargetIpv4}) {
		t.Fatalf("ipv4 preferences = %v", got)
	}
	if got := tracker.preferredTargetIps("other.space.example", 0, now); 0 < len(got) {
		t.Fatalf("another hostname has preferences %v", got)
	}

	// a newer move replaces the family's preference, and the failure of the
	// replaced address no longer drops it
	tracker.preferTargets(testMoveHost, []netip.Addr{testMoveTargetIpv4Other}, now.Add(time.Hour))
	tracker.dropPreferredTargets(testMoveHost, []netip.Addr{testMoveTargetIpv4})
	if got := tracker.preferredTargetIps(testMoveHost, 4, now); !slices.Equal(got, []netip.Addr{testMoveTargetIpv4Other}) {
		t.Fatalf("replaced ipv4 preference = %v", got)
	}
	tracker.dropPreferredTargets(testMoveHost, []netip.Addr{testMoveTargetIpv4Other})
	if got := tracker.preferredTargetIps(testMoveHost, 4, now); 0 < len(got) {
		t.Fatalf("dropped ipv4 preference = %v", got)
	}
	if got := tracker.preferredTargetIps(testMoveHost, 6, now.Add(time.Hour-time.Nanosecond)); !slices.Equal(got, []netip.Addr{testMoveTargetIpv6}) {
		t.Fatalf("ipv6 preference before its end = %v", got)
	}
	if got := tracker.preferredTargetIps(testMoveHost, 6, now.Add(time.Hour)); 0 < len(got) {
		t.Fatalf("ipv6 preference at its end = %v", got)
	}
}

// The fail hold and the client's own interval run from the recorded times; a
// non-positive setting holds nothing.
func TestResidentMoveTrackerHoldsAndIntervals(t *testing.T) {
	tracker := NewResidentMoveTracker()
	now := time.Now()
	tracker.NoteTargetFailed(testMoveTargetIpv4, now)
	if !tracker.TargetFailedWithin(testMoveTargetIpv4, time.Hour, now.Add(time.Hour-time.Nanosecond)) {
		t.Fatal("a target failed within its hold was not held")
	}
	if tracker.TargetFailedWithin(testMoveTargetIpv4, time.Hour, now.Add(time.Hour)) {
		t.Fatal("a target was held past its hold")
	}
	if tracker.TargetFailedWithin(testMoveTargetIpv4, 0, now) {
		t.Fatal("a non-positive hold held a target")
	}
	if tracker.TargetFailedWithin(testMoveTargetIpv6, time.Hour, now) {
		t.Fatal("a target that never failed was held")
	}
	// a failure past the longest hold asked about is pruned at the next note
	tracker.NoteTargetFailed(testMoveTargetIpv6, now.Add(2*time.Hour))
	func() {
		tracker.stateLock.Lock()
		defer tracker.stateLock.Unlock()
		if _, ok := tracker.targetFailTimes[testMoveTargetIpv4]; ok {
			t.Fatal("an expired failure was kept")
		}
	}()

	if tracker.MovedWithin(30*time.Minute, now) {
		t.Fatal("a client that never moved moved recently")
	}
	tracker.NoteMoved(now)
	if !tracker.MovedWithin(30*time.Minute, now.Add(29*time.Minute)) || tracker.MovedWithin(30*time.Minute, now.Add(30*time.Minute)) {
		t.Fatal("the client interval does not run from the move")
	}
	if tracker.MovedWithin(0, now) {
		t.Fatal("a non-positive interval declined a move")
	}
}

// An accepted advisory move dials each connected direct transport's family
// target, strict, decided by the carrier the old transport used.
func TestPlanResidentMoveAcceptsAnAdvisoryMove(t *testing.T) {
	move := testAdvisoryMove(testMoveTargetIpv4, testMoveTargetIpv6)
	ipv4 := testDirectMoveTransport(4)
	ipv6 := testDirectMoveTransport(6)
	ipv6.activeMode = TransportModeH3
	standby := &testResidentMoveTransport{}
	plan := PlanResidentMove(
		move,
		[]ResidentMoveTransport{ipv4, ipv6, standby},
		DefaultPlatformTransportSettings(),
		NewResidentMoveTracker(),
		time.Now(),
	)
	if plan.Declined || plan.SameName || plan.Dial == nil {
		t.Fatalf("plan = %+v, want a dial", plan)
	}
	if !plan.Dial.Strict() || plan.Dial.MoveId() != *move.MoveId {
		t.Fatal("an advisory dial is not strict, or lost its id")
	}
	for _, c := range []struct {
		ipFamily     int
		targetIps    []netip.Addr
		decisiveMode TransportMode
		targeted     bool
	}{
		{ipFamily: 4, targetIps: []netip.Addr{testMoveTargetIpv4}, decisiveMode: TransportModeH1, targeted: true},
		{ipFamily: 6, targetIps: []netip.Addr{testMoveTargetIpv6}, decisiveMode: TransportModeH3, targeted: true},
		// the standby was not connected: it dials every target, decides nothing
		{ipFamily: 0, targetIps: []netip.Addr{testMoveTargetIpv6, testMoveTargetIpv4}, decisiveMode: TransportModeNone},
	} {
		family := plan.Dial.families[c.ipFamily]
		if family == nil {
			t.Fatalf("no dial for family %d", c.ipFamily)
		}
		if !slices.Equal(family.targetIps, c.targetIps) || family.decisiveMode != c.decisiveMode || family.targeted != c.targeted {
			t.Errorf("family %d = %+v, want %+v", c.ipFamily, family, c)
		}
		if targetIps, strict, ok := plan.Dial.targetIpsFor(c.ipFamily); !ok || !strict || !slices.Equal(targetIps, c.targetIps) {
			t.Errorf("family %d dials %v strict=%t ok=%t", c.ipFamily, targetIps, strict, ok)
		}
	}
	plan.Dial.Conclude()
	if _, _, ok := plan.Dial.targetIpsFor(4); ok {
		t.Fatal("a concluded move still dials its target")
	}
}

// Each advisory decline rule, with the reason it reports.
func TestPlanResidentMoveDeclinesAdvisoryMoves(t *testing.T) {
	now := time.Now()
	for _, c := range []struct {
		name       string
		transports []ResidentMoveTransport
		targetIps  []netip.Addr
		configure  func(settings *PlatformTransportSettings, tracker *ResidentMoveTracker)
		reason     protocol.ResidentMoveReason
		ipFamily   int
	}{
		{
			name:       "extender carrier",
			transports: []ResidentMoveTransport{&testResidentMoveTransport{connected: true, activeMode: TransportModeH1, relayCarried: true}},
			targetIps:  []netip.Addr{testMoveTargetIpv4},
			reason:     protocol.ResidentMoveReason_ResidentMoveReasonExtenderCarrier,
		},
		{
			name:       "dns carrier",
			transports: []ResidentMoveTransport{&testResidentMoveTransport{connected: true, activeMode: TransportModeH3Dns}},
			targetIps:  []netip.Addr{testMoveTargetIpv4},
			reason:     protocol.ResidentMoveReason_ResidentMoveReasonDnsCarrier,
		},
		{
			name:       "dns pump carrier",
			transports: []ResidentMoveTransport{&testResidentMoveTransport{connected: true, activeMode: TransportModeH3DnsPump}},
			targetIps:  []netip.Addr{testMoveTargetIpv4},
			reason:     protocol.ResidentMoveReason_ResidentMoveReasonDnsCarrier,
		},
		{
			name:       "no family target",
			transports: []ResidentMoveTransport{testDirectMoveTransport(6)},
			targetIps:  []netip.Addr{testMoveTargetIpv4},
			reason:     protocol.ResidentMoveReason_ResidentMoveReasonNoFamilyTarget,
		},
		{
			name:       "no target at all",
			transports: []ResidentMoveTransport{testDirectMoveTransport(0)},
			reason:     protocol.ResidentMoveReason_ResidentMoveReasonNoFamilyTarget,
		},
		{
			name:       "target failed recently",
			transports: []ResidentMoveTransport{testDirectMoveTransport(4)},
			targetIps:  []netip.Addr{testMoveTargetIpv4},
			configure: func(settings *PlatformTransportSettings, tracker *ResidentMoveTracker) {
				tracker.NoteTargetFailed(testMoveTargetIpv4, now.Add(-59*time.Minute))
			},
			reason:   protocol.ResidentMoveReason_ResidentMoveReasonTargetFailedRecently,
			ipFamily: 4,
		},
		{
			name:       "recent move",
			transports: []ResidentMoveTransport{testDirectMoveTransport(0)},
			targetIps:  []netip.Addr{testMoveTargetIpv4},
			configure: func(settings *PlatformTransportSettings, tracker *ResidentMoveTracker) {
				tracker.NoteMoved(now.Add(-29 * time.Minute))
			},
			reason: protocol.ResidentMoveReason_ResidentMoveReasonRecentMove,
		},
		{
			name:       "mobile background or metered",
			transports: []ResidentMoveTransport{testDirectMoveTransport(0)},
			targetIps:  []netip.Addr{testMoveTargetIpv4},
			configure: func(settings *PlatformTransportSettings, tracker *ResidentMoveTracker) {
				settings.MoveDecline = func() bool { return true }
			},
			reason: protocol.ResidentMoveReason_ResidentMoveReasonMobileBackgroundOrMetered,
		},
	} {
		settings := DefaultPlatformTransportSettings()
		tracker := NewResidentMoveTracker()
		if c.configure != nil {
			c.configure(settings, tracker)
		}
		plan := PlanResidentMove(testAdvisoryMove(c.targetIps...), c.transports, settings, tracker, now)
		if !plan.Declined || plan.DeclineReason != c.reason || plan.DeclineIpFamily != c.ipFamily || plan.Dial != nil {
			t.Errorf("%s: plan = %+v, want declined %s family %d", c.name, plan, c.reason, c.ipFamily)
		}
	}
}

// The holds lapse: a target past its fail hold and a client past its own
// interval accept again, and a hook that declines nothing accepts.
func TestPlanResidentMoveAcceptsPastTheHolds(t *testing.T) {
	now := time.Now()
	settings := DefaultPlatformTransportSettings()
	settings.MoveDecline = func() bool { return false }
	tracker := NewResidentMoveTracker()
	tracker.NoteTargetFailed(testMoveTargetIpv4, now.Add(-time.Hour))
	tracker.NoteMoved(now.Add(-30 * time.Minute))
	plan := PlanResidentMove(
		testAdvisoryMove(testMoveTargetIpv4),
		[]ResidentMoveTransport{testDirectMoveTransport(0)},
		settings,
		tracker,
		now,
	)
	if plan.Declined || plan.Dial == nil {
		t.Fatalf("plan = %+v, want a dial", plan)
	}
}

// A compulsory move is never declined: a path that cannot use a target, and a
// client that does not honor moves, migrate by name. A held target is still
// dialed, once.
func TestPlanResidentMoveCompulsoryFallsBackToTheSameName(t *testing.T) {
	now := time.Now()
	for _, c := range []struct {
		name      string
		transport ResidentMoveTransport
		targetIps []netip.Addr
		enable    bool
	}{
		{name: "extender carrier", transport: &testResidentMoveTransport{connected: true, activeMode: TransportModeH1, relayCarried: true}, targetIps: []netip.Addr{testMoveTargetIpv4}, enable: true},
		{name: "dns carrier", transport: &testResidentMoveTransport{connected: true, activeMode: TransportModeH3Dns}, targetIps: []netip.Addr{testMoveTargetIpv4}, enable: true},
		{name: "no family target", transport: testDirectMoveTransport(6), targetIps: []netip.Addr{testMoveTargetIpv4}, enable: true},
		{name: "moves disabled", transport: testDirectMoveTransport(0), targetIps: []netip.Addr{testMoveTargetIpv4}, enable: false},
	} {
		settings := DefaultPlatformTransportSettings()
		settings.EnableMove = c.enable
		settings.MoveDecline = func() bool { return true }
		plan := PlanResidentMove(testCompulsoryMove(c.targetIps...), []ResidentMoveTransport{c.transport}, settings, NewResidentMoveTracker(), now)
		if !plan.SameName || plan.Declined || plan.Dial != nil {
			t.Errorf("%s: plan = %+v, want a migrate by name", c.name, plan)
		}
	}

	settings := DefaultPlatformTransportSettings()
	settings.MoveDecline = func() bool { return true }
	tracker := NewResidentMoveTracker()
	tracker.NoteTargetFailed(testMoveTargetIpv4, now)
	tracker.NoteMoved(now)
	plan := PlanResidentMove(testCompulsoryMove(testMoveTargetIpv4), []ResidentMoveTransport{testDirectMoveTransport(0)}, settings, tracker, now)
	if plan.Declined || plan.SameName || plan.Dial == nil || plan.Dial.Strict() {
		t.Fatalf("compulsory plan = %+v, want a dial that is not strict", plan)
	}
	if targetIps, _, ok := plan.Dial.targetIpsFor(0); !ok || !slices.Equal(targetIps, []netip.Addr{testMoveTargetIpv4}) {
		t.Fatalf("compulsory dial = %v", targetIps)
	}
}

// A failure counts only in the carrier the old transport used. It holds the
// target, the first one is what the move reports, and a strict move fails
// once every moved family failed. A transport that was not moved decides
// nothing.
func TestResidentMoveDialCountsOnlyDecisiveFailures(t *testing.T) {
	ipv4 := testDirectMoveTransport(4)
	ipv6 := testDirectMoveTransport(6)
	ipv6.activeMode = TransportModeH3
	tracker := NewResidentMoveTracker()
	plan := PlanResidentMove(
		testAdvisoryMove(testMoveTargetIpv4, testMoveTargetIpv6),
		[]ResidentMoveTransport{ipv4, ipv6},
		DefaultPlatformTransportSettings(),
		tracker,
		time.Now(),
	)
	dial := plan.Dial
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	replacement := newFakeWindowPlatformTransport(false)
	resolutions := make(chan ResidentMoveResolution, 1)
	go func() {
		resolutions <- dial.Wait(ctx, replacement, 0)
	}()

	now := time.Now()
	// the v6 transport moved off H3, so its H1 failure is no evidence
	dial.noteFailed(6, TransportModeH1, []netip.Addr{testMoveTargetIpv6}, protocol.ResidentMoveReason_ResidentMoveReasonDialError, time.Millisecond, now)
	// the standby was not moved
	dial.noteFailed(0, TransportModeH1, []netip.Addr{testMoveTargetIpv6, testMoveTargetIpv4}, protocol.ResidentMoveReason_ResidentMoveReasonDialError, time.Millisecond, now)
	if _, failed := dial.Failure(); failed {
		t.Fatal("a failure outside the decisive carrier counted")
	}
	dial.noteFailed(4, TransportModeH1, []netip.Addr{testMoveTargetIpv4}, protocol.ResidentMoveReason_ResidentMoveReasonTlsError, 120*time.Millisecond, now)
	failure, failed := dial.Failure()
	if !failed || failure.Reason != protocol.ResidentMoveReason_ResidentMoveReasonTlsError || failure.IpFamily != 4 || failure.Elapsed != 120*time.Millisecond {
		t.Fatalf("failure = %+v, want the v4 tls failure", failure)
	}
	if !tracker.TargetFailedWithin(testMoveTargetIpv4, time.Hour, now) || tracker.TargetFailedWithin(testMoveTargetIpv6, time.Hour, now) {
		t.Fatal("the hold does not follow the decisive failure")
	}
	select {
	case resolution := <-resolutions:
		t.Fatalf("one of two moved families failing resolved the move: %d", resolution)
	default:
	}
	dial.noteFailed(6, TransportModeH3, []netip.Addr{testMoveTargetIpv6}, protocol.ResidentMoveReason_ResidentMoveReasonConnectTimeout, time.Second, now)
	select {
	case resolution := <-resolutions:
		if resolution != ResidentMoveResolutionFailed {
			t.Fatalf("resolution = %d, want failed", resolution)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("every moved family failing did not resolve the move")
	}
	if failure, _ := dial.Failure(); failure.IpFamily != 4 {
		t.Fatal("a later failure replaced the first")
	}
}

// An arrival resolves a strict move and records the preference for the
// arriving hostname; once concluded, the dial ignores late outcomes.
func TestResidentMoveDialArrivalRecordsThePreference(t *testing.T) {
	tracker := NewResidentMoveTracker()
	plan := PlanResidentMove(
		testAdvisoryMove(testMoveTargetIpv4),
		[]ResidentMoveTransport{testDirectMoveTransport(0)},
		DefaultPlatformTransportSettings(),
		tracker,
		time.Now(),
	)
	dial := plan.Dial
	now := time.Now()
	dial.noteArrived(testMoveHost, []netip.Addr{testMoveTargetIpv4}, now)
	if resolution := dial.Wait(context.Background(), newFakeWindowPlatformTransport(false), time.Second); resolution != ResidentMoveResolutionConnected {
		t.Fatalf("resolution = %d, want connected", resolution)
	}
	if got := tracker.preferredTargetIps(testMoveHost, 0, now.Add(6*time.Hour-time.Second)); !slices.Equal(got, []netip.Addr{testMoveTargetIpv4}) {
		t.Fatalf("preference = %v", got)
	}
	if got := tracker.preferredTargetIps(testMoveHost, 0, now.Add(6*time.Hour)); 0 < len(got) {
		t.Fatalf("preference outlived the prefer duration: %v", got)
	}
	dial.Conclude()
	dial.noteFailed(0, TransportModeH1, []netip.Addr{testMoveTargetIpv4}, protocol.ResidentMoveReason_ResidentMoveReasonDialError, 0, now)
	if _, failed := dial.Failure(); failed {
		t.Fatal("a concluded move recorded a failure")
	}
}

// A move that is not strict resolves on any connection of the replacement;
// a strict one waits for its arrival, else its timeout.
func TestResidentMoveDialWaitFollowsStrictness(t *testing.T) {
	transports := []ResidentMoveTransport{testDirectMoveTransport(0)}
	compulsory := PlanResidentMove(testCompulsoryMove(testMoveTargetIpv4), transports, DefaultPlatformTransportSettings(), NewResidentMoveTracker(), time.Now())
	connected := newFakeWindowPlatformTransport(true)
	if resolution := compulsory.Dial.Wait(context.Background(), connected, time.Second); resolution != ResidentMoveResolutionConnected {
		t.Fatalf("compulsory resolution = %d, want connected", resolution)
	}
	advisory := PlanResidentMove(testAdvisoryMove(testMoveTargetIpv4), transports, DefaultPlatformTransportSettings(), NewResidentMoveTracker(), time.Now())
	if resolution := advisory.Dial.Wait(context.Background(), connected, 20*time.Millisecond); resolution != ResidentMoveResolutionTimedOut {
		t.Fatalf("advisory resolution = %d, want timed out without an arrival", resolution)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if resolution := advisory.Dial.Wait(ctx, connected, 0); resolution != ResidentMoveResolutionCanceled {
		t.Fatalf("canceled resolution = %d", resolution)
	}
}

// Each dial error maps to the reason the report carries.
func TestResidentMoveFailureReasonClassifiesDialErrors(t *testing.T) {
	refused := &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
	for _, c := range []struct {
		name   string
		err    error
		reason protocol.ResidentMoveReason
	}{
		{name: "connection refused", err: refused, reason: protocol.ResidentMoveReason_ResidentMoveReasonDialError},
		{name: "no family target", err: fmt.Errorf("dial: %w", errResidentMoveNoFamilyTarget), reason: protocol.ResidentMoveReason_ResidentMoveReasonDialError},
		{name: "certificate name", err: &tls.CertificateVerificationError{Err: x509.HostnameError{Host: testMoveHost}}, reason: protocol.ResidentMoveReason_ResidentMoveReasonTlsError},
		{name: "unknown authority", err: fmt.Errorf("handshake: %w", x509.UnknownAuthorityError{}), reason: protocol.ResidentMoveReason_ResidentMoveReasonTlsError},
		{name: "tls alert", err: tls.AlertError(40), reason: protocol.ResidentMoveReason_ResidentMoveReasonTlsError},
		{name: "quic crypto", err: &quic.TransportError{ErrorCode: 0x12a}, reason: protocol.ResidentMoveReason_ResidentMoveReasonTlsError},
		{name: "quic handshake timeout", err: &quic.HandshakeTimeoutError{}, reason: protocol.ResidentMoveReason_ResidentMoveReasonConnectTimeout},
		{name: "deadline", err: fmt.Errorf("dial: %w", context.DeadlineExceeded), reason: protocol.ResidentMoveReason_ResidentMoveReasonConnectTimeout},
		{name: "credential refused", err: &HTTPUpgradeError{StatusCode: http.StatusUnauthorized, Terminal: true}, reason: protocol.ResidentMoveReason_ResidentMoveReasonAuthError},
		{name: "other", err: errors.New("something else"), reason: protocol.ResidentMoveReason_ResidentMoveReasonOther},
	} {
		if reason := residentMoveFailureReason(c.err); reason != c.reason {
			t.Errorf("%s: reason = %s, want %s", c.name, reason, c.reason)
		}
	}
	// the strategy flattens its attempts; the most telling attempt error wins
	attemptErrors := &residentMoveAttemptErrors{}
	attemptErrors.observe(refused)
	attemptErrors.observe(&tls.CertificateVerificationError{Err: x509.HostnameError{Host: testMoveHost}})
	attemptErrors.observe(context.DeadlineExceeded)
	if reason := attemptErrors.failureReason(errors.New("Timeout.")); reason != protocol.ResidentMoveReason_ResidentMoveReasonTlsError {
		t.Fatalf("attempt reason = %s, want tls", reason)
	}
}

// The reports carry the move id, outcome, reason, family and elapsed time.
func TestResidentMoveResultReports(t *testing.T) {
	moveId := NewId()
	declined := NewResidentMoveDeclined(moveId, protocol.ResidentMoveReason_ResidentMoveReasonRecentMove, 0)
	if !proto.Equal(declined, &protocol.ResidentMoveResult{
		MoveId:  moveId.Bytes(),
		Outcome: protocol.ResidentMoveOutcome_ResidentMoveOutcomeDeclined,
		Reason:  protocol.ResidentMoveReason_ResidentMoveReasonRecentMove,
	}) {
		t.Fatalf("declined report = %v", declined)
	}
	failed := NewResidentMoveFailed(moveId, ResidentMoveFailure{
		Reason:   protocol.ResidentMoveReason_ResidentMoveReasonDialError,
		IpFamily: 6,
		Elapsed:  1500 * time.Millisecond,
	})
	if !proto.Equal(failed, &protocol.ResidentMoveResult{
		MoveId:   moveId.Bytes(),
		Outcome:  protocol.ResidentMoveOutcome_ResidentMoveOutcomeFailed,
		Reason:   protocol.ResidentMoveReason_ResidentMoveReasonDialError,
		IpFamily: 6,
		Elapsed:  1500,
	}) {
		t.Fatalf("failed report = %v", failed)
	}
}
