// Provider registration permits only complete raw provider heads in recovery.
// These worker-free cases preserve genuine pooled owners and RX loan portions;
// restoring each excluded shape proves available memory did not mask policy.
package connect

import (
	"bytes"
	"reflect"
	"testing"

	"github.com/urnetwork/connect/protocol"
	"google.golang.org/protobuf/proto"
)

// Changes one physical payload root in place and restores it before any owner
// return. The fixture alone owns the temporary ordinary Go scratch copy.
func privateProviderRecoveryExcludedShape(t *testing.T, pack *protocol.Pack, item *receiveItem, shape string) func() {
	t.Helper()
	if shape == "datagram_noack" {
		nack, ack := pack.Nack, item.ack
		pack.Nack, item.ack = true, false
		originals := make([][]byte, len(pack.Frames))
		for index, frame := range pack.Frames {
			originals[index] = append([]byte(nil), frame.MessageBytes...)
			packet := ipOosUdpPacket(udpTestPath(4), make([]byte, len(frame.MessageBytes)-Ipv4HeaderSizeWithoutExtensions-UdpHeaderSize))
			copy(frame.MessageBytes, packet)
		}
		return func() {
			for index, frame := range pack.Frames {
				copy(frame.MessageBytes, originals[index])
			}
			pack.Nack, item.ack = nack, ack
		}
	}
	frame := pack.Frames[0]
	messageType, raw, payload := frame.MessageType, frame.Raw, frame.MessageBytes
	original := append([]byte(nil), payload...)
	nack, ahead := pack.Nack, pack.ContractAhead
	ack, itemAhead := item.ack, item.contractAhead
	restore := func() {
		copy(payload, original)
		frame.MessageType, frame.Raw, frame.MessageBytes = messageType, raw, payload
		pack.Nack, pack.ContractAhead = nack, ahead
		item.ack, item.contractAhead = ack, itemAhead
	}
	switch shape {
	case "mixed_consumer":
		frame.MessageType = protocol.MessageType_IpIpPacketFromProvider
	case "wrapped":
		wrapped, err := proto.Marshal(&protocol.IpPacketToProvider{IpPacket: &protocol.IpPacket{PacketBytes: original}})
		if err != nil || cap(payload) < len(wrapped) {
			t.Fatal("excluded wrapped shape does not fit its original paid payload root")
		}
		frame.MessageBytes = payload[:len(wrapped)]
		copy(frame.MessageBytes, wrapped)
		frame.Raw = false
	case "malformed":
		frame.MessageBytes = payload[:Ipv4HeaderSizeWithoutExtensions-1]
	case "fragment":
		frame.MessageBytes[6] |= 0x20
	case "malformed_datagram", "fragment_datagram":
		packet := ipOosUdpPacket(udpTestPath(4), make([]byte, len(payload)-Ipv4HeaderSizeWithoutExtensions-UdpHeaderSize))
		copy(payload, packet)
		if shape == "malformed_datagram" {
			// Keep a complete IPv4 carrier, but declare less than one UDP
			// header so the actual borrowed transport parser must reject it.
			payload[Ipv4HeaderSizeWithoutExtensions+4], payload[Ipv4HeaderSizeWithoutExtensions+5] = 0, byte(UdpHeaderSize-1)
		} else {
			payload[6] |= 0x20
		}
	case "encrypted_control":
		frame.MessageType = protocol.MessageType_TransferEncryptedControl
	case "nack":
		pack.Nack, item.ack = true, false
	case "contract_ahead":
		pack.ContractAhead, item.contractAhead = true, true
	default:
		t.Fatalf("unknown excluded provider shape %q", shape)
	}
	return restore
}

// A registered provider cannot relabel mixed families, legacy, NoAck, control or
// malformed decoded data as a whole-D head, even with exact live RX loans.
func TestPrivateProviderRecoveryRejectsExcludedDecodedShapes(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, shape := range []string{"mixed_consumer", "wrapped", "datagram_noack", "malformed", "fragment", "malformed_datagram", "fragment_datagram", "encrypted_control", "nack", "contract_ahead"} {
		func() {
			f := newPrivateProviderHeadRecoveryReviewFixture(t, true, true)
			owner := f.take()
			restore := privateProviderRecoveryExcludedShape(t, &owner.pack, &owner.receiveItem, shape)
			defer restore()
			before := f.root.StatsWithDescendants(f.parent, f.rx, f.recovery, f.sibling)
			promise, pressure := f.sequence.holdPromise, f.sequence.memoryPressure
			if f.sequence.recoverProviderReceiveDeliveryCredit(&owner.receiveItem, f.additional) ||
				!reflect.DeepEqual(before, f.root.StatsWithDescendants(f.parent, f.rx, f.recovery, f.sibling)) ||
				promise != f.sequence.holdPromise || pressure != f.sequence.memoryPressure ||
				owner.receiveItem.memoryBudget != f.rx || owner.receiveItem.QueueByteCount() != f.scan.retainedByteCount ||
				owner.receivePack.ingressLoanByteCount != f.prefix {
				t.Fatalf("%s migrated an excluded provider owner or changed its exact RX portions", shape)
			}
			restore()
			if !f.sequence.prepareReceiveDeliveryCredit(&owner.receiveItem) || owner.receiveItem.memoryBudget != f.recovery ||
				owner.receiveItem.QueueByteCount() != f.full || f.recovery.UsedByteCount() != f.full {
				t.Fatalf("%s restoration did not admit the same valid paid provider owner", shape)
			}
			// The deferred restoration must precede the fixture's final pool return.
		}()
	}
}

