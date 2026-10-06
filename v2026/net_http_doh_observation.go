// Finite DoH evidence separates a socket attempt from the resolver call that
// initiated it. No names, endpoints, query ids, routes, or error text are kept.
package connect

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// The supplied dialer is only called a tun when its owning Tun sets the marker.
// A custom host binding, proxy, or unknown caller must retain caller provenance.
type dohDialPath uint8

const (
	dohPathUnknown dohDialPath = iota
	dohPathHost
	dohPathCaller
	dohPathTun
	dohPathCount
)

// Return only the fixed schema token, including unknown for invalid values.
func (self dohDialPath) String() string {
	switch self {
	case dohPathHost:
		return "host"
	case dohPathCaller:
		return "caller"
	case dohPathTun:
		return "tun"
	default:
		return "unknown"
	}
}

// Infer only the dialer ownership that the settings actually establish.
func dohRemoteDialPath(settings *DohSettings) dohDialPath {
	if settings == nil {
		return dohPathUnknown
	}
	if settings.DialContextSettings != nil {
		if settings.DialContextSettings.dohTun {
			return dohPathTun
		}
		return dohPathCaller
	}
	if settings.ProxySettings != nil {
		return dohPathCaller
	}
	return dohPathHost
}

// Scope names count complete public calls, including cache hits and coalesced
// waiters. A one-shot call may request several names; it is still one call.
type dohResolverScope uint8

const (
	dohScopeAddress dohResolverScope = iota
	dohScopeForward
	dohScopeOneShot
	dohScopeCount
)

// Map the three internal call boundaries to their fixed schema tokens.
func (self dohResolverScope) String() string {
	switch self {
	case dohScopeAddress:
		return "address"
	case dohScopeForward:
		return "forward"
	default:
		return "oneshot"
	}
}

// Pending is an observation at dial completion, never a terminal call outcome.
type dohResolverOutcome uint32

const (
	dohOutcomePending dohResolverOutcome = iota
	dohOutcomeAnswer
	dohOutcomeEmpty
	dohOutcomeStale
	dohOutcomeFailed
	dohOutcomeCanceled
	dohOutcomeTimeout
	dohOutcomeCount
)

// Render a terminal kind or pending without retaining transport error text.
func (self dohResolverOutcome) String() string {
	switch self {
	case dohOutcomePending:
		return "pending"
	case dohOutcomeAnswer:
		return "answer"
	case dohOutcomeEmpty:
		return "authoritative_empty"
	case dohOutcomeStale:
		return "stale"
	case dohOutcomeFailed:
		return "failed"
	case dohOutcomeCanceled:
		return "canceled"
	case dohOutcomeTimeout:
		return "timeout"
	default:
		return "unknown"
	}
}

// Private context key shared only by resolver entrypoints and dial observers.
type dohResolverObservationKey struct{}

// The transport deliberately detaches dial cancellation but preserves context
// values. This small identity-free state therefore survives a losing hedge and
// reports the initiating call's outcome, not an unrelated later pooled request.
type dohResolverObservation struct {
	path    dohDialPath
	scope   dohResolverScope
	outcome atomic.Uint32
}

// Attach one identity-free call result; nested resolver calls own their result.
func newDohResolverObservation(ctx context.Context, path dohDialPath, scope dohResolverScope) (context.Context, *dohResolverObservation) {
	observation := &dohResolverObservation{path: path, scope: scope}
	return context.WithValue(ctx, dohResolverObservationKey{}, observation), observation
}

// Results win over cancellation: a usable answer can race the caller deadline.
// The original caller context is read before cleanup cancellation is applied.
func dohFinalResolverOutcome(ctx context.Context, answered bool, authoritative bool, stale bool) dohResolverOutcome {
	if stale {
		return dohOutcomeStale
	}
	if answered {
		return dohOutcomeAnswer
	}
	if authoritative {
		return dohOutcomeEmpty
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return dohOutcomeTimeout
	}
	if ctx.Err() != nil {
		return dohOutcomeCanceled
	}
	return dohOutcomeFailed
}

