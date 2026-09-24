package connect

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"fmt"
	"math"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// The extender peer pinger (GEOMAP §2.1) over the ping seam and a fake clock:
// the spread, the refresh, the concurrency bound, the counters and ring, the
// reports and the join.

// The pinger's clock, counting its reads. With no ping in flight the pinger
// reads it once per pass, so a count of reads is a pass barrier that needs no
// sleep.
type testPingerClock struct {
	*testClock
	stateLock sync.Mutex
	readCount int
	read      chan struct{}
}

// A pinger clock on a fresh fake clock.
func newTestPingerClock() *testPingerClock {
	return &testPingerClock{
		testClock: newTestClock(),
		read:      make(chan struct{}, 1),
	}
}

// Reads the fake clock and counts the read.
func (self *testPingerClock) Now() time.Time {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.readCount += 1
	}()
	select {
	case self.read <- struct{}{}:
	default:
	}
	return self.testClock.Now()
}

// The reads so far.
func (self *testPingerClock) reads() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.readCount
}

// Waits for `count` more reads; the first of the passes they belong to has
// then completed.
func (self *testPingerClock) waitForReads(t *testing.T, count int) {
	t.Helper()
	target := self.reads() + count
	deadline := time.After(10 * time.Second)
	for self.reads() < target {
		select {
		case <-self.read:
		case <-deadline:
			t.Fatalf("the pinger read its clock %d times, expected %d", self.reads(), target)
		}
	}
}

// The ping seam: one fabricated probe per call, by ip -- what the peer
// answered, or a failed dial -- with the in-flight count and an optional
// gate every call waits on.
type testPeerPings struct {
	stateLock   sync.Mutex
	outcomes    map[string]ExtenderPingOutcome
	rtts        map[string][]time.Duration
	fails       map[string]bool
	calls       []string
	attestors   []*ExtenderProbeAttestor
	inFlight    int
	maxInFlight int
	gate        chan struct{}
	called      chan struct{}
}

// A ping seam where every peer co-signs nothing until the test says.
func newTestPeerPings() *testPeerPings {
	return &testPeerPings{
		outcomes: map[string]ExtenderPingOutcome{},
		rtts:     map[string][]time.Duration{},
		fails:    map[string]bool{},
		called:   make(chan struct{}, 1),
	}
}

// One fabricated probe of one carrier, as the test scripted it for the ip.
func (self *testPeerPings) ping(
	ctx context.Context,
	extenderConfig *ExtenderConfig,
	attestor *ExtenderProbeAttestor,
) (*ExtenderLatencyProbe, error) {
	ip := extenderConfig.Ip.String()
	self.stateLock.Lock()
	callIndex := 0
	for _, call := range self.calls {
		if call == ip {
			callIndex += 1
		}
	}
	self.calls = append(self.calls, ip)
	self.attestors = append(self.attestors, attestor)
	self.inFlight += 1
	self.maxInFlight = max(self.maxInFlight, self.inFlight)
	gate := self.gate
	outcome := self.outcomes[ip]
	rtts := self.rtts[ip]
	fails := self.fails[ip]
	self.stateLock.Unlock()
	defer func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.inFlight -= 1
	}()
	select {
	case self.called <- struct{}{}:
	default:
	}

	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if fails {
		return nil, fmt.Errorf("no route to %s in this test", ip)
	}
	rtt := 10 * time.Millisecond
	if 0 < len(rtts) {
		rtt = rtts[min(callIndex, len(rtts)-1)]
	}
	probe := &ExtenderLatencyProbe{
		Rtt:      rtt,
		Response: &protocol.ExtenderResponse{PublicKey: slices.Clone(extenderConfig.PublicKey)},
	}
	if attestor == nil || outcome == ExtenderPingUnattested {
		return probe, nil
	}
	nonce, err := NewExtenderProbeNonce()
	if err != nil {
		return nil, err
	}
	attestation := &protocol.ExtenderProbeAttestation{
		PingerExtenderPublicKey: slices.Clone(attestor.ExtenderPublicKey),
		ExtenderPublicKey:       slices.Clone(extenderConfig.PublicKey),
		ProbeNonce:              nonce,
		RttMs:                   extenderProbeRttMs(rtt),
		TimestampMs:             uint64(time.Now().UnixMilli()),
	}
	if err := SignExtenderProbeAttestation(attestor, attestation); err != nil {
		return nil, err
	}
	probe.Attested = true
	probe.Attestation = attestation
	probe.Outcome = outcome
	switch outcome {
	case ExtenderPingCosigned:
		probe.Cosigned = true
		probe.Verdict = &protocol.ExtenderProbeVerdict{Accepted: true, Cosignature: make([]byte, 64)}
	case ExtenderPingRejected:
		probe.Verdict = &protocol.ExtenderProbeVerdict{Reason: ExtenderProbeVerdictReasonUnknownPinger}
		probe.Reason = ExtenderProbeVerdictReasonUnknownPinger
	}
	return probe, nil
}

