package connect

import (
	"context"
	"crypto/ed25519"
	"net/netip"
	"slices"
	"testing"
	"time"
)

// A directory shared by strategies that dial along different paths: the
// owner, whose network client fills it, and readers, each dialing through a
// path of its own -- the sn miner's slots, each through its own proxy
// (ClientStrategySettings.ExtenderDirectoryReadOnly).

// A second strategy on a test strategy's directory, which only reads it when
// readOnly is set.
func newTestExtenderReaderStrategy(
	t *testing.T,
	directory *ExtenderDirectory,
	readOnly bool,
) *ClientStrategy {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	settings := DefaultClientStrategySettings()
	settings.ExtenderDirectory = directory
	settings.ExtenderDirectoryReadOnly = readOnly
	reader := NewClientStrategy(ctx, settings)
	t.Cleanup(func() {
		reader.Close()
		cancel()
	})
	return reader
}

// The extender dialers of a strategy for one address.
func testExtenderDialersOf(clientStrategy *ClientStrategy, ip netip.Addr) []*clientDialer {
	return slices.DeleteFunc(testExtenderDialers(clientStrategy), func(dialer *clientDialer) bool {
		return dialer.extenderConfig.Ip != ip
	})
}

// One verified address, with every socket carrier, in the directory.
func applyTestReadOnlyRecord(t *testing.T, clock *testClock, directory *ExtenderDirectory, rootPrivateKey ed25519.PrivateKey, ip netip.Addr) {
	t.Helper()
	record := signTestRecord(
		t,
		rootPrivateKey,
		newTestExtenderKey(t),
		clock.Now(),
		clock.Now().Add(14*24*time.Hour),
		testExtenderAddress(ip.String(), ExtenderCarrierTcp, ExtenderCarrierQuic, ExtenderCarrierDns),
	)
	if _, err := directory.ApplyRecord(record, ExtenderSourceFeed); err != nil {
		t.Fatal(err)
	}
}

// Root cause of sharing one directory among slots that dial through different
// proxies: a strategy that is not the directory's owner writes its failures
// into it, so one slot whose proxy is down holds the extenders it tried for
// every slot and for the owner. This is what the directory does for a writer,
// and why the slots read it only.
func TestClientStrategySharedDirectoryWriterHoldsTheAddressForEveryone(t *testing.T) {
	clock := newTestClock()
	owner, directory, rootPrivateKey := newTestExtenderStrategy(t, clock, nil)
	ip := netip.MustParseAddr("192.0.2.140")
	applyTestReadOnlyRecord(t, clock, directory, rootPrivateKey, ip)
	writer := newTestExtenderReaderStrategy(t, directory, false)

	expandedDialers := writer.expandExtenderDialers()
	if len(expandedDialers) == 0 {
		t.Fatal("the writer expanded no dialer")
	}
	expandedDialers[0].Update(context.Background(), context.DeadlineExceeded)
	if entry := testDirectoryEntry(t, directory, ip); entry.State != ExtenderStateHold {
		t.Fatalf("state = %s, expected the writer's failure to hold the shared address", entry.State)
	}
	if expanded := owner.expandExtenderDialers(); len(expanded) != 0 {
		t.Fatalf("the owner expanded %d dialers of an address another path's failure held", len(expanded))
	}
}

