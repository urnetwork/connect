// Whole pure-acknowledgement groups preserve native feedback without granting
// mixed data or connection controls an additional collapse exception.
package connect

import (
	"bytes"
	"encoding/binary"
	"io"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// The real provider's dispatch hook counts individual packets, independently
// of Transfer's physical group/chunk completion count. Quiescence precedes it.
func watchNativePureAckProvider(t *testing.T, fixture *nativeDuplicateAckFixture) *atomic.Int64 {
	t.Helper()
	sequence := fixture.providerSequence.Load()
	if sequence == nil {
		t.Fatal("pure ACK fixture has no provider TCP owner")
	}
	count := &atomic.Int64{}
	sequence.tcpBufferSettings.beforeSequenceSendForTest = func(actual *TcpSequence) {
		if actual != sequence {
			t.Error("pure ACK packet changed provider TCP owner")
		}
		count.Add(1)
	}
	return count
}

// The consuming native entry owns every copied member, including a refusal.
func offerNativePureAckPackets(t *testing.T, fixture *nativeDuplicateAckFixture, templates ...[]byte) (int, []bool) {
	t.Helper()
	packets := make([][]byte, len(templates))
	accepted := make([]bool, len(templates))
	for index, template := range templates {
		packets[index] = MessagePoolCopy(template)
	}
	count := fixture.parent.SendNativePacketBatchWithResults(SourceId(fixture.sender.ClientId()),
		protocol.ProvideMode_Network, packets, 5*time.Second, accepted)
	actual := 0
	for _, admitted := range accepted {
		if admitted {
			actual++
		}
	}
	if actual != count {
		t.Fatalf("native pure ACK results lost membership: count=%d accepted=%v", count, accepted)
	}
	return count, accepted
}

// Malformed metadata and every sequence-space edge keep their ordinary gate.
func TestPureTcpAckGroupRequiresEveryMember(t *testing.T) {
	ack := groupRecoveryParsed(t, groupRecoveryPacket(1001, 2001, 4096, tcpFlagAck, nil))
	if !(&parsedPacketGroup{packets: []parsedPacket{*ack}, ipPath: ack.ipPath}).onlyPureTcpAcks() {
		t.Fatal("real parsed pure ACK did not qualify")
	}
	if (&parsedPacketGroup{}).onlyPureTcpAcks() ||
		(&parsedPacketGroup{packets: []parsedPacket{{}}}).onlyPureTcpAcks() {
		t.Fatal("empty or missing-path group qualified")
	}
	for _, candidate := range []struct {
		name    string
		flags   byte
		payload []byte
	}{
		{name: "ack flag absent"},
		{name: "syn", flags: tcpFlagAck | tcpFlagSyn},
		{name: "fin", flags: tcpFlagAck | tcpFlagFin},
		{name: "rst", flags: tcpFlagAck | tcpFlagRst},
		{name: "payload", flags: tcpFlagAck, payload: []byte("data")},
	} {
		packet := groupRecoveryParsed(t, groupRecoveryPacket(1001, 2001, 4096, candidate.flags, candidate.payload))
		group := &parsedPacketGroup{packets: []parsedPacket{*ack, *packet}, ipPath: ack.ipPath}
		if group.onlyPureTcpAcks() || IsTcpAckOnlyPacket(packet.packet) {
			t.Fatalf("%s mixed group qualified as entirely pure ACK", candidate.name)
		}
	}
	pathPayload := *ack.ipPath
	pathPayload.TcpPayloadByteCount = 1
	for _, packet := range []parsedPacket{
		{ipPath: &pathPayload},
		{ipPath: ack.ipPath, payload: []byte{1}},
		{ipPath: &IpPath{Protocol: IpProtocolUdp, Ack: true}},
	} {
		if (&parsedPacketGroup{packets: []parsedPacket{*ack, packet}}).onlyPureTcpAcks() {
			t.Fatal("inconsistent parsed member bypassed the whole-group predicate")
		}
	}
}

// A pure ACK still needs the selected client's ordinary queue admission.
// The native refusal returns its original owner and the next event can retry.
func TestNativePureAckRefusalPreservesOrdinaryAdmission(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture := newNativeDuplicateAckFixture(t, false)
		defer fixture.close()
		providerPackets := watchNativePureAckProvider(t, fixture)
		fixture.consumer.stateLock.Lock()
		base := fixture.consumer.base
		fixture.consumer.stateLock.Unlock()
		ack := groupRecoveryPacket(fixture.initial+1, base, fixture.window, tcpFlagAck, nil)
		calls := 0
		fixture.selected.sendGroupForTest = func(_ *parsedPacketGroup, _ time.Duration, ackRequired bool) (bool, error) {
			calls++
			if !ackRequired {
				t.Error("pure ACK lost ordinary Transfer ACK recovery")
			}
			return false, nil
		}
		before, terminals, drops := time.Now(), fixture.senderTerminals.Load(), fixture.parent.TcpCollapseDropCount()
		if fixture.offer(ack) || calls != 1 {
			t.Fatal("selected-client refusal fabricated native pure ACK admission")
		}
		synctest.Wait()
		if providerPackets.Load() != 0 || fixture.senderTerminals.Load() != terminals ||
			fixture.parent.TcpCollapseDropCount() != drops || fixture.update.client.Load() != fixture.selected {
			t.Fatal("ordinary pure ACK refusal changed provider ownership or completion")
		}
		fixture.selected.sendGroupForTest = nil
		if !fixture.offer(ack) {
			t.Fatal("identical pure ACK could not retry after ordinary refusal")
		}
		synctest.Wait()
		if providerPackets.Load() != 1 || fixture.senderTerminals.Load()-terminals != 1 || time.Since(before) != 0 {
			t.Fatal("pure ACK retry needed a timer or lost ordinary provider delivery")
		}
	})
}

