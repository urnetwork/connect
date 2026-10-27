// Native TUN admission makes one ready remote offer without adding a packet
// queue. Local protocol owners retain their existing bounded send timeout.
package connect

import (
	"time"

	"github.com/urnetwork/connect/protocol"
)

// An optional native-input capability; takes every packet on entry and borrows
// the outer slice. Refused remote TCP packets remain the source TCP's recovery
// responsibility, before Transfer has accepted or acknowledged any ownership.
type UserNatNativeBatchClient interface {
	UserNatClient
	SendNativePacketBatch(source TransferPath, provideMode protocol.ProvideMode, packets [][]byte, localTimeout time.Duration) int
}

// Takes the native burst; only remote admission is ready-only. Nested local
// overrides and the existing fragment/SMTP policy keep their original owners.
func (self *RemoteUserNatMultiClient) SendNativePacketBatch(source TransferPath, provideMode protocol.ProvideMode, packets [][]byte, localTimeout time.Duration) int {
	return self.sendPacketBatchWithPolicy(source, provideMode, packets, localTimeout, nil, true)
}

// Takes the native burst through the same DNS, HTTP and exact-flow classifier.
// A custom upstream without the capability retains its compatibility contract.
func (self *UpgradeMux) SendNativePacketBatch(source TransferPath, provideMode protocol.ProvideMode, packets [][]byte, localTimeout time.Duration) int {
	upstream := self.upstreamMultiClient.Load()
	if upstream == nil {
		return self.SendPacketBatch(source, provideMode, packets, localTimeout)
	}
	return self.mux.sendPacketBatchWithUpstreams(source, provideMode, packets, localTimeout,
		func(source TransferPath, provideMode protocol.ProvideMode, packet []byte, timeout time.Duration) bool {
			return upstream.sendPacketWithPolicy(source, provideMode, packet, timeout, true)
		}, nil, upstream.sendNativePacketGroupWithResults, true)
}

// Takes on success; a native offer cannot retain a retry continuation. Policy,
// collapse and encryption readiness are still evaluated on the actual offer.
func (self *RemoteUserNatMultiClient) sendNativePacketGroup(source TransferPath, provideMode protocol.ProvideMode, group *ipPacketGroup, localTimeout time.Duration) (success bool) {
	if group == nil {
		return false
	}
	admission := ipPacketGroupAdmission{nativeIngress: true}
	previousAdmission := group.batchAdmission
	group.batchAdmission = &admission
	defer func() {
		if admission.parsed != nil && admission.parsed.admissionObservations != nil {
			admission.parsed.admissionObservations.complete(success)
		}
		group.batchAdmission = previousAdmission
	}()
	return self.sendPacketGroup(source, provideMode, group, localTimeout)
}

// The group has already passed local-vs-remote policy classification. No new
// delivery authority is created by a failed ready offer or encryption wait.
func (self *RemoteUserNatMultiClient) sendNativeParsedPacketGroup(source TransferPath, provideMode protocol.ProvideMode, group *parsedPacketGroup) bool {
	admission := ipPacketGroupAdmission{nativeIngress: true}
	return self.sendParsedPacketGroupAttempt(source, provideMode, group, 0, true, &admission)
}

// The receipt is metadata, never a policy limit. Unrepresentable input keeps
// complete refusal before any original is compacted or ownership transfers.
func nativeAckRetainedPathCount(packetCapacity int) (uint32, bool) {
	if packetCapacity < 0 || uint64(packetCapacity) > uint64(^uint32(0)) {
		return 0, false
	}
	return uint32(packetCapacity), true
}

// Only redundant data groups can shed covered members for independent feedback.
// Connection controls keep their existing indivisible generation/close behavior.
func (self *parsedPacketGroup) nativeAckPartitionCandidate() bool {
	if _, valid := nativeAckRetainedPathCount(cap(self.packets)); !valid {
		return false
	}
	hasAck, hasData := false, false
	for index := range self.packets {
		packet := &self.packets[index]
		path := packet.ipPath
		if path == nil || path.Protocol != IpProtocolTcp || path.Syn || path.Fin || path.Rst {
			return false
		}
		hasAck = hasAck || packet.pureTcpAck()
		hasData = hasData || len(packet.payload) != 0
	}
	return hasAck && hasData
}

