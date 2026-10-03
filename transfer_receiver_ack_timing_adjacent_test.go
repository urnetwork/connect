// Receiver timing remains attached to one exact Pack across ACK coalescing.
package connect

import (
	"math"
	"testing"
	"testing/synctest"
	"time"
	"unsafe"
)

// A clock-origin ingress, sub-microsecond delay and the largest wire value
// remain distinct from absent, future or overflowing local timestamps.
func TestReceiverAckTimingDurationBounds(t *testing.T) {
	base := time.Unix(1700000000, 0)
	client := &Client{feedbackTimeBase: base}
	cases := []struct {
		name  string
		stamp int64
		at    time.Time
		want  uint32
		set   bool
	}{
		{name: "zero at origin", stamp: 1, at: base, set: true},
		{name: "floor fraction", stamp: 1, at: base.Add(999 * time.Nanosecond), set: true},
		{name: "one microsecond", stamp: 1, at: base.Add(time.Microsecond), want: 1, set: true},
		{name: "wire maximum", stamp: 1, at: base.Add(time.Duration(math.MaxUint32) * time.Microsecond), want: math.MaxUint32, set: true},
		{name: "overflow", stamp: 1, at: base.Add((time.Duration(math.MaxUint32) + 1) * time.Microsecond)},
		{name: "future ingress", stamp: 1001, at: base},
		{name: "before origin", stamp: 1, at: base.Add(-time.Nanosecond)},
		{name: "missing ingress", stamp: 0, at: base},
	}
	for _, c := range cases {
		got, set := client.receiverAckDelayMicros(c.stamp, c.at)
		if got != c.want || set != c.set {
			t.Errorf("%s: delay=(%d,%t), want (%d,%t)", c.name, got, set, c.want, c.set)
		}
	}
	if got := client.feedbackTimeNanos(base); got != 1 {
		t.Fatalf("clock-origin ingress encoded as %d, want 1", got)
	}
	if got := client.feedbackTimeNanos(base.Add(-time.Nanosecond)); got != 0 {
		t.Fatalf("negative clock-origin ingress became available: %d", got)
	}
	if unsafe.Sizeof(sequenceAck{}) != 96 || unsafe.Sizeof(decodedPackOwner{}) > uintptr(decodedPackOwnerQueueByteCount) {
		t.Fatalf("timing escaped existing compact ACK/owner bounds: ack=%d owner=%d", unsafe.Sizeof(sequenceAck{}), unsafe.Sizeof(decodedPackOwner{}))
	}
}

// A later cumulative head keeps its own ingress and tag while absorbing
// older heads and SACKs. Above-head SACK timing survives oldest-first output.
func TestReceiverAckTimingCoalescesExactHeadAndSelectiveIdentity(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		opened, release := make(chan struct{}), make(chan struct{})
		sequence := newAckGapTestSequence(t, func(settings *ReceiveBufferSettings) {
			settings.AckCompressTimeout = 0
			settings.afterAckWriterOpenForTest = func(receiveSequenceId, MultiRouteWriter) { close(opened); <-release }
		})
		<-opened
		client := sequence.receiveSequence.client
		firstStamp := client.feedbackTimeNanos(time.Now())
		sequence.receiveSequence.sendAckAt(1, NewId(), false, sequenceTag{sendTime: 11, set: true}, false, TransportTypeH1, firstStamp)
		sequence.receiveSequence.sendAckAt(2, NewId(), true, sequenceTag{sendTime: 22, set: true}, false, TransportTypeH1, firstStamp)
		time.Sleep(3 * time.Millisecond)
		laterStamp := client.feedbackTimeNanos(time.Now())
		headId, selectiveId := NewId(), NewId()
		sequence.receiveSequence.sendAckAt(3, headId, false, sequenceTag{sendTime: 33, set: true}, false, TransportTypeH1, laterStamp)
		sequence.receiveSequence.sendAckAt(4, selectiveId, true, sequenceTag{sendTime: 44, set: true}, false, TransportTypeH1, firstStamp)
		// The stale update cannot replace the new head's timing tuple.
		sequence.receiveSequence.sendAckAt(1, NewId(), false, sequenceTag{sendTime: 111, set: true}, false, TransportTypeH1, laterStamp)
		time.Sleep(2 * time.Millisecond)
		close(release)
		for _, c := range []struct {
			messageId Id
			tag       uint64
			delay     uint32
			selective bool
		}{
			{messageId: headId, tag: 33, delay: 2000},
			{messageId: selectiveId, tag: 44, delay: 5000, selective: true},
		} {
			ack := readBurstTailTestAck(t, sequence)
			got, err := IdFromBytes(ack.MessageId)
			if err != nil || got != c.messageId || ack.Tag == nil || ack.Tag.SendTime != c.tag || ack.ReceiverAckDelayMicros == nil || *ack.ReceiverAckDelayMicros != c.delay || ack.Selective != c.selective {
				t.Fatalf("coalesced timing no longer names the encoded Pack: %v", ack)
			}
		}
		if len(sequence.route) != 0 {
			t.Fatal("absorbed SACK escaped the head")
		}
	})
}

