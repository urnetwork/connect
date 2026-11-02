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
// Each family keeps its own schedule (G3). It is posted at start, on its own 24
// hour tick, and when a trigger makes it due: the own key observed revoked in
// the directory and a network change make every family due, and a caller
// address from hello that the family no longer matches makes that family due.
// A refusal or a failure holds only the family that had it, for ten minutes
// doubling to six hours, and a success resets that family's hold. So a family
// the operator always refuses -- a v4 behind a NAT -- no longer re-posts the
// family that works on every step of its backoff. A refusal that says when the
// operator has room again (`retry_after_seconds`: a spent activation budget,
// or an operator too busy to probe; connect#230) holds the family that long
// instead, within a minute and six hours, and leaves its hold as it was. The
// ticks, the address check and the holds count the time the host slept, from
// the first minute it is awake after a sleep of fifteen minutes or more.
//
// Every clock and every side effect is a settings seam -- `Now`, `PassAfter`,
// `IpVersionSupported`, `IpVersionPublic`, `AddNetworkChangeListener` -- so
// the whole loop is deterministic in tests.
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
	// the run of the extender process that posted this activation (G3,
	// ExtenderRun): 32 lower case hex characters of 128 random bits drawn
	// once when the process starts, so every activation of one process
	// carries the same id and a restarted process a new one. An operator that
	// predates the three run fields ignores them.
	RunId string `json:"run_id,omitempty"`
	// when that process started, utc
	RunStartTime *time.Time `json:"run_start_time,omitempty"`
	// how the run before it ended, as the host that restarted the process
	// reported it (ExtenderPreviousExitEnv); empty when it reported none
	PreviousExit string `json:"previous_exit,omitempty"`
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
	// PublishedRealityPublicKey, when set, is read at each post and replaces
	// RealityPublicKey: a role's verified names change while it runs, so what it
	// publishes does too (the role's ExtenderCamouflageRole). It runs on the
	// loop goroutine and must not block.
	PublishedRealityPublicKey func() []byte

	// The carrier ports and the encoding tld this extender serves (C1, C2).
	// Zero ports and an empty tld take the operator's defaults.
	TcpPort int
	UdpPort int
	DnsPort int
	DnsTld  string

	// The carriers that are listening right now (G2). Only these are offered,
	// so a carrier whose bind failed is never probed. Nil offers none, which
	// holds every activation, as does a list with no carrier the operator can
	// accept an activation on (C2a): a post then could only spend the operator's
	// probe budget or be refused (TcpCarrierReturned).
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
	// then, the families that pass activated are posted once more at once to
	// carry it -- so this may be asked with no post following. It runs on the
	// loop goroutine and must not block.
	MeasurementCsr func(extenderId Id) string
	// MeasurementDisabled, when set, reports whether the owner has opted out of
	// measurement, or the extender cannot measure for want of a usable key
	// (lag.dev/PLAN.md C4): every post made while it reports true carries
	// measurement_disabled and no request, so the operator withdraws the
	// measurement name (C9). It runs on the loop goroutine and must not block.
	MeasurementDisabled func() bool

	// The run of the process every post reports (G3). Nil takes
	// ExtenderProcessRun(), which is what every host wants; a test sets its
	// own.
	Run *ExtenderRun

	// The re-activation period of each family (G3).
	ActivateTimeout time.Duration
	// The caller address check period (G3).
	AddressCheckTimeout time.Duration
	// Backoff of a family after its failure, doubling to the max (G3). Each
	// family keeps its own.
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
	// IpVersionPublic, when set, replaces the probe of whether the host has a
	// public address of the family, one the operator can dial back without a
	// port forward (G3). Nil reads the interface table as FamilyPublic does,
	// except that a table that cannot be read answers yes; or it answers as
	// IpVersionSupported does when that is set, so a seam that replaces the
	// host's family probe replaces both answers.
	IpVersionPublic func(ipVersion int) bool
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
	// set by a network change, taken by the next pass, which then posts every
	// family
	forcedActivate bool
	// set when the tcp carrier comes back, taken by the next pass, which then
	// posts every family that is not activated
	tcpCarrierReturned bool

	// this extender's id as the operator's last answer reported it, zero
	// until one has (lag.dev/PLAN.md N4), and whether the last pass forced this
	// one to carry the measurement request (C4), so that it is followed by no
	// other. Both are read and written by the loop goroutine alone.
	extenderId          Id
	measurementFollowUp bool

	// The schedule of each family (G3), keyed by ip version, or by 0 for the
	// one post a cycle of the fallback mode; and the address the caller address
	// check last saw on each family. Both are read and written by the loop
	// goroutine alone.
	schedules      map[int]*extenderActivateSchedule
	helloAddresses map[int]netip.Addr
}

