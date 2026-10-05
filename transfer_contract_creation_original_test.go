//go:build linux || darwin || freebsd

// Actual request/callback tests observe durable originals at transport admission.
package connect

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
	"google.golang.org/protobuf/proto"
)

// The synchronous transport boundary records what was durable before it took
// the request. The test controls response admission without scheduling sleeps.
type originalCreationTestOob struct {
	directory string
	ordinary  *NoContractClientOob
	callback  OobResultFunction
	request   *protocol.CreateContract
	retained  []byte
	readErr   error
}

// Takes frames and retains only an owned parsed request and response callback.
func (self *originalCreationTestOob) SendControl(frames []*protocol.Frame, callback OobResultFunction) {
	if len(frames) != 1 || frames[0].MessageType != protocol.MessageType_TransferCreateContract {
		self.ordinary.SendControl(frames, callback)
		return
	}
	defer MessagePoolReturn(frames[0].MessageBytes)
	self.callback = callback
	self.request = &protocol.CreateContract{}
	if err := ProtoUnmarshal(frames[0].MessageBytes, self.request); err != nil {
		self.readErr = err
		return
	}
	frameRaw, err := proto.Marshal(frames[0])
	if err != nil {
		self.readErr = err
		return
	}
	entries, err := os.ReadDir(self.directory)
	if err != nil {
		self.readErr = err
		return
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "request-") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(self.directory, entry.Name()))
		if err != nil {
			self.readErr = err
			return
		}
		original, err := protocol.DecodeOriginalContractRequest(context.Background(), raw)
		if err != nil {
			self.readErr = err
			return
		}
		if bytes.Equal(original.RequestFrame, frameRaw) {
			self.retained = raw
			return
		}
	}
	self.readErr = errors.New("transport took the request before its original was durable")
}

// Activate the real production capture setting before constructing the client.
func newOriginalCreationTestClient(t *testing.T) (*Client, *originalCreationTestOob) {
	t.Helper()
	directory := filepath.Join(physicalTempDir(t), "original-requests")
	oob := &originalCreationTestOob{directory: directory, ordinary: NewNoContractClientOob()}
	settings := DefaultClientSettings()
	settings.ControlPingTimeout = 0
	settings.Log = NewNoopLogger()
	settings.EncryptionSettings.Mode = EncryptionModeOff
	settings.ClientKeySeed = bytes.Repeat([]byte{121}, 32)
	settings.ContractManagerSettings = DefaultContractManagerSettingsNoNetworkEvents()
	settings.ContractManagerSettings.CloseReportDomainHash = [32]byte{122}
	clientId := NewId()
	key := ed25519.NewKeyFromSeed(settings.ClientKeySeed)
	scope := OriginalContractStoreScope{DomainHash: settings.ContractManagerSettings.CloseReportDomainHash, ClientId: [16]byte(clientId), SourceGeneration: [16]byte{124}}
	copy(scope.PublicKey[:], key[ed25519.SeedSize:])
	settings.ContractManagerSettings.OriginalContractCapture = &OriginalContractCaptureSettings{Directory: directory, PublicKey: scope.PublicKey, SourceGeneration: scope.SourceGeneration}
	prepareOriginalContractStoreTest(t, directory, scope)
	client := NewClient(t.Context(), clientId, oob, settings)
	t.Cleanup(func() {
		if oob.callback != nil {
			callback := oob.callback
			oob.callback = nil
			callback(nil, errors.New("synthetic unresolved request cleanup"))
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := client.CloseAndWait(ctx); err != nil {
			t.Error(err)
		}
	})
	return client, oob
}

// Return one exact echo through the actual manager callback owner.
func originalCreationTestReply(t *testing.T, client *Client, oob *originalCreationTestOob, destination Id) Id {
	t.Helper()
	contractId := NewId()
	stored, err := proto.Marshal(&protocol.StoredContract{ContractId: contractId.Bytes(), SourceId: client.ClientId().Bytes(), DestinationId: destination.Bytes(), TransferByteCount: 80})
	if err != nil {
		t.Fatal(err)
	}
	frame, err := ToFrame(&protocol.CreateContractResult{CreateContract: oob.request, Contract: &protocol.Contract{StoredContractBytes: stored, ProvideMode: protocol.ProvideMode_Public}}, DefaultProtocolVersion)
	if err != nil {
		t.Fatal(err)
	}
	defer MessagePoolReturn(frame.MessageBytes)
	callback := oob.callback
	oob.callback = nil
	if callback == nil {
		t.Fatal("the actual create request did not retain its callback")
	}
	callback([]*protocol.Frame{frame}, nil)
	return contractId
}

func TestOriginalContractCreationActualOwnerRetainsBeforeSendAndPublication(t *testing.T) {
	client, oob := newOriginalCreationTestClient(t)
	manager := client.ContractManager()
	destination := NewId()
	manager.CreateContract(ContractKey{Destination: DestinationId(destination)}, 0, 100)
	if oob.readErr != nil || len(oob.retained) == 0 {
		t.Fatal("pre-send original was not durable", oob.readErr)
	}
	request, err := protocol.DecodeOriginalContractRequest(t.Context(), oob.retained)
	if err != nil || request.ClientId != [16]byte(client.ClientId()) || request.Generation != [16]byte(manager.wholeWorkInventory.generation) {
		t.Fatal("request came from another owner", request, err)
	}
	id := originalCreationTestReply(t, client, oob, destination)
	cut, err := manager.OriginalWorkCut(t.Context(), 7, 100, [32]byte{123})
	if err != nil || !cut.Complete || len(cut.Contracts) != 1 || cut.Contracts[0].ContractId != [16]byte(id) {
		t.Fatal("actual response was not admitted", cut, err)
	}
	admission, err := protocol.DecodeOriginalContractAdmission(t.Context(), cut.Contracts[0].OriginalCreation)
	if err != nil || !bytes.Equal(admission.Request, oob.retained) {
		t.Fatal("published cut lost the pre-send original", err)
	}
	name, err := originalContractLeafName("admission", cut.Contracts[0].OriginalCreation)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(oob.directory, name))
	if err != nil || !bytes.Equal(raw, cut.Contracts[0].OriginalCreation) {
		t.Fatal("queue publication preceded immutable admission custody", err)
	}
}

