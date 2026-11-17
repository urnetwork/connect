package connect

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

// The rule of net_extender_directory_gated_block.go, against the fake clock and
// a fake dial. Each test names the rule it pins: one failed dial is nothing,
// every carrier of every address fails, attempts are spaced and counted only
// with proof of reach after them, a path change or an answer starts over, and
// nothing a client cannot verify or does not use is ever a suspect.

var errTestBlockDial = errors.New("the dial failed")

type testBlockEvidence struct {
	t              *testing.T
	clock          *testClock
	directory      *ExtenderDirectory
	rootPrivateKey ed25519.PrivateKey
	settings       *ExtenderBlockEvidenceSettings
	ipVersions     []int
}

func newTestBlockEvidence(t *testing.T) *testBlockEvidence {
	t.Helper()
	clock := newTestClock()
	directory, rootPrivateKey := newTestExtenderDirectory(t, clock, nil)
	directory.SetGatedPreference(&ExtenderGatedPreferenceSettings{Enabled: true})
	settings := DefaultExtenderBlockEvidenceSettings()
	settings.Enabled = true
	directory.SetBlockEvidence(settings)
	return &testBlockEvidence{
		t:              t,
		clock:          clock,
		directory:      directory,
		rootPrivateKey: rootPrivateKey,
		settings:       settings,
		ipVersions:     []int{4, 6},
	}
}

// Signs one record of `tier` at `addresses`, issued now for a day.
func (self *testBlockEvidence) sign(key ed25519.PublicKey, tier int, addresses ...*protocol.ExtenderAddress) *protocol.ExtenderRecord {
	self.t.Helper()
	record, err := SignExtenderRecord(self.rootPrivateKey, &protocol.ExtenderRecordBody{
		PublicKey:     key,
		Addresses:     addresses,
		TcpPort:       443,
		UdpPort:       443,
		DnsPort:       53,
		DnsTld:        "x.example.",
		CountryCode:   "us",
		IssueTimeMs:   uint64(self.clock.Now().UnixMilli()),
		ExpireTimeMs:  uint64(self.clock.Now().Add(24 * time.Hour).UnixMilli()),
		NetworkHost:   testExtenderNetworkHost,
		DirectoryTier: uint32(tier),
	})
	if err != nil {
		self.t.Fatal(err)
	}
	return record
}

// A gated record released to this client at `ips`, with every carrier.
func (self *testBlockEvidence) released(ips ...string) ed25519.PublicKey {
	self.t.Helper()
	key := newTestExtenderKey(self.t)
	self.apply(key, ExtenderDirectoryTierGated, ips...)
	return key
}

func (self *testBlockEvidence) apply(key ed25519.PublicKey, tier int, ips ...string) {
	self.t.Helper()
	addresses := []*protocol.ExtenderAddress{}
	for _, ip := range ips {
		addresses = append(addresses, testExtenderAddress(ip))
	}
	record := self.sign(key, tier, addresses...)
	var err error
	if tier == ExtenderDirectoryTierGated {
		_, err = self.directory.ApplyReleaseRecord(record, self.directory.ResetGeneration())
	} else {
		_, err = self.directory.ApplyRecord(record, ExtenderSourceFeed)
	}
	if err != nil {
		self.t.Fatal(err)
	}
}

// One failed dial of the client's, of any kind.
func (self *testBlockEvidence) dialFailed(ip string) {
	self.directory.RecordFailure(netip.MustParseAddr(ip), ExtenderConnectModeTcpTls)
}

func (self *testBlockEvidence) look() ExtenderBlockEvidenceLook {
	return self.directory.BlockEvidenceLook(self.ipVersions)
}

// One attempt pass, every dial failing and the control route answering after
// it: what the sender does when the rule is met. Reports the suspects that had
// an attempt due.
func (self *testBlockEvidence) passFailed() int {
	self.t.Helper()
	startTime := self.clock.Now()
	suspects := self.look().AttemptSuspects
	failed := []ExtenderBlockSuspect{}
	for _, suspect := range suspects {
		outcome := self.directory.AttemptBlockSuspect(context.Background(), suspect, func(ctx context.Context, extenderConfig *ExtenderConfig) error {
			return errTestBlockDial
		})
		if outcome != ExtenderBlockAttemptFailed {
			self.t.Fatalf("an attempt whose every dial failed came to %s", outcome)
		}
		failed = append(failed, suspect)
	}
	self.directory.RecordBlockAttempts(startTime, failed)
	return len(suspects)
}

// Moves to when the next attempt is due and passes it.
func (self *testBlockEvidence) nextPassFailed() {
	self.t.Helper()
	nextAttemptTime := self.look().NextAttemptTime
	if nextAttemptTime.IsZero() {
		self.t.Fatal("no attempt is due")
	}
	if wait := nextAttemptTime.Sub(self.clock.Now()); 0 < wait {
		self.clock.advance(wait)
	}
	if self.passFailed() == 0 {
		self.t.Fatal("the attempt that was due has no suspect")
	}
}

func (self *testBlockEvidence) requireDue(what string, keys ...ed25519.PublicKey) {
	self.t.Helper()
	due := self.look().DuePublicKeys
	if len(due) != len(keys) {
		self.t.Fatalf("%s: %d records due, want %d", what, len(due), len(keys))
	}
	for i, key := range keys {
		if !slices.Equal(due[i], key) {
			self.t.Fatalf("%s: the wrong record is due at %d", what, i)
		}
	}
}

func (self *testBlockEvidence) requireSuspectCount(what string, count int) {
	self.t.Helper()
	if suspectCount := self.look().SuspectCount; suspectCount != count {
		self.t.Fatalf("%s: %d suspects, want %d", what, suspectCount, count)
	}
}

// The whole way to a report: a failed dial arms, three attempts half an hour
// apart fail, and only then is the record due.
func (self *testBlockEvidence) reportDue(ip string, key ed25519.PublicKey) {
	self.t.Helper()
	self.dialFailed(ip)
	for range self.settings.MinAttemptCount {
		self.nextPassFailed()
	}
	self.requireDue("after every attempt", key)
}

