//go:build windows

package connect

import (
	"errors"
	"io"
	"net"
	"os"
	"testing"
	"time"
)

// The reorder technique takes a second handle to the socket so it can step the
// TTL between fragments. TCPConn.File cannot be that handle on Windows: it
// detaches the connection's own socket from the runtime's I/O completion port
// for good, so every later read and write on the connection parks an OS thread
// in an infinite wait, read and write deadlines stop working, and Close waits
// on a parked read until the peer sends or hangs up. A dial storm through the
// reorder dialer then grows the process by a thread per pending read (the
// windows service passed 1,500 threads in three minutes).
func TestDuplicateSocketHandleKeepsTheConnectionOnThePoller(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		peer, err := listener.Accept()
		if err != nil {
			close(accepted)
			return
		}
		accepted <- peer
	}()
	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	tcpConn := conn.(*net.TCPConn)
	defer tcpConn.Close()
	peer, ok := <-accepted
	if !ok {
		t.Fatal("accept failed")
	}
	// the failure paths below close the peer to release a parked read, since
	// closing the connection itself would wait on that read
	defer peer.Close()

	type readResult struct {
		b   []byte
		err error
	}
	readAsync := func() chan readResult {
		result := make(chan readResult, 1)
		go func() {
			b := make([]byte, 4)
			_, err := io.ReadFull(tcpConn, b)
			result <- readResult{b, err}
		}()
		return result
	}

	fd, closeFd, err := duplicateSocketHandle(tcpConn)
	if err != nil {
		t.Fatal(err)
	}
	// the duplicate reaches the connection's own socket
	if err := SetSocketTtl(fd, 63); err != nil {
		closeFd()
		t.Fatal(err)
	}
	// a read pending on the connection when the duplicate closes keeps going
	pending := readAsync()
	time.Sleep(100 * time.Millisecond)
	closeFd()
	if _, err := peer.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-pending:
		if r.err != nil || string(r.b) != "ping" {
			t.Fatalf("read across the duplicate's close = %q err = %v", r.b, r.err)
		}
	case <-time.After(5 * time.Second):
		peer.Close()
		<-pending
		t.Fatal("closing the duplicate stalled a read on the connection")
	}

	rawConn, err := tcpConn.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	connTtl := 0
	if err := rawConn.Control(func(handle uintptr) {
		connTtl = GetSocketTtl(SocketHandle(handle))
	}); err != nil {
		t.Fatal(err)
	}
	if connTtl != 63 {
		t.Fatalf("the connection's ttl is %d after setting 63 through the duplicate", connTtl)
	}

	// a deadline still interrupts a read with nothing to read
	if err := tcpConn.SetReadDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-readAsync():
		if !errors.Is(r.err, os.ErrDeadlineExceeded) {
			t.Fatalf("read err = %v, want the deadline", r.err)
		}
	case <-time.After(5 * time.Second):
		peer.Close()
		t.Fatal("the read deadline did not interrupt the read: the connection left the poller")
	}

	// and writes still go out
	if err := tcpConn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := tcpConn.Write([]byte("pong")); err != nil {
		t.Fatal(err)
	}
	peer.SetReadDeadline(time.Now().Add(5 * time.Second))
	b := make([]byte, 4)
	if _, err := io.ReadFull(peer, b); err != nil || string(b) != "pong" {
		t.Fatalf("peer read %q err = %v", b, err)
	}
}
