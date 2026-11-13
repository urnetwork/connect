package extender

// The safe flip from Phase A to Phase B (EXTENDER-UPDATE2 §3.2): the gate opens
// only after the extender's own records have carried its key and the requirement
// for one record lifetime and a margin, and anything else closes it and restarts
// the wait. The clock is fake and every record is signed by a test root.

import (
	"crypto/ed25519"
	"encoding/base64"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
)

const (
	testSettleRecordLifetime = 24 * time.Hour
	testSettleMargin         = time.Hour
	// a renewal, inside every record's lifetime
	testSettleRenewal = 6 * time.Hour
)

type testSettleClock struct {
	stateLock sync.Mutex
	now       time.Time
}

func (self *testSettleClock) Now() time.Time {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.now
}

func (self *testSettleClock) advance(wait time.Duration) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.now = self.now.Add(wait)
}

type testSettleFixture struct {
	t                 *testing.T
	clock             *testSettleClock
	rootPrivateKey    ed25519.PrivateKey
	rootKeys          *connect.ExtenderRootKeySet
	identityPublicKey ed25519.PublicKey
	realityPublicKey  []byte
	settle            *ExtenderCamouflageSpliceSettle
}

func newTestSettleFixture(t *testing.T) *testSettleFixture {
	t.Helper()
	rootSeed, err := connect.NewExtenderKeySeed()
	if err != nil {
		t.Fatal(err)
	}
	rootPrivateKey, err := connect.ExtenderPrivateKeyFromSeed(rootSeed)
	if err != nil {
		t.Fatal(err)
	}
	_, identityPublicKey, realityPublicKey := newCamouflageKeys(t)
	clock := &testSettleClock{now: time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)}
	return &testSettleFixture{
		t:                 t,
		clock:             clock,
		rootPrivateKey:    rootPrivateKey,
		rootKeys:          connect.NewExtenderRootKeySet(rootPrivateKey.Public().(ed25519.PublicKey)),
		identityPublicKey: identityPublicKey,
		realityPublicKey:  realityPublicKey,
		settle: NewExtenderCamouflageSpliceSettle(identityPublicKey, realityPublicKey, &ExtenderCamouflageSpliceSettleSettings{
			Margin: testSettleMargin,
			Now:    clock.Now,
		}),
	}
}

// A body issued now, qualifying as this fixture's extender. The change makes it
// something else.
func (self *testSettleFixture) body(change func(body *protocol.ExtenderRecordBody)) *protocol.ExtenderRecordBody {
	now := self.clock.Now()
	body := &protocol.ExtenderRecordBody{
		PublicKey:            self.identityPublicKey,
		IssueTimeMs:          uint64(now.UnixMilli()),
		ExpireTimeMs:         uint64(now.Add(testSettleRecordLifetime).UnixMilli()),
		NetworkHost:          "extender.example",
		RealityPublicKey:     self.realityPublicKey,
		RequiredCapabilities: []string{connect.ExtenderCapabilityTcpCamouflageV1},
	}
	if change != nil {
		change(body)
	}
	return body
}

// The activation answer that carries the body, signed by the fixture's root.
func (self *testSettleFixture) activation(body *protocol.ExtenderRecordBody) *connect.ExtenderActivateResult {
	self.t.Helper()
	record, err := connect.SignExtenderRecord(self.rootPrivateKey, body)
	if err != nil {
		self.t.Fatal(err)
	}
	recordBytes, err := proto.Marshal(record)
	if err != nil {
		self.t.Fatal(err)
	}
	return &connect.ExtenderActivateResult{
		Activated: true,
		Record:    base64.StdEncoding.EncodeToString(recordBytes),
	}
}

func (self *testSettleFixture) requireOpen(want bool, when string) {
	self.t.Helper()
	if got := self.settle.Open(); got != want {
		self.t.Fatalf("%s: the gate is open = %t, expected %t", when, got, want)
	}
}

// Renews the qualifying record through `duration`, as the activator does.
func (self *testSettleFixture) renewFor(duration time.Duration) {
	self.t.Helper()
	for elapsed := time.Duration(0); elapsed < duration; elapsed += testSettleRenewal {
		self.settle.ObserveActivation(self.activation(self.body(nil)), self.rootKeys)
		wait := min(testSettleRenewal, duration-elapsed)
		self.clock.advance(wait)
	}
}

