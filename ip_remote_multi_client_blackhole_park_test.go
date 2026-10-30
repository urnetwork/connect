package connect

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// A channel over a no-contract client with one live H1 carrier and nothing in
// flight, with its health passes counted. The returned close joins the client.
func newParkingHealthTestChannel(ctx context.Context, cancel context.CancelFunc) (*multiClientChannel, Transport, *atomic.Int64, func()) {
	destination := NewId()
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), DefaultClientSettings())
	route := make(Route, 64)
	client.ContractManager().AddNoContractPeer(destination)
	transport := &h1SendClientTransportForGroupTest{sendClientTransport: NewSendClientTransport(DestinationId(destination))}
	client.RouteManager().UpdateTransport(transport, []Route{route})
	channel := newPacketTransferTestChannel()
	channel.settings = DefaultMultiClientSettings()
	reliabilitySettings := ReliabilitySettingsFrom(channel.settings)
	channel.reliabilitySettingsFunc = func() *ReliabilitySettings {
		return reliabilitySettings
	}
	channel.ctx, channel.cancel, channel.client, channel.log = ctx, cancel, client, NewNoopLogger()
	channel.args = &multiClientChannelArgs{Destination: RequireMultiHopId(destination)}
	passes := &atomic.Int64{}
	channel.blackholePassForTest = func() {
		passes.Add(1)
	}
	return channel, transport, passes, func() {
		cancel()
		client.CloseAndWait(context.Background())
		for 0 < len(route) {
			MessagePoolReturn(<-route)
		}
	}
}

// Runs the channel's health loop. The returned channel closes when the loop
// ends, and the returned func reads when that was.
func startParkingHealthLoop(channel *multiClientChannel) (<-chan struct{}, func() time.Time) {
	done := make(chan struct{})
	var endTime time.Time
	go func() {
		defer close(done)
		channel.detectBlackhole()
		endTime = time.Now()
	}()
	return done, func() time.Time {
		<-done
		return endTime
	}
}

// A tcp path to one of a few documentation destinations.
func healthParkTestPath(destination byte, syn bool) *IpPath {
	return &IpPath{
		Version:         4,
		Protocol:        IpProtocolTcp,
		SourceIp:        net.ParseIP("10.0.0.2"),
		SourcePort:      40000,
		DestinationIp:   net.IPv4(192, 0, 2, destination),
		DestinationPort: 443,
		Syn:             syn,
	}
}

func healthParkTestEndErr(channel *multiClientChannel) error {
	channel.stateLock.Lock()
	defer channel.stateLock.Unlock()
	return channel.endErr
}

// An idle channel's health loop parks. With nothing outstanding, no send in
// its stats window, no unanswered connect, no quarantine and its carrier up,
// no verdict can mature, so the loop only checks its carrier each
// BlackholeIdlePollInterval. The first send wakes it, and that send's
// no-send-ack verdict still executes within one poll of its bound.
func TestDetectBlackholeParksOnIdleChannel(t *testing.T) {
	// the shared pools start their workers outside the bubble
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		channel, _, passes, closeChannel := newParkingHealthTestChannel(ctx, cancel)
		defer closeChannel()
		pollInterval, idlePollInterval := healthPollIntervals(channel)
		done, endTime := startParkingHealthLoop(channel)
		synctest.Wait()

		idle := 100 * pollInterval
		before := passes.Load()
		time.Sleep(idle)
		synctest.Wait()
		if n, most := passes.Load()-before, int64(idle/idlePollInterval)+1; most < n {
			t.Fatalf("the idle health loop judged %d times over %s, want at most %d", n, idle, most)
		}

		before = passes.Load()
		sendTime := time.Now()
		channel.addSend(64, healthParkTestPath(1, false))
		synctest.Wait()
		if passes.Load() == before {
			t.Fatal("the first send did not wake the parked health loop")
		}
		time.Sleep(channel.noSendAckTimeout() + pollInterval)
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Fatalf("an unacknowledged send was not convicted within one poll of its %s bound", channel.noSendAckTimeout())
		}
		if elapsed := endTime().Sub(sendTime); elapsed < channel.noSendAckTimeout() || channel.noSendAckTimeout()+pollInterval < elapsed {
			t.Fatalf("the unacknowledged send was convicted %s after it was sent, want within one poll of %s", elapsed, channel.noSendAckTimeout())
		}
		if err := healthParkTestEndErr(channel); err == nil || !strings.Contains(err.Error(), string(blackholeNoSendAck)) {
			t.Fatalf("the channel ended with %v, want a no-send-ack verdict", err)
		}
	})
}

