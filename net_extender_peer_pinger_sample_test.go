package connect

import (
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// The bounded sample of the peer pinger (GEOMAP §2.1, D26): at most
// PeerSampleSize peers, the nearest by the continent hint first, the rest a
// random slice drawn again every refresh, and a member keeping its schedule
// until it is rotated out.

// Adds a peer on a continent at one address, signed as the fixture's other
// peers are.
func (self *testPeerPingerFixture) addContinentPeer(t *testing.T, ip string, continent string) []byte {
	t.Helper()
	publicKey := newTestExtenderKey(t)
	self.clock.advance(time.Millisecond)
	record, err := SignExtenderRecord(self.rootPrivateKey, &protocol.ExtenderRecordBody{
		PublicKey:     publicKey,
		Addresses:     []*protocol.ExtenderAddress{testExtenderAddress(ip, ExtenderCarrierTcp, ExtenderCarrierQuic)},
		TcpPort:       443,
		UdpPort:       443,
		CountryCode:   "zz",
		ContinentCode: continent,
		IssueTimeMs:   uint64(self.clock.testClock.Now().UnixMilli()),
		ExpireTimeMs:  uint64(self.clock.testClock.Now().Add(30 * 24 * time.Hour).UnixMilli()),
		NetworkHost:   testExtenderNetworkHost,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := self.directory.ApplyRecord(record, ExtenderSourceGossip); err != nil {
		t.Fatal(err)
	}
	self.ipKeys[ip] = publicKey
	return publicKey
}

// The ips of one refresh's pings, in the order they started, checked to be
// distinct.
func testPingWindow(t *testing.T, calls []string) map[string]bool {
	t.Helper()
	ips := map[string]bool{}
	for _, call := range calls {
		if ips[call] {
			t.Fatalf("%s was pinged twice in one refresh", call)
		}
		ips[call] = true
	}
	return ips
}

// With 200 peers and a sample of 64, exactly 64 are pinged per refresh: the
// twenty on the hinted continent every time and first, the rest a random
// slice that is drawn again on the next refresh.
func TestExtenderPeerPingerPingsABoundedSample(t *testing.T) {
	fixture := newTestPeerPingerFixture(t, "192.0.2.1")
	fixture.directory.SetContinentHint("EU")
	nearIps := map[string]bool{}
	for i := range 200 {
		ip := fmt.Sprintf("198.51.100.%d", 1+i)
		continent := "NA"
		if i%10 == 0 {
			continent = "EU"
			nearIps[ip] = true
		}
		fixture.addContinentPeer(t, ip, continent)
	}
	// one ping at a time, so the order the pings start in is the order the
	// schedule put them in
	pinger := fixture.start(t, func(settings *ExtenderPeerPingerSettings) {
		settings.SpreadTimeout = 0
		settings.RefreshTimeout = time.Hour
		settings.Concurrency = 1
		settings.ProbeCount = 1
		settings.Random = testSeededRandom(11)
	})

	randomSlices := []map[string]bool{}
	for refresh := range 2 {
		if 0 < refresh {
			fixture.clock.testClock.advance(time.Hour)
		}
		status := waitForPingCount(t, pinger, 64*(refresh+1))
		assertNoNewPing(t, fixture, pinger, 64*(refresh+1))
		if status.PeerCount != 200 || status.SampleSize != 64 || status.SampledPeerCount != 64 {
			t.Fatalf("refresh %d status = %+v", refresh, status)
		}
		calls := fixture.pings.callsValue()[64*refresh : 64*(refresh+1)]
		window := testPingWindow(t, calls)
		// the hinted continent first, every one of it
		for i, call := range calls {
			if nearIps[call] != (i < len(nearIps)) {
				t.Fatalf("refresh %d ping %d is %s, expected the hinted continent's twenty first", refresh, i, call)
			}
		}
		randomSlice := map[string]bool{}
		for ip := range window {
			if !nearIps[ip] {
				randomSlice[ip] = true
			}
		}
		randomSlices = append(randomSlices, randomSlice)
		if sampled := pinger.SampledPeers(); len(sampled) != 64 {
			t.Fatalf("refresh %d sampled %d peers", refresh, len(sampled))
		}
	}
	if maps.Equal(randomSlices[0], randomSlices[1]) {
		t.Fatal("the random slice was not drawn again on the refresh")
	}
}

// Without a bound every active peer is pinged every refresh, as before the
// sample.
func TestExtenderPeerPingerUnboundedPingsEveryPeer(t *testing.T) {
	fixture := newTestPeerPingerFixture(t, "192.0.2.1")
	for i := range 100 {
		fixture.addPeer(t, fmt.Sprintf("198.51.100.%d", 1+i))
	}
	pinger := fixture.start(t, func(settings *ExtenderPeerPingerSettings) {
		settings.PeerSampleSize = 0
		settings.SpreadTimeout = 0
		settings.RefreshTimeout = time.Hour
		settings.Concurrency = 8
		settings.ProbeCount = 1
	})
	for refresh := range 2 {
		if 0 < refresh {
			fixture.clock.testClock.advance(time.Hour)
		}
		status := waitForPingCount(t, pinger, 100*(refresh+1))
		assertNoNewPing(t, fixture, pinger, 100*(refresh+1))
		if status.PeerCount != 100 || status.SampleSize != 0 || status.SampledPeerCount != 100 {
			t.Fatalf("refresh %d status = %+v", refresh, status)
		}
		testPingWindow(t, fixture.pings.callsValue()[100*refresh:100*(refresh+1)])
	}
}

// Between refreshes a member keeps its schedule; one that leaves is replaced
// by a fresh draw, due within the spread; and a new peer on the hinted
// continent joins the sample at once, in the place of a random member.
func TestExtenderPeerPingerSampleFollowsTheDirectory(t *testing.T) {
	fixture := newTestPeerPingerFixture(t, "192.0.2.1")
	fixture.directory.SetContinentHint("EU")
	for i := range 80 {
		fixture.addContinentPeer(t, fmt.Sprintf("198.51.100.%d", 1+i), "NA")
	}
	pinger := fixture.start(t, func(settings *ExtenderPeerPingerSettings) {
		settings.PeerSampleSize = 16
		settings.SpreadTimeout = time.Hour
		settings.RefreshTimeout = 12 * time.Hour
		settings.ProbeCount = 1
		settings.Random = testSeededRandom(13)
	})
	waitForPeerPingerStatus(t, pinger, "a full sample", func(status ExtenderPeerPingerStatus) bool {
		return status.SampledPeerCount == 16
	})
	dueTimes := func() map[string]time.Time {
		pinger.stateLock.Lock()
		defer pinger.stateLock.Unlock()
		keyHexDueTimes := map[string]time.Time{}
		for keyHex, peer := range pinger.peers {
			if peer.sampled {
				keyHexDueTimes[keyHex] = peer.dueTime
			}
		}
		return keyHexDueTimes
	}
	firstDueTimes := dueTimes()
	fixture.clock.waitForReads(t, 3)
	if keptDueTimes := dueTimes(); !maps.Equal(firstDueTimes, keptDueTimes) {
		t.Fatal("a pass without a refresh moved the sample or its schedule")
	}

	// a member leaves: its place is drawn again, due within the spread
	leftKeyHex := slices.Sorted(maps.Keys(firstDueTimes))[0]
	leftKey, err := hex.DecodeString(leftKeyHex)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.directory.ApplyRevocation(signTestRevocation(t, fixture.rootPrivateKey, leftKey, fixture.clock.testClock.Now())); err != nil {
		t.Fatal(err)
	}
	// a pass barrier: the passes after the revocation have run
	fixture.clock.waitForReads(t, 3)
	replacedDueTimes := dueTimes()
	if _, left := replacedDueTimes[leftKeyHex]; left || len(replacedDueTimes) != 16 {
		t.Fatalf("the sample holds %d members, the one that left among them %t", len(replacedDueTimes), left)
	}
	for keyHex, dueTime := range replacedDueTimes {
		firstDueTime, kept := firstDueTimes[keyHex]
		switch {
		case kept && !dueTime.Equal(firstDueTime):
			t.Fatalf("a kept member's schedule moved from %s to %s", firstDueTime, dueTime)
		case !kept && fixture.clock.testClock.Now().Add(time.Hour).Before(dueTime):
			t.Fatalf("the drawn member is due at %s, past the spread", dueTime)
		}
	}

	// a peer of the hinted continent arrives: it is in the sample, which
	// stays at its size
	nearKey := fixture.addContinentPeer(t, "198.51.100.200", "EU")
	fixture.clock.waitForReads(t, 3)
	nearDueTimes := dueTimes()
	if _, sampled := nearDueTimes[hex.EncodeToString(nearKey)]; !sampled || len(nearDueTimes) != 16 {
		t.Fatalf("the sample holds %d members, the near peer among them %t", len(nearDueTimes), sampled)
	}
	pinger.stateLock.Lock()
	near := pinger.peers[hex.EncodeToString(nearKey)].near
	pinger.stateLock.Unlock()
	if !near {
		t.Fatal("the hinted continent's peer is not among the nearest")
	}
}

// The ping ring keeps the newest RecordCount pings in order, and its backing
// slice within twice that: the head moves past what is dropped rather than the
// ring shifting on every ping.
func TestExtenderPeerPingerRecordRingStaysBounded(t *testing.T) {
	pinger := &ExtenderPeerPinger{
		settings: &ExtenderPeerPingerSettings{
			RecordCount: 4,
		},
		statusMonitor: NewMonitorValue(ExtenderPeerPingerStatus{}),
	}
	startTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := range 100 {
		pinger.addRecord(ExtenderPingRecord{
			Time: startTime.Add(time.Duration(i) * time.Second),
		}, false)
		pinger.stateLock.Lock()
		recordCount := len(pinger.records)
		pinger.stateLock.Unlock()
		if 2*4 < recordCount {
			t.Fatalf("ping %d left %d records behind a ring of 4", i, recordCount)
		}
	}
	records := pinger.Records()
	connectAssertCount(t, "records", len(records), 4)
	for i, record := range records {
		if want := startTime.Add(time.Duration(96+i) * time.Second); !record.Time.Equal(want) {
			t.Fatalf("record %d is of %s, expected the newest four in order", i, record.Time)
		}
	}
	connectAssertCount(t, "pings", pinger.Status().PingCount, 100)
}
