package connect

import (
	"net/netip"
	"strings"
)

// Measurement names of extenders (lag.dev/PLAN.md N). An open tier extender
// that serves the measurement API (M) is reached by browsers under one name
// per address family, in a zone of its own, and its measurement certificate
// names exactly those (N3, C4).
//
// The label is the canonical text of the address with '.' and ':' replaced by
// '-', and a label that would start or end with '-' gets a '0' there (N1).
// The canonical text is netip.Addr.Unmap().String(): lower case, the shortest
// ipv6 form (RFC 5952), and a v4-mapped address as plain ipv4. The mapping is
// one to one, so a name parses back to exactly one address: an ipv4 label has
// three single dashes, an ipv6 label has a "--" or seven dashes, and since a
// canonical ipv6 text never starts with "0::" or ends with "::0" (that zero
// group would have joined the compressed run) the '0' pad is unambiguous.
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

// The measurement label of an address (N1), or "" for the zero Addr. An ipv6
// scope zone is dropped: it is local to one host and never part of a name.
func ExtenderMeasurementLabel(addr netip.Addr) string {
	if !addr.IsValid() {
		return ""
	}
	label := []byte(addr.WithZone("").Unmap().String())
	for i, c := range label {
		if c == '.' || c == ':' {
			label[i] = '-'
		}
	}
	if label[0] == '-' {
		label = append([]byte{'0'}, label...)
	}
	if label[len(label)-1] == '-' {
		label = append(label, '0')
	}
	return string(label)
}

// The measurement name of an address (N1): its label, '.', and the zone.
// The zone is written as extenderMeasurementZone normalizes it, so "" means
// ExtenderMeasurementZone. The zero Addr has no name, "".
func ExtenderMeasurementName(addr netip.Addr, zone string) string {
	label := ExtenderMeasurementLabel(addr)
	if label == "" {
		return ""
	}
	return label + "." + extenderMeasurementZone(zone)
}

// The address a measurement name is for, the inverse of
// ExtenderMeasurementName (N2). Only the canonical spelling parses: upper
// case, an uncompressed or otherwise compressed ipv6, leading zeros, a missing
// '0' pad, a v4-mapped or scoped spelling, a trailing dot, another zone and an
// extra label are all refused. The address is returned canonical, plain ipv4
// or unscoped ipv6.
func ParseExtenderMeasurementName(name string, zone string) (netip.Addr, bool) {
	label, ok := strings.CutSuffix(name, "."+extenderMeasurementZone(zone))
	if !ok {
		return netip.Addr{}, false
	}
	// The '0' pad needs no undoing, since "0::1" and "2001:db8::0" are valid
	// ipv6 text. Any spelling but the canonical one has another canonical
	// label, or is no address at all, and the comparison refuses it.
	for _, text := range []string{strings.ReplaceAll(label, "-", "."), strings.ReplaceAll(label, "-", ":")} {
		addr, err := netip.ParseAddr(text)
		if err == nil && ExtenderMeasurementLabel(addr) == label {
			return addr, true
		}
	}
	return netip.Addr{}, false
}
