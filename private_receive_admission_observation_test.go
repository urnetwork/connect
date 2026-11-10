//go:build urnetwork_synthetic_fixture && urnetwork_receive_observation

// Fixed recorder controls validate diagnostic loss, storage and privacy without
// replacing the separate real Client decode/final-handoff ownership witnesses.
package connect

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// A same-goroutine held record lock must be a counted loss, never a wait.
func TestPrivateReceiveAdmissionObservationRecordContentionDrops(t *testing.T) {
	observer := &PrivateReceiveAdmissionObservation{started: time.Now()}
	observer.BeginWindow(1)
	observer.stateLock.Lock()
	observer.record(PrivateReceiveAdmissionObservationRow{SequenceNumber: 1}, NewId(), NewId(), 3)
	observer.stateLock.Unlock()
	report := observer.Snapshot()
	if report.Total != 1 || report.Counts[3] != 1 || report.Recorded != 0 || report.WindowRecorded != 0 ||
		report.LostRecordLock != 1 || report.LostRootLock != 0 || report.LostWindowTransition != 0 ||
		report.LostTotal != 1 || len(report.Rows) != 0 {
		t.Fatal("held observer lock did not return a single counted diagnostic loss")
	}
}

// A same-goroutine held root lock must also lose the observation immediately.
// Synthetic geometry reserves no bytes and starts no real Client workers.
func TestPrivateReceiveAdmissionObservationRootContentionDrops(t *testing.T) {
	observer, err := PrivateBeginReceiveAdmissionObservation()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(observer.Close)
	observer.BeginWindow(1)
	root := NewTransferMemoryBudget(8192)
	clientBudget := NewTransferMemoryBudgetWithParent(8192, root)
	rx := NewTransferMemoryBudgetWithParent(4096, clientBudget)
	head := NewTransferMemoryBudgetWithParent(4096, clientBudget)
	settings := DefaultReceiveBufferSettingsWithBufferSize(4)
	settings.ReceiveQueueBudget, settings.ReceiveHeadRecoveryBudget = rx, head
	client := &Client{clientId: NewId()}
	buffer := &ReceiveBuffer{client: client, receiveBufferSettings: settings}
	client.receiveBuffer = buffer
	root.admissionLock.Lock()
	privateObserveReceiveAdmission(buffer, nil, receiveMemoryPackScan{sequenceId: NewId(), retainedByteCount: 512}, nil,
		3, "all_predecode_owner_paths_refused", false, true, 0)
	root.admissionLock.Unlock()
	report := observer.Snapshot()
	if report.Total != 1 || report.Counts[3] != 1 || report.Recorded != 0 || report.LostRootLock != 1 ||
		report.LostRecordLock != 0 || report.LostWindowTransition != 0 || report.LostTotal != 1 || len(report.Rows) != 0 {
		t.Fatal("held root lock did not return a single counted diagnostic loss")
	}
	for _, budget := range []*TransferMemoryBudget{rx, head, clientBudget, root} {
		if budget.UsedByteCount() != 0 || budget.Stats().QuotaByteCount != 0 {
			t.Fatal("observation changed an unused physical or quota budget")
		}
	}
}

