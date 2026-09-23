// Forced lock/admission schedules cover duplex injection independently of host
// throughput. Established TCP finite tails separately validate stack delivery.
package connect

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/ports"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/waiter"
)

// The held endpoint cannot unlock until the one-slot outbound queue drains.
// Its drainer uses the real SendSequence admission gate, which cannot open
// until the inbound callback returns and the next carrier ACK can be consumed.
// The ACK-only rows are the existing bypass control; payload+ACK must also
// break this cycle without losing an already-read reliable Transfer payload.
func TestTunDuplexDataHandoffDoesNotCycleThroughAdmission(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, batch := range []bool{false, true} {
		for _, userOwned := range []bool{false, true} {
			for _, payload := range []bool{false, true} {
				checkTunDuplexOwnedAdmission(t, batch, userOwned, payload)
			}
		}
	}
}

// Each row owns its endpoint, bounded queues, and workers through cancellation.
func checkTunDuplexOwnedAdmission(t *testing.T, batch, userOwned, payload bool) {
	t.Helper()
	name := fmt.Sprintf("batch=%t user=%t payload=%t", batch, userOwned, payload)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings := DefaultTunSettingsWithBufferSize(1)
	// Suppress the NIC's loss escape while forcing the cycle. Production's
	// finite timeout otherwise hides it by dropping one packet every 250 ms.
	settings.OutboundQueueWaitTimeout = 0
	tun, err := CreateTun(ctx, settings)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	packet := newTunTcpInboundTestPacket(40000, 443)
	packet[32], packet[33] = 5<<4, tcpFlagAck
	if payload {
		packet = append(packet, 0x2a)
	}
	binary.BigEndian.PutUint16(packet[2:4], uint16(len(packet)))
	id, _, _ := tcpInboundFlow(packet)
	var waitQueue waiter.Queue
	raw, tcpErr := tun.stack.NewEndpoint(tcp.ProtocolNumber, ipv4.ProtocolNumber, &waitQueue)
	if tcpErr != nil {
		cancel()
		tun.Close()
		tun.stack.Wait()
		t.Fatal(tcpErr)
	}
	endpoint := raw.(*tcp.Endpoint)
	protocols := []tcpip.NetworkProtocolNumber{ipv4.ProtocolNumber}
	if tcpErr = tun.stack.RegisterTransportEndpoint(protocols, tcp.ProtocolNumber, id, endpoint, ports.Flags{}, tun.nicId); tcpErr != nil {
		cancel()
		endpoint.Close()
		tun.Close()
		tun.stack.Wait()
		t.Fatal(tcpErr)
	}
	var workers sync.WaitGroup
	defer func() {
		cancel()
		workers.Wait()
		tun.stack.UnregisterTransportEndpoint(protocols, tcp.ProtocolNumber, id, endpoint, ports.Flags{}, tun.nicId)
		endpoint.Close()
		tun.Close()
		tun.stack.Wait()
	}()
	filler := newTunLinkTestPacket(1)
	result := writeTunLinkPacket(tun.ep, filler)
	filler.DecRef()
	if result.n != 1 || result.err != nil {
		t.Fatal("one-slot outbound fixture failed to fill")
	}
	admission := newSendPackAdmission(1)
	if acquired, _, _ := admission.tryAcquire(sendSchedulingKey{}); !acquired {
		t.Fatal("admission fixture failed to fill")
	}
	sequence := &SendSequence{ctx: ctx, packAdmission: admission}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { admission.release(sendSchedulingKey{}) }) }
	drained := make(chan error, 1)
	workers.Go(func() {
		pack := &SendPack{Ctx: ctx}
		acquired, err, _ := sequence.acquirePackAdmission(pack, -1)
		if !acquired {
			drained <- err
			return
		}
		defer admission.release(sendSchedulingKey{})
		bytes, err := tun.Read()
		MessagePoolReturn(bytes)
		drained <- err
	})
	ownerLocked := make(chan struct{})
	ownerDone := make(chan tunLinkWriteResult, 1)
	workers.Go(func() {
		if userOwned {
			endpoint.LockUser()
			defer endpoint.UnlockUser()
		} else {
			endpoint.StopWork()
			defer endpoint.ResumeWork()
		}
		close(ownerLocked)
		outgoing := newTunLinkTestPacket(2)
		defer outgoing.DecRef()
		ownerDone <- writeTunLinkPacket(tun.ep, outgoing)
	})
	<-ownerLocked
	delivered := make(chan error, 1)
	workers.Go(func() {
		var err error
		if batch {
			_, err = tun.WriteBatch([][]byte{packet, packet})
		} else {
			_, err = tun.Write(packet)
		}
		release()
		delivered <- err
	})
	blocked := false
	select {
	case err := <-delivered:
		if err != nil {
			t.Errorf("%s injection: %v", name, err)
		}
	case <-time.After(time.Second):
		blocked = true
	}
	// Always release/join the failing schedule; this deadline is only an
	// assertion boundary because the held queue/credit proves the dependency.
	release()
	if err := <-drained; err != nil {
		t.Errorf("%s drainer: %v", name, err)
	}
	if result := <-ownerDone; result.n != 1 || result.err != nil {
		t.Errorf("%s owner=%+v", name, result)
	}
	if blocked {
		if err := <-delivered; err != nil {
			t.Error(err)
		}
	}
	if tun.OutboundDropCount() != 0 {
		t.Errorf("%s required an outbound drop", name)
	}
	if blocked {
		t.Errorf("%s: inbound data waited for outbound admission while that admission awaited the carrier ACK behind this callback", name)
	}
}

