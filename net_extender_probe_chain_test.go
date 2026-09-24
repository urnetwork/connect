package connect

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// What a pinger makes of the chain fields of a probe response (GEOMAP §2.9),
// against a hand-made responder that answers every probe with one fixed
// response: the chain end a response names is the probe's target when it is
// not the responder itself, and a probe answered for by another extender is
// never a direct ping, whatever depth the response claims.

// A tls responder on loopback that answers every extender request with the
// response given, and the config that dials it.
func newTestFixedExtenderResponder(t *testing.T, response *protocol.ExtenderResponse) *ExtenderConfig {
	t.Helper()
	frameBytes, err := ExtenderResponseFrame(response)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		io.ReadAll(req.Body)
		w.Header().Set("Content-Type", ExtenderContentType)
		w.Header().Set("Content-Length", strconv.Itoa(len(frameBytes)))
		w.WriteHeader(http.StatusOK)
		w.Write(frameBytes)
	}))
	t.Cleanup(server.Close)
	addrPort := netip.MustParseAddrPort(server.Listener.Addr().(*net.TCPAddr).String())
	return &ExtenderConfig{
		Profile: ExtenderProfile{
			ConnectMode: ExtenderConnectModeTcpTls,
			Port:        int(addrPort.Port()),
		},
		Ip: addrPort.Addr(),
	}
}

// A probe carries the hop count and the chain end key its response names, and
// neither for a direct probe (GEOMAP §2.9).
func TestExtenderLatencyProbeReadsTheChainOfItsResponse(t *testing.T) {
	newPublicKey := func() ed25519.PublicKey {
		publicKey, _, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		return publicKey
	}
	responderPublicKey := newPublicKey()
	endPublicKey := newPublicKey()

	cases := []struct {
		description             string
		response                *protocol.ExtenderResponse
		expectHopCount          uint32
		expectChainEndPublicKey []byte
	}{
		{
			description: "a direct probe",
			response: &protocol.ExtenderResponse{
				PublicKey: responderPublicKey,
			},
			expectHopCount: 0,
		},
		{
			description: "a relayed probe at its depth",
			response: &protocol.ExtenderResponse{
				PublicKey:         responderPublicKey,
				HopCount:          3,
				ChainEndPublicKey: endPublicKey,
			},
			expectHopCount:          3,
			expectChainEndPublicKey: endPublicKey,
		},
		{
			// the depth is copied back unsigned; the chain end is not the
			// extender dialed, so the probe crossed that one at least
			description: "a relayed probe that claims no depth",
			response: &protocol.ExtenderResponse{
				PublicKey:         responderPublicKey,
				ChainEndPublicKey: endPublicKey,
			},
			expectHopCount:          1,
			expectChainEndPublicKey: endPublicKey,
		},
		{
			description: "a responder naming itself the chain end",
			response: &protocol.ExtenderResponse{
				PublicKey:         responderPublicKey,
				ChainEndPublicKey: responderPublicKey,
			},
			expectHopCount: 0,
		},
	}
	for _, c := range cases {
		extenderConfig := newTestFixedExtenderResponder(t, c.response)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		probe, err := ProbeExtenderLatency(ctx, DefaultConnectSettings(), extenderConfig, nil)
		cancel()
		if err != nil {
			t.Fatalf("%s: %v", c.description, err)
		}
		if probe.HopCount != c.expectHopCount {
			t.Errorf("%s: hop count = %d, expected %d", c.description, probe.HopCount, c.expectHopCount)
		}
		if !slices.Equal(probe.ChainEndPublicKey, c.expectChainEndPublicKey) {
			t.Errorf("%s: chain end = %x, expected %x", c.description, probe.ChainEndPublicKey, c.expectChainEndPublicKey)
		}
	}
}

// The report of a probe carries its depth, under the name the operator reads.
func TestExtenderPingReportCarriesTheProbeHopCount(t *testing.T) {
	for _, c := range newTestPingReportCases(t) {
		probe := &ExtenderLatencyProbe{
			Attested:    true,
			Attestation: c.attestation,
			Verdict:     c.verdict,
			Outcome:     c.outcome,
			HopCount:    2,
		}
		report := ExtenderPingReportFromProbe(probe)
		if report == nil || report.HopCount != 2 {
			t.Fatalf("%s: report = %+v, expected hop count 2", c.name, report)
		}
	}
}
