package connect

// net_extender_camouflage_clock.go — the clock a camouflaged hello seals its
// time with (EXTENDER.md P1, P8; EXTENDER-UPDATE2 §3.2).
//
// The extender accepts a sealed time within its time window (two minutes by
// default) of its own clock. A client whose clock is further off is not
// authenticated: in Phase A its race falls to the legacy attempt, and in Phase B
// the legacy attempt is spliced away too, so it loses the tcp carrier. The client
// therefore seals with its clock corrected by an offset learned from an
// authenticated source, and only from one:
//
//   - The Date of a response from the extender itself, read over a connection
//     whose outer leaf passed the B3 check against the record's identity key, on
//     the tcp carrier (legacy or camouflaged) or the quic and dns carriers. The
//     offset is kept per extender identity, so an extender can move only the
//     time a client seals for that same extender — which it checks against its
//     own clock anyway — and never the time sealed for another.
//   - The Date of a platform api response whose tls chain verified
//     (ObserveExtenderCamouflagePlatformTime), which a client strategy reads off
//     every response of the hosts its settings name as the platform's own api
//     (ClientStrategySettings.PlatformApiHosts). That offset is the fallback for
//     an extender with no observation of its own.
//
// Nothing else moves it. A failed dial, a spliced handshake, a hello the
// extender did not authenticate, a response over a config with no record key
// (no B3 check), a webrtc response read before its challenge verifies, or a
// Date that does not parse leaves the offsets as they were. An offset is used
// for nothing but the sealed time: record expiry, the B3 leaf check and every
// other identity check read the local clock and the record keys as before, so
// a wrong offset can at worst fail a camouflaged attempt.
//
// Bounds: the per-extender table keeps the most recent
// extenderCamouflageClockMaxExtenderCount identities, an offset is used for at
// most extenderCamouflageClockMaxAge after it was observed, and an offset past
// extenderCamouflageClockMaxOffset is refused. A wall clock step since an
// observation (a manual change, an ntp correction) shows as a disagreement
// between the wall and monotonic clocks and drops that offset, since it no
// longer describes the local clock.
//
// Safe for concurrent use.

import (
	"container/list"
	"crypto/ed25519"
	"encoding/hex"
	"net/http"
	"slices"
	"sync"
	"time"
)

const (
	extenderCamouflageClockMaxExtenderCount = 256
	extenderCamouflageClockMaxAge           = 6 * time.Hour
	extenderCamouflageClockMaxOffset        = 366 * 24 * time.Hour
	// how far the wall and monotonic clocks may drift apart over one
	// observation's life before it is taken for a step
	extenderCamouflageClockStepTolerance = 10 * time.Second
	// a Date names a whole second, so the server's time is on average half a
	// second past it
	extenderCamouflageClockDateResolution = time.Second
)

// The local clock the camouflage seals with and observes against; a test skews
// it to stand in for a client whose clock is wrong.
var (
	extenderCamouflageLocalClockStateLock sync.Mutex
	extenderCamouflageLocalClock          = time.Now
)

func extenderCamouflageLocalNow() time.Time {
	extenderCamouflageLocalClockStateLock.Lock()
	now := extenderCamouflageLocalClock
	extenderCamouflageLocalClockStateLock.Unlock()
	return now()
}

// One observed offset: the authenticated time less the local time, and the
// local time it was observed at, which carries the monotonic clock.
type extenderCamouflageClockObservation struct {
	keyHex     string
	offset     time.Duration
	observedAt time.Time
}

// The process's camouflage clock offsets.
type extenderCamouflageClock struct {
	stateLock sync.Mutex
	// identity key hex to its element in order, newest at the front
	extenderObservations map[string]*list.Element
	order                *list.List
	platformObservation  *extenderCamouflageClockObservation
}

func newExtenderCamouflageClock() *extenderCamouflageClock {
	return &extenderCamouflageClock{
		extenderObservations: map[string]*list.Element{},
		order:                list.New(),
	}
}

var (
	defaultExtenderCamouflageClockStateLock sync.Mutex
	defaultExtenderCamouflageClock          = newExtenderCamouflageClock()
)

func currentExtenderCamouflageClock() *extenderCamouflageClock {
	defaultExtenderCamouflageClockStateLock.Lock()
	defer defaultExtenderCamouflageClockStateLock.Unlock()
	return defaultExtenderCamouflageClock
}

