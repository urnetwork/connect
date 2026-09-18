package connect

import (
	"context"
	"reflect"
	"sync"

	"github.com/urnetwork/connect/protocol"
	"google.golang.org/protobuf/proto"
)

// NatMemoryPolicyError is permanent configuration incompatibility, not a
// capacity refusal. Retrying unchanged settings cannot make it admissible.
type NatMemoryPolicyError struct{}

func (*NatMemoryPolicyError) Error() string {
	return "custom provider security policy has no bounded memory contract"
}

var ErrNatMemoryPolicy = &NatMemoryPolicyError{}

const (
	natProviderSourceLimit             = 16
	natProviderFragmentBytes           = 8 * 1024
	natProviderFixedBytes    ByteCount = 480 * 1024
	natProviderSourceBytes   ByteCount = 4 * 1024
	natProviderControlBytes  ByteCount = 32 * 1024
	natSmtpFlowBytes         ByteCount = 160 * 1024
)

// A provider prepays 480 KiB + 4 KiB/source (at most 544 KiB):
// 128 KiB four fragment caches, geometric metadata and reconstruction;
// 96 KiB built-in DPI (64 flows), stats (32 destinations/result) and clones;
// 96 KiB workers/channels/registrations and primary/transient retirement maps;
// 64 KiB precharged synchronous TCP callback/item workspace (two 32-KiB slots);
// 32 KiB ingress ACK/RST decoding/grouping workspace (one nonblocking slot);
// 64 KiB allocator/map growth slack. The per-source row covers lifecycle,
// evidence, diagnostics, mode/priority maps and six leaf tombstones.
// SMTP, callback scratch and return roots are separately admitted dynamically.
// With the required 1-KiB stats registration per provider, two providers plus
// fallback/old/new NATs cost 1858 KiB, leaving 190 KiB of the shared 2-MiB
// child for useful traffic during generation overlap.
func natProviderMemoryByteCount(sourceCount int) ByteCount {
	return natProviderFixedBytes + ByteCount(sourceCount)*natProviderSourceBytes
}

func natProviderSourceCount(settings *RemoteUserNatProviderSettings) int {
	if settings != nil && settings.MaxSourceCount > 0 {
		return min(settings.MaxSourceCount, natProviderSourceLimit)
	}
	return natProviderSourceLimit
}

func natProviderPolicySupported(settings *RemoteUserNatProviderSettings) bool {
	if settings == nil || settings.SecurityPolicyGenerator == nil {
		return true
	}
	pointer := reflect.ValueOf(settings.SecurityPolicyGenerator).Pointer()
	return pointer == reflect.ValueOf(DefaultProviderSecurityPolicyWithStats).Pointer() ||
		pointer == reflect.ValueOf(DisableSecurityPolicyWithStats).Pointer()
}

func newNatProviderSecurityPolicy(ctx context.Context, settings *RemoteUserNatProviderSettings, bounded bool) SecurityPolicy {
	stats := DefaultSecurityPolicyStatsCollector()
	if !bounded {
		return settings.SecurityPolicyGenerator(ctx, stats)
	}
	stats.maxDestinationsPerResult = 32
	if settings.SecurityPolicyGenerator != nil && reflect.ValueOf(settings.SecurityPolicyGenerator).Pointer() ==
		reflect.ValueOf(DisableSecurityPolicyWithStats).Pointer() {
		return DisableSecurityPolicyWithStats(ctx, stats)
	}
	dmca := DefaultDmcaSecurityPolicySettings()
	dmca.MaxFlows = 64
	return Reverse(NewSecurityPolicy(ctx, DefaultCfaaSecurityPolicySettings(), dmca, DefaultWebStandardSettings(), stats))
}

func waitNatProviderSecurityPolicy(policy SecurityPolicy) {
	switch policy := policy.(type) {
	case *reverseSecurityPolicy:
		waitNatProviderSecurityPolicy(policy.policy)
	case *securityPolicy:
		if policy.dmca.runDone != nil {
			<-policy.dmca.runDone
		}
	}
}

