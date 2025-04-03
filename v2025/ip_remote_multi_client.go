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

	"github.com/urnetwork/connect/protocol/v2025"
)

// multi client is a sender approach to mitigate bad destinations
// it maintains a window of compatible clients chosen using specs
// (e.g. from a desription of the intent of use)
// - the clients are rate limited by the number of outstanding acks (nacks)
// - the size of allowed outstanding nacks increases with each ack,
// scaling up successful destinations to use the full transfer buffer
// - the clients are chosen with probability weighted by their
// net frame count statistics (acks - nacks)

// TODO surface window stats to show to users

type clientReceivePacketFunction func(client *multiClientChannel, source TransferPath, provideMode protocol.ProvideMode, ipPath *IpPath, packet []byte)

// for each `NewClientArgs`,
//
//	`RemoveClientWithArgs` will be called if a client was created for the args,
//	else `RemoveClientArgs`
type MultiClientGenerator interface {
	// path -> estimated byte count per second
	// the enumeration should typically
	// 1. not repeat final destination ids from any path
	// 2. not repeat intermediary elements from any path
	NextDestinations(count int, excludeDestinations []MultiHopId) (map[MultiHopId]ByteCount, error)
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
		WindowSizeMin:       4,
		// TODO increase this when p2p is deployed
		WindowSizeMinP2pOnly: 0,
		WindowSizeMax:        8,
		// reconnects per source
		WindowSizeReconnectScale: 1.0,
		SendRetryTimeout:         200 * time.Millisecond,
		// this includes the time to establish the transport
		PingWriteTimeout: 5 * time.Second,
		PingTimeout:      10 * time.Second,
		// a lower ack timeout helps cycle through bad providers faster
		AckTimeout:             15 * time.Second,
		BlackholeTimeout:       15 * time.Second,
		WindowResizeTimeout:    5 * time.Second,
		StatsWindowGraceperiod: 5 * time.Second,
		StatsWindowEntropy:     0.25,
		WindowExpandTimeout:    15 * time.Second,
		// wait this time before enumerating potential clients again
		WindowEnumerateEmptyTimeout:  60 * time.Second,
		WindowEnumerateErrorTimeout:  1 * time.Second,
		WindowExpandScale:            2.0,
		WindowCollapseScale:          0.5,
		WindowExpandMaxOvershotScale: 4.0,
		StatsWindowDuration:          10 * time.Second,
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

		RemoteUserNatMultiClientMonitorSettings: *DefaultRemoteUserNatMultiClientMonitorSettings(),
	}
}

type MultiClientSettings struct {
	SequenceIdleTimeout time.Duration
	WindowSizeMin       int
	// the minimumum number of items in the windows that must be connected via p2p only
	WindowSizeMinP2pOnly int
	WindowSizeMax        int
	// reconnects per source
	WindowSizeReconnectScale float64
	// ClientNackInitialLimit int
	// ClientNackMaxLimit int
	// ClientNackScale float64
	// ClientWriteTimeout time.Duration
	// SendTimeout time.Duration
	// WriteTimeout time.Duration
	SendRetryTimeout             time.Duration
	PingWriteTimeout             time.Duration
	PingTimeout                  time.Duration
	AckTimeout                   time.Duration
	BlackholeTimeout             time.Duration
	WindowResizeTimeout          time.Duration
	StatsWindowGraceperiod       time.Duration
	StatsWindowEntropy           float32
	WindowExpandTimeout          time.Duration
	WindowEnumerateEmptyTimeout  time.Duration
	WindowEnumerateErrorTimeout  time.Duration
	WindowExpandScale            float64
	WindowCollapseScale          float64
	WindowExpandMaxOvershotScale float64
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

	RemoteUserNatMultiClientMonitorSettings
}