// Records a synchronous refusal before any queue acquired packet ownership.
// Eligible native owners account only the members they ultimately collapse.
func (self *RemoteUserNatMultiClient) recordCollapsedPacketGroup(group *parsedPacketGroup, admission *ipPacketGroupAdmission) {
	if admission != nil && admission.nativeIngress && admission.partitionPureAcks && group.nativeAckPartitionCandidate() {
		admission.collapseRejected = true
		return
	}
	self.tcpCollapseDropCount.Add(uint64(len(group.packets)))
}

// Takes every packet and borrows the descriptor and admitted callback. A failed
// complete-group collapse offer can retain its original parsed allocation,
// compact only pure acknowledgements, and make one ordinary ready offer.
// Original scalar metadata supplies exact caller indexes after byte handoff.
func (self *RemoteUserNatMultiClient) sendNativePacketGroupWithResults(
	source TransferPath,
	provideMode protocol.ProvideMode,
	group *ipPacketGroup,
	localTimeout time.Duration,
	admitted func(int),
) int {
	if group == nil {
		return 0
	}
	admission := ipPacketGroupAdmission{nativeIngress: true, partitionPureAcks: true}
	previousAdmission := group.batchAdmission
	group.batchAdmission = &admission
	allAccepted, acksAccepted := false, false
	memberIsAck := func(index int) bool {
		path := &group.ipPaths[index]
		return path.Protocol == IpProtocolTcp && path.Ack && !path.Syn && !path.Fin && !path.Rst &&
			path.TcpPayloadByteCount == 0 && len(group.payloads[index]) == 0
	}
	defer func() {
		if admission.parsed != nil && admission.parsed.admissionObservations != nil {
			admission.parsed.admissionObservations.complete(allAccepted || acksAccepted)
		}
		group.batchAdmission = previousAdmission
		if !allAccepted {
			for index, packet := range group.packets {
				if !acksAccepted || !memberIsAck(index) {
					MessagePoolReturn(packet)
				}
			}
		}
	}()
	report := func(onlyAcks bool) {
		if admitted != nil {
			for index := range group.packets {
				if !onlyAcks || memberIsAck(index) {
					admitted(index)
				}
			}
		}
	}
	if self.sendPacketGroup(source, provideMode, group, localTimeout) {
		allAccepted = true
		report(false)
		return len(group.packets)
	}
	if !admission.collapseRejected {
		return 0
	}

	// Every original remains owned here after the typed refusal. Distinct path
	// slots come from sendPacketGroup's one memberIpPaths allocation.
	parsed := admission.parsed
	pathCount, valid := nativeAckRetainedPathCount(cap(parsed.packets))
	if !valid {
		self.tcpCollapseDropCount.Add(uint64(len(group.packets)))
		return 0
	}
	packetCount, byteCount := 0, ByteCount(0)
	for index := range parsed.packets {
		packet := parsed.packets[index]
		if packet.pureTcpAck() {
			parsed.packets[packetCount] = packet
			packetCount++
			byteCount += ByteCount(len(packet.packet))
		} else {
			*packet.ipPath = IpPath{}
		}
	}
	clear(parsed.packets[packetCount:])
	parsed.retainedIpPathCount = pathCount
	// Keep parsed capacity and an explicit path receipt through race copies.
	parsed.packets = parsed.packets[:packetCount]
	self.tcpCollapseDropCount.Add(uint64(len(group.packets) - packetCount))
	if packetCount == 0 {
		return 0
	}
	parsed.ipPath, parsed.byteCount = parsed.packets[0].ipPath, byteCount
	parsed.completionFlags.Or(groupCompletionCompactedMetadata)
	if attribution := parsed.transportAttribution; attribution != nil {
		attribution.stateLock.Lock()
		attribution.packetCount, attribution.byteCount = int64(packetCount), byteCount
		attribution.stateLock.Unlock()
	}
	admission.partitionPureAcks, admission.collapseRejected = false, false
	if !self.sendParsedPacketGroupAttempt(source, provideMode, parsed, 0, false, &admission) {
		return 0
	}
	acksAccepted = true
	parsed.transportAttribution.admit()
	report(true)
	return packetCount
}
