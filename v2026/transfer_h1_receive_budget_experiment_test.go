package connect

import (
	"fmt"
	"os"
	"testing"
	"testing/synctest"
	"time"
)

// These controls exercise the production advertisement and estimator. They
// are not evidence that an iOS process fits a memory cap or that a physical
// H1 socket can achieve the modeled bandwidth-delay window.
func TestH1ReceiveBudgetExperimentPermission(t *testing.T) {
	for _, capacity := range []ByteCount{kib(1536), mib(2), kib(2560)} {
		t.Run(fmt.Sprint(capacity), func(t *testing.T) {
			root := NewTransferMemoryBudget(mib(13))
			leaf := NewTransferMemoryBudgetWithParent(capacity, root)
			receive := &ReceiveSequence{
				receiveBufferSettings: &ReceiveBufferSettings{ReceiveQueueMaxByteCount: capacity},
				receiveQueue:          newReceiveQueue(leaf, 0),
			}
			sender := newEstimatorFixture(t, func(settings *SendBufferSettings) {
				settings.ResendQueueMaxByteCount = kib(512)
				settings.ResendQueueMinByteCount = kib(64)
				settings.ResendQueueBudget = NewTransferMemoryBudget(mib(8))
				settings.DeliverySizedWindowScale = deliverySizedWindowScale
			})
			advertisement := receive.receiveWindowAdvertisement()
			sender.observeReceiveWindowAdvertisement(receiveAckMessage{
				receiveWindowSet: true, receiveWindowByteCount: uint32(advertisement),
			})
			estimate := sender.sendWindowEstimate(time.Now())
			if advertisement != uint64(capacity) || estimate.Ceiling != capacity || estimate.Window != kib(512) {
				t.Fatal("receive capacity did not set permission independently of learned delivery")
			}
			if !leaf.TryReserve(capacity) || leaf.TryReserve(1) || root.UsedByteCount() != capacity {
				t.Fatal("receive experiment did not charge the exact shared owner")
			}
			if receive.receiveWindowAdvertisement() != advertisement {
				t.Fatal("occupied receive bytes were subtracted twice from peer permission")
			}
			leaf.SetTotalByteCount(kib(1536))
			if receive.receiveWindowAdvertisement() != uint64(kib(1536)) || leaf.TryReserve(1) {
				t.Fatal("shrunk receive allowance admitted more before existing owners drained")
			}
			leaf.Release(capacity)
			if root.UsedByteCount() != 0 {
				t.Fatal("receive ownership did not release")
			}
			// Raising the pool alone is insufficient when the field is lower.
			leaf.SetTotalByteCount(kib(2560))
			receive.receiveBufferSettings.ReceiveQueueMaxByteCount = mib(2)
			if receive.receiveWindowAdvertisement() != uint64(mib(2)) {
				t.Fatal("advertisement bypassed its independent configured ceiling")
			}
		})
	}
}

// Opt-in mechanism screen with the existing actual Transfer workers and FIFO
// serializer. Every arm has the same eight-MiB send budget, lossless reliable
// route, one flow, packet size, rate and warmup. Only the aggregate advertised
// receive permission changes. This is NOT PERFVAR, a current SDK constructor
// capture, a physical H1/TLS benchmark, or the device-runtime memory gate.
func TestH1ReceiveBudgetExperimentWindowWorkers(t *testing.T) {
	if os.Getenv("CONNECT_H1_RECEIVE_BUDGET_EXPERIMENT") != "1" {
		t.Skip("opt-in virtual-time H1 receive-permission mechanism screen")
	}
	assertMessagePoolOwnership(t)
	for _, roundTrip := range []time.Duration{500 * time.Millisecond, time.Second} {
		for _, capacity := range []ByteCount{kib(1536), mib(2), kib(2560)} {
			t.Run(fmt.Sprintf("rtt=%s/receive=%d", roundTrip, capacity), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					reading := measureWindowPathCell(t, windowPathCell{
						Arm: "delivery", ReceiveWindow: capacity, Budget: mib(8),
						RoundTrip: roundTrip, Compression: 10 * time.Millisecond,
						Flows: 1, Payload: 1280, Rate: 100_000_000 / 8,
					}, 20*roundTrip)
					logWindowServiceReading(t, reading)
					if reading.Mbps <= 0 || reading.Window.Ceiling != capacity || reading.Window.Window > capacity ||
						reading.MeasurementRelayDrops != 0 || reading.NatRefused != 0 ||
						reading.Receiver.ReceiveQueueDropCount != 0 || reading.Receiver.ReceiveQueueEvictionCount != 0 ||
						reading.Receiver.PackHandoffDropCount != 0 || reading.Receiver.AckHandoffDropCount != 0 {
						t.Fatal("receive-permission experiment stalled, lost data or exceeded its bound")
					}
				})
			})
		}
	}
}
