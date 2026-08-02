package connect

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

func waitForTestCondition(t *testing.T, timeout time.Duration, condition func() bool, message string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal(message)
}

// stallGate is the reusable fault-injection primitive for callback boundaries:
// park one consumer indefinitely, then assert whether its producer is supposed
// to remain blocked (transfer backpressure) or continue (maintenance/events).
type stallGate struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func newStallGate() *stallGate {
	return &stallGate{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (self *stallGate) Wait() {
	self.once.Do(func() { close(self.started) })
	<-self.release
}

func (self *stallGate) Release() {
	select {
	case <-self.release:
	default:
		close(self.release)
	}
}

func waitForStallStart(t *testing.T, gate *stallGate) {
	t.Helper()
	select {
	case <-gate.started:
	case <-time.After(time.Second):
		t.Fatal("injected stall did not start")
	}
}

func assertReturnsWithin(t *testing.T, timeout time.Duration, operation func(), message string) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		operation()
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		t.Fatal(message)
	}
}

func assertStillBlocked(t *testing.T, done <-chan struct{}, duration time.Duration, message string) {
	t.Helper()
	select {
	case <-done:
		t.Fatal(message)
	case <-time.After(duration):
	}
}

// A monitor callback used to run inline from window.resize. If it blocked
// (notably an iOS reverse RPC whose containing app had been suspended), resize
// could never remove or add another peer. The worker must isolate the publisher,
// coalesce while blocked, and bound unique terminal ids with a reset snapshot.
func TestMultiClientMonitorBlockedCallbackCannotBlockMaintenance(t *testing.T) {
	settings := DefaultRemoteUserNatMultiClientMonitorSettings()
	settings.CallbackPendingProviderEventMaxCount = 2
	monitor := NewRemoteUserNatMultiClientMonitor(settings)

	block := make(chan struct{})
	started := make(chan struct{})
	delivered := make(chan monitorCallbackEvent, 4)
	var startOnce sync.Once
	unsub := monitor.AddMonitorEventCallback(func(
		windowExpandEvent *WindowExpandEvent,
		providerEvents map[Id]*ProviderEvent,
		reset bool,
	) {
		startOnce.Do(func() { close(started) })
		<-block
		delivered <- monitorCallbackEvent{
			windowExpandEvent: cloneWindowExpandEvent(windowExpandEvent),
			providerEvents:    providerEvents,
			reset:             reset,
		}
	})
	defer unsub()

	id1 := NewId()
	id2 := NewId()
	id3 := NewId()
	id4 := NewId()
	id5 := NewId()
	monitor.AddProviderEvent(id1, ProviderStateInEvaluation)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("callback did not start")
	}

	// Every call below executes on the same path window.resize uses. None may
	// wait for the callback currently parked above.
	publishDone := make(chan struct{})
	go func() {
		defer close(publishDone)
		monitor.AddProviderEvent(id2, ProviderStateAdded)
		monitor.AddProviderEvent(id3, ProviderStateEvaluationFailed)
		monitor.AddProviderEvent(id4, ProviderStateNotAdded)
		monitor.AddProviderEvent(id1, ProviderStateRemoved)
		monitor.AddProviderEvent(id5, ProviderStateInEvaluation)
	}()
	select {
	case <-publishDone:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("blocked callback stalled monitor publisher/maintenance")
	}

	close(block)
	first := <-delivered
	if first.reset {
		t.Fatal("first differential event unexpectedly reset")
	}
	var coalesced monitorCallbackEvent
	select {
	case coalesced = <-delivered:
	case <-time.After(time.Second):
		t.Fatal("coalesced callback was not delivered")
	}
	if !coalesced.reset {
		t.Fatal("over-cap pending diffs were not collapsed to a bounded reset snapshot")
	}
	if len(coalesced.providerEvents) != 2 {
		t.Fatalf("reset snapshot has %d providers, want 2", len(coalesced.providerEvents))
	}
	if event := coalesced.providerEvents[id2]; event == nil || event.State != ProviderStateAdded {
		t.Fatal("reset snapshot lost active provider id2")
	}
	if event := coalesced.providerEvents[id5]; event == nil || event.State != ProviderStateInEvaluation {
		t.Fatal("reset snapshot lost active provider id5")
	}
	for _, terminalId := range []Id{id1, id3, id4} {
		if _, ok := coalesced.providerEvents[terminalId]; ok {
			t.Fatalf("reset snapshot retained terminal provider %s", terminalId)
		}
	}
}

