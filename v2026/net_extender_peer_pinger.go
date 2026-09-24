package connect

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"maps"
	"math"
	mathrand "math/rand"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"
)

// The extender peer pinger (GEOMAP §2.1).
//
// An extender measures its peers. Every active verified extender in its
// directory is pinged soon after it is first seen and again on a long
// cadence, each ping signed as this extender and answered by the peer's
// verdict. The pings are the extender to extender edges that a provider's
// probes cannot give, between the nodes whose positions are the most stable.
//
// A record released to gossip reaches every extender at once, so a newcomer
// is pinged at a uniform offset within the spread rather than by everyone
// together, which its per-source probe limit would turn into a measurement of
// a queue. After that every peer is pinged again a refresh timeout after its
// last ping, jittered, so the pings of a network that came up together drift
// apart. One ping is one candidate per address family the peer lists, walked
// exactly as the network client's probe pass walks one, and every probe that
// attested goes to the reporter whatever the peer answered. The pinger keeps
// the rest -- the latest pings and their counts -- in memory only, because
// yesterday's path is not today's.
//
// The peers are a bounded sample of the directory, not every extender it
// holds (GEOMAP §2.1, D26): at most PeerSampleSize of them, the nearest by the
// continent hint first and the rest a uniform draw of the others, drawn again
// every refresh, so one day's pinging stays linear in the fleet's size while
// over days a source's pings spread across it. The sample is read from the
// directory's active tier index, so a pass costs what the sample holds, not
// what the directory does.
//
// The schedule is keyed by the peer's identity key and brought up to date
// from the sample on every directory change and every pass timeout: a peer
// that leaves the sample leaves the schedule, and one whose addresses change
// is due again within the spread. One loop owns the schedule; each ping runs
// on its own goroutine, at most Concurrency at once, and Close joins them all.

// Whom a peer pinger pings, how often and how hard.
type ExtenderPeerPingerSettings struct {
	Log Logger

	// This extender's identity key. Its own record is never pinged, and with an attestor nothing is pinged while its own record is
	// not active in the directory: a peer judges this extender against the
	// same records, and would refuse every claim as an unknown pinger. Empty
	// takes the attestor's key.
	OwnPublicKey []byte
	// Signs every ping as this extender, and must be of the extender kind. Nil, or another kind, pings to measure only: nothing is attested
	// and nothing reported.
	Attestor *ExtenderProbeAttestor
	// Receives every ping that attested, whatever the peer answered (GEOMAP
	// §2.5). Nil keeps the measurements local.
	Reporter *ExtenderPingReporter

	// A peer first seen, or seen at new addresses, is pinged at a uniform
	// offset within this. 0 pings it at once.
	SpreadTimeout time.Duration
	// A peer is pinged again this long after its last ping, moved by the
	// jitter. The default is half the day the operator keeps pings for, and
	// the jitter keeps every refresh under twice this, so a peer's pings are
	// always renewed before the last ones age out.
	RefreshTimeout time.Duration
	// The fraction of RefreshTimeout a refresh is moved by, either way, drawn
	// uniformly per ping. Outside [0, 1) it is the default, so a refresh never
	// collapses to nothing or doubles.
	Jitter float64
	// Pings in flight at once.
	Concurrency int
	// Probes of one ping, of which the lowest rtt is kept.
	ProbeCount int
	// Budget of one probe.
	ProbeTimeout time.Duration
	// Pings kept in the status ring, the oldest dropped first.
	RecordCount int
	// The most peers pinged (GEOMAP §2.1, D26): the nearest by the directory's
	// continent hint first, in the directory's candidate order, and the rest a
	// uniform draw of the others, drawn again every RefreshTimeout. A peer
	// that stays in the sample keeps its schedule; one rotated out is dropped
	// from it once any ping of it has ended. <= 0 pings every active peer,
	// which is for tests.
	PeerSampleSize int
	// The schedule is also brought up to date this often with no directory
	// change, which is what notices a record that expired or a hold that
	// lapsed: neither changes the directory.
	PassTimeout time.Duration

	// The only clock the schedule reads. Tests install a fake one.
	Now func() time.Time
	// When set, draws the uniform [0, 1) the spread, the jitter and the
	// rotation of the sample's members are taken from. Nil is math/rand. Tests pin the schedule with it. It is
	// only ever called with the pinger's state lock held.
	Random func() float64
	// When set, replaces one probe of one peer carrier inside the carrier
	// walk, which is ProbeExtenderLatency otherwise. Tests drive the
	// verdicts through it; what it returns is recorded and reported exactly
	// as a real probe is.
	Ping func(
		ctx context.Context,
		extenderConfig *ExtenderConfig,
		attestor *ExtenderProbeAttestor,
	) (*ExtenderLatencyProbe, error)
	// When set, replaces the host family probe. Nil uses
	// probeFamilySupport: a family this host has no address of is never
	// pinged, since every probe of it would fail and hold the peer's address.
	IpVersionSupported func(ipVersion int) bool
}

