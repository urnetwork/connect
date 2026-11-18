package extender

// This server's part of the camouflage report (extender_camouflage_report.go): each
// count goes out once, a verdict goes out with nothing counted, what is past a bound
// waits, and a real splice comes through as load on its borrowed site.

import (
	"context"
	"fmt"
	"net"
	"reflect"
	"sync"
	"testing"

	"github.com/urnetwork/connect/v2026"
)

func newTestCamouflageReportServer(t *testing.T) *ExtenderServer {
	t.Helper()
	settings := DefaultExtenderSettings()
	settings.CamouflageEnabled = true
	settings.IdentityKeySeed = mustSeed(t)
	// a healthy Phase A host: a verified name, and the splice off
	settings.CamouflageBorrowNames = []string{testBorrowName}
	server := NewExtenderServer(context.Background(), nil, nil, nil, &net.Dialer{}, settings)
	t.Cleanup(server.CloseAndWait)
	return server
}

// Sets one borrowed site's load, the way a splice adds to it.
func setTestCamouflageTargetLoad(server *ExtenderServer, name string, spliceCount int64, relayedByteCount int64) {
	server.camouflage.stateLock.Lock()
	defer server.camouflage.stateLock.Unlock()
	server.camouflage.targetLoadCounts[name] = &extenderCamouflageTargetLoad{
		spliceCount:      spliceCount,
		relayedByteCount: relayedByteCount,
	}
}

func TestExtenderCamouflageReportCountsAreTakenOnce(t *testing.T) {
	server := newTestCamouflageReportServer(t)
	server.camouflageAuthenticatedCount.Add(1)
	server.camouflageSplicedCount.Add(2)
	server.camouflageTerminatedCount.Add(3)
	server.camouflageSpliceFallbackTerminatedCount.Add(4)
	server.camouflageReplayRefusedCount.Add(5)
	server.camouflageTimeWindowFailedCount.Add(6)
	server.camouflageSpliceDialFailedCount.Add(7)
	server.camouflageBorrowVerifyFailCount.Add(8)
	setTestCamouflageTargetLoad(server, "borrowed.example", 9, 900)

	counts := server.TakeCamouflageReportCounts()
	want := &connect.ExtenderCamouflageExtenderCounts{
		Authenticated:            1,
		Spliced:                  2,
		Terminated:               3,
		SpliceFallbackTerminated: 4,
		ReplayRefused:            5,
		TimeWindowFailed:         6,
		SpliceDialFailed:         7,
		BorrowVerifyFailed:       8,
		Targets: []*connect.ExtenderCamouflageTargetCounts{
			{Name: "borrowed.example", SpliceCount: 9, RelayedByteCount: 900},
		},
		DegradedReason: ExtenderCamouflageDegradedSpliceOff,
	}
	if !reflect.DeepEqual(counts, want) {
		t.Fatalf("first take = %+v, want %+v", counts, want)
	}

	// nothing new: no counts and no sites, and still a verdict
	counts = server.TakeCamouflageReportCounts()
	want = &connect.ExtenderCamouflageExtenderCounts{DegradedReason: ExtenderCamouflageDegradedSpliceOff}
	if !reflect.DeepEqual(counts, want) {
		t.Fatalf("second take = %+v, want %+v", counts, want)
	}

	// only what is new
	server.camouflageSplicedCount.Add(10)
	setTestCamouflageTargetLoad(server, "borrowed.example", 11, 1000)
	counts = server.TakeCamouflageReportCounts()
	want = &connect.ExtenderCamouflageExtenderCounts{
		Spliced: 10,
		Targets: []*connect.ExtenderCamouflageTargetCounts{
			{Name: "borrowed.example", SpliceCount: 2, RelayedByteCount: 100},
		},
		DegradedReason: ExtenderCamouflageDegradedSpliceOff,
	}
	if !reflect.DeepEqual(counts, want) {
		t.Fatalf("third take = %+v, want %+v", counts, want)
	}
}

