package connect

import (
	"context"
	"sync"
	"time"

	// "reflect"
	"errors"
	"fmt"
	"math"
	mathrand "math/rand"
	"slices"
	"strings"

	"golang.org/x/exp/maps"

	"google.golang.org/protobuf/proto"

	"github.com/golang/glog"

	"github.com/urnetwork/connect/v2025/protocol"
)

// multi client is a sender approach to mitigate bad destinations
// it maintains a window of compatible clients chosen using specs
// (e.g. from a desription of the intent of use)
// - the clients are rate limited by the number of outstanding acks (nacks)
// - the size of allowed outstanding nacks increases with each ack,
// scaling up successful destinations to use the full transfer buffer
// - the clients are chosen with probability weighted by their
// net frame count statistics (acks - nacks)

// the following functions handle moving clients in and out of the window:
// - `resize`
//   The goal of the resize is to meet a target window size based on the number
//   of different source ip:port per destination ip:port.
//   Two statistics are used: effective bytes per second ([ack used])
//     and expected bytes per second ([capacity]-[ack used]-[unacked used]).
//   Unhealthy clients are removed from the window based on low effective stats,
//   unless the window is fixed size.
//   Fundamentally this approach can't tell the difference between an
//   unhealthy client and an idle client, so the norm is to continually change clients
//   in the lull after a burst of usage.
// - `detectBlackhole`
//   When a client acks traffic but does not return traffic,
//   it gets labeled a black hole. Black hole clients may be malicious
//   or have network filtering. Black hole clients are removed.
// - `ping`
//   When a client is idle it must continually ack ping requests.
//   Clients that fail to ack are removed.

// TODO surface window stats to show to users

type clientReceivePacketFunction func(client *multiClientChannel, source TransferPath, provideMode protocol.ProvideMode, ipPath *IpPath, packet []byte)

type DestinationStats struct {
	EstimatedBytesPerSecond ByteCount
	Tier                    int
}

type WindowType int

func (self WindowType) RankMode() string {
	switch self {
	case WindowTypeQuality:
		return "quality"
	case WindowTypeSpeed:
		return "speed"
	default:
		return ""
	}
}

const (
	WindowTypeQuality WindowType = 0
	WindowTypeSpeed   WindowType = 1
)

// for each `NewClientArgs`,
//
//	`RemoveClientWithArgs` will be called if a client was created for the args,
//	else `RemoveClientArgs`
type MultiClientGenerator interface {
	// path -> estimated byte count per second
	// the enumeration should typically
	// 1. not repeat final destination ids from any path
	// 2. not repeat intermediary elements from any path
	NextDestinations(count int, excludeDestinations []MultiHopId, rankMode string) (map[MultiHopId]DestinationStats, error)
	// client id, client auth
	NewClientArgs() (*MultiClientGeneratorClientArgs, error)
	RemoveClientArgs(args *MultiClientGeneratorClientArgs)
	RemoveClientWithArgs(client *Client, args *MultiClientGeneratorClientArgs)
	NewClientSettings() *ClientSettings
	NewClient(ctx context.Context, args *MultiClientGeneratorClientArgs, clientSettings *ClientSettings) (*Client, error)
	FixedDestinationSize() (int, bool)
}

func DefaultMultiClientSettings() *MultiClientSettings {
	return &MultiClientSettings{
		SequenceIdleTimeout: 30 * time.Second,

		WindowSizes: map[WindowType]WindowSizeSettings{
			WindowTypeQuality: WindowSizeSettings{
				WindowSizeMin: 4,
				// TODO increase this when p2p is deployed
				WindowSizeMinP2pOnly: 0,
				WindowSizeMax:        8,
				// reconnects per source
				WindowSizeReconnectScale: 1.0,
			},
			WindowTypeSpeed: WindowSizeSettings{
				WindowSizeMin: 1,
				// TODO increase this when p2p is deployed
				WindowSizeMinP2pOnly: 0,
				WindowSizeMax:        2,
				WindowSizeUseMax:     1,
				// reconnects per source
				WindowSizeReconnectScale: 1.0,
			},
		},
		SendRetryTimeout:           200 * time.Millisecond,
		PingWriteTimeout:           5 * time.Second,
		CPingWriteTimeout:          5 * time.Second,
		CPingMaxByteCountPerSecond: kib(8),
		// the initial ping includes creating the transports and contract
		// ease up the timeout until perf issues are fully resolved
		PingTimeout:  15 * time.Second,
		CPingTimeout: 15 * time.Second,
		// a lower ack timeout helps cycle through bad providers faster
		AckTimeout:             15 * time.Second,
		BlackholeTimeout:       15 * time.Second,
		WindowResizeTimeout:    15 * time.Second,
		StatsWindowGraceperiod: 30 * time.Second,
		StatsWindowMaxEstimatedByteCountPerSecond:      mib(8),
		StatsWindowMaxEffectiveByteCountPerSecondScale: 0.2,
		StatsWindowEntropy:                             0.1,
		WindowExpandTimeout:                            15 * time.Second,
		// WindowExpandBlockTimeout: 5 * time.Second,
		WindowExpandBlockCount: 8,
		// wait this time before enumerating potential clients again
		WindowEnumerateEmptyTimeout:  60 * time.Second,
		WindowEnumerateErrorTimeout:  1 * time.Second,
		WindowExpandScale:            2.0,
		WindowCollapseScale:          0.8,
		WindowExpandMaxOvershotScale: 4.0,
		WindowCollapseBeforeExpand:   false,
		WindowRevisitTimeout:         2 * time.Minute,
		StatsWindowDuration:          15 * time.Second,
		StatsWindowBucketDuration:    1 * time.Second,
		StatsSampleWeightsCount:      8,
		StatsSourceCountSelection:    0.95,
		// ClientAffinityTimeout:        0 * time.Second,

		MultiRaceSetOnNoResponseTimeout:      1000 * time.Millisecond,
		MultiRaceSetOnResponseTimeout:        100 * time.Millisecond,
		MultiRaceClientSentPacketMaxCount:    16,
		MultiRaceClientPacketMaxCount:        4,
		MultiRacePacketMaxCount:              16,
		MultiRaceClientEarlyCompleteFraction: 0.25,
		// TODO on platforms with more memory, increase this
		MultiRaceClientCount: 4,

		StatsWindowMaxUnhealthyDuration:                  15 * time.Second,
		StatsWindowMinHealthyEffectiveByteCountPerSecond: kib(2),

		ProtocolVersion: DefaultProtocolVersion,

		RemoteUserNatMultiClientMonitorSettings: *DefaultRemoteUserNatMultiClientMonitorSettings(),
	}
}

type MultiClientSettings struct {
	SequenceIdleTimeout time.Duration
	WindowSizes         map[WindowType]WindowSizeSettings
	// ClientNackInitialLimit int
	// ClientNackMaxLimit int
	// ClientNackScale float64
	// ClientWriteTimeout time.Duration
	// SendTimeout time.Duration
	// WriteTimeout time.Duration
	SendRetryTimeout                               time.Duration
	PingWriteTimeout                               time.Duration
	CPingWriteTimeout                              time.Duration
	CPingMaxByteCountPerSecond                     ByteCount
	PingTimeout                                    time.Duration
	CPingTimeout                                   time.Duration
	AckTimeout                                     time.Duration
	BlackholeTimeout                               time.Duration
	WindowResizeTimeout                            time.Duration
	StatsWindowGraceperiod                         time.Duration
	StatsWindowMaxEstimatedByteCountPerSecond      ByteCount
	StatsWindowMaxEffectiveByteCountPerSecondScale float32
	StatsWindowEntropy                             float32
	WindowExpandTimeout                            time.Duration
	// WindowExpandBlockTimeout     time.Duration
	WindowExpandBlockCount       int
	WindowEnumerateEmptyTimeout  time.Duration
	WindowEnumerateErrorTimeout  time.Duration
	WindowExpandScale            float64
	WindowCollapseScale          float64
	WindowExpandMaxOvershotScale float64
	WindowCollapseBeforeExpand   bool
	WindowRevisitTimeout         time.Duration
	StatsWindowDuration          time.Duration
	StatsWindowBucketDuration    time.Duration
	StatsSampleWeightsCount      int
	StatsSourceCountSelection    float64
	// lower affinity is more private
	// however, there may be some applications that assume the same ip across multiple connections
	// in those cases, we would need some small affinity
	// ClientAffinityTimeout time.Duration

	// time since first send to end the race, if no response
	MultiRaceSetOnNoResponseTimeout time.Duration
	// time after the first response to end the race
	MultiRaceSetOnResponseTimeout        time.Duration
	MultiRaceClientSentPacketMaxCount    int
	MultiRaceClientPacketMaxCount        int
	MultiRacePacketMaxCount              int
	MultiRaceClientEarlyCompleteFraction float32
	MultiRaceClientCount                 int

	StatsWindowMaxUnhealthyDuration                  time.Duration
	StatsWindowMinHealthyEffectiveByteCountPerSecond ByteCount

	ProtocolVersion int

	RemoteUserNatMultiClientMonitorSettings
}

type WindowSizeSettings struct {
	WindowSizeMin int
	// the minimumum number of items in the windows that must be connected via p2p only
	WindowSizeMinP2pOnly int
	WindowSizeMax        int
	WindowSizeUseMax     int
	// reconnects per source
	WindowSizeReconnectScale float64
}

type receivePacket struct {
	Source      TransferPath
	ProvideMode protocol.ProvideMode
	IpPath      *IpPath
	Packet      []byte
	Pooled      bool
}

type RemoteUserNatMultiClient struct {
	ctx    context.Context
	cancel context.CancelFunc

	generator MultiClientGenerator

	receivePacketCallback ReceivePacketFunction

	settings *MultiClientSettings

	windows map[WindowType]*multiClientWindow
	monitor MultiClientMonitor

	securityPolicyStats   *securityPolicyStats
	securityPolicy        SecurityPolicy
	ingressSecurityPolicy SecurityPolicy

	// the provide mode of the source packets
	// for locally generated packets this is `ProvideMode_Network`
	provideMode protocol.ProvideMode

	stateLock      sync.Mutex
	ip4PathUpdates map[Ip4Path]*multiClientChannelUpdate
	ip6PathUpdates map[Ip6Path]*multiClientChannelUpdate
	clientUpdates  map[*multiClientChannel]map[*multiClientChannelUpdate]bool
}

func NewRemoteUserNatMultiClientWithDefaults(
	ctx context.Context,
	generator MultiClientGenerator,
	receivePacketCallback ReceivePacketFunction,
	provideMode protocol.ProvideMode,
) *RemoteUserNatMultiClient {
	return NewRemoteUserNatMultiClient(
		ctx,
		generator,
		receivePacketCallback,
		provideMode,
		DefaultMultiClientSettings(),
	)
}

