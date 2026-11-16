// A real advancing native ACK can be lost before Transfer custody even when
// the provider has consumed an earlier positive advertised TCP right edge.
package connect

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Every scalar is copied while the actual provider TCP mutex is held.
type nativePositiveProviderCut struct {
	zeroWindowWaitSnapshot
	edge                    uint32
	scale                   uint32
	attempts, duplicateAcks int
}

func nativePositiveProviderSnapshot(sequence *TcpSequence) nativePositiveProviderCut {
	sequence.mutex.Lock()
	defer sequence.mutex.Unlock()
	return nativePositiveProviderCut{
		zeroWindowWaitSnapshot: zeroWindowWaitSnapshot{at: time.Now(), window: sequence.receiveWindowSize,
			ack: sequence.receiveSeqAck, next: sequence.receiveSeq, retained: sequence.returnByteCount,
			chunks: len(sequence.returnChunks) - sequence.returnHead},
		edge: sequence.receiveWindowEnd, scale: sequence.receiveWindowScale,
		attempts: sequence.returnAttempts, duplicateAcks: sequence.returnDuplicateAcks,
	}
}

// The native admission history and applied provider frontier are independent.
type nativePositiveAdmissionCut struct {
	seen     bool
	ack      uint32
	window   uint16
	position uint32
	epoch    uint64
	client   *multiClientChannel
}

func nativePositiveAdmissionSnapshot(f *persistNativeFixture, packet []byte) nativePositiveAdmissionCut {
	path, _, err := ParseIpPathWithPayload(packet)
	if err != nil {
		f.t.Fatal(err)
	}
	update, _, _ := f.parent.sendUpdate(path, flowPin{})
	update.stateLock.Lock()
	defer update.stateLock.Unlock()
	return nativePositiveAdmissionCut{seen: update.sequenceAdmissionStateSeen, ack: update.sequenceAdmissionAck,
		window: update.sequenceAdmissionWindow, position: update.sequenceAckPosition,
		epoch: update.sequenceAdmissionEpoch, client: update.client.Load()}
}

type nativePositiveAppliedPacket struct {
	offer    persistNativeOffer
	provider nativePositiveProviderCut
}

type nativePositiveRecovery struct {
	provider nativePositiveProviderCut
	header   persistNativeHeader
	mode     receiveRecoveryMode
}

// Fixed slots distinguish the manual original from the one optional local
// retry. Borrowed frames and pooled send items never escape their owner hook.
type nativePositiveTargetOriginal struct {
	wire                           persistNativeWire
	token                          uint64
	dispositions, writes, receipts int
}

// Only six source pressure originals and one exact native feedback packet are
// retained. The public native false result never means Transfer acceptance.
type nativePositiveWitness struct {
	t                *testing.T
	f                *persistNativeFixture
	stateLock        sync.Mutex
	changed          chan struct{}
	first, last      *nativePositiveAppliedPacket
	zeroReady        chan persistNativeHeader
	allowZero        chan struct{}
	allowZeroDo      sync.Once
	pressure         atomic.Pointer[nativePositiveAppliedPacket]
	wires            [6]*persistNativeWire
	written          int
	retired          [6]bool
	callbacks        [6]int
	claims           [3]*receiveDeliveryClaim
	claimCount       int
	holdClaims       bool
	lifetimes        [256]nativeWindowCadenceLifecycle
	latestToken      uint64
	cutRequest       chan chan nativeWindowCadenceCut
	target           atomic.Pointer[[]byte]
	opened           atomic.Pointer[nativePositiveProviderCut]
	openedSignal     chan struct{}
	openedDo         sync.Once
	recovery         atomic.Pointer[nativePositiveRecovery]
	recoverySignal   chan struct{}
	recoveryDo       sync.Once
	manualControl    bool
	targetOriginals  [2]nativePositiveTargetOriginal // manual, automatic retry
	manualAckSignal  chan struct{}
	manualAckDo      sync.Once
	responseAcks     chan struct{}
	responseAckCount int
	responseLastAck  uint32
}

func (self *nativePositiveWitness) notify() {
	select {
	case self.changed <- struct{}{}:
	default:
	}
}

func (self *nativePositiveWitness) wait(label string, ready func() bool) {
	self.t.Helper()
	for {
		self.stateLock.Lock()
		ok := ready()
		self.stateLock.Unlock()
		if ok {
			return
		}
		select {
		case <-self.changed:
		case <-self.f.ctx.Done():
			self.t.Fatal(label, self.f.ctx.Err())
		}
	}
}

