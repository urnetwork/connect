// Receiver-side nat regressions keep authenticated sources paired with their
// complete reply lanes while socket flows survive transfer reformations.
package connect

import (
	"context"
	"net"
	"testing"

	"github.com/urnetwork/connect/v2026/protocol"
)

// A source/lane snapshot is returned from one NAT callback.
type natTransferSnapshot struct {
	source       TransferPath
	transferKey  TransferKey
	recoveryMode receiveRecoveryMode
}

// Returns one authenticated source and two receive lanes for its flow.
func natTransferKeyPair() (TransferPath, TransferKey, TransferKey) {
	source := TransferPath{
		SourceId: NewId(),
		StreamId: NewId(),
	}
	initial := TransferKey{
		EncryptionRole: protocol.SequenceRole_SequenceRoleClient,
	}
	latest := TransferKey{
		ForceStream:         true,
		CompanionContract:   true,
		EncryptionRole:      protocol.SequenceRole_SequenceRoleServer,
		EncryptionCompanion: true,
	}
	return source, initial, latest
}

// Reproduces a live flow moving to a new transfer lane while its socket stays
// open. Every protocol must return the newest key with the original source.
func TestNatSequencesReturnLatestTransferKey(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	source, initial, latest := natTransferKeyPair()
	sourceIp := net.IPv4(10, 0, 0, 1)
	destinationIp := net.IPv4(203, 0, 113, 1)
	tests := []struct {
		name             string
		wantRecoveryMode receiveRecoveryMode
		receive          func() natTransferSnapshot
	}{
		{
			name:             "udp",
			wantRecoveryMode: receiveRecoveryModeNonblocking,
			receive: func() natTransferSnapshot {
				var received natTransferSnapshot
				sequence := newUdpSequenceWithTransferKey(
					ctx,
					func(source TransferPath, transferKey TransferKey, _ protocol.ProvideMode, recoveryMode receiveRecoveryMode, _ *IpPath, _ []byte) {
						received = natTransferSnapshot{source: source, transferKey: transferKey, recoveryMode: recoveryMode}
					},
					source, initial, protocol.ProvideMode_Network, 4,
					sourceIp, 1000, destinationIp, 2000,
					DefaultUdpBufferSettingsWithBufferSize(1),
				)
				defer sequence.Cancel()
				outbound := MessagePoolCopy([]byte{1})
				success, err := sequence.send(&UdpSendItem{source: source, transferKey: latest, ipPacket: outbound}, 0)
				if err != nil || !success {
					t.Fatalf("update udp lane: success=%t err=%v", success, err)
				}
				queued := <-sequence.sendItems
				defer MessagePoolReturn(queued.ipPacket)
				sequence.receivePacket(MessagePoolCopy([]byte{2}))
				return received
			},
		},
		{
			name:             "tcp",
			wantRecoveryMode: receiveRecoveryModeTcpSocket,
			receive: func() natTransferSnapshot {
				var received natTransferSnapshot
				sequence := newTcpSequenceWithTransferKey(
					ctx,
					func(source TransferPath, transferKey TransferKey, _ protocol.ProvideMode, recoveryMode receiveRecoveryMode, _ *IpPath, _ []byte) {
						received = natTransferSnapshot{source: source, transferKey: transferKey, recoveryMode: recoveryMode}
					},
					source, initial, protocol.ProvideMode_Network, 4,
					sourceIp, 1000, destinationIp, 2000, 1,
					DefaultTcpBufferSettingsWithBufferSize(1),
				)
				defer sequence.Cancel()
				outbound := MessagePoolCopy([]byte{1})
				success, err := sequence.send(&TcpSendItem{source: source, transferKey: latest, ipPacket: outbound}, 0)
				if err != nil || !success {
					t.Fatalf("update tcp lane: success=%t err=%v", success, err)
				}
				queued := <-sequence.sendItems
				defer MessagePoolReturn(queued.ipPacket)
				sequence.receivePacket(MessagePoolCopy([]byte{2}), receiveRecoveryModeTcpSocket)
				return received
			},
		},
		{
			name:             "icmp",
			wantRecoveryMode: receiveRecoveryModeNonblocking,
			receive: func() natTransferSnapshot {
				var received natTransferSnapshot
				sequence := newIcmpSequenceWithTransferKey(
					ctx,
					func(source TransferPath, transferKey TransferKey, _ protocol.ProvideMode, recoveryMode receiveRecoveryMode, _ *IpPath, _ []byte) {
						received = natTransferSnapshot{source: source, transferKey: transferKey, recoveryMode: recoveryMode}
					},
					source, initial, protocol.ProvideMode_Network, 4,
					sourceIp, 1, destinationIp,
					DefaultIcmpBufferSettingsWithBufferSize(1),
				)
				defer sequence.Cancel()
				outbound := MessagePoolCopy([]byte{1})
				success, err := sequence.send(&IcmpSendItem{source: source, transferKey: latest, ipPacket: outbound}, 0)
				if err != nil || !success {
					t.Fatalf("update icmp lane: success=%t err=%v", success, err)
				}
				queued := <-sequence.sendItems
				defer MessagePoolReturn(queued.ipPacket)
				sequence.receivePacket(MessagePoolCopy([]byte{2}))
				return received
			},
		},
	}
	for _, test := range tests {
		want := natTransferSnapshot{
			source:       source.LocalMask(),
			transferKey:  latest,
			recoveryMode: test.wantRecoveryMode,
		}
		if received := test.receive(); received != want {
			t.Errorf("%s transfer snapshot = %#v, want %#v", test.name, received, want)
		}
	}
}

