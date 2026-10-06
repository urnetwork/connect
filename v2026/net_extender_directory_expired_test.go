package connect

import (
	"context"
	"crypto/ed25519"
	"net/netip"
	"slices"
	"testing"
	"time"
)

// The expired identities a directory keeps past their expiry
// (MaxExpiredRecordCount), and the active-key question an extender asks of a
// pinger (GEOMAP §2.4).

// Applies one record for a fresh key at one address, issued now and expiring
// `expireAfter` from now, and returns the key.
func applyTestExpiringRecord(
	t *testing.T,
	directory *ExtenderDirectory,
	rootPrivateKey ed25519.PrivateKey,
	clock *testClock,
	ip string,
	expireAfter time.Duration,
) ed25519.PublicKey {
	t.Helper()
	publicKey := newTestExtenderKey(t)
	record := signTestRecord(
		t,
		rootPrivateKey,
		publicKey,
		clock.Now(),
		clock.Now().Add(expireAfter),
		testExtenderAddress(ip),
	)
	if _, err := directory.ApplyRecord(record, ExtenderSourceFeed); err != nil {
		t.Fatal(err)
	}
	return publicKey
}

// A key is active while its record is verified, unrevoked and unexpired,
// and an unknown key never is.
func TestExtenderDirectoryIsActiveKey(t *testing.T) {
	clock := newTestClock()
	directory, rootPrivateKey := newTestExtenderDirectory(t, clock, nil)

	activeKey := applyTestExpiringRecord(t, directory, rootPrivateKey, clock, "192.0.2.1", 24*time.Hour)
	expiringKey := applyTestExpiringRecord(t, directory, rootPrivateKey, clock, "192.0.2.2", time.Hour)
	revokedKey := applyTestExpiringRecord(t, directory, rootPrivateKey, clock, "192.0.2.3", 24*time.Hour)
	if _, err := directory.ApplyRevocation(signTestRevocation(t, rootPrivateKey, revokedKey, clock.Now())); err != nil {
		t.Fatal(err)
	}

	if !directory.IsActiveKey(activeKey) || !directory.IsActiveKey(expiringKey) {
		t.Fatal("an active record's key is not active")
	}
	if directory.IsActiveKey(revokedKey) {
		t.Fatal("a revoked key is active")
	}
	if directory.IsActiveKey(newTestExtenderKey(t)) {
		t.Fatal("an unknown key is active")
	}
	if directory.IsActiveKey(activeKey[0:31]) || directory.IsActiveKey(nil) {
		t.Fatal("a malformed key is active")
	}
	// an unverified manual address has no key to be active
	directory.AddManual(netip.MustParseAddr("192.0.2.9"))
	if directory.IsActiveKey(make([]byte, ed25519.PublicKeySize)) {
		t.Fatal("the zero key is active")
	}

	// past the expiry and its skew the key is no longer active, even while
	// the directory retains it as a last resort
	clock.advance(time.Hour + directory.settings.RecordExpireSkew + time.Second)
	if directory.IsActiveKey(expiringKey) {
		t.Fatal("an expired key is active")
	}
	if !directory.AddressUsable(netip.MustParseAddr("192.0.2.2")) {
		t.Fatal("the expired key's address was not retained")
	}
	// held evidence is not the question: a held address's key stays active
	directory.RecordFailure(netip.MustParseAddr("192.0.2.1"), ExtenderConnectModeTcpTls)
	if !directory.IsActiveKey(activeKey) {
		t.Fatal("a held address made its key inactive")
	}
	// a newer record brings the expired key back
	record := signTestRecord(
		t,
		rootPrivateKey,
		expiringKey,
		clock.Now(),
		clock.Now().Add(24*time.Hour),
		testExtenderAddress("192.0.2.2"),
	)
	if _, err := directory.ApplyRecord(record, ExtenderSourceFeed); err != nil {
		t.Fatal(err)
	}
	if !directory.IsActiveKey(expiringKey) {
		t.Fatal("a renewed key is not active")
	}
}

