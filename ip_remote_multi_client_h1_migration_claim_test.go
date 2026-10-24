// Migration claims belong to one worker even when failed-carrier retirement
// overlaps its successor. H1 capacity refusal preserves the live carrier.
package connect

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gorilla/websocket"
	"github.com/urnetwork/connect/protocol"
)

// The close/join and constructor gates force A to release, admit B, and
// finish A's defers while B still owns the migration claim. Quiescence proves
// completion without adding a production hook or sampling scheduler timing.
func requireApiMigrationSuccessorClaim(t *testing.T, failure string) {
	t.Helper()
	// The lazy process-global sampler must be born outside this bubble.
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		client := NewClient(ctx, NewId(), NewNoContractClientOob(), closeWaitClientSettings())
		old := newFakeMoveWindowTransport(true)
		a := &joiningMigrationTestTransport{
			fakeWindowPlatformTransport: newFakeWindowPlatformTransport(false),
			joinEntered:                 make(chan struct{}),
			release:                     make(chan struct{}),
		}
		b := newFakeWindowPlatformTransport(false)
		bEntered, releaseB := make(chan struct{}), make(chan struct{})
		var releaseAOnce, releaseBOnce sync.Once
		var created atomic.Int32
		aSettings := make(chan *PlatformTransportSettings, 1)
		settings := DefaultApiMultiClientGeneratorSettings()
		settings.MigrateConnectTimeout = time.Second
		state := &apiWindowClientTransport{
			current:  old,
			settings: DefaultPlatformTransportSettings(),
			auth:     ClientAuth{InstanceId: NewId()},
		}
		reports := &testMoveReports{}
		generator := &ApiMultiClientGenerator{
			settings:                      settings,
			platformTransportMode:         TransportModeH1,
			transports:                    map[*Client]*apiWindowClientTransport{client: state},
			sendResidentMoveResultForTest: reports.observe,
			newPlatformTransport: func(
				_ *Client,
				_ *ClientAuth,
				_ TransportMode,
				settings *PlatformTransportSettings,
			) apiWindowPlatformTransport {
				switch created.Add(1) {
				case 1:
					aSettings <- settings
					return a
				case 2:
					close(bEntered)
					<-releaseB
					return b
				default:
					return newFakeWindowPlatformTransport(false)
				}
			},
		}
		defer func() {
			releaseAOnce.Do(func() { close(a.release) })
			releaseBOnce.Do(func() { close(releaseB) })
			cancel()
			if err := generator.CloseTransportCreationAndWait(context.Background()); err != nil {
				t.Error(err)
			}
			if err := client.CloseAndWait(context.Background()); err != nil {
				t.Error(err)
			}
		}()
		var migratingAtClose bool
		a.onClose = func() {
			generator.transportLock.Lock()
			migratingAtClose = state.migrating
			generator.transportLock.Unlock()
		}
		var move *ResidentMove
		if failure == "ordinary-timeout" {
			generator.MigrateClientTransport(client, nil, time.Now())
		} else {
			if failure == "compulsory-timeout" {
				move = testCompulsoryMove(testMoveTargetIpv4)
			} else {
				move = testAdvisoryMove(testMoveTargetIpv4)
			}
			generator.MoveClientTransport(client, nil, move)
		}
		createdSettings := <-aSettings
		synctest.Wait()
		if failure == "target-failure" {
			if createdSettings.ResidentMoveDial == nil {
				t.Fatal("target failure did not enter the target MOVE path")
			}
			createdSettings.ResidentMoveDial.noteFailed(
				0, TransportModeH1, []netip.Addr{testMoveTargetIpv4},
				protocol.ResidentMoveReason_ResidentMoveReasonTlsError,
				250*time.Millisecond, time.Now(),
			)
		} else {
			if move != nil && createdSettings.ResidentMoveDial == nil {
				t.Fatal("target timeout did not enter the target MOVE path")
			}
			time.Sleep(settings.MigrateConnectTimeout)
		}
		<-a.joinEntered
		synctest.Wait()
		if migratingAtClose {
			t.Fatal("failed migration did not release before replacement close")
		}
		generator.transportLock.Lock()
		current, migrating := state.current, state.migrating
		generator.transportLock.Unlock()
		if current != old || migrating {
			t.Fatal("failed migration displaced the live carrier or retained its claim")
		}

		generator.MigrateClientTransport(client, nil, time.Now())
		<-bEntered
		synctest.Wait()
		generator.transportLock.Lock()
		migrating = state.migrating
		generator.transportLock.Unlock()
		if !migrating || created.Load() != 2 {
			t.Fatal("successor did not own its claim while the old worker's join was held")
		}
		releaseAOnce.Do(func() { close(a.release) })
		synctest.Wait()
		generator.transportCreation.mutex.Lock()
		active := generator.transportCreation.active
		generator.transportCreation.mutex.Unlock()
		if active != 1 {
			t.Fatalf("retiring worker did not finish while successor was held: active=%d", active)
		}
		generator.transportLock.Lock()
		migrating = state.migrating
		generator.transportLock.Unlock()
		if !migrating {
			t.Error("retiring migration erased successor's claim")
		}
		generator.MigrateClientTransport(client, nil, time.Now())
		synctest.Wait()
		if got := created.Load(); got != 2 {
			t.Fatalf("third migration constructed while successor remained in flight: created=%d", got)
		}
		select {
		case <-old.closed:
			t.Fatal("claim refusal closed the healthy current carrier")
		default:
		}
		if move != nil {
			results := reports.snapshot()
			wantReason := protocol.ResidentMoveReason_ResidentMoveReasonConnectTimeout
			if failure == "target-failure" {
				wantReason = protocol.ResidentMoveReason_ResidentMoveReasonTlsError
			}
			if len(results) != 1 || !bytes.Equal(results[0].MoveId, move.MoveId.Bytes()) ||
				results[0].Outcome != protocol.ResidentMoveOutcome_ResidentMoveOutcomeFailed ||
				results[0].Reason != wantReason {
				t.Fatalf("failed move report lost its exact owner or outcome: %+v", results)
			}
		}

		// The surviving successor can still connect, publish, and retire old.
		releaseBOnce.Do(func() { close(releaseB) })
		synctest.Wait()
		b.connect()
		synctest.Wait()
		generator.transportLock.Lock()
		current, migrating = state.current, state.migrating
		generator.transportLock.Unlock()
		if current != b || migrating || !b.IsConnected() {
			t.Fatal("successor could not finish after its predecessor retired")
		}
		select {
		case <-old.closed:
		default:
			t.Fatal("connected successor did not retire the old carrier")
		}
	})
}