// Nil/default bridge behavior is unchanged in all existing custody roots.
func newNativePositiveWitness(t *testing.T, manualControl bool) *nativePositiveWitness {
	w := &nativePositiveWitness{t: t, changed: make(chan struct{}, 1), zeroReady: make(chan persistNativeHeader, 1),
		allowZero: make(chan struct{}), cutRequest: make(chan chan nativeWindowCadenceCut, 1),
		openedSignal: make(chan struct{}), recoverySignal: make(chan struct{}), holdClaims: true,
		responseAcks: make(chan struct{}, 2), manualControl: manualControl, manualAckSignal: make(chan struct{})}
	hooks := &persistNativeBridgeHooks{
		writeResponse: func(f *persistNativeFixture, writer http.ResponseWriter, request *http.Request, body []byte) {
			if request.URL.Path != "/asset" {
				_, _ = writer.Write(body)
				return
			}
			// Two real flushed prefixes each await their actual native ACK
			// and terminal receipt; segment coalescing cannot erase this premise.
			for i := range 2 {
				if _, err := writer.Write(body[i*1024 : (i+1)*1024]); err != nil {
					if f.ctx.Err() == nil {
						t.Error("staged origin response", err)
					}
					return
				}
				writer.(http.Flusher).Flush()
				select {
				case <-w.responseAcks:
				case <-f.ctx.Done():
					return
				}
			}
			_, _ = writer.Write(body[2*1024:])
		},
		beforeOffer: func(f *persistNativeFixture, _ []byte, header persistNativeHeader, firstZero bool) {
			if !firstZero {
				return
			}
			w.zeroReady <- header
			select {
			case <-w.allowZero:
			case <-f.ctx.Done():
			}
		},
		afterReceipt: func(f *persistNativeFixture, offer persistNativeOffer) {
			if !offer.header.pureAck() || offer.header.window == 0 || f.firstReturnData.Load() == nil || f.zero.Load() != nil {
				return
			}
			cut := nativePositiveProviderSnapshot(f.providerSequence.Load())
			if offer.wire == nil || cut.ack != offer.header.ack {
				t.Error("native positive ACK terminal did not pin its applied frontier")
				return
			}
			if offer.wire.owner.LogicalLane == 0 {
				return
			}
			value := &nativePositiveAppliedPacket{offer: offer, provider: cut}
			w.stateLock.Lock()
			if w.first == nil || w.first.offer.wire.owner != offer.wire.owner || w.first.offer.wire.generation != offer.wire.generation {
				w.first = value
			}
			w.last = value
			if w.responseAckCount < 2 && (w.responseAckCount == 0 || int32(offer.header.ack-w.responseLastAck) > 0) {
				w.responseLastAck = offer.header.ack
				w.responseAckCount++
				w.responseAcks <- struct{}{}
			}
			w.stateLock.Unlock()
		},
	}
	w.f = newPersistNativeFixtureWithBridge(t, false, func(settings *ClientSettings, source bool) {
		if !source {
			return
		}
		defaults := DefaultClientSettings().SendBufferSettings
		settings.SendBufferSettings.SequenceBufferSize = 21
		settings.SendBufferSettings.LogicalDataLaneCount = 10
		settings.SendBufferSettings.ResendQueueMaxByteCount = defaults.ResendQueueMaxByteCount
		settings.SendBufferSettings.ResendQueueMinByteCount = defaults.ResendQueueMinByteCount
		settings.SendBufferSettings.DeliverySizedWindowCeilingByteCount = defaults.DeliverySizedWindowCeilingByteCount
		priorDisposition := settings.SendBufferSettings.afterGroupDispositionForTest
		settings.SendBufferSettings.afterGroupDispositionForTest = func(sequence *SendSequence, disposition sendGroupDisposition, frames []*protocol.Frame) {
			if priorDisposition != nil {
				priorDisposition(sequence, disposition, frames)
			}
			target := w.target.Load()
			if !w.manualControl || target == nil || len(frames) != 1 || frames[0].MessageType != protocol.MessageType_IpIpPacketToProvider {
				return
			}
			packet, err := ipPacketToProviderBytes(frames[0])
			if err != nil || !bytes.Equal(packet, *target) {
				return
			}
			group, ok := disposition.target.(*parsedPacketGroup)
			if !ok || group == nil || len(sequence.sendItems) == 0 || disposition.start != 0 || disposition.end != 1 {
				t.Error("exact native feedback lacks its actual singleton group disposition")
				return
			}
			item := sequence.sendItems[len(sequence.sendItems)-1]
			if item == nil || !item.expectsAck || !item.deliverImmediately || !item.noBudget || item.acks.count != 1 {
				t.Error("exact native feedback lost its reliable immediate retained owner")
				return
			}
			record := item.acks.records[0]
			if record.group != nil {
				record = record.group.ack
			}
			if record.target != group || record.lifecycle.token == 0 || record.lifecycle.clientId != sequence.client.ClientId() {
				t.Error("exact native feedback lost its original lifecycle identity")
				return
			}
			index := 0
			if attempt := group.nativeAckRetry; attempt != nil && attempt.replay {
				index = 1
				if attempt.source != sequence {
					t.Error("automatic feedback retry changed source owner")
					return
				}
			}
			w.stateLock.Lock()
			original := &w.targetOriginals[index]
			original.dispositions++
			if original.dispositions != 1 {
				t.Error("exact target produced a second original of the same provenance")
			} else {
				original.wire = persistNativeWire{owner: sequence.id(), generation: sequence.sequenceId,
					message: item.messageId, number: item.sequenceNumber, sequence: sequence}
				original.token = record.lifecycle.token
			}
			w.stateLock.Unlock()
		}
		priorWire := settings.SendBufferSettings.TransferWireMessageObserver
		settings.SendBufferSettings.TransferWireMessageObserver = func(event TransferWireMessageObservation) {
			priorWire(event)
			if event.Resend || w.f == nil || w.f.closing.Load() {
				return
			}
			wire, pressure := w.f.sourceWire.Load(), w.pressure.Load()
			if wire == nil || pressure == nil {
				return
			}
			if wire.header == pressure.offer.header {
				pack := decodeSendPackLifecycleWirePack(t, event.TransferFrameBytes)
				if pack.Nack || !pack.DeliverImmediately || !protocolOnlyPack(pack) || len(pack.Frames) != 1 {
					t.Error("positive ACK pressure lost reliable immediate protocol ownership")
					return
				}
				w.stateLock.Lock()
				if w.written == len(w.wires) {
					t.Error("pressure created more than six actual originals")
				} else {
					w.wires[w.written] = wire
					w.written++
				}
				w.stateLock.Unlock()
				w.notify()
			}
			if target := w.target.Load(); target != nil && wire.header.opens(*w.f.zero.Load()) {
				pack := decodeSendPackLifecycleWirePack(t, event.TransferFrameBytes)
				if len(pack.Frames) != 1 {
					t.Error("native feedback changed singleton shape")
					return
				}
				packet, err := ipPacketToProviderBytes(pack.Frames[0])
				if err == nil && bytes.Equal(packet, *target) {
					if w.manualControl {
						w.stateLock.Lock()
						matched := false
						for index := range w.targetOriginals {
							original := &w.targetOriginals[index]
							if original.dispositions != 0 && original.wire.owner == wire.owner && original.wire.generation == wire.generation &&
								original.wire.message == wire.message && original.wire.number == wire.number && original.wire.sequence == wire.sequence {
								original.writes++
								original.wire = *wire
								matched = true
							}
						}
						if !matched {
							t.Error("exact target wire lacks its captured manual or automatic original")
						}
						w.stateLock.Unlock()
					}
				}
			}
		}
		priorAck := settings.SendBufferSettings.afterAckSendItemForTest
		settings.SendBufferSettings.afterAckSendItemForTest = func(id sendSequenceId, number uint64) {
			w.stateLock.Lock()
			pressure := false
			for i, wire := range w.wires {
				if wire != nil && wire.owner == id && wire.number == number {
					pressure = true
					if w.retired[i] {
						t.Error("pressure original retired twice")
					}
					w.retired[i] = true
				}
			}
			w.stateLock.Unlock()
			if pressure {
				w.notify()
				return
			}
			manual := false
			if w.manualControl {
				w.stateLock.Lock()
				originals := w.targetOriginals
				w.stateLock.Unlock()
				for index, original := range originals {
					if original.dispositions != 0 && original.wire.owner == id && original.wire.number == number {
						buffer := original.wire.sequence.client.sendBuffer
						buffer.mutex.Lock()
						current := buffer.sendSequences[id] == original.wire.sequence && buffer.sendSequencesBySequenceId[original.wire.generation] == original.wire.sequence
						buffer.mutex.Unlock()
						w.stateLock.Lock()
						w.targetOriginals[index].receipts++
						receipts := w.targetOriginals[index].receipts
						w.stateLock.Unlock()
						if !current || original.writes != 1 || receipts != 1 {
							t.Error("exact target terminal lacks one actual original wire")
						}
						manual = index == 0
					}
				}
			}
			opened := false
			if w.f != nil {
				if wire := w.f.openWire.Load(); wire != nil && wire.owner == id && wire.number == number {
					// Capture applied state before the bridge can admit another
					// packet, but publish completion after its serial ACK handoff.
					cut := nativePositiveProviderSnapshot(w.f.providerSequence.Load())
					w.opened.CompareAndSwap(nil, &cut)
					opened = true
				}
			}
			priorAck(id, number)
			if manual {
				// The existing hook publishes the genuine serial receipt first.
				w.manualAckDo.Do(func() { close(w.manualAckSignal) })
			}
			if opened {
				w.openedDo.Do(func() { close(w.openedSignal) })
			}
		}
		settings.SendBufferSettings.SendPackLifecycleObserver = func(event SendPackLifecycleObservation) {
			if event.Token == 0 || event.Token >= uint64(len(w.lifetimes)) {
				t.Error("positive feedback lifetime bound exceeded")
				return
			}
			w.stateLock.Lock()
			entry := &w.lifetimes[event.Token]
			entry.token = event.Token
			switch event.Phase {
			case SendPackLifecyclePhaseStarted:
				entry.started++
				w.latestToken = event.Token
			case SendPackLifecyclePhaseFirstRouteWrite:
				entry.written++
				entry.writeErr = event.Err
			case SendPackLifecyclePhaseTerminal:
				entry.terminal++
				entry.terminalErr = event.Err
			}
			w.stateLock.Unlock()
			w.notify()
		}
		settings.SendBufferSettings.afterApplyAckSnapshotForTest = func(id sendSequenceId) {
			w.stateLock.Lock()
			wire := w.wires[0]
			w.stateLock.Unlock()
			if wire == nil || wire.owner != id {
				return
			}
			select {
			case reply := <-w.cutRequest:
				sequence := wire.sequence
				cut := nativeWindowCadenceCut{at: time.Now(), immediate: sequence.immediateSendCount, limit: sequence.immediateSendLimit,
					retained: sequence.resendQueue.Len(), dataBytes: sequence.sendBufferSettings.ResendQueueBudget.UsedByteCount()}
				sequence.protocolAdmission.mutex.Lock()
				cut.raw, cut.rawLimit = sequence.protocolAdmission.count, sequence.protocolAdmission.capacity
				sequence.protocolAdmission.mutex.Unlock()
				reply <- cut
			default:
			}
		}
	}, func(sequence *TcpSequence) {
		prior := sequence.receiveCallback
		sequence.receiveCallback = func(source TransferPath, key TransferKey, mode protocol.ProvideMode, recovery receiveRecoveryMode, path *IpPath, packet []byte) {
			if w.f != nil && w.pressure.Load() != nil {
				header, _, err := persistNativePacketHeader(packet)
				zero := w.f.zero.Load()
				cut := nativePositiveProviderSnapshot(sequence)
				if err == nil && zero != nil && recovery == receiveRecoveryModeTcpSocket && cut.attempts > 0 &&
					header.payload > 0 && header.source == zero.destination && header.destination == zero.source &&
					header.sourcePort == zero.destinationPort && header.destinationPort == zero.sourcePort &&
					header.ack == zero.seq && int32(header.seq-zero.ack) < 0 && int32(header.seq-cut.ack) >= 0 {
					w.recoveryDo.Do(func() {
						w.recovery.Store(&nativePositiveRecovery{provider: cut, header: header, mode: recovery})
						close(w.recoverySignal)
					})
				}
			}
			prior(source, key, mode, recovery, path, packet)
		}
	}, hooks)
	t.Cleanup(func() { w.allowZeroDo.Do(func() { close(w.allowZero) }); w.releaseClaims() })
	w.f.receiver.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, peer Peer) {
		pressure := w.pressure.Load()
		if pressure == nil || w.f.closing.Load() {
			return
		}
		for _, frame := range frames {
			if frame.MessageType != protocol.MessageType_IpIpPacketToProvider {
				continue
			}
			packet, err := ipPacketToProviderBytes(frame)
			if err != nil {
				continue
			}
			header, _, err := persistNativePacketHeader(packet)
			if err != nil || header != pressure.offer.header {
				continue
			}
			w.stateLock.Lock()
			if w.holdClaims && w.claimCount < len(w.claims) {
				claim := peer.deliveryReceipt().hold()
				if claim == nil {
					t.Error("positive pressure lacks actual delivery custody")
				} else {
					w.claims[w.claimCount] = claim
					w.claimCount++
				}
			}
			w.stateLock.Unlock()
			w.notify()
		}
	})
	return w
}

