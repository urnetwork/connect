// An explicit lossless test carrier holds one physical Pack until the actual
// receive worker releases its local handoff. This is never a transfer ACK and
// does not change the ordinary harness's zero-wait refusal/recovery workload.
package connect

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// The root pointer is identity only: it is never dereferenced after transfer.
// Matching wire metadata alone cannot identify a distinct physical copy.
type sendWindowWireIdentity struct {
	path           TransferPath
	sequenceId     Id
	messageId      Id
	sequenceNumber uint64
	logicalLane    uint32
	head           bool
	nack           bool
	root           *byte
	byteCount      int
}

// One channel belongs to one physical publication and closes only on its real
// matching dequeue. Cancellation abandons the claim without pretending release.
type sendWindowWireClaim struct {
	identity sendWindowWireIdentity
	done     chan struct{}
}

// Copies of scalar state support bounded custody controls after quiescence.
type sendWindowWireCreditSnapshot struct {
	pending     bool
	identity    sendWindowWireIdentity
	armed       uint64
	transferred uint64
	released    uint64
	abandoned   uint64
	maxPending  int
	closed      bool
}

// The opt-in carrier owns a single claim, no packet queue and one worker.
// Receiver/path bindings are immutable before incoming routes are connected.
type sendWindowWireCredit struct {
	t         *testing.T
	receiver  *Client
	path      TransferPath
	done      chan struct{}
	stateLock sync.Mutex
	claim     *sendWindowWireClaim
	state     sendWindowWireCreditSnapshot
	// Only controls install this cancellation-aware pre-publication barrier.
	beforeForwardForTest func(sendWindowWireIdentity)
}

// Construction does not alter a client setting, byte limit, service clock or
// timeout. Only the named lossless carrier constructor opts into this credit.
func newSendWindowWireCredit(t *testing.T) *sendWindowWireCredit {
	t.Helper()
	return &sendWindowWireCredit{t: t, done: make(chan struct{})}
}

// The ordinary link still owns serialization and propagation. This wrapper
// adds only local handoff credit, with the original finite upstream queues.
func newLosslessSendWindowHarnessWithClient(t *testing.T, ctx context.Context, ackDelay time.Duration,
	configure func(*SendBufferSettings), configureClient func(*ClientSettings)) *sendWindowHarness {
	t.Helper()
	return newPacedSendWindowHarnessWithClient(t, ctx, ackDelay, 0, deepCarrierFrameCapacity,
		configure, configureClient, newSendWindowWireCredit(t))
}

// A new carrier claim cannot replace or queue behind an existing claim.
func (self *sendWindowWireCredit) arm(identity sendWindowWireIdentity) *sendWindowWireClaim {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.state.closed || self.claim != nil {
		self.t.Error("lossless wire credit attempted a second pending physical owner")
		return nil
	}
	claim := &sendWindowWireClaim{identity: identity, done: make(chan struct{})}
	self.claim = claim
	self.state.armed++
	self.state.maxPending = 1
	return claim
}

// A scalar snapshot never borrows a Pack, decoded owner or payload bytes.
func (self *sendWindowWireCredit) snapshot() sendWindowWireCreditSnapshot {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	state := self.state
	state.pending = self.claim != nil
	if self.claim != nil {
		state.identity = self.claim.identity
	}
	return state
}

