package connect

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"sync"
	"time"

	// "google.golang.org/protobuf/proto"

	// "github.com/urnetwork/glog"

	"github.com/urnetwork/connect/protocol"
)

type MultiClientGeneratorClientArgs struct {
	ClientId   Id
	ClientAuth *ClientAuth
	P2pOnly    bool
}

// NetworkClientCredentials is an optional in-process authority for the
// generator's existing derived-client lifecycle. It must honor ctx and keep
// the same ownership/authorization rules as the API. Implementations own
// their parent credential; SetByJwt updates only the default HTTP authority.
// A returned error never falls back to HTTP: a mint may already have committed.
type NetworkClientCredentials interface {
	AuthNetworkClient(context.Context, *AuthNetworkClientArgs) (*AuthNetworkClientResult, error)
	RemoveNetworkClient(context.Context, *RemoveNetworkClientArgs) (*RemoveNetworkClientResult, error)
}

// A caller-owned discovery authority preserves request context, credentials,
// filters and ranking. A local error never falls back to public HTTP.
type NetworkProviderDiscovery interface {
	FindProviders2(context.Context, string, *FindProviders2Args) (*FindProviders2Result, error)
}

func DefaultApiMultiClientGeneratorSettings() *ApiMultiClientGeneratorSettings {
	return &ApiMultiClientGeneratorSettings{
		MigrateConnectTimeout:        60 * time.Second,
		MigrateMaxScheduleDelay:      5 * time.Minute,
		IdentityLoadTimeout:          5 * time.Second,
		RuntimeExcludeClientMaxCount: defaultApiRuntimeExcludeClientMaxCount(),
	}
}

// Retain one complete live-window generation for every normal maintenance
// opportunity across a channel lifetime. Reliability exclusions are rare in a
// healthy session, but this makes an incident's request and map growth finite
// without inventing a second unrelated sizing constant.
func apiRuntimeExcludeClientMaxCount(settings *MultiClientSettings) int {
	liveClientMaxCount := 0
	for _, windowSize := range settings.WindowSizes {
		liveClientMaxCount += max(0, windowSize.WindowSizeHardMax)
	}
	liveClientMaxCount = max(1, liveClientMaxCount)

	maintenanceCount := 1
	if 0 < settings.MaxClientLifetime && 0 < settings.WindowResizeTimeout {
		maintenanceCount = int(settings.MaxClientLifetime / settings.WindowResizeTimeout)
		if settings.MaxClientLifetime%settings.WindowResizeTimeout != 0 {
			maintenanceCount += 1
		}
	}
	return liveClientMaxCount * max(1, maintenanceCount)
}

func defaultApiRuntimeExcludeClientMaxCount() int {
	return apiRuntimeExcludeClientMaxCount(DefaultMultiClientSettings())
}

// Constructor policy is durable, but duplicate ids add no policy and should
// not inflate every discovery request. Preserve first-seen order and detach
// the generator from the caller's mutable slice.
func cloneUniqueApiExcludeClientIds(clientIds []Id) []Id {
	uniqueClientIds := make([]Id, 0, len(clientIds))
	seen := map[Id]bool{}
	for _, clientId := range clientIds {
		if seen[clientId] {
			continue
		}
		seen[clientId] = true
		uniqueClientIds = append(uniqueClientIds, clientId)
	}
	return uniqueClientIds
}

type ApiMultiClientGeneratorSettings struct {
	// ControlTelemetryProbe marks only this generator's OOB control POSTs.
	// Captured at construction; telemetry only, untrusted by the server.
	ControlTelemetryProbe bool
	// ClientCredentials is captured at construction, not read from mutable
	// settings during a mint or retirement. Nil preserves the public API path.
	ClientCredentials NetworkClientCredentials
	// Captured per generator; nil retains the ordinary HTTP control path.
	ClientControl NetworkClientControl
	// Captured per generator; nil preserves ordinary HTTP discovery.
	ProviderDiscovery NetworkProviderDiscovery
	// MigrateConnectTimeout bounds the temporary second platform transport.
	// If it cannot establish a route in this interval, it is closed and the
	// old transport remains until the server's drain fallback evicts it.
	MigrateConnectTimeout time.Duration
	// MigrateMaxScheduleDelay bounds an absolute server-provided migration
	// time, protecting the retained request/state from clock skew or a
	// malformed far-future value.
	MigrateMaxScheduleDelay time.Duration
	// IdentityLoadTimeout bounds the optional persisted-window identity load.
	// Continuity restoration is abandoned after this deadline so a slow remote
	// store cannot hold both window enumerators ahead of provider discovery.
	// Values <= 0 use the caller's generator deadline.
	IdentityLoadTimeout time.Duration
	// RuntimeExcludeClientMaxCount bounds Reliability and app-removal
	// exclusions added after construction. Under the default multi-client
	// settings the strict bound is 2,400 ids: about 38 KiB of raw ids, with the
	// complete JSON request pinned below 512 KiB by a wire-format test. Arbitrary
	// custom settings can select a different bound. On overflow the oldest
	// runtime exclusion is evicted, allowing eventual recovery instead of
	// permanently closing discovery.
	// Constructor-supplied exclusions are durable and do not consume this cap.
	// Values <= 0 use the derived default.
	RuntimeExcludeClientMaxCount int
	// PlatformTransportSettingsGenerator customizes window transports. Tests
	// use it to inject userspace sockets; nil or a nil result retains the
	// production defaults. The returned settings are copied before use.
	PlatformTransportSettingsGenerator func() *PlatformTransportSettings
	// PlatformTransportMode forces a carrier for deterministic measurements.
	// The zero value retains automatic production selection.
	PlatformTransportMode TransportMode
	// PlatformTransportModePreferences configures Auto. Nil retains the
	// per-transport production defaults. Lower values are preferred; equal
	// healthy modes remain active in parallel.
	PlatformTransportModePreferences map[TransportMode]int
	// PlatformTransportCreated observes each concrete window transport after
	// construction. Integration measurements use it to force a completed P2P
	// route after promotion; nil has no production effect. The callback must not
	// block the window setup path.
	PlatformTransportCreated func(client *Client, transport *PlatformTransport)
}

type apiWindowPlatformTransport interface {
	ConnectedNotify() <-chan struct{}
	IsConnected() bool
	Close()
}

type apiWindowClientTransport struct {
	current  apiWindowPlatformTransport
	settings *PlatformTransportSettings
	auth     ClientAuth
	// One change counter for this client's extender addresses across every
	// transport generation (K1). Each transport bumps it, and so does a
	// migration swap, so a watcher subscribed to it never has to notice that
	// the transport under it was replaced.
	extenderIpsMonitor *MonitorValue[uint64]
	// Initial setup owns the transport before the provide secret is committed.
	// Live policy migration must not replace that transport until setup returns.
	initializing bool
	// policyVersion identifies the target mode/preferences used to construct
	// current. A concurrent policy change schedules one follow-up replacement.
	policyVersion uint64
	migrating     bool
}

// apiTransportCreationLifecycle closes the Add-versus-Wait race around both
// initial window transports and asynchronous migration replacements.
type apiTransportCreationLifecycle struct {
	mutex             sync.Mutex
	active            int
	closed            bool
	idle              chan struct{}
	beforeWaitForTest func()
}

