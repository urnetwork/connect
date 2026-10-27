// Native acknowledgement compaction keeps original allocation charges and
// packet owners through real source refusal, serialization and completion.
package connect

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
	"unsafe"
)

// Establishes coverage from an actual provider upload and inner acknowledgement.
// The test-owned reader is joined even when a later assertion fails.
func nativeAckPartitionCoveredFixture(t *testing.T) (*nativeDuplicateAckFixture, []byte, []byte) {
	t.Helper()
	fixture := newNativeDuplicateAckFixture(t, false)
	fixture.consumer.stateLock.Lock()
	base := fixture.consumer.base
	fixture.consumer.stateLock.Unlock()
	payload := []byte("data")
	data := groupRecoveryPacket(fixture.initial+1, base, fixture.window, tcpFlagAck, payload)
	done := make(chan struct{})
	type uploadResult struct {
		payload []byte
		err     error
	}
	result := make(chan uploadResult, 1)
	t.Cleanup(func() { fixture.close(); <-done })
	go func() {
		defer close(done)
		received := make([]byte, len(payload))
		_, err := io.ReadFull(fixture.origin, received)
		result <- uploadResult{payload: received, err: err}
	}()
	if !fixture.offer(data) {
		t.Fatal("partition control's initial upload was refused")
	}
	synctest.Wait()
	select {
	case actual := <-result:
		if actual.err != nil || !bytes.Equal(actual.payload, payload) {
			t.Fatalf("partition control's actual upload: payload=%q err=%v", actual.payload, actual.err)
		}
	default:
		t.Fatal("partition control's actual upload did not drain")
	}
	parsed := groupRecoveryParsed(t, data)
	if fixture.parent.canSendPacket(parsed, fixture.update, fixture.selected) {
		t.Fatal("partition control lacks already-covered upload data")
	}
	ack := groupRecoveryPacket(fixture.initial+1+uint32(len(payload)), base, fixture.window, tcpFlagAck, nil)
	return fixture, data, ack
}

// Keeps one witness per input until production and its reader have joined.
func nativeAckPartitionOwnPackets(t *testing.T, fixture *nativeDuplicateAckFixture, templates ...[]byte) [][]byte {
	t.Helper()
	packets := make([][]byte, len(templates))
	for index, template := range templates {
		packets[index] = MessagePoolCopy(template)
	}
	witnesses := groupTestPacketWitnesses(t, packets)
	t.Cleanup(func() {
		fixture.close()
		requireGroupTestWitnessesReleased(t, packets, witnesses)
	})
	return packets
}

// Thirty-one covered copies and one pure acknowledgement retain the complete
// original allocation while admitting exactly the final original position.
func nativeAckPartitionThirtyTwo(data, ack []byte) [][]byte {
	templates := make([][]byte, 32)
	for index := range templates[:31] {
		templates[index] = data
	}
	templates[31] = ack
	return templates
}

// Independent lower bound exposes the original 32-member allocation even
// when the live wire item contains only one acknowledgement.
func nativeAckPartitionMetadataLowerBound() ByteCount {
	round := func(bytes ByteCount) ByteCount { return (bytes + 63) / 64 * 64 }
	return round(ByteCount(unsafe.Sizeof(parsedPacketGroup{}))) +
		round(32*ByteCount(unsafe.Sizeof(parsedPacket{}))) +
		32*round(ByteCount(unsafe.Sizeof(IpPath{})))
}

// The range owner and full original graph must be funded before the route
// observes bytes; discarded scalar/path slots cannot retain auxiliary roots.
func TestNativeAckCompactionChargesThirtyTwoMemberMetadata(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture, data, ack := nativeAckPartitionCoveredFixture(t)
		providerPackets := watchNativePureAckProvider(t, fixture)
		budget := fixture.sender.settings.SendBufferSettings.ResendQueueBudget
		baseline := budget.UsedByteCount()
		entered, release := make(chan struct{}), make(chan struct{})
		var enterOnce, releaseOnce sync.Once
		resume := func() { releaseOnce.Do(func() { close(release) }) }
		t.Cleanup(resume)
		fixture.sender.settings.SendBufferSettings.beforeGroupRetainForTest = func() {
			enterOnce.Do(func() { close(entered) })
			select {
			case <-release:
			case <-fixture.ctx.Done():
			}
		}
		var admitted atomic.Pointer[parsedPacketGroup]
		var wireCharge atomic.Int64
		var originalPaths [32]*IpPath
		fixture.parent.beforePacketGroupGateForTest = func(_ *multiClientChannelUpdate, _ *multiClientChannel, group *parsedPacketGroup) {
			if len(group.packets) == len(originalPaths) {
				for index := range originalPaths {
					if index != len(originalPaths)-1 {
						group.packets[index].ipPath.ServerName = string(bytes.Repeat([]byte("discarded.example"), 2))
					}
					originalPaths[index] = group.packets[index].ipPath
				}
			}
		}
		fixture.sender.settings.SendBufferSettings.afterGroupAdmissionForTest = func(target sendGroupAdmissionTarget) {
			admitted.Store(target.(*parsedPacketGroup))
		}
		fixture.sender.settings.SendBufferSettings.TransferWireMessageObserver = func(TransferWireMessageObservation) {
			group := admitted.Load()
			if group == nil {
				t.Error("route write preceded the captured compacted admission")
				return
			}
			fixture.update.stateLock.Lock()
			var charge ByteCount
			if group.collapseOwner != nil {
				charge = group.collapseOwner.budgetBytes
			}
			fixture.update.stateLock.Unlock()
			wireCharge.Store(int64(charge))
			if budget.UsedByteCount() < baseline+charge {
				t.Error("route write escaped the joint graph reservation")
			}
		}
		templates := nativeAckPartitionThirtyTwo(data, ack)
		membership := make([]bool, len(templates))
		count := fixture.parent.SendNativePacketBatchWithResults(SourceId(fixture.sender.ClientId()),
			fixture.parent.provideMode, nativeAckPartitionOwnPackets(t, fixture, templates...), 5*time.Second, membership)
		synctest.Wait()
		select {
		case <-entered:
		default:
			t.Fatal("compacted acknowledgement never reached the held pre-reservation source boundary")
		}
		group := admitted.Load()
		want := make([]bool, 32)
		want[31] = true
		if count != 1 || !slices.Equal(membership, want) || group == nil || len(group.packets) != 1 || cap(group.packets) != 32 || group.retainedIpPathCount != 32 ||
			group.completionFlags.Load()&groupCompletionCompactedMetadata == 0 || !group.packets[0].pureTcpAck() ||
			group.collapseOwner != nil || budget.UsedByteCount() != baseline || providerPackets.Load() != 0 {
			t.Fatalf("32-to-1 pre-reservation custody changed: count=%d membership=%v group=%p budget=%d baseline=%d provider=%d",
				count, membership, group, budget.UsedByteCount(), baseline, providerPackets.Load())
		}
		charge := group.sendGroupMemoryByteCount()
		if charge < nativeAckPartitionMetadataLowerBound() {
			t.Fatalf("32-to-1 metadata charge follows surviving length: charge=%d minimum=%d", charge, nativeAckPartitionMetadataLowerBound())
		}
		for index, path := range originalPaths[:31] {
			if path == nil || !reflect.DeepEqual(*path, IpPath{}) {
				t.Fatalf("discarded path %d retained metadata roots", index)
			}
		}
		for index, packet := range group.packets[:cap(group.packets)][1:] {
			if !reflect.DeepEqual(packet, parsedPacket{}) {
				t.Fatalf("discarded parsed slot %d retained an owner", index+1)
			}
		}
		resume()
		synctest.Wait()
		fixture.update.stateLock.Lock()
		remainingCharge := group.collapseOwner.budgetBytes
		fixture.update.stateLock.Unlock()
		if providerPackets.Load() != 1 || ByteCount(wireCharge.Load()) != charge || budget.UsedByteCount() != baseline || remainingCharge != 0 ||
			group.completionFlags.Load()&groupCompletionCompactedMetadata == 0 || group.collapseOrder() == 0 {
			t.Fatalf("32-to-1 graph charge did not span exact ownership: provider=%d wire_charge=%d want=%d budget=%d baseline=%d remaining=%d",
				providerPackets.Load(), wireCharge.Load(), charge, budget.UsedByteCount(), baseline, remainingCharge)
		}
	})
}

