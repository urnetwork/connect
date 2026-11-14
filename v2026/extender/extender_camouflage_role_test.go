package extender

// The camouflage of a provider role (extender_camouflage_role.go): the key it
// publishes, the names it verifies at start and every revalidation interval and
// feeds the running server, and the safe-flip gate it feeds from its activations.
// Time is the schedule seam and a fake clock; the only real dials are to loopback
// fake borrowed sites, and every list is synthetic.

import (
	"bytes"
	"context"
	"net"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
)

// The schedule seam of a role: each wait is reported, and held until fired.
type testRoleSchedule struct {
	waits chan time.Duration
	fire  chan time.Time
}

func newTestRoleSchedule() *testRoleSchedule {
	return &testRoleSchedule{
		// a role that closes does not read its fire; nothing here blocks it
		waits: make(chan time.Duration, 64),
		fire:  make(chan time.Time),
	}
}

// Implements ExtenderCamouflageRoleSettings.After.
func (self *testRoleSchedule) after(wait time.Duration) <-chan time.Time {
	self.waits <- wait
	return self.fire
}

// The next wait of the role, which proves the pass before it has fed the server.
func (self *testRoleSchedule) nextWait(t *testing.T) time.Duration {
	t.Helper()
	select {
	case wait := <-self.waits:
		return wait
	case <-time.After(30 * time.Second):
		t.Fatal("the role did not finish a pass")
		return 0
	}
}

// Ends the role's current wait.
func (self *testRoleSchedule) fireNow(t *testing.T) {
	t.Helper()
	select {
	case self.fire <- time.Now():
	case <-time.After(30 * time.Second):
		t.Fatal("the role is not waiting")
	}
}

// The verification seam of a role: passes unless told otherwise, and records what
// it was asked.
type testRoleVerifier struct {
	stateLock sync.Mutex
	// names that fail, on every network or on the one given as "<network> <name>"
	failing map[string]bool
	calls   []string
}

func newTestRoleVerifier() *testRoleVerifier {
	return &testRoleVerifier{failing: map[string]bool{}}
}

// Implements ExtenderCamouflageRoleSettings.VerifyBorrowName.
func (self *testRoleVerifier) verify(ctx context.Context, network string, borrowName string) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.calls = append(self.calls, network+" "+borrowName)
	return !self.failing[borrowName] && !self.failing[network+" "+borrowName]
}

// Replaces what fails.
func (self *testRoleVerifier) setFailing(failing ...string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.failing = map[string]bool{}
	for _, name := range failing {
		self.failing[name] = true
	}
}

// What it was asked since the last take, sorted, since passes run concurrently.
func (self *testRoleVerifier) takeCalls() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	calls := self.calls
	self.calls = nil
	slices.Sort(calls)
	return calls
}

// A measured list of the names, current on the real clock.
func newTestRoleBorrowList(names ...string) *connect.ExtenderBorrowList {
	borrowList := &connect.ExtenderBorrowList{
		Version:           1,
		Measured:          true,
		MeasuredAt:        time.Now(),
		MaxMeasurementAge: 365 * 24 * time.Hour,
	}
	for _, name := range names {
		borrowList.Names = append(borrowList.Names, connect.ExtenderBorrowName{Name: name})
	}
	return borrowList
}

// Installs the list as the only bundled one for the rest of the test.
func installTestRoleBorrowLists(t *testing.T, borrowLists ...*connect.ExtenderBorrowList) {
	t.Helper()
	t.Cleanup(connect.Testing_SetExtenderCamouflageBorrowLists(borrowLists))
}

// One role and the running server it configured.
type testRoleFixture struct {
	seed              []byte
	identityPublicKey []byte
	realityPublicKey  []byte
	clock             *testSettleClock
	schedule          *testRoleSchedule
	verifier          *testRoleVerifier
	role              *ExtenderCamouflageRole
	server            *extenderFixture
}

// Builds a role on a v4 host, its schedule and verification under the test's
// hand, with its clock and its gate's on a fake clock, and starts it on a running
// server. configure runs before it is built, so what it scripts holds for the
// first pass, and configureServer adjusts the server's settings after the role
// has. Neither is required.
func newTestRoleFixture(
	t *testing.T,
	configure func(fixture *testRoleFixture, settings *ExtenderCamouflageRoleSettings),
	configureServer func(settings *ExtenderSettings),
) *testRoleFixture {
	t.Helper()
	seed, identityPublicKey, realityPublicKey := newCamouflageKeys(t)
	fixture := &testRoleFixture{
		seed:              seed,
		identityPublicKey: identityPublicKey,
		realityPublicKey:  realityPublicKey,
		clock:             &testSettleClock{now: time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)},
		schedule:          newTestRoleSchedule(),
		verifier:          newTestRoleVerifier(),
	}
	settings := DefaultExtenderCamouflageRoleSettings()
	settings.Now = fixture.clock.Now
	settings.After = fixture.schedule.after
	settings.VerifyBorrowName = fixture.verifier.verify
	settings.IpVersionSupported = func(ipVersion int) bool { return ipVersion == 4 }
	settings.Settle = &ExtenderCamouflageSpliceSettleSettings{Margin: testSettleMargin, Now: fixture.clock.Now}
	if configure != nil {
		configure(fixture, settings)
	}
	role, err := NewExtenderCamouflageRole(seed, settings)
	if err != nil {
		t.Fatal(err)
	}
	fixture.role = role
	fixture.server = newExtenderFixture(t, "127.0.0.1", func(serverSettings *ExtenderSettings) {
		role.ConfigureServer(serverSettings)
		if configureServer != nil {
			configureServer(serverSettings)
		}
	})
	t.Cleanup(role.Close)
	role.Start(context.Background(), fixture.server.server)
	return fixture
}

