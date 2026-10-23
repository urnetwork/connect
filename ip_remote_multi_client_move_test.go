package connect

import (
	"context"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect/protocol"
)

// A window transport fixture that answers as a move sees a platform
// transport.
type fakeMoveWindowTransport struct {
	*fakeWindowPlatformTransport
	ipFamily     int
	activeMode   TransportMode
	relayCarried bool
}

func newFakeMoveWindowTransport(connected bool) *fakeMoveWindowTransport {
	return &fakeMoveWindowTransport{
		fakeWindowPlatformTransport: newFakeWindowPlatformTransport(connected),
		activeMode:                  TransportModeH1,
	}
}

func (self *fakeMoveWindowTransport) IpFamily() int             { return self.ipFamily }
func (self *fakeMoveWindowTransport) ActiveMode() TransportMode { return self.activeMode }
func (self *fakeMoveWindowTransport) RelayCarried() bool        { return self.relayCarried }

// The reports a generator sent, in order.
type testMoveReports struct {
	stateLock sync.Mutex
	results   []*protocol.ResidentMoveResult
}

func (self *testMoveReports) observe(client *Client, result *protocol.ResidentMoveResult) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.results = append(self.results, proto.Clone(result).(*protocol.ResidentMoveResult))
}

func (self *testMoveReports) snapshot() []*protocol.ResidentMoveResult {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.results)
}

// A generator holding one window client on `old`. Each replacement is built by
// `create`, which receives the replacement's own settings, and every move
// report is recorded.
type testMoveGenerator struct {
	generator *ApiMultiClientGenerator
	state     *apiWindowClientTransport
	reports   *testMoveReports
	created   chan *PlatformTransportSettings
}

func newTestMoveGenerator(
	t *testing.T,
	client *Client,
	old apiWindowPlatformTransport,
	settings *PlatformTransportSettings,
	create func(settings *PlatformTransportSettings) apiWindowPlatformTransport,
) *testMoveGenerator {
	t.Helper()
	generatorSettings := DefaultApiMultiClientGeneratorSettings()
	generatorSettings.MigrateConnectTimeout = 5 * time.Second
	generatorSettings.MigrateMaxScheduleDelay = 20 * time.Millisecond
	self := &testMoveGenerator{
		state: &apiWindowClientTransport{
			current:            old,
			settings:           settings,
			auth:               ClientAuth{InstanceId: NewId()},
			extenderIpsMonitor: NewMonitorValue[uint64](0),
		},
		reports: &testMoveReports{},
		created: make(chan *PlatformTransportSettings, 8),
	}
	self.generator = &ApiMultiClientGenerator{
		settings:                      generatorSettings,
		transports:                    map[*Client]*apiWindowClientTransport{client: self.state},
		sendResidentMoveResultForTest: self.reports.observe,
		newPlatformTransport: func(
			client *Client,
			auth *ClientAuth,
			_ TransportMode,
			settings *PlatformTransportSettings,
		) apiWindowPlatformTransport {
			self.created <- settings
			return create(settings)
		},
	}
	return self
}

// Joins every migration worker, after which every report is recorded.
func (self *testMoveGenerator) join(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := self.generator.CloseTransportCreationAndWait(ctx); err != nil {
		t.Fatalf("migration workers did not join: %v", err)
	}
}

func (self *testMoveGenerator) current() apiWindowPlatformTransport {
	self.generator.transportLock.Lock()
	defer self.generator.transportLock.Unlock()
	return self.state.current
}

func (self *testMoveGenerator) migrating() bool {
	self.generator.transportLock.Lock()
	defer self.generator.transportLock.Unlock()
	return self.state.migrating
}

func (self *testMoveGenerator) tracker() *ResidentMoveTracker {
	self.generator.transportLock.Lock()
	defer self.generator.transportLock.Unlock()
	return self.state.residentMoveTrackerWithLock()
}

func requireTestMoveReports(t *testing.T, reports *testMoveReports, want ...*protocol.ResidentMoveResult) {
	t.Helper()
	got := reports.snapshot()
	if len(got) != len(want) {
		t.Fatalf("reports = %v, want %v", got, want)
	}
	for i := range want {
		if !proto.Equal(got[i], want[i]) {
			t.Fatalf("report %d = %v, want %v", i, got[i], want[i])
		}
	}
}

