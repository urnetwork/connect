package connect

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// The probe pass and the continent hint of the network client
// (DESIGNNOTES4.md §4), over the probe and hint seams.

// One TXT value carrying a continent-tagged record for one ip.
func testExtenderDnsRecordTxtWithContinent(
	t *testing.T,
	rootPrivateKey ed25519.PrivateKey,
	clock *testClock,
	continentCode string,
	ip string,
) string {
	t.Helper()
	record := signTestRecordWithContinent(t, rootPrivateKey, clock, continentCode, testExtenderAddress(ip))
	txt, err := EncodeExtenderDnsRecord(&protocol.ExtenderGossipMessage{
		Message: &protocol.ExtenderGossipMessage_Record{Record: record},
	})
	if err != nil {
		t.Fatal(err)
	}
	return txt
}

// The probes a test's client made, in order, with the attestor each carried.
type testProbeLog struct {
	stateLock sync.Mutex
	ips       []string
	attested  []bool
	probed    chan struct{}
	rtts      map[string]time.Duration
}

func newTestProbeLog(rtts map[string]time.Duration) *testProbeLog {
	return &testProbeLog{
		probed: make(chan struct{}, 64),
		rtts:   rtts,
	}
}

func (self *testProbeLog) probe(
	ctx context.Context,
	candidate *ExtenderCandidate,
	attestor *ExtenderProbeAttestor,
) (time.Duration, bool, error) {
	self.stateLock.Lock()
	self.ips = append(self.ips, candidate.Ip.String())
	self.attested = append(self.attested, attestor != nil)
	rtt, ok := self.rtts[candidate.Ip.String()]
	self.stateLock.Unlock()
	select {
	case self.probed <- struct{}{}:
	default:
	}
	if !ok {
		return 0, false, fmt.Errorf("no route to %s in this test", candidate.Ip)
	}
	return rtt, attestor != nil, nil
}

func (self *testProbeLog) count() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return len(self.ips)
}

func (self *testProbeLog) waitForProbes(t *testing.T, count int) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for self.count() < count {
		select {
		case <-self.probed:
		case <-deadline:
			t.Fatalf("probes = %v, expected %d", self.ips, count)
		}
	}
}

