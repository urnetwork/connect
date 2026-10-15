package connect

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

type legacyPriorityNoAckBenchmarkWriter struct{ MultiRouteWriter }

func (*legacyPriorityNoAckBenchmarkWriter) WriteDetailedWithTransport(_ context.Context, wire []byte, _ time.Duration) (bool, TransportType, error) {
	MessagePoolReturn(wire)
	return true, TransportTypeP2p, nil
}

// Measures the real immediate NoAck sender, including final-root encryption
// and ownership. Unlike the WebRTC route benchmark, this covers the metadata
// mark added above route publication. The sink is intentionally identical in
// baseline and candidate builds; no physical latency hides a hot-path cost.
func BenchmarkP2pLegacyPriorityNoAckSender(b *testing.B) {
	for _, encrypted := range []bool{false, true} {
		for _, size := range []int{60, 1280} {
			b.Run(fmt.Sprintf("encrypted=%t/payload=%d", encrypted, size), func(b *testing.B) {
				sequence := &SendSequence{
					ctx: context.Background(), client: &Client{clientId: NewId()},
					destination: NewId(), sequenceId: NewId(), sendBufferSettings: DefaultSendBufferSettings(),
				}
				if encrypted {
					sequence.session = &peerEncryptionSession{
						client: sequence.client, role: sequenceTlsRoleClient,
						establishedEpoch: &tlsHandshakeEpoch{peerIdentityVerified: true, derivedTlsCipher: newFrameCodecTestSequenceCipher(b)},
					}
				}
				snapshot := &noAckFastPathSnapshot{writer: &legacyPriorityNoAckBenchmarkWriter{}}
				frame := &protocol.Frame{MessageType: protocol.MessageType_IpIpPacketFromProvider, Raw: true}
				pack := &SendPack{Frame: frame}
				b.ReportAllocs()
				b.SetBytes(int64(size))
				for b.Loop() {
					frame.MessageBytes = MessagePoolGet(size)
					if !sequence.writeNoAckFastPath(snapshot, pack) {
						MessagePoolReturn(frame.MessageBytes)
						b.Fatal("immediate sender refused available route")
					}
				}
			})
		}
	}
}
