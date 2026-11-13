// PRIVATE / UNCOMPILED / UNRUN. Focused state-machine adjacencies complement
// the real native Multi/Transfer/TcpSequence public-retry integration tests.
package connect

import (
	"encoding/binary"
	"sync"
	"testing"
)

func tcpRecoveryUnitGroup(t *testing.T, sequence uint32, payload string) *parsedPacketGroup {
	t.Helper()
	packet := groupRecoveryParsed(t, groupRecoveryPacket(sequence, 900, 4096, tcpFlagAck, []byte(payload)))
	return &parsedPacketGroup{ipPath: packet.ipPath, packets: []parsedPacket{*packet}}
}

func tcpRecoveryUnitScalePacket(t *testing.T, template []byte, scale int) *parsedPacket {
	t.Helper()
	if scale < 0 {
		return groupRecoveryParsed(t, template)
	}
	path, err := ParseIpPath(template)
	if err != nil {
		t.Fatal(err)
	}
	packet, tcp := ipTransportPacket(path, ipProtocolNumberTcp, TcpHeaderSizeWithoutExtensions+4)
	copy(tcp, template[Ipv4HeaderSizeWithoutExtensions:])
	copy(tcp[TcpHeaderSizeWithoutExtensions:], []byte{3, 3, byte(scale), 0})
	tcp[12] = byte((TcpHeaderSizeWithoutExtensions+4)/4) << 4
	binary.BigEndian.PutUint16(tcp[16:18], 0)
	binary.BigEndian.PutUint16(tcp[16:18], ipPathTransportChecksum(path, ipProtocolNumberTcp, tcp))
	return groupRecoveryParsed(t, packet)
}

// No network or asynchronous owner is represented by this fixture: commits
// deliberately exercise the scalar legacy whole-group admission contract.
func tcpRecoveryUnitFlow(t *testing.T, initial uint32) (*RemoteUserNatMultiClient, *multiClientChannelUpdate, *multiClientChannel) {
	t.Helper()
	parent, update := collapseTestClient(0)
	t.Cleanup(update.Close)
	client := &multiClientChannel{ctx: update.ctx, settings: parent.settings}
	update.client.Store(client)
	syn := groupRecoveryParsed(t, groupRecoveryPacket(initial, 0, 4096, tcpFlagSyn, nil))
	group := &parsedPacketGroup{ipPath: syn.ipPath, packets: []parsedPacket{*syn}}
	group.prepareCollapseAdmission(update)
	update.commitSequenceGroupForClient(group, client)
	update.markReceivedInbound(client, tcpControlObservation{valid: true, syn: true, ack: true, sequenceNumber: 899, ackSequenceNumber: initial + 1})
	data := tcpRecoveryUnitGroup(t, initial+1, "ABCDEFGH")
	data.prepareCollapseAdmission(update)
	update.commitSequenceGroupForClient(data, client)
	update.observeEgressTcpGroup(data)
	return parent, update, client
}

func tcpRecoveryUnitAck(update *multiClientChannelUpdate, client *multiClientChannel, ack uint32) {
	update.markReceivedInbound(client, tcpControlObservation{valid: true, ack: true, sequenceNumber: 900, ackSequenceNumber: ack})
}

func TestTcpRecoveryConcurrentOverlapsReserveOneLease(t *testing.T) {
	parent, update, client := tcpRecoveryUnitFlow(t, 1000)
	tcpRecoveryUnitAck(update, client, 1005)
	groups := []*parsedPacketGroup{tcpRecoveryUnitGroup(t, 1005, "E"), tcpRecoveryUnitGroup(t, 1004, "DE")}
	start, joined := make(chan struct{}), make(chan int, 2)
	var workers sync.WaitGroup
	for index, group := range groups {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			if group.prepareCollapseAdmission(update, parent) {
				joined <- index
			} else {
				joined <- -1
			}
		}()
	}
	close(start)
	first, second := <-joined, <-joined
	workers.Wait()
	if (first >= 0) == (second >= 0) {
		t.Fatalf("concurrent overlapping authority winners=%d/%d, want exactly one", first, second)
	}
	winner := groups[max(first, second)]
	winner.collapseAdmission.releaseTcpRecovery() // Actual refusal never spends authority.
	retry := tcpRecoveryUnitGroup(t, 1005, "E")
	if !retry.prepareCollapseAdmission(update, parent) || retry.collapseAdmission.recovery == winner.collapseAdmission.recovery {
		t.Fatal("refused lease did not reopen with a fresh scalar token")
	}
	winner.collapseAdmission.releaseTcpRecovery() // A late old release cannot revoke the new reservation.
	if update.canUpdateSequenceForClient(&groups[1].packets[0], client) {
		t.Fatal("stale rollback released another offer's active lease")
	}
	update.commitSequenceGroupForClient(retry, client)
	if !update.sequenceRecovery.used || update.sequenceRecovery.reserved {
		t.Fatal("successful matching materialization did not consume its lease")
	}
	tcpRecoveryUnitAck(update, client, 1005)
	tcpRecoveryUnitAck(update, client, 1004)
	if update.canUpdateSequenceForClient(&retry.packets[0], client) {
		t.Fatal("duplicate/regressive ACK rearmed consumed authority")
	}
}

