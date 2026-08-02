package connect

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/urnetwork/connect/protocol"
)

func DefaultStreamManagerSettings() *StreamManagerSettings {
	return &StreamManagerSettings{
		StreamBufferSettings: DefaultStreamBufferSettings(),
		// WebRtcSettings:       DefaultWebRtcSettings(),
	}
}

func DefaultStreamBufferSettings() *StreamBufferSettings {
	return &StreamBufferSettings{
		ReadTimeout:          time.Duration(-1),
		WriteTimeout:         time.Duration(-1),
		P2pTransportSettings: DefaultP2pTransportSettings(),
	}
}

type StreamManagerSettings struct {
	StreamBufferSettings *StreamBufferSettings
}

type StreamManager struct {
	ctx context.Context

	client *Client

	webRtcManager *WebRtcManager

	streamBuffer *StreamBuffer

	streamManagerSettings *StreamManagerSettings
	// A provider-owned client must retire every disallowed P2P stream
	// direction. An ordinary destination client preserves all StreamOpen
	// directions because they are transport state, not provider policy.
	providerStreamPolicy bool
}

func NewStreamManager(ctx context.Context, client *Client, webRtcManager *WebRtcManager, streamManagerSettings *StreamManagerSettings) *StreamManager {
	streamManager := &StreamManager{
		ctx:                   ctx,
		client:                client,
		streamManagerSettings: streamManagerSettings,
		providerStreamPolicy:  client.settings.ProviderStreamPolicy,
	}

	// webRtcManager := NewWebRtcManager(ctx, streamManagerSettings.WebRtcSettings)

	streamManager.initBuffers(webRtcManager)

	return streamManager
}

func (self *StreamManager) initBuffers(webRtcManager *WebRtcManager) {
	self.webRtcManager = webRtcManager
	self.streamBuffer = NewStreamBuffer(self.ctx, self, self.streamManagerSettings.StreamBufferSettings)
}

func (self *StreamManager) Client() *Client {
	return self.client
}

func (self *StreamManager) WebRtcManager() *WebRtcManager {
	return self.webRtcManager
}

// allowStreamOpen rejects provider/relay work when this client no longer
// advertises the corresponding provide mode. A source id means traffic enters
// this client from a peer: source-only is a provider endpoint, while source +
// destination is an intermediary relay. Only destination-only streams are
// outbound client work and independent of this client's provide policy.
func (self *StreamManager) allowStreamOpen(sourceId *Id, destinationId *Id) bool {
	if !self.providerStreamPolicy {
		// An ordinary destination/window client does not serve provider work.
		// Its source-only, destination-only, and paired entries are transport
		// directions for its own connection, including return/companion state
		// restored by StreamReset. Provider contract verification remains the
		// authorization boundary; do not infer provider policy from direction.
		return true
	}

	// A public-capable provider accepts clients that never appeared in its
	// same-network peer list. An explicit disconnect marker is different: it
	// proves this particular identity's old stream is stale, even while Public
	// providing remains enabled. Reject delayed relists until a peer reconnect
	// update clears the marker.
	if sourceId != nil && self.client.peerManager.isDisconnectedNetworkPeer(*sourceId) {
		return false
	}
	if destinationId != nil && self.client.peerManager.isDisconnectedNetworkPeer(*destinationId) {
		return false
	}

	allowAny, allowNetwork := self.client.contractManager.inboundProviderStreamPolicy()
	if allowAny {
		return true
	}
	if !allowNetwork || (sourceId == nil) == (destinationId == nil) {
		// no adjacent peer, or two adjacent peers (public relay)
		return false
	}
	peerId := destinationId
	if sourceId != nil {
		peerId = sourceId
	}
	return self.client.peerManager.isConnectedNetworkPeer(*peerId)
}

