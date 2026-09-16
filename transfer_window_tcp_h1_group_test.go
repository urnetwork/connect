// The host TCP workload must use the same bounded wire grouping as the real
// provider. A large socket batch is logical work, not one oversized H1 frame.
package connect

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gorilla/websocket"
	"github.com/urnetwork/connect/protocol"
	"google.golang.org/protobuf/proto"
)

// Force an indivisible 17,600-byte socket batch through the actual H1 writer.
// The server's production 8-KiB read cap rejects SendMulti's single raw Pack;
// production logical grouping must deliver every packet through bounded Packs.
func TestWindowTcpSocketBatchFitsPhysicalH1(t *testing.T) {
	assertMessagePoolOwnership(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings := DefaultClientSettings()
	settings.Log = NewNoopLogger()
	settings.EncryptionSettings.Mode = EncryptionModeOff
	settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
	settings.SendBufferSettings.WindowSizing = WindowSizingConstant
	settings.SendBufferSettings.ApplyWindowSizing()
	carrier := DefaultPlatformTransportSettingsWithMemoryTarget(24 * 1024 * 1024)
	carrier.Log = NewNoopLogger()
	carrier.PingTimeout = 30 * time.Second
	if carrier.H1MaxMessageByteCount != 8192 {
		t.Fatalf("production H1 message cap changed: %d", carrier.H1MaxMessageByteCount)
	}
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
	destination := NewId()
	client.ContractManager().AddNoContractPeer(destination)
	type result struct {
		packets, messages, maxMessageBytes int
		err                                error
	}
	completed := make(chan result, 1)
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := newTestingLoopbackHttpServer(t, 4, http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		ws, err := upgrader.Upgrade(w, request, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		closed := make(chan struct{})
		watchDone := make(chan struct{})
		go func() {
			defer close(watchDone)
			select {
			case <-ctx.Done():
				ws.Close()
			case <-closed:
			}
		}()
		defer func() { close(closed); <-watchDone }()
		ws.SetReadLimit(carrier.H1MaxMessageByteCount)
		reading := result{}
		var seen [16]bool
		for reading.packets < len(seen) {
			kind, message, err := ws.ReadMessage()
			if err != nil {
				reading.err = err
				break
			}
			if len(message) == 0 {
				continue
			}
			frame := &protocol.TransferFrame{}
			if kind != websocket.BinaryMessage {
				reading.err = fmt.Errorf("message kind %d", kind)
				break
			}
			if err := proto.Unmarshal(message, frame); err != nil {
				reading.err = err
				break
			}
			pack := frame.GetPack()
			if pack == nil {
				reading.err = fmt.Errorf("expected data Pack")
				break
			}
			reading.messages++
			reading.maxMessageBytes = max(reading.maxMessageBytes, len(message))
			for _, payload := range pack.Frames {
				if len(payload.MessageBytes) != 1100 {
					reading.err = fmt.Errorf("packet bytes %d", len(payload.MessageBytes))
					break
				}
				index := int(payload.MessageBytes[0])
				if len(seen) <= index || seen[index] {
					reading.err = fmt.Errorf("duplicate or invalid packet %d", index)
					break
				}
				seen[index] = true
				reading.packets++
			}
			if reading.err != nil {
				break
			}
		}
		completed <- reading
		<-ctx.Done()
	}), true)
	strategySettings := DefaultClientStrategySettings()
	strategySettings.Log = NewNoopLogger()
	strategySettings.EnableResilient = false
	strategySettings.TlsConfig = server.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	strategy := NewClientStrategy(ctx, strategySettings)
	transport := NewPlatformTransportWithTargetMode(ctx, strategy, client.RouteManager(), "wss"+strings.TrimPrefix(server.URL, "https"), &ClientAuth{ByJwt: "synthetic-h1-batch", InstanceId: NewId(), AppVersion: "synthetic"}, TransportModeH1, carrier)
	defer func() {
		cancel()
		joinCtx, joinCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer joinCancel()
		if err := transport.CloseAndWait(joinCtx); err != nil {
			t.Errorf("H1 close: %v", err)
		}
		if err := client.CloseAndWait(joinCtx); err != nil {
			t.Errorf("client close: %v", err)
		}
		strategy.Close()
		server.Close()
		stats := carrier.PlatformTransportBudget.Stats()
		if stats.UsedByteCount != 0 || stats.UsedTransportCount != 0 || stats.ReservedByteCount != stats.ReleasedByteCount {
			t.Errorf("carrier budget did not balance: %+v", stats)
		}
	}()
	deadline := time.After(10 * time.Second)
	for {
		notify := transport.ConnectedNotify()
		if transport.IsConnected() {
			break
		}
		select {
		case <-notify:
		case <-deadline:
			t.Fatal("physical H1 route did not connect")
		}
	}
	packets := make([][]byte, 16)
	for index := range packets {
		packets[index] = MessagePoolGet(1100)
		clear(packets[index])
		packets[index][0] = byte(index)
	}
	sendWindowTcpPackets(t, ctx, client, destination, protocol.MessageType_IpIpPacketFromProvider, packets)
	for _, packet := range packets {
		MessagePoolReturn(packet)
	}
	select {
	case reading := <-completed:
		t.Logf("socket-batch packets=%d messages=%d max-message=%d limit=%d", reading.packets, reading.messages, reading.maxMessageBytes, carrier.H1MaxMessageByteCount)
		if reading.err != nil {
			t.Fatalf("actual H1 rejected workload batch: %v", reading.err)
		}
		if reading.packets != len(packets) || reading.messages <= 1 || int64(reading.maxMessageBytes) > carrier.H1MaxMessageByteCount {
			t.Fatalf("wrong physical batch shape: %+v", reading)
		}
	case <-deadline:
		t.Fatal("physical H1 did not deliver the socket batch")
	}
}

