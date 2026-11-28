package connect

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"
	"time"
)

// What a decode53 socket does with a failure that is not its end (EXTENDER.md G2).
// An extender's dns carrier serves every client on one socket, so a datagram it
// cannot write, or a read that fails for a moment, costs that datagram. Before
// 2026-11-02 one such write would end the translation, and the extender's whole
// server with it.

// A socket whose next writes and reads answer scripted errors.
type dnsCarrierFailingConn struct {
	*memoryPacketConn

	stateLock sync.Mutex
	writeErrs []error
	readErrs  []error

	writeAttempts atomic.Int64
	readAttempts  atomic.Int64
}

func (self *dnsCarrierFailingConn) failWrites(errs ...error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.writeErrs = append(self.writeErrs, errs...)
}

func (self *dnsCarrierFailingConn) failReads(errs ...error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.readErrs = append(self.readErrs, errs...)
}

func (self *dnsCarrierFailingConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	self.writeAttempts.Add(1)
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
		return 0, err
	}
	return self.memoryPacketConn.WriteTo(p, addr)
}

func (self *dnsCarrierFailingConn) ReadFrom(p []byte) (int, net.Addr, error) {
	self.readAttempts.Add(1)
	err := func() error {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if len(self.readErrs) == 0 {
			return nil
		}
		err := self.readErrs[0]
		self.readErrs = self.readErrs[1:]
		return err
	}()
	if err != nil {
		return 0, nil, err
	}
	return self.memoryPacketConn.ReadFrom(p)
}

// One translation over a socket a test can fail, and the peer it talks to.
type dnsCarrierFailingFixture struct {
	conn        *dnsCarrierFailingConn
	peer        *memoryPacketConn
	translation *packetTranslation
	tld         []byte
}

func newDnsCarrierFailingFixture(
	t *testing.T,
	mode PacketTranslationMode,
	prepare func(conn *dnsCarrierFailingConn),
) *dnsCarrierFailingFixture {
	t.Helper()
	return newDnsCarrierFailingFixtureWithSettings(t, mode, prepare, nil)
}

func newDnsCarrierFailingFixtureWithSettings(
	t *testing.T,
	mode PacketTranslationMode,
	prepare func(conn *dnsCarrierFailingConn),
	configure func(settings *PacketTranslationSettings),
) *dnsCarrierFailingFixture {
	t.Helper()
	tld := []byte("pt.example.")
	network := newMemoryPacketNetwork()
	conn := &dnsCarrierFailingConn{
		memoryPacketConn: network.listen(dnsCarrierTestAddr("192.0.2.1", 4053)),
	}
	if prepare != nil {
		prepare(conn)
	}
	peer := network.listen(dnsCarrierTestAddr("192.0.2.10", 1053))
	t.Cleanup(func() { peer.Close() })
	settings := dnsCarrierTestSettings(tld)
	if configure != nil {
		configure(settings)
	}
	translation, err := NewPacketTranslation(t.Context(), mode, conn, settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { translation.Close() })
	return &dnsCarrierFailingFixture{
		conn:        conn,
		peer:        peer,
		translation: translation,
		tld:         tld,
	}
}

// Fails the test unless the translation has ended within the wait.
func (self *dnsCarrierFailingFixture) waitEnded(t *testing.T) {
	t.Helper()
	select {
	case <-self.translation.ctx.Done():
	case <-time.After(dnsCarrierTestWait):
		t.Fatal("the translation did not end")
	}
}

// Fails the test if the translation has ended.
func (self *dnsCarrierFailingFixture) expectAlive(t *testing.T, what string) {
	t.Helper()
	if err := self.translation.ctx.Err(); err != nil {
		t.Fatalf("%s: the translation ended: %v, %v", what, err, self.translation.EndError())
	}
}

