package connect

import (
	"bytes"
	"testing"

	"github.com/urnetwork/connect/protocol"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

// These are ownership/serialization guardrails for the clone. The original
// mutex-by-value defect is a go vet copylocks failure, not a proven runtime race.
func TestOriginalCloseInventoryClonePreservesCallerAndRetainedBytes(t *testing.T) {
	for _, checkpoint := range []bool{true, false} {
		name := "terminal"
		if checkpoint {
			name = "checkpoint"
		}
		t.Run(name, func(t *testing.T) {
			client := newWholeWorkTestClient(t, NewNoContractClientOob(), nil)
			manager := client.ContractManager()
			id := NewId()
			stored := wholeWorkTestAdmit(t, client, id, NewId())
			report := &protocol.CloseContract{
				ContractId:        id.Bytes(),
				AckedByteCount:    37,
				UnackedByteCount:  11,
				Checkpoint:        checkpoint,
				ReportId:          NewId().Bytes(),
				OriginalReport:    []byte("caller-owned original"),
				OriginalInventory: []byte("caller-owned inventory"),
			}
			unknown := protowire.AppendTag(nil, 127, protowire.BytesType)
			unknown = protowire.AppendBytes(unknown, []byte("future close field"))
			report.ProtoReflect().SetUnknown(unknown)
			// Marshal first so this is an initialized protobuf message, including
			// valid unknown wire data, rather than a never-reflected struct.
			before, err := proto.MarshalOptions{Deterministic: true}.Marshal(report)
			if err != nil {
				t.Fatal(err)
			}
			originalRaw, inventoryRaw, err := manager.signOriginalCloseInventory(report)
			if err != nil {
				t.Fatal(err)
			}
			after, err := proto.MarshalOptions{Deterministic: true}.Marshal(report)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("signing changed the caller's serialized fields or unknown data", err)
			}
			original, err := protocol.DecodeOriginalCloseReport(originalRaw)
			if err != nil || !original.Matches([16]byte(client.ClientId()), report) {
				t.Fatal("signed original differs from the caller's close tuple", err)
			}
			inventory, err := protocol.DecodeOriginalCloseInventory(inventoryRaw)
			if err != nil || !inventory.Matches(original) || inventory.Sequence != 1 || inventory.CumulativeAckedBytes != 37 || inventory.Terminal != !checkpoint {
				t.Fatal("generated inventory differs from the signed original", err)
			}
			wantInventory := bytes.Clone(inventoryRaw)
			assertRetained := func() {
				t.Helper()
				cut, err := manager.OriginalWorkCut(t.Context(), 1, 1, [32]byte{1})
				if err != nil || !cut.Complete || len(cut.Contracts) != 1 {
					t.Fatal("generated inventory did not retain complete owner custody", cut, err)
				}
				retained := cut.Contracts[0]
				if retained.ContractId != [16]byte(id) || !bytes.Equal(retained.StoredContract, stored) || !bytes.Equal(retained.LatestInventory, wantInventory) {
					t.Fatal("whole owner retained caller inventory or borrowed mutable bytes")
				}
			}
			assertRetained()
			// Once signing returns, neither caller storage nor returned evidence
			// owns the whole-work inventory's retained bytes.
			for _, raw := range [][]byte{report.ContractId, report.ReportId, report.OriginalReport, report.OriginalInventory, report.ProtoReflect().GetUnknown(), originalRaw, inventoryRaw} {
				clear(raw)
			}
			assertRetained()
		})
	}
}

func TestOriginalCloseInventoryCloneDoesNotReuseCallerInventoryAfterTerminal(t *testing.T) {
	client := newWholeWorkTestClient(t, NewNoContractClientOob(), nil)
	manager := client.ContractManager()
	id := NewId()
	wholeWorkTestAdmit(t, client, id, NewId())
	first := &protocol.CloseContract{ContractId: id.Bytes(), ReportId: NewId().Bytes(), AckedByteCount: 37}
	firstOriginal, firstInventory, err := manager.signOriginalCloseInventory(first)
	if err != nil || len(firstInventory) == 0 {
		t.Fatal("terminal fixture did not retain its original inventory", err)
	}
	report := &protocol.CloseContract{
		ContractId:        id.Bytes(),
		ReportId:          NewId().Bytes(),
		AckedByteCount:    3,
		OriginalReport:    firstOriginal,
		OriginalInventory: firstInventory,
	}
	before, err := proto.MarshalOptions{Deterministic: true}.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	originalRaw, inventoryRaw, err := manager.signOriginalCloseInventory(report)
	if err != nil || len(inventoryRaw) != 0 {
		t.Fatal("terminal chain admitted another inventory", err)
	}
	original, err := protocol.DecodeOriginalCloseReport(originalRaw)
	if err != nil || !original.Matches([16]byte(client.ClientId()), report) {
		t.Fatal("optional inventory refusal lost the ordinary signed close", err)
	}
	after, err := proto.MarshalOptions{Deterministic: true}.Marshal(report)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("inventory refusal changed the caller's serialized report", err)
	}
	cut, err := manager.OriginalWorkCut(t.Context(), 1, 1, [32]byte{1})
	if err != nil || cut.Complete || len(cut.Contracts) != 1 || !bytes.Equal(cut.Contracts[0].LatestInventory, firstInventory) {
		t.Fatal("caller inventory hid the refused close or changed the terminal head", cut, err)
	}
}