// A reader's failure is its own: the directory hears nothing, the owner still
// dials the address, and the reader holds it for itself on the directory's
// doubling schedule until the hold passes or a dial succeeds.
func TestClientStrategyReadOnlyHoldsItsOwnFailuresOnly(t *testing.T) {
	clock := newTestClock()
	owner, directory, rootPrivateKey := newTestExtenderStrategy(t, clock, nil)
	ip := netip.MustParseAddr("192.0.2.141")
	applyTestReadOnlyRecord(t, clock, directory, rootPrivateKey, ip)
	reader := newTestExtenderReaderStrategy(t, directory, true)

	expandedDialers := reader.expandExtenderDialers()
	if len(expandedDialers) != 3 {
		t.Fatalf("the reader expanded %d dialers, expected one per carrier", len(expandedDialers))
	}
	for _, dialer := range expandedDialers {
		if dialer.extenderEvidence == nil {
			t.Fatal("a reader's dialer reports to the directory")
		}
	}
	ctx := context.Background()
	expandedDialers[0].Update(ctx, context.DeadlineExceeded)

	entry := testDirectoryEntry(t, directory, ip)
	if entry.FailureCount != 0 || entry.SuccessCount != 0 || entry.State != ExtenderStateActive {
		t.Fatalf("the shared entry took the reader's outcome: %+v", entry)
	}
	if expanded := owner.expandExtenderDialers(); len(expanded) != 3 {
		t.Fatalf("the owner expanded %d dialers, expected the address to stay its candidate", len(expanded))
	}

	// the reader drops its dialers of the address and draws it no more
	reader.collapseExtenderDialers()
	if count := len(testExtenderDialersOf(reader, ip)); count != 0 {
		t.Fatalf("the reader kept %d dialers of an address it holds", count)
	}
	if expanded := reader.expandExtenderDialers(); len(expanded) != 0 {
		t.Fatalf("the reader expanded %d dialers of an address it holds", len(expanded))
	}

	// the hold is the directory's first one, and a second failure doubles it
	holdTimeout := directory.holdTimeout(1)
	clock.advance(holdTimeout)
	expandedDialers = reader.expandExtenderDialers()
	if len(expandedDialers) != 3 {
		t.Fatalf("the reader expanded %d dialers once its hold passed", len(expandedDialers))
	}
	expandedDialers[0].Update(ctx, context.DeadlineExceeded)
	clock.advance(holdTimeout)
	if !reader.extenderEvidence.held(ip) {
		t.Fatal("a second failure held the address no longer than the first")
	}
	clock.advance(directory.holdTimeout(2) - holdTimeout)
	if reader.extenderEvidence.held(ip) {
		t.Fatal("the second hold outlived the directory's second hold timeout")
	}

	// a success ends the run of failures
	dialers := testExtenderDialersOf(reader, ip)
	if len(dialers) == 0 {
		t.Fatal("the reader has no dialer of the address after its hold passed")
	}
	dialers[0].Update(ctx, nil)
	dialers[0].Update(ctx, context.DeadlineExceeded)
	clock.advance(holdTimeout)
	if reader.extenderEvidence.held(ip) {
		t.Fatal("a failure after a success held the address past the first hold timeout")
	}
	if entry := testDirectoryEntry(t, directory, ip); entry.FailureCount != 0 || entry.SuccessCount != 0 {
		t.Fatalf("the shared entry took the reader's outcomes: %+v", entry)
	}
}

// A reader honors what the owner judged: an address the owner's own failure
// held is no candidate for the reader either.
func TestClientStrategyReadOnlyHonorsTheOwnersHolds(t *testing.T) {
	clock := newTestClock()
	_, directory, rootPrivateKey := newTestExtenderStrategy(t, clock, nil)
	ip := netip.MustParseAddr("192.0.2.142")
	applyTestReadOnlyRecord(t, clock, directory, rootPrivateKey, ip)
	reader := newTestExtenderReaderStrategy(t, directory, true)

	if expanded := reader.expandExtenderDialers(); len(expanded) != 3 {
		t.Fatalf("the reader expanded %d dialers", len(expanded))
	}
	for _, dialer := range testExtenderDialers(reader) {
		dialer.Update(context.Background(), nil)
	}
	directory.RecordFailure(ip, ExtenderConnectModeTcpTls)
	reader.collapseExtenderDialers()
	if count := len(testExtenderDialersOf(reader, ip)); count != 0 {
		t.Fatalf("the reader kept %d dialers of an address the owner held", count)
	}
}