// Admission and the zero-to-one idle generation are published under one lock
// so teardown cannot miss a creator between its closed check and wait capture.
func (self *apiTransportCreationLifecycle) begin() bool {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	if self.closed {
		return false
	}
	if self.active == 0 {
		self.idle = make(chan struct{})
	}
	self.active += 1
	return true
}

// The final admitted creator closes its generation only after the generated
// transport callback has returned to its caller.
func (self *apiTransportCreationLifecycle) end() {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	self.active -= 1
	if self.active == 0 {
		close(self.idle)
	}
}

// Closing rejects later creators and joins the exact active generation. A
// canceled caller may stop waiting without reopening creation.
func (self *apiTransportCreationLifecycle) closeAndWait(ctx context.Context) error {
	self.mutex.Lock()
	self.closed = true
	if self.active == 0 {
		self.mutex.Unlock()
		return nil
	}
	idle := self.idle
	self.mutex.Unlock()
	if self.beforeWaitForTest != nil {
		self.beforeWaitForTest()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-idle:
		return nil
	}
}

type ApiMultiClientGenerator struct {
	ctx    context.Context
	cancel context.CancelFunc

	specs          []*ProviderSpec
	clientStrategy *ClientStrategy

	// Constructor exclusions are durable for the generator lifetime. Runtime
	// exclusions use a lazy bounded FIFO ring so repeated Reliability churn
	// cannot grow every subsequent discovery request without limit.
	excludeLock                  sync.Mutex
	excludeClientIds             []Id
	runtimeExcludeClientIds      []Id
	runtimeExcludeClientIdSet    map[Id]bool
	runtimeExcludeClientHead     int
	runtimeExcludeClientMaxCount int

	apiUrl      string
	platformUrl string

	deviceDescription       string
	deviceSpec              string
	appVersion              string
	sourceClientId          *Id
	clientSettingsGenerator func() *ClientSettings
	settings                *ApiMultiClientGeneratorSettings
	clientCredentials       NetworkClientCredentials
	clientControl           NetworkClientControl
	providerDiscovery       NetworkProviderDiscovery
	controlTelemetryProbe   bool
	// Window carriers created without an explicit caller budget all belong to
	// this generator. Separate generators never contend through a package root.
	defaultPlatformTransportBudget *PlatformTransportBudget

	transportPolicyLock        sync.RWMutex
	platformTransportMode      TransportMode
	platformModePreferences    map[TransportMode]int
	platformTransportPolicyVer uint64

	api *BringYourApi

	// window identity persistence (PROXYDRAIN1.md §3.5); nil state behavior
	// is identical to no persistence
	identityState *windowIdentityState

	// A window client used to discard its PlatformTransport handle. Retaining
	// one bounded entry per live client lets ResidentMigrate build a
	// replacement before closing the old route. The map is bounded by the
	// quality/speed window hard maxima; each state permits at most one
	// temporary replacement.
	transportLock     sync.Mutex
	transports        map[*Client]*apiWindowClientTransport
	transportIdle     chan struct{}
	transportCreation apiTransportCreationLifecycle
	retirementOnce    sync.Once
	retirements       *lifecycleAdmission
	// Keep only the first finite failure; a long-lived generator must not
	// retain every remote response or a growing error chain.
	retirementStateLock sync.Mutex
	retirementErr       error
	// Injectable lifecycle barriers for deterministic ownership tests.
	beforeRetirementWaitForTest func()
	newPlatformTransport        func(
		client *Client,
		auth *ClientAuth,
		targetMode TransportMode,
		settings *PlatformTransportSettings,
	) apiWindowPlatformTransport
}

func NewApiMultiClientGeneratorWithDefaults(
	ctx context.Context,
	specs []*ProviderSpec,
	clientStrategy *ClientStrategy,
	excludeClientIds []Id,
	apiUrl string,
	byJwt string,
	platformUrl string,
	deviceDescription string,
	deviceSpec string,
	appVersion string,
	sourceClientId *Id,
) *ApiMultiClientGenerator {
	return NewApiMultiClientGenerator(
		ctx,
		specs,
		clientStrategy,
		excludeClientIds,
		apiUrl,
		byJwt,
		platformUrl,
		deviceDescription,
		deviceSpec,
		appVersion,
		sourceClientId,
		DefaultClientSettings,
		DefaultApiMultiClientGeneratorSettings(),
	)
}

func NewApiMultiClientGenerator(
	ctx context.Context,
	specs []*ProviderSpec,
	clientStrategy *ClientStrategy,
	excludeClientIds []Id,
	apiUrl string,
	byJwt string,
	platformUrl string,
	deviceDescription string,
	deviceSpec string,
	appVersion string,
	sourceClientId *Id,
	clientSettingsGenerator func() *ClientSettings,
	settings *ApiMultiClientGeneratorSettings,
) *ApiMultiClientGenerator {
	generatorCtx, generatorCancel := context.WithCancel(ctx)
	api := NewBringYourApi(generatorCtx, clientStrategy, apiUrl)
	api.SetByJwt(byJwt)
	transportIdle := make(chan struct{})
	close(transportIdle)

	platformTransportMode := settings.PlatformTransportMode
	if platformTransportMode == TransportModeNone {
		platformTransportMode = TransportModeAuto
	}
	runtimeExcludeClientMaxCount := settings.RuntimeExcludeClientMaxCount
	if runtimeExcludeClientMaxCount <= 0 {
		runtimeExcludeClientMaxCount = defaultApiRuntimeExcludeClientMaxCount()
	}
	return &ApiMultiClientGenerator{
		ctx:                            generatorCtx,
		cancel:                         generatorCancel,
		specs:                          specs,
		clientStrategy:                 clientStrategy,
		excludeClientIds:               cloneUniqueApiExcludeClientIds(excludeClientIds),
		runtimeExcludeClientMaxCount:   runtimeExcludeClientMaxCount,
		apiUrl:                         apiUrl,
		platformUrl:                    platformUrl,
		deviceDescription:              deviceDescription,
		deviceSpec:                     deviceSpec,
		appVersion:                     appVersion,
		sourceClientId:                 sourceClientId,
		clientSettingsGenerator:        clientSettingsGenerator,
		settings:                       settings,
		clientCredentials:              settings.ClientCredentials,
		clientControl:                  settings.ClientControl,
		providerDiscovery:              settings.ProviderDiscovery,
		controlTelemetryProbe:          settings.ControlTelemetryProbe,
		defaultPlatformTransportBudget: DefaultPlatformTransportBudget(),
		platformTransportMode:          platformTransportMode,
		platformModePreferences:        maps.Clone(settings.PlatformTransportModePreferences),
		platformTransportPolicyVer:     1,
		api:                            api,
		identityState:                  newWindowIdentityState(generatorCtx, nil),
		transports:                     map[*Client]*apiWindowClientTransport{},
		transportIdle:                  transportIdle,
	}
}

// SetByJwt updates the network credential used to mint and retire future
// derived window clients. A generator can outlive the device API's startup
// refresh; retaining its constructor token eventually makes later window
// expansion and cleanup authenticate with an expired credential.
func (self *ApiMultiClientGenerator) SetByJwt(byJwt string) {
	self.api.SetByJwt(byJwt)
}

