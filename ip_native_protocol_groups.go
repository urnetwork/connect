// Native protocol runs preserve whole-group authorization and exact input receipts.
package connect

import "github.com/urnetwork/connect/protocol"

// Only established TCP groups split here. SYN keeps its atomic affinity
// transition; data-bearing controls remain DATA under the shared wire parser.
func (self *parsedPacketGroup) nativeProtocolPartitionCandidate() bool {
	hasControl, hasData := false, false
	for index := range self.packets {
		packet := &self.packets[index]
		if packet.ipPath == nil || packet.ipPath.Protocol != IpProtocolTcp || packet.ipPath.Syn {
			return false
		}
		if protocolClassPureTcpControl(packet.packet) {
			hasControl = true
		} else {
			hasData = true
		}
	}
	return hasControl && hasData
}

// Borrows the authorized parent descriptor and takes only accepted packet
// runs. The synchronous caller returns every refused original. Fresh path
// arrays prevent one retained run from keeping another run's descriptor graph.
func (self *RemoteUserNatMultiClient) sendNativeProtocolRuns(
	source TransferPath,
	provideMode protocol.ProvideMode,
	group *parsedPacketGroup,
	update *multiClientChannelUpdate,
	epoch uint64,
	admitted func(int),
) int {
	acceptedCount := 0
	for start := 0; start < len(group.packets); {
		control := protocolClassPureTcpControl(group.packets[start].packet)
		end := start + 1
		for end < len(group.packets) && protocolClassPureTcpControl(group.packets[end].packet) == control {
			end++
		}
		packets := make([]parsedPacket, end-start)
		paths := make([]IpPath, len(packets))
		byteCount := ByteCount(0)
		for index := range packets {
			original := &group.packets[start+index]
			paths[index] = *original.ipPath
			packets[index] = parsedPacket{
				packet: original.packet, ipPath: &paths[index], payload: original.payload,
				pin: original.pin, transportAttribution: original.transportAttribution,
			}
			byteCount += ByteCount(len(original.packet))
		}
		child := &parsedPacketGroup{
			packets: packets, ipPath: packets[0].ipPath, pin: group.pin, byteCount: byteCount,
			transportAttribution: newTransportPacketAttribution(self.packetStatsCounters, len(packets), byteCount),
		}
		admission := ipPacketGroupAdmission{nativeIngress: true, expectedUpdate: update, expectedEpoch: epoch}
		if self.sendParsedPacketGroupAttempt(source, provideMode, child, 0, true, &admission) {
			child.transportAttribution.admit()
			acceptedCount += len(packets)
			for index := start; index < end; index++ {
				admitted(index)
			}
		}
		start = end
	}
	return acceptedCount
}
