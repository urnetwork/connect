package connect

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Exercise actual probe framing, Transfer ownership/ACK, provider admission,
// NAT socket owners and return packet parsing. Only the physical route and
// upstream socket are in memory; no host network or wall-clock sleeps occur.
type probeProviderMemoryFixture struct {
	parent   *RemoteUserNatMultiClient
	channel  *multiClientChannel
	nat      *LocalUserNat
	provider *RemoteUserNatProvider
	budget   *TransferMemoryBudget
	root     *TransferMemoryBudget
	close    func()
}

func newProbeProviderMemoryFixture(t *testing.T, target ByteCount, version int, udpDial, tcpDial DialContextFunction) *probeProviderMemoryFixture {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	root := NewTransferMemoryBudget(target)
	budget := NewTransferMemoryBudgetWithParent(max(mib(2), target/10), root)
	settings := DefaultProviderLocalUserNatSettingsWithMemoryTarget(target / 5)
	settings.MemoryBudget, settings.Log = budget, NewNoopLogger()
	// net.Pipe has no pollable fd. Select the portable lifecycle explicitly:
	// starting the platform kqueue/epoll worker would prevent virtual time
	// from advancing. The bounded flow reservation covers this fallback.
	settings.UdpBufferSettings.SocketReadShardCount = 0
	settings.UdpBufferSettings.SharedSocketLifecycle = false
	settings.UdpBufferSettings.DialContextSettings = &DialContextSettings{DialContext: udpDial}
	settings.TcpBufferSettings.DialContextSettings = &DialContextSettings{DialContext: tcpDial}
	nat, err := TryNewLocalUserNat(ctx, "probe-provider", settings)
	if err != nil {
		t.Fatal(err)
	}
	fallbackSettings := DefaultLocalUserNatSettings()
	fallbackSettings.MemoryBudget, fallbackSettings.Log = budget, NewNoopLogger()
	fallback, err := TryNewLocalUserNat(ctx, "device-fallback", fallbackSettings)
	if err != nil {
		t.Fatal(err)
	}
	newClient := func() *Client {
		s := DefaultClientSettings()
		s.Log = NewNoopLogger()
		s.ProtocolVersion, s.SendBufferSettings.ProtocolVersion = version, version
		s.EncryptionSettings.Mode = EncryptionModeOff
		s.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
		return NewClient(ctx, NewId(), NewNoContractClientOob(), s)
	}
	sender, receiver := newClient(), newClient()
	providerSettings := DefaultRemoteUserNatProviderSettingsWithMemoryTarget(target / 5)
	providerSettings.SecurityPolicyGenerator = DisableSecurityPolicyWithStats
	provider, unsubStats, err := TryNewRemoteUserNatProviderWithPacketStats(receiver, nat, providerSettings, func(*RemoteUserNatProvider, *PacketStats) {})
	if err != nil {
		t.Fatal(err)
	}
	provider.localUserNatUnsub()
	parent, channel, _ := probeTestParent(t)
	parent.ctx, parent.log = ctx, NewNoopLogger()
	channel.ctx, channel.log, channel.client = ctx, NewNoopLogger(), sender
	channel.settings.ProtocolVersion = version
	channel.args.Destination = RequireMultiHopId(receiver.ClientId())
	channel.stalled.Store(false)
	sender.ContractManager().AddNoContractPeer(receiver.ClientId())
	route := make(Route, 16)
	sender.RouteManager().UpdateTransport(NewSendClientTransport(DestinationId(receiver.ClientId())), []Route{route})
	var pump sync.WaitGroup
	pump.Add(1)
	go func() {
		defer pump.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case wire := <-route:
				pack := decodeSendPackLifecycleWirePack(t, wire)
				provider.ClientReceive(SourceId(sender.ClientId()), pack.Frames, Peer{ProvideMode: protocol.ProvideMode_Network})
				acknowledgeSendPackLifecycleWirePack(t, sender, receiver.ClientId(), pack)
				MessagePoolReturn(wire)
			}
		}
	}()
	nat.AddReceivePacketCallback(func(_ TransferPath, _ protocol.ProvideMode, _ *IpPath, packet []byte) {
		path, err := ParseIpPath(packet)
		if err != nil {
			t.Errorf("parse provider return: %v", err)
			return
		}
		if probeIngressPath(path) {
			parent.clientReceiveProbePacket(channel, path, packet)
		}
	})
	fixture := &probeProviderMemoryFixture{parent: parent, channel: channel, nat: nat, provider: provider, budget: budget, root: root}
	fixture.close = func() {
		cancel()
		pump.Wait()
		provider.Close()
		unsubStats()
		for _, client := range []*Client{sender, receiver} {
			if err := client.CloseAndWait(context.Background()); err != nil {
				t.Error(err)
			}
		}
		for len(route) > 0 {
			MessagePoolReturn(<-route)
		}
		for _, owner := range []*LocalUserNat{nat, fallback} {
			if err := owner.CloseAndWait(context.Background()); err != nil {
				t.Error(err)
			}
		}
		if budget.UsedByteCount() != 0 || root.UsedByteCount() != 0 {
			t.Errorf("owner cleanup child=%d root=%d", budget.UsedByteCount(), root.UsedByteCount())
		}
	}
	if got, want := budget.UsedByteCount(), 2*natMemoryFixedBytes+natProviderMemoryByteCount(natProviderSourceLimit)+kib(1); got != want {
		t.Fatalf("fixed owner graph=%d, want %d", got, want)
	}
	return fixture
}

