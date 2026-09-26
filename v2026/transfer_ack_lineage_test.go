//go:build acklineagetrace

package connect

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
	"unsafe"

	"github.com/urnetwork/connect/v2026/protocol"
)

// A first-event recorder, not a tail ring: missing evidence is actionable only
// if every reservation was published and no slot overflowed. Each producer
// owns one slot, so packet callbacks never wait, allocate, format or retain a
// packet. Snapshot requires quiescence. This test-only instrument is not a
// production performance or memory measurement.
const ackLineageCapacity = 256

type ackLineageTrace struct {
	next      atomic.Uint64
	events    [ackLineageCapacity]TransferProgressEvent
	published [ackLineageCapacity]atomic.Bool
}

func (self *ackLineageTrace) observe(event TransferProgressEvent) {
	index := self.next.Add(1) - 1
	if index < ackLineageCapacity {
		self.events[index] = event
		self.published[index].Store(true)
	}
}

func (self *ackLineageTrace) snapshot() ([]TransferProgressEvent, error) {
	count := self.next.Load()
	if count > ackLineageCapacity {
		return nil, fmt.Errorf("lineage overflow: %d > %d", count, ackLineageCapacity)
	}
	for index := range count {
		if !self.published[index].Load() {
			return nil, fmt.Errorf("lineage slot %d not published", index)
		}
	}
	return self.events[:count], nil
}

func (self *ackLineageTrace) configure(settings *ClientSettings) {
	settings.SendBufferSettings.ProgressObserver = self.observe
	settings.ReceiveBufferSettings.ProgressObserver = self.observe
}

// Follow a specific original owner, allowing only a same-sequence cumulative
// ACK of a known later message to cover it. A selective ACK or contract request
// never establishes application delivery. Successful route admission and
// successful sender handoff remain separate from coalescer publication.
func (self *ackLineageTrace) firstMissing(sender, receiver, sequence, message Id, number uint64, physical ...bool) (string, error) {
	events, err := self.snapshot()
	if err != nil {
		return "invalid_observer", err
	}
	for _, stage := range []string{
		"send_attempt", "receive_pack_begin", "deliver_item", "ack_write_begin",
		"ack_write_end", "ack_physical_write_begin", "ack_physical_read", "ack_physical_write_end",
		"receive_ack_begin", "receive_ack_end", "ack_coalesce_end", "terminal",
	} {
		physicalStage := stage == "ack_physical_write_begin" || stage == "ack_physical_read" || stage == "ack_physical_write_end"
		if physicalStage && (len(physical) == 0 || !physical[0]) {
			continue
		}
		found := false
		for _, event := range events {
			if event.Stage != stage || event.SequenceId != sequence {
				continue
			}
			from, to := sender, receiver
			if stage == "receive_pack_begin" || stage == "deliver_item" || stage == "ack_write_begin" || stage == "ack_write_end" ||
				stage == "ack_physical_write_begin" || stage == "ack_physical_write_end" {
				from, to = receiver, sender
			}
			if event.ClientId != from || event.PeerId != to {
				continue
			}
			ackStage := physicalStage || stage == "ack_write_begin" || stage == "ack_write_end" ||
				stage == "receive_ack_begin" || stage == "receive_ack_end" || stage == "ack_coalesce_end"
			if ackStage {
				if event.Selective || (stage == "ack_coalesce_end" && event.Outcome != "cumulative") {
					continue
				}
				knownCover := false
				for _, sent := range events {
					if sent.Stage == "send_attempt" && sent.ClientId == sender && sent.PeerId == receiver &&
						sent.SequenceId == sequence && sent.MessageId == event.MessageId && sent.SequenceNumber >= number {
						knownCover = true
						break
					}
				}
				if !knownCover {
					continue
				}
				if physicalStage {
					matchedWire := false
					for _, generated := range events {
						if generated.Stage == "ack_write_begin" && generated.ClientId == receiver && generated.PeerId == sender &&
							generated.SequenceId == sequence && generated.MessageId == event.MessageId &&
							generated.WireHash != 0 && generated.WireHash == event.WireHash && generated.ByteCount == event.ByteCount {
							matchedWire = true
							break
						}
					}
					if !matchedWire {
						continue
					}
				}
			} else if event.MessageId != message {
				continue
			}
			if (stage == "ack_physical_read" || stage == "ack_physical_write_end" || stage == "deliver_item" || stage == "ack_write_end" || stage == "receive_ack_end" ||
				stage == "ack_coalesce_end" || stage == "terminal") && !event.Success {
				continue
			}
			found = true
			break
		}
		if !found {
			return stage, nil
		}
	}
	return "complete", nil
}