// Expire keeps the newest expired identities with their addresses and local
// evidence, and evicts the rest oldest expiry first.
func TestExtenderDirectoryExpireRetainsTheNewestExpired(t *testing.T) {
	clock := newTestClock()
	directory, rootPrivateKey := newTestExtenderDirectory(t, clock, func(settings *ExtenderDirectorySettings) {
		settings.MaxExpiredRecordCount = 2
	})
	skew := directory.settings.RecordExpireSkew

	oldestIp := netip.MustParseAddr("192.0.2.1")
	middleIp := netip.MustParseAddr("192.0.2.2")
	newestIp := netip.MustParseAddr("192.0.2.3")
	activeIp := netip.MustParseAddr("192.0.2.4")
	oldestKey := applyTestExpiringRecord(t, directory, rootPrivateKey, clock, oldestIp.String(), 1*time.Hour)
	middleKey := applyTestExpiringRecord(t, directory, rootPrivateKey, clock, middleIp.String(), 2*time.Hour)
	newestKey := applyTestExpiringRecord(t, directory, rootPrivateKey, clock, newestIp.String(), 3*time.Hour)
	applyTestExpiringRecord(t, directory, rootPrivateKey, clock, activeIp.String(), 30*24*time.Hour)
	// local evidence the retention must keep
	directory.RecordSuccess(middleIp, ExtenderConnectModeTcpTls)
	directory.RecordLatency(newestIp, 30*time.Millisecond, true)

	// two expired: both within the retained count, nothing evicted
	clock.advance(2*time.Hour + skew + time.Second)
	directory.Expire(clock.Now())
	for _, ip := range []netip.Addr{oldestIp, middleIp, newestIp, activeIp} {
		if !testDirectoryKnown(directory, ip) {
			t.Fatalf("%s was evicted with the expired within the retained count", ip)
		}
	}

	// three expired: the oldest expiry goes, with its identity
	clock.advance(time.Hour)
	directory.Expire(clock.Now())
	if testDirectoryKnown(directory, oldestIp) {
		t.Fatal("the oldest expired address was kept beyond the retained count")
	}
	if key := testDirectoryKeyRecord(directory, oldestKey); key {
		t.Fatal("the oldest expired identity was kept beyond the retained count")
	}
	for _, ip := range []netip.Addr{middleIp, newestIp} {
		if state := testDirectoryState(t, directory, ip); state != ExtenderStateExpired {
			t.Fatalf("%s state = %s, expected a retained expired", ip, state)
		}
	}
	if state := testDirectoryState(t, directory, activeIp); state != ExtenderStateActive {
		t.Fatalf("the active address is %s", state)
	}
	if entry := testDirectoryEntry(t, directory, middleIp); entry.SuccessCount != 1 {
		t.Fatalf("the retained address lost its evidence: %+v", entry)
	}
	if !testDirectoryKeyRecord(directory, middleKey) || !testDirectoryKeyRecord(directory, newestKey) {
		t.Fatal("a retained identity was dropped")
	}

	// a newer expiry pushes the oldest retained out
	laterIp := netip.MustParseAddr("192.0.2.5")
	applyTestExpiringRecord(t, directory, rootPrivateKey, clock, laterIp.String(), time.Minute)
	clock.advance(time.Minute + skew + time.Second)
	directory.Expire(clock.Now())
	if testDirectoryKnown(directory, middleIp) {
		t.Fatal("the oldest retained was kept past a newer expiry")
	}
	for _, ip := range []netip.Addr{newestIp, laterIp, activeIp} {
		if !testDirectoryKnown(directory, ip) {
			t.Fatalf("%s was evicted", ip)
		}
	}
	// Expire reports the eviction as a change, and a pass with nothing to
	// evict as none
	if directory.Expire(clock.Now()) {
		t.Fatal("an Expire with nothing to evict reported a change")
	}
}

// Whether the directory still holds an identity for the key.
func testDirectoryKeyRecord(directory *ExtenderDirectory, publicKey ed25519.PublicKey) bool {
	directory.stateLock.Lock()
	defer directory.stateLock.Unlock()
	for _, keyRecord := range directory.keyHexRecords {
		if slices.Equal(keyRecord.publicKey, publicKey) {
			return true
		}
	}
	return false
}

