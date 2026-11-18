// The advertised encoded window and retained decoder capacity have distinct
// units. This is a capacity-contract witness, not a delivery or speed claim.
package connect

import "testing"

// Real raw8 decoding can exhaust physical RX while all offered wire bytes are
// still below its numerically equal encoded advertisement. Returning one actual
// owner admits the exact refused carrier without changing that advertisement.
func TestReceiveEncodedAdvertisementDoesNotPromiseDecodedPhysicalCapacity(t *testing.T) {
	assertMessagePoolOwnership(t)
	path := TransferPath{SourceId: NewId(), DestinationId: NewId()}
	var wires [3][]byte
	var scans [3]receiveMemoryPackScan
	var owners [3]*decodedPackOwner
	var root, rx *TransferMemoryBudget
	t.Cleanup(func() {
		for _, owner := range owners {
			if owner != nil {
				owner.receivePack.messagePoolReturn()
			}
		}
		for _, wire := range wires {
			MessagePoolReturn(wire)
		}
		for _, budget := range []*TransferMemoryBudget{rx, root} {
			if budget != nil {
				assertRetainedBudgetBalance(t, budget)
				if budget.Stats().QuotaByteCount != 0 {
					t.Error("capacity witness retained logical quota after actual owner return")
				}
			}
		}
	})
	encoded := ByteCount(0)
	for i := range wires {
		body, _, _ := receiveMemoryScanTestBody(t, 8, 1280)
		wires[i] = MessagePoolCopy(receiveMemoryScanTestWire(path, body, false))
		var status receiveMemoryPackScanStatus
		scans[i], status = scanReceiveMemoryPack(wires[i], path)
		if status != receiveMemoryPackScanReady || !scans[i].loanSafe || scans[i].compatibility ||
			scans[i].legacy || scans[i].frameCount != 8 || scans[i].consumerFrameCount != 8 {
			t.Fatal("raw8 encoded/decoded capacity fixture is not a canonical consumer Pack")
		}
		encoded += ByteCount(len(wires[i]))
	}
	cost := scans[0].retainedByteCount
	if cost != scans[1].retainedByteCount || cost != scans[2].retainedByteCount ||
		cost <= ByteCount(len(wires[0])) || encoded >= 2*cost {
		t.Fatal("capacity witness does not place three encoded carriers below two actual decoded claims")
	}
	root = NewTransferMemoryBudget(2 * cost)
	rx = NewTransferMemoryBudgetWithParent(2*cost, root)
	settings := DefaultClientSettings()
	settings.EncryptionSettings.Mode = EncryptionModeOff
	settings.ReceiveBufferSettings.ReceiveQueueBudget = rx
	settings.ReceiveBufferSettings.ReceiveQueueRetainedByteAccounting = true
	settings.ReceiveBufferSettings.ReceiveQueueMaxByteCount = 2 * cost
	settings.ReceiveBufferSettings.ReceiveHeadRecoveryBudget = nil
	client := &Client{settings: settings, clientId: path.DestinationId, log: NewNoopLogger()}
	client.receiveBuffer = &ReceiveBuffer{client: client, receiveBufferSettings: settings.ReceiveBufferSettings}
	sequence := &ReceiveSequence{receiveQueue: newReceiveQueue(rx, 0), receiveBufferSettings: settings.ReceiveBufferSettings}
	advertised := sequence.receiveWindowAdvertisement()
	if ByteCount(advertised) != 2*cost || encoded >= ByteCount(advertised) {
		t.Fatal("encoded advertisement was not independently larger than all three carriers")
	}
	decode := func(index int) (bool, bool) {
		t.Helper()
		decoded := &decodedTransferFrame{}
		defer decoded.release()
		ok, pressure := client.unmarshalReceiveMemoryFrame(wires[index], decoded, false, path, false, sequenceTlsRoleServer, false)
		if ok {
			owner := decoded.detachPackOwner()
			if owner == nil {
				t.Fatal("successful real owned decoder returned no physical claim")
			}
			owner.receivePack.TransferFrameBytes = wires[index]
			wires[index] = nil
			owners[index] = owner
			if owner.receiveItem.memoryBudget != rx || owner.receiveItem.QueueByteCount() != cost ||
				owner.receivePack.receiveQueueByteCount() != cost {
				t.Fatal("actual decoder roots differ from their preflight physical charge")
			}
		}
		return ok, pressure
	}
	for index := range 2 {
		if ok, pressure := decode(index); !ok || pressure {
			t.Fatal("two fully paid decoded claims did not fit their exact physical leaf")
		}
	}
	if rx.UsedByteCount() != 2*cost || root.UsedByteCount() != 2*cost {
		t.Fatal("real decoded owners did not fill the exact physical leaf")
	}
	if ok, pressure := decode(2); ok || !pressure || owners[2] != nil || wires[2] == nil ||
		client.receivePackHandoffDropCount.Load() != 1 || rx.UsedByteCount() != 2*cost || root.UsedByteCount() != 2*cost ||
		sequence.receiveWindowAdvertisement() != advertised {
		t.Fatal("encoded ceiling silently promised physical RX or the refused carrier lost ownership")
	}
	owners[0].receivePack.messagePoolReturn()
	owners[0] = nil
	if ok, pressure := decode(2); !ok || pressure || rx.UsedByteCount() != 2*cost || root.UsedByteCount() != 2*cost ||
		sequence.receiveWindowAdvertisement() != advertised {
		t.Fatal("actual owner return did not admit the exact refused carrier within unchanged capacities")
	}
	for index, owner := range owners {
		if owner != nil {
			owner.receivePack.messagePoolReturn()
			owners[index] = nil
		}
	}
	for _, budget := range []*TransferMemoryBudget{rx, root} {
		assertRetainedBudgetBalance(t, budget)
		if budget.Stats().QuotaByteCount != 0 {
			t.Fatal("encoded/physical capacity witness created logical quota")
		}
	}
	t.Logf("[receive-capacity-units] encoded_offered=%d encoded_advertised=%d one_retained_claim=%d frames=8 root=0 rx=0", encoded, advertised, cost)
}
