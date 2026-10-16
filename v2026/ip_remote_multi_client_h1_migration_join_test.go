package connect

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// Keep the actual delegated write slice alive while the test socket is
// blocked, just as a real pending net.Conn.Write does. Pool counters do not
// include WebSocket's owned 16-KiB batch buffer or large unpooled messages.
type retainedH1MigrationWriteConn struct {
	*closeInterruptWriteConn
	retainedBytes atomic.Int64
}

func (c *retainedH1MigrationWriteConn) Write(buffer []byte) (int, error) {
	if c.blockWrite.Load() {
		c.retainedBytes.Store(int64(cap(buffer)))
		defer c.retainedBytes.Store(0)
		defer runtime.KeepAlive(buffer)
	}
	return c.closeInterruptWriteConn.Write(buffer)
}

// A migration unlinks the old carrier before the generated Client retires.
// Its socket writer and receive cleanup are external to the Client's indexed
// census. A successful generator join must cover that old generation too.
func TestApiMultiClientGeneratorJoinsUnlinkedH1MigrationOwners(t *testing.T) {
	assertMessagePoolOwnership(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	generator, removeCount, closeAPI := newRemoveClientTestGenerator(t, 4, ctx, ctx)
	defer closeAPI()
	defer generator.clientStrategy.Close()
	platform := newTestingPlatformServer(t)
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), closeWaitClientSettings())
	defer client.CloseAndWait(context.Background())

	wrapped := make(chan *retainedH1MigrationWriteConn, 1)
	strategySettings := DefaultClientStrategySettings()
	strategySettings.EnableResilient = false
	strategySettings.ParallelBlockSize = 1
	strategySettings.MinNextConnectDelay = 0
	strategySettings.MaxNextConnectDelay = 0
	dialer := &net.Dialer{}
	strategySettings.DialContextSettings = &DialContextSettings{
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			conn, err := dialer.DialContext(ctx, network, address)
			if err != nil {
				return nil, err
			}
			blocked := &retainedH1MigrationWriteConn{closeInterruptWriteConn: newCloseInterruptWriteConn(conn)}
			wrapped <- blocked
			return blocked, nil
		},
	}
	oldStrategy := NewClientStrategy(ctx, strategySettings)
	defer oldStrategy.Close()
	budget := NewPlatformTransportBudget(16*1024*1024, 4)
	settings := testingPlatformTransportSettings()
	settings.PlatformTransportBudget = budget
	routesRemoved, receiveCleanup := make(chan struct{}), make(chan struct{})
	releaseRoutes, releaseReceive := make(chan struct{}), make(chan struct{})
	var routesOnce, receiveOnce, releaseRoutesOnce, releaseReceiveOnce sync.Once
	settings.afterRoutesRemovedForTest = func() {
		routesOnce.Do(func() { close(routesRemoved) })
		<-releaseRoutes
	}
	settings.beforeReceiveWorkerCleanupForTest = func() {
		receiveOnce.Do(func() { close(receiveCleanup) })
		<-releaseReceive
	}
	auth := &ClientAuth{ByJwt: "local-fixture", InstanceId: NewId(), AppVersion: "test"}
	old := NewPlatformTransportWithTargetMode(client.Ctx(), oldStrategy,
		client.RouteManager(), platform.url, auth, TransportModeH1, settings)
	var nextPointer atomic.Pointer[PlatformTransport]
	defer func() {
		releaseRoutesOnce.Do(func() { close(releaseRoutes) })
		releaseReceiveOnce.Do(func() { close(releaseReceive) })
		_ = old.CloseAndWait(context.Background())
		if next := nextPointer.Load(); next != nil {
			_ = next.CloseAndWait(context.Background())
		}
		generator.transportLock.Lock()
		_, indexed := generator.transports[client]
		generator.transportLock.Unlock()
		if indexed {
			generator.RemoveClientWithArgs(client, &MultiClientGeneratorClientArgs{ClientId: client.ClientId(), ClientAuth: auth})
		}
		client.Cancel()
		_ = client.CloseAndWait(context.Background())
		_ = generator.CloseAndWait(context.Background())
	}()
	waitConnected := func(transport *PlatformTransport) {
		t.Helper()
		for !transport.IsConnected() {
			notify := transport.ConnectedNotify()
			if transport.IsConnected() {
				break
			}
			select {
			case <-notify:
			case <-ctx.Done():
				t.Fatal("local H1 carrier did not connect")
			}
		}
	}
	waitConnected(old)
	var blocked *retainedH1MigrationWriteConn
	select {
	case blocked = <-wrapped:
	case <-ctx.Done():
		t.Fatal("old H1 socket was not wrapped")
	}
	blocked.blockWrite.Store(true)
	writer := client.RouteManager().OpenMultiRouteWriter(DestinationId(NewId()))
	defer client.RouteManager().CloseMultiRouteWriter(writer)
	poolBefore := MessagePoolOutstandingByteCount()
	message := MessagePoolGet(32 * 1024)
	if err := writer.Write(ctx, message, time.Second); err != nil {
		MessagePoolReturn(message)
		t.Fatal(err)
	}
	message = nil // only the real H1 writer retains the submitted message
	waitCloseWaitBarrier(t, ctx, blocked.writeStarted, "old H1 socket write")
	args := &MultiClientGeneratorClientArgs{ClientId: client.ClientId(), ClientAuth: auth}
	generator.transportLock.Lock()
	generator.transportIdle = make(chan struct{})
	generator.transports[client] = &apiWindowClientTransport{current: old, settings: settings, auth: *auth}
	generator.transportLock.Unlock()
	generator.newPlatformTransport = func(client *Client, auth *ClientAuth, _ TransportMode, _ *PlatformTransportSettings) apiWindowPlatformTransport {
		nextSettings := testingPlatformTransportSettings()
		nextSettings.PlatformTransportBudget = budget
		next := NewPlatformTransportWithTargetMode(client.Ctx(), generator.clientStrategy,
			client.RouteManager(), platform.url, auth, TransportModeH1, nextSettings)
		nextPointer.Store(next)
		return next
	}
	generator.MigrateClientTransport(client, args, time.Now())
	waitCloseWaitBarrier(t, ctx, routesRemoved, "unlinked old H1 route removal")
	generator.transportLock.Lock()
	current := generator.transports[client].current
	generator.transportLock.Unlock()
	next := nextPointer.Load()
	if current != next || next == nil || !next.IsConnected() {
		t.Fatal("replacement was not live before old carrier retirement")
	}
	// The replacement must carry useful work even while retirement is held.
	probe := MessagePoolGet(64)
	if err := writer.Write(ctx, probe, time.Second); err != nil {
		MessagePoolReturn(probe)
		t.Fatal(err)
	}
	if !waitForCondition(time.Second, func() bool { return platform.dataMessages.Load() != 0 }) {
		t.Fatal("held old retirement blocked healthy replacement H1 traffic")
	}
	heldWriteBytes := blocked.retainedBytes.Load()
	if heldWriteBytes == 0 {
		t.Fatal("old carrier did not retain its blocked socket Write buffer")
	}

	generator.RemoveClientWithArgs(client, args)
	client.Cancel()
	if err := client.CloseAndWait(ctx); err != nil {
		t.Fatal(err)
	}
	owners := client.MemoryOwnerCensus()
	if owners.SendWorkers != 0 || owners.ReceiveWorkers != 0 || owners.PacingServices != 0 {
		t.Fatalf("indexed client workers did not retire: %+v", owners)
	}
	if !generator.transportLock.TryLock() {
		t.Fatal("retired carrier join holds the transport lock")
	}
	indexed := len(generator.transports)
	generator.transportLock.Unlock()
	if indexed != 0 {
		t.Fatalf("old generation remained indexed: %d", indexed)
	}
	shortJoin, stopJoin := context.WithTimeout(ctx, 50*time.Millisecond)
	err := generator.CloseAndWait(shortJoin)
	stopJoin()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("generator completed with unlinked H1 writer still held: err=%v retained-write-buffer=%d pool-delta=%d", err, blocked.retainedBytes.Load(), MessagePoolOutstandingByteCount()-poolBefore)
	}
	select {
	case <-old.Done():
		t.Fatal("old H1 completed before its blocked socket was closed")
	default:
	}
	releaseRoutesOnce.Do(func() { close(releaseRoutes) })
	waitCloseWaitBarrier(t, ctx, receiveCleanup, "old H1 receive cleanup")
	select {
	case <-blocked.closed:
	default:
		t.Fatal("old receive cleanup preceded interrupting the socket writer")
	}
	secondJoin, stopSecond := context.WithTimeout(ctx, 50*time.Millisecond)
	err = generator.CloseAndWait(secondJoin)
	stopSecond()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("generator completed before unlinked H1 receive cleanup: %v", err)
	}
	releaseReceiveOnce.Do(func() { close(releaseReceive) })
	if err := generator.CloseAndWait(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-old.Done():
	default:
		t.Fatal("successful join precedes old carrier completion")
	}
	if claims := budget.MemoryOwnerCensus(); claims.H1Count != 0 || claims.H1Bytes != 0 {
		t.Fatalf("joined carriers retain H1 claims: %+v", claims)
	}
	if held := blocked.retainedBytes.Load(); held != 0 {
		t.Fatalf("successful join retained socket Write bytes: %d", held)
	}
	if got := removeCount.Load(); got != 1 {
		t.Fatalf("client identity retired %d times, want one", got)
	}
	t.Logf("unlinked carrier: indexed-clients=0 indexed-workers=0 blocked-write-buffer=%d->0 joined-pool-delta=%d; replacement traffic succeeded while old retirement held", heldWriteBytes, MessagePoolOutstandingByteCount()-poolBefore)
}