// Capture the only server payload at the wire boundary. No retransmission or
// subsequent packet is injected after the finite tail under test.
func TestTunFiniteTcpTailProgressesAfterEndpointOwnerReleases(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, batch := range []bool{false, true} {
		for _, userOwned := range []bool{false, true} {
			checkTunFiniteOwnedTail(t, batch, userOwned)
		}
	}
}

// Both endpoint ownership paths use the same real gVisor connection and bytes.
func checkTunFiniteOwnedTail(t *testing.T, batch, userOwned bool) {
	t.Helper()
	name := fmt.Sprintf("batch=%t user=%t", batch, userOwned)
	ctx, cancel := context.WithCancel(context.Background())
	left, err := CreateTun(ctx, DefaultTunSettingsWithBufferSize(64))
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	right, err := CreateTun(ctx, DefaultTunSettingsWithBufferSize(64))
	if err != nil {
		cancel()
		left.Close()
		left.stack.Wait()
		t.Fatal(err)
	}
	listener, err := right.ListenTCP(&net.TCPAddr{IP: net.IP(right.LocalAddresses()[0].AsSlice()), Port: 0})
	if err != nil {
		cancel()
		left.Close()
		right.Close()
		left.stack.Wait()
		right.stack.Wait()
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	payloadPackets := make(chan []byte, 16)
	ackNumbers := make(chan uint32, 32)
	workers.Go(func() {
		for ctx.Err() == nil {
			packet, err := left.Read()
			if err != nil {
				return
			}
			if len(packet) >= 40 && packet[0]>>4 == 4 && packet[9] == 6 {
				offset := int(packet[0]&15) * 4
				if offset+20 <= len(packet) && packet[offset+13]&tcpFlagAck != 0 {
					select {
					case ackNumbers <- binary.BigEndian.Uint32(packet[offset+8 : offset+12]):
					default:
					}
				}
			}
			_, err = right.Write(packet)
			MessagePoolReturn(packet)
			if err != nil {
				return
			}
		}
	})
	workers.Go(func() {
		for ctx.Err() == nil {
			packet, err := right.Read()
			if err != nil {
				return
			}
			var path IpPath
			payload, parseErr := parseIpPathWithPayloadBorrowed(packet, &path)
			if parseErr == nil && path.Protocol == IpProtocolTcp && len(payload) > 0 {
				select {
				case payloadPackets <- packet:
				case <-ctx.Done():
					MessagePoolReturn(packet)
					return
				}
			} else {
				_, err = left.Write(packet)
				MessagePoolReturn(packet)
				if err != nil {
					return
				}
			}
		}
	})
	accepted := make(chan net.Conn, 1)
	workers.Go(func() {
		conn, err := listener.Accept()
		if err == nil {
			select {
			case accepted <- conn:
			case <-ctx.Done():
				conn.Close()
			}
		}
	})
	var conn, peer net.Conn
	var packets [][]byte
	defer func() {
		cancel()
		if conn != nil {
			conn.Close()
		}
		if peer != nil {
			peer.Close()
		}
		listener.Close()
		left.Close()
		right.Close()
		workers.Wait()
		select {
		case acceptedConn := <-accepted:
			acceptedConn.Close()
		default:
		}
		left.stack.Wait()
		right.stack.Wait()
		for _, packet := range packets {
			MessagePoolReturn(packet)
		}
		for {
			select {
			case packet := <-payloadPackets:
				MessagePoolReturn(packet)
			default:
				return
			}
		}
	}()
	conn, err = left.DialContext(ctx, "tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	select {
	case peer = <-accepted:
	case <-time.After(5 * time.Second):
		t.Fatal("established peer not accepted")
	}
	endpoint := conn.(*TunTcpConn).endpoint.(*tcp.Endpoint)
	if userOwned {
		endpoint.LockUser()
	} else {
		endpoint.StopWork()
	}
	unlocked := false
	unlock := func() {
		if !unlocked {
			unlocked = true
			if userOwned {
				endpoint.UnlockUser()
			} else {
				endpoint.ResumeWork()
			}
		}
	}
	defer unlock()
	payload := bytes.Repeat([]byte{0x3a}, 2000)
	if _, err := peer.Write(payload); err != nil {
		t.Fatal(err)
	}
	total := 0
	for total < len(payload) {
		select {
		case packet := <-payloadPackets:
			packets = append(packets, packet)
			var path IpPath
			body, err := parseIpPathWithPayloadBorrowed(packet, &path)
			if err != nil {
				t.Fatal(err)
			}
			total += len(body)
		case <-time.After(5 * time.Second):
			t.Fatal("finite payload did not reach owned wire boundary")
		}
	}
	if len(packets) < 2 {
		t.Fatal("finite batch did not contain two TCP segments")
	}
	injected := make(chan error, 1)
	workers.Go(func() {
		var err error
		if batch {
			_, err = left.WriteBatch(packets)
		} else {
			for _, packet := range packets {
				if _, err = left.Write(packet); err != nil {
					break
				}
			}
		}
		injected <- err
	})
	blocked := false
	select {
	case err := <-injected:
		if err != nil {
			t.Error(err)
		}
	case <-time.After(time.Second):
		blocked = true
	}
	unlock()
	if blocked {
		if err := <-injected; err != nil {
			t.Error(err)
		}
	}
	// Observe TCP's cumulative acknowledgement before Read can itself take
	// and unlock the endpoint. This proves native finite-tail progress, not
	// progress accidentally kicked by the application's checking syscall.
	offset := int(packets[0][0]&15) * 4
	wantAck := binary.BigEndian.Uint32(packets[0][offset+4:offset+8]) + uint32(len(payload))
	ackDeadline := time.NewTimer(5 * time.Second)
	defer ackDeadline.Stop()
acknowledged:
	for {
		select {
		case ack := <-ackNumbers:
			if ack == wantAck {
				break acknowledged
			}
		case <-ackDeadline.C:
			t.Fatalf("%s finite tail emitted no cumulative ACK without a later packet or Read", name)
		}
	}
	if endpoint.Readiness(waiter.ReadableEvents)&waiter.ReadableEvents == 0 {
		t.Fatalf("%s acknowledged tail is not readable", name)
	}
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	received := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, received); err != nil {
		t.Fatalf("%s finite tail needed another packet: %v", name, err)
	}
	if !bytes.Equal(received, payload) {
		t.Fatalf("%s finite tail changed bytes", name)
	}
	if left.Stats().DroppedPackets.Value() != 0 || left.OutboundDropCount() != 0 {
		t.Errorf("%s dropped finite tail", name)
	}
	if blocked {
		t.Errorf("%s: data injection waited for endpoint owner before returning", name)
	}
}
