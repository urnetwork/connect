package connect

import (
	"fmt"
	"math/rand/v2"
	"net/netip"
	"regexp"
	"strings"
	"testing"
)

// The N1 table of lag.dev/PLAN.md, verbatim.
var extenderMeasurementNamePlanRows = []struct {
	address string
	name    string
}{
	{"203.0.113.7", "203-0-113-7.extender.bringyour.com"},
	{"::ffff:203.0.113.7", "203-0-113-7.extender.bringyour.com"},
	{"2001:db8::1", "2001-db8--1.extender.bringyour.com"},
	{"2001:db8:0:1:1:1:1:1", "2001-db8-0-1-1-1-1-1.extender.bringyour.com"},
	{"2001:db8::", "2001-db8--0.extender.bringyour.com"},
	{"::1", "0--1.extender.bringyour.com"},
}

// A label is lower case hex digits and dashes that starts and ends with a
// digit, and fits a dns label.
var extenderMeasurementLabelPattern = regexp.MustCompile(`^[0-9a-f]([0-9a-f-]{0,61}[0-9a-f])?$`)

func TestExtenderMeasurementNamePlanTable(t *testing.T) {
	for _, row := range extenderMeasurementNamePlanRows {
		addr := netip.MustParseAddr(row.address)
		if name := ExtenderMeasurementName(addr, ExtenderMeasurementZone); name != row.name {
			t.Errorf("name of %s = %q, want %q", row.address, name, row.name)
		}
		wantLabel, _, _ := strings.Cut(row.name, ".")
		if label := ExtenderMeasurementLabel(addr); label != wantLabel {
			t.Errorf("label of %s = %q, want %q", row.address, label, wantLabel)
		}
		parsed, ok := ParseExtenderMeasurementName(row.name, ExtenderMeasurementZone)
		if !ok || parsed != addr.Unmap() {
			t.Errorf("parse of %q = %v %v, want %v", row.name, parsed, ok, addr.Unmap())
		}
	}
}

// Padding at both ends, RFC 5952's choices, case and scope.
func TestExtenderMeasurementNameEdges(t *testing.T) {
	rows := []struct {
		address string
		label   string
		// the address the name parses back to, when not the address itself
		parsed string
	}{
		{address: "::", label: "0--0"},
		{address: "1::", label: "1--0"},
		{address: "0:0:1::", label: "0-0-1--0"},
		{address: "0.0.0.0", label: "0-0-0-0"},
		{address: "255.255.255.255", label: "255-255-255-255"},
		// a lone zero group is not compressed
		{address: "0:2:3:4:5:6:7:8", label: "0-2-3-4-5-6-7-8"},
		{address: "2001:db8:0:1:1:1:1:0", label: "2001-db8-0-1-1-1-1-0"},
		// the longest zero run is compressed, and the first of equal runs
		{address: "2001:0:0:1:0:0:0:1", label: "2001-0-0-1--1"},
		{address: "2001:db8:0:0:1:0:0:1", label: "2001-db8--1-0-0-1"},
		{address: "2001:DB8::A", label: "2001-db8--a", parsed: "2001:db8::a"},
		{address: "fe80::1%eth0", label: "fe80--1", parsed: "fe80::1"},
		// v4-mapped in hex is still plain ipv4
		{address: "::ffff:cb00:7107", label: "203-0-113-7", parsed: "203.0.113.7"},
		{address: "::ffff:0:0", label: "0-0-0-0", parsed: "0.0.0.0"},
		// not v4-mapped: dashes that were dots read back as ipv6 groups, so
		// this label is another address's, never 203.0.113.7's
		{address: "::ffff:203:0:113:7", label: "0--ffff-203-0-113-7"},
		{address: "64:ff9b::203.0.113.7", label: "64-ff9b--cb00-7107", parsed: "64:ff9b::cb00:7107"},
	}
	for _, row := range rows {
		addr := netip.MustParseAddr(row.address)
		if label := ExtenderMeasurementLabel(addr); label != row.label {
			t.Errorf("label of %s = %q, want %q", row.address, label, row.label)
			continue
		}
		want := addr
		if row.parsed != "" {
			want = netip.MustParseAddr(row.parsed)
		}
		parsed, ok := ParseExtenderMeasurementName(row.label+"."+ExtenderMeasurementZone, ExtenderMeasurementZone)
		if !ok || parsed != want {
			t.Errorf("parse of %q = %v %v, want %v", row.label, parsed, ok, want)
		}
	}
}

