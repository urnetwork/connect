// Actual native and cleanup paths retain the same original under key rotation.
package connect

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
	"google.golang.org/protobuf/proto"
)

// Withhold the actual native acknowledgment until the same serialized original
// arrives twice. A local key rotation between deliveries must not resign it.
func TestOriginalCloseReportNativeRetryRetainsRotatedKeyOriginal(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	settings := DefaultClientSettings()
	settings.EncryptionSettings.Mode = EncryptionModeOff
	settings.ControlPingTimeout = 0
	settings.Log = NewNoopLogger()
	settings.SendBufferSettings.AckTimeout = 40 * time.Millisecond
	settings.SendBufferSettings.UnreliableAckTimeout = 40 * time.Millisecond
	settings.SendBufferSettings.SelectiveAckTimeout = 40 * time.Millisecond
	settings.ReceiveBufferSettings.WriteTimeout = time.Millisecond
	domain := [32]byte{17}
	settings.ContractManagerSettings.CloseReportDomainHash = domain
	sender := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
	receiver := NewClient(ctx, ControlId, NewNoContractClientOob(), settings)
	sender.ContractManager().AddNoContractPeer(ControlId)
	receiver.ContractManager().AddNoContractPeer(sender.ClientId())
	t.Cleanup(func() {
		cancel()
		join, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		for _, client := range []*Client{sender, receiver} {
			if err := client.CloseAndWait(join); err != nil {
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
			if report, ok := message.(*protocol.CloseContract); err == nil && ok {
				select {
				case observations <- proto.Clone(report).(*protocol.CloseContract):
				default:
				}
			}
		}
	})
	next := func() *protocol.CloseContract {
		t.Helper()
		select {
		case report := <-observations:
			return report
		case <-ctx.Done():
			t.Fatal("actual native original report did not arrive")
			return nil
		}
	}
	sender.ContractManager().CheckpointContract(NewId(), 121, 7)
	first := next()
	original, err := protocol.DecodeOriginalCloseReport(first.OriginalReport)
	if err != nil || original.DomainHash != domain || !original.Matches([16]byte(sender.ClientId()), first) {
		t.Fatal("actual native close lacked exact original client signature", err)
	}
	if err := sender.ClientKeyManager().SetSeed(bytes.Repeat([]byte{41}, ed25519.SeedSize)); err != nil {
		t.Fatal(err)
	}
	retry := next()
	if !proto.Equal(first, retry) || bytes.Equal(original.PublicKey[:], sender.ClientKeyManager().PublicKey()) {
		t.Fatal("retry was resigned after key rotation")
	}
	receiver.RouteManager().UpdateTransport(NewSendGatewayTransport(), []Route{ackRoute})
}

// Cleanup keeps the constructor's original domain even if the caller later
// mutates shared settings. Equal work remains independently signed by report id.
func TestOriginalCloseReportCleanupRetainsOwnedDomainAndDistinctReports(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	oob := &closeReportRecordingOob{}
	settings := DefaultClientSettings()
	settings.ControlPingTimeout = 0
	settings.Log = NewNoopLogger()
	domain := [32]byte{18}
	settings.ContractManagerSettings.CloseReportDomainHash = domain
	client := NewClient(ctx, NewId(), oob, settings)
	settings.ContractManagerSettings.CloseReportDomainHash[0]++
	cancel()
	join, stop := context.WithTimeout(context.Background(), 3*time.Second)
	defer stop()
	if err := client.CloseAndWait(join); err != nil {
		t.Fatal(err)
	}
	contractId := NewId()
	client.ContractManager().CheckpointContract(contractId, 121, 7)
	client.ContractManager().CloseContract(contractId, 121, 7)
	oob.mu.Lock()
	defer oob.mu.Unlock()
	if oob.bad || len(oob.reports) != 2 {
		t.Fatal("cleanup did not deliver original obligations")
	}
	for _, report := range oob.reports {
		original, err := protocol.DecodeOriginalCloseReport(report.OriginalReport)
		if err != nil || original.DomainHash != domain || !original.Matches([16]byte(client.ClientId()), report) {
			t.Fatal("cleanup changed owned original domain or signature", err)
		}
	}
	if bytes.Equal(oob.reports[0].ReportId, oob.reports[1].ReportId) || bytes.Equal(oob.reports[0].OriginalReport, oob.reports[1].OriginalReport) {
		t.Fatal("independent close reports shared original signed identity")
	}
}

// Missing optional evidence does not suppress ordinary settlement obligations.
func TestOriginalCloseReportUnavailableKeyKeepsLegacyObligation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	oob := &closeReportRecordingOob{}
	settings := DefaultClientSettings()
	settings.ControlPingTimeout = 0
	settings.Log = NewNoopLogger()
	settings.ContractManagerSettings.CloseReportDomainHash = [32]byte{19}
	client := NewClient(ctx, NewId(), oob, settings)
	cancel()
	join, stop := context.WithTimeout(context.Background(), 3*time.Second)
	defer stop()
	if err := client.CloseAndWait(join); err != nil {
		t.Fatal(err)
	}
	client.clientKeyManager = nil
	client.ContractManager().CloseContract(NewId(), 121, 7)
	oob.mu.Lock()
	defer oob.mu.Unlock()
	if oob.bad || len(oob.reports) != 1 || len(oob.reports[0].OriginalReport) != 0 || oob.reports[0].AckedByteCount != 121 {
		t.Fatal("optional signing refusal erased a close obligation")
	}
}