// An advisory move builds its replacement with a strict dial of the target and
// keeps the old transport until the replacement arrives there; then the old
// closes, the move counts for the client interval, and nothing is reported.
func TestApiWindowAdvisoryMoveIsMakeBeforeBreakAtTheTarget(t *testing.T) {
	client, _ := newApiMigrationTestClient(t)
	old := newFakeMoveWindowTransport(true)
	next := newFakeWindowPlatformTransport(false)
	fixture := newTestMoveGenerator(t, client, old, DefaultPlatformTransportSettings(),
		func(*PlatformTransportSettings) apiWindowPlatformTransport { return next })

	move := testAdvisoryMove(testMoveTargetIpv4, testMoveTargetIpv6)
	fixture.generator.MoveClientTransport(client, nil, move)
	settings := receiveTestMoveValue(t, fixture.created, "the move did not build a replacement")
	dial := settings.ResidentMoveDial
	if dial == nil || !dial.Strict() || dial.MoveId() != *move.MoveId {
		t.Fatalf("replacement dial = %+v, want the strict move dial", dial)
	}
	if settings.ResidentMoveTracker != fixture.tracker() {
		t.Fatal("the replacement does not share the client's move memory")
	}
	if targetIps, _, ok := dial.targetIpsFor(0); !ok || !slices.Equal(targetIps, []netip.Addr{testMoveTargetIpv6, testMoveTargetIpv4}) {
		t.Fatalf("replacement targets = %v", targetIps)
	}
	if !waitForCondition(5*time.Second, func() bool {
		select {
		case <-next.waitStarted:
			return true
		default:
			return false
		}
	}) {
		t.Fatal("the move did not wait on its replacement")
	}

	// a connection that is not an arrival keeps the old transport
	next.connect()
	select {
	case <-old.closed:
		t.Fatal("the old transport closed before the replacement arrived at its target")
	case <-time.After(50 * time.Millisecond):
	}
	dial.noteArrived(testMoveHost, []netip.Addr{testMoveTargetIpv4}, time.Now())
	receiveTestMoveValue(t, old.closed, "the old transport did not close after the arrival")
	fixture.join(t)
	if fixture.current() != next {
		t.Fatal("the replacement was not installed")
	}
	if !fixture.tracker().MovedWithin(time.Minute, time.Now()) {
		t.Fatal("the completed move does not count for the client interval")
	}
	if _, _, ok := dial.targetIpsFor(0); ok {
		t.Fatal("the move was not concluded")
	}
	requireTestMoveReports(t, fixture.reports)
}

// A decisive failure at the target keeps the old transport, closes the
// replacement, holds the target and reports the failure.
func TestApiWindowAdvisoryMoveFailureKeepsTheOldTransport(t *testing.T) {
	client, _ := newApiMigrationTestClient(t)
	old := newFakeMoveWindowTransport(true)
	next := newFakeWindowPlatformTransport(false)
	fixture := newTestMoveGenerator(t, client, old, DefaultPlatformTransportSettings(),
		func(*PlatformTransportSettings) apiWindowPlatformTransport { return next })

	move := testAdvisoryMove(testMoveTargetIpv4)
	fixture.generator.MoveClientTransport(client, nil, move)
	dial := receiveTestMoveValue(t, fixture.created, "the move did not build a replacement").ResidentMoveDial
	dial.noteFailed(0, TransportModeH1, []netip.Addr{testMoveTargetIpv4}, protocol.ResidentMoveReason_ResidentMoveReasonTlsError, 250*time.Millisecond, time.Now())
	receiveTestMoveValue(t, next.closed, "the failed replacement was not closed")
	fixture.join(t)
	select {
	case <-old.closed:
		t.Fatal("a failed move closed the old transport")
	default:
	}
	if fixture.current() != old || fixture.migrating() {
		t.Fatal("a failed move displaced the old transport or stayed armed")
	}
	if !fixture.tracker().TargetFailedWithin(testMoveTargetIpv4, time.Hour, time.Now()) {
		t.Fatal("the failed target is not held")
	}
	requireTestMoveReports(t, fixture.reports, &protocol.ResidentMoveResult{
		MoveId:   move.MoveId.Bytes(),
		Outcome:  protocol.ResidentMoveOutcome_ResidentMoveOutcomeFailed,
		Reason:   protocol.ResidentMoveReason_ResidentMoveReasonTlsError,
		IpFamily: 4,
		Elapsed:  250,
	})
}

// A replacement that neither arrives nor fails within the connect timeout
// keeps the old transport and reports a connect timeout.
func TestApiWindowAdvisoryMoveTimeoutReportsConnectTimeout(t *testing.T) {
	client, _ := newApiMigrationTestClient(t)
	old := newFakeMoveWindowTransport(true)
	next := newFakeWindowPlatformTransport(false)
	fixture := newTestMoveGenerator(t, client, old, DefaultPlatformTransportSettings(),
		func(*PlatformTransportSettings) apiWindowPlatformTransport { return next })
	fixture.generator.settings.MigrateConnectTimeout = 25 * time.Millisecond

	move := testAdvisoryMove(testMoveTargetIpv4)
	fixture.generator.MoveClientTransport(client, nil, move)
	receiveTestMoveValue(t, next.closed, "the timed out replacement was not closed")
	fixture.join(t)
	if fixture.current() != old {
		t.Fatal("a timed out move displaced the old transport")
	}
	reports := fixture.reports.snapshot()
	if len(reports) != 1 ||
		reports[0].Outcome != protocol.ResidentMoveOutcome_ResidentMoveOutcomeFailed ||
		reports[0].Reason != protocol.ResidentMoveReason_ResidentMoveReasonConnectTimeout ||
		reports[0].IpFamily != 0 || reports[0].Elapsed < 25 {
		t.Fatalf("reports = %v, want one connect timeout", reports)
	}
}

