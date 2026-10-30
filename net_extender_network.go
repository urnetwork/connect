package connect

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	mathrand "math/rand"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/connect/protocol"
	"google.golang.org/protobuf/proto"
)

// The extender network client (EXTENDER.md E3).
//
// One instance serves one network space. It owns everything that fills the
// directory from outside: the dns bootstrap, the root key refresh from hello,
// and the feed connection that carries the sample and, in the feed role, the
// live subscription.
//
// The shape is one `run` loop started by the constructor. It bootstraps, takes
// a sample, and then either holds the subscription open or sleeps until the
// next tick; every failure path goes back through the same backoff. Four
// things wake it early: a network path change, a drop below the low-water
// mark, new root keys from hello, and `Close`. The hello, the operator's hint
// and the latency probe pass each run in a loop of their own beside it
// (`runHellos`, `runHints`, `runProbes`), so none holds up a pass.
//
// Every clock and every side effect is a settings seam -- `Now`, `ResolveDns`,
// `Hello` -- so the whole loop is deterministic in tests.

// The hello answer this client reads. Both fields are additive (B4, C7): an
// operator that has not configured root keys answers with none, which leaves
// the stored anchor alone, and one that does not serve a gossip identity yet
// answers with an empty id, which means there is no operator to dial (D3).
type ExtenderHelloResult struct {
	// Hex ed25519 root public keys, the trust anchor of every record (B4).
	RootPublicKeyHexes []string
	// The operator gossip node's libp2p peer id, empty when it has none (C6).
	GossipPeerId string
}

// The json shape of the hello fields this client reads.
type extenderHelloResultJson struct {
	ExtenderRootPublicKeys []string `json:"extender_root_public_keys"`
	GossipPeerId           string   `json:"gossip_peer_id"`
}

type ExtenderNetworkClientSettings struct {
	Log Logger

	// The dns name whose A and AAAA answers bootstrap the directory (F1).
	// Empty disables the bootstrap, which leaves the client on the stored
	// directory alone.
	ExtenderDnsName string
	// The api url whose /hello carries the root keys (B4). Empty disables the
	// refresh.
	ApiUrl string

	// Subscribe keeps the feed stream open after the sample and applies
	// everything the server pushes (D5, the feed role).
	Subscribe bool
	// Records asked for in one sample (D4).
	SampleCount int

	// Reconnect backoff after a failed feed attempt, doubling to the max.
	MinBackoff time.Duration
	MaxBackoff time.Duration
	// The dns bootstrap repeats on this period, and so does the hello refresh.
	RebootstrapTimeout time.Duration
	// A directory with fewer usable addresses than this re-bootstraps at once.
	LowWaterCount int
	// Budget of one feed dial and of one hello.
	DialTimeout  time.Duration
	HelloTimeout time.Duration
	// Longest an open subscription may be silent. The server keepalives an
	// idle subscription every 30 s (D4), so a longer silence is a stream that
	// is no longer there. Not named in E3; without it a black-holed
	// subscription would never reconnect, because nothing else ends the read.
	SubscribeIdleTimeout time.Duration

	// A hint read that failed is read again only once this backoff has
	// passed, and the backoff doubles with each further failure on the same
	// path up to HintMaxBackoff; a path change reads it again at once, and an
	// answer is read again on the rebootstrap period. An operator that a
	// direct read cannot reach on this path -- a whitelist-only network, a
	// blocked api -- does not answer at the next pass either, and every read
	// dials the operator's address directly (GetExtenderHint).
	HintMinBackoff time.Duration
	HintMaxBackoff time.Duration
	// The same for a hello that failed: read again once this backoff has
	// passed, doubling with each further failure on the same path up to
	// HelloMaxBackoff, and at once after a path change. An answer is read
	// again on the rebootstrap period, whatever the path, since neither the
	// root keys nor the gossip identity depend on it. Hello reads through the
	// client strategy, so on a network that routes only extenders it fails
	// until the directory has one that reaches the operator.
	HelloMinBackoff time.Duration
	HelloMaxBackoff time.Duration

	// The latency probe pass (DESIGNNOTES4.md §4). ProbeWindowCount is m:
	// the pass stops once this many usable extenders of a family measure
	// close enough. 0 disables probing.
	ProbeWindowCount int
	// ProbeCountPerExtender is n: the probes one extender gets in a pass, of
	// which the lowest rtt is kept.
	ProbeCountPerExtender int
	// The most extenders one pass probes, so a pass over a large directory
	// with nothing close still ends.
	ProbeMaxCandidateCount int
	// Budget of one probe.
	ProbeTimeout time.Duration
	// Close enough is within ProbeCloseFactor of the best rtt measured, or
	// under ProbeCloseFloor, whichever admits more, so a badly connected
	// region still fills its window.
	ProbeCloseFactor float64
	ProbeCloseFloor  time.Duration

	// A resume from a sleep of at least ResumeMinSleep is a path change for
	// measurement (DESIGNNOTES4.md §6): the samples taken before the sleep
	// stay in use, due a refresh, and the probe pass measures them again once
	// a sample has completed after it. Every timer here runs on the monotonic
	// clock, which stops while the host sleeps, so the probe loop reads the
	// host clock every ResumeCheckTimeout while it waits, and acts on a
	// resume once the host has stayed awake that long since the check that
	// saw the sleep (hostResumeWatch). The defaults, fifteen minutes and a
	// minute, are argued at defaultResumeMinSleep. <= 0 for either disables
	// it.
	ResumeCheckTimeout time.Duration
	ResumeMinSleep     time.Duration

	// ManualHosts are hostnames or ip literals configured by hand (K6). An ip
	// literal is added as a manual address at start; a hostname is resolved
	// through the resolver seam below at start and on every rebootstrap, and
	// its answers are added the same way. They supplement discovery: manual
	// addresses union with the dns bootstrap and with everything the feed and
	// the mesh deliver, and are never removed by policy.
	ManualHosts []string

	// The bootstrap resolution. Nil takes the DoH settings the strategy has in
	// force at each pass, which a user's bootstrap DoH servers replace on a
	// running strategy.
	DohSettings *DohSettings

	// The only clock this client reads. Tests install a fake one.
	Now func() time.Time
	// When set, replaces time.After as the wait between two passes of the
	// refresh loop. Tests read the wait the loop chose through it, and hold
	// the loop on it without a sleep.
	PassAfter func(wait time.Duration) <-chan time.Time
	// When set, replaces time.After as the probe loop's waits: the refresh
	// period between two passes, and each resume check while it waits
	// (ResumeCheckTimeout). Tests fire the checks through it, and hold the
	// loop on the period without a sleep.
	ProbeAfter func(wait time.Duration) <-chan time.Time
	// ResolveDns, when set, replaces the bootstrap resolution. Nil resolves A
	// and AAAA over DoH with the system resolver as the fallback (E3).
	ResolveDns func(ctx context.Context, name string) ([]netip.Addr, error)
	// ResolveDnsTxt, when set, replaces the bootstrap TXT resolution. Nil
	// resolves TXT over the strategy's DoH settings with the system resolver
	// as the fallback, the same way ResolveDns does for addresses.
	ResolveDnsTxt func(ctx context.Context, name string) ([]string, error)
	// Hello, when set, replaces the hello fetch. Nil reads /hello through the
	// client strategy.
	Hello func(ctx context.Context) (*ExtenderHelloResult, error)
	// IpVersionSupported, when set, replaces the host family probe. Nil uses
	// probeFamilySupport, which is what the strategy also dials by.
	IpVersionSupported func(ipVersion int) bool
	// Probe, when set, replaces the whole probe of one candidate
	// (DESIGNNOTES4.md): the carrier walk and every probe on it. Nil walks
	// the candidate's carriers in order with ProbeLatency. It returns the
	// lowest rtt and what became of the attestation, of which only
	// ExtenderPingCosigned marks the sample attested. It hands back no claim,
	// so nothing is reported for a probe it made.
	Probe func(
		ctx context.Context,
		candidate *ExtenderCandidate,
		attestor *ExtenderProbeAttestor,
	) (time.Duration, ExtenderPingOutcome, error)
	// ProbeLatency, when set, replaces one probe of one carrier inside the
	// carrier walk. Nil is ProbeExtenderLatency over the strategy's connect
	// settings. Tests drive the attestation and the verdict through it, and
	// what it returns is reported exactly as a real probe is.
	ProbeLatency func(
		ctx context.Context,
		extenderConfig *ExtenderConfig,
		attestor *ExtenderProbeAttestor,
	) (*ExtenderLatencyProbe, error)
	// When set, replaces the hint fetch. Nil reads /network/extender-hint
	// through direct dialers alone, built from the client strategy's settings
	// (GetExtenderHint). An empty field with no error is an operator that
	// cannot place the caller; an error is an operator that cannot be asked.
	Hint func(ctx context.Context) (*ExtenderHintResult, error)
}

