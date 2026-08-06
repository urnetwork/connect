package connect

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

const providerColdPageTestFlowCount = 192

// TestProviderNatTargetUsesMinimalLosslessTcpQueue verifies an explicit
// provider target removes generic process-profile headroom from every flow
// without shrinking below one full advertised tcp window.
func TestProviderNatTargetUsesMinimalLosslessTcpQueue(t *testing.T) {
	defer SetMemoryBudget(0)

	cases := []struct {
		memoryBudgetByteCount ByteCount
	}{
		{memoryBudgetByteCount: 0},
		{memoryBudgetByteCount: 8 * 1024 * 1024},
		{memoryBudgetByteCount: 20 * 1024 * 1024},
		{memoryBudgetByteCount: 64 * 1024 * 1024},
	}
	for _, c := range cases {
		SetMemoryBudget(c.memoryBudgetByteCount)
		settings := DefaultProviderLocalUserNatSettingsWithMemoryTarget(4 * 1024 * 1024)
		tcpSettings := settings.TcpBufferSettings
		payloadByteCount := tcpSettings.Mtu - Ipv6HeaderSize - TcpHeaderSizeWithoutExtensions
		want := int(
			(tcpSettings.MaxWindowSize + uint32(payloadByteCount) - 1) /
				uint32(payloadByteCount),
		)
		if tcpSettings.SequenceBufferSize != want {
			t.Errorf(
				"budget=%d tcp queue=%d, want minimal max-window depth %d",
				c.memoryBudgetByteCount,
				tcpSettings.SequenceBufferSize,
				want,
			)
		}
		if tcpSettings.SequenceBufferSize*payloadByteCount < int(tcpSettings.MaxWindowSize) {
			t.Errorf(
				"budget=%d tcp queue %d x payload %d does not cover window %d",
				c.memoryBudgetByteCount,
				tcpSettings.SequenceBufferSize,
				payloadByteCount,
				tcpSettings.MaxWindowSize,
			)
		}
	}
}

// TestProviderNatMobileTargetCoversColdPageFanout pins the functional floor
// measured from a real 113-origin public page. The former 4 MiB provider
// profile admitted only 51 tcp flows and evicted live page handshakes.
func TestProviderNatMobileTargetCoversColdPageFanout(t *testing.T) {
	settings := DefaultProviderLocalUserNatSettingsWithMemoryTarget(4 * 1024 * 1024)
	if settings.TcpBufferSettings.UserLimit < providerColdPageTestFlowCount {
		t.Fatalf(
			"tcp user limit=%d, want at least %d cold-page flows",
			settings.TcpBufferSettings.UserLimit,
			providerColdPageTestFlowCount,
		)
	}
	if settings.TcpBufferSettings.GlobalLimit < 2*providerColdPageTestFlowCount {
		t.Fatalf(
			"tcp global limit=%d, want at least %d bounded two-user flows",
			settings.TcpBufferSettings.GlobalLimit,
			2*providerColdPageTestFlowCount,
		)
	}
	if settings.UdpBufferSettings.UserLimit < providerColdPageTestFlowCount {
		t.Fatalf(
			"udp user limit=%d, want at least %d cold-page flows",
			settings.UdpBufferSettings.UserLimit,
			providerColdPageTestFlowCount,
		)
	}
	if settings.UdpBufferSettings.GlobalLimit < 2*providerColdPageTestFlowCount {
		t.Fatalf(
			"udp global limit=%d, want at least %d bounded two-user flows",
			settings.UdpBufferSettings.GlobalLimit,
			2*providerColdPageTestFlowCount,
		)
	}
}

// TestProviderNatKeepsColdPageTcpFlows verifies the cap is applied to the
// actual per-source table, not only reported by the settings constructor.
func TestProviderNatKeepsColdPageTcpFlows(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	settings := DefaultProviderLocalUserNatSettingsWithMemoryTarget(4 * 1024 * 1024)
	settings.TcpBufferSettings.SequenceBufferSize = 1
	settings.TcpBufferSettings.DialContextSettings = &DialContextSettings{
		DialContext: func(dialCtx context.Context, network string, addr string) (net.Conn, error) {
			<-dialCtx.Done()
			return nil, dialCtx.Err()
		},
	}
	buffer := NewTcp4Buffer(
		ctx,
		func(
			source TransferPath,
			provideMode protocol.ProvideMode,
			ipPath *IpPath,
			packet []byte,
		) {
		},
		settings.TcpBufferSettings,
	)
	source := SourceId(NewId())
	for i := range providerColdPageTestFlowCount {
		packet := MessagePoolGet(Ipv4HeaderSizeWithoutExtensions + TcpHeaderSizeWithoutExtensions)
		tcp := &parsedTcp{
			sourceIp:        net.IPv4(10, 0, 0, 1).To4(),
			destinationIp:   net.IPv4(203, 0, 113, 7).To4(),
			sourcePort:      uint16(30000 + i),
			destinationPort: 443,
			syn:             true,
			seq:             uint32(1000 + i),
			windowSize:      65535,
		}
		success, err := buffer.send(source, protocol.ProvideMode_Network, tcp, -1, packet)
		if err != nil {
			MessagePoolReturn(packet)
			t.Fatalf("send flow %d: %v", i, err)
		}
		if !success {
			MessagePoolReturn(packet)
			t.Fatalf("flow %d was rejected", i)
		}
	}

	buffer.mutex.Lock()
	flowCount := len(buffer.sequences)
	sourceFlowCount := len(buffer.sourceSequences[source])
	buffer.mutex.Unlock()
	if flowCount != providerColdPageTestFlowCount {
		t.Fatalf("global flow count=%d, want %d", flowCount, providerColdPageTestFlowCount)
	}
	if sourceFlowCount != providerColdPageTestFlowCount {
		t.Fatalf("source flow count=%d, want %d", sourceFlowCount, providerColdPageTestFlowCount)
	}

	cancel()
	deadline := time.Now().Add(2 * time.Second)
	for {
		buffer.mutex.Lock()
		remainingFlowCount := len(buffer.sequences)
		buffer.mutex.Unlock()
		if remainingFlowCount == 0 {
			break
		}
		if deadline.Before(time.Now()) {
			t.Fatalf("%d provider flows remained after cancellation", remainingFlowCount)
		}
		time.Sleep(time.Millisecond)
	}
}
