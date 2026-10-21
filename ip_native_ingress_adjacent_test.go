// Native ingress keeps readiness, local ownership, result indexing and bounded
// allocation observable beside the real retained-credit regression.
package connect

import (
	"context"
	"net"
	"os"
	"reflect"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// An unavailable encryption generation is not a failed peer or a delivered
// original. The same packet remains admissible once that generation is ready.
func TestNativeIngressEncryptionReadinessKeepsOwner(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, throughMux := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			parent, update, closeParent := groupTestParent(t, DisableSecurityPolicy())
			defer closeParent()
			parent.settings.TcpCollapsePrevention, parent.settings.TcpCollapseMaxHold = true, 0
			selected := newPacketTransferTestChannel()
			selected.ctx, selected.settings = parent.ctx, parent.settings
			update.client.Store(selected)
			ready, calls, resets := false, 0, 0
			parent.SetReceivePacketCallback(func(TransferPath, protocol.ProvideMode, *IpPath, []byte) { resets++ })
			selected.sendGroupForTest = func(group *parsedPacketGroup, timeout time.Duration, ack bool) (bool, error) {
				calls++
				if timeout != 0 || !ack {
					t.Error("native encryption offer changed its ready-only or reliable contract")
				}
				if !ready {
					return false, ErrEncryptionRequiredNotEstablished
				}
				for _, member := range group.packets {
					MessagePoolReturn(member.packet)
				}
				return true, nil
			}
			var entry nativeIngressTestBatchEntry = parent
			if throughMux {
				mux := &UpgradeMux{mux: &IpMux{}}
				mux.mux.SetUpstream(parent.SendPacket)
				mux.mux.setUpstreamGroupSend(parent.sendPacketGroup)
				mux.upstreamMultiClient.Store(parent)
				entry = mux
			}
			offer := func() int {
				packets := [][]byte{MessagePoolCopy(groupRecoveryPacket(101, 500, 64, tcpFlagAck, []byte{0x42}))}
				witnesses := groupTestPacketWitnesses(t, packets)
				count := offerNativeIngressTest(entry, SourceId(NewId()), packets)
				requireGroupTestWitnessesReleased(t, packets, witnesses)
				return count
			}
			start := time.Now()
			if offer() != 0 || calls != 1 || resets != 0 || time.Since(start) != 0 ||
				update.client.Load() != selected || selected.endErr != nil {
				t.Fatalf("encryption readiness reset, waited or committed an original: mux=%t calls=%d resets=%d", throughMux, calls, resets)
			}
			ready = true
			if offer() != 1 || calls != 2 || resets != 0 {
				t.Fatalf("previous refusal consumed retry/collapse authority: mux=%t calls=%d resets=%d", throughMux, calls, resets)
			}
		})
	}
}

// A cold exact flow refuses immediately; installing an already usable selected
// owner then admits its first original without a retry timer or stale collapse.
func TestNativeIngressColdFlowThenReadyOriginal(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		parent, update, closeParent := groupTestParent(t, DisableSecurityPolicy())
		defer closeParent()
		resets, calls := 0, 0
		parent.SetReceivePacketCallback(func(TransferPath, protocol.ProvideMode, *IpPath, []byte) { resets++ })
		offer := func() int {
			packets := [][]byte{MessagePoolCopy(groupRecoveryPacket(100, 0, 64, tcpFlagSyn, nil))}
			witnesses := groupTestPacketWitnesses(t, packets)
			count := parent.SendNativePacketBatch(SourceId(NewId()), protocol.ProvideMode_Network, packets, 5*time.Second)
			requireGroupTestWitnessesReleased(t, packets, witnesses)
			return count
		}
		start := time.Now()
		if offer() != 0 || time.Since(start) != 0 || resets != 0 {
			t.Fatal("cold native flow waited or fabricated a reset")
		}
		selected := newPacketTransferTestChannel()
		selected.ctx, selected.settings = parent.ctx, parent.settings
		selected.sendGroupForTest = func(group *parsedPacketGroup, timeout time.Duration, ack bool) (bool, error) {
			calls++
			if timeout != 0 || !ack {
				t.Error("ready first original lost its admission/reliability policy")
			}
			for _, packet := range group.packets {
				MessagePoolReturn(packet.packet)
			}
			return true, nil
		}
		update.client.Store(selected)
		if offer() != 1 || calls != 1 || resets != 0 {
			t.Fatal("ready first original was mistaken for a refused duplicate")
		}
	})
}

