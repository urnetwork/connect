// A send caller owns only its unadmitted Pack. Cancellation must preserve the
// shared destination worker and every buffer already admitted by that worker.
package connect

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Pause real data workers before Run, after their real admission state is
// constructed. No provider, ACK policy, route or socket fixture participates.
type sendPackCallerOwnerFixture struct {
	client   *Client
	id       sendSequenceId
	sequence *SendSequence
}

// The hook is immutable before NewClient starts even its control publisher.
// Client cancellation releases paused owners and joins their final drains.
func newSendPackCallerOwnerFixture(t *testing.T) *sendPackCallerOwnerFixture {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	settings := closeWaitClientSettings()
	settings.SendBufferSettings.SequenceBufferSize = 2
	settings.SendBufferSettings.beforeRunSendSequenceForTest = func(id sendSequenceId) {
		if id.Destination != ControlId {
			<-ctx.Done()
		}
	}
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
	fixture := &sendPackCallerOwnerFixture{
		client: client,
		id:     sendSequenceId{Destination: NewId(), EncryptionRole: sequenceTlsRoleClient},
	}
	t.Cleanup(func() {
		cancel()
		if err := client.CloseAndWait(context.Background()); err != nil {
			t.Errorf("send caller fixture close: %v", err)
		}
	})
	return fixture
}

// Read the generation under its actual replacement lock.
func (self *sendPackCallerOwnerFixture) currentSequence() *SendSequence {
	self.client.sendBuffer.mutex.Lock()
	defer self.client.sendBuffer.mutex.Unlock()
	return self.client.sendBuffer.sendSequences[self.id]
}

// A successful Pack takes the pooled bytes; every failure leaves them here.
func (self *sendPackCallerOwnerFixture) send(ctx context.Context, value string, timeout time.Duration) (*SendPack, bool, error) {
	bytes := MessagePoolCopy([]byte(value))
	pack := &SendPack{
		TransferOptions: TransferOptions{Ack: true},
		Frame:           &protocol.Frame{MessageBytes: bytes}, MessageByteCount: ByteCount(len(bytes)),
		Destination: self.id.Destination, EncryptionRole: self.id.EncryptionRole,
		Ctx: ctx, logicalLaneExplicit: true,
	}
	accepted, err := self.client.sendBuffer.Pack(pack, timeout)
	if !accepted {
		MessagePoolReturn(bytes)
	}
	return pack, accepted, err
}

// Two real accepted Packs occupy the exact fixed admission budget.
func (self *sendPackCallerOwnerFixture) fill(t *testing.T) {
	t.Helper()
	for _, value := range []string{"accepted-first", "accepted-second"} {
		if _, accepted, err := self.send(self.client.ctx, value, 0); !accepted || err != nil {
			t.Fatalf("send sibling admission: %t %v", accepted, err)
		}
	}
	self.sequence = self.currentSequence()
	if self.sequence == nil || len(self.sequence.packs) != 2 || self.sequence.packAdmission.count != 2 {
		t.Fatal("send fixture did not occupy its two admitted slots")
	}
}

// A caller-only failure must preserve generation, admission and exact bytes.
func (self *sendPackCallerOwnerFixture) requireSiblings(t *testing.T) {
	t.Helper()
	if self.currentSequence() != self.sequence || self.sequence.ctx.Err() != nil {
		t.Error("canceled send caller retired the healthy shared destination")
	}
	if len(self.sequence.packs) != 2 || self.sequence.packAdmission.count != 2 {
		t.Errorf("accepted sibling admission changed: queued=%d admitted=%d", len(self.sequence.packs), self.sequence.packAdmission.count)
	}
	for _, expected := range []string{"accepted-first", "accepted-second"} {
		select {
		case pack := <-self.sequence.packs:
			if string(pack.Frame.MessageBytes) != expected {
				t.Errorf("send sibling bytes changed: %q", pack.Frame.MessageBytes)
			}
			pack.returnFrames()
			pack.releaseRaw()
		default:
			t.Error("accepted send sibling disappeared before its consumer")
		}
	}
}

