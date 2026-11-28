package connect

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// The activation post's own budget (EXTENDER.md G3, S8; support inbox 8791).
//
// The operator answers an activation only once its probe of this host has run.
// For a host whose tcp carrier the probe does not reach, that is the tcp
// challenge and then the webrtc test, which ends up to 15 s after the judging
// began, and the store after it, so the answer can start past 15 s. The
// activator gives its post its RequestTimeout, 60 s, but the strategy bounded
// every request by its own RequestTimeout and by its transport's wait for the
// response headers (ConnectTimeout), 15 s each, whatever the caller asked for.
// So such an activation was given up on before it was answered, and the
// extender backed off from an answer that was on its way.
//
// The posts run through a real direct strategy with the default settings, as
// the role builds one, to an operator in memory: every dial is one end of a
// net.Pipe whose other end answers the one request written to it. The api url
// is plain http, so the fixture needs no certificate; the bounds under test are
// the strategy's and its transport's, which an https request meets alike. Each
// test runs in a synctest bubble, so the operator's delays and every bound pass
// in bubble time, with no wait and no network.

// The api url of the operator in memory, which every dial reaches.
const testActivateTimeoutApiUrl = "http://192.0.2.4"

// testActivateTimeoutOperator answers each connection's one request. A hello
// is answered at once, and its connection closed. A post is answered
// `answerDelay` after it was read, or never when `answerDelay` is negative;
// one to the activation path with an activation of v4. Its connection is left
// open after the answer, so what closes it is the client, and when the client
// closes it is recorded (`postEnds`).
type testActivateTimeoutOperator struct {
	answerDelay time.Duration
	done        chan struct{}
	waitGroup   *sync.WaitGroup

	// the path of each post, once it is read
	postReads chan string
	// the time the client closed each post's connection
	postEnds chan time.Time

	stateLock sync.Mutex
	postCount int
}

func newTestActivateTimeoutOperator(t *testing.T, answerDelay time.Duration) *testActivateTimeoutOperator {
	t.Helper()
	operator := &testActivateTimeoutOperator{
		answerDelay: answerDelay,
		done:        make(chan struct{}),
		waitGroup:   &sync.WaitGroup{},
		postReads:   make(chan string, 16),
		postEnds:    make(chan time.Time, 16),
	}
	// runs inside the bubble, before it ends
	t.Cleanup(func() {
		close(operator.done)
		operator.waitGroup.Wait()
	})
	return operator
}

// The strategy's stream dial: one end of a new pipe, whose other end serves one
// request.
func (self *testActivateTimeoutOperator) dial(ctx context.Context, network string, address string) (net.Conn, error) {
	clientConn, serverConn := net.Pipe()
	self.waitGroup.Add(1)
	go func() {
		defer self.waitGroup.Done()
		defer serverConn.Close()
		self.serve(serverConn)
	}()
	return clientConn, nil
}

func (self *testActivateTimeoutOperator) serve(conn net.Conn) {
	request, err := http.ReadRequest(bufio.NewReader(conn))
	if err != nil {
		// a client that gave up before it wrote
		return
	}
	io.Copy(io.Discard, request.Body)
	request.Body.Close()

	if request.Method != http.MethodPost {
		self.answer(conn, []byte(`{"client_address":"198.51.100.7:41001"}`), true)
		return
	}

	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.postCount += 1
	}()
	select {
	case self.postReads <- request.URL.Path:
	default:
	}

	// the client sends nothing more on the connection, so this read returns
	// only once the client closes it, or the operator does
	clientClosed := make(chan struct{})
	self.waitGroup.Add(1)
	go func() {
		defer self.waitGroup.Done()
		defer close(clientClosed)
		io.Copy(io.Discard, conn)
	}()
	clientEnd := func() {
		select {
		case self.postEnds <- time.Now():
		default:
		}
	}

	var answerAfter <-chan time.Time
	if 0 <= self.answerDelay {
		answerAfter = time.After(self.answerDelay)
	}
	select {
	case <-answerAfter:
	case <-clientClosed:
		clientEnd()
		return
	case <-self.done:
		return
	}

	body := []byte(`{}`)
	if request.URL.Path == ExtenderActivatePath {
		body, err = json.Marshal(&ExtenderActivateResult{
			Activated: true,
			Ip:        "198.51.100.7",
			IpVersion: 4,
			Carriers:  []string{ExtenderCarrierWebRtc},
		})
		if err != nil {
			return
		}
	}
	if !self.answer(conn, body, false) {
		return
	}
	select {
	case <-clientClosed:
		clientEnd()
	case <-self.done:
	}
}