func normalizePlatformTransportTargetMode(mode TransportMode) TransportMode {
	switch mode {
	case TransportModeH3, TransportModeH1, TransportModeH3Dns, TransportModeH3DnsPump:
		return mode
	default:
		return TransportModeAuto
	}
}

func (self *ApiMultiClientGenerator) platformTransportPolicy() (
	mode TransportMode,
	preferences map[TransportMode]int,
	version uint64,
) {
	self.transportPolicyLock.RLock()
	defer self.transportPolicyLock.RUnlock()
	return normalizePlatformTransportTargetMode(self.platformTransportMode), maps.Clone(self.platformModePreferences), self.platformTransportPolicyVer
}

// SetPlatformTransportPolicy applies one target mode or Auto policy to new and
// live window transports. Existing windows use the same make-before-break path
// as resident migration; a policy change racing an in-flight replacement is
// detected by version and schedules one follow-up replacement.
func (self *ApiMultiClientGenerator) SetPlatformTransportPolicy(
	mode TransportMode,
	preferences map[TransportMode]int,
) {
	mode = normalizePlatformTransportTargetMode(mode)
	if mode == TransportModeAuto {
		preferences = normalizeTransportModePreferences(preferences)
	} else {
		preferences = nil
	}

	self.transportPolicyLock.Lock()
	if self.platformTransportMode == mode && maps.Equal(self.platformModePreferences, preferences) {
		self.transportPolicyLock.Unlock()
		return
	}
	self.platformTransportMode = mode
	self.platformModePreferences = maps.Clone(preferences)
	self.platformTransportPolicyVer += 1
	self.transportPolicyLock.Unlock()

	self.transportLock.Lock()
	clients := make([]*Client, 0, len(self.transports))
	for client, state := range self.transports {
		if state != nil && !state.initializing {
			clients = append(clients, client)
		}
	}
	self.transportLock.Unlock()
	for _, client := range clients {
		self.MigrateClientTransport(client, nil, time.Now())
	}
}

// SetIdentityStore enables window identity persistence (PROXYDRAIN1.md
// §3.5): live (client identity, destination) pairs are mirrored to the
// store, and a restarted process reuses the persisted identities against
// their destinations instead of minting fresh ones — keeping the egress
// providers' NAT flows (keyed by source client id) resumable. Set before
// the multi client starts expanding the window.
func (self *ApiMultiClientGenerator) SetIdentityStore(store MultiClientIdentityStore) {
	self.identityState = newWindowIdentityState(self.ctx, store)
}

// CloseTransportCreationAndWait prevents later window or migration transports
// and joins every creator that entered before this call, including retirement
// of carriers a migration unlinked or discarded. PlatformTransportCreated
// has therefore returned for every generated transport when this returns.
func (self *ApiMultiClientGenerator) CloseTransportCreationAndWait(ctx context.Context) error {
	return self.transportCreation.closeAndWait(ctx)
}

// The retirement gate is lazy so focused tests that construct the generator
// literally retain the same zero-value behavior as production constructors.
func (self *ApiMultiClientGenerator) retirementLifecycle() *lifecycleAdmission {
	self.retirementOnce.Do(func() {
		self.retirements = newLifecycleAdmission()
	})
	return self.retirements
}

// Only fixed stage/class strings cross the final join boundary. HTTP bodies,
// credential errors, identities, and remote URLs never enter the stored error.
func (self *ApiMultiClientGenerator) recordRetirementError(stage string, err error) {
	if err == nil {
		return
	}
	cause := errors.New("request failed")
	if errors.Is(err, context.DeadlineExceeded) {
		cause = context.DeadlineExceeded
	} else if errors.Is(err, context.Canceled) {
		cause = context.Canceled
	}
	self.retirementStateLock.Lock()
	defer self.retirementStateLock.Unlock()
	if self.retirementErr == nil {
		self.retirementErr = fmt.Errorf("client retirement %s: %w", stage, cause)
	}
}

// CloseAndWait prevents new transports, cancels every generated client, waits
// until each channel has handed its client back through RemoveClientWithArgs,
// and joins the resulting Client/OOB retirement workers. A successful return
// therefore makes every generated client's message-pool ownership terminal.
func (self *ApiMultiClientGenerator) CloseAndWait(ctx context.Context) error {
	closeOwned := func() {
		if self.cancel != nil {
			self.cancel()
		}
		if self.api != nil {
			self.api.Close()
		}
	}
	// A canceled waiter must still stop this generator's private context.
	// All clients are canceled below before the potentially bounded joins.
	defer closeOwned()

	if err := self.CloseTransportCreationAndWait(ctx); err != nil {
		return err
	}

	self.transportLock.Lock()
	clients := make([]*Client, 0, len(self.transports))
	transports := make([]apiWindowPlatformTransport, 0, len(self.transports))
	for client, state := range self.transports {
		clients = append(clients, client)
		if state != nil && state.current != nil {
			transports = append(transports, state.current)
		}
	}
	transportIdle := self.transportIdle
	if 0 < len(self.transports) && transportIdle == nil {
		transportIdle = make(chan struct{})
		self.transportIdle = transportIdle
	}
	self.transportLock.Unlock()

	// Stop physical ingress before canceling the client. The channel owner sees
	// Client.Done and synchronously admits its retirement before deleting the
	// matching transport entry, so the idle edge below cannot outrun admission.
	for _, transport := range transports {
		transport.Close()
	}
	for _, client := range clients {
		client.Cancel()
	}
	if 0 < len(clients) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-transportIdle:
		}
	}

	retirements := self.retirementLifecycle()
	retirements.close()
	if self.beforeRetirementWaitForTest != nil {
		self.beforeRetirementWaitForTest()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-retirements.Done():
	}

	// The parent DeviceLocal deliberately outlives destination replacement.
	// Cancel the generator's private context only after live-window retirement
	// has authenticated its final contract cleanup and client removal. This
	// stops the API context and identity writer without making RemoveClientArgs
	// misclassify a destination change as process shutdown.
	closeOwned()
	identityErr := self.identityState.CloseAndWait(ctx)
	self.retirementStateLock.Lock()
	defer self.retirementStateLock.Unlock()
	return errors.Join(identityErr, self.retirementErr)
}

func (self *ApiMultiClientGenerator) NextDestinations(count int, excludeDestinations []MultiHopId, rankMode string) (map[MultiHopId]DestinationStats, error) {
	return self.NextDestinationsContext(self.ctx, count, excludeDestinations, rankMode)
}

// NextDestinationsWithIpFamily implements MultiClientGeneratorWithIpFamily.
func (self *ApiMultiClientGenerator) NextDestinationsWithIpFamily(count int, excludeDestinations []MultiHopId, rankMode string, ipFamily IpFamilyFilter) (map[MultiHopId]DestinationStats, error) {
	return self.nextDestinationsContext(self.ctx, count, excludeDestinations, rankMode, ipFamily)
}

