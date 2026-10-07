package connect

// net_tls_hello_conformance_test.go -- Layer A of the fingerprint-drift
// conformance harness (fingerprint/README.md). it points connect's own merged
// dialers (normal and resilient) at the shared local endpoint, captures the
// client hello they emit the same way the golden was captured, and diffs the
// discriminating fields against the committed Chrome golden. a drift fails the
// test and names the field -- the gate the owner asked for to prevent the
// chrome tcp hello drifting from real Chrome.
//
// the per-dialer comparisons for the pieces still in flight (the extender
// camouflage carrier hello, the quicv2 Initial, and the IPREAL egress syn/ttl)
// are skipped placeholders at the end, each naming the branch it waits on. they
// are wired, rebased on origin/main, when the lead says that branch has merged;
// they are not stubbed against connect's dialer in the meantime.

import (
	"context"
	"fmt"
	"net"
	"slices"
	"testing"

	"github.com/urnetwork/connect/fingerprint"
)

// fingerprintEndpointSettings is strategy settings whose dials reach the shared
// endpoint whatever name they dial, trusting its private ca -- the Layer-A
// mirror of the harness endpoint real Chrome hits in Layer B.
func fingerprintEndpointSettings(t *testing.T, endpoint *fingerprint.Endpoint) *ClientStrategySettings {
	t.Helper()
	tlsConfig, err := DefaultTlsConfig()
	if err != nil {
		t.Fatal(err)
	}
	tlsConfig.RootCAs = endpoint.CaCertPool()
	settings := DefaultClientStrategySettings()
	settings.TlsConfig = tlsConfig
	addr := endpoint.Addr()
	settings.ConnectSettings.DialContextSettings = &DialContextSettings{
		DialContext: func(ctx context.Context, network string, address string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp4", addr)
		},
	}
	return settings
}

// fingerprintEndpointAuthority is the endpoint reached under its certified
// documentation name, which every dial resolves to the endpoint's address.
func fingerprintEndpointAuthority(endpoint *fingerprint.Endpoint) string {
	return fmt.Sprintf("%s:%d", fingerprint.ServerName, endpoint.Port())
}

// diffOptionsForPath is the dial-path shape the hello of a path offering
// nextProtos should present: its alpn, alps only beside an offered h2, no
// first-contact ticket, and -- for a dialer that does not reshape records --
// the single record Chrome sends.
func diffOptionsForPath(nextProtos []string, expectSingleRecord bool) fingerprint.DiffOptions {
	opts := fingerprint.DiffOptions{
		ExpectedServerName:        fingerprint.ServerName,
		ExpectedAlpnProtocols:     nextProtos,
		ExpectApplicationSettings: slices.Contains(nextProtos, "h2"),
		ExpectPreSharedKey:        false,
	}
	if expectSingleRecord {
		opts.ExpectRecordCount = 1
	}
	return opts
}

// captureDialerHellos runs one api request (h2) and one websocket dial
// (http/1.1) of dialer against endpoint and returns the two client hellos the
// endpoint captured, parsed, in that order.
func captureDialerHellos(t *testing.T, endpoint *fingerprint.Endpoint, dialer *clientDialer) (api *fingerprint.ClientHelloFingerprint, webSocket *fingerprint.ClientHelloFingerprint) {
	t.Helper()
	authority := fingerprintEndpointAuthority(endpoint)
	client := dialer.HttpClient()
	testTlsHelloApiRequest(t, client, authority)
	client.CloseIdleConnections()
	testTlsHelloWebSocket(t, dialer, authority)

	captures := endpoint.CapturedClientHellos()
	if len(captures) != 2 {
		t.Fatalf("captured %d client hellos, want 2 (api then websocket)", len(captures))
	}
	parse := func(capture fingerprint.CapturedClientHello) *fingerprint.ClientHelloFingerprint {
		fingerprintValue, err := fingerprint.ParseClientHello(capture.Message)
		if err != nil {
			t.Fatal(err)
		}
		fingerprintValue.RecordCount = capture.RecordCount
		return fingerprintValue
	}
	return parse(captures[0]), parse(captures[1])
}

