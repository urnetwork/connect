package connect

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect/protocol"
)

// The client extender directory (EXTENDER.md E1).
//
// Two kinds of entry live here. A verified identity is keyed by the extender
// public key and holds the newest signed record and the newest revocation by
// issue time (B5). An address is keyed by its ip and carries the local
// evidence -- successes, failures, hold, use -- that decides whether the
// strategy may dial it. An address learned from dns bootstrap or from manual
// configuration has no key; it upgrades to verified when a record listing that
// ip arrives, keeping the local evidence it has already collected.
//
// The policy numbers are all settings so a test can pin every transition, and
// `Now` is the only clock the policy reads, so a fake clock makes the whole
// policy deterministic. A time.Now reading carries a monotonic reading beside
// the wall one, and the monotonic clock stops while the host sleeps, so a
// latency sample's age and when a hold or a limit lapses are judged by
// whichever of the two clocks has moved further (extenderElapsed,
// extenderBefore): a sleep counts toward them, and a wall clock set back does
// not extend them.
//
// Methods are safe for concurrent use. The state lock is never held across a
// call to the store or to a monitor consumer; the save runs on the internal
// `run` goroutine started by the constructor, coalesced one save timeout after
// a change, so a burst of applied records costs one write. `Reset` writes the
// store itself before it returns, and a save never writes a state older than
// one already written, so the state a save read before a reset never lands
// after it.

// Where an address was learned (F2). The source is descriptive: it never
// changes once an address is known, so an upgrade to verified keeps the origin
// that first produced it.
const (
	ExtenderSourceDns       = "dns"
	ExtenderSourceFeed      = "feed"
	ExtenderSourceGossip    = "gossip"
	ExtenderSourceBootstrap = "bootstrap"
	ExtenderSourceManual    = "manual"
	// An address taken from a shared payload (K7). It is an ordinary
	// unverified bootstrap entry -- the removal policy applies to it, and it
	// upgrades when a record naming it arrives -- and is kept distinct from
	// `manual` so the status can say where it came from.
	ExtenderSourceImport = "import"
	// A record the operator released to this client's authenticated identity
	// (Q3): the gated tier. Not a network event (K4), like a bootstrap.
	ExtenderSourceRelease = "release"
)

// The address states reported by the status (F2), in precedence order: trust
// first, because a revoked or expired record is not dialable whatever the
// local evidence says, then the local failure evidence.
const (
	ExtenderStateActive     = "active"
	ExtenderStateWarning    = "warning"
	ExtenderStateHold       = "hold"
	ExtenderStateUnverified = "unverified"
	ExtenderStateRevoked    = "revoked"
	ExtenderStateExpired    = "expired"
)

// Version of the persisted envelope. A stored document of another version is
// discarded, exactly like an unreadable one.
const ExtenderDirectoryStoreVersion = 1

// Version of the operator's stored country, a section of the envelope with a
// version of its own (extenderDirectoryStoreCountryHint). A section of another
// version is skipped alone: the records and addresses beside it still load.
const extenderDirectoryStoreCountryHintVersion = 1

// The bounded buffer of one Subscribe consumer (D4). A consumer that falls
// this far behind is cut off rather than waited on.
const ExtenderDirectorySubscribeBufferCount = 64

// Cap of the apply-time ring (K4). The window prunes it long before this on
// any normal feed; the cap is what bounds a flood.
const ExtenderDirectoryEventRingCount = 1024

// The answer to a record or revocation whose verification began before a
// reset and ended after it (Reset): it is not applied.
var errExtenderDirectoryReset = errors.New("the extender directory was reset while the message was verified")

// The progress of the network client's first feed sample, which is what the
// startup gate waits on (E4). `None` means no network client is running, so
// nothing will ever complete and the gate must not wait at all.
type ExtenderInitialSampleState int

const (
	ExtenderInitialSampleNone ExtenderInitialSampleState = iota
	ExtenderInitialSamplePending
	ExtenderInitialSampleDone
)

// Persistence of one directory. The bytes are the JSON envelope below. A
// storage-less host installs no store, which keeps the directory in memory.
type ExtenderDirectoryStore interface {
	Load() ([]byte, error)
	Save([]byte) error
}

type ExtenderDirectorySettings struct {
	Log Logger

	// Store, when set, loads at construction and receives a coalesced save
	// after every change. Nil keeps the directory in memory.
	Store ExtenderDirectoryStore

	// NetworkHosts are the hosts whose records this directory accepts (B2):
	// the space host and its migration host. A record for any other host is
	// rejected, which is what separates two network spaces sharing one root
	// key.
	NetworkHosts []string

	// Hold after a failure, doubling per consecutive failure up to the max.
	// A hold lapses once either clock has passed it (extenderBefore), so the
	// time the host slept counts toward it.
	HoldTimeout    time.Duration
	MaxHoldTimeout time.Duration
	// Consecutive failures that make an address a warning.
	WarningConsecutiveFailureCount int
	// An address that has never succeeded is removed this long after its
	// first failure.
	NeverSucceededRemoveTimeout time.Duration
	// An address that has succeeded is removed when its last success is older
	// than this and it has reached the consecutive failure count.
	StaleSuccessRemoveTimeout     time.Duration
	RemoveConsecutiveFailureCount int
	// Clock skew allowed against a record expiry.
	RecordExpireSkew time.Duration
	// Cap on known addresses, the backstop beneath MaxActiveRecordCount: the
	// default leaves room for the addresses of every active record of either
	// family, the held, the retained expired and the bootstrap ones. Over the
	// cap the eviction order is revoked, then expired beyond the retained ones
	// oldest expiry first, then never succeeded oldest first, then oldest last
	// success. Manual addresses and the addresses of the retained expired
	// identities are never evicted. <= 0 keeps every address.
	MaxAddressCount int
	// The expired verified identities kept past their expiry, newest expiry
	// first, with their addresses and local evidence. A record lives a day,
	// so a client that loses every path to the operator would otherwise lose
	// every extender it knew within a day. What is kept is the last tier of
	// the candidate order and nothing more: never sampled into a feed or
	// gossip, never counted as active or usable, dialed only after every
	// current address. Beyond this many, `Expire` evicts the oldest expiry
	// first. <= 0 keeps none.
	MaxExpiredRecordCount int
	// The active verified identities kept (GEOMAP §2.1, D26): the records of
	// an active key with an address that is not held for failure, which is
	// what a phone can hold and what a feed sample, the gossip mesh and a
	// peer pinger draw from. The records on the hinted continent are
	// preferred, then the measured ones, then a random sample of the rest:
	// past the cap a new record from gossip or the feed evicts a random one
	// of the rest -- possibly itself -- and only once there is none of the
	// rest the oldest applied measured one, then the oldest applied one on
	// the hinted continent. A held record and an expired one are never
	// evicted by it, since their tiers keep their own bounds (MaxAddressCount
	// and the removal policy, MaxExpiredRecordCount), nor are this extender's
	// own record (KeepPublicKey) and a record with a manual address.
	// SetMaxActiveRecordCount replaces it at run time. <= 0 keeps every
	// record.
	MaxActiveRecordCount int
	// A change is saved this long after it lands, so a burst costs one write.
	SaveTimeout time.Duration
	// The trailing window the applied-record and applied-revocation rate is
	// kept and reported over (K4). The app panel shows the count over the
	// last minute, which is the default.
	EventWindowTimeout time.Duration
	// A latency sample older than this counts as never measured
	// (DESIGNNOTES4.md §4): the ordering stops trusting it and a probe pass
	// measures the address again. The default is half the day the operator
	// keeps pings for, so a provider's attested pings are renewed before the
	// previous ones age out of what each derivation reads (GEOMAP §2.1). The
	// age counts the time the host slept (extenderElapsed). A path change, and
	// a resume from a long sleep for the samples taken before it, keep every
	// sample in use until a probe replaces it (RefreshLatencies,
	// RefreshSleptLatencies). <= 0 keeps a sample until a probe replaces it.
	LatencyMaxAge time.Duration
	// How long an address that answered 429 with no Retry-After is left alone
	// before the jitter, which is the same +-50 % a Retry-After gets (A12). A
	// limited address is never a failure: it orders after every healthy one
	// until its backoff passes, and the probe pass and the feed dial skip it.
	// A backoff passes like a hold, by either clock.
	ExtenderLimitedBackoff time.Duration
	// How long the operator's last country stands in once its hint is no
	// longer current (SpoofCountryCode), by the wall clock from the operator's
	// last answer. The country is stored, so a restart keeps it: the iOS packet
	// tunnel extension usually ends with its tunnel, and a start where no
	// direct hint read succeeds (a whitelist-only network) on a host that
	// reports no network country would otherwise have none. Each answer renews
	// it, and the operator is asked at each start, path change and refresh
	// period, so this bounds only a client with no direct answer for that
	// long, such as one that left the country; past it the country is never
	// used. The default is a week: it outlasts the whitelist-only days between
	// two answers, and it is the age at which this directory already stops
	// trusting an address's last success (StaleSuccessRemoveTimeout). <= 0
	// never uses the last country once its hint is stale.
	CountryHintMaxAge time.Duration

	// WebRtcCarrierEnabled lets a candidate carry the peer-to-peer webrtc
	// carrier its record lists (EXTENDER.md S). Off by default: without a
	// signaling path a webrtc dial can only fail, and a failure counts
	// against the address's other carriers. The owner that installs a
	// carrier on its connect settings turns it on, here or at run time
	// (SetWebRtcCarrierEnabled).
	WebRtcCarrierEnabled bool

	// The secret the feed sample this directory serves is partitioned by
	// (Q2, net_extender_directory_partition.go): which open records a feed
	// client at one vantage is sampled from, and in what order each epoch.
	// Nil draws one at random for the life of the directory, which is all an
	// extender needs: a vantage is bound to one partition for as long as the
	// process runs, and a restart deals the partitions again. Never on the
	// wire. Tests pin it.
	PartitionSecret []byte
	// The epoch of the feed sample (Q2): within one epoch a vantage is
	// served the same sample of its partition, and the next epoch another.
	// <= 0 is one epoch forever. The default is ExtenderOpenEpochTimeout.
	OpenEpochTimeout time.Duration

	// The only clock the policy reads. Tests install a fake one.
	Now func() time.Time
	// When set, draws the uniform [0, 1) the active cap picks a random record
	// to evict by, and a peer pinger its random slice. Nil is math/rand.
	// Tests pin both with it. It is only ever called with the directory's
	// state lock held.
	Random func() float64
}

func DefaultExtenderDirectorySettings() *ExtenderDirectorySettings {
	return &ExtenderDirectorySettings{
		HoldTimeout:                    10 * time.Minute,
		MaxHoldTimeout:                 6 * time.Hour,
		WarningConsecutiveFailureCount: 1,
		NeverSucceededRemoveTimeout:    24 * time.Hour,
		StaleSuccessRemoveTimeout:      7 * 24 * time.Hour,
		RemoveConsecutiveFailureCount:  3,
		RecordExpireSkew:               5 * time.Minute,
		MaxAddressCount:                2048,
		MaxExpiredRecordCount:          64,
		MaxActiveRecordCount:           512,
		SaveTimeout:                    1 * time.Second,
		EventWindowTimeout:             60 * time.Second,
		LatencyMaxAge:                  12 * time.Hour,
		ExtenderLimitedBackoff:         30 * time.Second,
		CountryHintMaxAge:              7 * 24 * time.Hour,
		OpenEpochTimeout:               ExtenderOpenEpochTimeout,
		Now:                            time.Now,
	}
}

// One verified identity: the newest record and the newest revocation by issue
// time (B5). The revocation is kept even with no record, so a replayed older
// record cannot reactivate a revoked key.
type extenderDirectoryRecord struct {
	publicKey []byte

	record     *protocol.ExtenderRecord
	recordBody *protocol.ExtenderRecordBody

	revocation     *protocol.ExtenderRevocation
	revocationBody *protocol.ExtenderRevocationBody

	// every address a record of this key has listed, parsed, in the order
	// first listed; one another key has claimed since, or that the directory
	// dropped, is skipped wherever it is read and forgotten by a rebuild of
	// the tier index
	ips []netip.Addr
	// the order the records were applied in, which is the order the active
	// cap evicts a preferred record in, the oldest first
	applySerial uint64
	// the pool of the active tier index the key is in, and its index there
	// (net_extender_directory_tier.go)
	tierPool  extenderTierPool
	tierIndex int
}

// The local evidence about one address. `publicKeyHex` is empty while the
// address is unverified.
type extenderDirectoryAddress struct {
	ip           netip.Addr
	source       string
	publicKeyHex string

	addTime                 time.Time
	successCount            int
	failureCount            int
	lastSuccessTime         time.Time
	lastFailureTime         time.Time
	firstFailureTime        time.Time
	consecutiveFailureCount int
	holdUntilTime           time.Time
	lastUseTime             time.Time
	// the address answered 429 and is left alone until then (A12); per
	// process, never stored, and never a failure
	limitedUntilTime time.Time

	// the latest latency sample (DESIGNNOTES4.md): the lowest rtt of one
	// probe pass, when it was taken and whether the target co-signed a claim
	// of that pass (GEOMAP §2.3). Per process: never stored. A path change,
	// and a resume from a long sleep that followed it, keep it and make it
	// due a refresh (RefreshLatencies, RefreshSleptLatencies): it still ranks,
	// and the probe pass measures it again, the next sample replacing it.
	latency           time.Duration
	latencyTime       time.Time
	latencyAttested   bool
	latencyRefreshDue bool
	probeCount        int
}

// One dialable endpoint handed to the strategy and to the network client. The
// carriers, ports and tld come from the record; an unverified address carries
// the fixed carrier defaults, which is what a dns bootstrap address is dialed
// with before any record names it.
type ExtenderCandidate struct {
	Ip        netip.Addr
	IpVersion int
	// The identity key of a verified record, empty when unverified. The outer
	// leaf is checked against it (B3, E5).
	PublicKey []byte
	Carriers  []string
	// The exchange rendezvous id of the webrtc carrier the record lists
	// (EXTENDER.md S), zero when the record carries none. Carriers names
	// the carrier only when the directory has it enabled and the record
	// carries this id.
	WebRtcClientId Id
	TcpPort        int
	UdpPort        int
	// The first dns port, kept for a caller that predates DnsPorts.
	DnsPort int
	// Every dns port the record offers, ascending (L2). A dial tries these
	// first and then whichever of 4053 and 53 they do not include
	// (dnsCarrierPorts). Empty falls back to DnsPort.
	DnsPorts    []int
	DnsTld      string
	CountryCode string
	// The continent the operator stamped on the record, upper case; empty
	// for a record that predates it and for an unverified address
	// (DESIGNNOTES4.md §2).
	ContinentCode string
	// The directory tier the record is signed into (Q1):
	// ExtenderDirectoryTierOpen for a record that predates the field and for
	// an unverified address, ExtenderDirectoryTierGated for a released one.
	DirectoryTier int
	// The current latency sample, zero when there is none (DESIGNNOTES4.md).
	Latency time.Duration
	// Whether the target co-signed a claim of the pass that took the sample
	// (GEOMAP §2.3), which a provider's probe pass reads to find what it has
	// no co-signed measurement of yet.
	LatencyAttested bool
	// Whether the sample is due a refresh: it was taken before the last path
	// change, or before a long sleep the host resumed from (RefreshLatencies).
	// It still ranks, and a probe pass measures it again, as it does an
	// address with none.
	LatencyRefreshDue bool
	Source            string
	Verified          bool
	// Whether the record has expired: the candidate is one of the retained
	// expired identities (MaxExpiredRecordCount), which only the last tier of
	// Candidates carries.
	Expired bool
	// When the address stops being limited (A12), zero when it is not: it
	// answered 429, and a caller leaves it alone until then.
	LimitedUntil time.Time
}

