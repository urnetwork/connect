// A source's FIN closes its direction of a flow and nothing more (RFC 9293
// 3.6). These run the provider egress path through the real NAT and provider
// constructors, over in-memory upstreams that half-close as a TCP socket does,
// under synctest, so no result depends on timing or a network.
package connect

import (
	"io"
	"net"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// One end of an in-memory duplex made of two net.Pipe pairs, each used in one
// direction, so an end can close its writing direction alone, as CloseWrite
// does on a TCP socket: the other end reads EOF and can still write. Deadlines
// are accepted and never fire; these tests do not wait on a socket timeout.
type halfClosePipeConn struct {
	read        net.Conn
	write       net.Conn
	writeClosed chan struct{}
	closed      chan struct{}
	writeOnce   sync.Once
	closeOnce   sync.Once
}

// Returns the two ends of one duplex, the flow's end first.
func newHalfClosePipe() (net.Conn, net.Conn) {
	firstWrite, secondRead := net.Pipe()
	secondWrite, firstRead := net.Pipe()
	first := &halfClosePipeConn{read: firstRead, write: firstWrite, writeClosed: make(chan struct{}), closed: make(chan struct{})}
	second := &halfClosePipeConn{read: secondRead, write: secondWrite, writeClosed: make(chan struct{}), closed: make(chan struct{})}
	return first, second
}

func (self *halfClosePipeConn) Read(b []byte) (int, error)  { return self.read.Read(b) }
func (self *halfClosePipeConn) Write(b []byte) (int, error) { return self.write.Write(b) }

func (self *halfClosePipeConn) CloseWrite() error {
	self.writeOnce.Do(func() { close(self.writeClosed) })
	return self.write.Close()
}

func (self *halfClosePipeConn) Close() error {
	self.closeOnce.Do(func() { close(self.closed) })
	self.write.Close()
	return self.read.Close()
}

func (self *halfClosePipeConn) LocalAddr() net.Addr                { return self.read.LocalAddr() }
func (self *halfClosePipeConn) RemoteAddr() net.Addr               { return self.read.RemoteAddr() }
func (self *halfClosePipeConn) SetDeadline(_ time.Time) error      { return nil }
func (self *halfClosePipeConn) SetReadDeadline(_ time.Time) error  { return nil }
func (self *halfClosePipeConn) SetWriteDeadline(_ time.Time) error { return nil }

// The flow's first SYN. Not the retirement fixture's 1000, whose generation
// carries that fixture's own acknowledgement hooks.
const tcpHalfCloseSyn = uint32(5000)

type tcpHalfCloseFixture struct {
	base     *tcpProviderRetirementFixture
	flow     *TcpSequence
	upstream net.Conn
	// the flow's end of the upstream socket, when it can half-close
	socket *halfClosePipeConn
}

// Opens one flow through the provider egress path. A half-closable upstream is
// the in-memory duplex above; otherwise it is a net.Pipe, which has no
// CloseWrite. The acknowledgement worker coalesces for ackCompressTimeout.
func newTcpHalfCloseFixture(t *testing.T, halfClosable bool, ackCompressTimeout time.Duration) *tcpHalfCloseFixture {
	t.Helper()
	options := tcpProviderRetirementOptions{
		configureNat: func(settings *LocalUserNatSettings) {
			settings.TcpBufferSettings.AckCompressTimeout = ackCompressTimeout
		},
	}
	if halfClosable {
		options.socketPair = newHalfClosePipe
	}
	fixture := &tcpHalfCloseFixture{base: newTcpProviderRetirementFixtureWithOptions(t, options)}
	fixture.base.send(tcpFlagSyn, tcpHalfCloseSyn, 0, nil)
	fixture.flow = fixture.base.nextSequence(tcpHalfCloseSyn)
	fixture.upstream = fixture.base.upstreams[0]
	if halfClosable {
		fixture.socket = fixture.base.sequenceSockets[0].(*halfClosePipeConn)
	}
	controls := fixture.base.takeControls()
	if len(controls) != 1 || !controls[0].syn || !controls[0].ack || controls[0].ackNumber != tcpHalfCloseSyn+1 {
		t.Fatalf("handshake=%+v", controls)
	}
	return fixture
}

// Sends one segment acknowledging everything the flow has returned so far.
func (self *tcpHalfCloseFixture) send(flags byte, sequenceNumber uint32, payload []byte) {
	self.base.t.Helper()
	self.flow.mutex.Lock()
	ackNumber := self.flow.receiveSeq
	self.flow.mutex.Unlock()
	self.base.send(flags, sequenceNumber, ackNumber, payload)
}

// The upstream reads the request the source uploaded.
func (self *tcpHalfCloseFixture) request(payload string) {
	self.base.t.Helper()
	self.base.upload(self.flow, self.upstream, tcpHalfCloseSyn+1, payload)
	controls := self.base.takeControls()
	if len(controls) != 1 || !tcpHalfClosePureAck(controls[0], tcpHalfCloseSyn+1+uint32(len(payload))) {
		self.base.t.Fatalf("request acknowledgement=%+v", controls)
	}
}

// Reads the upstream until the flow half-closes it, which must happen now.
func (self *tcpHalfCloseFixture) requireUpstreamEof() {
	self.base.t.Helper()
	read := make(chan error, 1)
	go func() {
		n, err := self.upstream.Read(make([]byte, 1))
		if n != 0 {
			err = io.ErrUnexpectedEOF
		}
		read <- err
	}()
	synctest.Wait()
	select {
	case err := <-read:
		if err != io.EOF {
			self.base.t.Fatalf("upstream read after the source's FIN = %v, want EOF", err)
		}
	default:
		self.base.t.Fatal("the upstream has no EOF after the source's FIN")
	}
}

// The flow is live, and its end of the upstream socket is half-closed, not
// closed.
func (self *tcpHalfCloseFixture) requireHalfClosed(when string) {
	self.base.t.Helper()
	if self.flow.ctx.Err() != nil {
		self.base.t.Fatalf("%s: the flow ended", when)
	}
	select {
	case <-self.socket.closed:
		self.base.t.Fatalf("%s: the flow closed the upstream socket", when)
	default:
	}
}

// The flow has retired and closed its end of the upstream socket.
func (self *tcpHalfCloseFixture) requireRetired(when string) {
	self.base.t.Helper()
	synctest.Wait()
	select {
	case <-self.flow.retirementDone:
	default:
		self.base.t.Fatalf("%s: the flow has not retired", when)
	}
	if self.socket != nil {
		select {
		case <-self.socket.closed:
		default:
			self.base.t.Fatalf("%s: the flow retired with its upstream socket open", when)
		}
	}
}

func tcpHalfClosePureAck(packet tcpProviderRetirementPacket, ackNumber uint32) bool {
	return !packet.syn && packet.ack && !packet.fin && !packet.rst && packet.payloadByteCount == 0 &&
		packet.ackNumber == ackNumber
}

func tcpHalfCloseRequireNoReset(t *testing.T, controls []tcpProviderRetirementPacket) {
	t.Helper()
	for _, packet := range controls {
		if packet.rst {
			t.Fatalf("the flow sent a reset: %+v", controls)
		}
	}
}

// A source that half-closes after its request gets its FIN acknowledged, the
// upstream reads EOF while its socket stays open, and the response that
// follows reaches the source, then the upstream's FIN. The flow retires once
// the source acknowledges that FIN, with no reset.
func TestProviderTcpSourceHalfCloseDeliversTheResponse(t *testing.T) {
	assertMessagePoolOwnership(t)
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		f := newTcpHalfCloseFixture(t, true, 0)
		const request = "request"
		const response = "response"
		f.request(request)
		finSeq := tcpHalfCloseSyn + 1 + uint32(len(request))

		f.send(tcpFlagFin|tcpFlagAck, finSeq, nil)
		synctest.Wait()
		controls := f.base.takeControls()
		if len(controls) != 1 || !tcpHalfClosePureAck(controls[0], finSeq+1) {
			t.Fatalf("the source's FIN got %+v, want one acknowledgement of it", controls)
		}
		f.requireHalfClosed("after the source's FIN")
		select {
		case <-f.socket.writeClosed:
		default:
			t.Fatal("the flow did not half-close the upstream after the source's FIN")
		}
		f.requireUpstreamEof()

		// the upstream answers after the source's EOF, as a server that reads
		// its request to the end does
		if n, err := f.upstream.Write([]byte(response)); n != len(response) || err != nil {
			t.Fatalf("upstream response write = (%d, %v)", n, err)
		}
		synctest.Wait()
		controls = f.base.takeControls()
		if len(controls) != 1 || controls[0].fin || controls[0].rst || controls[0].payloadByteCount != len(response) ||
			controls[0].ackNumber != finSeq+1 {
			t.Fatalf("the response reached the source as %+v", controls)
		}
		f.requireHalfClosed("after the response")

		f.upstream.(*halfClosePipeConn).CloseWrite()
		synctest.Wait()
		controls = f.base.takeControls()
		if len(controls) != 1 || !controls[0].fin || !controls[0].ack || controls[0].rst || controls[0].payloadByteCount != 0 {
			t.Fatalf("the upstream's EOF reached the source as %+v, want its FIN", controls)
		}
		f.requireHalfClosed("before the source acknowledges the upstream's FIN")

		f.send(tcpFlagAck, finSeq+1, nil)
		f.requireRetired("after the source acknowledged the upstream's FIN")
		controls = f.base.takeControls()
		tcpHalfCloseRequireNoReset(t, controls)
		if len(controls) != 0 {
			t.Fatalf("the close sent more: %+v", controls)
		}
	})
}