// Every merged dialer -- the normal one and the three resilient ones -- emits
// the committed Chrome golden's fingerprint to the shared endpoint: the api
// path on go 1.27, where net/http reads the uTLS connection state, and the
// websocket path on every toolchain. the fragmenting dialers reshape records on
// purpose, so their record count is not asserted; the reassembled hello still
// matches, which is what a reassembling filter sees.
func TestConnectDialersPresentChromeGoldenFingerprint(t *testing.T) {
	golden, err := fingerprint.LoadGolden(fingerprint.GoldenChrome133Synthetic)
	if err != nil {
		t.Fatal(err)
	}
	for _, testDialer := range testTlsHelloDialers {
		endpoint, err := fingerprint.NewEndpoint(fingerprint.EndpointOptions{Handler: testTlsHelloHandler()})
		if err != nil {
			t.Fatal(err)
		}
		dialer := testDialer.clientDialer(fingerprintEndpointSettings(t, endpoint))
		apiHello, webSocketHello := captureDialerHellos(t, endpoint, dialer)
		endpoint.Close()

		expectSingleRecord := !testDialer.fragment
		// the websocket path presents the Chrome hello on any toolchain.
		if drifts := fingerprint.Diff(golden.Fingerprint, webSocketHello, diffOptionsForPath(clientWebSocketNextProtos, expectSingleRecord)); len(drifts) != 0 {
			t.Errorf("%s websocket path: %s", testDialer.description, fingerprint.FormatDrift(fingerprint.GoldenChrome133Synthetic, drifts))
		}
		// the api path offers h2, which presents the Chrome hello only where
		// net/http reads the negotiated protocol off the uTLS connection
		// (net_tls_hello_go127.go); before go 1.27 it keeps Go's hello there.
		if httpTransportReadsTlsConnectionState {
			if drifts := fingerprint.Diff(golden.Fingerprint, apiHello, diffOptionsForPath(clientHttpNextProtos, expectSingleRecord)); len(drifts) != 0 {
				t.Errorf("%s api path: %s", testDialer.description, fingerprint.FormatDrift(fingerprint.GoldenChrome133Synthetic, drifts))
			}
		}
	}
}

// The drift gate catches a connect that regresses off the Chrome parrot: with
// the shipped kill switch (TlsClientHelloFingerprintGo, a faithful revert of
// the Chrome-hello behavior) the dialer emits Go's hello, and the diff against
// the Chrome golden reports drift -- grease, the cipher list and the extension
// set among the fields. this is the fail-before to the test above's pass-after.
func TestConformanceGateCatchesGoHelloRevert(t *testing.T) {
	golden, err := fingerprint.LoadGolden(fingerprint.GoldenChrome133Synthetic)
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err := fingerprint.NewEndpoint(fingerprint.EndpointOptions{Handler: testTlsHelloHandler()})
	if err != nil {
		t.Fatal(err)
	}
	defer endpoint.Close()
	settings := fingerprintEndpointSettings(t, endpoint)
	settings.TlsClientHelloFingerprint = TlsClientHelloFingerprintGo
	dialer := testTlsHelloDialers[0].clientDialer(settings)

	_, webSocketHello := captureDialerHellos(t, endpoint, dialer)
	drifts := fingerprint.Diff(golden.Fingerprint, webSocketHello, diffOptionsForPath(clientWebSocketNextProtos, true))
	if len(drifts) == 0 {
		t.Fatal("Go's hello did not drift from the Chrome golden, so the gate would not catch a parrot regression")
	}
	// the regression must be visible in the parrot fields, not only the record
	// shape: Go's hello carries no grease.
	fields := make([]string, len(drifts))
	for i, drift := range drifts {
		fields[i] = drift.Field
	}
	if !slices.Contains(fields, "cipher_suites") || !slices.Contains(fields, "extension_grease_structure") {
		t.Errorf("Go's hello drifted in %v, want the cipher list and the grease structure named", fields)
	}
}