func (self *nativePositiveWitness) releaseClaims() {
	self.stateLock.Lock()
	claims := self.claims
	self.claims = [3]*receiveDeliveryClaim{}
	self.holdClaims = false
	self.stateLock.Unlock()
	for _, claim := range claims {
		if claim != nil {
			claim.complete(true)
		}
	}
}

func (self *nativePositiveWitness) cut() nativeWindowCadenceCut {
	self.stateLock.Lock()
	wire := self.wires[0]
	self.stateLock.Unlock()
	reply := make(chan nativeWindowCadenceCut, 1)
	self.cutRequest <- reply
	wire.sequence.ackWindow.Wake()
	select {
	case cut := <-reply:
		return cut
	case <-self.f.ctx.Done():
		self.t.Fatal("positive feedback source cut", self.f.ctx.Err())
		return nativeWindowCadenceCut{}
	}
}

// The desired outcome is progress from the genuine refused advancing ACK,
// before ordinary retained-DATA recovery. A pass falsifies this candidate gap.
func TestTcpNativeAdvancingFeedbackProgressesAfterFiniteAckService(t *testing.T) {
	checkNativePositiveFeedback(t, false)
}

// The sole intervention is the exact same native packet after source owners
// drain; no provider field, TCP timer or acknowledgement is manufactured.
func TestTcpNativeAdvancingFeedbackExactPacketControl(t *testing.T) {
	checkNativePositiveFeedback(t, true)
}

