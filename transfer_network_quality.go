// Network-quality notifications remeasure live transfer estimators locally
// and carry the same hint to the affected peer over a reliable transfer lane.
// The hint is advisory: ordinary feedback still adapts without it, and an old
// peer safely consumes the unregistered reserved subprotocol frame.
package connect

import (
	"context"
	"encoding/binary"
	"sort"
	"sync"
	"time"

	"github.com/urnetwork/connect/protocol"
)

const (
	networkQualitySubprotocolId              SubprotocolId = 1
	networkQualityMessageByteCount                         = len(Id{}) + 8
	networkQualityMaximumRememberedPeerCount               = 1024
	networkQualityNotificationRetryInterval                = 100 * time.Millisecond
)

type networkQualityPeer struct {
	destinationId   Id
	transferOptions TransferOptions
	transferKey     TransferKey
	observedAt      time.Time
}

type networkQualityRemoteGeneration struct {
	instanceId Id
	generation uint64
	observedAt time.Time
}

type networkQualityPendingPeer struct {
	peer       networkQualityPeer
	generation uint64
}

type networkQualityPendingLocal struct {
	at         time.Time
	generation uint64
}

// One client-owned worker keeps OS listener and shared receive callbacks
// nonblocking. State under stateLock is copied before estimator or send calls.
type clientNetworkQualityState struct {
	client     *Client
	instanceId Id
	worker     *coalescingCallbackWorker

	stateLock                    sync.Mutex
	lastLocalNotification        time.Time
	localGeneration              uint64
	pendingLocal                 networkQualityPendingLocal
	pendingRemotePeerIdAts       map[Id]time.Time
	pendingPeerNotifications     map[Id]networkQualityPendingPeer
	observedPeers                map[Id]networkQualityPeer
	remoteGenerations            map[Id]networkQualityRemoteGeneration
	removeGlobalListener         func()
	removeSubprotocolListener    func()
	closeOnce                    sync.Once
	beforeApplyForTest           func()
	afterLocalApplyForTest       func(uint64)
	afterRemoteApplyForTest      func(Id)
	afterPeerNotificationForTest func(Id)
}

func newClientNetworkQualityState(client *Client) *clientNetworkQualityState {
	state := &clientNetworkQualityState{
		client:                   client,
		instanceId:               NewId(),
		pendingRemotePeerIdAts:   map[Id]time.Time{},
		pendingPeerNotifications: map[Id]networkQualityPendingPeer{},
		observedPeers:            map[Id]networkQualityPeer{},
		remoteGenerations:        map[Id]networkQualityRemoteGeneration{},
	}
	state.worker = newCoalescingCallbackWorker(client.ctx, state.applyPending)
	removeSubprotocolListener, err := client.addReservedSubprotocolRawCallback(
		networkQualitySubprotocolId,
		state.receive,
	)
	if err != nil {
		panic(err)
	}
	state.removeSubprotocolListener = removeSubprotocolListener
	state.removeGlobalListener = AddNetworkQualityChangeListener(client.NetworkQualityChanged)
	return state
}

// NetworkQualityChanged requests a bounded estimator remeasurement for this
// client and its known peers. It only enqueues work and is safe on an OS
// listener thread.
func (self *Client) NetworkQualityChanged() {
	if self == nil || self.networkQuality == nil {
		return
	}
	self.networkQuality.dispatchLocal(time.Now())
}

func (self *clientNetworkQualityState) dispatchLocal(at time.Time) {
	self.stateLock.Lock()
	if !self.lastLocalNotification.IsZero() &&
		at.Sub(self.lastLocalNotification) <= windowQualityRemeasureInterval {
		if at.After(self.lastLocalNotification) {
			self.lastLocalNotification = at
		}
		self.stateLock.Unlock()
		return
	}
	self.lastLocalNotification = at
	self.localGeneration += 1
	self.pendingLocal = networkQualityPendingLocal{
		at:         at,
		generation: self.localGeneration,
	}
	self.stateLock.Unlock()
	self.worker.Dispatch()
}

// Every inbound peer is remembered before subprotocol dispatch. A provider
// can therefore notify a receive-only client even when it has no return send
// sequence yet.
func (self *Client) observeNetworkQualityPeer(source TransferPath, peer Peer) {
	if self == nil || self.networkQuality == nil {
		return
	}
	self.networkQuality.observePeer(source, peer)
}