// A client on EU with two EU extenders and two NA ones: the hinted continent
// is probed first, and once the window holds two close extenders the pass
// stops without ever probing the others.
func TestExtenderNetworkClientProbesTheHintedContinentFirstAndStops(t *testing.T) {
	clock := newTestClock()
	rootPrivateKey, rootPublicKey := newTestRootKeyPair(t)
	probes := newTestProbeLog(map[string]time.Duration{
		"192.0.2.10": 20 * time.Millisecond,
		"192.0.2.11": 25 * time.Millisecond,
		"192.0.2.20": 120 * time.Millisecond,
		"192.0.2.21": 130 * time.Millisecond,
	})
	txts := []string{
		testExtenderDnsRecordTxtWithContinent(t, rootPrivateKey, clock, "NA", "192.0.2.20"),
		testExtenderDnsRecordTxtWithContinent(t, rootPrivateKey, clock, "NA", "192.0.2.21"),
		testExtenderDnsRecordTxtWithContinent(t, rootPrivateKey, clock, "EU", "192.0.2.10"),
		testExtenderDnsRecordTxtWithContinent(t, rootPrivateKey, clock, "EU", "192.0.2.11"),
	}

	networkClient, directory, _ := newTestExtenderNetworkClientWithDirectory(
		t,
		clock,
		func(settings *ExtenderDirectorySettings) {
			// the feed loop dials every candidate against the dead strategy
			// and fails; with no hold the candidates stay what the records
			// made them
			settings.HoldTimeout = 0
			settings.MaxHoldTimeout = 0
		},
		func(settings *ExtenderNetworkClientSettings) {
			settings.ProbeWindowCount = 2
			settings.ProbeMaxCandidateCount = 8
			settings.ProbeCountPerExtender = 1
			settings.ProbeCloseFactor = 2
			settings.ProbeCloseFloor = 10 * time.Millisecond
			settings.Probe = probes.probe
			settings.Hint = func(ctx context.Context) (string, error) {
				return "eu", nil
			}
			settings.ResolveDns = func(ctx context.Context, name string) ([]netip.Addr, error) {
				return nil, nil
			}
			settings.ResolveDnsTxt = func(ctx context.Context, name string) ([]string, error) {
				return txts, nil
			}
			settings.Hello = func(ctx context.Context) (*ExtenderHelloResult, error) {
				return &ExtenderHelloResult{
					RootPublicKeyHexes: []string{ExtenderKeySeedHex(rootPublicKey)},
				}, nil
			}
		},
	)

	probes.waitForProbes(t, 2)
	// both EU extenders are close enough (within 10 ms or 2x the best 20 ms),
	// so the window is full and nothing else is probed
	select {
	case <-probes.probed:
	case <-time.After(300 * time.Millisecond):
	}
	probes.stateLock.Lock()
	ips := append([]string(nil), probes.ips...)
	probes.stateLock.Unlock()
	if len(ips) != 2 {
		t.Fatalf("probes = %v, expected exactly the two EU extenders", ips)
	}
	for _, ip := range ips {
		if ip != "192.0.2.10" && ip != "192.0.2.11" {
			t.Fatalf("probes = %v, expected only EU extenders", ips)
		}
	}
	// a ranking client attests nothing
	for _, attested := range probes.attested {
		if attested {
			t.Fatal("a ranking probe carried an attestor")
		}
	}

	if directory.ContinentHint() != "EU" {
		t.Fatalf("hint = %q, expected the operator's EU", directory.ContinentHint())
	}
	status := networkClient.Status()
	if status.ContinentHint != "EU" {
		t.Fatalf("status hint = %q", status.ContinentHint)
	}
	if status.LastProbeTime.IsZero() {
		t.Fatal("the status carries no probe time")
	}
	// the measured EU extenders lead, best first, then the unmeasured NA ones
	candidates := directory.Candidates(4, 8)
	assertIpOrder(t, candidates, "192.0.2.10", "192.0.2.11", "192.0.2.20", "192.0.2.21")
	if candidates[0].Latency != 20*time.Millisecond || candidates[2].Latency != 0 {
		t.Fatalf("latencies = %s / %s", candidates[0].Latency, candidates[2].Latency)
	}
}

