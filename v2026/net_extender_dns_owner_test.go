// The foreground may leave a DNS generation early, but the client remains
// its owner. Virtual quiescence distinguishes readiness from joined completion.
package connect

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// A custom resolver deliberately waits after cancellation. Close must join
// that tail itself; an external fixture Wait cannot hide an unowned worker.
func TestExtenderDohFirstAnswerReusesGenerationAndCloseJoinsTail(t *testing.T) {
	// This process allocator worker is not owned by the client under test.
	// Prime it outside the bubble, matching the existing packet tests.
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		tailEntered := make(chan struct{})
		tailCanceled := make(chan struct{})
		tailExited := make(chan struct{})
		releaseTail := make(chan struct{})
		feedEntered := make(chan struct{})
		markEntered := sync.OnceFunc(func() { close(tailEntered) })
		markCanceled := sync.OnceFunc(func() { close(tailCanceled) })
		markExited := sync.OnceFunc(func() { close(tailExited) })
		release := sync.OnceFunc(func() { close(releaseTail) })
		markFeed := sync.OnceFunc(func() { close(feedEntered) })
		var queries atomic.Int64

		strategySettings := DefaultClientStrategySettings()
		strategySettings.ConnectSettings.Log = NewNoopLogger()
		strategySettings.ConnectSettings.DialContextSettings = &DialContextSettings{
			DialContext: func(dialCtx context.Context, network string, address string) (net.Conn, error) {
				markFeed()
				<-dialCtx.Done()
				return nil, dialCtx.Err()
			},
			PacketConnFactory: func(context.Context) (net.PacketConn, error) {
				return nil, errors.New("no packet socket in owned DNS fixture")
			},
		}
		// Only the feed transport settings are needed; no unrelated strategy
		// background process or outside-bubble socket is part of this proof.
		strategy := &ClientStrategy{settings: strategySettings}
		directory := NewExtenderDirectory(ctx, DefaultExtenderDirectorySettings())
		defer directory.Close()
		settings := DefaultExtenderNetworkClientSettings()
		settings.Log = NewNoopLogger()
		settings.ExtenderDnsName = ""
		settings.ManualHosts = []string{"ready-owner.example", "held-owner.example"}
		settings.ProbeWindowCount = 0
		settings.HelloTimeout = time.Hour
		settings.DialTimeout = time.Hour
		settings.MinBackoff = time.Hour
		settings.MaxBackoff = time.Hour
		settings.RebootstrapTimeout = time.Hour
		settings.IpVersionSupported = func(int) bool { return true }
		settings.ResolveDns = func(resolveCtx context.Context, name string) ([]netip.Addr, error) {
			queries.Add(1)
			switch name {
			case "ready-owner.example":
				return []netip.Addr{netip.MustParseAddr("192.0.2.124")}, nil
			case "held-owner.example":
				markEntered()
				<-resolveCtx.Done()
				markCanceled()
				<-releaseTail
				markExited()
				return nil, resolveCtx.Err()
			default:
				return nil, errors.New("unexpected synthetic name")
			}
		}
		client := NewExtenderNetworkClient(ctx, strategy, directory, settings)
		defer func() {
			cancel()
			release()
			client.Close()
		}()
		synctest.Wait()
		select {
		case <-tailEntered:
		default:
			t.Fatal("the real manual-host pass did not reach its held tail")
		}
		select {
		case <-feedEntered:
		default:
			t.Error("the real feed consumer waited for the manual-host tail")
		}

		refreshDone := make(chan struct{})
		go func() {
			defer close(refreshDone)
			client.applyManualHosts()
		}()
		synctest.Wait()
		select {
		case <-refreshDone:
		default:
			t.Error("an active generation was not reused for foreground readiness")
		}
		if count := queries.Load(); count != 2 {
			t.Errorf("same generation started %d resolver calls, want exactly 2", count)
		}

		closeDone := make(chan struct{})
		go func() {
			client.Close()
			close(closeDone)
		}()
		synctest.Wait()
		select {
		case <-tailCanceled:
		default:
			t.Error("Close did not cancel the held resolver owner")
		}
		closeReturned := false
		select {
		case <-closeDone:
			closeReturned = true
			t.Error("Close returned before the owned resolver tail exited")
		default:
		}
		select {
		case <-tailExited:
			t.Error("fixture released the held tail before the join assertion")
		default:
		}
		release()
		if !closeReturned {
			<-closeDone
		}
		<-refreshDone
		select {
		case <-tailExited:
		default:
			t.Error("Close completed without joined resolver completion")
		}
	})
}