// Cancellation between a native read and its offer leaves no flow to inspect.
// The fixture must return the still-owned packet instead of panicking in cleanup.
func TestTcpNativeFeedbackOfferAfterParentCloseReturnsBuffer(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	assertMessagePoolOwnership(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	f := &persistNativeFixture{t: t, parent: &RemoteUserNatMultiClient{ctx: ctx}}
	packet := MessagePoolCopy(reraceTcpPacket(reraceFlow(4, 0, 51543), tcpFlagAck, 100, 500, nil))
	result := f.offer(packet, true)
	if !result.header.pureAck() || result.accepted || result.count != 0 || f.nativeOffers.Load() != 1 {
		t.Fatal("closed parent retained or accepted native feedback")
	}
}

func checkNativePositiveFeedback(t *testing.T, manualReoffer bool) {
	t.Helper()
	MessagePoolReturn(MessagePoolGet(1))
	assertMessagePoolOwnership(t)
	penaltyAfterJoin := false
	t.Cleanup(func() {
		if penaltyAfterJoin {
			t.Log("NATIVE_ADVANCING_FEEDBACK_OWNERS_JOINED: pressure_originals=6 actual_receipts=6 http_exact=true reused=true")
			t.Error("NATIVE_ADVANCING_FEEDBACK_LOST_BEFORE_TRANSFER: finite receipt service drained accepted owners but exhausted positive TCP grant waited for retained DATA recovery before the genuine advancing feedback could progress")
		}
	})
	w := newNativePositiveWitness(t, manualReoffer)
	f := w.f
	connection, err := f.tun.DialContext(f.ctx, "tcp4", "198.51.100.19:80")
	if err != nil {
		t.Fatal(err)
	}
	f.connection = connection
	deadline, _ := f.ctx.Deadline()
	if err := connection.SetDeadline(deadline); err != nil {
		t.Fatal(err)
	}
	request := []byte("GET /asset HTTP/1.1\r\nHost: native-persist.example\r\n\r\n")
	if n, err := connection.Write(request); err != nil || n != len(request) {
		t.Fatalf("real request: n=%d err=%v", n, err)
	}
	var zero persistNativeHeader
	select {
	case zero = <-w.zeroReady:
	case <-f.ctx.Done():
		t.Fatal("pre-zero native boundary", f.ctx.Err())
	}
	w.stateLock.Lock()
	first, last := w.first, w.last
	w.stateLock.Unlock()
	if first == nil || last == nil || first.offer.header.seq != zero.seq || last.offer.header.seq != zero.seq ||
		int32(last.offer.header.ack-first.offer.header.ack) <= 0 || int32(zero.ack-last.offer.header.ack) <= 0 ||
		first.offer.header.window == 0 || last.offer.header.window == 0 || first.offer.wire.owner != last.offer.wire.owner ||
		first.offer.wire.generation != last.offer.wire.generation {
		t.Fatalf("missing genuine earlier/later applied positive ACK pair: first=%+v last=%+v zero=%+v", first, last, zero)
	}
	before := nativePositiveProviderSnapshot(f.providerSequence.Load())
	if before.ack != last.offer.header.ack || before.edge != before.next || before.next != zero.ack ||
		int32(before.next-before.ack) <= 0 || before.window != before.next-before.ack || before.retained == 0 || before.chunks == 0 || before.attempts != 0 {
		t.Fatalf("positive grant not exhausted with actual retained DATA: %+v", before)
	}
	// Require the actual reader's Cond.Wait, not a sample of a runnable owner.
	var waited zeroWindowWaitSnapshot
	for waited.ack != before.ack || waited.next != before.next || waited.retained == 0 || waited.window != before.window {
		select {
		case waited = <-f.waits:
		case <-f.ctx.Done():
			t.Fatal("positive-edge reader did not park", f.ctx.Err())
		}
	}
	if f.waitLocker.Load().lost.Load() != 0 {
		t.Fatal("provider wait observations overflowed")
	}
	admission := nativePositiveAdmissionSnapshot(f, last.offer.copy)
	if !admission.seen || admission.window != last.offer.header.window || admission.ack != last.offer.header.ack || admission.position != zero.seq || admission.client != f.selected {
		t.Fatalf("native admitted positive history not pinned: %+v", admission)
	}
	w.pressure.Store(first)
	owner := first.offer.wire.owner
	key := TransferKey{ForceStream: owner.ForceStream, CompanionContract: owner.CompanionContract,
		EncryptionRole: owner.EncryptionRole.toProtobuf(), EncryptionCompanion: owner.EncryptionCompanion, LogicalLane: owner.LogicalLane}
	for i := range 6 {
		frame := &protocol.Frame{MessageType: protocol.MessageType_IpIpPacketToProvider, Raw: true, MessageBytes: MessagePoolCopy(first.offer.copy)}
		index := i
		accepted, sendErr := f.sender.sendGroupWithTimeoutDetailed([]*protocol.Frame{frame}, f.receiver.ClientId(), func(err error) {
			if err != nil && !f.closing.Load() {
				t.Error("pressure callback", err)
			}
			w.stateLock.Lock()
			w.callbacks[index]++
			w.stateLock.Unlock()
			w.notify()
		}, 0, key, NoBudget(), DeliverImmediately())
		if !accepted || sendErr != nil {
			MessagePoolReturn(frame.MessageBytes)
			t.Fatalf("actual source pressure owner%d refused: %v", i, sendErr)
		}
		if i < 3 {
			w.wait("three actual pressure delivery claims", func() bool { return w.claimCount >= index+1 && w.written >= index+1 })
		}
	}
	full := w.cut()
	if full.immediate != 3 || full.limit != 3 || full.retained != 3 || full.raw != 3 || full.rawLimit != 3 || full.dataBytes != 0 || owner.LogicalLane == 0 || f.sender.settings.SendBufferSettings.ApplicationPacing {
		t.Fatalf("actual independent immediate3/raw3 protocol owners missing: %+v", full)
	}
	if after := nativePositiveAdmissionSnapshot(f, last.offer.copy); after != admission {
		t.Fatalf("source-level pressure mutated native admission history: before=%+v after=%+v", admission, after)
	}
	afterPressure := nativePositiveProviderSnapshot(f.providerSequence.Load())
	if afterPressure.ack != before.ack || afterPressure.next != before.next || afterPressure.edge != before.edge || afterPressure.window != before.window || afterPressure.retained != before.retained || afterPressure.chunks != before.chunks || afterPressure.attempts != 0 || afterPressure.duplicateAcks != before.duplicateAcks {
		t.Fatalf("older pressure ACK changed applied provider frontier: before=%+v after=%+v", before, afterPressure)
	}
	w.allowZeroDo.Do(func() { close(w.allowZero) })
	var zeroOffer persistNativeOffer
	select {
	case zeroOffer = <-f.zeroOffered:
	case <-f.ctx.Done():
		t.Fatal("actual zero offer", f.ctx.Err())
	}
	if zeroOffer.accepted || zeroOffer.count != 0 || zeroOffer.header != zero || zeroOffer.collapseDrops != 0 || !zeroOffer.beforeEligible || !zeroOffer.afterEligible {
		t.Fatalf("actual advancing zero ACK not refused at full source custody: %+v", zeroOffer)
	}
	synAck := f.synAck.Load()
	prefixCount := int(uint32(zero.ack - (synAck.seq + 1)))
	if prefixCount < 1024 || prefixCount > 16*1024 {
		t.Fatalf("actual native prefix %d", prefixCount)
	}
	prefix := make([]byte, prefixCount)
	if _, err := io.ReadFull(connection, prefix); err != nil {
		t.Fatal(err)
	}
	var target persistNativeOffer
	var refusals [64]nativeWindowCadenceLifecycle
	refusalCount := 0
	for n := 0; ; n++ {
		if n == 64 {
			t.Fatal("native feedback queue exceeded fixed bound")
		}
		packet := f.tun.ep.Read()
		if packet == nil {
			break
		}
		copyPacket := messagePoolCopyPacketBuffer(packet)
		packet.DecRef()
		offer := f.offer(copyPacket, true)
		if !offer.header.opens(zero) {
			if offer.header.pureAck() && offer.header.window == 0 && offer.header.ack == zero.ack && !offer.accepted {
				continue
			}
			t.Fatalf("unexpected real feedback: %+v", offer)
		}
		if offer.accepted || offer.count != 0 || offer.collapseDrops != 0 || !offer.beforeEligible || !offer.afterEligible ||
			offer.beforeWindow != admission.window || offer.afterWindow != admission.window || offer.beforeAck != admission.ack || offer.afterAck != admission.ack {
			t.Fatalf("advancing native feedback changed genuine admitted baseline: %+v", offer)
		}
		w.stateLock.Lock()
		life := w.lifetimes[w.latestToken]
		w.stateLock.Unlock()
		var admissionErr *SendPackAdmissionError
		if life.started != 1 || life.written != 1 || life.terminal != 1 || !errors.As(life.terminalErr, &admissionErr) ||
			admissionErr.Boundary != "pack-admission" || admissionErr.Timeout != 0 || admissionErr.RecoveredByOwner || admissionErr.OwnerTrackingOverflow {
			t.Fatalf("actual advancing ACK lacks exact unowned raw refusal: %+v", life)
		}
		refusals[refusalCount] = life
		refusalCount++
		target = offer
	}
	if refusalCount == 0 || f.openWire.Load() != nil || int32(target.header.ack-admission.ack) <= 0 {
		t.Fatal("no genuine refused advancing positive feedback")
	}
	expected := target.copy
	w.target.Store(&expected)
	refusedAt := time.Now()
	service := time.NewTimer(40 * time.Millisecond)
	select {
	case <-service.C:
	case <-f.ctx.Done():
		service.Stop()
		t.Fatal(f.ctx.Err())
	}
	w.releaseClaims()
	w.wait("all six actual source owners and receipts drain", func() bool {
		if w.written != 6 {
			return false
		}
		for i := range 6 {
			if !w.retired[i] || w.callbacks[i] != 1 {
				return false
			}
		}
		return true
	})
	drained := w.cut()
	if drained.dataBytes != 0 {
		t.Fatal("protocol pressure charged DATA bytes")
	}
	if manualReoffer {
		offer := f.offer(MessagePoolCopy(target.copy), false)
		if !offer.accepted || offer.count != 1 || offer.header != target.header {
			t.Fatalf("identical native feedback did not admit after finite drain: %+v", offer)
		}
		f.await(w.manualAckSignal, "manual original applied and serial terminal handoff complete")
		// The automatic retry may have completed first. Only its separately
		// identified original may precede the manual receipt in these two slots.
		manualReceipt := false
		var consumed [2]bool
		for count := 0; count < len(consumed) && !manualReceipt; count++ {
			select {
			case receipt := <-f.serialAcks:
				w.stateLock.Lock()
				originals := w.targetOriginals
				w.stateLock.Unlock()
				matched := -1
				for index, original := range originals {
					if original.dispositions == 1 && original.writes == 1 && original.receipts == 1 && receipt == original.wire {
						matched = index
					}
				}
				if matched < 0 || consumed[matched] {
					t.Fatal("manual feedback received an unknown or repeated original terminal")
				}
				consumed[matched] = true
				manualReceipt = matched == 0
			case <-f.ctx.Done():
				t.Fatal("exact manual target terminal receipt", f.ctx.Err())
			}
		}
		if !manualReceipt {
			t.Fatal("bounded target receipt set omitted the manual original")
		}
	}
	f.resumeDo.Do(func() { close(f.resumeBridge) })
	select {
	case <-w.openedSignal:
	case <-w.recoverySignal:
	case <-f.ctx.Done():
		t.Fatal("neither actual advancing feedback nor retained DATA recovery progressed", f.ctx.Err())
	}
	opened, recovery := w.opened.Load(), w.recovery.Load()
	penalty := recovery != nil && (opened == nil || !opened.at.Before(recovery.provider.at))
	if penalty {
		cut := recovery.provider
		if !cut.at.After(drained.at) || cut.retained == 0 || cut.chunks == 0 || cut.ack != before.ack || cut.edge != before.edge ||
			cut.next != before.next || cut.window != before.window || cut.attempts == 0 || recovery.mode != receiveRecoveryModeTcpSocket {
			t.Fatalf("recovery penalty lacks actual unchanged exhausted positive grant: %+v", recovery)
		}
		t.Logf("NATIVE_ADVANCING_FEEDBACK_RECOVERY: prior_ack=%d target_ack=%d applied_edge=%d next=%d retained=%d chunks=%d raw_window=%d scale=%d service_ns=%d drained_after_refusal_ns=%d recovery_after_drain_ns=%d pressure_native_history_unchanged=true", before.ack, target.header.ack, before.edge, before.next, before.retained, before.chunks, admission.window, before.scale, (40 * time.Millisecond).Nanoseconds(), drained.at.Sub(refusedAt).Nanoseconds(), cut.at.Sub(drained.at).Nanoseconds())
	}
	reader := bufio.NewReader(io.MultiReader(bytes.NewReader(prefix), connection))
	readResponse := func(expected []byte) {
		t.Helper()
		response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodGet})
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		closeErr := response.Body.Close()
		if err != nil || closeErr != nil || response.StatusCode != http.StatusOK || response.ContentLength != int64(len(expected)) || !bytes.Equal(body, expected) {
			t.Fatalf("exact HTTP response: status=%d bytes=%d read=%v close=%v", response.StatusCode, len(body), err, closeErr)
		}
	}
	readResponse(f.body)
	reuse := []byte("GET /reuse HTTP/1.1\r\nHost: native-persist.example\r\n\r\n")
	if n, err := connection.Write(reuse); err != nil || n != len(reuse) {
		t.Fatalf("actual same-flow reuse: n=%d err=%v", n, err)
	}
	readResponse([]byte("reuse"))
	if f.dials.Load() != 1 || f.tun.LinkStats().OutboundDropCount != 0 || f.ctx.Err() != nil {
		t.Fatal("feedback proof reconnected, dropped native traffic or canceled")
	}
	if manualReoffer && penalty {
		t.Fatal("exact feedback control still waited for TCP DATA recovery")
	}
	if !penalty {
		opened = w.opened.Load()
		if opened == nil || int32(opened.ack-target.header.ack) < 0 || opened.window == 0 {
			t.Fatal("progress lacks actual advancing feedback and terminal receipt")
		}
	}
	w.stateLock.Lock()
	if manualReoffer {
		for index, original := range w.targetOriginals {
			if index == 1 && original.dispositions == 0 {
				continue
			}
			if original.dispositions != 1 || original.writes != 1 || original.receipts != 1 || original.token == 0 || original.token >= uint64(len(w.lifetimes)) ||
				original.wire.header != target.header || original.wire.owner != owner || original.wire.generation != first.offer.wire.generation {
				t.Errorf("target original%d lacks exact disposition, wire and receipt: %+v", index, original)
				continue
			}
			life := w.lifetimes[original.token]
			if life.started != 1 || life.written != 1 || life.terminal != 1 || life.writeErr != nil || life.terminalErr != nil {
				t.Errorf("target original%d lacks one successful lifecycle: %+v", index, life)
			}
		}
	}
	for _, prior := range refusals[:refusalCount] {
		after := w.lifetimes[prior.token]
		if after != prior {
			t.Error("later recovery changed original false lifetime")
		}
	}
	for i, wire := range w.wires {
		if wire == nil || wire.owner != owner || wire.generation != first.offer.wire.generation || !w.retired[i] || w.callbacks[i] != 1 {
			t.Error("pressure source/generation/terminal identity changed")
		}
	}
	w.stateLock.Unlock()
	t.Logf("NATIVE_ADVANCING_FEEDBACK_COMPLETE: manual_exact_packet=%t retained_DATA_recovery=%t body=%d reused=true", manualReoffer, penalty, len(f.body))
	penaltyAfterJoin = !manualReoffer && penalty
}
