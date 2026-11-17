//go:build urnetwork_synthetic_fixture && urnetwork_receive_observation

// Diagnostic controls preserve branch identity, zero-allocation producers and
// actual signed decode/refund behavior. Synthetic state below grants no authority.
package connect

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestPrivateReceiveAdmissionReasonsSurviveRecordLoss(t *testing.T) {
	observer := &PrivateReceiveAdmissionObservation{started: time.Now()}
	observer.BeginWindow(1)
	observer.stateLock.Lock()
	observer.record(PrivateReceiveAdmissionObservationRow{Reason: "recovery_contract_ahead"}, NewId(), NewId(), 2)
	observer.stateLock.Unlock()
	report := observer.Snapshot()
	found := false
	for i, name := range report.ReasonNames {
		if name == "recovery_contract_ahead" {
			found = report.ReasonCounts[i] == 1
		}
	}
	if !found || report.Counts[2] != 1 || report.Total != 1 || report.Recorded != 0 || report.LostRecordLock != 1 {
		t.Fatal("actual reason attempt was lost or relabeled with its contended row")
	}
}

func TestPrivateReceiveAdmissionReasonsKeepPublishedFloorAndOpaqueIds(t *testing.T) {
	observer, err := PrivateBeginReceiveAdmissionObservationAt(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	defer observer.Close()
	observer.BeginWindow(1)
	root := NewTransferMemoryBudget(8192)
	parent := NewTransferMemoryBudgetWithParent(8192, root)
	rx := NewTransferMemoryBudgetWithParent(4096, parent)
	head := NewTransferMemoryBudgetWithParent(4096, parent)
	settings := DefaultReceiveBufferSettingsWithBufferSize(4)
	settings.ReceiveQueueBudget, settings.ReceiveHeadRecoveryBudget = rx, head
	client := &Client{clientId: NewId()}
	buffer := &ReceiveBuffer{client: client, receiveBufferSettings: settings}
	client.receiveBuffer = buffer
	sequence := &ReceiveSequence{ctx: context.Background(), sequenceId: NewId()}
	sequence.holdPromise.ingressFloor = 73
	sequence.holdPromise.establishedContract = true
	scan := receiveMemoryPackScan{messageId: NewId(), sequenceId: sequence.sequenceId,
		sequenceNumber: 73, contractAhead: true, contractId: NewId(), contractIdSet: true,
		retainedByteCount: 1536, path: TransferPath{SourceId: NewId()}}
	root.admissionLock.Lock()
	privateObserveReceiveAdmission(buffer, sequence, scan, nil, 2, "recovery_contract_ahead", true, true, 0)
	root.admissionLock.Unlock()
	report := observer.Snapshot()
	if len(report.Rows) != 1 {
		t.Fatalf("recorded rows=%d", len(report.Rows))
	}
	row := report.Rows[0]
	if row.PublishedFloor != 73 || row.LogicalNextKnown || !row.HistoricalEstablished || !row.RootStateAtBranch || !row.ContractAhead ||
		row.MessageToken != privateReceiveAdmissionIdToken(scan.messageId) ||
		row.GenerationToken != privateReceiveAdmissionIdToken(scan.sequenceId) ||
		row.PeerToken != privateReceiveAdmissionIdToken(scan.path.SourceId) ||
		row.ContractToken != privateReceiveAdmissionIdToken(scan.contractId) || row.FirstInsufficientAncestorAtSample != "" {
		t.Fatal("published physical metadata was mistaken for logical/contract authority or identity changed")
	}
	serialized, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []Id{scan.messageId, scan.sequenceId, scan.path.SourceId, scan.contractId} {
		if bytes.Contains(serialized, []byte(id.String())) || bytes.Contains(serialized, id[:]) {
			t.Fatal("raw identity escaped")
		}
	}
	if rx.UsedByteCount() != 0 || head.UsedByteCount() != 0 || parent.UsedByteCount() != 0 || root.UsedByteCount() != 0 {
		t.Fatal("diagnostic reserved physical memory")
	}
}

func TestPrivateReceiveAdmissionReasonsProducerAllocatesNothing(t *testing.T) {
	observer, err := PrivateBeginReceiveAdmissionObservationAt(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	defer observer.Close()
	observer.BeginWindow(1)
	root := NewTransferMemoryBudget(8192)
	parent := NewTransferMemoryBudgetWithParent(8192, root)
	rx := NewTransferMemoryBudgetWithParent(4096, parent)
	head := NewTransferMemoryBudgetWithParent(4096, parent)
	settings := DefaultReceiveBufferSettingsWithBufferSize(4)
	settings.ReceiveQueueBudget, settings.ReceiveHeadRecoveryBudget = rx, head
	client := &Client{clientId: NewId()}
	buffer := &ReceiveBuffer{client: client, receiveBufferSettings: settings}
	client.receiveBuffer = buffer
	scan := receiveMemoryPackScan{messageId: NewId(), sequenceId: NewId(), retainedByteCount: 1536, contractAhead: true}
	allocations := testing.AllocsPerRun(1000, func() {
		privateObserveReceiveAdmission(buffer, nil, scan, nil, 2, "recovery_contract_ahead", false, true, 0)
	})
	if allocations != 0 {
		t.Fatalf("diagnostic producer allocated %g objects per event", allocations)
	}
	report := observer.Snapshot()
	if report.Total != 1001 || report.Recorded != 1001 || report.LostTotal != 0 || len(report.Rows) != 512 {
		t.Fatal("bounded allocation control lost or changed its real producer calls")
	}
}

// Reuse the independently qualified real signed wire/owner/callback/retry fixture;
// no metadata-only observation is accepted as a replacement for its custody proof.
func TestPrivateReceiveAdmissionReasonsActualFreshDecodeKeepsCustody(t *testing.T) {
	observer, err := PrivateBeginReceiveAdmissionObservationAt(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	defer observer.Close()
	observer.BeginWindow(1)
	TestReceiveConsumerFreshSignedOpeningWithoutOwnTailProgresses(t)
	report := observer.Snapshot()
	var missing, paid bool
	for _, row := range report.Rows {
		if row.Reason != "recovery_no_sequence" || row.MessageToken == "" || row.GenerationToken == "" {
			continue
		}
		missing = true
		for _, success := range report.Rows {
			if success.Reason == "recovery_decode_completed" && success.MessageToken == row.MessageToken && success.GenerationToken == row.GenerationToken &&
				success.OwnerClass == "head_recovery" && success.PhysicalOwnerBytes == 1536 && success.PhysicalRootCostsKnown &&
				success.PhysicalActualRootBytes == 1536 && success.DecodedContextKnown && success.FullContract {
				paid = true
			}
		}
	}
	if !missing || !paid {
		t.Fatalf("actual refused ordinary path and rescued signed physical owner were not joined: missing=%t paid=%t lost=%d", missing, paid, report.LostTotal)
	}
}

// Real pooled decoder owners fill each budget boundary. No anonymous byte fill
// or metadata-only claim substitutes for the existing owner-return controls.
func TestPrivateReceiveAdmissionReasonsNameActualInsufficientAncestor(t *testing.T) {
	observer, err := PrivateBeginReceiveAdmissionObservationAt(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	defer observer.Close()
	observer.BeginWindow(1)
	for _, boundary := range []string{"rx", "head_recovery", "client", "root"} {
		f := newEstablishedHeadGuardFixtureWithShape(t, "compact", 1)
		client := &Client{clientId: NewId()}
		client.receiveBuffer = f.buffer
		f.buffer.client = client
		switch boundary {
		case "head_recovery":
			f.allocate(t, f.head)
		case "client":
			f.allocate(t, f.clientPressure)
			f.allocate(t, f.clientPressure)
		case "root":
			f.allocate(t, f.rootPressure)
			f.allocate(t, f.rootPressure)
			f.allocate(t, f.rootPressure)
		}
		var owner *decodedPackOwner
		if boundary == "rx" {
			owner = f.buffer.takeReceiveMemoryOwner(f.sequence, f.scan.messageId, f.scan.sequenceNumber, f.scan.retainedByteCount)
		} else {
			owner = f.buffer.takeReceiveHeadRecoveryOwner(f.sequence, f.scan)
		}
		if owner != nil {
			owner.receivePack.messagePoolReturn()
			t.Fatalf("%s pressure unexpectedly admitted", boundary)
		}
		report := observer.Snapshot()
		found := false
		for _, row := range report.Rows {
			if row.MessageToken == privateReceiveAdmissionIdToken(f.scan.messageId) && row.RootStateAtBranch &&
				row.FirstInsufficientAncestorAtSample == boundary && row.BranchRequestedBytes == f.scan.retainedByteCount {
				found = true
			}
		}
		if !found {
			t.Fatalf("exact paid %s refusal was not attributed under its actual root lock", boundary)
		}
	}
}

// The maintained signed callback root forces final logical refusal after the
// physical decode succeeds and still verifies real retry, ACK and zero owners.
func TestPrivateReceiveAdmissionReasonsActualFinalQuotaKeepsCustody(t *testing.T) {
	observer, err := PrivateBeginReceiveAdmissionObservationAt(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	defer observer.Close()
	observer.BeginWindow(1)
	TestReceiveConsumerFullRxRealFinalCallbackRejectsRxQuota(t)
	report := observer.Snapshot()
	var logical, final bool
	for i, name := range report.ReasonNames {
		if name == "pack_quota_or_budget" && report.ReasonCounts[i] > 0 {
			logical = true
		}
		if name == "decoded_final_pack_refused" && report.ReasonCounts[i] > 0 {
			final = true
		}
	}
	if !logical || !final {
		t.Fatalf("real physical-paid final refusal not classified: logical=%t final=%t", logical, final)
	}
}
