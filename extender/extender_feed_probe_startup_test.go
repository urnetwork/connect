package extender

import (
	"context"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

// Bootstrap readiness releases the first probe while the feed is holding its
// sample. The eventual end-of-sample wake must still measure the new records
// while the refresh loop remains parked on the open subscription.
func TestExtenderNetworkClientInitialProbeReadinessKeepsFeedWake(t *testing.T) {
	feed := newFeedServer(t)
	releaseSample := make(chan struct{})
	release := sync.OnceFunc(func() { close(releaseSample) })
	defer release()
	feed.sampleRelease = releaseSample
	fixture := newFeedFixture(t, feed)
	issueTime := time.Now()
	for _, ip := range []string{"198.51.100.51", "198.51.100.52"} {
		feed.sample = append(feed.sample, &protocol.ExtenderFeedFrame{
			Frame: &protocol.ExtenderFeedFrame_Record{
				Record: fixture.signRecord(t, newFeedExtenderKey(t), ip, issueTime, connect.ExtenderCarrierTcp),
			},
		})
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	directory := fixture.newDirectory(t, ctx, nil)
	if _, err := directory.ApplyRecord(
		fixture.signRecord(t, fixture.extenderKey, fixture.extenderIp.String(), issueTime, connect.ExtenderCarrierTcp),
		connect.ExtenderSourceBootstrap,
	); err != nil {
		t.Fatal(err)
	}
	client := fixture.newNetworkClient(t, ctx, directory, func(settings *connect.ExtenderNetworkClientSettings) {
		settings.ExtenderDnsName = ""
		settings.Subscribe = true
		settings.RebootstrapTimeout = time.Hour
		settings.IpVersionSupported = func(ipVersion int) bool { return ipVersion == 4 }
		settings.ProbeWindowCount = 3
		settings.Probe = func(context.Context, *connect.ExtenderCandidate, *connect.ExtenderProbeAttestor) (time.Duration, connect.ExtenderPingOutcome, error) {
			return 20 * time.Millisecond, connect.ExtenderPingUnattested, nil
		}
	})
	select {
	case <-feed.opened:
	case <-ctx.Done():
		t.Fatal("feed request never reached the held sample")
	}
	for {
		status, changed := client.StatusMonitor().Get()
		if !status.LastProbeTime.IsZero() {
			if status.InitialAttemptDone || !status.LastSampleTime.IsZero() {
				t.Fatal("initial probe waited for the held feed sample")
			}
			break
		}
		select {
		case <-changed:
		case <-ctx.Done():
			t.Fatal("bootstrap candidate was not measured while the sample was held")
		}
	}
	if got := len(directory.MeasuredLatencies(4, false)); got != 1 {
		t.Fatalf("measured %d extenders before sample release, want the bootstrap candidate alone", got)
	}
	release()
	for {
		_, changed := directory.ChangeMonitor().Get()
		if len(directory.MeasuredLatencies(4, false)) == 3 {
			break
		}
		select {
		case <-changed:
		case <-ctx.Done():
			t.Fatal("the feed sample did not wake probes for its two new records")
		}
	}
	status := client.Status()
	if !status.FeedConnected || !status.InitialAttemptDone || status.LastSampleTime.IsZero() {
		t.Fatal("new records were not probed on the live subscription")
	}
	for _, ip := range []netip.Addr{fixture.extenderIp, netip.MustParseAddr("198.51.100.51"), netip.MustParseAddr("198.51.100.52")} {
		found := false
		for _, candidate := range directory.Candidates(4, 8) {
			if candidate.Ip == ip && candidate.Latency == 20*time.Millisecond {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s has no completed probe measurement", ip)
		}
	}
}
