// Adjacent controls distinguish legal metadata refresh from changed attempts,
// and keep real discovery/credit ownership across immutable cache replacement.
package connect

import (
	"bytes"
	"encoding/binary"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Complete checksums are regenerated only for deliberately valid wire changes.
func synMetadataTestRepairChecksums(packet []byte, version int) {
	offset := Ipv6HeaderSize
	if version == 4 {
		offset = int(packet[0]&15) * 4
		writeIpv4HeaderChecksum(packet[:offset])
	}
	tcp := packet[offset:]
	binary.BigEndian.PutUint16(tcp[16:18], 0)
	var path *IpPath
	if version == 4 {
		path = &IpPath{Version: 4, SourceIp: packet[12:16], DestinationIp: packet[16:20]}
	} else {
		path = &IpPath{Version: 6, SourceIp: packet[8:24], DestinationIp: packet[24:40]}
	}
	binary.BigEndian.PutUint16(tcp[16:18], ipPathTransportChecksum(path, ipProtocolNumberTcp, tcp))
}

// Only the enumerated metadata bytes may differ, with both full checksums
// valid. Allocation remains zero even with ordinary TCP payload present.
func TestSynMetadataContinuityRequiresValidExactRemainder(t *testing.T) {
	for _, version := range []int{4, 6} {
		path := reraceFlow(version, 89, 53520+version)
		previous := synMetadataTestPacket(path, 500, 700, []byte("same synthetic early bytes"))
		refreshed := synMetadataTestPacket(path, 501, 701, []byte("same synthetic early bytes"))
		if !sameSynRetryMetadata(previous, refreshed) || !sameSynRetryMetadata(refreshed, previous) {
			t.Fatalf("version=%d: legal timestamp/identification refresh rejected", version)
		}
		if got := testing.AllocsPerRun(100, func() { sameSynRetryMetadata(previous, refreshed) }); got != 0 {
			t.Fatalf("version=%d: native comparator allocated %v", version, got)
		}
		offset := Ipv6HeaderSize
		if version == 4 {
			offset = Ipv4HeaderSizeWithoutExtensions
			idOnly := synMetadataTestPacket(path, 500, 701, []byte("same synthetic early bytes"))
			if !sameSynRetryMetadata(previous, idOnly) {
				t.Fatal("checksummed IPv4 identification-only retry rejected")
			}
		}
		for _, change := range []struct {
			name   string
			index  int
			repair bool
		}{
			{name: "source port", index: offset, repair: true}, {name: "destination port", index: offset + 2, repair: true},
			{name: "new sequence", index: offset + 4, repair: true}, {name: "ack field", index: offset + 8, repair: true},
			{name: "flags", index: offset + 13, repair: true}, {name: "window", index: offset + 14, repair: true},
			{name: "timestamp echo", index: offset + 28, repair: true}, {name: "other option", index: offset + 20, repair: true},
			{name: "payload", index: offset + 32, repair: true}, {name: "bad TCP checksum", index: offset + 16},
			{name: "bad timestamp checksum", index: offset + 24}, {name: "IP field", index: 1, repair: true},
		} {
			next := bytes.Clone(refreshed)
			next[change.index] ^= 1
			if change.repair {
				synMetadataTestRepairChecksums(next, version)
			}
			if sameSynRetryMetadata(previous, next) {
				t.Fatalf("version=%d change=%s: unrelated or corrupt bytes acquired continuity", version, change.name)
			}
		}
		badLength := bytes.Clone(refreshed)
		badLength[offset+23] = 9
		synMetadataTestRepairChecksums(badLength, version)
		if sameSynRetryMetadata(previous, badLength) || sameSynRetryMetadata(previous, refreshed[:len(refreshed)-1]) {
			t.Fatalf("version=%d: malformed timestamp or truncated carrier accepted", version)
		}
		if version == 4 {
			badIp := bytes.Clone(refreshed)
			badIp[10] ^= 1
			if sameSynRetryMetadata(previous, badIp) {
				t.Fatal("corrupt IPv4 header acquired metadata continuity")
			}
		}
	}
}

// Metadata equivalence must still pass each existing flow/source/generation
// fence before an ordinary native descriptor receives its scalar attestation.
func TestNativeSynMetadataRefreshKeepsIdentityAndControlFences(t *testing.T) {
	for _, change := range []struct {
		name  string
		alter func(*nativeSynAttestationFixture)
	}{
		{name: "source", alter: func(f *nativeSynAttestationFixture) { f.source.SourceId = NewId() }},
		{name: "mode", alter: func(f *nativeSynAttestationFixture) { f.owner.provideMode = 1 }},
		{name: "pin", alter: func(f *nativeSynAttestationFixture) { f.group.pin.appId += "-other" }},
		{name: "generation", alter: func(f *nativeSynAttestationFixture) { f.update.synGenerationNumber++ }},
		{name: "reset", alter: func(f *nativeSynAttestationFixture) { f.update.sourceReset.Store(true) }},
		{name: "answered", alter: func(f *nativeSynAttestationFixture) { f.update.receivedInbound.Store(true) }},
		{name: "closed", alter: func(f *nativeSynAttestationFixture) { f.cancel() }},
	} {
		f := newNativeSynAttestationFixture(t, 4)
		f.owner.packet = synMetadataTestPacket(f.group.ipPath, 1, 1, nil)
		f.group.packets[0].packet = synMetadataTestPacket(f.group.ipPath, 2, 2, nil)
		f.prepare(f.group)
		f.requireAttested(t, f.group, change.name)
		f.group.synRetry = nil
		change.alter(f)
		f.prepare(f.group)
		if request := f.group.synRetry; request != nil && (request.nativeOwnerSerial != 0 || request.update != nil) {
			t.Fatalf("%s: timestamp refresh bypassed the independent identity fence", change.name)
		}
	}
}

// The second actual Pack owns its charge before optional successor admission
// sees no spare capacity. Keeping the paid old image never invents latest bytes.
// An early-data SYN is DATA-funded; pure protocol-only SYN metadata is
// intentionally exempt from byte credit and cannot prove this denial premise.
func TestNativeSynMetadataRefreshExactBudgetKeepsPaidHistory(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		root := NewTransferMemoryBudget(8 * 1024 * 1024)
		budget := NewTransferMemoryBudgetWithParent(root.TotalByteCount(), root)
		t.Cleanup(func() {
			for _, ledger := range []*TransferMemoryBudget{budget, root} {
				r, l := ledger.Counts()
				if ledger.UsedByteCount() != 0 || r != l {
					t.Error("exact-budget native cleanup retained paid bytes")
				}
			}
		})
		var denied atomic.Int32
		var original *tcpSynRetry
		var credit *transferRetainedControlCredit
		var originalPacket []byte
		var history []Id
		wire := make(chan []byte, 16)
		configure := func(settings *ClientSettings) {
			settings.SendBufferSettings.ResendQueueBudget = budget
			settings.SendBufferSettings.ResendQueueRetainedByteAccounting = true
			settings.SendBufferSettings.afterGroupDispositionForTest = func(source *SendSequence, disposition sendGroupDisposition, frames []*protocol.Frame) {
				for _, frame := range frames {
					if frame == nil || frame.MessageType != protocol.MessageType_IpIpPacketToProvider {
						continue
					}
					packet, err := ipPacketToProviderBytes(frame)
					if err != nil || len(packet) != 53 || binary.BigEndian.Uint32(packet[44:48]) != 102 {
						continue
					}
					if source.resendQueue.Budget() != budget {
						t.Error("metadata Pack changed its actual source budget")
						return
					}
					budget.SetTotalByteCount(budget.UsedByteCount())
					root.SetTotalByteCount(root.UsedByteCount())
					denied.Add(1)
				}
			}
		}
		options := &synPublicationPacketOptions{
			packet: func(path *IpPath, ordinal int) []byte {
				return synMetadataTestPacket(path, uint32(100+ordinal), uint16(200+ordinal), []byte("x"))
			},
			beforeRetry: func(f *synPublicationTestFixture) {
				original, credit = f.owner, f.owner.credit
				originalPacket, history = bytes.Clone(original.packet), slices.Clone(original.refusedRemoteIds)
				for _, exit := range f.h.generator.exitList() {
					exit.provider.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, _ Peer) {
						for _, frame := range frames {
							if frame.MessageType != protocol.MessageType_IpIpPacketToProvider {
								continue
							}
							packet, err := ipPacketToProviderBytes(frame)
							if err == nil && len(packet) == 53 && binary.BigEndian.Uint32(packet[44:48]) == 102 {
								wire <- bytes.Clone(packet)
							}
						}
					})
				}
			},
			afterRetry: func(f *synPublicationTestFixture) {
				f.update.stateLock.Lock()
				valid := f.update.synRetry == original && original.credit == credit && !credit.released.Load() &&
					bytes.Equal(original.packet, originalPacket) && slices.Equal(original.refusedRemoteIds, history) &&
					f.update.synRetryHasRemoteRefusalWithLock()
				f.update.stateLock.Unlock()
				if denied.Load() == 0 || len(wire) == 0 || !valid {
					t.Fatal("unfunded metadata successor lost or mutated the old paid cache/history, or hid ordinary wire progress")
				}
				for len(wire) != 0 {
					if !bytes.Equal(<-wire, synMetadataTestPacket(f.path, 102, 202, []byte("x"))) {
						t.Fatal("ordinary native retry sent different wire bytes")
					}
				}
				root.SetTotalByteCount(8 * 1024 * 1024)
				budget.SetTotalByteCount(root.TotalByteCount())
			},
		}
		f := newSynPublicationPacketFixture(t, 4, false, configure, options)
		beforeNative := f.nativeOffers.Load()
		f.resume()
		synctest.Wait()
		f.requireAnswer(t)
		if f.nativeOffers.Load() != beforeNative || !credit.released.Load() {
			t.Fatal("paid fallback required another source offer or leaked its exact credit")
		}
		t.Log("SYN_METADATA_EXACT_BUDGET_OLD_PAID_IMAGE_RECOVERED")
	})
}

