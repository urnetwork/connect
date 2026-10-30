// Provider flows install their socket deadlines lazily: a busy flow refreshes
// a deadline only when less than half its timeout remains, and a stall still
// ends a full timeout after the read or write that stalled began. Every event
// runs in virtual time over an in-memory upstream.
package connect

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Runs one provider UDP flow, with its own socket reader and send loop, over
// net.Pipe inside the caller's synctest bubble.
type udpDeadlineTestFlow struct {
	t        *testing.T
	sequence *UdpSequence
	local    *deadlineCountingConn
	upstream net.Conn
	runDone  chan struct{}
}

func newUdpDeadlineTestFlow(t *testing.T, settings *UdpBufferSettings) *udpDeadlineTestFlow {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	pipe, upstream := net.Pipe()
	f := &udpDeadlineTestFlow{
		t:        t,
		local:    &deadlineCountingConn{Conn: pipe},
		upstream: upstream,
		runDone:  make(chan struct{}),
	}
	settings.Log = NewNoopLogger()
	settings.SharedSocketLifecycle = false
	settings.DialContextSettings = &DialContextSettings{
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			return f.local, nil
		},
	}
	f.sequence = NewUdpSequence(ctx, func(TransferPath, protocol.ProvideMode, *IpPath, []byte) {},
		SourceId(NewId()), protocol.ProvideMode_Network, 4,
		net.IPv4(192, 0, 2, 10).To4(), 40001, net.IPv4(203, 0, 113, 20).To4(), 443, settings)
	if f.sequence == nil {
		t.Fatal("could not admit a UDP flow")
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
	synctest.Wait()
	return f
}

// Moves one pool-owned datagram's ownership to the flow.
func (self *udpDeadlineTestFlow) send(payload []byte) {
	self.t.Helper()
	packet := MessagePoolGet(len(payload))
	copy(packet, payload)
	item := &UdpSendItem{
		source:      self.sequence.source,
		provideMode: protocol.ProvideMode_Network,
		udp:         parsedUdp{payload: packet},
		ipPacket:    packet,
	}
	if accepted, err := self.sequence.send(item, -1); !accepted {
		item.release()
		self.t.Fatalf("the flow refused a datagram: %v", err)
	}
}

func (self *udpDeadlineTestFlow) done() bool {
	select {
	case <-self.runDone:
		return true
	default:
		return false
	}
}

// A TCP flow moving data both ways for less than half of each timeout installs
// each deadline once, where it installed one per read and per write batch.
func TestTcpFlowInstallsDeadlinesLazily(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		settings := DefaultTcpBufferSettingsWithBufferSize(16)
		f := newTcpReapTestFlow(t, settings)
		go io.Copy(io.Discard, f.upstream)

		const exchangeCount = 64
		const exchangeSpacing = 100 * time.Millisecond
		for index := range exchangeCount {
			f.send(f.newSendItem([]byte{byte(index)}))
			if _, err := f.upstream.Write([]byte{byte(index)}); err != nil {
				t.Fatal(err)
			}
			time.Sleep(exchangeSpacing)
			synctest.Wait()
		}
		if reads := f.local.readDeadlines.Load(); 1 < reads {
			t.Errorf("%d reads in %s installed %d read deadlines, want one for a %s timeout",
				exchangeCount, exchangeCount*exchangeSpacing, reads, settings.ReadTimeout)
		}
		if writes := f.local.writeDeadlines.Load(); 1 < writes {
			t.Errorf("%d write batches in %s installed %d write deadlines, want one for a %s timeout",
				exchangeCount, exchangeCount*exchangeSpacing, writes, settings.WriteTimeout)
		}
	})
}

// A UDP flow with its own reader and send loop does the same.
func TestUdpFlowInstallsDeadlinesLazily(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		settings := DefaultUdpBufferSettingsWithBufferSize(4)
		settings.IdleTimeout = time.Hour
		f := newUdpDeadlineTestFlow(t, settings)
		go io.Copy(io.Discard, f.upstream)

		const exchangeCount = 64
		const exchangeSpacing = 100 * time.Millisecond
		for index := range exchangeCount {
			f.send([]byte{byte(index)})
			if _, err := f.upstream.Write([]byte{byte(index)}); err != nil {
				t.Fatal(err)
			}
			time.Sleep(exchangeSpacing)
			synctest.Wait()
		}
		if reads := f.local.readDeadlines.Load(); 1 < reads {
			t.Errorf("%d datagrams read in %s installed %d read deadlines, want one for a %s timeout",
				exchangeCount, exchangeCount*exchangeSpacing, reads, settings.ReadTimeout)
		}
		if writes := f.local.writeDeadlines.Load(); 1 < writes {
			t.Errorf("%d datagrams written in %s installed %d write deadlines, want one for a %s timeout",
				exchangeCount, exchangeCount*exchangeSpacing, writes, settings.WriteTimeout)
		}
	})
}