// A datagram the socket refuses is lost, and that is all: the write answers as if
// the network had lost it, so a quic connection is not ended by it, and the next
// one goes out. The errors are what a socket says of one address or of a moment:
// a source port of zero, a firewall rule, a route that is gone, a full queue.
func TestPacketTranslationServerDropsADatagramItCannotWrite(t *testing.T) {
	for _, writeErr := range []error{
		syscall.EINVAL,
		syscall.EPERM,
		syscall.ENETUNREACH,
		syscall.EHOSTUNREACH,
		syscall.ENOBUFS,
		errors.New("a failure this does not know"),
	} {
		t.Run(writeErr.Error(), func(t *testing.T) {
			fixture := newDnsCarrierFailingFixture(t, PacketTranslationModeDecode53, nil)
			fixture.conn.failWrites(writeErr)

			if n, err := fixture.translation.WriteTo([]byte("the lost one"), fixture.peer.addr); err != nil || n != len("the lost one") {
				t.Fatalf("write = %d, %v, expected a lost datagram to answer as written", n, err)
			}
			if attempts := fixture.conn.writeAttempts.Load(); attempts != 1 {
				t.Fatalf("write attempts = %d", attempts)
			}
			fixture.expectAlive(t, "after the failed write")

			if n, err := fixture.translation.WriteTo([]byte("the next one"), fixture.peer.addr); err != nil || n != len("the next one") {
				t.Fatalf("next write = %d, %v", n, err)
			}
			fixture.conn.nextWrite(t, "the next one")
			fixture.expectAlive(t, "after the next write")
			if err := fixture.translation.EndError(); err != nil {
				t.Fatalf("end error = %v", err)
			}
		})
	}
}

// Every datagram failing costs a line an interval, and no spin: the encoder waits
// on the next datagram to write.
func TestPacketTranslationServerSurvivesAPathThatStaysDown(t *testing.T) {
	fixture := newDnsCarrierFailingFixture(t, PacketTranslationModeDecode53, nil)
	const count = 64
	errs := make([]error, count)
	for i := range errs {
		errs[i] = syscall.ENETUNREACH
	}
	fixture.conn.failWrites(errs...)
	for i := range count {
		if _, err := fixture.translation.WriteTo([]byte{byte(i)}, fixture.peer.addr); err != nil {
			t.Fatalf("write %d = %v", i, err)
		}
	}
	if attempts := fixture.conn.writeAttempts.Load(); attempts != count {
		t.Fatalf("write attempts = %d, expected one a datagram", attempts)
	}
	fixture.expectAlive(t, "after every write failed")
	time.Sleep(50 * time.Millisecond)
	if attempts := fixture.conn.writeAttempts.Load(); attempts != count {
		t.Fatalf("write attempts = %d with nothing to write", attempts)
	}
}

// A socket that is gone is the end, and the writer and the owner both hear why.
func TestPacketTranslationServerEndsOnAClosedSocket(t *testing.T) {
	fixture := newDnsCarrierFailingFixture(t, PacketTranslationModeDecode53, nil)
	closedErr := &net.OpError{Op: "write", Net: "udp", Err: net.ErrClosed}
	fixture.conn.failWrites(closedErr)

	if _, err := fixture.translation.WriteTo([]byte("too late"), fixture.peer.addr); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("write = %v, expected the closed socket", err)
	}
	fixture.waitEnded(t)
	if err := fixture.translation.EndError(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("end error = %v", err)
	}
}

// What net/http and quic-go read as temporary is waited out on the same socket,
// on a doubling backoff: nothing else paces a socket that keeps failing.
func TestPacketTranslationServerRetriesATemporaryRead(t *testing.T) {
	const failures = 5
	temporaryErr := &net.OpError{Op: "read", Net: "udp", Err: syscall.EMFILE}
	fixture := newDnsCarrierFailingFixture(t, PacketTranslationModeDecode53, func(conn *dnsCarrierFailingConn) {
		for range failures {
			conn.failReads(temporaryErr)
		}
	})

	// 5, 10, 20, 40 and 80 ms pass before the read that succeeds
	var buf [1024]byte
	header := dnsCarrierTestHeader(0x11, 1, 0)
	_, datagram, err := encodeDnsRequest(1, header, []byte("after the failures"), buf, fixture.tld)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.peer.WriteTo(datagram, fixture.conn.addr); err != nil {
		t.Fatal(err)
	}
	if err := fixture.translation.SetReadDeadline(time.Now().Add(dnsCarrierTestWait)); err != nil {
		t.Fatal(err)
	}
	var packetData [2048]byte
	n, _, err := fixture.translation.ReadFrom(packetData[:])
	if err != nil {
		t.Fatalf("read after the failures: %v", err)
	}
	if string(packetData[:n]) != "after the failures" {
		t.Fatalf("read %q", packetData[:n])
	}
	if attempts := fixture.conn.readAttempts.Load(); attempts != failures+2 {
		// the failures, the read of the datagram and the one now blocked
		t.Fatalf("read attempts = %d, expected %d", attempts, failures+2)
	}
	fixture.expectAlive(t, "after the temporary reads")
}

