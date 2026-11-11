// Real decoder owners isolate the paid-byte handoff exception. These guards
// grant no contract, delivery or ACK authority; the signed burst tests exercise
// those effects through actual Clients and sender retry ownership.
package connect

import (
	"bytes"
	"context"
	"testing"

	"github.com/urnetwork/connect/protocol"
)

// Immutable test geometry separates physical ownership, encoded permission,
// local count/channel capacity and the legacy external protobuf body.
type paidPackByteGuardSettings struct {
	mode           string
	count          int
	physicalSlots  int
	quotaSlots     int
	retainedQuota  bool
	inheritH1Bytes bool
}

// Only this fixture owns unqueued Packs. Successful Pack admission moves them
// to the real channel; Close drains that channel before the budget census.
type paidPackByteGuardFixture struct {
	t                 *testing.T
	root, client, rx  *TransferMemoryBudget
	pack, quotaParent *TransferMemoryBudget
	otherRoot         *TransferMemoryBudget
	buffer            *ReceiveBuffer
	sequence          *ReceiveSequence
	path              TransferPath
	sequenceId        Id
	legacy            bool
	unowned           bool
	encoded, physical ByteCount
	callers           map[*ReceivePack]bool
	number            uint64
}

// The carrier is caller-owned. Parsing and decoding use the same canonical
// raw consumer frame and the same complete preflight cost as Client ingress.
func paidPackByteGuardWire(t *testing.T, path TransferPath, sequenceId Id, number uint64, legacy bool) ([]byte, receiveMemoryPackScan) {
	t.Helper()
	packet := ipOosTcpPacketSequence(reliableDeliveryTestPath(4), tcpFlagAck, 17,
		bytes.Repeat([]byte{0x71}, 1240))
	frame := &protocol.Frame{MessageType: protocol.MessageType_IpIpPacketFromProvider,
		Raw: true, MessageBytes: packet}
	body := (&sendPackFrame{messageId: NewId(), sequenceId: sequenceId,
		sequenceNumber: number, frames: []*protocol.Frame{frame}}).appendPack(nil)
	wire := MessagePoolCopy(receiveMemoryScanTestWire(path, body, legacy))
	scan, status := scanReceiveMemoryPack(wire, path)
	if status != receiveMemoryPackScanReady || scan.consumerFrameCount != 1 ||
		scan.legacy != legacy || scan.retainedByteCount < ByteCount(len(wire)) {
		MessagePoolReturn(wire)
		t.Fatal("paid-byte guard lacks an actual complete decoder envelope")
	}
	return wire, scan
}