type RemoteUserNatMultiClient struct {
	ctx    context.Context
	cancel context.CancelFunc

	generator MultiClientGenerator

	receivePacketCallback ReceivePacketFunction

	settings *MultiClientSettings

	window *multiClientWindow

	securityPolicy *SecurityPolicy
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

	multiClient := &RemoteUserNatMultiClient{
		ctx:                   cancelCtx,
		cancel:                cancel,
		generator:             generator,
		receivePacketCallback: receivePacketCallback,
		settings:              settings,
		// window:                window,
		securityPolicy: DefaultSecurityPolicy(),
		provideMode:    provideMode,
		ip4PathUpdates: map[Ip4Path]*multiClientChannelUpdate{},
		ip6PathUpdates: map[Ip6Path]*multiClientChannelUpdate{},
		clientUpdates:  map[*multiClientChannel]map[*multiClientChannelUpdate]bool{},
	}

	multiClient.window = newMultiClientWindow(
		cancelCtx,
		cancel,
		generator,
		multiClient.clientReceivePacket,
		multiClient.removeClient,
		settings,
	)

	return multiClient
}

func (self *RemoteUserNatMultiClient) SecurityPolicyStats(reset bool) SecurityPolicyStats {
	return self.securityPolicy.Stats().Stats(reset)
}

func (self *RemoteUserNatMultiClient) Monitor() *RemoteUserNatMultiClientMonitor {
	return self.window.monitor
}

func (self *RemoteUserNatMultiClient) AddContractStatusCallback(contractStatusCallback ContractStatusFunction) func() {
	return self.window.AddContractStatusCallback(contractStatusCallback)
}

func (self *RemoteUserNatMultiClient) updateClientPath(ipPath *IpPath, callback func(*multiClientChannelUpdate)) {
	update, client := self.reserveUpdate(ipPath)
	callback(update)
	self.updateClient(update, client)
}