// reconcileInboundProviderStreams retires already-open provider streams after
// a provide-mode/pause change. Merely unregistering the provide key stops new
// contracts at the platform, but old StreamOpen state otherwise keeps its P2P
// transports alive indefinitely, including admission and crypto retry loops.
func (self *StreamManager) reconcileInboundProviderStreams(allowAny bool, allowNetwork bool) {
	self.streamBuffer.CloseDisallowedInboundProviderStreams(
		allowAny,
		allowNetwork,
		self.providerStreamPolicy,
		self.client.peerManager.isConnectedNetworkPeer,
	)
}

// closeDisconnectedPeerStreams retires streams owned by identities that the
// platform explicitly disconnected. This is independent of Public provider
// policy: allowing unknown public clients must not retain known-dead Network
// clients and their P2P admission/crypto retry loops.
func (self *StreamManager) closeDisconnectedPeerStreams(peerIds map[Id]bool) {
	if !self.providerStreamPolicy || len(peerIds) == 0 {
		return
	}
	self.streamBuffer.CloseDisconnectedPeerStreams(peerIds)
}

// ReceiveFunction
func (self *StreamManager) Receive(source TransferPath, frames []*protocol.Frame, peer Peer) {
	if source.IsControlSource() {
		for _, frame := range frames {
			// ignore error
			self.handleControlFrame(frame)
		}
	}
}

func (self *StreamManager) handleControlFrame(frame *protocol.Frame) error {
	switch frame.MessageType {
	case protocol.MessageType_TransferStreamOpen, protocol.MessageType_TransferStreamClose, protocol.MessageType_TransferStreamReset:
		if message, err := FromFrame(frame); err == nil {

			streamOpenIds := func(v *protocol.StreamOpen) (sourceId *Id, destinationId *Id, streamId Id, err error) {
				if v.SourceId != nil {
					var sourceId_ Id
					sourceId_, err = IdFromBytes(v.SourceId)
					if err != nil {
						return
					}
					sourceId = &sourceId_
				}

				if v.DestinationId != nil {
					var destinationId_ Id
					destinationId_, err = IdFromBytes(v.DestinationId)
					if err != nil {
						return
					}
					destinationId = &destinationId_
				}

				streamId, err = IdFromBytes(v.StreamId)
				return
			}

			streamOpen := func(v *protocol.StreamOpen) error {
				sourceId, destinationId, streamId, err := streamOpenIds(v)
				if err != nil {
					return err
				}
				if !self.allowStreamOpen(sourceId, destinationId) {
					if self.client.log.V(1).Enabled() {
						self.client.log.Infof(
							"[sm]%s reject disabled provider s(%s) source=%v destination=%v\n",
							self.client.ClientTag(),
							streamId,
							sourceId,
							destinationId,
						)
					}
					return nil
				}

				if self.client.log.V(1).Enabled() {
					self.client.log.Infof("[sm]%s open s(%s) %v->%v\n", self.client.ClientTag(), streamId, sourceId, destinationId)
				}
				if _, err := self.streamBuffer.OpenStream(sourceId, destinationId, streamId); err != nil {
					return err
				}
				return nil
			}

			switch v := message.(type) {
			case *protocol.StreamOpen:
				err := streamOpen(v)
				if err != nil {
					return err
				}

			case *protocol.StreamClose:
				streamId, err := IdFromBytes(v.StreamId)
				if err != nil {
					return err
				}

				if self.client.log.V(1).Enabled() {
					self.client.log.Infof("[sm]%s close s(%s)\n", self.client.ClientTag(), streamId)
				}
				self.streamBuffer.CloseStream(streamId)

			case *protocol.StreamReset:
				// reconcile instead of tear down:
				// keep the sequences of relisted streams so that their state
				// (including p2p transports) survives a resident migration.
				// streams not in the list are canceled,
				// and listed streams not yet open are opened below
				keep := map[streamSequenceId]bool{}
				for _, m := range v.Streams {
					sourceId, destinationId, streamId, err := streamOpenIds(m)
					if err != nil {
						continue
					}
					if !self.allowStreamOpen(sourceId, destinationId) {
						continue
					}
					keep[newStreamSequenceId(sourceId, destinationId, streamId)] = true
				}
				if self.client.log.V(1).Enabled() {
					self.client.log.Infof("[sm]%s reset streams = %d\n", self.client.ClientTag(), len(v.Streams))
				}
				self.streamBuffer.ResetStreams(keep)
				for _, m := range v.Streams {
					if err := streamOpen(m); err != nil {
						// skip and continue: one malformed or un-openable
						// entry must not strand the remaining listed
						// streams (they would stay closed until the next
						// reset)
						if self.client.log.V(1).Enabled() {
							self.client.log.Infof("[sm]%s reset open err = %s\n", self.client.ClientTag(), err)
						}
						continue
					}
				}
			}
		}
	}
	return nil
}

