package connect

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// a recorder for sent (upstream) and received (downstream) packets
type ipMuxRecorder struct {
	mu       sync.Mutex
	sent     [][]byte
	received [][]byte
}

func (self *ipMuxRecorder) upstream(source TransferPath, provideMode protocol.ProvideMode, packet []byte, timeout time.Duration) bool {
	self.mu.Lock()
	defer self.mu.Unlock()
	self.sent = append(self.sent, append([]byte{}, packet...))
	return true
}

func (self *ipMuxRecorder) receive(source TransferPath, provideMode protocol.ProvideMode, ipPath *IpPath, packet []byte) {
	self.mu.Lock()
	defer self.mu.Unlock()
	self.received = append(self.received, append([]byte{}, packet...))
}

func (self *ipMuxRecorder) counts() (int, int) {
	self.mu.Lock()
	defer self.mu.Unlock()
	return len(self.sent), len(self.received)
}

func TestIpMuxPassthrough(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tun, err := CreateTunWithDefaults(ctx)
	if err != nil {
		t.Fatal(err)
	}

	rec := &ipMuxRecorder{}
	// onSend nil => pure pass-through
	mux := NewIpMux(ctx, tun, TransferPath{}, protocol.ProvideMode_Network, 0, nil, nil, rec.receive, nil)
	defer mux.Close()
	mux.SetUpstream(rec.upstream)

	// send path: not claimed => forwarded to upstream verbatim
	pkt := []byte("a-send-packet")
	if !mux.SendPacket(TransferPath{}, protocol.ProvideMode_Network, pkt, 0) {
		t.Fatal("SendPacket returned false")
	}
	if sent, _ := rec.counts(); sent != 1 {
		t.Fatalf("upstream got %d packets, want 1", sent)
	}

	// receive path: external destination => dispatched downstream
	external := &IpPath{Version: 4, Protocol: IpProtocolUdp, DestinationIp: net.ParseIP("8.8.8.8"), DestinationPort: 443}
	mux.Receive(TransferPath{}, protocol.ProvideMode_Network, external, []byte("a-receive-packet"))
	if _, received := rec.counts(); received != 1 {
		t.Fatalf("downstream got %d packets, want 1", received)
	}

	// receive path: destination is the mux's own tun address => delivered into the
	// internal stack, NOT downstream
	addrs := tun.LocalAddresses()
	if len(addrs) == 0 {
		t.Fatal("tun has no local address")
	}
	toMux := &IpPath{Version: 4, Protocol: IpProtocolUdp, DestinationIp: net.IP(addrs[0].AsSlice()), DestinationPort: 53}
	mux.Receive(TransferPath{}, protocol.ProvideMode_Network, toMux, make([]byte, 40))
	if _, received := rec.counts(); received != 1 {
		t.Fatalf("downstream got %d packets after mux-addressed receive, want still 1", received)
	}
}
