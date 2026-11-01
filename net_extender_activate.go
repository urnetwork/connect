package connect

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect/protocol"
)

// The extender activation loop (EXTENDER.md C2, G3).
//
// An extender proves itself to the operator rather than announcing itself: it
// posts the carriers it is listening on, the operator probes back on the caller
// address of that request, and what comes back is a signed record naming the
// address the operator actually saw. That is why the activation is posted once
// per family, to the family api url, and why it must never cross an extender --
// the operator would see the extender's address and publish a record for the
// wrong host. An operator that has no family api hosts is activated through the
// plain api url instead, once per cycle: the operator derives the family from
// the caller address either way, so the answer names the family that was
// activated and the outcome is recorded under it.
//
// The loop runs on five triggers (G3): start, the 24 hour tick, the own key
// observed revoked in the directory, a changed caller address from hello, and a
// network change. Everything else is backoff: a refusal holds the next attempt
// for ten minutes, doubling to six hours, and a success resets it. A refusal
// that says when the operator has room again (`retry_after_seconds`: a spent
// activation budget, or an operator too busy to probe) holds the family that
// long instead, within a minute and six hours, and leaves the backoff as it
// was. Each family keeps its own schedule (connect#230): a family that passed
// waits out its own tick while the other family retries, rather than being
// posted again with it and spending its activation budget for nothing. A
// trigger makes every family due. The tick, the address check and the backoff
// count the time the host slept, from the first minute it is awake after a
// sleep of fifteen minutes or more.
//
// Every clock and every side effect is a settings seam -- `Now`, `PassAfter`,
// `IpVersionSupported`, `AddNetworkChangeListener` -- so the whole loop is
// deterministic in tests.
//
// The activator is safe for concurrent use.

// The activation endpoint, appended to a family api url (C2).
const ExtenderActivatePath = "/network/extender-activate"

// The activation request (C2). The json is the server's contract; the field
// names are the wire and are not ours to rename.
type ExtenderActivateArgs struct {
	PublicKeyHex string `json:"public_key_hex"`
	// the hex X25519 static public key of the camouflage (EXTENDER.md P1, P6),
	// derived from the identity seed. Empty on an extender with camouflage off;
	// an operator that predates the field ignores it (Go's decoder drops an
	// unknown field), so activation stays non-fatal.
	RealityPublicKeyHex string `json:"reality_public_key_hex,omitempty"`
	TcpPort             int    `json:"tcp_port"`
	UdpPort             int    `json:"udp_port"`
	DnsPort             int    `json:"dns_port"`
	// every dns port that is listening, which the operator probes one by one
	// and the record then lists (L2). Empty leaves the operator on DnsPort
	// alone, which is what an extender that predates the list offers.
	DnsPorts []int    `json:"dns_ports,omitempty"`
	DnsTld   string   `json:"dns_tld"`
	Carriers []string `json:"carriers"`
	// the certificate request of the extender's measurement key
	// (lag.dev/PLAN.md C4): standard base64 of the DER, whose one name is the
	// extender's measurement name (N1). Empty when the extender asks for no
	// certificate, which is most activations; an operator that predates the
	// field ignores it.
	MeasurementCsr string `json:"measurement_csr,omitempty"`
	// the owner opted out of lag.dev measurement (lag.dev/PLAN.md C4, decision
	// 18), or the extender cannot use its measurement key, so the operator
	// withdraws the measurement name (C9). Set on every activation while
	// either stands, and only by an extender that keeps its identity, which
	// is all that can have a name to withdraw.
	MeasurementDisabled bool `json:"measurement_disabled,omitempty"`
}

// The activation answer (C2). A refusal is a normal answer with `Activated`
// false and `Error` naming the carrier that failed its probe, not a request
// error, so the caller can act on it rather than on an http status.
type ExtenderActivateResult struct {
	Activated bool `json:"activated"`
	// the operator's id of this extender, one per identity key and the same at
	// every activation (lag.dev/PLAN.md N4). The extender's measurement name
	// is this id under the measurement zone (N1), so it is how an extender
	// learns the name its certificate request asks for (C4). Nil from an
	// operator that predates it, and on a refusal.
	ExtenderId *Id    `json:"extender_id,omitempty"`
	Ip         string `json:"ip,omitempty"`
	IpVersion  int    `json:"ip_version,omitempty"`
	// the carriers that passed their probe, which is what the signed record
	// lists
	Carriers   []string   `json:"carriers,omitempty"`
	Error      string     `json:"error,omitempty"`
	ExpireTime *time.Time `json:"expire_time,omitempty"`
	// the operator patterns this extender may forward to (A5)
	AllowedHosts []string `json:"allowed_hosts,omitempty"`
	// base64 of a serialized protocol.ExtenderRecord, this extender's own
	Record string `json:"record,omitempty"`
	// base64 of serialized protocol.ExtenderRecord messages of other active
	// extenders, the directory this extender starts from (D6)
	Bootstrap []string `json:"bootstrap,omitempty"`
	// on a refusal that judged nothing about this extender -- a spent
	// activation budget, or an operator too busy to probe -- the seconds until
	// the operator has room for this family's next post (C2, connect#230). 0
	// on every other answer, and from an operator that predates the field.
	RetryAfterSeconds int `json:"retry_after_seconds,omitempty"`
}

