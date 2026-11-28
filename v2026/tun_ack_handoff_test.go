package connect

import (
	"context"
	"encoding/binary"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/ports"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/waiter"
)

// Header options and ECN still describe pure ACKs. Payload, lifecycle flags
// and malformed TCP header lengths must retain the data handoff.
func TestTunAckHandoffClassifiesOnlyAcknowledgements(t *testing.T) {
	for _, version := range []int{4, 6} {
		for _, test := range []struct {
			name       string
			flags      byte
			options    int
			payload    int
			headerSize int
			want       bool
		}{
			{name: "ack", flags: tcpFlagAck, options: 0, payload: 0, headerSize: 20, want: true},
			{name: "options", flags: tcpFlagAck, options: 12, payload: 0, headerSize: 32, want: true},
			{name: "ecn", flags: tcpFlagAck | 0x40, options: 0, payload: 0, headerSize: 20, want: true},
			{name: "data", flags: tcpFlagAck, options: 0, payload: 1, headerSize: 20, want: false},
			{name: "syn", flags: tcpFlagAck | tcpFlagSyn, options: 0, payload: 0, headerSize: 20, want: false},
			{name: "fin", flags: tcpFlagAck | tcpFlagFin, options: 0, payload: 0, headerSize: 20, want: false},
			{name: "rst", flags: tcpFlagAck | tcpFlagRst, options: 0, payload: 0, headerSize: 20, want: false},
			{name: "no-ack", flags: 0, options: 0, payload: 0, headerSize: 20, want: false},
			{name: "short-header", flags: tcpFlagAck, options: 0, payload: 0, headerSize: 16, want: false},
			{name: "long-header", flags: tcpFlagAck, options: 0, payload: 0, headerSize: 24, want: false},
		} {
			headerSize := Ipv4HeaderSizeWithoutExtensions
			if version == 6 {
				headerSize = Ipv6HeaderSize
			}
			packet := make([]byte, headerSize+20+test.options+test.payload)
			if version == 4 {
				packet[0], packet[9] = 0x45, byte(ipProtocolNumberTcp)
				binary.BigEndian.PutUint16(packet[2:4], uint16(len(packet)))
			} else {
				packet[0], packet[6] = 0x60, byte(ipProtocolNumberTcp)
				binary.BigEndian.PutUint16(packet[4:6], uint16(len(packet)-headerSize))
			}
			packet[headerSize+12], packet[headerSize+13] = byte(test.headerSize/4)<<4, test.flags
			if _, _, ok := tcpInboundFlow(packet); !ok {
				t.Fatalf("IPv%d %s: invalid flow fixture", version, test.name)
			}
			if got := tcpInboundAcknowledgementOnly(packet); got != test.want {
				t.Errorf("IPv%d %s: acknowledgement-only=%t, want %t", version, test.name, got, test.want)
			}
		}
	}
}

// A TCP processor can own the endpoint while waiting for outbound capacity.
// An inbound ACK callback must return without taking that lock: the same
// Transfer carrier carries the capacity-release ACK needed to drain output.
func TestTunAckHandoffDoesNotWaitForBusyTcpEndpoint(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tun, err := CreateTunWithDefaults(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tun.Close()
	for _, batch := range []bool{false, true} {
		packet := newTunTcpInboundTestPacket(40000, 443)
		packet[32], packet[33] = 5<<4, tcpFlagAck
		id, _, _ := tcpInboundFlow(packet)
		var waitQueue waiter.Queue
		raw, err := tun.stack.NewEndpoint(tcp.ProtocolNumber, ipv4.ProtocolNumber, &waitQueue)
		if err != nil {
			t.Fatal(err)
		}
		endpoint := raw.(*tcp.Endpoint)
		protocols := []tcpip.NetworkProtocolNumber{ipv4.ProtocolNumber}
		if err := tun.stack.RegisterTransportEndpoint(protocols, tcp.ProtocolNumber, id, endpoint, ports.Flags{}, tun.nicId); err != nil {
			endpoint.Close()
			t.Fatal(err)
		}
		endpoint.LockUser() // force the dependency; no scheduler race can unlock it
		done := make(chan error, 1)
		go func() {
			var err error
			if batch {
				_, err = tun.WriteBatch([][]byte{packet, packet})
			} else {
				_, err = tun.Write(packet)
			}
			done <- err
		}()
		blocked := false
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(time.Second): // failure deadline; the lock forces the ordering
			blocked = true
		}
		endpoint.UnlockUser()
		if blocked {
			<-done
			t.Errorf("batch=%t: ACK handoff waited for a busy TCP endpoint", batch)
		}
		tun.stack.UnregisterTransportEndpoint(protocols, tcp.ProtocolNumber, id, endpoint, ports.Flags{}, tun.nicId)
		endpoint.Close()
	}
}