// A prior generation, unrelated packet, wrong physical copy or duplicate event
// cannot release the current claim. The caller has only copied identity facts.
func (self *sendWindowWireCredit) release(sequence *ReceiveSequence, identity sendWindowWireIdentity) bool {
	if sequence == nil || sequence.client != self.receiver || sequence.ctx.Err() != nil ||
		sequence.source.SourceId != identity.path.SourceId || sequence.source.StreamId != identity.path.StreamId ||
		sequence.sequenceId != identity.sequenceId || sequence.transferKey.LogicalLane != identity.logicalLane {
		return false
	}
	buffer := self.receiver.receiveBuffer
	buffer.mutex.Lock()
	current := buffer.receiveSequences[sequence.id()] == sequence
	buffer.mutex.Unlock()
	if !current {
		return false
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.state.closed || self.claim == nil || self.claim.identity != identity {
		return false
	}
	claim := self.claim
	self.claim = nil
	self.state.released++
	close(claim.done)
	return true
}

// Takes identity only from the exact root still owned by the real worker.
// Protocol and DATA share this hook; neither receives an artificial ACK.
func sendWindowWireReleasedIdentity(sequence *ReceiveSequence, owner *decodedPackOwner) (sendWindowWireIdentity, bool) {
	if sequence == nil || owner == nil {
		return sendWindowWireIdentity{}, false
	}
	pack := &owner.receivePack
	if pack.Pack == nil || len(pack.TransferFrameBytes) == 0 {
		return sendWindowWireIdentity{}, false
	}
	messageId, err := IdFromBytes(pack.Pack.MessageId)
	if err != nil {
		return sendWindowWireIdentity{}, false
	}
	sequenceId, err := IdFromBytes(pack.Pack.SequenceId)
	if err != nil || sequenceId != pack.SequenceId {
		return sendWindowWireIdentity{}, false
	}
	return sendWindowWireIdentity{
		path:       TransferPath{SourceId: pack.Source.SourceId, DestinationId: sequence.client.ClientId(), StreamId: pack.Source.StreamId},
		sequenceId: sequenceId, messageId: messageId, sequenceNumber: pack.Pack.SequenceNumber,
		logicalLane: pack.Pack.LogicalLane, head: pack.Pack.Head, nack: pack.Pack.Nack,
		root: &pack.TransferFrameBytes[0], byteCount: len(pack.TransferFrameBytes),
	}, true
}

// Observing another stage never grants credit. In particular a successful raw
// Pack offer and later ACK callback are different boundaries from this dequeue.
func (self *sendWindowWireCredit) observe(sequence *ReceiveSequence, owner *decodedPackOwner, stage string) {
	if stage != "pack_released" {
		return
	}
	identity, ok := sendWindowWireReleasedIdentity(sequence, owner)
	if ok {
		self.release(sequence, identity)
	}
}

// Takes each frame from the carrier stage. Before a successful output it owns
// and returns the frame on cancellation; after output only the Client owns it.
// There is no timer, service charge, new buffered queue or packet replay here.
func (self *sendWindowWireCredit) run(ctx context.Context, from, to Route) {
	defer close(self.done)
	defer func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.claim != nil {
			self.state.abandoned++
			self.claim = nil
		}
		self.state.closed = true
	}()
	for {
		var wire []byte
		select {
		case <-ctx.Done():
			return
		case wire = <-from:
		}
		if ctx.Err() != nil {
			MessagePoolReturn(wire)
			return
		}
		scan, status := scanReceiveMemoryPack(wire, self.path)
		var claim *sendWindowWireClaim
		if status == receiveMemoryPackScanReady && scan.pathMatches && !scan.encrypted {
			identity := sendWindowWireIdentity{
				path: scan.path, sequenceId: scan.sequenceId, messageId: scan.messageId,
				sequenceNumber: scan.sequenceNumber, logicalLane: scan.logicalLane, head: scan.head, nack: scan.nack,
				root: &wire[0], byteCount: len(wire),
			}
			claim = self.arm(identity)
			if claim == nil {
				MessagePoolReturn(wire)
				return
			}
			if self.beforeForwardForTest != nil {
				self.beforeForwardForTest(identity)
			}
		} else if status != receiveMemoryPackScanNotPack || !scan.ack || !scan.pathMatches {
			MessagePoolReturn(wire)
			self.t.Error("lossless test carrier received an unsupported physical wire shape")
			return
		}
		if ctx.Err() != nil {
			MessagePoolReturn(wire)
			return
		}
		select {
		case <-ctx.Done():
			MessagePoolReturn(wire)
			return
		case to <- wire:
		}
		if claim != nil {
			self.stateLock.Lock()
			self.state.transferred++
			self.stateLock.Unlock()
			select {
			case <-ctx.Done():
				return
			case <-claim.done:
			}
		}
	}
}