type joiningMigrationTestTransport struct {
	*fakeWindowPlatformTransport
	joinEntered chan struct{}
	release     chan struct{}
	joinOnce    sync.Once
}

func (p *joiningMigrationTestTransport) CloseAndWait(ctx context.Context) error {
	p.Close()
	p.joinOnce.Do(func() { close(p.joinEntered) })
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-p.release:
		return nil
	}
}

// A never-published replacement is also externally owned. Cancellation,
// timeout, and losing the indexed generation must not make it invisible to
// the transport-creation join while its teardown still runs.
func TestApiWindowTransportMigrationJoinsDiscardedReplacement(t *testing.T) {
	for _, reason := range []string{"connect-timeout", "client-canceled", "lost-generation"} {
		t.Run(reason, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			client, cancelClient := newApiMigrationTestClient(t)
			defer cancelClient()
			old := newFakeWindowPlatformTransport(true)
			next := &joiningMigrationTestTransport{
				fakeWindowPlatformTransport: newFakeWindowPlatformTransport(false),
				joinEntered:                 make(chan struct{}), release: make(chan struct{}),
			}
			var releaseOnce sync.Once
			settings := DefaultApiMultiClientGeneratorSettings()
			if reason == "connect-timeout" {
				settings.MigrateConnectTimeout = 10 * time.Millisecond
			}
			state := &apiWindowClientTransport{current: old, settings: DefaultPlatformTransportSettings()}
			generator := &ApiMultiClientGenerator{
				settings: settings, transports: map[*Client]*apiWindowClientTransport{client: state},
				newPlatformTransport: func(*Client, *ClientAuth, TransportMode, *PlatformTransportSettings) apiWindowPlatformTransport {
					return next
				},
			}
			defer func() {
				releaseOnce.Do(func() { close(next.release) })
				_ = generator.CloseTransportCreationAndWait(context.Background())
			}()
			generator.MigrateClientTransport(client, nil, time.Now())
			waitCloseWaitBarrier(t, ctx, next.waitStarted, "replacement readiness wait")
			switch reason {
			case "client-canceled":
				client.Cancel()
			case "lost-generation":
				generator.transportLock.Lock()
				delete(generator.transports, client)
				generator.transportLock.Unlock()
				next.connect()
			}
			waitCloseWaitBarrier(t, ctx, next.closed, "discarded replacement cancellation")
			waitCloseWaitBarrier(t, ctx, next.joinEntered, "discarded replacement join")
			if !generator.transportLock.TryLock() {
				t.Fatal("discarded carrier join holds transport lock")
			}
			generator.transportLock.Unlock()
			if !generator.transportPolicyLock.TryLock() {
				t.Fatal("discarded carrier join holds policy lock")
			}
			generator.transportPolicyLock.Unlock()
			short, stop := context.WithTimeout(ctx, 10*time.Millisecond)
			err := generator.CloseTransportCreationAndWait(short)
			stop()
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("creator join abandoned discarded carrier: %v", err)
			}
			select {
			case <-old.closed:
				t.Fatal("discarding an unconnected replacement retired the healthy old carrier")
			default:
			}
			releaseOnce.Do(func() { close(next.release) })
			if err := generator.CloseTransportCreationAndWait(ctx); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Opt-in local measurement, not a timing assertion or mobile-memory gate.
// Measure full old-carrier retirement on both revisions so the baseline's
// earlier, incorrect completion publication cannot masquerade as speed.
func TestApiWindowH1MigrationRetirementMeasurements(t *testing.T) {
	if os.Getenv("CONNECT_H1_RETIREMENT_MEASURE") != "1" {
		t.Skip("set CONNECT_H1_RETIREMENT_MEASURE=1 for local H1 retirement timings")
	}
	assertMessagePoolOwnership(t)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	generator, _, closeAPI := newRemoveClientTestGenerator(t, 4, ctx, ctx)
	defer closeAPI()
	defer generator.clientStrategy.Close()
	// Ordinary platform counters also include control traffic. Match only
	// unique measurement payloads, not every nonempty WebSocket message.
	received := make(chan int, 16)
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	var serverWorkers sync.WaitGroup
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		serverWorkers.Add(1)
		defer serverWorkers.Done()
		defer ws.Close()
		for {
			_, packet, err := ws.ReadMessage()
			if err != nil {
				return
			}
			if len(packet) == 256 && packet[0] == 0xb1 && packet[1] == 0xaf {
				received <- int(packet[2])
			}
		}
	}))
	defer func() { server.Close(); serverWorkers.Wait() }()
	platformURL := "ws" + strings.TrimPrefix(server.URL, "http")
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), closeWaitClientSettings())
	settings := testingPlatformTransportSettings()
	budget := NewPlatformTransportBudget(16*1024*1024, 4)
	settings.PlatformTransportBudget = budget
	auth := &ClientAuth{ByJwt: "local-fixture", InstanceId: NewId(), AppVersion: "test"}
	args := &MultiClientGeneratorClientArgs{ClientId: client.ClientId(), ClientAuth: auth}
	makeCarrier := func() *PlatformTransport {
		return NewPlatformTransportWithTargetMode(client.Ctx(), generator.clientStrategy,
			client.RouteManager(), platformURL, auth, TransportModeH1, settings)
	}
	current := makeCarrier()
	defer func() {
		generator.transportLock.Lock()
		_, indexed := generator.transports[client]
		generator.transportLock.Unlock()
		if indexed {
			generator.RemoveClientWithArgs(client, args)
		}
		client.Cancel()
		_ = generator.CloseAndWait(context.Background())
		_ = current.CloseAndWait(context.Background())
		_ = client.CloseAndWait(context.Background())
	}()
	for !current.IsConnected() {
		notify := current.ConnectedNotify()
		if current.IsConnected() {
			break
		}
		waitCloseWaitBarrier(t, ctx, notify, "initial healthy H1 readiness")
	}
	generator.transportLock.Lock()
	generator.transportIdle = make(chan struct{})
	generator.transports[client] = &apiWindowClientTransport{current: current, settings: settings, auth: *auth}
	generator.transportLock.Unlock()
	created := make(chan *PlatformTransport, 1)
	generator.newPlatformTransport = func(*Client, *ClientAuth, TransportMode, *PlatformTransportSettings) apiWindowPlatformTransport {
		next := makeCarrier()
		created <- next
		return next
	}
	writer := client.RouteManager().OpenMultiRouteWriter(DestinationId(NewId()))
	defer client.RouteManager().CloseMultiRouteWriter(writer)
	var retirement [8]time.Duration
	for index := range retirement {
		old := current
		start := time.Now()
		generator.MigrateClientTransport(client, args, start)
		select {
		case current = <-created:
		case <-ctx.Done():
			t.Fatal("healthy migration did not create its replacement")
		}
		generator.transportCreation.mutex.Lock()
		idle := generator.transportCreation.idle
		generator.transportCreation.mutex.Unlock()
		waitCloseWaitBarrier(t, ctx, idle, "healthy migration owner")
		waitCloseWaitBarrier(t, ctx, old.Done(), "full healthy old-carrier retirement")
		retirement[index] = time.Since(start)
		if !current.IsConnected() {
			t.Fatal("healthy retirement lost its connected replacement")
		}
		packet := MessagePoolGet(256)
		packet[0], packet[1], packet[2] = 0xb1, 0xaf, byte(index)
		if err := writer.Write(ctx, packet, time.Second); err != nil {
			MessagePoolReturn(packet)
			t.Fatal(err)
		}
		select {
		case got := <-received:
			if got != index {
				t.Fatalf("healthy H1 payload mismatch: got=%d want=%d", got, index)
			}
		case <-ctx.Done():
			t.Fatal("healthy replacement did not deliver its measurement packet")
		}
	}
	closeStart := time.Now()
	generator.RemoveClientWithArgs(client, args)
	client.Cancel()
	if err := generator.CloseAndWait(ctx); err != nil {
		t.Fatal(err)
	}
	closed := time.Since(closeStart)
	if claims := budget.MemoryOwnerCensus(); claims.H1Count != 0 || claims.H1Bytes != 0 {
		t.Fatalf("healthy joined migration retained claims: %+v", claims)
	}
	t.Logf("healthy-h1 full-retirement=%v close=%s delivered=%d final-claims=0", retirement, closed, len(retirement))
}