func (self *clientNetworkQualityState) observePeer(source TransferPath, peer Peer) {
	destinationId := source.SourceId
	if destinationId == (Id{}) || destinationId == ControlId || destinationId == self.client.clientId {
		return
	}
	provideMode := protocol.ProvideMode_Stream
	if peer.ProvideMode == protocol.ProvideMode_Network {
		provideMode = protocol.ProvideMode_Network
	}
	transferKey := providerReplyTransferKey(peer.TransferKey, provideMode)
	transferOptions := providerReturnTransferOptions(
		self.client.settings.DefaultTransferOpts,
		provideMode,
		transferKey,
	)
	transferOptions.Ack = true

	self.stateLock.Lock()
	if _, found := self.observedPeers[destinationId]; !found &&
		len(self.observedPeers) >= networkQualityMaximumRememberedPeerCount {
		var oldestId Id
		oldestAt := time.Time{}
		for candidateId, candidate := range self.observedPeers {
			if oldestAt.IsZero() || candidate.observedAt.Before(oldestAt) ||
				(candidate.observedAt.Equal(oldestAt) && candidateId.LessThan(oldestId)) {
				oldestId, oldestAt = candidateId, candidate.observedAt
			}
		}
		delete(self.observedPeers, oldestId)
	}
	self.observedPeers[destinationId] = networkQualityPeer{
		destinationId:   destinationId,
		transferOptions: transferOptions,
		transferKey:     transferKey,
		observedAt:      time.Now(),
	}
	self.stateLock.Unlock()
}

func encodeNetworkQualityMessage(instanceId Id, generation uint64) []byte {
	messageBytes := MessagePoolGet(networkQualityMessageByteCount)
	copy(messageBytes[:len(instanceId)], instanceId[:])
	binary.BigEndian.PutUint64(messageBytes[len(instanceId):], generation)
	return messageBytes
}

func decodeNetworkQualityMessage(messageBytes []byte) (Id, uint64, bool) {
	if len(messageBytes) != networkQualityMessageByteCount {
		return Id{}, 0, false
	}
	instanceId, err := IdFromBytes(messageBytes[:len(Id{})])
	if err != nil {
		return Id{}, 0, false
	}
	generation := binary.BigEndian.Uint64(messageBytes[len(Id{}):])
	return instanceId, generation, instanceId != (Id{}) && generation != 0
}

// The receive callback validates, deduplicates and enqueues only. Estimator
// locks are taken by the client-owned worker, outside the shared receive path.
func (self *clientNetworkQualityState) receive(
	source TransferPath,
	_ SubprotocolId,
	messageBytes []byte,
	_ Peer,
) {
	instanceId, generation, ok := decodeNetworkQualityMessage(messageBytes)
	if !ok || source.SourceId == (Id{}) || source.SourceId == ControlId {
		return
	}
	self.stateLock.Lock()
	previous, found := self.remoteGenerations[source.SourceId]
	if found {
		if instanceId.LessThan(previous.instanceId) ||
			(previous.instanceId == instanceId && generation <= previous.generation) {
			self.stateLock.Unlock()
			return
		}
	}
	if !found && len(self.remoteGenerations) >= networkQualityMaximumRememberedPeerCount {
		var oldestId Id
		oldestAt := time.Time{}
		for candidateId, candidate := range self.remoteGenerations {
			if oldestAt.IsZero() || candidate.observedAt.Before(oldestAt) ||
				(candidate.observedAt.Equal(oldestAt) && candidateId.LessThan(oldestId)) {
				oldestId, oldestAt = candidateId, candidate.observedAt
			}
		}
		delete(self.remoteGenerations, oldestId)
		delete(self.pendingRemotePeerIdAts, oldestId)
	}
	at := time.Now()
	self.remoteGenerations[source.SourceId] = networkQualityRemoteGeneration{
		instanceId: instanceId,
		generation: generation,
		observedAt: at,
	}
	self.pendingRemotePeerIdAts[source.SourceId] = at
	self.stateLock.Unlock()
	self.worker.Dispatch()
}

func (self *clientNetworkQualityState) applyPending() {
	self.stateLock.Lock()
	local := self.pendingLocal
	self.pendingLocal = networkQualityPendingLocal{}
	remotePeerIdAts := self.pendingRemotePeerIdAts
	self.pendingRemotePeerIdAts = map[Id]time.Time{}
	beforeApplyForTest := self.beforeApplyForTest
	afterLocalApplyForTest := self.afterLocalApplyForTest
	afterRemoteApplyForTest := self.afterRemoteApplyForTest
	self.stateLock.Unlock()

	if beforeApplyForTest != nil {
		beforeApplyForTest()
	}
	if local.generation != 0 {
		self.client.sendBuffer.networkQualityChanged(Id{}, local.at)
		self.queuePeerNotifications(local.generation)
		if afterLocalApplyForTest != nil {
			afterLocalApplyForTest(local.generation)
		}
	}
	peerIds := make([]Id, 0, len(remotePeerIdAts))
	for peerId := range remotePeerIdAts {
		peerIds = append(peerIds, peerId)
	}
	sort.Slice(peerIds, func(i, j int) bool { return peerIds[i].LessThan(peerIds[j]) })
	for _, peerId := range peerIds {
		self.client.sendBuffer.networkQualityChanged(peerId, remotePeerIdAts[peerId])
		if afterRemoteApplyForTest != nil {
			afterRemoteApplyForTest(peerId)
		}
	}
	if self.sendPeerNotifications() {
		select {
		case <-self.client.ctx.Done():
		case <-time.After(networkQualityNotificationRetryInterval):
			self.worker.Dispatch()
		}
	}
}

