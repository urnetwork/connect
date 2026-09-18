package extender

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

// The latency probe end to end against the in-process extender
// (DESIGNNOTES4.md): a ranking client measures and identifies nothing, a
// provider attests and the gate holds.

// A provider's attestor over a fresh client key, and the key the operator
// would verify with.
func newTestProviderAttestor(t *testing.T) (*connect.ExtenderProbeAttestor, ed25519.PublicKey) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &connect.ExtenderProbeAttestor{
		ClientId: connect.NewId(),
		Sign: func(data []byte) []byte {
			return ed25519.Sign(privateKey, data)
		},
	}, publicKey
}

// The attestations an extender's handler received.
type testAttestations struct {
	received chan *protocol.ExtenderProbeAttestation
}

func newTestAttestations() *testAttestations {
	return &testAttestations{
		received: make(chan *protocol.ExtenderProbeAttestation, 64),
	}
}

func (self *testAttestations) handle(attestation *protocol.ExtenderProbeAttestation) {
	self.received <- attestation
}

func (self *testAttestations) next(t *testing.T) *protocol.ExtenderProbeAttestation {
	t.Helper()
	select {
	case attestation := <-self.received:
		return attestation
	case <-time.After(10 * time.Second):
		t.Fatal("no attestation reached the handler")
		return nil
	}
}

func (self *testAttestations) none(t *testing.T) {
	t.Helper()
	select {
	case attestation := <-self.received:
		t.Fatalf("an attestation reached the handler: %v", attestation)
	case <-time.After(300 * time.Millisecond):
	}
}