// Every advisory decline builds nothing, leaves the old transport alone and
// reports its reason once.
func TestApiWindowAdvisoryMoveDeclines(t *testing.T) {
	for _, c := range []struct {
		name      string
		targetIps []netip.Addr
		configure func(fixture *testMoveGenerator, old *fakeMoveWindowTransport)
		reason    protocol.ResidentMoveReason
		ipFamily  uint32
	}{
		{
			name:      "extender carrier",
			targetIps: []netip.Addr{testMoveTargetIpv4},
			configure: func(fixture *testMoveGenerator, old *fakeMoveWindowTransport) {
				old.relayCarried = true
			},
			reason: protocol.ResidentMoveReason_ResidentMoveReasonExtenderCarrier,
		},
		{
			name:      "dns carrier",
			targetIps: []netip.Addr{testMoveTargetIpv4},
			configure: func(fixture *testMoveGenerator, old *fakeMoveWindowTransport) {
				old.activeMode = TransportModeH3Dns
			},
			reason: protocol.ResidentMoveReason_ResidentMoveReasonDnsCarrier,
		},
		{
			name:      "no family target",
			targetIps: []netip.Addr{testMoveTargetIpv4},
			configure: func(fixture *testMoveGenerator, old *fakeMoveWindowTransport) {
				old.ipFamily = 6
			},
			reason: protocol.ResidentMoveReason_ResidentMoveReasonNoFamilyTarget,
		},
		{
			name:      "target failed recently",
			targetIps: []netip.Addr{testMoveTargetIpv4},
			configure: func(fixture *testMoveGenerator, old *fakeMoveWindowTransport) {
				fixture.tracker().NoteTargetFailed(testMoveTargetIpv4, time.Now())
			},
			reason: protocol.ResidentMoveReason_ResidentMoveReasonTargetFailedRecently,
		},
		{
			name:      "recent move",
			targetIps: []netip.Addr{testMoveTargetIpv4},
			configure: func(fixture *testMoveGenerator, old *fakeMoveWindowTransport) {
				fixture.tracker().NoteMoved(time.Now())
			},
			reason: protocol.ResidentMoveReason_ResidentMoveReasonRecentMove,
		},
		{
			name:      "mobile background or metered",
			targetIps: []netip.Addr{testMoveTargetIpv4},
			configure: func(fixture *testMoveGenerator, old *fakeMoveWindowTransport) {
				fixture.state.settings.MoveDecline = func() bool { return true }
			},
			reason: protocol.ResidentMoveReason_ResidentMoveReasonMobileBackgroundOrMetered,
		},
		{
			name:      "move in progress",
			targetIps: []netip.Addr{testMoveTargetIpv4},
			configure: func(fixture *testMoveGenerator, old *fakeMoveWindowTransport) {
				fixture.state.migrating = true
			},
			reason: protocol.ResidentMoveReason_ResidentMoveReasonMoveInProgress,
		},
		{
			name:      "client initializing",
			targetIps: []netip.Addr{testMoveTargetIpv4},
			configure: func(fixture *testMoveGenerator, old *fakeMoveWindowTransport) {
				fixture.state.initializing = true
			},
			reason: protocol.ResidentMoveReason_ResidentMoveReasonMoveInProgress,
		},
	} {
		client, _ := newApiMigrationTestClient(t)
		old := newFakeMoveWindowTransport(true)
		fixture := newTestMoveGenerator(t, client, old, DefaultPlatformTransportSettings(),
			func(*PlatformTransportSettings) apiWindowPlatformTransport {
				return newFakeWindowPlatformTransport(false)
			})
		c.configure(fixture, old)
		move := testAdvisoryMove(c.targetIps...)
		fixture.generator.MoveClientTransport(client, nil, move)
		fixture.join(t)
		select {
		case <-fixture.created:
			t.Fatalf("%s: a declined move built a replacement", c.name)
		default:
		}
		select {
		case <-old.closed:
			t.Fatalf("%s: a declined move closed the old transport", c.name)
		default:
		}
		got := fixture.reports.snapshot()
		want := NewResidentMoveDeclined(*move.MoveId, c.reason, int(c.ipFamily))
		if len(got) != 1 || !proto.Equal(got[0], want) {
			t.Errorf("%s: reports = %v, want %v", c.name, got, want)
		}
		if c.name != "move in progress" && fixture.migrating() {
			t.Errorf("%s: the decline left the client migrating", c.name)
		}
	}
}