func TestMultiClientMonitorCallbackPanicIsListenerLocal(t *testing.T) {
	monitor := NewRemoteUserNatMultiClientMonitorWithDefaults()
	var calls atomic.Int32
	unsub := monitor.AddMonitorEventCallback(func(
		_ *WindowExpandEvent,
		_ map[Id]*ProviderEvent,
		_ bool,
	) {
		if calls.Add(1) == 1 {
			panic("listener failure")
		}
	})
	defer unsub()

	monitor.AddProviderEvent(NewId(), ProviderStateAdded)
	waitForTestCondition(t, time.Second, func() bool {
		return calls.Load() == 1
	}, "first callback did not run")

	monitor.AddProviderEvent(NewId(), ProviderStateAdded)
	waitForTestCondition(t, time.Second, func() bool {
		return 2 <= calls.Load()
	}, "callback worker died after listener panic")
}

func TestMergedMultiClientMonitorResetIncludesEveryWindow(t *testing.T) {
	first := NewRemoteUserNatMultiClientMonitorWithDefaults()
	second := NewRemoteUserNatMultiClientMonitorWithDefaults()
	merged := NewMergedMultiClientMonitor([]MultiClientMonitor{first, second})
	gate := newStallGate()
	delivered := make(chan monitorCallbackEvent, 4)
	unsub := merged.AddMonitorEventCallback(func(
		windowExpandEvent *WindowExpandEvent,
		providerEvents map[Id]*ProviderEvent,
		reset bool,
	) {
		gate.Wait()
		delivered <- monitorCallbackEvent{
			windowExpandEvent: cloneWindowExpandEvent(windowExpandEvent),
			providerEvents:    providerEvents,
			reset:             reset,
		}
	})
	defer func() {
		gate.Release()
		unsub()
	}()

	firstId := NewId()
	secondId := NewId()
	first.AddProviderEvent(firstId, ProviderStateAdded)
	waitForStallStart(t, gate)
	second.AddProviderEvent(secondId, ProviderStateAdded)
	// Inject the bounded reset that an underlying listener produces on
	// overflow. Call its merge adapter directly so the test does not depend
	// on scheduler timing between two asynchronous callback workers.
	underlyingWorkers := first.monitorEventCallbacks.Get()
	if len(underlyingWorkers) != 1 {
		t.Fatalf("underlying merged callback workers = %d, want 1", len(underlyingWorkers))
	}
	firstWindow, firstProviders := first.Events()
	underlyingWorkers[0].callback(firstWindow, firstProviders, true)

	gate.Release()
	var resetEvent monitorCallbackEvent
	deadline := time.After(time.Second)
	for !resetEvent.reset {
		select {
		case resetEvent = <-delivered:
		case <-deadline:
			t.Fatal("merged reset event was not delivered")
		}
	}
	for _, id := range []Id{firstId, secondId} {
		event := resetEvent.providerEvents[id]
		if event == nil || event.State != ProviderStateAdded {
			t.Fatalf("merged reset lost live provider %s", id)
		}
	}
}

// End-to-end guard for the original failure: park the merged monitor observer
// while providers repeatedly go dark. The underlying window state must still
// replace client ids, proving resize/evaluation no longer runs through that
// observer.
func TestMultiClientPeerReplacementContinuesWhileMonitorObserverStalls(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings := DefaultMultiClientSettings()
	settings.WindowSizes[WindowTypeQuality] = WindowSizeSettings{
		WindowSizeMin:     1,
		WindowSizeMax:     1,
		WindowSizeHardMax: 2,
		FixedWindowSize:   1,
	}
	settings.WindowSizes[WindowTypeSpeed] = WindowSizeSettings{
		WindowSizeMin:     1,
		WindowSizeMax:     1,
		WindowSizeHardMax: 2,
		FixedWindowSize:   1,
	}
	settings.PingWriteTimeout = 200 * time.Millisecond
	settings.PingTimeout = 500 * time.Millisecond
	settings.WindowResizeTimeout = 100 * time.Millisecond
	settings.WindowExpandTimeout = 500 * time.Millisecond
	settings.WindowEnumerateErrorTimeout = 25 * time.Millisecond
	settings.StatsWindowMaxUnhealthyDuration = 500 * time.Millisecond
	settings.StatsWindowWarnUnhealthyDuration = 250 * time.Millisecond
	settings.StatsWindowKeepUnhealthyDuration = time.Second
	settings.StatsWindowGraceperiod = 500 * time.Millisecond
	settings.AckTimeout = 500 * time.Millisecond
	settings.BlackholeTimeout = 500 * time.Millisecond
	settings.BlackholeConnectTimeout = time.Second
	settings.CPingWriteTimeout = 100 * time.Millisecond
	settings.CPingTimeout = 250 * time.Millisecond
	settings.CPingRestTimeout = 100 * time.Millisecond

	generator := newFlappingProviderGenerator(ctx)
	defer generator.close()
	multi := NewRemoteUserNatMultiClient(
		ctx,
		generator,
		func(TransferPath, protocol.ProvideMode, *IpPath, []byte) {},
		protocol.ProvideMode_Public,
		settings,
	)
	defer multi.Close()

	gate := newStallGate()
	unsub := multi.Monitor().AddMonitorEventCallback(func(
		*WindowExpandEvent,
		map[Id]*ProviderEvent,
		bool,
	) {
		gate.Wait()
	})
	defer func() {
		gate.Release()
		unsub()
	}()
	waitForStallStart(t, gate)

	activeIds := func() map[Id]bool {
		_, events := multi.Monitor().Events()
		ids := map[Id]bool{}
		for id, event := range events {
			if event != nil && event.State == ProviderStateAdded {
				ids[id] = true
			}
		}
		return ids
	}
	var initial map[Id]bool
	waitForTestCondition(t, 3*time.Second, func() bool {
		initial = activeIds()
		return 0 < len(initial)
	}, "window never formed its initial peers")

	waitForTestCondition(t, 8*time.Second, func() bool {
		for id := range activeIds() {
			if !initial[id] {
				return true
			}
		}
		return false
	}, "peer replacement stopped while monitor observer was stalled")
}