// All deliberate pressure occurs at the actual receiver worker. One accepted
// physical Pack is pending while later frames remain in the existing carrier;
// wrong identities, physical copies and generations cannot open that credit.
func TestSendWindowLosslessCreditWaitsForExactReceiverRelease(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		entered, resume := make(chan struct{}), make(chan struct{})
		secondEntered, resumeSecond := make(chan struct{}), make(chan struct{})
		var enterOnce, resumeOnce, secondOnce, resumeSecondOnce sync.Once
		releaseWorker := func() { resumeOnce.Do(func() { close(resume) }) }
		releaseSecond := func() { resumeSecondOnce.Do(func() { close(resumeSecond) }) }
		defer releaseWorker()
		defer releaseSecond()
		var firstSequence *ReceiveSequence
		var firstRelease sendWindowWireIdentity
		var actualReleases atomic.Int32
		harness := newLosslessSendWindowHarnessWithClient(t, ctx, 200*time.Millisecond, nil,
			func(settings *ClientSettings) {
				settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
				settings.ReceiveBufferSettings.SequenceBufferSize = 1
				settings.ReceiveBufferSettings.beforeRunReceiveSequenceForTest = func(receiveSequenceId) {
					enterOnce.Do(func() { close(entered) })
					select {
					case <-resume:
					case <-ctx.Done():
					}
				}
				settings.ReceiveBufferSettings.receiveOwnerStageForTest = func(sequence *ReceiveSequence, owner *decodedPackOwner, stage string) {
					if stage != "pack_released" {
						return
					}
					identity, ok := sendWindowWireReleasedIdentity(sequence, owner)
					if !ok {
						t.Error("actual receiver release lost its physical identity")
						return
					}
					if actualReleases.Add(1) == 1 {
						firstSequence, firstRelease = sequence, identity
					} else {
						secondOnce.Do(func() { close(secondEntered) })
						select {
						case <-resumeSecond:
						case <-ctx.Done():
						}
					}
				}
			})
		var delivered atomic.Int32
		harness.receiver.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, _ Peer) { delivered.Add(int32(len(frames))) })
		acks := make(chan error, 2)
		for i := 0; i < 2; i++ {
			frame := RequireToFrameWithDefaultProtocolVersion(&protocol.SimpleMessage{Content: "synthetic lossless wire"})
			if accepted, err := harness.sender.SendWithTimeoutDetailed(frame, harness.receiverId, func(err error) { acks <- err }, 0); !accepted || err != nil {
				MessagePoolReturn(frame.MessageBytes)
				t.Fatalf("lossless public send refused: accepted=%t err=%v", accepted, err)
			}
			if i == 0 {
				<-entered
			}
			synctest.Wait()
		}
		credit := harness.wireCredit
		before := credit.snapshot()
		var queued *ReceiveSequence
		harness.receiver.receiveBuffer.mutex.Lock()
		for _, sequence := range harness.receiver.receiveBuffer.receiveSequences {
			if sequence.source.SourceId == harness.sender.ClientId() {
				queued = sequence
			}
		}
		harness.receiver.receiveBuffer.mutex.Unlock()
		if !before.pending || before.armed != 1 || before.transferred != 1 || before.released != 0 || before.maxPending != 1 ||
			queued == nil || len(queued.packs) != 1 || queued.packQueueCount.Load() != 1 || delivered.Load() != 0 || len(acks) != 0 {
			t.Fatalf("paused receiver did not retain exactly one physical credit: %+v", before)
		}
		for _, mutation := range []string{"source", "sequence", "message", "number", "physical-root"} {
			wrong := before.identity
			switch mutation {
			case "source":
				wrong.path.SourceId = NewId()
			case "sequence":
				wrong.sequenceId = NewId()
			case "message":
				wrong.messageId = NewId()
			case "number":
				wrong.sequenceNumber++
			case "physical-root":
				wrong.root = new(byte)
			}
			if credit.release(queued, wrong) {
				t.Fatalf("unrelated %s event released physical credit", mutation)
			}
		}
		stale := &ReceiveSequence{ctx: queued.ctx, client: queued.client, source: queued.source,
			sequenceId: queued.sequenceId, encryptionRole: queued.encryptionRole, transferKey: queued.transferKey}
		if credit.release(stale, before.identity) || credit.snapshot().released != 0 {
			t.Fatal("an unregistered sequence generation released physical credit")
		}
		releaseWorker()
		<-secondEntered
		synctest.Wait()
		second := credit.snapshot()
		if !second.pending || second.armed != 2 || second.transferred != 2 || second.released != 1 ||
			second.identity.messageId == firstRelease.messageId || len(acks) != 0 || firstSequence != queued {
			t.Fatalf("first real release did not admit exactly one next physical copy without an ACK: %+v", second)
		}
		if credit.release(firstSequence, firstRelease) || credit.release(firstSequence, firstRelease) || credit.snapshot().released != 1 {
			t.Fatal("duplicate/stale release returned the next physical credit")
		}
		releaseSecond()
		synctest.Wait()
		after := credit.snapshot()
		stats := harness.receiver.ReceiveStats()
		if after.pending || after.released != 2 || actualReleases.Load() != 2 || delivered.Load() != 2 || len(acks) != 0 ||
			stats.PackHandoffDropCount != 0 || stats.PackHandoffWaitCount != 0 || stats.PackHandoffMaxCount != 1 {
			t.Fatalf("real release changed bounded handoff or delivered/ACK truth: credit=%+v delivered=%d ACKs=%d stats=%+v", after, delivered.Load(), len(acks), stats)
		}
		deadline := time.After(time.Second)
		for i := 0; i < 2; i++ {
			select {
			case err := <-acks:
				if err != nil {
					t.Fatalf("real delayed ACK failed: %v", err)
				}
			case <-deadline:
				t.Fatal("real delayed ACK did not complete within the original prefix bound")
			}
		}
		synctest.Wait()
		if stats := harness.sender.DestinationSendStats(harness.receiverId); stats.ResendWriteByteCount != 0 {
			t.Fatal("lossless local credit required a retransmission")
		}
		harness.sender.sendBuffer.mutex.Lock()
		source := harness.sender.sendBuffer.sendSequencesBySequenceId[firstRelease.sequenceId]
		harness.sender.sendBuffer.mutex.Unlock()
		if source == nil {
			t.Fatal("real delayed ACKs lost their actual source sequence")
		}
		retained, _, _, _ := source.ResendQueueSizeAndMessageTypes()
		if retained != 0 || len(acks) != 0 {
			t.Fatal("real delayed ACKs did not retire exactly the two source originals")
		}
	})
}

