// Wire-level controls keep admission separate from ready route progress.
package connect

import (
	"bytes"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

// Consumes one actual NoAck wire Pack and compares every original in order.
// It does not release the unrelated seed's reliable delivery credit.
func (self *nativeBatchCreditFixture) requireUdpWire(templates ...[]byte) {
	self.t.Helper()
	synctest.Wait()
	select {
	case wire := <-self.base.route:
		defer MessagePoolReturn(wire)
		pack := decodeSendPackLifecycleWirePack(self.t, wire)
		if !pack.Nack || len(pack.Frames) != len(templates) {
			self.t.Fatal("ready whole-group write changed reliability or member count")
		}
		for index, template := range templates {
			if !bytes.Equal(pack.Frames[index].MessageBytes, template) {
				self.t.Fatalf("ready whole-group member %d changed order or bytes", index)
			}
		}
	default:
		self.t.Fatal("ready UDP whole group did not reach the wire before unrelated TCP acknowledgement")
	}
}

// Runs the same real seed and public entry as the original causal admission
// tests, but requires the physical NoAck wire before releasing its peer ACK.
func testNativeBatchReadyUdpWire(t *testing.T, mux bool, udpCount int) {
	t.Helper()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newNativeBatchCreditFixture(t)
		udp := [][]byte{f.udp}
		if udpCount == 2 {
			udp = append(udp, ipOosUdpPacket(f.udpPath, []byte{0x34}))
		}
		templates := append([][]byte{f.tcp}, udp...)
		done := f.startBatch(mux, -1, templates...)
		synctest.Wait()
		select {
		case <-done:
			t.Fatal("mixed batch returned while its reliable original still lacked credit")
		default:
		}
		if count, size := f.source.resendQueue.QueueSize(); count != 1 || size <= 1 ||
			!f.source.resendCapacityUnavailable.Load() {
			t.Fatal("ready UDP progress consumed or bypassed the real TCP retention premise")
		}
		if f.udpAdmitted.Load() != 1 || f.fastWritten.Load() != 1 {
			t.Fatal("ready UDP group was not admitted and written exactly once")
		}
		f.requireUdpWire(udp...)
		if len(f.base.route) != 0 {
			t.Fatal("a single ready whole group produced multiple wire Packs")
		}
		f.releasePeerCredit()
		result := f.completedBatch(done)
		if result.count != 1+udpCount || !mux && result.accepted != uint64(1<<uint(1+udpCount))-1 {
			t.Fatal("wire progress changed exact batch membership after credit release")
		}
		f.finishPackets(f.tcp)
	})
}

// Desired physical-progress regression, distinct from admission-only success.
func TestNativeBatchTcpFirstUdpWireBeforeCredit(t *testing.T) {
	testNativeBatchReadyUdpWire(t, false, 1)
}

// The mux's real grouped entry must reach the same ready physical boundary.
func TestIpMuxTcpFirstUdpWireBeforeCredit(t *testing.T) {
	testNativeBatchReadyUdpWire(t, true, 1)
}

// A bounded two-member logical group keeps one all-or-nothing wire disposition.
func TestNativeBatchWholeUdpGroupWireBeforeCredit(t *testing.T) {
	testNativeBatchReadyUdpWire(t, false, 2)
}

// The same whole-group guarantee crosses mux classification.
func TestIpMuxWholeUdpGroupWireBeforeCredit(t *testing.T) {
	testNativeBatchReadyUdpWire(t, true, 2)
}

// A source-owned split group remains ahead of a later ready-sized group.
// No guessed queue capacity or retention flag establishes this ordering.
func TestNativeBatchQueuedUdpPrefixCannotBeOvertaken(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newNativeBatchCreditFixture(t)
		f.base.parent.settings.PacketGroupMaxPacketCount = 3
		udp := [][]byte{
			f.udp,
			ipOosUdpPacket(f.udpPath, []byte{0x34}),
			ipOosUdpPacket(f.udpPath, []byte{0x35}),
			ipOosUdpPacket(f.udpPath, []byte{0x36}),
		}
		if result := <-f.startBatch(false, 0, udp...); result.count != 4 || result.accepted != 15 {
			t.Fatal("bounded same-flow groups lost atomic admission")
		}
		if len(f.base.route) != 0 || f.fastWritten.Load() != 0 {
			t.Fatal("later small UDP group overtook its already-admitted split prefix")
		}
		f.releasePeerCredit()
		f.requireUdpWire(udp[:2]...)
		f.requireUdpWire(udp[2])
		f.requireUdpWire(udp[3])
		if len(f.base.route) != 0 {
			t.Fatal("queued prefix produced an extra physical owner")
		}
	})
}

