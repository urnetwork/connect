//go:build acklineagetrace

package connect

import (
	"context"
	"fmt"
	"net"
	"testing"
	"testing/synctest"
	"time"
)

// One captured, real Transfer ACK passes through the production H1 framed
// writer or legacy P2P send worker to an owned net.Pipe peer. There is no TCP,
// TLS, SCTP, ICE, simulator link or host-socket claim here. Identity is pinned
// before publication and joined to the actual carrier bytes by checksum; the
// observer itself owns no bytes. Each call owns one physical generation.
func ackLineagePhysicalReply(t *testing.T, fixture *windowRoundFixture, trace *ackLineageTrace, carrier TransportType, ack *windowRoundFrame) func() {
	t.Helper()
	left, right := net.Pipe()
	identity := TransferProgressEvent{
		ClientId: fixture.receiver.ClientId(), PeerId: fixture.sender.ClientId(),
		SequenceId: RequireIdFromBytes(ack.ack.SequenceId), MessageId: RequireIdFromBytes(ack.ack.MessageId),
		TransportType: carrier,
	}
	ctx, cancel := context.WithCancel(fixture.ctx)
	var read func() ([]byte, error)
	if carrier == TransportTypeH1 {
		writer, err := NewFramedMessageConn(left, H1FramerProtocol, 1200, nil)
		if err != nil {
			t.Fatal(err)
		}
		reader, err := NewFramedMessageConn(right, H1FramerProtocol, 1200, nil)
		if err != nil {
			t.Fatal(err)
		}
		read = func() ([]byte, error) { _, wire, err := reader.ReadPooledMessage(); return wire, err }
		done := make(chan struct{})
		wire := ack.bytes
		ack.bytes = nil
		go func() {
			defer close(done)
			event := identity
			event.Stage = "ack_physical_write_begin"
			event = beginTransferProgress(trace.observe, event, wire)
			_, err := writeH1FramedReadyBatch(ctx, writer, nil, nil, wire, false, DefaultPlatformTransportSettings().WriteTimeout, nil)
			endTransferProgress(trace.observe, event, "ack_physical_write_end", err == nil, err)
		}()
		t.Cleanup(func() { cancel(); writer.Close(); reader.Close(); <-done })
	} else {
		settings := DefaultP2pTransportSettings()
		settings.DataPlaneMode = P2pDataPlaneModeLegacyOnly
		settings.ProgressObserver = func(event TransferProgressEvent) {
			if event.Stage != "p2p_write_begin" && event.Stage != "p2p_write_end" {
				return
			}
			event.ClientId, event.PeerId = identity.ClientId, identity.PeerId
			event.SequenceId, event.MessageId = identity.SequenceId, identity.MessageId
			if event.Stage == "p2p_write_begin" {
				event.Stage = "ack_physical_write_begin"
			} else {
				event.Stage = "ack_physical_write_end"
			}
			trace.observe(event)
		}
		transport, route := NewP2pSendTransport(ctx, cancel, left, NewId(), settings)
		route <- ack.bytes
		ack.bytes = nil
		read = func() ([]byte, error) { return readP2pMessage(right, 4096, 4096, settings.MaxMessageByteCount) }
		t.Cleanup(func() {
			cancel()
			left.Close()
			right.Close()
			if err := transport.(P2pRouteLifecycle).CloseAndWait(context.Background()); err != nil {
				t.Error(err)
			}
		})
	}
	return func() {
		wire, err := read()
		if err != nil {
			MessagePoolReturn(wire)
			t.Fatal(err)
		}
		event := identity
		event.ClientId, event.PeerId = identity.PeerId, identity.ClientId
		event.Stage, event.Success = "ack_physical_read", true
		beginTransferProgress(trace.observe, event, wire)
		fixture.senderIn <- wire
		synctest.Wait()
	}
}

// Local ACK route admission succeeds in every arm. Holding the actual peer
// reader still causes a physical writer deadline and then the unchanged 30s
// Transfer terminal. The same observer also proves the positive read path.
func TestTransferAckLineagePhysicalReply(t *testing.T) {
	for _, carrier := range []TransportType{TransportTypeH1, TransportTypeP2p} {
		for _, version := range []int{1, 2} {
			for _, readPeer := range []bool{true, false} {
				t.Run(fmt.Sprintf("%s/v%d/read=%t", carrier, version, readPeer), func(t *testing.T) {
					assertMessagePoolOwnership(t)
					synctest.Test(t, func(t *testing.T) {
						trace := &ackLineageTrace{}
						fixture, _, _ := newAckRetirementFixture(t, carrier, version, NewNoopLogger(), trace.configure)
						started := time.Now()
						wire, terminal := ackLineageSend(t, fixture, trace, 0)
						sequence := fixture.sequence()
						read := ackLineagePhysicalReply(t, fixture, trace, carrier, fixture.receive(wire))
						time.Sleep(7 * time.Second)
						requireAckLineageBoundary(t, trace, fixture, wire, "ack_physical_read", true)
						if readPeer {
							read()
						}
						time.Sleep(time.Until(started.Add(31 * time.Second)))
						synctest.Wait()
						if len(terminal) != 1 {
							t.Fatalf("terminal callbacks=%d, want one", len(terminal))
						}
						result := <-terminal
						physicalDeadline := DefaultP2pTransportSettings().WriteTimeout
						if carrier == TransportTypeH1 {
							physicalDeadline = DefaultPlatformTransportSettings().WriteTimeout
						}
						physicalEnds, physicalFailures := 0, 0
						events, err := trace.snapshot()
						if err != nil {
							t.Fatal(err)
						}
						for _, event := range events {
							if event.Stage == "ack_write_end" && !event.Success {
								t.Fatal("physical failure was falsely assigned to route admission")
							}
							if event.Stage != "ack_physical_write_end" {
								continue
							}
							physicalEnds++
							if !event.Success {
								physicalFailures++
								if event.ElapsedNanos != int64(physicalDeadline) {
									t.Fatalf("physical deadline=%s", time.Duration(event.ElapsedNanos))
								}
							}
						}
						if physicalEnds != 1 {
							t.Fatalf("physical write completions=%d", physicalEnds)
						}
						if readPeer {
							if result.err != nil || result.at != started.Add(7*time.Second) || physicalFailures != 0 {
								t.Fatalf("read reply result=%+v failures=%d", result, physicalFailures)
							}
							requireAckLineageBoundary(t, trace, fixture, wire, "complete", true)
						} else {
							if result.err == nil || result.at != started.Add(30*time.Second) || physicalFailures != 1 {
								t.Fatalf("held physical result=%+v failures=%d", result, physicalFailures)
							}
							requireAckLineageBoundary(t, trace, fixture, wire, "ack_physical_read", true)
						}
						if fixture.deliveredCount != 1 || sequence.resendQueue.Len() != 0 || len(sequence.ackLifetimes.items) != 0 {
							t.Fatal("physical fixture changed delivery or retained ownership")
						}
						t.Logf("first_missing=ack_physical_read read=%t write_deadline=%s terminal=%s success=%t", readPeer, physicalDeadline, result.at.Sub(started), result.err == nil)
					})
				})
			}
		}
	}
}