// The ips pinged so far, in the order the pings started.
func (self *testPeerPings) callsValue() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.calls)
}

// The pings in flight now, and the most there ever were.
func (self *testPeerPings) inFlightValue() (int, int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.inFlight, self.maxInFlight
}

// One extender's view: its directory with its own record, its attestor and a
// reporter that posts every ping at once.
type testPeerPingerFixture struct {
	clock          *testPingerClock
	directory      *ExtenderDirectory
	rootPrivateKey ed25519.PrivateKey
	ownPublicKey   ed25519.PublicKey
	attestor       *ExtenderProbeAttestor
	pings          *testPeerPings
	posts          *testPingPosts
	reporter       *ExtenderPingReporter
	// every peer's key by its ip
	ipKeys map[string]ed25519.PublicKey
}

// A fixture with this extender's own record at ownIp, or none when empty.
func newTestPeerPingerFixture(t *testing.T, ownIp string) *testPeerPingerFixture {
	t.Helper()
	clock := newTestPingerClock()
	directory, rootPrivateKey := newTestExtenderDirectory(t, clock.testClock, nil)
	attestor, ownPublicKey := newTestPeerProbeAttestor(t)
	posts := newTestPingPosts()
	reporter := newTestPingReporter(t, posts, func(settings *ExtenderPingReporterSettings) {
		settings.MaxBatchCount = 1
	})
	fixture := &testPeerPingerFixture{
		clock:          clock,
		directory:      directory,
		rootPrivateKey: rootPrivateKey,
		ownPublicKey:   ownPublicKey,
		attestor:       attestor,
		pings:          newTestPeerPings(),
		posts:          posts,
		reporter:       reporter,
		ipKeys:         map[string]ed25519.PublicKey{},
	}
	if ownIp != "" {
		fixture.applyRecord(t, ownPublicKey, 30*24*time.Hour, ownIp)
	}
	return fixture
}

// Signs and applies a record of the key at the ips, newer than the last.
func (self *testPeerPingerFixture) applyRecord(
	t *testing.T,
	publicKey ed25519.PublicKey,
	expireAfter time.Duration,
	ips ...string,
) {
	t.Helper()
	addresses := []*protocol.ExtenderAddress{}
	for _, ip := range ips {
		addresses = append(addresses, testExtenderAddress(ip, ExtenderCarrierTcp, ExtenderCarrierQuic))
	}
	// every record is newer than the one before, so a re-issue wins
	self.clock.advance(time.Millisecond)
	record := signTestRecord(
		t,
		self.rootPrivateKey,
		publicKey,
		self.clock.testClock.Now(),
		self.clock.testClock.Now().Add(expireAfter),
		addresses...,
	)
	if _, err := self.directory.ApplyRecord(record, ExtenderSourceGossip); err != nil {
		t.Fatal(err)
	}
}

// A peer with a fresh key at the ips.
func (self *testPeerPingerFixture) addPeer(t *testing.T, ips ...string) ed25519.PublicKey {
	t.Helper()
	publicKey := newTestExtenderKey(t)
	self.applyRecord(t, publicKey, 30*24*time.Hour, ips...)
	for _, ip := range ips {
		self.ipKeys[ip] = publicKey
	}
	return publicKey
}

// Starts a pinger on the fixture, with configure run on its settings first.
func (self *testPeerPingerFixture) start(
	t *testing.T,
	configure func(settings *ExtenderPeerPingerSettings),
) *ExtenderPeerPinger {
	t.Helper()
	settings := DefaultExtenderPeerPingerSettings()
	settings.OwnPublicKey = self.ownPublicKey
	settings.Attestor = self.attestor
	settings.Reporter = self.reporter
	settings.Now = self.clock.Now
	settings.PassTimeout = time.Millisecond
	settings.Ping = self.pings.ping
	settings.IpVersionSupported = func(ipVersion int) bool { return true }
	settings.Jitter = 0
	if configure != nil {
		configure(settings)
	}
	ctx, cancel := context.WithCancel(context.Background())
	pinger := NewExtenderPeerPinger(ctx, nil, self.directory, settings)
	t.Cleanup(func() {
		pinger.Close()
		cancel()
	})
	return pinger
}

// Waits for a status the predicate accepts.
func waitForPeerPingerStatus(
	t *testing.T,
	pinger *ExtenderPeerPinger,
	description string,
	predicate func(status ExtenderPeerPingerStatus) bool,
) ExtenderPeerPingerStatus {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		status, change := pinger.StatusMonitor().Get()
		if predicate(status) {
			return status
		}
		select {
		case <-change:
		case <-deadline:
			t.Fatalf("the pinger never reached %s: %+v", description, status)
		}
	}
}

// Waits for at least count pings.
func waitForPingCount(t *testing.T, pinger *ExtenderPeerPinger, count int) ExtenderPeerPingerStatus {
	t.Helper()
	return waitForPeerPingerStatus(t, pinger, fmt.Sprintf("%d pings", count), func(status ExtenderPeerPingerStatus) bool {
		return count <= status.PingCount
	})
}