func NewRemoteUserNatMultiClient(
	ctx context.Context,
	generator MultiClientGenerator,
	receivePacketCallback ReceivePacketFunction,
	provideMode protocol.ProvideMode,
	settings *MultiClientSettings,
) *RemoteUserNatMultiClient {
	cancelCtx, cancel := context.WithCancel(ctx)

	securityPolicyStats := DefaultSecurityPolicyStats()

	multiClient := &RemoteUserNatMultiClient{
		ctx:                   cancelCtx,
		cancel:                cancel,
		generator:             generator,
		receivePacketCallback: receivePacketCallback,
		settings:              settings,
		windows:               map[WindowType]*multiClientWindow{},
		securityPolicyStats:   securityPolicyStats,
		securityPolicy:        DefaultEgressSecurityPolicyWithStats(securityPolicyStats),
		ingressSecurityPolicy: DefaultIngressSecurityPolicyWithStats(securityPolicyStats),
		provideMode:           provideMode,
		ip4PathUpdates:        map[Ip4Path]*multiClientChannelUpdate{},
		ip6PathUpdates:        map[Ip6Path]*multiClientChannelUpdate{},
		clientUpdates:         map[*multiClientChannel]map[*multiClientChannelUpdate]bool{},
	}

	multiClient.windows[WindowTypeQuality] = newMultiClientWindow(
		cancelCtx,
		cancel,
		generator,
		multiClient.clientReceivePacket,
		multiClient.ingressSecurityPolicy,
		multiClient.removeClient,
		WindowTypeQuality,
		settings,
	)
	multiClient.windows[WindowTypeSpeed] = newMultiClientWindow(
		cancelCtx,
		cancel,
		generator,
		multiClient.clientReceivePacket,
		multiClient.ingressSecurityPolicy,
		multiClient.removeClient,
		WindowTypeSpeed,
		settings,
	)

	monitors := []MultiClientMonitor{}
	for _, window := range multiClient.windows {
		monitors = append(monitors, window.monitor)
	}
	multiClient.monitor = NewMergedMultiClientMonitor(monitors)

	return multiClient
}

func (self *RemoteUserNatMultiClient) SecurityPolicyStats(reset bool) SecurityPolicyStats {
	return self.securityPolicyStats.Stats(reset)
}

func (self *RemoteUserNatMultiClient) Monitor() MultiClientMonitor {
	return self.monitor
}

func (self *RemoteUserNatMultiClient) AddContractStatusCallback(contractStatusCallback ContractStatusFunction) func() {
	subs := []func(){}
	for _, window := range self.windows {
		sub := window.AddContractStatusCallback(contractStatusCallback)
		subs = append(subs, sub)
	}
	return func() {
		for _, sub := range subs {
			sub()
		}
	}
}

func (self *RemoteUserNatMultiClient) updateClientPath(ipPath *IpPath, callback func(*multiClientChannelUpdate)) {
	update, client := self.reserveUpdate(ipPath)
	callback(update)
	self.updateClient(update, client)
}

func (self *RemoteUserNatMultiClient) reserveUpdate(ipPath *IpPath) (*multiClientChannelUpdate, *multiClientChannel) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	waitForIdle := func(update *multiClientChannelUpdate) {
		for {
			select {
			case <-update.ctx.Done():
				return
			default:
			}

			var idleTimeout time.Duration
			func() {
				self.stateLock.Lock()
				defer self.stateLock.Unlock()

				idleTimeout = update.activityTime.Add(self.settings.SequenceIdleTimeout).Sub(time.Now())
			}()
			if idleTimeout <= 0 {
				return
			} else {
				select {
				case <-update.ctx.Done():
					return
				case <-time.After(idleTimeout):
				}
			}
		}
	}

	rst := func(client *multiClientChannel) {
		if client != nil {
			// rst to destination
			if packet, ok := ipOosRst(ipPath); ok {
				client.Send(&parsedPacket{
					packet: packet,
					ipPath: ipPath,
				}, 0)
			}
		}
		// rst to source
		if packet, ok := ipOosRst(ipPath.Reverse()); ok {
			self.receivePacketCallback(TransferPath{}, protocol.ProvideMode_Network, ipPath, packet)
		}
	}

	switch ipPath.Version {
	case 4:
		ip4Path := ipPath.ToIp4Path()
		update, ok := self.ip4PathUpdates[ip4Path]
		if !ok || update.IsDone() {
			update = newMultiClientChannelUpdate(self.ctx, ipPath)
			go HandleError(func() {
				defer update.cancel()

				waitForIdle(update)

				var client *multiClientChannel
				func() {
					self.stateLock.Lock()
					defer self.stateLock.Unlock()

					client = update.client

					if self.ip4PathUpdates[ip4Path] == update {
						delete(self.ip4PathUpdates, ip4Path)
					}
					if client != nil {
						if updates, ok := self.clientUpdates[client]; ok {
							delete(updates, update)
							if len(updates) == 0 {
								delete(self.clientUpdates, client)
							}
						}
					}
				}()

				select {
				case <-self.ctx.Done():
				default:
					rst(client)
				}
			}, update.cancel)
			self.ip4PathUpdates[ip4Path] = update
		}
		return update, update.client
	case 6:
		ip6Path := ipPath.ToIp6Path()
		update, ok := self.ip6PathUpdates[ip6Path]
		if !ok || update.IsDone() {
			update = newMultiClientChannelUpdate(self.ctx, ipPath)
			go HandleError(func() {
				defer update.cancel()

				waitForIdle(update)

				var client *multiClientChannel
				func() {
					self.stateLock.Lock()
					defer self.stateLock.Unlock()

					client = update.client

					if self.ip6PathUpdates[ip6Path] == update {
						delete(self.ip6PathUpdates, ip6Path)
					}
					if client != nil {
						if updates, ok := self.clientUpdates[client]; ok {
							delete(updates, update)
							if len(updates) == 0 {
								delete(self.clientUpdates, client)
							}
						}
					}
				}()

				select {
				case <-self.ctx.Done():
				default:
					rst(client)
				}
			}, update.cancel)
			self.ip6PathUpdates[ip6Path] = update
		}
		return update, update.client
	default:
		panic(fmt.Errorf("Bad protocol version %d", ipPath.Version))
	}
}

func (self *RemoteUserNatMultiClient) updateClient(update *multiClientChannelUpdate, previousClient *multiClientChannel) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	client := update.client
	update.activityTime = time.Now()

	if previousClient != client {
		if previousClient != nil {
			if updates, ok := self.clientUpdates[previousClient]; ok {
				delete(updates, update)
				if len(updates) == 0 {
					delete(self.clientUpdates, previousClient)
				}
			}
		}
		if client != nil && !client.IsDone() {
			updates, ok := self.clientUpdates[client]
			if !ok {
				updates = map[*multiClientChannelUpdate]bool{}
				self.clientUpdates[client] = updates
			}
			updates[update] = true
		}
	}
}

// remove a client from all updates
func (self *RemoteUserNatMultiClient) removeClient(client *multiClientChannel) {
	rstPackets := []*receivePacket{}

	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		// note client must be marked as done, otherwise it may be re-added by updates in flight
		if !client.IsDone() {
			glog.Errorf("[multi]removed client that is not marked as done. This might lead to memory leak.")
		}

		if updates, ok := self.clientUpdates[client]; ok {
			delete(self.clientUpdates, client)
			for update, _ := range updates {
				if update.client == client {
					update.client = nil

					if packet, ok := ipOosRst(update.ipPath.Reverse()); ok {
						rstPacket := &receivePacket{
							Source:      TransferPath{},
							ProvideMode: protocol.ProvideMode_Network,
							IpPath:      update.ipPath,
							Packet:      packet,
						}
						rstPackets = append(rstPackets, rstPacket)
					}
				} else {
					glog.Errorf("[multi]update associated with incorrect client")
				}
			}
		}
	}()

	select {
	case <-self.ctx.Done():
	default:
		for _, p := range rstPackets {
			self.receivePacketCallback(p.Source, p.ProvideMode, p.IpPath, p.Packet)
		}
	}
}

// `SendPacketFunction`
func (self *RemoteUserNatMultiClient) SendPacket(
	source TransferPath,
	provideMode protocol.ProvideMode,
	packet []byte,
	timeout time.Duration,
) bool {
	minRelationship := max(provideMode, self.provideMode)

	ipPath, r, err := self.securityPolicy.Inspect(minRelationship, packet)
	if err != nil {
		glog.Infof("[multi]send bad packet = %s\n", err)
		return false
	}
	switch r {
	case SecurityPolicyResultAllow:
		parsedPacket := &parsedPacket{
			packet: packet,
			ipPath: ipPath,
		}
		return self.sendPacket(source, provideMode, parsedPacket, timeout)
	default:
		// TODO upgrade port 53 and port 80 here with protocol specific conversions
		glog.Infof("[multi]drop packet ipv%d p%v -> %s:%d\n", ipPath.Version, ipPath.Protocol, ipPath.DestinationIp, ipPath.DestinationPort)
		return false
	}
}

// ordered by choice descending
func (self *RemoteUserNatMultiClient) selectWindowTypes(sendPacket *parsedPacket) []WindowType {
	// - web traffic is routed to quality providers
	// - all other traffic is routed to speed providers
	if sendPacket.ipPath.DestinationPort == 443 {
		return []WindowType{WindowTypeQuality, WindowTypeSpeed}
	}
	return []WindowType{WindowTypeSpeed, WindowTypeQuality}
}

