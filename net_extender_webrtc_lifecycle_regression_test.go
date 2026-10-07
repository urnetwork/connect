//go:build !js

package connect

// Explicit barriers force concurrent calls at the stream and signaling
// lifecycle boundaries; synctest proves when all other workers are blocked.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/urnetwork/connect/protocol"
)

// Each read owns a distinct message, but the first holds the supplied buffer
// while a second caller attempts to read through the same adapter.
type testWebRtcConcurrentReadChannel struct {
	testMessageChannel
	calls        atomic.Int32
	firstCopied  chan struct{}
	releaseFirst chan struct{}
}

// Pauses after filling the caller's buffer to expose shared scratch storage.
func (self *testWebRtcConcurrentReadChannel) Read(b []byte) (int, error) {
	switch self.calls.Add(1) {
	case 1:
		n := copy(b, "AAAA")
		close(self.firstCopied)
		<-self.releaseFirst
		return n, nil
	case 2:
		return copy(b, "BBBB"), nil
	default:
		return 0, io.EOF
	}
}

// Concurrent net.Conn reads must consume each byte exactly once. Holding the
// first read before launching the second also fixes which message comes first.
func TestWebRtcExtenderConcurrentReadsPreserveBytes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		channel := &testWebRtcConcurrentReadChannel{firstCopied: make(chan struct{}), releaseFirst: make(chan struct{})}
		conn := newWebRtcDataChannelConn(channel, nil, nil, 16)
		read := func(done chan<- string) {
			buffer := make([]byte, 4)
			n, err := conn.Read(buffer)
			if err != nil {
				done <- "error: " + err.Error()
				return
			}
			done <- string(buffer[:n])
		}
		firstDone, secondDone := make(chan string, 1), make(chan string, 1)
		go read(firstDone)
		<-channel.firstCopied
		// A mutex wait is not durably blocked in synctest. Inspect ownership
		// at the first channel barrier, then let both calls finish normally.
		serialized := !conn.readLock.TryLock()
		if serialized {
			go read(secondDone)
		} else {
			conn.readLock.Unlock()
			// Without serialization the second caller can finish while the
			// first still owns its channel buffer, forcing the old corruption.
			read(secondDone)
		}
		close(channel.releaseFirst)
		first, second := <-firstDone, <-secondDone
		if !serialized || first != "AAAA" || second != "BBBB" {
			t.Fatalf("read serialized = %t; caller bytes = %q, %q", serialized, first, second)
		}
	})
}

// Records each written message while pausing the first before it returns.
type testWebRtcConcurrentWriteChannel struct {
	testMessageChannel
	calls        atomic.Int32
	firstWritten chan struct{}
	releaseFirst chan struct{}
	messages     chan []byte
}

// The queue owns copies so callers may reuse their buffers after Write.
func (self *testWebRtcConcurrentWriteChannel) Write(b []byte) (int, error) {
	call := self.calls.Add(1)
	self.messages <- append([]byte(nil), b...)
	if call == 1 {
		close(self.firstWritten)
		<-self.releaseFirst
	}
	return len(b), nil
}

// Chunking one Write must preserve its byte order against another Write.
func TestWebRtcExtenderConcurrentWritesKeepChunksTogether(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		channel := &testWebRtcConcurrentWriteChannel{firstWritten: make(chan struct{}), releaseFirst: make(chan struct{}), messages: make(chan []byte, 3)}
		conn := newWebRtcDataChannelConn(channel, nil, nil, 16)
		first := bytes.Repeat([]byte("A"), webRtcExtenderMaxMessageByteCount+1)
		second := []byte("B")
		done := make(chan error, 2)
		go func() { _, err := conn.Write(first); done <- err }()
		<-channel.firstWritten
		serialized := !conn.writeLock.TryLock()
		if serialized {
			go func() { _, err := conn.Write(second); done <- err }()
		} else {
			conn.writeLock.Unlock()
			_, err := conn.Write(second)
			done <- err
		}
		close(channel.releaseFirst)
		for range 2 {
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		}
		var written []byte
		for range 3 {
			written = append(written, <-channel.messages...)
		}
		if !serialized || !bytes.Equal(written, append(first, second...)) {
			t.Fatalf("write serialized = %t, chunks interleaved = %t", serialized, !bytes.Equal(written, append(first, second...)))
		}
	})
}

