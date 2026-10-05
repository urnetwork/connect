package connect

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// The extender hint is read through direct dialers only (open bug P052). The
// operator places the address a request arrives from, so a hint that crossed
// an extender would place the extender, and the client would front its dials
// with the list of the extender's country instead of its own. Where only an
// extender reaches the operator the read fails instead, and the directory
// takes the network country the host reports.

// Where the test operator places an address: the client's own for a request
// that arrives directly, the relay's for one the extender relayed. The two
// differ in both the continent and the country, so either tells the routes
// apart.
var (
	testHintDirectPlace = ExtenderHintResult{ContinentCode: "EU", CountryCode: "nl"}
	testHintRelayPlace  = ExtenderHintResult{ContinentCode: "NA", CountryCode: "us"}
)

// A tcp carrier extender on loopback. It relays each dial to the destination
// its header names, and keeps the local address of each relayed connection,
// which is the address the destination sees the request arrive from.
type testRelayExtender struct {
	listener net.Listener
	workers  sync.WaitGroup

	stateLock  sync.Mutex
	closed     bool
	conns      []net.Conn
	relayAddrs map[string]bool
}

// Listens on the v4 loopback and relays until the test ends.
func newTestRelayExtender(t *testing.T) *testRelayExtender {
	t.Helper()
	certPem, keyPem, err := selfSign([]string{"127.0.0.1"}, "relay-extender", time.Hour, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := tls.X509KeyPair(certPem, keyPem)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := tls.Listen("tcp4", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{certificate},
	})
	if err != nil {
		t.Fatal(err)
	}
	self := &testRelayExtender{
		listener:   listener,
		relayAddrs: map[string]bool{},
	}
	self.workers.Add(1)
	go func() {
		defer self.workers.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			if !self.track(conn) {
				conn.Close()
				return
			}
			self.workers.Add(1)
			go func() {
				defer self.workers.Done()
				self.relay(conn)
			}()
		}
	}()
	t.Cleanup(self.close)
	return self
}

// The port the extender listens on.
func (self *testRelayExtender) port() int {
	return self.listener.Addr().(*net.TCPAddr).Port
}

// Keeps a connection for the close, or reports that the extender is closed.
func (self *testRelayExtender) track(conn net.Conn) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.closed {
		return false
	}
	self.conns = append(self.conns, conn)
	return true
}

// Closes the listener and every connection, and joins the workers.
func (self *testRelayExtender) close() {
	self.listener.Close()
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.closed = true
		for _, conn := range self.conns {
			conn.Close()
		}
	}()
	self.workers.Wait()
}

// Serves the request of one carrier connection (A3) and relays the stream
// that follows it.
func (self *testRelayExtender) relay(conn net.Conn) {
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	reader := bufio.NewReader(conn)
	request, err := http.ReadRequest(reader)
	if err != nil {
		return
	}
	headerBytes, err := io.ReadAll(io.LimitReader(request.Body, ExtenderMaxHeaderByteCount))
	if err != nil {
		return
	}
	header := &protocol.ExtenderHeader{}
	if err := ProtoUnmarshal(headerBytes, header); err != nil {
		return
	}
	destination, err := net.DialTimeout(
		"tcp",
		net.JoinHostPort(header.DestinationHost, strconv.Itoa(int(header.DestinationPort))),
		5*time.Second,
	)
	if err != nil {
		return
	}
	defer destination.Close()
	if !self.track(destination) {
		return
	}
	// recorded before the response, so before the client can send anything
	// the destination would place
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.relayAddrs[destination.LocalAddr().String()] = true
	}()
	frameBytes, err := ExtenderResponseFrame(&protocol.ExtenderResponse{})
	if err != nil {
		return
	}
	if _, err := fmt.Fprintf(conn, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\n\r\n", len(frameBytes)); err != nil {
		return
	}
	if _, err := conn.Write(frameBytes); err != nil {
		return
	}
	conn.SetDeadline(time.Time{})

	done := make(chan struct{})
	go func() {
		defer close(done)
		io.Copy(destination, reader)
		destination.Close()
	}()
	io.Copy(conn, destination)
	conn.Close()
	<-done
}

// Reports whether a request from this address is one the extender relayed.
func (self *testRelayExtender) relayed(remoteAddr string) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.relayAddrs[remoteAddr]
}

