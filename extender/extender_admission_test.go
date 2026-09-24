// The admission limits of EXTENDER.md A12: the per-instance limit on distinct
// source subnets and the per-subnet limit on actions, both sliding token
// buckets under a fake clock; the exemptions of a signed header and of an
// unlimited source; the 429 with its Retry-After on every carrier; the close
// at accept past the refusal cap; and a hop's 429 through an NLayer chain.

package extender

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// A clock the admission windows read, moved by hand.
type testAdmissionClock struct {
	stateLock sync.Mutex
	now       time.Time
}

// A clock at a fixed instant.
func newTestAdmissionClock() *testAdmissionClock {
	return &testAdmissionClock{now: time.Unix(1_700_000_000, 0)}
}

// The instant the clock is at.
func (self *testAdmissionClock) Now() time.Time {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.now
}

// Moves the clock on.
func (self *testAdmissionClock) advance(elapsed time.Duration) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.now = self.now.Add(elapsed)
}

// A server with no listener, for judging admissions directly.
func newTestAdmissionServer(t *testing.T, configure func(settings *ExtenderSettings)) (*ExtenderServer, *testAdmissionClock) {
	t.Helper()
	clock := newTestAdmissionClock()
	settings := DefaultExtenderSettings()
	settings.AdmissionNow = clock.Now
	if configure != nil {
		configure(settings)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	server := NewExtenderServer(ctx, nil, []string{"dest.example"}, nil, &net.Dialer{}, settings)
	t.Cleanup(server.Close)
	return server, clock
}

// The source address of the i-th distinct /56 in the documentation prefix.
func testAdmissionSubnetSource(i int) string {
	return fmt.Sprintf("[2001:db8:%x:%x00::1]:443", i/256, i%256)
}

// The 1000th distinct subnet of a minute is admitted and the 1001st is not,
// under the default per-instance limit; a subnet already admitted in the
// window is not a new one; and the limit slides with the clock.
func TestExtenderAdmissionLimitsDistinctSubnetsPerMinute(t *testing.T) {
	server, clock := newTestAdmissionServer(t, nil)
	for i := 0; i < 1000; i += 1 {
		if limit, _ := server.admitAction(testAdmissionSubnetSource(i), false); limit != extenderAdmitted {
			t.Fatalf("subnet %d was refused by %d", i+1, limit)
		}
	}
	limit, retryAfter := server.admitAction(testAdmissionSubnetSource(1000), false)
	if limit != extenderLimitedBySubnets || retryAfter <= 0 {
		t.Fatalf("the 1001st subnet got %d, retry after %s", limit, retryAfter)
	}
	// the first subnet is not new within the window, so its next action is
	// judged by its own limit alone
	if limit, _ := server.admitAction(testAdmissionSubnetSource(0), false); limit != extenderAdmitted {
		t.Fatalf("an admitted subnet was refused as a new one: %d", limit)
	}
	// a token of the instance limit returns every 60 ms
	clock.advance(60 * time.Millisecond)
	if limit, _ := server.admitAction(testAdmissionSubnetSource(1000), false); limit != extenderAdmitted {
		t.Fatalf("the refilled token was not granted: %d", limit)
	}
	if limit, _ := server.admitAction(testAdmissionSubnetSource(1001), false); limit != extenderLimitedBySubnets {
		t.Fatalf("a second subnet got the one refilled token: %d", limit)
	}
	stats := server.AdmissionStats()
	if stats.LimitedBySubnetsCount != 2 || stats.LimitedBySourceCount != 0 {
		t.Fatalf("stats = %+v", stats)
	}
}

// The 9th action of one subnet in a minute is refused under the default
// per-subnet limit, one token returns every 7.5 s, and a quiet ten minutes
// banks no more than the burst.
func TestExtenderAdmissionLimitsActionsPerSubnet(t *testing.T) {
	server, clock := newTestAdmissionServer(t, nil)
	source := testAdmissionSubnetSource(0)
	// another address in the same /56 is the same subnet
	sameSubnet := "[2001:db8:0:ff::2]:1234"
	for i := 0; i < 8; i += 1 {
		address := source
		if i%2 == 1 {
			address = sameSubnet
		}
		if limit, _ := server.admitAction(address, false); limit != extenderAdmitted {
			t.Fatalf("action %d was refused by %d", i+1, limit)
		}
	}
	if limit, _ := server.admitAction(source, false); limit != extenderLimitedBySource {
		t.Fatalf("the 9th action got %d", limit)
	}
	clock.advance(7500 * time.Millisecond)
	if limit, _ := server.admitAction(source, false); limit != extenderAdmitted {
		t.Fatalf("the refilled action was refused: %d", limit)
	}
	if limit, _ := server.admitAction(source, false); limit != extenderLimitedBySource {
		t.Fatalf("a second action got the one refilled token: %d", limit)
	}
	clock.advance(10 * time.Minute)
	for i := 0; i < 8; i += 1 {
		if limit, _ := server.admitAction(source, false); limit != extenderAdmitted {
			t.Fatalf("after a quiet window action %d was refused: %d", i+1, limit)
		}
	}
	if limit, _ := server.admitAction(source, false); limit != extenderLimitedBySource {
		t.Fatalf("a quiet window banked more than the burst: %d", limit)
	}
	// another subnet has its own budget
	if limit, _ := server.admitAction(testAdmissionSubnetSource(1), false); limit != extenderAdmitted {
		t.Fatalf("another subnet was refused: %d", limit)
	}
	if stats := server.AdmissionStats(); stats.LimitedBySourceCount != 3 || stats.LimitedBySubnetsCount != 0 {
		t.Fatalf("stats = %+v", stats)
	}
}

// A header signed with one of the extender's secrets is spared the per-subnet
// limit and still counts toward the per-instance one.
func TestExtenderAdmissionExemptsASignedHeaderFromTheSubnetLimitOnly(t *testing.T) {
	server, _ := newTestAdmissionServer(t, func(settings *ExtenderSettings) {
		settings.AdmissionSubnetsPerMinute = 2
	})
	for i := 0; i < 20; i += 1 {
		if limit, _ := server.admitAction(testAdmissionSubnetSource(0), true); limit != extenderAdmitted {
			t.Fatalf("signed action %d was refused by %d", i+1, limit)
		}
	}
	if limit, _ := server.admitAction(testAdmissionSubnetSource(1), false); limit != extenderAdmitted {
		t.Fatalf("the second subnet was refused: %d", limit)
	}
	// the signed subnet took one of the two
	if limit, _ := server.admitAction(testAdmissionSubnetSource(2), true); limit != extenderLimitedBySubnets {
		t.Fatalf("a third subnet, signed, got %d", limit)
	}
}

// A source inside an unlimited prefix is admitted past both limits, judged on
// its address, and counted apart; a source outside is limited as before.
func TestExtenderAdmissionUnlimitedSources(t *testing.T) {
	server, _ := newTestAdmissionServer(t, func(settings *ExtenderSettings) {
		settings.AdmissionSubnetsPerMinute = 1
		settings.AdmissionActionsPerSubnetPerMinute = 1
		settings.AdmissionUnlimitedSources = []netip.Prefix{
			netip.MustParsePrefix("2001:db8:ffff::/48"),
			netip.MustParsePrefix("198.51.100.0/24"),
			// a v4 prefix in its mapped form names the v4 prefix
			netip.MustParsePrefix("::ffff:203.0.113.0/120"),
		}
	})
	unlimitedSources := []string{
		"[2001:db8:ffff:1::1]:443",
		"198.51.100.200:443",
		"203.0.113.9:443",
		// a mapped source is judged unmapped
		"[::ffff:198.51.100.7]:443",
	}
	for _, source := range unlimitedSources {
		for i := 0; i < 20; i += 1 {
			if limit, _ := server.admitAction(source, false); limit != extenderAdmitted {
				t.Fatalf("unlimited %s action %d was refused by %d", source, i+1, limit)
			}
		}
	}
	if stats := server.AdmissionStats(); stats.UnlimitedCount != int64(20*len(unlimitedSources)) {
		t.Fatalf("stats = %+v, expected %d unlimited", stats, 20*len(unlimitedSources))
	}
	// outside the prefixes: one subnet, one action
	if limit, _ := server.admitAction("[2001:db8:1::1]:443", false); limit != extenderAdmitted {
		t.Fatalf("the first limited source was refused: %d", limit)
	}
	if limit, _ := server.admitAction("[2001:db8:1::1]:443", false); limit != extenderLimitedBySource {
		t.Fatalf("a limited source's second action got %d", limit)
	}
	if limit, _ := server.admitAction("[2001:db8:2::1]:443", false); limit != extenderLimitedBySubnets {
		t.Fatalf("a second limited subnet got %d", limit)
	}
	// the unlimited sources took nothing from the instance limit and left no
	// entry behind
	if stats := server.AdmissionStats(); stats.LimitedBySourceCount != 1 || stats.LimitedBySubnetsCount != 1 {
		t.Fatalf("stats = %+v", stats)
	}
	server.admission.stateLock.Lock()
	entryCount := len(server.admission.subnetEntries)
	server.admission.stateLock.Unlock()
	if entryCount != 2 {
		t.Fatalf("%d subnet entries, expected the two limited ones", entryCount)
	}
}

// The Retry-After of a refusal is whole seconds drawn within the bounds.
func TestExtenderAdmissionRetryAfterIsWithinItsRange(t *testing.T) {
	server, _ := newTestAdmissionServer(t, nil)
	seen := map[time.Duration]bool{}
	for i := 0; i < 200; i += 1 {
		retryAfter := server.admissionRetryAfter()
		if retryAfter < 15*time.Second || 60*time.Second < retryAfter || retryAfter%time.Second != 0 {
			t.Fatalf("retry after %s", retryAfter)
		}
		seen[retryAfter] = true
	}
	if len(seen) < 10 {
		t.Fatalf("%d distinct retry afters in 200 draws", len(seen))
	}
	fixed, _ := newTestAdmissionServer(t, func(settings *ExtenderSettings) {
		settings.AdmissionRetryAfterMin = 20 * time.Second
		settings.AdmissionRetryAfterMax = 20 * time.Second
	})
	if retryAfter := fixed.admissionRetryAfter(); retryAfter != 20*time.Second {
		t.Fatalf("a fixed range drew %s", retryAfter)
	}
}

// A subnet past its refusals is closed at accept until a refusal refills; no
// other subnet is.
func TestExtenderAdmissionClosesASubnetPastItsRefusalsAtAccept(t *testing.T) {
	server, clock := newTestAdmissionServer(t, func(settings *ExtenderSettings) {
		settings.AdmissionActionsPerSubnetPerMinute = 1
		settings.AdmissionRefusalsPerSubnetPerMinute = 2
	})
	// two /56s: they differ in the high byte of the fourth group
	remoteAddr := net.TCPAddrFromAddrPort(netip.MustParseAddrPort("[2001:db8:0:100::7]:4000"))
	otherAddr := net.TCPAddrFromAddrPort(netip.MustParseAddrPort("[2001:db8:0:200::7]:4000"))
	source := remoteAddr.String()
	if limit, _ := server.admitAction(source, false); limit != extenderAdmitted {
		t.Fatal("the first action was refused")
	}
	for i := 0; i < 2; i += 1 {
		if server.closedAtAccept(remoteAddr) {
			t.Fatalf("closed at accept after %d refusals", i)
		}
		if limit, _ := server.admitAction(source, false); limit != extenderLimitedBySource {
			t.Fatalf("refusal %d got %d", i+1, limit)
		}
	}
	if !server.closedAtAccept(remoteAddr) {
		t.Fatal("a subnet past its refusals was not closed at accept")
	}
	if server.closedAtAccept(otherAddr) {
		t.Fatal("another subnet was closed at accept")
	}
	clock.advance(30 * time.Second)
	if server.closedAtAccept(remoteAddr) {
		t.Fatal("a refilled refusal still closed the subnet at accept")
	}
}

// The subnet table holds its bound: once full of subnets of the window, a new
// one is refused without an entry, and once the window has passed the old
// entries, now indistinguishable from none, make room again.
func TestExtenderAdmissionTableIsBounded(t *testing.T) {
	server, clock := newTestAdmissionServer(t, func(settings *ExtenderSettings) {
		settings.AdmissionSubnetsPerMinute = 5000
	})
	maxSubnetCount := max(DefaultExtenderSettings().AdmissionMinSubnetCount, 4*5000)
	for i := 0; i < maxSubnetCount; i += 1 {
		server.admitAction(testAdmissionSubnetSource(i), false)
	}
	if limit, _ := server.admitAction(testAdmissionSubnetSource(maxSubnetCount), false); limit != extenderLimitedBySubnets {
		t.Fatalf("a subnet past a full table got %d", limit)
	}
	clock.advance(2 * time.Minute)
	if limit, _ := server.admitAction(testAdmissionSubnetSource(maxSubnetCount+1), false); limit != extenderAdmitted {
		t.Fatalf("a subnet after the window got %d", limit)
	}
	server.admission.stateLock.Lock()
	entryCount := len(server.admission.subnetEntries)
	server.admission.stateLock.Unlock()
	if maxSubnetCount < entryCount {
		t.Fatalf("%d entries past the bound of %d", entryCount, maxSubnetCount)
	}
}

// An open fixture whose per-subnet limit is `actions` a minute, under a fake
// clock. Every client of a test is the one loopback subnet.
func newTestAdmissionFixture(
	t *testing.T,
	actions int,
	configure func(settings *ExtenderSettings),
) (*extenderFixture, *testAdmissionClock) {
	t.Helper()
	clock := newTestAdmissionClock()
	fixture := newExtenderFixtureWithSecrets(t, "127.0.0.1", nil, func(settings *ExtenderSettings) {
		settings.AdmissionActionsPerSubnetPerMinute = actions
		settings.AdmissionNow = clock.Now
		if configure != nil {
			configure(settings)
		}
	})
	return fixture, clock
}

// One forward request on a carrier of a fixture, closed at once.
func dialTestAdmission(fixture *extenderFixture, carrier string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	conn, _, err := connect.DialExtender(ctx, fixture.connectSettings(), fixture.extenderConfig(carrier), &connect.ExtenderDial{
		DestinationHost: "dest.example",
		DestinationPort: 443,
	})
	if conn != nil {
		conn.Close()
	}
	return err
}

// Over the per-subnet limit every carrier answers 429 with a Retry-After in
// range, which the client reads as a limit and not a refusal, and the status
// counts it by limit.
func TestExtenderAnswersALimitWith429OnEveryCarrier(t *testing.T) {
	for _, carrier := range testProbeCarriers {
		fixture, _ := newTestAdmissionFixture(t, 2, nil)
		for i := 0; i < 2; i += 1 {
			if err := dialTestAdmission(fixture, carrier); err != nil {
				t.Fatalf("%s: action %d: %v", carrier, i+1, err)
			}
		}
		err := dialTestAdmission(fixture, carrier)
		var limitedErr *connect.ExtenderLimitedError
		if !errors.As(err, &limitedErr) {
			t.Fatalf("%s: the third action got %v, expected a limit", carrier, err)
		}
		if limitedErr.RetryAfter < 15*time.Second || 60*time.Second < limitedErr.RetryAfter {
			t.Fatalf("%s: retry after %s", carrier, limitedErr.RetryAfter)
		}
		var refusedErr *connect.ExtenderRefusedError
		if errors.As(err, &refusedErr) {
			t.Fatalf("%s: a limit read as a refusal", carrier)
		}
		if _, ok := nextNLayerError(fixture, "admission source"); !ok {
			t.Fatalf("%s: the limit was not attributed", carrier)
		}
		if stats := fixture.server.AdmissionStats(); stats.LimitedBySourceCount != 1 {
			t.Fatalf("%s: stats = %+v", carrier, stats)
		}
	}
}

// The 429 itself is the ordinary answer: no body, a Retry-After in seconds,
// and the connection closed after it.
func TestExtenderLimitAnswerHasNoBody(t *testing.T) {
	fixture, _ := newTestAdmissionFixture(t, 1, nil)
	if err := dialTestAdmission(fixture, connect.ExtenderCarrierTcp); err != nil {
		t.Fatal(err)
	}
	client := newRawExtenderHttpClient(fixture, nil)
	defer client.CloseIdleConnections()
	request, err := http.NewRequest(
		http.MethodPost,
		"https://"+testServerName+"/",
		bytes.NewReader(testExtenderHeaderBytes(t, "dest.example", 443, "")),
	)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", connect.ExtenderContentType)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d", response.StatusCode)
	}
	if 0 < len(body) {
		t.Fatalf("the 429 carried a body of %d bytes", len(body))
	}
	seconds, err := strconv.Atoi(response.Header.Get("Retry-After"))
	if err != nil || seconds < 15 || 60 < seconds {
		t.Fatalf("Retry-After = %q", response.Header.Get("Retry-After"))
	}
	if !response.Close {
		t.Fatal("the connection was not closed after the 429")
	}
}

