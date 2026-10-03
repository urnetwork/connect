// Real Client-to-Sequence refusals must retain their exact local gate without
// changing zero-wait signaling, public send results, or pooled-frame ownership.
package connect

import (
	"context"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/urnetwork/connect/protocol"
)

// The real sequence owner is paused before Run. Capacity and rendezvous state
// are therefore fixed before a signal offers into the ordinary send path.
func newSignalAdmissionFixture(t *testing.T, capacity int) (*Client, Id, *captureLogger) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	destinationId := NewId()
	log := &captureLogger{}
	settings := closeWaitClientSettings()
	settings.Log = log
	settings.SendBufferSettings.SequenceBufferSize = capacity
	settings.SendBufferSettings.beforeRunSendSequenceForTest = func(id sendSequenceId) {
		if id.Destination == destinationId {
			<-ctx.Done()
		}
	}
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
	t.Cleanup(func() {
		cancel()
		if err := client.CloseAndWait(context.Background()); err != nil {
			t.Errorf("signal admission fixture close: %v", err)
		}
	})
	return client, destinationId, log
}

// Consumes one test signal and proves immediate refusal, exact pooled return,
// and a finite diagnostic that cannot include any of the private frame values.
func requireSignalAdmissionRefusal(
	t *testing.T,
	client *Client,
	destinationId Id,
	log *captureLogger,
	wantBoundary string,
) {
	t.Helper()
	streamId, generationId := NewId(), NewId()
	privatePayload := "synthetic-private-sdp-and-candidate"
	frame := RequireToFrameWithDefaultProtocolVersion(&protocol.ExchangeSignals{
		StreamId: streamId.Bytes(), SenderGenerationId: generationId.Bytes(),
		Signals: []*protocol.ExchangeSignal{{
			SignalType: protocol.SignalType_SdpAnswer, Sdp: []byte(privatePayload),
		}},
	})
	witness := MessagePoolShareReadOnly(frame.MessageBytes)
	done := make(chan struct{})
	go func() {
		NewClientSignalSender(client).SendSignal(destinationId, frame, signalSendNonBlocking{})
		close(done)
	}()
	synctest.Wait()
	select {
	case <-done:
	default:
		t.Fatal("receive-originated signal waited on a deliberately unavailable send gate")
	}
	if frame.MessageBytes != nil || !MessagePoolReturn(witness) {
		t.Fatal("refused signal did not return exactly its pooled owner")
	}
	log.mu.Lock()
	defer log.mu.Unlock()
	var signalLogs []string
	for _, line := range log.info {
		if strings.HasPrefix(line, "[signal]send failed ") {
			signalLogs = append(signalLogs, line)
		}
	}
	want := "[signal]send failed mode=receive-reply reason=not-admitted boundary=" +
		wantBoundary + " kind=answer reset=false\n"
	if len(signalLogs) != 1 || signalLogs[0] != want {
		t.Fatalf("signal refusal = %q, want %q", signalLogs, want)
	}
	for _, private := range []string{destinationId.String(), streamId.String(), generationId.String(), privatePayload} {
		if strings.Contains(signalLogs[0], private) {
			t.Fatal("signal diagnostic retained private identity or payload")
		}
	}
}

// An occupied real Pack slot refuses before the final channel offer.
func TestClientSignalSenderReportsPackAdmissionBoundary(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		client, destinationId, log := newSignalAdmissionFixture(t, 1)
		first := RequireToFrameWithDefaultProtocolVersion(&protocol.SimpleMessage{Content: "occupied-slot"})
		if !client.SendWithTimeout(first, destinationId, nil, 0) {
			MessagePoolReturn(first.MessageBytes)
			t.Fatal("first Pack did not occupy the fixed admission slot")
		}
		requireSignalAdmissionRefusal(t, client, destinationId, log, "pack-admission")
	})
}

// A zero-size sequence has no Pack budget to fill. Its absent consumer is a
// final handoff refusal, even though it returns the same public false/nil.
func TestClientSignalSenderReportsQueueHandoffBoundary(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		client, destinationId, log := newSignalAdmissionFixture(t, 0)
		requireSignalAdmissionRefusal(t, client, destinationId, log, "queue-handoff")
	})
}

// A reliable Pack can refuse before count admission despite a free count slot.
// Publish exactly the capacity state that the real resend owner publishes.
func TestClientSignalSenderReportsResendCapacityBoundary(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		client, destinationId, log := newSignalAdmissionFixture(t, 2)
		first := RequireToFrameWithDefaultProtocolVersion(&protocol.SimpleMessage{Content: "create-sequence"})
		if !client.SendWithTimeout(first, destinationId, nil, 0) {
			MessagePoolReturn(first.MessageBytes)
			t.Fatal("first Pack did not create the send sequence")
		}
		client.sendBuffer.mutex.Lock()
		sequence := client.sendBuffer.sendSequences[sendSequenceId{
			Destination: destinationId, EncryptionRole: sequenceTlsRoleClient,
		}]
		client.sendBuffer.mutex.Unlock()
		if sequence == nil {
			t.Fatal("missing admitted send sequence")
		}
		sequence.resendCapacityUnavailable.Store(true)
		requireSignalAdmissionRefusal(t, client, destinationId, log, "resend-capacity")
	})
}