// With nothing retained every expired identity goes at the next Expire, but a
// manual address outlives its identity, and a revocation the identity carried
// is kept so a replayed older record cannot bring the key back.
func TestExtenderDirectoryExpireEvictsWithNothingRetained(t *testing.T) {
	clock := newTestClock()
	directory, rootPrivateKey := newTestExtenderDirectory(t, clock, func(settings *ExtenderDirectorySettings) {
		settings.MaxExpiredRecordCount = 0
	})
	skew := directory.settings.RecordExpireSkew

	expiredIp := netip.MustParseAddr("192.0.2.1")
	applyTestExpiringRecord(t, directory, rootPrivateKey, clock, expiredIp.String(), time.Hour)

	manualIp := netip.MustParseAddr("192.0.2.2")
	directory.AddManual(manualIp)
	manualKey := applyTestExpiringRecord(t, directory, rootPrivateKey, clock, manualIp.String(), time.Hour)

	// a key revoked long ago and re-issued since: the revocation is older than
	// the record, so it does not revoke it, but it must outlive the eviction
	revokedIp := netip.MustParseAddr("192.0.2.3")
	revokedKey := newTestExtenderKey(t)
	revocationTime := clock.Now()
	if _, err := directory.ApplyRevocation(signTestRevocation(t, rootPrivateKey, revokedKey, revocationTime)); err != nil {
		t.Fatal(err)
	}
	clock.advance(time.Minute)
	reissued := signTestRecord(t, rootPrivateKey, revokedKey, clock.Now(), clock.Now().Add(time.Hour), testExtenderAddress(revokedIp.String()))
	if _, err := directory.ApplyRecord(reissued, ExtenderSourceFeed); err != nil {
		t.Fatal(err)
	}
	if !directory.IsActiveKey(revokedKey) {
		t.Fatal("the re-issued key is not active")
	}

	// nothing retained: not usable once expired, even before Expire runs --
	// the manual address included, while the expired record still claims it
	clock.advance(time.Hour + skew + time.Second)
	if directory.AddressUsable(expiredIp) || directory.AddressUsable(manualIp) {
		t.Fatal("an expired address was usable with nothing retained")
	}
	if candidates := directory.Candidates(4, 8); len(candidates) != 0 {
		t.Fatalf("candidates = %v, expected none with every record expired", candidateIps(candidates))
	}

	if !directory.Expire(clock.Now()) {
		t.Fatal("the eviction was not a change")
	}
	if testDirectoryKnown(directory, expiredIp) || testDirectoryKnown(directory, revokedIp) {
		t.Fatal("an expired address survived")
	}
	if !testDirectoryKnown(directory, manualIp) {
		t.Fatal("a manual address was evicted with its identity")
	}
	if state := testDirectoryState(t, directory, manualIp); state != ExtenderStateUnverified {
		t.Fatalf("the manual address is %s, expected unverified", state)
	}
	// and dialable again, on the carrier defaults, as any manual entry is
	if candidates := directory.Candidates(4, 8); len(candidates) != 1 || candidates[0].Ip != manualIp || candidates[0].Expired || candidates[0].Verified {
		t.Fatalf("candidates = %v, expected only the unverified manual address", candidateIps(candidates))
	}
	if testDirectoryKeyRecord(directory, manualKey) {
		t.Fatal("the manual address's expired identity was kept")
	}
	if !testDirectoryKeyRecord(directory, revokedKey) {
		t.Fatal("the revocation was dropped with the expired record")
	}
	// a replay of a record from before the revocation stays revoked
	replayed := signTestRecord(
		t,
		rootPrivateKey,
		revokedKey,
		revocationTime.Add(-time.Minute),
		clock.Now().Add(time.Hour),
		testExtenderAddress(revokedIp.String()),
	)
	if _, err := directory.ApplyRecord(replayed, ExtenderSourceFeed); err != nil {
		t.Fatal(err)
	}
	if directory.IsActiveKey(revokedKey) || directory.AddressUsable(revokedIp) {
		t.Fatal("a replayed record from before the revocation brought the key back")
	}
}

