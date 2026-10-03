package extender

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

// The latency probe end to end against the in-process extender
// (DESIGNNOTES4.md, GEOMAP §2): a ranking client measures and identifies
// nothing, a provider or a peer extender attests, the gate holds, and every
// claim is answered with a verdict.

// A provider's attestor over a fresh client key, and the key the operator
// would verify with.
func newTestProviderAttestor(t *testing.T) (*connect.ExtenderProbeAttestor, ed25519.PublicKey) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return connect.NewExtenderProbeProviderAttestor(connect.NewId(), func(data []byte) []byte {
		return ed25519.Sign(privateKey, data)
	}), publicKey
}

// A peer extender's attestor over a fresh identity key, signing as an
// extender does, and the key.
func newTestPeerAttestor(t *testing.T) (*connect.ExtenderProbeAttestor, ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return connect.NewExtenderProbeExtenderAttestor(publicKey, connect.NewExtenderPeerProbeSigner(privateKey)), publicKey, privateKey
}

// The peer verifier of a fixture: the keys it vouches for, and every key it
// was asked about.
type testPeerVerifier struct {
	stateLock sync.Mutex
	active    [][]byte
	asked     [][]byte
}

// Whether the key is one the verifier vouches for, recording the ask.
func (self *testPeerVerifier) verify(publicKey []byte) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.asked = append(self.asked, slices.Clone(publicKey))
	for _, active := range self.active {
		if bytes.Equal(active, publicKey) {
			return true
		}
	}
	return false
}

// Every key asked about so far, as a copy.
func (self *testPeerVerifier) askedValue() [][]byte {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.asked)
}

// An open extender with an identity, and the identity's key pair.
func newTestProbeFixture(
	t *testing.T,
	configure func(settings *ExtenderSettings),
) (*extenderFixture, ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	seed, err := connect.NewExtenderKeySeed()
	if err != nil {
		t.Fatal(err)
	}
	privateKey, err := connect.ExtenderPrivateKeyFromSeed(seed)
	if err != nil {
		t.Fatal(err)
	}
	fixture := newExtenderFixtureWithSecrets(t, "127.0.0.1", nil, func(settings *ExtenderSettings) {
		settings.IdentityKeySeed = seed
		if configure != nil {
			configure(settings)
		}
	})
	return fixture, privateKey.Public().(ed25519.PublicKey), privateKey
}

func testProbeConfig(fixture *extenderFixture, carrier string, publicKey []byte) *connect.ExtenderConfig {
	return &connect.ExtenderConfig{
		Profile:   fixture.profile(carrier),
		Ip:        fixture.ip,
		PublicKey: publicKey,
	}
}

// The next fixture error whose stage matches, within a bound.
func waitForFixtureError(t *testing.T, fixture *extenderFixture, stage string) error {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case err := <-fixture.errors:
			if strings.HasPrefix(err.Error(), stage+":") {
				return err
			}
		case <-deadline:
			t.Fatalf("no %q error was reported", stage)
			return nil
		}
	}
}

// Every fixture error reported so far, without waiting.
func drainFixtureErrors(fixture *extenderFixture) []error {
	errs := []error{}
	for {
		select {
		case err := <-fixture.errors:
			errs = append(errs, err)
		default:
			return errs
		}
	}
}

// Probes the fixture once over the carrier with the attestor, failing the test
// on an error.
func probeTestFixture(
	t *testing.T,
	fixture *extenderFixture,
	carrier string,
	publicKey []byte,
	attestor *connect.ExtenderProbeAttestor,
) *connect.ExtenderLatencyProbe {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	probe, err := connect.ProbeExtenderLatency(ctx, fixture.connectSettings(), testProbeConfig(fixture, carrier, publicKey), attestor)
	if err != nil {
		t.Fatalf("%s: %v", carrier, err)
	}
	if probe.Rtt <= 0 {
		t.Fatalf("%s: rtt = %s", carrier, probe.Rtt)
	}
	return probe
}

var testProbeCarriers = []string{connect.ExtenderCarrierTcp, connect.ExtenderCarrierQuic, connect.ExtenderCarrierDns}

