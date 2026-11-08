// Native retries may refresh timestamps and IPv4 identification without
// starting a new TCP attempt. Immutable paid caches keep that narrow lineage.
package connect

import (
	"encoding/binary"
	"net"
)

// Both complete SYNs must have valid checksums. Only TSVal, IPv4 ID and their
// checksum bytes may differ; tuple, ISN, flags, TSEcr, other options and payload
// remain exact. No borrowed bytes escape this allocation-free comparison.
func sameSynRetryMetadata(previous, next []byte) bool {
	if len(previous) != len(next) || len(previous) == 0 {
		return false
	}
	previousTcp, previousTimestamp, previousOk := synRetryMetadataOffsets(previous)
	nextTcp, nextTimestamp, nextOk := synRetryMetadataOffsets(next)
	if !previousOk || !nextOk || previousTcp != nextTcp || previousTimestamp != nextTimestamp || previous[0]>>4 != next[0]>>4 {
		return false
	}
	for index, value := range previous {
		if index == previousTcp+16 || index == previousTcp+17 ||
			previousTimestamp >= 0 && previousTimestamp <= index && index < previousTimestamp+4 ||
			previous[0]>>4 == 4 && (index == 4 || index == 5 || index == 10 || index == 11) {
			continue
		}
		if value != next[index] {
			return false
		}
	}
	return true
}

// Existing IP parsers reject incomplete/non-atomic fragments. The stricter
// bounded option walk rejects malformed and duplicate timestamp options.
func synRetryMetadataOffsets(packet []byte) (tcpOffset, timestampOffset int, valid bool) {
	if len(packet) == 0 {
		return 0, 0, false
	}
	var protocol ipProtocolNumber
	var source, destination net.IP
	var transport []byte
	var ok bool
	switch packet[0] >> 4 {
	case 4:
		protocol, source, destination, transport, ok = parseIpv4(packet)
		if !ok || int(binary.BigEndian.Uint16(packet[2:4])) != len(packet) {
			return 0, 0, false
		}
		tcpOffset = int(packet[0]&15) * 4
		if checksumFinish(checksumAdd(0, packet[:tcpOffset])) != 0 {
			return 0, 0, false
		}
	case 6:
		protocol, source, destination, transport, ok = parseIpv6(packet)
		if !ok || Ipv6HeaderSize+int(binary.BigEndian.Uint16(packet[4:6])) != len(packet) {
			return 0, 0, false
		}
		tcpOffset = len(packet) - len(transport)
	default:
		return 0, 0, false
	}
	if protocol != ipProtocolNumberTcp || len(transport) < TcpHeaderSizeWithoutExtensions ||
		transport[13]&(tcpFlagSyn|tcpFlagAck|tcpFlagFin|tcpFlagRst) != tcpFlagSyn ||
		transportChecksum(protocol, source, destination, transport) != 0 {
		return 0, 0, false
	}
	headerBytes := int(transport[12]>>4) * 4
	if headerBytes < TcpHeaderSizeWithoutExtensions || len(transport) < headerBytes {
		return 0, 0, false
	}
	timestampOffset = -1
	for index := TcpHeaderSizeWithoutExtensions; index < headerBytes; {
		kind := transport[index]
		if kind == 0 {
			break
		}
		if kind == 1 {
			index++
			continue
		}
		if headerBytes < index+2 {
			return 0, 0, false
		}
		size := int(transport[index+1])
		if size < 2 || headerBytes-index < size {
			return 0, 0, false
		}
		if kind == 8 {
			if size != 10 || timestampOffset != -1 {
				return 0, 0, false
			}
			timestampOffset = tcpOffset + index + 2
		}
		index += size
	}
	return tcpOffset, timestampOffset, true
}

// The flow lock protects both immutable caches. A captured service acquisition
// may follow only an unbroken metadata-only lineage; A->other bytes->A is not
// continuity even when its numeric ISN and discovery stamp happen to match.
func (self *multiClientChannelUpdate) currentSynRetryLineageWithLock(captured *tcpSynRetry) *tcpSynRetry {
	current := self.synRetry
	if current == captured {
		return current
	}
	if captured == nil || current == nil || captured.references == 0 || captured.update != self ||
		current.references == 0 || !current.validGenerationWithLock(self) || captured.retryLineage == 0 ||
		captured.retryLineage != current.retryLineage || captured.sequence != current.sequence ||
		captured.source != current.source || captured.provideMode != current.provideMode || captured.pin != current.pin {
		return nil
	}
	return current
}
