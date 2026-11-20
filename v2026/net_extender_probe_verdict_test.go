package connect

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strconv"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect/v2026/protocol"
)

// The pinger's side of the verdict (GEOMAP §2.3) against targets the extender
// in this module never is: one that predates the verdict, one that answers
// garbage, one that accepts with a signature nobody can check. The fake speaks
// the probe exactly up to the attestation over the tcp carrier and then
// answers as the test says.

// A fake target: its tls server, its identity, what it answers once it has
// read the attestation, and what it received.
type testVerdictTarget struct {
	server     *httptest.Server
	publicKey  ed25519.PublicKey
	privateKey ed25519.PrivateKey
	// what the target does with the stream once it has read the attestation
	answer func(conn net.Conn, attestation *protocol.ExtenderProbeAttestation)
	// the headers and attestations it received
	headers      chan *protocol.ExtenderHeader
	attestations chan *protocol.ExtenderProbeAttestation
	// publishes no identity key when set
	anonymous bool
}

// A running fake target whose answer is the test's, closed with the test.
func newTestVerdictTarget(
	t *testing.T,
	answer func(target *testVerdictTarget, conn net.Conn, attestation *protocol.ExtenderProbeAttestation),
) *testVerdictTarget {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	target := &testVerdictTarget{
		publicKey:    publicKey,
		privateKey:   privateKey,
		headers:      make(chan *protocol.ExtenderHeader, 16),
		attestations: make(chan *protocol.ExtenderProbeAttestation, 16),
	}
	target.answer = func(conn net.Conn, attestation *protocol.ExtenderProbeAttestation) {
		answer(target, conn, attestation)
	}
	target.server = httptest.NewUnstartedServer(http.HandlerFunc(target.handle))
	target.server.TLS = &tls.Config{MinVersion: tls.VersionTLS13}
	target.server.StartTLS()
	t.Cleanup(target.server.Close)
	return target
}