// ExcludeClientIds snapshots durable constructor exclusions followed by
// runtime exclusions in oldest-to-newest order.
func (self *ApiMultiClientGenerator) ExcludeClientIds() []Id {
	self.excludeLock.Lock()
	defer self.excludeLock.Unlock()
	excludeClientIds := slices.Clone(self.excludeClientIds)
	for i := range len(self.runtimeExcludeClientIds) {
		index := (self.runtimeExcludeClientHead + i) % len(self.runtimeExcludeClientIds)
		excludeClientIds = append(excludeClientIds, self.runtimeExcludeClientIds[index])
	}
	return excludeClientIds
}

// ExcludeClientId implements MultiClientGeneratorExcluder. The exclusion lives
// in the runtime FIFO. Duplicate and constructor-excluded ids are no-ops. Once
// the bounded history is full, the oldest runtime id becomes eligible again;
// this limits request growth and lets a long-lived generator recover after the
// provider population changes instead of failing closed forever.
func (self *ApiMultiClientGenerator) ExcludeClientId(clientId Id) {
	self.excludeLock.Lock()
	defer self.excludeLock.Unlock()
	if slices.Contains(self.excludeClientIds, clientId) ||
		self.runtimeExcludeClientIdSet[clientId] {
		return
	}
	maxCount := self.runtimeExcludeClientMaxCount
	if maxCount <= 0 {
		maxCount = defaultApiRuntimeExcludeClientMaxCount()
		self.runtimeExcludeClientMaxCount = maxCount
	}
	if self.runtimeExcludeClientIdSet == nil {
		self.runtimeExcludeClientIdSet = map[Id]bool{}
	}
	if len(self.runtimeExcludeClientIds) < maxCount {
		self.runtimeExcludeClientIds = append(self.runtimeExcludeClientIds, clientId)
	} else {
		oldestClientId := self.runtimeExcludeClientIds[self.runtimeExcludeClientHead]
		delete(self.runtimeExcludeClientIdSet, oldestClientId)
		self.runtimeExcludeClientIds[self.runtimeExcludeClientHead] = clientId
		self.runtimeExcludeClientHead =
			(self.runtimeExcludeClientHead + 1) % len(self.runtimeExcludeClientIds)
	}
	self.runtimeExcludeClientIdSet[clientId] = true
}

// NextDestinationsContext implements MultiClientGeneratorContext. Discovery is
// owned by the caller's maintenance deadline rather than only by the
// generator's process-lifetime context.
func (self *ApiMultiClientGenerator) NextDestinationsContext(ctx context.Context, count int, excludeDestinations []MultiHopId, rankMode string) (map[MultiHopId]DestinationStats, error) {
	return self.nextDestinationsContext(ctx, count, excludeDestinations, rankMode, IpFamilyFilterDefault)
}

// nextDestinationsContext is the discovery body shared by the plain and the
// family-filtered entry points. The filter rides straight into find-providers2;
// the server treats the empty filter as v4-capable.
func (self *ApiMultiClientGenerator) nextDestinationsContext(ctx context.Context, count int, excludeDestinations []MultiHopId, rankMode string, ipFamily IpFamilyFilter) (map[MultiHopId]DestinationStats, error) {
	excludeClientIds := self.ExcludeClientIds()
	excludeDestinationsIds := [][]Id{}
	for _, excludeDestination := range excludeDestinations {
		excludeDestinationsIds = append(excludeDestinationsIds, excludeDestination.Ids())
	}
	destinations := map[MultiHopId]DestinationStats{}

	// A fixed-destination spec (an explicit client id, e.g. a known network peer)
	// is its own destination — there is nothing to discover. Short-circuit
	// find-providers2 for these so a peer connect is a direct send with no platform
	// round trip (and does not hang if the server would not return the peer).
	// Specs that need discovery (location / group / best-available) still go
	// through the api below.
	excludedClientIds := map[Id]bool{}
	for _, id := range excludeClientIds {
		excludedClientIds[id] = true
	}
	discoverySpecs := []*ProviderSpec{}
	for _, spec := range self.specs {
		if spec.ClientId == nil {
			discoverySpecs = append(discoverySpecs, spec)
			continue
		}
		clientId := *spec.ClientId
		if excludedClientIds[clientId] {
			continue
		}
		destination, err := NewMultiHopId(clientId)
		if err != nil {
			continue
		}
		if slices.Contains(excludeDestinations, destination) {
			continue
		}
		destinations[destination] = DestinationStats{}
	}

	// destinations with a restored identity pending reuse are dialed first
	// (PROXYDRAIN1.md §3.5): the restarted window re-forms against the SAME
	// providers so their NAT flows resume
	identityLoadCtx := ctx
	cancelIdentityLoad := func() {}
	if 0 < self.settings.IdentityLoadTimeout {
		identityLoadCtx, cancelIdentityLoad = context.WithTimeout(ctx, self.settings.IdentityLoadTimeout)
	}
	restoredDestinations, err := self.identityState.RestoredDestinationsContext(identityLoadCtx)
	cancelIdentityLoad()
	if err != nil {
		// Persistence is a continuity optimization, never an availability
		// dependency. If its narrower budget expires (or the optional store
		// fails), continue this SAME discovery attempt with fresh identities.
		// Only cancellation of the authoritative generator call stops work.
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		restoredDestinations = nil
	}
	for _, destination := range restoredDestinations {
		if slices.Contains(excludeDestinations, destination) {
			continue
		}
		if _, ok := destinations[destination]; ok {
			continue
		}
		destinations[destination] = DestinationStats{}
	}

	if 0 < len(discoverySpecs) {
		findProviders2 := &FindProviders2Args{
			Specs:               discoverySpecs,
			ExcludeClientIds:    excludeClientIds,
			ExcludeDestinations: excludeDestinationsIds,
			Count:               count,
			RankMode:            rankMode,
			IpFamily:            ipFamily,
		}

		result, err := self.findProviders2(ctx, findProviders2)
		if err != nil {
			// prefer returning any fixed destinations over failing the whole call
			if 0 < len(destinations) {
				return destinations, nil
			}
			return nil, err
		}
		addProviderDestinations(destinations, result.Providers)
	}

	return destinations, nil
}

// Implements MultiClientGeneratorWithClientId: one find-providers2 request that
// names only clientId, so the platform's exclusions decide whether the provider
// may be dialed again. An id this generator excludes is answered empty without
// a request, as discovery would never return it either.
func (self *ApiMultiClientGenerator) NextDestinationsForClientId(clientId Id, excludeDestinations []MultiHopId, rankMode string) (map[MultiHopId]DestinationStats, error) {
	destinations := map[MultiHopId]DestinationStats{}
	excludeClientIds := self.ExcludeClientIds()
	if slices.Contains(excludeClientIds, clientId) {
		return destinations, nil
	}
	excludeDestinationsIds := [][]Id{}
	for _, excludeDestination := range excludeDestinations {
		excludeDestinationsIds = append(excludeDestinationsIds, excludeDestination.Ids())
	}
	result, err := self.findProviders2(self.ctx, &FindProviders2Args{
		Specs: []*ProviderSpec{
			{ClientId: &clientId},
		},
		ExcludeClientIds:    excludeClientIds,
		ExcludeDestinations: excludeDestinationsIds,
		Count:               1,
		RankMode:            rankMode,
	})
	if err != nil {
		return nil, err
	}
	addProviderDestinations(destinations, result.Providers)
	return destinations, nil
}

