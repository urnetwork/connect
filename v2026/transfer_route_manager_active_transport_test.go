package connect

import (
	"context"
	"testing"
)

// Whether any transport is registered can be waited on: the change channel
// closes when the first transport registers and when the last one leaves,
// and not for a change that leaves a carrier registered.
func TestRouteManagerActiveTransportNotify(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	routeManager := NewRouteManager(ctx, "test")
	closed := func(notify <-chan struct{}) bool {
		select {
		case <-notify:
			return true
		default:
			return false
		}
	}

	active, notify := routeManager.ActiveTransportNotify()
	if active {
		t.Fatal("a route manager with no transport reports an active one")
	}
	first := NewSendClientTransport(DestinationId(NewId()))
	routeManager.UpdateTransport(first, []Route{make(Route)})
	if !closed(notify) {
		t.Fatal("the first transport registering did not notify")
	}
	active, notify = routeManager.ActiveTransportNotify()
	if !active || active != routeManager.HasActiveTransport() {
		t.Fatal("the active transport read disagrees with HasActiveTransport")
	}

	second := NewSendClientTransport(DestinationId(NewId()))
	routeManager.UpdateTransport(second, []Route{make(Route)})
	routeManager.RemoveTransport(first)
	if closed(notify) {
		t.Fatal("a change that left a carrier registered notified")
	}
	routeManager.RemoveTransport(second)
	if !closed(notify) {
		t.Fatal("the last transport leaving did not notify")
	}
	if active, _ := routeManager.ActiveTransportNotify(); active || routeManager.HasActiveTransport() {
		t.Fatal("a route manager whose last transport left reports an active one")
	}
}
