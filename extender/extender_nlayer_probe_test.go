// Probes through a chain of NLayer extenders (EXTENDER.md A11, GEOMAP §2.9):
// the front relays a probe to the end of its chain and answers with its own
// identity, the end's nonce, the end's depth and the end's key; the pinger
// binds its claim to that key; the end judges and co-signs exactly as for a
// direct probe; and the front keeps one relayed probe in flight per signed
// source, which is what ends a probe that loops.

package extender

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

// One layer of a probe chain: an open extender with an identity, as an
// operator activated one is, and its key.
type nlayerProbeLayer struct {
	fixture   *extenderFixture
	publicKey ed25519.PublicKey
}

// Builds depth layers on loopback, each relaying to the next over tcp with the
// next layer's key pinned, the last answering probes itself. configure runs on
// every layer's settings with its position, 0 being the front.
func newNLayerProbeChain(
	t *testing.T,
	depth int,
	configure func(position int, settings *ExtenderSettings),
) []*nlayerProbeLayer {
	t.Helper()
	layers := make([]*nlayerProbeLayer, depth)
	for position := depth - 1; 0 <= position; position -= 1 {
		var hop *nlayerProbeLayer
		if position+1 < depth {
			hop = layers[position+1]
		}
		fixture, publicKey, _ := newTestProbeFixture(t, func(settings *ExtenderSettings) {
			// every probe of a test leaves the same loopback address, which
			// the per-source rate would count as one pinger's
			settings.ProbeMaxRatePerSource = 0
			settings.NLayerMaxDepth = 8
			if hop != nil {
				hopConfig := hop.fixture.extenderConfig(connect.ExtenderCarrierTcp)
				hopConfig.PublicKey = hop.publicKey
				settings.NLayerHops = []*connect.ExtenderConfig{hopConfig}
			}
			if configure != nil {
				configure(position, settings)
			}
		})
		layers[position] = &nlayerProbeLayer{
			fixture:   fixture,
			publicKey: publicKey,
		}
	}
	return layers
}

// The pinger identities a front holds a relayed probe in flight for.
func nlayerProbeSourceCount(server *ExtenderServer) int {
	server.stateLock.Lock()
	defer server.stateLock.Unlock()
	return len(server.nlayerProbeSources)
}

// The lowest rtt of count ranking probes of one layer, pinned to its key.
func lowestNLayerProbeRtt(t *testing.T, layer *nlayerProbeLayer, count int) time.Duration {
	t.Helper()
	var lowestRtt time.Duration
	for i := 0; i < count; i += 1 {
		probe := probeTestFixture(t, layer.fixture, connect.ExtenderCarrierTcp, layer.publicKey, nil)
		if lowestRtt == 0 || probe.Rtt < lowestRtt {
			lowestRtt = probe.Rtt
		}
	}
	return lowestRtt
}