func (self *RemoteUserNatMultiClient) reserveUpdate(ipPath *IpPath) (*multiClientChannelUpdate, *multiClientChannel) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	run := func(update *multiClientChannelUpdate) {
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
			update = newMultiClientChannelUpdate(self.ctx)
			go HandleError(func() {
				defer update.cancel()

				run(update)

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

				rst(client)
			}, update.cancel)
			self.ip4PathUpdates[ip4Path] = update
		}
		return update, update.client
	case 6:
		ip6Path := ipPath.ToIp6Path()
		update, ok := self.ip6PathUpdates[ip6Path]
		if !ok || update.IsDone() {
			update = newMultiClientChannelUpdate(self.ctx)
			go HandleError(func() {
				defer update.cancel()

				run(update)

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

				rst(client)
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
			} else {
				glog.Errorf("[multi]update associated with incorrect client")
			}
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
				var err error
				success, err = client.SendDetailed(sendPacket, timeout)
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

				if client.Send(sendPacket, sendTimeout) {
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
						bufferExceeded := self.settings.MultiRaceSetOnNoResponseTimeout <= time.Now().Sub(state.sendTime) || self.settings.MultiRaceClientSentPacketMaxCount < race.sentPacketCount
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
				}
			}
		}

		raceClients(self.window.OrderedClients(), 0)
		if success {
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

			if orderedClients := self.window.OrderedClients(); 0 < len(orderedClients) {
				// distribute the timeout evenly via wait
				retryTimeoutPerClient := retryTimeout / time.Duration(len(orderedClients))
				raceClients(orderedClients, retryTimeoutPerClient)
				if success {
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
	receivePacket := &ReceivePacket{
		Source:      source,
		ProvideMode: provideMode,
		IpPath:      ipPath,
		Packet:      packet,
	}

	ipPath = ipPath.Reverse()

	var abandonedClients []*multiClientChannel
	var receivePackets []*ReceivePacket
	self.updateClientPath(ipPath, func(update *multiClientChannelUpdate) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		client := update.client

		if client == sourceClient {
			receivePackets = []*ReceivePacket{receivePacket}
		} else if client != nil {
			// another client already chosen, drop
		} else if race := update.race; race == nil {
			// no race, no client, drop
			glog.Infof("[multi]receive no race and no client")
		} else if state, ok := race.clientStates[sourceClient]; !ok {
			// this client is not part of the race, drop
			glog.Infof("[multi]receive client not part of race")
		} else if len(state.packets) < self.settings.MultiRaceClientPacketMaxCount && race.packetCount < self.settings.MultiRacePacketMaxCount {
			state.packets = append(state.packets, receivePacket)
			if 1 == len(state.packets) {
				state.receiveTime = time.Now()
			}
			race.packetCount += 1
			if race.packetCount == 1 {
				// schedule the race evaluation on first packet
				earlyComplete := race.completeMonitor.NotifyChannel()
				self.scheduleCompleteRace(ipPath, race, earlyComplete)
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

			for abandonedClient, _ := range race.clientStates {
				if abandonedClient != client {
					abandonedClients = append(abandonedClients, abandonedClient)
				}
			}

			update.clearRace()
			update.client = client
			receivePackets = append(state.packets, receivePacket)
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
		var receivePackets []*ReceivePacket
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

				for abandonedClient, _ := range race.clientStates {
					if abandonedClient != client {
						abandonedClients = append(abandonedClients, abandonedClient)
					}
				}

				update.clearRace()
				update.client = client
				receivePackets = race.clientStates[update.client].packets
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
	})
}

func (self *RemoteUserNatMultiClient) Shuffle() {
	self.window.shuffle()
}

func (self *RemoteUserNatMultiClient) Close() {
	self.cancel()
	self.window.Close()

	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		for _, update := range self.ip4PathUpdates {
			update.Close()
		}
		for _, update := range self.ip6PathUpdates {
			update.Close()
		}
		clear(self.ip4PathUpdates)
		clear(self.ip6PathUpdates)
		// clear(self.updateIp4Paths)
		// clear(self.updateIp6Paths)
		// clear(clientUpdates)
	}()
}

type multiClientChannelUpdate struct {
	ctx    context.Context
	cancel context.CancelFunc

	client       *multiClientChannel
	race         *multiClientChannelUpdateRace
	activityTime time.Time
}

func newMultiClientChannelUpdate(ctx context.Context) *multiClientChannelUpdate {
	cancelCtx, cancel := context.WithCancel(ctx)
	return &multiClientChannelUpdate{
		ctx:          cancelCtx,
		cancel:       cancel,
		activityTime: time.Now(),
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
	packets     []*ReceivePacket
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
		clientSettingsGenerator: clientSettingsGenerator,
		settings:                settings,
		api:                     api,
	}
}

func (self *ApiMultiClientGenerator) NextDestinations(count int, excludeDestinations []MultiHopId) (map[MultiHopId]ByteCount, error) {
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
	}

	result, err := self.api.FindProviders2Sync(findProviders2)
	if err != nil {
		return nil, err
	}

	clientIdEstimatedBytesPerSecond := map[MultiHopId]ByteCount{}
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
			clientIdEstimatedBytesPerSecond[destination] = provider.EstimatedBytesPerSecond
		}
	}

	return clientIdEstimatedBytesPerSecond, nil
}

