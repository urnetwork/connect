package connect

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect/protocol"
)

// Activation loop tests (EXTENDER.md C2, G3).
//
// The operator is two httptest servers, one per family loopback, exactly as the
// api-v4 and api-v6 hosts an activation is posted to. The caller address check
// runs against a third server, so the loop's own hello is the only traffic
// there and can be used as a pass barrier: the loop is single threaded, so a
// hello of pass n+1 proves pass n ran to completion.
//
// Nothing here sleeps. The cadence decisions all read the fake clock, and the
// loop is woken between clock steps by a directory change, which is a wake it
// already selects on.

// The addresses the fixture operator publishes for each family.
const (
	testActivateIpv4 = "198.51.100.7"
	testActivateIpv6 = "2001:db8::7"
	// another extender, returned in the bootstrap sample (C2, D6)
	testActivateBootstrapIpv4 = "198.51.100.9"
)

// One activation post as the operator saw it.
type testActivatePost struct {
	ipVersion int
	args      *ExtenderActivateArgs
	// the fake clock at the post, which is what the cadence assertions read
	postTime time.Time
}

// How the fixture operator answers an activation it does not refuse.
type testActivateAnswer int

const (
	// the signed activation of the family the post arrived on
	testActivateAnswerActivated testActivateAnswer = iota
	// an http error, which reaches the extender as a failed request
	testActivateAnswerUnavailable
	// a json null, which decodes to no activation result at all
	testActivateAnswerNull
	// the signed activation without the family or the address, which leaves
	// an activation through the plain api url with no family to record
	testActivateAnswerNoFamily
)

// The body of the unavailable answer, which the failed request's error carries.
const testActivateUnavailable = "the operator is unavailable"

// testActivateOperator serves `/network/extender-activate` and `/hello` for
// both families (C2, C7).
type testActivateOperator struct {
	clock          *testClock
	rootPrivateKey ed25519.PrivateKey
	bootstrapKey   ed25519.PublicKey

	v4Url    string
	v6Url    string
	helloUrl string

	posts  chan *testActivatePost
	hellos chan struct{}

	stateLock     sync.Mutex
	clientAddress string
	// non-empty refuses every activation with this message
	refusal string
	// non-empty refuses every activation of that family with this message,
	// ahead of `refusal`
	familyRefusals map[int]string
	// the `retry_after_seconds` a refusal carries, 0 for none (connect#230)
	retryAfterSeconds int
	// the `refusal` a refusal carries (C2c), "" for none, as an operator that
	// predates the field sends
	refusalCode string
	// the `probe_steps` a refusal carries (C2b), none as an operator that
	// predates the field sends
	probeSteps []*ExtenderActivateProbeStep
	// the `refused_carriers` an activation carries (C2a), none as an operator
	// that predates the field sends
	refusedCarriers []*ExtenderActivateCarrierRefusal
	// answers as an operator that predates the webrtc carrier (EXTENDER.md
	// S8): a list naming a carrier it does not know, or none with tcp, is
	// refused before anything is probed, in that operator's words
	predatesWebRtc bool
	// the address an activation of a family is answered with, by family; the
	// family's fixed address while unset
	publishedIps map[int]string
	postCounts   map[int]int
	// how an activation that is not refused is answered
	answer testActivateAnswer
	// while set, an activation is answered only once this closes, or its caller goes
	answerHold chan struct{}
	// the addresses this extender has activated, by family. One record names
	// every one of them, as the operator's does from its address rows (C1, C2).
	activatedIps map[int]string
	// keeps each signed record newer than the last, which is what the newest
	// wins rule of B5 needs when two activations land in the same instant
	issueSerial int
}

func newTestActivateOperator(
	t *testing.T,
	clock *testClock,
	rootPrivateKey ed25519.PrivateKey,
) *testActivateOperator {
	t.Helper()
	operator := &testActivateOperator{
		clock:          clock,
		rootPrivateKey: rootPrivateKey,
		bootstrapKey:   newTestExtenderKey(t),
		posts:          make(chan *testActivatePost, 64),
		hellos:         make(chan struct{}, 64),
		clientAddress:  testActivateIpv4 + ":41001",
		familyRefusals: map[int]string{},
		publishedIps:   map[int]string{},
		postCounts:     map[int]int{},
		activatedIps:   map[int]string{},
	}
	v4Server := newFamilyHttptestServer(t, 4, operator.familyHandler(4))
	v6Server := newFamilyHttptestServer(t, 6, operator.familyHandler(6))
	helloServer := newFamilyHttptestServer(t, 4, http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			select {
			case operator.hellos <- struct{}{}:
			default:
			}
			operator.writeHello(w)
		}))
	t.Cleanup(func() {
		v4Server.Close()
		v6Server.Close()
		helloServer.Close()
	})
	operator.v4Url = v4Server.URL
	operator.v6Url = v6Server.URL
	operator.helloUrl = helloServer.URL
	return operator
}

// The api host of one family. The strategy's own hello probe reaches these
// too, so `/hello` is served here as well and is deliberately not the barrier.
func (self *testActivateOperator) familyHandler(ipVersion int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/hello" {
			self.writeHello(w)
			return
		}
		if r.URL.Path != ExtenderActivatePath {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		args := &ExtenderActivateArgs{}
		if err := json.NewDecoder(r.Body).Decode(args); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		now := self.clock.Now()
		self.stateLock.Lock()
		refusal := self.refusal
		if familyRefusal := self.familyRefusals[ipVersion]; familyRefusal != "" {
			refusal = familyRefusal
		}
		retryAfterSeconds := self.retryAfterSeconds
		refusalCode := self.refusalCode
		probeSteps := self.probeSteps
		answer := self.answer
		self.postCounts[ipVersion] += 1
		if self.predatesWebRtc && refusal == "" {
			for _, carrier := range args.Carriers {
				switch carrier {
				case ExtenderCarrierTcp, ExtenderCarrierQuic, ExtenderCarrierDns:
				default:
					if refusal == "" {
						refusal = fmt.Sprintf("unknown carrier %q", carrier)
					}
				}
			}
			if refusal == "" && !slices.Contains(args.Carriers, ExtenderCarrierTcp) {
				refusal = "an activation must offer the tcp carrier"
			}
		}
		self.stateLock.Unlock()
		select {
		case self.posts <- &testActivatePost{ipVersion: ipVersion, args: args, postTime: now}:
		default:
		}
		self.stateLock.Lock()
		answerHold := self.answerHold
		self.stateLock.Unlock()
		if answerHold != nil {
			select {
			case <-answerHold:
			case <-r.Context().Done():
				return
			}
		}

		w.Header().Set("Content-Type", "application/json")
		if refusal != "" {
			json.NewEncoder(w).Encode(&ExtenderActivateResult{
				Activated:         false,
				Error:             refusal,
				RetryAfterSeconds: retryAfterSeconds,
				Refusal:           refusalCode,
				ProbeSteps:        probeSteps,
			})
			return
		}
		switch answer {
		case testActivateAnswerUnavailable:
			http.Error(w, testActivateUnavailable, http.StatusServiceUnavailable)
			return
		case testActivateAnswerNull:
			fmt.Fprint(w, "null")
			return
		}
		result, err := self.activateResult(ipVersion, args, now)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if answer == testActivateAnswerNoFamily {
			result.Ip = ""
			result.IpVersion = 0
		}
		json.NewEncoder(w).Encode(result)
	})
}