// A reversible read timeout must not discard output already accepted by SCTP.
func TestWebRtcExtenderCloseDrainsAfterReadDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		channel := newTestBufferedMessageChannel(300)
		conn := newWebRtcDataChannelConn(channel, nil, nil, 0)
		conn.drainTimeout = time.Hour
		if err := conn.SetReadDeadline(time.Now().Add(-time.Second)); err != nil {
			t.Fatal(err)
		}
		closeDone := make(chan error, 1)
		go func() { closeDone <- conn.Close() }()
		synctest.Wait()
		closedBeforeAcknowledgement, closedWith := channel.closeCount != 0, channel.closedWhile
		channel.acknowledge()
		if err := <-closeDone; err != nil {
			t.Fatal(err)
		}
		if closedBeforeAcknowledgement || channel.closedWhile != 0 {
			t.Fatalf("read timeout discarded output: early close = %t, unacknowledged = %d", closedBeforeAcknowledgement, closedWith)
		}
	})
}

// Closing while an answer holds its waiter must serialize the nonblocking
// delivery with channel closure, including when close arrives in that gap.
func TestWebRtcExtenderAnswerRacingCloseDoesNotPanic(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		signaling := newWebRtcExtenderSignaling(context.Background(), NewNoopLogger(), nil, DefaultWebRtcSettings())
		peerId, streamId := NewId(), NewId()
		waiter, ok := signaling.register(peerConnKey{PeerId: peerId, StreamId: streamId})
		if !ok {
			t.Fatal("register")
		}
		lookedUp, release := make(chan struct{}), make(chan struct{})
		signaling.afterWaiterLookupForTest = func() { close(lookedUp); <-release }
		panicked := make(chan any, 1)
		go func() {
			defer func() { panicked <- recover() }()
			signaling.receiveAnswer(TransferPath{SourceId: peerId}, streamId, &protocol.ExchangeSignal{SignalType: protocol.SignalType_SdpAnswer})
		}()
		<-lookedUp
		closed := make(chan struct{})
		serialized := !signaling.stateLock.TryLock()
		if serialized {
			go func() { signaling.close(); close(closed) }()
		} else {
			signaling.stateLock.Unlock()
			signaling.close()
			close(closed)
		}
		close(release)
		value := <-panicked
		<-closed
		if value != nil {
			t.Fatalf("answer raced shutdown: %v", value)
		}
		if !serialized {
			t.Fatal("waiter lookup and delivery did not share lifecycle ownership")
		}
		for range waiter {
		}
	})
}

// Duplicate answers refuse immediately when their single delivery slot is
// occupied; the shared receive path cannot wait for the dialer to consume it.
func TestWebRtcExtenderAnswerDeliveryRemainsBounded(t *testing.T) {
	signaling := newWebRtcExtenderSignaling(context.Background(), NewNoopLogger(), nil, DefaultWebRtcSettings())
	peerId, streamId := NewId(), NewId()
	waiter, ok := signaling.register(peerConnKey{PeerId: peerId, StreamId: streamId})
	if !ok {
		t.Fatal("register")
	}
	first := &protocol.ExchangeSignal{SignalType: protocol.SignalType_SdpAnswer, Sdp: []byte("first")}
	signaling.receiveAnswer(TransferPath{SourceId: peerId}, streamId, first)
	signaling.receiveAnswer(TransferPath{SourceId: peerId}, streamId, &protocol.ExchangeSignal{SignalType: protocol.SignalType_SdpAnswer})
	if signal := <-waiter; signal != first {
		t.Fatal("duplicate replaced the admitted answer")
	}
	signaling.close()
	signaling.receiveAnswer(TransferPath{SourceId: peerId}, streamId, first)
	if signaling.stats().UnknownAnswerCount != 1 {
		t.Fatal("late answer was not refused")
	}
}

// A cooperative exchanger exposes the actual negotiation context and joins
// its cancellation before returning, without adding its own timeout.
type testWebRtcContextExchanger struct{ entered chan context.Context }

// Waits for the supplied context so only the carrier can bound this stage.
func (self *testWebRtcContextExchanger) ExchangeOffer(ctx context.Context, offer webrtc.SessionDescription) (webrtc.SessionDescription, error) {
	self.entered <- ctx
	<-ctx.Done()
	return webrtc.SessionDescription{}, ctx.Err()
}

