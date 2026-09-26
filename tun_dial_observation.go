// Request-local progress separates target resolution from target TCP. Resolver
// transports do not inherit the target observer, and no identities are emitted.
package connect

import (
	"context"
	"net"
	"net/netip"
)

// Positive progress only: a losing family cannot erase an answer or connection.
// A later event is the furthest observed progress, not proof that every DNS
// family finished or that no other dial remains pending.
type TunDialEvent uint8

const (
	TunDialDnsStarted TunDialEvent = iota + 1
	TunDialDnsAnswered
	TunDialTcpStarted
	TunDialTcpConnected
)

// The caller owns the observer. Calls may be concurrent and must not block;
// they carry no hostname, address, provider identity, or free-form error.
// Nil clears an inherited observer at a nested resolver-transport boundary.
func WithTunDialObserver(ctx context.Context, observer func(TunDialEvent)) context.Context {
	if observer == nil && tunDialObserver(ctx) == nil {
		return ctx
	}
	return context.WithValue(ctx, tunDialObserverContextKey{}, observer)
}

// A private context key cannot collide with another request's diagnostic state.
type tunDialObserverContextKey struct{}

// The absence of an observer leaves ordinary dial behavior unchanged.
func tunDialObserver(ctx context.Context) func(TunDialEvent) {
	observer, _ := ctx.Value(tunDialObserverContextKey{}).(func(TunDialEvent))
	return observer
}

// Only this target's literal dials report TCP progress. Clearing the context
// prevents a DoH HTTP connection from masquerading as the target connection.
func observeTunTcpDial(ctx context.Context, dial dialAddrFunction) (context.Context, dialAddrFunction) {
	observer := tunDialObserver(ctx)
	if observer == nil {
		return ctx, dial
	}
	return WithTunDialObserver(ctx, nil), func(ctx context.Context, addr netip.Addr) (net.Conn, error) {
		if ctx.Err() == nil {
			observer(TunDialTcpStarted)
		}
		conn, err := dial(ctx, addr)
		if err == nil && conn != nil && ctx.Err() == nil {
			observer(TunDialTcpConnected)
		}
		return conn, err
	}
}