// A joint reservation failure occurs before range allocation or any write.
// Returning capacity admits a fresh identical burst without a double return.
func TestNativeAckCompactionMetadataRefusalThenRetry(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture, data, ack := nativeAckPartitionCoveredFixture(t)
		providerPackets := watchNativePureAckProvider(t, fixture)
		budget := fixture.sender.settings.SendBufferSettings.ResendQueueBudget
		baseline := budget.UsedByteCount()
		entered, release := make(chan struct{}), make(chan struct{})
		var enterOnce, releaseOnce sync.Once
		resume := func() { releaseOnce.Do(func() { close(release) }) }
		t.Cleanup(resume)
		fixture.sender.settings.SendBufferSettings.beforeGroupRetainForTest = func() {
			enterOnce.Do(func() { close(entered) })
			select {
			case <-release:
			case <-fixture.ctx.Done():
			}
		}
		var admitted atomic.Pointer[parsedPacketGroup]
		fixture.sender.settings.SendBufferSettings.afterGroupAdmissionForTest = func(target sendGroupAdmissionTarget) {
			admitted.Store(target.(*parsedPacketGroup))
		}
		previous := fixture.sender.settings.SendBufferSettings.SendPackLifecycleObserver
		var unwrittenRefusals atomic.Int64
		fixture.sender.settings.SendBufferSettings.SendPackLifecycleObserver = func(event SendPackLifecycleObservation) {
			if event.Phase == SendPackLifecyclePhaseTerminal && sendGroupCapacityFailure(event.Err) {
				unwrittenRefusals.Add(1)
				return
			}
			previous(event)
		}
		templates := nativeAckPartitionThirtyTwo(data, ack)
		offer := func() (int, []bool) {
			membership := make([]bool, len(templates))
			count := fixture.parent.SendNativePacketBatchWithResults(SourceId(fixture.sender.ClientId()),
				fixture.parent.provideMode, nativeAckPartitionOwnPackets(t, fixture, templates...), 5*time.Second, membership)
			return count, membership
		}
		before := time.Now()
		count, membership := offer()
		synctest.Wait()
		select {
		case <-entered:
		default:
			t.Fatal("metadata refusal missed its exact pre-reservation boundary")
		}
		group := admitted.Load()
		want := make([]bool, 32)
		want[31] = true
		if count != 1 || !slices.Equal(membership, want) || group == nil {
			t.Fatalf("metadata test never secured its one source owner: count=%d membership=%v", count, membership)
		}
		graphCharge := group.sendGroupMemoryByteCount()
		if graphCharge < nativeAckPartitionMetadataLowerBound() {
			t.Fatal("metadata refusal premise lacks the original full graph charge")
		}
		held := budget.Available() - (graphCharge - 1)
		if held <= 0 || !budget.TryReserve(held) {
			t.Fatal("metadata test could not leave one byte less than its exact graph")
		}
		defer func() {
			if held > 0 {
				budget.Release(held)
			}
		}()
		resume()
		synctest.Wait()
		if providerPackets.Load() != 0 || unwrittenRefusals.Load() != 1 || group.collapseOwner != nil ||
			budget.UsedByteCount() != baseline+held || fixture.parent.ctx.Err() != nil || fixture.update.client.Load() != fixture.selected ||
			time.Since(before) != 0 {
			t.Fatalf("unfunded metadata acquired serializer ownership: provider=%d unwritten=%d owner=%p budget=%d held=%d elapsed=%s",
				providerPackets.Load(), unwrittenRefusals.Load(), group.collapseOwner, budget.UsedByteCount(), held, time.Since(before))
		}
		budget.Release(held)
		held = 0
		count, membership = offer()
		synctest.Wait()
		if count != 1 || !slices.Equal(membership, want) || providerPackets.Load() != 1 || unwrittenRefusals.Load() != 1 ||
			budget.UsedByteCount() != baseline || time.Since(before) != 0 {
			t.Fatalf("metadata capacity retry changed exact owners: count=%d membership=%v provider=%d unwritten=%d budget=%d elapsed=%s",
				count, membership, providerPackets.Load(), unwrittenRefusals.Load(), budget.UsedByteCount(), time.Since(before))
		}
	})
}

// An ordinary source-queue refusal retains no delivery or collapse promise
// for the pure acknowledgement. The next native read retries independently.
func TestNativeAckCompactionQueueRefusalThenRetry(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture, data, ack := nativeAckPartitionCoveredFixture(t)
		providerPackets := watchNativePureAckProvider(t, fixture)
		calls := 0
		fixture.selected.sendGroupForTest = func(group *parsedPacketGroup, timeout time.Duration, ackRequired bool) (bool, error) {
			calls++
			if len(group.packets) != 1 || !group.packets[0].pureTcpAck() || timeout != 0 || !ackRequired {
				t.Error("queue refusal received covered data or a changed admission policy")
			}
			return false, nil
		}
		templates := nativeAckPartitionThirtyTwo(data, ack)
		offer := func() (int, []bool) {
			membership := make([]bool, len(templates))
			count := fixture.parent.SendNativePacketBatchWithResults(SourceId(fixture.sender.ClientId()),
				fixture.parent.provideMode, nativeAckPartitionOwnPackets(t, fixture, templates...), 5*time.Second, membership)
			return count, membership
		}
		before, drops := time.Now(), fixture.parent.TcpCollapseDropCount()
		count, membership := offer()
		synctest.Wait()
		if count != 0 || slices.Contains(membership, true) || calls != 1 || providerPackets.Load() != 0 ||
			fixture.parent.TcpCollapseDropCount() != drops+31 || fixture.update.client.Load() != fixture.selected || time.Since(before) != 0 {
			t.Fatalf("queue refusal changed native custody: count=%d membership=%v calls=%d provider=%d drops=%d elapsed=%s",
				count, membership, calls, providerPackets.Load(), fixture.parent.TcpCollapseDropCount()-drops, time.Since(before))
		}
		fixture.selected.sendGroupForTest = nil
		count, membership = offer()
		synctest.Wait()
		want := make([]bool, 32)
		want[31] = true
		if count != 1 || !slices.Equal(membership, want) || providerPackets.Load() != 1 ||
			fixture.parent.TcpCollapseDropCount() != drops+62 || time.Since(before) != 0 {
			t.Fatalf("queue refusal prevented exact fresh-read recovery: count=%d membership=%v provider=%d drops=%d elapsed=%s",
				count, membership, providerPackets.Load(), fixture.parent.TcpCollapseDropCount()-drops, time.Since(before))
		}
	})
}

