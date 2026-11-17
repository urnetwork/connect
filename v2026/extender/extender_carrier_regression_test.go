// The extender server must outlive a failure of one of its carriers (EXTENDER.md
// G2). On 2026-11-01 most of one operator's extenders stopped answering on
// tcp 443 and udp 53 while their processes stayed connected and kept posting
// activations. One accept error, one udp carrier's exit or one failed write on
// the dns carrier closed every carrier for the life of the process, and nothing
// restarted the server, which is probably how, though no host's log confirms it.
// Each test here injects one of those failures and fails on the code before the
// fix, through the seams that code already had.

package extender

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
)

// How long a probe of a carrier that is expected to be serving is given. Long
// enough for quic to resend what a test made the server drop, short enough to
// fail a test that is about to anyway.
const testCarrierProbeTimeout = 15 * time.Second

// The error accept returns for a condition of the host, as the net package
// builds it.
func testAcceptError(errno syscall.Errno) error {
	return &net.OpError{Op: "accept", Net: "tcp", Err: os.NewSyscallError("accept", errno)}
}

// A listener whose Accept answers the scripted errors first and then the real
// listener. An error added while a real Accept is blocked is answered after the
// next connection.
type testScriptedListener struct {
	net.Listener

	acceptCount atomic.Int64

	stateLock sync.Mutex
	errs      []error
	// answers every Accept with this error while it is set
	standingErr error
	// panics in the next Accept
	panicNext bool
	// closed by the first Close
	closed    chan struct{}
	closeOnce sync.Once
}

func newTestScriptedListener(listener net.Listener, errs ...error) *testScriptedListener {
	return &testScriptedListener{
		Listener: listener,
		errs:     errs,
		closed:   make(chan struct{}),
	}
}

func (self *testScriptedListener) Accept() (net.Conn, error) {
	self.acceptCount.Add(1)
	err, shouldPanic := func() (error, bool) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.panicNext {
			self.panicNext = false
			return nil, true
		}
		if self.standingErr != nil {
			return self.standingErr, false
		}
		if 0 < len(self.errs) {
			err := self.errs[0]
			self.errs = self.errs[1:]
			return err, false
		}
		return nil, false
	}()
	if shouldPanic {
		panic("the test's accept panicked")
	}
	if err != nil {
		return nil, err
	}
	return self.Listener.Accept()
}

func (self *testScriptedListener) Close() error {
	self.closeOnce.Do(func() {
		close(self.closed)
	})
	return self.Listener.Close()
}

func (self *testScriptedListener) setStandingErr(err error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.standingErr = err
}

func (self *testScriptedListener) setPanicNext() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.panicNext = true
}

// A packet socket whose next read or write can be made to fail. A read blocked on
// the real socket is not interrupted: failNextRead also wakes it.
type testScriptedPacketConn struct {
	net.PacketConn

	stateLock sync.Mutex
	readErrs  []error
	writeErrs []error
	// the errors that were answered, for a test to wait on
	readErrCount  atomic.Int64
	writeErrCount atomic.Int64
	writeCount    atomic.Int64

	wakeReads chan struct{}
	closed    chan struct{}
	closeOnce sync.Once
}

func newTestScriptedPacketConn(packetConn net.PacketConn) *testScriptedPacketConn {
	return &testScriptedPacketConn{
		PacketConn: packetConn,
		wakeReads:  make(chan struct{}, 1),
		closed:     make(chan struct{}),
	}
}

// Makes the next read fail, including the one that is blocked now.
func (self *testScriptedPacketConn) failNextRead(err error) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.readErrs = append(self.readErrs, err)
	}()
	select {
	case self.wakeReads <- struct{}{}:
	default:
	}
	// a read blocked in the kernel sees this as a timeout, which ReadFrom below
	// turns into the scripted error
	self.PacketConn.SetReadDeadline(time.Now())
}

func (self *testScriptedPacketConn) failNextWrites(errs ...error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.writeErrs = append(self.writeErrs, errs...)
}

func (self *testScriptedPacketConn) takeReadErr() error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if len(self.readErrs) == 0 {
		return nil
	}
	err := self.readErrs[0]
	self.readErrs = self.readErrs[1:]
	return err
}

func (self *testScriptedPacketConn) ReadFrom(p []byte) (int, net.Addr, error) {
	for {
		if err := self.takeReadErr(); err != nil {
			self.readErrCount.Add(1)
			self.PacketConn.SetReadDeadline(time.Time{})
			return 0, nil, err
		}
		n, addr, err := self.PacketConn.ReadFrom(p)
		if err != nil {
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				if scripted := self.takeReadErr(); scripted != nil {
					self.readErrCount.Add(1)
					self.PacketConn.SetReadDeadline(time.Time{})
					return 0, nil, scripted
				}
				// a deadline the owner set is the owner's
				return n, addr, err
			}
		}
		return n, addr, err
	}
}

