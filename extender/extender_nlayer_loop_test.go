// The loop check of an NLayer extender (EXTENDER.md A11): the tls client random
// of a stream's first inner record, held for the life of its relay, so a
// request that comes back to an extender it already crossed is refused there.
//
// The parser is pinned against real ClientHellos, captured from crypto/tls over
// a pipe, and against every way the start of a stream can fail to be one. The
// relay tests use a plain echo server as the destination, so what a client
// writes is what it reads back only if every byte crossed the chain in order,
// the bytes the check read included.

package extender

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"io"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// A reader that yields one byte forever, which makes a tls client's random
// known: every byte of it is that byte, wherever the random is read from.
type constantByteReader struct {
	value byte
}

// Implements io.Reader: every byte is the value.
func (self constantByteReader) Read(b []byte) (int, error) {
	for i := range b {
		b[i] = self.value
	}
	return len(b), nil
}

// The first record a tls client writes, which is its ClientHello, captured from
// a pipe nothing answers.
func captureClientHello(t *testing.T, tlsConfig *tls.Config) []byte {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	handshakeDone := make(chan struct{})
	go func() {
		defer close(handshakeDone)
		// fails once the pipe closes, which is all that ends it
		tls.Client(clientConn, tlsConfig).Handshake()
	}()
	defer func() {
		serverConn.Close()
		clientConn.Close()
		<-handshakeDone
	}()
	serverConn.SetReadDeadline(time.Now().Add(10 * time.Second))
	recordHeader := make([]byte, tlsRecordHeaderByteCount)
	if _, err := io.ReadFull(serverConn, recordHeader); err != nil {
		t.Fatal(err)
	}
	recordBody := make([]byte, binary.BigEndian.Uint16(recordHeader[3:5]))
	if _, err := io.ReadFull(serverConn, recordBody); err != nil {
		t.Fatal(err)
	}
	return append(recordHeader, recordBody...)
}

// The random of a real ClientHello is read from exactly its place in the first
// record, for a tls 1.3 and a tls 1.2 client alike; every proper prefix of the
// record is still a possible ClientHello with no random yet; and two hellos
// have two randoms.
func TestTlsClientHelloRandomIsReadFromTheFirstRecord(t *testing.T) {
	for _, maxVersion := range []uint16{tls.VersionTLS13, tls.VersionTLS12} {
		helloBytes := captureClientHello(t, &tls.Config{
			ServerName:         "dest.example",
			InsecureSkipVerify: true,
			MaxVersion:         maxVersion,
			Rand:               constantByteReader{value: 0xa5},
		})
		clientRandom, ok := tlsClientHelloRandom(helloBytes)
		if !ok {
			t.Fatalf("version %x: a real ClientHello was not read", maxVersion)
		}
		// a window off by one byte either way takes in the legacy version or
		// the session id length, neither of which is the reader's byte
		if clientRandom != [32]byte(bytes.Repeat([]byte{0xa5}, 32)) {
			t.Fatalf("version %x: random = %x", maxVersion, clientRandom)
		}
		for n := 0; n < tlsClientHelloPrefixByteCount; n += 1 {
			if !isTlsClientHelloPrefix(helloBytes[:n]) {
				t.Fatalf("version %x: the first %d bytes of a ClientHello were ruled out", maxVersion, n)
			}
			if _, ok := tlsClientHelloRandom(helloBytes[:n]); ok {
				t.Fatalf("version %x: a random was read from %d bytes", maxVersion, n)
			}
		}
	}

	first := captureClientHello(t, &tls.Config{ServerName: "dest.example", InsecureSkipVerify: true})
	second := captureClientHello(t, &tls.Config{ServerName: "dest.example", InsecureSkipVerify: true})
	firstRandom, firstOk := tlsClientHelloRandom(first)
	secondRandom, secondOk := tlsClientHelloRandom(second)
	if !firstOk || !secondOk {
		t.Fatal("a real ClientHello was not read")
	}
	if firstRandom == secondRandom {
		t.Fatal("two ClientHellos read as one random")
	}
	if againRandom, _ := tlsClientHelloRandom(slices.Clone(first)); againRandom != firstRandom {
		t.Fatal("the same bytes read as another random")
	}
}