// Dequeue and the actual provider acknowledgement may beat the enqueue
// observer; metadata credit survives until that producer returns.
func TestNativeAckCompactionCallbackBeforeReturnKeepsCharge(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture, data, ack := nativeAckPartitionCoveredFixture(t)
		providerPackets := watchNativePureAckProvider(t, fixture)
		budget := fixture.sender.settings.SendBufferSettings.ResendQueueBudget
		baseline := budget.UsedByteCount()
		entered, release := make(chan struct{}), make(chan struct{})
		var enterOnce, releaseOnce sync.Once
		resume := func() { releaseOnce.Do(func() { close(release) }) }
		fixture.sender.settings.SendBufferSettings.beforeGroupAdmissionForTest = func() {
			enterOnce.Do(func() { close(entered) })
			select {
			case <-release:
			case <-fixture.ctx.Done():
			}
		}
		var compacted atomic.Pointer[parsedPacketGroup]
		fixture.parent.beforePacketGroupGateForTest = func(_ *multiClientChannelUpdate, _ *multiClientChannel, group *parsedPacketGroup) {
			if group.completionFlags.Load()&groupCompletionCompactedMetadata != 0 {
				compacted.Store(group)
			}
		}
		templates := nativeAckPartitionThirtyTwo(data, ack)
		packets := nativeAckPartitionOwnPackets(t, fixture, templates...)
		membership := make([]bool, len(packets))
		result, done := make(chan int, 1), make(chan struct{})
		t.Cleanup(func() { resume(); fixture.cancel(); <-done })
		before := time.Now()
		go func() {
			defer close(done)
			result <- fixture.parent.SendNativePacketBatchWithResults(SourceId(fixture.sender.ClientId()),
				fixture.parent.provideMode, packets, 5*time.Second, membership)
		}()
		synctest.Wait()
		select {
		case <-entered:
		default:
			t.Fatal("compacted source missed the irreversible enqueue observer barrier")
		}
		group := compacted.Load()
		if group == nil {
			t.Fatal("callback inversion did not compact its covered-data group")
		}
		fixture.update.stateLock.Lock()
		var retainedCharge ByteCount
		if group.collapseOwner != nil {
			retainedCharge = group.collapseOwner.budgetBytes
		}
		fixture.update.stateLock.Unlock()
		flags := group.completionFlags.Load()
		select {
		case <-done:
			t.Fatal("producer crossed its held public-return boundary")
		default:
		}
		if providerPackets.Load() != 1 || flags&groupCompletionTerminal == 0 || flags&groupCompletionReturned != 0 ||
			flags&groupCompletionCompactedMetadata == 0 || retainedCharge < nativeAckPartitionMetadataLowerBound() ||
			budget.UsedByteCount() < baseline+retainedCharge || time.Since(before) != 0 {
			t.Fatalf("callback released compacted metadata before return: provider=%d flags=%x charge=%d budget=%d baseline=%d elapsed=%s",
				providerPackets.Load(), flags, retainedCharge, budget.UsedByteCount(), baseline, time.Since(before))
		}
		resume()
		synctest.Wait()
		select {
		case count := <-result:
			want := make([]bool, 32)
			want[31] = true
			if count != 1 || !slices.Equal(membership, want) {
				t.Fatalf("late return changed original indexes: count=%d membership=%v", count, membership)
			}
		default:
			t.Fatal("released producer did not finish its native owner")
		}
		if budget.UsedByteCount() != baseline || group.completionFlags.Load()&groupCompletionCompactedMetadata == 0 || time.Since(before) != 0 {
			t.Fatal("joined return did not release exactly one full metadata charge")
		}
	})
}

// Fresh data follows the unchanged complete-group admission and actual socket
// write, with the following pure acknowledgement kept in that same group.
func TestNativeAckCompactionLeavesFreshDataGroupWhole(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture, _, ack := nativeAckPartitionCoveredFixture(t)
		path := groupRecoveryParsed(t, ack).ipPath
		payload := []byte("next")
		data := groupRecoveryPacket(path.SequenceNumber, path.AckSequenceNumber, path.TcpWindowSize, tcpFlagAck, payload)
		nextAck := groupRecoveryPacket(path.SequenceNumber+uint32(len(payload)), path.AckSequenceNumber, path.TcpWindowSize, tcpFlagAck, nil)
		done, uploaded := make(chan struct{}), make(chan []byte, 1)
		t.Cleanup(func() { fixture.close(); <-done })
		go func() {
			defer close(done)
			received := make([]byte, len(payload))
			if _, err := io.ReadFull(fixture.origin, received); err == nil {
				uploaded <- received
			}
		}()
		var admitted atomic.Pointer[parsedPacketGroup]
		fixture.sender.settings.SendBufferSettings.afterGroupAdmissionForTest = func(target sendGroupAdmissionTarget) {
			admitted.Store(target.(*parsedPacketGroup))
		}
		before, drops := time.Now(), fixture.parent.TcpCollapseDropCount()
		membership := []bool{false, false}
		count := fixture.parent.SendNativePacketBatchWithResults(SourceId(fixture.sender.ClientId()),
			fixture.parent.provideMode, nativeAckPartitionOwnPackets(t, fixture, data, nextAck), 5*time.Second, membership)
		synctest.Wait()
		group := admitted.Load()
		if count != 2 || !slices.Equal(membership, []bool{true, true}) || group == nil || len(group.packets) != 2 ||
			group.completionFlags.Load()&groupCompletionCompactedMetadata != 0 || fixture.parent.TcpCollapseDropCount() != drops || time.Since(before) != 0 {
			t.Fatalf("fresh data group changed admission: count=%d membership=%v group=%p drops=%d elapsed=%s",
				count, membership, group, fixture.parent.TcpCollapseDropCount()-drops, time.Since(before))
		}
		select {
		case actual := <-uploaded:
			if !bytes.Equal(actual, payload) {
				t.Fatalf("fresh data changed actual socket bytes: %q", actual)
			}
		default:
			t.Fatal("fresh data did not reach its original socket")
		}
	})
}