func DefaultExtenderNetworkClientSettings() *ExtenderNetworkClientSettings {
	return &ExtenderNetworkClientSettings{
		Subscribe:              true,
		SampleCount:            DefaultExtenderFeedSampleCount,
		MinBackoff:             1 * time.Second,
		MaxBackoff:             5 * time.Minute,
		RebootstrapTimeout:     6 * time.Hour,
		LowWaterCount:          4,
		DialTimeout:            30 * time.Second,
		HelloTimeout:           30 * time.Second,
		SubscribeIdleTimeout:   90 * time.Second,
		HintMinBackoff:         1 * time.Minute,
		HintMaxBackoff:         6 * time.Hour,
		HelloMinBackoff:        1 * time.Minute,
		HelloMaxBackoff:        6 * time.Hour,
		ProbeWindowCount:       4,
		ProbeCountPerExtender:  2,
		ProbeMaxCandidateCount: 16,
		ProbeTimeout:           5 * time.Second,
		ProbeCloseFactor:       2.0,
		ProbeCloseFloor:        50 * time.Millisecond,
		ResumeCheckTimeout:     defaultResumeCheckTimeout,
		ResumeMinSleep:         defaultResumeMinSleep,
		Now:                    time.Now,
	}
}

// What the sdk status reports (E3, F2). Comparable, so it rides a
// MonitorValue and a consumer is woken only on an actual change.
type ExtenderNetworkClientStatus struct {
	FeedConnected bool
	// True while a sample or subscribe dial is in flight and no stream is up
	// yet, which is the app's yellow connecting state (K4).
	Connecting bool
	FeedIp     netip.Addr
	// The time of the last completed sample, zero when there has been none.
	LastSampleTime time.Time
	LastError      string
	// True once the first attempt has finished, whether or not it produced a
	// sample. The startup gate reads the same fact from the directory.
	InitialAttemptDone bool
	// The operator gossip node's peer id as hello last carried it, empty until
	// the operator serves one (C6, D3). The member role's node dials the
	// operator only once this is known.
	GossipPeerId string
	// The continent the candidate order prefers: the operator's hint, else
	// the one inferred from the dns bootstrap; empty when neither has said
	// (DESIGNNOTES4.md §4).
	ContinentHint string
	// When the last probe pass that measured something ended, zero when
	// there has been none.
	LastProbeTime time.Time
}

// The state of the gossip network as the app's status dot shows it (K4, K5).
// The two roles read different evidence -- a feed app has a stream, a member
// has a mesh -- so the derivation lives here, once, rather than in each app.
const (
	ExtenderGossipStateConnected    = "connected"
	ExtenderGossipStateConnecting   = "connecting"
	ExtenderGossipStateDisconnected = "disconnected"
)

// The feed role's state: green while the stream is up, yellow while a dial is
// in flight, red otherwise -- backoff, no candidate, or disabled.
func ExtenderGossipStateForFeed(status ExtenderNetworkClientStatus) string {
	switch {
	case status.FeedConnected:
		return ExtenderGossipStateConnected
	case status.Connecting:
		return ExtenderGossipStateConnecting
	default:
		return ExtenderGossipStateDisconnected
	}
}

// The member role's state, from a gossip node status: green with at least one
// mesh peer, yellow while a peering round has dials in flight. The node status
// is passed as its two fields rather than as the value, because connect root
// cannot import its own gossip subpackage.
func ExtenderGossipStateForMember(meshPeerCount int, connecting bool) string {
	switch {
	case 0 < meshPeerCount:
		return ExtenderGossipStateConnected
	case connecting:
		return ExtenderGossipStateConnecting
	default:
		return ExtenderGossipStateDisconnected
	}
}

type ExtenderNetworkClient struct {
	ctx       context.Context
	cancel    context.CancelFunc
	closeOnce sync.Once
	done      chan struct{}
	log       Logger

	clientStrategy *ClientStrategy
	directory      *ExtenderDirectory
	settings       *ExtenderNetworkClientSettings

	statusMonitor *MonitorValue[ExtenderNetworkClientStatus]
	// closed and replaced on a wake request; a network change and a low-water
	// drop both take this path
	wakeMonitor        *Monitor
	unsubNetworkChange func()

	stateLock sync.Mutex
	// the open subscription, so a network change can end it at once rather
	// than leaving the loop parked on a stream bound to the old path
	feedStream *ExtenderFeedStream
	// when it opened, so a resume ends it only when it predates the sleep
	// (hostResumed)
	feedStreamTime time.Time
	// the manually configured hosts and the version that changes with them,
	// which is what makes `SetManualHosts` re-resolve at once rather than at
	// the next tick (K6)
	manualHosts        []string
	manualHostsVersion uint64

	// the probe pass goroutine's join and wake (DESIGNNOTES4.md §4)
	probeDone chan struct{}
	probeWake *Monitor
	// closed by run after its first bootstrap/manual readiness and expiry,
	// before sampling; early hints must not probe a partial bootstrap set
	initialProbeReady chan struct{}
	// the hint loop's join and wake (DESIGNNOTES4.md §4). The hello loop
	// waits on the same wake: both look whether their read is due at each
	// pass and at a path change, so whatever wakes the one wakes the other,
	// even in a client built bare, without the hello loop's fields
	hintDone chan struct{}
	hintWake *Monitor
	// closed by the hint loop once its first read has ended, answered or
	// not; the first probe pass waits for it too, so the operator's
	// continent is probed first
	initialHintDone chan struct{}
	// the hello loop's join; it waits on hintWake
	helloDone chan struct{}
	// closed by the hello loop once its first read has ended, answered or
	// not; a pass with no root keys in force waits for it, so it bootstraps
	// and samples under the keys hello brings, as when hello was read ahead
	// of it
	initialHelloDone chan struct{}
	// the attesting provider, nil for a client that only ranks. Installed
	// by the provider role and cleared when it stops.
	probeAttestor *ExtenderProbeAttestor
	// where the attesting provider's pings are reported, nil for nowhere
	// (GEOMAP §2.5)
	probeReporter *ExtenderPingReporter
	// set by a path change: the hint placed the address of the old path, and
	// a failure on the old path says nothing about the new one, so the hint
	// loop reads it again at once rather than waiting out the refresh period
	// or a failure's backoff
	hintRearmed bool
	// set by a path change too: a hello that failed on the old path is read
	// again at once rather than waiting out its backoff
	helloRearmed bool

	// Resolver publications share this client's lifetime, never process state.
	// stateLock guards registration and closure before dnsWorkers is joined.
	dnsPublicationKVs map[string]*extenderDnsPublication
	dnsWorkers        sync.WaitGroup
	dnsClosed         bool

	// guards the continent hint's decision: the hint loop's operator answer
	// and the bootstrap's dns inference decide side by side, and an inference
	// must not be decided after the operator's answer (setContinentHint)
	continentLock sync.Mutex
	// true once the operator's hint has been decided, which the dns
	// inference then defers to
	operatorHintApplied bool
	// the continent last decided, the source that decided it, and a version
	// that every decision advances
	continentHintCode    string
	continentHintSource  string
	continentHintVersion uint64
	// test seam only, nil otherwise: runs between reading a decision and
	// setting the directory to it, where a newer decision can land
	// (setContinentHint)
	continentHintSetHook func()
}

// The client is running when this returns: the directory has been told a first
// attempt is in flight, and the loop is up.
func NewExtenderNetworkClient(
	ctx context.Context,
	clientStrategy *ClientStrategy,
	directory *ExtenderDirectory,
	settings *ExtenderNetworkClientSettings,
) *ExtenderNetworkClient {
	if settings == nil {
		settings = DefaultExtenderNetworkClientSettings()
	}
	if settings.Now == nil {
		copied := *settings
		copied.Now = time.Now
		settings = &copied
	}
	cancelCtx, cancel := context.WithCancel(ctx)
	self := &ExtenderNetworkClient{
		ctx:               cancelCtx,
		cancel:            cancel,
		done:              make(chan struct{}),
		log:               loggerOrDefault(settings.Log),
		clientStrategy:    clientStrategy,
		directory:         directory,
		settings:          settings,
		statusMonitor:     NewMonitorValue[ExtenderNetworkClientStatus](ExtenderNetworkClientStatus{}),
		wakeMonitor:       NewMonitor(),
		manualHosts:       slices.Clone(settings.ManualHosts),
		probeDone:         make(chan struct{}),
		probeWake:         NewMonitor(),
		initialProbeReady: make(chan struct{}),
		hintDone:          make(chan struct{}),
		hintWake:          NewMonitor(),
		initialHintDone:   make(chan struct{}),
		helloDone:         make(chan struct{}),
		initialHelloDone:  make(chan struct{}),
	}
	directory.SetInitialSamplePending()
	// a path change invalidates the feed connection and the addresses that
	// were reachable on the old path
	self.unsubNetworkChange = AddNetworkChangeListener(self.networkChanged)
	go HandleError(func() {
		defer close(self.done)
		self.run()
	}, cancel)
	// hello has its own loop too: a pass verifies under the root keys in
	// force while a read is out, and where only extenders reach the operator
	// a read ahead of the bootstrap could not answer before the bootstrap
	// had found one. Only a pass with no keys in force waits, for the first
	// read alone (runHellos).
	go HandleError(func() {
		defer close(self.helloDone)
		self.runHellos()
	}, cancel)
	// the hint has its own loop: a pass needs nothing from its answer, and a
	// read the operator does not answer must not hold up the bootstrap and
	// the sample that follow it (DESIGNNOTES4.md §4)
	go HandleError(func() {
		defer close(self.hintDone)
		self.runHints()
	}, cancel)
	// the probe pass has its own loop: in the feed role the refresh loop is
	// parked on the subscription for as long as it lives, and records that
	// arrive over it must still be measured (DESIGNNOTES4.md §4)
	go HandleError(func() {
		defer close(self.probeDone)
		self.runProbes()
	}, cancel)
	return self
}

