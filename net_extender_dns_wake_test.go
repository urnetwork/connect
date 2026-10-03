// A feed pass may leave discovery before its first new answer when an older
// candidate exists. A changed publication still has to wake the waiting pass.
package connect

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// The old candidate fails before discovery finishes. The first new candidate
// must trigger a real feed dial without consuming the reconnect timer.
func TestExtenderDohFirstAnswerNewPublicationWakesBackoff(t *testing.T) {
	// This process allocator worker is not owned by the client under test.
	// Prime it outside the bubble, matching the existing packet tests.
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		oldAddr := netip.MustParseAddr("192.0.2.125")
		newAddr := netip.MustParseAddr("192.0.2.126")
		oldDial := make(chan struct{})
		newDial := make(chan struct{})
		resolverEntered := make(chan struct{})
		releaseResolver := make(chan struct{})
		markOld := sync.OnceFunc(func() { close(oldDial) })
		markNew := sync.OnceFunc(func() { close(newDial) })
		markResolver := sync.OnceFunc(func() { close(resolverEntered) })
		release := sync.OnceFunc(func() { close(releaseResolver) })

		strategySettings := DefaultClientStrategySettings()
		strategySettings.ConnectSettings.Log = NewNoopLogger()
		strategySettings.ConnectSettings.DialContextSettings = &DialContextSettings{
			DialContext: func(dialCtx context.Context, network string, address string) (net.Conn, error) {
				host, _, err := net.SplitHostPort(address)
				if err != nil {
					return nil, errors.New("invalid synthetic feed address")
				}
				if host == oldAddr.String() {
					markOld()
					return nil, errors.New("synthetic old candidate unavailable")
				}
				if host != newAddr.String() {
					return nil, errors.New("unexpected synthetic feed address")
				}
				markNew()
				<-dialCtx.Done()
				return nil, dialCtx.Err()
			},
			PacketConnFactory: func(context.Context) (net.PacketConn, error) {
				return nil, errors.New("no packet socket in publication fixture")
			},
		}
		strategy := &ClientStrategy{settings: strategySettings}
		directory := NewExtenderDirectory(ctx, DefaultExtenderDirectorySettings())
		defer directory.Close()
		directory.AddManual(oldAddr)
		settings := DefaultExtenderNetworkClientSettings()
		settings.Log = NewNoopLogger()
		settings.ExtenderDnsName = ""
		settings.ManualHosts = []string{"late-publication.example"}
		settings.ProbeWindowCount = 0
		settings.HelloTimeout = time.Hour
		settings.DialTimeout = time.Hour
		settings.MinBackoff = time.Hour
		settings.MaxBackoff = time.Hour
		settings.RebootstrapTimeout = time.Hour
		settings.IpVersionSupported = func(int) bool { return true }
		settings.ResolveDns = func(resolveCtx context.Context, name string) ([]netip.Addr, error) {
			markResolver()
			select {
			case <-releaseResolver:
				return []netip.Addr{newAddr}, nil
			case <-resolveCtx.Done():
				return nil, resolveCtx.Err()
			}
		}
		client := NewExtenderNetworkClient(ctx, strategy, directory, settings)
		defer func() {
			cancel()
			release()
			client.Close()
		}()
		started := time.Now()
		synctest.Wait()
		select {
		case <-resolverEntered:
		default:
			t.Fatal("discovery never reached its publication barrier")
		}
		select {
		case <-oldDial:
		default:
			t.Error("an existing manual candidate waited behind new discovery")
		}
		if !client.Status().InitialAttemptDone {
			t.Error("old feed attempt did not finish before the new publication")
		}

		release()
		synctest.Wait()
		select {
		case <-newDial:
		default:
			t.Error("new usable DNS publication did not wake the feed from backoff")
		}
		if !time.Now().Equal(started) {
			t.Errorf("first new candidate consumed a timer: %s", time.Since(started))
		}
		// A refresh of the same answer must not create a wake edge which
		// would discard the feed loop's ordinary reconnect backoff.
		wake := client.wakeMonitor.NotifyChannel()
		client.applyManualHosts()
		synctest.Wait()
		select {
		case <-wake:
			t.Error("unchanged DNS inventory emitted a feed retry wake")
		default:
		}
	})
}
