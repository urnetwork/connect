package connect

// Client settings for a client whose peers' identity keys are vouched for by an
// operator api.
//
// The platform api publishes each client's long-lived identity key at the
// unauthenticated `/key/<client_id>` route and its signed registration history
// at `/key/<client_id>/history` (`GetClientKeyResult`,
// `GetClientKeyHistoryResult`). A client created against that api checks its
// peers against both routes. This file is the one place that configuration is
// built, so every client an application creates (a device's provider, its
// window clients, an application's own clients) gets the same verification
// instead of a copy that can drift. It uses nothing outside this package and
// the standard library.

import (
	"context"
	"fmt"
)

// Returns a copy of `settings`, or of `DefaultClientSettings()` when nil,
// completed and wired to verify peer identity keys against the operator api at
// `apiUrl`. The caller's settings are never modified.
//
// Copy: the struct is copied, and so are its nested `EncryptionSettings`,
// `SendBufferSettings`, `ReceiveBufferSettings`, `ForwardBufferSettings`,
// `ContractManagerSettings` and `WebRtcSettings`, so a later assignment to the
// result (a per-client budget, a per-client encryption mode) never writes
// through to the caller. Pointers inside those structs (budgets, the key pin
// store, stats) carry over and stay shared until the caller replaces them.
// Every other nested settings pointer is shared with the caller.
//
// Completion: a missing `SendBufferSettings`, `ReceiveBufferSettings` or
// `WebRtcSettings` is filled from the defaults, because memory sizing reads
// them before the client is created. Every supplied value and pointer-sharing
// choice is kept, and every other nested setting is left as supplied. A nil
// `EncryptionSettings` stays nil, and then neither fetcher below is installed.
//
// Verification, each installed only when the caller has not set its own:
//   - `NewPeerClientPublicKeyFetcher` reads `<apiUrl>/key/<peer_id>`. It is the
//     out-of-band cross-check of the key a contract carries, and the identity
//     source of a peer whose contracts carry none. Set a no-op fetcher to
//     disable it.
//   - `NewPeerClientKeyHistoryFetcher` reads `<apiUrl>/key/<peer_id>/history`,
//     the signed registration history that `EncryptionModeRequired` enforces
//     (DESIGNNOTES3). A failed fetch returns its error: the session treats that
//     as an availability failure, never as evidence of substitution.
//
// Both are unauthenticated GETs through `clientStrategy`, made only when a
// session runs the fetcher.
func NewOperatorClientSettings(
	settings *ClientSettings,
	apiUrl string,
	clientStrategy *ClientStrategy,
) *ClientSettings {
	// Shallow-copy settings (and nested EncryptionSettings) so that
	// filling in defaults never mutates the caller's struct.
	var clientSettings ClientSettings
	if settings != nil {
		clientSettings = *settings
	} else {
		clientSettings = *DefaultClientSettings()
	}
	if clientSettings.EncryptionSettings != nil {
		encryptionSettings := *clientSettings.EncryptionSettings
		clientSettings.EncryptionSettings = &encryptionSettings
	}
	// copy the buffer settings structs too, so a caller-specific budget
	// assignment (a provider pair, per-window client stamps) never mutates
	// the caller's structs through the alias. the budget pointers inside
	// carry over, preserving sharing until a caller overwrites them.
	if clientSettings.SendBufferSettings != nil {
		sendBufferSettings := *clientSettings.SendBufferSettings
		clientSettings.SendBufferSettings = &sendBufferSettings
	}
	if clientSettings.ReceiveBufferSettings != nil {
		receiveBufferSettings := *clientSettings.ReceiveBufferSettings
		clientSettings.ReceiveBufferSettings = &receiveBufferSettings
	}
	if clientSettings.ForwardBufferSettings != nil {
		forwardBufferSettings := *clientSettings.ForwardBufferSettings
		clientSettings.ForwardBufferSettings = &forwardBufferSettings
	}
	if clientSettings.ContractManagerSettings != nil {
		contractManagerSettings := *clientSettings.ContractManagerSettings
		clientSettings.ContractManagerSettings = &contractManagerSettings
	}
	if clientSettings.WebRtcSettings != nil {
		webRtcSettings := *clientSettings.WebRtcSettings
		clientSettings.WebRtcSettings = &webRtcSettings
	}
	// A caller may intentionally provide a partial ClientSettings override.
	// Memory sizing dereferences these nested settings before the client is
	// created, so complete only the missing pieces here while preserving
	// every supplied value and pointer-sharing choice.
	defaults := DefaultClientSettings()
	if clientSettings.SendBufferSettings == nil {
		sendBufferSettings := *defaults.SendBufferSettings
		clientSettings.SendBufferSettings = &sendBufferSettings
	}
	if clientSettings.ReceiveBufferSettings == nil {
		receiveBufferSettings := *defaults.ReceiveBufferSettings
		clientSettings.ReceiveBufferSettings = &receiveBufferSettings
	}
	if clientSettings.WebRtcSettings == nil {
		webRtcSettings := *defaults.WebRtcSettings
		clientSettings.WebRtcSettings = &webRtcSettings
	}

	// Install the default out-of-band peer-key cross-check when none
	// is configured. Callers who want to disable the check can set a
	// no-op NewPeerClientPublicKeyFetcher in their settings.
	if clientSettings.EncryptionSettings != nil &&
		clientSettings.EncryptionSettings.NewPeerClientPublicKeyFetcher == nil {
		clientSettings.EncryptionSettings.NewPeerClientPublicKeyFetcher = func(peerId Id) func(context.Context) ([]byte, error) {
			url := fmt.Sprintf("%s/key/%s", apiUrl, peerId)
			return func(fetchCtx context.Context) ([]byte, error) {
				r, err := HttpGetWithStrategy(
					fetchCtx,
					clientStrategy,
					url,
					"",
					&GetClientKeyResult{},
					NewNoopApiCallback[*GetClientKeyResult](),
				)
				if err != nil {
					return nil, err
				}
				return r.PublicKey, nil
			}
		}
	}

	// Install the signed-identity resolver when none is configured. Unlike the
	// cross-check above this one is enforcing under `EncryptionModeRequired`:
	// it withholds the session cipher until the contract-supplied identity key
	// is corroborated against evidence the operator signed, and a verified
	// disagreement is terminal for that peer. See DESIGNNOTES3.
	if clientSettings.EncryptionSettings != nil &&
		clientSettings.EncryptionSettings.NewPeerClientKeyHistoryFetcher == nil {
		clientSettings.EncryptionSettings.NewPeerClientKeyHistoryFetcher = func(peerId Id) func(context.Context) ([][]byte, error) {
			url := fmt.Sprintf("%s/key/%s/history", apiUrl, peerId)
			return func(fetchCtx context.Context) ([][]byte, error) {
				r, err := HttpGetWithStrategy(
					fetchCtx,
					clientStrategy,
					url,
					"",
					&GetClientKeyHistoryResult{},
					NewNoopApiCallback[*GetClientKeyHistoryResult](),
				)
				if err != nil {
					// an availability failure, never evidence of substitution
					return nil, err
				}
				return r.History, nil
			}
		}
	}

	return &clientSettings
}
