// Reader-exit regressions force lifecycle ordering through real NAT, TCP and
// provider constructors. Only the upstream read and existing test seams pause;
// no helper synthesizes a return reset or changes its admission policy.
package connect

import (
	"encoding/binary"
	"io"
	"net"
	"sync"
	"testing"
	"testing/synctest"
)

// Keeps one real socket read outstanding until the test releases its terminal
// error. Close remains independently observable, so cancellation can precede
// reader exit without relying on scheduling or a short timeout.
type tcpReaderResetSocket struct {
	net.Conn
	entered     chan struct{}
	released    chan struct{}
	closed      chan struct{}
	enteredOnce sync.Once
	releaseOnce sync.Once
	closeOnce   sync.Once
}

// The fixed non-EOF error reaches the real reader's fallback reset path.
func (self *tcpReaderResetSocket) Read(_ []byte) (int, error) {
	self.enteredOnce.Do(func() { close(self.entered) })
	<-self.released
	return 0, io.ErrUnexpectedEOF
}

// Does not release the read barrier; the test controls that distinct event.
func (self *tcpReaderResetSocket) Close() error {
	self.closeOnce.Do(func() { close(self.closed) })
	return self.Conn.Close()
}

// Idempotence lets failure cleanup discharge the same barrier safely.
func (self *tcpReaderResetSocket) release() {
	self.releaseOnce.Do(func() { close(self.released) })
}

// Both flows use real provider return admission. A different live tuple is a
// positive isolation control, not merely an uncanceled shared context.
type tcpReaderResetFixture struct {
	base        *tcpProviderRetirementFixture
	flow        *TcpSequence
	sibling     *TcpSequence
	siblingPath IpPath
	socket      *tcpReaderResetSocket
	results     chan providerReturnSendResult
}

// Takes a generated packet on successful NAT admission; returns it on refusal.
// Timestamp negotiation reproduces the observed 52-byte IPv4 terminal control.
func tcpReaderResetSend(t *testing.T, base *tcpProviderRetirementFixture, path *IpPath, flags byte, sequence, ack uint32, payload []byte) {
	t.Helper()
	headerBytes := TcpHeaderSizeWithoutExtensions + 12
	packet, tcp := ipTransportPacket(path, ipProtocolNumberTcp, headerBytes+len(payload))
	binary.BigEndian.PutUint16(tcp[0:2], uint16(path.SourcePort))
	binary.BigEndian.PutUint16(tcp[2:4], uint16(path.DestinationPort))
	binary.BigEndian.PutUint32(tcp[4:8], sequence)
	binary.BigEndian.PutUint32(tcp[8:12], ack)
	tcp[12] = byte(headerBytes/4) << 4
	tcp[13] = flags
	binary.BigEndian.PutUint16(tcp[14:16], 4096)
	copy(tcp[TcpHeaderSizeWithoutExtensions:headerBytes], []byte{1, 1, 8, 10, 0, 0, 0, 1, 0, 0, 0, 0})
	copy(tcp[headerBytes:], payload)
	binary.BigEndian.PutUint16(tcp[16:18], ipPathTransportChecksum(path, ipProtocolNumberTcp, tcp))
	owned := MessagePoolCopy(packet)
	if !base.nat.SendPacket(base.source, tcpRetirementNetworkProvideMode, owned, 0) {
		MessagePoolReturn(owned)
		t.Fatal("reader reset fixture input refused before the TCP sequence")
	}
}