// The cadence of GEOMAP §2.1: a new peer within an hour, every peer twice a
// day, two at a time, the lowest of two probes, 64 peers at most.
func DefaultExtenderPeerPingerSettings() *ExtenderPeerPingerSettings {
	return &ExtenderPeerPingerSettings{
		SpreadTimeout:  1 * time.Hour,
		RefreshTimeout: 12 * time.Hour,
		Jitter:         0.1,
		Concurrency:    2,
		ProbeCount:     2,
		ProbeTimeout:   5 * time.Second,
		RecordCount:    256,
		PeerSampleSize: 64,
		PassTimeout:    1 * time.Minute,
		Now:            time.Now,
	}
}

// What the extender status shows of its pings. Comparable, so it rides a
// MonitorValue and a consumer is woken only on a change.
type ExtenderPeerPingerStatus struct {
	// The active verified extenders but this one, which the sample is drawn
	// from.
	PeerCount int
	// The most peers the sample holds, PeerSampleSize, 0 when every peer is
	// pinged; and the peers in the sample now, which is the schedule.
	SampleSize       int
	SampledPeerCount int
	// Pings completed, one per peer address family, and what they came to.
	PingCount     int
	CosignedCount int
	RejectedCount int
	UnknownCount  int
	// Pings that measured and attested nothing: the peer issued no nonce,
	// which one that predates extender pings does.
	UnattestedCount int
	// Pings in which no carrier of the peer answered.
	FailedCount int
	// When the last ping completed, zero before the first.
	LastPingTime time.Time
}

// One ping as the status ring keeps it.
type ExtenderPingRecord struct {
	Time            time.Time
	TargetPublicKey []byte
	Ip              netip.Addr
	// The lowest rtt of the ping's probes, zero when no carrier answered.
	Rtt     time.Duration
	Outcome ExtenderPingOutcome
	Reason  uint32
}

// One extender's pinger of its peers. Safe for concurrent use; the schedule is
// owned by its one loop.
type ExtenderPeerPinger struct {
	ctx       context.Context
	cancel    context.CancelFunc
	closeOnce sync.Once
	done      chan struct{}
	log       Logger

	directory       *ExtenderDirectory
	settings        *ExtenderPeerPingerSettings
	connectSettings *ConnectSettings
	// the extender kind attestor, nil to measure only
	attestor        *ExtenderProbeAttestor
	ownPublicKey    []byte
	ownPublicKeyHex string

	statusMonitor *MonitorValue[ExtenderPeerPingerStatus]
	// notified when a ping ends, which frees a slot
	pingEndMonitor *Monitor

	stateLock sync.Mutex
	// the schedule by the peer's hex identity key: the sample, and a peer
	// rotated out of it while a ping of it is in flight
	peers map[string]*extenderPeerPingerPeer
	// the latest pings, oldest first from recordHead; the dropped prefix is
	// reclaimed once it is half the slice, so a ping costs the ring constant
	// time
	records    []ExtenderPingRecord
	recordHead int
	// when the random slice of the sample was last drawn, zero before the
	// first
	sampleTime time.Time
	// the nearest peers in order, and the version of the directory's near
	// pool they were read at
	nearestKeyHexes []string
	nearVersion     uint64

	// the ping goroutines, added only by the loop and joined when it ends
	pingWorkers sync.WaitGroup
}

