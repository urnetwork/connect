// Owner policy is normalized before constructors allocate or admit a batch.
package connect

import (
	"context"
	"net"
	"testing"
	"testing/synctest"
)

func TestTcpAdaptiveBatchPolicyOwningConstructors(t *testing.T) {
	for _, row := range []struct {
		name                                          string
		write, cold, hot, sequence, wantCold, wantHot int
	}{
		{name: "defaults", write: 64, cold: 2, hot: 8, sequence: 16, wantCold: 2, wantHot: 8},
		{name: "hot-disabled-zero", write: 64, cold: 2, hot: 0, sequence: 16, wantCold: 2, wantHot: 2},
		{name: "hot-disabled-negative", write: 64, cold: 2, hot: -1, sequence: 16, wantCold: 2, wantHot: 2},
		{name: "cold-zero", write: 64, cold: 0, hot: 8, sequence: 16, wantCold: 1, wantHot: 8},
		{name: "cold-negative", write: 64, cold: -1, hot: 8, sequence: 16, wantCold: 1, wantHot: 8},
		{name: "both-disabled", write: 64, cold: 0, hot: 0, sequence: 16, wantCold: 1, wantHot: 1},
		{name: "hot-lower", write: 64, cold: 2, hot: 4, sequence: 16, wantCold: 2, wantHot: 4},
		{name: "cold-lower", write: 64, cold: 1, hot: 4, sequence: 16, wantCold: 1, wantHot: 4},
		{name: "single", write: 64, cold: 2, hot: 1, sequence: 16, wantCold: 1, wantHot: 1},
		{name: "write-zero", write: 0, cold: 2, hot: 8, sequence: 16, wantCold: 1, wantHot: 1},
		{name: "write-negative", write: -1, cold: 2, hot: 8, sequence: 16, wantCold: 1, wantHot: 1},
		{name: "requested-lower", write: 3, cold: 2, hot: 8, sequence: 16, wantCold: 2, wantHot: 3},
		{name: "queue-one", write: 64, cold: 2, hot: 8, sequence: 1, wantCold: 1, wantHot: 1},
	} {
		func() {
			settings := DefaultLocalUserNatSettingsWithBufferSize(row.sequence)
			budget := NewTransferMemoryBudget(mib(2))
			settings.MemoryBudget = budget
			tcp := settings.TcpBufferSettings
			tcp.MemoryBudget = budget
			tcp.WriteBatchSize, tcp.MemoryColdWriteBatchSize, tcp.MemoryWriteBatchSize = row.write, row.cold, row.hot
			originalReturnBudget, originalRead, originalMtu := tcp.ReturnQueueBudget, tcp.ReadBufferByteCount, tcp.Mtu
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			nat, err := TryNewLocalUserNat(ctx, "batch-settings", settings)
			if err != nil {
				t.Fatalf("%s: %v", row.name, err)
			}
			defer func() {
				if err := nat.CloseAndWait(context.Background()); err != nil {
					t.Errorf("%s: %v", row.name, err)
				}
				if budget.UsedByteCount() != 0 {
					t.Errorf("%s: joined constructor retained budget: %+v", row.name, budget.Stats())
				}
			}()
			four, six := NewTcp4Buffer(ctx, nil, tcp), NewTcp6Buffer(ctx, nil, tcp)
			for _, actual := range []*TcpBufferSettings{nat.settings.TcpBufferSettings, four.tcpBufferSettings, six.tcpBufferSettings} {
				if actual.WriteBatchSize != row.wantHot || actual.MemoryColdWriteBatchSize != row.wantCold || actual.MemoryBudget != budget {
					t.Fatalf("%s: constructor policy cold=%d hot=%d want=%d/%d", row.name, actual.MemoryColdWriteBatchSize, actual.WriteBatchSize, row.wantCold, row.wantHot)
				}
				if boundedNatTcpMemorySettings(actual, budget) != actual {
					t.Fatalf("%s: normalization changed an already effective settings identity", row.name)
				}
			}
			if tcp.WriteBatchSize != row.write || tcp.MemoryColdWriteBatchSize != row.cold || tcp.MemoryWriteBatchSize != row.hot ||
				tcp.ReturnQueueBudget != originalReturnBudget || tcp.ReadBufferByteCount != originalRead || tcp.Mtu != originalMtu {
				t.Fatalf("%s: construction mutated caller settings", row.name)
			}
			if four.tcpBufferSettings.ReturnQueueBudget != originalReturnBudget || six.tcpBufferSettings.ReturnQueueBudget != originalReturnBudget ||
				four.tcpBufferSettings.ReadBufferByteCount != originalRead || six.tcpBufferSettings.Mtu != originalMtu {
				t.Fatalf("%s: direct TCP batch policy changed an unrelated owner limit", row.name)
			}
			cancel()
			four.waitForLifecycle()
			six.waitForLifecycle()
		}()
	}
	unbounded := DefaultTcpBufferSettings()
	if unbounded.WriteBatchSize != 64 || unbounded.MemoryWriteBatchSize != 8 || unbounded.MemoryColdWriteBatchSize != 2 {
		t.Fatal("defaults no longer declare independent unbudgeted64/cold2/hot8")
	}
	unbounded.MemoryWriteBatchSize, unbounded.MemoryColdWriteBatchSize = 1, 1
	buffer := NewTcp4Buffer(t.Context(), nil, unbounded)
	if buffer.tcpBufferSettings != unbounded || buffer.tcpBufferSettings.WriteBatchSize != 64 {
		t.Fatal("budgeted batch policy leaked into unbudgeted TCP")
	}
	buffer.waitForLifecycle()
}