type ackLineageTerminal struct {
	at  time.Time
	err error
}

// The test owns one logical offer until admission and captures its immutable
// wire identity before any peer can answer. This terminal seam belongs to the
// fixture; it does not claim the public lifecycle token already contains UUIDs.
func ackLineageSend(t *testing.T, fixture *windowRoundFixture, trace *ackLineageTrace, number uint64) (*windowRoundFrame, <-chan ackLineageTerminal) {
	t.Helper()
	terminal := make(chan ackLineageTerminal, 4)
	var identity atomic.Pointer[TransferProgressEvent]
	frame := budgetTestFrame(64)
	admitted, err := fixture.sender.SendWithTimeoutDetailed(frame, fixture.receiver.ClientId(), func(err error) {
		var event TransferProgressEvent
		if published := identity.Load(); published != nil {
			event = *published
		}
		event.Stage, event.AtUnixNano, event.Success = "terminal", time.Now().UnixNano(), err == nil
		event.ErrorKind = transferProgressErrorKind(err)
		trace.observe(event)
		terminal <- ackLineageTerminal{at: time.Now(), err: err}
	}, time.Second)
	if !admitted || err != nil {
		MessagePoolReturn(frame.MessageBytes)
		t.Fatalf("lineage offer admission=%t err=%v", admitted, err)
	}
	wire := fixture.takePack(number)
	identity.Store(&TransferProgressEvent{
		ClientId: fixture.sender.ClientId(), PeerId: fixture.receiver.ClientId(),
		SequenceId: RequireIdFromBytes(wire.pack.SequenceId), MessageId: RequireIdFromBytes(wire.pack.MessageId),
		SequenceNumber: wire.pack.SequenceNumber,
	})
	return wire, terminal
}

func requireAckLineageBoundary(t *testing.T, trace *ackLineageTrace, fixture *windowRoundFixture, wire *windowRoundFrame, expected string, physical ...bool) {
	t.Helper()
	synctest.Wait()
	got, err := trace.firstMissing(fixture.sender.ClientId(), fixture.receiver.ClientId(),
		RequireIdFromBytes(wire.pack.SequenceId), RequireIdFromBytes(wire.pack.MessageId), wire.pack.SequenceNumber, physical...)
	if err != nil || got != expected {
		t.Fatalf("first missing boundary=%s err=%v, want %s", got, err, expected)
	}
}

