// Real route and physical-owner barriers pin byte admission independently of
// message-count capacity, including the live Client handoff during retirement.
package connect

import (
	"bytes"
	"context"
	"encoding/binary"
	"math"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gorilla/websocket"
	"github.com/urnetwork/connect/protocol"
)

// Copy mutable lane state under its owner lock. Callers still establish the
// operation's readiness or join barrier; a snapshot is not a progress oracle.
func h1RetainedTestByteCount(lane *platformH1RetainedLane) ByteCount {
	lane.stateLock.Lock()
	defer lane.stateLock.Unlock()
	return lane.usedByteCount
}

// The sdk's acknowledgement counts are explicit here; the separate actual
// sdk-constructor test pins their wiring. Counts and the quarter-target root
// remain unchanged, including the ordinary window plus provider and handoff.
func TestPlatformH1RetainedProfileClaims(t *testing.T) {
	previous := MemoryBudget()
	SetMemoryBudget(mib(32))
	defer SetMemoryBudget(previous)
	for _, c := range []struct {
		target           ByteCount
		priority, graphs int
		claim            ByteCount
	}{
		{target: mib(32), priority: 10, graphs: 10, claim: 697592},
		{target: mib(64), priority: 21, graphs: 16, claim: 1004924},
	} {
		settings := DefaultPlatformTransportSettingsWithMemoryTarget(c.target)
		if c.target == mib(64) && (settings.h1RetainedByteAccounting || settings.H1MemoryReservationByteCount() != kib(512)) {
			t.Fatal("explicit desktop owner inherited constrained process accounting")
		}
		ApplyMobilePlatformTransportMemoryPolicy(settings, c.target)
		settings.H1AckPriorityBufferSize = c.priority
		owner := newPlatformH1RetainedBudget(settings)
		if settings.TransportBufferSize != 32 || settings.H1MaxMessageByteCount != 16384 ||
			owner.send.byteLimit != 131456 || owner.receive.byteLimit != 131456 || owner.control.byteLimit != 131456 ||
			owner.priority.byteLimit != ByteCount(c.priority)*4108 || settings.H1MemoryReservationByteCount() != c.claim {
			t.Fatalf("target=%d changed count/max or retained profile: claim=%d owner=%+v", c.target, settings.H1MemoryReservationByteCount(), owner)
		}
		root := NewPlatformTransportBudgetForMemoryTarget(c.target)
		var claims []*platformTransportBudgetReservation
		for range c.graphs {
			claim := root.register(platformTransportBudgetH1, settings.H1MemoryReservationByteCount(), false)
			claims = append(claims, claim)
			if !claim.TryAcquire() {
				t.Error("normal window/provider/replacement graphs did not fit their unchanged root")
			}
		}
		if root.Stats().UsedByteCount != c.claim*ByteCount(c.graphs) || root.Stats().UsedByteCount > c.target/4 {
			t.Errorf("profile graph claims escaped or undercharged their owner: %+v", root.Stats())
		}
		for _, claim := range claims {
			claim.Release()
		}
		if stats := root.Stats(); stats.UsedByteCount != 0 || stats.ReservedByteCount != stats.ReleasedByteCount {
			t.Fatal(stats)
		}
	}
}

