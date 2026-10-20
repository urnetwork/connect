// Virtual-time donor policy roots use real routing; the public queue root
// additionally proves the provider race is reached without a routing override.
package connect

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"testing"
	"testing/synctest"
	"time"
)

// No background reaper is started: each test owns exact virtual deadlines.
func affinityTimeTestParent(t *testing.T) *RemoteUserNatMultiClient {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	settings := DefaultMultiClientSettings()
	settings.Log = NewNoopLogger()
	parent := flowReaperTestParent(ctx, settings)
	parent.cancel = cancel
	parent.reliabilityMetrics = newReliabilityMetrics()
	parent.config.Store(&multiClientConfig{})
	t.Cleanup(func() {
		cancel()
		for update := range parent.flowUpdates {
			update.Close()
		}
	})
	return parent
}

// All fixture endpoints are documentation addresses; only the source port
// changes when the test creates a new connection to the same destination.
func affinityTimeTestPath(version, sourcePort int) *IpPath {
	source, destination := net.ParseIP("192.0.2.61"), net.ParseIP("198.51.100.62")
	if version == 6 {
		source, destination = net.ParseIP("2001:db8:1::61"), net.ParseIP("2001:db8:2::62")
	}
	return &IpPath{Version: version, Protocol: IpProtocolTcp, SourceIp: source,
		DestinationIp: destination, SourcePort: sourcePort, DestinationPort: 443,
		Syn: true, SequenceNumber: uint32(sourcePort)}
}

// Installs an explicitly healthy actual selection, not a timestamp injected
// into an affinity index. The public queue root below covers real admission.
func affinityTimeTestDonor(t *testing.T, parent *RemoteUserNatMultiClient, version int) (*multiClientChannelUpdate, *multiClientChannel) {
	t.Helper()
	update, _, client := parent.sendUpdate(affinityTimeTestPath(version, 44000), flowPin{})
	if update == nil || client != nil {
		t.Fatal("first fixture flow was not unbound")
	}
	donor := &multiClientChannel{
		ctx: parent.ctx, settings: parent.settings, healthy: true,
		args: &multiClientChannelArgs{DestinationStats: DestinationStats{IpFamily: IpFamilyDualstack}},
	}
	if !donor.supportsIpVersion(version) || donor.args.FixedDestination {
		t.Fatal("ordinary donor fixture lacks the requested address family")
	}
	update.selectAffinityClient(donor)
	parent.bindClientFlow(update, donor)
	return update, donor
}

// Both protocol families share exactly the same strict monotonic boundary.
func testAffinityMaxTimeBoundary(t *testing.T, version int, named bool) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		for _, age := range []time.Duration{0, 59 * time.Second, time.Minute - time.Nanosecond, time.Minute, 61 * time.Second} {
			parent := affinityTimeTestParent(t)
			if named {
				parent.config.Store(&multiClientConfig{serverNameLookup: stubServerNameLookup{names: []string{"shop.affinity.example"}}})
			}
			old, donor := affinityTimeTestDonor(t, parent, version)
			time.Sleep(age)
			fresh, _, client := parent.sendUpdate(affinityTimeTestPath(version, 44001), flowPin{})
			if (client == donor) != (age < time.Minute) {
				t.Fatalf("v%d named=%v age=%v inherited=%v", version, named, age, client == donor)
			}
			if old.client.Load() != donor || old.IsDone() || fresh == old {
				t.Fatal("new-flow age policy mutated established tuple ownership")
			}
			if client != nil && fresh.client.loadSelection() != old.client.loadSelection() {
				t.Fatal("inherited flow minted a younger selection receipt")
			}
		}
	})
}

func TestAffinityMaxTimeIp4ExactBoundary(t *testing.T) { testAffinityMaxTimeBoundary(t, 4, false) }
func TestAffinityMaxTimeIp6ExactBoundary(t *testing.T) { testAffinityMaxTimeBoundary(t, 6, false) }
func TestAffinityMaxTimeDomainExactBoundary(t *testing.T) {
	testAffinityMaxTimeBoundary(t, 4, true)
	testAffinityMaxTimeBoundary(t, 6, true)
}