// An installed attestor re-measures what a ranking pass already measured,
// because those samples were never attested, and every probe carries it.
func TestExtenderNetworkClientAttestsWhenTheProviderStarts(t *testing.T) {
	clock := newTestClock()
	rootPrivateKey, rootPublicKey := newTestRootKeyPair(t)
	probes := newTestProbeLog(map[string]time.Duration{
		"192.0.2.10": 20 * time.Millisecond,
		"192.0.2.11": 25 * time.Millisecond,
	})
	txts := []string{
		testExtenderDnsRecordTxtWithContinent(t, rootPrivateKey, clock, "EU", "192.0.2.10"),
		testExtenderDnsRecordTxtWithContinent(t, rootPrivateKey, clock, "EU", "192.0.2.11"),
	}
	networkClient, directory, _ := newTestExtenderNetworkClientWithDirectory(
		t,
		clock,
		func(settings *ExtenderDirectorySettings) {
			settings.HoldTimeout = 0
			settings.MaxHoldTimeout = 0
		},
		func(settings *ExtenderNetworkClientSettings) {
			settings.ProbeWindowCount = 2
			settings.ProbeMaxCandidateCount = 8
			settings.ProbeCountPerExtender = 1
			settings.Probe = probes.probe
			settings.ResolveDns = func(ctx context.Context, name string) ([]netip.Addr, error) {
				return nil, nil
			}
			settings.ResolveDnsTxt = func(ctx context.Context, name string) ([]string, error) {
				return txts, nil
			}
			settings.Hello = func(ctx context.Context) (*ExtenderHelloResult, error) {
				return &ExtenderHelloResult{
					RootPublicKeyHexes: []string{ExtenderKeySeedHex(rootPublicKey)},
				}, nil
			}
		},
	)
	probes.waitForProbes(t, 2)
	for _, candidate := range directory.Candidates(4, 8) {
		if candidate.LatencyAttested {
			t.Fatal("a ranking sample is attested")
		}
	}

	attestor, _ := newTestProbeAttestor(t)
	networkClient.SetProbeAttestor(attestor)
	probes.waitForProbes(t, 4)
	probes.stateLock.Lock()
	attested := append([]bool(nil), probes.attested...)
	probes.stateLock.Unlock()
	if attested[0] || attested[1] || !attested[2] || !attested[3] {
		t.Fatalf("attested = %v, expected the two probes after the install to attest", attested)
	}
	for _, candidate := range directory.Candidates(4, 8) {
		if !candidate.LatencyAttested {
			t.Fatalf("%s is not attested after the provider pass", candidate.Ip)
		}
	}

	// the provider stops: every sample is current for a ranking pass, so
	// nothing more is probed
	before := probes.count()
	networkClient.SetProbeAttestor(nil)
	time.Sleep(200 * time.Millisecond)
	if after := probes.count(); after != before {
		t.Fatalf("clearing the attestor probed again: %d -> %d", before, after)
	}
}