// An error on a parked channel ends its health loop, and with it the channel,
// within one poll, as it did before the loop parked.
func TestDetectBlackholeParkedChannelEndsOnError(t *testing.T) {
	// the shared pools start their workers outside the bubble
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		channel, _, _, closeChannel := newParkingHealthTestChannel(ctx, cancel)
		defer closeChannel()
		pollInterval, _ := healthPollIntervals(channel)
		done, endTime := startParkingHealthLoop(channel)
		time.Sleep(10 * pollInterval)
		synctest.Wait()

		errorTime := time.Now()
		channel.addError(errors.New("synthetic frame failure"))
		time.Sleep(pollInterval)
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Fatal("an error on the parked channel did not end its health loop within one poll")
		}
		if elapsed := endTime().Sub(errorTime); pollInterval < elapsed {
			t.Fatalf("the health loop ended %s after the error, want within one poll", elapsed)
		}
		if ctx.Err() == nil {
			t.Fatal("the health loop ended without canceling its channel")
		}
	})
}

// A carrier lost while the loop is parked is found at the next idle poll, and
// its retirement hold runs from there: the channel retires within
// StatsWindowKeepUnhealthyDuration + BlackholeIdlePollInterval of the loss,
// plus one poll. Before the loop parked, the bound was one poll tighter than
// the hold plus a poll.
func TestDetectBlackholeParkedChannelRetiresLostCarrier(t *testing.T) {
	// the shared pools start their workers outside the bubble
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		channel, transport, _, closeChannel := newParkingHealthTestChannel(ctx, cancel)
		defer closeChannel()
		pollInterval, idlePollInterval := healthPollIntervals(channel)
		hold := channel.settings.StatsWindowKeepUnhealthyDuration
		done, endTime := startParkingHealthLoop(channel)
		time.Sleep(10 * pollInterval)
		synctest.Wait()

		lostTime := time.Now()
		channel.client.RouteManager().RemoveTransport(transport)
		time.Sleep(hold + idlePollInterval + pollInterval)
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Fatalf("a carrier lost while parked was not retired within %s", hold+idlePollInterval+pollInterval)
		}
		if elapsed := endTime().Sub(lostTime); elapsed < hold {
			t.Fatalf("the channel retired %s after its carrier was lost, before the %s hold", elapsed, hold)
		}
		if err := healthParkTestEndErr(channel); err != errTransportDownTimeout {
			t.Fatalf("the channel ended with %v, want %v", err, errTransportDownTimeout)
		}
	})
}

// Acknowledged sends keep the loop judging every poll while they are in the
// stats window, so a no-receive-ack verdict on them matures on time: the send
// acknowledgements leave nothing outstanding, but the verdict reads the
// window.
func TestDetectBlackholeJudgesSendsInItsWindow(t *testing.T) {
	// the shared pools start their workers outside the bubble
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		channel, _, passes, closeChannel := newParkingHealthTestChannel(ctx, cancel)
		defer closeChannel()
		pollInterval, _ := healthPollIntervals(channel)
		receiveTimeout := channel.reliabilitySettings().BlackholeReceiveTimeout
		done, endTime := startParkingHealthLoop(channel)
		time.Sleep(10 * pollInterval)
		synctest.Wait()

		// acknowledged sends to two destinations over three stats buckets,
		// so the oldest is past the two partial buckets the verdict skips
		sendTime := time.Now()
		for i := range 3 {
			channel.addSend(64, healthParkTestPath(byte(1+i%2), false))
			channel.addSendAck(64)
			time.Sleep(channel.settings.StatsWindowBucketDuration + time.Millisecond)
		}
		// every poll, whatever the alignment of a parked loop's polls
		judged := receiveTimeout / 2
		before := passes.Load()
		time.Sleep(judged)
		synctest.Wait()
		if n, least := passes.Load()-before, int64(judged/pollInterval)-1; n < least {
			t.Fatalf("the health loop judged %d times over %s with acknowledged sends in its window, want at least %d", n, judged, least)
		}
		time.Sleep(time.Until(sendTime.Add(receiveTimeout + pollInterval)))
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Fatalf("acknowledged sends with nothing received were not convicted within one poll of %s", receiveTimeout)
		}
		if elapsed := endTime().Sub(sendTime); elapsed < receiveTimeout || receiveTimeout+pollInterval < elapsed {
			t.Fatalf("the silent destination was convicted %s after the first send, want within one poll of %s", elapsed, receiveTimeout)
		}
		if err := healthParkTestEndErr(channel); err == nil || !strings.Contains(err.Error(), string(blackholeNoReceiveAck)) {
			t.Fatalf("the channel ended with %v, want a no-receive-ack verdict", err)
		}
	})
}

