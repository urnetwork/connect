// The bound of one forward dial (A7, A9, ForwardDialTimeout). The forward
// dial runs after the extender has answered the request, so a dial that never
// connects leaves the client -- the operator's activation probe included --
// holding a stream that says nothing, and before the bound the extender
// waited with it, silently, for as long as its dialer did (connect#230). Each
// dial goes through an injected egress seam. The dials themselves run on the
// synctest clock, so every instant is exact and a dial that is never ended
// shows as one that ends an hour later rather than as a hung test; the
// composed case runs over loopback sockets, as a probe does.
package extender

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
)

// One forward dial the seam saw: its network and address, and the deadline
// of its context.
type forwardDialTestCall struct {
	network     string
	address     string
	deadline    time.Time
	hasDeadline bool
}

// An egress seam that hangs: it answers only when its context ends, or an
// hour later when nothing ends it.
func forwardDialTestHang(calls chan<- forwardDialTestCall) func(ctx context.Context, network string, address string) (net.Conn, error) {
	return func(ctx context.Context, network string, address string) (net.Conn, error) {
		deadline, hasDeadline := ctx.Deadline()
		calls <- forwardDialTestCall{
			network:     network,
			address:     address,
			deadline:    deadline,
			hasDeadline: hasDeadline,
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Hour):
			return nil, errors.New("the forward dial was never ended")
		}
	}
}

// One failure the server reported to its ErrorHandler.
type forwardDialTestError struct {
	stage string
	err   error
}

// The default is 5 s, which is what the cases below and the operator's 10 s
// activation probe budget (C2) are reasoned against: half the budget, so a
// dial that cannot connect fails while the probe that asked for it can
// still see the stream close.
func TestExtenderForwardDialTimeoutDefault(t *testing.T) {
	if timeout := DefaultExtenderSettings().ForwardDialTimeout; timeout != 5*time.Second {
		t.Fatalf("the default forward dial timeout is %s, expected 5s", timeout)
	}
}

// A stream forward whose dial hangs is ended exactly ForwardDialTimeout after
// it started, on the client's family, and the failure reaches ErrorHandler
// as the forward dial stage with the deadline as its cause. Before the bound
// the seam saw no deadline and the dial ended only when something else did.
func TestExtenderForwardDialThatHangsEndsAtTheForwardDialTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := make(chan forwardDialTestCall, 1)
		reported := make(chan forwardDialTestError, 1)
		settings := DefaultExtenderSettings()
		settings.DialContext = forwardDialTestHang(calls)
		settings.ErrorHandler = func(stage string, err error) {
			reported <- forwardDialTestError{stage: stage, err: err}
		}
		server := &ExtenderServer{settings: settings}

		start := time.Now()
		forwardConn, err := server.dialForward(
			context.Background(),
			"192.0.2.10:52000",
			&protocol.ExtenderHeader{DestinationHost: "target.test", DestinationPort: 443},
		)
		elapsed := time.Since(start)
		if forwardConn != nil {
			forwardConn.Close()
			t.Fatal("a forward dial that hung returned a connection")
		}

		call := <-calls
		if call.network != "tcp4" || call.address != "target.test:443" {
			t.Fatalf("forward dial = %s %s, expected tcp4 target.test:443", call.network, call.address)
		}
		if !call.hasDeadline {
			t.Fatal("the forward dial had no deadline")
		}
		if call.deadline != start.Add(settings.ForwardDialTimeout) {
			t.Fatalf("the forward dial's deadline is %s after it started, expected %s", call.deadline.Sub(start), settings.ForwardDialTimeout)
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("forward dial err = %v, expected its deadline", err)
		}
		if elapsed != settings.ForwardDialTimeout {
			t.Fatalf("the forward dial ended %s after it started, expected %s", elapsed, settings.ForwardDialTimeout)
		}
		select {
		case observed := <-reported:
			if observed.stage != ExtenderErrorStageForwardDial || !errors.Is(observed.err, context.DeadlineExceeded) {
				t.Fatalf("reported %q %v, expected %q with its deadline", observed.stage, observed.err, ExtenderErrorStageForwardDial)
			}
		default:
			t.Fatal("the forward dial failure was not reported")
		}
	})
}

// The datagram relay's dial, which only resolves the name, is bounded the
// same way and reported as its own stage.
func TestExtenderDatagramForwardDialThatHangsEndsAtTheForwardDialTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := make(chan forwardDialTestCall, 1)
		reported := make(chan forwardDialTestError, 1)
		settings := DefaultExtenderSettings()
		settings.DialPacketContext = forwardDialTestHang(calls)
		settings.ErrorHandler = func(stage string, err error) {
			reported <- forwardDialTestError{stage: stage, err: err}
		}
		server := &ExtenderServer{settings: settings}

		start := time.Now()
		forwardConn, err := server.dialDatagramForward(context.Background(), "udp6", "target.test:443")
		elapsed := time.Since(start)
		if forwardConn != nil {
			forwardConn.Close()
			t.Fatal("a datagram forward dial that hung returned a connection")
		}

		call := <-calls
		if call.network != "udp6" || call.address != "target.test:443" {
			t.Fatalf("datagram forward dial = %s %s, expected udp6 target.test:443", call.network, call.address)
		}
		if !call.hasDeadline || call.deadline != start.Add(settings.ForwardDialTimeout) {
			t.Fatalf("the datagram forward dial's deadline is %t %s, expected %s after it started", call.hasDeadline, call.deadline.Sub(start), settings.ForwardDialTimeout)
		}
		if !errors.Is(err, context.DeadlineExceeded) || elapsed != settings.ForwardDialTimeout {
			t.Fatalf("the datagram forward dial ended after %s with %v, expected its deadline at %s", elapsed, err, settings.ForwardDialTimeout)
		}
		select {
		case observed := <-reported:
			if observed.stage != ExtenderErrorStageDatagramForwardDial || !errors.Is(observed.err, context.DeadlineExceeded) {
				t.Fatalf("reported %q %v, expected %q with its deadline", observed.stage, observed.err, ExtenderErrorStageDatagramForwardDial)
			}
		default:
			t.Fatal("the datagram forward dial failure was not reported")
		}
	})
}

