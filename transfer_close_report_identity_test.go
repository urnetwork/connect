package connect

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
	"google.golang.org/protobuf/proto"
)

// Real native delivery, with the ACK route withheld until the application has
// received two deliveries of one manager-issued checkpoint. Distinct logical
// checkpoints with the same byte count must not acquire the retry's identity.
func TestContractManagerCloseReportIdentityAcrossNativeRetry(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	settings := DefaultClientSettings()
	settings.EncryptionSettings.Mode = EncryptionModeOff
	settings.ControlPingTimeout = 0
	settings.Log = NewNoopLogger()
	settings.SendBufferSettings.AckTimeout = 40 * time.Millisecond
	settings.SendBufferSettings.UnreliableAckTimeout = 40 * time.Millisecond
	settings.SendBufferSettings.SelectiveAckTimeout = 40 * time.Millisecond
	settings.ReceiveBufferSettings.WriteTimeout = time.Millisecond
	sender := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
	receiver := NewClient(ctx, ControlId, NewNoContractClientOob(), settings)
	sender.ContractManager().AddNoContractPeer(ControlId)
	receiver.ContractManager().AddNoContractPeer(sender.ClientId())
	t.Cleanup(func() {
		cancel()
		join, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		for _, c := range []*Client{sender, receiver} {
			if err := c.CloseAndWait(join); err != nil {
				t.Error(err)
			}
		}
	})
	sendRoute, ackRoute := make(chan []byte), make(chan []byte)
	sender.RouteManager().UpdateTransport(NewSendGatewayTransport(), []Route{sendRoute})
	receiver.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{sendRoute})
	sender.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{ackRoute})
	observations := make(chan *protocol.CloseContract, 128)
	receiver.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, _ Peer) {
		for _, frame := range frames {
			message, err := FromFrame(frame)
			if err != nil {
				continue
			}
			if close, ok := message.(*protocol.CloseContract); ok {
				select {
				case observations <- proto.Clone(close).(*protocol.CloseContract):
				case <-ctx.Done():
				}
			}
		}
	})
	next := func() *protocol.CloseContract {
		t.Helper()
		select {
		case m := <-observations:
			return m
		case <-ctx.Done():
			t.Fatal("native close report did not arrive")
			return nil
		}
	}
	waitSync := func() {
		t.Helper()
		manager := sender.ContractManager()
		for {
			manager.mutex.Lock()
			pending := len(manager.closeControlSyncs)
			manager.mutex.Unlock()
			if pending == 0 {
				return
			}
			select {
			case <-ctx.Done():
				t.Fatal("restored native ACK did not retire the owning close sync")
			case <-time.After(time.Millisecond):
			}
		}
	}
	contract := NewId()
	sender.ContractManager().CheckpointContract(contract, 100, 7)
	first, retry := next(), next()
	if len(first.ReportId) != 16 || bytes.Equal(first.ReportId, make([]byte, 16)) {
		t.Fatal("manager checkpoint lacks a nonzero report ID")
	}
	if !proto.Equal(first, retry) || !bytes.Equal(first.ContractId, contract.Bytes()) || !first.Checkpoint || first.AckedByteCount != 100 || first.UnackedByteCount != 7 {
		t.Fatal("native lost-ACK retry changed the logical close report")
	}
	receiver.RouteManager().UpdateTransport(NewSendGatewayTransport(), []Route{ackRoute})
	waitSync()
	// Drain already received copies of the first operation after its owned
	// ControlSync has completed. No following operation has been submitted yet.
	for len(observations) > 0 {
		if !proto.Equal(first, next()) {
			t.Fatal("one checkpoint used multiple report identities")
		}
	}
	sender.ContractManager().CheckpointContract(contract, 100, 7)
	second := next()
	waitSync()
	if len(second.ReportId) != 16 || bytes.Equal(first.ReportId, second.ReportId) || !second.Checkpoint || second.AckedByteCount != 100 {
		t.Fatal("distinct equal-byte checkpoint did not receive a distinct identity")
	}
	for len(observations) > 0 {
		if !proto.Equal(second, next()) {
			t.Fatal("second checkpoint retry identity changed")
		}
	}
	sender.ContractManager().CloseContract(contract, 100, 7)
	final := next()
	waitSync()
	if len(final.ReportId) != 16 || bytes.Equal(first.ReportId, final.ReportId) || bytes.Equal(second.ReportId, final.ReportId) || final.Checkpoint || final.AckedByteCount != 100 {
		t.Fatal("final close reused a checkpoint identity or changed its amount")
	}
}

type closeReportRecordingOob struct {
	mu      sync.Mutex
	reports []*protocol.CloseContract
	bad     bool
}

func (o *closeReportRecordingOob) SendControl(frames []*protocol.Frame, callback OobResultFunction) {
	o.mu.Lock()
	for _, frame := range frames {
		m, err := FromFrame(frame)
		if report, ok := m.(*protocol.CloseContract); err == nil && ok {
			o.reports = append(o.reports, proto.Clone(report).(*protocol.CloseContract))
		} else {
			o.bad = true
		}
		MessagePoolReturn(frame.MessageBytes)
	}
	o.mu.Unlock()
	callback(nil, nil)
}

func TestContractManagerCloseReportIdentityOnClosedClientOob(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	oob := &closeReportRecordingOob{}
	settings := DefaultClientSettings()
	settings.ControlPingTimeout = 0
	settings.Log = NewNoopLogger()
	client := NewClient(ctx, NewId(), oob, settings)
	cancel()
	join, stop := context.WithTimeout(context.Background(), 3*time.Second)
	defer stop()
	if err := client.CloseAndWait(join); err != nil {
		t.Fatal(err)
	}
	contract := NewId()
	client.ContractManager().CheckpointContract(contract, 100, 7)
	client.ContractManager().CheckpointContract(contract, 100, 7)
	client.ContractManager().CloseContract(contract, 100, 7)
	oob.mu.Lock()
	defer oob.mu.Unlock()
	if oob.bad || len(oob.reports) != 3 {
		t.Fatal("closed-client cleanup did not submit the three logical reports")
	}
	seen := map[string]bool{}
	for n, report := range oob.reports {
		if len(report.ReportId) != 16 || bytes.Equal(report.ReportId, make([]byte, 16)) || seen[string(report.ReportId)] || report.AckedByteCount != 100 || report.UnackedByteCount != 7 || report.Checkpoint != (n < 2) {
			t.Fatal("closed-client OOB report identity or operation changed")
		}
		seen[string(report.ReportId)] = true
	}
}

func TestCloseReportIdentityLegacyWireRemainsOptional(t *testing.T) {
	legacy := &protocol.CloseContract{ContractId: NewId().Bytes(), AckedByteCount: 100, Checkpoint: true}
	frame := RequireToFrameWithDefaultProtocolVersion(legacy)
	defer MessagePoolReturn(frame.MessageBytes)
	decoded, err := FromFrame(frame)
	if err != nil {
		t.Fatal(err)
	}
	if report, ok := decoded.(*protocol.CloseContract); !ok || len(report.ReportId) != 0 || !proto.Equal(legacy, report) {
		t.Fatal("ID-less wire compatibility changed")
	}
}
