//go:build !js

package connect

import (
	"context"
	"encoding/binary"
	"net"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Supplies the same shared reservation boundary as the production peer owner;
// the controlled writer below replaces service, not admission or buffering.
type providerUdpAdmissionTestConn struct {
	*p2pProbePressureConn
	budget *TransferMemoryBudget
}

func (self *providerUdpAdmissionTestConn) legacySendMemoryBudget() *TransferMemoryBudget {
	return self.budget
}

// Distinguishes a missing caller-side bypass from a full carrier followed by
// a full bounded fallback. The one-second barrier is an explicit service-gap
// discriminator, not a claim to replay SCTP's historical loss/timing trace.
func TestProviderUdpReturnAdmissionIdentifiesFullCarrierFallback(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, stalled := range []bool{false, true} {
		name := "ready-carrier"
		if stalled {
			name = "full-carrier-and-fallback"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				peerId := NewId()
				type attempt struct {
					tried, written bool
					timeout        time.Duration
				}
				var attempts []attempt
				var failures []*SendPackAdmissionError
				settings := DefaultClientSettings()
				settings.Log = NewNoopLogger()
				settings.EncryptionSettings.Mode = EncryptionModeOff
				settings.SendBufferSettings.SendPackLifecycleObserver = func(event SendPackLifecycleObservation) {
					if event.DestinationId != peerId || event.MessageType != protocol.MessageType_IpIpPacketFromProvider ||
						event.Phase != SendPackLifecyclePhaseTerminal || event.Err == nil {
						return
					}
					admission, ok := event.Err.(*SendPackAdmissionError)
					if !ok || event.AckRequired || admission.Boundary != "pack-admission" ||
						admission.Timeout != 0 || admission.Err != ErrSendPackNotAdmitted ||
						admission.RecoveredByOwner || admission.OwnerTrackingOverflow {
						t.Errorf("wrong source refusal owner: %+v", event)
						return
					}
					failures = append(failures, admission)
				}
				provider, client, _ := newProviderTransferKeyTestFixtureWithClientSettings(
					t, DefaultRemoteUserNatProviderSettings(), settings,
				)
				defer closeTransferGroupTestClient(t, client)
				defer provider.Close()
				client.ContractManager().AddNoContractPeer(peerId)
				client.sendBuffer.afterNoAckFastPathForTest = func(id sendSequenceId, tried, written bool, timeout time.Duration) {
					if id.Destination == peerId {
						attempts = append(attempts, attempt{tried, written, timeout})
					}
				}
				ctx, cancel := context.WithCancel(provider.ctx)
				defer cancel()
				budget := NewTransferMemoryBudget(768 * 1024)
				if !budget.TryReserve(512 * 1024) {
					t.Fatal("fixed SCTP reservation failed")
				}
				defer func() {
					if budget.UsedByteCount() != 512*1024 {
						t.Errorf("carrier owner leaked: %d", budget.UsedByteCount())
					}
					budget.Release(512 * 1024)
				}()
				conn := &providerUdpAdmissionTestConn{
					p2pProbePressureConn: &p2pProbePressureConn{ctx: ctx}, budget: budget,
				}
				const payloadBytes = 1000
				const interval = time.Duration(8 * payloadBytes * int64(time.Second) / 3_750_000)
				const offers = int(time.Second / interval)
				admitted := make([]bool, offers+2)
				written := make([]bool, offers+2)
				conn.onWire = func(wire []byte) {
					pack := decodeCompactContractTestPack(t, wire)
					if !pack.GetNack() || pack.GetHead() || pack.GetSequenceNumber() != 0 {
						t.Error("UDP entered reliable Transfer delivery")
					}
					for _, frame := range pack.Frames {
						packet := frame.MessageBytes
						index := int(binary.BigEndian.Uint32(packet[len(packet)-4:]))
						if index >= len(written) || written[index] {
							t.Errorf("unexpected or duplicated datagram %d", index)
							continue
						}
						written[index] = true
					}
				}
				p2pSettings := DefaultP2pTransportSettings()
				p2pSettings.DataPlaneMode = P2pDataPlaneModeLegacyOnly
				transport, route := newP2pSendTransportForPeer(ctx, cancel, conn, peerId, NewId(), p2pSettings, false, nil)
				client.RouteManager().UpdateTransport(transport, []Route{route})
				defer func() {
					client.RouteManager().RemoveTransport(transport)
					if err := transport.(P2pRouteLifecycle).CloseAndWait(context.Background()); err != nil {
						t.Error(err)
					}
				}()
				completed := make(chan providerReturnSendResult, 1)
				provider.afterReturnSendForTest = func(result providerReturnSendResult) { completed <- result }
				template := craftSecurityPacket(IpProtocolUdp, net.ParseIP("203.0.113.7"), 8080,
					net.ParseIP("10.0.0.9"), 42001, false, make([]byte, payloadBytes))
				ipPath, err := ParseIpPath(template)
				if err != nil {
					t.Fatal(err)
				}
				key := TransferKey{ForceStream: true, EncryptionRole: protocol.SequenceRole_SequenceRoleServer}
				offer := func(index int) bool {
					packet := MessagePoolCopy(template)
					binary.BigEndian.PutUint32(packet[len(packet)-4:], uint32(index))
					before := time.Now()
					// The NAT normally reports a one-packet batch. It must retain
					// the singleton's direct try, not become a logical group.
					provider.receiveTransferBatch(SourceId(peerId), key, protocol.ProvideMode_Public, ipPath, [][]byte{packet})
					MessagePoolReturn(packet)
					synctest.Wait()
					select {
					case result := <-completed:
						if time.Now() != before || result.packetCount != 1 || result.packetByteCount != ByteCount(len(template)) {
							t.Fatalf("changed nonblocking source contract: elapsed=%s result=%+v", time.Since(before), result)
						}
						admitted[index] = result.sent
						return result.sent
					default:
						t.Fatal("missing immediate return disposition")
						return false
					}
				}
				if !offer(0) || !written[0] {
					t.Fatal("warmup did not establish the direct-write snapshot")
				}
				attempts = nil
				gate := make(chan struct{})
				if stalled {
					conn.mutex.Lock()
					conn.gate = gate
					conn.mutex.Unlock()
				}
				refused, queued := 0, 0
				for index := 1; index <= offers; index++ {
					sent := offer(index)
					if len(attempts) != index || !attempts[index-1].tried || attempts[index-1].timeout != 0 {
						t.Fatalf("singleton bypass was absent/reset: index=%d attempts=%+v", index, attempts)
					}
					if !sent {
						refused++
						if attempts[index-1].written || len(route) != cap(route) {
							t.Fatal("source refusal was not preceded by a full-carrier direct try")
						}
					} else if !attempts[index-1].written {
						queued++
					}
					time.Sleep(interval)
				}
				if (stalled && (refused == 0 || queued != 32)) || (!stalled && (refused != 0 || queued != 0)) {
					t.Fatalf("wrong ready/full boundary: refused=%d queued=%d", refused, queued)
				}
				if len(failures) != refused {
					t.Fatalf("typed source failures=%d want=%d", len(failures), refused)
				}
				if cap(route) != 4 || settings.SendBufferSettings.SequenceBufferSize != 32 ||
					p2pSettings.LegacySendQueueByteCount != 256*1024 || budget.UsedByteCount() > 768*1024 {
					t.Fatal("changed admission or shared queue-memory bounds")
				}
				close(gate)
				synctest.Wait()
				if !slices.Equal(admitted, written) || !offer(offers+1) || !written[offers+1] {
					t.Fatal("carrier drain lost an admitted identity or poisoned source recovery")
				}
				drops := provider.CongestionDropStats()
				if drops.ReturnQueuePacketCount != 0 || drops.ReturnSendPacketCount != int64(refused) ||
					drops.ReturnSendByteCount != ByteCount(refused*len(template)) {
					t.Fatalf("wrong exact refusal accounting: %+v", drops)
				}
				t.Logf("offered=%d direct=%d queued=%d refused=%d; every admitted identity drained once", offers, offers-queued-refused, queued, refused)
			})
		})
	}
}
