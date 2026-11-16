// Exact source cancellation retires only the native opening it actually closes.
// Real paired Transfer clients prove materialization and peer delivery first.
package connect

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// The actual native batch takes its input; the retained pool witness belongs
// only to fixture cleanup, never to a manufactured receipt or retry.
func offerSourceResetConnectPacket(t *testing.T, fixture *replyExpectationFixture, path *IpPath, flags byte) {
	t.Helper()
	sequence := path.SequenceNumber
	if flags&tcpFlagRst != 0 {
		sequence++
	}
	packet := MessagePoolCopy(reraceTcpPacket(path, flags, sequence, 0, nil))
	fixture.base.witnesses = append(fixture.base.witnesses, MessagePoolShareReadOnly(packet))
	accepted := [1]bool{}
	count := fixture.base.parent.SendNativePacketBatchWithResults(fixture.base.source,
		protocol.ProvideMode_Network, [][]byte{packet}, 0, accepted[:])
	if count != 1 || !accepted[0] {
		t.Fatalf("actual native source did not admit its exact control: flags=%d count=%d accepted=%t", flags, count, accepted[0])
	}
}

// The fixture has exactly one delivered native SYN and one genuine peer
// receipt. Its channel-wide unanswered minimum is therefore this opening's own
// health clock; no materialized descriptor is assumed to stay on a raw list.
func sourceResetConnectOpening(t *testing.T, fixture *replyExpectationFixture) (*IpPath, *multiClientChannelUpdate, time.Time) {
	t.Helper()
	path := replyExpectationPath(0, 45000)
	offerSourceResetConnectPacket(t, fixture, path, tcpFlagSyn)
	stats := fixture.await(t, 1, 1, 0)
	fixture.stateLock.Lock()
	update := fixture.updates[0]
	packet := fixture.packets[0]
	packetCount := fixture.packetCount
	fixture.stateLock.Unlock()
	if update == nil || !packet.syn || packet.ack || packet.rst || packet.fin || packet.payload || packetCount != 1 {
		t.Fatalf("single actual native SYN premise failed: update=%t peer_packets=%d SYN=%t ACK=%t RST=%t FIN=%t payload=%t",
			update != nil, packetCount, packet.syn, packet.ack, packet.rst, packet.fin, packet.payload)
	}
	update.stateLock.Lock()
	generationSeen, generationNumber := update.synGenerationSeen, update.synGenerationNumber
	metricSeen, metricNumber := update.sequenceMetricSynSeen, update.sequenceMetricSynNumber
	materializedCount := update.sequencePacketCount
	receivedInbound, done := update.receivedInbound.Load(), update.IsDone()
	update.stateLock.Unlock()
	started := stats.firstUnansweredSendSynTime
	if !generationSeen || generationNumber != path.SequenceNumber || !metricSeen || metricNumber != path.SequenceNumber ||
		materializedCount <= 0 || receivedInbound || done || started.IsZero() ||
		stats.sendSynCount != 1 || stats.sendNackByteCount != 0 || stats.receiveAckCount != 0 || stats.receiveSynCount != 0 ||
		fixture.base.client.IsDone() || fixture.peer.IsDone() || !fixture.base.selected.hasActiveTransport() {
		t.Fatalf("single materialized SYN/real receipt premise failed: generation_seen=%t generation_match=%t metric_seen=%t metric_match=%t source_packets=%d received_inbound=%t generation_done=%t marker_zero=%t sent_SYN=%d native_received=%d/%d transport_pending=%d/%d",
			generationSeen, generationNumber == path.SequenceNumber, metricSeen, metricNumber == path.SequenceNumber,
			materializedCount, receivedInbound, done, started.IsZero(),
			stats.sendSynCount, stats.receiveAckCount, stats.receiveSynCount, stats.sendNackCount, stats.sendNackByteCount)
	}
	return path, update, started
}

// An admitted native source reset really retires that exact update, while
// both carrier clients stay live and its peer receipt retires transport debt.
func sourceResetConnectClose(t *testing.T, fixture *replyExpectationFixture, path *IpPath, update *multiClientChannelUpdate, received int) *clientWindowStats {
	t.Helper()
	offerSourceResetConnectPacket(t, fixture, path, tcpFlagRst)
	stats := fixture.await(t, received, received, 0)
	fixture.stateLock.Lock()
	packet := fixture.packets[received-1]
	current := fixture.updates[path.SourcePort-45000]
	fixture.stateLock.Unlock()
	if current != update || !update.IsDone() || !packet.rst || packet.syn || packet.ack || packet.fin || packet.payload ||
		stats.sendNackByteCount != 0 || stats.receiveAckCount != 0 || stats.receiveSynCount != 0 ||
		fixture.base.client.IsDone() || fixture.peer.IsDone() || !fixture.base.selected.hasActiveTransport() {
		t.Fatal("actual source reset did not retire only its exact materialized generation")
	}
	return stats
}