// The dns carrier ports one dial of this candidate races, in launch order
// (L2, net_extender_dns_ports.go): the ports it offers, ascending -- DnsPorts
// when it has them, else the single DnsPort a candidate built before DnsPorts
// carries -- and then whichever of 4053 and 53 those do not include, 4053
// first. Every extender binds 4053 and only the sn miner also binds 53, so a
// record that lists 4053 alone, which is every app extender's, and an
// address with no record are both dialed on 4053 first. A candidate that
// offers no dns port has no dns carrier to dial (E3).
func (self *ExtenderCandidate) dnsCarrierPorts() []int {
	dnsPorts := orderedDnsPorts(self.DnsPorts)
	if len(dnsPorts) == 0 {
		dnsPorts = orderedDnsPorts([]int{self.DnsPort})
	}
	if len(dnsPorts) == 0 {
		return nil
	}
	for _, dnsPort := range []int{ExtenderDnsPort, DefaultDnsPort} {
		if !slices.Contains(dnsPorts, dnsPort) {
			dnsPorts = append(dnsPorts, dnsPort)
		}
	}
	return dnsPorts
}

// One address as the status reports it (F2).
type ExtenderDirectoryEntry struct {
	Ip            netip.Addr
	IpVersion     int
	PublicKey     []byte
	Carriers      []string
	CountryCode   string
	ContinentCode string
	// the directory tier of the record (Q1), open for an unverified address
	DirectoryTier   int
	Latency         time.Duration
	State           string
	Source          string
	LastSuccessTime time.Time
	LastFailureTime time.Time
	SuccessCount    int
	FailureCount    int
	InUse           int
	ExpireTime      time.Time
	// When the address stops being limited (A12), zero when it is not.
	LimitedUntil time.Time
}

// The whole directory as the status reads it, with the counts the sdk exposes
// so a caller does not recount the entries.
type ExtenderDirectorySnapshot struct {
	Entries      []*ExtenderDirectoryEntry
	KnownCount   int
	ActiveCount  int
	WarningCount int
	HoldCount    int
	// Addresses carrying at least one live platform transport connection right
	// now, which is what the app panel draws a ring for (K4). It counts
	// addresses, not connections.
	InUseCount int
}

type ExtenderDirectory struct {
	ctx       context.Context
	cancel    context.CancelFunc
	closeOnce sync.Once
	done      chan struct{}
	log       Logger

	settings *ExtenderDirectorySettings

	// increments on every change; the save loop watches it, and a consumer
	// that renders the directory watches it too
	changeMonitor *MonitorValue[uint64]
	// the startup gate's rendezvous with the network client (E4)
	initialSampleMonitor *MonitorValue[ExtenderInitialSampleState]

	// orders the writes through the store: a save holds it from judging
	// whether what it read is still the newest to the store's answer, so a
	// save that read the state before a reset never lands after the reset's
	// own (save). Taken before the state lock, never inside it.
	saveLock sync.Mutex
	// test seam only, nil otherwise: runs between a save's read of the state
	// and its write, where a reset and its own save can land
	saveReadHook func()

	stateLock  sync.Mutex
	rootKeySet *ExtenderRootKeySet
	// whether candidates carry the webrtc carrier their records list
	// (ExtenderDirectorySettings.WebRtcCarrierEnabled, SetWebRtcCarrierEnabled)
	webRtcCarrierEnabled bool
	// advances with every Reset, which is how a record or revocation whose
	// verification began before a reset is told apart from one that began
	// after it (applyVerifiedRecord)
	resetVersion uint64
	// test seam only, nil otherwise: runs between the verification of an
	// applied message and its apply, where a reset can land
	applyVerifiedHook func()
	// the live connections through each address (SetInUse), by ip whether or
	// not the address is known: a connection outlives a reset that dropped
	// its address, and its release must still balance its hold once the
	// address is known again
	ipInUseCounts map[netip.Addr]int
	// The apply times of the records and revocations that arrived over the
	// feed or the mesh, oldest first from eventHead, pruned to the event
	// window (K4). Only those two sources count: a stored record loaded at
	// start and an address added by hand are not network events. The pruned
	// prefix before eventHead is reclaimed once it is half the slice, so a
	// flood of applies costs each one constant time.
	eventTimes []time.Time
	eventHead  int
	// the continent the candidate order prefers, upper case, empty until the
	// network client learns one (DESIGNNOTES4.md §4)
	continentHint string
	// the country the operator's hint last placed this client in, lower
	// case, empty until it has placed it; when the operator last answered it,
	// by the wall clock alone, as the store keeps it; and whether that answer
	// is current: given on the path this client is on now, by the latest
	// hint. A failed hint and a path change leave the country stale, and a
	// country loaded from the store is stale from the start (SpoofCountryCode)
	countryHint        string
	countryHintTime    time.Time
	countryHintCurrent bool
	// verified identities by hex public key
	keyHexRecords map[string]*extenderDirectoryRecord
	// every known address
	ipAddresses  map[netip.Addr]*extenderDirectoryAddress
	version      uint64
	savedVersion uint64
	// the live subscriptions of Subscribe, which the feed server of phase 5a
	// streams from
	subscriptions map[*extenderDirectorySubscription]bool

	// the active tier index (net_extender_directory_tier.go): the keys of
	// each pool, the earliest time a pooled record changes pool with the
	// clock alone, zero when none will, and the version of the near pool
	tierPoolKeyHexes [extenderTierPoolCount][]string
	tierSweepTime    time.Time
	tierNearVersion  uint64
	// the keys whose records the active cap never evicts (KeepPublicKey)
	keptKeyHexes map[string]bool
	// the active cap in force: MaxActiveRecordCount, until
	// SetMaxActiveRecordCount replaces it
	maxActiveRecordCount int
	// the serial the next applied record takes
	nextApplySerial uint64
	// the secret the feed sample is partitioned by (Q2): the setting, or one
	// drawn at construction
	partitionSecret []byte
}

// One live subscription to the applied messages (D4). The channel is the
// bounded buffer: the directory never waits on a subscriber, and a subscriber
// that fills it is cut off by closing the channel, which is what its consumer
// reads as "fell behind, disconnect".
type extenderDirectorySubscription struct {
	messages chan *protocol.ExtenderGossipMessage
	closed   bool
}

func NewExtenderDirectoryWithDefaults(ctx context.Context) *ExtenderDirectory {
	return NewExtenderDirectory(ctx, DefaultExtenderDirectorySettings())
}

// The directory is running when this returns: the store has been loaded and
// the coalesced save loop is up.
func NewExtenderDirectory(
	ctx context.Context,
	settings *ExtenderDirectorySettings,
) *ExtenderDirectory {
	if settings == nil {
		settings = DefaultExtenderDirectorySettings()
	}
	if settings.Now == nil {
		copied := *settings
		copied.Now = time.Now
		settings = &copied
	}
	cancelCtx, cancel := context.WithCancel(ctx)
	self := &ExtenderDirectory{
		ctx:                  cancelCtx,
		cancel:               cancel,
		done:                 make(chan struct{}),
		log:                  loggerOrDefault(settings.Log),
		settings:             settings,
		changeMonitor:        NewMonitorValue[uint64](0),
		initialSampleMonitor: NewMonitorValue[ExtenderInitialSampleState](ExtenderInitialSampleNone),
		rootKeySet:           NewExtenderRootKeySet(),
		ipInUseCounts:        map[netip.Addr]int{},
		keyHexRecords:        map[string]*extenderDirectoryRecord{},
		ipAddresses:          map[netip.Addr]*extenderDirectoryAddress{},
		subscriptions:        map[*extenderDirectorySubscription]bool{},
		keptKeyHexes:         map[string]bool{},
		maxActiveRecordCount: settings.MaxActiveRecordCount,
		partitionSecret:      slices.Clone(settings.PartitionSecret),
		webRtcCarrierEnabled: settings.WebRtcCarrierEnabled,
	}
	if len(self.partitionSecret) == 0 {
		// one per directory: a vantage is bound to one partition for the life
		// of the process, and nothing outside it can compute the placement
		self.partitionSecret = make([]byte, 32)
		if _, err := rand.Read(self.partitionSecret); err != nil {
			panic(err)
		}
	}
	self.load()
	// arm the save loop's subscription here, not inside the goroutine: a
	// change that lands between the constructor returning and the goroutine
	// being scheduled would otherwise close a channel nobody held, and the
	// first save would wait for a second change
	_, notify := self.changeMonitor.Get()
	go HandleError(func() {
		defer close(self.done)
		self.run(notify)
	}, cancel)
	return self
}

// The change counter. A consumer reads the snapshot and waits on the channel
// the same `Get` returned, which is what keeps the read and the subscribe from
// separating.
func (self *ExtenderDirectory) ChangeMonitor() *MonitorValue[uint64] {
	return self.changeMonitor
}

// The startup gate's view of the network client's first sample (E4).
func (self *ExtenderDirectory) InitialSampleMonitor() *MonitorValue[ExtenderInitialSampleState] {
	return self.initialSampleMonitor
}

// Announces that a network client is running and has not finished its first
// attempt. Only the network client calls this, from its constructor.
func (self *ExtenderDirectory) SetInitialSamplePending() {
	self.initialSampleMonitor.Update(func(state ExtenderInitialSampleState) ExtenderInitialSampleState {
		if state == ExtenderInitialSampleDone {
			return state
		}
		return ExtenderInitialSamplePending
	})
}

// Announces that the first attempt finished, whether or not it produced a
// sample. The gate never waits again after this.
func (self *ExtenderDirectory) SetInitialSampleDone() {
	self.initialSampleMonitor.Set(ExtenderInitialSampleDone)
}

// Replaces the accepted root keys (B4). Stored records are re-verified under
// the new set and dropped when they no longer verify, so a rotation that
// retires a key also retires everything it signed. An empty set is the
// unconfigured state and judges nothing: `Apply` will reject every message
// until keys arrive, and what is already stored is left alone.
func (self *ExtenderDirectory) SetRootKeys(keySet *ExtenderRootKeySet) {
	if keySet == nil {
		keySet = NewExtenderRootKeySet()
	}
	changed := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		self.rootKeySet = keySet
		if keySet.Len() == 0 {
			return
		}
		for keyHex, keyRecord := range self.keyHexRecords {
			if keyRecord.record != nil {
				if _, err := keySet.VerifyRecord(keyRecord.record); err != nil {
					keyRecord.record = nil
					keyRecord.recordBody = nil
					changed = true
				}
			}
			if keyRecord.revocation != nil {
				if _, err := keySet.VerifyRevocation(keyRecord.revocation); err != nil {
					keyRecord.revocation = nil
					keyRecord.revocationBody = nil
					changed = true
				}
			}
			if keyRecord.record == nil && keyRecord.revocation == nil {
				self.deleteKeyRecordWithLock(keyHex)
				// the addresses that record produced become unverified rather
				// than disappearing: the local evidence about them is still
				// evidence, and a later record can claim them again
				for _, address := range self.ipAddresses {
					if address.publicKeyHex == keyHex {
						address.publicKeyHex = ""
					}
				}
			}
		}
		if changed {
			self.tierRebuildWithLock(self.settings.Now())
			self.changedWithLock()
		}
	}()
}

// The accepted root keys.
func (self *ExtenderDirectory) RootKeys() *ExtenderRootKeySet {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.rootKeySet
}

// The root keys a message is verified under and the reset it is verified in,
// read together, so the apply can tell whether a reset landed in between
// (applyVerifiedRecord).
func (self *ExtenderDirectory) rootKeysAndResetVersion() (*ExtenderRootKeySet, uint64) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.rootKeySet, self.resetVersion
}

// Runs the test seam between a verification and its apply, when one is set.
func (self *ExtenderDirectory) verified() {
	if self.applyVerifiedHook != nil {
		self.applyVerifiedHook()
	}
}

// Applies one signed gossip message, verifying it under the current root keys
// and the allowed network hosts. Records applied in this phase arrive over the
// feed; the gossip node of phase 5a applies with its own source.
func (self *ExtenderDirectory) Apply(message *protocol.ExtenderGossipMessage) (changed bool, err error) {
	return self.ApplySource(message, ExtenderSourceFeed)
}

func (self *ExtenderDirectory) ApplySource(
	message *protocol.ExtenderGossipMessage,
	source string,
) (changed bool, err error) {
	if message == nil {
		return false, fmt.Errorf("extender gossip message is missing")
	}
	switch {
	case message.GetRecord() != nil:
		return self.ApplyRecord(message.GetRecord(), source)
	case message.GetRevocation() != nil:
		return self.ApplyRevocationSource(message.GetRevocation(), source)
	default:
		return false, fmt.Errorf("extender gossip message carries neither a record nor a revocation")
	}
}

// Applies one signed record. A record older than the one already held for the
// key changes nothing, which is what makes the newest-wins rule of B5 order
// independent. A record new to a directory at MaxActiveRecordCount evicts one
// it prefers less -- a random one off the hinted continent and unmeasured,
// possibly this one -- and is published to the subscribers either way.
func (self *ExtenderDirectory) ApplyRecord(
	record *protocol.ExtenderRecord,
	source string,
) (changed bool, err error) {
	keySet, resetVersion := self.rootKeysAndResetVersion()
	body, err := keySet.VerifyRecord(record)
	if err != nil {
		return false, err
	}
	if !ExtenderNetworkHostAllowed(body.NetworkHost, self.settings.NetworkHosts...) {
		return false, fmt.Errorf("extender record is for network host %q", body.NetworkHost)
	}
	if len(body.PublicKey) == 0 {
		return false, fmt.Errorf("extender record carries no public key")
	}
	self.verified()
	return self.applyVerifiedRecord(record, body, keySet, resetVersion, source)
}

