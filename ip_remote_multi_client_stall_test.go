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

// callbackPanicTestLogger captures the expected recovery report without
// emitting it through the process-wide glog sink used by test runners.
type callbackPanicTestLogger struct {
	Logger
	warned chan struct{}
	once   sync.Once
}

func (self *callbackPanicTestLogger) Warningf(string, ...any) {
	self.once.Do(func() { close(self.warned) })
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
	previousLogger := DefaultLogger()
	panicLogger := &callbackPanicTestLogger{
		Logger: NewNoopLogger(),
		warned: make(chan struct{}),
	}
	SetDefaultLogger(panicLogger)
	defer SetDefaultLogger(previousLogger)

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
	select {
	case <-panicLogger.warned:
	case <-time.After(time.Second):
		t.Fatal("listener panic was isolated but not reported")
	}

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
	}
	multi.receivePacketCallback.Store(&receivePacketCallbackHolder{callback: func(
		TransferPath,
		protocol.ProvideMode,
		*IpPath,
		[]byte,
	) {
		startOnce.Do(func() { close(started) })
		<-block
	}})
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

type closeDetachesServerNameLookup struct {
	unsubscribed atomic.Bool
}

func (self *closeDetachesServerNameLookup) ServerNames(string) []string {
	return nil
}

func (self *closeDetachesServerNameLookup) AddServerNamesLearnedCallback(ServerNamesLearnedFunction) func() {
	return func() {
		self.unsubscribed.Store(true)
	}
}

func TestMultiClientCloseDetachesRetiredOwnerReferences(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	owner := &struct{ value int }{value: 1}
	multi := NewRemoteUserNatMultiClient(
		ctx,
		&testingEmptyMultiClientGenerator{},
		func(TransferPath, protocol.ProvideMode, *IpPath, []byte) {
			_ = owner.value
		},
		protocol.ProvideMode_Network,
		DefaultMultiClientSettings(),
	)
	lookup := &closeDetachesServerNameLookup{}
	multi.SetServerNameLookup(lookup)

	multi.Close()

	if holder := multi.receivePacketCallback.Load(); holder != nil {
		t.Fatal("close retained the receive callback owner")
	}
	if config := multi.config.Load(); config.serverNameLookup != nil {
		t.Fatal("close retained the server-name lookup in the routing snapshot")
	}
	if multi.ipAssoc != nil {
		if holder := multi.ipAssoc.serverNameLookup.Load(); holder != nil && holder.lookup != nil {
			t.Fatal("close retained the server-name lookup in IP association state")
		}
	}
	if !lookup.unsubscribed.Load() {
		t.Fatal("close did not release the server-name learned subscription")
	}
}

func TestMultiClientStaleFlowCleanupCannotDeleteReplacement(t *testing.T) {
	for _, version := range []int{4, 6} {
		func() {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			settings := DefaultMultiClientSettings()
			settings.DestinationAffinity = false
			settings.SequenceIdleTimeout = time.Hour
			multi := &RemoteUserNatMultiClient{
				ctx:              ctx,
				cancel:           cancel,
				log:              NewNoopLogger(),
				settings:         settings,
				ip4PathUpdates:   map[Ip4Path]*multiClientChannelUpdate{},
				ip6PathUpdates:   map[Ip6Path]*multiClientChannelUpdate{},
				affinityIp4Paths: map[Ip4Path]map[Ip4Path]time.Time{},
				affinityIp6Paths: map[Ip6Path]map[Ip6Path]time.Time{},
				clientUpdates:    map[*multiClientChannel]map[*multiClientChannelUpdate]bool{},
			}
			path := &IpPath{
				Version:         version,
				Protocol:        IpProtocolTcp,
				SourcePort:      12345,
				DestinationPort: 443,
			}
			if version == 4 {
				path.SourceIp = []byte{10, 0, 0, 1}
				path.DestinationIp = []byte{192, 0, 2, 1}
			} else {
				path.SourceIp = []byte{0x20, 0x01, 0x0d, 0xb8, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1}
				path.DestinationIp = []byte{0x20, 0x01, 0x0d, 0xb8, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 2}
			}

			old, _, _ := multi.sendUpdate(path)
			if old == nil {
				t.Fatal("initial flow was not created")
			}
			clientCtx, cancelClient := context.WithCancel(ctx)
			defer cancelClient()
			client := &multiClientChannel{ctx: clientCtx}
			replacement := newMultiClientChannelUpdate(ctx, path)
			defer replacement.Close()

			// Hold the parent lock while waking the old teardown and installing
			// the replacement. This deterministically makes the old goroutine
			// observe the newer generation when it resumes.
			multi.stateLock.Lock()
			old.client.Store(client)
			multi.clientUpdates[client] = map[*multiClientChannelUpdate]bool{old: true}
			old.cancel()
			if version == 4 {
				multi.ip4PathUpdates[path.ToIp4Path()] = replacement
			} else {
				multi.ip6PathUpdates[path.ToIp6Path()] = replacement
			}
			multi.stateLock.Unlock()

			waitForTestCondition(t, time.Second, func() bool {
				multi.stateLock.Lock()
				defer multi.stateLock.Unlock()
				_, oldRetained := multi.clientUpdates[client]
				return !oldRetained
			}, "stale flow teardown did not finish")

			multi.stateLock.Lock()
			defer multi.stateLock.Unlock()
			if version == 4 {
				if got := multi.ip4PathUpdates[path.ToIp4Path()]; got != replacement {
					t.Fatal("stale IPv4 teardown deleted the replacement flow")
				}
			} else {
				if got := multi.ip6PathUpdates[path.ToIp6Path()]; got != replacement {
					t.Fatal("stale IPv6 teardown deleted the replacement flow")
				}
			}
		}()
	}
}

