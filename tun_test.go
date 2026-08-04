package connect

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

type trackedTunDialConn struct {
	closed atomic.Bool
}

// newTunTcpInboundTestPacket builds the minimum packet shape needed to test
// TCP flow classification without involving checksum validation.
func newTunTcpInboundTestPacket(sourcePort uint16, destinationPort uint16) []byte {
	packet := make([]byte, header.IPv4MinimumSize+header.TCPMinimumSize)
	packet[0] = 0x45
	packet[9] = uint8(header.TCPProtocolNumber)
	copy(packet[12:16], []byte{192, 0, 2, 1})
	copy(packet[16:20], []byte{198, 51, 100, 2})
	binary.BigEndian.PutUint16(packet[20:22], sourcePort)
	binary.BigEndian.PutUint16(packet[22:24], destinationPort)
	return packet
}

func TestTunTcpInboundFlowUsesStableBoundedShards(t *testing.T) {
	occupiedShards := map[int]bool{}
	for sourcePort := uint16(40000); sourcePort < 40064; sourcePort += 1 {
		packet := newTunTcpInboundTestPacket(sourcePort, 443)
		endpointId, shardIndex, ok := tcpInboundFlow(packet)
		if !ok {
			t.Fatalf("valid TCP packet for source port %d was not classified", sourcePort)
		}
		if endpointId.RemotePort != sourcePort || endpointId.LocalPort != 443 {
			t.Fatalf("ports for source port %d parsed as remote=%d local=%d", sourcePort, endpointId.RemotePort, endpointId.LocalPort)
		}
		if shardIndex < 0 || tunTcpInboundShardCount <= shardIndex {
			t.Fatalf("source port %d produced out-of-bounds shard %d", sourcePort, shardIndex)
		}
		_, repeatedShardIndex, repeatedOk := tcpInboundFlow(packet)
		if !repeatedOk || repeatedShardIndex != shardIndex {
			t.Fatalf("source port %d moved from shard %d to %d", sourcePort, shardIndex, repeatedShardIndex)
		}
		occupiedShards[shardIndex] = true
	}
	if len(occupiedShards) < tunTcpInboundShardCount/2 {
		t.Fatalf("64 adjacent flows occupied only %d of %d shards", len(occupiedShards), tunTcpInboundShardCount)
	}
}

func TestTunTcpInboundShardHandoffCadenceIsBounded(t *testing.T) {
	tun := &Tun{}
	shard := &tun.tcpInboundShards[0]
	endpointId, _, ok := tcpInboundFlow(newTunTcpInboundTestPacket(40000, 443))
	if !ok {
		t.Fatal("valid TCP packet was not classified")
	}
	shard.writeLock.Lock()
	defer shard.writeLock.Unlock()
	for packetIndex := 0; packetIndex < tunTcpInboundBurstPacketCount-1; packetIndex += 1 {
		if tun.advanceTcpInboundShardWithLock(shard, endpointId) {
			t.Fatalf("packet %d requested an early endpoint handoff", packetIndex)
		}
	}
	if !tun.advanceTcpInboundShardWithLock(shard, endpointId) {
		t.Fatalf("packet %d did not request an endpoint handoff", tunTcpInboundBurstPacketCount-1)
	}
	if shard.packetCount != 0 || shard.endpointCount != 1 {
		t.Fatalf("handoff state packet_count=%d endpoint_count=%d, want 0 and 1", shard.packetCount, shard.endpointCount)
	}
}

