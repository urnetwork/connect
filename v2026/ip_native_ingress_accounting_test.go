// Native refusal counters distinguish original source offers from policy,
// duplicate prevention, successful recovery and terminal transport failures.
package connect

import (
	"bytes"
	"errors"
	"net"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

// A real retained Transfer original closes admission for a two-packet group.
// Releasing its exact peer acknowledgement admits the source's retry intact.
func TestNativeAdmissionRefusalCountsGroupedRetainedCredit(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, throughMux := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			f := newNativeBatchCreditFixture(t)
			second := groupRecoveryPacket(102, 500, 64, tcpFlagAck, []byte{0x44})
			var entry nativeIngressTestBatchEntry = f.base.parent
			if throughMux {
				mux := &UpgradeMux{mux: &IpMux{}}
				mux.mux.SetUpstream(f.base.parent.SendPacket)
				mux.mux.setUpstreamGroupSend(f.base.parent.sendPacketGroup)
				mux.upstreamMultiClient.Store(f.base.parent)
				entry = mux
			}
			if count := offerNativeIngressTest(entry, f.base.source, f.ownPackets(f.tcp, second)); count != 0 {
				t.Fatalf("full retained window accepted %d original packets", count)
			}
			if got := f.base.parent.NativeAdmissionRefusalCount(); got != 2 {
				t.Fatalf("grouped refusal count=%d, want two original packets", got)
			}
			if f.base.parent.PacketStats().BlockEgressPacketCount != 0 || f.base.parent.TcpCollapseDropCount() != 0 {
				t.Fatal("retained-credit refusal changed policy or collapse accounting")
			}
			f.releasePeerCredit()
			if count := offerNativeIngressTest(entry, f.base.source, f.ownPackets(f.tcp, second)); count != 2 {
				t.Fatalf("released source retry accepted %d packets, want 2", count)
			}
			synctest.Wait()
			if len(f.base.route) != 1 {
				t.Fatal("accepted group did not produce its one reliable Pack")
			}
			wire := <-f.base.route
			pack := decodeSendPackLifecycleWirePack(t, wire)
			exact := !pack.Nack && len(pack.Frames) == 2 &&
				bytes.Equal(pack.Frames[0].MessageBytes, f.tcp) && bytes.Equal(pack.Frames[1].MessageBytes, second)
			acked := f.base.client.sendBuffer.Ack(f.base.peer, &protocol.Ack{MessageId: pack.MessageId, SequenceId: pack.SequenceId}, 0)
			MessagePoolReturn(wire)
			if !exact || !acked {
				t.Fatal("grouped retry lost original bytes, order or reliable acknowledgement")
			}
			synctest.Wait()
			if got := f.base.parent.NativeAdmissionRefusalCount(); got != 2 {
				t.Fatalf("successful grouped retry changed refusal count to %d", got)
			}
		})
	}
}

