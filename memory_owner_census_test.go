package connect

import (
	"context"
	"io"
	"net/http"
	"net/netip"
	"sync"
	"testing"
	"time"
	"unsafe"

	"github.com/urnetwork/connect/protocol"
)

func TestMemoryOwnerCensusCountsWorkersAfterIndexRemoval(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	destination := NewId()
	entered, releaseRun := make(chan struct{}), make(chan struct{})
	retired, releaseFinal := make(chan struct{}), make(chan struct{})
	var releaseRunOnce, releaseFinalOnce sync.Once
	settings := closeWaitClientSettings()
	settings.SendBufferSettings.DeliverySizedWindowScale = 1
	settings.SendBufferSettings.beforeRunSendSequenceForTest = func(id sendSequenceId) {
		if id.Destination == destination {
			close(entered)
			<-releaseRun
		}
	}
	settings.SendBufferSettings.afterRunSendSequenceForTest = func(id sendSequenceId) {
		if id.Destination == destination {
			close(retired)
			<-releaseFinal
		}
	}
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
	defer func() {
		releaseRunOnce.Do(func() { close(releaseRun) })
		releaseFinalOnce.Do(func() { close(releaseFinal) })
		cleanup, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		if err := client.CloseAndWait(cleanup); err != nil {
			t.Error(err)
		}
	}()
	accepted, err := client.sendBuffer.Pack(&SendPack{
		TransferOptions: TransferOptions{Ack: true}, Destination: destination, Ctx: ctx,
		Frame:            &protocol.Frame{MessageType: protocol.MessageType_TransferPack, MessageBytes: []byte{1}},
		MessageByteCount: 1, AckCallback: func(error) {},
	}, 0)
	if !accepted || err != nil {
		t.Fatalf("admission=%t err=%v", accepted, err)
	}
	waitCloseWaitBarrier(t, ctx, entered, "census admitted worker")
	before := client.MemoryOwnerCensus()
	// Key/control publication may own another sequence. The held destination
	// is guaranteed present; every counted worker must contribute its storage.
	if before.SendIndexed < 1 || before.SendWorkers < before.SendIndexed || before.SendCanceledWorkers != 0 ||
		before.PacingServices < 1 || before.KnownSequenceStructBytes != before.SendWorkers*int64(unsafe.Sizeof(SendSequence{})) ||
		before.SendPackSlots == 0 || before.KnownChannelSlotBytes == 0 {
		t.Fatalf("admitted census: %+v", before)
	}
	client.Close()
	releaseRunOnce.Do(func() { close(releaseRun) })
	waitCloseWaitBarrier(t, ctx, retired, "census retired worker")
	held := client.MemoryOwnerCensus()
	if held.SendIndexed >= held.SendWorkers || held.SendWorkers < 1 || held.SendCanceledWorkers != held.SendWorkers || held.PacingServices < 1 {
		t.Fatalf("index-only census would hide a retained worker: %+v", held)
	}
	releaseFinalOnce.Do(func() { close(releaseFinal) })
	if err := client.CloseAndWait(ctx); err != nil {
		t.Fatal(err)
	}
	after := client.MemoryOwnerCensus()
	if after.SendWorkers != 0 || after.PacingServices != 0 || after.KnownSequenceStructBytes != 0 || after.KnownChannelSlotBytes != 0 {
		t.Fatalf("joined owner remains: %+v", after)
	}
}

func TestMemoryOwnerCensusCountsReceiveLifetimeAndNoAllocations(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	source, id := SourceId(NewId()), NewId()
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	settings := closeWaitClientSettings()
	settings.ReceiveBufferSettings.beforeRunReceiveSequenceForTest = func(key receiveSequenceId) {
		if key.Source == source && key.SequenceId == id {
			close(entered)
			<-release
		}
	}
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
	defer func() {
		releaseOnce.Do(func() { close(release) })
		_ = client.CloseAndWait(ctx)
	}()
	client.ContractManager().AddNoContractPeer(source.SourceId)
	accepted, err := client.receiveBuffer.Pack(closeWaitReceivePack(ctx, source, id, []byte{1}), 0)
	if !accepted || err != nil {
		t.Fatalf("admission=%t err=%v", accepted, err)
	}
	waitCloseWaitBarrier(t, ctx, entered, "receive census worker")
	before := client.MemoryOwnerCensus()
	if before.ReceiveWorkers != 1 || before.ReceiveIndexed != 1 || before.ReceivePackSlots == 0 ||
		before.KnownSequenceStructBytes != before.SendWorkers*int64(unsafe.Sizeof(SendSequence{}))+int64(unsafe.Sizeof(ReceiveSequence{})) {
		t.Fatalf("receive census: %+v", before)
	}
	if n := testing.AllocsPerRun(100, func() { _ = client.MemoryOwnerCensus() }); n != 0 {
		t.Fatalf("census allocations=%g", n)
	}
	client.Close()
	if got := client.MemoryOwnerCensus(); got.ReceiveCanceledWorkers != 1 {
		t.Fatalf("canceled receive worker hidden: %+v", got)
	}
	releaseOnce.Do(func() { close(release) })
	if err := client.CloseAndWait(ctx); err != nil {
		t.Fatal(err)
	}
	if got := client.MemoryOwnerCensus(); got.ReceiveWorkers != 0 || got.ReceivePackSlots != 0 {
		t.Fatalf("joined receive owner remains: %+v", got)
	}
}