// The offset of an authenticated Date read between sendTime and receiveTime,
// both local, and whether it is usable. The server stamped the Date between
// the two, so the midpoint stands in for its local time.
func extenderCamouflageDateOffset(header http.Header, sendTime time.Time, receiveTime time.Time) (time.Duration, bool) {
	if header == nil || sendTime.IsZero() || receiveTime.IsZero() || receiveTime.Before(sendTime) {
		return 0, false
	}
	date, err := http.ParseTime(header.Get("Date"))
	if err != nil {
		return 0, false
	}
	localTime := sendTime.Add(receiveTime.Sub(sendTime) / 2)
	offset := date.Add(extenderCamouflageClockDateResolution / 2).Sub(localTime.Round(0))
	if offset < -extenderCamouflageClockMaxOffset || extenderCamouflageClockMaxOffset < offset {
		return 0, false
	}
	return offset, true
}

// Records the offset an authenticated response of one extender showed.
func (self *extenderCamouflageClock) observeExtender(identityPublicKey []byte, offset time.Duration, observedAt time.Time) {
	keyHex := hex.EncodeToString(identityPublicKey)
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	observation := &extenderCamouflageClockObservation{
		keyHex:     keyHex,
		offset:     offset,
		observedAt: observedAt,
	}
	if element, ok := self.extenderObservations[keyHex]; ok {
		element.Value = observation
		self.order.MoveToFront(element)
		return
	}
	self.extenderObservations[keyHex] = self.order.PushFront(observation)
	for extenderCamouflageClockMaxExtenderCount < self.order.Len() {
		oldest := self.order.Back()
		self.order.Remove(oldest)
		delete(self.extenderObservations, oldest.Value.(*extenderCamouflageClockObservation).keyHex)
	}
}

// Records the offset an authenticated platform response showed.
func (self *extenderCamouflageClock) observePlatform(offset time.Duration, observedAt time.Time) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.platformObservation = &extenderCamouflageClockObservation{
		offset:     offset,
		observedAt: observedAt,
	}
}

// The offset to seal for one extender at now: its own current observation, else
// the platform's, else zero.
func (self *extenderCamouflageClock) offsetFor(identityPublicKey []byte, now time.Time) time.Duration {
	keyHex := hex.EncodeToString(identityPublicKey)
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if element, ok := self.extenderObservations[keyHex]; ok {
		if observation := element.Value.(*extenderCamouflageClockObservation); observation.current(now) {
			return observation.offset
		}
	}
	if self.platformObservation != nil && self.platformObservation.current(now) {
		return self.platformObservation.offset
	}
	return 0
}

// Whether an observation still describes the local clock at now: younger than
// the maximum age, and with no wall clock step since it was taken.
func (self *extenderCamouflageClockObservation) current(now time.Time) bool {
	// on the monotonic clock when both readings carry it
	elapsed := now.Sub(self.observedAt)
	if elapsed < 0 || extenderCamouflageClockMaxAge < elapsed {
		return false
	}
	wallElapsed := now.Round(0).Sub(self.observedAt.Round(0))
	step := wallElapsed - elapsed
	return -extenderCamouflageClockStepTolerance <= step && step <= extenderCamouflageClockStepTolerance
}

// A source of authenticated extender time: the identity key the outer leaf of a
// connection verified under. Nil, and a no-op, when the connection was not
// verified against a record key.
type extenderCamouflageTimeSource struct {
	identityPublicKey []byte
}

// The time source of a dial whose outer leaf is checked against the config's
// record key (B3): only a full ed25519 key, with a tls config that installs the
// leaf verifier. Nil otherwise, so an unverified address never moves the clock.
func newExtenderCamouflageTimeSource(extenderConfig *ExtenderConfig, leafVerified bool) *extenderCamouflageTimeSource {
	if extenderConfig == nil || !leafVerified || len(extenderConfig.PublicKey) != ed25519.PublicKeySize {
		return nil
	}
	return &extenderCamouflageTimeSource{identityPublicKey: append([]byte(nil), extenderConfig.PublicKey...)}
}

// Records the Date of a response read over the verified connection.
func (self *extenderCamouflageTimeSource) observe(header http.Header, sendTime time.Time, receiveTime time.Time) {
	if self == nil {
		return
	}
	if offset, ok := extenderCamouflageDateOffset(header, sendTime, receiveTime); ok {
		currentExtenderCamouflageClock().observeExtender(self.identityPublicKey, offset, receiveTime)
	}
}