// No ping is in flight and none was made past the given count, established
// over a pass barrier rather than a sleep.
func assertNoNewPing(t *testing.T, fixture *testPeerPingerFixture, pinger *ExtenderPeerPinger, callCount int) {
	t.Helper()
	fixture.clock.waitForReads(t, 3)
	pinger.stateLock.Lock()
	pinging := []string{}
	for keyHex, peer := range pinger.peers {
		if peer.pinging {
			pinging = append(pinging, keyHex)
		}
	}
	pinger.stateLock.Unlock()
	if 0 < len(pinging) {
		t.Fatalf("pings in flight: %v", pinging)
	}
	if calls := fixture.pings.callsValue(); len(calls) != callCount {
		t.Fatalf("calls = %v, expected %d", calls, callCount)
	}
}

// A seam over a script of draws, repeating the last.
func testRandomScript(values ...float64) func() float64 {
	var stateLock sync.Mutex
	i := 0
	return func() float64 {
		stateLock.Lock()
		defer stateLock.Unlock()
		value := values[min(i, len(values)-1)]
		i += 1
		return value
	}
}

// The keys of the peers in the order their draws are taken: by hex key.
func testSortedPeerIps(fixture *testPeerPingerFixture, ips ...string) []string {
	sorted := slices.Clone(ips)
	slices.SortFunc(sorted, func(a string, b string) int {
		aHex := hex.EncodeToString(fixture.ipKeys[a])
		bHex := hex.EncodeToString(fixture.ipKeys[b])
		switch {
		case aHex < bHex:
			return -1
		case bHex < aHex:
			return 1
		default:
			return 0
		}
	})
	return sorted
}

// A newly seen peer is pinged at its draw within the spread, not at once and
// not together with the others.
func TestExtenderPeerPingerSpreadsNewPeers(t *testing.T) {
	fixture := newTestPeerPingerFixture(t, "192.0.2.1")
	ips := []string{"192.0.2.11", "192.0.2.12", "192.0.2.13"}
	for _, ip := range ips {
		fixture.addPeer(t, ip)
	}
	startTime := fixture.clock.testClock.Now()
	pinger := fixture.start(t, func(settings *ExtenderPeerPingerSettings) {
		settings.SpreadTimeout = time.Hour
		settings.ProbeCount = 1
		settings.Random = testRandomScript(0.25, 0.5, 0.75, 0.5)
	})
	sortedIps := testSortedPeerIps(fixture, ips...)

	// nothing is due at the start
	waitForPeerPingerStatus(t, pinger, "three peers", func(status ExtenderPeerPingerStatus) bool {
		return status.PeerCount == 3
	})
	assertNoNewPing(t, fixture, pinger, 0)

	for i, ip := range sortedIps {
		dueTime := startTime.Add(time.Duration(i+1) * 15 * time.Minute)
		// just before its draw nothing more is pinged
		fixture.clock.testClock.advance(dueTime.Sub(fixture.clock.testClock.Now()) - time.Second)
		assertNoNewPing(t, fixture, pinger, i)
		fixture.clock.testClock.advance(time.Second)
		waitForPingCount(t, pinger, i+1)
		calls := fixture.pings.callsValue()
		if len(calls) != i+1 || calls[i] != ip {
			t.Fatalf("calls = %v, expected %s at its draw", calls, ip)
		}
		records := pinger.Records()
		if record := records[len(records)-1]; !record.Time.Equal(dueTime) || record.Ip.String() != ip {
			t.Fatalf("record = %+v, expected %s at %s", record, ip, dueTime)
		}
	}
	// every first ping fell within the spread
	for _, record := range pinger.Records() {
		if startTime.Add(time.Hour).Before(record.Time) {
			t.Fatalf("a first ping at %s is past the spread", record.Time)
		}
	}
}

// With the default draws every new peer still lands within the spread.
func TestExtenderPeerPingerSpreadBoundsTheFirstPing(t *testing.T) {
	fixture := newTestPeerPingerFixture(t, "192.0.2.1")
	for i := range 12 {
		fixture.addPeer(t, fmt.Sprintf("192.0.2.%d", 20+i))
	}
	startTime := fixture.clock.testClock.Now()
	pinger := fixture.start(t, func(settings *ExtenderPeerPingerSettings) {
		settings.SpreadTimeout = time.Hour
		settings.ProbeCount = 1
		settings.Concurrency = 12
	})
	waitForPeerPingerStatus(t, pinger, "twelve peers", func(status ExtenderPeerPingerStatus) bool {
		return status.PeerCount == 12
	})
	pinger.stateLock.Lock()
	for keyHex, peer := range pinger.peers {
		if peer.dueTime.Before(startTime) || startTime.Add(time.Hour).Before(peer.dueTime) {
			pinger.stateLock.Unlock()
			t.Fatalf("%s is due at %s, outside the spread from %s", keyHex, peer.dueTime, startTime)
		}
	}
	pinger.stateLock.Unlock()
	fixture.clock.testClock.advance(time.Hour)
	waitForPingCount(t, pinger, 12)
}

