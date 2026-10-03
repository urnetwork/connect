package connect

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// A completed warmup connection still owns its NAT state until a real
// terminal packet or socket event retires it. If its legitimate FIN cannot
// enter the provider, a subsequent connection cannot borrow that state's
// still-live reservation. This fixture never evicts a flow, shortens an idle
// timeout, releases a live owner early, or raises the shared cap.
//
// The old-flow FIN is an explicit condition, not an assertion about Android's
// HttpURLConnection: retained Android logs do not prove that it sent a FIN.
func TestNatProviderMemoryNewProbeAfterWarmupFin(t *testing.T) {
	for _, version := range []int{1, 2} {
		for _, headroom := range []ByteCount{7_000, 20_260} {
			for _, sendFin := range []bool{true, false} {
				t.Run(fmt.Sprintf("v%d/free-%d/fin-%t", version, headroom, sendFin), func(t *testing.T) {
					assertMessagePoolOwnership(t)
					synctest.Test(t, func(t *testing.T) {
						testNatProviderMemoryNewProbeAfterWarmup(t, version, headroom, sendFin)
					})
				})
			}
		}
	}
}

func testNatProviderMemoryNewProbeAfterWarmup(t *testing.T, version int, headroom ByteCount, sendFin bool) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	budget := NewTransferMemoryBudget(2_936_012)
	request := bytes.Repeat([]byte{0x5a}, 500)
	response := []byte("bounded-origin-response")
	type flow struct {
		path       *IpPath
		local      net.Conn
		origin     net.Conn
		peerAck    atomic.Uint32
		peerSeq    atomic.Uint32
		handshaken atomic.Bool
		originRead atomic.Int64
		returned   atomic.Int64
	}
	var flows [2]flow
	for i := range flows {
		f := &flows[i]
		f.path = &IpPath{Version: 4, Protocol: IpProtocolTcp,
			SourceIp: net.IPv4(10, 0, 0, 1).To4(), SourcePort: 40000 + i,
			DestinationIp: net.IPv4(203, 0, 113, byte(i+1)).To4(), DestinationPort: 443}
		f.local, f.origin = net.Pipe()
		defer f.local.Close()
		defer f.origin.Close()
		f.peerSeq.Store(101)
	}
	var dials atomic.Int64
	settings := DefaultLocalUserNatSettings()
	settings.MemoryBudget, settings.Log = budget, NewNoopLogger()
	settings.TcpBufferSettings.DialContextSettings = &DialContextSettings{
		DialContext: func(_ context.Context, _ string, address string) (net.Conn, error) {
			for i := range flows {
				if address == flows[i].path.DestinationHostPort() {
					dials.Add(1)
					return flows[i].local, nil
				}
			}
			return nil, fmt.Errorf("unexpected fake origin %s", address)
		},
	}
	nat, err := TryNewLocalUserNat(ctx, "warmup-fin", settings)
	if err != nil {
		t.Fatal(err)
	}
	defer nat.CloseAndWait(context.Background())
	clientSettings := DefaultClientSettings()
	clientSettings.Log = NewNoopLogger()
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), clientSettings)
	defer client.CloseAndWait(context.Background())
	providerSettings := DefaultRemoteUserNatProviderSettings()
	providerSettings.SecurityPolicyGenerator = DisableSecurityPolicyWithStats
	provider, err := TryNewRemoteUserNatProvider(client, nat, providerSettings)
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	provider.localUserNatUnsub()
	source, peer := SourceId(NewId()), Peer{ProvideMode: protocol.ProvideMode_Network}
	send := func(packet []byte) {
		defer MessagePoolReturn(packet)
		frame, err := ipPacketToProviderFrame(packet, version)
		if err != nil {
			t.Error(err)
			return
		}
		if !frame.Raw {
			defer MessagePoolReturn(frame.MessageBytes)
		}
		provider.ClientReceive(source, []*protocol.Frame{frame}, peer)
	}
	packet := func(f *flow, flags byte, seq uint32, payload []byte) []byte {
		state := ConnectionState{ipVersion: 4,
			sourceIp: f.path.DestinationIp, sourcePort: uint16(f.path.DestinationPort),
			destinationIp: f.path.SourceIp, destinationPort: uint16(f.path.SourcePort),
			sendSeq: f.peerAck.Load(), windowSize: 65535}
		return state.tcpPacket(flags, seq, payload)
	}
	var oldClosed atomic.Bool
	nat.addTcpFlowCloseCallback(func(_ TransferPath, path *IpPath) {
		if path.SourcePort == flows[0].path.SourcePort {
			oldClosed.Store(true)
		}
	})
	nat.AddReceivePacketCallback(func(_ TransferPath, _ protocol.ProvideMode, path *IpPath, data []byte) {
		f := &flows[path.SourcePort-40000]
		_, sourceIp, destinationIp, transport, ok := parseIpv4(data)
		var tcp parsedTcp
		if !ok || !parseTcpPacket(sourceIp, destinationIp, transport, &tcp) || (!tcp.syn && len(tcp.payload) == 0) {
			return
		}
		ack := tcp.seq + uint32(len(tcp.payload))
		if tcp.syn {
			ack++
		}
		f.peerAck.Store(ack)
		send(packet(f, tcpFlagAck, f.peerSeq.Load(), nil))
		if tcp.syn {
			f.handshaken.Store(true)
		} else if bytes.Equal(tcp.payload, response) {
			f.returned.Add(int64(len(tcp.payload)))
		}
	})
	var originWorkers sync.WaitGroup
	for i := range flows {
		f := &flows[i]
		originWorkers.Add(1)
		go func() {
			defer originWorkers.Done()
			read := make([]byte, len(request))
			n, err := io.ReadFull(f.origin, read)
			f.originRead.Add(int64(n))
			if err == nil {
				if !bytes.Equal(read, request) {
					t.Error("altered request reached fake origin")
					return
				}
				if _, err := f.origin.Write(response); err != nil && ctx.Err() == nil {
					t.Error(err)
				}
				// Keep the successful origin open: the source's FIN is the
				// only event that can retire warmup before the new probe.
				var extra [1]byte
				f.origin.Read(extra[:])
			}
		}()
	}
	defer func() {
		cancel()
		for i := range flows {
			flows[i].origin.Close()
		}
		originWorkers.Wait()
	}()
	old, next := &flows[0], &flows[1]
	send(packet(old, tcpFlagSyn, 100, nil))
	synctest.Wait()
	if !old.handshaken.Load() {
		t.Fatal("unpressured warmup SYN failed")
	}
	old.peerSeq.Store(101 + uint32(len(request)))
	send(packet(old, tcpFlagAck|tcpFlagPsh, 101, request))
	synctest.Wait()
	if old.returned.Load() != int64(len(response)) || old.originRead.Load() != int64(len(request)) || oldClosed.Load() {
		t.Fatal("warmup did not complete on a still-live TCP flow")
	}
	fill := budget.Available() - headroom
	if fill < 0 || !budget.TryReserve(fill) {
		t.Fatal("could not establish retained-owner pressure")
	}
	defer budget.Release(fill)
	if sendFin {
		send(packet(old, tcpFlagFin|tcpFlagAck, old.peerSeq.Load(), nil))
		synctest.Wait()
	}
	freedByFin := budget.Available() - headroom
	start := time.Now()
	deadline := start.Add(20 * time.Second)
	attempts := 0
	for time.Now().Before(deadline) && next.returned.Load() == 0 {
		attempts++
		if !next.handshaken.Load() {
			send(packet(next, tcpFlagSyn, 100, nil))
			synctest.Wait()
		}
		if next.handshaken.Load() {
			next.peerSeq.Store(101 + uint32(len(request)))
			send(packet(next, tcpFlagAck|tcpFlagPsh, 101, request))
			synctest.Wait()
		}
		if next.returned.Load() == 0 {
			time.Sleep(time.Second)
		}
	}
	succeeded := next.originRead.Load() == int64(len(request)) && next.returned.Load() == int64(len(response))
	t.Logf("headroom=%d fin=%t old_closed=%t retired_bytes=%d new_dials=%d new_origin_bytes=%d new_response_bytes=%d attempts=%d deadline=20s elapsed=%s success=%t cap=%d used=%d",
		headroom, sendFin, oldClosed.Load(), freedByFin, dials.Load()-1, next.originRead.Load(), next.returned.Load(), attempts,
		time.Since(start), succeeded, budget.TotalByteCount(), budget.UsedByteCount())
	if succeeded != sendFin || oldClosed.Load() != sendFin {
		t.Fatalf("new-flow success=%t old_closed=%t, want %t for real source FIN", succeeded, oldClosed.Load(), sendFin)
	}
	if sendFin && freedByFin < natTcpFlowMemoryByteCount(nat.settings.TcpBufferSettings) {
		t.Fatal("new flow was not financed by joined retirement of the old flow")
	}
	if budget.UsedByteCount() > budget.TotalByteCount() {
		t.Fatal("new flow overran the unchanged cap")
	}
}