// Removal-generated TCP resets are advisory. If the downstream packet/TUN
// receiver blocks, enqueue must remain non-blocking and retained packets must
// stay fixed at the configured queue capacity.
func TestMultiClientRemovalReceiveIsBoundedAndNonBlocking(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	started := make(chan struct{})
	block := make(chan struct{})
	var startOnce sync.Once
	multi := &RemoteUserNatMultiClient{
		ctx:                 ctx,
		log:                 NewNoopLogger(),
		removalReceiveQueue: make(chan receivePacket, 1),
		receivePacketCallback: func(
			TransferPath,
			protocol.ProvideMode,
			*IpPath,
			[]byte,
		) {
			startOnce.Do(func() { close(started) })
			<-block
		},
	}
	go multi.runRemovalReceive()

	multi.enqueueRemovalReceive(&receivePacket{Packet: []byte{1}})
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("removal receive worker did not start")
	}

	begin := time.Now()
	for range 1024 {
		multi.enqueueRemovalReceive(&receivePacket{Packet: []byte{2}})
	}
	if elapsed := time.Since(begin); 250*time.Millisecond < elapsed {
		t.Fatalf("bounded enqueue blocked for %s", elapsed)
	}
	if got := len(multi.removalReceiveQueue); got != cap(multi.removalReceiveQueue) {
		t.Fatalf("queue retained %d packets, want fixed capacity %d", got, cap(multi.removalReceiveQueue))
	}
	if multi.removalReceiveDropCount.Load() == 0 {
		t.Fatal("full bounded queue did not account for dropped advisory packets")
	}
	close(block)
}

// Exercise the real teardown path, not only its queue primitive. Clearing
// every flow association is mandatory, but reset construction and retention
// must be bounded by queue capacity even for a large flow table.
func TestMultiClientRemovalClearsAllFlowsWithBoundedResetWork(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	clientCtx, cancelClient := context.WithCancel(ctx)
	cancelClient()
	client := &multiClientChannel{ctx: clientCtx}
	multi := &RemoteUserNatMultiClient{
		ctx:                 ctx,
		log:                 NewNoopLogger(),
		removalReceiveQueue: make(chan receivePacket, 2),
		clientUpdates:       map[*multiClientChannel]map[*multiClientChannelUpdate]bool{},
	}
	updates := make(map[*multiClientChannelUpdate]bool, 4096)
	for i := range 4096 {
		update := &multiClientChannelUpdate{
			ipPath: &IpPath{
				Version:         4,
				Protocol:        IpProtocolTcp,
				SourceIp:        []byte{10, 0, byte(i >> 8), byte(i)},
				SourcePort:      10_000 + i,
				DestinationIp:   []byte{192, 0, 2, 1},
				DestinationPort: 443,
			},
		}
		update.client.Store(client)
		updates[update] = true
	}
	multi.clientUpdates[client] = updates

	assertReturnsWithin(t, 250*time.Millisecond, func() {
		multi.removeClient(client)
	}, "large client teardown stalled maintenance")

	if _, ok := multi.clientUpdates[client]; ok {
		t.Fatal("removed client retained its flow set")
	}
	for update := range updates {
		if update.client.Load() != nil {
			t.Fatal("client teardown did not clear every flow association")
		}
	}
	if got := len(multi.removalReceiveQueue); got != 2 {
		t.Fatalf("retained reset count = %d, want queue capacity 2", got)
	}
	if got := multi.removalReceiveDropCount.Load(); got != uint64(len(updates)-2) {
		t.Fatalf("accounted reset drops = %d, want %d", got, len(updates)-2)
	}
}