func TestMultiClientCancelledFlowTableRejectsNewGeneration(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	settings := DefaultMultiClientSettings()
	settings.DestinationAffinity = false
	multi := &RemoteUserNatMultiClient{
		ctx:              ctx,
		cancel:           cancel,
		settings:         settings,
		ip4PathUpdates:   map[Ip4Path]*multiClientChannelUpdate{},
		ip6PathUpdates:   map[Ip6Path]*multiClientChannelUpdate{},
		affinityIp4Paths: map[Ip4Path]map[Ip4Path]time.Time{},
		affinityIp6Paths: map[Ip6Path]map[Ip6Path]time.Time{},
		clientUpdates:    map[*multiClientChannel]map[*multiClientChannelUpdate]bool{},
	}
	cancel()

	update, _, _ := multi.sendUpdate(&IpPath{
		Version:         4,
		Protocol:        IpProtocolUdp,
		SourceIp:        []byte{10, 0, 0, 1},
		SourcePort:      53000,
		DestinationIp:   []byte{192, 0, 2, 53},
		DestinationPort: 53,
	})
	if update != nil {
		t.Fatal("cancelled multi-client admitted a post-close flow generation")
	}
	if len(multi.ip4PathUpdates) != 0 || len(multi.ip6PathUpdates) != 0 {
		t.Fatal("cancelled multi-client recreated flow-table state")
	}
}

func TestMultiClientLateSendReconcileCannotRepublishRetiredGeneration(t *testing.T) {
	for _, version := range []int{4, 6} {
		func() {
			ctx, cancel := context.WithCancel(context.Background())
			multi := &RemoteUserNatMultiClient{
				ctx:              ctx,
				cancel:           cancel,
				ip4PathUpdates:   map[Ip4Path]*multiClientChannelUpdate{},
				ip6PathUpdates:   map[Ip6Path]*multiClientChannelUpdate{},
				affinityIp4Paths: map[Ip4Path]map[Ip4Path]time.Time{},
				affinityIp6Paths: map[Ip6Path]map[Ip6Path]time.Time{},
				clientUpdates:    map[*multiClientChannel]map[*multiClientChannelUpdate]bool{},
			}
			path := &IpPath{
				Version:         version,
				Protocol:        IpProtocolTcp,
				SourcePort:      12345,
				DestinationPort: 443,
			}
			if version == 4 {
				path.SourceIp = []byte{10, 0, 0, 1}
				path.DestinationIp = []byte{192, 0, 2, 1}
			} else {
				path.SourceIp = []byte{0x20, 0x01, 0x0d, 0xb8, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1}
				path.DestinationIp = []byte{0x20, 0x01, 0x0d, 0xb8, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 2}
			}
			update := newMultiClientChannelUpdate(ctx, path)
			defer update.Close()
			if version == 4 {
				multi.ip4PathUpdates[path.ToIp4Path()] = update
			} else {
				multi.ip6PathUpdates[path.ToIp6Path()] = update
			}

			clientCtx, cancelClient := context.WithCancel(context.Background())
			defer cancelClient()
			client := &multiClientChannel{ctx: clientCtx}

			// Model a send callback that began while the flow was live and
			// selected its client only after Close had canceled and removed the
			// map generation. Its deferred reconciliation must not make the
			// retired update reachable again.
			cancel()
			multi.stateLock.Lock()
			clear(multi.ip4PathUpdates)
			clear(multi.ip6PathUpdates)
			clear(multi.clientUpdates)
			multi.stateLock.Unlock()
			update.client.Store(client)
			multi.reconcileSendClientPath(update, nil)

			if got := update.client.Load(); got != nil {
				t.Fatal("retired flow retained a client selected by a late send")
			}
			if len(multi.clientUpdates) != 0 {
				t.Fatal("late send republished a retired flow through clientUpdates")
			}
		}()
	}
}