// Even count-zero/count-one routes can carry the maximum valid message. A
// malformed count/maximum saturates the claim, never wraps it into cheap bytes.
func TestPlatformH1RetainedNarrowAndOverflowProfiles(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, count := range []int{0, 1} {
		settings := DefaultPlatformTransportSettingsWithMemoryTarget(mib(32))
		settings.TransportBufferSize = count
		settings.H1AckPriorityBufferSize = 0
		settings.H1MaxMessageByteCount = 0
		settings.FramerSettings = nil
		owner := newPlatformH1RetainedBudget(settings)
		message := MessagePoolGet(16 * 1024)
		cost := ByteCount(cap(message))
		for _, lane := range []*platformH1RetainedLane{&owner.send, &owner.receive, &owner.control} {
			if !lane.tryReserve(cost) {
				t.Errorf("count=%d valid maximum root refused", count)
			} else {
				lane.release(cost)
			}
		}
		if owner.priority.byteLimit != 0 || owner.priority.tryReserve(cost) {
			t.Error("disabled priority acquired byte permission")
		}
		MessagePoolReturn(message)
	}
	settings := DefaultPlatformTransportSettingsWithMemoryTarget(mib(32))
	settings.TransportBufferSize = math.MaxInt
	if got := settings.H1MemoryReservationByteCount(); got != ByteCount(math.MaxInt64) {
		t.Fatalf("count overflow claim=%d", got)
	}
	settings.TransportBufferSize = 1
	settings.H1MaxMessageByteCount = math.MaxInt64
	if got := settings.H1MemoryReservationByteCount(); got != ByteCount(math.MaxInt64) {
		t.Fatalf("maximum overflow claim=%d", got)
	}
	settings.H1MaxMessageByteCount = 128 * 1024
	settings.H1BudgetByteCount = 1
	settings.FramerSettings = DefaultFramerSettings(128 * 1024)
	settings.FramerSettings.SplitMinimumLen = 256 * 1024
	minimum := ByteCount(4*(128*1024+1)+8192+MessagePoolMetaByteCount+webSocketWriteBatchMaxByteCount) + retainedMessageCapacity(128*1024+4)
	if settings.h1RetainedScratchMinimum() < minimum || settings.H1MemoryReservationByteCount() < minimum+3*retainedMessageCapacity(128*1024) {
		t.Fatal("custom split/tiny base omitted the full framed writer copy")
	}
}

// Status composes the actual application graph, while a nested raw tcp/tls
// connection still owns only its base row. Overflow never advertises h3.
func TestPlatformH1RetainedAutoAndRawBaseComposition(t *testing.T) {
	for _, c := range []struct {
		target   ByteCount
		priority int
	}{
		{target: mib(32), priority: 10},
		{target: mib(64), priority: 21},
	} {
		settings := DefaultPlatformTransportSettingsWithMemoryTarget(c.target)
		ApplyMobilePlatformTransportMemoryPolicy(settings, c.target)
		settings.H1AckPriorityBufferSize = c.priority
		settings.ModePreferences = map[TransportMode]int{TransportModeH1: 1, TransportModeH3: 2}
		graph := settings.H1MemoryReservationByteCount()
		if graph <= settings.H1BudgetByteCount {
			t.Fatal("application graph lost its queue ownership")
		}
		settings.PlatformTransportBudget = NewPlatformTransportBudget(graph-1, 1)
		if modes := PlatformTransportAutoEligibility(settings); len(modes) != 0 {
			t.Fatalf("unfunded h1 graph advertised Auto: %v", modes)
		}
		settings.PlatformTransportBudget = NewPlatformTransportBudget(graph+settings.H3BudgetByteCount-1, 1)
		if modes := PlatformTransportAutoEligibility(settings); !modes[TransportModeH1] || modes[TransportModeH3] {
			t.Fatalf("unfunded combined graph advertised h3: %v", modes)
		}
		settings.PlatformTransportBudget = NewPlatformTransportBudget(graph+settings.H3BudgetByteCount, 1)
		if modes := PlatformTransportAutoEligibility(settings); !modes[TransportModeH1] || !modes[TransportModeH3] {
			t.Fatalf("exactly funded graph refused Auto: %v", modes)
		}
		settings.PlatformTransportBudget = NewPlatformTransportBudget(settings.H1BudgetByteCount, 1)
		ctx := context.WithValue(context.Background(), extenderTransportSettingsContextKey{}, settings)
		claim, err := acquireExtenderTcpMemory(ctx)
		if err != nil {
			t.Fatalf("raw outer connection was charged application queues: %v", err)
		}
		if got := settings.PlatformTransportBudget.Stats().UsedByteCount; got != settings.H1BudgetByteCount {
			t.Errorf("raw outer claim=%d, base=%d", got, settings.H1BudgetByteCount)
		}
		claim.Release()
		if stats := settings.PlatformTransportBudget.Stats(); stats.UsedByteCount != 0 || stats.ReservedByteCount != stats.ReleasedByteCount {
			t.Fatal(stats)
		}
	}
	settings := DefaultPlatformTransportSettingsWithMemoryTarget(mib(32))
	settings.TransportBufferSize = math.MaxInt
	settings.PlatformTransportBudget = NewPlatformTransportBudget(ByteCount(math.MaxInt64), 1)
	if settings.H1MemoryReservationByteCount() != ByteCount(math.MaxInt64) {
		t.Fatal("overflow fixture did not saturate h1")
	}
	if modes := PlatformTransportAutoEligibility(settings); modes[TransportModeH3] || modes[TransportModeH3Dns] || modes[TransportModeH3DnsPump] {
		t.Fatalf("saturated h1 plus positive h3 fit a max root: %v", modes)
	}
	settings.ModePreferences = map[TransportMode]int{TransportModeH3Dns: 1}
	settings.H3BudgetByteCount = ByteCount(math.MaxInt64)
	if modes := PlatformTransportAutoEligibility(settings); len(modes) != 0 {
		t.Fatalf("h3 plus translation overflow advertised eligible: %v", modes)
	}
}

