// Root keys that hello installs while the feed stream is open or being dialed
// (open bug P052).
//
// Hello is read beside the network client's refresh pass, so its keys can
// land while the pass dials an extender or holds the subscription. They judge
// every frame after them, and they end the stream once its sample is in, so
// the next pass samples under them: the records the old keys refused are
// judged again, and the client does not stay with an extender that keys no
// longer in force chose.

package extender

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

// The first sample does not wait for hello, and the keys hello installs while
// the subscription is open end it at its next frame, after which the client
// samples again under them.
func TestExtenderNetworkClientSamplesAgainWhenHelloInstallsKeys(t *testing.T) {
	issueTime := time.Now()
	feed := newFeedServer(t)
	fixture := newFeedFixture(t, feed)

	sampledIp := netip.MustParseAddr("198.51.100.60")
	feed.sample = []*protocol.ExtenderFeedFrame{
		{Frame: &protocol.ExtenderFeedFrame_Record{
			Record: fixture.signRecord(
				t,
				newFeedExtenderKey(t),
				sampledIp.String(),
				issueTime,
				connect.ExtenderCarrierTcp,
			),
		}},
	}

	// the key the operator rotates to, which the directory does not hold
	rotatedSeed, err := connect.NewExtenderKeySeed()
	if err != nil {
		t.Fatal(err)
	}
	rotatedPrivateKey, err := connect.ExtenderPrivateKeyFromSeed(rotatedSeed)
	if err != nil {
		t.Fatal(err)
	}
	rotatedPublicKey := rotatedPrivateKey.Public().(ed25519.PublicKey)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	directory := fixture.newDirectory(t, ctx, nil)
	// the fixture's own record makes it a verified candidate on its test port
	if _, err := directory.ApplyRecord(
		fixture.signRecord(t, fixture.extenderKey, fixture.extenderIp.String(), issueTime, connect.ExtenderCarrierTcp),
		connect.ExtenderSourceBootstrap,
	); err != nil {
		t.Fatal(err)
	}

	releaseHello := make(chan struct{})
	fixture.newNetworkClient(t, ctx, directory, func(settings *connect.ExtenderNetworkClientSettings) {
		settings.ResolveDns = func(ctx context.Context, name string) ([]netip.Addr, error) {
			return nil, nil
		}
		settings.ResolveDnsTxt = func(ctx context.Context, name string) ([]string, error) {
			return nil, nil
		}
		settings.Hello = func(ctx context.Context) (*connect.ExtenderHelloResult, error) {
			select {
			case <-releaseHello:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			// the rotation: the key in force and the one it rotates to
			return &connect.ExtenderHelloResult{
				RootPublicKeyHexes: []string{
					hex.EncodeToString(fixture.rootPublic),
					hex.EncodeToString(rotatedPublicKey),
				},
			}, nil
		}
	})

	// the first sample lands and the subscription opens while hello is out
	select {
	case <-feed.opened:
	case <-time.After(10 * time.Second):
		t.Fatal("the first sample waited for hello")
	}
	waitForDirectory(t, directory, func(snapshot *connect.ExtenderDirectorySnapshot) bool {
		for _, entry := range snapshot.Entries {
			if entry.Ip == sampledIp && entry.State == connect.ExtenderStateActive {
				return true
			}
		}
		return false
	}, "the sampled record never reached the directory")

	// hello answers with the rotation: the open stream ends at its next frame,
	// a keepalive at this fixture's cadence, and the client samples again
	close(releaseHello)
	select {
	case <-feed.opened:
	case <-time.After(10 * time.Second):
		t.Fatal("the feed stream stayed open after hello installed new root keys")
	}
	if !directory.RootKeys().Equal(connect.NewExtenderRootKeySet(fixture.rootPublic, rotatedPublicKey)) {
		t.Fatal("the keys in force are not the ones hello answered with")
	}
}

// Keys that hello installs while the feed dial is out end the stream once its
// sample is in, as keys installed after it opened do: the extender was chosen
// under the keys the install replaced, so the client samples again under the
// new ones rather than keeping the subscription.
func TestExtenderNetworkClientSamplesAgainWhenHelloInstallsKeysDuringTheDial(t *testing.T) {
	issueTime := time.Now()
	feed := newFeedServer(t)
	fixture := newFeedFixture(t, feed)

	sampledIp := netip.MustParseAddr("198.51.100.61")
	feed.sample = []*protocol.ExtenderFeedFrame{
		{Frame: &protocol.ExtenderFeedFrame_Record{
			Record: fixture.signRecord(
				t,
				newFeedExtenderKey(t),
				sampledIp.String(),
				issueTime,
				connect.ExtenderCarrierTcp,
			),
		}},
	}

	// the key the operator rotates to, which the directory does not hold
	rotatedSeed, err := connect.NewExtenderKeySeed()
	if err != nil {
		t.Fatal(err)
	}
	rotatedPrivateKey, err := connect.ExtenderPrivateKeyFromSeed(rotatedSeed)
	if err != nil {
		t.Fatal(err)
	}
	rotatedPublicKey := rotatedPrivateKey.Public().(ed25519.PublicKey)
	gossipPeerId := "12D3KooWtestrotatingoperator"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	directory := fixture.newDirectory(t, ctx, nil)
	// the fixture's own record makes it a verified candidate on its test port
	if _, err := directory.ApplyRecord(
		fixture.signRecord(t, fixture.extenderKey, fixture.extenderIp.String(), issueTime, connect.ExtenderCarrierTcp),
		connect.ExtenderSourceBootstrap,
	); err != nil {
		t.Fatal(err)
	}

	// the first dial of the extender is held until the test lets it go on
	extenderAddr := net.JoinHostPort(
		fixture.extenderIp.String(),
		strconv.Itoa(fixture.extenderPorts[connect.ExtenderCarrierTcp]),
	)
	dialHeld := make(chan struct{})
	releaseDial := make(chan struct{})
	var holdOnce sync.Once
	strategySettings := connect.DefaultClientStrategySettings()
	strategySettings.ConnectSettings = *fixture.extender.connectSettings()
	strategySettings.ConnectSettings.DialNetworkHook = func(network string, addr string) {
		if addr != extenderAddr {
			return
		}
		holdOnce.Do(func() {
			close(dialHeld)
			select {
			case <-releaseDial:
			case <-ctx.Done():
			}
		})
	}
	strategySettings.ExtenderDirectory = directory
	clientStrategy := connect.NewClientStrategy(ctx, strategySettings)
	t.Cleanup(clientStrategy.Close)

	releaseHello := make(chan struct{})
	settings := connect.DefaultExtenderNetworkClientSettings()
	settings.ExtenderDnsName = "extender.space.example"
	settings.MinBackoff = 10 * time.Millisecond
	settings.MaxBackoff = 200 * time.Millisecond
	settings.DialTimeout = 20 * time.Second
	settings.HelloTimeout = 20 * time.Second
	settings.IpVersionSupported = func(ipVersion int) bool { return true }
	settings.ResolveDns = func(ctx context.Context, name string) ([]netip.Addr, error) {
		return nil, nil
	}
	settings.ResolveDnsTxt = func(ctx context.Context, name string) ([]string, error) {
		return nil, nil
	}
	settings.Hello = func(ctx context.Context) (*connect.ExtenderHelloResult, error) {
		select {
		case <-releaseHello:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		// the rotation: the key in force and the one it rotates to
		return &connect.ExtenderHelloResult{
			RootPublicKeyHexes: []string{
				hex.EncodeToString(fixture.rootPublic),
				hex.EncodeToString(rotatedPublicKey),
			},
			GossipPeerId: gossipPeerId,
		}, nil
	}
	networkClient := connect.NewExtenderNetworkClient(ctx, clientStrategy, directory, settings)
	t.Cleanup(networkClient.Close)

	select {
	case <-dialHeld:
	case <-time.After(10 * time.Second):
		t.Fatal("the first pass never dialed the extender")
	}

	// hello answers with the rotation while the dial is out; the status names
	// the gossip identity once the keys it came with are in force
	close(releaseHello)
	timeout := time.After(10 * time.Second)
	for {
		status, changed := networkClient.StatusMonitor().Get()
		if status.GossipPeerId == gossipPeerId {
			break
		}
		select {
		case <-changed:
		case <-timeout:
			t.Fatal("hello never answered while the dial was out")
		}
	}
	if !directory.RootKeys().Equal(connect.NewExtenderRootKeySet(fixture.rootPublic, rotatedPublicKey)) {
		t.Fatal("the keys in force are not the ones hello answered with")
	}

	// the held dial opens the stream and its sample lands, and the stream
	// ends once it has: the next pass opens another
	close(releaseDial)
	select {
	case <-feed.opened:
	case <-time.After(10 * time.Second):
		t.Fatal("the held dial never opened the feed stream")
	}
	select {
	case <-feed.opened:
	case <-time.After(10 * time.Second):
		t.Fatal("the stream to the extender the replaced keys chose stayed open")
	}
}
