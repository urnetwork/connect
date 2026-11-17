// Actual ordinary source and receiver rounds qualify feedback chunk policy and completion.
package connect

import (
	"bytes"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
	"unsafe"

	"github.com/urnetwork/connect/protocol"
)

// This fixture owns the ordinary parent and borrows real two-client wire rounds.
// Only copied native bytes and scalar costs survive a receiver callback.
type pureAckChunkFixture struct {
	*windowRoundFixture
	parent                                                *RemoteUserNatMultiClient
	selected                                              *multiClientChannel
	transport                                             Transport
	received                                              [][]byte
	starts, writes, terminals, terminalErrors             atomic.Int64
	initialChunks                                         int
	admitted                                              int
	rawRootBytes, retainedRootBytes, carrierRootBytes     ByteCount
	physical                                              int
	decodedSamples, legacyGraphs, decodedPointerMax       int
	decodedBackingSum, decodedMetadataSum, decodedMaximum ByteCount
}

// Every source policy and hook is installed before either client starts.
func newPureAckChunkFixture(t *testing.T, version int, carrier TransportType,
	configure func(*SendBufferSettings), receive func(*ReceiveBufferSettings)) *pureAckChunkFixture {
	t.Helper()
	self := &pureAckChunkFixture{}
	self.windowRoundFixture = newWindowRoundFixture(t, func(settings *SendBufferSettings) {
		settings.SequenceBufferSize = DefaultClientSettings().SendBufferSettings.SequenceBufferSize
		settings.ProtocolVersion = version
		settings.SendPackLifecycleObserver = func(event SendPackLifecycleObservation) {
			switch event.Phase {
			case SendPackLifecyclePhaseStarted:
				self.starts.Add(1)
			case SendPackLifecyclePhaseFirstRouteWrite:
				self.writes.Add(1)
			case SendPackLifecyclePhaseTerminal:
				if event.Err != nil {
					self.terminalErrors.Add(1)
				}
				self.terminals.Add(1)
			}
		}
		settings.afterGroupAdmissionForTest = func(target sendGroupAdmissionTarget) {
			if group, ok := target.(*parsedPacketGroup); ok {
				self.admitted += len(group.packets)
			}
		}
		settings.afterCreateSendGroupCompletionForTest = func(_ sendSequenceId, count int) { self.initialChunks = count }
		if configure != nil {
			configure(settings)
		}
	}, receive, carrier)
	parent, update, closeParent := groupTestParent(t, DisableSecurityPolicy())
	self.parent = parent
	parent.settings.ProtocolVersion = version
	parent.settings.TcpCollapsePrevention = true
	parent.settings.TcpCollapseMaxHold = 0
	self.selected = newPacketTransferTestChannel()
	self.selected.ctx, self.selected.client, self.selected.settings = parent.ctx, self.sender, parent.settings
	self.selected.args = &multiClientChannelArgs{Destination: RequireMultiHopId(self.receiver.ClientId())}
	update.client.Store(self.selected)
	t.Cleanup(func() { closeParent(); self.selected.closeConnectSynOwners() })
	self.receiver.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, peer Peer) {
		receipt := peer.deliveryReceipt()
		if receipt == nil || receipt.item == nil {
			t.Error("actual immediate TCP callback lacks its receipt owner")
			return
		}
		cost := pureAckReceivedItemCost(t, receipt.item)
		self.decodedSamples++
		if cost.legacyGraph {
			self.legacyGraphs++
		}
		self.decodedPointerMax = max(self.decodedPointerMax, cost.pointerCapacity)
		self.decodedBackingSum += cost.uniqueBackingBytes
		self.decodedMetadataSum += cost.metadataEnvelope
		self.decodedMaximum = max(self.decodedMaximum, cost.uniqueBackingBytes+cost.metadataEnvelope)
		for _, frame := range frames {
			if frame.MessageType != protocol.MessageType_IpIpPacketToProvider {
				t.Error("changed provider-IP message type")
				continue
			}
			packet, err := ipPacketToProviderBytes(frame)
			if err != nil {
				t.Error(err)
				continue
			}
			self.received = append(self.received, bytes.Clone(packet))
		}
	})
	manager := self.sender.RouteManager()
	manager.mutex.Lock()
	for transport, routes := range manager.writerMatchState.transportRoutes {
		if len(routes) == 1 && routes[0] == self.senderOut {
			if self.transport != nil {
				t.Error("ambiguous initial fixture carrier")
			}
			self.transport = transport
		}
	}
	manager.mutex.Unlock()
	if self.transport == nil {
		t.Fatal("fixture did not register its actual writer carrier")
	}
	return self
}