func (self *testActivateOperator) writeHello(w http.ResponseWriter) {
	self.stateLock.Lock()
	clientAddress := self.clientAddress
	self.stateLock.Unlock()
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"client_address":%q}`, clientAddress)
}

// The signed answer of one activation: this extender's own record for the
// family's address, and one other extender as the bootstrap sample (C2).
func (self *testActivateOperator) activateResult(
	ipVersion int,
	args *ExtenderActivateArgs,
	now time.Time,
) (*ExtenderActivateResult, error) {
	publicKey, err := ParseExtenderPublicKeyHex(args.PublicKeyHex)
	if err != nil {
		return nil, err
	}
	ip := testActivateIpv4
	if ipVersion == 6 {
		ip = testActivateIpv6
	}
	self.stateLock.Lock()
	if publishedIp, ok := self.publishedIps[ipVersion]; ok {
		ip = publishedIp
	}
	refusedCarriers := self.refusedCarriers
	self.activatedIps[ipVersion] = ip
	activatedIps := []string{}
	for _, activatedIpVersion := range []int{4, 6} {
		if activatedIp, ok := self.activatedIps[activatedIpVersion]; ok {
			activatedIps = append(activatedIps, activatedIp)
		}
	}
	self.stateLock.Unlock()

	expireTime := now.Add(14 * 24 * time.Hour)
	record, err := self.signRecord(publicKey, activatedIps, now, expireTime, args.Carriers)
	if err != nil {
		return nil, err
	}
	bootstrapRecord, err := self.signRecord(
		self.bootstrapKey,
		[]string{testActivateBootstrapIpv4},
		now,
		expireTime,
		[]string{ExtenderCarrierTcp},
	)
	if err != nil {
		return nil, err
	}
	return &ExtenderActivateResult{
		Activated:       true,
		Ip:              ip,
		IpVersion:       ipVersion,
		Carriers:        args.Carriers,
		ExpireTime:      &expireTime,
		AllowedHosts:    []string{testExtenderNetworkHost, "*." + testExtenderNetworkHost},
		Record:          record,
		Bootstrap:       []string{bootstrapRecord},
		RefusedCarriers: refusedCarriers,
	}, nil
}

func (self *testActivateOperator) signRecord(
	publicKey ed25519.PublicKey,
	ips []string,
	issueTime time.Time,
	expireTime time.Time,
	carriers []string,
) (string, error) {
	addresses := []*protocol.ExtenderAddress{}
	for _, ip := range ips {
		addresses = append(addresses, testExtenderAddress(ip, carriers...))
	}
	self.stateLock.Lock()
	self.issueSerial += 1
	issueTime = issueTime.Add(time.Duration(self.issueSerial) * time.Millisecond)
	self.stateLock.Unlock()
	record, err := SignExtenderRecord(self.rootPrivateKey, &protocol.ExtenderRecordBody{
		PublicKey:    publicKey,
		Addresses:    addresses,
		TcpPort:      443,
		UdpPort:      443,
		DnsPort:      53,
		DnsTld:       testActivateDnsTld,
		CountryCode:  "zz",
		IssueTimeMs:  uint64(issueTime.UnixMilli()),
		ExpireTimeMs: uint64(expireTime.UnixMilli()),
		NetworkHost:  testExtenderNetworkHost,
	})
	if err != nil {
		return "", err
	}
	recordBytes, err := proto.Marshal(record)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(recordBytes), nil
}

func (self *testActivateOperator) setClientAddress(clientAddress string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.clientAddress = clientAddress
}

func (self *testActivateOperator) setRefusal(refusal string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.refusal = refusal
}

func (self *testActivateOperator) setAnswer(answer testActivateAnswer) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.answer = answer
}

// Refuses every activation of one family, ahead of the refusal of every
// family; "" ends it.
func (self *testActivateOperator) setFamilyRefusal(ipVersion int, refusal string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.familyRefusals[ipVersion] = refusal
}

// The `retry_after_seconds` every refusal carries from now on, 0 for none
// (connect#230).
func (self *testActivateOperator) setAnswerHold(answerHold chan struct{}) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.answerHold = answerHold
}

func (self *testActivateOperator) setRetryAfterSeconds(retryAfterSeconds int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.retryAfterSeconds = retryAfterSeconds
}

// The `refusal` every refusal carries from now on (C2c), "" for none.
func (self *testActivateOperator) setRefusalCode(refusalCode string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.refusalCode = refusalCode
}

// The `refused_carriers` every activation carries from now on (C2a), nil for
// none.
func (self *testActivateOperator) setRefusedCarriers(refusedCarriers []*ExtenderActivateCarrierRefusal) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.refusedCarriers = refusedCarriers
}

// The `probe_steps` every refusal carries from now on (C2b), nil for none.
func (self *testActivateOperator) setProbeSteps(probeSteps []*ExtenderActivateProbeStep) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.probeSteps = probeSteps
}

// Answers the activations of one family with `ip`, as the operator publishes
// the address it saw.
func (self *testActivateOperator) setPublishedIp(ipVersion int, ip string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.publishedIps[ipVersion] = ip
}

func (self *testActivateOperator) postCount(ipVersion int) int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.postCounts[ipVersion]
}

// Synthetic encoding tld of the dns carrier in these tests.
const testActivateDnsTld = "x.example."

// testActivatorFixture is one activator against the fixture operator, with the
// directory it fills and the seams the loop reads.
type testActivatorFixture struct {
	t         *testing.T
	clock     *testClock
	directory *ExtenderDirectory
	operator  *testActivateOperator
	activator *ExtenderActivator
	// what it was built from, for one that replaces it
	activatorSettings *ExtenderActivatorSettings
	activatorCtx      context.Context

	publicKey ed25519.PublicKey

	// the activations OnActivated reported
	activations chan int
	// the process network change listener the loop subscribed with
	networkChanged func()

	stateLock  sync.Mutex
	supported  map[int]bool
	carriers   []string
	wakeSerial int
}

func newTestActivatorFixture(
	t *testing.T,
	configure func(settings *ExtenderActivatorSettings),
) *testActivatorFixture {
	t.Helper()
	return newTestActivatorFixtureWithOperator(t, nil, configure)
}

// newTestActivatorFixture with `prepare` run on the operator before the
// activator starts, so its first pass already meets what `prepare` set.
func newTestActivatorFixtureWithOperator(
	t *testing.T,
	prepare func(operator *testActivateOperator),
	configure func(settings *ExtenderActivatorSettings),
) *testActivatorFixture {
	t.Helper()
	return newTestActivatorFixtureWithPrepare(t, func(fixture *testActivatorFixture) {
		if prepare != nil {
			prepare(fixture.operator)
		}
	}, configure)
}

// newTestActivatorFixture with `prepare` run once the operator, the directory
// and the extender key exist and before the activator starts, so its first
// pass already meets what `prepare` set: the operator's answers, or the records
// a restart finds in the directory.
func newTestActivatorFixtureWithPrepare(
	t *testing.T,
	prepare func(fixture *testActivatorFixture),
	configure func(settings *ExtenderActivatorSettings),
) *testActivatorFixture {
	t.Helper()
	clock := newTestClock()
	directory, rootPrivateKey := newTestExtenderDirectory(t, clock, nil)
	operator := newTestActivateOperator(t, clock, rootPrivateKey)

	seed, err := NewExtenderKeySeed()
	if err != nil {
		t.Fatal(err)
	}
	publicKey, err := ExtenderPublicKeyFromSeed(seed)
	if err != nil {
		t.Fatal(err)
	}

	fixture := &testActivatorFixture{
		t:           t,
		clock:       clock,
		directory:   directory,
		operator:    operator,
		publicKey:   publicKey,
		activations: make(chan int, 64),
		supported:   map[int]bool{4: true, 6: true},
		carriers: []string{
			ExtenderCarrierTcp,
			ExtenderCarrierQuic,
			ExtenderCarrierDns,
		},
	}
	if prepare != nil {
		prepare(fixture)
	}

	ctx, cancel := context.WithCancel(context.Background())
	// one dialer, so one request reaches the operator per call: the resilient
	// variants race the same request and would make the pass barrier count a
	// pass more than once
	strategySettings := DefaultClientStrategySettings()
	strategySettings.EnableResilient = false
	clientStrategy := NewDirectClientStrategy(ctx, strategySettings, 0)

	settings := DefaultExtenderActivatorSettings()
	settings.ApiUrlV4 = operator.v4Url
	settings.ApiUrlV6 = operator.v6Url
	settings.HelloUrl = operator.helloUrl
	settings.ByJwt = func() string { return "test-jwt" }
	settings.ClientStrategy = clientStrategy
	settings.PublicKey = publicKey
	settings.DnsTld = testActivateDnsTld
	settings.Carriers = fixture.currentCarriers
	settings.Directory = directory
	settings.OnActivated = func(ipVersion int, result *ExtenderActivateResult) {
		select {
		case fixture.activations <- ipVersion:
		default:
		}
	}
	// every stepped pass advances the fake clock by at least this, so the
	// caller address check -- the pass barrier -- runs on every step
	settings.AddressCheckTimeout = 1 * time.Minute
	settings.RequestTimeout = 20 * time.Second
	settings.Now = clock.Now
	settings.IpVersionSupported = fixture.ipVersionSupported
	settings.AddNetworkChangeListener = func(listener func()) func() {
		fixture.networkChanged = listener
		return func() {}
	}
	if configure != nil {
		configure(settings)
	}
	fixture.activatorSettings = settings
	fixture.activatorCtx = ctx
	fixture.activator = NewExtenderActivator(ctx, settings)
	t.Cleanup(func() {
		fixture.activator.Close()
		clientStrategy.Close()
		cancel()
	})
	return fixture
}

func (self *testActivatorFixture) currentCarriers() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.carriers)
}

func (self *testActivatorFixture) ipVersionSupported(ipVersion int) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.supported[ipVersion]
}

func (self *testActivatorFixture) setSupported(ipVersion int, supported bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.supported[ipVersion] = supported
}

// Wakes the loop with a directory change, which is a wake it already selects
// on. Each call adds a fresh unverified address so the change is real.
func (self *testActivatorFixture) wake() {
	self.stateLock.Lock()
	self.wakeSerial += 1
	serial := self.wakeSerial
	self.stateLock.Unlock()
	self.directory.AddBootstrap(
		netip.MustParseAddr(fmt.Sprintf("203.0.113.%d", serial)), ExtenderSourceDns)
}

// Waits for the loop to begin one pass, which its caller address check marks.
// Because the loop is single threaded, this also proves every earlier pass
// finished.
func (self *testActivatorFixture) waitPass() {
	self.t.Helper()
	select {
	case <-self.operator.hellos:
	case <-time.After(30 * time.Second):
		self.t.Fatal("the activator did not run a pass")
	}
}

// Advances the fake clock, wakes the loop and waits for the pass it starts.
func (self *testActivatorFixture) step(d time.Duration) {
	self.t.Helper()
	self.clock.advance(d)
	self.wake()
	self.waitPass()
}

// Waits for one activation post.
func (self *testActivatorFixture) waitPost() *testActivatePost {
	self.t.Helper()
	select {
	case post := <-self.operator.posts:
		return post
	case <-time.After(30 * time.Second):
		self.t.Fatal("the activator did not post an activation")
		return nil
	}
}

// Waits for one activation to be reported, which is after its record has been
// applied to the directory.
func (self *testActivatorFixture) waitActivation() int {
	self.t.Helper()
	select {
	case ipVersion := <-self.activations:
		return ipVersion
	case <-time.After(30 * time.Second):
		self.t.Fatal("the activator did not report an activation")
		return 0
	}
}

// The state of one address as the directory reports it.
func (self *testActivatorFixture) directoryState(ip string) string {
	return testDirectoryState(self.t, self.directory, netip.MustParseAddr(ip))
}

// Waits for the loop to record an activation outcome after `version`, the
// change counter read before the attempt, and returns the status it left.
// Only a recorded outcome moves the counter here, since no test that waits on
// it revokes the key.
func (self *testActivatorFixture) waitOutcome(version uint64) *ExtenderActivatorStatus {
	self.t.Helper()
	for {
		current, change := self.activator.ChangeMonitor().Get()
		if version < current {
			return self.activator.Status()
		}
		select {
		case <-change:
		case <-time.After(30 * time.Second):
			self.t.Fatal("the activator did not record an activation outcome")
			return nil
		}
	}
}

// Forces one activation attempt with a network change and returns the status
// its outcome left. The clock stays put, so no hold expires and the forced
// pass is the only attempt.
func (self *testActivatorFixture) forceActivation() *ExtenderActivatorStatus {
	self.t.Helper()
	version, _ := self.activator.ChangeMonitor().Get()
	self.networkChanged()
	return self.waitOutcome(version)
}

// Both families activate against their own api url, the record and the
// bootstrap land in the directory, and the callback reports each family (C2,
// G3).
func TestExtenderActivatorActivatesEveryFamily(t *testing.T) {
	fixture := newTestActivatorFixture(t, nil)

	fixture.waitPass()
	posts := map[int]*testActivatePost{}
	for range 2 {
		post := fixture.waitPost()
		posts[post.ipVersion] = post
	}
	if len(posts) != 2 {
		t.Fatalf("posts = %v, expected one per family", posts)
	}

	for ipVersion, post := range posts {
		if post.args.PublicKeyHex != hex.EncodeToString(fixture.publicKey) {
			t.Fatalf("v%d public key = %q", ipVersion, post.args.PublicKeyHex)
		}
		expectedCarriers := []string{
			ExtenderCarrierTcp,
			ExtenderCarrierQuic,
			ExtenderCarrierDns,
		}
		if !slices.Equal(post.args.Carriers, expectedCarriers) {
			t.Fatalf("v%d carriers = %v, expected %v", ipVersion, post.args.Carriers, expectedCarriers)
		}
		if post.args.TcpPort != ExtenderTcpPort ||
			post.args.UdpPort != ExtenderQuicPort ||
			post.args.DnsPort != ExtenderDnsPort {
			t.Fatalf("v%d ports = %d/%d/%d", ipVersion,
				post.args.TcpPort, post.args.UdpPort, post.args.DnsPort)
		}
		if post.args.DnsTld != testActivateDnsTld {
			t.Fatalf("v%d dns tld = %q", ipVersion, post.args.DnsTld)
		}
		// with no listening ports to offer the operator reads DnsPort alone,
		// which is what an extender that predates the list sends (L2)
		if post.args.DnsPorts != nil {
			t.Fatalf("v%d dns ports = %v, expected none", ipVersion, post.args.DnsPorts)
		}
	}

	activated := map[int]bool{}
	for range 2 {
		select {
		case ipVersion := <-fixture.activations:
			activated[ipVersion] = true
		case <-time.After(30 * time.Second):
			t.Fatal("the activation callback did not report both families")
		}
	}
	if !activated[4] || !activated[6] {
		t.Fatalf("activated = %v, expected both families", activated)
	}

	// the own record and the bootstrap sample are both in the directory, and
	// the own addresses are verified and active (C2, E1)
	for _, ip := range []string{testActivateIpv4, testActivateIpv6, testActivateBootstrapIpv4} {
		if state := fixture.directoryState(ip); state != ExtenderStateActive {
			t.Fatalf("%s state = %q, expected active", ip, state)
		}
	}
	ownEntry := testDirectoryEntry(t, fixture.directory, netip.MustParseAddr(testActivateIpv4))
	if !slices.Equal(ownEntry.PublicKey, fixture.publicKey) {
		t.Fatal("the own record was not applied under the extender key")
	}
	if ownEntry.Source != ExtenderSourceBootstrap {
		t.Fatalf("own record source = %q, expected bootstrap", ownEntry.Source)
	}

	status := fixture.activator.Status()
	for _, ipVersion := range []int{4, 6} {
		family := status.Family(ipVersion)
		if family == nil || !family.Activated {
			t.Fatalf("v%d status = %+v, expected activated", ipVersion, family)
		}
		if family.LastActivationTime != fixture.clock.Now() {
			t.Fatalf("v%d activation time = %s", ipVersion, family.LastActivationTime)
		}
		if family.LastError != "" {
			t.Fatalf("v%d error = %q", ipVersion, family.LastError)
		}
		if !slices.Equal(family.AllowedHosts, []string{
			testExtenderNetworkHost,
			"*." + testExtenderNetworkHost,
		}) {
			t.Fatalf("v%d allowed hosts = %v", ipVersion, family.AllowedHosts)
		}
	}
	if ip := status.Family(4).Ip.String(); ip != testActivateIpv4 {
		t.Fatalf("v4 ip = %s", ip)
	}
	if ip := status.Family(6).Ip.String(); ip != testActivateIpv6 {
		t.Fatalf("v6 ip = %s", ip)
	}
}

// A family with no api url is not run at all, and a family this host has no
// address for is never posted (G3).
func TestExtenderActivatorSkipsEmptyAndUnsupportedFamilies(t *testing.T) {
	// v6 has an api url but no address on this host
	unsupported := newTestActivatorFixture(t, func(settings *ExtenderActivatorSettings) {
		settings.IpVersionSupported = func(ipVersion int) bool { return ipVersion == 4 }
	})
	unsupported.waitPass()
	if post := unsupported.waitPost(); post.ipVersion != 4 {
		t.Fatalf("posted family = %d, expected v4", post.ipVersion)
	}
	// a second stepped pass proves the first completed without a v6 post
	unsupported.step(2 * time.Minute)
	unsupported.step(2 * time.Minute)
	if count := unsupported.operator.postCount(6); count != 0 {
		t.Fatalf("v6 posts = %d, expected none", count)
	}
	if family := unsupported.activator.Status().Family(6); family == nil || family.Activated {
		t.Fatalf("v6 status = %+v, expected an inactive entry", family)
	}
	unsupported.activator.Close()

	// v6 has no api url at all, so it is not a family this activator runs
	noUrl := newTestActivatorFixture(t, func(settings *ExtenderActivatorSettings) {
		settings.ApiUrlV6 = ""
	})
	noUrl.waitPass()
	if post := noUrl.waitPost(); post.ipVersion != 4 {
		t.Fatalf("posted family = %d, expected v4", post.ipVersion)
	}
	noUrl.step(2 * time.Minute)
	noUrl.step(2 * time.Minute)
	if count := noUrl.operator.postCount(6); count != 0 {
		t.Fatalf("v6 posts = %d, expected none", count)
	}
	if family := noUrl.activator.Status().Family(6); family != nil {
		t.Fatalf("v6 status = %+v, expected no entry", family)
	}
}

// A refusal holds the next attempt for ten minutes and doubles from there,
// then holds at the six hour cap; a success resets the hold to the floor (G3).
//
// The clock is never advanced while a post may be in flight: the operator
// stamps a post with the clock it reads when it handles the request, so
// advancing between the wake and the post would stamp the next time instead of
// the one under test.
func TestExtenderActivatorBacksOffOnRefusal(t *testing.T) {
	// refused from before the loop starts, so its first post is refused
	fixture := newTestActivatorFixtureWithOperator(
		t,
		func(operator *testActivateOperator) {
			operator.setRefusal(testActivateFamilyRefusal)
		},
		func(settings *ExtenderActivatorSettings) {
			settings.ApiUrlV6 = ""
		},
	)
	startTime := fixture.clock.Now()

	fixture.waitPass()
	first := fixture.waitPost()
	if first.postTime != startTime {
		t.Fatalf("first post at %s, expected %s", first.postTime, startTime)
	}

	// each refusal doubles the hold from the ten minute floor, so the posts
	// land at a cumulative 10, 30, 70, 150 and 310 minutes, and then every six
	// hours once the cap is reached
	holds := []time.Duration{
		10 * time.Minute,
		20 * time.Minute,
		40 * time.Minute,
		80 * time.Minute,
		160 * time.Minute,
		320 * time.Minute,
		6 * time.Hour,
		6 * time.Hour,
	}
	postTime := startTime
	for i, hold := range holds {
		// one minute short of the hold nothing is posted; the pass barrier is
		// the caller address check, which runs before the activation would,
		// so a post cannot be in flight when the count is read
		fixture.step(hold - time.Minute)
		if count := fixture.operator.postCount(4); count != i+1 {
			t.Fatalf(
				"posts one minute short of the %s hold = %d, expected %d",
				hold, count, i+1)
		}
		fixture.clock.advance(time.Minute)
		fixture.wake()
		post := fixture.waitPost()
		postTime = postTime.Add(hold)
		if post.postTime != postTime {
			t.Fatalf(
				"post %d at %s, expected %s",
				i+2, post.postTime.Sub(startTime), postTime.Sub(startTime))
		}
	}

	status := fixture.activator.Status().Family(4)
	if status.Activated {
		t.Fatal("a refused activation reported activated")
	}
	if status.LastError != testActivateFamilyRefusal {
		t.Fatalf("last error = %q", status.LastError)
	}

	// a success resets the hold: the operator stops refusing, a network change
	// forces the pass, and the refusal after it holds the floor again rather
	// than the cap
	fixture.operator.setRefusal("")
	fixture.clock.advance(time.Minute)
	fixture.networkChanged()
	successTime := fixture.clock.Now()
	if post := fixture.waitPost(); post.postTime != successTime {
		t.Fatalf("the forced post is at %s", post.postTime.Sub(startTime))
	}
	if ipVersion := fixture.waitActivation(); ipVersion != 4 {
		t.Fatalf("activated family = %d", ipVersion)
	}

	fixture.operator.setRefusal(testActivateFamilyRefusal)
	fixture.clock.advance(24 * time.Hour)
	fixture.wake()
	tickTime := fixture.clock.Now()
	if post := fixture.waitPost(); post.postTime != tickTime {
		t.Fatalf("the daily post is at %s", post.postTime.Sub(successTime))
	}
	// the hold is the floor again, not the cap it had reached
	fixture.step(9 * time.Minute)
	beforeCount := fixture.operator.postCount(4)
	fixture.clock.advance(time.Minute)
	fixture.wake()
	if post := fixture.waitPost(); post.postTime != tickTime.Add(10*time.Minute) {
		t.Fatalf(
			"the post after the reset is at %s, expected the ten minute floor",
			post.postTime.Sub(tickTime))
	}
	if count := fixture.operator.postCount(4); count != beforeCount+1 {
		t.Fatalf("posts after the reset = %d, expected %d", count, beforeCount+1)
	}
}

// A family that passed is not posted again when only the other family failed
// (connect#230). The refused family retries on its own backoff and the family
// that passed waits out its own 24 hour tick. Before, a refusal of either
// family held the whole pass and every retry re-posted both, so a fleet whose
// ipv6 failed everywhere spent its ipv4 activation budget again on every
// retry, reissuing records it already held.
func TestExtenderActivatorRetriesOnlyTheFamilyThatFailed(t *testing.T) {
	// refused from before the loop starts, so the first ipv6 post is refused
	fixture := newTestActivatorFixtureWithOperator(
		t,
		func(operator *testActivateOperator) {
			operator.setFamilyRefusal(6, testActivateFamilyRefusal)
		},
		nil,
	)
	startTime := fixture.clock.Now()

	fixture.waitPass()
	for range 2 {
		fixture.waitPost()
	}
	if ipVersion := fixture.waitActivation(); ipVersion != 4 {
		t.Fatalf("activated v%d, expected v4", ipVersion)
	}

	// expects the next post at `postTime`, from the family named, after a
	// pass barrier one minute short of it proves the pass before it finished
	expectPost := func(name string, ipVersion int, postTime time.Time) {
		t.Helper()
		fixture.step(postTime.Sub(fixture.clock.Now()) - time.Minute)
		fixture.clock.advance(time.Minute)
		fixture.wake()
		post := fixture.waitPost()
		if post.ipVersion != ipVersion || post.postTime != postTime {
			t.Fatalf(
				"%s: v%d posted at %s, expected v%d at %s",
				name, post.ipVersion, post.postTime.Sub(startTime),
				ipVersion, postTime.Sub(startTime))
		}
	}
	// the counts once the pass of the last post has finished
	expectPostCounts := func(name string, v4Count int, v6Count int) {
		t.Helper()
		fixture.step(time.Minute)
		if count := fixture.operator.postCount(4); count != v4Count {
			t.Fatalf("%s: v4 posts = %d, expected %d", name, count, v4Count)
		}
		if count := fixture.operator.postCount(6); count != v6Count {
			t.Fatalf("%s: v6 posts = %d, expected %d", name, count, v6Count)
		}
	}

	// ipv6 retries on its own backoff, 10 then 20 minutes, alone
	expectPost("the first v6 retry", 6, startTime.Add(10*time.Minute))
	expectPost("the second v6 retry", 6, startTime.Add(30*time.Minute))
	expectPostCounts("the v6 retries", 1, 3)
	status := fixture.activator.Status()
	if family := status.Family(4); !family.Activated || family.LastActivationTime != startTime {
		t.Fatalf("v4 status = %+v, expected activated at the start", family)
	}
	if family := status.Family(6); family.Activated || !family.LastRefused {
		t.Fatalf("v6 status = %+v, expected refused", family)
	}

	// ipv6 passes on its next retry, 40 minutes on
	fixture.operator.setFamilyRefusal(6, "")
	v6ActivateTime := startTime.Add(70 * time.Minute)
	expectPost("the v6 retry that passes", 6, v6ActivateTime)
	if ipVersion := fixture.waitActivation(); ipVersion != 6 {
		t.Fatalf("activated v%d, expected v6", ipVersion)
	}
	expectPostCounts("the v6 activation", 1, 4)

	// each family re-activates on its own tick
	expectPost("the v4 tick", 4, startTime.Add(24*time.Hour))
	expectPostCounts("the v4 tick", 2, 4)
	expectPost("the v6 tick", 6, v6ActivateTime.Add(24*time.Hour))
	expectPostCounts("the v6 tick", 2, 5)
}

// The holds the activation loop ends its passes with, read through its
// PassAfter seam. The loop calls the seam once a pass has posted, recorded
// what came back and set every family's schedule, so a hold read here never
// races the post that set it. With no address check and no resume watch the
// hold is exactly the time until the next activation, and the loop moves on
// only when the test wakes it.
type testActivatePassHolds struct {
	holds chan time.Duration
}

func newTestActivatePassHolds() *testActivatePassHolds {
	return &testActivatePassHolds{
		holds: make(chan time.Duration, 64),
	}
}

func (self *testActivatePassHolds) configure(settings *ExtenderActivatorSettings) {
	settings.HelloUrl = ""
	settings.ResumeCheckTimeout = 0
	settings.PassAfter = func(wait time.Duration) <-chan time.Time {
		self.holds <- wait
		// never fires: the test wakes the loop
		return nil
	}
}

// The hold the next pass ends with.
func (self *testActivatePassHolds) next(t *testing.T, name string) time.Duration {
	t.Helper()
	select {
	case hold := <-self.holds:
		return hold
	case <-time.After(30 * time.Second):
		t.Fatalf("%s: the activator did not end a pass", name)
		return 0
	}
}

// A refusal that says when the operator has room again (`retry_after_seconds`,
// connect#230) holds the family exactly that long, within a minute and six
// hours, and does not double the backoff. Before, the loop read nothing from
// the refusal: a fleet refused by its account's activation budget, which had
// room again in 50 minutes, retried at 10, 30 and 70 minutes, and each retry
// that came too early was refused again.
func TestExtenderActivatorWaitsTheOperatorsRetryAfter(t *testing.T) {
	const refusal = "You have reached the maximum number of attempts for activating " +
		"your account's extenders over IPv4 in the last hour. Please try again in 50 minutes."
	passHolds := newTestActivatePassHolds()
	fixture := newTestActivatorFixtureWithOperator(
		t,
		func(operator *testActivateOperator) {
			operator.setRefusal(refusal)
			operator.setRetryAfterSeconds(50 * 60)
		},
		func(settings *ExtenderActivatorSettings) {
			settings.ApiUrlV6 = ""
			passHolds.configure(settings)
		},
	)
	startTime := fixture.clock.Now()

	// each post is refused with the wait of its case, and the loop holds the
	// family for the case's hold before it posts again
	cases := []struct {
		name              string
		retryAfterSeconds int
		hold              time.Duration
	}{
		// where the backoff would hold 10 and then 20 minutes
		{name: "a refusal with 50 minutes", retryAfterSeconds: 50 * 60, hold: 50 * time.Minute},
		{name: "a second refusal with 50 minutes", retryAfterSeconds: 50 * 60, hold: 50 * time.Minute},
		{name: "a refusal with five seconds", retryAfterSeconds: 5, hold: time.Minute},
		{name: "a refusal with a week", retryAfterSeconds: 7 * 24 * 60 * 60, hold: 6 * time.Hour},
		// the waits asked for left the backoff at its floor
		{name: "a refusal with no wait", retryAfterSeconds: 0, hold: 10 * time.Minute},
		{name: "a second refusal with no wait", retryAfterSeconds: 0, hold: 20 * time.Minute},
	}
	postTime := startTime
	for i, c := range cases {
		fixture.operator.setRetryAfterSeconds(c.retryAfterSeconds)
		if 0 < i {
			fixture.clock.advance(cases[i-1].hold)
			fixture.wake()
		}
		post := fixture.waitPost()
		if post.postTime != postTime {
			t.Fatalf("%s: posted at %s, expected %s", c.name, post.postTime.Sub(startTime), postTime.Sub(startTime))
		}
		if hold := passHolds.next(t, c.name); hold != c.hold {
			t.Fatalf("%s: the loop holds the family %s, expected %s", c.name, hold, c.hold)
		}
		status := fixture.activator.Status().Family(4)
		if status.Activated || status.LastError != refusal || !status.LastRefused {
			t.Fatalf("%s: status = %+v, expected the operator's refusal", c.name, status)
		}
		postTime = postTime.Add(c.hold)
	}
}

// The plain api url, whose one post a cycle names no family until the answer
// does, waits the operator's `retry_after_seconds` the same way
// (connect#230).
func TestExtenderActivatorWaitsTheRetryAfterThroughThePlainApiUrl(t *testing.T) {
	passHolds := newTestActivatePassHolds()
	fixture := newTestActivatorFixtureWithOperator(
		t,
		func(operator *testActivateOperator) {
			operator.setRefusal("The operator is busy probing other extenders. Please try again in 30 minutes.")
			operator.setRetryAfterSeconds(30 * 60)
		},
		func(settings *ExtenderActivatorSettings) {
			settings.ApiUrl = settings.ApiUrlV4
			settings.ApiUrlV4 = ""
			settings.ApiUrlV6 = ""
			passHolds.configure(settings)
		},
	)

	fixture.waitPost()
	if hold := passHolds.next(t, "the refusal"); hold != 30*time.Minute {
		t.Fatalf("the loop holds the plain api url %s, expected 30m0s", hold)
	}
}

// A refusal that names when the operator has room again probed nothing, so it
// says nothing about whether a private family's NAT forwards (connect#230):
// such a family waits out the named wait, not the day that a refusal of a
// private family that has never activated holds it for. A refusal of its
// probe, which names no wait, then holds it for the day.
func TestExtenderActivatorWaitsTheRetryAfterOfAPrivateFamily(t *testing.T) {
	passHolds := newTestActivatePassHolds()
	fixture := newTestActivatorFixtureWithOperator(
		t,
		func(operator *testActivateOperator) {
			operator.setRefusal("You have reached the maximum number of attempts for activating " +
				"an extender from this address over IPv4 in the last hour. Please try again in 30 minutes.")
			operator.setRetryAfterSeconds(30 * 60)
		},
		func(settings *ExtenderActivatorSettings) {
			settings.ApiUrlV6 = ""
			settings.IpVersionPublic = func(ipVersion int) bool {
				return false
			}
			passHolds.configure(settings)
		},
	)

	fixture.waitPost()
	if hold := passHolds.next(t, "the budget's refusal"); hold != 30*time.Minute {
		t.Fatalf("the loop holds the private family %s after the budget's refusal, expected 30m0s", hold)
	}

	fixture.operator.setRefusal("the tcp carrier did not answer")
	fixture.operator.setRetryAfterSeconds(0)
	fixture.clock.advance(30 * time.Minute)
	fixture.wake()
	fixture.waitPost()
	if hold := passHolds.next(t, "the probe's refusal"); hold != 24*time.Hour {
		t.Fatalf("the loop holds the private family %s after its probe's refusal, expected 24h0m0s", hold)
	}
}

// A wake that arrives as a pass starts is not lost. The loop read its clock and
// then subscribed to its wakes, so a wake between the two was missed by the
// subscribe while its pass ran on the time before it, found nothing due, and
// went back to wait with nothing to end the wait. The clock read makes the
// interleaving exact here: the first pass's read returns its time and then
// advances the clock and wakes the loop, as a test's step does. On a loaded
// host that window took
// TestExtenderActivatorPostsAFamilyThatGainsAnAddressLate, whose first step
// lands as the activation's own directory change starts the next pass.
func TestExtenderActivatorKeepsAWakeThatArrivesAsAPassStarts(t *testing.T) {
	var clockReads atomic.Int64
	var prepared *testActivatorFixture
	fixture := newTestActivatorFixtureWithPrepare(
		t,
		func(fixture *testActivatorFixture) {
			prepared = fixture
			// a refusal applies no record, so no directory change of the
			// pass's own can end the wait in the wake's place
			fixture.operator.setRefusal(testActivateFamilyRefusal)
		},
		func(settings *ExtenderActivatorSettings) {
			settings.ApiUrlV6 = ""
			settings.ResumeCheckTimeout = 0
			// nothing but a wake ends the wait
			settings.PassAfter = func(wait time.Duration) <-chan time.Time {
				return nil
			}
			settings.Now = func() time.Time {
				now := prepared.clock.Now()
				// the resume watch takes the first read, and the first pass
				// the second
				if clockReads.Add(1) == 2 {
					prepared.clock.advance(time.Hour)
					prepared.wake()
				}
				return now
			}
		},
	)

	// the first pass, on the time before the wake
	fixture.waitPass()
	fixture.waitPost()
	// the pass the wake asked for, an hour on, with the hold of the first
	// pass's refusal run out
	fixture.waitPass()
	fixture.waitPost()
	if count := fixture.operator.postCount(4); count != 2 {
		t.Fatalf("v4 posts = %d, expected 2", count)
	}
}

// A revocation of this extender's own key, observed in the directory,
// re-activates at once rather than waiting out the 24 hour tick (G3, B5).
func TestExtenderActivatorReactivatesOnRevocation(t *testing.T) {
	fixture := newTestActivatorFixture(t, func(settings *ExtenderActivatorSettings) {
		settings.ApiUrlV6 = ""
	})
	fixture.waitPass()
	fixture.waitPost()
	fixture.waitActivation()
	if state := fixture.directoryState(testActivateIpv4); state != ExtenderStateActive {
		t.Fatalf("state = %q, expected active", state)
	}

	// the operator revoked this key; the record it signs next is newer, so the
	// address returns to active
	fixture.clock.advance(1 * time.Hour)
	revocation := signTestRevocation(
		t, fixture.operator.rootPrivateKey, fixture.publicKey, fixture.clock.Now())
	if _, err := fixture.directory.ApplyRevocation(revocation); err != nil {
		t.Fatal(err)
	}

	second := fixture.waitPost()
	if second.postTime != fixture.clock.Now() {
		t.Fatalf("re-activation at %s, expected the revocation instant", second.postTime)
	}
	fixture.waitActivation()
	if state := fixture.directoryState(testActivateIpv4); state != ExtenderStateActive {
		t.Fatalf("state after re-activation = %q, expected active", state)
	}
}

// A caller address that no longer matches what the operator published
// re-activates on the hourly check (G3).
func TestExtenderActivatorReactivatesOnAddressChange(t *testing.T) {
	fixture := newTestActivatorFixture(t, func(settings *ExtenderActivatorSettings) {
		settings.ApiUrlV6 = ""
	})
	fixture.waitPass()
	first := fixture.waitPost()

	// a pass with the same address changes nothing, and neither does a new
	// source port: every check arrives on its own connection
	fixture.step(2 * time.Minute)
	fixture.operator.setClientAddress(testActivateIpv4 + ":41002")
	fixture.step(2 * time.Minute)
	fixture.step(2 * time.Minute)
	if count := fixture.operator.postCount(4); count != 1 {
		t.Fatalf("posts with an unchanged address = %d, expected one", count)
	}

	fixture.operator.setClientAddress("198.51.100.8:41003")
	fixture.clock.advance(2 * time.Minute)
	fixture.wake()
	second := fixture.waitPost()
	if !second.postTime.After(first.postTime) {
		t.Fatalf("re-activation at %s, expected after %s", second.postTime, first.postTime)
	}
}

// The caller address the operator reports carries the source port, which is
// not part of the address the record is published under (G3).
func TestExtenderClientAddressIp(t *testing.T) {
	cases := []struct {
		clientAddress string
		want          string
	}{
		{clientAddress: "198.51.100.7:41001", want: "198.51.100.7"},
		{clientAddress: "198.51.100.7", want: "198.51.100.7"},
		{clientAddress: "[2001:db8::7]:41001", want: "2001:db8::7"},
		{clientAddress: "2001:db8::7", want: "2001:db8::7"},
		{clientAddress: "::ffff:198.51.100.7", want: "198.51.100.7"},
		{clientAddress: "", want: ""},
	}
	for _, c := range cases {
		if got := extenderClientAddressIp(c.clientAddress); got != c.want {
			t.Errorf("extenderClientAddressIp(%q) = %q, want %q", c.clientAddress, got, c.want)
		}
	}
}

// A path change re-activates: the address the operator published may no longer
// be this host's (G3).
func TestExtenderActivatorReactivatesOnNetworkChange(t *testing.T) {
	fixture := newTestActivatorFixture(t, func(settings *ExtenderActivatorSettings) {
		settings.ApiUrlV6 = ""
	})
	fixture.waitPass()
	first := fixture.waitPost()
	if fixture.networkChanged == nil {
		t.Fatal("the activator did not subscribe to network changes")
	}

	fixture.clock.advance(2 * time.Minute)
	fixture.networkChanged()
	second := fixture.waitPost()
	if !second.postTime.After(first.postTime) {
		t.Fatalf("re-activation at %s, expected after %s", second.postTime, first.postTime)
	}
}

// A successful activation is reissued on the 24 hour tick and not before (G3).
func TestExtenderActivatorReactivatesOnTheTick(t *testing.T) {
	fixture := newTestActivatorFixture(t, func(settings *ExtenderActivatorSettings) {
		settings.ApiUrlV6 = ""
	})
	startTime := fixture.clock.Now()
	fixture.waitPass()
	fixture.waitPost()

	fixture.step(23 * time.Hour)
	fixture.step(1 * time.Minute)
	if count := fixture.operator.postCount(4); count != 1 {
		t.Fatalf("posts before the tick = %d, expected one", count)
	}

	fixture.clock.advance(59 * time.Minute)
	fixture.wake()
	second := fixture.waitPost()
	if second.postTime != startTime.Add(24*time.Hour) {
		t.Fatalf("tick post at %s, expected +24h", second.postTime.Sub(startTime))
	}
}

// An extender with no carrier listening never offers an empty carrier list: it
// holds on the backoff instead, since an activation with no carrier cannot be
// probed (C2, G2).
func TestExtenderActivatorHoldsWithNoCarrier(t *testing.T) {
	fixture := newTestActivatorFixture(t, func(settings *ExtenderActivatorSettings) {
		settings.ApiUrlV6 = ""
		settings.Carriers = func() []string { return nil }
	})
	fixture.waitPass()
	fixture.step(2 * time.Minute)
	fixture.step(2 * time.Minute)
	if count := fixture.operator.postCount(4); count != 0 {
		t.Fatalf("posts = %d, expected none", count)
	}
	status := fixture.activator.Status().Family(4)
	if status.Activated || status.LastError != "no carrier is listening" {
		t.Fatalf("status = %+v", status)
	}
}

// With neither family api url set the activator posts once per cycle to the
// plain api url and records the outcome under the family the answer reports,
// which is the family the operator derived from the caller address (C2, G3).
func TestExtenderActivatorUsesThePlainApiUrl(t *testing.T) {
	// the plain url is the v6 host of the fixture, so the family in the status
	// can only have come from the answer and never from a default
	fixture := newTestActivatorFixture(t, func(settings *ExtenderActivatorSettings) {
		settings.ApiUrl = settings.ApiUrlV6
		settings.ApiUrlV4 = ""
		settings.ApiUrlV6 = ""
	})

	fixture.waitPass()
	if post := fixture.waitPost(); post.ipVersion != 6 {
		t.Fatalf("posted to v%d, expected the plain api url", post.ipVersion)
	}
	if ipVersion := fixture.waitActivation(); ipVersion != 6 {
		t.Fatalf("activated v%d, expected the family the answer reported", ipVersion)
	}

	status := fixture.activator.Status()
	if len(status.Families) != 1 {
		t.Fatalf("families = %+v, expected only the family the answer named", status.Families)
	}
	family := status.Family(6)
	if family == nil || !family.Activated {
		t.Fatalf("v6 status = %+v, expected activated", family)
	}
	if ip := family.Ip.String(); ip != testActivateIpv6 {
		t.Fatalf("v6 ip = %s, expected %s", ip, testActivateIpv6)
	}
	if state := fixture.directoryState(testActivateIpv6); state != ExtenderStateActive {
		t.Fatalf("%s state = %q, expected active", testActivateIpv6, state)
	}

	// one post per cycle: there is no second url to reach the other family with
	fixture.clock.advance(25 * time.Hour)
	fixture.wake()
	fixture.waitPost()
	fixture.waitActivation()
	fixture.step(1 * time.Minute)
	if count := fixture.operator.postCount(6); count != 2 {
		t.Fatalf("posts = %d, expected one per cycle", count)
	}
	if count := fixture.operator.postCount(4); count != 0 {
		t.Fatalf("posts to the v4 host = %d, expected none", count)
	}
}

// A refusal through the plain api url, which names no family, is still visible
// in the status: it is held under the placeholder family until an answer names
// one (C2, F3).
func TestExtenderActivatorRecordsAPlainApiUrlRefusal(t *testing.T) {
	// refused from before the loop starts, so its first post is refused
	fixture := newTestActivatorFixtureWithOperator(
		t,
		func(operator *testActivateOperator) {
			operator.setRefusal("the tcp carrier did not answer")
		},
		func(settings *ExtenderActivatorSettings) {
			settings.ApiUrl = settings.ApiUrlV4
			settings.ApiUrlV4 = ""
			settings.ApiUrlV6 = ""
		},
	)

	fixture.waitPass()
	fixture.waitPost()
	fixture.step(1 * time.Minute)

	status := fixture.activator.Status()
	if len(status.Families) != 1 {
		t.Fatalf("families = %+v, expected the placeholder only", status.Families)
	}
	placeholder := status.Families[0]
	if placeholder.IpVersion != 0 || placeholder.Activated {
		t.Fatalf("placeholder = %+v, expected an unactivated family 0", placeholder)
	}
	if placeholder.LastError != "the tcp carrier did not answer" {
		t.Fatalf("last error = %q", placeholder.LastError)
	}

	// the first answer that names a family replaces the placeholder
	fixture.operator.setRefusal("")
	fixture.clock.advance(25 * time.Hour)
	fixture.wake()
	fixture.waitPost()
	if ipVersion := fixture.waitActivation(); ipVersion != 4 {
		t.Fatalf("activated v%d, expected v4", ipVersion)
	}
	status = fixture.activator.Status()
	if len(status.Families) != 1 || status.Families[0].IpVersion != 4 {
		t.Fatalf("families = %+v, expected v4 alone", status.Families)
	}
	if !status.Families[0].Activated || status.Families[0].LastError != "" {
		t.Fatalf("v4 status = %+v, expected activated", status.Families[0])
	}
}

// One activation attempt of an outcome sequence: what is listening, how the
// operator answers, and the family status the attempt must leave.
type testActivateOutcome struct {
	name      string
	listening bool
	refusal   string
	answer    testActivateAnswer

	// the family the outcome is recorded under, 0 for the placeholder of the
	// plain api url
	ipVersion int
	activated bool
	lastError string
	refused   bool
}

// The carriers seam of an outcome sequence: every carrier while `listening`
// holds and none otherwise. It starts with none, so the loop's own first pass
// is a failure a test can wait on before it switches anything.
func testActivateCarriers(listening *atomic.Bool) func() []string {
	return func() []string {
		if !listening.Load() {
			return nil
		}
		return []string{ExtenderCarrierTcp, ExtenderCarrierQuic, ExtenderCarrierDns}
	}
}

// Checks the family status one outcome left. The activators of the outcome
// sequences run one family, so the status has one entry.
func (self *testActivatorFixture) expectOutcome(
	status *ExtenderActivatorStatus,
	outcome testActivateOutcome,
) {
	self.t.Helper()
	if len(status.Families) != 1 {
		self.t.Fatalf("%s: families = %+v, expected one", outcome.name, status.Families)
	}
	family := status.Families[0]
	if family.IpVersion != outcome.ipVersion ||
		family.Activated != outcome.activated ||
		family.LastError != outcome.lastError ||
		family.LastRefused != outcome.refused {
		self.t.Fatalf(
			"%s: status = %+v, expected v%d activated %t, error %q, refused %t",
			outcome.name, family,
			outcome.ipVersion, outcome.activated, outcome.lastError, outcome.refused)
	}
}

// Forces one attempt per outcome, in order, each against the carriers and the
// answer the outcome names.
func (self *testActivatorFixture) expectOutcomes(
	listening *atomic.Bool,
	outcomes []testActivateOutcome,
) {
	self.t.Helper()
	for i, outcome := range outcomes {
		listening.Store(outcome.listening)
		self.operator.setRefusal(outcome.refusal)
		self.operator.setAnswer(outcome.answer)
		outcome.name = fmt.Sprintf("attempt %d, %s", i+1, outcome.name)
		self.expectOutcome(self.forceActivation(), outcome)
	}
}

// The operator's refusal is told apart from every other failure: it marks the
// family refused beside the operator's reason, a failure of any other kind
// after it clears the mark, and so does a success (N3, N7).
func TestExtenderActivatorTellsARefusalFromAFailure(t *testing.T) {
	listening := &atomic.Bool{}
	fixture := newTestActivatorFixture(t, func(settings *ExtenderActivatorSettings) {
		settings.ApiUrlV6 = ""
		settings.Carriers = testActivateCarriers(listening)
	})
	const refusal = "the tcp carrier did not answer"

	fixture.expectOutcome(fixture.waitOutcome(0), testActivateOutcome{
		name:      "no carrier on the first pass",
		ipVersion: 4,
		lastError: "no carrier is listening",
	})

	refused := testActivateOutcome{
		name:      "refused",
		listening: true,
		refusal:   refusal,
		ipVersion: 4,
		lastError: refusal,
		refused:   true,
	}
	fixture.expectOutcomes(listening, []testActivateOutcome{
		refused,
		{
			name:      "a failed request after a refusal",
			listening: true,
			answer:    testActivateAnswerUnavailable,
			ipVersion: 4,
			lastError: "503 Service Unavailable: " + testActivateUnavailable,
		},
		refused,
		{
			name:      "no activation result after a refusal",
			listening: true,
			answer:    testActivateAnswerNull,
			ipVersion: 4,
			lastError: "the operator sent no activation result",
		},
		refused,
		{
			name:      "no carrier after a refusal",
			ipVersion: 4,
			lastError: "no carrier is listening",
		},
		refused,
		{
			name:      "activated after a refusal",
			listening: true,
			ipVersion: 4,
			activated: true,
		},
	})
}

// The plain api url tells a refusal from a failure the same way: under the
// placeholder while no answer has named a family, and under the family once
// one has. An activation whose answer names no family is a failure, not a
// refusal (C2, N7).
func TestExtenderActivatorTellsAPlainApiUrlRefusalFromAFailure(t *testing.T) {
	listening := &atomic.Bool{}
	fixture := newTestActivatorFixture(t, func(settings *ExtenderActivatorSettings) {
		settings.ApiUrl = settings.ApiUrlV4
		settings.ApiUrlV4 = ""
		settings.ApiUrlV6 = ""
		settings.Carriers = testActivateCarriers(listening)
	})
	const refusal = "the tcp carrier did not answer"

	fixture.expectOutcome(fixture.waitOutcome(0), testActivateOutcome{
		name:      "no carrier on the first pass",
		lastError: "no carrier is listening",
	})

	refused := testActivateOutcome{
		name:      "refused",
		listening: true,
		refusal:   refusal,
		lastError: refusal,
		refused:   true,
	}
	fixture.expectOutcomes(listening, []testActivateOutcome{
		refused,
		{
			name:      "a failed request after a refusal",
			listening: true,
			answer:    testActivateAnswerUnavailable,
			lastError: "503 Service Unavailable: " + testActivateUnavailable,
		},
		refused,
		{
			name:      "no activation result after a refusal",
			listening: true,
			answer:    testActivateAnswerNull,
			lastError: "the operator sent no activation result",
		},
		refused,
		{
			name:      "no carrier after a refusal",
			lastError: "no carrier is listening",
		},
		refused,
		{
			name:      "an answer naming no family after a refusal",
			listening: true,
			answer:    testActivateAnswerNoFamily,
			lastError: "the operator named no address family",
		},
		refused,
		{
			// the first answer that names a family replaces the placeholder
			name:      "activated after a refusal",
			listening: true,
			ipVersion: 4,
			activated: true,
		},
		{
			// a refusal names no family, so it lands on the family there is
			name:      "refused after the activation",
			listening: true,
			refusal:   refusal,
			ipVersion: 4,
			lastError: refusal,
			refused:   true,
		},
		{
			name:      "activated after that refusal",
			listening: true,
			ipVersion: 4,
			activated: true,
		},
	})
}

// The posted args are the server's json contract field for field (C2). The
// literal document is the shape `controller.ExtenderActivateArgs` declares.
func TestExtenderActivateArgsJsonContract(t *testing.T) {
	args := &ExtenderActivateArgs{
		PublicKeyHex: "00112233",
		TcpPort:      443,
		UdpPort:      443,
		DnsPort:      53,
		DnsTld:       "x.example.",
		Carriers:     []string{"tcp", "quic", "dns"},
	}
	argsBytes, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	expected := `{"public_key_hex":"00112233","tcp_port":443,"udp_port":443,` +
		`"dns_port":53,"dns_tld":"x.example.","carriers":["tcp","quic","dns"]}`
	if string(argsBytes) != expected {
		t.Fatalf("args json = %s, expected %s", argsBytes, expected)
	}

	decoded := &ExtenderActivateArgs{}
	if err := json.Unmarshal([]byte(expected), decoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded, args) {
		t.Fatalf("decoded args = %+v, expected %+v", decoded, args)
	}
}

// The activation answer decodes field for field from the server's json (C2).
func TestExtenderActivateResultJsonContract(t *testing.T) {
	document := `{"activated":true,"ip":"198.51.100.7","ip_version":4,` +
		`"carriers":["tcp","quic"],"error":"","expire_time":"2026-01-15T00:00:00Z",` +
		`"allowed_hosts":["space.example","*.space.example"],` +
		`"record":"AQID","bootstrap":["BAUG","BwgJ"]}`
	result := &ExtenderActivateResult{}
	if err := json.Unmarshal([]byte(document), result); err != nil {
		t.Fatal(err)
	}
	expireTime := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)
	expected := &ExtenderActivateResult{
		Activated:    true,
		Ip:           "198.51.100.7",
		IpVersion:    4,
		Carriers:     []string{"tcp", "quic"},
		ExpireTime:   &expireTime,
		AllowedHosts: []string{"space.example", "*.space.example"},
		Record:       "AQID",
		Bootstrap:    []string{"BAUG", "BwgJ"},
	}
	if !reflect.DeepEqual(result, expected) {
		t.Fatalf("result = %+v, expected %+v", result, expected)
	}

	refusal := `{"activated":false,"error":"the tcp carrier did not answer"}`
	refused := &ExtenderActivateResult{}
	if err := json.Unmarshal([]byte(refusal), refused); err != nil {
		t.Fatal(err)
	}
	if refused.Activated || refused.Error != "the tcp carrier did not answer" {
		t.Fatalf("refusal = %+v", refused)
	}
	if refused.ExpireTime != nil {
		t.Fatalf("refusal expire time = %v, expected none", refused.ExpireTime)
	}
	if refused.RetryAfterSeconds != 0 {
		t.Fatalf("refusal retry after = %d, expected none", refused.RetryAfterSeconds)
	}

	// a refusal that says when the operator has room again (connect#230)
	budgetRefusal := `{"activated":false,"error":"Please try again in 2 minutes.","retry_after_seconds":95}`
	budgetRefused := &ExtenderActivateResult{}
	if err := json.Unmarshal([]byte(budgetRefusal), budgetRefused); err != nil {
		t.Fatal(err)
	}
	if budgetRefused.Activated || budgetRefused.RetryAfterSeconds != 95 {
		t.Fatalf("budget refusal = %+v, expected a retry after of 95 seconds", budgetRefused)
	}
}

// The activation advertises every dns port that is listening, in dial order,
// beside the single port an operator that predates the list reads (L2).
func TestExtenderActivatorSendsDnsPorts(t *testing.T) {
	fixture := newTestActivatorFixture(t, func(settings *ExtenderActivatorSettings) {
		// the bound order is the server's; the wire order is the dial order
		settings.DnsPorts = func() []int { return []int{DefaultWhodisPort, DefaultDnsPort} }
	})

	fixture.waitPass()
	post := fixture.waitPost()
	if !slices.Equal(post.args.DnsPorts, []int{DefaultDnsPort, DefaultWhodisPort}) {
		t.Fatalf("dns ports = %v, expected 53 then 4053", post.args.DnsPorts)
	}
	if post.args.DnsPort != ExtenderDnsPort {
		t.Fatalf("dns port = %d, expected %d", post.args.DnsPort, ExtenderDnsPort)
	}
}

// A post offers the ports a router's mapping forwards to this extender, read
// at each post: they replace the configured ports one by one, and a mapping the
// router moved is offered by the next post (S10, the port mapping).
func TestExtenderActivatorOffersTheMappedPorts(t *testing.T) {
	var stateLock sync.Mutex
	tcpPort, udpPort, dnsPort := 40443, 0, 44053
	fixture := newTestActivatorFixture(t, func(settings *ExtenderActivatorSettings) {
		settings.OfferedPorts = func() (int, int, int) {
			stateLock.Lock()
			defer stateLock.Unlock()
			return tcpPort, udpPort, dnsPort
		}
	})

	fixture.waitPass()
	for range 2 {
		post := fixture.waitPost()
		// the unmapped udp carrier keeps its configured port
		if post.args.TcpPort != 40443 || post.args.UdpPort != ExtenderQuicPort || post.args.DnsPort != 44053 {
			t.Fatalf("v%d ports = %d/%d/%d, expected the mapped tcp and dns ports",
				post.ipVersion, post.args.TcpPort, post.args.UdpPort, post.args.DnsPort)
		}
	}

	func() {
		stateLock.Lock()
		defer stateLock.Unlock()
		tcpPort, udpPort = 41443, 40444
	}()
	fixture.activator.Reactivate()
	for range 2 {
		post := fixture.waitPost()
		if post.args.TcpPort != 41443 || post.args.UdpPort != 40444 || post.args.DnsPort != 44053 {
			t.Fatalf("v%d ports after the move = %d/%d/%d, expected the moved mapping",
				post.ipVersion, post.args.TcpPort, post.args.UdpPort, post.args.DnsPort)
		}
	}
}