// Carrier shutdown owns pending exchanges even when their caller stays live.
func TestWebRtcExtenderCarrierCloseCancelsOutstandingExchange(t *testing.T) {
	topology := newTestWebRtcNat(t)
	carrier := NewWebRtcExtenderCarrier(context.Background(), testWebRtcCarrierSettings(topology.dialerNet), nil)
	defer carrier.Close()
	callerCtx, cancelCaller := context.WithCancel(context.Background())
	defer cancelCaller()
	exchanger := &testWebRtcContextExchanger{entered: make(chan context.Context, 1)}
	dialDone := make(chan error, 1)
	go func() {
		_, _, err := carrier.DialWithExchanger(callerCtx, DefaultConnectSettings(), exchanger,
			&ExtenderConfig{Profile: ExtenderProfile{ConnectMode: ExtenderConnectModeWebRtc, Port: ExtenderTcpPort}}, &ExtenderDial{Service: ExtenderServiceFeed})
		dialDone <- err
	}()
	exchangeCtx := <-exchanger.entered
	carrier.Close()
	cancelErr := exchangeCtx.Err()
	cancelCaller()
	if err := <-dialDone; err == nil {
		t.Fatal("canceled dial succeeded")
	}
	if !errors.Is(cancelErr, context.Canceled) {
		t.Fatalf("Close left the exchange context live: %v", cancelErr)
	}
}

// A background caller must still give the whole negotiation a finite bound.
func TestWebRtcExtenderCarrierBoundsOutstandingExchange(t *testing.T) {
	topology := newTestWebRtcNat(t)
	settings := testWebRtcCarrierSettings(topology.dialerNet)
	settings.ExtenderCarrierOpenTimeout = time.Minute
	carrier := NewWebRtcExtenderCarrier(context.Background(), settings, nil)
	defer carrier.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	exchanger := &testWebRtcContextExchanger{entered: make(chan context.Context, 1)}
	dialDone := make(chan error, 1)
	started := time.Now()
	go func() {
		_, _, err := carrier.DialWithExchanger(ctx, DefaultConnectSettings(), exchanger,
			&ExtenderConfig{Profile: ExtenderProfile{ConnectMode: ExtenderConnectModeWebRtc, Port: ExtenderTcpPort}}, &ExtenderDial{Service: ExtenderServiceFeed})
		dialDone <- err
	}()
	exchangeCtx := <-exchanger.entered
	deadline, bounded := exchangeCtx.Deadline()
	cancel()
	<-dialDone
	if !bounded || deadline.Before(started) || time.Minute < time.Until(deadline) {
		t.Fatalf("exchange deadline = %v, bounded = %t", deadline, bounded)
	}
}

// Tracks actual channel readers, including the close drain, and optionally
// holds the final reader at its return to prove that Close joins its worker.
type testWebRtcObservedReadChannel struct {
	*testBufferedMessageChannel
	active        atomic.Int32
	maximum       atomic.Int32
	entered       chan struct{}
	returning     chan struct{}
	releaseReturn chan struct{}
}

// Records overlap without sharing the buffers under test with the observer.
func (self *testWebRtcObservedReadChannel) Read(b []byte) (int, error) {
	active := self.active.Add(1)
	defer self.active.Add(-1)
	for maximum := self.maximum.Load(); maximum < active && !self.maximum.CompareAndSwap(maximum, active); maximum = self.maximum.Load() {
	}
	if self.entered != nil {
		select {
		case self.entered <- struct{}{}:
		default:
		}
	}
	n, err := self.testBufferedMessageChannel.Read(b)
	if self.returning != nil {
		close(self.returning)
		<-self.releaseReturn
	}
	return n, err
}

// Close interrupts and replaces a caller's read; two underlying reads must
// never run together, even though the drain no longer returns caller bytes.
func TestWebRtcExtenderCloseSharesTheReader(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		channel := &testWebRtcObservedReadChannel{testBufferedMessageChannel: newTestBufferedMessageChannel(300), entered: make(chan struct{}, 2)}
		conn := newWebRtcDataChannelConn(channel, nil, nil, 16)
		readDone, closeDone := make(chan error, 1), make(chan error, 1)
		go func() { _, err := conn.Read(make([]byte, 4)); readDone <- err }()
		<-channel.entered
		go func() { closeDone <- conn.Close() }()
		synctest.Wait()
		maximum := channel.maximum.Load()
		channel.acknowledge()
		if err := <-closeDone; err != nil {
			t.Fatal(err)
		}
		if err := <-readDone; err == nil {
			t.Fatal("close did not interrupt read")
		}
		if maximum != 1 {
			t.Fatalf("close ran %d channel readers concurrently", maximum)
		}
	})
}

