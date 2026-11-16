package connect

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

func nativeProtocolBurstInput(count int) ([][]byte, [][]byte) {
	packets, expected := make([][]byte, count), make([][]byte, count)
	for i := range packets {
		path := &IpPath{Version: 4, Protocol: IpProtocolTcp, SourceIp: net.ParseIP("192.0.2.10"), DestinationIp: net.ParseIP("198.51.100.20"), SourcePort: 41000 + i, DestinationPort: 443}
		expected[i] = ipOosTcpPacketSequence(path, tcpFlagAck, uint32(100+i), nil)
		packets[i] = MessagePoolCopy(expected[i])
	}
	return packets, expected
}

func nativeProtocolBurstDrain(t *testing.T, nat *LocalUserNat, expected [][]byte, want int) {
	t.Helper()
	count := 0
	for len(nat.sendPackets) > 0 {
		queued := <-nat.sendPackets
		if len(queued.packets) > providerReturnBatchMaxFrames {
			t.Error("protocol owner exceeded its unchanged frame bound")
		}
		for _, packet := range queued.packets {
			if count >= len(expected) || !bytes.Equal(packet, expected[count]) {
				t.Error("accepted protocol prefix changed order or bytes")
			}
			count++
			MessagePoolReturn(packet)
		}
		queued.finish()
	}
	if count != want {
		t.Errorf("queued owned packet count=%d want=%d", count, want)
	}
}

// The borrowing API still refuses oversized protocol groups all-or-nothing,
// even with an infinite queue wait and no DATA byte budget. The consuming API
// instead secures bounded owners for an ordinary native burst of 17 or 64.
func TestLocalNatConsumingProtocolBurstUsesBoundedOwners(t *testing.T) {
	assertMessagePoolOwnership(t)
	var causes []string
	for _, count := range []int{17, 64} {
		for _, fullBudget := range []bool{false, true} {
			for _, timeout := range []time.Duration{0, -1} {
				ctx, cancel := context.WithCancel(context.Background())
				var budget *TransferMemoryBudget
				if fullBudget {
					budget = NewTransferMemoryBudget(1)
					if !budget.TryReserve(1) {
						t.Fatal("could not fill DATA budget")
					}
				}
				nat := &LocalUserNat{ctx: ctx, cancel: cancel, settings: &LocalUserNatSettings{MemoryBudget: budget}, sendPackets: make(chan *SendPacket, 4)}
				packets, expected := nativeProtocolBurstInput(count)
				if nat.SendPackets(SourceId(NewId()), protocol.ProvideMode_Network, packets, timeout) || len(nat.sendPackets) != 0 {
					t.Fatal("borrowing API lost oversized all-or-nothing refusal")
				}
				for i, packet := range packets {
					if !bytes.Equal(packet, expected[i]) {
						t.Fatal("borrowing refusal consumed a caller root")
					}
				}
				accepted := nat.SendPacketBatch(SourceId(NewId()), protocol.ProvideMode_Network, packets, timeout)
				clear(packets) // outer list is borrowed; accepted queue lists are independent.
				if accepted != count {
					causes = append(causes, fmt.Sprintf("count=%d full=%t timeout=%d accepted=%d", count, fullBudget, timeout, accepted))
				}
				nativeProtocolBurstDrain(t, nat, expected, accepted)
				cancel()
				if budget != nil {
					if budget.UsedByteCount() != 1 {
						t.Error("protocol burst charged DATA bytes")
					}
					budget.Release(1)
				}
			}
		}
	}
	t.Log("[protocol-burst-cleanup] all input roots consumed or returned; all queued prefixes released")
	if len(causes) != 0 {
		t.Errorf("CONSUMING_PROTOCOL_BURST_REJECTED_BEFORE_BOUNDED_CHUNKING: %s", strings.Join(causes, "; "))
	}
}