func (self *ExtenderNetworkClient) Status() ExtenderNetworkClientStatus {
	return self.statusMonitor.Value()
}

// The status value and a channel armed at the same instant, for a consumer
// that renders it.
func (self *ExtenderNetworkClient) StatusMonitor() *MonitorValue[ExtenderNetworkClientStatus] {
	return self.statusMonitor
}

// A path change invalidates the open subscription: it is bound to the old
// path, and the loop would otherwise sit on it until the idle timeout. The
// stream is closed with no lock held, as any external object is.
//
// It invalidates the hint's country too, which placed the address of the old
// path: the country goes stale at once, so the network country the host
// reports for the new path stands in until the operator answers for it, and
// the hint loop asks the operator again at once, whatever a failure on the
// old path had it waiting for. A hello that failed on the old path is read
// again at once as well.
//
// The extender evidence stays: holds, limits, failure counts and latency
// samples are kept, since an extender that answered, failed or was fast on
// the old path most likely is on the new one too, and learning it all again
// would cost dials and time after every network or link change. The samples
// measured the old path, so they are due a refresh (RefreshLatencies): the
// candidate order keeps ranking by them, and the probe pass that follows the
// first sample on the new path measures them again, each new sample replacing
// an old one as it lands.
func (self *ExtenderNetworkClient) networkChanged() {
	feedStream := func() *ExtenderFeedStream {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.hintRearmed = true
		self.helloRearmed = true
		return self.feedStream
	}()
	self.directory.ExpireCountryHint()
	self.directory.RefreshLatencies()
	if feedStream != nil {
		feedStream.Close()
	}
	// the hint and the hello loops share the wake
	self.hintWake.NotifyAll()
	self.wakeMonitor.NotifyAll()
}

// Reports whether a path change asked for the hint again since the last call,
// and clears the request.
func (self *ExtenderNetworkClient) takeHintRearmed() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	hintRearmed := self.hintRearmed
	self.hintRearmed = false
	return hintRearmed
}

// Publishes the open subscription so a network change can end it, with when
// it opened.
func (self *ExtenderNetworkClient) setFeedStream(feedStream *ExtenderFeedStream) {
	var feedStreamTime time.Time
	if feedStream != nil {
		feedStreamTime = self.settings.Now()
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.feedStream = feedStream
	self.feedStreamTime = feedStreamTime
}

// Ends the loop and joins it.
func (self *ExtenderNetworkClient) Close() {
	self.closeOnce.Do(func() {
		if self.unsubNetworkChange != nil {
			self.unsubNetworkChange()
		}
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.dnsClosed = true
		}()
		self.cancel()
		<-self.done
		<-self.probeDone
		<-self.hintDone
		<-self.helloDone
		self.dnsWorkers.Wait()
	})
}

// Releases the startup gate (E4). It is called as soon as the first sample
// completes -- not when the pass ends -- because in the feed role the pass
// lasts as long as the subscription, and a cold start must not wait out the
// gate timeout after the sample it was waiting for has already landed.
func (self *ExtenderNetworkClient) markInitialAttemptDone() {
	self.directory.SetInitialSampleDone()
	self.updateStatus(func(status *ExtenderNetworkClientStatus) {
		status.InitialAttemptDone = true
	})
}

func (self *ExtenderNetworkClient) updateStatus(update func(*ExtenderNetworkClientStatus)) {
	self.statusMonitor.Update(func(status ExtenderNetworkClientStatus) ExtenderNetworkClientStatus {
		update(&status)
		return status
	})
}

// The refresh loop. One pass bootstraps when it is due and then takes a
// sample, holding the subscription for the feed role. Every exit from a pass
// goes through the backoff, which a success resets. A pass that finds no root
// keys in force first waits for the first hello read (runHellos).
func (self *ExtenderNetworkClient) run() {
	backoff := self.settings.MinBackoff
	initialProbeReady := self.initialProbeReady
	var lastBootstrapTime time.Time
	// the root keys the last bootstrap judged its TXT records under. Keys
	// that hello installs after it make the bootstrap due at the next pass,
	// so the records the old keys refused, or that waited for keys, are
	// judged under the new ones (refreshRootKeys)
	var bootstrapRootKeySet *ExtenderRootKeySet
	var lastManualTime time.Time
	// the manual host list this loop has already applied; a reconfiguration
	// changes the version and re-resolves at once (K6)
	var manualHostsVersion uint64

	for {
		select {
		case <-self.ctx.Done():
			return
		default:
		}

		// with no root keys in force nothing the pass applies can verify, so
		// it waits for the first hello read, as when hello was read ahead of
		// it, and for no read after that one (runHellos). With keys in force
		// it never waits.
		if self.directory.RootKeys().Len() == 0 {
			select {
			case <-self.ctx.Done():
				return
			case <-self.initialHelloDone:
			}
		}

		now := self.settings.Now()
		// subscribe before the reads below, so a wake that lands while this
		// pass runs is carried into the next wait instead of being lost. Keys
		// that the first hello read installed before this are already in
		// force for the pass, and wake no second one.
		wake := self.wakeMonitor.NotifyChannel()

		// each pass has the hello and hint loops look whether their read is
		// due, and goes on without waiting for either (runHellos, runHints);
		// they share the wake
		self.hintWake.NotifyAll()
		if lastBootstrapTime.IsZero() ||
			self.settings.RebootstrapTimeout <= now.Sub(lastBootstrapTime) ||
			self.directory.ActiveCount(0) < self.settings.LowWaterCount ||
			self.directory.RootKeys() != bootstrapRootKeySet {
			bootstrapRootKeySet = self.bootstrap()
			lastBootstrapTime = now
		}
		if version := self.manualHostsVersionValue(); lastManualTime.IsZero() ||
			version != manualHostsVersion ||
			self.settings.RebootstrapTimeout <= now.Sub(lastManualTime) {
			manualHostsVersion = self.applyManualHosts()
			lastManualTime = now
		}
		self.directory.Expire(self.settings.Now())
		if initialProbeReady != nil {
			// Bootstrap/manual discovery has reached first readiness or
			// terminal failure. Its progressive DNS tail remains live; do not
			// wait for that tail or for a possibly long-lived feed sample.
			close(initialProbeReady)
			initialProbeReady = nil
		}

		// a subscription holds inside this call for as long as it lives; the
		// first attempt is marked done from inside, as soon as the sample
		// completes
		passStartTime := self.settings.Now()
		sampled, limitedUntil := self.sample()
		// the first attempt is complete either way; the startup gate must not
		// wait on an attempt that has already failed
		self.markInitialAttemptDone()

		var wait time.Duration
		switch {
		case sampled && !self.settings.Subscribe:
			// a one-shot sample; nothing to do until the next refresh
			backoff = self.settings.MinBackoff
			wait = self.settings.RebootstrapTimeout
		case sampled:
			// the subscription ended, whatever ended it, so the next pass
			// reconnects through another candidate on the backoff. The backoff
			// resets only when the stream stayed up: an extender that accepts,
			// serves a sample and drops at once would otherwise be redialed
			// once a second forever, since a served sample is never a failure
			// the directory would hold it for.
			if self.settings.MaxBackoff <= self.settings.Now().Sub(passStartTime) {
				backoff = self.settings.MinBackoff
			}
			wait = backoff
			backoff = min(2*backoff, self.settings.MaxBackoff)
		case !limitedUntil.IsZero():
			// every candidate is limited: wait for the first backoff to pass
			// rather than dialing again at once (A12)
			wait = max(backoff, limitedUntil.Sub(self.settings.Now()))
		default:
			wait = backoff
			backoff = min(2*backoff, self.settings.MaxBackoff)
		}
		if wait <= 0 {
			wait = self.settings.MinBackoff
		}

		passAfter := self.settings.PassAfter
		if passAfter == nil {
			passAfter = time.After
		}
		select {
		case <-self.ctx.Done():
			return
		case <-wake:
		case <-passAfter(wait):
		}
	}
}

