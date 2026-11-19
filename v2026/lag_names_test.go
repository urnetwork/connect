package connect

import (
	"bytes"
	"math/rand/v2"
	"regexp"
	"slices"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/urnetwork/connect/v2026/protocol"
)

// The N1 example of lag.dev/PLAN.md: the id's bytes in order, as lower case
// hex grouped 8-4-4-4-12.
const testExtenderMeasurementName = "0192e4a1-7b3c-7d2e-9f10-5a6b7c8d9e0f.extender.bringyour.com"

var testExtenderMeasurementId = Id{0x01, 0x92, 0xe4, 0xa1, 0x7b, 0x3c, 0x7d, 0x2e, 0x9f, 0x10, 0x5a, 0x6b, 0x7c, 0x8d, 0x9e, 0x0f}

var extenderMeasurementLabelPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func TestExtenderMeasurementNamePlanExample(t *testing.T) {
	name := ExtenderMeasurementName(testExtenderMeasurementId, ExtenderMeasurementZone)
	if name != testExtenderMeasurementName {
		t.Fatalf("name = %q, want %q", name, testExtenderMeasurementName)
	}
	// the label is the id as every api and log writes it
	if label, _, _ := strings.Cut(name, "."); label != testExtenderMeasurementId.String() {
		t.Fatalf("label %q is not the id %s", label, testExtenderMeasurementId)
	}
	extenderId, ok := ParseExtenderMeasurementName(testExtenderMeasurementName, ExtenderMeasurementZone)
	if !ok || extenderId != testExtenderMeasurementId {
		t.Fatalf("parse = %v %v, want %v", extenderId, ok, testExtenderMeasurementId)
	}
}

func TestExtenderMeasurementNameZone(t *testing.T) {
	for _, zone := range []string{"", ExtenderMeasurementZone, "Extender.BringYour.COM", "extender.bringyour.com.", " extender.bringyour.com "} {
		if name := ExtenderMeasurementName(testExtenderMeasurementId, zone); name != testExtenderMeasurementName {
			t.Errorf("name in zone %q = %q", zone, name)
		}
		if extenderId, ok := ParseExtenderMeasurementName(testExtenderMeasurementName, zone); !ok || extenderId != testExtenderMeasurementId {
			t.Errorf("parse in zone %q = %v %v", zone, extenderId, ok)
		}
	}
	// one trailing dot is the root, a second is not
	if name := ExtenderMeasurementName(testExtenderMeasurementId, "extender.bringyour.com.."); name == testExtenderMeasurementName {
		t.Errorf("a zone with two trailing dots was normalized to the default")
	}

	name := ExtenderMeasurementName(testExtenderMeasurementId, "Measure.Example.")
	if name != "0192e4a1-7b3c-7d2e-9f10-5a6b7c8d9e0f.measure.example" {
		t.Fatalf("name in another zone = %q", name)
	}
	if extenderId, ok := ParseExtenderMeasurementName(name, "measure.example"); !ok || extenderId != testExtenderMeasurementId {
		t.Fatalf("parse in another zone = %v %v", extenderId, ok)
	}
	if _, ok := ParseExtenderMeasurementName(name, ExtenderMeasurementZone); ok {
		t.Fatal("a name in another zone parsed in the default zone")
	}
	if _, ok := ParseExtenderMeasurementName(testExtenderMeasurementName, "measure.example"); ok {
		t.Fatal("a name in the default zone parsed in another zone")
	}
}

// The zero Id is how the rest of the package says "no id": an extender that
// has not learned its id yet has no name to ask a certificate for.
func TestExtenderMeasurementNameUnsetId(t *testing.T) {
	if name := ExtenderMeasurementName(Id{}, ExtenderMeasurementZone); name != "" {
		t.Fatalf("name of the zero Id = %q", name)
	}
	if extenderId, ok := ParseExtenderMeasurementName(Id{}.String()+"."+ExtenderMeasurementZone, ExtenderMeasurementZone); ok {
		t.Fatalf("the zero Id's spelling parsed as %v", extenderId)
	}
}