// An operator whose hint places the address a request arrives from, as the
// real one does: the relay's place for a connection the extender opened, the
// client's own for any other. Every other path answers an empty object, for a
// request that only has to reach the operator.
type testHintOperator struct {
	server   *httptest.Server
	extender *testRelayExtender

	stateLock        sync.Mutex
	relayedCount     int
	hintCount        int
	relayedHintCount int
}

// Serves the operator on the v4 loopback until the test ends. With no
// extender every request is placed as one that arrived directly.
func newTestHintOperator(t *testing.T, extender *testRelayExtender) *testHintOperator {
	t.Helper()
	self := &testHintOperator{
		extender: extender,
	}
	self.server = newFamilyHttptestServer(t, 4, http.HandlerFunc(self.serve))
	t.Cleanup(self.server.Close)
	return self
}

// The operator's base url.
func (self *testHintOperator) url() string {
	return self.server.URL
}

// The address the operator listens on, as a dial names it.
func (self *testHintOperator) addr() string {
	return self.server.Listener.Addr().String()
}

// Answers the hint with the place of the address the request arrived from,
// and counts the requests.
func (self *testHintOperator) serve(w http.ResponseWriter, r *http.Request) {
	relayed := self.extender != nil && self.extender.relayed(r.RemoteAddr)
	hint := r.URL.Path == ExtenderHintPath
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if relayed {
			self.relayedCount += 1
		}
		if hint {
			self.hintCount += 1
			if relayed {
				self.relayedHintCount += 1
			}
		}
	}()
	w.Header().Set("Content-Type", "application/json")
	if !hint {
		fmt.Fprint(w, "{}")
		return
	}
	place := testHintDirectPlace
	if relayed {
		place = testHintRelayPlace
	}
	json.NewEncoder(w).Encode(&place)
}

// The requests that arrived through the extender, the hints answered, and the
// hints answered for a request that arrived through the extender.
func (self *testHintOperator) counts() (int, int, int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.relayedCount, self.hintCount, self.relayedHintCount
}

// The two routes to the test operator a client strategy holds: its direct
// dialers, which reach the operator only while the network routes the
// operator's address, and one configured extender, the relay, whose address
// the network always routes. A whitelist-only network has the first closed
// and the second open.
type testHintRoutes struct {
	extender       *testRelayExtender
	operator       *testHintOperator
	clientStrategy *ClientStrategy

	operatorUnroutable atomic.Bool
	// a dial of the operator's address gets no answer at all and lasts until
	// its context ends, as on a network that drops what it does not route
	operatorBlackholed atomic.Bool
	// dials of the operator's address through the strategy's dial seam: every
	// direct attempt. The relay dials the operator on its own.
	directDialCount atomic.Int64
}

// The relay, the operator, and a client strategy over both routes. configure,
// when set, changes the strategy settings before the strategy is built.
func newTestHintRoutes(t *testing.T, configure func(settings *ClientStrategySettings)) *testHintRoutes {
	t.Helper()
	self := &testHintRoutes{}
	self.extender = newTestRelayExtender(t)
	self.operator = newTestHintOperator(t, self.extender)

	operatorAddr := self.operator.addr()
	settings := DefaultClientStrategySettings()
	settings.ExtenderConfigs = []*ExtenderConfig{{
		Profile: ExtenderProfile{
			ConnectMode: ExtenderConnectModeTcpTls,
			Port:        self.extender.port(),
		},
		Ip: netip.MustParseAddr("127.0.0.1"),
	}}
	settings.ConnectSettings.DialContextSettings = &DialContextSettings{
		DialContext: func(ctx context.Context, network string, addr string) (net.Conn, error) {
			if addr == operatorAddr {
				self.directDialCount.Add(1)
				if self.operatorUnroutable.Load() {
					return nil, errors.New("the operator address is not routable on this network")
				}
				if self.operatorBlackholed.Load() {
					<-ctx.Done()
					return nil, ctx.Err()
				}
			}
			dialer := &net.Dialer{}
			return dialer.DialContext(ctx, network, addr)
		},
	}
	// the preferred route is tried alone, so a request takes the route the
	// strategy prefers rather than whichever of a race answers first
	settings.GetPreferredRouteHedgeDelay = 0
	if configure != nil {
		configure(settings)
	}
	ctx, cancel := context.WithCancel(context.Background())
	self.clientStrategy = NewClientStrategy(ctx, settings)
	t.Cleanup(func() {
		self.clientStrategy.Close()
		cancel()
	})
	return self
}