// Renews a qualifying record through the wait the gate asks for, as the activator
// does, which opens the gate.
func (self *testRoleFixture) settleTheRecords(t *testing.T) *testSettleFixture {
	t.Helper()
	settle := newTestSettleFixture(t)
	settle.clock = self.clock
	settle.identityPublicKey = self.identityPublicKey
	settle.realityPublicKey = self.realityPublicKey
	settle.settle = self.role.settle
	settle.renewFor(testSettleRecordLifetime + testSettleMargin)
	self.role.ObserveActivation(settle.activation(settle.body(nil)), settle.rootKeys)
	if !self.role.settle.Open() {
		t.Fatal("the records did not settle")
	}
	return settle
}

// What the role passes the server: the camouflage on, the posture it was given,
// the gate, no name yet, and the identity it derives the key from, so the key the
// server opens tags under is the one it publishes.
func TestExtenderCamouflageRoleConfiguresTheServer(t *testing.T) {
	seed, _, realityPublicKey := newCamouflageKeys(t)
	for _, splice := range []bool{false, true} {
		role, err := NewExtenderCamouflageRole(seed, &ExtenderCamouflageRoleSettings{Splice: splice})
		if err != nil {
			t.Fatal(err)
		}
		settings := DefaultExtenderSettings()
		settings.CamouflageBorrowNames = []string{"left.example"}
		role.ConfigureServer(settings)
		if !settings.CamouflageEnabled {
			t.Error("the camouflage is not on")
		}
		if settings.ExtenderCamouflageSplice != splice {
			t.Errorf("posture = %t, expected %t", settings.ExtenderCamouflageSplice, splice)
		}
		if settings.CamouflageSpliceGate == nil {
			t.Error("the gate is not wired")
		} else if settings.CamouflageSpliceGate() {
			t.Error("the gate opened before any record")
		}
		if 0 < len(settings.CamouflageBorrowNames) {
			t.Errorf("names = %v, expected none before they verify", settings.CamouflageBorrowNames)
		}
		if !bytes.Equal(settings.IdentityKeySeed, seed) {
			t.Error("the server is not on the role's identity")
		}

		server := NewExtenderServer(context.Background(), nil, nil, nil, &net.Dialer{}, settings)
		if !bytes.Equal(server.RealityPublicKey(), realityPublicKey) {
			t.Error("the server's key is not the one derived from the identity")
		}
		server.CloseAndWait()
	}
}

// A seed the key cannot derive from is the role's start failing, with a message
// that says what is wrong and nothing of the seed.
func TestExtenderCamouflageRoleRefusesAnUnusableSeed(t *testing.T) {
	for _, seed := range [][]byte{nil, {}, []byte("too short")} {
		role, err := NewExtenderCamouflageRole(seed, nil)
		if err == nil {
			t.Fatalf("a seed of %d bytes built a role", len(seed))
		}
		if role != nil {
			t.Fatal("a refused seed returned a role")
		}
		if !strings.Contains(err.Error(), "identity key") {
			t.Fatalf("error = %q, which does not name the identity key", err)
		}
		if 0 < len(seed) && strings.Contains(err.Error(), string(seed)) {
			t.Fatalf("error = %q, which repeats the seed", err)
		}
	}
}

// The names verify at start and at every interval, the running server takes what
// passes, and a name that fails is dropped and counted. The wait is the interval
// at most, spread by at most a tenth.
func TestExtenderCamouflageRoleVerifiesAtStartAndEveryInterval(t *testing.T) {
	installTestRoleBorrowLists(t, newTestRoleBorrowList("a.example", "b.example"))
	fixture := newTestRoleFixture(t, func(_ *testRoleFixture, settings *ExtenderCamouflageRoleSettings) {
		settings.RevalidationInterval = 12 * time.Hour
	}, nil)
	server := fixture.server.server

	wait := fixture.schedule.nextWait(t)
	if 12*time.Hour < wait || wait < 12*time.Hour-12*time.Hour/10 {
		t.Fatalf("wait = %s, expected within a tenth under the interval", wait)
	}
	if calls := fixture.verifier.takeCalls(); !slices.Equal(calls, []string{"tcp4 a.example", "tcp4 b.example"}) {
		t.Fatalf("calls at start = %v", calls)
	}
	if count := server.CamouflageStatus().VerifiedTargetCount; count != 2 {
		t.Fatalf("verified names at start = %d, expected 2", count)
	}
	if count := server.CamouflageStats().BorrowVerifyFailCount; count != 0 {
		t.Fatalf("failed verifications at start = %d", count)
	}

	fixture.verifier.setFailing("b.example")
	fixture.schedule.fireNow(t)
	fixture.schedule.nextWait(t)
	if calls := fixture.verifier.takeCalls(); !slices.Equal(calls, []string{"tcp4 a.example", "tcp4 b.example"}) {
		t.Fatalf("calls at the interval = %v", calls)
	}
	if count := server.CamouflageStatus().VerifiedTargetCount; count != 1 {
		t.Fatalf("verified names after the failure = %d, expected 1", count)
	}
	if count := server.CamouflageStats().BorrowVerifyFailCount; count != 1 {
		t.Fatalf("failed verifications = %d, expected 1", count)
	}
}