// The verdict is the honest one as of the report: a server with camouflage off says
// so, and only the coverage the status calls coverage reads covered.
func TestExtenderCamouflageReportCarriesTheVerdict(t *testing.T) {
	settings := DefaultExtenderSettings()
	off := NewExtenderServer(context.Background(), nil, nil, nil, &net.Dialer{}, settings)
	t.Cleanup(off.CloseAndWait)
	counts := off.TakeCamouflageReportCounts()
	if counts.Covered || counts.DegradedReason != ExtenderCamouflageDegradedCamouflageOff {
		t.Errorf("off = %+v", counts)
	}
	off.DiscardCamouflageReportCounts()

	coveredSettings := DefaultExtenderSettings()
	coveredSettings.CamouflageEnabled = true
	coveredSettings.IdentityKeySeed = mustSeed(t)
	coveredSettings.ExtenderCamouflageSplice = true
	coveredSettings.CamouflageBorrowNames = []string{testBorrowName}
	covered := NewExtenderServer(context.Background(), nil, nil, nil, &net.Dialer{}, coveredSettings)
	t.Cleanup(covered.CloseAndWait)
	counts = covered.TakeCamouflageReportCounts()
	if !counts.Covered || counts.DegradedReason != "" {
		t.Errorf("covered = %+v, status = %+v", counts, covered.CamouflageStatus())
	}

	// a fallback that ran is degraded, as the status says
	covered.camouflageSpliceFallbackTerminatedCount.Add(1)
	counts = covered.TakeCamouflageReportCounts()
	if counts.Covered || counts.DegradedReason != ExtenderCamouflageDegradedFallbackActive || counts.SpliceFallbackTerminated != 1 {
		t.Errorf("fallback = %+v", counts)
	}
}

// What is past a bound waits for the next take rather than being lost, and at most the
// report's count of sites goes in one take, the most bytes first.
func TestExtenderCamouflageReportHoldsTheExcess(t *testing.T) {
	server := newTestCamouflageReportServer(t)
	bound := connect.ExtenderCamouflageReportMaxExtenderCount
	server.camouflageAuthenticatedCount.Add(bound + 3)
	targetBound := connect.ExtenderCamouflageReportMaxTargetSpliceCount
	byteBound := connect.ExtenderCamouflageReportMaxTargetByteCount
	setTestCamouflageTargetLoad(server, "heavy.example", targetBound+1, byteBound+2)

	counts := server.TakeCamouflageReportCounts()
	if counts.Authenticated != bound {
		t.Errorf("first = %d", counts.Authenticated)
	}
	if target := counts.Targets[0]; target.SpliceCount != targetBound || target.RelayedByteCount != byteBound {
		t.Errorf("first target = %+v", target)
	}
	counts = server.TakeCamouflageReportCounts()
	if counts.Authenticated != 3 {
		t.Errorf("second = %d", counts.Authenticated)
	}
	if target := counts.Targets[0]; target.SpliceCount != 1 || target.RelayedByteCount != 2 {
		t.Errorf("second target = %+v", target)
	}

	maxTargets := connect.ExtenderCamouflageReportMaxTargetCount
	for i := range maxTargets + 3 {
		setTestCamouflageTargetLoad(server, fmt.Sprintf("site-%03d.example", i), 1, int64(i+1))
	}
	counts = server.TakeCamouflageReportCounts()
	if len(counts.Targets) != maxTargets {
		t.Fatalf("targets = %d", len(counts.Targets))
	}
	if first := counts.Targets[0]; first.Name != fmt.Sprintf("site-%03d.example", maxTargets+2) {
		t.Errorf("the most bytes = %+v", first)
	}
	counts = server.TakeCamouflageReportCounts()
	if len(counts.Targets) != 3 {
		t.Errorf("the ones that waited = %d", len(counts.Targets))
	}
	for _, target := range counts.Targets {
		if target.SpliceCount != 1 {
			t.Errorf("waited = %+v", target)
		}
	}
	// a name past the bound of the route never reports, and nothing else stops for it
	long := make([]byte, connect.ExtenderCamouflageReportMaxTargetNameByteCount+1)
	for i := range long {
		long[i] = 'a'
	}
	setTestCamouflageTargetLoad(server, string(long), 1, 1)
	if counts := server.TakeCamouflageReportCounts(); len(counts.Targets) != 0 {
		t.Errorf("a long name reported: %+v", counts.Targets[0])
	}
}