// An upstream that closes first sends its response and FIN; the flow then
// waits for the source's FIN, which it acknowledges before it retires, so the
// source closes without a reset.
func TestProviderTcpUpstreamCloseWaitsForTheSourceFin(t *testing.T) {
	assertMessagePoolOwnership(t)
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		f := newTcpHalfCloseFixture(t, true, 0)
		const request = "request"
		const response = "response"
		f.request(request)
		finSeq := tcpHalfCloseSyn + 1 + uint32(len(request))

		if n, err := f.upstream.Write([]byte(response)); n != len(response) || err != nil {
			t.Fatalf("upstream response write = (%d, %v)", n, err)
		}
		f.upstream.(*halfClosePipeConn).CloseWrite()
		synctest.Wait()
		controls := f.base.takeControls()
		if len(controls) != 2 || controls[0].payloadByteCount != len(response) || controls[0].fin ||
			!controls[1].fin || controls[1].payloadByteCount != 0 {
			t.Fatalf("the response and the upstream's FIN reached the source as %+v", controls)
		}
		tcpHalfCloseRequireNoReset(t, controls)

		// the source has all of it; its own direction is still open
		f.send(tcpFlagAck, finSeq, nil)
		synctest.Wait()
		f.requireHalfClosed("after the source acknowledged the upstream's FIN")
		if controls := f.base.takeControls(); len(controls) != 0 {
			t.Fatalf("an acknowledgement drew %+v", controls)
		}

		f.send(tcpFlagFin|tcpFlagAck, finSeq, nil)
		f.requireRetired("after the source's FIN")
		controls = f.base.takeControls()
		if len(controls) != 1 || !tcpHalfClosePureAck(controls[0], finSeq+1) {
			t.Fatalf("the source's last FIN got %+v, want one acknowledgement of it", controls)
		}
		f.requireUpstreamEof()
	})
}

