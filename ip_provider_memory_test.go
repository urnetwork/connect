package connect

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

func newNatProviderMemoryClient(t *testing.T) *Client {
	t.Helper()
	settings := DefaultClientSettings()
	settings.Log = NewNoopLogger()
	client := NewClient(t.Context(), NewId(), NewNoContractClientOob(), settings)
	t.Cleanup(func() { client.CloseAndWait(context.Background()) })
	return client
}

func newNatProviderMemoryNat(t *testing.T, budget *TransferMemoryBudget) *LocalUserNat {
	t.Helper()
	settings := DefaultLocalUserNatSettings()
	settings.Log = NewNoopLogger()
	settings.MemoryBudget = budget
	nat, err := TryNewLocalUserNat(t.Context(), "provider-memory", settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { nat.CloseAndWait(context.Background()) })
	return nat
}

func newNatProviderMemory(t *testing.T, client *Client, nat *LocalUserNat, configure func(*RemoteUserNatProviderSettings)) *RemoteUserNatProvider {
	t.Helper()
	settings := DefaultRemoteUserNatProviderSettings()
	settings.WriteTimeout = time.Millisecond
	if configure != nil {
		configure(settings)
	}
	provider, err := TryNewRemoteUserNatProvider(client, nat, settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(provider.Close)
	return provider
}

func waitNatProviderMemoryUsed(t *testing.T, budget *TransferMemoryBudget, want ByteCount) {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		changed := budget.CapacityNotify()
		if budget.UsedByteCount() == want {
			return
		}
		select {
		case <-changed:
		case <-timer.C:
			t.Fatalf("memory used=%d, want %d", budget.UsedByteCount(), want)
		}
	}
}

func TestNatProviderMemoryConstructorRefusesBeforeAllocation(t *testing.T) {
	budget := NewTransferMemoryBudget(natMemoryFixedBytes + natProviderMemoryByteCount(natProviderSourceLimit) - 1)
	client := newNatProviderMemoryClient(t)
	nat := newNatProviderMemoryNat(t, budget)
	settings := DefaultRemoteUserNatProviderSettings()
	if allocations := testing.AllocsPerRun(100, func() {
		provider, err := TryNewRemoteUserNatProvider(client, nat, settings)
		if provider != nil || !errors.Is(err, ErrNatMemoryBudget) {
			panic("provider not refused")
		}
	}); allocations != 0 {
		t.Fatalf("refusal allocated %g", allocations)
	}
	var factoryCalls atomic.Int64
	settings.SecurityPolicyGenerator = func(context.Context, *SecurityPolicyStatsCollector) SecurityPolicy {
		factoryCalls.Add(1)
		return DisableSecurityPolicy()
	}
	if allocations := testing.AllocsPerRun(100, func() {
		provider, err := TryNewRemoteUserNatProvider(client, nat, settings)
		if provider != nil || err != ErrNatMemoryPolicy {
			panic("opaque policy was not refused")
		}
	}); allocations != 0 {
		t.Fatalf("policy refusal allocated %g", allocations)
	}
	if factoryCalls.Load() != 0 || len(nat.sourceRetirementOwnerIds) != 0 || budget.UsedByteCount() != natMemoryFixedBytes {
		t.Fatal("refused provider allocated policy/owner state or retained capacity")
	}
	budget.SetTotalByteCount(natMemoryFixedBytes + natProviderMemoryByteCount(natProviderSourceLimit))
	settings.SecurityPolicyGenerator = DefaultProviderSecurityPolicyWithStats
	if allocations := testing.AllocsPerRun(100, func() {
		provider, sub, err := TryNewRemoteUserNatProviderWithPacketStats(client, nat, settings, func(*RemoteUserNatProvider, *PacketStats) {})
		if provider != nil || sub != nil || err != ErrNatMemoryBudget {
			panic("mandatory subscription was not admitted atomically")
		}
	}); allocations != 0 {
		t.Fatalf("atomic subscription refusal allocated %g", allocations)
	}
}