func (self *ApiMultiClientGenerator) NewClientArgs() (*MultiClientGeneratorClientArgs, error) {
	auth := func() (string, error) {
		// note the derived client id will be inferred by the api jwt
		authNetworkClient := &AuthNetworkClientArgs{
			Description: self.deviceDescription,
			DeviceSpec:  self.deviceSpec,
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
	clientRemoveCallback        func(client *multiClientChannel)

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
	clientRemoveCallback func(client *multiClientChannel),
	settings *MultiClientSettings,
) *multiClientWindow {
	window := &multiClientWindow{
		ctx:                         ctx,
		cancel:                      cancel,
		generator:                   generator,
		clientReceivePacketCallback: clientReceivePacketCallback,
		clientRemoveCallback:        clientRemoveCallback,
		settings:                    settings,
		clientChannelArgs:           make(chan *multiClientChannelArgs, settings.WindowSizeMin),
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
	visitedDestinations := map[MultiHopId]bool{}
	for {
		destinationEstimatedBytesPerSecond := map[MultiHopId]ByteCount{}
		for len(destinationEstimatedBytesPerSecond) == 0 {
			// exclude destinations that are already in the window
			func() {
				self.stateLock.Lock()
				defer self.stateLock.Unlock()
				for destination, _ := range self.destinationClients {
					visitedDestinations[destination] = true
				}
			}()
			nextDestinationEstimatedBytesPerSecond, err := self.generator.NextDestinations(
				1,
				maps.Keys(visitedDestinations),
			)
			if err != nil {
				glog.Infof("[multi]window enumerate error timeout = %s\n", err)
				select {
				case <-self.ctx.Done():
					return
				case <-time.After(self.settings.WindowEnumerateErrorTimeout):
				}
			} else {
				for destination, estimatedBytesPerSecond := range nextDestinationEstimatedBytesPerSecond {
					destinationEstimatedBytesPerSecond[destination] = estimatedBytesPerSecond
					visitedDestinations[destination] = true
				}
				// remove destinations that are already in the window
				func() {
					self.stateLock.Lock()
					defer self.stateLock.Unlock()
					for destination, _ := range self.destinationClients {
						delete(destinationEstimatedBytesPerSecond, destination)
					}
				}()

				if len(destinationEstimatedBytesPerSecond) == 0 {
					// reset
					visitedDestinations = map[MultiHopId]bool{}
					glog.Infof("[multi]window enumerate empty timeout.\n")
					select {
					case <-self.ctx.Done():
						return
					case <-time.After(self.settings.WindowEnumerateEmptyTimeout):
					}
				}
			}
		}

		for destination, estimatedBytesPerSecond := range destinationEstimatedBytesPerSecond {
			if clientArgs, err := self.generator.NewClientArgs(); err == nil {
				args := &multiClientChannelArgs{
					Destination:                    destination,
					EstimatedBytesPerSecond:        estimatedBytesPerSecond,
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

		removedClients := []*multiClientChannel{}

		for _, client := range self.clients() {
			if stats, err := client.WindowStats(); err == nil {
				clients = append(clients, client)
				maxSourceCount = max(maxSourceCount, stats.sourceCount)
				// byte count per second
				weights[client] = float32(stats.ByteCountPerSecond())
				durations[client] = stats.duration
			} else {
				glog.Infof("[multi]remove client = %s\n", err)
				removedClients = append(removedClients, client)
			}
		}

		if 0 < len(removedClients) {
			func() {
				self.stateLock.Lock()
				defer self.stateLock.Unlock()

				for _, client := range removedClients {
					client.Close()
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

		var targetWindowSize int
		var expandWindowSize int
		var collapseWindowSize int
		if fixedDestinationSize, fixed := self.generator.FixedDestinationSize(); fixed {
			glog.Infof("[multi]fixed = %d\n", fixedDestinationSize)
			targetWindowSize = fixedDestinationSize
			expandWindowSize = fixedDestinationSize
			collapseWindowSize = fixedDestinationSize
		} else {
			targetWindowSize = min(
				self.settings.WindowSizeMax,
				max(
					self.settings.WindowSizeMin,
					int(math.Ceil(float64(maxSourceCount)*self.settings.WindowSizeReconnectScale)),
				),
			)

			// expand and collapse have scale thresholds to avoid jittery resizing
			// too much resing wastes device resources
			expandWindowSize = min(
				self.settings.WindowSizeMax,
				max(
					self.settings.WindowSizeMin,
					int(math.Ceil(self.settings.WindowExpandScale*float64(len(clients)))),
				),
			)
			collapseWindowSize = int(math.Ceil(self.settings.WindowCollapseScale * float64(len(clients))))
		}

		collapseLowestWeighted := func(windowSize int) []*multiClientChannel {
			// try to remove the lowest weighted clients to resize the window to `windowSize`
			// clients in the graceperiod or with activity cannot be removed

			self.stateLock.Lock()
			defer self.stateLock.Unlock()

			// n := 0

			removedClients := []*multiClientChannel{}
			// collapseClients := clients[windowSize:]
			// clients = clients[:windowSize]
			for _, client := range clients[windowSize:] {
				if self.settings.StatsWindowGraceperiod <= durations[client] && weights[client] <= 0 {
					client.Close()
					// n += 1
					delete(self.destinationClients, client.Destination())
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
		if expandWindowSize <= targetWindowSize && len(clients) < expandWindowSize || p2pOnlyWindowSize < self.settings.WindowSizeMinP2pOnly {
			// collapse badly performing clients before expanding
			removedClients := collapseLowestWeighted(0)
			if 0 < len(removedClients) {
				glog.Infof("[multi]window optimize -%d ->%d\n", len(removedClients), len(clients))
				// for _, client := range removedClients {
				// 	self.monitor.AddProviderEvent(client.ClientId(), ProviderStateRemoved)
				// }
				self.removeClients(removedClients)
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

	endTime := time.Now().Add(self.settings.WindowExpandTimeout)
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
					a := max(self.settings.WindowSizeMin-(currentWindowSize+added), 0)
					b := max(self.settings.WindowSizeMinP2pOnly-(currentP2pOnlyWindowSize+addedP2pOnly), 0)
					var p2pOnlyP float32
					if a+b == 0 {
						p2pOnlyP = 0
					} else {
						p2pOnlyP = float32(b) / float32(a+b)
					}
					args.MultiClientGeneratorClientArgs.P2pOnly = mathrand.Float32() < p2pOnlyP
				}

				client, err := newMultiClientChannel(
					self.ctx,
					args,
					self.generator,
					self.clientReceivePacketCallback,
					self.contractStatus,
					self.settings,
				)
				if err == nil {
					added += 1
					if client.IsP2pOnly() {
						addedP2pOnly += 1
					}

					self.monitor.AddProviderEvent(args.ClientId, ProviderStateInEvaluation)

					// send an initial ping on the client and let the ack timeout close it
					pingDone := make(chan struct{})
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
						client.Cancel()
					} else if !success {
						client.Cancel()
						self.monitor.AddProviderEvent(args.ClientId, ProviderStateEvaluationFailed)
					} else {
						// async wait for the ping
						pendingPingDones = append(pendingPingDones, pingDone)
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
					self.generator.RemoveClientArgs(&args.MultiClientGeneratorClientArgs)
					self.monitor.AddProviderEvent(args.ClientId, ProviderStateEvaluationFailed)
				}
			}
		case <-time.After(timeout):
			glog.V(2).Infof("[multi]expand window timeout waiting for args\n")
		}
	}

	// wait for pending pings
	for _, pingDone := range pendingPingDones {
		select {
		case <-self.ctx.Done():
			return
		case <-pingDone:
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

	weights := map[*multiClientChannel]float32{}
	durations := map[*multiClientChannel]time.Duration{}

	for _, client := range self.clients() {
		if stats, err := client.WindowStats(); err == nil {
			clients = append(clients, client)
			weights[client] = float32(stats.ByteCountPerSecond())
			durations[client] = stats.duration
		}
	}

	// iterate and adjust weights for clients with weights >= 0
	nonNegativeClients := []*multiClientChannel{}
	for _, client := range clients {
		if weight := weights[client]; 0 <= weight {
			if duration := durations[client]; duration < self.settings.StatsWindowGraceperiod {
				// use the estimate
				weights[client] = float32(client.EstimatedByteCountPerSecond())
			} else if 0 == weight {
				// not used, use the estimate
				weights[client] = float32(client.EstimatedByteCountPerSecond())
			}
			nonNegativeClients = append(nonNegativeClients, client)
		}
	}

	if glog.V(1) {
		self.statsSampleWeights(weights)
	}

	WeightedShuffleWithEntropy(nonNegativeClients, weights, self.settings.StatsWindowEntropy)

	return nonNegativeClients
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
			client.Close()
			removedClients = append(removedClients, client)
		}
		clear(self.destinationClients)
	}()
	self.removeClients(removedClients)
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

	Destination             MultiHopId
	EstimatedBytesPerSecond ByteCount
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
	sourceCount         int
	sendAckCount        int
	sendAckByteCount    ByteCount
	sendNackCount       int
	sendNackByteCount   ByteCount
	receiveAckCount     int
	receiveAckByteCount ByteCount
	ackByteCount        ByteCount
	duration            time.Duration
	sendAckDuration     time.Duration

	// internal
	bucketCount int
}

func (self *clientWindowStats) ByteCountPerSecond() ByteCount {
	seconds := float64(self.duration / time.Second)
	if seconds <= 0 {
		return ByteCount(0)
	}
	return ByteCount(float64(self.sendAckByteCount-self.sendNackByteCount+self.receiveAckByteCount) / seconds)
}

type multiClientChannel struct {
	ctx    context.Context
	cancel context.CancelFunc

	args *multiClientChannelArgs

	api *BringYourApi

	clientReceivePacketCallback clientReceivePacketFunction

	settings *MultiClientSettings

	// sourceFilter map[TransferPath]bool

	client *Client

	stateLock    sync.Mutex
	eventBuckets []*multiClientEventBucket
	// destination -> source -> count
	ip4DestinationSourceCount map[Ip4Path]map[Ip4Path]int
	ip6DestinationSourceCount map[Ip6Path]map[Ip6Path]int
	packetStats               *clientWindowStats
	endErr                    error

	// affinityCount int
	// affinityTime  time.Time

	clientReceiveUnsub func()
}

func newMultiClientChannel(
	ctx context.Context,
	args *multiClientChannelArgs,
	generator MultiClientGenerator,
	clientReceivePacketCallback clientReceivePacketFunction,
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
		settings:                    settings,
		// sourceFilter: sourceFilter,
		client:                    client,
		eventBuckets:              []*multiClientEventBucket{},
		ip4DestinationSourceCount: map[Ip4Path]map[Ip4Path]int{},
		ip6DestinationSourceCount: map[Ip6Path]map[Ip6Path]int{},
		packetStats:               &clientWindowStats{},
		endErr:                    nil,
		// affinityCount:             0,
		// affinityTime:              time.Time{},
	}
	go HandleError(clientChannel.detectBlackhole, cancel)

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
	if frame, err := ToFrame(ipPacketToProvider); err != nil {
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
		return self.client.SendMultiHopWithTimeoutDetailed(
			frame,
			self.args.Destination,
			ackCallback,
			timeout,
			opts...,
		)
	}
}

func (self *multiClientChannel) SendDetailedMessage(message proto.Message, timeout time.Duration, ackCallback func(error)) (bool, error) {
	if frame, err := ToFrame(message); err != nil {
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

func (self *multiClientChannel) EstimatedByteCountPerSecond() ByteCount {
	return self.args.EstimatedBytesPerSecond
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
			timeout := self.settings.BlackholeTimeout - windowStats.sendAckDuration
			if timeout <= 0 {
				timeout = self.settings.BlackholeTimeout

				if 0 < windowStats.sendAckCount && windowStats.receiveAckCount <= 0 {
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
			}

			select {
			case <-self.ctx.Done():
				return
			case <-self.client.Done():
				return
			case <-time.After(timeout):
			}
		}
	}
}

func (self *multiClientChannel) addSendNack(ackByteCount ByteCount) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.packetStats.sendNackCount += 1
	self.packetStats.sendNackByteCount += ackByteCount

	eventBucket := self.eventBucket()
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

	// remove events before the window start
	i := 0
	for i < len(self.eventBuckets) && minBucketCount < len(self.eventBuckets) {
		eventBucket := self.eventBuckets[i]
		if windowStart.Before(eventBucket.eventTime) {
			break
		}

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
		duration = time.Now().Sub(self.eventBuckets[0].createTime)
	}
	sendAckDuration := time.Duration(0)
	for _, eventBucket := range self.eventBuckets {
		if 0 < eventBucket.sendAckCount {
			sendAckDuration = time.Now().Sub(eventBucket.sendAckTime)
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
	if selectionIndex := int(math.Ceil(self.settings.StatsSourceCountSelection * float64(len(netSourceCounts)-1))); selectionIndex < len(netSourceCounts) {
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
		sendAckDuration:     sendAckDuration,
		bucketCount:         len(self.eventBuckets),
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

				ipPath, err := ParseIpPath(packet)
				if err == nil {
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
