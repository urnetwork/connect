package connect

import (
	"context"
	"maps"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

func TestMultiClientChannelAcceptsMigrationOnlyFromControl(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	migrator := &recordingWindowTransportMigrator{calls: make(chan time.Time, 2)}
	channel := &multiClientChannel{
		ctx:               ctx,
		args:              &multiClientChannelArgs{},
		transportMigrator: migrator,
	}
	want := time.Now().Add(time.Second).Truncate(time.Millisecond)
	frame := RequireToFrameWithDefaultProtocolVersion(&protocol.ResidentMigrate{
		MigrateTime: uint64(want.UnixMilli()),
	})

	channel.clientReceive(SourceId(NewId()), []*protocol.Frame{frame}, Peer{})
	select {
	case <-migrator.calls:
		t.Fatal("ordinary data source triggered transport migration")
	default:
	}

	channel.clientReceive(SourceId(ControlId), []*protocol.Frame{frame}, Peer{})
	select {
	case got := <-migrator.calls:
		if !got.Equal(want) {
			t.Fatalf("migration time = %s, want %s", got, want)
		}
	case <-time.After(time.Second):
		t.Fatal("control-source migration was not forwarded")
	}
}

type recordingWindowTransportMigrator struct {
	calls chan time.Time
}

func (self *recordingWindowTransportMigrator) MigrateClientTransport(
	client *Client,
	args *MultiClientGeneratorClientArgs,
	migrateTime time.Time,
) {
	self.calls <- migrateTime
}

type fakeWindowPlatformTransport struct {
	mutex     sync.Mutex
	connected bool
	notify    chan struct{}
	closed    chan struct{}
	closeOnce sync.Once
	// onClose, when set, runs synchronously inside Close before `closed` is
	// signaled — a seam to observe migrator state at the exact instant of
	// close (see TestApiWindowTransportMigrationDisarmsBeforeClosingReplacement).
	onClose func()
}

func newFakeWindowPlatformTransport(connected bool) *fakeWindowPlatformTransport {
	return &fakeWindowPlatformTransport{
		connected: connected,
		notify:    make(chan struct{}),
		closed:    make(chan struct{}),
	}
}

func (self *fakeWindowPlatformTransport) ConnectedNotify() <-chan struct{} {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return self.notify
}

func (self *fakeWindowPlatformTransport) IsConnected() bool {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return self.connected
}

func (self *fakeWindowPlatformTransport) Close() {
	self.closeOnce.Do(func() {
		if self.onClose != nil {
			self.onClose()
		}
		close(self.closed)
	})
}

func (self *fakeWindowPlatformTransport) connect() {
	self.mutex.Lock()
	if !self.connected {
		self.connected = true
		close(self.notify)
		self.notify = make(chan struct{})
	}
	self.mutex.Unlock()
}

func newApiMigrationTestClient(t *testing.T) (*Client, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	clientSettings := DefaultClientSettings()
	clientSettings.Log = NewNoopLogger()
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), clientSettings)
	t.Cleanup(func() {
		client.Close()
		cancel()
	})
	return client, cancel
}

func TestApiWindowTransportMigrationIsMakeBeforeBreakAndDeduplicated(t *testing.T) {
	client, _ := newApiMigrationTestClient(t)
	old := newFakeWindowPlatformTransport(true)
	next := newFakeWindowPlatformTransport(false)
	created := make(chan struct{}, 2)
	settings := DefaultApiMultiClientGeneratorSettings()
	settings.MigrateConnectTimeout = time.Second
	settings.MigrateMaxScheduleDelay = 20 * time.Millisecond
	transportSettings := DefaultPlatformTransportSettings()
	state := &apiWindowClientTransport{
		current:  old,
		settings: transportSettings,
		auth:     ClientAuth{InstanceId: NewId()},
	}
	generator := &ApiMultiClientGenerator{
		settings:   settings,
		transports: map[*Client]*apiWindowClientTransport{client: state},
		newPlatformTransport: func(
			client *Client,
			auth *ClientAuth,
			settings *PlatformTransportSettings,
		) apiWindowPlatformTransport {
			created <- struct{}{}
			return next
		},
	}

	// A far-future timestamp is clamped, and a duplicate while pending must
	// not construct a second replacement.
	generator.MigrateClientTransport(client, nil, time.Now().Add(24*time.Hour))
	generator.MigrateClientTransport(client, nil, time.Now())
	select {
	case <-created:
	case <-time.After(time.Second):
		t.Fatal("clamped migration did not construct a replacement")
	}
	select {
	case <-created:
		t.Fatal("duplicate migration constructed another replacement")
	case <-time.After(30 * time.Millisecond):
	}

	select {
	case <-old.closed:
		t.Fatal("old transport closed before replacement connected")
	default:
	}

	next.connect()
	select {
	case <-old.closed:
	case <-time.After(time.Second):
		t.Fatal("old transport was not closed after replacement connected")
	}
	generator.transportLock.Lock()
	current := state.current
	generator.transportLock.Unlock()
	if current != next {
		t.Fatal("connected replacement was not installed")
	}
}

