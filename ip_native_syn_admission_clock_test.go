// A refused native SYN has no provider-delivery evidence. Real queue controls
// distinguish that local attempt from an admitted unanswered connection.
package connect

import (
	"net"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// The canonical fixture owns a real retained original and its closed credit
// gate. Only routing lookup is selected by its existing seam; sends, native
// results, collapse admission, source queues and pooled ownership stay real.
type nativeSynAdmissionClockFixture struct {
	credit     *nativeBatchCreditFixture
	path       *IpPath
	update     *multiClientChannelUpdate
	syn        []byte
	admissions atomic.Int64
}

// Register the exact tuple used by clientDialFailure, rather than letting a
// test-only lookup hide that path by leaving the parent's flow map empty.
func newNativeSynAdmissionClockFixture(t *testing.T) *nativeSynAdmissionClockFixture {
	t.Helper()
	self := &nativeSynAdmissionClockFixture{credit: newNativeBatchCreditFixture(t)}
	parent := self.credit.base.parent
	parent.settings.DialFailureRerace = true
	self.path = &IpPath{
		Version:         4,
		Protocol:        IpProtocolTcp,
		SourceIp:        net.IPv4(192, 0, 2, 111),
		SourcePort:      45111,
		DestinationIp:   net.IPv4(198, 51, 100, 112),
		DestinationPort: 443,
		Syn:             true,
		SequenceNumber:  71000,
	}
	self.syn = ipOosTcpPacketSequence(self.path, tcpFlagSyn, self.path.SequenceNumber, nil)
	self.update = newMultiClientChannelUpdate(parent.ctx, self.path)
	self.update.client.Store(self.credit.base.selected)
	key := self.path.ToIp4Path()
	parent.stateLock.Lock()
	if parent.ip4PathUpdates == nil {
		parent.ip4PathUpdates = map[Ip4Path]*multiClientChannelUpdate{}
	}
	parent.ip4PathUpdates[key] = self.update
	if parent.flowUpdates == nil {
		parent.flowUpdates = map[*multiClientChannelUpdate]bool{}
	}
	parent.flowUpdates[self.update] = true
	if parent.clientUpdates[self.credit.base.selected] == nil {
		parent.clientUpdates[self.credit.base.selected] = map[*multiClientChannelUpdate]bool{}
	}
	parent.clientUpdates[self.credit.base.selected][self.update] = true
	parent.stateLock.Unlock()
	previousLookup := parent.sendClientPathForTest
	parent.sendClientPathForTest = func(path *IpPath, pin flowPin, callback func(*multiClientChannelUpdate, *multiClientChannel)) {
		if path.Version == 4 && path.ToIp4Path() == key {
			callback(self.update, self.update.client.Load())
			return
		}
		previousLookup(path, pin, callback)
	}
	settings := self.credit.base.client.sendBuffer.sendBufferSettings
	previousAdmission := settings.afterGroupAdmissionForTest
	settings.afterGroupAdmissionForTest = func(target sendGroupAdmissionTarget) {
		if previousAdmission != nil {
			previousAdmission(target)
		}
		if group, ok := target.(*parsedPacketGroup); ok && group.ipPath != nil &&
			group.ipPath.Version == 4 && group.ipPath.ToIp4Path() == key && group.ipPath.Syn && !group.ipPath.Ack {
			self.admissions.Add(1)
		}
	}
	t.Cleanup(self.update.Close)
	return self
}

// The native batch takes every pooled input. The canonical witnesses prove
// refusal did not leave a retained packet or consume source retry authority.
func (self *nativeSynAdmissionClockFixture) offer(t *testing.T, packet []byte) bool {
	t.Helper()
	accepted := [1]bool{}
	started := time.Now()
	count := self.credit.base.parent.SendNativePacketBatchWithResults(
		self.credit.base.source,
		protocol.ProvideMode_Network,
		self.credit.ownPackets(packet),
		5*time.Second,
		accepted[:],
	)
	if count < 0 || count > 1 || (count == 1) != accepted[0] {
		t.Fatalf("native SYN result count=%d accepted=%t", count, accepted[0])
	}
	if elapsed := time.Since(started); elapsed != 0 {
		t.Fatalf("native SYN waited for %s instead of returning its ready offer", elapsed)
	}
	synctest.Wait()
	return accepted[0]
}

// Read each owner under its own lock; the lookup cache and provider's strike
// history are separate observations, not one aggregated failure counter.
func (self *nativeSynAdmissionClockFixture) failureState(t *testing.T) (serviceFailures uint32, strikes int, reraces uint64, intercepted uint64) {
	t.Helper()
	parent, selected := self.credit.base.parent, self.credit.base.selected
	key, ok := destinationServiceFailureKeyFor(self.path, selected)
	if !ok {
		t.Fatal("native SYN fixture has no exact destination/provider failure key")
	}
	parent.stateLock.Lock()
	serviceFailures = parent.destinationServiceFailures[key].count
	parent.stateLock.Unlock()
	strikes = selected.dialFailureCount()
	metrics := parent.ReliabilityMetrics()
	return serviceFailures, strikes, metrics.FlowsReraced, metrics.DialFailuresIntercepted
}

// The first SYN and both retransmissions meet a real closed source-credit
// gate. Advancing virtual protocol time must not convert those three local
// refusals into evidence that the provider failed an upstream connection.
func TestNativeSynAdmissionClockLocallyRefusedDoesNotBlameProvider(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newNativeSynAdmissionClockFixture(t)
		selected := f.credit.base.selected
		selected.stateLock.Lock()
		priorCount, priorBytes, priorPending := selected.packetStats.sendNackCount,
			selected.packetStats.sendNackByteCount, selected.pendingSendTime
		selected.stateLock.Unlock()
		started := time.Now()
		for _, offset := range []time.Duration{0, time.Second, inferredDialFailureTimeout + time.Nanosecond} {
			if remaining := started.Add(offset).Sub(time.Now()); remaining > 0 {
				// Virtual time crosses the unchanged inference bar; it is not a
				// scheduler-dependent wait for an asynchronous observation.
				time.Sleep(remaining)
			}
			if f.offer(t, f.syn) {
				t.Fatalf("closed real credit gate admitted the fresh SYN at %s", offset)
			}
		}
		if f.admissions.Load() != 0 || len(f.credit.base.route) != 0 {
			t.Fatalf("refusal premise lost: SYN admissions=%d route frames=%d", f.admissions.Load(), len(f.credit.base.route))
		}
		if count, size := f.credit.source.resendQueue.QueueSize(); count != 1 || size <= 1 || !f.credit.source.resendCapacityUnavailable.Load() {
			t.Fatal("local SYN offers changed the unrelated retained original or opened its real capacity gate")
		}
		selected.stateLock.Lock()
		retainedUnchanged := selected.packetStats.sendNackCount == priorCount &&
			selected.packetStats.sendNackByteCount == priorBytes && selected.pendingSendTime == priorPending && selected.endErr == nil
		selected.stateLock.Unlock()
		if !retainedUnchanged || f.credit.base.update.client.Load() != selected {
			t.Fatal("refused fresh SYN disturbed the existing reliable owner")
		}
		f.update.stateLock.Lock()
		unadmitted := !f.update.synGenerationSeen && f.update.sequenceClaims == nil && !f.update.receivedInbound.Load()
		f.update.stateLock.Unlock()
		if !unadmitted {
			t.Fatal("refused SYN acquired generation, collapse or response authority")
		}
		serviceFailures, strikes, reraces, intercepted := f.failureState(t)
		if serviceFailures != 0 || strikes != 0 || reraces != 0 || intercepted != 0 || f.update.client.Load() != selected {
			t.Errorf("OLD_NATIVE_SYN_REFUSAL_PROVIDER_ATTRIBUTION: admitted=0 written=0 service_failures=%d dial_strikes=%d reraces=%d intercepted=%d binding_preserved=%t",
				serviceFailures, strikes, reraces, intercepted, f.update.client.Load() == selected)
		}
	})
}