// A discard makes what is counted so far never reported: an account change.
func TestExtenderCamouflageReportDiscard(t *testing.T) {
	server := newTestCamouflageReportServer(t)
	server.camouflageSplicedCount.Add(5)
	setTestCamouflageTargetLoad(server, "borrowed.example", 5, 500)
	server.DiscardCamouflageReportCounts()
	server.camouflageSplicedCount.Add(1)
	setTestCamouflageTargetLoad(server, "borrowed.example", 6, 600)
	counts := server.TakeCamouflageReportCounts()
	if counts.Spliced != 1 || len(counts.Targets) != 1 || counts.Targets[0].SpliceCount != 1 || counts.Targets[0].RelayedByteCount != 100 {
		t.Errorf("after a discard = %+v, targets = %+v", counts, counts.Targets)
	}
}

// A new server is new counters and a new ledger together: nothing of the old one's
// comes through, and two senders on one server never have the same count.
func TestExtenderCamouflageReportRestartStartsAtZero(t *testing.T) {
	first := newTestCamouflageReportServer(t)
	first.camouflageSplicedCount.Add(5)
	if counts := first.TakeCamouflageReportCounts(); counts.Spliced != 5 {
		t.Errorf("first = %+v", counts)
	}
	second := newTestCamouflageReportServer(t)
	if counts := second.TakeCamouflageReportCounts(); counts.Spliced != 0 {
		t.Errorf("a new server reported %+v", counts)
	}
	second.camouflageSplicedCount.Add(2)
	if counts := second.TakeCamouflageReportCounts(); counts.Spliced != 2 {
		t.Errorf("second = %+v", counts)
	}
	if counts := second.TakeCamouflageReportCounts(); counts.Spliced != 0 {
		t.Errorf("a second take of one count = %+v", counts)
	}
}

// The server is what a sender takes its extender's part from, without connect knowing
// this package.
var _ connect.ExtenderCamouflageReportSource = (*ExtenderServer)(nil)

// A real splice comes through as load on its borrowed site, read once the close has
// joined the splice, which is when its relayed bytes are counted.
func TestExtenderCamouflageReportCarriesARealSplice(t *testing.T) {
	floodByteCount := 4096
	site := newFakeBorrowSite(t, floodBorrowSiteHandler(floodByteCount))
	siteCloseRelease := make(chan struct{})
	releaseSiteClose := sync.OnceFunc(func() { close(siteCloseRelease) })
	defer releaseSiteClose()
	fixture := newExtenderFixture(t, "127.0.0.1", func(settings *ExtenderSettings) {
		settings.CamouflageEnabled = true
		settings.IdentityKeySeed = mustSeed(t)
		settings.ExtenderCamouflageSplice = true
		settings.CamouflageBorrowNames = []string{testBorrowName}
		settings.CamouflageSpliceDialContext = site.dialContextHoldingClose(siteCloseRelease)
	})
	spliceProberReceiveByteCount(t, fixture)
	releaseSiteClose()
	fixture.server.CloseAndWait()

	counts := fixture.server.TakeCamouflageReportCounts()
	if counts.Spliced == 0 {
		t.Errorf("spliced = %+v", counts)
	}
	if len(counts.Targets) != 1 || counts.Targets[0].Name != testBorrowName || counts.Targets[0].SpliceCount == 0 || counts.Targets[0].RelayedByteCount == 0 {
		t.Errorf("targets = %+v", counts.Targets)
	}
}