// The decline report reaches the platform control as a ResidentMoveResult
// frame.
func TestApiWindowMoveDeclineReachesTheControl(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	settings := DefaultClientSettings()
	settings.EncryptionSettings.Mode = EncryptionModeOff
	settings.ControlPingTimeout = 0
	settings.Log = NewNoopLogger()
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
	control := NewClient(ctx, ControlId, NewNoContractClientOob(), settings)
	client.ContractManager().AddNoContractPeer(ControlId)
	control.ContractManager().AddNoContractPeer(client.ClientId())
	t.Cleanup(func() {
		cancel()
		join, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		for _, c := range []*Client{client, control} {
			c.CloseAndWait(join)
		}
	})
	sendRoute, ackRoute := make(chan []byte), make(chan []byte)
	client.RouteManager().UpdateTransport(NewSendGatewayTransport(), []Route{sendRoute})
	control.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{sendRoute})
	control.RouteManager().UpdateTransport(NewSendGatewayTransport(), []Route{ackRoute})
	client.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{ackRoute})
	results := make(chan *protocol.ResidentMoveResult, 4)
	control.AddReceiveCallback(func(source TransferPath, frames []*protocol.Frame, _ Peer) {
		for _, frame := range frames {
			if frame.MessageType != protocol.MessageType_TransferResidentMoveResult {
				continue
			}
			if message, err := FromFrame(frame); err == nil {
				select {
				case results <- proto.Clone(message).(*protocol.ResidentMoveResult):
				default:
				}
			}
		}
	})

	old := newFakeMoveWindowTransport(true)
	old.relayCarried = true
	fixture := newTestMoveGenerator(t, client, old, DefaultPlatformTransportSettings(),
		func(*PlatformTransportSettings) apiWindowPlatformTransport {
			return newFakeWindowPlatformTransport(false)
		})
	move := testAdvisoryMove(testMoveTargetIpv4)
	fixture.generator.MoveClientTransport(client, nil, move)
	result := receiveTestMoveValue(t, results, "the decline did not reach the control")
	if !proto.Equal(result, NewResidentMoveDeclined(
		*move.MoveId,
		protocol.ResidentMoveReason_ResidentMoveReasonExtenderCarrier,
		0,
	)) {
		t.Fatalf("control received %v", result)
	}
}

// A move id the client already handled is dropped: no replacement and no
// report, even after the first move finished.
func TestApiWindowMoveIdIsDeduplicated(t *testing.T) {
	client, _ := newApiMigrationTestClient(t)
	old := newFakeMoveWindowTransport(true)
	fixture := newTestMoveGenerator(t, client, old, DefaultPlatformTransportSettings(),
		func(*PlatformTransportSettings) apiWindowPlatformTransport {
			return newFakeWindowPlatformTransport(false)
		})
	fixture.generator.settings.MigrateConnectTimeout = 25 * time.Millisecond
	move := testAdvisoryMove(testMoveTargetIpv4)
	fixture.generator.MoveClientTransport(client, nil, move)
	receiveTestMoveValue(t, fixture.created, "the first move did not build a replacement")
	// the repeat lands while the first is in progress, and again after it
	fixture.generator.MoveClientTransport(client, nil, move)
	if !waitForCondition(5*time.Second, func() bool { return !fixture.migrating() }) {
		t.Fatal("the first move did not finish")
	}
	fixture.generator.MoveClientTransport(client, nil, move)
	fixture.join(t)
	select {
	case <-fixture.created:
		t.Fatal("a repeated move id built a replacement")
	default:
	}
	reports := fixture.reports.snapshot()
	if len(reports) != 1 || reports[0].Reason != protocol.ResidentMoveReason_ResidentMoveReasonConnectTimeout {
		t.Fatalf("reports = %v, want only the first move's timeout", reports)
	}
}

// A compulsory move whose carrier cannot use a target migrates by name: the
// replacement carries no move dial, the old transport closes once it
// connects, and nothing is reported.
func TestApiWindowCompulsoryMoveMigratesByNameWithoutAUsableTarget(t *testing.T) {
	client, _ := newApiMigrationTestClient(t)
	old := newFakeMoveWindowTransport(true)
	old.relayCarried = true
	next := newFakeWindowPlatformTransport(false)
	fixture := newTestMoveGenerator(t, client, old, DefaultPlatformTransportSettings(),
		func(*PlatformTransportSettings) apiWindowPlatformTransport { return next })
	fixture.generator.MoveClientTransport(client, nil, testCompulsoryMove(testMoveTargetIpv4))
	settings := receiveTestMoveValue(t, fixture.created, "the compulsory move did not build a replacement")
	if settings.ResidentMoveDial != nil {
		t.Fatal("a compulsory move through an extender dials a target")
	}
	select {
	case <-old.closed:
		t.Fatal("the old transport closed before the replacement connected")
	default:
	}
	next.connect()
	receiveTestMoveValue(t, old.closed, "the old transport did not close after the replacement connected")
	fixture.join(t)
	if fixture.current() != next {
		t.Fatal("the replacement was not installed")
	}
	requireTestMoveReports(t, fixture.reports)
}

