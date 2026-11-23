package connect

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"sync/atomic"
	"testing"
	"time"
)

// A feed dial the client's own close ends says nothing about the extender it
// was dialing (sampleCandidate): no failure is recorded against it and it is
// not held, as the probe, the strategy's dialers and the platform transports
// record nothing for a dial their owner canceled. Before, the close recorded
// the dial as the extender's failure and held it for the hold timeout, so the
// client a settings change put in its place (K6) probed and offered every
// candidate but the one the old client had been dialing. A dial that fails on
// its own is still held against the extender.
func TestExtenderNetworkClientCloseHoldsNothingAgainstItsFeedDial(t *testing.T) {
	clock := newTestClock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	directorySettings := DefaultExtenderDirectorySettings()
	directorySettings.Now = clock.Now
	directorySettings.NetworkHosts = []string{testExtenderNetworkHost}
	directory := NewExtenderDirectory(ctx, directorySettings)
	t.Cleanup(directory.Close)
	// the txt answer vouches for the address, so the feed dials it; the keys
	// are in force before either client starts
	rootPrivateKey, rootPublicKey := newTestRootKeyPair(t)
	directory.SetRootKeys(NewExtenderRootKeySet(rootPublicKey))
	ip := netip.MustParseAddr("192.0.2.71")
	txt := testExtenderDnsRecordTxt(t, rootPrivateKey, clock, ip.String())

	// every dial of the address is announced, then waits for its owner to
	// end it, or is refused at once once `refuse` is set
	var refuse atomic.Bool
	dialed := make(chan struct{}, 64)
	strategySettings := DefaultClientStrategySettings()
	strategySettings.ConnectSettings.DialContextSettings = &DialContextSettings{
		DialContext: func(dialCtx context.Context, network string, address string) (net.Conn, error) {
			if host, _, err := net.SplitHostPort(address); err != nil || host != ip.String() {
				return nil, fmt.Errorf("unexpected dial of %s in this test", address)
			}
			select {
			case dialed <- struct{}{}:
			default:
			}
			if refuse.Load() {
				return nil, fmt.Errorf("refused in this test")
			}
			<-dialCtx.Done()
			return nil, dialCtx.Err()
		},
		PacketConnFactory: func(dialCtx context.Context) (net.PacketConn, error) {
			return nil, fmt.Errorf("no packet endpoint in this test")
		},
	}
	clientStrategy := NewClientStrategy(ctx, strategySettings)
	t.Cleanup(clientStrategy.Close)
	newClient := func() *ExtenderNetworkClient {
		settings := DefaultExtenderNetworkClientSettings()
		settings.Now = clock.Now
		settings.ExtenderDnsName = "extender.space.example"
		settings.MinBackoff = time.Millisecond
		settings.MaxBackoff = 10 * time.Millisecond
		settings.DialTimeout = 30 * time.Second
		settings.HelloTimeout = 30 * time.Second
		settings.IpVersionSupported = func(ipVersion int) bool { return ipVersion == 4 }
		settings.ProbeWindowCount = 0
		settings.Hello = func(ctx context.Context) (*ExtenderHelloResult, error) {
			return nil, nil
		}
		settings.Hint = func(ctx context.Context) (*ExtenderHintResult, error) {
			return &ExtenderHintResult{}, nil
		}
		settings.ResolveDns = func(ctx context.Context, name string) ([]netip.Addr, error) {
			return []netip.Addr{ip}, nil
		}
		settings.ResolveDnsTxt = func(ctx context.Context, name string) ([]string, error) {
			return []string{txt}, nil
		}
		return NewExtenderNetworkClient(ctx, clientStrategy, directory, settings)
	}
	entry := func() *ExtenderDirectoryEntry {
		for _, entry := range directory.Snapshot().Entries {
			if entry.Ip == ip {
				return entry
			}
		}
		t.Fatalf("the directory does not hold %s", ip)
		return nil
	}
	offered := func() bool {
		return slices.ContainsFunc(directory.Candidates(4, 8), func(candidate *ExtenderCandidate) bool {
			return candidate.Ip == ip
		})
	}

	// closed while its feed dial is out: Close joins the loop, so whatever the
	// ended dial records has landed when it returns
	networkClient := newClient()
	select {
	case <-dialed:
	case <-time.After(30 * time.Second):
		networkClient.Close()
		t.Fatal("the feed never dialed the vouched address")
	}
	networkClient.Close()
	if closed := entry(); closed.FailureCount != 0 || directory.Snapshot().HoldCount != 0 || !offered() {
		t.Fatalf("the close held its own ended dial against %s: failures=%d holds=%d offered=%t",
			ip, closed.FailureCount, directory.Snapshot().HoldCount, offered())
	}

	// the control: a dial the extender refuses while the client runs is still
	// a failure, and holds the address
	refuse.Store(true)
	networkClient = newClient()
	defer networkClient.Close()
	timeout := time.After(30 * time.Second)
	for {
		_, change := directory.ChangeMonitor().Get()
		if 0 < entry().FailureCount {
			break
		}
		select {
		case <-change:
		case <-timeout:
			t.Fatalf("a refused feed dial was never held against %s", ip)
		}
	}
	if offered() {
		t.Fatalf("%s is still offered after its feed dial was refused", ip)
	}
}
