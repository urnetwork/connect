// A probe answer belongs to the exact registered local channel generation.
package connect

import (
	"bytes"
	"encoding/binary"
	"net"
	"testing"

	"github.com/urnetwork/connect/protocol"
)

// Two different local channels deliberately share the same provider identity.
// Provider equality cannot substitute for the registered channel owner.
func testingProbeExactAnswerOwner(t *testing.T, batch bool, class string) {
	t.Helper()
	parent, owner, _ := probeTestParent(t)
	foreign := probeTestChannel(owner.settings)
	destination := RequireMultiHopId(NewId())
	owner.args.Destination, foreign.args.Destination = destination, destination
	if owner == foreign || owner.args.Destination.Tail() != foreign.args.Destination.Tail() {
		t.Fatal("fixture did not construct different channels to the same provider")
	}
	target := probeTestTarget()
	var names []string
	if class != "health" {
		target = probeResolverTarget(net.IPv4(198, 51, 100, 53), "")
		if class == "shared-dns" {
			names = []string{"owner.example.test"}
		}
	}
	probe, ok := parent.registerProbeFlow(owner, target, names...)
	if !ok {
		t.Fatal("could not register actual probe owner")
	}
	defer parent.unregisterProbeFlows([]*probeFlow{probe})
	parent.stateLock.Lock()
	update := parent.ip4PathUpdates[probe.ipPath.ToIp4Path()]
	parent.stateLock.Unlock()
	result := probe
	payload := make([]byte, 12)
	binary.BigEndian.PutUint16(payload[:2], uint16(probe.synSequence))
	payload[2], payload[3] = 0x81, 0x80
	var template []byte
	if class == "health" {
		path := probe.ipPath.Reverse()
		template = ipOosTcpPacketSequence(path, tcpFlagSyn|tcpFlagAck, 907, nil)
		tcp := template[int(template[0]&15)*4:]
		binary.BigEndian.PutUint32(tcp[8:12], probe.synSequence+1)
		binary.BigEndian.PutUint16(tcp[16:18], 0)
		binary.BigEndian.PutUint16(tcp[16:18], ipPathTransportChecksum(path, ipProtocolNumberTcp, tcp))
	} else {
		template = ipOosUdpPacket(probe.ipPath.Reverse(), payload)
		if class == "shared-dns" {
			result = probe.dnsQueries[uint16(probe.synSequence)]
		}
	}
	path, err := ParseIpPath(template)
	if err != nil || result == nil {
		t.Fatal("could not parse the actual probe response")
	}
	packet := MessagePoolCopy(template)
	witness := MessagePoolShareReadOnly(packet)
	defer func() {
		if MessagePoolReturn(packet) {
			t.Error("probe receive consumed its borrowed response")
		}
		if !MessagePoolReturn(witness) {
			t.Error("probe receive retained its borrowed response")
		}
	}()
	forwarded := 0
	parent.SetReceivePacketCallback(func(TransferPath, protocol.ProvideMode, *IpPath, []byte) { forwarded++ })
	if batch {
		parent.SetReceivePacketsCallback(func(_ TransferPath, _ protocol.ProvideMode, _ *IpPath, packets [][]byte) { forwarded += len(packets) })
	}
	receive := func(channel *multiClientChannel) {
		if batch {
			parent.clientReceivePackets(channel, SourceId(destination.Tail()), protocol.ProvideMode_Public, TransportTypeH1, []*IpPath{path}, [][]byte{packet})
		} else {
			parent.clientReceivePacket(channel, SourceId(destination.Tail()), protocol.ProvideMode_Public, TransportTypeH1, path, packet)
		}
	}
	receive(foreign)
	parent.stateLock.Lock()
	current := parent.ip4PathUpdates[probe.ipPath.ToIp4Path()]
	parent.stateLock.Unlock()
	completed := false
	select {
	case <-result.done:
		completed = true
	default:
	}
	if current != update || update.client.Load() != owner || update.IsDone() || completed || result.answered.Load() || len(foreign.connectSuccessTimes) != 0 || forwarded != 0 {
		t.Fatalf("PROBE_ANSWER_WRONG_CHANNEL_CONSUMED: batch=%t class=%s different local channel to the same provider changed registered probe custody", batch, class)
	}

	receive(owner)
	select {
	case <-result.done:
		if !result.answered.Load() {
			t.Fatal("rightful channel did not publish positive probe completion")
		}
	default:
		t.Fatal("foreign response prevented the rightful owner's completion")
	}
	if class == "shared-dns" {
		if update.IsDone() || !bytes.Equal(result.answer, payload) {
			t.Fatal("shared DNS answer lost its live anchor or copied payload")
		}
		parent.unregisterProbeFlows([]*probeFlow{probe})
	}
	parent.stateLock.Lock()
	current = parent.ip4PathUpdates[probe.ipPath.ToIp4Path()]
	parent.stateLock.Unlock()
	expectedConnects := 0
	if class == "health" {
		expectedConnects = 1
	}
	if current != nil || !update.IsDone() || len(owner.connectSuccessTimes) != expectedConnects || len(foreign.connectSuccessTimes) != 0 || forwarded != 0 {
		t.Fatal("rightful completion changed exact owner, cleanup or probe consumption")
	}
	receive(foreign)
	receive(owner)
	if len(owner.connectSuccessTimes) != expectedConnects || len(foreign.connectSuccessTimes) != 0 || forwarded != 0 || !result.answered.Load() || owner.IsDone() || foreign.IsDone() {
		t.Fatal("late/duplicate response reopened or credited another probe owner")
	}
}

// A mismatched channel cannot consume a health/DNS question or credit itself,
// even when its authenticated provider destination equals the current owner's.
func TestProbeAnswerRequiresExactRegisteredChannel(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, batch := range []bool{false, true} {
		for _, class := range []string{"health", "dns", "shared-dns"} {
			testingProbeExactAnswerOwner(t, batch, class)
		}
	}
}
