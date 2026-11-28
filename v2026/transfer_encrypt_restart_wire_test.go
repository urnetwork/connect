// Restart coverage holds a genuine identity proof across replacement TLS sessions.
package connect

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

// Hold a genuine proof retransmission at the head of a new transfer sequence.
// The restarted receiver can consider the proof even without the old sequence.
func TestRequiredSessionRestartWithDelayedWireProof(t *testing.T) {
	for _, beforeHello := range []bool{true, false} {
		ownedTestWireSynctest(t, func(t *testing.T) {
			p := newTlsWirePair(t, [2]Id{NewId(), NewId()}, [2]OutOfBandControl{}, func(_ int, s *ClientSettings) {
				s.EncryptionSettings.IdentityProofResendInterval = 200 * time.Millisecond
			})
			defer p.close()
			a := p.clients[0]
			firstReceipt := p.send("before restart")
			if err := p.waitResult("first application receipt", firstReceipt); err != nil || !slices.Equal(p.deliveries(1), []string{"before restart"}) {
				t.Fatalf("initial application ack=%v deliveries=%v", err, p.deliveries(1))
			}
			sender := a.EncryptionSessionManager().Lookup(p.ids[1], sequenceTlsRoleClient, false)
			sender.stateLock.Lock()
			oldId := sender.establishedEpoch.epochId
			oldExporter := bytes.Clone(sender.establishedEpoch.tlsExporter)
			sender.stateLock.Unlock()
			var oldWire []byte
			p.setWirePolicy(func(side int, wire []byte) bool {
				if side != 0 {
					return true
				}
				proof := tlsWireControl(wire, protocol.EncryptedControlType_EncryptedControlIdentityProof)
				pack, _ := tlsWireContents(wire)
				if pack.GetHead() && oldWire == nil && bytes.Equal(proof.GetEpochId(), oldId.Bytes()) {
					if !ed25519.Verify(a.ClientKeyManager().PublicKey(), oldExporter, proof.Payload) {
						t.Error("held proof does not sign the original exporter")
					}
					oldWire = bytes.Clone(wire)
				}
				return false
			})
			a.Flush()
			p.await("original proof retransmitted as a transfer head", func() bool { return oldWire != nil })
			if err := p.clients[1].CloseAndWait(context.Background()); err != nil {
				t.Fatal(err)
			}
			a.EncryptionSessionManager().Testing_DropSessions(p.ids[1])
			a.Flush()
			synctest.Wait()
			p.start(1)

			var hello, freshProof []byte
			var freshId Id
			holdHello, holdProof := true, true
			var sealedApplication []byte
			p.setWirePolicy(func(side int, wire []byte) bool {
				if side != 0 {
					return true
				}
				control := tlsWireControl(wire, protocol.EncryptedControlType_EncryptedControlHandshake)
				if holdHello && isClientHelloRecord(control.GetPayload()) {
					if hello == nil {
						hello = bytes.Clone(wire)
						freshId = RequireIdFromBytes(control.EpochId)
					}
					return false
				}
				proof := tlsWireControl(wire, protocol.EncryptedControlType_EncryptedControlIdentityProof)
				if holdProof && proof != nil {
					if freshProof == nil {
						freshProof = bytes.Clone(wire)
					}
					return false
				}
				pack, record := tlsWireContents(wire)
				if len(tlsWireApplicationLabels(t, pack.GetFrames())) > 0 {
					t.Error("Required mode sent plaintext application bytes")
				}
				current := a.EncryptionSessionManager().Lookup(p.ids[1], sequenceTlsRoleClient, false)
				if sealedApplication == nil && len(record) > 0 && current != nil {
					if cipher := current.Cipher(); cipher != nil {
						if opened, err := cipher.Open(record); err == nil {
							pack, _ := tlsWireContents(opened)
							if slices.Contains(tlsWireApplicationLabels(t, pack.GetFrames()), "after restart") {
								sealedApplication = bytes.Clone(wire)
							}
							MessagePoolReturn(opened)
						}
					}
				}
				return true
			})
			receipt := p.send("after restart")
			p.await("replacement ClientHello", func() bool { return hello != nil })
			releaseHello := func() {
				p.wireLock.Lock()
				holdHello = false
				p.wireLock.Unlock()
				p.in[1] <- MessagePoolCopy(hello)
				synctest.Wait()
			}
			if !beforeHello {
				releaseHello()
			}
			p.in[1] <- MessagePoolCopy(oldWire)
			synctest.Wait()
			receiver := p.clients[1].EncryptionSessionManager().Lookup(p.ids[0], sequenceTlsRoleServer, false)
			if receiver == nil {
				t.Fatal("test did not reach the responder")
			}
			wantId := freshId
			if beforeHello {
				wantId = Id{}
			}
			receiver.stateLock.Lock()
			epoch := receiver.epoch
			refused := epoch != nil && epoch.epochId == wantId && len(epoch.pendingPeerIdentityProof) == 0 &&
				!epoch.peerIdentityVerified && !epoch.identityFailed
			receiver.stateLock.Unlock()
			if !refused {
				t.Fatalf("before ClientHello=%t: an earlier wire proof affected the replacement epoch", beforeHello)
			}
			if beforeHello {
				// The fresh head can first retire the receive sequence opened
				// by the delayed proof. Let its ordinary retransmission run.
				releaseHello()
			}
			p.await("fresh identity proof", func() bool { return freshProof != nil })
			if len(p.deliveries(1)) != 1 || receiver.Cipher() != nil {
				t.Fatal("replacement admitted traffic before its own proof")
			}
			p.wireLock.Lock()
			holdProof = false
			p.wireLock.Unlock()
			p.in[1] <- MessagePoolCopy(freshProof)
			if err := p.waitResult("acknowledged post-restart delivery", receipt); err != nil {
				t.Fatalf("fresh application message was not acknowledged: %v", err)
			}
			sender = a.EncryptionSessionManager().Lookup(p.ids[1], sequenceTlsRoleClient, false)
			if receiver.Cipher() == nil || sender.Cipher() == nil || freshId == oldId ||
				receiver.epochIdOf(receiver.currentEpoch()) != freshId || sender.epochIdOf(sender.currentEpoch()) != freshId {
				t.Fatal("peers did not establish the fresh epoch")
			}
			p.wireLock.Lock()
			application := sealedApplication
			p.wireLock.Unlock()
			if application == nil {
				t.Fatal("fresh application message did not cross the route under the new cipher")
			}
			// The transfer receiver dispatches a reliable message once across retries.
			p.in[1] <- MessagePoolCopy(application)
			synctest.Wait()
			if !slices.Equal(p.deliveries(1), []string{"before restart", "after restart"}) {
				t.Fatalf("post-restart application deliveries %v", p.deliveries(1))
			}
		})
	}
}