// PostExtenderActivate posts one activation to one family api url (C2). The
// strategy must be direct-only: an activation that crossed an extender would
// publish that extender's address as this host's.
func PostExtenderActivate(
	ctx context.Context,
	clientStrategy *ClientStrategy,
	apiUrl string,
	byJwt string,
	args *ExtenderActivateArgs,
) (*ExtenderActivateResult, error) {
	if clientStrategy == nil {
		return nil, fmt.Errorf("the extender activation needs a client strategy")
	}
	if args == nil {
		return nil, fmt.Errorf("the extender activation needs args")
	}
	apiUrl = strings.TrimRight(strings.TrimSpace(apiUrl), "/")
	if apiUrl == "" {
		return nil, fmt.Errorf("the extender activation needs an api url")
	}
	return HttpPostWithStrategy(
		ctx,
		clientStrategy,
		apiUrl+ExtenderActivatePath,
		args,
		byJwt,
		&ExtenderActivateResult{},
		NewNoopApiCallback[*ExtenderActivateResult](),
	)
}

type ExtenderActivatorSettings struct {
	Log Logger

	// The family api urls (C2). An activation is posted to the url of the
	// family it activates, so the caller address the operator probes back has
	// a known family. Either may be empty, which skips that family.
	ApiUrlV4 string
	ApiUrlV6 string
	// The plain api url, used only when neither family url is set. The
	// operator derives the family from the caller address, so one post per
	// cycle reaches whichever family this host's egress used and the answer
	// names it. An operator with api-v4 and api-v6 hosts never takes this
	// path, because a family url is exact for the family it names.
	ApiUrl string
	// The api url whose `/hello` reports the caller address this extender is
	// published under (G3). Empty disables the address check.
	HelloUrl string

	// The client jwt. A getter rather than a value, since the jwt refreshes
	// and an activation may be 24 hours after the last one.
	ByJwt func() string

	// The direct-only strategy every activation and hello is posted through
	// (A4, C2). An activation that crossed an extender would tell the operator
	// the extender's address.
	ClientStrategy *ClientStrategy

	// This extender's identity key (B1), which the operator probes back
	// against and signs the record for.
	PublicKey []byte

	// This extender's X25519 static public key of the camouflage (EXTENDER.md
	// P1, P6), which the operator signs into the record so a client can front
	// the tcp carrier with an authenticated hello. The role derives it from the
	// identity seed (ExtenderRealityStaticPublicKey) and sets it only when
	// camouflage is on and at least one borrowed name verified; empty leaves the
	// extender on the legacy terminate path and publishes no camouflage key.
	RealityPublicKey []byte

	// The carrier ports and the encoding tld this extender serves (C1, C2).
	// Zero ports and an empty tld take the operator's defaults.
	TcpPort int
	UdpPort int
	DnsPort int
	DnsTld  string

	// The carriers that are listening right now (G2). Only these are offered,
	// so a carrier whose bind failed is never probed. Nil offers none, which
	// holds every activation.
	Carriers func() []string

	// The dns ports that are listening right now (L2), which is what the
	// operator probes and the record lists. Nil offers DnsPort alone.
	DnsPorts func() []int

	// The directory the signed record and the bootstrap records are applied
	// into (C2, E1). It is also where a revocation of this extender's own key
	// is observed, which is one of the re-activation triggers (G3).
	Directory *ExtenderDirectory

	// OnActivated runs after each successful activation of one family, with
	// the result that produced it. The extender's mesh listen addresses are
	// updated from it (D2, G4). It runs on the loop goroutine and must not
	// block.
	OnActivated func(ipVersion int, result *ExtenderActivateResult)

	// MeasurementCsr, when set, is asked for the measurement certificate
	// request a post carries (lag.dev/PLAN.md C4): the base64 DER request
	// whose one name is the measurement name of `extenderId`, this extender's
	// id as the operator's answers report it (N4); or "" for none, because
	// measurement is off or the operator already holds a request or a
	// certificate for that name. It is asked only once an answer has reported
	// the id, so the first post of a new identity carries none and the request
	// rides the next post, and a pass carries at most one. A pass that learned
	// the id from its last answer has no next post: when a request is wanted
	// then, one more pass follows at once to carry it -- so this may be asked
	// with no post following. It runs on the loop goroutine and must not
	// block.
	MeasurementCsr func(extenderId Id) string
	// MeasurementDisabled, when set, reports whether the owner has opted out of
	// measurement, or the extender cannot measure for want of a usable key
	// (lag.dev/PLAN.md C4): every post made while it reports true carries
	// measurement_disabled and no request, so the operator withdraws the
	// measurement name (C9). It runs on the loop goroutine and must not block.
	MeasurementDisabled func() bool

	// The re-activation period (G3).
	ActivateTimeout time.Duration
	// The caller address check period (G3).
	AddressCheckTimeout time.Duration
	// Backoff after a failed pass, doubling to the max (G3).
	MinBackoff time.Duration
	MaxBackoff time.Duration
	// The shortest wait a refusal's `retry_after_seconds` is taken as (C2,
	// connect#230). A refusal that carries one holds its family exactly that
	// long, at least this and at most MaxBackoff, and leaves the backoff as it
	// was: the operator said when it has room, and nothing about this host. A
	// minute keeps an operator's short answer from turning the loop into a
	// poll. Non-positive takes the default.
	MinRetryAfter time.Duration
	// Budget of one activation post and of one hello.
	RequestTimeout time.Duration
	// A resume from a sleep of at least ResumeMinSleep pulls the address
	// check and the re-activation forward by the time slept, so each comes
	// due as it would have had its timer run through the sleep: the loop's
	// timers run on the monotonic clock, which stops while the host sleeps.
	// The loop reads the host clock every ResumeCheckTimeout while it waits,
	// and acts once per resume, when the host has stayed awake that long
	// since the check that saw the sleep (hostResumeWatch). The defaults are
	// argued at defaultResumeMinSleep. <= 0 for either disables it.
	ResumeCheckTimeout time.Duration
	ResumeMinSleep     time.Duration

	// The only clock this loop reads. Tests install a fake one.
	Now func() time.Time
	// When set, replaces time.After as the loop's waits: until the next pass
	// is due, and each resume check while it waits (ResumeCheckTimeout).
	// Tests fire the checks through it, and hold the loop without a sleep.
	PassAfter func(wait time.Duration) <-chan time.Time
	// IpVersionSupported, when set, replaces the host family probe. Nil uses
	// FamilySupported, which is what a family without a global address answers
	// no for (G3).
	IpVersionSupported func(ipVersion int) bool
	// AddNetworkChangeListener, when set, replaces the process network change
	// subscription. It returns the unsubscribe. Nil takes the process one.
	AddNetworkChangeListener func(listener func()) func()
}

