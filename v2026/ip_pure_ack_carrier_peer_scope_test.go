// Replacing an encrypted fixture carrier preserves its original peer-only scope.
package connect

import (
	"bytes"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/urnetwork/connect/v2026/protocol"
)

// Pending public platform owners remain separate from actual encrypted peer
// delivery through either replacement carrier and its real terminal receipt.
func TestPureTcpAckEncryptedCarrierKeepsPeerRouteScope(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		for _, carrier := range []TransportType{TransportTypeP2p, TransportTypeH1} {
			var controlStarts, controlTerminals, controlSuccess atomic.Int64
			f := newPrivateProviderEncryptedAckFixture(t, func(settings *ClientSettings) {
				settings.SendBufferSettings.SendPackLifecycleObserver = func(event SendPackLifecycleObservation) {
					if event.DestinationId == ControlId && event.MessageType == protocol.MessageType_TransferProvide {
						switch event.Phase {
						case SendPackLifecyclePhaseStarted:
							controlStarts.Add(1)
						case SendPackLifecyclePhaseTerminal:
							controlTerminals.Add(1)
							if event.Err == nil {
								controlSuccess.Add(1)
							}
						}
					}
				}
			})
			defer f.close()
			// The constructor already called public SetProvideModes for both
			// clients. Observe those genuine pending owners; another call could
			// wait behind their blocking ControlSync admission while holding
			// the same send lock, before this routing witness reaches its cut.
			synctest.Wait()
			manager := f.sender.RouteManager()
			thirdPeer := NewId()
			checkUnrelated := func(stage string) {
				for _, destination := range []Id{ControlId, thirdPeer} {
					writer := manager.OpenMultiRouteWriter(DestinationId(destination))
					routes := writer.GetActiveRoutes()
					manager.CloseMultiRouteWriter(writer)
					if len(routes) != 0 {
						t.Fatalf("PURE_ACK_CARRIER_ESCAPED_PEER_SCOPE: stage=%s carrier=%v destination=%v control=%t routes=%d", stage, carrier, destination, destination == ControlId, len(routes))
					}
				}
			}
			checkUnrelated("before replacement")
			controls := make(map[Id]*SendSequence)
			f.sender.sendBuffer.mutex.Lock()
			for id, source := range f.sender.sendBuffer.sendSequencesBySequenceId {
				if source.destination == ControlId {
					controls[id] = source
				}
			}
			f.sender.sendBuffer.mutex.Unlock()
			// A refused setup attempt may terminate before ControlSync retries;
			// only new terminals after this settled cut would consume its live
			// platform ownership during replacement or the peer receipt.
			controlTerminalsBefore := controlTerminals.Load()
			if len(controls) == 0 || controlStarts.Load() <= controlTerminalsBefore || controlSuccess.Load() != 0 || len(f.senderOut) != 0 || len(f.receiverOut) != 0 {
				t.Fatal("public Provide did not retain its genuine separate platform owner")
			}
			pureAckEncryptedCarrier(t, f, carrier)
			checkUnrelated("after replacement")
			if controlSuccess.Load() != 0 || controlTerminals.Load() != controlTerminalsBefore || len(f.senderOut) != 0 || len(f.receiverOut) != 0 {
				t.Fatal("replacement wrote or acknowledged unrelated platform traffic")
			}
			probe := budgetTestFrame(64)
			probeType, probeRaw := probe.MessageType, probe.Raw
			probeBytes := bytes.Clone(probe.MessageBytes)
			var peerCallbacks, peerSuccess atomic.Int64
			accepted, err := f.sender.SendWithTimeoutDetailed(probe, f.receiver.ClientId(), func(err error) {
				peerCallbacks.Add(1)
				if err == nil {
					peerSuccess.Add(1)
				}
			}, 0)
			if !accepted {
				MessagePoolReturn(probe.MessageBytes)
			}
			if !accepted || err != nil {
				t.Fatalf("replacement peer admission failed: %t %v", accepted, err)
			}
			synctest.Wait()
			var original *privateProviderEncryptedAckFrame
			for count := 0; len(f.senderOut) != 0; count++ {
				if count == cap(f.senderOut) {
					t.Fatal("replacement peer wire did not reach a bounded original")
				}
				frame := f.capture(<-f.senderOut, f.receiver)
				if frame.pack != nil && len(frame.pack.Frames) == 1 && frame.pack.Frames[0].MessageType == probeType {
					original = frame
					break
				}
				if frame.pack != nil && !privateProviderEncryptedControl(frame.pack) || frame.pack == nil && frame.ack == nil {
					t.Fatal("replacement peer path carried an unclassified original")
				}
				f.forward(frame.windowRoundFrame, f.receiverIn)
				f.feedback()
			}
			if original == nil || !original.encrypted || original.pack.Nack || original.pack.DeliverImmediately || original.pack.Frames[0].Raw != probeRaw || !bytes.Equal(original.pack.Frames[0].MessageBytes, probeBytes) || !bytes.Equal(original.pack.SequenceId, f.sequence.sequenceId.Bytes()) || peerCallbacks.Load() != 0 {
				t.Fatal("replacement lost the exact encrypted peer DATA original")
			}
			privateProviderAssertOriginalPending(t, f.windowRoundFixture, original.pack, false, false)
			f.forward(original.windowRoundFrame, f.receiverIn)
			f.feedback()
			privateProviderAssertOriginalRetired(t, f.windowRoundFixture, original.pack)
			if peerCallbacks.Load() != 1 || peerSuccess.Load() != 1 || f.deliveredCount != 2 || controlSuccess.Load() != 0 || controlTerminals.Load() != controlTerminalsBefore || len(f.senderOut) != 0 || len(f.receiverOut) != 0 {
				t.Fatal("peer ACK changed unrelated ownership or its exact callback")
			}
			f.sender.sendBuffer.mutex.Lock()
			controlsUnchanged := true
			for id, source := range controls {
				controlsUnchanged = controlsUnchanged && f.sender.sendBuffer.sendSequencesBySequenceId[id] == source && source.ctx.Err() == nil
			}
			f.sender.sendBuffer.mutex.Unlock()
			if !controlsUnchanged {
				t.Fatal("peer completion retired pending platform ownership")
			}
			checkUnrelated("after peer receipt")
			f.close()
			if controlSuccess.Load() != 0 {
				t.Fatal("joined cancellation falsely acknowledged pending platform ownership")
			}
		}
	})
}
