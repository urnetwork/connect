// Controlled real Client ingress exposes owned slab lifetime independently
// of queue counters. Stage barriers borrow only while the actual worker is
// stopped outside callbacks/locks; no test share keeps a retired buffer alive.
package connect

import (
	"context"
	"testing"
	"testing/synctest"
	"time"
)

// Values only: no pooled byte slice or owner pointer survives a stage hook.
type receiveMemoryLifecycleSample struct {
	Stage                                   string
	InUse, Identity, LiveRoots, RawPayloads bool
	Frames                                  int
	Carrier, Payload, Metadata, Cost        ByteCount
	Root, Client, Rx, Pack                  ByteCount
	PackCount                               int64
	PackEncoded, PackClaim                  ByteCount
	ItemClaim                               bool
	BufferedItems, BufferedFrames           int
	BufferedIdentity                        bool
	NewPoolRoots                            int64
	NewPoolBytes                            ByteCount
}

// Fixed channels give each requested boundary one stop/release. Cancellation
// cleanup releases every stop before the fixture joins its real workers.
type receiveMemoryLifecycleStage struct {
	Name    string
	Entered chan receiveMemoryLifecycleSample
	Resume  chan struct{}
}

type receiveMemoryLifecycleGate struct {
	root, client, rx, pack *TransferMemoryBudget
	target                 Id
	beforeRoots            uint64
	beforeBytes            ByteCount
	stages                 []*receiveMemoryLifecycleStage
}

// Stage storage is constant for this one original; each call borrows owner.
func (self *receiveMemoryLifecycleGate) observe(sequence *ReceiveSequence, owner *decodedPackOwner, stage string) {
	var stop *receiveMemoryLifecycleStage
	for _, candidate := range self.stages {
		if candidate.Name == stage {
			stop = candidate
			break
		}
	}
	if stop == nil {
		return
	}
	stop.Entered <- self.sample(sequence, owner, stage)
	<-stop.Resume
}

// The caller exclusively pauses the actual owner while scalar state is copied.
func (self *receiveMemoryLifecycleGate) sample(sequence *ReceiveSequence, owner *decodedPackOwner, stage string) receiveMemoryLifecycleSample {
	sample := receiveMemoryLifecycleSample{Stage: stage}
	states := self.root.StatsWithDescendants(self.client, self.rx, self.pack)
	sample.Root, sample.Client, sample.Rx, sample.Pack = states[0].UsedByteCount, states[1].UsedByteCount, states[2].UsedByteCount, states[3].UsedByteCount
	if owner != nil && owner.inUse {
		sample.InUse = true
		id, err := IdFromBytes(owner.pack.MessageId)
		sample.Identity = err == nil && id == self.target
		sample.Frames = len(owner.pack.Frames)
		sample.LiveRoots = true
		sample.RawPayloads = true
		carrier := owner.receivePack.TransferFrameBytes
		if stage == "head_buffered" {
			carrier = owner.receiveItem.transferFrameBytes
		}
		pooled, _ := MessagePoolCheck(carrier)
		sample.LiveRoots = sample.LiveRoots && pooled
		sample.Carrier = MessagePoolRootByteCount(carrier)
		for _, frame := range owner.pack.Frames {
			if frame == nil {
				sample.LiveRoots = false
				sample.RawPayloads = false
				continue
			}
			pooled, _ := MessagePoolCheck(frame.MessageBytes)
			sample.LiveRoots = sample.LiveRoots && pooled
			sample.Payload += MessagePoolRootByteCount(frame.MessageBytes)
			packet, err := ipPacketFromProviderBytes(frame)
			sample.RawPayloads = sample.RawPayloads && err == nil && frame.Raw && len(packet) == 1280 && packet[len(packet)-1] == 0x42
		}
		sample.Metadata = owner.metadataQueueByteCount()
		sample.Cost = sample.Carrier + sample.Payload + sample.Metadata
		sample.PackEncoded = owner.receivePack.sequenceQueueByteCount
		sample.PackClaim = owner.receivePack.sequenceQueueBudgetByteCount
		sample.ItemClaim = owner.receiveItem.memoryBudget != nil
		if sequence != nil {
			sample.PackCount = sequence.packQueueCount.Load()
			sample.BufferedItems = len(sequence.deliverItems)
			sample.BufferedFrames = len(sequence.deliverFrames)
			sample.BufferedIdentity = sample.BufferedItems == 1 && sequence.deliverItems[0] == &owner.receiveItem
		}
	}
	roots, bytes := messagePoolOutstandingSnapshot()
	sample.NewPoolRoots = int64(roots) - int64(self.beforeRoots)
	sample.NewPoolBytes = bytes - self.beforeBytes
	return sample
}

