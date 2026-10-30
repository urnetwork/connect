// A provider TCP flow is reaped by its own timeouts, not by kernel
// keepalive: a silent upstream at ReadTimeout after the last read began, a
// silent source at IdleTimeout after its last packet. Every event runs in
// virtual time over an in-memory upstream, with the default timeouts.
package connect

import (
	"context"
	"io"
	"math"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Counts the deadlines a flow installs on its in-memory socket.
type deadlineCountingConn struct {
	net.Conn
	readDeadlines  atomic.Int64
	writeDeadlines atomic.Int64
}

func (self *deadlineCountingConn) SetReadDeadline(t time.Time) error {
	self.readDeadlines.Add(1)
	return self.Conn.SetReadDeadline(t)
}

func (self *deadlineCountingConn) SetWriteDeadline(t time.Time) error {
	self.writeDeadlines.Add(1)
	return self.Conn.SetWriteDeadline(t)
}

// Runs one provider TCP flow over net.Pipe inside the caller's synctest
// bubble and records what reaches the source.
type tcpReapTestFlow struct {
	t        *testing.T
	settings *TcpBufferSettings
	sequence *TcpSequence
	// the flow's end of the pipe, and the upstream's
	local    *deadlineCountingConn
	upstream net.Conn
	source   TransferPath
	// the source's next sequence number, and the provider's
	sourceSeq   uint32
	providerSeq uint32
	runDone     chan struct{}

	stateLock sync.Mutex
	resetTime time.Time
	synAcked  bool
	// pure acknowledgements toward the source, in order
	acks []tcpReapTestAck
}

type tcpReapTestAck struct {
	at        time.Time
	ackNumber uint32
}

func newTcpReapTestFlow(t *testing.T, settings *TcpBufferSettings) *tcpReapTestFlow {
	t.Helper()
	return newTcpReapTestFlowWithSetup(t, settings, nil)
}

// Applies setup to the flow before it runs.
func newTcpReapTestFlowWithSetup(t *testing.T, settings *TcpBufferSettings, setup func(*TcpSequence)) *tcpReapTestFlow {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	pipe, upstream := net.Pipe()
	local := &deadlineCountingConn{Conn: pipe}
	settings.Log = NewNoopLogger()
	settings.DialContextSettings = &DialContextSettings{
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			return local, nil
		},
	}
	const initialSeq = uint32(1000)
	f := &tcpReapTestFlow{
		t:         t,
		settings:  settings,
		local:     local,
		upstream:  upstream,
		source:    SourceId(NewId()),
		sourceSeq: initialSeq + 1,
		runDone:   make(chan struct{}),
	}
	f.sequence = NewTcpSequence(ctx, func(_ TransferPath, _ protocol.ProvideMode, _ *IpPath, packet []byte) {
		_, sourceIp, destinationIp, transport, ok := parseIpv4(packet)
		var tcp parsedTcp
		if !ok || !parseTcpPacket(sourceIp, destinationIp, transport, &tcp) {
			t.Error("invalid packet toward the source")
			return
		}
		f.stateLock.Lock()
		defer f.stateLock.Unlock()
		if tcp.syn {
			f.synAcked = true
			f.providerSeq = tcp.seq + 1
		}
		if tcp.rst && f.resetTime.IsZero() {
			f.resetTime = time.Now()
		}
		if tcp.ack && !tcp.syn && !tcp.rst && !tcp.fin && len(tcp.payload) == 0 {
			f.acks = append(f.acks, tcpReapTestAck{at: time.Now(), ackNumber: tcp.ackNumber})
		}
	}, f.source, protocol.ProvideMode_Network, 4,
		net.IPv4(192, 0, 2, 2).To4(), 41000, net.IPv4(198, 51, 100, 2).To4(), 443,
		initialSeq, settings)
	if f.sequence == nil {
		t.Fatal("could not admit a TCP flow")
	}
	if setup != nil {
		setup(f.sequence)
	}
	go func() {
		defer close(f.runDone)
		f.sequence.Run()
	}()
	t.Cleanup(func() {
		cancel()
		upstream.Close()
		<-f.runDone
	})

	syn := f.newSendItem(nil)
	syn.tcp.seq = initialSeq
	syn.tcp.syn = true
	syn.tcp.ack = false
	f.send(syn)
	synctest.Wait()
	f.stateLock.Lock()
	synAcked := f.synAcked
	f.stateLock.Unlock()
	if !synAcked {
		t.Fatal("the flow did not answer the SYN")
	}
	// complete the handshake
	f.send(f.newSendItem(nil))
	synctest.Wait()
	return f
}