// The hello loop (B4, C7). Hello is read beside the refresh pass, never ahead
// of it: a read that cannot answer -- where only extenders reach the
// operator, it fails until the bootstrap has found one -- must not hold up
// the bootstrap that would find it.
//
// What a pass does with the root keys is apply signed messages -- the
// bootstrap's TXT records, the feed's records and revocations -- and each is
// verified as it is applied, under the keys in force then: the space's
// configured or bundled keys, which are installed before the client starts,
// until hello answers, and hello's after. Nothing else in a pass needs them:
// the address answers stay unverified until a record names them, manual
// hosts are trusted by configuration, and a candidate is dialed only once it
// was verified, or because it is manual. A pass judged under the keys in
// force whenever hello failed ahead of it, and it does so too while a read
// is out. Where no keys are in force nothing can verify, so a pass waits for
// the first read, as long as it lasts (run, initialHelloDone); after a read
// that brought none, the bootstrap's TXT records wait for the first keys
// (bootstrap). Keys that hello installs judge again what was judged under
// the old ones (refreshRootKeys).
//
// The loop has no clock of its own. It looks whether hello is due at its
// start, at each pass and at a path change -- it waits on the hint loop's
// wake, which takes exactly those -- which is when hello was read as part of
// the pass. A failed read waits out its backoff (extenderReadSchedule), so a
// refresh loop that passes after every feed drop does not read it at each
// one, and a path change clears the backoff. One read is out at a time.
func (self *ExtenderNetworkClient) runHellos() {
	initialHelloDone := self.initialHelloDone
	helloSchedule := newExtenderReadSchedule(
		self.settings.RebootstrapTimeout,
		self.settings.HelloMinBackoff,
		self.settings.HelloMaxBackoff,
	)
	for {
		// subscribe before the read, so a pass or a path change that lands
		// while it runs is carried into the next wait instead of being lost;
		// the wake is the hint loop's, which takes the same events
		wake := self.hintWake.NotifyChannel()
		// a path change since the last look asks for a failed hello at once
		helloRearmed := func() bool {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			helloRearmed := self.helloRearmed
			self.helloRearmed = false
			return helloRearmed
		}()
		if helloRearmed {
			helloSchedule.ClearBackoff()
		}
		if helloSchedule.Due(self.settings.Now()) {
			if self.refreshRootKeys() {
				helloSchedule.Answer(self.settings.Now())
			} else {
				helloSchedule.Fail(self.settings.Now())
			}
		}
		if initialHelloDone != nil {
			// after any keys the read installed, so a pass that waited for
			// it finds them in force
			close(initialHelloDone)
			initialHelloDone = nil
		}
		select {
		case <-self.ctx.Done():
			return
		case <-wake:
		}
	}
}

// Reads hello and applies what it carries (B4, C7). An empty root key list
// leaves the stored anchor alone, which is what an operator that has not
// configured keys yet answers; the gossip peer id is published on the status
// either way, once the keys it came with are in force, so the member role's
// node learns the operator as soon as the operator serves one. Reports whether
// hello answered.
//
// Keys that are not the keys in force replace them. The directory judges
// everything it holds again under them (SetRootKeys), including a message
// whose verification under the old keys was still out (applyVerifiedRecord),
// and the refresh loop is woken to take the steps that judged signed messages
// under the old keys again: the bootstrap, which is due once the keys in force
// are not the ones its TXT records were judged under (run), and the sample,
// since a pass dials no candidate it chose under the old keys after the
// change, and a stream to one ends once its sample is in (sample).
func (self *ExtenderNetworkClient) refreshRootKeys() bool {
	hello := self.settings.Hello
	if hello == nil {
		if self.settings.ApiUrl == "" || self.clientStrategy == nil {
			return false
		}
		hello = self.hello
	}
	ctx, cancel := context.WithTimeout(self.ctx, self.settings.HelloTimeout)
	defer cancel()
	helloResult, err := hello(ctx)
	if err != nil {
		self.log.Infof("[extender]hello err = %s\n", err)
		return false
	}
	if helloResult == nil {
		return true
	}
	answered := true
	// a list of blank entries is as empty as no list: installed, it would
	// refuse every record until the next answer
	if keySet, err := NewExtenderRootKeySetFromHex(helloResult.RootPublicKeyHexes...); err != nil {
		self.log.Infof("[extender]hello root keys err = %s\n", err)
		answered = false
	} else if 0 < keySet.Len() && !keySet.Equal(self.directory.RootKeys()) {
		self.directory.SetRootKeys(keySet)
		self.log.Infof("[extender]hello root keys installed (%d)\n", keySet.Len())
		self.wakeMonitor.NotifyAll()
	}
	self.updateStatus(func(status *ExtenderNetworkClientStatus) {
		status.GossipPeerId = helloResult.GossipPeerId
	})
	return answered
}

func (self *ExtenderNetworkClient) hello(ctx context.Context) (*ExtenderHelloResult, error) {
	request, err := HelloRequestFromUrl(ctx, self.settings.ApiUrl, "")
	if err != nil {
		return nil, err
	}
	bodyBytes, err := HttpGetWithStrategyRaw(ctx, self.clientStrategy, request.URL.String(), "")
	if err != nil {
		return nil, err
	}
	helloResultJson := &extenderHelloResultJson{}
	if err := json.Unmarshal(bodyBytes, helloResultJson); err != nil {
		return nil, err
	}
	return &ExtenderHelloResult{
		RootPublicKeyHexes: helloResultJson.ExtenderRootPublicKeys,
		GossipPeerId:       helloResultJson.GossipPeerId,
	}, nil
}

// Replaces the manually configured hosts and re-resolves them at once (K6).
// The addresses the previous list produced stay in the directory: a manual
// address is only removed by the directory being rebuilt, which is what
// saving the setting does.
func (self *ExtenderNetworkClient) SetManualHosts(hosts []string) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.manualHosts = slices.Clone(hosts)
		self.manualHostsVersion += 1
	}()
	// the resolution runs here rather than only at the next pass: in the feed
	// role the loop is parked on a live subscription for as long as it lasts,
	// and a reconfiguration must not wait that out. It is bounded by the
	// client context, and the loop's own apply of the same version is
	// idempotent, so the overlap costs at most one resolution.
	if self.startDnsWorker() {
		go HandleError(func() {
			defer self.dnsWorkers.Done()
			self.applyManualHosts()
		})
	}
	self.wakeMonitor.NotifyAll()
}

// The configured hosts and the version they are at, read together so the loop
// records exactly the version it applied.
func (self *ExtenderNetworkClient) manualHostsValue() ([]string, uint64) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.manualHosts), self.manualHostsVersion
}

func (self *ExtenderNetworkClient) manualHostsVersionValue() uint64 {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.manualHostsVersion
}

// Adds the manually configured hosts (K6). An ip literal is added as it
// stands; a name is resolved through the same seam the dns bootstrap uses, so
// a host that configured DoH resolves manual hosts over DoH too. Every answer
// becomes a manual address, which the removal policy never takes away, and
// unions with the dns bootstrap and with everything the feed and the mesh
// deliver. Returns after the first usable publication; the client owns the tail.
func (self *ExtenderNetworkClient) applyManualHosts() uint64 {
	return self.applyManualHostsProgressive()
}

// Resolves the extender dns name (E3).
//
// The TXT answers come first. Each is a signed record, the same bytes the
// operator gossips, and applying it verifies it under the root keys and lands
// its addresses already verified. Only then are the A and AAAA answers added,
// as unverified addresses with source dns, which is a no-op for an address a
// record just named. The order is what makes dns a verified bootstrap rather
// than an unverified one: dns itself is not trusted, the operator's signature
// is, and dns is merely where it was fetched from.
//
// A TXT answer that does not verify is dropped and logged, never applied: a
// poisoned resolver can hand out any bytes it likes, and the whole point is
// that only the root key decides what counts.
//
// The records are judged under the root keys in force. With none in force no
// record can verify, and one refused now would not be offered again until the
// next bootstrap, so the TXT answers wait for keys and none is resolved.
// Returns the keys read before any record was judged: keys that hello installs
// after that make the bootstrap due again (run).
func (self *ExtenderNetworkClient) bootstrap() *ExtenderRootKeySet {
	if self.settings.ExtenderDnsName == "" {
		return self.directory.RootKeys()
	}
	resolveTxt := self.settings.ResolveDnsTxt
	if resolveTxt == nil {
		resolveTxt = self.resolveDnsTxt
	}
	// the resolution shares the hello budget: both are one short name-service
	// round trip before anything can be dialed
	ctx, cancel := context.WithTimeout(self.ctx, self.settings.HelloTimeout)
	defer cancel()

	rootKeySet := self.directory.RootKeys()
	var txts []string
	if rootKeySet.Len() == 0 {
		self.log.V(1).Infof("[extender]bootstrap txt waits for root keys\n")
	} else {
		var err error
		txts, err = resolveTxt(ctx, self.settings.ExtenderDnsName)
		if err != nil {
			// not fatal: the address answers may still bootstrap, unverified
			self.log.Infof("[extender]bootstrap txt err = %s\n", err)
		}
		// keys installed while the answer was out judge it
		rootKeySet = self.directory.RootKeys()
	}
	applied := 0
	// the continents of the records that verified: the geo dns answered the
	// set of the caller's continent, so they are the caller's continent as
	// the operator judges it (DESIGNNOTES4.md §4)
	continentCounts := map[string]int{}
	for _, txt := range txts {
		message, err := DecodeExtenderDnsRecord(txt)
		if err != nil {
			self.log.Infof("[extender]bootstrap txt record ignored: %s\n", err)
			continue
		}
		if _, err := self.directory.ApplySource(message, ExtenderSourceDns); err != nil {
			self.log.Infof("[extender]bootstrap txt record refused: %s\n", err)
			continue
		}
		applied += 1
		if continentCode := extenderRecordContinentCode(message); continentCode != "" {
			continentCounts[continentCode] += 1
		}
	}
	if 0 < len(txts) {
		self.log.Infof("[extender]bootstrap applied %d of %d txt records\n", applied, len(txts))
	}
	self.inferContinentHint(continentCounts)

	// The TXT trust/hint step is complete. Address families continue under
	// this client's joined owner; one usable publication releases sampling.
	self.bootstrapDnsAddresses(ctx)
	return rootKeySet
}