// Cancellation on either side of the physical transfer joins the one carrier
// and both real clients. Before transfer the carrier returns the root; after
// transfer the receiver does. The worker gate opens only after its own context
// is canceled: the parent's Done channel can close before child cancellation.
// Neither ordered cancellation is recorded as a release.
func TestSendWindowLosslessCreditCancellationReturnsExactOwner(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, control := range []struct {
		name        string
		transferred bool
	}{{"before_handoff", false}, {"after_handoff_before_receiver_run", true}} {
		t.Run(control.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				transferred := control.transferred
				ctx, cancel := context.WithCancel(context.Background())
				entered, resume := make(chan struct{}), make(chan struct{})
				var once, resumeOnce sync.Once
				releaseWorker := func() { resumeOnce.Do(func() { close(resume) }) }
				defer func() {
					cancel()
					releaseWorker()
				}()
				credit := newSendWindowWireCredit(t)
				if !transferred {
					credit.beforeForwardForTest = func(sendWindowWireIdentity) {
						once.Do(func() { close(entered) })
						<-ctx.Done()
					}
				}
				harness := newPacedSendWindowHarnessWithClient(t, ctx, 200*time.Millisecond, 0, deepCarrierFrameCapacity, nil,
					func(settings *ClientSettings) {
						settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
						settings.ReceiveBufferSettings.SequenceBufferSize = 1
						if transferred {
							settings.ReceiveBufferSettings.beforeRunReceiveSequenceForTest = func(receiveSequenceId) {
								once.Do(func() { close(entered) })
								<-resume
							}
						}
					}, credit)
				var delivered, succeeded atomic.Int32
				harness.receiver.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, _ Peer) { delivered.Add(int32(len(frames))) })
				frame := RequireToFrameWithDefaultProtocolVersion(&protocol.SimpleMessage{Content: "synthetic canceled physical owner"})
				if accepted, err := harness.sender.SendWithTimeoutDetailed(frame, harness.receiverId, func(err error) {
					if err == nil {
						succeeded.Add(1)
					}
				}, 0); !accepted || err != nil {
					MessagePoolReturn(frame.MessageBytes)
					t.Fatalf("cancellation public send refused: accepted=%t err=%v", accepted, err)
				}
				<-entered
				synctest.Wait()
				before := credit.snapshot()
				wantTransferred := uint64(0)
				var queued *ReceiveSequence
				if transferred {
					wantTransferred = 1
					harness.receiver.receiveBuffer.mutex.Lock()
					for _, sequence := range harness.receiver.receiveBuffer.receiveSequences {
						if sequence.sequenceId == before.identity.sequenceId && sequence.source.SourceId == before.identity.path.SourceId {
							queued = sequence
						}
					}
					harness.receiver.receiveBuffer.mutex.Unlock()
					if queued == nil || queued.ctx.Err() != nil || len(queued.packs) != 1 || queued.packQueueCount.Load() != 1 {
						t.Fatal("post-handoff cancellation did not hold the live receiver's exact accepted Pack")
					}
				}
				if !before.pending || before.armed != 1 || before.transferred != wantTransferred || before.released != 0 ||
					delivered.Load() != 0 || succeeded.Load() != 0 {
					t.Fatalf("cancellation did not reach its exact ownership side: transferred=%t state=%+v", transferred, before)
				}
				cancel()
				if harness.receiver.ctx.Err() == nil || queued != nil && queued.ctx.Err() == nil {
					t.Fatal("parent cancellation returned before the held receiver's cancellation fence")
				}
				releaseWorker()
				<-credit.done
				joined, stop := context.WithTimeout(context.Background(), 10*time.Second)
				defer stop()
				for _, client := range []*Client{harness.sender, harness.receiver} {
					if err := client.CloseAndWait(joined); err != nil {
						t.Fatal(err)
					}
				}
				synctest.Wait()
				after := credit.snapshot()
				if after.pending || !after.closed || after.armed != 1 || after.transferred != wantTransferred ||
					after.abandoned != 1 || after.released != 0 || delivered.Load() != 0 || succeeded.Load() != 0 {
					t.Fatalf("ordered cancellation fabricated credit/delivery/ACK or retained its owner: transferred=%t state=%+v delivered=%d successful=%d", transferred, after, delivered.Load(), succeeded.Load())
				}
			})
		})
	}
}