// Cancellation is the indefinite-wait helper's refusal boundary. A refused
// logical group must release its shares while the caller retains every packet.
func TestWindowTcpCanceledBatchReturnsShares(t *testing.T) {
	assertMessagePoolOwnership(t)
	ctx, cancel := context.WithCancel(context.Background())
	settings := DefaultClientSettings()
	settings.Log = NewNoopLogger()
	settings.EncryptionSettings.Mode = EncryptionModeOff
	settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
	cancel()
	joinCtx, joinCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer joinCancel()
	if err := client.CloseAndWait(joinCtx); err != nil {
		t.Fatal(err)
	}
	baselineCount := MessagePoolOutstandingCount()
	packets := make([][]byte, 16)
	for index := range packets {
		packets[index] = MessagePoolGet(1100)
		clear(packets[index])
		packets[index][0] = byte(index)
	}
	sendWindowTcpPackets(t, ctx, client, NewId(), protocol.MessageType_IpIpPacketFromProvider, packets)
	for index, packet := range packets {
		if pooled, _ := MessagePoolCheck(packet); !pooled || packet[0] != byte(index) {
			t.Errorf("refused helper lost caller ownership of packet %d", index)
		}
		MessagePoolReturn(packet)
	}
	if count := MessagePoolOutstandingCount(); count != baselineCount {
		t.Errorf("refused group retained %d pooled roots", count-baselineCount)
	}
}

// A paused consumer and zero queue slots force real SendSequence admission to
// wait. Cancel just the workload; the still-live Client must not pin its shares.
func TestWindowTcpWorkloadCancelUnblocksGroupAdmission(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		clientCtx, clientCancel := context.WithCancel(context.Background())
		workloadCtx, workloadCancel := context.WithCancel(context.Background())
		defer workloadCancel()
		dispatch := make(chan struct{})
		settings := DefaultClientSettings()
		settings.Log = NewNoopLogger()
		settings.EncryptionSettings.Mode = EncryptionModeOff
		settings.beforeClientKeyPublishForTest = func() { <-clientCtx.Done() }
		settings.SendBufferSettings.SequenceBufferSize = 0
		settings.SendBufferSettings.beforeRunSendSequenceForTest = func(sendSequenceId) { <-dispatch }
		client := NewClient(clientCtx, NewId(), NewNoContractClientOob(), settings)
		destination := NewId()
		client.ContractManager().AddNoContractPeer(destination)
		packets := make([][]byte, 16)
		for index := range packets {
			packets[index] = MessagePoolGet(1100)
			clear(packets[index])
			packets[index][0] = byte(index)
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			sendWindowTcpPackets(t, workloadCtx, client, destination, protocol.MessageType_IpIpPacketFromProvider, packets)
		}()
		synctest.Wait()
		select {
		case <-done:
			t.Error("group bypassed the closed admission gate")
		default:
		}
		workloadCancel()
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Error("workload cancellation left group admission blocked on a live Client")
		}
		if client.Ctx().Err() != nil {
			t.Error("workload cancellation closed its shared Client")
		}
		// Release all barriers and join even on the failure-before schedule.
		clientCancel()
		close(dispatch)
		<-done
		if err := client.CloseAndWait(context.Background()); err != nil {
			t.Error(err)
		}
		for index, packet := range packets {
			if pooled, _ := MessagePoolCheck(packet); !pooled || packet[0] != byte(index) {
				t.Errorf("canceled admission lost caller packet %d", index)
			}
			MessagePoolReturn(packet)
		}
	})
}
