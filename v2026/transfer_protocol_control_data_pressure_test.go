// Real public sends keep data owners full while independent reliable TCP
// controls cross the same clients, retry lost wire, and retain exact ownership.
package connect

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// Completion belongs to one accepted public send, including cancellation.
type protocolPressureCompletion struct {
	calls    atomic.Int32
	success  atomic.Int32
	canceled atomic.Int32
	other    atomic.Int32
}

// Records terminal fate without borrowing packet bytes or blocking a worker.
func (self *protocolPressureCompletion) record(err error) {
	self.calls.Add(1)
	switch {
	case err == nil:
		self.success.Add(1)
	case errors.Is(err, context.Canceled), err.Error() == "Send sequence closed.":
		self.canceled.Add(1)
	default:
		self.other.Add(1)
	}
}

// Byte counters must remain unchanged through every control transition.
type protocolPressureBudget struct {
	budget             *TransferMemoryBudget
	used, total        ByteCount
	reserved, released ByteCount
}

// A missing data predecessor keeps actual resend and decoded receive owners
// alive. Controls use another logical lane; reliable ordering is not bypassed.
type protocolDataPressureFixture struct {
	wire         *windowRoundFixture
	budgets      []protocolPressureBudget
	dataSender   *SendSequence
	dataReceiver *ReceiveSequence
	dataResults  [2]protocolPressureCompletion
	version      int
}