// A ranking probe measures a round trip on every carrier, gets no nonce, and
// neither sends nor waits for anything more.
func TestProbeExtenderLatencyRanksWithoutIdentifying(t *testing.T) {
	fixture, publicKey, _ := newTestProbeFixture(t, nil)
	for _, carrier := range testProbeCarriers {
		probe := probeTestFixture(t, fixture, carrier, publicKey, nil)
		if probe.Attested || probe.AttestErr != nil || probe.Outcome != connect.ExtenderPingUnattested {
			t.Fatalf("%s: a ranking probe attested (%v)", carrier, probe.AttestErr)
		}
		if probe.Verdict != nil || probe.Attestation != nil {
			t.Fatalf("%s: a ranking probe carries a claim or a verdict", carrier)
		}
		if len(probe.Response.ProbeNonce) != 0 {
			t.Fatalf("%s: a ranking probe was issued a nonce", carrier)
		}
		if !slices.Equal(probe.Response.PublicKey, publicKey) {
			t.Fatalf("%s: the response names another key", carrier)
		}
	}
	for _, err := range drainFixtureErrors(fixture) {
		if strings.HasPrefix(err.Error(), "probe") {
			t.Fatalf("a ranking probe reached the gate: %v", err)
		}
	}
}

// A provider's probe is issued a nonce and attests on every carrier; the
// extender accepts it and answers with its co-signature, which verifies under
// the extender's key over exactly the claim the provider signed.
func TestProbeExtenderLatencyCosignsForAProvider(t *testing.T) {
	fixture, publicKey, _ := newTestProbeFixture(t, nil)
	attestor, providerPublicKey := newTestProviderAttestor(t)

	for _, carrier := range testProbeCarriers {
		probe := probeTestFixture(t, fixture, carrier, publicKey, attestor)
		if !probe.Attested || probe.Outcome != connect.ExtenderPingCosigned || !probe.Cosigned {
			t.Fatalf("%s: outcome %q attested %t: %v / %v", carrier, probe.Outcome, probe.Attested, probe.AttestErr, probe.VerdictErr)
		}
		if len(probe.Response.ProbeNonce) != connect.ExtenderProbeNonceByteCount {
			t.Fatalf("%s: nonce is %d bytes", carrier, len(probe.Response.ProbeNonce))
		}
		attestation := probe.Attestation
		if !slices.Equal(attestation.ProbeClientId, attestor.ClientId.Bytes()) || 0 < len(attestation.PingerExtenderPublicKey) {
			t.Fatalf("%s: the claim names another pinger", carrier)
		}
		if !slices.Equal(attestation.ExtenderPublicKey, publicKey) || !slices.Equal(attestation.ProbeNonce, probe.Response.ProbeNonce) {
			t.Fatalf("%s: the claim names another extender or nonce", carrier)
		}
		if attestation.RttMs == 0 {
			t.Fatalf("%s: the claim is of no rtt", carrier)
		}
		if !connect.VerifyExtenderProbeAttestation(providerPublicKey, attestation) {
			t.Fatalf("%s: the claim does not verify under the provider's key", carrier)
		}
		if !probe.Verdict.Accepted || probe.Verdict.Reason != connect.ExtenderProbeVerdictReasonOk {
			t.Fatalf("%s: verdict = %v", carrier, probe.Verdict)
		}
		if !connect.VerifyExtenderProbeVerdict(publicKey, attestation, probe.Verdict) {
			t.Fatalf("%s: the co-signature does not verify under the extender's key", carrier)
		}
	}
}

// A peer extender the verifier vouches for is co-signed on every carrier, and
// the verifier is asked about exactly its key.
func TestProbeExtenderLatencyCosignsAnActivePeer(t *testing.T) {
	attestor, pingerPublicKey, _ := newTestPeerAttestor(t)
	verifier := &testPeerVerifier{active: [][]byte{pingerPublicKey}}
	fixture, publicKey, _ := newTestProbeFixture(t, func(settings *ExtenderSettings) {
		settings.ProbePeerVerifier = verifier.verify
	})
	for _, carrier := range testProbeCarriers {
		probe := probeTestFixture(t, fixture, carrier, publicKey, attestor)
		if probe.Outcome != connect.ExtenderPingCosigned || !probe.Cosigned {
			t.Fatalf("%s: outcome %q: %v / %v / %v", carrier, probe.Outcome, probe.AttestErr, probe.VerdictErr, probe.Verdict)
		}
		attestation := probe.Attestation
		if !slices.Equal(attestation.PingerExtenderPublicKey, pingerPublicKey) || 0 < len(attestation.ProbeClientId) {
			t.Fatalf("%s: the claim names another pinger", carrier)
		}
		if !connect.VerifyExtenderProbeAttestation(pingerPublicKey, attestation) {
			t.Fatalf("%s: the claim does not verify under the pinger's key", carrier)
		}
		if !connect.VerifyExtenderProbeVerdict(publicKey, attestation, probe.Verdict) {
			t.Fatalf("%s: the co-signature does not verify under the extender's key", carrier)
		}
		// the co-signature is the target's and only the target's
		if connect.VerifyExtenderProbeVerdict(pingerPublicKey, attestation, probe.Verdict) {
			t.Fatalf("%s: the co-signature verifies under the pinger's key", carrier)
		}
	}
	asked := verifier.askedValue()
	if len(asked) != len(testProbeCarriers) {
		t.Fatalf("the verifier was asked %d times, expected once per probe", len(asked))
	}
	for _, key := range asked {
		if !bytes.Equal(key, pingerPublicKey) {
			t.Fatal("the verifier was asked about another key")
		}
	}
}

