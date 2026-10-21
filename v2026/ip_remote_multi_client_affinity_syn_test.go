// Public SYN roots traverse canonical lookup, bounded group admission, actual
// source callbacks and pool ownership. Only the provider candidate list is fixed.
package connect

import (
	"bytes"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// The first candidate enumeration offers one source; later enumerations offer
// both. Individual roots override this policy when a fixed topology is needed.
type affinitySynQueueFixture struct {
	first  *groupDispositionQueueFixture
	second *groupDispositionQueueFixture
	parent *RemoteUserNatMultiClient
	path   *IpPath
	races  int
}

func newAffinitySynQueueFixture(t *testing.T, version int, configureSecond func(*ClientSettings), configureFirst ...func(*ClientSettings)) *affinitySynQueueFixture {
	t.Helper()
	var firstConfig func(*ClientSettings)
	if len(configureFirst) != 0 {
		firstConfig = configureFirst[0]
	}
	fixture := &affinitySynQueueFixture{
		first:  newGroupDispositionQueueFixture(t, nil, firstConfig),
		second: newGroupDispositionQueueFixture(t, nil, configureSecond),
		path:   affinityTimeTestPath(version, 44100),
	}
	fixture.path.SequenceNumber = 1000
	fixture.parent = fixture.first.parent
	parent := fixture.parent
	parent.sendClientPathForTest = nil
	parent.ip4PathUpdates = map[Ip4Path]*multiClientChannelUpdate{}
	parent.ip6PathUpdates = map[Ip6Path]*multiClientChannelUpdate{}
	parent.affinityIp4Paths = map[Ip4Path]map[Ip4Path]time.Time{}
	parent.affinityIp6Paths = map[Ip6Path]map[Ip6Path]time.Time{}
	parent.flowUpdates = map[*multiClientChannelUpdate]bool{}
	for _, channel := range []*multiClientChannel{fixture.first.selected, fixture.second.selected} {
		channel.healthy = true
		channel.args.DestinationStats.IpFamily = IpFamilyDualstack
	}
	parent.groupRaceCandidatesForTest = func(*parsedPacketGroup) []*multiClientChannel {
		fixture.races++
		if fixture.races == 1 {
			return []*multiClientChannel{fixture.first.selected}
		}
		return []*multiClientChannel{fixture.first.selected, fixture.second.selected}
	}
	t.Cleanup(func() {
		for update := range parent.flowUpdates {
			update.Close()
		}
	})
	return fixture
}

// A real cumulative peer ACK joins the initial source owner before aging.
func (self *affinitySynQueueFixture) initial(t *testing.T) *multiClientChannelUpdate {
	t.Helper()
	template := reraceTcpPacket(self.path, tcpFlagSyn, self.path.SequenceNumber, 0, nil)
	if self.first.offer(template) != 1 {
		t.Fatal("initial public SYN was refused")
	}
	self.first.requireParked()
	self.first.unpark()
	self.first.acknowledgeAll(template)
	update := self.parent.receiveUpdate(self.path)
	if update == nil || update.client.Load() != self.first.selected || !update.synGenerationSeen {
		t.Fatal("actual initial admission did not publish its selected SYN generation")
	}
	return update
}

// Ingress enters the real remote receive path with a parsed, checksummed packet.
func (self *affinitySynQueueFixture) receive(t *testing.T, client *multiClientChannel, flags byte, sequence, ack uint32) {
	t.Helper()
	packet := reraceTcpPacket(self.path.Reverse(), flags, sequence, ack, nil)
	path, err := ParseIpPath(packet)
	if err != nil {
		t.Fatal(err)
	}
	self.parent.clientReceivePacket(client, TransferPath{}, affinityRenewalProvideModeNetwork,
		TransportTypeUnknown, path, packet)
}

// Custody-focused roots explicitly choose their available candidate set. The
// ordinary-race roots separately prove that a previous healthy client may win.
func (self *affinitySynQueueFixture) raceOnly(clients ...*multiClientChannel) {
	self.parent.groupRaceCandidatesForTest = func(*parsedPacketGroup) []*multiClientChannel {
		self.races++
		return clients
	}
}

func testAffinityRenewalPublicBoundary(t *testing.T, version int, age time.Duration, cap time.Duration, sameIsn bool) {
	t.Helper()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newAffinitySynQueueFixture(t, version, nil)
		old := f.initial(t)
		f.raceOnly(f.first.selected)
		settings := ReliabilitySettingsFrom(f.parent.settings)
		settings.AffinityMaxTime = cap
		f.parent.SetReliabilitySettings(settings)
		time.Sleep(age)
		sequence := uint32(2000)
		if sameIsn {
			sequence = f.path.SequenceNumber
		}
		template := reraceTcpPacket(f.path, tcpFlagSyn, sequence, 0, nil)
		accepted := f.first.offer(template)
		renew := cap <= 0 || age >= cap
		current := f.parent.receiveUpdate(f.path)
		if renew {
			if accepted != 1 || current == old || !old.IsDone() || current.client.Load() != f.first.selected || f.races != 2 {
				t.Fatalf("v%d age=%v cap=%v sameISN=%v did not admit an ordinary new race", version, age, cap, sameIsn)
			}
			f.first.acknowledgeAll(template)
		} else {
			if current != old || old.IsDone() || current.client.Load() != f.first.selected || f.races != 1 {
				t.Fatal("young healthy binding did not retain its actual provider")
			}
			if !sameIsn {
				if accepted != 1 {
					t.Fatal("young different-ISN SYN was not admitted on its retained provider")
				}
				f.first.acknowledgeAll(template)
			}
		}
		if len(f.parent.affinityTransitions) != 0 || len(f.parent.affinityDeliveries) != 0 {
			t.Fatal("public return retained a transition or executing callback")
		}
	})
}

