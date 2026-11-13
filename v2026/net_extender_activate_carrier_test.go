package connect

import (
	"slices"
	"sync/atomic"
	"testing"
	"time"
)

// An extender posts no activation while no required carrier is serving (EXTENDER.md
// G2, G3, C2a), and a carrier that returns owes the operator only what it held back.
// On 2026-11-01 extenders whose listeners had probably closed kept posting for them.

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

	// the pass that took the return may have read the clock a step on, so this is past
	// the draw from either
	fixture.clock.advance(2 * time.Minute)
	if post := fixture.wakeForPost(); post.ipVersion != 4 {
		t.Fatalf("post = v%d, expected v4", post.ipVersion)
	}
	if post := fixture.waitPost(); post.ipVersion != 6 {
		t.Fatalf("post = v%d, expected v6", post.ipVersion)
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

// A post held back asked nothing of the operator, so what the family last heard stands:
// a tick or a Reactivate that falls within a flap does not read as a family lost, and
// the status gives the reason only while the carrier is gone.
func TestExtenderActivatorHeldPostKeepsAGoodRecord(t *testing.T) {
	fixture := newTestActivatorFixtureActivated(t, func(settings *ExtenderActivatorSettings) {
		settings.postJitterForTest = func(maxJitter time.Duration) time.Duration {
			return 3 * time.Minute
		}
	})
	fixture.setCarriers(ExtenderCarrierQuic)
	fixture.activator.Reactivate()
	fixture.waitHeldForCarrier()
	for _, ipVersion := range []int{4, 6} {
		status := fixture.activator.Status().Family(ipVersion)
		if !status.Activated || !status.Ip.IsValid() {
			t.Fatalf("v%d status = %+v, expected the record to stand", ipVersion, status)
		}
	}

	fixture.setCarriers(ExtenderCarrierTcp, ExtenderCarrierQuic)
	fixture.activator.TcpCarrierReturned()
	fixture.step(time.Minute)
	for _, ipVersion := range []int{4, 6} {
		status := fixture.activator.Status().Family(ipVersion)
		if !status.Activated || status.LastError != "" {
			t.Fatalf("v%d status = %+v, expected it active with the carrier back", ipVersion, status)
		}
	}
	// what was held back is still owed, since a post came due
	fixture.clock.advance(3 * time.Minute)
	if post := fixture.wakeForPost(); post.ipVersion != 4 {
		t.Fatalf("post = v%d after the draw", post.ipVersion)
	}
	fixture.waitPost()
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

// A post that went out with a carrier being rebuilt leaves it off the record, which
// would stay that way until the tick. Its return makes the family due, after a draw.
func TestExtenderActivatorPostsARecordMissingACarrierWhenItReturns(t *testing.T) {
	draws := make(chan time.Duration, 8)
	fixture := newTestActivatorFixtureWithPrepare(t, func(fixture *testActivatorFixture) {
		fixture.carriers = []string{ExtenderCarrierTcp, ExtenderCarrierDns}
	}, func(settings *ExtenderActivatorSettings) {
		settings.postJitterForTest = func(maxJitter time.Duration) time.Duration {
			draws <- maxJitter
			return 3 * time.Minute
		}
	})
	fixture.waitFirstPass()
	fixture.waitActivation()
	fixture.waitActivation()
	fixture.step(time.Minute)

	fixture.setCarriers(ExtenderCarrierTcp, ExtenderCarrierQuic, ExtenderCarrierDns)
	fixture.activator.CarriersChanged()
	fixture.step(time.Minute)
	fixture.step(time.Minute)
	fixture.expectFamilyPostCounts("within the draw", 1, 1)
	if len(draws) != 2 {
		t.Fatalf("%d draws, expected each family's", len(draws))
	}
	for range 2 {
		if maxJitter := <-draws; maxJitter != DefaultExtenderActivatorSettings().CarrierReturnJitter {
			t.Fatalf("drew from %s", maxJitter)
		}
	}

	// the pass that took the change may have read the clock a step on, so this is past
	// the draw from either
	fixture.clock.advance(2 * time.Minute)
	for _, ipVersion := range []int{4, 6} {
		var post *testActivatePost
		if ipVersion == 4 {
			post = fixture.wakeForPost()
		} else {
			post = fixture.waitPost()
		}
		if post.ipVersion != ipVersion || !slices.Contains(post.args.Carriers, ExtenderCarrierQuic) {
			t.Fatalf("post = v%d of %v, expected v%d with the carrier", post.ipVersion, post.args.Carriers, ipVersion)
		}
	}
	fixture.waitActivation()
	fixture.waitActivation()

	// and with the record naming it, the next change costs nothing
	fixture.step(time.Minute)
	fixture.activator.CarriersChanged()
	fixture.step(time.Minute)
	fixture.expectFamilyPostCounts("with nothing missing", 2, 2)
	if 0 < len(draws) {
		t.Fatalf("%d draws, expected none with nothing missing", len(draws))
	}
}

// A dns port is on the record or off it the same way (L2).
func TestExtenderActivatorPostsARecordMissingADnsPortWhenItReturns(t *testing.T) {
	var dnsPorts atomic.Pointer[[]int]
	setDnsPorts := func(ports ...int) {
		dnsPorts.Store(&ports)
	}
	setDnsPorts(4053)
	fixture := newTestActivatorFixtureActivated(t, func(settings *ExtenderActivatorSettings) {
		settings.DnsPorts = func() []int {
			return *dnsPorts.Load()
		}
		settings.CarrierReturnJitter = -1
	})
	fixture.step(time.Minute)

	setDnsPorts(4053, 53)
	fixture.activator.CarriersChanged()
	post := fixture.waitPost()
	if post.ipVersion != 4 || !slices.Contains(post.args.DnsPorts, 53) {
		t.Fatalf("post = v%d of %v, expected v4 with the port", post.ipVersion, post.args.DnsPorts)
	}
	fixture.waitPost()
}

// What is on the record going and coming back posts nothing: this is every flap of a
// udp carrier that had served when the family was posted, so the operator hears of
// none of them.
func TestExtenderActivatorCarrierThatFlapsOnTheRecordPostsNothing(t *testing.T) {
	fixture := newTestActivatorFixtureActivated(t, testActivatorNoReturnJitter)
	for range 5 {
		fixture.setCarriers(ExtenderCarrierTcp, ExtenderCarrierDns)
		fixture.activator.CarriersChanged()
		fixture.step(time.Minute)
		fixture.setCarriers(ExtenderCarrierTcp, ExtenderCarrierQuic, ExtenderCarrierDns)
		fixture.activator.CarriersChanged()
		fixture.step(time.Minute)
	}
	fixture.expectFamilyPostCounts("after the flaps", 1, 1)
}

// Runs a carrier through a return and its loss again, as one that binds and dies at
// once does, past a draw of 3 minutes. The last step is what proves the pass that the
// draw came up in has finished, with the carrier down.
func (self *testActivatorFixture) carrierBindsAndDies(serving []string, down []string) {
	self.t.Helper()
	self.setCarriers(serving...)
	self.activator.CarriersChanged()
	self.step(time.Minute)
	self.setCarriers(down...)
	self.activator.CarriersChanged()
	self.step(time.Minute)
	self.step(3 * time.Minute)
	self.step(time.Minute)
}

// Activated without the quic carrier, and with its post coming up in 3 minutes each
// time it returns.
func newTestActivatorFixtureWithoutQuic(t *testing.T) *testActivatorFixture {
	t.Helper()
	fixture := newTestActivatorFixtureWithPrepare(t, func(fixture *testActivatorFixture) {
		fixture.carriers = []string{ExtenderCarrierTcp, ExtenderCarrierDns}
	}, func(settings *ExtenderActivatorSettings) {
		settings.postJitterForTest = func(maxJitter time.Duration) time.Duration {
			return 3 * time.Minute
		}
	})
	fixture.waitFirstPass()
	fixture.waitActivation()
	fixture.waitActivation()
	fixture.step(time.Minute)
	return fixture
}

// A carrier that binds and dies at once returns over and over, and is gone again when
// the post for it comes due. That post could not name it, so it is not made: a review
// measured 12 posts an hour for each family, indefinitely. The family stays on the tick
// it was on, which still goes.
func TestExtenderActivatorCarrierThatDiesAtOncePostsOnlyOnTheTick(t *testing.T) {
	fixture := newTestActivatorFixtureWithoutQuic(t)
	serving := []string{ExtenderCarrierTcp, ExtenderCarrierQuic, ExtenderCarrierDns}
	down := []string{ExtenderCarrierTcp, ExtenderCarrierDns}
	for range 12 {
		fixture.carrierBindsAndDies(serving, down)
	}
	fixture.expectFamilyPostCounts("after 12 returns", 1, 1)

	// short of the tick, which is a day from the first post
	fixture.step(22 * time.Hour)
	fixture.expectFamilyPostCounts("short of the tick", 1, 1)
	fixture.clock.advance(1 * time.Hour)
	if post := fixture.wakeForPost(); post.ipVersion != 4 || slices.Contains(post.args.Carriers, ExtenderCarrierQuic) {
		t.Fatalf("post = v%d of %v, expected v4's tick without the carrier", post.ipVersion, post.args.Carriers)
	}
	fixture.waitPost()
}

// The same carrier serving when the post comes due is on the record from then, so the
// skip costs nothing but the posts that could not name it, and its later returns post
// nothing.
func TestExtenderActivatorCarrierThatStaysAfterDyingIsPosted(t *testing.T) {
	fixture := newTestActivatorFixtureWithoutQuic(t)
	serving := []string{ExtenderCarrierTcp, ExtenderCarrierQuic, ExtenderCarrierDns}
	down := []string{ExtenderCarrierTcp, ExtenderCarrierDns}
	fixture.carrierBindsAndDies(serving, down)
	fixture.expectFamilyPostCounts("after it died", 1, 1)

	fixture.setCarriers(serving...)
	fixture.activator.CarriersChanged()
	fixture.step(time.Minute)
	fixture.clock.advance(3 * time.Minute)
	if post := fixture.wakeForPost(); post.ipVersion != 4 || !slices.Contains(post.args.Carriers, ExtenderCarrierQuic) {
		t.Fatalf("post = v%d of %v, expected v4 with the carrier", post.ipVersion, post.args.Carriers)
	}
	fixture.waitPost()
	fixture.waitActivation()
	fixture.waitActivation()

	fixture.step(time.Minute)
	fixture.carrierBindsAndDies(serving, down)
	fixture.expectFamilyPostCounts("with it on the record", 2, 2)
}

// A trigger that moved the post is not the carrier's, so the carrier being down again
// does not cancel it: a network change is owed its post whatever else is serving.
func TestExtenderActivatorCarrierSkipLeavesAnotherTriggersPost(t *testing.T) {
	fixture := newTestActivatorFixtureWithoutQuic(t)
	fixture.setCarriers(ExtenderCarrierTcp, ExtenderCarrierQuic, ExtenderCarrierDns)
	fixture.activator.CarriersChanged()
	fixture.step(time.Minute)
	fixture.setCarriers(ExtenderCarrierTcp, ExtenderCarrierDns)
	fixture.activator.Reactivate()
	fixture.waitPost()
	fixture.waitPost()
}

// A family on a hold posts at its end with what is serving then, so a carrier's return
// is no reason to cut that short.
func TestExtenderActivatorCarrierReturnKeepsAHold(t *testing.T) {
	fixture := newTestActivatorFixtureWithPrepare(t, func(fixture *testActivatorFixture) {
		fixture.carriers = []string{ExtenderCarrierTcp}
		fixture.operator.setRefusal(testActivateFamilyRefusal)
		fixture.operator.setRetryAfterSeconds(50 * 60)
	}, testActivatorNoReturnJitter)
	fixture.waitFirstPass()
	fixture.step(time.Minute)

	fixture.setCarriers(ExtenderCarrierTcp, ExtenderCarrierQuic)
	fixture.activator.CarriersChanged()
	fixture.step(time.Minute)
	fixture.expectFamilyPostCounts("within the wait", 1, 1)
}

// Closes the activator and starts the one that replaces it on the same settings, as
// a role being replaced does.
func (self *testActivatorFixture) replaceActivator(configure func(settings *ExtenderActivatorSettings)) {
	self.t.Helper()
	self.activator.Close()
	settings := *self.activatorSettings
	settings.PreviousState = self.activator.State()
	if configure != nil {
		configure(&settings)
	}
	self.activator = NewExtenderActivator(self.activatorCtx, &settings)
}

// Replacing a role is not a restart of the host. A family that stands activated
// stays activated, in its status from the start, and is posted once more within the
// spread, since a new role needs what a post gives it. Then it is back on its tick.
func TestExtenderActivatorReplacementKeepsARecordAndSpreadsThePost(t *testing.T) {
	draws := make(chan time.Duration, 8)
	fixture := newTestActivatorFixtureActivated(t, nil)
	fixture.step(time.Minute)
	fixture.replaceActivator(func(settings *ExtenderActivatorSettings) {
		settings.postJitterForTest = func(maxJitter time.Duration) time.Duration {
			draws <- maxJitter
			return 30 * time.Minute
		}
	})
	for _, ipVersion := range []int{4, 6} {
		status := fixture.activator.Status().Family(ipVersion)
		if !status.Activated || !status.Ip.IsValid() {
			t.Fatalf("v%d status = %+v, expected it carried over", ipVersion, status)
		}
		if maxJitter := <-draws; maxJitter != DefaultExtenderActivatorSettings().ReplacedPostJitter {
			t.Fatalf("drew from %s", maxJitter)
		}
	}

	fixture.step(29 * time.Minute)
	fixture.expectFamilyPostCounts("within the draw", 1, 1)

	fixture.clock.advance(time.Minute)
	if post := fixture.wakeForPost(); post.ipVersion != 4 {
		t.Fatalf("post = v%d after the draw", post.ipVersion)
	}
	fixture.waitPost()
	fixture.step(23 * time.Hour)
	fixture.expectFamilyPostCounts("on the tick from there", 2, 2)
}

// A family's tick stands if it comes before the draw.
func TestExtenderActivatorReplacementPostsNoLaterThanTheTick(t *testing.T) {
	fixture := newTestActivatorFixtureActivated(t, nil)
	fixture.step(23*time.Hour + 30*time.Minute)
	fixture.replaceActivator(func(settings *ExtenderActivatorSettings) {
		settings.postJitterForTest = func(maxJitter time.Duration) time.Duration {
			return 59 * time.Minute
		}
	})
	fixture.step(time.Minute)
	fixture.expectFamilyPostCounts("short of the tick", 1, 1)
	fixture.clock.advance(29 * time.Minute)
	if post := fixture.wakeForPost(); post.ipVersion != 4 {
		t.Fatalf("post = v%d at the tick", post.ipVersion)
	}
	fixture.waitPost()
}

// A family waiting out what the operator said goes on waiting: replacing the role
// used to post at once.
func TestExtenderActivatorReplacementKeepsARetryAfter(t *testing.T) {
	fixture := newTestActivatorFixtureWithOperator(t, func(operator *testActivateOperator) {
		operator.setRefusal(testActivateFamilyRefusal)
		operator.setRetryAfterSeconds(50 * 60)
	}, nil)
	fixture.waitFirstPass()
	fixture.step(time.Minute)
	fixture.replaceActivator(nil)

	fixture.step(time.Minute)
	fixture.step(time.Minute)
	fixture.expectFamilyPostCounts("after the replacement", 1, 1)
	if status := fixture.activator.Status().Family(4); !status.LastRefused || status.LastError != testActivateFamilyRefusal {
		t.Fatalf("v4 status = %+v, expected the refusal carried over", status)
	}

	fixture.clock.advance(47 * time.Minute)
	if post := fixture.wakeForPost(); post.ipVersion != 4 {
		t.Fatalf("post = v%d after the wait", post.ipVersion)
	}
	fixture.waitPost()
}

// So does a family on a hold that has doubled, and the next refusal doubles it again
// rather than starting over from the floor.
func TestExtenderActivatorReplacementKeepsAHold(t *testing.T) {
	fixture := newTestActivatorFixtureWithOperator(t, func(operator *testActivateOperator) {
		operator.setFamilyRefusal(4, testActivateFamilyRefusal)
	}, nil)
	fixture.waitFirstPass()
	fixture.waitActivation()
	fixture.step(time.Minute)
	fixture.expectFamilyPostCounts("after start", 1, 1)

	// the second refusal holds for 20 minutes, of which this replacement leaves 19
	fixture.clock.advance(9 * time.Minute)
	if post := fixture.wakeForPost(); post.ipVersion != 4 {
		t.Fatalf("post = v%d after the hold", post.ipVersion)
	}
	fixture.step(time.Minute)
	fixture.expectFamilyPostCounts("after the second refusal", 2, 1)
	// v6 stands activated, and its post is not what this is about
	fixture.replaceActivator(func(settings *ExtenderActivatorSettings) {
		settings.ReplacedPostJitter = 24 * 365 * time.Hour
	})
	fixture.step(17 * time.Minute)
	fixture.expectFamilyPostCounts("within the hold", 2, 1)

	fixture.clock.advance(2 * time.Minute)
	if post := fixture.wakeForPost(); post.ipVersion != 4 {
		t.Fatalf("post = v%d after the hold", post.ipVersion)
	}
	// and the hold after that is 40 minutes, not 10
	fixture.step(time.Minute)
	fixture.step(30 * time.Minute)
	fixture.expectFamilyPostCounts("within the next hold", 3, 1)
}

// What the gate held back is owed by the replacement, which is usually what brought
// the port back, spread as a return's is.
func TestExtenderActivatorReplacementPostsWhatTheGateHeldBack(t *testing.T) {
	fixture := newTestActivatorFixtureActivated(t, nil)
	fixture.setCarriers(ExtenderCarrierQuic)
	fixture.revoke()
	fixture.waitHeldForCarrier()
	fixture.step(time.Minute)
	fixture.expectFamilyPostCounts("with the listener gone", 1, 1)

	fixture.setCarriers(ExtenderCarrierTcp, ExtenderCarrierQuic)
	fixture.replaceActivator(func(settings *ExtenderActivatorSettings) {
		settings.postJitterForTest = func(maxJitter time.Duration) time.Duration {
			if maxJitter != settings.CarrierReturnJitter {
				t.Errorf("drew from %s", maxJitter)
			}
			return 3 * time.Minute
		}
	})
	if status := fixture.activator.Status().Family(4); status.LastError != "" {
		t.Fatalf("v4 status = %+v, expected the old role's carrier no longer given as the reason", status)
	}
	fixture.step(time.Minute)
	fixture.expectFamilyPostCounts("within the draw", 1, 1)
	fixture.clock.advance(2 * time.Minute)
	if post := fixture.wakeForPost(); post.ipVersion != 4 {
		t.Fatalf("post = v%d after the draw", post.ipVersion)
	}
	fixture.waitPost()
}

// A post the close of a replaced role interrupted says nothing of the operator: it
// neither holds nor reads as a failure, and the next role still owes it.
func TestExtenderActivatorReplacementStillOwesAnInterruptedPost(t *testing.T) {
	fixture := newTestActivatorFixtureActivated(t, nil)
	fixture.step(time.Minute)
	release := make(chan struct{})
	defer close(release)
	fixture.operator.setAnswerHold(release)
	fixture.activator.Reactivate()
	fixture.waitPost()

	fixture.replaceActivator(func(settings *ExtenderActivatorSettings) {
		settings.postJitterForTest = func(maxJitter time.Duration) time.Duration {
			if maxJitter != settings.CarrierReturnJitter {
				t.Errorf("drew from %s", maxJitter)
			}
			return 3 * time.Minute
		}
	})
	fixture.operator.setAnswerHold(nil)
	for _, ipVersion := range []int{4, 6} {
		status := fixture.activator.Status().Family(ipVersion)
		if !status.Activated || status.LastError != "" {
			t.Fatalf("v%d status = %+v, expected the interrupted post to say nothing", ipVersion, status)
		}
	}
	fixture.step(time.Minute)
	fixture.expectFamilyPostCounts("within the draw", 2, 1)
	fixture.clock.advance(2 * time.Minute)
	if post := fixture.wakeForPost(); post.ipVersion != 4 {
		t.Fatalf("post = v%d after the draw", post.ipVersion)
	}
	fixture.waitPost()
}

// A revocation the old activator already answered is not news to the new one, which
// would post every family at once for it.
func TestExtenderActivatorReplacementDoesNotSeeARevocationAgain(t *testing.T) {
	fixture := newTestActivatorFixtureActivated(t, nil)
	fixture.step(time.Minute)
	fixture.operator.setRefusal(testActivateFamilyRefusal)
	fixture.operator.setRetryAfterSeconds(50 * 60)
	fixture.revoke()
	// the edge posts every family at once
	fixture.waitPost()
	fixture.waitPost()
	fixture.waitFamilies("refused", func(family *ExtenderFamilyActivationStatus) bool {
		return family.LastRefused
	})
	fixture.step(time.Minute)
	fixture.step(time.Minute)
	fixture.expectFamilyPostCounts("after the revocation", 2, 2)

	fixture.replaceActivator(nil)
	fixture.step(time.Minute)
	fixture.expectFamilyPostCounts("after the replacement", 2, 2)
	if status := fixture.activator.Status().Family(4); status.RevokedTime.IsZero() {
		t.Fatalf("v4 status = %+v, expected the revocation carried over", status)
	}
}

// A trigger that landed just ahead of the close, which no pass acted on, is still owed:
// a network change would otherwise wait for the post the replacement spreads over an
// hour, and a carrier's return for the tick.
func TestExtenderActivatorReplacementKeepsATriggerNotYetActedOn(t *testing.T) {
	t.Run("a network change", func(t *testing.T) {
		fixture := newTestActivatorFixtureActivated(t, func(settings *ExtenderActivatorSettings) {
			settings.ReplacedPostJitter = 24 * 365 * time.Hour
		})
		fixture.step(time.Minute)
		fixture.activator.Close()
		fixture.activator.Reactivate()
		settings := *fixture.activatorSettings
		settings.PreviousState = fixture.activator.State()
		fixture.activator = NewExtenderActivator(fixture.activatorCtx, &settings)
		fixture.waitPost()
		fixture.waitPost()
	})
	t.Run("a carrier's return", func(t *testing.T) {
		fixture := newTestActivatorFixtureWithPrepare(t, func(fixture *testActivatorFixture) {
			fixture.carriers = []string{ExtenderCarrierTcp}
		}, func(settings *ExtenderActivatorSettings) {
			settings.CarrierReturnJitter = -1
			settings.ReplacedPostJitter = 24 * 365 * time.Hour
		})
		fixture.waitFirstPass()
		fixture.waitActivation()
		fixture.waitActivation()
		fixture.step(time.Minute)
		fixture.activator.Close()
		fixture.setCarriers(ExtenderCarrierTcp, ExtenderCarrierQuic)
		fixture.activator.CarriersChanged()
		settings := *fixture.activatorSettings
		settings.PreviousState = fixture.activator.State()
		fixture.activator = NewExtenderActivator(fixture.activatorCtx, &settings)
		fixture.waitPost()
		fixture.waitPost()
	})
}

// What another identity's activation held says nothing of this one: it starts as a
// new process does.
func TestExtenderActivatorReplacementOfAnotherIdentityStartsDue(t *testing.T) {
	fixture := newTestActivatorFixtureActivated(t, nil)
	fixture.step(time.Minute)
	fixture.activator.Close()
	state := fixture.activator.State()
	state.publicKey = []byte("another identity")
	settings := *fixture.activatorSettings
	settings.PreviousState = state
	fixture.activator = NewExtenderActivator(fixture.activatorCtx, &settings)
	if status := fixture.activator.Status().Family(4); status.Activated {
		t.Fatalf("v4 status = %+v, expected nothing carried over", status)
	}
	fixture.waitPass()
	fixture.waitPost()
	fixture.waitPost()
}

// An activator with nothing handed over starts every family due, as a new process
// does, so a host that restarted is posted at once.
func TestExtenderActivatorWithNoPreviousStateStartsDue(t *testing.T) {
	fixture := newTestActivatorFixtureActivated(t, nil)
	fixture.step(time.Minute)
	fixture.activator.Close()
	settings := *fixture.activatorSettings
	fixture.activator = NewExtenderActivator(fixture.activatorCtx, &settings)
	fixture.waitPass()
	fixture.waitPost()
	fixture.waitPost()
}

// Root cause: an operator that predates the webrtc carrier refuses a list
// naming it as an unknown carrier, so an extender serving webrtc beside tcp
// would be refused there for good (S8), the miner's other operators among
// them. Observable: the refused post goes again at once without the carrier
// and activates, in the same pass, and no later post names the carrier.
func TestExtenderActivatorPostsWithoutWebRtcToAnOperatorThatPredatesIt(t *testing.T) {
	fixture := newTestActivatorFixtureWithPrepare(t, func(fixture *testActivatorFixture) {
		fixture.carriers = []string{ExtenderCarrierTcp, ExtenderCarrierQuic, ExtenderCarrierWebRtc}
		fixture.operator.predatesWebRtc = true
	}, nil)
	fixture.waitPass()
	for _, ipVersion := range []int{4, 6} {
		refused := fixture.waitPost()
		if refused.ipVersion != ipVersion || !slices.Contains(refused.args.Carriers, ExtenderCarrierWebRtc) {
			t.Fatalf("post = v%d %v, expected v%d with webrtc first", refused.ipVersion, refused.args.Carriers, ipVersion)
		}
		again := fixture.waitPost()
		if again.ipVersion != ipVersion || !slices.Equal(again.args.Carriers, []string{ExtenderCarrierTcp, ExtenderCarrierQuic}) {
			t.Fatalf("post = v%d %v, expected v%d again without webrtc", again.ipVersion, again.args.Carriers, ipVersion)
		}
		if refused.postTime != again.postTime {
			t.Fatalf("the post without webrtc went at %s, expected at once at %s", again.postTime, refused.postTime)
		}
		fixture.waitActivation()
	}

	fixture.activator.Reactivate()
	for range 2 {
		if post := fixture.waitPost(); slices.Contains(post.args.Carriers, ExtenderCarrierWebRtc) {
			t.Fatalf("a later post named webrtc again: %v", post.args.Carriers)
		}
	}
}

// A host serving webrtc and no tcp has no carrier such an operator accepts,
// so after the refusal it is held for its carriers, as one without tcp always
// was, and posts nothing more.
func TestExtenderActivatorHoldsAWebRtcOnlyHostForAnOperatorThatPredatesIt(t *testing.T) {
	fixture := newTestActivatorFixtureWithPrepare(t, func(fixture *testActivatorFixture) {
		fixture.carriers = []string{ExtenderCarrierQuic, ExtenderCarrierWebRtc}
		fixture.operator.predatesWebRtc = true
	}, nil)
	fixture.waitHeldForCarrier()
	fixture.expectFamilyPostCounts("after the refusal", 1, 1)
	fixture.step(2 * time.Minute)
	fixture.step(30 * time.Minute)
	fixture.expectFamilyPostCounts("held for the carrier", 1, 1)
}