func TestMemoryOwnerCensusFlowGenerationAndBoundedTopology(t *testing.T) {
	parent := flowReaperTestParent(t.Context(), DefaultMultiClientSettings())
	parent.windows = map[WindowType]*multiClientWindow{
		WindowTypeQuality: {clients: map[Id]*multiClientChannel{}},
		WindowTypeSpeed:   {clients: map[Id]*multiClientChannel{}},
	}
	client := &Client{}
	parent.windows[WindowTypeQuality].clients[NewId()] = &multiClientChannel{client: client}
	parent.windows[WindowTypeSpeed].clients[NewId()] = &multiClientChannel{client: client}
	now := time.Now()
	for i := range 400 {
		path := flowReaperTestPath(4, IpProtocolTcp, 10000+i)
		flow := newMultiClientChannelUpdate(t.Context(), path)
		flow.activityTime = now
		parent.flowUpdates[flow] = true
		parent.ip4PathUpdates[path.ToIp4Path()] = flow
		if i != 0 {
			flow.cancel()
		}
	}
	before := parent.MemoryOwnerCensus()
	if before.Transfer.Clients != 1 || before.Flows != 400 || before.CanceledFlows != 399 || before.TcpFlows != 400 {
		t.Fatalf("flow census: %+v", before)
	}
	if n := testing.AllocsPerRun(100, func() { _ = parent.MemoryOwnerCensus() }); n != 0 {
		t.Fatalf("topology census allocations=%g", n)
	}
	retired, _, _ := parent.detachIdleFlows(now)
	for _, flow := range retired {
		flow.update.Close()
	}
	if after := parent.MemoryOwnerCensus(); after.Flows != 1 || after.CanceledFlows != 0 || after.PathEntries != 1 {
		t.Fatalf("reaper did not release finished generations/preserve live TCP: %+v", after)
	}
	for range 70 {
		parent.windows[WindowTypeQuality].clients[NewId()] = &multiClientChannel{client: &Client{clientId: NewId()}}
	}
	if bounded := parent.MemoryOwnerCensus(); bounded.Transfer.Clients != 64 || bounded.ClientEntriesOmitted < 7 {
		t.Fatalf("large topology silently truncated: %+v", bounded)
	}
	for flow := range parent.flowUpdates {
		flow.Close()
	}
}

func TestMemoryOwnerCensusReportsScratchCapacityAndPressureRelease(t *testing.T) {
	assoc := testingIpAssocScratchMatrix(40, 4, true)
	assoc.updateClusters()
	before := assoc.MemoryOwnerCensus()
	if before.Blocks != 4 || before.Entities != 160 || before.Pairs != 3120 ||
		before.ScratchRawPairCapacity < 3120 || before.KnownScratchSliceBytes == 0 || before.PublishedEntities != 40 {
		t.Fatalf("compact output hid raw retained scratch: %+v", before)
	}
	if n := testing.AllocsPerRun(100, func() { _ = assoc.MemoryOwnerCensus() }); n != 0 {
		t.Fatalf("scratch census allocations=%g", n)
	}
	assoc.ShedMemory()
	if after := assoc.MemoryOwnerCensus(); after != (IpAssocOwnerCensus{}) {
		t.Fatalf("released scratch still counted: %+v", after)
	}
}

func TestMemoryOwnerCensusPoolCountsAndClear(t *testing.T) {
	clearSendItemPool()
	t.Cleanup(clearSendItemPool)
	first, second := takeSendItem(), takeSendItem()
	first.messagePoolReturn()
	second.messagePoolReturn()
	got := GetPoolOwnerCensus()
	if got.SendItems != 2 || got.KnownRetainedStructBytes != 2*int64(unsafe.Sizeof(sendItem{})) {
		t.Fatalf("pool census: %+v", got)
	}
	clearSendItemPool()
	if got := GetPoolOwnerCensus(); got != (PoolOwnerCensus{}) {
		t.Fatalf("cleared pool still counted: %+v", got)
	}
}