// An already-ACKed idle connection still emits meaningful repeated controls.
// No origin write or return DATA supplies a credit before these six ACKs.
func TestNativeRepeatedHealthyPureAcksNeedNoFreshData(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture := newNativeDuplicateAckFixture(t, false)
		defer fixture.close()
		providerPackets := watchNativePureAckProvider(t, fixture)
		fixture.consumer.stateLock.Lock()
		base := fixture.consumer.base
		fixture.consumer.stateLock.Unlock()
		ack := groupRecoveryPacket(fixture.initial+1, base, fixture.window, tcpFlagAck, nil)
		before := time.Now()
		terminals := fixture.senderTerminals.Load()
		for range 6 {
			if !fixture.offer(ack) {
				t.Fatal("healthy repeated pure ACK was refused without fresh return DATA")
			}
			synctest.Wait()
		}
		sequence := fixture.providerSequence.Load()
		sequence.mutex.Lock()
		retained, attempts := sequence.returnByteCount, sequence.returnAttempts
		sequence.mutex.Unlock()
		fixture.consumer.stateLock.Lock()
		returnPackets, replays := fixture.consumer.packets, fixture.consumer.replays
		fixture.consumer.stateLock.Unlock()
		if providerPackets.Load() != 6 || fixture.senderTerminals.Load()-terminals != 6 ||
			retained != 0 || attempts != 0 || returnPackets != 0 || replays != 0 || time.Since(before) != 0 {
			t.Fatalf("idle ACK feedback changed custody or time: provider=%d terminals=%d retained=%d attempts=%d returns=%d replays=%d elapsed=%s",
				providerPackets.Load(), fixture.senderTerminals.Load()-terminals, retained, attempts, returnPackets, replays, time.Since(before))
		}
	})
}