func (self *StreamManager) IsStreamOpen(streamId Id) bool {
	return self.streamBuffer.IsStreamOpen(streamId)
}

type StreamBufferSettings struct {
	ReadTimeout  time.Duration
	WriteTimeout time.Duration

	P2pTransportSettings *P2pTransportSettings
}

type streamSequenceId struct {
	SourceId       Id
	HasSource      bool
	DestinationId  Id
	HasDestination bool
	StreamId       Id
}

func newStreamSequenceId(sourceId *Id, destinationId *Id, streamId Id) streamSequenceId {
	streamSequenceId := streamSequenceId{
		StreamId: streamId,
	}
	if sourceId != nil {
		streamSequenceId.SourceId = *sourceId
		streamSequenceId.HasSource = true
	}
	if destinationId != nil {
		streamSequenceId.DestinationId = *destinationId
		streamSequenceId.HasDestination = true
	}
	return streamSequenceId
}

type StreamBuffer struct {
	ctx context.Context

	streamManager *StreamManager

	streamBufferSettings *StreamBufferSettings

	mutex                     sync.Mutex
	streamSequences           map[streamSequenceId]*StreamSequence
	streamSequencesByStreamId map[Id]*StreamSequence
}

func NewStreamBuffer(ctx context.Context, streamManager *StreamManager, streamBufferSettings *StreamBufferSettings) *StreamBuffer {
	return &StreamBuffer{
		ctx:                       ctx,
		streamManager:             streamManager,
		streamBufferSettings:      streamBufferSettings,
		streamSequences:           map[streamSequenceId]*StreamSequence{},
		streamSequencesByStreamId: map[Id]*StreamSequence{},
	}
}

// ResetStreams cancels all stream sequences except those in `keep`.
// A reset that relists the current streams reconciles instead of tearing down,
// so the kept sequences (and their p2p transports) survive a resident migration
func (self *StreamBuffer) ResetStreams(keep map[streamSequenceId]bool) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	for streamSequenceId, streamSequence := range self.streamSequences {
		if !keep[streamSequenceId] {
			streamSequence.Cancel()
		}
	}
}

func (self *StreamBuffer) OpenStream(sourceId *Id, destinationId *Id, streamId Id) (bool, error) {
	streamSequenceId := newStreamSequenceId(sourceId, destinationId, streamId)

	initStreamSequence := func(skip *StreamSequence) *StreamSequence {
		self.mutex.Lock()
		defer self.mutex.Unlock()

		streamSequence, ok := self.streamSequences[streamSequenceId]
		if ok {
			if skip == nil || skip != streamSequence {
				return streamSequence
			} else {
				streamSequence.Cancel()
				delete(self.streamSequences, streamSequenceId)
			}
		}

		if streamSequenceByStreamId, ok := self.streamSequencesByStreamId[streamId]; ok {
			streamSequenceByStreamId.Cancel()
			delete(self.streamSequencesByStreamId, streamId)
		}

		streamSequence = NewStreamSequence(self.ctx, self.streamManager, sourceId, destinationId, streamId, self.streamBufferSettings)

		self.streamSequences[streamSequenceId] = streamSequence
		self.streamSequencesByStreamId[streamId] = streamSequence
		go HandleError(func() {
			defer func() {
				self.mutex.Lock()
				defer self.mutex.Unlock()
				streamSequence.Close()
				// clean up
				if streamSequence == self.streamSequences[streamSequenceId] {
					delete(self.streamSequences, streamSequenceId)
				}
				if streamSequence == self.streamSequencesByStreamId[streamId] {
					delete(self.streamSequencesByStreamId, streamId)
				}
			}()
			streamSequence.Run()
		})
		return streamSequence
	}

	var streamSequence *StreamSequence
	var success bool
	var err error
	for i := 0; i < 2; i += 1 {
		select {
		case <-self.ctx.Done():
			return false, errors.New("Done.")
		default:
		}
		streamSequence = initStreamSequence(streamSequence)
		if success, err = streamSequence.Open(); err == nil {
			return success, nil
		}
		// sequence closed
	}
	return success, err
}