// Every setting is installed before constructing the sequence. No worker runs;
// the tests move actual channel ownership explicitly and never fake an ACK.
func newPaidPackByteGuardFixture(t *testing.T, geometry paidPackByteGuardSettings) *paidPackByteGuardFixture {
	t.Helper()
	f := &paidPackByteGuardFixture{t: t,
		path:       TransferPath{SourceId: NewId(), DestinationId: NewId()},
		sequenceId: NewId(), legacy: geometry.mode == "legacy",
		unowned: geometry.mode == "unowned", callers: map[*ReceivePack]bool{}}
	wire, scan := paidPackByteGuardWire(t, f.path, f.sequenceId, 1, f.legacy)
	f.encoded, f.physical = ByteCount(len(wire)), scan.retainedByteCount
	MessagePoolReturn(wire)
	f.root = NewTransferMemoryBudget(ByteCount(geometry.physicalSlots) * f.physical)
	f.client = NewTransferMemoryBudgetWithParent(f.root.TotalByteCount(), f.root)
	f.rx = NewTransferMemoryBudgetWithParent(f.root.TotalByteCount(), f.client)
	quota := ByteCount(geometry.quotaSlots) * f.encoded
	if geometry.retainedQuota {
		quota = ByteCount(geometry.quotaSlots) * f.physical
	}
	switch geometry.mode {
	case "no-pack-budget":
	case "distinct-root":
		f.otherRoot = NewTransferMemoryBudget(8 * f.physical)
		f.pack = NewTransferMemoryBudgetWithParent(quota, f.otherRoot)
	case "quota-ancestor":
		f.quotaParent = NewTransferMemoryBudgetWithParent(quota, f.root)
		f.pack = NewTransferMemoryBudgetWithParent(8*f.physical, f.quotaParent)
	default:
		f.pack = NewTransferMemoryBudgetWithParent(quota, f.client)
	}
	settings := DefaultReceiveBufferSettingsWithBufferSize(geometry.count)
	settings.ReceiveQueueBudget = f.rx
	settings.ReceiveQueueRetainedByteAccounting = true
	settings.ReceiveQueueMinByteCount = 0
	settings.PackQueueBudget = f.pack
	settings.PackQueueRetainedByteAccounting = geometry.retainedQuota
	settings.SequenceBufferByteCount = f.encoded + f.encoded/2
	settings.H1SequenceBufferByteCount = settings.SequenceBufferByteCount
	if geometry.inheritH1Bytes {
		settings.H1SequenceBufferByteCount = 0
	}
	settings.H1SequenceBufferSize = geometry.count
	settings.H1SequenceBufferAdaptiveMaxSize = 0
	f.buffer = &ReceiveBuffer{receiveBufferSettings: settings}
	ctx, cancel := context.WithCancel(t.Context())
	f.sequence = newReceiveSequence(ctx, &Client{log: NewNoopLogger()},
		f.path.SourceMask(), f.sequenceId, TransferKey{}, settings)
	t.Cleanup(func() {
		cancel()
		f.sequence.Close()
		for pack := range f.callers {
			pack.messagePoolReturn()
			delete(f.callers, pack)
		}
		f.sequence.releaseHoldPromise()
		if f.sequence.packQueueCount.Load() != 0 || f.sequence.packQueueByteCount.Load() != 0 ||
			f.sequence.holdPromise.ingressHead != nil {
			t.Error("paid-byte guard retained channel or ingress ownership after close")
		}
		for _, budget := range []*TransferMemoryBudget{f.pack, f.quotaParent, f.rx, f.client, f.root, f.otherRoot} {
			if budget == nil {
				continue
			}
			stats := budget.Stats()
			if stats.UsedByteCount != 0 || stats.QuotaByteCount != 0 || stats.ReservedByteCount != stats.ReleasedByteCount {
				t.Errorf("paid-byte guard leaked an actual budget: %+v", stats)
			}
		}
	})
	return f
}

// Materialize only after the real RX claim succeeds. The external legacy body
// uses the maintained protobuf decoder and keeps its funded owner for return.
func (f *paidPackByteGuardFixture) original() *ReceivePack {
	f.t.Helper()
	f.number++
	wire, scan := paidPackByteGuardWire(f.t, f.path, f.sequenceId, f.number, f.legacy)
	if ByteCount(len(wire)) != f.encoded || scan.retainedByteCount != f.physical {
		MessagePoolReturn(wire)
		f.t.Fatal("paid-byte guard changed its fixed wire geometry")
	}
	if f.unowned {
		pack := &ReceivePack{TransferFrameBytes: wire, MessageByteCount: scan.messageByteCount,
			TransportType: TransportTypeH1}
		f.callers[pack] = true
		return pack
	}
	owner := f.buffer.takeReceiveMemoryOwner(f.sequence, scan.messageId, scan.sequenceNumber, scan.retainedByteCount)
	if owner == nil {
		MessagePoolReturn(wire)
		f.t.Fatal("paid-byte guard failed its independently bounded physical reservation")
	}
	pack := &owner.receivePack
	pack.TransferFrameBytes = wire
	f.callers[pack] = true
	body := wire[scan.packOffset : scan.packOffset+scan.packByteCount]
	if f.legacy {
		pack.Pack = &protocol.Pack{}
		if err := ProtoUnmarshal(body, pack.Pack); err != nil {
			f.t.Fatal(err)
		}
	} else {
		if decoded, ok := decodePackOwned(body, owner); !ok || decoded != owner {
			f.t.Fatal("paid-byte guard lost its actual owned decoder")
		}
		pack.Pack = &owner.pack
	}
	pack.Source, pack.SequenceId = f.path.SourceMask(), f.sequenceId
	pack.MessageByteCount = MessageByteCount(pack.Pack.Frames)
	pack.TransportType = TransportTypeH1
	owner.receiveItem.decodedOwner = owner
	return pack
}

// Preserve the API's takes-on-success boundary, including failed guards.
func (f *paidPackByteGuardFixture) offer(pack *ReceivePack) (bool, error) {
	f.t.Helper()
	accepted, err := f.sequence.Pack(pack, 0)
	if accepted {
		delete(f.callers, pack)
	}
	return accepted, err
}