// A record carries the measurement id as the id's 16 raw bytes on field 17
// (C10), which is all a reader of the feed or gossip needs for the name (N4).
// A record without it names no measurement API.
func TestExtenderMeasurementIdOnRecord(t *testing.T) {
	body := &protocol.ExtenderRecordBody{MeasurementId: testExtenderMeasurementId.Bytes()}
	field := body.ProtoReflect().Descriptor().Fields().ByName("MeasurementId")
	if field == nil || field.Number() != 17 || field.Kind() != protoreflect.BytesKind {
		t.Fatal("MeasurementId is not bytes field 17")
	}
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	expected := protowire.AppendBytes(protowire.AppendTag(nil, 17, protowire.BytesType), testExtenderMeasurementId.Bytes())
	if !bytes.Equal(encoded, expected) {
		t.Fatalf("record measurement id bytes = %x, want %x", encoded, expected)
	}

	rootPrivateKey, rootPublicKey := newTestLagKey(t)
	record, err := SignExtenderRecord(rootPrivateKey, body)
	if err != nil {
		t.Fatal(err)
	}
	verifiedBody, err := NewExtenderRootKeySet(rootPublicKey).VerifyRecord(record)
	if err != nil {
		t.Fatal(err)
	}
	extenderId, err := IdFromBytes(verifiedBody.MeasurementId)
	if err != nil {
		t.Fatal(err)
	}
	if name := ExtenderMeasurementName(extenderId, ExtenderMeasurementZone); name != testExtenderMeasurementName {
		t.Fatalf("name from the record = %q, want %q", name, testExtenderMeasurementName)
	}

	var without protocol.ExtenderRecordBody
	if err := proto.Unmarshal(nil, &without); err != nil {
		t.Fatal(err)
	}
	if len(without.MeasurementId) != 0 {
		t.Fatalf("a record without the field has measurement id %x", without.MeasurementId)
	}
	if extenderId, err := IdFromBytes(without.MeasurementId); err == nil {
		t.Fatalf("a record without the field yields the id %v", extenderId)
	}
}