// The same timing contract survives both wire formats and outer encryption.
// A retry with no echoed tag has no sample even when a local stamp is supplied.
func TestReceiverAckTimingWrappersAndUntaggedRecovery(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, version := range []int{1, 2} {
		for _, encrypted := range []bool{false, true} {
			synctest.Test(t, func(t *testing.T) {
				sequence := newAckGapTestSequence(t, func(settings *ReceiveBufferSettings) {
					settings.ProtocolVersion = version
					settings.AckCompressTimeout = 0
				})
				synctest.Wait()
				if encrypted {
					sequence.receiveSequence.session = &peerEncryptionSession{client: sequence.receiveSequence.client, role: sequenceTlsRoleServer, companion: true, establishedEpoch: &tlsHandshakeEpoch{peerIdentityVerified: true, derivedTlsCipher: newFrameCodecTestSequenceCipher(t)}}
					t.Cleanup(func() { sequence.receiveSequence.session = nil })
				}
				stamp := sequence.receiveSequence.client.feedbackTimeNanos(time.Now())
				time.Sleep(1250*time.Microsecond + 999*time.Nanosecond)
				sequence.receiveSequence.sendAckAt(1, NewId(), false, sequenceTag{sendTime: 100, set: true}, false, TransportTypeH1, stamp)
				ack := readBurstTailTestAck(t, sequence)
				if ack.ReceiverAckDelayMicros == nil || *ack.ReceiverAckDelayMicros != 1250 {
					t.Fatalf("version=%d encrypted=%t lost exact delay", version, encrypted)
				}
				sequence.receiveSequence.sendAckAt(2, NewId(), false, sequenceTag{}, false, TransportTypeH1, stamp)
				ack = readBurstTailTestAck(t, sequence)
				if ack.Tag != nil || ack.ReceiverAckDelayMicros != nil {
					t.Fatal("untagged recovery invented a physical-copy RTT sample")
				}
			})
		}
	}
}

// Repeated receiver-state feedback re-encodes elapsed time for the original
// head. It cannot reuse the older encoded duration after another interval.
func TestReceiverAckTimingRepeatedHeadUsesCurrentEncodingTime(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		sequence := newAckGapTestSequence(t, func(settings *ReceiveBufferSettings) { settings.AckCompressTimeout = 10 * time.Millisecond })
		stamp := sequence.receiveSequence.client.feedbackTimeNanos(time.Now())
		headId := NewId()
		sequence.receiveSequence.sendAckAt(1, headId, false, sequenceTag{sendTime: 10, set: true}, false, TransportTypeH1, stamp)
		ack := readBurstTailTestAck(t, sequence)
		if ack.ReceiverAckDelayMicros == nil || *ack.ReceiverAckDelayMicros != 0 {
			t.Fatal("first head did not retain zero receiver delay")
		}
		time.Sleep(3 * time.Millisecond)
		sequence.receiveSequence.noteEviction(5)
		sequence.receiveSequence.sendAck(1, headId, false, sequenceTag{}, false, TransportTypeH1)
		time.Sleep(7 * time.Millisecond)
		ack = readBurstTailTestAck(t, sequence)
		got, err := IdFromBytes(ack.MessageId)
		if err != nil || got != headId || ack.ReceiverAckDelayMicros == nil || *ack.ReceiverAckDelayMicros != 10000 || len(ack.EvictedSequenceNumbers) != 1 {
			t.Fatal("repeated head retained stale encoded delay or changed identity")
		}
	})
}