// A failed name is tried again sooner than the interval, alone, each retry twice
// as long after the last, and comes back when it passes. No wait outlasts the
// interval, so the next pass over every name is on time.
func TestExtenderCamouflageRoleRetriesAFailedNameAloneAndBacksOff(t *testing.T) {
	installTestRoleBorrowLists(t, newTestRoleBorrowList("a.example", "b.example"))
	interval := DefaultExtenderCamouflageRevalidationInterval
	retry := DefaultExtenderCamouflageRoleSettings().VerifyRetryInterval
	fixture := newTestRoleFixture(t, func(fixture *testRoleFixture, _ *ExtenderCamouflageRoleSettings) {
		fixture.verifier.setFailing("b.example")
	}, nil)
	server := fixture.server.server

	wait := fixture.schedule.nextWait(t)
	if wait != retry {
		t.Fatalf("the first retry waits %s, expected %s", wait, retry)
	}
	if calls := fixture.verifier.takeCalls(); !slices.Equal(calls, []string{"tcp4 a.example", "tcp4 b.example"}) {
		t.Fatalf("the first pass asked %v", calls)
	}
	elapsed := wait
	fixture.schedule.fireNow(t)
	wait = fixture.schedule.nextWait(t)
	if wait != 2*retry {
		t.Fatalf("the second retry waits %s, expected %s", wait, 2*retry)
	}
	if calls := fixture.verifier.takeCalls(); !slices.Equal(calls, []string{"tcp4 b.example"}) {
		t.Fatalf("a retry asked %v, expected the failed name alone", calls)
	}
	if count := server.CamouflageStatus().VerifiedTargetCount; count != 1 {
		t.Fatalf("verified names during the retries = %d, expected the one that passed", count)
	}
	if count := server.CamouflageStats().BorrowVerifyFailCount; count != 2 {
		t.Fatalf("failed verifications = %d, expected one a pass", count)
	}
	elapsed += wait

	fixture.verifier.setFailing()
	fixture.schedule.fireNow(t)
	wait = fixture.schedule.nextWait(t)
	elapsed += wait
	if count := server.CamouflageStatus().VerifiedTargetCount; count != 2 {
		t.Fatalf("verified names after the retry passed = %d, expected 2", count)
	}
	if interval < elapsed || elapsed < interval-interval/10 {
		t.Fatalf("the interval's waits sum to %s, expected within a tenth under %s", elapsed, interval)
	}
	if calls := fixture.verifier.takeCalls(); !slices.Equal(calls, []string{"tcp4 b.example"}) {
		t.Fatalf("the last retry asked %v", calls)
	}

	fixture.schedule.fireNow(t)
	fixture.schedule.nextWait(t)
	if calls := fixture.verifier.takeCalls(); !slices.Equal(calls, []string{"tcp4 a.example", "tcp4 b.example"}) {
		t.Fatalf("the next interval asked %v, expected every name", calls)
	}
}

// A name that keeps failing is retried on a schedule that doubles and stops short
// of the interval, so what a dead name costs a site is bounded, and every name is
// verified again on time.
func TestExtenderCamouflageRoleRetryScheduleStaysInsideTheInterval(t *testing.T) {
	installTestRoleBorrowLists(t, newTestRoleBorrowList("a.example", "dead.example"))
	fixture := newTestRoleFixture(t, func(fixture *testRoleFixture, _ *ExtenderCamouflageRoleSettings) {
		fixture.verifier.setFailing("dead.example")
	}, nil)
	interval := DefaultExtenderCamouflageRevalidationInterval

	retry := DefaultExtenderCamouflageRoleSettings().VerifyRetryInterval
	elapsed := time.Duration(0)
	retries := 0
	for {
		wait := fixture.schedule.nextWait(t)
		elapsed += wait
		if interval < elapsed {
			t.Fatalf("the waits ran to %s, past the interval", elapsed)
		}
		if wait != retry {
			// the wait that ends the interval
			break
		}
		retries += 1
		retry *= 2
		fixture.schedule.fireNow(t)
	}
	if retries != 6 {
		t.Fatalf("%d retries in the interval, expected 6", retries)
	}
	if elapsed < interval-interval/10 {
		t.Fatalf("the waits sum to %s, expected within a tenth under %s", elapsed, interval)
	}
}

// An empty list means no coverage, reported as no_verified_target, and no error:
// nothing is dialed or counted, no key is published, and the server runs. There is
// nothing to retry, so the next look is the interval.
func TestExtenderCamouflageRoleEmptyListIsNoCoverageNotAnError(t *testing.T) {
	for _, c := range []struct {
		name          string
		splice        bool
		expectedPhase string
	}{
		{name: "phase a", splice: false, expectedPhase: ExtenderCamouflagePhaseAuthenticatedRollout},
		{name: "phase b", splice: true, expectedPhase: ExtenderCamouflagePhaseAuthenticatedRollout},
	} {
		t.Run(c.name, func(t *testing.T) {
			installTestRoleBorrowLists(t, newTestRoleBorrowList())
			fixture := newTestRoleFixture(t, func(_ *testRoleFixture, settings *ExtenderCamouflageRoleSettings) {
				settings.Splice = c.splice
			}, nil)
			wait := fixture.schedule.nextWait(t)
			interval := DefaultExtenderCamouflageRevalidationInterval
			if interval < wait || wait < interval-interval/10 {
				t.Fatalf("wait = %s, expected the interval", wait)
			}
			if calls := fixture.verifier.takeCalls(); 0 < len(calls) {
				t.Fatalf("an empty list asked %v", calls)
			}
			status := fixture.server.server.CamouflageStatus()
			if status.Covered || status.DegradedReason != ExtenderCamouflageDegradedNoVerifiedTarget {
				t.Fatalf("status = %+v, expected no_verified_target", status)
			}
			if !status.Enabled || status.Phase != c.expectedPhase || status.VerifiedTargetCount != 0 {
				t.Fatalf("status = %+v, expected the camouflage on in %s", status, c.expectedPhase)
			}
			if count := fixture.server.server.CamouflageStats().BorrowVerifyFailCount; count != 0 {
				t.Fatalf("an empty list counted %d failures", count)
			}
			if key := fixture.role.PublishedRealityPublicKey(); key != nil {
				t.Fatal("an empty list published a key")
			}
		})
	}
}

