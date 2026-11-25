package connect

import (
	"testing"
	"time"
)

func TestOrderedConnectAnswerPreservesPendingIndependentEvidence(t *testing.T) {
	channel := newConnectAdmissionTestChannel(t)
	started := time.Now()
	clock := started
	channel.packetTransferNowForTest = func() time.Time { return clock }
	origin := &connectSynOffer{orderedReturnEligible: true}
	channel.observeConnectSynAdmission(origin)
	clock = started.Add(2 * time.Millisecond)
	independent := &connectSynOffer{orderedReturnEligible: true}
	channel.observeConnectSynAdmission(independent)
	origin.orderedAnswer.Store(true)
	channel.connectSynAnswersPending.Store(true)

	// The exact peer return can prove the opening before its send terminal.
	// A later retained callback must not restore the now-answered clock.
	requireConnectAdmissionEvidence(t, "before terminal", channel, true, clock, nil)
	channel.finishConnectSynOffer(origin, true)
	requireConnectAdmissionEvidence(t, "after old terminal", channel, true, clock, nil)
	channel.finishConnectSynOffer(independent, false)
	requireConnectAdmissionEvidence(t, "after unrelated local retraction", channel, false, time.Time{}, nil)
}

func TestOrderedConnectEvidenceKeepsOverflowClockConservative(t *testing.T) {
	channel := newConnectAdmissionTestChannel(t)
	started := time.Now()
	clock := started
	channel.packetTransferNowForTest = func() time.Time { return clock }

	for index := range len(channel.connectSynCompleted) + 1 {
		clock = started.Add(time.Duration(index) * time.Millisecond)
		offer := &connectSynOffer{orderedReturnEligible: true}
		channel.observeConnectSynAdmission(offer)
		channel.finishConnectSynOffer(offer, true)
	}
	for _, offer := range channel.connectSynCompleted {
		if offer == nil {
			t.Fatal("eligible completed evidence did not fill its exact capacity")
		}
		offer.orderedAnswer.Store(true)
	}
	channel.connectSynAnswersPending.Store(true)
	requireConnectAdmissionEvidence(t, "after retiring all recorded candidates", channel, true, clock, nil)
	channel.addReceiveSyn(1)
	requireConnectAdmissionEvidence(t, "after normal ordered SYN", channel, false, time.Time{}, nil)
}

func TestOrderedConnectReturnCannotSpendForeignOrReplacedSelection(t *testing.T) {
	channel := newConnectAdmissionTestChannel(t)
	update := &multiClientChannelUpdate{
		ctx: t.Context(), synGenerationNumber: 10, synGenerationSeen: true, sequenceAdmissionEpoch: 3,
		sequenceSourceId: NewId(),
	}
	update.selectAffinityClient(channel)
	offer := &connectSynOffer{
		orderedReturnEligible: true, returnSource: update.sequenceSourceId, returnEpoch: update.sequenceAdmissionEpoch,
	}
	update.connectSynReturn = tcpConnectSynReturn{offer: offer, selection: update.client.loadSelection()}
	ahead := tcpControlObservation{valid: true, syn: true, ack: true, aheadOfGap: true,
		sequenceNumber: 20, ackSequenceNumber: 11, reliableReceiveToken: new(byte)}
	update.observeConnectSynReturnWithLock(channel, ahead, true)
	if offer.returnReceiveToken != nil {
		t.Fatal("foreign return source armed an exact ordered-connect receipt")
	}
	ahead.selectedSource = true
	update.observeConnectSynReturnWithLock(channel, ahead, true)
	if offer.returnReceiveToken != ahead.reliableReceiveToken || offer.orderedAnswer.Load() {
		t.Fatal("ahead-of-gap SYN-ACK did not stage evidence separately from resolution")
	}
	data := tcpControlObservation{valid: true, ack: true, selectedSource: true,
		sequenceNumber: 21, ackSequenceNumber: 11, payloadByteCount: 1, reliableReceiveToken: new(byte)}
	update.observeConnectSynReturnWithLock(channel, data, false)
	if offer.orderedAnswer.Load() {
		t.Fatal("unrelated reliable return sequence spent the earlier proof")
	}
	data.reliableReceiveToken = ahead.reliableReceiveToken
	update.selectAffinityClient(channel)
	update.observeConnectSynReturnWithLock(channel, data, false)
	if offer.orderedAnswer.Load() || update.connectSynReturn.offer != nil {
		t.Fatal("a newly selected generation spent an older receive proof")
	}
}