// Every spelling but the canonical one is refused.
func TestExtenderMeasurementNameRefusesNonCanonical(t *testing.T) {
	names := map[string][]string{
		"upper case": {
			"0192E4A1-7B3C-7D2E-9F10-5A6B7C8D9E0F.extender.bringyour.com",
			"0192e4a1-7b3c-7d2e-9f10-5a6b7c8d9e0F.extender.bringyour.com",
			"0192e4a1-7b3c-7d2e-9f10-5a6b7c8d9e0f.EXTENDER.bringyour.com",
			"0192e4a1-7b3c-7d2e-9f10-5a6b7c8d9e0f.extender.BringYour.com",
		},
		"missing hyphens": {
			// all four, which ParseId reads as the same id
			"0192e4a17b3c7d2e9f105a6b7c8d9e0f.extender.bringyour.com",
			"0192e4a17b3c-7d2e-9f10-5a6b7c8d9e0f.extender.bringyour.com",
			"0192e4a1-7b3c-7d2e-9f105a6b7c8d9e0f.extender.bringyour.com",
		},
		"another character in a hyphen's place": {
			// ParseId skips these four positions whatever they hold
			"0192e4a1_7b3c_7d2e_9f10_5a6b7c8d9e0f.extender.bringyour.com",
			"0192e4a1:7b3c:7d2e:9f10:5a6b7c8d9e0f.extender.bringyour.com",
			"0192e4a1-7b3c-7d2e-9f10 5a6b7c8d9e0f.extender.bringyour.com",
			"0192e4a107b3c07d2e09f1005a6b7c8d9e0f.extender.bringyour.com",
			"0192e4a1.7b3c.7d2e.9f10.5a6b7c8d9e0f.extender.bringyour.com",
		},
		"extra or moved hyphens": {
			"0192e4a1--7b3c-7d2e-9f10-5a6b7c8d9e0f.extender.bringyour.com",
			"-0192e4a1-7b3c-7d2e-9f10-5a6b7c8d9e0f.extender.bringyour.com",
			"0192e4a1-7b3c-7d2e-9f10-5a6b7c8d9e0f-.extender.bringyour.com",
			"0192e4a1-7b3c-7d2e-9f10-5a6b-7c8d9e0f.extender.bringyour.com",
			// still 36 characters
			"0192e4a-17b3c-7d2e-9f10-5a6b7c8d9e0f.extender.bringyour.com",
			"0192e4a1-7b3c7-d2e-9f10-5a6b7c8d9e0f.extender.bringyour.com",
			"0192-e4a1-7b3c-7d2e-9f10-5a6b7c8d9e0.extender.bringyour.com",
		},
		"wrong length": {
			"0192e4a1-7b3c-7d2e-9f10-5a6b7c8d9e0.extender.bringyour.com",
			"0192e4a1-7b3c-7d2e-9f10-5a6b7c8d9e0f0.extender.bringyour.com",
			"192e4a1-7b3c-7d2e-9f10-5a6b7c8d9e0f.extender.bringyour.com",
			"0192e4a1-7b3c-7d2e-9f10.extender.bringyour.com",
			"0192e4a1.extender.bringyour.com",
			"0192e4a17b3c7d2e9f105a6b7c8d9e.extender.bringyour.com",
			"0192e4a17b3c7d2e9f105a6b7c8d9e0f00.extender.bringyour.com",
		},
		"non-hex": {
			"0192e4a1-7b3c-7d2e-9f10-5a6b7c8d9e0g.extender.bringyour.com",
			"g192e4a1-7b3c-7d2e-9f10-5a6b7c8d9e0f.extender.bringyour.com",
			"0x92e4a1-7b3c-7d2e-9f10-5a6b7c8d9e0f.extender.bringyour.com",
			"0192e4a1-7b3c-7d2e-9f10-5a6b7c8d9e+f.extender.bringyour.com",
			// 36 bytes, the last two an arabic-indic digit
			"0192e4a1-7b3c-7d2e-9f10-5a6b7c8d9e٠.extender.bringyour.com",
		},
		"wrong zone": {
			"0192e4a1-7b3c-7d2e-9f10-5a6b7c8d9e0f.extender.bringyour.co",
			"0192e4a1-7b3c-7d2e-9f10-5a6b7c8d9e0f.bringyour.com",
			"0192e4a1-7b3c-7d2e-9f10-5a6b7c8d9e0f.xextender.bringyour.com",
			"0192e4a1-7b3c-7d2e-9f10-5a6b7c8d9e0fextender.bringyour.com",
			"0192e4a1-7b3c-7d2e-9f10-5a6b7c8d9e0f.extender.bringyour.com.example",
			"0192e4a1-7b3c-7d2e-9f10-5a6b7c8d9e0f.measure.example",
			"0192e4a1-7b3c-7d2e-9f10-5a6b7c8d9e0f",
		},
		"trailing dot": {
			"0192e4a1-7b3c-7d2e-9f10-5a6b7c8d9e0f.extender.bringyour.com.",
			"0192e4a1-7b3c-7d2e-9f10-5a6b7c8d9e0f..extender.bringyour.com",
		},
		"extra labels": {
			"www.0192e4a1-7b3c-7d2e-9f10-5a6b7c8d9e0f.extender.bringyour.com",
			// the dns-01 record of the name is not the name (C1, C8)
			"_acme-challenge.0192e4a1-7b3c-7d2e-9f10-5a6b7c8d9e0f.extender.bringyour.com",
			"0192e4a1-7b3c-7d2e-9f10-5a6b7c8d9e0f.0192e4a1-7b3c-7d2e-9f10-5a6b7c8d9e0f.extender.bringyour.com",
		},
		"empty, the apex or not an id": {
			"",
			".extender.bringyour.com",
			// the apex is the geo dns name (C1)
			"extender.bringyour.com",
			"localhost.extender.bringyour.com",
			"203-0-113-7.extender.bringyour.com",
			" 0192e4a1-7b3c-7d2e-9f10-5a6b7c8d9e0f.extender.bringyour.com",
			"0192e4a1-7b3c-7d2e-9f10-5a6b7c8d9e0f .extender.bringyour.com",
		},
	}
	for kind, kindNames := range names {
		for _, name := range kindNames {
			if extenderId, ok := ParseExtenderMeasurementName(name, ExtenderMeasurementZone); ok {
				t.Errorf("%s: %q parsed as %v", kind, name, extenderId)
			}
		}
	}
}

