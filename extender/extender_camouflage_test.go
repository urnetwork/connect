// Root-cause tests for the tcp carrier camouflage (EXTENDER.md P, P10).
//
// Deterministic and in-process: the authenticated tests drive the real client
// dial against the fixture extender over loopback, the splice tests drive a raw
// prober against an in-process fake borrowed site, the server clock is an
// injected seam, and synthetic .example names stand in for the borrow list. No
// real network and no production identity.

package extender

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"net"
	"testing"
	"time"

	utls "github.com/refraction-networking/utls"

	"github.com/urnetwork/connect"
)

// Synthetic borrowed name the client fronts with and the server splices to.
const testBorrowName = "borrow.example"

// The identity seed and the two public keys a camouflage client needs.
func newCamouflageKeys(t *testing.T) (seed []byte, identityPublicKey []byte, realityPublicKey []byte) {
	t.Helper()
	seed, err := connect.NewExtenderKeySeed()
	if err != nil {
		t.Fatal(err)
	}
	identityKey, err := connect.ExtenderPublicKeyFromSeed(seed)
	if err != nil {
		t.Fatal(err)
	}
	realityKey, err := connect.ExtenderRealityStaticPublicKey(seed)
	if err != nil {
		t.Fatal(err)
	}
	return seed, []byte(identityKey), realityKey
}

// An in-process fake borrowed site the splice relays to. Each accepted
// connection runs handler; reached counts them and accepted signals each one, so
// a test can wait for a splice to begin without a sleep.
type fakeBorrowSite struct {
	listener net.Listener
	reached  *atomicCount
	accepted chan struct{}
}

func newFakeBorrowSite(t *testing.T, handler func(conn net.Conn)) *fakeBorrowSite {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	site := &fakeBorrowSite{
		listener: listener,
		reached:  &atomicCount{},
		accepted: make(chan struct{}, 1024),
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			site.reached.add()
			select {
			case site.accepted <- struct{}{}:
			default:
			}
			go handler(conn)
		}
	}()
	t.Cleanup(func() {
		listener.Close()
		<-done
	})
	return site
}

// The splice dial seam that reaches this site for any borrowed name.
func (self *fakeBorrowSite) dialContext() connect.DialContextFunction {
	return func(ctx context.Context, network string, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", self.listener.Addr().String())
	}
}

// A TLS borrowed-site handler: it completes a handshake with a self-signed cert,
// so a spliced client's handshake finishes and then fails its own B3 check.
func tlsBorrowSiteHandler(t *testing.T) func(conn net.Conn) {
	t.Helper()
	certificate, err := selfSignedCertificate([]string{testBorrowName}, "Borrow Test", time.Hour, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	config := &tls.Config{Certificates: []tls.Certificate{*certificate}}
	return func(conn net.Conn) {
		defer conn.Close()
		tlsConn := tls.Server(conn, config)
		tlsConn.HandshakeContext(context.Background())
		io.Copy(io.Discard, tlsConn)
	}
}

// A flooding borrowed-site handler: it drains reads and writes floodByteCount
// bytes, so a test can prove the per-connection byte bound cuts the relay.
func floodBorrowSiteHandler(floodByteCount int) func(conn net.Conn) {
	return func(conn net.Conn) {
		defer conn.Close()
		go io.Copy(io.Discard, conn)
		conn.Write(bytes.Repeat([]byte("x"), floodByteCount))
	}
}

// A blocking borrowed-site handler: it drains reads and holds the connection
// open until released, so a test can hold splice slots and probe the caps.
func blockingBorrowSiteHandler(release <-chan struct{}) func(conn net.Conn) {
	return func(conn net.Conn) {
		defer conn.Close()
		go io.Copy(io.Discard, conn)
		<-release
	}
}

// Builds the TLS record bytes of a plain, unauthenticated browser ClientHello
// fronting serverName, as a raw prober sends it. The random session id is not a
// valid tag, so the server opens nothing and classifies it unauthenticated.
func rawUnauthenticatedHelloRecord(t *testing.T, serverName string) []byte {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() {
		clientConn.Close()
		serverConn.Close()
	})
	uconn := utls.UClient(clientConn, &utls.Config{
		ServerName:         serverName,
		InsecureSkipVerify: true,
	}, utls.HelloChrome_133)
	t.Cleanup(func() { uconn.Close() })
	if err := uconn.BuildHandshakeState(); err != nil {
		t.Fatal(err)
	}
	raw := uconn.HandshakeState.Hello.Raw
	record := []byte{22, 0x03, 0x01, byte(len(raw) >> 8), byte(len(raw))}
	return append(record, raw...)
}