// The key is published while at least one name is verified, and the moment none
// is, it is not (P5).
func TestExtenderCamouflageRolePublishesTheKeyOnlyWhileANameIsVerified(t *testing.T) {
	installTestRoleBorrowLists(t, newTestRoleBorrowList("a.example"))
	fixture := newTestRoleFixture(t, nil, nil)
	fixture.schedule.nextWait(t)
	if key := fixture.role.PublishedRealityPublicKey(); !bytes.Equal(key, fixture.realityPublicKey) {
		t.Fatal("a verified name did not publish the key")
	}
	fixture.verifier.setFailing("a.example")
	fixture.schedule.fireNow(t)
	fixture.schedule.nextWait(t)
	if key := fixture.role.PublishedRealityPublicKey(); key != nil {
		t.Fatal("the key was published with no verified name")
	}
	fixture.verifier.setFailing()
	fixture.schedule.fireNow(t)
	fixture.schedule.nextWait(t)
	if key := fixture.role.PublishedRealityPublicKey(); !bytes.Equal(key, fixture.realityPublicKey) {
		t.Fatal("the key did not come back with its name")
	}
}

// A pass is asked for only when a post went out with no key and a name has since
// verified: once for that, never for a name that verified before any post, and
// never for a repeat. An activation is a budget of the operator's.
func TestExtenderCamouflageRoleAsksForAPostOnlyForAKeyThatWentUnposted(t *testing.T) {
	asked := make(chan struct{}, 16)
	reactivate := func() { asked <- struct{}{} }
	requireAsked := func(t *testing.T, expected int, when string) {
		t.Helper()
		if len(asked) != expected {
			t.Fatalf("%s: %d passes asked for, expected %d", when, len(asked), expected)
		}
		for range expected {
			<-asked
		}
	}

	t.Run("the first post carried it", func(t *testing.T) {
		installTestRoleBorrowLists(t, newTestRoleBorrowList("a.example"))
		fixture := newTestRoleFixture(t, nil, nil)
		fixture.schedule.nextWait(t)
		fixture.role.PublishedRealityPublicKey()
		fixture.role.SetReactivate(reactivate)
		requireAsked(t, 0, "a name that verified before the first post")
		fixture.schedule.fireNow(t)
		fixture.schedule.nextWait(t)
		requireAsked(t, 0, "another pass over the name")
	})

	t.Run("the first post had none", func(t *testing.T) {
		installTestRoleBorrowLists(t, newTestRoleBorrowList("a.example"))
		fixture := newTestRoleFixture(t, func(fixture *testRoleFixture, _ *ExtenderCamouflageRoleSettings) {
			fixture.verifier.setFailing("a.example")
		}, nil)
		fixture.schedule.nextWait(t)
		if key := fixture.role.PublishedRealityPublicKey(); key != nil {
			t.Fatal("a post read a key with no verified name")
		}
		fixture.role.SetReactivate(reactivate)
		requireAsked(t, 0, "no name yet")

		fixture.schedule.fireNow(t)
		fixture.schedule.nextWait(t)
		requireAsked(t, 0, "a retry that failed")

		fixture.verifier.setFailing()
		fixture.schedule.fireNow(t)
		fixture.schedule.nextWait(t)
		requireAsked(t, 1, "the name verifying")

		// the key reaches the next post, and nothing more is asked
		if key := fixture.role.PublishedRealityPublicKey(); !bytes.Equal(key, fixture.realityPublicKey) {
			t.Fatal("the post asked for did not read the key")
		}
		fixture.schedule.fireNow(t)
		fixture.schedule.nextWait(t)
		requireAsked(t, 0, "another pass")
	})

	t.Run("the names came back with no post in between", func(t *testing.T) {
		installTestRoleBorrowLists(t, newTestRoleBorrowList("a.example"))
		fixture := newTestRoleFixture(t, nil, nil)
		fixture.schedule.nextWait(t)
		fixture.role.PublishedRealityPublicKey()
		fixture.role.SetReactivate(reactivate)
		fixture.verifier.setFailing("a.example")
		fixture.schedule.fireNow(t)
		fixture.schedule.nextWait(t)
		fixture.verifier.setFailing()
		fixture.schedule.fireNow(t)
		fixture.schedule.nextWait(t)
		requireAsked(t, 0, "the operator never heard the key go")
	})

	t.Run("the name verified before the activator stood", func(t *testing.T) {
		installTestRoleBorrowLists(t, newTestRoleBorrowList("a.example"))
		fixture := newTestRoleFixture(t, func(fixture *testRoleFixture, _ *ExtenderCamouflageRoleSettings) {
			fixture.verifier.setFailing("a.example")
		}, nil)
		fixture.schedule.nextWait(t)
		fixture.role.PublishedRealityPublicKey()
		fixture.verifier.setFailing()
		fixture.schedule.fireNow(t)
		fixture.schedule.nextWait(t)
		requireAsked(t, 0, "nothing to ask through")
		fixture.role.SetReactivate(reactivate)
		requireAsked(t, 1, "installing what asks")
	})
}