// Ids name and parse back, and each one's other spellings fail.
func TestExtenderMeasurementNameRoundTrip(t *testing.T) {
	random := rand.New(rand.NewPCG(0x1a9, 0xde5))
	extenderIds := []Id{
		testExtenderMeasurementId,
		// fixed width: leading and trailing zeros stay
		{15: 1},
		{0: 1},
		{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff},
		NewId(),
		NewId(),
	}
	for i := 0; i < 5000; i++ {
		var extenderId Id
		for j := range extenderId {
			extenderId[j] = byte(random.IntN(256))
		}
		extenderIds = append(extenderIds, extenderId)
	}
	zones := []string{ExtenderMeasurementZone, "measure.example"}
	for i, extenderId := range extenderIds {
		zone := zones[i%len(zones)]

		name := ExtenderMeasurementName(extenderId, zone)
		label, ok := strings.CutSuffix(name, "."+zone)
		if !ok || !extenderMeasurementLabelPattern.MatchString(label) {
			t.Fatalf("name of %x = %q is not a canonical id in %s", extenderId[:], name, zone)
		}
		parsed, ok := ParseExtenderMeasurementName(name, zone)
		if !ok || parsed != extenderId {
			t.Fatalf("parse of %q = %v %v, want %v", name, parsed, ok, extenderId)
		}

		others := []string{
			strings.ToUpper(label) + "." + zone,
			label + "." + strings.ToUpper(zone),
			strings.ReplaceAll(label, "-", "") + "." + zone,
			name + ".",
			"x." + name,
			label[1:] + "." + zone,
			label + "0." + zone,
			label[:35] + "g." + zone,
		}
		for _, hyphen := range []int{8, 13, 18, 23} {
			others = append(
				others,
				label[:hyphen]+"_"+label[hyphen+1:]+"."+zone,
				label[:hyphen]+label[hyphen+1:]+"."+zone,
				label[:hyphen]+"-"+label[hyphen:]+"."+zone,
				// the hyphen one place early
				label[:hyphen-1]+"-"+label[hyphen-1:hyphen]+label[hyphen+1:]+"."+zone,
			)
		}
		for _, other := range others {
			if other == name {
				continue
			}
			if otherId, ok := ParseExtenderMeasurementName(other, zone); ok {
				t.Fatalf("%q, another spelling of %q, parsed as %v", other, name, otherId)
			}
		}
	}
}

// The family-pinned names of the N1 example (lag.dev/DRIFTTEST.md K6): the
// id, the family's label and the zone.
const (
	testExtenderMeasurementIpv4Name = "0192e4a1-7b3c-7d2e-9f10-5a6b7c8d9e0f.ipv4.extender.bringyour.com"
	testExtenderMeasurementIpv6Name = "0192e4a1-7b3c-7d2e-9f10-5a6b7c8d9e0f.ipv6.extender.bringyour.com"
)

func TestExtenderMeasurementFamilyNamePlanExample(t *testing.T) {
	for ipFamily, want := range map[int]string{4: testExtenderMeasurementIpv4Name, 6: testExtenderMeasurementIpv6Name} {
		if name := ExtenderMeasurementFamilyName(testExtenderMeasurementId, ExtenderMeasurementZone, ipFamily); name != want {
			t.Fatalf("family %d name = %q, want %q", ipFamily, name, want)
		}
	}
	// the certificate's names: the measurement name, then each family's
	names := ExtenderMeasurementNames(testExtenderMeasurementId, ExtenderMeasurementZone)
	want := []string{testExtenderMeasurementName, testExtenderMeasurementIpv4Name, testExtenderMeasurementIpv6Name}
	if !slices.Equal(names, want) {
		t.Fatalf("names = %q, want %q", names, want)
	}
	for i, ipFamily := range []int{0, 4, 6} {
		extenderId, parsedFamily, ok := ParseExtenderMeasurementFamilyName(want[i], ExtenderMeasurementZone)
		if !ok || extenderId != testExtenderMeasurementId || parsedFamily != ipFamily {
			t.Fatalf("parse of %q = %v %d %v, want %v %d", want[i], extenderId, parsedFamily, ok, testExtenderMeasurementId, ipFamily)
		}
	}
	// the measurement name's own parse is unchanged: a family's name is not it
	for _, name := range want[1:] {
		if extenderId, ok := ParseExtenderMeasurementName(name, ExtenderMeasurementZone); ok {
			t.Fatalf("%q parsed as a measurement name of %v", name, extenderId)
		}
	}
}