// A read that keeps failing temporarily is paced, not spun on.
func TestPacketTranslationServerReadRetryBacksOff(t *testing.T) {
	const scripted = 100000
	temporaryErr := &net.OpError{Op: "read", Net: "udp", Err: syscall.EMFILE}
	errs := make([]error, scripted)
	for i := range errs {
		errs[i] = temporaryErr
	}
	fixture := newDnsCarrierFailingFixture(t, PacketTranslationModeDecode53, func(conn *dnsCarrierFailingConn) {
		conn.failReads(errs...)
	})
	time.Sleep(300 * time.Millisecond)
	// 5, 10, 20, 40, 80 and 160 ms: seven reads at the most in the window
	if attempts := fixture.conn.readAttempts.Load(); 8 < attempts {
		t.Fatalf("%d reads in 300 ms, expected a backoff", attempts)
	}
	fixture.expectAlive(t, "while the reads fail")
}

// Any other read error is the socket's end, which its owner rebuilds, and the
// owner hears why.
func TestPacketTranslationServerEndsOnAnotherReadError(t *testing.T) {
	readErr := &net.OpError{Op: "read", Net: "udp", Err: syscall.EIO}
	fixture := newDnsCarrierFailingFixture(t, PacketTranslationModeDecode53, func(conn *dnsCarrierFailingConn) {
		conn.failReads(readErr)
	})
	fixture.waitEnded(t)
	if err := fixture.translation.EndError(); !errors.Is(err, syscall.EIO) {
		t.Fatalf("end error = %v", err)
	}
	if attempts := fixture.conn.readAttempts.Load(); attempts != 1 {
		t.Fatalf("read attempts = %d", attempts)
	}
}

// A client's translation is one dial's, and its failure is for the caller to act
// on: a write failure still ends it and is answered as a failure.
func TestPacketTranslationClientEndsOnAWriteFailure(t *testing.T) {
	fixture := newDnsCarrierFailingFixture(t, PacketTranslationModeDns, nil)
	fixture.conn.failWrites(syscall.ENETUNREACH)
	if _, err := fixture.translation.WriteTo([]byte("the client's"), fixture.peer.addr); !errors.Is(err, syscall.ENETUNREACH) {
		t.Fatalf("write = %v", err)
	}
	fixture.waitEnded(t)
}

// A client's read failure still ends it too, temporary or not.
func TestPacketTranslationClientEndsOnAReadFailure(t *testing.T) {
	temporaryErr := &net.OpError{Op: "read", Net: "udp", Err: syscall.EMFILE}
	fixture := newDnsCarrierFailingFixture(t, PacketTranslationModeDns, func(conn *dnsCarrierFailingConn) {
		conn.failReads(temporaryErr)
	})
	fixture.waitEnded(t)
}

// A socket whose every write fails stays up, however long that goes on: no count or
// duration of failed writes may end it (EXTENDER.md G2). This is a week of such
// failures on the fake clock, so a bound of any length fails it, and it is what must
// keep one from coming back.
func TestPacketTranslationServerOutlivesWritesThatAllFail(t *testing.T) {
	// the pools start a goroutine for good at their first use, which must be outside
	// the bubble
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		log := &dnsCarrierLogLines{}
		fixture := newDnsCarrierFailingFixtureWithSettings(t, PacketTranslationModeDecode53, nil, func(settings *PacketTranslationSettings) {
			settings.Log = log
		})
		const failures = 7 * 24 * 60
		for i := range failures {
			fixture.conn.failWrites(syscall.EINVAL)
			if n, err := fixture.translation.WriteTo([]byte("a datagram"), fixture.peer.addr); err != nil || n != 10 {
				t.Fatalf("write %d = %d, %v, expected a lost datagram to answer as written", i, n, err)
			}
			time.Sleep(time.Minute)
		}
		if attempts := fixture.conn.writeAttempts.Load(); attempts != failures {
			t.Fatalf("write attempts = %d, expected %d", attempts, failures)
		}
		fixture.expectAlive(t, "after a week of failed writes")
		if err := fixture.translation.EndError(); err != nil {
			t.Fatalf("end error = %v", err)
		}
		// each failure is on the record, at one line a minute
		if count := strings.Count(log.all(), "dropped a datagram"); count < failures/2 {
			t.Fatalf("%d lines of the failures, expected them on the record", count)
		}

		if _, err := fixture.translation.WriteTo([]byte("the next one"), fixture.peer.addr); err != nil {
			t.Fatalf("next write = %v", err)
		}
		fixture.conn.nextWrite(t, "the next one")
	})
}

