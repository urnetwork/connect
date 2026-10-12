package connect

import (
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// The measurement certificate request an activation carries (lag.dev/PLAN.md
// C4). The fixture operator of net_extender_activate_test.go answers v4 with
// testActivateIpv4 and v6 with testActivateIpv6. The hook here stands in for
// the extender's measurement certificate: it answers a request named for the
// addresses it is asked about, and nothing once the operator holds one for
// exactly those.

type testMeasurementCsrHook struct {
	stateLock sync.Mutex
	// every set of addresses the hook was asked about, in order
	asks [][]netip.Addr
	// the addresses the operator holds a request for
	held []netip.Addr
}

func (self *testMeasurementCsrHook) measurementCsr(addrs []netip.Addr) string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.asks = append(self.asks, slices.Clone(addrs))
	if slices.Equal(addrs, self.held) {
		return ""
	}
	return testMeasurementCsr(addrs)
}

func (self *testMeasurementCsrHook) setHeld(addrs []netip.Addr) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.held = slices.Clone(addrs)
}

func (self *testMeasurementCsrHook) takeAsks() [][]netip.Addr {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	asks := self.asks
	self.asks = nil
	return asks
}

// The request the hook answers for a set of addresses.
func testMeasurementCsr(addrs []netip.Addr) string {
	addrTexts := []string{}
	for _, addr := range addrs {
		addrTexts = append(addrTexts, addr.String())
	}
	return "csr:" + strings.Join(addrTexts, ",")
}

// Both fixture addresses, in ip version order.
func testMeasurementAddrs() []netip.Addr {
	return []netip.Addr{
		netip.MustParseAddr(testActivateIpv4),
		netip.MustParseAddr(testActivateIpv6),
	}
}

// One activation post as expected: the family it went to and the request it
// carried.
type testMeasurementPost struct {
	ipVersion int
	csr       string
}

func (self *testActivatorFixture) expectMeasurementPosts(expectedPosts ...testMeasurementPost) {
	self.t.Helper()
	for i, expected := range expectedPosts {
		post := self.waitPost()
		if post.ipVersion != expected.ipVersion || post.args.MeasurementCsr != expected.csr {
			self.t.Fatalf(
				"post %d = v%d %q, expected v%d %q",
				i, post.ipVersion, post.args.MeasurementCsr, expected.ipVersion, expected.csr)
		}
	}
}

// Steps one pass, which proves every earlier pass finished, and checks the
// posts each family has had.
func (self *testActivatorFixture) expectMeasurementPostCounts(v4Count int, v6Count int) {
	self.t.Helper()
	self.step(2 * time.Minute)
	if count := self.operator.postCount(4); count != v4Count {
		self.t.Fatalf("v4 posts = %d, expected %d", count, v4Count)
	}
	if count := self.operator.postCount(6); count != v6Count {
		self.t.Fatalf("v6 posts = %d, expected %d", count, v6Count)
	}
}

// A first pass cannot name an address the operator has not reported yet, so it
// carries no request. The pass that follows at once carries the one request,
// on its last post, for both families, and is followed by nothing. Once the
// operator holds it, a pass carries none.
func TestExtenderActivatorMeasurementCsrFollowsTheFirstPass(t *testing.T) {
	hook := &testMeasurementCsrHook{}
	fixture := newTestActivatorFixture(t, func(settings *ExtenderActivatorSettings) {
		settings.MeasurementCsr = hook.measurementCsr
	})

	fixture.waitPass()
	fixture.expectMeasurementPosts(
		testMeasurementPost{ipVersion: 4},
		testMeasurementPost{ipVersion: 6},
		testMeasurementPost{ipVersion: 4},
		testMeasurementPost{ipVersion: 6, csr: testMeasurementCsr(testMeasurementAddrs())},
	)
	fixture.expectMeasurementPostCounts(2, 2)
	// asked after the first pass, and for the post that followed; never for a
	// partial set of names
	asks := hook.takeAsks()
	if len(asks) != 2 || !slices.Equal(asks[0], testMeasurementAddrs()) || !slices.Equal(asks[1], testMeasurementAddrs()) {
		t.Fatalf("asks = %v, expected both addresses twice", asks)
	}

	// the operator holds the request now: a pass carries none and nothing
	// follows it
	hook.setHeld(testMeasurementAddrs())
	fixture.forceActivation()
	fixture.expectMeasurementPosts(
		testMeasurementPost{ipVersion: 4},
		testMeasurementPost{ipVersion: 6},
	)
	fixture.expectMeasurementPostCounts(3, 3)
}

// An extender of one family asks for its one name on the pass that follows its
// first.
func TestExtenderActivatorMeasurementCsrOneFamily(t *testing.T) {
	hook := &testMeasurementCsrHook{}
	fixture := newTestActivatorFixture(t, func(settings *ExtenderActivatorSettings) {
		settings.MeasurementCsr = hook.measurementCsr
		settings.IpVersionSupported = func(ipVersion int) bool {
			return ipVersion == 4
		}
	})

	fixture.waitPass()
	fixture.expectMeasurementPosts(
		testMeasurementPost{ipVersion: 4},
		testMeasurementPost{ipVersion: 4, csr: testMeasurementCsr(testMeasurementAddrs()[0:1])},
	)
	fixture.expectMeasurementPostCounts(2, 0)
}

// Through the plain api url the one post of a pass carries the request for the
// families answered so far, and the first is followed like any other.
func TestExtenderActivatorMeasurementCsrThroughThePlainApiUrl(t *testing.T) {
	hook := &testMeasurementCsrHook{}
	fixture := newTestActivatorFixture(t, func(settings *ExtenderActivatorSettings) {
		settings.MeasurementCsr = hook.measurementCsr
		settings.ApiUrl = settings.ApiUrlV4
		settings.ApiUrlV4 = ""
		settings.ApiUrlV6 = ""
	})

	fixture.waitPass()
	fixture.expectMeasurementPosts(
		testMeasurementPost{ipVersion: 4},
		testMeasurementPost{ipVersion: 4, csr: testMeasurementCsr(testMeasurementAddrs()[0:1])},
	)
	fixture.expectMeasurementPostCounts(2, 0)
}

// An extender whose operator already holds the request for its addresses -- a
// restart -- is not followed by a second pass.
func TestExtenderActivatorMeasurementCsrHeldIsNotFollowed(t *testing.T) {
	hook := &testMeasurementCsrHook{}
	hook.setHeld(testMeasurementAddrs())
	fixture := newTestActivatorFixture(t, func(settings *ExtenderActivatorSettings) {
		settings.MeasurementCsr = hook.measurementCsr
	})

	fixture.waitPass()
	fixture.expectMeasurementPosts(
		testMeasurementPost{ipVersion: 4},
		testMeasurementPost{ipVersion: 6},
	)
	fixture.expectMeasurementPostCounts(1, 1)
	if asks := hook.takeAsks(); len(asks) != 1 || !slices.Equal(asks[0], testMeasurementAddrs()) {
		t.Fatalf("asks = %v, expected one ask for both addresses", asks)
	}
}

// Without the hook no post carries a request and no pass is followed, which is
// every extender with measurement off.
func TestExtenderActivatorWithoutMeasurementCsr(t *testing.T) {
	fixture := newTestActivatorFixture(t, nil)

	fixture.waitPass()
	fixture.expectMeasurementPosts(
		testMeasurementPost{ipVersion: 4},
		testMeasurementPost{ipVersion: 6},
	)
	fixture.expectMeasurementPostCounts(1, 1)
}
