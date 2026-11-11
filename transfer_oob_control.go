package connect

import (
	"context"
	"errors"
	"fmt"
	"io"
	mathrand "math/rand"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"encoding/base64"

	// "google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect/protocol"
)

// control messages for a client out of band with the client sequence
// some control messages require blocking response, but there is a potential deadlock
// when a send blocks to wait for a control receive, or vice versa, since
// all clients messages are multiplexed in the same client sequence
// and the receive/send may be blocked on the send/receive
// for example think of a remote provider setup forwarding traffic as fast as possible
// to an "echo" server with a finite buffer

type OobResultFunction = func(resultFrames []*protocol.Frame, err error)

type OutOfBandControl interface {
	SendControl(frames []*protocol.Frame, callback OobResultFunction)
}

// OutOfBandControlWithCtx is an optional upgrade interface for one-shot
// control with a caller-chosen context — e.g. shutdown cleanup that must
// outlive the closed client lifecycle (closing pending contracts). Normal
// control should use `SendControl`, which stays bound to the lifecycle
// context.
type OutOfBandControlWithCtx interface {
	OutOfBandControl
	SendControlWithCtx(ctx context.Context, frames []*protocol.Frame, callback OobResultFunction)
}

// An optional upgrade for one-shot control whose frames the platform applies
// once however often they arrive. Identified close reports are such frames:
// the platform keeps each report by its owner and report id, and answers an
// exact repeat without counting it again. A transient failure is retried
// inside the one admitted request, so a join of the control also joins the
// retries, within the bounds of retry.
type OutOfBandControlWithRetry interface {
	OutOfBandControl
	SendControlWithRetry(ctx context.Context, frames []*protocol.Frame, retry *OobRetrySettings, callback OobResultFunction)
}

// Bounds the attempts of one out-of-band request that is safe to repeat
// (SendControlWithRetry). The zero value sends once.
type OobRetrySettings struct {
	// Attempts of one request, the first included. At or below 1 the request is
	// sent once.
	MaxAttemptCount int
	// Bounds the retries from the start of the first attempt: a running retry is
	// canceled at the bound, and a wait that would reach it ends the request
	// instead. The first attempt keeps only the strategy's RequestTimeout, as a
	// request sent once does, so a request never ends sooner than its one
	// attempt would have; a first attempt that outlasts the bound leaves no
	// retry. Every attempt also keeps the strategy's RequestTimeout.
	// Non-positive bounds the request by MaxAttemptCount alone.
	Timeout time.Duration
	// The nominal wait after a request's first failed attempt. It doubles per
	// further failure up to RetryMaxTimeout, and each wait is drawn uniformly
	// from [d/2, d) of the nominal wait d (ControlSyncOobSettings' shape), so
	// requests that failed together do not retry in step. Non-positive retries
	// at once.
	RetryTimeout time.Duration
	// Caps the nominal wait. At or below RetryTimeout, the nominal wait stays
	// RetryTimeout.
	RetryMaxTimeout time.Duration
}

// What the requests sent with SendControlWithRetry came to. Identified close
// reports that outlive their client are today's only such requests
// (ContractManagerSettings.CloseReportRetry). Each count is cumulative and read
// independently, so a sampler reads deltas, never a total that must agree
// across counts.
type OobRetryStats struct {
	// requests admitted with a retry budget
	RequestCount int64
	// attempts after a request's first
	RetryCount int64
	// requests the platform processed on a retry
	RetrySuccessCount int64
	// requests that ended on a transient failure because their attempts, time,
	// caller context or strategy ran out
	GiveUpCount int64
	// requests that ended on a definitive answer, such as a 401 or a rejected
	// frame
	RefusedCount int64
}

// The events OobRetryStats counts, one per field.
type oobRetryEvent int

const (
	oobRetryEventRequest oobRetryEvent = iota
	oobRetryEventRetry
	oobRetryEventRetrySuccess
	oobRetryEventGiveUp
	oobRetryEventRefused
	oobRetryEventCount
)

// One owner's counts; safe for concurrent use.
type oobRetryCounters struct {
	eventCounts [oobRetryEventCount]atomic.Int64
}