// The default bootstrap TXT resolution: over the strategy's DoH settings in
// force, with the system resolver as the fallback when DoH yields nothing,
// mirroring resolveDns.
func (self *ExtenderNetworkClient) resolveDnsTxt(
	ctx context.Context,
	name string,
) ([]string, error) {
	dohSettings := self.settings.DohSettings
	if dohSettings == nil && self.clientStrategy != nil {
		dohSettings = self.clientStrategy.DohSettings()
	}
	if dohSettings != nil {
		if txts := DohQueryTxt(ctx, dohSettings, name); 0 < len(txts) {
			return txts, nil
		}
	}
	var customResolver *net.Resolver
	if self.clientStrategy != nil {
		customResolver = self.clientStrategy.settings.ConnectSettings.Resolver
	}
	return dialResolver(customResolver).LookupTXT(ctx, name)
}

// DecodeExtenderDnsRecord decodes one TXT value of the extender dns name: the
// base64 of a serialized ExtenderGossipMessage, exactly the bytes the operator
// gossips. Decoding is all this does; the signature is checked where the
// message is applied, under the root keys the directory holds.
func DecodeExtenderDnsRecord(txt string) (*protocol.ExtenderGossipMessage, error) {
	txt = strings.TrimSpace(txt)
	if txt == "" {
		return nil, fmt.Errorf("extender dns record is empty")
	}
	data, err := base64.StdEncoding.DecodeString(txt)
	if err != nil {
		return nil, fmt.Errorf("extender dns record is not base64: %w", err)
	}
	message := &protocol.ExtenderGossipMessage{}
	if err := proto.Unmarshal(data, message); err != nil {
		return nil, fmt.Errorf("extender dns record is not a gossip message: %w", err)
	}
	return message, nil
}

// EncodeExtenderDnsRecord is the inverse, for the operator's publisher and for
// tests: the TXT value that carries one gossip message.
func EncodeExtenderDnsRecord(message *protocol.ExtenderGossipMessage) (string, error) {
	data, err := proto.Marshal(message)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(data), nil
}

// The default bootstrap resolution: A and AAAA over the strategy's DoH
// settings, with the system resolver as the fallback when DoH yields nothing.
func (self *ExtenderNetworkClient) resolveDns(
	ctx context.Context,
	name string,
) ([]netip.Addr, error) {
	return self.resolveDnsProgress(ctx, name, nil)
}

func (self *ExtenderNetworkClient) ipVersionSupported(ipVersion int) bool {
	if self.settings.IpVersionSupported != nil {
		return self.settings.IpVersionSupported(ipVersion)
	}
	return probeFamilySupport(ipVersion)
}

// Takes one sample from the best candidate that answers, applying every frame.
// It reports whether a sample completed, and when nothing was dialed because
// every candidate is limited, the earliest time one stops being (A12). In the
// feed role the same stream is then read until it ends, which is what makes a
// pass long lived.
//
// The candidates are chosen under the root keys in force. Keys that hello
// installs during the pass may no longer vouch for them, so no candidate is
// dialed after the install (sampleCandidate), and a stream opened to one ends
// once its sample is in (runFeed). The install woke the refresh loop, whose
// next pass chooses under the new keys.
func (self *ExtenderNetworkClient) sample() (bool, time.Time) {
	candidateRootKeySet := self.directory.RootKeys()
	candidates, limitedUntil := self.feedCandidates()
	if len(candidates) == 0 {
		// nothing to dial is not connecting, it is disconnected (K4)
		lastError := "no extender candidate"
		if !limitedUntil.IsZero() {
			lastError = "every extender candidate is limited"
		}
		self.updateStatus(func(status *ExtenderNetworkClientStatus) {
			status.FeedConnected = false
			status.Connecting = false
			status.FeedIp = netip.Addr{}
			status.LastError = lastError
		})
		return false, limitedUntil
	}

	// the whole pass is the connecting state, from the first dial to the last
	// candidate; a dial that connects clears it inside `runFeed`, so a live
	// subscription never reads as connecting (K4)
	self.updateStatus(func(status *ExtenderNetworkClientStatus) {
		status.Connecting = true
	})
	defer self.updateStatus(func(status *ExtenderNetworkClientStatus) {
		status.Connecting = false
	})

	for _, candidate := range candidates {
		select {
		case <-self.ctx.Done():
			return false, time.Time{}
		default:
		}
		sampled, err := self.sampleCandidate(candidate, candidateRootKeySet)
		if sampled {
			return true, time.Time{}
		}
		if err != nil {
			self.log.Infof("[extender]feed %s err = %s\n", candidate.Ip, err)
			self.updateStatus(func(status *ExtenderNetworkClientStatus) {
				status.FeedConnected = false
				status.FeedIp = netip.Addr{}
				status.LastError = err.Error()
			})
		}
	}
	return false, time.Time{}
}

// Every candidate of a family this host has, verified first (E3).
func (self *ExtenderNetworkClient) candidates() []*ExtenderCandidate {
	candidates := []*ExtenderCandidate{}
	for _, ipVersion := range []int{4, 6} {
		if !self.ipVersionSupported(ipVersion) {
			continue
		}
		candidates = append(
			candidates,
			self.directory.Candidates(ipVersion, self.settings.LowWaterCount+1)...,
		)
	}
	return candidates
}

// The candidates the feed dials: every candidate but the limited ones (A12),
// and the earliest time one of those stops being limited, zero when none is.
func (self *ExtenderNetworkClient) feedCandidates() ([]*ExtenderCandidate, time.Time) {
	candidates := []*ExtenderCandidate{}
	var limitedUntil time.Time
	for _, candidate := range self.candidates() {
		// the directory sets the time only while the address is limited
		if !candidate.LimitedUntil.IsZero() {
			if limitedUntil.IsZero() || candidate.LimitedUntil.Before(limitedUntil) {
				limitedUntil = candidate.LimitedUntil
			}
			continue
		}
		candidates = append(candidates, candidate)
	}
	return candidates, limitedUntil
}

// Tries the carriers of one candidate in order -- tcp, then quic, then dns --
// and runs the feed on the first that answers. `candidateRootKeySet` is the
// root keys the candidate was chosen under: once other keys are in force, no
// carrier is dialed, and nothing is held against the candidate for it.
func (self *ExtenderNetworkClient) sampleCandidate(
	candidate *ExtenderCandidate,
	candidateRootKeySet *ExtenderRootKeySet,
) (sampled bool, resultErr error) {
	connectSettings := DefaultConnectSettings()
	if self.clientStrategy != nil {
		connectSettings = &self.clientStrategy.settings.ConnectSettings
	}
	spoofDomains, _ := directorySpoofDomains(self.directory)
	for _, carrier := range orderedExtenderCarriers(candidate.Carriers) {
		select {
		case <-self.ctx.Done():
			return false, nil
		default:
		}
		if self.directory.RootKeys() != candidateRootKeySet {
			return false, nil
		}
		connectMode, ok := ExtenderConnectModeForCarrier(carrier)
		if !ok {
			continue
		}
		extenderConfig := extenderFeedConfig(candidate, connectMode, spoofDomains)
		if extenderConfig == nil {
			continue
		}
		sampled, err := self.runFeed(connectSettings, extenderConfig, candidateRootKeySet)
		if sampled {
			return true, nil
		}
		var limitedErr *ExtenderLimitedError
		if errors.As(err, &limitedErr) {
			// over the extender's admission limits (A12): a backoff, never a
			// failure, and its other carriers are limited the same way
			self.directory.RecordLimited(candidate.Ip, limitedErr.RetryAfter)
			return false, err
		}
		resultErr = err
		self.directory.RecordFailure(candidate.Ip, connectMode)
	}
	return false, resultErr
}