// Contract status is observer state, not transfer callback backpressure. A
// blocked observer must neither park HandleControlFrame nor grow a status queue
// without bound; latest state per key is retained.
func TestContractStatusObserverStallIsBoundedAndNonBlocking(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	gate := newStallGate()
	delivered := make(chan *ContractStatus, 8)
	worker := newContractStatusCallbackWorker(ctx, func(status *ContractStatus) {
		gate.Wait()
		delivered <- status
	}, 3)
	defer worker.Close()

	worker.Dispatch(&ContractStatus{Key: ContractKey{Destination: DestinationId(NewId())}})
	waitForStallStart(t, gate)

	var latestKey ContractKey
	assertReturnsWithin(t, 250*time.Millisecond, func() {
		for range 128 {
			latestKey = ContractKey{Destination: DestinationId(NewId())}
			worker.Dispatch(&ContractStatus{Key: latestKey})
		}
		worker.Dispatch(&ContractStatus{Key: latestKey, Premium: true})
	}, "blocked contract-status observer stalled its control-plane producer")

	worker.stateLock.Lock()
	pendingCount := len(worker.pending)
	orderCount := worker.orderCount
	latest := worker.pending[latestKey]
	worker.stateLock.Unlock()
	if pendingCount > 3 || orderCount > 3 {
		t.Fatalf("status backlog exceeded bound: pending=%d order=%d", pendingCount, orderCount)
	}
	if latest == nil || !latest.Premium {
		t.Fatal("status coalescing did not retain the latest value for a contract key")
	}
	gate.Release()
}

func TestContractStatsObserverStallIsBoundedAndCoalesced(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	gate := newStallGate()
	worker := newContractStatsCallbackWorker(ctx, func([]*ContractStatsEvent) {
		gate.Wait()
	}, 2)
	defer worker.Close()

	firstId := NewId()
	worker.Dispatch([]*ContractStatsEvent{{
		ContractId:         firstId,
		UsedByteCountDelta: 1,
		Open:               true,
		Sequence:           1,
	}})
	waitForStallStart(t, gate)

	latestId := NewId()
	assertReturnsWithin(t, 250*time.Millisecond, func() {
		for range 128 {
			worker.Dispatch([]*ContractStatsEvent{{
				ContractId: NewId(),
				Open:       true,
				Sequence:   1,
			}})
		}
		worker.Dispatch([]*ContractStatsEvent{{
			ContractId:         latestId,
			UsedByteCount:      10,
			UsedByteCountDelta: 4,
			Open:               true,
			Sequence:           1,
		}})
		worker.Dispatch([]*ContractStatsEvent{{
			ContractId:         latestId,
			UsedByteCount:      15,
			UsedByteCountDelta: 5,
			Open:               false,
			Sequence:           2,
		}})
	}, "blocked contract-stats observer stalled cleanup/stats production")

	worker.stateLock.Lock()
	pendingCount := len(worker.pending)
	orderCount := worker.orderCount
	latest := worker.pending[contractStatsKey{contractId: latestId}]
	worker.stateLock.Unlock()
	if pendingCount > 2 || orderCount > 2 {
		t.Fatalf("stats backlog exceeded bound: pending=%d order=%d", pendingCount, orderCount)
	}
	if latest == nil || latest.Open || latest.Sequence != 2 ||
		latest.UsedByteCount != 15 || latest.UsedByteCountDelta != 9 {
		t.Fatalf("stats coalescing lost latest/accumulated state: %+v", latest)
	}
	gate.Release()
}

func TestPeerIdentityObserverStallCoalesces(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	gate := newStallGate()
	var calls atomic.Int32
	worker := newCoalescingCallbackWorker(ctx, func() {
		calls.Add(1)
		gate.Wait()
	})
	defer worker.Close()

	worker.Dispatch()
	waitForStallStart(t, gate)
	assertReturnsWithin(t, 250*time.Millisecond, func() {
		for range 1024 {
			worker.Dispatch()
		}
	}, "blocked identity observer stalled its producer")
	if got := len(worker.notify); got != 1 {
		t.Fatalf("coalesced identity backlog = %d, want 1", got)
	}
	gate.Release()
	waitForTestCondition(t, time.Second, func() bool {
		return calls.Load() == 2
	}, "coalesced identity change was not delivered")
}

type stalledLoadIdentityStore struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
	result  []*WindowClientIdentity
}

func (self *stalledLoadIdentityStore) LoadWindowClientIdentities() []*WindowClientIdentity {
	self.once.Do(func() { close(self.started) })
	<-self.release
	return self.result
}

func (*stalledLoadIdentityStore) StoreWindowClientIdentities([]*WindowClientIdentity) {
}

type contextLoadIdentityStore struct {
	started  chan struct{}
	canceled chan struct{}
	once     sync.Once
}

func (*contextLoadIdentityStore) LoadWindowClientIdentities() []*WindowClientIdentity {
	panic("legacy identity load used despite context capability")
}

func (*contextLoadIdentityStore) StoreWindowClientIdentities([]*WindowClientIdentity) {
}

func (self *contextLoadIdentityStore) LoadWindowClientIdentitiesContext(ctx context.Context) []*WindowClientIdentity {
	self.once.Do(func() { close(self.started) })
	<-ctx.Done()
	close(self.canceled)
	return nil
}

