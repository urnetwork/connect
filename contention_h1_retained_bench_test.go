// Selector microbenchmarks compare the same explicit Connect memory profiles
// with and without h1 lane accounting. Timed work includes balanced pooled
// shares and synchronous channel handoffs, not sockets, workers, or throughput.
// The cancelled/full-route case measures failure-path allocations only. Run
// with -benchmem; device and integrated performance qualification remain separate.
package connect

import (
	"context"
	"fmt"
	"testing"
)

// One serial fixture owns all routes, graph claims, and pooled shares. No
// background consumer can hide an unbalanced credit or an unfinished handoff.
type h1RetainedSelectorBenchmark struct {
	ctx              context.Context
	selector         *MultiRouteSelector
	frame            []byte
	routes           []Route
	propertiesKVs    map[Route]TransferCarrierProperties
	priorityRouteKVs map[Route]Route
}

// Each operation includes a ready selector write and the exact consumer return.
func BenchmarkRouteSelectorH1RetainedReadyWrite(b *testing.B) {
	benchmarkH1RetainedSelectorCases(b, false, false, func(b *testing.B, fixture *h1RetainedSelectorBenchmark) {
		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			message := MessagePoolShareReadOnly(fixture.frame)
			success, disposition, err := fixture.selector.writeDetailedWithCarrier(fixture.ctx, message, -1)
			if !success || err != nil {
				if !success {
					MessagePoolReturn(message)
				}
				b.Fatalf("ready write: success=%t err=%v", success, err)
			}
			fixture.consumeWrite(b, disposition.route, fixture.propertiesKVs[disposition.route].h1SendRetained)
		}
		b.StopTimer()
	})
}

// Each operation prepays and queues one share, then exercises public Read's
// receive-credit settlement and returns that share. Other active routes stay empty.
func BenchmarkRouteSelectorH1RetainedReadyRead(b *testing.B) {
	benchmarkH1RetainedSelectorCases(b, true, false, func(b *testing.B, fixture *h1RetainedSelectorBenchmark) {
		route := fixture.routes[0]
		lane := fixture.propertiesKVs[route].h1ReceiveRetained
		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			message := MessagePoolShareReadOnly(fixture.frame)
			if !lane.tryReserve(ByteCount(cap(message))) {
				MessagePoolReturn(message)
				b.Fatal("empty receive route has no byte credit")
			}
			select {
			case route <- message:
			default:
				returnH1RetainedMessage(message, lane)
				b.Fatal("receive route was not drained by the previous read")
			}
			received, err := fixture.selector.Read(fixture.ctx, 0)
			if err != nil || received == nil {
				b.Fatalf("ready read: message=%t err=%v", received != nil, err)
			}
			matches := len(received) == len(fixture.frame) && &received[0] == &fixture.frame[0]
			MessagePoolReturn(received)
			if !matches {
				b.Fatal("ready read changed the owned share")
			}
		}
		b.StopTimer()
	})
}

// Every ordinary route is full before timing. Only the already-cancelled
// caller can win the blocking select; negative timeout creates no timer.
func BenchmarkRouteSelectorH1RetainedFullWriteCallerCanceled(b *testing.B) {
	benchmarkH1RetainedSelectorCases(b, false, false, func(b *testing.B, fixture *h1RetainedSelectorBenchmark) {
		fixture.fillOrdinaryRoutes(b)
		ctx, cancel := context.WithCancel(fixture.ctx)
		cancel()
		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			message := MessagePoolShareReadOnly(fixture.frame)
			success, disposition, err := fixture.selector.writeDetailedWithCarrier(ctx, message, -1)
			if !success {
				MessagePoolReturn(message)
			}
			if success || err == nil || !disposition.failureInitiallyBlocked ||
				disposition.failureKind != transferWriteFailureCallerCanceled {
				b.Fatalf("full-route cancellation: success=%t kind=%d blocked=%t err=%v",
					success, disposition.failureKind, disposition.failureInitiallyBlocked, err)
			}
		}
		b.StopTimer()
	})
}

// The eight-slot companion is an explicit synthetic opt-in, not the separately
// scaled sdk mobile default. Full ordinary routes cannot consume its allowance.
func BenchmarkRouteSelectorH1RetainedPriorityReadySynthetic8(b *testing.B) {
	benchmarkH1RetainedSelectorCases(b, false, true, func(b *testing.B, fixture *h1RetainedSelectorBenchmark) {
		fixture.fillOrdinaryRoutes(b)
		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			message := MessagePoolShareReadOnly(fixture.frame)
			success, disposition := fixture.selector.tryWriteH1AckPriorityWithCarrierPreference(message, TransportTypeH1)
			if !success {
				MessagePoolReturn(message)
				b.Fatal("ready priority route refused behind full ordinary routes")
			}
			fixture.consumeWrite(b, fixture.priorityRouteKVs[disposition.route], fixture.propertiesKVs[disposition.route].h1PriorityRetained)
		}
		b.StopTimer()
	})
}

