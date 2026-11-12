// Publication controls keep the real discovery, native admission, retained
// source, provider wire and peer response boundaries composed.
package connect

import (
	"bytes"
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// The late client is installed and healthy, but its real admission callback
// has not run. Both original native offers have returned; only publication
// may wake the already-paid retained source from this point.
type synPublicationTestFixture struct {
	h              *reraceHarness
	generator      *discoveryDemandTestGenerator
	window         *multiClientWindow
	path           *IpPath
	update         *multiClientChannelUpdate
	owner          *tcpSynRetry
	late           *multiClientChannel
	lateExit       *reraceExit
	siblingPaths   []*IpPath
	siblingOwners  []*multiClientChannel
	resume         func()
	callbackDone   <-chan struct{}
	beforeReads    int
	beforeEmpty    int
	beforeMints    int
	beforeRetired  int
	expectedReads  int
	expectedEmpty  int
	nativeOffers   atomic.Int32
	retainedOffers atomic.Int32
}

// Every provider is real. The callback barrier orders publication after the
// source's completed selection without minting, selecting or sending for it.
func newSynPublicationTestFixture(t *testing.T, version int, partial bool, configure func(*ClientSettings), addLateExit ...func(*synPublicationTestFixture) *reraceExit) *synPublicationTestFixture {
	t.Helper()
	return newSynPublicationPacketFixture(t, version, partial, configure, nil, addLateExit...)
}

// Optional test-owned wire construction preserves the original ordering and
// all admission predicates while modeling real native metadata refreshes.
type synPublicationPacketOptions struct {
	silentRepeat      *atomic.Bool
	silentRepeatIndex int
	packet            func(*IpPath, int) []byte
	beforeRetry       func(*synPublicationTestFixture)
	afterRetry        func(*synPublicationTestFixture)
	// Holds the four original providers' frames to their sources while set.
	providerHold *reraceProviderHold
}

// Nil options retain the original public native packet and every old oracle.
func newSynPublicationPacketFixture(t *testing.T, version int, partial bool, configure func(*ClientSettings), options *synPublicationPacketOptions, addLateExit ...func(*synPublicationTestFixture) *reraceExit) *synPublicationTestFixture {
	t.Helper()
	f := &synPublicationTestFixture{path: reraceFlow(version, 88, 52920+version)}
	offerCount := 0
	offer := func() bool {
		offerCount++
		if options == nil || options.packet == nil {
			return privateCrossWindowNativeSyn(f.h, f.path)
		}
		return synRetryControlOffer(t, f.h, MessagePoolCopy(options.packet(f.path, offerCount)))
	}
	var providerHold *reraceProviderHold
	if options != nil {
		providerHold = options.providerHold
	}
	specs := make([]reraceExitSpec, 4)
	for index := range specs {
		position := index
		specs[index] = reraceExitSpec{name: string(rune('a' + index)), clientSettings: configure, providerHold: providerHold,
			answer: func(candidate *IpPath) reraceAnswer {
				if reraceSameFlow(candidate, f.path) {
					if options != nil && options.silentRepeat != nil && options.silentRepeat.Load() && position == options.silentRepeatIndex {
						return reraceSilent
					}
					if partial && position != 0 {
						return reraceSilent
					}
					return reraceSignal
				}
				return reraceAccept
			}}
	}
	f.h, f.generator = newDiscoveryDemandTestHarness(t, specs, func(settings *MultiClientSettings) {
		settings.StrictWindowHandover = true
	})
	f.window, f.siblingPaths, f.siblingOwners = discoveryDemandTestCarryFour(t, f.h)
	f.h.multiClient.beforePacketGroupGateForTest = func(_ *multiClientChannelUpdate, _ *multiClientChannel, group *parsedPacketGroup) {
		if reraceSameFlow(group.ipPath, f.path) {
			if group.isSynRetry() {
				f.retainedOffers.Add(1)
			} else {
				f.nativeOffers.Add(1)
			}
		}
	}
	f.beforeReads, f.beforeEmpty, f.beforeMints, f.beforeRetired = f.generator.counts()
	f.expectedReads, f.expectedEmpty = f.beforeReads+2, f.beforeEmpty+1
	start := time.Now()
	if !offer() {
		t.Fatal("publication fixture's original native SYN was refused")
	}
	f.update = f.h.update(f.path)
	if f.update == nil {
		t.Fatal("publication fixture lost its original canonical source")
	}
	f.owner = synRetryControlOwner(t, f.update)
	f.h.multiClient.stateLock.Lock()
	indexed := f.owner.publicationIndexed && f.h.multiClient.synPublicationOwnerCount > 0
	f.h.multiClient.stateLock.Unlock()
	if !indexed {
		t.Fatal("actual retained source was not present in its paid publication index")
	}
	f.update.stateLock.Lock()
	wantedRefusals := 4
	if partial {
		wantedRefusals = 1
	}
	actualRefusals := len(f.update.dialFailedClients)
	hasCurrentRefusal := f.update.synRetryHasRemoteRefusalWithLock()
	valid := hasCurrentRefusal && actualRefusals == wantedRefusals &&
		!f.update.receivedInbound.Load() && f.update.client.Load() == nil
	// Failure-only scalar receipt; every original predicate and expectation stays.
	var custody map[string]any
	var observedOwner *tcpSynRetry
	var observedRace *multiClientChannelUpdateRace
	var observedClient *multiClientChannel
	if !valid {
		observedOwner, observedRace, observedClient = f.update.synRetry, f.update.race, f.update.client.Load()
		custody = map[string]any{
			"has_current_refusal_at_oracle": hasCurrentRefusal,
			"actual_refusals":               actualRefusals,
			"wanted_refusals":               wantedRefusals,
			"signaled_clients":              len(f.update.dialFailureSignaledClients),
			"update_done":                   f.update.IsDone(),
			"source_reset":                  f.update.sourceReset.Load(),
			"received_inbound":              f.update.receivedInbound.Load(),
			"bound_client_present":          observedClient != nil,
			"affinity_closed":               f.update.affinitySources.closed,
			"pending_collapse_reset":        f.update.pendingCollapseResetWithLock(),
			"pending_collapse_close":        f.update.pendingCollapseCloseWithLock(),
			"syn_generation_seen":           f.update.synGenerationSeen,
			"syn_generation_number":         f.update.synGenerationNumber,
			"syn_generation_end":            f.update.synGenerationEnd,
			"syn_generation_awaiting":       f.update.synGenerationAwaiting,
			"dial_failed_syn_seen":          f.update.dialFailedSynSeen,
			"dial_failed_syn":               f.update.dialFailedSyn,
			"dial_failed_syn_end":           f.update.dialFailedSynEnd,
			"sequence_admission_epoch":      f.update.sequenceAdmissionEpoch,
			"sequence_source_order":         f.update.sequenceSourceOrder,
			"sequence_close_ready":          f.update.sequenceCloseReady.Load(),
			"sequence_syn_offers":           f.update.sequenceSynOffers,
			"owner_present":                 observedOwner != nil,
			"owner_same_fixture":            observedOwner == f.owner,
			"race_present":                  observedRace != nil,
		}
		if owner := observedOwner; owner != nil {
			custody["owner_update_same"] = owner.update == f.update
			custody["owner_valid_after_oracle"] = owner.validWithLock(f.update)
			custody["owner_pending_conflicting_control"] = owner.pendingConflictingControlWithLock(f.update)
			custody["owner_serial"] = owner.serial
			custody["owner_sequence"] = owner.sequence
			custody["owner_sequence_matches_failure"] = owner.sequence == f.update.dialFailedSyn
			custody["owner_sequence_matches_generation"] = !f.update.synGenerationSeen || owner.sequence == f.update.synGenerationNumber
			custody["owner_references"] = owner.references
			custody["owner_packet_bytes"] = len(owner.packet)
			custody["owner_credit_present"] = owner.credit != nil
			custody["owner_publication_retired"] = owner.publicationRetired.Load()
			custody["remote_refusal_count"] = len(owner.refusedRemoteIds)
			custody["remote_refusal_capacity"] = cap(owner.refusedRemoteIds)
			custody["remote_refusal_overflow"] = owner.remoteRefusalOverflow
			if owner.credit != nil {
				custody["owner_credit_released"] = owner.credit.released.Load()
			}
		}
		if race := observedRace; race != nil {
			custody["race_single"] = race.single
			custody["race_client_limit"] = race.clientLimit
			custody["race_client_states"] = len(race.clientStates)
			custody["race_pending_send_count"] = race.pendingSendCount
			custody["race_collapse_order"] = race.collapseOrder
			custody["race_syn_retry_pending"] = race.synRetryPending
		}
	}
	f.update.stateLock.Unlock()
	reads, empty, _, _ := f.generator.counts()
	if !valid {
		t.Fatalf("publication fixture lacks current refusal custody: partial=%t refused=%d want=%d reads=%d empty=%d race=%p owner=%p expected_owner=%p client=%p custody=%v", partial, actualRefusals, wantedRefusals, reads-f.beforeReads, empty-f.beforeEmpty, observedRace, observedOwner, f.owner, observedClient, custody)
	}
	if partial {
		// Three live contenders have not refused this source. Their still-young
		// attempt has no observational read authority, unlike an exhausted field.
		if reads != f.beforeReads || empty != f.beforeEmpty {
			t.Fatal("SYN_PUBLICATION_PARTIAL_DEMAND_PREMATURE: one refusal triggered discovery before the attempt-age boundary")
		}
		if _, eligible := f.update.observeDiscoveryAttempt(f.siblingOwners, time.Now(), f.path); eligible {
			t.Fatal("partial race acquired discovery authority at its initial source age")
		}
		f.expectedReads, f.expectedEmpty = f.beforeReads+1, f.beforeEmpty
	} else if reads != f.beforeReads+1 || empty != f.beforeEmpty+1 {
		t.Fatal("exhausted publication fixture lacks its completed empty listing")
	}
	f.generator.mutex.Lock()
	f.generator.holdNextAvailable = true
	f.generator.mutex.Unlock()
	if len(addLateExit) == 0 {
		f.lateExit = f.h.addExit(reraceExitSpec{name: "late", clientSettings: configure})
	} else {
		f.lateExit = addLateExit[0](f)
	}
	var partialRace *multiClientChannelUpdateRace
	var observedAt time.Time
	if partial {
		f.update.stateLock.Lock()
		partialRace = f.update.race
		observedAt = f.update.discoveryAttemptStarted.Add(inferredDialFailureTimeout)
		f.update.stateLock.Unlock()
		before := observedAt.Add(-time.Nanosecond)
		f.h.sleep(before.Sub(time.Now()))
		offer()
		reads, empty, _, _ := f.generator.counts()
		if !time.Now().Equal(before) || reads != f.beforeReads || empty != f.beforeEmpty {
			t.Fatal("SYN_PUBLICATION_PARTIAL_DEMAND_EARLY: just-before-boundary source offer triggered a discovery read")
		}
		if _, eligible := f.update.observeDiscoveryAttempt(f.siblingOwners, time.Now(), f.path); eligible {
			t.Fatal("partial observation became eligible before its exact attempt-age boundary")
		}
		select {
		case <-f.generator.availableEntered:
			t.Fatal("partial listing entered before its actual source attempt was old enough")
		default:
		}
		f.h.sleep(observedAt.Sub(time.Now()))
	} else {
		f.h.sleep(start.Add(reraceSynSchedule[1]).Sub(time.Now()))
	}
	if options != nil && options.beforeRetry != nil {
		options.beforeRetry(f)
	}
	admitted := offer()
	if options != nil && options.afterRetry != nil {
		options.afterRetry(f)
	}
	if !partial && !admitted {
		t.Fatal("publication fixture's reasserting source offer was refused")
	}
	if partial {
		if !time.Now().Equal(observedAt) {
			t.Fatal("partial source offer did not land at its exact attempt-age boundary")
		}
		if _, eligible := f.update.observeDiscoveryAttempt(f.siblingOwners, time.Now(), f.path); !eligible {
			t.Fatal("SYN_PUBLICATION_PARTIAL_DEMAND_BOUNDARY_MISSED: exact-age source lacked observational discovery authority")
		}
		f.update.stateLock.Lock()
		unchanged := f.update.race == partialRace && partialRace != nil && !partialRace.single &&
			partialRace.clientLimit == 4 && len(partialRace.clientStates) == 3 && partialRace.pendingSendCount == 0 &&
			len(f.update.dialFailedClients) == 1 && !f.update.receivedInbound.Load() && f.update.client.Load() == nil
		f.update.stateLock.Unlock()
		if !unchanged {
			t.Fatal("partial observational listing changed the original live three-candidate race or fabricated another refusal")
		}
	}
	select {
	case <-f.generator.availableEntered:
	default:
		t.Fatal("publication fixture did not gate the actual nonempty listing")
	}
	previous := f.window.clientAddCallback
	entered, release, done := make(chan *multiClientChannel, 1), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	f.resume = func() { releaseOnce.Do(func() { close(release) }) }
	f.callbackDone = done
	f.window.clientAddCallback = func(client *multiClientChannel) {
		entered <- client
		select {
		case <-release:
		case <-f.h.multiClient.ctx.Done():
		}
		previous(client)
		close(done)
	}
	t.Cleanup(func() {
		f.resume()
		if f.late == nil {
			synPublicationTestCloseFixture(t, f.h.multiClient, f.resume)
		} else {
			<-done
		}
	})
	f.window.stateLock.Lock()
	acquisitionLive := f.window.discoveryDemand.owner != nil
	acquisitionDeadline := f.window.discoveryDemand.deadline
	f.window.stateLock.Unlock()
	nextSyn := reraceSynSchedule[2]
	if partial {
		nextSyn = reraceSynSchedule[3]
	}
	installationDeadline := discoveryDemandTestEarlierTime(acquisitionDeadline, start.Add(nextSyn))
	if !acquisitionLive || !time.Now().Before(installationDeadline) {
		t.Fatal("publication fixture lost its original acquisition before releasing the available listing")
	}
	close(f.generator.availableRelease)
	var installed bool
	f.late, installed = synPublicationTestAwaitInstalled(f.h.multiClient.ctx, entered, installationDeadline)
	if !installed {
		t.Fatalf("SYN_PUBLICATION_INSTALL_NOT_OBSERVED: original_deadline=%s now=%s parent_done=%t", installationDeadline, time.Now(), f.h.multiClient.ctx.Err() != nil)
	}
	reads, empty, mints, retired := f.generator.counts()
	if f.late != f.h.channel("late") || f.h.update(f.path) != f.update || len(f.h.liveChannels()) != 5 ||
		reads != f.expectedReads || empty != f.expectedEmpty || mints != f.beforeMints+1 || retired != f.beforeRetired ||
		len(f.lateExit.syns(f.path)) != 0 || f.update.receivedInbound.Load() {
		t.Fatal("publication fixture changed source generation, original discovery custody or provider hard bound")
	}
	return f
}

// A settled bubble can still own a healthy first-ping response behind its
// existing acknowledgement timer. Observe actual installation within the
// original acquisition and next-source boundary, without issuing another SYN.
func synPublicationTestAwaitInstalled(ctx context.Context, entered <-chan *multiClientChannel, deadline time.Time) (*multiClientChannel, bool) {
	if ctx.Err() != nil || !time.Now().Before(deadline) {
		return nil, false
	}
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case client := <-entered:
		return client, client != nil && ctx.Err() == nil && time.Now().Before(deadline)
	case <-ctx.Done():
		return nil, false
	case <-timer.C:
		return nil, false
	}
}