// The actual native batch keeps all six ACK events in one exact-flow group.
// Admission and final provider dispatch need no prior return-DATA evidence.
func TestNativeSixPureAckBatchNeedsNoFreshData(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture := newNativeDuplicateAckFixture(t, false)
		defer fixture.close()
		providerPackets := watchNativePureAckProvider(t, fixture)
		fixture.consumer.stateLock.Lock()
		base := fixture.consumer.base
		fixture.consumer.stateLock.Unlock()
		ack := groupRecoveryPacket(fixture.initial+1, base, fixture.window, tcpFlagAck, nil)
		before := time.Now()
		terminals := fixture.senderTerminals.Load()
		if count, accepted := offerNativePureAckPackets(t, fixture, ack, ack, ack, ack, ack, ack); count != 6 {
			t.Fatalf("six-ACK native batch refused: count=%d accepted=%v", count, accepted)
		}
		synctest.Wait()
		sequence := fixture.providerSequence.Load()
		sequence.mutex.Lock()
		retained, attempts := sequence.returnByteCount, sequence.returnAttempts
		sequence.mutex.Unlock()
		fixture.consumer.stateLock.Lock()
		returnPackets, replays := fixture.consumer.packets, fixture.consumer.replays
		fixture.consumer.stateLock.Unlock()
		if providerPackets.Load() != 6 || fixture.senderTerminals.Load() <= terminals || retained != 0 ||
			attempts != 0 || returnPackets != 0 || replays != 0 || time.Since(before) != 0 {
			t.Fatalf("six-ACK batch did not reach the original provider unchanged: provider=%d terminals=%d retained=%d attempts=%d returns=%d replays=%d elapsed=%s",
				providerPackets.Load(), fixture.senderTerminals.Load()-terminals, retained, attempts, returnPackets, replays, time.Since(before))
		}
	})
}

// A same-ISN SYN first keeps this as one group through affinity splitting.
// The redundant ACK cannot supply progress for that connection-state edge.
func TestNativePureAckCannotReleaseMixedRedundantSyn(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture := newNativeDuplicateAckFixture(t, false)
		defer fixture.close()
		providerPackets := watchNativePureAckProvider(t, fixture)
		fixture.consumer.stateLock.Lock()
		base := fixture.consumer.base
		fixture.consumer.stateLock.Unlock()
		syn := groupRecoveryPacket(fixture.initial, 0, fixture.window, tcpFlagSyn, nil)
		ack := groupRecoveryPacket(fixture.initial+1, base, fixture.window, tcpFlagAck, nil)
		if count, accepted := offerNativePureAckPackets(t, fixture, syn, ack); count != 0 {
			t.Fatalf("pure ACK released a redundant SYN group: count=%d accepted=%v", count, accepted)
		}
		synctest.Wait()
		if providerPackets.Load() != 0 {
			t.Fatal("mixed redundant SYN group reached the provider")
		}
		if !fixture.offer(ack) {
			t.Fatal("mixed SYN refusal prevented an independent pure ACK")
		}
		synctest.Wait()
		if providerPackets.Load() != 1 {
			t.Fatal("independent pure ACK did not preserve its provider owner")
		}
	})
}

// Exact provider delivery establishes upload coverage before its duplicate
// shares a native group with an otherwise admissible pure ACK. Later pure ACKs
// preserve that coverage while retaining their own ordinary admission.
func TestNativePureAckCannotReleaseMixedCoveredData(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture := newNativeDuplicateAckFixture(t, false)
		defer fixture.close()
		fixture.consumer.stateLock.Lock()
		base := fixture.consumer.base
		fixture.consumer.stateLock.Unlock()
		payload := []byte("data")
		readResult := make(chan error, 1)
		go func() {
			received := make([]byte, len(payload))
			_, err := io.ReadFull(fixture.origin, received)
			if err == nil && !bytes.Equal(received, payload) {
				t.Error("provider upload changed exact application bytes")
			}
			readResult <- err
		}()
		data := groupRecoveryPacket(fixture.initial+1, base, fixture.window, tcpFlagAck, payload)
		if !fixture.offer(data) {
			t.Fatal("initial upload DATA was refused")
		}
		synctest.Wait()
		if err := <-readResult; err != nil {
			t.Fatal(err)
		}
		providerPackets := watchNativePureAckProvider(t, fixture)
		ack := groupRecoveryPacket(fixture.initial+1+uint32(len(payload)), base, fixture.window, tcpFlagAck, nil)
		if count, accepted := offerNativePureAckPackets(t, fixture, data, ack); count != 1 || accepted[0] || !accepted[1] {
			t.Fatalf("mixed native group lost isolated ACK admission: count=%d accepted=%v", count, accepted)
		}
		synctest.Wait()
		if providerPackets.Load() != 1 {
			t.Fatal("mixed native group did not deliver exactly its pure ACK")
		}
		if !fixture.offer(ack) {
			t.Fatal("mixed DATA refusal prevented an independent pure ACK")
		}
		synctest.Wait()
		if providerPackets.Load() != 2 {
			t.Fatal("independent pure ACK did not reach the provider")
		}
		before, drops := time.Now(), fixture.parent.TcpCollapseDropCount()
		if fixture.offer(data) {
			t.Fatal("admitted pure ACK erased covered upload DATA")
		}
		synctest.Wait()
		if providerPackets.Load() != 2 || fixture.parent.TcpCollapseDropCount() != drops+1 {
			t.Fatal("covered upload DATA reached the provider after pure ACK admission")
		}
		if !fixture.offer(ack) {
			t.Fatal("covered DATA refusal prevented another independent pure ACK")
		}
		synctest.Wait()
		if providerPackets.Load() != 3 || time.Since(before) != 0 {
			t.Fatal("repeated pure ACK lost its ordinary admission after covered DATA refusal")
		}
	})
}