// A header signed with the extender's secret is admitted past the per-subnet
// limit on the wire too.
func TestExtenderAdmitsASignedHeaderPastTheSubnetLimit(t *testing.T) {
	clock := newTestAdmissionClock()
	fixture := newExtenderFixture(t, "127.0.0.1", func(settings *ExtenderSettings) {
		settings.AdmissionActionsPerSubnetPerMinute = 1
		settings.AdmissionNow = clock.Now
	})
	for i := 0; i < 5; i += 1 {
		if err := dialTestAdmission(fixture, connect.ExtenderCarrierTcp); err != nil {
			t.Fatalf("signed action %d: %v", i+1, err)
		}
	}
	if stats := fixture.server.AdmissionStats(); stats.LimitedBySourceCount != 0 {
		t.Fatalf("stats = %+v", stats)
	}
}

// Past its refusals a subnet is closed at accept, before any handshake, on tcp
// and on quic; the handshake counter of the fixture sees nothing for it.
func TestExtenderClosesAtAcceptPastTheRefusals(t *testing.T) {
	fixture, _ := newTestAdmissionFixture(t, 1, func(settings *ExtenderSettings) {
		settings.AdmissionRefusalsPerSubnetPerMinute = 2
	})
	if err := dialTestAdmission(fixture, connect.ExtenderCarrierTcp); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i += 1 {
		var limitedErr *connect.ExtenderLimitedError
		if err := dialTestAdmission(fixture, connect.ExtenderCarrierTcp); !errors.As(err, &limitedErr) {
			t.Fatalf("refusal %d got %v", i+1, err)
		}
	}
	// the handshakes so far are the fixture's; none may follow
	for {
		select {
		case <-fixture.serverNames:
			continue
		default:
		}
		break
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", fixture.authority(fixture.tcpPort))
	if err != nil {
		t.Fatal(err)
	}
	tlsConn := tls.Client(conn, &tls.Config{ServerName: testServerName, InsecureSkipVerify: true})
	if err := tlsConn.HandshakeContext(ctx); err == nil {
		t.Fatal("a subnet past its refusals completed a tls handshake")
	}
	conn.Close()

	var limitedErr *connect.ExtenderLimitedError
	err = dialTestAdmission(fixture, connect.ExtenderCarrierQuic)
	if err == nil || errors.As(err, &limitedErr) {
		t.Fatalf("a quic dial past the refusals got %v, expected a refused connection", err)
	}
	select {
	case serverName := <-fixture.serverNames:
		t.Fatalf("a handshake was terminated for %q past the refusals", serverName)
	default:
	}
}

// A hop's 429 limits the hop without holding it and the front turns to its
// other hop; once every hop is limited, the front answers its own client 429
// with the shortest backoff, for a forward before its response and for a
// relayed probe after its hop dial.
func TestNLayerLimitedHop(t *testing.T) {
	// the hop that limits: one action a minute, which its first direct dial
	// spends, since the front's hop dials leave the same loopback subnet
	limitedHop, _ := newTestAdmissionFixture(t, 1, nil)
	if err := dialTestAdmission(limitedHop, connect.ExtenderCarrierTcp); err != nil {
		t.Fatal(err)
	}
	openHop := newExtenderFixture(t, "127.0.0.1", nil)
	front := newExtenderFixture(t, "127.0.0.1", func(settings *ExtenderSettings) {
		settings.NLayerHops = []*connect.ExtenderConfig{
			limitedHop.extenderConfig(connect.ExtenderCarrierTcp),
			openHop.extenderConfig(connect.ExtenderCarrierTcp),
		}
		// both hops are tried within one connection
		settings.NLayerAttempts = 2
	})
	connectSettings := nlayerConnectSettings(limitedHop, openHop)
	for i := 0; front.server.NLayerStats()[0].LimitedCount == 0; i += 1 {
		if 32 <= i {
			t.Fatal("the limited hop was never picked")
		}
		if _, err := getThroughNLayer(front, connect.ExtenderCarrierTcp, connectSettings); err != nil {
			t.Fatalf("dial %d: %v; %s", i, err, nlayerErrorsOf(front, limitedHop, openHop))
		}
	}
	hopStats := front.server.NLayerStats()
	if hopStats[0].FailedCount != 0 || !hopStats[0].HeldUntil.IsZero() || hopStats[0].LimitedUntil.IsZero() {
		t.Fatalf("stats = %+v, expected the hop limited and not held", hopStats[0])
	}
	// while it is limited, every connection goes to the other hop
	relaysBefore := hopStats[1].RelayCount
	for i := 0; i < 4; i += 1 {
		if _, err := getThroughNLayer(front, connect.ExtenderCarrierTcp, connectSettings); err != nil {
			t.Fatalf("dial during the limit: %v", err)
		}
	}
	if hopStats := front.server.NLayerStats(); hopStats[1].RelayCount != relaysBefore+4 || hopStats[0].LimitedCount != 1 {
		t.Fatalf("stats = %+v, expected every connection on the open hop", hopStats)
	}

	// a front whose only hop is limited
	lonely := newExtenderFixture(t, "127.0.0.1", func(settings *ExtenderSettings) {
		settings.NLayerHops = []*connect.ExtenderConfig{limitedHop.extenderConfig(connect.ExtenderCarrierTcp)}
	})
	// the first forward is answered before the hop dial, so it can only close
	if _, err := getThroughNLayer(lonely, connect.ExtenderCarrierTcp, limitedHop.connectSettings()); err == nil {
		t.Fatal("a forward through a limited hop reached the destination")
	}
	waitForNLayer(t, "the lonely front to limit its hop", func() bool {
		return lonely.server.NLayerStats()[0].LimitedCount == 1
	})
	remaining := time.Until(lonely.server.NLayerStats()[0].LimitedUntil)
	err := dialTestAdmission(lonely, connect.ExtenderCarrierTcp)
	var limitedErr *connect.ExtenderLimitedError
	if !errors.As(err, &limitedErr) {
		t.Fatalf("a forward through a front with every hop limited got %v, expected a limit", err)
	}
	if limitedErr.RetryAfter <= 0 || remaining+time.Second < limitedErr.RetryAfter {
		t.Fatalf("the front's retry after %s, the hop's remaining backoff %s", limitedErr.RetryAfter, remaining)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	conn, _, err := connect.DialExtender(ctx, lonely.connectSettings(), lonely.extenderConfig(connect.ExtenderCarrierTcp), &connect.ExtenderDial{
		Service: connect.ExtenderServiceProbe,
	})
	if conn != nil {
		conn.Close()
	}
	if !errors.As(err, &limitedErr) {
		t.Fatalf("a probe through a front with every hop limited got %v, expected a limit", err)
	}
}

// The prefix widths a source's subnet is taken at are settings: a wider v4
// prefix puts two /29s in one subnet, and a width outside the family's range
// is the default, never a disabled limit.
func TestExtenderAdmissionPrefixWidthsAreSettings(t *testing.T) {
	cases := []struct {
		ipv4PrefixBitCount int
		// whether 198.51.100.1 and 198.51.100.100 are one subnet
		oneSubnet bool
	}{
		{ipv4PrefixBitCount: 29, oneSubnet: false},
		{ipv4PrefixBitCount: 24, oneSubnet: true},
		{ipv4PrefixBitCount: 0, oneSubnet: false},
		{ipv4PrefixBitCount: 33, oneSubnet: false},
	}
	for _, c := range cases {
		server, _ := newTestAdmissionServer(t, func(settings *ExtenderSettings) {
			settings.AdmissionSubnetsPerMinute = 0
			settings.AdmissionActionsPerSubnetPerMinute = 1
			settings.AdmissionIpv4PrefixBitCount = c.ipv4PrefixBitCount
		})
		if limit, _ := server.admitAction("198.51.100.1:443", false); limit != extenderAdmitted {
			t.Fatalf("width %d: the first action was refused by %d", c.ipv4PrefixBitCount, limit)
		}
		limit, _ := server.admitAction("198.51.100.100:443", false)
		if oneSubnet := limit == extenderLimitedBySource; oneSubnet != c.oneSubnet {
			t.Errorf("width %d: the two sources are one subnet %t, expected %t", c.ipv4PrefixBitCount, oneSubnet, c.oneSubnet)
		}
	}
}