// An actually admitted and delivered SYN still earns the existing silence
// inference. Only its inner response is silent; the healthy sibling, real
// provider queues and public native retransmission remain in the path.
func TestNativeSynAdmissionClockAcceptedSilentStillReraces(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		harness := newSilenceInferenceHarness(t, []reraceExitSpec{
			{name: "a", tier: 0, answer: reraceAlways(reraceSilent)},
			{name: "b", tier: 1},
		}, nil)
		path := reraceFlow(4, 0, 45112)
		offer := func() bool {
			packet := MessagePoolCopy(reraceTcpPacket(path, tcpFlagSyn, path.SequenceNumber, 0, nil))
			accepted := [1]bool{}
			count := harness.multiClient.SendNativePacketBatchWithResults(
				harness.source, protocol.ProvideMode_Network, [][]byte{packet}, reraceSendTimeout, accepted[:])
			harness.settle()
			if count != 0 && count != 1 || (count == 1) != accepted[0] {
				t.Fatalf("accepted-silent native result count=%d accepted=%t", count, accepted[0])
			}
			return accepted[0]
		}
		started := time.Now()
		if !offer() || harness.boundTo(path) != harness.channel("a") || len(harness.exit("a").syns(path)) != 1 {
			t.Fatal("positive control lacks one actual SYN admission delivered to healthy A")
		}
		update := harness.update(path)
		if update == nil || update.receivedInbound.Load() {
			t.Fatal("positive control unexpectedly received an inner response")
		}
		update.stateLock.Lock()
		acceptedClock, acceptedClient := update.synWaitStart, update.synWaitClient
		update.stateLock.Unlock()
		if acceptedClock.IsZero() || acceptedClient != harness.channel("a") {
			t.Fatal("actual accepted SYN did not arm its provider-specific silence clock")
		}
		harness.sleep(started.Add(time.Second).Sub(time.Now()))
		requireSilenceInferenceAffinity(t, harness, path)
		if offer() || len(harness.exit("a").syns(path)) != 1 {
			t.Fatal("covered native retransmission escaped its admitted original's collapse ownership")
		}
		update.stateLock.Lock()
		preservedClock := update.synWaitStart.Equal(acceptedClock) && update.synWaitClient == acceptedClient
		update.stateLock.Unlock()
		if !preservedClock || harness.metrics().FlowsReraced != 0 || harness.channel("a").dialFailureCount() != 0 {
			t.Fatal("locally refused retransmission cleared or renewed a previously accepted SYN's valid clock")
		}
		harness.sleep(started.Add(inferredDialFailureTimeout + time.Nanosecond).Sub(time.Now()))
		requireSilenceInferenceAffinity(t, harness, path)
		if !offer() {
			t.Fatal("admitted-silent SYN's native retransmission did not reach a fresh candidate")
		}
		metrics := harness.metrics()
		if metrics.FlowsReraced != 1 || metrics.DialFailuresIntercepted != 0 || harness.channel("a").dialFailureCount() != 1 {
			t.Fatalf("accepted silence lost inference: reraces=%d intercepted=%d A_strikes=%d",
				metrics.FlowsReraced, metrics.DialFailuresIntercepted, harness.channel("a").dialFailureCount())
		}
		if _, ok := harness.synAck(path); !ok || harness.boundTo(path) != harness.channel("b") || harness.update(path) != update {
			t.Fatal("accepted-silent control did not deliver B's real SYN-ACK on the original flow owner")
		}
		reraceAssertNoErrorDelivered(t, harness, path)
	})
}