// Two windows keep stable opaque joins while preserving the first transition
// and latest tail, not only the end of the collapse. Old-window work is lost.
func TestPrivateReceiveAdmissionObservationWindowBoundsAndOpaqueJoins(t *testing.T) {
	observer := &PrivateReceiveAdmissionObservation{started: time.Now()}
	observer.BeginWindow(1)
	client, sequence := NewId(), NewId()
	for i := range 700 {
		observer.record(PrivateReceiveAdmissionObservationRow{SequenceNumber: uint64(i), SequenceKnown: true}, client, sequence, 0)
	}
	first := observer.Snapshot()
	if first.Window != 1 || first.Total != 700 || first.Recorded != 700 || first.WindowRecorded != 700 ||
		first.Counts[0] != 700 || first.WindowRecordedCounts[0] != 700 || first.Overwritten != 188 ||
		first.LostTotal != 0 || first.IdentityOverflow != 0 || len(first.Rows) != 512 ||
		len(first.ClientTokens) != 1 || len(first.SequenceTokens) != 1 {
		t.Fatal("first/latest recorder storage or cumulative counts are not bounded exactly")
	}
	for i, row := range first.Rows {
		want := i
		if 128 <= i {
			want = 316 + i - 128
		}
		if row.Window != 1 || row.SequenceNumber != uint64(want) || row.EventOrdinal != uint64(want+1) ||
			row.ClientOrdinal != 0 || row.SequenceOrdinal != 0 {
			t.Fatalf("bounded recorder changed order or opaque identity at row %d", i)
		}
	}
	clientDigest := sha256.Sum256(client[:])
	var joined [32]byte
	copy(joined[:16], client[:])
	copy(joined[16:], sequence[:])
	sequenceDigest := sha256.Sum256(joined[:])
	if first.ClientTokens[0] != hex.EncodeToString(clientDigest[:]) || first.SequenceTokens[0] != hex.EncodeToString(sequenceDigest[:]) {
		t.Fatal("opaque joins no longer match the exact local source identities")
	}
	serialized, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(serialized, []byte(client.String())) || bytes.Contains(serialized, []byte(sequence.String())) ||
		bytes.Contains(serialized, client[:]) || bytes.Contains(serialized, sequence[:]) {
		t.Fatal("raw source identity escaped the private recorder")
	}
	old := PrivateReceiveAdmissionObservationRow{Window: 1, EventOrdinal: observer.attempt(2)}
	observer.BeginWindow(2)
	observer.record(old, client, sequence, 2)
	observer.record(PrivateReceiveAdmissionObservationRow{SequenceNumber: 900, SequenceKnown: true}, client, sequence, 0)
	second := observer.Snapshot()
	if second.Window != 2 || second.Total != 702 || second.Recorded != 701 || second.WindowRecorded != 1 ||
		second.Counts[0] != 701 || second.Counts[2] != 1 || second.WindowRecordedCounts[0] != 1 ||
		second.WindowRecordedCounts[2] != 0 || second.Overwritten != 0 || second.LostWindowTransition != 1 ||
		second.LostTotal != 1 || len(second.Rows) != 1 || second.Rows[0].SequenceNumber != 900 ||
		second.Rows[0].Window != 2 || second.Rows[0].ClientOrdinal != 0 || second.Rows[0].SequenceOrdinal != 0 ||
		second.ClientTokens[0] != first.ClientTokens[0] || second.SequenceTokens[0] != first.SequenceTokens[0] ||
		first.Rows[0].Window != 1 || len(first.Rows) != 512 {
		t.Fatal("window reset relabeled old work, lost the independent first snapshot or changed stable joins")
	}
}

// A borrowed alias is reported without deduplicating the measured root sum.
// This tests only diagnostic classification; it grants no physical ownership.
func TestPrivateReceiveAdmissionObservationBorrowedRootAliases(t *testing.T) {
	carrier := make([]byte, 128)
	owner := &decodedPackOwner{}
	owner.receivePack.Pack = &owner.pack
	owner.pack.Frames = []*protocol.Frame{{MessageType: protocol.MessageType_IpIpPacketFromProvider, Raw: true, MessageBytes: carrier[:64]}}
	var alias PrivateReceiveAdmissionObservationRow
	privateReceiveAdmissionPhysicalRoots(&alias, owner, carrier, false)
	if !alias.PhysicalRootCostsKnown || !alias.PhysicalRootAliasKnown || !alias.PhysicalRootAlias || alias.PhysicalRootScanTruncated ||
		alias.PhysicalPayloadRoots != 1 || alias.PhysicalCarrierRootBytes != 128 || alias.PhysicalPayloadRootBytes != 64 ||
		alias.PhysicalActualRootBytes != alias.PhysicalMetadataBytes+128+64 {
		t.Fatal("borrowed alias was hidden or changed the diagnostic physical root sum")
	}
	owner.pack.Frames[0].MessageBytes = bytes.Repeat([]byte{0x29}, 64)
	var copied PrivateReceiveAdmissionObservationRow
	privateReceiveAdmissionPhysicalRoots(&copied, owner, carrier, false)
	if !copied.PhysicalRootCostsKnown || !copied.PhysicalRootAliasKnown || copied.PhysicalRootAlias || copied.PhysicalRootScanTruncated ||
		copied.PhysicalPayloadRoots != 1 || copied.PhysicalActualRootBytes != copied.PhysicalMetadataBytes+128+64 {
		t.Fatal("independent borrowed root was classified as a carrier alias")
	}
}