// Off is the default: failures keep no evidence and wake no one, and an attempt
// dials nothing.
func TestExtenderBlockEvidenceIsOffByDefault(t *testing.T) {
	harness := newTestBlockEvidence(t)
	harness.directory.SetBlockEvidence(nil)
	key := harness.released("192.0.2.1")
	update := harness.directory.BlockEvidenceMonitor().NotifyChannel()

	harness.dialFailed("192.0.2.1")

	if look := harness.look(); look.SuspectCount != 0 || !look.NextAttemptTime.IsZero() || 0 < len(look.DuePublicKeys) {
		t.Fatalf("evidence off produced %+v", look)
	}
	select {
	case <-update:
		t.Fatal("evidence off woke the sender")
	default:
	}
	outcome := harness.directory.AttemptBlockSuspect(
		context.Background(),
		ExtenderBlockSuspect{PublicKey: key, keyHex: fmt.Sprintf("%x", key), ipVersions: []int{4}},
		func(ctx context.Context, extenderConfig *ExtenderConfig) error {
			t.Error("evidence off dialed")
			return nil
		},
	)
	if outcome != ExtenderBlockAttemptAborted {
		t.Fatalf("an attempt with evidence off came to %s", outcome)
	}
	harness.directory.RecordBlockAttempts(harness.clock.Now(), nil)
	harness.directory.BlockEvidenceReported(key)
	harness.directory.BlockEvidencePathChanged()
}

// One failed dial is never a report, nor are many: only the attempts count,
// and they count one, two, three. A record is due only after the third, an hour
// after the first (rule 1 and 2).
func TestExtenderBlockEvidenceTakesThreeSpacedAttempts(t *testing.T) {
	harness := newTestBlockEvidence(t)
	key := harness.released("192.0.2.1")
	update := harness.directory.BlockEvidenceMonitor().NotifyChannel()

	armTime := harness.clock.Now()
	for range 30 {
		harness.dialFailed("192.0.2.1")
		harness.clock.advance(time.Second)
	}
	select {
	case <-update:
	default:
		t.Fatal("arming a suspect woke no one")
	}
	harness.requireDue("after many failed dials and no attempt")
	harness.requireSuspectCount("after many failed dials", 1)

	// the first attempt waits for the settle timeout, from the first failure
	harness.clock.advance(armTime.Add(harness.settings.AttemptSettleTimeout - time.Second).Sub(harness.clock.Now()))
	look := harness.look()
	if 0 < len(look.AttemptSuspects) {
		t.Fatal("an attempt is due before the settle timeout")
	}
	if !look.NextAttemptTime.Equal(armTime.Add(harness.settings.AttemptSettleTimeout)) {
		t.Fatalf("the first attempt is due at %s", look.NextAttemptTime)
	}

	harness.nextPassFailed()
	firstAttemptTime := harness.clock.Now()
	harness.requireDue("after one attempt")
	harness.nextPassFailed()
	harness.requireDue("after two attempts")
	if elapsed := harness.clock.Now().Sub(firstAttemptTime); elapsed != harness.settings.AttemptInterval {
		t.Fatalf("the second attempt was %s after the first", elapsed)
	}
	harness.nextPassFailed()
	harness.requireDue("after three attempts", key)
	if elapsed := harness.clock.Now().Sub(firstAttemptTime); elapsed < time.Hour {
		t.Fatalf("a record was due %s after its first attempt", elapsed)
	}
}

// An attempt too soon after the last says nothing new and does not count, so
// passes in a burst are not three attempts (rule 2).
func TestExtenderBlockEvidenceCountsOneAttemptPerInterval(t *testing.T) {
	harness := newTestBlockEvidence(t)
	harness.released("192.0.2.1")
	harness.dialFailed("192.0.2.1")
	harness.nextPassFailed()
	suspects := harness.directory.BlockEvidenceLook(harness.ipVersions).AttemptSuspects
	if 0 < len(suspects) {
		t.Fatal("an attempt is due right after one")
	}

	// a caller that records an attempt early anyway does not gain a count
	harness.clock.advance(harness.settings.AttemptInterval - time.Second)
	key := ""
	for keyHex := range harness.directory.blockEvidence.suspects {
		key = keyHex
	}
	stored := harness.directory.blockEvidence.suspects[key]
	soon := ExtenderBlockSuspect{keyHex: key, serial: stored.serial, addressKey: stored.addressKey}
	harness.directory.RecordBlockAttempts(harness.clock.Now(), []ExtenderBlockSuspect{soon, soon, soon})
	if stored.attemptCount != 1 {
		t.Fatalf("attempts within the interval made a count of %d", stored.attemptCount)
	}
}

// One attempt is every carrier of every address in the families the client
// dials, tcp first, each dial with the operator's probe budget (rule 2).
func TestExtenderBlockAttemptDialsEveryCarrierOfEveryAddress(t *testing.T) {
	for _, test := range []struct {
		name       string
		ipVersions []int
		want       []string
	}{
		{
			name:       "dual stack",
			ipVersions: []int{4, 6},
			want: []string{
				"192.0.2.1 tcptls", "192.0.2.1 quic", "192.0.2.1 dns",
				"2001:db8::1 tcptls", "2001:db8::1 quic", "2001:db8::1 dns",
			},
		},
		{
			name:       "ipv4 only",
			ipVersions: []int{4},
			want:       []string{"192.0.2.1 tcptls", "192.0.2.1 quic", "192.0.2.1 dns"},
		},
		{
			name:       "ipv6 only",
			ipVersions: []int{6},
			want:       []string{"2001:db8::1 tcptls", "2001:db8::1 quic", "2001:db8::1 dns"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			harness := newTestBlockEvidence(t)
			harness.ipVersions = test.ipVersions
			harness.released("192.0.2.1", "2001:db8::1")
			if test.ipVersions[0] == 4 {
				harness.dialFailed("192.0.2.1")
			} else {
				harness.dialFailed("2001:db8::1")
			}
			harness.clock.advance(harness.settings.AttemptSettleTimeout)
			suspects := harness.look().AttemptSuspects
			if len(suspects) != 1 {
				t.Fatalf("%d suspects to attempt", len(suspects))
			}
			dials := []string{}
			outcome := harness.directory.AttemptBlockSuspect(context.Background(), suspects[0], func(ctx context.Context, extenderConfig *ExtenderConfig) error {
				dials = append(dials, fmt.Sprintf("%s %s", extenderConfig.Ip, extenderConfig.Profile.ConnectMode))
				if !slices.Equal(extenderConfig.PublicKey, suspects[0].PublicKey) {
					t.Error("a dial did not check the record's own key")
				}
				deadline, ok := ctx.Deadline()
				if !ok || harness.settings.ProbeTimeout < time.Until(deadline) {
					t.Errorf("a dial had the budget %s", time.Until(deadline))
				}
				return errTestBlockDial
			})
			if outcome != ExtenderBlockAttemptFailed {
				t.Fatalf("the attempt came to %s", outcome)
			}
			if !slices.Equal(dials, test.want) {
				t.Fatalf("the attempt dialed %v", dials)
			}
		})
	}
}

