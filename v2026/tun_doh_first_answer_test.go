// A real UDP packet must not depend on completion of an unrelated DNS family.
package connect

import (
	"bytes"
	"fmt"
	"net"
	"testing"
)

// The held DNS answer can only finish after the first application datagram.
// Dial therefore must return before that release; the deadline is a watchdog,
// not a short negative timing assertion. Both stacks and DNS remain local.
func checkTunDohFirstAnswerUdp(t *testing.T, readyVersion int) {
	t.Helper()
	self := newDohProgressTunFixture(t, readyVersion)
	readyAddr := tunTestLocalAddress(t, self.right, readyVersion)
	listener, err := self.right.ListenUDP(&net.UDPAddr{IP: net.IP(readyAddr.AsSlice())})
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("first-answer-datagram")
	serverDone := make(chan error, 1)
	go func() {
		buffer := make([]byte, len(payload)+1)
		n, remote, err := listener.ReadFrom(buffer)
		if err != nil {
			serverDone <- err
			return
		}
		if !bytes.Equal(buffer[:n], payload) {
			serverDone <- fmt.Errorf("application datagram differs")
			return
		}
		self.applicationAcceptOnce.Do(func() { close(self.applicationAccepted) })
		_, err = listener.WriteTo(buffer[:n], remote)
		serverDone <- err
	}()
	joined := false
	t.Cleanup(func() {
		_ = listener.Close()
		if !joined {
			<-serverDone
		}
	})
	self.hold.Store(true)
	address := net.JoinHostPort(self.host, itoa(listener.LocalAddr().(*net.UDPAddr).Port))
	conn, err := self.left.DialContext(self.ctx, "udp", address)
	if conn != nil {
		defer conn.Close()
	}
	if err != nil || self.ctx.Err() != nil {
		t.Fatalf("usable TUN UDP family waited for held DNS before returning: ready_queries=%d held_queries=%d dial_error=%v context_error=%v", self.readyQueries.Load(), self.heldQueries.Load(), err, self.ctx.Err())
	}
	select {
	case <-self.applicationAccepted:
		t.Fatal("held DNS release preceded the first UDP dial")
	default:
	}
	deadline, _ := self.ctx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write(payload); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, len(payload)+1)
	n, err := conn.Read(buffer)
	if err != nil || !bytes.Equal(buffer[:n], payload) {
		t.Fatalf("first UDP packet did not round trip: err=%v bytes=%d", err, n)
	}
	serverErr := <-serverDone
	joined = true
	if serverErr != nil {
		t.Fatal(serverErr)
	}
	if self.readyQueries.Load() != 1 || self.heldQueries.Load() != 1 {
		t.Fatalf("custom DNS query counts ready=%d held=%d", self.readyQueries.Load(), self.heldQueries.Load())
	}
}

// The public generic TUN UDP dial can send through ready A while AAAA is held.
func TestTunDohFirstAnswerUdpReadyIpv4(t *testing.T) {
	checkTunDohFirstAnswerUdp(t, 4)
}

// The symmetric case must not introduce a hidden IPv4 preference for UDP.
func TestTunDohFirstAnswerUdpReadyIpv6(t *testing.T) {
	checkTunDohFirstAnswerUdp(t, 6)
}
