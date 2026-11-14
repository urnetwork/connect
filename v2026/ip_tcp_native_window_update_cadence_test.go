// Actual native window feedback can be refused before reliable Transfer
// custody, leaving otherwise drained transport owners to wait for TCP persist.
package connect

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

// Fixed observer storage records scalar identities, never borrowed payloads.
type nativeWindowCadenceLifecycle struct {
	token                      uint64
	started, written, terminal int
	writeErr, terminalErr      error
}

type nativeWindowCadenceProbe struct {
	at       time.Time
	provider zeroWindowWaitSnapshot
}

type nativeWindowCadenceCut struct {
	at                                        time.Time
	immediate, limit, retained, raw, rawLimit int
	dataBytes                                 ByteCount
}

// All SendSequence scalars in a cut are copied by that sequence's Run owner.
// Queue/admission counts retain their existing locks; no ACK is fabricated.
type nativeWindowCadenceWitness struct {
	t                            *testing.T
	f                            *persistNativeFixture
	stateLock                    sync.Mutex
	lifetimes                    [256]nativeWindowCadenceLifecycle
	latestToken                  uint64
	zeroWires                    [6]*persistNativeWire
	zeroRetired                  [6]bool
	zeroWireCount                int
	extraClaims                  [2]*receiveDeliveryClaim
	extraClaimCount              int
	changed                      chan struct{}
	cutRequest                   chan chan nativeWindowCadenceCut
	opened                       chan struct{}
	openAcked                    chan struct{}
	openDo                       sync.Once
	openAckDo                    sync.Once
	pauseFourth                  atomic.Bool
	fourthWritten                atomic.Bool
	fourthEntered, releaseFourth chan struct{}
	fourthDo, releaseFourthDo    sync.Once
	probe                        atomic.Pointer[nativeWindowCadenceProbe]
	openedAt                     atomic.Pointer[time.Time]
	openedState                  atomic.Pointer[zeroWindowWaitSnapshot]
	expectedOpen                 atomic.Pointer[[]byte]
	exactOpenWrites              atomic.Int32
	targetResends                atomic.Int32
	openAdmitted                 chan struct{}
	openAdmitDo                  sync.Once
	pauseRetry                   atomic.Bool
	retryGroup                   atomic.Pointer[parsedPacketGroup]
	retryEntered, releaseRetry   chan struct{}
	retryDo, releaseRetryDo      sync.Once
	supersedingWire              atomic.Pointer[persistNativeWire]
}

func (self *nativeWindowCadenceWitness) notify() {
	select {
	case self.changed <- struct{}{}:
	default:
	}
}