func TestNatProviderMemoryWorstOverlapMakesUdpProgress(t *testing.T) {
	assertMessagePoolOwnership(t)
	root := NewTransferMemoryBudget(mib(13))
	budget := NewTransferMemoryBudgetWithParent(mib(2), root)
	client := newNatProviderMemoryClient(t)
	fallback := newNatProviderMemoryNat(t, budget)
	oldNat := newNatProviderMemoryNat(t, budget)
	newNat := newNatProviderMemoryNat(t, budget)
	create := func(nat *LocalUserNat) (*RemoteUserNatProvider, func()) {
		settings := DefaultRemoteUserNatProviderSettings()
		settings.WriteTimeout = time.Millisecond
		provider, unsubscribe, err := TryNewRemoteUserNatProviderWithPacketStats(client, nat, settings, func(*RemoteUserNatProvider, *PacketStats) {})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(provider.Close)
		return provider, unsubscribe
	}
	oldProvider, oldUnsubscribe := create(oldNat)
	newProvider, newUnsubscribe := create(newNat)
	want := 3*natMemoryFixedBytes + 2*(natProviderMemoryByteCount(natProviderSourceLimit)+kib(1))
	if want != kib(1858) || budget.UsedByteCount() != want || root.UsedByteCount() != want || budget.Available() != kib(190) {
		t.Fatalf("fallback + old/new NAT/provider ledger = %d, want %d", budget.UsedByteCount(), want)
	}
	if oldProvider.settings.MaxSourceCount != 16 || newProvider.settings.MaxSourceCount != 16 {
		t.Fatal("source cap is not per generation")
	}
	// Hold an old-generation callback through Close, then use the replacement
	// while all five fixed owners remain charged.
	memory, admitted := oldProvider.startMemoryOperation(kib(1))
	if !admitted {
		t.Fatal("held callback refused")
	}
	var releaseOnce sync.Once
	releaseOld := func() { releaseOnce.Do(func() { oldProvider.finishMemoryOperation(&memory) }) }
	defer releaseOld()
	closed := make(chan struct{})
	go func() { oldProvider.Close(); close(closed) }()
	select {
	case <-closed:
		t.Fatal("old provider released an active callback")
	default:
	}
	port, stop := startUdpLoopbackEcho(t, 4)
	defer stop()
	received := make(chan struct{}, 1)
	newNat.AddReceivePacketCallback(func(TransferPath, protocol.ProvideMode, *IpPath, []byte) {
		select {
		case received <- struct{}{}:
		default:
		}
	})
	newNat.AddReceivePacketsCallback(func(TransferPath, protocol.ProvideMode, *IpPath, [][]byte) {
		select {
		case received <- struct{}{}:
		default:
		}
	})
	path := udpTestPath(4)
	path.DestinationIp, path.DestinationPort = net.IPv4(127, 0, 0, 1), int(port)
	packet := MessagePoolCopy(ipOosUdpPacket(path, []byte("overlap-progress")))
	if !newNat.SendPacket(SourceId(NewId()), protocol.ProvideMode_Network, packet, 0) {
		MessagePoolReturn(packet)
		t.Fatal("replacement UDP admission refused")
	}
	select {
	case <-received:
	case <-time.After(5 * time.Second):
		t.Fatal("overlap stranded useful UDP traffic")
	}
	if budget.UsedByteCount() > mib(2) {
		t.Fatal("overlap overdraw")
	}
	releaseOld()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("old generation did not retire")
	}
	newProvider.Close()
	oldUnsubscribe()
	newUnsubscribe()
	for _, nat := range []*LocalUserNat{fallback, oldNat, newNat} {
		nat.CloseAndWait(context.Background())
	}
	if stats := budget.Stats(); stats.UsedByteCount != 0 || stats.ReservedByteCount != stats.ReleasedByteCount || root.UsedByteCount() != 0 {
		t.Fatalf("overlap teardown imbalance %+v", stats)
	}
}