// One peer in the schedule.
type extenderPeerPingerPeer struct {
	publicKey []byte
	// the peer's addresses as the directory last listed them, which is what
	// an address change is judged by
	addressKey string
	dueTime    time.Time
	// a ping of this peer is in flight
	pinging bool
	// the peer is in the sample, and among its nearest
	sampled bool
	near    bool
}

// One peer of a pass: its addresses as a key an address change is judged by,
// and whether it is one of the nearest.
type extenderPeerPingerTarget struct {
	addressKey string
	near       bool
}

// The pinger is running when this returns. The strategy's connect settings
// dial every probe; a nil strategy dials with the defaults.
func NewExtenderPeerPinger(
	ctx context.Context,
	clientStrategy *ClientStrategy,
	directory *ExtenderDirectory,
	settings *ExtenderPeerPingerSettings,
) *ExtenderPeerPinger {
	if settings == nil {
		settings = DefaultExtenderPeerPingerSettings()
	}
	// the caller's settings are never written; what is kept is a copy with
	// every unusable value replaced by its default
	copied := *settings
	defaults := DefaultExtenderPeerPingerSettings()
	copied.OwnPublicKey = slices.Clone(settings.OwnPublicKey)
	if copied.SpreadTimeout < 0 {
		copied.SpreadTimeout = 0
	}
	if copied.RefreshTimeout <= 0 {
		copied.RefreshTimeout = defaults.RefreshTimeout
	}
	if copied.Jitter < 0 || 1 <= copied.Jitter || math.IsNaN(copied.Jitter) {
		copied.Jitter = defaults.Jitter
	}
	if copied.Concurrency <= 0 {
		copied.Concurrency = defaults.Concurrency
	}
	if copied.ProbeCount <= 0 {
		copied.ProbeCount = defaults.ProbeCount
	}
	if copied.ProbeTimeout <= 0 {
		copied.ProbeTimeout = defaults.ProbeTimeout
	}
	if copied.RecordCount <= 0 {
		copied.RecordCount = defaults.RecordCount
	}
	if copied.PassTimeout <= 0 {
		copied.PassTimeout = defaults.PassTimeout
	}
	if copied.Now == nil {
		copied.Now = time.Now
	}
	if copied.Random == nil {
		copied.Random = mathrand.Float64
	}
	settings = &copied

	log := loggerOrDefault(settings.Log)
	var attestor *ExtenderProbeAttestor
	if settings.Attestor != nil {
		if settings.Attestor.Kind() == ExtenderPingerKindExtender {
			copiedAttestor := *settings.Attestor
			copiedAttestor.ExtenderPublicKey = slices.Clone(settings.Attestor.ExtenderPublicKey)
			attestor = &copiedAttestor
		} else {
			log.Infof("[extender]peer ping attestor is not an extender; peers are measured only\n")
		}
	}
	settings.Attestor = attestor
	ownPublicKey := settings.OwnPublicKey
	if len(ownPublicKey) == 0 && attestor != nil {
		ownPublicKey = slices.Clone(attestor.ExtenderPublicKey)
	}
	if len(ownPublicKey) == ed25519.PublicKeySize {
		// the peers judge this extender's pings by its own record, so the
		// directory's cap never evicts it
		directory.KeepPublicKey(ownPublicKey)
	}

	connectSettings := DefaultConnectSettings()
	if clientStrategy != nil {
		connectSettings = &clientStrategy.settings.ConnectSettings
	}

	cancelCtx, cancel := context.WithCancel(ctx)
	self := &ExtenderPeerPinger{
		ctx:             cancelCtx,
		cancel:          cancel,
		done:            make(chan struct{}),
		log:             log,
		directory:       directory,
		settings:        settings,
		connectSettings: connectSettings,
		attestor:        attestor,
		ownPublicKey:    ownPublicKey,
		ownPublicKeyHex: hex.EncodeToString(ownPublicKey),
		statusMonitor:   NewMonitorValue[ExtenderPeerPingerStatus](ExtenderPeerPingerStatus{}),
		pingEndMonitor:  NewMonitor(),
		peers:           map[string]*extenderPeerPingerPeer{},
		records:         []ExtenderPingRecord{},
	}
	go HandleError(func() {
		defer close(self.done)
		self.run()
	}, cancel)
	return self
}

