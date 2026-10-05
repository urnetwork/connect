// Explicit shutdown barriers preserve the original close across delivery
// admission and join its one cleanup handoff without changing report identity.
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

// Retains observed bytes independently while optionally holding the synchronous
// external handoff. The launcher still owns its frame until this call returns.
type closeDeliveryTestOob struct {
	stateLock sync.Mutex
	frames    []*protocol.Frame
	bad       bool
	entered   chan struct{}
	release   chan struct{}
	once      sync.Once
}

// Takes every frame through the same cleanup observer as the context API.
func (self *closeDeliveryTestOob) SendControl(frames []*protocol.Frame, callback OobResultFunction) {
	self.SendControlWithCtx(context.Background(), frames, callback)
}

// Takes frames, recording the exact original before returning its pool owner.
func (self *closeDeliveryTestOob) SendControlWithCtx(ctx context.Context, frames []*protocol.Frame, callback OobResultFunction) {
	self.stateLock.Lock()
	if ctx.Err() != nil || ctx.Done() != nil {
		self.bad = true
	}
	for _, frame := range frames {
		message, err := FromFrame(frame)
		if _, ok := message.(*protocol.CloseContract); err != nil || !ok {
			self.bad = true
		}
		self.frames = append(self.frames, proto.Clone(frame).(*protocol.Frame))
	}
	self.stateLock.Unlock()
	if self.entered != nil {
		self.once.Do(func() { close(self.entered) })
	}
	if self.release != nil {
		<-self.release
	}
	for _, frame := range frames {
		MessagePoolReturn(frame.MessageBytes)
	}
	callback(nil, nil)
}