// An ordinary connect timeout cannot erase a successor admitted during join.
func TestApiWindowMigrationTimeoutCannotReleaseSuccessor(t *testing.T) {
	requireApiMigrationSuccessorClaim(t, "ordinary-timeout")
}

// An explicit target failure has the same worker ownership as a timeout.
func TestApiWindowMoveFailureCannotReleaseSuccessor(t *testing.T) {
	requireApiMigrationSuccessorClaim(t, "target-failure")
}

// A strict target MOVE timeout must release once while keeping its report.
func TestApiWindowMoveTimeoutCannotReleaseSuccessor(t *testing.T) {
	requireApiMigrationSuccessorClaim(t, "target-timeout")
}

// A compulsory target MOVE timeout shares the same early release boundary.
func TestApiWindowCompulsoryMoveTimeoutCannotReleaseSuccessor(t *testing.T) {
	requireApiMigrationSuccessorClaim(t, "compulsory-timeout")
}

// A resident H1 handoff can still belong to an older replacement. Refusing a
// new replacement at that exact boundary must preserve both established H1
// carriers, return the refused claim, and allow a later successful migration.
func TestApiWindowH1MigrationBudgetRefusalPreservesLiveCarrier(t *testing.T) {
	assertMessagePoolOwnership(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	received := make(chan byte, 8)
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
			if len(packet) == 64 && packet[0] == 0xc1 && packet[1] == 0xa7 {
				select {
				case received <- packet[2]:
				case <-ctx.Done():
					return
				}
			}
		}
	}))
	defer func() { server.Close(); serverWorkers.Wait() }()
	platformUrl := "ws" + strings.TrimPrefix(server.URL, "http")
	strategySettings := DefaultClientStrategySettings()
	strategySettings.EnableResilient = false
	strategySettings.ParallelBlockSize = 1
	strategySettings.MinNextConnectDelay = 0
	strategySettings.MaxNextConnectDelay = 0
	strategy := NewClientStrategy(ctx, strategySettings)
	defer strategy.Close()
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), closeWaitClientSettings())
	siblingClient := NewClient(ctx, NewId(), NewNoContractClientOob(), closeWaitClientSettings())
	budget := NewPlatformTransportBudget(16*1024*1024, 2)
	settings := testingPlatformTransportSettings()
	settings.PlatformTransportBudget = budget
	oldRoutesRemoved := make(chan struct{})
	var oldRoutesRemovedOnce sync.Once
	oldSettings := *settings
	oldSettings.afterRoutesRemovedForTest = func() {
		oldRoutesRemovedOnce.Do(func() { close(oldRoutesRemoved) })
	}
	auth := ClientAuth{ByJwt: "local-h1-claim-fixture", InstanceId: NewId(), AppVersion: "test"}
	old := NewPlatformTransportWithTargetMode(client.Ctx(), strategy,
		client.RouteManager(), platformUrl, &auth, TransportModeH1, &oldSettings)
	sibling := NewPlatformTransportWithTargetMode(siblingClient.Ctx(), strategy,
		siblingClient.RouteManager(), platformUrl, &auth, TransportModeH1, settings)
	var held, next *PlatformTransport
	generatorSettings := DefaultApiMultiClientGeneratorSettings()
	generatorSettings.MigrateConnectTimeout = 5 * time.Second
	state := &apiWindowClientTransport{current: old, settings: settings, auth: auth}
	created := make(chan *PlatformTransport, 2)
	var blockReplacement atomic.Bool
	blockReplacement.Store(true)
	generator := &ApiMultiClientGenerator{
		settings:              generatorSettings,
		platformTransportMode: TransportModeH1,
		transports:            map[*Client]*apiWindowClientTransport{client: state},
		newPlatformTransport: func(
			_ *Client, auth *ClientAuth, _ TransportMode, effective *PlatformTransportSettings,
		) apiWindowPlatformTransport {
			value := *effective
			value.StartDisabled = blockReplacement.Load()
			replacement := NewPlatformTransportWithTargetMode(client.Ctx(), strategy,
				client.RouteManager(), platformUrl, auth, TransportModeH1, &value)
			created <- replacement
			return replacement
		},
	}
	defer func() {
		cancel()
		_ = generator.CloseTransportCreationAndWait(context.Background())
		if next != nil {
			_ = next.CloseAndWait(context.Background())
		}
		if held != nil {
			_ = held.CloseAndWait(context.Background())
		}
		_ = old.CloseAndWait(context.Background())
		_ = sibling.CloseAndWait(context.Background())
		_ = client.CloseAndWait(context.Background())
		_ = siblingClient.CloseAndWait(context.Background())
		if census := budget.MemoryOwnerCensus(); census.H1Count != 0 || census.H1Bytes != 0 {
			t.Errorf("joined H1 capacity refusal retained claims: %+v", census)
		}
	}()
	connected := func(transport *PlatformTransport) {
		t.Helper()
		for !transport.IsConnected() {
			notify := transport.ConnectedNotify()
			if transport.IsConnected() {
				break
			}
			waitCloseWaitBarrier(t, ctx, notify, "local H1 readiness")
		}
	}
	connected(old)
	connected(sibling)
	writer := client.RouteManager().OpenMultiRouteWriter(DestinationId(NewId()))
	siblingWriter := siblingClient.RouteManager().OpenMultiRouteWriter(DestinationId(NewId()))
	defer client.RouteManager().CloseMultiRouteWriter(writer)
	defer siblingClient.RouteManager().CloseMultiRouteWriter(siblingWriter)
	deliver := func(siblingPacket bool, marker byte) {
		t.Helper()
		packet := MessagePoolGet(64)
		clear(packet)
		packet[0], packet[1], packet[2] = 0xc1, 0xa7, marker
		selectedWriter := writer
		if siblingPacket {
			selectedWriter = siblingWriter
		}
		if err := selectedWriter.Write(ctx, packet, time.Second); err != nil {
			MessagePoolReturn(packet)
			t.Fatalf("established H1 write failed: %v", err)
		}
		select {
		case got := <-received:
			if got != marker {
				t.Fatalf("H1 payload marker=%d, want=%d", got, marker)
			}
		case <-ctx.Done():
			t.Fatal("established H1 payload did not reach the local server")
		}
	}
	deliver(false, 1)
	deliver(true, 2)

	// A real constructor owns this extra H1 claim. Holding it disabled keeps
	// the exact old->replacement budget loan live without opening a socket.
	heldSettings := *settings
	heldSettings.StartDisabled = true
	held = NewPlatformTransportWithTargetMode(client.Ctx(), strategy,
		client.RouteManager(), platformUrl, &auth, TransportModeH1, &heldSettings)
	if !held.CanMakeBeforeBreakFrom(old) || !held.h1BudgetReservation.Acquire(ctx) {
		t.Fatal("could not establish the real H1 bounded handoff")
	}
	if stats := budget.Stats(); stats.UsedTransportCount != 3 || stats.ActiveHandoffCount != 1 {
		t.Fatalf("expected two resident H1 carriers plus one bounded handoff: %+v", stats)
	}
	requireBoundedHandoff := func() {
		t.Helper()
		stats := budget.Stats()
		if stats.UsedTransportCount > stats.MaxTransportCount+stats.ActiveHandoffTransportCount ||
			stats.UsedByteCount > stats.TotalByteCount+stats.ActiveHandoffH1ByteCount ||
			stats.ActiveHandoffH1ByteCount <= 0 {
			t.Fatalf("H1 overlap escaped its exact slot or byte loan: %+v", stats)
		}
		budget.root.mutex.Lock()
		paired := old.h1BudgetReservation.handoffTo == held.h1BudgetReservation &&
			held.h1BudgetReservation.handoffFrom == old.h1BudgetReservation &&
			budget.activeHandoff == held.h1BudgetReservation
		budget.root.mutex.Unlock()
		if !paired {
			t.Fatal("H1 capacity refusal changed the older handoff's owner")
		}
	}
	requireBoundedHandoff()
	generator.MigrateClientTransport(client, nil, time.Now())
	select {
	case next = <-created:
	case <-ctx.Done():
		t.Fatal("H1 migration did not construct its budget-blocked replacement")
	}
	// The old carrier's removal and the rejected replacement's completion
	// distinguish the two branches. Neither outcome relies on a negative wait.
	select {
	case <-oldRoutesRemoved:
		t.Fatal("H1 capacity refusal closed the live carrier")
	case <-next.Done():
	case <-ctx.Done():
		t.Fatal("H1 capacity refusal did not resolve")
	}
	generator.transportCreation.mutex.Lock()
	idle := generator.transportCreation.idle
	generator.transportCreation.mutex.Unlock()
	waitCloseWaitBarrier(t, ctx, idle, "refused H1 migration completion")
	generator.transportLock.Lock()
	current, migrating := state.current, state.migrating
	generator.transportLock.Unlock()
	if current != old || migrating || !old.IsConnected() || !sibling.IsConnected() {
		t.Fatal("H1 capacity refusal disturbed the current carrier, sibling, or claim")
	}
	if stats := budget.Stats(); stats.UsedTransportCount != 3 ||
		stats.PendingH1Count != 0 || stats.ActiveHandoffCount != 1 {
		t.Fatalf("refused replacement did not return only its own reservation: %+v", stats)
	}
	requireBoundedHandoff()
	deliver(false, 3)
	deliver(true, 4)

	if err := held.CloseAndWait(ctx); err != nil {
		t.Fatal(err)
	}
	if stats := budget.Stats(); stats.UsedTransportCount != 2 || stats.ActiveHandoffCount != 0 {
		t.Fatalf("retired handoff did not restore resident capacity: %+v", stats)
	}
	blockReplacement.Store(false)
	generator.MigrateClientTransport(client, nil, time.Now())
	select {
	case next = <-created:
	case <-ctx.Done():
		t.Fatal("H1 migration did not recover after the older handoff retired")
	}
	generator.transportCreation.mutex.Lock()
	idle = generator.transportCreation.idle
	generator.transportCreation.mutex.Unlock()
	waitCloseWaitBarrier(t, ctx, idle, "recovered H1 migration completion")
	generator.transportLock.Lock()
	current, migrating = state.current, state.migrating
	generator.transportLock.Unlock()
	if current != next || migrating || !next.IsConnected() || !sibling.IsConnected() {
		t.Fatal("H1 migration did not recover with its healthy sibling intact")
	}
	select {
	case <-old.Done():
	default:
		t.Fatal("successful recovery did not join the old H1 carrier")
	}
	deliver(false, 5)
	deliver(true, 6)
}