// Writes one 200 answer. `close` says the connection closes after it, which the
// client then does on its own. Whether it was written.
func (self *testActivateTimeoutOperator) answer(conn net.Conn, body []byte, close bool) bool {
	connectionHeader := ""
	if close {
		connectionHeader = "Connection: close\r\n"
	}
	_, err := fmt.Fprintf(
		conn,
		"HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\n%s\r\n%s",
		len(body),
		connectionHeader,
		body,
	)
	return err == nil
}

func (self *testActivateTimeoutOperator) posts() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.postCount
}

// A direct strategy with the default settings, as the role builds one, whose
// every stream dial reaches `operator`.
func newTestActivateTimeoutStrategy(ctx context.Context, operator *testActivateTimeoutOperator) *ClientStrategy {
	settings := DefaultClientStrategySettings()
	settings.Log = NewNoopLogger()
	settings.ConnectSettings.Log = NewNoopLogger()
	// the normal dialer alone, so a request reaches the operator once a round
	settings.EnableResilient = false
	settings.ConnectSettings.DialContextSettings = &DialContextSettings{
		DialContext: operator.dial,
	}
	return NewDirectClientStrategy(ctx, settings, 0)
}

// An activator of v4 alone with the default settings, its RequestTimeout among
// them, posting through `clientStrategy`. It offers tcp and webrtc, as a
// provider does.
func newTestActivateTimeoutActivator(ctx context.Context, clientStrategy *ClientStrategy) *ExtenderActivator {
	settings := DefaultExtenderActivatorSettings()
	settings.Log = NewNoopLogger()
	settings.ApiUrlV4 = testActivateTimeoutApiUrl
	settings.ByJwt = func() string {
		return "test-jwt"
	}
	settings.ClientStrategy = clientStrategy
	settings.PublicKey = []byte("test extender public key")
	settings.Carriers = func() []string {
		return []string{ExtenderCarrierTcp, ExtenderCarrierWebRtc}
	}
	settings.IpVersionSupported = func(ipVersion int) bool {
		return ipVersion == 4
	}
	settings.AddNetworkChangeListener = func(listener func()) func() {
		return func() {}
	}
	// the bubble clock never sleeps, so there is no resume to watch
	settings.ResumeCheckTimeout = 0
	return NewExtenderActivator(ctx, settings)
}

// The v4 status at the activator's first recorded outcome.
func testActivateTimeoutFirstOutcome(activator *ExtenderActivator) *ExtenderFamilyActivationStatus {
	for {
		version, change := activator.ChangeMonitor().Get()
		if 0 < version {
			return activator.Status().Family(4)
		}
		<-change
	}
}