// Opens the feed, applies the sample, and in the feed role keeps applying
// until the stream ends. It reports whether the sample completed.
//
// Root keys other than `candidateRootKeySet`, the keys the extender was
// chosen under, judge every frame after them, and end the stream once its
// sample is in, whether hello installed them during the dial or after it: the
// next pass, which the install woke, samples under them, so the records the
// old keys refused are judged again, and the stream does not stay with an
// extender the new keys may not vouch for (refreshRootKeys).
func (self *ExtenderNetworkClient) runFeed(
	connectSettings *ConnectSettings,
	extenderConfig *ExtenderConfig,
	candidateRootKeySet *ExtenderRootKeySet,
) (sampled bool, resultErr error) {
	dialCtx, dialCancel := context.WithTimeout(self.ctx, self.settings.DialTimeout)
	defer dialCancel()
	stream, err := DialExtenderFeed(
		dialCtx,
		connectSettings,
		extenderConfig,
		&protocol.ExtenderFeedRequest{
			SampleCount: uint32(self.settings.SampleCount),
			Subscribe:   self.settings.Subscribe,
		},
	)
	if err != nil {
		return false, err
	}
	defer stream.Close()
	self.setFeedStream(stream)
	defer self.setFeedStream(nil)

	self.directory.RecordSuccess(extenderConfig.Ip, extenderConfig.Profile.ConnectMode)
	self.directory.SetInUse(extenderConfig.Ip, 1)
	defer self.directory.SetInUse(extenderConfig.Ip, -1)
	self.updateStatus(func(status *ExtenderNetworkClientStatus) {
		status.FeedConnected = true
		status.Connecting = false
		status.FeedIp = extenderConfig.Ip
		status.LastError = ""
	})
	defer self.updateStatus(func(status *ExtenderNetworkClientStatus) {
		status.FeedConnected = false
		status.FeedIp = netip.Addr{}
	})

	// the sample is bounded by the dial budget; the subscription that follows
	// is bounded only by the client lifetime and the server's keepalive
	sampleCtx, sampleCancel := context.WithTimeout(self.ctx, self.settings.DialTimeout)
	defer sampleCancel()
	for {
		readCtx := sampleCtx
		var readCancel context.CancelFunc
		if sampled {
			// each subscribed read is bounded by the idle timeout, so a
			// subscription that went silent ends instead of parking the loop.
			// A zero timeout disables the bound rather than expiring at once.
			readCtx = self.ctx
			if 0 < self.settings.SubscribeIdleTimeout {
				readCtx, readCancel = context.WithTimeout(self.ctx, self.settings.SubscribeIdleTimeout)
			}
		}
		frame, err := stream.Next(readCtx)
		if readCancel != nil {
			readCancel()
		}
		if err != nil {
			if sampled {
				return true, nil
			}
			return false, err
		}
		switch {
		case frame.GetRecord() != nil:
			if _, err := self.directory.ApplyRecord(frame.GetRecord(), ExtenderSourceFeed); err != nil {
				self.log.Infof("[extender]feed record err = %s\n", err)
			}
		case frame.GetRevocation() != nil:
			if _, err := self.directory.ApplyRevocation(frame.GetRevocation()); err != nil {
				self.log.Infof("[extender]feed revocation err = %s\n", err)
			}
		case frame.GetEndOfSample():
			sampled = true
			sampleTime := self.settings.Now()
			self.updateStatus(func(status *ExtenderNetworkClientStatus) {
				status.LastSampleTime = sampleTime
				status.LastError = ""
			})
			self.markInitialAttemptDone()
			// the sample is in the directory; measure it (DESIGNNOTES4.md)
			self.probeWake.NotifyAll()
			if !self.settings.Subscribe {
				return true, nil
			}
		case frame.GetKeepalive():
			// an idle subscription is still alive; nothing to apply
		}
		if sampled && self.directory.RootKeys() != candidateRootKeySet {
			return true, nil
		}
	}
}

// The carrier order of E3: tcp, then quic, then dns. A carrier the record does
// not list is not tried.
func orderedExtenderCarriers(carriers []string) []string {
	ordered := []string{}
	// the webrtc carrier last: it is the least ordinary looking traffic and
	// needs a signaling path the others do not (EXTENDER.md S)
	for _, carrier := range []string{ExtenderCarrierTcp, ExtenderCarrierQuic, ExtenderCarrierDns, ExtenderCarrierWebRtc} {
		for _, candidateCarrier := range carriers {
			if candidateCarrier == carrier {
				ordered = append(ordered, carrier)
				break
			}
		}
	}
	return ordered
}

// The dial configuration of one candidate carrier. The outer name is one
// random name of the spoof list in force (A10, directorySpoofDomains); with an
// empty list the extender ip is presented, which sends no sni at all rather
// than naming the destination -- a feed dial has no destination host. The dns
// carrier is one dial that races its ports (L2, net_extender_dns_ports.go),
// so the feed, the probe and the peer pinger try both 53 and 4053 as the
// strategy does.
func extenderFeedConfig(
	candidate *ExtenderCandidate,
	connectMode ExtenderConnectMode,
	spoofDomains []string,
) *ExtenderConfig {
	profile := ExtenderProfile{
		ConnectMode: connectMode,
		ServerName:  candidate.Ip.String(),
	}
	if 0 < len(spoofDomains) {
		profile.ServerName = spoofDomains[mathrand.Intn(len(spoofDomains))]
	}
	var dnsPorts []int
	switch connectMode {
	case ExtenderConnectModeQuic:
		profile.Port = candidate.UdpPort
	case ExtenderConnectModeDns:
		dnsPorts = candidate.dnsCarrierPorts()
		if 0 < len(dnsPorts) {
			profile.Port = dnsPorts[0]
		}
		profile.DnsTld = candidate.DnsTld
	default:
		profile.Port = candidate.TcpPort
	}
	if profile.Port <= 0 {
		return nil
	}
	return &ExtenderConfig{
		Profile:   profile,
		Ip:        candidate.Ip,
		PublicKey: candidate.PublicKey,
		DnsPorts:  dnsPorts,
	}
}

// The hint (DESIGNNOTES4.md §4).

// The hint loop. The hint is read beside the refresh pass, never ahead of it:
// nothing a pass does needs the answer -- while there is none the network
// country the host reports stands in for the operator's (SpoofCountryCode),
// and the operator's continent overrides the dns inference whenever it lands
// -- so a read the operator does not answer, as where only extenders reach it
// (a whitelist-only network, a blocked api), never holds up the bootstrap,
// the manual hosts or the sample. One read is in flight at a time.
//
// The loop has no clock of its own. It looks whether the hint is due at its
// start, at each pass, which wakes it, and at a path change, which is the
// cadence the hint was read at as part of the pass; extenderReadSchedule
// decides what is due, so a failed read is not repeated at every pass.
func (self *ExtenderNetworkClient) runHints() {
	initialHintDone := self.initialHintDone
	hintSchedule := newExtenderReadSchedule(
		self.settings.RebootstrapTimeout,
		self.settings.HintMinBackoff,
		self.settings.HintMaxBackoff,
	)
	for {
		// subscribe before the read, so a pass or a path change that lands
		// while it runs is carried into the next wait instead of being lost
		wake := self.hintWake.NotifyChannel()
		if self.takeHintRearmed() {
			hintSchedule.Rearm()
		}
		if hintSchedule.Due(self.settings.Now()) {
			if self.refreshHint() {
				hintSchedule.Answer(self.settings.Now())
			} else {
				hintSchedule.Fail(self.settings.Now())
			}
		}
		if initialHintDone != nil {
			close(initialHintDone)
			initialHintDone = nil
		}
		select {
		case <-self.ctx.Done():
			return
		case <-wake:
		}
	}
}

// When a read of the operator is due, the hint's (runHints) or hello's
// (runHellos). An answer is read again after the refresh timeout. A failure
// is read again only once its backoff has passed, and the backoff doubles
// with each further failure, up to the max: an operator that a read cannot
// reach on a path does not answer at the next pass either, and the backoff is
// what keeps a refresh loop that passes after every feed drop from reading at
// each one. A path change is a fresh start for a failure: the read is due at
// once, and a failure on the new path backs off from the minimum. For the
// hint, whose answer places the address of the path, it is a fresh start for
// an answer too (Rearm); hello's answer holds on any path (ClearBackoff).
//
// Only the loop that reads holds one, so it takes no lock.
type extenderReadSchedule struct {
	refreshTimeout time.Duration
	minBackoff     time.Duration
	maxBackoff     time.Duration

	// when the last read on this path ended, zero for none
	readTime time.Time
	// the wait after readTime: the refresh timeout after an answer, the
	// backoff after a failure
	wait time.Duration
	// zero until a read on this path fails, then the wait the next failure
	// doubles
	backoff time.Duration
}

// A schedule with nothing read yet, so a read is due at once. A max below the
// minimum is the minimum.
func newExtenderReadSchedule(
	refreshTimeout time.Duration,
	minBackoff time.Duration,
	maxBackoff time.Duration,
) *extenderReadSchedule {
	return &extenderReadSchedule{
		refreshTimeout: refreshTimeout,
		minBackoff:     minBackoff,
		maxBackoff:     max(minBackoff, maxBackoff),
	}
}

// Whether a read is due at `now`: none has ended on this path yet, or the
// wait after the last has passed.
func (self *extenderReadSchedule) Due(now time.Time) bool {
	return self.readTime.IsZero() || self.wait <= now.Sub(self.readTime)
}

// The read that ended at `now` answered.
func (self *extenderReadSchedule) Answer(now time.Time) {
	self.readTime = now
	self.wait = self.refreshTimeout
	self.backoff = 0
}

// The read that ended at `now` failed.
func (self *extenderReadSchedule) Fail(now time.Time) {
	if self.backoff <= 0 {
		self.backoff = self.minBackoff
	} else {
		self.backoff = min(2*self.backoff, self.maxBackoff)
	}
	self.readTime = now
	self.wait = self.backoff
}

// The path changed, for a read whose answer depends on the path.
func (self *extenderReadSchedule) Rearm() {
	self.readTime = time.Time{}
	self.wait = 0
	self.backoff = 0
}

// The path changed, for a read whose answer does not: a failed read is due at
// once, and an answer keeps its refresh timeout.
func (self *extenderReadSchedule) ClearBackoff() {
	if 0 < self.backoff {
		self.Rearm()
	}
}

