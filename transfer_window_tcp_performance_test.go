package connect

import (
	"context"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Borrow packet buffers, retaining shares only when their logical group is
// admitted. Production provider-return grouping lets SendSequence split a
// socket batch into carrier-safe wire Packs. Workload cancellation ends waiting.
func sendWindowTcpPackets(t *testing.T, ctx context.Context, client *Client, destination Id, messageType protocol.MessageType, packets [][]byte) {
	t.Helper()
	frames := make([]*protocol.Frame, 0, len(packets))
	for _, packet := range packets {
		frames = append(frames, &protocol.Frame{MessageType: messageType, MessageBytes: MessagePoolShareReadOnly(packet), Raw: true})
	}
	if success, _ := client.sendGroupWithTimeoutDetailed(frames, destination, nil, -1, Ctx(ctx)); !success {
		for _, frame := range frames {
			MessagePoolReturn(frame.MessageBytes)
		}
		if ctx.Err() == nil {
			t.Error("TCP fixture Transfer admission failed")
		}
	}
}

// Runs real provider TCP against a loopback kernel origin and a gVisor source
// socket through the same two Transfer clients and FIFO links as the packet
// matrix. Only dialing the synthetic destination is redirected to the owned
// origin. This adds both inner TCP windows, segmentation, ACK cadence, replay
// retention and the final TUN handoff to the measured delivery boundary.
func startWindowTcpWorkload(t *testing.T, ctx context.Context, provider, device *Client, counts []atomic.Int64, upload bool, natRefused *atomic.Int64, tcpBufferMax ByteCount) func() {
	t.Helper()
	settings := DefaultLocalUserNatSettings()
	settings.TcpBufferSettings.ReturnQueueBudget = NewTransferMemoryBudget(mib(48))
	return startWindowTcpWorkloadWithNatSettings(t, ctx, provider, device, counts, upload, natRefused, tcpBufferMax, settings)
}

// The caller supplies fresh, fixture-owned NAT settings. Only this workload
// owns their redirected dialer and replay pool until the returned join closes.
func startWindowTcpWorkloadWithNatSettings(t *testing.T, ctx context.Context, provider, device *Client, counts []atomic.Int64, upload bool, natRefused *atomic.Int64, tcpBufferMax ByteCount, settings *LocalUserNatSettings) func() {
	t.Helper()
	tunSettings := DefaultTunSettingsWithBufferSize(2048)
	if tcpBufferMax > 0 {
		// A separate capacity control can reproduce the TCP ceiling of a
		// larger process budget without changing global settings or the
		// Transfer budget. Its explicit per-connection maximum is in the ledger.
		if tcpBufferMax < ByteCount(max(tunSettings.TcpReceiveBuffer.Default, tunSettings.TcpSendBuffer.Default)) {
			t.Fatal("TCP maximum is below the default buffer size")
		}
		tunSettings.TcpReceiveBuffer.Max = int(tcpBufferMax)
		tunSettings.TcpSendBuffer.Max = int(tcpBufferMax)
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tun, err := CreateTun(ctx, tunSettings)
	if err != nil {
		listener.Close()
		t.Fatal(err)
	}
	settings.Log = NewNoopLogger()
	settings.TcpBufferSettings.EnableSyntheticSpeed = false
	settings.TcpBufferSettings.DialContextSettings = &DialContextSettings{DialContext: func(dialCtx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(dialCtx, "tcp4", listener.Addr().String())
	}}
	nat := NewLocalUserNat(ctx, "window-tcp-test", settings)
	var workers sync.WaitGroup
	var sockets sync.Map
	writePackets := func(client *Client, destination Id, messageType protocol.MessageType, packets [][]byte) {
		sendWindowTcpPackets(t, ctx, client, destination, messageType, packets)
	}
	nat.AddReceivePacketCallback(func(source TransferPath, _ protocol.ProvideMode, _ *IpPath, packet []byte) {
		writePackets(provider, source.SourceId, protocol.MessageType_IpIpPacketFromProvider, [][]byte{packet})
	})
	nat.AddReceivePacketsCallback(func(source TransferPath, _ protocol.ProvideMode, _ *IpPath, packets [][]byte) {
		writePackets(provider, source.SourceId, protocol.MessageType_IpIpPacketFromProvider, packets)
	})
	provider.AddReceiveCallback(func(source TransferPath, frames []*protocol.Frame, _ Peer) {
		packets := make([][]byte, 0, len(frames))
		for _, frame := range frames {
			if frame.MessageType != protocol.MessageType_IpIpPacketToProvider {
				continue
			}
			packets = append(packets, MessagePoolShareReadOnly(frame.MessageBytes))
		}
		// Preserve the delivered batch at the NAT boundary. Splitting it into
		// one channel admission per packet manufactures queue pressure absent
		// from the provider's batch path.
		if len(packets) != 0 && !nat.SendPackets(source, protocol.ProvideMode_Network, packets, 0) {
			for _, packet := range packets {
				MessagePoolReturn(packet)
			}
			if ctx.Err() == nil {
				natRefused.Add(int64(len(packets)))
			}
		}
	})
	device.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, _ Peer) {
		packets := make([][]byte, 0, len(frames))
		for _, frame := range frames {
			if frame.MessageType == protocol.MessageType_IpIpPacketFromProvider {
				packets = append(packets, frame.MessageBytes)
			}
		}
		if _, err := tun.WriteBatch(packets); err != nil && ctx.Err() == nil {
			t.Errorf("TCP fixture TUN injection: %v", err)
		}
	})
	workers.Go(func() {
		packets := make([][]byte, 64)
		for ctx.Err() == nil {
			n, err := tun.ReadBatch(packets)
			if n != 0 {
				writePackets(device, provider.ClientId(), protocol.MessageType_IpIpPacketToProvider, packets[:n])
				for i := range n {
					MessagePoolReturn(packets[i])
					packets[i] = nil
				}
			}
			if err != nil {
				return
			}
		}
	})
	workers.Go(func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			if ctx.Err() != nil {
				conn.Close()
				return
			}
			sockets.Store(conn, true)
			workers.Go(func() {
				defer conn.Close()
				defer sockets.Delete(conn)
				// The one-byte request identifies the flow for common-interval
				// upload accounting; all measured payload is generated locally.
				var flow [1]byte
				if _, err := io.ReadFull(conn, flow[:]); err != nil {
					return
				}
				buffer := make([]byte, 64*1024)
				for ctx.Err() == nil {
					if upload {
						n, err := conn.Read(buffer)
						counts[int(flow[0])].Add(int64(n))
						if err != nil {
							return
						}
					} else if _, err := conn.Write(buffer); err != nil {
						return
					}
				}
			})
		}
	})
	for flow := range counts {
		workers.Go(func() {
			conn, err := tun.DialContext(ctx, "tcp", "198.51.100.2:443")
			if err != nil {
				if ctx.Err() == nil {
					t.Errorf("TCP fixture dial: %v", err)
				}
				return
			}
			sockets.Store(conn, true)
			defer conn.Close()
			defer sockets.Delete(conn)
			if _, err := conn.Write([]byte{byte(flow)}); err != nil {
				return
			}
			buffer := make([]byte, 64*1024)
			for ctx.Err() == nil {
				if upload {
					if _, err := conn.Write(buffer); err != nil {
						return
					}
				} else {
					n, err := conn.Read(buffer)
					counts[flow].Add(int64(n))
					if err != nil {
						return
					}
				}
			}
		})
	}
	return func() {
		listener.Close()
		sockets.Range(func(key, _ any) bool { key.(net.Conn).Close(); return true })
		nat.Close()
		tun.Close()
		workers.Wait()
		joinCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := nat.CloseAndWait(joinCtx); err != nil {
			t.Errorf("TCP fixture NAT cleanup: %v", err)
		}
		if budget := settings.TcpBufferSettings.ReturnQueueBudget; budget != nil {
			reserved, released := budget.Counts()
			if budget.UsedByteCount() != 0 || reserved != released {
				t.Errorf("TCP fixture replay budget did not balance: used=%d reserved=%d released=%d", budget.UsedByteCount(), reserved, released)
			}
		}
	}
}

func TestWindowTcpDownloadPerformanceMatrix(t *testing.T) {
	if os.Getenv("CONNECT_WINDOW_TCP_MEASURE") == "" {
		t.Skip("set CONNECT_WINDOW_TCP_MEASURE=1 for the TCP download matrix")
	}
	testWindowPathPerformanceMatrix(t, true, false)
}

func TestWindowTcpUploadPerformanceMatrix(t *testing.T) {
	if os.Getenv("CONNECT_WINDOW_TCP_MEASURE") == "" {
		t.Skip("set CONNECT_WINDOW_TCP_MEASURE=1 for the TCP upload matrix")
	}
	testWindowPathPerformanceMatrix(t, true, true)
}