// The camouflage client config for the fixture extender, with both keys set.
func camouflageClientConfig(fixture *extenderFixture, identityPublicKey []byte, realityPublicKey []byte) *connect.ExtenderConfig {
	config := fixture.extenderConfig(connect.ExtenderCarrierTcp)
	config.PublicKey = identityPublicKey
	config.RealityPublicKey = realityPublicKey
	return config
}

// --- unit tests: the replay set and the hello peek (P2, P3) ---

// The seen-tag set refuses a replay and never grows past its cap or its window
// (P2): the first sight of a tag is not a replay, the second is, and an entry
// older than twice the window is pruned so an old tag is a fresh sight again.
func TestExtenderCamouflageReplaySetRefusesReplay(t *testing.T) {
	settings := DefaultExtenderSettings()
	settings.ExtenderCamouflageTimeWindow = time.Minute
	settings.ExtenderCamouflageReplayTagCount = 4
	replay := newExtenderCamouflageReplaySet(settings)

	now := time.Unix(1_700_000_000, 0)
	tag := "session-id-aaaaaaaaaaaaaaaaaaaa"
	if replay.seen(tag, now) {
		t.Fatal("the first sight of a tag was a replay")
	}
	if !replay.seen(tag, now) {
		t.Fatal("the second sight of a tag was not a replay")
	}
	// after twice the window the entry is pruned and the tag is fresh again
	later := now.Add(3 * time.Minute)
	if replay.seen(tag, later) {
		t.Fatal("an expired tag was still a replay")
	}
}

// The cap evicts the oldest tag, so a flood can at worst let one replay through
// as a fresh auth (P2), never grow the set without bound.
func TestExtenderCamouflageReplaySetEvictsOldestPastTheCap(t *testing.T) {
	settings := DefaultExtenderSettings()
	settings.ExtenderCamouflageTimeWindow = time.Hour
	settings.ExtenderCamouflageReplayTagCount = 2
	replay := newExtenderCamouflageReplaySet(settings)

	now := time.Unix(1_700_000_000, 0)
	replay.seen("tag-1", now)
	replay.seen("tag-2", now)
	// a third tag evicts the oldest (tag-1)
	replay.seen("tag-3", now)
	if replay.seen("tag-1", now) {
		t.Fatal("tag-1 was not evicted past the cap")
	}
	// tag-3 is still remembered
	if !replay.seen("tag-3", now) {
		t.Fatal("tag-3 was evicted though it was within the cap")
	}
}

// The peek reassembles a ClientHello split across several tls records without
// consuming the bytes it read (P3): the parsed sni and the replayed bytes both
// come back whole.
func TestExtenderCamouflagePeekReassemblesFragmentedHello(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()
	uconn := utls.UClient(clientConn, &utls.Config{ServerName: "front.example", InsecureSkipVerify: true}, utls.HelloChrome_133)
	defer uconn.Close()
	if err := uconn.BuildHandshakeState(); err != nil {
		t.Fatal(err)
	}
	raw := uconn.HandshakeState.Hello.Raw

	// split the handshake message across three tls records
	fragmentSizes := []int{40, 200, len(raw) - 240}
	var wire []byte
	offset := 0
	for _, size := range fragmentSizes {
		if size <= 0 {
			continue
		}
		fragment := raw[offset : offset+size]
		offset += size
		wire = append(wire, 22, 0x03, 0x01, byte(len(fragment)>>8), byte(len(fragment)))
		wire = append(wire, fragment...)
	}

	go func() {
		clientConn.Write(wire)
	}()
	readBytes, parsed, ok := peekClientHello(serverConn, 16*1024)
	if !ok {
		t.Fatal("the fragmented hello did not parse")
	}
	if parsed.ServerName != "front.example" {
		t.Fatalf("parsed sni = %q", parsed.ServerName)
	}
	if !bytes.Equal(readBytes, wire) {
		t.Fatal("the peek did not return every byte it read")
	}
}

