// Two actual native send sources may finish their cache writes in the reverse
// order. The barrier retains real owned frames after disposition publication.
package connect

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
	"unsafe"

	"github.com/urnetwork/connect/protocol"
)

type nativeSynDispositionOrderEvent struct {
	source *SendSequence
	group  *parsedPacketGroup
	update *multiClientChannelUpdate
	bytes  int
}

// Physical provider observations retain only exact scalar wire identity.
type nativeSynDispositionWireEvent struct {
	provider string
	sequence uint32
	reset    bool
	bytes    int
}

// Exact identity sampled after retention, before the old sequence can dequeue
// a later reset/control and accidentally hide a transient stale cache write.
type nativeSynDispositionCacheEvent struct {
	owner  *tcpSynRetry
	credit *transferRetainedControlCredit
	live   bool
}

// Numeric permission follows the same production accounting helpers. One key
// publication remains unACKed in this fixture's absent platform path. A has one
// held native source, B may retain its three actually offered generations, and
// C may retain SYN plus its two healthy bodies until ACK compression fires.
// Each may also own one continuous ping. Two overlapping B cache generations
// include their separately paid readiness owners and delayed final disposal.
type nativeSynDispositionFunding struct {
	protocol                               int
	ordinary, group, cache, readiness      ByteCount
	readyCapacity, remoteCapacity, windows int
	children                               [3]ByteCount
	total                                  ByteCount
}

func nativeSynDispositionFundingFor(settings *MultiClientSettings) nativeSynDispositionFunding {
	round := func(bytes ByteCount) ByteCount { return (bytes + 63) / 64 * 64 }
	geometryParent := &RemoteUserNatMultiClient{settings: settings, windows: map[WindowType]*multiClientWindow{}}
	for kind := range settings.WindowSizes {
		geometryParent.windows[kind] = nil
	}
	geometryUpdate := &multiClientChannelUpdate{}
	readyCapacity := synRetryReadyCapacity(geometryParent, geometryUpdate)
	remoteCapacity := synRetryRemoteTailCapacity(geometryParent, geometryUpdate)
	path := reraceFlow(4, 171, 54401)
	// The largest fixed fixture payload is body-before (11B), exceeding the
	// five TFO bytes in the ABA control. This graph is sizing only, never sent.
	payload := []byte("body-before")
	packet := reraceTcpPacket(path, tcpFlagSyn, path.SequenceNumber, 0, payload)
	frame, err := ToFrame(&protocol.IpPacketToProvider{IpPacket: &protocol.IpPacket{PacketBytes: packet}}, settings.ProtocolVersion)
	if err != nil {
		panic(err)
	}
	defer MessagePoolReturn(frame.MessageBytes)
	source := &SendSequence{}
	ordinary := (&sendItem{}).retainedMemoryByteCount(source.retainedSendFrameByteCount(ByteCount(len(frame.MessageBytes)), sendPackBatchMaxFrames), sendPackBatchMaxFrames, settings.ProtocolVersion < 2)
	prototype := &parsedPacketGroup{
		packets: []parsedPacket{{packet: packet, ipPath: path, payload: payload, transportAttribution: &transportPacketAttribution{}}},
		ipPath:  path, synRetry: &tcpSynRetryRequest{}, retainedIpPathCount: 1,
		transportAttribution: &transportPacketAttribution{}, admissionObservations: &sendPackAdmissionObservations{},
	}
	prototype.completionFlags.Store(groupCompletionTracked | groupCompletionCandidate | groupCompletionCompactedMetadata)
	group := prototype.sendGroupMemoryByteCount()
	overhead := transferRetainedControlCreditByteCount()
	cache := addReceiveQueueByteCount(retainedMessageCapacity(ByteCount(len(packet))), round(ByteCount(unsafe.Sizeof(tcpSynRetry{}))))
	cache = addReceiveQueueByteCount(cache, retainedMessageCapacity(ByteCount(remoteCapacity)*ByteCount(unsafe.Sizeof(Id{}))))
	cache = addReceiveQueueByteCount(cache, overhead)
	readiness := addReceiveQueueByteCount(retainedMessageCapacity(ByteCount(unsafe.Sizeof(tcpSynRetryReadiness{}))), retainedMessageCapacity(ByteCount(readyCapacity)*ByteCount(unsafe.Sizeof(tcpSynRetryReadySlot{}))))
	readiness = addReceiveQueueByteCount(readiness, overhead)
	funding := nativeSynDispositionFunding{protocol: settings.ProtocolVersion, ordinary: ordinary, group: group, cache: cache, readiness: readiness, readyCapacity: readyCapacity, remoteCapacity: remoteCapacity, windows: len(geometryParent.windows)}
	nativeSlots := [3]int{1, 3, 3}
	cacheSlots := [3]int{1, 2, 1}
	for index := range funding.children {
		// One real control publisher (encryption is Off), the bounded native
		// cohort plus one continuous ping, and exact cache/readiness overlap.
		child := ordinary
		child = addReceiveQueueByteCount(child, ByteCount(nativeSlots[index]+1)*addReceiveQueueByteCount(ordinary, group))
		child = addReceiveQueueByteCount(child, ByteCount(cacheSlots[index])*addReceiveQueueByteCount(cache, readiness))
		funding.children[index] = child
		funding.total = addReceiveQueueByteCount(funding.total, child)
	}
	return funding
}

