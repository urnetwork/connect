package connect

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"
)

// Reset tests (the account screen's reset of the extender section). A reset
// returns the directory to what a fresh install starts with and the network
// client started after it relearns everything; every ordering a reset can race
// is forced through a seam, so nothing here waits on wall time.

// The store document of a directory, decoded.
func testDecodeExtenderDirectoryStore(t *testing.T, stateBytes []byte) *extenderDirectoryStoreState {
	t.Helper()
	state := &extenderDirectoryStoreState{}
	if err := json.Unmarshal(stateBytes, state); err != nil {
		t.Fatalf("the stored directory does not decode: %v", err)
	}
	return state
}

// Everything the directory learned and everything a user added goes: the
// records of the feed and the mesh, the dns and imported addresses, the manual
// ones, every hold, limit and latency sample, the continent hint, the
// operator's last country and the root keys a hello installed. The store holds
// the fresh state when the reset returns, and a directory loaded from it is
// fresh too.
func TestExtenderDirectoryResetReturnsTheFreshState(t *testing.T) {
	if networkCountryCode := NetworkCountryCode(); networkCountryCode != "" {
		t.Fatalf("the host network country is %q; this test reads the directory's own country", networkCountryCode)
	}
	clock := newTestClock()
	store := newTestExtenderDirectoryStore()
	directory, rootPrivateKey := newTestExtenderDirectory(t, clock, func(settings *ExtenderDirectorySettings) {
		settings.Store = store
		// the loop never saves on its own here: every write the test reads is
		// the reset's
		settings.SaveTimeout = time.Hour
	})
	bundledRootPrivateKey, bundledRootPublicKey := newTestRootKeyPair(t)
	// a hello installed the operator key beside the bundled one
	directory.SetRootKeys(NewExtenderRootKeySet(
		rootPrivateKey.Public().(ed25519.PublicKey),
		bundledRootPublicKey,
	))
	directory.SetInitialSamplePending()
	directory.SetInitialSampleDone()

	feedIp := netip.MustParseAddr("192.0.2.10")
	gossipIp := netip.MustParseAddr("2001:db8::11")
	dnsIp := netip.MustParseAddr("192.0.2.12")
	importIp := netip.MustParseAddr("192.0.2.13")
	manualIp := netip.MustParseAddr("192.0.2.14")
	feedRecord := signTestRecord(t, rootPrivateKey, newTestExtenderKey(t), clock.Now(), clock.Now().Add(14*24*time.Hour), testExtenderAddress(feedIp.String()))
	if _, err := directory.ApplyRecord(feedRecord, ExtenderSourceFeed); err != nil {
		t.Fatal(err)
	}
	gossipRecord := signTestRecord(t, rootPrivateKey, newTestExtenderKey(t), clock.Now(), clock.Now().Add(14*24*time.Hour), testExtenderAddress(gossipIp.String()))
	if _, err := directory.ApplyRecord(gossipRecord, ExtenderSourceGossip); err != nil {
		t.Fatal(err)
	}
	revokedKey := newTestExtenderKey(t)
	if _, err := directory.ApplyRevocation(signTestRevocation(t, rootPrivateKey, revokedKey, clock.Now())); err != nil {
		t.Fatal(err)
	}
	directory.AddBootstrap(dnsIp, ExtenderSourceDns)
	directory.AddBootstrap(importIp, ExtenderSourceImport)
	directory.AddManual(manualIp)
	directory.RecordFailure(feedIp, ExtenderConnectModeTcpTls)
	directory.RecordLimited(gossipIp, time.Minute)
	directory.RecordLatency(gossipIp, 40*time.Millisecond, true)
	directory.SetContinentHint("EU")
	directory.SetCountryHint("de")
	if snapshot := directory.Snapshot(); snapshot.KnownCount != 5 {
		t.Fatalf("known = %d before the reset, expected the 5 addresses the test added", snapshot.KnownCount)
	}
	if 0 == directory.EventCountLastMinute() {
		t.Fatal("the feed and mesh applies counted no event before the reset")
	}

	bundledRootKeySet := NewExtenderRootKeySet(bundledRootPublicKey)
	directory.Reset(bundledRootKeySet)

	snapshot := directory.Snapshot()
	if snapshot.KnownCount != 0 || len(snapshot.Entries) != 0 {
		t.Fatalf("the reset kept %d addresses: %+v", snapshot.KnownCount, snapshot.Entries)
	}
	if candidates := directory.Candidates(0, 16); len(candidates) != 0 {
		t.Fatalf("the reset left %d candidates", len(candidates))
	}
	if continentHint := directory.ContinentHint(); continentHint != "" {
		t.Fatalf("continent hint = %q after the reset, expected none", continentHint)
	}
	if countryCode := directory.SpoofCountryCode(); countryCode != "" {
		t.Fatalf("spoof country = %q after the reset, expected the global list", countryCode)
	}
	if count := directory.EventCountLastMinute(); count != 0 {
		t.Fatalf("event count = %d after the reset, expected none", count)
	}
	if !directory.RootKeys().Equal(bundledRootKeySet) {
		t.Fatal("the reset kept the root keys the hello installed")
	}
	if state := directory.InitialSampleMonitor().Value(); state != ExtenderInitialSampleNone {
		t.Fatalf("startup gate = %d after the reset, expected none until a client starts", state)
	}
	if directory.IsActiveKey(revokedKey) || directory.AddressUsable(manualIp) {
		t.Fatal("the reset kept an identity or a manual address")
	}

	// the store holds the fresh state the moment the reset returns
	saveCount, stateBytes := store.counts()
	if saveCount == 0 {
		t.Fatal("the reset wrote nothing to the store")
	}
	state := testDecodeExtenderDirectoryStore(t, stateBytes)
	if len(state.Records) != 0 || len(state.Addresses) != 0 || len(state.CountryHint) != 0 {
		t.Fatalf("the stored directory after the reset = %s", stateBytes)
	}
	loadedSettings := DefaultExtenderDirectorySettings()
	loadedSettings.Now = clock.Now
	loadedSettings.NetworkHosts = []string{testExtenderNetworkHost}
	loadedSettings.Store = store
	loaded := NewExtenderDirectory(context.Background(), loadedSettings)
	defer loaded.Close()
	if loadedSnapshot := loaded.Snapshot(); loadedSnapshot.KnownCount != 0 {
		t.Fatalf("a directory loaded after the reset knows %d addresses", loadedSnapshot.KnownCount)
	}
	if countryCode := loaded.SpoofCountryCode(); countryCode != "" {
		t.Fatalf("a directory loaded after the reset has the country %q", countryCode)
	}

	// what the hello keys alone vouch for is refused now, and the directory
	// learns again under the keys it was reset to
	if _, err := directory.ApplyRecord(signTestRecord(t, rootPrivateKey, newTestExtenderKey(t), clock.Now(), clock.Now().Add(14*24*time.Hour), testExtenderAddress("192.0.2.20")), ExtenderSourceFeed); err == nil {
		t.Fatal("a record signed by the key the hello installed applied after the reset")
	}
	if _, err := directory.ApplyRecord(signTestRecord(t, bundledRootPrivateKey, newTestExtenderKey(t), clock.Now(), clock.Now().Add(14*24*time.Hour), testExtenderAddress("192.0.2.21")), ExtenderSourceFeed); err != nil {
		t.Fatalf("a record signed by the bundled key was refused after the reset: %v", err)
	}
	if state := testDirectoryState(t, directory, netip.MustParseAddr("192.0.2.21")); state != ExtenderStateActive {
		t.Fatalf("state = %s for a record learned after the reset, expected active", state)
	}
}

