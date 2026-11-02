package connect

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/netip"
	"reflect"
	"testing"
	"time"

	quic "github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	h3qlog "github.com/quic-go/quic-go/http3/qlog"
	"github.com/quic-go/quic-go/qlog"
	"github.com/quic-go/quic-go/qlogwriter"
)

func TestH3QuicPacketStatsReduceFrameBoundaries(t *testing.T) {
	stats := &H3QuicPacketStats{}
	trace := stats.Tracer(context.Background(), true, quic.ConnectionID{})
	recorder := trace.AddProducer()
	recorder.RecordEvent(qlog.PacketSent{
		Raw: qlog.RawInfo{Length: 1200},
		Frames: []qlog.Frame{{
			Frame: &qlog.DatagramFrame{Length: 1100},
		}},
	})
	recorder.RecordEvent(qlog.PacketReceived{
		Raw: qlog.RawInfo{Length: 1250},
		Frames: []qlog.Frame{
			{Frame: &qlog.DatagramFrame{Length: 500}},
			{Frame: &qlog.DatagramFrame{Length: 600}},
		},
	})
	recorder.RecordEvent(qlog.PacketDropped{
		Raw:     qlog.RawInfo{Length: 88},
		Trigger: qlog.PacketDropDOSPrevention,
	})
	recorder.RecordEvent(&qlog.PacketDropped{
		Raw:     qlog.RawInfo{Length: 77},
		Trigger: qlog.PacketDropDuplicate,
	})
	recorder.RecordEvent(qlog.PacketDropped{
		Raw:     qlog.RawInfo{Length: 66},
		Trigger: qlog.PacketDropPayloadDecryptError,
	})
	recorder.RecordEvent(qlog.KeyUpdated{
		Trigger:  qlog.KeyUpdateLocal,
		KeyType:  qlog.KeyTypeClient1RTT,
		KeyPhase: 1,
	})
	recorder.RecordEvent(qlog.KeyUpdated{
		Trigger:  qlog.KeyUpdateLocal,
		KeyType:  qlog.KeyTypeServer1RTT,
		KeyPhase: 1,
	})
	recorder.RecordEvent(qlog.PacketDropped{
		Raw:     qlog.RawInfo{Length: 55},
		Trigger: qlog.PacketDropPayloadDecryptError,
	})
	recorder.RecordEvent(qlog.KeyUpdated{
		Trigger:  qlog.KeyUpdateRemote,
		KeyType:  qlog.KeyTypeClient1RTT,
		KeyPhase: 2,
	})
	recorder.RecordEvent(qlog.KeyDiscarded{
		KeyType:  qlog.KeyTypeClient1RTT,
		KeyPhase: 0,
	})
	recorder.RecordEvent(qlog.PacketLost{})
	recorder.RecordEvent(qlog.LossTimerUpdated{
		Type:      qlog.LossTimerUpdateTypeExpired,
		TimerType: qlog.TimerTypePTO,
	})
	recorder.RecordEvent(qlog.LossTimerUpdated{
		Type:      qlog.LossTimerUpdateTypeSet,
		TimerType: qlog.TimerTypePTO,
	})
	recorder.RecordEvent(qlog.LossTimerUpdated{
		Type:      qlog.LossTimerUpdateTypeExpired,
		TimerType: qlog.TimerTypeACK,
	})
	recorder.RecordEvent(qlog.MTUUpdated{Value: 1200})
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}

	want := H3QuicPacketStatsSnapshot{
		ConnectionCount:                           1,
		ClosedConnectionCount:                     1,
		SentPacketCount:                           1,
		SentPacketByteCount:                       1200,
		SentDatagramPacketCount:                   1,
		SentDatagramFrameCount:                    1,
		SentDatagramByteCount:                     1100,
		ReceivedPacketCount:                       1,
		ReceivedPacketByteCount:                   1250,
		ReceivedDatagramPacketCount:               1,
		ReceivedDatagramFrameCount:                2,
		ReceivedDatagramByteCount:                 1100,
		DroppedPacketCount:                        4,
		DroppedPacketByteCount:                    286,
		DroppedDosPreventionPacketCount:           1,
		DroppedDuplicatePacketCount:               1,
		DroppedOtherPacketCount:                   2,
		DroppedPayloadDecryptErrorPacketCount:     2,
		DroppedPayloadDecryptBeforeKeyUpdateCount: 1,
		DroppedPayloadDecryptAfterKeyUpdateCount:  1,
		LocalKeyUpdateCount:                       1,
		RemoteKeyUpdateCount:                      1,
		KeyDiscardCount:                           1,
		LostPacketCount:                           1,
		ProbeTimeoutCount:                         1,
		MtuUpdateCount:                            1,
		CurrentMtu:                                1200,
	}
	if got := stats.Snapshot(); got != want {
		t.Fatalf("QUIC packet stats=%+v want=%+v", got, want)
	}
}