// Real initial-ping admission and quiescent owner snapshots precede the first
// application SYN. Unrelated key controls remain explicit funded owners.
func (self *nativeSynDispositionOrderFixture) requireFundedBootstrap(t *testing.T) {
	t.Helper()
	self.h.settle()
	if self.funding.total <= 0 || self.root.TotalByteCount() != self.funding.total ||
		len(self.h.generator.clientList()) != 3 || len(self.h.liveChannels()) != 3 || len(self.h.multiClient.windows) != self.funding.windows {
		t.Fatal("funded disposition fixture lost its exact real three-client bootstrap or root geometry")
	}
	geometryUpdate := &multiClientChannelUpdate{}
	if synRetryReadyCapacity(self.h.multiClient, geometryUpdate) != self.funding.readyCapacity || synRetryRemoteTailCapacity(self.h.multiClient, geometryUpdate) != self.funding.remoteCapacity {
		t.Fatal("actual bootstrap window geometry differs from its funded readiness/remote-tail capacities")
	}
	self.h.multiClient.stateLock.Lock()
	flows := len(self.h.multiClient.flowUpdates)
	self.h.multiClient.stateLock.Unlock()
	if flows != 0 {
		t.Fatal("funded bootstrap already contains an unrelated application flow")
	}
	sum := ByteCount(0)
	for index, name := range []string{"a", "b", "c"} {
		channel := self.h.channel(name)
		client := channel.client
		budget := self.budgets[name]
		if channel.IsDone() || channel.isWarning() || !channel.hasActiveTransport() || client.settings.EncryptionSettings.Mode != EncryptionModeOff ||
			!client.ContractManager().SendNoContract(channel.Destination().Tail()) || budget.TotalByteCount() != self.funding.children[index] {
			t.Fatalf("funded bootstrap client %s lacks its real admitted no-contract owner", name)
		}
		client.sendBuffer.mutex.Lock()
		sources := make([]*SendSequence, 0, len(client.sendBuffer.sendSequences))
		for _, source := range client.sendBuffer.sendSequences {
			sources = append(sources, source)
		}
		client.sendBuffer.mutex.Unlock()
		var controlId Id
		controlItems, providerItems := 0, 0
		owned := ByteCount(0)
		for _, source := range sources {
			count, _, _, types := source.ResendQueueSizeAndMessageTypes()
			source.resendQueue.stateLock.Lock()
			charge, lifetime := source.resendQueue.queueByteCount, source.resendQueue.lifetimeBudget
			source.resendQueue.stateLock.Unlock()
			if source.resendQueue.Budget() != budget || !lifetime || source.sendBufferSettings.ProtocolVersion != self.funding.protocol || source.preparedContractByteCount.Load() != 0 {
				t.Fatalf("bootstrap source %s escaped its exact funding/protocol/no-contract geometry", name)
			}
			if source.destination == controlId {
				controlItems += count
				for _, kind := range types {
					if kind != protocol.MessageType_TransferClientKey {
						t.Fatalf("unexpected bootstrap control %s: %v", name, kind)
					}
				}
			} else if source.destination == channel.Destination().Tail() {
				providerItems += count
				for _, kind := range types {
					if kind != protocol.MessageType_IpIpPing {
						t.Fatalf("unexpected pre-scenario provider traffic %s: %v", name, kind)
					}
				}
			} else {
				t.Fatalf("unowned bootstrap destination for %s", name)
			}
			if charge > ByteCount(count)*self.funding.ordinary {
				t.Fatalf("bootstrap source %s exceeds its declared ordinary item charge", name)
			}
			owned = addReceiveQueueByteCount(owned, charge)
		}
		if controlItems != 1 || providerItems > 1 || budget.UsedByteCount() != owned || owned > 2*self.funding.ordinary {
			t.Fatalf("bootstrap %s has unexplained/stale credit: control_items=%d provider_items=%d queued=%d used=%d", name, controlItems, providerItems, owned, budget.UsedByteCount())
		}
		sum = addReceiveQueueByteCount(sum, owned)
	}
	if sum != self.root.UsedByteCount() {
		t.Fatal("bootstrap descendants do not explain the exact root ownership")
	}
	t.Logf("NATIVE_SYN_FUNDED_BOOTSTRAP protocol=%d ordinary=%d group=%d cache=%d readiness=%d ready_capacity=%d remote_capacity=%d child_caps=%v root_cap=%d bootstrap_used=%d", self.funding.protocol, self.funding.ordinary, self.funding.group, self.funding.cache, self.funding.readiness, self.funding.readyCapacity, self.funding.remoteCapacity, self.funding.children, self.funding.total, sum)
}

type nativeSynDispositionOrderFixture struct {
	h          *reraceHarness
	path       *IpPath
	body       *IpPath
	bodyOwner  *multiClientChannel
	update     *multiClientChannelUpdate
	race       *multiClientChannelUpdateRace
	root       *TransferMemoryBudget
	funding    nativeSynDispositionFunding
	budgets    map[string]*TransferMemoryBudget
	event      nativeSynDispositionOrderEvent
	release    chan struct{}
	releaseOne sync.Once
	held       atomic.Bool
	forceB     atomic.Bool
	wire       chan nativeSynDispositionWireEvent
	wireSeen   map[nativeSynDispositionWireEvent]bool
	afterOld   chan nativeSynDispositionCacheEvent
	sampledOld atomic.Bool
	answerB    atomic.Bool
}

func newNativeSynDispositionOrderFixture(t *testing.T, failedNewSource bool) *nativeSynDispositionOrderFixture {
	return newNativeSynDispositionOrderFixtureConfigured(t, failedNewSource, nil)
}