// Sends one request only the extender can carry: with the operator's address
// unroutable the direct dialers fail it and the extender answers, so the
// strategy prefers the extender from then on, as a strategy does whose
// requests an extender has been carrying.
func (self *testHintRoutes) preferTheExtender(t *testing.T) {
	t.Helper()
	self.operatorUnroutable.Store(true)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := HttpGetWithStrategyRaw(ctx, self.clientStrategy, self.operator.url()+"/relayed", ""); err != nil {
		t.Fatalf("the extender did not carry a request to the operator: %s", err)
	}
	if relayedCount, _, _ := self.operator.counts(); relayedCount == 0 {
		t.Fatal("a request reached the operator's unroutable address without the extender")
	}
}

// A network client that reads the hint for real, through the client strategy
// it is given, into a directory of its own. The returned channel closes once
// its first hint read has ended and what it answered has been applied; the
// read runs beside the refresh pass, so the pass is no measure of it. No
// later read is due while a test runs.
func newTestHintNetworkClient(
	t *testing.T,
	clientStrategy *ClientStrategy,
	apiUrl string,
	hintTimeout time.Duration,
) (*ExtenderDirectory, <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	directory := NewExtenderDirectory(ctx, DefaultExtenderDirectorySettings())
	settings := DefaultExtenderNetworkClientSettings()
	settings.ApiUrl = apiUrl
	settings.ExtenderDnsName = "extender.space.example"
	settings.Subscribe = false
	settings.ProbeWindowCount = 0
	// the hint's budget
	settings.HelloTimeout = hintTimeout
	settings.MinBackoff = time.Hour
	settings.MaxBackoff = time.Hour
	settings.IpVersionSupported = func(ipVersion int) bool { return true }
	settings.Hello = func(ctx context.Context) (*ExtenderHelloResult, error) {
		return nil, nil
	}
	settings.ResolveDnsTxt = func(ctx context.Context, name string) ([]string, error) {
		return nil, nil
	}
	settings.ResolveDns = func(ctx context.Context, name string) ([]netip.Addr, error) {
		return nil, nil
	}
	networkClient := NewExtenderNetworkClient(ctx, clientStrategy, directory, settings)
	t.Cleanup(func() {
		networkClient.Close()
		directory.Close()
		cancel()
	})
	return directory, networkClient.initialHintDone
}

// Waits for the network client's first hint read to end.
func waitForTestHintRead(t *testing.T, hinted <-chan struct{}) {
	t.Helper()
	select {
	case <-hinted:
	case <-time.After(30 * time.Second):
		t.Fatal("the network client's first hint read never ended")
	}
}

// The bug: where only an extender reaches the operator -- a whitelist-only
// network routes the extender's address and not the operator's -- the hint
// is tried directly and fails, rather than crossing the extender and placing
// it, and the directory takes the network country the host reports.
func TestExtenderHintIsNotReadThroughAnExtender(t *testing.T) {
	setTestNetworkCountryCode(t, "ru")
	routes := newTestHintRoutes(t, nil)
	// the extender carries requests to the operator, and the strategy prefers
	// it: a hint read through the strategy's own dialers would answer
	routes.preferTheExtender(t)
	directDialCount := routes.directDialCount.Load()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if result, err := GetExtenderHint(ctx, routes.clientStrategy, routes.operator.url()); err == nil {
		t.Errorf("the hint answered %+v where only an extender reaches the operator", *result)
	}
	if routes.directDialCount.Load() == directDialCount {
		t.Error("the hint was not tried directly")
	}
	if _, hintCount, _ := routes.operator.counts(); hintCount != 0 {
		t.Errorf("the operator answered %d hints, expected none to reach it", hintCount)
	}

	directory, hinted := newTestHintNetworkClient(t, routes.clientStrategy, routes.operator.url(), 2*time.Second)
	waitForTestHintRead(t, hinted)
	if countryCode := directory.SpoofCountryCode(); countryCode != "ru" {
		t.Errorf("spoof country = %q, expected the network country", countryCode)
	}
	if continentCode := directory.ContinentHint(); continentCode != "" {
		t.Errorf("continent = %q, expected no hint", continentCode)
	}
	if _, hintCount, _ := routes.operator.counts(); hintCount != 0 {
		t.Errorf("the operator answered %d hints, expected none to reach it", hintCount)
	}
}

