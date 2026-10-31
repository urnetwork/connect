package connect

import (
	"bytes"
)

// location_match.go — the find-providers2 location fallback as the window sees
// it. When the city or region a client asks for holds no admitted provider,
// the platform answers from the target's region, then its country (never
// another country), and marks each provider found there with
// `FindProvidersProvider.Match`. Discovery normalizes the marker into
// `DestinationStats.LocationMatch`, every provider event of a window client
// carries it (`ProviderEvent.LocationMatch`), and the window monitor folds the
// Added providers into `WindowExpandEvent.LocationMatch`, which is what an app
// reads to say "no providers in <city>; showing <region>".
//
// The empty `LocationMatch` is exact: a provider of the requested target
// itself, a provider named by client id, a fixed or restored destination, a
// strict request, and every provider of a server that predates the marker. An
// older server therefore never produces a fallback notice.

// LocationMatch is the level find-providers2 found a provider at.
type LocationMatch string

const (
	// LocationMatchExact is the zero value: the provider is in the requested
	// location, or no level was reported.
	LocationMatchExact LocationMatch = ""
	// LocationMatchRegion means the provider was found in the requested
	// city's region because the city had no admitted provider.
	LocationMatchRegion LocationMatch = "region"
	// LocationMatchCountry means the provider was found in the requested
	// city's or region's country because the region had none either.
	LocationMatchCountry LocationMatch = "country"
)

// Normalize maps a level this build does not know to exact, so a level a newer
// server introduces degrades to today's behavior (no notice) instead of to a
// notice this client cannot name.
func (self LocationMatch) Normalize() LocationMatch {
	switch self {
	case LocationMatchRegion, LocationMatchCountry:
		return self
	default:
		return LocationMatchExact
	}
}

// breadth orders the levels outward from the requested location.
func (self LocationMatch) breadth() int {
	switch self.Normalize() {
	case LocationMatchRegion:
		return 1
	case LocationMatchCountry:
		return 2
	default:
		return 0
	}
}

// WindowLocationMatch is the window-level answer to "do the providers in use
// match the location that was asked for?", derived from the Added providers'
// `ProviderEvent.LocationMatch`. A provider in evaluation carries no traffic
// and is not counted. The zero value is exact: every Added provider is in the
// requested location, or none is Added. It is comparable, so a
// `WindowExpandEvent` stays comparable.
type WindowLocationMatch struct {
	// Match is the broadest level among the Added providers. A window routes
	// a new flow to any of its Added providers, so while one of them is
	// outside the requested location the user's traffic can leave from there,
	// and the notice must not be hidden behind the providers that match.
	Match LocationMatch
	// Mixed is true while some Added providers are in the requested location
	// itself and others are not, so "no providers in <city>" would be untrue.
	// It is a transition: the location's own providers came back and the
	// fallback ones have not rotated out of the window yet. False when Match
	// is exact.
	Mixed bool
	// Name is the name of the location shown in place of the requested one:
	// the providers' region for `LocationMatchRegion`, their country for
	// `LocationMatchCountry`. Empty when Match is exact, and when the server
	// sent no location for the provider.
	Name string
	// LocationId is the id of that region or country. Zero when Match is
	// exact or the server did not send it.
	LocationId Id
}

// locationMatchFold folds Added provider events into a `WindowLocationMatch`.
// Among the Added providers at the broadest level, the one that names its
// location, then the longest connected, then the lowest egress client id (then
// window client id) names the shown location, a total order, so the result
// does not depend on map order. With one location target every provider at a
// level shares one region or country; the order matters only when a window
// discovers for several targets.
//
// The fold keeps the naming event rather than its name, so two folds merge by
// the same order: the multi client's merged monitor folds its windows' folds
// without copying their provider events. It reads only fields an event
// rewrite (`SetProviderIpFamily`, `SetProviderExtenderIps`) never changes.
type locationMatchFold struct {
	broadest LocationMatch
	shown    *ProviderEvent
	exact    bool
}

func (self *locationMatchFold) add(providerEvent *ProviderEvent) {
	if providerEvent == nil || !providerEvent.State.IsActive() {
		return
	}
	match := providerEvent.LocationMatch.Normalize()
	switch {
	case match == LocationMatchExact:
		self.exact = true
	case self.broadest.breadth() < match.breadth():
		self.broadest = match
		self.shown = providerEvent
	case match == self.broadest && locationMatchShownBefore(providerEvent, self.shown, match):
		self.shown = providerEvent
	}
}

func (self *locationMatchFold) merge(other locationMatchFold) {
	self.exact = self.exact || other.exact
	switch {
	case other.broadest == LocationMatchExact:
	case self.broadest.breadth() < other.broadest.breadth():
		self.broadest = other.broadest
		self.shown = other.shown
	case other.broadest == self.broadest && locationMatchShownBefore(other.shown, self.shown, other.broadest):
		self.shown = other.shown
	}
}

func (self *locationMatchFold) windowLocationMatch() WindowLocationMatch {
	if self.broadest == LocationMatchExact {
		return WindowLocationMatch{}
	}
	name, locationId := locationMatchName(self.shown.Location, self.broadest)
	return WindowLocationMatch{
		Match:      self.broadest,
		Mixed:      self.exact,
		Name:       name,
		LocationId: locationId,
	}
}

// locationMatchName is the name and id of the location a provider at match
// shows: its region for region, its country for country.
func locationMatchName(location *ProviderLocation, match LocationMatch) (string, Id) {
	if location == nil {
		return "", Id{}
	}
	switch match {
	case LocationMatchRegion:
		if location.RegionLocationId != nil {
			return location.Region, *location.RegionLocationId
		}
		return location.Region, Id{}
	case LocationMatchCountry:
		if location.CountryLocationId != nil {
			return location.Country, *location.CountryLocationId
		}
		return location.Country, Id{}
	default:
		return "", Id{}
	}
}

// locationMatchShownBefore reports whether a names the shown location ahead
// of b, two Added providers at match (see locationMatchFold).
func locationMatchShownBefore(a *ProviderEvent, b *ProviderEvent, match LocationMatch) bool {
	aName, _ := locationMatchName(a.Location, match)
	bName, _ := locationMatchName(b.Location, match)
	if (aName != "") != (bName != "") {
		return aName != ""
	}
	if !a.EventTime.Equal(b.EventTime) {
		if a.EventTime.IsZero() || b.EventTime.IsZero() {
			// an unknown connected-since sorts last
			return b.EventTime.IsZero()
		}
		return a.EventTime.Before(b.EventTime)
	}
	if c := bytes.Compare(a.EgressClientId[:], b.EgressClientId[:]); c != 0 {
		return c < 0
	}
	return bytes.Compare(a.ClientId[:], b.ClientId[:]) < 0
}