// The address cap never evicts a retained expired identity; an expired one
// beyond the retained count goes first, oldest expiry first, after anything
// revoked.
func TestExtenderDirectoryCapKeepsTheRetainedExpired(t *testing.T) {
	clock := newTestClock()
	directory, rootPrivateKey := newTestExtenderDirectory(t, clock, func(settings *ExtenderDirectorySettings) {
		settings.MaxAddressCount = 4
		settings.MaxExpiredRecordCount = 1
	})
	skew := directory.settings.RecordExpireSkew

	olderExpiredIp := netip.MustParseAddr("192.0.2.1")
	newerExpiredIp := netip.MustParseAddr("192.0.2.2")
	newestExpiredIp := netip.MustParseAddr("192.0.2.3")
	revokedIp := netip.MustParseAddr("192.0.2.4")
	applyTestExpiringRecord(t, directory, rootPrivateKey, clock, olderExpiredIp.String(), 1*time.Hour)
	applyTestExpiringRecord(t, directory, rootPrivateKey, clock, newerExpiredIp.String(), 2*time.Hour)
	applyTestExpiringRecord(t, directory, rootPrivateKey, clock, newestExpiredIp.String(), 3*time.Hour)
	revokedKey := applyTestExpiringRecord(t, directory, rootPrivateKey, clock, revokedIp.String(), 30*24*time.Hour)
	if _, err := directory.ApplyRevocation(signTestRevocation(t, rootPrivateKey, revokedKey, clock.Now())); err != nil {
		t.Fatal(err)
	}
	clock.advance(3*time.Hour + skew + time.Second)

	// over the cap by one: the revoked address goes first
	firstIp := netip.MustParseAddr("192.0.2.10")
	directory.AddBootstrap(firstIp, ExtenderSourceDns)
	if testDirectoryKnown(directory, revokedIp) {
		t.Fatal("the revoked address was not evicted first")
	}
	// then the expired beyond the retained one, oldest expiry first
	secondIp := netip.MustParseAddr("192.0.2.11")
	directory.AddBootstrap(secondIp, ExtenderSourceDns)
	if testDirectoryKnown(directory, olderExpiredIp) || !testDirectoryKnown(directory, newerExpiredIp) {
		t.Fatal("the older expired address was not evicted before the newer")
	}
	thirdIp := netip.MustParseAddr("192.0.2.12")
	directory.AddBootstrap(thirdIp, ExtenderSourceDns)
	if testDirectoryKnown(directory, newerExpiredIp) {
		t.Fatal("the newer expired beyond the retained count was not evicted next")
	}
	// the retained expired identity is never evicted: the never-succeeded
	// bootstrap addresses go instead, oldest first
	for i, ip := range []string{"192.0.2.13", "192.0.2.14", "192.0.2.15"} {
		clock.advance(time.Minute)
		directory.AddBootstrap(netip.MustParseAddr(ip), ExtenderSourceDns)
		if !testDirectoryKnown(directory, newestExpiredIp) {
			t.Fatalf("overflow %d evicted the retained expired address", i)
		}
	}
	if testDirectoryKnown(directory, firstIp) {
		t.Fatal("the oldest never-succeeded address was kept over the cap")
	}
	if state := testDirectoryState(t, directory, newestExpiredIp); state != ExtenderStateExpired {
		t.Fatalf("the retained address is %s", state)
	}
	if count := len(directory.Snapshot().Entries); count != 4 {
		t.Fatalf("the directory holds %d addresses under a cap of 4", count)
	}
}

