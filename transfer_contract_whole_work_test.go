// Actual manager admission and close paths drive deterministic whole-work cuts.
package connect

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
	"google.golang.org/protobuf/proto"
)

// The callback barrier exposes an unresolved real CreateContract obligation.
type wholeWorkTestOob struct {
	stateLock sync.Mutex
	callback  OobResultFunction
	entered   chan struct{}
	ordinary  *NoContractClientOob
}

// Takes request frames, retaining only the callback after their decoded identity.
func (self *wholeWorkTestOob) SendControl(frames []*protocol.Frame, callback OobResultFunction) {
	if len(frames) == 1 && frames[0].MessageType == protocol.MessageType_TransferCreateContract {
		MessagePoolReturn(frames[0].MessageBytes)
		self.stateLock.Lock()
		self.callback = callback
		self.stateLock.Unlock()
		close(self.entered)
		return
	}
	self.ordinary.SendControl(frames, callback)
}

// Construction enables the prospective original domain before any work starts.
func newWholeWorkTestClient(t *testing.T, oob OutOfBandControl, capture *OriginalWorkCaptureSettings, ids ...Id) *Client {
	t.Helper()
	settings := DefaultClientSettings()
	settings.ControlPingTimeout = 0
	settings.Log = NewNoopLogger()
	settings.EncryptionSettings.Mode = EncryptionModeOff
	settings.ContractManagerSettings = DefaultContractManagerSettingsNoNetworkEvents()
	settings.ContractManagerSettings.CloseReportDomainHash = [32]byte{71}
	settings.ContractManagerSettings.OriginalWorkCapture = capture
	settings.ClientKeySeed = bytes.Repeat([]byte{72}, 32)
	id := NewId()
	if len(ids) != 0 {
		id = ids[0]
	}
	client := NewClient(t.Context(), id, oob, settings)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := client.CloseAndWait(ctx); err != nil {
			t.Error(err)
		}
	})
	return client
}

// A source reservation goes through the ordinary decoded control consumer.
func wholeWorkTestAdmit(t *testing.T, client *Client, id, destination Id) []byte {
	t.Helper()
	stored, err := proto.Marshal(&protocol.StoredContract{ContractId: id.Bytes(), SourceId: client.ClientId().Bytes(), DestinationId: destination.Bytes(), TransferByteCount: 100})
	if err != nil {
		t.Fatal(err)
	}
	frame, err := ToFrame(&protocol.CreateContractResult{Contract: &protocol.Contract{StoredContractBytes: stored}}, client.ContractManager().settings.ProtocolVersion)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.ContractManager().HandleControlFrame(ContractKey{Destination: DestinationId(destination)}, frame); err != nil {
		t.Fatal(err)
	}
	MessagePoolReturn(frame.MessageBytes)
	return stored
}

func TestWholeWorkActualManagerRetainsSourceAdmissionAndEveryClose(t *testing.T) {
	client := newWholeWorkTestClient(t, NewNoContractClientOob(), nil)
	manager := client.ContractManager()
	start, err := manager.OriginalWorkCut(t.Context(), 7, 101, [32]byte{81})
	if err != nil || !start.Complete || len(start.Contracts) != 0 {
		t.Fatal(start, err)
	}
	id, destination := NewId(), NewId()
	stored := wholeWorkTestAdmit(t, client, id, destination)
	manager.CheckpointContract(id, 40, 1)
	manager.CloseContract(id, 60, 0)
	end, err := manager.OriginalWorkCut(t.Context(), 7, 102, [32]byte{82})
	if err != nil || !end.Complete || end.Generation != start.Generation || len(end.Contracts) != 1 || !bytes.Equal(end.Contracts[0].StoredContract, stored) {
		t.Fatal("actual production admission disappeared", end, err)
	}
	head, err := protocol.DecodeOriginalCloseInventory(end.Contracts[0].LatestInventory)
	if err != nil || head.Sequence != 2 || head.CumulativeAckedBytes != 100 || !head.Terminal {
		t.Fatal("whole cut lost real terminal sequence", head, err)
	}
}