// An alias spends its actual retained capacity, not its visible payload len.
// Each independent lane can still make progress while ordinary credit is full.
func TestPlatformH1RetainedLanesChargeRootsAndStayIndependent(t *testing.T) {
	assertMessagePoolOwnership(t)
	settings := DefaultPlatformTransportSettingsWithMemoryTarget(mib(32))
	settings.TransportBufferSize, settings.H1AckPriorityBufferSize = 1, 1
	owner := newPlatformH1RetainedBudget(settings)
	message := MessagePoolGet(16 * 1024)[:1]
	cost := ByteCount(cap(message))
	if !owner.send.tryReserve(cost) || owner.send.tryReserve(1) {
		t.Fatal("ordinary lane did not charge the complete root")
	}
	for _, lane := range []*platformH1RetainedLane{&owner.receive, &owner.control, &owner.priority} {
		if !lane.tryReserve(cost) {
			t.Fatal("ordinary pressure consumed a sibling lane")
		}
		lane.release(cost)
	}
	returnH1RetainedMessage(message, &owner.send)
	if owner.send.usedByteCount != 0 {
		t.Fatal("maximum-size root retained lane credit after return")
	}

	// The maximum frame is larger than the current pool classes. Exercise
	// reference counting separately with an actual pooled root, not a gc alias.
	message = MessagePoolGet(8192)[:1]
	pooledCost := ByteCount(cap(message))
	messageOwned, creditHeld := true, false
	var share []byte
	defer func() {
		if creditHeld {
			owner.send.release(pooledCost)
		}
		if messageOwned {
			MessagePoolReturn(message)
		}
		if share != nil {
			MessagePoolReturn(share)
		}
	}()
	if pooled, shared := MessagePoolCheck(message); !pooled || shared {
		t.Fatal("reference-count fixture did not acquire an unshared pooled root")
	}
	var pool *messagePool
	for _, candidate := range orderedMessagePools() {
		if candidate.size == 8192 {
			pool = candidate
			break
		}
	}
	if pool == nil {
		t.Fatal("reference-count fixture has no live 8192-byte pool class")
	}
	root := message[:cap(message)]
	shard, _ := pool.shardFor(root)
	referenceCount := func() uint16 {
		shard.stateLock.Lock()
		defer shard.stateLock.Unlock()
		return binary.BigEndian.Uint16(root[pool.size+10:])
	}
	if referenceCount() != 1 || !owner.send.tryReserve(pooledCost) {
		t.Fatal("initial pooled root ownership or lane admission changed")
	}
	creditHeld = true
	share = MessagePoolShareReadOnly(message)
	if pooled, shared := MessagePoolCheck(share); !pooled || !shared || referenceCount() != 2 {
		t.Fatal("read-only share did not retain the same pooled root")
	}
	returnH1RetainedMessage(message, &owner.send)
	messageOwned, creditHeld = false, false
	if pooled, shared := MessagePoolCheck(share); !pooled || !shared || referenceCount() != 1 || owner.send.usedByteCount != 0 {
		t.Fatal("lane return did not settle just its own pooled reference")
	}
	returned := MessagePoolReturn(share)
	share = nil
	if !returned {
		t.Fatal("lane return confused its share with final physical-root ownership")
	}
	if pooled, _ := MessagePoolCheck(root); pooled {
		t.Fatal("final pooled return left an owned root")
	}
}

