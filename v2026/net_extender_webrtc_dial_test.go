//go:build !js

package connect

import (
	"context"
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
)

// Tests of the device's dial side of the webrtc carrier (EXTENDER.md S8,
// net_extender_webrtc_dial.go) and of the dialer the strategy expands for it
// (net_http.go). Pure: the directory and the managers are in process, the
// signaling is the in-memory bridge of net_extender_webrtc_signal_test.go, and
// the answerer is a fake, so no pion session is opened.

// A directory that holds one verified record of a webrtc extender whose
// rendezvous is webRtcClientId, and the record's key.
func newTestWebRtcDialDirectory(t *testing.T, webRtcClientId Id) (*ExtenderDirectory, []byte) {
	t.Helper()
	clock := newTestClock()
	_, directory, rootPrivateKey := newTestExtenderStrategy(t, clock, nil)
	extenderPublicKey := newTestExtenderKey(t)
	record := signTestWebRtcRecord(
		t,
		rootPrivateKey,
		extenderPublicKey,
		clock.Now(),
		clock.Now().Add(14*24*time.Hour),
		webRtcClientId,
		testExtenderAddress("192.0.2.110", ExtenderCarrierTcp, ExtenderCarrierWebRtc),
	)
	if _, err := directory.ApplyRecord(record, ExtenderSourceFeed); err != nil {
		t.Fatal(err)
	}
	return directory, extenderPublicKey
}

// Root cause: a webrtc dial with no client to signal through can only fail,
// and the failure counts against the address's other carriers (S9), so the
// directory must list the carrier exactly while the device has a client.
// Observable: the gate opens when a client is set and closes when it is
// cleared, and the close of a client that was already replaced leaves its
// successor's path open.
func TestWebRtcExtenderDialSignalingOpensTheGateOnlyWithAClient(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	directory, _ := newTestWebRtcDialDirectory(t, NewId())
	bridge := newTestSignalBridge()
	firstManager, firstId := bridge.newManager(t, ctx, nil)
	secondManager, secondId := bridge.newManager(t, ctx, nil)

	signaling := NewWebRtcExtenderDialSignaling(directory)
	if directory.WebRtcCarrierEnabled() || signaling.HasClient() {
		t.Fatalf("the gate is open before any client")
	}
	carriers := func() []string {
		candidates := directory.Candidates(0, 10)
		if len(candidates) != 1 {
			t.Fatalf("candidates = %d, want 1", len(candidates))
		}
		return candidates[0].Carriers
	}
	if !slices.Equal(carriers(), []string{ExtenderCarrierTcp}) {
		t.Fatalf("carriers without a client = %v, want tcp only", carriers())
	}

	signaling.SetClient(firstManager, firstId)
	if !directory.WebRtcCarrierEnabled() || !signaling.HasClient() {
		t.Fatalf("the gate is closed with a client")
	}
	if !slices.Equal(carriers(), []string{ExtenderCarrierTcp, ExtenderCarrierWebRtc}) {
		t.Fatalf("carriers with a client = %v, want tcp and webrtc", carriers())
	}

	// a new client replaces the first; the first's close is stale
	signaling.SetClient(secondManager, secondId)
	signaling.ClearClient(firstManager)
	if !directory.WebRtcCarrierEnabled() || !signaling.HasClient() {
		t.Fatalf("the close of a replaced client closed its successor's gate")
	}

	signaling.ClearClient(secondManager)
	if directory.WebRtcCarrierEnabled() || signaling.HasClient() {
		t.Fatalf("the gate stayed open after the client closed")
	}
	if !slices.Equal(carriers(), []string{ExtenderCarrierTcp}) {
		t.Fatalf("carriers after the close = %v, want tcp only", carriers())
	}

	// setting nil is a clear
	signaling.SetClient(firstManager, firstId)
	signaling.SetClient(nil, Id{})
	if directory.WebRtcCarrierEnabled() {
		t.Fatalf("a nil client left the gate open")
	}
}

// Root cause: the dial profile carries only the identity key, so without a
// resolver from the record's rendezvous id through the device's client the
// carrier never reaches anyone. Observable: a key resolves only while a client
// is set, only when its record carries an id, never to the device's own
// client, and the exchanger it resolves carries the offer to the extender
// named by the record and brings back its answer.
func TestWebRtcExtenderDialSignalingResolvesTheRecordRendezvous(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	bridge := newTestSignalBridge()
	deviceManager, deviceId := bridge.newManager(t, ctx, nil)
	extenderManager, extenderId := bridge.newManager(t, ctx, nil)
	answerer := newTestOfferAnswerer()
	extenderManager.SetExtenderCarrierAnswerer(answerer)

	directory, extenderPublicKey := newTestWebRtcDialDirectory(t, extenderId)
	signaling := NewWebRtcExtenderDialSignaling(directory)

	if _, ok := signaling.Resolve(extenderPublicKey); ok {
		t.Fatalf("resolved with no client to signal through")
	}
	signaling.SetClient(deviceManager, deviceId)
	if _, ok := signaling.Resolve(newTestExtenderKey(t)); ok {
		t.Fatalf("resolved a key the directory has no record of")
	}
	if _, ok := signaling.Resolve(nil); ok {
		t.Fatalf("resolved an empty key")
	}

	exchanger, ok := signaling.Resolve(extenderPublicKey)
	if !ok || exchanger == nil {
		t.Fatalf("the record's key did not resolve")
	}
	answer, err := exchanger.ExchangeOffer(ctx, testOffer)
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if answer.Type != webrtc.SDPTypeAnswer || answer.SDP != answerer.answer.SDP {
		t.Fatalf("answer = %+v, want the extender's", answer)
	}
	if offer := <-answerer.offers; offer.SDP != testOffer.SDP {
		t.Fatalf("the extender saw offer %+v", offer)
	}

	// an extender device holds its own record: its own client is never a
	// destination of its dials
	ownDirectory, ownPublicKey := newTestWebRtcDialDirectory(t, deviceId)
	ownSignaling := NewWebRtcExtenderDialSignaling(ownDirectory)
	ownSignaling.SetClient(deviceManager, deviceId)
	if _, ok := ownSignaling.Resolve(ownPublicKey); ok {
		t.Fatalf("the device resolved its own rendezvous")
	}

	// a record with no rendezvous id resolves nothing
	noIdDirectory, noIdPublicKey := newTestWebRtcDialDirectory(t, Id{})
	noIdSignaling := NewWebRtcExtenderDialSignaling(noIdDirectory)
	noIdSignaling.SetClient(deviceManager, deviceId)
	if _, ok := noIdSignaling.Resolve(noIdPublicKey); ok {
		t.Fatalf("a record without a rendezvous id resolved")
	}

	signaling.ClearClient(deviceManager)
	if _, ok := signaling.Resolve(extenderPublicKey); ok {
		t.Fatalf("resolved after the client closed")
	}
}

