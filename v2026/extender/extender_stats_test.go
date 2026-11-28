// The relay counter tests of EXTENDER.md section 5 phase 12 (O1, O7): a
// relayed session on each carrier moves known bytes in both directions and the
// snapshot carries exactly those bytes and reads, summed over the carriers;
// the decoy reverse proxy and the dns forwarder count nothing; and the
// snapshot is safe to read while relays run.

package extender

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/urnetwork/connect/v2026"
)

// One session of the counter tests. The two directions carry different chunk
// sizes, so a counter that took one direction for the other is visible, and
// each chunk is answered before the next is sent, so a chunk is one read of
// the relay on each side however the carrier framed it. The first is sent after
// the forward dial, since one sent before the tcp carrier has taken its stream over
// can be two reads (TestExtenderStatsKeepEveryByteOfAChunkTheHijackSplits).
const (
	testRelayIngressChunkByteCount = 64
	testRelayEgressChunkByteCount  = 96
	testRelayChunkCount            = 3
)

// The address the counter tests forward to. It is on the fixture's allowed
// host list, and the dial seam sends it to the raw endpoint rather than to the
// https destination, so the relayed bytes are the test's own and nothing else.
const testRelayHost = "dest.example"

// relayEndpoint is one raw destination: for every chunk a client sends it
// reads exactly that chunk and answers with a chunk of the other size. It
// speaks no protocol of its own, so every relayed byte is accounted for by the
// test.
type relayEndpoint struct {
	address string

	stateLock sync.Mutex
	dialCount int
	// closed and replaced by each dial
	dialed chan struct{}
}

func newRelayEndpoint(t *testing.T, loopbackIp string) *relayEndpoint {
	t.Helper()
	listener, err := net.Listen("tcp", net.JoinHostPort(loopbackIp, "0"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		listener.Close()
	})
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				ingressChunk := make([]byte, testRelayIngressChunkByteCount)
				egressChunk := bytes.Repeat([]byte("e"), testRelayEgressChunkByteCount)
				for {
					if _, err := io.ReadFull(conn, ingressChunk); err != nil {
						return
					}
					if _, err := conn.Write(egressChunk); err != nil {
						return
					}
				}
			}()
		}
	}()
	return &relayEndpoint{
		address: listener.Addr().String(),
		dialed:  make(chan struct{}),
	}
}

