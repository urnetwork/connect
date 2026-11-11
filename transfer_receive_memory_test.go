// Exact root transactions isolate pending-original identity, promise retirement,
// and encoded handoff quota. Public-path owner stages are covered separately.
package connect

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"

	"github.com/urnetwork/connect/protocol"
	"testing"
	"unsafe"
)

// Builds only the existing ledger/sequence state; no worker or socket runs.
func receiveMemoryTestState(t *testing.T, capacity ByteCount) (*TransferMemoryBudget, *TransferMemoryBudget, *ReceiveBuffer, *ReceiveSequence) {
	t.Helper()
	root := NewTransferMemoryBudget(capacity)
	rx := NewTransferMemoryBudgetWithParent(capacity, root)
	settings := DefaultReceiveBufferSettings()
	settings.ReceiveQueueBudget = rx
	settings.ReceiveQueueRetainedByteAccounting = true
	queue := newReceiveQueue(rx, 0)
	queue.lifetimeBudget = true
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sequence := &ReceiveSequence{ctx: ctx, receiveQueue: queue, receiveBufferSettings: settings, nextSequenceNumber: 1}
	buffer := &ReceiveBuffer{receiveBufferSettings: settings}
	t.Cleanup(func() {
		sequence.releaseHoldPromise()
		if root.Stats().UsedByteCount != 0 {
			t.Errorf("receive claim leaked: root=%d", root.Stats().UsedByteCount)
		}
	})
	return root, rx, buffer, sequence
}

// Root saturation cannot block conversion of an already-funded predecessor.
// Both indexes reject a concurrent collision before it can borrow another slot.
func TestReceiveMemoryPendingPromiseIdentityAndGrowth(t *testing.T) {
	assertMessagePoolOwnership(t)
	const cost = ByteCount(4096)
	root, rx, buffer, sequence := receiveMemoryTestState(t, 2*cost)
	if !sequence.reserveHoldPromise(2 * cost) {
		t.Fatal("promise setup failed")
	}
	sequence.settleHoldPromise(true, 3, 2*cost)
	id := NewId()
	owner := buffer.takeReceiveMemoryOwner(sequence, id, 1, cost)
	if owner == nil {
		t.Fatal("full-root promised head could not convert paid credit")
	}
	defer func() {
		if owner != nil {
			owner.release()
		}
	}()
	if root.Stats().UsedByteCount != 2*cost || rx.Stats().UsedByteCount != 2*cost || sequence.holdPromise.reservedByteCount != cost || owner.receivePack.ingressLoanByteCount != cost {
		t.Fatal("conversion changed physical root or unpaid credit")
	}
	for _, collision := range []struct {
		id     Id
		number uint64
	}{{id: id, number: 2}, {id: NewId(), number: 1}, {id: id, number: 1}} {
		if duplicate := buffer.takeReceiveMemoryOwner(sequence, collision.id, collision.number, cost); duplicate != nil {
			duplicate.release()
			t.Fatal("pending original collision spent another promise")
		}
	}
	if foreign := buffer.takeReceiveMemoryOwner(nil, NewId(), 1, cost); foreign != nil {
		foreign.release()
		t.Fatal("unbound original borrowed another sequence's promise")
	}
	owner.receiveItem.decodedOwner = owner
	if !owner.receiveItem.growReceiveMemory(cost) || owner.receiveItem.QueueByteCount() != 2*cost || root.Stats().UsedByteCount != 2*cost || sequence.holdPromise.reservedByteCount != 0 {
		t.Fatal("downstream growth released/reacquired or double-charged prefix credit")
	}
	sequence.adoptReceiveMemoryOwner(&owner.receiveItem)
	sequence.settleHoldPromise(false, 0, 0)
	if root.Stats().UsedByteCount != 2*cost || owner.receivePack.ingressLoanByteCount != 0 {
		t.Fatal("adoption released live owner")
	}
	owner.release()
	owner = nil
	if root.Stats().UsedByteCount != 0 {
		t.Fatal("actual owner return did not retire claim")
	}
}