func TestMultiClientUpdateCloseReleasesCommittedClient(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	update := newMultiClientChannelUpdate(ctx, &IpPath{
		Version:         4,
		Protocol:        IpProtocolTcp,
		SourceIp:        []byte{10, 0, 0, 1},
		SourcePort:      12345,
		DestinationIp:   []byte{192, 0, 2, 1},
		DestinationPort: 443,
	})
	clientCtx, cancelClient := context.WithCancel(context.Background())
	defer cancelClient()
	update.client.Store(&multiClientChannel{ctx: clientCtx})

	update.Close()

	if got := update.client.Load(); got != nil {
		t.Fatal("closed flow generation retained its committed provider client")
	}
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

func TestMultiClientRemovalPrioritizesRecentlyActiveFlowResets(t *testing.T) {
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
	updates := make(map[*multiClientChannelUpdate]bool, 4)
	baseTime := time.Now().Add(-time.Minute)
	for i := range 4 {
		update := &multiClientChannelUpdate{
			ipPath: &IpPath{
				Version:         4,
				Protocol:        IpProtocolTcp,
				SourceIp:        []byte{10, 0, 0, byte(i + 1)},
				SourcePort:      10_000 + i,
				DestinationIp:   []byte{192, 0, 2, 1},
				DestinationPort: 443,
			},
			activityTime: baseTime.Add(time.Duration(i) * time.Second),
		}
		update.client.Store(client)
		updates[update] = true
	}
	multi.clientUpdates[client] = updates

	multi.removeClient(client)

	gotPorts := map[int]bool{}
	for range cap(multi.removalReceiveQueue) {
		packet := <-multi.removalReceiveQueue
		gotPorts[packet.IpPath.SourcePort] = true
	}
	if !gotPorts[10_002] || !gotPorts[10_003] || len(gotPorts) != 2 {
		t.Fatalf("bounded reset work selected ports %v, want the two most recently active flows", gotPorts)
	}
	if got := multi.removalReceiveDropCount.Load(); got != 2 {
		t.Fatalf("accounted reset drops = %d, want 2", got)
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

func testTransferCallbackBackpressure(t *testing.T, invoke func(*stallGate)) {
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

func TestTransferSendAckCallbackPreservesIntentionalBackpressure(t *testing.T) {
	testTransferCallbackBackpressure(t, func(gate *stallGate) {
		safeAck(func(error) { gate.Wait() }, nil)
	})
}

func TestTransferReceiveCallbackPreservesIntentionalBackpressure(t *testing.T) {
	client := &Client{
		log:              DefaultLogger(),
		receiveCallbacks: NewCallbackList[ReceiveFunction](),
	}
	testTransferCallbackBackpressure(t, func(gate *stallGate) {
		client.AddReceiveCallback(func(TransferPath, []*protocol.Frame, Peer) {
			gate.Wait()
		})
		client.receive(TransferPath{}, nil, Peer{})
	})
}

func TestTransferForwardCallbackPreservesIntentionalBackpressure(t *testing.T) {
	client := &Client{
		log:              DefaultLogger(),
		forwardCallbacks: NewCallbackList[ForwardFunction](),
	}
	testTransferCallbackBackpressure(t, func(gate *stallGate) {
		client.AddForwardCallback(func(TransferPath, []byte) {
			gate.Wait()
		})
		client.forward(TransferPath{}, nil)
	})
}

func TestTransferCancelDoesNotBypassInFlightReceiveBackpressure(t *testing.T) {
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
		func(*multiClientChannel, TransferPath, protocol.ProvideMode, IpPath, []byte) {},
		func(*ContractStatus) {},
		func([]*ContractStatsEvent) { statsGate.Wait() },
		func() {},
		nil,
		false,
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
		func(*multiClientChannel, TransferPath, protocol.ProvideMode, IpPath, []byte) {},
		func(*ContractStatus) {},
		func([]*ContractStatsEvent) {},
		func() {},
		nil,
		false,
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
		func(*multiClientChannel, TransferPath, protocol.ProvideMode, IpPath, []byte) {},
		func(*ContractStatus) {},
		func([]*ContractStatsEvent) {},
		func() {},
		nil,
		false,
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

func TestMultiClientPingAdmissionAcceptsSynchronousAck(t *testing.T) {
	pingDone, pingCancel := context.WithCancel(context.Background())
	defer pingCancel()
	var stateLock sync.Mutex
	var began bool
	var acknowledged bool
	var failed bool

	done := make(chan struct{})
	go func() {
		defer close(done)
		success, err := runMultiClientPingAdmission(
			pingDone,
			&stateLock,
			func() { began = true },
			func(ack func(error)) (bool, error) {
				// A transport is allowed to complete admission and its
				// acknowledgement in the same call stack. Holding
				// stateLock across send deadlocks here.
				ack(nil)
				return true, nil
			},
			func(error) {
				acknowledged = true
				pingCancel()
			},
			func() {
				failed = true
				pingCancel()
			},
		)
		if err != nil || !success {
			t.Errorf("synchronous admission failed: success=%v err=%v", success, err)
		}
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("synchronous ping acknowledgement deadlocked on the evaluation lock")
	}
	if !began || !acknowledged || failed {
		t.Fatalf("unexpected evaluation state: began=%v acknowledged=%v failed=%v", began, acknowledged, failed)
	}
}

func TestMultiClientPingAdmissionBlockedNeighborDoesNotStallAck(t *testing.T) {
	var stateLock sync.Mutex
	firstDone, firstCancel := context.WithCancel(context.Background())
	defer firstCancel()
	secondDone, secondCancel := context.WithCancel(context.Background())
	defer secondCancel()

	firstAck := make(chan func(error), 1)
	firstCommitted := make(chan struct{})
	success, err := runMultiClientPingAdmission(
		firstDone,
		&stateLock,
		func() {},
		func(ack func(error)) (bool, error) {
			firstAck <- ack
			return true, nil
		},
		func(error) {
			close(firstCommitted)
			firstCancel()
		},
		firstCancel,
	)
	if err != nil || !success {
		t.Fatalf("first ping admission failed: success=%v err=%v", success, err)
	}

	secondSendStarted := make(chan struct{})
	releaseSecondSend := make(chan struct{})
	secondAdmissionDone := make(chan struct{})
	go func() {
		defer close(secondAdmissionDone)
		_, _ = runMultiClientPingAdmission(
			secondDone,
			&stateLock,
			func() {},
			func(func(error)) (bool, error) {
				close(secondSendStarted)
				<-releaseSecondSend
				return false, nil
			},
			func(error) {},
			secondCancel,
		)
	}()
	select {
	case <-secondSendStarted:
	case <-time.After(time.Second):
		t.Fatal("second ping did not enter its blocked send admission")
	}

	(<-firstAck)(nil)
	select {
	case <-firstCommitted:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("one blocked candidate send stalled another candidate's acknowledgement")
	}

	close(releaseSecondSend)
	select {
	case <-secondAdmissionDone:
	case <-time.After(time.Second):
		t.Fatal("second ping admission did not finish after release")
	}
}

func testMultiClientPingAdmissionPanicReleasesLock(t *testing.T, phase string) {
	t.Helper()
	var stateLock sync.Mutex
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("injected evaluation panic was not propagated")
			}
		}()
		_, _ = runMultiClientPingAdmission(
			context.Background(),
			&stateLock,
			func() {
				if phase == "begin" {
					panic("begin")
				}
			},
			func(func(error)) (bool, error) {
				return false, nil
			},
			func(error) {},
			func() {
				if phase == "failure" {
					panic("failure")
				}
			},
		)
	}()

	lockAvailable := make(chan struct{})
	go func() {
		stateLock.Lock()
		stateLock.Unlock()
		close(lockAvailable)
	}()
	select {
	case <-lockAvailable:
	case <-time.After(time.Second):
		t.Fatal("recovered evaluation panic poisoned the shared window lock")
	}
}

func TestMultiClientPingAdmissionBeginPanicReleasesLock(t *testing.T) {
	testMultiClientPingAdmissionPanicReleasesLock(t, "begin")
}

func TestMultiClientPingAdmissionFailurePanicReleasesLock(t *testing.T) {
	testMultiClientPingAdmissionPanicReleasesLock(t, "failure")
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

func TestMultiClientAckedOneWayTrafficIsNotBlackhole(t *testing.T) {
	settings := DefaultMultiClientSettings()
	channel := &multiClientChannel{settings: settings}
	now := time.Now()
	stats := &clientWindowStats{
		sendAckCount:      5,
		sendAckByteCount:  304,
		firstSendNackTime: now.Add(-2 * settings.BlackholeTimeout),
		lastSendAckTime:   now.Add(-settings.BlackholeTimeout),
	}
	if channel.isBlackholeAt(stats, now) {
		t.Fatal("remotely acknowledged one-way traffic was classified as a blackhole")
	}
}

func TestMultiClientActiveProbeOwnsOutstandingReliableSendLiveness(t *testing.T) {
	settings := DefaultMultiClientSettings()
	channel := &multiClientChannel{settings: settings}
	now := time.Now()
	stats := &clientWindowStats{
		sendNackCount:            1,
		firstOutstandingSendTime: now.Add(-2 * settings.BlackholeTimeout),
	}
	if channel.isBlackholeAt(stats, now) {
		t.Fatal("passive detector canceled a stale reliable send before its active liveness probe")
	}
}

func TestMultiClientProbeDisabledOutstandingReliableSendIsBlackhole(t *testing.T) {
	settings := DefaultMultiClientSettings()
	settings.CPingBusyStaleTimeout = 0
	channel := &multiClientChannel{settings: settings}
	now := time.Now()
	stats := &clientWindowStats{
		sendNackCount:            1,
		firstOutstandingSendTime: now.Add(-2 * settings.BlackholeTimeout),
	}
	if !channel.isBlackholeAt(stats, now) {
		t.Fatal("probe-disabled stale reliable send lost its passive blackhole fallback")
	}
}

func TestMultiClientProbeDisabledTransferAckIsBlackholeProgress(t *testing.T) {
	settings := DefaultMultiClientSettings()
	settings.CPingBusyStaleTimeout = 0
	channel := &multiClientChannel{settings: settings}
	now := time.Now()
	stats := &clientWindowStats{
		sendNackCount:            1,
		firstOutstandingSendTime: now.Add(-2 * settings.BlackholeTimeout),
		lastSendAckTime:          now.Add(-settings.BlackholeTimeout / 2),
	}
	if channel.isBlackholeAt(stats, now) {
		t.Fatal("remote transfer progress did not suppress stale blackhole evidence")
	}
}

func TestMultiClientProbeDisabledExpiredTransferAckDoesNotMaskOutstandingBlackhole(t *testing.T) {
	settings := DefaultMultiClientSettings()
	settings.CPingBusyStaleTimeout = 0
	channel := &multiClientChannel{settings: settings}
	now := time.Now()
	stats := &clientWindowStats{
		sendNackCount:            1,
		firstOutstandingSendTime: now.Add(-3 * settings.BlackholeTimeout),
		lastSendAckTime:          now.Add(-2 * settings.BlackholeTimeout),
	}
	if !channel.isBlackholeAt(stats, now) {
		t.Fatal("old transfer progress masked a currently stalled reliable send")
	}
}

func TestMultiClientActiveProbeDoesNotDisableSilentTcpConnectBlackhole(t *testing.T) {
	settings := DefaultMultiClientSettings()
	channel := &multiClientChannel{settings: settings}
	now := time.Now()
	stats := &clientWindowStats{
		firstSendSynTime: now.Add(-2 * settings.BlackholeConnectTimeout),
		sendSynCount:     1,
	}
	if !channel.isBlackholeAt(stats, now) {
		t.Fatal("active reliable-send probing disabled independent silent TCP connect detection")
	}
}

func TestMultiClientRejectedSendDoesNotCreateBlackholeEvidence(t *testing.T) {
	settings := DefaultMultiClientSettings()
	log := NewNoopLogger()
	clientCtx, cancelClient := context.WithCancel(context.Background())
	cancelClient()
	channel := &multiClientChannel{
		log:                       log,
		args:                      &multiClientChannelArgs{Destination: RequireMultiHopId(NewId())},
		client:                    &Client{ctx: clientCtx},
		createTime:                time.Now(),
		settings:                  settings,
		eventBuckets:              []*multiClientEventBucket{},
		ip4DestinationSourceCount: map[Ip4Path]map[Ip4Path]int{},
		ip6DestinationSourceCount: map[Ip6Path]map[Ip6Path]int{},
		packetStats:               &clientWindowStats{log: log},
	}
	ipPath := &IpPath{
		Version:         4,
		Protocol:        IpProtocolTcp,
		SourceIp:        []byte{10, 0, 0, 1},
		SourcePort:      12345,
		DestinationIp:   []byte{192, 0, 2, 1},
		DestinationPort: 443,
	}

	success, err := channel.sendPacketDetailedWithAck(make([]byte, 512), ipPath, time.Second, true)
	if success || err == nil {
		t.Fatalf("canceled transfer queue accepted packet: success=%t err=%v", success, err)
	}

	var stats *clientWindowStats
	func() {
		channel.stateLock.Lock()
		defer channel.stateLock.Unlock()
		stats = &clientWindowStats{
			sendNackCount:            channel.packetStats.sendNackCount,
			sendNackByteCount:        channel.packetStats.sendNackByteCount,
			firstOutstandingSendTime: channel.packetStats.firstOutstandingSendTime,
		}
	}()
	if stats.sendNackCount != 0 || stats.sendNackByteCount != 0 {
		t.Fatalf(
			"rejected send retained outstanding accounting: %d %dB",
			stats.sendNackCount,
			stats.sendNackByteCount,
		)
	}
	if !stats.firstOutstandingSendTime.IsZero() {
		t.Fatal("rejected send retained a blackhole deadline")
	}
	if channel.isBlackholeAt(stats, time.Now().Add(2*settings.BlackholeTimeout)) {
		t.Fatal("rejected local enqueue was classified as a remote blackhole")
	}
}

func TestMultiClientBusyStaleIgnoresOneWayTrafficWithoutResponseHistory(t *testing.T) {
	settings := DefaultMultiClientSettings()
	settings.CPingBusyStaleTimeout = time.Second
	channel := &multiClientChannel{
		settings: settings,
		packetStats: &clientWindowStats{
			sendAckCount:       5,
			lastSendTime:       time.Now().Add(-2 * time.Second),
			lastSendAckTime:    time.Now().Add(-2 * time.Second),
			lastReceiveAckTime: time.Time{},
		},
	}
	if channel.busyStale() {
		t.Fatal("fully acknowledged one-way traffic requested a liveness probe")
	}
}

func TestMultiClientRemovalDoesNotWaitForPeerConnectionTeardown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	generator := &cleanupOrderTestGenerator{removeCalled: make(chan struct{})}
	settings := DefaultMultiClientSettings()
	settings.CPingRestTimeout = time.Hour
	settings.BlackholeTimeout = time.Hour

	channel, err := newMultiClientChannel(
		ctx,
		&multiClientChannelArgs{
			MultiClientGeneratorClientArgs: MultiClientGeneratorClientArgs{ClientId: NewId()},
			Destination:                    RequireMultiHopId(NewId()),
		},
		generator,
		func(*multiClientChannel, TransferPath, protocol.ProvideMode, IpPath, []byte) {},
		func(*ContractStatus) {},
		func([]*ContractStatsEvent) {},
		func() {},
		nil,
		false,
		settings,
	)
	if err != nil {
		t.Fatal(err)
	}

	// Model a slow Pion teardown without relying on OS/network timing.
	generator.client.webRtcManager.peerConnWorkers.Add(1)
	releaseTeardown := sync.OnceFunc(generator.client.webRtcManager.peerConnWorkers.Done)
	defer releaseTeardown()

	closeReturned := make(chan struct{})
	go func() {
		channel.Close()
		close(closeReturned)
	}()

	select {
	case <-generator.removeCalled:
	case <-time.After(time.Second):
		t.Fatal("platform client removal waited for peer-connection teardown")
	}
	select {
	case <-closeReturned:
	case <-time.After(time.Second):
		t.Fatal("multi-client channel close waited for peer-connection teardown")
	}

	releaseTeardown()
	select {
	case <-generator.client.webRtcManager.closeDone:
	case <-time.After(time.Second):
		t.Fatal("peer manager did not finish after teardown resumed")
	}
}