// With no record the gate is closed, and with a qualifying run it opens only once
// the run has lasted one record lifetime and the margin. Renewals do not restart
// the wait. It stays open while the records keep qualifying.
func TestExtenderCamouflageSpliceSettleOpensAfterOneLifetimeAndTheMargin(t *testing.T) {
	fixture := newTestSettleFixture(t)
	fixture.requireOpen(false, "before any record")

	fixture.renewFor(testSettleRecordLifetime)
	fixture.settle.ObserveActivation(fixture.activation(fixture.body(nil)), fixture.rootKeys)
	fixture.requireOpen(false, "after one lifetime and no margin")

	fixture.clock.advance(testSettleMargin - time.Second)
	fixture.requireOpen(false, "a second short of the margin")

	fixture.clock.advance(time.Second)
	fixture.requireOpen(true, "at one lifetime and the margin")

	fixture.settle.ObserveActivation(fixture.activation(fixture.body(nil)), fixture.rootKeys)
	fixture.requireOpen(true, "after another renewal")
}

// The wait is the longest lifetime in the run, so a longer lifetime the operator
// moves to extends it rather than shortening anything.
func TestExtenderCamouflageSpliceSettleWaitsTheLongestLifetime(t *testing.T) {
	fixture := newTestSettleFixture(t)
	fixture.settle.ObserveRecord(fixture.body(func(body *protocol.ExtenderRecordBody) {
		body.ExpireTimeMs = uint64(fixture.clock.Now().Add(48 * time.Hour).UnixMilli())
	}))
	fixture.clock.advance(testSettleRecordLifetime + testSettleMargin)
	fixture.settle.ObserveRecord(fixture.body(nil))
	fixture.requireOpen(false, "past the short lifetime, inside the long one")
	fixture.clock.advance(testSettleRecordLifetime)
	fixture.settle.ObserveRecord(fixture.body(nil))
	fixture.requireOpen(true, "past the long lifetime")
}

// A record that stops qualifying closes the gate at once, which is how a rollback
// reaches the extender, and the wait restarts from the next qualifying record.
func TestExtenderCamouflageSpliceSettleRestartsOnARecordThatDoesNotQualify(t *testing.T) {
	_, _, otherRealityPublicKey := newCamouflageKeys(t)
	for _, c := range []struct {
		name   string
		change func(body *protocol.ExtenderRecordBody)
	}{
		{"no key", func(body *protocol.ExtenderRecordBody) { body.RealityPublicKey = nil }},
		{"another key", func(body *protocol.ExtenderRecordBody) { body.RealityPublicKey = otherRealityPublicKey }},
		{"no requirement", func(body *protocol.ExtenderRecordBody) { body.RequiredCapabilities = nil }},
		{"another requirement", func(body *protocol.ExtenderRecordBody) { body.RequiredCapabilities = []string{"future-capability-v9"} }},
		{"no lifetime", func(body *protocol.ExtenderRecordBody) { body.ExpireTimeMs = body.IssueTimeMs }},
	} {
		t.Run(c.name, func(t *testing.T) {
			fixture := newTestSettleFixture(t)
			fixture.renewFor(testSettleRecordLifetime + testSettleMargin)
			fixture.settle.ObserveActivation(fixture.activation(fixture.body(nil)), fixture.rootKeys)
			fixture.requireOpen(true, "settled")

			fixture.settle.ObserveActivation(fixture.activation(fixture.body(c.change)), fixture.rootKeys)
			fixture.requireOpen(false, "after the record that does not qualify")

			fixture.renewFor(testSettleRecordLifetime)
			fixture.settle.ObserveActivation(fixture.activation(fixture.body(nil)), fixture.rootKeys)
			fixture.requireOpen(false, "one lifetime into the new run, without the margin")
			fixture.clock.advance(testSettleMargin)
			fixture.requireOpen(true, "the new run settled")
		})
	}
}