func probeOwnerNoDial(context.Context, string, string) (net.Conn, error) {
	return nil, fmt.Errorf("unexpected socket dial")
}

// The default pass has 124 DNS names. Those questions must not leave 124
// independent 31,260-byte socket owners behind after their local waiter ends.
// The 20-MiB sizing case is the iOS 24-MiB absolute-gate profile; 24 and 28
// cover literal sizing and the failed Android run, respectively.
func TestProbeResolverProviderOwnerBudget(t *testing.T) {
	for _, target := range []ByteCount{mib(20), mib(24), mib(28)} {
		for _, version := range []int{1, 2} {
			t.Run(fmt.Sprintf("target-%dMiB/v%d", target/mib(1), version), func(t *testing.T) {
				assertMessagePoolOwnership(t)
				synctest.Test(t, func(t *testing.T) {
					var dials, queries, active atomic.Int64
					var workers sync.WaitGroup
					dial := func(_ context.Context, network, address string) (net.Conn, error) {
						if network != "udp" || address != "1.1.1.1:53" {
							return nil, fmt.Errorf("unexpected resolver %s %s", network, address)
						}
						local, resolver := net.Pipe()
						dials.Add(1)
						active.Add(1)
						workers.Add(1)
						go func() {
							defer workers.Done()
							defer active.Add(-1)
							defer resolver.Close()
							var data [2048]byte
							for {
								n, err := resolver.Read(data[:])
								if err != nil {
									return
								}
								if n < 12 {
									t.Error("short DNS query")
									return
								}
								queries.Add(1)
								answer := append([]byte(nil), data[:n]...)
								binary.BigEndian.PutUint16(answer[2:4], 0x8180)
								binary.BigEndian.PutUint16(answer[6:8], 1)
								answer = append(answer, 0xc0, 12, 0, 1, 0, 1, 0, 0, 0, 60, 0, 4, 203, 0, 113, 9)
								if _, err := resolver.Write(answer); err != nil {
									return
								}
							}
						}()
						return local, nil
					}
					f := newProbeProviderMemoryFixture(t, target, version, dial, probeOwnerNoDial)
					defer workers.Wait()
					defer f.close()
					fixed := f.budget.UsedByteCount()
					if got := f.nat.settings.UdpBufferSettings.IdleTimeout; got != 60*time.Second {
						t.Fatalf("bounded UDP idle=%s", got)
					}
					names := []string{}
					for _, name := range probeHostNames {
						if net.ParseIP(name) == nil {
							names = append(names, name)
						}
					}
					start := time.Now()
					resolved, answered := f.parent.probeResolveNames(f.channel, net.IPv4(1, 1, 1, 1), names, 4*time.Second)
					synctest.Wait()
					retained := f.budget.UsedByteCount() - fixed
					cost := natUdpFlowMemoryByteCount(f.nat.settings.UdpBufferSettings)
					t.Logf("names=%d resolved=%d queries=%d sockets=%d retained=%d fixed=%d cap=%d elapsed=%s", len(names), len(resolved), queries.Load(), dials.Load(), retained, fixed, f.budget.TotalByteCount(), time.Since(start))
					if !answered || len(resolved) != len(names) || queries.Load() != int64(len(names)) {
						t.Errorf("healthy resolver lost coverage: resolved=%d queries=%d want=%d", len(resolved), queries.Load(), len(names))
					}
					if dials.Load() != 1 || active.Load() != 1 || retained != cost {
						t.Errorf("completed DNS questions retained %d sockets/%d bytes; want one socket/%d bytes", active.Load(), retained, cost)
					}
					if f.root.UsedByteCount() != f.budget.UsedByteCount() || f.budget.UsedByteCount() > f.budget.TotalByteCount() {
						t.Error("owner escaped the shared budget")
					}
					time.Sleep(61 * time.Second)
					synctest.Wait()
					if active.Load() != 0 || f.budget.UsedByteCount() != fixed {
						t.Errorf("idle retirement left %d sockets/%d bytes", active.Load(), f.budget.UsedByteCount()-fixed)
					}
				})
			})
		}
	}
}