func TestWholeWorkActualIncomingVerificationCapturesBeforeAcceptance(t *testing.T) {
	client := newWholeWorkTestClient(t, NewNoContractClientOob(), nil)
	manager := client.ContractManager()
	secret := bytes.Repeat([]byte{73}, 32)
	manager.mutex.Lock()
	manager.provideModes[protocol.ProvideMode_Network] = true
	manager.provideSecretKeys[protocol.ProvideMode_Network] = secret
	manager.mutex.Unlock()
	stored, err := proto.Marshal(&protocol.StoredContract{ContractId: NewId().Bytes(), SourceId: NewId().Bytes(), DestinationId: client.ClientId().Bytes(), TransferByteCount: 121})
	if err != nil {
		t.Fatal(err)
	}
	signature := SignStoredContract(manager.settings, secret, stored)
	if !manager.Verify(signature, stored, protocol.ProvideMode_Network) {
		t.Fatal("valid original reservation refused")
	}
	cut, err := manager.OriginalWorkCut(t.Context(), 8, 110, [32]byte{83})
	if err != nil || !cut.Complete || len(cut.Contracts) != 1 || !bytes.Equal(cut.Contracts[0].StoredContract, stored) {
		t.Fatal("incoming admission escaped complete owner", cut, err)
	}
	signature[0] ^= 1
	if manager.Verify(signature, stored, protocol.ProvideMode_Network) {
		t.Fatal("unverified reservation admitted")
	}
}

func TestWholeWorkActualCreateAmbiguityLatchesUnknownBeforeDelivery(t *testing.T) {
	oob := &wholeWorkTestOob{entered: make(chan struct{}), ordinary: NewNoContractClientOob()}
	client := newWholeWorkTestClient(t, oob, nil)
	manager := client.ContractManager()
	manager.CreateContract(ContractKey{Destination: DestinationId(NewId())}, 0, 100)
	select {
	case <-oob.entered:
	case <-t.Context().Done():
		t.Fatal(t.Context().Err())
	}
	cut, err := manager.OriginalWorkCut(t.Context(), 9, 120, [32]byte{84})
	if err != nil || cut.Complete {
		t.Fatal("inflight create became an empty complete window", cut, err)
	}
	oob.stateLock.Lock()
	callback := oob.callback
	oob.stateLock.Unlock()
	callback(nil, errors.New("synthetic lost create response"))
	cut, err = manager.OriginalWorkCut(t.Context(), 9, 121, [32]byte{85})
	if err != nil || cut.Complete {
		t.Fatal("lost reservation history was reset", cut, err)
	}
}

func TestWholeWorkOwnerOverflowUnknownNeverSuppressesOrdinaryClose(t *testing.T) {
	client, oob := inventoryTestClient(t)
	manager := client.ContractManager()
	manager.wholeWorkInventory.stateLock.Lock()
	manager.wholeWorkInventory.used = protocol.MaximumOriginalWorkCutBytes / 2
	manager.wholeWorkInventory.stateLock.Unlock()
	stored, _ := proto.Marshal(&protocol.StoredContract{ContractId: NewId().Bytes(), SourceId: client.ClientId().Bytes(), DestinationId: NewId().Bytes()})
	manager.admitOriginalWork(stored)
	manager.CloseContract(NewId(), 17, 0)
	cut, err := manager.OriginalWorkCut(t.Context(), 9, 122, [32]byte{86})
	if err != nil || cut.Complete {
		t.Fatal("capacity manufactured an empty proof", cut, err)
	}
	oob.mu.Lock()
	defer oob.mu.Unlock()
	if len(oob.reports) != 1 || oob.reports[0].AckedByteCount != 17 {
		t.Fatal("evidence capacity erased ordinary close")
	}
}

// A syntactically successful create result can still conceal a rejected or
// malformed reservation. Callback completion alone cannot certify no work.
func TestWholeWorkActualMalformedCreateResultCannotCertifyEmptyOwner(t *testing.T) {
	oob := &wholeWorkTestOob{entered: make(chan struct{}), ordinary: NewNoContractClientOob()}
	client := newWholeWorkTestClient(t, oob, nil)
	manager := client.ContractManager()
	manager.CreateContract(ContractKey{Destination: DestinationId(NewId())}, 0, 100)
	<-oob.entered
	frame, err := ToFrame(&protocol.CreateContractResult{Contract: &protocol.Contract{StoredContractBytes: []byte{255, 255}}}, manager.settings.ProtocolVersion)
	if err != nil {
		t.Fatal(err)
	}
	oob.stateLock.Lock()
	callback := oob.callback
	oob.stateLock.Unlock()
	callback([]*protocol.Frame{frame}, nil)
	MessagePoolReturn(frame.MessageBytes)
	cut, err := manager.OriginalWorkCut(t.Context(), 9, 123, [32]byte{87})
	if err != nil || cut.Complete {
		t.Fatal("ignored malformed reservation became empty authority", cut, err)
	}
}