// Old generation retirement revokes only the return association. The original
// remains funded, and its later return cannot replenish a new equal-size hold.
func TestReceiveMemoryClosedPromiseCannotRestoreIntoReplacement(t *testing.T) {
	assertMessagePoolOwnership(t)
	const cost = ByteCount(4096)
	root, rx, buffer, sequence := receiveMemoryTestState(t, 2*cost)
	if !sequence.reserveHoldPromise(2 * cost) {
		t.Fatal("promise setup failed")
	}
	sequence.settleHoldPromise(true, 3, 2*cost)
	owner := buffer.takeReceiveMemoryOwner(sequence, NewId(), 1, cost)
	if owner == nil {
		t.Fatal("initial loan failed")
	}
	defer func() {
		if owner != nil {
			owner.release()
		}
	}()
	sequence.releaseHoldPromise()
	if root.Stats().UsedByteCount != cost || owner.receivePack.ingressLoanByteCount != 0 {
		t.Fatal("closure released a pending owner's physical bytes")
	}
	queue := newReceiveQueue(rx, 0)
	queue.lifetimeBudget = true
	replacement := &ReceiveSequence{ctx: context.Background(), receiveQueue: queue, receiveBufferSettings: sequence.receiveBufferSettings, nextSequenceNumber: 1}
	defer replacement.releaseHoldPromise()
	if !replacement.reserveHoldPromise(cost) {
		t.Fatal("replacement reserve failed")
	}
	replacement.settleHoldPromise(true, 2, cost)
	owner.release()
	owner = nil
	if root.Stats().UsedByteCount != cost || replacement.holdPromise.reservedByteCount != cost || replacement.holdPromise.ingressLoanByteCount != 0 {
		t.Fatal("retired pending owner restored into replacement promise")
	}
	replacement.releaseHoldPromise()
}

// A real Head reset can obsolete a pending decode before that decode returns.
// Its loan association disappears, while physical custody stays intact.
func TestReceiveMemoryHeadAdvanceKeepsPendingOwnerFunded(t *testing.T) {
	assertMessagePoolOwnership(t)
	const cost = ByteCount(4096)
	root, _, buffer, sequence := receiveMemoryTestState(t, 2*cost)
	if !sequence.reserveHoldPromise(2 * cost) {
		t.Fatal("promise setup failed")
	}
	sequence.settleHoldPromise(true, 3, 2*cost)
	owner := buffer.takeReceiveMemoryOwner(sequence, NewId(), 1, cost)
	if owner == nil {
		t.Fatal("initial loan failed")
	}
	defer func() {
		if owner != nil {
			owner.release()
		}
	}()
	sequence.nextSequenceNumber = 4
	sequence.publishReceiveMemoryFloor()
	sequence.settleHoldPromise(false, 0, 0)
	if root.Stats().UsedByteCount != cost || owner.receivePack.ingressLoanByteCount != 0 {
		t.Fatal("head advancement returned a still-live pending owner")
	}
	owner.release()
	owner = nil
	if root.Stats().UsedByteCount != 0 {
		t.Fatal("head-obsolete owner failed to return exactly once")
	}
}