// Serves one probe up to the attestation, then hands the stream to the answer.
func (self *testVerdictTarget) handle(w http.ResponseWriter, r *http.Request) {
	headerBytes, err := io.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	header := &protocol.ExtenderHeader{}
	if err := proto.Unmarshal(headerBytes, header); err != nil {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	self.headers <- header
	response := &protocol.ExtenderResponse{
		Carriers: []string{ExtenderCarrierTcp},
	}
	if !self.anonymous {
		response.PublicKey = slices.Clone(self.publicKey)
	}
	if 0 < len(header.ProbeClientId) || 0 < len(header.ProbeExtenderPublicKey) {
		nonce, err := NewExtenderProbeNonce()
		if err != nil {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		response.ProbeNonce = nonce
	}
	frameBytes, err := ExtenderResponseFrame(response)
	if err != nil {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", ExtenderContentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(frameBytes)))
	w.WriteHeader(http.StatusOK)
	w.Write(frameBytes)
	w.(http.Flusher).Flush()
	conn, bufrw, err := w.(http.Hijacker).Hijack()
	if err != nil {
		return
	}
	defer conn.Close()
	if response.ProbeNonce == nil {
		return
	}
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	attestation, err := ReadExtenderProbeAttestationFrame(bufrw.Reader)
	if err != nil {
		return
	}
	self.attestations <- attestation
	self.answer(conn, attestation)
}

// The config that dials the target over its tcp carrier.
func (self *testVerdictTarget) config() *ExtenderConfig {
	addrPort := netip.MustParseAddrPort(self.server.Listener.Addr().String())
	return &ExtenderConfig{
		Profile: ExtenderProfile{
			ConnectMode: ExtenderConnectModeTcpTls,
			Port:        int(addrPort.Port()),
		},
		Ip: addrPort.Addr(),
	}
}

// Connect settings that fail a dial fast and give the probe room.
func testVerdictConnectSettings() *ConnectSettings {
	connectSettings := DefaultConnectSettings()
	connectSettings.ConnectTimeout = 500 * time.Millisecond
	connectSettings.TlsTimeout = 5 * time.Second
	connectSettings.RequestTimeout = 10 * time.Second
	return connectSettings
}

// Writes one verdict frame on the stream, as a target does.
func writeTestVerdict(conn net.Conn, verdict *protocol.ExtenderProbeVerdict) {
	frameBytes, err := ExtenderProbeVerdictFrame(verdict)
	if err != nil {
		panic(err)
	}
	conn.Write(frameBytes)
}

// Probes the fake target once with the attestor, failing the test on an error.
func probeTestVerdictTarget(t *testing.T, target *testVerdictTarget, attestor *ExtenderProbeAttestor) *ExtenderLatencyProbe {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	probe, err := ProbeExtenderLatency(ctx, testVerdictConnectSettings(), target.config(), attestor)
	if err != nil {
		t.Fatal(err)
	}
	if probe.Rtt <= 0 {
		t.Fatalf("the measurement is gone: rtt = %s", probe.Rtt)
	}
	return probe
}

// The outcome follows the verdict, for both pinger kinds, and the measurement
// stands whatever it is.
func TestProbeExtenderLatencyRecordsTheVerdict(t *testing.T) {
	cases := []struct {
		name     string
		answer   func(target *testVerdictTarget, conn net.Conn, attestation *protocol.ExtenderProbeAttestation)
		outcome  ExtenderPingOutcome
		reason   uint32
		verdict  bool
		cosigned bool
	}{
		{
			name: "co-signed",
			answer: func(target *testVerdictTarget, conn net.Conn, attestation *protocol.ExtenderProbeAttestation) {
				cosignature, err := SignExtenderProbeVerdict(testTargetSign(target.privateKey), attestation)
				if err != nil {
					panic(err)
				}
				writeTestVerdict(conn, &protocol.ExtenderProbeVerdict{Accepted: true, Cosignature: cosignature})
			},
			outcome:  ExtenderPingCosigned,
			verdict:  true,
			cosigned: true,
		},
		{
			name: "co-signed, then more bytes the pinger never reads",
			answer: func(target *testVerdictTarget, conn net.Conn, attestation *protocol.ExtenderProbeAttestation) {
				cosignature, err := SignExtenderProbeVerdict(testTargetSign(target.privateKey), attestation)
				if err != nil {
					panic(err)
				}
				writeTestVerdict(conn, &protocol.ExtenderProbeVerdict{Accepted: true, Cosignature: cosignature})
				conn.Write([]byte{0xff, 0xff, 0xff, 0xff, 1, 2, 3})
			},
			outcome:  ExtenderPingCosigned,
			verdict:  true,
			cosigned: true,
		},
		{
			name: "refused",
			answer: func(target *testVerdictTarget, conn net.Conn, attestation *protocol.ExtenderProbeAttestation) {
				writeTestVerdict(conn, &protocol.ExtenderProbeVerdict{Reason: ExtenderProbeVerdictReasonRttBelowObserved})
			},
			outcome: ExtenderPingRejected,
			reason:  ExtenderProbeVerdictReasonRttBelowObserved,
			verdict: true,
		},
		{
			name: "refused with a valid co-signature",
			answer: func(target *testVerdictTarget, conn net.Conn, attestation *protocol.ExtenderProbeAttestation) {
				cosignature, err := SignExtenderProbeVerdict(testTargetSign(target.privateKey), attestation)
				if err != nil {
					panic(err)
				}
				writeTestVerdict(conn, &protocol.ExtenderProbeVerdict{Reason: ExtenderProbeVerdictReasonNonce, Cosignature: cosignature})
			},
			outcome: ExtenderPingRejected,
			reason:  ExtenderProbeVerdictReasonNonce,
			verdict: true,
		},
		{
			name: "accepted with a co-signature that does not verify",
			answer: func(target *testVerdictTarget, conn net.Conn, attestation *protocol.ExtenderProbeAttestation) {
				writeTestVerdict(conn, &protocol.ExtenderProbeVerdict{Accepted: true, Cosignature: make([]byte, 64)})
			},
			outcome: ExtenderPingRejected,
			verdict: true,
		},
		{
			name: "accepted with another key's co-signature",
			answer: func(target *testVerdictTarget, conn net.Conn, attestation *protocol.ExtenderProbeAttestation) {
				_, otherPrivateKey, err := ed25519.GenerateKey(rand.Reader)
				if err != nil {
					panic(err)
				}
				cosignature, err := SignExtenderProbeVerdict(testTargetSign(otherPrivateKey), attestation)
				if err != nil {
					panic(err)
				}
				writeTestVerdict(conn, &protocol.ExtenderProbeVerdict{Accepted: true, Cosignature: cosignature})
			},
			outcome: ExtenderPingRejected,
			verdict: true,
		},
		{
			name: "accepted with no co-signature",
			answer: func(target *testVerdictTarget, conn net.Conn, attestation *protocol.ExtenderProbeAttestation) {
				writeTestVerdict(conn, &protocol.ExtenderProbeVerdict{Accepted: true})
			},
			outcome: ExtenderPingRejected,
			verdict: true,
		},
		{
			name: "an old target that closes",
			answer: func(target *testVerdictTarget, conn net.Conn, attestation *protocol.ExtenderProbeAttestation) {
			},
			outcome: ExtenderPingUnknown,
		},
		{
			name: "a target that never answers",
			answer: func(target *testVerdictTarget, conn net.Conn, attestation *protocol.ExtenderProbeAttestation) {
				// until the pinger gives up and closes
				io.Copy(io.Discard, conn)
			},
			outcome: ExtenderPingUnknown,
		},
		{
			name: "an empty frame",
			answer: func(target *testVerdictTarget, conn net.Conn, attestation *protocol.ExtenderProbeAttestation) {
				conn.Write([]byte{0, 0, 0, 0})
			},
			outcome: ExtenderPingUnknown,
		},
		{
			name: "an oversized frame",
			answer: func(target *testVerdictTarget, conn net.Conn, attestation *protocol.ExtenderProbeAttestation) {
				conn.Write([]byte{0xff, 0xff, 0xff, 0xff})
			},
			outcome: ExtenderPingUnknown,
		},
		{
			name: "a truncated frame",
			answer: func(target *testVerdictTarget, conn net.Conn, attestation *protocol.ExtenderProbeAttestation) {
				conn.Write([]byte{0, 0, 0, 10, 8, 1})
			},
			outcome: ExtenderPingUnknown,
		},
		{
			name: "a frame that is not a verdict",
			answer: func(target *testVerdictTarget, conn net.Conn, attestation *protocol.ExtenderProbeAttestation) {
				conn.Write([]byte{0, 0, 0, 2, 0xff, 0xff})
			},
			outcome: ExtenderPingUnknown,
		},
	}
	provider, _ := newTestProbeAttestor(t)
	extender, _ := newTestPeerProbeAttestor(t)
	for _, c := range cases {
		for _, attestor := range []*ExtenderProbeAttestor{provider, extender} {
			target := newTestVerdictTarget(t, c.answer)
			probe := probeTestVerdictTarget(t, target, attestor)
			name := c.name + " " + string(attestor.Kind())
			if !probe.Attested || probe.Attestation == nil || probe.AttestErr != nil {
				t.Fatalf("%s: not attested: %v", name, probe.AttestErr)
			}
			if probe.Outcome != c.outcome || probe.Cosigned != c.cosigned || probe.Reason != c.reason {
				t.Fatalf("%s: outcome %q cosigned %t reason %d", name, probe.Outcome, probe.Cosigned, probe.Reason)
			}
			if (probe.Verdict != nil) != c.verdict {
				t.Fatalf("%s: verdict = %v", name, probe.Verdict)
			}
			if !c.verdict && probe.VerdictErr == nil {
				t.Fatalf("%s: no verdict and no reason why", name)
			}
			// the claim the target received is the one the probe kept, naming
			// the target by the key it published
			received := <-target.attestations
			if !proto.Equal(received, probe.Attestation) {
				t.Fatalf("%s: the target received another claim", name)
			}
			if !slices.Equal(received.ExtenderPublicKey, target.publicKey) {
				t.Fatalf("%s: the claim names another target", name)
			}
			header := <-target.headers
			switch attestor.Kind() {
			case ExtenderPingerKindProvider:
				if !slices.Equal(header.ProbeClientId, attestor.ClientId.Bytes()) || 0 < len(header.ProbeExtenderPublicKey) {
					t.Fatalf("%s: the header names %x / %x", name, header.ProbeClientId, header.ProbeExtenderPublicKey)
				}
			case ExtenderPingerKindExtender:
				if !slices.Equal(header.ProbeExtenderPublicKey, attestor.ExtenderPublicKey) || 0 < len(header.ProbeClientId) {
					t.Fatalf("%s: the header names %x / %x", name, header.ProbeClientId, header.ProbeExtenderPublicKey)
				}
			}
			// and the transport form reports what was recorded
			report := ExtenderPingReportFromProbe(probe)
			if report == nil || report.Outcome != c.outcome || (report.Cosignature != "") != c.cosigned {
				t.Fatalf("%s: report = %+v", name, report)
			}
		}
	}
}

// A target that issues a nonce but publishes no key has nothing a claim could
// name: the pinger attests nothing and says why, and the probe still ranks.
func TestProbeExtenderLatencyRefusesToAttestToAnAnonymousTarget(t *testing.T) {
	target := newTestVerdictTarget(t, func(target *testVerdictTarget, conn net.Conn, attestation *protocol.ExtenderProbeAttestation) {
	})
	target.anonymous = true
	attestor, _ := newTestProbeAttestor(t)
	probe := probeTestVerdictTarget(t, target, attestor)
	if probe.Attested || probe.AttestErr == nil || probe.Outcome != ExtenderPingUnattested {
		t.Fatalf("attested=%t err=%v outcome=%q", probe.Attested, probe.AttestErr, probe.Outcome)
	}
	select {
	case attestation := <-target.attestations:
		t.Fatalf("the target received a claim: %v", attestation)
	default:
	}
}

// An attestor that names no single identity is carried by no probe: the probe
// ranks, identifies nothing, and says why it did not attest.
func TestProbeExtenderLatencyRanksWithAMalformedAttestor(t *testing.T) {
	target := newTestVerdictTarget(t, func(target *testVerdictTarget, conn net.Conn, attestation *protocol.ExtenderProbeAttestation) {
	})
	provider, _ := newTestProbeAttestor(t)
	malformed := &ExtenderProbeAttestor{
		ClientId:          provider.ClientId,
		ExtenderPublicKey: newTestExtenderKey(t),
		Sign:              provider.Sign,
	}
	probe := probeTestVerdictTarget(t, target, malformed)
	if probe.Attested || probe.AttestErr == nil || probe.Outcome != ExtenderPingUnattested {
		t.Fatalf("attested=%t err=%v outcome=%q", probe.Attested, probe.AttestErr, probe.Outcome)
	}
	header := <-target.headers
	if 0 < len(header.ProbeClientId) || 0 < len(header.ProbeExtenderPublicKey) {
		t.Fatal("a malformed attestor identified itself")
	}
	if 0 < len(probe.Response.ProbeNonce) {
		t.Fatal("a probe that named nobody was issued a nonce")
	}
}