func TestAffinityMaxTimeNonpositiveDisablesOrdinaryReuse(t *testing.T) {
	for _, cap := range []time.Duration{0, -time.Nanosecond, -time.Minute} {
		for _, version := range []int{4, 6} {
			parent := affinityTimeTestParent(t)
			old, donor := affinityTimeTestDonor(t, parent, version)
			settings := ReliabilitySettingsFrom(parent.settings)
			settings.AffinityMaxTime = cap
			parent.SetReliabilitySettings(settings)
			_, _, client := parent.sendUpdate(affinityTimeTestPath(version, 44001), flowPin{})
			if client != nil || old.client.Load() != donor || old.IsDone() {
				t.Fatalf("v%d cap=%v did not disable only cross-flow reuse", version, cap)
			}
		}
	}
}

func TestAffinityMaxTimeFutureOrMissingOriginRefuses(t *testing.T) {
	now := time.Now()
	client, other := &multiClientChannel{}, &multiClientChannel{}
	for _, receipt := range []*multiClientAffinitySelection{
		nil, {client: client}, {client: other, createTime: now},
		{client: client, createTime: now.Add(time.Nanosecond)},
	} {
		if receipt.eligible(client, now, time.Minute) {
			t.Fatal("invalid origin authorized donor reuse")
		}
	}
}

// Neither bookkeeping nor a same-object provider re-selection can make an
// old update young. A genuinely new update can start a new creation age.
func TestAffinityBindingSameClientStorePreservesOrigin(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		parent := affinityTimeTestParent(t)
		update, client := affinityTimeTestDonor(t, parent, 4)
		origin := update.client.loadSelection()
		olderUnpublished := newMultiClientChannelUpdate(parent.ctx, affinityTimeTestPath(4, 44003))
		defer olderUnpublished.Close()
		time.Sleep(59 * time.Second)
		update.client.Store(client)
		if !update.client.CompareAndSwap(client, client) || update.client.loadSelection() != origin {
			t.Fatal("same-provider bookkeeping minted a fresh origin")
		}
		child := newMultiClientChannelUpdate(parent.ctx, affinityTimeTestPath(4, 44001))
		defer child.Close()
		child.inheritAffinityClient(client, origin)
		if child.client.loadSelection() != origin || child.client.Load() != client {
			t.Fatal("inheritance did not publish the exact original binding")
		}
		update.selectAffinityClient(client)
		fresh := update.client.loadSelection()
		if fresh == origin || fresh.client != client || !fresh.createTime.Equal(origin.createTime) {
			t.Fatal("same-update selection lost epoch identity or renewed its creation age")
		}
		if child.client.loadSelection() != origin {
			t.Fatal("a later race mutated the inherited selection receipt")
		}
		child.client.Store(nil)
		child.client.Store(client)
		if !child.client.loadSelection().createTime.Equal(origin.createTime) {
			t.Fatal("nil/unbind bookkeeping laundered an inherited lineage age")
		}
		child.client.Store(nil)
		if !child.client.CompareAndSwap(nil, client) || !child.client.loadSelection().createTime.Equal(origin.createTime) {
			t.Fatal("nil-to-provider CAS laundered an inherited lineage age")
		}
		child.selectAffinityClient(client)
		if !child.client.loadSelection().createTime.Equal(origin.createTime) {
			t.Fatal("re-racing an inherited old update renewed its origin")
		}
		newUpdate := newMultiClientChannelUpdate(parent.ctx, affinityTimeTestPath(4, 44002))
		defer newUpdate.Close()
		newUpdate.selectAffinityClient(client)
		if !newUpdate.client.loadSelection().createTime.After(origin.createTime) {
			t.Fatal("genuinely new update failed to start its own creation age")
		}
		olderBirth := olderUnpublished.client.originTime
		olderUnpublished.inheritAffinityClient(client, newUpdate.client.loadSelection())
		if !olderUnpublished.client.loadSelection().createTime.Equal(olderBirth) {
			t.Fatal("inheritance from a newer donor laundered an earlier update birth")
		}
	})
}

