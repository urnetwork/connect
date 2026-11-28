//go:build darwin || ios || linux || android

// The shared poller must consume oversize without buying Transfer credit,
// while a following admissible datagram stays kernel-owned under backpressure.
package connect

import (
	"bytes"
	"errors"
	"net"
	"syscall"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

// RawConn readability is the barrier; no sleep or negative timeout proves
// consumption. The exact-limit datagram is peeked after oversize was discarded.
func TestUdpSharedReaderDiscardsOversizeBeforeFullCredit(t *testing.T) {
	assertMessagePoolOwnership(t)
	sequence, parent := newUdpReturnReadTestSequence(t)
	collector := &mtuUdpReadCollector{source: sequence.source, healthy: make(chan struct{})}
	defer collector.gate.close()
	siblingCollector := &mtuUdpReadCollector{source: SourceId(NewId()), healthy: make(chan struct{})}
	defer siblingCollector.gate.close()
	sibling := NewUdpSequence(t.Context(), func(_ TransferPath, _ protocol.ProvideMode, _ *IpPath, packet []byte) {
		siblingCollector.take([][]byte{MessagePoolShareReadOnly(packet)})
	}, siblingCollector.source, protocol.ProvideMode_Network, 4,
		net.IPv4(192, 0, 2, 1).To4(), 42001, net.IPv4(127, 0, 0, 1).To4(), 44001, sequence.udpBufferSettings)
	if sibling == nil {
		t.Fatal("healthy sibling construction failed")
	}
	sibling.sharedSocketLifecycle = true
	defer sibling.Close()
	const readCredit = 4096
	sequence.prepareReturnReadCallback = func(_ *UdpSequence, size int, released func()) (udpReturnReadConsumer, bool) {
		collector.prepared = append(collector.prepared, size)
		if !parent.TryReserve(readCredit) {
			return nil, false
		}
		return &mtuUdpReadConsumer{collector: collector, released: func() { parent.Release(readCredit); released() }}, true
	}
	occupied := parent.Available()
	if !parent.TryReserve(occupied) {
		t.Fatal("could not establish full parent")
	}
	defer func() { parent.Release(occupied) }()
	socket, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer socket.Close()
	if err := socket.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	sender, err := net.DialUDP("udp4", nil, socket.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer sender.Close()
	raw, err := socket.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	maximum := sequence.udpBufferSettings.ReadBufferByteCount
	buffer := make([]byte, udpReadScratchByteCount(maximum))
	drain := func(wantPeek int, wantErr error) {
		t.Helper()
		var drainErr, peekErr error
		var peekCount int
		if err := raw.Read(func(fd uintptr) bool {
			peekCount, _, peekErr = syscall.Recvfrom(int(fd), buffer, syscall.MSG_PEEK)
			if errors.Is(peekErr, syscall.EAGAIN) || errors.Is(peekErr, syscall.EWOULDBLOCK) {
				return false
			}
			if peekErr == nil {
				drainErr = drainReadyUdpSocketWithLimit(int(fd), sequence, buffer, 1)
			}
			return true
		}); err != nil {
			t.Fatal(err)
		}
		if peekErr != nil || peekCount != wantPeek || !errors.Is(drainErr, wantErr) {
			t.Fatalf("peek=%d/%d peekErr=%v drainErr=%v want=%v", peekCount, wantPeek, peekErr, drainErr, wantErr)
		}
	}
	write := func(payload []byte) {
		t.Helper()
		if n, err := sender.Write(payload); err != nil || n != len(payload) {
			t.Fatalf("UDP write=%d/%d err=%v", n, len(payload), err)
		}
	}
	for _, size := range []int{maximum + 1, 2 * maximum} {
		write(fragmentTestPayload(size))
		drain(maximum+1, nil)
		if len(collector.prepared) != 0 || len(collector.payloads) != 0 || sequence.returnReadPending.Load() {
			t.Fatal("oversized read acquired credit or delivered a truncated prefix")
		}
	}
	// A different already-funded flow remains deliverable while this flow's
	// Transfer admission is full; the shared parent is never canceled.
	siblingSocket := newUdpReturnReadTestSocket(t, []byte("healthy-after-oversize"))
	siblingRaw, err := siblingSocket.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var siblingErr error
	if err := siblingRaw.Control(func(fd uintptr) {
		siblingErr = drainReadyUdpSocketWithLimit(int(fd), sibling, buffer, 1)
	}); err != nil || siblingErr != nil {
		t.Fatalf("healthy shared flow: raw=%v drain=%v", err, siblingErr)
	}
	if siblingCollector.err != nil || len(siblingCollector.payloads) != 1 || !bytes.Equal(siblingCollector.payloads[0], []byte("healthy-after-oversize")) {
		t.Fatal("full parent or oversized sibling prevented healthy delivery")
	}
	exact := fragmentTestPayload(maximum)
	write(exact)
	drain(maximum, errUdpReturnReadPaused)
	if len(collector.prepared) != 1 || collector.prepared[0] != maximum || len(collector.payloads) != 0 {
		t.Fatal("admissible datagram did not remain paused behind full credit")
	}
	if sequence.ctx.Err() != nil || parent.UsedByteCount() != parent.TotalByteCount() {
		t.Fatal("oversize/refusal changed shared lifetime or full parent")
	}
	parent.Release(occupied)
	occupied = 0
	drain(maximum, nil) // the same datagram is still kernel-owned at this barrier
	write(nil)
	drain(0, nil)
	write([]byte("healthy-after-oversize"))
	drain(len("healthy-after-oversize"), nil)
	if collector.err != nil {
		t.Fatal(collector.err)
	}
	if len(collector.payloads) != 3 || !bytes.Equal(collector.payloads[0], exact) || len(collector.payloads[1]) != 0 || !bytes.Equal(collector.payloads[2], []byte("healthy-after-oversize")) {
		t.Fatal("exact-limit/empty/healthy UDP datagrams did not recover intact")
	}
	if collector.aborted != 0 || sequence.returnReadPending.Load() {
		t.Fatal("completed shared reads retained their leases")
	}
	sequence.Close()
	if sibling.ctx.Err() != nil || parent.UsedByteCount() != natUdpFlowMemoryByteCount(sibling.udpBufferSettings) {
		t.Fatal("retired flow changed a live sibling's owner")
	}
	sibling.Close()
	if parent.UsedByteCount() != 0 {
		t.Fatal("closed flows retained a parent claim")
	}
	// The same root remains reusable after both independent flow owners retire.
	if !parent.TryReserve(readCredit) {
		t.Fatal("healthy sibling could not reuse the released parent")
	}
	parent.Release(readCredit)
}
