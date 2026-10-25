// Public native/Transfer controls pin the retry's ownership, generation and
// bounded failure behavior. No test-only source admission replaces the queue.
package connect

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"testing/synctest"
	"unsafe"

	"github.com/urnetwork/connect/protocol"
)

func synRetryControlHarness(t *testing.T, port int, configure func(*ClientSettings), version int) (*reraceHarness, *IpPath) {
	t.Helper()
	path := reraceFlow(4, 0, port)
	harness := newSilenceInferenceHarness(t, []reraceExitSpec{
		{name: "a", tier: 0, answer: reraceAlways(reraceSilent), clientSettings: configure},
		{name: "b", tier: 1, answer: func(candidate *IpPath) reraceAnswer {
			if reraceSameFlow(candidate, path) {
				return reraceSilent
			}
			return reraceAccept
		}, clientSettings: configure},
	}, func(settings *MultiClientSettings) { settings.ProtocolVersion = version })
	return harness, path
}

func synRetryControlOffer(t *testing.T, harness *reraceHarness, packet []byte) bool {
	t.Helper()
	accepted := [1]bool{}
	count := harness.multiClient.SendNativePacketBatchWithResults(harness.source, protocol.ProvideMode_Network, [][]byte{packet}, 0, accepted[:])
	harness.settle()
	if count < 0 || count > 1 || (count == 1) != accepted[0] {
		t.Fatal("native retry control has inconsistent aggregate/packet ownership")
	}
	return accepted[0]
}

func synRetryControlOwner(t *testing.T, update *multiClientChannelUpdate) *tcpSynRetry {
	t.Helper()
	update.stateLock.Lock()
	defer update.stateLock.Unlock()
	owner := update.synRetry
	if owner == nil || owner.packet == nil || owner.credit == nil || owner.references != 1 || owner.serial == 0 {
		t.Fatal("actual SYN source did not retain one bounded owner")
	}
	return owner
}

// The blocked worker owns one reference, outside every parent/flow lock.
func synRetryControlHold(t *testing.T, harness *reraceHarness) (<-chan struct{}, func()) {
	t.Helper()
	entered, release := make(chan struct{}, 1), make(chan struct{})
	var once sync.Once
	finish := func() { once.Do(func() { close(release) }) }
	t.Cleanup(finish)
	harness.multiClient.beforeSynRetryDispatchForTest = func(*multiClientChannelUpdate) {
		select {
		case entered <- struct{}{}:
		default:
		}
		select {
		case <-release:
		case <-harness.multiClient.ctx.Done():
		}
	}
	return entered, finish
}

func synRetryControlEntered(t *testing.T, entered <-chan struct{}) {
	t.Helper()
	select {
	case <-entered:
	default:
		t.Fatal("explicit failure did not reach the owned retry worker")
	}
}

func synRetryControlReleased(t *testing.T, owner *tcpSynRetry) {
	t.Helper()
	if owner.packet != nil || owner.credit != nil || owner.update != nil || owner.references != 0 {
		t.Fatal("retired retry retained bytes, credit or a flow graph")
	}
}

func TestNativeSynRetryRepeatedSignalsCoalesce(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		harness, path := synRetryControlHarness(t, 51302, nil, DefaultProtocolVersion)
		if !privateStaleBoundNativeSyn(t, harness, path) {
			t.Fatal("initial native SYN refused")
		}
		update, a := harness.update(path), harness.channel("a")
		owner := synRetryControlOwner(t, update)
		entered, release := synRetryControlHold(t, harness)
		for range 3 {
			harness.inject("a", reraceProviderSignal(t, path))
		}
		synRetryControlEntered(t, entered)
		if harness.metrics().DialFailuresIntercepted != 3 || a.dialFailureCount() != 1 || len(harness.exit("b").syns(path)) != 0 {
			t.Fatal("repeated signal created source admission or duplicate provider strikes")
		}
		update.stateLock.Lock()
		references := owner.references
		update.stateLock.Unlock()
		if references != 2 {
			t.Fatal("coalesced worker/cache reference count differs")
		}
		release()
		harness.settle()
		if len(harness.exit("b").syns(path)) != 1 || len(harness.exit("a").syns(path)) != 1 {
			t.Fatal("one real explicit failure did not produce exactly one fresh source")
		}
		harness.inject("b", reraceSynAck(path))
		synRetryControlReleased(t, owner)
	})
}