// A real full-window acquisition owns the old immutable cache while native
// TSVal changes. Its exclusions and final candidate must use continuous history.
func TestNativeSynMetadataRefreshDuringHeldServiceAcquisition(t *testing.T) {
	testSynMetadataHeldServiceAcquisition(t, "continuous")
}

// Returning to old bytes after different payload cannot revive old provenance.
func TestNativeSynMetadataHeldServiceRejectsPayloadAba(t *testing.T) {
	testSynMetadataHeldServiceAcquisition(t, "payload-aba")
}

// Canceling the source while its old acquisition owns a reference stays final.
func TestNativeSynMetadataHeldServiceCancellationReleasesBothOwners(t *testing.T) {
	testSynMetadataHeldServiceAcquisition(t, "cancel")
}

// A borrowed service snapshot may survive its window reference. Its reader
// races the actual final graph release, while the successor remains paid.
func TestNativeSynMetadataRetiredServiceSnapshotIgnoresReleasedGraph(t *testing.T) {
	t.Cleanup(func() { t.Log("SYN_METADATA_RETIRED_SNAPSHOT_FIXTURE_CLEANUP_JOINED") })
	testSynMetadataHeldServiceAcquisition(t, "retired-snapshot")
}

// The continuous retry has no new refusal feedback. The ABA middle image gets
// actual feedback to vacate its race before the final silent return to old bytes.
func testSynMetadataHeldServiceAcquisition(t *testing.T, mode string) {
	t.Helper()
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		path := unboundFlowTcpPath(53531)
		var silent atomic.Bool
		specs := make([]reraceExitSpec, 5)
		for index := range specs {
			specs[index] = reraceExitSpec{name: string(rune('a' + index)), answer: func(candidate *IpPath) reraceAnswer {
				if reraceSameFlow(candidate, path) {
					if silent.Load() {
						return reraceSilent
					}
					return reraceSignal
				}
				return reraceAccept
			}}
		}
		h, generator := newDiscoveryDemandTestHarness(t, specs, func(settings *MultiClientSettings) { settings.StrictWindowHandover = true })
		window, siblingPaths, siblingOwners := discoveryDemandTestCarryFour(t, h)
		if len(window.unorderedClients()) != 5 {
			t.Fatal("metadata service fixture lost the original full strict window")
		}
		generator.mutex.Lock()
		generator.holdNextEmpty = true
		generator.mutex.Unlock()
		first := synMetadataTestPacket(path, 301, 401, nil)
		if !synRetryControlOffer(t, h, MessagePoolCopy(first)) {
			t.Fatal("initial service SYN was refused")
		}
		select {
		case <-generator.emptyEntered:
		default:
			t.Fatal("service acquisition did not reach its actual empty-listing barrier")
		}
		update := h.update(path)
		old := discoveryDemandServiceRefreshOwner(t, window, update)
		window.stateLock.Lock()
		snapshot := serviceRefreshSnapshot{owner: window.serviceRefreshOwner, victim: window.serviceRefreshVictim, stamp: window.serviceRefreshStamp, update: update}
		var deadline time.Time
		if acquisition := window.discoveryDemand.owner; acquisition != nil {
			deadline = acquisition.deadline
		}
		window.stateLock.Unlock()
		if snapshot.owner != old || snapshot.victim == nil || old.references != 2 || !bytes.Equal(old.packet, first) || !time.Now().Before(deadline) {
			t.Fatal("real acquisition did not retain the original paid cache and idle reservation")
		}
		credit, history := old.credit, slices.Clone(old.refusedRemoteIds)
		if len(history) == 0 {
			t.Fatal("held service acquisition lacks actual remote refusals")
		}
		late := h.addExit(reraceExitSpec{name: "late"})
		if mode == "payload-aba" {
			other := synMetadataTestPacket(path, 302, 402, []byte("different admitted early bytes"))
			if !synRetryControlOffer(t, h, MessagePoolCopy(other)) {
				t.Fatal("ABA control did not materialize its distinct payload")
			}
			update.stateLock.Lock()
			middle := update.synRetry
			validMiddle := middle != nil && middle != old && bytes.Equal(middle.packet, other) && middle.credit != nil &&
				!middle.credit.released.Load() && len(middle.refusedRemoteIds) == 4 && update.race == nil
			update.stateLock.Unlock()
			if !validMiddle {
				t.Fatal("ABA control lacks its actual distinct paid middle owner and four real race-clearing refusals")
			}
		}
		silent.Store(true)
		second := synMetadataTestPacket(path, 302, 402, nil)
		if !synRetryControlOffer(t, h, MessagePoolCopy(second)) {
			t.Fatal("metadata retry did not reach the old installed provider set")
		}
		update.stateLock.Lock()
		current := update.synRetry
		valid := current != nil && current != old && current.credit != nil && !current.credit.released.Load() &&
			bytes.Equal(current.packet, second) &&
			(mode == "payload-aba" && len(current.refusedRemoteIds) == 0 || mode != "payload-aba" && slices.Equal(current.refusedRemoteIds, history)) &&
			old.references == 1 && old.credit == credit && !credit.released.Load() && bytes.Equal(old.packet, first) &&
			update.discoveryAttemptStamp.Load() == snapshot.stamp &&
			(mode == "payload-aba" && !update.serviceRefreshRequested && update.serviceRefreshLineage.Load() == 0 ||
				mode != "payload-aba" && update.serviceRefreshRequested && update.serviceRefreshLineage.Load() == old.retryLineage)
		update.stateLock.Unlock()
		if !valid || len(late.syns(path)) != 0 || snapshot.victim.serviceRefreshReservation.Load() != old {
			t.Fatal("metadata refresh corrupted the in-flight immutable acquisition owner")
		}
		if mode == "retired-snapshot" {
			borrowed := window.serviceRefreshSnapshot()
			if borrowed.owner != old || borrowed.update != update {
				t.Fatal("actual service snapshot did not borrow the held original")
			}
			entered, release := make(chan struct{}), make(chan struct{})
			var enterOnce, releaseOnce sync.Once
			resume := func() { releaseOnce.Do(func() { close(release) }) }
			t.Cleanup(resume)
			h.multiClient.afterSequenceParentReadForTest = func(candidate *multiClientChannelUpdate) {
				if candidate != update || old.references != 0 || candidate.synRetry != current || candidate.synRetryReleased.Load() != old {
					return
				}
				enterOnce.Do(func() {
					close(entered)
					<-release
				})
			}
			writerDone := make(chan struct{})
			go func() {
				window.finishServiceRefresh(old)
				close(writerDone)
			}()
			select {
			case <-entered:
			case <-writerDone:
				t.Fatal("actual final service release missed its flow-locked retired-owner boundary")
			}
			readerDone := make(chan struct{})
			var resolved *tcpSynRetry
			go func() {
				borrowed.update.stateLock.Lock()
				resolved = borrowed.update.currentSynRetryLineageWithLock(borrowed.owner)
				borrowed.update.stateLock.Unlock()
				close(readerDone)
			}()
			// The reader and final graph clear both follow the writer's unlock;
			// neither orders the other's post-unlock memory access.
			resume()
			<-readerDone
			<-writerDone
			if resolved != nil || !credit.released.Load() {
				t.Fatal("retired borrowed snapshot retained authority or its original credit")
			}
			synRetryControlReleased(t, old)
			update.stateLock.Lock()
			currentPaid := update.synRetry == current && current.credit != nil && !current.credit.released.Load() &&
				current.references == 1 && bytes.Equal(current.packet, second) && slices.Equal(current.refusedRemoteIds, history)
			update.stateLock.Unlock()
			if !currentPaid {
				t.Fatal("retired-snapshot reader changed the live successor's paid ownership")
			}
			update.Close()
			close(generator.emptyRelease)
			h.settle()
			synRetryControlReleased(t, current)
			discoveryDemandFullWindowAssertSiblings(t, h, siblingPaths, siblingOwners)
			t.Log("SYN_METADATA_RETIRED_SNAPSHOT_RELEASE_JOINED")
			return
		}
		excluded := map[MultiHopId]bool{}
		window.addServiceRefreshExclusions(excluded)
		if mode == "payload-aba" {
			if len(excluded) != 0 || window.serviceRefreshCandidateAllowed(old, &multiClientChannel{args: &multiClientChannelArgs{Destination: late.destination}}) {
				t.Fatal("returned byte image regained an interrupted old service lineage")
			}
		} else {
			for _, id := range history {
				if !excluded[RequireMultiHopId(id)] {
					t.Fatal("held service acquisition lost a previously refused remote exclusion")
				}
			}
		}
		if mode == "cancel" {
			update.Close()
			h.settle()
			_, _, beforeMints, _ := generator.counts()
			close(generator.emptyRelease)
			h.settle()
			_, _, afterMints, _ := generator.counts()
			if afterMints != beforeMints || len(late.syns(path)) != 0 || window.serviceRefreshSnapshot().owner != nil {
				t.Fatal("canceled continuous source revived its acquisition or emitted a SYN")
			}
			synRetryControlReleased(t, old)
			synRetryControlReleased(t, current)
			discoveryDemandFullWindowAssertSiblings(t, h, siblingPaths, siblingOwners)
			return
		}
		evaluated := discoveryDemandTestObservePublication(window)
		close(generator.emptyRelease)
		if !discoveryDemandTestAwaitPublication(t, evaluated, deadline) {
			t.Fatal("continuous service candidate missed the unchanged acquisition bound")
		}
		h.settle()
		if mode == "payload-aba" {
			lateInstalled := false
			for _, client := range window.unorderedClients() {
				if client.Destination() == late.destination && !client.IsDone() {
					lateInstalled = true
				}
			}
			if _, answered := h.synAck(path); answered || len(late.syns(path)) != 0 || lateInstalled {
				t.Fatal("noncontinuous ABA candidate acquired old service publication authority")
			}
			update.Close()
			h.settle()
			synRetryControlReleased(t, old)
			synRetryControlReleased(t, current)
			discoveryDemandFullWindowAssertSiblings(t, h, siblingPaths, siblingOwners)
			return
		}
		lateClient := h.channel("late")
		window.stateLock.Lock()
		installed := lateClient != nil && window.clients[lateClient.ClientId()] == lateClient && len(window.clients) == 5 && window.serviceRefreshOwner == nil
		window.stateLock.Unlock()
		if !installed || lateClient.IsDone() || lateClient.isWarning() || snapshot.victim.serviceRefreshReservation.Load() != &serviceRefreshRetiringOwner {
			t.Fatal("continuous metadata acquisition rejected its real healthy candidate or lost exact idle retirement")
		}
		// This separate control proves acquisition publication. The second
		// native offer may fill every ordinary race slot; it creates no extra slot.
		_, answered := h.synAck(path)
		if !answered {
			update.stateLock.Lock()
			full := update.race != nil && h.multiClient.synRetryRaceSlotsWithLock(update, nil) == 0 && update.synRetry == current
			update.stateLock.Unlock()
			if !full || len(late.syns(path)) != 0 {
				t.Fatal("unanswered service publication lacks its independent full-race premise")
			}
		} else if h.boundTo(path) != lateClient || len(late.syns(path)) != 1 {
			t.Fatal("service publication produced inconsistent answer custody")
		}
		synRetryControlReleased(t, old)
		if !credit.released.Load() {
			t.Fatal("finished acquisition kept old immutable wire credit")
		}
		update.Close()
		h.settle()
		synRetryControlReleased(t, current)
		discoveryDemandFullWindowAssertSiblings(t, h, siblingPaths, siblingOwners)
		t.Logf("SYN_METADATA_HELD_SERVICE_ACQUISITION_JOINED answered=%t", answered)
	})
}