// Closing a publisher must not silently test a different replacement of its
// physical provider. Restore availability only after the parent has joined.
func (f *synPublicationTestFixture) withdrawLateAvailability(t *testing.T) {
	t.Helper()
	f.h.generator.mutex.Lock()
	before := append([]*reraceExit(nil), f.h.generator.exits...)
	kept := f.h.generator.exits[:0]
	for _, exit := range f.h.generator.exits {
		if exit != f.lateExit {
			kept = append(kept, exit)
		}
	}
	f.h.generator.exits = kept
	f.h.generator.mutex.Unlock()
	t.Cleanup(func() {
		if err := f.h.multiClient.CloseAndWait(context.Background()); err != nil {
			t.Error(err)
		}
		f.h.generator.mutex.Lock()
		f.h.generator.exits = before
		f.h.generator.mutex.Unlock()
	})
}

// No sibling send is allowed to wake the reaper before the target receipt.
func (f *synPublicationTestFixture) requireAnswer(t *testing.T) {
	t.Helper()
	_, answered := f.h.synAck(f.path)
	if !answered || f.h.boundTo(f.path) != f.late || len(f.lateExit.syns(f.path)) != 1 ||
		f.h.update(f.path) != f.update || !f.update.receivedInbound.Load() {
		t.Fatal("SYN_PUBLICATION_CONTINUATION_MISSING: real admitted provider did not answer the original retained SYN exactly once")
	}
	synRetryControlReleased(t, f.owner)
	if f.owner.publicationIndexed || f.owner.publicationPrevious != nil || f.owner.publicationNext != nil {
		t.Fatal("answered source retained its paid publication index links")
	}
	reads, empty, mints, retired := f.generator.counts()
	if len(f.h.liveChannels()) != 5 || reads != f.expectedReads || empty != f.expectedEmpty ||
		mints != f.beforeMints+1 || retired != f.beforeRetired {
		t.Fatal("publication retry bypassed original hard capacity, listing or mint bounds")
	}
	discoveryDemandFullWindowAssertSiblings(t, f.h, f.siblingPaths, f.siblingOwners)
	reraceAssertNoErrorDelivered(t, f.h, f.path)
}