// Sends one discovery request through the injected provider discovery when
// there is one, else the api.
func (self *ApiMultiClientGenerator) findProviders2(ctx context.Context, findProviders2 *FindProviders2Args) (*FindProviders2Result, error) {
	var result *FindProviders2Result
	var err error
	if self.providerDiscovery != nil {
		discoveryCtx, cancel := context.WithTimeout(ctx, self.clientStrategy.settings.RequestTimeout)
		result, err = self.providerDiscovery.FindProviders2(discoveryCtx, self.api.ByJwt(), findProviders2)
		cancel()
	} else {
		result, err = self.api.FindProviders2SyncWithCtx(ctx, findProviders2)
	}
	if err == nil && result == nil {
		err = errors.New("provider discovery returned no result")
	}
	return result, err
}

// Adds each discovered provider as a destination with its discovery stats.
func addProviderDestinations(destinations map[MultiHopId]DestinationStats, providers []*FindProvidersProvider) {
	for _, provider := range providers {
		ids := []Id{}
		if 0 < len(provider.IntermediaryIds) {
			ids = append(ids, provider.IntermediaryIds...)
		}
		ids = append(ids, provider.ClientId)
		// Keep the destination plus the nearest supported intermediaries.
		if maximumMultiHopIdLength < len(ids) {
			ids = ids[len(ids)-maximumMultiHopIdLength:]
		}
		if destination, err := NewMultiHopId(ids...); err == nil {
			destinations[destination] = DestinationStats{
				EstimatedBytesPerSecond: provider.EstimatedBytesPerSecond,
				Tier:                    provider.Tier,
				NetworkOnly:             provider.NetworkOnly,
				ReputationFailures:      normalizeProviderReputationFailures(provider.ReputationFailedNames),
				Location:                provider.Location,
				IpFamily:                provider.IpFamily.Normalize(),
			}
		}
	}
}

func (self *ApiMultiClientGenerator) NewClientArgs() (*MultiClientGeneratorClientArgs, error) {
	return self.NewClientArgsContext(self.ctx)
}

// NewClientArgsContext implements MultiClientGeneratorContext. Authentication
// must not be able to park the sole candidate producer beyond its maintenance
// budget.
func (self *ApiMultiClientGenerator) NewClientArgsContext(ctx context.Context) (*MultiClientGeneratorClientArgs, error) {
	auth := func() (string, error) {
		// note the derived client id will be inferred by the api jwt
		authNetworkClient := &AuthNetworkClientArgs{
			SourceClientId: self.sourceClientId,
			Description:    self.deviceDescription,
			DeviceSpec:     self.deviceSpec,
		}

		result, err := self.authNetworkClient(ctx, authNetworkClient)
		if err != nil {
			return "", err
		}
		if result == nil {
			return "", errors.New("client auth returned no result")
		}
		if result.Error != nil {
			return "", errors.New(result.Error.Message)
		}
		if result.ByClientJwt == "" {
			return "", errors.New("client auth returned no credential")
		}

		return result.ByClientJwt, nil
	}

	if byJwtStr, err := auth(); err == nil {
		byJwt, err := ParseByJwtUnverified(byJwtStr)
		if err != nil {
			// in this case we cannot clean up the client because we don't know the client id
			panic(err)
		}

		clientAuth := &ClientAuth{
			ByJwt:      byJwtStr,
			InstanceId: NewId(),
			AppVersion: self.appVersion,
		}
		return &MultiClientGeneratorClientArgs{
			ClientId:   byJwt.ClientId,
			ClientAuth: clientAuth,
		}, nil
	} else {
		return nil, err
	}
}

func (self *ApiMultiClientGenerator) authNetworkClient(ctx context.Context, args *AuthNetworkClientArgs) (*AuthNetworkClientResult, error) {
	if self.clientCredentials == nil {
		return self.api.AuthNetworkClientSyncWithCtx(ctx, args)
	}
	// Match the HTTP strategy's finite outer request budget. An ambiguous
	// callback error is final for this attempt, never a second public mint.
	authCtx, cancel := context.WithTimeout(ctx, self.clientStrategy.settings.RequestTimeout)
	defer cancel()
	return self.clientCredentials.AuthNetworkClient(authCtx, args)
}

// NewClientArgsForDestination implements `MultiClientGeneratorWithDestination`
// (PROXYDRAIN1.md §3.5): reuse the restored identity persisted for this
// destination when one exists — same client id, jwt, and instance id, so the
// provider's NAT flows keyed by the client id resume — otherwise mint fresh
// args. Either way the live (identity, destination) pair is recorded to the
// store, so the NEXT restart can restore it.
func (self *ApiMultiClientGenerator) NewClientArgsForDestination(destination MultiHopId) (*MultiClientGeneratorClientArgs, error) {
	return self.NewClientArgsForDestinationContext(self.ctx, destination)
}

// NewClientArgsForDestinationContext implements
// MultiClientGeneratorWithDestinationContext.
func (self *ApiMultiClientGenerator) NewClientArgsForDestinationContext(ctx context.Context, destination MultiHopId) (*MultiClientGeneratorClientArgs, error) {
	identity, err := self.identityState.TakeRestoredContext(ctx, destination)
	if err != nil {
		return nil, err
	}
	if identity != nil {
		self.identityState.Record(identity)
		return &MultiClientGeneratorClientArgs{
			ClientId: identity.ClientId,
			ClientAuth: &ClientAuth{
				ByJwt:      identity.ByJwt,
				InstanceId: identity.InstanceId,
				AppVersion: self.appVersion,
			},
		}, nil
	}

	args, err := self.NewClientArgsContext(ctx)
	if err != nil {
		return nil, err
	}
	self.identityState.Record(&WindowClientIdentity{
		ClientId:    args.ClientId,
		ByJwt:       args.ClientAuth.ByJwt,
		InstanceId:  args.ClientAuth.InstanceId,
		Destination: destination,
	})
	return args, nil
}

func (self *ApiMultiClientGenerator) RemoveClientArgs(args *MultiClientGeneratorClientArgs) {
	// Args that never reached a generated Client still own a platform identity.
	// Admit their asynchronous removal before launch so CloseAndWait cannot
	// cancel the API underneath it. If close already won, retain the historical
	// bounded best effort: no owner can join a producer admitted after close.
	retirements := self.retirementLifecycle()
	retirementAdmitted := retirements.start()
	go HandleError(func() {
		if retirementAdmitted {
			defer retirements.finish()
		}
		retireTimeout := self.clientStrategy.settings.RequestTimeout
		if retireTimeout < 30*time.Second {
			retireTimeout = 30 * time.Second
		}
		removeCtx, removeCancel := context.WithTimeout(context.Background(), retireTimeout)
		defer removeCancel()
		self.recordRetirementError("identity", self.removeClientArgsAndWait(removeCtx, args))
	})
}