// The retained expired addresses are the last tier of Candidates: after every
// active verified address and every manual one, the newest expiry first and
// then the usual order, and only in what the count leaves.
func TestExtenderDirectoryCandidatesPutTheExpiredLast(t *testing.T) {
	clock := newTestClock()
	directory, rootPrivateKey := newTestExtenderDirectory(t, clock, func(settings *ExtenderDirectorySettings) {
		settings.MaxExpiredRecordCount = 2
	})
	skew := directory.settings.RecordExpireSkew

	// the oldest expiry is beyond the retained count
	beyondIp := netip.MustParseAddr("192.0.2.1")
	applyTestExpiringRecord(t, directory, rootPrivateKey, clock, beyondIp.String(), 1*time.Hour)
	// one retained identity with two addresses, so its order within the
	// expiry falls to the usual keys, and a newer one
	olderKey := newTestExtenderKey(t)
	olderFirstIp := netip.MustParseAddr("192.0.2.2")
	olderSecondIp := netip.MustParseAddr("192.0.2.3")
	olderRecord := signTestRecord(
		t,
		rootPrivateKey,
		olderKey,
		clock.Now(),
		clock.Now().Add(2*time.Hour),
		testExtenderAddress(olderFirstIp.String()),
		testExtenderAddress(olderSecondIp.String()),
	)
	if _, err := directory.ApplyRecord(olderRecord, ExtenderSourceFeed); err != nil {
		t.Fatal(err)
	}
	newerIp := netip.MustParseAddr("192.0.2.4")
	applyTestExpiringRecord(t, directory, rootPrivateKey, clock, newerIp.String(), 3*time.Hour)
	// active and manual
	activeIp := netip.MustParseAddr("192.0.2.5")
	applyTestExpiringRecord(t, directory, rootPrivateKey, clock, activeIp.String(), 30*24*time.Hour)
	manualIp := netip.MustParseAddr("192.0.2.6")
	directory.AddManual(manualIp)
	// the most recent success puts the second address of the older identity
	// before its first
	directory.RecordSuccess(olderSecondIp, ExtenderConnectModeTcpTls)

	clock.advance(3*time.Hour + skew + time.Second)
	candidates := directory.Candidates(4, 16)
	assertIpOrder(
		t,
		candidates,
		activeIp.String(),
		manualIp.String(),
		newerIp.String(),
		olderSecondIp.String(),
		olderFirstIp.String(),
	)
	for _, candidate := range candidates {
		expired := candidate.Ip != activeIp && candidate.Ip != manualIp
		if candidate.Expired != expired {
			t.Fatalf("%s expired = %t", candidate.Ip, candidate.Expired)
		}
		if expired && !candidate.Verified {
			t.Fatalf("%s is expired but not verified", candidate.Ip)
		}
	}
	// the count cap: the expired tier fills only what is left
	assertIpOrder(t, directory.Candidates(4, 2), activeIp.String(), manualIp.String())
	assertIpOrder(t, directory.Candidates(4, 3), activeIp.String(), manualIp.String(), newerIp.String())
	// the exclusion and the family apply to the tier too
	assertIpOrder(
		t,
		directory.Candidates(4, 16, activeIp, newerIp),
		manualIp.String(),
		olderSecondIp.String(),
		olderFirstIp.String(),
	)
	if candidates := directory.Candidates(6, 16); len(candidates) != 0 {
		t.Fatalf("v6 candidates = %v", candidateIps(candidates))
	}
	// a held retained address is left out, like any held address
	directory.RecordFailure(newerIp, ExtenderConnectModeTcpTls)
	assertIpOrder(
		t,
		directory.Candidates(4, 16),
		activeIp.String(),
		manualIp.String(),
		olderSecondIp.String(),
		olderFirstIp.String(),
	)
	// a probe pass never measures the expired tier
	for _, candidate := range directory.ProbeCandidates(4, 16, false) {
		if candidate.Expired || candidate.Ip == olderFirstIp || candidate.Ip == olderSecondIp {
			t.Fatalf("an expired address is a probe candidate: %s", candidate.Ip)
		}
	}
}

// A retained expired record is never active: the counts leave it out, so the
// low-water re-bootstrap still fires, the feed and gossip never sample it,
// and the status says it is expired.
func TestExtenderDirectoryExpiredIsNeverActive(t *testing.T) {
	clock := newTestClock()
	directory, rootPrivateKey := newTestExtenderDirectory(t, clock, nil)
	skew := directory.settings.RecordExpireSkew

	ownKey := applyTestExpiringRecord(t, directory, rootPrivateKey, clock, "192.0.2.1", time.Hour)
	otherKey := applyTestExpiringRecord(t, directory, rootPrivateKey, clock, "192.0.2.2", time.Hour)
	activeKey := applyTestExpiringRecord(t, directory, rootPrivateKey, clock, "192.0.2.3", 30*24*time.Hour)
	if count := directory.ActiveCount(0); count != 3 {
		t.Fatalf("active = %d before the expiry", count)
	}

	clock.advance(time.Hour + skew + time.Second)
	directory.Expire(clock.Now())
	for _, ip := range []string{"192.0.2.1", "192.0.2.2"} {
		if !directory.AddressUsable(netip.MustParseAddr(ip)) {
			t.Fatalf("%s was not retained", ip)
		}
		if state := testDirectoryState(t, directory, netip.MustParseAddr(ip)); state != ExtenderStateExpired {
			t.Fatalf("%s state = %s, expected expired", ip, state)
		}
	}
	if count := directory.ActiveCount(0); count != 1 {
		t.Fatalf("active = %d, expected only the current record", count)
	}
	if count := directory.UsableCount(0); count != 1 {
		t.Fatalf("usable = %d, expected only the current record", count)
	}
	snapshot := directory.Snapshot()
	if snapshot.ActiveCount != 1 || snapshot.KnownCount != 3 {
		t.Fatalf("snapshot active %d of %d", snapshot.ActiveCount, snapshot.KnownCount)
	}
	if latencies := directory.MeasuredLatencies(0, false); len(latencies) != 0 {
		t.Fatalf("latencies = %v", latencies)
	}

	// never sampled, the node's own included
	messages := directory.SampleRecords(8, ownKey)
	if len(messages) != 1 {
		t.Fatalf("sample = %d records, expected only the current one", len(messages))
	}
	body, err := directory.RootKeys().VerifyRecord(messages[0].GetRecord())
	if err != nil {
		t.Fatal(err)
	}
	if !ed25519.PublicKey(body.PublicKey).Equal(activeKey) {
		t.Fatal("an expired record was sampled")
	}
	for _, message := range directory.SampleRecords(8, otherKey) {
		body, err := directory.RootKeys().VerifyRecord(message.GetRecord())
		if err != nil {
			t.Fatal(err)
		}
		if ed25519.PublicKey(body.PublicKey).Equal(otherKey) || ed25519.PublicKey(body.PublicKey).Equal(ownKey) {
			t.Fatal("an expired record was sampled")
		}
	}
}