// Applies one record whose body has been verified under keySet and whose
// network host is allowed: what ApplyRecord does once the signature holds.
// Past MaxActiveRecordCount a record new to the directory evicts one the
// directory prefers less, which may be this one; the record is published to
// the subscribers either way, whose own caps judge it.
//
// The verification runs outside the lock, so SetRootKeys can replace the keys
// between it and the store, after judging every record it held. A record
// verified under keys that are no longer in force is judged again under the
// lock, so it never lands after them. Keys change rarely, so the second
// verification is rarely paid. A record verified before a reset (`resetVersion`
// is not the directory's) is dropped: it was taken from what the reset
// cleared -- a stream or a mesh the owner is replacing -- and landing it would
// leave the reset incomplete.
func (self *ExtenderDirectory) applyVerifiedRecord(
	record *protocol.ExtenderRecord,
	body *protocol.ExtenderRecordBody,
	keySet *ExtenderRootKeySet,
	resetVersion uint64,
	source string,
) (changed bool, err error) {
	if source == "" {
		source = ExtenderSourceFeed
	}
	keyHex := hex.EncodeToString(body.PublicKey)
	// the addresses are parsed before the lock is taken, each once
	ips := make([]netip.Addr, 0, len(body.Addresses))
	for _, recordAddress := range body.Addresses {
		ip, parseErr := netip.ParseAddr(recordAddress.Ip)
		if parseErr != nil || !ip.IsValid() {
			continue
		}
		if ip = ip.Unmap(); !slices.Contains(ips, ip) {
			ips = append(ips, ip)
		}
	}
	now := self.settings.Now()

	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	if resetVersion != self.resetVersion {
		return false, errExtenderDirectoryReset
	}
	if keySet != self.rootKeySet {
		if _, err := self.rootKeySet.VerifyRecord(record); err != nil {
			return false, err
		}
	}
	keyRecord := self.keyHexRecords[keyHex]
	if keyRecord == nil {
		// the body is kept as the record's for as long as the key is, so its
		// key is the record's own
		keyRecord = &extenderDirectoryRecord{
			publicKey: body.PublicKey,
		}
		self.keyHexRecords[keyHex] = keyRecord
	} else if keyRecord.recordBody != nil && body.IssueTimeMs <= keyRecord.recordBody.IssueTimeMs {
		// an older or identical record; the newest one already held wins
		return false, nil
	}
	keyRecord.record = record
	keyRecord.recordBody = body
	keyRecord.applySerial = self.nextApplySerial
	self.nextApplySerial += 1
	if keyRecord.ips == nil {
		// a key new to the directory takes the parsed addresses as they are,
		// and the loop below finds each one already listed
		keyRecord.ips = ips
	}

	for _, ip := range ips {
		address := self.ipAddresses[ip]
		if address == nil {
			address = &extenderDirectoryAddress{
				ip:      ip,
				source:  source,
				addTime: now,
			}
			self.ipAddresses[ip] = address
		}
		// an unverified bootstrap address upgrades here, keeping the local
		// evidence it collected before any record named it; an address
		// another key held moves to this one
		previousKeyHex := address.publicKeyHex
		address.publicKeyHex = keyHex
		if previousKeyHex != "" && previousKeyHex != keyHex {
			self.tierUpdateWithLock(previousKeyHex, now)
		}
		if !slices.Contains(keyRecord.ips, ip) {
			keyRecord.ips = append(keyRecord.ips, ip)
		}
	}
	self.tierUpdateWithLock(keyHex, now)
	self.enforceActiveRecordCapWithLock(now)
	self.enforceAddressCapWithLock(now)
	self.noteEventWithLock(source, now)
	// the message is built only for a subscriber, since a directory fed a
	// record a millisecond pays for everything it builds per record. A gated
	// record goes to no subscriber whatever source it came from (Q1): the
	// subscribers are the feed stream and the mesh, the open channels, and a
	// durable record that reached them once would be enumerable from then on
	if 0 < len(self.subscriptions) && ExtenderRecordOpen(body) {
		self.publishWithLock(&protocol.ExtenderGossipMessage{
			Message: &protocol.ExtenderGossipMessage_Record{Record: record},
		})
	}
	self.changedWithLock()
	return true, nil
}

// Applies one signed revocation that arrived over the feed, which is what
// every caller that does not name its source is.
func (self *ExtenderDirectory) ApplyRevocation(
	revocation *protocol.ExtenderRevocation,
) (changed bool, err error) {
	return self.ApplyRevocationSource(revocation, ExtenderSourceFeed)
}

// Applies one signed revocation. A revocation with an issue time at or after
// the held record's issue time makes the key inactive at once (B5). The source
// decides only whether the apply counts as a network event (K4).
func (self *ExtenderDirectory) ApplyRevocationSource(
	revocation *protocol.ExtenderRevocation,
	source string,
) (changed bool, err error) {
	keySet, resetVersion := self.rootKeysAndResetVersion()
	body, err := keySet.VerifyRevocation(revocation)
	if err != nil {
		return false, err
	}
	if !ExtenderNetworkHostAllowed(body.NetworkHost, self.settings.NetworkHosts...) {
		return false, fmt.Errorf("extender revocation is for network host %q", body.NetworkHost)
	}
	if len(body.PublicKey) == 0 {
		return false, fmt.Errorf("extender revocation carries no public key")
	}
	self.verified()
	return self.applyVerifiedRevocation(revocation, body, keySet, resetVersion, source)
}

// Applies one revocation whose body has been verified under keySet and whose
// network host is allowed: what ApplyRevocationSource does once the signature
// holds. A revocation verified under keys that SetRootKeys has replaced since
// is judged again under the lock, and one verified before a reset is dropped,
// as a record is (applyVerifiedRecord).
func (self *ExtenderDirectory) applyVerifiedRevocation(
	revocation *protocol.ExtenderRevocation,
	body *protocol.ExtenderRevocationBody,
	keySet *ExtenderRootKeySet,
	resetVersion uint64,
	source string,
) (changed bool, err error) {
	keyHex := hex.EncodeToString(body.PublicKey)
	now := self.settings.Now()

	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	if resetVersion != self.resetVersion {
		return false, errExtenderDirectoryReset
	}
	if keySet != self.rootKeySet {
		if _, err := self.rootKeySet.VerifyRevocation(revocation); err != nil {
			return false, err
		}
	}
	keyRecord := self.keyHexRecords[keyHex]
	if keyRecord == nil {
		// keep the revocation even with no record, so a replayed older record
		// cannot reactivate the key
		keyRecord = &extenderDirectoryRecord{
			publicKey: slices.Clone(body.PublicKey),
		}
		self.keyHexRecords[keyHex] = keyRecord
	} else if keyRecord.revocationBody != nil && body.IssueTimeMs <= keyRecord.revocationBody.IssueTimeMs {
		return false, nil
	}
	keyRecord.revocation = revocation
	keyRecord.revocationBody = body
	self.tierUpdateWithLock(keyHex, now)
	self.noteEventWithLock(source, now)
	if 0 < len(self.subscriptions) {
		self.publishWithLock(&protocol.ExtenderGossipMessage{
			Message: &protocol.ExtenderGossipMessage_Revocation{Revocation: revocation},
		})
	}
	self.changedWithLock()
	return true, nil
}

// Subscribe returns the applied signed messages as they land, and the
// unsubscribe that releases the subscription (D4). The channel is bounded:
// when a consumer falls behind it is closed rather than waited on, so one slow
// feed subscriber can never stall an apply. A consumer therefore treats a
// closed channel as a disconnect, not as an orderly end of stream.
//
// The unsubscribe is idempotent and safe to call after the channel has been
// closed by an overflow.
func (self *ExtenderDirectory) Subscribe() (<-chan *protocol.ExtenderGossipMessage, func()) {
	subscription := &extenderDirectorySubscription{
		messages: make(chan *protocol.ExtenderGossipMessage, ExtenderDirectorySubscribeBufferCount),
	}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.subscriptions[subscription] = true
	}()
	unsubscribe := func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		delete(self.subscriptions, subscription)
		if !subscription.closed {
			subscription.closed = true
			close(subscription.messages)
		}
	}
	return subscription.messages, unsubscribe
}

// Hands one applied message to every subscriber with a zero-wait send. This
// runs under the state lock because a buffered send that never waits cannot
// deadlock and cannot re-enter the directory, and doing it here keeps the
// delivery in the same critical section as the apply, so no subscriber can see
// two applies out of order.
func (self *ExtenderDirectory) publishWithLock(message *protocol.ExtenderGossipMessage) {
	for subscription := range self.subscriptions {
		select {
		case subscription.messages <- message:
		default:
			// The consumer fell behind its whole buffer. What it has queued is
			// already an incomplete view -- this message is missing from it --
			// so the queue is voided and the channel closed, which its
			// consumer reads as a disconnect at once rather than after
			// flushing a buffer of records that no longer describe the
			// directory.
			for draining := true; draining; {
				select {
				case <-subscription.messages:
				default:
					draining = false
				}
			}
			subscription.closed = true
			close(subscription.messages)
			delete(self.subscriptions, subscription)
		}
	}
}

// SampleRecords returns up to `count` signed records of the open tier for one
// vantage (D4, Q2), with the record of `ownPublicKey` first when the directory
// holds one and it is open. The rest are the vantage's feed partition
// (ExtenderPartitionMembers), in the order of the current epoch
// (ExtenderPartitionOrder) interleaved by family, so a vantage that polls
// forever sees its partition and no more, and two polls in one epoch see the
// same sample. A gated record is never sampled, whatever source it arrived
// from (Q1). The messages carry the record exactly as it was received, so a
// relayed sample is still verifiable under the root keys. `vantage` is the
// caller's (ExtenderVantageKeyOfAddr); nil is a vantage of its own.
func (self *ExtenderDirectory) SampleRecords(
	count int,
	ownPublicKey []byte,
	vantage []byte,
) []*protocol.ExtenderGossipMessage {
	if count <= 0 {
		return []*protocol.ExtenderGossipMessage{}
	}
	ownKeyHex := hex.EncodeToString(ownPublicKey)
	now := self.settings.Now()
	epoch := ExtenderEpoch(now, self.settings.OpenEpochTimeout)

	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	var ownRecord *protocol.ExtenderRecord
	keyHexes := []string{}
	keyHexRecords := map[string]*protocol.ExtenderRecord{}
	recordBodies := map[*protocol.ExtenderRecord]*protocol.ExtenderRecordBody{}
	for _, keyHex := range self.openKeyHexesWithLock(now) {
		keyRecord := self.keyHexRecords[keyHex]
		if 0 < len(ownPublicKey) && keyHex == ownKeyHex {
			ownRecord = keyRecord.record
		}
		keyHexes = append(keyHexes, keyHex)
		keyHexRecords[keyHex] = keyRecord.record
		recordBodies[keyRecord.record] = keyRecord.recordBody
	}
	members, _, _ := ExtenderPartitionMembers(self.partitionSecret, ExtenderChannelFeed, vantage, keyHexes)
	records := []*protocol.ExtenderRecord{}
	for _, keyHex := range ExtenderPartitionOrder(self.partitionSecret, ExtenderChannelFeed, vantage, epoch, members) {
		if keyHex == ownKeyHex {
			continue
		}
		records = append(records, keyHexRecords[keyHex])
	}
	records = balanceRecordsByIpFamily(records, recordBodies)
	if ownRecord != nil {
		records = append([]*protocol.ExtenderRecord{ownRecord}, records...)
	}

	messages := []*protocol.ExtenderGossipMessage{}
	for _, record := range records {
		if count <= len(messages) {
			break
		}
		messages = append(messages, &protocol.ExtenderGossipMessage{
			Message: &protocol.ExtenderGossipMessage_Record{Record: record},
		})
	}
	return messages
}

// The keys of the open tier the feed may serve (Q1, Q2): every held record
// that is not revoked, not expired and not gated, in key order. The order is
// what makes the partitions reproducible whatever order the map hands the
// keys in.
func (self *ExtenderDirectory) openKeyHexesWithLock(now time.Time) []string {
	keyHexes := []string{}
	for keyHex, keyRecord := range self.keyHexRecords {
		if keyRecord.record == nil || keyRecord.recordBody == nil {
			continue
		}
		if self.keyRecordRevokedWithLock(keyRecord) || self.keyRecordExpiredWithLock(keyRecord, now) {
			continue
		}
		if !ExtenderRecordOpen(keyRecord.recordBody) {
			continue
		}
		keyHexes = append(keyHexes, keyHex)
	}
	slices.Sort(keyHexes)
	return keyHexes
}

// Whether the feed may stream the record of `publicKey` to a subscriber at
// `vantage` (Q2): it is in the open tier and in the vantage's feed partition,
// as SampleRecords would place it now. A record outside the partition is as
// unseen on the stream as it is in the sample, so a subscriber that stays
// connected through a whole drip rotation still learns its partition and no
// more. A key the directory does not hold, or holds revoked, expired or
// gated, is not streamed.
func (self *ExtenderDirectory) OpenPartitionContains(vantage []byte, publicKey []byte) bool {
	keyHex := hex.EncodeToString(publicKey)
	now := self.settings.Now()

	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	keyHexes := self.openKeyHexesWithLock(now)
	if !slices.Contains(keyHexes, keyHex) {
		return false
	}
	members, _, _ := ExtenderPartitionMembers(self.partitionSecret, ExtenderChannelFeed, vantage, keyHexes)
	return slices.Contains(members, keyHex)
}

// balanceRecordsByIpFamily orders records so that taking a prefix of any length
// yields as close to an equal number of v4-reachable and v6-reachable extenders
// as the directory can supply, keeping the relative order the caller gave
// within each family -- the epoch's keyed order (SampleRecords), so the result
// is as deterministic as its input.
//
// A plain cut does not do this. A directory that is mostly v4 -- which is the
// normal case, since v4 addresses are easier to come by -- hands a v6-only
// client a sample it cannot dial, and the client has no way to ask for more.
//
// Reachability, not exclusivity: a dual-stack extender is in both buckets and
// can satisfy either side of the interleave. That is deliberate. The point of
// the balance is that a client of either family finds something it can reach,
// and a dual-stack extender serves both, so it should never be held back in
// favour of a single-family one.
//
// The interleave starts with v6 because it is the scarcer family: when the
// count is odd, the extra slot goes to the side more likely to be short.
func balanceRecordsByIpFamily(
	records []*protocol.ExtenderRecord,
	recordBodies map[*protocol.ExtenderRecord]*protocol.ExtenderRecordBody,
) []*protocol.ExtenderRecord {
	if len(records) <= 1 {
		return records
	}

	ipv4Capable := []*protocol.ExtenderRecord{}
	ipv6Capable := []*protocol.ExtenderRecord{}
	unreachable := []*protocol.ExtenderRecord{}
	for _, record := range records {
		hasIpv4, hasIpv6 := recordIpFamilies(recordBodies[record])
		if hasIpv4 {
			ipv4Capable = append(ipv4Capable, record)
		}
		if hasIpv6 {
			ipv6Capable = append(ipv6Capable, record)
		}
		if !hasIpv4 && !hasIpv6 {
			// No usable address. Kept rather than dropped, so what the caller
			// reports as available does not change -- but held in its own list
			// so it cannot displace a record a client could actually dial.
			unreachable = append(unreachable, record)
		}
	}

	balanced := make([]*protocol.ExtenderRecord, 0, len(records))
	taken := map[*protocol.ExtenderRecord]bool{}
	take := func(pool []*protocol.ExtenderRecord, from int) int {
		for i := from; i < len(pool); i += 1 {
			if taken[pool[i]] {
				continue
			}
			taken[pool[i]] = true
			balanced = append(balanced, pool[i])
			return i + 1
		}
		return len(pool)
	}
	ipv4Next, ipv6Next := 0, 0
	for len(balanced) < len(records) {
		before := len(balanced)
		ipv6Next = take(ipv6Capable, ipv6Next)
		ipv4Next = take(ipv4Capable, ipv4Next)
		if len(balanced) == before {
			// both pools are exhausted of un-taken records
			break
		}
	}
	return append(balanced, unreachable...)
}