// The status now, without subscribing.
func (self *ExtenderPeerPinger) Status() ExtenderPeerPingerStatus {
	return self.statusMonitor.Value()
}

// The status value and a channel armed at the same instant, for a consumer
// that renders it.
func (self *ExtenderPeerPinger) StatusMonitor() *MonitorValue[ExtenderPeerPingerStatus] {
	return self.statusMonitor
}

// The latest pings, oldest first, as copies the caller owns.
func (self *ExtenderPeerPinger) Records() []ExtenderPingRecord {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	records := slices.Clone(self.records[self.recordHead:])
	for i := range records {
		records[i].TargetPublicKey = slices.Clone(records[i].TargetPublicKey)
	}
	return records
}

// Ends the loop and every ping in flight, and joins them.
func (self *ExtenderPeerPinger) Close() {
	self.closeOnce.Do(func() {
		self.cancel()
		<-self.done
	})
}

// The schedule loop: one pass on every directory change, every ended ping and
// every pass timeout, or sooner when a peer comes due.
func (self *ExtenderPeerPinger) run() {
	// a ping in flight holds the directory and the reporter; the loop does
	// not return until every one has ended
	defer self.pingWorkers.Wait()
	for {
		select {
		case <-self.ctx.Done():
			return
		default:
		}
		// subscribe before the pass, so a change or an ended ping that lands
		// while it runs wakes the next wait instead of being lost
		_, change := self.directory.ChangeMonitor().Get()
		pingEnd := self.pingEndMonitor.NotifyChannel()
		wait := self.pass()
		select {
		case <-self.ctx.Done():
			return
		case <-change:
		case <-pingEnd:
		case <-time.After(wait):
		}
	}
}

// One pass: brings the schedule up to date, starts what is due into the free
// slots, and returns how long the loop may wait before the next pass.
func (self *ExtenderPeerPinger) pass() time.Duration {
	now := self.settings.Now()
	if self.attestor != nil && !self.directory.IsActiveKey(self.ownPublicKey) {
		// every peer would refuse the claims of a pinger it does not know;
		// wait for this extender's own record to be active
		return self.settings.PassTimeout
	}
	var keyHexTargets map[string]extenderPeerPingerTarget
	var peerCount int
	if self.settings.PeerSampleSize <= 0 {
		// no bound: every active peer the directory lists
		keyHexTargets = self.activePeers()
		peerCount = len(keyHexTargets)
	} else {
		keyHexTargets = self.samplePeers(now)
		peerCount = self.directory.peerCount(self.ownPublicKey)
	}
	self.statusMonitor.Update(func(status ExtenderPeerPingerStatus) ExtenderPeerPingerStatus {
		status.PeerCount = peerCount
		status.SampleSize = max(0, self.settings.PeerSampleSize)
		status.SampledPeerCount = len(keyHexTargets)
		return status
	})
	// one dialable candidate per address family of a peer: the first of its
	// addresses in the directory's own candidate order, the order every other
	// dial takes. A family this host has no address of is left out -- every
	// probe of it would fail and hold the peer's address -- and so is a
	// limited address, which is pinged once its limit passes (A12).
	ipVersionSupported := self.settings.IpVersionSupported
	if ipVersionSupported == nil {
		ipVersionSupported = probeFamilySupport
	}
	peerCandidates := func(keyHex string) []*ExtenderCandidate {
		candidates := []*ExtenderCandidate{}
		for _, ipVersion := range []int{4, 6} {
			if !ipVersionSupported(ipVersion) {
				continue
			}
			candidate := self.directory.peerCandidate(keyHex, ipVersion)
			if candidate == nil || !candidate.Verified || candidate.Expired || len(candidate.PublicKey) != ed25519.PublicKeySize {
				continue
			}
			candidates = append(candidates, candidate)
		}
		return candidates
	}
	duePeers, freeCount := self.schedule(now, keyHexTargets)
	for _, keyHex := range duePeers {
		if freeCount <= 0 {
			break
		}
		candidates := peerCandidates(keyHex)
		if len(candidates) == 0 {
			// nothing of it is dialable now -- held, limited, or of a family
			// this host lacks; it stays due for the next pass
			continue
		}
		if self.startPing(keyHex, candidates) {
			freeCount -= 1
		}
	}
	return self.nextWait(now)
}