// The bound is the dial's alone. A dial that connects a nanosecond before it
// runs out is a forward like any other, and the connection it returns
// outlives the dial's context: it still carries bytes after the bound has
// passed, and the extender reports nothing.
func TestExtenderForwardDialTimeoutBoundsOnlyTheDial(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		forwardConn, forwardPeer := net.Pipe()
		defer forwardPeer.Close()
		dialCtxs := make(chan context.Context, 1)
		settings := DefaultExtenderSettings()
		settings.DialContext = func(ctx context.Context, network string, address string) (net.Conn, error) {
			dialCtxs <- ctx
			time.Sleep(settings.ForwardDialTimeout - time.Nanosecond)
			return forwardConn, nil
		}
		settings.ErrorHandler = func(stage string, err error) {
			t.Errorf("reported %q %v for a dial that connected", stage, err)
		}
		server := &ExtenderServer{settings: settings}

		dialedConn, err := server.dialForward(
			context.Background(),
			"[2001:db8::10]:52000",
			&protocol.ExtenderHeader{DestinationHost: "target.test", DestinationPort: 443},
		)
		if err != nil {
			t.Fatalf("a dial that connected inside the bound failed: %v", err)
		}
		defer dialedConn.Close()
		if dialCtx := <-dialCtxs; dialCtx.Err() == nil {
			t.Fatal("the dial's context outlived the dial")
		}

		time.Sleep(2 * settings.ForwardDialTimeout)
		go func() {
			forwardPeer.Write([]byte{0x7f})
		}()
		received := make([]byte, 1)
		if _, err := dialedConn.Read(received); err != nil || received[0] != 0x7f {
			t.Fatalf("the forward connection stopped carrying bytes after the bound: %v", err)
		}
	})
}

// A non-positive bound leaves the dial to its connection's context, which is
// what ends it then: here the context the connection was served with.
func TestExtenderForwardDialWithoutABoundEndsWithItsConnection(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := make(chan forwardDialTestCall, 1)
		settings := DefaultExtenderSettings()
		settings.ForwardDialTimeout = 0
		settings.DialContext = forwardDialTestHang(calls)
		server := &ExtenderServer{settings: settings}

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		start := time.Now()
		_, err := server.dialForward(
			ctx,
			"192.0.2.10:52000",
			&protocol.ExtenderHeader{DestinationHost: "target.test", DestinationPort: 443},
		)
		if call := <-calls; call.deadline != start.Add(30*time.Second) {
			t.Fatalf("the unbounded dial's deadline is %s after it started, expected the connection's 30s", call.deadline.Sub(start))
		}
		if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) != 30*time.Second {
			t.Fatalf("the unbounded dial ended after %s with %v, expected the connection's deadline", time.Since(start), err)
		}
	})
}

// The composed path, over real loopback sockets: the operator's forward probe
// -- an extender http client's `GET /hello` through the tcp carrier -- to a
// destination whose dial hangs is released by the extender closing its stream
// when the bound runs out, not by the client's own 10 s tls budget, and the
// extender reports the forward dial stage. The extender has already answered
// the request by then, so the close is all the probe can see of the failure;
// before the bound the probe waited out its own budget. The bound is short
// here so that the two cannot be confused.
func TestExtenderClosesTheStreamOfAForwardDialThatHangs(t *testing.T) {
	dialed := make(chan string, 1)
	fixture := newExtenderFixture(t, "127.0.0.1", func(settings *ExtenderSettings) {
		settings.ForwardDialTimeout = 200 * time.Millisecond
		settings.DialContext = func(ctx context.Context, network string, address string) (net.Conn, error) {
			select {
			case dialed <- network + " " + address:
			default:
			}
			<-ctx.Done()
			return nil, ctx.Err()
		}
	})
	connectSettings := fixture.connectSettings()
	if connectSettings.TlsTimeout != 10*time.Second {
		t.Fatalf("the test assumes a 10s client tls timeout, got %s", connectSettings.TlsTimeout)
	}
	client := connect.NewExtenderHttpClient(connectSettings, fixture.extenderConfig(connect.ExtenderCarrierTcp))
	defer client.CloseIdleConnections()

	start := time.Now()
	response, err := client.Get("https://dest.example/hello")
	elapsed := time.Since(start)
	if err == nil {
		response.Body.Close()
		t.Fatal("the hello was answered through a forward that never connected")
	}
	select {
	case dial := <-dialed:
		if dial != "tcp4 dest.example:443" {
			t.Fatalf("forward dial = %q, expected tcp4 dest.example:443", dial)
		}
	default:
		t.Fatal("the extender did not dial forward")
	}
	if connectSettings.TlsTimeout/2 <= elapsed {
		t.Fatalf("the probe was released %s after it started, expected the extender to close its stream at %s", elapsed, fixture.settings.ForwardDialTimeout)
	}
	reported, ok := fixture.nextError()
	if !ok {
		t.Fatal("the forward dial failure was not reported")
	}
	if !strings.HasPrefix(reported.Error(), ExtenderErrorStageForwardDial+": ") || !errors.Is(reported, context.DeadlineExceeded) {
		t.Fatalf("reported %v, expected %q with its deadline", reported, ExtenderErrorStageForwardDial)
	}
}