// The canonical IPv4 old-failure root is retained unchanged in its V3 donor.
func TestNativeSynLatePublicationCompletesSameIpv6Generation(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newSynPublicationTestFixture(t, 6, false, nil)
		beforeNative := f.nativeOffers.Load()
		f.resume()
		synctest.Wait()
		f.requireAnswer(t)
		if f.nativeOffers.Load() != beforeNative {
			t.Fatal("publication recovery required another native source offer")
		}
	})
}

// Repeated callback notifications share the existing intrusive work. A later
// notification after the real answer has no retained source left to revive.
func TestNativeSynLatePublicationCoalescesRepeatedNotifications(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newSynPublicationTestFixture(t, 4, false, nil)
		entered, release := synRetryControlHold(t, f.h)
		credit, beforeNative := f.owner.credit, f.nativeOffers.Load()
		f.resume()
		synctest.Wait()
		synRetryControlEntered(t, entered)
		for range 4 {
			f.h.multiClient.clientAdded(f.late)
		}
		f.update.stateLock.Lock()
		bounded := f.update.synRetry == f.owner && f.owner.references <= 3 && f.owner.credit == credit
		f.update.stateLock.Unlock()
		if !bounded || credit.released.Load() || len(f.lateExit.syns(f.path)) != 0 {
			t.Fatal("publication notifications duplicated owner storage or escaped the held dispatcher")
		}
		release()
		synctest.Wait()
		f.requireAnswer(t)
		for range 4 {
			f.h.multiClient.clientAdded(f.late)
		}
		synctest.Wait()
		if !credit.released.Load() || len(f.lateExit.syns(f.path)) != 1 || f.nativeOffers.Load() != beforeNative {
			t.Fatal("repeated publication revived an answered owner or introduced a new source offer")
		}
	})
}