// recordIpFamilies reports which families a record lists an address for. A
// record that fails to name any parseable address is reachable over neither.
func recordIpFamilies(body *protocol.ExtenderRecordBody) (hasIpv4 bool, hasIpv6 bool) {
	if body == nil {
		return false, false
	}
	for _, recordAddress := range body.Addresses {
		ip, err := netip.ParseAddr(recordAddress.Ip)
		if err != nil {
			continue
		}
		if ip.Unmap().Is4() {
			hasIpv4 = true
		} else {
			hasIpv6 = true
		}
	}
	return hasIpv4, hasIpv6
}

// Adds an address learned outside the signed path: a dns bootstrap answer or a
// manually configured extender. It is unverified until a record lists it. An
// address that is already known keeps everything it has.
func (self *ExtenderDirectory) AddBootstrap(ip netip.Addr, source string) (changed bool) {
	if !ip.IsValid() {
		return false
	}
	ip = ip.Unmap()
	if source == "" {
		source = ExtenderSourceBootstrap
	}
	now := self.settings.Now()

	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	if _, ok := self.ipAddresses[ip]; ok {
		return false
	}
	self.ipAddresses[ip] = &extenderDirectoryAddress{
		ip:      ip,
		source:  source,
		addTime: now,
	}
	self.enforceAddressCapWithLock(now)
	self.changedWithLock()
	return true
}

// Adds or promotes an address configured by hand (K6). An address already
// known from another source keeps every piece of local evidence it has
// collected and becomes manual, which is what takes it out of the removal
// policy: a hand-configured extender is only removed by a reconfiguration.
func (self *ExtenderDirectory) AddManual(ip netip.Addr) (changed bool) {
	if !ip.IsValid() {
		return false
	}
	ip = ip.Unmap()
	now := self.settings.Now()

	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	if address, ok := self.ipAddresses[ip]; ok {
		if address.source == ExtenderSourceManual {
			return false
		}
		address.source = ExtenderSourceManual
		// a record with a manual address is kept by the active cap
		self.tierUpdateAddressWithLock(address, now)
		self.changedWithLock()
		return true
	}
	self.ipAddresses[ip] = &extenderDirectoryAddress{
		ip:      ip,
		source:  ExtenderSourceManual,
		addTime: now,
	}
	self.enforceAddressCapWithLock(now)
	self.changedWithLock()
	return true
}

// Records a completed dial over `connectMode`. A success clears the hold and
// the consecutive failure run.
func (self *ExtenderDirectory) RecordSuccess(ip netip.Addr, connectMode ExtenderConnectMode) {
	if !ip.IsValid() {
		return
	}
	ip = ip.Unmap()
	now := self.settings.Now()

	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	address := self.ipAddresses[ip]
	if address == nil {
		return
	}
	address.successCount += 1
	address.lastSuccessTime = now
	address.lastUseTime = now
	address.consecutiveFailureCount = 0
	address.firstFailureTime = time.Time{}
	address.holdUntilTime = time.Time{}
	if self.log.V(2).Enabled() {
		self.log.Infof("[extender]success %s %s\n", ip, connectMode)
	}
	self.tierUpdateAddressWithLock(address, now)
	self.changedWithLock()
}

// Records a failed dial over `connectMode`: the address is held for the
// doubling hold timeout and removed when the removal policy is met.
func (self *ExtenderDirectory) RecordFailure(ip netip.Addr, connectMode ExtenderConnectMode) {
	if !ip.IsValid() {
		return
	}
	ip = ip.Unmap()
	now := self.settings.Now()

	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	address := self.ipAddresses[ip]
	if address == nil {
		return
	}
	address.failureCount += 1
	address.consecutiveFailureCount += 1
	address.lastFailureTime = now
	address.lastUseTime = now
	if address.firstFailureTime.IsZero() {
		address.firstFailureTime = now
	}
	address.holdUntilTime = now.Add(self.holdTimeout(address.consecutiveFailureCount))
	if self.log.V(2).Enabled() {
		self.log.Infof("[extender]failure %s %s consecutive=%d\n", ip, connectMode, address.consecutiveFailureCount)
	}
	if self.shouldRemoveWithLock(address, now) {
		delete(self.ipAddresses, ip)
		self.pruneKeyRecordsWithLock()
	}
	self.tierUpdateAddressWithLock(address, now)
	self.changedWithLock()
}

// Records a 429 from an address (A12): it is left alone until the Retry-After
// it answered with, or ExtenderLimitedBackoff when it gave none, jittered by
// +-50 % and never past MaxHoldTimeout. A limit is not a failure: no failure
// count, no hold, nothing toward removal. A later backoff is never shortened
// by an earlier one still in force; one that has passed by either clock is
// replaced, even when the monotonic clock alone, stopped while the host slept,
// would still place it after the new one. Returns when the address stops being
// limited, zero for an address the directory does not know, which it records
// nothing for.
func (self *ExtenderDirectory) RecordLimited(ip netip.Addr, retryAfter time.Duration) time.Time {
	if !ip.IsValid() {
		return time.Time{}
	}
	ip = ip.Unmap()
	now := self.settings.Now()
	backoff := min(
		JitterExtenderLimitedBackoff(retryAfter, self.settings.ExtenderLimitedBackoff),
		self.settings.MaxHoldTimeout,
	)

	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	address := self.ipAddresses[ip]
	if address == nil {
		return time.Time{}
	}
	address.lastUseTime = now
	if limitedUntilTime := now.Add(backoff); !extenderBefore(now, address.limitedUntilTime) ||
		address.limitedUntilTime.Before(limitedUntilTime) {
		address.limitedUntilTime = limitedUntilTime
	}
	if self.log.V(2).Enabled() {
		self.log.Infof("[extender]limited %s until %s\n", ip, address.limitedUntilTime)
	}
	self.changedWithLock()
	return address.limitedUntilTime
}

// When one address stops being limited, zero when it is not limited now or
// is not known (A12).
func (self *ExtenderDirectory) AddressLimitedUntil(ip netip.Addr) time.Time {
	if !ip.IsValid() {
		return time.Time{}
	}
	ip = ip.Unmap()
	now := self.settings.Now()

	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	address := self.ipAddresses[ip]
	if address == nil || !extenderBefore(now, address.limitedUntilTime) {
		return time.Time{}
	}
	return address.limitedUntilTime
}

// Adjusts the in-use count of one address, which the status reports. A
// positive delta also stamps the last use. The count is kept by ip whether or
// not the address is known, since a connection outlives a reset that dropped
// its address (Reset): its release balances its hold, and an address learned
// again while it lives is reported in use.
func (self *ExtenderDirectory) SetInUse(ip netip.Addr, delta int) {
	if !ip.IsValid() || delta == 0 {
		return
	}
	ip = ip.Unmap()
	now := self.settings.Now()

	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	if inUseCount := max(0, self.ipInUseCounts[ip]+delta); 0 < inUseCount {
		self.ipInUseCounts[ip] = inUseCount
	} else {
		delete(self.ipInUseCounts, ip)
	}
	address := self.ipAddresses[ip]
	if address == nil {
		// nothing the status shows has changed
		return
	}
	if 0 < delta {
		address.lastUseTime = now
	}
	self.changedWithLock()
}

// SetContinentHint sets the continent the candidate order prefers
// (DESIGNNOTES4.md §4): the operator's hint, or the one inferred from the dns
// bootstrap. Upper case; empty clears it. Reports whether it changed.
func (self *ExtenderDirectory) SetContinentHint(continentCode string) (changed bool) {
	continentCode = strings.ToUpper(strings.TrimSpace(continentCode))

	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	if self.continentHint == continentCode {
		return false
	}
	self.continentHint = continentCode
	// every record's preference follows the hint
	self.tierRebuildWithLock(self.settings.Now())
	self.changedWithLock()
	return true
}

// The continent the candidate order prefers, empty when nothing has said.
func (self *ExtenderDirectory) ContinentHint() string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.continentHint
}

// Records the country the operator's hint placed this client in, current
// until the next failed hint or path change (ExpireCountryHint). Every answer
// renews the country and its time, which the directory stores, so the next
// start has it (CountryHintMaxAge). An empty answer is an operator that could
// not place the client, or one that predates the country: it leaves the last
// country in place, stale, and does not renew it. Reports whether the country
// changed.
func (self *ExtenderDirectory) SetCountryHint(countryCode string) (changed bool) {
	countryCode = NormalizeSpoofCountryCode(countryCode)
	// the wall clock alone, as the store keeps it: the monotonic clock stops
	// while the host sleeps, and a journey slept through must count
	now := self.settings.Now().Round(0)

	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	if countryCode == "" {
		self.countryHintCurrent = false
		return false
	}
	changed = self.countryHint != countryCode
	self.countryHint = countryCode
	self.countryHintTime = now
	self.countryHintCurrent = true
	// the renewed time is saved whether or not the country changed
	self.changedWithLock()
	return changed
}

// Makes the operator's last country stale: the hint failed, or the path
// changed and the answer placed the address of the old one. Until the operator
// answers again, the network country the host reports stands in for it, and on
// a host that reports none, the last country itself while it is within
// CountryHintMaxAge (SpoofCountryCode).
func (self *ExtenderDirectory) ExpireCountryHint() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.countryHintCurrent = false
}

// The country the extender dials through this directory draw their outer
// names for (SpoofDomainsForCountry), lower case, empty for the global list:
// the operator's country while its hint is current, else the network country
// the host reports (SetNetworkCountryCode), else the operator's last country
// until CountryHintMaxAge has passed since the operator last answered it,
// which a restart keeps, since the country is stored. The host's report is
// the fallback rather than the rule because the operator placed this client's
// own address; but when the operator cannot be asked -- on a whitelist-only
// mobile network no operator address is routable -- the host's report is the
// only current one there is, and on a host that reports none, the operator's
// last answer is all there is.
func (self *ExtenderDirectory) SpoofCountryCode() string {
	now := self.settings.Now()

	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	if self.countryHintCurrent {
		return self.countryHint
	}
	if networkCountryCode := NetworkCountryCode(); networkCountryCode != "" {
		return networkCountryCode
	}
	if self.countryHintWithinMaxAge(self.countryHintTime, now) {
		return self.countryHint
	}
	return ""
}

// Whether the operator's last country, answered at `countryHintTime`, may still
// stand in at `now`: younger than CountryHintMaxAge by the wall clock. A time
// ahead of the clock cannot be aged -- the clock moved back, or the store is
// not what this directory wrote -- so it is not used. It reads only its
// arguments and the settings, so the state lock is neither needed nor taken,
// as in holdTimeout.
func (self *ExtenderDirectory) countryHintWithinMaxAge(countryHintTime time.Time, now time.Time) bool {
	if countryHintTime.IsZero() {
		return false
	}
	age := now.Sub(countryHintTime)
	return 0 <= age && age < self.settings.CountryHintMaxAge
}

// The spoof list the dials through one directory draw from, and the country
// whose list it is (spoofDomainsForCountry). A nil directory places nothing
// and draws from the global list.
func directorySpoofDomains(directory *ExtenderDirectory) ([]string, string) {
	countryCode := ""
	if directory != nil {
		countryCode = directory.SpoofCountryCode()
	}
	return spoofDomainsForCountry(countryCode)
}

// RecordLatency stores the outcome of one probe pass over an address: the
// lowest rtt it measured and whether the target co-signed a claim of that
// pass (DESIGNNOTES4.md, GEOMAP §2.3) -- a claim merely sent, refused or left
// without a verdict does not count. The sample is per process and ages out
// after LatencyMaxAge, the time the host slept included; it is never stored,
// because yesterday's path is not today's. It replaces the address's last
// sample, which is how one due a refresh after a path change or a resume is
// replaced (RefreshLatencies).
func (self *ExtenderDirectory) RecordLatency(ip netip.Addr, rtt time.Duration, attested bool) {
	if !ip.IsValid() || rtt <= 0 {
		return
	}
	ip = ip.Unmap()
	now := self.settings.Now()

	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	address := self.ipAddresses[ip]
	if address == nil {
		return
	}
	address.latency = rtt
	address.latencyTime = now
	address.latencyAttested = attested
	address.latencyRefreshDue = false
	address.probeCount += 1
	if self.log.V(2).Enabled() {
		self.log.Infof("[extender]latency %s %s attested=%t\n", ip, rtt, attested)
	}
	// a measured record is preferred by the active cap
	self.tierUpdateAddressWithLock(address, now)
	self.changedWithLock()
}

// Makes every latency sample due a refresh: the path changed, and each one
// measured the old path (DESIGNNOTES4.md §6). A sample due a refresh stays in
// use -- the candidate order still ranks by it and the active cap still counts
// its record measured -- until a probe measures the address again and the new
// sample replaces it (RecordLatency), so a path change leaves no time with no
// samples. A probe pass measures the samples due a refresh first and does not
// count them toward its window (ProbeCandidates, probeWindowLatencies), so the
// pass that follows the first sample on the new path measures them again. The
// rest of the local evidence -- successes, failures, holds and limits -- stays
// too. Nothing the order, the status or the store reads moves, so no change is
// published.
func (self *ExtenderDirectory) RefreshLatencies() {
	self.refreshLatencies(func(time.Time, time.Time) bool {
		return true
	})
}

// Makes due a refresh every latency sample the host has slept at least
// `minSleep` through since it was taken (hostSlept): the host resumed from a
// sleep that long, and may have woken where it measured nothing, so a sample
// from before it is measured again as one of another path is
// (RefreshLatencies), and stays in use until then. A sample taken since the
// host woke is not due. A wall clock set forward reads as a sleep here, and
// one set back hides as much sleep. A `minSleep` <= 0 makes nothing due.
func (self *ExtenderDirectory) RefreshSleptLatencies(minSleep time.Duration) {
	if minSleep <= 0 {
		return
	}
	self.refreshLatencies(func(latencyTime time.Time, now time.Time) bool {
		return minSleep <= hostSlept(now, latencyTime)
	})
}