func TestLocalNatConsumingProtocolBurstLaterRefusalAndCancellation(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, canceled := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		nat := &LocalUserNat{ctx: ctx, cancel: cancel, settings: &LocalUserNatSettings{}, sendPackets: make(chan *SendPacket, 1)}
		if canceled {
			nat.Close()
		}
		packets, expected := nativeProtocolBurstInput(64)
		accepted := nat.SendPacketBatch(SourceId(NewId()), protocol.ProvideMode_Network, packets, 0)
		want := providerReturnBatchMaxFrames
		if canceled {
			want = 0
		}
		// A failed second chunk must not return the first queue's live roots.
		nativeProtocolBurstDrain(t, nat, expected, accepted)
		cancel()
		if accepted != want {
			t.Errorf("canceled=%t accepted-prefix=%d want=%d", canceled, accepted, want)
		}
	}
	// Mixed groups are still DATA and keep their existing single admission.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	budget := NewTransferMemoryBudget(1)
	if !budget.TryReserve(1) {
		t.Fatal("full mixed DATA premise")
	}
	defer budget.Release(1)
	nat := &LocalUserNat{ctx: ctx, cancel: cancel, settings: &LocalUserNatSettings{MemoryBudget: budget}, sendPackets: make(chan *SendPacket, 4)}
	packets, _ := nativeProtocolBurstInput(17)
	MessagePoolReturn(packets[16])
	packets[16] = MessagePoolCopy(ipProtocolOfferPacket(tcpFlagAck, []byte("DATA")))
	if accepted := nat.SendPacketBatch(SourceId(NewId()), protocol.ProvideMode_Network, packets, 0); accepted != 0 || len(nat.sendPackets) != 0 || budget.UsedByteCount() != 1 {
		t.Fatal("mixed burst bypassed DATA admission or partially entered")
	}
}

// A real preloaded TUN burst hits the exact public count boundary before an
// unrelated real loopback TCP connection starts. A shared pump must remain
// alive even if a native read cannot be admitted as one borrowing batch.
func TestIpEgressProtocolBurstKeepsSharedPumpAlive(t *testing.T) {
	assertMessagePoolOwnership(t)
	ctx, cancel := context.WithCancel(context.Background())
	tun, err := CreateTunWithDefaults(ctx)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	settings := DefaultLocalUserNatSettings()
	settings.SendShardCount = 4
	nat := NewLocalUserNat(ctx, "protocol burst pump", settings)
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		cancel()
		tun.Close()
		nat.CloseAndWait(context.Background())
		t.Fatal(err)
	}
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = io.Copy(conn, conn)
	}()
	remove := func() {}
	var bridgeDone <-chan struct{}
	var conn net.Conn
	clean := func() {
		if conn != nil {
			conn.Close()
		}
		listener.Close()
		cancel()
		tun.Close()
		join, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if err := nat.CloseAndWait(join); err != nil {
			t.Error(err)
		}
		remove()
		for _, done := range []<-chan struct{}{bridgeDone, serverDone} {
			if done == nil {
				continue
			}
			select {
			case <-done:
			case <-join.Done():
				t.Error("protocol-burst fixture did not join")
			}
		}
		tun.stack.Wait()
	}
	defer func() { clean() }()
	packets, _ := nativeProtocolBurstInput(17)
	var list stack.PacketBufferList
	var creators []*stack.PacketBuffer
	for _, packet := range packets {
		pk := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(packet)})
		creators = append(creators, pk)
		list.PushBack(pk)
		MessagePoolReturn(packet)
	}
	queued, queueErr := tun.ep.WritePackets(list)
	for _, pk := range creators {
		pk.DecRef()
	}
	if queueErr != nil || queued != 17 {
		t.Fatalf("preloaded actual TUN queue=%d error=%v", queued, queueErr)
	}
	remove, bridgeDone = bridgeTunToLocalUserNatWithLifecycle(tun, nat, SourceId(NewId()), 64)
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for tun.ep.NumQueued() != 0 {
		select {
		case <-tick.C:
		case <-deadline.C:
			t.Fatal("shared reader did not take the preloaded burst")
		}
	}
	deadline.Stop()
	tick.Stop()
	flowCtx, flowCancel := context.WithTimeout(ctx, 2*time.Second)
	defer flowCancel()
	conn, err = tun.DialContext(flowCtx, "tcp", listener.Addr().String())
	if err == nil {
		payload := []byte("unrelated flow remains alive after protocol burst")
		err = writeConnPhaseWithDeadline(flowCtx, conn, payload, 2*time.Second)
		if err == nil {
			echo := make([]byte, len(payload))
			_, err = readFullWithProgressDeadline(flowCtx, conn, echo, 2*time.Second)
			if err == nil && !bytes.Equal(echo, payload) {
				err = fmt.Errorf("unrelated echo bytes changed")
			}
		}
	}
	// The old helper's terminal refusal is reported only after all actors join.
	clean()
	clean = func() {}
	t.Log("[protocol-pump-cleanup] NAT, bridge, echo socket and stack joined")
	if err != nil {
		t.Errorf("TUN_SHARED_PUMP_STOPPED_AFTER_PROTOCOL_BURST: %v", err)
	}
}