// Physical and encoded quota are different quantities, charged once each.
// An already full physical root cannot deny its independently paid handoff.
func TestReceivePaidPackBytesUsesSharedQuotaAtFullPhysicalRoot(t *testing.T) {
	assertMessagePoolOwnership(t)
	f := newPaidPackByteGuardFixture(t, paidPackByteGuardSettings{count: 4, physicalSlots: 2, quotaSlots: 2})
	first, second := f.original(), f.original()
	if f.root.Available() != 0 || f.rx.UsedByteCount() != 2*f.physical {
		t.Fatal("paid-byte guard did not actually fill its physical root")
	}
	for _, pack := range []*ReceivePack{first, second} {
		if accepted, err := f.offer(pack); !accepted || err != nil {
			t.Fatalf("fully funded canonical Pack was refused: accepted=%t error=%v", accepted, err)
		}
	}
	if f.sequence.packQueueByteCount.Load() != 2*f.encoded ||
		f.sequence.packQueueByteCount.Load() <= f.sequence.packQueueH1ByteLimit ||
		f.sequence.packQueueCount.Load() != 2 || len(f.sequence.packs) != 2 ||
		f.pack.UsedByteCount() != 0 || f.pack.Stats().QuotaByteCount != 2*f.encoded ||
		f.root.UsedByteCount() != 2*f.physical || f.client.UsedByteCount() != 2*f.physical {
		t.Fatal("paid-byte exception lost encoded accounting or double charged the physical owner")
	}
	if extra := f.buffer.takeReceiveMemoryOwner(f.sequence, NewId(), 3, f.physical); extra != nil {
		extra.release()
		t.Fatal("paid-byte exception expanded the physical root")
	}
	dequeued := <-f.sequence.packs
	f.sequence.releasePackQueue(dequeued)
	if f.root.UsedByteCount() != 2*f.physical || f.pack.Stats().QuotaByteCount != f.encoded {
		t.Fatal("handoff dequeue released physical roots before their real return")
	}
	dequeued.messagePoolReturn()
	if f.root.UsedByteCount() != f.physical || f.sequence.packQueueByteCount.Load() != f.encoded {
		t.Fatal("real owner return did not release exactly its own physical cost")
	}
}

// An accepted byte exception does not deepen the fixed per-sequence count.
func TestReceivePaidPackBytesKeepsCountBound(t *testing.T) {
	assertMessagePoolOwnership(t)
	f := newPaidPackByteGuardFixture(t, paidPackByteGuardSettings{count: 2, physicalSlots: 4, quotaSlots: 4})
	first, second, third := f.original(), f.original(), f.original()
	for _, pack := range []*ReceivePack{first, second} {
		if accepted, err := f.offer(pack); !accepted || err != nil {
			t.Fatal("paid prefix did not reach its exact count bound")
		}
	}
	before := f.pack.Stats()
	if accepted, err := f.offer(third); accepted || err != nil || f.pack.Stats() != before ||
		f.sequence.packQueueCount.Load() != 2 || f.sequence.packQueueByteCount.Load() != 2*f.encoded ||
		f.root.UsedByteCount() != 3*f.physical || f.root.Available() < f.physical ||
		f.sequence.client.ReceiveStats().PackHandoffDepthGrowCount != 0 {
		t.Fatal("paid byte permission bypassed count, changed quota, or grew the queue")
	}
}

// An unbuffered direct API still requires an actual receiver immediately.
func TestReceivePaidPackBytesKeepsChannelReadiness(t *testing.T) {
	assertMessagePoolOwnership(t)
	f := newPaidPackByteGuardFixture(t, paidPackByteGuardSettings{count: 0, physicalSlots: 2, quotaSlots: 2})
	pack := f.original()
	if cap(f.sequence.packs) != 0 {
		t.Fatal("channel refusal guard did not construct its unbuffered API")
	}
	if accepted, err := f.offer(pack); accepted || err != nil || f.pack.Stats().QuotaByteCount != 0 ||
		f.sequence.packQueueCount.Load() != 0 || f.sequence.packQueueByteCount.Load() != 0 ||
		f.root.UsedByteCount() != f.physical || pack.sequenceQueueReserved {
		t.Fatal("failed channel handoff leaked permission or consumed caller custody")
	}
}