// A first record that is not a handshake is not a parseable ClientHello (P3): the
// peek returns the bytes it read so the caller can replay them, and ok false.
func TestExtenderCamouflagePeekRejectsNonHandshake(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()
	// content type 23 is application data, not a handshake
	go func() {
		clientConn.Write([]byte{23, 0x03, 0x01, 0x00, 0x02, 0xaa, 0xbb})
	}()
	readBytes, parsed, ok := peekClientHello(serverConn, 16*1024)
	if ok || parsed != nil {
		t.Fatal("a non-handshake record parsed as a ClientHello")
	}
	if len(readBytes) == 0 {
		t.Fatal("the peek returned no read bytes to replay")
	}
}

// --- end-to-end: the authenticated path (P1, P3, P4) ---

// A sealed hello whose tag opens and whose time is in window takes the
// authenticated path, terminates with the identity leaf, and the client's B3
// check passes, so the dial reaches the destination (P10 auth ok).
func TestExtenderCamouflageAuthenticatedReachesDestination(t *testing.T) {
	seed, identityPublicKey, realityPublicKey := newCamouflageKeys(t)
	restore := connect.Testing_SetBorrowDomains([]string{testBorrowName})
	defer restore()
	fixture := newExtenderFixture(t, "127.0.0.1", func(settings *ExtenderSettings) {
		settings.CamouflageEnabled = true
		settings.IdentityKeySeed = seed
	})
	client := connect.NewExtenderHttpClient(
		fixture.connectSettings(),
		camouflageClientConfig(fixture, identityPublicKey, realityPublicKey),
	)
	defer client.CloseIdleConnections()

	response, err := client.Get("https://dest.example/hello")
	if err != nil {
		if extenderErr, ok := fixture.nextError(); ok {
			t.Fatalf("%v; extender: %v", err, extenderErr)
		}
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(body, []byte("dest.example")) {
		t.Fatalf("body = %q", body)
	}
	if count := fixture.server.CamouflageStats().AuthenticatedCount; count != 1 {
		t.Fatalf("authenticated count = %d, expected 1", count)
	}
}

// The authenticated connection negotiates http/1.1 though the client offered
// {h2,http/1.1}, and the choice is not on the wire (P10 ALPN): the peeked hello
// carries {h2,http/1.1}, the outcome is authenticated, and the request — which
// an h2 connection would get 403 — succeeds.
func TestExtenderCamouflageAuthenticatedNegotiatesHttp11(t *testing.T) {
	seed, identityPublicKey, realityPublicKey := newCamouflageKeys(t)
	restore := connect.Testing_SetBorrowDomains([]string{testBorrowName})
	defer restore()
	outcomes := &recordedValues[ExtenderCamouflageOutcome]{}
	fixture := newExtenderFixture(t, "127.0.0.1", func(settings *ExtenderSettings) {
		settings.CamouflageEnabled = true
		settings.IdentityKeySeed = seed
		settings.CamouflageHandler = func(outcome ExtenderCamouflageOutcome) {
			outcomes.add(outcome)
		}
	})
	client := connect.NewExtenderHttpClient(
		fixture.connectSettings(),
		camouflageClientConfig(fixture, identityPublicKey, realityPublicKey),
	)
	defer client.CloseIdleConnections()

	response, err := client.Get("https://dest.example/hello")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()

	var authenticated *ExtenderCamouflageOutcome
	for _, outcome := range outcomes.snapshot() {
		if outcome.Outcome == ExtenderCamouflageOutcomeAuthenticated {
			o := outcome
			authenticated = &o
		}
	}
	if authenticated == nil {
		t.Fatal("no hello was classified authenticated")
	}
	// the client offered both protocols on the wire
	if !containsString(authenticated.AlpnProtocols, "h2") || !containsString(authenticated.AlpnProtocols, "http/1.1") {
		t.Fatalf("client alpn = %v, expected it to offer {h2, http/1.1}", authenticated.AlpnProtocols)
	}
	// the server negotiated http/1.1 on the authenticated path, so the
	// camouflaged attempt carried the extender request itself and won outright:
	// the legacy attempt never launched and no hello was terminated. Were the
	// authenticated config to offer {h2,http/1.1}, the faithful hello would
	// negotiate h2, the extender request would 403, and the race would fall to
	// the legacy attempt, terminating a hello.
	stats := fixture.server.CamouflageStats()
	if stats.AuthenticatedCount != 1 {
		t.Fatalf("authenticated count = %d, expected 1", stats.AuthenticatedCount)
	}
	if stats.TerminatedCount != 0 {
		t.Fatalf("terminated count = %d, expected 0: the authenticated path did not negotiate http/1.1", stats.TerminatedCount)
	}
}

func containsString(values []string, value string) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}