// Any carrier of any address answering, or limiting the request, is a record
// the client can reach: the walk stops, the suspect goes, and the record is left
// alone for a while (rule 5).
func TestExtenderBlockAttemptAnsweredDropsTheSuspect(t *testing.T) {
	limited := &ExtenderLimitedError{RetryAfter: time.Minute}
	for _, test := range []struct {
		name      string
		answer    string
		answerErr error
		dials     int
	}{
		{name: "tcp answers", answer: "192.0.2.1 tcptls", dials: 1},
		{name: "quic answers after tcp failed", answer: "192.0.2.1 quic", dials: 2},
		{name: "dns answers last", answer: "192.0.2.1 dns", dials: 3},
		{name: "the other family answers", answer: "2001:db8::1 tcptls", dials: 4},
		{name: "a limit answers", answer: "192.0.2.1 quic", answerErr: limited, dials: 2},
		{name: "a wrapped limit answers", answer: "192.0.2.1 quic", answerErr: fmt.Errorf("dial: %w", limited), dials: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			harness := newTestBlockEvidence(t)
			harness.released("192.0.2.1", "2001:db8::1")
			harness.dialFailed("192.0.2.1")
			harness.clock.advance(harness.settings.AttemptSettleTimeout)
			suspects := harness.look().AttemptSuspects
			dials := 0
			outcome := harness.directory.AttemptBlockSuspect(context.Background(), suspects[0], func(ctx context.Context, extenderConfig *ExtenderConfig) error {
				dials += 1
				if fmt.Sprintf("%s %s", extenderConfig.Ip, extenderConfig.Profile.ConnectMode) == test.answer {
					return test.answerErr
				}
				return errTestBlockDial
			})
			if outcome != ExtenderBlockAttemptAnswered {
				t.Fatalf("the attempt came to %s", outcome)
			}
			if dials != test.dials {
				t.Fatalf("the attempt made %d dials", dials)
			}
			harness.requireSuspectCount("after an answer", 0)

			// a late record of the attempt counts for nothing
			harness.directory.RecordBlockAttempts(harness.clock.Now(), suspects)
			harness.requireSuspectCount("after a late record", 0)

			// quiet: another failed dial arms nothing until the quiet ends
			harness.dialFailed("192.0.2.1")
			harness.requireSuspectCount("within the quiet", 0)
			harness.clock.advance(harness.settings.AnsweredQuietTimeout)
			harness.dialFailed("192.0.2.1")
			harness.requireSuspectCount("after the quiet", 1)
		})
	}
}

// Any other dial of this client that the record answers does the same, however
// far the attempts had come, and an attempt in flight then counts for nothing
// (rule 5).
func TestExtenderBlockEvidenceAnotherDialAnsweringStartsOver(t *testing.T) {
	for _, test := range []struct {
		name   string
		answer func(directory *ExtenderDirectory, ip netip.Addr)
	}{
		{
			name: "success",
			answer: func(directory *ExtenderDirectory, ip netip.Addr) {
				directory.RecordSuccess(ip, ExtenderConnectModeQuic)
			},
		},
		{
			name: "limit",
			answer: func(directory *ExtenderDirectory, ip netip.Addr) {
				directory.RecordLimited(ip, time.Minute)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			harness := newTestBlockEvidence(t)
			harness.released("192.0.2.1", "2001:db8::1")
			harness.dialFailed("192.0.2.1")
			harness.nextPassFailed()
			harness.nextPassFailed()

			harness.clock.advance(harness.settings.AttemptInterval)
			inFlight := harness.look().AttemptSuspects
			// the other address answers, while the attempt is out
			test.answer(harness.directory, netip.MustParseAddr("2001:db8::1"))
			harness.requireSuspectCount("after an answer", 0)
			harness.directory.RecordBlockAttempts(harness.clock.Now(), inFlight)
			harness.requireDue("after the attempt in flight landed")

			// armed again after the quiet, with no count from before
			harness.clock.advance(harness.settings.AnsweredQuietTimeout)
			harness.dialFailed("192.0.2.1")
			harness.directory.RecordBlockAttempts(harness.clock.Now(), inFlight)
			if count := harness.look().SuspectCount; count != 1 {
				t.Fatalf("%d suspects after arming again", count)
			}
			harness.nextPassFailed()
			harness.nextPassFailed()
			harness.requireDue("two attempts after arming again")
		})
	}
}

