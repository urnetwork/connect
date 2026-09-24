package connect

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"maps"
	"net/netip"
	"runtime"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// The scale tests of the client directory and the peer pinger (GEOMAP §5.8
// item 5, D27): a million gossip records into a directory at its default cap,
// and a pinger over a hundred thousand known peers. Both are plain tests that
// run in the suite: the records are generated in memory and applied through
// the directory's own path past the signature check, whose cost per record
// is fixed and measured by the ingest benchmark of §5.8 item 3, not by the
// directory's size.

// The scale and the budgets of these tests.
type testExtenderScaleSettings struct {
	// records fed to the capped directory, one in NearEvery of them on the
	// hinted continent
	RecordCount int
	NearEvery   int
	// the records the budget is judged at, which the feed's cost per record
	// is extrapolated to when fewer are fed
	TargetRecordCount int
	// the feed of TargetRecordCount records completes within this
	FeedTimeout time.Duration
	// the heap the directory holds once the feed is done, at most
	MaxRetainedByteCount int64

	// peers known to the pinger's directory, one in PeerNearEvery of them on
	// the hinted continent
	PeerCount     int
	PeerNearEvery int
	// refreshes of the pinger's sample timed, and the most cpu time one may
	// take on average
	RefreshCount      int
	MaxSelectDuration time.Duration
}

// The scale of GEOMAP §5.8 item 5, with generous margins on the budgets.
// Under -race every instruction costs several times what it does otherwise,
// so the feed takes a quarter of the records, as many of them on the hinted
// continent, and its cost per record is extrapolated to the million, the way
// this package's other throughput tests scale under -race.
func defaultTestExtenderScaleSettings() *testExtenderScaleSettings {
	recordCount := 1000 * 1000
	nearEvery := 1000
	if raceEnabled {
		recordCount /= 4
		nearEvery /= 4
	}
	return &testExtenderScaleSettings{
		RecordCount:          recordCount,
		NearEvery:            nearEvery,
		TargetRecordCount:    1000 * 1000,
		FeedTimeout:          30 * time.Second,
		MaxRetainedByteCount: 16 * 1024 * 1024,
		PeerCount:            100 * 1000,
		PeerNearEvery:        5000,
		RefreshCount:         200,
		MaxSelectDuration:    time.Millisecond,
	}
}

// The carriers every generated record lists, shared, since a body is only
// read.
var testScaleCarriers = []string{ExtenderCarrierTcp}

// One generated record: its body verified in all but the signature, and the
// key it carries. The key and the address come from the index, so every
// record is distinct and nothing about one is kept to make the next.
func testScaleRecord(i int, continent string, now time.Time) (*protocol.ExtenderRecordBody, []byte) {
	publicKey := make([]byte, 32)
	copy(publicKey, "scale-test-extender-identity-key")
	binary.BigEndian.PutUint64(publicKey[24:], uint64(i))
	// the address in the v6 documentation prefix
	ipBytes := [16]byte{0x20, 0x01, 0x0d, 0xb8}
	binary.BigEndian.PutUint64(ipBytes[8:], uint64(i))
	body := &protocol.ExtenderRecordBody{
		PublicKey: publicKey,
		Addresses: []*protocol.ExtenderAddress{
			{
				Ip:        netip.AddrFrom16(ipBytes).String(),
				IpVersion: 6,
				Carriers:  testScaleCarriers,
			},
		},
		TcpPort:       443,
		ContinentCode: continent,
		IssueTimeMs:   uint64(now.UnixMilli()),
		ExpireTimeMs:  uint64(now.Add(24 * time.Hour).UnixMilli()),
		NetworkHost:   testExtenderNetworkHost,
	}
	return body, publicKey
}

// A directory for a scale test, on a clock that never moves.
func newTestScaleDirectory(t *testing.T, configure func(settings *ExtenderDirectorySettings)) (*ExtenderDirectory, time.Time) {
	t.Helper()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	settings := DefaultExtenderDirectorySettings()
	settings.Now = func() time.Time { return now }
	settings.NetworkHosts = []string{testExtenderNetworkHost}
	if configure != nil {
		configure(settings)
	}
	ctx, cancel := context.WithCancel(context.Background())
	directory := NewExtenderDirectory(ctx, settings)
	t.Cleanup(func() {
		directory.Close()
		cancel()
	})
	return directory, now
}

// The live heap after a collection.
func testHeapByteCount() int64 {
	runtime.GC()
	runtime.GC()
	var memStats runtime.MemStats
	runtime.ReadMemStats(&memStats)
	return int64(memStats.HeapAlloc)
}