// The extender's own record is never pinged, and a revoked or expired peer is
// not a peer.
func TestExtenderPeerPingerSkipsItselfAndInactivePeers(t *testing.T) {
	fixture := newTestPeerPingerFixture(t, "192.0.2.1")
	fixture.addPeer(t, "192.0.2.11")
	revokedKey := fixture.addPeer(t, "192.0.2.12")
	if _, err := fixture.directory.ApplyRevocation(signTestRevocation(t, fixture.rootPrivateKey, revokedKey, fixture.clock.testClock.Now())); err != nil {
		t.Fatal(err)
	}
	expiringKey := newTestExtenderKey(t)
	fixture.applyRecord(t, expiringKey, time.Minute, "192.0.2.13")
	fixture.clock.testClock.advance(time.Minute + fixture.directory.settings.RecordExpireSkew + time.Second)
	if !fixture.directory.AddressUsable(netip.MustParseAddr("192.0.2.13")) {
		t.Fatal("the expired peer is not retained, so this test proves nothing")
	}

	pinger := fixture.start(t, func(settings *ExtenderPeerPingerSettings) {
		settings.SpreadTimeout = 0
		settings.ProbeCount = 1
	})
	status := waitForPingCount(t, pinger, 1)
	if status.PeerCount != 1 {
		t.Fatalf("peers = %d, expected only the active peer", status.PeerCount)
	}
	assertNoNewPing(t, fixture, pinger, 1)
	if calls := fixture.pings.callsValue(); calls[0] != "192.0.2.11" {
		t.Fatalf("calls = %v", calls)
	}
}

// A peer is pinged again the refresh timeout after its last ping, and the
// jitter keeps every refresh inside its window.
func TestExtenderPeerPingerRefreshes(t *testing.T) {
	fixture := newTestPeerPingerFixture(t, "192.0.2.1")
	fixture.addPeer(t, "192.0.2.11")
	pinger := fixture.start(t, func(settings *ExtenderPeerPingerSettings) {
		settings.SpreadTimeout = 0
		settings.RefreshTimeout = 12 * time.Hour
		settings.ProbeCount = 1
	})
	waitForPingCount(t, pinger, 1)
	firstTime := pinger.Records()[0].Time

	fixture.clock.testClock.advance(12*time.Hour - time.Second)
	assertNoNewPing(t, fixture, pinger, 1)
	fixture.clock.testClock.advance(time.Second)
	waitForPingCount(t, pinger, 2)
	if secondTime := pinger.Records()[1].Time; secondTime.Sub(firstTime) != 12*time.Hour {
		t.Fatalf("refreshed after %s, expected 12h", secondTime.Sub(firstTime))
	}
}

// A refresh stays within the jitter of the refresh timeout and under a day,
// and a spread within the spread, whatever the seam draws.
func TestExtenderPeerPingerJitterWindow(t *testing.T) {
	for _, c := range []struct {
		random float64
		want   time.Duration
	}{
		{random: 0, want: time.Duration(float64(12*time.Hour) * 0.9)},
		{random: 0.5, want: 12 * time.Hour},
		{random: math.Nextafter(1, 0), want: 0},
		{random: 1, want: 0},
		{random: -1, want: time.Duration(float64(12*time.Hour) * 0.9)},
	} {
		pinger := &ExtenderPeerPinger{
			settings: &ExtenderPeerPingerSettings{
				RefreshTimeout: 12 * time.Hour,
				SpreadTimeout:  time.Hour,
				Jitter:         0.1,
				Random:         testRandomScript(c.random),
			},
		}
		refresh := pinger.refreshTimeoutWithLock()
		if refresh < time.Duration(float64(12*time.Hour)*0.9) || time.Duration(float64(12*time.Hour)*1.1) < refresh {
			t.Fatalf("random %v: refresh %s outside 12h +- 10%%", c.random, refresh)
		}
		if 24*time.Hour <= refresh {
			t.Fatalf("random %v: refresh %s is not under a day", c.random, refresh)
		}
		if 0 < c.want && refresh != c.want {
			t.Fatalf("random %v: refresh %s, expected %s", c.random, refresh, c.want)
		}
		if spread := pinger.spreadTimeoutWithLock(); spread < 0 || time.Hour <= spread {
			t.Fatalf("random %v: spread %s outside [0, 1h)", c.random, spread)
		}
	}
	// a jitter that would let a refresh collapse or double is the default
	settings := DefaultExtenderPeerPingerSettings()
	for _, jitter := range []float64{-0.1, 1, 1.5, math.NaN()} {
		settings.Jitter = jitter
		pinger := NewExtenderPeerPinger(context.Background(), nil, NewExtenderDirectoryWithDefaults(t.Context()), settings)
		if pinger.settings.Jitter != DefaultExtenderPeerPingerSettings().Jitter {
			t.Fatalf("jitter %v was kept", jitter)
		}
		pinger.Close()
	}
}