// The zone is normalized as for the measurement name, and a name of one zone
// does not parse in another.
func TestExtenderMeasurementFamilyNameZone(t *testing.T) {
	for _, zone := range []string{"", ExtenderMeasurementZone, "Extender.BringYour.COM", "extender.bringyour.com.", " extender.bringyour.com "} {
		for ipFamily, want := range map[int]string{4: testExtenderMeasurementIpv4Name, 6: testExtenderMeasurementIpv6Name} {
			if name := ExtenderMeasurementFamilyName(testExtenderMeasurementId, zone, ipFamily); name != want {
				t.Errorf("family %d name in zone %q = %q", ipFamily, zone, name)
			}
			if extenderId, parsedFamily, ok := ParseExtenderMeasurementFamilyName(want, zone); !ok || extenderId != testExtenderMeasurementId || parsedFamily != ipFamily {
				t.Errorf("parse of %q in zone %q = %v %d %v", want, zone, extenderId, parsedFamily, ok)
			}
		}
	}

	// one trailing dot is the root, a second is not, as for the measurement
	// name; the zone is normalized once, so the parse takes the same zone
	for _, ipFamily := range []int{4, 6} {
		zone := "extender.bringyour.com.."
		name := ExtenderMeasurementFamilyName(testExtenderMeasurementId, zone, ipFamily)
		if name != ExtenderMeasurementFamilyName(testExtenderMeasurementId, "", ipFamily)+"." {
			t.Fatalf("family %d name in a zone with two trailing dots = %q", ipFamily, name)
		}
		if extenderId, parsedFamily, ok := ParseExtenderMeasurementFamilyName(name, zone); !ok || extenderId != testExtenderMeasurementId || parsedFamily != ipFamily {
			t.Fatalf("parse of %q in its own zone = %v %d %v", name, extenderId, parsedFamily, ok)
		}
		if _, _, ok := ParseExtenderMeasurementFamilyName(name, ""); ok {
			t.Fatalf("%q parsed in the default zone", name)
		}
	}

	name := ExtenderMeasurementFamilyName(testExtenderMeasurementId, "Measure.Example.", 6)
	if name != "0192e4a1-7b3c-7d2e-9f10-5a6b7c8d9e0f.ipv6.measure.example" {
		t.Fatalf("name in another zone = %q", name)
	}
	if extenderId, ipFamily, ok := ParseExtenderMeasurementFamilyName(name, "measure.example"); !ok || extenderId != testExtenderMeasurementId || ipFamily != 6 {
		t.Fatalf("parse in another zone = %v %d %v", extenderId, ipFamily, ok)
	}
	if _, _, ok := ParseExtenderMeasurementFamilyName(name, ExtenderMeasurementZone); ok {
		t.Fatal("a name in another zone parsed in the default zone")
	}
	if _, _, ok := ParseExtenderMeasurementFamilyName(testExtenderMeasurementIpv6Name, "measure.example"); ok {
		t.Fatal("a name in the default zone parsed in another zone")
	}
}

// The zero Id has no names, and no family but 4 and 6 has one.
func TestExtenderMeasurementFamilyNameUnset(t *testing.T) {
	for _, ipFamily := range []int{4, 6} {
		if name := ExtenderMeasurementFamilyName(Id{}, ExtenderMeasurementZone, ipFamily); name != "" {
			t.Fatalf("family %d name of the zero Id = %q", ipFamily, name)
		}
	}
	if names := ExtenderMeasurementNames(Id{}, ExtenderMeasurementZone); names != nil {
		t.Fatalf("names of the zero Id = %q", names)
	}
	for _, ipFamily := range []int{0, 1, 5, 46, 64, -4, 41} {
		if name := ExtenderMeasurementFamilyName(testExtenderMeasurementId, ExtenderMeasurementZone, ipFamily); name != "" {
			t.Fatalf("family %d has the name %q", ipFamily, name)
		}
	}
	for _, label := range []string{"ipv4", "ipv6"} {
		name := Id{}.String() + "." + label + "." + ExtenderMeasurementZone
		if extenderId, ipFamily, ok := ParseExtenderMeasurementFamilyName(name, ExtenderMeasurementZone); ok {
			t.Fatalf("the zero Id's spelling %q parsed as %v %d", name, extenderId, ipFamily)
		}
	}
}