func TestTcpRecoveryOlderMaterializationCannotSpendNewAck(t *testing.T) {
	parent, update, client := tcpRecoveryUnitFlow(t, 1000)
	older := tcpRecoveryUnitGroup(t, 1005, "EF")
	older.prepareCollapseAdmission(update) // An original predating the real ACK has no recovery token.
	tcpRecoveryUnitAck(update, client, 1005)
	retry := tcpRecoveryUnitGroup(t, 1005, "EF")
	if !retry.prepareCollapseAdmission(update, parent) {
		t.Fatal("first frontier retry was not offered")
	}
	update.commitSequenceGroupForClient(older, client)
	if !update.sequenceRecovery.reserved || update.sequenceRecovery.used {
		t.Fatal("older original stole the newly observed ACK's authority")
	}
	tcpRecoveryUnitAck(update, client, 1006)
	update.commitSequenceGroupForClient(retry, client)
	if update.sequenceRecovery.used || update.sequenceRecovery.reserved {
		t.Fatal("old token spent authority from a newer cumulative ACK")
	}
	next := tcpRecoveryUnitGroup(t, 1006, "F")
	if !next.prepareCollapseAdmission(update, parent) {
		t.Fatal("ACK advance did not permit the next refused byte")
	}
	update.commitSequenceGroupForClient(next, client)
	if !update.sequenceRecovery.used {
		t.Fatal("matching retained retry did not consume current authority")
	}
	// Explicit unwritten provenance differs from an ordinary successful ACK.
	next.collapseAdmission.complete(errSendPackExpiredUnwritten)
	if update.sequenceRecovery.used || update.sequenceRecovery.reserved ||
		!update.canUpdateSequenceForClient(&next.packets[0], client) {
		t.Fatal("unwritten completion left false ownership or spent recovery authority")
	}
}

func TestTcpRecoveryRawDisposalReturnsUnspentLease(t *testing.T) {
	parent, update, client := tcpRecoveryUnitFlow(t, 1000)
	tcpRecoveryUnitAck(update, client, 1005)
	retry := tcpRecoveryUnitGroup(t, 1005, "E")
	if !retry.prepareCollapseAdmission(update, parent) {
		t.Fatal("recovery offer did not reserve")
	}
	// This adjacency isolates the native raw-disposal callback, not physical
	// delivery. Native root tests separately prove actual retained custody.
	retry.completionClient = client
	retry.completionFlags.Store(groupCompletionTracked)
	retry.sendGroupDisposition(0, 1, false)
	if update.sequenceRecovery.used || update.sequenceRecovery.reserved ||
		!update.canUpdateSequenceForClient(&retry.packets[0], client) {
		t.Fatal("unfunded raw disposal consumed the only retry authority")
	}
	partial := tcpRecoveryUnitGroup(t, 1005, "E")
	overlap := tcpRecoveryUnitGroup(t, 1004, "DE")
	partial.packets = append(partial.packets, overlap.packets[0])
	if !partial.prepareCollapseAdmission(update, parent) {
		t.Fatal("returned raw lease was unavailable")
	}
	partial.completionClient = client
	partial.completionFlags.Store(groupCompletionTracked | groupCompletionDequeued)
	partial.collapseOwner = &tcpCollapseGroup{remaining: 2}
	partial.sendGroupDisposition(0, 1, false)
	if !update.sequenceRecovery.reserved {
		t.Fatal("one disposed overlap released a group with another pending overlap")
	}
	partial.sendGroupDisposition(1, 2, false)
	if update.sequenceRecovery.reserved || update.sequenceRecovery.used {
		t.Fatal("fully disposed frontier group kept its reservation")
	}
}

