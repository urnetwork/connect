// What an extender forwards to under a `*.` pattern (EXTENDER.md A5): a client chooses
// everything in front of the pattern's suffix, so the name must be a host name and not
// just end like one.

package extender

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

// The pattern these tests allow, and names under it that are not host names.
const (
	testHostNameExact   = "dest.example"
	testHostNamePattern = "*.dest.example"
)

var testNamesThatAreNotHostNames = []string{
	"line\nbreak.dest.example",
	"line\r\nbreak.dest.example",
	"with space.dest.example",
	"with\ttab.dest.example",
	"[brackets] and words.dest.example",
	"percent%41.dest.example",
	"quote\".dest.example",
	"colon:443.dest.example",
	"slash/path.dest.example",
	"under_score.dest.example",
	"null\x00byte.dest.example",
	"café.dest.example",
	".dest.example",
	"a..dest.example",
	strings.Repeat("a", 64) + ".dest.example",
	strings.Repeat("a.", 124) + "dest.example",
}

// A name under a pattern is allowed only if it is a host name, in any case, at any depth.
// A name the operator wrote out is the operator's text, and is matched as it is.
func TestExtenderIsAllowedHostUnderAPatternMustBeAHostName(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := NewExtenderServer(
		ctx,
		nil,
		[]string{testHostNameExact, testHostNamePattern, "2001:db8::1", "operator_named.example"},
		nil,
		nil,
		DefaultExtenderSettings(),
	)
	defer server.CloseAndWait()

	for _, host := range testNamesThatAreNotHostNames {
		if server.IsAllowedHost(host) {
			t.Errorf("%q is allowed", host)
		}
	}
	for _, host := range []string{
		testHostNameExact,
		"sub.dest.example",
		"a.b.dest.example",
		"API.dest.example",
		"with-hyphen.dest.example",
		"203-0-113-7.dest.example",
		strings.Repeat("a", 63) + ".dest.example",
		"2001:db8::1",
		"operator_named.example",
	} {
		if !server.IsAllowedHost(host) {
			t.Errorf("%q is refused", host)
		}
	}
}

// Whether a request's destination was dialed, for a test that expects it was not, or that
// it was as written. Safe for concurrent use: the dial runs on the connection's goroutine.
type testHostDials struct {
	stateLock sync.Mutex
	addresses []string
}

func (self *testHostDials) dial(ctx context.Context, network string, address string) (net.Conn, error) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.addresses = append(self.addresses, address)
	}()
	return testErrorQuotingDial(ctx, network, address)
}

func (self *testHostDials) dialed() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]string{}, self.addresses...)
}

// A fixture that allows the pattern, and whose every dial is recorded and fails.
func newTestHostNameFixture(t *testing.T, configure func(settings *ExtenderSettings)) (*extenderFixture, *testHostDials) {
	t.Helper()
	dials := &testHostDials{}
	fixture := newExtenderFixtureWithSetup(
		t,
		"127.0.0.1",
		[]string{testSecret},
		[]string{testHostNamePattern},
		func(_ *extenderFixture, settings *ExtenderSettings) {
			settings.DialContext = dials.dial
			settings.DialPacketContext = dials.dial
			if configure != nil {
				configure(settings)
			}
		},
	)
	return fixture, dials
}

