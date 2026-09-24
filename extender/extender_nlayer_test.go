// The NLayer extenders of EXTENDER.md A11, end to end over loopback: chains of
// fixtures that each relay to the next, the last forwarding to its destination.
//
// Every fixture has its own destination with its own certificate authority, so
// the destination a chain reaches is its last layer's, and a client's inner tls
// verifies against that destination's roots and no other: which is what shows
// the inner tls runs end to end through every layer untouched. Every fixture's
// egress seam records its hop dials by address and counts its forward dials,
// and its header seam records the HopCount it accepted, so a test can say which
// layer dialed what at which depth.

package extender

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"
)

// Builds depth fixtures on one loopback address, each relaying to the next
// over its tcp carrier and the last forwarding to its destination. configure
// runs on every layer's settings once its hop is set, with the layer's
// position, 0 being the layer a client dials.
func newNLayerChain(
	t *testing.T,
	loopbackIp string,
	depth int,
	configure func(position int, settings *ExtenderSettings),
) []*extenderFixture {
	t.Helper()
	fixtures := make([]*extenderFixture, depth)
	// built from the last layer back, since a layer names its hop's port
	for position := depth - 1; 0 <= position; position -= 1 {
		var hop *extenderFixture
		if position+1 < depth {
			hop = fixtures[position+1]
		}
		fixtures[position] = newExtenderFixture(t, loopbackIp, func(settings *ExtenderSettings) {
			if hop != nil {
				settings.NLayerHops = []*connect.ExtenderConfig{
					hop.extenderConfig(connect.ExtenderCarrierTcp),
				}
			}
			if configure != nil {
				configure(position, settings)
			}
		})
	}
	return fixtures
}

// Client settings that trust the destinations of the given fixtures and
// nothing else, for a layer whose hops end at more than one destination.
func nlayerConnectSettings(fixtures ...*extenderFixture) *connect.ConnectSettings {
	connectSettings := fixtures[0].connectSettings()
	rootCas := x509.NewCertPool()
	for _, fixture := range fixtures {
		rootCas.AddCert(fixture.destination.certificate.Leaf)
	}
	connectSettings.TlsConfig = &tls.Config{
		RootCAs: rootCas,
	}
	return connectSettings
}

// One verified GET of /hello through entry's carrier, returning the body.
func getThroughNLayer(
	entry *extenderFixture,
	carrier string,
	connectSettings *connect.ConnectSettings,
) (string, error) {
	client := connect.NewExtenderHttpClient(connectSettings, entry.extenderConfig(carrier))
	defer client.CloseIdleConnections()
	response, err := client.Get("https://dest.example/hello")
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return "", err
	}
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("status %d", response.StatusCode)
	}
	return string(body), nil
}

// What one fixture's seams have seen so far, so a step can be judged by what
// it changed.
type nlayerCounts struct {
	hopDialCount     int
	forwardDialCount int
	requestCount     int
	acceptedCount    int
}

// What one fixture's seams have seen so far.
func nlayerCountsOf(fixture *extenderFixture) nlayerCounts {
	return nlayerCounts{
		hopDialCount:     fixture.hopDialAddresses.count(),
		forwardDialCount: fixture.forwardDialCount.get(),
		requestCount:     fixture.destination.requestCount.get(),
		acceptedCount:    fixture.acceptedHopCounts.count(),
	}
}

// What each fixture's seams have seen so far, in the order given.
func nlayerCountsOfAll(fixtures []*extenderFixture) []nlayerCounts {
	counts := []nlayerCounts{}
	for _, fixture := range fixtures {
		counts = append(counts, nlayerCountsOf(fixture))
	}
	return counts
}

// Empties the attributed errors of every fixture, so the next step's are its
// own.
func drainNLayerErrors(fixtures ...*extenderFixture) {
	for _, fixture := range fixtures {
		for {
			select {
			case <-fixture.errors:
				continue
			default:
			}
			break
		}
	}
}

// Waits for the next attributed error of one stage, skipping the others.
func nextNLayerError(fixture *extenderFixture, stage string) (error, bool) {
	deadline := time.After(10 * time.Second)
	for {
		select {
		case err := <-fixture.errors:
			if strings.HasPrefix(err.Error(), stage+":") {
				return err, true
			}
		case <-deadline:
			return nil, false
		}
	}
}

// Every attributed error the fixtures hold now, for a failure message.
func nlayerErrorsOf(fixtures ...*extenderFixture) string {
	lines := []string{}
	for position, fixture := range fixtures {
		for {
			select {
			case err := <-fixture.errors:
				lines = append(lines, fmt.Sprintf("layer %d: %v", position, err))
				continue
			default:
			}
			break
		}
	}
	return strings.Join(lines, "; ")
}

// Waits until the condition holds, for state that settles only once a relay
// has ended on its own goroutines.
func waitForNLayer(t *testing.T, description string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !condition() {
		if deadline.Before(time.Now()) {
			t.Fatalf("timed out waiting for %s", description)
		}
		// safe as a poll: every caller waits for a terminal state its own
		// action already ordered -- a connection it closed, a refusal it read
		// -- so this waits out the server's goroutines unwinding, never a race
		time.Sleep(10 * time.Millisecond)
	}
}

// The in-flight client randoms of one server, which the loop check holds for
// the life of each relay (A11).
func nlayerClientRandomCount(server *ExtenderServer) int {
	server.stateLock.Lock()
	defer server.stateLock.Unlock()
	return len(server.nlayerClientRandomCounts)
}

// A loopback port nothing listens on, for a hop that cannot be dialed.
func closedLoopbackPort(t *testing.T, loopbackIp string) int {
	t.Helper()
	listener, err := net.Listen("tcp", net.JoinHostPort(loopbackIp, "0"))
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	return port
}