// Once the source's FIN is in, a later segment's text is ignored and answered
// with an acknowledgement of what was accepted, and a retransmitted FIN is
// acknowledged again. Neither reaches the upstream nor ends the flow.
func TestProviderTcpSegmentsAfterTheSourceFinAreIgnored(t *testing.T) {
	assertMessagePoolOwnership(t)
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		f := newTcpHalfCloseFixture(t, true, 0)
		finSeq := tcpHalfCloseSyn + 1

		f.send(tcpFlagFin|tcpFlagAck, finSeq, nil)
		synctest.Wait()
		if controls := f.base.takeControls(); len(controls) != 1 || !tcpHalfClosePureAck(controls[0], finSeq+1) {
			t.Fatalf("the source's FIN got %+v", controls)
		}
		f.requireUpstreamEof()

		f.send(tcpFlagAck|tcpFlagPsh, finSeq+1, []byte("past the FIN"))
		synctest.Wait()
		if controls := f.base.takeControls(); len(controls) != 1 || !tcpHalfClosePureAck(controls[0], finSeq+1) {
			t.Fatalf("text past the FIN got %+v, want an acknowledgement of the FIN", controls)
		}
		f.send(tcpFlagFin|tcpFlagAck, finSeq, nil)
		synctest.Wait()
		if controls := f.base.takeControls(); len(controls) != 1 || !tcpHalfClosePureAck(controls[0], finSeq+1) {
			t.Fatalf("a retransmitted FIN got %+v, want an acknowledgement of it", controls)
		}
		f.requireHalfClosed("after segments past the source's FIN")

		f.upstream.(*halfClosePipeConn).CloseWrite()
		synctest.Wait()
		if controls := f.base.takeControls(); len(controls) != 1 || !controls[0].fin || controls[0].payloadByteCount != 0 {
			t.Fatalf("the upstream's EOF reached the source as %+v", controls)
		}
		f.send(tcpFlagAck, finSeq+1, nil)
		f.requireRetired("after the close")
		tcpHalfCloseRequireNoReset(t, f.base.takeControls())
	})
}

// A reset from the source still ends a half-closed flow at once.
func TestProviderTcpResetEndsAHalfClosedFlow(t *testing.T) {
	assertMessagePoolOwnership(t)
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		f := newTcpHalfCloseFixture(t, true, 0)
		finSeq := tcpHalfCloseSyn + 1
		f.send(tcpFlagFin|tcpFlagAck, finSeq, nil)
		synctest.Wait()
		f.base.takeControls()
		f.requireHalfClosed("after the source's FIN")

		f.send(tcpFlagRst|tcpFlagAck, finSeq+1, nil)
		f.requireRetired("after the source's reset")
		tcpHalfCloseRequireNoReset(t, f.base.takeControls())
	})
}

// An upstream that cannot half-close is closed after the source's FIN, as
// before, but the FIN is acknowledged first. The acknowledgement worker is
// coalescing when the FIN arrives, so only the send loop's own acknowledgement
// can reach the source before the flow ends.
func TestProviderTcpSourceFinIsAcknowledgedBeforeAFullClose(t *testing.T) {
	assertMessagePoolOwnership(t)
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		f := newTcpHalfCloseFixture(t, false, time.Minute)
		const request = "request"
		f.request(request)
		finSeq := tcpHalfCloseSyn + 1 + uint32(len(request))

		f.send(tcpFlagFin|tcpFlagAck, finSeq, nil)
		f.requireRetired("after the source's FIN on an upstream that cannot half-close")
		controls := f.base.takeControls()
		if len(controls) != 1 || !tcpHalfClosePureAck(controls[0], finSeq+1) {
			t.Fatalf("the source's FIN got %+v, want one acknowledgement of it", controls)
		}
		f.requireUpstreamEof()
	})
}