func TestWindowIdentityLoadStallCannotStopDiscovery(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lateDestination := RequireMultiHopId(NewId())
	store := &stalledLoadIdentityStore{
		started: make(chan struct{}),
		release: make(chan struct{}),
		result: []*WindowClientIdentity{{
			ClientId:    NewId(),
			Destination: lateDestination,
		}},
	}
	state := newWindowIdentityState(ctx, store)

	loadCtx, loadCancel := context.WithTimeout(ctx, 25*time.Millisecond)
	defer loadCancel()
	start := time.Now()
	_, err := state.RestoredDestinationsContext(loadCtx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stalled identity load error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(start); 250*time.Millisecond < elapsed {
		t.Fatalf("identity load deadline took %s", elapsed)
	}

	// Once continuity restoration misses its budget, later discovery calls
	// proceed immediately with fresh identities. Record also stays independent
	// of the still-stuck loader because no external call owns state.mutex.
	assertReturnsWithin(t, 250*time.Millisecond, func() {
		destinations, secondErr := state.RestoredDestinationsContext(ctx)
		if secondErr != nil || len(destinations) != 0 {
			t.Errorf("abandoned identity load = %v, %v; want empty success", destinations, secondErr)
		}
		state.Record(&WindowClientIdentity{
			ClientId:    NewId(),
			Destination: RequireMultiHopId(NewId()),
		})
	}, "abandoned identity load continued to block discovery/state mutation")

	close(store.release)
	waitForTestCondition(t, time.Second, func() bool {
		state.mutex.Lock()
		defer state.mutex.Unlock()
		return state.loadFinished
	}, "late identity load did not finish")
	destinations, err := state.RestoredDestinationsContext(ctx)
	if err != nil || len(destinations) != 0 {
		t.Fatalf("late abandoned identities were resurrected: %v, %v", destinations, err)
	}
}

func TestWindowIdentityDeadlineCancelsContextAwareStore(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := &contextLoadIdentityStore{
		started:  make(chan struct{}),
		canceled: make(chan struct{}),
	}
	state := newWindowIdentityState(ctx, store)
	select {
	case <-store.started:
	case <-time.After(time.Second):
		t.Fatal("context-aware identity load did not start")
	}

	loadCtx, cancelLoad := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancelLoad()
	if _, err := state.RestoredDestinationsContext(loadCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("identity load error = %v, want deadline exceeded", err)
	}
	select {
	case <-store.canceled:
	case <-time.After(time.Second):
		t.Fatal("identity deadline abandoned result but did not cancel underlying store request")
	}
}

func TestWindowIdentityLoadWarmsBeforeDiscovery(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := &stalledLoadIdentityStore{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	newWindowIdentityState(ctx, store)
	select {
	case <-store.started:
	case <-time.After(time.Second):
		t.Fatal("identity load did not start until first discovery call")
	}
	close(store.release)
}

// A persisted identity is an optimization. A slow Redis/disk adapter must not
// suppress an already-known fixed provider or force a second enumerate retry.
func TestOptionalIdentityLoadCannotSuppressFixedDestination(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := &stalledLoadIdentityStore{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	defer close(store.release)
	fixedId := NewId()
	generator := &ApiMultiClientGenerator{
		ctx:      ctx,
		specs:    []*ProviderSpec{{ClientId: &fixedId}},
		settings: &ApiMultiClientGeneratorSettings{IdentityLoadTimeout: 20 * time.Millisecond},
		identityState: newWindowIdentityState(
			ctx,
			store,
		),
	}

	start := time.Now()
	destinations, err := generator.NextDestinationsContext(ctx, 1, nil, "quality")
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); 250*time.Millisecond < elapsed {
		t.Fatalf("optional identity timeout delayed fixed discovery for %s", elapsed)
	}
	fixedDestination := RequireMultiHopId(fixedId)
	if _, ok := destinations[fixedDestination]; !ok {
		t.Fatal("optional identity timeout suppressed known fixed destination")
	}
}

// This is the explicit boundary guard requested for the packet path: transfer
// Client send-ack, receive, and forward callbacks intentionally apply
// backpressure. Future stall hardening must not make these callbacks lossy or
// asynchronous.
func TestTransferCallbacksPreserveIntentionalBackpressure(t *testing.T) {
	run := func(t *testing.T, invoke func(*stallGate)) {
		t.Helper()
		gate := newStallGate()
		done := make(chan struct{})
		go func() {
			defer close(done)
			invoke(gate)
		}()
		waitForStallStart(t, gate)
		assertStillBlocked(t, done, 25*time.Millisecond, "transfer callback did not apply backpressure")
		gate.Release()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("transfer callback did not resume after backpressure released")
		}
	}

	t.Run("send-ack", func(t *testing.T) {
		run(t, func(gate *stallGate) {
			safeAck(func(error) { gate.Wait() }, nil)
		})
	})
	t.Run("receive", func(t *testing.T) {
		client := &Client{
			log:              DefaultLogger(),
			receiveCallbacks: NewCallbackList[ReceiveFunction](),
		}
		run(t, func(gate *stallGate) {
			client.AddReceiveCallback(func(TransferPath, []*protocol.Frame, Peer) {
				gate.Wait()
			})
			client.receive(TransferPath{}, nil, Peer{})
		})
	})
	t.Run("forward", func(t *testing.T) {
		client := &Client{
			log:              DefaultLogger(),
			forwardCallbacks: NewCallbackList[ForwardFunction](),
		}
		run(t, func(gate *stallGate) {
			client.AddForwardCallback(func(TransferPath, []byte) {
				gate.Wait()
			})
			client.forward(TransferPath{}, nil)
		})
	})
	t.Run("cancel-does-not-bypass-receive", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		client := NewClient(
			ctx,
			NewId(),
			NewNoContractClientOob(),
			DefaultClientSettings(),
		)
		defer client.Close()
		gate := newStallGate()
		done := make(chan struct{})
		unsub := client.AddReceiveCallback(func(TransferPath, []*protocol.Frame, Peer) {
			gate.Wait()
		})
		defer unsub()
		go func() {
			defer close(done)
			client.receive(TransferPath{}, nil, Peer{})
		}()
		waitForStallStart(t, gate)

		assertReturnsWithin(
			t,
			250*time.Millisecond,
			client.Cancel,
			"client cancellation waited for intentional receive backpressure",
		)
		assertStillBlocked(
			t,
			done,
			25*time.Millisecond,
			"cancellation incorrectly bypassed an in-flight receive callback",
		)
		gate.Release()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("receive callback did not return after release")
		}
	})
}