// Every spelling of a family-pinned name but the canonical one is refused,
// and so is every other name under the zone.
func TestParseExtenderMeasurementFamilyNameRefuses(t *testing.T) {
	const id = "0192e4a1-7b3c-7d2e-9f10-5a6b7c8d9e0f"
	for _, name := range []string{
		// the family label in another case or spelling
		id + ".IPV4.extender.bringyour.com",
		id + ".Ipv6.extender.bringyour.com",
		id + ".ipv5.extender.bringyour.com",
		id + ".ip4.extender.bringyour.com",
		id + ".v6.extender.bringyour.com",
		id + ".ipv46.extender.bringyour.com",
		id + ".ipv4.ipv6.extender.bringyour.com",
		id + ".ipv6.ipv4.extender.bringyour.com",
		id + ".ipv4.ipv4.extender.bringyour.com",
		// the label joined to the id or the zone, or moved
		id + "ipv4.extender.bringyour.com",
		id + "-ipv4.extender.bringyour.com",
		id + ".ipv4extender.bringyour.com",
		id + ".ipv4..extender.bringyour.com",
		"ipv4." + id + ".extender.bringyour.com",
		// a trailing dot, extra labels, the dns-01 record of the name (C1, C5)
		id + ".ipv4.extender.bringyour.com.",
		"x." + id + ".ipv4.extender.bringyour.com",
		"_acme-challenge." + id + ".ipv4.extender.bringyour.com",
		"_acme-challenge." + id + ".ipv6.extender.bringyour.com",
		// an id that is not canonical
		strings.ToUpper(id) + ".ipv4.extender.bringyour.com",
		strings.ReplaceAll(id, "-", "") + ".ipv6.extender.bringyour.com",
		id[1:] + ".ipv4.extender.bringyour.com",
		id + "0.ipv6.extender.bringyour.com",
		"00000000-0000-0000-0000-000000000000.ipv4.extender.bringyour.com",
		// the labels between, the apex, and another zone
		"ipv4.extender.bringyour.com",
		"ipv6.extender.bringyour.com",
		".ipv4.extender.bringyour.com",
		"extender.bringyour.com",
		id + ".ipv4.bringyour.com",
		id + ".ipv4.measure.example",
		"",
	} {
		if extenderId, ipFamily, ok := ParseExtenderMeasurementFamilyName(name, ExtenderMeasurementZone); ok {
			t.Errorf("%q parsed as %v %d", name, extenderId, ipFamily)
		}
		if extenderId, ok := ParseExtenderMeasurementName(name, ExtenderMeasurementZone); ok {
			t.Errorf("%q parsed as a measurement name of %v", name, extenderId)
		}
	}
}

