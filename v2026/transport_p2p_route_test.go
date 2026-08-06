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
	go func() {
		updateP2pConnectionRoute(ctx, manager, transport, make(Route), true)
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
}
