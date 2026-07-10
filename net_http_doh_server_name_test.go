package connect

import (
	"context"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/urnetwork/connect/protocol"
)

// answerDnsA unpacks a plaintext dns query and packs an A answer with the
// given address, mirroring the query id and question
func answerDnsA(queryPayload []byte, a [4]byte) ([]byte, bool) {
	var msg dnsmessage.Message
	if err := msg.Unpack(queryPayload); err != nil {
		return nil, false
	}
	if len(msg.Questions) == 0 || msg.Questions[0].Type != dnsmessage.TypeA {
		return nil, false
	}
	resp := dnsmessage.Message{
		Header: dnsmessage.Header{
			ID:                 msg.Header.ID,
			Response:           true,
			RecursionAvailable: true,
		},
		Questions: msg.Questions,
		Answers: []dnsmessage.Resource{{
			Header: dnsmessage.ResourceHeader{
				Name:  msg.Questions[0].Name,
				Type:  dnsmessage.TypeA,
				Class: dnsmessage.ClassINET,
				TTL:   60,
			},
			Body: &dnsmessage.AResource{A: a},
		}},
	}
	respPayload, err := resp.Pack()
	if err != nil {
		return nil, false
	}
	return respPayload, true
}

func dnsQuestionName(payload []byte) (string, bool) {
	var msg dnsmessage.Message
	if err := msg.Unpack(payload); err != nil {
		return "", false
	}
	if len(msg.Questions) == 0 {
		return "", false
	}
	return strings.TrimSuffix(strings.ToLower(msg.Questions[0].Name.String()), "."), true
}

// TestDohServerNameRemoteDnsResolution verifies that a hostname-form doh
// server name resolves over remote plain dns even when EnableRemoteDns is
// false — the one permitted consumer — and that no other name uses remote
// dns while it is disabled.
func TestDohServerNameRemoteDnsResolution(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// a plaintext dns server that answers A queries for the doh server name
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	go func() {
		buf := make([]byte, 2048)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			if respPayload, ok := answerDnsA(buf[:n], [4]byte{203, 0, 113, 50}); ok {
				pc.WriteTo(respPayload, from)
			}
		}
	}()

	var mu sync.Mutex
	dns53Dials := []string{}
	resolvedServerNames := map[string][]netip.Addr{}

	settings := DefaultDohSettings()
	settings.DnsResolverSettings = &DnsResolverSettings{
		EnableRemoteDoh:   true,
		RemoteDohUrlsIpv4: []string{"https://doh.test/dns-query"},
		// EnableRemoteDns deliberately false: only the doh server name may
		// resolve over these
		RemoteDnsIpv4: []string{"192.0.2.53"},
	}
	settings.DohServerResolvedCallback = func(domain string, addrs []netip.Addr) {
		mu.Lock()
		defer mu.Unlock()
		resolvedServerNames[domain] = addrs
	}
	// the "tunnel" dialer: track plaintext :53 dials and redirect them to the
	// fake server; fail everything else (the doh https dial) fast
	settings.DialContextSettings = &DialContextSettings{
		DialContext: func(dialCtx context.Context, network string, addr string) (net.Conn, error) {
			if strings.HasSuffix(addr, ":53") {
				mu.Lock()
				dns53Dials = append(dns53Dials, addr)
				mu.Unlock()
				return (&net.Dialer{}).DialContext(dialCtx, network, pc.LocalAddr().String())
			}
			return nil, net.ErrClosed
		},
	}
	cache := NewDohCache(settings)

	// the doh server name resolves over remote plain dns despite
	// EnableRemoteDns being false
	addrs, authoritative := cache.QueryResult(ctx, "A", "doh.test")
	if !authoritative || len(addrs) == 0 || addrs[0] != netip.MustParseAddr("203.0.113.50") {
		t.Fatalf("doh server name did not resolve over remote dns: addrs=%v authoritative=%t", addrs, authoritative)
	}
	mu.Lock()
	if len(dns53Dials) == 0 || !strings.HasPrefix(dns53Dials[0], "192.0.2.53:") {
		t.Fatalf("expected the resolution to dial the remote dns server, dials=%v", dns53Dials)
	}
	if resolved := resolvedServerNames["doh.test"]; len(resolved) == 0 || resolved[0] != netip.MustParseAddr("203.0.113.50") {
		t.Fatalf("expected the server resolved callback, got %v", resolvedServerNames)
	}
	dialCountAfterServerName := len(dns53Dials)
	mu.Unlock()

	// any other name must not use remote dns while it is disabled: the remote
	// doh stage fails (the dialer refuses), and no stage falls through to
	// plaintext :53
	otherCtx, otherCancel := context.WithTimeout(ctx, 5*time.Second)
	defer otherCancel()
	otherAddrs, otherAuthoritative := cache.QueryResult(otherCtx, "A", "other.test")
	if len(otherAddrs) != 0 || otherAuthoritative {
		t.Fatalf("unexpected resolution for a non server name: addrs=%v authoritative=%t", otherAddrs, otherAuthoritative)
	}
	mu.Lock()
	if len(dns53Dials) != dialCountAfterServerName {
		t.Fatalf("a non server name used remote dns while disabled: dials=%v", dns53Dials)
	}
	mu.Unlock()
}