func DefaultExtenderActivatorSettings() *ExtenderActivatorSettings {
	return &ExtenderActivatorSettings{
		TcpPort:             ExtenderTcpPort,
		UdpPort:             ExtenderQuicPort,
		DnsPort:             ExtenderDnsPort,
		DnsTld:              DefaultExtenderDnsTld,
		ActivateTimeout:     24 * time.Hour,
		AddressCheckTimeout: 1 * time.Hour,
		MinBackoff:          10 * time.Minute,
		MaxBackoff:          6 * time.Hour,
		MinRetryAfter:       1 * time.Minute,
		RequestTimeout:      60 * time.Second,
		ResumeCheckTimeout:  defaultResumeCheckTimeout,
		ResumeMinSleep:      defaultResumeMinSleep,
		Now:                 time.Now,
	}
}

// The activation state of one family, which the sdk provider status renders
// (F3). `AllowedHosts` is what the operator last said this extender may
// forward to (A5); the extender's own whitelist is configured, not taken from
// here, so a difference is visible rather than silently applied.
type ExtenderFamilyActivationStatus struct {
	IpVersion int
	Activated bool
	Ip        netip.Addr
	// The last completed activation attempt, successful or not.
	LastActivationTime time.Time
	LastError          string
	// Whether `LastError` is the operator's refusal: the answer arrived with
	// `activated` false. Every other failure leaves it false -- the request
	// failed, the answer was unreadable or named no family, no carrier was
	// listening -- and a success clears it with `LastError`. It is what tells
	// the refused label of N3 from the failed one (N7).
	LastRefused bool
	// When a revocation of this extender's key was last observed in the
	// directory (G3).
	RevokedTime  time.Time
	ExpireTime   time.Time
	AllowedHosts []string
}

// The whole activation state, one entry per family in ip version order.
type ExtenderActivatorStatus struct {
	Families []*ExtenderFamilyActivationStatus
}

// The entry of one family, nil when the activator does not run that family.
func (self *ExtenderActivatorStatus) Family(ipVersion int) *ExtenderFamilyActivationStatus {
	for _, family := range self.Families {
		if family.IpVersion == ipVersion {
			return family
		}
	}
	return nil
}

type ExtenderActivator struct {
	ctx       context.Context
	cancel    context.CancelFunc
	closeOnce sync.Once
	done      chan struct{}
	log       Logger

	settings *ExtenderActivatorSettings

	// increments on every status change; a consumer that renders the status
	// watches it, exactly as it watches the directory's
	changeMonitor *MonitorValue[uint64]
	// closed and replaced on a wake request; a network change takes this path
	wakeMonitor        *Monitor
	unsubNetworkChange func()

	// true when neither family api url is set, so one post per cycle goes to
	// the plain api url and the answer names the family it activated
	fallback bool

	stateLock sync.Mutex
	version   uint64
	// keyed by ip version. In the fallback mode an entry is created when an
	// answer names its family, and key 0 is the placeholder of an outcome that
	// named none.
	families map[int]*ExtenderFamilyActivationStatus
	// true while a revocation of this key is standing in the directory, so the
	// re-activation triggers on the edge and the backoff governs the retry
	revokedObserved bool
	// set by a network change, taken by the next pass
	forcedActivate bool

	// this extender's id as the operator's last answer reported it, zero
	// until one has (lag.dev/PLAN.md N4), and whether the last pass forced this
	// one to carry the measurement request (C4), so that it is followed by no
	// other. Both are read and written by the loop goroutine alone.
	extenderId          Id
	measurementFollowUp bool
}