// Makes due a refresh each latency sample `due` picks by when it was taken and
// the time now. Every sample stays where it is.
func (self *ExtenderDirectory) refreshLatencies(due func(latencyTime time.Time, now time.Time) bool) {
	now := self.settings.Now()

	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	for _, address := range self.ipAddresses {
		if address.latencyTime.IsZero() || !due(address.latencyTime, now) {
			continue
		}
		address.latencyRefreshDue = true
	}
}

// The hold after `consecutiveFailureCount` failures: the base doubling per
// failure, capped. The shift is bounded before it is taken, so a long run
// cannot overflow into a negative duration.
func (self *ExtenderDirectory) holdTimeout(consecutiveFailureCount int) time.Duration {
	if consecutiveFailureCount <= 1 {
		return min(self.settings.HoldTimeout, self.settings.MaxHoldTimeout)
	}
	holdTimeout := self.settings.HoldTimeout
	for i := 1; i < consecutiveFailureCount; i += 1 {
		if self.settings.MaxHoldTimeout <= holdTimeout {
			return self.settings.MaxHoldTimeout
		}
		holdTimeout *= 2
	}
	return min(holdTimeout, self.settings.MaxHoldTimeout)
}

// Applies the removal policy, the expired retention and the address cap. The
// network client calls it on its refresh tick, so a directory that is only
// read still ages.
func (self *ExtenderDirectory) Expire(now time.Time) (changed bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	for ip, address := range self.ipAddresses {
		if self.shouldRemoveWithLock(address, now) {
			delete(self.ipAddresses, ip)
			changed = true
		}
	}
	if self.evictExpiredWithLock(now) {
		changed = true
	}
	if self.enforceAddressCapWithLock(now) {
		changed = true
	}
	if self.pruneKeyRecordsWithLock() {
		changed = true
	}
	// the tick is also where the index is rebuilt from scratch, whatever it
	// has missed, and the active cap met again
	self.tierRebuildWithLock(now)
	if self.enforceActiveRecordCapWithLock(now) {
		changed = true
	}
	if changed {
		self.changedWithLock()
	}
	return changed
}

// Up to `count` dialable endpoints of `ipVersion` (0 for any family), active
// and not held, verified first. The order is deterministic -- verified, then
// the proximity order of DESIGNNOTES4.md §4 (the hinted continent first, then
// measured latency ascending with unmeasured addresses last), then fewest
// consecutive failures, then the most recent success, then the address --
// because the strategy does its own weighting on top and a stable order makes
// the policy testable. With no hint and no samples it is the order it always
// was.
//
// After every active address and every manual one comes the last tier: the
// unheld addresses of the retained expired identities (MaxExpiredRecordCount),
// the newest expiry first and then the same order, so a client that has lost
// every current path can still try the extenders it last knew. They fill only
// what `count` leaves.
func (self *ExtenderDirectory) Candidates(
	ipVersion int,
	count int,
	exclude ...netip.Addr,
) []*ExtenderCandidate {
	return self.candidates(ipVersion, count, false, exclude)
}

// The candidates of `Candidates` that a signed record verifies, in the same
// order: a manual address no record verifies, the one unverified kind
// `Candidates` offers, is left out. A strategy that refuses extenders
// configured by hand (`ClientStrategySettings.DisableManualExtenders`) draws
// from here.
func (self *ExtenderDirectory) VerifiedCandidates(
	ipVersion int,
	count int,
	exclude ...netip.Addr,
) []*ExtenderCandidate {
	return self.candidates(ipVersion, count, true, exclude)
}

// The candidates of `Candidates`, or of `VerifiedCandidates` with
// `verifiedOnly`.
func (self *ExtenderDirectory) candidates(
	ipVersion int,
	count int,
	verifiedOnly bool,
	exclude []netip.Addr,
) []*ExtenderCandidate {
	if count <= 0 {
		return []*ExtenderCandidate{}
	}
	excludeIps := map[netip.Addr]bool{}
	for _, ip := range exclude {
		excludeIps[ip.Unmap()] = true
	}
	now := self.settings.Now()

	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	compareUsable := func(a *extenderDirectoryAddress, b *extenderDirectoryAddress) int {
		if c := compareExtenderVerified(a, b); c != 0 {
			return c
		}
		return self.compareCandidateWithLock(a, b, now)
	}
	compareExpired := func(a *extenderDirectoryAddress, b *extenderDirectoryAddress) int {
		if c := compareExpiredKeyRecords(
			a.publicKeyHex,
			self.keyHexRecords[a.publicKeyHex],
			b.publicKeyHex,
			self.keyHexRecords[b.publicKeyHex],
		); c != 0 {
			return c
		}
		return self.compareCandidateWithLock(a, b, now)
	}
	// a limited address comes after every healthy one, whatever its latency
	// or continent, and the one whose backoff ends first leads them (A12)
	compareLimited := func(compare func(a *extenderDirectoryAddress, b *extenderDirectoryAddress) int) func(a *extenderDirectoryAddress, b *extenderDirectoryAddress) int {
		return func(a *extenderDirectoryAddress, b *extenderDirectoryAddress) int {
			if c := a.limitedUntilTime.Compare(b.limitedUntilTime); c != 0 {
				return c
			}
			return compare(a, b)
		}
	}
	usable := self.usableAddressesWithLock(ipVersion, now, excludeIps)
	if verifiedOnly {
		// the retained expired addresses below are verified by construction
		usable = slices.DeleteFunc(usable, func(address *extenderDirectoryAddress) bool {
			return address.publicKeyHex == ""
		})
	}
	usableAddresses, limitedUsableAddresses := splitExtenderLimitedAddresses(usable, now)
	expiredAddresses, limitedExpiredAddresses := splitExtenderLimitedAddresses(
		self.retainedExpiredAddressesWithLock(ipVersion, now, excludeIps),
		now,
	)
	tiers := []struct {
		addresses []*extenderDirectoryAddress
		compare   func(a *extenderDirectoryAddress, b *extenderDirectoryAddress) int
	}{
		{addresses: usableAddresses, compare: compareUsable},
		{addresses: expiredAddresses, compare: compareExpired},
		{addresses: limitedUsableAddresses, compare: compareLimited(compareUsable)},
		{addresses: limitedExpiredAddresses, compare: compareLimited(compareExpired)},
	}

	candidates := []*ExtenderCandidate{}
	for _, tier := range tiers {
		slices.SortFunc(tier.addresses, tier.compare)
		for _, address := range tier.addresses {
			if count <= len(candidates) {
				return candidates
			}
			candidates = append(candidates, self.candidateWithLock(address, now))
		}
	}
	return candidates
}

// The addresses that are not limited now, and those that are (A12), each in
// the order given.
func splitExtenderLimitedAddresses(
	addresses []*extenderDirectoryAddress,
	now time.Time,
) ([]*extenderDirectoryAddress, []*extenderDirectoryAddress) {
	healthyAddresses := []*extenderDirectoryAddress{}
	limitedAddresses := []*extenderDirectoryAddress{}
	for _, address := range addresses {
		if extenderBefore(now, address.limitedUntilTime) {
			limitedAddresses = append(limitedAddresses, address)
		} else {
			healthyAddresses = append(healthyAddresses, address)
		}
	}
	return healthyAddresses, limitedAddresses
}

// The candidate order after the tier: proximity, then fewest consecutive
// failures, then the most recent success, then the address.
func (self *ExtenderDirectory) compareCandidateWithLock(
	a *extenderDirectoryAddress,
	b *extenderDirectoryAddress,
	now time.Time,
) int {
	if c := self.compareProximityWithLock(a, b, now, false, false); c != 0 {
		return c
	}
	if a.consecutiveFailureCount != b.consecutiveFailureCount {
		return a.consecutiveFailureCount - b.consecutiveFailureCount
	}
	if !a.lastSuccessTime.Equal(b.lastSuccessTime) {
		// the most recent success first
		if a.lastSuccessTime.After(b.lastSuccessTime) {
			return -1
		}
		return 1
	}
	return strings.Compare(a.ip.String(), b.ip.String())
}

// The unheld addresses of one family whose identity is one of the retained
// expired ones. `excludeIps` may be nil.
func (self *ExtenderDirectory) retainedExpiredAddressesWithLock(
	ipVersion int,
	now time.Time,
	excludeIps map[netip.Addr]bool,
) []*extenderDirectoryAddress {
	retained, _ := self.retainedExpiredKeyHexesWithLock(now)
	addresses := []*extenderDirectoryAddress{}
	if len(retained) == 0 {
		return addresses
	}
	for ip, address := range self.ipAddresses {
		if excludeIps[ip] {
			continue
		}
		if ipVersion != 0 && addressIpVersion(ip) != ipVersion {
			continue
		}
		if extenderBefore(now, address.holdUntilTime) {
			continue
		}
		if !retained[address.publicKeyHex] {
			continue
		}
		addresses = append(addresses, address)
	}
	return addresses
}

// The usable addresses of one family: not held, dialable, key active. The
// filter every candidate order shares. `excludeIps` may be nil.
func (self *ExtenderDirectory) usableAddressesWithLock(
	ipVersion int,
	now time.Time,
	excludeIps map[netip.Addr]bool,
) []*extenderDirectoryAddress {
	addresses := []*extenderDirectoryAddress{}
	for ip, address := range self.ipAddresses {
		if excludeIps[ip] {
			continue
		}
		if ipVersion != 0 && addressIpVersion(ip) != ipVersion {
			continue
		}
		if extenderBefore(now, address.holdUntilTime) {
			continue
		}
		if !self.addressDialableWithLock(address) {
			continue
		}
		if !self.addressActiveWithLock(address, now) {
			continue
		}
		addresses = append(addresses, address)
	}
	return addresses
}

// The verified-first rule every candidate order starts with.
func compareExtenderVerified(a *extenderDirectoryAddress, b *extenderDirectoryAddress) int {
	aVerified := a.publicKeyHex != ""
	bVerified := b.publicKeyHex != ""
	if aVerified == bVerified {
		return 0
	}
	if aVerified {
		return -1
	}
	return 1
}

// The proximity order of DESIGNNOTES4.md §4: the hinted continent before the
// others before unknown, then within a tier a measured address before an
// unmeasured one and ascending by rtt, a sample due a refresh ranking as it
// stands. `explore` is the probe pass asking for what to measure: the samples
// due a refresh first, ascending, since they lead the order until they are
// measured again, then the unmeasured, then the rest; `attesting` counts only
// an attested sample as a measurement.
func (self *ExtenderDirectory) compareProximityWithLock(
	a *extenderDirectoryAddress,
	b *extenderDirectoryAddress,
	now time.Time,
	explore bool,
	attesting bool,
) int {
	aTier := self.continentTierWithLock(a)
	bTier := self.continentTierWithLock(b)
	if aTier != bTier {
		return aTier - bTier
	}
	aLatency, aMeasured := self.latencyWithLock(a, now, attesting)
	bLatency, bMeasured := self.latencyWithLock(b, now, attesting)
	if explore {
		exploreRank := func(address *extenderDirectoryAddress, measured bool) int {
			switch {
			case measured && address.latencyRefreshDue:
				return 0
			case !measured:
				return 1
			default:
				return 2
			}
		}
		if aRank, bRank := exploreRank(a, aMeasured), exploreRank(b, bMeasured); aRank != bRank {
			return aRank - bRank
		}
	} else if aMeasured != bMeasured {
		if aMeasured {
			return -1
		}
		return 1
	}
	if aMeasured && aLatency != bLatency {
		if aLatency < bLatency {
			return -1
		}
		return 1
	}
	return 0
}

// The continent tier of an address under the current hint (DESIGNNOTES4.md
// §4): 0 on the hinted continent, 1 on another, 2 unknown -- an unverified
// address, or a record that predates the continent field. With no hint every
// address is tier 0, which leaves the order what it was.
func (self *ExtenderDirectory) continentTierWithLock(address *extenderDirectoryAddress) int {
	return self.continentTierOfRecordWithLock(self.keyHexRecords[address.publicKeyHex])
}

// The current latency sample of an address, and whether there is one: a sample
// exists, is younger than LatencyMaxAge by either clock and, when `attesting`,
// was attested.
func (self *ExtenderDirectory) latencyWithLock(
	address *extenderDirectoryAddress,
	now time.Time,
	attesting bool,
) (time.Duration, bool) {
	if address.latencyTime.IsZero() || address.latency <= 0 {
		return 0, false
	}
	if attesting && !address.latencyAttested {
		return 0, false
	}
	if 0 < self.settings.LatencyMaxAge && self.settings.LatencyMaxAge <= extenderElapsed(now, address.latencyTime) {
		return 0, false
	}
	return address.latency, true
}

// ProbeCandidates is what a probe pass measures, in the order it should
// (DESIGNNOTES4.md §4): the hinted continent first, and within a tier the
// samples due a refresh lowest first (RefreshLatencies), then the addresses
// with no current sample, then those with one, so the prior saves probes
// rather than merely reordering them. `attesting` treats an unattested sample
// as none, which is what a provider's pass has yet to do. A limited address
// is left out until its backoff passes (A12): a probe of it would be turned
// away again, and a limit is never a measurement.
func (self *ExtenderDirectory) ProbeCandidates(
	ipVersion int,
	count int,
	attesting bool,
) []*ExtenderCandidate {
	if count <= 0 {
		return []*ExtenderCandidate{}
	}
	now := self.settings.Now()

	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	addresses, _ := splitExtenderLimitedAddresses(self.usableAddressesWithLock(ipVersion, now, nil), now)
	slices.SortFunc(addresses, func(a *extenderDirectoryAddress, b *extenderDirectoryAddress) int {
		if c := compareExtenderVerified(a, b); c != 0 {
			return c
		}
		if c := self.compareProximityWithLock(a, b, now, true, attesting); c != 0 {
			return c
		}
		if a.consecutiveFailureCount != b.consecutiveFailureCount {
			return a.consecutiveFailureCount - b.consecutiveFailureCount
		}
		return strings.Compare(a.ip.String(), b.ip.String())
	})

	candidates := []*ExtenderCandidate{}
	for _, address := range addresses {
		if count <= len(candidates) {
			break
		}
		candidates = append(candidates, self.candidateWithLock(address, now))
	}
	return candidates
}

// MeasuredLatencies is every current latency sample of a usable address of
// one family (0 for any), due a refresh or not, which is what the candidate
// order ranks by. With `attesting` only attested samples count.
func (self *ExtenderDirectory) MeasuredLatencies(ipVersion int, attesting bool) []time.Duration {
	return self.usableLatencies(ipVersion, attesting, true)
}