// Sequence hooks are installed before their producing worker can start. The pure
// acknowledgement hook isolates the reader reset from a separate FIN-ack race;
// it does not supply cancellation and uses the worker's existing cleanup path.
func newTcpReaderResetFixture(t *testing.T, version int) *tcpReaderResetFixture {
	t.Helper()
	fixture := &tcpReaderResetFixture{results: make(chan providerReturnSendResult, 8)}
	fixture.base = newTcpProviderRetirementFixtureWithSetup(t, func(socket net.Conn) net.Conn {
		if fixture.socket != nil {
			return socket
		}
		fixture.socket = &tcpReaderResetSocket{
			Conn: socket, entered: make(chan struct{}), released: make(chan struct{}), closed: make(chan struct{}),
		}
		return fixture.socket
	}, func(sequence *TcpSequence) {
		if sequence.initialSynSeq == 1000 {
			sequence.afterPureAckBuildForTest = func([]byte) { <-sequence.ctx.Done() }
		}
	})
	t.Cleanup(fixture.socket.release)
	if version == 6 {
		fixture.base.path.Version = 6
		fixture.base.path.SourceIp = net.ParseIP("2001:db8::3").To16()
		fixture.base.path.DestinationIp = net.ParseIP("2001:db8::7").To16()
	}
	tcpReaderResetSend(t, fixture.base, fixture.base.path, tcpFlagSyn, 1000, 0, nil)
	fixture.flow = fixture.base.nextSequence(1000)
	controls := fixture.base.takeControls()
	if len(controls) != 1 || !controls[0].syn || !controls[0].ack || controls[0].ackNumber != 1001 {
		t.Fatalf("IPv%d first handshake=%+v", version, controls)
	}
	select {
	case <-fixture.socket.entered:
	default:
		t.Fatalf("IPv%d real reader did not reach the socket barrier", version)
	}
	fixture.flow.mutex.Lock()
	timestamps := fixture.flow.enableTimestamp
	fixture.flow.mutex.Unlock()
	if !timestamps || fixture.flow.ctx.Err() != nil {
		t.Fatalf("IPv%d first flow is not live with negotiated timestamps", version)
	}
	fixture.siblingPath = *fixture.base.path
	fixture.siblingPath.SourcePort++
	tcpReaderResetSend(t, fixture.base, &fixture.siblingPath, tcpFlagSyn, 2000, 0, nil)
	fixture.sibling = fixture.base.nextSequence(2000)
	controls = fixture.base.takeControls()
	if len(controls) != 1 || !controls[0].syn || !controls[0].ack || controls[0].ackNumber != 2001 || fixture.sibling.ctx.Err() != nil {
		t.Fatalf("IPv%d live sibling handshake=%+v", version, controls)
	}
	// nextSequence's synctest.Wait synchronizes prior callback accesses. Both readers
	// are now blocked, and the next input/barrier release publishes this hook.
	fixture.base.provider.afterReturnSendForTest = func(result providerReturnSendResult) { fixture.results <- result }
	return fixture
}

// A positive socket-close observation proves the Run owner has reacted to
// cancellation, while the reader is still held before its deferred reset path.
func (self *tcpReaderResetFixture) assertCanceledBeforeReadExit(t *testing.T) {
	t.Helper()
	synctest.Wait()
	select {
	case <-self.socket.closed:
	default:
		t.Fatal("TCP owner did not close its socket at the cancellation barrier")
	}
	if self.flow.ctx.Err() == nil {
		t.Fatal("socket closed without exact flow cancellation")
	}
	select {
	case <-self.flow.retirementDone:
		t.Fatal("TCP flow retired while its socket reader was still held")
	default:
	}
}

// Joins every sequence child before checking provider counters. Queued owners
// are inspected/released separately; retirement is not a claim of delivery.
func (self *tcpReaderResetFixture) assertRetired(t *testing.T) {
	t.Helper()
	synctest.Wait()
	for _, barrier := range []struct {
		name string
		done <-chan struct{}
	}{
		{name: "child join entry", done: self.base.oldChildJoinStarted},
		{name: "pure acknowledgement worker", done: self.base.oldAckWorkerDone},
		{name: "sequence retirement", done: self.flow.retirementDone},
	} {
		select {
		case <-barrier.done:
		default:
			t.Fatalf("TCP reader reset test has not joined %s", barrier.name)
		}
	}
	if self.flow.ctx.Err() == nil || self.base.provider.ctx.Err() != nil || self.base.client.Ctx().Err() != nil || self.base.nat.ctx.Err() != nil || self.sibling.ctx.Err() != nil {
		t.Fatal("retirement did not isolate the canceled flow from live shared owners and sibling")
	}
}