// A chain of two: the client's request crosses the first layer, which dials the
// second as its client over each of the second's carriers, and the second
// forwards to its destination. The first layer never touches its own
// destination, and the inner tls verifies against the second's roots only.
func TestNLayerChainOfTwoReachesTheLastDestination(t *testing.T) {
	carriers := []string{
		connect.ExtenderCarrierTcp,
		connect.ExtenderCarrierQuic,
		connect.ExtenderCarrierDns,
	}
	for _, carrier := range carriers {
		last := newExtenderFixture(t, "127.0.0.1", nil)
		first := newExtenderFixture(t, "127.0.0.1", func(settings *ExtenderSettings) {
			settings.NLayerHops = []*connect.ExtenderConfig{last.extenderConfig(carrier)}
		})

		body, err := getThroughNLayer(first, connect.ExtenderCarrierTcp, last.connectSettings())
		if err != nil {
			t.Fatalf("%s: %v; %s", carrier, err, nlayerErrorsOf(first, last))
		}
		if !strings.Contains(body, `"host":"dest.example"`) {
			t.Fatalf("%s body = %q", carrier, body)
		}
		if requestCount := last.destination.requestCount.get(); requestCount != 1 {
			t.Fatalf("%s: the last destination handled %d requests, expected 1", carrier, requestCount)
		}
		if requestCount := first.destination.requestCount.get(); requestCount != 0 {
			t.Fatalf("%s: the first layer's own destination handled %d requests", carrier, requestCount)
		}
		if forwardDialCount := last.forwardDialCount.get(); forwardDialCount != 1 {
			t.Fatalf("%s: the last layer made %d forward dials, expected 1", carrier, forwardDialCount)
		}
		if forwardDialCount := first.forwardDialCount.get(); forwardDialCount != 0 {
			t.Fatalf("%s: the first layer made %d forward dials, expected none", carrier, forwardDialCount)
		}
		// the forward of the last layer is on the family of the leg that
		// reached it (A7)
		if forwardNetwork, err := last.nextForwardNetwork(); err != nil || forwardNetwork != "tcp4" {
			t.Fatalf("%s: last forward network = %q, %v", carrier, forwardNetwork, err)
		}
		if hopCounts := first.acceptedHopCounts.snapshot(); !slices.Equal(hopCounts, []uint32{0}) {
			t.Fatalf("%s: the first layer accepted hop counts %v, expected [0]", carrier, hopCounts)
		}
		if hopCounts := last.acceptedHopCounts.snapshot(); !slices.Equal(hopCounts, []uint32{1}) {
			t.Fatalf("%s: the last layer accepted hop counts %v, expected [1]", carrier, hopCounts)
		}
		if carrier == connect.ExtenderCarrierTcp {
			// a tcp hop dial leaves through the same egress seam as a forward
			expectedAddress := last.authority(last.tcpPort)
			if hopDialAddresses := first.hopDialAddresses.snapshot(); !slices.Equal(hopDialAddresses, []string{expectedAddress}) {
				t.Fatalf("hop dials = %v, expected [%s]", hopDialAddresses, expectedAddress)
			}
		}
		if hopStats := first.server.NLayerStats(); len(hopStats) != 1 || hopStats[0].RelayCount != 1 {
			t.Fatalf("%s: stats = %+v, expected one relay", carrier, hopStats)
		}

		// the inner tls is the client's own, to the last destination: the roots
		// of the first layer's destination do not verify it
		if _, err := getThroughNLayer(first, connect.ExtenderCarrierTcp, first.connectSettings()); err == nil {
			t.Fatalf("%s: the inner tls verified against a destination the chain does not reach", carrier)
		} else if !strings.Contains(err.Error(), "certificate") {
			t.Fatalf("%s: the wrong roots failed with %v, expected a certificate error", carrier, err)
		}
	}
}

// Chains of one to eight layers, all under a depth bound of eight. A dial of
// the layer at depth d from the end crosses d extenders: every layer but the
// last makes exactly one hop dial and no forward, the last makes exactly one
// forward, its destination handles the one request, and the last layer reads a
// HopCount of d - 1. The same chain then carries datagrams eight layers deep,
// and a ninth layer in front of it is refused at the last layer, with no
// further dial.
func TestNLayerChainsOfOneToEightLayers(t *testing.T) {
	const maxDepth = 8
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	echoAddr := echoUdp(t, ctx, func(b []byte) []byte {
		return append([]byte("echo:"), b...)
	})
	datagramAddresses := &recordedValues[string]{}
	fixtures := newNLayerChain(t, "127.0.0.1", maxDepth, func(position int, settings *ExtenderSettings) {
		settings.NLayerMaxDepth = maxDepth
		if position == maxDepth-1 {
			// only the last layer turns datagram frames into udp
			settings.DialPacketContext = func(ctx context.Context, network string, address string) (net.Conn, error) {
				datagramAddresses.add(address)
				return (&net.Dialer{}).DialContext(ctx, "udp4", echoAddr.String())
			}
		}
	})
	last := fixtures[maxDepth-1]

	for depth := 1; depth <= maxDepth; depth += 1 {
		entryPosition := maxDepth - depth
		entry := fixtures[entryPosition]
		before := nlayerCountsOfAll(fixtures)
		drainNLayerErrors(fixtures...)

		startTime := time.Now()
		body, err := getThroughNLayer(entry, connect.ExtenderCarrierTcp, last.connectSettings())
		elapsed := time.Since(startTime)
		if err != nil {
			t.Fatalf("depth %d: %v; %s", depth, err, nlayerErrorsOf(fixtures...))
		}
		if !strings.Contains(body, `"host":"dest.example"`) {
			t.Fatalf("depth %d body = %q", depth, body)
		}

		after := nlayerCountsOfAll(fixtures)
		hopDialCount := 0
		for position := range fixtures {
			delta := nlayerCounts{
				hopDialCount:     after[position].hopDialCount - before[position].hopDialCount,
				forwardDialCount: after[position].forwardDialCount - before[position].forwardDialCount,
				requestCount:     after[position].requestCount - before[position].requestCount,
				acceptedCount:    after[position].acceptedCount - before[position].acceptedCount,
			}
			hopDialCount += delta.hopDialCount
			expected := nlayerCounts{}
			switch {
			case position < entryPosition:
				// in front of the entry; nothing reaches it
			case position < maxDepth-1:
				expected = nlayerCounts{hopDialCount: 1, acceptedCount: 1}
			default:
				expected = nlayerCounts{forwardDialCount: 1, requestCount: 1, acceptedCount: 1}
			}
			if delta != expected {
				t.Fatalf("depth %d layer %d saw %+v, expected %+v", depth, position, delta, expected)
			}
		}
		lastHopCounts := last.acceptedHopCounts.snapshot()
		lastHopCount := lastHopCounts[len(lastHopCounts)-1]
		if lastHopCount != uint32(depth-1) {
			t.Fatalf("depth %d: the last layer read HopCount %d, expected %d", depth, lastHopCount, depth-1)
		}
		t.Logf(
			"depth %d: GET reached the destination through %d extenders in %s; %d hop dials, 1 forward from the last layer, HopCount %d at the last layer",
			depth,
			depth,
			elapsed.Round(time.Millisecond),
			hopDialCount,
			lastHopCount,
		)
	}

	// the inner tls of the deepest chain is still the client's own
	if _, err := getThroughNLayer(fixtures[0], connect.ExtenderCarrierTcp, fixtures[0].connectSettings()); err == nil {
		t.Fatal("depth 8: the inner tls verified against a destination the chain does not reach")
	}

	// datagram mode eight layers deep: the frames cross seven layers untouched
	// and the last one turns them into udp
	func() {
		before := nlayerCountsOfAll(fixtures)
		startTime := time.Now()
		packetConn, err := connect.NewExtenderPacketDialContext(
			last.connectSettings(),
			fixtures[0].extenderConfig(connect.ExtenderCarrierTcp),
		)(ctx, "udp4", "dest.example:443")
		if err != nil {
			t.Fatalf("datagram depth 8: %v; %s", err, nlayerErrorsOf(fixtures...))
		}
		defer packetConn.Close()
		payloads := []string{"a", "bb", "ccc", strings.Repeat("d", 1200)}
		for _, payload := range payloads {
			if _, err := packetConn.WriteTo([]byte(payload), nil); err != nil {
				t.Fatalf("datagram depth 8 write %d bytes: %v", len(payload), err)
			}
		}
		buffer := make([]byte, 4096)
		for _, payload := range payloads {
			packetConn.SetReadDeadline(time.Now().Add(10 * time.Second))
			n, _, err := packetConn.ReadFrom(buffer)
			if err != nil {
				t.Fatalf("datagram depth 8 read: %v; %s", err, nlayerErrorsOf(fixtures...))
			}
			if got, want := string(buffer[:n]), "echo:"+payload; got != want {
				t.Fatalf("datagram depth 8 reply = %d bytes, expected the %d byte echo of the next datagram", len(got), len(want))
			}
		}
		if addresses := datagramAddresses.snapshot(); !slices.Equal(addresses, []string{"dest.example:443"}) {
			t.Fatalf("the last layer's udp egress was asked for %v, expected the one destination", addresses)
		}
		after := nlayerCountsOfAll(fixtures)
		for position := 0; position < maxDepth-1; position += 1 {
			if delta := after[position].hopDialCount - before[position].hopDialCount; delta != 1 {
				t.Fatalf("datagram depth 8 layer %d made %d hop dials, expected 1", position, delta)
			}
		}
		if lastHopCounts := last.acceptedHopCounts.snapshot(); lastHopCounts[len(lastHopCounts)-1] != maxDepth-1 {
			t.Fatalf("datagram depth 8: the last layer read HopCount %d", lastHopCounts[len(lastHopCounts)-1])
		}
		t.Logf(
			"datagram depth 8: %d datagrams echoed through 8 extenders in %s, boundaries and order kept",
			len(payloads),
			time.Since(startTime).Round(time.Millisecond),
		)
	}()

	// a ninth layer in front: the request is refused at the eighth fixture,
	// where the count crosses the bound, and nothing is dialed past it
	ninth := newExtenderFixture(t, "127.0.0.1", func(settings *ExtenderSettings) {
		settings.NLayerMaxDepth = maxDepth
		settings.NLayerHops = []*connect.ExtenderConfig{fixtures[0].extenderConfig(connect.ExtenderCarrierTcp)}
	})
	chain := append([]*extenderFixture{ninth}, fixtures...)
	before := nlayerCountsOfAll(chain)
	beforeStats := fixtures[maxDepth-2].server.NLayerStats()[0]
	drainNLayerErrors(chain...)
	if _, err := getThroughNLayer(ninth, connect.ExtenderCarrierTcp, last.connectSettings()); err == nil {
		t.Fatal("depth 9 reached the destination under a bound of 8")
	}
	if err, ok := nextNLayerError(last, "hop count"); !ok {
		t.Fatalf("depth 9 was not refused at the depth bound; %s", nlayerErrorsOf(chain...))
	} else {
		t.Logf("depth 9: refused at the last layer: %v", err)
	}
	after := nlayerCountsOfAll(chain)
	if delta := after[len(chain)-1].forwardDialCount - before[len(chain)-1].forwardDialCount; delta != 0 {
		t.Fatalf("depth 9: the refusing layer made %d forward dials", delta)
	}
	if delta := after[len(chain)-1].acceptedCount - before[len(chain)-1].acceptedCount; delta != 0 {
		t.Fatalf("depth 9: the refusing layer accepted the header")
	}
	if delta := after[len(chain)-1].requestCount - before[len(chain)-1].requestCount; delta != 0 {
		t.Fatalf("depth 9: the destination handled %d requests", delta)
	}
	// the layer in front of the refusal counts a refusal, which does not hold
	// its hop
	afterStats := fixtures[maxDepth-2].server.NLayerStats()[0]
	if afterStats.RefusedCount != beforeStats.RefusedCount+1 || afterStats.FailedCount != beforeStats.FailedCount || !afterStats.HeldUntil.IsZero() {
		t.Fatalf("depth 9: the eighth layer's stats went from %+v to %+v, expected one refusal and no hold", beforeStats, afterStats)
	}
}

