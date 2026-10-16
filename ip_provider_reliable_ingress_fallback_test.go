package connect

import (
	"bytes"
	"net"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/urnetwork/connect/protocol"
)

// An unrelated source's ordinary UDP upload between documentation addresses.
func reliableFallbackOtherUdpPath(version int) *IpPath {
	if version == 4 {
		return &IpPath{Version: 4, Protocol: IpProtocolUdp,
			SourceIp: net.ParseIP("192.0.2.30"), SourcePort: 49153,
			DestinationIp: net.ParseIP("203.0.113.40"), DestinationPort: 443}
	}
	return &IpPath{Version: 6, Protocol: IpProtocolUdp,
		SourceIp: net.ParseIP("2001:db8::30"), SourcePort: 49153,
		DestinationIp: net.ParseIP("2001:db8:2::40"), DestinationPort: 443}
}

// The provider's decode workspace is its data budget, then one prepaid
// fallback slot per kind that every source shares. Here an unrelated source's
// ordinary UDP ingress holds the data slot while the data budget is full, so a
// reliable TCP segment of this source finds no workspace at all. That used to
// fail the segment's receipt and cancel this source's whole sequence merely
// because another source was decoding. The segment must wait instead, unACKed,
// and reach the final TCP queue once workspace and NAT capacity return.
func TestProviderReliableTcpFallbackSlotHeldByAnotherSourceDefers(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, version := range []int{4, 6} {
		synctest.Test(t, func(t *testing.T) {
			budget := NewTransferMemoryBudget(mib(2))
			f := newReliableTcpIngressFixtureWithPath(t, reliableDeliveryTestPath(version), budget)
			sequence := f.runningReceiveSequence()

			udp := MessagePoolCopy(ipOosUdpPacket(reliableFallbackOtherUdpPath(version), make([]byte, 600)))
			defer MessagePoolReturn(udp)
			udpFrames := []*protocol.Frame{{MessageType: protocol.MessageType_IpIpPacketToProvider, Raw: true, MessageBytes: udp}}
			fill := budget.Available()
			if !budget.TryReserve(fill) {
				t.Fatalf("ipv%d could not fill the data budget", version)
			}
			filled := true
			defer func() {
				if filled {
					budget.Release(fill)
				}
			}()
			entered, release, otherDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			// A failed assertion must not leave the other source holding the
			// slot that provider teardown joins.
			defer unblock()
			f.provider.beforeLocalUserNatAdmissionForTest = func(Id, [][]byte) {
				close(entered)
				<-release
			}
			go func() {
				defer close(otherDone)
				f.provider.ClientReceive(SourceId(NewId()), udpFrames, Peer{ProvideMode: protocol.ProvideMode_Network})
			}()
			<-entered
			if f.provider.ingressMemory.UsedByteCount() != natProviderIngressBytes {
				t.Fatalf("ipv%d the other source does not hold the shared data slot", version)
			}

			payload := []byte("segment refused decode workspace")
			messageId := f.pack(sequence, 0, payload)
			synctest.Wait()
			if err := sequence.ctx.Err(); err != nil {
				t.Fatalf("ipv%d another source's decode slot canceled this sequence: %v", version, err)
			}
			if present, head := reliableIngressCumulativeHead(sequence); present {
				t.Fatalf("ipv%d the undecoded segment was ACKed: head=%s", version, head.messageId)
			}

			unblock()
			<-otherDone
			synctest.Wait()
			budget.Release(fill)
			filled = false
			synctest.Wait()
			(<-f.tcp.sendItems).release()
			synctest.Wait()
			select {
			case item := <-f.tcp.sendItems:
				if item.tcp.seq != 101 || !bytes.Equal(item.tcp.payload, payload) {
					t.Errorf("ipv%d the deferred segment changed: seq=%d payload=%q", version, item.tcp.seq, item.tcp.payload)
				}
				item.release()
			default:
				t.Fatalf("ipv%d the deferred segment never reached the final TCP queue", version)
			}
			synctest.Wait()
			if present, head := reliableIngressCumulativeHead(sequence); !present || head.messageId != messageId {
				t.Errorf("ipv%d the deferred segment was not ACKed after admission: present=%t head=%s want=%s", version, present, head.messageId, messageId)
			}
			if err := sequence.ctx.Err(); err != nil {
				t.Errorf("ipv%d the sequence ended: %v", version, err)
			}
		})
	}
}