// Setup time is part of the creation-age window: born at t0, first selected
// at t30, eligible at t59, expired at t60 rather than t90. Both families use
// the real fresh-flow lookup, and the old tuple remains bound throughout.
func TestAffinityMaxTimeDelayedSelectionKeepsCreationBoundary(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, version := range []int{4, 6} {
			parent := affinityTimeTestParent(t)
			old, _, _ := parent.sendUpdate(affinityTimeTestPath(version, 44000), flowPin{})
			created := old.openTime
			time.Sleep(30 * time.Second)
			donor := &multiClientChannel{
				ctx: parent.ctx, settings: parent.settings, healthy: true,
				args: &multiClientChannelArgs{DestinationStats: DestinationStats{IpFamily: IpFamilyDualstack}},
			}
			old.selectAffinityClient(donor)
			parent.bindClientFlow(old, donor)
			if !old.client.loadSelection().createTime.Equal(created) {
				t.Fatal("delayed first selection minted a later creation time")
			}
			time.Sleep(29 * time.Second)
			young, _, client := parent.sendUpdate(affinityTimeTestPath(version, 44001), flowPin{})
			if client != donor || !young.client.loadSelection().createTime.Equal(created) {
				t.Fatal("young inherited flow failed to keep the original update birth")
			}
			time.Sleep(time.Second)
			_, _, client = parent.sendUpdate(affinityTimeTestPath(version, 44002), flowPin{})
			if client != nil || old.client.Load() != donor || old.IsDone() {
				t.Fatal("setup delay extended the age cap or migrated the existing tuple")
			}
		}
	})
}

// Ranking must not publish provisional winners: otherwise a losing donor's
// older birth can leak into the immutable origin, depending on map iteration.
func TestAffinityMaxTimeDonorRankingPublishesOnlyWinner(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, version := range []int{4, 6} {
			for _, scored := range []bool{false, true} {
				parent := affinityTimeTestParent(t)
				parent.settings.ScoredAffinityDonor = scored
				old, _ := affinityTimeTestDonor(t, parent, version)
				time.Sleep(10 * time.Second)
				disabled := ReliabilitySettingsFrom(parent.settings)
				disabled.AffinityMaxTime = 0
				parent.SetReliabilitySettings(disabled)
				newer, _, current := parent.sendUpdate(affinityTimeTestPath(version, 44001), flowPin{})
				if current != nil {
					t.Fatal("disabled fixture inherited instead of creating a new origin")
				}
				winner := &multiClientChannel{
					ctx: parent.ctx, settings: parent.settings, healthy: true,
					args: &multiClientChannelArgs{DestinationStats: DestinationStats{IpFamily: IpFamilyDualstack}},
				}
				newer.selectAffinityClient(winner)
				parent.bindClientFlow(newer, winner)
				parent.SetReliabilitySettings(nil)
				origin := newer.client.loadSelection()
				if !origin.createTime.After(old.client.loadSelection().createTime) {
					t.Fatal("fixture donor creation times do not differ")
				}
				path := affinityTimeTestPath(version, 44002)
				adoptions := 0
				parent.beforeAffinityAdoptionForTest = func(client *multiClientChannel) {
					adoptions++
					var fresh *multiClientChannelUpdate
					if version == 4 {
						fresh = parent.ip4PathUpdates[path.ToIp4Path()]
					} else {
						fresh = parent.ip6PathUpdates[path.ToIp6Path()]
					}
					if client != winner || fresh == nil || fresh.client.Load() != nil {
						t.Fatal("ranking published or tried to publish a provisional donor")
					}
				}
				fresh, _, client := parent.sendUpdate(path, flowPin{})
				if adoptions != 1 || client != winner || fresh.client.loadSelection() != origin {
					t.Fatal("final donor did not retain exactly its winning lineage origin")
				}
			}
		}
	})
}