// Direct callers retain false/nil and caller ownership when diagnostics are
// available; successful recovery must not carry the preceding refusal label.
func TestSendAdmissionDiagnosticPreservesPublicContract(t *testing.T) {
	assertMessagePoolOwnership(t)
	loopback := make(chan *SendPack, 1)
	loopback <- &SendPack{}
	client := &Client{ctx: context.Background(), clientId: NewId(), loopback: loopback, settings: &ClientSettings{}}
	frame := RequireToFrameWithDefaultProtocolVersion(&protocol.ExchangeSignals{})
	witness := MessagePoolShareReadOnly(frame.MessageBytes)
	accepted, err := client.SendWithTimeoutDetailed(frame, client.clientId, nil, 0)
	if accepted || err != nil || frame.MessageBytes == nil {
		t.Fatalf("public refusal changed ownership or false/nil: %t %v", accepted, err)
	}
	<-loopback
	accepted, err, boundary := client.sendWithTimeoutAdmissionDetailed(frame, client.clientId, MultiHopId{}, nil, 0)
	if !accepted || err != nil || boundary != sendAdmissionUnknown {
		t.Fatalf("recovered admission = %t %v %s", accepted, err, boundary)
	}
	(<-loopback).returnFrames()
	if !MessagePoolReturn(witness) {
		t.Fatal("accepted recovered frame retained a pooled owner")
	}
}

// Homogeneous batches preserve kind, mixed batches never choose one member,
// and the reset flag is independent of the signal payload's kind.
func TestSignalSendFrameKindIsBounded(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, test := range []struct {
		types []protocol.SignalType
		kind  string
	}{
		{kind: "none"},
		{types: []protocol.SignalType{protocol.SignalType_NoSignal}, kind: "none"},
		{types: []protocol.SignalType{protocol.SignalType_SdpOffer}, kind: "offer"},
		{types: []protocol.SignalType{protocol.SignalType_SdpAnswer}, kind: "answer"},
		{types: []protocol.SignalType{protocol.SignalType_IceCandidate, protocol.SignalType_IceCandidate}, kind: "candidate"},
		{types: []protocol.SignalType{protocol.SignalType_WaitingForSdpOffer}, kind: "waiting"},
		{types: []protocol.SignalType{protocol.SignalType_SdpOffer, protocol.SignalType_IceCandidate}, kind: "mixed"},
		{types: []protocol.SignalType{protocol.SignalType(99)}, kind: "unknown"},
	} {
		for _, reset := range []bool{false, true} {
			signals := &protocol.ExchangeSignals{ResetSignals: reset}
			for _, signalType := range test.types {
				signals.Signals = append(signals.Signals, &protocol.ExchangeSignal{SignalType: signalType})
			}
			frame := RequireToFrameWithDefaultProtocolVersion(signals)
			kind, resetValue := signalSendFrameKind(frame)
			MessagePoolReturn(frame.MessageBytes)
			wantReset := "false"
			if reset {
				wantReset = "true"
			}
			if kind != test.kind || resetValue != wantReset {
				t.Errorf("types=%v reset=%t: kind=%q reset=%q", test.types, reset, kind, resetValue)
			}
		}
	}
	if got := sendAdmissionBoundary(255).String(); got != "unknown" {
		t.Fatalf("invalid boundary escaped finite vocabulary: %q", got)
	}
}

// Malformed, wrong-type, and excessive diagnostic inputs retain only unknown.
func TestSignalSendFrameKindBoundsDecodeWork(t *testing.T) {
	assertMessagePoolOwnership(t)
	manySignals := &protocol.ExchangeSignals{}
	for range 65 {
		manySignals.Signals = append(manySignals.Signals, &protocol.ExchangeSignal{SignalType: protocol.SignalType_SdpOffer})
	}
	frame := RequireToFrameWithDefaultProtocolVersion(manySignals)
	defer MessagePoolReturn(frame.MessageBytes)
	for _, input := range []*protocol.Frame{
		nil,
		{MessageType: protocol.MessageType_TestSimpleMessage},
		{MessageType: protocol.MessageType_TransferExchangeSignals, MessageBytes: []byte{0xff}},
		{MessageType: protocol.MessageType_TransferExchangeSignals, MessageBytes: make([]byte, 64*1024+1)},
		frame,
	} {
		if kind, reset := signalSendFrameKind(input); kind != "unknown" || reset != "unknown" {
			t.Errorf("unsupported diagnostic input yielded kind=%q reset=%q", kind, reset)
		}
	}
}
