package connect

import (
	"bytes"
	"context"
	"fmt"
	"io"
	mathrand "math/rand"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

func TestWebRtc(t *testing.T) {
	// the 1MiB transfer over local ice varies widely in time under -race
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	settingsA := DefaultWebRtcSettings()
	settingsB := DefaultWebRtcSettings()

	// each manager sends signals to each other
	signalPipeA := newSignalPipe(nil)
	signalPipeB := newSignalPipe(nil)

	webRtcManagerA := NewWebRtcManager(ctx, signalPipeA, settingsA)
	webRtcManagerB := NewWebRtcManager(ctx, signalPipeB, settingsB)

	signalPipeA.signalReceiver = webRtcManagerB
	signalPipeB.signalReceiver = webRtcManagerA

	peerIdA := NewId()
	peerIdB := NewId()
	streamId := NewId()

	connA, err := webRtcManagerA.NewP2pConnActive(ctx, NewTransferPath(peerIdA, peerIdB, streamId))
	AssertEqual(t, err, nil)
	defer connA.Close()

	connB, err := webRtcManagerB.NewP2pConnPassive(ctx, NewTransferPath(peerIdB, peerIdA, streamId))
	AssertEqual(t, err, nil)
	defer connB.Close()

	b := make([]byte, 1024*1024)
	mathrand.Read(b)

	received := make(chan []byte)

	// the helpers must not panic on conn errors. reads and writes that race
	// the test teardown see closed-conn errors, and a panic in a test
	// goroutine kills the whole test binary. missing data is detected by
	// the receive loop timeout below.
	send := func(conn net.Conn) {
		conn.Write(b)
	}
	receive := func(conn net.Conn) {
		b2 := make([]byte, len(b))
		if _, err := io.ReadFull(conn, b2); err != nil {
			return
		}
		select {
		case <-ctx.Done():
		case received <- b2:
		}
	}

	go send(connA)
	go receive(connA)
	go send(connB)
	go receive(connB)

	for range 2 {
		select {
		case <-ctx.Done():
			t.Fatal("timeout")
		case b2 := <-received:
			AssertEqual(t, b, b2)
		}
	}

}

// TestWebRtcMessageRoundTrip verifies the P2P transport's native message
// framing: the detached data channel is message-oriented (one Write becomes one
// SCTP message the peer reads back whole), so consecutive TransferFrames of
// varied sizes must each arrive intact and in order with no length prefix. The
// receive side mirrors P2pReceiveTransport: read each whole message into one
// reused buffer, then copy out the exact bytes.
func TestWebRtcMessageRoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	settingsA := DefaultWebRtcSettings()
	settingsB := DefaultWebRtcSettings()

	signalPipeA := newSignalPipe(nil)
	signalPipeB := newSignalPipe(nil)

	webRtcManagerA := NewWebRtcManager(ctx, signalPipeA, settingsA)
	webRtcManagerB := NewWebRtcManager(ctx, signalPipeB, settingsB)

	signalPipeA.signalReceiver = webRtcManagerB
	signalPipeB.signalReceiver = webRtcManagerA

	peerIdA := NewId()
	peerIdB := NewId()
	streamId := NewId()

	connA, err := webRtcManagerA.NewP2pConnActive(ctx, NewTransferPath(peerIdA, peerIdB, streamId))
	AssertEqual(t, err, nil)
	defer connA.Close()

	connB, err := webRtcManagerB.NewP2pConnPassive(ctx, NewTransferPath(peerIdB, peerIdA, streamId))
	AssertEqual(t, err, nil)
	defer connB.Close()

	sizes := []int{1, 100, 255, 256, 257, 1000, int(kib(4))}
	messages := make([][]byte, len(sizes))
	for i, size := range sizes {
		m := make([]byte, size)
		for j := range m {
			m[j] = byte((i*31 + j) % 256)
		}
		messages[i] = m
	}

	readErr := make(chan error, 1)
	go func() {
		// mirror the receive transport: one reused read buffer, copy out the
		// exact bytes of each whole message.
		readBuf := make([]byte, int(kib(4)))
		for i := range messages {
			n, err := connB.Read(readBuf)
			if err != nil {
				readErr <- fmt.Errorf("read %d: %w", i, err)
				return
			}
			got := make([]byte, n)
			copy(got, readBuf[:n])
			if !bytes.Equal(got, messages[i]) {
				readErr <- fmt.Errorf("frame %d mismatch (got %d bytes, want %d)", i, n, len(messages[i]))
				return
			}
		}
		readErr <- nil
	}()

	for i := range messages {
		if _, err := connA.Write(messages[i]); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}

	select {
	case <-ctx.Done():
		t.Fatal("timeout waiting for frames")
	case err := <-readErr:
		AssertEqual(t, err, nil)
	}
}