// Exact receipt CAS is stronger than pointer CAS: a failed operation on A
// cannot clear a later A after A->B->A, nor retain a provider after nil unbind.
func TestAffinityBindingExactSelectionCasRejectsAba(t *testing.T) {
	parent := affinityTimeTestParent(t)
	update, first := affinityTimeTestDonor(t, parent, 4)
	second := &multiClientChannel{ctx: parent.ctx, settings: parent.settings, healthy: true}
	old := update.client.loadSelection()
	update.selectAffinityClient(second)
	if update.client.clearSelection(old) || update.client.CompareAndSwap(first, nil) {
		t.Fatal("stale A operation erased B")
	}
	update.selectAffinityClient(first)
	fresh := update.client.loadSelection()
	if update.client.clearSelection(old) || update.client.loadSelection() != fresh {
		t.Fatal("old A receipt erased the replacement A selection")
	}
	if !update.client.clearSelection(fresh) || update.client.Load() != nil || update.client.loadSelection() != nil {
		t.Fatal("exact unbind retained client or provenance")
	}
	if !update.client.CompareAndSwap(nil, second) || update.client.Load() != second {
		t.Fatal("nil-to-provider atomic compatibility was lost")
	}
	update.client.Store(nil)
	if update.client.Load() != nil || update.client.loadSelection() != nil {
		t.Fatal("ordinary nil store retained the old receipt")
	}
}

// The public send captures A's receipt before the bounded queue call. An
// explicit A->B->A transition inside that call must survive its stale error.
func TestAffinityBindingFailedPublicSendCannotClearNewSameProviderSelection(t *testing.T) {
	assertMessagePoolOwnership(t)
	parent, update, closeParent := groupTestParent(t, DisableSecurityPolicy())
	defer closeParent()
	first := &multiClientChannel{ctx: parent.ctx, settings: parent.settings, healthy: true}
	second := &multiClientChannel{ctx: parent.ctx, settings: parent.settings, healthy: true}
	update.selectAffinityClient(first)
	old := update.client.loadSelection()
	var fresh *multiClientAffinitySelection
	calls := 0
	first.sendGroupForTest = func(*parsedPacketGroup, time.Duration, bool) (bool, error) {
		calls++
		update.selectAffinityClient(second)
		update.selectAffinityClient(first)
		fresh = update.client.loadSelection()
		return false, errors.New("synthetic stale queue error")
	}
	if collapseOwnershipPublicSend(t, parent, "batch", affinityTimeTestPath(4, 44001), 100, tcpFlagAck, []byte{1}) {
		t.Fatal("refused queue consumed the public packet")
	}
	if calls != 1 || fresh == nil || fresh == old || update.client.loadSelection() != fresh ||
		update.client.Load() != first || update.sourceReset.Load() {
		t.Fatal("failed old selection cleared or reset the new same-provider owner")
	}
}

func TestAffinityMaxTimeLineageCannotSlide(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		parent := affinityTimeTestParent(t)
		first, oldProvider := affinityTimeTestDonor(t, parent, 4)
		origin := first.client.loadSelection()
		time.Sleep(50 * time.Second)
		child, _, client := parent.sendUpdate(affinityTimeTestPath(4, 44001), flowPin{})
		if client != oldProvider || child.client.loadSelection() != origin {
			t.Fatal("young child did not share the originating selection")
		}
		time.Sleep(10 * time.Second)
		fresh, _, client := parent.sendUpdate(affinityTimeTestPath(4, 44002), flowPin{})
		if client != nil {
			t.Fatal("fresh child extended the expired provider-selection chain")
		}
		newProvider := &multiClientChannel{ctx: parent.ctx, settings: parent.settings, healthy: true}
		fresh.selectAffinityClient(newProvider)
		if fresh.client.loadSelection() == origin {
			t.Fatal("new actual selection failed to start its own origin")
		}
		time.Sleep(50 * time.Second)
		grandchild, _, client := parent.sendUpdate(affinityTimeTestPath(4, 44003), flowPin{})
		if client != newProvider || grandchild.client.loadSelection() != fresh.client.loadSelection() {
			t.Fatal("new healthy origin did not donate within its own window")
		}
		time.Sleep(11 * time.Second)
		_, _, client = parent.sendUpdate(affinityTimeTestPath(4, 44004), flowPin{})
		if client != nil || first.client.Load() != oldProvider || child.client.Load() != oldProvider {
			t.Fatal("second origin slid or old active connections migrated")
		}
	})
}

