package connect

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// TestMultiClientLifecyclePoolBalance pins the message-pool balance across the
// multi-client's whole lifecycle: build a RemoteUserNatMultiClient against an
// in-memory exit, push packets through the egress path, tear everything down, and
// require every pooled buffer back. This is the reconfiguration cycle a device
// performs on every destination change, so a single lost return here compounds by
// reconnect count in production.
func TestMultiClientLifecyclePoolBalance(t *testing.T) {
	poolOutstanding := func() int64 {
		taken, returned, _ := MessagePoolCounts()
		return int64(taken) - int64(returned)
	}
	settle := func() int64 {
		prev := poolOutstanding()
		stableCount := 0
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			time.Sleep(50 * time.Millisecond)
			n := poolOutstanding()
			if n == prev {
				stableCount += 1
				if 4 <= stableCount {
					break
				}
			} else {
				stableCount = 0
				prev = n
			}
		}
		return prev
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// warmup cycle to initialize process-global pools before the baseline
	runMultiClientPoolCycle(ctx, t)
	before := settle()

	const cycles = 10
	for i := 0; i < cycles; i += 1 {
		runMultiClientPoolCycle(ctx, t)
	}

	after := settle()
	if before < after {
		t.Errorf("pool buffers not returned across %d multi-client lifecycles: outstanding %d -> %d (+%d)",
			cycles, before, after, after-before)
	}
}

// The simple single-destination client uses the same raw v2 envelope as the
// multi-client. A successful send must consume exactly the caller's packet
// reference; taking an extra read-only share here leaves one reference
// outstanding on every packet even though all transfer queues drain cleanly.
func TestRemoteUserNatClientRawSendPoolBalance(t *testing.T) {
	poolOutstanding := func() int64 {
		taken, returned, _ := MessagePoolCounts()
		return int64(taken) - int64(returned)
	}
	settle := func() int64 {
		prev := poolOutstanding()
		stableCount := 0
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			time.Sleep(50 * time.Millisecond)
			n := poolOutstanding()
			if n == prev {
				stableCount += 1
				if 4 <= stableCount {
					break
				}
			} else {
				stableCount = 0
				prev = n
			}
		}
		return prev
	}

	ctx, cancel := context.WithCancel(context.Background())
	settings := DefaultClientSettings()
	settings.SendBufferSettings.SequenceBufferSize = 0
	settings.SendBufferSettings.AckBufferSize = 0
	settings.ReceiveBufferSettings.SequenceBufferSize = 0
	settings.ForwardBufferSettings.SequenceBufferSize = 0
	providerClient := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)

	received := make(chan struct{}, 32)
	providerClient.AddReceiveCallback(func(source TransferPath, frames []*protocol.Frame, peer Peer) {
		for _, frame := range frames {
			if frame.MessageType == protocol.MessageType_IpIpPacketToProvider {
				select {
				case received <- struct{}{}:
				default:
				}
			}
		}
	})
	natClient, err := testingNewClient(
		ctx,
		providerClient,
		func(TransferPath, protocol.ProvideMode, *IpPath, []byte) {},
	)
	if err != nil {
		t.Fatal(err)
	}

	before := settle()
	source := SourceId(NewId())
	const packetCount = 16
	for i := 0; i < packetCount; i += 1 {
		packet := poolBalanceUdp4Packet(
			net.ParseIP("10.0.0.1"),
			40000+i,
			net.ParseIP("203.0.113.7"),
			33434,
			[]byte("single-client pool balance"),
		)
		if !natClient.SendPacket(source, protocol.ProvideMode_Network, packet, time.Second) {
			MessagePoolReturn(packet)
			t.Fatal("single-destination send was not accepted")
		}
	}
	for i := 0; i < packetCount; i += 1 {
		select {
		case <-received:
		case <-time.After(5 * time.Second):
			t.Fatalf("provider received %d/%d packets", i, packetCount)
		}
	}

	natClient.Close()
	providerClient.Close()
	cancel()
	after := settle()
	if before < after {
		t.Fatalf("single-destination raw sends left pooled buffers outstanding: %d -> %d (+%d)",
			before, after, after-before)
	}
}

