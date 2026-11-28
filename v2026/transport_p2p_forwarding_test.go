// Normal route writer admission and physical writes in both carrier modes.
package connect

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"testing/synctest"
)

// Captures the complete physical SCTP or reassembled native message. Each
// queue entry owns its own copy of the writer's borrowed frame.
type streamForwardingTestPhysicalConn struct {
	*p2pProbeFastLoopConn
	legacyWrites chan []byte
}

// One ordinary SCTP write is one complete physical record.
func (self *streamForwardingTestPhysicalConn) Write(wire []byte) (int, error) {
	self.legacyWrites <- MessagePoolCopy(wire)
	return len(wire), nil
}

// Sizes a syntactically valid ordinary app Pack to an exact wire boundary.
func sizedStreamForwardingTestFrame(t *testing.T, source *streamForwardingAssociation, target int) []byte {
	t.Helper()
	template := streamForwardingTestPack(source, nil, false)
	payloadSize := target - 96
	for range 5 {
		template.Pack.Frames[0].MessageBytes = make([]byte, payloadSize)
		frame := marshalStreamForwardingTestFrame(t, template)
		if len(frame) == target {
			return frame
		}
		payloadSize += target - len(frame)
	}
	t.Fatalf("unable to produce %d native Transfer bytes", target)
	return nil
}

// A forward-only route returns the oversized caller's exact unsent root and
// remains usable. The direct sibling retains the whole previous capacity.
func TestStreamForwardingApplicationPhysicalWriters(t *testing.T) {
	ownedTestWireSynctest(t, func(t *testing.T) {
		for _, mode := range []P2pDataPlaneMode{P2pDataPlaneModeLegacyOnly, P2pDataPlaneModeFastOnly} {
			for _, size := range []int{1500, 65536} {
				for _, delayed := range []bool{false, true} {
					for _, forward := range []bool{false, true} {
						func() {
							source, destination := streamForwardingApplicationPair(t)
							ctx, cancel := context.WithCancel(t.Context())
							defer cancel()
							conn := &streamForwardingTestPhysicalConn{p2pProbeFastLoopConn: newP2pProbeFastLoopConn(), legacyWrites: make(chan []byte, 2)}
							defer conn.Close()
							settings := DefaultP2pTransportSettings()
							settings.DataPlaneMode = mode
							settings.MaxMessageByteCount = size
							settings.ChannelBufferSize = 2
							settings.LegacySendQueueByteCount = 0
							var delay *p2pTransportDelay
							if delayed {
								delay = newP2pTransportDelay(newTransportDelay(nil, NewId(), TransportTypeP2p, nil))
							}
							peerId := source.sendPath.DestinationId
							var binding *streamForwardingBinding
							if forward {
								peerId = NewId()
								binding = &streamForwardingBinding{readiness: source.readiness}
								binding.association.Store(source)
							}
							sender, route := newP2pSendTransportWithForwarding(ctx, cancel, conn, peerId, source.grant.path.StreamId, settings, false, nil, delay, true, binding)
							send := sender.(*P2pSendTransport)
							manager := NewRouteManager(ctx, "forwarded-physical-writer")
							manager.UpdateTransportWithProperties(sender, []Route{route}, p2pTransferCarrierProperties(sender))
							writer := manager.OpenMultiRouteWriter(StreamId(source.grant.path.StreamId))
							defer func() {
								manager.CloseMultiRouteWriter(writer)
								manager.RemoveTransport(sender)
								send.Close()
								<-send.Done()
							}()
							maximum := size - delay.headerByteCount()
							if forward {
								maximum -= streamForwardingTrailerByteCount
								oversize := MessagePoolCopy(sizedStreamForwardingTestFrame(t, source, maximum+1))
								accepted, err := writer.WriteDetailed(ctx, oversize, 0)
								if accepted {
									t.Fatal("oversize forwarded route consumed caller ownership")
								}
								returned := MessagePoolReturn(oversize)
								if !errors.Is(err, ErrMessageTooLarge) || size == 1500 && !returned {
									t.Fatalf("oversize mode %v delay %t size %d ownership %v error %v", mode, delayed, size, returned, err)
								}
							}
							frame := sizedStreamForwardingTestFrame(t, source, maximum)
							owned := MessagePoolCopy(frame)
							if accepted, err := writer.WriteDetailed(ctx, owned, 0); !accepted || err != nil {
								MessagePoolReturn(owned)
								t.Fatalf("exact boundary refused for forward=%t mode=%v delay=%t size=%d: %v", forward, mode, delayed, size, err)
							}
							synctest.Wait()
							var wire []byte
							if mode == P2pDataPlaneModeFastOnly {
								select {
								case physical := <-conn.fastMessages:
									wire = physical.message
								default:
									t.Fatal("expected native physical write")
								}
							} else {
								select {
								case wire = <-conn.legacyWrites:
								default:
									t.Fatal("expected reliable physical write")
								}
							}
							defer MessagePoolReturn(wire)
							if len(wire) != size {
								t.Fatalf("physical bytes %d, want configured %d", len(wire), size)
							}
							physical := wire[delay.headerByteCount():]
							if forward {
								inner, kind, ok := destination.open(physical, size-delay.headerByteCount())
								if !ok || kind != streamForwardingPack || !bytes.Equal(inner, frame) {
									t.Fatal("endpoint did not open exact physical DATA")
								}
							} else if !bytes.Equal(physical, frame) {
								t.Fatal("direct carrier changed its near-limit Transfer wire")
							}
						}()
					}
				}
			}
		}
	})
}