// A SYN which receives no answer stops being useful at the probe deadline,
// not at the provider's longer dial timeout. Its legitimate RST must cancel
// an already-pending dial and release its fixed owner under the same cap.
func TestProbePendingDialProviderOwnerDeadline(t *testing.T) {
	for _, version := range []int{1, 2} {
		t.Run(fmt.Sprintf("v%d", version), func(t *testing.T) {
			assertMessagePoolOwnership(t)
			synctest.Test(t, func(t *testing.T) {
				var active, canceled, timedOut atomic.Int64
				dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
					active.Add(1)
					defer active.Add(-1)
					timer := time.NewTimer(15 * time.Second)
					defer timer.Stop()
					select {
					case <-ctx.Done():
						canceled.Add(1)
						return nil, ctx.Err()
					case <-timer.C:
						timedOut.Add(1)
						return nil, context.DeadlineExceeded
					}
				}
				f := newProbeProviderMemoryFixture(t, mib(20), version, probeOwnerNoDial, dial)
				defer f.close()
				fixed := f.budget.UsedByteCount()
				start := time.Now()
				result := f.parent.probeExit(f.channel, probeTestTargets(1), 4*time.Second, 0)
				synctest.Wait()
				t.Logf("sent=%d answered=%d pending=%d canceled=%d timeout=%d retained=%d elapsed=%s", result.Sent, result.Answered, active.Load(), canceled.Load(), timedOut.Load(), f.budget.UsedByteCount()-fixed, time.Since(start))
				if result.Sent != 1 || result.Answered != 0 || result.Passed || f.parent.providerQualified(f.channel.probeDestination()) {
					t.Error("a silent dial manufactured positive qualification")
				}
				if active.Load() != 0 || canceled.Load() != 1 || timedOut.Load() != 0 || f.budget.UsedByteCount() != fixed {
					t.Errorf("expired probe still owns provider dial: pending=%d retained=%d", active.Load(), f.budget.UsedByteCount()-fixed)
				}
			})
		})
	}
}