// Called only after the provider's callbacks and detector worker have joined.
// The closed provider can remain referenced by SDK diagnostics without keeping
// its prepaid policy tables reachable after their fixed claim is returned.
func clearNatProviderSecurityPolicy(policy SecurityPolicy) {
	switch policy := policy.(type) {
	case *reverseSecurityPolicy:
		clearNatProviderSecurityPolicy(policy.policy)
	case *securityPolicy:
		for _, shard := range policy.dmca.shards {
			shard.mu.Lock()
			shard.flows = nil
			shard.mu.Unlock()
		}
	}
	stats := policy.Stats()
	stats.stateLock.Lock()
	stats.resultDestinationCounts = nil
	stats.stateLock.Unlock()
}

func (self *RemoteUserNatProvider) memoryBudget() *TransferMemoryBudget {
	return self.retainedMemoryBudget
}

// Saturation has one handoff per generation. It may call Close itself or wait
// for an SDK lock, so Close must not join it. Instead it retains the fixed
// envelope until both teardown and the handoff have finished. Admission is
// under stateLock, before Close can publish sourceLifecycleClosed.
func (self *RemoteUserNatProvider) retainSaturationCallbackWithLock(callback func()) {
	if callback != nil && self.memoryBudget() != nil {
		self.fixedMemoryOwners.Add(1)
	}
}

func (self *RemoteUserNatProvider) runSaturationCallback(callback func()) {
	if self.memoryBudget() != nil {
		defer self.releaseFixedMemoryOwner()
	}
	HandleError(callback)
}

func (self *RemoteUserNatProvider) releaseFixedMemoryOwner() {
	if self.memoryBudget() == nil || self.fixedMemoryOwners.Add(-1) == 0 {
		self.memory.release()
	}
}

// Start an externally captured callback before touching provider-owned maps.
// Close shuts this gate before releasing the fixed graph. Scratch admission
// also bounds simultaneous callback stacks and decoded/grouped packet owners.
func (self *RemoteUserNatProvider) startMemoryOperation(bytes ByteCount) (natMemoryReservation, bool) {
	if self.memoryOperations != nil && !self.memoryOperations.start() {
		return natMemoryReservation{}, false
	}
	memory, admitted := reserveNatMemory(self.memoryBudget(), bytes)
	if !admitted && self.memoryOperations != nil {
		self.memoryOperations.finish()
	}
	return memory, admitted
}

func (self *RemoteUserNatProvider) finishMemoryOperation(memory *natMemoryReservation) {
	memory.release()
	if self.memoryOperations != nil {
		self.memoryOperations.finish()
	}
}

// Ingress ACKs release socket replay owners, so their admission must not
// depend on those owners leaving data-budget capacity. The workspace is a
// prepaid partition of the provider's fixed claim, independent of both data
// admission and synchronous return callbacks. One small frame is decoded and
// dispatched at a time; mixed or large Packs cannot multiply this workspace.
func (self *RemoteUserNatProvider) receiveControlFrames(source TransferPath, frames []*protocol.Frame, peer Peer) {
	if self.ingressControlMemory == nil {
		return
	}
	for _, frame := range frames {
		if frame == nil || len(frame.MessageBytes) > 512 {
			continue
		}
		if source.IsControlSource() {
			if frame.MessageType != protocol.MessageType_TransferNetworkPeersUpdate {
				continue
			}
		} else if frame.MessageType != protocol.MessageType_IpIpPacketToProvider {
			continue
		}
		self.receiveControlFrame(source, frame, peer)
	}
}

func (self *RemoteUserNatProvider) receiveControlFrame(source TransferPath, frame *protocol.Frame, peer Peer) {
	if !self.memoryOperations.start() {
		return
	}
	memory, admitted := reserveNatMemory(self.ingressControlMemory, natProviderControlBytes)
	if !admitted {
		self.memoryOperations.finish()
		return
	}
	defer self.finishMemoryOperation(&memory)
	if source.IsControlSource() {
		self.retireDisconnectedSenders([]*protocol.Frame{frame})
		return
	}
	packet := frame.MessageBytes
	if !frame.Raw {
		var message protocol.IpPacketToProvider
		if err := proto.Unmarshal(frame.MessageBytes, &message); err != nil || message.IpPacket == nil {
			return
		}
		packet = message.IpPacket.PacketBytes
	}
	if !smallNatControlPacket(packet) {
		return
	}
	// Transfer may lend an ACK slice backed by an entire received Pack. The
	// NAT's prepaid control queue must own a small packet, not retain that
	// potentially large parent through a read-only share.
	packet = MessagePoolCopy(packet)
	defer MessagePoolReturn(packet)
	control := protocol.Frame{MessageType: protocol.MessageType_IpIpPacketToProvider, Raw: true, MessageBytes: packet}
	self.clientReceiveAdmitted(source, []*protocol.Frame{&control}, peer)
}