// A source retry admitted after the real gate opens gets the new admission's
// clock. The preceding refused offer must neither consume its SYN generation
// nor prevent reliable wire progress on the same selected provider.
func TestNativeSynAdmissionClockCapacityOpenAdmitsOriginal(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newNativeSynAdmissionClockFixture(t)
		if f.offer(t, f.syn) || f.admissions.Load() != 0 || len(f.credit.base.route) != 0 {
			t.Fatal("capacity-open control did not begin with a genuinely refused native SYN")
		}
		time.Sleep(time.Second)
		f.credit.releasePeerCredit()
		acceptedAt := time.Now()
		if !f.offer(t, f.syn) || f.admissions.Load() != 1 {
			t.Fatal("capacity-open source retry lost the original SYN's native admission authority")
		}
		f.credit.finishPackets(f.syn)
		f.update.stateLock.Lock()
		clock := f.update.synWaitStart
		admitted := f.update.synGenerationSeen && f.update.synGenerationNumber == f.path.SequenceNumber &&
			f.update.synWaitClient == f.credit.base.selected && !f.update.receivedInbound.Load()
		f.update.stateLock.Unlock()
		if !admitted || clock.Before(acceptedAt) {
			t.Fatalf("successful source retry inherited refused-offer state: admitted=%t clock=%s accepted_at=%s", admitted, clock, acceptedAt)
		}
		serviceFailures, strikes, reraces, intercepted := f.failureState(t)
		if serviceFailures != 0 || strikes != 0 || reraces != 0 || intercepted != 0 || f.update.client.Load() != f.credit.base.selected {
			t.Fatal("capacity opening fabricated a provider failure or changed the selected flow")
		}
	})
}