// A peer the verifier does not vouch for, or any peer with no verifier at
// all, is refused as an unknown pinger -- with a verdict, not silence.
func TestProbeExtenderLatencyRefusesAnUnknownPeer(t *testing.T) {
	attestor, _, _ := newTestPeerAttestor(t)
	_, otherPublicKey, _ := newTestPeerAttestor(t)
	for name, configure := range map[string]func(settings *ExtenderSettings){
		"verifier refuses": func(settings *ExtenderSettings) {
			settings.ProbePeerVerifier = (&testPeerVerifier{active: [][]byte{otherPublicKey}}).verify
		},
		"no verifier": nil,
	} {
		fixture, publicKey, _ := newTestProbeFixture(t, configure)
		for _, carrier := range []string{connect.ExtenderCarrierTcp, connect.ExtenderCarrierQuic} {
			probe := probeTestFixture(t, fixture, carrier, publicKey, attestor)
			if len(probe.Response.ProbeNonce) != connect.ExtenderProbeNonceByteCount {
				t.Fatalf("%s, %s: an extender pinger was issued no nonce", name, carrier)
			}
			if probe.Outcome != connect.ExtenderPingRejected || probe.Cosigned {
				t.Fatalf("%s, %s: outcome %q", name, carrier, probe.Outcome)
			}
			if probe.Verdict == nil || probe.Verdict.Accepted || probe.Reason != connect.ExtenderProbeVerdictReasonUnknownPinger {
				t.Fatalf("%s, %s: verdict = %v", name, carrier, probe.Verdict)
			}
			if 0 < len(probe.Verdict.Cosignature) {
				t.Fatalf("%s, %s: a refusal carries a co-signature", name, carrier)
			}
			if err := waitForFixtureError(t, fixture, "probe gate"); !strings.Contains(err.Error(), "not an active peer") {
				t.Fatalf("%s, %s: refused for %v", name, carrier, err)
			}
		}
	}
}

// A provider needs no verifier: the operator verifies it, and a verifier that
// vouches for nobody changes nothing for a provider.
func TestProbeExtenderLatencyAsksNoVerifierOfAProvider(t *testing.T) {
	verifier := &testPeerVerifier{}
	fixture, publicKey, _ := newTestProbeFixture(t, func(settings *ExtenderSettings) {
		settings.ProbePeerVerifier = verifier.verify
	})
	attestor, _ := newTestProviderAttestor(t)
	probe := probeTestFixture(t, fixture, connect.ExtenderCarrierTcp, publicKey, attestor)
	if probe.Outcome != connect.ExtenderPingCosigned {
		t.Fatalf("outcome %q", probe.Outcome)
	}
	if asked := verifier.askedValue(); len(asked) != 0 {
		t.Fatalf("the verifier was asked about a provider: %d", len(asked))
	}
}

// An extender without an identity has nothing to bind a claim to: it issues
// no nonce to either kind of pinger, and the probe still measures.
func TestProbeExtenderLatencyIssuesNoNonceWithoutIdentity(t *testing.T) {
	provider, _ := newTestProviderAttestor(t)
	extender, _, _ := newTestPeerAttestor(t)
	noIdentity := newExtenderFixtureWithSecrets(t, "127.0.0.1", nil, func(settings *ExtenderSettings) {
		settings.ProbePeerVerifier = func(publicKey []byte) bool { return true }
	})
	for _, attestor := range []*connect.ExtenderProbeAttestor{provider, extender} {
		probe := probeTestFixture(t, noIdentity, connect.ExtenderCarrierTcp, nil, attestor)
		if probe.Attested || len(probe.Response.ProbeNonce) != 0 || probe.Outcome != connect.ExtenderPingUnattested {
			t.Fatalf("%s: attested=%t nonce=%d outcome=%q", attestor.Kind(), probe.Attested, len(probe.Response.ProbeNonce), probe.Outcome)
		}
	}
}