func TestTcpRecoveryAuthorityRequiresCurrentSynAndProvider(t *testing.T) {
	parent, update, client := tcpRecoveryUnitFlow(t, 1000)
	tcpRecoveryUnitAck(update, client, 1005)
	old := tcpRecoveryUnitGroup(t, 1005, "E")
	if !old.prepareCollapseAdmission(update, parent) {
		t.Fatal("old cohort did not reserve")
	}
	syn := groupRecoveryParsed(t, groupRecoveryPacket(2000, 0, 4096, tcpFlagSyn, nil))
	group := &parsedPacketGroup{ipPath: syn.ipPath, packets: []parsedPacket{*syn}}
	group.prepareCollapseAdmission(update)
	update.commitSequenceGroupForClient(group, client)
	tcpRecoveryUnitAck(update, client, 1005)
	if update.sequenceRecovery.seen || !update.synGenerationAwaiting {
		t.Fatal("old ordinary ACK established a newly accepted SYN generation")
	}
	update.markReceivedInbound(client, tcpControlObservation{valid: true, syn: true, ack: true, sequenceNumber: 899, ackSequenceNumber: 2001})
	tcpRecoveryUnitAck(update, client, 5000)
	if update.sequenceRecovery.ack != 2001 {
		t.Fatal("late old-cohort ACK beyond current materialized high-water poisoned the new frontier")
	}
	data := tcpRecoveryUnitGroup(t, 2001, "ABCD")
	data.prepareCollapseAdmission(update)
	update.commitSequenceGroupForClient(data, client)
	tcpRecoveryUnitAck(update, client, 2003)
	old.collapseAdmission.releaseTcpRecovery()
	retry := tcpRecoveryUnitGroup(t, 2003, "C")
	if !update.canUpdateSequenceForClient(&retry.packets[0], client) {
		t.Fatal("old generation release disturbed the new generation's frontier")
	}
	replacement := &multiClientChannel{ctx: update.ctx, settings: parent.settings}
	update.client.Store(replacement)
	tcpRecoveryUnitAck(update, client, 2004)
	if update.sequenceRecovery.client != client || update.sequenceRecovery.ack != 2003 {
		t.Fatal("old provider ACK changed replacement recovery proof")
	}
	data.prepareCollapseAdmission(update)
	update.commitSequenceGroupForClient(data, replacement)
	if update.sequenceRecovery.seen || update.canUpdateSequenceForClient(&retry.packets[0], replacement) {
		t.Fatal("provider rebind inherited old provider's frontier")
	}
	update.markReceivedInbound(replacement, tcpControlObservation{valid: true, syn: true, ack: true, sequenceNumber: 899, ackSequenceNumber: 2001})
	tcpRecoveryUnitAck(update, replacement, 2003)
	if !update.canUpdateSequenceForClient(&retry.packets[0], replacement) {
		t.Fatal("replacement's own real frontier was not usable")
	}
}

func TestTcpRecoverySerialExhaustionNeverReusesToken(t *testing.T) {
	parent, update, client := tcpRecoveryUnitFlow(t, 1000)
	tcpRecoveryUnitAck(update, client, 1005)
	update.sequenceRecovery.serial = ^uint64(0) - 1
	retry := tcpRecoveryUnitGroup(t, 1005, "E")
	if !retry.prepareCollapseAdmission(update, parent) || retry.collapseAdmission.recovery != 0 ||
		update.sequenceRecovery.serial != ^uint64(0) || update.sequenceRecovery.reserved {
		t.Fatal("serial exhaustion did not enter explicit no-token fail-open mode")
	}
	tcpRecoveryUnitAck(update, client, 1006)
	next := tcpRecoveryUnitGroup(t, 1006, "F")
	if !next.prepareCollapseAdmission(update, parent) || next.collapseAdmission.recovery != 0 ||
		update.sequenceRecovery.reserved || update.sequenceRecovery.used {
		t.Fatal("new frontier issued an ABA token after serial exhaustion")
	}
	update.commitSequenceGroupForClient(retry, client)
	if update.sequenceRecovery.used || !update.canUpdateSequenceForClient(&next.packets[0], client) {
		t.Fatal("late materialization defeated exhausted fail-open mode")
	}
}