func TestTcpAdaptiveBatchPolicyLowerFundingAndFallback(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		root := NewTransferMemoryBudget(mib(2))
		defer func() {
			if root.UsedByteCount() != 0 {
				t.Error("lower-policy graph retained memory after all owners joined")
			}
		}()
		settings := DefaultLocalUserNatSettingsWithBufferSize(16)
		settings.MemoryBudget = NewTransferMemoryBudgetWithParent(mib(2), root)
		settings.TcpBufferSettings.MemoryColdWriteBatchSize, settings.TcpBufferSettings.MemoryWriteBatchSize = 1, 4
		tcp := boundedNatMemorySettings(settings).TcpBufferSettings
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		memory, admitted := reserveNatMemory(tcp.MemoryBudget, natTcpFlowMemoryByteCount(tcp))
		if !admitted {
			t.Fatal("cold owner refused")
		}
		defer memory.release()
		queue := newTcpReturnReadQueue(ctx, tcp, newTcpReturnBatchWorkspace(ctx, tcp))
		defer func() {
			queue.close()
			for {
				packets, ok := queue.takeBatch(context.Background(), 0)
				if !ok {
					break
				}
				adaptiveReturnFinish(queue, packets)
			}
			queue.releaseAfterJoin()
		}()
		base := root.UsedByteCount()
		adaptiveReturnOffer(t, queue)
		if queue.workspace.funded.Load() || root.UsedByteCount() != base {
			t.Fatal("one cold packet speculatively reserved hot credit")
		}
		other, ok := reserveNatMemory(root, root.TotalByteCount()-base)
		if !ok {
			t.Fatal("failed to fill real parent")
		}
		defer other.release()
		fundingAttempt := make(chan bool, 1)
		queue.afterFundingAttemptForTest = func(ok bool) { fundingAttempt <- ok }
		packet := MessagePoolCopy([]byte("second"))
		done := make(chan bool, 1)
		go func() {
			accepted := queue.offer(packet)
			if !accepted {
				MessagePoolReturn(packet)
			}
			done <- accepted
		}()
		synctest.Wait()
		select {
		case ok := <-fundingAttempt:
			if ok {
				t.Fatal("full parent granted optional memory")
			}
		default:
			t.Fatal("ready overflow did not attempt exact admission")
		}
		select {
		case <-done:
			t.Fatal("refused promotion bypassed cold backpressure")
		default:
		}
		packets, ok := queue.takeBatch(ctx, 0)
		if !ok || len(packets) != 1 {
			t.Fatal("cold one-packet drain changed")
		}
		adaptiveReturnFinish(queue, packets)
		synctest.Wait()
		if accepted := <-done; !accepted {
			t.Fatal("cold drain did not recover the original waiter")
		}
		queue.afterFundingAttemptForTest = nil
		other.release()
		for range 3 {
			adaptiveReturnOffer(t, queue)
		}
		if root.UsedByteCount() != natTcpFundedFlowMemoryByteCount(tcp) || queue.workspace.maximumBatch != 4 {
			t.Fatal("configured hot roots lack their exact complete claim")
		}
		packets, ok = queue.takeBatch(ctx, 0)
		if !ok || len(packets) != 4 || !queue.workspace.acquire() {
			t.Fatal("funded lower maximum was not delivered")
		}
		queue.workspace.release()
		adaptiveReturnFinish(queue, packets)
		if root.UsedByteCount() != base || queue.workspace.funded.Load() {
			t.Fatal("callback retained optional credit")
		}
	})
}

