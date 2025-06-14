package connect

import (
	"context"
	"encoding/binary"
	"net"
	// "reflect"
	"testing"
	"time"

	// "sync"
	"fmt"

	// "github.com/google/gopacket"
	// "github.com/google/gopacket/layers"

	"github.com/go-playground/assert/v2"

	"github.com/urnetwork/connect/protocol"
)

func TestClientUdp4(t *testing.T) {
	testClient(t, testingNewClient, udp4Packet, (*IpPath).ToIp4Path)
}

func TestClientTcp4(t *testing.T) {
	testClient(t, testingNewClient, tcp4Packet, (*IpPath).ToIp4Path)
}

func TestClientUdp6(t *testing.T) {
	testClient(t, testingNewClient, udp6Packet, (*IpPath).ToIp6Path)
}

func TestClientTcp6(t *testing.T) {
	testClient(t, testingNewClient, tcp6Packet, (*IpPath).ToIp6Path)
}

type PacketGeneratorFunction func(int, int, int, int) ([]byte, []byte)

func TestUdp4Path(t *testing.T) {
	packet, _ := udp4Packet(1, 1, 1, 1)
	ipPath, err := ParseIpPath(packet)
	assert.Equal(t, nil, err)

	assert.Equal(t, IpProtocolUdp, ipPath.Protocol)

	assert.Equal(t, &IpPath{
		Version:         4,
		Protocol:        IpProtocolUdp,
		SourceIp:        net.IPv4(byte(72), byte(0), byte(0), byte(1)).To4(),
		SourcePort:      40000 + 1,
		DestinationIp:   net.IPv4(byte(72), byte(1+1), byte(1+1), byte(1+1)).To4(),
		DestinationPort: 443,
	}, ipPath)

	ip4Path := ipPath.ToIp4Path()
	assert.Equal(t, Ip4Path{
		Protocol:        IpProtocolUdp,
		SourceIp:        [4]byte(net.IPv4(byte(72), byte(0), byte(0), byte(1)).To4()),
		SourcePort:      40000 + 1,
		DestinationIp:   [4]byte(net.IPv4(byte(72), byte(1+1), byte(1+1), byte(1+1)).To4()),
		DestinationPort: 443,
	}, ip4Path)
}

func TestTcp4Path(t *testing.T) {
	packet, _ := tcp4Packet(1, 1, 1, 1)
	ipPath, err := ParseIpPath(packet)
	assert.Equal(t, nil, err)

	assert.Equal(t, IpProtocolTcp, ipPath.Protocol)

	assert.Equal(t, &IpPath{
		Version:         4,
		Protocol:        IpProtocolTcp,
		SourceIp:        net.IPv4(byte(72), byte(0), byte(0), byte(1)).To4(),
		SourcePort:      40000 + 1,
		DestinationIp:   net.IPv4(byte(72), byte(1+1), byte(1+1), byte(1+1)).To4(),
		DestinationPort: 443,
	}, ipPath)

	ip4Path := ipPath.ToIp4Path()
	assert.Equal(t, Ip4Path{
		Protocol:        IpProtocolTcp,
		SourceIp:        [4]byte(net.IPv4(byte(72), byte(0), byte(0), byte(1)).To4()),
		SourcePort:      40000 + 1,
		DestinationIp:   [4]byte(net.IPv4(byte(72), byte(1+1), byte(1+1), byte(1+1)).To4()),
		DestinationPort: 443,
	}, ip4Path)
}

func TestUdp6Path(t *testing.T) {
	packet, _ := udp6Packet(1, 1, 1, 1)
	ipPath, err := ParseIpPath(packet)
	assert.Equal(t, nil, err)

	assert.Equal(t, IpProtocolUdp, ipPath.Protocol)

	assert.Equal(t, &IpPath{
		Version:         6,
		Protocol:        IpProtocolUdp,
		SourceIp:        net.IPv4(byte(72), byte(0), byte(0), byte(1)).To16(),
		SourcePort:      40000 + 1,
		DestinationIp:   net.IPv4(byte(72), byte(1+1), byte(1+1), byte(1+1)).To16(),
		DestinationPort: 443,
	}, ipPath)

	ip6Path := ipPath.ToIp6Path()
	assert.Equal(t, Ip6Path{
		Protocol:        IpProtocolUdp,
		SourceIp:        [16]byte(net.IPv4(byte(72), byte(0), byte(0), byte(1)).To16()),
		SourcePort:      40000 + 1,
		DestinationIp:   [16]byte(net.IPv4(byte(72), byte(1+1), byte(1+1), byte(1+1)).To16()),
		DestinationPort: 443,
	}, ip6Path)
}