// Where the operator's address is black-holed -- a dial of it gets no answer
// at all -- and an extender carries everything else, a direct read of the
// hint lasts its whole budget. The read runs beside the refresh pass, so the
// first pass completes while the read is still dialing, with the network
// country in force.
func TestExtenderHintReadDoesNotHoldThePassWhereTheOperatorIsBlackholed(t *testing.T) {
	setTestNetworkCountryCode(t, "ru")
	routes := newTestHintRoutes(t, func(settings *ClientStrategySettings) {
		// a dial of the black-holed address lasts as long as the read
		settings.ConnectSettings.ConnectTimeout = time.Hour
		settings.ConnectSettings.TlsTimeout = time.Hour
		settings.ConnectSettings.RequestTimeout = time.Hour
	})
	routes.operatorBlackholed.Store(true)

	directory, hinted := newTestHintNetworkClient(t, routes.clientStrategy, routes.operator.url(), time.Hour)
	timeout := time.After(30 * time.Second)
	for {
		state, update := directory.InitialSampleMonitor().Get()
		if state == ExtenderInitialSampleDone {
			break
		}
		select {
		case <-update:
		case <-timeout:
			t.Fatal("the first pass waited for the hint read")
		}
	}
	deadline := time.Now().Add(30 * time.Second)
	for routes.directDialCount.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the hint was not tried directly")
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case <-hinted:
		t.Fatal("the hint read ended, expected it still dialing the black-holed address")
	default:
	}
	if countryCode := directory.SpoofCountryCode(); countryCode != "ru" {
		t.Errorf("spoof country = %q, expected the network country", countryCode)
	}
	if _, hintCount, _ := routes.operator.counts(); hintCount != 0 {
		t.Errorf("the operator answered %d hints, expected none to reach it", hintCount)
	}
}

// With the operator reachable directly the hint answers with the client's own
// place -- also for a strategy whose requests an extender has been carrying,
// the route the strategy itself would take -- and the directory takes it over
// the network country.
func TestExtenderHintIsReadDirectly(t *testing.T) {
	setTestNetworkCountryCode(t, "")
	routes := newTestHintRoutes(t, nil)
	routes.preferTheExtender(t)
	// the network routes the operator again
	routes.operatorUnroutable.Store(false)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := GetExtenderHint(ctx, routes.clientStrategy, routes.operator.url())
	if err != nil {
		t.Fatalf("the hint failed with the operator reachable: %s", err)
	}
	if *result != testHintDirectPlace {
		t.Errorf("the hint placed %+v, expected the client's own place %+v", *result, testHintDirectPlace)
	}

	directory, hinted := newTestHintNetworkClient(t, routes.clientStrategy, routes.operator.url(), 10*time.Second)
	waitForTestHintRead(t, hinted)
	if countryCode := directory.SpoofCountryCode(); countryCode != testHintDirectPlace.CountryCode {
		t.Errorf("spoof country = %q, expected the operator's %q", countryCode, testHintDirectPlace.CountryCode)
	}
	if continentCode := directory.ContinentHint(); continentCode != testHintDirectPlace.ContinentCode {
		t.Errorf("continent = %q, expected the operator's %q", continentCode, testHintDirectPlace.ContinentCode)
	}
	// a new strategy races its dialers with the whole request, so one read
	// may arrive more than once; none may arrive through the extender
	if _, hintCount, relayedHintCount := routes.operator.counts(); hintCount == 0 || relayedHintCount != 0 {
		t.Errorf("the operator answered %d hints, %d through the extender; expected none through it", hintCount, relayedHintCount)
	}
}

