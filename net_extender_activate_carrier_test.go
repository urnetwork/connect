package connect

import (
	"slices"
	"testing"
	"time"
)

// An extender posts no activation while no required carrier is serving (EXTENDER.md
// G2, G3, C2a), and a carrier that returns owes the operator only what it held back.
// On 2026-11-01 extenders whose listeners had probably closed kept posting: each post
// cost the operator a probe of a closed port, and spent the hourly budget their
// account's other extenders share.

func (self *testActivatorFixture) setCarriers(carriers ...string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.carriers = slices.Clone(carriers)
}

// Revokes this extender's key, and waits for the pass that sees it: the directory
// applying it is what wakes the loop. The clock moves first, since a revocation no
// newer than the record it names is not one.
func (self *testActivatorFixture) revoke() {
	self.t.Helper()
	self.clock.advance(time.Minute)
	revocation := signTestRevocation(
		self.t, self.operator.rootPrivateKey, self.publicKey, self.clock.Now())
	if _, err := self.directory.ApplyRevocation(revocation); err != nil {
		self.t.Fatal(err)
	}
	self.waitFamilies("the revocation", func(family *ExtenderFamilyActivationStatus) bool {
		return !family.RevokedTime.IsZero()
	})
}

// Waits for a pass to hold every family back for its carriers, which it says last, so
// the carriers the pass read no longer matter. It takes that pass's mark, so the next
// step's barrier is the next pass's own.
func (self *testActivatorFixture) waitHeldForCarrier() {
	self.t.Helper()
	self.waitFamilies("held for the carrier", func(family *ExtenderFamilyActivationStatus) bool {
		return extenderActivateCarrierFailureMessage(family.LastError)
	})
	select {
	case <-self.operator.hellos:
	default:
	}
}

// Waits for every family to stand as `accept` says.
func (self *testActivatorFixture) waitFamilies(
	what string,
	accept func(family *ExtenderFamilyActivationStatus) bool,
) {
	self.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		families := self.activator.Status().Families
		if 0 < len(families) && !slices.ContainsFunc(families, func(family *ExtenderFamilyActivationStatus) bool {
			return !accept(family)
		}) {
			return
		}
		if deadline.Before(time.Now()) {
			self.t.Fatalf("%s: families = %+v", what, families)
		}
		time.Sleep(time.Millisecond)
	}
}

// Takes the tcp carrier away and brings it back, as a listener that flaps does.
func (self *testActivatorFixture) flapTcpCarrier() {
	self.t.Helper()
	self.setCarriers(ExtenderCarrierQuic)
	self.step(time.Minute)
	self.setCarriers(ExtenderCarrierTcp, ExtenderCarrierQuic)
	self.activator.TcpCarrierReturned()
	self.step(time.Minute)
}

// The post a return makes goes at once, so a test can pin when it goes.
func testActivatorNoReturnJitter(settings *ExtenderActivatorSettings) {
	settings.CarrierReturnJitter = -1
}

// Waits for an activator that activated both families at start.
func newTestActivatorFixtureActivated(
	t *testing.T,
	configure func(settings *ExtenderActivatorSettings),
) *testActivatorFixture {
	t.Helper()
	fixture := newTestActivatorFixture(t, configure)
	fixture.waitFirstPass()
	for range 2 {
		fixture.waitActivation()
	}
	return fixture
}

// A list without tcp cannot be offered, whatever else is serving: the operator
// refuses it before it probes anything, so the post could only be refused.
func TestExtenderActivatorHoldsWithoutTheTcpCarrier(t *testing.T) {
	fixture := newTestActivatorFixtureWithPrepare(t, func(fixture *testActivatorFixture) {
		fixture.carriers = []string{ExtenderCarrierQuic, ExtenderCarrierDns}
	}, nil)
	fixture.waitPass()
	fixture.step(2 * time.Minute)
	fixture.step(30 * time.Minute)
	fixture.expectFamilyPostCounts("with no tcp carrier", 0, 0)
	for _, ipVersion := range []int{4, 6} {
		status := fixture.activator.Status().Family(ipVersion)
		if status.Activated || status.LastRefused || status.LastError != "the tcp carrier is not listening" {
			t.Fatalf("v%d status = %+v", ipVersion, status)
		}
	}
}