// Returning from Close must join the drain worker after it has been unblocked.
func TestWebRtcExtenderCloseJoinsItsDrainReader(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		channel := &testWebRtcObservedReadChannel{testBufferedMessageChannel: newTestBufferedMessageChannel(300), entered: make(chan struct{}, 1), returning: make(chan struct{}), releaseReturn: make(chan struct{})}
		conn := newWebRtcDataChannelConn(channel, nil, nil, 16)
		closeDone := make(chan error, 1)
		go func() { closeDone <- conn.Close() }()
		<-channel.entered
		channel.acknowledge()
		<-channel.returning
		synctest.Wait()
		returnedBeforeReader := false
		select {
		case <-closeDone:
			returnedBeforeReader = true
		default:
		}
		close(channel.releaseReturn)
		if !returnedBeforeReader {
			if err := <-closeDone; err != nil {
				t.Fatal(err)
			}
		}
		if returnedBeforeReader {
			t.Fatal("Close returned while its drain reader still owned the channel")
		}
	})
}

// Holds one public deadline update before it reaches the channel, allowing
// Close to race the exact check/update gap without sleeping.
type testWebRtcDelayedDeadlineChannel struct {
	testMessageChannel
	calls   atomic.Int32
	entered chan struct{}
	release chan struct{}
}

// The first call is a caller clearing a deadline; later calls are shutdown.
func (self *testWebRtcDelayedDeadlineChannel) SetReadDeadline(deadline time.Time) error {
	if self.calls.Add(1) == 1 {
		close(self.entered)
		<-self.release
	}
	return self.testMessageChannel.SetReadDeadline(deadline)
}

// Once Close interrupts blocked I/O, an already-entered deadline setter must
// not clear that interruption and strand shutdown waiting for an I/O lock.
func TestWebRtcExtenderCloseSerializesDeadlineUpdates(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		channel := &testWebRtcDelayedDeadlineChannel{entered: make(chan struct{}), release: make(chan struct{})}
		conn := newWebRtcDataChannelConn(channel, nil, nil, 16)
		deadlineDone, closeDone := make(chan error, 1), make(chan error, 1)
		go func() { deadlineDone <- conn.SetReadDeadline(time.Time{}) }()
		<-channel.entered
		serialized := !conn.deadlineLock.TryLock()
		if serialized {
			go func() { closeDone <- conn.Close() }()
		} else {
			conn.deadlineLock.Unlock()
			closeDone <- conn.Close()
		}
		close(channel.release)
		if err := <-deadlineDone; err != nil {
			t.Fatal(err)
		}
		if err := <-closeDone; err != nil {
			t.Fatal(err)
		}
		if channel.readDeadline.IsZero() || time.Now().Before(channel.readDeadline) {
			t.Fatal("the caller cleared Close's interruption deadline")
		}
	})
}

// A live but unresponsive peer can consume only the configured close bound;
// the reader still joins after the channel aborts at that bound.
func TestWebRtcExtenderCloseBoundsTheDrain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		channel := &testWebRtcObservedReadChannel{testBufferedMessageChannel: newTestBufferedMessageChannel(300)}
		conn := newWebRtcDataChannelConn(channel, nil, nil, 16)
		conn.drainTimeout = time.Minute
		started := time.Now()
		if err := conn.Close(); err != nil {
			t.Fatal(err)
		}
		if elapsed := time.Since(started); elapsed != time.Minute {
			t.Fatalf("drain elapsed = %s, want one minute", elapsed)
		}
		if channel.closedWhile != 300 || channel.active.Load() != 0 {
			t.Fatalf("bounded close: buffered = %d, active readers = %d", channel.closedWhile, channel.active.Load())
		}
	})
}

// Parent cancellation reaches the same pending dial context and its callback
// is joined when the dial releases ownership.
func TestWebRtcExtenderCarrierParentCancellationReachesPendingDial(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		carrier := NewWebRtcExtenderCarrier(ctx, nil, nil)
		dialCtx, finish, err := carrier.beginDial(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		cancel()
		synctest.Wait()
		cancelErr := dialCtx.Err()
		finish()
		carrier.Close()
		if !errors.Is(cancelErr, context.Canceled) {
			t.Fatalf("parent did not cancel pending dial: %v", cancelErr)
		}
	})
}

// Canceling a dial only starts its cleanup. Close must retain the factory
// until the owner confirms that cleanup has finished.
func TestWebRtcExtenderCarrierCloseJoinsPendingDial(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		carrier := NewWebRtcExtenderCarrier(context.Background(), nil, nil)
		dialCtx, finish, err := carrier.beginDial(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		closed := make(chan struct{})
		go func() { carrier.Close(); close(closed) }()
		<-dialCtx.Done()
		synctest.Wait()
		select {
		case <-closed:
			t.Fatal("Close returned before the dial released its resources")
		default:
		}
		finish()
		<-closed
	})
}