func (self *RemoteUserNatMultiClient) sendPacket(
	source TransferPath,
	provideMode protocol.ProvideMode,
	sendPacket *parsedPacket,
	timeout time.Duration,
) (success bool) {
	self.updateClientPath(sendPacket.ipPath, func(update *multiClientChannelUpdate) {
		enterTime := time.Now()

		currentClient := func() *multiClientChannel {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			return update.client
		}
		sendCurrent := func() bool {
			for client := currentClient(); client != nil; {
				p := &parsedPacket{
					packet: sendPacket.packet,
					ipPath: sendPacket.ipPath,
				}

				var err error
				success, err = client.SendDetailed(p, timeout)
				// note we do not check success also because it may be normal to drop packets under load
				if err == nil {
					return true
				}

				func() {
					self.stateLock.Lock()
					defer self.stateLock.Unlock()
					if client == update.client {
						update.client = nil
						client = nil
					} else {
						// a new client was set, try the new client
						client = update.client
					}
				}()
			}
			return false
		}

		if sendCurrent() {
			return
		}

		// find a new client

		raceClients := func(orderedClients []*multiClientChannel, sendTimeout time.Duration) {
			successCount := 0
			for _, client := range orderedClients {
				select {
				case <-update.ctx.Done():
					return
				default:
				}

				done := false

				func() {
					self.stateLock.Lock()
					defer self.stateLock.Unlock()

					if update.client != nil {
						// another client already chosen, done
						done = true
						return
					}

					update.initRace()
					race := update.race
					state := race.clientStates[client]

					if state == nil {
						state = &multiClientChannelRaceClientState{
							sendTime: time.Now(),
						}
						race.clientStates[client] = state
					}
				}()
				if done {
					return
				}

				p := &parsedPacket{
					packet: MessagePoolShareReadOnly(sendPacket.packet),
					ipPath: sendPacket.ipPath,
				}
				if client.Send(p, sendTimeout) {
					successCount += 1
					success = true
					var abandonedClients []*multiClientChannel
					func() {
						self.stateLock.Lock()
						defer self.stateLock.Unlock()

						if update.client != nil {
							// another client already chosen, done
							done = true
							return
						}

						update.initRace()
						race := update.race
						state := race.clientStates[client]
						race.sentPacketCount += 1
						bufferExceeded := state != nil && self.settings.MultiRaceSetOnNoResponseTimeout <= time.Now().Sub(state.sendTime) || self.settings.MultiRaceClientSentPacketMaxCount < race.sentPacketCount
						if race.packetCount == 0 && bufferExceeded {
							// no client response in timeout, lock in this client
							// this happens for example when the client only sends and does not receive (e.g. udp send)

							for abandonedClient, _ := range race.clientStates {
								if abandonedClient != client {
									abandonedClients = append(abandonedClients, abandonedClient)
								}
							}

							update.clearRace()
							update.client = client

							done = true
							return
						} else {
							if self.settings.MultiRaceClientCount <= successCount {
								done = true
								return
							}
							// else continue sending to all clients
						}
					}()
					if 0 < len(abandonedClients) {
						if rstPacket, ok := ipOosRst(sendPacket.ipPath); ok {
							for _, abandonedClient := range abandonedClients {
								abandonedClient.Send(&parsedPacket{
									packet: rstPacket,
									ipPath: sendPacket.ipPath,
								}, 0)
							}
						}
					}
					if done {
						return
					}
				} else {
					MessagePoolReturn(p.packet)
				}
			}
		}

		windowTypes := self.selectWindowTypes(sendPacket)

		coalesceOrderedClients := func() []*multiClientChannel {
			for _, windowType := range windowTypes {
				if window, ok := self.windows[windowType]; ok {
					orderedClients := window.OrderedClients()
					if 0 < len(orderedClients) {
						return orderedClients
					}
				}
			}
			return []*multiClientChannel{}
		}

		raceClients(coalesceOrderedClients(), 0)
		if success {
			MessagePoolReturn(sendPacket.packet)
			return
		}

		for {
			select {
			case <-update.ctx.Done():
				return
			default:
			}

			if sendCurrent() {
				return
			}

			var retryTimeout time.Duration
			if 0 <= timeout {
				remainingTimeout := enterTime.Add(timeout).Sub(time.Now())

				if remainingTimeout <= 0 {
					// drop
					return
				}

				retryTimeout = min(remainingTimeout, self.settings.SendRetryTimeout)
			} else {
				retryTimeout = self.settings.SendRetryTimeout
			}

			if orderedClients := coalesceOrderedClients(); 0 < len(orderedClients) {
				// distribute the timeout evenly via wait
				retryTimeoutPerClient := retryTimeout / time.Duration(len(orderedClients))
				raceClients(orderedClients, retryTimeoutPerClient)
				if success {
					MessagePoolReturn(sendPacket.packet)
					return
				}
			} else {
				select {
				case <-update.ctx.Done():
					// drop
					return
				case <-time.After(retryTimeout):
				}
			}
		}
	})
	return
}

// clientReceivePacketFunction
func (self *RemoteUserNatMultiClient) clientReceivePacket(
	sourceClient *multiClientChannel,
	source TransferPath,
	provideMode protocol.ProvideMode,
	ipPath *IpPath,
	packet []byte,
) {
	// ipPath, err := ParseIpPath(packet)
	// if err != nil {
	// 	// bad ip packet, drop
	// 	return
	// }

	ipPath = ipPath.Reverse()

	var abandonedClients []*multiClientChannel
	var receivePackets []*receivePacket
	var returnPackets []*receivePacket
	self.updateClientPath(ipPath, func(update *multiClientChannelUpdate) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		client := update.client

		if client == sourceClient {
			p := &receivePacket{
				Source:      source,
				ProvideMode: provideMode,
				IpPath:      ipPath,
				Packet:      packet,
			}
			receivePackets = []*receivePacket{p}
		} else if client != nil {
			// another client already chosen, drop
		} else if race := update.race; race == nil {
			// no race, no client, drop
			glog.Infof("[multi]receive no race and no client")
		} else if state, ok := race.clientStates[sourceClient]; !ok {
			// this client is not part of the race, drop
			glog.Infof("[multi]receive client not part of race")
		} else if len(state.packets) < self.settings.MultiRaceClientPacketMaxCount && race.packetCount < self.settings.MultiRacePacketMaxCount {
			// note that `MessagePoolShare*` will not work on the packet
			// since the packet is typically a slice of the received transfer frame
			ipPathCopy := ipPath.Copy()
			packetCopy, pooled := MessagePoolCopyDetailed(packet)
			receivePacket := &receivePacket{
				Source:      source,
				ProvideMode: provideMode,
				IpPath:      ipPathCopy,
				Packet:      packetCopy,
				Pooled:      pooled,
			}
			state.packets = append(state.packets, receivePacket)
			if 1 == len(state.packets) {
				state.receiveTime = time.Now()
			}
			race.packetCount += 1
			if race.packetCount == 1 {
				// schedule the race evaluation on first packet
				earlyComplete := race.completeMonitor.NotifyChannel()
				// copy the ip path since the first packet may not be ultimately retained to the end of the race
				self.scheduleCompleteRace(ipPathCopy, race, earlyComplete)
			}
			if len(state.packets) == 1 {
				race.clientsWithPacketCount += 1
				if int(float32(len(race.clientStates))*self.settings.MultiRaceClientEarlyCompleteFraction) <= race.clientsWithPacketCount {
					race.completeMonitor.NotifyAll()
				}
			}
		} else {
			// race buffer limits exceeded, end the race immediately
			glog.Infof("[multi]receive race buffer limit reached")

			for abandonedClient, abandonedState := range race.clientStates {
				if abandonedClient != client {
					abandonedClients = append(abandonedClients, abandonedClient)
					for _, p := range abandonedState.packets {
						if p.Pooled {
							p.Pooled = false
							returnPackets = append(returnPackets, p)
						}
					}
				}
			}

			update.clearRace()
			update.client = client
			receivePacket := &receivePacket{
				Source:      source,
				ProvideMode: provideMode,
				IpPath:      ipPath,
				Packet:      packet,
			}
			receivePackets = append(state.packets, receivePacket)
			for _, p := range receivePackets {
				if p.Pooled {
					p.Pooled = false
					returnPackets = append(returnPackets, p)
				}
			}
		}
	})
	if 0 < len(abandonedClients) {
		if rstPacket, ok := ipOosRst(ipPath); ok {
			for _, abandonedClient := range abandonedClients {
				abandonedClient.Send(&parsedPacket{
					packet: rstPacket,
					ipPath: ipPath,
				}, 0)
			}
		}
	}
	for _, p := range receivePackets {
		self.receivePacketCallback(p.Source, p.ProvideMode, p.IpPath, p.Packet)
	}
	for _, p := range returnPackets {
		MessagePoolReturn(p.Packet)
	}
}

func (self *RemoteUserNatMultiClient) scheduleCompleteRace(
	ipPath *IpPath,
	race *multiClientChannelUpdateRace,
	earlyComplete <-chan struct{},
) {
	go HandleError(func() {
		// wait for the race to finish, then choose

		select {
		case <-race.ctx.Done():
			return
		case <-earlyComplete:
		case <-time.After(self.settings.MultiRaceSetOnResponseTimeout):
		}

		var abandonedClients []*multiClientChannel
		var receivePackets []*receivePacket
		var returnPackets []*receivePacket
		self.updateClientPath(ipPath, func(update *multiClientChannelUpdate) {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()

			if update.client == nil && update.race == race {
				// weighted shuffle clients by rtt
				orderedClients := []*multiClientChannel{}
				weights := map[*multiClientChannel]float32{}
				for client, state := range race.clientStates {
					if 0 < len(state.packets) {
						orderedClients = append(orderedClients, client)
						rtt := state.receiveTime.Sub(state.sendTime)
						weights[client] = float32(rtt / time.Millisecond)
					}
				}
				WeightedShuffleWithEntropy(orderedClients, weights, self.settings.StatsWindowEntropy)
				// the last is the lowest rtt
				client := orderedClients[len(orderedClients)-1]

				for abandonedClient, abandonedState := range race.clientStates {
					if abandonedClient != client {
						abandonedClients = append(abandonedClients, abandonedClient)
						for _, p := range abandonedState.packets {
							if p.Pooled {
								p.Pooled = false
								returnPackets = append(returnPackets, p)
							}
						}
					}
				}

				update.clearRace()
				update.client = client
				receivePackets = race.clientStates[update.client].packets
				for _, p := range receivePackets {
					if p.Pooled {
						p.Pooled = false
						returnPackets = append(returnPackets, p)
					}
				}
			}
			// else a client was already chosen, ignore
		})
		if 0 < len(abandonedClients) {
			if rstPacket, ok := ipOosRst(ipPath); ok {
				for _, abandonedClient := range abandonedClients {
					abandonedClient.Send(&parsedPacket{
						packet: rstPacket,
						ipPath: ipPath,
					}, 0)
				}
			}
		}
		for _, p := range receivePackets {
			self.receivePacketCallback(p.Source, p.ProvideMode, p.IpPath, p.Packet)
		}
		for _, p := range returnPackets {
			MessagePoolReturn(p.Packet)
		}
	})
}

func (self *RemoteUserNatMultiClient) Shuffle() {
	for _, window := range self.windows {
		window.shuffle()
	}
}

func (self *RemoteUserNatMultiClient) Close() {
	self.cancel()
	for _, window := range self.windows {
		window.Close()
	}

	removedUpdates := []*multiClientChannelUpdate{}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		for _, update := range self.ip4PathUpdates {
			removedUpdates = append(removedUpdates, update)
			// update.Close()
		}
		for _, update := range self.ip6PathUpdates {
			removedUpdates = append(removedUpdates, update)
			// update.Close()
		}
		clear(self.ip4PathUpdates)
		clear(self.ip6PathUpdates)
		// clear(self.updateIp4Paths)
		// clear(self.updateIp6Paths)
		// clear(clientUpdates)
	}()
	for _, update := range removedUpdates {
		update.Close()
	}
}

type multiClientChannelUpdate struct {
	ctx    context.Context
	cancel context.CancelFunc

	client       *multiClientChannel
	race         *multiClientChannelUpdateRace
	activityTime time.Time
	ipPath       *IpPath
}

func newMultiClientChannelUpdate(ctx context.Context, ipPath *IpPath) *multiClientChannelUpdate {
	cancelCtx, cancel := context.WithCancel(ctx)
	return &multiClientChannelUpdate{
		ctx:          cancelCtx,
		cancel:       cancel,
		activityTime: time.Now(),
		ipPath:       ipPath,
	}
}

func (self *multiClientChannelUpdate) initRace() {
	if self.race == nil {
		self.race = newMultiClientChannelUpdateRace(self.ctx)
	}
}

func (self *multiClientChannelUpdate) clearRace() {
	if self.race != nil {
		self.race.Close()
		self.race = nil
	}
}

func (self *multiClientChannelUpdate) IsDone() bool {
	select {
	case <-self.ctx.Done():
		return true
	default:
		return false
	}
}

func (self *multiClientChannelUpdate) Close() {
	self.cancel()
	self.client = nil
	self.clearRace()
}

type multiClientChannelUpdateRace struct {
	ctx                    context.Context
	cancel                 context.CancelFunc
	clientStates           map[*multiClientChannel]*multiClientChannelRaceClientState
	sentPacketCount        int
	packetCount            int
	clientsWithPacketCount int
	completeMonitor        *Monitor
}

func newMultiClientChannelUpdateRace(ctx context.Context) *multiClientChannelUpdateRace {
	cancelCtx, cancel := context.WithCancel(ctx)
	return &multiClientChannelUpdateRace{
		ctx:                    cancelCtx,
		cancel:                 cancel,
		clientStates:           map[*multiClientChannel]*multiClientChannelRaceClientState{},
		sentPacketCount:        0,
		packetCount:            0,
		clientsWithPacketCount: 0,
		completeMonitor:        NewMonitor(),
	}
}