// The identity the directory was told to keep -- this device's own extender
// record, which its peers judge its pings by -- is not knowledge of another
// extender: it stays, with fresh evidence for its address, while it verifies
// under the keys of the reset, and goes once it does not.
func TestExtenderDirectoryResetKeepsTheOwnIdentity(t *testing.T) {
	clock := newTestClock()
	directory, rootPrivateKey := newTestExtenderDirectory(t, clock, nil)
	rootKeySet := directory.RootKeys()

	ownKey := newTestExtenderKey(t)
	ownIp := netip.MustParseAddr("198.51.100.1")
	otherIp := netip.MustParseAddr("198.51.100.2")
	directory.KeepPublicKey(ownKey)
	ownRecord := signTestRecord(t, rootPrivateKey, ownKey, clock.Now(), clock.Now().Add(14*24*time.Hour), testExtenderAddress(ownIp.String()))
	if _, err := directory.ApplyRecord(ownRecord, ExtenderSourceBootstrap); err != nil {
		t.Fatal(err)
	}
	otherRecord := signTestRecord(t, rootPrivateKey, newTestExtenderKey(t), clock.Now(), clock.Now().Add(14*24*time.Hour), testExtenderAddress(otherIp.String()))
	if _, err := directory.ApplyRecord(otherRecord, ExtenderSourceFeed); err != nil {
		t.Fatal(err)
	}
	directory.AddManual(ownIp)
	directory.RecordFailure(ownIp, ExtenderConnectModeTcpTls)

	directory.Reset(rootKeySet)

	if !directory.IsActiveKey(ownKey) {
		t.Fatal("the reset dropped this device's own extender record")
	}
	snapshot := directory.Snapshot()
	if snapshot.KnownCount != 1 {
		t.Fatalf("known = %d after the reset, expected the own address alone: %+v", snapshot.KnownCount, snapshot.Entries)
	}
	entry := snapshot.Entries[0]
	if entry.Ip != ownIp || entry.State != ExtenderStateActive || entry.FailureCount != 0 || entry.Source != ExtenderSourceBootstrap {
		t.Fatalf("own address after the reset = %+v, expected an active bootstrap address with no evidence", entry)
	}
	if directory.ActiveRecordCount() != 1 {
		t.Fatalf("active records = %d after the reset, expected the own one", directory.ActiveRecordCount())
	}

	// keys that do not vouch for it retire it too
	_, otherRootPublicKey := newTestRootKeyPair(t)
	directory.Reset(NewExtenderRootKeySet(otherRootPublicKey))
	if directory.IsActiveKey(ownKey) || directory.Snapshot().KnownCount != 0 {
		t.Fatal("a reset to keys that do not vouch for the own record kept it")
	}
}