// A retained expired address is usable, so a dialer to it is kept; beyond the
// retained count, held, or revoked, it is not.
func TestExtenderDirectoryAddressUsableOnTheRetainedExpired(t *testing.T) {
	clock := newTestClock()
	directory, rootPrivateKey := newTestExtenderDirectory(t, clock, func(settings *ExtenderDirectorySettings) {
		settings.MaxExpiredRecordCount = 1
	})
	skew := directory.settings.RecordExpireSkew

	olderIp := netip.MustParseAddr("192.0.2.1")
	newerIp := netip.MustParseAddr("192.0.2.2")
	revokedIp := netip.MustParseAddr("192.0.2.3")
	applyTestExpiringRecord(t, directory, rootPrivateKey, clock, olderIp.String(), time.Hour)
	applyTestExpiringRecord(t, directory, rootPrivateKey, clock, newerIp.String(), 2*time.Hour)
	revokedKey := applyTestExpiringRecord(t, directory, rootPrivateKey, clock, revokedIp.String(), time.Hour)

	clock.advance(2*time.Hour + skew + time.Second)
	// before any Expire the retained set is still the newest one
	if !directory.AddressUsable(newerIp) {
		t.Fatal("the retained expired address is not usable")
	}
	if directory.AddressUsable(olderIp) {
		t.Fatal("an expired address beyond the retained count is usable")
	}
	// a revoked key is never a last resort, and never counts toward the
	// retained ones
	if _, err := directory.ApplyRevocation(signTestRevocation(t, rootPrivateKey, revokedKey, clock.Now())); err != nil {
		t.Fatal(err)
	}
	if directory.AddressUsable(revokedIp) {
		t.Fatal("a revoked expired address is usable")
	}
	if !directory.AddressUsable(newerIp) {
		t.Fatal("a revoked identity displaced the retained one")
	}
	// the hold still applies
	directory.RecordFailure(newerIp, ExtenderConnectModeTcpTls)
	if directory.AddressUsable(newerIp) {
		t.Fatal("a held retained address is usable")
	}
	clock.advance(directory.settings.HoldTimeout)
	if !directory.AddressUsable(newerIp) {
		t.Fatal("the retained address did not come back after its hold")
	}
}

// The retained expired records are the directory's own state: they are saved,
// and a client that restarts with no path to the operator still has them.
func TestExtenderDirectoryRetainedExpiredSurviveARestart(t *testing.T) {
	clock := newTestClock()
	store := newTestExtenderDirectoryStore()
	directory, rootPrivateKey := newTestExtenderDirectory(t, clock, func(settings *ExtenderDirectorySettings) {
		settings.Store = store
		settings.SaveTimeout = time.Millisecond
	})
	skew := directory.settings.RecordExpireSkew
	expiredIp := netip.MustParseAddr("192.0.2.1")
	applyTestExpiringRecord(t, directory, rootPrivateKey, clock, expiredIp.String(), time.Hour)
	clock.advance(time.Hour + skew + time.Second)
	directory.Expire(clock.Now())
	directory.Close()

	settings := DefaultExtenderDirectorySettings()
	settings.Now = clock.Now
	settings.NetworkHosts = []string{testExtenderNetworkHost}
	settings.Store = store
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	restarted := NewExtenderDirectory(ctx, settings)
	defer restarted.Close()
	candidates := restarted.Candidates(0, 8)
	if len(candidates) != 1 || candidates[0].Ip != expiredIp || !candidates[0].Expired {
		t.Fatalf("candidates after the restart = %v", candidateIps(candidates))
	}
	if !restarted.AddressUsable(expiredIp) {
		t.Fatal("the retained address is not usable after the restart")
	}
	if count := restarted.UsableCount(0); count != 0 {
		t.Fatalf("usable = %d after the restart", count)
	}
}