func TestTcpRecoveryWrapFinAndControlBoundaries(t *testing.T) {
	parent, update, client := tcpRecoveryUnitFlow(t, ^uint32(0)-2)
	tcpRecoveryUnitAck(update, client, 0)
	overlap := tcpRecoveryUnitGroup(t, ^uint32(0), "BC")
	if !overlap.prepareCollapseAdmission(update, parent) {
		t.Fatal("wrapped overlap did not span real frontier zero")
	}
	update.commitSequenceGroupForClient(overlap, client)
	tcpRecoveryUnitAck(update, client, ^uint32(0))
	tcpRecoveryUnitAck(update, client, 0)
	if !update.sequenceRecovery.used || update.sequenceRecovery.ack != 0 {
		t.Fatal("wrapped regressive/duplicate ACK rearmed recovery")
	}
	tcpRecoveryUnitAck(update, client, 1)
	fin := tcpRecoveryUnitGroup(t, 1, "")
	fin.packets[0].ipPath.Fin = true
	if !update.canUpdateSequenceForClient(&fin.packets[0], client) {
		t.Fatal("FIN's sequence byte did not span the advanced frontier")
	}
	pure := tcpRecoveryUnitGroup(t, 1, "")
	update.commitSequenceGroupForClient(pure, client)
	if update.canUpdateSequenceForClient(&pure.packets[0], client) {
		t.Fatal("unchanged pure ACK gained data recovery authority")
	}
	pure.packets[0].ipPath.TcpWindowSize++
	if !update.canUpdateSequenceForClient(&pure.packets[0], client) {
		t.Fatal("outgoing window advancement lost its independent admission")
	}
	pure.packets[0].ipPath.TcpWindowSize--
	pure.packets[0].ipPath.AckSequenceNumber++
	if !update.canUpdateSequenceForClient(&pure.packets[0], client) {
		t.Fatal("outgoing ACK advancement lost its independent admission")
	}
	fin.packets[0].ipPath.Syn = true
	if fin.packets[0].spansTcpRecoveryAck(1, 0) {
		t.Fatal("SYN used a data/FIN recovery token")
	}
}

func TestTcpRecoveryRejectsPriorPeerSequenceInsideUploadCoverage(t *testing.T) {
	parent, update, client := tcpRecoveryUnitFlow(t, 1000)
	// This delayed prior-cohort ACK has a plausible upload number, but its
	// old peer sequence is outside the new SYN-ACK's advertised receive window.
	update.markReceivedInbound(client, tcpControlObservation{valid: true, ack: true, sequenceNumber: 500, ackSequenceNumber: 1008})
	if update.sequenceRecovery.ack != 1001 {
		t.Fatal("old peer sequence poisoned a numerically plausible upload frontier")
	}
	tcpRecoveryUnitAck(update, client, 1005)
	retry := tcpRecoveryUnitGroup(t, 1005, "E")
	if !retry.prepareCollapseAdmission(update, parent) {
		t.Fatal("valid current peer ACK could not recover after stale ingress")
	}
	update.commitSequenceGroupForClient(retry, client)
	update.markReceivedInbound(client, tcpControlObservation{valid: true, ack: true, sequenceNumber: 500, ackSequenceNumber: 1008})
	if !update.sequenceRecovery.used || update.sequenceRecovery.ack != 1005 {
		t.Fatal("stale peer ACK rearmed a consumed current-generation lease")
	}
}

