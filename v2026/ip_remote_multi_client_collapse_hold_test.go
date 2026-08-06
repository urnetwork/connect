package connect

import (
	"context"
	"net"
	"testing"
	"time"
)

func collapseTestPacket(seq uint32, ack uint32, payload int, syn bool, rst bool) *parsedPacket {
	return &parsedPacket{
		payload: make([]byte, payload),
		ipPath: &IpPath{
			Version:           4,
			Protocol:          IpProtocolTcp,
			SourceIp:          net.ParseIP("10.11.12.13"),
			SourcePort:        54321,
			DestinationIp:     net.ParseIP("93.184.216.34"),
			DestinationPort:   443,
			SequenceNumber:    seq,
			AckSequenceNumber: ack,
			Syn:               syn,
			Rst:               rst,
		},
	}
}

func collapseTestClient(maxHold time.Duration) (*RemoteUserNatMultiClient, *multiClientChannelUpdate) {
	client := &RemoteUserNatMultiClient{
		settings: &MultiClientSettings{
			TcpCollapsePrevention: true,
			TcpCollapseMaxHold:    maxHold,
		},
	}
	update := newMultiClientChannelUpdate(context.Background(), collapseTestPacket(0, 0, 0, false, false).ipPath)
	return client, update
}

// a retransmit inside the hold is still collapsed, and the same retransmit
// after the hold is admitted so a stalled flow can recover without waiting out
// the 30s AckTimeout
func TestTcpCollapseHoldReleasesRetransmit(t *testing.T) {
	maxHold := 50 * time.Millisecond
	client, update := collapseTestClient(maxHold)

	// commit a data packet: this advances the sequence state
	first := collapseTestPacket(1000, 5000, 100, false, false)
	AssertEqual(t, client.canSendPacket(first, update), true)
	update.updateSequence(first)

	// an identical retransmit is collapsed while inside the hold
	retransmit := collapseTestPacket(1000, 5000, 100, false, false)
	AssertEqual(t, client.canSendPacket(retransmit, update), false)

	time.Sleep(maxHold + 20*time.Millisecond)

	// past the hold, the retransmit is let through
	AssertEqual(t, client.canSendPacket(retransmit, update), true)

	// and the window restarts, so the backlog is not released all at once
	AssertEqual(t, client.canSendPacket(retransmit, update), false)
}

// zero must reproduce the previous behavior exactly: retransmits collapsed
// indefinitely, however long the flow has been stuck
func TestTcpCollapseHoldDisabled(t *testing.T) {
	client, update := collapseTestClient(0)

	first := collapseTestPacket(1000, 5000, 100, false, false)
	AssertEqual(t, client.canSendPacket(first, update), true)
	update.updateSequence(first)

	retransmit := collapseTestPacket(1000, 5000, 100, false, false)
	AssertEqual(t, client.canSendPacket(retransmit, update), false)

	time.Sleep(60 * time.Millisecond)

	AssertEqual(t, client.canSendPacket(retransmit, update), false)
}

// the hold must not interfere with packets that legitimately advance the flow,
// nor with syn/rst which always pass
func TestTcpCollapseHoldAllowsProgress(t *testing.T) {
	client, update := collapseTestClient(50 * time.Millisecond)

	first := collapseTestPacket(1000, 5000, 100, false, false)
	AssertEqual(t, client.canSendPacket(first, update), true)
	update.updateSequence(first)

	// new data later in sequence space
	next := collapseTestPacket(1100, 5000, 100, false, false)
	AssertEqual(t, client.canSendPacket(next, update), true)

	// a pure ack that advances the ack number
	ack := collapseTestPacket(1100, 6000, 0, false, false)
	AssertEqual(t, client.canSendPacket(ack, update), true)

	// syn and rst are never collapsed
	AssertEqual(t, client.canSendPacket(collapseTestPacket(1000, 5000, 100, true, false), update), true)
	AssertEqual(t, client.canSendPacket(collapseTestPacket(1000, 5000, 100, false, true), update), true)
}

// with collapse prevention off entirely, the hold is irrelevant and everything
// passes as before
func TestTcpCollapseHoldIgnoredWhenPreventionOff(t *testing.T) {
	client := &RemoteUserNatMultiClient{
		settings: &MultiClientSettings{
			TcpCollapsePrevention: false,
			TcpCollapseMaxHold:    50 * time.Millisecond,
		},
	}
	update := newMultiClientChannelUpdate(context.Background(), collapseTestPacket(0, 0, 0, false, false).ipPath)

	first := collapseTestPacket(1000, 5000, 100, false, false)
	AssertEqual(t, client.canSendPacket(first, update), true)
	update.updateSequence(first)

	AssertEqual(t, client.canSendPacket(collapseTestPacket(1000, 5000, 100, false, false), update), true)
}

// udp is unaffected by the tcp collapse path
func TestTcpCollapseHoldUdpUnaffected(t *testing.T) {
	client, update := collapseTestClient(50 * time.Millisecond)

	udp := collapseTestPacket(1000, 5000, 100, false, false)
	udp.ipPath.Protocol = IpProtocolUdp
	AssertEqual(t, client.canSendPacket(udp, update), true)
	AssertEqual(t, client.canSendPacket(udp, update), true)
}

func TestDefaultMultiClientSettingsSetsTcpCollapseMaxHold(t *testing.T) {
	maxHold := DefaultMultiClientSettings().TcpCollapseMaxHold
	AssertEqual(t, 0 < maxHold, true)
	// must stay well under the AckTimeout it exists to preempt
	AssertEqual(t, maxHold < DefaultMultiClientSettings().AckTimeout, true)
}