// A million gossip records into a directory at the default cap of 512: the
// feed completes within its budget, the directory holds a bounded heap
// whatever it was fed, and the records on the hinted continent are kept ahead
// of every other, the last 512 of them fed being exactly what remains. Under
// -race the feed is a quarter of that, judged by its extrapolation.
func TestExtenderDirectoryScaleAMillionGossipRecords(t *testing.T) {
	scaleSettings := defaultTestExtenderScaleSettings()
	directory, now := newTestScaleDirectory(t, nil)
	directory.SetContinentHint("EU")
	record := &protocol.ExtenderRecord{}

	beforeByteCount := testHeapByteCount()
	// the keys of the last 512 records of the hinted continent fed, oldest
	// first
	lastNearKeyHexes := []string{}
	startTime := time.Now()
	for i := range scaleSettings.RecordCount {
		continent := "NA"
		if i%scaleSettings.NearEvery == 0 {
			continent = "EU"
		}
		body, publicKey := testScaleRecord(i, continent, now)
		if !directory.applyVerifiedRecord(record, body, ExtenderSourceGossip) {
			t.Fatalf("record %d was not applied", i)
		}
		if continent == "EU" {
			lastNearKeyHexes = append(lastNearKeyHexes, hex.EncodeToString(publicKey))
			if 512 < len(lastNearKeyHexes) {
				lastNearKeyHexes = lastNearKeyHexes[1:]
			}
		}
	}
	feedDuration := time.Since(startTime)
	retainedByteCount := testHeapByteCount() - beforeByteCount
	targetFeedDuration := time.Duration(
		float64(feedDuration) * float64(scaleSettings.TargetRecordCount) / float64(scaleSettings.RecordCount),
	)

	t.Logf(
		"fed %d records in %s (%s a record, %s for %d), %d bytes retained",
		scaleSettings.RecordCount,
		feedDuration,
		feedDuration/time.Duration(scaleSettings.RecordCount),
		targetFeedDuration,
		scaleSettings.TargetRecordCount,
		retainedByteCount,
	)
	if scaleSettings.FeedTimeout < targetFeedDuration {
		t.Fatalf(
			"the feed of %d records takes %s, past its budget of %s",
			scaleSettings.TargetRecordCount,
			targetFeedDuration,
			scaleSettings.FeedTimeout,
		)
	}
	if scaleSettings.MaxRetainedByteCount < retainedByteCount {
		t.Fatalf("the directory retained %d bytes, past its budget of %d", retainedByteCount, scaleSettings.MaxRetainedByteCount)
	}
	connectAssertCount(t, "active records", directory.ActiveRecordCount(), 512)
	connectAssertCount(t, "known addresses", directory.Snapshot().KnownCount, 512)
	for _, keyHex := range lastNearKeyHexes {
		publicKey, err := hex.DecodeString(keyHex)
		if err != nil {
			t.Fatal(err)
		}
		if !directory.IsActiveKey(publicKey) {
			t.Fatalf("the hinted continent's recent record %s was not kept", keyHex)
		}
	}
}

