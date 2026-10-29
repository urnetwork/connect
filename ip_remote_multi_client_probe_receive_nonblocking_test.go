// Real source capacity must not turn optional probe cleanup into receive HOL.
package connect

import (
	"bytes"
	"encoding/binary"
	"net"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/urnetwork/connect/protocol"
)

// First retire the fixture seed with its real ACK, then send an actual probe
// SYN through the same source. Its retained original closes the actual gate.
func testingProbeCourtesyReceive(t *testing.T, batch, ready bool) {
	t.Helper()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		credit := newNativeBatchCreditFixture(t)
		credit.releasePeerCredit()
		parent, _, _ := probeTestParent(t)
		client := credit.base.selected
		probe, ok := parent.registerProbeFlow(client, probeTestTarget())
		if !ok {
			t.Fatal("could not register real probe flow")
		}
		parent.stateLock.Lock()
		probeUpdate := parent.ip4PathUpdates[probe.ipPath.ToIp4Path()]
		parent.stateLock.Unlock()
		question, ok := probePacket(probe.ipPath, probe.target, probe.synSequence)
		if !ok {
			t.Fatal("could not construct actual probe SYN")
		}
		questionBytes := bytes.Clone(question)
		if !client.sendProbe(&parsedPacket{packet: question, ipPath: probe.ipPath}, 0) {
			MessagePoolReturn(question)
			t.Fatal("ready source refused actual initial probe SYN")
		}
		probe.sent.Store(true)
		synctest.Wait()
		if !credit.source.resendCapacityUnavailable.Load() || len(credit.base.route) != 1 {
			t.Fatal("actual retained probe SYN did not close its source gate")
		}
		wire := <-credit.base.route
		pack := decodeSendPackLifecycleWirePack(t, wire)
		if pack.Nack || len(pack.Frames) != 1 || !bytes.Equal(pack.Frames[0].MessageBytes, questionBytes) {
			MessagePoolReturn(wire)
			t.Fatal("source wire did not contain exactly the reliable probe SYN")
		}
		questionAck := protocol.Ack{MessageId: bytes.Clone(pack.MessageId), SequenceId: bytes.Clone(pack.SequenceId)}
		MessagePoolReturn(wire)
		ackQuestion := func() {
			if !credit.base.client.sendBuffer.Ack(credit.base.peer, &questionAck, 0) {
				t.Fatal("exact probe acknowledgement was refused")
			}
			synctest.Wait()
		}
		if ready {
			ackQuestion()
			if credit.source.resendCapacityUnavailable.Load() {
				t.Fatal("actual probe acknowledgement did not reopen source capacity")
			}
		}

		answerPath := probe.ipPath.Reverse()
		answerTemplate := ipOosTcpPacketSequence(answerPath, tcpFlagSyn|tcpFlagAck, 901, nil)
		tcp := answerTemplate[int(answerTemplate[0]&15)*4:]
		binary.BigEndian.PutUint32(tcp[8:12], probe.synSequence+1)
		binary.BigEndian.PutUint16(tcp[16:18], 0)
		binary.BigEndian.PutUint16(tcp[16:18], ipPathTransportChecksum(answerPath, ipProtocolNumberTcp, tcp))
		path, err := ParseIpPath(answerTemplate)
		if err != nil || !path.Syn || !path.Ack || path.AckSequenceNumber != probe.synSequence+1 {
			t.Fatal("could not build the exact valid probe SYN-ACK")
		}
		siblingPath := &IpPath{Version: 4, Protocol: IpProtocolTcp,
			SourceIp: net.IPv4(192, 0, 2, 73), DestinationIp: net.IPv4(198, 51, 100, 74),
			SourcePort: 43011, DestinationPort: 443}
		siblingTemplate := ipOosTcpPacketSequence(siblingPath.Reverse(), tcpFlagAck|tcpFlagPsh, 701, []byte("healthy sibling"))
		siblingIngress, err := ParseIpPath(siblingTemplate)
		if err != nil {
			t.Fatal(err)
		}
		sibling := newMultiClientChannelUpdate(parent.ctx, siblingPath)
		sibling.client.Store(client)
		sibling.receivedInbound.Store(true)
		parent.stateLock.Lock()
		parent.ip4PathUpdates[siblingPath.ToIp4Path()] = sibling
		parent.stateLock.Unlock()
		inputs := [][]byte{MessagePoolCopy(answerTemplate), MessagePoolCopy(siblingTemplate)}
		witnesses := groupTestPacketWitnesses(t, inputs)
		t.Cleanup(func() {
			credit.base.cancel()
			credit.releaseCapacity()
			credit.workers.Wait()
			parent.unregisterProbeFlows([]*probeFlow{probe})
			sibling.Close()
			if !probeUpdate.IsDone() {
				t.Error("answered probe update did not close after receive returned")
			}
			for index, packet := range inputs {
				if MessagePoolReturn(packet) {
					t.Error("receive consumed a borrowed input owner")
				}
				if !MessagePoolReturn(witnesses[index]) {
					t.Error("receive retained a borrowed packet after joined cleanup")
				}
			}
		})
		var delivered atomic.Int32
		var invalid atomic.Bool
		observe := func(packet []byte) {
			if !bytes.Equal(packet, siblingTemplate) {
				invalid.Store(true)
			}
			delivered.Add(1)
		}
		parent.SetReceivePacketCallback(func(_ TransferPath, _ protocol.ProvideMode, _ *IpPath, packet []byte) { observe(packet) })
		if batch {
			parent.SetReceivePacketsCallback(func(_ TransferPath, _ protocol.ProvideMode, _ *IpPath, packets [][]byte) {
				for _, packet := range packets {
					observe(packet)
				}
			})
		}
		started := credit.base.started.Load()
		returned := make(chan struct{})
		credit.workers.Add(1)
		go func() {
			defer credit.workers.Done()
			defer close(returned)
			if batch {
				parent.clientReceivePackets(client, SourceId(credit.base.peer), protocol.ProvideMode_Public, TransportTypeH1,
					[]*IpPath{path, siblingIngress}, inputs)
			} else {
				parent.clientReceivePacket(client, SourceId(credit.base.peer), protocol.ProvideMode_Public, TransportTypeH1, path, inputs[0])
				parent.clientReceivePacket(client, SourceId(credit.base.peer), protocol.ProvideMode_Public, TransportTypeH1, siblingIngress, inputs[1])
			}
		}()
		synctest.Wait()
		select {
		case <-probe.done:
			if !probe.answered.Load() {
				t.Fatal("valid SYN-ACK did not complete its actual registered probe")
			}
		default:
			t.Fatal("receive never reached the answered-probe completion boundary")
		}
		select {
		case <-returned:
		default:
			if !ready && credit.source.ctx.Err() == nil && credit.source.resendCapacityUnavailable.Load() && credit.source.resendCapacityUnavailable.waiterCount.Load() == 1 && credit.base.started.Load() == started+1 {
				t.Fatalf("PROBE_COURTESY_RST_BLOCKED_RECEIVE: batch=%t exact closed source trapped answered-probe cleanup before unrelated delivery", batch)
			}
			t.Fatal("receive did not return from its actual probe/sibling entry")
		}
		if delivered.Load() != 1 || invalid.Load() || !probeUpdate.IsDone() || sibling.IsDone() || client.IsDone() || client.dialFailureCount() != 0 {
			t.Fatal("optional cleanup changed probe consumption, sibling delivery or healthy ownership")
		}
		if credit.base.started.Load() != started+1 || credit.source.resendCapacityUnavailable.waiterCount.Load() != 0 {
			t.Fatal("courtesy reset duplicated admission or retained a FIFO waiter")
		}
		if ready {
			if len(credit.base.route) != 1 {
				t.Fatal("ready courtesy reset did not reach its actual wire")
			}
			func() {
				wire := <-credit.base.route
				defer MessagePoolReturn(wire)
				pack := decodeSendPackLifecycleWirePack(t, wire)
				if pack.Nack || len(pack.Frames) != 1 {
					t.Fatal("ready courtesy reset lost reliable single-packet ownership")
				}
				packet := pack.Frames[0].MessageBytes
				reset, err := ParseIpPath(packet)
				if err != nil || !reset.Rst || reset.Syn || reset.Fin || reset.SequenceNumber != probe.synSequence+1 ||
					reset.SourcePort != probe.ipPath.SourcePort || reset.DestinationPort != probe.ipPath.DestinationPort ||
					!net.IP(reset.SourceIp).Equal(net.IP(probe.ipPath.SourceIp)) || !net.IP(reset.DestinationIp).Equal(net.IP(probe.ipPath.DestinationIp)) || reset.TcpPayloadByteCount != 0 {
					t.Fatal("actual courtesy reset changed flags, exact tuple or ISN+1")
				}
				if ipPathTransportChecksum(reset, ipProtocolNumberTcp, packet[int(packet[0]&15)*4:]) != 0 {
					t.Fatal("actual courtesy reset checksum is invalid")
				}
				if !credit.base.client.sendBuffer.Ack(credit.base.peer, &protocol.Ack{MessageId: pack.MessageId, SequenceId: pack.SequenceId}, 0) {
					t.Fatal("exact admitted reset acknowledgement was refused")
				}
			}()
			synctest.Wait()
		} else {
			if len(credit.base.route) != 0 {
				t.Fatal("refused optional reset acquired wire ownership")
			}
			ackQuestion()
		}
		// A duplicate answer cannot re-complete, reopen a flow or send another
		// courtesy reset after the first optional attempt was refused/accepted.
		parent.clientReceivePacket(client, SourceId(credit.base.peer), protocol.ProvideMode_Public, TransportTypeH1, path, inputs[0])
		synctest.Wait()
		if credit.base.started.Load() != started+1 || len(credit.base.route) != 0 || delivered.Load() != 1 || len(client.connectSuccessTimes) != 1 {
			t.Fatal("duplicate probe answer replayed cleanup, completion or application delivery")
		}
	})
}

// Scalar receive must return while the real reliable source remains full.
func TestProbeCourtesyResetDoesNotBlockScalarReceive(t *testing.T) {
	testingProbeCourtesyReceive(t, false, false)
}

// A probe first in a batch cannot hold the valid application member behind it.
func TestProbeCourtesyResetDoesNotBlockBatchReceive(t *testing.T) {
	testingProbeCourtesyReceive(t, true, false)
}

// Ready optional cleanup remains reliable and sends exactly ISN+1 once.
func TestProbeCourtesyResetReadySourcePreservesWireAndOwnership(t *testing.T) {
	for _, batch := range []bool{false, true} {
		testingProbeCourtesyReceive(t, batch, true)
	}
}