func TestAffinityMaxTimeActivityAndSynResetDoNotSlide(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		parent := affinityTimeTestParent(t)
		old, donor := affinityTimeTestDonor(t, parent, 4)
		origin := old.client.loadSelection()
		time.Sleep(59 * time.Second)
		parent.sendUpdate(old.ipPath, flowPin{})
		old.stateLock.Lock()
		old.resetSynGenerationWithLock(900)
		old.stateLock.Unlock()
		parent.stateLock.Lock()
		parent.rebuildAffinityDonorIndexesWithLock(true)
		parent.stateLock.Unlock()
		time.Sleep(time.Second)
		_, _, client := parent.sendUpdate(affinityTimeTestPath(4, 44001), flowPin{})
		if client != nil || old.client.loadSelection() != origin || old.client.Load() != donor {
			t.Fatal("activity, SYN bookkeeping or index rebuild renewed affinity age")
		}
	})
}

func TestAffinityMaxTimeDonorHealthAgeMatrix(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, age := range []time.Duration{0, time.Minute} {
			for _, state := range []string{"healthy", "warning", "unhealthy", "unknown", "quarantine", "draining", "canceled", "terminal"} {
				parent := affinityTimeTestParent(t)
				old, donor := affinityTimeTestDonor(t, parent, 4)
				donorCtx, cancelDonor := context.WithCancel(parent.ctx)
				t.Cleanup(cancelDonor)
				donor.ctx = donorCtx
				switch state {
				case "warning":
					donor.warning = true
				case "unhealthy":
					donor.healthy, donor.lastUnhealthyTime = false, time.Now()
				case "unknown":
					donor.healthy = false
				case "quarantine":
					donor.quarantined = true
				case "draining":
					donor.warning, donor.warnCause = true, warnDraining
				case "canceled":
					cancelDonor()
				case "terminal":
					donor.endErr = errors.New("synthetic terminal provider failure")
				}
				time.Sleep(age)
				_, _, client := parent.sendUpdate(affinityTimeTestPath(4, 44001), flowPin{})
				if (client == donor) != (state == "healthy" && age < time.Minute) {
					t.Fatalf("state=%s age=%v inherited=%v", state, age, client == donor)
				}
				if old.client.Load() != donor {
					t.Fatal("donor refusal rewrote an existing connection")
				}
			}
		}
	})
}

// The hook is an explicit transition boundary, not a timing-based race.
func testAffinityMaxTimeHealthChange(t *testing.T, warning bool) {
	t.Helper()
	parent := affinityTimeTestParent(t)
	_, donor := affinityTimeTestDonor(t, parent, 4)
	visited := false
	parent.beforeAffinityAdoptionForTest = func(client *multiClientChannel) {
		visited = true
		client.stateLock.Lock()
		if warning {
			client.warning = true
		} else {
			client.healthy = false
		}
		client.stateLock.Unlock()
	}
	_, _, client := parent.sendUpdate(affinityTimeTestPath(4, 44001), flowPin{})
	if !visited || client == donor {
		t.Fatal("health changed after preliminary lookup but donor was adopted")
	}
}

func TestAffinityMaxTimeWarningBetweenLookupAndAdoption(t *testing.T) {
	testAffinityMaxTimeHealthChange(t, true)
}
func TestAffinityMaxTimeUnhealthyBetweenLookupAndAdoption(t *testing.T) {
	testAffinityMaxTimeHealthChange(t, false)
}

func TestAffinityMaxTimeDisconnectBetweenLookupAndAdoption(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newGroupDispositionQueueFixture(t, nil, nil)
		parent := affinityTimeTestParent(t)
		old, donor := affinityTimeTestDonor(t, parent, 4)
		donor.client = f.client
		if !donor.hasActiveTransport() {
			t.Fatal("fixture carrier was not active")
		}
		parent.beforeAffinityAdoptionForTest = func(*multiClientChannel) {
			f.client.RouteManager().RemoveTransport(f.transport)
		}
		_, _, client := parent.sendUpdate(affinityTimeTestPath(4, 44001), flowPin{})
		if client != nil || old.client.Load() != donor || donor.hasActiveTransport() {
			t.Fatal("a disconnected donor survived the final carrier snapshot")
		}
	})
}

