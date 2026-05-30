package connect

import (
	"context"
	"fmt"
	"io"
	mathrand "math/rand"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/go-playground/assert/v2"

	"github.com/urnetwork/connect/protocol"
)

func TestWebRtc(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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
	assert.Equal(t, err, nil)
	defer connA.Close()

	connB, err := webRtcManagerB.NewP2pConnPassive(ctx, NewTransferPath(peerIdB, peerIdA, streamId))
	assert.Equal(t, err, nil)
	defer connB.Close()

	b := make([]byte, 1024*1024)
	mathrand.Read(b)

	received := make(chan []byte)

	send := func(conn net.Conn) {
		_, err := conn.Write(b)
		if err != nil {
			panic(err)
		}
	}
	receive := func(conn net.Conn) {
		b2 := make([]byte, len(b))
		_, err := io.ReadFull(conn, b2)
		if err != nil {
			panic(err)
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
			panic("Timeout.")
		case b2 := <-received:
			assert.Equal(t, b, b2)
		}
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
		assert.Equal(t, err, nil)
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
		assert.Equal(t, err, nil)
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
		assert.Equal(t, err, nil)
		assert.Equal(t, receiver.enqueue(received), true)
	}

	readSignals := func() []*protocol.ExchangeSignal {
		received := receiver.dequeue()
		assert.NotEqual(t, received, nil)
		defer received.Close()
		err := received.prepareFrame()
		assert.Equal(t, err, nil)
		exchangeSignals := &protocol.ExchangeSignals{}
		err = ProtoUnmarshal(received.frame.MessageBytes, exchangeSignals)
		assert.Equal(t, err, nil)
		return exchangeSignals.Signals
	}

	signals := readSignals()
	assert.Equal(t, len(signals), 1)
	assert.Equal(t, signals[0].SignalType, protocol.SignalType_IceCandidate)
	assert.Equal(t, string(signals[0].IceCandidate), "c1")

	signals = readSignals()
	assert.Equal(t, len(signals), 1)
	assert.Equal(t, signals[0].SignalType, protocol.SignalType_SdpOffer)
	assert.Equal(t, string(signals[0].Sdp), "sdp")

	signals = readSignals()
	assert.Equal(t, len(signals), 1)
	assert.Equal(t, signals[0].SignalType, protocol.SignalType_IceCandidate)
	assert.Equal(t, string(signals[0].IceCandidate), "c2")
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
		assert.Equal(t, err, nil)
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
		assert.Equal(t, err, nil)
		assert.Equal(t, receiver.enqueue(received), true)
	}

	received := receiver.dequeue()
	assert.NotEqual(t, received, nil)
	defer received.Close()
	err := received.prepareFrame()
	assert.Equal(t, err, nil)
	exchangeSignals := &protocol.ExchangeSignals{}
	err = ProtoUnmarshal(received.frame.MessageBytes, exchangeSignals)
	assert.Equal(t, err, nil)
	assert.Equal(t, len(exchangeSignals.Signals), 2)
	assert.Equal(t, string(exchangeSignals.Signals[0].IceCandidate), "c1")
	assert.Equal(t, string(exchangeSignals.Signals[1].IceCandidate), "c2")
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
		fmt.Printf("[signal]%s (%s)\n", path, signal.MessageType)
		signalReceiver.ReceiveSignal(path.SourceMask(), signal)
	} else {
		fmt.Printf("[signal]drop %s (%s)\n", path, signal.MessageType)

	}
}