// Uses ordinary public TCP data to occupy both leaves and their shared root.
// Shrinking to those exact owners creates pressure without anonymous filler.
func newProtocolDataPressureFixture(t *testing.T, version int, flowIsolation bool, configure ...func(*SendBufferSettings)) *protocolDataPressureFixture {
	t.Helper()
	assertMessagePoolOwnership(t)
	root := NewTransferMemoryBudget(2 * 1024 * 1024)
	tx := NewTransferMemoryBudgetWithParent(1024*1024, root)
	rx := NewTransferMemoryBudgetWithParent(1024*1024, root)
	t.Cleanup(func() {
		for _, budget := range []*TransferMemoryBudget{tx, rx, root} {
			assertRetainedBudgetBalance(t, budget)
		}
	})
	self := &protocolDataPressureFixture{version: version}
	self.wire = newWindowRoundFixture(t, func(settings *SendBufferSettings) {
		settings.ResendQueueBudget = tx
		settings.ResendQueueRetainedByteAccounting = true
		settings.ResendQueueMinByteCount = 0
		settings.ResendQueueMaxByteCount = 1024 * 1024
		for _, change := range configure {
			change(settings)
		}
	}, func(settings *ReceiveBufferSettings) {
		settings.ReceiveQueueBudget = rx
		settings.ReceiveQueueRetainedByteAccounting = true
		settings.ReceiveQueueMinByteCount = 0
		settings.ReceiveQueueMaxByteCount = 1024 * 1024
		settings.ReceiveHoldPolicy = ReceiveHoldCommittedPrefix
	}, TransportTypeH1)
	routes := self.wire.sender.RouteManager()
	routes.mutex.Lock()
	var transport Transport
	for candidate := range routes.writerMatchState.transportRoutes {
		if transportTypeOf(candidate) == TransportTypeH1 {
			transport = candidate
			break
		}
	}
	routes.mutex.Unlock()
	if transport == nil {
		t.Fatal("controlled client did not publish its H1 send transport")
	}
	routes.UpdateTransportWithProperties(transport, []Route{self.wire.senderOut}, TransferCarrierProperties{
		Unreliable: true, UnreliableFlowIsolation: flowIsolation,
	})

	var warm protocolPressureCompletion
	if admitted, err := self.send(t, 1, tcpFlagAck, 1240, false, &warm, time.Second); !admitted || err != nil {
		t.Fatalf("initial public data admission=%t error=%v", admitted, err)
	}
	self.wire.forward(self.take(t, 1, 0), self.wire.receiverIn)
	self.wire.acknowledge()
	if warm.calls.Load() != 1 || warm.success.Load() != 1 || root.UsedByteCount() != 0 {
		t.Fatal("initial real delivery did not acknowledge and release data custody")
	}
	self.dataSender = self.wire.sequence()
	self.dataReceiver = self.wire.receiveSequence()
	if policy := self.dataSender.transferFlightPolicy(); !policy.limited || policy.flowIsolation != flowIsolation {
		t.Fatalf("actual H1 route did not publish the intended flow policy: %+v", policy)
	}
	// Establish this sibling before pressure, so a later zero-time refusal
	// tests byte admission rather than a cold worker's readiness.
	var controlWarm protocolPressureCompletion
	if admitted, err := self.send(t, 2, tcpFlagAck, 0, false, &controlWarm, time.Second); !admitted || err != nil {
		t.Fatalf("initial ordinary control admission=%t error=%v", admitted, err)
	}
	self.wire.forward(self.take(t, 2, 0), self.wire.receiverIn)
	self.wire.acknowledge()
	if controlWarm.calls.Load() != 1 || controlWarm.success.Load() != 1 || root.UsedByteCount() != 0 {
		t.Fatal("initial control sibling did not acknowledge and release ordinary send custody")
	}
	for index := range self.dataResults {
		if admitted, err := self.send(t, 1, tcpFlagAck, 1240, false, &self.dataResults[index], time.Second); !admitted || err != nil {
			t.Fatalf("held public data %d admission=%t error=%v", index, admitted, err)
		}
		frame := self.take(t, 1, uint64(index+1))
		if index == 1 {
			self.wire.forward(frame, self.wire.receiverIn)
		}
	}
	// Keep selective feedback away from the sender, so both real data sends
	// remain retained while the receiver owns the later decoded original.
	for _, ack := range self.wire.takeAcknowledgements() {
		if !ack.ack.Selective {
			t.Fatal("missing data predecessor received a false cumulative acknowledgement")
		}
	}
	held := self.dataReceiver.receiveQueue.GetBySequenceNumber(2)
	if held == nil || !held.committed || held.decodedOwner == nil || !held.decodedOwner.receivePack.receiveMemoryOwned ||
		self.dataSender.resendQueue.Len() != 2 || tx.UsedByteCount() == 0 || rx.UsedByteCount() == 0 ||
		root.UsedByteCount() != tx.UsedByteCount()+rx.UsedByteCount() {
		t.Fatal("actual public data did not retain composed send and decoded receive custody")
	}
	for _, budget := range []*TransferMemoryBudget{tx, rx, root} {
		budget.SetTotalByteCount(budget.UsedByteCount())
		reserved, released := budget.Counts()
		self.budgets = append(self.budgets, protocolPressureBudget{
			budget: budget, used: budget.UsedByteCount(), total: budget.TotalByteCount(),
			reserved: reserved, released: released,
		})
	}
	// A live resize need not wake an idle sender that has not subscribed to
	// budget pressure yet. Republish the same real carrier to wake both
	// established loops, then observe their ordinary admission publication.
	routes.UpdateTransportWithProperties(transport, []Route{self.wire.senderOut}, TransferCarrierProperties{
		Unreliable: true, UnreliableFlowIsolation: flowIsolation,
	})
	synctest.Wait()
	self.wire.sender.sendBuffer.mutex.Lock()
	fullLanes := 0
	for _, sequence := range self.wire.sender.sendBuffer.sendSequencesBySequenceId {
		if (sequence.logicalLane == 1 || sequence.logicalLane == 2) && sequence.resendCapacityUnavailable.Load() {
			fullLanes++
		}
	}
	self.wire.sender.sendBuffer.mutex.Unlock()
	if fullLanes != 2 {
		t.Fatal("actual carrier wake did not publish full data admission on both established lanes")
	}
	self.requireDataPressure(t)
	return self
}