// Real pooled wire classification has the same exclusions when provider
// registration is enabled. A valid raw TCP Pack with identical identity and
// frame geometry can then allocate the child without spending RX's promise.
func TestPrivateProviderRecoveryScannerRejectsExcludedProviderShapes(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, shape := range []string{"mixed_consumer", "wrapped", "datagram_noack", "malformed", "fragment", "malformed_datagram", "fragment_datagram", "encrypted_control", "nack", "contract_ahead"} {
		func() {
			f := newReceiveHeadRecoveryFixture(t)
			client := &Client{}
			client.reliableProviderIngress.Store(true)
			f.buffer.client, f.sequence.client = client, client
			frames := make([]*protocol.Frame, 8)
			for index := range frames {
				packet := ipOosTcpPacketSequence(reliableDeliveryTestPath(4), tcpFlagAck, 11, bytes.Repeat([]byte{0x33}, 1240))
				// Ordinary scratch has spare capacity for the legacy wrapper;
				// only the encoded carrier becomes a tracked pooled root.
				payload := make([]byte, len(packet), len(packet)+64)
				copy(payload, packet)
				frames[index] = &protocol.Frame{MessageType: protocol.MessageType_IpIpPacketToProvider, Raw: true, MessageBytes: payload}
			}
			pack := &protocol.Pack{Frames: frames}
			item := &receiveItem{ack: true}
			restore := privateProviderRecoveryExcludedShape(t, pack, item, shape)
			defer restore()
			scanWire := func() receiveMemoryPackScan {
				encoded := sendPackFrame{messageId: f.scan.messageId, sequenceId: f.scan.sequenceId,
					sequenceNumber: f.scan.sequenceNumber, frames: pack.Frames, nack: pack.Nack, contractAhead: pack.ContractAhead}
				wire := MessagePoolCopy(receiveMemoryScanTestWire(f.scan.path, encoded.appendPack(nil), false))
				defer MessagePoolReturn(wire)
				scan, status := scanReceiveMemoryPack(wire, f.scan.path)
				if status != receiveMemoryPackScanReady || !scan.loanSafe ||
					scan.retainedByteCount > f.recovery.Available() ||
					scan.retainedByteCount <= f.sequence.holdPromise.reservedByteCount+f.sequence.memoryPressure.reservedByteCount {
					t.Fatalf("%s has no exact bounded provider predecode refusal geometry", shape)
				}
				return scan
			}
			scan := scanWire()
			before := f.root.StatsWithDescendants(f.rx, f.recovery)
			promise, pressure := f.sequence.holdPromise, f.sequence.memoryPressure
			if scan.providerFrameCount == scan.frameCount && !scan.nack && !scan.contractAhead {
				t.Fatalf("%s was classified as an unrestricted raw provider Pack", shape)
			}
			if owner := f.buffer.takeReceiveHeadRecoveryOwner(f.sequence, scan); owner != nil {
				owner.release()
				t.Fatalf("%s acquired provider predecode recovery", shape)
			}
			if !reflect.DeepEqual(before, f.root.StatsWithDescendants(f.rx, f.recovery)) ||
				promise != f.sequence.holdPromise || pressure != f.sequence.memoryPressure {
				t.Fatalf("%s predecode refusal changed paid RX or child ownership", shape)
			}
			restore()
			scan = scanWire()
			if scan.providerFrameCount != scan.frameCount || scan.providerTcpFrameCount != scan.frameCount || scan.frameCount != 8 {
				t.Fatal("restored provider wire did not scan as eight complete raw TCP frames")
			}
			owner := f.buffer.takeReceiveHeadRecoveryOwner(f.sequence, scan)
			if owner == nil {
				t.Fatalf("%s restoration did not admit the valid provider predecode owner", shape)
			}
			f.owner = owner
			if owner.receiveItem.memoryBudget != f.recovery || owner.receivePack.ingressLoanByteCount != 0 ||
				f.sequence.holdPromise.reservedByteCount != promise.reservedByteCount ||
				f.sequence.memoryPressure != pressure || f.recovery.UsedByteCount() != scan.retainedByteCount {
				t.Fatal("valid provider predecode admission consumed existing RX loans")
			}
			owner.release()
			f.owner = nil
		}()
	}
}
