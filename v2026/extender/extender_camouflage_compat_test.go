package extender

// Client compatibility tests for the splice rollout (EXTENDER-UPDATE2 §3.2,
// §3.4): a compatible client succeeds in both phases, a kill-switched client
// keeps a Phase A path, the splice follows the requested SNI, and a replayed
// authenticated hello is refused and spliced. These complement the existing
// Phase A/B tests (missing-key legacy, skew, wrong-key) retained in
// extender_camouflage_test.go.

import (
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

// A compatible (authenticated) client reaches the destination in Phase B too:
// splice is on, but the authenticated hello is recognized and terminated with
// the identity leaf, never spliced, so the B3 check passes (§3.2 compatible
// clients succeed in both phases).
func TestExtenderCamouflageAuthenticatedReachesDestinationPhaseB(t *testing.T) {
	seed, identityPublicKey, realityPublicKey := newCamouflageKeys(t)
	restore := connect.Testing_SetBorrowDomains([]string{testBorrowName})
	defer restore()
	defer connect.Testing_HoldExtenderTcpCamouflageStagger()()
	site := newFakeBorrowSite(t, tlsBorrowSiteHandler(t))
	fixture := newExtenderFixture(t, "127.0.0.1", func(settings *ExtenderSettings) {
		settings.CamouflageEnabled = true
		settings.IdentityKeySeed = seed
		settings.ExtenderCamouflageSplice = true // Phase B
		settings.CamouflageBorrowNames = []string{testBorrowName}
		settings.CamouflageSpliceDialContext = site.dialContext()
	})
	client := connect.NewExtenderHttpClient(
		camouflageSlowFirstDialSettings(fixture),
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
	if stats.AuthenticatedCount != 1 {
		t.Fatalf("authenticated count = %d, expected 1 in Phase B", stats.AuthenticatedCount)
	}
	if stats.SplicedCount != 0 {
		t.Fatalf("spliced count = %d, expected 0: an authenticated client must not be spliced", stats.SplicedCount)
	}
	if site.reached.get() != 0 {
		t.Fatal("an authenticated client was spliced to the borrowed site in Phase B")
	}
}

// A kill-switched client (TlsClientHelloFingerprint "go") keeps a Phase A path:
// it dials the legacy Go-TLS attempt alone, which an extender in Phase A
// terminates and serves, so the client still reaches the destination (§3.2
// supported legacy clients retain a Phase A path).
func TestExtenderCamouflageKillSwitchClientPhaseA(t *testing.T) {
	seed, identityPublicKey, realityPublicKey := newCamouflageKeys(t)
	restore := connect.Testing_SetBorrowDomains([]string{testBorrowName})
	defer restore()
	fixture := newExtenderFixture(t, "127.0.0.1", func(settings *ExtenderSettings) {
		settings.CamouflageEnabled = true
		settings.IdentityKeySeed = seed
		// Phase A
	})
	connectSettings := fixture.connectSettings()
	connectSettings.TlsClientHelloFingerprint = connect.TlsClientHelloFingerprintGo
	client := connect.NewExtenderHttpClient(
		connectSettings,
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
	// the kill switch means no authenticated hello was sent; the legacy dial
	// was terminated and served
	if count := fixture.server.CamouflageStats().AuthenticatedCount; count != 0 {
		t.Fatalf("authenticated count = %d, expected 0 under the go kill switch", count)
	}
	if count := fixture.server.CamouflageStats().TerminatedCount; count == 0 {
		t.Fatal("the kill-switched client was not terminated and served in Phase A")
	}
}

// The splice follows the requested SNI: a prober fronting one verified borrowed
// name is spliced to THAT target, not another (§3.4 spliced handshakes checked
// against the requested SNI).
func TestExtenderCamouflageSpliceFollowsRequestedSni(t *testing.T) {
	const otherBorrowName = "other.example"
	reachedNames := &recordedValues[string]{}
	var reachedLock sync.Mutex
	dialByName := func(ctx context.Context, network string, address string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		reachedLock.Lock()
		reachedNames.add(host)
		reachedLock.Unlock()
		// both names resolve to one in-process site that drains
		clientConn, serverConn := net.Pipe()
		go func() { io.Copy(io.Discard, serverConn); serverConn.Close() }()
		return clientConn, nil
	}
	fixture := newExtenderFixture(t, "127.0.0.1", func(settings *ExtenderSettings) {
		settings.CamouflageEnabled = true
		settings.IdentityKeySeed = mustSeed(t)
		settings.ExtenderCamouflageSplice = true
		settings.CamouflageBorrowNames = []string{testBorrowName, otherBorrowName}
		settings.CamouflageSpliceDialContext = dialByName
	})
	// a prober fronting otherBorrowName must be spliced to other.example first
	conn, err := net.Dial("tcp", fixture.authority(fixture.tcpPort))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write(rawUnauthenticatedHelloRecord(t, otherBorrowName)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if names := reachedNames.snapshot(); len(names) != 0 {
			if names[0] != otherBorrowName {
				t.Fatalf("splice went to %q first, expected the requested sni %q", names[0], otherBorrowName)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the splice did not dial any target")
}

// A recording dialer that tees the first TLS record (the ClientHello) the
// camouflaged attempt writes, so a test can replay it verbatim.
type helloRecordingConn struct {
	net.Conn
	captured *[]byte
	done     *bool
	lock     *sync.Mutex
}

func (self *helloRecordingConn) Write(b []byte) (int, error) {
	self.lock.Lock()
	if !*self.done {
		*self.captured = append(*self.captured, b...)
		// the first handshake record is complete once header + length is read
		if 5 <= len(*self.captured) {
			recordLen := int((*self.captured)[3])<<8 | int((*self.captured)[4])
			if 5+recordLen <= len(*self.captured) {
				*self.captured = (*self.captured)[:5+recordLen]
				*self.done = true
			}
		}
	}
	self.lock.Unlock()
	return self.Conn.Write(b)
}

// A replayed authenticated hello is refused and spliced (§3.4 replayed hellos
// take their expected path): the first authenticated dial succeeds and the tag
// is remembered; resending the exact same ClientHello is detected as a replay,
// not authenticated, and handled as a prober — spliced in Phase B — with the
// replay counter incremented.
func TestExtenderCamouflageReplayedHelloRefusedAndSpliced(t *testing.T) {
	seed, identityPublicKey, realityPublicKey := newCamouflageKeys(t)
	restore := connect.Testing_SetBorrowDomains([]string{testBorrowName})
	defer restore()
	// the hello captured and the one splice awaited are the authenticated attempt's
	// and the replay's, so no legacy attempt can send one ahead of them
	defer connect.Testing_HoldExtenderTcpCamouflageStagger()()
	site := newFakeBorrowSite(t, tlsBorrowSiteHandler(t))
	fixture := newExtenderFixture(t, "127.0.0.1", func(settings *ExtenderSettings) {
		settings.CamouflageEnabled = true
		settings.IdentityKeySeed = seed
		settings.ExtenderCamouflageSplice = true
		settings.CamouflageBorrowNames = []string{testBorrowName}
		settings.CamouflageSpliceDialContext = site.dialContext()
	})

	var captured []byte
	done := false
	var lock sync.Mutex
	connectSettings := fixture.connectSettings()
	connectSettings.DialContextSettings = &connect.DialContextSettings{
		DialContext: func(ctx context.Context, network string, address string) (net.Conn, error) {
			conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
			if err != nil {
				return nil, err
			}
			return &helloRecordingConn{Conn: conn, captured: &captured, done: &done, lock: &lock}, nil
		},
	}

	client := connect.NewExtenderHttpClient(
		connectSettings,
		camouflageClientConfig(fixture, identityPublicKey, realityPublicKey),
	)
	response, err := client.Get("https://dest.example/hello")
	if err != nil {
		client.CloseIdleConnections()
		if extenderErr, ok := fixture.nextError(); ok {
			t.Fatalf("%v; extender: %v", err, extenderErr)
		}
		t.Fatal(err)
	}
	response.Body.Close()
	client.CloseIdleConnections()
	if fixture.server.CamouflageStats().AuthenticatedCount != 1 {
		t.Fatal("the first authenticated dial did not authenticate")
	}

	lock.Lock()
	helloRecord := append([]byte(nil), captured...)
	complete := done
	lock.Unlock()
	if !complete || len(helloRecord) < 6 {
		t.Fatalf("did not capture the authenticated ClientHello record (%d bytes, complete=%t)", len(helloRecord), complete)
	}

	// replay the exact authenticated hello as a raw prober
	replayConn, err := net.Dial("tcp", fixture.authority(fixture.tcpPort))
	if err != nil {
		t.Fatal(err)
	}
	defer replayConn.Close()
	if _, err := replayConn.Write(helloRecord); err != nil {
		t.Fatal(err)
	}
	select {
	case <-site.accepted:
	case <-time.After(10 * time.Second):
		t.Fatal("the replayed authenticated hello was not spliced")
	}
	if count := fixture.server.CamouflageStats().ReplayRefusedCount; count == 0 {
		t.Fatal("the replay counter did not increment for a replayed authenticated hello")
	}
}
