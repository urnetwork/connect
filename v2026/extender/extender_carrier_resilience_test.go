// What a carrier does when it fails, beyond surviving it
// (extender_carrier_regression_test.go): what a retried accept costs, what the
// server says while a carrier is gone, and how a lost carrier comes back (EXTENDER.md
// G2).

package extender

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

// Short enough that a test does not wait out a rebuild, and a test holds the
// rebuild itself when it wants to look at the time between.
func testFastCarrierRebuild(settings *ExtenderSettings) {
	settings.CarrierRebuildMinBackoff = 10 * time.Millisecond
	settings.CarrierRebuildMaxBackoff = 40 * time.Millisecond
	settings.CarrierRebuildStableTimeout = time.Hour
}

// Polls until `done`, which a test uses for what the server does on its own time.
func testEventually(t *testing.T, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(testCarrierProbeTimeout)
	for !done() {
		if deadline.Before(time.Now()) {
			t.Fatalf("%s: timed out", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// Waits for the carrier to be served, or not served.
func (self *testCarrierFixture) waitCarrier(carrier string, serving bool) {
	self.t.Helper()
	testEventually(self.t, fmt.Sprintf("%s serving = %t", carrier, serving), func() bool {
		return slices.Contains(self.server.Carriers(), carrier) == serving
	})
}

// A connection that wakes an Accept blocked on the real listener, so a scripted
// answer is read. It is accepted, and closed by the handshake it never starts. An
// Accept that had not started yet has read the answer already, and may have
// closed the listener by now, so a refused dial is not a failure here.
func (self *testCarrierFixture) wakeAccept() {
	self.t.Helper()
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(self.ip.String(), fmt.Sprint(self.tcpPort)), testCarrierProbeTimeout)
	if err == nil {
		conn.Close()
	}
}

// What accept can answer is told apart by whether a retry on the same listener
// can help.
func TestExtenderAcceptErrorIsTransient(t *testing.T) {
	transient := []error{
		testAcceptError(syscall.EMFILE),
		testAcceptError(syscall.ENFILE),
		testAcceptError(syscall.ECONNABORTED),
		testAcceptError(syscall.ECONNRESET),
		testAcceptError(syscall.EINTR),
		testAcceptError(syscall.ETIMEDOUT),
		testAcceptError(syscall.ENOBUFS),
		testAcceptError(syscall.ENOMEM),
		testAcceptError(syscall.EPROTO),
		testAcceptError(syscall.EPERM),
		testAcceptError(syscall.ENETDOWN),
		testAcceptError(syscall.ENETUNREACH),
		testAcceptError(syscall.EHOSTUNREACH),
		// what winsock numbers them, which syscall's names do not match on windows
		testAcceptError(syscall.Errno(10024)),
		testAcceptError(syscall.Errno(10053)),
		testAcceptError(syscall.Errno(10054)),
		testAcceptError(syscall.Errno(10055)),
		testAcceptError(syscall.Errno(10060)),
		// unwrapped, and through another's wrapping, as a seam may return it
		syscall.EMFILE,
		fmt.Errorf("the listener: %w", syscall.ENFILE),
		&net.OpError{Op: "accept", Err: os.ErrDeadlineExceeded},
	}
	for _, err := range transient {
		if !extenderAcceptErrorIsTransient(err) {
			t.Errorf("%v reads as the listener's end", err)
		}
	}
	ended := []error{
		net.ErrClosed,
		&net.OpError{Op: "accept", Err: net.ErrClosed},
		testAcceptError(syscall.EBADF),
		testAcceptError(syscall.EINVAL),
		testAcceptError(syscall.ENOTSOCK),
		errors.New("something this does not know"),
		io.EOF,
	}
	for _, err := range ended {
		if extenderAcceptErrorIsTransient(err) {
			t.Errorf("%v reads as passing", err)
		}
	}
}

// An accept that keeps failing is waited out on a doubling backoff, not spun on,
// and on the same listener: nothing is bound again. Since it does not go on for
// the down timeout, the carrier stays listed throughout.
func TestExtenderAcceptRetryBacksOff(t *testing.T) {
	const window = 400 * time.Millisecond
	changes := &atomic.Int64{}
	fixture := newTestCarrierFixture(t, nil, func(_ *testCarrierFixture, settings *ExtenderSettings) {
		settings.AcceptDownTimeout = time.Hour
		settings.CarriersChangeHandler = func() { changes.Add(1) }
	})
	listener := fixture.currentListener()
	listener.setStandingErr(testAcceptError(syscall.EMFILE))
	fixture.wakeAccept()

	testEventually(t, "the accept to fail", func() bool {
		// 2: the accept the connection woke, and the one that failed
		return 2 < listener.acceptCount.Load()
	})
	startCount := listener.acceptCount.Load()
	time.Sleep(window)
	retries := listener.acceptCount.Load() - startCount
	// 5, 10, 20, 40, 80, 160 and 320 ms: seven at the most from a backoff that
	// just started, and thousands from a loop with none
	if 8 < retries {
		t.Fatalf("%d accepts in %s, expected a backoff", retries, window)
	}
	if !slices.Contains(fixture.server.Carriers(), connect.ExtenderCarrierTcp) {
		t.Fatal("a short run of accept errors took the carrier out of the list")
	}

	// it recovers with no bind and no change anyone had to hear of
	listener.setStandingErr(nil)
	fixture.expectServing("after the errors stopped", connect.ExtenderCarrierTcp)
	if count := fixture.tcpBindCount.Load(); count != 1 {
		t.Fatalf("binds = %d, expected the same listener", count)
	}
	if count := changes.Load(); count != 0 {
		t.Fatalf("carrier changes = %d, expected none", count)
	}
}

// An accept that goes on failing serves nothing, however bound it is, so the
// carrier says so with the cause, and is back with its next connection. The
// listener is kept: a process out of file descriptors could not bind another.
func TestExtenderSustainedAcceptFailureIsReported(t *testing.T) {
	changes := &atomic.Int64{}
	reported := make(chan error, 8)
	fixture := newTestCarrierFixture(t, nil, func(_ *testCarrierFixture, settings *ExtenderSettings) {
		settings.AcceptRetryMaxBackoff = 10 * time.Millisecond
		settings.AcceptDownTimeout = 50 * time.Millisecond
		settings.CarriersChangeHandler = func() { changes.Add(1) }
		settings.ListenErrorHandler = func(carrier string, err error) {
			select {
			case reported <- fmt.Errorf("%s: %w", carrier, err):
			default:
			}
		}
	})
	listener := fixture.currentListener()
	listener.setStandingErr(testAcceptError(syscall.EMFILE))
	fixture.wakeAccept()

	fixture.waitCarrier(connect.ExtenderCarrierTcp, false)
	if err := fixture.server.ListenErrors()[connect.ExtenderCarrierTcp]; !errors.Is(err, syscall.EMFILE) {
		t.Fatalf("listen error = %v, expected the accept's", err)
	}
	select {
	case err := <-reported:
		if !errors.Is(err, syscall.EMFILE) || !strings.HasPrefix(err.Error(), connect.ExtenderCarrierTcp) {
			t.Fatalf("reported %v", err)
		}
	case <-time.After(testCarrierProbeTimeout):
		t.Fatal("the lost carrier was not reported")
	}
	if carriers := fixture.server.Carriers(); !slices.Equal(carriers, []string{connect.ExtenderCarrierQuic, connect.ExtenderCarrierDns}) {
		t.Fatalf("carriers = %v, expected the udp carriers alone", carriers)
	}

	// no connection is needed for it to come back: a revoked extender is sent
	// no uptime probes, and would wait on one forever
	listener.setStandingErr(nil)
	fixture.waitCarrier(connect.ExtenderCarrierTcp, true)
	if err := fixture.server.ListenErrors()[connect.ExtenderCarrierTcp]; err != nil {
		t.Fatalf("listen error = %v after the carrier came back", err)
	}
	fixture.expectServing("after it came back", testEveryCarrier...)
	if count := fixture.tcpBindCount.Load(); count != 1 {
		t.Fatalf("binds = %d, expected the same listener", count)
	}
	// each is told once, however many retries the loss lasted
	if count := changes.Load(); count != 2 {
		t.Fatalf("carrier changes = %d, expected the loss and the return", count)
	}
	fixture.expectNotEnded("after it came back")
}

// A listener that ended is bound again, and the udp carriers never notice. While
// the bind is held the status is what is true: no tcp, and why.
func TestExtenderRebuildsALostTcpCarrier(t *testing.T) {
	releaseBind := make(chan struct{})
	fixture := newTestCarrierFixture(t, nil, func(fixture *testCarrierFixture, settings *ExtenderSettings) {
		testFastCarrierRebuild(settings)
		listen := settings.Listen
		settings.Listen = func(network string, address string) (net.Listener, error) {
			if 1 <= fixture.tcpBindCount.Load() {
				<-releaseBind
			}
			return listen(network, address)
		}
	})
	listener := fixture.currentListener()
	// the listener closed under the server, which no retry can help
	listener.Listener.Close()

	fixture.waitCarrier(connect.ExtenderCarrierTcp, false)
	if err := fixture.server.ListenErrors()[connect.ExtenderCarrierTcp]; !errors.Is(err, net.ErrClosed) {
		t.Fatalf("listen error = %v, expected what ended the listener", err)
	}
	fixture.expectServing("while tcp is gone", connect.ExtenderCarrierQuic, connect.ExtenderCarrierDns)
	if _, err := fixture.probe(connect.ExtenderCarrierTcp); err == nil {
		t.Fatal("tcp answered with its listener gone")
	}
	// the answer a probe of another carrier gets is as true as the list
	response, err := fixture.probe(connect.ExtenderCarrierQuic)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(response.Carriers, connect.ExtenderCarrierTcp) {
		t.Fatalf("response carriers = %v, expected no tcp", response.Carriers)
	}

	close(releaseBind)
	fixture.waitCarrier(connect.ExtenderCarrierTcp, true)
	fixture.expectServing("after the rebuild", testEveryCarrier...)
	if len(fixture.server.ListenErrors()) != 0 {
		t.Fatalf("listen errors = %v after the rebuild", fixture.server.ListenErrors())
	}
	if count := fixture.tcpBindCount.Load(); count != 2 {
		t.Fatalf("binds = %d, expected one rebuild", count)
	}
	fixture.expectNotEnded("after the rebuild")
}

// A bind that keeps failing is retried on the backoff, each new reason told once,
// and the reason the status gives is the newest.
func TestExtenderRebuildRetriesAFailingBind(t *testing.T) {
	var reasons atomic.Value
	reasons.Store("first")
	attempts := &atomic.Int64{}
	reported := &recordedValues[string]{}
	fixture := newTestCarrierFixture(t, nil, func(fixture *testCarrierFixture, settings *ExtenderSettings) {
		testFastCarrierRebuild(settings)
		listen := settings.Listen
		settings.Listen = func(network string, address string) (net.Listener, error) {
			if 1 <= fixture.tcpBindCount.Load() {
				attempts.Add(1)
				if reason := reasons.Load().(string); reason != "" {
					return nil, errors.New(reason)
				}
			}
			return listen(network, address)
		}
		settings.ListenErrorHandler = func(carrier string, err error) {
			reported.add(carrier + ": " + err.Error())
		}
	})
	listener := fixture.currentListener()
	listener.Listener.Close()
	fixture.waitCarrier(connect.ExtenderCarrierTcp, false)

	testEventually(t, "retries", func() bool { return 3 <= attempts.Load() })
	reasons.Store("second")
	countAtChange := attempts.Load()
	testEventually(t, "retries with the new reason", func() bool { return countAtChange+3 <= attempts.Load() })
	if err := fixture.server.ListenErrors()[connect.ExtenderCarrierTcp]; err == nil || err.Error() != "second" {
		t.Fatalf("listen error = %v, expected the newest reason", err)
	}

	// at the 40 ms cap, 20 to 60 ms a try: no more than 15 in 300 ms
	before := attempts.Load()
	time.Sleep(300 * time.Millisecond)
	if tries := attempts.Load() - before; 16 < tries {
		t.Fatalf("%d binds in 300 ms, expected the backoff", tries)
	}
	fixture.expectServing("while tcp is gone", connect.ExtenderCarrierQuic)
	fixture.expectNotEnded("while tcp is gone")

	reasons.Store("")
	fixture.waitCarrier(connect.ExtenderCarrierTcp, true)
	fixture.expectServing("after the bind worked", connect.ExtenderCarrierTcp)

	// each reason once: what ended the listener, then the two the bind gave
	lines := reported.snapshot()
	if len(lines) != 3 ||
		!strings.HasSuffix(lines[0], net.ErrClosed.Error()) ||
		lines[1] != "tcp: first" ||
		lines[2] != "tcp: second" {
		t.Fatalf("reported %q", lines)
	}
}

// A listener that binds and dies at once is not bound in a loop: the backoff only
// starts over after a carrier has served for the stable timeout.
func TestExtenderRebuildOfACarrierThatDiesAtOnceBacksOff(t *testing.T) {
	binds := &atomic.Int64{}
	fixture := newTestCarrierFixture(t, nil, func(fixture *testCarrierFixture, settings *ExtenderSettings) {
		settings.CarrierRebuildMinBackoff = 10 * time.Millisecond
		settings.CarrierRebuildMaxBackoff = 80 * time.Millisecond
		settings.CarrierRebuildStableTimeout = time.Hour
		settings.Listen = func(network string, address string) (net.Listener, error) {
			binds.Add(1)
			return newTestScriptedListener(
				&testUnboundListener{},
				errors.New("the test's listener died"),
			), nil
		}
	})
	testEventually(t, "the first rebuilds", func() bool { return 3 <= binds.Load() })
	before := binds.Load()
	time.Sleep(400 * time.Millisecond)
	// at the 80 ms cap, 40 to 120 ms a try: no more than 11 in 400 ms
	if tries := binds.Load() - before; 12 < tries {
		t.Fatalf("%d binds in 400 ms, expected the backoff", tries)
	}
	fixture.expectServing("while tcp keeps dying", connect.ExtenderCarrierQuic, connect.ExtenderCarrierDns)
	fixture.expectNotEnded("while tcp keeps dying")
}

// What a listener needs in order to be closed. It is never accepted on.
type testUnboundListener struct{}

func (self *testUnboundListener) Accept() (net.Conn, error) { return nil, net.ErrClosed }
func (self *testUnboundListener) Close() error              { return nil }
func (self *testUnboundListener) Addr() net.Addr {
	return &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 18443}
}

// A panic in one carrier's accept ends that carrier alone, not the server. It
// used to cancel the server.
func TestExtenderCarrierPanicIsRebuilt(t *testing.T) {
	fixture := newTestCarrierFixture(t, nil, func(_ *testCarrierFixture, settings *ExtenderSettings) {
		testFastCarrierRebuild(settings)
	})
	listener := fixture.currentListener()
	listener.setPanicNext()
	fixture.wakeAccept()

	select {
	case <-listener.closed:
	case <-time.After(testCarrierProbeTimeout):
		t.Fatal("the panicked carrier did not release its listener")
	}
	testEventually(t, "the rebuild", func() bool { return fixture.tcpBindCount.Load() == 2 })
	fixture.waitCarrier(connect.ExtenderCarrierTcp, true)
	fixture.expectServing("after the panic", testEveryCarrier...)
	fixture.expectNotEnded("after the panic")
}

// An owner's handler that panics costs the owner the news and nothing else: the panic
// used to take the process, since supervision has no recovery of its own around
// them, and the port goes on being rebuilt.
func TestExtenderCarrierOwnerHandlerPanicIsContained(t *testing.T) {
	listenErrorCalls := &atomic.Int64{}
	carriersChangeCalls := &atomic.Int64{}
	fixture := newTestCarrierFixture(t, nil, func(_ *testCarrierFixture, settings *ExtenderSettings) {
		testFastCarrierRebuild(settings)
		settings.ListenErrorHandler = func(carrier string, err error) {
			listenErrorCalls.Add(1)
			panic("the test's panic in an owner's handler")
		}
		settings.CarriersChangeHandler = func() {
			carriersChangeCalls.Add(1)
			panic("the test's panic in an owner's handler")
		}
	})
	fixture.currentListener().Listener.Close()

	testEventually(t, "the rebuild", func() bool { return fixture.tcpBindCount.Load() == 2 })
	fixture.waitCarrier(connect.ExtenderCarrierTcp, true)
	testEventually(t, "the handlers", func() bool {
		// the loss and the return, and the reason for the loss
		return carriersChangeCalls.Load() == 2 && listenErrorCalls.Load() == 1
	})
	fixture.expectServing("after the panics", testEveryCarrier...)
	fixture.expectNotEnded("after the panics")

	// and it is still supervised
	fixture.currentListener().Listener.Close()
	testEventually(t, "the second rebuild", func() bool { return fixture.tcpBindCount.Load() == 3 })
	fixture.waitCarrier(connect.ExtenderCarrierTcp, true)
}

// A udp carrier whose socket fails is bound again and answers again, and tcp never
// stopped. Its fellow udp carrier is another socket and never noticed either.
func TestExtenderRebuildsALostUdpCarrier(t *testing.T) {
	for _, carrier := range []string{connect.ExtenderCarrierQuic, connect.ExtenderCarrierDns} {
		t.Run(carrier, func(t *testing.T) {
			releaseBind := make(chan struct{})
			fixture := newTestCarrierFixture(t, nil, func(fixture *testCarrierFixture, settings *ExtenderSettings) {
				testFastCarrierRebuild(settings)
				listenPacket := settings.ListenPacket
				settings.ListenPacket = func(network string, address string) (net.PacketConn, error) {
					if address == fmt.Sprintf(":%d", testCarrierPort(fixture, carrier)) && 1 <= testCarrierBindCount(fixture, carrier).Load() {
						<-releaseBind
					}
					return listenPacket(network, address)
				}
			})
			other := connect.ExtenderCarrierDns
			if carrier == connect.ExtenderCarrierDns {
				other = connect.ExtenderCarrierQuic
			}
			packetConn := fixture.currentQuicConn()
			if carrier == connect.ExtenderCarrierDns {
				packetConn = fixture.currentDnsConn()
			}
			packetConn.failNextRead(errors.New("the test's socket failed"))

			fixture.waitCarrier(carrier, false)
			if err := fixture.server.ListenErrors()[carrier]; err == nil || !strings.Contains(err.Error(), "the test's socket failed") {
				t.Fatalf("listen error = %v, expected the socket's", err)
			}
			if carrier == connect.ExtenderCarrierDns && len(fixture.server.DnsPorts()) != 0 {
				t.Fatalf("dns ports = %v with the carrier gone", fixture.server.DnsPorts())
			}
			fixture.expectServing("while it is gone", connect.ExtenderCarrierTcp, other)

			close(releaseBind)
			fixture.waitCarrier(carrier, true)
			fixture.expectServing("after the rebuild", testEveryCarrier...)
			if carrier == connect.ExtenderCarrierDns && !slices.Equal(fixture.server.DnsPorts(), []int{fixture.dnsPort}) {
				t.Fatalf("dns ports = %v after the rebuild", fixture.server.DnsPorts())
			}
			if count := testCarrierBindCount(fixture, carrier).Load(); count != 2 {
				t.Fatalf("binds = %d, expected one rebuild", count)
			}
			fixture.expectNotEnded("after the rebuild")
		})
	}
}

func testCarrierPort(fixture *testCarrierFixture, carrier string) int {
	if carrier == connect.ExtenderCarrierQuic {
		return fixture.quicPort
	}
	return fixture.dnsPort
}

func testCarrierBindCount(fixture *testCarrierFixture, carrier string) *atomic.Int64 {
	if carrier == connect.ExtenderCarrierQuic {
		return &fixture.quicBindCount
	}
	return &fixture.dnsBindCount
}

// The dns carrier stays listed until its last port is lost: one port of two going
// takes that port out of what is advertised and nothing else (L2).
func TestExtenderLostDnsPortLeavesTheOther(t *testing.T) {
	// the first answer is a socket a test can fail, and a rebuild a plain one
	first, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { first.Close() })
	scripted := newTestScriptedPacketConn(first)
	releaseBind := make(chan struct{})
	binds := &atomic.Int64{}
	fixture := newDnsPortFixture(t, true, func() (net.PacketConn, error) {
		if binds.Add(1) == 1 {
			return scripted, nil
		}
		<-releaseBind
		return net.ListenPacket("udp", "127.0.0.1:0")
	})
	allPorts := []int{connect.DefaultDnsPort, fixture.dnsPort}
	slices.Sort(allPorts)
	if ports := fixture.server.DnsPorts(); !slices.Equal(ports, allPorts) {
		t.Fatalf("dns ports = %v", ports)
	}

	scripted.failNextRead(errors.New("the test's socket failed"))
	testEventually(t, "the port to go", func() bool {
		return slices.Equal(fixture.server.DnsPorts(), []int{fixture.dnsPort})
	})
	if carriers := fixture.server.Carriers(); !slices.Equal(carriers, []string{connect.ExtenderCarrierDns}) {
		t.Fatalf("carriers = %v, expected the carrier still served on its other port", carriers)
	}
	if listenErrs := fixture.server.ListenErrors(); 0 < len(listenErrs) {
		t.Fatalf("listen errors = %v, expected none for a serving carrier", listenErrs)
	}

	close(releaseBind)
	testEventually(t, "the port to return", func() bool {
		return slices.Equal(fixture.server.DnsPorts(), allPorts)
	})
}

// A rebuilt dns carrier leaves no forwarder workers behind. Each one starts a
// pool of them, which used to exit only with the server.
func TestExtenderDnsCarrierRebuildLeavesNoWorkers(t *testing.T) {
	const rebuilds = 4
	const workerCount = 32
	fixture := newTestCarrierFixture(t, nil, func(_ *testCarrierFixture, settings *ExtenderSettings) {
		testFastCarrierRebuild(settings)
		settings.DnsForwardWorkerCount = workerCount
	})
	fixture.expectServing("before", connect.ExtenderCarrierDns)
	before := runtime.NumGoroutine()

	for i := range rebuilds {
		bindCount := fixture.dnsBindCount.Load()
		fixture.currentDnsConn().failNextRead(errors.New("the test's socket failed"))
		testEventually(t, fmt.Sprintf("rebuild %d", i), func() bool {
			return bindCount < fixture.dnsBindCount.Load() &&
				slices.Contains(fixture.server.Carriers(), connect.ExtenderCarrierDns)
		})
	}
	fixture.expectServing("after", connect.ExtenderCarrierDns)

	// leaked workers would be rebuilds*workerCount; a pool in flight is one
	testEventually(t, "the old workers to exit", func() bool {
		return runtime.NumGoroutine() < before+workerCount
	})
}

// Close ends a rebuild that is waiting or binding at once, and nothing is bound
// after it. The server ends with no error and says it serves nothing.
func TestExtenderCloseEndsARebuild(t *testing.T) {
	fixture := newTestCarrierFixture(t, nil, func(_ *testCarrierFixture, settings *ExtenderSettings) {
		settings.CarrierRebuildMinBackoff = time.Hour
		settings.CarrierRebuildMaxBackoff = time.Hour
	})
	fixture.currentListener().Listener.Close()
	fixture.waitCarrier(connect.ExtenderCarrierTcp, false)

	done := make(chan struct{})
	go func() {
		fixture.server.CloseAndWait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(testCarrierProbeTimeout):
		t.Fatal("close did not end the rebuild's wait")
	}
	if count := fixture.tcpBindCount.Load(); count != 1 {
		t.Fatalf("binds = %d, expected none after the loss", count)
	}
	if carriers := fixture.server.Carriers(); len(carriers) != 0 {
		t.Fatalf("carriers = %v after close", carriers)
	}
	if ports := fixture.server.DnsPorts(); len(ports) != 0 {
		t.Fatalf("dns ports = %v after close", ports)
	}
	select {
	case <-fixture.server.Done():
	default:
		t.Fatal("done is open after close")
	}
}

// A server that is serving, however many carriers it has lost, has not ended.
func TestExtenderLosingEveryCarrierDoesNotEndTheServer(t *testing.T) {
	fixture := newTestCarrierFixture(t, nil, func(_ *testCarrierFixture, settings *ExtenderSettings) {
		settings.CarrierRebuildMinBackoff = time.Hour
		settings.CarrierRebuildMaxBackoff = time.Hour
	})
	fixture.currentListener().Listener.Close()
	fixture.currentQuicConn().failNextRead(errors.New("the test's socket failed"))
	fixture.currentDnsConn().failNextRead(errors.New("the test's socket failed"))
	testEventually(t, "every carrier to go", func() bool {
		return len(fixture.server.Carriers()) == 0
	})
	fixture.expectNotEnded("with every carrier gone")
	select {
	case <-fixture.server.Done():
		t.Fatal("done closed with the server still rebuilding")
	default:
	}
}

// The wait before a rebuild spreads a fleet that lost its carriers together.
func TestExtenderCarrierRebuildWaitHasJitter(t *testing.T) {
	const backoff = 1 * time.Second
	if wait := extenderCarrierRebuildWait(0); wait != 0 {
		t.Fatalf("wait of no backoff = %s", wait)
	}
	waits := map[time.Duration]bool{}
	for range 1000 {
		wait := extenderCarrierRebuildWait(backoff)
		if wait < backoff/2 || backoff*3/2 <= wait {
			t.Fatalf("wait = %s, expected half to one and a half of %s", wait, backoff)
		}
		waits[wait] = true
	}
	if len(waits) < 900 {
		t.Fatalf("%d distinct waits of 1000", len(waits))
	}
}

// One carrier's end says why at error level, which is the line the next incident
// is explained by, and it names the carrier and the port.
func TestExtenderCarrierEndIsLoggedAtErrorLevel(t *testing.T) {
	log := &testCarrierLog{}
	connect.SetDefaultLogger(log)
	t.Cleanup(func() { connect.SetDefaultLogger(nil) })

	fixture := newTestCarrierFixture(t, nil, func(_ *testCarrierFixture, settings *ExtenderSettings) {
		settings.CarrierRebuildMinBackoff = time.Hour
		settings.CarrierRebuildMaxBackoff = time.Hour
		settings.AcceptDownTimeout = time.Hour
	})
	listener := fixture.currentListener()
	listener.setStandingErr(testAcceptError(syscall.EMFILE))
	fixture.wakeAccept()
	testEventually(t, "the accept error's line", func() bool {
		return 0 < log.errorLineCount(fmt.Sprintf("tcp port %d", fixture.tcpPort), "too many open files")
	})
	listener.setStandingErr(errors.New("the listener died"))
	testEventually(t, "the carrier's end", func() bool {
		return 0 < log.errorLineCount(fmt.Sprintf("tcp carrier on port %d", fixture.tcpPort), "the listener died")
	})
	fixture.currentQuicConn().failNextRead(errors.New("the socket died"))
	testEventually(t, "the udp carrier's end", func() bool {
		return 0 < log.errorLineCount(fmt.Sprintf("quic carrier on port %d", fixture.quicPort), "the socket died")
	})
	// retries are not a line each, here at the 1 s cap of the retry
	if count := log.errorLineCount("too many open files"); count != 1 {
		t.Fatalf("%d lines of the accept error, expected one an interval", count)
	}
}

// A logger that keeps the error lines.
type testCarrierLog struct {
	stateLock sync.Mutex
	lines     []string
}

func (self *testCarrierLog) Info(args ...any)                    {}
func (self *testCarrierLog) Infof(format string, args ...any)    {}
func (self *testCarrierLog) Warningf(format string, args ...any) {}
func (self *testCarrierLog) Errorf(format string, args ...any) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.lines = append(self.lines, fmt.Sprintf(format, args...))
}
func (self *testCarrierLog) V(level int32) connect.Verbose { return testCarrierVerbose{} }

type testCarrierVerbose struct{}

func (self testCarrierVerbose) Enabled() bool                    { return false }
func (self testCarrierVerbose) Info(args ...any)                 {}
func (self testCarrierVerbose) Infof(format string, args ...any) {}

func (self *testCarrierLog) errorLineCount(contains ...string) int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	count := 0
	for _, line := range self.lines {
		matches := true
		for _, text := range contains {
			matches = matches && strings.Contains(line, text)
		}
		if matches {
			count += 1
		}
	}
	return count
}
