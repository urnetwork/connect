// Native SYN retry must observe the exact bounded protocol admission source.
// A real pre-Run barrier holds three pure ACK groups; no budget, capacity flag,
// sequence number, retry deadline or retained packet state is manufactured.
package connect

import (
	"bytes"
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// A pure SYN refused by a full protocol count gate must be reoffered once when
// its actual source drains, without waiting for a new native TCP transmission.
func TestNativeSynRetryProtocolSlotReleaseReoffersRetainedOriginal(t *testing.T) {
	testNativeSynRetryProtocolReadiness(t, false)
}

// Canceling the original parent retires its cached SYN and every ready-edge
// reference before the independent healthy source opens its protocol slots.
func TestNativeSynRetryProtocolSlotCancellationRetiresOriginal(t *testing.T) {
	testNativeSynRetryProtocolReadiness(t, true)
}

// Production source and parent workers are joined before pooled witnesses are
// checked. Quiescence, not a short negative timeout, proves the missing wake.
func testNativeSynRetryProtocolReadiness(t *testing.T, cancelBeforeReady bool) {
	t.Helper()
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		var packRefusals, otherRefusals atomic.Int32
		fixture := newGroupDispositionQueueFixture(t, nil, func(settings *ClientSettings) {
			settings.SendBufferSettings.SequenceBufferSize = 3
			previous := settings.SendBufferSettings.SendPackLifecycleObserver
			settings.SendBufferSettings.SendPackLifecycleObserver = func(event SendPackLifecycleObservation) {
				previous(event)
				if event.Phase != SendPackLifecyclePhaseTerminal || event.Err == nil {
					return
				}
				var admissionErr *SendPackAdmissionError
				if errors.As(event.Err, &admissionErr) && admissionErr.Boundary == "pack-admission" && admissionErr.Timeout == 0 {
					packRefusals.Add(1)
				} else {
					otherRefusals.Add(1)
				}
			}
		})
		fixture.selected.settings = fixture.parent.settings
		acks := make([][]byte, 3)
		for index := range acks {
			acks[index] = groupRecoveryPacket(100, 500, uint16(64+index), tcpFlagAck, nil)
			if !protocolClassPureTcpControl(acks[index]) || fixture.offer(acks[index]) != 1 {
				t.Fatalf("premise: pure ACK group %d did not enter its real source", index)
			}
		}
		fixture.requireParked()
		group := fixture.update.sequenceClaims
		if group == nil || group.collapseSource == nil {
			t.Fatal("premise: public ACK admission did not publish its source")
		}
		source := group.collapseSource
		admission := source.protocolAdmission
		admission.mutex.Lock()
		count, capacity, waiting := admission.count, admission.capacity, len(admission.waiters)
		admission.mutex.Unlock()
		source.packAdmission.mutex.Lock()
		paidCount := source.packAdmission.count
		source.packAdmission.mutex.Unlock()
		if count != capacity || count != len(acks) || waiting != 0 || paidCount != 0 ||
			source.resendCapacityUnavailable.Load() || source.resendQueue.Len() != 0 || len(source.packs) != len(acks) {
			t.Fatalf("premise: protocol-only count pressure missing: protocol=%d/%d waiters=%d paid=%d retained=%d",
				count, capacity, waiting, paidCount, source.resendQueue.Len())
		}

		harness, path := synRetryControlHarness(t, 51327, nil, DefaultProtocolVersion)
		var sourceOffers, retryOffers atomic.Int32
		harness.multiClient.beforePacketGroupGateForTest = func(_ *multiClientChannelUpdate, _ *multiClientChannel, group *parsedPacketGroup) {
			if !reraceSameFlow(group.ipPath, path) || !group.ipPath.Syn || group.ipPath.Ack {
				return
			}
			if group.isSynRetry() {
				retryOffers.Add(1)
			} else {
				sourceOffers.Add(1)
			}
		}
		if !privateStaleBoundNativeSyn(t, harness, path) {
			t.Fatal("initial native SYN refused before the replacement source was selected")
		}
		update := harness.update(path)
		owner := synRetryControlOwner(t, update)
		update.stateLock.Lock()
		ownerCredit, ownerSerial := owner.credit, owner.serial
		original := bytes.Clone(owner.packet)
		update.stateLock.Unlock()
		if !protocolClassPureTcpControl(original) || !ownerCredit.noBudget {
			t.Fatal("premise: retained native SYN did not use the actual protocol exclusion")
		}
		started := fixture.started.Load()
		harness.multiClient.groupRaceCandidatesForTest = func(*parsedPacketGroup) []*multiClientChannel {
			return []*multiClientChannel{fixture.selected}
		}
		before := time.Now()
		for range 4 {
			harness.inject("a", reraceProviderSignal(t, path))
		}
		synctest.Wait()
		update.stateLock.Lock()
		quiet := update.synRetry == owner && owner.references == 1 && !owner.queued &&
			update.client.Load() == nil && !update.receivedInbound.Load()
		update.stateLock.Unlock()
		if fixture.started.Load() != started+1 || packRefusals.Load() != 1 || otherRefusals.Load() != 0 ||
			!quiet || sourceOffers.Load() != 1 || retryOffers.Load() != 1 ||
			fixture.selected.dialFailureCount() != 0 || fixture.selected.IsDone() {
			t.Fatalf("premise: retained SYN did not refuse once at actual protocol admission: starts=%d pack_refusals=%d other_refusals=%d quiet=%t source=%d retry=%d",
				fixture.started.Load()-started, packRefusals.Load(), otherRefusals.Load(), quiet, sourceOffers.Load(), retryOffers.Load())
		}
		synctest.Wait()
		if fixture.started.Load() != started+1 || time.Since(before) != 0 {
			t.Fatal("unchanged protocol capacity retried or advanced a native retransmission clock")
		}
		if cancelBeforeReady {
			if err := harness.multiClient.CloseAndWait(context.Background()); err != nil {
				t.Fatal(err)
			}
			synRetryControlReleased(t, owner)
			if !ownerCredit.released.Load() {
				t.Fatal("canceled parent retained its canonical SYN credit")
			}
		}

		// Let the real source write its admitted ACK groups. releaseRaw frees
		// their protocol slots; no capacity release or retry callback is faked.
		fixture.unpark()
		synctest.Wait()
		attempts := fixture.started.Load()
		if cancelBeforeReady {
			fixture.acknowledgeAll(acks...)
			if attempts != started+1 || sourceOffers.Load() != 1 || retryOffers.Load() != 1 {
				t.Fatal("protocol slot release revived a canceled native original")
			}
		} else {
			if attempts == started+1 {
				fixture.acknowledgeAll(acks...)
				admission.mutex.Lock()
				remaining, closed := admission.count, admission.closed
				admission.mutex.Unlock()
				if remaining != 0 || closed {
					t.Fatalf("premise: actual writer did not reopen protocol count capacity: count=%d closed=%t", remaining, closed)
				}
				t.Fatalf("NATIVE_SYN_PROTOCOL_READY_EDGE_LOST: pure SYN remained retained after actual protocol source reopened; retries=%d native_offers=%d elapsed=%s",
					retryOffers.Load(), sourceOffers.Load(), time.Since(before))
			}
			if attempts != started+2 || harness.boundTo(path) != fixture.selected ||
				sourceOffers.Load() != 1 || retryOffers.Load() != 2 {
				t.Fatal("one protocol ready edge did not bind exactly one retained replacement")
			}
			fixture.acknowledgeAll(append(acks, original)...)
			synctest.Wait()
			update.stateLock.Lock()
			balanced := update.synRetry == owner && owner.references == 1 && !owner.queued &&
				owner.credit == ownerCredit && owner.serial == ownerSerial && bytes.Equal(owner.packet, original) &&
				update.client.Load() == fixture.selected && !update.receivedInbound.Load()
			update.stateLock.Unlock()
			if !balanced || ownerCredit.released.Load() || harness.update(path) != update {
				t.Fatal("protocol readiness changed original SYN custody or retained an extra dispatcher reference")
			}
			harness.multiClient.stateLock.Lock()
			booked := harness.multiClient.clientUpdates[fixture.selected][update]
			harness.multiClient.stateLock.Unlock()
			if !booked {
				t.Fatal("admitted retained replacement is missing from real flow bookkeeping")
			}
			if err := harness.multiClient.CloseAndWait(context.Background()); err != nil {
				t.Fatal(err)
			}
			synRetryControlReleased(t, owner)
			if !ownerCredit.released.Load() {
				t.Fatal("joined native flow retained its final SYN credit")
			}
		}
		if fixture.selected.dialFailureCount() != 0 || fixture.selected.IsDone() || time.Since(before) != 0 {
			t.Fatal("count readiness changed provider health or depended on a native retransmission timer")
		}
		t.Logf("NATIVE_SYN_PROTOCOL_READY_EDGE cancel=%t protocol_capacity=%d native_offers=%d retry_offers=%d exact_protocol_refusals=%d elapsed=%s",
			cancelBeforeReady, capacity, sourceOffers.Load(), retryOffers.Load(), packRefusals.Load(), time.Since(before))
	})
}