// The webrtc carrier is the other path the operator can accept an activation on
// (C2a), so a list with it and no tcp is posted, as the operator decides.
func TestExtenderActivatorPostsTheWebRtcCarrierWithoutTcp(t *testing.T) {
	fixture := newTestActivatorFixtureWithPrepare(t, func(fixture *testActivatorFixture) {
		fixture.carriers = []string{ExtenderCarrierQuic, ExtenderCarrierWebRtc}
	}, nil)
	fixture.waitFirstPass()
}

// A revocation that arrives while the listener is gone posts nothing, however long
// it is gone. When the carrier returns, the posts it held back go.
func TestExtenderActivatorPostsAfterTheTcpCarrierReturns(t *testing.T) {
	fixture := newTestActivatorFixtureActivated(t, testActivatorNoReturnJitter)
	fixture.expectFamilyPostCounts("after start", 1, 1)

	fixture.setCarriers(ExtenderCarrierQuic)
	fixture.revoke()
	fixture.waitHeldForCarrier()
	fixture.step(time.Minute)
	for range 6 {
		fixture.step(6 * time.Hour)
	}
	fixture.expectFamilyPostCounts("with the listener gone", 1, 1)
	status := fixture.activator.Status().Family(4)
	if status.Activated || status.LastError != "the tcp carrier is not listening" {
		t.Fatalf("v4 status = %+v", status)
	}

	fixture.setCarriers(ExtenderCarrierTcp, ExtenderCarrierQuic)
	returnTime := fixture.clock.Now()
	fixture.activator.TcpCarrierReturned()
	for _, ipVersion := range []int{4, 6} {
		post := fixture.waitPost()
		if post.ipVersion != ipVersion || post.postTime != returnTime {
			t.Fatalf(
				"post = v%d at %s, expected v%d at the carrier's return at %s",
				post.ipVersion, post.postTime, ipVersion, returnTime)
		}
		fixture.waitActivation()
	}
}

// A fleet that lost its listeners together does not post together: each family's
// post goes after its own draw. The status stops giving the carrier as the reason
// when the carrier returns, rather than when the draw comes up.
func TestExtenderActivatorSpreadsThePostsOfAReturnedTcpCarrier(t *testing.T) {
	draws := make(chan time.Duration, 8)
	fixture := newTestActivatorFixtureActivated(t, func(settings *ExtenderActivatorSettings) {
		settings.postJitterForTest = func(maxJitter time.Duration) time.Duration {
			draws <- maxJitter
			return 3 * time.Minute
		}
	})
	fixture.setCarriers(ExtenderCarrierQuic)
	fixture.revoke()
	fixture.waitHeldForCarrier()
	fixture.step(time.Minute)
	fixture.expectFamilyPostCounts("with the listener gone", 1, 1)

	fixture.setCarriers(ExtenderCarrierTcp, ExtenderCarrierQuic)
	fixture.activator.TcpCarrierReturned()
	fixture.step(time.Minute)
	fixture.step(time.Minute)
	fixture.expectFamilyPostCounts("within the draw", 1, 1)
	if len(draws) != 2 {
		t.Fatalf("%d draws, expected each family's", len(draws))
	}
	for _, ipVersion := range []int{4, 6} {
		if maxJitter := <-draws; maxJitter != DefaultExtenderActivatorSettings().CarrierReturnJitter {
			t.Fatalf("drew from %s", maxJitter)
		}
		if status := fixture.activator.Status().Family(ipVersion); status.LastError != "" {
			t.Fatalf("v%d status = %+v, expected the carrier no longer given as the reason", ipVersion, status)
		}
	}

	fixture.step(time.Minute)
	for _, ipVersion := range []int{4, 6} {
		if post := fixture.waitPost(); post.ipVersion != ipVersion {
			t.Fatalf("post = v%d, expected v%d", post.ipVersion, ipVersion)
		}
	}
}

// A carrier that went and came back with nothing owed posts nothing, so a listener
// that flaps does not spend the operator's budgets: the record it holds is still
// good, and the probes never got as far as revoking it.
func TestExtenderActivatorPostsNothingIfTheTcpCarrierReturnsWithNothingOwed(t *testing.T) {
	fixture := newTestActivatorFixtureActivated(t, testActivatorNoReturnJitter)
	for range 3 {
		fixture.flapTcpCarrier()
	}
	fixture.expectFamilyPostCounts("after the flaps", 1, 1)
	for _, ipVersion := range []int{4, 6} {
		if status := fixture.activator.Status().Family(ipVersion); !status.Activated {
			t.Fatalf("v%d status = %+v", ipVersion, status)
		}
	}
}