func (self *StreamBuffer) CloseStream(streamId Id) {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	if streamSequence, ok := self.streamSequencesByStreamId[streamId]; ok {
		streamSequence.Cancel()
	}
}

// CloseDisallowedInboundProviderStreams cancels provider endpoint, return,
// companion, and intermediary relay streams that the current strict provider
// policy no longer permits. Network-only policy retains exactly one-adjacent-
// endpoint streams for known same-network peers; it does not permit public
// endpoints or relaying between two other peers. Ordinary destination/window
// clients bypass this provider policy entirely.
func (self *StreamBuffer) CloseDisallowedInboundProviderStreams(
	allowAny bool,
	allowNetwork bool,
	providerStreamPolicy bool,
	isNetworkPeer func(Id) bool,
) {
	if !providerStreamPolicy {
		return
	}

	self.mutex.Lock()
	defer self.mutex.Unlock()

	for id, sequence := range self.streamSequences {
		if allowAny {
			continue
		}
		if allowNetwork && id.HasSource != id.HasDestination {
			peerId := id.DestinationId
			if id.HasSource {
				peerId = id.SourceId
			}
			if isNetworkPeer(peerId) {
				continue
			}
		}
		sequence.Cancel()
	}
}

// CloseDisconnectedPeerStreams cancels every sequence adjacent to a known
// disconnected peer. The platform reconnect update clears the tombstone, and
// a subsequent StreamOpen then constructs a fresh generation.
func (self *StreamBuffer) CloseDisconnectedPeerStreams(peerIds map[Id]bool) {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	for id, sequence := range self.streamSequences {
		if (id.HasSource && peerIds[id.SourceId]) ||
			(id.HasDestination && peerIds[id.DestinationId]) {
			sequence.Cancel()
		}
	}
}

func (self *StreamBuffer) IsStreamOpen(streamId Id) bool {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	_, ok := self.streamSequencesByStreamId[streamId]
	return ok
}

type StreamSequence struct {
	ctx    context.Context
	cancel context.CancelFunc

	streamManager *StreamManager

	streamBufferSettings *StreamBufferSettings

	sourceId      *Id
	destinationId *Id
	streamId      Id

	idleCondition *IdleCondition
}

func NewStreamSequence(
	ctx context.Context,
	streamManager *StreamManager,
	sourceId *Id,
	destinationId *Id,
	streamId Id,
	streamBufferSettings *StreamBufferSettings) *StreamSequence {
	cancelCtx, cancel := context.WithCancel(ctx)

	return &StreamSequence{
		ctx:                  cancelCtx,
		cancel:               cancel,
		streamManager:        streamManager,
		streamBufferSettings: streamBufferSettings,
		sourceId:             sourceId,
		destinationId:        destinationId,
		streamId:             streamId,
		idleCondition:        NewIdleCondition(),
	}
}

func (self *StreamSequence) Open() (bool, error) {
	select {
	case <-self.ctx.Done():
		return false, errors.New("Done.")
	default:
	}

	if !self.idleCondition.UpdateOpen() {
		return false, errors.New("Done.")
	}
	defer self.idleCondition.UpdateClose()

	return true, nil
}