// Every invalidation happens after real publication queued the retained owner
// and before that worker materializes its packet. Native controls use real APIs.
func testNativeSynLatePublicationFence(t *testing.T, change string) {
	t.Helper()
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newSynPublicationTestFixture(t, 4, false, nil)
		credit := f.owner.credit
		entered, release := synRetryControlHold(t, f.h)
		f.resume()
		synctest.Wait()
		synRetryControlEntered(t, entered)
		switch change {
		case "parent":
			if err := f.h.multiClient.CloseAndWait(context.Background()); err != nil {
				t.Fatal(err)
			}
		case "owner":
			f.update.Close()
		case "FIN", "RST":
			flags := byte(tcpFlagAck | tcpFlagFin)
			if change == "RST" {
				flags = tcpFlagRst
			}
			packet := MessagePoolCopy(reraceTcpPacket(f.path, flags, f.path.SequenceNumber+1, 0, nil))
			if !synRetryControlOffer(t, f.h, packet) {
				t.Fatal("native terminal control did not transfer real source custody")
			}
		case "new-SYN", "answer":
			next := *f.path
			if change == "new-SYN" {
				next.SequenceNumber += 100
			}
			if !privateCrossWindowNativeSyn(f.h, &next) {
				t.Fatal("competing native source did not reach its real newly available provider")
			}
			if _, answered := f.h.synAck(&next); !answered || !f.update.receivedInbound.Load() || f.h.boundTo(&next) != f.late {
				t.Fatal("queued-publication fence lacks the competing real SYN-ACK")
			}
		default:
			t.Fatal("unknown publication fence")
		}
		release()
		synctest.Wait()
		synRetryControlReleased(t, f.owner)
		wanted := 0
		if change == "new-SYN" || change == "answer" {
			wanted = 1
		}
		syns := f.lateExit.syns(f.path)
		if len(syns) != wanted || !credit.released.Load() {
			t.Fatalf("SYN_PUBLICATION_STALE_OWNER_EMITTED: fence=%s late_syns=%d credit_released=%t", change, len(syns), credit.released.Load())
		}
		if change == "new-SYN" && syns[0].path.SequenceNumber != f.path.SequenceNumber+100 {
			t.Fatal("queued old SYN crossed the newly established sequence")
		}
		if f.owner.publicationIndexed || f.owner.publicationPrevious != nil || f.owner.publicationNext != nil {
			t.Fatal("terminal source kept its publication index graph after final credit return")
		}
		if change == "parent" && (f.h.multiClient.synPublicationOwnerCount != 0 ||
			f.h.multiClient.synPublicationHead != nil || f.h.multiClient.synPublicationCursor != nil) {
			t.Fatal("parent join kept publication event or retained-source index custody")
		}
		if change != "parent" {
			discoveryDemandFullWindowAssertSiblings(t, f.h, f.siblingPaths, f.siblingOwners)
		}
		reraceAssertNoErrorDelivered(t, f.h, f.path)
	})
}

func TestNativeSynLatePublicationDropsAfterRealSynAck(t *testing.T) {
	testNativeSynLatePublicationFence(t, "answer")
}

func TestNativeSynLatePublicationDropsAfterChangedSyn(t *testing.T) {
	testNativeSynLatePublicationFence(t, "new-SYN")
}

func TestNativeSynLatePublicationDropsAfterOwnerClose(t *testing.T) {
	testNativeSynLatePublicationFence(t, "owner")
}

func TestNativeSynLatePublicationDropsAfterFin(t *testing.T) {
	testNativeSynLatePublicationFence(t, "FIN")
}

func TestNativeSynLatePublicationDropsAfterRst(t *testing.T) {
	testNativeSynLatePublicationFence(t, "RST")
}

func TestNativeSynLatePublicationParentJoinReleasesAllCredit(t *testing.T) {
	testNativeSynLatePublicationFence(t, "parent")
}

// Admission reuses the exact existing SDK32-like shared retained parent.
// Final fixture joins precede the budget assertion registered here.
func TestNativeSynLatePublicationKeepsSharedRetainedBudget(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		root := NewTransferMemoryBudget(32 * 1024 * 1024 * 65 / 100)
		shared := NewTransferMemoryBudgetWithParent(root.TotalByteCount(), root)
		t.Cleanup(func() {
			reserved, released := shared.Counts()
			if shared.UsedByteCount() != 0 || root.UsedByteCount() != 0 || reserved != released {
				t.Fatalf("publication retained budget unbalanced: used=%d root=%d reserved=%d released=%d", shared.UsedByteCount(), root.UsedByteCount(), reserved, released)
			}
		})
		// This shared-credit arm intentionally carries one real TFO DATA byte.
		options := &synPublicationPacketOptions{packet: func(path *IpPath, _ int) []byte {
			return reraceTcpPacket(path, tcpFlagSyn, path.SequenceNumber, 0, []byte{0x64})
		}}
		f := newSynPublicationPacketFixture(t, 4, false, func(settings *ClientSettings) {
			settings.SendBufferSettings.ResendQueueBudget = shared
			settings.SendBufferSettings.ResendQueueRetainedByteAccounting = true
		}, options)
		credit, packet := f.owner.credit, bytes.Clone(f.owner.packet)
		if protocolClassPureTcpControl(packet) || credit.noBudget || credit.budget != shared || credit.local != nil || root.UsedByteCount() > root.TotalByteCount() {
			t.Fatal("retained publication cache did not use the composed source budget")
		}
		f.resume()
		synctest.Wait()
		f.requireAnswer(t)
		if !credit.released.Load() || !bytes.Equal(packet, reraceTcpPacket(f.path, tcpFlagSyn, f.path.SequenceNumber, 0, []byte{0x64})) {
			t.Fatal("publication changed original source bytes or kept its charged cache")
		}
	})
}

// Incompatible, ended and warned publishers supply no retry authority. The
// fixed reaper is otherwise running, so a queued attempt would be observable.
func testNativeSynLatePublicationDeclinesClient(t *testing.T, reason string) {
	t.Helper()
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		if reason == "warned" {
			f := synPublicationTestRejectWhileWarningLive(t)
			f.resume()
			<-f.callbackDone
			return
		}
		var addLateExit []func(*synPublicationTestFixture) *reraceExit
		if reason == "family" {
			// the server labels the late exit ipv4 in the very listing that
			// installs it for the v6 flow's demand read: the label, never a
			// local downgrade, is what makes it incompatible (server
			// FP2-PROTOCOL.md D11.8)
			addLateExit = append(addLateExit, func(f *synPublicationTestFixture) *reraceExit {
				exit := f.h.addExit(reraceExitSpec{name: "late"})
				capabilities := ProviderCapabilities{
					Ip:       ProviderIpCapabilityIpv4,
					Protocol: ProviderProtocolCapabilityDualproto,
				}
				f.generator.mutex.Lock()
				f.generator.resultOverrideForTest = map[MultiHopId]DestinationStats{
					exit.destination: {IpFamily: capabilities.IpFamily(), Capabilities: capabilities},
				}
				f.generator.mutex.Unlock()
				return exit
			})
		}
		f := newSynPublicationTestFixture(t, 6, false, nil, addLateExit...)
		switch reason {
		case "family":
			if f.late.supportsIpPath(f.path) {
				t.Fatal("late exit labelled ipv4 can carry the v6 flow")
			}
			// installed: no later listing offers it again
			f.generator.mutex.Lock()
			f.generator.resultOverrideForTest = nil
			f.generator.mutex.Unlock()
		case "closed":
			f.withdrawLateAvailability(t)
			f.late.Close()
		default:
			t.Fatal("unknown declined publication")
		}
		before := f.retainedOffers.Load()
		f.resume()
		synctest.Wait()
		if len(f.lateExit.syns(f.path)) != 0 || f.update.receivedInbound.Load() || f.h.boundTo(f.path) != nil || f.retainedOffers.Load() != before {
			t.Fatalf("SYN_PUBLICATION_INCOMPATIBLE_CLIENT_RETRIED: reason=%s", reason)
		}
		if reason != "closed" && synRetryControlOwner(t, f.update) != f.owner {
			t.Fatal("declined publication lost the original bounded source fallback")
		}
	})
}

