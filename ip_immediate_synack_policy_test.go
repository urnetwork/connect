// SYNACK early delivery is a narrow producer choice, independent of DATA
// permission and TCP connection validation at the destination.
package connect

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"
	"testing/synctest"

	"github.com/urnetwork/connect/protocol"
	"google.golang.org/protobuf/proto"
)

// Raw and legacy IP frames have exactly the same complete packet policy.
func synAckPolicyFrame(t *testing.T, packet []byte, kind protocol.MessageType, raw bool) *protocol.Frame {
	t.Helper()
	frame := &protocol.Frame{MessageType: kind, Raw: raw, MessageBytes: packet}
	if !raw {
		var err error
		if kind == protocol.MessageType_IpIpPacketFromProvider {
			frame.MessageBytes, err = proto.Marshal(&protocol.IpPacketFromProvider{IpPacket: &protocol.IpPacket{PacketBytes: packet}})
		} else {
			frame.MessageBytes, err = proto.Marshal(&protocol.IpPacketToProvider{IpPacket: &protocol.IpPacket{PacketBytes: packet}})
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	return frame
}

// TCP options and ECN flags do not turn a complete SYNACK into payload.
func TestImmediateSynAckPolicyAcceptsCompleteOptionsAndEcn(t *testing.T) {
	for _, ipv6 := range []bool{false, true} {
		for _, flags := range []byte{0x10, 0x50, 0x90, 0xd0, 0x12, 0x52, 0x92, 0xd2} {
			for _, options := range []int{0, 12, 40} {
				for _, kind := range []protocol.MessageType{protocol.MessageType_IpIpPacketFromProvider, protocol.MessageType_IpIpPacketToProvider} {
					for _, raw := range []bool{false, true} {
						packet := protocolClassTestPacket(ipv6, flags, options, 0)
						frame := synAckPolicyFrame(t, packet, kind, raw)
						if !immediateIpAckFrames([]*protocol.Frame{frame}) || !protocolOnlyFrames([]*protocol.Frame{frame}) {
							t.Fatalf("complete ACK/SYNACK refused: ipv6=%t flags=%x options=%d kind=%v raw=%t", ipv6, flags, options, kind, raw)
						}
					}
				}
			}
		}
	}
	for _, extension := range []byte{0, 43, 60, 51} {
		packet := protocolClassTestIpv6Extension(protocolClassTestPacket(true, 0x12, 12, 0), extension)
		if !immediateIpAckFrame(protocol.MessageType_IpIpPacketFromProvider, true, packet) {
			t.Fatalf("complete unfragmented SYNACK extension refused: %d", extension)
		}
	}
}

// No payload-bearing handshake, lifecycle sibling, malformed or fragmented
// packet acquires the new option. Existing atomic-fragment ACK policy stays.
func TestImmediateSynAckPolicyRejectsPayloadLifecycleAndFragments(t *testing.T) {
	for _, ipv6 := range []bool{false, true} {
		for _, flags := range []byte{0x02, 0x42, 0x01, 0x11, 0x04, 0x14, 0x13, 0x16, 0x32, 0x1a} {
			if immediateIpAckFrame(protocol.MessageType_IpIpPacketFromProvider, true, protocolClassTestPacket(ipv6, flags, 12, 0)) {
				t.Fatalf("lifecycle/noncanonical control became immediate: ipv6=%t flags=%x", ipv6, flags)
			}
		}
		for _, flags := range []byte{0x10, 0x02, 0x12, 0xd2} {
			packet := protocolClassTestPacket(ipv6, flags, 12, 1)
			if immediateIpAckFrame(protocol.MessageType_IpIpPacketFromProvider, true, packet) || protocolOnlyFrame(protocol.MessageType_IpIpPacketFromProvider, true, packet) {
				t.Fatal("DATA or TCP fast-open payload acquired protocol permission")
			}
		}
	}
	base := protocolClassTestPacket(false, 0x12, 0, 0)
	bad := [][]byte{nil, base[:19], base[:39], {0x45}}
	for _, change := range []func([]byte){
		func(p []byte) { p[2], p[3] = 0, 39 },
		func(p []byte) { p[6] = 0x20 },
		func(p []byte) { p[7] = 1 },
		func(p []byte) { p[32] = 0x40 },
		func(p []byte) { p[32] = 0xf0 },
		func(p []byte) { p[32] |= 0x02 },
	} {
		packet := bytes.Clone(base)
		change(packet)
		bad = append(bad, packet)
	}
	for _, fragment := range []uint16{0, 1, 8, 9} {
		packet := protocolClassTestIpv6Extension(protocolClassTestPacket(true, 0x12, 0, 0), 44)
		binary.BigEndian.PutUint16(packet[42:44], fragment)
		bad = append(bad, packet)
	}
	for index, packet := range bad {
		if immediateIpAckFrame(protocol.MessageType_IpIpPacketFromProvider, true, packet) {
			t.Fatalf("malformed/fragmented SYNACK became immediate: %d", index)
		}
	}
	atomicAck := protocolClassTestIpv6Extension(protocolClassTestPacket(true, 0x10, 0, 0), 44)
	if !immediateIpAckFrame(protocol.MessageType_IpIpPacketFromProvider, true, atomicAck) {
		t.Fatal("new SYNACK policy changed existing complete atomic-fragment ACK behavior")
	}
}

// A whole group must qualify; reliability is resolved by the actual Client.
func TestImmediateSynAckWholeGroupPreservesOptionAuthority(t *testing.T) {
	client := &Client{ctx: context.Background(), settings: DefaultClientSettings()}
	synAck := synAckPolicyFrame(t, protocolClassTestPacket(false, 0x12, 12, 0), protocol.MessageType_IpIpPacketToProvider, true)
	ack := synAckPolicyFrame(t, protocolClassTestPacket(true, 0x10, 0, 0), protocol.MessageType_IpIpPacketToProvider, false)
	base := []any{TransferOptions{Ack: false, ForceStream: true, CompanionContract: true}}
	for _, frames := range [][]*protocol.Frame{{synAck}, {synAck, ack}, {ack, synAck, synAck}} {
		options := client.resolveSendOptions(multiClientProtocolTransferOpts(base, frames)).transferOptions
		if !options.Ack || !options.NoBudget || !options.DeliverImmediately || !options.ForceStream || !options.CompanionContract {
			t.Fatal("complete SYNACK group lost reliability, byte exemption or routing")
		}
	}
	for _, sibling := range []*protocol.Frame{
		synAckPolicyFrame(t, protocolClassTestPacket(false, 0x02, 0, 0), protocol.MessageType_IpIpPacketToProvider, true),
		synAckPolicyFrame(t, protocolClassTestPacket(false, 0x11, 0, 0), protocol.MessageType_IpIpPacketToProvider, true),
		synAckPolicyFrame(t, protocolClassTestPacket(false, 0x14, 0, 0), protocol.MessageType_IpIpPacketToProvider, true),
		synAckPolicyFrame(t, protocolClassTestPacket(false, 0x12, 0, 1), protocol.MessageType_IpIpPacketToProvider, true),
		{MessageType: protocol.MessageType_IpIpPacketToProvider, Raw: true, MessageBytes: []byte{0x45}},
		{MessageType: protocol.MessageType_TestSimpleMessage}, nil,
	} {
		for _, frames := range [][]*protocol.Frame{{synAck, sibling}, {sibling, synAck}} {
			options := client.resolveSendOptions(multiClientProtocolTransferOpts(base, frames)).transferOptions
			if options.DeliverImmediately || options.Ack || options.NoBudget != protocolOnlyFrames(frames) || !options.ForceStream || !options.CompanionContract {
				t.Fatal("SYNACK widened its sibling's whole-group authority")
			}
		}
	}
}

// Public native input reaches the actual queued SendPack, with default Ack
// disabled to prove the immediate option itself retains reliable ownership.
func TestNativeSynAckProducerSelectsReliableImmediateOption(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newGroupDispositionQueueFixture(t, nil, func(settings *ClientSettings) {
			settings.DefaultTransferOpts.Ack = false
			settings.SendBufferSettings.SequenceBufferSize = 4
		})
		packet := groupDispositionControlPacket(700, 901, 64, tcpFlagSyn|tcpFlagAck)
		owned := MessagePoolCopy(packet)
		f.witnesses = append(f.witnesses, MessagePoolShareReadOnly(owned))
		accepted := make([]bool, 1)
		count := f.parent.SendNativePacketBatchWithResults(f.source, protocol.ProvideMode_Network, [][]byte{owned}, 0, accepted)
		if count != 1 || !accepted[0] {
			t.Fatal("public native SYNACK was not admitted")
		}
		f.requireParked()
		f.client.sendBuffer.mutex.Lock()
		var sequence *SendSequence
		for _, candidate := range f.client.sendBuffer.sendSequencesBySequenceId {
			if candidate.destination == f.peer {
				sequence = candidate
			}
		}
		f.client.sendBuffer.mutex.Unlock()
		if sequence == nil {
			t.Fatal("public native offer has no source owner")
		}
		pack := ipProtocolTakeOffer(t, sequence, true)
		valid := pack.Ack && pack.NoBudget && pack.DeliverImmediately && len(pack.frameList()) == 1 && bytes.Equal(pack.frameList()[0].MessageBytes, packet)
		sequence.packs <- pack // restore the same actual owner for ordinary close
		if err := f.closeClient(t.Context()); err != nil {
			t.Fatal(err)
		}
		if f.started.Load() != 1 || f.terminal.Load() != 1 {
			t.Fatal("native original did not settle exactly once on real source close")
		}
		if !valid {
			t.Error("SYNACK_NATIVE_EARLY_POLICY_REQUIRED: actual public native SYNACK lacks reliable early-delivery option")
		}
	})
}