// Encoded Pack permission is visible in its leaf, but never charged again to
// the physical root. A quota release grants an older waiter synchronously.
func TestReceiveMemoryCoveredQuotaAtFullRoot(t *testing.T) {
	root := NewTransferMemoryBudget(64)
	rx := NewTransferMemoryBudgetWithParent(64, root)
	pack := NewTransferMemoryBudgetWithParent(64, root)
	if !rx.TryReserve(64) {
		t.Fatal("physical owner setup failed")
	}
	defer rx.Release(64)
	stop := receiveMemoryQuotaStop(pack, rx)
	if stop != root || pack.receiveQuotaAvailable(stop) != 64 {
		t.Fatal("common root incorrectly consumes independent Pack permission")
	}
	if ok, waiter := pack.reserveReceiveQuota(32, stop, false); !ok || waiter != nil {
		t.Fatal("funded owner could not enter encoded handoff at full root")
	}
	if root.Stats().UsedByteCount != 64 || pack.Stats().UsedByteCount != 0 || pack.Stats().QuotaByteCount != 32 || pack.Available() != 0 {
		t.Fatal("quota changed physical accounting or ordinary root admission")
	}
	ok, waiter := pack.reserveReceiveQuota(48, stop, true)
	if ok || waiter == nil {
		t.Fatal("full encoded leaf did not queue bounded waiter")
	}
	pack.releaseReceiveQuota(32, stop)
	select {
	case <-waiter.ready:
	default:
		t.Fatal("quota-only release did not reserve older waiter before notification")
	}
	if root.Stats().UsedByteCount != 64 || pack.Stats().QuotaByteCount != 48 {
		t.Fatal("FIFO grant changed physical owner")
	}
	waiter.abandon()
	if pack.Stats().QuotaByteCount != 0 || root.Stats().UsedByteCount != 64 {
		t.Fatal("abandoned quota did not return exact permission")
	}
}

// Physical backing avoids charging the common root twice; it does not change
// the caller's retained-versus-encoded policy for the local Pack permission.
func TestReceiveMemoryCoveredPackPreservesRetainedQuotaPolicy(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, retained := range []bool{true, false} {
		func() {
			path := TransferPath{SourceId: NewId(), DestinationId: NewId()}
			body, _, sequenceId := receiveMemoryScanTestBody(t, 8, 1280)
			wire := MessagePoolCopy(receiveMemoryScanTestWire(path, body, false))
			defer func() {
				if wire != nil {
					MessagePoolReturn(wire)
				}
			}()
			scan, status := scanReceiveMemoryPack(wire, path)
			encoded := ByteCount(len(wire))
			cost := scan.retainedByteCount
			if status != receiveMemoryPackScanReady || cost <= encoded {
				t.Fatal("retained and encoded quota premise changed")
			}
			root := NewTransferMemoryBudget(cost)
			rx := NewTransferMemoryBudgetWithParent(cost, root)
			packBudget := NewTransferMemoryBudgetWithParent(encoded+(cost-encoded)/2, root)
			settings := DefaultReceiveBufferSettingsWithBufferSize(4)
			settings.ReceiveQueueBudget = rx
			settings.ReceiveQueueRetainedByteAccounting = true
			settings.PackQueueBudget = packBudget
			settings.PackQueueRetainedByteAccounting = retained
			clientSettings := DefaultClientSettings()
			clientSettings.ReceiveBufferSettings = settings
			clientSettings.EncryptionSettings.Mode = EncryptionModeOff
			client := &Client{settings: clientSettings, log: DefaultLogger(), receiveBuffer: &ReceiveBuffer{receiveBufferSettings: settings}}
			sequence := newReceiveSequence(context.Background(), client, path.SourceMask(), sequenceId,
				TransferKey{ForceStream: scan.forceStream, CompanionContract: scan.companionContract, LogicalLane: scan.logicalLane}, settings)
			defer sequence.Close()
			decoded := &decodedTransferFrame{}
			defer decoded.release()
			ok, pressure := client.unmarshalReceiveMemoryFrame(wire, decoded, false, path, false, sequenceTlsRoleServer, false)
			if !ok || pressure {
				t.Fatal("fresh physical owner could not be funded")
			}
			owner := decoded.detachPackOwner()
			if owner == nil {
				t.Fatal("selected decoder did not return a physical owner")
			}
			owner.receivePack.TransferFrameBytes = wire
			wire = nil
			owner.receivePack.TransportType = TransportTypeH1
			defer func() {
				if owner != nil {
					owner.receivePack.messagePoolReturn()
				}
			}()
			receivePack := &owner.receivePack
			if owner.receiveItem.memoryBudget != rx || owner.receiveItem.QueueByteCount() != cost || root.Available() != 0 {
				t.Fatal("Pack policy was not tested with a paid owner at a full physical root")
			}
			accepted, err := sequence.Pack(receivePack, 0)
			if accepted {
				owner = nil
			} // Queue owns the exact object until dequeue.
			if err != nil || accepted == retained {
				t.Fatalf("retained=%t accepted=%t err=%v; local policy changed", retained, accepted, err)
			}
			wantQuota := ByteCount(0)
			if !retained {
				wantQuota = encoded
			}
			if packBudget.Stats().QuotaByteCount != wantQuota || packBudget.UsedByteCount() != 0 || root.UsedByteCount() != cost ||
				receivePack.decodedOwner.receiveItem.memoryBudget != rx || receivePack.decodedOwner.receiveItem.QueueByteCount() != cost || sequence.ctx.Err() != nil {
				t.Fatal("local permission changed physical custody or canceled the receiver")
			}
			if accepted {
				dequeued := <-sequence.packs
				if dequeued != receivePack {
					t.Fatal("handoff changed the owned Pack")
				}
				owner = dequeued.decodedOwner
				sequence.releasePackQueue(dequeued)
			}
			owner.receivePack.messagePoolReturn()
			owner = nil
			if root.UsedByteCount() != 0 || rx.UsedByteCount() != 0 || packBudget.UsedByteCount() != 0 || packBudget.Stats().QuotaByteCount != 0 ||
				sequence.holdPromise.ingressHead != nil || sequence.packQueueCount.Load() != 0 || sequence.packQueueByteCount.Load() != 0 {
				t.Fatal("retained/encoded local policy did not return exact owner and quota")
			}
			t.Logf("[rx-retained-pack-quota] retained=%t admitted=%t encoded=%d physical=%d quota=%d", retained, accepted, encoded, cost, wantQuota)
		}()
	}
}