// Requires actual application bytes and its real pure acknowledgement to pass
// on the live sibling after the first flow's child workers have all joined.
func (self *tcpReaderResetFixture) assertSiblingProgress(t *testing.T) {
	t.Helper()
	const payload = "live sibling"
	self.sibling.mutex.Lock()
	ack := self.sibling.receiveSeq
	self.sibling.mutex.Unlock()
	read := make(chan error, 1)
	go func() {
		buffer := make([]byte, len(payload))
		_, err := io.ReadFull(self.base.upstreams[1], buffer)
		if err == nil && string(buffer) != payload {
			err = io.ErrUnexpectedEOF
		}
		read <- err
	}()
	tcpReaderResetSend(t, self.base, &self.siblingPath, tcpFlagAck, 2001, ack, []byte(payload))
	synctest.Wait()
	select {
	case err := <-read:
		if err != nil {
			t.Fatalf("healthy sibling socket read: %v", err)
		}
	default:
		t.Fatal("healthy sibling upload did not reach its actual socket writer")
	}
	controls := self.base.takeControls()
	if len(controls) != 1 || controls[0].syn || !controls[0].ack || controls[0].fin || controls[0].rst || controls[0].payloadByteCount != 0 || controls[0].ackNumber != 2001+uint32(len(payload)) {
		t.Fatalf("healthy sibling did not retain upload/ack progress: %+v", controls)
	}
	if self.sibling.ctx.Err() != nil || self.base.provider.ctx.Err() != nil || self.base.client.Ctx().Err() != nil {
		t.Fatal("healthy sibling progress canceled a shared owner")
	}
}

// Normal peer FIN closes the real writer queue. Its drained worker cancels the
// exact flow before the held reader exits; no explicit test cancellation does.
func TestProviderTcpReaderExitAfterFinDoesNotSynthesizeReset(t *testing.T) {
	assertMessagePoolOwnership(t)
	MessagePoolReturn(MessagePoolGet(1))
	for _, version := range []int{4, 6} {
		synctest.Test(t, func(t *testing.T) {
			fixture := newTcpReaderResetFixture(t, version)
			fixture.flow.mutex.Lock()
			ack := fixture.flow.receiveSeq
			fixture.flow.mutex.Unlock()
			tcpReaderResetSend(t, fixture.base, fixture.base.path, tcpFlagFin|tcpFlagAck, 1001, ack, nil)
			fixture.assertCanceledBeforeReadExit(t)
			fixture.socket.release()
			fixture.assertRetired(t)
			if drops := fixture.base.provider.CongestionDropStats(); drops != (ProviderCongestionDrops{}) {
				t.Errorf("IPv%d closed TCP reader synthesized a new reset after normal FIN: %+v", version, drops)
			}
			if controls := fixture.base.takeControls(); len(controls) != 0 {
				t.Errorf("IPv%d canceled reader published new controls: %+v", version, controls)
			}
			fixture.assertSiblingProgress(t)
		})
	}
}

// Explicit lifecycle cancellation has the same no-new-reset rule even without
// a FIN or replacement SYN to wake the flow. Shared parents remain live.
func TestProviderTcpReaderExitAfterCancelDoesNotSynthesizeReset(t *testing.T) {
	assertMessagePoolOwnership(t)
	MessagePoolReturn(MessagePoolGet(1))
	for _, version := range []int{4, 6} {
		synctest.Test(t, func(t *testing.T) {
			fixture := newTcpReaderResetFixture(t, version)
			fixture.flow.Cancel()
			fixture.assertCanceledBeforeReadExit(t)
			fixture.socket.release()
			fixture.assertRetired(t)
			if drops := fixture.base.provider.CongestionDropStats(); drops != (ProviderCongestionDrops{}) {
				t.Errorf("IPv%d closed TCP reader synthesized a new reset after explicit cancellation: %+v", version, drops)
			}
			if controls := fixture.base.takeControls(); len(controls) != 0 {
				t.Errorf("IPv%d canceled reader published new controls: %+v", version, controls)
			}
			fixture.assertSiblingProgress(t)
		})
	}
}