// A UDP flow writing under its lock, with the shared lifecycle, does the same
// for its writes.
func TestUdpSharedSocketInstallsWriteDeadlineLazily(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		settings := DefaultUdpBufferSettingsWithBufferSize(4)
		settings.Log = NewNoopLogger()
		settings.SharedSocketLifecycle = true
		sequence := NewUdpSequence(ctx, func(TransferPath, protocol.ProvideMode, *IpPath, []byte) {},
			SourceId(NewId()), protocol.ProvideMode_Network, 4,
			net.IPv4(192, 0, 2, 10).To4(), 40001, net.IPv4(203, 0, 113, 20).To4(), 443, settings)
		if sequence == nil {
			t.Fatal("could not admit a UDP flow")
		}
		pipe, upstream := net.Pipe()
		local := &deadlineCountingConn{Conn: pipe}
		sequence.sharedSocketLifecycle = true
		sequence.sharedSocket = local
		drained := make(chan struct{})
		go func() {
			defer close(drained)
			io.Copy(io.Discard, upstream)
		}()
		defer func() {
			sequence.Close()
			upstream.Close()
			<-drained
		}()

		const datagramCount = 64
		const datagramSpacing = 100 * time.Millisecond
		for index := range datagramCount {
			packet := MessagePoolGet(1)
			packet[0] = byte(index)
			item := &UdpSendItem{
				source:      sequence.source,
				provideMode: protocol.ProvideMode_Network,
				udp:         parsedUdp{payload: packet},
				ipPacket:    packet,
			}
			if accepted, err := sequence.send(item, 0); !accepted {
				item.release()
				t.Fatalf("the shared socket refused datagram %d: %v", index, err)
			}
			time.Sleep(datagramSpacing)
		}
		if writes := local.writeDeadlines.Load(); 1 < writes {
			t.Errorf("%d datagrams written in %s installed %d write deadlines, want one for a %s timeout",
				datagramCount, datagramCount*datagramSpacing, writes, settings.WriteTimeout)
		}
	})
}

// A read that began with less than the full timeout left on the installed
// deadline is not cut short: the silent upstream is reset a full ReadTimeout
// after the read that follows its last byte began.
func TestTcpFlowReadStallExpiresAFullTimeoutAfterTheLastRead(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		settings := DefaultTcpBufferSettingsWithBufferSize(16)
		// only the silent upstream can end the flow
		settings.IdleTimeout = time.Hour
		readTimeout := settings.ReadTimeout
		f := newTcpReapTestFlow(t, settings)
		go io.Copy(io.Discard, f.upstream)

		// past the connect deadline's start, short of its refresh point
		time.Sleep(readTimeout * 2 / 5)
		if _, err := f.upstream.Write([]byte{1}); err != nil {
			t.Fatal(err)
		}
		lastReadAt := time.Now()
		synctest.Wait()

		time.Sleep(readTimeout - time.Nanosecond)
		synctest.Wait()
		if got := f.reset(); !got.IsZero() || f.done() {
			t.Fatalf("the flow ended %s after the last read, before ReadTimeout", got.Sub(lastReadAt))
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		if got := f.reset(); !got.Equal(lastReadAt.Add(readTimeout)) {
			t.Fatalf("the silent upstream was reset %s after the last read, want %s", got.Sub(lastReadAt), readTimeout)
		}
	})
}

// A write batch that began with less than the full timeout left on the
// installed deadline is not cut short: an upstream that stops reading ends the
// flow a full WriteTimeout after the stalled batch began.
func TestTcpFlowWriteStallExpiresAFullTimeoutAfterTheBatchBegan(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		settings := DefaultTcpBufferSettingsWithBufferSize(16)
		// only the stalled write can end the flow
		settings.ReadTimeout = time.Hour
		settings.IdleTimeout = time.Hour
		writeTimeout := settings.WriteTimeout
		f := newTcpReapTestFlow(t, settings)

		f.send(f.newSendItem([]byte("first")))
		first := make([]byte, len("first"))
		if _, err := io.ReadFull(f.upstream, first); err != nil || string(first) != "first" {
			t.Fatalf("the upstream read %q, %v", first, err)
		}
		// past the first batch's deadline start, short of its refresh point;
		// the upstream reads nothing more
		time.Sleep(writeTimeout * 2 / 5)
		f.send(f.newSendItem([]byte("second")))
		stalledAt := time.Now()
		synctest.Wait()

		time.Sleep(writeTimeout - time.Nanosecond)
		synctest.Wait()
		if f.done() {
			t.Fatal("the stalled write ended the flow before WriteTimeout")
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		if !f.done() {
			t.Fatalf("the stalled write did not end the flow %s after its batch began", time.Since(stalledAt))
		}
	})
}

