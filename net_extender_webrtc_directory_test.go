package connect

import (
	"crypto/ed25519"
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Tests of the webrtc carrier in the directory and the strategy (EXTENDER.md
// S): a record that lists the carrier with its rendezvous id, the directory's
// gate on it, and the strategy expanding a dialer for it.

// A record whose addresses may list the webrtc carrier, with the rendezvous
// id the carrier needs; a zero id leaves the field empty.
func signTestWebRtcRecord(
	t *testing.T,
	rootPrivateKey ed25519.PrivateKey,
	extenderPublicKey ed25519.PublicKey,
	issueTime time.Time,
	expireTime time.Time,
	webRtcClientId Id,
	addresses ...*protocol.ExtenderAddress,
) *protocol.ExtenderRecord {
	t.Helper()
	body := &protocol.ExtenderRecordBody{
		PublicKey:    extenderPublicKey,
		Addresses:    addresses,
		TcpPort:      443,
		UdpPort:      443,
		DnsPort:      53,
		DnsTld:       "x.example.",
		CountryCode:  "us",
		IssueTimeMs:  uint64(issueTime.UnixMilli()),
		ExpireTimeMs: uint64(expireTime.UnixMilli()),
		NetworkHost:  testExtenderNetworkHost,
	}
	if webRtcClientId != (Id{}) {
		body.WebRtcClientId = webRtcClientId.Bytes()
	}
	record, err := SignExtenderRecord(rootPrivateKey, body)
	if err != nil {
		t.Fatal(err)
	}
	return record
}

// Root cause: a webrtc dial without a signaling path can only fail, and a
// failure counts against the address's other carriers, so the carrier must
// not reach a candidate until the owner has signaling. Observable: the
// candidate lists the carrier only once the directory is enabled, while the
// record's rendezvous id is readable either way.
func TestExtenderDirectoryHidesTheWebRtcCarrierUntilEnabled(t *testing.T) {
	clock := newTestClock()
	_, directory, rootPrivateKey := newTestExtenderStrategy(t, clock, nil)
	extenderPublicKey := newTestExtenderKey(t)
	webRtcClientId := NewId()
	record := signTestWebRtcRecord(
		t,
		rootPrivateKey,
		extenderPublicKey,
		clock.Now(),
		clock.Now().Add(14*24*time.Hour),
		webRtcClientId,
		testExtenderAddress("192.0.2.100", ExtenderCarrierTcp, ExtenderCarrierWebRtc),
	)
	if _, err := directory.ApplyRecord(record, ExtenderSourceFeed); err != nil {
		t.Fatal(err)
	}

	candidates := directory.Candidates(0, 10)
	if len(candidates) != 1 {
		t.Fatalf("candidates = %d, want 1", len(candidates))
	}
	if !slices.Equal(candidates[0].Carriers, []string{ExtenderCarrierTcp}) {
		t.Fatalf("carriers while disabled = %v, want tcp only", candidates[0].Carriers)
	}
	if id, ok := directory.WebRtcClientId(extenderPublicKey); !ok || id != webRtcClientId {
		t.Fatalf("rendezvous id = %s, %t, want the record's", id, ok)
	}
	if directory.WebRtcCarrierEnabled() {
		t.Fatalf("the carrier is enabled by default")
	}

	version, _ := directory.ChangeMonitor().Get()
	directory.SetWebRtcCarrierEnabled(true)
	if nextVersion, _ := directory.ChangeMonitor().Get(); nextVersion == version {
		t.Fatalf("enabling the carrier did not announce a change")
	}
	candidates = directory.Candidates(0, 10)
	if len(candidates) != 1 {
		t.Fatalf("candidates = %d, want 1", len(candidates))
	}
	if !slices.Equal(candidates[0].Carriers, []string{ExtenderCarrierTcp, ExtenderCarrierWebRtc}) {
		t.Fatalf("carriers while enabled = %v, want tcp and webrtc", candidates[0].Carriers)
	}
	if candidates[0].WebRtcClientId != webRtcClientId {
		t.Fatalf("candidate rendezvous id = %s, want %s", candidates[0].WebRtcClientId, webRtcClientId)
	}

	// enabling again announces nothing, and disabling hides it again
	version, _ = directory.ChangeMonitor().Get()
	directory.SetWebRtcCarrierEnabled(true)
	if nextVersion, _ := directory.ChangeMonitor().Get(); nextVersion != version {
		t.Fatalf("an unchanged enable announced a change")
	}
	directory.SetWebRtcCarrierEnabled(false)
	if candidates = directory.Candidates(0, 10); !slices.Equal(candidates[0].Carriers, []string{ExtenderCarrierTcp}) {
		t.Fatalf("carriers after disabling = %v, want tcp only", candidates[0].Carriers)
	}
}

// A record that lists the carrier without a rendezvous id gives the dial
// nothing to signal to, so the carrier is dropped from its candidate and the
// lookup says so.
func TestExtenderDirectoryDropsTheWebRtcCarrierWithoutARendezvousId(t *testing.T) {
	clock := newTestClock()
	_, directory, rootPrivateKey := newTestExtenderStrategy(t, clock, nil)
	directory.SetWebRtcCarrierEnabled(true)
	extenderPublicKey := newTestExtenderKey(t)
	record := signTestWebRtcRecord(
		t,
		rootPrivateKey,
		extenderPublicKey,
		clock.Now(),
		clock.Now().Add(14*24*time.Hour),
		Id{},
		testExtenderAddress("192.0.2.101", ExtenderCarrierTcp, ExtenderCarrierWebRtc),
	)
	if _, err := directory.ApplyRecord(record, ExtenderSourceFeed); err != nil {
		t.Fatal(err)
	}
	candidates := directory.Candidates(0, 10)
	if len(candidates) != 1 || !slices.Equal(candidates[0].Carriers, []string{ExtenderCarrierTcp}) {
		t.Fatalf("candidates = %+v, want tcp only", candidates)
	}
	if _, ok := directory.WebRtcClientId(extenderPublicKey); ok {
		t.Fatalf("a record without a rendezvous id answered one")
	}
	if _, ok := directory.WebRtcClientId(newTestExtenderKey(t)); ok {
		t.Fatalf("an unknown key answered a rendezvous id")
	}
}

// Root cause: a carrier the strategy does not expand a dialer for is never
// tried, however many records list it. Observable: a webrtc-only candidate
// expands to exactly one dialer in the webrtc connect mode carrying the
// record key, and to none while the directory has the carrier disabled.
func TestClientStrategyExpandsAWebRtcDialerForAWebRtcCandidate(t *testing.T) {
	clock := newTestClock()
	for _, enabled := range []bool{true, false} {
		clientStrategy, directory, rootPrivateKey := newTestExtenderStrategy(t, clock, nil)
		directory.SetWebRtcCarrierEnabled(enabled)
		extenderPublicKey := newTestExtenderKey(t)
		record := signTestWebRtcRecord(
			t,
			rootPrivateKey,
			extenderPublicKey,
			clock.Now(),
			clock.Now().Add(14*24*time.Hour),
			NewId(),
			testExtenderAddress("192.0.2.102", ExtenderCarrierWebRtc),
		)
		if _, err := directory.ApplyRecord(record, ExtenderSourceFeed); err != nil {
			t.Fatal(err)
		}
		expandedDialers := clientStrategy.expandExtenderDialers()
		if !enabled {
			if len(expandedDialers) != 0 {
				t.Fatalf("disabled: dialers = %d, want none", len(expandedDialers))
			}
			continue
		}
		if len(expandedDialers) != 1 {
			t.Fatalf("enabled: dialers = %d, want one", len(expandedDialers))
		}
		extenderConfig := expandedDialers[0].extenderConfig
		if extenderConfig.Profile.ConnectMode != ExtenderConnectModeWebRtc {
			t.Fatalf("dialer mode = %s, want webrtc", extenderConfig.Profile.ConnectMode)
		}
		if string(extenderConfig.PublicKey) != string(extenderPublicKey) {
			t.Fatalf("the dialer does not carry the record key")
		}
		if expandedDialers[0].description != "extender webrtc" {
			t.Fatalf("dialer description = %q", expandedDialers[0].description)
		}
	}
}

// The hold of one address, read under the directory's lock.
func testExtenderAddressHold(directory *ExtenderDirectory, ip netip.Addr) (time.Time, int) {
	directory.stateLock.Lock()
	defer directory.stateLock.Unlock()
	address := directory.ipAddresses[ip]
	if address == nil {
		return time.Time{}, -1
	}
	return address.holdUntilTime, address.consecutiveFailureCount
}

// Root cause: a hold is of the whole address, so a webrtc join this client
// could not make -- the nats between the two ends, its own signaling down --
// held an extender's working tcp, quic and dns too (S8, S9). Observable: a
// failed webrtc dial holds nothing of an address that lists a socket carrier,
// a failed tcp dial of the same address holds it, and a failed webrtc dial of
// an address that lists webrtc alone holds it, since webrtc is all it serves.
func TestExtenderDirectoryHoldsAWebRtcFailureOnlyAgainstAWebRtcOnlyAddress(t *testing.T) {
	clock := newTestClock()
	_, directory, rootPrivateKey := newTestExtenderStrategy(t, clock, nil)
	directory.SetWebRtcCarrierEnabled(true)
	apply := func(ip string, carriers ...string) netip.Addr {
		record := signTestWebRtcRecord(
			t,
			rootPrivateKey,
			newTestExtenderKey(t),
			clock.Now(),
			clock.Now().Add(14*24*time.Hour),
			NewId(),
			testExtenderAddress(ip, carriers...),
		)
		if _, err := directory.ApplyRecord(record, ExtenderSourceFeed); err != nil {
			t.Fatal(err)
		}
		return netip.MustParseAddr(ip)
	}
	dual := apply("192.0.2.120", ExtenderCarrierTcp, ExtenderCarrierWebRtc)
	webRtcOnly := apply("192.0.2.121", ExtenderCarrierWebRtc)

	directory.RecordFailure(dual, ExtenderConnectModeWebRtc)
	if holdUntil, failures := testExtenderAddressHold(directory, dual); !holdUntil.IsZero() || failures != 0 {
		t.Fatalf("a webrtc failure held a socket address: hold %s, %d failures", holdUntil, failures)
	}
	directory.RecordFailure(dual, ExtenderConnectModeTcpTls)
	if holdUntil, failures := testExtenderAddressHold(directory, dual); holdUntil.IsZero() || failures != 1 {
		t.Fatalf("a tcp failure did not hold the address: hold %s, %d failures", holdUntil, failures)
	}

	directory.RecordFailure(webRtcOnly, ExtenderConnectModeWebRtc)
	if holdUntil, failures := testExtenderAddressHold(directory, webRtcOnly); holdUntil.IsZero() || failures != 1 {
		t.Fatalf("a webrtc failure did not hold a webrtc-only address: hold %s, %d failures", holdUntil, failures)
	}
}
