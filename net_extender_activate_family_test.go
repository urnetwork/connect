package connect

import (
	"errors"
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// Per-family schedules of the activation loop (EXTENDER.md G3).
//
// The lag.dev health check of 2026-11-01 found extenders re-activated four to
// thirty times a day with nothing wrong with their records: a v4 behind a NAT,
// refused at every post, held the whole pass and re-posted the v6 that worked
// on every step of the shared backoff, every six hours at the cap; and a
// dual-stack host whose hourly hello arrived on the other family than the hour
// before read as a moved address and re-posted both families. Each family now
// keeps its own tick and hold, and the hello is compared within its family.
//
// These run on the fixture of net_extender_activate_test.go. The fixture's
// address check is a minute, so every step of a minute or more begins with a
// hello, the pass barrier: the hello of a pass proves the pass before it
// finished.

const testActivateFamilyRefusal = "the tcp carrier did not answer"

// Wakes the loop, waits for the hello that begins the pass it starts, and
// returns that pass's first post. Consuming the hello keeps the next step's
// barrier the next pass's own.
func (self *testActivatorFixture) wakeForPost() *testActivatePost {
	self.t.Helper()
	self.wake()
	self.waitPass()
	return self.waitPost()
}

// Checks the posts each family has had, once a step proves every earlier pass
// finished.
func (self *testActivatorFixture) expectFamilyPostCounts(what string, v4Count int, v6Count int) {
	self.t.Helper()
	if count := self.operator.postCount(4); count != v4Count {
		self.t.Fatalf("%s: v4 posts = %d, expected %d", what, count, v4Count)
	}
	if count := self.operator.postCount(6); count != v6Count {
		self.t.Fatalf("%s: v6 posts = %d, expected %d", what, count, v6Count)
	}
}

// Waits for the first pass, a post of each family, v4 first.
func (self *testActivatorFixture) waitFirstPass() {
	self.t.Helper()
	self.waitPass()
	for _, ipVersion := range []int{4, 6} {
		if post := self.waitPost(); post.ipVersion != ipVersion {
			self.t.Fatalf("first pass posted v%d, expected v%d", post.ipVersion, ipVersion)
		}
	}
}

// A family the operator refuses holds on its own backoff, and the family it
// accepts is posted on its own 24 hour tick and not on any step of the other's
// backoff: once a day, where it was every six hours.
func TestExtenderActivatorHoldsARefusedFamilyAlone(t *testing.T) {
	fixture := newTestActivatorFixtureWithOperator(t, func(operator *testActivateOperator) {
		operator.setFamilyRefusal(4, testActivateFamilyRefusal)
	}, nil)
	startTime := fixture.clock.Now()
	fixture.waitFirstPass()
	if ipVersion := fixture.waitActivation(); ipVersion != 6 {
		t.Fatalf("activated v%d, expected v6", ipVersion)
	}
	status := fixture.activator.Status()
	if family := status.Family(4); family.Activated || !family.LastRefused {
		t.Fatalf("v4 status = %+v, expected refused", family)
	}
	if family := status.Family(6); !family.Activated {
		t.Fatalf("v6 status = %+v, expected activated", family)
	}

	// v4's holds land at a cumulative 10, 30, 70, 150, 310 and 630 minutes,
	// then every six hours, as one family's always have
	holds := []time.Duration{
		10 * time.Minute,
		20 * time.Minute,
		40 * time.Minute,
		80 * time.Minute,
		160 * time.Minute,
		320 * time.Minute,
		6 * time.Hour,
		6 * time.Hour,
	}
	postTime := startTime
	for i, hold := range holds {
		fixture.step(hold - time.Minute)
		fixture.expectFamilyPostCounts(
			fmt.Sprintf("one minute short of v4's %s hold", hold), i+1, 1)
		fixture.clock.advance(time.Minute)
		post := fixture.wakeForPost()
		postTime = postTime.Add(hold)
		if post.ipVersion != 4 || post.postTime != postTime {
			t.Fatalf(
				"post at hold %d = v%d at %s, expected v4 at %s",
				i+1, post.ipVersion, post.postTime.Sub(startTime), postTime.Sub(startTime))
		}
	}

	// 22.5 hours in: v6 waits for its tick at 24 hours
	tickTime := startTime.Add(24 * time.Hour)
	fixture.step(tickTime.Sub(fixture.clock.Now()) - time.Minute)
	fixture.expectFamilyPostCounts("one minute short of v6's tick", len(holds)+1, 1)
	fixture.clock.advance(time.Minute)
	post := fixture.wakeForPost()
	if post.ipVersion != 6 || post.postTime != tickTime {
		t.Fatalf("tick post = v%d at %s, expected v6 at 24h", post.ipVersion, post.postTime.Sub(startTime))
	}
	fixture.waitActivation()
	fixture.step(time.Minute)
	fixture.expectFamilyPostCounts("after v6's tick", len(holds)+1, 2)
}

// A dual-stack host's hello arrives on either family, so the address check
// sees the activated v4 address one hour and the activated v6 address the
// next. Neither has moved, and nothing is posted.
func TestExtenderActivatorIgnoresAHelloOnTheOtherFamily(t *testing.T) {
	fixture := newTestActivatorFixture(t, nil)
	fixture.waitFirstPass()
	fixture.waitActivation()
	fixture.waitActivation()

	for i := range 6 {
		clientAddress := fmt.Sprintf("%s:%d", testActivateIpv4, 42000+i)
		if i%2 == 0 {
			clientAddress = fmt.Sprintf("[%s]:%d", testActivateIpv6, 42000+i)
		}
		fixture.operator.setClientAddress(clientAddress)
		fixture.step(2 * time.Minute)
	}
	fixture.step(2 * time.Minute)
	fixture.expectFamilyPostCounts("after a hello on each family in turn", 1, 1)
}

// A hello that shows a family's published address has moved posts that family
// alone, and once the operator publishes the new address it changes nothing
// more. The other family's address changes nothing either.
func TestExtenderActivatorPostsTheFamilyWhoseAddressMoved(t *testing.T) {
	fixture := newTestActivatorFixture(t, nil)
	fixture.waitFirstPass()
	fixture.waitActivation()
	fixture.waitActivation()

	const movedIpv4 = "198.51.100.8"
	fixture.operator.setPublishedIp(4, movedIpv4)
	fixture.operator.setClientAddress(movedIpv4 + ":42001")
	fixture.clock.advance(2 * time.Minute)
	if post := fixture.wakeForPost(); post.ipVersion != 4 {
		t.Fatalf("posted v%d for a moved v4 address, expected v4", post.ipVersion)
	}
	if ipVersion := fixture.waitActivation(); ipVersion != 4 {
		t.Fatalf("activated v%d, expected v4", ipVersion)
	}
	fixture.step(2 * time.Minute)
	fixture.step(2 * time.Minute)
	fixture.expectFamilyPostCounts("after the v4 address moved", 2, 1)
	if ip := fixture.activator.Status().Family(4).Ip.String(); ip != movedIpv4 {
		t.Fatalf("v4 ip = %s, expected %s", ip, movedIpv4)
	}

	fixture.operator.setClientAddress(fmt.Sprintf("[%s]:42002", testActivateIpv6))
	fixture.step(2 * time.Minute)
	fixture.operator.setClientAddress(movedIpv4 + ":42003")
	fixture.step(2 * time.Minute)
	fixture.step(2 * time.Minute)
	fixture.expectFamilyPostCounts("after a hello on each published address", 2, 1)

	const movedIpv6 = "2001:db8::8"
	fixture.operator.setPublishedIp(6, movedIpv6)
	fixture.operator.setClientAddress(fmt.Sprintf("[%s]:42004", movedIpv6))
	fixture.clock.advance(2 * time.Minute)
	if post := fixture.wakeForPost(); post.ipVersion != 6 {
		t.Fatalf("posted v%d for a moved v6 address, expected v6", post.ipVersion)
	}
	fixture.waitActivation()
	fixture.step(2 * time.Minute)
	fixture.expectFamilyPostCounts("after the v6 address moved", 2, 2)
}

// A host whose hello leaves from another of its addresses than its activation
// re-posts once, not on every check: the address the last check saw on the
// family is unchanged too.
func TestExtenderActivatorPostsAnotherSourceAddressOnce(t *testing.T) {
	fixture := newTestActivatorFixture(t, nil)
	fixture.waitFirstPass()
	fixture.waitActivation()
	fixture.waitActivation()

	// the operator keeps publishing the activation's own source address
	fixture.operator.setClientAddress("198.51.100.20:42001")
	fixture.clock.advance(2 * time.Minute)
	if post := fixture.wakeForPost(); post.ipVersion != 4 {
		t.Fatalf("posted v%d, expected v4", post.ipVersion)
	}
	fixture.waitActivation()
	for range 4 {
		fixture.step(2 * time.Minute)
	}
	fixture.expectFamilyPostCounts("after hellos from the other source address", 2, 1)
}

// A start and an observed revocation post every family, whatever each one's
// own schedule says: here v4 holding on its backoff and v6 on its tick.
func TestExtenderActivatorPostsEveryFamilyOnRevocation(t *testing.T) {
	fixture := newTestActivatorFixtureWithOperator(t, func(operator *testActivateOperator) {
		operator.setFamilyRefusal(4, testActivateFamilyRefusal)
	}, nil)
	fixture.waitFirstPass()
	fixture.waitActivation()
	fixture.step(5 * time.Minute)
	fixture.expectFamilyPostCounts("between v4's holds", 1, 1)

	revocation := signTestRevocation(
		t, fixture.operator.rootPrivateKey, fixture.publicKey, fixture.clock.Now())
	if _, err := fixture.directory.ApplyRevocation(revocation); err != nil {
		t.Fatal(err)
	}
	revocationTime := fixture.clock.Now()
	for _, ipVersion := range []int{4, 6} {
		post := fixture.waitPost()
		if post.ipVersion != ipVersion || post.postTime != revocationTime {
			t.Fatalf(
				"post after the revocation = v%d at %s, expected v%d at the revocation",
				post.ipVersion, post.postTime, ipVersion)
		}
	}
	if ipVersion := fixture.waitActivation(); ipVersion != 6 {
		t.Fatalf("activated v%d, expected v6", ipVersion)
	}
	if state := fixture.directoryState(testActivateIpv6); state != ExtenderStateActive {
		t.Fatalf("%s state = %q, expected active", testActivateIpv6, state)
	}
}

// A refused family whose host addresses are all private is a host behind a
// NAT, which no backoff changes: it is posted at start and then not until its
// tick, where a public family would have been posted eight times. The family
// with a public address keeps its own tick.
func TestExtenderActivatorHoldsARefusedPrivateFamilyForTheDay(t *testing.T) {
	fixture := newTestActivatorFixtureWithOperator(t, func(operator *testActivateOperator) {
		operator.setFamilyRefusal(4, testActivateFamilyRefusal)
	}, func(settings *ExtenderActivatorSettings) {
		settings.IpVersionPublic = func(ipVersion int) bool { return ipVersion == 6 }
	})
	startTime := fixture.clock.Now()
	fixture.waitFirstPass()
	fixture.waitActivation()

	// the instants a public family's holds would have posted it
	for _, at := range []time.Duration{
		10 * time.Minute,
		30 * time.Minute,
		70 * time.Minute,
		150 * time.Minute,
		310 * time.Minute,
		630 * time.Minute,
		990 * time.Minute,
		1350 * time.Minute,
		24*time.Hour - time.Minute,
	} {
		fixture.step(startTime.Add(at).Sub(fixture.clock.Now()))
		fixture.expectFamilyPostCounts(fmt.Sprintf("at %s", at), 1, 1)
	}

	// the tick posts each family
	fixture.clock.advance(time.Minute)
	tickTime := startTime.Add(24 * time.Hour)
	if post := fixture.wakeForPost(); post.ipVersion != 4 || post.postTime != tickTime {
		t.Fatalf("tick post = v%d at %s, expected v4 at 24h", post.ipVersion, post.postTime.Sub(startTime))
	}
	if post := fixture.waitPost(); post.ipVersion != 6 || post.postTime != tickTime {
		t.Fatalf("tick post = v%d at %s, expected v6 at 24h", post.ipVersion, post.postTime.Sub(startTime))
	}
}

// A family with only private addresses whose router forwards the carrier
// ports activates, and is posted on its tick like any other.
func TestExtenderActivatorKeepsAnAcceptedPrivateFamily(t *testing.T) {
	fixture := newTestActivatorFixture(t, func(settings *ExtenderActivatorSettings) {
		settings.IpVersionPublic = func(ipVersion int) bool { return false }
	})
	startTime := fixture.clock.Now()
	fixture.waitFirstPass()
	fixture.waitActivation()
	fixture.waitActivation()
	status := fixture.activator.Status()
	if !status.Family(4).Activated || !status.Family(6).Activated {
		t.Fatalf("status = %+v, expected both families activated", status.Families)
	}

	fixture.step(24*time.Hour - time.Minute)
	fixture.expectFamilyPostCounts("one minute short of the tick", 1, 1)
	fixture.clock.advance(time.Minute)
	tickTime := startTime.Add(24 * time.Hour)
	for i, post := range []*testActivatePost{fixture.wakeForPost(), fixture.waitPost()} {
		ipVersion := []int{4, 6}[i]
		if post.ipVersion != ipVersion || post.postTime != tickTime {
			t.Fatalf("tick post = v%d at %s, expected v%d at 24h", post.ipVersion, post.postTime.Sub(startTime), ipVersion)
		}
	}
}

// A family the host has no address for is never posted, and is looked at
// again on its own backoff: once the host has one, it is posted at the end of
// that hold rather than at the tick of the family that was posted.
func TestExtenderActivatorPostsAFamilyOnceTheHostHasAnAddress(t *testing.T) {
	var v6Supported atomic.Bool
	fixture := newTestActivatorFixture(t, func(settings *ExtenderActivatorSettings) {
		settings.IpVersionSupported = func(ipVersion int) bool {
			return ipVersion == 4 || v6Supported.Load()
		}
	})
	startTime := fixture.clock.Now()
	fixture.waitPass()
	if post := fixture.waitPost(); post.ipVersion != 4 {
		t.Fatalf("first pass posted v%d, expected v4", post.ipVersion)
	}
	fixture.waitActivation()
	fixture.step(5 * time.Minute)
	fixture.expectFamilyPostCounts("with no v6 address", 1, 0)

	// v6's hold, ten minutes from the pass that skipped it
	v6Supported.Store(true)
	fixture.step(4 * time.Minute)
	fixture.expectFamilyPostCounts("one minute short of v6's hold", 1, 0)
	fixture.clock.advance(time.Minute)
	if post := fixture.wakeForPost(); post.ipVersion != 6 || post.postTime != startTime.Add(10*time.Minute) {
		t.Fatalf("post = v%d at %s, expected v6 at 10m", post.ipVersion, post.postTime.Sub(startTime))
	}
	fixture.waitActivation()
	fixture.step(time.Minute)
	fixture.expectFamilyPostCounts("once the host has a v6 address", 1, 1)
}

// A new identity whose v4 is refused learns its id from the v6 answer, the
// last post of the first pass, so no post carried the measurement request: the
// request follows at once on v6, the family the operator answered, and not on
// the v4 it refused.
func TestExtenderActivatorMeasurementCsrFollowsOnTheActivatedFamily(t *testing.T) {
	hook := &testMeasurementCsrHook{}
	extenderId := NewId()
	fixture := newTestActivatorFixtureWithOperator(t, func(operator *testActivateOperator) {
		operator.setFamilyRefusal(4, testActivateFamilyRefusal)
	}, func(settings *ExtenderActivatorSettings) {
		settings.MeasurementCsr = hook.measurementCsr
		settings.ApiUrlV4 = newTestMeasurementOperator(t, 4, settings.ApiUrlV4, extenderId)
		settings.ApiUrlV6 = newTestMeasurementOperator(t, 6, settings.ApiUrlV6, extenderId)
	})

	fixture.waitPass()
	fixture.expectMeasurementPosts(
		testMeasurementPost{ipVersion: 4},
		testMeasurementPost{ipVersion: 6},
		testMeasurementPost{ipVersion: 6, csr: testMeasurementCsr(extenderId)},
	)
	fixture.expectMeasurementPostCounts(1, 2)
}

// FamilyPublic counts a public address only: what FamilySupported counts, less
// the private ranges (RFC 1918, RFC 4193), carrier-grade NAT (RFC 6598) and
// the 464XLAT range (RFC 7335), which nothing on the internet can dial back.
func TestFamilyPublicReadsTheInterfaceTable(t *testing.T) {
	loopback := controlFamilyInterface{
		name:  "lo0",
		flags: net.FlagUp | net.FlagLoopback,
		addrs: []net.Addr{ipNet("127.0.0.1/8"), ipNet("::1/128")},
	}
	interfaceWith := func(cidrs ...string) controlFamilyInterface {
		addrs := []net.Addr{}
		for _, cidr := range cidrs {
			addrs = append(addrs, ipNet(cidr))
		}
		return controlFamilyInterface{name: "en0", flags: net.FlagUp, addrs: addrs}
	}

	tests := []struct {
		name       string
		ifaces     []controlFamilyInterface
		supported4 bool
		public4    bool
		supported6 bool
		public6    bool
	}{
		{
			// 198.18.0.0/15 and 3fff::/20 are global unicast and unreserved,
			// so they stand in for public addresses without naming a network
			"public addresses of both families",
			[]controlFamilyInterface{loopback, interfaceWith("198.18.7.20/24", "3fff:db8::1/64")},
			true, true, true, true,
		},
		{
			"a home lan lease behind a NAT",
			[]controlFamilyInterface{loopback, interfaceWith("192.168.1.20/24")},
			true, false, false, false,
		},
		{
			"the other RFC 1918 ranges",
			[]controlFamilyInterface{loopback, interfaceWith("10.1.2.3/8", "172.16.5.4/12")},
			true, false, false, false,
		},
		{
			"a carrier-grade NAT address",
			[]controlFamilyInterface{loopback, interfaceWith("100.64.1.2/10")},
			true, false, false, false,
		},
		{
			"just past the carrier-grade NAT range",
			[]controlFamilyInterface{loopback, interfaceWith("100.128.0.1/24")},
			true, true, false, false,
		},
		{
			"a 464XLAT CLAT address",
			[]controlFamilyInterface{loopback, interfaceWith("192.0.0.2/29")},
			true, false, false, false,
		},
		{
			"link-local addresses",
			[]controlFamilyInterface{loopback, interfaceWith("169.254.3.4/16", "fe80::1/64")},
			false, false, false, false,
		},
		{
			"a unique local v6 address",
			[]controlFamilyInterface{loopback, interfaceWith("fd12:3456::1/64")},
			false, false, true, false,
		},
		{
			"a private address beside a public one",
			[]controlFamilyInterface{loopback, interfaceWith("10.0.0.5/24", "fd00::5/64", "198.18.7.20/24", "3fff:db8::1/64")},
			true, true, true, true,
		},
		{
			"a public address on a tunnel",
			[]controlFamilyInterface{loopback, {name: "utun4", flags: net.FlagUp | net.FlagPointToPoint, addrs: []net.Addr{ipNet("198.18.7.20/32")}}},
			false, false, false, false,
		},
		{
			"a public address on an interface that is down",
			[]controlFamilyInterface{loopback, {name: "en1", flags: 0, addrs: []net.Addr{ipNet("198.18.7.20/24")}}},
			false, false, false, false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			restore := swapControlFamilyInterfaces(
				func() ([]controlFamilyInterface, error) { return test.ifaces, nil })
			defer restore()
			if got := FamilySupported(4); got != test.supported4 {
				t.Errorf("FamilySupported(4) = %v, expected %v", got, test.supported4)
			}
			if got := FamilyPublic(4); got != test.public4 {
				t.Errorf("FamilyPublic(4) = %v, expected %v", got, test.public4)
			}
			if got := FamilySupported(6); got != test.supported6 {
				t.Errorf("FamilySupported(6) = %v, expected %v", got, test.supported6)
			}
			if got := FamilyPublic(6); got != test.public6 {
				t.Errorf("FamilyPublic(6) = %v, expected %v", got, test.public6)
			}
		})
	}

	restore := swapControlFamilyInterfaces(
		func() ([]controlFamilyInterface, error) {
			return nil, errors.New("operation not permitted")
		})
	defer restore()
	if FamilyPublic(4) || FamilyPublic(6) {
		t.Fatal("FamilyPublic said yes with no readable interface table")
	}
	for _, ipVersion := range []int{0, 5, -1, 46} {
		if FamilyPublic(ipVersion) {
			t.Fatalf("FamilyPublic(%d) = true, expected false", ipVersion)
		}
	}
}