// The UDP reader does the same for a silent upstream.
func TestUdpFlowReadStallExpiresAFullTimeoutAfterTheLastRead(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		settings := DefaultUdpBufferSettingsWithBufferSize(4)
		// only the silent upstream can end the flow
		settings.IdleTimeout = time.Hour
		readTimeout := settings.ReadTimeout
		f := newUdpDeadlineTestFlow(t, settings)

		time.Sleep(readTimeout * 2 / 5)
		if _, err := f.upstream.Write([]byte{1}); err != nil {
			t.Fatal(err)
		}
		lastReadAt := time.Now()
		synctest.Wait()

		time.Sleep(readTimeout - time.Nanosecond)
		synctest.Wait()
		if f.done() {
			t.Fatal("the flow ended before ReadTimeout after its last read")
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		if !f.done() {
			t.Fatalf("the silent upstream did not end the flow %s after the last read", time.Since(lastReadAt))
		}
	})
}

// Records each deadline installed, and can refuse them.
type ioDeadlineRecorder struct {
	installs []time.Time
	err      error
}

func (self *ioDeadlineRecorder) set(deadline time.Time) error {
	self.installs = append(self.installs, deadline)
	return self.err
}

// An operation installs a full deadline only once less than half the timeout
// remains on the installed one.
func TestIoDeadlineRefreshesOnlyPastHalfItsTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		timeout := 10 * time.Second
		recorder := &ioDeadlineRecorder{}
		deadline := newIoDeadline(timeout, recorder.set)
		start := time.Now()
		for range 11 {
			if _, err := deadline.begin(); err != nil {
				t.Fatal(err)
			}
			time.Sleep(time.Second)
		}
		want := []time.Time{start.Add(timeout), start.Add(6*time.Second + timeout)}
		if len(recorder.installs) != len(want) || !recorder.installs[0].Equal(want[0]) || !recorder.installs[1].Equal(want[1]) {
			t.Fatalf("operations each second for %s installed %v, want %v", 10*time.Second, recorder.installs, want)
		}
	})
}

// An expiry before the full timeout since the operation began re-arms to
// exactly that bound, once; the expiry at the bound, and any other failure,
// is the operation's result.
func TestIoDeadlineRetryWaitsOutTheFullTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		timeout := 10 * time.Second
		recorder := &ioDeadlineRecorder{}
		deadline := newIoDeadline(timeout, recorder.set)
		if _, err := deadline.begin(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(4 * time.Second)
		operationStart, err := deadline.begin()
		if err != nil || len(recorder.installs) != 1 {
			t.Fatalf("an operation with more than half the timeout left installed again: %v %v", recorder.installs, err)
		}

		if deadline.retry(operationStart, io.EOF) || len(recorder.installs) != 1 {
			t.Fatal("a failure other than an expiry was retried")
		}
		time.Sleep(6 * time.Second)
		if !deadline.retry(operationStart, os.ErrDeadlineExceeded) {
			t.Fatal("an expiry 6s into a 10s operation was taken as a stall")
		}
		if got := recorder.installs[len(recorder.installs)-1]; len(recorder.installs) != 2 || !got.Equal(operationStart.Add(timeout)) {
			t.Fatalf("the retry installed %v, want the operation's full bound %s", recorder.installs, operationStart.Add(timeout))
		}
		time.Sleep(4 * time.Second)
		if deadline.retry(operationStart, os.ErrDeadlineExceeded) || len(recorder.installs) != 2 {
			t.Fatal("an expiry a full timeout after the operation began was retried")
		}
	})
}

// A refused install leaves no deadline believed installed, so the next
// operation installs again.
func TestIoDeadlineRefusedInstallIsRetriedByTheNextOperation(t *testing.T) {
	recorder := &ioDeadlineRecorder{err: errors.New("synthetic refusal")}
	deadline := newIoDeadline(time.Minute, recorder.set)
	if _, err := deadline.begin(); err == nil {
		t.Fatal("a refused install was not reported")
	}
	recorder.err = nil
	if _, err := deadline.begin(); err != nil || len(recorder.installs) != 2 {
		t.Fatalf("the operation after a refusal installed %d deadlines, err=%v", len(recorder.installs), err)
	}
}