// Reliable provider ingress already enters one frame at a time under a common
// receipt. A17-frame original must secure all17 controls before its one ACK;
// the public borrowing-batch bound must never become a permanent retry gate.
func TestProviderReliableSeventeenControlFramesSecureOneReceipt(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newReliableTcpIngressFixture(t, 4, nil)
		f.establishForControl()
		sequence := NewReceiveSequence(f.ctx, f.client, f.source, NewId(), sequenceTlsRoleServer, false, DefaultReceiveBufferSettings())
		messageID := NewId()
		var processed atomic.Int64
		var earlyAck atomic.Bool
		previous := f.nat.afterSendPacketForTest
		f.nat.afterSendPacketForTest = func() {
			count := processed.Add(1)
			if count < 17 && sequence.ackWindow.Snapshot(false).ackUpdateCount != 0 {
				earlyAck.Store(true)
			}
			previous()
		}
		frames := make([]*protocol.Frame, 17)
		for i := range frames {
			packet := MessagePoolCopy(ipOosTcpPacketSequence(f.path, tcpFlagAck, 101, nil))
			frames[i] = &protocol.Frame{MessageType: protocol.MessageType_IpIpPacketToProvider, Raw: true, MessageBytes: packet}
		}
		sequence.deliverItems = []*receiveItem{{transferItem: transferItem{messageId: messageID, sequenceNumber: 0}, receiveCallback: f.client.receiveCallback, ack: true, noBudget: true, frames: frames}}
		// flushDeliver clears its dispatch slice. Keep the item's owning frame
		// headers independent, as receiveHead does when it appends appFrames.
		sequence.deliverFrames = append([]*protocol.Frame(nil), frames...)
		sequence.deliverPeer = Peer{ProvideMode: protocol.ProvideMode_Public}
		go runReliableIngressFixtureDelivery(sequence)
		synctest.Wait()
		ack := sequence.ackWindow.Snapshot(false)
		if processed.Load() != 17 || earlyAck.Load() || len(f.processed) != 17 || ack.ackUpdateCount != 1 || ack.headAck.messageId != messageID || len(f.tcp.sendItems) != 1 || sequence.deliveryQueue == nil || sequence.deliveryQueue.pending() {
			t.Fatalf("17 control frames did not secure one exact receipt: processed=%d ack=%d finalQueue=%d", len(f.processed), ack.ackUpdateCount, len(f.tcp.sendItems))
		}
		if f.dials.Load() != 0 {
			t.Fatal("control receipt waited for an upstream socket")
		}
		for index, frame := range frames {
			if frame == nil || frame.MessageBytes != nil {
				t.Fatalf("receipt did not return original frame %d exactly once", index)
			}
		}
		t.Log("17 real per-frame NAT admissions, one shared Transfer receipt ACK, initial queued SYN unchanged")
	})
}