func TestTcp6Path(t *testing.T) {
	packet, _ := tcp6Packet(1, 1, 1, 1)
	ipPath, err := ParseIpPath(packet)
	assert.Equal(t, nil, err)

	assert.Equal(t, IpProtocolTcp, ipPath.Protocol)

	assert.Equal(t, &IpPath{
		Version:         6,
		Protocol:        IpProtocolTcp,
		SourceIp:        net.IPv4(byte(72), byte(0), byte(0), byte(1)).To16(),
		SourcePort:      40000 + 1,
		DestinationIp:   net.IPv4(byte(72), byte(1+1), byte(1+1), byte(1+1)).To16(),
		DestinationPort: 443,
	}, ipPath)

	ip6Path := ipPath.ToIp6Path()
	assert.Equal(t, Ip6Path{
		Protocol:        IpProtocolTcp,
		SourceIp:        [16]byte(net.IPv4(byte(72), byte(0), byte(0), byte(1)).To16()),
		SourcePort:      40000 + 1,
		DestinationIp:   [16]byte(net.IPv4(byte(72), byte(1+1), byte(1+1), byte(1+1)).To16()),
		DestinationPort: 443,
	}, ip6Path)
}

func udp4Packet(s int, i int, j int, k int) (packet []byte, payload []byte) {
	payload = make([]byte, 4)
	binary.LittleEndian.PutUint32(payload, uint32(s))

	ipPath := &IpPath{
		Version:         4,
		Protocol:        IpProtocolUdp,
		SourceIp:        net.IPv4(72, 0, 0, 1),
		SourcePort:      40000 + s,
		DestinationIp:   net.IPv4(byte(72), byte(1+i), byte(1+j), byte(1+k)),
		DestinationPort: 443,
	}

	packet = ip4OosUdpPacket(ipPath, payload)
	return
}

func tcp4Packet(s int, i int, j int, k int) (packet []byte, payload []byte) {
	payload = make([]byte, 4)
	binary.LittleEndian.PutUint32(payload, uint32(s))

	ipPath := &IpPath{
		Version:         4,
		Protocol:        IpProtocolTcp,
		SourceIp:        net.IPv4(72, 0, 0, 1),
		SourcePort:      40000 + s,
		DestinationIp:   net.IPv4(byte(72), byte(1+i), byte(1+j), byte(1+k)),
		DestinationPort: 443,
	}

	packet = ip4OosTcpPacket(ipPath, payload)
	return
}

func udp6Packet(s int, i int, j int, k int) (packet []byte, payload []byte) {
	payload = make([]byte, 4)
	binary.LittleEndian.PutUint32(payload, uint32(s))

	ipPath := &IpPath{
		Version:         6,
		Protocol:        IpProtocolUdp,
		SourceIp:        net.IPv4(72, 0, 0, 1),
		SourcePort:      40000 + s,
		DestinationIp:   net.IPv4(byte(72), byte(1+i), byte(1+j), byte(1+k)),
		DestinationPort: 443,
	}

	packet = ip6OosUdpPacket(ipPath, payload)
	return
}

func tcp6Packet(s int, i int, j int, k int) (packet []byte, payload []byte) {
	payload = make([]byte, 4)
	binary.LittleEndian.PutUint32(payload, uint32(s))

	ipPath := &IpPath{
		Version:         6,
		Protocol:        IpProtocolTcp,
		SourceIp:        net.IPv4(72, 0, 0, 1),
		SourcePort:      40000 + s,
		DestinationIp:   net.IPv4(byte(72), byte(1+i), byte(1+j), byte(1+k)),
		DestinationPort: 443,
	}

	packet = ip6OosTcpPacket(ipPath, payload)
	return
}

func testingNewClient(ctx context.Context, providerClient *Client, receivePacketCallback ReceivePacketFunction) (UserNatClient, error) {
	settings := DefaultClientSettings()
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)

	routeSend := make(chan []byte)
	routeReceive := make(chan []byte)

	transportSend := NewSendGatewayTransport()
	transportReceive := NewReceiveGatewayTransport()
	client.RouteManager().UpdateTransport(transportSend, []Route{routeSend})
	client.RouteManager().UpdateTransport(transportReceive, []Route{routeReceive})

	client.ContractManager().AddNoContractPeer(providerClient.ClientId())

	providerTransportSend := NewSendClientTransport(DestinationId(client.ClientId()))
	providerTransportReceive := NewReceiveGatewayTransport()
	providerClient.RouteManager().UpdateTransport(providerTransportReceive, []Route{routeSend})
	providerClient.RouteManager().UpdateTransport(providerTransportSend, []Route{routeReceive})

	providerClient.ContractManager().AddNoContractPeer(client.ClientId())

	return NewRemoteUserNatClient(
		client,
		receivePacketCallback,
		[]MultiHopId{
			RequireMultiHopId(providerClient.ClientId()),
		},
		protocol.ProvideMode_Network,
	)
}