// At most Concurrency pings are in flight, and the rest wait for a slot.
func TestExtenderPeerPingerBoundsConcurrency(t *testing.T) {
	fixture := newTestPeerPingerFixture(t, "192.0.2.1")
	for i := range 5 {
		fixture.addPeer(t, fmt.Sprintf("192.0.2.%d", 11+i))
	}
	fixture.pings.gate = make(chan struct{})
	pinger := fixture.start(t, func(settings *ExtenderPeerPingerSettings) {
		settings.SpreadTimeout = 0
		settings.Concurrency = 2
		settings.ProbeCount = 1
	})
	deadline := time.After(10 * time.Second)
	for {
		if inFlight, _ := fixture.pings.inFlightValue(); inFlight == 2 {
			break
		}
		select {
		case <-fixture.pings.called:
		case <-deadline:
			t.Fatal("two pings never started")
		}
	}
	// the loop keeps passing with every peer due and both slots full
	fixture.clock.waitForReads(t, 5)
	if inFlight, maxInFlight := fixture.pings.inFlightValue(); inFlight != 2 || maxInFlight != 2 {
		t.Fatalf("in flight %d, at most %d, expected 2", inFlight, maxInFlight)
	}
	close(fixture.pings.gate)
	waitForPingCount(t, pinger, 5)
	if _, maxInFlight := fixture.pings.inFlightValue(); maxInFlight != 2 {
		t.Fatalf("at most %d in flight, expected 2", maxInFlight)
	}
	if calls := fixture.pings.callsValue(); len(calls) != 5 {
		t.Fatalf("calls = %v", calls)
	}
}

// Every outcome is counted, the ring keeps the newest, and every attested
// probe is reported -- a refusal and a missing verdict as much as a
// co-signature -- while an unattested or failed ping reports nothing.
func TestExtenderPeerPingerCountsRingsAndReports(t *testing.T) {
	fixture := newTestPeerPingerFixture(t, "192.0.2.1")
	outcomes := map[string]ExtenderPingOutcome{
		"192.0.2.11": ExtenderPingCosigned,
		"192.0.2.12": ExtenderPingRejected,
		"192.0.2.13": ExtenderPingUnknown,
		"192.0.2.14": ExtenderPingUnattested,
		"192.0.2.15": ExtenderPingUnattested,
	}
	for ip, outcome := range outcomes {
		fixture.addPeer(t, ip)
		fixture.pings.outcomes[ip] = outcome
	}
	fixture.pings.fails["192.0.2.15"] = true
	fixture.pings.rtts["192.0.2.11"] = []time.Duration{40 * time.Millisecond, 30 * time.Millisecond}
	pinger := fixture.start(t, func(settings *ExtenderPeerPingerSettings) {
		settings.SpreadTimeout = 0
		settings.Concurrency = 1
		settings.ProbeCount = 2
		settings.RecordCount = 3
	})

	status := waitForPingCount(t, pinger, 5)
	expected := ExtenderPeerPingerStatus{
		PeerCount:        5,
		SampleSize:       64,
		SampledPeerCount: 5,
		PingCount:        5,
		CosignedCount:    1,
		RejectedCount:    1,
		UnknownCount:     1,
		UnattestedCount:  1,
		FailedCount:      1,
		LastPingTime:     fixture.clock.testClock.Now(),
	}
	if status != expected {
		t.Fatalf("status = %+v, expected %+v", status, expected)
	}

	// the ring holds the newest three, in the order pinged -- the peers come
	// due together, and go in key order
	pingOrder := testSortedPeerIps(fixture, "192.0.2.11", "192.0.2.12", "192.0.2.13", "192.0.2.14", "192.0.2.15")
	records := pinger.Records()
	if len(records) != 3 {
		t.Fatalf("ring = %d records, expected 3", len(records))
	}
	for i, record := range records {
		ip := pingOrder[2+i]
		if record.Ip.String() != ip || !slices.Equal(record.TargetPublicKey, fixture.ipKeys[ip]) {
			t.Fatalf("record %d = %s, expected %s", i, record.Ip, ip)
		}
		switch {
		case fixture.pings.fails[ip]:
			if record.Rtt != 0 || record.Outcome != ExtenderPingUnattested {
				t.Fatalf("the failed ping is recorded as %+v", record)
			}
		default:
			if record.Outcome != outcomes[ip] || record.Rtt <= 0 {
				t.Fatalf("%s is recorded as %+v", ip, record)
			}
		}
		if outcomes[ip] == ExtenderPingRejected && record.Reason != ExtenderProbeVerdictReasonUnknownPinger {
			t.Fatalf("the refusal lost its reason: %+v", record)
		}
	}
	// the records are the caller's
	records[0].TargetPublicKey[0] ^= 1
	if slices.Equal(pinger.Records()[0].TargetPublicKey, records[0].TargetPublicKey) {
		t.Fatal("the records alias the ring")
	}

	// two probes of each of the three that attested
	reports := fixture.posts.waitForReports(t, 6)
	waitForPingPendingCount(t, fixture.reporter, 0)
	if reports = fixture.posts.allReports(); len(reports) != 6 {
		t.Fatalf("reports = %d, expected 6", len(reports))
	}
	reportOutcomes := map[string][]ExtenderPingOutcome{}
	for _, report := range reports {
		if report.PingerKind != ExtenderPingerKindExtender ||
			report.PingerExtenderPublicKeyHex != hex.EncodeToString(fixture.ownPublicKey) ||
			report.PingerClientId != "" {
			t.Fatalf("a report names another pinger: %+v", report)
		}
		attestation, err := report.Proto()
		if err != nil {
			t.Fatal(err)
		}
		if !VerifyExtenderProbeAttestation(fixture.ownPublicKey, attestation) {
			t.Fatal("a reported claim does not verify under this extender's key")
		}
		for ip, key := range fixture.ipKeys {
			if hex.EncodeToString(key) == report.TargetExtenderPublicKeyHex {
				reportOutcomes[ip] = append(reportOutcomes[ip], report.Outcome)
			}
		}
	}
	for _, ip := range []string{"192.0.2.11", "192.0.2.12", "192.0.2.13"} {
		if !slices.Equal(reportOutcomes[ip], []ExtenderPingOutcome{outcomes[ip], outcomes[ip]}) {
			t.Fatalf("%s reported %v", ip, reportOutcomes[ip])
		}
	}
	for _, ip := range []string{"192.0.2.14", "192.0.2.15"} {
		if 0 < len(reportOutcomes[ip]) {
			t.Fatalf("%s reported %v", ip, reportOutcomes[ip])
		}
	}
	// every probe carried this extender's attestor
	for _, attestor := range fixture.pings.attestors {
		if attestor == nil || !slices.Equal(attestor.ExtenderPublicKey, fixture.ownPublicKey) {
			t.Fatal("a ping did not attest as this extender")
		}
	}
}