// Every actual selector branch, including the reflective route set, must
// park on bytes despite empty count queues and wake on exact credit return.
func TestPlatformH1RetainedSelectorBlockedPaths(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		for _, count := range []int{1, 2, 3} {
			ctx, cancel := context.WithCancel(context.Background())
			selector := NewMultiRouteSelector(ctx, "synthetic-h1-bytes", NewNoopLogger(), DestinationId(NewId()), false)
			routes := make([]Route, count)
			lanes := make([]platformH1RetainedLane, count)
			message := MessagePoolGet(1200)
			cost := ByteCount(cap(message))
			for i := range routes {
				routes[i] = make(Route, 1)
				lanes[i].byteLimit = cost
				if !lanes[i].tryReserve(cost) {
					t.Fatal("synthetic prior writer claim refused")
				}
				selector.updateTransportWithProperties(NewSendGatewayTransportWithType(TransportTypeH1), []Route{routes[i]}, TransferCarrierProperties{h1SendRetained: &lanes[i]})
			}
			if selector.reliableRouteHasCapacity() {
				t.Fatal("empty count queues hid byte saturation")
			}
			result := make(chan error, 1)
			go func() { result <- selector.Write(ctx, message, -1) }()
			synctest.Wait()
			select {
			case err := <-result:
				t.Fatalf("byte-saturated selector completed: %v", err)
			default:
			}
			lanes[count-1].release(cost)
			synctest.Wait()
			select {
			case err := <-result:
				if err != nil {
					t.Fatal(err)
				}
			default:
				t.Fatal("returned byte permission did not wake the selector")
			}
			select {
			case got := <-routes[count-1]:
				if &got[0] != &message[0] {
					t.Fatal("selector copied the original root")
				}
				returnH1RetainedMessage(got, &lanes[count-1])
			default:
				t.Fatal("selector bypassed the only funded route")
			}
			for i := range count - 1 {
				lanes[i].release(cost)
			}
			selector.Close()
			cancel()
			for i := range lanes {
				if lanes[i].usedByteCount != 0 {
					t.Fatal("blocked selector leaked credit")
				}
			}
		}
	})
}

// Count-blocked selectors with spare bytes must reserve without constructing
// byte-pressure notifications. Record the causal result after all owners join
// so the original implementation reports every route-count case.
func TestPlatformH1RetainedFundedSelectorDoesNotSubscribe(t *testing.T) {
	assertMessagePoolOwnership(t)
	type observation struct {
		routeCount int
		routeIndex int
		cost       ByteCount
		reserved   ByteCount
		subscribed bool
	}
	var observations []observation
	synctest.Test(t, func(t *testing.T) {
		for _, count := range []int{1, 2, 3} {
			func() {
				ctx, cancel := context.WithCancel(context.Background())
				selector := NewMultiRouteSelector(ctx, "synthetic-h1-funded", NewNoopLogger(), DestinationId(NewId()), false)
				message := MessagePoolGet(1200)
				cost := ByteCount(cap(message))
				lanes := make([]platformH1RetainedLane, count)
				for i := range lanes {
					lanes[i].byteLimit = 2 * cost
					selector.updateTransportWithProperties(NewSendGatewayTransportWithType(TransportTypeH1), []Route{make(Route)}, TransferCarrierProperties{h1SendRetained: &lanes[i]})
				}
				result := make(chan error, 1)
				joined := false
				go func() { result <- selector.Write(ctx, message, -1) }()
				defer func() {
					cancel()
					if !joined {
						<-result
					}
					selector.Close()
					MessagePoolReturn(message)
				}()
				synctest.Wait()
				select {
				case err := <-result:
					joined = true
					t.Fatalf("unbuffered selector returned before cancellation: %v", err)
				default:
				}
				for i := range lanes {
					lanes[i].stateLock.Lock()
					observations = append(observations, observation{routeCount: count, routeIndex: i, cost: cost, reserved: lanes[i].usedByteCount, subscribed: lanes[i].notify != nil})
					lanes[i].stateLock.Unlock()
				}
				cancel()
				if err := <-result; err == nil {
					t.Error("canceled count-blocked selector reported success")
				}
				joined = true
				for i := range lanes {
					if lanes[i].usedByteCount != 0 {
						t.Errorf("routes=%d lane=%d cancellation retained %d bytes", count, i, lanes[i].usedByteCount)
					}
				}
			}()
		}
	})
	for _, observed := range observations {
		if observed.reserved != observed.cost {
			t.Errorf("routes=%d lane=%d count-blocked root was not prepaid: %d != %d", observed.routeCount, observed.routeIndex, observed.reserved, observed.cost)
		}
		if observed.subscribed {
			t.Errorf("H1_FUNDED_SELECTOR_ALLOCATED_BYTE_NOTIFY routes=%d lane=%d", observed.routeCount, observed.routeIndex)
		}
	}
}

