package connect

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The measurement certificate request an activation carries (lag.dev/PLAN.md
// C4, N4). The request is named by the extender's id, which the extender
// learns from the operator's activation answer.
//
// The fixture operator of net_extender_activate_test.go predates the id, so
// each family host is fronted here by a server that passes every request to
// the operator's own and adds `extender_id` to the activations it answered.
// The posts are still read where the operator records them. The hook stands in
// for the extender's measurement certificate: it answers a request named for
// the id it is asked about, and nothing once the operator holds one.

type testMeasurementCsrHook struct {
	stateLock sync.Mutex
	// every id the hook was asked about, in order
	asks []Id
	// whether the operator holds a request for the id
	held bool
	// when set, replaces the answer for the nth ask, counted from 1
	answer func(n int, extenderId Id) string
}

func (self *testMeasurementCsrHook) measurementCsr(extenderId Id) string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.asks = append(self.asks, extenderId)
	if self.answer != nil {
		return self.answer(len(self.asks), extenderId)
	}
	if self.held {
		return ""
	}
	return testMeasurementCsr(extenderId)
}

func (self *testMeasurementCsrHook) setHeld(held bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.held = held
}

func (self *testMeasurementCsrHook) takeAsks() []Id {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	asks := self.asks
	self.asks = nil
	return asks
}

// The request the hook answers for an id.
func testMeasurementCsr(extenderId Id) string {
	return "csr:" + extenderId.String()
}