// Exact byte bounds apply even to one oversized original; the ordinary
// queued path retains its documented oversized-first-member progress rule.
func TestNativeBatchOversizedUdpStaysQueued(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newNativeBatchCreditFixture(t)
		udp := ipOosUdpPacket(f.udpPath, make([]byte, int(sendPackBatchMaxMessageByteCount)))
		if result := <-f.startBatch(false, 0, udp); result.count != 1 || result.accepted != 1 {
			t.Fatal("oversized original lost ordinary bounded group admission")
		}
		if f.fastWritten.Load() != 0 || len(f.base.route) != 0 {
			t.Fatal("oversized group escaped conservative direct-write bounds")
		}
		f.releasePeerCredit()
		f.requireUdpWire(udp)
	})
}

// A real full route refuses the one physical attempt. Queue ownership still
// takes the complete logical group; every original is later written once.
func TestNativeBatchFailedReadyWritePreservesWholeGroup(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newNativeBatchCreditFixture(t)
		for range cap(f.base.route) {
			f.base.route <- MessagePoolCopy([]byte("synthetic occupied route"))
		}
		udp := ipOosUdpPacket(f.udpPath, []byte{0x34})
		if result := <-f.startBatch(false, 0, f.udp, udp); result.count != 2 || result.accepted != 3 {
			t.Fatal("failed direct write lost whole-group fallback admission")
		}
		if f.fastAttempted.Load() != 1 || f.fastWritten.Load() != 0 || f.udpAdmitted.Load() != 1 {
			t.Fatal("route-full attempt wrote a prefix or duplicated group admission")
		}
		for range cap(f.base.route) {
			MessagePoolReturn(<-f.base.route)
		}
		f.releasePeerCredit()
		f.requireUdpWire(f.udp, udp)
		if len(f.base.route) != 0 {
			t.Fatal("failed direct attempt duplicated a wire member")
		}
	})
}

// One native retry owns one policy decision and one recovery-observer scope.
// The first zero-wait refusal is recovered only by this exact input group.
func TestNativeBatchReadyPassInspectsPolicyOnce(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newNativeBatchCreditFixture(t)
		policy := &groupTestSecurityPolicy{stats: DefaultSecurityPolicyStatsCollector()}
		f.base.parent.securityPolicy = policy
		previous := f.base.client.settings.SendBufferSettings.SendPackLifecycleObserver
		var recovered, unrecovered int
		f.base.client.settings.SendBufferSettings.SendPackLifecycleObserver = func(event SendPackLifecycleObservation) {
			previous(event)
			if event.Phase == SendPackLifecyclePhaseTerminal {
				if err, ok := event.Err.(*SendPackAdmissionError); ok {
					if err.RecoveredByOwner {
						recovered++
					} else {
						unrecovered++
					}
				}
			}
		}
		done := f.startBatch(false, -1, f.tcp, f.udp)
		synctest.Wait()
		if policy.inspectCount.Load() != 2 || policy.refreshCount.Load() != 2 ||
			recovered != 0 || unrecovered != 0 {
			t.Fatal("initial visits repeated policy or finalized a still-owned refusal")
		}
		f.requireUdpWire(f.udp)
		f.releasePeerCredit()
		if result := f.completedBatch(done); result.count != 2 || result.accepted != 3 {
			t.Fatal("prepared retry lost exact input membership")
		}
		f.finishPackets(f.tcp)
		if policy.inspectCount.Load() != 2 || policy.refreshCount.Load() != 2 ||
			recovered != 1 || unrecovered != 0 {
			t.Fatal("retry repeated policy or lost its exact recovered refusal scope")
		}
	})
}

// Mux content callbacks keep first-seen order and run once, even though the
// upstream queue internally visits the TCP input more than once.
func TestIpMuxReadyPassClassifiesEachGroupOnce(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newNativeBatchCreditFixture(t)
		packets := f.ownPackets(f.tcp, f.udp)
		var order []IpProtocol
		entry := &IpMux{
			upstream:          f.base.parent.SendPacket,
			upstreamGroupSend: f.base.parent.sendPacketGroup,
			onSendGroup: func(_ TransferPath, _ protocol.ProvideMode, group *ipPacketGroup, timeout time.Duration) bool {
				if timeout != 5*time.Second {
					t.Error("mux observer inherited internal zero-wait admission budget")
				}
				order = append(order, group.ipPath.Protocol)
				return false
			},
		}
		done := make(chan nativeBatchCreditResult, 1)
		f.workers.Add(1)
		go func() {
			defer f.workers.Done()
			done <- nativeBatchCreditResult{count: entry.SendPacketBatch(f.base.source, protocol.ProvideMode_Network, packets, 5*time.Second)}
		}()
		synctest.Wait()
		if len(order) != 2 || order[0] != IpProtocolTcp || order[1] != IpProtocolUdp {
			t.Fatal("mux classification did not preserve one first-seen callback per group")
		}
		f.requireUdpWire(f.udp)
		f.releasePeerCredit()
		if result := f.completedBatch(done); result.count != 2 || len(order) != 2 {
			t.Fatal("queue retry repeated mux classification or lost a group")
		}
		f.finishPackets(f.tcp)
	})
}