// A smaller funded reservation must not clear or close the notification of
// an existing larger byte waiter; only its later credit return wakes that owner.
func TestPlatformH1RetainedFundedReservationPreservesWaiter(t *testing.T) {
	assertMessagePoolOwnership(t)
	message := MessagePoolGet(1200)
	defer MessagePoolReturn(message)
	waitingMessage := MessagePoolGet(4096)
	defer MessagePoolReturn(waitingMessage)
	cost := ByteCount(cap(message))
	lane := platformH1RetainedLane{byteLimit: 2 * cost}
	if !lane.tryReserve(cost) {
		t.Fatal("initial synthetic owner refused")
	}
	defer lane.release(cost)
	waiter := prepareH1RetainedWrite(make(Route), &lane, waitingMessage)
	defer waiter.releaseUnless(false)
	if waiter.reserved || waiter.route != nil || waiter.notify == nil {
		t.Fatal("larger root did not subscribe at actual byte pressure")
	}
	notify := waiter.notify
	funded := prepareH1RetainedWrite(make(Route), &lane, message)
	defer funded.releaseUnless(false)
	if !funded.reserved || funded.route == nil || funded.notify != nil || lane.notify != notify {
		t.Fatal("funded reservation changed the existing waiter's subscription")
	}
	select {
	case <-notify:
		t.Fatal("funded reservation prematurely woke another byte waiter")
	default:
	}
	funded.releaseUnless(false)
	select {
	case <-notify:
	default:
		t.Fatal("returned bytes did not wake the registered waiter")
	}
}