// Real parsed option/flag bytes reach the selected-client boundary unchanged;
// the shared classification permits SACK and ECN information on a pure ACK.
func TestNativePureAckGroupPreservesSackAndEcnBytes(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture := newNativeDuplicateAckFixture(t, false)
		defer fixture.close()
		fixture.consumer.stateLock.Lock()
		base := fixture.consumer.base
		fixture.consumer.stateLock.Unlock()
		packet := groupRecoveryPacket(fixture.initial+1, base, fixture.window, tcpFlagAck|0xc0, nil)
		path := groupRecoveryParsed(t, packet).ipPath
		packet = append(packet, 5, 10, 0, 0, 0, 0, 0, 0, 0, 0, 1, 1)
		binary.BigEndian.PutUint16(packet[2:4], uint16(len(packet)))
		packet[Ipv4HeaderSizeWithoutExtensions+12] = 8 << 4
		options := packet[Ipv4HeaderSizeWithoutExtensions+TcpHeaderSizeWithoutExtensions:]
		binary.BigEndian.PutUint32(options[2:6], base+4)
		binary.BigEndian.PutUint32(options[6:10], base+8)
		writeIpv4HeaderChecksum(packet[:Ipv4HeaderSizeWithoutExtensions])
		tcp := packet[Ipv4HeaderSizeWithoutExtensions:]
		binary.BigEndian.PutUint16(tcp[16:18], 0)
		binary.BigEndian.PutUint16(tcp[16:18], ipPathTransportChecksum(path, ipProtocolNumberTcp, tcp))
		parsed := groupRecoveryParsed(t, packet)
		group := &parsedPacketGroup{packets: []parsedPacket{*parsed}, ipPath: parsed.ipPath}
		if !IsTcpAckOnlyPacket(packet) || !group.onlyPureTcpAcks() {
			t.Fatal("parsed group classification diverged for SACK/ECN pure ACK")
		}
		calls := 0
		fixture.selected.sendGroupForTest = func(actual *parsedPacketGroup, _ time.Duration, ackRequired bool) (bool, error) {
			calls++
			if !ackRequired || len(actual.packets) != 2 {
				t.Error("native pure ACK group weakened Transfer recovery or group membership")
			}
			for _, member := range actual.packets {
				if !bytes.Equal(member.packet, packet) {
					t.Error("native pure ACK group changed SACK/ECN packet bytes")
				}
				MessagePoolReturn(member.packet)
			}
			return true, nil
		}
		if count, accepted := offerNativePureAckPackets(t, fixture, packet, packet); count != 2 || calls != 1 {
			t.Fatalf("option-bearing pure ACK group did not reach its selected client: count=%d accepted=%v calls=%d", count, accepted, calls)
		}
	})
}