// A name must pass on every family this host can dial: a dual-stack host drops a
// name one family cannot reach, and does not dial on after the first failure; a
// single-stack host dials its one family; and a host with neither passes nothing
// and dials nothing.
func TestExtenderCamouflageRoleVerifiesOnEveryFamilyThisHostDials(t *testing.T) {
	cases := []struct {
		name          string
		ipVersions    []int
		expectedCalls []string
		expectedCount int
	}{
		{
			name:       "dual stack",
			ipVersions: []int{4, 6},
			expectedCalls: []string{
				"tcp4 both.example", "tcp4 v4-fails.example", "tcp4 v6-fails.example",
				"tcp6 both.example", "tcp6 v6-fails.example",
			},
			expectedCount: 1,
		},
		{
			name:          "v4 alone",
			ipVersions:    []int{4},
			expectedCalls: []string{"tcp4 both.example", "tcp4 v4-fails.example", "tcp4 v6-fails.example"},
			expectedCount: 2,
		},
		{
			name:          "v6 alone",
			ipVersions:    []int{6},
			expectedCalls: []string{"tcp6 both.example", "tcp6 v4-fails.example", "tcp6 v6-fails.example"},
			expectedCount: 2,
		},
		{
			name:          "no family",
			ipVersions:    []int{},
			expectedCalls: nil,
			expectedCount: 0,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			installTestRoleBorrowLists(t, newTestRoleBorrowList("both.example", "v4-fails.example", "v6-fails.example"))
			fixture := newTestRoleFixture(t, func(fixture *testRoleFixture, settings *ExtenderCamouflageRoleSettings) {
				fixture.verifier.setFailing("tcp4 v4-fails.example", "tcp6 v6-fails.example")
				settings.IpVersionSupported = func(ipVersion int) bool {
					return slices.Contains(c.ipVersions, ipVersion)
				}
			}, nil)
			fixture.schedule.nextWait(t)
			calls := fixture.verifier.takeCalls()
			if !slices.Equal(calls, c.expectedCalls) {
				t.Fatalf("calls = %v, expected %v", calls, c.expectedCalls)
			}
			if count := fixture.server.server.CamouflageStatus().VerifiedTargetCount; count != c.expectedCount {
				t.Fatalf("verified names = %d, expected %d", count, c.expectedCount)
			}
		})
	}
}

// With no seam the verification is the real one, a tls 1.3 handshake over the path
// the splice dials: a fake modern site passes, and one the path cannot reach fails.
func TestExtenderCamouflageRoleVerifiesOverTheSpliceDial(t *testing.T) {
	installTestRoleBorrowLists(t, newTestRoleBorrowList(testBorrowName))
	site := newFakeBorrowSite(t, tlsBorrowSiteHandler(t))
	var unreachable atomic.Bool
	fixture := newTestRoleFixture(t, func(_ *testRoleFixture, settings *ExtenderCamouflageRoleSettings) {
		settings.VerifyBorrowName = nil
		settings.VerifyTimeout = 20 * time.Second
		settings.SpliceDialContext = func(ctx context.Context, network string, address string) (net.Conn, error) {
			if network != "tcp4" || address != net.JoinHostPort(testBorrowName, "443") {
				t.Errorf("the splice dial was asked for %s %s", network, address)
			}
			if unreachable.Load() {
				return nil, errUnreachableBorrowTarget
			}
			return site.dialContext()(ctx, network, address)
		}
	}, nil)
	server := fixture.server.server

	fixture.schedule.nextWait(t)
	if site.reached.get() == 0 {
		t.Fatal("the verification did not reach the site")
	}
	if count := server.CamouflageStatus().VerifiedTargetCount; count != 1 {
		t.Fatalf("verified names = %d, expected the name a modern site answers for", count)
	}

	unreachable.Store(true)
	fixture.schedule.fireNow(t)
	fixture.schedule.nextWait(t)
	if count := server.CamouflageStatus().VerifiedTargetCount; count != 0 {
		t.Fatalf("verified names = %d, expected none once the site cannot be reached", count)
	}
	if count := server.CamouflageStats().BorrowVerifyFailCount; count != 1 {
		t.Fatalf("failed verifications = %d, expected 1", count)
	}
}

// What a pass finds reaches the running server for the next hello: once the
// records have settled a prober is spliced, a pass that loses the name sends the
// next one to the degraded terminate, and the name coming back splices again.
func TestExtenderCamouflageRoleFeedsTheRunningServer(t *testing.T) {
	installTestRoleBorrowLists(t, newTestRoleBorrowList(testBorrowName))
	site := newFakeBorrowSite(t, tlsBorrowSiteHandler(t))
	fallback := make(chan struct{}, 8)
	fixture := newTestRoleFixture(t, func(_ *testRoleFixture, settings *ExtenderCamouflageRoleSettings) {
		settings.Splice = true
		settings.SpliceDialContext = site.dialContext()
	}, func(settings *ExtenderSettings) {
		settings.CamouflageHandler = func(outcome ExtenderCamouflageOutcome) {
			if outcome.Outcome == ExtenderCamouflageOutcomeSpliceFallbackTerminated {
				fallback <- struct{}{}
			}
		}
	})
	fixture.schedule.nextWait(t)
	fixture.settleTheRecords(t)
	requireSpliced := func(when string) {
		t.Helper()
		writeProber(t, fixture.server)
		select {
		case <-site.accepted:
		case <-time.After(30 * time.Second):
			t.Fatalf("%s: a prober was not spliced", when)
		}
	}
	requireSpliced("with the name verified")

	fixture.verifier.setFailing(testBorrowName)
	fixture.schedule.fireNow(t)
	fixture.schedule.nextWait(t)
	status := fixture.server.server.CamouflageStatus()
	if status.Covered || status.DegradedReason != ExtenderCamouflageDegradedNoVerifiedTarget {
		t.Fatalf("status with the name lost = %+v", status)
	}
	writeProber(t, fixture.server)
	select {
	case <-fallback:
	case <-time.After(30 * time.Second):
		t.Fatal("a prober with no verified name did not take the degraded terminate")
	}

	fixture.verifier.setFailing()
	fixture.schedule.fireNow(t)
	fixture.schedule.nextWait(t)
	requireSpliced("with the name back")
}