// A 429 limits the reader's own path: its dialer waits it out, and the
// directory keeps no limit of it.
func TestClientStrategyReadOnlyKeepsItsLimitsToItself(t *testing.T) {
	clock := newTestClock()
	_, directory, rootPrivateKey := newTestExtenderStrategy(t, clock, nil)
	ip := netip.MustParseAddr("192.0.2.143")
	applyTestReadOnlyRecord(t, clock, directory, rootPrivateKey, ip)
	reader := newTestExtenderReaderStrategy(t, directory, true)

	expandedDialers := reader.expandExtenderDialers()
	if len(expandedDialers) == 0 {
		t.Fatal("the reader expanded no dialer")
	}
	expandedDialers[0].Update(context.Background(), &ExtenderLimitedError{RetryAfter: time.Hour})
	if limitedUntil := directory.AddressLimitedUntil(ip); !limitedUntil.IsZero() {
		t.Fatalf("the directory took the reader's limit, until %s", limitedUntil)
	}
	if expandedDialers[0].LimitedUntil().IsZero() {
		t.Fatal("the reader's dialer does not wait out its limit")
	}
	if reader.extenderEvidence.held(ip) {
		t.Fatal("a limit held the address as a failure would")
	}
}

// A reader has no webrtc carrier: the carrier and its signaling are the
// owner's, who opened the directory's gate. It expands no webrtc profile, and
// a webrtc failure holds nothing of an address that serves a socket carrier,
// as in the directory (S8, S9).
func TestClientStrategyReadOnlyDialsNoWebRtcWithoutACarrier(t *testing.T) {
	clock := newTestClock()
	_, directory, rootPrivateKey := newTestExtenderStrategy(t, clock, nil)
	directory.SetWebRtcCarrierEnabled(true)
	webRtcIp := netip.MustParseAddr("192.0.2.144")
	record := signTestWebRtcRecord(
		t,
		rootPrivateKey,
		newTestExtenderKey(t),
		clock.Now(),
		clock.Now().Add(14*24*time.Hour),
		NewId(),
		testExtenderAddress(webRtcIp.String(), ExtenderCarrierWebRtc),
	)
	if _, err := directory.ApplyRecord(record, ExtenderSourceFeed); err != nil {
		t.Fatal(err)
	}
	reader := newTestExtenderReaderStrategy(t, directory, true)
	if expanded := reader.expandExtenderDialers(); len(expanded) != 0 {
		t.Fatalf("the reader expanded %d webrtc dialers with no carrier", len(expanded))
	}
	writer := newTestExtenderReaderStrategy(t, directory, false)
	if expanded := writer.expandExtenderDialers(); len(expanded) != 1 {
		t.Fatalf("a strategy that owns its directory expanded %d webrtc dialers, expected one", len(expanded))
	}

	socketIp := netip.MustParseAddr("192.0.2.145")
	applyTestReadOnlyRecord(t, clock, directory, rootPrivateKey, socketIp)
	reader.extenderEvidence.recordFailure(socketIp, ExtenderConnectModeWebRtc)
	if reader.extenderEvidence.held(socketIp) {
		t.Fatal("a webrtc failure held an address that serves a socket carrier")
	}
	reader.extenderEvidence.recordFailure(socketIp, ExtenderConnectModeTcpTls)
	if !reader.extenderEvidence.held(socketIp) {
		t.Fatal("a tcp failure did not hold the address")
	}
}

// The reader's holds are bounded as the directory's own addresses are: past
// the cap, the hold that passed longest ago goes first.
func TestClientStrategyReadOnlyHoldsAreBoundedByTheAddressCap(t *testing.T) {
	clock := newTestClock()
	_, directory, _ := newTestExtenderStrategy(t, clock, nil)
	directory.settings.MaxAddressCount = 2
	reader := newTestExtenderReaderStrategy(t, directory, true)
	evidence := reader.extenderEvidence
	first := netip.MustParseAddr("192.0.2.150")
	second := netip.MustParseAddr("192.0.2.151")
	third := netip.MustParseAddr("192.0.2.152")
	evidence.recordFailure(first, ExtenderConnectModeTcpTls)
	clock.advance(time.Second)
	evidence.recordFailure(second, ExtenderConnectModeTcpTls)
	clock.advance(time.Second)
	evidence.recordFailure(third, ExtenderConnectModeTcpTls)
	evidence.stateLock.Lock()
	_, firstKept := evidence.ipHolds[first]
	count := len(evidence.ipHolds)
	evidence.stateLock.Unlock()
	if count != 2 || firstKept {
		t.Fatalf("holds = %d, first kept %t; expected the oldest dropped at the cap", count, firstKept)
	}
	if !evidence.held(second) || !evidence.held(third) {
		t.Fatal("a newer hold was dropped")
	}
}