// A pinger over a hundred thousand known peers pings exactly 64 per refresh,
// the hinted continent's first, with the random slice drawn again on each
// refresh; and the selection of one refresh's sample reads so little of the
// directory that it takes under a millisecond, averaged over many refreshes.
func TestExtenderPeerPingerScaleAHundredThousandPeers(t *testing.T) {
	scaleSettings := defaultTestExtenderScaleSettings()
	directory, now := newTestScaleDirectory(t, func(settings *ExtenderDirectorySettings) {
		settings.MaxActiveRecordCount = 0
		settings.MaxAddressCount = 0
		settings.Random = testSeededRandom(17)
	})
	directory.SetContinentHint("EU")
	record := &protocol.ExtenderRecord{}
	nearIps := map[string]bool{}
	buildStartTime := time.Now()
	for i := range scaleSettings.PeerCount {
		continent := "NA"
		if i%scaleSettings.PeerNearEvery == 0 {
			continent = "EU"
		}
		body, _ := testScaleRecord(i, continent, now)
		directory.applyVerifiedRecord(record, body, ExtenderSourceGossip)
		if continent == "EU" {
			nearIps[body.Addresses[0].Ip] = true
		}
	}
	buildDuration := time.Since(buildStartTime)
	connectAssertCount(t, "active records", directory.ActiveRecordCount(), scaleSettings.PeerCount)

	// the selection alone: one sample drawn per refresh, on a pinger whose
	// loop never runs
	ownKey, _ := testScaleRecord(scaleSettings.PeerCount+1, "EU", now)
	selector := &ExtenderPeerPinger{
		directory: directory,
		settings: &ExtenderPeerPingerSettings{
			PeerSampleSize: 64,
			RefreshTimeout: time.Hour,
			Concurrency:    2,
			Random:         testSeededRandom(19),
		},
		ownPublicKeyHex: hex.EncodeToString(ownKey.PublicKey),
		peers:           map[string]*extenderPeerPingerPeer{},
	}
	selectTime := now
	var selectDuration time.Duration
	var selectCpuDuration time.Duration
	// the collection the build left pending is not the selection's to pay
	runtime.GC()
	var lastRandomKeyHexes map[string]bool
	for refresh := range scaleSettings.RefreshCount {
		selectTime = selectTime.Add(time.Hour)
		startTime := time.Now()
		startCpuTime := testProcessCpuTime()
		keyHexTargets := selector.samplePeers(selectTime)
		selector.schedule(selectTime, keyHexTargets)
		selectCpuDuration += testProcessCpuTime() - startCpuTime
		selectDuration += time.Since(startTime)

		connectAssertCount(t, "sample", len(keyHexTargets), 64)
		nearCount := 0
		randomKeyHexes := map[string]bool{}
		for keyHex, target := range keyHexTargets {
			if target.near {
				nearCount += 1
			} else {
				randomKeyHexes[keyHex] = true
			}
		}
		connectAssertCount(t, "nearest", nearCount, len(nearIps))
		if maps.Equal(randomKeyHexes, lastRandomKeyHexes) {
			t.Fatalf("refresh %d drew the same random slice as the one before", refresh)
		}
		lastRandomKeyHexes = randomKeyHexes
	}
	averageSelectDuration := selectDuration / time.Duration(scaleSettings.RefreshCount)
	averageSelectCpuDuration := selectCpuDuration / time.Duration(scaleSettings.RefreshCount)
	t.Logf(
		"built %d peers in %s; selected a sample of 64 in %s of cpu (%s of wall clock) a refresh over %d refreshes",
		scaleSettings.PeerCount,
		buildDuration,
		averageSelectCpuDuration,
		averageSelectDuration,
		scaleSettings.RefreshCount,
	)
	if scaleSettings.MaxSelectDuration < averageSelectCpuDuration {
		t.Fatalf("a refresh's selection took %s of cpu, past its budget of %s", averageSelectCpuDuration, scaleSettings.MaxSelectDuration)
	}

	// the pinger itself: exactly 64 pinged per refresh, the nearest first
	clock := newTestPingerClock()
	pings := newTestPeerPings()
	settings := DefaultExtenderPeerPingerSettings()
	settings.OwnPublicKey = ownKey.PublicKey
	settings.Now = clock.Now
	settings.PassTimeout = time.Millisecond
	settings.Ping = pings.ping
	settings.IpVersionSupported = func(ipVersion int) bool { return true }
	settings.SpreadTimeout = 0
	settings.RefreshTimeout = time.Hour
	settings.Jitter = 0
	settings.Concurrency = 1
	settings.ProbeCount = 1
	settings.Random = testSeededRandom(23)
	pinger := NewExtenderPeerPinger(t.Context(), nil, directory, settings)
	defer pinger.Close()
	randomSlices := []map[string]bool{}
	for refresh := range 2 {
		if 0 < refresh {
			clock.testClock.advance(time.Hour)
		}
		status := waitForPingCount(t, pinger, 64*(refresh+1))
		clock.waitForReads(t, 3)
		calls := pings.callsValue()
		connectAssertCount(t, "pings", len(calls), 64*(refresh+1))
		if status.PeerCount != scaleSettings.PeerCount || status.SampledPeerCount != 64 {
			t.Fatalf("refresh %d status = %+v", refresh, status)
		}
		window := testPingWindow(t, calls[64*refresh:])
		for i, call := range calls[64*refresh:] {
			if nearIps[call] != (i < len(nearIps)) {
				t.Fatalf("refresh %d ping %d is %s, expected the hinted continent's first", refresh, i, call)
			}
		}
		randomSlice := map[string]bool{}
		for ip := range window {
			if !nearIps[ip] {
				randomSlice[ip] = true
			}
		}
		randomSlices = append(randomSlices, randomSlice)
	}
	if maps.Equal(randomSlices[0], randomSlices[1]) {
		t.Fatal("the random slice was not drawn again on the refresh")
	}
}