func TestAffinityMaxTimeUnderlyingClientCloseBeforeAdoption(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newGroupDispositionQueueFixture(t, nil, nil)
		parent := affinityTimeTestParent(t)
		old, donor := affinityTimeTestDonor(t, parent, 4)
		donor.client = f.client
		visited := false
		parent.beforeAffinityAdoptionForTest = func(*multiClientChannel) {
			visited = true
			f.client.Close()
		}
		_, _, client := parent.sendUpdate(affinityTimeTestPath(4, 44001), flowPin{})
		if !visited || client != nil || old.client.Load() != donor || donor.IsDone() {
			t.Fatal("underlying client closure did not refuse new adoption independently of the channel context")
		}
	})
}

func TestAffinityMaxTimeDnsAnswerCannotRenewOldOrigin(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, version := range []int{4, 6} {
			parent := affinityTimeTestParent(t)
			old, _ := affinityTimeTestDonor(t, parent, version)
			address := netip.MustParseAddr("203.0.113.63")
			if version == 6 {
				address = netip.MustParseAddr("2001:db8:3::63")
			}
			domain := "shop.affinity.example"
			if !parent.bindDnsResultToExit(old.ipPath, domain, []netip.Addr{address}) {
				t.Fatal("young healthy DNS selection was not recorded")
			}
			time.Sleep(59 * time.Second)
			if !parent.bindDnsResultToExit(old.ipPath, domain, []netip.Addr{address}) {
				t.Fatal("young DNS refresh refused")
			}
			hint := parent.dnsAddressExitHints[address]
			if hint.selection != old.client.loadSelection() || time.Since(hint.createTime) != 0 {
				t.Fatal("DNS refresh did not separate answer age from selection origin")
			}
			time.Sleep(time.Second)
			parent.config.Store(&multiClientConfig{serverNameLookup: stubServerNameLookup{names: []string{domain}}})
			for _, byName := range []bool{false, true} {
				if byName {
					clear(parent.dnsAddressExitHints)
				}
				path := affinityTimeTestPath(version, 44001)
				path.DestinationIp = address.AsSlice()
				if byName {
					path.SourcePort++
				}
				_, _, client := parent.sendUpdate(path, flowPin{})
				if client != nil {
					t.Fatalf("v%d byName=%v refreshed DNS hint revived old selection", version, byName)
				}
			}
			if parent.bindDnsResultToExit(old.ipPath, domain, []netip.Addr{address}) {
				t.Fatal("expired DoH selection created another young DNS hint")
			}
		}
	})
}

func TestAffinityMaxTimeLateNameBridgeExpires(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, version := range []int{4, 6} {
			parent := affinityTimeTestParent(t)
			parent.settings.ServerNameAffinityBridge = true
			lookup := newTestingLearningServerNameLookup()
			parent.config.Store(&multiClientConfig{serverNameLookup: lookup})
			old, donor := affinityTimeTestDonor(t, parent, version)
			origin := old.client.loadSelection()
			lookup.learn(old.ipPath.DestinationIp.String(), "shop.affinity.example")
			time.Sleep(59 * time.Second)
			young, _, client := parent.sendUpdate(affinityTimeTestPath(version, 44001), flowPin{})
			if client != donor || young.client.loadSelection() != origin {
				t.Fatalf("v%d young late-name bridge did not preserve origin", version)
			}
			parent.stateLock.Lock()
			parent.rebuildAffinityDonorIndexesWithLock(true)
			parent.stateLock.Unlock()
			time.Sleep(time.Second)
			_, _, client = parent.sendUpdate(affinityTimeTestPath(version, 44002), flowPin{})
			if client != nil || old.client.Load() != donor || young.client.Load() != donor {
				t.Fatalf("v%d bridge/rebuild resurrected the expired origin or moved old tuples", version)
			}
		}
	})
}