// Real bubble time passes the unchanged full connect bar, then the classifier
// reads a fresh coalesced snapshot while both carrier clients remain live.
func sourceResetConnectMatureStats(t *testing.T, fixture *replyExpectationFixture, started time.Time) (*clientWindowStats, blackholeReason) {
	t.Helper()
	defaults := DefaultMultiClientSettings()
	settings := fixture.base.selected.settings
	if settings.BlackholeConnectTimeout != defaults.BlackholeConnectTimeout || settings.BlackholeConnectTimeout <= 0 ||
		settings.BlackholeTimeout != defaults.BlackholeTimeout || settings.BlackholeReceiveTimeout != defaults.BlackholeReceiveTimeout ||
		settings.StatsWindowDuration != defaults.StatsWindowDuration || settings.StatsWindowBucketDuration != defaults.StatsWindowBucketDuration {
		t.Fatal("actual native source changed the default health policy")
	}
	maturity := started.Add(settings.BlackholeConnectTimeout + settings.StatsWindowBucketDuration)
	if delay := time.Until(maturity); delay > 0 {
		time.Sleep(delay)
	}
	synctest.Wait()
	stats, err := fixture.base.selected.WindowStats()
	if err != nil || fixture.base.client.IsDone() || fixture.peer.IsDone() || !fixture.base.selected.hasActiveTransport() ||
		stats.sendNackCount != 0 || stats.sendNackByteCount != 0 || stats.receiveAckCount != 0 || stats.receiveSynCount != 0 {
		t.Fatal("fresh mature SYN snapshot lost real idle transport or receive-silence premises")
	}
	reason, held := blackholeReasonFromStats(time.Now(), stats, settings.BlackholeTimeout,
		settings.BlackholeReceiveTimeout, settings.BlackholeConnectTimeout,
		blackholeGates{minReceiveAckDestinations: settings.MinBlackholeDestinations})
	if held != blackholeNone {
		t.Fatal("isolated native connect evidence unexpectedly acquired a verdict hold")
	}
	return stats, reason
}

// A deliberate, delivered source reset ends the only attempted connection;
// ordinary ambiguous callback cancellation remains covered by the old control.
func TestNativeSourceResetDoesNotLeaveConnectDebt(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture := newReplyExpectationFixture(t)
		path, update, started := sourceResetConnectOpening(t, fixture)
		immediate := sourceResetConnectClose(t, fixture, path, update, 2)
		stats, reason := sourceResetConnectMatureStats(t, fixture, started)
		if !immediate.firstUnansweredSendSynTime.IsZero() || !stats.firstUnansweredSendSynTime.IsZero() || !unansweredConnectSince(stats).IsZero() || reason != blackholeNone {
			t.Fatalf("NATIVE_SOURCE_RESET_CONNECT_DEBT: delivered_SYN=1 delivered_RST=1 peer_acked=%d pending=%d/%d exact_generation_closed=%t marker_zero=%t unanswered_zero=%t verdict=%s",
				immediate.sendAckCount, stats.sendNackCount, stats.sendNackByteCount, update.IsDone(),
				stats.firstUnansweredSendSynTime.IsZero(), unansweredConnectSince(stats).IsZero(), reason)
		}
	})
}

// A reset on a separate tuple owns no right to acquit an existing opening.
func TestNativeUnrelatedSourceResetKeepsConnectDebt(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture := newReplyExpectationFixture(t)
		_, opening, started := sourceResetConnectOpening(t, fixture)
		before, err := fixture.base.selected.WindowStats()
		if err != nil {
			t.Fatal(err)
		}
		path := replyExpectationPath(1, 45001)
		offerSourceResetConnectPacket(t, fixture, path, tcpFlagRst)
		immediate := fixture.await(t, 2, 2, 0)
		stats, reason := sourceResetConnectMatureStats(t, fixture, started)
		fixture.stateLock.Lock()
		unrelated := fixture.updates[1]
		packet := fixture.packets[1]
		fixture.stateLock.Unlock()
		if unrelated == nil || unrelated == opening || !unrelated.IsDone() || opening.IsDone() ||
			!packet.rst || packet.syn || packet.payload || stats.receiveAckCount != 0 || stats.receiveSynCount != 0 ||
			!immediate.firstUnansweredSendSynTime.Equal(before.firstUnansweredSendSynTime) ||
			!stats.firstUnansweredSendSynTime.Equal(before.firstUnansweredSendSynTime) ||
			reason != blackholeNoReceiveSyn {
			t.Fatal("unrelated actual source reset acquitted or replaced an unanswered native opening")
		}
	})
}
