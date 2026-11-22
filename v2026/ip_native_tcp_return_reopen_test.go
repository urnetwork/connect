package connect

import (
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

type nativeFinReopenFixture struct {
	fixture  *nativeDuplicateAckFixture
	sequence *TcpSequence
	edge     uint32
	lock     sync.Mutex
	fins     int
	lastFin  time.Time
	open     bool
	accepted bool
}

// This reachable zero-window owner is FIN, rather than fabricated unACKed
// DATA beyond the greatest advertised edge. All packets/ACKs traverse the
// real native/Multi/Transfer/provider stack and an actual origin EOF.
func newNativeFinReopenFixture(t *testing.T, construct func() *nativeDuplicateAckFixture) *nativeFinReopenFixture {
	t.Helper()
	f := construct()
	state := &nativeFinReopenFixture{fixture: f, sequence: f.providerSequence.Load()}
	f.parent.SetReceivePacketCallback(func(source TransferPath, mode protocol.ProvideMode, path *IpPath, packet []byte) {
		parsed, payload, err := ParseIpPathWithPayload(packet)
		if err != nil || parsed == nil {
			t.Errorf("native FIN callback parse: %v", err)
			return
		}
		if !parsed.Fin {
			f.consumer.receive(t, packet)
			return
		}
		state.lock.Lock()
		defer state.lock.Unlock()
		if len(payload) != 0 || parsed.SequenceNumber != state.edge || parsed.Rst || !parsed.Ack {
			t.Error("native return FIN changed its exact retained sequence-space byte")
			return
		}
		state.fins++
		state.lastFin = time.Now()
		state.accepted = state.accepted || state.open
	})
	if count, err := f.origin.Write([]byte("abcdefgh")); err != nil || count != 8 {
		t.Fatalf("real zero-window origin write: count=%d err=%v", count, err)
	}
	synctest.Wait()
	f.consumer.stateLock.Lock()
	state.edge = f.consumer.next
	body := f.consumer.received
	f.consumer.stateLock.Unlock()
	if body != "abcdefgh" {
		t.Fatal("native consumer did not receive the whole advertised eight-byte window")
	}
	state.offer(t, state.edge, 0)
	state.sequence.mutex.Lock()
	closed := state.sequence.receiveSeq == state.edge && state.sequence.receiveSeqAck == state.edge &&
		state.sequence.receiveWindowEnd == state.edge && state.sequence.receiveWindowSize == 0 && state.sequence.returnByteCount == 0
	state.sequence.mutex.Unlock()
	if !closed {
		t.Fatal("actual native ACK did not close the greatest edge and release all ordinary DATA")
	}
	if err := f.origin.Close(); err != nil {
		t.Fatal(err)
	}
	synctest.Wait()
	state.requireFins(t, 1)
	delay := state.sequence.tcpBufferSettings.ReturnResendTimeout
	time.Sleep(delay)
	synctest.Wait()
	state.requireFins(t, 2)
	time.Sleep(2 * delay)
	synctest.Wait()
	state.requireFins(t, 3)
	state.sequence.mutex.Lock()
	backedOff := state.sequence.returnAttempts == 2 && state.sequence.receiveSeqAck == state.edge &&
		state.sequence.receiveSeq == state.edge+1 && state.sequence.receiveWindowSize == 0 &&
		state.sequence.returnHead < len(state.sequence.returnChunks)
	if backedOff {
		chunk := state.sequence.returnChunks[state.sequence.returnHead]
		backedOff = chunk.fin && chunk.start == state.edge && chunk.end == state.edge+1 && len(chunk.payload) == 0
	}
	state.sequence.mutex.Unlock()
	if !backedOff {
		t.Fatal("actual EOF did not establish one retained FIN with a backed-off recovery timer")
	}
	return state
}

func (self *nativeFinReopenFixture) offer(t *testing.T, number uint32, window uint16) {
	t.Helper()
	if !self.fixture.offer(groupRecoveryPacket(self.fixture.initial+1, number, window, tcpFlagAck, nil)) {
		t.Fatal("actual native ACK/window update was refused")
	}
	synctest.Wait()
}

func (self *nativeFinReopenFixture) requireFins(t *testing.T, count int) {
	t.Helper()
	self.lock.Lock()
	defer self.lock.Unlock()
	if self.fins != count {
		t.Fatalf("native return FIN offers=%d, want=%d", self.fins, count)
	}
}

func (self *nativeFinReopenFixture) reopen(t *testing.T) {
	t.Helper()
	self.lock.Lock()
	self.open = true
	self.lock.Unlock()
	before := time.Now()
	self.offer(t, self.edge, 1)
	self.requireFins(t, 4)
	self.lock.Lock()
	prompt := self.accepted && self.lastFin.Equal(before)
	self.lock.Unlock()
	self.sequence.mutex.Lock()
	owned := self.sequence.receiveSeqAck == self.edge && self.sequence.returnByteCount > 0 &&
		self.sequence.returnAttempts == 1
	self.sequence.mutex.Unlock()
	if !prompt || !owned {
		t.Fatal("validated same-ACK reopen did not promptly replay exactly the retained FIN without a false ACK")
	}
}

func (self *nativeFinReopenFixture) finish(t *testing.T) {
	t.Helper()
	self.offer(t, self.edge+1, 1)
	self.sequence.mutex.Lock()
	retained := self.sequence.returnByteCount
	self.sequence.mutex.Unlock()
	if retained != 0 {
		t.Fatal("the actual final native ACK failed to release FIN custody")
	}
	self.fixture.close()
}

func TestNativeTcpReturnFinReopenWakesBackedOffRecovery(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, maxPackets := range []int{1, 4} {
		synctest.Test(t, func(t *testing.T) {
			state := newNativeFinReopenFixture(t, func() *nativeDuplicateAckFixture {
				return newNativeDuplicateAckFixtureWithWindow(t, false, 8, maxPackets)
			})
			defer state.fixture.close()
			state.reopen(t)
			state.finish(t)
		})
	}
}

func TestNativeTcpReturnFinReopenRejectsStaleFeedback(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		state := newNativeFinReopenFixture(t, func() *nativeDuplicateAckFixture {
			return newNativeDuplicateAckFixtureWithWindow(t, false, 8)
		})
		defer state.fixture.close()
		for _, invalid := range []uint32{state.edge - 1, state.edge + 2} {
			state.offer(t, invalid, 32)
			state.requireFins(t, 3)
			state.sequence.mutex.Lock()
			unchanged := state.sequence.receiveSeqAck == state.edge && state.sequence.receiveWindowSize == 0 &&
				state.sequence.returnAttempts == 2
			state.sequence.mutex.Unlock()
			if !unchanged {
				t.Fatal("stale/future ACK reset backoff or manufactured reopen feedback")
			}
		}
		state.reopen(t)
		// Additional positive updates and an older smaller Window header do not
		// create another readiness credit at the same cumulative ACK.
		state.offer(t, state.edge, 2)
		state.offer(t, state.edge, 0)
		state.requireFins(t, 4)
		state.finish(t)
	})
}

