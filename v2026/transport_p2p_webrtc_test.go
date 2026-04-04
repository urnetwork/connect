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

	"github.com/urnetwork/connect/v2026/protocol"
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

func (self *signalPipe) SendSignal(path TransferPath, signal *protocol.Frame) {
	signalReceiver := self.SignalReceiver()
	if signalReceiver != nil {
		fmt.Printf("[signal]%s (%s)\n", path, signal.MessageType)
		signalReceiver.ReceiveSignal(path.SourceMask(), signal)
	} else {
		fmt.Printf("[signal]drop %s (%s)\n", path, signal.MessageType)

	}
}