// Both actual provider serialization entry points, including legacy IP
// wrappers, resolve the same reliable option and retain exact packet order.
func TestProviderSynAckProducerOptionsRawAndLegacy(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, version := range []int{1, 2} {
		provider, sequence, source, key := ipProtocolProviderOffer(t, version)
		for _, flags := range []byte{tcpFlagSyn | tcpFlagAck, tcpFlagSyn | tcpFlagAck | 0xc0, tcpFlagSyn, tcpFlagFin | tcpFlagAck, tcpFlagRst | tcpFlagAck} {
			for _, payload := range [][]byte{nil, []byte("tcp-fast-open")} {
				packet := ipProtocolOfferPacket(flags, payload)
				item := &providerReturnItem{source: source, transferKey: providerReplyTransferKey(key, protocol.ProvideMode_Public),
					provideMode: protocol.ProvideMode_Public, ipProtocol: IpProtocolTcp, packet: MessagePoolCopy(packet), packetByteCount: ByteCount(len(packet))}
				if !provider.sendReturnPacket(item) {
					t.Fatalf("v%d actual provider single offer refused", version)
				}
				pack := ipProtocolTakeOffer(t, sequence, len(payload) == 0)
				got, err := ipPacketFromProviderBytes(pack.Frame)
				wantImmediate := flags&0x17 == 0x12 && len(payload) == 0
				valid := err == nil && bytes.Equal(got, packet) && pack.Ack && pack.NoBudget == (len(payload) == 0) && pack.DeliverImmediately == wantImmediate && pack.ForceStream && pack.CompanionContract
				pack.returnFrames()
				pack.releaseRaw()
				if !valid {
					t.Fatalf("v%d flags=%x payload=%d changed actual provider single options", version, flags, len(payload))
				}
			}
		}
		for _, siblingFlags := range []byte{tcpFlagAck, tcpFlagSyn | tcpFlagAck, tcpFlagSyn, tcpFlagFin | tcpFlagAck, tcpFlagRst | tcpFlagAck} {
			packets := [][]byte{ipProtocolOfferPacket(tcpFlagSyn|tcpFlagAck, nil), ipProtocolOfferPacket(siblingFlags, nil)}
			item := &providerReturnItem{source: source, transferKey: providerReplyTransferKey(key, protocol.ProvideMode_Public),
				provideMode: protocol.ProvideMode_Public, ipProtocol: IpProtocolTcp,
				packets: [][]byte{MessagePoolCopy(packets[0]), MessagePoolCopy(packets[1])}, packetByteCount: ByteCount(len(packets[0]) + len(packets[1]))}
			if !provider.sendReturnBatchWithLimits(item, 2, providerReturnBatchMaxBytes) {
				t.Fatalf("v%d actual provider batch offer refused", version)
			}
			pack := ipProtocolTakeOffer(t, sequence, true)
			wantImmediate := siblingFlags == tcpFlagAck || siblingFlags == tcpFlagSyn|tcpFlagAck
			valid := len(pack.Frames) == 2 && pack.Ack && pack.NoBudget && pack.DeliverImmediately == wantImmediate && pack.ForceStream && pack.CompanionContract
			for index, frame := range pack.Frames {
				got, err := ipPacketFromProviderBytes(frame)
				valid = valid && index < len(packets) && err == nil
				if index < len(packets) {
					valid = valid && bytes.Equal(got, packets[index])
				}
			}
			pack.returnFrames()
			pack.releaseRaw()
			if !valid {
				t.Fatalf("v%d sibling=%x widened actual whole provider group", version, siblingFlags)
			}
		}
	}
}