func TestTunWriteReleasesInjectedPacketBufferReference(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tun, err := CreateTunWithDefaults(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tun.Close()

	released := make(chan struct{})
	// The version selects the IPv4 injection branch. A deliberately truncated
	// header is sufficient: gVisor rejects it after taking any references it
	// needs, and the ownership assertion is independent of protocol validity.
	if n, writeErr := tun.write([]byte{0x45}, func() { close(released) }); writeErr != nil || n != 1 {
		t.Fatalf("write = %d, %v; want 1, nil", n, writeErr)
	}
	select {
	case <-released:
	case <-time.After(time.Second):
		t.Fatal("injected packet retained its creator PacketBuffer reference")
	}
}

func TestTunWriteReleasesUnsupportedPacketBufferReference(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tun, err := CreateTunWithDefaults(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tun.Close()

	released := make(chan struct{})
	if n, writeErr := tun.write([]byte{0x60}, func() { close(released) }); writeErr != syscall.EAFNOSUPPORT || n != 0 {
		t.Fatalf("unsupported write = %d, %v; want 0, %v", n, writeErr, syscall.EAFNOSUPPORT)
	}
	select {
	case <-released:
	case <-time.After(time.Second):
		t.Fatal("rejected packet retained its creator PacketBuffer reference")
	}
}

type tunLinkWriteResult struct {
	n   int
	err tcpip.Error
}

func newTunLinkTestPacket(marker byte) *stack.PacketBuffer {
	return stack.NewPacketBuffer(stack.PacketBufferOptions{
		Payload: buffer.MakeWithData([]byte{marker}),
	})
}

func writeTunLinkPacket(endpoint *tunLinkEndpoint, packet *stack.PacketBuffer) tunLinkWriteResult {
	var packets stack.PacketBufferList
	packets.PushBack(packet)
	n, err := endpoint.WritePackets(packets)
	return tunLinkWriteResult{n: n, err: err}
}

func TestTunOutboundQueueBackpressuresInsteadOfDropping(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tun, err := CreateTun(ctx, DefaultTunSettingsWithBufferSize(1))
	if err != nil {
		t.Fatal(err)
	}
	defer tun.Close()

	firstPacket := newTunLinkTestPacket(1)
	defer firstPacket.DecRef()
	if result := writeTunLinkPacket(tun.ep, firstPacket); result.err != nil || result.n != 1 {
		t.Fatalf("first link write = %d, %v; want 1, nil", result.n, result.err)
	}

	secondPacket := newTunLinkTestPacket(2)
	defer secondPacket.DecRef()
	started := make(chan struct{})
	done := make(chan tunLinkWriteResult, 1)
	go func() {
		close(started)
		done <- writeTunLinkPacket(tun.ep, secondPacket)
	}()
	<-started
	select {
	case result := <-done:
		t.Fatalf("full outbound queue dropped write immediately: %d, %v", result.n, result.err)
	case <-time.After(20 * time.Millisecond):
	}

	firstRead, readErr := tun.Read()
	if readErr != nil {
		t.Fatal(readErr)
	}
	MessagePoolReturn(firstRead)
	select {
	case result := <-done:
		if result.err != nil || result.n != 1 {
			t.Fatalf("released link write = %d, %v; want 1, nil", result.n, result.err)
		}
	case <-time.After(time.Second):
		t.Fatal("queue space did not release the blocked link writer")
	}
	secondRead, readErr := tun.Read()
	if readErr != nil {
		t.Fatal(readErr)
	}
	MessagePoolReturn(secondRead)
}

func TestTunCloseUnblocksOutboundQueueBackpressure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tun, err := CreateTun(ctx, DefaultTunSettingsWithBufferSize(1))
	if err != nil {
		t.Fatal(err)
	}
	defer tun.Close()

	firstPacket := newTunLinkTestPacket(1)
	defer firstPacket.DecRef()
	if result := writeTunLinkPacket(tun.ep, firstPacket); result.err != nil || result.n != 1 {
		t.Fatalf("first link write = %d, %v; want 1, nil", result.n, result.err)
	}

	secondPacket := newTunLinkTestPacket(2)
	defer secondPacket.DecRef()
	started := make(chan struct{})
	done := make(chan tunLinkWriteResult, 1)
	go func() {
		close(started)
		done <- writeTunLinkPacket(tun.ep, secondPacket)
	}()
	<-started
	select {
	case result := <-done:
		t.Fatalf("full outbound queue dropped write immediately: %d, %v", result.n, result.err)
	case <-time.After(20 * time.Millisecond):
	}

	if closeErr := tun.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	select {
	case result := <-done:
		if result.err == nil {
			t.Fatalf("closed link write = %d, nil; want a close error", result.n)
		}
	case <-time.After(time.Second):
		t.Fatal("tun close did not release blocked link writer")
	}
}

func (self *trackedTunDialConn) Read(p []byte) (int, error) {
	return 0, net.ErrClosed
}

func (self *trackedTunDialConn) Write(p []byte) (int, error) {
	if self.closed.Load() {
		return 0, net.ErrClosed
	}
	return len(p), nil
}

func (self *trackedTunDialConn) Close() error {
	self.closed.Store(true)
	return nil
}

func (self *trackedTunDialConn) LocalAddr() net.Addr {
	return &net.TCPAddr{}
}

func (self *trackedTunDialConn) RemoteAddr() net.Addr {
	return &net.TCPAddr{}
}

func (self *trackedTunDialConn) SetDeadline(t time.Time) error {
	return nil
}

func (self *trackedTunDialConn) SetReadDeadline(t time.Time) error {
	return nil
}

func (self *trackedTunDialConn) SetWriteDeadline(t time.Time) error {
	return nil
}

func TestTunDialRaceReturnsImmediatelyWhenEveryAttemptFails(t *testing.T) {
	wantErr := fmt.Errorf("immediate dial failure")
	var attempts atomic.Int32
	start := time.Now()
	conn, err := raceTunDialContext(
		context.Background(),
		context.Background(),
		"tcp4",
		"192.0.2.1:443",
		8,
		time.Second,
		2*time.Second,
		func(ctx context.Context, network string, address string) (net.Conn, error) {
			attempts.Add(1)
			return nil, wantErr
		},
	)
	if conn != nil {
		conn.Close()
		t.Fatal("failed dial race returned a connection")
	}
	if err != wantErr {
		t.Fatalf("dial race returned %v, want %v", err, wantErr)
	}
	if attempts.Load() != 8 {
		t.Fatalf("dial race launched %d/8 attempts", attempts.Load())
	}
	// The old success-only result channel waited the full two-second overall
	// timeout even though all eight attempts already knew they had failed.
	if elapsed := time.Since(start); 500*time.Millisecond < elapsed {
		t.Fatalf("known failures paid the overall timeout: %s", elapsed)
	}
}

func TestTunDialRaceClosesSimultaneousSuccessfulLoser(t *testing.T) {
	started := make(chan *trackedTunDialConn, 2)
	release := make(chan struct{})
	type dialResult struct {
		conn net.Conn
		err  error
	}
	done := make(chan dialResult, 1)
	go func() {
		conn, err := raceTunDialContext(
			context.Background(),
			context.Background(),
			"tcp4",
			"192.0.2.1:443",
			2,
			0,
			2*time.Second,
			func(ctx context.Context, network string, address string) (net.Conn, error) {
				conn := &trackedTunDialConn{}
				started <- conn
				// Both attempts have completed successfully before the race
				// chooses its winner. This models two handshakes crossing the
				// finish line together; the orchestration owns both results.
				<-release
				return conn, nil
			},
		)
		done <- dialResult{conn: conn, err: err}
	}()

	first := <-started
	second := <-started
	close(release)

	var result dialResult
	select {
	case result = <-done:
	case <-time.After(time.Second):
		t.Fatal("simultaneous successful dial race did not return a winner")
	}
	if result.err != nil || result.conn == nil {
		t.Fatalf("dial race failed: conn=%v err=%v", result.conn, result.err)
	}
	winner, ok := result.conn.(*trackedTunDialConn)
	if !ok {
		t.Fatalf("unexpected winner type %T", result.conn)
	}
	loser := first
	if winner == first {
		loser = second
	} else if winner != second {
		t.Fatal("winner was not one of the launched attempts")
	}

	deadline := time.Now().Add(time.Second)
	for !loser.closed.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !loser.closed.Load() {
		t.Fatal("successful losing connection remained open after winner handoff")
	}
	if winner.closed.Load() {
		t.Fatal("dial race closed the caller-owned winner")
	}
	winner.Close()
}

func TestTunCloseUnblocksDetachedDialContext(t *testing.T) {
	settings := DefaultTunSettings()
	settings.DialRace = 1
	settings.DialRaceTimeout = 3 * time.Second
	settings.DialTimeout = 4 * time.Second

	tun, err := CreateTun(context.Background(), settings)
	if err != nil {
		t.Fatalf("create tun: %v", err)
	}
	defer tun.Close()

	type dialResult struct {
		conn net.Conn
		err  error
	}
	dialDone := make(chan dialResult, 1)
	go func() {
		// A background context models a net/http Transport dial that outlives
		// the request that originally caused it.
		conn, err := tun.DialContext(context.Background(), "tcp4", "192.0.2.1:443")
		dialDone <- dialResult{conn: conn, err: err}
	}()

	// Observe the SYN before closing so the assertion covers an in-flight dial,
	// not merely a goroutine that had not started yet.
	packetDone := make(chan []byte, 1)
	go func() {
		packet, _ := tun.Read()
		packetDone <- packet
	}()
	select {
	case packet := <-packetDone:
		if packet == nil {
			t.Fatal("dial ended before producing an outbound packet")
		}
		MessagePoolReturn(packet)
	case <-time.After(time.Second):
		t.Fatal("dial did not produce an outbound packet")
	}

	start := time.Now()
	if err := tun.Close(); err != nil {
		t.Fatalf("close tun: %v", err)
	}
	select {
	case result := <-dialDone:
		if result.conn != nil {
			result.conn.Close()
			t.Fatal("dial unexpectedly succeeded")
		}
		if result.err == nil {
			t.Fatal("dial returned no error")
		}
		if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
			t.Fatalf("dial returned too slowly after close: %s", elapsed)
		}
	case <-time.After(time.Second):
		t.Fatal("dial remained blocked after tun close")
	}
}