func TestNativeSynRetryReplacementRefusalDoesNotSpin(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		credit := newNativeBatchCreditFixture(t)
		harness, path := synRetryControlHarness(t, 51303, nil, DefaultProtocolVersion)
		if !privateStaleBoundNativeSyn(t, harness, path) {
			t.Fatal("initial native SYN refused")
		}
		update := harness.update(path)
		owner := synRetryControlOwner(t, update)
		started := credit.base.started.Load()
		harness.multiClient.groupRaceCandidatesForTest = func(*parsedPacketGroup) []*multiClientChannel { return []*multiClientChannel{credit.base.selected} }
		harness.inject("a", reraceProviderSignal(t, path))
		for range 3 {
			harness.inject("a", reraceProviderSignal(t, path))
		}
		if credit.base.started.Load() != started+1 || !credit.source.resendCapacityUnavailable.Load() || credit.base.selected.dialFailureCount() != 0 || credit.base.selected.IsDone() {
			t.Fatal("bounded local refusal retried, struck or destroyed a healthy carrier")
		}
		update.stateLock.Lock()
		valid := update.synRetry == owner && owner.references == 1 && !owner.queued && update.client.Load() == nil && !update.receivedInbound.Load()
		update.stateLock.Unlock()
		if !valid {
			t.Fatal("local refusal advanced replacement ownership or lost bounded original custody")
		}
	})
}

func TestNativeSynRetrySteadyTrafficPreparesNothing(t *testing.T) {
	parent := &RemoteUserNatMultiClient{flowReaperDone: make(chan struct{})}
	for _, protocol := range []IpProtocol{IpProtocolUdp, IpProtocolTcp} {
		path := &IpPath{Protocol: protocol}
		update := &multiClientChannelUpdate{}
		update.receivedInbound.Store(true)
		group := &parsedPacketGroup{ipPath: path}
		allocations := testing.AllocsPerRun(1000, func() { group.prepareSynRetry(TransferPath{}, 0, parent, update, path) })
		if allocations != 0 || group.synRetry != nil {
			t.Fatalf("steady protocol%v allocated retry request: allocations=%v", protocol, allocations)
		}
	}
}

func nativeSynRetryGenerationCase(t *testing.T, name string) {
	t.Helper()
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		harness, path := synRetryControlHarness(t, 51304, nil, DefaultProtocolVersion)
		if !privateStaleBoundNativeSyn(t, harness, path) {
			t.Fatal("initial native SYN refused")
		}
		update := harness.update(path)
		owner := synRetryControlOwner(t, update)
		switch name {
		case "wrong-source":
			harness.inject("b", reraceProviderSignal(t, path))
			if harness.boundTo(path) != harness.channel("a") || len(harness.exit("b").syns(path)) != 0 || synRetryControlOwner(t, update) != owner || harness.channel("b").dialFailureCount() != 0 {
				t.Fatal("a wrong source gained retry/strike authority")
			}
		case "established":
			harness.inject("a", reraceSynAck(path))
			synRetryControlReleased(t, owner)
			harness.inject("a", reraceProviderSignal(t, path))
			if !update.receivedInbound.Load() || harness.boundTo(path) != harness.channel("a") || len(harness.exit("b").syns(path)) != 0 {
				t.Fatal("an established source moved on a late failure")
			}
		default:
			entered, release := synRetryControlHold(t, harness)
			harness.inject("a", reraceProviderSignal(t, path))
			synRetryControlEntered(t, entered)
			if name == "closed-generation" {
				update.Close()
			}
			next := *path
			next.SequenceNumber += 100
			if !synRetryControlOffer(t, harness, MessagePoolCopy(reraceTcpPacket(&next, tcpFlagSyn, next.SequenceNumber, 0, nil))) {
				t.Fatal("replacement native generation refused")
			}
			current := harness.update(path)
			if name == "closed-generation" && current == update || name == "new-SYN" && current != update {
				t.Fatal("generation premise did not reach its intended canonical owner")
			}
			release()
			harness.settle()
			synRetryControlReleased(t, owner)
			syns := harness.exit("b").syns(path)
			if len(syns) != 1 || syns[0].path.SequenceNumber != next.SequenceNumber {
				t.Fatal("queued stale retry offered its original SYN over the accepted generation")
			}
			current.stateLock.Lock()
			unchanged := current.synRetry != nil && current.synRetry.sequence == next.SequenceNumber && current.dialFailedSyn == next.SequenceNumber
			current.stateLock.Unlock()
			if !unchanged {
				t.Fatal("stale retry changed current SYN ownership or attempt metadata")
			}
		}
	})
}