// Bind each leaf to the real target constructor, policy, graph-claim helper,
// and lane-owner helper. The control changes only the private accounting flag.
func benchmarkH1RetainedSelectorCases(b *testing.B, receive bool, priority bool, run func(*testing.B, *h1RetainedSelectorBenchmark)) {
	for _, target := range []ByteCount{32 * 1024 * 1024, 64 * 1024 * 1024} {
		for _, accounted := range []bool{false, true} {
			mode := "unaccounted_control"
			if accounted {
				mode = "accounted"
			}
			for routeCount := 1; routeCount <= 3; routeCount++ {
				b.Run(fmt.Sprintf("target_%dMiB/%s/routes_%d", target/(1024*1024), mode, routeCount), func(b *testing.B) {
					settings := DefaultPlatformTransportSettingsWithMemoryTarget(target)
					ApplyMobilePlatformTransportMemoryPolicy(settings, target)
					if !settings.h1RetainedByteAccounting || settings.TransportBufferSize != 32 ||
						settings.H1MaxMessageByteCount != 16*1024 || settings.H1AckPriorityBufferSize != 0 {
						b.Fatal("explicit Connect profile changed; requalify the benchmark setup")
					}
					if priority {
						settings.H1AckPriorityBufferSize = 8
					}
					settings.h1RetainedByteAccounting = accounted
					claimByteCount := settings.H1MemoryReservationByteCount()
					if claimByteCount <= 0 || accounted && claimByteCount <= settings.H1BudgetByteCount ||
						!accounted && claimByteCount != settings.H1BudgetByteCount {
						b.Fatal("graph claim does not match the selected accounting mode")
					}
					MessagePoolReturn(MessagePoolGet(1200))
					baselineCount, baselineBytes := messagePoolOutstandingSnapshot()
					baselineViolations := MessagePoolViolationCount()
					baselinePriorityRoutes := h1AckPriorityRouteCount.Load()
					ctx, cancel := context.WithCancel(context.Background())
					fixture := &h1RetainedSelectorBenchmark{
						ctx:              ctx,
						selector:         NewMultiRouteSelector(ctx, "synthetic-h1-selector", NewNoopLogger(), DestinationId(NewId()), !receive),
						propertiesKVs:    map[Route]TransferCarrierProperties{},
						priorityRouteKVs: map[Route]Route{},
					}
					var owners []*platformH1RetainedBudget
					var claims []*platformTransportBudgetReservation
					b.Cleanup(func() {
						cancel()
						fixture.selector.Close()
						if fixture.selector.readTimer != nil || fixture.selector.writeTimer != nil {
							b.Error("timer entered a no-timer selector benchmark")
						}
						drain := func(route Route, lane *platformH1RetainedLane) {
							for len(route) > 0 {
								returnH1RetainedMessage(<-route, lane)
							}
						}
						for _, route := range fixture.routes {
							properties := fixture.propertiesKVs[route]
							lane := properties.h1SendRetained
							if receive {
								lane = properties.h1ReceiveRetained
							}
							drain(route, lane)
							if priorityRoute := fixture.priorityRouteKVs[route]; priorityRoute != nil {
								unregisterH1AckPriorityRoute(route)
								drain(priorityRoute, properties.h1PriorityRetained)
							}
						}
						for _, owner := range owners {
							if owner != nil {
								for _, lane := range []*platformH1RetainedLane{&owner.send, &owner.receive, &owner.control, &owner.priority} {
									lane.stateLock.Lock()
									used := lane.usedByteCount
									lane.stateLock.Unlock()
									if used != 0 {
										b.Errorf("retained lane leaked %d bytes", used)
									}
								}
							}
						}
						for _, claim := range claims {
							claim.Release()
						}
						stats := settings.PlatformTransportBudget.Stats()
						if stats.UsedByteCount != 0 || stats.UsedTransportCount != 0 || stats.PendingH1Count != 0 ||
							stats.ReservedByteCount != stats.ReleasedByteCount {
							b.Errorf("graph claim did not balance: %+v", stats)
						}
						MessagePoolReturn(fixture.frame)
						if count, bytes := messagePoolOutstandingSnapshot(); count != baselineCount || bytes != baselineBytes {
							b.Errorf("pooled roots did not balance: before=%d/%d after=%d/%d", baselineCount, baselineBytes, count, bytes)
						}
						if MessagePoolViolationCount() != baselineViolations || h1AckPriorityRouteCount.Load() != baselinePriorityRoutes {
							b.Error("pool ownership or priority-route registration did not balance")
						}
					})
					fixture.frame = MessagePoolGet(1200)
					clear(fixture.frame)
					for range routeCount {
						claim := settings.PlatformTransportBudget.register(platformTransportBudgetH1, claimByteCount, true)
						claims = append(claims, claim)
						if !claim.TryAcquire() {
							b.Fatal("target budget cannot admit the selected graph count")
						}
						owner := newPlatformH1RetainedBudget(settings)
						owners = append(owners, owner)
						if (owner != nil) != accounted {
							b.Fatal("lane-owner helper does not match the selected accounting mode")
						}
						properties := TransferCarrierProperties{ReceiveReliability: CarrierReliabilityReliable}
						if owner != nil {
							properties.h1SendRetained = &owner.send
							properties.h1PriorityRetained = &owner.priority
							properties.h1ReceiveRetained = &owner.receive
							properties.h1ReceiveDone = ctx.Done()
							if owner.send.byteLimit != settings.h1RetainedQueueByteCount(settings.TransportBufferSize) ||
								owner.receive.byteLimit != owner.send.byteLimit || owner.control.byteLimit != owner.send.byteLimit {
								b.Fatal("lane ceilings are not derived from the actual profile")
							}
							expectedPriorityBytes := ByteCount(0)
							if priority {
								expectedPriorityBytes = settings.h1RetainedQueueByteCount(settings.H1AckPriorityBufferSize)
							}
							if owner.priority.byteLimit != expectedPriorityBytes {
								b.Fatal("priority lane ceiling differs from the explicit opt-in")
							}
						}
						route := make(Route, settings.TransportBufferSize)
						fixture.routes = append(fixture.routes, route)
						fixture.propertiesKVs[route] = properties
						var transport Transport = NewSendGatewayTransportWithType(TransportTypeH1)
						if receive {
							transport = NewReceiveGatewayTransportWithType(TransportTypeH1)
						}
						fixture.selector.updateTransportWithProperties(transport, []Route{route}, properties)
						if priority {
							priorityRoute := make(Route, settings.H1AckPriorityBufferSize)
							fixture.priorityRouteKVs[route] = priorityRoute
							registerH1AckPriorityRoute(route, priorityRoute)
						}
					}
					stats := settings.PlatformTransportBudget.Stats()
					snapshot := fixture.selector.activeRoutesSnapshot.Load()
					if stats.UsedByteCount != ByteCount(routeCount)*claimByteCount || stats.UsedTransportCount != routeCount ||
						len(snapshot.routes) != routeCount || snapshot.preferDirectRoute != nil {
						b.Fatal("active route count or graph admission differs from the benchmark label")
					}
					for _, route := range fixture.routes {
						actual := snapshot.routeCarrierProperties[route]
						expected := fixture.propertiesKVs[route]
						if actual.h1SendRetained != expected.h1SendRetained || actual.h1ReceiveRetained != expected.h1ReceiveRetained ||
							actual.h1PriorityRetained != expected.h1PriorityRetained {
							b.Fatal("selector does not use the admitted helper's exact lanes")
						}
					}
					b.Logf("Connect profile: target=%d queue=%d priority=%d packet=1200 root=%d claim/route=%d; helpers=H1MemoryReservationByteCount,newPlatformH1RetainedBudget",
						target, settings.TransportBufferSize, settings.H1AckPriorityBufferSize, cap(fixture.frame), claimByteCount)
					run(b, fixture)
				})
			}
		}
	}
}

// Seed complete ordinary queues before timing; each admitted share keeps its
// own conservative lane permission even though the immutable root is shared.
func (self *h1RetainedSelectorBenchmark) fillOrdinaryRoutes(b *testing.B) {
	for _, route := range self.routes {
		lane := self.propertiesKVs[route].h1SendRetained
		for range cap(route) {
			message := MessagePoolShareReadOnly(self.frame)
			if !tryWriteH1RetainedRoute(route, lane, message) {
				MessagePoolReturn(message)
				b.Fatal("profile cannot prefill an ordinary route with 1200-byte shares")
			}
		}
	}
}

// Takes one selector-published share and returns it through the physical
// writer's accounting helper. A missing publication fails instead of waiting.
func (self *h1RetainedSelectorBenchmark) consumeWrite(b *testing.B, route Route, lane *platformH1RetainedLane) {
	select {
	case message := <-route:
		matches := len(message) == len(self.frame) && &message[0] == &self.frame[0]
		returnH1RetainedMessage(message, lane)
		if !matches {
			b.Fatal("ready write changed the owned share")
		}
	default:
		b.Fatal("successful write did not publish its share")
	}
}