func (self *clientNetworkQualityState) queuePeerNotifications(generation uint64) {
	peerByDestinationId := self.client.sendBuffer.networkQualityPeers()
	self.stateLock.Lock()
	for destinationId, peer := range self.observedPeers {
		peerByDestinationId[destinationId] = peer
	}
	for destinationId, peer := range peerByDestinationId {
		if _, found := self.pendingPeerNotifications[destinationId]; !found &&
			len(self.pendingPeerNotifications) >= networkQualityMaximumRememberedPeerCount {
			var oldestId Id
			oldestAt := time.Time{}
			haveOldest := false
			for candidateId, candidate := range self.pendingPeerNotifications {
				candidateAt := candidate.peer.observedAt
				if !haveOldest || candidateAt.Before(oldestAt) ||
					(candidateAt.Equal(oldestAt) && candidateId.LessThan(oldestId)) {
					oldestId, oldestAt = candidateId, candidateAt
					haveOldest = true
				}
			}
			delete(self.pendingPeerNotifications, oldestId)
		}
		self.pendingPeerNotifications[destinationId] = networkQualityPendingPeer{
			peer:       peer,
			generation: generation,
		}
	}
	self.stateLock.Unlock()
}

func (self *clientNetworkQualityState) sendPeerNotifications() bool {
	self.stateLock.Lock()
	pendingByDestinationId := make(map[Id]networkQualityPendingPeer, len(self.pendingPeerNotifications))
	for destinationId, pending := range self.pendingPeerNotifications {
		pendingByDestinationId[destinationId] = pending
	}
	afterPeerNotificationForTest := self.afterPeerNotificationForTest
	self.stateLock.Unlock()

	destinationIds := make([]Id, 0, len(pendingByDestinationId))
	for destinationId := range pendingByDestinationId {
		destinationIds = append(destinationIds, destinationId)
	}
	sort.Slice(destinationIds, func(i, j int) bool { return destinationIds[i].LessThan(destinationIds[j]) })
	for _, destinationId := range destinationIds {
		pending := pendingByDestinationId[destinationId]
		messageBytes := encodeNetworkQualityMessage(self.instanceId, pending.generation)
		success, err := self.client.SendSubprotocolBytesWithTimeout(
			networkQualitySubprotocolId,
			messageBytes,
			destinationId,
			func(error) {},
			0,
			pending.peer.transferOptions,
			pending.peer.transferKey,
		)
		if success && err == nil {
			self.stateLock.Lock()
			if current, found := self.pendingPeerNotifications[destinationId]; found &&
				current.generation == pending.generation {
				delete(self.pendingPeerNotifications, destinationId)
			}
			self.stateLock.Unlock()
		}
		if success && err == nil && afterPeerNotificationForTest != nil {
			afterPeerNotificationForTest(destinationId)
		}
	}
	self.stateLock.Lock()
	retry := len(self.pendingPeerNotifications) != 0
	self.stateLock.Unlock()
	return retry
}

func (self *SendBuffer) networkQualityPeers() map[Id]networkQualityPeer {
	peers := map[Id]networkQualityPeer{}
	if self == nil {
		return peers
	}
	self.mutex.Lock()
	if !self.closed {
		for id, sequence := range self.sendSequences {
			if id.Destination == (Id{}) || id.Destination == ControlId ||
				id.Destination == self.client.clientId {
				continue
			}
			if _, found := peers[id.Destination]; found && id.LogicalLane != 0 {
				continue
			}
			transferOptions := self.client.settings.DefaultTransferOpts
			transferOptions.Ack = true
			transferOptions.CompanionContract = sequence.companionContract
			transferOptions.ForceStream = sequence.forceStream
			transferOptions.NetworkPeer = sequence.networkPeer
			peers[id.Destination] = networkQualityPeer{
				destinationId:   id.Destination,
				transferOptions: transferOptions,
				transferKey: TransferKey{
					ForceStream:         sequence.forceStream,
					CompanionContract:   sequence.companionContract,
					EncryptionRole:      sequence.encryptionRole.toProtobuf(),
					EncryptionCompanion: sequence.encryptionCompanion,
					LogicalLane:         sequence.logicalLane,
				},
			}
		}
	}
	self.mutex.Unlock()
	return peers
}

func (self *clientNetworkQualityState) close() {
	if self == nil {
		return
	}
	self.closeOnce.Do(func() {
		self.removeGlobalListener()
		self.removeSubprotocolListener()
		self.worker.Close()
	})
}

func (self *clientNetworkQualityState) wait(ctx context.Context) error {
	if self == nil {
		return nil
	}
	return waitForLifecycleDone(ctx, self.worker.done, "network quality worker")
}