// Verifies every batch callback receives one stable source/key snapshot used
// by the corresponding per-packet path.
func TestNatSequencesBatchOneTransferKeySnapshot(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	source, _, transferKey := natTransferKeyPair()
	wantSource := source.LocalMask()
	sourceIp := net.IPv4(10, 0, 0, 1)
	destinationIp := net.IPv4(203, 0, 113, 1)
	tests := []struct {
		name             string
		wantRecoveryMode receiveRecoveryMode
		receive          func(receiveTransferPacketsBatchFunction)
	}{
		{
			name:             "udp",
			wantRecoveryMode: receiveRecoveryModeNonblocking,
			receive: func(callback receiveTransferPacketsBatchFunction) {
				sequence := newUdpSequenceWithTransferKey(
					ctx, func(TransferPath, TransferKey, protocol.ProvideMode, receiveRecoveryMode, *IpPath, []byte) {},
					source, transferKey, protocol.ProvideMode_Network, 4,
					sourceIp, 1000, destinationIp, 2000,
					DefaultUdpBufferSettings(),
				)
				defer sequence.Cancel()
				sequence.receiveTransferPacketsCallback = callback
				sequence.receiveBatch([][]byte{MessagePoolCopy([]byte{1})})
			},
		},
		{
			name:             "tcp",
			wantRecoveryMode: receiveRecoveryModeTcpSocket,
			receive: func(callback receiveTransferPacketsBatchFunction) {
				sequence := newTcpSequenceWithTransferKey(
					ctx, func(TransferPath, TransferKey, protocol.ProvideMode, receiveRecoveryMode, *IpPath, []byte) {},
					source, transferKey, protocol.ProvideMode_Network, 4,
					sourceIp, 1000, destinationIp, 2000, 1,
					DefaultTcpBufferSettings(),
				)
				defer sequence.Cancel()
				sequence.receiveTransferPacketsCallback = callback
				sequence.receiveBatch(
					[][]byte{MessagePoolCopy([]byte{1})},
					receiveRecoveryModeTcpSocket,
				)
			},
		},
		{
			name:             "icmp",
			wantRecoveryMode: receiveRecoveryModeNonblocking,
			receive: func(callback receiveTransferPacketsBatchFunction) {
				sequence := newIcmpSequenceWithTransferKey(
					ctx, func(TransferPath, TransferKey, protocol.ProvideMode, receiveRecoveryMode, *IpPath, []byte) {},
					source, transferKey, protocol.ProvideMode_Network, 4,
					sourceIp, 1, destinationIp,
					DefaultIcmpBufferSettings(),
				)
				defer sequence.Cancel()
				sequence.receiveTransferPacketsCallback = callback
				sequence.receivePacket(MessagePoolCopy([]byte{1}))
			},
		},
	}
	for _, test := range tests {
		var received natTransferSnapshot
		test.receive(func(source TransferPath, key TransferKey, _ protocol.ProvideMode, recoveryMode receiveRecoveryMode, _ *IpPath, _ [][]byte) bool {
			received = natTransferSnapshot{source: source, transferKey: key, recoveryMode: recoveryMode}
			return true
		})
		want := natTransferSnapshot{
			source:       wantSource,
			transferKey:  transferKey,
			recoveryMode: test.wantRecoveryMode,
		}
		if received != want {
			t.Errorf("%s transfer snapshot = %#v, want %#v", test.name, received, want)
		}
	}
}

// A flow state never accepts another authenticated source's lane, while a
// lane refresh for the same source remains visible to its return callback.
func TestTransferStateKeepsSourceAndKeyPaired(t *testing.T) {
	source, initial, latest := natTransferKeyPair()
	state := newTransferState(source, initial)
	otherSource := SourceId(NewId())
	state.update(otherSource, latest)
	receivedSource, receivedKey := state.get()
	AssertEqual(t, source.LocalMask(), receivedSource)
	AssertEqual(t, initial, receivedKey)

	state.update(source, latest)
	receivedSource, receivedKey = state.get()
	AssertEqual(t, source.LocalMask(), receivedSource)
	AssertEqual(t, latest, receivedKey)
}
