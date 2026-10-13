// Stream-clock bootstrap must survive an unproven HTTP route without granting
// authority before a matching clock response or reviving retired work.
package connect

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Takes request frames and lends a response only until the supplied callback
// returns. Tests own any deliberately blocked synchronous launcher.
type bootstrapClockOob struct {
	send func(context.Context, Id, OobResultFunction)
}

// Retains the ordinary interface while making contextual use observable.
func (self *bootstrapClockOob) SendControl(frames []*protocol.Frame, callback OobResultFunction) {
	self.SendControlWithCtx(context.Background(), frames, callback)
}

// Decodes the scalar id before returning every owned request buffer.
func (self *bootstrapClockOob) SendControlWithCtx(ctx context.Context, frames []*protocol.Frame, callback OobResultFunction) {
	var clockId Id
	var decodeErr error
	if len(frames) != 1 {
		decodeErr = errors.New("expected one clock frame")
	} else {
		message, err := FromFrame(frames[0])
		decodeErr = err
		if err == nil {
			clock, ok := message.(*protocol.StreamAuthorization)
			if !ok {
				decodeErr = errors.New("expected stream clock request")
			} else {
				clockId, decodeErr = IdFromBytes(clock.ClockId)
			}
		}
	}
	for _, frame := range frames {
		MessagePoolReturn(frame.MessageBytes)
	}
	if decodeErr != nil {
		callback(nil, decodeErr)
		return
	}
	self.send(ctx, clockId, callback)
}

// Immediate overwrite makes retaining callback-owned bytes deterministically
// wrong; the consumer must finish decoding while the callback is running.
func respondBootstrapClock(clockId Id, serverMillis int64, callback OobResultFunction) {
	frame := RequireToFrameWithDefaultProtocolVersion(&protocol.StreamAuthorization{
		ClockId:         clockId.Bytes(),
		ClockUnixMillis: serverMillis,
	})
	callback([]*protocol.Frame{frame}, nil)
	clear(frame.MessageBytes)
	MessagePoolReturn(frame.MessageBytes)
}

// Creates the ordinary client lifecycle, with unrelated pings and Pion socket
// admission disabled. Cleanup joins the real stream and client workers.
func newBootstrapClockClient(t *testing.T, oob OutOfBandControl) *Client {
	t.Helper()
	settings := DefaultClientSettings()
	settings.Log = NewNoopLogger()
	settings.ControlPingTimeout = 0
	settings.WebRtcSettings.MemoryBudget = NewTransferMemoryBudget(0)
	client := NewClient(t.Context(), NewId(), oob, settings)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := client.CloseAndWait(ctx); err != nil {
			t.Errorf("join bootstrap client: %v", err)
		}
	})
	return client
}

// Owns the clock driver explicitly so each test can join the exact attempt.
// Receive still performs the production pending clone and authorization check.
func queueBootstrapClockOpen(t *testing.T, manager *StreamManager, open *protocol.StreamOpen) {
	t.Helper()
	manager.authorization.Lock()
	manager.authorization.clockPending = true
	manager.authorization.Unlock()
	frame := RequireToFrameWithDefaultProtocolVersion(open)
	err := manager.handleControlFrame(frame)
	clear(frame.MessageBytes)
	MessagePoolReturn(frame.MessageBytes)
	if err != nil {
		t.Fatal(err)
	}
}

// Builds synthetic authority whose absolute deadline remains fixed on replay.
func newBootstrapClockOpen(serverMillis int64) *protocol.StreamOpen {
	return &protocol.StreamOpen{
		StreamId:                        NewId().Bytes(),
		DestinationId:                   NewId().Bytes(),
		AuthorizationGeneration:         NewId().Bytes(),
		AuthorizationLeaseMillis:        uint32(MaxStreamAuthorizationLease / time.Millisecond),
		AuthorizationDeadlineUnixMillis: serverMillis + MaxStreamAuthorizationLease.Milliseconds(),
	}
}