func (self *oobRetryCounters) add(event oobRetryEvent) {
	self.eventCounts[event].Add(1)
}

func (self *oobRetryCounters) snapshot() OobRetryStats {
	return OobRetryStats{
		RequestCount:      self.eventCounts[oobRetryEventRequest].Load(),
		RetryCount:        self.eventCounts[oobRetryEventRetry].Load(),
		RetrySuccessCount: self.eventCounts[oobRetryEventRetrySuccess].Load(),
		GiveUpCount:       self.eventCounts[oobRetryEventGiveUp].Load(),
		RefusedCount:      self.eventCounts[oobRetryEventRefused].Load(),
	}
}

// The process-wide counts. They outlive the controls that record them: a
// window client's control is discarded with the client once its close reports
// have finished retrying.
var processOobRetryCounters oobRetryCounters

// Reads the process-wide counts of every control.
func OobRetryStatsSnapshot() OobRetryStats {
	return processOobRetryCounters.snapshot()
}

// Whether an attempt that failed with err may succeed when sent again
// unchanged: the platform gave no answer (a network error or a timeout), asked
// for the request again later (408 or 429), or failed on its side (5xx). Any
// other answer is definitive for the request, every other 4xx (401, 403, 404,
// ...) included. A rejection inside a processed response is not an error here
// and is never retried.
func oobRetryable(err error) bool {
	var statusErr *HttpStatusError
	if errors.As(err, &statusErr) {
		switch {
		case statusErr.StatusCode == http.StatusRequestTimeout,
			statusErr.StatusCode == http.StatusTooManyRequests:
			return true
		default:
			return 500 <= statusErr.StatusCode && statusErr.StatusCode <= 599
		}
	}
	if errors.Is(err, ErrHttpResponseBodyTooLarge) {
		return false
	}
	var exhaustedErr *HttpRequestExhaustedError
	var netErr net.Error
	return errors.As(err, &exhaustedErr) ||
		errors.As(err, &netErr) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF)
}

// NetworkClientControl is an explicitly supplied in-process control boundary.
// Implementations authenticate the current derived JWT, preserve controller
// accounting, and return only after processing the complete request. They must
// honor ctx cancellation; the caller still joins their terminal return.
type NetworkClientControl interface {
	ConnectControl(context.Context, string, *ConnectControlArgs) (*ConnectControlResult, error)
}

type ApiOutOfBandControl struct {
	localControl NetworkClientControl
	api          *BringYourApi
	ownsApi      bool
	requests     *lifecycleAdmission
	// Immutable, self-reported telemetry only; never an authorization claim.
	probeClaimed bool
	// The counts of this control's SendControlWithRetry requests.
	retryCounters oobRetryCounters

	// Nil test barrier exposes the exact join boundary after admission closes.
	beforeCloseWaitForTest func()
	// Nil test hook replaces the uniform jitter offset in [0, half) of a retry
	// wait.
	retryJitterForTest func(half time.Duration) time.Duration
}

func NewApiOutOfBandControl(
	ctx context.Context,
	clientStrategy *ClientStrategy,
	byJwt string,
	apiUrl string,
) *ApiOutOfBandControl {
	return newApiOutOfBandControl(ctx, clientStrategy, byJwt, apiUrl, false)
}

// The generator supplies the immutable probe marker for its private controls.
func newApiOutOfBandControl(ctx context.Context, clientStrategy *ClientStrategy, byJwt, apiUrl string, probeClaimed bool) *ApiOutOfBandControl {
	api := NewBringYourApi(ctx, clientStrategy, apiUrl)
	api.SetByJwt(byJwt)
	return &ApiOutOfBandControl{
		api:          api,
		ownsApi:      true,
		requests:     newLifecycleAdmission(),
		probeClaimed: probeClaimed,
	}
}

// Local control keeps the actual API OOB owner and its processed-result,
// frame-ownership and close/join rules. It does not replace the data transport.
func NewApiOutOfBandControlWithLocalControl(ctx context.Context, strategy *ClientStrategy, byJwt, apiUrl string, control NetworkClientControl) *ApiOutOfBandControl {
	if control == nil {
		panic("local control authority is required")
	}
	owner := NewApiOutOfBandControl(ctx, strategy, byJwt, apiUrl)
	owner.localControl = control
	return owner
}