// The operator's probe dials tcp alone, so a record with no tcp carrier on an
// address of the client's families has nothing to compare with: never a report,
// and no attempt either (rule 2).
func TestExtenderBlockEvidenceNeverJudgesARecordWithoutTcp(t *testing.T) {
	harness := newTestBlockEvidence(t)
	key := newTestExtenderKey(t)
	record := harness.sign(key, ExtenderDirectoryTierGated, testExtenderAddress("192.0.2.1", ExtenderCarrierQuic, ExtenderCarrierDns))
	if _, err := harness.directory.ApplyReleaseRecord(record, harness.directory.ResetGeneration()); err != nil {
		t.Fatal(err)
	}
	harness.dialFailed("192.0.2.1")
	harness.clock.advance(harness.settings.AttemptSettleTimeout)
	if look := harness.look(); look.SuspectCount != 0 || 0 < len(look.AttemptSuspects) || !look.NextAttemptTime.IsZero() {
		t.Fatalf("a record with no tcp carrier: %+v", look)
	}

	// tcp on one family only is not enough for a client that dials both
	dualKey := newTestExtenderKey(t)
	record = harness.sign(dualKey, ExtenderDirectoryTierGated,
		testExtenderAddress("192.0.2.2"),
		testExtenderAddress("2001:db8::2", ExtenderCarrierQuic),
	)
	if _, err := harness.directory.ApplyReleaseRecord(record, harness.directory.ResetGeneration()); err != nil {
		t.Fatal(err)
	}
	harness.dialFailed("192.0.2.2")
	harness.clock.advance(harness.settings.AttemptSettleTimeout)
	if look := harness.look(); len(look.AttemptSuspects) != 0 {
		t.Fatal("a dual stack client attempted a record with no tcp on one family")
	}
	harness.ipVersions = []int{4}
	harness.dialFailed("192.0.2.2")
	harness.clock.advance(harness.settings.AttemptSettleTimeout)
	if look := harness.look(); len(look.AttemptSuspects) != 1 {
		t.Fatal("an ipv4 client did not attempt a record with tcp on ipv4")
	}

	// a client that dials no family has nothing to judge
	harness.ipVersions = nil
	if look := harness.look(); look.SuspectCount != 0 {
		t.Fatal("a client that dials no family kept a suspect")
	}
}

// A path change drops every suspect with what its attempts found, an attempt in
// flight across it never counts, and the next attempt waits for the new path to
// settle. A failure during the change can arm, and no more (rule 4).
func TestExtenderBlockEvidencePathChangeStartsOver(t *testing.T) {
	harness := newTestBlockEvidence(t)
	key := harness.released("192.0.2.1")
	harness.dialFailed("192.0.2.1")
	harness.nextPassFailed()
	harness.nextPassFailed()

	harness.clock.advance(harness.settings.AttemptInterval)
	inFlight := harness.look().AttemptSuspects
	if len(inFlight) != 1 {
		t.Fatal("no attempt is due")
	}
	harness.directory.BlockEvidencePathChanged()
	harness.requireSuspectCount("after the path change", 0)
	harness.directory.RecordBlockAttempts(harness.clock.Now(), inFlight)
	harness.requireDue("after an attempt in flight across the change landed")

	// a failure of the change itself arms, and its attempt waits to settle
	pathChangeTime := harness.clock.Now()
	harness.dialFailed("192.0.2.1")
	harness.requireSuspectCount("after a failure during the change", 1)
	look := harness.look()
	if 0 < len(look.AttemptSuspects) {
		t.Fatal("an attempt is due right after the path change")
	}
	if !look.NextAttemptTime.Equal(pathChangeTime.Add(harness.settings.AttemptSettleTimeout)) {
		t.Fatalf("the next attempt is due at %s", look.NextAttemptTime)
	}
	// nothing from before carried over: it takes all three again
	harness.nextPassFailed()
	harness.requireDue("after one attempt on the new path")
	harness.nextPassFailed()
	harness.requireDue("after two attempts on the new path")
	harness.nextPassFailed()
	harness.requireDue("after three attempts on the new path", key)
}