// One pool-owned IPv4 segment from the source, acknowledging everything the
// provider has sent so far.
func (self *tcpReapTestFlow) newSendItem(payload []byte) *TcpSendItem {
	headerByteCount := Ipv4HeaderSizeWithoutExtensions + TcpHeaderSizeWithoutExtensions
	packet := MessagePoolGet(headerByteCount + len(payload))
	clear(packet)
	packet[0] = 0x45
	copy(packet[headerByteCount:], payload)
	self.stateLock.Lock()
	ackNumber := self.providerSeq
	self.stateLock.Unlock()
	return &TcpSendItem{
		source:      self.source,
		provideMode: protocol.ProvideMode_Network,
		tcp: parsedTcp{
			seq:        self.sourceSeq,
			ack:        true,
			ackNumber:  ackNumber,
			windowSize: math.MaxUint16,
			payload:    packet[headerByteCount:],
		},
		ipPacket: packet,
	}
}

// Moves the segment's ownership to the flow.
func (self *tcpReapTestFlow) send(sendItem *TcpSendItem) {
	self.t.Helper()
	payloadByteCount := uint32(len(sendItem.tcp.payload))
	if accepted, err := self.sequence.send(sendItem, -1); !accepted {
		MessagePoolReturn(sendItem.ipPacket)
		self.t.Fatalf("the flow refused a source segment: %v", err)
	}
	self.sourceSeq += payloadByteCount
}

// Takes the pure acknowledgements the source has received.
func (self *tcpReapTestFlow) takeAcks() []tcpReapTestAck {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	acks := self.acks
	self.acks = nil
	return acks
}

func (self *tcpReapTestFlow) reset() time.Time {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.resetTime
}

func (self *tcpReapTestFlow) done() bool {
	select {
	case <-self.runDone:
		return true
	default:
		return false
	}
}

// An upstream that accepts bytes and never answers is reaped ReadTimeout
// after its reader began waiting, while the source keeps sending.
func TestTcpFlowSilentUpstreamIsReapedAtReadTimeout(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		settings := DefaultTcpBufferSettingsWithBufferSize(16)
		readTimeout := settings.ReadTimeout
		if settings.IdleTimeout < readTimeout || readTimeout <= 0 {
			t.Fatalf("defaults read=%s idle=%s cannot isolate the read timeout", readTimeout, settings.IdleTimeout)
		}
		connectedAt := time.Now()
		f := newTcpReapTestFlow(t, settings)
		go io.Copy(io.Discard, f.upstream)

		// the source stays active, so only the silent upstream can end the flow
		step := readTimeout / 3
		for range 2 {
			time.Sleep(step)
			f.send(f.newSendItem([]byte("ping")))
			synctest.Wait()
		}
		time.Sleep(connectedAt.Add(readTimeout).Sub(time.Now()) - time.Nanosecond)
		synctest.Wait()
		if !f.reset().IsZero() || f.done() {
			t.Fatalf("the flow ended before ReadTimeout of upstream silence: reset=%s", f.reset())
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		if got := f.reset(); !got.Equal(connectedAt.Add(readTimeout)) {
			t.Fatalf("the silent upstream was reset at %s after connect, want %s", got.Sub(connectedAt), readTimeout)
		}
	})
}

// A source that stops sending is reaped IdleTimeout after its last segment,
// while the upstream keeps sending.
func TestTcpFlowSilentSourceIsReapedAtIdleTimeout(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		settings := DefaultTcpBufferSettingsWithBufferSize(16)
		idleTimeout := settings.IdleTimeout
		if idleTimeout <= 0 {
			t.Fatal("the default flow has no idle timeout")
		}
		f := newTcpReapTestFlow(t, settings)
		lastSourceAt := time.Now()
		stop := make(chan struct{})
		upstreamDone := make(chan struct{})
		defer func() {
			close(stop)
			f.upstream.Close()
			<-upstreamDone
		}()
		go func() {
			defer close(upstreamDone)
			// the upstream stays active, so only the silent source can end the flow
			for {
				if _, err := f.upstream.Write([]byte{1}); err != nil {
					return
				}
				select {
				case <-stop:
					return
				case <-time.After(idleTimeout / 4):
				}
			}
		}()

		time.Sleep(idleTimeout - time.Nanosecond)
		synctest.Wait()
		if f.done() {
			t.Fatal("the flow ended before IdleTimeout of source silence")
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		if !f.done() {
			t.Fatalf("the silent source was not reaped %s after its last segment", time.Since(lastSourceAt))
		}
	})
}