// Only requested stages stop. All channels are bounded, and no new worker or
// progress-observer callback is used to hold production execution.
func newReceiveMemoryLifecycleGate(t *testing.T, flow *receiveMemoryLifecycleFlow, root, client, rx, pack *TransferMemoryBudget, names ...string) *receiveMemoryLifecycleGate {
	t.Helper()
	gate := &receiveMemoryLifecycleGate{root: root, client: client, rx: rx, pack: pack}
	for _, name := range names {
		gate.stages = append(gate.stages, &receiveMemoryLifecycleStage{Name: name, Entered: make(chan receiveMemoryLifecycleSample, 1), Resume: make(chan struct{})})
	}
	t.Cleanup(func() {
		for _, stage := range gate.stages {
			select {
			case <-stage.Resume:
			default:
				close(stage.Resume)
			}
		}
	})
	flow.fixture.receiver.receiveBuffer.receiveBufferSettings.receiveOwnerStageForTest = gate.observe
	return gate
}

// This existing before-Run seam parks the worker before its first dequeue.
// Queue inspection below then borrows/restores without a release or wake edge.
func pauseReceiveMemoryLifecycleRun(t *testing.T, flow *receiveMemoryLifecycleFlow) (<-chan struct{}, func()) {
	t.Helper()
	entered := make(chan struct{})
	resume := make(chan struct{})
	flow.fixture.receiver.receiveBuffer.beforeRunReceiveSequenceForTest = func(receiveSequenceId) { close(entered); <-resume }
	release := func() {
		select {
		case <-resume:
		default:
			close(resume)
		}
	}
	t.Cleanup(release)
	return entered, release
}

// The worker is parked and the one original is already admitted. Only channel
// position is borrowed/restored; its count/bytes/budget and FIFO order stay put.
func receiveMemoryLifecycleQueuedOwner(t *testing.T, flow *receiveMemoryLifecycleFlow, gate *receiveMemoryLifecycleGate) receiveMemoryLifecycleSample {
	t.Helper()
	synctest.Wait()
	resident := flow.fixture.receiveSequence()
	if len(resident.packs) != 1 {
		t.Fatal("actual paused handoff queue does not own exactly one original")
	}
	pack := <-resident.packs
	sample := gate.sample(resident, pack.decodedOwner, "pack_queued")
	resident.packs <- pack
	if len(resident.packs) != 1 {
		t.Fatal("borrow/restore changed actual handoff position")
	}
	return sample
}

// Establishes a real source original before any ingress allocation. No extra
// share or captured owner pointer is retained to create the later live bytes.
func (self *receiveMemoryLifecycleGate) before(t *testing.T, wire *windowRoundFrame) {
	t.Helper()
	id, err := IdFromBytes(wire.pack.MessageId)
	if err != nil {
		t.Fatal(err)
	}
	self.target = id
	self.beforeRoots, self.beforeBytes = messagePoolOutstandingSnapshot()
}

// The protocol-owned copied MTU slabs are live even if a future decoder
// removes the carrier retention. This is physical ownership, not just a quota.
func assertReceiveMemoryLifecycleOwner(t *testing.T, sample receiveMemoryLifecycleSample) {
	t.Helper()
	if !sample.InUse || !sample.Identity || !sample.LiveRoots || !sample.RawPayloads || sample.Frames != 8 ||
		sample.Payload != 8*2048 || sample.Carrier != 12288 || sample.Metadata <= 0 || sample.Cost != 30544 ||
		sample.NewPoolRoots < 8 || sample.NewPoolBytes < sample.Payload {
		t.Fatalf("actual owned-decoder/slab premise failed: %+v", sample)
	}
}