// A ping is the shared carrier walk: its lowest rtt is the directory's sample,
// marked attested only when co-signed, and a peer no carrier of which
// answered is held like any failed dial.
func TestExtenderPeerPingerRecordsTheDirectorySample(t *testing.T) {
	fixture := newTestPeerPingerFixture(t, "192.0.2.1")
	fixture.addPeer(t, "192.0.2.11")
	fixture.addPeer(t, "192.0.2.12")
	fixture.addPeer(t, "192.0.2.13")
	fixture.pings.outcomes["192.0.2.11"] = ExtenderPingCosigned
	fixture.pings.rtts["192.0.2.11"] = []time.Duration{40 * time.Millisecond, 30 * time.Millisecond}
	fixture.pings.outcomes["192.0.2.12"] = ExtenderPingRejected
	fixture.pings.fails["192.0.2.13"] = true
	pinger := fixture.start(t, func(settings *ExtenderPeerPingerSettings) {
		settings.SpreadTimeout = 0
		settings.ProbeCount = 2
	})
	waitForPingCount(t, pinger, 3)

	cosigned := testDirectoryEntry(t, fixture.directory, netip.MustParseAddr("192.0.2.11"))
	if cosigned.Latency != 30*time.Millisecond || cosigned.SuccessCount != 1 {
		t.Fatalf("the co-signed peer = %+v", cosigned)
	}
	candidates := fixture.directory.Candidates(4, 8)
	attested := map[string]bool{}
	for _, candidate := range candidates {
		attested[candidate.Ip.String()] = candidate.LatencyAttested
	}
	if !attested["192.0.2.11"] || attested["192.0.2.12"] {
		t.Fatalf("attested = %v, expected only the co-signed peer", attested)
	}
	// the failed peer failed every carrier, and is held
	failed := testDirectoryEntry(t, fixture.directory, netip.MustParseAddr("192.0.2.13"))
	if failed.FailureCount != 2 || failed.State != ExtenderStateHold {
		t.Fatalf("the failed peer = %+v", failed)
	}
	calls := 0
	for _, call := range fixture.pings.callsValue() {
		if call == "192.0.2.13" {
			calls += 1
		}
	}
	if calls != 2 {
		t.Fatalf("the failed peer was dialed %d times, expected once per carrier", calls)
	}
}