func TestH3QuicPacketStatsSeparatesPtoAndHandshakeNoResponse(t *testing.T) {
	stats := &H3QuicPacketStats{}

	failedAttempt := stats.beginHandshakeAttempt()
	failedRecorder := stats.tracerForAttempt(failedAttempt)(
		context.Background(),
		true,
		quic.ConnectionID{},
	).AddProducer()
	failedRecorder.RecordEvent(qlog.PacketSent{Raw: qlog.RawInfo{Length: 1200}})
	failedRecorder.RecordEvent(qlog.LossTimerUpdated{
		Type:      qlog.LossTimerUpdateTypeExpired,
		TimerType: qlog.TimerTypePTO,
	})
	failedRecorder.RecordEvent(qlog.PacketSent{Raw: qlog.RawInfo{Length: 1200}})
	failedAttempt.finish(false)
	failedAttempt.finish(false)

	successfulAttempt := stats.beginHandshakeAttempt()
	successfulRecorder := stats.tracerForAttempt(successfulAttempt)(
		context.Background(),
		true,
		quic.ConnectionID{},
	).AddProducer()
	successfulRecorder.RecordEvent(qlog.PacketSent{Raw: qlog.RawInfo{Length: 1200}})
	successfulRecorder.RecordEvent(qlog.PacketReceived{Raw: qlog.RawInfo{Length: 1200}})
	successfulAttempt.finish(true)

	snapshot := stats.Snapshot()
	if snapshot.ProbeTimeoutCount != 1 ||
		snapshot.HandshakeAttemptCount != 2 ||
		snapshot.HandshakeSuccessCount != 1 ||
		snapshot.HandshakeFailureCount != 1 ||
		snapshot.HandshakeSentWithoutResponseCount != 1 {
		t.Fatalf("QUIC handshake/PTO stats=%+v", snapshot)
	}
}

func TestH3QuicPacketFingerprintStatsAreBoundedAndOwned(t *testing.T) {
	fingerprints := NewH3QuicPacketFingerprintStats(2)
	stats := &H3QuicPacketStats{PacketFingerprints: fingerprints}
	recorder := stats.Tracer(context.Background(), true, quic.ConnectionID{}).AddProducer()
	for _, checksum := range []qlog.DatagramPayloadChecksum{0, 11, 11, 22, 33} {
		recorder.RecordEvent(qlog.PacketSent{DatagramPayloadChecksum: checksum})
	}
	for _, checksum := range []qlog.DatagramPayloadChecksum{11, 33} {
		recorder.RecordEvent(qlog.PacketReceived{DatagramPayloadChecksum: checksum})
	}
	for _, checksum := range []qlog.DatagramPayloadChecksum{11, 11, 44} {
		recorder.RecordEvent(qlog.PacketDropped{
			DatagramPayloadChecksum: checksum,
			Trigger:                 qlog.PacketDropPayloadDecryptError,
		})
	}
	recorder.RecordEvent(qlog.PacketDropped{
		DatagramPayloadChecksum: 55,
		Trigger:                 qlog.PacketDropDuplicate,
	})

	want := H3QuicPacketFingerprintStatsSnapshot{
		Sent:                    map[uint32]uint64{11: 2, 22: 1},
		Received:                map[uint32]uint64{11: 1, 33: 1},
		DroppedPayloadDecrypt:   map[uint32]uint64{11: 2, 44: 1},
		RefusedFingerprintCount: 1,
		UnavailableCount:        1,
	}
	got := fingerprints.Snapshot()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("QUIC fingerprints=%+v want=%+v", got, want)
	}
	got.Sent[11] = 99
	if next := fingerprints.Snapshot(); next.Sent[11] != 2 {
		t.Fatalf("snapshot mutated retained fingerprints: %+v", next)
	}
}