// Configuration occurs before clients start; the same real source path owns
// every additional boundary control below.
func newNativeSynDispositionOrderFixtureConfigured(t *testing.T, failedNewSource bool, configure func(string, *ClientSettings, *nativeSynDispositionOrderFixture), extras ...[3]int) *nativeSynDispositionOrderFixture {
	t.Helper()
	f := &nativeSynDispositionOrderFixture{
		path: reraceFlow(4, 171, 54401), body: reraceFlow(4, 172, 54402),
		root:    NewTransferMemoryBudget(0),
		budgets: map[string]*TransferMemoryBudget{}, release: make(chan struct{}),
		wire: make(chan nativeSynDispositionWireEvent, 16), wireSeen: map[nativeSynDispositionWireEvent]bool{},
		afterOld: make(chan nativeSynDispositionCacheEvent, 1),
	}
	f.path.SequenceNumber = 1000
	// Registered before the harness: this runs after every actual client joins.
	t.Cleanup(func() {
		for name, budget := range f.budgets {
			reserved, released := budget.Counts()
			if reserved != released {
				t.Errorf("native disposition-order client %s credit counts unbalanced: reserved=%d released=%d", name, reserved, released)
			}
			if budget.UsedByteCount() != 0 {
				t.Errorf("native disposition-order client %s retained %d paid bytes", name, budget.UsedByteCount())
			}
		}
		reserved, released := f.root.Counts()
		if reserved != released {
			t.Errorf("native disposition-order root credit counts unbalanced: reserved=%d released=%d", reserved, released)
		}
		if f.root.UsedByteCount() != 0 {
			t.Errorf("native disposition-order root retained %d paid bytes", f.root.UsedByteCount())
		}
	})
	entered := make(chan nativeSynDispositionOrderEvent, 1)
	// The initial B cache must be newer than the actual held A disposition.
	// Admission is concurrent; only the existing dequeue boundary establishes
	// that order. Later native controls still exercise their original oracle.
	initialAMaterialized := make(chan struct{})
	var orderInitialB atomic.Bool
	specs := make([]reraceExitSpec, 3)
	for index := range specs {
		name := string(rune('a' + index))
		budget := NewTransferMemoryBudgetWithParent(0, f.root)
		f.budgets[name] = budget
		specs[index] = reraceExitSpec{name: name, answer: func(path *IpPath) reraceAnswer {
			if reraceSameFlow(path, f.path) && !(name == "b" && f.answerB.Load()) {
				return reraceSilent
			}
			return reraceAccept
		}, clientSettings: func(settings *ClientSettings) {
			settings.SendBufferSettings.ResendQueueBudget = budget
			settings.SendBufferSettings.ResendQueueRetainedByteAccounting = true
			if configure != nil {
				configure(name, settings, f)
			}
			if name == "b" {
				previous := settings.SendBufferSettings.beforeGroupDequeueForTest
				settings.SendBufferSettings.beforeGroupDequeueForTest = func(source *SendSequence) {
					if orderInitialB.CompareAndSwap(true, false) {
						select {
						case <-initialAMaterialized:
						case <-source.ctx.Done():
						}
					}
					if previous != nil {
						previous(source)
					}
				}
			}
			if name != "a" {
				return
			}
			previousWire := settings.SendBufferSettings.TransferWireMessageObserver
			settings.SendBufferSettings.TransferWireMessageObserver = func(observation TransferWireMessageObservation) {
				if previousWire != nil {
					previousWire(observation)
				}
				var transfer protocol.TransferFrame
				if ProtoUnmarshal(observation.TransferFrameBytes, &transfer) != nil {
					return
				}
				pack := transfer.Pack
				if pack == nil && transfer.Frame != nil && transfer.Frame.MessageType == protocol.MessageType_TransferPack {
					pack = &protocol.Pack{}
					if ProtoUnmarshal(transfer.Frame.MessageBytes, pack) != nil {
						return
					}
				}
				if pack == nil {
					return
				}
				for _, frame := range pack.Frames {
					if frame == nil || frame.MessageType != protocol.MessageType_IpIpPacketToProvider {
						continue
					}
					packet, err := ipPacketToProviderBytes(frame)
					if err != nil {
						continue
					}
					path, err := ParseIpPath(packet)
					if err != nil || !reraceSameFlow(path, f.path) || !path.Syn || path.Ack || path.SequenceNumber != f.path.SequenceNumber || len(packet) != 40 || !f.held.Load() || !f.sampledOld.CompareAndSwap(false, true) {
						continue
					}
					update := f.update
					if update == nil {
						t.Error("old-source writer resumed before its canonical update was captured")
						return
					}
					update.stateLock.Lock()
					owner := update.synRetry
					event := nativeSynDispositionCacheEvent{owner: owner}
					if owner != nil {
						event.credit = owner.credit
						event.live = owner.credit != nil && !owner.credit.released.Load()
					}
					update.stateLock.Unlock()
					// Existing wire observers borrow bytes and must not wait.
					select {
					case f.afterOld <- event:
					default:
						t.Error("old-source cache observer emitted more than its one scalar event")
					}
				}
			}
			settings.SendBufferSettings.afterGroupDispositionForTest = func(source *SendSequence, disposition sendGroupDisposition, frames []*protocol.Frame) {
				group, ok := disposition.target.(*parsedPacketGroup)
				if !ok || group.collapseAdmission.update == nil {
					return
				}
				for _, frame := range frames {
					if frame == nil || frame.MessageType != protocol.MessageType_IpIpPacketToProvider {
						continue
					}
					packet, err := ipPacketToProviderBytes(frame)
					if err != nil {
						continue
					}
					path, err := ParseIpPath(packet)
					if err != nil || !reraceSameFlow(path, f.path) || !path.Syn || path.Ack || path.SequenceNumber != f.path.SequenceNumber ||
						!f.held.CompareAndSwap(false, true) {
						continue
					}
					close(initialAMaterialized)
					entered <- nativeSynDispositionOrderEvent{source: source, group: group, update: group.collapseAdmission.update, bytes: len(packet)}
					select {
					case <-f.release:
					case <-source.ctx.Done():
					}
				}
			}
		}}
	}
	f.h = newReraceHarness(t, specs, func(settings *MultiClientSettings) {
		settings.WindowSizes[WindowTypeQuality] = WindowSizeSettings{WindowSizeMin: 3, WindowSizeMax: 3, WindowSizeHardMax: 3}
		settings.StrictWindowSizeHardMax, settings.StandingReserve = true, false
		f.funding = nativeSynDispositionFundingFor(settings)
		if len(extras) != 0 {
			// This one control admits exactly one additional native source on
			// an existing client. Pay its item/group/cache/readiness overlap.
			settings.startChannelPingForTest = func(func()) {}
			for index, count := range extras[0] {
				if count < 0 || count > 1 {
					t.Fatal("extra source exceeds its fixed control cohort")
				}
				extra := ByteCount(count) * (f.funding.ordinary + f.funding.group + f.funding.cache + f.funding.readiness)
				f.funding.children[index] = addReceiveQueueByteCount(f.funding.children[index], extra)
				f.funding.total = addReceiveQueueByteCount(f.funding.total, extra)
			}
		}
		for index, name := range []string{"a", "b", "c"} {
			f.budgets[name].SetTotalByteCount(f.funding.children[index])
		}
		f.root.SetTotalByteCount(f.funding.total)
	})
	// Public provider callbacks observe the real borrowed packet after its
	// normal decoder. No packet/frame share or callback replacement is made.
	for _, name := range []string{"a", "b", "c"} {
		providerName := name
		unsubscribe := f.h.exit(name).provider.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, _ Peer) {
			for _, frame := range frames {
				if frame == nil || frame.MessageType != protocol.MessageType_IpIpPacketToProvider {
					continue
				}
				packet, err := ipPacketToProviderBytes(frame)
				if err != nil {
					continue
				}
				path, err := ParseIpPath(packet)
				if err != nil || !reraceSameFlow(path, f.path) {
					continue
				}
				event := nativeSynDispositionWireEvent{provider: providerName, sequence: path.SequenceNumber, reset: path.Rst, bytes: len(packet)}
				select {
				case f.wire <- event:
				default:
					t.Error("native disposition wire observer overflowed its fixed scalar bound")
				}
			}
		})
		t.Cleanup(unsubscribe)
	}
	// Releasing every barrier precedes harness cancellation/join, also on Fatal.
	t.Cleanup(f.resumeOld)
	a, b, c := f.h.channel("a"), f.h.channel("b"), f.h.channel("c")
	f.h.multiClient.groupRaceCandidatesForTest = func(group *parsedPacketGroup) []*multiClientChannel {
		if !reraceSameFlow(group.ipPath, f.path) {
			return []*multiClientChannel{c}
		}
		if !f.forceB.Load() && group.ipPath.Syn && !group.ipPath.Ack && group.ipPath.SequenceNumber == f.path.SequenceNumber {
			return []*multiClientChannel{a, b}
		}
		return []*multiClientChannel{b}
	}
	f.requireFundedBootstrap(t)
	if !f.h.syn(f.body) {
		t.Fatal("disposition-order healthy body SYN was not admitted")
	}
	f.bodyOwner = f.h.boundTo(f.body)
	if _, answered := f.h.synAck(f.body); !answered || f.bodyOwner != c || !f.h.send(reraceSegment(f.body, 0, []byte("body-before"))) {
		t.Fatal("disposition-order fixture lacks one actual healthy third-provider body")
	}
	if failedNewSource {
		// The old A source is genuinely admitted; the newer B source will own
		// no raw packet. The root bound is never raised or waived.
		f.budgets["b"].SetTotalByteCount(0)
	}
	f.h.concurrent = true
	orderInitialB.Store(true)
	if !privateCrossWindowNativeSyn(f.h, f.path) {
		t.Fatal("old ordinary native source was not admitted")
	}
	timer := time.NewTimer(2500 * time.Millisecond)
	defer timer.Stop()
	select {
	case f.event = <-entered:
	case <-timer.C:
		t.Fatal("old ordinary native source did not reach post-disposition/pre-cache boundary")
	}
	f.h.concurrent = false
	f.h.settle()
	f.update = f.h.update(f.path)
	if f.update == nil || f.event.update != f.update || f.event.bytes != 40 || f.event.source.resendQueue.Budget() != f.budgets["a"] {
		t.Fatal("held disposition lacks the actual canonical source, owned SYN and paid A queue")
	}
	f.update.stateLock.Lock()
	flags := f.event.group.completionFlags.Load()
	const admitted = groupCompletionTracked | groupCompletionAdmitted | groupCompletionDequeued
	// An unanswered wide race publishes source controls per candidate. Global
	// sequence state is committed only by selecting a winner, which is absent.
	f.race = f.update.race
	candidateLocal := false
	expectedWidth := 2
	bUnmaterialized := true
	raceWidth, raceSingle := 0, false
	if race := f.race; race != nil {
		raceWidth, raceSingle = len(race.clientStates), race.single
		if state := race.clientStates[a]; state != nil {
			control := &state.collapseControl
			owner := f.event.group.collapseOwner
			candidateLocal = !race.single && len(race.clientStates) == expectedWidth && race.clientStates[b] != nil &&
				f.update.client.Load() == nil && !f.update.synGenerationSeen && !f.update.receivedInbound.Load() && !f.update.IsDone() &&
				f.update.dialFailedSynSeen && f.update.dialFailedSyn == f.path.SequenceNumber &&
				flags&(groupCompletionCandidate|groupCompletionLinked) == groupCompletionCandidate|groupCompletionLinked &&
				f.event.group.completionClient == a && f.event.group.collapseSource == f.event.source &&
				f.event.group.collapseAdmission.epoch == f.update.sequenceAdmissionEpoch && owner != nil && owner.remaining == 0 &&
				control.synSeen && !control.closed && control.synNumber == f.path.SequenceNumber &&
				control.order != 0 && control.order == f.event.group.collapseOrder() && control.synOrder == control.order &&
				control.sourceId == f.event.group.collapseSourceIdWithLock()
		}
	}
	published := flags&admitted == admitted && flags&groupCompletionAbandoned == 0 &&
		f.event.group.affinitySourceState&affinitySourceMaterialized != 0 && candidateLocal
	if failedNewSource {
		// Raw source handoff may succeed before capacity materializes a Pack.
		// Such a candidate belongs to the wide field but has no new receipt.
		for group := f.update.sequenceClaims; group != nil; group = group.collapseNext {
			if group.completionClient == b && group.affinitySourceState&affinitySourceMaterialized != 0 {
				bUnmaterialized = false
			}
		}
	}
	synSeen, synNumber := f.update.synGenerationSeen, f.update.synGenerationNumber
	selectedSource := f.update.client.Load() == f.event.group.completionClient
	sourceMaterialized := f.event.group.affinitySourceState&affinitySourceMaterialized != 0
	f.update.stateLock.Unlock()
	if !published || len(f.h.exit("a").syns(f.path)) != 0 || f.root.UsedByteCount() <= 0 {
		t.Logf("NATIVE_SYN_DISPOSITION_BOUNDARY_DIAGNOSTIC published=%t candidate_local=%t flags=%x source_materialized=%t global_syn_seen=%t global_syn=%d wanted_syn=%d selected_source=%t race_width=%d race_single=%t root_used=%d root_cap=%d", published, candidateLocal, flags, sourceMaterialized, synSeen, synNumber, f.path.SequenceNumber, selectedSource, raceWidth, raceSingle, f.root.UsedByteCount(), f.root.TotalByteCount())
		t.Fatal("old-source barrier lacks its actual live wide-candidate materialization before physical write")
	}
	if failedNewSource && (!bUnmaterialized || len(f.h.exit("b").syns(f.path)) != 0 || f.budgets["b"].UsedByteCount() != f.funding.ordinary) {
		t.Fatal("zero-budget B materialized application bytes or retained more than its actual bootstrap control")
	}
	return f
}