// Canceled, timed-out, and withdrawn selections roll back every speculative
// route credit. A valid larger-than-lane root is refused without parking.
func TestPlatformH1RetainedSelectorRollback(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		for _, count := range []int{1, 2, 3} {
			for _, reason := range []string{"cancel", "timeout", "replace", "oversize"} {
				func() {
					ctx, cancel := context.WithCancel(context.Background())
					selector := NewMultiRouteSelector(ctx, "synthetic-h1-rollback", NewNoopLogger(), DestinationId(NewId()), false)
					message := MessagePoolGet(1200)
					cost := ByteCount(cap(message))
					lanes := make([]platformH1RetainedLane, count)
					transports := make([]Transport, count)
					for i := range lanes {
						lanes[i].byteLimit = cost
						if reason == "oversize" {
							lanes[i].byteLimit--
						}
						transports[i] = NewSendGatewayTransportWithType(TransportTypeH1)
						selector.updateTransportWithProperties(transports[i], []Route{make(Route)}, TransferCarrierProperties{h1SendRetained: &lanes[i]})
					}
					result := make(chan error, 1)
					timeout := time.Duration(-1)
					if reason == "timeout" {
						timeout = time.Second
					}
					started := time.Now()
					joined := false
					go func() { result <- selector.Write(ctx, message, timeout) }()
					defer func() {
						cancel()
						if !joined {
							<-result
						}
						selector.Close()
						MessagePoolReturn(message)
					}()
					synctest.Wait()
					// Wait returns before the bubble advances time. The charge is
					// inspected at that stable boundary, before advancing the timer.
					if reason == "timeout" && !time.Now().Equal(started) {
						t.Fatal("selector deadline advanced before the admission snapshot")
					}
					if reason != "oversize" {
						for i := range lanes {
							if h1RetainedTestByteCount(&lanes[i]) != cost {
								t.Fatalf("%s routes=%d speculative route was not charged", reason, count)
							}
						}
					}
					switch reason {
					case "cancel":
						cancel()
					case "timeout":
						time.Sleep(time.Second)
					case "replace":
						for _, transport := range transports {
							selector.updateTransportWithProperties(transport, nil, TransferCarrierProperties{})
						}
						synctest.Wait()
						for i := range lanes {
							if h1RetainedTestByteCount(&lanes[i]) != 0 {
								t.Fatal("withdrawn route retained speculative credit")
							}
						}
						cancel()
					}
					synctest.Wait()
					select {
					case err := <-result:
						joined = true
						if err == nil {
							t.Fatal("failed selector reported success")
						}
					default:
						t.Fatal("selector did not finish at its terminal boundary")
					}
					if reason == "timeout" && time.Since(started) != timeout {
						t.Fatal("selector did not settle at its exact virtual deadline")
					}
					for i := range lanes {
						if lanes[i].usedByteCount != 0 {
							t.Fatal("failed selector leaked credit")
						}
					}
				}()
			}
		}
	})
}

// The ordinary physical writer holds its charge after channel dequeue. A
// control echo and priority acknowledgement have distinct charged owners.
func TestPlatformH1RetainedPhysicalWriterAndControl(t *testing.T) {
	resetH1UpgradeTestState(t)
	MessagePoolReturn(MessagePoolGet(1))
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture := newH1LivenessFixtureWithBackpressure(t, true, 0, func(settings *PlatformTransportSettings) {
			ApplyMobilePlatformTransportMemoryPolicy(settings, mib(32))
			settings.H1AckPriorityBufferSize = 1
		})
		stopH1BackpressurePeerReads(t, fixture)
		message := MessagePoolGet(16 * 1024)
		cost := ByteCount(cap(message))
		fixture.sendMessage(t, message)
		synctest.Wait()
		owner := fixture.transport.h1RetainedBudget
		if fixture.client.writesInProgress.Load() != 1 || h1RetainedTestByteCount(&owner.send) != cost || len(fixture.send) != 0 {
			t.Fatal("dequeue prematurely returned the blocked writer's root credit")
		}
		ack := MessagePoolGet(64)
		ackCost := ByteCount(cap(ack))
		if ok, _ := fixture.writer.(*MultiRouteSelector).tryWriteH1AckPriorityWithCarrierPreference(ack, TransportTypeH1); !ok {
			MessagePoolReturn(ack)
			t.Fatal("independent acknowledgement admission failed")
		}
		if err := fixture.peer.WriteMessage(websocket.BinaryMessage, bytes.Repeat([]byte{7}, 16)); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if h1RetainedTestByteCount(&owner.priority) != ackCost || h1RetainedTestByteCount(&owner.control) == 0 || h1RetainedTestByteCount(&owner.send) != cost {
			t.Fatal("physical data/control/acknowledgement ownership was not simultaneous")
		}
		if err := fixture.transport.CloseAndWait(context.Background()); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		for _, lane := range []*platformH1RetainedLane{&owner.send, &owner.receive, &owner.control, &owner.priority} {
			if lane.usedByteCount != 0 {
				t.Fatal("joined writer cancellation retained root credit")
			}
		}
	})
}