func TestTcpRecoveryNegotiatedAndZeroReceiveWindows(t *testing.T) {
	_, update, client := tcpRecoveryUnitFlow(t, 1000)
	// Isolate scalar negotiation/window arithmetic after a valid SYN cohort.
	update.sequenceRecovery.windowScale, update.sequenceRecovery.windowScaleEnabled = 6, true
	update.observeTcpControl(tcpControlObservation{valid: true, ack: true, ackSequenceNumber: 1000, windowSize: 2}, false)
	update.observeTcpControl(tcpControlObservation{valid: true, ack: true, ackSequenceNumber: 900, windowSize: 2}, false)
	accepted := tcpControlObservation{valid: true, ack: true, sequenceNumber: 1064, ackSequenceNumber: 1005}
	update.markReceivedInbound(client, accepted)
	if update.sequenceRecovery.ack != 1005 || update.sequenceRecovery.peerWindow != 128 {
		t.Fatal("scaled receive window rejected a valid ACK")
	}
	// No ingress separated the newer and reordered older source controls.
	accepted.ackSequenceNumber = 1006
	update.markReceivedInbound(client, accepted)
	if update.sequenceRecovery.ack != 1006 || update.sequenceRecovery.peerSequence != 1000 || update.sequenceRecovery.peerWindow != 128 {
		t.Fatal("regressive source ACK corrupted the moving scaled window")
	}
	update.observeTcpControl(tcpControlObservation{valid: true, ack: true, ackSequenceNumber: 1000, windowSize: 0}, false)
	accepted.sequenceNumber, accepted.ackSequenceNumber = 1001, 1007
	update.markReceivedInbound(client, accepted)
	if update.sequenceRecovery.ack != 1006 {
		t.Fatal("zero-window ACK beyond exact receive edge became authority")
	}
	accepted.sequenceNumber = 1000
	update.markReceivedInbound(client, accepted)
	if update.sequenceRecovery.ack != 1007 {
		t.Fatal("zero-length ACK at zero-window receive edge was rejected")
	}
	accepted.fin, accepted.ackSequenceNumber = true, 1008
	update.markReceivedInbound(client, accepted)
	if update.sequenceRecovery.ack != 1007 {
		t.Fatal("FIN consumed sequence space inside a closed receive window")
	}
}

func TestTcpRecoverySynNegotiationSurvivesSynchronousReply(t *testing.T) {
	parent, update, client := tcpRecoveryUnitFlow(t, 1000)
	for index, scales := range [][2]int{{-1, -1}, {-1, 6}, {6, -1}, {6, 6}, {255, 6}} {
		initial := uint32(2000 + index*1000)
		sourceScale, providerScale := scales[0], scales[1]
		negotiated := sourceScale >= 0 && providerScale >= 0
		syn := tcpRecoveryUnitScalePacket(t, groupRecoveryPacket(initial, 0, 2, tcpFlagSyn, nil), sourceScale)
		if syn.ipPath.TcpWindowScaleEnabled != (sourceScale >= 0) ||
			sourceScale >= 0 && syn.ipPath.TcpWindowScale != uint8(min(sourceScale, 14)) {
			t.Fatal("source SYN option metadata was not parsed or clamped")
		}
		group := &parsedPacketGroup{ipPath: syn.ipPath, packets: []parsedPacket{*syn}}
		if !group.prepareCollapseAdmission(update, parent) {
			t.Fatal("new SYN admission proof was not prepared")
		}
		// A response arrives while the successful legacy admission is still
		// returning. Its scalar proof must survive the subsequent cohort reset.
		reply := tcpRecoveryUnitScalePacket(t, groupRecoveryPacket(899, initial+1, 4096, tcpFlagSyn|tcpFlagAck, nil), providerScale)
		update.markReceivedInbound(client, tcpControlFromIpPath(reply.ipPath))
		update.commitSequenceGroupForClient(group, client)
		group.admissionObservations.synAdmission.clear()
		group.finishCollapseSynOffer()
		if !update.sequenceRecovery.peerSeen || update.sequenceRecovery.peerSequence != 900 ||
			update.sequenceRecovery.windowScaleEnabled != negotiated || update.sequenceRecovery.peerWindow != 2 {
			t.Fatal("SYN-ACK cohort/negotiation proof was lost or SYN window was scaled")
		}
		ack := tcpRecoveryUnitGroup(t, initial+1, "")
		ack.packets[0].ipPath.TcpWindowSize = 2
		ack.prepareCollapseAdmission(update)
		update.commitSequenceGroupForClient(ack, client)
		update.observeEgressTcpGroup(ack)
		want := uint32(2)
		if negotiated {
			want <<= uint8(min(sourceScale, 14))
		}
		if update.sequenceRecovery.peerWindow != want {
			t.Fatalf("negotiated post-handshake window=%d, want %d", update.sequenceRecovery.peerWindow, want)
		}
	}
}