func TestNatProviderMemorySourceChurnAndPolicyBounds(t *testing.T) {
	budget := NewTransferMemoryBudget(mib(2))
	nat := newNatProviderMemoryNat(t, budget)
	client := newNatProviderMemoryClient(t)
	var saturated atomic.Int64
	provider := newNatProviderMemory(t, client, nat, func(s *RemoteUserNatProviderSettings) {
		s.SourceLifecycleSaturated = func() { saturated.Add(1) }
	})
	baseline := budget.UsedByteCount()
	for range 1024 {
		id := NewId()
		lifecycle := provider.acquireSourceLifecycle(id)
		if lifecycle == nil {
			t.Fatal("benign idle source churn forced rotation")
		}
		provider.releaseSourceLifecycle(id, lifecycle)
	}
	if len(provider.sourceLifecycles) != 0 || provider.sourceLifecycleSaturated {
		t.Fatal("healthy idle sources were not reclaimed")
	}
	for range natProviderSourceLimit {
		id := NewId()
		lifecycle := provider.acquireSourceLifecycle(id)
		if lifecycle == nil {
			t.Fatal("supported source refused")
		}
		provider.releaseSourceLifecycle(id, lifecycle)
		provider.contractStatus(providerSourceReliabilityStatus(id))
	}
	for range 1024 {
		id := NewId()
		if lifecycle := provider.acquireSourceLifecycle(id); lifecycle != nil {
			t.Fatal("saturated generation reopened")
		}
		provider.contractStatus(providerSourceReliabilityStatus(id))
		provider.recordProviderBlock(id, true, 1, 1)
	}
	provider.stateLock.Lock()
	if len(provider.sourceLifecycles) != 16 || len(provider.sourceDiagnostics) > 16 {
		t.Fatal("source/diagnostics cap escaped")
	}
	provider.stateLock.Unlock()
	policy := provider.securityPolicy.(*reverseSecurityPolicy).policy.(*securityPolicy)
	if policy.dmca.settings.MaxFlows != 64 || policy.stats.maxDestinationsPerResult != 32 {
		t.Fatal("DPI/stat floors escaped")
	}
	for i := 0; i < 1024; i++ {
		policy.stats.add(SecurityDestination{Port: i}, SecurityPolicyResultAllow, 1)
	}
	if len(policy.stats.Stats(false)[SecurityPolicyResultAllow]) > 32 {
		t.Fatal("stats cap escaped")
	}
	if budget.UsedByteCount() != baseline {
		t.Fatal("source churn borrowed unclaimed root capacity")
	}
	provider.Close()
	if len(nat.sourceRetirementOwnerIds) != 0 || len(nat.sourceRetirementSourceIds) != 0 || len(nat.sourceRetirementDoneIds) != 0 {
		t.Fatal("retirement owner/tombstone survived provider teardown")
	}
	if budget.UsedByteCount() != natMemoryFixedBytes {
		waitNatProviderMemoryUsed(t, budget, natMemoryFixedBytes)
	}
}