type cleanupOrderTestGenerator struct {
	client       *Client
	removeOnce   sync.Once
	removeCalled chan struct{}
}

func (*cleanupOrderTestGenerator) NextDestinations(int, []MultiHopId, string) (map[MultiHopId]DestinationStats, error) {
	return nil, nil
}

func (*cleanupOrderTestGenerator) NewClientArgs() (*MultiClientGeneratorClientArgs, error) {
	return &MultiClientGeneratorClientArgs{ClientId: NewId()}, nil
}

func (*cleanupOrderTestGenerator) RemoveClientArgs(*MultiClientGeneratorClientArgs) {}

func (self *cleanupOrderTestGenerator) RemoveClientWithArgs(*Client, *MultiClientGeneratorClientArgs) {
	self.removeOnce.Do(func() { close(self.removeCalled) })
}

func (*cleanupOrderTestGenerator) NewClientSettings() *ClientSettings {
	settings := DefaultClientSettings()
	settings.ControlPingTimeout = 0
	return settings
}

func (self *cleanupOrderTestGenerator) NewClient(
	ctx context.Context,
	args *MultiClientGeneratorClientArgs,
	settings *ClientSettings,
) (*Client, error) {
	self.client = NewClient(ctx, args.ClientId, NewNoContractClientOob(), settings)
	return self.client, nil
}

func (*cleanupOrderTestGenerator) FixedDestinationSize() (int, bool) {
	return 1, true
}

// A stats observer can remain parked after app suspension. Essential client,
// transport, and generator cleanup must happen before observer-only close
// events, otherwise every peer churn retains another transport/client record.
func TestMultiClientCleanupPrecedesBlockedObservers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	generator := &cleanupOrderTestGenerator{removeCalled: make(chan struct{})}
	settings := DefaultMultiClientSettings()
	settings.CPingRestTimeout = time.Hour
	settings.BlackholeTimeout = time.Hour
	statsGate := newStallGate()

	channel, err := newMultiClientChannel(
		ctx,
		&multiClientChannelArgs{
			MultiClientGeneratorClientArgs: MultiClientGeneratorClientArgs{ClientId: NewId()},
			Destination:                    RequireMultiHopId(NewId()),
		},
		generator,
		func(*multiClientChannel, TransferPath, protocol.ProvideMode, *IpPath, []byte) {},
		DefaultSecurityPolicy(ctx),
		func(*ContractStatus) {},
		func([]*ContractStatsEvent) { statsGate.Wait() },
		func() {},
		nil,
		settings,
	)
	if err != nil {
		t.Fatal(err)
	}

	entry := generator.client.ContractManager().registerContractStats(
		NewId(),
		false,
		false,
		TransferPath{},
		100,
	)
	entry.updateUsedByteCount(10)
	channel.Cancel()

	select {
	case <-generator.removeCalled:
	case <-time.After(time.Second):
		t.Fatal("blocked observer retained essential generator/client resources")
	}
	waitForStallStart(t, statsGate)
	statsGate.Release()
}

