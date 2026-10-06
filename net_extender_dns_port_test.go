package connect

import (
	"crypto/ed25519"
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// The dns carrier ports a client dials (EXTENDER.md L2). A record lists every
// port that passed its activation probe; the client tries those first,
// ascending, and then whichever of 4053 and 53 the record does not list, so
// every extender is tried on both. One dial races them, so each candidate has
// one dns dialer whatever it lists.

// One signed record whose dns ports are exactly `dnsPorts`, with `dnsPort` as
// the single port an older reader sees.
func signTestDnsPortRecord(
	t *testing.T,
	rootPrivateKey ed25519.PrivateKey,
	clock *testClock,
	ip netip.Addr,
	dnsPort int,
	dnsPorts []int,
) *protocol.ExtenderRecord {
	t.Helper()
	recordDnsPorts := []uint32{}
	for _, recordDnsPort := range dnsPorts {
		recordDnsPorts = append(recordDnsPorts, uint32(recordDnsPort))
	}
	body := &protocol.ExtenderRecordBody{
		PublicKey:    newTestExtenderKey(t),
		Addresses:    []*protocol.ExtenderAddress{testExtenderAddress(ip.String(), ExtenderCarrierDns)},
		TcpPort:      443,
		UdpPort:      443,
		DnsPort:      uint32(dnsPort),
		DnsPorts:     recordDnsPorts,
		DnsTld:       "x.example.",
		IssueTimeMs:  uint64(clock.Now().UnixMilli()),
		ExpireTimeMs: uint64(clock.Now().Add(14 * 24 * time.Hour).UnixMilli()),
		NetworkHost:  testExtenderNetworkHost,
	}
	record, err := SignExtenderRecord(rootPrivateKey, body)
	if err != nil {
		t.Fatal(err)
	}
	return record
}

// The dns dialers of the first expand: every one the strategy drew for the
// candidates in the directory.
func testExtenderDnsDialers(t *testing.T, clientStrategy *ClientStrategy) []*clientDialer {
	t.Helper()
	dnsDialers := []*clientDialer{}
	for _, dialer := range clientStrategy.expandExtenderDialers() {
		if dialer.extenderConfig.Profile.ConnectMode == ExtenderConnectModeDns {
			dnsDialers = append(dnsDialers, dialer)
		}
	}
	return dnsDialers
}

// A record that lists both dns ports, as an sn miner's does, yields one dns
// dialer that races them, 53 first whatever order the record wrote them in.
func TestClientStrategyRacesBothListedDnsPortsOnOneDialer(t *testing.T) {
	clock := newTestClock()
	clientStrategy, directory, rootPrivateKey := newTestExtenderStrategy(t, clock, nil)

	ip := netip.MustParseAddr("192.0.2.121")
	record := signTestDnsPortRecord(t, rootPrivateKey, clock, ip, 4053, []int{4053, 53})
	if _, err := directory.ApplyRecord(record, ExtenderSourceFeed); err != nil {
		t.Fatal(err)
	}

	candidates := directory.Candidates(4, 4)
	if len(candidates) != 1 {
		t.Fatalf("candidates = %d, expected the one address", len(candidates))
	}
	if dnsPorts := candidates[0].DnsPorts; !slices.Equal(dnsPorts, []int{53, 4053}) {
		t.Fatalf("candidate dns ports = %v, expected the record's 53 and 4053", dnsPorts)
	}
	if candidates[0].DnsPort != 53 {
		t.Fatalf("candidate dns port = %d, expected the first listed", candidates[0].DnsPort)
	}

	dnsDialers := testExtenderDnsDialers(t, clientStrategy)
	if len(dnsDialers) != 1 {
		t.Fatalf("dns dialers = %d, expected one that races both ports", len(dnsDialers))
	}
	extenderConfig := dnsDialers[0].extenderConfig
	if dnsPorts := extenderConfig.DnsPorts; !slices.Equal(dnsPorts, []int{53, 4053}) {
		t.Fatalf("dialer dns ports = %v, expected 53 then 4053", dnsPorts)
	}
	if extenderConfig.Profile.Port != 53 {
		t.Fatalf("dialer port = %d, expected the first it races", extenderConfig.Profile.Port)
	}
}

// A record that lists 4053 alone, as every app extender's does, yields one dns
// dialer that races 4053 first and 53 behind it: the port the extender binds
// is dialed exactly as when it was the only one, and 53 is tried too.
func TestClientStrategyDialsA4053RecordOn4053Then53(t *testing.T) {
	clock := newTestClock()
	clientStrategy, directory, rootPrivateKey := newTestExtenderStrategy(t, clock, nil)

	ip := netip.MustParseAddr("192.0.2.122")
	record := signTestDnsPortRecord(t, rootPrivateKey, clock, ip, ExtenderDnsPort, []int{ExtenderDnsPort})
	if _, err := directory.ApplyRecord(record, ExtenderSourceFeed); err != nil {
		t.Fatal(err)
	}

	candidates := directory.Candidates(4, 4)
	if len(candidates) != 1 {
		t.Fatalf("candidates = %d, expected the one address", len(candidates))
	}
	if dnsPorts := candidates[0].DnsPorts; !slices.Equal(dnsPorts, []int{ExtenderDnsPort}) {
		t.Fatalf("candidate dns ports = %v, expected the record's 4053 alone", dnsPorts)
	}

	dnsDialers := testExtenderDnsDialers(t, clientStrategy)
	if len(dnsDialers) != 1 {
		t.Fatalf("dns dialers = %d, expected one", len(dnsDialers))
	}
	extenderConfig := dnsDialers[0].extenderConfig
	if dnsPorts := extenderConfig.DnsPorts; !slices.Equal(dnsPorts, []int{ExtenderDnsPort, DefaultDnsPort}) {
		t.Fatalf("dialer dns ports = %v, expected 4053 then 53", dnsPorts)
	}
	if extenderConfig.Profile.Port != ExtenderDnsPort {
		t.Fatalf("dialer port = %d, expected %d first", extenderConfig.Profile.Port, ExtenderDnsPort)
	}
}

// A record written before the port list, with the single port alone, is
// dialed as one that lists it.
func TestClientStrategyDialsTheSingleRecordDnsPortThen53(t *testing.T) {
	clock := newTestClock()
	clientStrategy, directory, rootPrivateKey := newTestExtenderStrategy(t, clock, nil)

	ip := netip.MustParseAddr("192.0.2.123")
	record := signTestDnsPortRecord(t, rootPrivateKey, clock, ip, ExtenderDnsPort, nil)
	if _, err := directory.ApplyRecord(record, ExtenderSourceFeed); err != nil {
		t.Fatal(err)
	}

	dnsDialers := testExtenderDnsDialers(t, clientStrategy)
	if len(dnsDialers) != 1 {
		t.Fatalf("dns dialers = %d, expected one", len(dnsDialers))
	}
	if dnsPorts := dnsDialers[0].extenderConfig.DnsPorts; !slices.Equal(dnsPorts, []int{ExtenderDnsPort, DefaultDnsPort}) {
		t.Fatalf("dialer dns ports = %v, expected 4053 then 53", dnsPorts)
	}
}

// An address no record names, which is dialed only when it was configured by
// hand, is dialed on the carrier defaults: 4053 first, then 53.
func TestClientStrategyDialsAnAddressWithNoRecordOn4053Then53(t *testing.T) {
	clock := newTestClock()
	clientStrategy, directory, _ := newTestExtenderStrategy(t, clock, nil)

	ip := netip.MustParseAddr("192.0.2.124")
	if !directory.AddManual(ip) {
		t.Fatal("the manual address was not added")
	}
	candidates := directory.Candidates(4, 4)
	if len(candidates) != 1 || candidates[0].Verified {
		t.Fatalf("candidates = %+v, expected the one unverified address", candidates)
	}
	if dnsPorts := candidates[0].DnsPorts; !slices.Equal(dnsPorts, []int{ExtenderDnsPort}) {
		t.Fatalf("candidate dns ports = %v, expected the carrier default", dnsPorts)
	}

	dnsDialers := testExtenderDnsDialers(t, clientStrategy)
	if len(dnsDialers) != 1 {
		t.Fatalf("dns dialers = %d, expected one", len(dnsDialers))
	}
	extenderConfig := dnsDialers[0].extenderConfig
	if dnsPorts := extenderConfig.DnsPorts; !slices.Equal(dnsPorts, []int{ExtenderDnsPort, DefaultDnsPort}) {
		t.Fatalf("dialer dns ports = %v, expected 4053 then 53", dnsPorts)
	}
	if extenderConfig.Profile.Port != ExtenderDnsPort {
		t.Fatalf("dialer port = %d, expected %d first", extenderConfig.Profile.Port, ExtenderDnsPort)
	}
}

// The feed, the network client's probe and the extender's peer pinger dial
// through the feed config, and race the same ports as the strategy: a record
// that lists 4053 alone is dialed on 4053 and then 53, one that lists both on
// 53 and then 4053.
func TestExtenderFeedConfigRacesBothDnsPorts(t *testing.T) {
	cases := []struct {
		recordDnsPorts []int
		dnsPorts       []int
	}{
		{recordDnsPorts: []int{ExtenderDnsPort}, dnsPorts: []int{ExtenderDnsPort, DefaultDnsPort}},
		{recordDnsPorts: []int{ExtenderDnsPort, DefaultDnsPort}, dnsPorts: []int{DefaultDnsPort, ExtenderDnsPort}},
	}
	for i, c := range cases {
		clock := newTestClock()
		_, directory, rootPrivateKey := newTestExtenderStrategy(t, clock, nil)
		ip := netip.AddrFrom4([4]byte{192, 0, 2, byte(130 + i)})
		record := signTestDnsPortRecord(t, rootPrivateKey, clock, ip, ExtenderDnsPort, c.recordDnsPorts)
		if _, err := directory.ApplyRecord(record, ExtenderSourceFeed); err != nil {
			t.Fatal(err)
		}
		candidates := directory.Candidates(4, 4)
		if len(candidates) != 1 {
			t.Fatalf("record %v: candidates = %d, expected the one address", c.recordDnsPorts, len(candidates))
		}
		extenderConfig := extenderFeedConfig(candidates[0], ExtenderConnectModeDns, nil)
		if extenderConfig == nil {
			t.Fatalf("record %v: no dns feed config", c.recordDnsPorts)
		}
		if !slices.Equal(extenderConfig.DnsPorts, c.dnsPorts) {
			t.Errorf("record %v: feed dns ports = %v, expected %v",
				c.recordDnsPorts, extenderConfig.DnsPorts, c.dnsPorts)
		}
		if extenderConfig.Profile.Port != c.dnsPorts[0] {
			t.Errorf("record %v: feed port = %d, expected %d",
				c.recordDnsPorts, extenderConfig.Profile.Port, c.dnsPorts[0])
		}
	}
}