// A blocked data member must deny the whole policy group before any ack
// refinement; the original group is inspected once and refreshed once.
func checkNativeAckPartitionSecurityRefusal(t *testing.T, throughMux bool) {
	t.Helper()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture, data, ack := nativeAckPartitionCoveredFixture(t)
		policy := &groupTestSecurityPolicy{stats: DefaultSecurityPolicyStatsCollector()}
		fixture.parent.securityPolicy = policy
		providerPackets := watchNativePureAckProvider(t, fixture)
		path := groupRecoveryParsed(t, data).ipPath
		blocked := groupRecoveryPacket(path.SequenceNumber, path.AckSequenceNumber, path.TcpWindowSize, tcpFlagAck, []byte{0xff})
		packets := nativeAckPartitionOwnPackets(t, fixture, ack, ack, blocked)
		before, drops := time.Now(), fixture.parent.TcpCollapseDropCount()
		membership := []bool{true, true, true}
		count := 0
		if throughMux {
			mux := &UpgradeMux{mux: &IpMux{}}
			mux.SetUpstreamBatchClient(fixture.parent)
			count = mux.SendNativePacketBatch(SourceId(fixture.sender.ClientId()), fixture.parent.provideMode, packets, 5*time.Second)
		} else {
			count = fixture.parent.SendNativePacketBatchWithResults(SourceId(fixture.sender.ClientId()), fixture.parent.provideMode,
				packets, 5*time.Second, membership)
		}
		synctest.Wait()
		if count != 0 || !throughMux && slices.Contains(membership, true) || providerPackets.Load() != 0 ||
			policy.inspectCount.Load() != 3 || policy.refreshCount.Load() != 1 ||
			fixture.parent.TcpCollapseDropCount() != drops || fixture.update.client.Load() != fixture.selected || time.Since(before) != 0 {
			t.Fatalf("security refusal leaked partial acknowledgements: mux=%t count=%d membership=%v provider=%d inspect=%d refresh=%d elapsed=%s",
				throughMux, count, membership, providerPackets.Load(), policy.inspectCount.Load(), policy.refreshCount.Load(), time.Since(before))
		}
	})
}

// Native indexing preserves whole-group security refusal.
func TestNativeAckCompactionKeepsSecurityRefusalAtomic(t *testing.T) {
	checkNativeAckPartitionSecurityRefusal(t, false)
}

// The concrete mux capability cannot bypass upstream whole-group security.
func TestUpgradeMuxAckCompactionKeepsSecurityRefusalAtomic(t *testing.T) {
	checkNativeAckPartitionSecurityRefusal(t, true)
}

// The actual plaintext http classifier claims the entire mixed burst before
// its native upstream capability can inspect, retain or transmit any member.
func TestUpgradeMuxAckCompactionKeepsLocalHttpClaim(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture, data, ack := nativeAckPartitionCoveredFixture(t)
		policy := &groupTestSecurityPolicy{stats: DefaultSecurityPolicyStatsCollector()}
		fixture.parent.securityPolicy = policy
		providerPackets := watchNativePureAckProvider(t, fixture)
		path := *groupRecoveryParsed(t, data).ipPath
		path.DestinationPort = 80
		blocked := ipOosTcpPacketSequence(&path, tcpFlagAck, path.SequenceNumber, []byte{0xff})
		controlPath := *groupRecoveryParsed(t, ack).ipPath
		controlPath.DestinationPort = 80
		control := ipOosTcpPacketSequence(&controlPath, tcpFlagAck, controlPath.SequenceNumber, nil)
		mux := &UpgradeMux{mux: &IpMux{}, firstLoad: &firstLoadTimeline{}}
		mux.settings.Store(&UpgradeMuxSettings{Http: &HttpUpgradeSettings{Mode: HttpUpgradeBlock}})
		mux.mux.setOnSendGroup(mux.onSendGroup)
		mux.SetUpstreamBatchClient(fixture.parent)
		before := time.Now()
		count := mux.SendNativePacketBatch(SourceId(fixture.sender.ClientId()), fixture.parent.provideMode,
			nativeAckPartitionOwnPackets(t, fixture, control, blocked, control), 5*time.Second)
		synctest.Wait()
		if count != 3 || policy.inspectCount.Load() != 0 || providerPackets.Load() != 0 || time.Since(before) != 0 {
			t.Fatalf("local HTTP claim leaked mixed members: count=%d policy=%d provider=%d elapsed=%s",
				count, policy.inspectCount.Load(), providerPackets.Load(), time.Since(before))
		}
	})
}

// An unresolved renewal remains one source transaction. Source refusal keeps
// the incumbent, and one later accepted source replaces its complete cohort.
func checkNativeAckPartitionSynRenewal(t *testing.T, throughMux bool) {
	t.Helper()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture := newAffinitySynQueueFixture(t, 4, func(settings *ClientSettings) {
			settings.SendBufferSettings.SequenceBufferSize = 0
		})
		old := fixture.initial(t)
		fixture.receive(t, fixture.first.selected, tcpFlagSyn|tcpFlagAck, 9000, 1001)
		fixture.raceOnly(fixture.second.selected)
		settings := ReliabilitySettingsFrom(fixture.parent.settings)
		settings.AffinityMaxTime = 0
		fixture.parent.SetReliabilitySettings(settings)
		syn := reraceTcpPacket(fixture.path, tcpFlagSyn, 2000, 0, nil)
		ack := reraceTcpPacket(fixture.path, tcpFlagAck, 2001, 9001, nil)
		var mux *UpgradeMux
		if throughMux {
			mux = &UpgradeMux{mux: &IpMux{}}
			mux.SetUpstreamBatchClient(fixture.parent)
		}
		offer := func() (int, []bool) {
			templates := [][]byte{syn, ack}
			packets := make([][]byte, len(templates))
			membership := []bool{true, true}
			for index, template := range templates {
				packets[index] = MessagePoolCopy(template)
				fixture.first.witnesses = append(fixture.first.witnesses, MessagePoolShareReadOnly(packets[index]))
			}
			if throughMux {
				return mux.SendNativePacketBatch(fixture.first.source, fixture.parent.provideMode, packets, 5*time.Second), nil
			}
			return fixture.parent.SendNativePacketBatchWithResults(fixture.first.source, fixture.parent.provideMode,
				packets, 5*time.Second, membership), membership
		}
		selection, generation := old.client.loadSelection(), old.synGenerationNumber
		before := time.Now()
		count, membership := offer()
		if count != 0 || !throughMux && slices.Contains(membership, true) ||
			fixture.parent.receiveUpdate(fixture.path) != old || old.IsDone() || old.client.loadSelection() != selection ||
			old.synGenerationNumber != generation || old.affinitySources.closed || old.affinityDelivery.Load() != 0 ||
			len(fixture.parent.affinityTransitions) != 0 || time.Since(before) != 0 {
			t.Fatalf("new SYN refusal leaked its acknowledgement cohort: mux=%t count=%d membership=%v elapsed=%s",
				throughMux, count, membership, time.Since(before))
		}
		fixture.second.requireParked()
		fixture.second.unpark()
		synctest.Wait()
		count, membership = offer()
		if count != 2 || !throughMux && !slices.Equal(membership, []bool{true, true}) {
			t.Fatalf("renewal retry split source custody: mux=%t count=%d membership=%v", throughMux, count, membership)
		}
		fixture.second.acknowledgeAll(syn, ack)
		current := fixture.parent.receiveUpdate(fixture.path)
		if current == old || current.client.Load() != fixture.second.selected || !old.IsDone() || time.Since(before) != 0 {
			t.Fatal("accepted renewal failed to replace exactly its original incumbent")
		}
	})
}

// Native indexing preserves a refused new handshake and its later retry.
func TestNativeAckCompactionKeepsSynRenewalAtomic(t *testing.T) {
	checkNativeAckPartitionSynRenewal(t, false)
}

// Concrete mux native routing preserves that same handshake transaction.
func TestUpgradeMuxAckCompactionKeepsSynRenewalAtomic(t *testing.T) {
	checkNativeAckPartitionSynRenewal(t, true)
}