// The latency samples a probe pass counts its window over (DESIGNNOTES4.md
// §4): every current sample of a usable address of one family (0 for any)
// that is not due a refresh, since one that is will be measured again
// (RefreshLatencies). With `attesting` only attested samples count.
func (self *ExtenderDirectory) probeWindowLatencies(ipVersion int, attesting bool) []time.Duration {
	return self.usableLatencies(ipVersion, attesting, false)
}

// The current latency samples of the usable addresses of one family, with or
// without the samples due a refresh.
func (self *ExtenderDirectory) usableLatencies(ipVersion int, attesting bool, includeRefreshDue bool) []time.Duration {
	now := self.settings.Now()

	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	latencies := []time.Duration{}
	for _, address := range self.usableAddressesWithLock(ipVersion, now, nil) {
		if address.latencyRefreshDue && !includeRefreshDue {
			continue
		}
		if latency, measured := self.latencyWithLock(address, now, attesting); measured {
			latencies = append(latencies, latency)
		}
	}
	return latencies
}

// The dialable form of one address, filled from its record when it has one.
func (self *ExtenderDirectory) candidateWithLock(
	address *extenderDirectoryAddress,
	now time.Time,
) *ExtenderCandidate {
	candidate := &ExtenderCandidate{
		Ip:        address.ip,
		IpVersion: addressIpVersion(address.ip),
		Carriers:  []string{ExtenderCarrierTcp, ExtenderCarrierQuic, ExtenderCarrierDns},
		TcpPort:   ExtenderTcpPort,
		UdpPort:   ExtenderQuicPort,
		DnsPort:   ExtenderDnsPort,
		DnsPorts:  []int{ExtenderDnsPort},
		DnsTld:    DefaultExtenderDnsTld,
		Source:    address.source,
	}
	if extenderBefore(now, address.limitedUntilTime) {
		candidate.LimitedUntil = address.limitedUntilTime
	}
	if latency, measured := self.latencyWithLock(address, now, false); measured {
		candidate.Latency = latency
		candidate.LatencyAttested = address.latencyAttested
		candidate.LatencyRefreshDue = address.latencyRefreshDue
	}
	keyRecord := self.keyHexRecords[address.publicKeyHex]
	if keyRecord == nil || keyRecord.recordBody == nil {
		return candidate
	}
	body := keyRecord.recordBody
	candidate.Verified = true
	candidate.Expired = self.keyRecordExpiredWithLock(keyRecord, now)
	candidate.PublicKey = slices.Clone(keyRecord.publicKey)
	candidate.CountryCode = body.CountryCode
	candidate.ContinentCode = strings.ToUpper(strings.TrimSpace(body.ContinentCode))
	candidate.DirectoryTier = int(body.DirectoryTier)
	if 0 < body.TcpPort {
		candidate.TcpPort = int(body.TcpPort)
	}
	if 0 < body.UdpPort {
		candidate.UdpPort = int(body.UdpPort)
	}
	// the list when the record carries one, else the single port, else the
	// default (L2)
	if dnsPorts := recordDnsPorts(body); 0 < len(dnsPorts) {
		candidate.DnsPort = dnsPorts[0]
		candidate.DnsPorts = dnsPorts
	}
	if body.DnsTld != "" {
		candidate.DnsTld = body.DnsTld
	}
	if carriers := recordAddressCarriers(body, address.ip); carriers != nil {
		candidate.Carriers = carriers
	}
	// the webrtc carrier needs the record's rendezvous id and a signaling
	// path on this host, which the enable says the owner has (S)
	if webRtcClientId, err := IdFromBytes(body.WebRtcClientId); err == nil {
		candidate.WebRtcClientId = webRtcClientId
	}
	if !self.webRtcCarrierEnabled || candidate.WebRtcClientId == (Id{}) {
		candidate.Carriers = slices.DeleteFunc(
			slices.Clone(candidate.Carriers),
			func(carrier string) bool { return carrier == ExtenderCarrierWebRtc },
		)
	}
	return candidate
}

// SetWebRtcCarrierEnabled turns the webrtc carrier of every candidate on or
// off at run time (EXTENDER.md S): on when the owner has installed a carrier
// with a signaling path on its connect settings, off again when it loses it.
func (self *ExtenderDirectory) SetWebRtcCarrierEnabled(enabled bool) {
	changed := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.webRtcCarrierEnabled == enabled {
			return
		}
		self.webRtcCarrierEnabled = enabled
		changed = true
	}()
	if changed {
		self.changeMonitor.Update(func(version uint64) uint64 { return version + 1 })
	}
}

// WebRtcCarrierEnabled reports whether candidates carry the webrtc carrier.
func (self *ExtenderDirectory) WebRtcCarrierEnabled() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.webRtcCarrierEnabled
}

// WebRtcClientId is the exchange rendezvous id the verified record of one
// identity carries for its webrtc carrier (EXTENDER.md S), which the dial
// side's resolver signals to. false for an unknown key, a key with no record,
// and a record that carries no id.
func (self *ExtenderDirectory) WebRtcClientId(publicKey []byte) (Id, bool) {
	keyHex := hex.EncodeToString(publicKey)
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	keyRecord := self.keyHexRecords[keyHex]
	if keyRecord == nil || keyRecord.recordBody == nil {
		return Id{}, false
	}
	webRtcClientId, err := IdFromBytes(keyRecord.recordBody.WebRtcClientId)
	if err != nil || webRtcClientId == (Id{}) {
		return Id{}, false
	}
	return webRtcClientId, true
}

// The dns ports one record offers, ascending (L2). DnsPorts when it has them,
// else the single DnsPort, else nothing, which leaves the candidate on the
// carrier default. A dial adds whichever of 4053 and 53 these do not include
// (ExtenderCandidate.dnsCarrierPorts).
func recordDnsPorts(body *protocol.ExtenderRecordBody) []int {
	dnsPorts := []int{}
	for _, dnsPort := range body.DnsPorts {
		dnsPorts = append(dnsPorts, int(dnsPort))
	}
	if dnsPorts = orderedDnsPorts(dnsPorts); 0 < len(dnsPorts) {
		return dnsPorts
	}
	if 0 < body.DnsPort {
		return []int{int(body.DnsPort)}
	}
	return nil
}

// The carriers the record lists for one address, or nil when the record does
// not name the address, which leaves the caller on the carrier defaults.
func recordAddressCarriers(body *protocol.ExtenderRecordBody, ip netip.Addr) []string {
	for _, recordAddress := range body.Addresses {
		recordIp, err := netip.ParseAddr(recordAddress.Ip)
		if err != nil || recordIp.Unmap() != ip {
			continue
		}
		carriers := []string{}
		for _, carrier := range recordAddress.Carriers {
			switch carrier {
			case ExtenderCarrierTcp, ExtenderCarrierQuic, ExtenderCarrierDns, ExtenderCarrierWebRtc:
				carriers = append(carriers, carrier)
			}
		}
		return carriers
	}
	return nil
}

// Reports whether the key backing an address is active (B5) -- an unverified
// address is dialable on the carrier defaults, so it counts as active here.
func (self *ExtenderDirectory) addressActiveWithLock(
	address *extenderDirectoryAddress,
	now time.Time,
) bool {
	if address.publicKeyHex == "" {
		return true
	}
	keyRecord := self.keyHexRecords[address.publicKeyHex]
	if keyRecord == nil || keyRecord.recordBody == nil {
		return false
	}
	if self.keyRecordRevokedWithLock(keyRecord) {
		return false
	}
	return !self.keyRecordExpiredWithLock(keyRecord, now)
}

// addressDialableWithLock is whether an address may be dialed at all, before
// any question of whether its record is current.
//
// An address with no verified key is never dialed unless it was configured by
// hand. A dns or import bootstrap entry holds no key until a signed record
// names it, and dialing it before then means handing the extender request --
// the destination, the shared secret -- to whoever answers at that address,
// with nothing checked. Dns is where the address came from, and dns is not
// trusted; the operator's signature is. With the TXT bootstrap a dns answer
// arrives verified, so this costs a client nothing it should have had.
//
// A manual entry is the operator's own configuration, carries its secret, and
// is the one case where the person configuring it is the trust anchor.
func (self *ExtenderDirectory) addressDialableWithLock(address *extenderDirectoryAddress) bool {
	if address.publicKeyHex != "" {
		return true
	}
	return address.source == ExtenderSourceManual
}

// B5: a revocation at or after the record's issue time.
func (self *ExtenderDirectory) keyRecordRevokedWithLock(keyRecord *extenderDirectoryRecord) bool {
	if keyRecord.revocationBody == nil {
		return false
	}
	if keyRecord.recordBody == nil {
		return true
	}
	return keyRecord.recordBody.IssueTimeMs <= keyRecord.revocationBody.IssueTimeMs
}

// B5: the record expiry with the configured skew.
func (self *ExtenderDirectory) keyRecordExpiredWithLock(
	keyRecord *extenderDirectoryRecord,
	now time.Time,
) bool {
	if keyRecord.recordBody == nil {
		return true
	}
	if keyRecord.recordBody.ExpireTimeMs == 0 {
		return false
	}
	expireTime := time.UnixMilli(int64(keyRecord.recordBody.ExpireTimeMs)).Add(self.settings.RecordExpireSkew)
	return expireTime.Before(now)
}

// Whether an identity is merely expired: a verified record, not revoked, past
// its expiry. That is the only kind the directory retains past its record
// (MaxExpiredRecordCount); a revoked key is never a last resort.
func (self *ExtenderDirectory) keyRecordLapsedWithLock(
	keyRecord *extenderDirectoryRecord,
	now time.Time,
) bool {
	if keyRecord == nil || keyRecord.recordBody == nil {
		return false
	}
	if self.keyRecordRevokedWithLock(keyRecord) {
		return false
	}
	return self.keyRecordExpiredWithLock(keyRecord, now)
}

// The order the expired identities are retained in: the newest expiry first,
// the key breaking a tie so the order is total and the retained set stable.
// Both records must be lapsed.
func compareExpiredKeyRecords(
	aKeyHex string,
	a *extenderDirectoryRecord,
	bKeyHex string,
	b *extenderDirectoryRecord,
) int {
	aExpireTimeMs := a.recordBody.ExpireTimeMs
	bExpireTimeMs := b.recordBody.ExpireTimeMs
	if aExpireTimeMs != bExpireTimeMs {
		if bExpireTimeMs < aExpireTimeMs {
			return -1
		}
		return 1
	}
	return strings.Compare(aKeyHex, bKeyHex)
}

// The retained expired identities -- the newest MaxExpiredRecordCount of the
// lapsed ones -- and the rest, oldest expiry first, which is the order the
// policy evicts them in.
func (self *ExtenderDirectory) retainedExpiredKeyHexesWithLock(
	now time.Time,
) (retained map[string]bool, evictable []string) {
	keyHexes := []string{}
	for keyHex, keyRecord := range self.keyHexRecords {
		if self.keyRecordLapsedWithLock(keyRecord, now) {
			keyHexes = append(keyHexes, keyHex)
		}
	}
	slices.SortFunc(keyHexes, func(a string, b string) int {
		return compareExpiredKeyRecords(a, self.keyHexRecords[a], b, self.keyHexRecords[b])
	})
	retainCount := min(len(keyHexes), max(0, self.settings.MaxExpiredRecordCount))
	retained = map[string]bool{}
	for _, keyHex := range keyHexes[0:retainCount] {
		retained[keyHex] = true
	}
	evictable = slices.Clone(keyHexes[retainCount:])
	slices.Reverse(evictable)
	return retained, evictable
}

// Whether one identity is among the retained expired ones, without building
// the whole set: it is lapsed and fewer than MaxExpiredRecordCount lapsed
// identities come before it.
func (self *ExtenderDirectory) keyRetainedExpiredWithLock(keyHex string, now time.Time) bool {
	keyRecord := self.keyHexRecords[keyHex]
	if !self.keyRecordLapsedWithLock(keyRecord, now) {
		return false
	}
	newerCount := 0
	for otherKeyHex, otherKeyRecord := range self.keyHexRecords {
		if otherKeyHex == keyHex || !self.keyRecordLapsedWithLock(otherKeyRecord, now) {
			continue
		}
		if compareExpiredKeyRecords(otherKeyHex, otherKeyRecord, keyHex, keyRecord) < 0 {
			newerCount += 1
			if self.settings.MaxExpiredRecordCount <= newerCount {
				return false
			}
		}
	}
	return newerCount < self.settings.MaxExpiredRecordCount
}

// Evicts the expired identities beyond the retained count, oldest expiry
// first, with their addresses. A manual address outlives its identity as an
// unverified manual entry, as it does a root key rotation, and a revocation
// the identity carried is kept so a replayed record cannot bring the key back.
func (self *ExtenderDirectory) evictExpiredWithLock(now time.Time) (changed bool) {
	_, evictable := self.retainedExpiredKeyHexesWithLock(now)
	for _, keyHex := range evictable {
		keyRecord := self.keyHexRecords[keyHex]
		keyRecord.record = nil
		keyRecord.recordBody = nil
		self.tierRemoveWithLock(keyHex, keyRecord)
		if keyRecord.revocation == nil {
			self.deleteKeyRecordWithLock(keyHex)
		}
		for ip, address := range self.ipAddresses {
			if address.publicKeyHex != keyHex {
				continue
			}
			if address.source == ExtenderSourceManual {
				address.publicKeyHex = ""
				continue
			}
			delete(self.ipAddresses, ip)
		}
		changed = true
	}
	return changed
}

// The removal policy of E1. A manual address is never removed by policy: it
// was configured by hand and only a reconfiguration takes it away.
func (self *ExtenderDirectory) shouldRemoveWithLock(
	address *extenderDirectoryAddress,
	now time.Time,
) bool {
	if address.source == ExtenderSourceManual {
		return false
	}
	if address.successCount == 0 {
		if address.firstFailureTime.IsZero() {
			return false
		}
		return self.settings.NeverSucceededRemoveTimeout <= now.Sub(address.firstFailureTime)
	}
	return self.settings.StaleSuccessRemoveTimeout <= now.Sub(address.lastSuccessTime) &&
		self.settings.RemoveConsecutiveFailureCount <= address.consecutiveFailureCount
}

