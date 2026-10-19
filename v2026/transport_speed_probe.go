package connect

import "encoding/binary"

// TransportSpeedTestProbeByteCount is the fixed H1 speed probe wire size.
const TransportSpeedTestProbeByteCount = 1024

// SetTransportSpeedTestProbeHeader writes the reserved non-Transfer prefix
// and active test identity. A zero protobuf tag cannot begin a Transfer frame.
func SetTransportSpeedTestProbeHeader(probe []byte, testId uint32) {
	probe[0] = 0
	binary.BigEndian.PutUint32(probe[1:5], testId)
}

// IsTransportSpeedTestProbe distinguishes echoed probes from interleaved user
// frames on both ends of H1. Only the active test's complete probes may echo.
func IsTransportSpeedTestProbe(message []byte, testId uint32) bool {
	return len(message) == TransportSpeedTestProbeByteCount && message[0] == 0 &&
		binary.BigEndian.Uint32(message[1:5]) == testId
}