// The sample of this pass (GEOMAP §2.1, D26): the nearest peers, as many as
// PeerSampleSize, and the rest a uniform draw of the others. The draw is
// taken again every RefreshTimeout, which is what rotates a source's pings
// across the fleet; between draws a peer that is still an active peer keeps
// its place, and the place of one that left is drawn again. The nearest are
// read again whenever the directory's near pool changes. Only the loop calls
// this, so what it reads of its own state between the directory calls, which
// take no pinger lock, stays its own.
func (self *ExtenderPeerPinger) samplePeers(now time.Time) map[string]extenderPeerPingerTarget {
	size := self.settings.PeerSampleSize

	var refresh bool
	var nearestKeyHexes []string
	var nearVersion uint64
	memberKeyHexes := []string{}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		refresh = self.sampleTime.IsZero() || self.settings.RefreshTimeout <= now.Sub(self.sampleTime)
		nearestKeyHexes = self.nearestKeyHexes
		nearVersion = self.nearVersion
		for keyHex, peer := range self.peers {
			if peer.sampled {
				memberKeyHexes = append(memberKeyHexes, keyHex)
			}
		}
	}()
	if directoryNearVersion := self.directory.nearVersion(); refresh || directoryNearVersion != nearVersion {
		nearestKeyHexes, nearVersion = self.directory.nearPeerKeyHexes(size, self.ownPublicKeyHex)
	}
	nearKeyHexes := make(map[string]bool, len(nearestKeyHexes))
	for _, keyHex := range nearestKeyHexes {
		nearKeyHexes[keyHex] = true
	}

	// between refreshes the members still active keep their places
	randomKeyHexes := []string{}
	if !refresh {
		slices.Sort(memberKeyHexes)
		for i, addressKey := range self.directory.peerAddressKeys(memberKeyHexes) {
			if keyHex := memberKeyHexes[i]; addressKey != "" && !nearKeyHexes[keyHex] {
				randomKeyHexes = append(randomKeyHexes, keyHex)
			}
		}
	}
	randomCount := max(0, size-len(nearestKeyHexes))
	if randomCount < len(randomKeyHexes) {
		// the nearest took places of the random slice: which members give
		// them up is drawn
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			for i := range randomCount {
				n := len(randomKeyHexes) - i
				j := i + min(max(int(self.randomWithLock()*float64(n)), 0), n-1)
				randomKeyHexes[i], randomKeyHexes[j] = randomKeyHexes[j], randomKeyHexes[i]
			}
		}()
		randomKeyHexes = randomKeyHexes[:randomCount]
	}
	if len(randomKeyHexes) < randomCount {
		// this extender, the nearest, and the members that keep their places
		keptKeyHexes := make(map[string]bool, len(randomKeyHexes))
		for _, keyHex := range randomKeyHexes {
			keptKeyHexes[keyHex] = true
		}
		excluded := func(keyHex string) bool {
			return keyHex == self.ownPublicKeyHex || nearKeyHexes[keyHex] || keptKeyHexes[keyHex]
		}
		randomKeyHexes = append(
			randomKeyHexes,
			self.directory.drawPeerKeyHexes(
				randomCount-len(randomKeyHexes),
				excluded,
				1+len(nearestKeyHexes)+len(randomKeyHexes),
			)...,
		)
	}

	keyHexes := append(slices.Clone(nearestKeyHexes), randomKeyHexes...)
	keyHexTargets := make(map[string]extenderPeerPingerTarget, len(keyHexes))
	for i, addressKey := range self.directory.peerAddressKeys(keyHexes) {
		if addressKey != "" {
			keyHex := keyHexes[i]
			keyHexTargets[keyHex] = extenderPeerPingerTarget{
				addressKey: addressKey,
				near:       nearKeyHexes[keyHex],
			}
		}
	}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.nearestKeyHexes = nearestKeyHexes
		self.nearVersion = nearVersion
		if refresh {
			self.sampleTime = now
		}
	}()
	return keyHexTargets
}

