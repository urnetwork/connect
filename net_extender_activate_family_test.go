package connect

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect/protocol"
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
// A review of the first version found three holes, each tested here: a private
// family held for a day on any one refusal, even one that had activated and
// was refused by a budget, a probe or a revocation's re-post; a hello address
// remembered across an activation that moved the family's address; and a
// family without an address looked at again on a hold that doubled to six
// hours. A second review carried "activated" across a restart: the directory's
// own current record of the family counts.
//
// These run on the fixture of net_extender_activate_test.go. The fixture's
// address check is a minute, so every step of a minute or more begins with a
// hello, the pass barrier: the hello of a pass proves the pass before it
// finished.

// A refusal of a step of the probe that reached the host, which the backoff
// holds on a public family. A refusal of the tcp step, which found no answer,
// holds a public family that has not activated for the day instead
// (net_extender_activate_unreachable_test.go).
const testActivateFamilyRefusal = "the forward did not answer"

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

// A family whose host addresses are all private, refused before it has ever
// activated, is a NAT that does not forward, which no backoff changes: it is
// posted at start and then not until its tick, where a public family would
// have been posted eight times (the owner's decision, 2026-11-01). The record
// the v6 answer brings names v6 alone, so the directory names no v4 of this
// extender. The family with a public address keeps its own tick.
func TestExtenderActivatorHoldsANeverActivatedPrivateFamilyForTheDay(t *testing.T) {
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
// again every ten minutes: once the host has one, it is posted at the next
// look rather than at the tick of the family that was posted.
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

	// v6's next look, ten minutes from the pass that skipped it
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

// However long a family went without an address on the host, it is posted
// within ten minutes of gaining one: the look is local and costs nothing, so it
// is repeated every ten minutes rather than on a hold that doubles to six
// hours.
func TestExtenderActivatorPostsAFamilyThatGainsAnAddressLate(t *testing.T) {
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
	for range 12 {
		fixture.step(time.Hour)
	}
	// a barrier, so the look at twelve hours has finished before v6 appears
	fixture.step(time.Minute)
	fixture.expectFamilyPostCounts("after twelve hours without v6", 1, 0)

	v6Supported.Store(true)
	lookTime := startTime.Add(12*time.Hour + 10*time.Minute)
	fixture.step(lookTime.Sub(fixture.clock.Now()) - time.Minute)
	fixture.expectFamilyPostCounts("one minute short of v6's next look", 1, 0)
	fixture.clock.advance(time.Minute)
	if post := fixture.wakeForPost(); post.ipVersion != 6 || post.postTime != lookTime {
		t.Fatalf("post = v%d at %s, expected v6 at 12h10m", post.ipVersion, post.postTime.Sub(startTime))
	}
	fixture.waitActivation()
}

// A private family that has activated is reachable through whatever forwards
// to it, so a refusal after that -- the operator's budget, a probe that failed
// -- is held as any family's is: once accepted, refused once, then accepted, it
// is posted again ten minutes after the refusal.
func TestExtenderActivatorRetriesAPrivateFamilyThatActivated(t *testing.T) {
	fixture := newTestActivatorFixture(t, func(settings *ExtenderActivatorSettings) {
		settings.ApiUrlV6 = ""
		settings.IpVersionPublic = func(ipVersion int) bool { return false }
	})
	fixture.waitPass()
	fixture.waitPost()
	fixture.waitActivation()

	fixture.step(5 * time.Minute)
	refusalTime := fixture.clock.Now()
	fixture.operator.setRefusal(testActivateFamilyRefusal)
	fixture.networkChanged()
	if post := fixture.waitPost(); post.postTime != refusalTime {
		t.Fatalf("forced post at %s, expected %s", post.postTime, refusalTime)
	}
	fixture.operator.setRefusal("")

	fixture.step(9 * time.Minute)
	fixture.expectFamilyPostCounts("one minute short of the hold", 2, 0)
	fixture.clock.advance(time.Minute)
	if post := fixture.wakeForPost(); post.postTime != refusalTime.Add(10*time.Minute) {
		t.Fatalf("post at %s after the refusal, expected 10m", post.postTime.Sub(refusalTime))
	}
	if ipVersion := fixture.waitActivation(); ipVersion != 4 {
		t.Fatalf("activated v%d, expected v4", ipVersion)
	}
}

// The re-post of a revocation that the operator refuses once is held as any
// family's refusal: a private v4 that had activated is posted again ten
// minutes later, not a day later, and its record replaces the revocation.
func TestExtenderActivatorRetriesAPrivateFamilyAfterARevocation(t *testing.T) {
	fixture := newTestActivatorFixture(t, func(settings *ExtenderActivatorSettings) {
		settings.ApiUrlV6 = ""
		settings.IpVersionPublic = func(ipVersion int) bool { return false }
	})
	fixture.waitPass()
	fixture.waitPost()
	fixture.waitActivation()
	fixture.step(time.Hour)

	fixture.operator.setRefusal(testActivateFamilyRefusal)
	revocationTime := fixture.clock.Now()
	revocation := signTestRevocation(
		t, fixture.operator.rootPrivateKey, fixture.publicKey, revocationTime)
	if _, err := fixture.directory.ApplyRevocation(revocation); err != nil {
		t.Fatal(err)
	}
	if post := fixture.waitPost(); post.postTime != revocationTime {
		t.Fatalf("post after the revocation at %s, expected the revocation", post.postTime)
	}
	fixture.operator.setRefusal("")

	fixture.step(9 * time.Minute)
	fixture.expectFamilyPostCounts("one minute short of the hold", 2, 0)
	fixture.clock.advance(time.Minute)
	if post := fixture.wakeForPost(); post.postTime != revocationTime.Add(10*time.Minute) {
		t.Fatalf("post at %s after the revocation, expected 10m", post.postTime.Sub(revocationTime))
	}
	fixture.waitActivation()
	if state := fixture.directoryState(testActivateIpv4); state != ExtenderStateActive {
		t.Fatalf("%s state = %q, expected active", testActivateIpv4, state)
	}
}

// Applies a record of this extender naming `ips`, signed by the operator, to the
// directory, as a restart finds one an earlier run's activation published: kept
// by the directory's store, or brought back by the feed.
func (self *testActivatorFixture) applyOwnRecord(ips ...string) {
	self.t.Helper()
	now := self.clock.Now()
	applyTestOperatorRecord(
		self.t, self.directory, self.operator, self.publicKey, now, now.Add(14*24*time.Hour), ips...)
}

// Applies a record of `publicKey` naming `ips`, signed by the operator with the
// given issue and expire times, to the directory, as the feed would.
func applyTestOperatorRecord(
	t *testing.T,
	directory *ExtenderDirectory,
	operator *testActivateOperator,
	publicKey ed25519.PublicKey,
	issueTime time.Time,
	expireTime time.Time,
	ips ...string,
) {
	t.Helper()
	recordBase64, err := operator.signRecord(
		publicKey, ips, issueTime, expireTime, []string{ExtenderCarrierTcp})
	if err != nil {
		t.Fatal(err)
	}
	recordBytes, err := base64.StdEncoding.DecodeString(recordBase64)
	if err != nil {
		t.Fatal(err)
	}
	record := &protocol.ExtenderRecord{}
	if err := proto.Unmarshal(recordBytes, record); err != nil {
		t.Fatal(err)
	}
	if _, err := directory.ApplyRecord(record, ExtenderSourceFeed); err != nil {
		t.Fatal(err)
	}
}

// Only the newest record of a key says what the operator publishes now. The
// directory keeps every address the key's records have listed, and they read
// active, but CurrentRecordNamesFamily reads the newest record alone, valid,
// with addresses still the key's.
func TestExtenderDirectoryCurrentRecordNamesFamily(t *testing.T) {
	clock := newTestClock()
	directory, rootPrivateKey := newTestExtenderDirectory(t, clock, nil)
	operator := newTestActivateOperator(t, clock, rootPrivateKey)
	newKey := func() ed25519.PublicKey {
		seed, err := NewExtenderKeySeed()
		if err != nil {
			t.Fatal(err)
		}
		publicKey, err := ExtenderPublicKeyFromSeed(seed)
		if err != nil {
			t.Fatal(err)
		}
		return publicKey
	}
	expectNames := func(what string, publicKey ed25519.PublicKey, names4 bool, names6 bool) {
		t.Helper()
		if got := directory.CurrentRecordNamesFamily(publicKey, 4); got != names4 {
			t.Fatalf("%s: names v4 = %t, expected %t", what, got, names4)
		}
		if got := directory.CurrentRecordNamesFamily(publicKey, 6); got != names6 {
			t.Fatalf("%s: names v6 = %t, expected %t", what, got, names6)
		}
	}
	day := 14 * 24 * time.Hour

	// a newer record that drops v4
	dropped := newKey()
	expectNames("no record", dropped, false, false)
	applyTestOperatorRecord(t, directory, operator, dropped, clock.Now(), clock.Now().Add(day), testActivateIpv4, testActivateIpv6)
	expectNames("a record of both", dropped, true, true)
	clock.advance(time.Second)
	applyTestOperatorRecord(t, directory, operator, dropped, clock.Now(), clock.Now().Add(day), testActivateIpv6)
	if state := testDirectoryState(t, directory, netip.MustParseAddr(testActivateIpv4)); state != ExtenderStateActive {
		t.Fatalf("%s state = %q, expected the address the older record named kept and active", testActivateIpv4, state)
	}
	expectNames("a newer record of v6 alone", dropped, false, true)

	// a revoked key
	revoked := newKey()
	applyTestOperatorRecord(t, directory, operator, revoked, clock.Now(), clock.Now().Add(day), "198.51.100.30")
	expectNames("a record of v4", revoked, true, false)
	clock.advance(time.Second)
	if _, err := directory.ApplyRevocation(signTestRevocation(t, rootPrivateKey, revoked, clock.Now())); err != nil {
		t.Fatal(err)
	}
	expectNames("revoked", revoked, false, false)

	// an address another key claims since
	claimed := newKey()
	applyTestOperatorRecord(t, directory, operator, claimed, clock.Now(), clock.Now().Add(day), "198.51.100.31")
	expectNames("a record of v4", claimed, true, false)
	clock.advance(time.Second)
	applyTestOperatorRecord(t, directory, operator, newKey(), clock.Now(), clock.Now().Add(day), "198.51.100.31")
	expectNames("its address claimed by another key", claimed, false, false)

	// an expired record
	expired := newKey()
	applyTestOperatorRecord(t, directory, operator, expired, clock.Now(), clock.Now().Add(time.Hour), "198.51.100.32")
	expectNames("a record of v4", expired, true, false)
	clock.advance(2 * time.Hour)
	expectNames("expired", expired, false, false)
}

// The newest record of this extender names v6 alone: the operator stopped
// publishing v4, which an earlier record named. The directory keeps that v4
// address under this key and it reads active, but it is no evidence that
// anything forwards to v4 now, so a private v4 refused at start waits for its
// tick, as the owner decided.
func TestExtenderActivatorCountsOnlyTheNewestOwnRecord(t *testing.T) {
	fixture := newTestActivatorFixtureWithPrepare(t, func(fixture *testActivatorFixture) {
		fixture.applyOwnRecord(testActivateIpv4, testActivateIpv6)
		fixture.clock.advance(time.Second)
		fixture.applyOwnRecord(testActivateIpv6)
		fixture.operator.setFamilyRefusal(4, testActivateFamilyRefusal)
	}, func(settings *ExtenderActivatorSettings) {
		settings.IpVersionPublic = func(ipVersion int) bool { return ipVersion == 6 }
	})
	startTime := fixture.clock.Now()
	if state := fixture.directoryState(testActivateIpv4); state != ExtenderStateActive {
		t.Fatalf("%s state = %q, expected the dropped address kept and active", testActivateIpv4, state)
	}
	if fixture.directory.CurrentRecordNamesFamily(fixture.publicKey, 4) {
		t.Fatal("the newest own record names v4")
	}
	fixture.waitFirstPass()
	fixture.waitActivation()

	for _, at := range []time.Duration{
		10 * time.Minute,
		30 * time.Minute,
		70 * time.Minute,
		6 * time.Hour,
		24*time.Hour - time.Minute,
	} {
		fixture.step(startTime.Add(at).Sub(fixture.clock.Now()))
		fixture.expectFamilyPostCounts(fmt.Sprintf("at %s", at), 1, 1)
	}
	fixture.clock.advance(time.Minute)
	tickTime := startTime.Add(24 * time.Hour)
	if post := fixture.wakeForPost(); post.ipVersion != 4 || post.postTime != tickTime {
		t.Fatalf("tick post = v%d at %s, expected v4 at 24h", post.ipVersion, post.postTime.Sub(startTime))
	}
}

// A private family the directory holds this extender's own current record for
// was published by the operator after its probes passed, so something forwards
// to it: when the first post after a restart is refused -- the operator's
// budget, a probe that failed -- it is posted again ten minutes later, as an
// activated family is, not at its tick.
func TestExtenderActivatorRetriesAPrivateFamilyTheDirectoryNames(t *testing.T) {
	fixture := newTestActivatorFixtureWithPrepare(t, func(fixture *testActivatorFixture) {
		fixture.applyOwnRecord(testActivateIpv4)
		fixture.operator.setRefusal(testActivateFamilyRefusal)
	}, func(settings *ExtenderActivatorSettings) {
		settings.ApiUrlV6 = ""
		settings.IpVersionPublic = func(ipVersion int) bool { return false }
	})
	startTime := fixture.clock.Now()
	if state := fixture.directoryState(testActivateIpv4); state != ExtenderStateActive {
		t.Fatalf("%s state = %q, expected the own record active", testActivateIpv4, state)
	}
	fixture.waitPass()
	if post := fixture.waitPost(); post.postTime != startTime {
		t.Fatalf("first post at %s, expected the start", post.postTime.Sub(startTime))
	}
	fixture.operator.setRefusal("")

	fixture.step(9 * time.Minute)
	fixture.expectFamilyPostCounts("one minute short of the hold", 1, 0)
	fixture.clock.advance(time.Minute)
	if post := fixture.wakeForPost(); post.postTime != startTime.Add(10*time.Minute) {
		t.Fatalf("post at %s, expected 10m", post.postTime.Sub(startTime))
	}
	if ipVersion := fixture.waitActivation(); ipVersion != 4 {
		t.Fatalf("activated v%d, expected v4", ipVersion)
	}
}

// The schedules move once every post of the pass has been answered, so the
// record another family's answer brings counts: a v4 refused at start, whose
// address the operator still holds from an earlier run, is named by the record
// the v6 answer carries, and is posted again ten minutes later.
func TestExtenderActivatorRetriesAPrivateFamilyAnotherAnswerNames(t *testing.T) {
	fixture := newTestActivatorFixtureWithPrepare(t, func(fixture *testActivatorFixture) {
		fixture.operator.setFamilyRefusal(4, testActivateFamilyRefusal)
		// the v4 address an earlier run activated, which the operator still
		// holds, so every record it signs names it
		fixture.operator.activatedIps[4] = testActivateIpv4
	}, func(settings *ExtenderActivatorSettings) {
		settings.IpVersionPublic = func(ipVersion int) bool { return ipVersion == 6 }
	})
	startTime := fixture.clock.Now()
	fixture.waitFirstPass()
	if ipVersion := fixture.waitActivation(); ipVersion != 6 {
		t.Fatalf("activated v%d, expected v6", ipVersion)
	}
	fixture.operator.setFamilyRefusal(4, "")

	fixture.step(9 * time.Minute)
	fixture.expectFamilyPostCounts("one minute short of v4's hold", 1, 1)
	if state := fixture.directoryState(testActivateIpv4); state != ExtenderStateActive {
		t.Fatalf("%s state = %q, expected the own record active", testActivateIpv4, state)
	}
	fixture.clock.advance(time.Minute)
	if post := fixture.wakeForPost(); post.ipVersion != 4 || post.postTime != startTime.Add(10*time.Minute) {
		t.Fatalf("post = v%d at %s, expected v4 at 10m", post.ipVersion, post.postTime.Sub(startTime))
	}
	if ipVersion := fixture.waitActivation(); ipVersion != 4 {
		t.Fatalf("activated v%d, expected v4", ipVersion)
	}
}

// A revoked own record is the operator taking the family back, which says
// nothing that forwards still does: a private family refused at start, with
// only a revoked record of its own in the directory, waits for its tick.
func TestExtenderActivatorDoesNotCountARevokedOwnRecord(t *testing.T) {
	fixture := newTestActivatorFixtureWithPrepare(t, func(fixture *testActivatorFixture) {
		fixture.applyOwnRecord(testActivateIpv4)
		fixture.clock.advance(time.Minute)
		revocation := signTestRevocation(
			t, fixture.operator.rootPrivateKey, fixture.publicKey, fixture.clock.Now())
		if _, err := fixture.directory.ApplyRevocation(revocation); err != nil {
			t.Fatal(err)
		}
		fixture.operator.setRefusal(testActivateFamilyRefusal)
	}, func(settings *ExtenderActivatorSettings) {
		settings.ApiUrlV6 = ""
		settings.IpVersionPublic = func(ipVersion int) bool { return false }
	})
	startTime := fixture.clock.Now()
	if state := fixture.directoryState(testActivateIpv4); state != ExtenderStateRevoked {
		t.Fatalf("%s state = %q, expected the own record revoked", testActivateIpv4, state)
	}
	fixture.waitPass()
	fixture.waitPost()

	for _, at := range []time.Duration{
		10 * time.Minute,
		30 * time.Minute,
		70 * time.Minute,
		6 * time.Hour,
		24*time.Hour - time.Minute,
	} {
		fixture.step(startTime.Add(at).Sub(fixture.clock.Now()))
		fixture.expectFamilyPostCounts(fmt.Sprintf("at %s", at), 1, 0)
	}
	fixture.clock.advance(time.Minute)
	if post := fixture.wakeForPost(); post.postTime != startTime.Add(24*time.Hour) {
		t.Fatalf("tick post at %s, expected 24h", post.postTime.Sub(startTime))
	}
}

// A request that fails, rather than one the operator refuses, says nothing of
// a NAT: a private family whose post fails is held on the backoff, even before
// it has activated.
func TestExtenderActivatorHoldsAFailedPrivateFamilyOnTheBackoff(t *testing.T) {
	fixture := newTestActivatorFixtureWithOperator(t, func(operator *testActivateOperator) {
		operator.setAnswer(testActivateAnswerUnavailable)
	}, func(settings *ExtenderActivatorSettings) {
		settings.ApiUrlV6 = ""
		settings.IpVersionPublic = func(ipVersion int) bool { return false }
	})
	startTime := fixture.clock.Now()
	fixture.waitPass()
	fixture.waitPost()
	fixture.operator.setAnswer(testActivateAnswerActivated)

	fixture.step(9 * time.Minute)
	fixture.expectFamilyPostCounts("one minute short of the hold", 1, 0)
	fixture.clock.advance(time.Minute)
	if post := fixture.wakeForPost(); post.postTime != startTime.Add(10*time.Minute) {
		t.Fatalf("post at %s, expected 10m", post.postTime.Sub(startTime))
	}
	fixture.waitActivation()
}

// A hello that arrives on a family that is not activated -- from the public
// address of a NAT that refuses the family -- posts nothing: the family is left
// to its own schedule.
func TestExtenderActivatorIgnoresAHelloOnAFamilyNotActivated(t *testing.T) {
	fixture := newTestActivatorFixtureWithOperator(t, func(operator *testActivateOperator) {
		operator.setFamilyRefusal(4, testActivateFamilyRefusal)
	}, func(settings *ExtenderActivatorSettings) {
		settings.IpVersionPublic = func(ipVersion int) bool { return ipVersion == 6 }
	})
	fixture.waitFirstPass()
	fixture.waitActivation()

	for i := range 4 {
		fixture.operator.setClientAddress(fmt.Sprintf("198.51.100.%d:%d", 50+i, 43000+i))
		fixture.step(30 * time.Minute)
	}
	fixture.expectFamilyPostCounts("after hellos on the refused family", 1, 1)
}

// The address the last check saw on a family is unchanged only while the
// family's published address is the one it was read against. Here the check
// sees v4 at X, a network change publishes Y, the checks then arrive on v6,
// and v4's egress returns to X: the next check that sees X on v4 posts v4,
// where the record would otherwise have named Y until the tick.
func TestExtenderActivatorForgetsAHelloOnceTheFamilyMoves(t *testing.T) {
	fixture := newTestActivatorFixture(t, nil)
	fixture.waitFirstPass()
	fixture.waitActivation()
	fixture.waitActivation()

	const movedIpv4 = "198.51.100.8"
	fixture.operator.setPublishedIp(4, movedIpv4)
	// the first pass's check saw X. The checks arrive on v6 from here, before the
	// change: a pass that read the clock just ahead of it posts first and checks in
	// the next pass, and a check that saw X then would be a move of its own
	fixture.operator.setClientAddress(fmt.Sprintf("[%s]:42001", testActivateIpv6))
	fixture.clock.advance(2 * time.Minute)
	fixture.networkChanged()
	for _, ipVersion := range []int{4, 6} {
		if post := fixture.waitPost(); post.ipVersion != ipVersion {
			t.Fatalf("network change posted v%d, expected v%d", post.ipVersion, ipVersion)
		}
	}
	fixture.waitActivation()
	fixture.waitActivation()
	if ip := fixture.activator.Status().Family(4).Ip.String(); ip != movedIpv4 {
		t.Fatalf("v4 ip = %s, expected %s", ip, movedIpv4)
	}

	fixture.step(2 * time.Minute)
	fixture.step(2 * time.Minute)
	fixture.expectFamilyPostCounts("with the checks on v6", 2, 2)

	fixture.operator.setPublishedIp(4, testActivateIpv4)
	fixture.operator.setClientAddress(testActivateIpv4 + ":42002")
	fixture.clock.advance(2 * time.Minute)
	if post := fixture.wakeForPost(); post.ipVersion != 4 {
		t.Fatalf("posted v%d for v4 back at its old address, expected v4", post.ipVersion)
	}
	fixture.waitActivation()
	fixture.step(2 * time.Minute)
	fixture.expectFamilyPostCounts("after the check saw X again", 3, 2)
	if ip := fixture.activator.Status().Family(4).Ip.String(); ip != testActivateIpv4 {
		t.Fatalf("v4 ip = %s, expected %s", ip, testActivateIpv4)
	}
}

// With no family seam set, the activator reads the host's interface table:
// FamilySupported counts a cloud vm's private v4, so v4 is posted, and the
// public-address probe finds it private, so a refusal before it has activated
// waits for the tick, while the public v6 keeps its own.
func TestExtenderActivatorReadsTheHostsFamiliesByDefault(t *testing.T) {
	restore := swapControlFamilyInterfaces(func() ([]controlFamilyInterface, error) {
		return []controlFamilyInterface{
			{
				name:  "lo0",
				flags: net.FlagUp | net.FlagLoopback,
				addrs: []net.Addr{ipNet("127.0.0.1/8"), ipNet("::1/128")},
			},
			{
				// 3fff::/20 stands in for a public v6 without naming a network
				name:  "en0",
				flags: net.FlagUp,
				addrs: []net.Addr{ipNet("10.1.2.3/24"), ipNet("3fff:db8::1/64")},
			},
		}, nil
	})
	defer restore()
	fixture := newTestActivatorFixtureWithOperator(t, func(operator *testActivateOperator) {
		operator.setFamilyRefusal(4, testActivateFamilyRefusal)
	}, func(settings *ExtenderActivatorSettings) {
		settings.IpVersionSupported = nil
		settings.IpVersionPublic = nil
	})
	startTime := fixture.clock.Now()
	fixture.waitFirstPass()
	fixture.waitActivation()

	for _, at := range []time.Duration{
		10 * time.Minute,
		30 * time.Minute,
		70 * time.Minute,
		6 * time.Hour,
		24*time.Hour - time.Minute,
	} {
		fixture.step(startTime.Add(at).Sub(fixture.clock.Now()))
		fixture.expectFamilyPostCounts(fmt.Sprintf("at %s", at), 1, 1)
	}
	fixture.clock.advance(time.Minute)
	tickTime := startTime.Add(24 * time.Hour)
	if post := fixture.wakeForPost(); post.ipVersion != 4 || post.postTime != tickTime {
		t.Fatalf("tick post = v%d at %s, expected v4 at 24h", post.ipVersion, post.postTime.Sub(startTime))
	}
	if post := fixture.waitPost(); post.ipVersion != 6 || post.postTime != tickTime {
		t.Fatalf("tick post = v%d at %s, expected v6 at 24h", post.ipVersion, post.postTime.Sub(startTime))
	}
}

// An interface table that cannot be read is no evidence of a NAT, so the
// activator holds a refusal as a public family's, on the backoff, while the
// exported probe still fails closed.
func TestExtenderActivatorTakesAnUnreadableTableForPublic(t *testing.T) {
	restore := swapControlFamilyInterfaces(func() ([]controlFamilyInterface, error) {
		return nil, errors.New("operation not permitted")
	})
	defer restore()
	activator := &ExtenderActivator{settings: DefaultExtenderActivatorSettings()}
	for _, ipVersion := range []int{4, 6} {
		if !activator.ipVersionPublic(ipVersion) {
			t.Fatalf("v%d with an unreadable table read as private", ipVersion)
		}
		if FamilyPublic(ipVersion) {
			t.Fatalf("FamilyPublic(%d) said yes with an unreadable table", ipVersion)
		}
	}
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