type maintenanceContextTestGenerator struct{}

func (*maintenanceContextTestGenerator) NextDestinations(int, []MultiHopId, string) (map[MultiHopId]DestinationStats, error) {
	panic("legacy unbounded discovery path used")
}

func (*maintenanceContextTestGenerator) NewClientArgs() (*MultiClientGeneratorClientArgs, error) {
	panic("legacy unbounded auth path used")
}

func (*maintenanceContextTestGenerator) RemoveClientArgs(*MultiClientGeneratorClientArgs) {}

func (*maintenanceContextTestGenerator) RemoveClientWithArgs(*Client, *MultiClientGeneratorClientArgs) {
}

func (*maintenanceContextTestGenerator) NewClientSettings() *ClientSettings {
	return DefaultClientSettings()
}

func (*maintenanceContextTestGenerator) NewClient(context.Context, *MultiClientGeneratorClientArgs, *ClientSettings) (*Client, error) {
	panic("legacy unbounded client setup path used")
}

func (*maintenanceContextTestGenerator) FixedDestinationSize() (int, bool) {
	return 1, true
}

func (*maintenanceContextTestGenerator) NextDestinationsContext(
	ctx context.Context,
	_ int,
	_ []MultiHopId,
	_ string,
) (map[MultiHopId]DestinationStats, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (*maintenanceContextTestGenerator) NewClientArgsContext(ctx context.Context) (*MultiClientGeneratorClientArgs, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (*maintenanceContextTestGenerator) NewClientContext(
	_ context.Context,
	callCtx context.Context,
	_ *MultiClientGeneratorClientArgs,
	_ *ClientSettings,
) (*Client, error) {
	<-callCtx.Done()
	return nil, callCtx.Err()
}

func TestMultiClientGeneratorMaintenanceDeadlines(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings := DefaultMultiClientSettings()
	settings.WindowGeneratorTimeout = 25 * time.Millisecond
	settings.WindowClientCreateTimeout = 25 * time.Millisecond
	generator := &maintenanceContextTestGenerator{}
	window := &multiClientWindow{
		ctx:       ctx,
		generator: generator,
		settings:  settings,
	}

	start := time.Now()
	_, err := window.nextDestinations(1, nil, "quality")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("discovery error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(start); 250*time.Millisecond < elapsed {
		t.Fatalf("discovery deadline took %s", elapsed)
	}

	start = time.Now()
	_, err = window.newClientArgs(RequireMultiHopId(NewId()))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("auth error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(start); 250*time.Millisecond < elapsed {
		t.Fatalf("auth deadline took %s", elapsed)
	}

	start = time.Now()
	_, err = newMultiClientChannel(
		ctx,
		&multiClientChannelArgs{
			MultiClientGeneratorClientArgs: MultiClientGeneratorClientArgs{ClientId: NewId()},
			Destination:                    RequireMultiHopId(NewId()),
		},
		generator,
		func(*multiClientChannel, TransferPath, protocol.ProvideMode, *IpPath, []byte) {},
		DefaultSecurityPolicy(ctx),
		func(*ContractStatus) {},
		func([]*ContractStatsEvent) {},
		func() {},
		nil,
		settings,
	)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("client setup error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(start); 250*time.Millisecond < elapsed {
		t.Fatalf("client setup deadline took %s", elapsed)
	}
}

type successfulMaintenanceGenerator struct {
	maintenanceContextTestGenerator
	client           *Client
	sawSetupDeadline atomic.Bool
}

func (self *successfulMaintenanceGenerator) NewClientContext(
	ctx context.Context,
	callCtx context.Context,
	args *MultiClientGeneratorClientArgs,
	settings *ClientSettings,
) (*Client, error) {
	_, hasDeadline := callCtx.Deadline()
	self.sawSetupDeadline.Store(hasDeadline)
	self.client = NewClient(ctx, args.ClientId, NewNoContractClientOob(), settings)
	return self.client, nil
}

// The setup deadline must bound only construction. Parenting the returned
// client to callCtx is an easy fix for a setup stall that silently kills every
// healthy peer when the setup timer expires later.
func TestMultiClientSetupDeadlineDoesNotOwnClientLifetime(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings := DefaultMultiClientSettings()
	settings.WindowClientCreateTimeout = 20 * time.Millisecond
	settings.CPingRestTimeout = time.Hour
	settings.BlackholeTimeout = time.Hour
	generator := &successfulMaintenanceGenerator{}

	channel, err := newMultiClientChannel(
		ctx,
		&multiClientChannelArgs{
			MultiClientGeneratorClientArgs: MultiClientGeneratorClientArgs{ClientId: NewId()},
			Destination:                    RequireMultiHopId(NewId()),
		},
		generator,
		func(*multiClientChannel, TransferPath, protocol.ProvideMode, *IpPath, []byte) {},
		DefaultSecurityPolicy(ctx),
		func(*ContractStatus) {},
		func([]*ContractStatsEvent) {},
		func() {},
		nil,
		settings,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer channel.Close()
	if !generator.sawSetupDeadline.Load() {
		t.Fatal("client setup did not receive its bounded call context")
	}

	time.Sleep(2 * settings.WindowClientCreateTimeout)
	select {
	case <-channel.Done():
		t.Fatal("successful client inherited the expired setup deadline")
	case <-generator.client.Done():
		t.Fatal("generator client inherited the expired setup deadline")
	default:
	}
}

type recoveringMaintenanceGenerator struct {
	destination MultiHopId
	discoveries atomic.Int32
	auths       atomic.Int32
}

func (*recoveringMaintenanceGenerator) NextDestinations(int, []MultiHopId, string) (map[MultiHopId]DestinationStats, error) {
	panic("legacy discovery path used")
}

func (*recoveringMaintenanceGenerator) NewClientArgs() (*MultiClientGeneratorClientArgs, error) {
	panic("legacy auth path used")
}

func (*recoveringMaintenanceGenerator) RemoveClientArgs(*MultiClientGeneratorClientArgs) {}

func (*recoveringMaintenanceGenerator) RemoveClientWithArgs(*Client, *MultiClientGeneratorClientArgs) {
}

func (*recoveringMaintenanceGenerator) NewClientSettings() *ClientSettings {
	return DefaultClientSettings()
}

func (*recoveringMaintenanceGenerator) NewClient(context.Context, *MultiClientGeneratorClientArgs, *ClientSettings) (*Client, error) {
	panic("legacy client path used")
}

func (*recoveringMaintenanceGenerator) FixedDestinationSize() (int, bool) {
	return 1, true
}

func (self *recoveringMaintenanceGenerator) NextDestinationsContext(
	ctx context.Context,
	_ int,
	_ []MultiHopId,
	_ string,
) (map[MultiHopId]DestinationStats, error) {
	if self.discoveries.Add(1) == 1 {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return map[MultiHopId]DestinationStats{self.destination: {}}, nil
}

func (self *recoveringMaintenanceGenerator) NewClientArgsContext(ctx context.Context) (*MultiClientGeneratorClientArgs, error) {
	if self.auths.Add(1) == 1 {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return &MultiClientGeneratorClientArgs{ClientId: NewId()}, nil
}

func (*recoveringMaintenanceGenerator) NewClientContext(
	context.Context,
	context.Context,
	*MultiClientGeneratorClientArgs,
	*ClientSettings,
) (*Client, error) {
	panic("client creation not used")
}

// A timeout must end only the attempt, not the sole enumerator. This catches
// the subtle failure where adding a deadline merely made the producer return
// permanently (closing clientChannelArgs and leaving resize unable to recover).
func TestMultiClientEnumeratorRetriesAfterStalledGeneratorCalls(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings := DefaultMultiClientSettings()
	settings.WindowGeneratorTimeout = 20 * time.Millisecond
	settings.WindowEnumerateErrorTimeout = time.Millisecond
	settings.WindowExpandArgsTimeout = time.Second
	generator := &recoveringMaintenanceGenerator{
		destination: RequireMultiHopId(NewId()),
	}
	window := &multiClientWindow{
		ctx:               ctx,
		log:               DefaultLogger(),
		generator:         generator,
		windowType:        WindowTypeQuality,
		settings:          settings,
		clientChannelArgs: make(chan *multiClientChannelArgs),
		clients:           map[Id]*multiClientChannel{},
		generatorMonitor:  NewMonitor(),
	}
	go window.randomEnumerateClientArgs()

	select {
	case args := <-window.clientChannelArgs:
		if args == nil || args.Destination != generator.destination {
			t.Fatalf("unexpected recovered args: %+v", args)
		}
	case <-time.After(time.Second):
		t.Fatal("enumerator did not recover after discovery/auth deadlines")
	}
	if generator.discoveries.Load() < 2 || generator.auths.Load() < 2 {
		t.Fatalf(
			"enumerator did not retry both stalled calls: discovery=%d auth=%d",
			generator.discoveries.Load(),
			generator.auths.Load(),
		)
	}
}

func TestSchedulerPauseDetectionDoesNotConfuseOrdinaryTimerDelay(t *testing.T) {
	expected := 100 * time.Millisecond
	tolerance := 50 * time.Millisecond
	if schedulerPauseDetected(time.Now(), expected, tolerance) {
		t.Fatal("fresh timer was classified as a scheduler pause")
	}
	if !schedulerPauseDetected(time.Now().Add(-time.Second), expected, tolerance) {
		t.Fatal("large scheduler gap was classified as an ordinary peer timeout")
	}
	if schedulerPauseDetected(time.Now().Add(-time.Second), expected, 0) {
		t.Fatal("disabled scheduler-pause detection still classified a pause")
	}
}