// test with all sequence buffer sizes set to 0
func testClient[P comparable](
	t *testing.T,
	userNatClientGenerator func(context.Context, *Client, ReceivePacketFunction) (UserNatClient, error),
	packetGenerator PacketGeneratorFunction,
	toComparableIpPath func(*IpPath) P,
) {

	if testing.Short() {
		t.Skip("skipping testing in short mode")
	}

	// runs a send-receive test on the `UserNatClient` produced by `userNatClientGenerator`
	// this is a multi-threaded stress test that is meant to stress the buffers and routing

	// n destinations
	// all have the same receiver callback, put into a channel of messages
	// echo the received packet, with paths reversed

	// create fake packets for all iterations of i,j,k in a range
	// retransmit some packets by increasing source port s
	// make sure all packets are received
	// make sure all packets are echoed back

	timeout := 30 * time.Second

	m := 6
	n := 6
	repeatCount := 6
	parallelCount := 6
	echoCount := 2

	// each packet gets echoed back
	totalCount := parallelCount * m * n * n * n * repeatCount * (1 + echoCount)

	// cMutex := sync.Mutex{}
	// cSendCount := 0
	// cReceiveCount := 0

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	clientId := NewId()
	providerClientId := NewId()

	settings := DefaultClientSettings()
	settings.SendBufferSettings.SequenceBufferSize = 0
	settings.SendBufferSettings.AckBufferSize = 0
	settings.ReceiveBufferSettings.SequenceBufferSize = 0
	// settings.ReceiveBufferSettings.AckBufferSize = 0
	settings.ForwardBufferSettings.SequenceBufferSize = 0
	providerClient := NewClient(ctx, providerClientId, NewNoContractClientOob(), settings)
	defer providerClient.Cancel()

	type receivePacket struct {
		source TransferPath
		packet []byte
	}

	receivePackets := make(chan *receivePacket)

	receivePacketCallback := func(source TransferPath, provideMode protocol.ProvideMode, ipPath *IpPath, packet []byte) {
		// record the echo packet

		// cMutex.Lock()
		// cReceiveCount += 1
		// // fmt.Printf("C Receive %d/%d (%.2f%%)\n", cReceiveCount, totalCount, 100.0 * float32(cReceiveCount) / float32(totalCount))
		// cMutex.Unlock()

		ipPath, payload, err := ParseIpPathWithPayload(packet)
		if err != nil {
			panic(err)
		}
		packet = ipOosPacket(ipPath.Reverse(), payload)

		receivePacket := &receivePacket{
			source: source,
			packet: packet,
		}

		select {
		case <-ctx.Done():
		case receivePackets <- receivePacket:
		}
	}

	natClient, err := userNatClientGenerator(ctx, providerClient, receivePacketCallback)
	assert.Equal(t, err, nil)

	providerClient.AddReceiveCallback(func(source TransferPath, frames []*protocol.Frame, provideMode protocol.ProvideMode) {
		// cMutex.Lock()
		// cReceiveCount += 1
		// // fmt.Printf("C Receive %d/%d (%.2f%%)\n", cReceiveCount, totalCount, 100.0 * float32(cReceiveCount) / float32(totalCount))
		// cMutex.Unlock()

		echo := func(packet []byte) {
			// reverse the packet
			ipPath, payload, err := ParseIpPathWithPayload(packet)
			if err != nil {
				panic(err)
			}
			packet = ipOosPacket(ipPath.Reverse(), payload)

			ipPacketFromProvider := &protocol.IpPacketFromProvider{
				IpPacket: &protocol.IpPacket{
					PacketBytes: packet,
				},
			}
			frame, err := ToFrame(ipPacketFromProvider, DefaultProtocolVersion)
			if err != nil {
				panic(err)
			}

			providerClient.SendWithTimeout(frame, source.Reverse(), func(err error) {}, -1)
			// assert.Equal(t, true, success)

			// cMutex.Lock()
			// cSendCount += 1
			// // fmt.Printf("C Send %d/%d (%.2f%%)\n", cSendCount, totalCount, 100.0 * float32(cSendCount) / float32(totalCount))
			// cMutex.Unlock()
		}
		for _, frame := range frames {
			if ipPacketToProvider_, err := FromFrame(frame); err == nil {
				if ipPacketToProvider, ok := ipPacketToProvider_.(*protocol.IpPacketToProvider); ok {
					packet := ipPacketToProvider.IpPacket.PacketBytes

					receivePacket := &receivePacket{
						source: source,
						packet: packet,
					}

					receivePackets <- receivePacket

					for i := 0; i < echoCount; i += 1 {
						// do not make a blocking call back into the client from the receiver
						// this could deadlock the client depending on whether other messages are
						// queued to this receiver
						go echo(packet)
					}
				}
			}

		}
	})

	for p := 0; p < parallelCount; p += 1 {
		go func() {
			source := SourceId(clientId)
			for s := 0; s < m; s += 1 {
				for i := 0; i < n; i += 1 {
					for j := 0; j < n; j += 1 {
						for k := 0; k < n; k += 1 {
							for a := 0; a < repeatCount; a += 1 {
								packet, _ := packetGenerator(s, i, j, k)
								natClient.SendPacket(source, protocol.ProvideMode_Network, packet, -1)
								// if !success {
								// 	fmt.Printf("[TIMEOUT]%T\n", natClient)
								// }
								// assert.Equal(t, true, success)

								// cMutex.Lock()
								// cSendCount += 1
								// // fmt.Printf("C Send %d/%d (%.2f%%)\n", cSendCount, totalCount, 100.0 * float32(cSendCount) / float32(totalCount))
								// cMutex.Unlock()
							}
						}
					}
				}
			}
		}()
	}

	comparableIpPathPayloads := map[P][][]byte{}
	comparableIpPathSources := map[P]map[TransferPath]bool{}

	for i := 0; i < totalCount; i += 1 {
		if totalCount < 100 || i%(totalCount/100) == 0 {
			fmt.Printf("[testr]%d/%d (%.2f%%)\n", i, totalCount, 100*float32(i)/float32(totalCount))
		}
		select {
		case receivePacket := <-receivePackets:
			// fmt.Printf("Receive %d/%d (%.2f%%)\n", i + 1, totalCount, 100.0 * float32(i + 1) / float32(totalCount))

			ipPath, payload, err := ParseIpPathWithPayload(receivePacket.packet)
			assert.Equal(t, err, nil)

			// var payload []byte
			// switch ipPath.Version {
			// case 4:
			// 	ipv4 := layers.IPv4{}
			// 	ipv4.DecodeFromBytes(receivePacket.packet, gopacket.NilDecodeFeedback)
			// 	payload = ipv4.Payload
			// case 6:
			// 	ipv6 := layers.IPv6{}
			// 	ipv6.DecodeFromBytes(receivePacket.packet, gopacket.NilDecodeFeedback)
			// 	payload = ipv6.Payload
			// }

			// switch ipPath.Protocol {
			// case IpProtocolUdp:
			// 	udp := layers.UDP{}
			// 	udp.DecodeFromBytes(payload, gopacket.NilDecodeFeedback)
			// 	payload = udp.Payload
			// case IpProtocolTcp:
			// 	tcp := layers.TCP{}
			// 	tcp.DecodeFromBytes(payload, gopacket.NilDecodeFeedback)
			// 	payload = tcp.Payload
			// }

			comparableIpPath := toComparableIpPath(ipPath)
			comparableIpPathPayloads[comparableIpPath] = append(comparableIpPathPayloads[comparableIpPath], payload)

			sources, ok := comparableIpPathSources[comparableIpPath]
			if !ok {
				sources = map[TransferPath]bool{}
				comparableIpPathSources[comparableIpPath] = sources
			}
			sources[receivePacket.source] = true
		case <-time.After(timeout):
			fmt.Printf("[TIMEOUT]receive\n")
			t.FailNow()
		}
	}
	select {
	case <-receivePackets:
		// excesss packets received
		t.FailNow()
	case <-time.After(1 * time.Second):
	}

	// make sure all messages were received
	// make sure each path has just one source
	for s := 0; s < m; s += 1 {
		for i := 0; i < n; i += 1 {
			for j := 0; j < n; j += 1 {
				for k := 0; k < n; k += 1 {

					packet, payload := packetGenerator(s, i, j, k)
					ipPath, err := ParseIpPath(packet)
					assert.Equal(t, err, nil)
					comparableIpPath := toComparableIpPath(ipPath)

					payloads := comparableIpPathPayloads[comparableIpPath]

					for _, b := range payloads {
						assert.Equal(t, b, payload)
					}

					assert.Equal(t, parallelCount*repeatCount*(1+echoCount), len(payloads))

					sources := comparableIpPathSources[comparableIpPath]

					if 0 < echoCount {
						assert.Equal(t, 2, len(sources))
					} else {
						assert.Equal(t, 1, len(sources))
					}
				}
			}
		}
	}
}