func TestNativeSynLatePublicationDeclinesWrongIpFamily(t *testing.T) {
	testNativeSynLatePublicationDeclinesClient(t, "family")
}

func TestNativeSynLatePublicationDeclinesWarnedClient(t *testing.T) {
	testNativeSynLatePublicationDeclinesClient(t, "warned")
}

func TestNativeSynLatePublicationDeclinesClosedClient(t *testing.T) {
	testNativeSynLatePublicationDeclinesClient(t, "closed")
}

// The new client fills exactly the vacancy in the original unanswered race;
// a degraded bound already spent by live contenders creates no new dispatch.
func testNativeSynLatePublicationRaceVacancy(t *testing.T, degraded bool) {
	t.Helper()
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newSynPublicationTestFixture(t, 4, true, nil)
		f.update.stateLock.Lock()
		race := f.update.race
		valid := race != nil && !race.single && race.clientLimit == 4 && len(race.clientStates) == 3 && race.pendingSendCount == 0
		f.update.stateLock.Unlock()
		if !valid {
			t.Fatal("publication partial-race control lacks exactly one actual vacated slot")
		}
		if degraded {
			f.h.multiClient.backendDegradedForTest = func() bool { return true }
		}
		before := f.retainedOffers.Load()
		f.resume()
		synctest.Wait()
		if degraded {
			if f.retainedOffers.Load() != before || len(f.lateExit.syns(f.path)) != 0 || f.update.receivedInbound.Load() {
				t.Fatal("SYN_PUBLICATION_EXCEEDED_DEGRADED_RACE_BOUND: publication bypassed live contender custody")
			}
		} else {
			f.requireAnswer(t)
			if f.retainedOffers.Load() != before+1 {
				t.Fatal("one real race vacancy did not produce exactly one publication continuation")
			}
		}
	})
}

func TestNativeSynLatePublicationFillsOnlyActualRaceVacancy(t *testing.T) {
	testNativeSynLatePublicationRaceVacancy(t, false)
}

func TestNativeSynLatePublicationKeepsDegradedRaceBound(t *testing.T) {
	testNativeSynLatePublicationRaceVacancy(t, true)
}

// A healthy ordinary flow deliberately holds its leaf lock throughout the
// actual window callback. Publication must return without inspecting that leaf.
func TestNativeSynLatePublicationDoesNotWaitForUnrelatedFlowLock(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newSynPublicationTestFixture(t, 4, false, nil)
		ordinary := f.h.update(f.siblingPaths[0])
		ordinary.stateLock.Lock()
		var ordinaryUnlockOnce sync.Once
		unlockOrdinary := func() { ordinaryUnlockOnce.Do(ordinary.stateLock.Unlock) }
		t.Cleanup(unlockOrdinary)
		if ordinary.synRetry != nil || !ordinary.receivedInbound.Load() {
			unlockOrdinary()
			t.Fatal("lock control did not isolate an established ordinary flow")
		}
		attempted := make(chan struct{})
		var attemptedOnce sync.Once
		f.h.multiClient.beforeSynPublicationOwnerLockForTest = func(current *multiClientChannelUpdate) {
			if current == ordinary {
				attemptedOnce.Do(func() { close(attempted) })
			}
		}
		f.resume()
		returned := false
		select {
		case <-f.callbackDone:
			returned = true
		case <-attempted:
		}
		unlockOrdinary()
		synctest.Wait()
		f.requireAnswer(t)
		if !returned {
			t.Fatal("SYN_PUBLICATION_CALLBACK_WAITED_FOR_UNRELATED_FLOW: admitted provider publication inspected an ordinary flow lock")
		}
	})
}

// Unit coverage isolates event-list fairness from actual source/provider
// behavior covered above. Empty events consume the same fixed turn allowance.
func TestSynPublicationReaperBoundsAndFairlyDrainsEvents(t *testing.T) {
	ctx := context.Background()
	parent := &RemoteUserNatMultiClient{ctx: ctx, settings: DefaultMultiClientSettings(), flowReaperDone: make(chan struct{})}
	batch := parent.settings.SynRetryPublicationBatchSize
	clients := make([]*multiClientChannel, 2*batch+3)
	for index := range clients {
		clients[index] = &multiClientChannel{ctx: ctx}
		parent.enqueuePublishedSynRetries(clients[index])
		parent.enqueuePublishedSynRetries(clients[index])
	}
	for turn := 1; turn <= 2; turn++ {
		parent.drainSynPublications()
		if parent.synPublicationHead != clients[turn*batch] ||
			parent.synPublicationTail != clients[len(clients)-1] {
			t.Fatal("SYN_PUBLICATION_UNBOUNDED_EVENT_TURN: one reaper call exceeded its fixed FIFO event allowance")
		}
	}
	parent.drainSynPublications()
	if parent.synPublicationHead != nil || parent.synPublicationTail != nil || parent.synPublicationActive {
		t.Fatal("finite publication events did not drain after their exact bounded turns")
	}
	for _, client := range clients {
		if client.synPublicationNext != nil || client.synPublicationQueued {
			t.Fatal("completed publication retained an intrusive client queue link")
		}
	}
}