// A send leaves the stats window a window after its bucket's last event, and
// the loop parks only then: it judges every poll while a send is in the
// window, and parks after.
func TestDetectBlackholeParksAfterSendsLeaveItsWindow(t *testing.T) {
	// the shared pools start their workers outside the bubble
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		channel, _, passes, closeChannel := newParkingHealthTestChannel(ctx, cancel)
		defer closeChannel()
		pollInterval, idlePollInterval := healthPollIntervals(channel)
		window := channel.settings.StatsWindowDuration
		startParkingHealthLoop(channel)
		time.Sleep(10 * pollInterval)
		synctest.Wait()

		// one destination is below the corroboration bar, so nothing convicts
		before := passes.Load()
		channel.addSend(64, healthParkTestPath(1, false))
		channel.addSendAck(64)
		time.Sleep(window - pollInterval)
		synctest.Wait()
		if n, least := passes.Load()-before, int64((window-pollInterval)/pollInterval)-1; n < least {
			t.Fatalf("the health loop judged %d times while a send was in its window, want at least %d", n, least)
		}

		time.Sleep(2 * window)
		synctest.Wait()
		idle := 100 * pollInterval
		before = passes.Load()
		time.Sleep(idle)
		synctest.Wait()
		if n, most := passes.Load()-before, int64(idle/idlePollInterval)+1; most < n {
			t.Fatalf("the health loop judged %d times over %s after its sends left the window, want at most %d", n, idle, most)
		}
	})
}

// An unanswered connect is judged on its own clock, which outlives the stats
// window: the loop keeps judging until it is answered or convicted, so a
// no-receive-syn verdict matures on time even after its syn left the window.
func TestDetectBlackholeJudgesUnansweredConnectPastItsWindow(t *testing.T) {
	// the shared pools start their workers outside the bubble
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		channel, _, passes, closeChannel := newParkingHealthTestChannel(ctx, cancel)
		defer closeChannel()
		pollInterval, _ := healthPollIntervals(channel)
		// a bar past the window, with the comparative cut off
		connectTimeout := 2 * channel.settings.StatsWindowDuration
		channel.settings.BlackholeConnectTimeout = connectTimeout
		reliabilitySettings := ReliabilitySettingsFrom(channel.settings)
		reliabilitySettings.BlackholeConnectComparativeTimeout = 0
		channel.reliabilitySettingsFunc = func() *ReliabilitySettings {
			return reliabilitySettings
		}
		done, endTime := startParkingHealthLoop(channel)
		time.Sleep(10 * pollInterval)
		synctest.Wait()

		synTime := time.Now()
		channel.addSend(64, healthParkTestPath(1, true))
		channel.addSendAck(64)
		// every poll after the syn left the window, whatever the alignment of
		// a parked loop's polls
		window := channel.settings.StatsWindowDuration
		time.Sleep(window + 2*channel.settings.StatsWindowBucketDuration)
		synctest.Wait()
		judged := connectTimeout - time.Since(synTime) - pollInterval
		before := passes.Load()
		time.Sleep(judged)
		synctest.Wait()
		if n, least := passes.Load()-before, int64(judged/pollInterval)-1; n < least {
			t.Fatalf("the health loop judged %d times over %s with its syn out of the window and unanswered, want at least %d", n, judged, least)
		}
		time.Sleep(time.Until(synTime.Add(connectTimeout + pollInterval)))
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Fatalf("an unanswered connect was not convicted within one poll of %s", connectTimeout)
		}
		if elapsed := endTime().Sub(synTime); elapsed < connectTimeout || connectTimeout+pollInterval < elapsed {
			t.Fatalf("the unanswered connect was convicted %s after its syn, want within one poll of %s", elapsed, connectTimeout)
		}
		if err := healthParkTestEndErr(channel); err == nil || !strings.Contains(err.Error(), string(blackholeNoReceiveSyn)) {
			t.Fatalf("the channel ended with %v, want a no-receive-syn verdict", err)
		}
	})
}

func healthPollIntervals(channel *multiClientChannel) (time.Duration, time.Duration) {
	return channel.settings.BlackholePollInterval, channel.settings.BlackholeIdlePollInterval
}