// Only a complete public admission transfers the caller's raw packet owners.
func (self *pureAckChunkFixture) offer(count int) [][]byte {
	self.t.Helper()
	packets := make([][]byte, count)
	templates := make([][]byte, count)
	accepted := make([]bool, count)
	for index := range templates {
		templates[index] = groupRecoveryPacket(1001, 5001+uint32(index), uint16(4096+index), tcpFlagAck, nil)
		packets[index] = MessagePoolCopy(templates[index])
		self.rawRootBytes += MessagePoolRootByteCount(packets[index])
	}
	if got := self.parent.SendPacketBatchWithResults(SourceId(self.sender.ClientId()), protocol.ProvideMode_Network, packets, -1, accepted); got != count {
		self.t.Fatalf("ordinary feedback batch refused: count=%d/%d bits=%v", got, count, accepted)
	}
	for _, ok := range accepted {
		if !ok {
			self.t.Fatal("ordinary group lost an accepted member")
		}
	}
	synctest.Wait()
	return templates
}

// Held receipts keep both the encoded source root and its one group completion live.
func (self *pureAckChunkFixture) original() *windowRoundFrame {
	self.t.Helper()
	wire := self.take(self.senderOut)
	if wire.pack == nil || wire.pack.Nack || !wire.pack.DeliverImmediately || len(wire.pack.Frames) == 0 || len(wire.pack.Frames) > sendPackH1GroupMaxFrames || !protocolOnlyPack(wire.pack) {
		self.t.Fatal("feedback original escaped reliable protocol/storage policy")
	}
	source := self.sequence()
	item := source.resendQueue.GetBySequenceNumber(wire.pack.SequenceNumber)
	if item == nil || !item.noBudget || !item.deliverImmediately || item.acks.count != 1 || item.acks.overflow != nil {
		self.t.Fatal("one logical chunk allocated unrelated completion ownership")
	}
	self.retainedRootBytes += MessagePoolRootByteCount(item.transferFrameBytes)
	self.carrierRootBytes += MessagePoolRootByteCount(wire.bytes)
	self.physical++
	return wire
}

// The receiver supplies its real exact-original terminal/cumulative response set.
func (self *pureAckChunkFixture) deliver(wire *windowRoundFrame) []*windowRoundFrame {
	self.t.Helper()
	self.forward(wire, self.receiverIn)
	return takePureAckCustodyReceipts(self.t, self.windowRoundFixture, wire)
}

// Real peer feedback retires every child before the one logical terminal is checked.
func (self *pureAckChunkFixture) finish(receipts []*windowRoundFrame, templates [][]byte, before time.Time) {
	self.t.Helper()
	if len(self.received) != len(templates) {
		self.t.Fatalf("native delivery count=%d want=%d", len(self.received), len(templates))
	}
	for index, packet := range templates {
		if !bytes.Equal(packet, self.received[index]) {
			self.t.Fatalf("native header/event %d changed or reordered", index)
		}
	}
	for _, receipt := range receipts {
		self.forward(receipt, self.senderIn)
	}
	synctest.Wait()
	source := self.sequence()
	self.selected.stateLock.Lock()
	acked, pending := self.selected.packetStats.sendAckCount, self.selected.packetStats.sendNackCount
	self.selected.stateLock.Unlock()
	if count, size := source.resendQueue.QueueSize(); count != 0 || size != 0 || source.immediateSendCount != 0 || self.starts.Load() != 1 || self.writes.Load() != 1 || self.terminals.Load() != 1 || self.terminalErrors.Load() != 0 || acked != len(templates) || pending != 0 || !time.Now().Equal(before) {
		self.t.Fatalf("logical/physical ownership did not close: retained=%d/%d immediate=%d phases=%d/%d/%d errors=%d acked=%d pending=%d", count, size, source.immediateSendCount, self.starts.Load(), self.writes.Load(), self.terminals.Load(), self.terminalErrors.Load(), acked, pending)
	}
	if self.decodedSamples != self.physical {
		self.t.Fatalf("decoded cost cuts=%d want physical=%d", self.decodedSamples, self.physical)
	}
	self.t.Logf("feedback-decode-cost samples=%d legacy_graphs=%d pointer_cap_max=%d unique_backing_sum=%d metadata_envelope_sum=%d max_live_decoded_component_estimate=%d scaled_32_owner_component_estimate=%d", self.decodedSamples, self.legacyGraphs, self.decodedPointerMax, self.decodedBackingSum, self.decodedMetadataSum, self.decodedMaximum, 32*self.decodedMaximum)
	self.t.Logf("feedback-cost raw_roots=%d retained_encoded_roots=%d carrier_roots=%d physical_items=%d send_item_size=%d native_headers=%d protocol_frame_size=%d pointer_size=%d", self.rawRootBytes, self.retainedRootBytes, self.carrierRootBytes, self.physical, unsafe.Sizeof(sendItem{}), len(templates), unsafe.Sizeof(protocol.Frame{}), unsafe.Sizeof((*protocol.Frame)(nil)))
}