// Include the runtime's pointer-bearing malloc header in the fixed envelope.
// The preexisting raw Sizeof guard alone permits a1024->1152 class cliff.
func TestReceiveMemoryOwnerEnvelopeStillFitsAllocationClass(t *testing.T) {
	if size := unsafe.Sizeof(decodedPackOwner{}); size+8 > uintptr(decodedPackOwnerQueueByteCount) {
		t.Fatalf("owner+allocator header=%d exceeds fixed envelope%d", size+8, decodedPackOwnerQueueByteCount)
	}
}

// A known carrier ceiling, including padded ciphertext and pool-class cliffs,
// is prepaid before a plaintext prefix can promise its encrypted predecessor.
// The actual trial-decrypt path first rejects a wrong key then opens the prior
// key without modifying ciphertext. No new root reservation is available.
func TestReceiveMemoryKnownCarrierDecryptAtFullPromisedRoot(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, count := range []int{0, 8} {
		func() {
			const maximum = 16 * 1024
			root, rx, buffer, sequence := receiveMemoryTestState(t, 128*1024)
			settings := DefaultClientSettings()
			settings.ReceiveBufferSettings = sequence.receiveBufferSettings
			settings.EncryptionSettings.Mode = EncryptionModeRequired
			client := &Client{settings: settings, receiveBuffer: buffer}
			defer client.releaseReceiveDecodeMemory()
			if !client.prepareReceiveDecodeMemory(nil, maximum) {
				t.Fatal("known carrier scratch was not prepaid before commitments")
			}
			scratch := client.receiveDecodeReservedByteCount.Load()
			path := TransferPath{SourceId: NewId(), DestinationId: NewId()}
			body, _, sequenceId := receiveMemoryScanTestBody(t, count, 1280)
			plain := receiveMemoryScanTestWire(path, body, false)
			preview := MessagePoolCopy(plain)
			scan, status := scanReceiveMemoryPack(preview, path)
			MessagePoolReturn(preview)
			if status != receiveMemoryPackScanReady {
				t.Fatal("valid inner preflight failed")
			}
			sequence.transferKey = TransferKey{ForceStream: true, CompanionContract: true, LogicalLane: 3}
			buffer.receiveSequences = map[receiveSequenceId]*ReceiveSequence{{Source: path.SourceMask(), SequenceId: sequenceId, EncryptionRole: sequenceTlsRoleServer, LogicalLane: 3}: sequence}
			if !sequence.reserveHoldPromise(scan.retainedByteCount) {
				t.Fatal("promised-owner setup failed")
			}
			sequence.settleHoldPromise(true, 10, scan.retainedByteCount)
			foreign := root.Available()
			if !root.TryReserve(foreign) {
				t.Fatal("root saturation setup failed")
			}
			defer func() {
				if foreign != 0 {
					root.Release(foreign)
				}
			}()
			if root.Available() != 0 {
				t.Fatal("decrypt boundary is not physically full")
			}
			good := newFrameCodecTestSequenceCipher(t)
			block, err := aes.NewCipher(bytes.Repeat([]byte{1}, sequenceTlsKeyLength))
			if err != nil {
				t.Fatal(err)
			}
			aead, err := cipher.NewGCM(block)
			if err != nil {
				t.Fatal(err)
			}
			session := &peerEncryptionSession{role: sequenceTlsRoleServer, establishedEpoch: &tlsHandshakeEpoch{derivedTlsCipher: &sequenceCipher{aead: aead}}, priorEstablishedEpoch: &tlsHandshakeEpoch{derivedTlsCipher: good}}
			client.encryptionSessionManager = &EncryptionSessionManager{settings: settings.EncryptionSettings, sessions: map[sessionKey]*peerEncryptionSession{{peerId: path.SourceId, role: sequenceTlsRoleServer}: session}}
			outer, err := good.SealOuterFrame(path, plain, protocol.SequenceRole_SequenceRoleClient, false)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if outer != nil {
					MessagePoolReturn(outer)
				}
			}()
			if count == 0 {
				padded := protoAppendTag(append([]byte{}, outer...), 99, protoWireBytes)
				padded = protoAppendVarint(padded, 5000)
				padded = append(padded, make([]byte, 5000)...)
				MessagePoolReturn(outer)
				outer = MessagePoolCopy(padded)
			}
			if len(outer) > maximum || !client.prepareReceiveDecodeMemory(outer, maximum) || client.receiveDecodeReservedByteCount.Load() != scratch {
				t.Fatal("prepaid known carrier needed fresh full-root scratch")
			}
			if !client.prepareReceiveDecodeMemory(outer, 64*1024) || client.receiveDecodeReservedByteCount.Load() != scratch {
				t.Fatal("larger route ceiling withdrew already-paid small-head permission")
			}
			outerDecoded := &decodedTransferFrame{}
			defer outerDecoded.release()
			if !unmarshalOwnedTransferFrame(outer, outerDecoded, true) {
				t.Fatal("actual outer wrapper decode failed")
			}
			companion := false
			inner, role, _, err := client.unwrapFrame(path.SourceId, protocol.SequenceRole_SequenceRoleClient, &companion, outerDecoded.frame.EncryptedTransferFrame)
			if err != nil || !bytes.Equal(inner, plain) || role != sequenceTlsRoleServer {
				t.Fatal("trial decrypt changed the retained ciphertext or failed prior-key recovery")
			}
			defer func() {
				if inner != nil {
					MessagePoolReturn(inner)
				}
			}()
			decoded := &decodedTransferFrame{}
			ok, pressure := client.unmarshalReceiveMemoryFrame(inner, decoded, true, path, true, role, false)
			if !ok || pressure {
				MessagePoolReturn(inner)
				inner = nil
				decoded.release()
				t.Fatal("prepaid promised head needed fresh root bytes")
			}
			owner := decoded.detachPackOwner()
			decoded.release()
			if owner == nil || owner.receiveItem.memoryBudget != rx || owner.receivePack.ingressLoanByteCount != scan.retainedByteCount || root.Available() != 0 {
				MessagePoolReturn(inner)
				inner = nil
				if owner != nil {
					owner.release()
				}
				t.Fatal("decrypted owner was not converted from its exact promise")
			}
			owner.receivePack.TransferFrameBytes = inner
			inner = nil
			MessagePoolReturn(outer)
			outer = nil
			outerDecoded.release()
			owner.receivePack.messagePoolReturn()
			if sequence.holdPromise.reservedByteCount != scan.retainedByteCount || root.Available() != 0 {
				t.Fatal("refused pending decode did not restore only its exact funded promise")
			}
			sequence.releaseHoldPromise()
			root.Release(foreign)
			foreign = 0
			client.releaseReceiveDecodeMemory()
			if root.Stats().UsedByteCount != 0 {
				t.Fatal("decrypt/owner/scratch terminal accounting did not balance")
			}
		}()
	}
}