func TestNativeTcpWindowReopenWithoutRetainedReturnUsesReaderWake(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newNativeDuplicateAckFixtureWithWindow(t, false, 8)
		defer f.close()
		if count, err := f.origin.Write([]byte("abcdefgh")); err != nil || count != 8 {
			t.Fatalf("first native origin write: count=%d err=%v", count, err)
		}
		synctest.Wait()
		f.consumer.stateLock.Lock()
		edge := f.consumer.next
		f.consumer.stateLock.Unlock()
		if !f.offer(groupRecoveryPacket(f.initial+1, edge, 0, tcpFlagAck, nil)) {
			t.Fatal("native zero-window close was refused")
		}
		synctest.Wait()
		if count, err := f.origin.Write([]byte("next")); err != nil || count != 4 {
			t.Fatalf("unsent native origin read: count=%d err=%v", count, err)
		}
		synctest.Wait()
		sequence := f.providerSequence.Load()
		sequence.mutex.Lock()
		parked := sequence.receiveSeq == edge && sequence.receiveSeqAck == edge && sequence.receiveWindowSize == 0 &&
			sequence.returnHead == len(sequence.returnChunks) && sequence.returnPersistPending
		sequence.mutex.Unlock()
		if !parked {
			t.Fatal("real socket reader did not park unsent bytes without a retained DATA head")
		}
		before := time.Now()
		if !f.offer(groupRecoveryPacket(f.initial+1, edge, 4, tcpFlagAck, nil)) {
			t.Fatal("native reader-window reopen was refused")
		}
		synctest.Wait()
		f.consumer.stateLock.Lock()
		received, packets := f.consumer.received, f.consumer.packets
		f.consumer.stateLock.Unlock()
		sequence.mutex.Lock()
		ordinary := sequence.returnAttempts == 0 && !sequence.returnPersistPending
		sequence.mutex.Unlock()
		if received != "abcdefghnext" || packets != 3 || !ordinary || time.Since(before) != 0 {
			t.Fatal("existing reader-condition wake was replaced by a replay/probe or delayed for a timer")
		}
		if !f.offer(groupRecoveryPacket(f.initial+1, edge+4, 4, tcpFlagAck, nil)) {
			t.Fatal("final native DATA ACK was refused")
		}
		synctest.Wait()
	})
}

func TestNativeTcpReturnFinReopenCancellationJoinsCustody(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		state := newNativeFinReopenFixture(t, func() *nativeDuplicateAckFixture {
			return newNativeDuplicateAckFixtureWithWindow(t, false, 8)
		})
		state.fixture.close()
		if state.fixture.offer(groupRecoveryPacket(state.fixture.initial+1, state.edge, 1, tcpFlagAck, nil)) {
			t.Fatal("retired native owner accepted a reopen")
		}
		synctest.Wait()
		state.requireFins(t, 3)
	})
}