func (self *multiClientChannelUpdateRace) Close() {
	self.cancel()
	// for _, state := range self.clientStates {
	// 	state.packets = nil
	// }
	// clear(self.clientStates)
}

type multiClientChannelRaceClientState struct {
	sendTime    time.Time
	receiveTime time.Time
	packets     []*receivePacket
}

type parsedPacket struct {
	packet []byte
	ipPath *IpPath
}

func newParsedPacket(packet []byte) (*parsedPacket, error) {
	ipPath, err := ParseIpPath(packet)
	if err != nil {
		return nil, err
	}
	return &parsedPacket{
		packet: packet,
		ipPath: ipPath,
	}, nil
}

type MultiClientGeneratorClientArgs struct {
	ClientId   Id
	ClientAuth *ClientAuth
	P2pOnly    bool
}

func DefaultApiMultiClientGeneratorSettings() *ApiMultiClientGeneratorSettings {
	return &ApiMultiClientGeneratorSettings{
		InitTimeout: 5 * time.Second,
	}
}

type ApiMultiClientGeneratorSettings struct {
	InitTimeout time.Duration
}

type ApiMultiClientGenerator struct {
	specs          []*ProviderSpec
	clientStrategy *ClientStrategy

	excludeClientIds []Id

	apiUrl      string
	byJwt       string
	platformUrl string

	deviceDescription       string
	deviceSpec              string
	appVersion              string
	sourceClientId          *Id
	clientSettingsGenerator func() *ClientSettings
	settings                *ApiMultiClientGeneratorSettings

	api *BringYourApi
}

func NewApiMultiClientGeneratorWithDefaults(
	ctx context.Context,
	specs []*ProviderSpec,
	clientStrategy *ClientStrategy,
	excludeClientIds []Id,
	apiUrl string,
	byJwt string,
	platformUrl string,
	deviceDescription string,
	deviceSpec string,
	appVersion string,
	sourceClientId *Id,
) *ApiMultiClientGenerator {
	return NewApiMultiClientGenerator(
		ctx,
		specs,
		clientStrategy,
		excludeClientIds,
		apiUrl,
		byJwt,
		platformUrl,
		deviceDescription,
		deviceSpec,
		appVersion,
		sourceClientId,
		DefaultClientSettings,
		DefaultApiMultiClientGeneratorSettings(),
	)
}

func NewApiMultiClientGenerator(
	ctx context.Context,
	specs []*ProviderSpec,
	clientStrategy *ClientStrategy,
	excludeClientIds []Id,
	apiUrl string,
	byJwt string,
	platformUrl string,
	deviceDescription string,
	deviceSpec string,
	appVersion string,
	sourceClientId *Id,
	clientSettingsGenerator func() *ClientSettings,
	settings *ApiMultiClientGeneratorSettings,
) *ApiMultiClientGenerator {
	api := NewBringYourApi(ctx, clientStrategy, apiUrl)
	api.SetByJwt(byJwt)

	return &ApiMultiClientGenerator{
		specs:                   specs,
		clientStrategy:          clientStrategy,
		excludeClientIds:        excludeClientIds,
		apiUrl:                  apiUrl,
		byJwt:                   byJwt,
		platformUrl:             platformUrl,
		deviceDescription:       deviceDescription,
		deviceSpec:              deviceSpec,
		appVersion:              appVersion,
		sourceClientId:          sourceClientId,
		clientSettingsGenerator: clientSettingsGenerator,
		settings:                settings,
		api:                     api,
	}
}

func (self *ApiMultiClientGenerator) NextDestinations(count int, excludeDestinations []MultiHopId, rankMode string) (map[MultiHopId]DestinationStats, error) {
	excludeClientIds := slices.Clone(self.excludeClientIds)
	excludeDestinationsIds := [][]Id{}
	for _, excludeDestination := range excludeDestinations {
		excludeDestinationsIds = append(excludeDestinationsIds, excludeDestination.Ids())
	}
	findProviders2 := &FindProviders2Args{
		Specs:               self.specs,
		ExcludeClientIds:    excludeClientIds,
		ExcludeDestinations: excludeDestinationsIds,
		Count:               count,
		RankMode:            rankMode,
	}

	result, err := self.api.FindProviders2Sync(findProviders2)
	if err != nil {
		return nil, err
	}

	destinations := map[MultiHopId]DestinationStats{}
	for _, provider := range result.Providers {
		ids := []Id{}
		if 0 < len(provider.IntermediaryIds) {
			ids = append(ids, provider.IntermediaryIds...)
		}
		ids = append(ids, provider.ClientId)
		// use the tail if the length exceeds the allowed maximum
		if MaxMultihopLength < len(ids) {
			ids = ids[len(ids)-MaxMultihopLength:]
		}
		if destination, err := NewMultiHopId(ids...); err == nil {
			destinations[destination] = DestinationStats{
				EstimatedBytesPerSecond: provider.EstimatedBytesPerSecond,
				Tier:                    provider.Tier,
			}
		}
	}

	return destinations, nil
}

func (self *ApiMultiClientGenerator) NewClientArgs() (*MultiClientGeneratorClientArgs, error) {
	auth := func() (string, error) {
		// note the derived client id will be inferred by the api jwt
		authNetworkClient := &AuthNetworkClientArgs{
			SourceClientId: self.sourceClientId,
			Description:    self.deviceDescription,
			DeviceSpec:     self.deviceSpec,
		}

		result, err := self.api.AuthNetworkClientSync(authNetworkClient)
		if err != nil {
			return "", err
		}

		if result.Error != nil {
			return "", errors.New(result.Error.Message)
		}

		return result.ByClientJwt, nil
	}

	if byJwtStr, err := auth(); err == nil {
		byJwt, err := ParseByJwtUnverified(byJwtStr)
		if err != nil {
			// in this case we cannot clean up the client because we don't know the client id
			panic(err)
		}

		clientAuth := &ClientAuth{
			ByJwt:      byJwtStr,
			InstanceId: NewId(),
			AppVersion: self.appVersion,
		}
		return &MultiClientGeneratorClientArgs{
			ClientId:   byJwt.ClientId,
			ClientAuth: clientAuth,
		}, nil
	} else {
		return nil, err
	}
}

func (self *ApiMultiClientGenerator) RemoveClientArgs(args *MultiClientGeneratorClientArgs) {
	removeNetworkClient := &RemoveNetworkClientArgs{
		ClientId: args.ClientId,
	}

	self.api.RemoveNetworkClient(removeNetworkClient, NewApiCallback(func(result *RemoveNetworkClientResult, err error) {
	}))
}

func (self *ApiMultiClientGenerator) RemoveClientWithArgs(client *Client, args *MultiClientGeneratorClientArgs) {
	self.RemoveClientArgs(args)
}

func (self *ApiMultiClientGenerator) NewClientSettings() *ClientSettings {
	return self.clientSettingsGenerator()
}

func (self *ApiMultiClientGenerator) NewClient(
	ctx context.Context,
	args *MultiClientGeneratorClientArgs,
	clientSettings *ClientSettings,
) (*Client, error) {
	byJwt, err := ParseByJwtUnverified(args.ClientAuth.ByJwt)
	if err != nil {
		return nil, err
	}
	clientOob := NewApiOutOfBandControl(ctx, self.clientStrategy, args.ClientAuth.ByJwt, self.apiUrl)
	client := NewClient(ctx, byJwt.ClientId, clientOob, clientSettings)
	settings := DefaultPlatformTransportSettings()
	if args.P2pOnly {
		settings.TransportGenerator = func() (sendTransport Transport, receiveTransport Transport) {
			// only use the platform transport for control
			sendTransport = NewSendClientTransport(DestinationId(ControlId))
			receiveTransport = NewReceiveGatewayTransport()
			return
		}
	}
	NewPlatformTransport(
		client.Ctx(),
		self.clientStrategy,
		client.RouteManager(),
		self.platformUrl,
		args.ClientAuth,
		settings,
	)
	// enable return traffic for this client
	ack := make(chan struct{})
	client.ContractManager().SetProvideModesWithReturnTrafficWithAckCallback(
		map[protocol.ProvideMode]bool{},
		func(err error) {
			close(ack)
		},
	)
	select {
	case <-ack:
	case <-time.After(self.settings.InitTimeout):
		client.Cancel()
		return nil, errors.New("Could not enable return traffic for client.")
	}
	return client, nil
}

func (self *ApiMultiClientGenerator) FixedDestinationSize() (int, bool) {
	specClientIds := []Id{}
	for _, spec := range self.specs {
		if spec.ClientId != nil {
			specClientIds = append(specClientIds, *spec.ClientId)
		}
	}
	// glog.Infof("[multi]eval fixed %d/%d\n", len(specClientIds), len(self.specs))
	return len(specClientIds), len(specClientIds) == len(self.specs)
}

type multiClientWindow struct {
	ctx    context.Context
	cancel context.CancelFunc

	generator                   MultiClientGenerator
	clientReceivePacketCallback clientReceivePacketFunction
	ingressSecurityPolicy       SecurityPolicy
	clientRemoveCallback        func(client *multiClientChannel)
	windowType                  WindowType

	settings *MultiClientSettings

	clientChannelArgs chan *multiClientChannelArgs

	monitor *RemoteUserNatMultiClientMonitor

	contractStatusCallbacks *CallbackList[ContractStatusFunction]

	stateLock          sync.Mutex
	destinationClients map[MultiHopId]*multiClientChannel
}

func newMultiClientWindow(
	ctx context.Context,
	cancel context.CancelFunc,
	generator MultiClientGenerator,
	clientReceivePacketCallback clientReceivePacketFunction,
	ingressSecurityPolicy SecurityPolicy,
	clientRemoveCallback func(client *multiClientChannel),
	windowType WindowType,
	settings *MultiClientSettings,
) *multiClientWindow {
	window := &multiClientWindow{
		ctx:                         ctx,
		cancel:                      cancel,
		generator:                   generator,
		clientReceivePacketCallback: clientReceivePacketCallback,
		ingressSecurityPolicy:       ingressSecurityPolicy,
		clientRemoveCallback:        clientRemoveCallback,
		windowType:                  windowType,
		settings:                    settings,
		clientChannelArgs:           make(chan *multiClientChannelArgs, settings.WindowSizes[windowType].WindowSizeMin),
		monitor:                     NewRemoteUserNatMultiClientMonitor(&settings.RemoteUserNatMultiClientMonitorSettings),
		contractStatusCallbacks:     NewCallbackList[ContractStatusFunction](),
		destinationClients:          map[MultiHopId]*multiClientChannel{},
	}

	go HandleError(window.randomEnumerateClientArgs, cancel)
	go HandleError(window.resize, cancel)

	return window
}

func (self *multiClientWindow) AddContractStatusCallback(contractStatusCallback ContractStatusFunction) func() {
	callbackId := self.contractStatusCallbacks.Add(contractStatusCallback)
	return func() {
		self.contractStatusCallbacks.Remove(callbackId)
	}
}

func (self *multiClientWindow) contractStatus(contractStatus *ContractStatus) {
	for _, contractStatusCallback := range self.contractStatusCallbacks.Get() {
		HandleError(func() {
			contractStatusCallback(contractStatus)
		})
	}
}