func (self *nativeSynDispositionOrderFixture) resumeOld() {
	self.releaseOne.Do(func() { close(self.release) })
	self.h.concurrent = false
}

// Wait on the actual decoded provider event, retaining the draft's existing
// finite setup bound. The ordering proof is the source gate, never a poll/sleep.
func (self *nativeSynDispositionOrderFixture) waitWire(t *testing.T, provider string, sequence uint32, reset bool, byteCounts ...int) {
	t.Helper()
	bytes := 40
	if len(byteCounts) != 0 {
		bytes = byteCounts[0]
	}
	wanted := nativeSynDispositionWireEvent{provider: provider, sequence: sequence, reset: reset, bytes: bytes}
	timer := time.NewTimer(2500 * time.Millisecond)
	defer timer.Stop()
	for !self.wireSeen[wanted] {
		select {
		case event := <-self.wire:
			self.wireSeen[event] = true
		case <-timer.C:
			t.Fatal("actual source did not reach the required provider wire before the existing fixture bound")
		}
	}
	self.h.settle()
}

func (self *nativeSynDispositionOrderFixture) cache(t *testing.T, sequence uint32, budget *TransferMemoryBudget) (*tcpSynRetry, *transferRetainedControlCredit) {
	t.Helper()
	self.update.stateLock.Lock()
	owner := self.update.synRetry
	var credit *transferRetainedControlCredit
	valid := owner != nil && owner.references != 0 && owner.packet != nil && owner.sequence == sequence && owner.serial != 0 &&
		owner.credit != nil && owner.credit.budget == budget && !owner.credit.released.Load() && budget.root == self.root
	if valid {
		credit = owner.credit
		ready := owner.readiness
		valid = credit.byteCount <= self.funding.cache && cap(owner.refusedRemoteIds) <= self.funding.remoteCapacity &&
			ready != nil && ready.credit != nil && !ready.credit.released.Load() && ready.credit.byteCount <= self.funding.readiness && len(ready.slots) <= self.funding.readyCapacity
	}
	var diagnostic string
	if !valid {
		// Current-source assertion census; every original assertion is preserved.
		diagnostic = fmt.Sprintf("valid=%t owner_present=%t budget_root_matches=%t child_used=%d child_cap=%d root_used=%d root_cap=%d funded_cache=%d funded_ready=%d funded_remote_capacity=%d funded_slots=%d", valid, owner != nil, budget.root == self.root, budget.UsedByteCount(), budget.TotalByteCount(), self.root.UsedByteCount(), self.root.TotalByteCount(), self.funding.cache, self.funding.readiness, self.funding.remoteCapacity, self.funding.readyCapacity)
		if owner != nil {
			diagnostic += fmt.Sprintf(" refs=%d packet_present=%t sequence=%d expected_sequence=%d serial=%d credit_present=%t refused_capacity=%d readiness_present=%t", owner.references, owner.packet != nil, owner.sequence, sequence, owner.serial, owner.credit != nil, cap(owner.refusedRemoteIds), owner.readiness != nil)
			if value := owner.credit; value != nil {
				diagnostic += fmt.Sprintf(" credit_budget_matches=%t credit_released=%t cache_bytes=%d", value.budget == budget, value.released.Load(), value.byteCount)
			}
			if ready := owner.readiness; ready != nil {
				diagnostic += fmt.Sprintf(" ready_credit_present=%t ready_slots=%d", ready.credit != nil, len(ready.slots))
				if value := ready.credit; value != nil {
					diagnostic += fmt.Sprintf(" ready_released=%t ready_bytes=%d", value.released.Load(), value.byteCount)
				}
			}
		}
	}
	self.update.stateLock.Unlock()
	if !valid || self.root.UsedByteCount() < credit.byteCount {
		t.Logf("MASTER_CURRENT_CACHE_CENSUS %s", diagnostic)
		t.Fatal("actual cached source lacks its exact live paid child/root ownership")
	}
	return owner, credit
}