// One ping per address family the peer lists, and only the families this
// host has.
func TestExtenderPeerPingerPingsEachFamily(t *testing.T) {
	for _, c := range []struct {
		name    string
		ipv6    bool
		wantIps []string
	}{
		{name: "dual stack", ipv6: true, wantIps: []string{"192.0.2.11", "2001:db8::11"}},
		{name: "v4 only", ipv6: false, wantIps: []string{"192.0.2.11"}},
	} {
		fixture := newTestPeerPingerFixture(t, "192.0.2.1")
		fixture.addPeer(t, "192.0.2.11", "2001:db8::11")
		pinger := fixture.start(t, func(settings *ExtenderPeerPingerSettings) {
			settings.SpreadTimeout = 0
			settings.ProbeCount = 1
			settings.IpVersionSupported = func(ipVersion int) bool {
				return ipVersion == 4 || c.ipv6
			}
		})
		waitForPingCount(t, pinger, len(c.wantIps))
		assertNoNewPing(t, fixture, pinger, len(c.wantIps))
		if calls := fixture.pings.callsValue(); !slices.Equal(calls, c.wantIps) {
			t.Fatalf("%s: calls = %v, expected %v", c.name, calls, c.wantIps)
		}
		if status := pinger.Status(); status.PeerCount != 1 || status.PingCount != len(c.wantIps) {
			t.Fatalf("%s: status = %+v", c.name, status)
		}
		pinger.Close()
	}
}

// Nothing is pinged while this extender's own record is not active: a peer
// would refuse every claim as an unknown pinger.
func TestExtenderPeerPingerWaitsForItsOwnRecord(t *testing.T) {
	fixture := newTestPeerPingerFixture(t, "")
	fixture.addPeer(t, "192.0.2.11")
	pinger := fixture.start(t, func(settings *ExtenderPeerPingerSettings) {
		settings.SpreadTimeout = 0
		settings.ProbeCount = 1
	})
	assertNoNewPing(t, fixture, pinger, 0)
	if status := pinger.Status(); status.PeerCount != 0 {
		t.Fatalf("status = %+v before the own record", status)
	}
	fixture.applyRecord(t, fixture.ownPublicKey, 30*24*time.Hour, "192.0.2.1")
	waitForPingCount(t, pinger, 1)
}

// A pinger without an attestor measures and attests nothing; nor does one
// given another kind's attestor.
func TestExtenderPeerPingerMeasuresOnlyWithoutAnExtenderAttestor(t *testing.T) {
	fixture := newTestPeerPingerFixture(t, "")
	fixture.addPeer(t, "192.0.2.11")
	fixture.pings.outcomes["192.0.2.11"] = ExtenderPingCosigned
	provider, _ := newTestProbeAttestor(t)
	pinger := fixture.start(t, func(settings *ExtenderPeerPingerSettings) {
		settings.SpreadTimeout = 0
		settings.ProbeCount = 1
		settings.Attestor = provider
	})
	status := waitForPingCount(t, pinger, 1)
	if status.UnattestedCount != 1 {
		t.Fatalf("status = %+v, expected one unattested ping", status)
	}
	for _, attestor := range fixture.pings.attestors {
		if attestor != nil {
			t.Fatal("a provider attestor was carried by a peer ping")
		}
	}
	if reporter := fixture.reporter; reporter.PendingCount() != 0 || reporter.PostCount() != 0 {
		t.Fatal("an unattested ping was reported")
	}
}

// A peer whose addresses change is due again within the spread, whatever its
// refresh said.
func TestExtenderPeerPingerFollowsAnAddressChange(t *testing.T) {
	fixture := newTestPeerPingerFixture(t, "192.0.2.1")
	peerKey := fixture.addPeer(t, "192.0.2.11")
	pinger := fixture.start(t, func(settings *ExtenderPeerPingerSettings) {
		settings.SpreadTimeout = time.Hour
		settings.RefreshTimeout = 12 * time.Hour
		settings.ProbeCount = 1
		settings.Random = testRandomScript(0.5)
	})
	// the peer is scheduled at the clock the first pass read
	waitForPeerPingerStatus(t, pinger, "one peer", func(status ExtenderPeerPingerStatus) bool {
		return status.PeerCount == 1
	})
	fixture.clock.testClock.advance(30 * time.Minute)
	waitForPingCount(t, pinger, 1)

	// the peer is re-activated at another address
	fixture.applyRecord(t, peerKey, 30*24*time.Hour, "192.0.2.21")
	fixture.ipKeys["192.0.2.21"] = peerKey
	fixture.clock.waitForReads(t, 3)
	pinger.stateLock.Lock()
	dueTime := pinger.peers[hex.EncodeToString(peerKey)].dueTime
	pinger.stateLock.Unlock()
	if limit := fixture.clock.testClock.Now().Add(time.Hour); limit.Before(dueTime) {
		t.Fatalf("due at %s after the move, past the spread %s", dueTime, limit)
	}
	fixture.clock.testClock.advance(30 * time.Minute)
	waitForPingCount(t, pinger, 2)
}

