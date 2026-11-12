package connect

// The ports of one record address (EXTENDER.md S10): a router's mapping
// forwards one family's carriers on ports it picks while the other family's
// address answers on the bound ones, so a record names an address's own ports
// beside the record's. An address that names none is dialed on the record's,
// which is every record that predates the fields.

import (
	"bytes"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/urnetwork/connect/protocol"
)

// The address's ports take fields 4 to 6 after its first three, so a reader
// that predates them skips them as unknown fields, and an address that names
// none encodes exactly as it did before the fields existed.
func TestExtenderAddressPortsKeepWireNumbers(t *testing.T) {
	address := &protocol.ExtenderAddress{
		Ip:        "192.0.2.10",
		IpVersion: 4,
		Carriers:  []string{ExtenderCarrierTcp},
		TcpPort:   40443,
		UdpPort:   40444,
		DnsPorts:  []uint32{40053, 40054},
	}
	for _, field := range []struct {
		name   protoreflect.Name
		number protowire.Number
	}{
		{name: "TcpPort", number: 4},
		{name: "UdpPort", number: 5},
		{name: "DnsPorts", number: 6},
	} {
		descriptor := address.ProtoReflect().Descriptor().Fields().ByName(field.name)
		if descriptor == nil || descriptor.Number() != field.number {
			t.Fatalf("%s does not use field %d", field.name, field.number)
		}
	}

	var legacy []byte
	legacy = protowire.AppendBytes(protowire.AppendTag(legacy, 1, protowire.BytesType), []byte("192.0.2.10"))
	legacy = protowire.AppendVarint(protowire.AppendTag(legacy, 2, protowire.VarintType), 4)
	legacy = protowire.AppendBytes(protowire.AppendTag(legacy, 3, protowire.BytesType), []byte(ExtenderCarrierTcp))
	expected := slices.Clone(legacy)
	expected = protowire.AppendVarint(protowire.AppendTag(expected, 4, protowire.VarintType), 40443)
	expected = protowire.AppendVarint(protowire.AppendTag(expected, 5, protowire.VarintType), 40444)
	packedDnsPorts := protowire.AppendVarint(protowire.AppendVarint(nil, 40053), 40054)
	expected = protowire.AppendBytes(protowire.AppendTag(expected, 6, protowire.BytesType), packedDnsPorts)

	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(address)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(encoded, expected) {
		t.Fatalf("address bytes = %x, want %x", encoded, expected)
	}

	// an address that names no ports of its own is the address of before
	legacyAddress := proto.Clone(address).(*protocol.ExtenderAddress)
	legacyAddress.TcpPort = 0
	legacyAddress.UdpPort = 0
	legacyAddress.DnsPorts = nil
	encoded, err = proto.MarshalOptions{Deterministic: true}.Marshal(legacyAddress)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(encoded, legacy) {
		t.Fatalf("an address without ports encodes as %x, want the legacy %x", encoded, legacy)
	}
	// and a legacy address reads with no ports of its own
	var decoded protocol.ExtenderAddress
	if err := proto.Unmarshal(legacy, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.TcpPort != 0 || decoded.UdpPort != 0 || len(decoded.DnsPorts) != 0 {
		t.Fatalf("a legacy address read ports %d/%d/%v", decoded.TcpPort, decoded.UdpPort, decoded.DnsPorts)
	}
}

// A candidate dials its address's own ports when the record names them, and the
// record's otherwise: the v4 address a router forwards on 40443, 40444 and
// 40053 is dialed there, while the v6 address of the same record, and every
// address of a record that predates the fields, is dialed on the record's 443,
// 443 and 53.
func TestExtenderDirectoryCandidateDialsTheAddressPorts(t *testing.T) {
	clock := newTestClock()
	directory, rootPrivateKey := newTestExtenderDirectory(t, clock, nil)

	mapped := testExtenderAddress("192.0.2.10")
	mapped.TcpPort = 40443
	mapped.UdpPort = 40444
	mapped.DnsPorts = []uint32{40053}
	bound := testExtenderAddress("2001:db8::10")
	record := signTestRecord(
		t,
		rootPrivateKey,
		newTestExtenderKey(t),
		clock.Now(),
		clock.Now().Add(time.Hour),
		mapped,
		bound,
	)
	if _, err := directory.ApplyRecord(record, ExtenderSourceDns); err != nil {
		t.Fatal(err)
	}
	legacyRecord := signTestRecord(
		t,
		rootPrivateKey,
		newTestExtenderKey(t),
		clock.Now(),
		clock.Now().Add(time.Hour),
		testExtenderAddress("192.0.2.20"),
	)
	if _, err := directory.ApplyRecord(legacyRecord, ExtenderSourceDns); err != nil {
		t.Fatal(err)
	}

	candidates := map[netip.Addr]*ExtenderCandidate{}
	for _, candidate := range directory.Candidates(0, 8) {
		candidates[candidate.Ip] = candidate
	}
	for _, c := range []struct {
		ip       string
		tcpPort  int
		udpPort  int
		dnsPorts []int
	}{
		{ip: "192.0.2.10", tcpPort: 40443, udpPort: 40444, dnsPorts: []int{40053}},
		{ip: "2001:db8::10", tcpPort: 443, udpPort: 443, dnsPorts: []int{53}},
		{ip: "192.0.2.20", tcpPort: 443, udpPort: 443, dnsPorts: []int{53}},
	} {
		candidate := candidates[netip.MustParseAddr(c.ip)]
		if candidate == nil {
			t.Fatalf("%s is not a candidate", c.ip)
		}
		if candidate.TcpPort != c.tcpPort || candidate.UdpPort != c.udpPort ||
			candidate.DnsPort != c.dnsPorts[0] || !slices.Equal(candidate.DnsPorts, c.dnsPorts) {
			t.Fatalf("%s dials %d/%d/%d %v, expected %d/%d/%d %v",
				c.ip,
				candidate.TcpPort, candidate.UdpPort, candidate.DnsPort, candidate.DnsPorts,
				c.tcpPort, c.udpPort, c.dnsPorts[0], c.dnsPorts)
		}
	}
}

// Each family's post offers that family's ports when FamilyOfferedPorts and
// FamilyDnsPorts are set: the v4 post the router's, the v6 post the bound ones,
// read again at each post, so a mapping the router moves reaches only the v4
// post.
func TestExtenderActivatorOffersEachFamilyItsPorts(t *testing.T) {
	var stateLock sync.Mutex
	mappedTcpPort := 40443
	fixture := newTestActivatorFixture(t, func(settings *ExtenderActivatorSettings) {
		// the configured ports, which a family the router does not forward
		// keeps
		settings.TcpPort = ExtenderTcpPort
		settings.UdpPort = ExtenderQuicPort
		settings.DnsPort = ExtenderDnsPort
		// set too, as an older caller does, and replaced by the family seams
		settings.OfferedPorts = func() (int, int, int) {
			return 1, 1, 1
		}
		settings.DnsPorts = func() []int {
			return []int{1}
		}
		settings.FamilyOfferedPorts = func(ipVersion int) (int, int, int) {
			stateLock.Lock()
			defer stateLock.Unlock()
			if ipVersion == 4 {
				return mappedTcpPort, 40444, 40053
			}
			return 0, 0, 0
		}
		settings.FamilyDnsPorts = func(ipVersion int) []int {
			if ipVersion == 4 {
				return []int{40053}
			}
			return []int{ExtenderDnsPort}
		}
	})

	expectPosts := func(v4TcpPort int) {
		t.Helper()
		posts := map[int]*testActivatePost{}
		for range 2 {
			post := fixture.waitPost()
			posts[post.ipVersion] = post
		}
		for ipVersion, expected := range map[int]struct {
			tcpPort  int
			udpPort  int
			dnsPort  int
			dnsPorts []int
		}{
			4: {tcpPort: v4TcpPort, udpPort: 40444, dnsPort: 40053, dnsPorts: []int{40053}},
			6: {tcpPort: ExtenderTcpPort, udpPort: ExtenderQuicPort, dnsPort: ExtenderDnsPort, dnsPorts: []int{ExtenderDnsPort}},
		} {
			post := posts[ipVersion]
			if post == nil {
				t.Fatalf("no v%d post", ipVersion)
			}
			if post.args.TcpPort != expected.tcpPort || post.args.UdpPort != expected.udpPort ||
				post.args.DnsPort != expected.dnsPort || !slices.Equal(post.args.DnsPorts, expected.dnsPorts) {
				t.Fatalf("v%d offers %d/%d/%d %v, expected %d/%d/%d %v",
					ipVersion,
					post.args.TcpPort, post.args.UdpPort, post.args.DnsPort, post.args.DnsPorts,
					expected.tcpPort, expected.udpPort, expected.dnsPort, expected.dnsPorts)
			}
		}
	}

	fixture.waitPass()
	expectPosts(40443)

	// the router moved the v4 mapping
	func() {
		stateLock.Lock()
		defer stateLock.Unlock()
		mappedTcpPort = 41443
	}()
	fixture.activator.Reactivate()
	expectPosts(41443)
}

// A carrier change compares each family's standing post with the dns ports that
// family offers (FamilyDnsPorts): neither family is missing a port the other
// one offers, and a port that returns on one family posts that family alone.
func TestExtenderActivatorComparesEachFamilyWithItsDnsPorts(t *testing.T) {
	var stateLock sync.Mutex
	familyDnsPorts := map[int][]int{
		4: {40053},
		6: {ExtenderDnsPort},
	}
	fixture := newTestActivatorFixtureActivated(t, func(settings *ExtenderActivatorSettings) {
		settings.CarrierReturnJitter = -1
		settings.FamilyDnsPorts = func(ipVersion int) []int {
			stateLock.Lock()
			defer stateLock.Unlock()
			return slices.Clone(familyDnsPorts[ipVersion])
		}
	})
	fixture.step(time.Minute)

	fixture.activator.CarriersChanged()
	fixture.step(time.Minute)
	fixture.expectFamilyPostCounts("with each family's ports on its record", 1, 1)

	func() {
		stateLock.Lock()
		defer stateLock.Unlock()
		familyDnsPorts[6] = []int{DefaultDnsPort, ExtenderDnsPort}
	}()
	fixture.activator.CarriersChanged()
	post := fixture.waitPost()
	if post.ipVersion != 6 || !slices.Equal(post.args.DnsPorts, []int{DefaultDnsPort, ExtenderDnsPort}) {
		t.Fatalf("post = v%d of %v, expected v6 with the returned port", post.ipVersion, post.args.DnsPorts)
	}
	fixture.waitActivation()
	fixture.step(time.Minute)
	fixture.expectFamilyPostCounts("with the v6 port returned", 1, 2)
}