// Takes on success only; a refused public send leaves the pooled root here.
func (self *protocolDataPressureFixture) send(t *testing.T, lane uint32, flags byte, payloadBytes int, noBudget bool, completion *protocolPressureCompletion, timeout time.Duration) (bool, error) {
	t.Helper()
	packet := MessagePoolCopy(ipOosTcpPacketSequence(reliableDeliveryTestPath(self.version), flags, 7000, bytes.Repeat([]byte{0x58}, payloadBytes)))
	frame, err := ipPacketToProviderFrame(packet, DefaultProtocolVersion)
	if err != nil || !frame.Raw {
		MessagePoolReturn(packet)
		t.Fatalf("public TCP frame construction: %v", err)
	}
	opts := []any{TransferKey{LogicalLane: lane}}
	if noBudget {
		opts = append(opts, NoBudget())
	}
	admitted, err := self.wire.sender.SendWithTimeoutDetailed(frame, self.wire.receiver.ClientId(), completion.record, timeout, opts...)
	if !admitted {
		MessagePoolReturn(frame.MessageBytes)
	}
	return admitted, err
}

// Retains other actual rewrites while selecting one lane's expected wire.
func (self *protocolDataPressureFixture) take(t *testing.T, lane uint32, number uint64) *windowRoundFrame {
	t.Helper()
	synctest.Wait()
	for range cap(self.wire.senderOut) {
		frame := self.wire.take(self.wire.senderOut)
		if frame.pack != nil && frame.pack.LogicalLane == lane && frame.pack.SequenceNumber == number {
			if frame.pack.Nack || len(frame.pack.Frames) != 1 {
				t.Fatal("public reliable singleton changed its acknowledgement or frame shape")
			}
			return frame
		}
	}
	t.Fatalf("no actual wire for lane %d number %d", lane, number)
	return nil
}

// No control may consume, release, resize, or acknowledge the retained data.
func (self *protocolDataPressureFixture) requireDataPressure(t *testing.T) {
	t.Helper()
	synctest.Wait()
	for _, snapshot := range self.budgets {
		reserved, released := snapshot.budget.Counts()
		if snapshot.budget.UsedByteCount() != snapshot.used || snapshot.budget.TotalByteCount() != snapshot.total ||
			reserved != snapshot.reserved || released != snapshot.released {
			t.Fatalf("control changed full data custody: used=%d/%d total=%d/%d reserved=%d/%d released=%d/%d",
				snapshot.budget.UsedByteCount(), snapshot.used, snapshot.budget.TotalByteCount(), snapshot.total,
				reserved, snapshot.reserved, released, snapshot.released)
		}
	}
	for index := range self.dataResults {
		if self.dataResults[index].calls.Load() != 0 {
			t.Fatal("held data completed before delivery or child cancellation")
		}
	}
	var refused protocolPressureCompletion
	if admitted, err := self.send(t, 1, tcpFlagAck, 1240, false, &refused, 0); admitted || err != nil {
		t.Fatalf("full data leaf admitted another ordinary public payload: admitted=%t error=%v", admitted, err)
	}
	synctest.Wait()
	if refused.calls.Load() != 0 {
		t.Fatal("refused caller-owned data acquired a terminal callback")
	}
}

// Cancels only the pressure-producing children and joins their real workers.
func (self *protocolDataPressureFixture) cancelData(t *testing.T) {
	t.Helper()
	self.dataSender.cancel()
	self.dataReceiver.cancel()
	<-self.dataSender.done
	<-self.dataReceiver.done
	synctest.Wait()
	if !errors.Is(self.dataSender.ctx.Err(), context.Canceled) || !errors.Is(self.dataReceiver.ctx.Err(), context.Canceled) {
		t.Fatal("data children joined without the requested cancellation cause")
	}
	for index := range self.dataResults {
		result := &self.dataResults[index]
		if result.calls.Load() != 1 || result.canceled.Load() != 1 || result.success.Load() != 0 || result.other.Load() != 0 {
			t.Fatal("held data cancellation lost or repeated a terminal callback")
		}
	}
	for _, snapshot := range self.budgets {
		assertRetainedBudgetBalance(t, snapshot.budget)
	}
	if self.wire.sender.ctx.Err() != nil || self.wire.receiver.ctx.Err() != nil {
		t.Fatal("canceling data children terminated a shared client")
	}
}