func TestNativeSynRetryWrongSourceCannotArm(t *testing.T) {
	nativeSynRetryGenerationCase(t, "wrong-source")
}

func TestNativeSynRetryEstablishedSourceSurvives(t *testing.T) {
	nativeSynRetryGenerationCase(t, "established")
}

func TestNativeSynRetryNewSynRevokesQueuedOldRetry(t *testing.T) {
	nativeSynRetryGenerationCase(t, "new-SYN")
}

func TestNativeSynRetryClosedGenerationRejectsQueuedRetry(t *testing.T) {
	nativeSynRetryGenerationCase(t, "closed-generation")
}

func nativeSynRetryCloseCase(t *testing.T, name string) {
	t.Helper()
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		harness, path := synRetryControlHarness(t, 51305, nil, DefaultProtocolVersion)
		if !privateStaleBoundNativeSyn(t, harness, path) {
			t.Fatal("initial native SYN refused")
		}
		update := harness.update(path)
		owner := synRetryControlOwner(t, update)
		entered, release := synRetryControlHold(t, harness)
		harness.inject("a", reraceProviderSignal(t, path))
		synRetryControlEntered(t, entered)
		if name == "cancel" {
			if err := harness.multiClient.CloseAndWait(context.Background()); err != nil {
				t.Fatal(err)
			}
		} else {
			flags := byte(tcpFlagAck | tcpFlagFin)
			if name == "RST" {
				flags = tcpFlagRst
			}
			if !synRetryControlOffer(t, harness, MessagePoolCopy(reraceTcpPacket(path, flags, path.SequenceNumber+1, reraceServerIsn+1, nil))) {
				t.Fatal("native close control did not enter a real source")
			}
			update.stateLock.Lock()
			waiting := update.synRetry == nil && owner.references == 1 && owner.packet != nil && owner.credit != nil
			update.stateLock.Unlock()
			if !waiting {
				t.Fatal("close failed to revoke cache or returned credit still owned by queued worker")
			}
			release()
			harness.settle()
		}
		synRetryControlReleased(t, owner)
		if len(harness.exit("b").syns(path)) != 0 {
			t.Fatal("retired retry emitted an old SYN after close")
		}
	})
}

func TestNativeSynRetryFinReleasesQueuedCredit(t *testing.T) { nativeSynRetryCloseCase(t, "FIN") }

func TestNativeSynRetryRstReleasesQueuedCredit(t *testing.T) { nativeSynRetryCloseCase(t, "RST") }

func TestNativeSynRetryParentJoinReleasesQueuedCredit(t *testing.T) {
	nativeSynRetryCloseCase(t, "cancel")
}

func nativeSynRetryCopyCase(t *testing.T, version int) {
	t.Helper()
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		configure := func(settings *ClientSettings) {
			settings.ProtocolVersion = version
			settings.SendBufferSettings.ProtocolVersion = version
		}
		harness, path := synRetryControlHarness(t, 51306, configure, version)
		payload := []byte("owned-fast-open-payload")
		expected := reraceTcpPacket(path, tcpFlagSyn, path.SequenceNumber, 0, payload)
		original := MessagePoolGet(64 * 1024)
		copy(original, expected)
		if !synRetryControlOffer(t, harness, original[:len(expected)]) {
			t.Fatal("large-backing original refused")
		}
		owner := synRetryControlOwner(t, harness.update(path))
		if !bytes.Equal(owner.packet, expected) || cap(owner.packet) >= cap(original) || &owner.packet[0] == &original[0] {
			t.Fatal("retained SYN borrowed a large native root or truncated packet bytes")
		}
		metadata := (ByteCount(unsafe.Sizeof(tcpSynRetry{})) + 63) / 64 * 64
		minimum := addReceiveQueueByteCount(retainedMessageCapacity(ByteCount(len(owner.packet))), metadata)
		minimum = addReceiveQueueByteCount(minimum, transferRetainedControlCreditByteCount())
		if owner.credit.byteCount < minimum {
			t.Fatal("copy backing or complete retry metadata was not charged")
		}
		harness.inject("a", reraceProviderSignal(t, path))
		syns := harness.exit("b").syns(path)
		if len(syns) != 1 || !bytes.Equal(syns[0].payload, payload) {
			t.Fatal("retry lost original TFO payload or replayed more than once")
		}
		harness.inject("b", reraceTcpPacket(path.Reverse(), tcpFlagSyn|tcpFlagAck, reraceServerIsn, path.SequenceNumber+1+uint32(len(payload)), nil))
		synRetryControlReleased(t, owner)
	})
}