// A server on the family's loopback that passes each request to `operatorUrl`
// and adds `extenderId` to an activation the operator answered (N4).
func newTestMeasurementOperator(t *testing.T, ipVersion int, operatorUrl string, extenderId Id) string {
	t.Helper()
	server := newFamilyHttptestServer(t, ipVersion, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestBytes, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		request, err := http.NewRequest(r.Method, operatorUrl+r.URL.RequestURI(), bytes.NewReader(requestBytes))
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		request.Header = r.Header.Clone()
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		defer response.Body.Close()
		responseBytes, err := io.ReadAll(response.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		if r.URL.Path == ExtenderActivatePath && response.StatusCode == http.StatusOK {
			result := &ExtenderActivateResult{}
			if err := json.Unmarshal(responseBytes, result); err == nil && result.Activated {
				result.ExtenderId = &extenderId
				if resultBytes, err := json.Marshal(result); err == nil {
					responseBytes = resultBytes
				}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(response.StatusCode)
		w.Write(responseBytes)
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// An activator whose operator answers `extenderId`, asked for its request
// through `hook`. `configure` adjusts the settings once both are in place.
func newTestMeasurementActivatorFixture(
	t *testing.T,
	hook *testMeasurementCsrHook,
	extenderId Id,
	configure func(settings *ExtenderActivatorSettings),
) *testActivatorFixture {
	t.Helper()
	return newTestActivatorFixture(t, func(settings *ExtenderActivatorSettings) {
		settings.MeasurementCsr = hook.measurementCsr
		settings.ApiUrlV4 = newTestMeasurementOperator(t, 4, settings.ApiUrlV4, extenderId)
		settings.ApiUrlV6 = newTestMeasurementOperator(t, 6, settings.ApiUrlV6, extenderId)
		if configure != nil {
			configure(settings)
		}
	})
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

// A new identity learns its id from its first activation answer, so that post
// carries no request and the request rides the next post, which with two
// families is in the same pass. Nothing follows the pass. While the operator
// holds nothing a later pass carries the request once, on its first post, and
// once the operator holds it a pass carries none.
func TestExtenderActivatorMeasurementCsrRidesTheNextPost(t *testing.T) {
	hook := &testMeasurementCsrHook{}
	extenderId := NewId()
	fixture := newTestMeasurementActivatorFixture(t, hook, extenderId, nil)

	fixture.waitPass()
	fixture.expectMeasurementPosts(
		testMeasurementPost{ipVersion: 4},
		testMeasurementPost{ipVersion: 6, csr: testMeasurementCsr(extenderId)},
	)
	fixture.expectMeasurementPostCounts(1, 1)
	// asked once, for the post that carried it, and by the operator's id
	if asks := hook.takeAsks(); !slices.Equal(asks, []Id{extenderId}) {
		t.Fatalf("asks = %v, expected the operator's id once", asks)
	}

	// the id is known from the first post on: the request rides it, once
	fixture.forceActivation()
	fixture.expectMeasurementPosts(
		testMeasurementPost{ipVersion: 4, csr: testMeasurementCsr(extenderId)},
		testMeasurementPost{ipVersion: 6},
	)
	fixture.expectMeasurementPostCounts(2, 2)

	// the operator holds the request now: a pass carries none and nothing
	// follows it
	hook.setHeld(true)
	fixture.forceActivation()
	fixture.expectMeasurementPosts(
		testMeasurementPost{ipVersion: 4},
		testMeasurementPost{ipVersion: 6},
	)
	fixture.expectMeasurementPostCounts(3, 3)
}

// With one post a pass, a new identity's first pass has no later post to carry
// the request on, so one more pass follows at once and carries it.
func TestExtenderActivatorMeasurementCsrFollowsASinglePost(t *testing.T) {
	hook := &testMeasurementCsrHook{}
	extenderId := NewId()
	fixture := newTestMeasurementActivatorFixture(t, hook, extenderId, func(settings *ExtenderActivatorSettings) {
		settings.IpVersionSupported = func(ipVersion int) bool {
			return ipVersion == 4
		}
	})

	fixture.waitPass()
	fixture.expectMeasurementPosts(
		testMeasurementPost{ipVersion: 4},
		testMeasurementPost{ipVersion: 4, csr: testMeasurementCsr(extenderId)},
	)
	fixture.expectMeasurementPostCounts(2, 0)
}

// Through the plain api url a pass is one post, and the first is followed like
// any other single post.
func TestExtenderActivatorMeasurementCsrThroughThePlainApiUrl(t *testing.T) {
	hook := &testMeasurementCsrHook{}
	extenderId := NewId()
	fixture := newTestMeasurementActivatorFixture(t, hook, extenderId, func(settings *ExtenderActivatorSettings) {
		settings.ApiUrl = settings.ApiUrlV4
		settings.ApiUrlV4 = ""
		settings.ApiUrlV6 = ""
	})

	fixture.waitPass()
	fixture.expectMeasurementPosts(
		testMeasurementPost{ipVersion: 4},
		testMeasurementPost{ipVersion: 4, csr: testMeasurementCsr(extenderId)},
	)
	fixture.expectMeasurementPostCounts(2, 0)
}

// The pass that follows is followed by no other, even when it carried nothing
// and a request is wanted again after it, so a request that keeps missing
// cannot hold the loop.
func TestExtenderActivatorMeasurementCsrIsFollowedOnce(t *testing.T) {
	// wanted whenever the loop asks between passes, never when a post asks
	hook := &testMeasurementCsrHook{
		answer: func(n int, extenderId Id) string {
			if n%2 == 1 {
				return testMeasurementCsr(extenderId)
			}
			return ""
		},
	}
	fixture := newTestMeasurementActivatorFixture(t, hook, NewId(), func(settings *ExtenderActivatorSettings) {
		settings.IpVersionSupported = func(ipVersion int) bool {
			return ipVersion == 4
		}
	})

	fixture.waitPass()
	fixture.expectMeasurementPosts(
		testMeasurementPost{ipVersion: 4},
		testMeasurementPost{ipVersion: 4},
	)
	fixture.expectMeasurementPostCounts(2, 0)
	// after the first pass, and for the post that followed; not after it
	if asks := hook.takeAsks(); len(asks) != 2 {
		t.Fatalf("asks = %d, expected 2", len(asks))
	}
}

// An extender whose operator already holds the request for its name -- a
// restart -- is not followed by a second pass, on two families or one.
func TestExtenderActivatorMeasurementCsrHeldIsNotFollowed(t *testing.T) {
	for _, ipVersions := range [][]int{{4, 6}, {4}} {
		hook := &testMeasurementCsrHook{held: true}
		fixture := newTestMeasurementActivatorFixture(t, hook, NewId(), func(settings *ExtenderActivatorSettings) {
			settings.IpVersionSupported = func(ipVersion int) bool {
				return slices.Contains(ipVersions, ipVersion)
			}
		})

		fixture.waitPass()
		expectedCounts := map[int]int{}
		for _, ipVersion := range ipVersions {
			fixture.expectMeasurementPosts(testMeasurementPost{ipVersion: ipVersion})
			expectedCounts[ipVersion] = 1
		}
		fixture.expectMeasurementPostCounts(expectedCounts[4], expectedCounts[6])
	}
}

// An operator that predates the id reports none, so the request has no name
// to ask for: the hook is never asked, and no pass is followed.
func TestExtenderActivatorMeasurementCsrNeedsTheOperatorsId(t *testing.T) {
	hook := &testMeasurementCsrHook{}
	fixture := newTestActivatorFixture(t, func(settings *ExtenderActivatorSettings) {
		settings.MeasurementCsr = hook.measurementCsr
	})

	fixture.waitPass()
	fixture.expectMeasurementPosts(
		testMeasurementPost{ipVersion: 4},
		testMeasurementPost{ipVersion: 6},
	)
	fixture.expectMeasurementPostCounts(1, 1)
	if asks := hook.takeAsks(); len(asks) != 0 {
		t.Fatalf("asks = %v, expected none without an id", asks)
	}
}

// Without the hook no post carries a request and no pass is followed, which is
// every extender with measurement off.
func TestExtenderActivatorWithoutMeasurementCsr(t *testing.T) {
	fixture := newTestMeasurementActivatorFixture(t, &testMeasurementCsrHook{}, NewId(), func(settings *ExtenderActivatorSettings) {
		settings.MeasurementCsr = nil
	})

	fixture.waitPass()
	fixture.expectMeasurementPosts(
		testMeasurementPost{ipVersion: 4},
		testMeasurementPost{ipVersion: 6},
	)
	fixture.expectMeasurementPostCounts(1, 1)
}

// The id rides the activation answer as the operator writes it (N4), and an
// answer without one, or with a null, leaves it unset rather than failing the
// activation.
func TestExtenderActivateResultExtenderId(t *testing.T) {
	result := &ExtenderActivateResult{}
	if err := json.Unmarshal(
		[]byte(`{"activated":true,"extender_id":"0192e4a1-7b3c-7d2e-9f10-5a6b7c8d9e0f","ip":"198.51.100.7"}`),
		result,
	); err != nil {
		t.Fatal(err)
	}
	if result.ExtenderId == nil || result.ExtenderId.String() != "0192e4a1-7b3c-7d2e-9f10-5a6b7c8d9e0f" {
		t.Fatalf("extender id = %v", result.ExtenderId)
	}
	for _, answer := range []string{
		`{"activated":true,"ip":"198.51.100.7"}`,
		`{"activated":true,"extender_id":null,"ip":"198.51.100.7"}`,
		`{"activated":false,"error":"the tcp carrier did not answer"}`,
	} {
		result := &ExtenderActivateResult{}
		if err := json.Unmarshal([]byte(answer), result); err != nil {
			t.Fatalf("%s: %v", answer, err)
		}
		if result.ExtenderId != nil {
			t.Fatalf("%s: extender id = %v, expected none", answer, result.ExtenderId)
		}
	}
	// an answer without one writes none
	resultBytes, err := json.Marshal(&ExtenderActivateResult{Activated: true})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(resultBytes, []byte("extender_id")) {
		t.Fatalf("answer = %s, expected no extender id", resultBytes)
	}
}

// While the owner has opted out of measurement every post says so and carries
// no request, and nothing follows a pass; once the opt-out ends a pass asked
// for at once carries the request again and no longer says so (lag.dev/PLAN.md
// C4).
func TestExtenderActivatorMeasurementDisabled(t *testing.T) {
	hook := &testMeasurementCsrHook{}
	extenderId := NewId()
	var disabled atomic.Bool
	disabled.Store(true)
	fixture := newTestMeasurementActivatorFixture(t, hook, extenderId, func(settings *ExtenderActivatorSettings) {
		settings.MeasurementDisabled = disabled.Load
	})

	fixture.waitPass()
	for i := range 2 {
		post := fixture.waitPost()
		if !post.args.MeasurementDisabled || post.args.MeasurementCsr != "" {
			t.Fatalf("post %d = disabled %t, request %q, expected the opt-out alone", i, post.args.MeasurementDisabled, post.args.MeasurementCsr)
		}
	}
	fixture.expectMeasurementPostCounts(1, 1)
	if asks := hook.takeAsks(); len(asks) != 0 {
		t.Fatalf("asks = %v, expected none while opted out", asks)
	}

	disabled.Store(false)
	fixture.activator.Reactivate()
	for i, expected := range []testMeasurementPost{
		{ipVersion: 4, csr: testMeasurementCsr(extenderId)},
		{ipVersion: 6},
	} {
		post := fixture.waitPost()
		if post.ipVersion != expected.ipVersion || post.args.MeasurementCsr != expected.csr || post.args.MeasurementDisabled {
			t.Fatalf("post %d = v%d disabled %t %q, expected v%d %q", i, post.ipVersion, post.args.MeasurementDisabled, post.args.MeasurementCsr, expected.ipVersion, expected.csr)
		}
	}
	fixture.expectMeasurementPostCounts(2, 2)
}

// Reactivate runs a pass at once, without the clock moving: what the
// measurement lifecycle asks for when the operator holds another key, and the
// role when the owner opts out or back in (C4).
func TestExtenderActivatorReactivate(t *testing.T) {
	fixture := newTestActivatorFixture(t, nil)
	fixture.waitPass()
	fixture.expectMeasurementPosts(
		testMeasurementPost{ipVersion: 4},
		testMeasurementPost{ipVersion: 6},
	)
	fixture.activator.Reactivate()
	fixture.expectMeasurementPosts(
		testMeasurementPost{ipVersion: 4},
		testMeasurementPost{ipVersion: 6},
	)
	fixture.expectMeasurementPostCounts(2, 2)
}

// The opt-out rides the activation as the operator reads it, and an activation
// without it writes none (A10).
func TestExtenderActivateArgsMeasurementDisabled(t *testing.T) {
	argsBytes, err := json.Marshal(&ExtenderActivateArgs{MeasurementDisabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(argsBytes, []byte(`"measurement_disabled":true`)) {
		t.Fatalf("args = %s, expected measurement_disabled", argsBytes)
	}
	argsBytes, err = json.Marshal(&ExtenderActivateArgs{})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(argsBytes, []byte("measurement_disabled")) {
		t.Fatalf("args = %s, expected no measurement_disabled", argsBytes)
	}
}
