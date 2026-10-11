// A provider's socket-owned TCP return to a destination that went away mid
// transfer, the shape a retired client window or a closed probe client leaves
// behind. Its producer has finished, so nothing parks and the provider's own
// unreachable-source release never runs, and during an outage the degraded
// backend gates that release off as well. Transfer itself must bound it.
package connect

import (
	"context"
	"net"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

func TestRemoteUserNatProviderStopsRewritingGoneTcpReturnWhileBackendDegraded(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		parked, unpark := context.WithCancel(context.Background())
		defer unpark()
		provider, _, client := newProviderSourceLifecycleTestFixtureWithClientSettings(
			t,
			NewNoContractClientOob(),
			func(settings *ClientSettings) {
				settings.Log = NewNoopLogger()
				settings.EncryptionSettings.Mode = EncryptionModeOff
				// background key publication is not part of this flow
				settings.beforeClientKeyPublishForTest = func() { <-parked.Done() }
			},
			func(settings *RemoteUserNatProviderSettings) {
				settings.ReturnSendRetryTimeout = time.Millisecond
			},
		)
		// an outage: the provider's own release would decline even if a
		// producer were parked
		provider.backendDegradedForTest = func() bool { return true }
		provider.hasActiveTransportForTest = func() bool { return true }

		peerId := NewId()
		client.ContractManager().AddNoContractPeer(peerId)
		route := make(Route, 64)
		client.RouteManager().UpdateTransport(NewSendClientTransport(DestinationId(peerId)), []Route{route})
		t.Cleanup(func() {
			unpark()
			for len(route) != 0 {
				MessagePoolReturn(<-route)
			}
		})

		packet := MessagePoolCopy(craftSecurityPacket(
			IpProtocolTcp,
			net.ParseIP("203.0.113.7"),
			443,
			net.ParseIP("10.0.0.9"),
			42001,
			false,
			make([]byte, 1200),
		))
		ipPath, err := ParseIpPath(packet)
		if err != nil {
			MessagePoolReturn(packet)
			t.Fatalf("parse provider TCP return packet: %v", err)
		}
		withBorrowedMessage(packet, func(packet []byte) {
			provider.receiveTransferWithRecovery(
				SourceId(peerId),
				TransferKey{ForceStream: true},
				protocol.ProvideMode_Public,
				receiveRecoveryModeTcpSocket,
				ipPath,
				packet,
			)
		})
		synctest.Wait()

		// The producer returned once Transfer admitted the bytes; the
		// destination never answers any of the writes.
		drain := func() int {
			synctest.Wait()
			count := 0
			for len(route) != 0 {
				MessagePoolReturn(<-route)
				count++
			}
			return count
		}
		settings := DefaultSendBufferSettings()
		writes := drain()
		if writes == 0 {
			t.Fatal("the socket-owned return was never written")
		}
		for elapsed := time.Duration(0); elapsed < settings.RetainedAckTimeout+settings.MaxResendInterval; elapsed += settings.MaxResendInterval {
			time.Sleep(settings.MaxResendInterval)
			writes += drain()
		}
		if bound := int(settings.RetainedAckTimeout/settings.MaxResendInterval) + 4; bound < writes {
			t.Fatalf("%d writes to a gone destination inside the retained bound, want at most %d", writes, bound)
		}
		late := 0
		for range 30 {
			time.Sleep(time.Minute)
			late += drain()
		}
		if late != 0 {
			t.Fatalf("%d writes to a gone destination during the half hour after the retained bound, while the backend was degraded", late)
		}
		evidence := func() *sourceAckEvidence {
			provider.stateLock.Lock()
			defer provider.stateLock.Unlock()
			return provider.sourceAckEvidences[peerId]
		}()
		if evidence != nil && evidence.outstanding.Load() != 0 {
			t.Fatalf("the gone destination still owns %d outstanding returns", evidence.outstanding.Load())
		}
	})
}