// The existing real API and scoring paths run against an already-connected
// request boundary. Virtual timers model exact RTT, not scheduler-based loss.
func checkBootstrapClockHttpBudget(t *testing.T, roundTrip time.Duration, proven bool) {
	t.Helper()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		settings := DefaultClientStrategySettings()
		settings.Log = NewNoopLogger()
		settings.EnableResilient = false
		settings.MinNextConnectDelay = 0
		settings.MaxNextConnectDelay = 0
		strategy := NewClientStrategy(ctx, settings)
		defer strategy.Close()

		strategy.mutex.Lock()
		var dialer *clientDialer
		count := len(strategy.dialers)
		for candidate := range strategy.dialers {
			dialer = candidate
		}
		strategy.mutex.Unlock()
		if count != 1 || dialer == nil || strategy.scores == nil {
			t.Fatalf("fixture needs one scored normal dialer, got %d", count)
		}
		// A prior successful handshake and an already-connected HTTP boundary
		// are deliberately insufficient to bypass hello when scoring is on.
		dialer.Update(ctx, nil)
		if proven {
			strategy.dialerInfo(dialer).observeRead(ctx, int(deliveryVerifiedByteCount), nil, false)
		}
		if strategy.dialerDelivered(dialer) != proven {
			t.Fatal("incorrect initial delivery evidence")
		}
		var stateLock sync.Mutex
		var methods []string
		var completed []string
		var canceledErr error
		// One scalar record per request; a third request is a fixture failure.
		type clockRequestObservation struct {
			Method     string
			Path       string
			Started    time.Duration
			Written    time.Duration
			Returned   time.Duration
			Deadline   time.Duration
			ClockId    Id
			WriteTrace bool
			Err        error
		}
		var requests []clockRequestObservation
		var start time.Time
		// The clock value is fixed to isolate request completion. This is not
		// a measurement of production clock-offset accuracy.
		serverMillis := time.Now().UnixMilli()
		dialer.httpClient = &http.Client{
			Transport: serialTestRoundTripper(func(request *http.Request) (response *http.Response, requestErr error) {
				if request.Body != nil {
					defer request.Body.Close()
				}
				deadline, hasDeadline := request.Context().Deadline()
				stateLock.Lock()
				index := len(requests)
				if index >= 3 {
					stateLock.Unlock()
					cancel()
					return nil, errors.New("clock request observation overflow")
				}
				requests = append(requests, clockRequestObservation{
					Method:   request.Method,
					Path:     request.URL.Path,
					Started:  time.Since(start),
					Deadline: deadline.Sub(start),
				})
				methods = append(methods, request.Method)
				stateLock.Unlock()
				defer func() {
					stateLock.Lock()
					requests[index].Returned = time.Since(start)
					requests[index].Err = requestErr
					stateLock.Unlock()
				}()
				if index == 2 {
					cancel()
					return nil, errors.New("unexpected third clock request")
				}
				if !hasDeadline || request.URL.Scheme != "http" || request.URL.Host != "clock.example" ||
					!((request.Method == http.MethodGet && request.URL.Path == "/hello") ||
						(request.Method == http.MethodPost && request.URL.Path == "/connect/control")) {
					return nil, errors.New("unexpected clock request identity or deadline")
				}
				body := []byte("{}")
				var clockId Id
				if request.Method == http.MethodPost {
					requestBody, err := io.ReadAll(request.Body)
					if err != nil {
						return nil, err
					}
					var args ConnectControlArgs
					if err := json.Unmarshal(requestBody, &args); err != nil {
						return nil, err
					}
					packBytes, err := DecodeBase64(base64.StdEncoding, args.Pack)
					if err != nil {
						return nil, err
					}
					pack := &protocol.Pack{}
					err = ProtoUnmarshal(packBytes, pack)
					MessagePoolReturn(packBytes)
					if err != nil || len(pack.Frames) != 1 {
						return nil, fmt.Errorf("decode clock request: %v", err)
					}
					message, err := FromFrame(pack.Frames[0])
					if err != nil {
						return nil, err
					}
					clock, ok := message.(*protocol.StreamAuthorization)
					if !ok || len(clock.StreamId) != 0 || len(clock.AuthorizationGeneration) != 0 || clock.Retired || clock.ClockUnixMillis != 0 {
						return nil, errors.New("unexpected control request")
					}
					clockId, err = IdFromBytes(clock.ClockId)
					if err != nil || clockId == (Id{}) {
						return nil, errors.New("invalid clock request id")
					}
					responseFrame := RequireToFrameWithDefaultProtocolVersion(&protocol.StreamAuthorization{
						ClockId:         clock.ClockId,
						ClockUnixMillis: serverMillis,
					})
					responseBytes, err := ProtoMarshal(&protocol.Pack{Frames: []*protocol.Frame{responseFrame}})
					MessagePoolReturn(responseFrame.MessageBytes)
					if err != nil {
						return nil, err
					}
					encoded := EncodeBase64(base64.StdEncoding, responseBytes)
					MessagePoolReturn(responseBytes)
					body, err = json.Marshal(&ConnectControlResult{Pack: encoded})
					if err != nil {
						return nil, err
					}
				}
				// A warm native transport reports local request admission before
				// waiting for the remote response. Without this positive event,
				// the strategy correctly cancels an apparently unwritten route.
				trace := httptrace.ContextClientTrace(request.Context())
				wroteTrace := trace != nil && trace.WroteRequest != nil
				if request.Method == http.MethodPost && !wroteTrace {
					return nil, errors.New("clock POST has no write-progress trace")
				}
				if wroteTrace {
					trace.WroteRequest(httptrace.WroteRequestInfo{})
				}
				stateLock.Lock()
				requests[index].ClockId = clockId
				requests[index].Written = time.Since(start)
				requests[index].WriteTrace = wroteTrace
				stateLock.Unlock()
				// Fixed propagation plus processing is a lower bound with no
				// dial cost. The unchanged parent deadline still owns the wait.
				select {
				case <-request.Context().Done():
					stateLock.Lock()
					canceledErr = request.Context().Err()
					stateLock.Unlock()
					return nil, request.Context().Err()
				case <-time.After(roundTrip + time.Millisecond):
				}
				stateLock.Lock()
				completed = append(completed, request.Method)
				stateLock.Unlock()
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{},
					Body:       io.NopCloser(bytes.NewReader(body)),
					Request:    request,
				}, nil
			}),
		}
		oob := NewApiOutOfBandControl(ctx, strategy, "", "http://clock.example")
		defer func() {
			if err := oob.CloseAndWait(context.Background()); err != nil {
				t.Errorf("join API clock request: %v", err)
			}
		}()
		manager := &StreamManager{
			ctx:           ctx,
			client:        &Client{clientOob: oob},
			authorization: newStreamAuthorizationState(),
		}
		start = time.Now()
		manager.requestAuthorizationClock()
		if err := oob.CloseAndWait(context.Background()); err != nil {
			t.Fatal(err)
		}
		stateLock.Lock()
		methodSnapshot := append([]string(nil), methods...)
		completedSnapshot := append([]string(nil), completed...)
		canceledSnapshot := canceledErr
		requestSnapshot := append([]clockRequestObservation(nil), requests...)
		stateLock.Unlock()
		expectedMethods := []string{http.MethodGet, http.MethodPost}
		if proven {
			expectedMethods = []string{http.MethodPost}
		}
		if fmt.Sprint(methodSnapshot) != fmt.Sprint(expectedMethods) {
			t.Fatalf("request order=%v expected=%v requests=%+v", methodSnapshot, expectedMethods, requestSnapshot)
		}
		if len(requestSnapshot) != len(expectedMethods) {
			t.Fatalf("request witness count=%d expected=%d", len(requestSnapshot), len(expectedMethods))
		}
		for i, request := range requestSnapshot {
			expectedPath := "/connect/control"
			if request.Method == http.MethodGet {
				expectedPath = "/hello"
			}
			expectedStart := time.Duration(i) * (roundTrip + time.Millisecond)
			if request.Path != expectedPath || request.Started != expectedStart || request.Written != request.Started ||
				request.Deadline != requestSnapshot[0].Deadline || request.Deadline <= request.Written ||
				(request.Method == http.MethodPost && (request.ClockId == (Id{}) || !request.WriteTrace)) {
				t.Fatalf("clock request lost identity, ordering, write progress, or parent deadline: %+v", requestSnapshot)
			}
		}
		t.Logf("clock bootstrap requests=%+v", requestSnapshot)
		state := manager.authorization
		state.Lock()
		established := !state.clockLocal.IsZero() && state.clockServer == serverMillis
		pending := state.clockPending
		state.Unlock()
		if !established || pending {
			if proven || roundTrip != time.Second || time.Since(start) != 2*time.Second ||
				!errors.Is(canceledSnapshot, context.DeadlineExceeded) ||
				requestSnapshot[0].Deadline != 2*time.Second || requestSnapshot[0].Returned != roundTrip+time.Millisecond ||
				requestSnapshot[0].Err != nil || requestSnapshot[1].Returned != 2*time.Second ||
				!errors.Is(requestSnapshot[1].Err, context.DeadlineExceeded) ||
				fmt.Sprint(completedSnapshot) != fmt.Sprint([]string{http.MethodGet}) {
				t.Fatalf("unexpected bootstrap fixture failure: rtt=%s proven=%t elapsed=%s completed=%v canceled=%v",
					roundTrip, proven, time.Since(start), completedSnapshot, canceledSnapshot)
			}
			t.Fatalf("clock bootstrap rejected: rtt=%s proven=%t elapsed=%s started=%v completed=%v established=%t pending=%t",
				roundTrip, proven, time.Since(start), methodSnapshot, completedSnapshot, established, pending)
		}
		if fmt.Sprint(completedSnapshot) != fmt.Sprint(expectedMethods) {
			t.Fatalf("clock published before complete response: %v", completedSnapshot)
		}
		for _, request := range requestSnapshot {
			if request.Err != nil || request.Returned-request.Started != roundTrip+time.Millisecond {
				t.Fatalf("successful control did not complete its exact response interval: %+v", requestSnapshot)
			}
		}
	})
}

