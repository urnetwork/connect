//go:build !js

package connect

import (
	"context"
	"errors"
	"net"
	"os"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// A detached data channel that answers every read and write at once and
// records the deadlines installed on it. On the real SCTP stream every read
// deadline install takes the stream lock, and a write deadline install also
// re-arms the stream's deadline timer.
type deadlineRecordingDetachedDataChannel struct {
	stateLock      sync.Mutex
	readDeadlines  []time.Time
	writeDeadlines []time.Time
}

func (self *deadlineRecordingDetachedDataChannel) Read(b []byte) (int, error) {
	return copy(b, []byte{0x5a}), nil
}

func (self *deadlineRecordingDetachedDataChannel) ReadDataChannel(b []byte) (int, bool, error) {
	n, err := self.Read(b)
	return n, false, err
}

func (self *deadlineRecordingDetachedDataChannel) Write(b []byte) (int, error) {
	return len(b), nil
}

func (self *deadlineRecordingDetachedDataChannel) WriteDataChannel(b []byte, _ bool) (int, error) {
	return self.Write(b)
}

func (self *deadlineRecordingDetachedDataChannel) Close() error {
	return nil
}

func (self *deadlineRecordingDetachedDataChannel) SetReadDeadline(deadline time.Time) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.readDeadlines = append(self.readDeadlines, deadline)
	return nil
}

func (self *deadlineRecordingDetachedDataChannel) SetWriteDeadline(deadline time.Time) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.writeDeadlines = append(self.writeDeadlines, deadline)
	return nil
}

// The read and the write deadlines installed so far, as copies.
func (self *deadlineRecordingDetachedDataChannel) installed() ([]time.Time, []time.Time) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]time.Time(nil), self.readDeadlines...), append([]time.Time(nil), self.writeDeadlines...)
}

// One peer connection around an already detached data channel, with the SCTP
// progress watchdog off since there is no association under it.
func newDeadlineReuseTestPeerConn(raw *deadlineRecordingDetachedDataChannel) (*peerConn, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	settings := DefaultWebRtcSettings()
	settings.SctpNoProgressTimeout = 0
	conn := &peerConn{
		ctx:              ctx,
		cancel:           cancel,
		log:              NewNoopLogger(),
		settings:         settings,
		connMonitor:      NewMonitor(),
		connectedMonitor: NewMonitor(),
		teardownDone:     make(chan struct{}),
	}
	if raw != nil {
		conn.conn = raw
	}
	return conn, cancel
}

// Messages under an unchanged deadline install it on the data channel once:
// the steady-state reader's cleared read deadline is never installed again,
// and a write deadline is installed when it changes, not per message.
func TestWebRtcPeerConnInstallsDeadlinesOnlyWhenTheyChange(t *testing.T) {
	raw := &deadlineRecordingDetachedDataChannel{}
	conn, cancel := newDeadlineReuseTestPeerConn(raw)
	defer cancel()

	const messageCount = 64
	message := make([]byte, 1200)
	buffer := make([]byte, 4096)
	firstWriteDeadline := time.Now().Add(time.Hour)
	secondWriteDeadline := firstWriteDeadline.Add(time.Minute)
	readDeadline := time.Now().Add(time.Hour)
	steps := []func() error{
		func() error { return conn.SetWriteDeadline(firstWriteDeadline) },
		func() error { return writeMessages(conn, message, messageCount) },
		func() error { return conn.SetWriteDeadline(secondWriteDeadline) },
		func() error { return writeMessages(conn, message, messageCount) },
		// the steady-state reader clears the read deadline once
		func() error { return conn.SetReadDeadline(time.Time{}) },
		func() error { return readMessages(conn, buffer, messageCount) },
		func() error { return conn.SetReadDeadline(readDeadline) },
		func() error { return readMessages(conn, buffer, messageCount) },
		func() error { return conn.SetReadDeadline(time.Time{}) },
		func() error { return readMessages(conn, buffer, messageCount) },
	}
	for i, step := range steps {
		if err := step(); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
	}

	readDeadlines, writeDeadlines := raw.installed()
	if len(writeDeadlines) != 2 || !writeDeadlines[0].Equal(firstWriteDeadline) || !writeDeadlines[1].Equal(secondWriteDeadline) {
		t.Errorf("%d write deadlines installed for %d messages, expected the 2 distinct ones", len(writeDeadlines), 2*messageCount)
	}
	if len(readDeadlines) != 2 || !readDeadlines[0].Equal(readDeadline) || !readDeadlines[1].IsZero() {
		t.Errorf("%d read deadlines installed for %d messages, expected the one set and its clearing", len(readDeadlines), 3*messageCount)
	}
}

func writeMessages(conn net.Conn, message []byte, messageCount int) error {
	for range messageCount {
		if _, err := conn.Write(message); err != nil {
			return err
		}
	}
	return nil
}

func readMessages(conn net.Conn, buffer []byte, messageCount int) error {
	for range messageCount {
		if _, err := conn.Read(buffer); err != nil {
			return err
		}
	}
	return nil
}

// A ready data channel is returned without arming a timer for the deadline:
// the timer bounds only the wait for a channel that is not open yet, and before
// one was created and stopped for every message.
func TestWebRtcPeerConnReadyDataChannelArmsNoDeadlineTimer(t *testing.T) {
	conn, cancel := newDeadlineReuseTestPeerConn(&deadlineRecordingDetachedDataChannel{})
	defer cancel()
	deadline := time.Now().Add(time.Hour)
	allocCount := testing.AllocsPerRun(64, func() {
		if _, err := conn.dataChannelConn(deadline); err != nil {
			t.Fatal(err)
		}
	})
	if allocCount != 0 {
		t.Fatalf("a ready data channel cost %.0f allocations per message, expected none", allocCount)
	}
}

// The deadline still bounds the wait for a data channel that is not open: it
// fails at the deadline, and a channel installed before it is returned. A
// deadline that has already passed fails at once, even with the channel open.
func TestWebRtcPeerConnDataChannelWaitKeepsItsDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		conn, cancel := newDeadlineReuseTestPeerConn(nil)
		defer cancel()
		start := time.Now()
		_, err := conn.dataChannelConn(start.Add(5 * time.Second))
		if !errors.Is(err, os.ErrDeadlineExceeded) || time.Since(start) != 5*time.Second {
			t.Fatalf("a channel that never opened ended the wait after %s with %v, expected its 5 s deadline", time.Since(start), err)
		}

		raw := &deadlineRecordingDetachedDataChannel{}
		waited := make(chan error, 1)
		start = time.Now()
		go func() {
			c, err := conn.dataChannelConn(start.Add(5 * time.Second))
			if err == nil && c != raw {
				err = errors.New("another data channel was returned")
			}
			waited <- err
		}()
		time.Sleep(2 * time.Second)
		if !conn.installOpenDataChannel(raw) {
			t.Fatal("the data channel was not installed")
		}
		synctest.Wait()
		select {
		case err := <-waited:
			if err != nil || time.Since(start) != 2*time.Second {
				t.Fatalf("the wait ended after %s with %v, expected the channel opened at 2 s", time.Since(start), err)
			}
		default:
			t.Fatal("the opened channel did not end the wait")
		}

		if _, err := conn.dataChannelConn(time.Now()); !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("a passed deadline with the channel open returned %v, expected the deadline", err)
		}
	})
}