// NoAck traffic shares a lane but never discharges its reliable prefix. Even
// sequence number zero must get fresh bytes, leaving a full promised head paid.
func TestReceiveMemoryNoAckCannotBorrowReliablePrefix(t *testing.T) {
	assertMessagePoolOwnership(t)
	path := TransferPath{SourceId: NewId(), DestinationId: NewId()}
	messageId, sequenceId := NewId(), NewId()
	encode := func(nack bool) []byte {
		frame := sendPackFrame{messageId: messageId, sequenceId: sequenceId, sequenceNumber: 0, nack: nack,
			forceStream: true, companionContract: true, logicalLane: 3}
		return MessagePoolCopy(receiveMemoryScanTestWire(path, frame.appendPack(nil), false))
	}
	noAck, head := encode(true), encode(false)
	defer func() { MessagePoolReturn(noAck); MessagePoolReturn(head) }()
	scan, status := scanReceiveMemoryPack(noAck, path)
	headScan, headStatus := scanReceiveMemoryPack(head, path)
	if status != receiveMemoryPackScanReady || !scan.nack || !scan.loanSafe || scan.sequenceNumber != 0 || headStatus != receiveMemoryPackScanReady || scan.retainedByteCount != headScan.retainedByteCount {
		t.Fatal("NoAck/reliable zero-sequence premise changed")
	}
	cost := scan.retainedByteCount
	root, rx, buffer, sequence := receiveMemoryTestState(t, cost)
	sequence.nextSequenceNumber = 0
	sequence.transferKey = TransferKey{ForceStream: true, CompanionContract: true, LogicalLane: 3}
	buffer.receiveSequences = map[receiveSequenceId]*ReceiveSequence{{Source: path.SourceMask(), SequenceId: sequenceId, EncryptionRole: sequenceTlsRoleClient, LogicalLane: 3}: sequence}
	settings := DefaultClientSettings()
	settings.ReceiveBufferSettings = sequence.receiveBufferSettings
	settings.EncryptionSettings.Mode = EncryptionModeOff
	client := &Client{settings: settings, receiveBuffer: buffer, log: DefaultLogger()}
	if !sequence.reserveHoldPromise(cost) {
		t.Fatal("reliable prefix was not prepaid")
	}
	sequence.settleHoldPromise(true, 1, cost)
	decoded := &decodedTransferFrame{}
	ok, pressure := client.unmarshalReceiveMemoryFrame(noAck, decoded, false, path, false, sequenceTlsRoleServer, false)
	if ok || !pressure || decoded.packOwner != nil || sequence.holdPromise.reservedByteCount != cost || sequence.holdPromise.ingressLoanByteCount != 0 || root.Stats().UsedByteCount != cost {
		MessagePoolReturn(noAck)
		noAck = nil
		decoded.release()
		t.Fatal("NoAck consumed reliable-prefix custody")
	}
	decoded.release()
	ok, pressure = client.unmarshalReceiveMemoryFrame(head, decoded, false, path, false, sequenceTlsRoleServer, false)
	if !ok || pressure {
		MessagePoolReturn(head)
		head = nil
		decoded.release()
		t.Fatal("NoAck refusal spent the reliable head's existing permission")
	}
	owner := decoded.detachPackOwner()
	decoded.release()
	if owner == nil || owner.receiveItem.memoryBudget != rx || owner.receivePack.ingressLoanByteCount != cost {
		MessagePoolReturn(head)
		head = nil
		if owner != nil {
			owner.release()
		}
		t.Fatal("reliable zero head did not consume its exact claim")
	}
	owner.receivePack.TransferFrameBytes = head
	head = nil
	owner.receivePack.messagePoolReturn()
	if sequence.holdPromise.reservedByteCount != cost || root.Stats().UsedByteCount != cost {
		t.Fatal("pending reliable return did not restore exact promise")
	}
}