// Original red: warm socket still needs hello plus POST when delivery is unproven.
func TestStreamAuthorizationBootstrapUnprovenOneSecond(t *testing.T) {
	checkBootstrapClockHttpBudget(t, time.Second, false)
}

// A half-second RTT leaves room for both requests under the original bound.
func TestStreamAuthorizationBootstrapUnprovenHalfSecond(t *testing.T) {
	checkBootstrapClockHttpBudget(t, 500*time.Millisecond, false)
}

// Proven delivery bypasses hello; it is distinct from a merely warm HTTP pool.
func TestStreamAuthorizationBootstrapProvenOneSecond(t *testing.T) {
	checkBootstrapClockHttpBudget(t, time.Second, true)
}

// Original red: a transient error or timeout must not silently discard the
// bounded pending grant while still failing closed without a clock.
func TestStreamAuthorizationBootstrapFailureRetainsPending(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, failure := range []error{errors.New("synthetic clock failure"), context.DeadlineExceeded} {
		func() {
			oob := &bootstrapClockOob{send: func(_ context.Context, _ Id, callback OobResultFunction) {
				callback(nil, failure)
			}}
			client := newBootstrapClockClient(t, oob)
			manager := client.streamManager
			open := newBootstrapClockOpen(time.Now().UnixMilli())
			streamId, _ := IdFromBytes(open.StreamId)
			queueBootstrapClockOpen(t, manager, open)
			manager.requestAuthorizationClock()
			state := manager.authorization
			state.Lock()
			retained := state.pending[streamId]
			validClock := !state.clockLocal.IsZero()
			clockPending := state.clockPending
			recordCount := len(state.records)
			state.Unlock()
			if retained == nil || validClock || clockPending || recordCount != 0 || manager.IsStreamOpen(streamId) {
				t.Errorf("failure=%v retained=%t clock=%t pending=%t records=%d open=%t",
					failure, retained != nil, validClock, clockPending, recordCount, manager.IsStreamOpen(streamId))
			}
		}()
	}
}