// The activator is running when this returns: the loop is up and its first
// pass has been started.
func NewExtenderActivator(
	ctx context.Context,
	settings *ExtenderActivatorSettings,
) *ExtenderActivator {
	if settings == nil {
		settings = DefaultExtenderActivatorSettings()
	}
	copied := *settings
	if copied.Now == nil {
		copied.Now = time.Now
	}
	if copied.ActivateTimeout <= 0 {
		copied.ActivateTimeout = 24 * time.Hour
	}
	if copied.AddressCheckTimeout <= 0 {
		copied.AddressCheckTimeout = 1 * time.Hour
	}
	if copied.MinBackoff <= 0 {
		copied.MinBackoff = 10 * time.Minute
	}
	if copied.MaxBackoff < copied.MinBackoff {
		copied.MaxBackoff = copied.MinBackoff
	}
	if copied.MinRetryAfter <= 0 {
		copied.MinRetryAfter = DefaultExtenderActivatorSettings().MinRetryAfter
	}
	if copied.RequestTimeout <= 0 {
		copied.RequestTimeout = 60 * time.Second
	}
	settings = &copied

	cancelCtx, cancel := context.WithCancel(ctx)
	self := &ExtenderActivator{
		ctx:           cancelCtx,
		cancel:        cancel,
		done:          make(chan struct{}),
		log:           loggerOrDefault(settings.Log),
		settings:      settings,
		changeMonitor: NewMonitorValue[uint64](0),
		wakeMonitor:   NewMonitor(),
		families:      map[int]*ExtenderFamilyActivationStatus{},
	}
	self.fallback = len(self.ipVersions()) == 0 && self.fallbackApiUrl() != ""
	for _, ipVersion := range self.ipVersions() {
		self.families[ipVersion] = &ExtenderFamilyActivationStatus{IpVersion: ipVersion}
	}

	addNetworkChangeListener := settings.AddNetworkChangeListener
	if addNetworkChangeListener == nil {
		addNetworkChangeListener = AddNetworkChangeListener
	}
	// a path change can move this host's public address, which is the address
	// the operator published for it
	self.unsubNetworkChange = addNetworkChangeListener(self.networkChanged)

	go HandleError(func() {
		defer close(self.done)
		self.run()
	}, cancel)
	return self
}

// The families this activator runs, in ip version order. A family with no api
// url is not run at all, so it never appears in the status.
func (self *ExtenderActivator) ipVersions() []int {
	ipVersions := []int{}
	if strings.TrimSpace(self.settings.ApiUrlV4) != "" {
		ipVersions = append(ipVersions, 4)
	}
	if strings.TrimSpace(self.settings.ApiUrlV6) != "" {
		ipVersions = append(ipVersions, 6)
	}
	return ipVersions
}

func (self *ExtenderActivator) apiUrl(ipVersion int) string {
	if ipVersion == 6 {
		return strings.TrimSpace(self.settings.ApiUrlV6)
	}
	return strings.TrimSpace(self.settings.ApiUrlV4)
}

// The plain api url an operator without family api hosts is activated through.
func (self *ExtenderActivator) fallbackApiUrl() string {
	return strings.TrimSpace(self.settings.ApiUrl)
}

// The status as a consumer reads it. The entries are copies, so a renderer
// cannot alias the loop's state.
func (self *ExtenderActivator) Status() *ExtenderActivatorStatus {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	status := &ExtenderActivatorStatus{Families: []*ExtenderFamilyActivationStatus{}}
	// 0 is the fallback placeholder, which carries the outcome of an answer
	// that named no family and is dropped as soon as one does
	for _, ipVersion := range []int{0, 4, 6} {
		family, ok := self.families[ipVersion]
		if !ok {
			continue
		}
		copied := *family
		copied.AllowedHosts = slices.Clone(family.AllowedHosts)
		status.Families = append(status.Families, &copied)
	}
	return status
}

// The change counter. A consumer reads the status and waits on the channel the
// same `Get` returned, which is what keeps the read and the subscribe from
// separating.
func (self *ExtenderActivator) ChangeMonitor() *MonitorValue[uint64] {
	return self.changeMonitor
}

// Starts a pass at once, as a network change does (G3), for something the
// operator should hear now rather than at the next tick: a measurement request
// for a key it does not hold, or an owner's opt-out of measurement or its end
// (lag.dev/PLAN.md C4). Safe to call from any goroutine.
func (self *ExtenderActivator) Reactivate() {
	self.networkChanged()
}

// Wakes the loop, which re-activates every family (G3). A path change can move
// this host's public address.
func (self *ExtenderActivator) networkChanged() {
	self.stateLock.Lock()
	// the next pass activates whatever the cadence says, because the address
	// the operator published may no longer be this host's
	self.forcedActivate = true
	self.stateLock.Unlock()
	self.wakeMonitor.NotifyAll()
}

// Ends the loop and joins it.
func (self *ExtenderActivator) Close() {
	self.closeOnce.Do(func() {
		if self.unsubNetworkChange != nil {
			self.unsubNetworkChange()
		}
		self.cancel()
		<-self.done
	})
}

// Publishes a status change. Called with the state lock held, so the change and
// its notification cannot be separated.
func (self *ExtenderActivator) changedWithLock() {
	self.version += 1
	self.changeMonitor.Set(self.version)
}

// The activation schedule of one family (G3): when it is next due, and how long
// its next failure holds it. The loop goroutine alone reads and writes it.
type extenderActivateSchedule struct {
	nextActivateTime time.Time
	backoff          time.Duration
}

// What one family's activation in a pass came to, which is what its schedule
// is set from.
type extenderActivateOutcome struct {
	succeeded bool
	// the wait the operator's refusal asked for (`retry_after_seconds`, C2),
	// 0 when it asked for none or the attempt was no refusal
	retryAfter time.Duration
}

// The keys of the activation schedules, one per family this activator runs, in
// ip version order. The plain api url has one schedule, under 0, since one
// post a cycle activates whichever family the answer names.
func (self *ExtenderActivator) scheduleIpVersions() []int {
	if self.fallback {
		return []int{0}
	}
	return self.ipVersions()
}