// Both wire versions use the independent owner cap, legacy disabled policy and
// lower message bytes without changing callback count, native bytes or custody.
func TestPureTcpAckChunkOwnerPolicyUsesActualOrdinaryRounds(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		for _, version := range []int{1, 2} {
			for _, c := range []struct {
				name   string
				pure   int
				shared int
				bytes  ByteCount
				first  int
			}{
				{name: "default", pure: 16, shared: 2, bytes: DefaultMtu, first: 16},
				{name: "zero", pure: 0, shared: 2, bytes: DefaultMtu, first: 2},
				{name: "negative", pure: -1, shared: 2, bytes: DefaultMtu, first: 2},
				{name: "pure-one", pure: 1, shared: 2, bytes: DefaultMtu, first: 1},
				{name: "shared-one", pure: 16, shared: 1, bytes: DefaultMtu, first: 16},
				{name: "lower-bytes", pure: 16, shared: 2, bytes: 64, first: 1},
			} {
				f := newPureAckChunkFixture(t, version, TransportTypeP2p, func(settings *SendBufferSettings) {
					settings.ChunkSettings.PureTcpAckMaxFrames = c.pure
					settings.ChunkSettings.SharedMaxFrames = c.shared
					settings.ChunkSettings.SharedMaxMessageByteCount = c.bytes
				}, nil)
				before := time.Now()
				templates := f.offer(17)
				var receipts []*windowRoundFrame
				first := true
				for len(f.senderOut) != 0 {
					wire := f.original()
					if first && len(wire.pack.Frames) != c.first {
						t.Fatalf("%s v%d first=%d want=%d", c.name, version, len(wire.pack.Frames), c.first)
					}
					first = false
					if MessageByteCount(wire.pack.Frames) > c.bytes {
						t.Fatalf("%s v%d exceeded message-byte cap", c.name, version)
					}
					receipts = append(receipts, f.deliver(wire)...)
				}
				if first || f.initialChunks != f.physical || f.admitted != 17 || f.terminals.Load() != 0 || f.sequence().immediateSendCount != f.physical || !time.Now().Equal(before) {
					t.Fatalf("%s v%d lost raw/physical/callback policy", c.name, version)
				}
				f.finish(receipts, templates, before)
				if version == 2 && f.legacyGraphs != 0 {
					t.Fatalf("%s v%d did not retain its inline decoded owner: legacy=%d physical=%d", c.name, version, f.legacyGraphs, f.physical)
				}
			}
		}
	})
}

// An initially unknown-H1 group keeps its already-proved established byte cap;
// the new non-H1 policy must not replace that cap with H1's opening limit.
func TestPureTcpAckChunkInitiallyH1KeepsEstablishedOwnerCap(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		for _, version := range []int{1, 2} {
			f := newPureAckChunkFixture(t, version, TransportTypeH1, func(settings *SendBufferSettings) {
				settings.ChunkSettings.H1UnknownMaxFrames = 8
				settings.ChunkSettings.H1UnknownMaxMessageByteCount = 80
				settings.ChunkSettings.H1EstablishedMaxMessageByteCount = 512
			}, nil)
			before := time.Now()
			templates := f.offer(8)
			wire := f.original()
			if len(wire.pack.Frames) != 8 || MessageByteCount(wire.pack.Frames) <= 80 || MessageByteCount(wire.pack.Frames) > 512 {
				t.Fatalf("v%d undid established H1 partition: frames=%d bytes=%d", version, len(wire.pack.Frames), MessageByteCount(wire.pack.Frames))
			}
			receipts := f.deliver(wire)
			if len(f.senderOut) != 0 || f.terminals.Load() != 0 {
				t.Fatal("initially H1 group changed physical/logical lifetime")
			}
			f.finish(receipts, templates, before)
		}
	})
}