// When one family is next posted, and how long its next failure holds it
// (G3). Every family keeps its own, so one family's refusals never hold or
// re-post another.
type extenderActivateSchedule struct {
	// the zero time is due at once, which is what start and every trigger set
	nextActivateTime time.Time
	backoff          time.Duration
	// whether the family has activated since the activator started. A
	// private family that has is reachable through whatever forwards to it,
	// so its refusals are held as any family's are (activate).
	activatedInRun bool
}

// After a success: the family's next post is its tick, and its hold is back to
// the floor.
func (self *extenderActivateSchedule) tick(now time.Time, settings *ExtenderActivatorSettings) {
	self.nextActivateTime = now.Add(settings.ActivateTimeout)
	self.backoff = settings.MinBackoff
	self.activatedInRun = true
}

// After a failure: the family waits out its hold, which doubles to the cap for
// the next one.
func (self *extenderActivateSchedule) hold(now time.Time, settings *ExtenderActivatorSettings) {
	self.nextActivateTime = now.Add(self.backoff)
	self.backoff = min(2*self.backoff, settings.MaxBackoff)
}

// After a pass that found no address for the family on the host: it is looked
// at again after the floor, every time, since the look is local and costs
// nothing, so a family that gains an address is posted within the floor
// however long it went without one. Its first failure once it has one holds
// the floor too.
func (self *extenderActivateSchedule) recheck(now time.Time, settings *ExtenderActivatorSettings) {
	self.nextActivateTime = now.Add(settings.MinBackoff)
	self.backoff = settings.MinBackoff
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
	if copied.Run == nil {
		copied.Run = ExtenderProcessRun()
	}
	settings = &copied

	cancelCtx, cancel := context.WithCancel(ctx)
	self := &ExtenderActivator{
		ctx:            cancelCtx,
		cancel:         cancel,
		done:           make(chan struct{}),
		log:            loggerOrDefault(settings.Log),
		settings:       settings,
		changeMonitor:  NewMonitorValue[uint64](0),
		wakeMonitor:    NewMonitor(),
		families:       map[int]*ExtenderFamilyActivationStatus{},
		schedules:      map[int]*extenderActivateSchedule{},
		helloAddresses: map[int]netip.Addr{},
	}
	// the run every post reports, so a host log names the run the operator's
	// activation rows carry (G3)
	self.log.Infof(
		"[extender]activate run = %s start = %s previous exit = %q\n",
		settings.Run.Id,
		settings.Run.StartTime.UTC().Format(time.RFC3339),
		settings.Run.PreviousExit,
	)
	self.fallback = len(self.ipVersions()) == 0 && self.fallbackApiUrl() != ""
	for _, ipVersion := range self.ipVersions() {
		self.families[ipVersion] = &ExtenderFamilyActivationStatus{IpVersion: ipVersion}
	}
	// every family is due at start
	for _, key := range self.scheduleKeys() {
		self.schedules[key] = &extenderActivateSchedule{backoff: settings.MinBackoff}
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

// The keys of the schedules this activator keeps, in order (G3): one per family
// it runs, or the one post a cycle of the fallback mode under 0, whose family
// only the answer names.
func (self *ExtenderActivator) scheduleKeys() []int {
	if self.fallback {
		return []int{0}
	}
	return self.ipVersions()
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

// Says the tcp carrier is serving again after it was lost (G2): every family
// that is not activated is due at once, whatever its hold says, and a family
// that stayed activated is left to its tick. While the carrier was gone nothing
// was posted and the holds kept doubling, so without this a family whose
// revocation arrived in the meantime could wait up to the hold's cap with a
// working listener. A carrier that came back before anything was owed posts
// nothing, so a flapping listener does not spend the operator's budgets. Safe to
// call from any goroutine.
func (self *ExtenderActivator) TcpCarrierReturned() {
	self.stateLock.Lock()
	self.tcpCarrierReturned = true
	self.stateLock.Unlock()
	self.wakeMonitor.NotifyAll()
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

// The activation loop. One pass observes the directory for a revocation of this
// key, checks the caller address when it is due, and posts the families their
// schedules or a trigger make due (G3). Everything the pass needs is read after
// the subscribe above it, so a trigger that lands while the pass runs is
// carried into the next wait rather than lost.
//
// The deadlines are monotonic, and the monotonic clock stops while the host
// sleeps, so the wait reads the host clock at every wakeup, and at least every
// ResumeCheckTimeout, to tell a resume (hostResumeWatch). A resume pulls every
// deadline forward by the time slept and starts a pass.
func (self *ExtenderActivator) run() {
	var nextAddressCheckTime time.Time

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

		// the clock and the directory are read below, so the subscribes belong
		// ahead of both: a revocation applied while this pass runs, or a wake
		// that arrives as it starts, must wake the next wait, and a wake ahead
		// of the subscribes is one this pass's reads already see. With no
		// directory the channel stays nil, which never fires.
		wake := self.wakeMonitor.NotifyChannel()
		var directoryChange chan struct{}
		if self.settings.Directory != nil {
			_, directoryChange = self.settings.Directory.ChangeMonitor().Get()
		}
		now := self.settings.Now()

		// a revocation and a network change leave every record of this host in
		// doubt, so every family is due, whatever its own schedule says
		if self.observeRevocation(now) {
			self.scheduleEveryFamily()
		}
		if self.takeForcedActivate() {
			self.scheduleEveryFamily()
		}
		if self.takeTcpCarrierReturned() {
			self.scheduleUnactivatedFamilies()
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
				self.checkAddress(address)
			}
		}

		if due := self.dueScheduleKeys(now); 0 < len(due) {
			self.activate(now, due)
		}

		wait := self.nextActivateWait(now)
		if self.settings.HelloUrl != "" {
			wait = min(wait, nextAddressCheckTime.Sub(now))
		}
		if wait <= 0 {
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
				for _, schedule := range self.schedules {
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

// Makes every family due at once, as start does (G3).
func (self *ExtenderActivator) scheduleEveryFamily() {
	for _, schedule := range self.schedules {
		schedule.nextActivateTime = time.Time{}
	}
}

// Takes and clears the tcp carrier's return.
func (self *ExtenderActivator) takeTcpCarrierReturned() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	returned := self.tcpCarrierReturned
	self.tcpCarrierReturned = false
	return returned
}

// Makes the families that are not activated due at once (TcpCarrierReturned).
// The one schedule of the fallback mode is due unless every family an answer
// has named stands activated.
func (self *ExtenderActivator) scheduleUnactivatedFamilies() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.fallback {
		activated := 0 < len(self.families)
		for _, family := range self.families {
			activated = activated && family.Activated
		}
		if !activated {
			self.scheduleEveryFamily()
		}
		return
	}
	for key, schedule := range self.schedules {
		if family, ok := self.families[key]; !ok || !family.Activated {
			schedule.nextActivateTime = time.Time{}
		}
	}
}

// The schedules due at `now`, in key order, so a pass posts v4 before v6 as it
// always has.
func (self *ExtenderActivator) dueScheduleKeys(now time.Time) []int {
	due := []int{}
	for _, key := range self.scheduleKeys() {
		if schedule := self.schedules[key]; !now.Before(schedule.nextActivateTime) {
			due = append(due, key)
		}
	}
	return due
}

// How long until the next family is due. With no family to post there is no
// deadline but the triggers, so the wait is the tick.
func (self *ExtenderActivator) nextActivateWait(now time.Time) time.Duration {
	wait := self.settings.ActivateTimeout
	for _, schedule := range self.schedules {
		wait = min(wait, schedule.nextActivateTime.Sub(now))
	}
	return wait
}

// Acts on the caller address one check read (G3): makes due the family whose
// published address it shows has moved.
func (self *ExtenderActivator) checkAddress(clientAddress string) {
	key, previous, changed := self.addressChange(clientAddress)
	if !changed {
		return
	}
	// the operator published an address this host no longer has; the record
	// must be reissued for the new one
	self.log.Infof("[extender]activate address %s -> %s\n", previous, clientAddress)
	self.schedules[key].nextActivateTime = time.Time{}
}

// Whether a caller address from hello shows a family's published address has
// moved, and then the schedule to post and the address it moved from (G3).
//
// The check is one hello, to the plain api url, so the family it arrives on is
// the host's choice and can differ from one check to the next: a dual-stack
// host reports its v4 address one hour and its v6 address the next. Compared
// with the last check, every such switch read as a moved address and re-posted
// both families (the lag.dev health check, 2026-11-01). So an address is read
// within its own family: one the operator published for any family is
// unchanged, and so is the one the last check saw on this family, which keeps
// a host whose hello leaves from another of its addresses than its activation
// from re-posting on every check. That address holds only while the family's
// published address is the one it was read against: an activation that moves
// the published address forgets it (activateUrl), so a host that returns to
// an address it left is posted at the next check rather than its tick. A
// family that is not activated is left to its own schedule, which posts it
// when it is due. In the fallback mode the one post a cycle is what can
// reissue the record, so the schedule is 0.
func (self *ExtenderActivator) addressChange(clientAddress string) (key int, previous netip.Addr, changed bool) {
	address, err := netip.ParseAddr(clientAddress)
	if err != nil {
		// the operator reports an ip; an answer that is not one says nothing
		// of either family
		return 0, netip.Addr{}, false
	}
	address = address.Unmap()
	ipVersion := 6
	if address.Is4() {
		ipVersion = 4
	}
	lastAddress, seen := self.helloAddresses[ipVersion]
	self.helloAddresses[ipVersion] = address

	published, activated, activatedAddress := func() (bool, bool, netip.Addr) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		published := false
		activated := false
		var activatedAddress netip.Addr
		for _, family := range self.families {
			if family.Ip.IsValid() && family.Ip == address {
				published = true
			}
			if family.IpVersion == ipVersion && family.Activated {
				activated = true
				activatedAddress = family.Ip
			}
		}
		return published, activated, activatedAddress
	}()
	if published || (seen && lastAddress == address) || !activated {
		return 0, netip.Addr{}, false
	}
	if self.fallback {
		return 0, activatedAddress, true
	}
	return ipVersion, activatedAddress, true
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

// What one post, or one family's turn in a pass, came to.
type extenderActivateOutcome int

const (
	// the family has no address on this host, so nothing was posted (G3)
	extenderActivateSkipped extenderActivateOutcome = iota
	// the post failed, or there was nothing to post
	extenderActivateFailed
	// the operator answered `activated` false (N7)
	extenderActivateRefused
	extenderActivateActivated
)

// Posts the families in `due`, in key order, and moves each one's schedule by
// its own outcome (G3): a success to its tick, a failure onto its own hold,
// and a family with no address on this host to a look again after the floor.
// So a family the operator keeps refusing holds itself alone and never
// re-posts another, and a family that gains an address is posted within the
// floor rather than at its tick. A refusal that names when the operator has
// room again (`retry_after_seconds`: a spent activation budget, or an operator
// too busy to probe; connect#230) is waited out instead, within MinRetryAfter
// and MaxBackoff, and leaves the hold as it was: the operator probed nothing,
// so it judged nothing about this host.
//
// One other refusal is held until the tick instead: that of a family whose host
// addresses are all private and which has not activated, neither since the
// activator started nor as far as the directory knows. The operator can dial
// such an address back only through something that forwards to it, and a NAT
// that never forwards refuses every retry alike, so it gets one post at start
// and then one a day, as the owner decided (2026-11-01). Once the family has
// activated something does forward to it, so its refusals are the operator's
// budgets, its probes and its revocations, which the hold retries as for any
// family. The directory carries that across a restart: this extender's own
// current record naming an address of the family is the operator's word that
// it published the family after its probes passed (ownRecordNamesFamily). The
// schedules move once every post of the pass has been answered, so the record
// another family's answer brought counts too.
func (self *ExtenderActivator) activate(now time.Time, due []int) {
	carriers := []string{}
	if self.settings.Carriers != nil {
		carriers = self.settings.Carriers()
	}
	// a pass forced to carry the measurement request is followed by no
	// other, so a request that keeps missing cannot hold the loop (C4)
	followUp := self.measurementFollowUp
	self.measurementFollowUp = false

	outcomes := make([]extenderActivateOutcome, len(due))
	// the wait each refusal named for its family's next post, if any
	retryAfters := make([]time.Duration, len(due))
	// whether a post of this pass delivered the measurement request
	measurementCarried := false
	for i, key := range due {
		var carried bool
		if self.fallback {
			outcomes[i], carried, retryAfters[i] = self.activateFallback(carriers, now)
		} else {
			outcomes[i], carried, retryAfters[i] = self.activateFamily(key, carriers, measurementCarried, now)
		}
		measurementCarried = measurementCarried || carried
	}

	activated := []int{}
	for i, key := range due {
		schedule := self.schedules[key]
		switch outcome := outcomes[i]; {
		case outcome == extenderActivateActivated:
			schedule.tick(now, self.settings)
			activated = append(activated, key)
		case outcome == extenderActivateSkipped:
			schedule.recheck(now, self.settings)
		case outcome == extenderActivateRefused && 0 < retryAfters[i]:
			// the operator said when it has room again (C2, connect#230): a
			// spent activation budget, or an operator too busy to probe. It
			// probed nothing, so the refusal says nothing about this host,
			// private or not: the family waits that long, within
			// MinRetryAfter and MaxBackoff, and its hold stays as it was
			schedule.nextActivateTime = now.Add(min(
				max(retryAfters[i], self.settings.MinRetryAfter),
				self.settings.MaxBackoff,
			))
		case outcome == extenderActivateRefused &&
			!self.fallback &&
			!schedule.activatedInRun &&
			!self.ownRecordNamesFamily(key) &&
			!self.ipVersionPublic(key):
			// a NAT that never forwarded refuses every retry alike, so the
			// family waits for its tick; its hold stays as it was
			schedule.nextActivateTime = now.Add(self.settings.ActivateTimeout)
		default:
			schedule.hold(now, self.settings)
		}
	}
	if 0 < len(activated) && !followUp && !measurementCarried {
		self.followMeasurementCsr(activated)
	}
}

// Whether the directory holds this extender's own current record naming an
// address of the family: the operator published the family after its probes
// passed, in this run or an earlier one whose record the directory kept or the
// feed brought back (G3). Only the newest record of the key counts, valid,
// neither revoked nor expired; an address an older record named and the newest
// no longer does is the operator no longer publishing it, though the directory
// keeps the address. Local dial evidence does not enter, so an address in
// warning or hold counts (ExtenderDirectory.CurrentRecordNamesFamily).
func (self *ExtenderActivator) ownRecordNamesFamily(ipVersion int) bool {
	if self.settings.Directory == nil || len(self.settings.PublicKey) == 0 {
		return false
	}
	return self.settings.Directory.CurrentRecordNamesFamily(self.settings.PublicKey, ipVersion)
}

// One family's turn in a pass: its post to its family api url, unless the host
// has no address of the family or no carrier is listening. The measurement
// request rides it when no earlier post of the pass carried one (C4), and
// whether this post delivered it comes back with the outcome.
func (self *ExtenderActivator) activateFamily(
	ipVersion int,
	carriers []string,
	measurementCarried bool,
	now time.Time,
) (outcome extenderActivateOutcome, carried bool, retryAfter time.Duration) {
	if !self.ipVersionSupported(ipVersion) {
		// a family with no global address on this host cannot be probed
		// back, so it is skipped rather than refused (G3)
		return extenderActivateSkipped, false, 0
	}
	if message := extenderActivateCarrierFailure(carriers); message != "" {
		self.recordFailure(ipVersion, now, message, false)
		return extenderActivateFailed, false, 0
	}
	// one request a pass, on the first post that can carry it (C4)
	measurementCsr := ""
	if !measurementCarried {
		measurementCsr = self.measurementCsr()
	}
	outcome, retryAfter = self.activateUrl(ipVersion, self.apiUrl(ipVersion), carriers, measurementCsr, now)
	return outcome, outcome == extenderActivateActivated && measurementCsr != "", retryAfter
}

// Why these carriers cannot be offered to the operator, empty when they can
// (G2, C2a). The operator's required test dials the tcp carrier, or the webrtc
// carrier once that path lands, so a post with neither could only be refused, and
// a listener that is down must not keep posting: a post it would fail is a probe
// of the operator's that an hourly budget pays for, and that budget is its
// account's other extenders' too.
func extenderActivateCarrierFailure(carriers []string) string {
	if len(carriers) == 0 {
		return "no carrier is listening"
	}
	if !slices.Contains(carriers, ExtenderCarrierTcp) && !slices.Contains(carriers, ExtenderCarrierWebRtc) {
		return "the tcp carrier is not listening"
	}
	return ""
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

// Follows a pass that activated a family and delivered no measurement request
// with one more post at once on each family it activated, when a request is
// wanted now. That is the first pass of a new identity whose last post was the
// first to be answered -- one family, the plain api url, or a family after one
// the operator refused -- which learned this extender's id from that answer and
// so had no later post to carry the request on. The families the pass
// activated are the ones the request is posted to, since those are the ones
// the operator just answered for.
//
// lag.dev/PLAN.md C4 has a new identity's request ride its next activation
// post. With one post a pass that next post is the 24 hour tick away, and a
// process that restarts more often than that would never reach it, since the
// id is learned again from the first answer of each run. This one extra post
// is the smallest change that makes the next post follow the first.
func (self *ExtenderActivator) followMeasurementCsr(keys []int) {
	if self.measurementCsr() == "" {
		// nothing is wanted: measurement is off, the id is not known, or the
		// operator already holds a request or a certificate for the name
		return
	}
	self.measurementFollowUp = true
	for _, key := range keys {
		self.schedules[key].nextActivateTime = time.Time{}
	}
	self.wakeMonitor.NotifyAll()
}

func (self *ExtenderActivator) ipVersionSupported(ipVersion int) bool {
	if self.settings.IpVersionSupported != nil {
		return self.settings.IpVersionSupported(ipVersion)
	}
	return FamilySupported(ipVersion)
}

// Whether this host has a public address of the family, one the operator can
// dial back without a port forward (G3). An interface table that cannot be
// read is no evidence of a NAT, so here it answers yes, which holds a refusal
// on the backoff; FamilyPublic itself fails closed.
func (self *ExtenderActivator) ipVersionPublic(ipVersion int) bool {
	if self.settings.IpVersionPublic != nil {
		return self.settings.IpVersionPublic(ipVersion)
	}
	if self.settings.IpVersionSupported != nil {
		return self.settings.IpVersionSupported(ipVersion)
	}
	public, readable := familyPublicReadable(ipVersion)
	return public || !readable
}

// Posts one activation to the plain api url, which is what an operator with no
// family api hosts offers. One post per cycle: there is no second url to reach
// the other family with, and the operator derives the family from the caller
// address, so the answer names whichever family this host's egress used. A host
// with no global address of either family is skipped exactly as a family
// without one is (G3). The one post carries the measurement request when one
// is wanted and this extender's id is known, and whether it delivered one
// comes back with the outcome (C4).
func (self *ExtenderActivator) activateFallback(carriers []string, now time.Time) (outcome extenderActivateOutcome, carried bool, retryAfter time.Duration) {
	if !self.ipVersionSupported(4) && !self.ipVersionSupported(6) {
		return extenderActivateSkipped, false, 0
	}
	if message := extenderActivateCarrierFailure(carriers); message != "" {
		self.recordFailure(0, now, message, false)
		return extenderActivateFailed, false, 0
	}
	measurementCsr := self.measurementCsr()
	outcome, retryAfter = self.activateUrl(0, self.fallbackApiUrl(), carriers, measurementCsr, now)
	return outcome, outcome == extenderActivateActivated && measurementCsr != "", retryAfter
}

// Posts one activation and records what came back (C2). `ipVersion` is the
// family the outcome belongs to; 0 takes it from the answer, which is the
// fallback mode. `measurementCsr` is the request the post carries, empty for
// none (C4). A refusal that names when the operator has room again
// (`retry_after_seconds`, C2) returns that wait with it; every other outcome
// returns none.
func (self *ExtenderActivator) activateUrl(
	ipVersion int,
	apiUrl string,
	carriers []string,
	measurementCsr string,
	now time.Time,
) (extenderActivateOutcome, time.Duration) {
	runStartTime := self.settings.Run.StartTime.UTC()
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
		// the same run on every post of this process (G3)
		RunId:        self.settings.Run.Id,
		RunStartTime: &runStartTime,
		PreviousExit: self.settings.Run.PreviousExit,
	}
	// publish the camouflage key only when the role set it (EXTENDER.md P6); an
	// empty key is left off the args so an operator sees no camouflage key
	realityPublicKey := self.settings.RealityPublicKey
	if self.settings.PublishedRealityPublicKey != nil {
		realityPublicKey = self.settings.PublishedRealityPublicKey()
	}
	if 0 < len(realityPublicKey) {
		args.RealityPublicKeyHex = hex.EncodeToString(realityPublicKey)
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
		return extenderActivateFailed, 0
	}
	if result == nil {
		self.recordFailure(ipVersion, now, "the operator sent no activation result", false)
		return extenderActivateFailed, 0
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
		return extenderActivateRefused, retryAfter
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
		return extenderActivateFailed, 0
	}
	if moved := self.recordSuccess(ipVersion, now, result); moved {
		// the address the last check saw on this family was read against
		// the address the operator published before, so it says nothing of
		// the one published now: a check that sees it again must compare it
		// with this one (addressChange)
		delete(self.helloAddresses, ipVersion)
	}
	if self.settings.OnActivated != nil {
		self.settings.OnActivated(ipVersion, result)
	}
	return extenderActivateActivated, 0
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

// Records a successful activation, and reports whether it moved the family's
// published address from another one.
func (self *ExtenderActivator) recordSuccess(
	ipVersion int,
	now time.Time,
	result *ExtenderActivateResult,
) (moved bool) {
	ip, _ := netip.ParseAddr(result.Ip)
	ip = ip.Unmap()

	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	family := self.familyWithLock(ipVersion)
	if family == nil {
		return false
	}
	moved = family.Ip.IsValid() && family.Ip != ip
	family.Activated = true
	family.Ip = ip
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
	return moved
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