// The drift gate catches a stale committed golden: connect's correct Chrome
// hello drifts from a golden whose post-quantum key share was removed, naming
// the pq field. so a golden that fell behind a real-Chrome refresh fails loudly
// rather than passing a now-wrong comparison.
func TestConformanceGateCatchesStaleGolden(t *testing.T) {
	golden, err := fingerprint.LoadGolden(fingerprint.GoldenChrome133Synthetic)
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err := fingerprint.NewEndpoint(fingerprint.EndpointOptions{Handler: testTlsHelloHandler()})
	if err != nil {
		t.Fatal(err)
	}
	defer endpoint.Close()
	dialer := testTlsHelloDialers[0].clientDialer(fingerprintEndpointSettings(t, endpoint))
	_, webSocketHello := captureDialerHellos(t, endpoint, dialer)

	stale := *golden.Fingerprint
	stale.SupportedGroups = slices.DeleteFunc(slices.Clone(golden.Fingerprint.SupportedGroups), func(group uint16) bool {
		return group == uint16(0x11ec) // X25519MLKEM768
	})
	stale.KeyShareGroups = slices.DeleteFunc(slices.Clone(golden.Fingerprint.KeyShareGroups), func(group uint16) bool {
		return group == uint16(0x11ec)
	})

	if drifts := fingerprint.Diff(&stale, webSocketHello, diffOptionsForPath(clientWebSocketNextProtos, true)); len(drifts) == 0 {
		t.Fatal("connect's hello did not drift from a stale golden missing the pq key share")
	}
	if drifts := fingerprint.Diff(golden.Fingerprint, webSocketHello, diffOptionsForPath(clientWebSocketNextProtos, true)); len(drifts) != 0 {
		t.Fatalf("connect's hello drifted from the correct golden: %s", fingerprint.FormatDrift(fingerprint.GoldenChrome133Synthetic, drifts))
	}
}

// The normal dialer delivers its hello in a single tls record, as Chrome does;
// a filter that keys on the record-layer shape sees one record, not connect's.
// the fragmenting dialers deliberately differ, which the test above does not
// assert for them; this pins the normal dialer's shape explicitly.
func TestNormalDialerSendsSingleRecordHello(t *testing.T) {
	endpoint, err := fingerprint.NewEndpoint(fingerprint.EndpointOptions{Handler: testTlsHelloHandler()})
	if err != nil {
		t.Fatal(err)
	}
	defer endpoint.Close()
	dialer := testTlsHelloDialers[0].clientDialer(fingerprintEndpointSettings(t, endpoint))
	_, webSocketHello := captureDialerHellos(t, endpoint, dialer)
	if webSocketHello.RecordCount != 1 {
		t.Fatalf("the normal dialer sent the hello in %d records, want 1", webSocketHello.RecordCount)
	}
}

// ---------------------------------------------------------------------------
// Placeholders for the per-dialer comparisons whose dialer branch is still in
// flight. each is a real test that skips naming the branch it waits on, so the
// harness lists the whole intended drift surface and the lead wires each one
// (rebased on origin/main) as its branch merges -- it is not stubbed against
// connect's dialer in the meantime (task + RULES.md coordination).
// ---------------------------------------------------------------------------

// The extender camouflage carrier hello: the same parrot assertions on the
// carrier's uTLS hello, plus that the sealed session occupies the 32-byte
// legacy session-id field and the rest matches Chrome. waits on the extender
// camouflage implementation (its design merged as 0a349da5; the impl that emits
// the authenticated REALITY hello is not yet on origin/main).
func TestExtenderCamouflageHelloConformance(t *testing.T) {
	t.Skip("waits on the extender camouflage carrier-hello implementation branch; Layer A wired when it is on origin/main (fingerprint/README.md)")
}

// The quic Initial comparison is WIRED, not a placeholder: feat/quicv2-extender-udp
// has merged, so net_quic_initial_conformance_test.go drives connect's merged
// QuicVersionPolicy to the shared QUIC endpoint and gates the first Initial's
// long-header version. the transport parameters and frame layout remain a
// documented next increment there (they need Initial decryption).

// The egress syn / ip ttl (JA4T): that the egress SYN's OS profile is
// consistent with the client ClientHello's OS. waits on connect/IPREAL.md's
// egress implementation; ground truth is the OS kernel, so this is a Layer-B
// check needing root, not a hermetic go test (fingerprint/README.md).
func TestEgressSynTtlConformance(t *testing.T) {
	t.Skip("waits on the IPREAL egress implementation; syn/ttl is a Layer-B, root-only kernel capture, not a hermetic go test (fingerprint/README.md)")
}