// Final application delivery, rather than quiescence before a batching timer,
// proves that the same independent provider still carries fresh sibling bytes.
func (self *nativeSynDispositionOrderFixture) requireHealthyBody(t *testing.T) {
	t.Helper()
	parent := self.h.multiClient
	previous := parent.receivePacketCallback.Load()
	observed := make(chan struct{}, 1)
	payload := []byte("body-after")
	offset := uint32(len("body-before"))
	reverse := self.body.Reverse()
	expectedAck := self.body.SequenceNumber + 1 + offset + uint32(len(payload))
	parent.SetReceivePacketCallback(func(source TransferPath, mode protocol.ProvideMode, path *IpPath, packet []byte) {
		if previous != nil {
			previous.callback(source, mode, path, packet)
		}
		received, body, err := ParseIpPathWithPayload(packet)
		if err == nil && reraceSameFlow(received, reverse) && received.AckSequenceNumber == expectedAck && bytes.Equal(body, payload) {
			select {
			case observed <- struct{}{}:
			default:
			}
		}
	})
	defer func() {
		if previous == nil {
			parent.SetReceivePacketCallback(nil)
		} else {
			parent.SetReceivePacketCallback(previous.callback)
		}
	}()
	if self.h.boundTo(self.body) != self.bodyOwner || self.bodyOwner.IsDone() || self.bodyOwner.isWarning() || !self.h.send(reraceSegment(self.body, offset, payload)) {
		t.Fatal("delayed native materialization refused fresh unrelated healthy bytes")
	}
	select {
	case <-observed:
	case <-self.h.ctx.Done():
		t.Fatal("healthy sibling canceled before its actual app delivery")
	}
	if len(self.h.delivered(self.body)) != 3 || self.h.boundTo(self.body) != self.bodyOwner || self.bodyOwner.IsDone() {
		t.Fatal("delayed native materialization changed healthy sibling ownership or exact body progress")
	}
	reraceAssertNoErrorDelivered(t, self.h, self.body)
}