// The boundary of the bound: a chain of eight is refused under a bound of
// seven, with a 403 at the eighth layer, where the count crosses, and no dial
// past it.
func TestNLayerChainOfEightIsRefusedUnderABoundOfSeven(t *testing.T) {
	const depth = 8
	fixtures := newNLayerChain(t, "127.0.0.1", depth, func(position int, settings *ExtenderSettings) {
		settings.NLayerMaxDepth = depth - 1
	})
	last := fixtures[depth-1]

	if _, err := getThroughNLayer(fixtures[0], connect.ExtenderCarrierTcp, last.connectSettings()); err == nil {
		t.Fatal("a chain of eight reached the destination under a bound of seven")
	}
	if _, ok := nextNLayerError(last, "hop count"); !ok {
		t.Fatalf("the eighth layer did not refuse at the depth bound; %s", nlayerErrorsOf(fixtures...))
	}
	if forwardDialCount := last.forwardDialCount.get(); forwardDialCount != 0 {
		t.Fatalf("the refusing layer made %d forward dials", forwardDialCount)
	}
	if hopDialCount := last.hopDialAddresses.count(); hopDialCount != 0 {
		t.Fatalf("the refusing layer made %d hop dials", hopDialCount)
	}
	if requestCount := last.destination.requestCount.get(); requestCount != 0 {
		t.Fatalf("the destination handled %d requests", requestCount)
	}
	// the refusal is a 403 the seventh layer read as its hop's answer
	err, ok := nextNLayerError(fixtures[depth-2], "nlayer hop dial")
	if !ok {
		t.Fatal("the seventh layer did not attribute its hop's refusal")
	}
	var refusedErr *connect.ExtenderRefusedError
	if !errors.As(err, &refusedErr) || refusedErr.StatusCode != http.StatusForbidden {
		t.Fatalf("the seventh layer's hop dial failed with %v, expected a 403", err)
	}
	for position := 0; position < depth-1; position += 1 {
		if hopDialCount := fixtures[position].hopDialAddresses.count(); hopDialCount != 1 {
			t.Fatalf("layer %d made %d hop dials, expected 1", position, hopDialCount)
		}
	}
}

// A chain of two under the bounds either side of it: two layers is at a bound
// of two and past a bound of one, which refuses at the second layer.
func TestNLayerChainOfTwoAtTheDepthBound(t *testing.T) {
	cases := []struct {
		maxDepth int
		reaches  bool
	}{
		{maxDepth: 3, reaches: true},
		{maxDepth: 2, reaches: true},
		{maxDepth: 1, reaches: false},
	}
	for _, c := range cases {
		fixtures := newNLayerChain(t, "127.0.0.1", 2, func(position int, settings *ExtenderSettings) {
			settings.NLayerMaxDepth = c.maxDepth
		})
		_, err := getThroughNLayer(fixtures[0], connect.ExtenderCarrierTcp, fixtures[1].connectSettings())
		if c.reaches && err != nil {
			t.Fatalf("bound %d: %v; %s", c.maxDepth, err, nlayerErrorsOf(fixtures...))
		}
		if !c.reaches {
			if err == nil {
				t.Fatalf("bound %d: a chain of two reached the destination", c.maxDepth)
			}
			if _, ok := nextNLayerError(fixtures[1], "hop count"); !ok {
				t.Fatalf("bound %d: the second layer did not refuse at the depth bound", c.maxDepth)
			}
		}
	}
}