// The unchanged native fixture supplies the real TCP handshake, bytes, window
// updates, provider socket, Multi admission, route writes and peer receipts.
func newNativeWindowCadenceWitness(t *testing.T) *nativeWindowCadenceWitness {
	t.Helper()
	w := &nativeWindowCadenceWitness{openAdmitted: make(chan struct{}), retryEntered: make(chan struct{}), releaseRetry: make(chan struct{}), t: t, changed: make(chan struct{}, 1), cutRequest: make(chan chan nativeWindowCadenceCut, 1),
		opened: make(chan struct{}), openAcked: make(chan struct{}), fourthEntered: make(chan struct{}), releaseFourth: make(chan struct{})}
	w.f = newPersistNativeFixtureConfigured(t, true, func(settings *ClientSettings, source bool) {
		if !source {
			return
		}
		// Exact mobile32 count projection: 21 raw owners and configured10
		// lanes negotiate the existing maximum8, yielding ceil(21/8)=3.
		// This component does not claim a whole SDK constructor projection.
		defaults := DefaultClientSettings().SendBufferSettings
		settings.SendBufferSettings.SequenceBufferSize = 21
		settings.SendBufferSettings.LogicalDataLaneCount = 10
		settings.SendBufferSettings.ResendQueueMaxByteCount = defaults.ResendQueueMaxByteCount
		settings.SendBufferSettings.ResendQueueMinByteCount = defaults.ResendQueueMinByteCount
		settings.SendBufferSettings.DeliverySizedWindowCeilingByteCount = defaults.DeliverySizedWindowCeilingByteCount
		previousAdmission := settings.SendBufferSettings.afterGroupAdmissionForTest
		settings.SendBufferSettings.afterGroupAdmissionForTest = func(target sendGroupAdmissionTarget) {
			if previousAdmission != nil {
				previousAdmission(target)
			}
			group, ok := target.(*parsedPacketGroup)
			if !ok || w.f == nil || len(group.packets) != 1 {
				return
			}
			zero := w.f.zero.Load()
			path := group.ipPath
			if zero != nil && path.Ack && !path.Syn && !path.Fin && !path.Rst && path.TcpPayloadByteCount == 0 &&
				path.SequenceNumber == zero.seq && path.AckSequenceNumber == zero.ack && path.TcpWindowSize != 0 {
				if group.nativeAckRetry != nil && group.nativeAckRetry.replay {
					w.retryGroup.Store(group)
				}
				w.openAdmitDo.Do(func() { close(w.openAdmitted) })
			}
		}
		previousRetain := settings.SendBufferSettings.beforeGroupRetainForTest
		settings.SendBufferSettings.beforeGroupRetainForTest = func() {
			if previousRetain != nil {
				previousRetain()
			}
			w.stateLock.Lock()
			pressureWritten := w.zeroWireCount == len(w.zeroWires)
			w.stateLock.Unlock()
			if pressureWritten && w.retryGroup.Load() != nil && w.pauseRetry.CompareAndSwap(true, false) {
				// The source has passed its entry check and contract work.
				// Its next step after this existing hook is materialization.
				w.retryDo.Do(func() { close(w.retryEntered) })
				select {
				case <-w.releaseRetry:
				case <-w.f.ctx.Done():
				}
			}
		}
		previousWire := settings.SendBufferSettings.TransferWireMessageObserver
		settings.SendBufferSettings.TransferWireMessageObserver = func(event TransferWireMessageObservation) {
			previousWire(event)
			if w.f == nil || w.f.closing.Load() {
				return
			}
			if event.Resend {
				if first := w.f.zeroWire.Load(); first != nil {
					pack := decodeSendPackLifecycleWirePack(t, event.TransferFrameBytes)
					if bytes.Equal(pack.SequenceId, first.generation.Bytes()) && pack.LogicalLane == first.owner.LogicalLane {
						w.targetResends.Add(1)
					}
				}
				return
			}
			wire := w.f.sourceWire.Load()
			zero := w.f.zero.Load()
			if wire == nil || zero == nil {
				return
			}
			if wire.header.opens(*zero) {
				if expected := w.expectedOpen.Load(); expected != nil {
					pack := decodeSendPackLifecycleWirePack(t, event.TransferFrameBytes)
					if len(pack.Frames) != 1 {
						t.Error("reopening retry changed its exact native singleton")
						return
					}
					packet, err := ipPacketToProviderBytes(pack.Frames[0])
					if err != nil || !bytes.Equal(packet, *expected) {
						t.Error("reopening retry changed native TCP options or packet bytes")
						return
					}
					w.exactOpenWrites.Add(1)
				}
			}
			if wire.header != *zero {
				return
			}
			pack := decodeSendPackLifecycleWirePack(t, event.TransferFrameBytes)
			if !pack.DeliverImmediately || pack.Nack || len(pack.Frames) != 1 || !protocolOnlyPack(pack) {
				t.Error("zero-window native original changed immediate/reliable/protocol policy")
				return
			}
			w.stateLock.Lock()
			for _, prior := range w.zeroWires {
				if prior != nil && prior.message == wire.message {
					w.stateLock.Unlock()
					return
				}
			}
			if w.zeroWireCount == len(w.zeroWires) {
				if w.retryGroup.Load() != nil {
					w.supersedingWire.CompareAndSwap(nil, wire)
				} else {
					t.Error("zero-window wire census exceeded six actual originals")
				}
			} else {
				w.zeroWires[w.zeroWireCount] = wire
				w.zeroWireCount++
			}
			w.stateLock.Unlock()
			w.notify()
		}
		previousAck := settings.SendBufferSettings.afterAckSendItemForTest
		settings.SendBufferSettings.afterAckSendItemForTest = func(id sendSequenceId, number uint64) {
			w.stateLock.Lock()
			zero := false
			for i, wire := range w.zeroWires {
				if wire != nil && wire.owner == id && wire.number == number {
					zero = true
					if w.zeroRetired[i] {
						t.Error("zero-window original retired twice")
					}
					w.zeroRetired[i] = true
				}
			}
			w.stateLock.Unlock()
			if zero {
				w.notify()
				return
			}
			opened := false
			if w.f != nil {
				if wire := w.f.openWire.Load(); wire != nil && wire.owner == id && wire.number == number {
					// This established pure ACK's real terminal receipt follows
					// NAT enterTcp -> applyEstablishedPureAck -> operation.complete.
					// ClientReceive itself only queues that asynchronous work.
					// Snapshot before previousAck wakes the native reader, which
					// could otherwise send a later ACK and change this frontier.
					cut := nativeWindowCadenceProviderCut(w.f.providerSequence.Load())
					w.openDo.Do(func() {
						w.openedState.Store(&cut)
						at := cut.at
						w.openedAt.Store(&at)
						close(w.opened)
					})
					opened = true
				}
			}
			previousAck(id, number)
			// The native bridge's serial receipt was published by previousAck.
			// Join this exact replay before resuming another native offer.
			if opened {
				w.openAckDo.Do(func() { close(w.openAcked) })
			}
		}
		settings.SendBufferSettings.SendPackLifecycleObserver = func(event SendPackLifecycleObservation) {
			if event.Token == 0 || event.Token >= uint64(len(w.lifetimes)) {
				t.Error("native lifecycle census exceeded its fixed bound")
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
			if w.f == nil {
				return
			}
			wire := w.f.zeroWire.Load()
			if wire == nil || id != wire.owner {
				return
			}
			if w.pauseFourth.Load() && w.fourthWritten.Load() {
				w.fourthDo.Do(func() {
					close(w.fourthEntered)
					select {
					case <-w.releaseFourth:
					case <-w.f.ctx.Done():
					}
				})
			}
			select {
			case reply := <-w.cutRequest:
				sequence := wire.sequence
				cut := nativeWindowCadenceCut{at: time.Now(), immediate: sequence.immediateSendCount,
					limit: sequence.immediateSendLimit, retained: sequence.resendQueue.Len(),
					dataBytes: sequence.sendBufferSettings.ResendQueueBudget.UsedByteCount()}
				sequence.protocolAdmission.mutex.Lock()
				cut.raw, cut.rawLimit = sequence.protocolAdmission.count, sequence.protocolAdmission.capacity
				sequence.protocolAdmission.mutex.Unlock()
				reply <- cut
			default:
			}
		}
		settings.SendBufferSettings.afterInitialWriteQueuedForTest = func(id sendSequenceId, number uint64) {
			if w.f == nil || !w.pauseFourth.Load() {
				return
			}
			wire := w.f.zeroWire.Load()
			if wire != nil && id == wire.owner && number == wire.number+3 {
				// Logical-group raw custody releases after this write hook.
				// The next owner-loop boundary is the first safe one-slot cut.
				w.fourthWritten.Store(true)
			}
		}
	}, func(sequence *TcpSequence) {
		prior := sequence.receiveCallback
		sequence.receiveCallback = func(source TransferPath, key TransferKey, mode protocol.ProvideMode, recovery receiveRecoveryMode, path *IpPath, packet []byte) {
			if recovery == receiveRecoveryModeDedicatedTcpControl && w.f != nil {
				header, _, err := persistNativePacketHeader(packet)
				zero := w.f.zero.Load()
				if err == nil && zero != nil && header.source == zero.destination && header.destination == zero.source &&
					header.sourcePort == zero.destinationPort && header.destinationPort == zero.sourcePort &&
					header.payload == 1 && header.seq == zero.ack-1 && header.ack == zero.seq && header.ackFlag && !header.syn && !header.fin && !header.rst {
					w.probe.CompareAndSwap(nil, &nativeWindowCadenceProbe{at: time.Now(), provider: nativeWindowCadenceProviderCut(sequence)})
				}
			}
			prior(source, key, mode, recovery, path, packet)
		}
	})
	// This runs before the native fixture cleanup, releasing every explicit
	// receipt/barrier even when a setup assertion aborts the root.
	t.Cleanup(func() {
		w.releaseFourthDo.Do(func() { close(w.releaseFourth) })
		w.releaseRetryDo.Do(func() { close(w.releaseRetry) })
		w.releaseExtraClaims()
	})
	w.f.receiver.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, peer Peer) {
		zero, first := w.f.zero.Load(), w.f.zeroWire.Load()
		if zero == nil || first == nil || w.f.closing.Load() {
			return
		}
		receipt := peer.deliveryReceipt()
		if receipt == nil || receipt.ack.messageId == first.message {
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
			if err != nil {
				continue
			}
			if header != *zero {
				continue
			}
			w.stateLock.Lock()
			if w.extraClaimCount < len(w.extraClaims) {
				claim := receipt.hold()
				if claim == nil {
					t.Error("real extra zero-window original lacks delivery custody")
				} else {
					w.extraClaims[w.extraClaimCount] = claim
					w.extraClaimCount++
				}
			}
			w.stateLock.Unlock()
			w.notify()
		}
	})
	return w
}