// The return is for a family whose post came due while the carrier was gone. A
// family on a hold that has not run out is owed nothing, whatever its status.
func TestExtenderActivatorTcpCarrierReturnPostsOnlyTheFamilyOwed(t *testing.T) {
	fixture := newTestActivatorFixtureWithOperator(t, func(operator *testActivateOperator) {
		operator.setFamilyRefusal(4, testActivateFamilyRefusal)
	}, testActivatorNoReturnJitter)
	fixture.waitFirstPass()
	fixture.waitActivation()
	fixture.step(time.Minute)
	fixture.expectFamilyPostCounts("after start", 1, 1)

	// a flap inside v4's hold posts nothing
	fixture.setCarriers(ExtenderCarrierQuic)
	fixture.setCarriers(ExtenderCarrierTcp, ExtenderCarrierQuic)
	fixture.activator.TcpCarrierReturned()
	fixture.step(time.Minute)
	fixture.expectFamilyPostCounts("after a flap inside the hold", 1, 1)

	// v4 comes due with the carrier gone: nothing is posted, and its return owes it
	fixture.operator.setFamilyRefusal(4, "")
	fixture.setCarriers(ExtenderCarrierQuic)
	fixture.step(10 * time.Minute)
	fixture.step(time.Minute)
	fixture.expectFamilyPostCounts("with the carrier gone", 1, 1)
	fixture.setCarriers(ExtenderCarrierTcp, ExtenderCarrierQuic)
	returnTime := fixture.clock.Now()
	fixture.activator.TcpCarrierReturned()
	post := fixture.waitPost()
	if post.ipVersion != 4 || post.postTime != returnTime {
		t.Fatalf("post = v%d at %s, expected v4 at %s", post.ipVersion, post.postTime, returnTime)
	}
	fixture.waitActivation()
	fixture.step(time.Minute)
	fixture.expectFamilyPostCounts("after the return", 2, 1)
}

// A listener that flaps does not post past what the operator said: a review
// measured 7 posts in 4 minutes against a retry_after_seconds of 50 minutes, each
// return making the family due again.
func TestExtenderActivatorFlappingTcpCarrierKeepsARetryAfter(t *testing.T) {
	fixture := newTestActivatorFixtureWithOperator(t, func(operator *testActivateOperator) {
		operator.setRefusal(testActivateFamilyRefusal)
		operator.setRetryAfterSeconds(50 * 60)
	}, testActivatorNoReturnJitter)
	fixture.waitFirstPass()
	fixture.step(time.Minute)
	fixture.expectFamilyPostCounts("after the refusals", 1, 1)

	for range 7 {
		fixture.flapTcpCarrier()
	}
	fixture.expectFamilyPostCounts("after 7 flaps", 1, 1)

	// what the operator named still ends when it said
	fixture.operator.setRefusal("")
	fixture.operator.setRetryAfterSeconds(0)
	fixture.clock.advance(35 * time.Minute)
	if post := fixture.wakeForPost(); post.ipVersion != 4 {
		t.Fatalf("post = v%d after the wait", post.ipVersion)
	}
}

// Nor past a family's hold, which a return is no reason to cut short.
func TestExtenderActivatorFlappingTcpCarrierKeepsAHold(t *testing.T) {
	fixture := newTestActivatorFixtureWithOperator(t, func(operator *testActivateOperator) {
		operator.setFamilyRefusal(4, testActivateFamilyRefusal)
	}, testActivatorNoReturnJitter)
	fixture.waitFirstPass()
	fixture.waitActivation()
	fixture.step(time.Minute)
	fixture.expectFamilyPostCounts("after start", 1, 1)

	// v4 holds for 10 minutes, and these 4 do not reach them
	fixture.flapTcpCarrier()
	fixture.flapTcpCarrier()
	fixture.expectFamilyPostCounts("after the flaps", 1, 1)

	fixture.operator.setFamilyRefusal(4, "")
	fixture.clock.advance(6 * time.Minute)
	if post := fixture.wakeForPost(); post.ipVersion != 4 {
		t.Fatalf("post = v%d after the hold", post.ipVersion)
	}
	fixture.waitActivation()
	fixture.step(time.Minute)
	fixture.expectFamilyPostCounts("after the hold", 2, 1)
}

