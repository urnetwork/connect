package connect

import (
	"testing"
	"time"
)

func TestMultiClientBusyStaleUsesProbeAckAsLiveness(t *testing.T) {
	const staleTimeout = 5 * time.Second
	now := time.Now()
	channel := &multiClientChannel{
		settings: &MultiClientSettings{
			CPingBusyStaleTimeout: staleTimeout,
		},
		packetStats: &clientWindowStats{
			lastSendTime:       now.Add(-time.Second),
			lastReceiveAckTime: now.Add(-2 * staleTimeout),
		},
	}

	if !channel.busyStale() {
		t.Fatal("recently sending channel with stale return acks should need a probe")
	}

	channel.packetStats.lastBusyProbeAckTime = now
	if channel.busyStale() {
		t.Fatal("successful probe should refresh liveness")
	}

	channel.packetStats.lastBusyProbeAckTime = now.Add(-2 * staleTimeout)
	if !channel.busyStale() {
		t.Fatal("expired probe ack should become stale while sends continue")
	}

	channel.packetStats.lastSendTime = now.Add(-2 * staleTimeout)
	if channel.busyStale() {
		t.Fatal("channel without a recent send is not busy-stale")
	}
}

func TestMultiClientBusyStaleUsesTransferAckAsLiveness(t *testing.T) {
	const staleTimeout = 5 * time.Second
	now := time.Now()
	channel := &multiClientChannel{
		settings: &MultiClientSettings{
			CPingBusyStaleTimeout: staleTimeout,
		},
		packetStats: &clientWindowStats{
			sendNackCount:            1,
			firstOutstandingSendTime: now.Add(-2 * staleTimeout),
			lastSendTime:             now,
			lastSendAckTime:          now,
		},
	}

	if channel.busyStale() {
		t.Fatal("recent transfer acknowledgement did not prove one-way peer liveness")
	}

	channel.packetStats.lastSendAckTime = now.Add(-2 * staleTimeout)
	if !channel.busyStale() {
		t.Fatal("expired transfer acknowledgement masked a stalled outstanding send")
	}
}
