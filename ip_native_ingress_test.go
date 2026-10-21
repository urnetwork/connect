// Native admission returns before unrelated future reads, while the original
// source keeps its real reliable credit and recovery ownership.
package connect

import (
	"bytes"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// This structural interface also compiles against the pre-fix implementation.
// Its fallback is precisely the old native call with the existing five-second
// SDK budget, not an alternative zero-timeout test implementation.
type nativeIngressTestEntry interface {
	SendNativePacketBatch(TransferPath, protocol.ProvideMode, [][]byte, time.Duration) int
}

// Both production native owners also expose the ordinary compatibility entry.
type nativeIngressTestBatchEntry interface {
	SendPacketBatch(TransferPath, protocol.ProvideMode, [][]byte, time.Duration) int
}

func offerNativeIngressTest(entry nativeIngressTestBatchEntry, source TransferPath, packets [][]byte) int {
	if native, ok := entry.(nativeIngressTestEntry); ok {
		return native.SendNativePacketBatch(source, protocol.ProvideMode_Network, packets, 5*time.Second)
	}
	return entry.SendPacketBatch(source, protocol.ProvideMode_Network, packets, 5*time.Second)
}

// A second native read is sequential, not another producer competing inside
// the first batch. Its independently ready destination must reach its actual
// route before the first destination's withheld peer credit is released.
func TestNativeIngressNextReadPrecedesUnrelatedCreditRelease(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, throughMux := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			t.Logf("native ingress owner through_mux=%t", throughMux)
			f := newNativeBatchCreditFixture(t)
			peer := NewId()
			selected := newPacketTransferTestChannel()
			selected.ctx, selected.client, selected.settings = f.base.parent.ctx, f.base.client, f.base.parent.settings
			selected.args = &multiClientChannelArgs{Destination: RequireMultiHopId(peer)}
			f.udpUpdate.client.Store(selected)
			f.otherRoute = make(Route, 4)
			f.base.client.ContractManager().AddNoContractPeer(peer)
			f.base.client.RouteManager().UpdateTransport(NewSendClientTransport(DestinationId(peer)), []Route{f.otherRoute})
			var entry nativeIngressTestBatchEntry = f.base.parent
			if throughMux {
				mux := &UpgradeMux{mux: &IpMux{}}
				mux.mux.SetUpstream(f.base.parent.SendPacket)
				mux.mux.setUpstreamGroupSend(f.base.parent.sendPacketGroup)
				mux.upstreamMultiClient.Store(f.base.parent)
				entry = mux
			}
			first, second := f.ownPackets(f.tcp), f.ownPackets(f.udp)
			f.base.selected.stateLock.Lock()
			priorCount := f.base.selected.packetStats.sendNackCount
			priorBytes := f.base.selected.packetStats.sendNackByteCount
			priorPending := f.base.selected.pendingSendTime
			f.base.selected.stateLock.Unlock()
			nextRead := make(chan struct{})
			done := make(chan [2]int, 1)
			started := time.Now()
			f.workers.Add(1)
			go func() {
				defer f.workers.Done()
				firstCount := offerNativeIngressTest(entry, f.base.source, first)
				close(nextRead)
				secondCount := offerNativeIngressTest(entry, f.base.source, second)
				done <- [2]int{firstCount, secondCount}
			}()
			synctest.Wait()
			select {
			case <-nextRead:
			default:
				t.Fatal("NATIVE_INGRESS_NEXT_READ_HELD_BY_UNRELATED_CREDIT: first native batch still owns the read loop")
			}
			select {
			case counts := <-done:
				if counts != [2]int{0, 1} || time.Since(started) != 0 {
					t.Fatalf("native disposition=%v elapsed=%s; want refused/accepted without waiting", counts, time.Since(started))
				}
			default:
				t.Fatal("ready later native batch did not finish before peer credit")
			}
			if count, size := f.source.resendQueue.QueueSize(); count != 1 || size <= 1 || !f.source.resendCapacityUnavailable.Load() {
				t.Fatal("native progress manufactured delivery credit or consumed the retained original")
			}
			if len(f.otherRoute) != 1 || len(f.base.route) != 0 {
				t.Fatal("independently ready destination did not reach its real route alone")
			}
			wire := <-f.otherRoute
			pack := decodeSendPackLifecycleWirePack(t, wire)
			matches := pack.Nack && len(pack.Frames) == 1 && bytes.Equal(pack.Frames[0].MessageBytes, f.udp)
			MessagePoolReturn(wire)
			if !matches {
				t.Fatal("independent native packet lost exact bytes or its existing UDP reliability policy")
			}
			f.base.selected.stateLock.Lock()
			unchanged := f.base.selected.packetStats.sendNackCount == priorCount &&
				f.base.selected.packetStats.sendNackByteCount == priorBytes &&
				f.base.selected.pendingSendTime == priorPending && f.base.selected.endErr == nil
			f.base.selected.stateLock.Unlock()
			if !unchanged || f.base.update.client.Load() != f.base.selected {
				t.Fatal("local refusal changed the retained original's accounting or reset its healthy route")
			}
			f.releasePeerCredit()
			if offerNativeIngressTest(entry, f.base.source, f.ownPackets(f.tcp)) != 1 {
				t.Fatal("refusal committed collapse state or prevented the original source's retransmission")
			}
			f.finishPackets(f.tcp)
		})
	}
}

// The ordinary embedding API keeps its explicit waiting contract. Native
// readiness cannot silently rewrite a generic caller's five-second deadline.
func TestNativeIngressKeepsGenericBlockingContract(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newNativeBatchCreditFixture(t)
		blocked := f.startBatch(false, 5*time.Second, f.tcp)
		synctest.Wait()
		select {
		case <-blocked:
			t.Fatal("ordinary explicit waiting call inherited native readiness semantics")
		default:
		}
		f.releasePeerCredit()
		if result := f.completedBatch(blocked); result.count != 1 || result.accepted != 1 {
			t.Fatal("ordinary waiting caller lost its owned original")
		}
		f.finishPackets(f.tcp)
	})
}
