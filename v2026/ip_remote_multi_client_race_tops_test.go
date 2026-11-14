package connect

// Tests of the race's order over a merged field (owner, 2026-11-13: "if it's
// 2, i want to make sure a member of the limited window is always tried. I
// want to combine that with the merged sort, but keep the top from each list
// to be always included"; server FP2-PROTOCOL.md §7.5): each window's top
// first, ordered between themselves by weight, then the merged weighted order;
// a held top gives way to its window's next; a field of one window is left as
// it is. The shuffles are pinned as in ip_remote_multi_client_race_merge_test.go,
// so every weighted order is descending weight.

import (
	"testing"
	"time"
)

// The limited window's top is in the first two of a tcp race even when every
// limited exit is lighter than every normal exit, beside the normal window's
// top, and the rest follow the merged weights.
func TestRaceTakesTheLimitedTopEvenWhenItIsLightest(t *testing.T) {
	fixture := newRaceMergeTestFixture(t, true)
	fixture.addLightLimitedExits(t)
	var offerChanged, limitedOfferChanged <-chan struct{}
	field := fixture.parent.raceField(fixture.quality, flowNeed{ipVersion: 4, ipProtocol: IpProtocolTcp}, &offerChanged, &limitedOfferChanged)
	if got := fixture.order(field); got != "first second third tcp tcp2" {
		t.Fatalf("merged field %q, want first second third tcp tcp2", got)
	}
	if got := fixture.order(raceWindowTopsFirst(field, fixture.settings, time.Now())); got != "first tcp second third tcp2" {
		t.Errorf("race order %q, want the two tops, first tcp, then the merged weights", got)
	}
	if got := fixture.order(field); got != "first second third tcp tcp2" {
		t.Errorf("the caller's field was reordered to %q", got)
	}
}

// The normal window's top is in the first two even when every limited exit
// is heavier than every normal exit.
func TestRaceTakesTheNormalTopEvenWhenItIsLightest(t *testing.T) {
	fixture := newRaceMergeTestFixture(t, true)
	fixture.add(t, fixture.quality, "first", capabilityClassA, 10_000)
	fixture.add(t, fixture.quality, "second", capabilityClassB, 1_000)
	fixture.add(t, fixture.limited, "tcp", capabilityClassD, 1_000_000)
	fixture.add(t, fixture.limited, "tcp2", capabilityClassC, 100_000)
	var offerChanged, limitedOfferChanged <-chan struct{}
	field := fixture.parent.raceField(fixture.quality, flowNeed{ipVersion: 4, ipProtocol: IpProtocolTcp}, &offerChanged, &limitedOfferChanged)
	if got := fixture.order(field); got != "tcp tcp2 first second" {
		t.Fatalf("merged field %q, want tcp tcp2 first second", got)
	}
	if got := fixture.order(raceWindowTopsFirst(field, fixture.settings, time.Now())); got != "tcp first tcp2 second" {
		t.Errorf("race order %q, want the two tops, tcp first, then the merged weights", got)
	}
}

// A udp race is the quality window's field as it is: no limited exit can
// carry it, so there is nothing to combine, and the same list comes back.
func TestUdpRaceOrderIsTheWindowsOwn(t *testing.T) {
	fixture := newRaceMergeTestFixture(t, true)
	fixture.addLightLimitedExits(t)
	var offerChanged, limitedOfferChanged <-chan struct{}
	field := fixture.parent.raceField(fixture.quality, flowNeed{ipVersion: 4, ipProtocol: IpProtocolUdp}, &offerChanged, &limitedOfferChanged)
	got := raceWindowTopsFirst(field, fixture.settings, time.Now())
	if fixture.order(got) != "first second third" || len(got) != len(field) || &got[0] != &field[0] {
		t.Errorf("udp race order %q, want the window's own field first second third as it is", fixture.order(got))
	}
}

// A top whose contract requests are held gives way to the next exit of its
// window, and the held exit races last.
func TestRaceTopsSkipAHeldClient(t *testing.T) {
	fixture := newRaceMergeTestFixture(t, true)
	fixture.addLightLimitedExits(t)
	fixture.hold(t, "tcp")
	var offerChanged, limitedOfferChanged <-chan struct{}
	field := fixture.parent.raceField(fixture.quality, flowNeed{ipVersion: 4, ipProtocol: IpProtocolTcp}, &offerChanged, &limitedOfferChanged)
	now := time.Now()
	if got := fixture.order(contractRateLimitHeldLast(raceWindowTopsFirst(field, fixture.settings, now), now)); got != "first tcp2 second third tcp" {
		t.Errorf("race order with the limited top held %q, want first tcp2 second third tcp", got)
	}
	// every limited exit held: no limited top to combine, and the held go last
	fixture.hold(t, "tcp2")
	field = fixture.parent.raceField(fixture.quality, flowNeed{ipVersion: 4, ipProtocol: IpProtocolTcp}, &offerChanged, &limitedOfferChanged)
	if got := fixture.order(contractRateLimitHeldLast(raceWindowTopsFirst(field, fixture.settings, now), now)); got != "first second third tcp tcp2" {
		t.Errorf("race order with every limited exit held %q, want first second third tcp tcp2", got)
	}
}