func (self *StreamSequence) Run() {
	defer self.cancel()

	if self.sourceId == nil || self.destinationId == nil {
		clientRouteManager := self.streamManager.Client().RouteManager()

		if self.sourceId != nil {
			NewP2pTransport(
				self.ctx,
				self.streamManager.Client(),
				self.streamManager.WebRtcManager(),
				clientRouteManager,
				clientRouteManager,
				*self.sourceId,
				self.streamId,
				PeerTypeSource,
				self.streamBufferSettings.P2pTransportSettings,
			)
		} else if self.destinationId != nil {
			NewP2pTransport(
				self.ctx,
				self.streamManager.Client(),
				self.streamManager.WebRtcManager(),
				clientRouteManager,
				clientRouteManager,
				*self.destinationId,
				self.streamId,
				PeerTypeDestination,
				self.streamBufferSettings.P2pTransportSettings,
			)
		} else {
			// the stream must have one of source or destination
			if self.streamManager.client.log.V(1).Enabled() {
				self.streamManager.client.log.Infof("[sm] s(%s) missing source or destination.\n", self.streamId)
			}
			return
		}
	} else {
		p2pToDestinationRouteManager := NewRouteManagerWithLogger(self.ctx, fmt.Sprintf("->s(%s)", self.streamId), self.streamManager.client.log)
		p2pToSourceRouteManager := NewRouteManagerWithLogger(self.ctx, fmt.Sprintf("<-s(%s)", self.streamId), self.streamManager.client.log)

		// to destination
		NewP2pTransport(
			self.ctx,
			self.streamManager.Client(),
			self.streamManager.WebRtcManager(),
			p2pToDestinationRouteManager,
			p2pToSourceRouteManager,
			*self.destinationId,
			self.streamId,
			PeerTypeDestination,
			self.streamBufferSettings.P2pTransportSettings,
		)
		// to source
		NewP2pTransport(
			self.ctx,
			self.streamManager.Client(),
			self.streamManager.WebRtcManager(),
			p2pToSourceRouteManager,
			p2pToDestinationRouteManager,
			*self.sourceId,
			self.streamId,
			PeerTypeSource,
			self.streamBufferSettings.P2pTransportSettings,
		)

		forward := func(routeManager *RouteManager) {
			defer self.cancel()

			mrr := routeManager.OpenMultiRouteReader(TransferPath{
				StreamId: self.streamId,
			})
			defer routeManager.CloseMultiRouteReader(mrr)
			mrw := routeManager.OpenMultiRouteWriter(TransferPath{
				StreamId: self.streamId,
			})
			defer routeManager.CloseMultiRouteWriter(mrw)

			for {
				checkpointId := self.idleCondition.Checkpoint()
				transferFrameBytes, err := mrr.Read(self.ctx, self.streamBufferSettings.ReadTimeout)
				if err != nil {
					return
				}
				if transferFrameBytes == nil {
					// idle timeout
					if self.idleCondition.Close(checkpointId) {
						// close the sequence
						return
					}
					// else the sequence was opened again
					continue
				}
				success, err := mrw.WriteDetailed(self.ctx, transferFrameBytes, self.streamBufferSettings.WriteTimeout)
				if err != nil {
					MessagePoolReturn(transferFrameBytes)
					return
				}
				if !success {
					// drop it
					MessagePoolReturn(transferFrameBytes)
				}
			}
		}

		go HandleError(func() {
			forward(p2pToDestinationRouteManager)
		}, self.cancel)
		go HandleError(func() {
			forward(p2pToSourceRouteManager)
		}, self.cancel)
	}

	select {
	case <-self.ctx.Done():
		return
	}
}

func (self *StreamSequence) Cancel() {
	self.cancel()
}

func (self *StreamSequence) Close() {
	self.cancel()
}