// An activation the operator answers after 20 s -- past the strategy's bounds
// and within the activator's own -- is activated by the one post that went out.
// The post's connection, which only that post used, is closed once its answer
// is read.
func TestExtenderActivatorWaitsItsOwnBudgetForTheAnswer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const answerDelay = 20 * time.Second
		strategySettings := DefaultClientStrategySettings()
		activatorSettings := DefaultExtenderActivatorSettings()
		if answerDelay <= strategySettings.RequestTimeout ||
			answerDelay <= strategySettings.ConnectTimeout ||
			activatorSettings.RequestTimeout <= answerDelay {
			t.Fatalf(
				"the answer at %s must come after the strategy's %s and %s and within the activator's %s",
				answerDelay,
				strategySettings.RequestTimeout,
				strategySettings.ConnectTimeout,
				activatorSettings.RequestTimeout,
			)
		}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		operator := newTestActivateTimeoutOperator(t, answerDelay)
		clientStrategy := newTestActivateTimeoutStrategy(ctx, operator)
		defer clientStrategy.Close()

		startTime := time.Now()
		activator := newTestActivateTimeoutActivator(ctx, clientStrategy)
		defer activator.Close()

		status := testActivateTimeoutFirstOutcome(activator)
		elapsed := time.Since(startTime)
		if status == nil || !status.Activated {
			t.Fatalf("after %s the v4 status = %+v; expected the activation the operator answered after %s", elapsed, status, answerDelay)
		}
		if elapsed != answerDelay {
			t.Fatalf("activated after %s; expected after the operator's %s", elapsed, answerDelay)
		}
		if posts := operator.posts(); posts != 1 {
			t.Fatalf("%d posts reached the operator; expected 1", posts)
		}

		synctest.Wait()
		select {
		case endTime := <-operator.postEnds:
			if endTime != startTime.Add(answerDelay) {
				t.Fatalf("the post's connection closed after %s; expected at its answer, after %s", endTime.Sub(startTime), answerDelay)
			}
		default:
			t.Fatal("the post's connection was left open after its answer was read")
		}
	})
}

// The activator's own budget still ends a post: one the operator never answers
// is recorded as failed at the activator's RequestTimeout.
func TestExtenderActivatorEndsAnUnansweredPostAtItsBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		operator := newTestActivateTimeoutOperator(t, -1)
		clientStrategy := newTestActivateTimeoutStrategy(ctx, operator)
		defer clientStrategy.Close()

		startTime := time.Now()
		activator := newTestActivateTimeoutActivator(ctx, clientStrategy)
		defer activator.Close()

		status := testActivateTimeoutFirstOutcome(activator)
		elapsed := time.Since(startTime)
		if status == nil || status.Activated || status.LastError == "" {
			t.Fatalf("after %s the v4 status = %+v; expected a failed activation", elapsed, status)
		}
		if requestTimeout := DefaultExtenderActivatorSettings().RequestTimeout; elapsed != requestTimeout {
			t.Fatalf("the unanswered post ended after %s; expected at the activator's %s", elapsed, requestTimeout)
		}
		if posts := operator.posts(); posts != 1 {
			t.Fatalf("%d posts reached the operator; expected 1", posts)
		}
	})
}

// Closing the activator ends its post in flight at once: Close returns and the
// post's connection is closed with no bubble time passing.
func TestExtenderActivatorCloseEndsItsPostInFlight(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		operator := newTestActivateTimeoutOperator(t, -1)
		clientStrategy := newTestActivateTimeoutStrategy(ctx, operator)
		defer clientStrategy.Close()

		activator := newTestActivateTimeoutActivator(ctx, clientStrategy)
		defer activator.Close()

		if path := <-operator.postReads; path != ExtenderActivatePath {
			t.Fatalf("the operator read a post to %q; expected %q", path, ExtenderActivatePath)
		}
		closeTime := time.Now()
		activator.Close()
		if elapsed := time.Since(closeTime); elapsed != 0 {
			t.Fatalf("Close returned after %s; expected at once", elapsed)
		}

		synctest.Wait()
		select {
		case endTime := <-operator.postEnds:
			if endTime != closeTime {
				t.Fatalf("the post's connection closed %s after Close; expected at once", endTime.Sub(closeTime))
			}
		default:
			t.Fatal("the post's connection was left open after Close")
		}
	})
}