func TestClientSignalReceiverCoalescesAdjacentCandidatesOnly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	source := SourceId(NewId())
	streamId := NewId()
	receiver := &clientSignalReceiver{
		ctx:          ctx,
		cancel:       cancel,
		queueLimit:   8,
		queueMonitor: NewMonitor(),
		spaceMonitor: NewMonitor(),
	}

	candidateFrame := func(candidate string) *protocol.Frame {
		messageBytes, err := ProtoMarshal(&protocol.ExchangeSignals{
			StreamId: streamId.Bytes(),
			Signals: []*protocol.ExchangeSignal{
				{
					SignalType:   protocol.SignalType_IceCandidate,
					IceCandidate: []byte(candidate),
				},
			},
		})
		AssertEqual(t, err, nil)
		return &protocol.Frame{
			MessageType:  protocol.MessageType_TransferExchangeSignals,
			MessageBytes: messageBytes,
		}
	}

	sdpFrame := func(sdp string) *protocol.Frame {
		messageBytes, err := ProtoMarshal(&protocol.ExchangeSignals{
			StreamId: streamId.Bytes(),
			Signals: []*protocol.ExchangeSignal{
				{
					SignalType: protocol.SignalType_SdpOffer,
					Sdp:        []byte(sdp),
				},
			},
		})
		AssertEqual(t, err, nil)
		return &protocol.Frame{
			MessageType:  protocol.MessageType_TransferExchangeSignals,
			MessageBytes: messageBytes,
		}
	}

	frames := []*protocol.Frame{
		candidateFrame("c1"),
		sdpFrame("sdp"),
		candidateFrame("c2"),
	}
	defer func() {
		for _, frame := range frames {
			MessagePoolReturn(frame.MessageBytes)
		}
	}()

	for _, frame := range frames {
		received, err := newReceivedSignalFrame(source, frame)
		AssertEqual(t, err, nil)
		AssertEqual(t, receiver.enqueue(received), true)
	}

	readSignals := func() []*protocol.ExchangeSignal {
		received := receiver.dequeue()
		AssertNotEqual(t, received, nil)
		defer received.Close()
		err := received.prepareFrame()
		AssertEqual(t, err, nil)
		exchangeSignals := &protocol.ExchangeSignals{}
		err = ProtoUnmarshal(received.frame.MessageBytes, exchangeSignals)
		AssertEqual(t, err, nil)
		return exchangeSignals.Signals
	}

	signals := readSignals()
	AssertEqual(t, len(signals), 1)
	AssertEqual(t, signals[0].SignalType, protocol.SignalType_IceCandidate)
	AssertEqual(t, string(signals[0].IceCandidate), "c1")

	signals = readSignals()
	AssertEqual(t, len(signals), 1)
	AssertEqual(t, signals[0].SignalType, protocol.SignalType_SdpOffer)
	AssertEqual(t, string(signals[0].Sdp), "sdp")

	signals = readSignals()
	AssertEqual(t, len(signals), 1)
	AssertEqual(t, signals[0].SignalType, protocol.SignalType_IceCandidate)
	AssertEqual(t, string(signals[0].IceCandidate), "c2")
}

func TestClientSignalReceiverCoalescesAdjacentCandidates(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	source := SourceId(NewId())
	streamId := NewId()
	receiver := &clientSignalReceiver{
		ctx:          ctx,
		cancel:       cancel,
		queueLimit:   8,
		queueMonitor: NewMonitor(),
		spaceMonitor: NewMonitor(),
	}

	candidateFrame := func(candidate string) *protocol.Frame {
		messageBytes, err := ProtoMarshal(&protocol.ExchangeSignals{
			StreamId: streamId.Bytes(),
			Signals: []*protocol.ExchangeSignal{
				{
					SignalType:   protocol.SignalType_IceCandidate,
					IceCandidate: []byte(candidate),
				},
			},
		})
		AssertEqual(t, err, nil)
		return &protocol.Frame{
			MessageType:  protocol.MessageType_TransferExchangeSignals,
			MessageBytes: messageBytes,
		}
	}

	frames := []*protocol.Frame{
		candidateFrame("c1"),
		candidateFrame("c2"),
	}
	defer func() {
		for _, frame := range frames {
			MessagePoolReturn(frame.MessageBytes)
		}
	}()

	for _, frame := range frames {
		received, err := newReceivedSignalFrame(source, frame)
		AssertEqual(t, err, nil)
		AssertEqual(t, receiver.enqueue(received), true)
	}

	received := receiver.dequeue()
	AssertNotEqual(t, received, nil)
	defer received.Close()
	err := received.prepareFrame()
	AssertEqual(t, err, nil)
	exchangeSignals := &protocol.ExchangeSignals{}
	err = ProtoUnmarshal(received.frame.MessageBytes, exchangeSignals)
	AssertEqual(t, err, nil)
	AssertEqual(t, len(exchangeSignals.Signals), 2)
	AssertEqual(t, string(exchangeSignals.Signals[0].IceCandidate), "c1")
	AssertEqual(t, string(exchangeSignals.Signals[1].IceCandidate), "c2")
}

type signalPipe struct {
	stateLock      sync.Mutex
	signalReceiver SignalReceiver
}

func newSignalPipe(signalReceiver SignalReceiver) *signalPipe {
	return &signalPipe{
		signalReceiver: signalReceiver,
	}
}

func (self *signalPipe) SetSignalReceiver(signalReceiver SignalReceiver) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.signalReceiver = signalReceiver
}

func (self *signalPipe) SignalReceiver() SignalReceiver {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.signalReceiver
}

func (self *signalPipe) SendSignal(path TransferPath, signal *protocol.Frame, opts ...any) {
	signalReceiver := self.SignalReceiver()
	if signalReceiver != nil {
		fmt.Printf("[signal][%s]%s\n", signal.MessageType, path)
		signalReceiver.ReceiveSignal(path.SourceMask(), signal)
	} else {
		fmt.Printf("[signal][%s]drop %s\n", signal.MessageType, path)

	}
}