// A header that names both a provider and an extender is refused outright.
func TestExtenderProbeRefusesBothIdentities(t *testing.T) {
	fixture, publicKey, _ := newTestProbeFixture(t, func(settings *ExtenderSettings) {
		settings.ProbePeerVerifier = func(publicKey []byte) bool { return true }
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	conn, _, err := connect.DialExtender(
		ctx,
		fixture.connectSettings(),
		testProbeConfig(fixture, connect.ExtenderCarrierTcp, publicKey),
		&connect.ExtenderDial{
			Service:                connect.ExtenderServiceProbe,
			ProbeClientId:          connect.NewId().Bytes(),
			ProbeExtenderPublicKey: bytes.Repeat([]byte{7}, ed25519.PublicKeySize),
		},
	)
	if conn != nil {
		conn.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("a probe naming both was not refused: %v", err)
	}
	if err := waitForFixtureError(t, fixture, "probe"); !strings.Contains(err.Error(), "both") {
		t.Fatalf("refused for %v", err)
	}
}

// Dials a probe that names one pinger, and returns the stream positioned
// after the response, with the nonce it was issued.
func dialTestAttestingProbe(
	t *testing.T,
	fixture *extenderFixture,
	carrier string,
	publicKey []byte,
	clientId []byte,
	pingerPublicKey []byte,
) (net.Conn, []byte, func()) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	conn, response, err := connect.DialExtender(
		ctx,
		fixture.connectSettings(),
		testProbeConfig(fixture, carrier, publicKey),
		&connect.ExtenderDial{
			Service:                connect.ExtenderServiceProbe,
			ProbeClientId:          clientId,
			ProbeExtenderPublicKey: pingerPublicKey,
			RoundTrip:              &connect.ExtenderRoundTrip{},
		},
	)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if len(response.ProbeNonce) != connect.ExtenderProbeNonceByteCount {
		conn.Close()
		cancel()
		t.Fatalf("nonce is %d bytes", len(response.ProbeNonce))
	}
	return conn, response.ProbeNonce, func() {
		conn.Close()
		cancel()
	}
}

// Reads the one verdict the extender answers a claim with.
func readTestVerdict(t *testing.T, conn net.Conn) *protocol.ExtenderProbeVerdict {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	verdict, err := connect.ReadExtenderProbeVerdictFrame(conn)
	if err != nil {
		t.Fatalf("no verdict: %v", err)
	}
	return verdict
}

// Writes one attestation frame on the probe's stream.
func writeTestAttestation(t *testing.T, conn net.Conn, attestation *protocol.ExtenderProbeAttestation) {
	t.Helper()
	frameBytes, err := connect.ExtenderProbeAttestationFrame(attestation)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write(frameBytes); err != nil {
		t.Fatal(err)
	}
}

// The gate (DESIGNNOTES4.md §3, GEOMAP §2.4) for both kinds of pinger: a
// claim below what the extender observed is refused, one above accepted; a
// claim that does not echo this probe's nonce, name this extender, name the
// pinger the header named, or carry a signature the extender can check is
// refused whatever it says -- and every one of them is answered with its
// reason.
func TestExtenderProbeGate(t *testing.T) {
	peerAttestor, peerPublicKey, _ := newTestPeerAttestor(t)
	otherPeerAttestor, otherPeerPublicKey, _ := newTestPeerAttestor(t)
	unknownPeerAttestor, _, _ := newTestPeerAttestor(t)
	verifier := &testPeerVerifier{active: [][]byte{peerPublicKey, otherPeerPublicKey}}
	fixture, publicKey, privateKey := newTestProbeFixture(t, func(settings *ExtenderSettings) {
		settings.ProbeRttTolerance = 20 * time.Millisecond
		settings.ProbePeerVerifier = verifier.verify
		// every case probes from the one loopback source
		settings.ProbeMaxRatePerSource = 0
	})
	// this extender's own key, as an attestor, for a claim of a ping to itself
	selfAttestor := connect.NewExtenderProbeExtenderAttestor(publicKey, connect.NewExtenderPeerProbeSigner(privateKey))
	provider, _ := newTestProviderAttestor(t)
	otherProvider, _ := newTestProviderAttestor(t)
	otherExtenderKey := func() []byte {
		seed, err := connect.NewExtenderKeySeed()
		if err != nil {
			t.Fatal(err)
		}
		key, err := connect.ExtenderPublicKeyFromSeed(seed)
		if err != nil {
			t.Fatal(err)
		}
		return key
	}()
	verifier.stateLock.Lock()
	verifier.active = append(verifier.active, slices.Clone(publicKey))
	verifier.stateLock.Unlock()

	cases := []struct {
		name     string
		attestor *connect.ExtenderProbeAttestor
		// how long to hold the attestation before sending it, which is what
		// the extender observes
		hold time.Duration
		// the claimed rtt
		rttMs uint32
		// before the signature, or after it
		mutate      func(a *protocol.ExtenderProbeAttestation)
		mutateAfter func(a *protocol.ExtenderProbeAttestation)
		reason      uint32
		message     string
	}{
		{name: "provider deflated", attestor: provider, hold: 150 * time.Millisecond, rttMs: 1, reason: connect.ExtenderProbeVerdictReasonRttBelowObserved, message: "below the observed"},
		{name: "provider just under the tolerance", attestor: provider, hold: 150 * time.Millisecond, rttMs: 100, reason: connect.ExtenderProbeVerdictReasonRttBelowObserved, message: "below the observed"},
		{name: "provider honest", attestor: provider, hold: 100 * time.Millisecond, rttMs: 100},
		{name: "provider inflated", attestor: provider, rttMs: 5000},
		{name: "provider another nonce", attestor: provider, rttMs: 5000, reason: connect.ExtenderProbeVerdictReasonNonce, message: "nonce", mutateAfter: func(a *protocol.ExtenderProbeAttestation) {
			nonce, _ := connect.NewExtenderProbeNonce()
			a.ProbeNonce = nonce
		}},
		{name: "provider another client id", attestor: provider, rttMs: 5000, reason: connect.ExtenderProbeVerdictReasonUnknownPinger, message: "client id", mutateAfter: func(a *protocol.ExtenderProbeAttestation) {
			a.ProbeClientId = otherProvider.ClientId.Bytes()
		}},
		{name: "provider also names a pinger key", attestor: provider, rttMs: 5000, reason: connect.ExtenderProbeVerdictReasonUnknownPinger, message: "client id", mutateAfter: func(a *protocol.ExtenderProbeAttestation) {
			a.PingerExtenderPublicKey = slices.Clone(peerPublicKey)
		}},
		{name: "provider another extender", attestor: provider, rttMs: 5000, reason: connect.ExtenderProbeVerdictReasonWrongExtender, message: "another extender", mutateAfter: func(a *protocol.ExtenderProbeAttestation) {
			a.ExtenderPublicKey = otherExtenderKey
		}},
		{name: "provider no signature", attestor: provider, rttMs: 5000, reason: connect.ExtenderProbeVerdictReasonBadSignature, message: "signature", mutateAfter: func(a *protocol.ExtenderProbeAttestation) {
			a.Signature = nil
		}},
		{name: "peer honest", attestor: peerAttestor, rttMs: 100},
		{name: "peer inflated", attestor: peerAttestor, rttMs: 5000},
		{name: "peer deflated", attestor: peerAttestor, hold: 150 * time.Millisecond, rttMs: 1, reason: connect.ExtenderProbeVerdictReasonRttBelowObserved, message: "below the observed"},
		{name: "peer another nonce", attestor: peerAttestor, rttMs: 5000, reason: connect.ExtenderProbeVerdictReasonNonce, message: "nonce", mutate: func(a *protocol.ExtenderProbeAttestation) {
			nonce, _ := connect.NewExtenderProbeNonce()
			a.ProbeNonce = nonce
		}},
		{name: "peer another extender", attestor: peerAttestor, rttMs: 5000, reason: connect.ExtenderProbeVerdictReasonWrongExtender, message: "another extender", mutate: func(a *protocol.ExtenderProbeAttestation) {
			a.ExtenderPublicKey = otherExtenderKey
		}},
		{name: "peer wrong signature", attestor: peerAttestor, rttMs: 5000, reason: connect.ExtenderProbeVerdictReasonBadSignature, message: "does not verify", mutateAfter: func(a *protocol.ExtenderProbeAttestation) {
			// signed by another active peer, over this claim
			other := &protocol.ExtenderProbeAttestation{
				PingerExtenderPublicKey: slices.Clone(otherPeerPublicKey),
				ExtenderPublicKey:       a.ExtenderPublicKey,
				ProbeNonce:              a.ProbeNonce,
				RttMs:                   a.RttMs,
				TimestampMs:             a.TimestampMs,
			}
			if err := connect.SignExtenderProbeAttestation(otherPeerAttestor, other); err != nil {
				panic(err)
			}
			a.Signature = other.Signature
		}},
		{name: "peer rtt changed after signing", attestor: peerAttestor, rttMs: 5000, reason: connect.ExtenderProbeVerdictReasonBadSignature, message: "does not verify", mutateAfter: func(a *protocol.ExtenderProbeAttestation) {
			a.RttMs += 1
		}},
		{name: "peer no signature", attestor: peerAttestor, rttMs: 5000, reason: connect.ExtenderProbeVerdictReasonBadSignature, message: "does not verify", mutateAfter: func(a *protocol.ExtenderProbeAttestation) {
			a.Signature = nil
		}},
		{name: "peer names another pinger", attestor: otherPeerAttestor, rttMs: 5000, reason: connect.ExtenderProbeVerdictReasonUnknownPinger, message: "another pinger extender"},
		{name: "peer also names a client id", attestor: peerAttestor, rttMs: 5000, reason: connect.ExtenderProbeVerdictReasonUnknownPinger, message: "another pinger extender", mutateAfter: func(a *protocol.ExtenderProbeAttestation) {
			a.ProbeClientId = provider.ClientId.Bytes()
		}},
		{name: "peer the verifier does not know", attestor: unknownPeerAttestor, rttMs: 5000, reason: connect.ExtenderProbeVerdictReasonUnknownPinger, message: "not an active peer"},
		{name: "a ping of itself", attestor: selfAttestor, rttMs: 5000, reason: connect.ExtenderProbeVerdictReasonUnknownPinger, message: "this extender"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var clientId, pingerPublicKey []byte
			switch c.attestor.Kind() {
			case connect.ExtenderPingerKindProvider:
				clientId = c.attestor.ClientId.Bytes()
			case connect.ExtenderPingerKindExtender:
				pingerPublicKey = slices.Clone(c.attestor.ExtenderPublicKey)
				if c.attestor == otherPeerAttestor {
					// the header names one peer and the claim another
					pingerPublicKey = slices.Clone(peerPublicKey)
				}
			}
			conn, nonce, closeProbe := dialTestAttestingProbe(t, fixture, connect.ExtenderCarrierTcp, publicKey, clientId, pingerPublicKey)
			defer closeProbe()

			attestation := &protocol.ExtenderProbeAttestation{
				ExtenderPublicKey: publicKey,
				ProbeNonce:        nonce,
				RttMs:             c.rttMs,
				TimestampMs:       uint64(time.Now().UnixMilli()),
			}
			switch c.attestor.Kind() {
			case connect.ExtenderPingerKindProvider:
				attestation.ProbeClientId = c.attestor.ClientId.Bytes()
			case connect.ExtenderPingerKindExtender:
				attestation.PingerExtenderPublicKey = slices.Clone(c.attestor.ExtenderPublicKey)
			}
			if c.mutate != nil {
				c.mutate(attestation)
			}
			if err := connect.SignExtenderProbeAttestation(c.attestor, attestation); err != nil {
				t.Fatal(err)
			}
			if c.mutateAfter != nil {
				c.mutateAfter(attestation)
			}
			if 0 < c.hold {
				time.Sleep(c.hold)
			}
			writeTestAttestation(t, conn, attestation)
			verdict := readTestVerdict(t, conn)

			if c.reason == connect.ExtenderProbeVerdictReasonOk && c.message == "" {
				if !verdict.Accepted || verdict.Reason != connect.ExtenderProbeVerdictReasonOk {
					t.Fatalf("refused: %v", verdict)
				}
				if !connect.VerifyExtenderProbeVerdict(publicKey, attestation, verdict) {
					t.Fatal("the acceptance's co-signature does not verify")
				}
				return
			}
			if verdict.Accepted || verdict.Reason != c.reason || 0 < len(verdict.Cosignature) {
				t.Fatalf("verdict = %v, expected a refusal with reason %d", verdict, c.reason)
			}
			err := waitForFixtureError(t, fixture, "probe gate")
			if !strings.Contains(err.Error(), c.message) {
				t.Fatalf("refused for %q, expected %q", err, c.message)
			}
		})
	}
}