// Reads the operator's hint and applies it to the directory. An empty
// continent is an operator that cannot place this client, which leaves
// whatever the dns inference said; an empty country leaves the last one, stale.
// A hint that cannot be had makes the country stale, so the network country
// the host reports stands in for it (SpoofCountryCode). Reports whether the
// fetch completed.
func (self *ExtenderNetworkClient) refreshHint() bool {
	hint := self.settings.Hint
	if hint == nil {
		if self.settings.ApiUrl == "" || self.clientStrategy == nil {
			return false
		}
		hint = self.hint
	}
	ctx, cancel := context.WithTimeout(self.ctx, self.settings.HelloTimeout)
	defer cancel()
	result, err := hint(ctx)
	if err != nil {
		self.log.Infof("[extender]hint err = %s\n", err)
		self.directory.ExpireCountryHint()
		return false
	}
	if result == nil {
		result = &ExtenderHintResult{}
	}
	if countryCode := NormalizeSpoofCountryCode(result.CountryCode); self.directory.SetCountryHint(countryCode) {
		self.log.Infof("[extender]country hint %s (operator)\n", countryCode)
	}
	continentCode := strings.ToUpper(strings.TrimSpace(result.ContinentCode))
	if continentCode == "" {
		return true
	}
	self.setContinentHint(continentCode, extenderContinentHintSourceOperator)
	return true
}

// The hint fetch when settings.Hint is nil: GetExtenderHint through the
// client strategy's direct dialers.
func (self *ExtenderNetworkClient) hint(ctx context.Context) (*ExtenderHintResult, error) {
	return GetExtenderHint(ctx, self.clientStrategy, self.settings.ApiUrl)
}

// Applies the continent the dns bootstrap implied: the one continent its
// records agree on. A split answer is not a hint, and the operator's own hint,
// once applied, is not overridden -- it judged this client's address directly,
// where the dns judged the resolver's.
func (self *ExtenderNetworkClient) inferContinentHint(continentCounts map[string]int) {
	if len(continentCounts) == 0 {
		return
	}
	best := ""
	bestCount := 0
	total := 0
	for continentCode, count := range continentCounts {
		total += count
		if bestCount < count || (bestCount == count && continentCode < best) {
			best = continentCode
			bestCount = count
		}
	}
	if bestCount*2 <= total {
		return
	}
	// the hint loop's answer may land while the bootstrap runs; once it is
	// decided, an inference is not (setContinentHint)
	self.setContinentHint(best, extenderContinentHintSourceDns)
}

// The sources of the continent hint, as its log names them.
const (
	extenderContinentHintSourceOperator = "operator"
	extenderContinentHintSourceDns      = "dns"
)

// Decides the continent the candidate order prefers and brings the directory
// and the status to it. The operator's answer always decides, and the dns
// inference only until the operator has answered: the operator judged this
// client's address, where the dns judged the resolver's.
//
// The two sources decide side by side, the hint loop and the bootstrap. A
// decision is made under continentLock and advances a version, and the
// directory and the status are set outside the lock, as any external call
// is. A setter that read an older decision can set the directory after a
// newer one has been set, so each setter checks under the lock, once it has
// set them, that the version it set is still the latest, and sets the latest
// again when it is not. The last set is therefore always of the latest
// decision, and an older continent shows at most between the two sets. The
// loop repeats only while decisions land faster than it sets them, which two
// sources that decide once per read and once per bootstrap never sustain.
func (self *ExtenderNetworkClient) setContinentHint(continentCode string, source string) {
	decided := func() bool {
		self.continentLock.Lock()
		defer self.continentLock.Unlock()
		if source == extenderContinentHintSourceOperator {
			self.operatorHintApplied = true
		} else if self.operatorHintApplied {
			return false
		}
		self.continentHintCode = continentCode
		self.continentHintSource = source
		self.continentHintVersion += 1
		return true
	}()
	if !decided {
		return
	}
	for {
		decidedContinentCode, decidedSource, decidedVersion := func() (string, string, uint64) {
			self.continentLock.Lock()
			defer self.continentLock.Unlock()
			return self.continentHintCode, self.continentHintSource, self.continentHintVersion
		}()
		if self.continentHintSetHook != nil {
			self.continentHintSetHook()
		}
		if self.directory.SetContinentHint(decidedContinentCode) {
			self.log.Infof("[extender]continent hint %s (%s)\n", decidedContinentCode, decidedSource)
			// the order changed; what the pass should measure first may have too
			self.probeWake.NotifyAll()
		}
		self.updateStatus(func(status *ExtenderNetworkClientStatus) {
			status.ContinentHint = decidedContinentCode
		})
		latest := func() bool {
			self.continentLock.Lock()
			defer self.continentLock.Unlock()
			return self.continentHintVersion == decidedVersion
		}()
		if latest {
			return
		}
	}
}

// The continent a signed record carries, upper case, empty when it predates
// the field or is not a record.
func extenderRecordContinentCode(message *protocol.ExtenderGossipMessage) string {
	record := message.GetRecord()
	if record == nil {
		return ""
	}
	body := &protocol.ExtenderRecordBody{}
	if err := proto.Unmarshal(record.Body, body); err != nil {
		return ""
	}
	return strings.ToUpper(strings.TrimSpace(body.ContinentCode))
}

// The latency probe pass (DESIGNNOTES4.md §4).

// Installs the attesting provider and the reporter its pings go to, or clears
// both with a nil attestor. Only the provider role calls this, when it starts
// and when it stops: a consumer client never identifies itself to an
// extender. The pinger reports its own pings, whatever the target
// answered (GEOMAP §2.5); with a nil reporter it attests and reports nothing.
// An attestor that does not name exactly one identity is refused, which
// leaves the client ranking. An install wakes the pass, so a provider that
// just started attests without waiting for the next tick.
func (self *ExtenderNetworkClient) SetProbeAttestor(
	attestor *ExtenderProbeAttestor,
	reporter *ExtenderPingReporter,
) {
	if attestor != nil && attestor.Kind() == "" {
		self.log.Infof("[extender]probe attestor names no single identity; probes rank only\n")
		attestor = nil
	}
	if attestor == nil {
		// nothing is attested, so there is nothing to report
		reporter = nil
	}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.probeAttestor = attestor
		self.probeReporter = reporter
	}()
	self.probeWake.NotifyAll()
}

// The attestor and reporter the probe pass uses now.
func (self *ExtenderNetworkClient) probeAttestorValue() (*ExtenderProbeAttestor, *ExtenderPingReporter) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.probeAttestor, self.probeReporter
}

// The attestor and reporter SetProbeAttestor installed, nil for a client that
// only ranks. An embedder that installs the pair on behalf
// of a provider reads it back through this rather than through the client's
// fields, so what it verifies is what the probe pass uses.
func (self *ExtenderNetworkClient) ProbeAttestor() (*ExtenderProbeAttestor, *ExtenderPingReporter) {
	return self.probeAttestorValue()
}

// The probe loop: one pass on every wake -- a bootstrap, a completed sample,
// a hint, an attestor -- and on the refresh cadence. A pass only measures
// what has no current sample, or one due a refresh, so a burst of wakes costs
// little.
//
// Its timers run on the monotonic clock, which stops while the host sleeps,
// so a host that woke on the same path would wait out the rest of the refresh
// period in awake time, up to six hours, before it measured again. The loop
// reads the host clock at every wakeup of its wait, and at least every
// ResumeCheckTimeout, to tell a resume (hostResumeWatch, hostResumed).
func (self *ExtenderNetworkClient) runProbes() {
	if self.settings.ProbeWindowCount <= 0 {
		<-self.ctx.Done()
		return
	}
	select {
	case <-self.ctx.Done():
		return
	case <-self.initialProbeReady:
	}
	// the first pass also waits for the first hint read, which runs beside
	// the bootstrap, so it probes the operator's continent first rather than
	// spending its pings before the operator has said
	select {
	case <-self.ctx.Done():
		return
	case <-self.initialHintDone:
	}
	probeAfter := self.settings.ProbeAfter
	if probeAfter == nil {
		probeAfter = time.After
	}
	resumeWatch := newHostResumeWatch(
		self.settings.ResumeMinSleep,
		self.settings.ResumeCheckTimeout,
		self.settings.Now(),
	)
	for {
		// subscribe before the pass, so a wake that lands while it runs is
		// carried into the next wait instead of being lost
		wake := self.probeWake.NotifyChannel()
		self.probePass()

		wait := self.settings.RebootstrapTimeout
		if wait <= 0 {
			wait = DefaultExtenderNetworkClientSettings().RebootstrapTimeout
		}
		passAfter := probeAfter(wait)
		for waiting := true; waiting; {
			// nil, which never fires, when resumes are not watched
			var checkAfter <-chan time.Time
			if resumeWatch.Watching() {
				checkAfter = probeAfter(self.settings.ResumeCheckTimeout)
			}
			select {
			case <-self.ctx.Done():
				return
			case <-wake:
				waiting = false
			case <-passAfter:
				waiting = false
			case <-checkAfter:
			}
			now := self.settings.Now()
			if sleep, resumed := resumeWatch.Check(now); resumed {
				self.hostResumed(now, sleep)
			}
		}
	}
}

// One pass over every family this host has.
func (self *ExtenderNetworkClient) probePass() {
	attestor, reporter := self.probeAttestorValue()
	probed := false
	for _, ipVersion := range []int{4, 6} {
		if !self.ipVersionSupported(ipVersion) {
			continue
		}
		if self.probeFamily(ipVersion, attestor, reporter) {
			probed = true
		}
	}
	if probed {
		probeTime := self.settings.Now()
		self.updateStatus(func(status *ExtenderNetworkClientStatus) {
			status.LastProbeTime = probeTime
		})
	}
}