func (self *multiClientWindow) randomEnumerateClientArgs() {
	defer func() {
		close(self.clientChannelArgs)

		// drain the channel
		func() {
			for {
				select {
				case args, ok := <-self.clientChannelArgs:
					if !ok {
						return
					}
					self.generator.RemoveClientArgs(&args.MultiClientGeneratorClientArgs)
				}
			}
		}()
	}()

	// continually reset the visited set when there are no more
	// a destination can be revisited after `WindowRevisitTimeout`
	visitedDestinations := map[MultiHopId]time.Time{}
	for {
		destinations := map[MultiHopId]DestinationStats{}
		for len(destinations) == 0 {
			// exclude destinations that are already in the window
			windowDestinations := map[MultiHopId]bool{}
			func() {
				self.stateLock.Lock()
				defer self.stateLock.Unlock()
				for destination, _ := range self.destinationClients {
					windowDestinations[destination] = true
					visitedDestinations[destination] = time.Now()
				}
			}()
			revisitTime := time.Now().Add(-self.settings.WindowRevisitTimeout)
			for destination, visitedTime := range visitedDestinations {
				if visitedTime.Before(revisitTime) && !windowDestinations[destination] {
					delete(visitedDestinations, destination)
				}
			}
			nextDestinations, err := self.generator.NextDestinations(
				self.settings.WindowExpandBlockCount,
				maps.Keys(visitedDestinations),
				self.windowType.RankMode(),
			)
			if err != nil {
				glog.Infof("[multi]window enumerate error timeout = %s\n", err)
				select {
				case <-self.ctx.Done():
					return
				case <-time.After(self.settings.WindowEnumerateErrorTimeout):
				}
			} else {
				for destination, stats := range nextDestinations {
					destinations[destination] = stats
					visitedDestinations[destination] = time.Now()
				}
				// remove destinations that are already in the window
				func() {
					self.stateLock.Lock()
					defer self.stateLock.Unlock()
					for destination, _ := range self.destinationClients {
						delete(destinations, destination)
					}
				}()

				if len(destinations) == 0 {
					// reset
					clear(visitedDestinations)
					glog.Infof("[multi]window enumerate empty timeout.\n")
					select {
					case <-self.ctx.Done():
						return
					case <-time.After(self.settings.WindowEnumerateEmptyTimeout):
					}
				}
			}
		}

		for destination, stats := range destinations {
			if clientArgs, err := self.generator.NewClientArgs(); err == nil {
				args := &multiClientChannelArgs{
					Destination:                    destination,
					DestinationStats:               stats,
					MultiClientGeneratorClientArgs: *clientArgs,
				}
				select {
				case <-self.ctx.Done():
					self.generator.RemoveClientArgs(clientArgs)
					return
				case self.clientChannelArgs <- args:
				}
			} else {
				glog.Infof("[multi]create client args error = %s\n", err)
			}
		}
	}
}

func (self *multiClientWindow) resize() {
	// based on the most recent expand failure
	expandOvershotScale := float64(1.0)
	for {
		startTime := time.Now()

		clients := []*multiClientChannel{}

		maxSourceCount := 0
		weights := map[*multiClientChannel]float32{}
		durations := map[*multiClientChannel]time.Duration{}

		removedClientCount := 0

		func() {
			removedClients := []*multiClientChannel{}

			for _, client := range self.clients() {
				if stats, err := client.WindowStats(); err == nil {
					effectiveByteCountPerSecond := stats.EffectiveByteCountPerSecond()
					expectedByteCountPerSecond := stats.ExpectedByteCountPerSecond()
					var healthy bool
					if _, fixed := self.generator.FixedDestinationSize(); fixed {
						// we will not cycle fixed destinations
						// any issue with routing is an issue with the destination
						// TODO this would be susceptible to any protocol/stability issues also, which we need to focus on resolving
						healthy = true
					} else {
						healthy = (0 < effectiveByteCountPerSecond || 0 < expectedByteCountPerSecond) && stats.unhealthyDuration < self.settings.StatsWindowMaxUnhealthyDuration
					}
					if healthy {
						glog.Infof(
							"[multi]client ok [%s]: effective=%db/s expected=%db/s send=%db sendNack=%db receive=%db\n",
							client.ClientId(),
							effectiveByteCountPerSecond,
							expectedByteCountPerSecond,
							stats.sendAckByteCount,
							stats.sendNackByteCount,
							stats.receiveAckByteCount,
						)
						clients = append(clients, client)
						maxSourceCount = max(maxSourceCount, stats.sourceCount)
						weights[client] = float32(effectiveByteCountPerSecond)
						durations[client] = stats.channelDuration
					} else {
						glog.Infof(
							"[multi]unhealthy client [%s]: effective=%db/s expected=%db/s send=%db sendNack=%db receive=%db\n",
							client.ClientId(),
							effectiveByteCountPerSecond,
							expectedByteCountPerSecond,
							stats.sendAckByteCount,
							stats.sendNackByteCount,
							stats.receiveAckByteCount,
						)
						removedClients = append(removedClients, client)
						removedClientCount += 1
					}
				} else {
					glog.Infof("[multi]remove error client [%s] = %s\n", client.ClientId(), err)
					removedClients = append(removedClients, client)
					removedClientCount += 1
				}
			}

			if 0 < len(removedClients) {
				for _, client := range removedClients {
					client.Close()
				}
				func() {
					self.stateLock.Lock()
					defer self.stateLock.Unlock()

					for _, client := range removedClients {
						delete(self.destinationClients, client.Destination())
					}
				}()
				// for _, client := range removedClients {
				// 	self.monitor.AddProviderEvent(client.ClientId(), ProviderStateRemoved)
				// }
				// for _, client := range removedClients {
				// 	self.clientClosed(removedClients)
				// }
				self.removeClients(removedClients)
			}
		}()

		slices.SortFunc(clients, func(a *multiClientChannel, b *multiClientChannel) int {
			// descending weight
			aWeight := weights[a]
			bWeight := weights[b]
			if aWeight < bWeight {
				return 1
			} else if bWeight < aWeight {
				return -1
			} else {
				return 0
			}
		})

		windowSize := self.settings.WindowSizes[self.windowType]

		var targetWindowSize int
		var expandWindowSize int
		var collapseWindowSize int
		if fixedDestinationSize, fixed := self.generator.FixedDestinationSize(); fixed {
			glog.Infof("[multi]fixed = %d\n", fixedDestinationSize)
			targetWindowSize = fixedDestinationSize
			expandWindowSize = fixedDestinationSize
			collapseWindowSize = fixedDestinationSize
		} else {
			targetWindowSize = removedClientCount + min(
				windowSize.WindowSizeMax,
				max(
					windowSize.WindowSizeMin,
					int(math.Ceil(float64(maxSourceCount)*windowSize.WindowSizeReconnectScale)),
				),
			)

			// expand and collapse have scale thresholds to avoid jittery resizing
			// too much resing wastes device resources
			expandWindowSize = min(
				windowSize.WindowSizeMax,
				max(
					windowSize.WindowSizeMin,
					int(math.Ceil(self.settings.WindowExpandScale*float64(len(clients)))),
				),
			)

			collapseWindowSize = max(
				windowSize.WindowSizeMin,
				max(
					windowSize.WindowSizeMin,
					int(math.Ceil(self.settings.WindowCollapseScale*float64(len(clients)))),
				),
			)
		}

		collapseLowestWeighted := func(windowSize int) []*multiClientChannel {
			// try to remove the lowest weighted clients to resize the window to `windowSize`
			// clients in the graceperiod or with activity cannot be removed

			// self.stateLock.Lock()
			// defer self.stateLock.Unlock()

			// n := 0

			removedClients := []*multiClientChannel{}
			// collapseClients := clients[windowSize:]
			// clients = clients[:windowSize]
			for _, client := range clients[windowSize:] {
				if self.settings.StatsWindowGraceperiod <= durations[client] && weights[client] <= 0 {
					// client.Close()
					// n += 1
					// delete(self.destinationClients, client.Destination())
					removedClients = append(removedClients, client)
				}
				//  else {
				// 	clients = append(clients, client)
				// }
			}

			// destinationClients := map[MultiHopId]*multiClientChannel{}
			// for _, client := range clients {
			// 	destinationClients[client.Destination()] = client
			// }
			// self.destinationClients = destinationClients

			return removedClients
		}

		p2pOnlyWindowSize := 0
		for _, client := range clients {
			if client.IsP2pOnly() {
				p2pOnlyWindowSize += 1
			}
		}
		if expandWindowSize <= targetWindowSize && len(clients) < expandWindowSize || p2pOnlyWindowSize < windowSize.WindowSizeMinP2pOnly {
			if self.settings.WindowCollapseBeforeExpand {
				// collapse badly performing clients before expanding
				removedClients := collapseLowestWeighted(collapseWindowSize)
				if 0 < len(removedClients) {
					glog.Infof("[multi]window optimize -%d ->%d\n", len(removedClients), len(clients))
					for _, client := range removedClients {
						client.Close()
					}
					func() {
						self.stateLock.Lock()
						defer self.stateLock.Unlock()

						for _, client := range removedClients {
							delete(self.destinationClients, client.Destination())
						}
					}()
					// for _, client := range removedClients {
					// 	self.monitor.AddProviderEvent(client.ClientId(), ProviderStateRemoved)
					// }
					self.removeClients(removedClients)
				}
			}

			// expand
			n := expandWindowSize - len(clients)
			self.monitor.AddWindowExpandEvent(false, expandWindowSize)
			overN := int(math.Ceil(expandOvershotScale * float64(n)))
			glog.Infof("[multi]window expand +%d(%d) %d->%d\n", n, overN, len(clients), expandWindowSize)
			self.expand(len(clients), p2pOnlyWindowSize, expandWindowSize, overN)

			// evaluate the next overshot scale
			func() {
				self.stateLock.Lock()
				defer self.stateLock.Unlock()
				n = len(self.destinationClients) - len(clients)
			}()
			if n <= 0 {
				expandOvershotScale = self.settings.WindowExpandMaxOvershotScale
			} else {
				// overN = s * n
				expandOvershotScale = min(
					self.settings.WindowExpandMaxOvershotScale,
					float64(overN)/float64(n),
				)
			}
		} else if targetWindowSize <= collapseWindowSize && collapseWindowSize < len(clients) {
			self.monitor.AddWindowExpandEvent(true, collapseWindowSize)
			removedClients := collapseLowestWeighted(collapseWindowSize)
			if 0 < len(removedClients) {
				glog.Infof("[multi]window collapse -%d ->%d\n", len(removedClients), len(clients))
				// for _, client := range removedClients {
				// 	self.monitor.AddProviderEvent(client.ClientId(), ProviderStateRemoved)
				// }
				for _, client := range removedClients {
					client.Close()
				}
				func() {
					self.stateLock.Lock()
					defer self.stateLock.Unlock()

					for _, client := range removedClients {
						delete(self.destinationClients, client.Destination())
					}
				}()
				self.removeClients(removedClients)
			}
			self.monitor.AddWindowExpandEvent(true, collapseWindowSize)
		} else {
			self.monitor.AddWindowExpandEvent(true, len(clients))
			glog.Infof("[multi]window stable =%d\n", len(clients))
		}

		timeout := self.settings.WindowResizeTimeout - time.Now().Sub(startTime)
		if timeout <= 0 {
			select {
			case <-self.ctx.Done():
				return
			default:
			}
		} else {
			select {
			case <-self.ctx.Done():
				return
			case <-time.After(timeout):
			}
		}
	}
}