func TestNatProviderMemoryTransientOwnersAndTombstonesBounded(t *testing.T) {
	budget := NewTransferMemoryBudget(mib(2))
	nat := newNatProviderMemoryNat(t, budget)
	provider := newNatProviderMemory(t, newNatProviderMemoryClient(t), nat, nil)
	baseline := budget.UsedByteCount()
	retired := make(chan struct{}, natProviderSourceLimit)
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	unsubscribe := nat.addSourceRetirementCallback(func(Id, bool) []<-chan struct{} {
		select {
		case retired <- struct{}{}:
		default:
		}
		return []<-chan struct{}{release}
	})
	defer unsubscribe()
	ids := make([]Id, 0, natProviderSourceLimit)
	for range natProviderSourceLimit {
		id := NewId()
		ids = append(ids, id)
		lifecycle := provider.acquireSourceLifecycle(id)
		if lifecycle == nil {
			t.Fatal("source refused before cap")
		}
		provider.releaseUnreachableSource(id, lifecycle)
		provider.releaseSourceLifecycle(id, lifecycle)
	}
	for range natProviderSourceLimit {
		select {
		case <-retired:
		case <-time.After(5 * time.Second):
			t.Fatal("transient owner did not reach retirement")
		}
	}
	for range 1024 {
		if lifecycle := provider.acquireSourceLifecycle(NewId()); lifecycle != nil {
			t.Fatal("idle producer reclaimed an active retirement generation")
		}
	}
	for _, id := range ids {
		provider.contractStatus(providerSourceReliabilityStatus(id))
	}
	provider.stateLock.Lock()
	if len(provider.sourceLifecycles) != 16 || len(provider.unreachableSourceReleases) != 16 {
		t.Fatal("retirement source/worker cap escaped")
	}
	provider.stateLock.Unlock()
	nat.sourceRetirementLock.Lock()
	if len(nat.sourceRetirementOwnerIds) != 17 || len(nat.sourceRetirementSourceIds) != 16 || len(nat.sourceRetirementDoneIds) != 16 {
		t.Fatal("primary plus 16 transient owner topology escaped")
	}
	nat.sourceRetirementLock.Unlock()
	if budget.UsedByteCount() != baseline {
		t.Fatal("precharged retirement graph borrowed capacity")
	}
	unblock()
	provider.unreachableSourceReleaseWorkers.Wait()
	provider.stateLock.Lock()
	if len(provider.unreachableTerminalOwnerIds) != 16 {
		t.Fatal("terminal race silently omitted an exact tombstone owner")
	}
	provider.stateLock.Unlock()
	provider.Close()
	if len(nat.sourceRetirementOwnerIds) != 0 || len(nat.sourceRetirementSourceIds) != 0 || len(nat.sourceRetirementDoneIds) != 0 {
		t.Fatal("joined generation left retirement roots")
	}
	waitNatProviderMemoryUsed(t, budget, natMemoryFixedBytes)
}

func TestNatProviderMemorySaturationCallbackRetainsEnvelope(t *testing.T) {
	for _, callbackCloses := range []bool{false, true} {
		t.Run(map[bool]string{false: "blocked handoff", true: "callback close"}[callbackCloses], func(t *testing.T) {
			budget := NewTransferMemoryBudget(mib(2))
			nat := newNatProviderMemoryNat(t, budget)
			entered, release, returned := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			defer unblock()
			var provider *RemoteUserNatProvider
			provider = newNatProviderMemory(t, newNatProviderMemoryClient(t), nat, func(s *RemoteUserNatProviderSettings) {
				s.MaxSourceCount = 1
				s.SourceLifecycleSaturated = func() {
					close(entered)
					<-release
					if callbackCloses {
						provider.Close()
					}
					close(returned)
				}
			})
			baseline := budget.UsedByteCount()
			provider.contractStatus(providerSourceReliabilityStatus(NewId()))
			<-entered
			if callbackCloses {
				unblock()
			} else {
				provider.Close()
				if budget.UsedByteCount() != baseline {
					t.Fatal("Close returned envelope while handoff goroutine retained it")
				}
				unblock()
			}
			select {
			case <-returned:
			case <-time.After(5 * time.Second):
				t.Fatal("saturation callback shutdown deadlocked")
			}
			waitNatProviderMemoryUsed(t, budget, natMemoryFixedBytes)
		})
	}
}