// Evicts down to the address cap: revoked first, then expired beyond the
// retained identities oldest expiry first, then never succeeded oldest first,
// then oldest last success. Manual addresses and the addresses of the
// retained expired identities are never evicted, so a cap smaller than those
// simply holds more than the cap.
func (self *ExtenderDirectory) enforceAddressCapWithLock(now time.Time) (changed bool) {
	if self.settings.MaxAddressCount <= 0 || len(self.ipAddresses) <= self.settings.MaxAddressCount {
		return false
	}
	retained, _ := self.retainedExpiredKeyHexesWithLock(now)
	evictable := []*extenderDirectoryAddress{}
	for _, address := range self.ipAddresses {
		if address.source == ExtenderSourceManual {
			continue
		}
		if retained[address.publicKeyHex] {
			// the last resort of a client that lost every current path
			continue
		}
		evictable = append(evictable, address)
	}
	tier := func(address *extenderDirectoryAddress) int {
		if address.publicKeyHex != "" && !self.addressActiveWithLock(address, now) {
			return 0
		}
		if address.successCount == 0 {
			return 1
		}
		return 2
	}
	// within the inactive tier a revoked key goes before an expired one, and
	// the expired go oldest expiry first
	expireTimeMs := func(address *extenderDirectoryAddress) uint64 {
		keyRecord := self.keyHexRecords[address.publicKeyHex]
		if !self.keyRecordLapsedWithLock(keyRecord, now) {
			return 0
		}
		return keyRecord.recordBody.ExpireTimeMs
	}
	slices.SortFunc(evictable, func(a *extenderDirectoryAddress, b *extenderDirectoryAddress) int {
		aTier, bTier := tier(a), tier(b)
		if aTier != bTier {
			return aTier - bTier
		}
		switch aTier {
		case 0:
			if aExpireTimeMs, bExpireTimeMs := expireTimeMs(a), expireTimeMs(b); aExpireTimeMs != bExpireTimeMs {
				if aExpireTimeMs < bExpireTimeMs {
					return -1
				}
				return 1
			}
		case 1:
			// the oldest known first
			if !a.addTime.Equal(b.addTime) {
				if a.addTime.Before(b.addTime) {
					return -1
				}
				return 1
			}
		case 2:
			// the oldest last success first
			if !a.lastSuccessTime.Equal(b.lastSuccessTime) {
				if a.lastSuccessTime.Before(b.lastSuccessTime) {
					return -1
				}
				return 1
			}
		}
		return strings.Compare(a.ip.String(), b.ip.String())
	})
	for _, address := range evictable {
		if len(self.ipAddresses) <= self.settings.MaxAddressCount {
			break
		}
		delete(self.ipAddresses, address.ip)
		changed = true
	}
	if changed {
		self.pruneKeyRecordsWithLock()
		// a record that lost an address may be in another pool now
		self.tierRebuildWithLock(now)
	}
	return changed
}

// Drops identities that no longer describe anything: no address and no
// revocation to enforce. A retained expired identity is kept either way; it is
// evicted only once it falls beyond the retained count, by `Expire`.
func (self *ExtenderDirectory) pruneKeyRecordsWithLock() (changed bool) {
	retained, _ := self.retainedExpiredKeyHexesWithLock(self.settings.Now())
	referencedKeyHexes := map[string]bool{}
	for _, address := range self.ipAddresses {
		if address.publicKeyHex != "" {
			referencedKeyHexes[address.publicKeyHex] = true
		}
	}
	for keyHex, keyRecord := range self.keyHexRecords {
		if referencedKeyHexes[keyHex] || keyRecord.revocation != nil || retained[keyHex] {
			continue
		}
		self.deleteKeyRecordWithLock(keyHex)
		changed = true
	}
	return changed
}

// The status view (F2).
func (self *ExtenderDirectory) Snapshot() *ExtenderDirectorySnapshot {
	now := self.settings.Now()

	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	snapshot := &ExtenderDirectorySnapshot{
		Entries: []*ExtenderDirectoryEntry{},
	}
	for _, address := range self.ipAddresses {
		candidate := self.candidateWithLock(address, now)
		state := self.addressStateWithLock(address, now)
		entry := &ExtenderDirectoryEntry{
			Ip:              address.ip,
			IpVersion:       addressIpVersion(address.ip),
			PublicKey:       candidate.PublicKey,
			Carriers:        candidate.Carriers,
			CountryCode:     candidate.CountryCode,
			ContinentCode:   candidate.ContinentCode,
			DirectoryTier:   candidate.DirectoryTier,
			Latency:         candidate.Latency,
			State:           state,
			Source:          address.source,
			LastSuccessTime: address.lastSuccessTime,
			LastFailureTime: address.lastFailureTime,
			SuccessCount:    address.successCount,
			FailureCount:    address.failureCount,
			InUse:           self.ipInUseCounts[address.ip],
			LimitedUntil:    candidate.LimitedUntil,
		}
		if keyRecord := self.keyHexRecords[address.publicKeyHex]; keyRecord != nil && keyRecord.recordBody != nil {
			if 0 < keyRecord.recordBody.ExpireTimeMs {
				entry.ExpireTime = time.UnixMilli(int64(keyRecord.recordBody.ExpireTimeMs))
			}
		}
		snapshot.Entries = append(snapshot.Entries, entry)
		snapshot.KnownCount += 1
		if 0 < entry.InUse {
			snapshot.InUseCount += 1
		}
		switch state {
		case ExtenderStateActive, ExtenderStateUnverified:
			snapshot.ActiveCount += 1
		case ExtenderStateWarning:
			snapshot.WarningCount += 1
		case ExtenderStateHold:
			snapshot.HoldCount += 1
		}
	}
	slices.SortFunc(snapshot.Entries, func(a *ExtenderDirectoryEntry, b *ExtenderDirectoryEntry) int {
		return strings.Compare(a.Ip.String(), b.Ip.String())
	})
	return snapshot
}

// The count of addresses whose key is active (B5), hold included. This is what
// the low-water re-bootstrap reads: a held address is still a known extender,
// and re-resolving dns would not produce a better one.
//
// An address with no key and no manual configuration does not count. It is
// not dialable, so it is not an extender the client has; and re-resolving dns
// is exactly what could produce a better one, since the TXT answers carry the
// records that would verify it.
func (self *ExtenderDirectory) ActiveCount(ipVersion int) int {
	now := self.settings.Now()

	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	count := 0
	for ip, address := range self.ipAddresses {
		if ipVersion != 0 && addressIpVersion(ip) != ipVersion {
			continue
		}
		if !self.addressDialableWithLock(address) {
			continue
		}
		if self.addressActiveWithLock(address, now) {
			count += 1
		}
	}
	return count
}

// The count of addresses that are dialable right now, hold excluded, which is
// what the startup gate reads.
func (self *ExtenderDirectory) UsableCount(ipVersion int) int {
	now := self.settings.Now()

	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	count := 0
	for ip, address := range self.ipAddresses {
		if ipVersion != 0 && addressIpVersion(ip) != ipVersion {
			continue
		}
		if extenderBefore(now, address.holdUntilTime) {
			continue
		}
		if !self.addressDialableWithLock(address) {
			continue
		}
		if self.addressActiveWithLock(address, now) {
			count += 1
		}
	}
	return count
}

// Whether the directory holds an active record for this identity key: verified under the root keys, not revoked and not expired
// (B5). Local dial evidence plays no part -- a held address is still an
// extender the operator vouched for. It is what an extender asks of a pinger
// that names itself by its key (GEOMAP §2.4).
func (self *ExtenderDirectory) IsActiveKey(publicKey []byte) bool {
	if len(publicKey) != ed25519.PublicKeySize {
		return false
	}
	keyHex := hex.EncodeToString(publicKey)
	now := self.settings.Now()

	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	keyRecord := self.keyHexRecords[keyHex]
	if keyRecord == nil || keyRecord.recordBody == nil {
		return false
	}
	if self.keyRecordRevokedWithLock(keyRecord) {
		return false
	}
	return !self.keyRecordExpiredWithLock(keyRecord, now)
}

// Reports whether this address may still be dialed: it is known, not held and
// its key is active -- or it is one of the retained expired identities, the
// last tier of Candidates, so an established dialer to it is kept rather than
// dropped the moment its record lapses. The strategy drops the dialers of
// everything else.
func (self *ExtenderDirectory) AddressUsable(ip netip.Addr) bool {
	if !ip.IsValid() {
		return false
	}
	ip = ip.Unmap()
	now := self.settings.Now()

	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	address := self.ipAddresses[ip]
	if address == nil {
		return false
	}
	if extenderBefore(now, address.holdUntilTime) {
		return false
	}
	if !self.addressDialableWithLock(address) {
		return false
	}
	if self.addressActiveWithLock(address, now) {
		return true
	}
	return self.keyRetainedExpiredWithLock(address.publicKeyHex, now)
}

func (self *ExtenderDirectory) addressStateWithLock(
	address *extenderDirectoryAddress,
	now time.Time,
) string {
	if address.publicKeyHex != "" {
		keyRecord := self.keyHexRecords[address.publicKeyHex]
		if keyRecord == nil {
			return ExtenderStateUnverified
		}
		if self.keyRecordRevokedWithLock(keyRecord) {
			return ExtenderStateRevoked
		}
		if self.keyRecordExpiredWithLock(keyRecord, now) {
			return ExtenderStateExpired
		}
	}
	if extenderBefore(now, address.holdUntilTime) {
		return ExtenderStateHold
	}
	if 0 < self.settings.WarningConsecutiveFailureCount &&
		self.settings.WarningConsecutiveFailureCount <= address.consecutiveFailureCount {
		return ExtenderStateWarning
	}
	if address.publicKeyHex == "" {
		return ExtenderStateUnverified
	}
	return ExtenderStateActive
}

// The trailing window the event rate is kept over. A settings value of zero
// falls back to the default rather than keeping nothing, so a partially filled
// settings struct still reports a rate.
func (self *ExtenderDirectory) eventWindowTimeout() time.Duration {
	if 0 < self.settings.EventWindowTimeout {
		return self.settings.EventWindowTimeout
	}
	return DefaultExtenderDirectorySettings().EventWindowTimeout
}

// Records one applied record or revocation (K4). Only the feed and the mesh
// count: they are the two paths that carry what other participants published,
// which is what the app's rate is a measure of. A stored record loaded at
// start, a manual address and an imported one are not events.
func (self *ExtenderDirectory) noteEventWithLock(source string, now time.Time) {
	switch source {
	case ExtenderSourceFeed, ExtenderSourceGossip:
	default:
		return
	}
	self.eventTimes = append(self.eventTimes, now)
	self.pruneEventsWithLock(now)
}

// Drops the apply times that have aged out of the window, and anything beyond
// the ring cap, by moving the head past them; the dropped prefix is reclaimed
// once it is at least half the slice, so a drop costs constant time however
// full the ring is.
func (self *ExtenderDirectory) pruneEventsWithLock(now time.Time) {
	windowStartTime := now.Add(-self.eventWindowTimeout())
	for self.eventHead < len(self.eventTimes) && self.eventTimes[self.eventHead].Before(windowStartTime) {
		self.eventHead += 1
	}
	if ExtenderDirectoryEventRingCount < len(self.eventTimes)-self.eventHead {
		self.eventHead = len(self.eventTimes) - ExtenderDirectoryEventRingCount
	}
	if 0 < self.eventHead && len(self.eventTimes) <= 2*self.eventHead {
		keptCount := copy(self.eventTimes, self.eventTimes[self.eventHead:])
		clear(self.eventTimes[keptCount:])
		self.eventTimes = self.eventTimes[:keptCount]
		self.eventHead = 0
	}
}

// The number of records and revocations applied from the feed or the mesh at
// or after `since` (K4). Nothing older than the event window is kept, so a
// `since` further back than the window reports what the window holds.
func (self *ExtenderDirectory) EventCountSince(since time.Time) int {
	now := self.settings.Now()

	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.pruneEventsWithLock(now)
	count := 0
	for _, eventTime := range self.eventTimes[self.eventHead:] {
		if !eventTime.Before(since) {
			count += 1
		}
	}
	return count
}

// The count over the trailing event window, which is the minute the app panel
// shows (K4).
func (self *ExtenderDirectory) EventCountLastMinute() int {
	return self.EventCountSince(self.settings.Now().Add(-self.eventWindowTimeout()))
}

func (self *ExtenderDirectory) changedWithLock() {
	self.version += 1
	self.changeMonitor.Set(self.version)
}

// Returns the directory to what a fresh install starts with (the reset of the
// account screen's extender section). Everything it learned goes: the records
// and revocations from the feed, the mesh, the dns bootstrap, an activation's
// sample and a share, every address with its local evidence -- successes,
// failures, holds, limits, latency samples -- the manual addresses, the
// continent hint and the operator's last country. `rootKeySet` replaces the
// root keys in force, which drops whatever a hello installed; nil or empty is
// the unconfigured state, which waits for the first hello. The store is
// written before this returns, so a process that ends right after it starts
// as fresh as the directory now is, and the startup gate is back where a
// directory with no network client has it, so the client started next waits
// it out once, as on a first run (E4).
//
// What stays is not knowledge of other extenders: the identities this
// directory was told to keep (KeepPublicKey), which are this device's own
// extender identity -- its record and any revocation, verified again under
// the new keys, with fresh evidence for its addresses, which take the source
// an activation applies them with -- and the settings, the active cap in
// force, the subscriptions and the live use of each address (SetInUse).
//
// A record or revocation whose verification began before the reset is not
// applied after it (applyVerifiedRecord). Anything else that writes learned
// state -- a network client's bootstrap and feed, a gossip node -- is stopped
// by its owner before the reset and started again after it, which is how the
// sdk's network space makes the client relearn from scratch: an address a
// stopped writer added after the reset would be one the reset never cleared.
func (self *ExtenderDirectory) Reset(rootKeySet *ExtenderRootKeySet) {
	if rootKeySet == nil {
		rootKeySet = NewExtenderRootKeySet()
	}
	now := self.settings.Now()
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		self.resetVersion += 1
		self.rootKeySet = rootKeySet
		keyHexRecords := map[string]*extenderDirectoryRecord{}
		ipAddresses := map[netip.Addr]*extenderDirectoryAddress{}
		for keyHex := range self.keptKeyHexes {
			keyRecord := self.keyHexRecords[keyHex]
			if keyRecord == nil {
				continue
			}
			// judged again as SetRootKeys judges, an empty set judging nothing
			if 0 < rootKeySet.Len() {
				if keyRecord.record != nil {
					if _, err := rootKeySet.VerifyRecord(keyRecord.record); err != nil {
						keyRecord.record = nil
						keyRecord.recordBody = nil
					}
				}
				if keyRecord.revocation != nil {
					if _, err := rootKeySet.VerifyRevocation(keyRecord.revocation); err != nil {
						keyRecord.revocation = nil
						keyRecord.revocationBody = nil
					}
				}
			}
			if keyRecord.record == nil && keyRecord.revocation == nil {
				continue
			}
			keptIps := []netip.Addr{}
			for _, ip := range keyRecord.ips {
				if address := self.ipAddresses[ip]; address == nil || address.publicKeyHex != keyHex {
					continue
				}
				ipAddresses[ip] = &extenderDirectoryAddress{
					ip:           ip,
					source:       ExtenderSourceBootstrap,
					publicKeyHex: keyHex,
					addTime:      now,
				}
				keptIps = append(keptIps, ip)
			}
			keyRecord.ips = keptIps
			keyHexRecords[keyHex] = keyRecord
		}
		self.keyHexRecords = keyHexRecords
		self.ipAddresses = ipAddresses
		self.continentHint = ""
		self.countryHint = ""
		self.countryHintTime = time.Time{}
		self.countryHintCurrent = false
		clear(self.eventTimes)
		self.eventTimes = self.eventTimes[:0]
		self.eventHead = 0
		self.tierRebuildWithLock(now)
		self.changedWithLock()
	}()
	self.initialSampleMonitor.Set(ExtenderInitialSampleNone)
	self.log.Infof("[extender]directory reset\n")
	self.save()
}