func TestAffinityRenewalPublicIp4AgeBoundaries(t *testing.T) {
	for _, age := range []time.Duration{0, 59 * time.Second, time.Minute - time.Nanosecond, time.Minute, 61 * time.Second} {
		testAffinityRenewalPublicBoundary(t, 4, age, time.Minute, false)
	}
}

func TestAffinityRenewalPublicIp6AgeBoundaries(t *testing.T) {
	for _, age := range []time.Duration{0, 59 * time.Second, time.Minute, 61 * time.Second} {
		testAffinityRenewalPublicBoundary(t, 6, age, time.Minute, false)
	}
}

func TestAffinityRenewalPublicNonpositiveDisablesSynReuse(t *testing.T) {
	for _, cap := range []time.Duration{0, -time.Nanosecond, -time.Minute} {
		for _, version := range []int{4, 6} {
			testAffinityRenewalPublicBoundary(t, version, 0, cap, true)
		}
	}
}

func TestAffinityRenewalPublicSameIsnUnestablishedBoundary(t *testing.T) {
	for _, age := range []time.Duration{59 * time.Second, time.Minute, 61 * time.Second} {
		testAffinityRenewalPublicBoundary(t, 4, age, time.Minute, true)
	}
}

func TestAffinityRenewalPublicEstablishedReplayAndDataStayStable(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newAffinitySynQueueFixture(t, 4, nil)
		old := f.initial(t)
		f.receive(t, f.first.selected, tcpFlagSyn|tcpFlagAck, 9000, 1001)
		if !old.receivedInbound.Load() {
			t.Fatal("actual SYN-ACK did not establish the incumbent")
		}
		time.Sleep(61 * time.Second)
		f.first.selected.stateLock.Lock()
		f.first.selected.healthy, f.first.selected.warning = false, true
		f.first.selected.stateLock.Unlock()
		f.first.offer(reraceTcpPacket(f.path, tcpFlagSyn, 1000, 0, nil))
		data := reraceTcpPacket(f.path, tcpFlagAck, 1001, 9001, []byte("live"))
		if f.first.offer(data) != 1 {
			t.Fatal("affinity expiry blocked established TCP data")
		}
		f.first.acknowledgeAll(data)
		if f.parent.receiveUpdate(f.path) != old || old.client.Load() != f.first.selected || old.IsDone() || f.races != 1 {
			t.Fatal("expired/unhealthy established replay or data migrated its connection")
		}
	})
}