// Cancellation does not revoke a head already accepted into the delivery
// batch. Its exit flush delivers exactly once, while the canceled real ACK
// carrier cannot complete the sender's delayed Transfer ACK.
func TestSendWindowLosslessCreditBufferedDeliverySurvivesCancellation(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		entered, resume := make(chan struct{}), make(chan struct{})
		var resumeOnce sync.Once
		releaseWorker := func() { resumeOnce.Do(func() { close(resume) }) }
		defer func() {
			cancel()
			releaseWorker()
		}()
		var buffered *ReceiveSequence
		var releasedIdentity, bufferedIdentity sendWindowWireIdentity
		var releasedOnce sync.Once
		var bufferedValid bool
		var heads atomic.Int32
		harness := newLosslessSendWindowHarnessWithClient(t, ctx, 200*time.Millisecond, nil,
			func(settings *ClientSettings) {
				settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
				settings.ReceiveBufferSettings.SequenceBufferSize = 1
				settings.ReceiveBufferSettings.receiveOwnerStageForTest = func(sequence *ReceiveSequence, owner *decodedPackOwner, stage string) {
					if stage == "pack_released" {
						releasedOnce.Do(func() { releasedIdentity, _ = sendWindowWireReleasedIdentity(sequence, owner) })
						return
					}
					if stage != "head_buffered" || heads.Add(1) != 1 {
						return
					}
					buffered = sequence
					bufferedIdentity, bufferedValid = sendWindowWireReleasedIdentity(sequence, owner)
					bufferedValid = bufferedValid && len(sequence.deliverItems) == 1 && len(sequence.deliverFrames) == 1 &&
						sequence.deliverItems[0].decodedOwner == owner
					close(entered)
					<-resume
				}
			})
		var delivered, succeeded atomic.Int32
		var changedContent atomic.Bool
		const content = "synthetic buffered physical owner"
		harness.receiver.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, _ Peer) {
			for _, frame := range frames {
				var message protocol.SimpleMessage
				if err := ProtoUnmarshal(frame.MessageBytes, &message); err != nil || message.Content != content {
					changedContent.Store(true)
				}
				delivered.Add(1)
			}
		})
		frame := RequireToFrameWithDefaultProtocolVersion(&protocol.SimpleMessage{Content: content})
		if accepted, err := harness.sender.SendWithTimeoutDetailed(frame, harness.receiverId, func(err error) {
			if err == nil {
				succeeded.Add(1)
			}
		}, 0); !accepted || err != nil {
			MessagePoolReturn(frame.MessageBytes)
			t.Fatalf("buffered public send refused: accepted=%t err=%v", accepted, err)
		}
		<-entered
		synctest.Wait()
		credit := harness.wireCredit
		before := credit.snapshot()
		if !bufferedValid || buffered == nil || buffered.ctx.Err() != nil || bufferedIdentity != releasedIdentity ||
			bufferedIdentity.root == nil || before.pending || before.armed != 1 || before.transferred != 1 ||
			before.released != 1 || before.abandoned != 0 || delivered.Load() != 0 || succeeded.Load() != 0 {
			t.Fatalf("buffered cancellation did not own the exact already-released head before dispatch: credit=%+v", before)
		}
		cancel()
		if buffered.ctx.Err() == nil {
			t.Fatal("buffered receiver resumed before its own context cancellation")
		}
		<-credit.done
		releaseWorker()
		joined, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		for _, client := range []*Client{harness.sender, harness.receiver} {
			if err := client.CloseAndWait(joined); err != nil {
				t.Fatal(err)
			}
		}
		synctest.Wait()
		after := credit.snapshot()
		if after.pending || !after.closed || after.armed != 1 || after.transferred != 1 || after.released != 1 ||
			after.abandoned != 0 || heads.Load() != 1 || delivered.Load() != 1 || changedContent.Load() || succeeded.Load() != 0 {
			t.Fatalf("accepted buffered head lost exact delivery/custody or fabricated a successful delayed ACK: credit=%+v heads=%d delivered=%d successful=%d", after, heads.Load(), delivered.Load(), succeeded.Load())
		}
	})
}