func TestOriginalContractCreationDoesNotRecreateDeletedRequestAtResponse(t *testing.T) {
	client, oob := newOriginalCreationTestClient(t)
	manager := client.ContractManager()
	destination := NewId()
	manager.CreateContract(ContractKey{Destination: DestinationId(destination)}, 0, 100)
	if oob.readErr != nil {
		t.Fatal(oob.readErr)
	}
	name, err := originalContractLeafName("request", oob.retained)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(oob.directory, name)); err != nil {
		t.Fatal(err)
	}
	originalCreationTestReply(t, client, oob, destination)
	cut, err := manager.OriginalWorkCut(t.Context(), 7, 100, [32]byte{123})
	if err != nil || !cut.Complete || len(cut.Contracts) != 1 || len(cut.Contracts[0].OriginalCreation) != 0 {
		t.Fatal("missing pre-send custody became a proved admission or erased ordinary traffic", cut, err)
	}
	if _, err := os.Stat(filepath.Join(oob.directory, name)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("response recreated the lost pre-send original", err)
	}
}

func TestOriginalContractCreationDifferentReturnedPartyStaysUnproved(t *testing.T) {
	client, oob := newOriginalCreationTestClient(t)
	manager := client.ContractManager()
	manager.CreateContract(ContractKey{Destination: DestinationId(NewId())}, 0, 100)
	if oob.readErr != nil {
		t.Fatal(oob.readErr)
	}
	originalCreationTestReply(t, client, oob, NewId())
	cut, err := manager.OriginalWorkCut(t.Context(), 7, 100, [32]byte{123})
	if err != nil || len(cut.Contracts) != 1 || len(cut.Contracts[0].OriginalCreation) != 0 {
		t.Fatal("different returned party acquired source original provenance", cut, err)
	}
}

func TestOriginalContractCreationStoreRefusesPartialOriginalAndSymlink(t *testing.T) {
	client, oob := newOriginalCreationTestClient(t)
	manager := client.ContractManager()
	destination := NewId()
	manager.CreateContract(ContractKey{Destination: DestinationId(destination)}, 0, 100)
	if oob.readErr != nil {
		t.Fatal(oob.readErr)
	}
	name, err := originalContractLeafName("request", oob.retained)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(oob.directory, name), 0600); err != nil {
		t.Fatal(err)
	}
	originalCreationTestReply(t, client, oob, destination)
	cut, err := manager.OriginalWorkCut(t.Context(), 7, 100, [32]byte{123})
	if err != nil || len(cut.Contracts) != 1 || len(cut.Contracts[0].OriginalCreation) != 0 {
		t.Fatal("partial retained request was accepted as immutable", cut, err)
	}
	alias := filepath.Join(physicalTempDir(t), "alias")
	if err := os.Symlink(oob.directory, alias); err != nil {
		t.Fatal(err)
	}
	if store, err := openOriginalContractStore(t.Context(), alias); err == nil {
		store.close()
		t.Fatal("symlink became a physical original request owner")
	}
}

func TestOriginalContractCreationCustodyCannotPolluteWholeWorkOutbox(t *testing.T) {
	parent := physicalTempDir(t)
	outbox := filepath.Join(parent, "whole-work")
	for _, directory := range []string{outbox, filepath.Join(outbox, "requests"), parent} {
		settings := &ContractManagerSettings{OriginalWorkCapture: &OriginalWorkCaptureSettings{OutboxDirectory: outbox}, OriginalContractCapture: &OriginalContractCaptureSettings{Directory: directory}}
		if _, err := originalContractCreationDirectory(settings); err == nil {
			t.Fatal("overlapping optional custody could alter the complete cut namespace", directory)
		}
	}
	settings := &ContractManagerSettings{OriginalWorkCapture: &OriginalWorkCaptureSettings{OutboxDirectory: outbox}, OriginalContractCapture: &OriginalContractCaptureSettings{Directory: filepath.Join(parent, "requests")}}
	if _, err := originalContractCreationDirectory(settings); err != nil {
		t.Fatal("disjoint sibling custody refused", err)
	}
	if err := os.Mkdir(outbox, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outbox, ".owner.lock"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if store, err := openOriginalContractStore(t.Context(), outbox); err == nil {
		store.close()
		t.Fatal("complete outbox opened as an original request store")
	}
	if _, err := os.Stat(filepath.Join(outbox, ".creation.lock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("refusal polluted the existing complete-cut namespace", err)
	}
}
