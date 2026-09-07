package connect

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

func startUdpLoopbackEcho(t *testing.T) (uint16, func()) {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("udp echo listen: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		buffer := make([]byte, 2048)
		for {
			n, addr, readErr := conn.ReadFromUDP(buffer)
			if readErr != nil {
				return
			}
			if _, writeErr := conn.WriteToUDP(buffer[:n], addr); writeErr != nil {
				return
			}
		}
	}()
	return uint16(conn.LocalAddr().(*net.UDPAddr).Port), func() {
		_ = conn.Close()
		<-done
	}
}

func TestProviderUdpSharedSocketLifecyclePreservesOrderAndReaps(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port, closeEcho := startUdpLoopbackEcho(t)
	defer closeEcho()

	settings := DefaultProviderLocalUserNatSettingsWithMemoryTarget(4 << 20).UdpBufferSettings
	settings.IdleTimeout = 100 * time.Millisecond
	settings.SequenceBufferSize = 8
	responses := make(chan byte, 128)
	buffer := NewUdp4Buffer(
		ctx,
		func(_ TransferPath, _ protocol.ProvideMode, _ *IpPath, packet []byte) {
			responses <- packet[len(packet)-1]
		},
		settings,
	)

	source := SourceId(NewId())
	const packetCount = 64
	for i := range packetCount {
		packet := MessagePoolGet(32)
		packet[0] = 0x5a
		packet[1] = byte(i)
		udp := &parsedUdp{
			sourceIp:        net.IPv4(10, 0, 0, 1).To4(),
			destinationIp:   net.IPv4(127, 0, 0, 1).To4(),
			sourcePort:      42000,
			destinationPort: port,
			payload:         packet[:2],
		}
		if success, sendErr := buffer.send(
			source,
			protocol.ProvideMode_Network,
			udp,
			-1,
			packet,
		); sendErr != nil || !success {
			MessagePoolReturn(packet)
			t.Fatalf("send %d: success=%t err=%v", i, success, sendErr)
		}
	}

	for i := range packetCount {
		select {
		case marker := <-responses:
			if marker != byte(i) {
				t.Fatalf("response %d marker=%d", i, marker)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("response %d timed out", i)
		}
	}

	buffer.mutex.Lock()
	if buffer.socketReadPoller == nil {
		buffer.mutex.Unlock()
		t.Skip("socket readiness poller unavailable on this platform")
	}
	if len(buffer.sequences) != 1 {
		buffer.mutex.Unlock()
		t.Fatalf("sequence count=%d, want 1", len(buffer.sequences))
	}
	var activeSequence *UdpSequence
	for _, sequence := range buffer.sequences {
		activeSequence = sequence
		if !sequence.sharedSocketLifecycle || sequence.sendItems != nil {
			buffer.mutex.Unlock()
			t.Fatalf("shared lifecycle=%t send channel present=%t",
				sequence.sharedSocketLifecycle, sequence.sendItems != nil)
		}
	}
	buffer.mutex.Unlock()

	pollUntil(t, 5*time.Second, "shared UDP idle reap", func() bool {
		buffer.mutex.Lock()
		defer buffer.mutex.Unlock()
		return len(buffer.sequences) == 0
	})
	for i := range buffer.socketReadPoller.shards {
		shard := &buffer.socketReadPoller.shards[i]
		shard.mutex.RLock()
		registrationCount := len(shard.byFd)
		shard.mutex.RUnlock()
		if registrationCount != 0 {
			t.Fatalf("poll shard %d retained %d registrations", i, registrationCount)
		}
	}
	if activeSequence == nil {
		t.Fatal("shared lifecycle exposed no active sequence")
	}
	if activeSequence.socketReadPollFd != -1 {
		t.Fatalf("reaped sequence poll fd=%d, want invalid -1", activeSequence.socketReadPollFd)
	}
}

func TestProviderUdpSharedSocketLifecycleDoesNotAllocatePerFlowQueues(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port, closeEcho := startUdpLoopbackEcho(t)
	defer closeEcho()

	settings := DefaultProviderLocalUserNatSettingsWithMemoryTarget(4 << 20).UdpBufferSettings
	settings.IdleTimeout = time.Minute
	responses := make(chan struct{}, providerColdPageTestFlowCount)
	buffer := NewUdp4Buffer(
		ctx,
		func(_ TransferPath, _ protocol.ProvideMode, _ *IpPath, _ []byte) {
			responses <- struct{}{}
		},
		settings,
	)
	source := SourceId(NewId())
	for i := range providerColdPageTestFlowCount {
		packet := MessagePoolGet(32)
		packet[0] = byte(i)
		udp := &parsedUdp{
			sourceIp:        net.IPv4(10, 0, 0, 1).To4(),
			destinationIp:   net.IPv4(127, 0, 0, 1).To4(),
			sourcePort:      uint16(43000 + i),
			destinationPort: port,
			payload:         packet[:1],
		}
		if success, sendErr := buffer.send(source, protocol.ProvideMode_Network, udp, -1, packet); sendErr != nil || !success {
			MessagePoolReturn(packet)
			t.Fatalf("flow %d: success=%t err=%v", i, success, sendErr)
		}
	}
	for i := 0; i < providerColdPageTestFlowCount; i++ {
		select {
		case <-responses:
		case <-time.After(5 * time.Second):
			t.Fatalf("response %d timed out", i)
		}
	}

	buffer.mutex.Lock()
	defer buffer.mutex.Unlock()
	if buffer.socketReadPoller == nil {
		t.Skip("socket readiness poller unavailable on this platform")
	}
	if len(buffer.sequences) != providerColdPageTestFlowCount {
		t.Fatalf("sequence count=%d, want %d", len(buffer.sequences), providerColdPageTestFlowCount)
	}
	for _, sequence := range buffer.sequences {
		if !sequence.sharedSocketLifecycle || sequence.sendItems != nil {
			t.Fatal("provider flow retained a send goroutine queue")
		}
	}
	if len(buffer.socketReadPoller.shards) != settings.SocketReadShardCount {
		t.Fatalf("poll shards=%d, want %d",
			len(buffer.socketReadPoller.shards), settings.SocketReadShardCount)
	}
}