// Selected, singleton and parallel candidate paths keep terminal errors and
// cancellation distinct from ordinary ready-only and encryption refusal.
func TestNativeAdmissionRefusalExcludesTerminalAndCancellation(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, testCase := range []struct {
		name       string
		candidates int
		err        error
		cancel     bool
		cancelFlow bool
		block      bool
		malformed  bool
		ordinary   bool
		want       uint64
	}{
		{name: "selected queue", want: 2},
		{name: "selected encryption", err: ErrEncryptionRequiredNotEstablished, want: 2},
		{name: "selected terminal", err: errors.New("synthetic terminal admission failure")},
		{name: "singleton queue", candidates: 1, want: 2},
		{name: "singleton encryption", candidates: 1, err: ErrEncryptionRequiredNotEstablished, want: 2},
		{name: "singleton terminal", candidates: 1, err: errors.New("synthetic terminal admission failure")},
		{name: "parallel queue", candidates: 2, want: 2},
		{name: "parallel encryption", candidates: 2, err: ErrEncryptionRequiredNotEstablished, want: 2},
		{name: "parallel terminal", candidates: 2, err: errors.New("synthetic terminal admission failure")},
		{name: "canceled owner", cancel: true},
		{name: "canceled flow with live parent", cancelFlow: true},
		{name: "policy refusal", block: true},
		{name: "malformed packet", malformed: true},
		{name: "ordinary caller", ordinary: true},
	} {
		synctest.Test(t, func(t *testing.T) {
			var policy SecurityPolicy = DisableSecurityPolicy()
			if testCase.block {
				policy = &groupTestSecurityPolicy{stats: DefaultSecurityPolicyStatsCollector()}
			}
			parent, update, closeParent := groupTestParent(t, policy)
			defer closeParent()
			channels := make([]*multiClientChannel, max(1, testCase.candidates))
			for index := range channels {
				channel := newPacketTransferTestChannel()
				channel.ctx, channel.settings = parent.ctx, parent.settings
				channel.sendGroupForTest = func(*parsedPacketGroup, time.Duration, bool) (bool, error) {
					return false, testCase.err
				}
				channels[index] = channel
			}
			if testCase.candidates == 0 {
				update.client.Store(channels[0])
			} else {
				parent.groupRaceCandidatesForTest = func(*parsedPacketGroup) []*multiClientChannel { return channels }
			}
			if testCase.cancel {
				closeParent()
			}
			if testCase.cancelFlow {
				update.Close()
				if parent.ctx.Err() != nil {
					t.Fatal("flow cancellation retired its shared parent")
				}
			}
			payload := byte(0x42)
			if testCase.block {
				payload = 0xff
			}
			packets := [][]byte{
				MessagePoolCopy(ipOosUdpPacket(udpTestPath(4), []byte{payload})),
				MessagePoolCopy(ipOosUdpPacket(udpTestPath(4), []byte{payload})),
			}
			if testCase.malformed {
				for index, packet := range packets {
					MessagePoolReturn(packet)
					packets[index] = MessagePoolCopy([]byte{0})
				}
			}
			witnesses := groupTestPacketWitnesses(t, packets)
			defer requireGroupTestWitnessesReleased(t, packets, witnesses)
			accepted := 0
			if testCase.ordinary {
				accepted = parent.SendPacketBatch(SourceId(NewId()), parent.provideMode, packets, 0)
			} else {
				accepted = parent.SendNativePacketBatch(SourceId(NewId()), parent.provideMode, packets, 5*time.Second)
			}
			wantBlocked := int64(0)
			if testCase.block {
				wantBlocked = int64(len(packets))
			}
			if accepted != 0 || parent.NativeAdmissionRefusalCount() != testCase.want ||
				parent.PacketStats().BlockEgressPacketCount != wantBlocked || parent.TcpCollapseDropCount() != 0 {
				t.Fatalf("%s: accepted=%d native-refused=%d want=%d", testCase.name, accepted, parent.NativeAdmissionRefusalCount(), testCase.want)
			}
		})
	}
}

// SMTP uses ordered singular policy beneath the public native batch entry.
// Every refused member must count once, even though no grouped offer is used.
func TestNativeAdmissionRefusalCountsOrderedSmtpMembers(t *testing.T) {
	assertMessagePoolOwnership(t)
	parent, update, closeParent := groupTestParent(t, DisableSecurityPolicy())
	defer closeParent()
	update.client.Store(&multiClientChannel{ctx: parent.ctx, settings: parent.settings,
		sendGroupForTest: func(*parsedPacketGroup, time.Duration, bool) (bool, error) { return false, nil },
	})
	path := &IpPath{Version: 4, Protocol: IpProtocolTcp,
		SourceIp: net.IPv4(192, 0, 2, 21), SourcePort: 43111,
		DestinationIp: net.IPv4(198, 51, 100, 22), DestinationPort: 465}
	packets := make([][]byte, 3)
	for index := range packets {
		packets[index] = MessagePoolCopy(ipOosTcpPacketSequence(path, tcpFlagSyn, uint32(100+index), nil))
	}
	witnesses := groupTestPacketWitnesses(t, packets)
	defer requireGroupTestWitnessesReleased(t, packets, witnesses)
	if count := parent.SendNativePacketBatch(SourceId(NewId()), protocol.ProvideMode_Network, packets, 5*time.Second); count != 0 {
		t.Fatalf("unavailable SMTP route accepted %d packets", count)
	}
	if parent.NativeAdmissionRefusalCount() != 3 || parent.PacketStats().BlockEgressPacketCount != 0 || parent.TcpCollapseDropCount() != 0 {
		t.Fatalf("SMTP refusals: native=%d policy=%d collapsed=%d", parent.NativeAdmissionRefusalCount(), parent.PacketStats().BlockEgressPacketCount, parent.TcpCollapseDropCount())
	}
}