// A physical owner must join with the Client still alive while the exact
// received Pack waits for bytes. The rejected unacknowledged Pack can then
// arrive on a new physical generation without a hole or duplicate delivery.
func TestH1ReceiveCarrierCloseJoinsLiveClientAndReplacement(t *testing.T) {
	resetH1UpgradeTestState(t)
	MessagePoolReturn(MessagePoolGet(1))
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		settings := DefaultClientSettings()
		settings.Log = NewNoopLogger()
		settings.EncryptionSettings.Mode = EncryptionModeOff
		settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
		receive := settings.ReceiveBufferSettings
		receive.ProtocolVersion = 2
		receive.AckCompressTimeout = 0
		receive.IdleTimeout = time.Hour
		receive.PackQueueBudget = NewTransferMemoryBudget(kib(64))
		receive.ReceiveQueueBudget = NewTransferMemoryBudget(kib(64))
		receive.PackQueueRetainedByteAccounting = true
		receive.ReceiveQueueRetainedByteAccounting = true
		packBudget := receive.PackQueueBudget
		if !packBudget.TryReserve(kib(64)) {
			t.Fatal("could not hold the actual Pack admission root")
		}
		fillerHeld := true
		client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
		peerId, sequenceId, messageId := NewId(), NewId(), NewId()
		client.ContractManager().AddNoContractPeer(peerId)
		delivered := make(chan byte, 2)
		client.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, _ Peer) {
			for _, frame := range frames {
				if len(frame.MessageBytes) != 0 {
					delivered <- frame.MessageBytes[0]
				}
			}
		})
		t.Cleanup(func() {
			cancel()
			if fillerHeld {
				packBudget.Release(kib(64))
			}
			if err := client.CloseAndWait(context.Background()); err != nil {
				t.Error(err)
			}
			for _, budget := range []*TransferMemoryBudget{packBudget, receive.ReceiveQueueBudget} {
				if stats := budget.Stats(); stats.UsedByteCount != 0 {
					t.Errorf("joined Client retained receive bytes: %+v", stats)
				}
			}
		})
		configure := func(settings *PlatformTransportSettings) {
			ApplyMobilePlatformTransportMemoryPolicy(settings, mib(32))
			settings.H1AckPriorityBufferSize = 1
		}
		fixture := newH1LivenessFixtureWithRoutes(t, false, 0, client.RouteManager(), configure)
		wire, err := ProtoMarshal(&protocol.TransferFrame{
			TransferPath: sendTransferPath(peerId, DestinationId(client.ClientId())).ToProtobuf(),
			Pack: &protocol.Pack{MessageId: messageId.Bytes(), SequenceId: sequenceId.Bytes(), SequenceNumber: 0, Head: true,
				Frames: []*protocol.Frame{{MessageType: protocol.MessageType_IpIpPacketToProvider, MessageBytes: []byte{91}}}},
		})
		if err != nil {
			t.Fatal(err)
		}
		defer MessagePoolReturn(wire)
		if err := fixture.peer.WriteMessage(websocket.BinaryMessage, wire); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if client.ReceiveStats().PackHandoffWaitCount != 1 || h1RetainedTestByteCount(&fixture.transport.h1RetainedBudget.receive) == 0 ||
			packBudget.Stats().UsedByteCount != kib(64) || len(delivered) != 0 || fixture.payloads.Load() != 0 {
			t.Fatal("actual Client did not hold an unacknowledged root at full Pack admission")
		}
		client.receiveBuffer.mutex.Lock()
		for _, sequence := range client.receiveBuffer.receiveSequences {
			if sequence.receiveBufferSettings.PackQueueBudget != packBudget {
				t.Error("receive sequence bypassed the held budget")
			}
		}
		client.receiveBuffer.mutex.Unlock()
		joined := make(chan error, 1)
		go func() { joined <- fixture.transport.CloseAndWait(context.Background()) }()
		synctest.Wait()
		select {
		case err := <-joined:
			if err != nil {
				t.Fatal(err)
			}
		default:
			// Reconcile a broken implementation before reporting the semantic
			// failure, rather than leaving its transport joined on a timer.
			cancel()
			<-joined
			t.Fatal("CARRIER_CLOSE_WAITED_FOR_CLIENT_CANCEL")
		}
		if client.ctx.Err() != nil || len(delivered) != 0 || fixture.payloads.Load() != 0 ||
			fixture.transport.h1RetainedBudget.receive.usedByteCount != 0 || fixture.transport.settings.PlatformTransportBudget.Stats().UsedByteCount != 0 {
			t.Fatal("retiring one lane canceled the Client, acknowledged refused work, or retained credit")
		}
		packBudget.Release(kib(64))
		fillerHeld = false
		replacement := newH1LivenessFixtureWithRoutes(t, false, 0, client.RouteManager(), configure)
		if err := replacement.peer.WriteMessage(websocket.BinaryMessage, wire); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		select {
		case marker := <-delivered:
			if marker != 91 {
				t.Fatal("replacement changed payload")
			}
		default:
			t.Fatal("new physical generation did not deliver the original unacknowledged Pack")
		}
		acks := 0
		for _, payload := range replacement.payloadMessagesSnapshot() {
			var frame protocol.TransferFrame
			if err := ProtoUnmarshal(payload, &frame); err != nil {
				t.Fatal(err)
			}
			if frame.Ack == nil && frame.Frame != nil && frame.Frame.MessageType == protocol.MessageType_TransferAck {
				frame.Ack = &protocol.Ack{}
				if err := ProtoUnmarshal(frame.Frame.MessageBytes, frame.Ack); err != nil {
					t.Fatal(err)
				}
			}
			if frame.Ack != nil && bytes.Equal(frame.Ack.MessageId, messageId.Bytes()) {
				acks++
			}
		}
		if acks != 1 || len(delivered) != 0 {
			t.Fatalf("replacement completion: acks=%d extra payloads=%d", acks, len(delivered))
		}
		if err := replacement.transport.CloseAndWait(context.Background()); err != nil {
			t.Fatal(err)
		}
		if client.ctx.Err() != nil {
			t.Fatal("replacement physical close canceled the logical Client")
		}
	})
}