// A new platform refresh retries the clock; it must replay the exact original
// absolute lease, not turn acquisition time into a fresh ninety-second lease.
func TestStreamAuthorizationBootstrapRefreshRecovers(t *testing.T) {
	assertMessagePoolOwnership(t)
	serverMillis := time.Now().UnixMilli()
	attempt := 0
	oob := &bootstrapClockOob{send: func(_ context.Context, clockId Id, callback OobResultFunction) {
		attempt++
		if attempt == 1 {
			callback(nil, context.DeadlineExceeded)
		} else {
			respondBootstrapClock(clockId, serverMillis, callback)
		}
	}}
	client := newBootstrapClockClient(t, oob)
	manager := client.streamManager
	tracker := newStreamOpenTestTracker(manager.streamBuffer)
	open := newBootstrapClockOpen(serverMillis)
	streamId, _ := IdFromBytes(open.StreamId)
	generation, _ := IdFromBytes(open.AuthorizationGeneration)
	queueBootstrapClockOpen(t, manager, open)
	manager.requestAuthorizationClock()
	if manager.IsStreamOpen(streamId) {
		t.Fatal("failed clock granted authority")
	}
	publication := tracker.expectPublication(streamId)
	defer publication.resume()
	queueBootstrapClockOpen(t, manager, open)
	manager.requestAuthorizationClock()
	publication.wait(t)
	state := manager.authorization
	state.Lock()
	record := state.records[streamId]
	var deadline time.Time
	var valid bool
	expected := state.clockLocal.Add(time.Duration(open.AuthorizationDeadlineUnixMillis-state.clockServer)*time.Millisecond - StreamAuthorizationClockMargin)
	if record != nil {
		deadline = record.deadline
		valid = record.generation == generation && deadline.Equal(expected)
	}
	state.Unlock()
	if attempt != 2 || !valid {
		t.Fatalf("refresh attempt=%d valid original lease=%t", attempt, valid)
	}
	frame := RequireToFrameWithDefaultProtocolVersion(open)
	err := manager.handleControlFrame(frame)
	MessagePoolReturn(frame.MessageBytes)
	if err != nil {
		t.Fatal(err)
	}
	state.Lock()
	record = state.records[streamId]
	unchanged := record != nil && record.deadline.Equal(deadline)
	state.Unlock()
	if !unchanged {
		t.Fatal("replaying the grant restarted its absolute deadline")
	}
}

