//go:build linux

// The linux echo backend keeps the identifier its ping socket's kernel bound:
// QD-1159's random draw and sequence check are for darwin, whose kernel binds
// none.
package connect

import (
	"context"
	"net"
	"testing"
	"time"
)

// On linux the echo identifier is the ping socket's kernel-assigned port. The
// flow draws nothing from the process's identifiers and awaits no sequence
// numbers, since that kernel delivers a ping socket only its own replies, and
// an echo to loopback is answered as before.
func TestIcmpEgressKeepsTheKernelIdentifierLinux(t *testing.T) {
	requireIcmpEgress(t)
	heldCount := icmpEchoIdentifierHeldCount(icmpEgressProcessIdentifiers)
	egressIcmp, err := newIcmpEgress(context.Background(), 4, net.IPv4(127, 0, 0, 1).To4(), DefaultIcmpBufferSettings())
	if err != nil {
		t.Fatal(err)
	}
	egress := egressIcmp.(*icmpEgressConn)
	defer egress.Close()
	port := egress.conn.LocalAddr().(*net.UDPAddr).Port
	if port == 0 {
		t.Fatal("the ping socket has no kernel-assigned identifier")
	}
	AssertEqual(t, int(egress.identifier), port)
	AssertEqual(t, egress.identifiers == nil, true)
	AssertEqual(t, egress.echoesInFlight == nil, true)
	AssertEqual(t, icmpEchoIdentifierHeldCount(icmpEgressProcessIdentifiers), heldCount)

	if err := egress.WriteEcho(time.Time{}, 64, 3, []byte("linux")); err != nil {
		t.Fatal(err)
	}
	// the bound only keeps a lost reply from hanging the test
	sequenceNumber, payload, err := egress.ReadEcho(time.Now().Add(10 * time.Second))
	if err != nil {
		t.Fatal(err)
	}
	AssertEqual(t, sequenceNumber, uint16(3))
	AssertEqual(t, string(payload), "linux")
}