// With no operator hint the dns bootstrap implies one: the geo dns answered
// one continent's set, and its records agree. A split answer implies nothing,
// and the operator's hint, when it comes, is not overridden.
func TestExtenderNetworkClientInfersTheContinentFromDns(t *testing.T) {
	waitForHint := func(t *testing.T, directory *ExtenderDirectory, expected string) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for directory.ContinentHint() != expected {
			if time.Now().After(deadline) {
				t.Fatalf("hint = %q, expected %q", directory.ContinentHint(), expected)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	newClient := func(t *testing.T, continents []string, hint func(ctx context.Context) (string, error)) *ExtenderDirectory {
		t.Helper()
		clock := newTestClock()
		rootPrivateKey, rootPublicKey := newTestRootKeyPair(t)
		txts := []string{}
		for i, continentCode := range continents {
			txts = append(txts, testExtenderDnsRecordTxtWithContinent(
				t, rootPrivateKey, clock, continentCode, fmt.Sprintf("192.0.2.%d", 50+i)))
		}
		_, directory, _ := newTestExtenderNetworkClientWithDirectory(
			t,
			clock,
			nil,
			func(settings *ExtenderNetworkClientSettings) {
				settings.Hint = hint
				settings.ResolveDns = func(ctx context.Context, name string) ([]netip.Addr, error) {
					return nil, nil
				}
				settings.ResolveDnsTxt = func(ctx context.Context, name string) ([]string, error) {
					return txts, nil
				}
				settings.Hello = func(ctx context.Context) (*ExtenderHelloResult, error) {
					return &ExtenderHelloResult{
						RootPublicKeyHexes: []string{ExtenderKeySeedHex(rootPublicKey)},
					}, nil
				}
			},
		)
		return directory
	}

	// the operator cannot be reached: the dns says NA
	directory := newClient(t, []string{"NA", "NA", "NA"}, func(ctx context.Context) (string, error) {
		return "", fmt.Errorf("no operator in this test")
	})
	waitForHint(t, directory, "NA")

	// the operator cannot place the caller: the dns still says NA
	directory = newClient(t, []string{"NA", "NA"}, func(ctx context.Context) (string, error) {
		return "", nil
	})
	waitForHint(t, directory, "NA")

	// a majority is enough; a split is not a hint
	directory = newClient(t, []string{"NA", "NA", "EU"}, func(ctx context.Context) (string, error) {
		return "", fmt.Errorf("no operator in this test")
	})
	waitForHint(t, directory, "NA")
	directory = newClient(t, []string{"NA", "EU"}, func(ctx context.Context) (string, error) {
		return "", fmt.Errorf("no operator in this test")
	})
	// give the bootstrap time to have run, then check nothing was inferred
	waitForDirectoryChanges(t, directory, 1)
	time.Sleep(50 * time.Millisecond)
	if directory.ContinentHint() != "" {
		t.Fatalf("a split dns answer inferred %q", directory.ContinentHint())
	}

	// the operator's hint wins over the dns
	directory = newClient(t, []string{"NA", "NA"}, func(ctx context.Context) (string, error) {
		return "AS", nil
	})
	waitForHint(t, directory, "AS")
	time.Sleep(50 * time.Millisecond)
	if directory.ContinentHint() != "AS" {
		t.Fatalf("the dns overrode the operator: %q", directory.ContinentHint())
	}
}

// The close window: within the factor of the best, or under the floor.
func TestExtenderCloseCount(t *testing.T) {
	ms := func(n int) time.Duration { return time.Duration(n) * time.Millisecond }
	if count := extenderCloseCount(nil, 2, ms(50)); count != 0 {
		t.Fatalf("empty = %d", count)
	}
	// best 20: within 40, or under 50 -> 50 admits more
	if count := extenderCloseCount([]time.Duration{ms(20), ms(45), ms(55)}, 2, ms(50)); count != 2 {
		t.Fatalf("floor case = %d", count)
	}
	// best 100: within 200 -> the factor admits more than the floor
	if count := extenderCloseCount([]time.Duration{ms(100), ms(190), ms(210)}, 2, ms(50)); count != 2 {
		t.Fatalf("factor case = %d", count)
	}
	// a factor under one is the best alone
	if count := extenderCloseCount([]time.Duration{ms(100), ms(101)}, 0, 0); count != 1 {
		t.Fatalf("factor < 1 = %d", count)
	}
}

// The probe seam is bounded by the probe timeout.
func TestExtenderNetworkClientProbeTimeoutBoundsTheSeam(t *testing.T) {
	clock := newTestClock()
	rootPrivateKey, rootPublicKey := newTestRootKeyPair(t)
	txts := []string{testExtenderDnsRecordTxtWithContinent(t, rootPrivateKey, clock, "EU", "192.0.2.10")}
	deadlines := make(chan bool, 4)
	newTestExtenderNetworkClientWithDirectory(
		t,
		clock,
		func(settings *ExtenderDirectorySettings) {
			settings.HoldTimeout = 0
			settings.MaxHoldTimeout = 0
		},
		func(settings *ExtenderNetworkClientSettings) {
			settings.ProbeWindowCount = 1
			settings.ProbeTimeout = 123 * time.Millisecond
			settings.Probe = func(ctx context.Context, candidate *ExtenderCandidate, attestor *ExtenderProbeAttestor) (time.Duration, bool, error) {
				_, hasDeadline := ctx.Deadline()
				select {
				case deadlines <- hasDeadline:
				default:
				}
				return 10 * time.Millisecond, false, nil
			}
			settings.ResolveDns = func(ctx context.Context, name string) ([]netip.Addr, error) {
				return nil, nil
			}
			settings.ResolveDnsTxt = func(ctx context.Context, name string) ([]string, error) {
				return txts, nil
			}
			settings.Hello = func(ctx context.Context) (*ExtenderHelloResult, error) {
				return &ExtenderHelloResult{
					RootPublicKeyHexes: []string{ExtenderKeySeedHex(rootPublicKey)},
				}, nil
			}
		},
	)
	select {
	case hasDeadline := <-deadlines:
		if !hasDeadline {
			t.Fatal("the probe context carries no deadline")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no probe")
	}
}