func (self *nativeWindowCadenceWitness) releaseExtraClaims() {
	self.stateLock.Lock()
	claims := self.extraClaims
	self.extraClaims = [2]*receiveDeliveryClaim{}
	self.stateLock.Unlock()
	for _, claim := range claims {
		if claim != nil {
			claim.complete(true)
		}
	}
}

// The wake requests only an owner diagnostic cut; it supplies no ACK authority.
func (self *nativeWindowCadenceWitness) cut() nativeWindowCadenceCut {
	self.t.Helper()
	reply := make(chan nativeWindowCadenceCut, 1)
	self.cutRequest <- reply
	self.f.zeroWire.Load().sequence.ackWindow.Wake()
	select {
	case cut := <-reply:
		return cut
	case <-self.f.ctx.Done():
		self.t.Fatal("native owner cut", self.f.ctx.Err())
		return nativeWindowCadenceCut{}
	}
}

func (self *nativeWindowCadenceWitness) latest() uint64 {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.latestToken
}

func (self *nativeWindowCadenceWitness) waitFor(label string, predicate func() bool) {
	self.t.Helper()
	for {
		self.stateLock.Lock()
		ready := predicate()
		self.stateLock.Unlock()
		if ready {
			return
		}
		select {
		case <-self.changed:
		case <-self.f.ctx.Done():
			self.t.Fatal(label, self.f.ctx.Err())
		}
	}
}