// Everything that is not the start of a ClientHello record is ruled out at the
// first byte that shows it, and yields no random.
func TestTlsClientHelloPrefixRulesOutEverythingElse(t *testing.T) {
	hello := captureClientHello(t, &tls.Config{ServerName: "dest.example", InsecureSkipVerify: true})
	with := func(offset int, value ...byte) []byte {
		b := slices.Clone(hello)
		copy(b[offset:], value)
		return b
	}
	cases := []struct {
		description string
		b           []byte
	}{
		{description: "an http request line", b: []byte("GET / HTTP/1.1\r\nHost: dest.example\r\n\r\n")},
		{description: "a v1 extender length", b: []byte{0x00, 0x00, 0x00, 0x40}},
		{description: "an alert record", b: with(0, 21)},
		{description: "an application data record", b: with(0, 23)},
		{description: "a record version of 2.x", b: with(1, 0x02)},
		{description: "a record too short to hold the random", b: with(3, 0x00, 0x25)},
		{description: "a record longer than a plaintext record", b: with(3, 0x40, 0x01)},
		{description: "a ServerHello", b: with(5, 2)},
		{description: "a hello too short to hold the random", b: with(6, 0x00, 0x00, 0x21)},
		{description: "a legacy version of 2.x", b: with(9, 0x02)},
		{description: "a truncated ClientHello", b: hello[:tlsClientHelloPrefixByteCount-1]},
		{description: "nothing", b: []byte{}},
	}
	for _, c := range cases {
		if _, ok := tlsClientHelloRandom(c.b); ok {
			t.Errorf("%s yielded a random", c.description)
		}
	}
	// the shortest prefix that rules each out is judged at its own byte
	for _, c := range cases[:len(cases)-2] {
		if isTlsClientHelloPrefix(c.b) {
			t.Errorf("%s is still a possible ClientHello", c.description)
		}
	}
	if !isTlsClientHelloPrefix(hello[:tlsClientHelloPrefixByteCount-1]) {
		t.Error("a truncated ClientHello was ruled out")
	}
}

// A tcp server that writes banner, when there is one, to every connection and
// then echoes what it reads, until the test ends.
func newPlainEchoServer(t *testing.T, banner []byte) string {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var connsLock sync.Mutex
	conns := []net.Conn{}
	var connWorkers sync.WaitGroup
	connWorkers.Add(1)
	go func() {
		defer connWorkers.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			connsLock.Lock()
			conns = append(conns, conn)
			connsLock.Unlock()
			connWorkers.Add(1)
			go func() {
				defer connWorkers.Done()
				defer conn.Close()
				if 0 < len(banner) {
					if _, err := conn.Write(banner); err != nil {
						return
					}
				}
				io.Copy(conn, conn)
			}()
		}
	}()
	t.Cleanup(func() {
		listener.Close()
		connsLock.Lock()
		for _, conn := range conns {
			conn.Close()
		}
		connsLock.Unlock()
		connWorkers.Wait()
	})
	return listener.Addr().String()
}

// A chain of two whose last layer forwards dest.example to a plain echo
// server, for inner streams that are not the destination's tls.
func newNLayerPlainChain(
	t *testing.T,
	banner []byte,
	configure func(settings *ExtenderSettings),
) (*extenderFixture, *extenderFixture) {
	t.Helper()
	echoAddress := newPlainEchoServer(t, banner)
	b := newExtenderFixture(t, "127.0.0.1", func(settings *ExtenderSettings) {
		dialContext := settings.DialContext
		settings.DialContext = func(ctx context.Context, network string, address string) (net.Conn, error) {
			if host, _, err := net.SplitHostPort(address); err == nil && host == "dest.example" {
				return (&net.Dialer{}).DialContext(ctx, "tcp4", echoAddress)
			}
			return dialContext(ctx, network, address)
		}
	})
	a := newExtenderFixture(t, "127.0.0.1", func(settings *ExtenderSettings) {
		settings.NLayerHops = []*connect.ExtenderConfig{b.extenderConfig(connect.ExtenderCarrierTcp)}
		if configure != nil {
			configure(settings)
		}
	})
	return a, b
}

// A plain dial of dest.example through the first layer's carrier, the raw
// relayed stream.
func dialPlainThroughNLayer(t *testing.T, ctx context.Context, a *extenderFixture, carrier string) net.Conn {
	t.Helper()
	conn, err := connect.NewExtenderDialContext(a.connectSettings(), a.extenderConfig(carrier))(
		ctx, "tcp", "dest.example:443",
	)
	if err != nil {
		t.Fatal(err)
	}
	return conn
}

// Writes b and reads the same bytes back, which crossed both layers and the
// echo in order only if nothing was dropped, added or reordered.
func echoThroughNLayer(conn net.Conn, b []byte) error {
	if _, err := conn.Write(b); err != nil {
		return err
	}
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	echoBytes := make([]byte, len(b))
	if _, err := io.ReadFull(conn, echoBytes); err != nil {
		return err
	}
	if !bytes.Equal(echoBytes, b) {
		return io.ErrUnexpectedEOF
	}
	return nil
}