// A pass that loses a name leaves a splice already relaying to it running (§3.3): a
// new list applies to new hellos, and the slot it holds is its own to release.
func TestExtenderCamouflageRoleLeavesAWorkingSpliceAlone(t *testing.T) {
	installTestRoleBorrowLists(t, newTestRoleBorrowList(testBorrowName))
	release := make(chan struct{})
	defer close(release)
	site := newFakeBorrowSite(t, blockingBorrowSiteHandler(release))
	fixture := newTestRoleFixture(t, func(_ *testRoleFixture, settings *ExtenderCamouflageRoleSettings) {
		settings.Splice = true
		settings.SpliceDialContext = site.dialContext()
	}, nil)
	fixture.schedule.nextWait(t)
	fixture.settleTheRecords(t)
	prober := writeProber(t, fixture.server)
	select {
	case <-site.accepted:
	case <-time.After(30 * time.Second):
		t.Fatal("the splice did not begin")
	}

	fixture.verifier.setFailing(testBorrowName)
	fixture.schedule.fireNow(t)
	fixture.schedule.nextWait(t)
	if count := fixture.server.server.CamouflageStatus().VerifiedTargetCount; count != 0 {
		t.Fatalf("verified names = %d, expected the name gone", count)
	}
	prober.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	_, err := prober.Read(make([]byte, 1))
	if netErr, ok := err.(net.Error); !ok || !netErr.Timeout() {
		t.Fatalf("the working splice was torn down (read err = %v), expected it to stay open", err)
	}
}

// The posture sits behind the gate: a role that starts in Phase B terminates a
// prober until its records have settled, a run-time flip cannot pass the gate of a
// role that started in Phase A, and the rollback holds whatever the gate says.
func TestExtenderCamouflageRoleKeepsThePostureBehindTheGate(t *testing.T) {
	for _, c := range []struct {
		name   string
		splice bool
	}{
		{name: "started in phase b", splice: true},
		{name: "flipped from phase a", splice: false},
	} {
		t.Run(c.name, func(t *testing.T) {
			installTestRoleBorrowLists(t, newTestRoleBorrowList(testBorrowName))
			site := newFakeBorrowSite(t, tlsBorrowSiteHandler(t))
			terminated := make(chan struct{}, 8)
			fixture := newTestRoleFixture(t, func(_ *testRoleFixture, settings *ExtenderCamouflageRoleSettings) {
				settings.Splice = c.splice
				settings.SpliceDialContext = site.dialContext()
			}, func(settings *ExtenderSettings) {
				settings.CamouflageHandler = func(outcome ExtenderCamouflageOutcome) {
					if outcome.Outcome == ExtenderCamouflageOutcomeTerminated {
						terminated <- struct{}{}
					}
				}
			})
			server := fixture.server.server
			fixture.schedule.nextWait(t)
			requireTerminated := func(when string) {
				t.Helper()
				reached := site.reached.get()
				writeProber(t, fixture.server)
				select {
				case <-terminated:
				case <-time.After(30 * time.Second):
					t.Fatalf("%s: a prober was not terminated", when)
				}
				if count := site.reached.get(); count != reached {
					t.Fatalf("%s: a prober reached the borrowed site", when)
				}
			}

			if server.CamouflageSplice() != c.splice {
				t.Fatalf("posture = %t, expected %t", server.CamouflageSplice(), c.splice)
			}
			if !c.splice {
				requireTerminated("in phase a")
				if status := server.CamouflageStatus(); status.DegradedReason != ExtenderCamouflageDegradedSpliceOff {
					t.Fatalf("status in phase a = %+v", status)
				}
				server.SetCamouflageSplice(true)
			}
			requireTerminated("before the records settle")
			if status := server.CamouflageStatus(); status.DegradedReason != ExtenderCamouflageDegradedGateClosed {
				t.Fatalf("status before the records settle = %+v", status)
			}

			fixture.settleTheRecords(t)
			writeProber(t, fixture.server)
			select {
			case <-site.accepted:
			case <-time.After(30 * time.Second):
				t.Fatal("a prober was not spliced after the records settled")
			}
			if status := server.CamouflageStatus(); !status.Covered {
				t.Fatalf("status after the records settled = %+v", status)
			}

			server.SetCamouflageSplice(false)
			requireTerminated("after the rollback")
			if status := server.CamouflageStatus(); status.Covered || status.DegradedReason != ExtenderCamouflageDegradedSpliceOff {
				t.Fatalf("status after the rollback = %+v", status)
			}
		})
	}
}

// The gate reads this role's activations: a record that stops qualifying closes it
// at once, and a record of another key never opens it.
func TestExtenderCamouflageRoleGateReadsItsActivations(t *testing.T) {
	installTestRoleBorrowLists(t, newTestRoleBorrowList(testBorrowName))
	fixture := newTestRoleFixture(t, func(_ *testRoleFixture, settings *ExtenderCamouflageRoleSettings) {
		settings.Splice = true
	}, nil)
	fixture.schedule.nextWait(t)
	settle := fixture.settleTheRecords(t)
	fixture.role.ObserveActivation(settle.activation(settle.body(func(body *protocol.ExtenderRecordBody) {
		body.RequiredCapabilities = nil
	})), settle.rootKeys)
	if fixture.role.settle.Open() {
		t.Fatal("a record without the requirement left the gate open")
	}

	_, _, otherRealityPublicKey := newCamouflageKeys(t)
	settle.renewFor(testSettleRecordLifetime + testSettleMargin)
	fixture.role.ObserveActivation(settle.activation(settle.body(func(body *protocol.ExtenderRecordBody) {
		body.RealityPublicKey = otherRealityPublicKey
	})), settle.rootKeys)
	if fixture.role.settle.Open() {
		t.Fatal("a record with another camouflage key left the gate open")
	}
}

