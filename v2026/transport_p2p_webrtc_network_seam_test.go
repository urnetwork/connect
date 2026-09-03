//go:build !js

package connect

// This file verifies that the native WebRTC factory uses, but does not own, a
// caller-supplied Pion socket network.

import (
	"strings"
	"testing"
	"time"

	"github.com/pion/logging"
	"github.com/pion/transport/v4/vnet"
	"github.com/pion/webrtc/v4"
)

// Injected candidate enumeration takes precedence over host and loopback
// selection, while peer teardown leaves the shared network usable by its owner.
func TestWebRtcPeerConnectionFactoryUsesCallerOwnedNetwork(t *testing.T) {
	router, err := vnet.NewRouter(&vnet.RouterConfig{
		CIDR:          "10.77.0.0/24",
		MinDelay:      time.Millisecond,
		LoggerFactory: logging.NewDefaultLoggerFactory(),
	})
	if err != nil {
		t.Fatal(err)
	}
	network, err := vnet.NewNet(&vnet.NetConfig{StaticIPs: []string{"10.77.0.2"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := router.AddNet(network); err != nil {
		t.Fatal(err)
	}
	if err := router.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := router.Stop(); err != nil {
			t.Errorf("stop router: %v", err)
		}
	}()

	settings := DefaultWebRtcSettings()
	settings.Log = NewNoopLogger()
	settings.IceServerUrls = nil
	settings.Network = network
	// If the injected network were ignored, this would restrict gathering to
	// the host loopback interfaces instead of the virtual static address.
	settings.UseLoopbackOnlyIceInterfaces = true
	settings.EnableDatagramFastPath = false
	factory, _, err := newWebRtcPeerConnectionFactory(settings, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer factory.Close()
	peerConnection, cancelResolve, err := factory.NewPeerConnection(false)
	if err != nil {
		t.Fatal(err)
	}
	defer cancelResolve()
	if _, err := peerConnection.CreateDataChannel("network-seam", nil); err != nil {
		peerConnection.Close()
		t.Fatal(err)
	}
	gathered := webrtc.GatheringCompletePromise(peerConnection)
	offer, err := peerConnection.CreateOffer(nil)
	if err != nil {
		peerConnection.Close()
		t.Fatal(err)
	}
	if err := peerConnection.SetLocalDescription(offer); err != nil {
		peerConnection.Close()
		t.Fatal(err)
	}
	select {
	case <-gathered:
	case <-time.After(5 * time.Second):
		peerConnection.Close()
		t.Fatal("virtual-network candidate gathering timed out")
	}
	localDescription := peerConnection.LocalDescription()
	if localDescription == nil || !strings.Contains(localDescription.SDP, "10.77.0.2") {
		peerConnection.Close()
		t.Fatalf("local candidates do not contain the injected address: %v", localDescription)
	}
	if err := peerConnection.Close(); err != nil {
		t.Fatal(err)
	}

	packetConn, err := network.ListenPacket("udp4", "0.0.0.0:0")
	if err != nil {
		t.Fatalf("peer teardown closed the caller-owned network: %v", err)
	}
	if err := packetConn.Close(); err != nil {
		t.Fatal(err)
	}
}

// Production defaults do not inject a socket network.
func TestWebRtcSettingsNetworkDefaultsToHostSelection(t *testing.T) {
	if DefaultWebRtcSettings().Network != nil {
		t.Fatal("default WebRTC settings unexpectedly inject a socket network")
	}
}