// A plain inner stream is relayed unchanged through an NLayer extender, the
// bytes the loop check read ahead of the rest: one that is not tls at all, one
// that starts like a handshake record but is no ClientHello, a ClientHello
// itself, one too short for the check to decide, which it waits out, and one
// whose server speaks first, which it waits out too. Nothing is refused as a
// loop, on the tcp carrier and on quic, whose stream carries http3 framing
// under the read.
func TestNLayerRelaysAPlainInnerStreamUnchanged(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	hello := captureClientHello(t, &tls.Config{ServerName: "dest.example", InsecureSkipVerify: true})
	notHello := slices.Clone(hello)
	notHello[5] = 2

	// short, since one case below is a stream the check has to wait out
	a, b := newNLayerPlainChain(t, nil, func(settings *ExtenderSettings) {
		settings.NLayerClientHelloTimeout = 500 * time.Millisecond
	})
	cases := []struct {
		description string
		b           []byte
	}{
		{description: "an http request", b: []byte("GET / HTTP/1.1\r\nHost: dest.example\r\n\r\n")},
		{description: "a handshake record that is not a ClientHello", b: notHello},
		{description: "a ClientHello", b: hello},
		{description: "a stream shorter than a ClientHello's random", b: []byte{22, 3, 1}},
	}
	for _, carrier := range []string{connect.ExtenderCarrierTcp, connect.ExtenderCarrierQuic} {
		for _, c := range cases {
			conn := dialPlainThroughNLayer(t, ctx, a, carrier)
			if err := echoThroughNLayer(conn, c.b); err != nil {
				t.Fatalf("%s %s: %v; %s", carrier, c.description, err, nlayerErrorsOf(a, b))
			}
			// a second exchange on the same stream follows the first unchanged
			if err := echoThroughNLayer(conn, []byte("and then more")); err != nil {
				t.Fatalf("%s %s, second exchange: %v", carrier, c.description, err)
			}
			conn.Close()
		}
	}

	// the server speaks first: the client writes nothing until the banner, so
	// the check waits out its timeout and then relays
	const helloTimeout = 300 * time.Millisecond
	banner := []byte("220 plain.example ready\r\n")
	bannerA, bannerB := newNLayerPlainChain(t, banner, func(settings *ExtenderSettings) {
		settings.NLayerClientHelloTimeout = helloTimeout
	})
	for _, carrier := range []string{connect.ExtenderCarrierTcp, connect.ExtenderCarrierQuic} {
		conn := dialPlainThroughNLayer(t, ctx, bannerA, carrier)
		conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		bannerBytes := make([]byte, len(banner))
		if _, err := io.ReadFull(conn, bannerBytes); err != nil {
			t.Fatalf("%s server first: %v; %s", carrier, err, nlayerErrorsOf(bannerA, bannerB))
		}
		if !bytes.Equal(bannerBytes, banner) {
			t.Fatalf("%s server first: banner = %q", carrier, bannerBytes)
		}
		if err := echoThroughNLayer(conn, []byte("EHLO client.example\r\n")); err != nil {
			t.Fatalf("%s server first, reply: %v", carrier, err)
		}
		conn.Close()
	}

	for _, fixture := range []*extenderFixture{a, bannerA} {
		for {
			select {
			case err := <-fixture.errors:
				if strings.HasPrefix(err.Error(), "nlayer loop:") {
					t.Fatalf("a plain stream was refused as a loop: %v", err)
				}
				continue
			default:
			}
			break
		}
	}
}

// Two clients through one chain carry two randoms and are never confused. The
// same ClientHello bytes on a second connection while the first is relayed
// are refused as a loop, and accepted again once the first has ended and its
// random is released.
func TestNLayerLoopCheckHoldsARandomForItsRelayOnly(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	a, b := newNLayerPlainChain(t, nil, nil)
	hello := captureClientHello(t, &tls.Config{ServerName: "dest.example", InsecureSkipVerify: true})
	otherHello := captureClientHello(t, &tls.Config{ServerName: "dest.example", InsecureSkipVerify: true})

	// two independent clients at once
	first := dialPlainThroughNLayer(t, ctx, a, connect.ExtenderCarrierTcp)
	defer first.Close()
	other := dialPlainThroughNLayer(t, ctx, a, connect.ExtenderCarrierQuic)
	defer other.Close()
	if err := echoThroughNLayer(first, hello); err != nil {
		t.Fatalf("first client: %v; %s", err, nlayerErrorsOf(a, b))
	}
	if err := echoThroughNLayer(other, otherHello); err != nil {
		t.Fatalf("second client: %v; %s", err, nlayerErrorsOf(a, b))
	}
	if randomCount := nlayerClientRandomCount(a.server); randomCount != 2 {
		t.Fatalf("%d randoms in flight, expected the two clients'", randomCount)
	}
	other.Close()

	// the same random again while the first relay holds it
	drainNLayerErrors(a)
	hopDialsBefore := a.hopDialAddresses.count()
	replay := dialPlainThroughNLayer(t, ctx, a, connect.ExtenderCarrierTcp)
	if err := echoThroughNLayer(replay, hello); err == nil {
		t.Fatal("a random already in flight was relayed")
	}
	replay.Close()
	if _, ok := nextNLayerError(a, "nlayer loop"); !ok {
		t.Fatal("the repeated random was not refused as a loop")
	}
	if hopDialCount := a.hopDialAddresses.count(); hopDialCount != hopDialsBefore {
		t.Fatal("the refused connection dialed a hop")
	}

	// the first relay ends, its random is released, and the same bytes are
	// accepted on a new connection
	first.Close()
	waitForNLayer(t, "the first relay's random to be released", func() bool {
		return nlayerClientRandomCount(a.server) == 0
	})
	again := dialPlainThroughNLayer(t, ctx, a, connect.ExtenderCarrierTcp)
	defer again.Close()
	if err := echoThroughNLayer(again, hello); err != nil {
		t.Fatalf("the released random was refused: %v; %s", err, nlayerErrorsOf(a, b))
	}
}
