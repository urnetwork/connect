// Private, uncompiled/unrun controls for handshake-only recovery authority.
// Native sends and real Transfer receipts preserve coverage before peer ACKs
// enter Multi's actual return path; no provider socket or timer is involved.
package connect

import (
	"encoding/binary"
	"testing"
	"testing/synctest"
)

// A first SYN-ACK establishes a baseline, not an advancing upload frontier.
func TestTcpRecoverySynAckKeepsNativePublicDataCollapsed(t *testing.T) {
	testTcpRecoveryHandshakePublicCoverage(t, false)
}

// One covered member must not reopen the complete public SYN/data group.
func TestTcpRecoverySynAckKeepsNativePublicGroupCollapsed(t *testing.T) {
	testTcpRecoveryHandshakePublicCoverage(t, true)
}

// The source worker is joined before every owner assertion. Peer controls are
// checksummed reverse-path packets delivered synchronously to the app callback.
func testTcpRecoveryHandshakePublicCoverage(t *testing.T, grouped bool) {
	t.Helper()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		budget := NewTransferMemoryBudget(64 * 1024)
		t.Cleanup(func() {
			if budget.UsedByteCount() != 0 {
				t.Errorf("joined handshake source retained %d bytes", budget.UsedByteCount())
			}
		})
		f := newGroupDispositionQueueFixture(t, budget, nil)
		if f.parent.provideMode.String() != "Network" {
			t.Fatal("native fixture must use Network provide mode")
		}
		syn := groupDispositionControlPacket(1000, 0, 64, tcpFlagSyn)
		if f.offer(syn) != 1 {
			t.Fatal("source SYN was not admitted")
		}
		f.requireParked()
		f.unpark()
		f.acknowledgeAll(syn)
		f.parent.ip4PathUpdates = map[Ip4Path]*multiClientChannelUpdate{f.update.ipPath.ToIp4Path(): f.update}
		delivered := 0
		f.parent.SetReceivePacketCallback(tcpRecoveryReceivePacketCallback(ReceivePacketFunction(nil), func([]byte) {
			delivered++
		}))
		deliver := func(ack uint32, syn bool) {
			t.Helper()
			sequence, flags := uint32(700), byte(tcpFlagAck)
			if syn {
				sequence, flags = 699, tcpFlagSyn|tcpFlagAck
			}
			reverse := f.update.ipPath.Reverse()
			packet := ipOosTcpPacketSequence(reverse, flags, sequence, nil)
			tcp := packet[Ipv4HeaderSizeWithoutExtensions:]
			binary.BigEndian.PutUint32(tcp[8:12], ack)
			binary.BigEndian.PutUint16(tcp[14:16], 64)
			binary.BigEndian.PutUint16(tcp[16:18], 0)
			binary.BigEndian.PutUint16(tcp[16:18], ipPathTransportChecksum(reverse, ipProtocolNumberTcp, tcp))
			path, err := ParseIpPath(packet)
			if err != nil {
				t.Fatal(err)
			}
			before := delivered
			f.parent.clientReceivePacket(f.selected, TransferPath{}, tcpRecoveryNetworkProvideMode,
				TransportTypeUnknown, path, packet)
			if delivered != before+1 {
				t.Fatal("peer control did not cross Multi's app delivery boundary")
			}
		}
		deliver(1001, true)
		ack := groupDispositionControlPacket(1001, 700, 64, tcpFlagAck)
		data := groupRecoveryPacket(1001, 700, 64, tcpFlagAck, []byte("ABCDE"))
		if f.offer(ack, data) != 2 {
			t.Fatal("source handshake ACK and original data were not admitted")
		}
		f.acknowledgeAll(ack, data)
		f.update.stateLock.Lock()
		covered, from, to := f.update.sequenceCovered, f.update.sequenceCoveredFrom, f.update.sequenceCoveredTo
		generation, inbound := f.update.synGenerationNumber, f.update.receivedInbound.Load()
		f.update.stateLock.Unlock()
		if !covered || from != 1000 || to != 1006 || generation != 1000 || !inbound {
			t.Fatalf("native retained cohort missing: coverage=%t [%d,%d), generation=%d inbound=%t", covered, from, to, generation, inbound)
		}
		requireCollapsed := func(templates ...[]byte) {
			t.Helper()
			started, drops := f.started.Load(), f.parent.TcpCollapseDropCount()
			if accepted := f.offer(templates...); accepted != 0 {
				t.Fatalf("handshake-only ACK reopened %d retained public packets without upload advancement", accepted)
			}
			synctest.Wait()
			if f.started.Load() != started || len(f.route) != 0 || f.parent.TcpCollapseDropCount() != drops+uint64(len(templates)) {
				t.Fatal("collapsed retry reached Transfer or lost its exact drop count")
			}
		}
		retry := [][]byte{data}
		if grouped {
			retry = [][]byte{syn, data}
		}
		requireCollapsed(retry...)
		// Neither duplicate handshake nor same/regressive ordinary ACK proves
		// that a previously refused future interval became the provider's gap.
		deliver(1001, true)
		requireCollapsed(retry...)
		deliver(1001, false)
		requireCollapsed(retry...)
		deliver(1000, false)
		requireCollapsed(retry...)
		// A real advancing ordinary ACK still grants exactly one frontier retry.
		deliver(1003, false)
		frontier := groupRecoveryPacket(1003, 700, 64, tcpFlagAck, []byte("C"))
		if f.offer(frontier) != 1 {
			t.Fatal("advancing upload ACK did not authorize its frontier retry")
		}
		f.acknowledgeAll(frontier)
		requireCollapsed(frontier)
		deliver(1001, true)
		deliver(1003, false)
		requireCollapsed(frontier)
		deliver(1006, false)
		requireCollapsed(retry...)
	})
}