// Ends the save loop after one last save of anything the loop has not written
// yet, so a directory closed inside the coalescing window is still durable.
func (self *ExtenderDirectory) Close() {
	self.closeOnce.Do(func() {
		self.cancel()
		<-self.done
		self.save()
	})
}

// The coalescing save loop (E1). It waits for a change, sleeps the save
// timeout so a burst collapses, and only then re-subscribes and writes the
// state it reads -- subscribing before the sleep would leave the next round
// armed by changes the write already carried.
func (self *ExtenderDirectory) run(notify chan struct{}) {
	if self.settings.Store == nil {
		<-self.ctx.Done()
		return
	}
	for {
		select {
		case <-self.ctx.Done():
			return
		case <-notify:
		}
		select {
		case <-self.ctx.Done():
			return
		case <-time.After(self.settings.SaveTimeout):
		}
		_, notify = self.changeMonitor.Get()
		self.save()
	}
}

// The persisted envelope (E1).
type extenderDirectoryStoreState struct {
	Version   int                              `json:"version"`
	Records   []*extenderDirectoryStoreRecord  `json:"records"`
	Addresses []*extenderDirectoryStoreAddress `json:"addresses"`
	// The operator's last country, absent until the operator has placed this
	// client. Kept raw and decoded on its own, so a section this build cannot
	// read is dropped alone (extenderDirectoryStoreCountryHint).
	CountryHint json.RawMessage `json:"country_hint,omitempty"`
}

// The operator's last country as the store keeps it (SpoofCountryCode): the
// two-letter code and when the operator last answered it, and nothing else --
// no address, path or network. It is a section with a version of its own
// rather than a new envelope version, because a build that predates it
// discards an envelope of another version whole, records and addresses with
// it, while it skips a field it does not know; so a store this build writes
// still loads in full on a build that predates it.
type extenderDirectoryStoreCountryHint struct {
	Version     int    `json:"version"`
	CountryCode string `json:"country_code"`
	TimeMs      int64  `json:"time_ms"`
}

// A stored identity carries the signed messages verbatim, so a later root key
// rotation can re-judge exactly what was received.
type extenderDirectoryStoreRecord struct {
	PublicKey  string `json:"public_key"`
	Record     []byte `json:"record,omitempty"`
	Revocation []byte `json:"revocation,omitempty"`
}

type extenderDirectoryStoreAddress struct {
	Ip                      string `json:"ip"`
	Source                  string `json:"source,omitempty"`
	PublicKey               string `json:"public_key,omitempty"`
	AddTimeMs               int64  `json:"add_time_ms,omitempty"`
	SuccessCount            int    `json:"success_count,omitempty"`
	FailureCount            int    `json:"failure_count,omitempty"`
	LastSuccessTimeMs       int64  `json:"last_success_time_ms,omitempty"`
	LastFailureTimeMs       int64  `json:"last_failure_time_ms,omitempty"`
	FirstFailureTimeMs      int64  `json:"first_failure_time_ms,omitempty"`
	ConsecutiveFailureCount int    `json:"consecutive_failure_count,omitempty"`
	HoldUntilTimeMs         int64  `json:"hold_until_time_ms,omitempty"`
	LastUseTimeMs           int64  `json:"last_use_time_ms,omitempty"`
}

// Writes the current state through the store. A store failure is logged and
// dropped: the directory is a cache, and losing a write costs a rediscovery.
//
// Two saves can be out at once -- the loop's and a reset's (Reset) -- and the
// state is read before the store is called, so a save that read an older state
// than one already written is skipped rather than written over it. The check
// and the write are one step under the save lock, so the last write is always
// of the newest state read.
func (self *ExtenderDirectory) save() {
	if self.settings.Store == nil {
		return
	}
	now := self.settings.Now()
	var stateBytes []byte
	var version uint64
	err := func() error {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		if self.version == self.savedVersion {
			return nil
		}
		version = self.version
		state := &extenderDirectoryStoreState{
			Version:   ExtenderDirectoryStoreVersion,
			Records:   []*extenderDirectoryStoreRecord{},
			Addresses: []*extenderDirectoryStoreAddress{},
		}
		for keyHex, keyRecord := range self.keyHexRecords {
			storeRecord := &extenderDirectoryStoreRecord{
				PublicKey: keyHex,
			}
			if keyRecord.record != nil {
				recordBytes, err := proto.Marshal(keyRecord.record)
				if err != nil {
					return err
				}
				storeRecord.Record = recordBytes
			}
			if keyRecord.revocation != nil {
				revocationBytes, err := proto.Marshal(keyRecord.revocation)
				if err != nil {
					return err
				}
				storeRecord.Revocation = revocationBytes
			}
			state.Records = append(state.Records, storeRecord)
		}
		slices.SortFunc(state.Records, func(a *extenderDirectoryStoreRecord, b *extenderDirectoryStoreRecord) int {
			return strings.Compare(a.PublicKey, b.PublicKey)
		})
		for _, address := range self.ipAddresses {
			state.Addresses = append(state.Addresses, &extenderDirectoryStoreAddress{
				Ip:                      address.ip.String(),
				Source:                  address.source,
				PublicKey:               address.publicKeyHex,
				AddTimeMs:               extenderTimeMs(address.addTime),
				SuccessCount:            address.successCount,
				FailureCount:            address.failureCount,
				LastSuccessTimeMs:       extenderTimeMs(address.lastSuccessTime),
				LastFailureTimeMs:       extenderTimeMs(address.lastFailureTime),
				FirstFailureTimeMs:      extenderTimeMs(address.firstFailureTime),
				ConsecutiveFailureCount: address.consecutiveFailureCount,
				HoldUntilTimeMs:         extenderTimeMs(address.holdUntilTime),
				LastUseTimeMs:           extenderTimeMs(address.lastUseTime),
			})
		}
		slices.SortFunc(state.Addresses, func(a *extenderDirectoryStoreAddress, b *extenderDirectoryStoreAddress) int {
			return strings.Compare(a.Ip, b.Ip)
		})
		// a country past its max age is not written: no start would use it
		if self.countryHint != "" && self.countryHintWithinMaxAge(self.countryHintTime, now) {
			countryHintBytes, err := json.Marshal(&extenderDirectoryStoreCountryHint{
				Version:     extenderDirectoryStoreCountryHintVersion,
				CountryCode: self.countryHint,
				TimeMs:      extenderTimeMs(self.countryHintTime),
			})
			if err != nil {
				return err
			}
			state.CountryHint = countryHintBytes
		}
		var err error
		stateBytes, err = json.Marshal(state)
		return err
	}()
	if err != nil {
		self.log.Infof("[extender]directory save err = %s\n", err)
		return
	}
	if stateBytes == nil {
		return
	}
	if self.saveReadHook != nil {
		self.saveReadHook()
	}

	self.saveLock.Lock()
	defer self.saveLock.Unlock()

	stale := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return version <= self.savedVersion
	}()
	if stale {
		// a newer state was written since this one was read
		return
	}
	// the store is an external object, so it is called with no state lock
	if err := self.settings.Store.Save(stateBytes); err != nil {
		self.log.Infof("[extender]directory save err = %s\n", err)
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.savedVersion = version
}

// Reads the store at construction. An unreadable, truncated or foreign-version
// document is treated as an empty directory: the cost is a rediscovery, and a
// corrupt file must never keep a client from starting. The stored records are
// decoded without re-verification -- the local store is as trusted as the
// process -- and re-judged as soon as `SetRootKeys` installs an anchor.
func (self *ExtenderDirectory) load() {
	if self.settings.Store == nil {
		return
	}
	stateBytes, err := self.settings.Store.Load()
	if err != nil || len(stateBytes) == 0 {
		return
	}
	state := &extenderDirectoryStoreState{}
	if err := json.Unmarshal(stateBytes, state); err != nil {
		self.log.Infof("[extender]directory load err = %s\n", err)
		return
	}
	if state.Version != ExtenderDirectoryStoreVersion {
		return
	}

	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	for _, storeRecord := range state.Records {
		publicKey, err := hex.DecodeString(storeRecord.PublicKey)
		if err != nil || len(publicKey) == 0 {
			continue
		}
		keyRecord := &extenderDirectoryRecord{
			publicKey: publicKey,
		}
		if 0 < len(storeRecord.Record) {
			record := &protocol.ExtenderRecord{}
			body := &protocol.ExtenderRecordBody{}
			if proto.Unmarshal(storeRecord.Record, record) == nil &&
				proto.Unmarshal(record.Body, body) == nil {
				keyRecord.record = record
				keyRecord.recordBody = body
				keyRecord.applySerial = self.nextApplySerial
				self.nextApplySerial += 1
				for _, recordAddress := range body.Addresses {
					if ip, err := netip.ParseAddr(recordAddress.Ip); err == nil && ip.IsValid() {
						keyRecord.ips = append(keyRecord.ips, ip.Unmap())
					}
				}
			}
		}
		if 0 < len(storeRecord.Revocation) {
			revocation := &protocol.ExtenderRevocation{}
			body := &protocol.ExtenderRevocationBody{}
			if proto.Unmarshal(storeRecord.Revocation, revocation) == nil &&
				proto.Unmarshal(revocation.Body, body) == nil {
				keyRecord.revocation = revocation
				keyRecord.revocationBody = body
			}
		}
		if keyRecord.record == nil && keyRecord.revocation == nil {
			continue
		}
		self.keyHexRecords[strings.ToLower(storeRecord.PublicKey)] = keyRecord
	}
	for _, storeAddress := range state.Addresses {
		ip, err := netip.ParseAddr(storeAddress.Ip)
		if err != nil || !ip.IsValid() {
			continue
		}
		ip = ip.Unmap()
		source := storeAddress.Source
		if source == "" {
			source = ExtenderSourceBootstrap
		}
		publicKeyHex := strings.ToLower(storeAddress.PublicKey)
		if publicKeyHex != "" && self.keyHexRecords[publicKeyHex] == nil {
			// the identity did not survive the load; keep the local evidence
			// and let a later record claim the address again
			publicKeyHex = ""
		}
		self.ipAddresses[ip] = &extenderDirectoryAddress{
			ip:                      ip,
			source:                  source,
			publicKeyHex:            publicKeyHex,
			addTime:                 extenderTimeFromMs(storeAddress.AddTimeMs),
			successCount:            storeAddress.SuccessCount,
			failureCount:            storeAddress.FailureCount,
			lastSuccessTime:         extenderTimeFromMs(storeAddress.LastSuccessTimeMs),
			lastFailureTime:         extenderTimeFromMs(storeAddress.LastFailureTimeMs),
			firstFailureTime:        extenderTimeFromMs(storeAddress.FirstFailureTimeMs),
			consecutiveFailureCount: storeAddress.ConsecutiveFailureCount,
			holdUntilTime:           extenderTimeFromMs(storeAddress.HoldUntilTimeMs),
			lastUseTime:             extenderTimeFromMs(storeAddress.LastUseTimeMs),
		}
		if keyRecord := self.keyHexRecords[publicKeyHex]; keyRecord != nil && !slices.Contains(keyRecord.ips, ip) {
			// an address the key held beyond what its newest record lists
			keyRecord.ips = append(keyRecord.ips, ip)
		}
	}
	now := self.settings.Now()
	// The operator's last country, a section of its own. No section, one of
	// another version or one that does not decode, a code that is not two
	// letters, and a time that is missing, ahead of the clock or past
	// CountryHintMaxAge all leave no country, and none of them costs the
	// records and addresses above. A restored country is stale: the operator
	// gave it on a path of an earlier process, so it ranks after the network
	// country the host reports (SpoofCountryCode).
	func() {
		if len(state.CountryHint) == 0 {
			return
		}
		storeCountryHint := &extenderDirectoryStoreCountryHint{}
		if err := json.Unmarshal(state.CountryHint, storeCountryHint); err != nil {
			self.log.Infof("[extender]directory country load err = %s\n", err)
			return
		}
		if storeCountryHint.Version != extenderDirectoryStoreCountryHintVersion {
			return
		}
		countryCode := NormalizeSpoofCountryCode(storeCountryHint.CountryCode)
		countryHintTime := extenderTimeFromMs(storeCountryHint.TimeMs)
		if countryCode == "" || !self.countryHintWithinMaxAge(countryHintTime, now) {
			return
		}
		self.countryHint = countryCode
		self.countryHintTime = countryHintTime
		self.countryHintCurrent = false
		self.log.Infof(
			"[extender]country hint %s (stored, %s old)\n",
			countryCode,
			now.Sub(countryHintTime).Round(time.Minute),
		)
	}()
	self.pruneKeyRecordsWithLock()
	// a load is the state the store already holds, so it is not a change to
	// save back
	self.savedVersion = self.version
	// but a store written under a larger cap is brought within this one, and
	// that is saved with the next change or the close
	self.tierRebuildWithLock(now)
	if self.enforceActiveRecordCapWithLock(now) {
		self.changedWithLock()
	}
}

// 4 or 6 for an address, 0 for an invalid one.
func addressIpVersion(ip netip.Addr) int {
	switch {
	case ip.Is4() || ip.Is4In6():
		return 4
	case ip.Is6():
		return 6
	default:
		return 0
	}
}

// The time from `t` to `now` as the directory's policy counts it: the longer of
// what the monotonic clock and the wall clock say. Go compares two readings of
// time.Now by their monotonic parts alone, and the monotonic clock stops while
// the host sleeps (mach_absolute_time on darwin, CLOCK_MONOTONIC on linux and
// android), so alone it would not count a sleep; the wall clock alone can be
// set back, which would make what came before it young again. The longer of
// the two counts a sleep and outlasts a clock set back. A time with no
// monotonic reading -- one loaded from the store, or a fake clock's -- is
// counted by the wall clock alone, as it always was.
func extenderElapsed(now time.Time, t time.Time) time.Duration {
	return max(now.Sub(t), now.Round(0).Sub(t.Round(0)))
}

// Whether `now` is before `t` by both clocks: what was set to last until `t`,
// a hold or a limit, has lapsed once either clock reaches it, for the reasons
// of extenderElapsed. A zero `t`, nothing set, is never in force.
func extenderBefore(now time.Time, t time.Time) bool {
	return now.Before(t) && now.Round(0).Before(t.Round(0))
}

func extenderTimeMs(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func extenderTimeFromMs(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}