func TestNativeSynRetryLegacyCopiesBackingAndPreservesTfo(t *testing.T) { nativeSynRetryCopyCase(t, 1) }

func TestNativeSynRetryRawCopiesBackingAndPreservesTfo(t *testing.T) {
	nativeSynRetryCopyCase(t, DefaultProtocolVersion)
}

func TestNativeSynRetryShared32BudgetCreditsEveryOwner(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		// Match the C21 SDK32 transfer-root ceiling and use one composed source
		// pool across both generated Clients. No per-source budget is minted.
		root := NewTransferMemoryBudget(32 * 1024 * 1024 * 65 / 100)
		shared := NewTransferMemoryBudgetWithParent(root.TotalByteCount(), root)
		t.Cleanup(func() {
			reserved, released := shared.Counts()
			if shared.UsedByteCount() != 0 || root.UsedByteCount() != 0 || reserved != released {
				t.Errorf("joined SYN retry budget unbalanced: used=%d root=%d reserved=%d released=%d", shared.UsedByteCount(), root.UsedByteCount(), reserved, released)
			}
		})
		configure := func(settings *ClientSettings) {
			settings.SendBufferSettings.ResendQueueBudget = shared
			settings.SendBufferSettings.ResendQueueRetainedByteAccounting = true
		}
		harness, path := synRetryControlHarness(t, 51307, configure, DefaultProtocolVersion)
		if !privateStaleBoundNativeSyn(t, harness, path) {
			t.Fatal("shared-budget original refused")
		}
		owner := synRetryControlOwner(t, harness.update(path))
		credit := owner.credit
		if credit.budget != shared || credit.local != nil || shared.UsedByteCount() < credit.byteCount || root.UsedByteCount() > root.TotalByteCount() {
			t.Fatal("SYN cache bypassed its existing shared retained budget")
		}
		harness.inject("a", reraceProviderSignal(t, path))
		if len(harness.exit("b").syns(path)) != 1 {
			t.Fatal("shared-budget recovery did not reach real B source")
		}
		harness.inject("b", reraceSynAck(path))
		synRetryControlReleased(t, owner)
		if !credit.released.Load() {
			t.Fatal("first real answer did not return exact retained credit")
		}
	})
}

func TestNativeSynRetryRefusedOriginalHasNoAuthority(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		credit := newNativeBatchCreditFixture(t)
		harness, path := synRetryControlHarness(t, 51308, nil, DefaultProtocolVersion)
		harness.multiClient.groupRaceCandidatesForTest = func(*parsedPacketGroup) []*multiClientChannel { return []*multiClientChannel{credit.base.selected} }
		if privateStaleBoundNativeSyn(t, harness, path) {
			t.Fatal("closed actual source queue accepted a SYN")
		}
		update := harness.update(path)
		started := credit.base.started.Load()
		harness.inject("a", reraceProviderSignal(t, path))
		update.stateLock.Lock()
		absent := update.synRetry == nil
		update.stateLock.Unlock()
		if !absent || credit.base.started.Load() != started || harness.channel("a").dialFailureCount() != 0 || len(harness.exit("b").syns(path)) != 0 {
			t.Fatal("an unowned/failed enqueue acquired retry or provider-strike authority")
		}
	})
}

