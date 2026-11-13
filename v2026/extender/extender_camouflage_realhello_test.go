package extender

// Real-browser conformance for the camouflage server paths (EXTENDER-UPDATE2
// §3.4): the committed real Chrome ClientHello (fingerprint.GoldenChrome156Real)
// is fed through the extender's actual peek, authentication and splice paths.
// It proves the server handles a genuine browser's first flight — not only
// uTLS-generated hellos — and that a real browser's honest visit takes the
// prober path (spliced to the real site in Phase B, terminated in Phase A),
// which is the REALITY property the splice depends on.
//
// Deterministic and offline: the hello is a committed fixture, the splice
// target is the in-process fake borrowed site, and no real browser or network
// runs here. The fixture itself is refreshed by the Layer-B -local-browser
// capture (fingerprint/README.md).

import (
	"net"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026/fingerprint"
)

// The committed real-browser ClientHello wrapped in a single TLS handshake
// record, as a raw prober puts it on the wire.
func realBrowserHelloRecord(t *testing.T) []byte {
	t.Helper()
	golden, err := fingerprint.LoadGolden(fingerprint.GoldenChrome156Real)
	if err != nil {
		t.Fatal(err)
	}
	raw := golden.Raw
	if len(raw) == 0 || 0xffff < len(raw) {
		t.Fatalf("real hello is %d bytes, not a single record", len(raw))
	}
	record := []byte{22, 0x03, 0x01, byte(len(raw) >> 8), byte(len(raw))}
	return append(record, raw...)
}

// The server's peek reads a real browser hello whole, and the camouflage's
// key-share and session-id machinery handle it: a 32-byte session id (the field
// the tag would seal into) and an X25519/MLKEM ephemeral the ECDH would read.
// With its genuine random session id — no valid tag — authenticate returns
// false, so a real browser visit is classified unauthenticated and takes the
// prober path (§3.4).
func TestExtenderCamouflageRealBrowserHelloPeekAndProbeClassification(t *testing.T) {
	record := realBrowserHelloRecord(t)
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()
	go func() { clientConn.Write(record) }()

	readBytes, parsed, ok := peekClientHello(serverConn, 16*1024)
	if !ok {
		t.Fatal("the server could not peek a real browser hello")
	}
	if len(readBytes) != len(record) {
		t.Fatalf("peek read %d bytes, want the whole %d-byte record", len(readBytes), len(record))
	}
	if len(parsed.SessionId) != 32 {
		t.Fatalf("real browser hello session id = %d bytes, want 32", len(parsed.SessionId))
	}

	settings := DefaultExtenderSettings()
	settings.CamouflageEnabled = true
	settings.IdentityKeySeed = mustSeed(t)
	server := &ExtenderServer{settings: settings}
	camouflage, err := newExtenderCamouflage(server, settings)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := camouflage.preferredEphemeral(parsed); !ok {
		t.Fatal("the camouflage found no X25519 ephemeral in a real browser hello")
	}
	// a real browser's own random session id is not a valid tag, so it does not
	// authenticate — it is a prober to the extender
	if camouflage.authenticate(parsed) {
		t.Fatal("a real browser hello authenticated though it carries no tag")
	}
}

// In Phase B a real browser's honest hello is spliced to the real borrowed site
// (§3.4): a prober — or a genuine visitor — sees the real site, not a tell.
func TestExtenderCamouflageRealBrowserHelloSplicedPhaseB(t *testing.T) {
	site := newFakeBorrowSite(t, tlsBorrowSiteHandler(t))
	fixture := newExtenderFixture(t, "127.0.0.1", func(settings *ExtenderSettings) {
		settings.CamouflageEnabled = true
		settings.IdentityKeySeed = mustSeed(t)
		settings.ExtenderCamouflageSplice = true
		settings.CamouflageBorrowNames = []string{testBorrowName}
		settings.CamouflageSpliceDialContext = site.dialContext()
	})
	conn, err := net.Dial("tcp", fixture.authority(fixture.tcpPort))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write(realBrowserHelloRecord(t)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-site.accepted:
	case <-time.After(10 * time.Second):
		t.Fatal("the real browser hello was not spliced to the borrowed site")
	}
	// the site accepts during the splice's dial, and the splice counts itself
	// only once that dial returns, so the count trails the accept
	deadline := time.Now().Add(10 * time.Second)
	for fixture.server.CamouflageStats().SplicedCount == 0 {
		if deadline.Before(time.Now()) {
			t.Fatal("spliced count did not increment for a real browser hello")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// In Phase A a real browser hello is terminated as today (§3.4): splice is off,
// so the server serves it the self-signed leaf, the legacy posture.
func TestExtenderCamouflageRealBrowserHelloTerminatedPhaseA(t *testing.T) {
	terminated := make(chan ExtenderCamouflageOutcome, 4)
	fixture := newExtenderFixture(t, "127.0.0.1", func(settings *ExtenderSettings) {
		settings.CamouflageEnabled = true
		settings.IdentityKeySeed = mustSeed(t)
		settings.CamouflageHandler = func(outcome ExtenderCamouflageOutcome) {
			if outcome.Outcome == ExtenderCamouflageOutcomeTerminated {
				terminated <- outcome
			}
		}
	})
	conn, err := net.Dial("tcp", fixture.authority(fixture.tcpPort))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write(realBrowserHelloRecord(t)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-terminated:
	case <-time.After(10 * time.Second):
		t.Fatal("the real browser hello was not terminated in Phase A")
	}
	if count := fixture.server.CamouflageStats().TerminatedCount; count == 0 {
		t.Fatal("terminated count did not increment for a real browser hello")
	}
}