// Synthetic weak-index entries exercise only cursor mutation and pass bounds;
// they carry no packet/credit and are never presented as funded source owners.
func TestSynPublicationCursorSurvivesIndexedRetirement(t *testing.T) {
	ctx := context.Background()
	parent := &RemoteUserNatMultiClient{ctx: ctx, settings: DefaultMultiClientSettings(), flowReaperDone: make(chan struct{})}
	batch := parent.settings.SynRetryPublicationBatchSize
	owners := make([]*tcpSynRetry, 2*batch+3)
	for index := range owners {
		owner := &tcpSynRetry{publicationIndexed: true, publicationPrevious: parent.synPublicationOwnerTail}
		if parent.synPublicationOwnerTail == nil {
			parent.synPublicationOwnerHead = owner
		} else {
			parent.synPublicationOwnerTail.publicationNext = owner
		}
		parent.synPublicationOwnerTail = owner
		parent.synPublicationOwnerCount++
		owners[index] = owner
	}
	client := &multiClientChannel{ctx: ctx}
	parent.enqueuePublishedSynRetries(client)
	parent.drainSynPublications()
	if parent.synPublicationCursor != owners[batch] ||
		parent.synPublicationRemaining != len(owners)-batch || !client.synPublicationQueued {
		t.Fatal("paid-owner cursor did not stop at the fixed first-turn boundary")
	}
	parent.removeSynPublicationOwner(owners[batch])
	parent.removeSynPublicationOwner(owners[0])
	if parent.synPublicationCursor != owners[batch+1] || parent.synPublicationOwnerCount != len(owners)-2 {
		t.Fatal("retirement lost the next live owner or retained the retired index entry")
	}
	parent.drainSynPublications()
	if parent.synPublicationCursor != owners[2*batch+1] {
		t.Fatal("SYN_PUBLICATION_CURSOR_SKIPPED_LIVE_OWNER: retirement changed the bounded successor walk")
	}
	parent.drainSynPublications()
	if parent.synPublicationHead != nil || client.synPublicationQueued || parent.synPublicationCursor != nil {
		t.Fatal("cursor retirement stranded a finite publication event")
	}
	for _, owner := range owners {
		parent.removeSynPublicationOwner(owner)
	}
	if parent.synPublicationOwnerHead != nil || parent.synPublicationOwnerTail != nil || parent.synPublicationOwnerCount != 0 {
		t.Fatal("retired weak index retained an owner graph")
	}
}

// Unit coverage composes the real source/refusal/retirement ledger with the
// publication enqueue predicate. The alias is identity data, not a wire fake.
func TestNativeSynLatePublicationRejectsRetiredRemoteAlias(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		h, _, update := newSynRetryRemoteRefusalFixture(t, nil)
		owner := synRetryControlOwner(t, update)
		old := h.channel("refused-first")
		alias := &multiClientChannel{
			ctx: context.Background(),
			args: &multiClientChannelArgs{
				MultiClientGeneratorClientArgs: MultiClientGeneratorClientArgs{ClientId: NewId()},
				Destination:                    old.Destination(),
			},
		}
		old.Close()
		h.settle()
		var dispatched atomic.Int32
		h.multiClient.beforeSynRetryDispatchForTest = func(current *multiClientChannelUpdate) {
			if current == update {
				dispatched.Add(1)
			}
		}
		h.multiClient.enqueuePublishedSynOwner(alias, update, owner)
		h.settle()
		if dispatched.Load() != 0 || synRetryControlOwner(t, update) != owner {
			t.Fatal("SYN_PUBLICATION_REMOTE_ALIAS_BYPASSED_REFUSAL: a new local identity revived the same SYN's rejected physical tail")
		}
	})
}

// Separate owners consume their own configured number of actual FIFO events.
// The defaults constructor alone supplies the production default.
func TestSynPublicationBatchSettingIsPerOwner(t *testing.T) {
	ctx := context.Background()
	firstSettings, secondSettings, defaults := DefaultMultiClientSettings(), DefaultMultiClientSettings(), DefaultMultiClientSettings()
	if firstSettings.SynRetryPublicationBatchSize != 32 || secondSettings.SynRetryPublicationBatchSize != 32 || defaults.SynRetryPublicationBatchSize != 32 {
		t.Fatal("publication work default changed without its explicit per-owner contract")
	}
	firstSettings.SynRetryPublicationBatchSize = 3
	secondSettings.SynRetryPublicationBatchSize = 7
	first := &RemoteUserNatMultiClient{ctx: ctx, settings: firstSettings, flowReaperDone: make(chan struct{})}
	second := &RemoteUserNatMultiClient{ctx: ctx, settings: secondSettings, flowReaperDone: make(chan struct{})}
	firstClients, secondClients := make([]*multiClientChannel, 10), make([]*multiClientChannel, 10)
	for index := range firstClients {
		firstClients[index], secondClients[index] = &multiClientChannel{ctx: ctx}, &multiClientChannel{ctx: ctx}
		first.enqueuePublishedSynRetries(firstClients[index])
		second.enqueuePublishedSynRetries(secondClients[index])
	}
	first.drainSynPublications()
	if first.synPublicationHead != firstClients[3] || second.synPublicationHead != secondClients[0] {
		t.Fatal("SYN_PUBLICATION_BATCH_IGNORED_OWNER_SETTING: first owner exceeded its configured three-event turn")
	}
	second.drainSynPublications()
	if second.synPublicationHead != secondClients[7] || first.synPublicationHead != firstClients[3] || defaults.SynRetryPublicationBatchSize != 32 {
		t.Fatal("SYN_PUBLICATION_BATCH_SHARED_BETWEEN_OWNERS: one owner's quantum changed another")
	}
	first.discardSynPublications()
	second.discardSynPublications()
}

// Nonpositive admission queues no work, and a pending legacy event relinquishes
// its links on the real drain without waking itself into another empty turn.
func TestSynPublicationNonpositiveBatchDeclinesAndDrainsWithoutWake(t *testing.T) {
	for _, batch := range []int{0, -3} {
		ctx := context.Background()
		settings := DefaultMultiClientSettings()
		settings.SynRetryPublicationBatchSize = batch
		parent := &RemoteUserNatMultiClient{ctx: ctx, settings: settings, flowReaperDone: make(chan struct{}), flowReaperWake: make(chan struct{}, 1)}
		client := &multiClientChannel{ctx: ctx}
		parent.enqueuePublishedSynRetries(client)
		// This synthetic descriptor carries no packet/credit; only disabled
		// index admission is under test, not source funding or materialization.
		owner := &tcpSynRetry{}
		update := &multiClientChannelUpdate{sequenceParent: parent, synRetry: owner}
		update.indexSynRetryPublication()
		indexed := owner.publicationIndexed || parent.synPublicationOwnerHead != nil || parent.synPublicationOwnerCount != 0
		if owner.publicationIndexed {
			parent.removeSynPublicationOwner(owner)
		}
		if indexed {
			t.Fatalf("SYN_PUBLICATION_DISABLED_INDEX_ADMITTED: batch=%d indexed a disabled descriptor", batch)
		}
		if parent.synPublicationHead != nil || client.synPublicationQueued || len(parent.flowReaperWake) != 0 {
			t.Fatalf("batch=%d admitted disabled publication work", batch)
		}
		parent.synPublicationHead, parent.synPublicationTail = client, client
		parent.synPublicationCursor, parent.synPublicationRemaining, parent.synPublicationActive = &tcpSynRetry{}, 1, true
		client.synPublicationQueued = true
		parent.drainSynPublications()
		if parent.synPublicationHead != nil || parent.synPublicationTail != nil || parent.synPublicationCursor != nil || parent.synPublicationActive ||
			parent.synPublicationRemaining != 0 || client.synPublicationQueued || client.synPublicationNext != nil || len(parent.flowReaperWake) != 0 {
			t.Fatalf("batch=%d retained disabled event custody or issued a busy-loop wake", batch)
		}
	}
}

