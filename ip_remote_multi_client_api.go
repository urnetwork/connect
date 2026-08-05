package connect

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	// "maps"

	// "google.golang.org/protobuf/proto"

	// "github.com/urnetwork/glog"

	"github.com/urnetwork/connect/protocol"
)

type MultiClientGeneratorClientArgs struct {
	ClientId   Id
	ClientAuth *ClientAuth
	P2pOnly    bool
}

func DefaultApiMultiClientGeneratorSettings() *ApiMultiClientGeneratorSettings {
	return &ApiMultiClientGeneratorSettings{
		MigrateConnectTimeout:   60 * time.Second,
		MigrateMaxScheduleDelay: 5 * time.Minute,
		IdentityLoadTimeout:     5 * time.Second,
	}
}

type ApiMultiClientGeneratorSettings struct {
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
}

type apiWindowPlatformTransport interface {
	ConnectedNotify() <-chan struct{}
	IsConnected() bool
	Close()
}

type apiWindowClientTransport struct {
	current   apiWindowPlatformTransport
	settings  *PlatformTransportSettings
	auth      ClientAuth
	migrating bool
}

type ApiMultiClientGenerator struct {
	ctx context.Context

	specs          []*ProviderSpec
	clientStrategy *ClientStrategy

	// guarded by excludeLock; grows when the app removes a provider
	excludeLock      sync.Mutex
	excludeClientIds []Id

	apiUrl      string
	byJwt       string
	platformUrl string

	deviceDescription       string
	deviceSpec              string
	appVersion              string
	sourceClientId          *Id
	clientSettingsGenerator func() *ClientSettings
	settings                *ApiMultiClientGeneratorSettings

	api *BringYourApi

	// window identity persistence (PROXYDRAIN1.md §3.5); nil state behavior
	// is identical to no persistence
	identityState *windowIdentityState

	// A window client used to discard its PlatformTransport handle. Retaining
	// one bounded entry per live client lets ResidentMigrate build a
	// replacement before closing the old route. The map is bounded by the
	// quality/speed window hard maxima; each state permits at most one
	// temporary replacement.
	transportLock sync.Mutex
	transports    map[*Client]*apiWindowClientTransport
	// injectable for deterministic make-before-break tests
	newPlatformTransport func(
		client *Client,
		auth *ClientAuth,
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
	api := NewBringYourApi(ctx, clientStrategy, apiUrl)
	api.SetByJwt(byJwt)

	return &ApiMultiClientGenerator{
		ctx:                     ctx,
		specs:                   specs,
		clientStrategy:          clientStrategy,
		excludeClientIds:        excludeClientIds,
		apiUrl:                  apiUrl,
		byJwt:                   byJwt,
		platformUrl:             platformUrl,
		deviceDescription:       deviceDescription,
		deviceSpec:              deviceSpec,
		appVersion:              appVersion,
		sourceClientId:          sourceClientId,
		clientSettingsGenerator: clientSettingsGenerator,
		settings:                settings,
		api:                     api,
		identityState:           newWindowIdentityState(ctx, nil),
		transports:              map[*Client]*apiWindowClientTransport{},
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

func (self *ApiMultiClientGenerator) NextDestinations(count int, excludeDestinations []MultiHopId, rankMode string) (map[MultiHopId]DestinationStats, error) {
	return self.NextDestinationsContext(self.ctx, count, excludeDestinations, rankMode)
}

// ExcludeClientIds is the current exclusion set: the client ids never returned
// by discovery. Read on the enumerator goroutine, mutated by the app thread
// (see ExcludeClientId), so it is snapshot under the lock.
func (self *ApiMultiClientGenerator) ExcludeClientIds() []Id {
	self.excludeLock.Lock()
	defer self.excludeLock.Unlock()
	return slices.Clone(self.excludeClientIds)
}

// ExcludeClientId implements MultiClientGeneratorExcluder. The exclusion lives
// as long as this generator: a destination change builds a new generator, so
// reconnecting gives every provider a clean slate.
func (self *ApiMultiClientGenerator) ExcludeClientId(clientId Id) {
	self.excludeLock.Lock()
	defer self.excludeLock.Unlock()
	if !slices.Contains(self.excludeClientIds, clientId) {
		self.excludeClientIds = append(self.excludeClientIds, clientId)
	}
}

// NextDestinationsContext implements MultiClientGeneratorContext. Discovery is
// owned by the caller's maintenance deadline rather than only by the
// generator's process-lifetime context.
func (self *ApiMultiClientGenerator) NextDestinationsContext(ctx context.Context, count int, excludeDestinations []MultiHopId, rankMode string) (map[MultiHopId]DestinationStats, error) {
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
		}

		result, err := self.api.FindProviders2SyncWithCtx(ctx, findProviders2)
		if err != nil {
			// prefer returning any fixed destinations over failing the whole call
			if 0 < len(destinations) {
				return destinations, nil
			}
			return nil, err
		}

		for _, provider := range result.Providers {
			ids := []Id{}
			if 0 < len(provider.IntermediaryIds) {
				ids = append(ids, provider.IntermediaryIds...)
			}
			ids = append(ids, provider.ClientId)
			// use the tail if the length exceeds the allowed maximum
			if MaxMultihopLength < len(ids) {
				ids = ids[len(ids)-MaxMultihopLength:]
			}
			if destination, err := NewMultiHopId(ids...); err == nil {
				destinations[destination] = DestinationStats{
					EstimatedBytesPerSecond: provider.EstimatedBytesPerSecond,
					Tier:                    provider.Tier,
					Location:                provider.Location,
				}
			}
		}
	}

	return destinations, nil
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

		result, err := self.api.AuthNetworkClientSyncWithCtx(ctx, authNetworkClient)
		if err != nil {
			return "", err
		}

		if result.Error != nil {
			return "", errors.New(result.Error.Message)
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
	// Distinguish a window eviction from a shutdown-caused teardown: every
	// channel teardown calls remove, but when the generator's ctx is done
	// the whole device/process is going away. What happens next depends on
	// whether an identity store is configured:
	// - store configured (the proxy case): the identities must SURVIVE —
	//   both in the persisted snapshot and as live network clients — so a
	//   replacement container can reuse them (PROXYDRAIN1.md §3.5). Skip
	//   everything.
	// - no store (plain sdk apps, the default): nothing will ever reuse
	//   these window clients, so keep the historical best-effort delete —
	//   otherwise the platform-client rows leak on every app shutdown and
	//   linger until server-side idle reap.
	// Window evictions happen while the ctx is live and remove for real.
	select {
	case <-self.ctx.Done():
		if self.identityState.hasStore() {
			return
		}
		// one shot on a Background context (the lifecycle ctx is closed, so
		// posting on it can never leave the process), mirroring the contract
		// manager's after-close cleanup; the server's idle client reap
		// remains the backstop if the attempt fails
		go HandleError(func() {
			removeCtx, removeCancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer removeCancel()
			HttpPostWithStrategy(
				removeCtx,
				self.clientStrategy,
				fmt.Sprintf("%s/network/remove-client", self.apiUrl),
				&RemoveNetworkClientArgs{
					ClientId: args.ClientId,
				},
				self.byJwt,
				&RemoveNetworkClientResult{},
				NewNoopApiCallback[*RemoveNetworkClientResult](),
			)
		})
		return
	default:
	}

	// The identity is being torn down for real (window eviction, expired
	// args), unless a newer channel has already replaced it under the same
	// client id. InstanceId is the generation token: stale asynchronous
	// cleanup must neither erase the replacement from the persisted snapshot
	// nor send remove-client for the replacement's still-live server row.
	instanceId := Id{}
	if args.ClientAuth != nil {
		instanceId = args.ClientAuth.InstanceId
	}
	if !self.identityState.RemoveIfCurrent(args.ClientId, instanceId) {
		return
	}

	removeNetworkClient := &RemoveNetworkClientArgs{
		ClientId: args.ClientId,
	}

	self.api.RemoveNetworkClient(removeNetworkClient, NewApiCallback(func(result *RemoveNetworkClientResult, err error) {
	}))
}

func (self *ApiMultiClientGenerator) RemoveClientWithArgs(client *Client, args *MultiClientGeneratorClientArgs) {
	var transport apiWindowPlatformTransport
	self.transportLock.Lock()
	if state := self.transports[client]; state != nil {
		delete(self.transports, client)
		transport = state.current
	}
	self.transportLock.Unlock()
	if transport != nil {
		transport.Close()
	}
	self.RemoveClientArgs(args)
}

func (self *ApiMultiClientGenerator) NewClientSettings() *ClientSettings {
	return self.clientSettingsGenerator()
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
	clientOob := NewApiOutOfBandControl(ctx, self.clientStrategy, args.ClientAuth.ByJwt, self.apiUrl)
	client := NewClient(ctx, args.ClientId, clientOob, clientSettings)
	settings := DefaultPlatformTransportSettings()
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
	transport := self.createPlatformTransport(client, args.ClientAuth, settings)
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
	select {
	case err := <-provideAck:
		if err != nil {
			transport.Close()
			client.Cancel()
			return nil, err
		}
	case <-provideTimer.C:
		transport.Close()
		client.Cancel()
		return nil, fmt.Errorf("provide secret registration timed out")
	case <-callCtx.Done():
		transport.Close()
		client.Cancel()
		return nil, callCtx.Err()
	case <-ctx.Done():
		transport.Close()
		client.Cancel()
		return nil, ctx.Err()
	}
	auth := *args.ClientAuth
	self.transportLock.Lock()
	if self.transports == nil {
		self.transports = map[*Client]*apiWindowClientTransport{}
	}
	self.transports[client] = &apiWindowClientTransport{
		current:  transport,
		settings: settings,
		auth:     auth,
	}
	self.transportLock.Unlock()
	return client, nil
}

func (self *ApiMultiClientGenerator) createPlatformTransport(
	client *Client,
	auth *ClientAuth,
	settings *PlatformTransportSettings,
) apiWindowPlatformTransport {
	if self.newPlatformTransport != nil {
		return self.newPlatformTransport(client, auth, settings)
	}
	return NewPlatformTransport(
		client.Ctx(),
		self.clientStrategy,
		client.RouteManager(),
		self.platformUrl,
		auth,
		settings,
	)
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
	self.transportLock.Lock()
	state := self.transports[client]
	if state == nil || state.migrating {
		self.transportLock.Unlock()
		return
	}
	state.migrating = true
	current := state.current
	settings := state.settings
	auth := state.auth
	self.transportLock.Unlock()

	go HandleError(func() {
		defer func() {
			self.transportLock.Lock()
			if self.transports[client] == state {
				state.migrating = false
			}
			self.transportLock.Unlock()
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

		next := self.createPlatformTransport(client, &auth, settings)
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
				next.Close()
				return
			case <-notify:
			case <-connectTimer.C:
				// Keep the old transport: it is still a valid route, and the
				// server's drain excuse/reconnect path remains the backstop.
				next.Close()
				return
			}
		}

		swapped := false
		self.transportLock.Lock()
		if self.transports[client] == state && state.current == current {
			state.current = next
			swapped = true
		}
		self.transportLock.Unlock()
		if !swapped {
			next.Close()
			return
		}
		// Only now break the old route. For the interval between next becoming
		// connected and this close, RouteManager can carry traffic over both.
		current.Close()
	})
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