// removeClientArgsAndWait is the joined form used by the generated Client's
// retirement worker. RemoveClientArgs is intentionally asynchronous for
// construction failures on the window-maintenance path, but a successful
// CloseAndWait must not cancel the generator API while that final request is
// still in flight. The caller supplies the already-bounded retirement context.
func (self *ApiMultiClientGenerator) removeClientArgsAndWait(
	ctx context.Context,
	args *MultiClientGeneratorClientArgs,
	afterRetirement ...func(context.Context) error,
) error {
	// Preserve restartable identities when the generator's parent is already
	// closing. With no store, still make the same best-effort removal attempt as
	// RemoveClientArgs; a strategy whose own owner has already closed may reject
	// it, and the server-side idle reaper remains the backstop.
	select {
	case <-self.ctx.Done():
		if self.identityState.hasStore() {
			return nil
		}
	default:
		instanceId := Id{}
		if args.ClientAuth != nil {
			instanceId = args.ClientAuth.InstanceId
		}
		if !self.identityState.RemoveIfCurrent(args.ClientId, instanceId) {
			return nil
		}
	}

	var result *RemoveNetworkClientResult
	var err error
	if self.clientCredentials != nil {
		// The existing drain/retirement lifecycle above owns this call and its
		// bounded context. Switching authorities cannot revoke before drain.
		result, err = self.clientCredentials.RemoveNetworkClient(ctx, &RemoveNetworkClientArgs{ClientId: args.ClientId})
	} else {
		result, err = HttpPostWithStrategy(
			ctx,
			self.clientStrategy,
			fmt.Sprintf("%s/network/remove-client", self.apiUrl),
			&RemoveNetworkClientArgs{ClientId: args.ClientId},
			self.api.ByJwt(),
			&RemoveNetworkClientResult{},
			NewNoopApiCallback[*RemoveNetworkClientResult](),
		)
	}
	if err != nil {
		return err
	}
	if result == nil || result.Error != nil {
		return errors.New("client retirement response absent or rejected")
	}
	// Only a successful authority response permits resident cleanup. The
	// identity-state skips above preserve both SQL and Redis ownership.
	for _, after := range afterRetirement {
		if after != nil {
			if err := after(ctx); err != nil {
				return err
			}
		}
	}
	return nil
}

func (self *ApiMultiClientGenerator) RemoveClientWithArgs(client *Client, args *MultiClientGeneratorClientArgs) {
	// The asynchronous owner must not observe caller mutations after capture.
	ownedArgs := *args
	if args.ClientAuth != nil {
		auth := *args.ClientAuth
		ownedArgs.ClientAuth = &auth
	}
	args = &ownedArgs
	retirements := self.retirementLifecycle()
	retirementAdmitted := retirements.start()
	// Capture while the generator still owns its carrier; the immutable token
	// is only committed after every join and a successful SQL retirement.
	residentRetirement, captureDone := self.prepareResidentRetirement(args)
	var transport apiWindowPlatformTransport
	self.transportLock.Lock()
	if state := self.transports[client]; state != nil {
		delete(self.transports, client)
		transport = state.current
		if len(self.transports) == 0 && self.transportIdle != nil {
			close(self.transportIdle)
		}
	}
	self.transportLock.Unlock()
	if transport != nil {
		transport.Close()
	}

	// Keep the derived network-client identity alive until this client's
	// contract-close controls have finished. Removing the identity first makes
	// those controls authenticate with a JWT that the server has already
	// revoked, so every close returns 401 and the server has to reap the open
	// contracts later. Besides leaking cleanup work, that ordering creates a
	// large, window-size-dependent source of database and log noise in the
	// latency simulator.
	//
	// The channel owner cancels the client immediately after this method
	// returns. Retire asynchronously so a slow WebRTC/stream teardown never
	// blocks window replacement. CloseAndWait joins every Client-owned producer
	// of contract-close controls; closing and joining the OOB boundary after
	// that proves all admitted cleanup requests and callbacks are done before
	// RemoveClientArgs revokes the identity. These asynchronous owners remain
	// until actually joined, even when CloseAndWait's caller has timed out.
	// Only then does identity removal start its independent finite budget.
	go HandleError(func() {
		if retirementAdmitted {
			defer retirements.finish()
		}
		// A Redis driver may outlive its context. Even a discarded late capture
		// remains this retirement owner's responsibility until it actually exits.
		if captureDone != nil {
			defer func() { <-captureDone }()
		}
		<-client.Done()
		// The generator, not Client, owns this external carrier. Route removal
		// and Close only cancel it; its socket/receive workers may still retain
		// pooled frames (even after the admission claim ends). Join outside
		// every generator lock before publishing retirement completion. Like provider teardown,
		// this asynchronous owner remains until the carrier is actually done;
		// CloseAndWait's caller context bounds waiting, not resource ownership.
		var drainErr error
		if joiningTransport, ok := transport.(interface {
			CloseAndWait(context.Context) error
		}); ok {
			drainErr = joiningTransport.CloseAndWait(context.Background())
		}
		drainErr = errors.Join(drainErr, client.CloseAndWait(context.Background()))
		if clientOob, ok := client.ClientOob().(interface {
			CloseAndWait(context.Context) error
		}); ok {
			drainErr = errors.Join(drainErr, clientOob.CloseAndWait(context.Background()))
		}
		if drainErr != nil {
			self.recordRetirementError("drain", drainErr)
			return
		}
		retireTimeout := self.clientStrategy.settings.RequestTimeout
		if retireTimeout < 30*time.Second {
			retireTimeout = 30 * time.Second
		}
		retireCtx, retireCancel := context.WithTimeout(context.Background(), retireTimeout)
		defer retireCancel()

		self.recordRetirementError("identity", self.removeClientArgsAndWait(retireCtx, args, residentRetirement))
	})
}

func (self *ApiMultiClientGenerator) NewClientSettings() *ClientSettings {
	return self.clientSettingsGenerator()
}

// ClientReadTimeout reads the same copied settings used to construct this
// client's platform carrier (and reused by migration generations). The
// transport's setting, not a second MultiClient default, owns H1 liveness.
func (self *ApiMultiClientGenerator) ClientReadTimeout(client *Client) (time.Duration, bool) {
	self.transportLock.Lock()
	defer self.transportLock.Unlock()
	state := self.transports[client]
	if state == nil || state.settings == nil {
		return 0, false
	}
	return state.settings.ReadTimeout, true
}

func (self *ApiMultiClientGenerator) NewClient(
	ctx context.Context,
	args *MultiClientGeneratorClientArgs,
	clientSettings *ClientSettings,
) (*Client, error) {
	return self.NewClientContext(ctx, ctx, args, clientSettings)
}

