// A hooked flow's idle timeout is its decision's (sidecar design §17.5, §17.6):
// a flow given 600 s outlives its NAT's 300 s TCP and 60 s UDP and ICMP idles,
// while a sibling whose decision names none keeps the NAT's, in the per-flow
// loops and in the shared UDP lifecycle. Every event runs in virtual time over
// in-memory upstreams.
package connect

import (
	"context"
	"io"
	"net"
	"net/netip"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// The idle a test hook gives each flow: 600 s for source port 42001, none for
// any other.
func newEgressIdleHook() *egressTestHook {
	hook := newEgressTestHook()
	hook.setResolver(func(flow *EgressFlow) EgressDecision {
		decision := EgressDecision{Verdict: EgressVerdictPass}
		if flow.From.Port() == 42001 || flow.Protocol == IpProtocolIcmp && flow.From.Addr() == netip.MustParseAddr("192.0.2.1") {
			decision.IdleTimeout = 600 * time.Second
		}
		return decision
	})
	return hook
}

// In-memory upstreams, one per dial in dial order, each recording when its
// flow closed it.
type egressIdleUpstreams struct {
	stateLock sync.Mutex
	closedAt  []time.Time
	waiters   sync.WaitGroup
}

func (self *egressIdleUpstreams) settings() *DialContextSettings {
	return &DialContextSettings{
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			local, upstream := net.Pipe()
			index := func() int {
				self.stateLock.Lock()
				defer self.stateLock.Unlock()
				self.closedAt = append(self.closedAt, time.Time{})
				return len(self.closedAt) - 1
			}()
			self.waiters.Add(1)
			go func() {
				defer self.waiters.Done()
				io.Copy(io.Discard, upstream)
				upstream.Close()
				self.stateLock.Lock()
				defer self.stateLock.Unlock()
				self.closedAt[index] = time.Now()
			}()
			return local, nil
		},
	}
}

func (self *egressIdleUpstreams) closed(index int) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return index < len(self.closedAt) && !self.closedAt[index].IsZero()
}

// A hooked NAT inside the caller's bubble, its packets toward the source
// collected in order.
type egressIdleNat struct {
	nat       *LocalUserNat
	source    TransferPath
	stateLock sync.Mutex
	received  [][]byte
}

func newEgressIdleNat(t *testing.T, configure func(*LocalUserNatSettings)) *egressIdleNat {
	t.Helper()
	settings := DefaultLocalUserNatSettingsWithBufferSize(16)
	settings.Log = NewNoopLogger()
	settings.EgressHook = newEgressIdleHook()
	// only the idle timeouts may end a flow
	settings.TcpBufferSettings.ReadTimeout = time.Hour
	settings.UdpBufferSettings.ReadTimeout = time.Hour
	settings.IcmpBufferSettings.ReadTimeout = time.Hour
	configure(settings)
	self := &egressIdleNat{source: SourceId(NewId())}
	self.nat = NewLocalUserNat(context.Background(), "egress idle test", settings)
	self.nat.AddReceivePacketCallback(func(_ TransferPath, _ protocol.ProvideMode, _ *IpPath, packet []byte) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.received = append(self.received, append([]byte(nil), packet...))
	})
	return self
}

func (self *egressIdleNat) send(t *testing.T, packet []byte) {
	t.Helper()
	owned := MessagePoolCopy(packet)
	if !self.nat.SendPacket(self.source, protocol.ProvideMode_Network, owned, -1) {
		MessagePoolReturn(owned)
		t.Fatal("the NAT refused a packet")
	}
	synctest.Wait()
}

func (self *egressIdleNat) takeReceived() [][]byte {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	received := self.received
	self.received = nil
	return received
}

func (self *egressIdleNat) close(t *testing.T) {
	if err := self.nat.CloseAndWait(context.Background()); err != nil {
		t.Error(err)
	}
}

// Checks, at each edge, which of the two flows its NAT has closed.
func checkEgressIdleEdges(t *testing.T, natIdleTimeout time.Duration, closed func(int) bool) {
	t.Helper()
	start := time.Now()
	at := func(offset time.Duration, wantLong bool, wantShort bool) {
		t.Helper()
		time.Sleep(start.Add(offset).Sub(time.Now()))
		synctest.Wait()
		if closed(0) != wantLong || closed(1) != wantShort {
			t.Fatalf("at %s: the 600 s flow closed %t and the NAT's %s flow closed %t, want %t and %t",
				offset, closed(0), natIdleTimeout, closed(1), wantLong, wantShort)
		}
	}
	at(natIdleTimeout-time.Second, false, false)
	at(natIdleTimeout+time.Second, false, true)
	at(600*time.Second-time.Second, false, true)
	at(600*time.Second+time.Second, true, true)
}