func (self *testScriptedPacketConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	self.writeCount.Add(1)
	err := func() error {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if len(self.writeErrs) == 0 {
			return nil
		}
		err := self.writeErrs[0]
		self.writeErrs = self.writeErrs[1:]
		return err
	}()
	if err != nil {
		self.writeErrCount.Add(1)
		return 0, err
	}
	return self.PacketConn.WriteTo(p, addr)
}

func (self *testScriptedPacketConn) Close() error {
	self.closeOnce.Do(func() {
		close(self.closed)
	})
	return self.PacketConn.Close()
}

// One open extender with an identity, whose tcp listener and udp sockets a test
// can make fail. Its sockets are the fixture's until they are lost; a bind after
// that is a new socket on the same port, as a real bind would be.
type testCarrierFixture struct {
	*extenderFixture

	publicKey []byte

	stateLock sync.Mutex
	listener  *testScriptedListener
	quicConn  *testScriptedPacketConn
	dnsConn   *testScriptedPacketConn

	tcpBindCount  atomic.Int64
	quicBindCount atomic.Int64
	dnsBindCount  atomic.Int64
}

func newTestCarrierFixture(
	t *testing.T,
	acceptErrs []error,
	configure func(fixture *testCarrierFixture, settings *ExtenderSettings),
) *testCarrierFixture {
	t.Helper()
	seed, err := connect.NewExtenderKeySeed()
	if err != nil {
		t.Fatal(err)
	}
	publicKey, err := connect.ExtenderPublicKeyFromSeed(seed)
	if err != nil {
		t.Fatal(err)
	}
	carrierFixture := &testCarrierFixture{publicKey: publicKey}
	newExtenderFixtureWithSetup(
		t,
		"127.0.0.1",
		nil,
		nil,
		func(fixture *extenderFixture, settings *ExtenderSettings) {
			// set before the server starts, which is what the seams read it on
			carrierFixture.extenderFixture = fixture
			settings.IdentityKeySeed = seed

			listen := settings.Listen
			settings.Listen = func(network string, address string) (net.Listener, error) {
				if carrierFixture.tcpBindCount.Add(1) == 1 {
					listener, err := listen(network, address)
					if err != nil {
						return nil, err
					}
					scripted := newTestScriptedListener(listener, acceptErrs...)
					carrierFixture.setListener(scripted)
					return scripted, nil
				}
				listener, err := net.Listen("tcp", net.JoinHostPort(fixture.ip.String(), fmt.Sprint(fixture.tcpPort)))
				if err != nil {
					return nil, err
				}
				scripted := newTestScriptedListener(listener)
				carrierFixture.setListener(scripted)
				return scripted, nil
			}

			listenPacket := settings.ListenPacket
			settings.ListenPacket = func(network string, address string) (net.PacketConn, error) {
				quic := address == fmt.Sprintf(":%d", fixture.quicPort)
				bindCount := &carrierFixture.dnsBindCount
				port := fixture.dnsPort
				if quic {
					bindCount = &carrierFixture.quicBindCount
					port = fixture.quicPort
				}
				var packetConn net.PacketConn
				var err error
				if bindCount.Add(1) == 1 {
					packetConn, err = listenPacket(network, address)
				} else {
					packetConn, err = net.ListenPacket("udp", net.JoinHostPort(fixture.ip.String(), fmt.Sprint(port)))
				}
				if err != nil {
					return nil, err
				}
				scripted := newTestScriptedPacketConn(packetConn)
				carrierFixture.setPacketConn(quic, scripted)
				return scripted, nil
			}

			if configure != nil {
				configure(carrierFixture, settings)
			}
		},
	)
	select {
	case <-carrierFixture.server.Listening():
	case <-time.After(testCarrierProbeTimeout):
		t.Fatal("the extender carriers did not bind")
	}
	return carrierFixture
}

func (self *testCarrierFixture) setListener(listener *testScriptedListener) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.listener = listener
}

func (self *testCarrierFixture) currentListener() *testScriptedListener {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.listener
}

func (self *testCarrierFixture) setPacketConn(quic bool, packetConn *testScriptedPacketConn) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if quic {
		self.quicConn = packetConn
	} else {
		self.dnsConn = packetConn
	}
}

func (self *testCarrierFixture) currentQuicConn() *testScriptedPacketConn {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.quicConn
}

func (self *testCarrierFixture) currentDnsConn() *testScriptedPacketConn {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.dnsConn
}

// Runs the operator's probe of one carrier: a fresh challenge, answered under the
// identity key (C2, C3). This is what the uptime probe and the activation run.
func (self *testCarrierFixture) probe(carrier string) (*protocol.ExtenderResponse, error) {
	self.t.Helper()
	port, dnsTld := testProbeEndpoint(self.extenderFixture, carrier)
	ctx, cancel := context.WithTimeout(context.Background(), testCarrierProbeTimeout)
	defer cancel()
	return connect.ProbeExtenderCarrier(
		ctx,
		self.connectSettings(),
		self.ip,
		testProbeConnectMode(carrier),
		port,
		dnsTld,
		testServerName,
		self.publicKey,
		"dest.example",
		443,
	)
}