func smallNatControlPacket(packet []byte) bool {
	return len(packet) <= smallPacketPoolSize && natControlPackets([][]byte{packet[:len(packet):len(packet)]})
}

// Dedicated TCP return producers must still make progress when replay owners
// fill the NAT child. This is a partition of the fixed claim, not extra root
// capacity. A blocked producer borrows its already-charged NAT packet roots.
func (self *RemoteUserNatProvider) startReturnMemoryOperation(packets [][]byte, recovery receiveRecoveryMode) (natMemoryReservation, bool) {
	if self.tcpReturnMemory == nil || !recovery.waitsForProviderReturnAdmission() {
		return self.startMemoryOperation(providerPacketOperationBytes(packets))
	}
	if len(packets) > natMemoryBatchCount {
		return natMemoryReservation{}, false
	}
	bytes := ByteCount(0)
	for _, packet := range packets {
		bytes += natPacketMemoryByteCount(packet)
	}
	if bytes > kib(8) || !self.memoryOperations.start() {
		return natMemoryReservation{}, false
	}
	for {
		changed := self.tcpReturnMemory.CapacityNotify()
		if memory, ok := reserveNatMemory(self.tcpReturnMemory, kib(32)); ok {
			return memory, true
		}
		select {
		case <-self.ctx.Done():
			self.memoryOperations.finish()
			return natMemoryReservation{}, false
		case <-changed:
		}
	}
}

func providerPacketOperationBytes(packets [][]byte) ByteCount {
	bytes := kib(24)
	for _, packet := range packets {
		bytes = addReceiveQueueByteCount(bytes, natPacketMemoryByteCount(packet)+512)
	}
	return bytes
}

func providerFrameOperationBytes(frames []*protocol.Frame) ByteCount {
	bytes := kib(24)
	for _, frame := range frames {
		if frame != nil {
			// Legacy protobuf decoding and the owned packet copy can coexist.
			root := retainedMessageCapacity(ByteCount(len(frame.MessageBytes)))
			bytes = addReceiveQueueByteCount(bytes, root+root+2048)
		}
	}
	return bytes
}

func (self *RemoteUserNatProvider) takeReturnItemForPackets(packets [][]byte, recovery receiveRecoveryMode) *providerReturnItem {
	if self.tcpReturnMemory != nil && recovery.waitsForProviderReturnAdmission() {
		// Synchronous item lifetime is nested within its prepaid callback slot.
		return self.takeReturnItem()
	}
	bytes := ByteCount(512 + 48*len(packets))
	for _, packet := range packets {
		bytes = addReceiveQueueByteCount(bytes, natPacketMemoryByteCount(packet))
	}
	memory, admitted := reserveNatMemory(self.memoryBudget(), bytes)
	if !admitted {
		return nil
	}
	item := self.takeReturnItem()
	item.memory = memory
	return item
}

// Async diagnostic work has its own admitted lifetime, not the callback's.
func (self *RemoteUserNatProvider) startMemoryWorker(run func()) bool {
	memory, admitted := self.startMemoryOperation(kib(8))
	if !admitted {
		return false
	}
	go HandleError(func() {
		defer self.finishMemoryOperation(&memory)
		run()
	})
	return true
}

type providerStatsSubscription struct {
	memory      natMemoryReservation
	gate        *lifecycleAdmission
	releaseOnce sync.Once
}

func (self *providerStatsSubscription) releaseIfDone() {
	select {
	case <-self.gate.Done():
		self.releaseOnce.Do(func() { self.memory.release() })
	default:
	}
}

func (self *providerStatsSubscription) close() {
	self.gate.close()
	self.releaseIfDone()
}