// A resilient client that fragments the Chrome hello across records is
// reassembled by the peek and authenticated (P10 fragmented hello).
func TestExtenderCamouflageFragmentedHelloAuthenticated(t *testing.T) {
	seed, identityPublicKey, realityPublicKey := newCamouflageKeys(t)
	restore := connect.Testing_SetBorrowDomains([]string{testBorrowName})
	defer restore()
	fixture := newExtenderFixture(t, "127.0.0.1", func(settings *ExtenderSettings) {
		settings.CamouflageEnabled = true
		settings.IdentityKeySeed = seed
	})
	config := camouflageClientConfig(fixture, identityPublicKey, realityPublicKey)
	config.Profile.Fragment = true
	config.Profile.Reorder = true
	client := connect.NewExtenderHttpClient(fixture.connectSettings(), config)
	defer client.CloseIdleConnections()

	response, err := client.Get("https://dest.example/hello")
	if err != nil {
		if extenderErr, ok := fixture.nextError(); ok {
			t.Fatalf("%v; extender: %v", err, extenderErr)
		}
		t.Fatal(err)
	}
	response.Body.Close()
	if count := fixture.server.CamouflageStats().AuthenticatedCount; count != 1 {
		t.Fatalf("authenticated count = %d, expected 1", count)
	}
}

// --- end-to-end: skew, legacy and wrong key (P1, P2, P3, P8) ---

// A client whose sealed time is outside the window is not authenticated; in
// Phase A the race falls to the legacy attempt, which still reaches the extender
// (P10 skew fallback). The server clock is advanced past the window so the
// client's honest seal is out of bounds deterministically.
func TestExtenderCamouflageSkewedClientFallsToLegacyPhaseA(t *testing.T) {
	seed, identityPublicKey, realityPublicKey := newCamouflageKeys(t)
	restore := connect.Testing_SetBorrowDomains([]string{testBorrowName})
	defer restore()
	skewedNow := func() time.Time { return time.Now().Add(10 * time.Minute) }
	fixture := newExtenderFixture(t, "127.0.0.1", func(settings *ExtenderSettings) {
		settings.CamouflageEnabled = true
		settings.IdentityKeySeed = seed
		settings.ExtenderCamouflageTimeWindow = 2 * time.Minute
		settings.CamouflageNow = skewedNow
	})
	client := connect.NewExtenderHttpClient(
		fixture.connectSettings(),
		camouflageClientConfig(fixture, identityPublicKey, realityPublicKey),
	)
	defer client.CloseIdleConnections()

	response, err := client.Get("https://dest.example/hello")
	if err != nil {
		if extenderErr, ok := fixture.nextError(); ok {
			t.Fatalf("%v; extender: %v", err, extenderErr)
		}
		t.Fatal(err)
	}
	response.Body.Close()
	stats := fixture.server.CamouflageStats()
	if stats.TimeWindowFailedCount != 1 {
		t.Fatalf("time window failed count = %d, expected 1", stats.TimeWindowFailedCount)
	}
	if stats.AuthenticatedCount != 0 {
		t.Fatalf("authenticated count = %d, expected 0 for a skewed client", stats.AuthenticatedCount)
	}
}