// A compulsory move tries its target; after the target fails, the replacement
// connects by name and is installed, and the target failure is reported.
func TestApiWindowCompulsoryMoveFallsBackByNameAfterATargetFailure(t *testing.T) {
	client, _ := newApiMigrationTestClient(t)
	old := newFakeMoveWindowTransport(true)
	next := newFakeWindowPlatformTransport(false)
	fixture := newTestMoveGenerator(t, client, old, DefaultPlatformTransportSettings(),
		func(*PlatformTransportSettings) apiWindowPlatformTransport { return next })
	move := testCompulsoryMove(testMoveTargetIpv4)
	fixture.generator.MoveClientTransport(client, nil, move)
	dial := receiveTestMoveValue(t, fixture.created, "the compulsory move did not build a replacement").ResidentMoveDial
	if dial == nil || dial.Strict() {
		t.Fatalf("compulsory dial = %+v, want one that is not strict", dial)
	}
	dial.noteFailed(0, TransportModeH1, []netip.Addr{testMoveTargetIpv4}, protocol.ResidentMoveReason_ResidentMoveReasonDialError, 30*time.Millisecond, time.Now())
	select {
	case <-next.closed:
		t.Fatal("a compulsory move gave up on its replacement at the target failure")
	case <-old.closed:
		t.Fatal("the old transport closed before the replacement connected")
	default:
	}
	next.connect()
	receiveTestMoveValue(t, old.closed, "the old transport did not close after the replacement connected by name")
	fixture.join(t)
	if fixture.current() != next {
		t.Fatal("the replacement was not installed")
	}
	if fixture.tracker().MovedWithin(time.Hour, time.Now()) {
		t.Fatal("a migrate by name counted as a completed move")
	}
	requireTestMoveReports(t, fixture.reports, &protocol.ResidentMoveResult{
		MoveId:   move.MoveId.Bytes(),
		Outcome:  protocol.ResidentMoveOutcome_ResidentMoveOutcomeFailed,
		Reason:   protocol.ResidentMoveReason_ResidentMoveReasonDialError,
		IpFamily: 4,
		Elapsed:  30,
	})
}

// A drain frame, and any move while moves are disabled, migrates by name
// exactly as before: no move dial, make-before-break, no report.
func TestApiWindowDrainAndDisabledMovesMigrateByName(t *testing.T) {
	for _, c := range []struct {
		name       string
		move       *ResidentMove
		enableMove bool
	}{
		{name: "drain", move: NewResidentMove(&protocol.ResidentMigrate{MigrateTime: uint64(time.Now().UnixMilli())}), enableMove: true},
		{name: "advisory with moves disabled", move: testAdvisoryMove(testMoveTargetIpv4), enableMove: false},
	} {
		client, _ := newApiMigrationTestClient(t)
		old := newFakeMoveWindowTransport(true)
		next := newFakeWindowPlatformTransport(false)
		settings := DefaultPlatformTransportSettings()
		settings.EnableMove = c.enableMove
		settings.MoveDecline = func() bool { return true }
		fixture := newTestMoveGenerator(t, client, old, settings,
			func(*PlatformTransportSettings) apiWindowPlatformTransport { return next })
		fixture.generator.MoveClientTransport(client, nil, c.move)
		created := receiveTestMoveValue(t, fixture.created, c.name+": no replacement")
		if created.ResidentMoveDial != nil {
			t.Fatalf("%s: the replacement dials a target", c.name)
		}
		// a duplicate while pending builds no second replacement
		fixture.generator.MoveClientTransport(client, nil, c.move)
		select {
		case <-old.closed:
			t.Fatalf("%s: the old transport closed before the replacement connected", c.name)
		default:
		}
		next.connect()
		receiveTestMoveValue(t, old.closed, c.name+": the old transport did not close")
		fixture.join(t)
		select {
		case <-fixture.created:
			t.Fatalf("%s: a duplicate built a second replacement", c.name)
		default:
		}
		if fixture.current() != next {
			t.Fatalf("%s: the replacement was not installed", c.name)
		}
		requireTestMoveReports(t, fixture.reports)
	}
}

