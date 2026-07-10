package connect

import (
	"context"
	"math"
	"net"
	"testing"
	"time"

	// "slices"
	"sync"

	"github.com/go-playground/assert/v2"

	"github.com/urnetwork/connect/protocol"
)

type stubServerNameLookup struct {
	names []string
}

func (self stubServerNameLookup) ServerNames(ip string) []string {
	return self.names
}

// TestMultiClientServerNameAffinity verifies ServerName path affinity collapses to the
// base domain: a.foo.com, b.c.foo.com and foo.com all share a single foo.com path.
func TestMultiClientServerNameAffinity(t *testing.T) {
	ipPath := &IpPath{
		Version:         4,
		Protocol:        IpProtocolTcp,
		DestinationIp:   net.ParseIP("93.184.216.34"),
		DestinationPort: 443,
	}

	// with a lookup returning sub-domains of one site, affinity is one base-domain path
	{
		mc := &RemoteUserNatMultiClient{}
		mc.config.Store(&multiClientConfig{
			serverNameLookup: stubServerNameLookup{names: []string{"a.foo.com", "b.c.foo.com", "foo.com"}},
		})
		paths := mc.affinityIpPathsWithLock(ipPath)
		if len(paths) != 1 {
			t.Fatalf("affinity paths = %d, want 1 (collapsed to base domain): %+v", len(paths), paths)
		}
		if paths[0].ServerName != "foo.com" {
			t.Fatalf("affinity ServerName = %q, want foo.com", paths[0].ServerName)
		}
	}

	// distinct sites get distinct affinity paths
	{
		mc := &RemoteUserNatMultiClient{}
		mc.config.Store(&multiClientConfig{
			serverNameLookup: stubServerNameLookup{names: []string{"a.foo.com", "x.bar.com"}},
		})
		paths := mc.affinityIpPathsWithLock(ipPath)
		names := map[string]bool{}
		for _, p := range paths {
			names[p.ServerName] = true
		}
		if !names["foo.com"] || !names["bar.com"] || len(paths) != 2 {
			t.Fatalf("affinity paths = %+v, want {foo.com, bar.com}", paths)
		}
	}

	// with no lookup, :443 falls back to destination ip/port affinity (no ServerName)
	{
		mc := &RemoteUserNatMultiClient{}
		mc.config.Store(&multiClientConfig{})
		paths := mc.affinityIpPathsWithLock(ipPath)
		if len(paths) != 1 || paths[0].ServerName != "" || !paths[0].DestinationIp.Equal(ipPath.DestinationIp) {
			t.Fatalf("affinity paths = %+v, want one destination-ip path (no lookup)", paths)
		}
	}
}

func TestMultiClientUdp4(t *testing.T) {
	testClient(t, testingNewMultiClient, udp4Packet, (*IpPath).ToIp4Path)
}

func TestMultiClientTcp4(t *testing.T) {
	testClient(t, testingNewMultiClient, tcp4Packet, (*IpPath).ToIp4Path)
}

func TestMultiClientUdp6(t *testing.T) {
	testClient(t, testingNewMultiClient, udp6Packet, (*IpPath).ToIp6Path)
}

func TestMultiClientTcp6(t *testing.T) {
	testClient(t, testingNewMultiClient, tcp6Packet, (*IpPath).ToIp6Path)
}