// A pre-return new-SYN response is replayed after source commit. Its saved
// peer metadata must not turn the new generation's baseline into a lease.
func TestTcpRecoverySynchronousSynAckStartsWithoutRetryLease(t *testing.T) {
	parent, update, client := tcpRecoveryUnitFlow(t, 1000)
	syn := groupRecoveryParsed(t, groupRecoveryPacket(2000, 0, 4096, tcpFlagSyn, nil))
	data := tcpRecoveryUnitGroup(t, 2001, "ABCDE")
	group := &parsedPacketGroup{ipPath: syn.ipPath, packets: []parsedPacket{*syn, data.packets[0]}}
	if !group.prepareCollapseAdmission(update, parent) {
		t.Fatal("new source SYN group was not prepared")
	}
	defer group.admissionObservations.synAdmission.clear()
	defer group.finishCollapseSynOffer()
	update.markReceivedInbound(client, tcpControlObservation{valid: true, syn: true, ack: true, sequenceNumber: 899, ackSequenceNumber: 2001})
	update.commitSequenceGroupForClient(group, client)
	update.observeEgressTcpGroup(group)
	if !update.sequenceRecovery.peerSeen || !update.sequenceRecovery.seen ||
		update.sequenceRecovery.ack != 2001 || !update.sequenceRecovery.used || update.sequenceRecovery.reserved {
		t.Fatal("synchronous SYN-ACK did not establish a spent new-cohort baseline")
	}
	if update.canUpdateSequenceForClient(&data.packets[0], client) {
		t.Fatal("synchronous handshake response reopened retained new-cohort data")
	}
	tcpRecoveryUnitAck(update, client, 2001)
	if update.canUpdateSequenceForClient(&data.packets[0], client) {
		t.Fatal("ordinary duplicate of the handshake frontier rearmed a lease")
	}
	tcpRecoveryUnitAck(update, client, 2003)
	frontier := tcpRecoveryUnitGroup(t, 2003, "C")
	if !frontier.prepareCollapseAdmission(update, parent) {
		t.Fatal("post-handshake cumulative advancement lost its recovery authority")
	}
	update.commitSequenceGroupForClient(frontier, client)
	if update.canUpdateSequenceForClient(&frontier.packets[0], client) {
		t.Fatal("materialized advancing-frontier retry did not consume its lease")
	}
}
