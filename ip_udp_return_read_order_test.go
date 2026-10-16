// UDP flows whose return reads wait for prepared Transfer credit share their
// provider's resend budget, so the order in which a freed credit reaches the
// waiting readers decides each flow's share of the return path.
package connect

import (
	"bytes"
	"context"
	"net"
	"runtime"
	"testing"
	"testing/synctest"

	"github.com/urnetwork/connect/protocol"
)

const udpReturnReadOrderPayloadByteCount = 1000

// Flows returning reliable datagrams through one real provider sender and
// client. The resend budget holds two written datagrams and one more read, so
// at most two are unacknowledged. The test stands in for the peer. Used only
// inside a synctest bubble.
type udpReturnReadOrderFixture struct {
	t              *testing.T
	fixture        *windowRoundFixture
	ctx            context.Context
	cancel         context.CancelFunc
	provider       *RemoteUserNatProvider
	sequences      []*UdpSequence
	unacknowledged []*windowRoundFrame
}

func newUdpReturnReadOrderFixture(t *testing.T, flowCount int, configureUdp func(*UdpBufferSettings)) *udpReturnReadOrderFixture {
	t.Helper()
	root := NewTransferMemoryBudget(mib(2))
	t.Cleanup(func() { assertRetainedBudgetBalance(t, root) })
	resendBudget := NewTransferMemoryBudgetWithParent(root.TotalByteCount(), root)
	fixture := newWindowRoundFixture(t, func(settings *SendBufferSettings) {
		settings.ResendQueueBudget = resendBudget
		settings.ResendQueueRetainedByteAccounting = true
		settings.SequenceBufferSize = 8
	}, nil)
	ctx, cancel := context.WithCancel(fixture.ctx)
	providerSettings := DefaultRemoteUserNatProviderSettings()
	providerSettings.UdpTransferNoAck = false
	providerSettings.SecurityPolicyGenerator = DisableSecurityPolicyWithStats
	provider := &RemoteUserNatProvider{
		ctx: ctx, cancel: cancel, client: fixture.sender, settings: providerSettings,
		memoryOperations:    newLifecycleAdmission(),
		localUserNat:        &LocalUserNat{ctx: ctx},
		securityPolicy:      DisableSecurityPolicyWithStats(ctx, DefaultSecurityPolicyStatsCollector()),
		packetStatsCounters: &packetStatsCounters{},
		sourceProvideMode:   map[Id]protocol.ProvideMode{},
	}
	provider.datagramSender = newProviderDatagramSender(provider)
	udpSettings := DefaultUdpBufferSettingsWithBufferSize(1)
	udpSettings.MemoryBudget = root
	udpSettings.ReadBufferByteCount = udpReturnReadOrderPayloadByteCount
	udpSettings.WriteBatchSize = 1
	udpSettings.Log = NewNoopLogger()
	if configureUdp != nil {
		configureUdp(udpSettings)
	}
	self := &udpReturnReadOrderFixture{t: t, fixture: fixture, ctx: ctx, cancel: cancel, provider: provider}
	for index := range flowCount {
		sequence := NewUdpSequence(ctx, nil, SourceId(fixture.receiver.ClientId()), protocol.ProvideMode_Network, 4,
			net.IPv4(192, 0, 2, 1).To4(), uint16(42000+index), net.IPv4(203, 0, 113, 7).To4(), 8080, udpSettings)
		if sequence == nil {
			t.Fatal("flow admission")
		}
		sequence.sharedSocketLifecycle = true
		sequence.prepareReturnReadCallback = provider.datagramSender.prepareUdp
		sequence.providerReturnCapacity = &provider.localUserNat.providerReturnCapacity
		self.sequences = append(self.sequences, sequence)
	}
	t.Cleanup(func() {
		cancel()
		for _, sequence := range self.sequences {
			sequence.Close()
		}
		<-provider.datagramSender.done
		provider.memoryOperations.close()
		<-provider.memoryOperations.Done()
	})
	// A read reserves its full pre-read credit, then trims it to the one
	// packet it wrote.
	readCredit := providerUdpReadCredit(self.sequences[0], udpReturnReadOrderPayloadByteCount, false)
	writtenCredit := providerDatagramPacketCredit(Ipv4HeaderSizeWithoutExtensions+UdpHeaderSize+udpReturnReadOrderPayloadByteCount, false)
	resendBudget.SetTotalByteCount(readCredit + writtenCredit + writtenCredit/2)
	return self
}