func NewApiOutOfBandControlWithApi(api *BringYourApi) *ApiOutOfBandControl {
	return &ApiOutOfBandControl{
		api:      api,
		requests: newLifecycleAdmission(),
	}
}

// SetByJwt updates the bearer token used by future out-of-band control
// requests. Long-lived clients call this when their renewable client JWT is
// rotated; BringYourApi provides the synchronization for concurrent sends.
func (self *ApiOutOfBandControl) SetByJwt(byJwt string) {
	self.api.SetByJwt(byJwt)
}

func (self *ApiOutOfBandControl) SendControl(
	frames []*protocol.Frame,
	callback OobResultFunction,
) {
	// bound to the api lifecycle context: keep trying as long as the
	// lifecycle is active
	self.SendControlWithCtx(self.api.ctx, frames, callback)
}

// SendControlWithCtx is a one-shot send on a caller-chosen context, for
// cleanup that must not be bound to the (possibly closed) lifecycle context.
// The request stays bounded by the client strategy's `RequestTimeout`.
func (self *ApiOutOfBandControl) SendControlWithCtx(
	ctx context.Context,
	frames []*protocol.Frame,
	callback OobResultFunction,
) {
	connectControl := func(connectControlArgs *ConnectControlArgs, apiCallback ConnectControlCallback) {
		if self.localControl == nil {
			self.api.connectControlWithCtx(ctx, connectControlArgs, apiCallback, self.probeClaimed)
			return
		}
		requestCtx, cancel := context.WithTimeout(ctx, self.api.clientStrategy.settings.RequestTimeout)
		go func() {
			defer cancel()
			var result *ConnectControlResult
			var err error
			byJwt := self.api.ByJwt()
			HandleError(func() {
				if err = requestCtx.Err(); err == nil {
					result, err = self.localControl.ConnectControl(requestCtx, byJwt, connectControlArgs)
				}
			}, func(recovered error) { err = fmt.Errorf("local control failed: %w", recovered) })
			if requestCtx.Err() != nil {
				// A local handler may return a committed contract after its
				// request was canceled. Retain this admitted owner through the
				// requester's zero-use close before discarding that result. The
				// canceled result never reaches a data producer or becomes success.
				var cleanupErr error
				HandleError(func() {
					cleanupErr = self.closeUndeliveredLocalContracts(requestCtx, byJwt, connectControlArgs, result)
				}, func(recovered error) {
					cleanupErr = fmt.Errorf("local undelivered contract cleanup failed: %w", recovered)
				})
				result, err = nil, errors.Join(requestCtx.Err(), cleanupErr)
			}
			apiCallback.Result(result, err)
		}()
	}
	self.sendControl(connectControl, frames, callback)
}