func TestNatProviderMemoryStatsCallbackRequestClose(t *testing.T) {
	budget := NewTransferMemoryBudget(mib(2))
	provider := newNatProviderMemory(t, newNatProviderMemoryClient(t), newNatProviderMemoryNat(t, budget), func(s *RemoteUserNatProviderSettings) { s.EventEpoch = time.Millisecond })
	requested := make(chan struct{})
	_, err := provider.TryAddPacketStatsCallback(func(*PacketStats) {
		provider.RequestClose()
		close(requested)
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-requested:
	case <-time.After(5 * time.Second):
		t.Fatal("callback-safe close did not return")
	}
	provider.Close()
	waitNatProviderMemoryUsed(t, budget, natMemoryFixedBytes)
}

func TestNatProviderMemoryFragmentsBothDirectionsAndFamilies(t *testing.T) {
	assertMessagePoolOwnership(t)
	budget := NewTransferMemoryBudget(mib(2))
	provider := newNatProviderMemory(t, newNatProviderMemoryClient(t), newNatProviderMemoryNat(t, budget), nil)
	baseline := budget.UsedByteCount()
	for _, gate := range []*ipFragmentGate{&provider.ingressIpv4Fragments, &provider.egressIpv4Fragments} {
		for _, version := range []int{4, 6} {
			fragments := fragmentTestPacketsForVersion(t, version, fragmentTestPayload(1400))
			for range 1024 {
				result := gate.processOwned(SourceId(NewId()), TransferKey{}, protocol.ProvideMode_Public, MessagePoolCopy(fragments[0]))
				if result.packet != nil {
					t.Fatal("incomplete fragment completed")
				}
				returnIpFragmentProcessResult(result)
			}
			gate.mutex.Lock()
			if version == 4 {
				if gate.reassembler4.retainedByteCount > natProviderFragmentBytes || len(gate.reassembler4.datagrams) > ipv4FragmentReassemblyMaxDatagrams {
					t.Fatal("v4 fragment high-water escaped fixed partition")
				}
			} else if gate.reassembler6.retainedByteCount > natProviderFragmentBytes || len(gate.reassembler6.datagrams) > ipv6FragmentReassemblyMaxDatagrams {
				t.Fatal("v6 fragment high-water escaped fixed partition")
			}
			gate.mutex.Unlock()
		}
	}
	if budget.UsedByteCount() != baseline {
		t.Fatal("fragment partition double-charged or escaped")
	}
	provider.Close()
	if provider.ingressIpv4Fragments.reassembler4 != nil || provider.ingressIpv4Fragments.reassembler6 != nil || provider.egressIpv4Fragments.reassembler4 != nil || provider.egressIpv4Fragments.reassembler6 != nil {
		t.Fatal("fragment owner survived fixed release")
	}
}

func TestNatProviderMemoryConcurrentTlsScratchFollowsFlow(t *testing.T) {
	budget := NewTransferMemoryBudget(mib(2))
	provider := newNatProviderMemory(t, newNatProviderMemoryClient(t), newNatProviderMemoryNat(t, budget), nil)
	baseline := budget.UsedByteCount()
	const flowCount = 4
	owner := NewId()
	for i := range flowCount {
		if provider.smtpIngressGuard.inspectForOwner(owner, smtpTestSyn(41000+i, smtpImplicitTlsPort, 100), nil) != smtpEgressAllow {
			t.Fatal("supported TLS flow refused")
		}
	}
	if budget.UsedByteCount() != baseline+flowCount*natSmtpFlowBytes {
		t.Fatal("TLS heap scratch was not retained by each flow")
	}
	fill := budget.Available()
	budget.TryReserve(fill)
	start := make(chan struct{})
	var workers sync.WaitGroup
	for i := range flowCount {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			if got := provider.smtpIngressGuard.inspectForOwner(owner, smtpTestPath(41000+i, smtpImplicitTlsPort, 101), smtpTestClientHello); got != smtpEgressAllow {
				t.Errorf("prepaid TLS inspection refused: %v", got)
			}
		}()
	}
	close(start)
	workers.Wait()
	provider.smtpIngressGuard.stateLock.Lock()
	for _, flow := range provider.smtpIngressGuard.flows {
		if flow.tlsScratch == nil || cap(flow.tlsScratch.handshake) != smtpMaxTlsClientHelloWireBytes || !flow.secure {
			t.Fatal("TLS scratch escaped to unowned caller stacks")
		}
	}
	provider.smtpIngressGuard.stateLock.Unlock()
	path := smtpTestSyn(42000, smtpImplicitTlsPort, 100)
	if allocations := testing.AllocsPerRun(100, func() {
		if provider.smtpIngressGuard.inspectForOwner(owner, path, nil) != smtpEgressReject {
			panic("SMTP full budget admitted another prefix/scratch")
		}
	}); allocations != 0 {
		t.Fatalf("SMTP refusal allocated %g", allocations)
	}
	budget.Release(fill)
	provider.smtpIngressGuard.retireOwner(owner)
	if budget.UsedByteCount() != baseline {
		t.Fatal("TLS scratch claim outlived flow")
	}
}