// Accepting the http3 schema arms the qlogger close that can panic in quic-go
// v0.61.0 (see h3QuicPacketStatsTrace.SupportsSchemas). Every way the stats
// reach a connection has to decline it, including the send flight's wrapper,
// which forwards any schema that is not its own.
func TestH3QuicPacketStatsTraceSupportsOnlyTheQuicSchema(t *testing.T) {
	stats := &H3QuicPacketStats{}
	sendFlightConfig := &quic.Config{Tracer: stats.Tracer}
	installQuicSendFlight(sendFlightConfig)
	for _, test := range []struct {
		name  string
		trace qlogwriter.Trace
	}{
		{"stats", stats.Tracer(t.Context(), true, quic.ConnectionID{})},
		{"handshake attempt", stats.tracerForAttempt(stats.beginHandshakeAttempt())(t.Context(), true, quic.ConnectionID{})},
		{"send flight over stats", sendFlightConfig.Tracer(t.Context(), true, quic.ConnectionID{})},
	} {
		t.Run(test.name, func(t *testing.T) {
			if !test.trace.SupportsSchemas(qlog.EventSchema) {
				t.Errorf("declined the quic schema %q", qlog.EventSchema)
			}
			if test.trace.SupportsSchemas(h3qlog.EventSchema) {
				t.Errorf("accepted the http3 schema %q", h3qlog.EventSchema)
			}
		})
	}
}

// The extender carrier is where the stats meet an http3 client. A real dial,
// closed abruptly with its request stream open, records its transport counts
// through the production composition and closes one recorder, not two.
func TestH3QuicPacketStatsRecordAnAbruptlyClosedExtenderQuicDial(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	certPEM, keyPEM, err := selfSign([]string{"127.0.0.1"}, "extender-quic-stats", time.Hour, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := quic.ListenAddr("127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{cert},
		NextProtos:   []string{http3.NextProtoH3},
	}, &quic.Config{MaxIdleTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	// as an extender does: take the stream over, send an empty response frame,
	// then relay until the connection goes away
	server := &http3.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stream := w.(http3.HTTPStreamer).HTTPStream()
		if _, err := stream.Write([]byte{0, 0, 0, 0}); err != nil {
			return
		}
		io.Copy(stream, stream)
	})}
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.ServeListener(listener) }()
	defer func() {
		server.Close()
		listener.Close()
		<-serverDone
	}()

	stats := &H3QuicPacketStats{PacketFingerprints: NewH3QuicPacketFingerprintStats(4096)}
	settings := DefaultPlatformTransportSettings()
	settings.H3QuicPacketStats = stats
	extenderConfig := &ExtenderConfig{Ip: netip.MustParseAddr("127.0.0.1")}
	extenderConfig.Profile.ConnectMode = ExtenderConnectModeQuic
	extenderConfig.Profile.Port = listener.Addr().(*net.UDPAddr).Port
	conn, _, err := dialExtenderQuic(
		(&PlatformTransport{settings: settings}).dialContext(ctx),
		DefaultConnectSettings(),
		extenderConfig,
		&tls.Config{InsecureSkipVerify: true}, // only this loopback fixture
		nil,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			t.Fatal(err)
		}
	}
	payload := make([]byte, 64*1024)
	if _, err := conn.Write(payload); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(conn, payload); err != nil {
		t.Fatal(err)
	}
	conn.Close()

	// quic-go closes its producer as its run loop exits, after Close returns
	for stats.Snapshot().ClosedConnectionCount == 0 {
		select {
		case <-ctx.Done():
			t.Fatalf("the connection's recorder never closed: %+v", stats.Snapshot())
		case <-time.After(time.Millisecond):
		}
	}
	// A second producer would close from its own goroutine. On a loaded machine
	// this can miss it, which only ever passes: the schema test is the deterministic pin.
	time.Sleep(200 * time.Millisecond)

	got := stats.Snapshot()
	t.Logf("stats: %+v", got)
	if got.ConnectionCount != 1 || got.ClosedConnectionCount != 1 {
		t.Errorf("connections=%d closed=%d, want one of each", got.ConnectionCount, got.ClosedConnectionCount)
	}
	if got.SentPacketCount == 0 || got.ReceivedPacketCount == 0 {
		t.Errorf("sent packets=%d received packets=%d", got.SentPacketCount, got.ReceivedPacketCount)
	}
	if got.SentPacketByteCount < uint64(len(payload)) || got.ReceivedPacketByteCount < uint64(len(payload)) {
		t.Errorf("sent bytes=%d received bytes=%d, want at least the %d each way", got.SentPacketByteCount, got.ReceivedPacketByteCount, len(payload))
	}
	fingerprints := stats.PacketFingerprints.Snapshot()
	if len(fingerprints.Sent) == 0 || len(fingerprints.Received) == 0 {
		t.Errorf("fingerprints sent=%d received=%d", len(fingerprints.Sent), len(fingerprints.Received))
	}
}