// A deferred data decode keeps its flow's place. The first segment is larger
// than either fallback slot, so with the data budget full only that budget can
// decode it; the second fits the free data slot. Decoded at once, the second
// would create its NAT owner first and reach the final TCP queue ahead of the
// first. It must queue behind the deferred segment instead.
func TestProviderReliableTcpDeferredDecodeKeepsFlowOrder(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, version := range []int{4, 6} {
		synctest.Test(t, func(t *testing.T) {
			budget := NewTransferMemoryBudget(mib(2))
			f := newReliableTcpIngressFixtureWithPath(t, reliableDeliveryTestPath(version), budget)
			sequence := f.runningReceiveSequence()
			fill := budget.Available()
			if !budget.TryReserve(fill) {
				t.Fatalf("ipv%d could not fill the data budget", version)
			}
			filled := true
			defer func() {
				if filled {
					budget.Release(fill)
				}
			}()
			large := bytes.Repeat([]byte{0x4c}, 3*1024)
			small := []byte("small segment behind a deferred one")
			first, _ := f.uploadPack(sequence, 0, 101, large)
			second, secondId := f.uploadPack(sequence, 1, uint32(101+len(large)), small)
			for _, pack := range []*ReceivePack{first, second} {
				if ok, err := sequence.Pack(pack, 0); !ok || err != nil {
					pack.messagePoolReturn()
					t.Fatalf("ipv%d segment was refused: ok=%t err=%v sequence=%v", version, ok, err, sequence.ctx.Err())
				}
				synctest.Wait()
			}
			if err := sequence.ctx.Err(); err != nil {
				t.Fatalf("ipv%d a segment without decode workspace canceled the sequence: %v", version, err)
			}
			if used := f.provider.ingressMemory.UsedByteCount(); used != 0 {
				t.Fatalf("ipv%d a deferred decode retained the data slot: used=%d", version, used)
			}

			budget.Release(fill)
			filled = false
			synctest.Wait()
			(<-f.tcp.sendItems).release()
			for _, want := range []struct {
				seq     uint32
				payload []byte
			}{{seq: 101, payload: large}, {seq: uint32(101 + len(large)), payload: small}} {
				synctest.Wait()
				select {
				case item := <-f.tcp.sendItems:
					if item.tcp.seq != want.seq || !bytes.Equal(item.tcp.payload, want.payload) {
						t.Errorf("ipv%d segments reached TCP out of order: seq=%d want=%d", version, item.tcp.seq, want.seq)
					}
					item.release()
				default:
					t.Fatalf("ipv%d segment seq=%d never reached the final TCP queue", version, want.seq)
				}
			}
			synctest.Wait()
			if present, head := reliableIngressCumulativeHead(sequence); !present || head.messageId != secondId {
				t.Errorf("ipv%d both segments were not ACKed: present=%t head=%s want=%s", version, present, head.messageId, secondId)
			}
		})
	}
}

// A small control never queues behind deferred data: it may pass pending data
// at final admission, and it can be what lets the data budget drain. With the
// data budget full and a large segment deferred, the flow's window update must
// still reach the TCP sequence through the control slot.
func TestProviderReliableTcpDeferredDecodeLetsControlPass(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, version := range []int{4, 6} {
		synctest.Test(t, func(t *testing.T) {
			budget := NewTransferMemoryBudget(mib(2))
			f := newReliableTcpIngressFixtureWithPath(t, reliableDeliveryTestPath(version), budget)
			f.establishForControl()
			sequence := f.runningReceiveSequence()
			fill := budget.Available()
			if !budget.TryReserve(fill) {
				t.Fatalf("ipv%d could not fill the data budget", version)
			}
			filled := true
			defer func() {
				if filled {
					budget.Release(fill)
				}
			}()
			large := bytes.Repeat([]byte{0x43}, 3*1024)
			data, _ := f.uploadPack(sequence, 0, 101, large)
			control, controlId := f.uploadPack(sequence, 1, 101, nil)
			for _, pack := range []*ReceivePack{data, control} {
				if ok, err := sequence.Pack(pack, 0); !ok || err != nil {
					pack.messagePoolReturn()
					t.Fatalf("ipv%d pack was refused: ok=%t err=%v sequence=%v", version, ok, err, sequence.ctx.Err())
				}
				synctest.Wait()
			}
			if err := sequence.ctx.Err(); err != nil {
				t.Fatalf("ipv%d a segment without decode workspace canceled the sequence: %v", version, err)
			}
			f.tcp.mutex.Lock()
			window := f.tcp.receiveWindowSize
			f.tcp.mutex.Unlock()
			if window != 4096 {
				t.Errorf("ipv%d the control queued behind deferred data: window=%d, want 4096", version, window)
			}
			if present, head := reliableIngressCumulativeHead(sequence); present {
				t.Errorf("ipv%d the control was cumulatively ACKed across deferred data: head=%s", version, head.messageId)
			}

			budget.Release(fill)
			filled = false
			synctest.Wait()
			(<-f.tcp.sendItems).release()
			synctest.Wait()
			select {
			case item := <-f.tcp.sendItems:
				if item.tcp.seq != 101 || !bytes.Equal(item.tcp.payload, large) {
					t.Errorf("ipv%d the deferred segment changed: seq=%d", version, item.tcp.seq)
				}
				item.release()
			default:
				t.Fatalf("ipv%d the deferred segment never reached the final TCP queue", version)
			}
			synctest.Wait()
			if present, head := reliableIngressCumulativeHead(sequence); !present || head.messageId != controlId {
				t.Errorf("ipv%d the admitted prefix was not ACKed: present=%t head=%s want=%s", version, present, head.messageId, controlId)
			}
		})
	}
}