func testNativeSynDispositionOrder(t *testing.T, mode string) {
	t.Helper()
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		cause := ""
		// Registered before every fixture owner: the expected OLD marker may
		// appear only after real client/worker joins and zero-budget cleanup.
		t.Cleanup(func() {
			if cause != "" && !t.Failed() {
				t.Fatal(cause)
			}
		})
		f := newNativeSynDispositionOrderFixture(t, mode == "refused")
		fresh := *f.path
		fresh.SequenceNumber++
		var beforeOwner *tcpSynRetry
		var beforeCredit *transferRetainedControlCredit
		if mode != "refused" {
			f.waitWire(t, "b", f.path.SequenceNumber, false)
			beforeOwner, beforeCredit = f.cache(t, f.path.SequenceNumber, f.budgets["b"])
		}
		switch mode {
		case "fresh", "aba", "extension":
			if mode == "extension" {
				fresh = *f.path
				payload := []byte("new-data")
				packet := MessagePoolCopy(reraceTcpPacket(&fresh, tcpFlagSyn, fresh.SequenceNumber, 0, payload))
				accepted := [1]bool{}
				count := f.h.multiClient.SendNativePacketBatchWithResults(f.h.source, protocol.ProvideMode_Network, [][]byte{packet}, reraceSendTimeout, accepted[:])
				f.h.settle()
				if count != 1 || !accepted[0] {
					t.Fatal("same-ISN extension did not acquire a real native source")
				}
				f.waitWire(t, "b", fresh.SequenceNumber, false, 40+len(payload))
			} else {
				if !privateCrossWindowNativeSyn(f.h, &fresh) {
					t.Fatal("newer different-ISN native source was not admitted")
				}
				f.waitWire(t, "b", fresh.SequenceNumber, false)
			}
			if mode == "aba" {
				// Return to the original numeric ISN in a distinct actually
				// admitted cohort. Different TFO bytes expose stale retention
				// even though an equality-only guard would now appear valid.
				f.forceB.Store(true)
				fresh = *f.path
				payload := []byte("new-A")
				packet := MessagePoolCopy(reraceTcpPacket(&fresh, tcpFlagSyn, fresh.SequenceNumber, 0, payload))
				accepted := [1]bool{}
				count := f.h.multiClient.SendNativePacketBatchWithResults(f.h.source, protocol.ProvideMode_Network, [][]byte{packet}, reraceSendTimeout, accepted[:])
				f.h.settle()
				if count != 1 || !accepted[0] {
					t.Fatal("ABA source did not enter the actual native boundary")
				}
				f.waitWire(t, "b", fresh.SequenceNumber, false, 40+len(payload))
			}
			beforeOwner, beforeCredit = f.cache(t, fresh.SequenceNumber, f.budgets["b"])
			currentUpdate := f.h.update(f.path)
			b := f.h.channel("b")
			f.update.stateLock.Lock()
			// The actual B wire/cache and its newer dequeued control attest
			// authority while the same unanswered race still has no winner.
			candidateCurrent := false
			oldStillLive := f.event.group.completionFlags.Load()&(groupCompletionAbandoned|groupCompletionLinked) == groupCompletionLinked &&
				f.event.group.collapseSource == f.event.source && f.event.group.collapseOwner != nil && f.event.group.collapseOwner.remaining == 0
			if race := f.update.race; race == f.race && race != nil && !race.single && len(race.clientStates) == 2 {
				if state := race.clientStates[b]; state != nil {
					control := &state.collapseControl
					candidateCurrent = control.synSeen && !control.closed && control.synNumber == fresh.SequenceNumber &&
						(mode == "extension" || control.synOrder == control.order) && f.event.group.collapseOrder() < control.order
				}
			}
			ready := currentUpdate == f.update && f.update.dialFailedSynSeen && f.update.dialFailedSyn == fresh.SequenceNumber &&
				f.update.client.Load() == nil && !f.update.synGenerationSeen && candidateCurrent && oldStillLive && beforeOwner.validGenerationWithLock(f.update)
			f.update.stateLock.Unlock()
			if !ready {
				t.Logf("NATIVE_SYN_NEW_CANDIDATE_AUTHORITY_DIAGNOSTIC same_update=%t candidate_current=%t old_still_live=%t", currentUpdate == f.update, candidateCurrent, oldStillLive)
				t.Fatal("fresh source did not establish real current-candidate cache authority before old writer release")
			}
		case "equal":
			collapse := f.h.multiClient.TcpCollapseDropCount()
			accepted := privateCrossWindowNativeSyn(f.h, f.path)
			if accepted || f.h.multiClient.TcpCollapseDropCount()-collapse != 1 {
				t.Fatal("equal-ISN native retransmit did not retain its ordinary persistent collapse behavior")
			}
		case "refused":
			// Preserve the already-owned A item while neither possible next
			// source has admission headroom. C's healthy body stays funded.
			f.budgets["a"].SetTotalByteCount(0)
			// Zero timeout selects the actual non-waiting capacity gate;
			// positive-timeout raw handoff alone is not materialization proof.
			packet := MessagePoolCopy(reraceTcpPacket(&fresh, tcpFlagSyn, fresh.SequenceNumber, 0, nil))
			accepted := [1]bool{}
			count := f.h.multiClient.SendNativePacketBatchWithResults(f.h.source, protocol.ProvideMode_Network, [][]byte{packet}, 0, accepted[:])
			f.h.settle()
			if count != 0 || accepted[0] || len(f.h.exit("b").syns(f.path)) != 0 || f.budgets["b"].UsedByteCount() != f.funding.ordinary {
				t.Fatal("non-waiting newer source lacked an actual admission refusal with zero B materialization")
			}
			f.update.stateLock.Lock()
			unpaid := f.update.synRetry == nil && f.update.dialFailedSyn == fresh.SequenceNumber && !f.update.synGenerationSeen &&
				f.update.synRetryMaterializedOrder == f.event.group.collapseOrder() && f.update.synRetryMaterializedIndex == 0
			f.update.stateLock.Unlock()
			if !unpaid {
				t.Fatal("refused new-source control has unexpected paid cache or committed generation")
			}
			// The already materialized A Pack regains only its cache headroom.
			// Keep B closed to new permission through the exact cache oracle.
			f.budgets["a"].SetTotalByteCount(f.funding.children[0])
		case "reset":
			packet := MessagePoolCopy(reraceTcpPacket(f.path, tcpFlagRst|tcpFlagAck, fresh.SequenceNumber, 0, nil))
			accepted := [1]bool{}
			count := f.h.multiClient.SendNativePacketBatchWithResults(f.h.source, protocol.ProvideMode_Network, [][]byte{packet}, reraceSendTimeout, accepted[:])
			f.h.settle()
			if count != 1 || !accepted[0] {
				t.Fatal("real native reset source was not admitted")
			}
			f.waitWire(t, "b", fresh.SequenceNumber, true)
		case "close":
			f.update.Close()
			f.h.settle()
		default:
			t.Fatal("unknown native disposition-order control")
		}
		f.resumeOld()
		f.waitWire(t, "a", f.path.SequenceNumber, false)
		var published nativeSynDispositionCacheEvent
		select {
		case published = <-f.afterOld:
		default:
			t.Fatal("actual old wire lacked its pre-write cache observation")
		}
		f.h.settle()
		f.requireHealthyBody(t)
		f.update.stateLock.Lock()
		after := f.update.synRetry
		afterSerial, afterSequence := uint64(0), uint32(0)
		if after != nil {
			afterSerial, afterSequence = after.serial, after.sequence
		}
		stable := published.owner == beforeOwner && published.credit == beforeCredit && published.live && after == beforeOwner && beforeCredit != nil && !beforeCredit.released.Load()
		f.update.stateLock.Unlock()
		switch mode {
		case "aba":
			if !stable {
				cause = fmt.Sprintf("OLDER_NATIVE_RETENTION_BORROWED_RETURNED_ISN_GENERATION: fresh_serial=%d after_serial=%d current_syn=%d after_syn=%d fresh_credit_released=%t", beforeOwner.serial, afterSerial, fresh.SequenceNumber, afterSequence, beforeCredit.released.Load())
			}
		case "fresh", "extension":
			if !stable {
				cause = fmt.Sprintf("OLDER_NATIVE_RETENTION_OVERWROTE_NEW_PAID_CACHE: fresh_serial=%d after_serial=%d expected_syn=%d after_syn=%d fresh_credit_released=%t", beforeOwner.serial, afterSerial, fresh.SequenceNumber, afterSequence, beforeCredit.released.Load())
			}
		case "equal":
			if !stable {
				t.Fatal("equal-ISN delayed materialization replaced its already-paid identical cache")
			}
		case "refused":
			f.cache(t, f.path.SequenceNumber, f.budgets["a"])
		case "reset", "close":
			if published.owner != nil || after != nil || !beforeCredit.released.Load() {
				t.Fatal("closed/reset source was resurrected by its delayed old cache writer")
			}
		}
		if err := f.h.multiClient.CloseAndWait(context.Background()); err != nil {
			t.Fatal(err)
		}
	})
}