func TestAffinityRenewalPublicUnhealthySynRaces(t *testing.T) {
	for _, state := range []string{"unknown", "unhealthy", "warning", "quarantined", "closed", "route-gone"} {
		assertMessagePoolOwnership(t)
		synctest.Test(t, func(t *testing.T) {
			f := newAffinitySynQueueFixture(t, 4, nil)
			old := f.initial(t)
			// The ordinary healthy offer contains only the second provider;
			// no historical-source exclusion is used to enforce this premise.
			f.raceOnly(f.second.selected)
			client := f.first.selected
			client.stateLock.Lock()
			switch state {
			case "unknown", "unhealthy":
				client.healthy = false
			case "warning":
				client.warning = true
			case "quarantined":
				client.quarantined = true
			}
			client.stateLock.Unlock()
			if state == "closed" {
				if err := f.first.closeClient(t.Context()); err != nil {
					t.Fatal(err)
				}
			} else if state == "route-gone" {
				f.first.client.RouteManager().UpdateTransport(f.first.transport, nil)
			}
			template := reraceTcpPacket(f.path, tcpFlagSyn, 1000, 0, nil)
			if f.first.offer(template) != 1 {
				t.Fatalf("%s bound SYN did not reach the healthy offered race", state)
			}
			current := f.parent.receiveUpdate(f.path)
			if current == old || current.client.Load() != f.second.selected || !old.IsDone() {
				t.Fatalf("%s was reused as ordinary SYN affinity", state)
			}
			f.second.unpark()
			f.second.acknowledgeAll(template)
		})
	}
}

func TestAffinityRenewalPublicRefusalKeepsOldOwnerAndRetries(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newAffinitySynQueueFixture(t, 4, func(settings *ClientSettings) {
			settings.SendBufferSettings.SequenceBufferSize = 0
		})
		old := f.initial(t)
		f.raceOnly(f.second.selected)
		time.Sleep(time.Minute)
		template := reraceTcpPacket(f.path, tcpFlagSyn, 2000, 0, nil)
		packet := MessagePoolCopy(template)
		f.first.witnesses = append(f.first.witnesses, MessagePoolShareReadOnly(packet))
		callerOwns := true
		defer func() {
			if callerOwns {
				MessagePoolReturn(packet)
			}
		}()
		selection, syn := old.client.loadSelection(), old.synGenerationNumber
		if f.parent.SendPacket(f.first.source, affinityRenewalProvideModeNetwork, packet, 0) {
			callerOwns = false
			t.Fatal("readerless renewal falsely consumed the caller's SYN")
		}
		if f.parent.receiveUpdate(f.path) != old || old.IsDone() || old.client.loadSelection() != selection || old.synGenerationNumber != syn ||
			old.affinitySources.closed || old.affinityDelivery.Load() != 0 || !bytes.Equal(packet, template) || len(f.parent.affinityTransitions) != 0 {
			t.Fatal("all-source refusal mutated the incumbent or retained caller custody")
		}
		f.second.requireParked()
		f.second.unpark()
		synctest.Wait()
		if !f.parent.SendPacket(f.first.source, affinityRenewalProvideModeNetwork, packet, 0) {
			t.Fatal("exact refused caller packet could not retry after source capacity returned")
		}
		callerOwns = false
		f.second.acknowledgeAll(template)
		if current := f.parent.receiveUpdate(f.path); current == old || current.client.Load() != f.second.selected || !old.IsDone() {
			t.Fatal("successful retry did not replace exactly the old generation")
		}
	})
}

func TestAffinityRenewalPublicDequeueCommitsBeforeProducerReturn(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		entered, release := make(chan struct{}), make(chan struct{})
		var releaseOnce sync.Once
		resume := func() { releaseOnce.Do(func() { close(release) }) }
		f := newAffinitySynQueueFixture(t, 4, func(settings *ClientSettings) {
			settings.SendBufferSettings.beforeGroupAdmissionForTest = func() { close(entered); <-release }
		})
		t.Cleanup(resume)
		old := f.initial(t)
		f.raceOnly(f.second.selected)
		time.Sleep(time.Minute)
		template := reraceTcpPacket(f.path, tcpFlagSyn, 2000, 0, nil)
		result := make(chan int, 1)
		go func() { result <- f.first.offer(template) }()
		synctest.Wait()
		select {
		case <-entered:
		default:
			t.Fatal("new SYN never reached the irreversible-enqueue producer barrier")
		}
		old.cancel()
		retired, _, _ := f.parent.detachIdleFlows(time.Now())
		if len(retired) != 0 || f.parent.receiveUpdate(f.path) != old {
			t.Fatal("reaper superseded a live source-admission reservation")
		}
		if replacement, _, _ := f.parent.sendUpdate(f.path, flowPin{}); replacement != nil {
			t.Fatal("competing sender replaced an unresolved accepted SYN owner")
		}
		f.second.unpark()
		synctest.Wait()
		current := f.parent.receiveUpdate(f.path)
		if current == nil || current == old || len(f.second.route) != 1 {
			t.Fatal("source dequeue wrote the new SYN before canonical replacement")
		}
		f.receive(t, f.second.selected, tcpFlagSyn|tcpFlagAck, 9000, 2001)
		current.stateLock.Lock()
		buffered := current.race != nil && current.race.clientStates[f.second.selected] != nil &&
			len(current.race.clientStates[f.second.selected].packets) == 1
		current.stateLock.Unlock()
		if !buffered {
			t.Fatal("ingress before public return lost the admitted generation's bounded response owner")
		}
		f.second.acknowledgeAll(template)
		resume()
		if <-result != 1 || f.parent.receiveUpdate(f.path) != current || !current.receivedInbound.Load() || len(f.parent.affinityTransitions) != 0 {
			t.Fatal("late producer observer changed the already-admitted canonical owner")
		}
	})
}