func TestMemoryOwnerCensusApiActiveIdleClosedAndReleased(t *testing.T) {
	server := newTestAltServer(t, false)
	strategy := newTestAltStrategy(t, server)
	dialer := testAltDialer(t, strategy, "alt h3")
	client := dialer.HttpClient()
	response, err := client.Get("https://" + testAltApiHost + "/census")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	_, err = io.Copy(io.Discard, response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if got := strategy.MemoryOwnerCensus(); got.AltActiveConnections != 1 || got.AltIdleConnections != 0 {
		t.Fatalf("active API owner missing: %+v", got)
	}
	if n := testing.AllocsPerRun(100, func() { _ = strategy.MemoryOwnerCensus() }); n != 0 {
		t.Fatalf("API census allocations=%g", n)
	}
	response.Body.Close()
	waitAltMemorySlot(t, client)
	if got := strategy.MemoryOwnerCensus(); got.AltIdleConnections != 1 || got.AltActiveConnections != 0 {
		t.Fatalf("idle API owner missing: %+v", got)
	}
	conn := client.Transport.(*altQuicBoundedTransport).connection(testAltApiHost + ":443")
	if err := conn.CloseWithError(0, "test census closed retention"); err != nil {
		t.Fatal(err)
	}
	<-conn.Context().Done()
	// The real connection callback already removes the closed owner. A census
	// must observe that lifecycle, not invent a leak from a live reservation.
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	poll := time.NewTicker(time.Millisecond)
	defer poll.Stop()
	for transport := client.Transport.(*altQuicBoundedTransport); transport.connection(testAltApiHost+":443") != nil; {
		select {
		case <-deadline.C:
			t.Fatal("closed API connection remained rooted after lifecycle callback")
		case <-poll.C:
		}
	}
	if got := strategy.MemoryOwnerCensus(); got.AltClosedRetained != 0 || got.AltIdleConnections != 0 || got.AltActiveConnections != 0 {
		t.Fatalf("retired connection still counted: %+v", got)
	}
	// Exercise the closed-but-still-rooted discriminator with an isolated
	// synthetic owner, without altering the actual callback/retirement path.
	retained := &altQuicBoundedTransport{}
	retained.connections[0] = &altQuicConnection{conn: conn, active: true}
	retainedDialer := &clientDialer{httpClient: &http.Client{Transport: retained}}
	retainedStrategy := &ClientStrategy{dialers: map[*clientDialer]bool{retainedDialer: true}}
	if got := retainedStrategy.MemoryOwnerCensus(); got.AltClosedRetained != 1 || got.AltActiveConnections != 0 || got.AltIdleConnections != 0 {
		t.Fatalf("closed-but-rooted discriminator: %+v", got)
	}
	strategy.shedMemory()
	if got := strategy.MemoryOwnerCensus(); got.AltClosedRetained != 0 || got.AltIdleConnections != 0 || got.AltPools != 1 {
		t.Fatalf("pressure release/retained reusable pool wrong: %+v", got)
	}
}

func TestMemoryOwnerCensusConcurrentReadAndTeardown(t *testing.T) {
	settings := DefaultClientStrategySettings()
	settings.EnableNormal, settings.EnableResilient = false, false
	settings.ExpandExtenderProfileCount = 0
	strategy := NewClientStrategy(t.Context(), settings)
	defer strategy.Close()
	dialer := &clientDialer{httpClient: &http.Client{Transport: &http.Transport{}}}
	strategy.dialers[dialer] = true
	assoc := testingIpAssocScratchMatrix(16, 2, false)
	var workers sync.WaitGroup
	for range 4 {
		workers.Go(func() {
			for range 100 {
				_ = strategy.MemoryOwnerCensus()
				_ = assoc.MemoryOwnerCensus()
				_ = GetPoolOwnerCensus()
			}
		})
	}
	for range 25 {
		strategy.shedMemory()
		assoc.updateClusters()
		assoc.ShedMemory()
	}
	strategy.Close()
	workers.Wait()
}

func TestMemoryOwnerCensusClaimsAreByClassAndNotPhysicalBytes(t *testing.T) {
	budget := NewPlatformTransportBudget(1024, 4)
	h1 := budget.register(platformTransportBudgetH1, 100, true)
	h3 := budget.register(platformTransportBudgetH3Explicit, 200, true)
	extender := budget.register(platformTransportBudgetExtender, 300, true)
	pending := budget.register(platformTransportBudgetH1, 50, true)
	for _, claim := range []*platformTransportBudgetReservation{h1, h3, extender} {
		if !claim.Acquire(t.Context()) {
			t.Fatal("claim refused")
		}
		t.Cleanup(claim.Release)
	}
	t.Cleanup(pending.Release)
	got := budget.MemoryOwnerCensus()
	if got != (TransportClaimCensus{H1Count: 1, H1Bytes: 100, H3Count: 1, H3Bytes: 200, ExtenderCount: 1, ExtenderBytes: 300, PendingCount: 1}) {
		t.Fatalf("class census: %+v", got)
	}
	if n := testing.AllocsPerRun(100, func() { _ = budget.MemoryOwnerCensus() }); n != 0 {
		t.Fatalf("claim census allocations=%g", n)
	}
	h1.Release()
	if got := budget.MemoryOwnerCensus(); got.H1Count != 0 || got.H1Bytes != 0 {
		t.Fatalf("released claim counted: %+v", got)
	}
}

func TestMemoryOwnerCensusDnsCountsEachOwnerAndRelease(t *testing.T) {
	key := NewDohKey("A", "private-name.invalid")
	cache := &DohCache{
		queryResultExpiration: map[DohKey]*DohResult{key: {}},
		inflight:              map[DohKey]*dohFlight{key: {}},
	}
	tun := &Tun{}
	tun.dohResolver.Store(cache)
	mux := &UpgradeMux{
		mux:         &IpMux{tun: tun},
		inflight:    map[DohKey]*dnsFlight{key: {}},
		dnsTcpFlows: map[dnsTcpFlowKey]dnsTcpFlow{{}: {}},
		reverse: &reverseIndex{entries: map[netip.Addr]reverseEntry{
			netip.MustParseAddr("192.0.2.1"): {serverNames: []string{"a.invalid", "b.invalid"}},
		}},
	}
	// A repeated pointer is one cache owner, not twice the memory.
	mux.fallbackDohCache.Store(cache)
	want := DnsOwnerCensus{CacheEntries: 1, CacheInflight: 1, MuxInflight: 1, ReverseEntries: 1, ReverseNames: 2, DnsTcpFlows: 1}
	if got := mux.MemoryOwnerCensus(); got != want {
		t.Fatalf("DNS census=%+v want=%+v", got, want)
	}
	if n := testing.AllocsPerRun(100, func() { _ = mux.MemoryOwnerCensus() }); n != 0 {
		t.Fatalf("DNS census allocations=%g", n)
	}
	// Match each production owner's lock. Concurrent reads must neither race
	// nor mutate lifecycle maps while the owner retires its entries.
	var readers sync.WaitGroup
	readers.Go(func() {
		for range 100 {
			_ = mux.MemoryOwnerCensus()
		}
	})
	cache.stateLock.Lock()
	clear(cache.queryResultExpiration)
	clear(cache.inflight)
	cache.stateLock.Unlock()
	mux.inflightLock.Lock()
	clear(mux.inflight)
	mux.inflightLock.Unlock()
	mux.dnsTcpLock.Lock()
	clear(mux.dnsTcpFlows)
	mux.dnsTcpLock.Unlock()
	mux.reverse.lock.Lock()
	clear(mux.reverse.entries)
	mux.reverse.lock.Unlock()
	readers.Wait()
	if got := mux.MemoryOwnerCensus(); got != (DnsOwnerCensus{}) {
		t.Fatalf("retired DNS owners still counted: %+v", got)
	}
}

func TestMemoryOwnerCensusNilOwners(t *testing.T) {
	if (*Client)(nil).MemoryOwnerCensus() != (TransferOwnerCensus{}) ||
		(*ClientStrategy)(nil).MemoryOwnerCensus() != (ApiOwnerCensus{}) ||
		(*RemoteUserNatMultiClient)(nil).MemoryOwnerCensus() != (MultiClientOwnerCensus{}) ||
		(*IpAssoc)(nil).MemoryOwnerCensus() != (IpAssocOwnerCensus{}) ||
		(*UpgradeMux)(nil).MemoryOwnerCensus() != (DnsOwnerCensus{}) ||
		(*PlatformTransportBudget)(nil).MemoryOwnerCensus() != (TransportClaimCensus{}) {
		t.Fatal("absent owner returned nonzero census")
	}
}

func BenchmarkMemoryOwnerCensusFlowTopology(b *testing.B) {
	parent := flowReaperTestParent(b.Context(), DefaultMultiClientSettings())
	for i := range 400 {
		path := flowReaperTestPath(4, IpProtocolTcp, 10000+i)
		flow := newMultiClientChannelUpdate(b.Context(), path)
		parent.flowUpdates[flow] = true
		parent.ip4PathUpdates[path.ToIp4Path()] = flow
	}
	b.Cleanup(func() {
		for flow := range parent.flowUpdates {
			flow.Close()
		}
	})
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_ = parent.MemoryOwnerCensus()
	}
}
