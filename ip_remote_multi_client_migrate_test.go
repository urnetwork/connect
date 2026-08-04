package connect

import (
	"context"
	"sync"
	"testing"
	"time"

)

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