// The extender waits a bounded time for the attestation frame and closes the
// stream with no verdict: there was no claim to judge.
func TestExtenderProbeAttestationTimeout(t *testing.T) {
	fixture, publicKey, _ := newTestProbeFixture(t, func(settings *ExtenderSettings) {
		settings.ProbeAttestationTimeout = 200 * time.Millisecond
	})
	attestor, _ := newTestProviderAttestor(t)

	conn, _, closeProbe := dialTestAttestingProbe(t, fixture, connect.ExtenderCarrierTcp, publicKey, attestor.ClientId.Bytes(), nil)
	defer closeProbe()

	err := waitForFixtureError(t, fixture, "probe attestation")
	var netErr interface{ Timeout() bool }
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Fatalf("the read ended with %v, expected a timeout", err)
	}
	// the stream ends with nothing written after the response
	if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if n, _ := io.Copy(io.Discard, conn); n != 0 {
		t.Fatalf("the extender wrote %d bytes after a missing claim", n)
	}
}

// A provider built before the verdict closes its side right after its frame,
// or reads one byte and closes. The extender still judges the claim -- the
// refusal is reported like any other -- a failed verdict write costs it
// nothing, and it keeps serving.
func TestExtenderProbeOldProviderClosesFirst(t *testing.T) {
	fixture, publicKey, _ := newTestProbeFixture(t, func(settings *ExtenderSettings) {
		// every case probes from the one loopback source
		settings.ProbeMaxRatePerSource = 0
	})
	attestor, _ := newTestProviderAttestor(t)

	for _, c := range []struct {
		name string
		// after the frame: close at once, or read the one byte the old
		// provider waited for
		readOne bool
		// claim a deflated rtt, so the judgement is visible as a refusal
		deflate bool
	}{
		{name: "closes, deflated", deflate: true},
		{name: "reads one byte, deflated", readOne: true, deflate: true},
		{name: "closes, honest"},
		{name: "reads one byte, honest", readOne: true},
	} {
		drainFixtureErrors(fixture)
		conn, nonce, closeProbe := dialTestAttestingProbe(t, fixture, connect.ExtenderCarrierTcp, publicKey, attestor.ClientId.Bytes(), nil)
		rttMs := uint32(5000)
		if c.deflate {
			rttMs = 1
			// holding the frame is the rtt the extender observes: it cannot
			// arrive before it is written, so the interval is at least this
			// whatever the scheduler does
			time.Sleep(150 * time.Millisecond)
		}
		attestation := &protocol.ExtenderProbeAttestation{
			ProbeClientId:     attestor.ClientId.Bytes(),
			ExtenderPublicKey: publicKey,
			ProbeNonce:        nonce,
			RttMs:             rttMs,
			TimestampMs:       uint64(time.Now().UnixMilli()),
		}
		if err := connect.SignExtenderProbeAttestation(attestor, attestation); err != nil {
			t.Fatal(err)
		}
		writeTestAttestation(t, conn, attestation)
		if c.readOne {
			if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
				t.Fatal(err)
			}
			io.Copy(io.Discard, io.LimitReader(conn, 1))
		}
		closeProbe()

		if c.deflate {
			if err := waitForFixtureError(t, fixture, "probe gate"); !strings.Contains(err.Error(), "below the observed") {
				t.Fatalf("%s: refused for %v", c.name, err)
			}
		}
		// the extender serves the next probe as if nothing happened, and the
		// stages it reported are the gate's and, at most, the write of a
		// verdict nobody read
		probe := probeTestFixture(t, fixture, connect.ExtenderCarrierTcp, publicKey, attestor)
		if probe.Outcome != connect.ExtenderPingCosigned {
			t.Fatalf("%s: the next probe came to %q", c.name, probe.Outcome)
		}
		for _, err := range drainFixtureErrors(fixture) {
			switch {
			case strings.HasPrefix(err.Error(), "probe verdict:"):
			case strings.HasPrefix(err.Error(), "probe gate:") && c.deflate:
			default:
				t.Fatalf("%s: an old provider's close was reported as %v", c.name, err)
			}
		}
	}
}

