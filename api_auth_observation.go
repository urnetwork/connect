// Opt-in, request-local diagnostics for synchronous network-client auth.
// Observation never owns work, changes a deadline, or retains an identity.
package connect

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptrace"
	"sync/atomic"
)

type authRequestPhase int32

const (
	authNoPost authRequestPhase = iota
	authPreWrite
	authResponseWait
	authBodyRead
)

type authRequestResult int32

const (
	authOK authRequestResult = iota
	authTimeout
	authCanceled
	authHttpAuth
	authHttpRate
	authHttpError
	authApiError
	authDecodeError
	authError
)

var authRequestPhaseLabels = [...]string{"no_post", "pre_write", "response_wait", "body_read"}
var authRequestResultLabels = [...]string{"ok", "timeout", "canceled", "http_auth", "http_rate", "http_error", "api_error", "decode_error", "error"}

// AuthNetworkClientObservations counts completed logical synchronous auth
// calls, not individual routes or hello requests. Only fixed atomic cells are
// updated; there is no callback, queue, I/O, or diagnostic goroutine. The
// embedding owner may share this collector without sharing any client state.
// Safe for concurrent use; do not copy after first use.
type AuthNetworkClientObservations struct {
	counts [len(authRequestPhaseLabels)][len(authRequestResultLabels)]atomic.Uint64
}

// AuthNetworkClientObservation contains only closed-vocabulary labels.
type AuthNetworkClientObservation struct {
	Phase  string
	Result string
	Count  uint64
}

// Snapshot reads independent monotonic cells, not an atomic cross-cell slice.
// Zero cells are present so capability cannot be mistaken for missing data.
func (self *AuthNetworkClientObservations) Snapshot() [len(authRequestPhaseLabels) * len(authRequestResultLabels)]AuthNetworkClientObservation {
	var values [len(authRequestPhaseLabels) * len(authRequestResultLabels)]AuthNetworkClientObservation
	index := 0
	for phase, phaseLabel := range authRequestPhaseLabels {
		for result, resultLabel := range authRequestResultLabels {
			value := AuthNetworkClientObservation{Phase: phaseLabel, Result: resultLabel}
			if self != nil {
				value.Count = self.counts[phase][result].Load()
			}
			values[index] = value
			index++
		}
	}
	return values
}

func (self *AuthNetworkClientObservations) record(phase authRequestPhase, result authRequestResult) {
	if self == nil || phase < 0 || len(authRequestPhaseLabels) <= int(phase) || result < 0 || len(authRequestResultLabels) <= int(result) {
		return
	}
	self.counts[phase][result].Add(1)
}

type authObservationsContextKey struct{}
type authRequestContextKey struct{}

// WithAuthNetworkClientObservations opts a context's synchronous auth calls
// into a caller-owned aggregate. Nil leaves the context unchanged. The API's
// construction context is also consulted when its caller uses another context.
func WithAuthNetworkClientObservations(ctx context.Context, observations *AuthNetworkClientObservations) context.Context {
	if observations == nil {
		return ctx
	}
	return context.WithValue(ctx, authObservationsContextKey{}, observations)
}

type authPostProgress struct {
	phase atomic.Int32
}

// A new POST replaces the previous attempt's progress. Its trace owns a
// separate cell, so a late callback from an earlier route cannot advance the
// current attempt. Hello discovery never receives one of these traces.
type authRequestObservation struct {
	counts      *AuthNetworkClientObservations
	lastPost    atomic.Pointer[authPostProgress]
	strategyEnd atomic.Int32
}

func beginAuthRequestObservation(ctx, ownerCtx context.Context) (context.Context, *authRequestObservation) {
	counts, _ := ctx.Value(authObservationsContextKey{}).(*AuthNetworkClientObservations)
	if counts == nil {
		counts, _ = ownerCtx.Value(authObservationsContextKey{}).(*AuthNetworkClientObservations)
	}
	if counts == nil {
		return ctx, nil
	}
	observation := &authRequestObservation{counts: counts}
	return context.WithValue(ctx, authRequestContextKey{}, observation), observation
}

func traceAuthPostAttempt(ctx context.Context) (context.Context, *authPostProgress) {
	observation, _ := ctx.Value(authRequestContextKey{}).(*authRequestObservation)
	if observation == nil {
		return ctx, nil
	}
	progress := &authPostProgress{}
	progress.phase.Store(int32(authPreWrite))
	observation.lastPost.Store(progress)
	return httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			if info.Err == nil {
				progress.phase.CompareAndSwap(int32(authPreWrite), int32(authResponseWait))
			}
		},
	}), progress
}

func (self *authPostProgress) responseHeaders(response *http.Response, err error) {
	if self != nil && err == nil && response != nil {
		// A response itself is positive evidence even for an alternative
		// transport that does not implement net/http's write trace.
		self.phase.Store(int32(authBodyRead))
	}
}

// Preserve the operation's terminal verdict even when retained earlier attempt
// failures contain another deadline or cancellation cause.
func observeAuthStrategyEnd(ctx, strategyCtx context.Context) {
	observation, _ := ctx.Value(authRequestContextKey{}).(*authRequestObservation)
	if observation == nil {
		return
	}
	result := authTimeout
	if errors.Is(ctx.Err(), context.Canceled) || errors.Is(strategyCtx.Err(), context.Canceled) {
		result = authCanceled
	}
	observation.strategyEnd.Store(int32(result))
}

func (self *authRequestObservation) finish(result *AuthNetworkClientResult, err error) {
	if self == nil {
		return
	}
	phase := authNoPost
	if progress := self.lastPost.Load(); progress != nil {
		phase = authRequestPhase(progress.phase.Load())
	}
	outcome := authOK
	if err != nil {
		var status *HttpStatusError
		var syntax *json.SyntaxError
		var unmarshal *json.UnmarshalTypeError
		var timeout net.Error
		switch {
		case self.strategyEnd.Load() != 0:
			outcome = authRequestResult(self.strategyEnd.Load())
		case errors.Is(err, context.DeadlineExceeded):
			outcome = authTimeout
		case errors.Is(err, context.Canceled):
			outcome = authCanceled
		case errors.As(err, &timeout) && timeout.Timeout():
			outcome = authTimeout
		case errors.As(err, &status):
			switch status.StatusCode {
			case http.StatusUnauthorized, http.StatusForbidden:
				outcome = authHttpAuth
			case http.StatusTooManyRequests:
				outcome = authHttpRate
			default:
				outcome = authHttpError
			}
		case errors.As(err, &syntax), errors.As(err, &unmarshal):
			outcome = authDecodeError
		default:
			outcome = authError
		}
	} else if result != nil && result.Error != nil {
		outcome = authApiError
	}
	self.counts.record(phase, outcome)
}