// A provider probe through chains of two and eight: the pinger's pin on the
// front verifies and the response is the front's, but the nonce is the end's,
// the depth is the end's and the claim names the end's key, which is the key
// the end co-signs under. The report names the end at its depth, every layer
// accepted the probe one deeper, and the round trip the pinger measures is
// the chain's: longer than a direct probe of the end.
func TestNLayerRelaysAProviderProbeToTheChainEnd(t *testing.T) {
	for _, depth := range []int{2, 8} {
		layers := newNLayerProbeChain(t, depth, nil)
		front, end := layers[0], layers[depth-1]
		attestor, _ := newTestProviderAttestor(t)

		probe := probeTestFixture(t, front.fixture, connect.ExtenderCarrierTcp, front.publicKey, attestor)
		if !slices.Equal(probe.Response.PublicKey, front.publicKey) {
			t.Fatalf("depth %d: the response is another extender's", depth)
		}
		if probe.HopCount != uint32(depth-1) || probe.Response.HopCount != uint32(depth-1) {
			t.Fatalf("depth %d: hop count = %d (response %d)", depth, probe.HopCount, probe.Response.HopCount)
		}
		if !slices.Equal(probe.ChainEndPublicKey, end.publicKey) || !slices.Equal(probe.Response.ChainEndPublicKey, end.publicKey) {
			t.Fatalf("depth %d: the chain end is not the last layer", depth)
		}
		// the end issued the nonce and judged the claim: only its own nonce
		// passes its gate
		if !probe.Attested || !probe.Cosigned || probe.Outcome != connect.ExtenderPingCosigned {
			t.Fatalf("depth %d: outcome = %q, attest err = %v, verdict err = %v", depth, probe.Outcome, probe.AttestErr, probe.VerdictErr)
		}
		if !slices.Equal(probe.Attestation.ExtenderPublicKey, end.publicKey) {
			t.Fatalf("depth %d: the claim names another extender than the end", depth)
		}
		if !connect.VerifyExtenderProbeVerdict(end.publicKey, probe.Attestation, probe.Verdict) {
			t.Fatalf("depth %d: the co-signature does not verify under the end's key", depth)
		}
		if connect.VerifyExtenderProbeVerdict(front.publicKey, probe.Attestation, probe.Verdict) {
			t.Fatalf("depth %d: the co-signature verifies under the front's key", depth)
		}
		report := connect.ExtenderPingReportFromProbe(probe)
		if report == nil || report.TargetExtenderPublicKeyHex != hex.EncodeToString(end.publicKey) ||
			report.HopCount != uint32(depth-1) || report.Outcome != connect.ExtenderPingCosigned {
			t.Fatalf("depth %d: report = %+v", depth, report)
		}
		for position, layer := range layers {
			if hopCounts := layer.fixture.acceptedHopCounts.snapshot(); !slices.Equal(hopCounts, []uint32{uint32(position)}) {
				t.Fatalf("depth %d: layer %d accepted hop counts %v", depth, position, hopCounts)
			}
		}
		if sourceCount := nlayerProbeSourceCount(front.fixture.server); sourceCount != 0 {
			waitForNLayer(t, "the front to release the source", func() bool {
				return nlayerProbeSourceCount(front.fixture.server) == 0
			})
		}

		chainRtt := lowestNLayerProbeRtt(t, front, 3)
		directRtt := lowestNLayerProbeRtt(t, end, 3)
		if chainRtt <= directRtt {
			t.Fatalf("depth %d: the chain's rtt %s is not above a direct probe's %s", depth, chainRtt, directRtt)
		}
		t.Logf(
			"provider probe through %d extenders: cosigned by the end at hop count %d; lowest rtt %s through the chain, %s direct to the end",
			depth,
			probe.HopCount,
			chainRtt,
			directRtt,
		)
	}
}