// Close real workers before checking once-only terminal and memory ownership.
// This does not change deliver-then-die behavior of already accepted heads.
func joinReceiveMemoryLifecycleFlow(t *testing.T, flow *receiveMemoryLifecycleFlow) {
	t.Helper()
	flow.fixture.cancel()
	ctx, cancel := context.WithTimeout(context.Background(), DefaultSendBufferSettings().WriteTimeout)
	defer cancel()
	for _, client := range []*Client{flow.fixture.sender, flow.fixture.receiver} {
		if err := client.CloseAndWait(ctx); err != nil {
			t.Fatal(err)
		}
	}
	for _, route := range []Route{flow.fixture.senderOut, flow.fixture.senderIn, flow.fixture.receiverOut, flow.fixture.receiverIn} {
		for len(route) != 0 {
			MessagePoolReturn(<-route)
		}
	}
	synctest.Wait()
	// R6-only adjacent tests additionally check the encoded quota. This common
	// witness also compiles with the historical pre-ownership decoder.
	if flow.fixture.ackedCount+flow.failed != flow.admitted {
		t.Fatal("join did not complete each accepted logical source owner once")
	}
}

// The same witness runs against old receive ownership and the fixed path.
// Report an uncharged live owner only after delivery, ACK, and cleanup succeed.
func TestReceiveMemoryOwnerClaimCoversDecodedLifetime(t *testing.T) {
	settings := receiveMemoryLifecycleTestSettings()
	assertMessagePoolOwnership(t)
	var witnesses []receiveMemoryLifecycleSample
	synctest.Test(t, func(t *testing.T) {
		rx, pack, client, root := receiveMemoryLifecycleBudgets(t, settings)
		flow := newReceiveMemoryLifecycleFlow(t, settings, rx, pack)
		gate := newReceiveMemoryLifecycleGate(t, flow, root, client, rx, pack, "pack_released", "head_buffered")
		entered, resume := pauseReceiveMemoryLifecycleRun(t, flow)
		wire := flow.group(t, 8)
		gate.before(t, wire)
		encoded := ByteCount(len(wire.bytes))
		flow.fixture.forward(wire, flow.fixture.receiverIn)
		<-entered
		queued := receiveMemoryLifecycleQueuedOwner(t, flow, gate)
		assertReceiveMemoryLifecycleOwner(t, queued)
		if queued.PackEncoded != encoded || queued.PackCount != 1 {
			t.Fatal("actual encoded handoff boundary changed")
		}
		witnesses = append(witnesses, queued)
		resume()
		synctest.Wait()
		for _, stage := range gate.stages {
			sample := <-stage.Entered
			synctest.Wait()
			assertReceiveMemoryLifecycleOwner(t, sample)
			if flow.fixture.ackedCount != 0 || flow.fixture.deliveredCount != 0 || len(flow.fixture.receiverOut) != 0 {
				t.Fatal("barrier crossed real callback or acknowledgement boundary")
			}
			if sample.PackEncoded != 0 || sample.PackCount != 0 {
				t.Fatalf("actual queue release boundary changed: %+v", sample)
			}
			if sample.Stage == "head_buffered" && (sample.BufferedItems != 1 || sample.BufferedFrames != 8 || !sample.BufferedIdentity) {
				t.Fatalf("actual in-order batch owner is not retained: %+v", sample)
			}
			witnesses = append(witnesses, sample)
			close(stage.Resume)
			synctest.Wait()
		}
		time.Sleep(2 * settings.ackCompressTimeout)
		flow.fixture.acknowledge()
		if flow.fixture.deliveredCount != 8 || flow.fixture.ackedCount != 1 || flow.failed != 0 || flow.fixture.sequence().resendQueue.Len() != 0 {
			t.Fatal("ordinary secured callback/cumulative retirement failed")
		}
		joinReceiveMemoryLifecycleFlow(t, flow)
		if root.UsedByteCount() != 0 {
			t.Fatal("terminal control leaked a receiver claim")
		}
	})
	if len(witnesses) != 3 {
		t.Fatal("incomplete actual owner lifetime")
	}
	for _, sample := range witnesses {
		t.Logf("[receive-memory-owner-lifetime] stage=%s owned=%d copied_payload_slabs=%d newly_live_pool_bytes=%d charged_root=%d item_claim=%t buffered_items=%d", sample.Stage, sample.Cost, sample.Payload, sample.NewPoolBytes, sample.Root, sample.ItemClaim, sample.BufferedItems)
	}
	for _, sample := range witnesses {
		if sample.Root < sample.Cost {
			t.Errorf("RX_RETAINED_OWNER_UNCHARGED stage=%s owned=%d root_claim=%d", sample.Stage, sample.Cost, sample.Root)
		}
	}
}