// The activation loop. One pass observes the directory for a revocation of this
// key, checks the caller address when it is due, and activates the families
// whose schedule or a trigger says so. Everything the pass needs is read after
// the subscribe above it, so a trigger that lands while the pass runs is
// carried into the next wait rather than lost.
//
// The deadlines are monotonic, and the monotonic clock stops while the host
// sleeps, so the wait reads the host clock at every wakeup, and at least every
// ResumeCheckTimeout, to tell a resume (hostResumeWatch). A resume pulls every
// deadline forward by the time slept and starts a pass.
func (self *ExtenderActivator) run() {
	scheduleIpVersions := self.scheduleIpVersions()
	ipVersionSchedules := map[int]*extenderActivateSchedule{}
	for _, ipVersion := range scheduleIpVersions {
		ipVersionSchedules[ipVersion] = &extenderActivateSchedule{
			backoff: self.settings.MinBackoff,
		}
	}
	// a trigger re-activates every family: the address the operator published
	// for each may no longer be this host's
	activateEveryFamily := func(now time.Time) {
		for _, schedule := range ipVersionSchedules {
			schedule.nextActivateTime = now
		}
	}
	var nextAddressCheckTime time.Time
	var clientAddress string

	passAfter := self.settings.PassAfter
	if passAfter == nil {
		passAfter = time.After
	}
	resumeWatch := newHostResumeWatch(
		self.settings.ResumeMinSleep,
		self.settings.ResumeCheckTimeout,
		self.settings.Now(),
	)

	for {
		select {
		case <-self.ctx.Done():
			return
		default:
		}

		now := self.settings.Now()
		wake := self.wakeMonitor.NotifyChannel()
		// the directory is read below, so the subscribe belongs here: a
		// revocation applied while this pass runs must wake the next wait.
		// With no directory the channel stays nil, which never fires.
		var directoryChange chan struct{}
		if self.settings.Directory != nil {
			_, directoryChange = self.settings.Directory.ChangeMonitor().Get()
		}

		if self.observeRevocation(now) {
			activateEveryFamily(now)
		}
		if self.takeForcedActivate() {
			activateEveryFamily(now)
		}

		if self.settings.HelloUrl != "" && !now.Before(nextAddressCheckTime) {
			address, err := self.hello()
			if err != nil {
				// a failed check retries on the backoff floor rather than on
				// every wake, which a directory change would otherwise be
				self.log.Infof("[extender]activate hello err = %s\n", httpRequestErrorWithCauses(err))
				nextAddressCheckTime = now.Add(self.settings.MinBackoff)
			} else {
				nextAddressCheckTime = now.Add(self.settings.AddressCheckTimeout)
				if clientAddress != "" && address != clientAddress {
					// the operator published an address this host no longer
					// has; the record must be reissued for the new one
					self.log.Infof(
						"[extender]activate address %s -> %s\n", clientAddress, address)
					activateEveryFamily(now)
				}
				clientAddress = address
			}
		}

		dueIpVersions := []int{}
		for _, ipVersion := range scheduleIpVersions {
			if !now.Before(ipVersionSchedules[ipVersion].nextActivateTime) {
				dueIpVersions = append(dueIpVersions, ipVersion)
			}
		}
		if 0 < len(dueIpVersions) {
			ipVersionOutcomes := self.activate(now, dueIpVersions)
			for _, ipVersion := range dueIpVersions {
				schedule := ipVersionSchedules[ipVersion]
				outcome := ipVersionOutcomes[ipVersion]
				switch {
				case outcome.succeeded:
					schedule.backoff = self.settings.MinBackoff
					schedule.nextActivateTime = now.Add(self.settings.ActivateTimeout)
				case 0 < outcome.retryAfter:
					// the operator said when it has room again (connect#230).
					// Its refusal judged nothing about this host, so the
					// backoff is left as it was rather than doubled
					schedule.nextActivateTime = now.Add(min(
						max(outcome.retryAfter, self.settings.MinRetryAfter),
						self.settings.MaxBackoff,
					))
				default:
					schedule.nextActivateTime = now.Add(schedule.backoff)
					schedule.backoff = min(2*schedule.backoff, self.settings.MaxBackoff)
				}
			}
		}

		// until the first deadline of any family, or of the address check
		var wait time.Duration
		waitSet := false
		waitUntil := func(deadline time.Time) {
			if remaining := deadline.Sub(now); !waitSet || remaining < wait {
				wait = remaining
				waitSet = true
			}
		}
		for _, schedule := range ipVersionSchedules {
			waitUntil(schedule.nextActivateTime)
		}
		if self.settings.HelloUrl != "" {
			waitUntil(nextAddressCheckTime)
		}
		if !waitSet || wait <= 0 {
			// nothing above left a deadline in the past; a zero here would be
			// a busy loop
			wait = self.settings.MinBackoff
		}

		dueAfter := passAfter(wait)
		for waiting := true; waiting; {
			// nil, which never fires, when resumes are not watched
			var checkAfter <-chan time.Time
			if resumeWatch.Watching() {
				checkAfter = passAfter(self.settings.ResumeCheckTimeout)
			}
			select {
			case <-self.ctx.Done():
				return
			case <-wake:
				waiting = false
			case <-directoryChange:
				waiting = false
			case <-dueAfter:
				waiting = false
			case <-checkAfter:
			}
			if sleep, resumed := resumeWatch.Check(self.settings.Now()); resumed {
				// what came due while the host slept is due now
				pullForward := func(deadline time.Time) time.Time {
					if deadline.IsZero() {
						return deadline
					}
					return deadline.Add(-sleep)
				}
				for _, schedule := range ipVersionSchedules {
					schedule.nextActivateTime = pullForward(schedule.nextActivateTime)
				}
				nextAddressCheckTime = pullForward(nextAddressCheckTime)
				self.log.Infof("[extender]activate resumed after %s asleep\n", sleep.Round(time.Second))
				waiting = false
			}
		}
	}
}