// The single-client IP producer reaches the same actual Client admission
// path; a default NoAck must not make the new early handshake unreliable.
func TestLocalClientSynAckProducerSelectsReliableImmediateOption(t *testing.T) {
	assertMessagePoolOwnership(t)
	provider, _, source, _ := ipProtocolProviderOffer(t, 2, false)
	client := provider.client
	sequence := installProviderReturnTestSequence(t, provider, client, sendSequenceId{Destination: source.SourceId, EncryptionRole: sequenceTlsRoleClient})
	sequence.packs = make(chan *SendPack, 4)
	sequence.protocolPacks = nil
	t.Cleanup(func() {
		for len(sequence.packs) != 0 {
			pack := <-sequence.packs
			pack.returnFrames()
			pack.releaseRaw()
		}
	})
	remote := &RemoteUserNatClient{client: client, securityPolicy: DisableSecurityPolicyWithStats(client.Ctx(), DefaultSecurityPolicyStatsCollector()),
		pathTable: newPathTable([]MultiHopId{RequireMultiHopId(source.SourceId)}), provideMode: protocol.ProvideMode_Network}
	packet := ipProtocolOfferPacket(tcpFlagSyn|tcpFlagAck, nil)
	owned := MessagePoolCopy(packet)
	if !remote.SendPacket(SourceId(NewId()), protocol.ProvideMode_Network, owned, 0) {
		MessagePoolReturn(owned)
		t.Fatal("single-client SYNACK offer refused")
	}
	pack := ipProtocolTakeOffer(t, sequence, true)
	got, err := ipPacketToProviderBytes(pack.Frame)
	valid := err == nil && bytes.Equal(got, packet) && pack.Ack && pack.NoBudget && pack.DeliverImmediately
	pack.returnFrames()
	pack.releaseRaw()
	if !valid {
		t.Fatal("single-client SYNACK lost actual reliable early-delivery options")
	}
}