// A record or revocation verified before a reset and applied after it came
// from what the reset cleared: it is dropped, and the directory stays fresh.
// The seam between the verification and the apply runs the reset there.
func TestExtenderDirectoryResetDropsAMessageVerifiedBeforeIt(t *testing.T) {
	clock := newTestClock()
	directory, rootPrivateKey := newTestExtenderDirectory(t, clock, nil)
	rootKeySet := directory.RootKeys()

	resetOnce := func() {
		directory.applyVerifiedHook = func() {
			directory.applyVerifiedHook = nil
			directory.Reset(rootKeySet)
		}
	}

	resetOnce()
	record := signTestRecord(t, rootPrivateKey, newTestExtenderKey(t), clock.Now(), clock.Now().Add(14*24*time.Hour), testExtenderAddress("203.0.113.1"))
	if changed, err := directory.ApplyRecord(record, ExtenderSourceFeed); !errors.Is(err, errExtenderDirectoryReset) || changed {
		t.Fatalf("a record verified before the reset: changed = %t, err = %v", changed, err)
	}
	if snapshot := directory.Snapshot(); snapshot.KnownCount != 0 {
		t.Fatalf("a record verified before the reset landed after it: %+v", snapshot.Entries)
	}

	resetOnce()
	revokedKey := newTestExtenderKey(t)
	revocation := signTestRevocation(t, rootPrivateKey, revokedKey, clock.Now())
	if changed, err := directory.ApplyRevocation(revocation); !errors.Is(err, errExtenderDirectoryReset) || changed {
		t.Fatalf("a revocation verified before the reset: changed = %t, err = %v", changed, err)
	}

	// what is verified after the reset applies
	if _, err := directory.ApplyRecord(record, ExtenderSourceFeed); err != nil {
		t.Fatalf("a record verified after the reset was refused: %v", err)
	}
	if _, err := directory.ApplyRevocation(revocation); err != nil {
		t.Fatalf("a revocation verified after the reset was refused: %v", err)
	}
	if !testDirectoryKnown(directory, netip.MustParseAddr("203.0.113.1")) {
		t.Fatal("the record applied after the reset is not in the directory")
	}
}

// A save that read the state before a reset is skipped once the reset's own
// save has written the fresh state: the stale state never lands over it. The
// seam between the save's read and its write runs the whole reset there.
func TestExtenderDirectoryResetIsNotOverwrittenByAnEarlierSave(t *testing.T) {
	clock := newTestClock()
	store := newTestExtenderDirectoryStore()
	directory, _ := newTestExtenderDirectory(t, clock, func(settings *ExtenderDirectorySettings) {
		settings.Store = store
		settings.SaveTimeout = time.Hour
	})
	rootKeySet := directory.RootKeys()
	directory.AddManual(netip.MustParseAddr("192.0.2.30"))

	directory.saveReadHook = func() {
		directory.saveReadHook = nil
		directory.Reset(rootKeySet)
	}
	directory.save()

	saveCount, stateBytes := store.counts()
	state := testDecodeExtenderDirectoryStore(t, stateBytes)
	if len(state.Addresses) != 0 {
		t.Fatalf("the save that read the state before the reset landed after it (%d saves): %s", saveCount, stateBytes)
	}
	if saveCount != 1 {
		t.Fatalf("saves = %d, expected the reset's alone", saveCount)
	}
}