// A rejected multi-client race candidate never takes ownership. The race
// helper must undo only its read-only share and leave the caller's original
// packet live for another candidate or retry.
func TestMultiClientRejectedRaceAttemptRetainsOriginalPacket(t *testing.T) {
	clientCtx, clientCancel := context.WithCancel(context.Background())
	client := NewClient(clientCtx, NewId(), NewNoContractClientOob(), DefaultClientSettings())
	clientCancel()

	settings := DefaultMultiClientSettings()
	channelCtx, channelCancel := context.WithCancel(context.Background())
	defer channelCancel()
	channel := &multiClientChannel{
		ctx:                       channelCtx,
		cancel:                    channelCancel,
		log:                       NewNoopLogger(),
		args:                      &multiClientChannelArgs{},
		settings:                  settings,
		client:                    client,
		eventBuckets:              []*multiClientEventBucket{},
		ip4DestinationSourceCount: map[Ip4Path]map[Ip4Path]int{},
		ip6DestinationSourceCount: map[Ip6Path]map[Ip6Path]int{},
		packetStats:               &clientWindowStats{log: NewNoopLogger()},
	}

	ipPath := &IpPath{
		Version:         4,
		Protocol:        IpProtocolUdp,
		SourceIp:        net.ParseIP("10.0.0.1"),
		SourcePort:      40000,
		DestinationIp:   net.ParseIP("203.0.113.7"),
		DestinationPort: 443,
	}
	packet := poolBalanceUdp4Packet(
		ipPath.SourceIp,
		ipPath.SourcePort,
		ipPath.DestinationIp,
		ipPath.DestinationPort,
		[]byte("rejected race ownership"),
	)

	if sendMultiClientRaceAttempt(channel, packet, ipPath, 0) {
		MessagePoolReturn(packet)
		t.Fatal("canceled client accepted race packet")
	}
	if pooled, _ := MessagePoolCheck(packet); !pooled {
		t.Fatal("rejected race attempt returned the caller's original packet")
	}
	if returned := MessagePoolReturn(packet); !returned {
		t.Fatal("original packet was not the final live reference after rejected race attempt")
	}
}

// A rejected race candidate and an accepted sibling overlap in production:
// the multi-client releases the original race owner when any candidate wins,
// then SendSequence releases the winner's share asynchronously. The rejected
// candidate must not consume either of those two references.
func TestMultiClientRejectedRaceAttemptRetainsSuccessfulSiblingPacket(t *testing.T) {
	clientCtx, clientCancel := context.WithCancel(context.Background())
	client := NewClient(clientCtx, NewId(), NewNoContractClientOob(), DefaultClientSettings())
	clientCancel()

	channelCtx, channelCancel := context.WithCancel(context.Background())
	defer channelCancel()
	channel := &multiClientChannel{
		ctx:                       channelCtx,
		cancel:                    channelCancel,
		log:                       NewNoopLogger(),
		args:                      &multiClientChannelArgs{},
		settings:                  DefaultMultiClientSettings(),
		client:                    client,
		eventBuckets:              []*multiClientEventBucket{},
		ip4DestinationSourceCount: map[Ip4Path]map[Ip4Path]int{},
		ip6DestinationSourceCount: map[Ip6Path]map[Ip6Path]int{},
		packetStats:               &clientWindowStats{log: NewNoopLogger()},
	}
	ipPath := &IpPath{
		Version:         4,
		Protocol:        IpProtocolUdp,
		SourceIp:        net.ParseIP("10.0.0.1"),
		SourcePort:      40000,
		DestinationIp:   net.ParseIP("203.0.113.7"),
		DestinationPort: 443,
	}
	packet := poolBalanceUdp4Packet(
		ipPath.SourceIp,
		ipPath.SourcePort,
		ipPath.DestinationIp,
		ipPath.DestinationPort,
		[]byte("mixed race ownership"),
	)
	successfulSiblingPacket := MessagePoolShareReadOnly(packet)

	if sendMultiClientRaceAttempt(channel, packet, ipPath, 0) {
		MessagePoolReturn(successfulSiblingPacket)
		MessagePoolReturn(packet)
		t.Fatal("canceled client accepted race packet")
	}
	if returned := MessagePoolReturn(packet); returned {
		t.Fatal("original race owner was the final reference while a successful sibling remained")
	}
	if returned := MessagePoolReturn(successfulSiblingPacket); !returned {
		t.Fatal("successful sibling did not retain the final live packet reference")
	}
}

