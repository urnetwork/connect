package main

import (
	"context"
	"fmt"

	gojwt "github.com/golang-jwt/jwt/v5"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
)

// The standalone extender's peer-to-peer webrtc carrier (EXTENDER.md S,
// S8), --webrtc.
//
// The carrier's offers reach an extender through the exchange: the operator's
// probes signal the client its record names as the rendezvous, which is the
// client the activation was posted under, and a device signals it through its
// own exchange connection. So --webrtc connects a client under --jwt to
// --connect_url and answers on its manager, as the sdk's role answers on its
// provider client's (S6). That client provides to its own network only: it
// is no egress provider, so the operator's probes and the network's own
// devices reach the carrier, and devices of other networks, whose signals
// ride a contract to it, do not.

// The client id a client jwt was issued to.
func extenderJwtClientId(jwt string) (connect.Id, error) {
	claims := gojwt.MapClaims{}
	if _, _, err := gojwt.NewParser().ParseUnverified(jwt, claims); err != nil {
		return connect.Id{}, fmt.Errorf("--webrtc needs a client jwt: %w", err)
	}
	clientIdValue, ok := claims["client_id"].(string)
	if !ok {
		return connect.Id{}, fmt.Errorf("--webrtc needs a client jwt, and this jwt names no client")
	}
	clientId, err := connect.ParseId(clientIdValue)
	if err != nil {
		return connect.Id{}, fmt.Errorf("--webrtc needs a client jwt, and its client id is not readable: %w", err)
	}
	return clientId, nil
}

// Connects the client the carrier's offers arrive on, and returns its manager
// and the close that releases the client and its transports.
func openExtenderWebRtcSignaling(
	ctx context.Context,
	options *extenderOptions,
	clientStrategy *connect.ClientStrategy,
) (*connect.WebRtcManager, func(), error) {
	clientId, err := extenderJwtClientId(options.jwt)
	if err != nil {
		return nil, nil, err
	}
	connectUrl := options.connectUrl
	if connectUrl == "" {
		connectUrl = DefaultConnectUrl
	}

	api := connect.NewBringYourApi(ctx, clientStrategy, options.apiUrl)
	api.SetByJwt(options.jwt)
	client := connect.NewClientWithDefaults(ctx, clientId, connect.NewApiOutOfBandControlWithApi(api))
	client.ContractManager().SetProvideModes(map[protocol.ProvideMode]bool{
		protocol.ProvideMode_Network: true,
	})
	transport := newFamilyPlatformTransportGroup(
		ctx,
		clientStrategy,
		client.RouteManager(),
		connectUrl,
		&connect.ClientAuth{
			ByJwt:      options.jwt,
			InstanceId: connect.NewId(),
			AppVersion: fmt.Sprintf("connectctl %s", ConnectCtlVersion),
		},
	)
	Out.Printf("extender webrtc: answering on client %s through %s", clientId, connectUrl)
	return client.WebRtcManager(), func() {
		transport.Close()
		client.Close()
		api.Close()
	}, nil
}