// An activation whose record cannot be read or verified says nothing about what
// the operator still signs: it closes the gate. So does one signed by another
// root, whatever its body claims.
func TestExtenderCamouflageSpliceSettleClosesOnARecordItCannotVerify(t *testing.T) {
	otherRootSeed, err := connect.NewExtenderKeySeed()
	if err != nil {
		t.Fatal(err)
	}
	otherRootPrivateKey, err := connect.ExtenderPrivateKeyFromSeed(otherRootSeed)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name    string
		answer  func(fixture *testSettleFixture) *connect.ExtenderActivateResult
		noRoots bool
	}{
		{name: "no answer", answer: func(fixture *testSettleFixture) *connect.ExtenderActivateResult { return nil }},
		{name: "no record", answer: func(fixture *testSettleFixture) *connect.ExtenderActivateResult {
			return &connect.ExtenderActivateResult{Activated: true}
		}},
		{name: "not base64", answer: func(fixture *testSettleFixture) *connect.ExtenderActivateResult {
			return &connect.ExtenderActivateResult{Activated: true, Record: "not base64!"}
		}},
		{name: "another root", answer: func(fixture *testSettleFixture) *connect.ExtenderActivateResult {
			record, err := connect.SignExtenderRecord(otherRootPrivateKey, fixture.body(nil))
			if err != nil {
				fixture.t.Fatal(err)
			}
			recordBytes, err := proto.Marshal(record)
			if err != nil {
				fixture.t.Fatal(err)
			}
			return &connect.ExtenderActivateResult{Activated: true, Record: base64.StdEncoding.EncodeToString(recordBytes)}
		}},
		{name: "no root keys", noRoots: true, answer: func(fixture *testSettleFixture) *connect.ExtenderActivateResult {
			return fixture.activation(fixture.body(nil))
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			fixture := newTestSettleFixture(t)
			fixture.renewFor(testSettleRecordLifetime + testSettleMargin)
			fixture.settle.ObserveActivation(fixture.activation(fixture.body(nil)), fixture.rootKeys)
			fixture.requireOpen(true, "settled")

			rootKeys := fixture.rootKeys
			if c.noRoots {
				rootKeys = nil
			}
			fixture.settle.ObserveActivation(c.answer(fixture), rootKeys)
			fixture.requireOpen(false, "after the activation it cannot verify")
		})
	}
}

// The gate closes when the latest record expires, since the operator may have
// signed anything in the gap, and a later qualifying record restarts the wait
// rather than resuming it. A record that arrives already expired starts nothing.
func TestExtenderCamouflageSpliceSettleClosesWhenTheLatestRecordExpires(t *testing.T) {
	fixture := newTestSettleFixture(t)
	fixture.renewFor(testSettleRecordLifetime + testSettleMargin)
	fixture.settle.ObserveRecord(fixture.body(nil))
	fixture.requireOpen(true, "settled")

	fixture.clock.advance(testSettleRecordLifetime)
	fixture.requireOpen(true, "at the latest record's expiry")
	fixture.clock.advance(time.Second)
	fixture.requireOpen(false, "past the latest record's expiry")

	fixture.settle.ObserveRecord(fixture.body(nil))
	fixture.requireOpen(false, "a new record after the gap")
	fixture.clock.advance(testSettleRecordLifetime + testSettleMargin)
	fixture.requireOpen(false, "the new record has expired too")

	expired := newTestSettleFixture(t)
	expired.settle.ObserveRecord(expired.body(func(body *protocol.ExtenderRecordBody) {
		body.IssueTimeMs = uint64(expired.clock.Now().Add(-48 * time.Hour).UnixMilli())
		body.ExpireTimeMs = uint64(expired.clock.Now().Add(-24 * time.Hour).UnixMilli())
	}))
	expired.clock.advance(testSettleRecordLifetime + testSettleMargin)
	expired.settle.ObserveRecord(expired.body(nil))
	expired.requireOpen(false, "the expired record started no run")
}

// A record of another identity changes nothing, so a bootstrap record of a peer
// neither starts the wait nor closes the gate.
func TestExtenderCamouflageSpliceSettleIgnoresAnotherIdentity(t *testing.T) {
	fixture := newTestSettleFixture(t)
	_, otherIdentityPublicKey, _ := newCamouflageKeys(t)
	other := func(body *protocol.ExtenderRecordBody) { body.PublicKey = otherIdentityPublicKey }

	fixture.settle.ObserveRecord(fixture.body(other))
	fixture.clock.advance(testSettleRecordLifetime + testSettleMargin)
	fixture.settle.ObserveRecord(fixture.body(nil))
	fixture.requireOpen(false, "another identity's record started the wait")

	fixture.renewFor(testSettleRecordLifetime + testSettleMargin)
	fixture.settle.ObserveRecord(fixture.body(nil))
	fixture.requireOpen(true, "settled")
	fixture.settle.ObserveRecord(fixture.body(func(body *protocol.ExtenderRecordBody) {
		other(body)
		body.RealityPublicKey = nil
	}))
	fixture.requireOpen(true, "another identity's record closed the gate")
}