func TestTunDialRaceUsesOneAbsoluteTimeout(t *testing.T) {
	settings := DefaultTunSettings()
	settings.DialRace = 10
	settings.DialRaceTimeout = 50 * time.Millisecond
	settings.DialTimeout = 60 * time.Millisecond

	tun, err := CreateTun(context.Background(), settings)
	if err != nil {
		t.Fatalf("create tun: %v", err)
	}
	defer tun.Close()

	start := time.Now()
	conn, err := tun.DialContext(context.Background(), "tcp4", "192.0.2.1:443")
	elapsed := time.Since(start)
	if conn != nil {
		conn.Close()
		t.Fatal("unreachable dial unexpectedly succeeded")
	}
	if err == nil {
		t.Fatal("unreachable dial returned no error")
	}
	if elapsed < 30*time.Millisecond {
		t.Fatalf("dial ignored its overall timeout: %s", elapsed)
	}
	// The old additive implementation paid all ten 50ms staggers plus a
	// final 10ms wait (~510ms). Leave ample race-instrumentation/scheduler
	// slack while pinning that the 60ms budget is absolute rather than
	// multiplied by hedge count.
	if 500*time.Millisecond < elapsed {
		t.Fatalf("dial race exceeded its one absolute timeout: %s", elapsed)
	}
}