// A name that is no host name is refused where every other destination is, on every carrier
// and either way of forwarding, before anything is dialed. So it reaches none of the stages
// that keep their whole error, and the refusal does not repeat it.
func TestExtenderRefusesADestinationThatIsNoHostName(t *testing.T) {
	const notAHostName = "line\nbreak.dest.example"
	expectRefused := func(t *testing.T, fixture *extenderFixture, dials *testHostDials, err error) {
		t.Helper()
		var refusedErr *connect.ExtenderRefusedError
		if !errors.As(err, &refusedErr) || refusedErr.StatusCode != http.StatusForbidden {
			t.Fatalf("the request got %v, expected a 403", err)
		}
		reported := waitForFixtureError(t, fixture, "destination authorization")
		testExpectErrorHoldsNone(t, errors.Unwrap(reported), "line", "break")
		if addresses := dials.dialed(); 0 < len(addresses) {
			t.Fatalf("the extender dialed %q", addresses)
		}
		for _, otherErr := range drainFixtureErrors(fixture) {
			if strings.Contains(otherErr.Error(), "break") {
				t.Fatalf("heard %q", otherErr)
			}
		}
	}

	for _, c := range []struct {
		description string
		carrier     string
		datagram    bool
		configure   func(settings *ExtenderSettings)
	}{
		{description: "a stream on tcp", carrier: connect.ExtenderCarrierTcp},
		{description: "a stream on quic", carrier: connect.ExtenderCarrierQuic},
		{description: "a stream on dns", carrier: connect.ExtenderCarrierDns},
		{description: "a datagram relay on tcp", carrier: connect.ExtenderCarrierTcp, datagram: true},
		{description: "a datagram relay on quic", carrier: connect.ExtenderCarrierQuic, datagram: true},
		{
			description: "a stream with a hop to relay to",
			carrier:     connect.ExtenderCarrierTcp,
			configure: func(settings *ExtenderSettings) {
				settings.NLayerHops = []*connect.ExtenderConfig{testErrorHop}
			},
		},
	} {
		t.Run(c.description, func(t *testing.T) {
			fixture, dials := newTestHostNameFixture(t, c.configure)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			conn, _, err := connect.DialExtender(ctx, fixture.connectSettings(), fixture.extenderConfig(c.carrier), &connect.ExtenderDial{
				DestinationHost: notAHostName,
				DestinationPort: 443,
				Datagram:        c.datagram,
			})
			if conn != nil {
				conn.Close()
			}
			expectRefused(t, fixture, dials, err)
		})
	}

	t.Run("the first release's framing", func(t *testing.T) {
		fixture, dials := newTestHostNameFixture(t, nil)
		if err := sendV1Header(t, fixture, testExtenderHeaderBytes(t, notAHostName, 443, testSecret)); err == nil {
			t.Fatal("the header was not refused")
		}
		reported := waitForFixtureError(t, fixture, "destination authorization")
		testExpectErrorHoldsNone(t, errors.Unwrap(reported), "line", "break")
		if addresses := dials.dialed(); 0 < len(addresses) {
			t.Fatalf("the extender dialed %q", addresses)
		}
	})

	t.Run("the stages that keep their whole error", func(t *testing.T) {
		dial := func(t *testing.T, fixture *extenderFixture, datagram bool) {
			t.Helper()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			conn, _, err := connect.DialExtender(ctx, fixture.connectSettings(), fixture.extenderConfig(connect.ExtenderCarrierTcp), &connect.ExtenderDial{
				DestinationHost: "client-chosen.dest.example",
				DestinationPort: 443,
				Datagram:        datagram,
			})
			if err != nil {
				t.Fatal(err)
			}
			conn.Close()
		}

		// a host name under the pattern is still forwarded, and what the forward stages say
		// of it is that name and no more, which is what is left of a client's choice
		fixture, dials := newTestHostNameFixture(t, nil)
		dial(t, fixture, false)
		if reported := waitForFixtureError(t, fixture, "forward dial"); !strings.Contains(reported.Error(), "client-chosen.dest.example:443") {
			t.Fatalf("heard %v", reported)
		}
		dial(t, fixture, true)
		if reported := waitForFixtureError(t, fixture, "datagram forward dial"); !strings.Contains(reported.Error(), "client-chosen.dest.example:443") {
			t.Fatalf("heard %v", reported)
		}
		if addresses := dials.dialed(); len(addresses) != 2 {
			t.Fatalf("the extender dialed %q", addresses)
		}

		// a hop dial is told the hop and nothing the client chose: the destination is in the
		// header the hop would be sent, and never in what the dial says
		hopFixture, hopDials := newTestHostNameFixture(t, func(settings *ExtenderSettings) {
			settings.NLayerHops = []*connect.ExtenderConfig{testErrorHop}
			settings.NLayerClientHelloTimeout = 0
		})
		dial(t, hopFixture, false)
		reported := waitForFixtureError(t, hopFixture, "nlayer hop dial")
		if strings.Contains(reported.Error(), "client-chosen") || !strings.Contains(reported.Error(), "192.0.2.9:9443") {
			t.Fatalf("heard %v, expected the hop and nothing the client chose", reported)
		}
		if addresses := hopDials.dialed(); len(addresses) != 1 || addresses[0] != "192.0.2.9:9443" {
			t.Fatalf("the extender dialed %q", addresses)
		}

		// a relayed probe carries the pinger the client named, bytes it chose, to the hop,
		// and what the dial says of a failure is the hop and none of them
		for _, pinger := range []struct {
			description string
			dial        connect.ExtenderDial
		}{
			{
				description: "a provider",
				dial: connect.ExtenderDial{
					Service:       connect.ExtenderServiceProbe,
					ProbeClientId: []byte("line\nbreak bytes"),
				},
			},
			{
				description: "an extender",
				dial: connect.ExtenderDial{
					Service:                connect.ExtenderServiceProbe,
					ProbeExtenderPublicKey: []byte("line\nbreak and thirty-two bytes!"),
				},
			},
		} {
			if len(pinger.dial.ProbeClientId) != connect.ExtenderProbeClientIdByteCount && len(pinger.dial.ProbeExtenderPublicKey) != 32 {
				t.Fatalf("%s: the pinger is not well formed, so the probe would name none", pinger.description)
			}
			probeFixture, probeDials := newTestHostNameFixture(t, func(settings *ExtenderSettings) {
				settings.NLayerHops = []*connect.ExtenderConfig{testErrorHop}
			})
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			_, _, err := connect.DialExtender(ctx, probeFixture.connectSettings(), probeFixture.extenderConfig(connect.ExtenderCarrierTcp), &pinger.dial)
			cancel()
			var refusedErr *connect.ExtenderRefusedError
			if !errors.As(err, &refusedErr) {
				t.Fatalf("%s: the probe ended with %v, expected a refusal", pinger.description, err)
			}
			reported := waitForFixtureError(t, probeFixture, "nlayer probe dial")
			if strings.Contains(reported.Error(), "break") || !strings.Contains(reported.Error(), "192.0.2.9:9443") {
				t.Fatalf("%s: heard %q, expected the hop and nothing the client chose", pinger.description, reported)
			}
			for _, otherErr := range drainFixtureErrors(probeFixture) {
				if strings.Contains(otherErr.Error(), "break") {
					t.Fatalf("%s: heard %q", pinger.description, otherErr)
				}
			}
			if addresses := probeDials.dialed(); len(addresses) != 1 || addresses[0] != "192.0.2.9:9443" {
				t.Fatalf("%s: the extender dialed %q", pinger.description, addresses)
			}
		}
	})
}
