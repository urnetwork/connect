//go:build !windows && !js

// The unix echo backend's own state, without sockets: the identifiers a
// process's flows hold and the sequence numbers a flow awaits replies to
// (QD-1159). The socket tests are in ip_icmp_egress_darwin_test.go and
// ip_icmp_egress_linux_test.go.
package connect

import (
	"context"
	"math"
	"net"
	"testing"

	"github.com/urnetwork/connect/protocol"
)

// A set whose draws are the given identifiers, in turn.
func newIcmpEchoIdentifierSetForTest(t *testing.T, draws ...uint16) *icmpEchoIdentifierSet {
	t.Helper()
	return newIcmpEchoIdentifierSet(func() (uint16, error) {
		if len(draws) == 0 {
			t.Fatal("the identifier set drew more than the test scripted")
		}
		identifier := draws[0]
		draws = draws[1:]
		return identifier, nil
	})
}

func icmpEchoIdentifierHeld(identifiers *icmpEchoIdentifierSet, identifier uint16) bool {
	identifiers.stateLock.Lock()
	defer identifiers.stateLock.Unlock()
	return identifiers.heldIdentifiers[identifier]
}

func icmpEchoIdentifierHeldCount(identifiers *icmpEchoIdentifierSet) int {
	identifiers.stateLock.Lock()
	defer identifiers.stateLock.Unlock()
	return len(identifiers.heldIdentifiers)
}

func takeIcmpEchoIdentifier(t *testing.T, identifiers *icmpEchoIdentifierSet) uint16 {
	t.Helper()
	identifier, err := identifiers.take()
	if err != nil {
		t.Fatal(err)
	}
	return identifier
}

// A draw a live flow holds moves on to the next identifier none holds,
// skipping zero and wrapping past the top; a released identifier can be drawn
// again.
func TestIcmpEchoIdentifierSetSkipsHeldIdentifiers(t *testing.T) {
	identifiers := newIcmpEchoIdentifierSetForTest(t, 0xa5c3, 0xa5c3, 0xa5c3, 0xa5c3, 0, math.MaxUint16, math.MaxUint16)

	AssertEqual(t, takeIcmpEchoIdentifier(t, identifiers), uint16(0xa5c3))
	AssertEqual(t, takeIcmpEchoIdentifier(t, identifiers), uint16(0xa5c4))
	AssertEqual(t, takeIcmpEchoIdentifier(t, identifiers), uint16(0xa5c5))
	identifiers.release(0xa5c3)
	AssertEqual(t, icmpEchoIdentifierHeld(identifiers, 0xa5c3), false)
	AssertEqual(t, takeIcmpEchoIdentifier(t, identifiers), uint16(0xa5c3))

	// zero reads as an unbound socket, so a zero draw takes the next
	AssertEqual(t, takeIcmpEchoIdentifier(t, identifiers), uint16(1))
	AssertEqual(t, takeIcmpEchoIdentifier(t, identifiers), uint16(math.MaxUint16))
	// held at the top: past it, zero, and the held 1, to 2
	AssertEqual(t, takeIcmpEchoIdentifier(t, identifiers), uint16(2))
	AssertEqual(t, icmpEchoIdentifierHeldCount(identifiers), 6)
}

// With every identifier held a flow gets none, and fails soft; the first one
// released is the one the next flow takes, whatever its draw.
func TestIcmpEchoIdentifierSetRefusesWhenEveryIdentifierIsHeld(t *testing.T) {
	identifiers := newIcmpEchoIdentifierSetForTest(t, 0x0101, 0x0101)
	for identifier := 1; identifier <= math.MaxUint16; identifier += 1 {
		identifiers.heldIdentifiers[uint16(identifier)] = true
	}
	if identifier, err := identifiers.take(); err == nil {
		t.Fatalf("took identifier %d with all of them held", identifier)
	}
	identifiers.release(0xfedc)
	AssertEqual(t, takeIcmpEchoIdentifier(t, identifiers), uint16(0xfedc))
}

// The process's identifiers are drawn at random, not counted up from zero as
// they were, which gave the first flow of every process identifier 1: 64 of
// them in a row are not a counter's run. A random draw makes that run once in
// 65,536^63 tries.
func TestIcmpEgressProcessIdentifiersAreRandom(t *testing.T) {
	var identifiers []uint16
	defer func() {
		for _, identifier := range identifiers {
			icmpEgressProcessIdentifiers.release(identifier)
		}
	}()
	for range 64 {
		identifiers = append(identifiers, takeIcmpEchoIdentifier(t, icmpEgressProcessIdentifiers))
	}
	counted := true
	for i := 1; i < len(identifiers); i += 1 {
		if identifiers[i] != identifiers[i-1]+1 {
			counted = false
		}
	}
	if counted {
		t.Fatalf("the process's identifiers count up from %d", identifiers[0])
	}
	for _, identifier := range identifiers {
		AssertEqual(t, icmpEchoIdentifierHeld(icmpEgressProcessIdentifiers, identifier), true)
	}
}

