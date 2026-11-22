// A fresh tuple can inherit an established provider before its first SYN.
// Raw protocol pressure must not make that placement depend on a native RTO.
package connect

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// Exposes the actual refused opening to narrowly ordered lifetime controls.
type inheritedSynOpeningFixture struct {
	harness               *reraceHarness
	selected              *multiClientChannel
	donor, update         *multiClientChannelUpdate
	donorPath, path       *IpPath
	selection             *multiClientAffinitySelection
	source                *SendSequence
	budget                *TransferMemoryBudget
	release               func()
	nativeOffers, retries *atomic.Int64
	before                time.Time
}

// Real provider traffic supplies positive donor health. The only held worker
// is an already-admitted ACK on the same source as the new inherited flow.
func testNativeFirstSynInheritedAffinityOpening(t *testing.T, configure func(*ClientSettings), inspect func(*testing.T, *inheritedSynOpeningFixture)) {
	t.Helper()
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		entered, release := make(chan *SendSequence, 1), make(chan struct{})
		var releaseOnce sync.Once
		finish := func() { releaseOnce.Do(func() { close(release) }) }
		defer finish()
		var armed atomic.Bool
		var refusals, otherErrors atomic.Int64
		budget := NewTransferMemoryBudget(1024 * 1024)
		harness := newSilenceInferenceHarness(t, []reraceExitSpec{{
			name: "a", answer: reraceAlways(reraceAccept),
			clientSettings: func(settings *ClientSettings) {
				settings.SendBufferSettings.SequenceBufferSize = 1
				settings.SendBufferSettings.ResendQueueBudget = budget
				settings.SendBufferSettings.ResendQueueRetainedByteAccounting = true
				settings.SendBufferSettings.beforeGroupDequeueForTest = func(source *SendSequence) {
					if armed.CompareAndSwap(true, false) {
						entered <- source
						select {
						case <-release:
						case <-source.ctx.Done():
						}
					}
				}
				settings.SendBufferSettings.SendPackLifecycleObserver = func(event SendPackLifecycleObservation) {
					if event.Phase != SendPackLifecyclePhaseTerminal || event.Err == nil {
						return
					}
					var admission *SendPackAdmissionError
					if errors.As(event.Err, &admission) && admission.Boundary == "pack-admission" && admission.Timeout == 0 {
						refusals.Add(1)
					} else {
						otherErrors.Add(1)
					}
				}
				if configure != nil {
					configure(settings)
				}
			},
		}}, nil)
		parent, selected := harness.multiClient, harness.channel("a")
		donorPath := reraceFlow(4, 0, 51501)
		if !harness.syn(donorPath) {
			t.Fatal("healthy donor SYN was not admitted")
		}
		donor := harness.update(donorPath)
		if _, ok := harness.synAck(donorPath); !ok || donor == nil || !donor.receivedInbound.Load() ||
			donor.client.Load() != selected || !selected.positiveAffinityHealth() || !selected.hasActiveTransport() {
			t.Fatal("actual donor did not establish on a healthy live provider")
		}
		selection := donor.client.loadSelection()
		path := reraceFlow(4, 0, 51502)
		if !parent.settings.DestinationAffinity || parent.reliabilitySettings().AffinityMaxTime <= 0 || harness.update(path) != nil {
			t.Fatal("new tuple must begin absent with ordinary destination affinity enabled")
		}
		var nativeOffers, retryOffers atomic.Int64
		parent.beforePacketGroupGateForTest = func(update *multiClientChannelUpdate, current *multiClientChannel, group *parsedPacketGroup) {
			if !reraceSameFlow(group.ipPath, path) {
				return
			}
			if group.isSynRetry() {
				retryOffers.Add(1)
			} else {
				nativeOffers.Add(1)
			}
		}
		armed.Store(true)
		if !synRetryControlOffer(t, harness, reraceTcpPacket(donorPath, tcpFlagAck, donorPath.SequenceNumber+1, 900000, nil)) {
			t.Fatal("healthy donor ACK did not acquire the sole protocol slot")
		}
		var source *SendSequence
		select {
		case source = <-entered:
		default:
			t.Fatal("admitted donor ACK did not reach the owned source barrier")
		}
		source.protocolAdmission.mutex.Lock()
		count, capacity := source.protocolAdmission.count, source.protocolAdmission.capacity
		source.protocolAdmission.mutex.Unlock()
		if count != 1 || capacity != 1 || budget.Available() < 512*1024 {
			t.Fatal("premise lacks exact raw pressure with ample retained backing")
		}
		refusals.Store(0)
		otherErrors.Store(0)
		before := time.Now()
		packet := reraceTcpPacket(path, tcpFlagSyn, path.SequenceNumber, 0, nil)
		if synRetryControlOffer(t, harness, packet) {
			t.Fatal("full raw protocol source unexpectedly admitted the first native SYN")
		}
		update := harness.update(path)
		if update == nil || update == donor || update.client.loadSelection() != selection || update.receivedInbound.Load() ||
			update.synGenerationSeen || !update.synWaitStart.IsZero() || nativeOffers.Load() != 1 || refusals.Load() != 1 || otherErrors.Load() != 0 {
			t.Fatal("first refusal did not traverse actual fresh-tuple inheritance without materialization")
		}
		if inspect != nil {
			inspect(t, &inheritedSynOpeningFixture{
				harness: harness, selected: selected, donor: donor, update: update,
				donorPath: donorPath, path: path, selection: selection,
				source: source, budget: budget, release: finish,
				nativeOffers: &nativeOffers, retries: &retryOffers, before: before,
			})
			return
		}
		finish()
		harness.settle()
		if time.Since(before) != 0 || donor.client.loadSelection() != selection || !donor.receivedInbound.Load() || selected.IsDone() {
			t.Fatal("raw release advanced the source clock or damaged its established donor")
		}
		if _, ok := harness.synAck(path); ok {
			if len(harness.exit("a").syns(path)) != 1 || nativeOffers.Load() != 1 || retryOffers.Load() != 1 ||
				update.client.loadSelection() != selection || budget.UsedByteCount() != 0 {
				t.Fatal("automatic inherited opening duplicated admission, changed selection or retained backing")
			}
			return
		}
		if len(harness.exit("a").syns(path)) != 0 || retryOffers.Load() != 0 || update.synRetry != nil {
			t.Fatalf("missing inherited SYN was not the isolated unretained raw refusal: provider_SYN=%d retry_offers=%d cache_present=%t budget_used=%d", len(harness.exit("a").syns(path)), retryOffers.Load(), update.synRetry != nil, budget.UsedByteCount())
		}
		if !synRetryControlOffer(t, harness, reraceTcpPacket(path, tcpFlagSyn, path.SequenceNumber, 0, nil)) {
			t.Fatal("open-source explicit native retry control was refused")
		}
		if _, ok := harness.synAck(path); !ok || len(harness.exit("a").syns(path)) != 1 || time.Since(before) != 0 {
			t.Fatal("explicit native retry did not immediately establish the same inherited provider")
		}
		t.Fatal("INHERITED_FIRST_SYN_PROTOCOL_OPENING_LOST: fresh tuple inherited healthy provider; first native offer refused at raw capacity; released source required a second native offer")
	})
}

// A new tuple's real inherited selection must recover at capacity release,
// before any native retransmission or TCP timer supplies another opportunity.
func TestNativeFirstSynInheritedAffinityOpeningReoffersWithoutSourceTimer(t *testing.T) {
	testNativeFirstSynInheritedAffinityOpening(t, nil, nil)
}