func TestTcpAdaptiveBatchPolicyProviderRejectsBeyondWitness(t *testing.T) {
	assertMessagePoolOwnership(t)
	budget := NewTransferMemoryBudget(mib(2))
	synctest.Test(t, func(t *testing.T) {
		fixture := newTcpProviderMemoryRetirementFixtureWithTcpSettings(t, budget, 4, func(settings *TcpBufferSettings) {
			settings.MemoryColdWriteBatchSize, settings.MemoryWriteBatchSize = 1, 4
		})
		fixture.send(tcpFlagSyn, 1000, 0, nil)
		sequence := fixture.nextSequence(1000)
		fixture.takeControls()
		workspace := sequence.ipPath.tcpReturnBatchWorkspace
		if sequence.tcpBufferSettings.MemoryColdWriteBatchSize != 1 || workspace == nil || workspace.maximumBatch != 4 {
			t.Fatal("actual constructor did not own its lower batch policy")
		}
		base := budget.UsedByteCount()
		queue := newTcpReturnReadQueue(sequence.ctx, sequence.tcpBufferSettings, workspace)
		defer func() {
			queue.close()
			for {
				packets, ok := queue.takeBatch(context.Background(), 0)
				if !ok {
					break
				}
				adaptiveReturnFinish(queue, packets)
			}
			queue.releaseAfterJoin()
		}()
		for range 4 {
			adaptiveReturnOffer(t, queue)
		}
		func() {
			packets, ok := queue.takeBatch(sequence.ctx, 0)
			if !ok || len(packets) != 4 {
				t.Fatal("configured callback did not drain")
			}
			defer adaptiveReturnFinish(queue, packets)
			if !workspace.acquire() {
				t.Fatal("funded callback permission missing")
			}
			defer workspace.release()
			extra := MessagePoolCopy(make([]byte, DefaultMtu))
			defer MessagePoolReturn(extra)
			oversized := append(append([][]byte(nil), packets...), extra)
			provider := fixture.provider
			before := provider.tcpReturnMemory.Stats()
			memory, source, admitted := provider.startReturnMemoryOperationForSourceWithWorkspace(oversized, receiveRecoveryModeTcpSocket, fixture.source.SourceId, sequence.ctx, workspace)
			if admitted {
				provider.releaseSourceLifecycle(fixture.source.SourceId, source)
				provider.finishMemoryOperation(&memory)
				t.Fatal("a fifth packet bypassed the four-packet funded witness")
			}
			after := provider.tcpReturnMemory.Stats()
			if after.UsedByteCount != before.UsedByteCount || after.ReservedByteCount != before.ReservedByteCount {
				t.Fatal("refused callback took provider scratch")
			}
			memory, source, admitted = provider.startReturnMemoryOperationForSourceWithWorkspace(packets, receiveRecoveryModeTcpSocket, fixture.source.SourceId, sequence.ctx, workspace)
			if !admitted {
				t.Fatal("valid funded lower callback was refused after the negative")
			}
			provider.releaseSourceLifecycle(fixture.source.SourceId, source)
			provider.finishMemoryOperation(&memory)
		}()
		if budget.UsedByteCount() != base || workspace.funded.Load() || workspace.inUse.Load() || fixture.provider.tcpReturnMemory.UsedByteCount() != 0 {
			t.Fatal("configured callback failed to balance its original owners")
		}
	})
	if budget.UsedByteCount() != 0 {
		t.Fatal("joined constructor/provider graph retained memory")
	}
}

