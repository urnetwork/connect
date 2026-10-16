package connect

import (
	"net/http"
	"testing"
)

// The proposed Android process cap is exactly the 64-MiB sizing reference.
// Reaching that boundary must not discard mobile HTTP/2 receive-window limits
// or hand HTTP/WebSocket buffers back to library defaults. This is a settings
// and constructor test only: it opens no sockets and changes no runtime limit.
func TestClientStrategyMemoryBudgetBoundaries(t *testing.T) {
	previous := MemoryBudget()
	t.Cleanup(func() { SetMemoryBudget(previous) })
	// These constructors sample a process-global setting; do not parallelize.
	for _, test := range []struct {
		name       string
		budget     ByteCount
		buffer     int
		connection int
		stream     int
	}{
		{name: "unset_library_defaults"},
		{name: "ios_32_mib", budget: mib(32), buffer: 2048, connection: 524288, stream: 262144},
		{name: "android_previous_40_mib", budget: mib(40), buffer: 2560, connection: 655360, stream: 327680},
		{name: "below_reference_63_mib", budget: mib(63), buffer: 4032, connection: 1032192, stream: 516096},
		{name: "android_candidate_exact_64_mib", budget: mib(64), buffer: 4096, connection: 1048576, stream: 524288},
		{name: "above_mobile_65_mib_library_defaults", budget: mib(65)},
	} {
		t.Run(test.name, func(t *testing.T) {
			SetMemoryBudget(test.budget)
			settings := DefaultClientStrategySettings()
			want := [8]int{
				test.buffer, test.buffer, test.buffer, test.buffer,
				test.buffer, test.buffer, test.connection, test.stream,
			}
			got := [8]int{
				settings.HttpReadBufferSize, settings.HttpWriteBufferSize,
				settings.WebSocketReadBufferSize, settings.WebSocketWriteBufferSize,
				settings.Http2MaxDecoderHeaderTableSize, settings.Http2MaxEncoderHeaderTableSize,
				settings.Http2MaxReceiveBufferPerConnection, settings.Http2MaxReceiveBufferPerStream,
			}
			if got != want {
				t.Errorf("HTTP/WS buffers, H2 tables/connection/stream = %v, want %v", got, want)
			}

			// Pin the real consumer fields, not only unused defaults. Neither
			// constructor dials until a request is made.
			dialer := &clientDialer{settings: settings}
			client := dialer.HttpClient()
			t.Cleanup(client.CloseIdleConnections)
			transport, ok := client.Transport.(*http.Transport)
			if !ok || transport.HTTP2 == nil {
				t.Fatal("native HTTP transport or HTTP/2 configuration missing")
			}
			websocket := dialer.WsDialer(settings)
			got = [8]int{
				transport.ReadBufferSize, transport.WriteBufferSize,
				websocket.ReadBufferSize, websocket.WriteBufferSize,
				transport.HTTP2.MaxDecoderHeaderTableSize, transport.HTTP2.MaxEncoderHeaderTableSize,
				transport.HTTP2.MaxReceiveBufferPerConnection, transport.HTTP2.MaxReceiveBufferPerStream,
			}
			if got != want {
				t.Errorf("constructed HTTP/WS buffers, H2 tables/connection/stream = %v, want %v", got, want)
			}
			if MemoryBudget() != test.budget {
				t.Fatal("constructing HTTP/WebSocket settings changed the process sizing target")
			}
		})
	}
}

func TestMobileProcessBudgetKeepsExplicitDeviceCarrierTarget(t *testing.T) {
	previous := MemoryBudget()
	t.Cleanup(func() { SetMemoryBudget(previous) })
	for _, test := range []struct {
		name            string
		process, device ByteCount
	}{
		{"ios_20_32", mib(32), mib(20)},
		{"android_28_40", mib(40), mib(28)},
		{"android_28_64", mib(64), mib(28)},
	} {
		t.Run(test.name, func(t *testing.T) {
			SetMemoryBudget(test.process)
			processDefault := DefaultPlatformTransportBudget()
			device := NewPlatformTransportBudgetForMemoryTarget(test.device)
			if got := processDefault.Stats(); got.TotalByteCount != test.process/4 || got.MaxTransportCount != 16 {
				t.Fatalf("process-default carrier limits = %+v", got)
			}
			if got := device.Stats(); got.TotalByteCount != test.device/4 || got.MaxTransportCount != 16 {
				t.Fatalf("explicit device carrier limits = %+v", got)
			}
			if processDefault == device || processDefault.parent != nil || device.parent != nil {
				t.Fatal("independent lifecycle owners unexpectedly share an admission root")
			}
		})
	}
}