// A record the client cannot verify, or that is not current, is never a suspect,
// and a suspect whose record stops being current is dropped, never reported
// (rule 1).
func TestExtenderBlockEvidenceOnlyJudgesACurrentVerifiedRecord(t *testing.T) {
	t.Run("a bare address", func(t *testing.T) {
		harness := newTestBlockEvidence(t)
		ip := netip.MustParseAddr("192.0.2.1")
		harness.directory.AddBootstrap(ip, ExtenderSourceDns)
		harness.dialFailed("192.0.2.1")
		harness.requireSuspectCount("an unverified address", 0)
		harness.directory.AddManual(netip.MustParseAddr("192.0.2.2"))
		harness.dialFailed("192.0.2.2")
		harness.requireSuspectCount("a manual address", 0)
	})
	t.Run("a manual address with a record", func(t *testing.T) {
		harness := newTestBlockEvidence(t)
		harness.directory.AddManual(netip.MustParseAddr("192.0.2.1"))
		harness.released("192.0.2.1")
		harness.dialFailed("192.0.2.1")
		harness.requireSuspectCount("a record over a manual address", 0)
	})
	t.Run("this extender's own record", func(t *testing.T) {
		key := newTestExtenderKey(t)
		clock := newTestClock()
		directory, rootPrivateKey := newTestExtenderDirectory(t, clock, nil)
		directory.KeepPublicKey(key)
		settings := DefaultExtenderBlockEvidenceSettings()
		settings.Enabled = true
		directory.SetBlockEvidence(settings)
		harness := &testBlockEvidence{t: t, clock: clock, directory: directory, rootPrivateKey: rootPrivateKey, settings: settings, ipVersions: []int{4}}
		harness.apply(key, ExtenderDirectoryTierGated, "192.0.2.1")
		harness.dialFailed("192.0.2.1")
		harness.requireSuspectCount("the extender's own record", 0)
	})
	t.Run("an expired record", func(t *testing.T) {
		harness := newTestBlockEvidence(t)
		harness.released("192.0.2.1")
		harness.clock.advance(25 * time.Hour)
		harness.dialFailed("192.0.2.1")
		harness.requireSuspectCount("an expired record", 0)
	})
	t.Run("a revoked record", func(t *testing.T) {
		harness := newTestBlockEvidence(t)
		key := harness.released("192.0.2.1")
		if _, err := harness.directory.ApplyRevocation(signTestRevocation(t, harness.rootPrivateKey, key, harness.clock.Now().Add(time.Second))); err != nil {
			t.Fatal(err)
		}
		harness.dialFailed("192.0.2.1")
		harness.requireSuspectCount("a revoked record", 0)
	})
	for _, test := range []struct {
		name string
		stop func(harness *testBlockEvidence, key ed25519.PublicKey)
	}{
		{
			name: "revoked",
			stop: func(harness *testBlockEvidence, key ed25519.PublicKey) {
				if _, err := harness.directory.ApplyRevocation(signTestRevocation(t, harness.rootPrivateKey, key, harness.clock.Now().Add(time.Second))); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "expired",
			stop: func(harness *testBlockEvidence, key ed25519.PublicKey) {
				harness.clock.advance(25 * time.Hour)
			},
		},
		{
			name: "no longer verifying under the root keys",
			stop: func(harness *testBlockEvidence, key ed25519.PublicKey) {
				rootSeed, err := NewExtenderKeySeed()
				if err != nil {
					t.Fatal(err)
				}
				otherPrivateKey, err := ExtenderPrivateKeyFromSeed(rootSeed)
				if err != nil {
					t.Fatal(err)
				}
				harness.directory.SetRootKeys(NewExtenderRootKeySet(otherPrivateKey.Public().(ed25519.PublicKey)))
			},
		},
		{
			name: "forgotten by an account change",
			stop: func(harness *testBlockEvidence, key ed25519.PublicKey) {
				harness.directory.ForgetGatedRecords()
			},
		},
	} {
		t.Run("a due record "+test.name, func(t *testing.T) {
			harness := newTestBlockEvidence(t)
			key := harness.released("192.0.2.1")
			harness.reportDue("192.0.2.1", key)
			test.stop(harness, key)
			harness.requireDue("after the record stopped being current")
			harness.requireSuspectCount("after the record stopped being current", 0)
		})
	}
}

// An open record is a suspect only once the connection path drew it, since the
// feed, the probes and the mesh hear of records the client never uses. A
// released one needs no draw (rule 1).
func TestExtenderBlockEvidenceOnlyJudgesARecordTheClientUses(t *testing.T) {
	harness := newTestBlockEvidence(t)
	drawnKey := newTestExtenderKey(t)
	harness.apply(drawnKey, ExtenderDirectoryTierOpen, "198.51.100.1")

	harness.dialFailed("198.51.100.1")
	harness.requireSuspectCount("an open record no draw named", 0)

	// the hold of that failure keeps it out of a draw, as in use
	harness.clock.advance(time.Hour)
	drawn := harness.directory.ConnectCandidates([]int{4}, 8, true, nil)
	if len(drawn[0]) != 1 {
		t.Fatalf("the draw was %v", drawn)
	}
	harness.dialFailed("198.51.100.1")
	harness.requireSuspectCount("an open record the connection path drew", 1)

	// a record the connection path never drew stays out, and the probe pass
	// reading it changes nothing
	otherKey := newTestExtenderKey(t)
	harness.apply(otherKey, ExtenderDirectoryTierOpen, "198.51.100.2")
	harness.directory.ProbeCandidates(4, 8, false)
	harness.directory.Candidates(4, 8)
	harness.dialFailed("198.51.100.2")
	harness.requireSuspectCount("an open record no draw named, again", 1)

	harness.released("192.0.2.1")
	harness.dialFailed("192.0.2.1")
	harness.requireSuspectCount("a released record", 2)
}

// The bounds: suspects kept, records per pass with gated first and then the
// oldest, and quiet records remembered.
func TestExtenderBlockEvidenceIsBounded(t *testing.T) {
	harness := newTestBlockEvidence(t)
	harness.settings.MaxSuspectCount = 4
	harness.settings.MaxAttemptSuspectCount = 3
	harness.directory.SetBlockEvidence(harness.settings)

	openKeys := []ed25519.PublicKey{}
	for i := range 4 {
		key := newTestExtenderKey(t)
		ip := fmt.Sprintf("198.51.100.%d", i+1)
		harness.apply(key, ExtenderDirectoryTierOpen, ip)
		openKeys = append(openKeys, key)
	}
	if drawn := harness.directory.ConnectCandidates([]int{4}, 8, true, nil); len(drawn[0]) != 4 {
		t.Fatalf("the draw was %v", drawn)
	}
	for i := range 4 {
		harness.dialFailed(fmt.Sprintf("198.51.100.%d", i+1))
		harness.clock.advance(time.Second)
	}
	harness.requireSuspectCount("at the cap", 4)

	// past the cap an open record is left out
	extraOpenKey := newTestExtenderKey(t)
	harness.apply(extraOpenKey, ExtenderDirectoryTierOpen, "198.51.100.9")
	if drawn := harness.directory.ConnectCandidates([]int{4}, 8, true, nil); len(drawn[0]) == 0 {
		t.Fatal("the draw was empty")
	}
	harness.dialFailed("198.51.100.9")
	harness.requireSuspectCount("an open record past the cap", 4)

	// a gated one takes the place of the newest open one
	gatedKey := harness.released("192.0.2.1")
	harness.clock.advance(time.Second)
	harness.dialFailed("192.0.2.1")
	harness.requireSuspectCount("a gated record past the cap", 4)

	harness.clock.advance(harness.settings.AttemptSettleTimeout)
	suspects := harness.look().AttemptSuspects
	if len(suspects) != harness.settings.MaxAttemptSuspectCount {
		t.Fatalf("a pass takes %d suspects", len(suspects))
	}
	if !slices.Equal(suspects[0].PublicKey, gatedKey) {
		t.Fatal("the gated suspect does not lead the pass")
	}
	if !slices.Equal(suspects[1].PublicKey, openKeys[0]) || !slices.Equal(suspects[2].PublicKey, openKeys[1]) {
		t.Fatal("the oldest suspects do not follow the gated one")
	}

	// a cap of gated suspects alone leaves another gated one out
	full := newTestBlockEvidence(t)
	full.settings.MaxSuspectCount = 1
	full.directory.SetBlockEvidence(full.settings)
	full.released("192.0.2.1")
	full.dialFailed("192.0.2.1")
	full.released("192.0.2.2")
	full.dialFailed("192.0.2.2")
	full.requireSuspectCount("a gated record past a cap of gated records", 1)

	// quiet records are bounded too, whichever ends first going first
	quiet := newTestBlockEvidence(t)
	firstKey := newTestExtenderKey(t)
	quiet.directory.BlockEvidenceReported(firstKey)
	for range extenderBlockMaxQuietCount + 8 {
		quiet.clock.advance(time.Second)
		quiet.directory.BlockEvidenceReported(newTestExtenderKey(t))
	}
	if count := len(quiet.directory.blockEvidence.quietUntilTimes); count != extenderBlockMaxQuietCount {
		t.Fatalf("%d quiet records", count)
	}
	if _, ok := quiet.directory.blockEvidence.quietUntilTimes[fmt.Sprintf("%x", []byte(firstKey))]; ok {
		t.Fatal("the quiet that ends first stayed")
	}
}

// A record too big to attempt is not attempted (rule 2's budget).
func TestExtenderBlockEvidenceBoundsTheDialsOfOneAttempt(t *testing.T) {
	harness := newTestBlockEvidence(t)
	addresses := []*protocol.ExtenderAddress{}
	for i := range extenderBlockMaxAttemptDialCount {
		addresses = append(addresses, testExtenderAddress(fmt.Sprintf("192.0.2.%d", i+1)))
	}
	key := newTestExtenderKey(t)
	if _, err := harness.directory.ApplyReleaseRecord(harness.sign(key, ExtenderDirectoryTierGated, addresses...), harness.directory.ResetGeneration()); err != nil {
		t.Fatal(err)
	}
	harness.dialFailed("192.0.2.1")
	harness.clock.advance(harness.settings.AttemptSettleTimeout)
	if look := harness.look(); look.SuspectCount != 0 {
		t.Fatal("a record past the dial budget stayed a suspect")
	}
}

// Attempts and reports move nothing the connection path reads: no hold, no
// count, no removal, no policy mode, no draw (a report never changes local trust
// or selection).
func TestExtenderBlockEvidenceLeavesSelectionAlone(t *testing.T) {
	harness := newTestBlockEvidence(t)
	key := harness.released("192.0.2.1")
	harness.directory.ConnectCandidates([]int{4}, 8, true, nil)
	snapshotKnown := func() ExtenderDirectoryEntry {
		for _, entry := range harness.directory.Snapshot().Entries {
			if entry.Ip == netip.MustParseAddr("192.0.2.1") {
				return *entry
			}
		}
		t.Fatal("the address is gone")
		return ExtenderDirectoryEntry{}
	}

	harness.dialFailed("192.0.2.1")
	before := snapshotKnown()
	beforePreference := harness.directory.GatedPreference()
	for range harness.settings.MinAttemptCount {
		harness.nextPassFailed()
	}
	harness.requireDue("after every attempt", key)
	harness.directory.BlockEvidenceReported(key)

	after := snapshotKnown()
	if after.FailureCount != before.FailureCount || after.SuccessCount != before.SuccessCount {
		t.Fatalf("the attempts moved the counts: %+v, then %+v", before, after)
	}
	if !after.LastFailureTime.Equal(before.LastFailureTime) || !after.LastSuccessTime.Equal(before.LastSuccessTime) {
		t.Fatalf("the attempts moved the last dial times: %+v, then %+v", before, after)
	}
	if afterPreference := harness.directory.GatedPreference(); afterPreference.Mode != beforePreference.Mode ||
		afterPreference.FallbackReason != beforePreference.FallbackReason {
		t.Fatalf("the attempts moved the policy: %+v, then %+v", beforePreference, afterPreference)
	}

	// the record is still drawn when its hold lapses, with the record kept
	harness.clock.advance(time.Hour)
	if drawn := harness.directory.ConnectCandidates([]int{4}, 8, true, nil); len(drawn[0]) != 1 || !slices.Equal(drawn[0][0].PublicKey, key) {
		t.Fatalf("a reported record is no longer drawn: %v", drawn)
	}
}

// A successful attempt leaves a hold as it was too: a record the attempt reached
// stays out of the draw until its hold lapses, rather than the attempt clearing
// it.
func TestExtenderBlockAttemptSuccessLeavesTheHold(t *testing.T) {
	harness := newTestBlockEvidence(t)
	harness.released("192.0.2.1")
	harness.dialFailed("192.0.2.1")
	harness.clock.advance(harness.settings.AttemptSettleTimeout)
	suspects := harness.look().AttemptSuspects
	outcome := harness.directory.AttemptBlockSuspect(context.Background(), suspects[0], func(ctx context.Context, extenderConfig *ExtenderConfig) error {
		return nil
	})
	if outcome != ExtenderBlockAttemptAnswered {
		t.Fatalf("the attempt came to %s", outcome)
	}
	if drawn := harness.directory.ConnectCandidates([]int{4}, 8, true, nil); len(drawn[0]) != 0 {
		t.Fatal("an attempt that was answered cleared the hold")
	}
}

// What is due is due while its last attempt is fresh. Past that another attempt
// has to pass first, and past MaxWindow the suspect goes (rule 6).
func TestExtenderBlockEvidenceReportsOnlyFreshEvidence(t *testing.T) {
	harness := newTestBlockEvidence(t)
	key := harness.released("192.0.2.1")
	harness.reportDue("192.0.2.1", key)

	harness.clock.advance(harness.settings.ReportFreshTimeout)
	harness.requireDue("at the fresh timeout", key)
	harness.clock.advance(time.Second)
	harness.requireDue("past the fresh timeout")
	look := harness.look()
	if len(look.AttemptSuspects) != 1 {
		t.Fatal("stale evidence asks for no attempt")
	}
	harness.passFailed()
	harness.requireDue("after another attempt", key)

	// the first attempt counted, and a window later the suspect is gone
	stale := newTestBlockEvidence(t)
	stale.released("192.0.2.1")
	stale.dialFailed("192.0.2.1")
	stale.nextPassFailed()
	stale.clock.advance(stale.settings.MaxWindow)
	stale.requireSuspectCount("past the window", 0)
	if look := stale.look(); !look.NextAttemptTime.IsZero() || 0 < len(look.DuePublicKeys) {
		t.Fatalf("past the window: %+v", look)
	}
}

// A report answered leaves the record alone for a day. A later failed dial arms
// it again, for three new attempts.
func TestExtenderBlockEvidenceReportedGoesQuiet(t *testing.T) {
	harness := newTestBlockEvidence(t)
	key := harness.released("192.0.2.1")
	harness.reportDue("192.0.2.1", key)
	harness.directory.BlockEvidenceReported(key)
	harness.requireDue("after the report")

	// a record refreshed within the quiet stays quiet
	harness.clock.advance(time.Hour)
	harness.apply(key, ExtenderDirectoryTierGated, "192.0.2.1")
	harness.dialFailed("192.0.2.1")
	harness.requireSuspectCount("within the quiet", 0)

	// a day of failures also removes an address that never answered (E1), and
	// the refresh that follows brings it back: the quiet is over by then
	harness.clock.advance(harness.settings.ReportedQuietTimeout)
	harness.dialFailed("192.0.2.1")
	harness.requireSuspectCount("once the removal policy took the address", 0)
	harness.clock.advance(time.Second)
	harness.apply(key, ExtenderDirectoryTierGated, "192.0.2.1")
	harness.dialFailed("192.0.2.1")
	harness.requireSuspectCount("after the quiet", 1)
	harness.requireDue("after the quiet")
}

// A routine endpoint change carries no evidence across: attempts on the old
// address say nothing about the new one, in either family (EXTENDER-UPDATE2
// §1.3, §1.4). What the record no longer names is not dialed.
func TestExtenderBlockEvidenceStartsOverOnAnEndpointChange(t *testing.T) {
	for _, test := range []struct {
		name   string
		oldIp  string
		newIp  string
		family int
	}{
		{name: "ipv4", oldIp: "192.0.2.1", newIp: "192.0.2.2", family: 4},
		{name: "ipv6", oldIp: "2001:db8::1", newIp: "2001:db8::2", family: 6},
	} {
		t.Run(test.name, func(t *testing.T) {
			harness := newTestBlockEvidence(t)
			harness.ipVersions = []int{test.family}
			key := harness.released(test.oldIp)
			harness.dialFailed(test.oldIp)
			harness.nextPassFailed()
			harness.nextPassFailed()

			// the next release names another address
			harness.clock.advance(harness.settings.AttemptInterval)
			harness.apply(key, ExtenderDirectoryTierGated, test.newIp)
			suspects := harness.look().AttemptSuspects
			if len(suspects) != 1 {
				t.Fatal("no attempt is due")
			}
			dials := []string{}
			outcome := harness.directory.AttemptBlockSuspect(context.Background(), suspects[0], func(ctx context.Context, extenderConfig *ExtenderConfig) error {
				dials = append(dials, extenderConfig.Ip.String())
				return errTestBlockDial
			})
			if outcome != ExtenderBlockAttemptFailed {
				t.Fatalf("the attempt came to %s", outcome)
			}
			if slices.Contains(dials, test.oldIp) || !slices.Contains(dials, test.newIp) {
				t.Fatalf("the attempt dialed %v", dials)
			}
			harness.directory.RecordBlockAttempts(harness.clock.Now(), suspects)
			harness.requireDue("one attempt on the new address")
			harness.nextPassFailed()
			harness.requireDue("two attempts on the new address")
			harness.nextPassFailed()
			harness.requireDue("three attempts on the new address", key)
		})
	}
}

// A record that changes under an attempt about to dial is not dialed: what it
// would find is about another address set.
func TestExtenderBlockAttemptAbortsOnAChangedRecord(t *testing.T) {
	harness := newTestBlockEvidence(t)
	key := harness.released("192.0.2.1")
	harness.dialFailed("192.0.2.1")
	harness.clock.advance(harness.settings.AttemptSettleTimeout)
	suspects := harness.look().AttemptSuspects

	harness.clock.advance(time.Second)
	harness.apply(key, ExtenderDirectoryTierGated, "192.0.2.2")
	outcome := harness.directory.AttemptBlockSuspect(context.Background(), suspects[0], func(ctx context.Context, extenderConfig *ExtenderConfig) error {
		t.Error("a suspect whose record changed was dialed")
		return nil
	})
	if outcome != ExtenderBlockAttemptAborted {
		t.Fatalf("the attempt came to %s", outcome)
	}
}

// A dial the caller ended says nothing about the extender (rule 3, 4): the
// attempt is aborted, not failed, before a dial and across one.
func TestExtenderBlockAttemptCanceledSaysNothing(t *testing.T) {
	harness := newTestBlockEvidence(t)
	harness.released("192.0.2.1")
	harness.dialFailed("192.0.2.1")
	harness.clock.advance(harness.settings.AttemptSettleTimeout)
	suspects := harness.look().AttemptSuspects

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	outcome := harness.directory.AttemptBlockSuspect(canceled, suspects[0], func(ctx context.Context, extenderConfig *ExtenderConfig) error {
		t.Error("a canceled attempt dialed")
		return nil
	})
	if outcome != ExtenderBlockAttemptAborted {
		t.Fatalf("a canceled attempt came to %s", outcome)
	}

	acrossCtx, acrossCancel := context.WithCancel(context.Background())
	defer acrossCancel()
	dials := 0
	outcome = harness.directory.AttemptBlockSuspect(acrossCtx, suspects[0], func(ctx context.Context, extenderConfig *ExtenderConfig) error {
		dials += 1
		acrossCancel()
		return ctx.Err()
	})
	if outcome != ExtenderBlockAttemptAborted || dials != 1 {
		t.Fatalf("an attempt canceled across a dial came to %s after %d dials", outcome, dials)
	}
	harness.requireSuspectCount("after a canceled attempt", 1)
}

// This host's memory budget refusing a dial is a local condition, not an extender
// that did not answer (E1, A11): the attempt says nothing, whichever carrier it
// was on.
func TestExtenderBlockAttemptMemoryRefusalSaysNothing(t *testing.T) {
	for _, refusedMode := range []ExtenderConnectMode{
		ExtenderConnectModeTcpTls,
		ExtenderConnectModeQuic,
		ExtenderConnectModeDns,
	} {
		t.Run(string(refusedMode), func(t *testing.T) {
			harness := newTestBlockEvidence(t)
			harness.released("192.0.2.1")
			harness.dialFailed("192.0.2.1")
			harness.clock.advance(harness.settings.AttemptSettleTimeout)
			suspects := harness.look().AttemptSuspects
			outcome := harness.directory.AttemptBlockSuspect(context.Background(), suspects[0], func(ctx context.Context, extenderConfig *ExtenderConfig) error {
				if extenderConfig.Profile.ConnectMode == refusedMode {
					return fmt.Errorf("dial: %w", errExtenderMemoryBudget)
				}
				return errTestBlockDial
			})
			if outcome != ExtenderBlockAttemptAborted {
				t.Fatalf("a memory refusal came to %s", outcome)
			}
			harness.requireSuspectCount("after a memory refusal", 1)
		})
	}
}

// An account change, an off and a reset forget everything: suspects, quiet
// records and what the connection path drew (the records of one identity are
// not the next one's).
func TestExtenderBlockEvidenceForgets(t *testing.T) {
	for _, test := range []struct {
		name   string
		forget func(directory *ExtenderDirectory, settings *ExtenderBlockEvidenceSettings)
	}{
		{
			name: "an account change",
			forget: func(directory *ExtenderDirectory, settings *ExtenderBlockEvidenceSettings) {
				directory.ForgetBlockEvidence()
			},
		},
		{
			name: "evidence off and on",
			forget: func(directory *ExtenderDirectory, settings *ExtenderBlockEvidenceSettings) {
				directory.SetBlockEvidence(nil)
				directory.SetBlockEvidence(settings)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			harness := newTestBlockEvidence(t)
			quietKey := harness.released("192.0.2.1")
			harness.reportDue("192.0.2.1", quietKey)
			harness.directory.BlockEvidenceReported(quietKey)
			harness.released("192.0.2.2")
			harness.dialFailed("192.0.2.2")
			openKey := newTestExtenderKey(t)
			harness.apply(openKey, ExtenderDirectoryTierOpen, "198.51.100.1")
			harness.directory.ConnectCandidates([]int{4}, 8, true, nil)

			test.forget(harness.directory, harness.settings)

			harness.requireSuspectCount("forgotten", 0)
			harness.dialFailed("198.51.100.1")
			harness.requireSuspectCount("an open record the draw named before", 0)
			harness.dialFailed("192.0.2.1")
			harness.requireSuspectCount("a record quiet before", 1)
		})
	}
	t.Run("a reset", func(t *testing.T) {
		harness := newTestBlockEvidence(t)
		harness.released("192.0.2.1")
		harness.dialFailed("192.0.2.1")
		harness.directory.Reset(harness.directory.RootKeys())
		if suspects := harness.directory.blockEvidence.suspects; len(suspects) != 0 {
			t.Fatalf("%d suspects after a reset", len(suspects))
		}
	})
}

// Through the strategy itself: a gated record it fails to dial is armed, an open
// record it drew and then fails to dial is armed, and one that answers is not. The
// strategy's draw is what makes an open record one the client uses.
func TestClientStrategyDialsArmBlockEvidence(t *testing.T) {
	clock := newTestClock()
	clientStrategy, directory, apply := newTestGatedStrategy(t, clock)
	settings := DefaultExtenderBlockEvidenceSettings()
	settings.Enabled = true
	directory.SetBlockEvidence(settings)
	apply("192.0.2.10", true, false)
	apply("198.51.100.1", false, false)
	gatedIp := netip.MustParseAddr("192.0.2.10")
	firstOpenIp := netip.MustParseAddr("198.51.100.1")
	secondOpenIp := netip.MustParseAddr("198.51.100.2")

	stateLock := sync.Mutex{}
	failing := map[netip.Addr]bool{gatedIp: true}
	observer := newTestGatedDialObserver(clock, func(ctx context.Context, dialer *clientDialer) *evalResult {
		fails := func() bool {
			stateLock.Lock()
			defer stateLock.Unlock()
			return failing[dialer.extenderConfig.Ip]
		}()
		if fails {
			err := errors.New("the dial failed")
			dialer.Update(ctx, err)
			return &evalResult{err: err}
		}
		// the second open record answers only once the first has failed: a win
		// cancels the race, and a loser canceled by it says nothing
		if dialer.extenderConfig.Ip == secondOpenIp {
			deadline := time.Now().Add(5 * time.Second)
			for directory.BlockEvidenceLook([]int{4}).SuspectCount < 2 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
		}
		dialer.Update(ctx, nil)
		return &evalResult{}
	})

	// the gated record fails, and the open one the fallback draws answers
	if selected := testGatedRequest(t, clientStrategy, observer); selected != firstOpenIp {
		t.Fatalf("the request went through %s", selected)
	}
	look := directory.BlockEvidenceLook([]int{4})
	if look.SuspectCount != 1 {
		t.Fatalf("%d suspects after the gated record failed", look.SuspectCount)
	}
	// that says nothing yet
	if 0 < len(look.DuePublicKeys) || 0 < len(look.AttemptSuspects) {
		t.Fatalf("a failed dial alone asked for %+v", look)
	}

	// the open record it drew fails too, and the next one answers
	func() {
		stateLock.Lock()
		defer stateLock.Unlock()
		failing[firstOpenIp] = true
	}()
	apply("198.51.100.2", false, false)
	if selected := testGatedRequest(t, clientStrategy, observer); selected != secondOpenIp {
		t.Fatalf("the request went through %s", selected)
	}
	if look := directory.BlockEvidenceLook([]int{4}); look.SuspectCount != 2 {
		t.Fatalf("%d suspects after the drawn open record failed", look.SuspectCount)
	}

	clock.advance(settings.AttemptSettleTimeout)
	look = directory.BlockEvidenceLook([]int{4})
	if len(look.AttemptSuspects) != 2 {
		t.Fatalf("%d suspects to attempt", len(look.AttemptSuspects))
	}
	dialed := []netip.Addr{}
	directory.AttemptBlockSuspect(context.Background(), look.AttemptSuspects[0], func(ctx context.Context, extenderConfig *ExtenderConfig) error {
		dialed = append(dialed, extenderConfig.Ip)
		return errors.New("the dial failed")
	})
	if len(dialed) == 0 || dialed[0] != gatedIp {
		t.Fatalf("the gated record does not lead the attempts: %v", dialed)
	}
}