// All arms use real wire decoding, ReceiveSequence delivery/ACK compression,
// sender handoff, coalescer and terminal ownership. The held boundary is an
// explicit fixture fault, never evidence about the historical round-04 UUIDs.
func TestTransferAckLineageHeldBoundary(t *testing.T) {
	for _, carrier := range []TransportType{TransportTypeH1, TransportTypeP2p} {
		for _, version := range []int{1, 2} {
			for _, boundary := range []string{"forward", "callback", "reply_route", "reply_wire", "covering_reply"} {
				for _, releaseAt := range []time.Duration{29 * time.Second, 31 * time.Second} {
					t.Run(fmt.Sprintf("%s/v%d/%s/%s", carrier, version, boundary, releaseAt), func(t *testing.T) {
						assertMessagePoolOwnership(t)
						synctest.Test(t, func(t *testing.T) {
							trace := &ackLineageTrace{}
							fixture, _, _ := newAckRetirementFixture(t, carrier, version, NewNoopLogger(), trace.configure)
							release := make(chan struct{})
							released := false
							defer func() {
								if !released {
									close(release)
								}
							}()
							if boundary == "callback" {
								fixture.receiver.AddReceiveCallback(func(TransferPath, []*protocol.Frame, Peer) { <-release })
							} else if boundary == "reply_route" {
								for range cap(fixture.receiverOut) {
									fixture.receiverOut <- nil
								}
							}
							// No physical or admission delay elapses before this write.
							// Retain the test clock, never a mutable pooled sendItem.
							started := time.Now()
							wire, terminal := ackLineageSend(t, fixture, trace, 0)
							sequence := fixture.sequence()
							if sequence.sendBufferSettings.AckTimeout != 30*time.Second {
								t.Fatalf("changed production lifetime: %s", sequence.sendBufferSettings.AckTimeout)
							}
							var ack *windowRoundFrame
							var coveredTerminal <-chan ackLineageTerminal
							expectedBoundary, deliveries := "receive_ack_begin", 1
							switch boundary {
							case "forward":
								expectedBoundary = "receive_pack_begin"
							case "callback":
								fixture.forward(wire, fixture.receiverIn)
								expectedBoundary = "deliver_item"
							case "reply_route":
								fixture.forward(wire, fixture.receiverIn)
								expectedBoundary = "ack_write_end"
							case "covering_reply":
								second, done := ackLineageSend(t, fixture, trace, 1)
								coveredTerminal = done
								fixture.drop(fixture.receive(wire))
								ack = fixture.receive(second)
								if RequireIdFromBytes(ack.ack.MessageId) != RequireIdFromBytes(second.pack.MessageId) {
									t.Fatal("covering ACK does not name the later retained message")
								}
								deliveries = 2
							default:
								ack = fixture.receive(wire)
							}
							time.Sleep(time.Until(started.Add(29 * time.Second)))
							requireAckLineageBoundary(t, trace, fixture, wire, expectedBoundary)
							if len(terminal) != 0 {
								t.Fatal("sender terminated before the unchanged ACK lifetime")
							}
							time.Sleep(time.Until(started.Add(releaseAt)))
							if boundary == "reply_route" {
								for range cap(fixture.receiverOut) {
									if placeholder := <-fixture.receiverOut; placeholder != nil {
										MessagePoolReturn(placeholder)
										t.Fatal("held reply route admitted an ACK early")
									}
								}
							}
							close(release)
							released = true
							if boundary == "forward" {
								ack = fixture.receive(wire)
							} else if ack == nil {
								ack = fixture.take(fixture.receiverOut)
							}
							fixture.forward(ack, fixture.senderIn)
							time.Sleep(time.Until(started.Add(31 * time.Second)))
							synctest.Wait()
							if len(terminal) != 1 {
								t.Fatalf("terminal callbacks=%d, want one", len(terminal))
							}
							result := <-terminal
							if releaseAt < 30*time.Second {
								if result.err != nil || result.at != started.Add(releaseAt) {
									t.Fatalf("timely reply: at=%s err=%v", result.at.Sub(started), result.err)
								}
								requireAckLineageBoundary(t, trace, fixture, wire, "complete")
							} else if result.err == nil || result.at != started.Add(30*time.Second) {
								t.Fatalf("late reply changed original deadline: at=%s err=%v", result.at.Sub(started), result.err)
							}
							if coveredTerminal != nil {
								covered := <-coveredTerminal
								if (covered.err == nil) != (result.err == nil) || covered.at != result.at {
									t.Fatal("covering ACK did not resolve both original owners together")
								}
							}
							if fixture.deliveredCount != deliveries || sequence.resendQueue.Len() != 0 || len(sequence.ackLifetimes.items) != 0 {
								t.Fatal("duplicate delivery or retained recovery ownership")
							}
							t.Logf("first_missing=%s release=%s terminal=%s success=%t deliveries=%d events=%d", expectedBoundary,
								releaseAt, result.at.Sub(started), result.err == nil, fixture.deliveredCount, trace.next.Load())
						})
					})
				}
			}
		}
	}
}