// A role does not call an extender covered while a name its supported clients
// select is unverified, or its list is unmeasured, even when the splice can reach
// something: a prober replaying that name meets a substitution, which is degraded.
// A name no client selects for this extender costs nothing here, though its own
// prober is substituted too.
func TestExtenderCamouflageRoleReportsIncompleteCoverage(t *testing.T) {
	names := []string{"a.example", "b.example", "c.example"}
	settled := func(t *testing.T, borrowList *connect.ExtenderBorrowList) *testRoleFixture {
		t.Helper()
		installTestRoleBorrowLists(t, borrowList)
		fixture := newTestRoleFixture(t, func(_ *testRoleFixture, settings *ExtenderCamouflageRoleSettings) {
			settings.Splice = true
		}, nil)
		fixture.schedule.nextWait(t)
		fixture.settleTheRecords(t)
		return fixture
	}
	frontName := func(t *testing.T, fixture *testRoleFixture) string {
		t.Helper()
		name := connect.ExtenderCamouflageFrontName(fixture.identityPublicKey, names)
		if name == "" {
			t.Fatal("no front name")
		}
		return name
	}

	t.Run("the front name failed", func(t *testing.T) {
		fixture := settled(t, newTestRoleBorrowList(names...))
		if status := fixture.server.server.CamouflageStatus(); !status.Covered {
			t.Fatalf("status with every name verified = %+v", status)
		}
		fixture.verifier.setFailing(frontName(t, fixture))
		fixture.schedule.fireNow(t)
		fixture.schedule.nextWait(t)
		status := fixture.server.server.CamouflageStatus()
		if status.Covered || status.DegradedReason != ExtenderCamouflageDegradedCoverageIncomplete {
			t.Fatalf("status with the front name lost = %+v", status)
		}
		if status.VerifiedTargetCount != 2 || status.Phase != ExtenderCamouflagePhaseSplicePilot {
			t.Fatalf("status with the front name lost = %+v, expected the pilot on the other two", status)
		}
	})
	t.Run("another name failed", func(t *testing.T) {
		fixture := settled(t, newTestRoleBorrowList(names...))
		other := names[0]
		if other == frontName(t, fixture) {
			other = names[1]
		}
		fixture.verifier.setFailing(other)
		fixture.schedule.fireNow(t)
		fixture.schedule.nextWait(t)
		if status := fixture.server.server.CamouflageStatus(); !status.Covered {
			t.Fatalf("status with a name no client selects lost = %+v", status)
		}
	})
	t.Run("the list is unmeasured", func(t *testing.T) {
		borrowList := newTestRoleBorrowList(names...)
		borrowList.Measured = false
		fixture := settled(t, borrowList)
		status := fixture.server.server.CamouflageStatus()
		if status.Covered || status.DegradedReason != ExtenderCamouflageDegradedCoverageIncomplete {
			t.Fatalf("status on an unmeasured list = %+v", status)
		}
	})
}

// The interval is the setting, or a list's shorter one, and never under the floor,
// so neither a mistyped option nor a list can hammer the borrowed sites.
func TestExtenderCamouflageRoleRevalidationIntervalFollowsTheListWithinTheFloor(t *testing.T) {
	cases := []struct {
		name     string
		setting  time.Duration
		list     time.Duration
		expected time.Duration
	}{
		{name: "the default", expected: DefaultExtenderCamouflageRevalidationInterval},
		{name: "the setting", setting: 2 * time.Hour, expected: 2 * time.Hour},
		{name: "a longer list", setting: 2 * time.Hour, list: 3 * time.Hour, expected: 2 * time.Hour},
		{name: "a shorter list", setting: 2 * time.Hour, list: time.Hour, expected: time.Hour},
		{name: "a setting under the floor", setting: time.Second, expected: ExtenderCamouflageMinRevalidationInterval},
		{name: "a list under the floor", setting: time.Hour, list: time.Second, expected: ExtenderCamouflageMinRevalidationInterval},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			borrowList := newTestRoleBorrowList("a.example")
			borrowList.RevalidationInterval = c.list
			installTestRoleBorrowLists(t, borrowList)
			fixture := newTestRoleFixture(t, func(_ *testRoleFixture, settings *ExtenderCamouflageRoleSettings) {
				settings.RevalidationInterval = c.setting
			}, nil)
			wait := fixture.schedule.nextWait(t)
			if c.expected < wait || wait < c.expected-c.expected/10 {
				t.Fatalf("wait = %s, expected within a tenth under %s", wait, c.expected)
			}
		})
	}
}