// An extender with no camouflage key has nothing to authenticate with, so no
// record opens its gate, not even one with an empty key.
func TestExtenderCamouflageSpliceSettleNeverOpensWithoutAKey(t *testing.T) {
	fixture := newTestSettleFixture(t)
	settle := NewExtenderCamouflageSpliceSettle(fixture.identityPublicKey, nil, &ExtenderCamouflageSpliceSettleSettings{
		Margin: testSettleMargin,
		Now:    fixture.clock.Now,
	})
	settle.ObserveRecord(fixture.body(func(body *protocol.ExtenderRecordBody) { body.RealityPublicKey = nil }))
	fixture.clock.advance(testSettleRecordLifetime + testSettleMargin)
	settle.ObserveRecord(fixture.body(func(body *protocol.ExtenderRecordBody) { body.RealityPublicKey = nil }))
	if settle.Open() {
		t.Fatal("the gate of an extender with no camouflage key opened")
	}
}

// As the extender's CamouflageSpliceGate: with the posture on, a prober is
// terminated until the records settle and spliced after, and a record that stops
// qualifying sends the next prober back to Phase A.
func TestExtenderCamouflageSpliceSettleGatesTheSplice(t *testing.T) {
	seed, identityPublicKey, realityPublicKey := newCamouflageKeys(t)
	fixture := newTestSettleFixture(t)
	fixture.identityPublicKey = identityPublicKey
	fixture.realityPublicKey = realityPublicKey
	settle := NewExtenderCamouflageSpliceSettle(identityPublicKey, realityPublicKey, &ExtenderCamouflageSpliceSettleSettings{
		Margin: testSettleMargin,
		Now:    fixture.clock.Now,
	})
	fixture.settle = settle

	site := newFakeBorrowSite(t, tlsBorrowSiteHandler(t))
	terminated := make(chan struct{}, 8)
	server := newExtenderFixture(t, "127.0.0.1", func(settings *ExtenderSettings) {
		settings.CamouflageEnabled = true
		settings.IdentityKeySeed = seed
		settings.ExtenderCamouflageSplice = true
		settings.CamouflageBorrowNames = []string{testBorrowName}
		settings.CamouflageSpliceDialContext = site.dialContext()
		settings.CamouflageSpliceGate = settle.Open
		settings.CamouflageHandler = func(outcome ExtenderCamouflageOutcome) {
			if outcome.Outcome == ExtenderCamouflageOutcomeTerminated {
				terminated <- struct{}{}
			}
		}
	})
	requireTerminated := func(when string) {
		t.Helper()
		writeProber(t, server)
		select {
		case <-terminated:
		case <-time.After(10 * time.Second):
			t.Fatalf("%s: a prober was not terminated", when)
		}
	}

	requireTerminated("before any record")
	if status := server.server.CamouflageStatus(); status.Covered || status.DegradedReason != ExtenderCamouflageDegradedGateClosed {
		t.Fatalf("status before the records settled = %+v", status)
	}
	if count := site.reached.get(); count != 0 {
		t.Fatalf("a prober reached the borrowed site %d times before the records settled", count)
	}

	// a qualifying record alone is not enough: legacy clients still hold records
	// issued without the requirement
	settle.ObserveActivation(fixture.activation(fixture.body(nil)), fixture.rootKeys)
	requireTerminated("on the first qualifying record")
	if count := site.reached.get(); count != 0 {
		t.Fatalf("a prober reached the borrowed site %d times before the records settled", count)
	}

	fixture.renewFor(testSettleRecordLifetime + testSettleMargin)
	settle.ObserveActivation(fixture.activation(fixture.body(nil)), fixture.rootKeys)
	writeProber(t, server)
	select {
	case <-site.accepted:
	case <-time.After(10 * time.Second):
		t.Fatal("a prober was not spliced after the records settled")
	}

	settle.ObserveActivation(fixture.activation(fixture.body(func(body *protocol.ExtenderRecordBody) {
		body.RequiredCapabilities = nil
	})), fixture.rootKeys)
	reached := site.reached.get()
	requireTerminated("after the requirement came out")
	if count := site.reached.get(); count != reached {
		t.Fatal("a prober was spliced after the requirement came out")
	}
}