// Reports whether a revocation of this extender's key has just appeared in the
// directory (G3, B5). It is an edge, not a level: a re-activation that the
// operator refuses leaves the revocation standing, and retrying it on every
// directory change instead of on the backoff would spin.
func (self *ExtenderActivator) observeRevocation(now time.Time) (observed bool) {
	if self.settings.Directory == nil || len(self.settings.PublicKey) == 0 {
		return false
	}
	revoked := false
	for _, entry := range self.settings.Directory.Snapshot().Entries {
		if entry.State == ExtenderStateRevoked && slices.Equal(entry.PublicKey, self.settings.PublicKey) {
			revoked = true
			break
		}
	}

	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if revoked == self.revokedObserved {
		return false
	}
	self.revokedObserved = revoked
	if !revoked {
		return false
	}
	for _, family := range self.families {
		family.Activated = false
		family.RevokedTime = now
	}
	self.changedWithLock()
	return true
}

// Takes and clears the network change trigger.
func (self *ExtenderActivator) takeForcedActivate() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	forced := self.forcedActivate
	self.forcedActivate = false
	return forced
}

// Reads the caller address this host is seen at (G3). Only the address is
// read; the root keys of the same answer are the network client's business
// (B4, E3).
func (self *ExtenderActivator) hello() (string, error) {
	if self.settings.ClientStrategy == nil {
		return "", fmt.Errorf("the extender activation needs a client strategy")
	}
	ctx, cancel := context.WithTimeout(self.ctx, self.settings.RequestTimeout)
	defer cancel()
	request, err := HelloRequestFromUrl(ctx, self.settings.HelloUrl, self.byJwt())
	if err != nil {
		return "", err
	}
	bodyBytes, err := HttpGetWithStrategyRaw(ctx, self.settings.ClientStrategy, request.URL.String(), self.byJwt())
	if err != nil {
		return "", err
	}
	helloResult := &extenderHelloResult{}
	if err := json.Unmarshal(bodyBytes, helloResult); err != nil {
		return "", err
	}
	if helloResult.ClientAddress == "" {
		return "", fmt.Errorf("hello carried no client address")
	}
	return extenderClientAddressIp(helloResult.ClientAddress), nil
}

// The address half of a caller address. The operator reports the source port
// with it, and that changes on every connection: comparing the whole string
// would re-activate on every check (G3).
func extenderClientAddressIp(clientAddress string) string {
	clientAddress = strings.TrimSpace(clientAddress)
	if addrPort, err := netip.ParseAddrPort(clientAddress); err == nil {
		return addrPort.Addr().Unmap().String()
	}
	if addr, err := netip.ParseAddr(clientAddress); err == nil {
		return addr.Unmap().String()
	}
	if host, _, err := net.SplitHostPort(clientAddress); err == nil {
		return host
	}
	return clientAddress
}

func (self *ExtenderActivator) byJwt() string {
	if self.settings.ByJwt == nil {
		return ""
	}
	return self.settings.ByJwt()
}

// The dns ports this extender is listening on, ascending (L2). Nil when the
// caller offers none, which leaves the operator on DnsPort alone.
func (self *ExtenderActivator) dnsPorts() []int {
	if self.settings.DnsPorts == nil {
		return nil
	}
	if dnsPorts := orderedDnsPorts(self.settings.DnsPorts()); 0 < len(dnsPorts) {
		return dnsPorts
	}
	return nil
}

// Activates the due families that have an address on this host, each with its
// own post, and reports what each came to, keyed by schedule
// (scheduleIpVersions). Each family's outcome sets only its own schedule
// (connect#230): a family that passed is not posted again for the other's
// refusal, since that post would spend the passed family's activation budget
// to reissue a record it already holds. A family with no address on this host
// is not attempted and reports no success, so a host that gains an address is
// retried on that family's backoff rather than on the 24 hour tick.
//
// The pass as a whole -- every due family it attempted passed, and it
// attempted one -- is what decides the measurement follow-up (C4).
func (self *ExtenderActivator) activate(now time.Time, dueIpVersions []int) map[int]extenderActivateOutcome {
	carriers := []string{}
	if self.settings.Carriers != nil {
		carriers = self.settings.Carriers()
	}
	// a pass forced to carry the measurement request is followed by no
	// other, so a request that keeps missing cannot hold the loop (C4)
	followUp := self.measurementFollowUp
	self.measurementFollowUp = false

	ipVersionOutcomes := map[int]extenderActivateOutcome{}
	succeeded := false
	// whether a post of this pass delivered the measurement request
	measurementCarried := false
	if self.fallback {
		var outcome extenderActivateOutcome
		outcome, measurementCarried = self.activateFallback(carriers, now)
		ipVersionOutcomes[0] = outcome
		succeeded = outcome.succeeded
	} else {
		attempted := false
		succeeded = true
		for _, ipVersion := range dueIpVersions {
			if !self.ipVersionSupported(ipVersion) {
				// a family with no global address on this host cannot be probed
				// back, so it is skipped rather than refused (G3)
				continue
			}
			attempted = true
			if len(carriers) == 0 {
				self.recordFailure(ipVersion, now, "no carrier is listening", false)
				succeeded = false
				continue
			}
			// one request a pass, on the first post that can carry it (C4)
			measurementCsr := ""
			if !measurementCarried {
				measurementCsr = self.measurementCsr()
			}
			outcome := self.activateUrl(ipVersion, self.apiUrl(ipVersion), carriers, measurementCsr, now)
			ipVersionOutcomes[ipVersion] = outcome
			if outcome.succeeded {
				measurementCarried = measurementCarried || measurementCsr != ""
			} else {
				succeeded = false
			}
		}
		succeeded = attempted && succeeded
	}
	if succeeded && !followUp && !measurementCarried {
		self.followMeasurementCsr()
	}
	return ipVersionOutcomes
}

