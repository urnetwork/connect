package connect

import (
	"strings"
)

// Measurement names of extenders (lag.dev/PLAN.md N). An open tier extender
// that serves the measurement API (M) is reached by browsers under its
// measurement name, in a zone of its own (N3, C4).
//
// The name is the extender's id, '.', and the zone (N1). The id is the
// operator's id of the extender, one per identity key, written as Id.String
// writes it: lower case hex grouped 8-4-4-4-12. An id says nothing of where
// the extender is, so the certificate logs list ids rather than addresses,
// and the name outlives every address change: its A and AAAA records are the
// extender's current addresses of each family. An extender learns its id
// from its activation answer, and a reader of the feed or gossip from the
// record's MeasurementId (N4, C10).
//
// Beside it, an extender has a name for each IP family that resolves in that
// family alone (lag.dev/DRIFTTEST.md K6): `<id>.ipv4.<zone>` with only its A
// records and `<id>.ipv6.<zone>` with only its AAAA records. A browser that
// connects to the measurement name picks the family itself, and a drift test
// run is bound to the customer's client_ip only when its sessions use
// client_ip's family, so a drift ticket names the family's own name. The
// measurement certificate carries all three names. An extender from before
// K6 asks for its measurement name alone, and has no family-pinned names.
//
// The TypeScript twins are extenderMeasurementHost and
// extenderMeasurementFamilyHost in sdk/js (S1, S2), with their parses, pinned
// by vectors generated from these functions.

// The zone measurement names are under unless configured otherwise (N1, C1).
// The zone is a setting (measurement.zone_name); this is its default.
const ExtenderMeasurementZone = "extender.bringyour.com"

// The zone as names are written, without case, surrounding space or a
// trailing dot, as ExtenderNetworkHostAllowed compares hosts. Route 53 spells
// zones with the trailing dot, and a name for a certificate must not carry
// one. Empty means ExtenderMeasurementZone, the default the TypeScript twin
// takes.
func extenderMeasurementZone(zone string) string {
	zone = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(zone), "."))
	if zone == "" {
		return ExtenderMeasurementZone
	}
	return zone
}

// The measurement name of an extender (N1): its id, '.', and the zone. The
// zone is written as extenderMeasurementZone normalizes it, so "" means
// ExtenderMeasurementZone. The zero Id is no extender's and has no name, "".
func ExtenderMeasurementName(extenderId Id, zone string) string {
	if extenderId == (Id{}) {
		return ""
	}
	return extenderId.String() + "." + extenderMeasurementZone(zone)
}

// The id of the extender a measurement name is for, the inverse of
// ExtenderMeasurementName (N2). Only the canonical spelling parses. ParseId
// also reads upper case, an id without its hyphens and any character in a
// hyphen's place, so the id must write back as exactly the label given.
// Another zone, an extra label, a trailing dot and the zero Id are refused
// with the rest, and so is a family-pinned name, which
// ParseExtenderMeasurementFamilyName reads.
func ParseExtenderMeasurementName(name string, zone string) (Id, bool) {
	return parseExtenderMeasurementId(name, "."+extenderMeasurementZone(zone))
}

// The id that `name` spells before `suffix`, in its canonical spelling only
// (ParseExtenderMeasurementName).
func parseExtenderMeasurementId(name string, suffix string) (Id, bool) {
	label, ok := strings.CutSuffix(name, suffix)
	if !ok {
		return Id{}, false
	}
	extenderId, err := ParseId(label)
	if err != nil || extenderId == (Id{}) || extenderId.String() != label {
		return Id{}, false
	}
	return extenderId, true
}

// The label between the id and the zone of the name that resolves in one IP
// family alone (K6), "" for anything but 4 and 6.
func extenderMeasurementFamilyLabel(ipFamily int) string {
	switch ipFamily {
	case 4:
		return "ipv4"
	case 6:
		return "ipv6"
	default:
		return ""
	}
}

// The name of an extender that resolves in IP family `ipFamily` alone, 4 or 6
// (lag.dev/DRIFTTEST.md K6): its id, '.', "ipv4" or "ipv6", '.', and the
// zone, which is written as ExtenderMeasurementName writes it. Its records are
// those of the measurement name in that family only. The zero Id is no
// extender's, and it and any other family have no name, "".
func ExtenderMeasurementFamilyName(extenderId Id, zone string, ipFamily int) string {
	label := extenderMeasurementFamilyLabel(ipFamily)
	if extenderId == (Id{}) || label == "" {
		return ""
	}
	return extenderId.String() + "." + label + "." + extenderMeasurementZone(zone)
}

// The names an extender's measurement certificate carries (C4, K6): its
// measurement name, then its IPv4 name and its IPv6 name. Nil for the zero Id.
func ExtenderMeasurementNames(extenderId Id, zone string) []string {
	if extenderId == (Id{}) {
		return nil
	}
	return []string{
		ExtenderMeasurementName(extenderId, zone),
		ExtenderMeasurementFamilyName(extenderId, zone, 4),
		ExtenderMeasurementFamilyName(extenderId, zone, 6),
	}
}

// The id of the extender any of its measurement names is for, and the IP
// family the name resolves in: 0 for its measurement name, which resolves in
// both, and 4 or 6 for a name of one family (K6). Exactly those three shapes
// parse, each in the one canonical spelling ParseExtenderMeasurementName
// takes, so another label between the id and the zone, a family label in
// upper case and a name at the zone's apex are refused with the rest.
func ParseExtenderMeasurementFamilyName(name string, zone string) (extenderId Id, ipFamily int, ok bool) {
	zone = extenderMeasurementZone(zone)
	if extenderId, ok := parseExtenderMeasurementId(name, "."+zone); ok {
		return extenderId, 0, true
	}
	for _, ipFamily := range []int{4, 6} {
		if extenderId, ok := parseExtenderMeasurementId(name, "."+extenderMeasurementFamilyLabel(ipFamily)+"."+zone); ok {
			return extenderId, ipFamily, true
		}
	}
	return Id{}, 0, false
}

// The extender a measurement certificate or its request is for, read from its
// names (C4, K6), and whether they include the family-pinned names. Two sets
// are an extender's, in any order: its measurement name alone, which an
// extender from before K6 asks for, and its measurement name with both of its
// family-pinned names. Every other set is refused: another extender's name
// beside its own, a name twice, a family-pinned name without the measurement
// name or without the other family's, a name of another shape or zone.
func ParseExtenderMeasurementNames(names []string, zone string) (extenderId Id, familyNames bool, ok bool) {
	if len(names) != 1 && len(names) != 3 {
		return Id{}, false, false
	}
	// each family's name at most once, all of one extender
	ipFamilies := map[int]bool{}
	for i, name := range names {
		nameId, ipFamily, ok := ParseExtenderMeasurementFamilyName(name, zone)
		if !ok || ipFamilies[ipFamily] || (0 < i && nameId != extenderId) {
			return Id{}, false, false
		}
		extenderId = nameId
		ipFamilies[ipFamily] = true
	}
	// one name is the measurement name, and three are one of each family
	if !ipFamilies[0] {
		return Id{}, false, false
	}
	return extenderId, len(names) == 3, true
}