// Local override is decided beneath the remote multi-client. Its actual NAT
// channel retains the explicit local budget, even for the native capability.
func TestNativeIngressNestedLocalOverrideKeepsBudget(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		parent, _, closeParent := groupTestParent(t, DisableSecurityPolicy())
		defer closeParent()
		if err := parent.localUserNat.CloseAndWait(context.Background()); err != nil {
			t.Fatal(err)
		}
		settings := DefaultLocalUserNatSettings()
		settings.SequenceBufferSize = 1
		settings.TcpBufferSettings.DialContextSettings = &DialContextSettings{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}}
		nat := NewLocalUserNat(parent.ctx, "native-local-budget", settings)
		parent.localUserNat = nat
		entered, release := make(chan struct{}), make(chan struct{})
		var once, releaseOnce sync.Once
		unpark := func() { releaseOnce.Do(func() { close(release) }) }
		nat.beforeSendPacketForTest = func(*SendPacket) { once.Do(func() { close(entered); <-release }) }
		parent.SetBlockActionOverrides([]*BlockActionOverride{{
			OverrideId: NewId(), Hosts: []string{"198.51.100.92"}, RouteOverride: &RouteOverride{Local: true},
		}})
		var packets, witnesses [][]byte
		var worker sync.WaitGroup
		defer func() {
			unpark()
			parent.cancel()
			worker.Wait()
			if err := nat.CloseAndWait(context.Background()); err != nil {
				t.Error(err)
			}
			requireGroupTestWitnessesReleased(t, packets, witnesses)
		}()
		offer := func(sequence uint32) [][]byte {
			packet := MessagePoolCopy(groupRecoveryPacket(sequence, 0, 64, tcpFlagSyn, nil))
			packets = append(packets, packet)
			witnesses = append(witnesses, MessagePoolShareReadOnly(packet))
			return [][]byte{packet}
		}
		source := SourceId(NewId())
		if parent.SendNativePacketBatch(source, protocol.ProvideMode_Network, offer(100), 5*time.Second) != 1 {
			t.Fatal("first local owner was not admitted")
		}
		synctest.Wait()
		select {
		case <-entered:
		default:
			t.Fatal("local worker did not reach the held dispatch boundary")
		}
		if parent.SendNativePacketBatch(source, protocol.ProvideMode_Network, offer(101), 5*time.Second) != 1 {
			t.Fatal("second local owner did not occupy its fixed channel")
		}
		third := offer(102)
		done := make(chan int, 1)
		worker.Add(1)
		go func() {
			defer worker.Done()
			done <- parent.SendNativePacketBatch(source, protocol.ProvideMode_Network, third, 5*time.Second)
		}()
		synctest.Wait()
		select {
		case <-done:
			t.Fatal("nested local override was silently changed to zero-wait")
		default:
		}
		unpark()
		synctest.Wait()
		select {
		case count := <-done:
			if count != 1 || parent.PacketStats().LocalEgressPacketCount != 3 {
				t.Fatal("local release lost exact batch ownership")
			}
		default:
			t.Fatal("local capacity release did not resume its bounded sender")
		}
	})
}

