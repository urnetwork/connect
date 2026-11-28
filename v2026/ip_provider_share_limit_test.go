// The provider's return sender waits on the owner's share limiter before any
// Transfer attempt: a return the rate does not yet cover leaves nothing queued
// until the rate covers it, and a wait the provider's close ends sends nothing
// and counts as a refused send.
package connect

import (
	"bytes"
	"context"
	"net"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

// A public TCP return of one small packet through the paused fixture's
// sequence, with the bucket of a 1,000,000 bytes/s limiter already spent.
type providerShareLimitFixture struct {
	clock    *providerShareLimiterTestClock
	provider *RemoteUserNatProvider
	sequence *SendSequence
	source   TransferPath
	key      TransferKey
	response []byte
	path     *IpPath
	sends    chan providerReturnSendResult
}

func newProviderShareLimitFixture(t *testing.T) *providerShareLimitFixture {
	t.Helper()
	clock := newProviderShareLimiterTestClock(t)
	limiter := newProviderShareLimiterForTest(clock, 1000*1000)
	if !limiter.Wait(context.Background(), 200*1000) {
		t.Fatal("limiter refused its own burst")
	}
	settings := DefaultRemoteUserNatProviderSettings()
	settings.ShareLimiter = limiter
	provider, client, _ := newProviderTransferKeyTestFixtureWithSettings(t, settings)
	sends := make(chan providerReturnSendResult, 4)
	provider.afterReturnSendForTest = func(result providerReturnSendResult) {
		sends <- result
	}
	peerId := NewId()
	sequence := installProviderReturnTestSequence(t, provider, client, sendSequenceId{
		Destination:       peerId,
		CompanionContract: true,
		ForceStream:       true,
		EncryptionRole:    sequenceTlsRoleServer,
	})
	response := craftSecurityPacket(
		IpProtocolTcp,
		net.ParseIP("203.0.113.7"),
		8080,
		net.ParseIP("10.0.0.9"),
		42001,
		false,
		[]byte("HTTP/1.1 200 OK\r\n\r\n"),
	)
	path, err := ParseIpPath(response)
	if err != nil {
		t.Fatalf("parse provider return packet: %v", err)
	}
	return &providerShareLimitFixture{
		clock:    clock,
		provider: provider,
		sequence: sequence,
		source:   SourceId(peerId),
		key: TransferKey{
			ForceStream:    true,
			EncryptionRole: protocol.SequenceRole_SequenceRoleServer,
		},
		response: response,
		path:     path,
		sends:    sends,
	}
}

// The return as the local NAT hands it over, then the wait the sender asks for:
// the packet's own bytes at a microsecond each.
func (self *providerShareLimitFixture) returnAndRequireWait(t *testing.T) {
	t.Helper()
	self.provider.receiveTransfer(self.source, self.key, protocol.ProvideMode_Public, self.path, self.response)
	requireProviderShareLimiterWait(t, self.clock, time.Duration(len(self.response))*time.Microsecond)
}

func (self *providerShareLimitFixture) requireNothingQueued(t *testing.T) {
	t.Helper()
	select {
	case queued := <-self.sequence.packs:
		queued.returnFrames()
		t.Fatal("provider queued a return the share limit did not cover")
	default:
	}
}

// Until the limiter's timer fires the return has reached no Transfer sequence;
// once it fires the exact packet is queued.
func TestRemoteUserNatProviderShareLimiterHoldsReturnUntilTheRateCoversIt(t *testing.T) {
	assertMessagePoolOwnership(t)
	fixture := newProviderShareLimitFixture(t)
	fixture.returnAndRequireWait(t)
	fixture.requireNothingQueued(t)
	select {
	case result := <-fixture.sends:
		t.Fatalf("provider completed a paced return before its timer: %+v", result)
	default:
	}

	fixture.clock.fire <- fixture.clock.nowTime
	queued := waitProviderReturnTestPack(t, fixture.sequence)
	defer queued.returnFrames()
	packetBytes, err := ipPacketFromProviderBytes(queued.Frame)
	if err != nil {
		t.Fatalf("decode provider return: %v", err)
	}
	if !bytes.Equal(packetBytes, fixture.response) {
		t.Fatal("paced provider return changed the packet bytes")
	}
	select {
	case result := <-fixture.sends:
		if !result.sent || result.packetCount != 1 || result.packetByteCount != ByteCount(len(fixture.response)) {
			t.Fatalf("paced return completion = %+v, want one sent packet of %d bytes", result, len(fixture.response))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("paced return never completed")
	}
	if drops := fixture.provider.CongestionDropStats(); drops != (ProviderCongestionDrops{}) {
		t.Fatalf("paced return counted drops: %+v", drops)
	}
}

// A provider that closes while its return waits on the limiter sends nothing:
// the return completes unsent, counts as a refused send and never reaches
// Transfer.
func TestRemoteUserNatProviderShareLimiterWaitEndedByCloseSendsNothing(t *testing.T) {
	assertMessagePoolOwnership(t)
	fixture := newProviderShareLimitFixture(t)
	fixture.returnAndRequireWait(t)

	fixture.provider.cancel()
	select {
	case result := <-fixture.sends:
		if result.sent || result.packetCount != 1 || result.packetByteCount != ByteCount(len(fixture.response)) {
			t.Fatalf("canceled paced return completion = %+v, want one unsent packet of %d bytes", result, len(fixture.response))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled paced return never completed")
	}
	fixture.requireNothingQueued(t)
	wantDrops := ProviderCongestionDrops{
		ReturnSendPacketCount: 1,
		ReturnSendByteCount:   ByteCount(len(fixture.response)),
	}
	if drops := fixture.provider.CongestionDropStats(); drops != wantDrops {
		t.Fatalf("canceled paced return drops = %+v, want %+v", drops, wantDrops)
	}
	if stats := fixture.provider.PacketStats(); stats.RemoteEgressPacketCount != 0 || stats.RemoteEgressByteCount != 0 {
		t.Fatalf("canceled paced return counted remote egress: %+v", stats)
	}
}