// A two extender loop, each the other's only hop. b is built first, so its hop
// names a placeholder port that its own egress seam carries to a once a is
// bound.
func newNLayerLoop(
	t *testing.T,
	configure func(settings *ExtenderSettings),
) (*extenderFixture, *extenderFixture) {
	t.Helper()
	placeholderPort := closedLoopbackPort(t, "127.0.0.1")
	placeholderAddress := net.JoinHostPort("127.0.0.1", fmt.Sprintf("%d", placeholderPort))
	var aAddressLock sync.Mutex
	aAddress := ""

	b := newExtenderFixture(t, "127.0.0.1", func(settings *ExtenderSettings) {
		settings.NLayerHops = []*connect.ExtenderConfig{{
			Profile: connect.ExtenderProfile{
				ConnectMode: connect.ExtenderConnectModeTcpTls,
				ServerName:  testServerName,
				Port:        placeholderPort,
			},
			Ip:     netip.MustParseAddr("127.0.0.1"),
			Secret: testSecret,
		}}
		dialContext := settings.DialContext
		settings.DialContext = func(ctx context.Context, network string, address string) (net.Conn, error) {
			if address == placeholderAddress {
				aAddressLock.Lock()
				address = aAddress
				aAddressLock.Unlock()
			}
			return dialContext(ctx, network, address)
		}
		configure(settings)
	})
	a := newExtenderFixture(t, "127.0.0.1", func(settings *ExtenderSettings) {
		settings.NLayerHops = []*connect.ExtenderConfig{b.extenderConfig(connect.ExtenderCarrierTcp)}
		configure(settings)
	})
	aAddressLock.Lock()
	aAddress = a.authority(a.tcpPort)
	aAddressLock.Unlock()
	return a, b
}

// A loop a -> b -> a with the loop check: the request comes back to a with the
// client random a is already relaying, and a refuses its second entry. Exactly
// two hop dials are made, one by each extender.
func TestNLayerLoopIsRefusedAtTheSecondEntry(t *testing.T) {
	a, b := newNLayerLoop(t, func(settings *ExtenderSettings) {
		settings.NLayerMaxDepth = 8
	})

	if _, err := getThroughNLayer(a, connect.ExtenderCarrierTcp, a.connectSettings()); err == nil {
		t.Fatal("a looping chain reached a destination")
	}
	if _, ok := nextNLayerError(a, "nlayer loop"); !ok {
		t.Fatalf("a did not refuse the request that came back to it; %s", nlayerErrorsOf(a, b))
	}
	// the in-flight randoms are released once the relays end
	waitForNLayer(t, "the loop's relays to end", func() bool {
		return nlayerClientRandomCount(a.server) == 0 && nlayerClientRandomCount(b.server) == 0
	})
	hopDialCount := a.hopDialAddresses.count() + b.hopDialAddresses.count()
	if hopDialCount != 2 {
		t.Fatalf("the loop made %d hop dials, expected exactly 2", hopDialCount)
	}
	if hopCounts := a.acceptedHopCounts.snapshot(); !slices.Equal(hopCounts, []uint32{0, 2}) {
		t.Fatalf("a accepted hop counts %v, expected [0 2]", hopCounts)
	}
	if hopCounts := b.acceptedHopCounts.snapshot(); !slices.Equal(hopCounts, []uint32{1}) {
		t.Fatalf("b accepted hop counts %v, expected [1]", hopCounts)
	}
	if a.forwardDialCount.get() != 0 || b.forwardDialCount.get() != 0 {
		t.Fatal("a looping chain dialed a destination")
	}
	t.Logf("loop a -> b -> a with the loop check: refused at a's second entry after %d hop dials", hopDialCount)
}

// The same loop with the loop check off still ends: the depth bound refuses
// the ninth entry with a 403 after eight traversals, rather than relaying
// forever.
func TestNLayerLoopWithoutTheCheckEndsAtTheDepthBound(t *testing.T) {
	const maxDepth = 8
	a, b := newNLayerLoop(t, func(settings *ExtenderSettings) {
		settings.NLayerMaxDepth = maxDepth
		settings.NLayerClientHelloTimeout = 0
	})

	if _, err := getThroughNLayer(a, connect.ExtenderCarrierTcp, a.connectSettings()); err == nil {
		t.Fatal("a looping chain reached a destination")
	}
	if _, ok := nextNLayerError(a, "hop count"); !ok {
		t.Fatalf("the ninth entry was not refused at the depth bound; %s", nlayerErrorsOf(a, b))
	}
	waitForNLayer(t, "the ninth entry's refusal to be read", func() bool {
		return b.server.NLayerStats()[0].RefusedCount == 1
	})
	hopDialCount := a.hopDialAddresses.count() + b.hopDialAddresses.count()
	if hopDialCount != maxDepth {
		t.Fatalf("the loop made %d hop dials, expected %d", hopDialCount, maxDepth)
	}
	if hopCounts := a.acceptedHopCounts.snapshot(); !slices.Equal(hopCounts, []uint32{0, 2, 4, 6}) {
		t.Fatalf("a accepted hop counts %v, expected [0 2 4 6]", hopCounts)
	}
	if hopCounts := b.acceptedHopCounts.snapshot(); !slices.Equal(hopCounts, []uint32{1, 3, 5, 7}) {
		t.Fatalf("b accepted hop counts %v, expected [1 3 5 7]", hopCounts)
	}
	// a refusal is about the request, so the hop that refused is not held
	if hopStats := b.server.NLayerStats()[0]; hopStats.FailedCount != 0 || !hopStats.HeldUntil.IsZero() {
		t.Fatalf("b's stats of a = %+v, expected no failure and no hold", hopStats)
	}
	t.Logf("loop a -> b -> a without the loop check: refused with 403 at the ninth entry after %d hop dials", hopDialCount)
}

// Two hops share the connections: over sixteen dials both carry some, and the
// counts of NLayerStats agree with what each destination handled.
func TestNLayerBalancesAcrossItsHops(t *testing.T) {
	b := newExtenderFixture(t, "127.0.0.1", nil)
	c := newExtenderFixture(t, "127.0.0.1", nil)
	a := newExtenderFixture(t, "127.0.0.1", func(settings *ExtenderSettings) {
		settings.NLayerHops = []*connect.ExtenderConfig{
			b.extenderConfig(connect.ExtenderCarrierTcp),
			c.extenderConfig(connect.ExtenderCarrierTcp),
		}
	})
	connectSettings := nlayerConnectSettings(b, c)

	const dialCount = 16
	for i := 0; i < dialCount; i += 1 {
		if _, err := getThroughNLayer(a, connect.ExtenderCarrierTcp, connectSettings); err != nil {
			t.Fatalf("dial %d: %v; %s", i, err, nlayerErrorsOf(a, b, c))
		}
	}
	bCount := b.destination.requestCount.get()
	cCount := c.destination.requestCount.get()
	if bCount == 0 || cCount == 0 || bCount+cCount != dialCount {
		t.Fatalf("the hops carried %d and %d of %d connections", bCount, cCount, dialCount)
	}
	hopStats := a.server.NLayerStats()
	if hopStats[0].RelayCount != int64(bCount) || hopStats[1].RelayCount != int64(cCount) {
		t.Fatalf("stats = %+v, expected relays %d and %d", hopStats, bCount, cCount)
	}
	t.Logf("balance over two hops: %d and %d of %d connections", bCount, cCount, dialCount)
}