// TestUpgradeMuxDohServerNameResolvesThroughTunnel verifies the end to end
// path: with a hostname-form doh server configured, a client dns query into
// the mux drives the doh server name resolution as a plaintext dns packet
// that egresses THROUGH the mux to the remote side, is answered there, and
// the resolved server address lands in the mux's ip→hostname reverse index
// (so the block action ignore matcher covers the server name and address).
func TestUpgradeMuxDohServerNameResolvesThroughTunnel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var mu sync.Mutex
	serverNameQueried := false
	var mux *UpgradeMux

	// the fake remote side: watch the tunnel egress for the plaintext dns
	// query of the doh server name and answer it
	upstream := func(source TransferPath, provideMode protocol.ProvideMode, packet []byte, timeout time.Duration) bool {
		ipPath, payload, err := ParseIpPathWithPayload(packet)
		if err != nil {
			return true
		}
		if ipPath.Protocol != IpProtocolUdp || ipPath.DestinationPort != 53 {
			// the doh https connect attempts and other egress pass through
			return true
		}
		name, ok := dnsQuestionName(payload)
		if !ok || name != "doh.test" {
			return true
		}
		if ipPath.DestinationIp.String() != "192.0.2.53" {
			t.Errorf("server name query egressed to %s, want the configured remote dns server", ipPath.DestinationIp)
		}
		respPayload, ok := answerDnsA(payload, [4]byte{203, 0, 113, 99})
		if !ok {
			return true
		}
		mu.Lock()
		serverNameQueried = true
		mu.Unlock()
		respPath := ipPath.Reverse()
		respPacket := ipOosUdpPacket(respPath, respPayload)
		// deliver the remote answer back through the tunnel
		go mux.Receive(source, provideMode, respPath, respPacket)
		return true
	}

	rec := &ipMuxRecorder{}
	settings := DefaultUpgradeMuxSettings()
	settings.Dns.Resolver = &DnsResolverSettings{
		EnableRemoteDoh:   true,
		RemoteDohUrlsIpv4: []string{"https://doh.test/dns-query"},
		// EnableRemoteDns stays false: the server name is the one permitted
		// plaintext consumer
		RemoteDnsIpv4: []string{"192.0.2.53"},
	}
	// no local fallback: resolution must flow through the tunnel only
	settings.Dns.Fallback = nil

	m, err := NewUpgradeMux(ctx, TransferPath{}, protocol.ProvideMode_Network, 0, rec.receive, settings, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	mux = m
	mux.SetUpstream(upstream)

	// a client query for any name forces the doh stage, which needs the doh
	// server address, which drives the server name resolution
	if !mux.SendPacket(TransferPath{}, protocol.ProvideMode_Network, dnsQueryPacket(t, "site.example.test."), 0) {
		t.Fatal("SendPacket returned false; the DNS query was not claimed")
	}

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		queried := serverNameQueried
		mu.Unlock()
		if queried {
			// the resolved server address reaches the reverse index, keyed to
			// the server name (the exclusion linkage)
			names := mux.ServerNames("203.0.113.99")
			found := false
			for _, name := range names {
				if name == "doh.test" {
					found = true
					break
				}
			}
			if found {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	queried := serverNameQueried
	mu.Unlock()
	if !queried {
		t.Fatal("the doh server name dns query never egressed through the mux to the remote side")
	}
	t.Fatal("the resolved doh server address never reached the mux reverse index")
}