// A certificate's or a request's names are an extender's measurement name
// alone, or all three of its names in any order, and nothing else.
func TestParseExtenderMeasurementNames(t *testing.T) {
	zone := ExtenderMeasurementZone
	names := ExtenderMeasurementNames(testExtenderMeasurementId, zone)
	plain, ipv4, ipv6 := names[0], names[1], names[2]
	otherId := Id{0x01, 0x92, 0xe4, 0xa1, 0x7b, 0x3c, 0x7d, 0x2e, 0x9f, 0x10, 0x5a, 0x6b, 0x7c, 0x8d, 0x9e, 0x10}
	other := ExtenderMeasurementNames(otherId, zone)

	// the measurement name alone, as an extender from before K6 asks for it
	if extenderId, familyNames, ok := ParseExtenderMeasurementNames([]string{plain}, zone); !ok || extenderId != testExtenderMeasurementId || familyNames {
		t.Fatalf("the measurement name alone = %v %v %v", extenderId, familyNames, ok)
	}
	// all three in every order
	for _, order := range [][]string{
		{plain, ipv4, ipv6},
		{plain, ipv6, ipv4},
		{ipv4, plain, ipv6},
		{ipv4, ipv6, plain},
		{ipv6, plain, ipv4},
		{ipv6, ipv4, plain},
	} {
		if extenderId, familyNames, ok := ParseExtenderMeasurementNames(order, zone); !ok || extenderId != testExtenderMeasurementId || !familyNames {
			t.Fatalf("%q = %v %v %v", order, extenderId, familyNames, ok)
		}
	}
	// another extender's sets are its own
	if extenderId, familyNames, ok := ParseExtenderMeasurementNames(other, zone); !ok || extenderId != otherId || !familyNames {
		t.Fatalf("another extender's names = %v %v %v", extenderId, familyNames, ok)
	}

	for kind, sets := range map[string][][]string{
		"no names":                   {nil, {}},
		"a family's name alone":      {{ipv4}, {ipv6}, {ipv4, ipv6}},
		"one family's name missing":  {{plain, ipv4}, {plain, ipv6}, {ipv6, plain}},
		"a name twice":               {{plain, plain}, {plain, ipv4, ipv4}, {plain, plain, ipv4}, {plain, plain, plain}, {plain, ipv6, ipv6}},
		"a fourth name":              {{plain, ipv4, ipv6, plain}, {plain, ipv4, ipv6, other[0]}, {plain, ipv4, ipv6, "x." + plain}},
		"another extender's name":    {{plain, ipv4, other[2]}, {other[0], ipv4, ipv6}, {plain, other[1], ipv6}, {plain, other[0]}},
		"another zone":               {ExtenderMeasurementNames(testExtenderMeasurementId, "measure.example"), {ExtenderMeasurementName(testExtenderMeasurementId, "measure.example")}, {plain, ExtenderMeasurementFamilyName(testExtenderMeasurementId, "measure.example", 4), ipv6}},
		"not a measurement name":     {{plain + "."}, {strings.ToUpper(plain)}, {"_acme-challenge." + plain}, {"", ipv4, ipv6}, {plain, "_acme-challenge." + ipv4, ipv6}, {zone}},
		"a family label in its case": {{plain, strings.Replace(ipv4, ".ipv4.", ".IPV4.", 1), ipv6}},
	} {
		for _, set := range sets {
			if extenderId, familyNames, ok := ParseExtenderMeasurementNames(set, zone); ok {
				t.Errorf("%s: %q parsed as %v %v", kind, set, extenderId, familyNames)
			}
		}
	}
}

// Every id's three names parse back to it and their family, in any order as
// a set, and a family's name never parses as another family's or as the
// measurement name.
func TestExtenderMeasurementFamilyNameRoundTrip(t *testing.T) {
	random := rand.New(rand.NewPCG(0x4b6, 0x6b4))
	zones := []string{ExtenderMeasurementZone, "measure.example", "ipv4.example"}
	for i := 0; i < 3000; i++ {
		var extenderId Id
		for j := range extenderId {
			extenderId[j] = byte(random.IntN(256))
		}
		if extenderId == (Id{}) {
			continue
		}
		zone := zones[i%len(zones)]

		names := ExtenderMeasurementNames(extenderId, zone)
		for j, ipFamily := range []int{0, 4, 6} {
			parsedId, parsedFamily, ok := ParseExtenderMeasurementFamilyName(names[j], zone)
			if !ok || parsedId != extenderId || parsedFamily != ipFamily {
				t.Fatalf("parse of %q = %v %d %v, want %v %d", names[j], parsedId, parsedFamily, ok, extenderId, ipFamily)
			}
			_, plain := ParseExtenderMeasurementName(names[j], zone)
			if plain != (ipFamily == 0) {
				t.Fatalf("%q parsed as a measurement name: %v", names[j], plain)
			}
		}
		random.Shuffle(len(names), func(a int, b int) {
			names[a], names[b] = names[b], names[a]
		})
		if parsedId, familyNames, ok := ParseExtenderMeasurementNames(names, zone); !ok || parsedId != extenderId || !familyNames {
			t.Fatalf("names %q = %v %v %v", names, parsedId, familyNames, ok)
		}
	}
}