// An open extender with an identity and a handler, and the client config of
// one of its carriers pinned to that identity.
func newTestProbeFixture(
	t *testing.T,
	attestations *testAttestations,
	configure func(settings *ExtenderSettings),
) (*extenderFixture, ed25519.PublicKey) {
	t.Helper()
	seed, err := connect.NewExtenderKeySeed()
	if err != nil {
		t.Fatal(err)
	}
	publicKey, err := connect.ExtenderPublicKeyFromSeed(seed)
	if err != nil {
		t.Fatal(err)
	}
	fixture := newExtenderFixtureWithSecrets(t, "127.0.0.1", nil, func(settings *ExtenderSettings) {
		settings.IdentityKeySeed = seed
		if attestations != nil {
			settings.ProbeAttestationHandler = attestations.handle
		}
		if configure != nil {
			configure(settings)
		}
	})
	return fixture, publicKey
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

// A ranking probe measures a round trip on every carrier, gets no nonce, and
// never reaches the handler.
func TestProbeExtenderLatencyRanksWithoutIdentifying(t *testing.T) {
	attestations := newTestAttestations()
	fixture, publicKey := newTestProbeFixture(t, attestations, nil)

	for _, carrier := range []string{connect.ExtenderCarrierTcp, connect.ExtenderCarrierQuic, connect.ExtenderCarrierDns} {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		probe, err := connect.ProbeExtenderLatency(ctx, fixture.connectSettings(), testProbeConfig(fixture, carrier, publicKey), nil)
		cancel()
		if err != nil {
			t.Fatalf("%s: %v", carrier, err)
		}
		if probe.Rtt <= 0 {
			t.Fatalf("%s: rtt = %s", carrier, probe.Rtt)
		}
		if probe.Attested || probe.AttestErr != nil {
			t.Fatalf("%s: a ranking probe attested (%v)", carrier, probe.AttestErr)
		}
		if len(probe.Response.ProbeNonce) != 0 {
			t.Fatalf("%s: a ranking probe was issued a nonce", carrier)
		}
		if !slices.Equal(probe.Response.PublicKey, publicKey) {
			t.Fatalf("%s: the response names another key", carrier)
		}
	}
	attestations.none(t)
}

// A provider's probe is issued a nonce, attests on every carrier, and what
// reaches the handler verifies under the provider's key with the fields the
// provider signed.
func TestProbeExtenderLatencyAttestsForAProvider(t *testing.T) {
	attestations := newTestAttestations()
	fixture, publicKey := newTestProbeFixture(t, attestations, nil)
	attestor, providerPublicKey := newTestProviderAttestor(t)

	for _, carrier := range []string{connect.ExtenderCarrierTcp, connect.ExtenderCarrierQuic, connect.ExtenderCarrierDns} {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		probe, err := connect.ProbeExtenderLatency(ctx, fixture.connectSettings(), testProbeConfig(fixture, carrier, publicKey), attestor)
		cancel()
		if err != nil {
			t.Fatalf("%s: %v", carrier, err)
		}
		if !probe.Attested {
			t.Fatalf("%s: not attested: %v", carrier, probe.AttestErr)
		}
		if len(probe.Response.ProbeNonce) != connect.ExtenderProbeNonceByteCount {
			t.Fatalf("%s: nonce is %d bytes", carrier, len(probe.Response.ProbeNonce))
		}

		attestation := attestations.next(t)
		if !slices.Equal(attestation.ProbeClientId, attestor.ClientId.Bytes()) {
			t.Fatalf("%s: the attestation names another client", carrier)
		}
		if !slices.Equal(attestation.ExtenderPublicKey, publicKey) {
			t.Fatalf("%s: the attestation names another extender", carrier)
		}
		if !slices.Equal(attestation.ProbeNonce, probe.Response.ProbeNonce) {
			t.Fatalf("%s: the attestation echoes another nonce", carrier)
		}
		if attestation.RttMs == 0 {
			t.Fatalf("%s: the attestation claims no rtt", carrier)
		}
		if !connect.VerifyExtenderProbeAttestation(providerPublicKey, attestation) {
			t.Fatalf("%s: the attestation does not verify under the provider's key", carrier)
		}
	}
}

// An extender without an identity has nothing to bind a claim to, and one
// without a handler has nowhere to send it: neither issues a nonce, and the
// provider's probe still measures.
func TestProbeExtenderLatencyIssuesNoNonceWithoutIdentityOrHandler(t *testing.T) {
	attestor, _ := newTestProviderAttestor(t)

	attestations := newTestAttestations()
	noIdentity := newExtenderFixtureWithSecrets(t, "127.0.0.1", nil, func(settings *ExtenderSettings) {
		settings.ProbeAttestationHandler = attestations.handle
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	probe, err := connect.ProbeExtenderLatency(ctx, noIdentity.connectSettings(), testProbeConfig(noIdentity, connect.ExtenderCarrierTcp, nil), attestor)
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	if probe.Attested || len(probe.Response.ProbeNonce) != 0 || probe.Rtt <= 0 {
		t.Fatalf("no identity: attested=%t nonce=%d rtt=%s", probe.Attested, len(probe.Response.ProbeNonce), probe.Rtt)
	}
	attestations.none(t)

	noHandler, publicKey := newTestProbeFixture(t, nil, nil)
	ctx, cancel = context.WithTimeout(context.Background(), 20*time.Second)
	probe, err = connect.ProbeExtenderLatency(ctx, noHandler.connectSettings(), testProbeConfig(noHandler, connect.ExtenderCarrierTcp, publicKey), attestor)
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	if probe.Attested || len(probe.Response.ProbeNonce) != 0 || probe.Rtt <= 0 {
		t.Fatalf("no handler: attested=%t nonce=%d rtt=%s", probe.Attested, len(probe.Response.ProbeNonce), probe.Rtt)
	}
}

// Dials a probe as an attesting provider and returns the stream, positioned
// after the response, with the nonce it was issued.
func dialTestAttestingProbe(
	t *testing.T,
	fixture *extenderFixture,
	publicKey []byte,
	attestor *connect.ExtenderProbeAttestor,
) (*connect.ExtenderRoundTrip, []byte, func([]byte) error, func()) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	roundTrip := &connect.ExtenderRoundTrip{}
	conn, response, err := connect.DialExtender(
		ctx,
		fixture.connectSettings(),
		testProbeConfig(fixture, connect.ExtenderCarrierTcp, publicKey),
		&connect.ExtenderDial{
			Service:       connect.ExtenderServiceProbe,
			ProbeClientId: attestor.ClientId.Bytes(),
			RoundTrip:     roundTrip,
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
	write := func(frameBytes []byte) error {
		_, err := conn.Write(frameBytes)
		return err
	}
	return roundTrip, response.ProbeNonce, write, func() {
		conn.Close()
		cancel()
	}
}

// The gate (DESIGNNOTES4.md §3): a claim below what the extender observed is
// refused, one above is accepted, and a claim that does not echo this probe's
// nonce, client id or extender key is refused whatever it says.
func TestExtenderProbeGate(t *testing.T) {
	attestations := newTestAttestations()
	fixture, publicKey := newTestProbeFixture(t, attestations, func(settings *ExtenderSettings) {
		settings.ProbeRttTolerance = 20 * time.Millisecond
	})
	attestor, _ := newTestProviderAttestor(t)
	otherAttestor, _ := newTestProviderAttestor(t)
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

	cases := []struct {
		name string
		// how long to hold the attestation before sending it, which is what
		// the extender observes
		hold time.Duration
		// the claimed rtt
		rttMs    uint32
		mutate   func(a *protocol.ExtenderProbeAttestation)
		accepted bool
		reason   string
	}{
		{name: "deflated", hold: 150 * time.Millisecond, rttMs: 1, accepted: false, reason: "below the observed"},
		{name: "just under the tolerance", hold: 150 * time.Millisecond, rttMs: 100, accepted: false, reason: "below the observed"},
		{name: "honest", hold: 100 * time.Millisecond, rttMs: 100, accepted: true},
		{name: "inflated", hold: 0, rttMs: 5000, accepted: true},
		{name: "another nonce", hold: 0, rttMs: 5000, accepted: false, reason: "nonce", mutate: func(a *protocol.ExtenderProbeAttestation) {
			nonce, _ := connect.NewExtenderProbeNonce()
			a.ProbeNonce = nonce
		}},
		{name: "another client id", hold: 0, rttMs: 5000, accepted: false, reason: "client id", mutate: func(a *protocol.ExtenderProbeAttestation) {
			a.ProbeClientId = otherAttestor.ClientId.Bytes()
		}},
		{name: "another extender", hold: 0, rttMs: 5000, accepted: false, reason: "another extender", mutate: func(a *protocol.ExtenderProbeAttestation) {
			a.ExtenderPublicKey = otherExtenderKey
		}},
		{name: "no signature", hold: 0, rttMs: 5000, accepted: false, reason: "signature", mutate: func(a *protocol.ExtenderProbeAttestation) {
			a.Signature = nil
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, nonce, write, closeProbe := dialTestAttestingProbe(t, fixture, publicKey, attestor)
			defer closeProbe()

			attestation := &protocol.ExtenderProbeAttestation{
				ProbeClientId:     attestor.ClientId.Bytes(),
				ExtenderPublicKey: publicKey,
				ProbeNonce:        nonce,
				RttMs:             c.rttMs,
				TimestampMs:       uint64(time.Now().UnixMilli()),
			}
			if err := connect.SignExtenderProbeAttestation(attestor, attestation); err != nil {
				t.Fatal(err)
			}
			if c.mutate != nil {
				c.mutate(attestation)
			}
			frameBytes, err := connect.ExtenderProbeAttestationFrame(attestation)
			if err != nil {
				t.Fatal(err)
			}
			if 0 < c.hold {
				time.Sleep(c.hold)
			}
			if err := write(frameBytes); err != nil {
				t.Fatal(err)
			}

			if c.accepted {
				received := attestations.next(t)
				if received.RttMs != c.rttMs {
					t.Fatalf("the handler received rtt %d, expected %d", received.RttMs, c.rttMs)
				}
				return
			}
			err = waitForFixtureError(t, fixture, "probe gate")
			if !strings.Contains(err.Error(), c.reason) {
				t.Fatalf("refused for %q, expected %q", err, c.reason)
			}
			attestations.none(t)
		})
	}
}

// The extender waits a bounded time for the attestation frame and closes the
// stream; a provider that never sends one costs it nothing more.
func TestExtenderProbeAttestationTimeout(t *testing.T) {
	attestations := newTestAttestations()
	fixture, publicKey := newTestProbeFixture(t, attestations, func(settings *ExtenderSettings) {
		settings.ProbeAttestationTimeout = 200 * time.Millisecond
	})
	attestor, _ := newTestProviderAttestor(t)

	_, _, _, closeProbe := dialTestAttestingProbe(t, fixture, publicKey, attestor)
	defer closeProbe()

	err := waitForFixtureError(t, fixture, "probe attestation")
	var netErr interface{ Timeout() bool }
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Fatalf("the read ended with %v, expected a timeout", err)
	}
	attestations.none(t)
}

// A source over its probe rate is refused before any of the above.
func TestExtenderProbeRateLimitRefusesAFlood(t *testing.T) {
	attestations := newTestAttestations()
	fixture, publicKey := newTestProbeFixture(t, attestations, func(settings *ExtenderSettings) {
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
	fixture, publicKey := newTestProbeFixture(t, nil, nil)
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