func TestNatProviderMemorySmtpAndCallbackClaimsBoundConcurrency(t *testing.T) {
	budget := NewTransferMemoryBudget(mib(2))
	provider := newNatProviderMemory(t, newNatProviderMemoryClient(t), newNatProviderMemoryNat(t, budget), nil)
	baseline := budget.UsedByteCount()
	path := smtpTestSyn(40000, smtpStartTlsPort, 100)
	owner := NewId()
	if got := provider.smtpIngressGuard.inspectForOwner(owner, path, nil); got != smtpEgressAllow {
		t.Fatal("SMTP SYN refused")
	}
	if budget.UsedByteCount() != baseline+natSmtpFlowBytes {
		t.Fatal("SMTP prefix/parse scratch not prepaid")
	}
	fill := budget.Available()
	if !budget.TryReserve(fill) {
		t.Fatal("fill failed")
	}
	other := smtpTestSyn(40001, smtpStartTlsPort, 100)
	if got := provider.smtpIngressGuard.inspectForOwner(owner, other, nil); got != smtpEgressReject {
		t.Fatal("SMTP overflow not fail-closed")
	}
	if operation, ok := provider.startMemoryOperation(kib(24)); ok {
		provider.finishMemoryOperation(&operation)
		t.Fatal("concurrent inspection escaped root")
	}
	budget.Release(fill)
	provider.smtpIngressGuard.retireForOwner(owner, path)
	if budget.UsedByteCount() != baseline {
		t.Fatal("SMTP claim did not retire")
	}
}

func TestNatProviderMemoryStatsRegistrationAndTcpPartitionLifetime(t *testing.T) {
	budget := NewTransferMemoryBudget(mib(2))
	provider := newNatProviderMemory(t, newNatProviderMemoryClient(t), newNatProviderMemoryNat(t, budget), nil)
	baseline := budget.UsedByteCount()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unsubscribe, err := provider.TryAddPacketStatsCallback(func(*PacketStats) { once.Do(func() { close(entered) }); <-release })
	if err != nil {
		t.Fatal(err)
	}
	callback := provider.packetStatsCallbacks.Get()[0]
	done := make(chan struct{})
	go func() { callback(&PacketStats{}); close(done) }()
	<-entered
	unsubscribe()
	if budget.UsedByteCount() != baseline+kib(1) {
		t.Fatal("unsubscribe released a captured callback")
	}
	close(release)
	<-done
	if budget.UsedByteCount() != baseline {
		t.Fatal("callback claim leaked")
	}
	fill := budget.Available()
	budget.TryReserve(fill)
	if unsub, err := provider.TryAddPacketStatsCallback(func(*PacketStats) {}); unsub != nil || !errors.Is(err, ErrNatMemoryBudget) {
		t.Fatal("full registration not refused")
	}
	packet := MessagePoolGet(40)
	defer MessagePoolReturn(packet)
	first, ok := provider.startReturnMemoryOperation([][]byte{packet}, receiveRecoveryModeTcpSocket)
	if !ok {
		t.Fatal("full data root blocked prepaid TCP return")
	}
	second, ok := provider.startReturnMemoryOperation([][]byte{packet}, receiveRecoveryModeTcpSocket)
	if !ok {
		t.Fatal("second prepaid TCP return refused")
	}
	if provider.tcpReturnMemory.UsedByteCount() != kib(64) || budget.UsedByteCount() != mib(2) {
		t.Fatal("TCP workspace became hidden root capacity")
	}
	provider.finishMemoryOperation(&first)
	provider.finishMemoryOperation(&second)
	budget.Release(fill)
}