// Sends as SendControlWithCtx does, for frames the platform applies once
// however often they arrive (OutOfBandControlWithRetry). One admission covers
// every attempt. An attempt that got no answer (a network error or a
// timeout), a 408, a 429 or a 5xx is sent again with the same bytes and the
// current credential after a jittered exponential wait, within retry's attempt
// and time bounds, while ctx and the strategy are live. Any other answer ends
// the request: a 4xx such as 401, or a rejection inside a processed response
// such as an unknown contract. Close does not cancel admitted retries, which
// end within their bounds; CloseAndWait joins them. A callback error after
// more than one attempt names the attempts. In-process local control has no
// transport to fail transiently and is sent once.
func (self *ApiOutOfBandControl) SendControlWithRetry(
	ctx context.Context,
	frames []*protocol.Frame,
	retry *OobRetrySettings,
	callback OobResultFunction,
) {
	if self.localControl != nil || retry == nil || retry.MaxAttemptCount <= 1 {
		self.SendControlWithCtx(ctx, frames, callback)
		return
	}
	settings := *retry
	var strategyDone <-chan struct{}
	strategyLog := NewNoopLogger()
	if strategy := self.api.clientStrategy; strategy != nil {
		if strategy.ctx != nil {
			strategyDone = strategy.ctx.Done()
		}
		if strategy.log != nil {
			strategyLog = strategy.log
		}
	}
	// Each event moves this control's count and the process's together.
	count := func(event oobRetryEvent) {
		self.retryCounters.add(event)
		processOobRetryCounters.add(event)
	}
	connectControl := func(connectControlArgs *ConnectControlArgs, apiCallback ConnectControlCallback) {
		count(oobRetryEventRequest)
		startTime := time.Now()
		// one budget for every wait and retry of the request
		var retryCtx context.Context
		var retryCancel context.CancelFunc
		if 0 < settings.Timeout {
			retryCtx, retryCancel = context.WithTimeout(ctx, settings.Timeout)
		} else {
			retryCtx, retryCancel = context.WithCancel(ctx)
		}

		// Attempts run one at a time, each launched from the previous attempt's
		// callback after its wait, so only one callback touches this state.
		attemptCount := 0
		nominalRetryTimeout := settings.RetryTimeout
		// Draws a failed attempt's wait uniformly from [d/2, d) of the nominal
		// wait d, then doubles d up to RetryMaxTimeout for the next failure.
		// Jitter only downward, so the cap is a real ceiling.
		nextRetryTimeout := func() time.Duration {
			d := nominalRetryTimeout
			if d <= 0 {
				return 0
			}
			maxTimeout := max(settings.RetryTimeout, settings.RetryMaxTimeout)
			// compare against half the cap, since doubling d could overflow
			if maxTimeout/2 < d {
				nominalRetryTimeout = maxTimeout
			} else {
				nominalRetryTimeout = 2 * d
			}
			half := d / 2
			if half <= 0 {
				return d
			}
			if self.retryJitterForTest != nil {
				return half + max(0, min(half-1, self.retryJitterForTest(half)))
			}
			return half + time.Duration(mathrand.Int63n(int64(half)))
		}

		var attempt func()
		attempt = func() {
			attemptCount += 1
			// The first attempt is the request sent once; only retries spend the
			// time budget.
			attemptCtx := retryCtx
			if attemptCount == 1 {
				attemptCtx = ctx
			}
			self.api.connectControlWithCtx(attemptCtx, connectControlArgs, NewApiCallback(func(result *ConnectControlResult, err error) {
				// Every path that launches no further attempt publishes this
				// attempt's outcome, so admission always finishes.
				retried := false
				defer func() {
					if !retried {
						retryCancel()
						apiCallback.Result(result, err)
					}
				}()
				if err == nil {
					if result != nil && result.Error == nil {
						if 1 < attemptCount {
							count(oobRetryEventRetrySuccess)
							if strategyLog.V(1).Enabled() {
								strategyLog.Infof("[oob]control processed on attempt %d of %d\n", attemptCount, settings.MaxAttemptCount)
							}
						}
					} else {
						// an absent or rejected processed response
						count(oobRetryEventRefused)
					}
					return
				}
				if !oobRetryable(err) {
					count(oobRetryEventRefused)
					if 1 < attemptCount {
						err = fmt.Errorf("control refused on attempt %d of %d: %w", attemptCount, settings.MaxAttemptCount, err)
					}
					return
				}
				giveUp := func() {
					count(oobRetryEventGiveUp)
					err = fmt.Errorf("control gave up after %d of %d attempts in %s: %w", attemptCount, settings.MaxAttemptCount, time.Since(startTime).Round(time.Millisecond), err)
				}
				if settings.MaxAttemptCount <= attemptCount || retryCtx.Err() != nil {
					giveUp()
					return
				}
				select {
				case <-strategyDone:
					// a closed strategy fails every later attempt at once
					giveUp()
					return
				default:
				}
				retryTimeout := nextRetryTimeout()
				if deadline, ok := retryCtx.Deadline(); ok && !time.Now().Add(retryTimeout).Before(deadline) {
					giveUp()
					return
				}
				if strategyLog.V(1).Enabled() {
					strategyLog.Infof("[oob]control retry %d of %d in %s = %s\n", attemptCount+1, settings.MaxAttemptCount, retryTimeout, httpRequestErrorWithCauses(err))
				}
				select {
				case <-retryCtx.Done():
					giveUp()
					return
				case <-strategyDone:
					giveUp()
					return
				case <-time.After(retryTimeout):
				}
				count(oobRetryEventRetry)
				retried = true
				attempt()
			}), self.probeClaimed)
		}
		attempt()
	}
	self.sendControl(connectControl, frames, callback)
}