// Every other request on the same strategy keeps the strategy's bounds: a post
// the operator answers after 20 s ends at the strategy's RequestTimeout,
// though its caller's context allows the activator's 60 s.
func TestExtenderActivateBudgetIsTheActivationsAlone(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		operator := newTestActivateTimeoutOperator(t, 20*time.Second)
		clientStrategy := newTestActivateTimeoutStrategy(ctx, operator)
		defer clientStrategy.Close()

		requestCtx, requestCancel := context.WithTimeout(ctx, DefaultExtenderActivatorSettings().RequestTimeout)
		defer requestCancel()
		startTime := time.Now()
		_, err := HttpPostWithStrategyRaw(
			requestCtx,
			clientStrategy,
			testActivateTimeoutApiUrl+"/network/other",
			[]byte(`{}`),
			"test-jwt",
		)
		elapsed := time.Since(startTime)
		if err == nil {
			t.Fatalf("the post was answered after %s; expected it to end at the strategy's bound", elapsed)
		}
		if requestTimeout := DefaultClientStrategySettings().RequestTimeout; elapsed != requestTimeout {
			t.Fatalf("the post ended after %s (%s); expected at the strategy's %s", elapsed, err, requestTimeout)
		}
	})
}

// A request whose budget no bound of the strategy would cut short keeps the
// strategy's cached client: one with no budget of its own, whatever its
// bounds -- an unbounded response-header wait (a ConnectTimeout of zero or
// less) included -- and one whose budget is within them. Only a positive
// budget that a positive bound would end first takes a client of its own.
func TestHttpSerialWithinTheStrategysBoundsKeepsTheCachedClient(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, connectTimeout := range []time.Duration{15 * time.Second, 0, -1} {
			for _, requestTimeout := range []time.Duration{0, 10 * time.Second} {
				ctx, cancel := context.WithCancel(context.Background())
				settings := DefaultClientStrategySettings()
				settings.ConnectTimeout = connectTimeout
				// only a client of its own dials. Its dial ends the request at
				// once, which would otherwise retry the route, failing as fast,
				// for as long as its deadline allows
				var ownClientDials atomic.Int32
				settings.ConnectSettings.DialContextSettings = &DialContextSettings{
					DialContext: func(ctx context.Context, network string, address string) (net.Conn, error) {
						ownClientDials.Add(1)
						cancel()
						return nil, errors.New("test: a client of its own dialed")
					},
				}
				posts := 0
				dialer := &clientDialer{
					description:     "cached",
					minimumWeight:   1,
					settings:        settings,
					successCount:    1,
					lastSuccessTime: time.Now(),
					httpClient: &http.Client{Transport: serialTestRoundTripper(func(request *http.Request) (*http.Response, error) {
						if request.Method == http.MethodPost {
							posts += 1
						}
						return &http.Response{
							StatusCode: http.StatusOK,
							Header:     http.Header{},
							Body:       io.NopCloser(bytes.NewBufferString(`{}`)),
							Request:    request,
						}, nil
					})},
				}
				strategy := &ClientStrategy{
					ctx:               ctx,
					log:               NewNoopLogger(),
					settings:          settings,
					dialers:           map[*clientDialer]bool{dialer: true},
					extenderIpSecrets: map[netip.Addr]string{},
				}
				request, err := http.NewRequestWithContext(ctx, http.MethodPost, testActivateTimeoutApiUrl+"/auth-client", bytes.NewBufferString(`{}`))
				if err != nil {
					t.Fatal(err)
				}
				hello, err := http.NewRequestWithContext(ctx, http.MethodGet, testActivateTimeoutApiUrl+"/hello", nil)
				if err != nil {
					t.Fatal(err)
				}
				result, err := strategy.httpSerial(request, hello, requestTimeout)
				if err != nil || result == nil || posts != 1 || ownClientDials.Load() != 0 {
					t.Errorf(
						"ConnectTimeout %s, budget %s: err=%v posts=%d dials of a client of its own=%d; expected the cached client to answer the one post",
						connectTimeout, requestTimeout, err, posts, ownClientDials.Load(),
					)
				}
				cancel()
			}
		}
	})
}