// A move never breaks before it makes. When the memory budget cannot hold an
// H3 replacement beside the old H3 carrier, an advisory move is declined and
// reported, and a compulsory one keeps the old carrier, which the platform
// closes at the close time; neither releases the old carrier.
func TestApiWindowMoveNeverBreaksBeforeMakeWhenTheBudgetIsFull(t *testing.T) {
	for _, advisory := range []bool{true, false} {
		testApiWindowMoveNeverBreaksBeforeMakeWhenTheBudgetIsFull(t, advisory)
	}
}

func testApiWindowMoveNeverBreaksBeforeMakeWhenTheBudgetIsFull(t *testing.T, advisory bool) {
	client, _ := newApiMigrationTestClient(t)
	strategy := NewClientStrategyWithDefaults(client.Ctx())
	budget := NewPlatformTransportBudget(4, 1)
	auth := ClientAuth{InstanceId: NewId()}

	oldAssigned := make(chan struct{})
	oldActive := make(chan struct{})
	var old *PlatformTransport
	oldSettings := testingPlatformTransportSettings()
	oldSettings.PlatformTransportBudget = budget
	oldSettings.H3BudgetByteCount = 4
	oldSettings.runH3ModeForTest = func(ctx context.Context, mode TransportMode, _ time.Duration) {
		<-oldAssigned
		old.setModeAvailable(mode, true)
		old.setRegistered(true)
		close(oldActive)
		<-ctx.Done()
		old.setRegistered(false)
		old.setModeAvailable(mode, false)
	}
	old = NewPlatformTransportWithTargetMode(
		client.Ctx(),
		strategy,
		client.RouteManager(),
		"https://"+testMoveHost,
		&auth,
		TransportModeH3,
		oldSettings,
	)
	close(oldAssigned)
	t.Cleanup(old.Close)
	receiveTestMoveValue(t, oldActive, "the old H3 did not acquire the budget")

	created := make(chan *PlatformTransport, 1)
	fixture := newTestMoveGenerator(t, client, old, oldSettings,
		func(settings *PlatformTransportSettings) apiWindowPlatformTransport {
			settingsValue := *settings
			settingsValue.runH3ModeForTest = func(ctx context.Context, _ TransportMode, _ time.Duration) {
				<-ctx.Done()
			}
			next := NewPlatformTransportWithTargetMode(
				client.Ctx(),
				strategy,
				client.RouteManager(),
				"https://"+testMoveHost,
				&auth,
				TransportModeH3,
				&settingsValue,
			)
			created <- next
			return next
		})
	fixture.generator.clientStrategy = strategy
	fixture.generator.platformTransportMode = TransportModeH3
	fixture.generator.platformTransportPolicyVer = 1
	fixture.state.policyVersion = 1

	move := testCompulsoryMove(testMoveTargetIpv4)
	if advisory {
		move = testAdvisoryMove(testMoveTargetIpv4)
	}
	fixture.generator.MoveClientTransport(client, nil, move)
	next := receiveTestMoveValue(t, created, "the move did not build a replacement")
	fixture.join(t)
	select {
	case <-next.Done():
	default:
		t.Fatalf("advisory=%t: the replacement the budget cannot hold was not closed", advisory)
	}
	select {
	case <-old.Done():
		t.Fatalf("advisory=%t: the move released the old H3 carrier", advisory)
	default:
	}
	if fixture.current() != old || !old.IsConnected() {
		t.Fatalf("advisory=%t: the old carrier does not carry the client", advisory)
	}
	if advisory {
		requireTestMoveReports(t, fixture.reports, NewResidentMoveDeclined(
			*move.MoveId,
			protocol.ResidentMoveReason_ResidentMoveReasonMemoryBudget,
			0,
		))
	} else {
		requireTestMoveReports(t, fixture.reports)
	}
}

// A frame from the control source reaches a mover whole -- kind, targets and
// preference -- and a frame from any other source reaches nothing.
func TestMultiClientChannelPassesTheWholeMoveToAMover(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mover := &recordingWindowTransportMover{
		moves:      make(chan *ResidentMove, 2),
		migrations: make(chan time.Time, 2),
	}
	channel := &multiClientChannel{
		ctx:               ctx,
		args:              &multiClientChannelArgs{},
		transportMigrator: mover,
	}
	message := testResidentMigrate(protocol.ResidentMoveKind_ResidentMoveKindAdvisory, NewId(), testMoveTargetIpv4)
	frame := RequireToFrameWithDefaultProtocolVersion(message)
	channel.clientReceive(SourceId(NewId()), []*protocol.Frame{frame}, Peer{})
	select {
	case <-mover.moves:
		t.Fatal("an ordinary source moved the transport")
	default:
	}
	channel.clientReceive(SourceId(ControlId), []*protocol.Frame{frame}, Peer{})
	move := receiveTestMoveValue(t, mover.moves, "the control move did not reach the mover")
	if !move.Advisory() || !slices.Equal(move.TargetIps, []netip.Addr{testMoveTargetIpv4}) || move.PreferDuration != 6*time.Hour {
		t.Fatalf("mover received %+v", move)
	}
	select {
	case <-mover.migrations:
		t.Fatal("a mover was also asked to migrate by name")
	default:
	}
}