// The keys of the current sample, as copies the caller owns, in key order.
func (self *ExtenderPeerPinger) SampledPeers() [][]byte {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	keyHexes := []string{}
	for keyHex, peer := range self.peers {
		if peer.sampled {
			keyHexes = append(keyHexes, keyHex)
		}
	}
	slices.Sort(keyHexes)
	publicKeys := [][]byte{}
	for _, keyHex := range keyHexes {
		publicKeys = append(publicKeys, slices.Clone(self.peers[keyHex].publicKey))
	}
	return publicKeys
}

// The peers the directory vouches for now, each with its addresses sorted and
// joined: every active verified identity but this extender's own. A held or
// failing address is still a peer; only revoked, expired and unverified
// entries are not. This is the whole directory, which only an unbounded
// sample reads.
func (self *ExtenderPeerPinger) activePeers() map[string]extenderPeerPingerTarget {
	keyHexIps := map[string][]string{}
	for _, entry := range self.directory.Snapshot().Entries {
		if len(entry.PublicKey) != ed25519.PublicKeySize {
			continue
		}
		switch entry.State {
		case ExtenderStateActive, ExtenderStateWarning, ExtenderStateHold:
		default:
			continue
		}
		keyHex := hex.EncodeToString(entry.PublicKey)
		if keyHex == self.ownPublicKeyHex {
			continue
		}
		keyHexIps[keyHex] = append(keyHexIps[keyHex], entry.Ip.String())
	}
	activePeers := map[string]extenderPeerPingerTarget{}
	for keyHex, ips := range keyHexIps {
		slices.Sort(ips)
		activePeers[keyHex] = extenderPeerPingerTarget{
			addressKey: strings.Join(ips, ","),
		}
	}
	return activePeers
}

// Brings the schedule up to date with the peers of this pass: a new peer is
// due within the spread, a peer at new addresses is due within the spread at
// the latest, and a peer that left is dropped once no ping of it is in
// flight. Returns the due peers that are not being pinged, the nearest first
// and then the longest overdue, and how many pings may start.
func (self *ExtenderPeerPinger) schedule(now time.Time, keyHexTargets map[string]extenderPeerPingerTarget) ([]string, int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	for keyHex, peer := range self.peers {
		if _, target := keyHexTargets[keyHex]; !target {
			peer.sampled = false
			peer.near = false
			if !peer.pinging {
				delete(self.peers, keyHex)
			}
		}
	}
	// in key order, so the draws a schedule takes from Random are stable
	for _, keyHex := range slices.Sorted(maps.Keys(keyHexTargets)) {
		target := keyHexTargets[keyHex]
		peer := self.peers[keyHex]
		switch {
		case peer == nil:
			publicKey, err := hex.DecodeString(keyHex)
			if err != nil {
				continue
			}
			peer = &extenderPeerPingerPeer{
				publicKey:  publicKey,
				addressKey: target.addressKey,
				dueTime:    now.Add(self.spreadTimeoutWithLock()),
			}
			self.peers[keyHex] = peer
		case peer.addressKey != target.addressKey:
			peer.addressKey = target.addressKey
			if spreadTime := now.Add(self.spreadTimeoutWithLock()); spreadTime.Before(peer.dueTime) {
				peer.dueTime = spreadTime
			}
		}
		peer.sampled = true
		peer.near = target.near
	}

	pingingCount := 0
	duePeers := []string{}
	for keyHex, peer := range self.peers {
		if peer.pinging {
			pingingCount += 1
			continue
		}
		if !now.Before(peer.dueTime) {
			duePeers = append(duePeers, keyHex)
		}
	}
	slices.SortFunc(duePeers, func(a string, b string) int {
		if aNear, bNear := self.peers[a].near, self.peers[b].near; aNear != bNear {
			if aNear {
				return -1
			}
			return 1
		}
		if c := self.peers[a].dueTime.Compare(self.peers[b].dueTime); c != 0 {
			return c
		}
		return strings.Compare(a, b)
	})
	return duePeers, self.settings.Concurrency - pingingCount
}