// Nor past a private family's day (G3): a NAT that never forwarded refuses every
// retry alike, and a flap changes nothing about that.
func TestExtenderActivatorFlappingTcpCarrierKeepsAPrivateFamilysDay(t *testing.T) {
	fixture := newTestActivatorFixtureWithOperator(t, func(operator *testActivateOperator) {
		operator.setFamilyRefusal(4, testActivateFamilyRefusal)
	}, func(settings *ExtenderActivatorSettings) {
		settings.CarrierReturnJitter = -1
		settings.IpVersionPublic = func(ipVersion int) bool { return ipVersion != 4 }
	})
	fixture.waitFirstPass()
	fixture.waitActivation()
	fixture.step(time.Minute)
	for range 10 {
		fixture.flapTcpCarrier()
	}
	fixture.expectFamilyPostCounts("after the flaps", 1, 1)
}

// The look for a carrier is local, as the look for an address is, so what it finds
// leaves the hold where it was.
func TestExtenderActivatorCarrierHoldDoesNotDouble(t *testing.T) {
	passHolds := newTestActivatePassHolds()
	fixture := newTestActivatorFixtureWithPrepare(t, func(fixture *testActivatorFixture) {
		fixture.carriers = []string{ExtenderCarrierQuic}
		fixture.operator.setRefusal("the forward did not answer")
	}, func(settings *ExtenderActivatorSettings) {
		settings.ApiUrlV6 = ""
		settings.CarrierReturnJitter = -1
		passHolds.configure(settings)
	})
	for range 8 {
		if hold := passHolds.next(t, "held"); hold != 10*time.Minute {
			t.Fatalf("held for %s, expected the floor", hold)
		}
		fixture.clock.advance(10 * time.Minute)
		fixture.wake()
	}
	passHolds.next(t, "held")
	fixture.setCarriers(ExtenderCarrierTcp, ExtenderCarrierQuic)
	fixture.activator.TcpCarrierReturned()
	fixture.waitPost()
	if hold := passHolds.next(t, "refused"); hold != 10*time.Minute {
		t.Fatalf("the first refusal holds %s, expected the floor", hold)
	}
}

// An owner that never says the carrier returned, as a webrtc carrier's does not,
// still has what was held back posted within the floor: the gate looks again, and
// the look is local.
func TestExtenderActivatorFindsAReturnedCarrierByItself(t *testing.T) {
	fixture := newTestActivatorFixtureActivated(t, nil)
	fixture.setCarriers(ExtenderCarrierQuic)
	fixture.revoke()
	fixture.waitHeldForCarrier()
	fixture.step(time.Minute)
	fixture.step(5 * time.Hour)
	fixture.expectFamilyPostCounts("with the listener gone", 1, 1)

	fixture.setCarriers(ExtenderCarrierTcp, ExtenderCarrierQuic)
	fixture.clock.advance(10 * time.Minute)
	if post := fixture.wakeForPost(); post.ipVersion != 4 {
		t.Fatalf("post = v%d after the floor", post.ipVersion)
	}
	fixture.waitPost()
}

// The one schedule of the plain api url follows the same rule.
func TestExtenderActivatorTcpCarrierReturnWithThePlainApiUrl(t *testing.T) {
	fixture := newTestActivatorFixture(t, func(settings *ExtenderActivatorSettings) {
		settings.ApiUrl = settings.ApiUrlV4
		settings.ApiUrlV4 = ""
		settings.ApiUrlV6 = ""
		settings.CarrierReturnJitter = -1
	})
	fixture.waitPass()
	fixture.waitPost()
	fixture.waitActivation()

	// nothing owed
	fixture.activator.TcpCarrierReturned()
	fixture.step(time.Minute)
	if count := fixture.operator.postCount(4); count != 1 {
		t.Fatalf("posts = %d with nothing owed", count)
	}

	fixture.setCarriers(ExtenderCarrierQuic)
	fixture.revoke()
	fixture.waitHeldForCarrier()
	fixture.step(time.Minute)
	fixture.step(time.Minute)
	if count := fixture.operator.postCount(4); count != 1 {
		t.Fatalf("posts = %d with the listener gone", count)
	}

	fixture.setCarriers(ExtenderCarrierTcp, ExtenderCarrierQuic)
	fixture.activator.TcpCarrierReturned()
	fixture.waitPost()
	fixture.waitActivation()
}