func TestTcpAdaptiveBatchPolicyLargeRequestsStayWithinChargedStorage(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, mtu := range []int{44, DefaultMtu, DefaultTunnelMtu} {
		settings := DefaultLocalUserNatSettingsWithBufferSize(16)
		settings.MemoryBudget = NewTransferMemoryBudget(mib(2))
		settings.TcpBufferSettings.Mtu = mtu
		settings.TcpBufferSettings.WriteBatchSize = int(^uint(0) >> 1)
		settings.TcpBufferSettings.MemoryColdWriteBatchSize = int(^uint(0) >> 1)
		settings.TcpBufferSettings.MemoryWriteBatchSize = int(^uint(0) >> 1)
		nat, err := TryNewLocalUserNat(t.Context(), "large-policy", settings)
		if err != nil {
			t.Fatal(err)
		}
		func() {
			defer func() {
				if err := nat.CloseAndWait(context.Background()); err != nil {
					t.Error(err)
				}
			}()
			tcp := nat.settings.TcpBufferSettings
			packetBytes := retainedMessageCapacity(ByteCount(tcp.Mtu)) + natPacketMemoryByteCount(nil)
			cold, hot := tcp.MemoryColdWriteBatchSize, tcp.WriteBatchSize
			if cold < 1 || hot < cold || hot > tcp.SequenceBufferSize || ByteCount(cold)*packetBytes > kib(8) ||
				providerPacketOperationBytes(nil)+ByteCount(cold)*(packetBytes+natProviderPacketOperationPerPacketBytes) > natProviderTcpReturnSlotBytes ||
				ByteCount(hot)*packetBytes > natProviderTcpReturnSlotBytes ||
				providerPacketOperationBytes(nil)+ByteCount(hot)*(packetBytes+natProviderPacketOperationPerPacketBytes) > 2*natProviderTcpReturnSlotBytes {
				t.Fatalf("large policy exceeded its charged storage: mtu=%d cold=%d hot=%d", mtu, cold, hot)
			}
			state := ConnectionState{ipVersion: 4, peerMss: 1, sourceIp: net.IPv4(192, 0, 2, 1).To4(), destinationIp: net.IPv4(198, 51, 100, 1).To4()}
			limit := state.natPacketizationByteLimit(tcp.Mtu, cold)
			packets, err := state.DataPackets(make([]byte, limit), limit, tcp.Mtu)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				for _, packet := range packets {
					MessagePoolReturn(packet)
				}
			}()
			if len(packets) != cold {
				t.Fatal("MSS1 producer exceeded or ignored its charged packet count")
			}
			root := NewTransferMemoryBudget(natTcpFundedFlowMemoryByteCount(tcp))
			defer func() {
				if root.UsedByteCount() != 0 {
					t.Error("larger-policy graph retained exact root charge")
				}
			}()
			copySettings := *tcp
			copySettings.MemoryBudget, copySettings.ReturnQueueBudget = root, root
			memory, ok := reserveNatMemory(root, natTcpFlowMemoryByteCount(&copySettings))
			if !ok {
				t.Fatal("cold claim refused exact funded root")
			}
			defer memory.release()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			queue := newTcpReturnReadQueue(ctx, &copySettings, newTcpReturnBatchWorkspace(ctx, &copySettings))
			defer func() {
				queue.close()
				for {
					batch, ok := queue.takeBatch(context.Background(), 0)
					if !ok {
						break
					}
					adaptiveReturnFinish(queue, batch)
				}
				queue.releaseAfterJoin()
			}()
			for range hot {
				packet := MessagePoolCopy(make([]byte, tcp.Mtu))
				if !queue.offer(packet) {
					MessagePoolReturn(packet)
					t.Fatal("funded configured root refused")
				}
			}
			if root.UsedByteCount() != root.TotalByteCount() {
				t.Fatal("larger configured batch lacks its complete root charge")
			}
			if root.TryReserve(1) {
				root.Release(1)
				t.Fatal("larger configured batch bypassed exact root admission")
			}
			batch, ok := queue.takeBatch(ctx, 0)
			if !ok || len(batch) != hot {
				t.Fatal("bounded larger callback failed to drain")
			}
			adaptiveReturnFinish(queue, batch)
			t.Logf("charged configured batch: mtu=%d cold=%d hot=%d base=%d complete=%d", mtu, cold, hot, natTcpFlowMemoryByteCount(tcp), natTcpFundedFlowMemoryByteCount(tcp))
		}()
		if settings.MemoryBudget.UsedByteCount() != 0 {
			t.Fatal("large-policy constructor retained memory after join")
		}
	}
}