// Actual provider state is copied under its TCP mutex, after ordinary ACK
// application. An empty return queue is independent of unread socket data.
func nativeWindowCadenceProviderCut(sequence *TcpSequence) zeroWindowWaitSnapshot {
	sequence.mutex.Lock()
	defer sequence.mutex.Unlock()
	return zeroWindowWaitSnapshot{at: time.Now(), window: sequence.receiveWindowSize,
		ack: sequence.receiveSeqAck, next: sequence.receiveSeq,
		retained: sequence.returnByteCount, chunks: len(sequence.returnChunks) - sequence.returnHead}
}

// This desired-progress root is expected to fail current production only
// after natural persist, complete HTTP recovery and all cleanup. It does not
// assert that a false native offer acquired a retry owner.
func TestTcpNativeWindowUpdateProgressesAfterFiniteAckService(t *testing.T) {
	checkNativeWindowUpdateCadence(t, false, false)
}

// The retained identical native update is admitted automatically when one
// raw slot becomes free; older zero-window originals must not close it again.
func TestTcpNativeWindowUpdateSamePacketAfterOneSlotResumesHttp(t *testing.T) {
	checkNativeWindowUpdateCadence(t, true, false)
}

// New native feedback arrives after Client admission and inside preparation.
// Its stale retry must consume no ordinal before the next real exact update.
func TestTcpNativeWindowRetrySupersededBeforeSerializationLeavesNoHole(t *testing.T) {
	checkNativeWindowUpdateCadence(t, true, true)
}