// A connection made through an extender outlives the reset that dropped its
// address: once the address is learned again it is reported in use, and the
// connection's release balances its hold.
func TestExtenderDirectoryResetKeepsLiveUseBalanced(t *testing.T) {
	clock := newTestClock()
	directory, _ := newTestExtenderDirectory(t, clock, nil)
	ip := netip.MustParseAddr("192.0.2.40")
	directory.AddManual(ip)
	directory.SetInUse(ip, 1)

	directory.Reset(directory.RootKeys())
	if snapshot := directory.Snapshot(); snapshot.KnownCount != 0 || snapshot.InUseCount != 0 {
		t.Fatalf("after the reset known = %d, in use = %d, expected neither", snapshot.KnownCount, snapshot.InUseCount)
	}

	// learned again while the connection lives
	directory.AddBootstrap(ip, ExtenderSourceDns)
	if entry := testDirectoryEntry(t, directory, ip); entry.InUse != 1 {
		t.Fatalf("in use = %d for an address carrying a connection from before the reset", entry.InUse)
	}
	// a second connection after it, then the first one's release
	directory.SetInUse(ip, 1)
	directory.SetInUse(ip, -1)
	if entry := testDirectoryEntry(t, directory, ip); entry.InUse != 1 {
		t.Fatalf("in use = %d after the older connection ended, expected the newer one", entry.InUse)
	}
	directory.SetInUse(ip, -1)
	if snapshot := directory.Snapshot(); snapshot.InUseCount != 0 {
		t.Fatalf("in use count = %d after every connection ended", snapshot.InUseCount)
	}
}

// The startup gate waits on the first sample of a client started after the
// reset, as on a first run: the reset puts it back to no client, and the next
// client's announcement makes it pending again rather than leaving it done.
func TestExtenderDirectoryResetRearmsTheStartupGate(t *testing.T) {
	clock := newTestClock()
	directory, _ := newTestExtenderDirectory(t, clock, nil)
	directory.SetInitialSamplePending()
	directory.SetInitialSampleDone()

	directory.Reset(directory.RootKeys())
	directory.SetInitialSamplePending()
	if state := directory.InitialSampleMonitor().Value(); state != ExtenderInitialSamplePending {
		t.Fatalf("startup gate = %d for the client started after the reset, expected pending", state)
	}
}

// A network client started after a reset relearns everything from scratch:
// hello is read again and installs its keys, under which the dns bootstrap
// applies its records again, as on a first run.
func TestExtenderNetworkClientRelearnsAfterTheDirectoryReset(t *testing.T) {
	clock := newTestClock()
	rootPrivateKey, rootPublicKey := newTestRootKeyPair(t)
	recordTxt := testExtenderDnsRecordTxt(t, rootPrivateKey, clock, "192.0.2.50")
	helloCount := atomic.Int64{}
	configure := func(settings *ExtenderNetworkClientSettings) {
		settings.Hello = func(ctx context.Context) (*ExtenderHelloResult, error) {
			helloCount.Add(1)
			return &ExtenderHelloResult{
				RootPublicKeyHexes: []string{ExtenderKeySeedHex(rootPublicKey)},
			}, nil
		}
		settings.ResolveDnsTxt = func(ctx context.Context, name string) ([]string, error) {
			return []string{recordTxt}, nil
		}
		settings.ResolveDns = func(ctx context.Context, name string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("192.0.2.51")}, nil
		}
	}
	networkClient, directory, _ := newTestExtenderNetworkClient(t, clock, configure)
	waitForDirectoryAddresses(t, directory, map[string]string{
		"192.0.2.50": ExtenderSourceDns,
		"192.0.2.51": ExtenderSourceDns,
	})

	// the owner's restart: the client stops, the directory resets with no
	// bundled keys, and a new client starts
	networkClient.Close()
	firstHelloCount := helloCount.Load()
	directory.Reset(nil)
	if directory.Snapshot().KnownCount != 0 || directory.RootKeys().Len() != 0 {
		t.Fatal("the reset kept addresses or root keys")
	}

	settings := DefaultExtenderNetworkClientSettings()
	settings.Now = clock.Now
	settings.ExtenderDnsName = "extender.space.example"
	settings.MinBackoff = time.Millisecond
	settings.MaxBackoff = 10 * time.Millisecond
	settings.DialTimeout = 2 * time.Second
	settings.HelloTimeout = 2 * time.Second
	settings.IpVersionSupported = func(ipVersion int) bool { return true }
	settings.ProbeWindowCount = 0
	configure(settings)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	relearningClient := NewExtenderNetworkClient(ctx, newTestDeadDialStrategy(t, ctx), directory, settings)
	defer relearningClient.Close()

	waitForDirectoryAddresses(t, directory, map[string]string{
		"192.0.2.50": ExtenderSourceDns,
		"192.0.2.51": ExtenderSourceDns,
	})
	if helloCount.Load() <= firstHelloCount {
		t.Fatal("the client started after the reset did not read hello again")
	}
	if !directory.RootKeys().Equal(NewExtenderRootKeySet(rootPublicKey)) {
		t.Fatal("the client started after the reset did not install the hello keys again")
	}
}