// Close ends the pings in flight and joins them: nothing of the pinger runs
// once it returns.
func TestExtenderPeerPingerCloseJoins(t *testing.T) {
	fixture := newTestPeerPingerFixture(t, "192.0.2.1")
	fixture.addPeer(t, "192.0.2.11")
	fixture.addPeer(t, "192.0.2.12")
	// the gate is never opened: only the pinger's end releases the pings
	fixture.pings.gate = make(chan struct{})
	settings := DefaultExtenderPeerPingerSettings()
	settings.OwnPublicKey = fixture.ownPublicKey
	settings.Attestor = fixture.attestor
	settings.Reporter = fixture.reporter
	settings.Now = fixture.clock.Now
	settings.PassTimeout = time.Millisecond
	settings.SpreadTimeout = 0
	settings.Ping = fixture.pings.ping
	settings.IpVersionSupported = func(ipVersion int) bool { return true }
	pinger := NewExtenderPeerPinger(context.Background(), nil, fixture.directory, settings)

	deadline := time.After(10 * time.Second)
	for {
		if inFlight, _ := fixture.pings.inFlightValue(); inFlight == 2 {
			break
		}
		select {
		case <-fixture.pings.called:
		case <-deadline:
			t.Fatal("the pings never started")
		}
	}
	closed := make(chan struct{})
	go func() {
		pinger.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("close did not join the pings in flight")
	}
	if inFlight, _ := fixture.pings.inFlightValue(); inFlight != 0 {
		t.Fatalf("%d pings still in flight after close", inFlight)
	}
	// the end of the pinger is not a failed ping
	if status := pinger.Status(); status.FailedCount != 0 || status.PingCount != 0 {
		t.Fatalf("status = %+v after close", status)
	}
	pinger.Close()
}

// The constructor keeps its own copy of the settings and fills what is unset
// with the defaults, without touching the caller's.
func TestExtenderPeerPingerCopiesItsSettings(t *testing.T) {
	directory := NewExtenderDirectoryWithDefaults(t.Context())
	t.Cleanup(directory.Close)
	attestor, publicKey := newTestPeerProbeAttestor(t)
	settings := &ExtenderPeerPingerSettings{
		OwnPublicKey: slices.Clone(publicKey),
		Attestor:     attestor,
	}
	pinger := NewExtenderPeerPinger(context.Background(), nil, directory, settings)
	defer pinger.Close()
	if settings.Concurrency != 0 || settings.Now != nil || settings.Random != nil || settings.PassTimeout != 0 {
		t.Fatalf("the caller's settings were written: %+v", settings)
	}
	defaults := DefaultExtenderPeerPingerSettings()
	if pinger.settings.Concurrency != defaults.Concurrency ||
		pinger.settings.RefreshTimeout != defaults.RefreshTimeout ||
		pinger.settings.ProbeCount != defaults.ProbeCount ||
		pinger.settings.RecordCount != defaults.RecordCount {
		t.Fatalf("the unset settings are not the defaults: %+v", pinger.settings)
	}
	// a zero spread is kept: it pings a new peer at once
	if pinger.settings.SpreadTimeout != 0 {
		t.Fatalf("spread = %s", pinger.settings.SpreadTimeout)
	}
	settings.OwnPublicKey[0] ^= 1
	attestor.ExtenderPublicKey[0] ^= 1
	if !slices.Equal(pinger.ownPublicKey, publicKey) || !slices.Equal(pinger.attestor.ExtenderPublicKey, publicKey) {
		t.Fatal("the pinger aliases the caller's keys")
	}
	// the own key defaults to the attestor's
	restored, _ := newTestPeerProbeAttestor(t)
	defaulted := NewExtenderPeerPinger(context.Background(), nil, directory, &ExtenderPeerPingerSettings{Attestor: restored})
	defer defaulted.Close()
	if !slices.Equal(defaulted.ownPublicKey, restored.ExtenderPublicKey) {
		t.Fatal("the own key did not default to the attestor's")
	}
}

// The cadence of GEOMAP §2.1: a new peer within an hour, every peer twice a
// day with every refresh under a day, two at a time, lowest of two probes.
func TestExtenderPeerPingerDefaultSettings(t *testing.T) {
	settings := DefaultExtenderPeerPingerSettings()
	cases := []struct {
		name string
		got  any
		want any
	}{
		{name: "SpreadTimeout", got: settings.SpreadTimeout, want: time.Hour},
		{name: "RefreshTimeout", got: settings.RefreshTimeout, want: 12 * time.Hour},
		{name: "Concurrency", got: settings.Concurrency, want: 2},
		{name: "ProbeCount", got: settings.ProbeCount, want: 2},
		{name: "ProbeTimeout", got: settings.ProbeTimeout, want: 5 * time.Second},
		{name: "RecordCount", got: settings.RecordCount, want: 256},
		{name: "PeerSampleSize", got: settings.PeerSampleSize, want: 64},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %v, expected %v", c.name, c.got, c.want)
		}
	}
	if settings.Jitter < 0 || 1 <= settings.Jitter {
		t.Errorf("jitter = %v", settings.Jitter)
	}
	if longest := time.Duration(float64(settings.RefreshTimeout) * (1 + settings.Jitter)); 24*time.Hour <= longest {
		t.Errorf("the longest refresh is %s, not under a day", longest)
	}
	if settings.Now == nil || settings.PassTimeout <= 0 {
		t.Error("the defaults carry no clock or pass timeout")
	}
}
