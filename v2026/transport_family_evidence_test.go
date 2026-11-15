package connect

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"
)

// No DNS, socket or timer races: drive the typed dial observations that H1
// and H3 publish and inspect the standby predicate after each exact transition.
func TestFamilyStandbyTracksChangingDialEvidence(t *testing.T) {
	newPin := func(family int) *PlatformTransport {
		pin := &PlatformTransport{ipFamily: family, log: NewNoopLogger(), connectedMonitor: NewMonitor(), pinnedBackoff: newPinnedDialBackoff(time.Second, time.Minute)}
		pin.enabled.Store(true)
		pin.familyHold.Store(int32(PlatformTransportStateConnecting))
		return pin
	}
	v4, v6 := newPin(4), newPin(6)
	group := &FamilyPlatformTransportGroup{ipv4Transport: v4, ipv6Transport: v6}
	missing := fmt.Errorf("dial: %w", &net.DNSError{Name: "missing.example", IsNotFound: true})
	for _, pin := range []*PlatformTransport{v4, v6} {
		notify := pin.ConnectedNotify()
		observeDialAttempt(withDialAttemptObserver(context.Background(), pin.noteDialError), missing)
		select {
		case <-notify:
		default:
			t.Fatal("new DNS evidence did not wake the group")
		}
		if group.pinnedCannotConnectWithLock() != (pin == v6) {
			t.Fatal("one usable pin did not retain the delay")
		}
	}
	v4.noteDialError(&net.DNSError{Name: "temporary.example", IsTimeout: true})
	if group.pinnedCannotConnectWithLock() {
		t.Fatal("temporary failure retained obsolete NXDOMAIN evidence")
	}
	v4.noteDialError(missing)
	v6.noteDialSuccess()
	if group.pinnedCannotConnectWithLock() || v6.unresolvableHost() {
		t.Fatal("successful H3 dial did not clear missing-host evidence")
	}
	v6.noteDialError(missing)
	v4.registeredCount.Store(1)
	if group.pinnedCannotConnectWithLock() {
		t.Fatal("connected pin was declared unable to connect")
	}
	v4.registeredCount.Store(0)
	v4.familyHold.Store(int32(PlatformTransportStateSleeping))
	if !group.pinnedCannotConnectWithLock() {
		t.Fatal("sleeping family prevented fallback beside an unresolved pin")
	}
	group.ipv4Transport = nil
	if !group.pinnedCannotConnectWithLock() {
		t.Fatal("absent family prevented fallback")
	}
}