func testMultiClientGenerator(providerClient *Client) *TestMultiClientGenerator {
	mutex := sync.Mutex{}
	unsubs := map[*Client]func(){}

	return &TestMultiClientGenerator{
		nextDestinations: func(count int, excludeDestinations []MultiHopId, rankMode string) (map[MultiHopId]DestinationStats, error) {
			next := map[MultiHopId]DestinationStats{}
			containsTail := func() bool {
				for _, destination := range excludeDestinations {
					if 0 < destination.Len() && destination.Tail() == providerClient.ClientId() {
						return true
					}
				}
				return false
			}
			if !containsTail() {
				next[RequireMultiHopId(providerClient.ClientId())] = DestinationStats{
					EstimatedBytesPerSecond: ByteCount(0),
					Tier:                    0,
				}
			}
			return next, nil
		},
		newClientArgs: func() (*MultiClientGeneratorClientArgs, error) {
			args := &MultiClientGeneratorClientArgs{
				ClientId:   NewId(),
				ClientAuth: nil,
			}
			return args, nil
		},
		removeClientArgs: func(args *MultiClientGeneratorClientArgs) {
			// do nothing
		},
		removeClientWithArgs: func(client *Client, args *MultiClientGeneratorClientArgs) {
			var unsub func()
			var ok bool
			func() {
				mutex.Lock()
				defer mutex.Unlock()
				unsub, ok = unsubs[client]
				if ok {
					delete(unsubs, client)
				}
			}()
			if ok {
				unsub()
			}
		},
		newClientSettings: func() *ClientSettings {
			settings := DefaultClientSettings()
			settings.SendBufferSettings.SequenceBufferSize = 0
			settings.SendBufferSettings.AckBufferSize = 0
			settings.ReceiveBufferSettings.SequenceBufferSize = 0
			// settings.ReceiveBufferSettings.AckBufferSize = 0
			settings.ForwardBufferSettings.SequenceBufferSize = 0
			return settings
		},
		newClient: func(ctx context.Context, args *MultiClientGeneratorClientArgs, clientSettings *ClientSettings) (*Client, error) {
			client := NewClient(ctx, args.ClientId, NewNoContractClientOob(), clientSettings)

			routeSend := make(chan []byte)
			routeReceive := make(chan []byte)

			transportSend := NewSendGatewayTransport()
			transportReceive := NewReceiveGatewayTransport()
			client.RouteManager().UpdateTransport(transportSend, []Route{routeSend})
			client.RouteManager().UpdateTransport(transportReceive, []Route{routeReceive})

			client.ContractManager().AddNoContractPeer(providerClient.ClientId())

			providerTransportSend := NewSendClientTransport(DestinationId(args.ClientId))
			providerTransportReceive := NewReceiveGatewayTransport()
			providerClient.RouteManager().UpdateTransport(providerTransportReceive, []Route{routeSend})
			providerClient.RouteManager().UpdateTransport(providerTransportSend, []Route{routeReceive})

			providerClient.ContractManager().AddNoContractPeer(client.ClientId())

			unsub := func() {
				client.RouteManager().RemoveTransport(transportSend)
				client.RouteManager().RemoveTransport(transportReceive)
				providerClient.RouteManager().RemoveTransport(providerTransportReceive)
				providerClient.RouteManager().RemoveTransport(providerTransportSend)
			}

			func() {
				mutex.Lock()
				defer mutex.Unlock()
				unsubs[client] = unsub
			}()

			return client, nil
		},
	}
}

func testingNewMultiClient(ctx context.Context, providerClient *Client, receivePacketCallback ReceivePacketFunction) (UserNatClient, error) {
	generator := testMultiClientGenerator(providerClient)

	settings := DefaultMultiClientSettings()
	// TODO the tcp packets must use real seq numbers for this to work
	settings.TcpCollapsePrevention = false

	multiClient := NewRemoteUserNatMultiClient(
		ctx,
		generator,
		receivePacketCallback,
		protocol.ProvideMode_Network,
		settings,
	)

	return multiClient, nil
}

type TestMultiClientGenerator struct {
	nextDestinations     func(count int, excludeDestinations []MultiHopId, rankMode string) (map[MultiHopId]DestinationStats, error)
	newClientArgs        func() (*MultiClientGeneratorClientArgs, error)
	removeClientArgs     func(args *MultiClientGeneratorClientArgs)
	removeClientWithArgs func(client *Client, args *MultiClientGeneratorClientArgs)
	newClientSettings    func() *ClientSettings
	newClient            func(ctx context.Context, args *MultiClientGeneratorClientArgs, clientSettings *ClientSettings) (*Client, error)
}

func (self *TestMultiClientGenerator) NextDestinations(count int, excludeDestinations []MultiHopId, rankMode string) (map[MultiHopId]DestinationStats, error) {
	return self.nextDestinations(count, excludeDestinations, rankMode)
}

func (self *TestMultiClientGenerator) NewClientArgs() (*MultiClientGeneratorClientArgs, error) {
	return self.newClientArgs()
}

func (self *TestMultiClientGenerator) RemoveClientArgs(args *MultiClientGeneratorClientArgs) {
	self.removeClientArgs(args)
}

func (self *TestMultiClientGenerator) RemoveClientWithArgs(client *Client, args *MultiClientGeneratorClientArgs) {
	self.removeClientWithArgs(client, args)
}

func (self *TestMultiClientGenerator) NewClientSettings() *ClientSettings {
	return self.newClientSettings()
}

func (self *TestMultiClientGenerator) NewClient(ctx context.Context, args *MultiClientGeneratorClientArgs, clientSettings *ClientSettings) (*Client, error) {
	return self.newClient(ctx, args, clientSettings)
}