func TestAffinityRenewalPublicFastOpenAndOldSourceIngress(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newAffinitySynQueueFixture(t, 6, nil)
		old := f.initial(t)
		f.raceOnly(f.second.selected)
		time.Sleep(time.Minute)
		template := reraceTcpPacket(f.path, tcpFlagSyn, 0xfffffffe, 0, []byte{1, 2, 3})
		if f.first.offer(template) != 1 {
			t.Fatal("expired fast-open SYN was refused")
		}
		f.second.unpark()
		f.second.acknowledgeAll(template)
		current := f.parent.receiveUpdate(f.path)
		delivered := 0
		f.parent.SetReceivePacketCallback(affinityDeliveryTestCallback(ReceivePacketFunction(nil), func(*IpPath) { delivered++ }))
		for _, ack := range []uint32{0xfffffffe, 3} {
			f.receive(t, f.second.selected, tcpFlagSyn|tcpFlagAck, 9000, ack)
		}
		// The first provider is not a candidate of this particular race.
		// Correct sequence numbers do not confer candidate ownership.
		f.receive(t, f.first.selected, tcpFlagRst|tcpFlagAck, 9001, 2)
		if delivered != 0 || current.receivedInbound.Load() || current.IsDone() {
			t.Fatal("out-of-range or noncandidate ingress established/closed the renewed generation")
		}
		f.receive(t, f.second.selected, tcpFlagSyn|tcpFlagAck, 9000, 2)
		if delivered != 1 || !current.receivedInbound.Load() || current == old || !old.IsDone() {
			t.Fatal("valid wrapped fast-open SYN-ACK failed to establish exactly the renewed generation")
		}
		f.receive(t, f.first.selected, tcpFlagRst|tcpFlagAck, 9001, 2)
		if delivered != 1 || current.IsDone() {
			t.Fatal("nonselected provider reset crossed the established winner boundary")
		}
	})
}

func TestAffinityRenewalPublicActiveCallbackRefusesThenRetries(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newAffinitySynQueueFixture(t, 4, nil)
		old := f.initial(t)
		f.raceOnly(f.second.selected)
		f.receive(t, f.first.selected, tcpFlagSyn|tcpFlagAck, 9000, 1001)
		time.Sleep(time.Minute)
		entered, release, returned := make(chan struct{}), make(chan struct{}), make(chan struct{})
		var releaseOnce sync.Once
		resume := func() { releaseOnce.Do(func() { close(release) }) }
		f.parent.SetReceivePacketCallback(affinityDeliveryTestCallback(ReceivePacketFunction(nil), func(*IpPath) {
			close(entered)
			<-release
		}))
		go func() {
			defer close(returned)
			f.receive(t, f.first.selected, tcpFlagAck, 9001, 1001)
		}()
		defer func() { resume(); <-returned }()
		synctest.Wait()
		select {
		case <-entered:
		default:
			t.Fatal("actual incumbent ingress did not reach the application callback")
		}
		template := reraceTcpPacket(f.path, tcpFlagSyn, 2000, 0, nil)
		packet := MessagePoolCopy(template)
		f.first.witnesses = append(f.first.witnesses, MessagePoolShareReadOnly(packet))
		callerOwns := true
		defer func() {
			if callerOwns {
				MessagePoolReturn(packet)
			}
		}()
		selection := old.client.loadSelection()
		if f.parent.SendPacket(f.first.source, affinityRenewalProvideModeNetwork, packet, 0) {
			callerOwns = false
			t.Fatal("renewal overtook an executing old-generation app callback")
		}
		if old.IsDone() || f.parent.receiveUpdate(f.path) != old || old.client.loadSelection() != selection ||
			len(f.parent.affinityTransitions) != 0 || len(f.parent.affinityDeliveries) != 1 ||
			!bytes.Equal(packet, template) || f.races != 1 {
			t.Fatal("callback-busy refusal changed incumbent/source/caller ownership")
		}
		resume()
		<-returned
		if len(f.parent.affinityDeliveries) != 0 || old.affinityDelivery.Load() != 0 {
			t.Fatal("actual callback return did not join its lease")
		}
		f.parent.SetReceivePacketCallback(nil)
		if !f.parent.SendPacket(f.first.source, affinityRenewalProvideModeNetwork, packet, 0) {
			t.Fatal("unchanged caller-owned SYN could not retry after callback return")
		}
		callerOwns = false
		f.second.unpark()
		f.second.acknowledgeAll(template)
		if current := f.parent.receiveUpdate(f.path); current == old || current.client.Load() != f.second.selected || !old.IsDone() {
			t.Fatal("retry failed to publish its one admitted generation")
		}
	})
}