// How long the loop may wait: until the next peer comes due, and never past
// the pass timeout. A peer already due that could not start waits for a slot
// to free or for the next pass, not for a busy loop.
func (self *ExtenderPeerPinger) nextWait(now time.Time) time.Duration {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	wait := self.settings.PassTimeout
	for _, peer := range self.peers {
		if peer.pinging {
			continue
		}
		if untilDue := peer.dueTime.Sub(now); 0 < untilDue && untilDue < wait {
			wait = untilDue
		}
	}
	return wait
}

// Starts the ping of one due peer on its own goroutine, and reports whether
// it started. Called only by the loop, which is what lets the loop join the
// ping goroutines when it ends.
func (self *ExtenderPeerPinger) startPing(keyHex string, candidates []*ExtenderCandidate) bool {
	var publicKey []byte
	var pingAddressKey string
	started := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		peer := self.peers[keyHex]
		if peer == nil || peer.pinging {
			return false
		}
		peer.pinging = true
		publicKey = slices.Clone(peer.publicKey)
		pingAddressKey = peer.addressKey
		return true
	}()
	if !started {
		return false
	}
	self.pingWorkers.Add(1)
	go HandleError(func() {
		defer self.pingWorkers.Done()
		// the peer is rescheduled and the slot freed however the ping ends,
		// a panic included
		var retryTime time.Time
		defer func() {
			self.endPing(keyHex, pingAddressKey, retryTime)
		}()
		retryTime = self.ping(publicKey, candidates)
	})
	return true
}

// Pings one peer: each family's candidate through the shared carrier walk,
// the lowest rtt into the directory, every attested probe to the reporter,
// and one record per family into the ring. A candidate that answers 429 is
// recorded nowhere -- a limit is a retry, never a refusal and never evidence
// (A12, GEOMAP §2.1) -- and the returned time, when set, is when the peer is
// to be pinged again: when its limit passes rather than a refresh away.
func (self *ExtenderPeerPinger) ping(publicKey []byte, candidates []*ExtenderCandidate) (retryTime time.Time) {
	for _, candidate := range candidates {
		select {
		case <-self.ctx.Done():
			return retryTime
		default:
		}
		candidateProbe, err := probeExtenderCandidate(
			self.ctx,
			self.directory,
			candidate,
			self.settings.ProbeCount,
			self.settings.ProbeTimeout,
			self.pingLatency,
		)
		if err != nil {
			if self.ctx.Err() != nil {
				// the end of the pinger, not a failure of the peer
				return retryTime
			}
			var limitedErr *ExtenderLimitedError
			if errors.As(err, &limitedErr) {
				// the walk marked the address limited; the peer is due again
				// once that passes
				if limitedUntil := self.directory.AddressLimitedUntil(candidate.Ip); !limitedUntil.IsZero() &&
					(retryTime.IsZero() || limitedUntil.Before(retryTime)) {
					retryTime = limitedUntil
				}
				if self.log.V(1).Enabled() {
					self.log.Infof("[extender]peer ping %s limited until %s\n", candidate.Ip, retryTime)
				}
				continue
			}
			if self.log.V(1).Enabled() {
				self.log.Infof("[extender]peer ping %s err = %s\n", candidate.Ip, err)
			}
			self.addRecord(ExtenderPingRecord{
				Time:            self.settings.Now(),
				TargetPublicKey: slices.Clone(publicKey),
				Ip:              candidate.Ip,
			}, true)
			continue
		}
		outcome, reason := candidateProbe.outcome()
		self.directory.RecordLatency(candidate.Ip, candidateProbe.rtt, outcome == ExtenderPingCosigned)
		if self.settings.Reporter != nil {
			for _, probe := range candidateProbe.probes {
				self.settings.Reporter.Report(ExtenderPingReportFromProbe(probe))
			}
		}
		if self.log.V(1).Enabled() {
			self.log.Infof(
				"[extender]peer ping %s rtt=%s outcome=%q reason=%d\n",
				candidate.Ip,
				candidateProbe.rtt,
				outcome,
				reason,
			)
		}
		self.addRecord(ExtenderPingRecord{
			Time:            self.settings.Now(),
			TargetPublicKey: slices.Clone(publicKey),
			Ip:              candidate.Ip,
			Rtt:             candidateProbe.rtt,
			Outcome:         outcome,
			Reason:          reason,
		}, false)
	}
	return retryTime
}