// Fails the test unless every named carrier answers its probe.
func (self *testCarrierFixture) expectServing(what string, carriers ...string) {
	self.t.Helper()
	for _, carrier := range carriers {
		if _, err := self.probe(carrier); err != nil {
			self.t.Fatalf("%s: the %s carrier did not answer: %v", what, carrier, err)
		}
	}
}

// Fails the test if the server has stopped serving.
func (self *testCarrierFixture) expectNotEnded(what string) {
	self.t.Helper()
	select {
	case err := <-self.serveDone:
		self.t.Fatalf("%s: ListenAndServe ended with %v", what, err)
	default:
	}
}

var testEveryCarrier = []string{
	connect.ExtenderCarrierTcp,
	connect.ExtenderCarrierQuic,
	connect.ExtenderCarrierDns,
}

// One accept error used to close every carrier: the listener's goroutine
// returned, and it closed the whole server on its way out.
func TestExtenderSurvivesAnAcceptError(t *testing.T) {
	for _, errno := range []syscall.Errno{syscall.EMFILE, syscall.ENFILE, syscall.ECONNABORTED} {
		t.Run(errno.Error(), func(t *testing.T) {
			fixture := newTestCarrierFixture(t, []error{testAcceptError(errno)}, nil)
			// the first Accept has answered the error by the time this connects
			fixture.expectServing("after an accept error", testEveryCarrier...)
			fixture.expectNotEnded("after an accept error")
		})
	}
}

// The udp carrier's socket failing used to close every carrier too: its goroutine
// closed the whole server on its way out, so tcp 443 went with udp 443.
func TestExtenderSurvivesAUdpCarrierExit(t *testing.T) {
	fixture := newTestCarrierFixture(t, nil, nil)
	fixture.expectServing("before", testEveryCarrier...)

	quicConn := fixture.currentQuicConn()
	quicConn.failNextRead(errors.New("the test's socket failed"))
	select {
	case <-quicConn.closed:
	case <-time.After(testCarrierProbeTimeout):
		t.Fatal("the quic carrier did not end on its socket's failure")
	}

	fixture.expectServing("after the quic carrier ended", connect.ExtenderCarrierTcp, connect.ExtenderCarrierDns)
	fixture.expectNotEnded("after the quic carrier ended")
}

// One failed write on the dns carrier's socket used to end the packet translation
// every client of the carrier shares, and with it the carrier and every other
// carrier. A write fails for one address: a source port of zero, a source the
// host has no route to, a firewall rule; or for a moment, while a path is down.
func TestExtenderSurvivesADnsWriteFailure(t *testing.T) {
	for _, writeErr := range []error{
		syscall.EINVAL,
		syscall.ENETUNREACH,
		syscall.EPERM,
		syscall.ENOBUFS,
	} {
		t.Run(writeErr.Error(), func(t *testing.T) {
			fixture := newTestCarrierFixture(t, nil, nil)
			dnsConn := fixture.currentDnsConn()
			dnsConn.failNextWrites(writeErr)

			// the probe's own traffic is what the write failed on; quic resends
			// what was dropped, so the probe itself succeeds
			fixture.expectServing("after a failed write", connect.ExtenderCarrierDns)
			if count := dnsConn.writeErrCount.Load(); count != 1 {
				t.Fatalf("failed writes = %d, expected the probe to have reached the failure", count)
			}
			fixture.expectServing("after a failed write", connect.ExtenderCarrierTcp, connect.ExtenderCarrierQuic)
			fixture.expectNotEnded("after a failed write")
		})
	}
}

// A failed write on the quic carrier ends at most that one client's connection,
// which quic-go closes: the socket and the carrier are not touched, and the next
// client is served. This one held before the fix too, and is here so that it goes
// on holding.
func TestExtenderSurvivesAQuicWriteFailure(t *testing.T) {
	fixture := newTestCarrierFixture(t, nil, nil)
	quicConn := fixture.currentQuicConn()
	quicConn.failNextWrites(syscall.ENETUNREACH)

	// this one may or may not be the client whose connection it ends
	fixture.probe(connect.ExtenderCarrierQuic)
	if count := quicConn.writeErrCount.Load(); count != 1 {
		t.Fatalf("failed writes = %d, expected the probe to have reached the failure", count)
	}
	fixture.expectServing("after a failed write", testEveryCarrier...)
	fixture.expectNotEnded("after a failed write")
	if count := fixture.quicBindCount.Load(); count != 1 {
		t.Fatalf("quic binds = %d, expected the same socket", count)
	}
}
