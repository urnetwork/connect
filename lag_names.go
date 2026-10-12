package connect

import (
	"strings"
)

// Measurement names of extenders (lag.dev/PLAN.md N). An open tier extender
// that serves the measurement API (M) is reached by browsers under one name,
// in a zone of its own, and its measurement certificate names exactly that
// (N3, C4).
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
// The TypeScript twin is extenderMeasurementHost in sdk/js (S1, S2), pinned
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
// with the rest.
func ParseExtenderMeasurementName(name string, zone string) (Id, bool) {
	label, ok := strings.CutSuffix(name, "."+extenderMeasurementZone(zone))
	if !ok {
		return Id{}, false
	}
	extenderId, err := ParseId(label)
	if err != nil || extenderId == (Id{}) || extenderId.String() != label {
		return Id{}, false
	}
	return extenderId, true
}