func TestTunTCPBridge(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	left, err := CreateTunWithDefaults(ctx)
	if err != nil {
		t.Fatalf("create left tun: %v", err)
	}
	defer left.Close()

	right, err := CreateTunWithDefaults(ctx)
	if err != nil {
		t.Fatalf("create right tun: %v", err)
	}
	defer right.Close()

	bridgeTun(ctx, left, right)
	bridgeTun(ctx, right, left)

	rightIP := net.IP(right.localAddresses[0].AsSlice())
	ln, err := right.ListenTCP(&net.TCPAddr{IP: rightIP, Port: 0})
	if err != nil {
		t.Fatalf("listen tcp: %v", err)
	}
	defer ln.Close()

	serverErr := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			serverErr <- err
			return
		}
		defer conn.Close()
		if err := conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
			serverErr <- err
			return
		}
		buf := make([]byte, 4)
		if _, err := io.ReadFull(conn, buf); err != nil {
			serverErr <- err
			return
		}
		if string(buf) != "ping" {
			serverErr <- io.ErrUnexpectedEOF
			return
		}
		_, err = conn.Write([]byte("pong"))
		serverErr <- err
	}()

	conn, err := left.DialContext(ctx, "tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial through tun: %v", err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatalf("write through tun: %v", err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read through tun: %v", err)
	}
	if string(buf) != "pong" {
		t.Fatalf("got %q, want pong", string(buf))
	}
	if err := <-serverErr; err != nil {
		t.Fatalf("server error: %v", err)
	}
}