// Lost pure controls retry and receive actual peer acknowledgements while all
// data leaves stay full. Canceling the data children leaves a healthy sibling.
func testProtocolControlNoBudgetRetriesThroughFullData(t *testing.T, flowIsolation bool) {
	t.Helper()
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		fixture := newProtocolDataPressureFixture(t, 4, flowIsolation)
		for index, flags := range []byte{tcpFlagAck, tcpFlagSyn, tcpFlagFin, tcpFlagRst} {
			var result protocolPressureCompletion
			var duplicate *windowRoundFrame
			if index == 0 {
				expected := ipOosTcpPacketSequence(reliableDeliveryTestPath(4), tcpFlagAck, 7000, nil)
				packet := MessagePoolCopy(expected)
				owned := true
				t.Cleanup(func() {
					if owned {
						MessagePoolReturn(packet)
					}
				})
				frame, err := ipPacketToProviderFrame(packet, DefaultProtocolVersion)
				if err != nil || !protocolOnlyFrame(frame.MessageType, frame.Raw, frame.MessageBytes) {
					t.Fatalf("control policy comparison did not construct a pure TCP ACK: %v", err)
				}
				var refused protocolPressureCompletion
				writes := fixture.wire.sender.initialSendWriteCount.Load()
				admitted, err := fixture.wire.sender.SendWithTimeoutDetailed(frame, fixture.wire.receiver.ClientId(), refused.record, 0, TransferKey{LogicalLane: 2})
				if admitted {
					owned = false
				}
				synctest.Wait()
				if admitted || err != nil || refused.calls.Load() != 0 || fixture.wire.sender.initialSendWriteCount.Load() != writes || !bytes.Equal(frame.MessageBytes, expected) {
					t.Fatalf("pure TCP ACK without NoBudget did not preserve caller custody on full-data refusal: admitted=%t error=%v", admitted, err)
				}
				fixture.requireDataPressure(t)
				// The exact refused frame, caller-owned root, and logical lane
				// remain the same. Only explicit local byte policy changes.
				admitted, err = fixture.wire.sender.SendWithTimeoutDetailed(frame, fixture.wire.receiver.ClientId(), result.record, time.Second, TransferKey{LogicalLane: 2}, NoBudget())
				if admitted {
					owned = false
				}
				if !admitted || err != nil {
					t.Fatalf("full data blocked the same public TCP ACK with NoBudget: admitted=%t error=%v", admitted, err)
				}
			} else if admitted, err := fixture.send(t, 2, flags, 0, true, &result, time.Second); !admitted || err != nil {
				t.Fatalf("full data blocked public NoBudget TCP control %d: admitted=%t error=%v", index, admitted, err)
			}
			wire := fixture.take(t, 2, uint64(index+1))
			if !protocolOnlyFrame(wire.pack.Frames[0].MessageType, wire.pack.Frames[0].Raw, wire.pack.Frames[0].MessageBytes) {
				t.Fatal("actual control wire was not a complete payload-free TCP packet")
			}
			if index == 0 {
				fixture.wire.drop(wire)
				fixture.requireDataPressure(t)
				// Advance exactly the configured retry ceiling in the virtual
				// protocol clock; no scheduler sleep determines the ordering.
				time.Sleep(fixture.wire.sender.settings.SendBufferSettings.MaxResendInterval)
				synctest.Wait()
				retry := fixture.take(t, 2, wire.pack.SequenceNumber)
				if !bytes.Equal(retry.pack.SequenceId, wire.pack.SequenceId) || !bytes.Equal(retry.pack.MessageId, wire.pack.MessageId) {
					t.Fatal("actual retry replaced the reliable control identity")
				}
				wire = retry
				duplicate = &windowRoundFrame{bytes: MessagePoolShareReadOnly(retry.bytes), pack: retry.pack}
				fixture.wire.heldFrames = append(fixture.wire.heldFrames, duplicate)
			}
			if result.calls.Load() != 0 {
				t.Fatal("unacknowledged control completed at initial write or retry")
			}
			fixture.wire.forward(wire, fixture.wire.receiverIn)
			if result.calls.Load() != 0 || fixture.wire.deliveredCount != index+3 {
				t.Fatal("control failed actual receive delivery or completed before peer acknowledgement")
			}
			fixture.wire.acknowledge()
			if result.calls.Load() != 1 || result.success.Load() != 1 {
				t.Fatal("peer acknowledgement did not complete the reliable control exactly once")
			}
			if duplicate != nil {
				fixture.wire.forward(duplicate, fixture.wire.receiverIn)
				fixture.wire.acknowledge()
				if result.calls.Load() != 1 || fixture.wire.deliveredCount != index+3 {
					t.Fatal("duplicate actual control wire repeated delivery or completion")
				}
			}
			fixture.requireDataPressure(t)
		}
		fixture.cancelData(t)
		var recovered protocolPressureCompletion
		if admitted, err := fixture.send(t, 2, tcpFlagAck, 1240, false, &recovered, time.Second); !admitted || err != nil {
			t.Fatalf("healthy sibling could not reuse refunded data capacity: %t %v", admitted, err)
		}
		fixture.wire.forward(fixture.take(t, 2, 5), fixture.wire.receiverIn)
		fixture.wire.acknowledge()
		if recovered.calls.Load() != 1 || recovered.success.Load() != 1 || fixture.wire.deliveredCount != 7 {
			t.Fatal("healthy sibling did not deliver and acknowledge ordinary data after refund")
		}
		for _, snapshot := range fixture.budgets {
			assertRetainedBudgetBalance(t, snapshot.budget)
		}
	})
}