func TestExtenderMeasurementNameZone(t *testing.T) {
	addr := netip.MustParseAddr("203.0.113.7")
	for _, zone := range []string{"", ExtenderMeasurementZone, "Extender.BringYour.COM", "extender.bringyour.com.", " extender.bringyour.com "} {
		if name := ExtenderMeasurementName(addr, zone); name != "203-0-113-7.extender.bringyour.com" {
			t.Errorf("name in zone %q = %q", zone, name)
		}
		if parsed, ok := ParseExtenderMeasurementName("203-0-113-7.extender.bringyour.com", zone); !ok || parsed != addr {
			t.Errorf("parse in zone %q = %v %v", zone, parsed, ok)
		}
	}
	name := ExtenderMeasurementName(addr, "measure.example")
	if name != "203-0-113-7.measure.example" {
		t.Fatalf("name in another zone = %q", name)
	}
	if parsed, ok := ParseExtenderMeasurementName(name, "measure.example"); !ok || parsed != addr {
		t.Fatalf("parse in another zone = %v %v", parsed, ok)
	}
	if _, ok := ParseExtenderMeasurementName(name, ExtenderMeasurementZone); ok {
		t.Fatal("a name in another zone parsed in the default zone")
	}
	if _, ok := ParseExtenderMeasurementName("203-0-113-7.extender.bringyour.com", "measure.example"); ok {
		t.Fatal("a name in the default zone parsed in another zone")
	}
}

func TestExtenderMeasurementNameInvalidAddress(t *testing.T) {
	if label := ExtenderMeasurementLabel(netip.Addr{}); label != "" {
		t.Fatalf("label of the zero Addr = %q", label)
	}
	if name := ExtenderMeasurementName(netip.Addr{}, ExtenderMeasurementZone); name != "" {
		t.Fatalf("name of the zero Addr = %q", name)
	}
}

// Every spelling but the canonical one is refused.
func TestExtenderMeasurementNameRefusesNonCanonical(t *testing.T) {
	zone := ExtenderMeasurementZone
	names := map[string][]string{
		"upper case": {
			"2001-DB8--1.extender.bringyour.com",
			"2001-db8--A.extender.bringyour.com",
			"203-0-113-7.EXTENDER.bringyour.com",
			"203-0-113-7.extender.BringYour.com",
		},
		"uncompressed or otherwise compressed ipv6": {
			"2001-db8-0-0-0-0-0-1.extender.bringyour.com",
			"0-0-0-0-0-0-0-1.extender.bringyour.com",
			"0-0-0-0-0-0-0-0.extender.bringyour.com",
			"2001-0db8--1.extender.bringyour.com",
			// a lone zero group compressed
			"2001-db8--1-1-1-1-1.extender.bringyour.com",
			// the shorter of two runs compressed
			"2001--1-0-0-0-1.extender.bringyour.com",
			// the second of two equal runs compressed
			"2001-db8-0-0-1--1.extender.bringyour.com",
		},
		"non-canonical ipv4": {
			"203-0-113-07.extender.bringyour.com",
			"203-000-113-7.extender.bringyour.com",
			"203-0-113.extender.bringyour.com",
			"203-0-113-7-1.extender.bringyour.com",
			"203-0-113-256.extender.bringyour.com",
		},
		"missing pad": {
			"--1.extender.bringyour.com",
			"2001-db8--.extender.bringyour.com",
			"--.extender.bringyour.com",
			"0--.extender.bringyour.com",
			"--0.extender.bringyour.com",
		},
		"extra pad": {
			"00--1.extender.bringyour.com",
			"0-0--1.extender.bringyour.com",
			"2001-db8--00.extender.bringyour.com",
		},
		"v4-mapped or scoped spelling": {
			"0--ffff-cb00-7107.extender.bringyour.com",
			"0--ffff-203-0-113-7-.extender.bringyour.com",
			"fe80--1%eth0.extender.bringyour.com",
			"fe80--1-eth0.extender.bringyour.com",
		},
		"wrong zone": {
			"203-0-113-7.extender.bringyour.com.",
			"203-0-113-7.extender.bringyour.co",
			"203-0-113-7.bringyour.com",
			"203-0-113-7.xextender.bringyour.com",
			"203-0-113-7extender.bringyour.com",
			"203-0-113-7.extender.bringyour.com.example",
			"203-0-113-7.measure.example",
			"203-0-113-7",
		},
		"extra labels": {
			"www.203-0-113-7.extender.bringyour.com",
			"203.0.113.7.extender.bringyour.com",
			"2001-db8--1.203-0-113-7.extender.bringyour.com",
			"203-0-113-7..extender.bringyour.com",
		},
		"empty or not an address": {
			"",
			".extender.bringyour.com",
			"extender.bringyour.com",
			"-.extender.bringyour.com",
			"0.extender.bringyour.com",
			"localhost.extender.bringyour.com",
			" 203-0-113-7.extender.bringyour.com",
			"203-0-113-7 .extender.bringyour.com",
			"203_0_113_7.extender.bringyour.com",
			"٢٠٣-0-113-7.extender.bringyour.com",
		},
	}
	for kind, kindNames := range names {
		for _, name := range kindNames {
			if addr, ok := ParseExtenderMeasurementName(name, zone); ok {
				t.Errorf("%s: %q parsed as %v", kind, name, addr)
			}
		}
	}
}

