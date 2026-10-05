// Real cleanup delivery preserves report inventory across rotation and cancel.
package connect

import (
	"bytes"
	"context"
	"crypto/sha256"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Optional operations run under the live key owner. The returned client is
// joined so subsequent reports exercise the synchronous cleanup handoff.
func inventoryTestClient(t *testing.T, beforeClose ...func(*Client)) (*Client, *closeReportRecordingOob) {
	t.Helper()
	owner, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	oob := &closeReportRecordingOob{}
	settings := DefaultClientSettings()
	settings.ControlPingTimeout = 0
	settings.Log = NewNoopLogger()
	settings.EncryptionSettings.Mode = EncryptionModeOff
	settings.ContractManagerSettings = DefaultContractManagerSettingsNoNetworkEvents()
	settings.ContractManagerSettings.CloseReportDomainHash = [32]byte{61}
	client := NewClient(owner, NewId(), oob, settings)
	t.Cleanup(func() {
		cancel()
		join, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		if err := client.CloseAndWait(join); err != nil {
			t.Error(err)
		}
	})
	for _, operation := range beforeClose {
		operation(client)
	}
	cancel()
	join, stop := context.WithTimeout(context.Background(), 3*time.Second)
	defer stop()
	if err := client.CloseAndWait(join); err != nil {
		t.Fatal(err)
	}
	return client, oob
}

func TestOriginalInventoryActualCleanupRotationAndIndependentContracts(t *testing.T) {
	contract := NewId()
	client, oob := inventoryTestClient(t, func(client *Client) {
		client.ContractManager().CheckpointContract(contract, 73, 5)
		if err := client.ClientKeyManager().SetSeed(bytes.Repeat([]byte{62}, 32)); err != nil {
			t.Fatal("live owner could not rotate", err)
		}
	})
	rotatedKey := bytes.Clone(client.ClientKeyManager().PublicKey())
	if err := client.ClientKeyManager().SetSeed(bytes.Repeat([]byte{63}, 32)); err == nil {
		t.Fatal("joined key owner accepted a new rotation")
	}
	if !bytes.Equal(rotatedKey, client.ClientKeyManager().PublicKey()) {
		t.Fatal("rejected rotation changed the cleanup signing key")
	}
	client.ContractManager().CloseContract(contract, 48, 7)
	client.ContractManager().CloseContract(NewId(), 121, 0)
	oob.mu.Lock()
	defer oob.mu.Unlock()
	if oob.bad || len(oob.reports) != 3 {
		t.Fatal("actual cleanup inventory obligations missing")
	}
	var prior [32]byte
	for i, report := range oob.reports {
		value, err := protocol.DecodeOriginalCloseInventory(report.OriginalInventory)
		original, e := protocol.DecodeOriginalCloseReport(report.OriginalReport)
		if err != nil || e != nil || !value.Matches(original) {
			t.Fatal("actual cleanup lost exact original inventory", err, e)
		}
		want := uint64(1)
		total := uint64(121)
		previous := [32]byte{}
		if i == 0 {
			total = 73
		}
		if i == 1 {
			want = 2
			previous = prior
		}
		if value.Sequence != want || value.CumulativeAckedBytes != total || value.Previous != previous || value.Terminal == (i == 0) {
			t.Fatal("original rotation reset or crossed contract inventory", value)
		}
		prior = sha256.Sum256(report.OriginalInventory)
	}
	first, _ := protocol.DecodeOriginalCloseInventory(oob.reports[0].OriginalInventory)
	second, _ := protocol.DecodeOriginalCloseInventory(oob.reports[1].OriginalInventory)
	if first.PublicKey == second.PublicKey {
		t.Fatal("fixture did not rotate original signing owner")
	}
}

func TestOriginalInventoryBoundedOwnerNeverDropsOrdinaryClose(t *testing.T) {
	client, oob := inventoryTestClient(t)
	manager := client.ContractManager()
	contract := NewId()
	manager.CloseContract(contract, 1, 0)
	manager.CloseContract(contract, 2, 0)
	manager.closeInventory.stateLock.Lock()
	for len(manager.closeInventory.contractKVs) < maximumOriginalCloseInventoryContracts {
		manager.closeInventory.contractKVs[NewId()] = originalCloseInventoryState{terminal: true}
	}
	manager.closeInventory.stateLock.Unlock()
	manager.CloseContract(NewId(), 3, 0)
	oob.mu.Lock()
	defer oob.mu.Unlock()
	if oob.bad || len(oob.reports) != 3 {
		t.Fatal("optional inventory capacity erased ordinary obligation")
	}
	for i, report := range oob.reports {
		if _, err := protocol.DecodeOriginalCloseReport(report.OriginalReport); err != nil {
			t.Fatal(err)
		}
		if i > 0 && len(report.OriginalInventory) != 0 {
			t.Fatal("terminal or full owner fabricated a fresh inventory")
		}
	}
}

func TestOriginalInventoryMissingIncrementCannotRestartKnownOwner(t *testing.T) {
	client, oob := inventoryTestClient(t)
	manager := client.ContractManager()
	contract := NewId()
	manager.closeInventory.contractKVs = map[Id]originalCloseInventoryState{contract: {sequence: protocol.MaximumOriginalCloseInventoryReports}}
	manager.CheckpointContract(contract, 1, 0)
	manager.CloseContract(contract, 2, 0)
	oob.mu.Lock()
	defer oob.mu.Unlock()
	if oob.bad || len(oob.reports) != 2 {
		t.Fatal("bounded inventory stopped closes")
	}
	for _, report := range oob.reports {
		if len(report.OriginalReport) == 0 || len(report.OriginalInventory) != 0 {
			t.Fatal("missing inventory was silently restarted")
		}
	}
}