// The measurement request a post carries (lag.dev/PLAN.md C4): what the
// extender asks for under its id, and nothing until an answer has reported
// that id (N4), since the request's one name is made from it.
func (self *ExtenderActivator) measurementCsr() string {
	if self.settings.MeasurementCsr == nil || self.extenderId == (Id{}) || self.measurementDisabled() {
		return ""
	}
	return self.settings.MeasurementCsr(self.extenderId)
}

// Whether the owner has opted out of measurement (lag.dev/PLAN.md C4).
func (self *ExtenderActivator) measurementDisabled() bool {
	return self.settings.MeasurementDisabled != nil && self.settings.MeasurementDisabled()
}

// Follows a successful pass that delivered no measurement request with one
// more pass at once, when a request is wanted now. That is the first pass of
// a new identity with a single post -- one family, or the plain api url --
// which learned this extender's id from the answer to that post and so had no
// later post to carry the request on.
//
// lag.dev/PLAN.md C4 has a new identity's request ride its next activation
// post. With one post a pass that next post is the 24 hour tick away, and a
// process that restarts more often than that would never reach it, since the
// id is learned again from the first answer of each run. This one extra post
// is the smallest change that makes the next post follow the first.
func (self *ExtenderActivator) followMeasurementCsr() {
	if self.measurementCsr() == "" {
		// nothing is wanted: measurement is off, the id is not known, or the
		// operator already holds a request or a certificate for the name
		return
	}
	self.measurementFollowUp = true
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.forcedActivate = true
	}()
	self.wakeMonitor.NotifyAll()
}

func (self *ExtenderActivator) ipVersionSupported(ipVersion int) bool {
	if self.settings.IpVersionSupported != nil {
		return self.settings.IpVersionSupported(ipVersion)
	}
	return FamilySupported(ipVersion)
}

// Posts one activation to the plain api url, which is what an operator with no
// family api hosts offers. One post per cycle: there is no second url to reach
// the other family with, and the operator derives the family from the caller
// address, so the answer names whichever family this host's egress used. A host
// with no global address of either family is skipped exactly as a family
// without one is (G3). The one post carries the measurement request when one
// is wanted and this extender's id is known, and whether it delivered one
// comes back with the outcome (C4).
func (self *ExtenderActivator) activateFallback(carriers []string, now time.Time) (outcome extenderActivateOutcome, measurementCarried bool) {
	if !self.ipVersionSupported(4) && !self.ipVersionSupported(6) {
		return extenderActivateOutcome{}, false
	}
	if len(carriers) == 0 {
		self.recordFailure(0, now, "no carrier is listening", false)
		return extenderActivateOutcome{}, false
	}
	measurementCsr := self.measurementCsr()
	outcome = self.activateUrl(0, self.fallbackApiUrl(), carriers, measurementCsr, now)
	return outcome, outcome.succeeded && measurementCsr != ""
}

// Posts one activation and records what came back (C2). `ipVersion` is the
// family the outcome belongs to; 0 takes it from the answer, which is the
// fallback mode. `measurementCsr` is the request the post carries, empty for
// none (C4). A refusal that names when the operator has room again
// (`retry_after_seconds`) carries that wait in the outcome.
func (self *ExtenderActivator) activateUrl(
	ipVersion int,
	apiUrl string,
	carriers []string,
	measurementCsr string,
	now time.Time,
) extenderActivateOutcome {
	args := &ExtenderActivateArgs{
		PublicKeyHex:        hex.EncodeToString(self.settings.PublicKey),
		TcpPort:             self.settings.TcpPort,
		UdpPort:             self.settings.UdpPort,
		DnsPort:             self.settings.DnsPort,
		DnsPorts:            self.dnsPorts(),
		DnsTld:              self.settings.DnsTld,
		Carriers:            carriers,
		MeasurementCsr:      measurementCsr,
		MeasurementDisabled: self.measurementDisabled(),
	}
	// publish the camouflage key only when the role set it (EXTENDER.md P6); an
	// empty key is left off the args so an operator sees no camouflage key
	if 0 < len(self.settings.RealityPublicKey) {
		args.RealityPublicKeyHex = hex.EncodeToString(self.settings.RealityPublicKey)
	}
	ctx, cancel := context.WithTimeout(self.ctx, self.settings.RequestTimeout)
	defer cancel()
	result, err := PostExtenderActivate(
		ctx,
		self.settings.ClientStrategy,
		apiUrl,
		self.byJwt(),
		args,
	)
	if err != nil {
		// the causes of every attempt, which an exhausted request's own
		// message hides (connect#229)
		self.log.Infof("[extender]activate v%d err = %s\n", ipVersion, httpRequestErrorWithCauses(err))
		self.recordFailure(ipVersion, now, err.Error(), false)
		return extenderActivateOutcome{}
	}
	if result == nil {
		self.recordFailure(ipVersion, now, "the operator sent no activation result", false)
		return extenderActivateOutcome{}
	}
	if ipVersion == 0 {
		ipVersion = extenderActivateResultIpVersion(result)
	}
	if !result.Activated {
		message := result.Error
		if message == "" {
			message = "the operator refused the activation"
		}
		retryAfter := time.Duration(0)
		if 0 < result.RetryAfterSeconds {
			retryAfter = time.Duration(result.RetryAfterSeconds) * time.Second
			self.log.Infof("[extender]activate v%d refused = %s (retry after %s)\n", ipVersion, message, retryAfter)
		} else {
			self.log.Infof("[extender]activate v%d refused = %s\n", ipVersion, message)
		}
		self.recordFailure(ipVersion, now, message, true)
		return extenderActivateOutcome{retryAfter: retryAfter}
	}

	// the operator's id of this extender, the same at every activation, which
	// is what its measurement certificate request is named by (lag.dev/PLAN.md
	// N4, C4)
	if result.ExtenderId != nil && *result.ExtenderId != (Id{}) {
		self.extenderId = *result.ExtenderId
	}

	// the record is applied either way: it is signed and carries its own
	// addresses, which the directory keys by the public key rather than by the
	// family this loop tracks
	self.applyRecords(result)
	if ipVersion == 0 {
		// an activation with no family cannot be attributed to one, and the
		// mesh address of D2 has no family to be published under
		self.recordFailure(ipVersion, now, "the operator named no address family", false)
		return extenderActivateOutcome{}
	}
	self.recordSuccess(ipVersion, now, result)
	if self.settings.OnActivated != nil {
		self.settings.OnActivated(ipVersion, result)
	}
	return extenderActivateOutcome{succeeded: true}
}