// Pre-canceled admission is not evidence that the shared destination closed.
func TestSendBufferCallerCancelPreservesSharedAdmission(t *testing.T) {
	assertMessagePoolOwnership(t)
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		fixture := newSendPackCallerOwnerFixture(t)
		fixture.fill(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, accepted, err := fixture.send(ctx, "canceled-unadmitted", 0)
		if accepted || err == nil {
			t.Errorf("canceled send caller admission: %t %v", accepted, err)
		}
		fixture.requireSiblings(t)
	})
}

// A cancellation during the actual count wait retains caller-only scope.
func TestSendBufferCallerCancelDuringAdmissionKeepsSiblings(t *testing.T) {
	assertMessagePoolOwnership(t)
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		fixture := newSendPackCallerOwnerFixture(t)
		fixture.fill(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		type result struct {
			accepted bool
			err      error
		}
		done := make(chan result, 1)
		go func() {
			_, accepted, err := fixture.send(ctx, "blocked-unadmitted", -1)
			done <- result{accepted: accepted, err: err}
		}()
		synctest.Wait()
		select {
		case <-done:
			t.Fatal("the full two-slot send lane did not retain its unadmitted caller")
		default:
		}
		cancel()
		got := <-done
		if got.accepted || got.err == nil {
			t.Errorf("canceled blocked send admission: %t %v", got.accepted, got.err)
		}
		fixture.requireSiblings(t)
	})
}

// Success already transfers the first Pack. A canceled next send cannot
// retract its admitted predecessor, even when both share the same caller.
func TestSendBufferCallerCancelKeepsAcceptedPackOwned(t *testing.T) {
	assertMessagePoolOwnership(t)
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		fixture := newSendPackCallerOwnerFixture(t)
		ctx, cancel := context.WithCancel(context.Background())
		first, accepted, err := fixture.send(ctx, "accepted-caller", 0)
		if !accepted || err != nil {
			cancel()
			t.Fatalf("first caller was not admitted: %t %v", accepted, err)
		}
		original := fixture.currentSequence()
		cancel()
		_, accepted, err = fixture.send(ctx, "canceled-next", 0)
		if accepted || err == nil || fixture.currentSequence() != original || original.ctx.Err() != nil {
			t.Error("the canceled next send retired its admitted predecessor's lane")
		}
		select {
		case owned := <-original.packs:
			if owned != first || string(owned.Frame.MessageBytes) != "accepted-caller" {
				t.Error("admitted Pack ownership or bytes changed")
			}
			owned.returnFrames()
			owned.releaseRaw()
		default:
			t.Error("admitted Pack was reclaimed before its consumer")
		}
	})
}

// Real sequence closure remains a generation failure; a live caller must
// recreate it and transfer its Pack into the replacement's fixed admission.
func TestSendBufferClosedSequenceStillRecreatesForLiveCaller(t *testing.T) {
	assertMessagePoolOwnership(t)
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		fixture := newSendPackCallerOwnerFixture(t)
		fixture.fill(t)
		fixture.sequence.Cancel()
		pack, accepted, err := fixture.send(fixture.client.ctx, "replacement-caller", 0)
		replacement := fixture.currentSequence()
		if !accepted || err != nil || replacement == nil || replacement == fixture.sequence {
			t.Fatalf("live caller failed closed-sequence replacement: %t %v", accepted, err)
		}
		select {
		case owned := <-replacement.packs:
			if owned != pack || string(owned.Frame.MessageBytes) != "replacement-caller" {
				t.Error("replacement admitted different Pack ownership")
			}
			owned.returnFrames()
			owned.releaseRaw()
		default:
			t.Error("replacement lost admitted caller ownership")
		}
	})
}