// A hot sequence cannot spend past either its shared Pack leaf or an
// exclusive ancestor, even though both originals have real physical backing.
func TestReceivePaidPackBytesKeepsSharedQuotaBounds(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, mode := range []string{"shared", "quota-ancestor"} {
		f := newPaidPackByteGuardFixture(t, paidPackByteGuardSettings{mode: mode, count: 4, physicalSlots: 4, quotaSlots: 1})
		first, second := f.original(), f.original()
		if accepted, err := f.offer(first); !accepted || err != nil {
			t.Fatalf("%s: first paid Pack was refused", mode)
		}
		before := f.pack.Stats()
		if accepted, err := f.offer(second); accepted || err != nil || f.pack.Stats() != before ||
			f.sequence.packQueueCount.Load() != 1 || f.sequence.packQueueByteCount.Load() != f.encoded ||
			f.root.UsedByteCount() != 2*f.physical || second.sequenceQueueReserved {
			t.Fatalf("%s: paid-byte exception bypassed shared quota or lost caller ownership", mode)
		}
		if f.quotaParent != nil && f.quotaParent.Stats().QuotaByteCount != f.encoded {
			t.Fatal("target-exclusive ancestor quota changed on refusal")
		}
	}
}

// The public encoded limit remains exact for all excluded ownership models.
func TestReceivePaidPackBytesKeepsUnownedLegacyAndIndependentLimits(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, mode := range []string{"unowned", "legacy", "no-pack-budget", "distinct-root"} {
		f := newPaidPackByteGuardFixture(t, paidPackByteGuardSettings{mode: mode, count: 4, physicalSlots: 4, quotaSlots: 4})
		first, second := f.original(), f.original()
		if accepted, err := f.offer(first); !accepted || err != nil {
			t.Fatalf("%s: legacy bounded prefix refused", mode)
		}
		physical := f.root.UsedByteCount()
		if accepted, err := f.offer(second); accepted || err != nil ||
			f.sequence.packQueueCount.Load() != 1 || f.sequence.packQueueByteCount.Load() != f.encoded ||
			f.root.UsedByteCount() != physical || second.sequenceQueueReserved {
			t.Fatalf("%s: excluded ownership model escaped its encoded byte bound", mode)
		}
		if f.pack != nil {
			stats := f.pack.Stats()
			if mode == "legacy" && (stats.UsedByteCount != 0 || stats.QuotaByteCount != f.encoded) ||
				mode != "legacy" && (stats.UsedByteCount != f.encoded || stats.QuotaByteCount != 0) {
				t.Fatalf("%s: excluded model changed its original Pack accounting", mode)
			}
		}
	}
}

// Proof-copy metadata may supply an encoded length larger than its retained
// owner. Such a claim is not fully backed and keeps the local byte ceiling.
func TestReceivePaidPackBytesKeepsLargerLogicalClaimBounded(t *testing.T) {
	assertMessagePoolOwnership(t)
	f := newPaidPackByteGuardFixture(t, paidPackByteGuardSettings{count: 4, physicalSlots: 4, quotaSlots: 8})
	first, second := f.original(), f.original()
	first.sequenceQueueByteCount = f.physical + 1
	second.sequenceQueueByteCount = f.physical + 1
	if accepted, err := f.offer(first); !accepted || err != nil {
		t.Fatal("one oversized logical original lost its existing empty-queue allowance")
	}
	if accepted, err := f.offer(second); accepted || err != nil ||
		f.sequence.packQueueByteCount.Load() != f.physical+1 || f.sequence.packQueueCount.Load() != 1 ||
		f.pack.Stats().QuotaByteCount != f.physical+1 || f.root.UsedByteCount() != 2*f.physical {
		t.Fatal("uncovered encoded claim escaped its local ceiling")
	}
}