// The family an activation answer belongs to: what the operator reported, else
// the family of the address it published, else 0.
func extenderActivateResultIpVersion(result *ExtenderActivateResult) int {
	switch result.IpVersion {
	case 4, 6:
		return result.IpVersion
	}
	ip, err := netip.ParseAddr(result.Ip)
	if err != nil {
		return 0
	}
	if ip.Unmap().Is6() {
		return 6
	}
	return 4
}

// Applies this extender's own record and the bootstrap sample into the
// directory (C2, D6). Both carry the bootstrap source: they came from the
// activation, not from the feed or the mesh. A record that does not verify is
// dropped with a log -- the activation itself still stands, because the
// operator proved the carriers.
func (self *ExtenderActivator) applyRecords(result *ExtenderActivateResult) {
	if self.settings.Directory == nil {
		return
	}
	// this extender's own record is one the directory's cap never evicts: its
	// peers judge its pings by it, and its feed serves it first (D26)
	self.settings.Directory.KeepPublicKey(self.settings.PublicKey)
	apply := func(recordBase64 string) {
		recordBytes, err := base64.StdEncoding.DecodeString(recordBase64)
		if err != nil {
			self.log.Infof("[extender]activate record err = %s\n", err)
			return
		}
		record := &protocol.ExtenderRecord{}
		if err := proto.Unmarshal(recordBytes, record); err != nil {
			self.log.Infof("[extender]activate record err = %s\n", err)
			return
		}
		if _, err := self.settings.Directory.ApplyRecord(record, ExtenderSourceBootstrap); err != nil {
			self.log.Infof("[extender]activate record err = %s\n", err)
		}
	}
	if result.Record != "" {
		apply(result.Record)
	}
	for _, recordBase64 := range result.Bootstrap {
		apply(recordBase64)
	}
}

// The entry one outcome is recorded under. Every family this activator runs
// has an entry from construction, so an unknown family records nothing. In the
// fallback mode the family is whatever the answer named, so the entry is
// created on first use and the placeholder of an answer that named none is
// dropped by the first one that does.
func (self *ExtenderActivator) familyWithLock(ipVersion int) *ExtenderFamilyActivationStatus {
	if family, ok := self.families[ipVersion]; ok {
		return family
	}
	if !self.fallback {
		return nil
	}
	if 0 < ipVersion {
		delete(self.families, 0)
	}
	family := &ExtenderFamilyActivationStatus{IpVersion: ipVersion}
	self.families[ipVersion] = family
	return family
}

func (self *ExtenderActivator) recordSuccess(
	ipVersion int,
	now time.Time,
	result *ExtenderActivateResult,
) {
	ip, _ := netip.ParseAddr(result.Ip)

	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	family := self.familyWithLock(ipVersion)
	if family == nil {
		return
	}
	family.Activated = true
	family.Ip = ip.Unmap()
	family.LastActivationTime = now
	family.LastError = ""
	family.LastRefused = false
	family.RevokedTime = time.Time{}
	family.AllowedHosts = slices.Clone(result.AllowedHosts)
	family.ExpireTime = time.Time{}
	if result.ExpireTime != nil {
		family.ExpireTime = *result.ExpireTime
	}
	self.changedWithLock()
}

// Records a failed attempt. `refused` is true only for the operator's refusal,
// an answer with `activated` false; every other failure passes false, so the
// status tells the two apart (N7).
func (self *ExtenderActivator) recordFailure(
	ipVersion int,
	now time.Time,
	message string,
	refused bool,
) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	families := []*ExtenderFamilyActivationStatus{}
	if ipVersion == 0 && 0 < len(self.families) {
		// the answer named no family, and in the fallback mode every family
		// was activated through the one url this failure came from
		for _, family := range self.families {
			families = append(families, family)
		}
	} else if family := self.familyWithLock(ipVersion); family != nil {
		families = append(families, family)
	}
	if len(families) == 0 {
		return
	}
	for _, family := range families {
		family.Activated = false
		family.LastActivationTime = now
		family.LastError = message
		family.LastRefused = refused
	}
	self.changedWithLock()
}