// NewClientContext implements MultiClientGeneratorContext. ctx owns the
// successfully-created client; callCtx only bounds setup. Keeping them
// separate avoids the subtle failure where a setup deadline later cancels an
// otherwise healthy long-lived client.
func (self *ApiMultiClientGenerator) NewClientContext(
	ctx context.Context,
	callCtx context.Context,
	args *MultiClientGeneratorClientArgs,
	clientSettings *ClientSettings,
) (*Client, error) {
	if !self.transportCreation.begin() {
		return nil, errors.New("platform transport creation is closed")
	}
	defer self.transportCreation.end()
	clientOob := self.newClientOob(ctx, args.ClientAuth.ByJwt)
	client := NewClient(ctx, args.ClientId, clientOob, clientSettings)
	settings := self.newPlatformTransportSettings()
	// propagate so the client-level logger covers the platform transport
	settings.Log = client.Log()
	if args.P2pOnly {
		settings.TransportGenerator = func() (sendTransport Transport, receiveTransport Transport) {
			// only use the platform transport for control
			sendTransport = NewSendClientTransport(DestinationId(ControlId))
			receiveTransport = NewReceiveGatewayTransport()
			return
		}
	}
	// the counter is installed on the per-client settings before the first
	// transport is built, so every generation this client ever gets -- the
	// first and every migration replacement, which reuse these settings --
	// publishes its extender addresses through the same monitor (K1)
	extenderIpsMonitor := NewMonitorValue[uint64](0)
	settings.ExtenderIpsMonitor = extenderIpsMonitor
	transport, _, policyVersion := self.createPlatformTransport(client, args.ClientAuth, settings)
	auth := *args.ClientAuth
	self.transportLock.Lock()
	if self.transports == nil {
		self.transports = map[*Client]*apiWindowClientTransport{}
	}
	if len(self.transports) == 0 {
		self.transportIdle = make(chan struct{})
	}
	self.transports[client] = &apiWindowClientTransport{
		current:            transport,
		settings:           settings,
		auth:               auth,
		initializing:       true,
		policyVersion:      policyVersion,
		extenderIpsMonitor: extenderIpsMonitor,
	}
	self.transportLock.Unlock()
	// Enable return traffic for this client and block until the platform has
	// committed the provide secret. The companion (Stream) contract on the return
	// path is verified against this secret, so using the client before it is
	// registered races and fails verification ("Contract verification failed").
	// The oob ack means the secret is committed (an in-band control ack only
	// means the message was delivered, not processed).
	// Network is also enabled so a same-network provider can return traffic
	// under the network relationship (no companion contract), which the
	// provider echoes for network-mode flows. Cross-network providers continue
	// to use the companion (Stream) return path.
	provideAck := make(chan error, 1)
	client.ContractManager().SetProvideModesWithReturnTrafficWithOobAckCallback(
		map[protocol.ProvideMode]bool{
			protocol.ProvideMode_Network: true,
		},
		func(err error) {
			select {
			case provideAck <- err:
			default:
			}
		},
	)
	provideTimeout := clientSettings.ControlPingTimeout
	if provideTimeout <= 0 {
		provideTimeout = 30 * time.Second
	}
	provideTimer := time.NewTimer(provideTimeout)
	defer provideTimer.Stop()
	// A constructed client owns joined retirement even when registration
	// fails. The window must not revoke its args again before that drain.
	failSetup := func(err error) (*Client, error) {
		self.RemoveClientWithArgs(client, args)
		client.Cancel()
		return nil, &multiClientSetupError{err: &localControlRegistrationError{err: err}, argsOwned: true}
	}
	select {
	case err := <-provideAck:
		if err != nil {
			return failSetup(err)
		}
	case <-provideTimer.C:
		return failSetup(fmt.Errorf("provide secret registration timed out"))
	case <-callCtx.Done():
		return failSetup(callCtx.Err())
	case <-ctx.Done():
		return failSetup(ctx.Err())
	}
	// A transport delivery ack does not prove the platform has published the
	// identity key that the provider needs to authenticate this client's proof.
	// Keep setup owned by callCtx until the processed registration completes.
	if clientSettings.ClientKeyRegistrationRequired {
		keyManager := client.ClientKeyManager()
		var registrationErr error
		if keyManager == nil {
			registrationErr = errors.New("client key manager is unavailable")
		} else {
			registrationErr = keyManager.WaitForRegistration(callCtx)
		}
		if registrationErr != nil {
			return failSetup(fmt.Errorf("client key registration: %w", registrationErr))
		}
	}
	self.transportLock.Lock()
	if state := self.transports[client]; state != nil {
		state.initializing = false
	}
	self.transportLock.Unlock()
	_, _, currentPolicyVersion := self.platformTransportPolicy()
	if policyVersion != currentPolicyVersion {
		self.MigrateClientTransport(client, nil, time.Now())
	}
	return client, nil
}

func (self *ApiMultiClientGenerator) newPlatformTransportSettings() *PlatformTransportSettings {
	generatedSettings := DefaultPlatformTransportSettings()
	if self.settings.PlatformTransportSettingsGenerator != nil {
		if candidate := self.settings.PlatformTransportSettingsGenerator(); candidate != nil {
			generatedSettings = candidate
		}
	}
	// The generator may return shared fixture state. Window-specific changes
	// must not mutate it or race another window.
	value := *generatedSettings
	if self.settings.PlatformTransportSettingsGenerator == nil || value.PlatformTransportBudget == nil {
		value.PlatformTransportBudget = self.defaultPlatformTransportBudget
	}
	return &value
}

func (self *ApiMultiClientGenerator) createPlatformTransport(
	client *Client,
	auth *ClientAuth,
	settings *PlatformTransportSettings,
) (apiWindowPlatformTransport, TransportMode, uint64) {
	targetMode, modePreferences, policyVersion := self.platformTransportPolicy()
	settingsValue := *settings
	if modePreferences != nil {
		settingsValue.ModePreferences = maps.Clone(modePreferences)
	}
	effectiveSettings := &settingsValue
	var transport apiWindowPlatformTransport
	if self.newPlatformTransport != nil {
		transport = self.newPlatformTransport(client, auth, targetMode, effectiveSettings)
	} else {
		transport = NewPlatformTransportWithTargetMode(
			client.Ctx(),
			self.clientStrategy,
			client.RouteManager(),
			self.platformUrl,
			auth,
			targetMode,
			effectiveSettings,
		)
	}
	if self.settings.PlatformTransportCreated != nil {
		if platformTransport, ok := transport.(*PlatformTransport); ok {
			self.settings.PlatformTransportCreated(client, platformTransport)
		}
	}
	return transport, targetMode, policyVersion
}

// A migration owns the carrier it unlinks, not just its indexed replacement.
// Close cancels socket workers but does not prove their buffers and callbacks
// have unwound. Keep that retirement in the already-admitted migration worker
// so generator/DeviceLocal joins cannot overlook an old generation. Call only
// outside transport/policy locks. Make-before-break publishes its replacement
// before retiring the old carrier; failed replacements are simply discarded.
// The join caller's deadline never abandons resource ownership.
func closeApiWindowPlatformTransportAndWait(transport apiWindowPlatformTransport) {
	transport.Close()
	if joining, ok := transport.(interface {
		CloseAndWait(context.Context) error
	}); ok {
		_ = joining.CloseAndWait(context.Background())
	}
}

