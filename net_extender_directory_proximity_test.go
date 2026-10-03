package connect

import (
	"crypto/ed25519"
	"net/netip"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// The proximity order of DESIGNNOTES4.md §4: the hinted continent first, then
// measured latency, and a sample that ages out counts as none.

// A signed record with a continent tag, which the operator stamps at signing.
func signTestRecordWithContinent(
	t *testing.T,
	rootPrivateKey ed25519.PrivateKey,
	clock *testClock,
	continentCode string,
	addresses ...*protocol.ExtenderAddress,
) *protocol.ExtenderRecord {
	t.Helper()
	body := &protocol.ExtenderRecordBody{
		PublicKey:     newTestExtenderKey(t),
		Addresses:     addresses,
		TcpPort:       443,
		UdpPort:       443,
		DnsPort:       53,
		DnsTld:        "x.example.",
		CountryCode:   "us",
		ContinentCode: continentCode,
		IssueTimeMs:   uint64(clock.Now().UnixMilli()),
		ExpireTimeMs:  uint64(clock.Now().Add(14 * 24 * time.Hour).UnixMilli()),
		NetworkHost:   testExtenderNetworkHost,
	}
	record, err := SignExtenderRecord(rootPrivateKey, body)
	if err != nil {
		t.Fatal(err)
	}
	return record
}

// One directory holding three verified v4 addresses: .1 on EU, .2 on NA, and
// .3 from a record that predates the continent field.
func newTestProximityDirectory(t *testing.T) (*ExtenderDirectory, *testClock) {
	t.Helper()
	clock := newTestClock()
	directory, rootPrivateKey := newTestExtenderDirectory(t, clock, nil)
	for _, entry := range []struct {
		ip        string
		continent string
	}{
		{ip: "192.0.2.1", continent: "EU"},
		{ip: "192.0.2.2", continent: "NA"},
		{ip: "192.0.2.3", continent: ""},
	} {
		record := signTestRecordWithContinent(t, rootPrivateKey, clock, entry.continent, testExtenderAddress(entry.ip))
		if _, err := directory.ApplyRecord(record, ExtenderSourceFeed); err != nil {
			t.Fatal(err)
		}
	}
	return directory, clock
}

func candidateIps(candidates []*ExtenderCandidate) []string {
	ips := []string{}
	for _, candidate := range candidates {
		ips = append(ips, candidate.Ip.String())
	}
	return ips
}

func assertIpOrder(t *testing.T, candidates []*ExtenderCandidate, expected ...string) {
	t.Helper()
	got := candidateIps(candidates)
	if len(got) != len(expected) {
		t.Fatalf("candidates = %v, expected %v", got, expected)
	}
	for i := range expected {
		if got[i] != expected[i] {
			t.Fatalf("candidates = %v, expected %v", got, expected)
		}
	}
}

func TestExtenderDirectoryCandidatesPreferTheHintedContinent(t *testing.T) {
	directory, _ := newTestProximityDirectory(t)

	// no hint: the order it always was
	assertIpOrder(t, directory.Candidates(4, 8), "192.0.2.1", "192.0.2.2", "192.0.2.3")

	if !directory.SetContinentHint("na") {
		t.Fatal("the hint did not change")
	}
	if directory.ContinentHint() != "NA" {
		t.Fatalf("hint = %q, expected NA", directory.ContinentHint())
	}
	// the hinted continent, then the other, then the record with none
	assertIpOrder(t, directory.Candidates(4, 8), "192.0.2.2", "192.0.2.1", "192.0.2.3")

	directory.SetContinentHint("EU")
	assertIpOrder(t, directory.Candidates(4, 8), "192.0.2.1", "192.0.2.2", "192.0.2.3")

	// a continent no record is on still puts the tagged records first
	directory.SetContinentHint("AS")
	assertIpOrder(t, directory.Candidates(4, 8), "192.0.2.1", "192.0.2.2", "192.0.2.3")

	// clearing the hint restores the plain order
	directory.SetContinentHint("")
	assertIpOrder(t, directory.Candidates(4, 8), "192.0.2.1", "192.0.2.2", "192.0.2.3")

	// and the tag is on the candidate and the status entry
	for _, candidate := range directory.Candidates(4, 8) {
		expected := map[string]string{"192.0.2.1": "EU", "192.0.2.2": "NA", "192.0.2.3": ""}[candidate.Ip.String()]
		if candidate.ContinentCode != expected {
			t.Fatalf("%s continent = %q, expected %q", candidate.Ip, candidate.ContinentCode, expected)
		}
	}
	entry := testDirectoryEntry(t, directory, netip.MustParseAddr("192.0.2.2"))
	if entry.ContinentCode != "NA" {
		t.Fatalf("entry continent = %q", entry.ContinentCode)
	}
}

func TestExtenderDirectoryContinentHintNormalizes(t *testing.T) {
	directory, _ := newTestProximityDirectory(t)
	if !directory.SetContinentHint(" na ") {
		t.Fatal("no change")
	}
	if directory.ContinentHint() != "NA" {
		t.Fatalf("hint = %q", directory.ContinentHint())
	}
	if directory.SetContinentHint("NA") {
		t.Fatal("the same hint reported a change")
	}
}

// Within a continent tier, a measured address comes first, ascending, and an
// unmeasured one last.
func TestExtenderDirectoryCandidatesOrderByLatency(t *testing.T) {
	directory, clock := newTestProximityDirectory(t)

	directory.RecordLatency(netip.MustParseAddr("192.0.2.3"), 20*time.Millisecond, false)
	directory.RecordLatency(netip.MustParseAddr("192.0.2.1"), 80*time.Millisecond, false)
	assertIpOrder(t, directory.Candidates(4, 8), "192.0.2.3", "192.0.2.1", "192.0.2.2")

	candidates := directory.Candidates(4, 8)
	if candidates[0].Latency != 20*time.Millisecond || candidates[1].Latency != 80*time.Millisecond || candidates[2].Latency != 0 {
		t.Fatalf("latencies = %s %s %s", candidates[0].Latency, candidates[1].Latency, candidates[2].Latency)
	}
	entry := testDirectoryEntry(t, directory, netip.MustParseAddr("192.0.2.3"))
	if entry.Latency != 20*time.Millisecond {
		t.Fatalf("entry latency = %s", entry.Latency)
	}

	// the continent tier is judged before the latency: with a hint for NA
	// the unmeasured NA address leads the measured EU and untagged ones
	directory.SetContinentHint("NA")
	assertIpOrder(t, directory.Candidates(4, 8), "192.0.2.2", "192.0.2.1", "192.0.2.3")
	directory.SetContinentHint("")

	// a sample that aged out counts as none
	clock.advance(directory.settings.LatencyMaxAge)
	assertIpOrder(t, directory.Candidates(4, 8), "192.0.2.1", "192.0.2.2", "192.0.2.3")
	if candidates := directory.Candidates(4, 8); candidates[0].Latency != 0 {
		t.Fatalf("an aged sample is still reported: %s", candidates[0].Latency)
	}

	// a new sample replaces it
	directory.RecordLatency(netip.MustParseAddr("192.0.2.2"), 5*time.Millisecond, true)
	assertIpOrder(t, directory.Candidates(4, 8), "192.0.2.2", "192.0.2.1", "192.0.2.3")
	if candidates := directory.Candidates(4, 8); !candidates[0].LatencyAttested {
		t.Fatal("the attested sample is not reported as attested")
	}
}

// The probe pass asks for the opposite within a tier: what has no sample
// first, so it measures what it does not know before refreshing what it does.
func TestExtenderDirectoryProbeCandidatesExploreFirst(t *testing.T) {
	directory, _ := newTestProximityDirectory(t)

	directory.RecordLatency(netip.MustParseAddr("192.0.2.3"), 20*time.Millisecond, false)
	directory.RecordLatency(netip.MustParseAddr("192.0.2.1"), 80*time.Millisecond, false)
	assertIpOrder(t, directory.ProbeCandidates(4, 8, false), "192.0.2.2", "192.0.2.3", "192.0.2.1")

	// the hint still leads
	directory.SetContinentHint("EU")
	assertIpOrder(t, directory.ProbeCandidates(4, 8, false), "192.0.2.1", "192.0.2.2", "192.0.2.3")
	directory.SetContinentHint("")

	// an attesting pass counts only attested samples, so every address is
	// unmeasured to it until it attests one
	assertIpOrder(t, directory.ProbeCandidates(4, 8, true), "192.0.2.1", "192.0.2.2", "192.0.2.3")
	directory.RecordLatency(netip.MustParseAddr("192.0.2.3"), 20*time.Millisecond, true)
	assertIpOrder(t, directory.ProbeCandidates(4, 8, true), "192.0.2.1", "192.0.2.2", "192.0.2.3")
	directory.RecordLatency(netip.MustParseAddr("192.0.2.1"), 80*time.Millisecond, true)
	assertIpOrder(t, directory.ProbeCandidates(4, 8, true), "192.0.2.2", "192.0.2.3", "192.0.2.1")

	// the count bounds the pass
	assertIpOrder(t, directory.ProbeCandidates(4, 1, true), "192.0.2.2")
	if len(directory.ProbeCandidates(4, 0, false)) != 0 {
		t.Fatal("a zero count returned candidates")
	}
}

func TestExtenderDirectoryMeasuredLatencies(t *testing.T) {
	directory, clock := newTestProximityDirectory(t)

	if latencies := directory.MeasuredLatencies(4, false); len(latencies) != 0 {
		t.Fatalf("latencies = %v before any sample", latencies)
	}
	directory.RecordLatency(netip.MustParseAddr("192.0.2.1"), 30*time.Millisecond, true)
	directory.RecordLatency(netip.MustParseAddr("192.0.2.2"), 90*time.Millisecond, false)

	if latencies := directory.MeasuredLatencies(4, false); len(latencies) != 2 {
		t.Fatalf("latencies = %v, expected two", latencies)
	}
	if latencies := directory.MeasuredLatencies(4, true); len(latencies) != 1 || latencies[0] != 30*time.Millisecond {
		t.Fatalf("attested latencies = %v, expected the one attested", latencies)
	}
	// another family has none
	if latencies := directory.MeasuredLatencies(6, false); len(latencies) != 0 {
		t.Fatalf("v6 latencies = %v", latencies)
	}
	// a held address does not count: it is not usable
	directory.RecordFailure(netip.MustParseAddr("192.0.2.1"), ExtenderConnectModeTcpTls)
	if latencies := directory.MeasuredLatencies(4, false); len(latencies) != 1 || latencies[0] != 90*time.Millisecond {
		t.Fatalf("latencies with a hold = %v", latencies)
	}
	// and an aged sample does not either
	clock.advance(directory.settings.LatencyMaxAge)
	if latencies := directory.MeasuredLatencies(4, false); len(latencies) != 0 {
		t.Fatalf("aged latencies = %v", latencies)
	}
}

// A sample for an address the directory does not know, or a non-positive
// rtt, changes nothing.
func TestExtenderDirectoryRecordLatencyIgnoresTheUnknown(t *testing.T) {
	directory, _ := newTestProximityDirectory(t)
	version, _ := directory.ChangeMonitor().Get()

	directory.RecordLatency(netip.MustParseAddr("192.0.2.99"), 10*time.Millisecond, false)
	directory.RecordLatency(netip.MustParseAddr("192.0.2.1"), 0, false)
	directory.RecordLatency(netip.MustParseAddr("192.0.2.1"), -1, false)
	directory.RecordLatency(netip.Addr{}, 10*time.Millisecond, false)

	if after, _ := directory.ChangeMonitor().Get(); after != version {
		t.Fatal("an ignored sample changed the directory")
	}
	if latencies := directory.MeasuredLatencies(4, false); len(latencies) != 0 {
		t.Fatalf("latencies = %v", latencies)
	}
}

// The verified-first rule still comes before proximity: a manual unverified
// address with a fast sample does not overtake a verified one without.
func TestExtenderDirectoryProximityDoesNotOvertakeVerification(t *testing.T) {
	directory, _ := newTestProximityDirectory(t)
	manualIp := netip.MustParseAddr("192.0.2.40")
	directory.AddManual(manualIp)
	directory.RecordLatency(manualIp, 1*time.Millisecond, false)

	candidates := directory.Candidates(4, 8)
	if candidates[len(candidates)-1].Ip != manualIp {
		t.Fatalf("candidates = %v, expected the unverified manual address last", candidateIps(candidates))
	}
}