func checkNativeWindowUpdateCadence(t *testing.T, replayAfterOneSlot, supersedeBeforeMaterialization bool) {
	t.Helper()
	MessagePoolReturn(MessagePoolGet(1))
	assertMessagePoolOwnership(t)
	penaltyAfterJoin := false
	// Registered before construction: the native fixture and extra claims
	// join first. The desired-progress error is emitted only afterward.
	t.Cleanup(func() {
		if penaltyAfterJoin {
			t.Log("NATIVE_WINDOW_UPDATE_OWNERS_JOINED: native_bridge=true provider=true clients=true all_accepted_wire_receipts=true")
			t.Error("NATIVE_WINDOW_UPDATE_LOST_BEFORE_TRANSFER: finite 40ms receipt service drained every accepted original, but the real open-window update had no retry owner and the same HTTP flow waited for ordinary TCP persist")
		}
	})
	w := newNativeWindowCadenceWitness(t)
	w.pauseRetry.Store(supersedeBeforeMaterialization)
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
		t.Fatalf("native HTTP write: n=%d err=%v", n, err)
	}
	var zeroOffer persistNativeOffer
	select {
	case zeroOffer = <-f.zeroOffered:
	case <-f.ctx.Done():
		t.Fatal("native zero-window offer", f.ctx.Err())
	}
	if !zeroOffer.accepted || zeroOffer.count != 1 || !zeroOffer.header.pureAck() || zeroOffer.header.window != 0 {
		t.Fatalf("real zero-window offer: %+v", zeroOffer)
	}
	var tokens [6]uint64
	tokens[0] = w.latest()
	f.await(f.claimReady, "first real zero-window delivery claim")
	var parked zeroWindowWaitSnapshot
	for parked.at.IsZero() || parked.window != 0 || parked.ack != zeroOffer.header.ack || parked.next != parked.ack || parked.retained != 0 || parked.chunks != 0 {
		select {
		case parked = <-f.waits:
		case <-f.ctx.Done():
			t.Fatal("fully ACKed zero-window provider wait", f.ctx.Err())
		}
	}
	zero := zeroOffer.header
	wire := f.zeroWire.Load()
	if wire == nil || wire.owner.LogicalLane == 0 || wire.owner.LogicalLane > maxLogicalDataLaneCount ||
		wire.sequence.immediateSendLimit != 3 || f.sender.settings.SendBufferSettings.ApplicationPacing {
		t.Fatal("actual native flow did not negotiate the unchanged mobile32 count3 lane")
	}
	if f.nat.settings.TcpBufferSettings.ReturnPersistTimeout != time.Second || f.nat.settings.TcpBufferSettings.ReturnPersistMaxTimeout != 30*time.Second {
		t.Fatal("test changed default provider persist cadence")
	}
	// Submit and physically deliver two further copies before offering the
	// three raw owners. Distinct Transfer originals are observed, not assumed.
	for i := 1; i < 3; i++ {
		offer := f.offer(MessagePoolCopy(zeroOffer.copy), false)
		if !offer.accepted || offer.count != 1 || offer.header != zero || offer.collapseDrops != 0 {
			t.Fatalf("zero-window original%d: %+v", i, offer)
		}
		tokens[i] = w.latest()
		w.waitFor("extra real zero-window claim", func() bool { return w.extraClaimCount >= i && w.zeroWireCount >= i+1 })
	}
	for i := 3; i < len(tokens); i++ {
		offer := f.offer(MessagePoolCopy(zeroOffer.copy), false)
		if !offer.accepted || offer.count != 1 || offer.header != zero || offer.collapseDrops != 0 {
			t.Fatalf("raw zero-window owner%d: %+v", i, offer)
		}
		tokens[i] = w.latest()
	}
	full := w.cut()
	if full.immediate != 3 || full.limit != 3 || full.retained != 3 || full.raw != 3 || full.rawLimit != 3 || full.dataBytes != 0 {
		t.Fatalf("missing independent immediate3/raw3 zero-DATA-charge premise: %+v", full)
	}
	w.stateLock.Lock()
	for i := range 3 {
		item := w.zeroWires[i]
		if item == nil || item.owner != wire.owner || item.generation != wire.generation || item.number != wire.number+uint64(i) || w.zeroRetired[i] {
			t.Error("pressure original identity/receipt mismatch")
		}
	}
	w.stateLock.Unlock()
	// This read is the real source of the enlarged advertised TCP window.
	syn, synAck := f.syn.Load(), f.synAck.Load()
	if syn == nil || synAck == nil || zero.seq != syn.seq+1+uint32(len(request)) || synAck.ack != syn.seq+1 {
		t.Fatal("native handshake/request edge not pinned")
	}
	prefixCount := int(uint32(zero.ack - (synAck.seq + 1)))
	if prefixCount < 1024 || prefixCount > 16*1024 {
		t.Fatalf("native receive prefix: %d", prefixCount)
	}
	prefix := make([]byte, prefixCount)
	if _, err := io.ReadFull(connection, prefix); err != nil {
		t.Fatal("native application drain", err)
	}
	var target persistNativeOffer
	var originalRefusals [64]nativeWindowCadenceLifecycle
	openOffers := 0
	for n := 0; ; n++ {
		if n == 64 {
			t.Fatal("native update drain exceeded actual queue bound")
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
			t.Fatalf("unexpected traffic during native reopen drain: %+v", offer)
		}
		openOffers++
		if offer.accepted || offer.count != 0 || !offer.beforeEligible || !offer.afterEligible || offer.collapseDrops != 0 ||
			!offer.beforeSeen || !offer.afterSeen || offer.beforeWindow != 0 || offer.afterWindow != 0 || offer.beforeAck != zero.ack || offer.afterAck != zero.ack {
			t.Fatalf("reopen not an unowned readiness refusal at unchanged TCP metadata: %+v", offer)
		}
		// The public indexed result and the same synchronous lifecycle token
		// identify this refusal; aggregate ACK counters cannot do that join.
		w.stateLock.Lock()
		refusal := w.lifetimes[w.latestToken]
		w.stateLock.Unlock()
		var admissionErr *SendPackAdmissionError
		if refusal.started != 1 || refusal.written != 1 || refusal.terminal != 1 ||
			!errors.As(refusal.terminalErr, &admissionErr) || admissionErr.Boundary != "pack-admission" ||
			admissionErr.Timeout != 0 || admissionErr.RecoveredByOwner || admissionErr.OwnerTrackingOverflow {
			t.Fatalf("actual native reopening lacks its exact unrecovered raw-admission refusal: %+v", refusal)
		}
		originalRefusals[openOffers-1] = refusal
		target = offer
	}
	if openOffers == 0 || f.openWire.Load() != nil {
		t.Fatal("missing real refused native window update")
	}
	expectedOpen := target.copy
	w.expectedOpen.Store(&expectedOpen)
	refusedAt := time.Now()
	beforeService := nativeWindowCadenceProviderCut(f.providerSequence.Load())
	if beforeService.window != 0 || beforeService.ack != zero.ack || beforeService.next != zero.ack || beforeService.retained != 0 || beforeService.chunks != 0 {
		t.Fatalf("refusal changed provider TCP state: %+v", beforeService)
	}
	// Finite receiver receipt service is an explicit test condition. It never
	// drops, fabricates or indefinitely withholds an accepted original.
	service := time.NewTimer(40 * time.Millisecond)
	select {
	case <-service.C:
	case <-f.ctx.Done():
		service.Stop()
		t.Fatal(f.ctx.Err())
	}
	claim := f.held.Swap(nil)
	if claim == nil {
		t.Fatal("lost first real held receipt")
	}
	var replayToken uint64
	if replayAfterOneSlot {
		w.pauseFourth.Store(true)
	}
	claim.complete(true)
	if replayAfterOneSlot {
		f.await(w.fourthEntered, "one original released and one raw owner serialized")
		// The finite real release created one slot. The retained local copy,
		// not another native read or an explicit test replay, must own it.
		f.await(w.openAdmitted, "automatic same-packet retry after one actual raw slot release")
		wire.sequence.protocolAdmission.mutex.Lock()
		raw := wire.sequence.protocolAdmission.count
		wire.sequence.protocolAdmission.mutex.Unlock()
		if raw != 3 {
			t.Fatalf("automatic retry lacks exact two old raw owners plus one new retry: %d", raw)
		}
		replayToken = w.latest()
		w.releaseFourthDo.Do(func() { close(w.releaseFourth) })
	}
	w.releaseExtraClaims()
	if supersedeBeforeMaterialization {
		f.await(w.retryEntered, "admitted retry parked after contract work and before materialization")
		group := w.retryGroup.Load()
		if group == nil || group.nativeAckRetry == nil || !group.nativeAckRetry.current() {
			t.Fatal("late-fence premise lacks the still-current admitted retry")
		}
		// This is a newer real native zero-window packet, not a forced serial
		// change. The source is parked, so its public acceptance precedes the
		// old retry's final materialization decision.
		newZero := f.offer(MessagePoolCopy(zeroOffer.copy), false)
		if !newZero.accepted || newZero.count != 1 || newZero.header != zero {
			t.Fatalf("new native feedback was not admitted at the held source: %+v", newZero)
		}
		zeroToken := w.latest()
		w.releaseRetryDo.Do(func() { close(w.releaseRetry) })
		w.waitFor("unwritten stale retry and actual new zero-window receipt", func() bool {
			return w.lifetimes[replayToken].terminal == 1 && w.lifetimes[zeroToken].terminal == 1
		})
		w.stateLock.Lock()
		stale, zeroLife := w.lifetimes[replayToken], w.lifetimes[zeroToken]
		w.stateLock.Unlock()
		if !errors.Is(stale.terminalErr, errNativeAckRetrySuperseded) || !sendPackFailedUnwritten(stale.terminalErr) ||
			stale.started != 1 || stale.written != 1 || stale.terminal != 1 ||
			zeroLife.started != 1 || zeroLife.written != 1 || zeroLife.terminal != 1 || zeroLife.terminalErr != nil ||
			f.openWire.Load() != nil || w.exactOpenWrites.Load() != 0 {
			t.Fatalf("superseded retry escaped or lost local custody: stale=%+v new_zero=%+v", stale, zeroLife)
		}
		zeroWire := w.supersedingWire.Load()
		if zeroWire == nil || zeroWire.owner != wire.owner || zeroWire.generation != wire.generation || zeroWire.number != wire.number+6 {
			t.Fatal("unwritten stale retry consumed a sequence ordinal")
		}
		actual := f.offer(MessagePoolCopy(target.copy), false)
		if !actual.accepted || actual.count != 1 || !actual.header.opens(zero) {
			t.Fatalf("same exact newer native update did not recover after stale disposal: %+v", actual)
		}
		replayToken = w.latest()
	}
	w.waitFor("all six accepted native owners retired with actual peer feedback", func() bool {
		if w.zeroWireCount != 6 {
			return false
		}
		for i, token := range tokens {
			entry := w.lifetimes[token]
			if !w.zeroRetired[i] || entry.terminal == 0 {
				return false
			}
		}
		return true
	})
	w.stateLock.Lock()
	for _, token := range tokens {
		entry := w.lifetimes[token]
		if entry.started != 1 || entry.written != 1 || entry.terminal != 1 || entry.writeErr != nil || entry.terminalErr != nil {
			t.Errorf("accepted original lost exact wire/terminal custody: %+v", entry)
		}
	}
	w.stateLock.Unlock()

	if replayAfterOneSlot {
		f.await(w.openAcked, "same-packet replay's actual terminal Transfer ACK")
		w.stateLock.Lock()
		replay := w.lifetimes[replayToken]
		w.stateLock.Unlock()
		if replay.started != 1 || replay.written != 1 || replay.terminal != 1 || replay.writeErr != nil || replay.terminalErr != nil {
			t.Fatalf("same-packet replay did not complete its own exact custody: %+v", replay)
		}
		if supersedeBeforeMaterialization {
			openWire := f.openWire.Load()
			if openWire == nil || openWire.owner != wire.owner || openWire.generation != wire.generation || openWire.number != wire.number+7 {
				t.Fatal("new exact ACK did not follow the actual zero-window original without a hole")
			}
		}
	}
	drained := w.cut()
	if drained.dataBytes != 0 {
		t.Fatalf("protocol recovery acquired DATA permission: %+v", drained)
	}
	// The six original token/receipt pairs above are all terminal. A distinct
	// automatic reopening may already own a new raw or reliable slot here.
	// Resume all spontaneous native traffic before the discriminating wait.
	// The sole remaining condition is TCP's actual unreceived window update.
	for len(f.serialAcks) != 0 {
		<-f.serialAcks
	}
	f.resumeDo.Do(func() { close(f.resumeBridge) })
	penalty := false
	var probe persistNativeHeader
	select {
	case <-w.opened:
	case probe = <-f.probeSeen:
		penalty = true
	case <-f.ctx.Done():
		t.Fatal("neither native reopening nor ordinary persist progressed", f.ctx.Err())
	}
	// Read timestamps rather than trusting select order when both events are
	// ready. An earlier real probe cannot be hidden by a later applied-open window.
	observedProbe, openedAt := w.probe.Load(), w.openedAt.Load()
	if openedAt != nil {
		applied := w.openedState.Load()
		if applied == nil || applied.window == 0 || applied.ack != zero.ack {
			t.Fatalf("exact terminal pure-ACK receipt lacks applied TCP window/frontier: %+v", applied)
		}
		t.Logf("NATIVE_WINDOW_UPDATE_APPLIED: same_frontier=true window=%d actual_terminal_ACK=true terminal_snapshot_before_serial_release=true snapshot_after_drain_ns=%d", applied.window, applied.at.Sub(drained.at).Nanoseconds())
	}
	penalty = observedProbe != nil && (openedAt == nil || !openedAt.Before(observedProbe.at))
	if penalty {
		if probe.payload == 0 {
			select {
			case probe = <-f.probeSeen:
			case <-f.ctx.Done():
				t.Fatal(f.ctx.Err())
			}
		}
	}
	if penalty {
		if observedProbe == nil {
			t.Fatal("persist event lacks exact timestamp/frontier capture")
		}
		at, cut := observedProbe.at, observedProbe.provider
		if !at.After(drained.at) || probe.seq != zero.ack-1 || probe.payload != 1 || cut.window != 0 || cut.ack != zero.ack || cut.next != zero.ack || cut.retained != 0 || cut.chunks != 0 {
			t.Fatalf("persist penalty lacks drained custody and exact fully ACKed zero-window frontier: probe=%+v cut=%+v", probe, cut)
		}
		t.Logf("NATIVE_WINDOW_UPDATE_PERSIST_EVIDENCE: lane=%d immediate=3 raw=3 accepted=6 receipts=6 refused_updates=%d service_ns=%d drained_after_refusal_ns=%d probe_after_zero_wait_ns=%d probe_after_drain_ns=%d default_persist_ns=%d DATA_bytes=0", wire.owner.LogicalLane, openOffers, (40 * time.Millisecond).Nanoseconds(), drained.at.Sub(refusedAt).Nanoseconds(), at.Sub(parked.at).Nanoseconds(), at.Sub(drained.at).Nanoseconds(), f.nat.settings.TcpBufferSettings.ReturnPersistTimeout.Nanoseconds())
	}
	f.probeReleaseDo.Do(func() { close(f.releaseProbe) })
	if penalty {
		select {
		case regenerated := <-f.regenerated:
			if !regenerated.header.opens(zero) || !regenerated.accepted || regenerated.count != 1 || regenerated.collapseDrops != 0 {
				t.Fatalf("natural persist response failed real native reoffer: %+v", regenerated)
			}
		case <-f.ctx.Done():
			t.Fatal("real native persist response did not recover", f.ctx.Err())
		}
	}
	reader := bufio.NewReader(io.MultiReader(bytes.NewReader(prefix), connection))
	readResponse := func(expected []byte) {
		t.Helper()
		response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodGet})
		if err != nil {
			t.Fatal(err)
		}
		body, readErr := io.ReadAll(response.Body)
		closeErr := response.Body.Close()
		if response.StatusCode != http.StatusOK || response.ContentLength != int64(len(expected)) || !bytes.Equal(body, expected) || readErr != nil || closeErr != nil {
			t.Fatalf("HTTP bytes/custody: status=%d len=%d read=%v close=%v", response.StatusCode, len(body), readErr, closeErr)
		}
	}
	readResponse(f.body)
	reuse := []byte("GET /reuse HTTP/1.1\r\nHost: native-persist.example\r\n\r\n")
	if n, err := connection.Write(reuse); err != nil || n != len(reuse) {
		t.Fatalf("same-flow small request: n=%d err=%v", n, err)
	}
	readResponse([]byte("reuse"))
	if f.dials.Load() != 1 || f.tun.LinkStats().OutboundDropCount != 0 || f.ctx.Err() != nil || f.parent.ctx.Err() != nil {
		t.Fatal("proof lost native traffic, reconnected or canceled a shared parent")
	}
	if !penalty && (w.exactOpenWrites.Load() != 1 || w.openedState.Load() == nil) {
		t.Fatal("progress lacks one unchanged automatic native update and its real terminal ACK")
	}
	if replayAfterOneSlot && penalty {
		t.Fatal("automatic admitted window update still required a persist probe")
	}
	if supersedeBeforeMaterialization && w.targetResends.Load() != 0 {
		t.Fatalf("stale retry left a recovery hole: actual source retransmits=%d", w.targetResends.Load())
	}
	w.stateLock.Lock()
	for _, original := range originalRefusals[:openOffers] {
		after := w.lifetimes[original.token]
		var admissionErr *SendPackAdmissionError
		if after.started != original.started || after.written != original.written || after.terminal != original.terminal ||
			after.writeErr != original.writeErr || after.terminalErr != original.terminalErr ||
			!errors.As(after.terminalErr, &admissionErr) || admissionErr.RecoveredByOwner {
			t.Error("later recovery rewrote the original native false/refusal lifetime")
		}
	}
	w.stateLock.Unlock()
	path, _, err := ParseIpPathWithPayload(target.copy)
	if err != nil {
		t.Fatal(err)
	}
	f.parent.stateLock.Lock()
	update := f.parent.affinityUpdateWithLock(path)
	pending := f.parent.nativeAckRetryCount
	if update != nil {
		update.stateLock.Lock()
		if update.nativeAckRetry != nil {
			pending++
		}
		update.stateLock.Unlock()
	}
	f.parent.stateLock.Unlock()
	if pending != 0 {
		t.Fatal("real terminal receipt left a local retry owner that could repeat")
	}
	t.Logf("NATIVE_WINDOW_UPDATE_RECOVERED: replay_after_one_slot=%t natural_persist=%t complete_body=%d reused_http=true", replayAfterOneSlot, penalty, len(f.body))
	penaltyAfterJoin = !replayAfterOneSlot && penalty
}
