// A feed-only dial barrier must not be consumed by the independently owned
// startup probe. The existing loop seams force that probe ahead of the feed.
package extender

import (
	"context"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// Completing a probe while the refresh loop is held proves whether the address
// hook belongs to a feed dial. No absence timeout or scheduler ordering decides it.
func TestExtenderNetworkClientRootKeyDialBarrierIsNotConsumedByProbe(t *testing.T) {
	feed := newFeedServer(t)
	fixture := newFeedFixture(t, feed)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	directory := fixture.newDirectory(t, ctx, nil)

	extenderAddr := net.JoinHostPort(
		fixture.extenderIp.String(),
		strconv.Itoa(fixture.extenderPorts[connect.ExtenderCarrierTcp]),
	)
	dialSeen := make(chan struct{})
	var dialOnce sync.Once
	strategySettings := connect.DefaultClientStrategySettings()
	strategySettings.ConnectSettings = *fixture.extender.connectSettings()
	strategySettings.ConnectSettings.DialNetworkHook = func(network string, addr string) {
		if addr == extenderAddr {
			dialOnce.Do(func() { close(dialSeen) })
		}
	}
	strategySettings.ExtenderDirectory = directory
	strategy := connect.NewClientStrategy(ctx, strategySettings)
	t.Cleanup(strategy.Close)

	feedParked := make(chan struct{})
	releaseFeed := make(chan time.Time)
	release := sync.OnceFunc(func() { close(releaseFeed) })
	defer release()
	probeParked := make(chan struct{})
	releaseProbe := make(chan time.Time)
	probeCompleted := make(chan struct{})
	settings := feedRootKeyDialSettings()
	settings.IpVersionSupported = func(ipVersion int) bool { return ipVersion == 4 }
	// Suppress only the unrelated periodic resume check in this ordering witness.
	settings.ResumeCheckTimeout = 0
	feedWaitCount := 0
	settings.PassAfter = func(wait time.Duration) <-chan time.Time {
		feedWaitCount += 1
		if feedWaitCount != 1 {
			return nil
		}
		close(feedParked)
		select {
		case <-releaseFeed:
		case <-ctx.Done():
		}
		return releaseFeed
	}
	probeWaitCount := 0
	settings.ProbeAfter = func(wait time.Duration) <-chan time.Time {
		probeWaitCount += 1
		switch probeWaitCount {
		case 1:
			close(probeParked)
			return releaseProbe
		case 2:
			close(probeCompleted)
		}
		return nil
	}
	client := connect.NewExtenderNetworkClient(ctx, strategy, directory, settings)
	t.Cleanup(client.Close)

	wait := func(reached <-chan struct{}, description string) {
		t.Helper()
		select {
		case <-reached:
		case <-ctx.Done():
			t.Fatalf("ordering setup did not reach %s: %v", description, ctx.Err())
		}
	}
	// Both initial passes saw an empty directory. The feed remains inside its
	// wait callback even if a later directory or hello update wakes its select.
	wait(feedParked, "empty feed pass")
	wait(probeParked, "empty probe pass")
	if _, err := directory.ApplyRecord(
		fixture.signRecord(t, fixture.extenderKey, fixture.extenderIp.String(), time.Now(), connect.ExtenderCarrierTcp),
		connect.ExtenderSourceBootstrap,
	); err != nil {
		t.Fatal(err)
	}
	close(releaseProbe)
	wait(probeCompleted, "completed candidate probe")

	status := client.Status()
	if status.LastProbeTime.IsZero() || !status.LastSampleTime.IsZero() || status.FeedConnected {
		t.Fatalf("probe-before-feed ordering was not established: %+v", status)
	}
	if count := len(directory.MeasuredLatencies(4, false)); count != 1 {
		t.Fatalf("completed probe measured %d candidates, want the one fixture candidate", count)
	}
	select {
	case <-feed.opened:
		t.Fatal("feed opened while its refresh loop was held")
	default:
	}
	select {
	case <-dialSeen:
		t.Error("background probe consumed the feed-only dial barrier before any feed request")
	default:
	}

	// Even the expected-red control releases and joins normally. The feed must
	// still exercise the actual dial path; replacing all network work cannot pass.
	release()
	select {
	case <-feed.opened:
	case <-ctx.Done():
		t.Fatal("released feed did not open its actual stream")
	}
	select {
	case <-dialSeen:
	default:
		t.Fatal("actual feed did not reach its dial barrier")
	}
	cancel()
	client.Close()
}