// A reply is kept once for each echo sent with its sequence number: never
// for one not sent, and not again once answered.
func TestIcmpEchoesInFlightAwaitEachSentEchoOnce(t *testing.T) {
	settings := DefaultIcmpBufferSettings()
	settings.InFlightEchoLimit = 4
	echoes := newIcmpEchoesInFlight(settings)

	echoes.add(1)
	echoes.add(2)
	AssertEqual(t, echoes.answer(3), false)
	AssertEqual(t, echoes.answer(2), true)
	AssertEqual(t, echoes.answer(2), false)
	AssertEqual(t, echoes.answer(1), true)
	AssertEqual(t, echoes.answer(1), false)

	// a sequence number sent twice awaits two replies, and a third drops
	echoes.add(5)
	echoes.add(5)
	AssertEqual(t, echoes.answer(5), true)
	AssertEqual(t, echoes.answer(5), true)
	AssertEqual(t, echoes.answer(5), false)

	// the wrap is a sequence number like any other
	echoes.add(math.MaxUint16)
	echoes.add(0)
	AssertEqual(t, echoes.answer(0), true)
	AssertEqual(t, echoes.answer(math.MaxUint16), true)
	AssertEqual(t, len(echoes.sequenceNumbers), 0)
}

// Past its limit a flow forgets its oldest awaited echo, whose late reply then
// drops, and it never grows past the limit; a non-positive limit awaits one.
func TestIcmpEchoesInFlightForgetTheOldestPastTheLimit(t *testing.T) {
	settings := DefaultIcmpBufferSettings()
	settings.InFlightEchoLimit = 3
	echoes := newIcmpEchoesInFlight(settings)
	for sequenceNumber := uint16(1); sequenceNumber <= 4; sequenceNumber += 1 {
		echoes.add(sequenceNumber)
	}
	AssertEqual(t, cap(echoes.sequenceNumbers), 3)
	AssertEqual(t, echoes.answer(1), false)
	AssertEqual(t, echoes.answer(4), true)
	AssertEqual(t, echoes.answer(2), true)
	AssertEqual(t, echoes.answer(3), true)

	for _, limit := range []int{0, -1} {
		settings.InFlightEchoLimit = limit
		echoes := newIcmpEchoesInFlight(settings)
		echoes.add(1)
		echoes.add(2)
		if echoes.answer(1) || !echoes.answer(2) {
			t.Errorf("limit %d: awaits %v rather than the last echo alone", limit, echoes.sequenceNumbers)
		}
	}
}

// An echo whose send failed awaits no reply: a retry of it awaits one, not two.
func TestIcmpEchoesInFlightWithdrawAnUnsentEcho(t *testing.T) {
	echoes := newIcmpEchoesInFlight(DefaultIcmpBufferSettings())
	echoes.add(7)
	echoes.add(8)
	echoes.add(7)
	echoes.withdraw(7)
	echoes.withdraw(9)
	AssertEqual(t, echoes.sequenceNumbers, []uint16{7, 8})
	AssertEqual(t, echoes.answer(7), true)
	AssertEqual(t, echoes.answer(7), false)
	AssertEqual(t, echoes.answer(8), true)
}

// The flow's reservation holds its awaited sequence numbers.
func TestIcmpFlowReservationCoversTheEchoesInFlight(t *testing.T) {
	settings := DefaultIcmpBufferSettings()
	settings.MemoryBudget = NewTransferMemoryBudget(mib(1))
	settings.InFlightEchoLimit = 1000
	sequence := NewIcmpSequence(
		context.Background(),
		func(TransferPath, protocol.ProvideMode, *IpPath, []byte) {},
		SourceId(NewId()), protocol.ProvideMode_Network, 4,
		net.IPv4(192, 0, 2, 1).To4(), 1, net.IPv4(198, 51, 100, 1).To4(), settings,
	)
	if sequence == nil {
		t.Fatal("could not admit an echo flow")
	}
	defer sequence.memory.release()
	defer sequence.Cancel()
	AssertEqual(t, sequence.memory.bytes, natIcmpFlowMemoryByteCount(settings)+ByteCount(2000))
}