// A source over its probe rate is refused before any of the above.
func TestExtenderProbeRateLimitRefusesAFlood(t *testing.T) {
	fixture, publicKey, _ := newTestProbeFixture(t, func(settings *ExtenderSettings) {
		settings.ProbeMaxRatePerSource = 0.001
		settings.ProbeMaxBurstPerSource = 2
	})
	for i := range 2 {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		_, err := connect.ProbeExtenderLatency(ctx, fixture.connectSettings(), testProbeConfig(fixture, connect.ExtenderCarrierTcp, publicKey), nil)
		cancel()
		if err != nil {
			t.Fatalf("probe %d: %v", i, err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	_, err := connect.ProbeExtenderLatency(ctx, fixture.connectSettings(), testProbeConfig(fixture, connect.ExtenderCarrierTcp, publicKey), nil)
	cancel()
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("the third probe was not refused: %v", err)
	}
	// the limit is per probe, not per connection: a forward still works
	ctx, cancel = context.WithTimeout(context.Background(), 20*time.Second)
	_, err = connect.ProbeExtenderCarrier(
		ctx, fixture.connectSettings(), fixture.ip, connect.ExtenderConnectModeTcpTls, fixture.tcpPort, "",
		testServerName, publicKey, "dest.example", 443)
	cancel()
	if err != nil {
		t.Fatalf("a forward was refused with the probe limit: %v", err)
	}
}

// The round trip a probe measures is one request and its response over the
// established carrier, not the handshake: it is small on loopback, and it is
// set on every carrier.
func TestExtenderProbeRoundTripBracketsTheRequest(t *testing.T) {
	fixture, publicKey, _ := newTestProbeFixture(t, nil)
	for _, carrier := range []string{connect.ExtenderCarrierTcp, connect.ExtenderCarrierQuic} {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		roundTrip := &connect.ExtenderRoundTrip{}
		start := time.Now()
		conn, _, err := connect.DialExtender(
			ctx,
			fixture.connectSettings(),
			testProbeConfig(fixture, carrier, publicKey),
			&connect.ExtenderDial{Service: connect.ExtenderServiceProbe, RoundTrip: roundTrip},
		)
		dialTime := time.Since(start)
		cancel()
		if err != nil {
			t.Fatalf("%s: %v", carrier, err)
		}
		conn.Close()
		if roundTrip.SendTime.IsZero() || roundTrip.ReceiveTime.IsZero() {
			t.Fatalf("%s: the round trip was not marked", carrier)
		}
		if roundTrip.Rtt() <= 0 || dialTime < roundTrip.Rtt() {
			t.Fatalf("%s: rtt %s is not inside the dial %s", carrier, roundTrip.Rtt(), dialTime)
		}
		if !roundTrip.SendTime.After(start) {
			t.Fatalf("%s: the request was marked sent before the dial began", carrier)
		}
	}
}

// The identity key signs co-signatures only under their domain: the same key
// signs challenges and the certificate authority.
func TestExtenderCertificatesSignOnlyCosignatures(t *testing.T) {
	seed, err := connect.NewExtenderKeySeed()
	if err != nil {
		t.Fatal(err)
	}
	certificates, err := newExtenderCertificates(seed, DefaultExtenderSettings())
	if err != nil {
		t.Fatal(err)
	}
	cosignBytes := append([]byte(connect.ExtenderProbeCosignDomain), make([]byte, 176)...)
	signature := certificates.SignProbeCosign(cosignBytes)
	if !ed25519.Verify(certificates.PublicKey(), cosignBytes, signature) {
		t.Fatal("the co-signature does not verify under the identity key")
	}
	for _, data := range [][]byte{
		append([]byte(connect.ExtenderChallengeSignatureDomain), make([]byte, 32)...),
		append([]byte(connect.ExtenderPeerProbeSignatureDomain), make([]byte, 108)...),
		append([]byte(connect.ExtenderProbeSignatureDomain), make([]byte, 92)...),
		{0x30, 0x82},
		nil,
	} {
		if signature := certificates.SignProbeCosign(data); signature != nil {
			t.Fatalf("the identity key signed %q as a co-signature", data)
		}
	}
	anonymous, err := newExtenderCertificates(nil, DefaultExtenderSettings())
	if err != nil {
		t.Fatal(err)
	}
	if signature := anonymous.SignProbeCosign(cosignBytes); signature != nil {
		t.Fatal("an extender without an identity co-signed")
	}
}
