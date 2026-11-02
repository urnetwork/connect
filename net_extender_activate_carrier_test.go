package connect

import (
	"slices"
	"testing"
	"time"
)

// An extender posts no activation while its tcp carrier is not serving, the one
// the operator requires (EXTENDER.md G2, G3, C2). On 2026-11-01 extenders whose
// listeners had closed kept posting: each post cost the operator a probe of a
// closed port, and spent the hourly budget their account's other extenders share.

func (self *testActivatorFixture) setCarriers(carriers ...string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.carriers = slices.Clone(carriers)
}

// Waits for the pass a revocation of this extender's key starts: the directory
// applying it is what wakes the loop.
func (self *testActivatorFixture) revoke() {
	self.t.Helper()
	revocation := signTestRevocation(
		self.t, self.operator.rootPrivateKey, self.publicKey, self.clock.Now())
	if _, err := self.directory.ApplyRevocation(revocation); err != nil {
		self.t.Fatal(err)
	}
}

// Waits for an activator that activated both families at start.
func newTestActivatorFixtureActivated(t *testing.T) *testActivatorFixture {
	t.Helper()
	fixture := newTestActivatorFixture(t, nil)
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

// A revocation that arrives while the listener is gone posts nothing, and neither
// does the hold it falls on. The moment the carrier returns, every family is
// posted, without waiting out the holds it built up in the meantime.
func TestExtenderActivatorPostsAfterTheTcpCarrierReturns(t *testing.T) {
	fixture := newTestActivatorFixtureActivated(t)
	fixture.expectFamilyPostCounts("after start", 1, 1)

	fixture.setCarriers(ExtenderCarrierQuic)
	fixture.revoke()
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

// A carrier that went and came back with nothing owed posts nothing, so a
// listener that flaps does not spend the operator's budgets: the record it holds
// is still good, and the probes never got as far as revoking it.
func TestExtenderActivatorPostsNothingIfTheTcpCarrierReturnsWithNothingOwed(t *testing.T) {
	fixture := newTestActivatorFixtureActivated(t)
	for range 3 {
		fixture.setCarriers(ExtenderCarrierQuic)
		fixture.step(time.Minute)
		fixture.setCarriers(ExtenderCarrierTcp, ExtenderCarrierQuic)
		fixture.activator.TcpCarrierReturned()
		fixture.step(time.Minute)
	}
	fixture.expectFamilyPostCounts("after the flaps", 1, 1)
	for _, ipVersion := range []int{4, 6} {
		if status := fixture.activator.Status().Family(ipVersion); !status.Activated {
			t.Fatalf("v%d status = %+v", ipVersion, status)
		}
	}
}

// The return is for the family that is owed a post. The one that stands activated
// stays on its tick.
func TestExtenderActivatorTcpCarrierReturnPostsOnlyTheFamilyOwed(t *testing.T) {
	fixture := newTestActivatorFixtureWithOperator(t, func(operator *testActivateOperator) {
		operator.setFamilyRefusal(4, testActivateFamilyRefusal)
	}, nil)
	fixture.waitFirstPass()
	fixture.waitActivation()
	fixture.step(time.Minute)
	fixture.expectFamilyPostCounts("after start", 1, 1)

	// v4 is holding on its backoff, which this does not wait out
	fixture.operator.setFamilyRefusal(4, "")
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

// The one schedule of the plain api url follows the same rule.
func TestExtenderActivatorTcpCarrierReturnWithThePlainApiUrl(t *testing.T) {
	fixture := newTestActivatorFixture(t, func(settings *ExtenderActivatorSettings) {
		settings.ApiUrl = settings.ApiUrlV4
		settings.ApiUrlV4 = ""
		settings.ApiUrlV6 = ""
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