func TestApiWindowTransportMigrationKeepsOldOnTimeout(t *testing.T) {
	client, _ := newApiMigrationTestClient(t)
	old := newFakeWindowPlatformTransport(true)
	next := newFakeWindowPlatformTransport(false)
	settings := DefaultApiMultiClientGeneratorSettings()
	settings.MigrateConnectTimeout = 25 * time.Millisecond
	state := &apiWindowClientTransport{
		current:  old,
		settings: DefaultPlatformTransportSettings(),
		auth:     ClientAuth{InstanceId: NewId()},
	}
	generator := &ApiMultiClientGenerator{
		settings:   settings,
		transports: map[*Client]*apiWindowClientTransport{client: state},
		newPlatformTransport: func(
			client *Client,
			auth *ClientAuth,
			settings *PlatformTransportSettings,
		) apiWindowPlatformTransport {
			return next
		},
	}

	generator.MigrateClientTransport(client, nil, time.Now())
	select {
	case <-next.closed:
	case <-time.After(time.Second):
		t.Fatal("failed replacement was not closed at timeout")
	}
	select {
	case <-old.closed:
		t.Fatal("old transport was closed after replacement timeout")
	default:
	}
	generator.transportLock.Lock()
	current := state.current
	migrating := state.migrating
	generator.transportLock.Unlock()
	if current != old {
		t.Fatal("failed replacement displaced the old transport")
	}
	if migrating {
		t.Fatal("migration remained armed after timeout")
	}
}

// TestApiWindowTransportMigrationDisarmsBeforeClosingReplacement pins the
// ordering that made TestApiWindowTransportMigrationKeepsOldOnTimeout a
// load-sensitive flake: on the connect-timeout path the migrator must release
// its migration claim (state.migrating = false) BEFORE it closes the failed
// replacement, so the replacement's close is the definitive "migration
// released" signal. The disarm otherwise ran in a defer AFTER the close,
// leaving a window — observable under full-suite load — where the replacement
// was closed but a follow-up migration was still refused as "already
// migrating".
//
// Deterministic by construction: the fake transport's onClose hook samples
// state.migrating at the exact instant of close. Disarm-before-close => false
// at that instant; the deferred-only ordering => true.
func TestApiWindowTransportMigrationDisarmsBeforeClosingReplacement(t *testing.T) {
	client, _ := newApiMigrationTestClient(t)
	old := newFakeWindowPlatformTransport(true)
	next := newFakeWindowPlatformTransport(false)
	settings := DefaultApiMultiClientGeneratorSettings()
	settings.MigrateConnectTimeout = 25 * time.Millisecond
	state := &apiWindowClientTransport{
		current:  old,
		settings: DefaultPlatformTransportSettings(),
		auth:     ClientAuth{InstanceId: NewId()},
	}
	generator := &ApiMultiClientGenerator{
		settings:   settings,
		transports: map[*Client]*apiWindowClientTransport{client: state},
		newPlatformTransport: func(
			client *Client,
			auth *ClientAuth,
			settings *PlatformTransportSettings,
		) apiWindowPlatformTransport {
			return next
		},
	}

	var migratingAtClose bool
	next.onClose = func() {
		generator.transportLock.Lock()
		migratingAtClose = state.migrating
		generator.transportLock.Unlock()
	}

	generator.MigrateClientTransport(client, nil, time.Now())
	select {
	case <-next.closed:
	case <-time.After(time.Second):
		t.Fatal("failed replacement was not closed at timeout")
	}
	if migratingAtClose {
		t.Fatal("migration was still armed at the instant the replacement was closed: disarm must happen-before the close so the close is the definitive released signal")
	}
}