// ObserveExtenderCamouflagePlatformTime records the Date of a platform api
// response as the camouflage clock's fallback offset. sendTime and receiveTime
// bracket the request on the local clock. Only a response whose tls chain
// verified counts; the result says whether it was recorded. Call it only with a
// response of the platform's own api, never of another host.
func ObserveExtenderCamouflagePlatformTime(response *http.Response, sendTime time.Time, receiveTime time.Time) bool {
	if response == nil || response.TLS == nil || len(response.TLS.VerifiedChains) == 0 {
		return false
	}
	offset, ok := extenderCamouflageDateOffset(response.Header, sendTime, receiveTime)
	if !ok {
		return false
	}
	currentExtenderCamouflageClock().observePlatform(offset, receiveTime)
	return true
}

// PlatformApiHostNames are the hosts of the given api urls, normalized and
// distinct, for ClientStrategySettings.PlatformApiHosts. A url that names no
// host, such as the family url of a space that has none, adds nothing.
func PlatformApiHostNames(apiUrls ...string) []string {
	hostNames := []string{}
	for _, apiUrl := range apiUrls {
		if hostName, err := ExtenderApiHostName(apiUrl); err == nil && !slices.Contains(hostNames, hostName) {
			hostNames = append(hostNames, hostName)
		}
	}
	return hostNames
}

// PlatformApiHosts are the hosts this strategy reads the camouflage clock from
// (ClientStrategySettings.PlatformApiHosts). The result is a copy.
func (self *ClientStrategy) PlatformApiHosts() []string {
	return slices.Clone(self.settings.PlatformApiHosts)
}

// Reads the camouflage clock's fallback offset off one attempt of this strategy
// (ObserveExtenderCamouflagePlatformTime), when the response came from a host
// the settings name as the platform's own api. The host is the one the response
// actually came from, the request after any redirect, so the api redirecting
// elsewhere moves nothing. sendTime is the local clock before the attempt.
func (self *ClientStrategy) observeExtenderCamouflagePlatformTime(response *http.Response, err error, sendTime time.Time) {
	if err != nil || response == nil || response.Request == nil || response.Request.URL == nil {
		return
	}
	hostName := NormalizeExtenderNetworkHost(response.Request.URL.Hostname())
	if hostName == "" || !slices.Contains(self.settings.PlatformApiHosts, hostName) {
		return
	}
	ObserveExtenderCamouflagePlatformTime(response, sendTime, extenderCamouflageLocalNow())
}

// The time a camouflaged hello to the extender with this identity key seals:
// the local clock corrected by the current authenticated offset.
func extenderCamouflageSealTime(identityPublicKey []byte) time.Time {
	now := extenderCamouflageLocalNow()
	return now.Add(currentExtenderCamouflageClock().offsetFor(identityPublicKey, now))
}

// Testing_SetExtenderCamouflageLocalClock replaces the local clock the
// camouflage seals with, and gives every test a fresh set of offsets; the
// returned function restores both. Tests in other packages use it to stand in
// for a client whose clock is wrong.
func Testing_SetExtenderCamouflageLocalClock(now func() time.Time) func() {
	extenderCamouflageLocalClockStateLock.Lock()
	previousClock := extenderCamouflageLocalClock
	if now != nil {
		extenderCamouflageLocalClock = now
	}
	extenderCamouflageLocalClockStateLock.Unlock()

	defaultExtenderCamouflageClockStateLock.Lock()
	previousOffsets := defaultExtenderCamouflageClock
	defaultExtenderCamouflageClock = newExtenderCamouflageClock()
	defaultExtenderCamouflageClockStateLock.Unlock()
	return func() {
		extenderCamouflageLocalClockStateLock.Lock()
		extenderCamouflageLocalClock = previousClock
		extenderCamouflageLocalClockStateLock.Unlock()
		defaultExtenderCamouflageClockStateLock.Lock()
		defaultExtenderCamouflageClock = previousOffsets
		defaultExtenderCamouflageClockStateLock.Unlock()
	}
}

// Testing_ExtenderCamouflageClockOffset is the offset the next camouflaged hello
// to the extender with this identity key would seal with, for assertions.
func Testing_ExtenderCamouflageClockOffset(identityPublicKey []byte) time.Duration {
	return currentExtenderCamouflageClock().offsetFor(identityPublicKey, extenderCamouflageLocalNow())
}