// An upstream error while the flow is live must still create and actually admit
// the terminal reset. The reader, not the test, constructs the packet.
func TestProviderTcpReaderActiveFailurePreservesReset(t *testing.T) {
	assertMessagePoolOwnership(t)
	MessagePoolReturn(MessagePoolGet(1))
	for _, version := range []int{4, 6} {
		synctest.Test(t, func(t *testing.T) {
			fixture := newTcpReaderResetFixture(t, version)
			fixture.socket.release()
			fixture.assertRetired(t)
			controls := fixture.base.takeControls()
			if len(controls) != 1 || controls[0].syn || !controls[0].ack || controls[0].fin || !controls[0].rst || controls[0].payloadByteCount != 0 {
				t.Fatalf("IPv%d active-origin error lost its actual reset: %+v", version, controls)
			}
			select {
			case result := <-fixture.results:
				wantBytes := ByteCount(52)
				if version == 6 {
					wantBytes = 72
				}
				if !result.sent || result.packetCount != 1 || result.packetByteCount != wantBytes {
					t.Fatalf("IPv%d active reset disposition=%+v, want one admitted %d-byte packet", version, result, wantBytes)
				}
			default:
				t.Fatal("active reader reset has no completed provider disposition")
			}
			if drops := fixture.base.provider.CongestionDropStats(); drops != (ProviderCongestionDrops{}) {
				t.Fatalf("IPv%d active-origin reset was counted as refused: %+v", version, drops)
			}
			fixture.assertSiblingProgress(t)
		})
	}
}

// Once the active reader's reset is owned by the provider, later cancellation
// must retain ordinary refusal accounting and release every pooled share. This
// explicitly covers the race after the new pre-construction context check.
func TestProviderTcpReaderQueuedResetCancellationKeepsRefusal(t *testing.T) {
	assertMessagePoolOwnership(t)
	MessagePoolReturn(MessagePoolGet(1))
	for _, version := range []int{4, 6} {
		synctest.Test(t, func(t *testing.T) {
			fixture := newTcpReaderResetFixture(t, version)
			started, release := make(chan struct{}), make(chan struct{})
			var startedOnce, releaseOnce sync.Once
			releaseReturn := func() { releaseOnce.Do(func() { close(release) }) }
			t.Cleanup(releaseReturn)
			// Setup ended at synctest quiescence; releasing the held reader
			// publishes this hook before any subsequent callback can access it.
			fixture.base.provider.returnSendStarted = func() { startedOnce.Do(func() { close(started) }); <-release }
			fixture.socket.release()
			synctest.Wait()
			select {
			case <-started:
			default:
				t.Fatal("active reader did not transfer its reset to provider custody")
			}
			if fixture.flow.ctx.Err() != nil {
				t.Fatal("reset was not constructed while its exact flow was active")
			}
			fixture.flow.Cancel()
			releaseReturn()
			fixture.assertRetired(t)
			// The first flow has joined and the sibling is still blocked.
			fixture.base.provider.returnSendStarted = nil
			wantBytes := ByteCount(52)
			if version == 6 {
				wantBytes = 72
			}
			wantDrops := ProviderCongestionDrops{ReturnSendPacketCount: 1, ReturnSendByteCount: wantBytes}
			if drops := fixture.base.provider.CongestionDropStats(); drops != wantDrops {
				t.Fatalf("IPv%d already-owned reset cancellation lost exact refusal accounting: got %+v want %+v", version, drops, wantDrops)
			}
			select {
			case result := <-fixture.results:
				if result.sent || result.packetCount != 1 || result.packetByteCount != wantBytes {
					t.Fatalf("IPv%d canceled owned reset disposition=%+v", version, result)
				}
			default:
				t.Fatal("canceled owned reset has no completed provider disposition")
			}
			if controls := fixture.base.takeControls(); len(controls) != 0 {
				t.Fatalf("IPv%d canceled owned reset reached Transfer: %+v", version, controls)
			}
			fixture.assertSiblingProgress(t)
		})
	}
}
