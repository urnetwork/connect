// A real tunneled DoH SYN and a ready local TCP DNS reply share one TUN pump.
// The queue premise retains and requeues actual gVisor packet references only.
package connect

import (
	"context"
	"encoding/binary"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
	"golang.org/x/net/dns/dnsmessage"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

// Complete scalar identities distinguish only the two actual handshake flows.
// Sequence/ACK/flags must stay identical across any same-flow retransmission.
type dohPumpHandshakeIdentity struct {
	source, destination         [4]byte
	sourcePort, destinationPort int
	sequence, ack               uint32
	syn, ackFlag                bool
}

// Handshake-only evidence cannot accept application data or TCP close/reset.
func dohPumpHandshake(path *IpPath, payloadBytes int) (dohPumpHandshakeIdentity, bool) {
	if path.Version != 4 || path.Protocol != IpProtocolTcp || payloadBytes != 0 || !path.Syn || path.Fin || path.Rst {
		return dohPumpHandshakeIdentity{}, false
	}
	value := dohPumpHandshakeIdentity{sourcePort: path.SourcePort, destinationPort: path.DestinationPort,
		sequence: path.SequenceNumber, ack: path.AckSequenceNumber, syn: path.Syn, ackFlag: path.Ack}
	copy(value.source[:], path.SourceIp.To4())
	copy(value.destination[:], path.DestinationIp.To4())
	return value, true
}

// Observe the first real DoH SYN before the later DNS control is generated.
// The embedded policy still decides and no borrowed packet bytes are retained.
type dohPumpReadPolicy struct {
	SecurityPolicy
	entered    chan struct{}
	release    <-chan struct{}
	once       sync.Once
	first      dohPumpHandshakeIdentity
	firstKnown bool
}

// Closing entered publishes the immutable copied first-SYN identity.
func (self *dohPumpReadPolicy) InspectEgress(mode protocol.ProvideMode, path *IpPath, payload []byte) (SecurityPolicyResult, error) {
	if path.Protocol == IpProtocolTcp && path.Syn && !path.Ack && path.DestinationPort == 443 && path.DestinationIp.Equal(net.IPv4(203, 0, 113, 44)) {
		self.once.Do(func() {
			self.first, self.firstKnown = dohPumpHandshake(path, len(payload))
			close(self.entered)
			<-self.release
		})
	}
	return self.SecurityPolicy.InspectEgress(mode, path, payload)
}

// Reuse the checksum-valid SYN fixture, selecting the actual internal DNS
// listener. This input is synthetic; the observed SYNACK is produced by gVisor.
func dohPumpDnsSyn(source, destination netip.Addr) []byte {
	packet := dotTestSyn(source, destination, 7000)
	tcp := packet[Ipv4HeaderSizeWithoutExtensions:]
	binary.BigEndian.PutUint16(tcp[2:4], 53)
	binary.BigEndian.PutUint16(tcp[16:18], 0)
	binary.BigEndian.PutUint16(tcp[16:18], transportChecksum(ipProtocolNumberTcp, source.AsSlice(), destination.AsSlice(), tcp))
	return packet
}

// The first policy barrier holds the pump while this positive queue read
// finds the real local SYNACK. Restore the same packet references in prefix
// order; concurrent cross-flow enqueue order is deliberately not claimed.
func dohPumpRestoreHandshakePrefix(t *testing.T, endpoint *channel.Endpoint, remote dohPumpHandshakeIdentity, localDns netip.Addr, client netip.Addr) uint32 {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	var retained [64]*stack.PacketBuffer
	count, remoteDuplicates := 0, 0
	defer func() {
		for _, packet := range retained[:count] {
			packet.DecRef()
		}
	}()
	for count < len(retained) {
		packet := endpoint.ReadContext(ctx)
		if packet == nil {
			t.Fatal("actual internal TUN did not publish a TCP DNS SYNACK before the positive read watchdog")
		}
		retained[count], count = packet, count+1
		view := packet.ToView()
		path, payload, err := ParseIpPathWithPayload(view.AsSlice())
		var identity dohPumpHandshakeIdentity
		valid := false
		if err == nil {
			identity, valid = dohPumpHandshake(path, len(payload))
		}
		view.Release()
		if !valid {
			t.Fatal("actual TUN prefix contains non-handshake traffic; queue-order premise is unqualified")
		}
		if identity == remote {
			remoteDuplicates++
			continue
		}
		if identity.source != localDns.As4() || identity.destination != client.As4() || identity.sourcePort != 53 || identity.destinationPort != dotTestSourcePort || !identity.syn || !identity.ackFlag || identity.ack != 7001 {
			t.Fatal("actual TUN prefix contains an unknown handshake identity")
		}
		var packets stack.PacketBufferList
		for _, original := range retained[:count] {
			packets.PushBack(original)
		}
		// Endpoint.WritePackets clones successful queue references and borrows
		// each original. The defer releases every caller reference exactly once.
		if written, err := endpoint.WritePackets(packets); err != nil || written != count {
			t.Fatalf("actual handshake prefix could not be fully requeued: packets=%d/%d error=%v", written, count, err)
		}
		t.Logf("DOH_PUMP_QUEUE_PREFIX: packets=%d known_remote_syn_duplicates=%d real_tcp53_synack=1 same_packet_refs=true prefix_order_preserved=true global_enqueue_order_unclaimed=true", count, remoteDuplicates)
		return identity.sequence
	}
	t.Fatal("actual handshake prefix exceeded64retained packets before TCP DNS SYNACK")
	return 0
}

// Actual DoH and local TCP DNS run through the same ordinary constructor and
// reader. The2s watchdog distinguishes the untouched generic5s budget; exact
// barriers and queue identity provide the premise, not virtual-time scheduling.
func TestUpgradeMuxDohPumpTcpDnsReplyPrecedesRemoteReadiness(t *testing.T) {
	for _, size := range []int{60, 256, 2048} {
		MessagePoolReturn(MessagePoolGet(size))
	}
	baselineRoots, baselineBytes := MessagePoolOutstandingCount(), MessagePoolOutstandingByteCount()
	previousViolation := SetMessagePoolViolationHandler(func(err error, _ []byte) { t.Errorf("DoH pump custody: %v", err) })
	t.Cleanup(func() { SetMessagePoolViolationHandler(previousViolation) })
	ctx, cancel := context.WithCancel(context.Background())
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unpark := func() { releaseOnce.Do(func() { close(release) }) }
	var mux *UpgradeMux
	var remote *RemoteUserNatMultiClient
	var queryDone <-chan struct{}
	t.Cleanup(func() {
		unpark()
		cancel()
		if mux != nil {
			mux.Close()
			joinCtx, joinCancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := mux.WaitForPump(joinCtx); err != nil {
				t.Errorf("actual DoH pump did not join: %v", err)
			}
			joinCancel()
		}
		if queryDone != nil {
			select {
			case <-queryDone:
			case <-time.After(5 * time.Second):
				t.Error("actual DoH query did not join")
			}
		}
		if remote != nil {
			joinCtx, joinCancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := remote.CloseAndWait(joinCtx); err != nil {
				t.Errorf("actual DoH remote owner did not join: %v", err)
			}
			joinCancel()
		}
		if roots, bytes := MessagePoolOutstandingCount(), MessagePoolOutstandingByteCount(); roots != baselineRoots || bytes != baselineBytes {
			t.Errorf("DoH pump retained roots after joins: roots=%d/%d bytes=%d/%d", roots, baselineRoots, bytes, baselineBytes)
		}
	})
	policy := &dohPumpReadPolicy{SecurityPolicy: DisableSecurityPolicy(), entered: entered, release: release}
	settings := DefaultMultiClientSettings()
	settings.Log, settings.ProviderProbe = NewNoopLogger(), false
	settings.SecurityPolicyGenerator = func(context.Context, *SecurityPolicyStatsCollector) SecurityPolicy { return policy }
	remote = NewRemoteUserNatMultiClient(ctx, &testingEmptyMultiClientGenerator{}, nil, protocol.ProvideMode_Network, settings)
	muxSettings := DefaultUpgradeMuxSettings()
	muxSettings.Dns.Resolver = &DnsResolverSettings{EnableRemoteDoh: true, DnsUpgradeMaskAddress: DefaultDnsUpgradeMaskAddress, RemoteDohUrlsIpv4: []string{"https://203.0.113.44/dns-query"}}
	muxSettings.Dns.Fallback = nil
	client, server := netip.MustParseAddr("192.0.2.20"), netip.MustParseAddr(DefaultDnsUpgradeMaskAddress)
	reply := make(chan bool, 1)
	var expectedSequence atomic.Uint32
	var err error
	mux, err = NewUpgradeMux(ctx, SourceId(NewId()), protocol.ProvideMode_Network, 5*time.Second,
		func(_ TransferPath, _ protocol.ProvideMode, _ *IpPath, packet []byte) {
			path, payload, err := ParseIpPathWithPayload(packet)
			if err == nil && path.Protocol == IpProtocolTcp && path.DestinationPort == dotTestSourcePort && path.DestinationIp.Equal(net.IP(client.AsSlice())) {
				valid := path.SourcePort == 53 && path.SourceIp.Equal(net.IP(server.AsSlice())) && path.Syn && path.Ack && !path.Rst && !path.Fin && len(payload) == 0 && path.AckSequenceNumber == 7001 && path.SequenceNumber == expectedSequence.Load()
				select {
				case reply <- valid:
				default:
				}
			}
		}, muxSettings, NewNoopLogger())
	if err != nil {
		t.Fatal(err)
	}
	mux.SetUpstreamBatchClient(remote)
	if mux.mux.sendTimeout != 5*time.Second {
		t.Fatal("DoH pump changed generic five-second budget")
	}
	type queryResult struct {
		usable       bool
		bytes        int
		contextError error
	}
	queryTerminal := make(chan queryResult, 1)
	querying := make(chan struct{})
	queryDone = querying
	go func() {
		defer close(querying)
		response, usable := mux.mux.Tun().DohCache().Forward(ctx, dnsmessage.TypeA, "pump-control.example")
		queryTerminal <- queryResult{usable: usable, bytes: len(response), contextError: ctx.Err()}
	}()
	select {
	case <-entered:
	case result := <-queryTerminal:
		t.Fatalf("actual DoH query ended before SYN: usable=%t bytes=%d context_error=%v", result.usable, result.bytes, result.contextError)
	case <-time.After(2 * time.Second):
		t.Fatal("actual tunneled DoH request is pending without first remote SYN")
	}
	if !policy.firstKnown {
		t.Fatal("actual first remote packet lacks exact SYN identity")
	}
	syn := MessagePoolCopy(dohPumpDnsSyn(client, server))
	if !mux.SendPacket(SourceId(NewId()), protocol.ProvideMode_Network, syn, 5*time.Second) {
		MessagePoolReturn(syn)
		t.Fatal("actual local TCP DNS SYN was not claimed")
	}
	expectedSequence.Store(dohPumpRestoreHandshakePrefix(t, mux.mux.Tun().ep.Endpoint, policy.first, mux.dnsTcpLocalAddrFor(4), client))
	unpark()
	select {
	case valid := <-reply:
		if !valid {
			t.Fatal("actual shared pump returned a different TCP DNS reply")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("DOH_PUMP_TCP_DNS_REPLY_WAITED_FOR_REMOTE: actual ready TCP53 reply stayed behind generic remote admission")
	}
}