func TestNativeSynCacheOlderMaterializationCannotOverwriteNewerAdmittedGeneration(t *testing.T) {
	testNativeSynDispositionOrder(t, "fresh")
}

func TestNativeSynCacheEqualIsnMaterializationKeepsPaidOwner(t *testing.T) {
	testNativeSynDispositionOrder(t, "equal")
}

func TestNativeSynCacheRefusedNewSourceKeepsOlderAdmittedCredit(t *testing.T) {
	testNativeSynDispositionOrder(t, "refused")
}

func TestNativeSynCacheResetBeforeDelayedRetentionCannotResurrect(t *testing.T) {
	testNativeSynDispositionOrder(t, "reset")
}

func TestNativeSynCacheClosedSourceBeforeDelayedRetentionCannotResurrect(t *testing.T) {
	testNativeSynDispositionOrder(t, "close")
}

// Numeric ISN equality after an admitted A→B→A sequence is not source identity.
func TestNativeSynCacheReturnedIsnCannotBorrowOlderMaterialization(t *testing.T) {
	testNativeSynDispositionOrder(t, "aba")
}

// Different bytes at the same ISN belong to a real admitted extension, while
// the existing equal-packet control keeps its original duplicate suppression.
func TestNativeSynCacheEqualIsnExtensionKeepsLatestMaterializedBytes(t *testing.T) {
	testNativeSynDispositionOrder(t, "extension")
}

// A successfully materialized newer SYN remains authoritative even when its
// optional cache reservation is refused. A later old writer cannot revive it.
func TestNativeSynCacheUnfundedNewMaterializationRejectsOlderResurrection(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		cause := ""
		t.Cleanup(func() {
			if cause != "" && !t.Failed() {
				t.Fatal(cause)
			}
		})
		var denied atomic.Bool
		f := newNativeSynDispositionOrderFixtureConfigured(t, false, func(name string, settings *ClientSettings, f *nativeSynDispositionOrderFixture) {
			if name != "b" {
				return
			}
			settings.SendBufferSettings.afterGroupDispositionForTest = func(source *SendSequence, disposition sendGroupDisposition, frames []*protocol.Frame) {
				for _, frame := range frames {
					if frame == nil || frame.MessageType != protocol.MessageType_IpIpPacketToProvider {
						continue
					}
					packet, err := ipPacketToProviderBytes(frame)
					if err != nil {
						continue
					}
					path, err := ParseIpPath(packet)
					if err == nil && reraceSameFlow(path, f.path) && path.Syn && !path.Ack && path.SequenceNumber == f.path.SequenceNumber+1 {
						// The actual new Pack already owns its full charge.
						// Close future permission; existing owners stay paid.
						budget := source.resendQueue.Budget()
						budget.SetTotalByteCount(0)
						denied.Store(true)
					}
				}
			}
		})
		f.waitWire(t, "b", f.path.SequenceNumber, false)
		_, oldCredit := f.cache(t, f.path.SequenceNumber, f.budgets["b"])
		fresh := *f.path
		fresh.SequenceNumber++
		if !privateCrossWindowNativeSyn(f.h, &fresh) {
			t.Fatal("new source did not acquire its actual Pack before optional-cache refusal")
		}
		f.waitWire(t, "b", fresh.SequenceNumber, false)
		f.update.stateLock.Lock()
		noCache := f.update.synRetry == nil && f.update.synRetryMaterializedOrder > f.event.group.collapseOrder() && !f.update.IsDone() && !f.update.receivedInbound.Load()
		f.update.stateLock.Unlock()
		if !denied.Load() || !noCache || !oldCredit.released.Load() || f.h.channel("b").IsDone() || f.h.channel("b").isWarning() {
			t.Fatal("optional cache denial lacked real newer materialization or poisoned its provider")
		}
		f.resumeOld()
		f.waitWire(t, "a", f.path.SequenceNumber, false)
		f.requireHealthyBody(t)
		f.update.stateLock.Lock()
		resurrected := f.update.synRetry != nil
		f.update.stateLock.Unlock()
		if resurrected {
			cause = "SYN_CACHE_OLDER_RESURRECTED_AFTER_NEW_ALLOCATION_REFUSAL: real newer Pack lost authority when optional storage was unavailable"
		}
		f.budgets["b"].SetTotalByteCount(f.funding.children[1])
		if err := f.h.multiClient.CloseAndWait(context.Background()); err != nil {
			t.Fatal(err)
		}
	})
}

// A real new-generation SYN-ACK promotes B and clears retry storage. Delayed
// A retention cannot resurrect it or change the selected established owner.
func TestNativeSynCacheSelectedPromotionRejectsDelayedLoser(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newNativeSynDispositionOrderFixture(t, false)
		f.waitWire(t, "b", f.path.SequenceNumber, false)
		_, oldCredit := f.cache(t, f.path.SequenceNumber, f.budgets["b"])
		fresh := *f.path
		fresh.SequenceNumber++
		f.answerB.Store(true)
		if !privateCrossWindowNativeSyn(f.h, &fresh) {
			t.Fatal("new selected-generation native source was not admitted")
		}
		f.waitWire(t, "b", fresh.SequenceNumber, false)
		ack, answered := f.h.synAck(&fresh)
		var ackPath *IpPath
		if answered {
			ackPath, _ = ParseIpPath(ack.packet)
		}
		b := f.h.channel("b")
		f.update.stateLock.Lock()
		established := f.update.client.Load() == b && f.update.receivedInbound.Load() && f.update.synGenerationSeen && f.update.synGenerationNumber == fresh.SequenceNumber && f.update.synRetry == nil &&
			f.event.group.completionFlags.Load()&groupCompletionAbandoned != 0
		f.update.stateLock.Unlock()
		if !answered || ackPath == nil || ackPath.AckSequenceNumber != fresh.SequenceNumber+1 || !established || !oldCredit.released.Load() {
			t.Fatal("actual SYN-ACK did not establish the newer selected generation before old retention")
		}
		f.resumeOld()
		f.waitWire(t, "a", f.path.SequenceNumber, false)
		f.requireHealthyBody(t)
		f.update.stateLock.Lock()
		stable := f.update.client.Load() == b && f.update.receivedInbound.Load() && f.update.synRetry == nil
		f.update.stateLock.Unlock()
		if !stable {
			t.Fatal("delayed loser changed real selected-generation ownership or resurrected storage")
		}
		if err := f.h.multiClient.CloseAndWait(context.Background()); err != nil {
			t.Fatal(err)
		}
	})
}