// A skewed client in Phase B loses the tcp carrier: its camouflaged attempt is
// spliced and its legacy attempt is spliced, both failing the B3 check, so the
// tcp dial fails (P10 skew fallback, Phase B).
func TestExtenderCamouflageSkewedClientLosesTcpPhaseB(t *testing.T) {
	seed, identityPublicKey, realityPublicKey := newCamouflageKeys(t)
	restore := connect.Testing_SetBorrowDomains([]string{testBorrowName})
	defer restore()
	site := newFakeBorrowSite(t, tlsBorrowSiteHandler(t))
	skewedNow := func() time.Time { return time.Now().Add(10 * time.Minute) }
	fixture := newExtenderFixture(t, "127.0.0.1", func(settings *ExtenderSettings) {
		settings.CamouflageEnabled = true
		settings.IdentityKeySeed = seed
		settings.ExtenderCamouflageTimeWindow = 2 * time.Minute
		settings.ExtenderCamouflageSplice = true
		settings.CamouflageBorrowNames = []string{testBorrowName}
		settings.CamouflageSpliceDialContext = site.dialContext()
		settings.CamouflageNow = skewedNow
	})
	config := camouflageClientConfig(fixture, identityPublicKey, realityPublicKey)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	conn, _, err := connect.DialExtender(
		ctx,
		fixture.connectSettings(),
		config,
		&connect.ExtenderDial{DestinationHost: "dest.example", DestinationPort: 443},
	)
	if conn != nil {
		conn.Close()
	}
	if err == nil {
		t.Fatal("the skewed client reached the extender over the spliced tcp carrier")
	}
	if site.reached.get() == 0 {
		t.Fatal("the unauthenticated hello was not spliced to the borrowed site")
	}
	stats := fixture.server.CamouflageStats()
	if stats.TimeWindowFailedCount != 1 {
		t.Fatalf("time window failed count = %d, expected 1", stats.TimeWindowFailedCount)
	}
	if stats.SplicedCount == 0 {
		t.Fatal("no connection was spliced")
	}
}

// A legacy client (no camouflage key) is terminated and served in Phase A (P10
// legacy client): the server issues the identity leaf and the B3 check passes.
func TestExtenderCamouflageLegacyClientPhaseA(t *testing.T) {
	seed, identityPublicKey, _ := newCamouflageKeys(t)
	fixture := newExtenderFixture(t, "127.0.0.1", func(settings *ExtenderSettings) {
		settings.CamouflageEnabled = true
		settings.IdentityKeySeed = seed
	})
	// a legacy client: it knows the identity key for the B3 check but carries no
	// camouflage key, so the tcp dial is the legacy Go-TLS attempt alone
	config := fixture.extenderConfig(connect.ExtenderCarrierTcp)
	config.PublicKey = identityPublicKey
	client := connect.NewExtenderHttpClient(fixture.connectSettings(), config)
	defer client.CloseIdleConnections()

	response, err := client.Get("https://dest.example/hello")
	if err != nil {
		if extenderErr, ok := fixture.nextError(); ok {
			t.Fatalf("%v; extender: %v", err, extenderErr)
		}
		t.Fatal(err)
	}
	response.Body.Close()
	if count := fixture.server.CamouflageStats().TerminatedCount; count == 0 {
		t.Fatal("the legacy client was not terminated")
	}
}

// A legacy client in Phase B is spliced to the real borrowed site, so its dial
// fails the B3 check (P10 legacy client, Phase B).
func TestExtenderCamouflageLegacyClientPhaseB(t *testing.T) {
	seed, identityPublicKey, _ := newCamouflageKeys(t)
	site := newFakeBorrowSite(t, tlsBorrowSiteHandler(t))
	fixture := newExtenderFixture(t, "127.0.0.1", func(settings *ExtenderSettings) {
		settings.CamouflageEnabled = true
		settings.IdentityKeySeed = seed
		settings.ExtenderCamouflageSplice = true
		settings.CamouflageBorrowNames = []string{testBorrowName}
		settings.CamouflageSpliceDialContext = site.dialContext()
	})
	config := fixture.extenderConfig(connect.ExtenderCarrierTcp)
	config.PublicKey = identityPublicKey
	// front the legacy Go-TLS hello with a borrowed sni so the splice targets it
	config.Profile.ServerName = testBorrowName
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	conn, _, err := connect.DialExtender(
		ctx,
		fixture.connectSettings(),
		config,
		&connect.ExtenderDial{DestinationHost: "dest.example", DestinationPort: 443},
	)
	if conn != nil {
		conn.Close()
	}
	if err == nil {
		t.Fatal("the legacy client reached the extender though its hello was spliced")
	}
	if site.reached.get() == 0 {
		t.Fatal("the legacy hello was not spliced to the borrowed site")
	}
	if count := fixture.server.CamouflageStats().SplicedCount; count == 0 {
		t.Fatal("no connection was spliced")
	}
}

