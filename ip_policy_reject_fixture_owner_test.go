// Pin the policy fixture's real local-nat owner boundary. A changed packet
// source is a different tcp owner even when its ip tuple is unchanged.
package connect

import (
	"bytes"
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Every packet sent by the existing scalar helper must reach the syn's owner.
// Before the fixture fix only the syn reaches it; all three payloads get resets.
func TestPolicyRejectFixtureKeepsOneLocalTcpOwner(t *testing.T) {
	assertPolicyRejectLocalOwner(t, false, false)
}

// A real batch keeps the same owner; its admission count is not an ownership
// boolean and no rejected input may be returned again by the caller.
func TestPolicyRejectBatchKeepsOneLocalTcpOwner(t *testing.T) {
	assertPolicyRejectLocalOwner(t, true, false)
}

// A genuinely different source must still get the nat's orphan reset. This
// distinguishes the fixture repair from suppressing a production reject.
func TestPolicyRejectDifferentSourceStillGetsOrphanReset(t *testing.T) {
	assertPolicyRejectLocalOwner(t, false, true)
}

// The packet-disposition edge follows the orphan callback and its pooled
// return. The held dial supplies neither a socket nor a timing-based reply.
func assertPolicyRejectLocalOwner(t *testing.T, batch bool, differentSource bool) {
	t.Helper()
	capture := &policyRejectCapture{}
	multi := newPolicyRejectMulti(t, capture, true)
	multi.SetBlockActionOverrides([]*BlockActionOverride{{
		OverrideId:    NewId(),
		Hosts:         []string{policyRejectDestinationIp.String()},
		RouteOverride: &RouteOverride{Local: true},
	}})

	dialEntered := make(chan struct{}, 1)
	dialExited := make(chan struct{}, 1)
	multi.localUserNat.settings.TcpBufferSettings.DialContextSettings = &DialContextSettings{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			dialEntered <- struct{}{}
			<-ctx.Done()
			dialExited <- struct{}{}
			return nil, ctx.Err()
		},
	}
	// This cleanup also joins the unchanged pre-fix helper, whose cleanup
	// only cancels. A failed assertion must not leave the held dial behind.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := multi.CloseAndWait(ctx); err != nil {
			t.Errorf("local NAT did not join: %v", err)
		}
	})

	var stateLock sync.Mutex
	var owners []*TcpSequence
	if multi.localUserNat.settings.TcpBufferSettings.beforeSequenceSendForTest != nil {
		t.Fatal("local TCP owner observation hook is already occupied")
	}
	multi.localUserNat.settings.TcpBufferSettings.beforeSequenceSendForTest = func(owner *TcpSequence) {
		stateLock.Lock()
		defer stateLock.Unlock()
		owners = append(owners, owner)
	}
	processed := make(chan struct{}, 4)
	previousAfter := multi.localUserNat.afterSendPacketForTest
	multi.localUserNat.afterSendPacketForTest = func() {
		if previousAfter != nil {
			previousAfter()
		}
		processed <- struct{}{}
	}
	waitProcessed := func(packetCount int) {
		for i := 0; i < packetCount; i += 1 {
			select {
			case <-processed:
			case <-time.After(5 * time.Second):
				t.Fatalf("packet %d/%d did not complete NAT disposition", i+1, packetCount)
			}
		}
	}

	const sourcePort = 47101
	policyRejectSend(multi, IpProtocolTcp, sourcePort, true, nil)
	waitProcessed(1)
	stateLock.Lock()
	initialOwners := append([]*TcpSequence(nil), owners...)
	stateLock.Unlock()
	if len(initialOwners) != 1 {
		t.Fatalf("SYN reached %d TCP owners, want one", len(initialOwners))
	}
	initialOwner := initialOwners[0]
	select {
	case <-dialEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("SYN owner did not enter its owned dial")
	}

	payloadCount := 3
	if differentSource {
		payloadCount = 1
		packet := MessagePoolCopy(craftSecurityPacket(IpProtocolTcp, policyRejectSourceIp, sourcePort, policyRejectDestinationIp, policyRejectPort, false, encryptedPayload(512)))
		if !multi.SendPacket(SourceId(NewId()), protocol.ProvideMode_Public, packet, 0) {
			MessagePoolReturn(packet)
			t.Fatal("different-source packet did not enter the local dispatcher")
		}
	} else if batch {
		packets := make([][]byte, payloadCount)
		for i := range packets {
			packets[i] = MessagePoolCopy(craftSecurityPacket(IpProtocolTcp, policyRejectSourceIp, sourcePort, policyRejectDestinationIp, policyRejectPort, false, encryptedPayload(512)))
		}
		// SendPacketBatch consumes all inputs, whether or not it admits them.
		if admitted := multi.SendPacketBatch(initialOwner.source, protocol.ProvideMode_Public, packets, 0); admitted != payloadCount {
			t.Fatalf("batch admitted %d packets, want %d", admitted, payloadCount)
		}
	} else {
		for i := 0; i < payloadCount; i += 1 {
			policyRejectSend(multi, IpProtocolTcp, sourcePort, false, encryptedPayload(512))
		}
	}
	waitProcessed(payloadCount)

	stateLock.Lock()
	observedOwners := append([]*TcpSequence(nil), owners...)
	stateLock.Unlock()
	wantOwners := 1 + payloadCount
	wantIngress := int64(0)
	if differentSource {
		wantOwners = 1
		wantIngress = 1
	}
	if len(observedOwners) != wantOwners {
		t.Errorf("local TCP owner lookups = %d, want %d for SYN and payloads", len(observedOwners), wantOwners)
	}
	for i, owner := range observedOwners {
		if owner != initialOwner || owner.source.LocalMask() != initialOwner.source.LocalMask() {
			t.Errorf("packet %d changed the SYN's exact TCP owner", i)
		}
	}
	packets := capture.take()
	if differentSource {
		requireTcpReset(t, "different-source orphan", packets, sourcePort)
	} else if len(packets) != 0 {
		t.Errorf("same-owner local flow received %d replies while upstream was held", len(packets))
	}
	stats := multi.PacketStats()
	if stats.LocalEgressPacketCount != int64(1+payloadCount) || stats.BlockEgressPacketCount != 0 || stats.LocalIngressPacketCount != wantIngress {
		t.Errorf("local=%d block=%d local replies=%d, want %d/0/%d", stats.LocalEgressPacketCount, stats.BlockEgressPacketCount, stats.LocalIngressPacketCount, 1+payloadCount, wantIngress)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := multi.CloseAndWait(ctx); err != nil {
		t.Fatalf("held-dial owner did not join: %v", err)
	}
	select {
	case <-dialExited:
	default:
		t.Fatal("NAT reported joined before its owned dial returned")
	}
	if packets := capture.take(); len(packets) != 0 {
		t.Errorf("cancellation emitted %d new replies", len(packets))
	}
}

// Taking one batch cannot expose borrowed packet storage or make a later
// callback mutate the previously returned batch.
func TestPolicyRejectCaptureOwnsBorrowedBytesAcrossTake(t *testing.T) {
	capture := &policyRejectCapture{}
	first := []byte{1, 2, 3}
	capture.receive(TransferPath{}, protocol.ProvideMode_Public, nil, first)
	first[0] = 9
	firstBatch := capture.take()
	second := []byte{4, 5, 6}
	capture.receive(TransferPath{}, protocol.ProvideMode_Public, nil, second)
	second[0] = 9
	secondBatch := capture.take()
	if len(firstBatch) != 1 || !bytes.Equal(firstBatch[0], []byte{1, 2, 3}) ||
		len(secondBatch) != 1 || !bytes.Equal(secondBatch[0], []byte{4, 5, 6}) {
		t.Fatalf("callback ownership changed: first=%v second=%v", firstBatch, secondBatch)
	}
	if packets := capture.take(); len(packets) != 0 {
		t.Fatalf("take retained %d packets", len(packets))
	}
}