// At most VerifyConcurrency verifications run at once, however long the list.
func TestExtenderCamouflageRoleBoundsConcurrentVerifications(t *testing.T) {
	names := []string{}
	for _, label := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
		names = append(names, label+".example")
	}
	installTestRoleBorrowLists(t, newTestRoleBorrowList(names...))
	const concurrency = 2
	var inFlight atomic.Int32
	var peak atomic.Int32
	release := make(chan struct{})
	entered := make(chan struct{}, len(names))
	fixture := newTestRoleFixture(t, func(_ *testRoleFixture, settings *ExtenderCamouflageRoleSettings) {
		settings.VerifyConcurrency = concurrency
		settings.VerifyBorrowName = func(ctx context.Context, network string, borrowName string) bool {
			count := inFlight.Add(1)
			defer inFlight.Add(-1)
			for {
				previous := peak.Load()
				if count <= previous || peak.CompareAndSwap(previous, count) {
					break
				}
			}
			entered <- struct{}{}
			<-release
			return true
		}
	}, nil)
	// the bound is full before anything is let go, so nothing below can pass it
	// by accident of timing
	for range concurrency {
		select {
		case <-entered:
		case <-time.After(30 * time.Second):
			t.Fatal("a verification never ran")
		}
	}
	close(release)
	fixture.schedule.nextWait(t)
	if got := peak.Load(); got != concurrency {
		t.Fatalf("peak = %d, expected %d", got, concurrency)
	}
	if count := fixture.server.server.CamouflageStatus().VerifiedTargetCount; count != len(names) {
		t.Fatalf("verified names = %d, expected %d", count, len(names))
	}
}

// Close ends a verification in flight and joins it, and a pass ended that way says
// nothing about the name. It is safe twice and on a role that never started.
func TestExtenderCamouflageRoleCloseEndsAVerificationInFlight(t *testing.T) {
	installTestRoleBorrowLists(t, newTestRoleBorrowList("a.example"))
	entered := make(chan struct{})
	ended := make(chan struct{})
	fixture := newTestRoleFixture(t, func(_ *testRoleFixture, settings *ExtenderCamouflageRoleSettings) {
		settings.VerifyBorrowName = func(ctx context.Context, network string, borrowName string) bool {
			close(entered)
			<-ctx.Done()
			close(ended)
			return false
		}
	}, nil)
	select {
	case <-entered:
	case <-time.After(30 * time.Second):
		t.Fatal("the verification never ran")
	}
	fixture.role.Close()
	select {
	case <-ended:
	default:
		t.Fatal("Close returned with the verification in flight")
	}
	if count := fixture.server.server.CamouflageStats().BorrowVerifyFailCount; count != 0 {
		t.Fatalf("a closed pass counted %d failures", count)
	}
	fixture.role.Close()

	seed, _, _ := newCamouflageKeys(t)
	role, err := NewExtenderCamouflageRole(seed, nil)
	if err != nil {
		t.Fatal(err)
	}
	role.Close()
}

// The wait for the first pass is bounded and ends with its context, so a role never
// holds its activation on a slow site for longer than it said.
func TestExtenderCamouflageRoleWaitForTheFirstPassIsBounded(t *testing.T) {
	installTestRoleBorrowLists(t, newTestRoleBorrowList("a.example"))
	release := make(chan struct{})
	defer close(release)
	fixture := newTestRoleFixture(t, func(_ *testRoleFixture, settings *ExtenderCamouflageRoleSettings) {
		settings.FirstVerificationTimeout = time.Millisecond
		settings.VerifyBorrowName = func(ctx context.Context, network string, borrowName string) bool {
			select {
			case <-release:
			case <-ctx.Done():
			}
			return true
		}
	}, nil)
	done := make(chan struct{})
	go func() {
		defer close(done)
		fixture.role.WaitFirstVerification(context.Background())
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the wait did not end at its timeout")
	}
	// the slow pass has published nothing yet
	if key := fixture.role.PublishedRealityPublicKey(); key != nil {
		t.Fatal("a key was published before any name verified")
	}
}

// The wait ends as soon as the first pass does.
func TestExtenderCamouflageRoleWaitForTheFirstPassEndsWithIt(t *testing.T) {
	installTestRoleBorrowLists(t, newTestRoleBorrowList("a.example"))
	fixture := newTestRoleFixture(t, func(_ *testRoleFixture, settings *ExtenderCamouflageRoleSettings) {
		settings.FirstVerificationTimeout = time.Hour
	}, nil)
	done := make(chan struct{})
	go func() {
		defer close(done)
		fixture.role.WaitFirstVerification(context.Background())
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the wait outlasted the first pass")
	}
	if key := fixture.role.PublishedRealityPublicKey(); !bytes.Equal(key, fixture.realityPublicKey) {
		t.Fatal("the first pass ended with no key to publish")
	}
}

// The standing as a role logs it never calls a degraded state coverage.
func TestExtenderCamouflageStatusLine(t *testing.T) {
	line := ExtenderCamouflageStatusLine(
		ExtenderCamouflageStatus{
			Phase:               ExtenderCamouflagePhaseSplicePilot,
			Covered:             true,
			VerifiedTargetCount: 3,
		},
		ExtenderCamouflageStats{
			BorrowVerifyFailCount:         2,
			AuthenticatedCount:            5,
			SplicedCount:                  7,
			TerminatedCount:               11,
			SpliceFallbackTerminatedCount: 13,
		},
	)
	expected := "splice_pilot, covered; 3 verified names, 2 failed verifications; 5 authenticated, 7 spliced, 11 terminated, 13 fallback terminated"
	if line != expected {
		t.Fatalf("line = %q, expected %q", line, expected)
	}
	line = ExtenderCamouflageStatusLine(
		ExtenderCamouflageStatus{
			Phase:          ExtenderCamouflagePhaseAuthenticatedRollout,
			DegradedReason: ExtenderCamouflageDegradedNoVerifiedTarget,
		},
		ExtenderCamouflageStats{},
	)
	expected = "authenticated_rollout, not covered: no_verified_target; 0 verified names, 0 failed verifications; 0 authenticated, 0 spliced, 0 terminated, 0 fallback terminated"
	if line != expected {
		t.Fatalf("line = %q, expected %q", line, expected)
	}
}
