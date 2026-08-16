package connect

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type blockingP2pRouteManager struct {
	updateStarted chan struct{}
	releaseUpdate chan struct{}
	startOnce     sync.Once
	active        atomic.Bool
	removeCount   atomic.Int32
}

func (self *blockingP2pRouteManager) UpdateTransport(Transport, []Route) {
	self.startOnce.Do(func() { close(self.updateStarted) })
	<-self.releaseUpdate
	self.active.Store(true)
}

func (self *blockingP2pRouteManager) RemoveTransport(Transport) {
	self.active.Store(false)
	self.removeCount.Add(1)
}

type p2pRouteTestTransport struct {
	id Id
}

func (self *p2pRouteTestTransport) TransportId() Id { return self.id }
func (self *p2pRouteTestTransport) Priority() int   { return TransportMaxPriority }
func (self *p2pRouteTestTransport) Weight() float32 { return 0 }
func (self *p2pRouteTestTransport) CanEvalRouteWeight(*RouteStats, map[Transport]*RouteStats) bool {
	return true
}
func (self *p2pRouteTestTransport) RouteWeight(*RouteStats, map[Transport]*RouteStats) float32 {
	return 1
}
func (self *p2pRouteTestTransport) MatchesSend(TransferPath) bool    { return true }
func (self *p2pRouteTestTransport) MatchesReceive(TransferPath) bool { return true }
func (self *p2pRouteTestTransport) Downgrade(TransferPath)           {}

func TestP2pLateConnectedCallbackCannotRestoreCanceledRoute(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	manager := &blockingP2pRouteManager{
		updateStarted: make(chan struct{}),
		releaseUpdate: make(chan struct{}),
	}
	transport := &p2pRouteTestTransport{id: NewId()}
	done := make(chan struct{})
	var installed atomic.Bool
	go func() {
		installed.Store(updateP2pConnectionRoute(ctx, manager, transport, make(Route), true))
		close(done)
	}()

	select {
	case <-manager.updateStarted:
	case <-time.After(time.Second):
		t.Fatal("connected callback did not enter route update")
	}
	// Teardown wins while the callback is parked inside the route manager.
	// When the old update finally returns, its post-update check must remove
	// the route again rather than resurrecting the retired connection.
	cancel()
	close(manager.releaseUpdate)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("late connected callback did not finish")
	}
	if manager.active.Load() {
		t.Fatal("late connected callback restored a route after cancellation")
	}
	if manager.removeCount.Load() != 1 {
		t.Fatalf("compensating route removals=%d, want 1", manager.removeCount.Load())
	}
	if installed.Load() {
		t.Fatal("late connected callback reported a canceled route as installed")
	}
}

// Duplicate connection edges leave each active route gauge exact and return
// it to zero after removal.
func TestP2pActiveRouteStatsTrackIdempotentTransitions(t *testing.T) {
	stats := &P2pDataPlaneStats{}
	states := []P2pRouteState{}
	transport := &P2pTransport{
		peerId:   NewId(),
		streamId: NewId(),
		settings: &P2pTransportSettings{
			DataPlaneStats: stats,
			RouteStateObserver: func(state P2pRouteState) {
				states = append(states, state)
			},
		},
	}
	var sendConnected atomic.Bool
	var receiveConnected atomic.Bool
	transport.observeRouteState(&sendConnected, true, true)
	transport.observeRouteState(&sendConnected, true, true)
	transport.observeRouteState(&receiveConnected, false, true)
	transport.observeRouteState(&receiveConnected, false, true)
	snapshot := stats.Snapshot()
	if snapshot.ActiveSendRouteCount != 1 || snapshot.ActiveReceiveRouteCount != 1 {
		t.Fatalf("active route counts after connect=%+v, want send=1 receive=1", snapshot)
	}
	transport.observeRouteState(&sendConnected, true, false)
	transport.observeRouteState(&sendConnected, true, false)
	transport.observeRouteState(&receiveConnected, false, false)
	transport.observeRouteState(&receiveConnected, false, false)
	snapshot = stats.Snapshot()
	if snapshot.ActiveSendRouteCount != 0 || snapshot.ActiveReceiveRouteCount != 0 {
		t.Fatalf("active route counts after disconnect=%+v, want send=0 receive=0", snapshot)
	}
	if len(states) != 4 {
		t.Fatalf("route state observations=%d, want four real edges: %+v", len(states), states)
	}
	if states[0].PeerId != transport.peerId || states[0].StreamId != transport.streamId ||
		!states[0].Send || !states[0].Connected ||
		states[1].Send || !states[1].Connected ||
		!states[2].Send || states[2].Connected ||
		states[3].Send || states[3].Connected {
		t.Fatalf("route state observations=%+v", states)
	}
}