// MigrateClientTransport implements MultiClientGeneratorTransportMigrator.
// The call is deliberately non-blocking: server jitter, connect waiting, and
// handoff happen off the receive path. A duplicate frame while one migration
// is pending is ignored, bounding overlap to one replacement per client.
func (self *ApiMultiClientGenerator) MigrateClientTransport(
	client *Client,
	args *MultiClientGeneratorClientArgs,
	migrateTime time.Time,
) {
	if !self.transportCreation.begin() {
		return
	}
	async := false
	defer func() {
		if !async {
			self.transportCreation.end()
		}
	}()
	self.transportLock.Lock()
	state := self.transports[client]
	if state == nil || state.initializing || state.migrating {
		self.transportLock.Unlock()
		return
	}
	state.migrating = true
	current := state.current
	settings := state.settings
	auth := state.auth
	self.transportLock.Unlock()

	async = true
	go HandleError(func() {
		defer self.transportCreation.end()
		defer func() {
			_, _, currentPolicyVersion := self.platformTransportPolicy()
			stalePolicy := false
			self.transportLock.Lock()
			if self.transports[client] == state {
				state.migrating = false
				stalePolicy = state.policyVersion != currentPolicyVersion
			}
			self.transportLock.Unlock()
			if stalePolicy {
				self.MigrateClientTransport(client, nil, time.Now())
			}
		}()

		maxScheduleDelay := self.settings.MigrateMaxScheduleDelay
		if maxScheduleDelay <= 0 {
			maxScheduleDelay = 5 * time.Minute
		}
		now := time.Now()
		if latest := now.Add(maxScheduleDelay); latest.Before(migrateTime) {
			migrateTime = latest
		}
		if wait := time.Until(migrateTime); 0 < wait {
			timer := time.NewTimer(wait)
			defer timer.Stop()
			select {
			case <-client.Ctx().Done():
				return
			case <-timer.C:
			}
		}

		// Recheck ownership after the scheduled wait. The client might have
		// been removed while its migration was merely pending.
		self.transportLock.Lock()
		stillCurrent := self.transports[client] == state && state.current == current
		self.transportLock.Unlock()
		if !stillCurrent {
			return
		}

		next, _, nextPolicyVersion := self.createPlatformTransport(client, &auth, settings)
		brokeBeforeMake := false
		if nextPlatform, ok := next.(*PlatformTransport); ok {
			if currentPlatform, ok := current.(*PlatformTransport); ok &&
				!nextPlatform.CanMakeBeforeBreakFrom(currentPlatform) {
				// Two full H3 claims can exceed the platform memory cap. Recheck
				// ownership before releasing the old carrier; transitions involving
				// H1 use the bounded handoff and retain make-before-break instead.
				self.transportLock.Lock()
				stillCurrent := self.transports[client] == state && state.current == current
				self.transportLock.Unlock()
				if !stillCurrent {
					closeApiWindowPlatformTransportAndWait(next)
					return
				}
				closeApiWindowPlatformTransportAndWait(current)
				brokeBeforeMake = true
			}
		}
		connectTimeout := self.settings.MigrateConnectTimeout
		if connectTimeout <= 0 {
			connectTimeout = 60 * time.Second
		}
		connectTimer := time.NewTimer(connectTimeout)
		defer connectTimer.Stop()
		for !next.IsConnected() {
			notify := next.ConnectedNotify()
			// Capture notify before the second state check so a connection
			// transition cannot be missed between the two operations.
			if next.IsConnected() {
				break
			}
			select {
			case <-client.Ctx().Done():
				closeApiWindowPlatformTransportAndWait(next)
				return
			case <-notify:
			case <-connectTimer.C:
				if brokeBeforeMake {
					// The old full-H3 working set was released to respect the
					// memory cap. It is no longer a usable fallback, so install the
					// replacement even if its first dial has not connected yet. The
					// PlatformTransport owns its reconnect loop and will continue
					// trying under the requested policy.
					swapped := false
					self.transportLock.Lock()
					if self.transports[client] == state && state.current == current {
						state.current = next
						state.policyVersion = nextPolicyVersion
						swapped = true
					}
					self.transportLock.Unlock()
					if !swapped {
						closeApiWindowPlatformTransportAndWait(next)
						return
					}
					state.noteExtenderIpsChanged()
					return
				}
				// Keep the old transport: it is still a valid route, and the
				// server's drain excuse/reconnect path remains the backstop. Closing
				// the failed replacement also returns any temporary handoff loan.
				// Disarm BEFORE closing the replacement so the close is the
				// definitive "migration released" signal: the deferred disarm
				// runs after this return, so an observer gating on the
				// replacement's close (or a follow-up MigrateClientTransport)
				// would otherwise see migrating still armed in the window
				// between the close and the return. The defer re-clears
				// idempotently.
				func() {
					self.transportLock.Lock()
					defer self.transportLock.Unlock()
					if self.transports[client] == state {
						state.migrating = false
					}
				}()
				closeApiWindowPlatformTransportAndWait(next)
				return
			}
		}

		swapped := false
		self.transportLock.Lock()
		if self.transports[client] == state && state.current == current {
			state.current = next
			state.policyVersion = nextPolicyVersion
			swapped = true
		}
		self.transportLock.Unlock()
		if !swapped {
			closeApiWindowPlatformTransportAndWait(next)
			return
		}
		// the addresses a watcher reads come from the current transport, so
		// the replacement itself is a change even when neither transport moved
		state.noteExtenderIpsChanged()
		// Only now break the old route. For the interval between next becoming
		// connected and this close, RouteManager can carry traffic over both.
		if !brokeBeforeMake {
			closeApiWindowPlatformTransportAndWait(current)
		}
	})
}

// Bumps the per-client change counter, which is how a transport swap reaches
// a watcher subscribed across generations. A state built by a test fixture
// without a counter changes nothing.
func (self *apiWindowClientTransport) noteExtenderIpsChanged() {
	if self.extenderIpsMonitor == nil {
		return
	}
	self.extenderIpsMonitor.Update(func(count uint64) uint64 {
		return count + 1
	})
}

// ClientExtenderIps implements MultiClientGeneratorWithExtenderIps: the
// extenders carrying this client's live platform transport, with the change
// channel armed immediately before the read (K1). A client with no transport
// -- removed, or a fixture that never installed one -- reports no addresses
// and no channel, which parks its watcher until the client itself ends.
func (self *ApiMultiClientGenerator) ClientExtenderIps(client *Client) ([]netip.Addr, <-chan struct{}) {
	var transport apiWindowPlatformTransport
	var extenderIpsMonitor *MonitorValue[uint64]
	func() {
		self.transportLock.Lock()
		defer self.transportLock.Unlock()
		if state := self.transports[client]; state != nil {
			transport = state.current
			extenderIpsMonitor = state.extenderIpsMonitor
		}
	}()
	if extenderIpsMonitor == nil {
		return nil, nil
	}
	// subscribe, then read: a change in between would otherwise close a
	// channel nobody holds and the watcher would sit on a stale set
	_, change := extenderIpsMonitor.Get()
	source, ok := transport.(interface{ ExtenderIps() []netip.Addr })
	if !ok {
		return nil, change
	}
	return source.ExtenderIps(), change
}

func (self *ApiMultiClientGenerator) FixedDestinationSize() (int, bool) {
	specClientIds := []Id{}
	for _, spec := range self.specs {
		if spec.ClientId != nil {
			specClientIds = append(specClientIds, *spec.ClientId)
		}
	}
	// self.log.Infof("[multi]eval fixed %d/%d\n", len(specClientIds), len(self.specs))
	return len(specClientIds), len(specClientIds) == len(self.specs)
}