// Deferral waits only for workspace that can come. A segment larger than
// either fallback slot can be decoded only under the data budget, and once
// that budget can never hold it the segment fails its receipt at once, as an
// impossible NAT admission does, rather than waiting for a drain.
func TestProviderReliableTcpFrameThatCanNeverFitTheDataBudgetFails(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, version := range []int{4, 6} {
		synctest.Test(t, func(t *testing.T) {
			budget := NewTransferMemoryBudget(mib(2))
			f := newReliableTcpIngressFixtureWithPath(t, reliableDeliveryTestPath(version), budget)
			sequence := f.runningReceiveSequence()
			// Retained owners survive a role-driven shrink, but no decode
			// workspace for this segment can ever fit the new ceiling.
			budget.SetTotalByteCount(1)
			defer budget.SetTotalByteCount(mib(2))
			pack, messageId := f.uploadPack(sequence, 0, 101, bytes.Repeat([]byte{0x46}, 3*1024))
			if ok, err := sequence.Pack(pack, 0); !ok || err != nil {
				pack.messagePoolReturn()
				t.Fatalf("ipv%d the segment was refused: ok=%t err=%v", version, ok, err)
			}
			synctest.Wait()
			if sequence.ctx.Err() == nil {
				t.Fatalf("ipv%d a segment that can never be decoded waited instead of failing", version)
			}
			if present, head := reliableIngressCumulativeHead(sequence); present {
				t.Fatalf("ipv%d the undecodable segment was ACKed: head=%s original=%s", version, head.messageId, messageId)
			}
		})
	}
}

// A deferred decode belongs to its provider. Closing the provider cancels the
// deferred segment: its receipt fails unACKed, and the operation returns its
// share of the borrowed frame.
func TestProviderReliableTcpDeferredDecodeEndsWithItsProvider(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, version := range []int{4, 6} {
		synctest.Test(t, func(t *testing.T) {
			budget := NewTransferMemoryBudget(mib(2))
			f := newReliableTcpIngressFixtureWithPath(t, reliableDeliveryTestPath(version), budget)
			sequence := f.runningReceiveSequence()
			fill := budget.Available()
			if !budget.TryReserve(fill) {
				t.Fatalf("ipv%d could not fill the data budget", version)
			}
			defer budget.Release(fill)
			pack, messageId := f.uploadPack(sequence, 0, 101, bytes.Repeat([]byte{0x45}, 3*1024))
			if ok, err := sequence.Pack(pack, 0); !ok || err != nil {
				pack.messagePoolReturn()
				t.Fatalf("ipv%d the segment was refused: ok=%t err=%v", version, ok, err)
			}
			synctest.Wait()
			if err := sequence.ctx.Err(); err != nil {
				t.Fatalf("ipv%d the deferred segment ended its sequence: %v", version, err)
			}
			f.provider.Close()
			synctest.Wait()
			if sequence.ctx.Err() == nil {
				t.Fatalf("ipv%d the closed provider left its deferred segment waiting", version)
			}
			if present, head := reliableIngressCumulativeHead(sequence); present {
				t.Fatalf("ipv%d the canceled segment was ACKed: head=%s original=%s", version, head.messageId, messageId)
			}
		})
	}
}