// A strategy that relays every request -- a manual extender carries every
// request of its strategy, a proxy every dial -- dials nothing direct. The
// hint has no direct path there and fails without a dial, rather than being
// the one request that goes direct. Here the manual extender.
func TestExtenderHintHasNoDirectPathWhereEveryRequestIsRelayedByAManualExtender(t *testing.T) {
	routes := newTestHintRoutes(t, func(settings *ClientStrategySettings) {
		settings.ExtenderConfigs = nil
	})
	routes.clientStrategy.SetCustomExtenders(map[netip.Addr]string{
		netip.MustParseAddr("192.0.2.1"): "",
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if result, err := GetExtenderHint(ctx, routes.clientStrategy, routes.operator.url()); err == nil {
		t.Errorf("the hint answered %+v with a manual extender", *result)
	}
	if directDialCount := routes.directDialCount.Load(); directDialCount != 0 {
		t.Errorf("the hint dialed the operator directly %d times with a manual extender", directDialCount)
	}
	if _, hintCount, _ := routes.operator.counts(); hintCount != 0 {
		t.Errorf("the operator answered %d hints", hintCount)
	}
}

// The same for a strategy whose every dial crosses a proxy.
func TestExtenderHintHasNoDirectPathWhereEveryRequestIsRelayedByAProxy(t *testing.T) {
	operator := newTestHintOperator(t, nil)
	proxy := newTestCountingListener(t)
	// no dial seam: an injected dial would take the place of the proxy
	settings := DefaultClientStrategySettings()
	settings.ConnectSettings.ProxySettings = &ProxySettings{
		Network: "tcp",
		Address: proxy.listener.Addr().String(),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	clientStrategy := NewClientStrategy(ctx, settings)
	defer clientStrategy.Close()

	if result, err := GetExtenderHint(ctx, clientStrategy, operator.url()); err == nil {
		t.Errorf("the hint answered %+v with a proxy", *result)
	}
	if count := proxy.count.Load(); count != 0 {
		t.Errorf("the hint dialed the proxy %d times", count)
	}
	if _, hintCount, _ := operator.counts(); hintCount != 0 {
		t.Errorf("the operator answered %d hints read past the proxy", hintCount)
	}
}

// A listener that counts the connections it is offered and closes each at
// once, which is all a proxy that must never be used has to do.
type testCountingListener struct {
	listener net.Listener
	done     chan struct{}
	count    atomic.Int64
}

// Listens on the v4 loopback until the test ends.
func newTestCountingListener(t *testing.T) *testCountingListener {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	self := &testCountingListener{
		listener: listener,
		done:     make(chan struct{}),
	}
	go func() {
		defer close(self.done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			self.count.Add(1)
			conn.Close()
		}
	}()
	t.Cleanup(func() {
		listener.Close()
		<-self.done
	})
	return self
}

// The direct strategy a hint is read through is built from the settings the
// caller passed, without its extenders, VLESS servers and proxy, and with the
// DoH settings in force. It is not built from the strategy's own settings:
// there the internal DoH resolver wraps the caller's dial, proxy and all, and
// a request would still cross the proxy.
func TestDirectClientStrategyIsBuiltFromTheCallerSettings(t *testing.T) {
	operator := newTestHintOperator(t, nil)
	proxy := newTestCountingListener(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings := DefaultClientStrategySettings()
	// an operator domain, so the internal DoH resolver wraps the caller's dial
	settings.InternalDohDomains = []string{"api.space.example"}
	settings.ConnectSettings.ProxySettings = &ProxySettings{
		Network: "tcp",
		Address: proxy.listener.Addr().String(),
	}
	settings.ExtenderConfigs = []*ExtenderConfig{{
		Profile: ExtenderProfile{ConnectMode: ExtenderConnectModeTcpTls, Port: 443},
		Ip:      netip.MustParseAddr("192.0.2.1"),
	}}
	settings.VlessConfigs = []*VlessConfig{{
		Address:  "192.0.2.2",
		Port:     443,
		Id:       testVlessUserId,
		Network:  VlessNetworkTcp,
		Security: VlessSecurityNone,
	}}
	clientStrategy := NewClientStrategy(ctx, settings)
	defer clientStrategy.Close()
	// a user's bootstrap DoH servers, applied to the running strategy
	dohSettings := DefaultDohSettings()
	clientStrategy.SetInternalDohSettings(dohSettings)

	directClientStrategy := clientStrategy.newDirectClientStrategy(ctx)
	defer directClientStrategy.Close()
	if directClientStrategy.DohSettings() != dohSettings {
		t.Error("the direct strategy took the DoH settings it was built with, not the ones in force")
	}
	func() {
		directClientStrategy.mutex.Lock()
		defer directClientStrategy.mutex.Unlock()
		for dialer := range directClientStrategy.dialers {
			if dialer.extenderConfig != nil || dialer.vlessConfig != nil {
				t.Errorf("the direct strategy holds the relay dialer %q", dialer.description)
			}
		}
	}()

	requestCtx, requestCancel := context.WithTimeout(ctx, 5*time.Second)
	defer requestCancel()
	if _, err := HttpGetWithStrategyRaw(requestCtx, directClientStrategy, operator.url()+"/direct", ""); err != nil {
		t.Errorf("the direct strategy did not reach the operator: %s", err)
	}
	if count := proxy.count.Load(); count != 0 {
		t.Errorf("the direct strategy dialed through the proxy %d times", count)
	}
}