// The forward dial seam of the counter fixtures, which sends every allowed
// destination to the raw endpoint.
func (self *relayEndpoint) dialContext(
	ctx context.Context,
	network string,
	address string,
) (net.Conn, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", self.address)
	if err != nil {
		return nil, err
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.dialCount += 1
	close(self.dialed)
	self.dialed = make(chan struct{})
	return conn, nil
}

// Waits for this many forward dials in all. A carrier dials forward after it took
// its stream over, so a session that has seen its dial here sends every chunk to the
// relay.
func (self *relayEndpoint) waitForDialCount(ctx context.Context, dialCount int) error {
	for {
		self.stateLock.Lock()
		reached := dialCount <= self.dialCount
		dialed := self.dialed
		self.stateLock.Unlock()
		if reached {
			return nil
		}
		select {
		case <-dialed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// One relayed session over one carrier: chunkCount chunks toward the endpoint,
// each answered before the next is sent. The answer of a chunk is written by
// the endpoint only after the relay counted the chunk, and the relay counts an
// answer before it writes it on, so every count of the session has landed when
// this returns. The chunks wait for dialCount forward dials to the endpoint, which
// are this session's and those of any others running beside it.
func relayTestSession(fixture *extenderFixture, endpoint *relayEndpoint, dialCount int, carrier string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	conn, _, err := connect.DialExtender(
		ctx,
		fixture.connectSettings(),
		fixture.extenderConfig(carrier),
		&connect.ExtenderDial{
			DestinationHost: testRelayHost,
			DestinationPort: 443,
		},
	)
	if err != nil {
		return fmt.Errorf("%s dial: %w", carrier, err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(20 * time.Second)); err != nil {
		return fmt.Errorf("%s deadline: %w", carrier, err)
	}
	if err := endpoint.waitForDialCount(ctx, dialCount); err != nil {
		return fmt.Errorf("%s forward dial: %w", carrier, err)
	}

	ingressChunk := bytes.Repeat([]byte("i"), testRelayIngressChunkByteCount)
	egressChunk := make([]byte, testRelayEgressChunkByteCount)
	for i := range testRelayChunkCount {
		if _, err := conn.Write(ingressChunk); err != nil {
			return fmt.Errorf("%s chunk %d write: %w", carrier, i, err)
		}
		if _, err := io.ReadFull(conn, egressChunk); err != nil {
			return fmt.Errorf("%s chunk %d read: %w", carrier, i, err)
		}
	}
	return nil
}

// What one session adds to the counters.
func relayTestSessionStats(sessionCount int64) ExtenderStats {
	return ExtenderStats{
		IngressByteCount: sessionCount * testRelayChunkCount * testRelayIngressChunkByteCount,
		IngressReadCount: sessionCount * testRelayChunkCount,
		EgressByteCount:  sessionCount * testRelayChunkCount * testRelayEgressChunkByteCount,
		EgressReadCount:  sessionCount * testRelayChunkCount,
	}
}

// The extender of the counter tests: every forward dial reaches the raw
// endpoint.
func newRelayStatsFixture(t *testing.T, endpoint *relayEndpoint) *extenderFixture {
	t.Helper()
	return newExtenderFixture(t, "127.0.0.1", func(settings *ExtenderSettings) {
		settings.DialContext = endpoint.dialContext
	})
}

// Every carrier's relayed session lands on the same counters, in the direction
// the operator sees: the bytes a client sent toward the destination are the
// ingress and the bytes that came back are the egress, each with one read per
// chunk (O1).
func TestExtenderStatsCountEveryCarrierInBothDirections(t *testing.T) {
	endpoint := newRelayEndpoint(t, "127.0.0.1")
	fixture := newRelayStatsFixture(t, endpoint)

	if stats := fixture.server.Stats(); stats != (ExtenderStats{}) {
		t.Fatalf("a server that has relayed nothing counts %+v", stats)
	}

	carriers := []string{
		connect.ExtenderCarrierTcp,
		connect.ExtenderCarrierQuic,
		connect.ExtenderCarrierDns,
	}
	for i, carrier := range carriers {
		if err := relayTestSession(fixture, endpoint, i+1, carrier); err != nil {
			if extenderErr, ok := fixture.nextError(); ok {
				t.Fatalf("%v; extender: %v", err, extenderErr)
			}
			t.Fatal(err)
		}
		// the counters are cumulative, so each carrier is checked against the
		// sum of every session so far
		expected := relayTestSessionStats(int64(i + 1))
		if stats := fixture.server.Stats(); stats != expected {
			t.Fatalf("through %s stats = %+v, expected %+v", carrier, stats, expected)
		}
	}
}

// The decoy reverse proxy and the dns forwarder answer probers rather than
// relaying for a client, so neither moves the counters, over any carrier (O1).
func TestExtenderStatsIgnoreTheProxyAndTheDnsForwarder(t *testing.T) {
	answerBytes := newTestDnsAnswer(
		t,
		testForwardName,
		dnsmessage.TypeA,
		netip.MustParseAddr("192.0.2.9"),
		1,
	)
	fixture := newExtenderFixture(t, "127.0.0.1", func(settings *ExtenderSettings) {
		settings.DnsForward = func(
			ctx context.Context,
			qType dnsmessage.Type,
			name string,
		) ([]byte, bool) {
			return answerBytes, true
		}
	})

	// a prober that gets the real site behind a whitelisted name, over every
	// protocol the extender speaks, which covers the tcp and the quic carrier
	// (A5)
	for _, protocol := range testProxyProtocols {
		response := getThroughExtender(t, fixture, protocol, testSpoofName, "/")
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil {
			t.Fatalf("%s: %v", protocol, err)
		}
		if response.StatusCode != http.StatusOK {
			t.Fatalf("%s status = %d, expected %d", protocol, response.StatusCode, http.StatusOK)
		}
		if string(body) != `{"host":"`+testSpoofName+`"}` {
			t.Fatalf("%s body = %q", protocol, body)
		}
	}

	// and a plain query the forwarder answers on the dns carrier's socket (A6)
	conn := newTestDnsClient(t, fixture)
	queryBytes := newTestDnsQuery(t, 0x1234, testForwardName, dnsmessage.TypeA)
	responseBytes := exchangeTestDnsQuery(t, conn, queryBytes)
	var parser dnsmessage.Parser
	header, err := parser.Start(responseBytes)
	if err != nil {
		t.Fatal(err)
	}
	if !header.Response || header.ID != 0x1234 {
		t.Fatalf("the forwarder did not answer the query: %+v", header)
	}

	if stats := fixture.server.Stats(); stats != (ExtenderStats{}) {
		t.Fatalf("the reverse proxy and the dns forwarder counted %+v", stats)
	}
}

// A snapshot read while every carrier relays never tears a counter backward
// and never loses a chunk: the totals after the sessions are exactly what the
// sessions moved (O1). Run with -race, this is also the data race check of the
// counters.
func TestExtenderStatsAreSafeToReadWhileRelaying(t *testing.T) {
	endpoint := newRelayEndpoint(t, "127.0.0.1")
	fixture := newRelayStatsFixture(t, endpoint)

	carriers := []string{
		connect.ExtenderCarrierTcp,
		connect.ExtenderCarrierQuic,
		connect.ExtenderCarrierDns,
	}

	snapshotsDone := make(chan struct{})
	snapshotsStop := make(chan struct{})
	snapshotErrs := make(chan error, 1)
	go func() {
		defer close(snapshotsDone)
		previous := ExtenderStats{}
		for {
			stats := fixture.server.Stats()
			if stats.IngressByteCount < previous.IngressByteCount ||
				stats.IngressReadCount < previous.IngressReadCount ||
				stats.EgressByteCount < previous.EgressByteCount ||
				stats.EgressReadCount < previous.EgressReadCount {
				select {
				case snapshotErrs <- fmt.Errorf("stats went backward: %+v then %+v", previous, stats):
				default:
				}
				return
			}
			previous = stats
			select {
			case <-snapshotsStop:
				return
			default:
			}
			runtime.Gosched()
		}
	}()

	var sessions sync.WaitGroup
	sessionErrs := make(chan error, len(carriers))
	for _, carrier := range carriers {
		sessions.Add(1)
		go func() {
			defer sessions.Done()
			if err := relayTestSession(fixture, endpoint, len(carriers), carrier); err != nil {
				sessionErrs <- err
			}
		}()
	}
	sessions.Wait()
	close(snapshotsStop)
	<-snapshotsDone

	close(sessionErrs)
	for err := range sessionErrs {
		t.Error(err)
	}
	select {
	case err := <-snapshotErrs:
		t.Fatal(err)
	default:
	}
	if t.Failed() {
		return
	}

	expected := relayTestSessionStats(int64(len(carriers)))
	if stats := fixture.server.Stats(); stats != expected {
		t.Fatalf("stats after the concurrent sessions = %+v, expected %+v", stats, expected)
	}
}

// A listener whose connections stop net/http's hijack where it aborts its background
// read, and say when a read returns anything after that.
type hijackHoldListener struct {
	net.Listener
	// closed when a hijack reaches the hold
	held chan struct{}
	// closed by the test to let the hijack go on
	release chan struct{}
	// closed when the connection is read from during the hold
	readDuringHold chan struct{}
}

func (self *hijackHoldListener) Accept() (net.Conn, error) {
	conn, err := self.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &hijackHoldConn{Conn: conn, listener: self}, nil
}

type hijackHoldConn struct {
	net.Conn
	listener *hijackHoldListener
	holdOnce sync.Once
	readOnce sync.Once
	holding  atomic.Bool
}

// net/http aborts its background read with a deadline in the distant past, the
// first thing a hijack does, and nothing else sets one.
func (self *hijackHoldConn) SetReadDeadline(deadline time.Time) error {
	if !deadline.IsZero() && deadline.Before(time.Unix(2, 0)) {
		self.holdOnce.Do(func() {
			self.holding.Store(true)
			close(self.listener.held)
			<-self.listener.release
		})
	}
	return self.Conn.SetReadDeadline(deadline)
}

func (self *hijackHoldConn) Read(b []byte) (int, error) {
	n, err := self.Conn.Read(b)
	if 0 < n && self.holding.Load() {
		self.readOnce.Do(func() {
			close(self.listener.readDuringHold)
		})
	}
	return n, err
}

// A client may send as soon as it has the response, and the tcp carrier flushes that
// before it takes the stream from net/http. A chunk that lands between the two has
// its first byte taken by net/http's background read, and the relay reads that byte
// apart from the rest: every byte is relayed and counted, and the chunk is two reads.
// Under load this order comes up by itself, so the sessions above wait for the
// forward dial to keep their exact read counts.
func TestExtenderStatsKeepEveryByteOfAChunkTheHijackSplits(t *testing.T) {
	endpoint := newRelayEndpoint(t, "127.0.0.1")
	listener := &hijackHoldListener{
		held:           make(chan struct{}),
		release:        make(chan struct{}),
		readDuringHold: make(chan struct{}),
	}
	fixture := newExtenderFixture(t, "127.0.0.1", func(settings *ExtenderSettings) {
		settings.DialContext = endpoint.dialContext
		listen := settings.Listen
		settings.Listen = func(network string, address string) (net.Listener, error) {
			innerListener, err := listen(network, address)
			if err != nil {
				return nil, err
			}
			listener.Listener = innerListener
			return listener, nil
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	conn, _, err := connect.DialExtender(
		ctx,
		fixture.connectSettings(),
		fixture.extenderConfig(connect.ExtenderCarrierTcp),
		&connect.ExtenderDial{
			DestinationHost: testRelayHost,
			DestinationPort: 443,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(20 * time.Second)); err != nil {
		t.Fatal(err)
	}
	waitFor := func(reached <-chan struct{}, description string) {
		t.Helper()
		select {
		case <-reached:
		case <-ctx.Done():
			t.Fatalf("%s: %v", description, ctx.Err())
		}
	}
	waitFor(listener.held, "the hijack did not reach its hold")

	ingressChunk := bytes.Repeat([]byte("i"), testRelayIngressChunkByteCount)
	egressChunk := make([]byte, testRelayEgressChunkByteCount)
	if _, err := conn.Write(ingressChunk); err != nil {
		t.Fatal(err)
	}
	waitFor(listener.readDuringHold, "the background read did not see the chunk")
	close(listener.release)
	if _, err := io.ReadFull(conn, egressChunk); err != nil {
		t.Fatal(err)
	}
	for i := 1; i < testRelayChunkCount; i += 1 {
		if _, err := conn.Write(ingressChunk); err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadFull(conn, egressChunk); err != nil {
			t.Fatal(err)
		}
	}

	expected := relayTestSessionStats(1)
	expected.IngressReadCount += 1
	if stats := fixture.server.Stats(); stats != expected {
		t.Fatalf("stats = %+v, expected %+v", stats, expected)
	}
}
