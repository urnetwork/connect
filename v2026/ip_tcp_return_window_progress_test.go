// Window progress is not loss evidence for an outstanding native TCP return.
package connect

import (
	"testing"
	"testing/synctest"
	"time"
)

// Real serialized source ACKs enter the established receive transition while
// the ordinary replay worker owns the already-emitted, retained return bytes.
func TestTcpReturnIncreasingWindowDoesNotCountAsDuplicateAcks(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		const base = uint32(101)
		payload := []byte("synthetic retained return body")
		fixture := newTcpReturnFlightFixture(t, 1, base, payload, 128, nil)
		defer fixture.close()
		sequence := fixture.sequence
		sequence.mutex.Lock()
		retained := sequence.returnByteCount
		sequence.mutex.Unlock()
		if retained <= 0 || len(fixture.snapshot()) != 0 {
			t.Fatal("the ordinary return worker did not retain its unanswered original")
		}
		started := time.Now()
		duplicates, attempts := 0, 0
		for index, sourceSequence := range []uint32{133, 101, 117} {
			window := uint16(129 + index)
			// ConnectionState builds return-direction packets, so this source
			// packet uses the exact live flow's reversed immutable addresses.
			state := ConnectionState{
				ipVersion:       sequence.ipVersion,
				sourceIp:        sequence.destinationIp,
				sourcePort:      sequence.destinationPort,
				destinationIp:   sequence.sourceIp,
				destinationPort: sequence.sourcePort,
				sendSeq:         base,
				windowSize:      uint32(window),
			}
			packet := state.tcpPacket(tcpFlagAck, sourceSequence, nil)
			_, sourceIp, destinationIp, transport, valid := parseIpv4(packet)
			var tcp parsedTcp
			if !valid || !parseTcpPacket(sourceIp, destinationIp, transport, &tcp) {
				MessagePoolReturn(packet)
				t.Fatal("the source window update did not serialize and parse")
			}
			if !sequence.applyEstablishedPureAck(sequence.source, TransferKey{}, &tcp, packet) {
				MessagePoolReturn(packet)
				t.Fatal("the actual established ACK entry refused a valid window update")
			}
			synctest.Wait()
			sequence.mutex.Lock()
			ack, edge, currentWindow := sequence.receiveSeqAck, sequence.receiveWindowEnd, sequence.receiveWindowSize
			currentRetained := sequence.returnByteCount
			duplicates, attempts = sequence.returnDuplicateAcks, sequence.returnAttempts
			sequence.mutex.Unlock()
			if ack != base || edge != base+uint32(window) || currentWindow != uint32(window) || currentRetained != retained {
				t.Fatal("the genuine source update did not grow its advertised edge while preserving original custody")
			}
			if !time.Now().Equal(started) {
				t.Fatal("the explicit ACK sequence unexpectedly reached a return timeout")
			}
			if index < 2 && len(fixture.snapshot()) != 0 {
				t.Fatal("native return replay preceded the third explicit source ACK")
			}
		}
		if duplicates != 0 || attempts != 0 || len(fixture.snapshot()) != 0 {
			t.Fatalf("three native window advances became duplicate-ACK loss evidence: duplicates=%d attempts=%d replays=%d",
				duplicates, attempts, len(fixture.snapshot()))
		}
	})
}

// The same retained return and positive advertised-window baseline still
// recover on three genuine unchanged-window duplicate acknowledgements.
func TestTcpReturnUnchangedWindowDuplicateAcksStillRecover(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		const base = uint32(101)
		payload := []byte("synthetic retained return body")
		fixture := newTcpReturnFlightFixture(t, 1, base, payload, 128, nil)
		defer fixture.close()
		started := time.Now()
		fixture.trigger(base, 128)
		events := requireTcpReturnFlightCount(t, fixture, 1)
		if !time.Now().Equal(started) || events[0].seq != base || events[0].payload != string(payload[:4]) || events[0].fin {
			t.Fatal("genuine duplicate acknowledgements lost their immediate exact oldest-range repair")
		}
	})
}