// Root cause: the webrtc dialer took the tcp carrier's default branch of
// extenderConfigsForCandidate (S3), so it raced at tcp's priority ahead of
// quic and dns, drew a spoof name for an outer tls it does not have, and took
// tcp's fragment and reorder. Observable: the webrtc config has the tcp
// default port as its authority, no outer name, no tcp shaping, and its
// dialer sorts after dns.
func TestExtenderWebRtcDialerComesAfterDnsWithNoOuterName(t *testing.T) {
	if !(extenderDialerPriority(ExtenderConnectModeDns) < extenderDialerPriority(ExtenderConnectModeWebRtc)) {
		t.Fatalf(
			"webrtc priority %d is not after dns %d",
			extenderDialerPriority(ExtenderConnectModeWebRtc),
			extenderDialerPriority(ExtenderConnectModeDns),
		)
	}
	for _, connectMode := range []ExtenderConnectMode{ExtenderConnectModeTcpTls, ExtenderConnectModeQuic} {
		if !(extenderDialerPriority(connectMode) < extenderDialerPriority(ExtenderConnectModeWebRtc)) {
			t.Fatalf("webrtc priority is not after %s", connectMode)
		}
	}

	candidate := &ExtenderCandidate{
		Ip:        netip.MustParseAddr("192.0.2.111"),
		IpVersion: 4,
		PublicKey: newTestExtenderKey(t),
		Carriers:  []string{ExtenderCarrierTcp, ExtenderCarrierWebRtc},
		// a record's tcp port names the tcp carrier only
		TcpPort:  8443,
		UdpPort:  ExtenderQuicPort,
		DnsPort:  ExtenderDnsPort,
		DnsPorts: []int{ExtenderDnsPort},
		DnsTld:   DefaultExtenderDnsTld,
	}
	spoofDomains := []string{"a.example", "b.example", "c.example"}
	// the tcp shaping is drawn at random, so enough draws that a webrtc config
	// taking it would show it
	for range 64 {
		extenderConfigs := extenderConfigsForCandidate(candidate, "", spoofDomains, "")
		var webRtcConfig *ExtenderConfig
		for _, extenderConfig := range extenderConfigs {
			if extenderConfig.Profile.ConnectMode == ExtenderConnectModeWebRtc {
				webRtcConfig = extenderConfig
			}
		}
		if webRtcConfig == nil {
			t.Fatalf("no webrtc config among %d", len(extenderConfigs))
		}
		profile := webRtcConfig.Profile
		if profile.Port != ExtenderTcpPort {
			t.Fatalf("webrtc port = %d, want the tcp default %d", profile.Port, ExtenderTcpPort)
		}
		if profile.ServerName != "" {
			t.Fatalf("webrtc config drew outer name %q", profile.ServerName)
		}
		if profile.Fragment || profile.Reorder {
			t.Fatalf("webrtc config took tcp shaping: fragment=%t reorder=%t", profile.Fragment, profile.Reorder)
		}
		if string(webRtcConfig.PublicKey) != string(candidate.PublicKey) {
			t.Fatalf("webrtc config does not carry the record key")
		}
		dialer := newExtenderClientDialer(DefaultClientStrategySettings(), webRtcConfig)
		if dialer.priority != extenderDialerPriority(ExtenderConnectModeWebRtc) {
			t.Fatalf("webrtc dialer priority = %d", dialer.priority)
		}
	}
}

// Root cause: a role that closes after its successor installed its own
// answerer cleared it with SetExtenderCarrierAnswerer(nil), and the device
// declined every offer until its next role. Observable: a remove of an
// answerer that was replaced leaves the successor answering; a remove of the
// one installed declines again.
func TestWebRtcManagerRemovesOnlyItsOwnExtenderAnswerer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	bridge := newTestSignalBridge()
	manager, _ := bridge.newManager(t, ctx, nil)
	first := newTestOfferAnswerer()
	second := newTestOfferAnswerer()
	manager.SetExtenderCarrierAnswerer(first)
	manager.SetExtenderCarrierAnswerer(second)
	manager.RemoveExtenderCarrierAnswerer(first)
	if !manager.HasExtenderCarrierAnswerer() {
		t.Fatal("the remove of a replaced answerer cleared its successor")
	}
	manager.RemoveExtenderCarrierAnswerer(nil)
	if !manager.HasExtenderCarrierAnswerer() {
		t.Fatal("a remove of nil cleared the answerer")
	}
	manager.RemoveExtenderCarrierAnswerer(second)
	if manager.HasExtenderCarrierAnswerer() {
		t.Fatal("the remove of the installed answerer left it answering")
	}
}