// The optional retained-cost quota uses its original unit, independently of
// the encoded census and physical lifetime; byte permission cannot change it.
func TestReceivePaidPackBytesKeepsRetainedQuotaUnit(t *testing.T) {
	assertMessagePoolOwnership(t)
	f := newPaidPackByteGuardFixture(t, paidPackByteGuardSettings{count: 4, physicalSlots: 4, quotaSlots: 2, retainedQuota: true})
	first, second, third := f.original(), f.original(), f.original()
	for _, pack := range []*ReceivePack{first, second} {
		if accepted, err := f.offer(pack); !accepted || err != nil {
			t.Fatal("retained quota could not admit its paid prefix")
		}
	}
	if accepted, err := f.offer(third); accepted || err != nil ||
		f.pack.Stats().QuotaByteCount != 2*f.physical || f.pack.UsedByteCount() != 0 ||
		f.sequence.packQueueByteCount.Load() != 2*f.encoded || f.root.UsedByteCount() != 3*f.physical {
		t.Fatal("paid-byte exception changed retained quota units or its hard ceiling")
	}
}

// Closure remains earlier than count/quota admission and never consumes an
// already paid caller owner. No lifecycle permission comes from physical debt.
func TestReceivePaidPackBytesKeepsClosedCarrierAndCancellation(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, carrierClosed := range []bool{false, true} {
		f := newPaidPackByteGuardFixture(t, paidPackByteGuardSettings{count: 4, physicalSlots: 2, quotaSlots: 2})
		pack := f.original()
		if carrierClosed {
			done := make(chan struct{})
			close(done)
			pack.carrierDone = done
		} else {
			f.sequence.Cancel()
		}
		if accepted, err := f.offer(pack); accepted || err == nil ||
			f.sequence.packQueueCount.Load() != 0 || f.sequence.packQueueByteCount.Load() != 0 ||
			f.pack.Stats().QuotaByteCount != 0 || f.root.UsedByteCount() != f.physical || pack.sequenceQueueReserved {
			t.Fatal("closed handoff changed lifetime, quota or caller custody")
		}
	}
}

// A paid owner alone cannot consume the H1 exception on another carrier.
// The same owned original can subsequently arrive over H1 without recharging.
func TestReceivePaidPackBytesKeepsTransportIsolation(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, inherit := range []bool{false, true} {
		for _, transport := range []TransportType{
			TransportTypeH3, TransportTypeH3Dns, TransportTypeH3DnsPump,
			TransportTypeP2p, TransportTypeUnknown,
		} {
			f := newPaidPackByteGuardFixture(t, paidPackByteGuardSettings{
				count: 4, physicalSlots: 3, quotaSlots: 3, inheritH1Bytes: inherit,
			})
			first, second := f.original(), f.original()
			if accepted, err := f.offer(first); !accepted || err != nil {
				t.Fatalf("transport isolation first owner: accepted=%t error=%v", accepted, err)
			}
			second.TransportType = transport
			rootBefore, rxBefore, quotaBefore := f.root.Stats(), f.rx.Stats(), f.pack.Stats()
			owner := second.decodedOwner
			if rootBefore.TotalByteCount-rootBefore.UsedByteCount < f.physical ||
				quotaBefore.TotalByteCount-quotaBefore.QuotaByteCount < f.encoded ||
				f.sequence.packQueueBaseByteLimit >= 2*f.encoded {
				t.Fatal("transport isolation lacks physical/quota room or byte pressure")
			}
			if accepted, err := f.offer(second); accepted || err != nil ||
				f.root.Stats() != rootBefore || f.rx.Stats() != rxBefore || f.pack.Stats() != quotaBefore ||
				second.decodedOwner != owner || !second.receiveMemoryOwned || second.sequenceQueueReserved ||
				f.sequence.packQueueCount.Load() != 1 || f.sequence.packQueueByteCount.Load() != f.encoded ||
				len(f.sequence.packs) != 1 {
				t.Fatalf("paid non-H1 owner bypassed its byte cap or changed custody: transport=%s inherited=%t accepted=%t error=%v", transport, inherit, accepted, err)
			}
			second.TransportType = TransportTypeH1
			if accepted, err := f.offer(second); !accepted || err != nil ||
				f.root.Stats() != rootBefore || f.rx.Stats() != rxBefore ||
				f.pack.Stats().QuotaByteCount != 2*f.encoded ||
				f.sequence.packQueueCount.Load() != 2 || f.sequence.packQueueByteCount.Load() != 2*f.encoded ||
				second.decodedOwner != owner || len(f.sequence.packs) != 2 {
				t.Fatalf("same paid H1 owner lost its explicit or inherited exception: transport=%s inherited=%t accepted=%t error=%v", transport, inherit, accepted, err)
			}
		}
	}
}