// Cancellation after original creation moves that exact signed tuple to the
// cleanup path. The barrier fixes the ordering without a scheduler/time guess.
func TestOriginalCloseReportCreatedBeforeCancelKeepsOneCleanupOriginal(t *testing.T) {
	owner, stopOwner := context.WithTimeout(t.Context(), 10*time.Second)
	defer stopOwner()
	ctx, cancel := context.WithCancel(owner)
	oob := &closeReportRecordingOob{}
	settings := DefaultClientSettings()
	settings.ControlPingTimeout = 0
	settings.Log = NewNoopLogger()
	settings.ContractManagerSettings.CloseReportDomainHash = [32]byte{21}
	client := NewClient(ctx, NewId(), oob, settings)
	created, release, done := make(chan *protocol.CloseContract, 1), make(chan struct{}), make(chan struct{})
	var released sync.Once
	releaseOriginal := func() { released.Do(func() { close(release) }) }
	client.ContractManager().beforeOriginalCloseFrameForTest = func(report *protocol.CloseContract) {
		select {
		case created <- proto.Clone(report).(*protocol.CloseContract):
		case <-owner.Done():
			return
		}
		select {
		case <-release:
		case <-owner.Done():
		}
	}
	t.Cleanup(func() {
		cancel()
		releaseOriginal()
		stopOwner()
		join, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		if err := client.CloseAndWait(join); err != nil {
			t.Error("original close client did not join", err)
		}
		select {
		case <-done:
		case <-join.Done():
			t.Error("original close report worker did not join", join.Err())
		}
	})
	go func() {
		defer close(done)
		client.ContractManager().CloseContract(NewId(), 121, 7)
	}()
	var original *protocol.CloseContract
	select {
	case original = <-created:
	case <-owner.Done():
		t.Fatal("original close was not created at the owned barrier", owner.Err())
	}
	cancel()
	join, stop := context.WithTimeout(owner, 3*time.Second)
	defer stop()
	if err := client.CloseAndWait(join); err != nil {
		t.Fatal("client cancellation did not join before original release", err)
	}
	releaseOriginal()
	select {
	case <-done:
	case <-owner.Done():
		t.Fatal("released original report did not complete", owner.Err())
	}
	oob.mu.Lock()
	defer oob.mu.Unlock()
	if oob.bad || len(oob.reports) != 1 || !proto.Equal(original, oob.reports[0]) {
		t.Fatal("close/cancel boundary replaced or repeated the original")
	}
	if _, err := protocol.DecodeOriginalCloseReport(original.OriginalReport); err != nil {
		t.Fatal("cleanup original was not client signed", err)
	}
}