// A fresh NoAck owner must not occupy the reliable original's collision domain.
// The legacy cell restores only the old registration operation under its real
// lock; the fixed cell uses selected decode/bind/adopt without that restoration.
func TestReceiveMemoryNoAckOwnerDoesNotBlockPromisedHead(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, legacyBinding := range []bool{true, false} {
		func() {
			path := TransferPath{SourceId: NewId(), DestinationId: NewId()}
			sequenceId, noAckId, headId := NewId(), NewId(), NewId()
			encode := func(id Id, nack bool) []byte {
				frame := sendPackFrame{messageId: id, sequenceId: sequenceId, sequenceNumber: 0, nack: nack,
					forceStream: true, companionContract: true, logicalLane: 3}
				return MessagePoolCopy(receiveMemoryScanTestWire(path, frame.appendPack(nil), false))
			}
			noAckWire, headWire := encode(noAckId, true), encode(headId, false)
			defer func() { MessagePoolReturn(noAckWire); MessagePoolReturn(headWire) }()
			scan, status := scanReceiveMemoryPack(noAckWire, path)
			headScan, headStatus := scanReceiveMemoryPack(headWire, path)
			if status != receiveMemoryPackScanReady || !scan.nack || !scan.loanSafe || headStatus != receiveMemoryPackScanReady || headScan.nack || !headScan.loanSafe || scan.retainedByteCount != headScan.retainedByteCount {
				t.Fatal("coexisting zero-original premise changed")
			}
			cost := scan.retainedByteCount
			root, rx, buffer, sequence := receiveMemoryTestState(t, 2*cost)
			sequence.nextSequenceNumber = 0
			sequence.transferKey = TransferKey{ForceStream: true, CompanionContract: true, LogicalLane: 3}
			buffer.receiveSequences = map[receiveSequenceId]*ReceiveSequence{{Source: path.SourceMask(), SequenceId: sequenceId, EncryptionRole: sequenceTlsRoleClient, LogicalLane: 3}: sequence}
			settings := DefaultClientSettings()
			settings.ReceiveBufferSettings = sequence.receiveBufferSettings
			settings.EncryptionSettings.Mode = EncryptionModeOff
			client := &Client{settings: settings, receiveBuffer: buffer, log: DefaultLogger()}
			if !sequence.reserveHoldPromise(cost) {
				t.Fatal("coexisting reliable prefix was not prepaid")
			}
			sequence.settleHoldPromise(true, 1, cost)
			var noAckOwner, headOwner *decodedPackOwner
			defer func() {
				if headOwner != nil {
					headOwner.receivePack.messagePoolReturn()
				}
				if noAckOwner != nil {
					noAckOwner.receivePack.messagePoolReturn()
				}
			}()
			decode := func(wire *[]byte) (*decodedPackOwner, bool, bool) {
				decoded := &decodedTransferFrame{}
				ok, pressure := client.unmarshalReceiveMemoryFrame(*wire, decoded, false, path, false, sequenceTlsRoleServer, false)
				owner := decoded.detachPackOwner()
				decoded.release()
				if owner != nil {
					owner.receivePack.TransferFrameBytes = *wire
					*wire = nil
				}
				return owner, ok, pressure
			}
			var ok, pressure bool
			noAckOwner, ok, pressure = decode(&noAckWire)
			if !ok || pressure || noAckOwner == nil || noAckOwner.receiveItem.memoryBudget != rx || !sequence.bindReceiveMemoryOwner(&noAckOwner.receivePack) {
				t.Fatal("fresh NoAck did not acquire its independent physical claim")
			}
			noAckOwner.receiveItem.decodedOwner = noAckOwner
			sequence.adoptReceiveMemoryOwner(&noAckOwner.receiveItem)
			if noAckOwner.receivePack.ingressLoanByteCount != 0 {
				t.Fatal("fresh NoAck borrowed reliable-prefix credit")
			}
			if legacyBinding {
				root.admissionLock.Lock()
				sequence.registerReceiveIngressWithLock(&noAckOwner.receivePack)
				root.admissionLock.Unlock()
			}
			if root.Stats().UsedByteCount != 2*cost || root.Available() != 0 || sequence.holdPromise.reservedByteCount != cost {
				t.Fatal("NoAck plus reliable promise do not exactly fill the root")
			}
			headOwner, ok, pressure = decode(&headWire)
			if legacyBinding {
				if ok || !pressure || headOwner != nil || noAckOwner.receiveItem.memoryBudget != rx || noAckOwner.receiveItem.queueByteCount != cost || sequence.holdPromise.reservedByteCount != cost || sequence.holdPromise.ingressLoanByteCount != 0 {
					t.Fatal("synthetic old binding did not isolate its unchanged-credit head refusal")
				}
				noAckOwner.receivePack.messagePoolReturn()
				noAckOwner = nil
				headOwner, ok, pressure = decode(&headWire)
			}
			if !ok || pressure || headOwner == nil || headOwner.receiveItem.messageId != headId || headOwner.receiveItem.sequenceNumber != 0 || headOwner.receiveItem.memoryBudget != rx || headOwner.receivePack.ingressLoanByteCount != cost || sequence.holdPromise.reservedByteCount != 0 {
				t.Fatal("paid reliable head was blocked by independent NoAck custody or used fresh bytes")
			}
			if !legacyBinding && noAckOwner.receivePack.ingressSequence != nil {
				t.Fatal("live NoAck remained in reliable pending identity")
			}
			if !sequence.bindReceiveMemoryOwner(&headOwner.receivePack) {
				t.Fatal("genuine reliable original failed exact generation binding")
			}
			headOwner.receivePack.messagePoolReturn()
			headOwner = nil
			if sequence.holdPromise.reservedByteCount != cost || sequence.holdPromise.ingressLoanByteCount != 0 {
				t.Fatal("reliable pending return changed its original promise")
			}
			if noAckOwner != nil {
				noAckOwner.receivePack.messagePoolReturn()
				noAckOwner = nil
			}
			if sequence.holdPromise.ingressHead != nil || rx.Stats().UsedByteCount != cost || root.Stats().UsedByteCount != cost || rx.Stats().QuotaByteCount != 0 {
				t.Fatal("coexisting originals did not retire their independent ownership")
			}
			sequence.releaseHoldPromise()
			if rx.Stats().UsedByteCount != 0 || root.Stats().UsedByteCount != 0 {
				t.Fatal("coexisting prefix did not return exact final bytes")
			}
			t.Logf("[rx-noack-coexistence] legacy_binding=%t cost=%d same_head_retry=%t root_balanced=true", legacyBinding, cost, legacyBinding)
		}()
	}
}