// A datagram too long to read is what windows reports as a read error, and anyone can
// send one. It costs that datagram and no wait, and a line a minute however many
// arrive. These are winsock's number, as a real socket's error wraps it, on a socket
// that is not windows': a windows socket has not been run here.
func TestPacketTranslationServerDropsADatagramTooLongToRead(t *testing.T) {
	const tooLongCount = 1000
	log := &dnsCarrierLogLines{}
	tooLong := &net.OpError{Op: "read", Net: "udp", Err: os.NewSyscallError("wsarecvfrom", syscall.Errno(10040))}
	fixture := newDnsCarrierFailingFixtureWithSettings(t, PacketTranslationModeDecode53, func(conn *dnsCarrierFailingConn) {
		for range tooLongCount {
			conn.failReads(tooLong)
		}
	}, func(settings *PacketTranslationSettings) {
		settings.Log = log
	})

	var buf [1024]byte
	header := dnsCarrierTestHeader(0x12, 1, 0)
	_, datagram, err := encodeDnsRequest(1, header, []byte("after the long ones"), buf, fixture.tld)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.peer.WriteTo(datagram, fixture.conn.addr); err != nil {
		t.Fatal(err)
	}
	if err := fixture.translation.SetReadDeadline(time.Now().Add(dnsCarrierTestWait)); err != nil {
		t.Fatal(err)
	}
	var packetData [2048]byte
	n, _, err := fixture.translation.ReadFrom(packetData[:])
	if err != nil {
		t.Fatalf("read after the long ones: %v", err)
	}
	if string(packetData[:n]) != "after the long ones" {
		t.Fatalf("read %q", packetData[:n])
	}
	fixture.expectAlive(t, "after the long ones")
	if count := strings.Count(log.all(), "too long"); count != 1 {
		t.Fatalf("%d lines of the long ones, expected one a minute", count)
	}
}

// A logger that keeps every line, whatever its level.
type dnsCarrierLogLines struct {
	stateLock sync.Mutex
	lines     []string
}

func (self *dnsCarrierLogLines) add(format string, args ...any) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.lines = append(self.lines, fmt.Sprintf(format, args...))
}

func (self *dnsCarrierLogLines) all() string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return strings.Join(self.lines, "\n")
}

func (self *dnsCarrierLogLines) Info(args ...any)                    { self.add("%v", args) }
func (self *dnsCarrierLogLines) Infof(format string, args ...any)    { self.add(format, args...) }
func (self *dnsCarrierLogLines) Warningf(format string, args ...any) { self.add(format, args...) }
func (self *dnsCarrierLogLines) Errorf(format string, args ...any)   { self.add(format, args...) }
func (self *dnsCarrierLogLines) V(level int32) Verbose               { return dnsCarrierLogVerbose{self} }

type dnsCarrierLogVerbose struct {
	log *dnsCarrierLogLines
}

func (self dnsCarrierLogVerbose) Enabled() bool                    { return true }
func (self dnsCarrierLogVerbose) Info(args ...any)                 { self.log.Info(args...) }
func (self dnsCarrierLogVerbose) Infof(format string, args ...any) { self.log.Infof(format, args...) }