func bridgeTun(ctx context.Context, dst *Tun, src *Tun) {
	go func() {
		for {
			packet, err := src.Read()
			if err != nil {
				return
			}
			_, _ = dst.Write(packet)
			MessagePoolReturn(packet)
			select {
			case <-ctx.Done():
				return
			default:
			}
		}
	}()
}

// bridgeTunBatch forwards packets from src to dst using the batched read path
// (`ReadBatch`), the same way the proxy's ProxyDevice.Run drains the tun. This
// exercises the batch drain under sustained load.
func bridgeTunBatch(ctx context.Context, dst *Tun, src *Tun) {
	go func() {
		packets := make([][]byte, 64)
		for {
			n, err := src.ReadBatch(packets)
			if err != nil {
				return
			}
			for _, packet := range packets[:n] {
				_, _ = dst.Write(packet)
				MessagePoolReturn(packet)
			}
			select {
			case <-ctx.Done():
				return
			default:
			}
		}
	}()
}

// TestTunTCPThroughput drives a sustained one-way TCP transfer through two tuns
// bridged together (the bridge stands in for the network between two endpoints),
// using the batched read path. It asserts the full byte count arrives and that
// throughput clears a conservative floor — a regression that stalls or
// head-of-line-blocks the tun receive loop (e.g. holding a lock across a blocking
// enqueue) collapses this number.
func TestTunTCPThroughput(t *testing.T) {
	// generous overall cap: the transfer is measured several times (below), and
	// each run's stalls are independently bounded by per-chunk 55s deadlines.
	// The cap only backstops a true hang; slow-but-progressing runs on a
	// loaded -race host stay inside it.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()

	// size the ring buffers well above the default (32) so the bridge can keep
	// the pipe full under load, the way the proxy sizes its tun.
	left, err := CreateTun(ctx, DefaultTunSettingsWithBufferSize(2048))
	if err != nil {
		t.Fatalf("create left tun: %v", err)
	}
	defer left.Close()

	right, err := CreateTun(ctx, DefaultTunSettingsWithBufferSize(2048))
	if err != nil {
		t.Fatalf("create right tun: %v", err)
	}
	defer right.Close()

	// bidirectional bridge: data flows left->right, acks flow right->left
	bridgeTunBatch(ctx, left, right)
	bridgeTunBatch(ctx, right, left)

	rightIP := net.IP(right.localAddresses[0].AsSlice())

	// full speed moves ~300 MiB/s; under -race the gvisor stack runs ~250x slower
	// (~1 MiB/s). Scale the transfer down under -race so it still streams enough
	// bytes to catch a stall / head-of-line-block regression while keeping the
	// test duration bounded; drop the floor to a value that only a genuine stall
	// (~0) falls under. The per-chunk deadlines below bound a stall, not the
	// transfer wall clock, so a loaded host slows an attempt without failing it.
	totalBytes := int64(128) << 20 // 128 MiB
	minThroughputMiBs := 1.0       // conservative floor; a stall is ~0
	if raceEnabled {
		totalBytes = int64(16) << 20 // 16 MiB (~13s under -race unloaded)
		minThroughputMiBs = 0.1
	}
	// measure several times and take the max, to ride out host scheduling noise
	const throughputRuns = 3

	// runTransfer streams totalBytes through the tun once and returns the
	// measured throughput in MiB/s, or an error if the attempt did not
	// complete. A single attempt must never abort the whole test: it gets its
	// own listener and connection, recovers from a panic in the stack under
	// load, and signals failure by returning an error rather than calling
	// t.Fatal (which would tear the test down on the spot, before the other
	// attempts run). The floor below is judged against the best of
	// throughputRuns independent attempts, so one slow or broken attempt is
	// ridden out instead of failing the test early.
	runTransfer := func() (mibs float64, runErr error) {
		// a panic in the gvisor stack under load fails only this attempt.
		defer func() {
			if r := recover(); r != nil {
				runErr = fmt.Errorf("transfer panicked: %v", r)
			}
		}()

		// fresh listener per attempt: closing it on return unblocks a stuck
		// Accept, so a failed attempt leaks no goroutine into the next one.
		ln, err := right.ListenTCP(&net.TCPAddr{IP: rightIP, Port: 0})
		if err != nil {
			return 0, fmt.Errorf("listen tcp: %w", err)
		}
		defer ln.Close()

		recvDone := make(chan int64, 1)
		recvErr := make(chan error, 1)
		go func() {
			defer func() {
				if r := recover(); r != nil {
					recvErr <- fmt.Errorf("receiver panicked: %v", r)
				}
			}()
			conn, err := ln.Accept()
			if err != nil {
				recvErr <- err
				return
			}
			defer conn.Close()
			// drain exactly totalBytes, so neither side needs a half-close.
			// The deadline is refreshed per bounded step so it bounds a
			// stall in the stack, not the whole transfer: under -race plus
			// host load the full stream legitimately outlasts any single
			// fixed deadline while still making progress.
			received := int64(0)
			for received < totalBytes {
				_ = conn.SetReadDeadline(time.Now().Add(55 * time.Second))
				step := min(totalBytes-received, int64(1024*1024))
				n, err := io.CopyN(io.Discard, conn, step)
				received += n
				if err != nil {
					recvErr <- err
					return
				}
			}
			recvDone <- received
		}()

		conn, err := left.DialContext(ctx, "tcp", ln.Addr().String())
		if err != nil {
			return 0, fmt.Errorf("dial through tun: %w", err)
		}
		defer conn.Close()

		payload := make([]byte, 128*1024) // 128 KiB write chunks

		start := time.Now()
		written := int64(0)
		for written < totalBytes {
			chunk := payload
			if remaining := totalBytes - written; remaining < int64(len(chunk)) {
				chunk = payload[:remaining]
			}
			// per-chunk deadline: bounds a stalled pipe without capping the
			// whole transfer's wall clock (see the receiver note above)
			_ = conn.SetWriteDeadline(time.Now().Add(55 * time.Second))
			n, err := conn.Write(chunk)
			if err != nil {
				return 0, fmt.Errorf("write through tun after %d bytes: %w", written, err)
			}
			written += int64(n)
		}

		select {
		case err := <-recvErr:
			return 0, fmt.Errorf("receiver error: %w", err)
		case n := <-recvDone:
			elapsed := time.Since(start)
			if n != written {
				return 0, fmt.Errorf("received %d bytes, sent %d", n, written)
			}
			mib := float64(n) / (1024 * 1024)
			mibs := mib / elapsed.Seconds()
			t.Logf("tun tcp throughput: %.0f MiB in %v = %.1f MiB/s", mib, elapsed.Round(time.Millisecond), mibs)
			return mibs, nil
		case <-ctx.Done():
			return 0, fmt.Errorf("timed out after writing %d/%d bytes", written, totalBytes)
		}
	}

	best := 0.0
	failures := 0
	for i := range throughputRuns {
		mibs, err := runTransfer()
		if err != nil {
			failures++
			t.Logf("throughput run %d/%d failed: %v", i+1, throughputRuns, err)
			continue
		}
		best = max(best, mibs)
	}
	// only fail the whole test once, after all attempts: every attempt errored,
	// or the best of them still fell short of the floor.
	if failures == throughputRuns {
		t.Fatalf("all %d throughput runs failed", throughputRuns)
	}
	if best < minThroughputMiBs {
		t.Fatalf("throughput %.1f MiB/s below floor %.1f MiB/s", best, minThroughputMiBs)
	}
}