func (self *multiClientWindow) expand(currentWindowSize int, currentP2pOnlyWindowSize int, targetWindowSize int, n int) {
	mutex := sync.Mutex{}
	addedCount := 0

	windowSize := self.settings.WindowSizes[self.windowType]

	endTime := time.Now().Add(self.settings.WindowExpandTimeout)
	// blockEndTime := time.Now().Add(self.settings.WindowExpandBlockTimeout)
	pendingPingDones := []chan struct{}{}
	added := 0
	addedP2pOnly := 0
	for i := 0; i < n; i += 1 {
		timeout := endTime.Sub(time.Now())
		if timeout < 0 {
			glog.Infof("[multi]expand window timeout\n")
			return
		}

		select {
		case <-self.ctx.Done():
			return
		// case <- update:
		//     // continue
		case args, ok := <-self.clientChannelArgs:
			if !ok {
				return
			}
			func() {
				self.stateLock.Lock()
				defer self.stateLock.Unlock()
				_, ok = self.destinationClients[args.Destination]
			}()

			if ok {
				// already have a client in the window for this destination
				self.generator.RemoveClientArgs(&args.MultiClientGeneratorClientArgs)
			} else {
				// randomly set to p2p only to meet the minimum requirement
				if !args.MultiClientGeneratorClientArgs.P2pOnly {
					a := max(windowSize.WindowSizeMin-(currentWindowSize+added), 0)
					b := max(windowSize.WindowSizeMinP2pOnly-(currentP2pOnlyWindowSize+addedP2pOnly), 0)
					var p2pOnlyP float32
					if a+b == 0 {
						p2pOnlyP = 0
					} else {
						p2pOnlyP = float32(b) / float32(a+b)
					}
					args.MultiClientGeneratorClientArgs.P2pOnly = mathrand.Float32() < p2pOnlyP
				}
				// send an initial ping on the client and let the ack timeout close it
				pingDone := make(chan struct{})
				pendingPingDones = append(pendingPingDones, pingDone)

				go func() {
					client, err := newMultiClientChannel(
						self.ctx,
						args,
						self.generator,
						self.clientReceivePacketCallback,
						self.ingressSecurityPolicy,
						self.contractStatus,
						self.settings,
					)
					if err == nil {
						added += 1
						if client.IsP2pOnly() {
							addedP2pOnly += 1
						}

						self.monitor.AddProviderEvent(args.ClientId, ProviderStateInEvaluation)

						success, err := client.SendDetailedMessage(
							&protocol.IpPing{},
							self.settings.PingWriteTimeout,
							func(err error) {
								close(pingDone)
								if err == nil {
									glog.Infof("[multi]expand new client\n")

									self.monitor.AddProviderEvent(args.ClientId, ProviderStateAdded)
									var replacedClient *multiClientChannel
									func() {
										self.stateLock.Lock()
										defer self.stateLock.Unlock()
										replacedClient = self.destinationClients[args.Destination]
										self.destinationClients[args.Destination] = client
									}()
									if replacedClient != nil {
										replacedClient.Cancel()
										self.monitor.AddProviderEvent(replacedClient.ClientId(), ProviderStateRemoved)
									}
									func() {
										mutex.Lock()
										defer mutex.Unlock()
										addedCount += 1
										self.monitor.AddWindowExpandEvent(
											targetWindowSize <= currentWindowSize+addedCount,
											targetWindowSize,
										)
									}()
								} else {
									glog.Infof("[multi]create ping error = %s\n", err)
									client.Cancel()
									self.monitor.AddProviderEvent(args.ClientId, ProviderStateEvaluationFailed)
								}
							},
						)
						if err != nil {
							glog.Infof("[multi]create client ping error = %s\n", err)
							close(pingDone)
							client.Cancel()
						} else if !success {
							close(pingDone)
							client.Cancel()
							self.monitor.AddProviderEvent(args.ClientId, ProviderStateEvaluationFailed)
						} else {
							// async wait for the ping
							go HandleError(func() {
								select {
								case <-pingDone:
								case <-time.After(self.settings.PingTimeout):
									glog.V(2).Infof("[multi]expand window timeout waiting for ping\n")
									client.Cancel()
								}
							}, client.Cancel)
						}
					} else {
						glog.Infof("[multi]create client error = %s\n", err)
						close(pingDone)
						self.generator.RemoveClientArgs(&args.MultiClientGeneratorClientArgs)
						self.monitor.AddProviderEvent(args.ClientId, ProviderStateEvaluationFailed)
					}
				}()
			}
		case <-time.After(timeout):
			glog.V(2).Infof("[multi]expand window timeout waiting for args\n")
		}
	}

	// wait for pending pings
	for _, pingDone := range pendingPingDones {
		timeout := endTime.Sub(time.Now())
		if timeout <= 0 {
			break
		}

		select {
		case <-self.ctx.Done():
			return
		case <-pingDone:
		case <-time.After(timeout):
		}
	}

	return
}

func (self *multiClientWindow) shuffle() {
	for _, client := range self.clients() {
		client.Cancel()
	}
}

func (self *multiClientWindow) clients() []*multiClientChannel {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return maps.Values(self.destinationClients)
}

func (self *multiClientWindow) OrderedClients() []*multiClientChannel {
	clients := []*multiClientChannel{}

	windowSize := self.settings.WindowSizes[self.windowType]

	weights := map[*multiClientChannel]float32{}
	// durations := map[*multiClientChannel]time.Duration{}

	for _, client := range self.clients() {
		if stats, err := client.WindowStats(); err == nil {
			clients = append(clients, client)
			// durations[client] = stats.duration
			weights[client] = float32(1 + stats.ExpectedByteCountPerSecond())
		}
	}

	// set weights for clients
	// for _, client := range clients {
	// 	if weight := float32(stats.ByteCountPerSecond()); 0 <= weight {
	// 		if duration := durations[client]; duration < self.settings.StatsWindowGraceperiod {
	// 			// use the estimate
	// 			weights[client] = float32(client.EstimatedByteCountPerSecond())
	// 		} else if 0 == weight {
	// 			// not used, use the estimate
	// 			weights[client] = float32(client.EstimatedByteCountPerSecond())
	// 		}
	// 	} else {
	// 		weights[client] = 0
	// 	}
	// }

	if glog.V(1) {
		self.statsSampleWeights(weights)
	}

	WeightedShuffleWithEntropy(clients, weights, self.settings.StatsWindowEntropy)

	if 0 == len(clients) {
		return clients
	}

	// use only clients in the min tier
	// this prevents the window from crossing rank until necessary
	minTierClients := []*multiClientChannel{}
	minTier := clients[0].Tier()
	for _, client := range clients[1:] {
		minTier = min(minTier, client.Tier())
	}
	for _, client := range clients {
		if client.Tier() == minTier {
			minTierClients = append(minTierClients, client)
		} else {
			glog.Infof("[multi]exclude tier from window %d>%d\n", client.Tier(), minTier)
		}
	}

	// use only the top n items from the window
	if 0 < windowSize.WindowSizeUseMax {
		minTierClients = minTierClients[:min(len(minTierClients), windowSize.WindowSizeUseMax)]
	}

	return minTierClients
}

func (self *multiClientWindow) statsSampleWeights(weights map[*multiClientChannel]float32) {
	// randonly sample log statistics for weights
	if mathrand.Intn(self.settings.StatsSampleWeightsCount) == 0 {
		// sample the weights
		weightValues := maps.Values(weights)
		slices.SortFunc(weightValues, func(a float32, b float32) int {
			// descending
			if a < b {
				return 1
			} else if b < a {
				return -1
			} else {
				return 0
			}
		})
		net := float32(0)
		for _, weight := range weightValues {
			net += weight
		}
		if 0 < net {
			var sb strings.Builder
			netThresh := float32(0.99)
			netp := float32(0)
			netCount := 0
			for i, weight := range weightValues {
				p := 100 * weight / net
				netp += p
				netCount += 1
				if 0 < i {
					sb.WriteString(" ")
				}
				sb.WriteString(fmt.Sprintf("[%d]%.2f", i, p))
				if netThresh*100 <= netp {
					break
				}
			}

			glog.Infof("[multi]sample weights: %s (+%d more in window <%.0f%%)\n", sb.String(), len(weights)-netCount, 100*(1-netThresh))
		} else {
			glog.Infof("[multi]sample weights: zero (%d in window)\n", len(weights))
		}
	}
}

func (self *multiClientWindow) Close() {
	var removedClients []*multiClientChannel
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		for _, client := range self.destinationClients {
			// client.Close()
			removedClients = append(removedClients, client)
		}
		clear(self.destinationClients)
	}()
	for _, client := range removedClients {
		client.Close()
	}
	// self.removeClients(removedClients)
}

func (self *multiClientWindow) removeClients(removedClients []*multiClientChannel) {
	for _, client := range removedClients {
		self.monitor.AddProviderEvent(client.ClientId(), ProviderStateRemoved)
	}
	for _, client := range removedClients {
		self.clientRemoveCallback(client)
	}
}

type multiClientChannelArgs struct {
	MultiClientGeneratorClientArgs

	Destination MultiHopId
	DestinationStats
}

type multiClientEventType int

const (
	multiClientEventTypeAck    multiClientEventType = 1
	multiClientEventTypeNack   multiClientEventType = 2
	multiClientEventTypeError  multiClientEventType = 3
	multiClientEventTypeSource multiClientEventType = 4
)

type multiClientEventBucket struct {
	createTime time.Time
	eventTime  time.Time

	sendAckCount        int
	sendAckByteCount    ByteCount
	sendNackCount       int
	sendNackByteCount   ByteCount
	receiveAckCount     int
	receiveAckByteCount ByteCount
	sendAckTime         time.Time
	sendNackTime        time.Time
	errs                []error
	ip4Paths            map[Ip4Path]bool
	ip6Paths            map[Ip6Path]bool
}

func newMultiClientEventBucket() *multiClientEventBucket {
	now := time.Now()
	return &multiClientEventBucket{
		createTime: now,
		eventTime:  now,
	}
}

type clientWindowStats struct {
	sourceCount                 int
	sendAckCount                int
	sendAckByteCount            ByteCount
	sendNackCount               int
	sendNackByteCount           ByteCount
	receiveAckCount             int
	receiveAckByteCount         ByteCount
	ackByteCount                ByteCount
	duration                    time.Duration
	firstSendAckTime            time.Time
	firstSendNackTime           time.Time
	estimatedByteCountPerSecond ByteCount
	// FIXME firstStatDuration
	channelDuration   time.Duration
	unhealthyDuration time.Duration

	// internal
	bucketCount int
}

func (self *clientWindowStats) EffectiveByteCountPerSecond() ByteCount {
	millis := int64(self.duration / time.Millisecond)
	if millis <= 0 {
		return ByteCount(0)
	}
	netByteCount := int64(self.sendAckByteCount + self.receiveAckByteCount)
	return ByteCount((1000*netByteCount + millis/2) / millis)
}