type recordingWindowTransportMover struct {
	moves      chan *ResidentMove
	migrations chan time.Time
}

func (self *recordingWindowTransportMover) MigrateClientTransport(
	client *Client,
	args *MultiClientGeneratorClientArgs,
	migrateTime time.Time,
) {
	self.migrations <- migrateTime
}

func (self *recordingWindowTransportMover) MoveClientTransport(
	client *Client,
	args *MultiClientGeneratorClientArgs,
	move *ResidentMove,
) {
	self.moves <- move
}

// The whole move over real transports: a window client on one edge, reached
// by name, takes an advisory move to a target address served by a second
// edge. The replacement dials the target with the platform hostname kept for
// TLS and HTTP, the old transport keeps carrying until the replacement has
// arrived, and only then closes; reconnects then prefer the target.
func TestApiWindowMoveSwapsToTheTargetEdgeMakeBeforeBreak(t *testing.T) {
	certPem, keyPem, err := selfSign([]string{testMoveHost}, "move-test", time.Hour, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	edgeByName := newTestMoveH1ServerWithCertificate(t, certPem, keyPem)
	edgeAtTarget := newTestMoveH1ServerWithCertificate(t, certPem, keyPem)
	seam := newTestMoveDialSeam(edgeByName.address, testMoveHostAddress())
	seam.setRoute(testMoveTargetAddress(testMoveTargetIpv4), edgeAtTarget.address)

	client, _ := newApiMigrationTestClient(t)
	strategy := newTestMoveStrategy(t, client.Ctx(), edgeByName.roots, seam.dial)
	settings := testingPlatformTransportSettings()
	settings.ResidentMoveTracker = NewResidentMoveTracker()
	auth := ClientAuth{ByJwt: "testing", InstanceId: NewId(), AppVersion: "testing"}
	old := NewPlatformTransportWithTargetMode(
		client.Ctx(),
		strategy,
		client.RouteManager(),
		"wss://"+testMoveHost,
		&auth,
		TransportModeH1,
		settings,
	)
	t.Cleanup(old.Close)
	receiveTestMoveValue(t, edgeByName.requests, "the window did not reach the edge by name")
	if !waitForCondition(10*time.Second, func() bool {
		return old.IsConnected() && old.ActiveMode() == TransportModeH1
	}) {
		t.Fatal("the window transport did not connect")
	}

	generatorSettings := DefaultApiMultiClientGeneratorSettings()
	generatorSettings.MigrateConnectTimeout = 10 * time.Second
	generatorSettings.MigrateMaxScheduleDelay = 20 * time.Millisecond
	reports := &testMoveReports{}
	state := &apiWindowClientTransport{
		current:             old,
		settings:            settings,
		auth:                auth,
		policyVersion:       1,
		extenderIpsMonitor:  NewMonitorValue[uint64](0),
		residentMoveTracker: settings.ResidentMoveTracker,
	}
	generator := &ApiMultiClientGenerator{
		settings:                      generatorSettings,
		clientStrategy:                strategy,
		platformUrl:                   "wss://" + testMoveHost,
		platformTransportMode:         TransportModeH1,
		platformTransportPolicyVer:    1,
		transports:                    map[*Client]*apiWindowClientTransport{client: state},
		sendResidentMoveResultForTest: reports.observe,
	}

	generator.MoveClientTransport(client, nil, testAdvisoryMove(testMoveTargetIpv4))
	request := receiveTestMoveValue(t, edgeAtTarget.requests, "the replacement did not reach the target edge")
	if request.host != testMoveHost || request.moveVersion != "1" {
		t.Fatalf("target edge upgrade = %+v", request)
	}
	if serverName := receiveTestMoveValue(t, edgeAtTarget.serverNames, "no client hello at the target"); serverName != testMoveHost {
		t.Fatalf("target TLS server name = %q", serverName)
	}
	// make before break: the old transport still carried when the replacement
	// reached the target edge
	select {
	case <-old.Done():
		t.Fatal("the old transport closed before the replacement reached the target")
	default:
	}
	receiveTestMoveValue(t, old.Done(), "the old transport did not close after the arrival")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := generator.CloseTransportCreationAndWait(ctx); err != nil {
		t.Fatal(err)
	}
	generator.transportLock.Lock()
	current := state.current
	generator.transportLock.Unlock()
	next, ok := current.(*PlatformTransport)
	if !ok || next == old || !next.IsConnected() {
		t.Fatal("the connected replacement is not the window's transport")
	}
	t.Cleanup(next.Close)
	if got := settings.ResidentMoveTracker.preferredTargetIps(testMoveHost, 0, time.Now()); !slices.Equal(got, []netip.Addr{testMoveTargetIpv4}) {
		t.Fatalf("preference after the move = %v", got)
	}
	if !settings.ResidentMoveTracker.MovedWithin(time.Minute, time.Now()) {
		t.Fatal("the move does not count for the client interval")
	}
	requireTestMoveReports(t, reports)
}

// A window carried on H3 through alt moves over H1, the one carrier that can
// reach the target. When this network refuses H1 at the target, that failure
// is no evidence about the target -- the old transport proved H3, not H1 -- so
// it neither fails the move nor holds the target: the move times out with a
// connect timeout and the old transport stays. The replacement's H3 waits
// throughout and never sends to the target.
func TestApiWindowAltH3MoveTimesOutWhenTheTargetRefusesH1(t *testing.T) {
	server := newTestMoveH1Server(t, testMoveHost)
	// no route at all: H1 to the target is refused
	seam := newTestMoveDialSeam(server.address)
	client, _ := newApiMigrationTestClient(t)
	strategy := newTestMoveStrategy(t, client.Ctx(), server.roots, seam.dial)
	factory, h3DialedAddresses := newTestMoveRecordingPacketConnFactory()
	settings := testingPlatformTransportSettings()
	settings.AltUrl = "https://127.0.0.1:1"
	settings.H3PacketConnFactory = factory
	settings.ResidentMoveTracker = NewResidentMoveTracker()

	old := newFakeMoveWindowTransport(true)
	old.activeMode = TransportModeH3
	generatorSettings := DefaultApiMultiClientGeneratorSettings()
	generatorSettings.MigrateConnectTimeout = 3 * time.Second
	generatorSettings.MigrateMaxScheduleDelay = 20 * time.Millisecond
	reports := &testMoveReports{}
	state := &apiWindowClientTransport{
		current:             old,
		settings:            settings,
		auth:                ClientAuth{ByJwt: "testing", InstanceId: NewId(), AppVersion: "testing"},
		policyVersion:       1,
		extenderIpsMonitor:  NewMonitorValue[uint64](0),
		residentMoveTracker: settings.ResidentMoveTracker,
	}
	created := make(chan *PlatformTransport, 1)
	generator := &ApiMultiClientGenerator{
		settings:                      generatorSettings,
		clientStrategy:                strategy,
		platformUrl:                   "wss://" + testMoveHost,
		platformTransportMode:         TransportModeAuto,
		platformTransportPolicyVer:    1,
		transports:                    map[*Client]*apiWindowClientTransport{client: state},
		sendResidentMoveResultForTest: reports.observe,
	}
	generator.settings.PlatformTransportCreated = func(_ *Client, transport *PlatformTransport) {
		created <- transport
	}

	move := testAdvisoryMove(testMoveTargetIpv4)
	generator.MoveClientTransport(client, nil, move)
	next := receiveTestMoveValue(t, created, "the move on alt built no replacement")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := generator.CloseTransportCreationAndWait(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-next.Done():
	default:
		t.Fatal("the timed out replacement was not closed")
	}
	select {
	case <-old.closed:
		t.Fatal("the timed out move closed the old transport")
	default:
	}
	generator.transportLock.Lock()
	current := state.current
	generator.transportLock.Unlock()
	if current != old {
		t.Fatal("the timed out move displaced the old transport")
	}
	if !slices.Contains(seam.dialedAddresses(), testMoveTargetAddress(testMoveTargetIpv4)) {
		t.Fatal("the replacement never tried the target over H1")
	}
	if settings.ResidentMoveTracker.TargetFailedWithin(testMoveTargetIpv4, time.Hour, time.Now()) {
		t.Fatal("an H1 refusal on a client that moved off H3 held the target")
	}
	for _, address := range h3DialedAddresses() {
		if address == testMoveTargetAddress(testMoveTargetIpv4) {
			t.Fatal("the alt H3 carrier sent to the target")
		}
	}
	got := reports.snapshot()
	if len(got) != 1 ||
		got[0].Outcome != protocol.ResidentMoveOutcome_ResidentMoveOutcomeFailed ||
		got[0].Reason != protocol.ResidentMoveReason_ResidentMoveReasonConnectTimeout ||
		got[0].IpFamily != 0 || got[0].Elapsed < 3000 ||
		!slices.Equal(got[0].MoveId, move.MoveId.Bytes()) {
		t.Fatalf("reports = %v, want one connect timeout", got)
	}
}