// An active head, queued middle and tail retire without retaining channel
// graphs; the next live event starts its own complete cursor allowance.
func TestSynPublicationRetirementUnlinksActiveAndQueuedClients(t *testing.T) {
	ctx := context.Background()
	settings := DefaultMultiClientSettings()
	settings.SynRetryPublicationBatchSize = 1
	parent := &RemoteUserNatMultiClient{ctx: ctx, settings: settings, flowReaperDone: make(chan struct{})}
	clients := make([]*multiClientChannel, 4)
	for index := range clients {
		clients[index] = &multiClientChannel{ctx: ctx}
		parent.enqueuePublishedSynRetries(clients[index])
	}
	first, last := &tcpSynRetry{publicationIndexed: true}, &tcpSynRetry{publicationIndexed: true}
	first.publicationNext, last.publicationPrevious = last, first
	parent.synPublicationOwnerHead, parent.synPublicationOwnerTail, parent.synPublicationOwnerCount = first, last, 2
	parent.drainSynPublications()
	if parent.synPublicationHead != clients[0] || parent.synPublicationCursor != last || !parent.synPublicationActive {
		t.Fatal("retirement control did not hold the original active cursor")
	}
	parent.retireSynPublicationClientMetadata(clients[0])
	parent.retireSynPublicationClientMetadata(clients[2])
	parent.retireSynPublicationClientMetadata(clients[3])
	if parent.synPublicationHead != clients[1] || parent.synPublicationTail != clients[1] ||
		parent.synPublicationCursor != nil || parent.synPublicationActive || parent.synPublicationRemaining != 0 {
		t.Fatal("SYN_PUBLICATION_RETIRED_CLIENT_GRAPH_RETAINED: active, middle or tail retirement kept queue custody")
	}
	for _, index := range []int{0, 2, 3} {
		if clients[index].synPublicationNext != nil || clients[index].synPublicationQueued {
			t.Fatal("retired publication client retained intrusive links")
		}
	}
	parent.drainSynPublications()
	if parent.synPublicationCursor != last || parent.synPublicationRemaining != 1 || !parent.synPublicationActive {
		t.Fatal("next live event inherited the retired client's partial cursor")
	}
	parent.discardSynPublications()
	parent.removeSynPublicationOwner(first)
	parent.removeSynPublicationOwner(last)
}

// The real installed publisher closes at a durably gated queue boundary.
// Removal and eventual drain stay joined without assuming a blocking leaf wait.
func TestNativeSynLatePublicationQueuedClientCloseReleasesQueueGraph(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newSynPublicationTestFixture(t, 4, false, nil)
		f.withdrawLateAvailability(t)
		attempted, queueRelease := make(chan struct{}), make(chan struct{})
		var attemptedOnce, releaseOnce sync.Once
		resumeQueue := func() { releaseOnce.Do(func() { close(queueRelease) }) }
		t.Cleanup(resumeQueue)
		f.h.multiClient.beforeSynPublicationQueueForTest = func(current *multiClientChannelUpdate) {
			if current == f.update {
				attemptedOnce.Do(func() {
					close(attempted)
					select {
					case <-queueRelease:
					case <-f.h.multiClient.ctx.Done():
					}
				})
			}
		}
		f.update.stateLock.Lock()
		var unlockOnce sync.Once
		unlock := func() { unlockOnce.Do(f.update.stateLock.Unlock) }
		t.Cleanup(unlock)
		f.resume()
		<-f.callbackDone
		<-attempted
		f.h.multiClient.stateLock.Lock()
		queued := f.h.multiClient.synPublicationHead == f.late && f.late.synPublicationQueued
		f.h.multiClient.stateLock.Unlock()
		if !queued {
			unlock()
			t.Fatal("queued-close control did not capture its actual publisher event")
		}
		f.late.Close()
		resumeQueue()
		unlock()
		synctest.Wait()
		f.h.multiClient.stateLock.Lock()
		retained := f.late.synPublicationQueued || f.late.synPublicationNext != nil || f.h.multiClient.synPublicationHead == f.late
		f.h.multiClient.stateLock.Unlock()
		if retained || len(f.lateExit.syns(f.path)) != 0 || f.update.receivedInbound.Load() {
			t.Fatal("SYN_PUBLICATION_CLOSED_CLIENT_RETAINED: retired publisher kept a queue graph or emitted retained SYN bytes")
		}
	})
}

// A busy paid owner spends one visit without parking its healthy sibling.
// The same publisher event returns on the existing bounded retry timer.
func TestNativeSynLatePublicationPreservesEventAfterLeafContention(t *testing.T) {
	testSynPublicationDeferredPaidPair(t, false)
}

