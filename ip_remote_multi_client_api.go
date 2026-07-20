package connect

import (
	"context"
	"errors"
	"fmt"
	"slices"
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
	return &ApiMultiClientGeneratorSettings{}
}

type ApiMultiClientGeneratorSettings struct {
}

type ApiMultiClientGenerator struct {
	ctx context.Context

	specs          []*ProviderSpec
	clientStrategy *ClientStrategy

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
	excludeClientIds := slices.Clone(self.excludeClientIds)
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
	for _, destination := range self.identityState.RestoredDestinations() {
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

		result, err := self.api.FindProviders2Sync(findProviders2)
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
				}
			}
		}
	}

	return destinations, nil
}

func (self *ApiMultiClientGenerator) NewClientArgs() (*MultiClientGeneratorClientArgs, error) {
	auth := func() (string, error) {
		// note the derived client id will be inferred by the api jwt
		authNetworkClient := &AuthNetworkClientArgs{
			SourceClientId: self.sourceClientId,
			Description:    self.deviceDescription,
			DeviceSpec:     self.deviceSpec,
		}

		result, err := self.api.AuthNetworkClientSync(authNetworkClient)
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
	if identity := self.identityState.TakeRestored(destination); identity != nil {
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

	args, err := self.NewClientArgs()
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

	// the identity is being torn down for real (window eviction, expired
	// args): drop it from the persisted snapshot so a restart does not
	// restore a removed client
	self.identityState.Remove(args.ClientId)

	removeNetworkClient := &RemoveNetworkClientArgs{
		ClientId: args.ClientId,
	}

	self.api.RemoveNetworkClient(removeNetworkClient, NewApiCallback(func(result *RemoveNetworkClientResult, err error) {
	}))
}

func (self *ApiMultiClientGenerator) RemoveClientWithArgs(client *Client, args *MultiClientGeneratorClientArgs) {
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
	NewPlatformTransport(
		client.Ctx(),
		self.clientStrategy,
		client.RouteManager(),
		self.platformUrl,
		args.ClientAuth,
		settings,
	)
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
	select {
	case err := <-provideAck:
		if err != nil {
			client.Cancel()
			return nil, err
		}
	case <-time.After(provideTimeout):
		client.Cancel()
		return nil, fmt.Errorf("provide secret registration timed out")
	case <-ctx.Done():
		client.Cancel()
		return nil, ctx.Err()
	}
	return client, nil
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
