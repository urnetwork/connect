package connect

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// A multi client with the shipped pause tolerance and none of the other
// optional watchers. A nil detector runs the client's own.
func newPauseDetectorTestMultiClient(detector *SchedulerPauseDetector) *RemoteUserNatMultiClient {
	settings := DefaultMultiClientSettings()
	settings.HeartbeatInterval = 0
	settings.ProviderProbe = false
	settings.IpAssocSettings = nil
	settings.SchedulerPauseDetector = detector
	return NewRemoteUserNatMultiClient(
		context.Background(),
		&testingEmptyMultiClientGenerator{},
		func(source TransferPath, provideMode protocol.ProvideMode, ipPath *IpPath, packet []byte) {
		},
		protocol.ProvideMode_Network,
		settings,
	)
}

func closePauseDetectorTestMultiClient(t *testing.T, multiClient *RemoteUserNatMultiClient) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := multiClient.CloseAndWait(ctx); err != nil {
		t.Errorf("multi client workers did not join: %v", err)
	}
}

func schedulerPauseHoldUntil(multiClient *RemoteUserNatMultiClient) time.Time {
	multiClient.uplinkStateLock.Lock()
	defer multiClient.uplinkStateLock.Unlock()
	return multiClient.schedulerPauseHoldUntil
}

// A client's own detector arms one timer for its life, not one per probe.
func TestSchedulerPauseDetectorReusesOneTimer(t *testing.T) {
	// the shared pools start their workers outside the bubble
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		multiClient := newPauseDetectorTestMultiClient(nil)
		defer closePauseDetectorTestMultiClient(t, multiClient)

		timers := timerAllocationsUnder(t, ").runSchedulerPauseDetector", func() {
			time.Sleep(100 * schedulerPauseProbeInterval)
		})
		if 4 < timers {
			t.Fatalf("the pause detector allocated %d timer objects over 100 probes, want one timer for its life", timers)
		}
	})
}

// Clients given a shared detector run no detector of their own: the one
// shared timer judges every client, each with its own hold, and a closed
// client leaves the detector.
func TestSharedSchedulerPauseDetectorJudgesEveryClient(t *testing.T) {
	// the shared pools start their workers outside the bubble
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		detector := NewSchedulerPauseDetector(ctx)
		defer detector.Close()

		clients := []*RemoteUserNatMultiClient{}
		clientTimers := timerAllocationsUnder(t, ").runSchedulerPauseDetector", func() {
			for range 8 {
				clients = append(clients, newPauseDetectorTestMultiClient(detector))
			}
			time.Sleep(10 * schedulerPauseProbeInterval)
		})
		if clientTimers != 0 {
			t.Fatalf("clients with a shared detector allocated %d detector timer objects of their own, want none", clientTimers)
		}
		AssertEqual(t, 8, detector.ClientCount())

		sharedTimers := timerAllocationsUnder(t, "(*SchedulerPauseDetector).run", func() {
			time.Sleep(100 * schedulerPauseProbeInterval)
		})
		if 2 < sharedTimers {
			t.Fatalf("the shared detector allocated %d timer objects over 100 probes, want its one timer", sharedTimers)
		}
		// waits that came back on time open no hold
		for i, multiClient := range clients {
			if hold := schedulerPauseHoldUntil(multiClient); !hold.IsZero() {
				t.Fatalf("client %d holds until %v with no pause", i, hold)
			}
		}

		// a pause seen by the shared timer opens every client's hold
		armed := time.Now()
		detector.observeSchedulerPause(nil, armed, armed.Add(30*time.Second))
		for i, multiClient := range clients {
			if hold := schedulerPauseHoldUntil(multiClient); !armed.Before(hold) {
				t.Fatalf("client %d did not open its hold for the pause: %v", i, hold)
			}
		}

		// a closed client leaves the detector
		closePauseDetectorTestMultiClient(t, clients[0])
		synctest.Wait()
		AssertEqual(t, 7, detector.ClientCount())
		for _, multiClient := range clients[1:] {
			closePauseDetectorTestMultiClient(t, multiClient)
		}
		synctest.Wait()
		AssertEqual(t, 0, detector.ClientCount())
	})
}

// A client built with the detector off joins no shared detector.
func TestSharedSchedulerPauseDetectorSkipsDisabledClients(t *testing.T) {
	// the shared pools start their workers outside the bubble
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		detector := NewSchedulerPauseDetector(ctx)
		defer detector.Close()

		settings := DefaultMultiClientSettings()
		settings.HeartbeatInterval = 0
		settings.ProviderProbe = false
		settings.IpAssocSettings = nil
		settings.SchedulerPauseTolerance = 0
		settings.SchedulerPauseDetector = detector
		multiClient := NewRemoteUserNatMultiClient(
			context.Background(),
			&testingEmptyMultiClientGenerator{},
			func(source TransferPath, provideMode protocol.ProvideMode, ipPath *IpPath, packet []byte) {
			},
			protocol.ProvideMode_Network,
			settings,
		)
		defer closePauseDetectorTestMultiClient(t, multiClient)
		AssertEqual(t, 0, detector.ClientCount())
	})
}