// Closing from a refused compacted offer must join all original ownership.
func TestNativeAckCompactionCancellationReturnsEveryOriginal(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture, data, ack := nativeAckPartitionCoveredFixture(t)
		calls := 0
		fixture.parent.beforePacketGroupGateForTest = func(_ *multiClientChannelUpdate, _ *multiClientChannel, group *parsedPacketGroup) {
			if group.completionFlags.Load()&groupCompletionCompactedMetadata != 0 {
				calls++
				fixture.cancel()
			}
		}
		templates := nativeAckPartitionThirtyTwo(data, ack)
		membership := make([]bool, len(templates))
		count := fixture.parent.SendNativePacketBatchWithResults(SourceId(fixture.sender.ClientId()), fixture.parent.provideMode,
			nativeAckPartitionOwnPackets(t, fixture, templates...), 5*time.Second, membership)
		synctest.Wait()
		if count != 0 || slices.Contains(membership, true) || calls != 1 {
			t.Fatalf("canceled compaction retained original owners: count=%d membership=%v calls=%d", count, membership, calls)
		}
		if err := fixture.parent.CloseAndWait(context.Background()); err != nil {
			t.Fatal(err)
		}
	})
}

// The atomic lease gate supplies the same typed native receipt as the earlier
// selection gate, while connection controls keep complete refusal accounting.
func TestNativeAckCompactionLeaseRefusalReceipt(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture, data, ack := nativeAckPartitionCoveredFixture(t)
		dataPacket, ackPacket := groupRecoveryParsed(t, data), groupRecoveryParsed(t, ack)
		group := &parsedPacketGroup{
			packets: []parsedPacket{*dataPacket, *ackPacket},
			ipPath:  dataPacket.ipPath,
		}
		admission := ipPacketGroupAdmission{nativeIngress: true, partitionPureAcks: true}
		drops := fixture.parent.TcpCollapseDropCount()
		if group.prepareCollapseAdmissionWithRefusal(fixture.update, &admission, fixture.parent) ||
			!admission.collapseRejected || group.collapseOwner != nil || fixture.parent.TcpCollapseDropCount() != drops {
			t.Fatal("atomic lease refusal lost the native owner receipt or prematurely counted its ack")
		}
		synPacket := groupRecoveryParsed(t, groupRecoveryPacket(fixture.initial, 0, fixture.window, tcpFlagSyn, nil))
		group = &parsedPacketGroup{
			packets: []parsedPacket{*synPacket, *ackPacket},
			ipPath:  synPacket.ipPath,
		}
		admission = ipPacketGroupAdmission{nativeIngress: true, partitionPureAcks: true}
		if group.prepareCollapseAdmissionWithRefusal(fixture.update, &admission, fixture.parent) ||
			admission.collapseRejected || fixture.parent.TcpCollapseDropCount() != drops+2 {
			t.Fatal("atomic lease refusal partitioned a connection-control cohort")
		}
	})
}

// Withdrawing the selected owner between collapse and ACK admission enters
// actual provider selection. Its race copy owns one parsed member but retains
// all 32 original path slots until actual provider return selects the winner.
func TestNativeAckCompactionRaceCloneKeepsOriginalPathCharge(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture, data, ack := nativeAckPartitionCoveredFixture(t)
		providerPackets := watchNativePureAckProvider(t, fixture)
		other := newGroupDispositionQueueFixture(t, nil, func(settings *ClientSettings) {
			settings.SendBufferSettings.SequenceBufferSize = 0
		})
		other.selected.settings = fixture.parent.settings
		fixture.parent.settings.MultiRaceClientCount = 2
		fixture.parent.settings.MultiRaceDegradedClientCount = 2
		fixture.parent.settings.MultiRaceSetOnNoResponseTimeout = time.Second
		fixture.parent.settings.MultiRaceSetOnResponseTimeout = time.Second
		fixture.parent.settings.MultiRaceClientSentPacketMaxCount = 64
		fixture.parent.settings.MultiRaceClientEarlyCompleteFraction = 1
		fixture.parent.backendDegradedForTest = func() bool { return false }
		fixture.parent.groupRaceCandidatesForTest = func(*parsedPacketGroup) []*multiClientChannel {
			return []*multiClientChannel{fixture.selected, other.selected}
		}
		var withdrawals atomic.Int64
		fixture.parent.beforePacketGroupGateForTest = func(update *multiClientChannelUpdate, _ *multiClientChannel, group *parsedPacketGroup) {
			if group.completionFlags.Load()&groupCompletionCompactedMetadata == 0 || withdrawals.Load() != 0 {
				return
			}
			cleared := false
			func() {
				update.stateLock.Lock()
				defer update.stateLock.Unlock()
				selection := update.client.loadSelection()
				cleared = selection != nil && update.client.clearSelection(selection)
			}()
			if !cleared {
				t.Error("race control could not withdraw its actual selected owner")
			}
			withdrawals.Add(1)
		}
		entered, release := make(chan struct{}), make(chan struct{})
		var enterOnce, releaseOnce sync.Once
		resume := func() { releaseOnce.Do(func() { close(release) }) }
		t.Cleanup(resume)
		fixture.sender.settings.SendBufferSettings.beforeGroupRetainForTest = func() {
			enterOnce.Do(func() { close(entered) })
			select {
			case <-release:
			case <-fixture.ctx.Done():
			}
		}
		var admitted atomic.Pointer[parsedPacketGroup]
		fixture.sender.settings.SendBufferSettings.afterGroupAdmissionForTest = func(target sendGroupAdmissionTarget) {
			admitted.Store(target.(*parsedPacketGroup))
		}
		var wireCharge atomic.Int64
		fixture.sender.settings.SendBufferSettings.TransferWireMessageObserver = func(TransferWireMessageObservation) {
			group := admitted.Load()
			if group == nil {
				t.Error("raced route write preceded admission metadata")
				return
			}
			fixture.update.stateLock.Lock()
			var charge ByteCount
			if group.collapseOwner != nil {
				charge = group.collapseOwner.budgetBytes
			}
			fixture.update.stateLock.Unlock()
			wireCharge.Store(int64(charge))
		}
		budget := fixture.sender.settings.SendBufferSettings.ResendQueueBudget
		baseline := budget.UsedByteCount()
		templates := nativeAckPartitionThirtyTwo(data, ack)
		membership := make([]bool, len(templates))
		before := time.Now()
		count := fixture.parent.SendNativePacketBatchWithResults(SourceId(fixture.sender.ClientId()), fixture.parent.provideMode,
			nativeAckPartitionOwnPackets(t, fixture, templates...), 5*time.Second, membership)
		synctest.Wait()
		other.requireParked()
		select {
		case <-entered:
		default:
			t.Fatal("actual provider race never reached its pre-reservation boundary")
		}
		group := admitted.Load()
		want := make([]bool, 32)
		want[31] = true
		if count != 1 || !slices.Equal(membership, want) || withdrawals.Load() != 1 || group == nil ||
			len(group.packets) != 1 || cap(group.packets) != 1 || group.retainedIpPathCount != 32 ||
			group.completionFlags.Load()&(groupCompletionCandidate|groupCompletionCompactedMetadata) != groupCompletionCandidate|groupCompletionCompactedMetadata ||
			group.collapseOwner != nil || budget.UsedByteCount() != baseline || other.client.IsDone() || providerPackets.Load() != 0 {
			t.Fatalf("provider race lost original path ownership: count=%d membership=%v withdrawals=%d group=%p budget=%d baseline=%d provider=%d",
				count, membership, withdrawals.Load(), group, budget.UsedByteCount(), baseline, providerPackets.Load())
		}
		round := func(bytes ByteCount) ByteCount { return (bytes + 63) / 64 * 64 }
		minimumGraph := round(ByteCount(unsafe.Sizeof(parsedPacketGroup{}))) +
			round(ByteCount(unsafe.Sizeof(parsedPacket{}))) +
			32*round(ByteCount(unsafe.Sizeof(IpPath{})))
		charge := group.sendGroupMemoryByteCount()
		if charge < minimumGraph {
			t.Fatalf("race clone charged one path for a 32-path backing: charge=%d minimum=%d", charge, minimumGraph)
		}
		resume()
		synctest.Wait()
		fixture.update.stateLock.Lock()
		var retainedCharge ByteCount
		if group.collapseOwner != nil {
			retainedCharge = group.collapseOwner.budgetBytes
		}
		pendingRace := fixture.update.race != nil && fixture.update.client.Load() == nil
		fixture.update.stateLock.Unlock()
		flags := group.completionFlags.Load()
		if providerPackets.Load() != 1 || !pendingRace || flags&(groupCompletionReturned|groupCompletionTerminal) != groupCompletionReturned|groupCompletionTerminal ||
			retainedCharge != charge || ByteCount(wireCharge.Load()) != charge || budget.UsedByteCount() < baseline+charge || time.Since(before) != 0 {
			t.Fatalf("acknowledged race clone released original metadata before promotion: provider=%d pending_race=%t flags=%x retained=%d wire=%d want=%d budget=%d elapsed=%s",
				providerPackets.Load(), pendingRace, flags, retainedCharge, wireCharge.Load(), charge, budget.UsedByteCount(), time.Since(before))
		}

		// Actual provider socket data supplies response evidence; the ordinary
		// race's early-completion edge selects that same live provider.
		body := "race"
		if count, err := fixture.origin.Write([]byte(body)); err != nil || count != len(body) {
			t.Fatalf("actual provider race response: count=%d err=%v", count, err)
		}
		synctest.Wait()
		fixture.update.stateLock.Lock()
		remainingCharge := group.collapseOwner.budgetBytes
		selected, raceDone := fixture.update.client.Load(), fixture.update.race == nil
		fixture.update.stateLock.Unlock()
		fixture.consumer.stateLock.Lock()
		received, frontier := fixture.consumer.received, fixture.consumer.next
		fixture.consumer.stateLock.Unlock()
		if selected != fixture.selected || !raceDone || received != body || remainingCharge != 0 ||
			budget.UsedByteCount() != baseline || time.Since(before) != 0 {
			t.Fatalf("actual response failed to release the selected race graph: selected=%p race_done=%t received=%q charge=%d budget=%d elapsed=%s",
				selected, raceDone, received, remainingCharge, budget.UsedByteCount(), time.Since(before))
		}
		ackPath := groupRecoveryParsed(t, ack).ipPath
		if !fixture.offer(groupRecoveryPacket(ackPath.SequenceNumber, frontier, ackPath.TcpWindowSize, tcpFlagAck, nil)) {
			t.Fatal("selected race's final actual return acknowledgement was refused")
		}
		synctest.Wait()
		sequence := fixture.providerSequence.Load()
		sequence.mutex.Lock()
		returnCharge := sequence.returnByteCount
		sequence.mutex.Unlock()
		if returnCharge != 0 || providerPackets.Load() != 2 || other.client.IsDone() || time.Since(before) != 0 {
			t.Fatal("race clone cleanup lost return custody or an unrelated refused source")
		}
	})
}