// Recording a healthy hint is not enduring health authority. Both lookup
// indexes must revalidate the same channel when the fresh flow adopts it.
func TestAffinityMaxTimeDnsHealthChangesAfterRecording(t *testing.T) {
	for _, version := range []int{4, 6} {
		for _, byName := range []bool{false, true} {
			for _, warning := range []bool{false, true} {
				parent := affinityTimeTestParent(t)
				old, donor := affinityTimeTestDonor(t, parent, version)
				address := netip.MustParseAddr("203.0.113.63")
				if version == 6 {
					address = netip.MustParseAddr("2001:db8:3::63")
				}
				domain := "shop.affinity.example"
				if !parent.bindDnsResultToExit(old.ipPath, domain, []netip.Addr{address}) {
					t.Fatal("healthy fixture did not publish its DNS hint")
				}
				if byName {
					clear(parent.dnsAddressExitHints)
				}
				visited := false
				parent.beforeAffinityAdoptionForTest = func(client *multiClientChannel) {
					visited = true
					client.stateLock.Lock()
					if warning {
						client.warning = true
					} else {
						client.healthy = false
					}
					client.stateLock.Unlock()
				}
				path := affinityTimeTestPath(version, 44001)
				path.DestinationIp = address.AsSlice()
				update := newMultiClientChannelUpdate(parent.ctx, path)
				parent.stateLock.Lock()
				bound := parent.inheritDnsExitHintWithLock(update, path, []*IpPath{{ServerName: affinityNameForServerName(domain)}})
				parent.stateLock.Unlock()
				update.Close()
				if !visited || bound || update.client.Load() != nil || old.client.Load() != donor {
					t.Fatalf("v%d name=%v warning=%v DNS health change bypassed actual adoption", version, byName, warning)
				}
			}
		}
	}
}

func TestAffinityMaxTimeExpiredHostPinIsExplicit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, version := range []int{4, 6} {
			parent := affinityTimeTestParent(t)
			old, donor := affinityTimeTestDonor(t, parent, version)
			time.Sleep(time.Minute)
			pinned, _, client := parent.sendUpdate(affinityTimeTestPath(version, 44001), flowPin{site: true})
			if client != donor || pinned.client.loadSelection() != old.client.loadSelection() {
				t.Fatal("explicit pin did not retain the original provider without renewing its age")
			}
			_, _, client = parent.sendUpdate(affinityTimeTestPath(version, 44002), flowPin{})
			if client != nil {
				t.Fatal("ordinary descendant inherited a pin's age exemption")
			}
		}
	})
}

// Age controls only new-flow placement in this independently testable slice.
// Same-tuple SYN renewal is a separate, not-yet-implemented transaction.
func TestAffinityMaxTimeEstablishedTcpAndUdpDataKeepOwner(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, version := range []int{4, 6} {
			for _, protocol := range []IpProtocol{IpProtocolTcp, IpProtocolUdp} {
				parent := affinityTimeTestParent(t)
				path := affinityTimeTestPath(version, 44000)
				path.Protocol, path.Syn = protocol, false
				old, _, _ := parent.sendUpdate(path, flowPin{})
				donor := &multiClientChannel{ctx: parent.ctx, settings: parent.settings, healthy: true}
				old.selectAffinityClient(donor)
				old.receivedInbound.Store(true)
				time.Sleep(121 * time.Second)
				donor.healthy = false
				current, _, client := parent.sendUpdate(path, flowPin{})
				if current != old || client != donor || old.IsDone() {
					t.Fatalf("v%d protocol=%v established data migrated on age/health", version, protocol)
				}
			}
		}
	})
}

func TestAffinityMaxTimeFlowIdleDefaults(t *testing.T) {
	settings := DefaultMultiClientSettings()
	if settings.SequenceIdleTimeout != 5*time.Minute || settings.TcpSequenceIdleTimeout != 10*time.Minute ||
		settings.StickyFlowIdleTimeout < 5*time.Minute {
		t.Fatal("default keepalive is shorter than the requested protocol lifetime")
	}
	parent := affinityTimeTestParent(t)
	override := ReliabilitySettingsFrom(settings)
	override.SequenceIdleTimeout, override.TcpSequenceIdleTimeout = time.Second, 2*time.Second
	parent.SetReliabilitySettings(override)
	udp := affinityTimeTestPath(4, 44001)
	udp.Protocol = IpProtocolUdp
	if parent.sequenceIdleTimeout(udp) != time.Second || parent.sequenceIdleTimeout(affinityTimeTestPath(4, 44002)) != 2*time.Second {
		t.Fatal("explicit runtime idle overrides were silently clamped")
	}
}