// Captures every Pack the provider wrote since the last call and returns the
// flow of each, in write order.
func (self *udpReturnReadOrderFixture) takeWritten() []int {
	self.t.Helper()
	synctest.Wait()
	var flows []int
	for len(self.fixture.senderOut) != 0 {
		frame := self.fixture.take(self.fixture.senderOut)
		if frame.pack == nil || len(frame.pack.Frames) != 1 {
			self.t.Fatalf("provider wrote %+v, want one returned datagram", frame.pack)
		}
		packet, err := ipPacketFromProviderBytes(frame.pack.Frames[0])
		if err != nil {
			self.t.Fatal(err)
		}
		path, _, err := ParseIpPathWithPayload(packet)
		if err != nil {
			self.t.Fatal(err)
		}
		flows = append(flows, int(path.DestinationPort)-42000)
		self.unacknowledged = append(self.unacknowledged, frame)
	}
	return flows
}

// Delivers the oldest unacknowledged Pack and returns its acknowledgement,
// which frees that datagram's credit.
func (self *udpReturnReadOrderFixture) acknowledgeOldest() {
	self.t.Helper()
	self.fixture.forward(self.unacknowledged[0], self.fixture.receiverIn)
	self.unacknowledged = self.unacknowledged[1:]
	self.fixture.acknowledge()
}

// Acknowledges ackCount Packs one at a time once the readers have filled the
// budget, and counts whose datagram each freed credit carries.
func (self *udpReturnReadOrderFixture) shareFreedCredits(ackCount int) []int {
	self.t.Helper()
	if flows := self.takeWritten(); len(flows) != 2 {
		self.t.Fatalf("readers filled the two-datagram budget with %v", flows)
	}
	var order []int
	writtenCounts := make([]int, len(self.sequences))
	for range ackCount {
		self.acknowledgeOldest()
		for _, index := range self.takeWritten() {
			order = append(order, index)
			writtenCounts[index] += 1
		}
	}
	self.t.Logf("flows written as credit freed: %v", order)
	return writtenCounts
}

// The root cause of one UDP flow taking every return credit of a provider.
// Three flows, each with a dedicated reader that always has another datagram,
// as a bulk download does, wait for credit that the test frees one
// acknowledgement at a time. Each flow must get an equal share. Every freed
// credit used to wake every paused reader to race for it, and on one
// processor the same flow's reader won all 30 while the other two got none.
func TestUdpReturnReadersShareScarceProviderCreditOnOneProcessor(t *testing.T) {
	assertMessagePoolOwnership(t)
	// Keep the process-global statistics worker outside the bubble.
	MessagePoolReturn(MessagePoolGet(1))
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	synctest.Test(t, func(t *testing.T) {
		const flowCount = 3
		const ackCount = 10 * flowCount
		order := newUdpReturnReadOrderFixture(t, flowCount, nil)
		readersDone := make(chan struct{}, flowCount)
		t.Cleanup(func() {
			order.cancel()
			for range flowCount {
				<-readersDone
			}
		})
		payload := bytes.Repeat([]byte{0x5a}, udpReturnReadOrderPayloadByteCount)
		// Readers start, and so first wait, in index order. A socket without
		// a raw descriptor skips the readiness peek, as a custom one does.
		for _, sequence := range order.sequences {
			go func() {
				defer func() { readersDone <- struct{}{} }()
				buffer := make([]byte, len(payload))
				for {
					lease, err := sequence.awaitReturnRead(nil, buffer)
					if err != nil || lease == nil {
						return
					}
					copy(buffer, payload)
					packets, err := sequence.DataPackets(buffer, len(buffer), sequence.udpBufferSettings.Mtu)
					if err != nil {
						lease.abort()
						return
					}
					lease.commit(packets)
				}
			}()
			synctest.Wait()
		}

		for index, count := range order.shareFreedCredits(ackCount) {
			if count != ackCount/flowCount {
				t.Errorf("flow %d got %d of %d freed credits, want %d", index, count, ackCount, ackCount/flowCount)
			}
		}
	})
}