// A hop that cannot be dialed is held: the connection that found it broken
// still reaches the other hop within its attempts, every connection during the
// hold goes to the other hop without dialing the broken one, and once the hold
// runs out the broken hop is tried again. The hold and its release reach the
// hold handler.
func TestNLayerHoldsAHopItCannotDial(t *testing.T) {
	// long enough that a loaded host still fits dials inside the hold
	const holdTimeout = 2500 * time.Millisecond
	b := newExtenderFixture(t, "127.0.0.1", nil)
	brokenConfig := b.extenderConfig(connect.ExtenderCarrierTcp)
	brokenConfig.Profile.Port = closedLoopbackPort(t, "127.0.0.1")
	brokenAddress := net.JoinHostPort("127.0.0.1", fmt.Sprintf("%d", brokenConfig.Profile.Port))
	holdEvents := &recordedValues[string]{}
	a := newExtenderFixture(t, "127.0.0.1", func(settings *ExtenderSettings) {
		settings.NLayerHops = []*connect.ExtenderConfig{
			b.extenderConfig(connect.ExtenderCarrierTcp),
			brokenConfig,
		}
		settings.NLayerHoldTimeout = holdTimeout
		settings.NLayerAttempts = 2
		settings.NLayerHoldHandler = func(index int, held bool, err error) {
			holdEvents.add(fmt.Sprintf("%d held=%t", index, held))
		}
	})
	brokenDialCount := func() int {
		count := 0
		for _, address := range a.hopDialAddresses.snapshot() {
			if address == brokenAddress {
				count += 1
			}
		}
		return count
	}
	get := func(description string) {
		t.Helper()
		if _, err := getThroughNLayer(a, connect.ExtenderCarrierTcp, b.connectSettings()); err != nil {
			t.Fatalf("%s: %v; %s", description, err, nlayerErrorsOf(a, b))
		}
	}

	// every connection reaches b, including the one that finds the broken
	// hop, since it has a second attempt
	for i := 0; a.server.NLayerStats()[1].FailedCount == 0; i += 1 {
		if 32 <= i {
			t.Fatal("the broken hop was never picked")
		}
		get("before the hold")
	}
	heldUntil := a.server.NLayerStats()[1].HeldUntil
	if heldUntil.IsZero() {
		t.Fatal("the broken hop was not held")
	}

	// while it is held, it is not dialed
	brokenDialsBefore := brokenDialCount()
	relaysBefore := a.server.NLayerStats()[0].RelayCount
	heldDialCount := 0
	for heldDialCount < 8 && time.Now().Add(time.Second).Before(heldUntil) {
		get("during the hold")
		heldDialCount += 1
	}
	if heldDialCount == 0 {
		t.Fatal("no connection was made during the hold")
	}
	if brokenDials := brokenDialCount(); brokenDials != brokenDialsBefore {
		t.Fatalf("the held hop was dialed %d times during its hold", brokenDials-brokenDialsBefore)
	}
	if relays := a.server.NLayerStats()[0].RelayCount; relays != relaysBefore+int64(heldDialCount) {
		t.Fatalf("b relayed %d of the %d connections made during the hold", relays-relaysBefore, heldDialCount)
	}

	// once the hold runs out, it is tried again. The hold ends at the wall
	// clock instant the server reports, which is the signal: this waits for
	// that instant, and the dials below are the proof
	time.Sleep(time.Until(heldUntil) + 50*time.Millisecond)
	for i := 0; a.server.NLayerStats()[1].FailedCount == 1; i += 1 {
		if 32 <= i {
			t.Fatal("the broken hop was never tried again after its hold")
		}
		get("after the hold")
	}
	expectedEvents := []string{"1 held=true", "1 held=false", "1 held=true"}
	if events := holdEvents.snapshot(); !slices.Equal(events, expectedEvents) {
		t.Fatalf("hold events = %v, expected %v", events, expectedEvents)
	}
	t.Logf(
		"hold: the broken hop was skipped by all %d connections made during its hold and tried again after it",
		heldDialCount,
	)
}

// A hop of the client's own family is preferred, so a chain egresses on the
// family the client reached it on (A7); a hop of the other family carries the
// connection only when no hop of its own family can.
func TestNLayerPrefersTheClientFamily(t *testing.T) {
	b4 := newExtenderFixture(t, "127.0.0.1", nil)
	c6 := newExtenderFixture(t, "::1", nil)
	a6 := newExtenderFixture(t, "::1", func(settings *ExtenderSettings) {
		settings.NLayerHops = []*connect.ExtenderConfig{
			b4.extenderConfig(connect.ExtenderCarrierTcp),
			c6.extenderConfig(connect.ExtenderCarrierTcp),
		}
	})
	connectSettings := nlayerConnectSettings(b4, c6)
	const dialCount = 8
	for i := 0; i < dialCount; i += 1 {
		if _, err := getThroughNLayer(a6, connect.ExtenderCarrierTcp, connectSettings); err != nil {
			t.Fatalf("dial %d: %v; %s", i, err, nlayerErrorsOf(a6, b4, c6))
		}
	}
	if count := c6.destination.requestCount.get(); count != dialCount {
		t.Fatalf("the v6 hop carried %d of %d connections from a v6 client", count, dialCount)
	}
	if count := b4.destination.requestCount.get(); count != 0 {
		t.Fatalf("the v4 hop carried %d connections from a v6 client", count)
	}
	if forwardNetwork, err := c6.nextForwardNetwork(); err != nil || forwardNetwork != "tcp6" {
		t.Fatalf("the chain egressed on %q, %v, expected tcp6", forwardNetwork, err)
	}

	// with the only v6 hop broken, the v4 hop carries the connection
	brokenConfig := c6.extenderConfig(connect.ExtenderCarrierTcp)
	brokenConfig.Profile.Port = closedLoopbackPort(t, "::1")
	fallback6 := newExtenderFixture(t, "::1", func(settings *ExtenderSettings) {
		settings.NLayerHops = []*connect.ExtenderConfig{
			b4.extenderConfig(connect.ExtenderCarrierTcp),
			brokenConfig,
		}
	})
	if _, err := getThroughNLayer(fallback6, connect.ExtenderCarrierTcp, connectSettings); err != nil {
		t.Fatalf("fallback: %v; %s", err, nlayerErrorsOf(fallback6, b4))
	}
	hopStats := fallback6.server.NLayerStats()
	if hopStats[1].FailedCount != 1 || hopStats[1].HeldUntil.IsZero() || hopStats[0].RelayCount != 1 {
		t.Fatalf("fallback stats = %+v, expected the v6 hop tried and held, then the v4 hop", hopStats)
	}
	if count := b4.destination.requestCount.get(); count != 1 {
		t.Fatalf("the v4 hop carried %d connections, expected the one fallback", count)
	}
}

