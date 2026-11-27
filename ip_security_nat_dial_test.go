package connect

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"syscall"
	"testing"
	"testing/synctest"

	"github.com/urnetwork/connect/protocol"
)

// Move a public destination onto the provider after admission. Both flow
// constructors must consult the new snapshot before invoking even a custom dial.
func TestProviderNatRechecksNumericDestinationAtDial(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		nets, _ := newTestProviderLocalNetworks(t, newTestProviderLocalRead(t, "192.0.2.11/24"))
		release := nets.acquire()
		defer release()
		settings := DefaultProviderLocalUserNatSettingsWithMemoryTargets(0, 0)
		settings.Log = NewNoopLogger()
		settings.TcpBufferSettings.providerDialPolicy = newProviderNatDialPolicy(nets)
		settings.UdpBufferSettings.providerDialPolicy = settings.TcpBufferSettings.providerDialPolicy
		target := netip.MustParseAddr("203.0.113.89")
		atSocket := false
		refusedAtSocket := map[string]bool{}
		native := providerEgressDialer(&net.Dialer{ControlContext: func(context.Context, string, string, syscall.RawConn) error {
			nets.setObservedAddresses([]netip.Addr{target})
			return nil
		}})
		dialed := map[string]int{}
		dial := &DialContextSettings{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			dialed[network]++
			if atSocket {
				// Invoke the real pre-connect callback after dial preparation
				// changes the snapshot. No socket or packet leaves this fixture.
				var refused *ProviderDialRefusedError
				refusedAtSocket[network] = errors.As(native.ControlContext(ctx, network, address, &providerEgressFailedRawConn{}), &refused)
			}
			return nil, context.Canceled
		}}
		settings.TcpBufferSettings.DialContextSettings = dial
		settings.UdpBufferSettings.DialContextSettings = dial
		nat := NewLocalUserNat(ctx, "numeric destination", settings)
		defer func() { cancel(); nat.CloseAndWait(context.Background()) }()
		if err := nets.inspectDial(netip.AddrPortFrom(target, 443).String(), DefaultProviderLocalSecurityPolicySettings()); err != nil {
			t.Fatal(err)
		}
		nets.setObservedAddresses([]netip.Addr{target})
		for i, mode := range []protocol.ProvideMode{protocol.ProvideMode_Public, protocol.ProvideMode_Network, protocol.ProvideMode_Public} {
			if i == 2 {
				nets.setObservedAddresses(nil)
			}
			for _, p := range []IpProtocol{IpProtocolTcp, IpProtocolUdp} {
				path := &IpPath{Version: 4, Protocol: p, SourceIp: net.IPv4(198, 51, 100, 9).To4(), SourcePort: 42001 + i, DestinationIp: net.IP(target.AsSlice()), DestinationPort: 443}
				wire := providerEgressTestPacket(path, 64)
				if !nat.SendPacket(SourceId(NewId()), mode, wire, 0) {
					MessagePoolReturn(wire)
					t.Fatal("NAT refused fixture input")
				}
				synctest.Wait()
			}
			if dialed["tcp"] != i || dialed["udp"] != i {
				t.Errorf("step %d called tcp %d / udp %d times", i, dialed["tcp"], dialed["udp"])
			}
		}
		atSocket = true
		for _, p := range []IpProtocol{IpProtocolTcp, IpProtocolUdp} {
			nets.setObservedAddresses(nil)
			path := &IpPath{Version: 4, Protocol: p, SourceIp: net.IPv4(198, 51, 100, 9).To4(), SourcePort: 42005, DestinationIp: net.IP(target.AsSlice()), DestinationPort: 443}
			wire := providerEgressTestPacket(path, 64)
			if !nat.SendPacket(SourceId(NewId()), protocol.ProvideMode_Public, wire, 0) {
				MessagePoolReturn(wire)
				t.Fatal("NAT refused pre-connect fixture")
			}
			synctest.Wait()
		}
		if !refusedAtSocket["tcp"] || !refusedAtSocket["udp"] {
			t.Errorf("native pre-connect refusal after snapshot change = %v", refusedAtSocket)
		}
	})
}

// Adopting a generic NAT for the provider role must protect its ordinary
// sockets and the platform-specific ICMP egress factory. No fixture opens a socket.
func TestAdoptedProviderNatGuardsAllEgressFactories(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		nets, _ := newTestProviderLocalNetworks(t, newTestProviderLocalRead(t, "192.0.2.8/24"))
		settings := DefaultLocalUserNatSettingsWithBufferSize(2)
		settings.Log = NewNoopLogger()
		calls := 0
		dial := &DialContextSettings{DialContext: func(context.Context, string, string) (net.Conn, error) { calls++; return nil, context.Canceled }}
		settings.TcpBufferSettings.DialContextSettings = dial
		settings.UdpBufferSettings.DialContextSettings = dial
		settings.IcmpBufferSettings.newEgressForTest = func(context.Context, int, net.IP, *IcmpBufferSettings) (icmpEgress, error) {
			calls++
			return nil, context.Canceled
		}
		nat := NewLocalUserNat(ctx, "adopted", settings)
		cs := closeWaitClientSettings()
		cs.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
		c := NewClient(ctx, NewId(), NewNoContractClientOob(), cs)
		ps := DefaultRemoteUserNatProviderSettings()
		ps.SecurityPolicyGenerator = func(ctx context.Context, stats *SecurityPolicyStatsCollector) SecurityPolicy {
			return newTestProviderSecurityPolicy(ctx, nets, stats)
		}
		provider := NewRemoteUserNatProvider(c, nat, ps)
		defer func() {
			cancel()
			provider.Close()
			c.CloseAndWait(context.Background())
			nat.CloseAndWait(context.Background())
		}()
		dest := "192.0.2.9"
		send := func(wire []byte) {
			if !nat.SendPacket(SourceId(NewId()), protocol.ProvideMode_Public, wire, 0) {
				MessagePoolReturn(wire)
				t.Fatal("NAT fixture admission")
			}
			synctest.Wait()
		}
		for _, p := range []IpProtocol{IpProtocolTcp, IpProtocolUdp} {
			send(providerEgressTestPacket(&IpPath{Version: 4, Protocol: p, SourceIp: net.IPv4(198, 51, 100, 9).To4(), SourcePort: 43000, DestinationIp: net.ParseIP(dest).To4(), DestinationPort: 443}, 64))
		}
		send(MessagePoolCopy(testingIcmp4EchoPacket("198.51.100.9", dest, 7, 1, []byte("echo"))))
		if calls != 0 {
			t.Errorf("adopted provider invoked %d destination factories", calls)
		}
	})
}