// Measures the candidates of one family that have no current sample, or one
// due a refresh, hinted continent first, until the window holds enough close
// extenders (DESIGNNOTES4.md §4). A sample due a refresh -- one from before a
// path change or a long sleep -- keeps ranking but does not count toward the
// window, so the pass measures it again, and the new sample replaces it as it
// lands. With an attestor a sample counts only once the target co-signed it
// (GEOMAP §2.3), so a provider that just started measures -- and attests --
// what a ranking pass already measured, and a target that refused or sent no
// verdict is measured again on the next pass. Reports whether anything was
// probed.
func (self *ExtenderNetworkClient) probeFamily(
	ipVersion int,
	attestor *ExtenderProbeAttestor,
	reporter *ExtenderPingReporter,
) (probed bool) {
	attesting := attestor != nil
	closeCount := func() int {
		return extenderCloseCount(
			self.directory.probeWindowLatencies(ipVersion, attesting),
			self.settings.ProbeCloseFactor,
			self.settings.ProbeCloseFloor,
		)
	}
	if self.settings.ProbeWindowCount <= closeCount() {
		return false
	}
	candidates := self.directory.ProbeCandidates(ipVersion, self.settings.ProbeMaxCandidateCount, attesting)
	for _, candidate := range candidates {
		select {
		case <-self.ctx.Done():
			return probed
		default:
		}
		// an attestor installed or cleared under the pass ends it: the change
		// woke the next pass, which probes as the change says, so a provider
		// that stops providing attests no further candidate (DESIGNNOTES4.md
		// §1)
		if currentAttestor, _ := self.probeAttestorValue(); currentAttestor != attestor {
			return probed
		}
		if self.settings.ProbeWindowCount <= closeCount() {
			return probed
		}
		if 0 < candidate.Latency && !candidate.LatencyRefreshDue && (!attesting || candidate.LatencyAttested) {
			// a current sample; the pass is for what has none, or one due a
			// refresh
			continue
		}
		probed = true
		rtt, outcome, err := self.probeCandidate(candidate, attestor, reporter)
		if err != nil {
			self.log.Infof("[extender]probe %s err = %s\n", candidate.Ip, err)
			continue
		}
		self.directory.RecordLatency(candidate.Ip, rtt, outcome == ExtenderPingCosigned)
		if self.log.V(1).Enabled() {
			self.log.Infof("[extender]probe %s rtt=%s outcome=%q\n", candidate.Ip, rtt, outcome)
		}
	}
	return probed
}

// Probes one candidate through the shared carrier walk and reports every
// probe that attested, whatever the target answered (GEOMAP §2.5). The outcome
// is what the probes amount to, of which only co-signed marks the sample.
func (self *ExtenderNetworkClient) probeCandidate(
	candidate *ExtenderCandidate,
	attestor *ExtenderProbeAttestor,
	reporter *ExtenderPingReporter,
) (time.Duration, ExtenderPingOutcome, error) {
	if self.settings.Probe != nil {
		ctx, cancel := context.WithTimeout(self.ctx, self.settings.ProbeTimeout)
		defer cancel()
		return self.settings.Probe(ctx, candidate, attestor)
	}
	probeLatency := self.settings.ProbeLatency
	if probeLatency == nil {
		connectSettings := DefaultConnectSettings()
		if self.clientStrategy != nil {
			connectSettings = &self.clientStrategy.settings.ConnectSettings
		}
		probeLatency = func(
			ctx context.Context,
			extenderConfig *ExtenderConfig,
			attestor *ExtenderProbeAttestor,
		) (*ExtenderLatencyProbe, error) {
			return ProbeExtenderLatency(ctx, connectSettings, extenderConfig, attestor)
		}
	}
	candidateProbe, err := probeExtenderCandidate(
		self.ctx,
		self.directory,
		candidate,
		self.settings.ProbeCountPerExtender,
		self.settings.ProbeTimeout,
		func(ctx context.Context, extenderConfig *ExtenderConfig) (*ExtenderLatencyProbe, error) {
			return probeLatency(ctx, extenderConfig, attestor)
		},
	)
	if err != nil {
		return 0, ExtenderPingUnattested, err
	}
	if reporter != nil {
		for _, probe := range candidateProbe.probes {
			reporter.Report(ExtenderPingReportFromProbe(probe))
		}
	}
	outcome, _ := candidateProbe.outcome()
	return candidateProbe.rtt, outcome, nil
}

// The outcome of probing one candidate: the lowest rtt measured on the first
// carrier that answered, and every probe made there, which is what a pass
// reports (GEOMAP §2.5).
type extenderCandidateProbe struct {
	connectMode ExtenderConnectMode
	rtt         time.Duration
	// in the order they were made
	probes []*ExtenderLatencyProbe
}

// What the candidate's probes amount to: co-signed when any was, since the
// target then vouched for a claim of this pinger at least once; else rejected
// when any was refused, with the first refusal's reason; else unknown when any
// attested; else unattested.
func (self *extenderCandidateProbe) outcome() (ExtenderPingOutcome, uint32) {
	outcome := ExtenderPingUnattested
	var reason uint32
	for _, probe := range self.probes {
		switch probe.Outcome {
		case ExtenderPingCosigned:
			return ExtenderPingCosigned, probe.Reason
		case ExtenderPingRejected:
			if outcome != ExtenderPingRejected {
				outcome = ExtenderPingRejected
				reason = probe.Reason
			}
		case ExtenderPingUnknown:
			if outcome == ExtenderPingUnattested {
				outcome = ExtenderPingUnknown
			}
		}
	}
	return outcome, reason
}

// Probes one candidate: its carriers in order until one answers, and on that carrier up to `count` probes, of which the lowest rtt
// is kept. A carrier that does not answer is a failed dial of that carrier,
// exactly as a feed dial records it, and a carrier that does clears the hold.
// Both probe passes walk carriers here -- the network client's
// (DESIGNNOTES4.md §4) and the extender's peer pinger (GEOMAP §2.1) -- so the
// two can never disagree about what a ping of one extender is.
func probeExtenderCandidate(
	ctx context.Context,
	directory *ExtenderDirectory,
	candidate *ExtenderCandidate,
	count int,
	probeTimeout time.Duration,
	probeLatency func(ctx context.Context, extenderConfig *ExtenderConfig) (*ExtenderLatencyProbe, error),
) (*extenderCandidateProbe, error) {
	count = max(1, count)
	var resultErr error
	spoofDomains, _ := directorySpoofDomains(directory)
	for _, carrier := range orderedExtenderCarriers(candidate.Carriers) {
		connectMode, ok := ExtenderConnectModeForCarrier(carrier)
		if !ok {
			continue
		}
		extenderConfig := extenderFeedConfig(candidate, connectMode, spoofDomains)
		if extenderConfig == nil {
			continue
		}
		candidateProbe := &extenderCandidateProbe{
			connectMode: connectMode,
			probes:      []*ExtenderLatencyProbe{},
		}
		for i := 0; i < count; i += 1 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			default:
			}
			probe, err := func() (*ExtenderLatencyProbe, error) {
				probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
				defer cancel()
				return probeLatency(probeCtx, extenderConfig)
			}()
			if err == nil && probe == nil {
				err = fmt.Errorf("extender probe returned no result")
			}
			if err != nil {
				if ctxErr := ctx.Err(); ctxErr != nil {
					// the pass is ending, which says nothing about the
					// extender: no failure is recorded against it
					return nil, ctxErr
				}
				var limitedErr *ExtenderLimitedError
				if errors.As(err, &limitedErr) {
					// over the extender's admission limits (A12): a backoff,
					// never a failure and never a measurement, and another
					// carrier of the same extender is limited the same way
					directory.RecordLimited(candidate.Ip, limitedErr.RetryAfter)
					return nil, err
				}
				resultErr = err
				break
			}
			candidateProbe.probes = append(candidateProbe.probes, probe)
			if 0 < probe.Rtt && (candidateProbe.rtt == 0 || probe.Rtt < candidateProbe.rtt) {
				candidateProbe.rtt = probe.Rtt
			}
		}
		if 0 < candidateProbe.rtt {
			directory.RecordSuccess(candidate.Ip, connectMode)
			return candidateProbe, nil
		}
		directory.RecordFailure(candidate.Ip, connectMode)
	}
	if resultErr == nil {
		resultErr = fmt.Errorf("no carrier to probe")
	}
	return nil, resultErr
}

// How many of the latencies are close enough (DESIGNNOTES4.md §4): within
// `factor` of the best, or under `floor`, whichever admits more.
func extenderCloseCount(latencies []time.Duration, factor float64, floor time.Duration) int {
	if len(latencies) == 0 {
		return 0
	}
	if factor < 1 {
		factor = 1
	}
	best := slices.Min(latencies)
	limit := time.Duration(float64(best) * factor)
	if limit < floor {
		limit = floor
	}
	count := 0
	for _, latency := range latencies {
		if latency <= limit {
			count += 1
		}
	}
	return count
}