func (self *clientWindowStats) ExpectedByteCountPerSecond() ByteCount {
	millis := int64(self.duration / time.Millisecond)
	if millis <= 0 {
		return self.estimatedByteCountPerSecond
	}
	netByteCount := int64(self.sendAckByteCount + self.sendNackByteCount + self.receiveAckByteCount)
	glog.V(2).Infof("[multi]expected use estimated = %dbps (net = %db/%dms)\n", self.estimatedByteCountPerSecond, netByteCount, millis)
	return max(
		self.estimatedByteCountPerSecond-ByteCount((1000*netByteCount+millis/2)/millis),
		0,
	)
}

type multiClientChannel struct {
	ctx    context.Context
	cancel context.CancelFunc

	args *multiClientChannelArgs

	api *BringYourApi

	clientReceivePacketCallback clientReceivePacketFunction
	ingressSecurityPolicy       SecurityPolicy

	settings *MultiClientSettings

	// sourceFilter map[TransferPath]bool

	client *Client

	stateLock    sync.Mutex
	eventBuckets []*multiClientEventBucket
	// destination -> source -> count
	ip4DestinationSourceCount          map[Ip4Path]map[Ip4Path]int
	ip6DestinationSourceCount          map[Ip6Path]map[Ip6Path]int
	packetStats                        *clientWindowStats
	endErr                             error
	maxEffectiveByteCountPerSecond     ByteCount
	maxEffectiveByteCountPerSecondTime time.Time
	firstEventTime                     time.Time
	lastHealthyTime                    time.Time

	// affinityCount int
	// affinityTime  time.Time

	clientReceiveUnsub func()
}

func newMultiClientChannel(
	ctx context.Context,
	args *multiClientChannelArgs,
	generator MultiClientGenerator,
	clientReceivePacketCallback clientReceivePacketFunction,
	ingressSecurityPolicy SecurityPolicy,
	contractStatusCallback ContractStatusFunction,
	settings *MultiClientSettings,
) (*multiClientChannel, error) {
	cancelCtx, cancel := context.WithCancel(ctx)

	clientSettings := generator.NewClientSettings()
	clientSettings.SendBufferSettings.AckTimeout = settings.AckTimeout

	client, err := generator.NewClient(
		cancelCtx,
		&args.MultiClientGeneratorClientArgs,
		clientSettings,
	)
	if err != nil {
		return nil, err
	}
	contractStatusSub := client.ContractManager().AddContractStatusCallback(contractStatusCallback)
	go HandleError(func() {
		select {
		case <-cancelCtx.Done():
		case <-client.Done():
		}
		client.Cancel()
		contractStatusSub()
		generator.RemoveClientWithArgs(client, &args.MultiClientGeneratorClientArgs)
	}, cancel)

	// sourceFilter := map[TransferPath]bool{
	//     Path{ClientId:args.DestinationId}: true,
	// }

	clientChannel := &multiClientChannel{
		ctx:                         cancelCtx,
		cancel:                      cancel,
		args:                        args,
		clientReceivePacketCallback: clientReceivePacketCallback,
		ingressSecurityPolicy:       ingressSecurityPolicy,
		settings:                    settings,
		// sourceFilter: sourceFilter,
		client:                    client,
		eventBuckets:              []*multiClientEventBucket{},
		ip4DestinationSourceCount: map[Ip4Path]map[Ip4Path]int{},
		ip6DestinationSourceCount: map[Ip6Path]map[Ip6Path]int{},
		packetStats:               &clientWindowStats{},
		// affinityCount:             0,
		// affinityTime:              time.Time{},
	}
	go HandleError(clientChannel.detectBlackhole, cancel)
	go HandleError(clientChannel.ping, cancel)

	clientReceiveUnsub := client.AddReceiveCallback(clientChannel.clientReceive)
	clientChannel.clientReceiveUnsub = clientReceiveUnsub

	return clientChannel, nil
}

func (self *multiClientChannel) ClientId() Id {
	return self.client.ClientId()
}

func (self *multiClientChannel) IsP2pOnly() bool {
	return self.args.MultiClientGeneratorClientArgs.P2pOnly
}

func (self *multiClientChannel) Tier() int {
	return self.args.DestinationStats.Tier
}

func (self *multiClientChannel) EstimatedByteCountPerSecond() ByteCount {
	return self.args.EstimatedBytesPerSecond
}

// func (self *multiClientChannel) UpdateAffinity() {
// 	self.stateLock.Lock()
// 	defer self.stateLock.Unlock()

// 	self.affinityCount += 1
// 	self.affinityTime = time.Now()
// }

// func (self *multiClientChannel) ClearAffinity() {
// 	self.stateLock.Lock()
// 	defer self.stateLock.Unlock()

// 	self.affinityCount = 0
// 	self.affinityTime = time.Time{}
// }

// func (self *multiClientChannel) MostRecentAffinity() (int, time.Time) {
// 	self.stateLock.Lock()
// 	defer self.stateLock.Unlock()

// 	return self.affinityCount, self.affinityTime
// }

func (self *multiClientChannel) Send(parsedPacket *parsedPacket, timeout time.Duration) bool {
	success, err := self.SendDetailed(parsedPacket, timeout)
	return success && err == nil
}

func (self *multiClientChannel) SendDetailed(parsedPacket *parsedPacket, timeout time.Duration) (bool, error) {
	ipPacketToProvider := &protocol.IpPacketToProvider{
		IpPacket: &protocol.IpPacket{
			PacketBytes: parsedPacket.packet,
		},
	}
	if frame, err := ToFrame(ipPacketToProvider, self.settings.ProtocolVersion); err != nil {
		self.addError(err)
		return false, err
	} else {
		packetByteCount := ByteCount(len(parsedPacket.packet))
		self.addSendNack(packetByteCount)
		self.addSource(parsedPacket.ipPath)
		ackCallback := func(err error) {
			if err == nil {
				self.addSendAck(packetByteCount)
			} else {
				self.addError(err)
			}
		}

		opts := []any{
			ForceStream(),
		}
		switch parsedPacket.ipPath.Protocol {
		case IpProtocolUdp:
			opts = append(opts, NoAck())
		}
		success, err := self.client.SendMultiHopWithTimeoutDetailed(
			frame,
			self.args.Destination,
			ackCallback,
			timeout,
			opts...,
		)
		if err != nil {
			return success, err
		}
		if success {
			if !frame.Raw {
				MessagePoolReturn(parsedPacket.packet)
			}
		} else {
			if !frame.Raw {
				MessagePoolReturn(frame.MessageBytes)
			}
		}
		return success, err
	}
}

func (self *multiClientChannel) SendDetailedMessage(message proto.Message, timeout time.Duration, ackCallback func(error)) (bool, error) {
	if frame, err := ToFrame(message, self.settings.ProtocolVersion); err != nil {
		return false, err
	} else {
		return self.client.SendMultiHopWithTimeoutDetailed(
			frame,
			self.args.Destination,
			ackCallback,
			timeout,
			ForceStream(),
		)
	}
}

func (self *multiClientChannel) Done() <-chan struct{} {
	return self.ctx.Done()
}

func (self *multiClientChannel) Destination() MultiHopId {
	return self.args.Destination
}

func (self *multiClientChannel) detectBlackhole() {
	// within a timeout window, if there are sent data but none received,
	// error out. This is similar to an ack timeout.
	defer self.cancel()

	for {
		if windowStats, err := self.WindowStats(); err != nil {
			return
		} else {

			blackhole := func() bool {
				if 0 < windowStats.sendAckCount {
					timeout := self.settings.BlackholeTimeout - time.Now().Sub(windowStats.firstSendAckTime)
					if timeout <= 0 {
						return windowStats.receiveAckCount <= 0
					}
					return false
				}
				if 0 < windowStats.sendNackCount {
					timeout := self.settings.BlackholeTimeout - time.Now().Sub(windowStats.firstSendNackTime)
					if timeout <= 0 {
						return windowStats.receiveAckCount <= 0
					}
					return false
				}
				return false
			}()

			if blackhole {
				// the client has sent data but received nothing back
				// this looks like a blackhole
				glog.Infof("[multi]routing %s blackhole: %d %dB <> %d %dB\n",
					self.args.Destination,
					windowStats.sendAckCount,
					windowStats.sendAckByteCount,
					windowStats.receiveAckCount,
					windowStats.receiveAckByteCount,
				)
				self.addError(fmt.Errorf("Blackhole (%d %dB)",
					windowStats.sendAckCount,
					windowStats.sendAckByteCount,
				))
				return
			} else {
				glog.Infof(
					"[multi]routing ok %s: %d %dB <> %d %dB\n",
					self.args.Destination,
					windowStats.sendAckCount,
					windowStats.sendAckByteCount,
					windowStats.receiveAckCount,
					windowStats.receiveAckByteCount,
				)
			}

			select {
			case <-self.ctx.Done():
				return
			case <-self.client.Done():
				return
			case <-time.After(self.settings.BlackholeTimeout / 2):
			}
		}
	}
}

func (self *multiClientChannel) ping() {
	defer self.cancel()

	for {
		if windowStats, err := self.WindowStats(); err != nil {
			return
		} else if windowStats.EffectiveByteCountPerSecond() <= self.settings.CPingMaxByteCountPerSecond {
			pingDone := make(chan error)
			success, err := self.SendDetailedMessage(
				&protocol.IpPing{},
				self.settings.CPingWriteTimeout,
				func(err error) {
					defer close(pingDone)
					select {
					case <-self.ctx.Done():
						return
					case pingDone <- err:
					}
				},
			)
			if err != nil {
				close(pingDone)
				return
			} else if !success {
				close(pingDone)
				return
			} else {
				select {
				case <-self.ctx.Done():
					return
				case <-self.client.Done():
					return
				case err := <-pingDone:
					if err != nil {
						self.addError(err)
						return
					}
				case <-time.After(self.settings.CPingTimeout):
					return
				}
			}
		}

		select {
		case <-self.ctx.Done():
			return
		case <-self.client.Done():
			return
		case <-WakeupAfter(self.settings.CPingTimeout, self.settings.CPingTimeout):
		}
	}
}

func (self *multiClientChannel) addSendNack(ackByteCount ByteCount) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.packetStats.sendNackCount += 1
	self.packetStats.sendNackByteCount += ackByteCount

	eventBucket := self.eventBucket()
	if eventBucket.sendNackCount == 0 {
		eventBucket.sendNackTime = time.Now()
	}
	eventBucket.sendNackCount += 1
	eventBucket.sendNackByteCount += ackByteCount
}

func (self *multiClientChannel) addSendAck(ackByteCount ByteCount) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.packetStats.sendNackCount -= 1
	self.packetStats.sendNackByteCount -= ackByteCount
	self.packetStats.sendAckCount += 1
	self.packetStats.sendAckByteCount += ackByteCount

	eventBucket := self.eventBucket()
	if eventBucket.sendAckCount == 0 {
		eventBucket.sendAckTime = time.Now()
	}
	eventBucket.sendAckCount += 1
	eventBucket.sendAckByteCount += ackByteCount
}

func (self *multiClientChannel) addReceiveAck(ackByteCount ByteCount) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.packetStats.receiveAckCount += 1
	self.packetStats.receiveAckByteCount += ackByteCount

	eventBucket := self.eventBucket()
	eventBucket.receiveAckCount += 1
	eventBucket.receiveAckByteCount += ackByteCount
}