// One probe of one peer carrier, signed as this extender.
func (self *ExtenderPeerPinger) pingLatency(
	ctx context.Context,
	extenderConfig *ExtenderConfig,
) (*ExtenderLatencyProbe, error) {
	if self.settings.Ping != nil {
		return self.settings.Ping(ctx, extenderConfig, self.attestor)
	}
	return ProbeExtenderLatency(ctx, self.connectSettings, extenderConfig, self.attestor)
}

// Frees the peer's slot and schedules its next ping a jittered refresh out --
// sooner, within the spread, when its addresses changed while it was pinged,
// since what was measured is the old path -- and wakes the loop.
func (self *ExtenderPeerPinger) endPing(keyHex string, pingAddressKey string, retryTime time.Time) {
	now := self.settings.Now()
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		peer := self.peers[keyHex]
		if peer == nil {
			return
		}
		peer.pinging = false
		peer.dueTime = now.Add(self.refreshTimeoutWithLock())
		if !retryTime.IsZero() && retryTime.Before(peer.dueTime) {
			// a limited peer is pinged again when its limit passes (A12)
			peer.dueTime = retryTime
		}
		if peer.addressKey != pingAddressKey {
			if spreadTime := now.Add(self.spreadTimeoutWithLock()); spreadTime.Before(peer.dueTime) {
				peer.dueTime = spreadTime
			}
		}
	}()
	self.pingEndMonitor.NotifyAll()
}

// Keeps one ping in the ring and counts it.
func (self *ExtenderPeerPinger) addRecord(record ExtenderPingRecord, failed bool) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.records = append(self.records, record)
		if self.settings.RecordCount < len(self.records)-self.recordHead {
			self.recordHead = len(self.records) - self.settings.RecordCount
		}
		if 0 < self.recordHead && len(self.records) <= 2*self.recordHead {
			keptCount := copy(self.records, self.records[self.recordHead:])
			clear(self.records[keptCount:])
			self.records = self.records[:keptCount]
			self.recordHead = 0
		}
	}()
	self.statusMonitor.Update(func(status ExtenderPeerPingerStatus) ExtenderPeerPingerStatus {
		status.PingCount += 1
		switch {
		case failed:
			status.FailedCount += 1
		case record.Outcome == ExtenderPingCosigned:
			status.CosignedCount += 1
		case record.Outcome == ExtenderPingRejected:
			status.RejectedCount += 1
		case record.Outcome == ExtenderPingUnknown:
			status.UnknownCount += 1
		default:
			status.UnattestedCount += 1
		}
		status.LastPingTime = record.Time
		return status
	})
}

// A new peer's offset: uniform within the spread.
func (self *ExtenderPeerPinger) spreadTimeoutWithLock() time.Duration {
	return time.Duration(float64(self.settings.SpreadTimeout) * self.randomWithLock())
}

// The wait after one ping: the refresh timeout, moved by up to the jitter of
// itself either way.
func (self *ExtenderPeerPinger) refreshTimeoutWithLock() time.Duration {
	offset := (2*self.randomWithLock() - 1) * self.settings.Jitter
	return time.Duration(float64(self.settings.RefreshTimeout) * (1 + offset))
}

// The seam's draw, clamped to [0, 1) so a test's value can never move a
// schedule outside its window.
func (self *ExtenderPeerPinger) randomWithLock() float64 {
	value := self.settings.Random()
	if value < 0 || math.IsNaN(value) {
		return 0
	}
	if 1 <= value {
		return math.Nextafter(1, 0)
	}
	return value
}
