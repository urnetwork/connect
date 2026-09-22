package connect

import (
	"context"
	"net"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

// The static PERFVAR 5-Mbit/s download offers 1,000-byte UDP datagrams at
// 75% of the link rate. A measured legacy SCTP write stalled for 120.958 ms
// with its four-slot route full. Nominal link bandwidth does not guarantee
// that the finite, zero-wait source can admit every datagram through that
// service gap. Exercise the actual provider, SendSequence, route selector,
// and P2P writer with an exact connection barrier rather than random loss.
// The healthy control admits everything; the blocked arm must count refusals,
// keep the shared worker and route alive, and resume after the carrier drains.
func TestProviderUdpFixedOfferCountsBoundedCarrierRefusal(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, blocked := range []bool{false, true} {
		name := "ready-carrier"
		if blocked {
			name = "121ms-carrier-stall"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				clientSettings := DefaultClientSettings()
				clientSettings.EncryptionSettings.Mode = EncryptionModeOff
				providerSettings := DefaultRemoteUserNatProviderSettings()
				provider, client, _ := newProviderTransferKeyTestFixtureWithClientSettings(t, providerSettings, clientSettings)
				peerId := NewId()
				client.ContractManager().AddNoContractPeer(peerId)
				ctx, cancel := context.WithCancel(provider.ctx)
				defer cancel()
				settings := DefaultP2pTransportSettings()
				settings.DataPlaneMode = P2pDataPlaneModeLegacyOnly
				settings.LegacySendQueueByteCount = 0 // historical finite-carrier control
				conn := &p2pProbePressureConn{ctx: ctx}
				writtenPackets := 0
				conn.onWire = func(wire []byte) {
					pack := decodeCompactContractTestPack(t, wire)
					if !pack.GetNack() {
						t.Error("UDP source unexpectedly requested reliable Transfer delivery")
					}
					writtenPackets += len(pack.Frames)
				}
				transport, route := newP2pSendTransportForPeer(ctx, cancel, conn, peerId, NewId(), settings, false, nil)
				client.RouteManager().UpdateTransport(transport, []Route{route})
				defer func() {
					client.RouteManager().RemoveTransport(transport)
					if err := transport.(P2pRouteLifecycle).CloseAndWait(context.Background()); err != nil {
						t.Error(err)
					}
				}()
				completed := make(chan providerReturnSendResult, 1)
				provider.afterReturnSendForTest = func(result providerReturnSendResult) { completed <- result }
				packet := MessagePoolCopy(craftSecurityPacket(
					IpProtocolUdp, net.ParseIP("203.0.113.7"), 8080,
					net.ParseIP("10.0.0.9"), 42001, false, make([]byte, 1000),
				))
				defer MessagePoolReturn(packet)
				ipPath, err := ParseIpPath(packet)
				if err != nil {
					t.Fatal(err)
				}
				transferKey := TransferKey{ForceStream: true, EncryptionRole: protocol.SequenceRole_SequenceRoleServer}
				offer := func() bool {
					before := time.Now()
					provider.receiveTransfer(SourceId(peerId), transferKey, protocol.ProvideMode_Public, ipPath, packet)
					synctest.Wait()
					select {
					case result := <-completed:
						if !time.Now().Equal(before) {
							t.Fatal("the shared UDP return worker waited for carrier capacity")
						}
						if result.packetCount != 1 || result.packetByteCount != ByteCount(len(packet)) {
							t.Fatalf("source terminal accounting=%+v", result)
						}
						return result.sent
					default:
						t.Fatal("zero-wait UDP return did not publish a terminal disposition")
						return false
					}
				}
				// Establish the ordinary no-contract sequence before the service
				// stall, so cold contract setup is not the refusal mechanism.
				if !offer() || writtenPackets != 1 {
					t.Fatal("warm source packet did not reach the ready carrier")
				}
				gate := make(chan struct{})
				if blocked {
					conn.mutex.Lock()
					conn.gate = gate
					conn.mutex.Unlock()
				}
				const offeredBitRate = 3_750_000
				const interval = time.Duration(8 * 1000 * int64(time.Second) / offeredBitRate)
				const stall = 121 * time.Millisecond
				const offerCount = int((stall-1)/interval) + 1
				admitted, refused := 0, 0
				for index := range offerCount {
					if offer() {
						admitted++
					} else {
						refused++
					}
					if index+1 < offerCount {
						time.Sleep(interval)
					}
				}
				time.Sleep(stall - time.Duration(offerCount-1)*interval)
				if blocked && refused == 0 {
					t.Fatal("stalled finite carrier admitted an offer exceeding its bounded capacity")
				}
				if !blocked && refused != 0 {
					t.Fatalf("ready carrier refused %d/%d datagrams", refused, offerCount)
				}
				if blocked {
					if writtenPackets != 1 || len(route) != cap(route) {
						t.Fatalf("barrier did not hold the physical writer: written=%d route=%d/%d", writtenPackets, len(route), cap(route))
					}
					close(gate)
					synctest.Wait()
				}
				if writtenPackets != 1+admitted {
					t.Fatalf("admitted ownership was lost: physical=%d source=%d", writtenPackets, 1+admitted)
				}
				if !offer() || writtenPackets != 2+admitted || client.ctx.Err() != nil {
					t.Fatal("local UDP refusal poisoned a healthy route instead of recovering")
				}
				if drops := provider.CongestionDropStats(); drops.ReturnQueuePacketCount != 0 || drops.ReturnSendPacketCount != int64(refused) ||
					drops.ReturnSendByteCount != ByteCount(refused*len(packet)) {
					t.Fatalf("exact refusal accounting=%+v refused=%d", drops, refused)
				}
				t.Logf("offered=%d admitted=%d refused=%d physical=%d carrier_slots=%d transfer_slots=%d", offerCount, admitted, refused, writtenPackets-2, cap(route), clientSettings.SendBufferSettings.SequenceBufferSize)
			})
		})
	}
}