// Random addresses, with ipv6 zero groups common enough to exercise every
// compression, name and parse back, and each one's other spellings fail.
func TestExtenderMeasurementNameRoundTrip(t *testing.T) {
	random := rand.New(rand.NewPCG(0x1a9, 0xde5))
	randomAddr := func() netip.Addr {
		switch random.IntN(3) {
		case 0:
			var a [4]byte
			for i := range a {
				a[i] = byte(random.IntN(256))
			}
			return netip.AddrFrom4(a)
		case 1:
			var a [16]byte
			for i := range a {
				a[i] = byte(random.IntN(256))
			}
			a[10], a[11] = 0xff, 0xff
			for i := 0; i < 10; i++ {
				a[i] = 0
			}
			return netip.AddrFrom16(a)
		default:
			var a [16]byte
			for group := 0; group < 8; group++ {
				if random.IntN(2) == 0 {
					continue
				}
				// short groups as often as full ones
				value := random.IntN(1 << (4 * (1 + random.IntN(4))))
				a[2*group], a[2*group+1] = byte(value>>8), byte(value)
			}
			return netip.AddrFrom16(a)
		}
	}
	zones := []string{ExtenderMeasurementZone, "measure.example"}
	for i := 0; i < 20000; i++ {
		addr := randomAddr()
		zone := zones[i%len(zones)]
		canonical := addr.Unmap()

		name := ExtenderMeasurementName(addr, zone)
		label, ok := strings.CutSuffix(name, "."+zone)
		if !ok || !extenderMeasurementLabelPattern.MatchString(label) {
			t.Fatalf("name of %s = %q is not a canonical label in %s", addr, name, zone)
		}
		parsed, ok := ParseExtenderMeasurementName(name, zone)
		if !ok || parsed != canonical || parsed.Is4In6() || parsed.Zone() != "" {
			t.Fatalf("parse of %q = %v %v, want %v", name, parsed, ok, canonical)
		}
		if again := ExtenderMeasurementName(parsed, zone); again != name {
			t.Fatalf("name of parsed %v = %q, want %q", parsed, again, name)
		}

		// spellings of the same address, or of none
		unpadded := label
		if strings.HasPrefix(unpadded, "0--") {
			unpadded = unpadded[1:]
		}
		if strings.HasSuffix(unpadded, "--0") {
			unpadded = unpadded[:len(unpadded)-1]
		}
		others := []string{
			strings.ToUpper(label) + "." + zone,
			label + "." + strings.ToUpper(zone),
			name + ".",
			"x." + name,
			unpadded + "." + zone,
		}
		if canonical.Is6() {
			others = append(
				others,
				strings.ReplaceAll(canonical.StringExpanded(), ":", "-")+"."+zone,
				strings.ReplaceAll(extenderMeasurementUncompressed(canonical), ":", "-")+"."+zone,
			)
		} else {
			a := canonical.As16()
			mapped := fmt.Sprintf("0--ffff-%x-%x", uint16(a[12])<<8|uint16(a[13]), uint16(a[14])<<8|uint16(a[15]))
			others = append(others, mapped+"."+zone)
		}
		for _, other := range others {
			if other == name {
				continue
			}
			if otherAddr, ok := ParseExtenderMeasurementName(other, zone); ok {
				t.Fatalf("%q, another spelling of %q, parsed as %v", other, name, otherAddr)
			}
		}
	}
}

// The eight groups without leading zeros or compression, as "2001:db8:0:0:0:0:0:1".
func extenderMeasurementUncompressed(addr netip.Addr) string {
	a := addr.As16()
	groups := make([]string, 8)
	for i := range groups {
		groups[i] = fmt.Sprintf("%x", uint16(a[2*i])<<8|uint16(a[2*i+1]))
	}
	return strings.Join(groups, ":")
}