// A socket's errors say which addresses they failed between, and on a server the
// far one is a client's. It must reach no line of the log, which sdk/log_upload.go
// uploads, no error the translation keeps and nothing it returns, in either family,
// on any way a socket can fail. The server's alt listener and an extender's dns
// carrier are both this translation.
func TestPacketTranslationErrorsNameNoAddress(t *testing.T) {
	for _, addrs := range []struct {
		name   string
		source *net.UDPAddr
		client *net.UDPAddr
	}{
		{
			name:   "ipv4",
			source: &net.UDPAddr{IP: net.ParseIP("192.0.2.1"), Port: 53866},
			client: &net.UDPAddr{IP: net.ParseIP("203.0.113.7"), Port: 44444},
		},
		{
			name:   "ipv6",
			source: &net.UDPAddr{IP: net.ParseIP("2001:db8::1"), Port: 53866},
			client: &net.UDPAddr{IP: net.ParseIP("2001:db8:1234::7"), Port: 44444},
		},
	} {
		opError := func(op string, err error) error {
			return &net.OpError{Op: op, Net: "udp", Source: addrs.source, Addr: addrs.client, Err: err}
		}
		sendErr := os.NewSyscallError("sendto", syscall.ENETUNREACH)
		// what the net package builds for a family the socket cannot send to
		addrErr := &net.AddrError{Err: "non-IPv4 address", Addr: addrs.client.IP.String()}
		cases := []struct {
			name       string
			mode       PacketTranslationMode
			writeErr   error
			readErr    error
			expectText string
			ends       bool
		}{
			{name: "a dropped datagram", mode: PacketTranslationModeDecode53, writeErr: opError("write", sendErr), expectText: "network is unreachable"},
			{name: "an address the socket cannot send to", mode: PacketTranslationModeDecode53, writeErr: opError("write", addrErr), expectText: "non-IPv4 address"},
			{name: "a closed socket on write", mode: PacketTranslationModeDecode53, writeErr: opError("write", net.ErrClosed), expectText: "closed", ends: true},
			{name: "a read that retries", mode: PacketTranslationModeDecode53, readErr: opError("read", os.NewSyscallError("recvfrom", syscall.EMFILE)), expectText: "too many open files"},
			{name: "a read that ends it", mode: PacketTranslationModeDecode53, readErr: opError("read", os.NewSyscallError("recvfrom", syscall.EIO)), expectText: "recvfrom", ends: true},
			{name: "a client's write", mode: PacketTranslationModeDns, writeErr: opError("write", sendErr), expectText: "network is unreachable", ends: true},
			{name: "a client's read", mode: PacketTranslationModeDns, readErr: opError("read", os.NewSyscallError("recvfrom", syscall.EIO)), expectText: "recvfrom", ends: true},
		}
		for _, testCase := range cases {
			t.Run(addrs.name+"/"+testCase.name, func(t *testing.T) {
				log := &dnsCarrierLogLines{}
				fixture := newDnsCarrierFailingFixtureWithSettings(t, testCase.mode, func(conn *dnsCarrierFailingConn) {
					if testCase.readErr != nil {
						conn.failReads(testCase.readErr)
					}
					if testCase.writeErr != nil {
						conn.failWrites(testCase.writeErr)
					}
				}, func(settings *PacketTranslationSettings) {
					settings.Log = log
				})

				results := []string{}
				if testCase.writeErr != nil {
					_, err := fixture.translation.WriteTo([]byte("a datagram"), fixture.peer.addr)
					results = append(results, fmt.Sprint(err))
				}
				if testCase.ends {
					fixture.waitEnded(t)
				} else {
					deadline := time.Now().Add(dnsCarrierTestWait)
					for !strings.Contains(log.all(), testCase.expectText) {
						if deadline.Before(time.Now()) {
							t.Fatalf("the failure was not logged, lines = %q", log.all())
						}
						time.Sleep(time.Millisecond)
					}
				}
				results = append(results, fmt.Sprint(fixture.translation.EndError()), log.all())

				everything := strings.Join(results, "\n")
				if !strings.Contains(everything, testCase.expectText) {
					t.Fatalf("what was said = %q, expected the cause %q", everything, testCase.expectText)
				}
				for _, forbidden := range []string{
					addrs.client.IP.String(),
					addrs.source.IP.String(),
					fmt.Sprint(addrs.client.Port),
					fmt.Sprint(addrs.source.Port),
				} {
					if strings.Contains(everything, forbidden) {
						t.Fatalf("what was said names %q: %q", forbidden, everything)
					}
				}
			})
		}
	}
}

// The cause survives the scrubbing for whoever asks: a closed socket stays closed,
// and the errno and the *os.SyscallError quic-go reads for a send it can recover
// from are still there.
func TestPacketTranslationSocketErrorKeepsTheCause(t *testing.T) {
	addr := &net.UDPAddr{IP: net.ParseIP("203.0.113.7"), Port: 44444}
	if err := socketErrorWithoutAddress(nil); err != nil {
		t.Fatalf("nil = %v", err)
	}
	closed := socketErrorWithoutAddress(&net.OpError{Op: "write", Addr: addr, Err: net.ErrClosed})
	if !errors.Is(closed, net.ErrClosed) {
		t.Fatalf("closed = %v", closed)
	}
	sent := socketErrorWithoutAddress(&net.OpError{
		Op:   "write",
		Addr: addr,
		Err:  os.NewSyscallError("sendto", syscall.EMSGSIZE),
	})
	if !errors.Is(sent, syscall.EMSGSIZE) {
		t.Fatalf("sent = %v", sent)
	}
	var syscallErr *os.SyscallError
	if !errors.As(sent, &syscallErr) {
		t.Fatalf("sent = %T, expected the syscall error", sent)
	}
	// nothing to scrub is left alone
	plain := errors.New("a failure this does not know")
	if err := socketErrorWithoutAddress(plain); err != plain {
		t.Fatalf("plain = %v", err)
	}
	wrapped := socketErrorWithoutAddress(fmt.Errorf("through a seam: %w", &net.OpError{Op: "write", Addr: addr, Err: syscall.EPERM}))
	if !errors.Is(wrapped, syscall.EPERM) || strings.Contains(wrapped.Error(), "203.0.113.7") {
		t.Fatalf("wrapped = %v", wrapped)
	}
}