// The ordinary carrier path must not hide protocol work behind a full resend leaf.
func TestProtocolControlNoBudgetRetriesThroughFullData(t *testing.T) {
	testProtocolControlNoBudgetRetriesThroughFullData(t, false)
}

// The same public custody and retry proof covers the actual flow-isolated route.
func TestProtocolControlNoBudgetRetriesThroughFlowIsolatedFullData(t *testing.T) {
	testProtocolControlNoBudgetRetriesThroughFullData(t, true)
}

// An unacknowledged exempt owner still belongs to its child lifecycle. Its
// cancellation cannot refund unrelated data or terminate a healthy sibling.
func TestProtocolControlNoBudgetCancellationKeepsFullDataSibling(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		fixture := newProtocolDataPressureFixture(t, 6, true)
		var canceled protocolPressureCompletion
		if admitted, err := fixture.send(t, 2, tcpFlagAck, 0, true, &canceled, time.Second); !admitted || err != nil {
			t.Fatalf("full data blocked cancellable control: %t %v", admitted, err)
		}
		wire := fixture.take(t, 2, 1)
		fixture.wire.drop(wire)
		fixture.wire.sender.sendBuffer.mutex.Lock()
		sequence := fixture.wire.sender.sendBuffer.sendSequencesBySequenceId[RequireIdFromBytes(wire.pack.SequenceId)]
		fixture.wire.sender.sendBuffer.mutex.Unlock()
		if sequence == nil || canceled.calls.Load() != 0 {
			t.Fatal("unacknowledged public control lost its live send owner")
		}
		sequence.cancel()
		<-sequence.done
		synctest.Wait()
		if !errors.Is(sequence.ctx.Err(), context.Canceled) || canceled.calls.Load() != 1 || canceled.canceled.Load() != 1 || canceled.success.Load() != 0 || canceled.other.Load() != 0 {
			t.Fatal("control child cancellation did not complete exactly once with cancellation")
		}
		fixture.requireDataPressure(t)
		var healthy protocolPressureCompletion
		if admitted, err := fixture.send(t, 3, tcpFlagAck, 0, true, &healthy, time.Second); !admitted || err != nil {
			t.Fatalf("canceled control child blocked healthy sibling: %t %v", admitted, err)
		}
		fixture.wire.forward(fixture.take(t, 3, 0), fixture.wire.receiverIn)
		fixture.wire.acknowledge()
		if healthy.calls.Load() != 1 || healthy.success.Load() != 1 || fixture.wire.deliveredCount != 3 {
			t.Fatal("healthy control sibling did not deliver and acknowledge under unchanged data pressure")
		}
		fixture.requireDataPressure(t)
		fixture.cancelData(t)
	})
}