func (self *TestMultiClientGenerator) FixedDestinationSize() (int, bool) {
	return 1, true
}

func TestMultiClientChannelWindowStats(t *testing.T) {
	// ensure that the bucket counts are bounded
	// if this is broken, the coalesce logic is broken and there will be a memory issue

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	timeout := 10 * time.Second

	m := 6
	n := 6
	repeatCount := 6
	parallelCount := 6

	generator := &TestMultiClientGenerator{
		nextDestinations: func(count int, excludedDestinations []MultiHopId, rankMode string) (map[MultiHopId]DestinationStats, error) {
			// not used
			return nil, nil
		},
		newClientArgs: func() (*MultiClientGeneratorClientArgs, error) {
			args := &MultiClientGeneratorClientArgs{
				ClientId:   NewId(),
				ClientAuth: nil,
			}
			return args, nil
		},
		removeClientArgs: func(args *MultiClientGeneratorClientArgs) {
			// do nothing
		},
		removeClientWithArgs: func(client *Client, args *MultiClientGeneratorClientArgs) {
			// do nothing
		},
		newClientSettings: DefaultClientSettings,
		newClient: func(ctx context.Context, args *MultiClientGeneratorClientArgs, clientSettings *ClientSettings) (*Client, error) {
			client := NewClient(ctx, args.ClientId, NewNoContractClientOob(), clientSettings)
			return client, nil
		},
	}

	clientReceivePacket := func(client *multiClientChannel, source TransferPath, provideMode protocol.ProvideMode, ipPath *IpPath, packet []byte) {
		// Do nothing
	}

	contractStatus := func(contractStatus *ContractStatus) {
		// Do nothing
	}

	settings := DefaultMultiClientSettings()
	settings.StatsWindowBucketDuration = 100 * time.Millisecond
	settings.StatsWindowDuration = 1 * time.Second
	settings.BlackholeTimeout = 300 * time.Second

	// the coalesce logic trims from the last event in a bucket
	// if events are uniformly distributed in a bucket, this means there will be an extra bucket
	maxBucketCount := 1 + int(math.Ceil(float64(settings.StatsWindowDuration)/float64(settings.StatsWindowBucketDuration)))

	args, err := generator.NewClientArgs()
	channelArgs := &multiClientChannelArgs{
		MultiClientGeneratorClientArgs: *args,
		Destination:                    RequireMultiHopId(NewId()),
		DestinationStats: DestinationStats{
			EstimatedBytesPerSecond: 0,
			Tier:                    0,
		},
	}
	assert.Equal(t, nil, err)

	clientChannel, err := newMultiClientChannel(ctx, channelArgs, generator, clientReceivePacket, DefaultSecurityPolicy(ctx), contractStatus, func(contractStatsEvents []*ContractStatsEvent) {}, nil, settings)
	assert.Equal(t, nil, err)

	cancelCtxs := []context.Context{}

	for p := 0; p < parallelCount; p += 1 {
		cancelCtx, cancel := context.WithCancel(ctx)
		cancelCtxs = append(cancelCtxs, cancelCtx)
		go func() {
			defer cancel()
			for endTime := time.Now().Add(timeout); time.Now().Before(endTime); {
				for s := 0; s < m; s += 1 {
					for i := 0; i < n; i += 1 {
						for j := 0; j < n; j += 1 {
							for k := 0; k < n; k += 1 {
								for a := 0; a < repeatCount; a += 1 {
									packet, _ := udp4Packet(s, i, j, k)
									ipPath, err := ParseIpPath(packet)
									assert.Equal(t, nil, err)

									clientChannel.addSendNack(1)
									clientChannel.addSendAck(1)
									clientChannel.addReceiveAck(1)
									clientChannel.addSource(ipPath)

								}
							}
						}
					}
				}
			}
		}()
	}

	for _, cancelCtx := range cancelCtxs {
		<-cancelCtx.Done()
	}

	stats, err := clientChannel.windowStatsWithCoalesce(false)
	assert.Equal(t, nil, err)

	// [1, maxBucketCount]
	assert.Equal(t, true, 1 <= stats.bucketCount)
	assert.Equal(t, true, stats.bucketCount <= maxBucketCount)

	stats, err = clientChannel.WindowStats()
	assert.Equal(t, nil, err)

	// [1, maxBucketCount]
	assert.Equal(t, true, 1 <= stats.bucketCount)
	assert.Equal(t, true, stats.bucketCount <= maxBucketCount)
}