// Source shutdown releases its real sender without allowing its delayed
// retention path to clear another live source's newest cache or permission.
func TestNativeSynCacheCanceledSourcePreservesNewerPaidOwner(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newNativeSynDispositionOrderFixture(t, false)
		f.waitWire(t, "b", f.path.SequenceNumber, false)
		fresh := *f.path
		fresh.SequenceNumber++
		if !privateCrossWindowNativeSyn(f.h, &fresh) {
			t.Fatal("newer source was not admitted before canceling old source")
		}
		f.waitWire(t, "b", fresh.SequenceNumber, false)
		owner, credit := f.cache(t, fresh.SequenceNumber, f.budgets["b"])
		f.event.source.Cancel()
		timer := time.NewTimer(2500 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-f.event.source.done:
		case <-timer.C:
			t.Fatal("canceled source did not join its actual owned sender")
		}
		f.resumeOld()
		f.h.settle()
		f.requireHealthyBody(t)
		f.update.stateLock.Lock()
		stable := f.update.synRetry == owner && owner.credit == credit && !credit.released.Load()
		f.update.stateLock.Unlock()
		if !stable {
			t.Fatal("canceled old source changed another source's exact newest paid owner")
		}
		if err := f.h.multiClient.CloseAndWait(context.Background()); err != nil {
			t.Fatal(err)
		}
	})
}

// Completion order is not successful source-admission order. B dequeues its
// source first but cannot materialize until A's newer native source is retained.
func TestNativeSynCacheLateMaterializationCannotReplaceNewerAdmission(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		cause := ""
		t.Cleanup(func() {
			if cause != "" && !t.Failed() {
				t.Fatal(cause)
			}
		})
		entered, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		unpark := func() { once.Do(func() { close(release) }) }
		var armed atomic.Bool
		f := newNativeSynDispositionOrderFixtureConfigured(t, false, func(name string, settings *ClientSettings, _ *nativeSynDispositionOrderFixture) {
			if name == "b" {
				settings.SendBufferSettings.beforeGroupRetainForTest = func() {
					if armed.CompareAndSwap(true, false) {
						close(entered)
						<-release
					}
				}
			}
		}, [3]int{1, 0, 0})
		t.Cleanup(unpark)
		f.waitWire(t, "b", f.path.SequenceNumber, false)
		f.resumeOld()
		f.waitWire(t, "a", f.path.SequenceNumber, false)
		a, b, c := f.h.channel("a"), f.h.channel("b"), f.h.channel("c")
		older, newer := *f.path, *f.path
		older.SequenceNumber++
		newer.SequenceNumber += 2
		f.h.multiClient.groupRaceCandidatesForTest = func(group *parsedPacketGroup) []*multiClientChannel {
			if !reraceSameFlow(group.ipPath, f.path) {
				return []*multiClientChannel{c}
			}
			if group.ipPath.SequenceNumber == newer.SequenceNumber {
				return []*multiClientChannel{a}
			}
			return []*multiClientChannel{b}
		}
		armed.Store(true)
		if !privateCrossWindowNativeSyn(f.h, &older) {
			t.Fatal("older B source was not publicly admitted")
		}
		timer := time.NewTimer(2500 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-entered:
		case <-timer.C:
			t.Fatal("older B source did not reach its actual pre-materialization boundary")
		}
		f.update.stateLock.Lock()
		var olderGroup *parsedPacketGroup
		for group := f.update.sequenceClaims; group != nil; group = group.collapseNext {
			if group.completionClient == b && group.ipPath.Syn && group.ipPath.SequenceNumber == older.SequenceNumber {
				olderGroup = group
				break
			}
		}
		valid := olderGroup != nil && olderGroup.collapseOrder() != 0 && olderGroup.collapseSource != nil && olderGroup.affinitySourceState&affinitySourceMaterialized == 0 &&
			olderGroup.completionFlags.Load()&(groupCompletionTracked|groupCompletionAdmitted|groupCompletionDequeued|groupCompletionAbandoned) == groupCompletionTracked|groupCompletionAdmitted|groupCompletionDequeued
		olderOrder := uint64(0)
		if olderGroup != nil {
			olderOrder = olderGroup.collapseOrder()
		}
		f.update.stateLock.Unlock()
		if !valid {
			t.Fatal("held writer was not the exact older, dequeued but unmaterialized native B group")
		}
		if !privateCrossWindowNativeSyn(f.h, &newer) {
			t.Fatal("newer A source was not publicly admitted while B stayed held")
		}
		f.waitWire(t, "a", newer.SequenceNumber, false)
		owner, credit := f.cache(t, newer.SequenceNumber, f.budgets["a"])
		f.update.stateLock.Lock()
		newerOrder := f.update.synRetryMaterializedOrder
		f.update.stateLock.Unlock()
		if newerOrder <= olderOrder {
			t.Fatal("newer real admission did not have a later dequeue order")
		}
		unpark()
		f.waitWire(t, "b", older.SequenceNumber, false)
		f.requireHealthyBody(t)
		f.update.stateLock.Lock()
		stable := f.update.synRetry == owner && owner.credit == credit && !credit.released.Load() && f.update.synRetryMaterializedOrder == newerOrder
		f.update.stateLock.Unlock()
		if !stable {
			cause = "SYN_CACHE_COMPLETION_ORDER_REPLACED_NEWER_ADMISSION: delayed older materialization revoked the newer real source/cache"
		}
		if err := f.h.multiClient.CloseAndWait(context.Background()); err != nil {
			t.Fatal(err)
		}
	})
}