// Physical H1 ceilings include frame-entry and legacy-wrapper overhead; the
// full-head future-proof bound remains authoritative for both wire versions.
func TestPureTcpAckChunkRetainsFullH1EnvelopeBound(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		for _, version := range []int{1, 2} {
			f := newPureAckChunkFixture(t, version, TransportTypeH1, func(settings *SendBufferSettings) { settings.ChunkSettings.H1KnownMaxFrames = 16 }, nil)
			const ceiling = 512
			f.sender.RouteManager().UpdateTransportWithProperties(f.transport, []Route{f.senderOut}, TransferCarrierProperties{MaxMessageByteCount: ceiling})
			before := time.Now()
			templates := f.offer(16)
			var receipts []*windowRoundFrame
			for len(f.senderOut) != 0 {
				wire := f.original()
				if len(wire.bytes) > ceiling || !f.sequence().h1FramesEnvelopeFits(wire.pack.Frames, ceiling) {
					t.Fatalf("v%d escaped complete H1 ceiling: wire=%d frames=%d", version, len(wire.bytes), len(wire.pack.Frames))
				}
				receipts = append(receipts, f.deliver(wire)...)
			}
			if f.physical <= 1 || f.initialChunks != f.physical || f.terminals.Load() != 0 {
				t.Fatal("bounded H1 partition did not preserve its child completion count")
			}
			f.finish(receipts, templates, before)
		}
	})
}

// A dense prefix and a narrower replacement share one logical owner. The
// expected remainder grows before any real prefix receipt can finish it.
func TestPureTcpAckChunkRouteReplacementCountsEveryRemainingChild(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		for _, version := range []int{1, 2} {
			entered, release := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			var armed atomic.Bool
			armed.Store(true)
			f := newPureAckChunkFixture(t, version, TransportTypeP2p, func(settings *SendBufferSettings) {
				settings.ChunkSettings.H1KnownMaxFrames = 2
				settings.afterInitialWriteQueuedForTest = func(_ sendSequenceId, _ uint64) {
					if armed.CompareAndSwap(true, false) {
						close(entered)
						<-release
					}
				}
			}, nil)
			t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
			before := time.Now()
			templates := f.offer(64)
			select {
			case <-entered:
			default:
				t.Fatal("dense first child never reached the real post-write barrier")
			}
			prefix := f.original()
			if len(prefix.pack.Frames) != 16 || f.initialChunks != 4 {
				t.Fatalf("v%d did not pin dense first partition: frames=%d children=%d", version, len(prefix.pack.Frames), f.initialChunks)
			}
			source := f.sequence()
			item := source.resendQueue.GetBySequenceNumber(prefix.pack.SequenceNumber)
			completion := item.acks.records[0].group
			if completion == nil {
				t.Fatal("dense prefix has no live logical completion owner")
			}
			for _, receipt := range f.deliver(prefix) {
				f.forward(receipt, f.senderIn)
			}
			replacement := NewSendGatewayTransportWithType(TransportTypeH1)
			// Run is still parked. Retire the old registration before reusing
			// its Route key so removal cannot deactivate the new registration.
			f.sender.RouteManager().RemoveTransport(f.transport)
			f.sender.RouteManager().UpdateTransportWithProperties(replacement, []Route{f.senderOut}, TransferCarrierProperties{MaxMessageByteCount: 512})
			active := source.contractMultiRouteWriter.GetActiveRoutes()
			policy := source.transferFlightPolicy()
			if len(active) != 1 || active[0] != f.senderOut || !policy.h1Only || !policy.h1MessageLimitsKnown || policy.h1MaxMessageByteCount != 512 {
				t.Fatal("parked replacement did not publish its exact active H1 route and ceiling")
			}
			releaseOnce.Do(func() { close(release) })
			synctest.Wait()
			var receipts []*windowRoundFrame
			for len(f.senderOut) != 0 {
				wire := f.original()
				if len(wire.pack.Frames) != 2 || len(wire.bytes) > 512 {
					t.Fatalf("v%d replacement inherited dense partition: frames=%d wire=%d", version, len(wire.pack.Frames), len(wire.bytes))
				}
				receipts = append(receipts, f.deliver(wire)...)
			}
			completion.mutex.Lock()
			children, completed, done := completion.chunkCount, completion.terminalCount, completion.terminalDone
			completion.mutex.Unlock()
			if f.physical != 25 || children != 25 || completed != 1 || done || f.terminals.Load() != 0 || source.immediateSendCount != 24 || !time.Now().Equal(before) {
				t.Fatalf("v%d replacement completion mismatch: physical=%d children=%d terminal_children=%d done=%t callbacks=%d custody=%d", version, f.physical, children, completed, done, f.terminals.Load(), source.immediateSendCount)
			}
			f.finish(receipts, templates, before)
		}
	})
}