// A real queued data original precedes the control before the cold worker is
// released. Both carrier selectors must leave that data unwritten and advance
// the exempt reliable control without changing any existing data reservation.
func testProtocolControlNoBudgetPassesQueuedData(t *testing.T, flowIsolation bool) {
	t.Helper()
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		entered, resume := make(chan struct{}), make(chan struct{})
		var resumeOnce sync.Once
		release := func() { resumeOnce.Do(func() { close(resume) }) }
		fixture := newProtocolDataPressureFixture(t, 4, flowIsolation, func(settings *SendBufferSettings) {
			settings.SequenceBufferSize = 4
			settings.beforeRunSendSequenceForTest = func(id sendSequenceId) {
				if id.LogicalLane == 3 {
					close(entered)
					<-resume
				}
			}
		})
		t.Cleanup(release)
		var queued, control protocolPressureCompletion
		if admitted, err := fixture.send(t, 3, tcpFlagAck, 1240, false, &queued, time.Second); !admitted || err != nil {
			t.Fatalf("cold public data did not enter its real bounded channel: %t %v", admitted, err)
		}
		<-entered
		if admitted, err := fixture.send(t, 3, tcpFlagAck, 0, true, &control, time.Second); !admitted || err != nil {
			t.Fatalf("queued data prevented public NoBudget control admission: %t %v", admitted, err)
		}
		fixture.wire.sender.sendBuffer.mutex.Lock()
		var sequence *SendSequence
		for _, candidate := range fixture.wire.sender.sendBuffer.sendSequencesBySequenceId {
			if candidate.id().LogicalLane == 3 {
				sequence = candidate
				break
			}
		}
		fixture.wire.sender.sendBuffer.mutex.Unlock()
		if sequence == nil || len(sequence.packs) != 2 || sequence.packAdmission.count != 1 ||
			sequence.protocolAdmission.count != 1 || queued.calls.Load() != 0 || control.calls.Load() != 0 {
			t.Fatal("worker barrier did not retain the earlier actual data original")
		}
		release()
		wire := fixture.take(t, 3, 0)
		if !protocolOnlyFrame(wire.pack.Frames[0].MessageType, wire.pack.Frames[0].Raw, wire.pack.Frames[0].MessageBytes) || queued.calls.Load() != 0 {
			t.Fatal("full data selector wrote the blocked payload before the control")
		}
		fixture.wire.forward(wire, fixture.wire.receiverIn)
		fixture.wire.acknowledge()
		if control.calls.Load() != 1 || control.success.Load() != 1 || queued.calls.Load() != 0 || fixture.wire.deliveredCount != 3 {
			t.Fatal("control behind queued data did not deliver and acknowledge independently")
		}
		fixture.requireDataPressure(t)
		sequence.cancel()
		<-sequence.done
		synctest.Wait()
		if !errors.Is(sequence.ctx.Err(), context.Canceled) || queued.calls.Load() != 1 || queued.canceled.Load() != 1 || queued.success.Load() != 0 || queued.other.Load() != 0 {
			t.Fatal("queued data cancellation lost its real caller-owned root or terminal callback")
		}
		fixture.requireDataPressure(t)
		fixture.cancelData(t)
	})
}

// The default non-isolating selector must inspect protocol work under pressure.
func TestProtocolControlNoBudgetPassesQueuedData(t *testing.T) {
	testProtocolControlNoBudgetPassesQueuedData(t, false)
}

// The flow-isolated selector must preserve the same exemption and cancellation.
func TestProtocolControlNoBudgetPassesFlowIsolatedQueuedData(t *testing.T) {
	testProtocolControlNoBudgetPassesQueuedData(t, true)
}
