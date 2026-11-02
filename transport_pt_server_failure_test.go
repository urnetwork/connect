package connect

import (
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// What a decode53 socket does with a failure that is not its end (EXTENDER.md G2).
// An extender's dns carrier serves every client on one socket, so a datagram it
// cannot write, or a read that fails for a moment, costs that datagram. On
// 2026-11-01 one such write would end the translation, and the extender's whole
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
	translation, err := NewPacketTranslation(t.Context(), mode, conn, dnsCarrierTestSettings(tld))
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