// A tag sealed against a different static key fails the open and is spliced in
// Phase B (P10 wrong key): the client holds the right identity key for the B3
// check but the wrong reality key, so its tag never opens and it is treated as a
// prober.
func TestExtenderCamouflageWrongKeySplicedPhaseB(t *testing.T) {
	seed, identityPublicKey, _ := newCamouflageKeys(t)
	_, _, otherRealityPublicKey := newCamouflageKeys(t)
	restore := connect.Testing_SetBorrowDomains([]string{testBorrowName})
	defer restore()
	site := newFakeBorrowSite(t, tlsBorrowSiteHandler(t))
	fixture := newExtenderFixture(t, "127.0.0.1", func(settings *ExtenderSettings) {
		settings.CamouflageEnabled = true
		settings.IdentityKeySeed = seed
		settings.ExtenderCamouflageSplice = true
		settings.CamouflageBorrowNames = []string{testBorrowName}
		settings.CamouflageSpliceDialContext = site.dialContext()
	})
	config := camouflageClientConfig(fixture, identityPublicKey, otherRealityPublicKey)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	conn, _, err := connect.DialExtender(
		ctx,
		fixture.connectSettings(),
		config,
		&connect.ExtenderDial{DestinationHost: "dest.example", DestinationPort: 443},
	)
	if conn != nil {
		conn.Close()
	}
	if err == nil {
		t.Fatal("a wrong-key client reached the extender though its tag never opened")
	}
	if site.reached.get() == 0 {
		t.Fatal("the wrong-key hello was not spliced to the borrowed site")
	}
	stats := fixture.server.CamouflageStats()
	if stats.AuthenticatedCount != 0 {
		t.Fatalf("authenticated count = %d, expected 0 for a wrong key", stats.AuthenticatedCount)
	}
	if stats.SplicedCount == 0 {
		t.Fatal("no connection was spliced")
	}
}

// --- splice bounds (P3, P10 bounded splice) ---

// The per-connection relayed-byte bound cuts the splice (P10 bounded splice,
// byte): a flooding site sends far more than the bound, and the prober receives
// at most the bound.
func TestExtenderCamouflageSpliceByteBound(t *testing.T) {
	maxByteCount := 10000
	floodByteCount := 20 * maxByteCount
	site := newFakeBorrowSite(t, floodBorrowSiteHandler(floodByteCount))
	fixture := newExtenderFixture(t, "127.0.0.1", func(settings *ExtenderSettings) {
		settings.CamouflageEnabled = true
		settings.IdentityKeySeed = mustSeed(t)
		settings.ExtenderCamouflageSplice = true
		settings.CamouflageBorrowNames = []string{testBorrowName}
		settings.CamouflageSpliceDialContext = site.dialContext()
		settings.ProxyMaxResponseByteCount = int64(maxByteCount)
	})

	received := spliceProberReceiveByteCount(t, fixture)
	if received == 0 {
		t.Fatal("the prober received nothing; the hello was not spliced")
	}
	if received > maxByteCount {
		t.Fatalf("the prober received %d bytes, past the %d byte bound", received, maxByteCount)
	}
}

// A spliced byte is never counted as relay traffic (P10 bounded splice, O1):
// after a splice the O1 ingress and egress counters are still zero.
func TestExtenderCamouflageSpliceNotCountedAsRelay(t *testing.T) {
	site := newFakeBorrowSite(t, floodBorrowSiteHandler(4096))
	fixture := newExtenderFixture(t, "127.0.0.1", func(settings *ExtenderSettings) {
		settings.CamouflageEnabled = true
		settings.IdentityKeySeed = mustSeed(t)
		settings.ExtenderCamouflageSplice = true
		settings.CamouflageBorrowNames = []string{testBorrowName}
		settings.CamouflageSpliceDialContext = site.dialContext()
	})

	spliceProberReceiveByteCount(t, fixture)
	stats := fixture.server.Stats()
	if stats.IngressByteCount != 0 || stats.EgressByteCount != 0 {
		t.Fatalf("spliced bytes were counted as relay traffic: %+v", stats)
	}
	if fixture.server.CamouflageStats().SplicedCount == 0 {
		t.Fatal("no connection was spliced")
	}
}