// Gossip and feed are served by the NLayer extender itself (A8): they reach
// its own handlers and dial no hop. A latency probe is relayed to the end of
// the chain (GEOMAP §2.9), and like a forward it is answered with the first
// layer's own key, so the identity a chain shows a client is its first
// layer's.
func TestNLayerServesItsOwnServices(t *testing.T) {
	seed, err := connect.NewExtenderKeySeed()
	if err != nil {
		t.Fatal(err)
	}
	publicKey, err := connect.ExtenderPublicKeyFromSeed(seed)
	if err != nil {
		t.Fatal(err)
	}
	b := newExtenderFixture(t, "127.0.0.1", nil)
	gossipConns := make(chan struct{}, 4)
	feedConns := make(chan struct{}, 4)
	// open, as an operator activated extender is, so the carrier probe below
	// needs no secret (A4)
	a := newExtenderFixtureWithSecrets(t, "127.0.0.1", nil, func(settings *ExtenderSettings) {
		settings.IdentityKeySeed = seed
		settings.NLayerHops = []*connect.ExtenderConfig{b.extenderConfig(connect.ExtenderCarrierTcp)}
		settings.GossipConnHandler = func(conn net.Conn) {
			gossipConns <- struct{}{}
		}
		settings.FeedConnHandler = func(conn net.Conn) {
			feedConns <- struct{}{}
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	probe, err := connect.ProbeExtenderLatency(ctx, a.connectSettings(), a.extenderConfig(connect.ExtenderCarrierTcp), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(probe.Response.PublicKey, publicKey) {
		t.Fatal("the probe was answered with another key")
	}
	if hopDialCount := a.hopDialAddresses.count(); hopDialCount != 1 {
		t.Fatalf("the probe made %d hop dials, expected the one relay", hopDialCount)
	}
	for _, service := range []struct {
		service uint32
		conns   chan struct{}
	}{
		{service: connect.ExtenderServiceGossip, conns: gossipConns},
		{service: connect.ExtenderServiceFeed, conns: feedConns},
	} {
		conn, response, err := connect.DialExtender(ctx, a.connectSettings(), a.extenderConfig(connect.ExtenderCarrierTcp), &connect.ExtenderDial{
			Service: service.service,
		})
		if err != nil {
			t.Fatalf("service %d: %v", service.service, err)
		}
		if !slices.Equal(response.PublicKey, publicKey) {
			t.Fatalf("service %d was answered with another key", service.service)
		}
		select {
		case <-service.conns:
		case <-time.After(10 * time.Second):
			t.Fatalf("service %d did not reach the NLayer extender's own handler", service.service)
		}
		conn.Close()
	}
	if hopDialCount := a.hopDialAddresses.count(); hopDialCount != 1 {
		t.Fatalf("gossip and feed made %d hop dials", hopDialCount-1)
	}
	if hopCounts := b.acceptedHopCounts.snapshot(); !slices.Equal(hopCounts, []uint32{1}) {
		t.Fatalf("the hop accepted hop counts %v, expected only the relayed probe", hopCounts)
	}

	// a forward is relayed, and its response still carries the first layer's
	// key and challenge signature
	if _, err := connect.ProbeExtenderCarrier(
		ctx,
		a.connectSettings(),
		a.ip,
		connect.ExtenderConnectModeTcpTls,
		a.tcpPort,
		"",
		testServerName,
		publicKey,
		"dest.example",
		443,
	); err != nil {
		t.Fatalf("the forward's response did not verify under the first layer's key: %v", err)
	}
}

// The whitelist is the first layer's to apply (A5): a destination it does not
// allow is refused with 403 before any hop is dialed.
func TestNLayerWhitelistRefusesBeforeAnyHopDial(t *testing.T) {
	b := newExtenderFixture(t, "127.0.0.1", nil)
	a := newExtenderFixture(t, "127.0.0.1", func(settings *ExtenderSettings) {
		settings.NLayerHops = []*connect.ExtenderConfig{b.extenderConfig(connect.ExtenderCarrierTcp)}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	conn, _, err := connect.DialExtender(ctx, a.connectSettings(), a.extenderConfig(connect.ExtenderCarrierTcp), &connect.ExtenderDial{
		DestinationHost: "other.example",
		DestinationPort: 443,
	})
	if conn != nil {
		conn.Close()
	}
	var refusedErr *connect.ExtenderRefusedError
	if !errors.As(err, &refusedErr) || refusedErr.StatusCode != http.StatusForbidden {
		t.Fatalf("a destination off the whitelist got %v, expected a 403", err)
	}
	if _, ok := nextNLayerError(a, "destination authorization"); !ok {
		t.Fatal("the refusal was not attributed to the whitelist")
	}
	if hopDialCount := a.hopDialAddresses.count(); hopDialCount != 0 {
		t.Fatalf("a refused destination made %d hop dials", hopDialCount)
	}
	if acceptedCount := b.acceptedHopCounts.count(); acceptedCount != 0 {
		t.Fatal("the hop saw a request the first layer refused")
	}
}

// The depth bound applies on every extender, before the whitelist and whether
// or not it has hops: a header whose HopCount reaches the bound is refused
// with 403 at the extender it arrives at, one below it is accepted, and a count
// that cannot be made one deeper is refused even with the bound off. The v1
// framing, whose only refusal is the close, is bounded the same way.
func TestNLayerDepthBoundRefusesAtTheLimit(t *testing.T) {
	const maxDepth = 3
	terminal := newExtenderFixture(t, "127.0.0.1", func(settings *ExtenderSettings) {
		settings.NLayerMaxDepth = maxDepth
	})
	nlayer := newExtenderFixture(t, "127.0.0.1", func(settings *ExtenderSettings) {
		settings.NLayerMaxDepth = maxDepth
		settings.NLayerHops = []*connect.ExtenderConfig{terminal.extenderConfig(connect.ExtenderCarrierTcp)}
		// the stream below sends nothing, so the loop check waits this out
		settings.NLayerClientHelloTimeout = 200 * time.Millisecond
	})
	unbounded := newExtenderFixture(t, "127.0.0.1", func(settings *ExtenderSettings) {
		settings.NLayerMaxDepth = 0
	})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	dial := func(fixture *extenderFixture, hopCount uint32) (net.Conn, error) {
		return connectDialAt(ctx, fixture, hopCount)
	}

	for _, fixture := range []*extenderFixture{terminal, nlayer} {
		drainNLayerErrors(fixture)
		hopDialsBefore := fixture.hopDialAddresses.count()
		forwardDialsBefore := fixture.forwardDialCount.get()
		conn, err := dial(fixture, maxDepth)
		if conn != nil {
			conn.Close()
		}
		var refusedErr *connect.ExtenderRefusedError
		if !errors.As(err, &refusedErr) || refusedErr.StatusCode != http.StatusForbidden {
			t.Fatalf("a header at the limit got %v, expected a 403", err)
		}
		if _, ok := nextNLayerError(fixture, "hop count"); !ok {
			t.Fatal("the refusal was not attributed to the depth bound")
		}
		if fixture.hopDialAddresses.count() != hopDialsBefore || fixture.forwardDialCount.get() != forwardDialsBefore {
			t.Fatal("a header at the limit was dialed onward")
		}
	}

	// one below the limit is accepted at either; the NLayer extender's hop then
	// refuses it one layer deeper, which reaches the client as a closed stream
	conn, err := dial(terminal, maxDepth-1)
	if err != nil {
		t.Fatalf("a header one below the limit was refused: %v", err)
	}
	conn.Close()
	conn, err = dial(nlayer, maxDepth-1)
	if err != nil {
		t.Fatalf("a header one below the limit was refused by the NLayer extender: %v", err)
	}
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("the stream of a request its hop refused carried bytes")
	}
	conn.Close()
	waitForNLayer(t, "the hop's refusal to be counted", func() bool {
		return nlayer.server.NLayerStats()[0].RefusedCount == 1
	})

	// with the bound off, any count is carried but the one that cannot grow
	conn, err = dial(unbounded, 1000)
	if err != nil {
		t.Fatalf("an unbounded extender refused a deep header: %v", err)
	}
	conn.Close()
	conn, err = dial(unbounded, ^uint32(0))
	if conn != nil {
		conn.Close()
	}
	if err == nil {
		t.Fatal("a HopCount that cannot grow was accepted")
	}

	// v1: the header at the limit is closed without a forward
	drainNLayerErrors(terminal)
	forwardDialsBefore := terminal.forwardDialCount.get()
	if err := v1ExchangeAt(ctx, terminal, maxDepth, terminal.destination.rootCas); err == nil {
		t.Fatal("a v1 header at the limit was relayed")
	}
	if _, ok := nextNLayerError(terminal, "hop count"); !ok {
		t.Fatal("the v1 refusal was not attributed to the depth bound")
	}
	if terminal.forwardDialCount.get() != forwardDialsBefore {
		t.Fatal("a v1 header at the limit was forwarded")
	}
}

// One forward request at the given depth on the fixture's tcp carrier.
func connectDialAt(ctx context.Context, fixture *extenderFixture, hopCount uint32) (net.Conn, error) {
	conn, _, err := connect.DialExtender(ctx, fixture.connectSettings(), fixture.extenderConfig(connect.ExtenderCarrierTcp), &connect.ExtenderDial{
		DestinationHost: "dest.example",
		DestinationPort: 443,
		HopCount:        hopCount,
	})
	return conn, err
}

// One v1 exchange at the given depth: the header, then the inner tls to the
// destination, verified against rootCas. A refused header is closed with
// nothing relayed, which is the error returned. It is written by hand, so it
// makes no memory claim of the connect client's own.
func v1ExchangeAt(ctx context.Context, fixture *extenderFixture, hopCount uint32, rootCas *x509.CertPool) error {
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", fixture.authority(fixture.tcpPort))
	if err != nil {
		return err
	}
	defer conn.Close()
	outerConn := tls.Client(conn, &tls.Config{
		ServerName:         testServerName,
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS13,
	})
	if err := outerConn.HandshakeContext(ctx); err != nil {
		return err
	}
	header := newTestExtenderHeader("dest.example", 443, testSecret)
	header.HopCount = hopCount
	headerMessageBytes, err := proto.Marshal(header)
	if err != nil {
		return err
	}
	headerBytes := make([]byte, 4+len(headerMessageBytes))
	binary.BigEndian.PutUint32(headerBytes[0:4], uint32(len(headerMessageBytes)))
	copy(headerBytes[4:], headerMessageBytes)
	if _, err := outerConn.Write(headerBytes); err != nil {
		return err
	}
	// an accepted header is followed by the inner tls, which this exchange
	// starts: the destination's answer is the proof that it was relayed
	innerConn := tls.Client(outerConn, &tls.Config{
		ServerName: "dest.example",
		RootCAs:    rootCas,
	})
	return innerConn.HandshakeContext(ctx)
}

// A v1 client through an NLayer extender reaches the last destination: the
// legacy framing is relayed exactly as the http one is (A3).
func TestNLayerRelaysAV1Client(t *testing.T) {
	fixtures := newNLayerChain(t, "127.0.0.1", 2, nil)
	last := fixtures[1]
	client := &http.Client{
		Transport: &http.Transport{
			DialTLSContext: newV1ExtenderDialTlsContextWithRoots(t, fixtures[0], last.destination.rootCas),
		},
		Timeout: 20 * time.Second,
	}
	defer client.CloseIdleConnections()
	response, err := client.Get("https://dest.example/hello")
	if err != nil {
		t.Fatalf("%v; %s", err, nlayerErrorsOf(fixtures...))
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"host":"dest.example"`) {
		t.Fatalf("v1 body = %q", body)
	}
	if hopCounts := last.acceptedHopCounts.snapshot(); !slices.Equal(hopCounts, []uint32{1}) {
		t.Fatalf("the last layer accepted hop counts %v, expected [1]", hopCounts)
	}
}

// The hop's own secret and identity key are what the dial presents and pins
// (A4, B3): the hop's key reaches it, another key is a dial failure that holds
// the hop, and a secret the hop does not allow is a refusal that does not.
func TestNLayerHonoursTheHopSecretAndKey(t *testing.T) {
	seed, err := connect.NewExtenderKeySeed()
	if err != nil {
		t.Fatal(err)
	}
	publicKey, err := connect.ExtenderPublicKeyFromSeed(seed)
	if err != nil {
		t.Fatal(err)
	}
	otherSeed, err := connect.NewExtenderKeySeed()
	if err != nil {
		t.Fatal(err)
	}
	otherPublicKey, err := connect.ExtenderPublicKeyFromSeed(otherSeed)
	if err != nil {
		t.Fatal(err)
	}
	b := newExtenderFixture(t, "127.0.0.1", func(settings *ExtenderSettings) {
		settings.IdentityKeySeed = seed
	})

	cases := []struct {
		description  string
		configure    func(hop *connect.ExtenderConfig)
		reaches      bool
		refusedCount int64
		failedCount  int64
	}{
		{
			description: "the hop's own key",
			configure: func(hop *connect.ExtenderConfig) {
				hop.PublicKey = publicKey
			},
			reaches: true,
		},
		{
			description: "another key",
			configure: func(hop *connect.ExtenderConfig) {
				hop.PublicKey = otherPublicKey
			},
			failedCount: 1,
		},
		{
			description: "a secret the hop does not allow",
			configure: func(hop *connect.ExtenderConfig) {
				hop.Secret = "not-the-hop-secret"
			},
			refusedCount: 1,
		},
	}
	for _, c := range cases {
		hopConfig := b.extenderConfig(connect.ExtenderCarrierTcp)
		c.configure(hopConfig)
		a := newExtenderFixture(t, "127.0.0.1", func(settings *ExtenderSettings) {
			settings.NLayerHops = []*connect.ExtenderConfig{hopConfig}
		})
		_, err := getThroughNLayer(a, connect.ExtenderCarrierTcp, b.connectSettings())
		if c.reaches && err != nil {
			t.Fatalf("%s: %v; %s", c.description, err, nlayerErrorsOf(a, b))
		}
		if !c.reaches && err == nil {
			t.Fatalf("%s: the chain reached the destination", c.description)
		}
		waitForNLayer(t, c.description+" to be counted", func() bool {
			hopStats := a.server.NLayerStats()[0]
			return hopStats.RelayCount+hopStats.RefusedCount+hopStats.FailedCount == 1
		})
		hopStats := a.server.NLayerStats()[0]
		if hopStats.RefusedCount != c.refusedCount || hopStats.FailedCount != c.failedCount {
			t.Fatalf("%s: stats = %+v, expected %d refused and %d failed", c.description, hopStats, c.refusedCount, c.failedCount)
		}
		if held := !hopStats.HeldUntil.IsZero(); held != (0 < c.failedCount) {
			t.Fatalf("%s: held = %t", c.description, held)
		}
	}
}

// A request datagram mode is relayed without waiting for a ClientHello: its
// stream carries frames, not an inner tls, so the loop check never reads it
// and the hop is dialed at once even under a loop check timeout far longer
// than this test waits.
func TestNLayerDatagramRequestIsNotPeeked(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	echoAddr := echoUdp(t, ctx, func(b []byte) []byte { return b })
	b := newExtenderFixture(t, "127.0.0.1", func(settings *ExtenderSettings) {
		// the hop turns the frames into udp, to an echo that never resolves the
		// destination name
		settings.DialPacketContext = func(ctx context.Context, network string, address string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "udp4", echoAddr.String())
		}
	})
	a := newExtenderFixture(t, "127.0.0.1", func(settings *ExtenderSettings) {
		settings.NLayerHops = []*connect.ExtenderConfig{b.extenderConfig(connect.ExtenderCarrierTcp)}
		settings.NLayerClientHelloTimeout = time.Minute
	})
	conn, _, err := connect.DialExtender(ctx, a.connectSettings(), a.extenderConfig(connect.ExtenderCarrierTcp), &connect.ExtenderDial{
		DestinationHost: "dest.example",
		DestinationPort: 443,
		Datagram:        true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// nothing is written: a stream would hold the hop dial for the minute
	waitForNLayer(t, "the hop to accept the datagram request", func() bool {
		return b.acceptedHopCounts.count() == 1
	})
	if hopCounts := b.acceptedHopCounts.snapshot(); !slices.Equal(hopCounts, []uint32{1}) {
		t.Fatalf("the hop accepted hop counts %v, expected [1]", hopCounts)
	}
}

// Close interrupts a hop dial in flight, as it does every other part of a
// connection: the dial of a hop that takes the tcp connection and never
// answers the handshake ends with the server, not at its own budget of a
// minute.
func TestNLayerCloseInterruptsAHopDial(t *testing.T) {
	stallListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	stalledConns := make(chan net.Conn, 4)
	go func() {
		for {
			conn, err := stallListener.Accept()
			if err != nil {
				return
			}
			stalledConns <- conn
		}
	}()
	t.Cleanup(func() {
		stallListener.Close()
		for {
			select {
			case conn := <-stalledConns:
				conn.Close()
				continue
			default:
			}
			break
		}
	})
	a := newExtenderFixture(t, "127.0.0.1", func(settings *ExtenderSettings) {
		settings.NLayerHops = []*connect.ExtenderConfig{{
			Profile: connect.ExtenderProfile{
				ConnectMode: connect.ExtenderConnectModeTcpTls,
				ServerName:  testServerName,
				Port:        stallListener.Addr().(*net.TCPAddr).Port,
			},
			Ip:     netip.MustParseAddr("127.0.0.1"),
			Secret: testSecret,
		}}
		settings.NLayerDialTimeout = time.Minute
		// the dial starts at once, with nothing to read ahead
		settings.NLayerClientHelloTimeout = 0
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := connectDialAt(ctx, a, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var stalledConn net.Conn
	select {
	case stalledConn = <-stalledConns:
		// held open, so only the dial's own context can end its handshake
		defer stalledConn.Close()
	case <-time.After(10 * time.Second):
		t.Fatal("the hop was never dialed")
	}

	a.server.CloseAndWait()
	// the close cancels the dial, whose handshake closes its connection: the
	// hop's end of it reading that close is the signal the dial ended
	if err := stalledConn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, stalledConn); err != nil && errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatal("the hop dial outlived the server's close")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if lifecycleResidueCount(lifecycleResidueStacks(), "(*ExtenderServer).dialNLayerHop(") == 0 {
			break
		}
		if deadline.Before(time.Now()) {
			t.Fatal("the hop dial's goroutine outlived the server's close")
		}
		// safe as a poll: the dial's connection closed above, so this only
		// waits out its goroutine returning from what was already canceled
		time.Sleep(10 * time.Millisecond)
	}
}

// A hop dial this host's memory budget refuses is this host's condition, not
// the hop's: the connection is refused, the hop is neither held nor counted as
// failed, and it carries the very next connection the budget has room for. The
// host is budgeted below one carrier's claim, and the client is the v1 framing
// written by hand, so the only claim made is the hop dial's.
func TestNLayerLocalMemoryBudgetRefusalDoesNotHoldTheHop(t *testing.T) {
	fixtures := newNLayerChain(t, "127.0.0.1", 2, nil)
	first, last := fixtures[0], fixtures[1]
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	previousMemoryBudget := connect.MemoryBudget()
	restored := false
	restoreMemoryBudget := func() {
		if !restored {
			connect.SetMemoryBudget(previousMemoryBudget)
			restored = true
		}
	}
	defer restoreMemoryBudget()
	connect.SetMemoryBudget(256 * 1024)

	drainNLayerErrors(first)
	if err := v1ExchangeAt(ctx, first, 0, last.destination.rootCas); err == nil {
		t.Fatal("a connection whose hop dial the budget refused was relayed")
	}
	err, ok := nextNLayerError(first, "nlayer hop dial")
	if !ok {
		t.Fatal("the refused hop dial was not attributed")
	}
	if !connect.IsExtenderMemoryBudgetError(err) {
		t.Fatalf("the hop dial failed with %v, expected the memory budget", err)
	}
	hopStats := first.server.NLayerStats()[0]
	if hopStats.FailedCount != 0 || hopStats.RefusedCount != 0 || !hopStats.HeldUntil.IsZero() {
		t.Fatalf("stats = %+v, expected the hop neither failed nor held", hopStats)
	}
	// the claim is refused before anything leaves the host
	if hopDialCount := first.hopDialAddresses.count(); hopDialCount != 0 {
		t.Fatalf("a refused claim made %d hop dials", hopDialCount)
	}
	if acceptedCount := last.acceptedHopCounts.count(); acceptedCount != 0 {
		t.Fatal("the hop saw a request the budget refused")
	}

	// with room again, the next connection goes straight to the hop, which a
	// hold would have kept it from for NLayerHoldTimeout
	restoreMemoryBudget()
	if err := v1ExchangeAt(ctx, first, 0, last.destination.rootCas); err != nil {
		t.Fatalf("the hop did not carry the next connection: %v; %s", err, nlayerErrorsOf(first, last))
	}
	if hopStats := first.server.NLayerStats()[0]; hopStats.RelayCount != 1 || hopStats.FailedCount != 0 {
		t.Fatalf("stats = %+v, expected one relay and no failure", hopStats)
	}
}