// Numeric receipt limits are tested without allocating enormous inputs.
// Rejection is decided before any packet or path slot can be compacted.
func TestNativeAckCompactionPathReceiptChecksRepresentableCapacity(t *testing.T) {
	for _, count := range []int{-1, 0, 1, 31, 32} {
		receipt, valid := nativeAckRetainedPathCount(count)
		if valid != (count >= 0) || valid && uint64(receipt) != uint64(count) {
			t.Fatalf("capacity receipt count=%d receipt=%d valid=%t", count, receipt, valid)
		}
	}
	largest := uint64(^uint32(0))
	if uint64(^uint(0)>>1) <= largest {
		return
	}
	if receipt, valid := nativeAckRetainedPathCount(int(largest)); !valid || uint64(receipt) != largest {
		t.Fatal("largest exact capacity lost its numeric receipt")
	}
	if receipt, valid := nativeAckRetainedPathCount(int(largest + 1)); valid || receipt != 0 {
		t.Fatal("unrepresentable capacity silently truncated its ownership receipt")
	}
}

// A real readerless source first refuses after completing preparation. Restored
// proof then rejects the full group at the late selected-client gate; one fresh
// preparation reuses that descriptor for its pure acknowledgement only.
func checkNativeAckCompactionPreparedRefusal(t *testing.T, throughMux bool) {
	t.Helper()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture, data, ack := nativeAckPartitionCoveredFixture(t)
		providerPackets := watchNativePureAckProvider(t, fixture)
		budgetB := fixture.sender.settings.SendBufferSettings.ResendQueueBudget
		baselineB := budgetB.UsedByteCount()
		budgetA := NewTransferMemoryBudget(128 * 1024)
		var refusingOwner atomic.Pointer[groupDispositionQueueFixture]
		var original atomic.Pointer[parsedPacketGroup]
		var preparedA, settledA, compactedAfterRefusal, preparedB atomic.Bool
		var aRegistration atomic.Uint64
		var sourceCreates, enumerations, compactedGates, bAdmissions atomic.Int64
		var lifecycleLock sync.Mutex
		lifecyclePhases := map[uint64]uint8{}
		lifecycleProblem := false
		refusing := newGroupDispositionQueueFixture(t, budgetA, func(settings *ClientSettings) {
			settings.SendBufferSettings.SequenceBufferSize = 0
			previousObserver := settings.SendBufferSettings.SendPackLifecycleObserver
			settings.SendBufferSettings.SendPackLifecycleObserver = func(event SendPackLifecycleObservation) {
				owner := refusingOwner.Load()
				if owner != nil && event.DestinationId == owner.peer {
					func() {
						lifecycleLock.Lock()
						defer lifecycleLock.Unlock()
						bit := uint8(1) << uint8(event.Phase)
						if event.Token == 0 || lifecyclePhases[event.Token]&bit != 0 {
							lifecycleProblem = true
						}
						lifecyclePhases[event.Token] |= bit
						if event.Phase == SendPackLifecyclePhaseTerminal {
							admissionErr, valid := event.Err.(*SendPackAdmissionError)
							if !valid || admissionErr.Boundary != sendAdmissionHandoff.String() ||
								admissionErr.Timeout != 0 || admissionErr.OwnerTrackingOverflow {
								lifecycleProblem = true
							}
						}
					}()
				}
				previousObserver(event)
			}
			// SendBuffer captures this constructor setting. It runs after the
			// channel's actual completion preparation and before queue handoff.
			settings.SendBufferSettings.beforeCreateSendSequenceForTest = func(id sendSequenceId) {
				owner, group := refusingOwner.Load(), original.Load()
				if owner == nil || id.Destination != owner.peer {
					return
				}
				sourceCreates.Add(1)
				if group == nil {
					t.Error("actual refusing source lost its original descriptor")
					return
				}
				validPreparation, restored := false, false
				func() {
					fixture.update.stateLock.Lock()
					defer fixture.update.stateLock.Unlock()
					flags := group.completionFlags.Load()
					registration := group.affinitySourceRegistration
					entry := fixture.update.affinitySources.entryWithLock(int(registration&0xff) - 1)
					validPreparation = flags&groupCompletionTracked != 0 &&
						flags&(groupCompletionReturned|groupCompletionAdmitted|groupCompletionTerminal|groupCompletionCompactedMetadata) == 0 &&
						group.completionClient == owner.selected && group.collapseOwner == nil &&
						group.completionSelection == fixture.update.client.loadSelection() &&
						group.affinitySourceState == 0 &&
						registration != 0 && entry != nil && entry.epoch == registration>>8 && entry.pending == 1 &&
						fixture.update.sequenceClient == fixture.selected
					aRegistration.Store(registration)
					restored = fixture.update.client.CompareAndSwap(owner.selected, fixture.selected)
				}()
				preparedA.Store(validPreparation)
				if !validPreparation || !restored || budgetA.UsedByteCount() != 0 {
					t.Error("selection changed before real preparation or source A acquired retention")
				}
			}
		})
		refusing.selected.settings = fixture.parent.settings
		refusing.selected.healthy = true
		refusingOwner.Store(refusing)

		templates := nativeAckPartitionThirtyTwo(data, ack)
		packets := make([][]byte, len(templates))
		for index, template := range templates {
			packets[index] = MessagePoolCopy(template)
		}
		witnesses := groupTestPacketWitnesses(t, packets)
		// A witness keeps the pool root alive. Read its real reference count
		// under the pool's shard lock; the shared flag is deliberately sticky.
		witnessOwners := func(witness []byte) uint16 {
			for _, pool := range orderedMessagePools() {
				if cap(witness) != pool.size+MessagePoolMetaByteCount {
					continue
				}
				root := witness[:cap(witness)]
				shard, _ := pool.shardFor(root)
				shard.stateLock.Lock()
				owners := binary.BigEndian.Uint16(root[pool.size+10:])
				shard.stateLock.Unlock()
				return owners
			}
			return 0
		}
		enteredB, releaseB := make(chan struct{}), make(chan struct{})
		var enterOnce, releaseOnce sync.Once
		resumeB := func() { releaseOnce.Do(func() { close(releaseB) }) }
		t.Cleanup(func() {
			resumeB()
			fixture.close()
			if err := refusing.closeClient(context.Background()); err != nil {
				t.Error(err)
			}
			requireGroupTestWitnessesReleased(t, packets, witnesses)
		})
		fixture.sender.settings.SendBufferSettings.beforeGroupRetainForTest = func() {
			enterOnce.Do(func() { close(enteredB) })
			select {
			case <-releaseB:
			case <-fixture.ctx.Done():
			}
		}
		fixture.parent.beforePacketGroupGateForTest = func(_ *multiClientChannelUpdate, _ *multiClientChannel, group *parsedPacketGroup) {
			if len(group.packets) == len(templates) {
				if !original.CompareAndSwap(nil, group) && original.Load() != group {
					t.Error("late-refusal control replaced its original descriptor")
				}
				return
			}
			if group.completionFlags.Load()&groupCompletionCompactedMetadata == 0 {
				return
			}
			compactedGates.Add(1)
			valid := false
			func() {
				fixture.update.stateLock.Lock()
				defer fixture.update.stateLock.Unlock()
				flags := group.completionFlags.Load()
				valid = group == original.Load() && len(group.packets) == 1 && cap(group.packets) == 32 &&
					group.retainedIpPathCount == 32 &&
					flags&(groupCompletionTracked|groupCompletionReturned) == groupCompletionTracked|groupCompletionReturned &&
					flags&(groupCompletionAdmitted|groupCompletionTerminal) == 0 &&
					group.collapseOwner == nil && group.completionClient == refusing.selected &&
					group.affinitySourceRegistration == aRegistration.Load() &&
					group.affinitySourceState&(affinitySourceReturned|affinitySourceSettled) == affinitySourceReturned|affinitySourceSettled
			}()
			// A refused source never took these bytes, and B has not yet been
			// offered them. This verifies compaction preserves the original root.
			valid = valid && bytes.Equal(group.packets[0].packet, ack) &&
				&group.packets[0].packet[0] == &packets[len(packets)-1][0]
			compactedAfterRefusal.Store(valid)
			if !valid {
				t.Error("same descriptor did not retain its completed refusal state through compaction")
			}
		}
		fixture.parent.groupRaceCandidatesForTest = func(group *parsedPacketGroup) []*multiClientChannel {
			enumerations.Add(1)
			valid := false
			func() {
				fixture.update.stateLock.Lock()
				defer fixture.update.stateLock.Unlock()
				flags := group.completionFlags.Load()
				registration := group.affinitySourceRegistration
				entry := fixture.update.affinitySources.entryWithLock(int(registration&0xff) - 1)
				valid = group == original.Load() && len(group.packets) == len(templates) &&
					flags&(groupCompletionTracked|groupCompletionReturned) == groupCompletionTracked|groupCompletionReturned &&
					flags&(groupCompletionAdmitted|groupCompletionTerminal|groupCompletionCompactedMetadata) == 0 &&
					group.collapseOwner == nil && group.completionClient == refusing.selected && group.completionSelection == nil &&
					registration == aRegistration.Load() && entry != nil && entry.epoch == registration>>8 &&
					entry.pending == 0 && !entry.ambiguous && !entry.seen &&
					group.affinitySourceState&affinitySourceSettled != 0 &&
					fixture.update.client.Load() == fixture.selected && fixture.update.sequenceClient == fixture.selected &&
					fixture.update.sequenceCoversWithLock(&group.packets[0])
			}()
			for _, witness := range witnesses {
				valid = valid && witnessOwners(witness) == 2
			}
			settledA.Store(valid)
			if !valid || budgetA.UsedByteCount() != 0 || len(refusing.route) != 0 ||
				refusing.started.Load() != 1 || refusing.terminal.Load() != 0 {
				t.Error("late gate was reached before actual queue refusal settled caller custody")
			}
			return []*multiClientChannel{fixture.selected}
		}
		var admittedB atomic.Pointer[parsedPacketGroup]
		fixture.sender.settings.SendBufferSettings.afterGroupAdmissionForTest = func(target sendGroupAdmissionTarget) {
			group := target.(*parsedPacketGroup)
			bAdmissions.Add(1)
			valid := false
			func() {
				fixture.update.stateLock.Lock()
				defer fixture.update.stateLock.Unlock()
				flags := group.completionFlags.Load()
				valid = group == original.Load() && len(group.packets) == 1 && group.packets[0].pureTcpAck() &&
					group.completionClient == fixture.selected && group.affinitySourceRegistration != 0 &&
					group.affinitySourceRegistration != aRegistration.Load() &&
					flags&(groupCompletionTracked|groupCompletionAdmitted|groupCompletionCompactedMetadata) ==
						groupCompletionTracked|groupCompletionAdmitted|groupCompletionCompactedMetadata &&
					flags&(groupCompletionReturned|groupCompletionTerminal) == 0 &&
					group.affinitySourceState&affinitySourceAdmitted != 0 &&
					group.affinitySourceState&(affinitySourceReturned|affinitySourceTerminal|affinitySourceMaterialized|affinitySourceFailed|affinitySourceSettled) == 0
			}()
			preparedB.Store(valid)
			admittedB.Store(group)
			if !valid {
				t.Error("B reused A's completion flags or source registration instead of preparing afresh")
			}
		}
		var wireCharge atomic.Int64
		fixture.sender.settings.SendBufferSettings.TransferWireMessageObserver = func(TransferWireMessageObservation) {
			group := admittedB.Load()
			if group == nil {
				t.Error("B wrote before its exact admission receipt")
				return
			}
			fixture.update.stateLock.Lock()
			var charge ByteCount
			if group.collapseOwner != nil {
				charge = group.collapseOwner.budgetBytes
			}
			fixture.update.stateLock.Unlock()
			wireCharge.Store(int64(charge))
		}
		func() {
			fixture.update.stateLock.Lock()
			defer fixture.update.stateLock.Unlock()
			fixture.update.client.Store(refusing.selected)
		}()
		var membership []bool
		if !throughMux {
			membership = make([]bool, len(packets))
			for index := range membership {
				membership[index] = true
			}
		}
		before, drops, terminalsB := time.Now(), fixture.parent.TcpCollapseDropCount(), fixture.senderTerminals.Load()
		count := 0
		if throughMux {
			mux := &UpgradeMux{mux: &IpMux{}}
			mux.SetUpstreamBatchClient(fixture.parent)
			count = mux.SendNativePacketBatch(SourceId(fixture.sender.ClientId()), fixture.parent.provideMode, packets, 5*time.Second)
		} else {
			count = fixture.parent.SendNativePacketBatchWithResults(SourceId(fixture.sender.ClientId()), fixture.parent.provideMode,
				packets, 5*time.Second, membership)
		}
		synctest.Wait()
		refusing.requireParked()
		select {
		case <-enteredB:
		default:
			t.Fatal("late-refusal recovery did not reach B's actual reservation boundary")
		}
		want := make([]bool, 32)
		want[31] = true
		group := admittedB.Load()
		if group == nil {
			t.Fatal("prepared refusal never produced an actual B source owner")
		}
		if count != 1 || !throughMux && !slices.Equal(membership, want) ||
			sourceCreates.Load() != 1 || enumerations.Load() != 1 || compactedGates.Load() != 1 || bAdmissions.Load() != 1 ||
			!preparedA.Load() || !settledA.Load() || !compactedAfterRefusal.Load() || !preparedB.Load() ||
			group.collapseOwner != nil || budgetA.UsedByteCount() != 0 || budgetB.UsedByteCount() != baselineB ||
			len(refusing.route) != 0 || providerPackets.Load() != 0 ||
			fixture.parent.TcpCollapseDropCount() != drops+31 || time.Since(before) != 0 {
			t.Fatalf("PREPARED_REFUSAL_LOST_NATIVE_ACK_REOFFER: entry_has_indexed_results=%t count=%d membership=%v creates=%d enumerations=%d compacted_gates=%d B_admissions=%d prepared_A=%t settled_A=%t compacted=%t prepared_B=%t owner=%p budget_A=%d budget_B=%d provider=%d drops=%d elapsed=%s",
				!throughMux, count, membership, sourceCreates.Load(), enumerations.Load(), compactedGates.Load(), bAdmissions.Load(),
				preparedA.Load(), settledA.Load(), compactedAfterRefusal.Load(), preparedB.Load(), group.collapseOwner,
				budgetA.UsedByteCount(), budgetB.UsedByteCount(), providerPackets.Load(), fixture.parent.TcpCollapseDropCount()-drops, time.Since(before))
		}
		for index, witness := range witnesses {
			owners := witnessOwners(witness)
			// Wrapped frames may already have returned the original ack root.
			if owners != 1 && (index != len(witnesses)-1 || owners != 2) {
				t.Fatalf("late-refusal pre-serialization owner %d: references=%d", index, owners)
			}
		}
		lifecycleLock.Lock()
		expectedPhases := uint8(1)<<uint8(SendPackLifecyclePhaseStarted) |
			uint8(1)<<uint8(SendPackLifecyclePhaseFirstRouteWrite) | uint8(1)<<uint8(SendPackLifecyclePhaseTerminal)
		lifecycleComplete := len(lifecyclePhases) == 1 && !lifecycleProblem
		for _, phases := range lifecyclePhases {
			lifecycleComplete = lifecycleComplete && phases == expectedPhases
		}
		lifecycleLock.Unlock()
		if !lifecycleComplete || refusing.started.Load() != 1 || refusing.terminal.Load() != 1 {
			t.Fatal("actual A refusal did not publish each lifecycle phase exactly once")
		}
		charge := group.sendGroupMemoryByteCount()
		if charge < nativeAckPartitionMetadataLowerBound() {
			t.Fatal("prepared-refusal reuse lost the original full metadata charge")
		}
		resumeB()
		synctest.Wait()
		fixture.update.stateLock.Lock()
		remainingCharge := group.collapseOwner.budgetBytes
		current, sourceOwner := fixture.update.client.Load(), fixture.update.sequenceClient
		fixture.update.stateLock.Unlock()
		if providerPackets.Load() != 1 || fixture.senderTerminals.Load() != terminalsB+1 ||
			ByteCount(wireCharge.Load()) != charge || remainingCharge != 0 || budgetB.UsedByteCount() != baselineB ||
			budgetA.UsedByteCount() != 0 || current != fixture.selected || sourceOwner != fixture.selected ||
			refusing.client.IsDone() || fixture.parent.ctx.Err() != nil || time.Since(before) != 0 {
			t.Fatalf("prepared-refusal reoffer lost exact final ownership: provider=%d terminals=%d wire_charge=%d want=%d retained=%d budget_A=%d budget_B=%d selected=%p proof=%p elapsed=%s",
				providerPackets.Load(), fixture.senderTerminals.Load()-terminalsB, wireCharge.Load(), charge, remainingCharge,
				budgetA.UsedByteCount(), budgetB.UsedByteCount(), current, sourceOwner, time.Since(before))
		}
		for index, witness := range witnesses {
			if owners := witnessOwners(witness); owners != 1 {
				t.Fatalf("late-refusal terminal owner %d survived: references=%d", index, owners)
			}
		}
	})
}

// The indexed native path must reset a previously refused descriptor safely.
func TestNativeAckCompactionPreparedRefusalReoffersSameDescriptor(t *testing.T) {
	checkNativeAckCompactionPreparedRefusal(t, false)
}

// The concrete mux count-only bridge must preserve that same exact custody.
func TestUpgradeMuxAckCompactionPreparedRefusalReoffersSameDescriptor(t *testing.T) {
	checkNativeAckCompactionPreparedRefusal(t, true)
}