// Reads the counts of this control's SendControlWithRetry requests.
func (self *ApiOutOfBandControl) RetryStats() OobRetryStats {
	return self.retryCounters.snapshot()
}

func (self *ApiOutOfBandControl) sendControl(
	connectControl func(*ConnectControlArgs, ConnectControlCallback),
	frames []*protocol.Frame,
	callback OobResultFunction,
) {
	safeCallback := func(resultFrames []*protocol.Frame, err error) {
		if callback != nil {
			HandleError(func() {
				callback(resultFrames, err)
			})
		}
	}
	returnFrames := func() {
		for _, frame := range frames {
			MessagePoolReturn(frame.MessageBytes)
		}
	}
	if !self.requests.start() {
		returnFrames()
		safeCallback(nil, context.Canceled)
		return
	}

	pack := &protocol.Pack{
		Frames: frames,
	}
	packBytes, err := ProtoMarshal(pack)
	if err != nil {
		defer self.requests.finish()
		returnFrames()
		safeCallback(nil, err)
		return
	}
	encodedPack := EncodeBase64(base64.StdEncoding, packBytes)
	MessagePoolReturn(packBytes)
	returnFrames()

	connectControl(
		&ConnectControlArgs{
			Pack: encodedPack,
		},
		NewApiCallback(func(result *ConnectControlResult, err error) {
			// Request completion is published after every callback-local pooled
			// buffer has returned. CloseAndWait may therefore use completion as
			// an exact ownership barrier.
			defer self.requests.finish()
			if err != nil {
				safeCallback(nil, err)
				return
			}
			if result == nil {
				safeCallback(nil, errors.New("connect control response is absent"))
				return
			}
			if result.Error != nil {
				safeCallback(nil, errors.New("connect control rejected: "+result.Error.Message))
				return
			}

			packBytes, err := DecodeBase64(base64.StdEncoding, result.Pack)
			if err != nil {
				safeCallback(nil, err)
				return
			}
			defer MessagePoolReturn(packBytes)

			responsePack := &protocol.Pack{}
			err = ProtoUnmarshal(packBytes, responsePack)
			if err != nil {
				safeCallback(nil, err)
				return
			}

			safeCallback(responsePack.Frames, nil)
		}),
	)
}

// Close prevents later request admission. A control constructed with its own
// API also cancels API-bound requests; a wrapper around a caller-owned API
// leaves that shared API open. Caller-context cleanup requests remain bounded
// by their client-strategy timeout and are joined by CloseAndWait.
func (self *ApiOutOfBandControl) Close() {
	self.requests.close()
	if self.ownsApi {
		self.api.Close()
	}
}

// CloseAndWait closes request admission and joins every request and callback
// admitted before that boundary. An OOB callback must not call CloseAndWait
// because it would wait for its own return. It may call Close and ask an
// external owner goroutine to perform the wait.
func (self *ApiOutOfBandControl) CloseAndWait(ctx context.Context) error {
	self.Close()
	if self.beforeCloseWaitForTest != nil {
		self.beforeCloseWaitForTest()
	}
	return waitForLifecycleDone(ctx, self.requests.Done(), "api out-of-band requests")
}

type NoContractClientOob struct {
}

func NewNoContractClientOob() *NoContractClientOob {
	return &NoContractClientOob{}
}

func (self *NoContractClientOob) SendControl(frames []*protocol.Frame, callback func(resultFrames []*protocol.Frame, err error)) {
	safeCallback := func(resultFrames []*protocol.Frame, err error) {
		if callback != nil {
			HandleError(func() {
				callback(resultFrames, err)
			})
		}
	}

	// SendControl takes ownership of the frames; this oob cannot deliver them but
	// must still release the pooled bytes (mirrors ApiOutOfBandControl.sendControl)
	for _, frame := range frames {
		MessagePoolReturn(frame.MessageBytes)
	}

	safeCallback(nil, errors.New("Not supported."))
}