func TestTransferAckLineagePublishedFeedbackWaitsForOwner(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		trace := &ackLineageTrace{}
		release := make(chan struct{})
		released := false
		defer func() {
			if !released {
				close(release)
			}
		}()
		logger := &ackRewriteBarrierLogger{Logger: NewNoopLogger(), reached: make(chan struct{}), release: release}
		fixture, _, _ := newAckRetirementFixture(t, TransportTypeH1, 2, logger, trace.configure)
		first, firstTerminal := ackLineageSend(t, fixture, trace, 0)
		second, terminal := ackLineageSend(t, fixture, trace, 1)
		fixture.forward(fixture.receive(first), fixture.senderIn)
		ack := fixture.receive(second)
		<-logger.reached
		if firstResult := <-firstTerminal; firstResult.err != nil {
			t.Fatal(firstResult.err)
		}
		fixture.forward(ack, fixture.senderIn)
		requireAckLineageBoundary(t, trace, fixture, second, "terminal")
		if len(terminal) != 0 {
			t.Fatal("held owner completed before release")
		}
		close(release)
		released = true
		synctest.Wait()
		if result := <-terminal; result.err != nil {
			t.Fatal(result.err)
		}
		requireAckLineageBoundary(t, trace, fixture, second, "complete")
	})
}

func TestTransferAckLineageCumulativeBatchPreservesEveryOwner(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		trace := &ackLineageTrace{}
		fixture, _, _ := newAckRetirementFixture(t, TransportTypeH1, 2, NewNoopLogger(), trace.configure)
		first, firstTerminal := ackLineageSend(t, fixture, trace, 0)
		fixture.forward(fixture.receive(first), fixture.senderIn)
		if result := <-firstTerminal; result.err != nil {
			t.Fatal(result.err)
		}
		second, secondTerminal := ackLineageSend(t, fixture, trace, 1)
		third, thirdTerminal := ackLineageSend(t, fixture, trace, 2)
		selective := fixture.receive(third)
		if !selective.ack.Selective {
			t.Fatal("out-of-order third message did not enter the receive hold")
		}
		fixture.drop(selective)
		cumulative := fixture.receive(second)
		if cumulative.ack.Selective || RequireIdFromBytes(cumulative.ack.MessageId) != RequireIdFromBytes(third.pack.MessageId) {
			t.Fatal("batch did not coalesce at its final delivered message")
		}
		fixture.forward(cumulative, fixture.senderIn)
		for _, terminal := range []<-chan ackLineageTerminal{secondTerminal, thirdTerminal} {
			if result := <-terminal; result.err != nil {
				t.Fatal(result.err)
			}
		}
		requireAckLineageBoundary(t, trace, fixture, second, "complete")
		requireAckLineageBoundary(t, trace, fixture, third, "complete")
		batchBegins, batchItems := 0, 0
		events, err := trace.snapshot()
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range events {
			if event.SequenceNumber < 1 {
				continue
			}
			if event.Stage == "deliver_begin" {
				batchBegins++
			}
			if event.Stage == "deliver_item" {
				batchItems++
			}
		}
		if batchBegins != 1 || batchItems != 2 || fixture.deliveredCount != 3 {
			t.Fatalf("batch callbacks=%d exact items=%d app deliveries=%d", batchBegins, batchItems, fixture.deliveredCount)
		}
	})
}