// TCP: the 600 s flow outlives the NAT's 300 s.
func TestEgressIdleTcpFlowKeepsItsDecisionsIdle(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		upstreams := &egressIdleUpstreams{}
		rig := newEgressIdleNat(t, func(settings *LocalUserNatSettings) {
			settings.TcpBufferSettings.IdleTimeout = 300 * time.Second
			settings.TcpBufferSettings.DialContextSettings = upstreams.settings()
		})
		defer upstreams.waiters.Wait()
		defer rig.close(t)
		// no handshake ACK follows: a pure ACK is activity, which would move
		// each reap a full window later
		for _, sourcePort := range []int{42001, 42002} {
			path := egressTestFlowPath(IpProtocolTcp, net.IPv4(192, 0, 2, 87), 8443)
			path.SourcePort = sourcePort
			rig.send(t, egressTestTcpPacket(path, tcpFlagSyn, 100, 0, nil))
			if received := rig.takeReceived(); len(received) != 1 || !egressTestParse(t, received[0]).Syn {
				t.Fatalf("a SYN got %d answers, want its SYN-ACK", len(received))
			}
		}
		checkEgressIdleEdges(t, 300*time.Second, upstreams.closed)
	})
}

// UDP's per-flow loop: the 600 s flow outlives the NAT's 60 s.
func TestEgressIdleUdpFlowKeepsItsDecisionsIdle(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		upstreams := &egressIdleUpstreams{}
		rig := newEgressIdleNat(t, func(settings *LocalUserNatSettings) {
			settings.UdpBufferSettings.IdleTimeout = 60 * time.Second
			settings.UdpBufferSettings.DialContextSettings = upstreams.settings()
		})
		defer upstreams.waiters.Wait()
		defer rig.close(t)
		for _, sourcePort := range []int{42001, 42002} {
			path := egressTestFlowPath(IpProtocolUdp, net.IPv4(192, 0, 2, 88), 8443)
			path.SourcePort = sourcePort
			rig.send(t, ipOosUdpPacket(path, []byte("datagram")))
		}
		checkEgressIdleEdges(t, 60*time.Second, upstreams.closed)
	})
}

// ICMP: the 600 s echo flow outlives the NAT's 60 s.
func TestEgressIdleIcmpFlowKeepsItsDecisionsIdle(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		var stateLock sync.Mutex
		var echoes []*egressTestEcho
		rig := newEgressIdleNat(t, func(settings *LocalUserNatSettings) {
			settings.IcmpBufferSettings.IdleTimeout = 60 * time.Second
			settings.IcmpBufferSettings.newEgressForTest = func(context.Context, int, net.IP, *IcmpBufferSettings) (icmpEgress, error) {
				stateLock.Lock()
				defer stateLock.Unlock()
				echo := &egressTestEcho{replies: make(chan icmpTimerTestReply, 8), closed: make(chan struct{})}
				echoes = append(echoes, echo)
				return echo, nil
			}
		})
		defer rig.close(t)
		// the hook gives the flow from 192.0.2.1 600 s
		rig.send(t, testingIcmp4EchoPacket("192.0.2.1", "192.0.2.89", 0x5555, 1, []byte("ping")))
		rig.send(t, testingIcmp4EchoPacket("192.0.2.2", "192.0.2.89", 0x5556, 1, []byte("ping")))
		closed := func(index int) bool {
			stateLock.Lock()
			defer stateLock.Unlock()
			if len(echoes) <= index {
				return false
			}
			select {
			case <-echoes[index].closed:
				return true
			default:
				return false
			}
		}
		checkEgressIdleEdges(t, 60*time.Second, closed)
	})
}

// UDP's shared lifecycle: one sweep reaps each flow at its own idle.
func TestEgressIdleUdpSharedSweepKeepsEachFlowsIdle(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		settings := DefaultUdpBufferSettingsWithBufferSize(1)
		settings.IdleTimeout = 60 * time.Second
		settings.SharedSocketLifecycle = true
		settings.ReceiveShardCount = 1
		settings.Log = NewNoopLogger()
		buffer := NewUdp4Buffer(ctx, nil, settings)
		source := SourceId(NewId())
		var sequences []*UdpSequence
		for _, sourcePort := range []uint16{42001, 42002} {
			sequence := NewUdpSequence(
				ctx, nil, source, protocol.ProvideMode_Network, 4,
				net.IPv4(192, 0, 2, 1).To4(), sourcePort,
				net.IPv4(198, 51, 100, 1).To4(), 44000,
				settings,
			)
			sequence.sharedSocketLifecycle = true
			sequence.sharedLifecycleWake = buffer.sharedLifecycleWake
			if sourcePort == 42001 {
				sequence.egressFlow.registration.Store(&egressRegistration{
					decision: EgressDecision{Verdict: EgressVerdictPass, IdleTimeout: 600 * time.Second},
				})
			}
			bufferId := NewBufferId4(source, sequence.sourceIp, int(sequence.sourcePort),
				sequence.destinationIp, int(sequence.destinationPort))
			buffer.sequences[bufferId] = sequence
			if buffer.sourceSequences[source] == nil {
				buffer.sourceSequences[source] = map[BufferId4]*UdpSequence{}
			}
			buffer.sourceSequences[source][bufferId] = sequence
			sequences = append(sequences, sequence)
		}
		buffer.sharedLifecycleWaitGroup.Add(1)
		go func() {
			defer buffer.sharedLifecycleWaitGroup.Done()
			buffer.runSharedSocketLifecycle()
		}()
		defer buffer.waitForLifecycle()
		defer cancel()
		buffer.wakeSharedSocketLifecycle()
		synctest.Wait()
		checkEgressIdleEdges(t, 60*time.Second, func(index int) bool {
			return udpSequenceDone(sequences[index])
		})
	})
}