// The shared reaper's explicit clock proves lifetime boundaries without
// keeping a real five-minute timer or relying on a goroutine to wake in time.
func TestAffinityMaxTimeFlowRetainedBeforeFiveMinutes(t *testing.T) {
	for _, version := range []int{4, 6} {
		parent := affinityTimeTestParent(t)
		now := time.Now()
		udpPath := affinityTimeTestPath(version, 44001)
		udpPath.Protocol, udpPath.Syn = IpProtocolUdp, false
		udp, _, _ := parent.sendUpdate(udpPath, flowPin{})
		tcp, _, _ := parent.sendUpdate(affinityTimeTestPath(version, 44002), flowPin{})
		udp.activityTime, tcp.activityTime = now, now
		retired, delay, live := parent.detachIdleFlows(now.Add(299 * time.Second))
		if len(retired) != 0 || !live || delay != time.Second || udp.IsDone() || tcp.IsDone() {
			t.Fatal("default flow retired before the five-minute boundary")
		}
		retired, delay, live = parent.detachIdleFlows(now.Add(300 * time.Second))
		if len(retired) != 1 || retired[0].update != udp || !live || delay != 5*time.Minute || tcp.IsDone() {
			t.Fatal("five-minute boundary failed to retire only UDP")
		}
		udp.Close()
		retired, delay, live = parent.detachIdleFlows(now.Add(600*time.Second - time.Nanosecond))
		if len(retired) != 0 || !live || delay != time.Nanosecond || tcp.IsDone() {
			t.Fatal("TCP did not retain its existing ten-minute default")
		}
		retired, _, live = parent.detachIdleFlows(now.Add(600 * time.Second))
		if len(retired) != 1 || retired[0].update != tcp || live {
			t.Fatal("TCP did not retire at its exact idle deadline")
		}
		tcp.Close()
	}
}

// An actual first admission stamps the origin. A child inherits at59s but a
// new flow at60s reaches the real one-provider race again, with pool-balanced
// physical wire and peer acknowledgements after each public admission.
func TestAffinityMaxTimePublicFreshFlowRaceBoundary(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newGroupDispositionQueueFixture(t, nil, nil)
		f.parent.sendClientPathForTest = nil
		f.parent.ip4PathUpdates = map[Ip4Path]*multiClientChannelUpdate{}
		f.parent.ip6PathUpdates = map[Ip6Path]*multiClientChannelUpdate{}
		f.parent.affinityIp4Paths = map[Ip4Path]map[Ip4Path]time.Time{}
		f.parent.affinityIp6Paths = map[Ip6Path]map[Ip6Path]time.Time{}
		f.parent.flowUpdates = map[*multiClientChannelUpdate]bool{}
		f.selected.healthy = true
		t.Cleanup(func() {
			for update := range f.parent.flowUpdates {
				update.Close()
			}
		})
		races := 0
		f.parent.groupRaceCandidatesForTest = func(*parsedPacketGroup) []*multiClientChannel {
			races++
			return []*multiClientChannel{f.selected}
		}
		f.unpark()
		for index, age := range []time.Duration{0, 59 * time.Second, time.Second} {
			time.Sleep(age)
			path := affinityTimeTestPath(4, 44100+index)
			packet := ipOosTcpPacketSequence(path, tcpFlagSyn, path.SequenceNumber, nil)
			if f.offer(packet) != 1 {
				t.Fatalf("public SYN %d was refused", index)
			}
			f.acknowledgeAll(packet)
			want := 1
			if index == 2 {
				want = 2
			}
			if races != want {
				t.Fatalf("after SYN %d race count=%d want=%d", index, races, want)
			}
		}
	})
}