// Authenticated incoming admission supplies a real stored contract without
// placing a second unused reservation in the shutdown queue flush.
func newCloseDeliveryTestClient(t *testing.T, ctx context.Context, oob OutOfBandControl) (*Client, Id) {
	t.Helper()
	settings := closeWaitClientSettings()
	settings.ContractManagerSettings = DefaultContractManagerSettingsNoNetworkEvents()
	settings.ContractManagerSettings.CloseReportDomainHash = [32]byte{91}
	settings.ClientKeySeed = bytes.Repeat([]byte{92}, 32)
	client := NewClient(ctx, NewId(), oob, settings)
	t.Cleanup(func() {
		join, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		if err := client.CloseAndWait(join); err != nil {
			t.Error(err)
		}
	})
	manager := client.ContractManager()
	secret := bytes.Repeat([]byte{93}, 32)
	manager.mutex.Lock()
	manager.provideModes[protocol.ProvideMode_Network] = true
	manager.provideSecretKeys[protocol.ProvideMode_Network] = secret
	manager.mutex.Unlock()
	id := NewId()
	stored, err := proto.Marshal(&protocol.StoredContract{
		ContractId: id.Bytes(), SourceId: NewId().Bytes(),
		DestinationId: client.ClientId().Bytes(), TransferByteCount: 121,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !manager.Verify(SignStoredContract(manager.settings, secret, stored), stored, protocol.ProvideMode_Network) {
		t.Fatal("actual incoming reservation was not authenticated")
	}
	return client, id
}

// Both barriers follow the initial live-context check. One loses retry
// admission; the other owns admission but loses the first native Send.
func runCloseDeliveryShutdown(t *testing.T, admitted bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	releaseClose, closeEntered, closeDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	releaseCleanup := make(chan struct{})
	var releaseCloseOnce, releaseCleanupOnce sync.Once
	resumeClose := func() { releaseCloseOnce.Do(func() { close(releaseClose) }) }
	resumeCleanup := func() { releaseCleanupOnce.Do(func() { close(releaseCleanup) }) }
	oob := &closeDeliveryTestOob{entered: make(chan struct{}), release: releaseCleanup}
	client, id := newCloseDeliveryTestClient(t, ctx, oob)
	manager := client.ContractManager()
	var original *protocol.Frame
	var witness []byte
	barrier := func(frame *protocol.Frame) {
		original = proto.Clone(frame).(*protocol.Frame)
		witness = MessagePoolShareReadOnly(frame.MessageBytes)
		close(closeEntered)
		select {
		case <-releaseClose:
		case <-ctx.Done():
		}
	}
	if admitted {
		manager.beforeCloseControlSendForTest = barrier
	} else {
		manager.beforeCloseControlAdmissionForTest = barrier
	}
	t.Cleanup(func() {
		resumeClose()
		resumeCleanup()
		join, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		if err := client.CloseAndWait(join); err != nil {
			t.Error("cleanup client join", err)
		}
		select {
		case <-closeDone:
			if witness != nil {
				if !MessagePoolReturn(witness) {
					t.Error("joined close retained an extra frame share")
				}
			}
		case <-join.Done():
			t.Error("cleanup close caller join", join.Err())
		}
	})
	go func() {
		defer close(closeDone)
		manager.CloseContract(id, 121, 7)
	}()
	waitCloseWaitBarrier(t, ctx, closeEntered, "original close delivery barrier")
	if err := client.ClientKeyManager().SetSeed(bytes.Repeat([]byte{94}, 32)); err != nil {
		t.Fatal(err)
	}
	client.Close()
	if admitted {
		// The admitted cleanup owns the launcher even while its original
		// caller has not entered the now-closed ControlSync.Send yet.
		waitCloseWaitBarrier(t, ctx, oob.entered, "one retained cleanup handoff")
		joinEntered := make(chan struct{})
		var joinEnteredOnce sync.Once
		manager.beforeCloseWaitForTest = func() { joinEnteredOnce.Do(func() { close(joinEntered) }) }
		joined := make(chan error, 1)
		go func() { joined <- client.CloseAndWait(ctx) }()
		waitCloseWaitBarrier(t, ctx, joinEntered, "manager cleanup join")
		requireCloseWaitBlocked(t, joined, "held original cleanup handoff")
		resumeCleanup()
		waitCloseWaitResult(t, ctx, joined, "one original cleanup joined")
		resumeClose()
	} else {
		// A caller paused before admission stays caller-owned after shutdown.
		if err := client.CloseAndWait(ctx); err != nil {
			t.Fatal(err)
		}
		resumeClose()
		waitCloseWaitBarrier(t, ctx, oob.entered, "rejected admission cleanup")
		resumeCleanup()
	}
	waitCloseWaitBarrier(t, ctx, closeDone, "close caller completion")
	oob.stateLock.Lock()
	if oob.bad || len(oob.frames) != 1 || !proto.Equal(original, oob.frames[0]) {
		oob.stateLock.Unlock()
		t.Fatal("shutdown changed, repeated, or dropped the original cleanup frame")
	}
	oob.stateLock.Unlock()
	message, err := FromFrame(original)
	if err != nil {
		t.Fatal(err)
	}
	report := message.(*protocol.CloseContract)
	signed, err := protocol.DecodeOriginalCloseReport(report.OriginalReport)
	if err != nil || !signed.Matches([16]byte(client.ClientId()), report) ||
		bytes.Equal(signed.PublicKey[:], client.ClientKeyManager().PublicKey()) {
		t.Fatal("cleanup resigned the report after key rotation", err)
	}
	cut, err := manager.OriginalWorkCut(ctx, 9, 130, [32]byte{95})
	if err != nil || !cut.Complete || len(cut.Contracts) != 1 ||
		!bytes.Equal(cut.Contracts[0].LatestInventory, report.OriginalInventory) {
		t.Fatal("cleanup changed the retained complete original inventory", err)
	}
}

// Shutdown after the live check cannot discard the report at retry admission.
func TestOriginalCloseDeliveryShutdownBeforeAdmissionKeepsExactCleanup(t *testing.T) {
	runCloseDeliveryShutdown(t, false)
}

// Admitted cleanup joins its external launcher even if shutdown beats Send.
func TestOriginalCloseDeliveryShutdownBeforeSendJoinsExactCleanup(t *testing.T) {
	runCloseDeliveryShutdown(t, true)
}

// Actual native acknowledgment disposes the retained cleanup share and never
// repeats an already acknowledged obligation when the client later closes.
func TestOriginalCloseDeliveryNativeAckSuppressesShutdownCleanup(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	oob := &closeDeliveryTestOob{}
	client, id := newCloseDeliveryTestClient(t, ctx, oob)
	receiver := NewClient(ctx, ControlId, NewNoContractClientOob(), closeWaitClientSettings())
	client.ContractManager().AddNoContractPeer(ControlId)
	receiver.ContractManager().AddNoContractPeer(client.ClientId())
	sendRoute, ackRoute := make(chan []byte), make(chan []byte)
	client.RouteManager().UpdateTransport(NewSendGatewayTransport(), []Route{sendRoute})
	receiver.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{sendRoute})
	client.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{ackRoute})
	receiver.RouteManager().UpdateTransport(NewSendGatewayTransport(), []Route{ackRoute})
	manager := client.ContractManager()
	completed := make(chan struct{})
	var completedOnce sync.Once
	manager.beforeWorkerDoneForTest = func(name string) {
		if name == "contract close sync" {
			completedOnce.Do(func() { close(completed) })
		}
	}
	var original *protocol.Frame
	var witness []byte
	manager.beforeCloseControlSendForTest = func(frame *protocol.Frame) {
		original = proto.Clone(frame).(*protocol.Frame)
		witness = MessagePoolShareReadOnly(frame.MessageBytes)
	}
	observed := make(chan *protocol.Frame, 8)
	receiver.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, _ Peer) {
		for _, frame := range frames {
			if frame.MessageType == protocol.MessageType_TransferCloseContract {
				select {
				case observed <- proto.Clone(frame).(*protocol.Frame):
				default:
				}
			}
		}
	})
	t.Cleanup(func() {
		join, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		for _, c := range []*Client{client, receiver} {
			if err := c.CloseAndWait(join); err != nil {
				t.Error(err)
			}
		}
		if witness != nil && !MessagePoolReturn(witness) {
			t.Error("acknowledged close retained its cleanup share")
		}
	})
	manager.CloseContract(id, 121, 7)
	select {
	case actual := <-observed:
		if !proto.Equal(original, actual) {
			t.Fatal("native delivery changed original close bytes")
		}
	case <-ctx.Done():
		t.Fatal("native close was not delivered", ctx.Err())
	}
	waitCloseWaitBarrier(t, ctx, completed, "native acknowledgment cleanup")
	if err := client.CloseAndWait(ctx); err != nil {
		t.Fatal(err)
	}
	oob.stateLock.Lock()
	defer oob.stateLock.Unlock()
	if oob.bad || len(oob.frames) != 0 {
		t.Fatal("native acknowledgment still sent shutdown cleanup")
	}
}