// Revocation wins while the synchronous OOB launcher is held. A subsequent
// different generation cannot resurrect this permanently retired binding.
func TestStreamAuthorizationBootstrapRevocationBeforeResponse(t *testing.T) {
	checkBootstrapClockTerminalBeforeResponse(t, false)
}

// Closing authorization wins even when an external synchronous OOB launcher
// ignores context until released; the test owns and joins that launcher.
func TestStreamAuthorizationBootstrapCloseBeforeResponse(t *testing.T) {
	checkBootstrapClockTerminalBeforeResponse(t, true)
}

// Forces terminal authorization before a borrowed success callback without
// relying on a scheduler race or a negative timeout.
func checkBootstrapClockTerminalBeforeResponse(t *testing.T, closeClient bool) {
	t.Helper()
	assertMessagePoolOwnership(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	var releaseOnce sync.Once
	var launcherCtx context.Context
	serverMillis := time.Now().UnixMilli()
	oob := &bootstrapClockOob{send: func(ctx context.Context, clockId Id, callback OobResultFunction) {
		launcherCtx = ctx
		close(entered)
		<-release
		respondBootstrapClock(clockId, serverMillis, callback)
	}}
	client := newBootstrapClockClient(t, oob)
	manager := client.streamManager
	open := newBootstrapClockOpen(serverMillis)
	streamId, _ := IdFromBytes(open.StreamId)
	generation, _ := IdFromBytes(open.AuthorizationGeneration)
	queueBootstrapClockOpen(t, manager, open)
	go func() {
		defer close(done)
		manager.requestAuthorizationClock()
	}()
	defer func() {
		releaseOnce.Do(func() { close(release) })
		waitForStreamLifecycleSignal(t, done, "clock launcher did not join")
	}()
	waitForStreamLifecycleSignal(t, entered, "clock launcher did not enter")
	if closeClient {
		client.Close()
		waitForStreamLifecycleSignal(t, launcherCtx.Done(), "held launcher did not receive client cancellation")
	} else {
		manager.retireStreamAuthorization(streamId, open.AuthorizationGeneration)
	}
	releaseOnce.Do(func() { close(release) })
	waitForStreamLifecycleSignal(t, done, "clock launcher did not return")
	open.AuthorizationGeneration = NewId().Bytes()
	open.AuthorizationDeadlineUnixMillis += MaxStreamAuthorizationLease.Milliseconds()
	frame := RequireToFrameWithDefaultProtocolVersion(open)
	err := manager.handleControlFrame(frame)
	MessagePoolReturn(frame.MessageBytes)
	if err != nil {
		t.Fatal(err)
	}
	state := manager.authorization
	state.Lock()
	remaining := len(state.pending)
	clockSet := !state.clockLocal.IsZero()
	record := state.records[streamId]
	retired := record != nil && record.retired && record.generation == generation
	state.Unlock()
	if remaining != 0 || manager.IsStreamOpen(streamId) || (closeClient && clockSet) || (!closeClient && !retired) {
		t.Fatalf("terminal authorization lost: closed=%t pending=%d clock=%t retired=%t open=%t",
			closeClient, remaining, clockSet, retired, manager.IsStreamOpen(streamId))
	}
}