// The directory of a peer pinger, signed by a fresh root key: a verified
// record for a layer on its tcp carrier, and the pinger's own, since a pinger
// waits for its own record to be active before it claims anything.
func newNLayerPeerDirectory(
	t *testing.T,
	ctx context.Context,
	layer *nlayerProbeLayer,
	ownPublicKey ed25519.PublicKey,
) *connect.ExtenderDirectory {
	t.Helper()
	const networkHost = "nlayer.space.example"
	rootPublicKey, rootPrivateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	settings := connect.DefaultExtenderDirectorySettings()
	settings.NetworkHosts = []string{networkHost}
	directory := connect.NewExtenderDirectory(ctx, settings)
	t.Cleanup(directory.Close)
	directory.SetRootKeys(connect.NewExtenderRootKeySet(rootPublicKey))
	issueTime := time.Now()
	applyRecord := func(publicKey ed25519.PublicKey, ip string, tcpPort int, udpPort int, dnsPort int) {
		record, err := connect.SignExtenderRecord(rootPrivateKey, &protocol.ExtenderRecordBody{
			PublicKey: publicKey,
			Addresses: []*protocol.ExtenderAddress{{
				Ip:        ip,
				IpVersion: 4,
				Carriers:  []string{connect.ExtenderCarrierTcp},
			}},
			TcpPort:      uint32(tcpPort),
			UdpPort:      uint32(udpPort),
			DnsPort:      uint32(dnsPort),
			DnsTld:       testDnsTld,
			CountryCode:  "us",
			IssueTimeMs:  uint64(issueTime.UnixMilli()),
			ExpireTimeMs: uint64(issueTime.Add(24 * time.Hour).UnixMilli()),
			NetworkHost:  networkHost,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := directory.ApplyRecord(record, connect.ExtenderSourceBootstrap); err != nil {
			t.Fatal(err)
		}
	}
	applyRecord(layer.publicKey, layer.fixture.ip.String(), layer.fixture.tcpPort, layer.fixture.quicPort, layer.fixture.dnsPort)
	// never dialed: a pinger does not ping itself
	applyRecord(ownPublicKey, "198.51.100.9", connect.ExtenderTcpPort, connect.ExtenderQuicPort, connect.ExtenderDnsPort)
	return directory
}

// An extender's own peer pinger pings a front from its directory: the ping is
// relayed to the end, whose peer verifier judges the pinger's key exactly as
// for a direct ping and co-signs; the pinger verifies the co-signature under
// the end's key, reports the end at depth one, and records the sample against
// the front's address, which is the path it ranks. The front judges nothing.
func TestNLayerRelaysAPeerPingToTheChainEnd(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pingerAttestor, pingerPublicKey, _ := newTestPeerAttestor(t)
	endVerifier := &testPeerVerifier{active: [][]byte{pingerPublicKey}}
	frontVerifier := &testPeerVerifier{active: [][]byte{pingerPublicKey}}
	layers := newNLayerProbeChain(t, 2, func(position int, settings *ExtenderSettings) {
		if position == 0 {
			settings.ProbePeerVerifier = frontVerifier.verify
		} else {
			settings.ProbePeerVerifier = endVerifier.verify
		}
	})
	front, end := layers[0], layers[1]
	directory := newNLayerPeerDirectory(t, ctx, front, pingerPublicKey)

	reports := &recordedValues[*connect.ExtenderPingReport]{}
	reporterSettings := connect.DefaultExtenderPingReporterSettings()
	reporterSettings.MaxBatchCount = 1
	reporterSettings.Post = func(ctx context.Context, args *connect.ExtenderPingReportArgs) (*connect.ExtenderPingReportResult, error) {
		for _, report := range args.Pings {
			reports.add(report)
		}
		return &connect.ExtenderPingReportResult{Accepted: len(args.Pings)}, nil
	}
	reporter := connect.NewExtenderPingReporter(ctx, reporterSettings)
	t.Cleanup(reporter.Close)

	pingerSettings := connect.DefaultExtenderPeerPingerSettings()
	pingerSettings.OwnPublicKey = pingerPublicKey
	pingerSettings.Attestor = pingerAttestor
	pingerSettings.Reporter = reporter
	pingerSettings.SpreadTimeout = 0
	pingerSettings.ProbeCount = 1
	pingerSettings.IpVersionSupported = func(ipVersion int) bool {
		return ipVersion == 4
	}
	pinger := connect.NewExtenderPeerPinger(ctx, nil, directory, pingerSettings)
	t.Cleanup(pinger.Close)

	waitForNLayer(t, "the peer ping", func() bool {
		status, _ := pinger.StatusMonitor().Get()
		return 1 <= status.PingCount
	})
	status, _ := pinger.StatusMonitor().Get()
	if status.CosignedCount != 1 {
		t.Fatalf("status = %+v, expected one cosigned ping", status)
	}
	waitForNLayer(t, "the ping report", func() bool {
		return 1 <= reports.count()
	})
	report := reports.snapshot()[0]
	if report.PingerKind != connect.ExtenderPingerKindExtender ||
		report.PingerExtenderPublicKeyHex != hex.EncodeToString(pingerPublicKey) ||
		report.TargetExtenderPublicKeyHex != hex.EncodeToString(end.publicKey) ||
		report.HopCount != 1 ||
		report.Outcome != connect.ExtenderPingCosigned {
		t.Fatalf("report = %+v", report)
	}
	// the operator's own check of the pair, from the report alone
	attestation, err := report.Proto()
	if err != nil {
		t.Fatal(err)
	}
	verdict, err := report.VerdictProto()
	if err != nil {
		t.Fatal(err)
	}
	if !connect.VerifyExtenderProbeAttestation(pingerPublicKey, attestation) ||
		!connect.VerifyExtenderProbeVerdict(end.publicKey, attestation, verdict) {
		t.Fatal("the reported claim or co-signature does not verify")
	}

	// the sample is on the address dialed, attested
	var frontEntry *connect.ExtenderDirectoryEntry
	for _, entry := range directory.Snapshot().Entries {
		if entry.Ip == front.fixture.ip {
			frontEntry = entry
		}
	}
	if frontEntry == nil || frontEntry.Latency <= 0 {
		t.Fatalf("the front's address has no latency sample: %+v", frontEntry)
	}
	if latencies := directory.MeasuredLatencies(4, true); len(latencies) != 1 || latencies[0] != frontEntry.Latency {
		t.Fatalf("attested latencies = %v, expected the front's", latencies)
	}

	if !slices.ContainsFunc(endVerifier.askedValue(), func(asked []byte) bool {
		return slices.Equal(asked, pingerPublicKey)
	}) {
		t.Fatal("the end did not judge the pinger's key")
	}
	if asked := frontVerifier.askedValue(); 0 < len(asked) {
		t.Fatalf("the front judged %d pingers; it only relays", len(asked))
	}
	t.Logf("peer ping through a front: cosigned by the end at hop count %d, sample %s on the front's address", report.HopCount, frontEntry.Latency)
}

// One raw attesting probe of the front for a provider client id, returning
// the stream after the response, the response, and the dial error.
func dialNLayerProviderProbe(
	ctx context.Context,
	layer *nlayerProbeLayer,
	clientId connect.Id,
) (net.Conn, *protocol.ExtenderResponse, error) {
	return connect.DialExtender(
		ctx,
		layer.fixture.connectSettings(),
		testProbeConfig(layer.fixture, connect.ExtenderCarrierTcp, layer.publicKey),
		&connect.ExtenderDial{
			Service:       connect.ExtenderServiceProbe,
			ProbeClientId: clientId.Bytes(),
		},
	)
}

// Completes one held provider probe: the claim for the end's nonce under the
// end's key, and the verdict. The claim is generous, since the end's interval
// runs from its response to this frame and the test held the stream between.
func completeNLayerProviderProbe(
	t *testing.T,
	conn net.Conn,
	response *protocol.ExtenderResponse,
	attestor *connect.ExtenderProbeAttestor,
) *protocol.ExtenderProbeVerdict {
	t.Helper()
	attestation := &protocol.ExtenderProbeAttestation{
		ProbeClientId:     attestor.ClientId.Bytes(),
		ExtenderPublicKey: response.ChainEndPublicKey,
		ProbeNonce:        response.ProbeNonce,
		RttMs:             60 * 1000,
		TimestampMs:       uint64(time.Now().UnixMilli()),
	}
	if err := connect.SignExtenderProbeAttestation(attestor, attestation); err != nil {
		t.Fatal(err)
	}
	writeTestAttestation(t, conn, attestation)
	verdict := readTestVerdict(t, conn)
	if !connect.VerifyExtenderProbeVerdict(response.ChainEndPublicKey, attestation, verdict) {
		t.Fatalf("verdict = %+v, expected the end's co-signature", verdict)
	}
	return verdict
}

// A front holds one relayed probe per source: a second probe from a source
// with one in flight is refused with 403 while the first completes, a probe
// from another source is served beside it, and once the first relay ends its
// source is released and served again.
func TestNLayerHoldsOneRelayedProbePerSource(t *testing.T) {
	layers := newNLayerProbeChain(t, 2, nil)
	front := layers[0]
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	attestor, _ := newTestProviderAttestor(t)
	otherAttestor, _ := newTestProviderAttestor(t)

	first, firstResponse, err := dialNLayerProviderProbe(ctx, front, attestor.ClientId)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if len(firstResponse.ProbeNonce) != connect.ExtenderProbeNonceByteCount {
		t.Fatal("the relayed probe carries no nonce of the end's")
	}

	drainNLayerErrors(front.fixture)
	second, _, err := dialNLayerProviderProbe(ctx, front, attestor.ClientId)
	if second != nil {
		second.Close()
	}
	var refusedErr *connect.ExtenderRefusedError
	if !errors.As(err, &refusedErr) || refusedErr.StatusCode != http.StatusForbidden {
		t.Fatalf("a second probe of the same source got %v, expected a 403", err)
	}
	if _, ok := nextNLayerError(front.fixture, "nlayer probe"); !ok {
		t.Fatal("the refusal was not attributed to the per-source rule")
	}

	other, otherResponse, err := dialNLayerProviderProbe(ctx, front, otherAttestor.ClientId)
	if err != nil {
		t.Fatalf("a probe of another source was refused: %v", err)
	}
	defer other.Close()
	if sourceCount := nlayerProbeSourceCount(front.fixture.server); sourceCount != 2 {
		t.Fatalf("%d sources in flight, expected the two", sourceCount)
	}

	completeNLayerProviderProbe(t, first, firstResponse, attestor)
	completeNLayerProviderProbe(t, other, otherResponse, otherAttestor)
	first.Close()
	other.Close()
	waitForNLayer(t, "the relays to release their sources", func() bool {
		return nlayerProbeSourceCount(front.fixture.server) == 0
	})

	again, againResponse, err := dialNLayerProviderProbe(ctx, front, attestor.ClientId)
	if err != nil {
		t.Fatalf("the released source was refused: %v", err)
	}
	defer again.Close()
	completeNLayerProviderProbe(t, again, againResponse, attestor)
}

// A probe around a loop a -> b -> a is refused at a's second entry by its
// source, after exactly two hop dials: the probe carries no inner tls for the
// ClientHello check to read. A ranking probe names no source, and the depth
// bound stops it in the same loop after eight.
func TestNLayerProbeLoopIsRefusedBySource(t *testing.T) {
	const maxDepth = 8
	a, b := newNLayerLoop(t, func(settings *ExtenderSettings) {
		settings.NLayerMaxDepth = maxDepth
		settings.ProbeMaxRatePerSource = 0
	})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	attestor, _ := newTestProviderAttestor(t)

	_, err := connect.ProbeExtenderLatency(ctx, a.connectSettings(), a.extenderConfig(connect.ExtenderCarrierTcp), attestor)
	var refusedErr *connect.ExtenderRefusedError
	if !errors.As(err, &refusedErr) || refusedErr.StatusCode != http.StatusForbidden {
		t.Fatalf("a probe around the loop got %v, expected a 403", err)
	}
	if _, ok := nextNLayerError(a, "nlayer probe"); !ok {
		t.Fatalf("a did not refuse the probe that came back to it; %s", nlayerErrorsOf(a, b))
	}
	if hopDialCount := a.hopDialAddresses.count() + b.hopDialAddresses.count(); hopDialCount != 2 {
		t.Fatalf("the loop made %d hop dials, expected exactly 2", hopDialCount)
	}
	waitForNLayer(t, "the loop to release its sources", func() bool {
		return nlayerProbeSourceCount(a.server) == 0 && nlayerProbeSourceCount(b.server) == 0
	})

	// a ranking probe has no source to key, so only the depth bound ends it
	hopDialsBefore := a.hopDialAddresses.count() + b.hopDialAddresses.count()
	_, err = connect.ProbeExtenderLatency(ctx, a.connectSettings(), a.extenderConfig(connect.ExtenderCarrierTcp), nil)
	if !errors.As(err, &refusedErr) || refusedErr.StatusCode != http.StatusForbidden {
		t.Fatalf("a ranking probe around the loop got %v, expected a 403", err)
	}
	if _, ok := nextNLayerError(a, "hop count"); !ok {
		t.Fatalf("the ninth entry was not refused at the depth bound; %s", nlayerErrorsOf(a, b))
	}
	if hopDialCount := a.hopDialAddresses.count() + b.hopDialAddresses.count() - hopDialsBefore; hopDialCount != maxDepth {
		t.Fatalf("the ranking probe made %d hop dials, expected %d", hopDialCount, maxDepth)
	}
	t.Logf("probe loop a -> b -> a: refused by source after 2 hop dials; a ranking probe refused at the depth bound after %d", maxDepth)
}

// A ranking probe is relayed too, one layer deeper, and answered with the
// front's identity: its key, and its signature over the client's challenge.
func TestNLayerRelaysARankingProbe(t *testing.T) {
	layers := newNLayerProbeChain(t, 2, nil)
	front, end := layers[0], layers[1]
	probe := probeTestFixture(t, front.fixture, connect.ExtenderCarrierTcp, front.publicKey, nil)
	if !slices.Equal(probe.Response.PublicKey, front.publicKey) || 0 < len(probe.Response.ProbeNonce) {
		t.Fatalf("response = %+v, expected the front's key and no nonce", probe.Response)
	}
	if probe.HopCount != 1 || !slices.Equal(probe.ChainEndPublicKey, end.publicKey) {
		t.Fatalf("hop count %d, chain end %x", probe.HopCount, probe.ChainEndPublicKey)
	}
	if hopCounts := end.fixture.acceptedHopCounts.snapshot(); !slices.Equal(hopCounts, []uint32{1}) {
		t.Fatalf("the end accepted hop counts %v, expected [1]", hopCounts)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	challenge, err := connect.NewExtenderChallenge()
	if err != nil {
		t.Fatal(err)
	}
	conn, response, err := connect.DialExtender(
		ctx,
		front.fixture.connectSettings(),
		testProbeConfig(front.fixture, connect.ExtenderCarrierTcp, front.publicKey),
		&connect.ExtenderDial{
			Service:   connect.ExtenderServiceProbe,
			Challenge: challenge,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	if !connect.VerifyExtenderChallenge(front.publicKey, challenge, response.ChallengeSignature) {
		t.Fatal("the challenge is not signed by the front")
	}
	if connect.VerifyExtenderChallenge(end.publicKey, challenge, response.ChallengeSignature) {
		t.Fatal("the challenge is signed by the end")
	}
}

// A direct probe of an extender with no hops answers for itself: hop count 0,
// no chain end, and a claim named, co-signed and reported under its own key.
func TestExtenderDirectProbeNamesNoChain(t *testing.T) {
	fixture, publicKey, _ := newTestProbeFixture(t, nil)
	attestor, _ := newTestProviderAttestor(t)
	probe := probeTestFixture(t, fixture, connect.ExtenderCarrierTcp, publicKey, attestor)
	if probe.Response.HopCount != 0 || 0 < len(probe.Response.ChainEndPublicKey) {
		t.Fatalf("response = %+v, expected no chain", probe.Response)
	}
	if probe.HopCount != 0 || probe.ChainEndPublicKey != nil {
		t.Fatalf("hop count %d, chain end %x", probe.HopCount, probe.ChainEndPublicKey)
	}
	if !probe.Cosigned || !slices.Equal(probe.Attestation.ExtenderPublicKey, publicKey) {
		t.Fatalf("outcome = %q, claim names %x", probe.Outcome, probe.Attestation.ExtenderPublicKey)
	}
	if report := connect.ExtenderPingReportFromProbe(probe); report.HopCount != 0 ||
		report.TargetExtenderPublicKeyHex != hex.EncodeToString(publicKey) {
		t.Fatalf("report = %+v", report)
	}
}

// A hop that cannot be dialed leaves the pinger no response: the front refuses
// with 403, so the pinger probes again, holds the hop as it would for a
// forward, and releases the source.
func TestNLayerProbeWithNoHopIsRefused(t *testing.T) {
	brokenPort := closedLoopbackPort(t, "127.0.0.1")
	fixture, publicKey, _ := newTestProbeFixture(t, func(settings *ExtenderSettings) {
		settings.ProbeMaxRatePerSource = 0
		settings.NLayerHops = []*connect.ExtenderConfig{{
			Profile: connect.ExtenderProfile{
				ConnectMode: connect.ExtenderConnectModeTcpTls,
				ServerName:  testServerName,
				Port:        brokenPort,
			},
			Ip: netip.MustParseAddr("127.0.0.1"),
		}}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	attestor, _ := newTestProviderAttestor(t)
	_, err := connect.ProbeExtenderLatency(ctx, fixture.connectSettings(), testProbeConfig(fixture, connect.ExtenderCarrierTcp, publicKey), attestor)
	var refusedErr *connect.ExtenderRefusedError
	if !errors.As(err, &refusedErr) || refusedErr.StatusCode != http.StatusForbidden {
		t.Fatalf("a probe whose hop cannot be dialed got %v, expected a 403", err)
	}
	if _, ok := nextNLayerError(fixture, "nlayer probe dial"); !ok {
		t.Fatal("the refusal was not attributed to the hop dial")
	}
	if hopStats := fixture.server.NLayerStats()[0]; hopStats.FailedCount != 1 || hopStats.HeldUntil.IsZero() {
		t.Fatalf("stats = %+v, expected the hop failed and held", hopStats)
	}
	if sourceCount := nlayerProbeSourceCount(fixture.server); sourceCount != 0 {
		t.Fatalf("%d sources still in flight", sourceCount)
	}
}

// The depth bound comes before anything else of a probe, on an extender with
// no hops as on a front: a header at the bound is refused with 403.
func TestNLayerDepthBoundRefusesAProbeAtTheLimit(t *testing.T) {
	layers := newNLayerProbeChain(t, 2, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for position, layer := range layers {
		drainNLayerErrors(layer.fixture)
		conn, _, err := connect.DialExtender(
			ctx,
			layer.fixture.connectSettings(),
			testProbeConfig(layer.fixture, connect.ExtenderCarrierTcp, layer.publicKey),
			&connect.ExtenderDial{
				Service:  connect.ExtenderServiceProbe,
				HopCount: 8,
			},
		)
		if conn != nil {
			conn.Close()
		}
		var refusedErr *connect.ExtenderRefusedError
		if !errors.As(err, &refusedErr) || refusedErr.StatusCode != http.StatusForbidden {
			t.Fatalf("layer %d: a probe at the bound got %v, expected a 403", position, err)
		}
		if _, ok := nextNLayerError(layer.fixture, "hop count"); !ok {
			t.Fatalf("layer %d: the refusal was not attributed to the depth bound", position)
		}
	}
	if hopDialCount := layers[0].fixture.hopDialAddresses.count(); hopDialCount != 0 {
		t.Fatal(fmt.Sprintf("the front made %d hop dials for a refused probe", hopDialCount))
	}
}