func TestNativeSynRetryWarnedExitExcluded(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		path := reraceFlow(4, 0, 51309)
		harness := newSilenceInferenceHarness(t, []reraceExitSpec{
			{name: "a", tier: 0, answer: reraceAlways(reraceSilent)},
			{name: "b", tier: 1, answer: reraceAlways(reraceSilent)},
			{name: "c", tier: 2, answer: reraceAlways(reraceSilent)},
		}, nil)
		if !privateStaleBoundNativeSyn(t, harness, path) {
			t.Fatal("initial native SYN refused")
		}
		b := harness.channel("b")
		b.stateLock.Lock()
		b.warning = true
		b.stateLock.Unlock()
		harness.inject("a", reraceProviderSignal(t, path))
		if len(harness.exit("b").syns(path)) != 0 || len(harness.exit("c").syns(path)) != 1 {
			t.Fatal("owned recovery selected the warned fallback ahead of a healthy exit")
		}
	})
}

func TestNativeSynRetryRemovalReleasesResetCache(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		harness, path := synRetryControlHarness(t, 51310, nil, DefaultProtocolVersion)
		if !privateStaleBoundNativeSyn(t, harness, path) {
			t.Fatal("initial native SYN refused")
		}
		update := harness.update(path)
		owner := synRetryControlOwner(t, update)
		// Removal alone must release a cache which has no queued retry reference.
		harness.multiClient.removeClient(harness.channel("a"))
		harness.settle()
		synRetryControlReleased(t, owner)
		if !update.sourceReset.Load() || update.synRetry != nil || len(harness.exit("b").syns(path)) != 0 {
			t.Fatal("removal retained or replayed the reset source's SYN")
		}
	})
}

func TestNativeSynRetryFormerSourceRemovalKeepsQueuedAuthority(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		harness, path := synRetryControlHarness(t, 51312, nil, DefaultProtocolVersion)
		if !privateStaleBoundNativeSyn(t, harness, path) {
			t.Fatal("initial native SYN refused")
		}
		update, a := harness.update(path), harness.channel("a")
		owner := synRetryControlOwner(t, update)
		entered, release := synRetryControlHold(t, harness)
		harness.inject("a", reraceProviderSignal(t, path))
		synRetryControlEntered(t, entered)
		harness.multiClient.removeClient(a)
		if err := a.client.CloseAndWait(context.Background()); err != nil {
			t.Fatal(err)
		}
		harness.settle()
		update.stateLock.Lock()
		valid := update.synRetry == owner && owner.references == 2 && owner.packet != nil && owner.credit != nil && !update.sourceReset.Load()
		update.stateLock.Unlock()
		if !valid {
			t.Fatal("former source removal revoked canonical copied-SYN authority")
		}
		release()
		harness.settle()
		if len(harness.exit("b").syns(path)) != 1 || harness.boundTo(path) != harness.channel("b") {
			t.Fatal("closing the old source prevented healthy replacement custody")
		}
		harness.inject("b", reraceSynAck(path))
		synRetryControlReleased(t, owner)
	})
}

func TestNativeSynRetryActualSendErrorReleasesCache(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		harness, path := synRetryControlHarness(t, 51311, nil, DefaultProtocolVersion)
		if !privateStaleBoundNativeSyn(t, harness, path) {
			t.Fatal("initial native SYN refused")
		}
		update, a := harness.update(path), harness.channel("a")
		owner := synRetryControlOwner(t, update)
		// Close actual Transfer admission while its Client/route remains live.
		// A new native segment reaches the existing nonnil send-error reset.
		a.client.sendBuffer.Close()
		harness.settle()
		if update.synRetry != owner || update.sourceReset.Load() {
			t.Fatal("buffer-close premise reset the flow before the public send")
		}
		if synRetryControlOffer(t, harness, MessagePoolCopy(reraceTcpPacket(path, tcpFlagAck, path.SequenceNumber+1, reraceServerIsn+1, []byte{1}))) {
			t.Fatal("closed real Transfer sender accepted a segment")
		}
		if !update.sourceReset.Load() || update.client.Load() != nil {
			t.Fatal("actual send failure did not reach the source-reset transition")
		}
		synRetryControlReleased(t, owner)
		if len(harness.exit("b").syns(path)) != 0 {
			t.Fatal("fatal send reset acquired retry authority")
		}
	})
}