// Process-local cumulative counts have a fixed cardinality and emit at most one
// snapshot per path/scope every five seconds. A quiet or lost log interval is
// not a zero-failure interval; compare fresh same-process snapshots only.
type dohResolverCounter struct {
	stateLock sync.Mutex
	counts    [dohOutcomeCount]uint64
	nextLog   time.Time
}

var dohResolverCounters [dohPathCount][dohScopeCount]dohResolverCounter

// Count every completion and return an immutable snapshot when emission is due.
func (self *dohResolverCounter) record(outcome dohResolverOutcome, now time.Time) ([dohOutcomeCount]uint64, bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.counts[outcome]++
	if now.Before(self.nextLog) {
		return self.counts, false
	}
	self.nextLog = now.Add(controlDialLogInterval)
	return self.counts, true
}

// Publish before logging so detached dials see the terminal state even when a
// logger is slow. Snapshot counts are exact; emitted line counts are sampled.
func (self *dohResolverObservation) finish(log Logger, outcome dohResolverOutcome) {
	self.outcome.Store(uint32(outcome))
	counts, emit := dohResolverCounters[self.path][self.scope].record(outcome, time.Now())
	if emit {
		loggerOrDefault(log).Infof(
			"[doh]resolver observable=v1 owner_path=%s scope=%s answer=%d authoritative_empty=%d stale=%d failed=%d canceled=%d timeout=%d\n",
			self.path, self.scope, counts[dohOutcomeAnswer], counts[dohOutcomeEmpty], counts[dohOutcomeStale],
			counts[dohOutcomeFailed], counts[dohOutcomeCanceled], counts[dohOutcomeTimeout],
		)
	}
}

// Failed sockets have no RemoteAddr; classify the literal input independently
// of process policy, query record type, or a successful connection's family.
func dohAttemptFamily(address string) string {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		host = address
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return "unknown"
	}
	if ip.Unmap().Is4() {
		return "4"
	}
	return "6"
}

// All values in the line are finite. Error strings may contain endpoint and
// query material and must never cross this diagnostic boundary.
func dohDialObservationLine(ctx context.Context, path dohDialPath, address string, err error) string {
	result := "success"
	switch {
	case err == nil:
	case errors.Is(err, context.Canceled):
		result = "canceled"
	case isPathTimeout(err) || err.Error() == "i/o timeout":
		result = "timeout"
	case errors.Is(err, syscall.EAFNOSUPPORT):
		result = "unsupported_family"
	case errors.Is(err, syscall.ECONNREFUSED):
		result = "refused"
	default:
		result = "error"
	}
	owner, scope, outcome := "unknown", "unknown", "unknown"
	if observation, ok := ctx.Value(dohResolverObservationKey{}).(*dohResolverObservation); ok {
		owner = observation.path.String()
		scope = observation.scope.String()
		outcome = dohResolverOutcome(observation.outcome.Load()).String()
	}
	// Retain only fixed legacy error tokens so an older monitor still sees
	// timeout/refusal pages during a rolling producer/parser deployment.
	suffix := ""
	switch result {
	case "timeout":
		suffix = " err=i/o timeout"
	case "refused":
		suffix = " err=connect: connection refused"
	case "unsupported_family":
		suffix = " err=address family not supported by protocol"
	case "canceled":
		suffix = " err=context canceled"
	case "error":
		suffix = " err=dial error"
	}
	return fmt.Sprintf(
		"[family]dial tag=doh observable=v1 path=%s attempted_family=%s result=%s resolver_owner=%s resolver_scope=%s resolver_outcome=%s%s",
		path, dohAttemptFamily(address), result, owner, scope, outcome, suffix,
	)
}

// Preserve the existing per-target emission bound, partitioned by actual path.
// The internal throttle key is bounded and never emitted; dials are untouched.
func wrapDohDial(log Logger, path dohDialPath, dial DialContextFunction) DialContextFunction {
	return func(ctx context.Context, network string, address string) (net.Conn, error) {
		conn, err := dial(ctx, network, address)
		if emit, _ := controlDialThrottle("doh|" + path.String() + "|" + address).Allow(time.Now()); emit {
			loggerOrDefault(log).Infof("%s\n", dohDialObservationLine(ctx, path, address, err))
		}
		return conn, err
	}
}
