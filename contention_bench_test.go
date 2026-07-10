package connect

// contention micro-benchmarks for the per-packet locking paths.
// these isolate the per-packet route-selector and multi-client send dispatch
// so the lock + allocation costs are visible (run with -benchmem). they guard
// against regressions in the per-packet lock/allocation budget.

import (
	"context"
	"runtime"
	"sync/atomic"
	"testing"

	"github.com/urnetwork/connect/protocol"
)

// isolates the route-selector write hot path (one active route, the common
// case). before the snapshot change this took the selector mutex + allocated a
// new []Route in GetActiveRoutes and took the monitor lock in NotifyChannel on
// every call; after, it reads an immutable snapshot lock-free with no
// allocation.
func BenchmarkRouteSelectorWrite(b *testing.B) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sel := NewMultiRouteSelector(ctx, "bench", nil, SourceId(NewId()), true)
	route := make(chan []byte, 8192)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-route:
			}
		}
	}()
	sel.updateTransport(NewSendGatewayTransport(), []Route{route})

	frame := make([]byte, 1400)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i += 1 {
		if err := sel.Write(ctx, frame, -1); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
}

// isolates the route-selector read hot path (one active route).
func BenchmarkRouteSelectorRead(b *testing.B) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sel := NewMultiRouteSelector(ctx, "bench", nil, SourceId(NewId()), false)
	route := make(chan []byte, 8192)
	frame := make([]byte, 1400)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case route <- frame:
			}
		}
	}()
	sel.updateTransport(NewReceiveGatewayTransport(), []Route{route})

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i += 1 {
		if _, err := sel.Read(ctx, -1); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
}

// drives parallel egress flows through the RemoteUserNatMultiClient send
// dispatch with the provider draining (no echo), to exercise the per-packet
// send dispatch path under parallel flows.
func BenchmarkMultiClientEgressParallel(b *testing.B) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	providerClientId := NewId()
	settings := DefaultClientSettings()
	settings.SendBufferSettings.SequenceBufferSize = 0
	settings.SendBufferSettings.AckBufferSize = 0
	settings.ReceiveBufferSettings.SequenceBufferSize = 0
	settings.ForwardBufferSettings.SequenceBufferSize = 0
	providerClient := NewClient(ctx, providerClientId, NewNoContractClientOob(), settings)
	defer providerClient.Cancel()

	providerClient.AddReceiveCallback(func(source TransferPath, frames []*protocol.Frame, peer Peer) {})

	natClient, err := testingNewMultiClient(
		ctx,
		providerClient,
		func(source TransferPath, provideMode protocol.ProvideMode, ipPath *IpPath, packet []byte) {},
	)
	if err != nil {
		b.Fatal(err)
	}

	clientId := NewId()
	source := SourceId(clientId)

	send := func(s int) {
		template, _ := tcp4Packet(s, 0, 0, 0)
		packet := MessagePoolCopy(template)
		if !natClient.SendPacket(source, protocol.ProvideMode_Network, packet, -1) {
			MessagePoolReturn(packet)
		}
	}

	g := runtime.GOMAXPROCS(0)
	for s := 1; s <= g; s += 1 {
		for i := 0; i < 32; i += 1 {
			send(s)
		}
	}

	var flowCounter atomic.Int32

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		s := int(flowCounter.Add(1))
		template, _ := tcp4Packet(s, 0, 0, 0)
		for pb.Next() {
			packet := MessagePoolCopy(template)
			if !natClient.SendPacket(source, protocol.ProvideMode_Network, packet, -1) {
				MessagePoolReturn(packet)
			}
		}
	})
	b.StopTimer()
}

// drives bidirectional traffic through the RemoteUserNatMultiClient: parallel
// egress senders plus a provider that echoes every packet back, so the parent
// stateLock, per-channel stats, transfer send/receive buffers, and route
// selector are all exercised on both the send and receive paths. this is the
// measurement vehicle for the de-contention work; profile with -mutexprofile.
func BenchmarkMultiClientBidirectional(b *testing.B) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	providerClientId := NewId()
	settings := DefaultClientSettings()
	settings.SendBufferSettings.SequenceBufferSize = 0
	settings.SendBufferSettings.AckBufferSize = 0
	settings.ReceiveBufferSettings.SequenceBufferSize = 0
	settings.ForwardBufferSettings.SequenceBufferSize = 0
	providerClient := NewClient(ctx, providerClientId, NewNoContractClientOob(), settings)
	defer providerClient.Cancel()

	// the provider echoes each received packet back with the path reversed, so
	// the echo lands on the originating flow's update (the steady-state ingress
	// path).
	providerClient.AddReceiveCallback(func(source TransferPath, frames []*protocol.Frame, peer Peer) {
		for _, frame := range frames {
			v, err := FromFrame(frame)
			if err != nil {
				continue
			}
			ipToProvider, ok := v.(*protocol.IpPacketToProvider)
			if !ok {
				continue
			}
			ipPath, payload, err := ParseIpPathWithPayload(ipToProvider.IpPacket.PacketBytes)
			if err != nil {
				continue
			}
			echo := ipOosPacket(ipPath.Reverse(), payload)
			ipFromProvider := &protocol.IpPacketFromProvider{
				IpPacket: &protocol.IpPacket{PacketBytes: echo},
			}
			echoFrame, err := ToFrame(ipFromProvider, DefaultProtocolVersion)
			if err != nil {
				continue
			}
			providerClient.SendWithTimeout(echoFrame, source.Reverse(), func(err error) {}, -1)
		}
	})

	var receiveCount atomic.Int64
	natClient, err := testingNewMultiClient(
		ctx,
		providerClient,
		func(source TransferPath, provideMode protocol.ProvideMode, ipPath *IpPath, packet []byte) {
			receiveCount.Add(1)
		},
	)
	if err != nil {
		b.Fatal(err)
	}

	clientId := NewId()
	source := SourceId(clientId)

	send := func(s int) {
		template, _ := tcp4Packet(s, 0, 0, 0)
		packet := MessagePoolCopy(template)
		if !natClient.SendPacket(source, protocol.ProvideMode_Network, packet, -1) {
			MessagePoolReturn(packet)
		}
	}

	g := runtime.GOMAXPROCS(0)
	for s := 1; s <= g; s += 1 {
		for i := 0; i < 32; i += 1 {
			send(s)
		}
	}

	var flowCounter atomic.Int32
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		s := int(flowCounter.Add(1))
		template, _ := tcp4Packet(s, 0, 0, 0)
		for pb.Next() {
			packet := MessagePoolCopy(template)
			if !natClient.SendPacket(source, protocol.ProvideMode_Network, packet, -1) {
				MessagePoolReturn(packet)
			}
		}
	})
	b.StopTimer()
}