func TestTransferAckLineageObserverRejectsLostEvidence(t *testing.T) {
	trace := &ackLineageTrace{}
	if allocations := testing.AllocsPerRun(100, func() {
		trace.next.Store(0)
		trace.observe(TransferProgressEvent{Stage: "sent"})
	}); allocations != 0 {
		t.Fatalf("observer allocations=%g", allocations)
	}
	trace.next.Store(ackLineageCapacity)
	trace.observe(TransferProgressEvent{})
	if _, err := trace.snapshot(); err == nil {
		t.Fatal("overflow masquerades as a missing boundary")
	}
	trace = &ackLineageTrace{}
	trace.next.Store(1)
	if _, err := trace.snapshot(); err == nil {
		t.Fatal("unpublished slot masquerades as a missing boundary")
	}
	t.Logf("capacity=%d bytes=%d payloads=0 hot_allocations=0", ackLineageCapacity, unsafe.Sizeof(*trace))
}

func TestTransferAckLineageObserverConcurrentPublication(t *testing.T) {
	trace := &ackLineageTrace{}
	var workers sync.WaitGroup
	for worker := range 4 {
		workers.Go(func() {
			for index := range ackLineageCapacity / 4 {
				trace.observe(TransferProgressEvent{SequenceNumber: uint64(worker*ackLineageCapacity/4 + index)})
			}
		})
	}
	workers.Wait()
	events, err := trace.snapshot()
	if err != nil || len(events) != ackLineageCapacity {
		t.Fatalf("concurrent observer count=%d err=%v", len(events), err)
	}
	var seen [ackLineageCapacity]bool
	for _, event := range events {
		if event.SequenceNumber >= ackLineageCapacity || seen[event.SequenceNumber] {
			t.Fatal("concurrent observer lost slot ownership")
		}
		seen[event.SequenceNumber] = true
	}
}

// Corrupt only a diagnostic copy of a real completed path. The analyzer must
// not borrow a peer, sequence, unknown UUID, selective ACK, contract request or
// another terminal owner to fill the target's missing boundary.
func TestTransferAckLineageObserverRejectsFalseJoins(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		trace := &ackLineageTrace{}
		fixture, _, _ := newAckRetirementFixture(t, TransportTypeH1, 2, NewNoopLogger(), trace.configure)
		wire, terminal := ackLineageSend(t, fixture, trace, 0)
		read := ackLineagePhysicalReply(t, fixture, trace, TransportTypeH1, fixture.receive(wire))
		read()
		if result := <-terminal; result.err != nil {
			t.Fatal(result.err)
		}
		requireAckLineageBoundary(t, trace, fixture, wire, "complete", true)
		events, err := trace.snapshot()
		if err != nil {
			t.Fatal(err)
		}
		for _, kind := range []string{"wrong_sequence", "wrong_peer", "unknown_message", "foreign_cover", "selective", "contract", "other_terminal", "wrong_wire"} {
			copyTrace := &ackLineageTrace{}
			other := NewId()
			expected := "ack_write_begin"
			for _, original := range events {
				event := original
				ackStage := strings.HasPrefix(event.Stage, "ack_") || strings.HasPrefix(event.Stage, "receive_ack_")
				if ackStage {
					switch kind {
					case "wrong_sequence":
						event.SequenceId = other
					case "wrong_peer":
						event.PeerId = other
					case "unknown_message", "foreign_cover":
						event.MessageId = other
					case "selective":
						event.Selective = true
					case "contract":
						if event.Stage == "ack_coalesce_end" {
							event.Outcome = "contract_request"
						}
						expected = "ack_coalesce_end"
					case "wrong_wire":
						if event.Stage == "ack_physical_read" {
							event.WireHash ^= 1
						}
						expected = "ack_physical_read"
					}
				}
				if kind == "other_terminal" && event.Stage == "terminal" {
					event.MessageId, expected = other, "terminal"
				}
				copyTrace.observe(event)
				if kind == "foreign_cover" && event.Stage == "send_attempt" {
					event.MessageId, event.SequenceId, event.SequenceNumber = other, NewId(), 100
					copyTrace.observe(event)
				}
			}
			t.Logf("rejected=%s first_missing=%s", kind, expected)
			requireAckLineageBoundary(t, copyTrace, fixture, wire, expected, true)
		}
	})
}