// Both count and byte pressure cancel the one reader hold, including an
// unbuffered route. No extra packet goroutine survives the owner barrier.
func TestPlatformH1RetainedReaderCancellation(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		for _, count := range []int{0, 1} {
			settings := DefaultPlatformTransportSettingsWithMemoryTarget(mib(32))
			settings.TransportBufferSize = count
			owner := newPlatformH1RetainedBudget(settings)
			transport := &PlatformTransport{receiveStats: &PlatformTransportReceiveStats{}}
			done := make(chan struct{})
			route := make(Route, count)
			progress := make(chan bool, 1)
			go func() {
				progress <- transport.offerH1Retained(done, route, MessagePoolGet(16*1024), &owner.receive, false)
			}()
			synctest.Wait()
			select {
			case message := <-route:
				returnH1RetainedMessage(message, &owner.receive)
			default:
				close(done)
				<-progress
				t.Fatal("empty narrow route cannot deliver a valid maximum frame")
			}
			if !<-progress {
				t.Fatal("valid narrow-lane handoff failed")
			}
			before := transport.receiveStats.Snapshot().H1.QueueBackpressureMessageCount
			if count != 0 {
				first := MessagePoolGet(16 * 1024)
				if !transport.offerH1Retained(done, route, first, &owner.receive, false) {
					t.Fatal("empty valid receive lane refused")
				}
			}
			result := make(chan bool, 1)
			go func() {
				result <- transport.offerH1Retained(done, route, MessagePoolGet(16*1024), &owner.receive, false)
			}()
			synctest.Wait()
			if transport.receiveStats.Snapshot().H1.QueueBackpressureMessageCount != before+1 {
				t.Fatal("reader did not reach actual pressure")
			}
			close(done)
			synctest.Wait()
			if <-result {
				t.Fatal("canceled reader admitted a held root")
			}
			if count != 0 {
				returnH1RetainedMessage(<-route, &owner.receive)
			}
			if owner.receive.usedByteCount != 0 {
				t.Fatal("reader cancellation retained bytes")
			}
		}
	})
}
