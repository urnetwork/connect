// Receive-silence evidence requires an acknowledged native response obligation.
// Transport completion, outstanding custody and offer telemetry stay separate.
package connect

import "time"

// One-byte flags reuse each existing bucket path map. Completion-only entries
// do not contribute to the independent offered-path reference counts.
type responsePathFlags uint8

const (
	responsePathOffered responsePathFlags = 1
	responsePathAcked   responsePathFlags = 2
)

// Empty ACK feedback and resets do not require another native packet. SYN,
// FIN, payload and non-TCP requests retain response expectations. An empty
// TCP segment with no response-bearing flags does not invent an obligation.
func packetExpectsNativeResponse(path *IpPath) bool {
	if path == nil {
		return false
	}
	if path.Protocol != IpProtocolTcp {
		return true
	}
	if path.Rst {
		return false
	}
	return path.Syn || path.Fin || 0 < path.TcpPayloadByteCount
}

// Scalar callbacks retain copied tuple values only, never the packet, borrowed
// IP backing, TCP reader workspace or caller context. Groups already own these
// parsed values through their terminal callback and need no extra owner.
type responsePacketPath struct {
	version int
	ip      Ip6Path
}

func newResponsePacketPath(path *IpPath) responsePacketPath {
	if !packetExpectsNativeResponse(path) {
		return responsePacketPath{}
	}
	switch path.Version {
	case 4:
		ip4 := path.ToIp4Path()
		response := responsePacketPath{version: 4, ip: Ip6Path{Protocol: ip4.Protocol, SourcePort: ip4.SourcePort, DestinationPort: ip4.DestinationPort, ServerName: ip4.ServerName}}
		copy(response.ip.SourceIp[:4], ip4.SourceIp[:])
		copy(response.ip.DestinationIp[:4], ip4.DestinationIp[:])
		return response
	case 6:
		return responsePacketPath{version: 6, ip: path.ToIp6Path()}
	default:
		return responsePacketPath{}
	}
}

// Called under the channel accounting lock, only for this exact packet's
// successful peer ACK. The current lastSendAckTime belongs to that completion;
// an older control receipt cannot lend either proof or a receive deadline.
func (self *multiClientChannel) recordResponseAckWithLock(bucket *multiClientEventBucket, path responsePacketPath) {
	switch path.version {
	case 4:
		ip4 := Ip4Path{Protocol: path.ip.Protocol, SourcePort: path.ip.SourcePort, DestinationPort: path.ip.DestinationPort, ServerName: path.ip.ServerName}
		copy(ip4.SourceIp[:], path.ip.SourceIp[:4])
		copy(ip4.DestinationIp[:], path.ip.DestinationIp[:4])
		if bucket.ip4Paths == nil {
			bucket.ip4Paths = map[Ip4Path]responsePathFlags{}
		}
		bucket.ip4Paths[ip4] |= responsePathAcked
	case 6:
		if bucket.ip6Paths == nil {
			bucket.ip6Paths = map[Ip6Path]responsePathFlags{}
		}
		bucket.ip6Paths[path.ip] |= responsePathAcked
	default:
		return
	}
	if bucket.responseAckTime.IsZero() {
		bucket.responseAckTime = self.lastSendAckTime
	}
}

// The grouped callback already retains parsed scalar metadata. Mixed groups
// inspect every member rather than borrowing the first packet's class.
func (self *multiClientChannel) recordGroupResponseAckWithLock(bucket *multiClientEventBucket, packets []parsedPacket) {
	for index := range packets {
		path := packets[index].ipPath
		if !packetExpectsNativeResponse(path) {
			continue
		}
		self.recordResponseAckWithLock(bucket, newResponsePacketPath(path))
	}
}

// Snapshot work uses temporary exact sets, never a resident or packet-path
// index. Each retained path is visited once, including large destination sets.
func responseAckDestinationCount(buckets []*multiClientEventBucket) int {
	var ip4DestinationKVs map[Ip4Path]bool
	var ip6DestinationKVs map[Ip6Path]bool
	for _, bucket := range buckets {
		for path, flags := range bucket.ip4Paths {
			if flags&responsePathAcked != 0 {
				if ip4DestinationKVs == nil {
					ip4DestinationKVs = map[Ip4Path]bool{}
				}
				ip4DestinationKVs[path.Destination()] = true
			}
		}
		for path, flags := range bucket.ip6Paths {
			if flags&responsePathAcked != 0 {
				if ip6DestinationKVs == nil {
					ip6DestinationKVs = map[Ip6Path]bool{}
				}
				ip6DestinationKVs[path.Destination()] = true
			}
		}
	}
	return len(ip4DestinationKVs) + len(ip6DestinationKVs)
}

// Older bare classifier/counter fixtures have no exact packet completion
// metadata. Real packet/group completions always provide known expectations,
// including a zero value for controls which require no native response.
func receiveExpectedSince(stats *clientWindowStats) time.Time {
	if stats.responseEvidenceKnown {
		return stats.firstResponseAckTime
	}
	return stats.firstSendNackTime
}

func receiveExpectedDestinationCount(stats *clientWindowStats) int {
	if stats.responseEvidenceKnown {
		return stats.responseDestinationCount
	}
	return stats.sendDestinationCount
}