func TestAffinityRenewalPublicRefusedFastOpenRetryCannotExpandIngress(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newAffinitySynQueueFixture(t, 6, nil)
		f.initial(t)
		f.raceOnly(f.second.selected)
		time.Sleep(time.Minute)
		template := reraceTcpPacket(f.path, tcpFlagSyn, 2000, 0, []byte{1})
		if f.first.offer(template) != 1 {
			t.Fatal("initial renewed TFO was not admitted")
		}
		f.second.unpark()
		f.second.acknowledgeAll(template)
		current := f.parent.receiveUpdate(f.path)
		selection := current.client.loadSelection()
		// The preliminary routing snapshot is healthy. Change it exactly
		// before the final offer gate; no source owns this larger retry.
		f.second.selected.beforeGroupCompletionForTest = func() {
			f.second.selected.stateLock.Lock()
			f.second.selected.warning = true
			f.second.selected.stateLock.Unlock()
		}
		retry := MessagePoolCopy(reraceTcpPacket(f.path, tcpFlagSyn, 2000, 0, []byte{1, 2, 3}))
		accepted := f.parent.SendPacket(f.first.source, affinityRenewalProvideModeNetwork, retry, 0)
		if !accepted {
			MessagePoolReturn(retry)
		}
		f.second.selected.beforeGroupCompletionForTest = nil
		f.second.selected.stateLock.Lock()
		f.second.selected.warning = false
		f.second.selected.stateLock.Unlock()
		if accepted || current.client.loadSelection() != selection || current.synGenerationEnd != 2002 || current.dialFailedSynEnd != 2004 {
			t.Fatal("warning-at-offer retry consumed custody or expanded admitted TFO authority")
		}
		delivered := 0
		f.parent.SetReceivePacketCallback(affinityDeliveryTestCallback(ReceivePacketFunction(nil), func(*IpPath) { delivered++ }))
		f.receive(t, f.second.selected, tcpFlagSyn|tcpFlagAck, 9000, 2004)
		if delivered != 0 || current.receivedInbound.Load() {
			t.Fatal("refused SYN bytes authorized the renewed flow's first answer")
		}
		f.receive(t, f.second.selected, tcpFlagSyn|tcpFlagAck, 9000, 2002)
		if delivered != 1 || !current.receivedInbound.Load() {
			t.Fatal("actual admitted TFO edge lost first-answer authority")
		}
	})
}

func TestAffinityRenewalFastOpenAckBoundsRejectMalformedSpan(t *testing.T) {
	for _, sequence := range []uint32{1000, 0xfffffffe, 0xffffffff} {
		base, end := sequence+1, sequence+4
		for offset := uint32(0); offset <= 3; offset++ {
			if !tcpSynAckInRange(sequence, end, base+offset) {
				t.Fatal("valid SYN payload ACK range was rejected across sequence wrap")
			}
		}
		if tcpSynAckInRange(sequence, end, base-1) || tcpSynAckInRange(sequence, end, end+1) ||
			tcpSynAckInRange(sequence, sequence, base) || tcpSynAckInRange(sequence, base+1<<31, base) {
			t.Fatal("malformed or out-of-range SYN payload ACK gained authority")
		}
	}
}