// Generator teardown must join a migration that has entered replacement
// construction and reject every creator that arrives after the close edge.
func TestApiWindowTransportCreationCloseJoinsHeldMigrationCreator(t *testing.T) {
	client, _ := newApiMigrationTestClient(t)
	old := newFakeWindowPlatformTransport(true)
	next := newFakeWindowPlatformTransport(true)
	creationEntered := make(chan struct{})
	releaseCreation := make(chan struct{})
	lateCreation := make(chan struct{}, 1)
	var creationCount atomic.Int32
	state := &apiWindowClientTransport{
		current:  old,
		settings: DefaultPlatformTransportSettings(),
		auth:     ClientAuth{InstanceId: NewId()},
	}
	generator := &ApiMultiClientGenerator{
		settings:   DefaultApiMultiClientGeneratorSettings(),
		transports: map[*Client]*apiWindowClientTransport{client: state},
		newPlatformTransport: func(
			client *Client,
			auth *ClientAuth,
			settings *PlatformTransportSettings,
		) apiWindowPlatformTransport {
			if creationCount.Add(1) == 1 {
				close(creationEntered)
				<-releaseCreation
				return next
			}
			lateCreation <- struct{}{}
			return newFakeWindowPlatformTransport(true)
		},
	}

	generator.MigrateClientTransport(client, nil, time.Now())
	<-creationEntered
	closeWaitEntered := make(chan struct{})
	generator.transportCreation.beforeWaitForTest = func() {
		close(closeWaitEntered)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	closeResult := make(chan error, 1)
	go func() {
		closeResult <- generator.CloseTransportCreationAndWait(ctx)
	}()
	<-closeWaitEntered
	select {
	case err := <-closeResult:
		t.Fatalf("transport-creation close skipped held migration: %v", err)
	default:
	}
	close(releaseCreation)
	if err := <-closeResult; err != nil {
		t.Fatal(err)
	}

	generator.MigrateClientTransport(client, nil, time.Now())
	select {
	case <-lateCreation:
		t.Fatal("migration created a transport after generator close")
	default:
	}
}

// A runtime Device policy change replaces every live window
// make-before-break and preserves equal Auto priorities in the concrete
// PlatformTransport settings used for the replacement.
func TestApiWindowTransportPolicyChangeIsLiveAndMakeBeforeBreak(t *testing.T) {
	client, _ := newApiMigrationTestClient(t)
	old := newFakeWindowPlatformTransport(true)
	next := newFakeWindowPlatformTransport(false)
	createdSettings := make(chan *PlatformTransportSettings, 1)
	state := &apiWindowClientTransport{
		current:       old,
		settings:      DefaultPlatformTransportSettings(),
		auth:          ClientAuth{InstanceId: NewId()},
		policyVersion: 1,
	}
	generator := &ApiMultiClientGenerator{
		settings:                   DefaultApiMultiClientGeneratorSettings(),
		platformTransportMode:      TransportModeH1,
		platformTransportPolicyVer: 1,
		transports:                 map[*Client]*apiWindowClientTransport{client: state},
		newPlatformTransport: func(
			client *Client,
			auth *ClientAuth,
			settings *PlatformTransportSettings,
		) apiWindowPlatformTransport {
			createdSettings <- settings
			return next
		},
	}

	preferences := map[TransportMode]int{
		TransportModeH3:        1,
		TransportModeH1:        1,
		TransportModeH3Dns:     2,
		TransportModeH3DnsPump: 3,
	}
	generator.SetPlatformTransportPolicy(TransportModeAuto, preferences)
	var settings *PlatformTransportSettings
	select {
	case settings = <-createdSettings:
	case <-time.After(time.Second):
		t.Fatal("policy change did not construct a replacement")
	}
	if !maps.Equal(settings.ModePreferences, preferences) {
		t.Fatalf("replacement preferences = %v, want %v", settings.ModePreferences, preferences)
	}
	select {
	case <-old.closed:
		t.Fatal("old transport closed before policy replacement connected")
	default:
	}

	next.connect()
	select {
	case <-old.closed:
	case <-time.After(time.Second):
		t.Fatal("old transport was not closed after policy replacement connected")
	}
	if state.current != next {
		t.Fatal("policy replacement was not installed")
	}

	// Canonically identical input is a no-op and cannot churn live routes.
	generator.SetPlatformTransportPolicy(TransportModeAuto, maps.Clone(preferences))
	select {
	case <-createdSettings:
		t.Fatal("identical policy constructed another replacement")
	case <-time.After(50 * time.Millisecond):
	}
}
