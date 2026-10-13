package connect

import (
	"bytes"
	"math/rand/v2"
	"regexp"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/urnetwork/connect/protocol"
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