// Partial acceptance is reported at original positions, including stale result
// bits, malformed packets and noncontiguous members of a refused policy group.
func TestNativeIngressResultsKeepExactMembership(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, limit := range []int{1, 2, 64} {
		parent, update, closeParent := groupTestParent(t, &groupTestSecurityPolicy{stats: DefaultSecurityPolicyStatsCollector()})
		parent.settings.PacketGroupMaxPacketCount = limit
		calls := 0
		update.client.Store(&multiClientChannel{ctx: parent.ctx, settings: parent.settings,
			sendGroupForTest: func(group *parsedPacketGroup, timeout time.Duration, _ bool) (bool, error) {
				calls++
				if timeout != 0 {
					t.Error("native results took a blocking remote offer")
				}
				for _, packet := range group.packets {
					MessagePoolReturn(packet.packet)
				}
				return true, nil
			},
		})
		other := *udpTestPath(4)
		other.SourcePort++
		packets := [][]byte{
			batchResultTestUdpPacket(udpTestPath(4), []byte{1}),
			batchResultTestUdpPacket(&other, []byte{2}),
			MessagePoolCopy([]byte{0}),
			batchResultTestUdpPacket(udpTestPath(4), []byte{0xff}),
			batchResultTestUdpPacket(udpTestPath(4), []byte{3}),
		}
		witnesses := groupTestPacketWitnesses(t, packets)
		want, wantCount := []bool{false, true, false, false, false}, 1
		if limit == 1 {
			want, wantCount = []bool{true, true, false, false, true}, 3
		}
		if limit == 2 {
			want, wantCount = []bool{false, true, false, false, true}, 2
		}
		accepted := []bool{true, true, true, true, true}
		count := parent.SendNativePacketBatchWithResults(SourceId(NewId()), protocol.ProvideMode_Network, packets, 5*time.Second, accepted)
		requireGroupTestWitnessesReleased(t, packets, witnesses)
		closeParent()
		if count != wantCount || calls != wantCount || !reflect.DeepEqual(accepted, want) {
			t.Fatalf("limit=%d count/calls=%d/%d accepted=%v want=%v", limit, count, calls, accepted, want)
		}
	}
}

// A result-shape error precedes every packet ownership transfer.
func TestNativeIngressResultsLengthMismatchKeepsOwnership(t *testing.T) {
	assertMessagePoolOwnership(t)
	parent, _, closeParent := groupTestParent(t, DisableSecurityPolicy())
	defer closeParent()
	packet := batchResultTestUdpPacket(udpTestPath(4), []byte{1})
	defer func() {
		if recover() == nil {
			t.Error("invalid native result length did not panic")
		}
		if !MessagePoolReturn(packet) {
			t.Error("invalid result shape consumed the caller's packet")
		}
	}()
	parent.SendNativePacketBatchWithResults(SourceId(NewId()), protocol.ProvideMode_Network, [][]byte{packet}, 5*time.Second, nil)
}

// This is a measured micro-cost, not a zero-allocation claim. It includes
// grouping and actual ownership disposal for 1- and 64-packet native bursts.
func TestNativeIngressAllocationCensus(t *testing.T) {
	if os.Getenv(allocationCountRootEnv) == "" {
		output, err := runAllocationCountChild(t, t.Name(), "")
		t.Logf("%s", output)
		if err != nil {
			t.Fatal(err)
		}
		return
	}
	parent, update, closeParent := groupTestParent(t, DisableSecurityPolicy())
	defer closeParent()
	update.client.Store(&multiClientChannel{ctx: parent.ctx, settings: parent.settings,
		sendGroupForTest: func(*parsedPacketGroup, time.Duration, bool) (bool, error) { return false, nil },
	})
	template := ipOosUdpPacket(udpTestPath(4), []byte{1})
	source := SourceId(NewId())
	for _, count := range []int{1, 64} {
		packets := make([][]byte, count)
		measure := func(native bool) float64 {
			return testing.AllocsPerRun(100, func() {
				for index := range packets {
					packets[index] = MessagePoolCopy(template)
				}
				var admitted int
				if native {
					admitted = parent.SendNativePacketBatch(source, protocol.ProvideMode_Network, packets, 5*time.Second)
				} else {
					admitted = parent.SendPacketBatch(source, protocol.ProvideMode_Network, packets, 0)
				}
				if admitted != 0 {
					t.Fatal("allocation census unexpectedly transferred a packet")
				}
			})
		}
		generic, native := measure(false), measure(true)
		t.Logf("native-admission-allocation packets=%d generic_allocs=%g native_allocs=%g delta=%g", count, generic, native, native-generic)
	}
}