func (self *multiClientChannel) addSource(ipPath *IpPath) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	eventBucket := self.eventBucket()
	switch ipPath.Version {
	case 4:
		ip4Path := ipPath.ToIp4Path()

		if eventBucket.ip4Paths == nil {
			eventBucket.ip4Paths = map[Ip4Path]bool{}
		}
		eventBucket.ip4Paths[ip4Path] = true

		source := ip4Path.Source()
		destination := ip4Path.Destination()

		sourceCount, ok := self.ip4DestinationSourceCount[destination]
		if !ok {
			sourceCount = map[Ip4Path]int{}
			self.ip4DestinationSourceCount[destination] = sourceCount
		}
		sourceCount[source] += 1
	case 6:
		ip6Path := ipPath.ToIp6Path()

		if eventBucket.ip6Paths == nil {
			eventBucket.ip6Paths = map[Ip6Path]bool{}
		}
		eventBucket.ip6Paths[ip6Path] = true

		source := ip6Path.Source()
		destination := ip6Path.Destination()

		sourceCount, ok := self.ip6DestinationSourceCount[destination]
		if !ok {
			sourceCount = map[Ip6Path]int{}
			self.ip6DestinationSourceCount[destination] = sourceCount
		}
		sourceCount[source] += 1
	default:
		panic(fmt.Errorf("Bad protocol version %d", ipPath.Version))
	}
}

func (self *multiClientChannel) addError(err error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	if self.endErr == nil {
		self.endErr = err
	}

	eventBucket := self.eventBucket()
	eventBucket.errs = append(eventBucket.errs, err)
}

// must be called with `stateLock`
func (self *multiClientChannel) eventBucket() *multiClientEventBucket {
	now := time.Now()

	var eventBucket *multiClientEventBucket
	if n := len(self.eventBuckets); 0 < n {
		eventBucket = self.eventBuckets[n-1]
	}

	if eventBucket == nil || eventBucket.createTime.Add(self.settings.StatsWindowBucketDuration).Before(now) {
		eventBucket = newMultiClientEventBucket()
		self.eventBuckets = append(self.eventBuckets, eventBucket)
	}

	eventBucket.eventTime = now

	self.coalesceEventBuckets()

	return eventBucket
}

// must be called with `stateLock`
func (self *multiClientChannel) coalesceEventBuckets() {
	// if there is no activity (no new buckets), keep historical buckets around
	minBucketCount := 1 + int(self.settings.StatsWindowDuration/self.settings.StatsWindowBucketDuration)

	windowStart := time.Now().Add(-self.settings.StatsWindowDuration)

	removeEventBucket := func(eventBucket *multiClientEventBucket) {
		self.packetStats.sendAckCount -= eventBucket.sendAckCount
		self.packetStats.sendAckByteCount -= eventBucket.sendAckByteCount
		self.packetStats.receiveAckCount -= eventBucket.receiveAckCount
		self.packetStats.receiveAckByteCount -= eventBucket.receiveAckByteCount

		for ip4Path, _ := range eventBucket.ip4Paths {
			source := ip4Path.Source()
			destination := ip4Path.Destination()

			sourceCount, ok := self.ip4DestinationSourceCount[destination]
			if ok {
				count := sourceCount[source]
				if count-1 <= 0 {
					delete(sourceCount, source)
				} else {
					sourceCount[source] = count - 1
				}
				if len(sourceCount) == 0 {
					delete(self.ip4DestinationSourceCount, destination)
				}
			}
		}

		for ip6Path, _ := range eventBucket.ip6Paths {
			source := ip6Path.Source()
			destination := ip6Path.Destination()

			sourceCount, ok := self.ip6DestinationSourceCount[destination]
			if ok {
				count := sourceCount[source]
				if count-1 <= 0 {
					delete(sourceCount, source)
				} else {
					sourceCount[source] = count - 1
				}
				if len(sourceCount) == 0 {
					delete(self.ip6DestinationSourceCount, destination)
				}
			}
		}
	}

	// remove all events before the window start
	i := 0
	for i < len(self.eventBuckets) && self.eventBuckets[i].eventTime.Before(windowStart) {
		removeEventBucket(self.eventBuckets[i])
		self.eventBuckets[i] = nil
		i += 1
	}
	for i < len(self.eventBuckets) && minBucketCount < len(self.eventBuckets) {
		removeEventBucket(self.eventBuckets[i])
		self.eventBuckets[i] = nil
		i += 1
	}
	if 0 < i {
		self.eventBuckets = self.eventBuckets[i:]
	}
}

func (self *multiClientChannel) WindowStats() (*clientWindowStats, error) {
	return self.windowStatsWithCoalesce(true)
}

func (self *multiClientChannel) windowStatsWithCoalesce(coalesce bool) (*clientWindowStats, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	if coalesce {
		self.coalesceEventBuckets()
	}

	duration := time.Duration(0)
	if 0 < len(self.eventBuckets) {
		endTime := self.eventBuckets[len(self.eventBuckets)-1].eventTime
		duration = endTime.Sub(self.eventBuckets[0].createTime)
	}
	var firstSendAckTime time.Time
	for _, eventBucket := range self.eventBuckets {
		if 0 < eventBucket.sendAckCount {
			firstSendAckTime = eventBucket.sendAckTime
			break
		}
	}
	var firstSendNackTime time.Time
	for _, eventBucket := range self.eventBuckets {
		if 0 < eventBucket.sendNackCount {
			firstSendNackTime = eventBucket.sendNackTime
			break
		}
	}

	// public internet resource ports
	isPublicPort := func(port int) bool {
		switch port {
		case 443:
			return true
		default:
			return false
		}
	}

	netSourceCounts := []int{}
	for ip4Path, sourceCounts := range self.ip4DestinationSourceCount {
		if isPublicPort(ip4Path.DestinationPort) {
			netSourceCounts = append(netSourceCounts, len(sourceCounts))
		}
	}
	for ip6Path, sourceCounts := range self.ip6DestinationSourceCount {
		if isPublicPort(ip6Path.DestinationPort) {
			netSourceCounts = append(netSourceCounts, len(sourceCounts))
		}
	}
	slices.Sort(netSourceCounts)
	maxSourceCount := 0
	selectionIndex := int(math.Ceil(
		self.settings.StatsSourceCountSelection * float64(len(netSourceCounts)-1),
	))
	if selectionIndex < len(netSourceCounts) {
		maxSourceCount = netSourceCounts[selectionIndex]
	}
	if glog.V(2) {
		for ip4Path, sourceCounts := range self.ip4DestinationSourceCount {
			if isPublicPort(ip4Path.DestinationPort) {
				if len(sourceCounts) == maxSourceCount {
					glog.Infof("[multi]max source count %d = %v\n", maxSourceCount, ip4Path)
				}
			}
		}
		for ip6Path, sourceCounts := range self.ip6DestinationSourceCount {
			if isPublicPort(ip6Path.DestinationPort) {
				if len(sourceCounts) == maxSourceCount {
					glog.Infof("[multi]max source count %d = %v\n", maxSourceCount, ip6Path)
				}
			}
		}
	}

	stats := &clientWindowStats{
		sourceCount:         maxSourceCount,
		sendAckCount:        self.packetStats.sendAckCount,
		sendNackCount:       self.packetStats.sendNackCount,
		sendAckByteCount:    self.packetStats.sendAckByteCount,
		sendNackByteCount:   self.packetStats.sendNackByteCount,
		receiveAckCount:     self.packetStats.receiveAckCount,
		receiveAckByteCount: self.packetStats.receiveAckByteCount,
		duration:            duration,
		firstSendAckTime:    firstSendAckTime,
		firstSendNackTime:   firstSendNackTime,
		bucketCount:         len(self.eventBuckets),
	}
	if 0 < len(self.eventBuckets) {
		eventTime := self.eventBuckets[len(self.eventBuckets)-1].eventTime

		effectiveByteCountPerSecond := stats.EffectiveByteCountPerSecond()
		scaledEffectiveByteCountPerSecond := ByteCount(self.settings.StatsWindowMaxEffectiveByteCountPerSecondScale * float32(effectiveByteCountPerSecond))
		if self.maxEffectiveByteCountPerSecond < scaledEffectiveByteCountPerSecond {
			self.maxEffectiveByteCountPerSecond = scaledEffectiveByteCountPerSecond
			self.maxEffectiveByteCountPerSecondTime = eventTime
		}
		if self.settings.StatsWindowMinHealthyEffectiveByteCountPerSecond <= effectiveByteCountPerSecond {
			self.lastHealthyTime = eventTime
		} else if !self.lastHealthyTime.IsZero() {
			stats.unhealthyDuration = eventTime.Sub(self.lastHealthyTime)
		}
		if self.firstEventTime.IsZero() {
			self.firstEventTime = self.eventBuckets[0].createTime
		} else {
			stats.channelDuration = eventTime.Sub(self.firstEventTime)
		}
	}
	if self.settings.StatsWindowGraceperiod < stats.channelDuration {
		stats.estimatedByteCountPerSecond = self.maxEffectiveByteCountPerSecond
	} else {
		stats.estimatedByteCountPerSecond = max(
			min(self.EstimatedByteCountPerSecond(), self.settings.StatsWindowMaxEstimatedByteCountPerSecond),
			self.maxEffectiveByteCountPerSecond,
		)
	}

	err := self.endErr
	if err == nil {
		select {
		case <-self.ctx.Done():
			err = errors.New("Done.")
		case <-self.client.Done():
			err = errors.New("Done.")
		default:
		}
	}

	return stats, err
}

// `connect.ReceiveFunction`
func (self *multiClientChannel) clientReceive(source TransferPath, frames []*protocol.Frame, provideMode protocol.ProvideMode) {
	select {
	case <-self.ctx.Done():
		return
	default:
	}

	// only process frames from the destinations
	// if allow := self.sourceFilter[source]; !allow {
	//     glog.V(2).Infof("[multi]receive drop %d %s<-\n", len(frames), self.args.DestinationId)
	//     return
	// }

	for _, frame := range frames {
		switch frame.MessageType {
		case protocol.MessageType_IpIpPacketFromProvider:
			if ipPacketFromProvider_, err := FromFrame(frame); err == nil {
				ipPacketFromProvider := ipPacketFromProvider_.(*protocol.IpPacketFromProvider)

				packet := ipPacketFromProvider.IpPacket.PacketBytes

				self.addReceiveAck(ByteCount(len(packet)))

				// ipPath, err := ParseIpPath(packet)
				ipPath, r, err := self.ingressSecurityPolicy.Inspect(provideMode, packet)
				if err == nil && r == SecurityPolicyResultAllow {
					self.clientReceivePacketCallback(self, source, provideMode, ipPath, packet)
				}
				// else not an ip packet, drop
			} else {
				glog.V(2).Infof("[multi]receive drop %s<- = %s\n", self.args.Destination, err)
			}
		default:
			// unknown message, drop
		}
	}
}

func (self *multiClientChannel) Cancel() {
	self.addError(errors.New("Done."))
	self.cancel()
	self.client.Cancel()
}

func (self *multiClientChannel) Close() {
	self.addError(errors.New("Done."))
	self.cancel()
	self.client.Close()

	self.clientReceiveUnsub()
}

func (self *multiClientChannel) IsDone() bool {
	select {
	case <-self.ctx.Done():
		return true
	default:
		return false
	}
}