// The total concurrent-splice cap refuses a splice past the bound (P10 bounded
// splice, total): two held splices fill the cap, and a third hello is not
// spliced, so the borrowed site is never dialed a third time.
func TestExtenderCamouflageSpliceTotalCap(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	site := newFakeBorrowSite(t, blockingBorrowSiteHandler(release))
	terminated := make(chan struct{}, 4)
	fixture := newExtenderFixture(t, "127.0.0.1", func(settings *ExtenderSettings) {
		settings.CamouflageEnabled = true
		settings.IdentityKeySeed = mustSeed(t)
		settings.ExtenderCamouflageSplice = true
		settings.CamouflageBorrowNames = []string{testBorrowName}
		settings.CamouflageSpliceDialContext = site.dialContext()
		settings.ExtenderCamouflageSpliceMaxCount = 2
		settings.CamouflageHandler = func(outcome ExtenderCamouflageOutcome) {
			if outcome.Outcome == ExtenderCamouflageOutcomeTerminated {
				terminated <- struct{}{}
			}
		}
	})

	// two probers fill the cap; each splice holds open on the blocking site
	for i := 0; i < 2; i += 1 {
		openSpliceProber(t, fixture)
		select {
		case <-site.accepted:
		case <-time.After(10 * time.Second):
			t.Fatalf("splice %d was not started", i)
		}
	}
	// the third prober is over the cap: it is terminated, not spliced, so the
	// borrowed site is never dialed a third time
	openSpliceProber(t, fixture)
	select {
	case <-terminated:
	case <-site.accepted:
		t.Fatal("the over-cap hello was spliced past the total cap")
	case <-time.After(10 * time.Second):
		t.Fatal("the third hello was neither spliced nor terminated")
	}
	if reached := site.reached.get(); reached != 2 {
		t.Fatalf("the borrowed site was dialed %d times, expected 2 (the cap)", reached)
	}
	if count := fixture.server.CamouflageStats().SplicedCount; count != 2 {
		t.Fatalf("spliced count = %d, expected 2 (the cap)", count)
	}
}

// The per-target cap refuses a splice to one borrowed site past the bound (P10
// bounded splice, per-target): one held splice fills the per-target cap, and a
// second hello to the same target is not spliced.
func TestExtenderCamouflageSplicePerTargetCap(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	site := newFakeBorrowSite(t, blockingBorrowSiteHandler(release))
	terminated := make(chan struct{}, 4)
	fixture := newExtenderFixture(t, "127.0.0.1", func(settings *ExtenderSettings) {
		settings.CamouflageEnabled = true
		settings.IdentityKeySeed = mustSeed(t)
		settings.ExtenderCamouflageSplice = true
		settings.CamouflageBorrowNames = []string{testBorrowName}
		settings.CamouflageSpliceDialContext = site.dialContext()
		settings.ExtenderCamouflageSpliceMaxPerTarget = 1
		settings.CamouflageHandler = func(outcome ExtenderCamouflageOutcome) {
			if outcome.Outcome == ExtenderCamouflageOutcomeTerminated {
				terminated <- struct{}{}
			}
		}
	})

	openSpliceProber(t, fixture)
	select {
	case <-site.accepted:
	case <-time.After(10 * time.Second):
		t.Fatal("the first splice was not started")
	}
	openSpliceProber(t, fixture)
	select {
	case <-terminated:
	case <-site.accepted:
		t.Fatal("the over-cap hello was spliced past the per-target cap")
	case <-time.After(10 * time.Second):
		t.Fatal("the second hello was neither spliced nor terminated")
	}
	if reached := site.reached.get(); reached != 1 {
		t.Fatalf("the borrowed site was dialed %d times, expected 1 (the per-target cap)", reached)
	}
}

// Opens a raw prober that sends an unauthenticated hello fronting the borrowed
// name and leaves the connection open, for the cap tests.
func openSpliceProber(t *testing.T, fixture *extenderFixture) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", fixture.authority(fixture.tcpPort))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if _, err := conn.Write(rawUnauthenticatedHelloRecord(t, testBorrowName)); err != nil {
		t.Fatal(err)
	}
	return conn
}

// Splices one raw prober and returns how many relayed bytes it received before
// the connection ended.
func spliceProberReceiveByteCount(t *testing.T, fixture *extenderFixture) int {
	t.Helper()
	conn, err := net.Dial("tcp", fixture.authority(fixture.tcpPort))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write(rawUnauthenticatedHelloRecord(t, testBorrowName)); err != nil {
		t.Fatal(err)
	}
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	received := 0
	buffer := make([]byte, 4096)
	for {
		n, err := conn.Read(buffer)
		received += n
		if err != nil {
			return received
		}
	}
}

// mustSeed is a fresh identity seed for a splice test that does not need the
// public keys.
func mustSeed(t *testing.T) []byte {
	t.Helper()
	seed, err := connect.NewExtenderKeySeed()
	if err != nil {
		t.Fatal(err)
	}
	return seed
}