// runMultiClientPoolCycle is one destination-change cycle: an in-memory exit, a
// multi-client over it, a burst of egress packets, then teardown of both.
func runMultiClientPoolCycle(ctx context.Context, t *testing.T) {
	cycleCtx, cycleCancel := context.WithCancel(ctx)
	defer cycleCancel()

	clientSettings := DefaultClientSettings()
	clientSettings.SendBufferSettings.SequenceBufferSize = 0
	clientSettings.SendBufferSettings.AckBufferSize = 0
	clientSettings.ReceiveBufferSettings.SequenceBufferSize = 0
	clientSettings.ForwardBufferSettings.SequenceBufferSize = 0
	providerClient := NewClient(cycleCtx, NewId(), NewNoContractClientOob(), clientSettings)
	defer providerClient.Cancel()

	// echo any IpPacketToProvider back with the path reversed, like an exit would
	providerClient.AddReceiveCallback(func(src TransferPath, frames []*protocol.Frame, peer Peer) {
		for _, frame := range frames {
			if frame.MessageType != protocol.MessageType_IpIpPacketToProvider {
				continue
			}
			message, err := FromFrame(frame)
			if err != nil {
				continue
			}
			ipPacketToProvider, ok := message.(*protocol.IpPacketToProvider)
			if !ok {
				continue
			}
			ipPath, payload, err := ParseIpPathWithPayload(ipPacketToProvider.IpPacket.PacketBytes)
			if err != nil {
				continue
			}
			reversed := ipPath.Reverse()
			packet := poolBalanceUdp4Packet(reversed.SourceIp, reversed.SourcePort, reversed.DestinationIp, reversed.DestinationPort, payload)
			frame, err := ipPacketFromProviderFrame(packet, DefaultProtocolVersion)
			if err != nil {
				MessagePoolReturn(packet)
				continue
			}
			sent := providerClient.SendWithTimeout(frame, src.SourceId, func(err error) {}, -1)
			if !sent {
				MessagePoolReturn(frame.MessageBytes)
			}
		}
	})

	multiSettings := DefaultMultiClientSettings()
	multiSettings.SecurityPolicyGenerator = DisableSecurityPolicyWithStats
	received := make(chan struct{}, 64)
	multi := NewRemoteUserNatMultiClient(
		cycleCtx,
		testMultiClientGenerator(providerClient),
		func(source TransferPath, provideMode protocol.ProvideMode, ipPath *IpPath, packet []byte) {
			select {
			case received <- struct{}{}:
			default:
			}
		},
		protocol.ProvideMode_Network,
		multiSettings,
	)
	defer multi.Close()

	source := SourceId(NewId())
	for i := 0; i < 20; i += 1 {
		packet := poolBalanceUdp4Packet(net.ParseIP("10.0.0.1"), 40000+i, net.ParseIP("203.0.113.7"), 33434, []byte("pool balance probe"))
		if !multi.SendPacket(source, protocol.ProvideMode_Network, packet, 1*time.Second) {
			MessagePoolReturn(packet)
		}
	}

	// wait briefly for echoes so the ingress path also runs
	echoDeadline := time.NewTimer(2 * time.Second)
	defer echoDeadline.Stop()
	echoes := 0
	for echoes < 1 {
		select {
		case <-received:
			echoes += 1
		case <-echoDeadline.C:
			t.Logf("cycle saw %d echoes (echo not required for balance)", echoes)
			return
		}
	}
}

// udp4Packet hand-rolls a valid IPv4/UDP packet into a pooled buffer.
func poolBalanceUdp4Packet(srcIp net.IP, srcPort int, dstIp net.IP, dstPort int, payload []byte) []byte {
	total := 28 + len(payload)
	packet := MessagePoolGet(total)
	packet[0] = 0x45
	packet[1] = 0
	packet[2] = byte(total >> 8)
	packet[3] = byte(total)
	packet[4], packet[5], packet[6], packet[7] = 0, 0, 0, 0
	packet[8] = 64
	packet[9] = 17
	packet[10], packet[11] = 0, 0
	copy(packet[12:16], srcIp.To4())
	copy(packet[16:20], dstIp.To4())
	packet[20] = byte(srcPort >> 8)
	packet[21] = byte(srcPort)
	packet[22] = byte(dstPort >> 8)
	packet[23] = byte(dstPort)
	udpLen := 8 + len(payload)
	packet[24] = byte(udpLen >> 8)
	packet[25] = byte(udpLen)
	packet[26], packet[27] = 0, 0
	copy(packet[28:], payload)
	return packet
}