// The channel's real first ping anchors acknowledgement compression before
// evaluation sends its second ping. Waiting only for durable blocking must not
// turn that healthy timer into a failed installation premise.
func TestNativeSynLatePublicationWaitsForActualHealthyAck(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		state := &discoveryDemandTestAckTimerState{firstAck: make(chan struct{})}
		ingress := make(chan struct{})
		var ingressOnce sync.Once
		var ingressDeadline time.Time
		f := newSynPublicationTestFixture(t, 4, false, nil, func(f *synPublicationTestFixture) *reraceExit {
			f.window.stateLock.Lock()
			deadline := f.window.discoveryDemand.deadline
			f.window.stateLock.Unlock()
			ingressDeadline = deadline
			f.generator.mutex.Lock()
			f.generator.configureNewClient = func(args *MultiClientGeneratorClientArgs, settings *ClientSettings) {
				state.mutex.Lock()
				state.client = args.ClientId
				state.mutex.Unlock()
				settings.SendBufferSettings.TransferWireMessageObserver = state.observeWire
				settings.ReceiveBufferSettings.ProgressObserver = func(event TransferProgressEvent) {
					state.observeClientProgress(event)
					state.mutex.Lock()
					observed := state.secondIngress
					state.mutex.Unlock()
					if observed {
						ingressOnce.Do(func() { close(ingress) })
					}
				}
			}
			f.generator.mutex.Unlock()
			f.window.settings.startChannelPingForTest = func(start func()) {
				start()
				timer := time.NewTimer(time.Until(deadline))
				defer timer.Stop()
				select {
				case <-state.firstAck:
				case <-f.h.ctx.Done():
				case <-timer.C:
				}
			}
			return discoveryDemandTestAddAckTimerExit(f.h, state)
		})
		// The installed callback still holds expand's mutex. Await the actual
		// ingress witness without asking every bubble goroutine to settle.
		if !discoveryDemandTestAwaitPublication(t, ingress, ingressDeadline) {
			t.Fatal("SYN_PUBLICATION_HEALTHY_ACK_INGRESS_NOT_OBSERVED: original acquisition deadline elapsed")
		}
		state.mutex.Lock()
		compressed := state.firstSeen && state.secondSeen && state.firstWritten && state.anchored &&
			state.timerSeen && state.secondIngress && state.timerAt.Equal(state.anchorAt)
		anchorAt, timerAt, ingressAt := state.anchorAt, state.timerAt, state.ingressAt
		state.mutex.Unlock()
		compression := f.lateExit.provider.settings.ReceiveBufferSettings.AckCompressTimeout
		if !compressed || !ingressAt.After(timerAt) || ingressAt.After(anchorAt.Add(compression)) ||
			f.update.receivedInbound.Load() || len(f.lateExit.syns(f.path)) != 0 {
			t.Fatal("SYN_PUBLICATION_HEALTHY_ACK_BOUNDARY_MISSING: fixture did not cross its real acknowledgement-compression timer before installation")
		}
		beforeNative := f.nativeOffers.Load()
		f.resume()
		synctest.Wait()
		f.requireAnswer(t)
		if f.nativeOffers.Load() != beforeNative {
			t.Fatal("actual healthy admission needed an additional native source offer")
		}
	})
}

// Acquisition time already spent is not renewed by the fixture's observer.
func TestSynPublicationAdmissionWaitKeepsOriginalDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		budget := DefaultMultiClientSettings().WindowExpandTimeout
		deadline := time.Now().Add(budget)
		time.Sleep(budget / 2)
		client, installed := synPublicationTestAwaitInstalled(context.Background(), make(chan *multiClientChannel), deadline)
		if installed || client != nil || !time.Now().Equal(deadline) {
			t.Fatal("SYN_PUBLICATION_INSTALL_RENEWED_DEADLINE: observer did not stop at the original absolute acquisition deadline")
		}
	})
}

// A canceled parent cannot leave its fixture observer waiting for admission.
func TestSynPublicationAdmissionWaitStopsAtParentCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		start := time.Now()
		client, installed := synPublicationTestAwaitInstalled(ctx, make(chan *multiClientChannel), start.Add(DefaultMultiClientSettings().WindowExpandTimeout))
		if installed || client != nil || !time.Now().Equal(start) {
			t.Fatal("SYN_PUBLICATION_INSTALL_IGNORED_PARENT_CLOSE: canceled observer retained its admission wait")
		}
	})
}

// Warning is recomputed by resize. Keep its real admission callback held while
// the same installed channel receives a publication notification and completes
// one actual reaper pass; no bubble-wide Wait or fake clock is needed here.
func synPublicationTestRejectWhileWarningLive(t *testing.T) *synPublicationTestFixture {
	t.Helper()
	publicationReturned, inspected, releaseTurn := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var inspectedOnce, releaseOnce sync.Once
	var armed atomic.Bool
	finish := func() { releaseOnce.Do(func() { close(releaseTurn) }) }
	f := newSynPublicationTestFixture(t, 6, false, nil, func(f *synPublicationTestFixture) *reraceExit {
		// No late provider or held callback exists yet. This settles prior
		// reaper reads before installing the immutable observer for this owner.
		f.h.settle()
		f.h.multiClient.afterSynPublicationTurnForTest = func() {
			if !armed.Load() {
				return
			}
			select {
			case <-publicationReturned:
			case <-f.h.multiClient.ctx.Done():
				return
			}
			f.h.multiClient.stateLock.Lock()
			pending := f.late.synPublicationQueued
			f.h.multiClient.stateLock.Unlock()
			if !pending {
				inspectedOnce.Do(func() {
					close(inspected)
					select {
					case <-releaseTurn:
					case <-f.h.multiClient.ctx.Done():
					}
				})
			}
		}
		return f.h.addExit(reraceExitSpec{name: "late"})
	})
	t.Cleanup(finish)
	f.late.setWarning(true, warnUnhealthy)
	armed.Store(true)
	before := f.retainedOffers.Load()
	// This is the existing callback on an actually installed, healthy-pinged
	// channel. The held window callback later supplies its ordinary duplicate.
	f.h.multiClient.clientAdded(f.late)
	close(publicationReturned)
	<-inspected
	f.late.stateLock.Lock()
	warning, cause := f.late.warning, f.late.warnCause
	f.late.stateLock.Unlock()
	f.update.stateLock.Lock()
	owner := f.update.synRetry
	queued := owner != nil && owner.queued
	f.update.stateLock.Unlock()
	if !warning || cause != warnUnhealthy || !f.late.isWarning() {
		finish()
		t.Fatal("SYN_PUBLICATION_WARNING_PREMISE_CHANGED: actual eligibility turn did not retain the live warning")
	}
	if len(f.lateExit.syns(f.path)) != 0 || f.update.receivedInbound.Load() || f.h.boundTo(f.path) != nil ||
		f.retainedOffers.Load() != before || queued {
		finish()
		t.Fatal("SYN_PUBLICATION_INCOMPATIBLE_CLIENT_RETRIED: reason=warned")
	}
	if owner != f.owner || owner == nil {
		finish()
		t.Fatal("declined warned publication lost the original bounded source fallback")
	}
	finish()
	return f
}

// A warning is a current eligibility fact, not an irreversible provider
// conviction. The same original source resumes after that fact is cleared.
func TestNativeSynLatePublicationRecoveredWarningKeepsOriginalOwner(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := synPublicationTestRejectWhileWarningLive(t)
		f.late.setWarning(false, warnNone)
		if f.late.isWarning() || f.h.update(f.path) != f.update || synRetryControlOwner(t, f.update) != f.owner {
			t.Fatal("explicit warning recovery changed source ownership or remained excluded")
		}
		beforeNative := f.nativeOffers.Load()
		f.resume()
		<-f.callbackDone
		synctest.Wait()
		f.requireAnswer(t)
		if f.nativeOffers.Load() != beforeNative {
			t.Fatal("healthy warning recovery needed another native SYN offer")
		}
	})
}
